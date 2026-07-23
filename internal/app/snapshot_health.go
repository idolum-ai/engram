package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/terminalshot"
)

const (
	snapshotProbeInitialDelay = 5 * time.Second
	snapshotProbeMaximumDelay = 5 * time.Minute
	snapshotStatusErrorBytes  = 768
)

type snapshotProber interface {
	Probe(context.Context) (string, error)
}

func (a *App) snapshotAvailable() bool {
	a.snapshotHealthMu.RLock()
	defer a.snapshotHealthMu.RUnlock()
	return a.snapshotReady
}

func boolUint64(value bool) uint64 {
	if value {
		return 1
	}
	return 0
}

func (a *App) snapshotRenderHealth() (ready bool, generation uint64) {
	a.snapshotHealthMu.RLock()
	defer a.snapshotHealthMu.RUnlock()
	return a.snapshotReady, a.snapshotGeneration
}

// acquireSnapshotRender reserves the renderer and samples health only after
// the wait, so a recovery or failure while queued cannot stale its generation.
func (a *App) acquireSnapshotRender(ctx context.Context) (generation uint64, ok bool) {
	if !acquireSlot(ctx, a.renderSlots) {
		return 0, false
	}
	ready, generation := a.snapshotRenderHealth()
	if !ready {
		releaseSlot(a.renderSlots)
		return 0, false
	}
	return generation, true
}

func (a *App) snapshotHealth() (ready bool, browserPath, probeError string, probedAt, retryAt time.Time) {
	a.snapshotHealthMu.RLock()
	defer a.snapshotHealthMu.RUnlock()
	return a.snapshotReady, a.snapshotBrowserPath, a.snapshotProbeError, a.snapshotProbeAt, a.snapshotNextProbe
}

func snapshotProbeDelay(failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	delay := snapshotProbeInitialDelay
	for i := 1; i < failures && delay < snapshotProbeMaximumDelay; i++ {
		delay *= 2
		if delay >= snapshotProbeMaximumDelay {
			return snapshotProbeMaximumDelay
		}
	}
	return delay
}

// recoverSnapshots performs one bounded, single-flight browser probe when the
// renderer is unavailable and its retry deadline has arrived. It returns true
// only for the caller that changes the renderer from unavailable to ready.
func (a *App) recoverSnapshots(ctx context.Context, now time.Time) bool {
	if a.SnapshotProber == nil {
		return false
	}
	a.snapshotHealthMu.Lock()
	if a.snapshotReady || a.snapshotProbeRunning || (!a.snapshotNextProbe.IsZero() && now.Before(a.snapshotNextProbe)) {
		a.snapshotHealthMu.Unlock()
		return false
	}
	a.snapshotProbeRunning = true
	a.snapshotHealthMu.Unlock()

	path, err := a.SnapshotProber.Probe(ctx)
	completedAt := a.snapshotCurrentTime()
	a.snapshotHealthMu.Lock()
	a.snapshotProbeRunning = false
	a.snapshotProbeAt = completedAt
	if err != nil {
		a.snapshotReady = false
		a.snapshotProbeError = err.Error()
		a.snapshotProbeFailures++
		delay := snapshotProbeDelay(a.snapshotProbeFailures)
		a.snapshotNextProbe = completedAt.Add(delay)
		a.snapshotHealthMu.Unlock()
		if a.Store != nil {
			_ = a.audit("snapshot.probe", "failed", map[string]any{"error": err.Error(), "retry_in": delay.String()})
		}
		return false
	}
	a.snapshotReady = true
	a.snapshotGeneration++
	a.snapshotBrowserPath = path
	a.snapshotProbeError = ""
	a.snapshotProbeFailures = 0
	a.snapshotNextProbe = time.Time{}
	a.snapshotHealthMu.Unlock()
	if a.Store != nil {
		_ = a.audit("snapshot.probe", "recovered", map[string]any{"browser": path})
	}
	return true
}

func (a *App) snapshotCurrentTime() time.Time {
	if a.snapshotNow != nil {
		return a.snapshotNow().UTC()
	}
	return time.Now().UTC()
}

func (a *App) snapshotRecoveryLoop(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if !a.recoverSnapshots(ctx, now) {
				continue
			}
			for _, session := range a.Store.Snapshot().TerminalSessions {
				if snapshotRecoveryEligible(session) {
					a.queueRefresh(session.ID, true, 0)
					a.reconcileAnchorPresentation(ctx, session.ID)
				}
			}
		}
	}
}

func snapshotRecoveryEligible(session state.TerminalSession) bool {
	return session.State == state.TerminalRunning && session.WatchEnabled && !session.Collapsed
}

func (a *App) markSnapshotsUnavailable(err error, now time.Time, observedGeneration uint64) bool {
	if err == nil || errors.Is(err, context.Canceled) || !terminalshot.IsBrowserFailure(err) {
		return false
	}
	a.snapshotHealthMu.Lock()
	if !a.snapshotReady || a.snapshotGeneration != observedGeneration {
		a.snapshotHealthMu.Unlock()
		return false
	}
	a.snapshotReady = false
	a.snapshotProbeError = err.Error()
	a.snapshotProbeAt = now.UTC()
	a.snapshotProbeFailures = 1
	delay := snapshotProbeDelay(a.snapshotProbeFailures)
	a.snapshotNextProbe = now.UTC().Add(delay)
	a.snapshotHealthMu.Unlock()
	if a.Store != nil {
		_ = a.audit("snapshot.health", "unavailable", map[string]any{"error": err.Error(), "retry_in": delay.String()})
	}
	return true
}

func snapshotUnavailableStatus(probeError string, retryAt time.Time) string {
	status := "unavailable"
	if probeError != "" {
		diagnostic := headUTF8(probeError, snapshotStatusErrorBytes)
		if diagnostic != probeError {
			diagnostic += "..."
		}
		status += ": " + diagnostic
	}
	if !retryAt.IsZero() {
		status += fmt.Sprintf("; retry after %s", retryAt.UTC().Format(time.RFC3339))
	}
	return status
}
