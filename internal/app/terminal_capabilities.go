package app

import (
	"context"
	"sort"
	"time"

	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/tmux"
)

const (
	terminalCapabilityInitialRetry = time.Second
	terminalCapabilityMaxRetry     = 30 * time.Second
	terminalCapabilitySweepTimeout = 2 * time.Second
)

type capabilityRetry struct {
	due   time.Time
	delay time.Duration
}

// The session argument identifies which desired state changed. Reconciliation
// always re-reads current state under the per-session capability lock, so a
// stale caller cannot republish an option after unwatch or clear a new watch.
func (a *App) advertiseTerminalCapabilities(ctx context.Context, session state.TerminalSession) {
	a.reconcileTerminalCapabilities(ctx, session.ID)
}

func (a *App) clearTerminalCapabilities(ctx context.Context, session state.TerminalSession) {
	a.reconcileTerminalCapabilities(ctx, session.ID)
}

func (a *App) reconcileTerminalCapabilities(ctx context.Context, sessionID int) {
	lock := a.capabilityLocks.handle(sessionID)
	lock.Lock()
	session, ok := a.Store.FindSession(sessionID)
	if !ok || a.Tmux.Runner == nil || session.TmuxPaneID == "" {
		a.clearTerminalCapabilityRetry(sessionID)
		lock.Unlock()
		return
	}
	advertise := session.State == state.TerminalRunning && session.WatchEnabled
	tmuxCtx, cancel := tmux.TimeoutContext(ctx)
	var err error
	if advertise {
		err = a.terminalMechanics().AdvertiseEngram(tmuxCtx, terminalBinding(session), session.ID)
	} else {
		err = a.terminalMechanics().ClearEngramAdvertisement(tmuxCtx, terminalBinding(session))
	}
	cancel()
	// Retry state is part of the serialized capability result. Finalizing it
	// before unlock prevents an older success from deleting a newer failure's
	// pending retry after concurrent watch state changes.
	if err == nil || tmux.IsIdentityLoss(err) {
		a.clearTerminalCapabilityRetry(session.ID)
	} else {
		a.scheduleTerminalCapabilityRetry(session.ID)
	}
	lock.Unlock()
	if a.capabilityFinishHook != nil {
		a.capabilityFinishHook(session.ID, err)
	}

	a.finishTerminalCapabilityReconcile(ctx, session, advertise, err)
}

func (a *App) finishTerminalCapabilityReconcile(ctx context.Context, session state.TerminalSession, advertised bool, err error) {
	action := "cleared"
	failure := "clear_failed"
	if advertised {
		action = "advertised"
		failure = "advertise_failed"
	}
	if err == nil {
		_ = a.audit("tmux.capabilities", action, map[string]any{"session_id": session.ID, "pane_id": session.TmuxPaneID})
		return
	}
	if advertised && tmux.IsIdentityLoss(err) {
		a.markWatchedSessionLost(ctx, session, err)
	}
	if advertised || !tmux.IsIdentityLoss(err) {
		_ = a.audit("tmux.capabilities", failure, map[string]any{"session_id": session.ID, "pane_id": session.TmuxPaneID, "error": err.Error()})
	}
}

func (a *App) queueTerminalCapabilityReconcile(sessionID int) {
	if sessionID <= 0 {
		return
	}
	a.capabilityRetryMu.Lock()
	if a.capabilityRetries == nil {
		a.capabilityRetries = make(map[int]capabilityRetry)
	}
	a.capabilityRetries[sessionID] = capabilityRetry{due: time.Now()}
	a.capabilityRetryMu.Unlock()
}

func (a *App) scheduleTerminalCapabilityRetry(sessionID int) {
	a.capabilityRetryMu.Lock()
	if a.capabilityRetries == nil {
		a.capabilityRetries = make(map[int]capabilityRetry)
	}
	retry := a.capabilityRetries[sessionID]
	if retry.delay <= 0 {
		retry.delay = terminalCapabilityInitialRetry
	} else {
		retry.delay = min(retry.delay*2, terminalCapabilityMaxRetry)
	}
	retry.due = time.Now().Add(retry.delay)
	a.capabilityRetries[sessionID] = retry
	a.capabilityRetryMu.Unlock()
}

func (a *App) clearTerminalCapabilityRetry(sessionID int) {
	a.capabilityRetryMu.Lock()
	delete(a.capabilityRetries, sessionID)
	a.capabilityRetryMu.Unlock()
}

func (a *App) dueTerminalCapabilityRetries(now time.Time) []int {
	a.capabilityRetryMu.Lock()
	ids := make([]int, 0, len(a.capabilityRetries))
	for id, retry := range a.capabilityRetries {
		if !retry.due.After(now) {
			ids = append(ids, id)
		}
	}
	a.capabilityRetryMu.Unlock()
	sort.Ints(ids)
	return ids
}

func (a *App) reconcileDueTerminalCapabilities(ctx context.Context, now time.Time) {
	ids := a.dueTerminalCapabilityRetries(now)
	if len(ids) == 0 {
		return
	}
	sweepCtx, cancel := context.WithTimeout(ctx, terminalCapabilitySweepTimeout)
	defer cancel()
	for _, id := range ids {
		if sweepCtx.Err() != nil {
			return
		}
		a.reconcileTerminalCapabilities(sweepCtx, id)
	}
}
