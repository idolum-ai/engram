package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
	"github.com/idolum-ai/engram/internal/tmux"
)

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
	updated, _, err := a.Store.UpdateSession(ts.ID, func(s *state.TerminalSession) {
		s.TmuxSessionID = sessionID
		s.LastInputPreview = preview(input)
		s.LastInputMode = "command"
	})
	if err != nil {
		a.reply(ctx, msg, "state error: "+err.Error())
		return
	}
	ts = updated
	if err := a.Tmux.SendCommand(tmuxCtx, paneID, input); err != nil {
		a.reply(ctx, msg, "tmux send error: "+err.Error())
		return
	}
	resp, err := a.sendAnchor(ctx, msg.Chat.ID, renderLocal(ts, "starting; waiting for terminal output"), msg.MessageID, telegram.RefreshMarkup(ts.ID))
	if err == nil {
		if _, _, err := a.Store.UpdateSession(ts.ID, func(s *state.TerminalSession) {
			s.AnchorChatID = resp.Chat.ID
			s.AnchorMessageID = resp.MessageID
			s.WatchEnabled = true
		}); err != nil {
			_ = a.audit("state.session", "failed", map[string]any{"session_id": ts.ID, "error": err.Error()})
		}
	} else {
		_ = a.audit("telegram.send", "failed", map[string]any{"command": "new", "error": err.Error()})
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

func (a *App) attachTarget(ctx context.Context, msg telegram.Message, target string) actionResult {
	tmuxCtx, cancel := tmux.TimeoutContext(ctx)
	defer cancel()
	window, err := a.Tmux.ResolveTarget(tmuxCtx, strings.TrimSpace(target))
	if err != nil {
		a.reply(ctx, msg, "tmux error: "+err.Error())
		return actionResult{Outcome: actionTmuxFailed, Message: "tmux target not found"}
	}
	if existing, ok := a.Store.FindByPane(window.PaneID); ok {
		a.reply(ctx, msg, fmt.Sprintf("%s is already tracked as [%d]", window.PaneID, existing.ID))
		return actionResult{Outcome: actionUserError, Message: fmt.Sprintf("already tracked as [%d]", existing.ID)}
	}
	title := tmux.AttachedTitle(window)
	ts, err := a.Store.AllocateSession(msg.Chat.ID, msg.From.ID, window.SessionName, window.ID, window.PaneID, title)
	if err != nil {
		a.reply(ctx, msg, "state error: "+err.Error())
		return actionResult{Outcome: actionStateFailed, Message: "state error"}
	}
	updated, _, err := a.Store.UpdateSession(ts.ID, func(s *state.TerminalSession) {
		s.LastKnownCWD = window.CurrentPath
		s.LastInputPreview = "attached " + strings.TrimSpace(target)
		s.LastInputMode = "attach"
	})
	if err != nil {
		a.reply(ctx, msg, "state error: "+err.Error())
		return actionResult{Outcome: actionStateFailed, Message: "state error"}
	}
	ts = updated
	resp, err := a.sendAnchor(ctx, msg.Chat.ID, renderLocal(ts, "attached existing tmux target; waiting for terminal output"), msg.MessageID, telegram.RefreshMarkup(ts.ID))
	if err == nil {
		if _, _, err := a.Store.UpdateSession(ts.ID, func(s *state.TerminalSession) {
			s.AnchorChatID = resp.Chat.ID
			s.AnchorMessageID = resp.MessageID
			s.WatchEnabled = true
		}); err != nil {
			_ = a.audit("state.session", "failed", map[string]any{"session_id": ts.ID, "error": err.Error()})
		}
	} else {
		_ = a.audit("telegram.send", "failed", map[string]any{"command": "attach", "error": err.Error()})
		return actionResult{Outcome: actionTelegramFailed, Message: "could not send session anchor"}
	}
	a.queueRefresh(ts.ID, true, summaryQuietPeriod)
	return actionResult{Outcome: actionOK, Message: fmt.Sprintf("attached [%d]", ts.ID)}
}

func (a *App) rename(ctx context.Context, id int, name string, msg telegram.Message) {
	_, ok, err := a.Store.UpdateSession(id, func(s *state.TerminalSession) { s.Title = strings.TrimSpace(name) })
	if err != nil {
		a.reply(ctx, msg, "state error: "+err.Error())
		return
	}
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
	if _, _, err := a.Store.UpdateSession(id, func(s *state.TerminalSession) { s.LastKnownCWD = cwd }); err != nil {
		a.reply(ctx, msg, "state error: "+err.Error())
		return
	}
	a.reply(ctx, msg, fmt.Sprintf("[%d] cwd\n%s", id, cwd))
}

func (a *App) cd(ctx context.Context, id int, path string) actionResult {
	cmd := "cd " + tmux.ShellQuote(config.ExpandPath(strings.TrimSpace(path)))
	return a.sendInput(ctx, id, cmd, "command", true)
}

func (a *App) watchSession(ctx context.Context, id int, replyTo int) actionResult {
	ts, ok := a.Store.FindSession(id)
	if !ok {
		return actionResult{Outcome: actionUserError, Message: "session not found"}
	}
	if ts.AnchorMessageID == 0 {
		msg, err := a.sendAnchor(ctx, a.Config.TelegramChatID, renderLocal(ts, firstNonEmpty(ts.LastSummary, "watching")), replyTo, telegram.RefreshMarkup(id))
		if err == nil {
			if _, _, err := a.Store.UpdateSession(id, func(s *state.TerminalSession) {
				s.AnchorChatID = msg.Chat.ID
				s.AnchorMessageID = msg.MessageID
				s.WatchEnabled = true
			}); err != nil {
				_ = a.audit("state.session", "failed", map[string]any{"session_id": id, "error": err.Error()})
				return actionResult{Outcome: actionStateFailed, Message: "state update failed"}
			}
			return actionResult{Outcome: actionOK, Message: "watching"}
		}
		return actionResult{Outcome: actionTelegramFailed, Message: "could not send session anchor"}
	}
	if _, _, err := a.Store.UpdateSession(id, func(s *state.TerminalSession) { s.WatchEnabled = true }); err != nil {
		_ = a.audit("state.session", "failed", map[string]any{"session_id": id, "error": err.Error()})
		return actionResult{Outcome: actionStateFailed, Message: "state update failed"}
	}
	a.queueRefresh(id, true, 0)
	return actionResult{Outcome: actionOK, Message: "watching"}
}

func (a *App) closeSession(ctx context.Context, id int) actionResult {
	ts, ok := a.Store.FindSession(id)
	if !ok {
		return actionResult{Outcome: actionUserError, Message: "session not found"}
	}
	tctx, cancel := tmux.TimeoutContext(ctx)
	defer cancel()
	if err := a.Tmux.KillWindow(tctx, ts.TmuxWindowID); err != nil {
		_ = a.audit("tmux.close", "failed", map[string]any{"session_id": id, "window_id": ts.TmuxWindowID, "error": err.Error()})
		a.updateAnchorLocal(ctx, id, "status:\nClose failed; the tmux window remains open.\n\nrecommendation:\nCheck the session with /sessions and retry when tmux is available.", true)
		return actionResult{Outcome: actionTmuxFailed, Message: "close failed: " + err.Error()}
	}
	if _, _, err := a.Store.UpdateSession(id, func(s *state.TerminalSession) {
		s.State = state.TerminalClosed
		s.WatchEnabled = false
		s.LastSummary = "summary:\n- Session closed by request."
	}); err != nil {
		_ = a.audit("state.session", "failed", map[string]any{"session_id": id, "error": err.Error()})
		return actionResult{Outcome: actionStateFailed, Message: "state update failed after close"}
	}
	a.updateAnchorLocal(ctx, id, "summary:\n- Session closed by request.", true)
	return actionResult{Outcome: actionOK, Message: "closed"}
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
			attachTargets = append(attachTargets, telegram.AttachTarget{Label: target, Target: window.PaneID})
		}
	}
	b.WriteString("\nUse /attach <target>, for example /attach " + windows[0].SessionName + ":" + windows[0].Index)
	return attachTargets
}
