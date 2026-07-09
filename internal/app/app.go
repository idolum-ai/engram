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
	Config    config.Config
	Store     *state.Store
	Telegram  *telegram.Client
	Anthropic *anthropic.Client
	Tmux      tmux.Manager
	lock      *lockfile.Lock
	startedAt time.Time
	quitCode  int
	stopCh    chan struct{}
}

func New(cfg config.Config) (*App, error) {
	if err := config.EnsureDirs(cfg); err != nil {
		return nil, err
	}
	key := lockfile.Key(cfg.TelegramBotToken, strconv.FormatInt(cfg.TelegramAllowedUserID, 10), strconv.FormatInt(cfg.TelegramChatID, 10))
	l, err := lockfile.Acquire(cfg.LockDir(), key)
	if err != nil {
		return nil, err
	}
	store, err := state.Open(cfg.StatePath(), cfg.AuditPath())
	if err != nil {
		l.Close()
		return nil, err
	}
	return &App{
		Config:    cfg,
		Store:     store,
		Telegram:  telegram.New(cfg.TelegramBotToken),
		Anthropic: anthropic.New(cfg.AnthropicAPIKey, cfg.AnthropicModel),
		Tmux:      tmux.New(tmux.ExecRunner{}),
		lock:      l,
		startedAt: time.Now().UTC(),
		stopCh:    make(chan struct{}),
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
	_ = a.Store.Audit("service.start", "ok", map[string]any{"version": version.String()})
	a.registerCommands(ctx)
	schedulerCtx, cancelScheduler := context.WithCancel(ctx)
	defer cancelScheduler()
	go a.scheduler(schedulerCtx)
	offset := a.Store.Snapshot().LastUpdateID + 1
	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			_ = a.Store.Audit("service.stop", "context", nil)
			return a.quitCode
		case <-a.stopCh:
			_ = a.Store.Audit("service.stop", "requested", map[string]any{"code": a.quitCode})
			return a.quitCode
		default:
		}
		updates, err := a.Telegram.GetUpdates(ctx, offset, a.Config.TelegramPollTimeoutSeconds)
		if err != nil {
			_ = a.Store.Audit("telegram.poll", "failed", err.Error())
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
			_ = a.Store.MarkPoll(update.UpdateID)
			a.handleUpdate(ctx, update)
			if a.quitCode != 0 || ctx.Err() != nil {
				return a.quitCode
			}
		}
	}
}

func (a *App) handleUpdate(ctx context.Context, update telegram.Update) {
	if update.CallbackQuery != nil {
		a.handleCallback(ctx, *update.CallbackQuery)
		return
	}
	if update.Message == nil {
		return
	}
	msg := *update.Message
	if !a.authorized(&msg) {
		_ = a.Store.Audit("auth.reject", "rejected", map[string]any{"chat_id": msg.Chat.ID})
		return
	}
	key := fmt.Sprintf("%d:%d", msg.Chat.ID, msg.MessageID)
	if a.Store.SeenMessage(key) {
		return
	}
	_ = a.Store.MarkMessage(key)
	if doc, ok := msg.FileAttachment(); ok {
		a.handleAttachment(ctx, msg, doc)
		return
	}
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return
	}
	if strings.HasPrefix(text, "/") {
		a.handleCommand(ctx, msg, text)
		return
	}
	if msg.ReplyToMessage != nil {
		if ts, ok := a.Store.FindByAnchor(msg.Chat.ID, msg.ReplyToMessage.MessageID); ok {
			a.sendInput(ctx, ts.ID, text, "command", true)
			return
		}
	}
	a.newSession(ctx, msg, text)
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

func (a *App) handleCallback(ctx context.Context, cb telegram.CallbackQuery) {
	if cb.From.ID != a.Config.TelegramAllowedUserID || cb.Message == nil || cb.Message.Chat.ID != a.Config.TelegramChatID {
		return
	}
	parts := strings.SplitN(cb.Data, ":", 2)
	if len(parts) != 2 {
		_ = a.Telegram.AnswerCallback(ctx, cb.ID, "unknown action")
		return
	}
	id, err := strconv.Atoi(parts[1])
	if err != nil {
		_ = a.Telegram.AnswerCallback(ctx, cb.ID, "bad session id")
		return
	}
	switch parts[0] {
	case "refresh":
		_ = a.Telegram.AnswerCallback(ctx, cb.ID, "refreshing")
		a.refreshSession(ctx, id, true)
	case "watch":
		_ = a.Telegram.AnswerCallback(ctx, cb.ID, "watching")
		a.watchSession(ctx, id, cb.Message.MessageID)
	case "close":
		_ = a.Telegram.AnswerCallback(ctx, cb.ID, "closing")
		a.closeSession(ctx, id)
	default:
		_ = a.Telegram.AnswerCallback(ctx, cb.ID, "unknown action")
	}
}

func (a *App) handleCommand(ctx context.Context, msg telegram.Message, text string) {
	fields := strings.Fields(text)
	cmd := strings.TrimPrefix(fields[0], "/")
	if at := strings.IndexByte(cmd, '@'); at >= 0 {
		cmd = cmd[:at]
	}
	args := strings.TrimSpace(strings.TrimPrefix(text, fields[0]))
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
		a.reply(ctx, msg, "Send the large attachment again with caption `/attachment-bypass sha256:<hash>`.")
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
		_ = a.Store.Audit("telegram.commands", "failed", err.Error())
		return
	}
	_ = a.Store.Audit("telegram.commands", "ok", map[string]any{"count": len(tgCommands)})
}

func (a *App) newSession(ctx context.Context, msg telegram.Message, input string) {
	tmuxCtx, cancel := tmux.TimeoutContext(ctx)
	defer cancel()
	sessionName := tmux.SessionName(msg.Chat.ID)
	if err := a.Tmux.EnsureSession(tmuxCtx, sessionName, a.Config.Workdir); err != nil {
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
	resp, err := a.Telegram.SendMessage(ctx, msg.Chat.ID, renderLocal(ts, "starting; waiting for terminal output"), msg.MessageID, telegram.RefreshMarkup(ts.ID))
	if err == nil {
		a.Store.UpdateSession(ts.ID, func(s *state.TerminalSession) {
			s.AnchorChatID = resp.Chat.ID
			s.AnchorMessageID = resp.MessageID
			s.WatchEnabled = true
		})
	}
	a.refreshSession(ctx, ts.ID, true)
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
		a.updateAnchorLocal(ctx, id, "tmux send error: "+err.Error(), true)
		return
	}
	a.Store.UpdateSession(id, func(s *state.TerminalSession) {
		s.LastInputPreview = preview(text)
		s.LastInputMode = mode
		s.LastActivityAt = time.Now().UTC()
		s.PendingRefresh = true
	})
}

func (a *App) sendKeys(ctx context.Context, id int, keys []string) {
	if err := tmux.ValidKeys(keys); err != nil {
		a.updateAnchorLocal(ctx, id, err.Error(), true)
		return
	}
	ts, ok := a.Store.FindSession(id)
	if !ok {
		return
	}
	tctx, cancel := tmux.TimeoutContext(ctx)
	defer cancel()
	if err := a.Tmux.SendKeys(tctx, ts.TmuxPaneID, keys); err != nil {
		a.updateAnchorLocal(ctx, id, "tmux key error: "+err.Error(), true)
		return
	}
	a.Store.UpdateSession(id, func(s *state.TerminalSession) {
		s.LastInputPreview = strings.Join(keys, " ")
		s.LastInputMode = "keys"
		s.LastActivityAt = time.Now().UTC()
		s.PendingRefresh = true
	})
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
		msg, err := a.Telegram.SendMessage(ctx, a.Config.TelegramChatID, renderLocal(ts, firstNonEmpty(ts.LastSummary, "watching")), replyTo, telegram.RefreshMarkup(id))
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
	a.refreshSession(ctx, id, true)
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
	b.WriteString("Active sessions\n\n")
	for _, ts := range st.TerminalSessions {
		ids = append(ids, ts.ID)
		fmt.Fprintf(&b, "[%d] %s  %s  last: %s\n", ts.ID, ts.State, firstNonEmpty(ts.Title, "-"), firstNonEmpty(ts.LastInputPreview, "-"))
	}
	if len(ids) == 0 {
		b.WriteString("No sessions.")
	}
	_, _ = a.Telegram.SendMessage(ctx, msg.Chat.ID, b.String(), msg.MessageID, telegram.SessionListMarkup(ids))
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
	summary, err := a.Anthropic.Summarize(ctx, anthropic.SummaryInput{
		SessionID:       id,
		State:           string(ts.State),
		LastInput:       ts.LastInputPreview,
		LastInputMode:   ts.LastInputMode,
		PreviousSummary: ts.LastSummary,
		VisibleCapture:  capture,
	})
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
	_, err := a.Telegram.EditMessage(ctx, ts.AnchorChatID, ts.AnchorMessageID, rendered, telegram.RefreshMarkup(id))
	if err != nil {
		msg, sendErr := a.Telegram.SendMessage(ctx, a.Config.TelegramChatID, rendered, 0, telegram.RefreshMarkup(id))
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
					a.refreshSession(ctx, ts.ID, false)
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
	if doc.FileSize > a.Config.AttachmentSoftMaxBytes && !allowLarge {
		space := diskFree(a.Config.ArtifactDir())
		a.reply(ctx, msg, fmt.Sprintf("attachment too large: %d bytes exceeds soft limit %d bytes\navailable /tmp/engram space: %d bytes\nresend with caption `/attachment-bypass sha256:<hash>` to bypass", doc.FileSize, a.Config.AttachmentSoftMaxBytes, space))
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
	if !filepath.IsAbs(path) {
		a.reply(ctx, msg, "/download requires an absolute path")
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		a.reply(ctx, msg, "stat error: "+err.Error())
		return
	}
	if !info.Mode().IsRegular() {
		a.reply(ctx, msg, "not a regular file")
		return
	}
	if _, err := a.Telegram.SendDocument(ctx, a.Config.TelegramChatID, path, filepath.Base(path)); err != nil {
		a.reply(ctx, msg, "upload error: "+err.Error())
	}
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
	_, _ = a.Telegram.SendMessage(ctx, msg.Chat.ID, text, msg.MessageID, nil)
}

func (a *App) statusText() string {
	st := a.Store.Snapshot()
	space := diskFree(a.Config.ArtifactDir())
	return fmt.Sprintf("Engram status\nversion: %s\nuptime: %s\nsessions: %d\nstate: %s\naudit: %s\nattachments: %s\n/tmp free: %d\nlast poll: %s\nlast haiku: %s\nlast haiku error: %s",
		version.String(),
		time.Since(a.startedAt).Round(time.Second),
		len(st.TerminalSessions),
		a.Config.StatePath(),
		a.Config.AuditPath(),
		a.Config.AttachmentDir(),
		space,
		st.LastPollAt.Format(time.RFC3339),
		st.LastHaikuAt.Format(time.RFC3339),
		firstNonEmpty(st.LastHaikuError, "-"),
	)
}

func renderLocal(ts state.TerminalSession, summary string) string {
	title := firstNonEmpty(ts.Title, "-")
	if len(title) > 40 {
		title = title[:40]
	}
	return fmt.Sprintf("[%d] %s  %s\nlast input: %s\nupdated: %s\n\n%s",
		ts.ID,
		ts.State,
		title,
		firstNonEmpty(ts.LastInputPreview, "-"),
		time.Now().UTC().Format("15:04:05 UTC"),
		summary,
	)
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
