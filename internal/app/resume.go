package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/idolum-ai/engram/internal/mechanics"
	"github.com/idolum-ai/engram/internal/recovery"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/tmux"
)

const resumeProcessObservationTimeout = 5 * time.Second
const resumeProcessSettlePeriod = 500 * time.Millisecond

var errResumeProcessNotObserved = errors.New("resume process not observed")

func parseResumeRequest(args string) (id int, program, sessionID string, ok bool) {
	fields := strings.Fields(args)
	if len(fields) != 1 && len(fields) != 3 {
		return 0, "", "", false
	}
	id, err := strconv.Atoi(strings.Trim(fields[0], "[]"))
	if err != nil || id <= 0 {
		return 0, "", "", false
	}
	if len(fields) == 1 {
		return id, "", "", true
	}
	program = strings.ToLower(fields[1])
	sessionID = strings.ToLower(fields[2])
	if !validResumeProgram(program) || !validResumeSessionID(sessionID) {
		return 0, "", "", false
	}
	return id, program, sessionID, true
}

func validResumeProgram(program string) bool {
	return recovery.ValidProgram(program)
}

func validResumeSessionID(id string) bool {
	return recovery.ValidSessionID(id)
}

func resumeCommand(program, sessionID string) string {
	return recovery.ResumeCommand(program, sessionID)
}

func (a *App) resumeSession(ctx context.Context, id int, program, sessionID string) actionResult {
	var resumed state.TerminalSession
	result := func() actionResult {
		disclosureLock := a.disclosureMutex(id)
		disclosureLock.Lock()
		defer disclosureLock.Unlock()
		sessionLock := a.sessionMutex(id)
		sessionLock.Lock()
		defer sessionLock.Unlock()
		anchorLock := a.anchorMutex(id)
		anchorLock.Lock()
		defer anchorLock.Unlock()

		current, ok := a.Store.FindSession(id)
		if !ok {
			return actionResult{Outcome: actionUserError, Message: "session not found"}
		}
		if current.State == state.TerminalRunning {
			return actionResult{Outcome: actionUserError, Message: fmt.Sprintf("[%d] is already running", id)}
		}
		if current.State != state.TerminalLost {
			return actionResult{Outcome: actionUserError, Message: fmt.Sprintf("[%d] is %s; only lost sessions can be resumed", id, current.State)}
		}
		if current.PendingResume != nil {
			return actionResult{Outcome: actionUserError, Message: fmt.Sprintf("[%d] has an interrupted resume; restart Engram to reconcile it", id)}
		}
		if program == "" && sessionID == "" {
			program = current.ResumeProgram
			sessionID = current.ResumeSessionID
		}
		program = strings.ToLower(strings.TrimSpace(program))
		sessionID = strings.ToLower(strings.TrimSpace(sessionID))
		if !validResumeProgram(program) || !validResumeSessionID(sessionID) {
			return actionResult{Outcome: actionUserError, Message: "usage: /resume <id> <codex|claude> <session-uuid>"}
		}
		for _, candidate := range a.Store.Snapshot().TerminalSessions {
			if candidate.ID != id && candidate.State == state.TerminalRunning && candidate.ResumeProgram == program && candidate.ResumeSessionID == sessionID {
				return actionResult{Outcome: actionUserError, Message: fmt.Sprintf("that %s session is already running as [%d]", program, candidate.ID)}
			}
		}

		tmuxCtx, cancel := tmux.TimeoutContext(ctx)
		defer cancel()
		sessionName, tmuxSessionID, err := a.targetTmuxSession(tmuxCtx, a.Config.TelegramChatID)
		if err != nil {
			return actionResult{Outcome: actionTmuxFailed, Message: "tmux session unavailable: " + err.Error()}
		}
		serverID, err := a.Tmux.EnsureServerID(tmuxCtx)
		if err != nil {
			return actionResult{Outcome: actionTmuxFailed, Message: "tmux server identity unavailable: " + err.Error()}
		}
		if current.TmuxServerID == serverID {
			panes, listErr := a.Tmux.ListPanes(tmuxCtx)
			if listErr != nil {
				return actionResult{Outcome: actionTmuxFailed, Message: "could not verify whether the original pane still exists: " + listErr.Error()}
			}
			for _, candidate := range panes {
				if candidate.ID == current.TmuxPaneID {
					return actionResult{Outcome: actionUserError, Message: "the original pane still exists; use /sessions to reattach it instead"}
				}
			}
		}
		workdir := a.resumeWorkdir(current)
		title := tmux.WindowTitle(id, firstNonEmpty(current.Title, program))
		windowID, paneID, err := a.Tmux.NewWindow(tmuxCtx, tmuxSessionID, workdir, title)
		if err != nil {
			return actionResult{Outcome: actionTmuxFailed, Message: "tmux window creation failed: " + err.Error()}
		}
		binding := mechanics.Binding{PaneID: paneID, WindowID: windowID, ServerID: serverID}
		now := time.Now().UTC()
		prepared, found, applied, prepareErr := a.updateSessionIfCurrent(current, func(session *state.TerminalSession) {
			session.PendingResume = &state.PendingResume{
				StartedAt:               now,
				PreviousTmuxSessionName: current.TmuxSessionName,
				PreviousTmuxWindowID:    current.TmuxWindowID,
				PreviousTmuxPaneID:      current.TmuxPaneID,
				PreviousTmuxServerID:    current.TmuxServerID,
				PreviousOrigin:          current.Origin,
				PreviousCWD:             current.LastKnownCWD,
				PreviousResumeProgram:   current.ResumeProgram,
				PreviousResumeSessionID: current.ResumeSessionID,
			}
			session.TmuxSessionName = sessionName
			session.TmuxWindowID = windowID
			session.TmuxPaneID = paneID
			session.TmuxServerID = serverID
			session.Origin = state.TerminalOriginCreated
			session.State = state.TerminalLost
			session.WatchEnabled = false
			session.ResumeProgram = program
			session.ResumeSessionID = sessionID
			session.LastKnownCWD = workdir
		})
		preparedCommitted := found && applied && (prepareErr == nil || state.PersistenceReachedReplacement(prepareErr))
		if !preparedCommitted {
			if cleanupErr := a.closeResumeWindow(binding); cleanupErr != nil {
				return actionResult{Outcome: actionStateFailed, Message: "session changed while preparing resume; replacement cleanup also failed: " + cleanupErr.Error()}
			}
			return actionResult{Outcome: actionStateFailed, Message: "session changed while preparing resume"}
		}
		if prepareErr != nil {
			_ = a.audit("state.session", "durability_uncertain", map[string]any{"session_id": id, "operation": "prepare_resume", "error": prepareErr.Error()})
		}

		_, err = a.terminalMechanics().SendCommand(tmuxCtx, binding, resumeCommand(program, sessionID))
		if err != nil {
			return a.abortPreparedResume(prepared, binding, "session resume command failed: "+err.Error(), actionTmuxFailed)
		}
		pane, err := a.waitForResumeProcess(tmuxCtx, binding, program)
		if err != nil {
			if !errors.Is(err, errResumeProcessNotObserved) {
				return actionResult{Outcome: actionStateFailed, Message: "resume outcome is uncertain; the replacement remains linked and will be reconciled after restart: " + err.Error()}
			}
			return a.abortPreparedResume(prepared, binding, "session resume did not start: "+err.Error(), actionTmuxFailed)
		}

		updated, found, applied, updateErr := a.updateSessionIfCurrent(prepared, func(session *state.TerminalSession) {
			session.State = state.TerminalRunning
			session.WatchEnabled = true
			session.PendingResume = nil
			session.LastKnownCWD = firstNonEmptyExact(pane.CurrentPath, workdir)
			session.LastActivityAt = now
			session.LastRawCaptureHash = ""
			session.LastRawCapture = ""
			session.LastSnapshotCaptureHash = ""
			session.LastSnapshotAttemptAt = time.Time{}
			session.LastRenderHash = ""
			session.LastSummary = ""
			session.SeenUpstreamSignalIDs = nil
			session.LastUpstreamSignalAt = time.Time{}
			session.UpstreamRetryAt = time.Time{}
			retireAlternateReplyTargets(session)
			setAnchorFiles(session, nil)
			session.RecordRecoveryEvent(state.RecoveryEvent{
				At: now, Kind: "resume", Command: resumeCommand(program, sessionID),
				CWD: workdir, Validation: "explicit_resume", Program: program, ProviderSessionID: sessionID,
			})
		})
		committed := found && applied && (updateErr == nil || state.PersistenceReachedReplacement(updateErr))
		if !committed {
			return a.abortPreparedResume(prepared, binding, "session changed while resuming", actionStateFailed)
		}
		if updateErr != nil {
			_ = a.audit("state.session", "durability_uncertain", map[string]any{"session_id": id, "operation": "resume", "error": updateErr.Error()})
		}
		resumed = updated
		_ = a.audit("tmux.resume", "ok", map[string]any{"session_id": id, "program": program, "resume_session_id": sessionID, "pane_id": paneID})
		return actionResult{Outcome: actionOK, Message: fmt.Sprintf("resumed [%d] with %s", id, program)}
	}()

	if result.OK() && resumed.AnchorMessageID != 0 {
		a.advertiseTerminalCapabilities(ctx, resumed)
		a.reconcileAnchorPresentation(ctx, resumed.ID)
		a.queueRefresh(resumed.ID, true, 0)
	}
	return result
}

func (a *App) waitForResumeProcess(ctx context.Context, binding mechanics.Binding, program string) (tmux.Pane, error) {
	deadline := time.Now().Add(resumeProcessObservationTimeout)
	var firstObserved time.Time
	for {
		pane, err := a.terminalMechanics().Validate(ctx, binding)
		if err != nil {
			return tmux.Pane{}, err
		}
		if commandExecutable(pane.CurrentCmd) == program {
			if firstObserved.IsZero() {
				firstObserved = time.Now()
			} else if time.Since(firstObserved) >= resumeProcessSettlePeriod {
				return pane, nil
			}
		} else {
			firstObserved = time.Time{}
		}
		if !time.Now().Before(deadline) {
			return tmux.Pane{}, fmt.Errorf("%w: %s was not observed as the pane foreground process", errResumeProcessNotObserved, program)
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return tmux.Pane{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func (a *App) closeResumeWindow(binding mechanics.Binding) error {
	cleanupCtx, cancel := tmux.TimeoutContext(context.Background())
	defer cancel()
	_, err := a.terminalMechanics().CloseWindow(cleanupCtx, binding)
	return err
}

func (a *App) restorePreparedResume(prepared state.TerminalSession) error {
	pending := prepared.PendingResume
	if pending == nil {
		return fmt.Errorf("pending resume metadata is missing")
	}
	_, found, applied, err := a.updateSessionIfCurrent(prepared, func(session *state.TerminalSession) {
		session.TmuxSessionName = pending.PreviousTmuxSessionName
		session.TmuxWindowID = pending.PreviousTmuxWindowID
		session.TmuxPaneID = pending.PreviousTmuxPaneID
		session.TmuxServerID = pending.PreviousTmuxServerID
		session.Origin = pending.PreviousOrigin
		session.LastKnownCWD = pending.PreviousCWD
		session.ResumeProgram = pending.PreviousResumeProgram
		session.ResumeSessionID = pending.PreviousResumeSessionID
		session.PendingResume = nil
		session.State = state.TerminalLost
		session.WatchEnabled = false
	})
	if err != nil {
		_ = a.audit("state.session", "failed", map[string]any{"session_id": prepared.ID, "operation": "rollback_resume", "error": err.Error()})
		return err
	}
	if !found || !applied {
		return fmt.Errorf("session changed before resume rollback")
	}
	return nil
}

func (a *App) abortPreparedResume(prepared state.TerminalSession, binding mechanics.Binding, message string, outcome actionOutcome) actionResult {
	if err := a.closeResumeWindow(binding); err != nil {
		return actionResult{Outcome: actionStateFailed, Message: message + "; replacement cleanup failed and remains linked to this watch: " + err.Error()}
	}
	if err := a.restorePreparedResume(prepared); err != nil {
		return actionResult{Outcome: actionStateFailed, Message: message + "; recovery state rollback failed: " + err.Error()}
	}
	return actionResult{Outcome: outcome, Message: message}
}

func (a *App) resumeWorkdir(session state.TerminalSession) string {
	for _, candidate := range []string{session.LastKnownCWD, a.Config.Workdir} {
		if candidate == "" {
			continue
		}
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}
	return a.Config.Workdir
}

func firstNonEmptyExact(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
