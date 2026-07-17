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
	"github.com/idolum-ai/engram/internal/guide"
	"github.com/idolum-ai/engram/internal/lockfile"
	"github.com/idolum-ai/engram/internal/openai"
	"github.com/idolum-ai/engram/internal/redact"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
	"github.com/idolum-ai/engram/internal/terminalshot"
	"github.com/idolum-ai/engram/internal/tmux"
	"github.com/idolum-ai/engram/internal/version"
)

type App struct {
	Config               config.Config
	Store                *state.Store
	Telegram             *telegram.Client
	Guide                guide.Renderer
	Transcriber          voiceTranscriber
	Tmux                 tmux.Manager
	Snapshots            snapshotRenderer
	modeMu               sync.RWMutex
	mode                 string
	presentationMu       sync.RWMutex
	guideAvailable       bool
	snapshotReady        bool
	lock                 *lockfile.Lock
	startedAt            time.Time
	quitCode             int
	stopCh               chan struct{}
	runCtx               context.Context
	refreshWG            sync.WaitGroup
	schedulerWG          sync.WaitGroup
	transferWG           sync.WaitGroup
	captureSlots         chan struct{}
	guideSlots           chan struct{}
	renderSlots          chan struct{}
	transferSlots        chan struct{}
	transferQueue        chan struct{}
	summaryMu            sync.Mutex
	summaryQueued        map[int]bool
	summaryRunning       map[int]bool
	summaryForce         map[int]bool
	summaryDue           map[int]time.Time
	manualRefresh        map[int]bool
	conversationMu       sync.Mutex
	conversationEpochs   map[int]conversationEpoch
	conversationRevision uint64
	conversationGateMu   sync.Mutex
	conversationGates    map[int]*conversationGate
	closeConfirmMu       sync.Mutex
	closeConfirms        map[string]closeConfirmation
	sessionLocks         keyedMutexSet
	anchorLocks          keyedMutexSet
	disclosureLocks      keyedMutexSet
	signalRetries        sync.Map
	snapshotTextFrames   sync.Map
	sleepHook            func(time.Duration)
	refreshHook          func(context.Context, int, bool)
}

const summaryQuietPeriod = 2 * time.Second
const maxConcurrentCaptures = 2
const maxConcurrentGuideRequests = 2
const maxConcurrentSnapshotRenders = 2
const maxConcurrentTransfers = 2
const maxQueuedTransfers = 8

func New(cfg config.Config) (*App, error) {
	if err := config.EnsureDirs(cfg); err != nil {
		return nil, err
	}
	telegramClient, err := telegram.NewAt(cfg.TelegramBotToken, cfg.EffectiveTelegramAPIBase())
	if err != nil {
		return nil, fmt.Errorf("configure Telegram client: %w", err)
	}
	snapshotRenderer := terminalshot.New(cfg.SnapshotBrowser, cfg.SnapshotTheme)
	_, snapshotErr := snapshotRenderer.Probe(context.Background())
	snapshotReady := snapshotErr == nil
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
	guideRenderer := guideRendererFor(cfg)
	var transcriber voiceTranscriber
	if cfg.VoiceTranscriptionConfigured() {
		transcriber = openai.NewTranscriber(cfg.OpenAIAPIKey, cfg.OpenAITranscriptionModel)
	}
	mode, err := cfg.ResolveAnchorMode(store.Snapshot().AnchorMode, config.ModeCapabilities{
		GuideConfigured: guideRenderer != nil,
		SnapshotReady:   snapshotReady,
	})
	if err != nil {
		l.Close()
		return nil, err
	}
	if store.Snapshot().AnchorMode != mode {
		if err := store.SetAnchorMode(mode); err != nil {
			l.Close()
			return nil, err
		}
	}
	return &App{
		Config:             cfg,
		Store:              store,
		Telegram:           telegramClient,
		Guide:              guideRenderer,
		Transcriber:        transcriber,
		Tmux:               tmux.New(tmux.ExecRunner{}),
		Snapshots:          snapshotRenderer,
		mode:               mode,
		guideAvailable:     guideRenderer != nil,
		snapshotReady:      snapshotReady,
		lock:               l,
		startedAt:          time.Now().UTC(),
		stopCh:             make(chan struct{}),
		captureSlots:       make(chan struct{}, maxConcurrentCaptures),
		guideSlots:         make(chan struct{}, maxConcurrentGuideRequests),
		renderSlots:        make(chan struct{}, maxConcurrentSnapshotRenders),
		transferSlots:      make(chan struct{}, maxConcurrentTransfers),
		transferQueue:      make(chan struct{}, maxQueuedTransfers),
		summaryQueued:      map[int]bool{},
		summaryRunning:     map[int]bool{},
		summaryForce:       map[int]bool{},
		summaryDue:         map[int]time.Time{},
		manualRefresh:      map[int]bool{},
		conversationEpochs: map[int]conversationEpoch{},
		conversationGates:  map[int]*conversationGate{},
		closeConfirms:      map[string]closeConfirmation{},
	}, nil
}

func guideRendererFor(cfg config.Config) guide.Renderer {
	if !cfg.GuideConfigured() {
		return nil
	}
	switch cfg.EffectiveLLMProvider() {
	case config.LLMProviderAnthropic:
		return anthropic.New(cfg.AnthropicAPIKey, cfg.AnthropicModel)
	case config.LLMProviderOpenAI:
		return openai.New(cfg.OpenAIAPIKey, cfg.OpenAIModel)
	default:
		return nil
	}
}

func modeAvailable(mode string, guideReady, snapshotReady bool) bool {
	switch mode {
	case config.AnchorModeGuide:
		return guideReady
	case config.AnchorModeSnapshot:
		return snapshotReady
	default:
		return false
	}
}

func (a *App) anchorMode() string {
	a.modeMu.RLock()
	defer a.modeMu.RUnlock()
	if a.mode == "" {
		return a.Config.EffectiveAnchorMode()
	}
	return a.mode
}

func (a *App) snapshotAnchors() bool { return a.anchorMode() == config.AnchorModeSnapshot }

func (a *App) setAnchorMode(mode string) {
	a.modeMu.Lock()
	a.mode = mode
	a.modeMu.Unlock()
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
	defer func() {
		cancel()
		a.schedulerWG.Wait()
		a.refreshWG.Wait()
		a.transferWG.Wait()
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
			kind, refs := a.updateJournalRefs(update)
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
		_ = a.audit("auth.reject", "rejected", map[string]any{"kind": "message"})
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
	if msg.Voice != nil && msg.ReplyToMessage != nil {
		return a.handleVoiceReply(ctx, msg).status("voice_reply")
	}
	if doc, ok := msg.FileAttachment(); ok {
		return a.handleAttachment(ctx, msg, doc).status("attachment")
	}
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return "skipped_empty_message"
	}
	if input, ok := escapedSlashInput(text); ok {
		if msg.ReplyToMessage != nil {
			if ts, targetState, found := a.Store.FindReplyTarget(msg.Chat.ID, msg.ReplyToMessage.MessageID); found && targetState == state.ReplyTargetCurrent {
				result := a.sendReplyInput(ctx, ts, msg.Chat.ID, msg.ReplyToMessage.MessageID, input)
				if !result.OK() {
					a.reply(ctx, msg, result.Message)
				}
				return result.status("anchor_reply")
			} else if found && targetState == state.ReplyTargetStale {
				a.reply(ctx, msg, staleAlternateReply(ts.ID))
				return "anchor_reply_stale"
			}
		}
		a.reply(ctx, msg, "reply to a session anchor to send slash input; for example, //clear sends /clear")
		return "handled_unroutable_slash_input"
	}
	if strings.HasPrefix(text, "/") {
		return a.handleCommand(ctx, msg, text)
	}
	if msg.ReplyToMessage != nil {
		if ts, targetState, found := a.Store.FindReplyTarget(msg.Chat.ID, msg.ReplyToMessage.MessageID); found && targetState == state.ReplyTargetCurrent {
			result := a.sendReplyInput(ctx, ts, msg.Chat.ID, msg.ReplyToMessage.MessageID, text)
			if !result.OK() {
				a.reply(ctx, msg, result.Message)
			}
			return result.status("anchor_reply")
		} else if found && targetState == state.ReplyTargetStale {
			a.reply(ctx, msg, staleAlternateReply(ts.ID))
			return "anchor_reply_stale"
		}
		a.reply(ctx, msg, "session not found for this reply; use /sessions to find an active anchor")
		return "anchor_reply_user_error"
	}
	return a.newSession(ctx, msg, text).status("new_session")
}

func staleAlternateReply(id int) string {
	return fmt.Sprintf("That view of [%d] is no longer current. Reply to its latest view or live anchor.", id)
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
	case "status":
		a.reply(ctx, msg, a.statusText())
	case "mode":
		if strings.TrimSpace(args) == "" {
			a.reply(ctx, msg, a.modeText())
			break
		}
		result := a.switchAnchorMode(ctx, args)
		status = result.status("command")
		a.reply(ctx, msg, result.Message)
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
			status = "command_user_error"
			a.reply(ctx, msg, "usage: /new <text>")
			return
		}
		status = a.newSession(ctx, msg, args).status("command")
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
			status = "command_user_error"
			a.reply(ctx, msg, "usage: /rename <id> <name>")
			return
		}
		status = a.rename(ctx, id, rest, msg).status("command")
	case "cwd":
		id, ok := parseID(args)
		if !ok {
			status = "command_user_error"
			a.reply(ctx, msg, "usage: /cwd <id>")
			return
		}
		status = a.cwd(ctx, id, msg).status("command")
	case "cd":
		id, rest, ok := parseIDRest(args)
		if !ok || strings.TrimSpace(rest) == "" {
			status = "command_user_error"
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
			status = "command_user_error"
			a.reply(ctx, msg, "usage: /watch <id>")
			return
		}
		result := a.watchSession(ctx, id, 0)
		status = result.status("command")
		if !result.OK() {
			a.reply(ctx, msg, result.Message)
		}
	case "dump":
		status = a.captureFile(ctx, msg, args, true).status("command")
	case "raw":
		status = a.captureFile(ctx, msg, args, false).status("command")
	case "close":
		id, ok := parseID(args)
		if !ok {
			status = "command_user_error"
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
		status = a.download(ctx, msg, args).status("command")
	case "logs":
		a.logs(ctx, msg)
	case "stop", "unwatch":
		id, ok := parseID(args)
		if !ok {
			status = "command_user_error"
			a.reply(ctx, msg, "usage: /"+cmd+" <id>")
			return
		}
		ok, err := a.stopWatching(id)
		if err != nil {
			status = "command_state_failed"
			a.reply(ctx, msg, "state error: "+err.Error())
			return
		}
		if !ok {
			status = "command_user_error"
			a.reply(ctx, msg, "session not found")
			return
		}
		a.reconcileAnchorPresentation(ctx, id)
		a.reply(ctx, msg, fmt.Sprintf("[%d] watch stopped", id))
	case "restart":
		a.reply(ctx, msg, "Engram restarting. tmux sessions remain open.")
		a.quitCode = 2
		a.stop()
	case "attachment-bypass", "attachment_bypass":
		hash := parseBypassHash(args)
		if hash == "" || !validSHA256Hex(hash) {
			status = "command_user_error"
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
			status = "command_state_failed"
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

func (a *App) stopWatching(id int) (bool, error) {
	lock := a.anchorMutex(id)
	lock.Lock()
	defer lock.Unlock()
	_, ok, err := a.Store.UpdateSession(id, func(ts *state.TerminalSession) { ts.WatchEnabled = false })
	if ok && err == nil {
		a.resetConversationEpochLocked(id)
	}
	return ok, err
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
		return redact.Secrets(v, a.Config.TelegramBotToken, a.Config.AnthropicAPIKey, a.Config.OpenAIAPIKey)
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

func (a *App) redactText(text string) string {
	return redact.Secrets(text, a.Config.TelegramBotToken, a.Config.AnthropicAPIKey, a.Config.OpenAIAPIKey)
}

func (a *App) redactSessionPresentation(ts *state.TerminalSession) {
	ts.Title = a.redactText(ts.Title)
	ts.LastSummary = a.redactText(ts.LastSummary)
	ts.LastKnownCWD = a.redactText(ts.LastKnownCWD)
}

func (a *App) renderLocal(ts state.TerminalSession, summary string) string {
	a.redactSessionPresentation(&ts)
	references := renderReferences(a.visibleReferences(ts.LastRawCapture), true, maxGuideReferenceBytes)
	return renderLocalWithReferences(ts, a.redactText(summary), references)
}

func (a *App) visibleReferences(capture string) visibleReferences {
	return visibleReferencesForCapture(capture, a.Config.TelegramBotToken, a.Config.AnthropicAPIKey, a.Config.OpenAIAPIKey)
}

func (a *App) statusText() string {
	st := a.Store.Snapshot()
	space := diskFree(a.Config.ArtifactDir())
	guideStatus := "unavailable"
	if a.guideAvailable {
		guideStatus = "configured, not probed (" + a.Config.EffectiveLLMProvider() + "/" + a.Config.GuideModel() + ")"
	}
	voiceStatus := "path (local attachment)"
	if a.Config.EffectiveVoiceInputMode() == config.VoiceInputModeTranscribe {
		voiceStatus = "transcribe, configured but not probed (openai/" + a.Config.OpenAITranscriptionModel + ")"
	}
	return fmt.Sprintf("Engram status\nversion: %s\nuptime: %s\nsessions: %d\nanchor mode: %s\nguide: %s\nvoice input: %s\nsnapshots: %s\nstate: %s\naudit: %s\nattachments: %s\n/tmp free: %d\nlast poll: %s\nlast update: %d\nupdate journal: %d\nlast guide: %s\nlast guide error: %s",
		version.String(),
		time.Since(a.startedAt).Round(time.Second),
		len(st.TerminalSessions),
		a.anchorMode(),
		guideStatus,
		voiceStatus,
		a.snapshotStatus(),
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
	return renderLocalWithReferences(ts, summary, renderVisibleReferences(ts.LastRawCapture))
}

func renderLocalWithReferences(ts state.TerminalSession, summary, references string) string {
	title := firstNonEmpty(ts.Title, "-")
	if len(title) > 40 {
		title = headUTF8(title, 40)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[%d] %s  %s\n", ts.ID, ts.State, title)
	if ts.LastKnownCWD != "" {
		fmt.Fprintf(&b, "cwd: %s\n", ts.LastKnownCWD)
	}
	b.WriteString("\n")
	b.WriteString(summary)
	if references != "" {
		b.WriteString("\n\n")
		b.WriteString(references)
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
		return headUTF8(s, 80)
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

func (a *App) updateJournalRefs(update telegram.Update) (string, state.UpdateRefs) {
	if update.CallbackQuery != nil {
		if !a.callbackAuthorized(*update.CallbackQuery) {
			return "callback_query", state.UpdateRefs{}
		}
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
		if !a.authorized(update.Message) {
			return "message", state.UpdateRefs{}
		}
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
