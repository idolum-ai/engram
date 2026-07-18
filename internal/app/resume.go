package app

import (
	"context"
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

const (
	resumeProgramCodex  = recovery.ProgramCodex
	resumeProgramClaude = recovery.ProgramClaude
)

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
		workdir := a.resumeWorkdir(current)
		title := tmux.WindowTitle(id, firstNonEmpty(current.Title, program))
		windowID, paneID, err := a.Tmux.NewWindow(tmuxCtx, tmuxSessionID, workdir, title)
		if err != nil {
			return actionResult{Outcome: actionTmuxFailed, Message: "tmux window creation failed: " + err.Error()}
		}
		binding := mechanics.Binding{PaneID: paneID, WindowID: windowID, ServerID: serverID}
		pane, err := a.terminalMechanics().SendCommand(tmuxCtx, binding, resumeCommand(program, sessionID))
		if err != nil {
			_, _ = a.terminalMechanics().CloseWindow(tmuxCtx, binding)
			return actionResult{Outcome: actionTmuxFailed, Message: "session resume command failed: " + err.Error()}
		}

		now := time.Now().UTC()
		updated, found, applied, updateErr := a.updateSessionIfCurrent(current, func(session *state.TerminalSession) {
			session.TmuxSessionName = sessionName
			session.TmuxWindowID = windowID
			session.TmuxPaneID = paneID
			session.TmuxServerID = serverID
			session.Origin = state.TerminalOriginCreated
			session.State = state.TerminalRunning
			session.WatchEnabled = true
			session.ResumeProgram = program
			session.ResumeSessionID = sessionID
			session.LastKnownCWD = firstNonEmpty(pane.CurrentPath, workdir)
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
			_, _ = a.terminalMechanics().CloseWindow(tmuxCtx, binding)
			return actionResult{Outcome: actionStateFailed, Message: "session changed while resuming"}
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

func (a *App) resumeWorkdir(session state.TerminalSession) string {
	for _, candidate := range []string{session.LastKnownCWD, a.Config.Workdir} {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}
	return a.Config.Workdir
}
