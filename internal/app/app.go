package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	_ = a.audit("service.start", "ok", map[string]any{"version": version.String()})
	a.registerCommands(ctx)
	schedulerCtx, cancelScheduler := context.WithCancel(ctx)
	defer cancelScheduler()
	go a.scheduler(schedulerCtx)
	offset := a.Store.Snapshot().LastUpdateID + 1
	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			_ = a.audit("service.stop", "context", nil)
			return a.quitCode
		case <-a.stopCh:
			_ = a.audit("service.stop", "requested", map[string]any{"code": a.quitCode})
			return a.quitCode
		default:
		}
		updates, err := a.Telegram.GetUpdates(ctx, offset, a.Config.TelegramPollTimeoutSeconds)
		if err != nil {
			_ = a.audit("telegram.poll", "failed", err.Error())
			time.Sleep(backoff)
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		for _, update := range updates {
			if update.UpdateID >= offset {
				offset = update.UpdateID + 1
			}
			kind, refs := updateJournalRefs(update)
			_ = a.Store.MarkPoll(update.UpdateID, kind, refs)
			status := a.handleUpdate(ctx, update)
			_ = a.Store.RecordUpdate(update.UpdateID, kind, status, "", refs)
			if a.quitCode != 0 || ctx.Err() != nil {
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
	_ = a.Store.MarkMessage(key)
	if doc, ok := msg.FileAttachment(); ok {
		a.handleAttachment(ctx, msg, doc)
		return "handled_attachment"
	}
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return "skipped_empty_message"
	}
	if strings.HasPrefix(text, "/") {
		a.handleCommand(ctx, msg, text)
		return "handled_command"
	}
	if msg.ReplyToMessage != nil {
		if ts, ok := a.Store.FindByAnchor(msg.Chat.ID, msg.ReplyToMessage.MessageID); ok {
			a.sendInput(ctx, ts.ID, text, "command", true)
			return "handled_anchor_reply"
		}
	}
	a.newSession(ctx, msg, text)
	return "handled_new_session"
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

func (a *App) handleCallback(ctx context.Context, cb telegram.CallbackQuery) string {
	if cb.From.ID != a.Config.TelegramAllowedUserID || cb.Message == nil || cb.Message.Chat.ID != a.Config.TelegramChatID {
		return "rejected_unauthorized_callback"
	}
	parts := strings.SplitN(cb.Data, ":", 2)
	if len(parts) != 2 {
		_ = a.Telegram.AnswerCallback(ctx, cb.ID, "unknown action")
		return "skipped_unknown_callback"
	}
	switch parts[0] {
	case "refresh":
		id, err := strconv.Atoi(parts[1])
		if err != nil {
			_ = a.Telegram.AnswerCallback(ctx, cb.ID, "bad session id")
			return "failed_bad_callback_id"
		}
		_ = a.Telegram.AnswerCallback(ctx, cb.ID, "refreshing")
		a.clearHaikuCaptureHistory(id)
		a.queueRefresh(id, true, 0)
	case "key":
		id, preset, ok := parseKeyCallback(parts[1])
		if !ok {
			_ = a.Telegram.AnswerCallback(ctx, cb.ID, "bad key")
			return "failed_bad_callback_key"
		}
		action, ok := anchorKeyAction(preset)
		if !ok {
			_ = a.Telegram.AnswerCallback(ctx, cb.ID, "unknown key")
			return "failed_unknown_callback_key"
		}
		_ = a.Telegram.AnswerCallback(ctx, cb.ID, "sent "+action.Label)
		a.sendKeyGroups(ctx, id, action.Groups, action.Label, action.Delay)
	case "watch":
		id, err := strconv.Atoi(parts[1])
		if err != nil {
			_ = a.Telegram.AnswerCallback(ctx, cb.ID, "bad session id")
			return "failed_bad_callback_id"
		}
		_ = a.Telegram.AnswerCallback(ctx, cb.ID, "watching")
		a.watchSession(ctx, id, cb.Message.MessageID)
	case "close":
		id, err := strconv.Atoi(parts[1])
		if err != nil {
			_ = a.Telegram.AnswerCallback(ctx, cb.ID, "bad session id")
			return "failed_bad_callback_id"
		}
		_ = a.Telegram.AnswerCallback(ctx, cb.ID, "closing")
		a.closeSession(ctx, id)
	case "attach":
		_ = a.Telegram.AnswerCallback(ctx, cb.ID, "attaching")
		msg := *cb.Message
		msg.From = &cb.From
		a.attachTarget(ctx, msg, parts[1])
	default:
		_ = a.Telegram.AnswerCallback(ctx, cb.ID, "unknown action")
		return "skipped_unknown_callback"
	}
	return "handled_callback"
}

type anchorKeyPreset struct {
	Label  string
	Groups [][]string
	Delay  time.Duration
}

func parseKeyCallback(value string) (int, string, bool) {
	idText, preset, ok := strings.Cut(value, ":")
	if !ok {
		return 0, "", false
	}
	id, err := strconv.Atoi(idText)
	if err != nil || id <= 0 || strings.TrimSpace(preset) == "" {
		return 0, "", false
	}
	return id, preset, true
}

func anchorKeyAction(preset string) (anchorKeyPreset, bool) {
	switch preset {
	case "esc":
		return anchorKeyPreset{Label: "Esc", Groups: [][]string{{"Escape"}}}, true
	case "esc2":
		return anchorKeyPreset{Label: "Esc Esc", Groups: [][]string{{"Escape"}, {"Escape"}}, Delay: 500 * time.Millisecond}, true
	case "ctrl-c":
		return anchorKeyPreset{Label: "Ctrl+C", Groups: [][]string{{"C-c"}}}, true
	case "ctrl-d":
		return anchorKeyPreset{Label: "Ctrl+D", Groups: [][]string{{"C-d"}}}, true
	case "enter":
		return anchorKeyPreset{Label: "Enter", Groups: [][]string{{"Enter"}}}, true
	default:
		return anchorKeyPreset{}, false
	}
}

func (a *App) handleCommand(ctx context.Context, msg telegram.Message, text string) {
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
		a.attachTarget(ctx, msg, args)
	case "new":
		if args == "" {
			a.reply(ctx, msg, "usage: /new <text>")
			return
		}
		a.newSession(ctx, msg, args)
	case "send":
		id, rest, ok := parseIDRest(args)
		if !ok {
			a.reply(ctx, msg, "usage: /send <id> <text>")
			return
		}
		a.sendInput(ctx, id, rest, "command", true)
	case "text":
		id, rest, ok := parseIDRest(args)
		if !ok {
			a.reply(ctx, msg, "usage: /text <id> <text>")
			return
		}
		a.sendInput(ctx, id, rest, "text", false)
	case "key":
		id, rest, ok := parseIDRest(args)
		if !ok {
			a.reply(ctx, msg, "usage: /key <id> <keys...>")
			return
		}
		a.sendKeys(ctx, id, strings.Fields(rest))
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
		a.cd(ctx, id, rest)
	case "watch":
		id, ok := parseID(args)
		if !ok {
			a.reply(ctx, msg, "usage: /watch <id>")
			return
		}
		a.watchSession(ctx, id, 0)
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
		a.closeSession(ctx, id)
	case "attachments":
		a.attachments(ctx, msg)
	case "download":
		a.download(ctx, msg, args)
	case "logs":
		a.logs(ctx, msg)
	case "stop":
		id, ok := parseID(args)
		if !ok {
			a.reply(ctx, msg, "usage: /stop <id>")
			return
		}
		a.Store.UpdateSession(id, func(ts *state.TerminalSession) { ts.WatchEnabled = false })
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
	case "attachment-bypass":
		hash := parseBypassHash(args)
		if hash == "" || !validSHA256Hex(hash) {
			a.reply(ctx, msg, "usage: /attachment-bypass sha256:<hash>")
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
		a.reply(ctx, msg, "unknown command; try /help")
	}
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

func (a *App) newSession(ctx context.Context, msg telegram.Message, input string) {
	tmuxCtx, cancel := tmux.TimeoutContext(ctx)
	defer cancel()
	sessionName, err := a.targetTmuxSession(tmuxCtx, msg.Chat.ID)
	if err != nil {
		a.reply(ctx, msg, "tmux error: "+err.Error())
		return
	}
	title := tmux.WindowTitle(0, input)
	sessionID, windowID, paneID, err := a.Tmux.NewWindow(tmuxCtx, sessionName, a.Config.Workdir, title)
	if err != nil {
		a.reply(ctx, msg, "tmux error: "+err.Error())
		return
	}
	ts, err := a.Store.AllocateSession(msg.Chat.ID, msg.From.ID, sessionName, windowID, paneID, title)
	if err != nil {
		a.reply(ctx, msg, "state error: "+err.Error())
		return
	}
	ts.TmuxSessionID = sessionID
	a.Store.UpdateSession(ts.ID, func(s *state.TerminalSession) {
		s.TmuxSessionID = sessionID
		s.LastInputPreview = preview(input)
		s.LastInputMode = "command"
	})
	if err := a.Tmux.SendCommand(tmuxCtx, paneID, input); err != nil {
		a.reply(ctx, msg, "tmux send error: "+err.Error())
		return
	}
	resp, err := a.sendAnchor(ctx, msg.Chat.ID, renderLocal(ts, "starting; waiting for terminal output"), msg.MessageID, telegram.RefreshMarkup(ts.ID))
	if err == nil {
		a.Store.UpdateSession(ts.ID, func(s *state.TerminalSession) {
			s.AnchorChatID = resp.Chat.ID
			s.AnchorMessageID = resp.MessageID
			s.WatchEnabled = true
		})
	}
	a.queueRefresh(ts.ID, true, summaryQuietPeriod)
}

func (a *App) targetTmuxSession(ctx context.Context, chatID int64) (string, error) {
	if strings.TrimSpace(a.Config.TmuxSession) != "" {
		name := strings.TrimSpace(a.Config.TmuxSession)
		return name, a.Tmux.EnsureSession(ctx, name, a.Config.Workdir)
	}
	sessions, err := a.Tmux.ListSessions(ctx)
	if err == nil && len(sessions) > 0 {
		return sessions[0].Name, nil
	}
	name := tmux.SessionName(chatID)
	return name, a.Tmux.EnsureSession(ctx, name, a.Config.Workdir)
}

func (a *App) attachTarget(ctx context.Context, msg telegram.Message, target string) {
	tmuxCtx, cancel := tmux.TimeoutContext(ctx)
	defer cancel()
	window, err := a.Tmux.ResolveTarget(tmuxCtx, strings.TrimSpace(target))
	if err != nil {
		a.reply(ctx, msg, "tmux error: "+err.Error())
		return
	}
	if existing, ok := a.Store.FindByPane(window.PaneID); ok {
		a.reply(ctx, msg, fmt.Sprintf("%s is already tracked as [%d]", window.PaneID, existing.ID))
		return
	}
	title := tmux.AttachedTitle(window)
	ts, err := a.Store.AllocateSession(msg.Chat.ID, msg.From.ID, window.SessionName, window.ID, window.PaneID, title)
	if err != nil {
		a.reply(ctx, msg, "state error: "+err.Error())
		return
	}
	a.Store.UpdateSession(ts.ID, func(s *state.TerminalSession) {
		s.LastKnownCWD = window.CurrentPath
		s.LastInputPreview = "attached " + strings.TrimSpace(target)
		s.LastInputMode = "attach"
	})
	resp, err := a.sendAnchor(ctx, msg.Chat.ID, renderLocal(ts, "attached existing tmux target; waiting for terminal output"), msg.MessageID, telegram.RefreshMarkup(ts.ID))
	if err == nil {
		a.Store.UpdateSession(ts.ID, func(s *state.TerminalSession) {
			s.AnchorChatID = resp.Chat.ID
			s.AnchorMessageID = resp.MessageID
			s.WatchEnabled = true
		})
	} else {
		_ = a.audit("telegram.send", "failed", map[string]any{"command": "attach", "error": err.Error()})
	}
	a.queueRefresh(ts.ID, true, summaryQuietPeriod)
}

func (a *App) sendInput(ctx context.Context, id int, text, mode string, enter bool) {
	ts, ok := a.Store.FindSession(id)
	if !ok {
		return
	}
	tctx, cancel := tmux.TimeoutContext(ctx)
	defer cancel()
	var err error
	if enter {
		err = a.Tmux.SendCommand(tctx, ts.TmuxPaneID, text)
	} else {
		err = a.Tmux.SendText(tctx, ts.TmuxPaneID, text)
	}
	if err != nil {
		_ = a.audit("tmux.send", "failed", map[string]any{"session_id": id, "pane_id": ts.TmuxPaneID, "mode": mode, "enter": enter, "error": err.Error()})
		a.updateAnchorLocal(ctx, id, "tmux send error: "+err.Error(), true)
		return
	}
	_ = a.audit("tmux.send", "ok", map[string]any{"session_id": id, "pane_id": ts.TmuxPaneID, "mode": mode, "enter": enter})
	a.Store.UpdateSession(id, func(s *state.TerminalSession) {
		s.LastInputPreview = preview(text)
		s.LastInputMode = mode
		s.LastActivityAt = time.Now().UTC()
		s.PendingRefresh = true
	})
	a.refreshSoon(id)
}

func (a *App) sendKeys(ctx context.Context, id int, keys []string) {
	a.sendKeyGroups(ctx, id, [][]string{keys}, strings.Join(keys, " "), 0)
}

func (a *App) sendKeyGroups(ctx context.Context, id int, groups [][]string, preview string, delay time.Duration) {
	if len(groups) == 0 {
		a.updateAnchorLocal(ctx, id, "missing keys", true)
		return
	}
	for _, keys := range groups {
		if err := tmux.ValidKeys(keys); err != nil {
			a.updateAnchorLocal(ctx, id, err.Error(), true)
			return
		}
	}
	ts, ok := a.Store.FindSession(id)
	if !ok {
		return
	}
	tctx, cancel := tmux.TimeoutContext(ctx)
	defer cancel()
	for i, keys := range groups {
		if err := a.Tmux.SendKeys(tctx, ts.TmuxPaneID, keys); err != nil {
			a.updateAnchorLocal(ctx, id, "tmux key error: "+err.Error(), true)
			return
		}
		if delay > 0 && i < len(groups)-1 {
			a.sleep(delay)
		}
	}
	a.Store.UpdateSession(id, func(s *state.TerminalSession) {
		s.LastInputPreview = firstNonEmpty(strings.TrimSpace(preview), flattenKeyPreview(groups))
		s.LastInputMode = "keys"
		s.LastActivityAt = time.Now().UTC()
		s.PendingRefresh = true
	})
	a.refreshSoon(id)
}

func flattenKeyPreview(groups [][]string) string {
	var keys []string
	for _, group := range groups {
		keys = append(keys, group...)
	}
	return strings.Join(keys, " ")
}

func (a *App) rename(ctx context.Context, id int, name string, msg telegram.Message) {
	_, ok, _ := a.Store.UpdateSession(id, func(s *state.TerminalSession) { s.Title = strings.TrimSpace(name) })
	if !ok {
		a.reply(ctx, msg, "session not found")
		return
	}
	a.reply(ctx, msg, fmt.Sprintf("[%d] renamed to %s", id, strings.TrimSpace(name)))
}

func (a *App) cwd(ctx context.Context, id int, msg telegram.Message) {
	ts, ok := a.Store.FindSession(id)
	if !ok {
		a.reply(ctx, msg, "session not found")
		return
	}
	tctx, cancel := tmux.TimeoutContext(ctx)
	defer cancel()
	cwd, err := a.Tmux.PaneCWD(tctx, ts.TmuxPaneID)
	if err != nil {
		a.reply(ctx, msg, "cwd error: "+err.Error())
		return
	}
	a.Store.UpdateSession(id, func(s *state.TerminalSession) { s.LastKnownCWD = cwd })
	a.reply(ctx, msg, fmt.Sprintf("[%d] cwd\n%s", id, cwd))
}

func (a *App) cd(ctx context.Context, id int, path string) {
	cmd := "cd " + tmux.ShellQuote(config.ExpandPath(strings.TrimSpace(path)))
	a.sendInput(ctx, id, cmd, "command", true)
}

func (a *App) watchSession(ctx context.Context, id int, replyTo int) {
	ts, ok := a.Store.FindSession(id)
	if !ok {
		return
	}
	if ts.AnchorMessageID == 0 {
		msg, err := a.sendAnchor(ctx, a.Config.TelegramChatID, renderLocal(ts, firstNonEmpty(ts.LastSummary, "watching")), replyTo, telegram.RefreshMarkup(id))
		if err == nil {
			a.Store.UpdateSession(id, func(s *state.TerminalSession) {
				s.AnchorChatID = msg.Chat.ID
				s.AnchorMessageID = msg.MessageID
				s.WatchEnabled = true
			})
		}
		return
	}
	a.Store.UpdateSession(id, func(s *state.TerminalSession) { s.WatchEnabled = true })
	a.queueRefresh(id, true, 0)
}

func (a *App) closeSession(ctx context.Context, id int) {
	ts, ok := a.Store.FindSession(id)
	if !ok {
		return
	}
	tctx, cancel := tmux.TimeoutContext(ctx)
	defer cancel()
	_ = a.Tmux.KillWindow(tctx, ts.TmuxWindowID)
	a.Store.UpdateSession(id, func(s *state.TerminalSession) {
		s.State = state.TerminalClosed
		s.WatchEnabled = false
		s.LastSummary = "summary:\n- Session closed by request."
	})
	a.updateAnchorLocal(ctx, id, "summary:\n- Session closed by request.", true)
}

func (a *App) sessions(ctx context.Context, msg telegram.Message) {
	st := a.Store.Snapshot()
	var ids []int
	var b strings.Builder
	b.WriteString("Engram sessions\n\n")
	for _, ts := range st.TerminalSessions {
		ids = append(ids, ts.ID)
		fmt.Fprintf(&b, "[%d] %s  %s  last: %s\n", ts.ID, ts.State, firstNonEmpty(ts.Title, "-"), firstNonEmpty(ts.LastInputPreview, "-"))
	}
	if len(ids) == 0 {
		b.WriteString("No sessions.")
	}
	b.WriteString("\n\nTmux sessions\n\n")
	attachTargets := a.writeTmuxSessions(ctx, &b)
	if _, err := a.Telegram.SendMessage(ctx, msg.Chat.ID, b.String(), msg.MessageID, telegram.SessionListMarkup(ids, attachTargets)); err != nil {
		_ = a.audit("telegram.send", "failed", map[string]any{"command": "sessions", "error": err.Error()})
	}
}

func (a *App) writeTmuxSessions(ctx context.Context, b *strings.Builder) []telegram.AttachTarget {
	tctx, cancel := tmux.TimeoutContext(ctx)
	defer cancel()
	sessions, err := a.Tmux.ListSessions(tctx)
	if err != nil {
		fmt.Fprintf(b, "Unavailable: %s", err)
		return nil
	}
	if len(sessions) == 0 {
		b.WriteString("No tmux sessions.")
		return nil
	}
	selected := strings.TrimSpace(a.Config.TmuxSession)
	if selected == "" {
		selected = sessions[0].Name
	}
	for _, session := range sessions {
		marker := " "
		if session.Name == selected {
			marker = "*"
		}
		fmt.Fprintf(b, "%s %s  id:%s  windows:%s  attached:%s\n", marker, session.Name, firstNonEmpty(session.ID, "-"), firstNonEmpty(session.Windows, "?"), firstNonEmpty(session.Attached, "?"))
	}
	windows, err := a.Tmux.ListWindows(tctx)
	if err != nil {
		fmt.Fprintf(b, "\nWindows unavailable: %s", err)
		return nil
	}
	if len(windows) == 0 {
		return nil
	}
	var attachTargets []telegram.AttachTarget
	b.WriteString("\nWindows\n")
	for _, window := range windows {
		target := window.SessionName + ":" + window.Index
		tracked := ""
		if ts, ok := a.Store.FindByPane(window.PaneID); ok {
			tracked = fmt.Sprintf(" tracked:[%d]", ts.ID)
		}
		active := ""
		if window.Active == "1" {
			active = " active"
		}
		fmt.Fprintf(b, "%s  id:%s  %s  cmd:%s%s%s\n", target, firstNonEmpty(window.ID, "-"), firstNonEmpty(window.Name, "-"), firstNonEmpty(window.CurrentCmd, "-"), active, tracked)
		if tracked == "" {
			attachTargets = append(attachTargets, telegram.AttachTarget{Label: target, Target: target})
		}
	}
	b.WriteString("\nUse /attach <target>, for example /attach " + windows[0].SessionName + ":" + windows[0].Index)
	return attachTargets
}

func (a *App) refreshSession(ctx context.Context, id int, force bool) {
	ts, ok := a.Store.FindSession(id)
	if !ok || ts.TmuxPaneID == "" {
		return
	}
	if !force && time.Since(ts.LastAnchorEditAt) < 10*time.Second {
		return
	}
	tctx, cancel := tmux.TimeoutContext(ctx)
	defer cancel()
	capture, err := a.Tmux.CaptureVisible(tctx, ts.TmuxPaneID)
	if err != nil {
		a.Store.UpdateSession(id, func(s *state.TerminalSession) {
			s.State = state.TerminalLost
			s.LastTelegramError = err.Error()
		})
		a.updateAnchorLocal(ctx, id, "lost: "+err.Error(), true)
		return
	}
	hash := sha(capture)
	if hash == ts.LastRawCaptureHash && !force {
		return
	}
	summary, err := a.guideSummary(ctx, ts, capture)
	if err != nil {
		_ = a.Store.NoteHaiku(err.Error())
		if ts.LastSummary != "" {
			summary = ts.LastSummary + "\n\n[summary stale: " + err.Error() + "]"
		} else {
			summary = "summary unavailable: " + err.Error()
		}
	} else {
		_ = a.Store.NoteHaiku("")
	}
	a.Store.UpdateSession(id, func(s *state.TerminalSession) {
		s.LastRawCapture = capture
		s.LastRawCaptureHash = hash
		s.LastSummary = summary
		s.LastSummaryHash = sha(summary)
		s.LastSummaryModel = a.Config.AnthropicModel
		s.PendingRefresh = false
	})
	a.updateAnchorLocal(ctx, id, summary, force)
}

func (a *App) guideSummary(ctx context.Context, ts state.TerminalSession, capture string) (string, error) {
	visibleForHaiku, repeatedLines := a.prepareHaikuVisibleCapture(ts.ID, capture)
	input := anthropic.SummaryInput{
		SessionID:       ts.ID,
		State:           string(ts.State),
		LastInput:       ts.LastInputPreview,
		LastInputMode:   ts.LastInputMode,
		PreviousSummary: ts.LastSummary,
		VisibleCapture:  visibleForHaiku,
	}
	report, err := a.Anthropic.Guide(ctx, input)
	if err != nil {
		return "", err
	}
	if !report.WantsFullBuffer() {
		return report.TelegramText(), nil
	}
	tctx, cancel := tmux.TimeoutContext(ctx)
	defer cancel()
	full, err := a.Tmux.CaptureFull(tctx, ts.TmuxPaneID)
	if err != nil || strings.TrimSpace(full) == "" {
		return report.TelegramText(), nil
	}
	input.FullCapture = filterCaptureLines(full, repeatedLines)
	refined, err := a.Anthropic.Guide(ctx, input)
	if err != nil {
		return report.TelegramText(), nil
	}
	return refined.TelegramText(), nil
}

func (a *App) filterHaikuVisibleCapture(sessionID int, capture string) string {
	filtered, _ := a.prepareHaikuVisibleCapture(sessionID, capture)
	return filtered
}

func (a *App) prepareHaikuVisibleCapture(sessionID int, capture string) (string, map[string]bool) {
	a.captureMu.Lock()
	defer a.captureMu.Unlock()
	if a.captureHistory == nil {
		a.captureHistory = map[int][]map[string]bool{}
	}
	history := a.captureHistory[sessionID]
	repeated := map[string]bool{}
	for _, previous := range history {
		for line := range previous {
			repeated[line] = true
		}
	}
	current := map[string]bool{}
	for _, line := range strings.Split(capture, "\n") {
		current[line] = true
	}
	history = append(history, current)
	if len(history) > haikuCaptureHistoryLimit {
		history = history[len(history)-haikuCaptureHistoryLimit:]
	}
	a.captureHistory[sessionID] = history
	return filterCaptureLines(capture, repeated), repeated
}

func filterCaptureLines(capture string, repeated map[string]bool) string {
	if len(repeated) == 0 {
		return capture
	}
	lines := strings.Split(capture, "\n")
	filtered := make([]string, 0, len(lines))
	for _, line := range lines {
		if repeated[line] {
			continue
		}
		filtered = append(filtered, line)
	}
	return strings.Join(filtered, "\n")
}

func (a *App) clearHaikuCaptureHistory(sessionID int) {
	a.captureMu.Lock()
	defer a.captureMu.Unlock()
	delete(a.captureHistory, sessionID)
}

func (a *App) updateAnchorLocal(ctx context.Context, id int, summary string, final bool) {
	ts, ok := a.Store.FindSession(id)
	if !ok || ts.AnchorMessageID == 0 {
		return
	}
	rendered := renderLocal(ts, summary)
	renderHash := sha(rendered)
	if renderHash == ts.LastRenderHash && !final {
		return
	}
	if !final && time.Since(ts.LastAnchorEditAt) < 10*time.Second {
		return
	}
	_, err := a.editAnchor(ctx, ts.AnchorChatID, ts.AnchorMessageID, rendered, telegram.RefreshMarkup(id))
	if err != nil {
		msg, sendErr := a.sendAnchor(ctx, a.Config.TelegramChatID, rendered, 0, telegram.RefreshMarkup(id))
		if sendErr == nil {
			a.Store.UpdateSession(id, func(s *state.TerminalSession) {
				s.AnchorChatID = msg.Chat.ID
				s.AnchorMessageID = msg.MessageID
				s.LastRenderHash = renderHash
				s.LastAnchorEditAt = time.Now().UTC()
			})
		}
		return
	}
	a.Store.UpdateSession(id, func(s *state.TerminalSession) {
		s.LastRenderHash = renderHash
		s.LastAnchorEditAt = time.Now().UTC()
	})
}

func (a *App) refreshSoon(id int) {
	a.queueRefresh(id, true, summaryQuietPeriod)
}

func (a *App) queueRefresh(id int, force bool, delay time.Duration) {
	a.summaryMu.Lock()
	a.ensureSummaryQueuesLocked()
	if a.summaryRunning[id] || a.summaryQueued[id] {
		a.summaryQueued[id] = true
		a.summaryForce[id] = a.summaryForce[id] || force
		a.summaryMu.Unlock()
		return
	}
	a.summaryQueued[id] = true
	a.summaryForce[id] = force
	a.summaryMu.Unlock()
	go func() {
		a.refreshWorker(id, delay)
	}()
}

func (a *App) refreshWorker(id int, delay time.Duration) {
	for {
		if delay > 0 {
			a.sleep(delay)
		}
		a.summaryMu.Lock()
		a.ensureSummaryQueuesLocked()
		force := a.summaryForce[id]
		a.summaryQueued[id] = false
		a.summaryForce[id] = false
		a.summaryRunning[id] = true
		a.summaryMu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), 110*time.Second)
		a.runRefresh(ctx, id, force)
		cancel()

		a.summaryMu.Lock()
		a.summaryRunning[id] = false
		if !a.summaryQueued[id] {
			delete(a.summaryQueued, id)
			delete(a.summaryForce, id)
			delete(a.summaryRunning, id)
			a.summaryMu.Unlock()
			return
		}
		a.summaryMu.Unlock()
		delay = summaryQuietPeriod
	}
}

func (a *App) sleep(delay time.Duration) {
	if a.sleepHook != nil {
		a.sleepHook(delay)
		return
	}
	time.Sleep(delay)
}

func (a *App) runRefresh(ctx context.Context, id int, force bool) {
	if a.refreshHook != nil {
		a.refreshHook(ctx, id, force)
		return
	}
	a.refreshSession(ctx, id, force)
}

func (a *App) ensureSummaryQueuesLocked() {
	if a.summaryQueued == nil {
		a.summaryQueued = map[int]bool{}
	}
	if a.summaryRunning == nil {
		a.summaryRunning = map[int]bool{}
	}
	if a.summaryForce == nil {
		a.summaryForce = map[int]bool{}
	}
}

func (a *App) sendAnchor(ctx context.Context, chatID int64, text string, replyTo int, markup *telegram.InlineKeyboardMarkup) (telegram.Message, error) {
	html := telegram.MarkdownToHTML(text)
	msg, err := a.Telegram.SendHTMLMessage(ctx, chatID, html, replyTo, markup)
	if err == nil {
		return msg, nil
	}
	_ = a.audit("telegram.anchor_html", "failed", err.Error())
	return a.Telegram.SendMessage(ctx, chatID, text, replyTo, markup)
}

func (a *App) editAnchor(ctx context.Context, chatID int64, messageID int, text string, markup *telegram.InlineKeyboardMarkup) (telegram.Message, error) {
	html := telegram.MarkdownToHTML(text)
	msg, err := a.Telegram.EditHTMLMessage(ctx, chatID, messageID, html, markup)
	if err == nil {
		return msg, nil
	}
	_ = a.audit("telegram.anchor_html", "failed", err.Error())
	return a.Telegram.EditMessage(ctx, chatID, messageID, text, markup)
}

func (a *App) scheduler(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			st := a.Store.Snapshot()
			for _, ts := range st.TerminalSessions {
				if ts.WatchEnabled && ts.State != state.TerminalClosed {
					a.queueRefresh(ts.ID, false, 0)
				}
			}
		}
	}
}

func (a *App) captureFile(ctx context.Context, msg telegram.Message, arg string, full bool) {
	id, ok := parseID(arg)
	if !ok {
		if full {
			a.reply(ctx, msg, "usage: /dump <id>")
		} else {
			a.reply(ctx, msg, "usage: /raw <id>")
		}
		return
	}
	ts, ok := a.Store.FindSession(id)
	if !ok {
		a.reply(ctx, msg, "session not found")
		return
	}
	tctx, cancel := tmux.TimeoutContext(ctx)
	defer cancel()
	var text string
	var err error
	name := "raw"
	if full {
		text, err = a.Tmux.CaptureFull(tctx, ts.TmuxPaneID)
		name = "dump"
	} else {
		text, err = a.Tmux.CaptureVisible(tctx, ts.TmuxPaneID)
	}
	if err != nil {
		a.reply(ctx, msg, "capture error: "+err.Error())
		return
	}
	path := filepath.Join(a.Config.ArtifactDir(), fmt.Sprintf("engram-%s-%d-%s.txt", name, id, time.Now().UTC().Format("20060102T150405Z")))
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		a.reply(ctx, msg, "write error: "+err.Error())
		return
	}
	if _, err := a.Telegram.SendDocument(ctx, msg.Chat.ID, path, fmt.Sprintf("[%d] %s", id, name)); err != nil {
		a.reply(ctx, msg, "upload error: "+err.Error())
	}
}

func (a *App) commandsMetadata(ctx context.Context, msg telegram.Message) {
	data, err := commands.JSON()
	if err != nil {
		a.reply(ctx, msg, "commands error: "+err.Error())
		return
	}
	path := filepath.Join(a.Config.ArtifactDir(), "engram-commands-"+time.Now().UTC().Format("20060102T150405Z")+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		a.reply(ctx, msg, "write error: "+err.Error())
		return
	}
	if _, err := a.Telegram.SendDocument(ctx, msg.Chat.ID, path, "Engram command metadata"); err != nil {
		a.reply(ctx, msg, "upload error: "+err.Error())
	}
}

func (a *App) handleAttachment(ctx context.Context, msg telegram.Message, doc telegram.Document) {
	allowLarge := strings.Contains(msg.Caption, "/attachment-bypass")
	expectedHash := parseBypassHash(msg.Caption)
	if expectedHash == "" {
		if bypass, ok := a.Store.FindAttachmentBypass(msg.Chat.ID, msg.From.ID); ok {
			expectedHash = bypass.SHA256
			allowLarge = true
		}
	}
	if doc.FileSize > a.Config.AttachmentSoftMaxBytes && !allowLarge {
		space := diskFree(a.Config.ArtifactDir())
		a.reply(ctx, msg, fmt.Sprintf("attachment too large: %d bytes exceeds soft limit %d bytes\navailable /tmp/engram space: %d bytes\nresend with caption `/attachment-bypass sha256:<hash>` to bypass", doc.FileSize, a.Config.AttachmentSoftMaxBytes, space))
		return
	}
	if allowLarge && !validSHA256Hex(expectedHash) {
		a.reply(ctx, msg, "large attachment bypass requires sha256:<hash>")
		return
	}
	file, err := a.Telegram.GetFile(ctx, doc.FileID)
	if err != nil {
		a.reply(ctx, msg, "getFile error: "+err.Error())
		return
	}
	name := safeName(firstNonEmpty(doc.FileName, filepath.Base(file.FilePath), doc.FileID))
	path := filepath.Join(a.Config.AttachmentDir(), time.Now().UTC().Format("20060102T150405Z")+"-"+doc.FileID+"-"+name)
	n, err := a.Telegram.DownloadFile(ctx, file.FilePath, path, 0)
	if err != nil {
		a.reply(ctx, msg, "download error: "+err.Error())
		return
	}
	sum, err := fileSHA256(path)
	if err != nil {
		a.reply(ctx, msg, "hash error: "+err.Error())
		return
	}
	if allowLarge && expectedHash != "" && !strings.EqualFold(sum, expectedHash) {
		_ = os.Remove(path)
		a.reply(ctx, msg, fmt.Sprintf("attachment hash mismatch\nexpected: %s\nactual: %s", expectedHash, sum))
		return
	}
	if allowLarge && expectedHash != "" {
		_ = a.Store.ConsumeAttachmentBypass(msg.Chat.ID, msg.From.ID, expectedHash)
	}
	_ = a.Store.AddAttachment(state.Attachment{
		TelegramFileID:       doc.FileID,
		TelegramUniqueFileID: doc.FileUniqueID,
		ChatID:               msg.Chat.ID,
		UserID:               msg.From.ID,
		OriginalName:         doc.FileName,
		ContentType:          doc.MimeType,
		SizeBytes:            n,
		SHA256:               sum,
		StoredPath:           path,
		ReceivedAt:           time.Now().UTC(),
		BypassRequested:      allowLarge,
	})
	a.reply(ctx, msg, "attachment saved\n"+path)
}

func (a *App) attachments(ctx context.Context, msg telegram.Message) {
	st := a.Store.Snapshot()
	if len(st.Attachments) == 0 {
		a.reply(ctx, msg, "No attachments.")
		return
	}
	var b strings.Builder
	for _, att := range st.Attachments {
		fmt.Fprintf(&b, "%s  %d bytes\n%s\n", att.ReceivedAt.Format(time.RFC3339), att.SizeBytes, att.StoredPath)
	}
	a.reply(ctx, msg, b.String())
}

func (a *App) download(ctx context.Context, msg telegram.Message, path string) {
	path = strings.TrimSpace(path)
	info, err := validateDownloadPath(path)
	if err != nil {
		a.reply(ctx, msg, err.Error())
		return
	}
	if _, err := a.Telegram.SendDocument(ctx, a.Config.TelegramChatID, path, filepath.Base(path)); err != nil {
		a.reply(ctx, msg, "upload error: "+err.Error())
	}
	_ = info
}

func (a *App) logs(ctx context.Context, msg telegram.Message) {
	b, _ := os.ReadFile(a.Config.AuditPath())
	text := redact.Secrets(string(b), a.Config.TelegramBotToken, a.Config.AnthropicAPIKey)
	if len(text) > 200_000 {
		text = text[len(text)-200_000:]
	}
	path := filepath.Join(a.Config.ArtifactDir(), "engram-logs-"+time.Now().UTC().Format("20060102T150405Z")+".jsonl")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		a.reply(ctx, msg, "log write error: "+err.Error())
		return
	}
	if _, err := a.Telegram.SendDocument(ctx, msg.Chat.ID, path, "Engram logs"); err != nil {
		a.reply(ctx, msg, "log upload error: "+err.Error())
	}
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
	fmt.Fprintf(&b, "[%d] %s  %s\nlast input: %s\n\n[Haiku]\n%s",
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

func safeName(name string) string {
	name = filepath.Base(name)
	name = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == 0 || r == ':' {
			return '-'
		}
		return r
	}, name)
	if name == "." || name == "" {
		return "attachment"
	}
	return name
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

func validateDownloadPath(path string) (os.FileInfo, error) {
	path = strings.TrimSpace(path)
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("/download requires an absolute path")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("stat error: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("refusing symlink")
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	return info, nil
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

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func diskFree(path string) uint64 {
	_ = os.MkdirAll(path, 0o700)
	var stat syscallStatfs
	if err := statfs(path, &stat); err != nil {
		return 0
	}
	return stat.Bavail * uint64(stat.Bsize)
}

func GoVersion() string { return runtime.Version() }

func (a *App) stop() {
	select {
	case <-a.stopCh:
	default:
		close(a.stopCh)
	}
}
