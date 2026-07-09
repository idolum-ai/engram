package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/idolum-ai/engram/internal/anthropic"
	"github.com/idolum-ai/engram/internal/commands"
	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/lockfile"
	"github.com/idolum-ai/engram/internal/redact"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
	"github.com/idolum-ai/engram/internal/tmux"
	"github.com/idolum-ai/engram/internal/version"
)

type App struct {
	Config         config.Config
	Store          *state.Store
	Telegram       *telegram.Client
	Anthropic      *anthropic.Client
	Tmux           tmux.Manager
	lock           *lockfile.Lock
	startedAt      time.Time
	quitCode       int
	stopCh         chan struct{}
	runCtx         context.Context
	runCancel      context.CancelFunc
	refreshWG      sync.WaitGroup
	schedulerWG    sync.WaitGroup
	captureSlots   chan struct{}
	haikuSlots     chan struct{}
	summaryMu      sync.Mutex
	summaryQueued  map[int]bool
	summaryRunning map[int]bool
	summaryForce   map[int]bool
	captureMu      sync.Mutex
	captureHistory map[int][]map[string]bool
	sleepHook      func(time.Duration)
	refreshHook    func(context.Context, int, bool)
}

const summaryQuietPeriod = 2 * time.Second
const haikuCaptureHistoryLimit = 5
const maxConcurrentCaptures = 2
const maxConcurrentHaikuRequests = 2

func New(cfg config.Config) (*App, error) {
	if err := config.EnsureDirs(cfg); err != nil {
		return nil, err
	}
	key := lockfile.Key(cfg.TelegramBotToken, strconv.FormatInt(cfg.TelegramAllowedUserID, 10), strconv.FormatInt(cfg.TelegramChatID, 10))
	l, err := lockfile.Acquire(cfg.LockDir(), key, lockfile.Metadata{Details: map[string]string{
		"telegram_user_id": strconv.FormatInt(cfg.TelegramAllowedUserID, 10),
		"telegram_chat_id": strconv.FormatInt(cfg.TelegramChatID, 10),
		"version":          version.String(),
	}})
	if err != nil {
		return nil, err
	}
	store, err := state.Open(cfg.StatePath(), cfg.AuditPath())
	if err != nil {
		l.Close()
		return nil, err
	}
	return &App{
		Config:         cfg,
		Store:          store,
		Telegram:       telegram.New(cfg.TelegramBotToken),
		Anthropic:      anthropic.New(cfg.AnthropicAPIKey, cfg.AnthropicModel),
		Tmux:           tmux.New(tmux.ExecRunner{}),
		lock:           l,
		startedAt:      time.Now().UTC(),
		stopCh:         make(chan struct{}),
		captureSlots:   make(chan struct{}, maxConcurrentCaptures),
		haikuSlots:     make(chan struct{}, maxConcurrentHaikuRequests),
		summaryQueued:  map[int]bool{},
		summaryRunning: map[int]bool{},
		summaryForce:   map[int]bool{},
		captureHistory: map[int][]map[string]bool{},
	}, nil
}

func (a *App) Close() error {
	if a.lock != nil {
		return a.lock.Close()
	}
	return nil
}

func (a *App) Run(ctx context.Context) int {
	defer a.Close()
	runCtx, cancel := context.WithCancel(ctx)
	a.runCtx = runCtx
	a.runCancel = cancel
	defer func() {
		cancel()
		a.schedulerWG.Wait()
		a.refreshWG.Wait()
	}()
	_ = a.audit("service.start", "ok", map[string]any{"version": version.String()})
	a.registerCommands(runCtx)
	a.schedulerWG.Add(1)
	go func() {
		defer a.schedulerWG.Done()
		a.scheduler(runCtx)
	}()
	offset := a.Store.Snapshot().LastUpdateID + 1
	backoff := time.Second
	for {
		select {
		case <-runCtx.Done():
			_ = a.audit("service.stop", "context", nil)
			return a.quitCode
		case <-a.stopCh:
			_ = a.audit("service.stop", "requested", map[string]any{"code": a.quitCode})
			return a.quitCode
		default:
		}
		updates, err := a.Telegram.GetUpdates(runCtx, offset, a.Config.TelegramPollTimeoutSeconds)
		if err != nil {
			_ = a.audit("telegram.poll", "failed", err.Error())
			if !a.sleepContext(runCtx, backoff) {
				return a.quitCode
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		for _, update := range updates {
			kind, refs := updateJournalRefs(update)
			if err := a.Store.MarkPoll(update.UpdateID, kind, refs); err != nil {
				fmt.Fprintln(os.Stderr, "state mark poll:", err)
				return 1
			}
			if update.UpdateID >= offset {
				offset = update.UpdateID + 1
			}
			status := a.handleUpdate(runCtx, update)
			if err := a.Store.RecordUpdate(update.UpdateID, kind, status, "", refs); err != nil {
				fmt.Fprintln(os.Stderr, "state record update:", err)
				return 1
			}
			if a.quitCode != 0 || runCtx.Err() != nil {
				return a.quitCode
			}
		}
	}
}

func (a *App) handleUpdate(ctx context.Context, update telegram.Update) string {
	if update.CallbackQuery != nil {
		return a.handleCallback(ctx, *update.CallbackQuery)
	}
	if update.Message == nil {
		return "skipped_no_message"
	}
	msg := *update.Message
	if !a.authorized(&msg) {
		_ = a.audit("auth.reject", "rejected", map[string]any{"chat_id": msg.Chat.ID})
		return "rejected_unauthorized"
	}
	key := fmt.Sprintf("%d:%d", msg.Chat.ID, msg.MessageID)
	if a.Store.SeenMessage(key) {
		return "skipped_duplicate_message"
	}
	if err := a.Store.MarkMessage(key); err != nil {
		_ = a.audit("state.message", "failed", map[string]any{"message_id": msg.MessageID, "error": err.Error()})
		return "failed_state_mark_message"
	}
	if doc, ok := msg.FileAttachment(); ok {
		a.handleAttachment(ctx, msg, doc)
		return "handled_attachment"
	}
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return "skipped_empty_message"
	}
	if input, ok := escapedSlashInput(text); ok {
		if msg.ReplyToMessage != nil {
			if ts, found := a.Store.FindByAnchor(msg.Chat.ID, msg.ReplyToMessage.MessageID); found {
				result := a.sendInput(ctx, ts.ID, input, "command", true)
				if !result.OK() {
					a.reply(ctx, msg, result.Message)
				}
				return result.status("anchor_reply")
			}
		}
		a.reply(ctx, msg, "reply to a session anchor to send slash input; for example, //clear sends /clear")
		return "handled_unroutable_slash_input"
	}
	if strings.HasPrefix(text, "/") {
		return a.handleCommand(ctx, msg, text)
	}
	if msg.ReplyToMessage != nil {
		if ts, ok := a.Store.FindByAnchor(msg.Chat.ID, msg.ReplyToMessage.MessageID); ok {
			result := a.sendInput(ctx, ts.ID, text, "command", true)
			if !result.OK() {
				a.reply(ctx, msg, result.Message)
			}
			return result.status("anchor_reply")
		}
		a.reply(ctx, msg, "session not found for this reply; use /sessions to find an active anchor")
		return "anchor_reply_user_error"
	}
	a.newSession(ctx, msg, text)
	return "handled_new_session"
}

func escapedSlashInput(text string) (string, bool) {
	if !strings.HasPrefix(text, "//") {
		return "", false
	}
	return text[1:], true
}

func (a *App) authorized(msg *telegram.Message) bool {
	if msg.Chat.ID != a.Config.TelegramChatID {
		return false
	}
	if msg.From == nil || msg.From.ID != a.Config.TelegramAllowedUserID {
		return false
	}
	return true
}

func (a *App) handleCommand(ctx context.Context, msg telegram.Message, text string) (status string) {
	status = "command_ok"
	fields := strings.Fields(text)
	cmd := strings.TrimPrefix(fields[0], "/")
	if at := strings.IndexByte(cmd, '@'); at >= 0 {
		cmd = cmd[:at]
	}
	args := strings.TrimSpace(strings.TrimPrefix(text, fields[0]))
	_ = a.audit("telegram.command", "received", map[string]any{"command": cmd, "message_id": msg.MessageID})
	switch cmd {
	case "help":
		a.reply(ctx, msg, commands.HelpText())
	case "commands":
		a.commandsMetadata(ctx, msg)
	case "status":
		a.reply(ctx, msg, a.statusText())
	case "version":
		a.reply(ctx, msg, version.String())
	case "sessions":
		a.sessions(ctx, msg)
	case "attach":
		if args == "" {
			a.reply(ctx, msg, "usage: /attach <tmux-target>")
			return
		}
		status = a.attachTarget(ctx, msg, args).status("command")
	case "new":
		if args == "" {
			a.reply(ctx, msg, "usage: /new <text>")
			return
		}
		a.newSession(ctx, msg, args)
	case "send", "run":
		id, rest, ok := parseIDRest(args)
		if !ok {
			status = "command_user_error"
			a.reply(ctx, msg, "usage: /"+cmd+" <id> <text>")
			return
		}
		result := a.sendInput(ctx, id, rest, "command", true)
		status = result.status("command")
		if !result.OK() {
			a.reply(ctx, msg, result.Message)
		}
	case "text", "type":
		id, rest, ok := parseIDRest(args)
		if !ok {
			status = "command_user_error"
			a.reply(ctx, msg, "usage: /"+cmd+" <id> <text>")
			return
		}
		result := a.sendInput(ctx, id, rest, "text", false)
		status = result.status("command")
		if !result.OK() {
			a.reply(ctx, msg, result.Message)
		}
	case "key":
		id, rest, ok := parseIDRest(args)
		if !ok {
			status = "command_user_error"
			a.reply(ctx, msg, "usage: /key <id> <keys...>")
			return
		}
		result := a.sendKeys(ctx, id, strings.Fields(rest))
		status = result.status("command")
		if !result.OK() {
			a.reply(ctx, msg, result.Message)
		}
	case "rename":
		id, rest, ok := parseIDRest(args)
		if !ok || strings.TrimSpace(rest) == "" {
			a.reply(ctx, msg, "usage: /rename <id> <name>")
			return
		}
		a.rename(ctx, id, rest, msg)
	case "cwd":
		id, ok := parseID(args)
		if !ok {
			a.reply(ctx, msg, "usage: /cwd <id>")
			return
		}
		a.cwd(ctx, id, msg)
	case "cd":
		id, rest, ok := parseIDRest(args)
		if !ok || strings.TrimSpace(rest) == "" {
			a.reply(ctx, msg, "usage: /cd <id> <path>")
			return
		}
		result := a.cd(ctx, id, rest)
		status = result.status("command")
		if !result.OK() {
			a.reply(ctx, msg, result.Message)
		}
	case "watch":
		id, ok := parseID(args)
		if !ok {
			a.reply(ctx, msg, "usage: /watch <id>")
			return
		}
		result := a.watchSession(ctx, id, 0)
		status = result.status("command")
		if !result.OK() {
			a.reply(ctx, msg, result.Message)
		}
	case "dump":
		a.captureFile(ctx, msg, args, true)
	case "raw":
		a.captureFile(ctx, msg, args, false)
	case "close":
		id, ok := parseID(args)
		if !ok {
			a.reply(ctx, msg, "usage: /close <id>")
			return
		}
		result := a.closeSession(ctx, id)
		status = result.status("command")
		if !result.OK() {
			a.reply(ctx, msg, result.Message)
		}
	case "attachments":
		a.attachments(ctx, msg)
	case "download":
		a.download(ctx, msg, args)
	case "logs":
		a.logs(ctx, msg)
	case "stop", "unwatch":
		id, ok := parseID(args)
		if !ok {
			a.reply(ctx, msg, "usage: /"+cmd+" <id>")
			return
		}
		_, ok, err := a.Store.UpdateSession(id, func(ts *state.TerminalSession) { ts.WatchEnabled = false })
		if err != nil {
			a.reply(ctx, msg, "state error: "+err.Error())
			return
		}
		if !ok {
			a.reply(ctx, msg, "session not found")
			return
		}
		a.reply(ctx, msg, fmt.Sprintf("[%d] watch stopped", id))
	case "quit":
		a.reply(ctx, msg, "Engram stopping. tmux sessions remain open.")
		a.quitCode = 0
		a.stop()
	case "restart":
		a.reply(ctx, msg, "Engram restarting. tmux sessions remain open.")
		a.quitCode = 2
		a.stop()
	case "kill":
		a.reply(ctx, msg, "/kill is reserved; use /close <id>.")
	case "attachment-bypass", "attachment_bypass":
		hash := parseBypassHash(args)
		if hash == "" || !validSHA256Hex(hash) {
			a.reply(ctx, msg, "usage: /attachment_bypass sha256:<hash>")
			return
		}
		expires := time.Now().UTC().Add(30 * time.Minute)
		if err := a.Store.AddAttachmentBypass(state.AttachmentBypass{
			ChatID:    msg.Chat.ID,
			UserID:    msg.From.ID,
			SHA256:    strings.ToLower(hash),
			CreatedAt: time.Now().UTC(),
			ExpiresAt: expires,
		}); err != nil {
			a.reply(ctx, msg, "state error: "+err.Error())
			return
		}
		a.reply(ctx, msg, "large attachment bypass recorded until "+expires.Format(time.RFC3339))
	default:
		status = "command_user_error"
		a.reply(ctx, msg, "unknown command; try /help")
	}
	return status
}

func (a *App) registerCommands(ctx context.Context) {
	meta := commands.BotCommands()
	tgCommands := make([]telegram.BotCommand, 0, len(meta))
	for _, cmd := range meta {
		tgCommands = append(tgCommands, telegram.BotCommand{
			Command:     cmd.Command,
			Description: cmd.Description,
		})
	}
	if err := a.Telegram.SetMyCommands(ctx, tgCommands); err != nil {
		_ = a.audit("telegram.commands", "failed", err.Error())
		return
	}
	_ = a.audit("telegram.commands", "ok", map[string]any{"count": len(tgCommands)})
}

func (a *App) reply(ctx context.Context, msg telegram.Message, text string) {
	if _, err := a.Telegram.SendMessage(ctx, msg.Chat.ID, text, msg.MessageID, nil); err != nil {
		_ = a.audit("telegram.send", "failed", map[string]any{"reply_to": msg.MessageID, "error": err.Error()})
	}
}

func (a *App) audit(eventType, status string, payload any) error {
	return a.Store.Audit(eventType, status, a.redactAuditPayload(payload))
}

func (a *App) redactAuditPayload(payload any) any {
	switch v := payload.(type) {
	case string:
		return redact.Secrets(v, a.Config.TelegramBotToken, a.Config.AnthropicAPIKey)
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, value := range v {
			out[key] = a.redactAuditPayload(value)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, value := range v {
			out[i] = a.redactAuditPayload(value)
		}
		return out
	default:
		return payload
	}
}

func (a *App) statusText() string {
	st := a.Store.Snapshot()
	space := diskFree(a.Config.ArtifactDir())
	return fmt.Sprintf("Engram status\nversion: %s\nuptime: %s\nsessions: %d\nstate: %s\naudit: %s\nattachments: %s\n/tmp free: %d\nlast poll: %s\nlast update: %d\nupdate journal: %d\nlast haiku: %s\nlast haiku error: %s",
		version.String(),
		time.Since(a.startedAt).Round(time.Second),
		len(st.TerminalSessions),
		a.Config.StatePath(),
		a.Config.AuditPath(),
		a.Config.AttachmentDir(),
		space,
		st.LastPollAt.Format(time.RFC3339),
		st.LastUpdateID,
		len(st.UpdateJournal),
		st.LastHaikuAt.Format(time.RFC3339),
		firstNonEmpty(st.LastHaikuError, "-"),
	)
}

func renderLocal(ts state.TerminalSession, summary string) string {
	title := firstNonEmpty(ts.Title, "-")
	if len(title) > 40 {
		title = title[:40]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[%d] %s  %s\nlast: %s\n\n%s",
		ts.ID,
		ts.State,
		title,
		firstNonEmpty(ts.LastInputPreview, "-"),
		summary,
	)
	if paths := renderVisiblePaths(ts.LastRawCapture); paths != "" {
		b.WriteString("\n\n")
		b.WriteString(paths)
	}
	return b.String()
}

func parseID(arg string) (int, bool) {
	fields := strings.Fields(arg)
	if len(fields) != 1 {
		return 0, false
	}
	n, err := strconv.Atoi(strings.Trim(fields[0], "[]"))
	return n, err == nil && n > 0
}

func parseIDRest(arg string) (int, string, bool) {
	fields := strings.Fields(arg)
	if len(fields) < 2 {
		return 0, "", false
	}
	n, err := strconv.Atoi(strings.Trim(fields[0], "[]"))
	if err != nil || n <= 0 {
		return 0, "", false
	}
	return n, strings.TrimSpace(strings.TrimPrefix(arg, fields[0])), true
}

func preview(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > 80 {
		return s[:80]
	}
	return s
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func parseBypassHash(caption string) string {
	for _, field := range strings.Fields(caption) {
		if strings.HasPrefix(field, "sha256:") {
			return strings.TrimPrefix(field, "sha256:")
		}
	}
	return ""
}

func validSHA256Hex(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') {
			continue
		}
		return false
	}
	return true
}

func updateJournalRefs(update telegram.Update) (string, state.UpdateRefs) {
	if update.CallbackQuery != nil {
		refs := state.UpdateRefs{
			UserID: update.CallbackQuery.From.ID,
		}
		if update.CallbackQuery.Message != nil {
			refs.MessageID = update.CallbackQuery.Message.MessageID
			refs.ChatID = update.CallbackQuery.Message.Chat.ID
		}
		return "callback_query", refs
	}
	if update.Message != nil {
		refs := state.UpdateRefs{
			ChatID:    update.Message.Chat.ID,
			MessageID: update.Message.MessageID,
		}
		if update.Message.From != nil {
			refs.UserID = update.Message.From.ID
		}
		return "message", refs
	}
	return "unknown", state.UpdateRefs{}
}

func GoVersion() string { return runtime.Version() }

func (a *App) stop() {
	select {
	case <-a.stopCh:
	default:
		close(a.stopCh)
	}
}
