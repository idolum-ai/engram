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
	sessions := a.Store.Snapshot().TerminalSessions
	a.presentationMu.Lock()
	modeErr := a.Store.SetAnchorMode(mode)
	if modeErr != nil && !state.PersistenceReachedReplacement(modeErr) {
		a.presentationMu.Unlock()
		return actionResult{Outcome: actionStateFailed, Message: "could not persist anchor mode: " + modeErr.Error()}
	}
	a.setAnchorMode(mode)
	modeOutcome := "changed"
	if modeErr != nil {
		modeOutcome = "durability_uncertain"
	}
	_ = a.audit("anchor.mode", modeOutcome, map[string]any{"mode": mode, "error": errorText(modeErr)})
	for _, session := range sessions {
		if session.State == state.TerminalRunning && session.WatchEnabled {
			a.resetConversationEpoch(session.ID)
		}
	}
	a.presentationMu.Unlock()
	for _, session := range sessions {
		if session.State == state.TerminalRunning && session.WatchEnabled {
			a.reconcileAnchorPresentation(ctx, session.ID)
			a.queueRefresh(session.ID, true, 0)
		}
	}
	return actionResult{Outcome: actionOK, Message: "switching to " + mode + " mode"}
}
