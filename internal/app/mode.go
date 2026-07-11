package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/state"
)

func normalizeAnchorMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "guide", "haiku":
		return config.AnchorModeGuide
	case "snapshot", "chromium":
		return config.AnchorModeSnapshot
	default:
		return ""
	}
}

func (a *App) modeText() string {
	available := make([]string, 0, 2)
	if a.haikuAvailable {
		available = append(available, "guide (Haiku configured, not probed)")
	}
	if a.snapshotReady {
		available = append(available, "snapshot (Chromium probed and ready)")
	}
	return fmt.Sprintf("anchor mode: %s\navailable: %s", a.anchorMode(), strings.Join(available, ", "))
}

func (a *App) switchAnchorMode(ctx context.Context, raw string) actionResult {
	mode := normalizeAnchorMode(raw)
	if mode == "" {
		return actionResult{Outcome: actionUserError, Message: "usage: /mode [guide|snapshot]"}
	}
	if !modeAvailable(mode, a.haikuAvailable, a.snapshotReady) {
		return actionResult{Outcome: actionUserError, Message: mode + " mode is unavailable; check /status"}
	}
	if mode == a.anchorMode() {
		return actionResult{Outcome: actionOK, Message: "already using " + mode + " mode"}
	}
	if err := a.Store.SetAnchorMode(mode); err != nil {
		return actionResult{Outcome: actionStateFailed, Message: "could not persist anchor mode: " + err.Error()}
	}
	a.setAnchorMode(mode)
	_ = a.audit("anchor.mode", "changed", map[string]any{"mode": mode})
	for _, session := range a.Store.Snapshot().TerminalSessions {
		if session.State == state.TerminalRunning && session.WatchEnabled {
			a.reconcileAnchorPresentation(ctx, session.ID)
			a.queueRefresh(session.ID, true, 0)
		}
	}
	return actionResult{Outcome: actionOK, Message: "switching to " + mode + " mode"}
}
