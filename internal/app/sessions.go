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

func (a *App) newSession(ctx context.Context, msg telegram.Message, input string) actionResult {
	tmuxCtx, cancel := tmux.TimeoutContext(ctx)
	defer cancel()
	sessionName, err := a.targetTmuxSession(tmuxCtx, msg.Chat.ID)
	if err != nil {
		a.reply(ctx, msg, "tmux error: "+err.Error())
		return actionResult{Outcome: actionTmuxFailed, Message: "tmux session unavailable"}
	}
	title := tmux.WindowTitle(0, input)
	sessionID, windowID, paneID, err := a.Tmux.NewWindow(tmuxCtx, sessionName, a.Config.Workdir, title)
	if err != nil {
		a.reply(ctx, msg, "tmux error: "+err.Error())
		return actionResult{Outcome: actionTmuxFailed, Message: "tmux window creation failed"}
	}
	ts, err := a.Store.AllocateSession(msg.Chat.ID, msg.From.ID, sessionName, windowID, paneID, title)
	if err != nil {
		_ = a.Tmux.KillWindow(tmuxCtx, windowID)
		a.reply(ctx, msg, "state error: "+err.Error())
		return actionResult{Outcome: actionStateFailed, Message: "session state allocation failed"}
	}
	ts.TmuxSessionID = sessionID
	updated, _, err := a.Store.UpdateSession(ts.ID, func(s *state.TerminalSession) {
		s.TmuxSessionID = sessionID
		s.Origin = state.TerminalOriginCreated
		s.LastInputPreview = preview(input)
		s.LastInputMode = "command"
	})
	if err != nil {
		a.reply(ctx, msg, "state error: "+err.Error())
		return actionResult{Outcome: actionStateFailed, Message: "session state update failed"}
	}
	ts = updated
	if err := a.Tmux.SendCommand(tmuxCtx, paneID, input); err != nil {
		a.reply(ctx, msg, "tmux send error: "+err.Error())
		return actionResult{Outcome: actionTmuxFailed, Message: "initial tmux input failed"}
	}
	resp, err := a.sendAnchor(ctx, msg.Chat.ID, renderLocal(ts, "starting; waiting for terminal output"), msg.MessageID, telegram.RefreshMarkup(ts.ID))
	anchorReady := false
	if err == nil {
		if _, _, err := a.Store.UpdateSession(ts.ID, func(s *state.TerminalSession) {
			s.AnchorChatID = resp.Chat.ID
			s.AnchorMessageID = resp.MessageID
			s.WatchEnabled = true
		}); err != nil {
			_ = a.audit("state.session", "failed", map[string]any{"session_id": ts.ID, "error": err.Error()})
			return actionResult{Outcome: actionStateFailed, Message: "session anchor state update failed"}
		} else {
			anchorReady = true
		}
	} else {
		_ = a.audit("telegram.send", "failed", map[string]any{"command": "new", "error": err.Error()})
		return actionResult{Outcome: actionTelegramFailed, Message: "could not send session anchor"}
	}
	if anchorReady {
		a.queueRefresh(ts.ID, true, summaryQuietPeriod)
	}
	return actionResult{Outcome: actionOK, Message: fmt.Sprintf("created [%d]", ts.ID)}
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
		s.Origin = state.TerminalOriginAttached
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
	anchorReady := false
	if err == nil {
		if _, _, err := a.Store.UpdateSession(ts.ID, func(s *state.TerminalSession) {
			s.AnchorChatID = resp.Chat.ID
			s.AnchorMessageID = resp.MessageID
			s.WatchEnabled = true
		}); err != nil {
			_ = a.audit("state.session", "failed", map[string]any{"session_id": ts.ID, "error": err.Error()})
		} else {
			anchorReady = true
		}
	} else {
		_ = a.audit("telegram.send", "failed", map[string]any{"command": "attach", "error": err.Error()})
		return actionResult{Outcome: actionTelegramFailed, Message: "could not send session anchor"}
	}
	if anchorReady {
		a.queueRefresh(ts.ID, true, summaryQuietPeriod)
	}
	return actionResult{Outcome: actionOK, Message: fmt.Sprintf("attached [%d]", ts.ID)}
}

func (a *App) rename(ctx context.Context, id int, name string, msg telegram.Message) actionResult {
	_, ok, err := a.Store.UpdateSession(id, func(s *state.TerminalSession) { s.Title = strings.TrimSpace(name) })
	if err != nil {
		a.reply(ctx, msg, "state error: "+err.Error())
		return actionResult{Outcome: actionStateFailed, Message: "state update failed"}
	}
	if !ok {
		a.reply(ctx, msg, "session not found")
		return actionResult{Outcome: actionUserError, Message: "session not found"}
	}
	a.reply(ctx, msg, fmt.Sprintf("[%d] renamed to %s", id, strings.TrimSpace(name)))
	return actionResult{Outcome: actionOK, Message: "renamed"}
}

func (a *App) cwd(ctx context.Context, id int, msg telegram.Message) actionResult {
	ts, ok := a.Store.FindSession(id)
	if !ok {
		a.reply(ctx, msg, "session not found")
		return actionResult{Outcome: actionUserError, Message: "session not found"}
	}
	tctx, cancel := tmux.TimeoutContext(ctx)
	defer cancel()
	if err := a.validateSessionPane(tctx, ts); err != nil {
		a.reply(ctx, msg, "session lost; use /sessions to attach the intended pane again")
		return actionResult{Outcome: actionTmuxFailed, Message: "session identity lost"}
	}
	cwd, err := a.Tmux.PaneCWD(tctx, ts.TmuxPaneID)
	if err != nil {
		a.reply(ctx, msg, "cwd error: "+err.Error())
		return actionResult{Outcome: actionTmuxFailed, Message: "cwd lookup failed"}
	}
	if _, _, err := a.Store.UpdateSession(id, func(s *state.TerminalSession) { s.LastKnownCWD = cwd }); err != nil {
		a.reply(ctx, msg, "state error: "+err.Error())
		return actionResult{Outcome: actionStateFailed, Message: "cwd state update failed"}
	}
	a.reply(ctx, msg, fmt.Sprintf("[%d] cwd\n%s", id, cwd))
	return actionResult{Outcome: actionOK, Message: cwd}
}

func (a *App) cd(ctx context.Context, id int, path string) actionResult {
	cmd := "cd " + tmux.ShellQuote(config.ExpandPath(strings.TrimSpace(path)))
	return a.sendInput(ctx, id, cmd, "command", true)
}

func (a *App) watchSession(ctx context.Context, id int, replyTo int) actionResult {
	lock := a.sessionMutex(id)
	lock.Lock()
	defer lock.Unlock()
	ts, ok := a.Store.FindSession(id)
	if !ok {
		return actionResult{Outcome: actionUserError, Message: "session not found"}
	}
	if ts.State == state.TerminalClosed {
		return actionResult{Outcome: actionUserError, Message: "session is " + string(ts.State) + "; use /sessions to attach an active pane"}
	}
	if ts.State == state.TerminalLost {
		tctx, cancel := tmux.TimeoutContext(ctx)
		defer cancel()
		if err := a.validateSessionPane(tctx, ts); err != nil {
			return actionResult{Outcome: actionTmuxFailed, Message: "session is still lost; use /sessions to locate the intended pane"}
		}
		ts, _ = a.Store.FindSession(id)
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
	lock := a.sessionMutex(id)
	lock.Lock()
	defer lock.Unlock()
	ts, ok := a.Store.FindSession(id)
	if !ok {
		return actionResult{Outcome: actionUserError, Message: "session not found"}
	}
	if ts.Origin != state.TerminalOriginCreated {
		if _, _, err := a.Store.UpdateSession(id, func(s *state.TerminalSession) {
			s.State = state.TerminalClosed
			s.WatchEnabled = false
			s.LastSummary = "status:\nThis session is no longer tracked. Its tmux window remains open.\n\nrecommendation:\nUse /sessions to attach it again when needed."
		}); err != nil {
			_ = a.audit("state.session", "failed", map[string]any{"session_id": id, "error": err.Error()})
			return actionResult{Outcome: actionStateFailed, Message: "state update failed while untracking"}
		}
		a.updateAnchorLocal(ctx, id, "status:\nThis session is no longer tracked. Its tmux window remains open.\n\nrecommendation:\nUse /sessions to attach it again when needed.", true)
		_ = a.audit("tmux.untrack", "ok", map[string]any{"session_id": id, "pane_id": ts.TmuxPaneID, "origin": ts.Origin})
		return actionResult{Outcome: actionOK, Message: "untracked; tmux remains open"}
	}
	tctx, cancel := tmux.TimeoutContext(ctx)
	defer cancel()
	if _, err := a.Tmux.ValidatePane(tctx, ts.TmuxPaneID, ts.TmuxWindowID); err != nil {
		a.markSessionLost(ctx, ts, err)
		return actionResult{Outcome: actionTmuxFailed, Message: "close failed: session identity is no longer valid"}
	}
	if err := a.Tmux.KillWindow(tctx, ts.TmuxWindowID); err != nil {
		_ = a.audit("tmux.close", "failed", map[string]any{"session_id": id, "window_id": ts.TmuxWindowID, "error": err.Error()})
		a.updateAnchorLocal(ctx, id, "status:\nClose failed; the tmux window remains open.\n\nrecommendation:\nCheck the session with /sessions and retry when tmux is available.", true)
		return actionResult{Outcome: actionTmuxFailed, Message: "close failed: " + err.Error()}
	}
	if _, _, err := a.Store.UpdateSession(id, func(s *state.TerminalSession) {
		s.State = state.TerminalClosed
		s.WatchEnabled = false
		s.LastSummary = "status:\nThe Engram-created tmux window was closed."
	}); err != nil {
		_ = a.audit("state.session", "failed", map[string]any{"session_id": id, "error": err.Error()})
		return actionResult{Outcome: actionStateFailed, Message: "state update failed after close"}
	}
	a.updateAnchorLocal(ctx, id, "status:\nThe Engram-created tmux window was closed.", true)
	return actionResult{Outcome: actionOK, Message: "closed"}
}

func closeConfirmationText(ts state.TerminalSession) string {
	if ts.Origin == state.TerminalOriginCreated {
		return fmt.Sprintf("Close [%d]? This will kill its Engram-created tmux window.", ts.ID)
	}
	return fmt.Sprintf("Untrack [%d]? Its existing tmux window will remain open.", ts.ID)
}

func (a *App) markSessionLost(ctx context.Context, ts state.TerminalSession, cause error) {
	message := "session identity is no longer valid"
	if cause != nil {
		message = cause.Error()
	}
	if _, _, err := a.Store.UpdateSession(ts.ID, func(s *state.TerminalSession) {
		s.State = state.TerminalLost
		s.WatchEnabled = false
		s.PendingRefresh = false
		s.LastTelegramError = message
	}); err != nil {
		_ = a.audit("state.session", "failed", map[string]any{"session_id": ts.ID, "error": err.Error()})
		return
	}
	_ = a.audit("tmux.identity", "lost", map[string]any{"session_id": ts.ID, "pane_id": ts.TmuxPaneID, "window_id": ts.TmuxWindowID, "error": message})
	a.updateAnchorLocal(ctx, ts.ID, "status:\nThe tracked tmux pane no longer matches this session. Engram stopped watching it.\n\nrecommendation:\nUse /sessions and attach the intended pane again.", true)
}

func (a *App) validateSessionPane(ctx context.Context, ts state.TerminalSession) error {
	_, err := a.Tmux.ValidatePane(ctx, ts.TmuxPaneID, ts.TmuxWindowID)
	if err != nil {
		if tmux.IsIdentityLoss(err) {
			a.markSessionLost(ctx, ts, err)
		} else {
			_ = a.audit("tmux.identity", "unavailable", map[string]any{"session_id": ts.ID, "pane_id": ts.TmuxPaneID, "window_id": ts.TmuxWindowID, "error": err.Error()})
		}
		return err
	}
	if ts.State == state.TerminalLost {
		if _, _, stateErr := a.Store.UpdateSession(ts.ID, func(s *state.TerminalSession) {
			s.State = state.TerminalRunning
			s.WatchEnabled = true
			s.PendingRefresh = true
			s.LastTelegramError = ""
		}); stateErr != nil {
			_ = a.audit("state.session", "failed", map[string]any{"session_id": ts.ID, "error": stateErr.Error()})
			return fmt.Errorf("recover session state: %w", stateErr)
		}
		_ = a.audit("tmux.identity", "recovered", map[string]any{"session_id": ts.ID, "pane_id": ts.TmuxPaneID, "window_id": ts.TmuxWindowID})
	}
	return nil
}

func (a *App) sessions(ctx context.Context, msg telegram.Message) {
	st := a.Store.Snapshot()
	var ids []int
	var b strings.Builder
	b.WriteString("sessions\n")
	for _, ts := range st.TerminalSessions {
		if ts.State == state.TerminalClosed {
			continue
		}
		ids = append(ids, ts.ID)
		fmt.Fprintf(&b, "\n[%d] %s  %s\n  last: %s\n", ts.ID, ts.State, firstNonEmpty(ts.Title, "-"), firstNonEmpty(ts.LastInputPreview, "-"))
	}
	if len(ids) == 0 {
		b.WriteString("\nNo tracked sessions.\n")
	}
	b.WriteString("\ntmux\n")
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
		fmt.Fprintf(b, "\n%s %s  %s windows  %s clients", marker, session.Name, firstNonEmpty(session.Windows, "?"), firstNonEmpty(session.Attached, "?"))
	}
	panes, err := a.Tmux.ListPanes(tctx)
	if err != nil {
		fmt.Fprintf(b, "\n\nPanes unavailable: %s", err)
		return nil
	}
	if len(panes) == 0 {
		return nil
	}
	var attachTargets []telegram.AttachTarget
	b.WriteString("\n\navailable panes\n")
	for _, pane := range panes {
		target := fmt.Sprintf("%s:%s.%s", pane.SessionName, pane.WindowIndex, pane.Index)
		tracked := ""
		if ts, ok := a.Store.FindByPane(pane.ID); ok {
			tracked = fmt.Sprintf(" tracked:[%d]", ts.ID)
		}
		active := ""
		if pane.Active {
			active = " active"
		}
		fmt.Fprintf(b, "%s  %s%s%s\n", target, firstNonEmpty(pane.CurrentCmd, "-"), active, tracked)
		if tracked == "" {
			attachTargets = append(attachTargets, telegram.AttachTarget{Label: target, Target: pane.ID})
		}
	}
	b.WriteString("\nUse /attach <pane>, for example /attach " + panes[0].ID)
	return attachTargets
}
