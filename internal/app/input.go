package app

import (
	"context"
	"strings"
	"time"

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
	lock := a.sessionMutex(id)
	lock.Lock()
	defer lock.Unlock()
	ts, ok := a.Store.FindSession(id)
	if !ok {
		return actionResult{Outcome: actionUserError, Message: "session not found"}
	}
	if ts.State == state.TerminalClosed {
		return actionResult{Outcome: actionUserError, Message: "session is closed"}
	}
	if expectedBinding != nil && !sameTerminalBinding(ts, *expectedBinding) {
		return actionResult{Outcome: actionUserError, Message: "session changed before input could be sent"}
	}
	tctx, cancel := tmux.TimeoutContext(ctx)
	defer cancel()
	var pane tmux.Pane
	var err error
	if enter {
		pane, err = a.terminalMechanics().SendCommand(tctx, terminalBinding(ts), text)
	} else {
		pane, err = a.terminalMechanics().SendText(tctx, terminalBinding(ts), text)
	}
	if err != nil {
		a.recordIdentityLoss(ctx, ts, err)
		_ = a.audit("tmux.send", "failed", map[string]any{"session_id": id, "pane_id": ts.TmuxPaneID, "mode": mode, "enter": enter, "error": err.Error()})
		a.updateAnchorLocal(ctx, id, "tmux send error: "+err.Error(), true)
		if tmux.IsIdentityLoss(err) {
			return actionResult{Outcome: actionTmuxFailed, Message: "session lost; use /sessions to attach the intended pane again"}
		}
		return actionResult{Outcome: actionTmuxFailed, Message: "tmux send failed: " + err.Error()}
	}
	if err := a.recordValidatedPane(ts, pane); err != nil {
		return actionResult{Outcome: actionStateFailed, Message: err.Error()}
	}
	_ = a.audit("tmux.send", "ok", map[string]any{"session_id": id, "pane_id": ts.TmuxPaneID, "mode": mode, "enter": enter})
	expected := ts
	expected.State = state.TerminalRunning
	_, found, applied, err := a.updateSessionIfCurrent(expected, func(s *state.TerminalSession) {
		s.LastActivityAt = time.Now().UTC()
	})
	if err != nil {
		_ = a.audit("state.session", "failed", map[string]any{"session_id": id, "mode": mode, "error": err.Error()})
		a.updateAnchorLocal(ctx, id, "state update error after tmux input: "+err.Error(), true)
		return actionResult{Outcome: actionStateFailed, Message: "state update failed after tmux input: " + err.Error()}
	}
	if !found || !applied {
		return actionResult{Outcome: actionStateFailed, Message: "session no longer current after tmux input"}
	}
	a.refreshSoon(id)
	return actionResult{Outcome: actionOK, Message: "sent"}
}

func (a *App) sendKeys(ctx context.Context, id int, keys []string) actionResult {
	return a.sendKeyGroups(ctx, id, [][]string{keys}, strings.Join(keys, " "), 0)
}

func (a *App) sendKeyGroups(ctx context.Context, id int, groups [][]string, preview string, delay time.Duration) actionResult {
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
	defer lock.Unlock()
	ts, ok := a.Store.FindSession(id)
	if !ok {
		return actionResult{Outcome: actionUserError, Message: "session not found"}
	}
	if ts.State == state.TerminalClosed {
		return actionResult{Outcome: actionUserError, Message: "session is closed"}
	}
	tctx, cancel := tmux.TimeoutContext(ctx)
	defer cancel()
	var pane tmux.Pane
	for i, keys := range groups {
		validated, err := a.terminalMechanics().SendKeys(tctx, terminalBinding(ts), keys)
		if err != nil {
			a.recordIdentityLoss(ctx, ts, err)
			a.updateAnchorLocal(ctx, id, "tmux key error: "+err.Error(), true)
			if tmux.IsIdentityLoss(err) {
				return actionResult{Outcome: actionTmuxFailed, Message: "session lost; use /sessions to attach the intended pane again"}
			}
			return actionResult{Outcome: actionTmuxFailed, Message: "tmux key failed: " + err.Error()}
		}
		pane = validated
		if delay > 0 && i < len(groups)-1 && !a.sleepContext(ctx, delay) {
			return actionResult{Outcome: actionTmuxFailed, Message: "key sequence canceled"}
		}
	}
	if err := a.recordValidatedPane(ts, pane); err != nil {
		return actionResult{Outcome: actionStateFailed, Message: err.Error()}
	}
	expected := ts
	expected.State = state.TerminalRunning
	_, found, applied, err := a.updateSessionIfCurrent(expected, func(s *state.TerminalSession) {
		s.LastActivityAt = time.Now().UTC()
	})
	if err != nil {
		_ = a.audit("state.session", "failed", map[string]any{"session_id": id, "mode": "keys", "error": err.Error()})
		a.updateAnchorLocal(ctx, id, "state update error after tmux keys: "+err.Error(), true)
		return actionResult{Outcome: actionStateFailed, Message: "state update failed after tmux keys: " + err.Error()}
	}
	if !found || !applied {
		return actionResult{Outcome: actionStateFailed, Message: "session no longer current after tmux keys"}
	}
	a.refreshSoon(id)
	return actionResult{Outcome: actionOK, Message: "sent " + firstNonEmpty(strings.TrimSpace(preview), flattenKeyPreview(groups))}
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
