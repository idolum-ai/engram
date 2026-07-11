package app

import (
	"context"
	"fmt"
	"time"

	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/terminalshot"
	"github.com/idolum-ai/engram/internal/tmux"
	"github.com/idolum-ai/engram/internal/upstream"
)

const upstreamSignalInterval = 10 * time.Second

// captureUpstreamSignal reconstructs wrapped logical lines only when the
// normal frame contains a marker. Ordinary refreshes pay no additional tmux
// capture cost.
func (a *App) captureUpstreamSignal(ctx context.Context, ts state.TerminalSession, capture tmux.StyledCapture) (payload string, found bool, presentationText string, safe bool) {
	if !upstream.Contains(capture.Text) {
		a.clearUpstreamSignal(ts.ID)
		return "", false, capture.Text, true
	}
	if !acquireSlot(ctx, a.captureSlots) {
		return "", false, "", false
	}
	joined, err := a.Tmux.CaptureJoinedText(ctx, ts.TmuxPaneID, capture.VisibleRows, terminalshot.TargetRows)
	releaseSlot(a.captureSlots)
	if err != nil {
		_ = a.audit("terminal.upstream_signal", "capture_failed", map[string]any{"session_id": ts.ID, "pane_id": ts.TmuxPaneID, "error": err.Error()})
		return "", false, "", false
	}
	payload, ok := upstream.Latest(joined)
	if !ok {
		_ = a.audit("terminal.upstream_signal", "record_missing", map[string]any{"session_id": ts.ID, "pane_id": ts.TmuxPaneID})
		return "", false, "", false
	}
	return payload, true, upstream.WithoutRecords(joined), true
}

func (a *App) clearUpstreamSignal(id int) {
	ts, ok := a.Store.FindSession(id)
	if !ok || ts.LastUpstreamSignalHash == "" {
		return
	}
	if _, _, err := a.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.LastUpstreamSignalHash = ""
	}); err != nil {
		_ = a.audit("state.upstream_signal", "clear_failed", map[string]any{"session_id": id, "error": err.Error()})
	}
}

func (a *App) deliverUpstreamSignal(ctx context.Context, observed state.TerminalSession, payload string) {
	lock := a.anchorMutex(observed.ID)
	lock.Lock()
	defer lock.Unlock()
	a.deliverUpstreamSignalLocked(ctx, observed, payload)
}

func (a *App) deliverUpstreamSignalLocked(ctx context.Context, observed state.TerminalSession, payload string) {
	payloadHash := sha(payload)
	redacted := a.redactText(payload)
	latest, ok := a.Store.FindSession(observed.ID)
	if !ok || latest.State != state.TerminalRunning || !latest.WatchEnabled || latest.AnchorChatID == 0 || latest.AnchorMessageID == 0 || latest.RetiringAnchorMessageID != 0 || latest.TmuxPaneID != observed.TmuxPaneID || latest.TmuxWindowID != observed.TmuxWindowID {
		_ = a.audit("terminal.upstream_signal", "superseded", map[string]any{"session_id": observed.ID, "payload": redacted})
		return
	}
	if latest.LastUpstreamSignalHash == payloadHash {
		return
	}
	now := time.Now().UTC()
	if !latest.LastUpstreamSignalAt.IsZero() && now.Sub(latest.LastUpstreamSignalAt) < upstreamSignalInterval {
		_ = a.audit("terminal.upstream_signal", "coalesced", map[string]any{"session_id": latest.ID, "payload": redacted})
		return
	}
	if a.Telegram == nil {
		_ = a.audit("terminal.upstream_signal", "delivery_failed", map[string]any{"session_id": latest.ID, "payload": redacted, "error": "telegram unavailable"})
		return
	}
	message, err := a.Telegram.SendMessage(ctx, latest.AnchorChatID, fmt.Sprintf("[%d] upstream\n\n%s", latest.ID, redacted), latest.AnchorMessageID, nil)
	if err != nil {
		_ = a.audit("terminal.upstream_signal", "delivery_failed", map[string]any{"session_id": latest.ID, "payload": redacted, "error": err.Error()})
		return
	}
	updated := false
	if _, _, err := a.Store.UpdateSession(latest.ID, func(session *state.TerminalSession) {
		if session.AnchorMessageID == latest.AnchorMessageID && session.TmuxPaneID == latest.TmuxPaneID && session.State == state.TerminalRunning {
			recordAlternateMessage(session, "upstream", message.MessageID)
			session.LastUpstreamSignalHash = payloadHash
			session.LastUpstreamSignalAt = now
			session.LastActivityAt = now
			updated = true
		}
	}); err != nil || !updated {
		_ = a.audit("state.upstream_signal", "delivery_failed", map[string]any{"session_id": latest.ID, "message_id": message.MessageID, "error": firstNonEmpty(errorText(err), "superseded")})
		return
	}
	_ = a.audit("terminal.upstream_signal", "delivered", map[string]any{"session_id": latest.ID, "message_id": message.MessageID, "payload": redacted})
}
