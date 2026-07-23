package app

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/mechanics"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
	"github.com/idolum-ai/engram/internal/tmux"
)

func (a *App) newSession(ctx context.Context, msg telegram.Message, input string) actionResult {
	tmuxCtx, cancel := tmux.TimeoutContext(ctx)
	defer cancel()
	sessionName, sessionID, err := a.targetTmuxSession(tmuxCtx, msg.Chat.ID)
	if err != nil {
		a.reply(ctx, msg, "tmux error: "+err.Error())
		return actionResult{Outcome: actionTmuxFailed, Message: "tmux session unavailable"}
	}
	serverID, err := a.Tmux.EnsureServerID(tmuxCtx)
	if err != nil {
		a.reply(ctx, msg, "tmux identity error: "+err.Error())
		return actionResult{Outcome: actionTmuxFailed, Message: "tmux server identity unavailable"}
	}
	title := tmux.WindowTitle(0, input)
	windowID, paneID, err := a.Tmux.NewWindow(tmuxCtx, sessionID, a.Config.Workdir, title)
	if err != nil {
		a.reply(ctx, msg, "tmux error: "+err.Error())
		return actionResult{Outcome: actionTmuxFailed, Message: "tmux window creation failed"}
	}
	ts, err := a.Store.AllocateSession(sessionName, windowID, paneID, title)
	if err != nil && !state.PersistenceReachedReplacement(err) {
		_, _ = a.terminalMechanics().CloseWindow(tmuxCtx, mechanics.Binding{PaneID: paneID, WindowID: windowID, ServerID: serverID})
		a.reply(ctx, msg, "state error: "+err.Error())
		return actionResult{Outcome: actionStateFailed, Message: "session state allocation failed"}
	}
	if err != nil {
		_ = a.audit("state.session", "durability_uncertain", map[string]any{"session_id": ts.ID, "operation": "allocate", "error": err.Error()})
	}
	updated, _, err := a.Store.UpdateSession(ts.ID, func(s *state.TerminalSession) {
		s.Origin = state.TerminalOriginCreated
		s.TmuxServerID = serverID
	})
	if err != nil {
		a.reply(ctx, msg, "state error: "+err.Error())
		return actionResult{Outcome: actionStateFailed, Message: "session state update failed"}
	}
	ts = updated
	pane, err := a.terminalMechanics().SendCommand(tmuxCtx, terminalBinding(ts), input)
	if err != nil {
		a.recordIdentityLoss(ctx, ts, err)
		a.reply(ctx, msg, "tmux send error: "+err.Error())
		return actionResult{Outcome: actionTmuxFailed, Message: "initial tmux input failed"}
	}
	if err := a.recordValidatedPane(ts, pane); err != nil {
		a.reply(ctx, msg, "state error: "+err.Error())
		return actionResult{Outcome: actionStateFailed, Message: "initial session state update failed"}
	}
	if current, ok := a.Store.FindSession(ts.ID); ok {
		a.recordSentRecoveryCommand(current, pane, input)
	}
	resp, err := a.sendAnchor(ctx, msg.Chat.ID, a.renderLocal(ts, "starting; waiting for terminal output"), msg.MessageID, a.anchorMarkup(ts))
	anchorReady := false
	if err == nil {
		if _, _, err := a.Store.UpdateSession(ts.ID, func(s *state.TerminalSession) {
			s.AnchorChatID = resp.Chat.ID
			s.AnchorMessageID = resp.MessageID
			s.AnchorFormat = "text"
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
		if current, ok := a.Store.FindSession(ts.ID); ok {
			a.advertiseTerminalCapabilities(ctx, current)
		}
		a.reconcileAnchorPresentation(ctx, ts.ID)
		a.queueRefresh(ts.ID, true, summaryQuietPeriod)
	}
	return actionResult{Outcome: actionOK, Message: fmt.Sprintf("created [%d]", ts.ID)}
}

func (a *App) targetTmuxSession(ctx context.Context, chatID int64) (string, string, error) {
	if strings.TrimSpace(a.Config.TmuxSession) != "" {
		name := strings.TrimSpace(a.Config.TmuxSession)
		id, err := a.Tmux.EnsureSession(ctx, name, a.Config.Workdir)
		return name, id, err
	}
	sessions, err := a.Tmux.ListSessions(ctx)
	if err == nil && len(sessions) > 0 {
		return sessions[0].Name, sessions[0].ID, nil
	}
	name := tmux.SessionName(chatID)
	id, err := a.Tmux.EnsureSession(ctx, name, a.Config.Workdir)
	return name, id, err
}

func (a *App) attachTarget(ctx context.Context, msg telegram.Message, target string) actionResult {
	tmuxCtx, cancel := tmux.TimeoutContext(ctx)
	defer cancel()
	serverID, err := a.Tmux.EnsureServerID(tmuxCtx)
	if err != nil {
		a.reply(ctx, msg, "tmux identity error: "+err.Error())
		return actionResult{Outcome: actionTmuxFailed, Message: "tmux server identity unavailable"}
	}
	window, err := a.Tmux.ResolveTarget(tmuxCtx, strings.TrimSpace(target))
	if err != nil {
		a.reply(ctx, msg, "tmux error: "+err.Error())
		return actionResult{Outcome: actionTmuxFailed, Message: "tmux target not found"}
	}
	currentServerID, err := a.Tmux.CurrentServerID(tmuxCtx)
	if err != nil || currentServerID != serverID {
		a.reply(ctx, msg, "tmux changed while attaching; run /sessions and try again")
		return actionResult{Outcome: actionTmuxFailed, Message: "tmux changed while attaching"}
	}
	if existing, ok := a.Store.FindByBinding(window.PaneID, window.ID, serverID); ok {
		a.reply(ctx, msg, fmt.Sprintf("%s is already tracked as [%d]", window.PaneID, existing.ID))
		return actionResult{Outcome: actionUserError, Message: fmt.Sprintf("already tracked as [%d]", existing.ID)}
	}
	if existing, ok := a.Store.FindByPane(window.PaneID); ok {
		disclosureLock := a.disclosureMutex(existing.ID)
		disclosureLock.Lock()
		defer disclosureLock.Unlock()
		lock := a.sessionMutex(existing.ID)
		lock.Lock()
		anchorLock := a.anchorMutex(existing.ID)
		anchorLock.Lock()
		freshWindow, revalidateErr := a.revalidateAttachTarget(ctx, target, serverID, window)
		if revalidateErr != nil {
			anchorLock.Unlock()
			lock.Unlock()
			a.reply(ctx, msg, "tmux changed while attaching; run /sessions and try again")
			return actionResult{Outcome: actionTmuxFailed, Message: "tmux changed while attaching"}
		}
		window = freshWindow
		lockedCurrent, current := a.Store.FindSession(existing.ID)
		if !current || !sameTerminalBinding(lockedCurrent, existing) || lockedCurrent.AnchorMessageID != existing.AnchorMessageID {
			anchorLock.Unlock()
			lock.Unlock()
			a.reply(ctx, msg, "state changed while attaching; run /sessions and try again")
			return actionResult{Outcome: actionStateFailed, Message: "session changed while attaching"}
		}
		existing = lockedCurrent
		if err := a.neutralizeAnchorForReattachLocked(ctx, existing); err != nil {
			anchorLock.Unlock()
			lock.Unlock()
			a.reply(ctx, msg, "could not make the old anchor inactive; try again")
			return actionResult{Outcome: actionTelegramFailed, Message: "could not retire old session view"}
		}
		updated, found, applied, updateErr := a.updateSessionIfCurrent(existing, func(s *state.TerminalSession) {
			s.TmuxSessionName = window.SessionName
			s.TmuxWindowID = window.ID
			s.TmuxPaneID = window.PaneID
			s.TmuxServerID = serverID
			s.Origin = state.TerminalOriginAttached
			s.State = state.TerminalRunning
			s.WatchEnabled = true
			s.LastKnownCWD = window.CurrentPath
			s.LastRawCaptureHash = ""
			s.LastRawCapture = ""
			s.LastSnapshotCaptureHash = ""
			s.LastSnapshotAttemptAt = time.Time{}
			s.LastRenderHash = ""
			s.LastSummary = ""
			s.SeenUpstreamSignalIDs = nil
			s.LastUpstreamSignalAt = time.Time{}
			s.UpstreamRetryAt = time.Time{}
			retireAlternateReplyTargets(s)
			setAnchorFiles(s, nil)
		})
		committed := found && applied && (updateErr == nil || state.PersistenceReachedReplacement(updateErr))
		if updateErr != nil && committed {
			_ = a.audit("state.session", "durability_uncertain", map[string]any{"session_id": existing.ID, "operation": "reattach", "error": updateErr.Error()})
		}
		if !committed {
			a.restoreAnchorAfterFailedReattachLocked(ctx, existing)
			anchorLock.Unlock()
			lock.Unlock()
			a.reply(ctx, msg, "state changed while attaching; run /sessions and try again")
			return actionResult{Outcome: actionStateFailed, Message: "session changed while attaching"}
		}
		a.signalRetries.Delete(updated.ID)
		a.resetConversationEpochLocked(updated.ID)
		anchorLock.Unlock()
		lock.Unlock()
		a.reply(ctx, msg, fmt.Sprintf("reattached %s as [%d]", window.PaneID, updated.ID))
		a.advertiseTerminalCapabilities(ctx, updated)
		a.reconcileAnchorPresentation(ctx, updated.ID)
		a.queueRefresh(updated.ID, true, 0)
		return actionResult{Outcome: actionOK, Message: fmt.Sprintf("reattached [%d]", updated.ID)}
	}
	title := tmux.AttachedTitle(window)
	ts, err := a.Store.AllocateSession(window.SessionName, window.ID, window.PaneID, title)
	if err != nil && !state.PersistenceReachedReplacement(err) {
		a.reply(ctx, msg, "state error: "+err.Error())
		return actionResult{Outcome: actionStateFailed, Message: "state error"}
	}
	if err != nil {
		_ = a.audit("state.session", "durability_uncertain", map[string]any{"session_id": ts.ID, "operation": "allocate_attached", "error": err.Error()})
	}
	updated, _, err := a.Store.UpdateSession(ts.ID, func(s *state.TerminalSession) {
		s.Origin = state.TerminalOriginAttached
		s.TmuxServerID = serverID
		s.LastKnownCWD = window.CurrentPath
	})
	if err != nil {
		a.reply(ctx, msg, "state error: "+err.Error())
		return actionResult{Outcome: actionStateFailed, Message: "state error"}
	}
	ts = updated
	resp, err := a.sendAnchor(ctx, msg.Chat.ID, a.renderLocal(ts, "attached existing tmux target; waiting for terminal output"), msg.MessageID, a.anchorMarkup(ts))
	anchorReady := false
	if err == nil {
		if _, _, err := a.Store.UpdateSession(ts.ID, func(s *state.TerminalSession) {
			s.AnchorChatID = resp.Chat.ID
			s.AnchorMessageID = resp.MessageID
			s.AnchorFormat = "text"
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
		if current, ok := a.Store.FindSession(ts.ID); ok {
			a.advertiseTerminalCapabilities(ctx, current)
		}
		a.reconcileAnchorPresentation(ctx, ts.ID)
		a.queueRefresh(ts.ID, true, summaryQuietPeriod)
	}
	return actionResult{Outcome: actionOK, Message: fmt.Sprintf("attached [%d]", ts.ID)}
}

func (a *App) revalidateAttachTarget(ctx context.Context, target, serverID string, expected tmux.Window) (tmux.Window, error) {
	tmuxCtx, cancel := tmux.TimeoutContext(ctx)
	defer cancel()
	before, err := a.Tmux.CurrentServerID(tmuxCtx)
	if err != nil || before != serverID {
		return tmux.Window{}, fmt.Errorf("tmux server changed")
	}
	window, err := a.Tmux.ResolveTarget(tmuxCtx, strings.TrimSpace(target))
	if err != nil {
		return tmux.Window{}, err
	}
	after, err := a.Tmux.CurrentServerID(tmuxCtx)
	if err != nil || after != before || window.ID != expected.ID || window.PaneID != expected.PaneID {
		return tmux.Window{}, fmt.Errorf("tmux target changed")
	}
	return window, nil
}

func (a *App) neutralizeAnchorForReattachLocked(ctx context.Context, session state.TerminalSession) error {
	if session.AnchorMessageID == 0 || a.Telegram == nil {
		return nil
	}
	text := fmt.Sprintf("[%d] %s\nreattaching to a new tmux binding", session.ID, a.redactText(firstNonEmpty(session.Title, "session")))
	var err error
	if mediaAnchorFormat(session.AnchorFormat) {
		_, err = a.Telegram.EditCaption(ctx, session.AnchorChatID, session.AnchorMessageID, text, telegram.ClearMarkup())
	} else {
		_, err = a.editAnchor(ctx, session.AnchorChatID, session.AnchorMessageID, text, telegram.ClearMarkup())
	}
	if telegram.IsMessageNotModified(err) {
		return nil
	}
	return err
}

func (a *App) restoreAnchorAfterFailedReattachLocked(ctx context.Context, session state.TerminalSession) {
	if session.AnchorMessageID == 0 || a.Telegram == nil {
		return
	}
	text := a.renderLocal(session, firstNonEmpty(session.LastSummary, "waiting for terminal output"))
	markup := a.anchorMarkup(session)
	var err error
	if mediaAnchorFormat(session.AnchorFormat) {
		_, err = a.Telegram.EditCaption(ctx, session.AnchorChatID, session.AnchorMessageID, text, markup)
	} else {
		_, err = a.editAnchor(ctx, session.AnchorChatID, session.AnchorMessageID, text, markup)
	}
	if err != nil && !telegram.IsMessageNotModified(err) {
		_ = a.audit("telegram.anchor_reattach", "restore_failed", map[string]any{"session_id": session.ID, "error": err.Error()})
	}
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
	a.queueManualRefresh(id)
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
	ts, ok = a.Store.FindSession(id)
	if !ok {
		a.reply(ctx, msg, "session no longer tracked")
		return actionResult{Outcome: actionUserError, Message: "session no longer tracked"}
	}
	cwd := firstNonEmpty(ts.LastKnownCWD, "unknown")
	a.reply(ctx, msg, fmt.Sprintf("[%d] cwd (last observed)\n%s", id, cwd))
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
	if ts.PendingResume != nil {
		return actionResult{Outcome: actionUserError, Message: "resume recovery is still being reconciled; try again shortly"}
	}
	if ts.Collapsed {
		return actionResult{Outcome: actionUserError, Message: "session is on the collapsed shelf; tap ➕ Show on the Collapsed sessions shelf"}
	}
	if ts.State == state.TerminalLost {
		tctx, cancel := tmux.TimeoutContext(ctx)
		defer cancel()
		if err := a.validateSessionPane(tctx, ts); err != nil {
			return actionResult{Outcome: actionTmuxFailed, Message: "session is still lost; use /sessions to locate the intended pane"}
		}
		ts, ok = a.Store.FindSession(id)
		if !ok {
			return actionResult{Outcome: actionUserError, Message: "session no longer tracked"}
		}
	}
	if ts.AnchorMessageID == 0 {
		msg, err := a.sendAnchor(ctx, a.Config.TelegramChatID, a.renderLocal(ts, firstNonEmpty(ts.LastSummary, "watching")), replyTo, a.anchorMarkup(ts))
		if err == nil {
			if _, _, err := a.Store.UpdateSession(id, func(s *state.TerminalSession) {
				s.AnchorChatID = msg.Chat.ID
				s.AnchorMessageID = msg.MessageID
				s.AnchorFormat = "text"
				s.WatchEnabled = true
			}); err != nil {
				_ = a.audit("state.session", "failed", map[string]any{"session_id": id, "error": err.Error()})
				return actionResult{Outcome: actionStateFailed, Message: "state update failed"}
			}
			if current, ok := a.Store.FindSession(id); ok {
				a.advertiseTerminalCapabilities(ctx, current)
			}
			a.reconcileAnchorPresentation(ctx, id)
			return actionResult{Outcome: actionOK, Message: "watching"}
		}
		return actionResult{Outcome: actionTelegramFailed, Message: "could not send session anchor"}
	}
	if _, _, err := a.Store.UpdateSession(id, func(s *state.TerminalSession) { s.WatchEnabled = true }); err != nil {
		_ = a.audit("state.session", "failed", map[string]any{"session_id": id, "error": err.Error()})
		return actionResult{Outcome: actionStateFailed, Message: "state update failed"}
	}
	if current, ok := a.Store.FindSession(id); ok {
		a.advertiseTerminalCapabilities(ctx, current)
	}
	a.reconcileAnchorPresentation(ctx, id)
	a.queueRefresh(id, true, 0)
	return actionResult{Outcome: actionOK, Message: "watching"}
}

func (a *App) closeSession(ctx context.Context, id int) actionResult {
	return a.closeSessionExpected(ctx, id, nil)
}

func (a *App) closeSessionExpected(ctx context.Context, id int, confirmation *closeConfirmation) actionResult {
	a.collapsedShelfMu.Lock()
	before, _ := a.Store.FindSession(id)
	reconcileShelf := before.Collapsed || before.PendingRestore != nil
	result := a.closeSessionExpectedLocked(ctx, id, confirmation)
	a.collapsedShelfMu.Unlock()
	if result.OK() && reconcileShelf {
		a.reconcileCollapsedShelf(ctx)
	}
	return result
}

func (a *App) closeSessionExpectedLocked(ctx context.Context, id int, confirmation *closeConfirmation) actionResult {
	disclosureLock := a.disclosureMutex(id)
	disclosureLock.Lock()
	defer disclosureLock.Unlock()
	lock := a.sessionMutex(id)
	lock.Lock()
	ts, ok := a.Store.FindSession(id)
	if !ok {
		lock.Unlock()
		return actionResult{Outcome: actionUserError, Message: "session not found"}
	}
	if confirmation != nil && (confirmation.SessionID != ts.ID || confirmation.TmuxServerID != ts.TmuxServerID || confirmation.TmuxWindowID != ts.TmuxWindowID || confirmation.TmuxPaneID != ts.TmuxPaneID) {
		lock.Unlock()
		return actionResult{Outcome: actionUserError, Message: "confirmation is stale; use the current session anchor"}
	}
	if ts.Origin != state.TerminalOriginCreated {
		anchorLock := a.anchorMutex(id)
		anchorLock.Lock()
		_, found, applied, err := a.updateSessionIfCurrent(ts, func(s *state.TerminalSession) {
			s.State = state.TerminalClosed
			s.WatchEnabled = false
			s.Collapsed = false
			s.PendingCollapse = false
			clearRecoveryMetadata(s)
			s.LastSummary = "status:\nThis session is no longer tracked. Its tmux window remains open.\n\nrecommendation:\nUse /sessions to attach it again when needed."
		})
		committed := found && applied && (err == nil || state.PersistenceReachedReplacement(err))
		if err != nil {
			outcome := "failed"
			if committed {
				outcome = "durability_uncertain"
			}
			_ = a.audit("state.session", outcome, map[string]any{"session_id": id, "operation": "untrack", "error": err.Error()})
		}
		if !committed {
			anchorLock.Unlock()
			lock.Unlock()
			return actionResult{Outcome: actionStateFailed, Message: "session changed while untracking"}
		}
		if ts.PendingRestore != nil {
			a.retireClosedPendingRestoreLocked(ctx, ts)
		}
		a.resetConversationEpochLocked(id)
		anchorLock.Unlock()
		lock.Unlock()
		a.clearTerminalCapabilities(ctx, ts)
		a.updateAnchorLocal(ctx, id, "status:\nThis session is no longer tracked. Its tmux window remains open.\n\nrecommendation:\nUse /sessions to attach it again when needed.", true)
		a.reconcileAnchorPresentation(ctx, id)
		_ = a.audit("tmux.untrack", "ok", map[string]any{"session_id": id, "pane_id": ts.TmuxPaneID, "origin": ts.Origin})
		return actionResult{Outcome: actionOK, Message: "untracked; tmux remains open"}
	}
	tctx, cancel := tmux.TimeoutContext(ctx)
	defer cancel()
	if _, err := a.terminalMechanics().CloseWindow(tctx, terminalBinding(ts)); err != nil {
		lock.Unlock()
		a.recordIdentityLoss(ctx, ts, err)
		if tmux.IsIdentityLoss(err) {
			return actionResult{Outcome: actionTmuxFailed, Message: "close failed: session identity is no longer valid"}
		}
		_ = a.audit("tmux.close", "failed", map[string]any{"session_id": id, "window_id": ts.TmuxWindowID, "error": err.Error()})
		a.updateAnchorLocal(ctx, id, "status:\nClose failed; the tmux window remains open.\n\nrecommendation:\nCheck the session with /sessions and retry when tmux is available.", true)
		return actionResult{Outcome: actionTmuxFailed, Message: "close failed: " + err.Error()}
	}
	anchorLock := a.anchorMutex(id)
	anchorLock.Lock()
	_, found, applied, err := a.updateSessionIfCurrent(ts, func(s *state.TerminalSession) {
		s.State = state.TerminalClosed
		s.WatchEnabled = false
		s.Collapsed = false
		s.PendingCollapse = false
		clearRecoveryMetadata(s)
		s.LastSummary = "status:\nThe Engram-created tmux window was closed."
	})
	committed := found && applied && (err == nil || state.PersistenceReachedReplacement(err))
	if err != nil {
		outcome := "failed"
		if committed {
			outcome = "durability_uncertain"
		}
		_ = a.audit("state.session", outcome, map[string]any{"session_id": id, "operation": "close", "error": err.Error()})
	}
	if !committed {
		anchorLock.Unlock()
		lock.Unlock()
		return actionResult{Outcome: actionStateFailed, Message: "session no longer tracked after close"}
	}
	if ts.PendingRestore != nil {
		a.retireClosedPendingRestoreLocked(ctx, ts)
	}
	a.resetConversationEpochLocked(id)
	anchorLock.Unlock()
	lock.Unlock()
	a.updateAnchorLocal(ctx, id, "status:\nThe Engram-created tmux window was closed.", true)
	a.reconcileAnchorPresentation(ctx, id)
	return actionResult{Outcome: actionOK, Message: "closed"}
}

func clearRecoveryMetadata(session *state.TerminalSession) {
	session.ResumeProgram = ""
	session.ResumeSessionID = ""
	session.PendingResume = nil
	session.RecoveryEvents = nil
}

func closeConfirmationText(ts state.TerminalSession) string {
	if ts.Origin == state.TerminalOriginCreated {
		return fmt.Sprintf("Close [%d]? This will kill its Engram-created tmux window.", ts.ID)
	}
	return fmt.Sprintf("Untrack [%d]? Its existing tmux window will remain open.", ts.ID)
}

func (a *App) markSessionLost(ctx context.Context, ts state.TerminalSession, cause error) error {
	return a.markSessionLostConditionally(ctx, ts, cause, false)
}

func (a *App) markWatchedSessionLost(ctx context.Context, ts state.TerminalSession, cause error) error {
	return a.markSessionLostConditionally(ctx, ts, cause, true)
}

func (a *App) markSessionLostConditionally(ctx context.Context, ts state.TerminalSession, cause error, requireWatching bool) error {
	if ts.State == state.TerminalClosed {
		return nil
	}
	message := "session identity is no longer valid"
	if cause != nil {
		message = cause.Error()
	}
	anchorLock := a.anchorMutex(ts.ID)
	anchorLock.Lock()
	applied := false
	_, found, err := a.Store.UpdateSession(ts.ID, func(s *state.TerminalSession) {
		if !sameTerminalBinding(*s, ts) || s.State != ts.State || requireWatching && !s.WatchEnabled {
			return
		}
		s.State = state.TerminalLost
		s.WatchEnabled = false
		applied = true
	})
	committed := found && applied && (err == nil || state.PersistenceReachedReplacement(err))
	if err != nil {
		outcome := "failed"
		if committed {
			outcome = "durability_uncertain"
		}
		_ = a.audit("state.session", outcome, map[string]any{"session_id": ts.ID, "operation": "mark_lost", "error": err.Error()})
	}
	if !committed {
		anchorLock.Unlock()
		if err != nil {
			return err
		}
		return fmt.Errorf("session changed before it could be marked lost")
	}
	a.resetConversationEpochLocked(ts.ID)
	anchorLock.Unlock()
	_ = a.audit("tmux.identity", "lost", map[string]any{"session_id": ts.ID, "pane_id": ts.TmuxPaneID, "window_id": ts.TmuxWindowID, "error": message})
	a.updateAnchorLocal(ctx, ts.ID, "status:\nThe tracked tmux pane no longer matches this session. Engram stopped watching it.\n\nrecommendation:\nUse /sessions and attach the intended pane again.", true)
	a.reconcileAnchorPresentation(ctx, ts.ID)
	return nil
}

func (a *App) sessions(ctx context.Context, msg telegram.Message) {
	st := a.Store.Snapshot()
	for i := range st.TerminalSessions {
		a.redactSessionPresentation(&st.TerminalSessions[i])
	}
	var b strings.Builder
	b.WriteString("sessions\n")
	actions := writeTrackedSessions(&b, st.TerminalSessions)
	if !hasTrackedSessions(st.TerminalSessions) {
		b.WriteString("\nNo tracked sessions.\n")
	}
	b.WriteString("\ntmux\n")
	attachTargets := a.writeTmuxSessions(ctx, &b)
	if _, err := a.Telegram.SendMessage(ctx, msg.Chat.ID, b.String(), msg.MessageID, telegram.SessionListMarkup(actions, attachTargets)); err != nil {
		_ = a.audit("telegram.send", "failed", map[string]any{"command": "sessions", "error": err.Error()})
	}
}

func hasTrackedSessions(sessions []state.TerminalSession) bool {
	for _, session := range sessions {
		if session.State != state.TerminalClosed {
			return true
		}
	}
	return false
}

func writeTrackedSessions(b *strings.Builder, sessions []state.TerminalSession) []telegram.SessionAction {
	active := make([]state.TerminalSession, 0, len(sessions))
	for _, session := range sessions {
		if session.State != state.TerminalClosed {
			active = append(active, session)
		}
	}
	sort.SliceStable(active, func(i, j int) bool {
		left, right := active[i], active[j]
		leftRank, rightRank := sessionPresentationRank(left), sessionPresentationRank(right)
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		leftTime, rightTime := sessionPresentationTime(left), sessionPresentationTime(right)
		if !leftTime.Equal(rightTime) {
			return leftTime.After(rightTime)
		}
		return left.ID < right.ID
	})
	labels := map[int]string{
		0: "lost",
		1: "collapsed",
		2: "active",
	}
	lastRank := -1
	actions := make([]telegram.SessionAction, 0, len(active))
	for _, session := range active {
		rank := sessionPresentationRank(session)
		if rank != lastRank {
			fmt.Fprintf(b, "\n%s\n", labels[rank])
			lastRank = rank
		}
		fmt.Fprintf(b, "[%d] %s", session.ID, firstNonEmpty(session.Title, "-"))
		b.WriteString("\n")
		actions = append(actions, telegram.SessionAction{
			ID:        session.ID,
			Token:     sessionActionToken(session),
			CloseOnly: session.Collapsed,
		})
	}
	return actions
}

func sessionActionToken(session state.TerminalSession) string {
	return sha(fmt.Sprintf("%d\x00%s\x00%s\x00%s\x00%s\x00%s", session.ID, session.CreatedAt.UTC().Format(time.RFC3339Nano), session.TmuxServerID, session.TmuxWindowID, session.TmuxPaneID, session.ResumeSessionID))[:12]
}

func sessionPresentationRank(session state.TerminalSession) int {
	if session.State == state.TerminalLost {
		return 0
	}
	if session.Collapsed {
		return 1
	}
	return 2
}

func sessionPresentationTime(session state.TerminalSession) time.Time {
	if session.State == state.TerminalLost {
		return session.UpdatedAt
	}
	return session.LastActivityAt
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
	serverID, err := a.Tmux.EnsureServerID(tctx)
	if err != nil {
		fmt.Fprintf(b, "\n\nIdentity unavailable: %s", err)
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
		if ts, ok := a.Store.FindByBinding(pane.ID, pane.WindowID, serverID); ok {
			tracked = fmt.Sprintf(" tracked:[%d]", ts.ID)
		} else if ts, ok := a.Store.FindByPane(pane.ID); ok {
			tracked = fmt.Sprintf(" reattach:[%d]", ts.ID)
			attachTargets = append(attachTargets, telegram.AttachTarget{Label: target, Target: pane.ID})
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
