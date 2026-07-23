package app

import (
	"context"
	"strings"
	"time"

	"github.com/idolum-ai/engram/internal/mechanics"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/tmux"
)

type actionOutcome string

const (
	actionOK             actionOutcome = "ok"
	actionUserError      actionOutcome = "user_error"
	actionTmuxFailed     actionOutcome = "tmux_failed"
	actionTelegramFailed actionOutcome = "telegram_failed"
	actionStateFailed    actionOutcome = "state_failed"
)

const collapsedSessionActionMessage = "session is on the Collapsed sessions shelf; tap ➕ Show to restore all sessions first"

type actionResult struct {
	Outcome actionOutcome
	Message string
}

func (r actionResult) OK() bool {
	return r.Outcome == actionOK
}

func (r actionResult) status(prefix string) string {
	if r.Outcome == "" {
		return prefix + "_unknown"
	}
	return prefix + "_" + string(r.Outcome)
}

func (a *App) sendInput(ctx context.Context, id int, text, mode string, enter bool) actionResult {
	return a.sendInputExpected(ctx, id, text, mode, enter, nil)
}

func (a *App) sendInputExpected(ctx context.Context, id int, text, mode string, enter bool, expectedBinding *state.TerminalSession) actionResult {
	if !enter && strings.ContainsAny(text, "\r\n") {
		return actionResult{Outcome: actionUserError, Message: "/text accepts one line so it cannot submit input implicitly; use /send for multiline input"}
	}
	lock := a.sessionMutex(id)
	lock.Lock()
	completion := a.sendInputExpectedLocked(ctx, id, text, mode, enter, expectedBinding)
	lock.Unlock()
	return a.finishInput(ctx, id, completion)
}

func (a *App) sendReplyInput(ctx context.Context, expected state.TerminalSession, chatID int64, messageID int, text string) actionResult {
	sessionLock := a.sessionMutex(expected.ID)
	sessionLock.Lock()
	anchorLock := a.anchorMutex(expected.ID)
	anchorLock.Lock()
	current, targetState, found := a.Store.FindReplyTarget(chatID, messageID)
	if !found || targetState != state.ReplyTargetCurrent || !sameTerminalBinding(current, expected) {
		anchorLock.Unlock()
		sessionLock.Unlock()
		return actionResult{Outcome: actionUserError, Message: a.staleReply(expected)}
	}
	completion := a.sendInputExpectedLocked(ctx, expected.ID, text, "command", true, &expected)
	anchorLock.Unlock()
	sessionLock.Unlock()
	return a.finishInput(ctx, expected.ID, completion)
}

type inputCompletion struct {
	result         actionResult
	anchorNotice   string
	noticeBinding  state.TerminalSession
	identitySource state.TerminalSession
	identityError  error
	refresh        bool
}

// sendInputExpectedLocked performs terminal and state work while the caller
// owns sessionMutex(id). It deliberately defers every anchor effect so callers
// may also hold anchorMutex(id) in the established session-then-anchor order.
func (a *App) sendInputExpectedLocked(ctx context.Context, id int, text, mode string, enter bool, expectedBinding *state.TerminalSession) inputCompletion {
	ts, ok := a.Store.FindSession(id)
	if !ok {
		return inputCompletion{result: actionResult{Outcome: actionUserError, Message: "session not found"}}
	}
	if ts.State == state.TerminalClosed {
		return inputCompletion{result: actionResult{Outcome: actionUserError, Message: "session is closed"}}
	}
	if ts.Collapsed {
		return inputCompletion{result: actionResult{Outcome: actionUserError, Message: collapsedSessionActionMessage}}
	}
	if ts.PendingResume != nil {
		return inputCompletion{result: actionResult{Outcome: actionUserError, Message: "resume recovery is still being reconciled; try again shortly"}}
	}
	if expectedBinding != nil && !sameTerminalBinding(ts, *expectedBinding) {
		return inputCompletion{result: actionResult{Outcome: actionUserError, Message: "session changed before input could be sent"}}
	}
	tctx, cancel := tmux.TimeoutContext(ctx)
	defer cancel()
	tctx = tmux.InteractiveContext(tctx)
	var pane tmux.Pane
	var err error
	if enter {
		pane, err = a.terminalMechanics().SendCommand(tctx, terminalBinding(ts), text)
	} else {
		pane, err = a.terminalMechanics().SendText(tctx, terminalBinding(ts), text)
	}
	if err != nil {
		stage := mechanics.FailureStage(err)
		_ = a.audit("tmux.send", "failed", map[string]any{"session_id": id, "pane_id": ts.TmuxPaneID, "mode": mode, "enter": enter, "stage": stage, "error": err.Error()})
		if tmux.IsIdentityLoss(err) {
			return inputCompletion{
				result:         actionResult{Outcome: actionTmuxFailed, Message: "session lost; use /sessions to attach the intended pane again"},
				anchorNotice:   "tmux send error: " + err.Error(),
				noticeBinding:  ts,
				identitySource: ts,
				identityError:  err,
			}
		}
		return inputCompletion{
			result:        actionResult{Outcome: actionTmuxFailed, Message: tmuxSendFailureMessage(stage, err)},
			anchorNotice:  "tmux send error: " + err.Error(),
			noticeBinding: ts,
		}
	}
	if err := a.recordValidatedPane(ts, pane); err != nil {
		return inputCompletion{result: actionResult{Outcome: actionStateFailed, Message: err.Error()}}
	}
	_ = a.audit("tmux.send", "ok", map[string]any{"session_id": id, "pane_id": ts.TmuxPaneID, "mode": mode, "enter": enter})
	expected := ts
	expected.State = state.TerminalRunning
	_, found, applied, err := a.updateSessionIfCurrent(expected, func(s *state.TerminalSession) {
		s.LastActivityAt = time.Now().UTC()
		if enter && shellForeground(pane.CurrentCmd) {
			preview := strings.TrimSpace(a.redactText(text))
			if len(preview) > maxRecoveryCommandBytes {
				preview = headUTF8(preview, maxRecoveryCommandBytes)
			}
			s.RecordRecoveryEvent(state.RecoveryEvent{
				At: time.Now().UTC(), Kind: "command", Command: preview, CommandHash: sha(text),
				CWD: pane.CurrentPath, ForegroundBefore: pane.CurrentCmd,
				ExpectedProcess: commandExecutable(text), Validation: "sent_to_shell", Program: commandProgram(text),
			})
		}
	})
	if err != nil {
		_ = a.audit("state.session", "failed", map[string]any{"session_id": id, "mode": mode, "error": err.Error()})
		return inputCompletion{
			result:        actionResult{Outcome: actionStateFailed, Message: "state update failed after tmux input: " + err.Error()},
			anchorNotice:  "state update error after tmux input: " + err.Error(),
			noticeBinding: ts,
		}
	}
	if !found || !applied {
		return inputCompletion{result: actionResult{Outcome: actionStateFailed, Message: "session no longer current after tmux input"}}
	}
	return inputCompletion{result: actionResult{Outcome: actionOK, Message: "sent"}, refresh: true}
}

func (a *App) finishInput(ctx context.Context, id int, completion inputCompletion) actionResult {
	if completion.identityError != nil {
		a.recordIdentityLoss(ctx, completion.identitySource, completion.identityError)
	}
	if completion.anchorNotice != "" {
		if completion.identityError == nil {
			a.invalidatePresentationHashes(completion.noticeBinding)
		}
		a.updateAnchorLocalGuarded(ctx, id, completion.anchorNotice, true, func() bool {
			current, ok := a.Store.FindSession(id)
			return ok && sameTerminalBinding(current, completion.noticeBinding)
		}, nil)
	}
	if completion.refresh {
		a.refreshSoon(id)
	}
	return completion.result
}

func (a *App) sendKeys(ctx context.Context, id int, keys []string) actionResult {
	return a.sendKeyGroupsExpected(ctx, id, [][]string{keys}, strings.Join(keys, " "), 0, nil)
}

func (a *App) sendKeyGroups(ctx context.Context, id int, groups [][]string, preview string, delay time.Duration) actionResult {
	return a.sendKeyGroupsExpected(ctx, id, groups, preview, delay, nil)
}

func (a *App) sendKeyGroupsExpected(ctx context.Context, id int, groups [][]string, preview string, delay time.Duration, expectedBinding *state.TerminalSession) actionResult {
	if len(groups) == 0 {
		a.updateAnchorLocal(ctx, id, "missing keys", true)
		return actionResult{Outcome: actionUserError, Message: "missing keys"}
	}
	for _, keys := range groups {
		if err := tmux.ValidKeys(keys); err != nil {
			a.updateAnchorLocal(ctx, id, err.Error(), true)
			return actionResult{Outcome: actionUserError, Message: err.Error()}
		}
	}
	lock := a.sessionMutex(id)
	lock.Lock()
	completion := a.sendKeyGroupsExpectedLocked(ctx, id, groups, preview, delay, expectedBinding)
	lock.Unlock()
	return a.finishInput(ctx, id, completion)
}

func (a *App) sendKeyGroupsForAnchorExpected(ctx context.Context, expected state.TerminalSession, groups [][]string, preview string, delays []time.Duration) actionResult {
	if len(groups) == 0 || len(groups) != len(delays) {
		return actionResult{Outcome: actionUserError, Message: "invalid key sequence"}
	}
	for _, keys := range groups {
		if err := tmux.ValidKeys(keys); err != nil {
			return actionResult{Outcome: actionUserError, Message: err.Error()}
		}
	}
	sessionLock := a.sessionMutex(expected.ID)
	sessionLock.Lock()
	anchorLock := a.anchorMutex(expected.ID)
	anchorLock.Lock()
	current, ok := a.Store.FindSession(expected.ID)
	if !ok || current.State != state.TerminalRunning || !current.WatchEnabled || current.Collapsed ||
		current.AnchorChatID != expected.AnchorChatID || current.AnchorMessageID != expected.AnchorMessageID ||
		!sameTerminalBinding(current, expected) {
		anchorLock.Unlock()
		sessionLock.Unlock()
		return actionResult{Outcome: actionUserError, Message: "anchor moved; use the newer live message"}
	}
	completion := a.sendKeyGroupsWithDelaysExpectedLocked(ctx, expected.ID, groups, preview, delays, &expected)
	anchorLock.Unlock()
	sessionLock.Unlock()
	return a.finishInput(ctx, expected.ID, completion)
}

func (a *App) sendKeyGroupsExpectedLocked(ctx context.Context, id int, groups [][]string, preview string, delay time.Duration, expectedBinding *state.TerminalSession) inputCompletion {
	delays := make([]time.Duration, len(groups))
	for index := 0; index+1 < len(groups); index++ {
		delays[index] = delay
	}
	return a.sendKeyGroupsWithDelaysExpectedLocked(ctx, id, groups, preview, delays, expectedBinding)
}

func (a *App) sendKeyGroupsWithDelaysExpectedLocked(ctx context.Context, id int, groups [][]string, preview string, delays []time.Duration, expectedBinding *state.TerminalSession) inputCompletion {
	ts, ok := a.Store.FindSession(id)
	if !ok {
		return inputCompletion{result: actionResult{Outcome: actionUserError, Message: "session not found"}}
	}
	if ts.State == state.TerminalClosed {
		return inputCompletion{result: actionResult{Outcome: actionUserError, Message: "session is closed"}}
	}
	if ts.Collapsed {
		return inputCompletion{result: actionResult{Outcome: actionUserError, Message: collapsedSessionActionMessage}}
	}
	if ts.PendingResume != nil {
		return inputCompletion{result: actionResult{Outcome: actionUserError, Message: "resume recovery is still being reconciled; try again shortly"}}
	}
	if expectedBinding != nil && !sameTerminalBinding(ts, *expectedBinding) {
		return inputCompletion{result: actionResult{Outcome: actionUserError, Message: "session changed before keys could be sent"}}
	}
	tctx, cancel := tmux.TimeoutContext(ctx)
	defer cancel()
	tctx = tmux.InteractiveContext(tctx)
	var pane tmux.Pane
	for i, keys := range groups {
		validated, err := a.terminalMechanics().SendKeys(tctx, terminalBinding(ts), keys)
		if err != nil {
			if tmux.IsIdentityLoss(err) {
				return inputCompletion{
					result:         actionResult{Outcome: actionTmuxFailed, Message: "session lost; use /sessions to attach the intended pane again"},
					anchorNotice:   "tmux key error: " + err.Error(),
					noticeBinding:  ts,
					identitySource: ts,
					identityError:  err,
				}
			}
			return inputCompletion{
				result:        actionResult{Outcome: actionTmuxFailed, Message: "tmux key failed: " + err.Error()},
				anchorNotice:  "tmux key error: " + err.Error(),
				noticeBinding: ts,
			}
		}
		pane = validated
		if i < len(delays) && delays[i] > 0 && i < len(groups)-1 && !a.sleepContext(ctx, delays[i]) {
			return inputCompletion{result: actionResult{Outcome: actionTmuxFailed, Message: "key sequence canceled"}}
		}
	}
	if err := a.recordValidatedPane(ts, pane); err != nil {
		return inputCompletion{result: actionResult{Outcome: actionStateFailed, Message: err.Error()}}
	}
	expected := ts
	expected.State = state.TerminalRunning
	_, found, applied, err := a.updateSessionIfCurrent(expected, func(s *state.TerminalSession) {
		s.LastActivityAt = time.Now().UTC()
	})
	if err != nil {
		_ = a.audit("state.session", "failed", map[string]any{"session_id": id, "mode": "keys", "error": err.Error()})
		return inputCompletion{
			result:        actionResult{Outcome: actionStateFailed, Message: "state update failed after tmux keys: " + err.Error()},
			anchorNotice:  "state update error after tmux keys: " + err.Error(),
			noticeBinding: ts,
		}
	}
	if !found || !applied {
		return inputCompletion{result: actionResult{Outcome: actionStateFailed, Message: "session no longer current after tmux keys"}}
	}
	return inputCompletion{
		result:  actionResult{Outcome: actionOK, Message: "sent " + firstNonEmpty(strings.TrimSpace(preview), flattenKeyPreview(groups))},
		refresh: true,
	}
}

func tmuxSendFailureMessage(stage string, err error) string {
	label := "tmux operation"
	switch stage {
	case mechanics.StageIdentity:
		label = "identity check"
	case mechanics.StageInput:
		label = "input delivery"
	}
	return "tmux send failed during " + label + ": " + err.Error()
}

func (a *App) invalidatePresentationHashes(expected state.TerminalSession) {
	_, found, applied, err := a.updateSessionIfCurrent(expected, func(session *state.TerminalSession) {
		session.LastRawCaptureHash = ""
		session.LastSnapshotCaptureHash = ""
	})
	if err != nil || !found || !applied {
		_ = a.audit("state.presentation", "invalidate_failed", map[string]any{"session_id": expected.ID, "error": firstNonEmpty(errorText(err), "superseded")})
	}
}

func (a *App) sessionMutex(id int) *keyedMutexHandle {
	return a.sessionLocks.handle(id)
}

func flattenKeyPreview(groups [][]string) string {
	var keys []string
	for _, group := range groups {
		keys = append(keys, group...)
	}
	return strings.Join(keys, " ")
}
