package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
	"github.com/idolum-ai/engram/internal/tmux"
	"github.com/idolum-ai/engram/internal/upstream"
)

const upstreamSignalInterval = 10 * time.Second

func observeUpstreamSignal(capture tmux.StyledCapture) upstream.Observation {
	return upstream.Observe(capture.JoinedText)
}

// processCapturedFrame is the app boundary for styled captures: terminal
// records are delivered and removed before semantic text reaches a caller.
func (a *App) processCapturedFrame(ctx context.Context, observed state.TerminalSession, capture tmux.StyledCapture) string {
	observation := observeUpstreamSignal(capture)
	if observation.Found {
		a.deliverUpstreamSignal(ctx, observed, observation.Latest)
	}
	return observation.PresentationText
}

func (a *App) deliverUpstreamSignal(ctx context.Context, observed state.TerminalSession, record upstream.Record) {
	lock := a.anchorMutex(observed.ID)
	lock.Lock()
	defer lock.Unlock()
	a.deliverUpstreamSignalLocked(ctx, observed, record)
}

func (a *App) deliverUpstreamSignalLocked(ctx context.Context, observed state.TerminalSession, record upstream.Record) {
	latest, ok := a.Store.FindSession(observed.ID)
	if !ok || latest.State != state.TerminalRunning || !latest.WatchEnabled || latest.AnchorChatID == 0 || latest.AnchorMessageID == 0 || latest.RetiringAnchorMessageID != 0 || !sameTerminalBinding(latest, observed) {
		_ = a.audit("terminal.upstream_signal", "superseded", map[string]any{"session_id": observed.ID, "record_id": record.ID})
		return
	}
	if latest.HasSeenUpstreamSignal(record.ID) {
		return
	}
	now := time.Now().UTC()
	if now.Before(a.upstreamRetryDeadline(latest.ID, latest.UpstreamRetryAt)) {
		return
	}
	if !latest.LastUpstreamSignalAt.IsZero() && now.Sub(latest.LastUpstreamSignalAt) < upstreamSignalInterval {
		_ = a.audit("terminal.upstream_signal", "coalesced", map[string]any{"session_id": latest.ID, "record_id": record.ID})
		return
	}
	redacted := a.redactText(record.Payload)
	if a.Telegram == nil {
		_ = a.audit("terminal.upstream_signal", "delivery_failed", map[string]any{"session_id": latest.ID, "record_id": record.ID, "payload": redacted, "error": "telegram unavailable"})
		return
	}
	text := fmt.Sprintf("[%d] terminal-authored signal\n\n%s", latest.ID, redacted)
	message, err := a.Telegram.SendMessage(ctx, latest.AnchorChatID, text, latest.AnchorMessageID, nil)
	standalone := false
	if isTelegramReplyUnavailable(err) {
		message, err = a.Telegram.SendMessage(ctx, latest.AnchorChatID, text, 0, nil)
		standalone = err == nil
		if standalone {
			a.queueSignalAnchorRecovery(latest.ID)
		}
	}
	if err != nil {
		a.noteUpstreamRetry(latest.ID, err)
		_ = a.audit("terminal.upstream_signal", "delivery_failed", map[string]any{"session_id": latest.ID, "record_id": record.ID, "payload": redacted, "error": err.Error()})
		return
	}
	deliveredAt := time.Now().UTC()
	updated := false
	_, _, stateErr := a.Store.UpdateSession(latest.ID, func(session *state.TerminalSession) {
		if session.AnchorMessageID == latest.AnchorMessageID && sameTerminalBinding(*session, latest) && session.State == state.TerminalRunning {
			recordAlternateMessage(session, "upstream", message.MessageID)
			session.RecordSeenUpstreamSignal(record.ID)
			session.LastUpstreamSignalAt = deliveredAt
			session.UpstreamRetryAt = time.Time{}
			session.LastActivityAt = deliveredAt
			updated = true
		}
	})
	if stateErr != nil && state.PersistenceReachedReplacement(stateErr) && updated {
		_ = a.audit("state.upstream_signal", "durability_uncertain", map[string]any{"session_id": latest.ID, "message_id": message.MessageID, "record_id": record.ID, "error": stateErr.Error()})
		return
	}
	if stateErr != nil || !updated {
		deleteCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		deleteErr := a.Telegram.DeleteMessage(deleteCtx, latest.AnchorChatID, message.MessageID)
		cancel()
		_ = a.audit("state.upstream_signal", "delivery_failed", map[string]any{"session_id": latest.ID, "message_id": message.MessageID, "record_id": record.ID, "compensating_delete_error": errorText(deleteErr), "error": firstNonEmpty(errorText(stateErr), "superseded")})
		return
	}
	a.signalRetries.Delete(latest.ID)
	_ = a.audit("terminal.upstream_signal", "delivered", map[string]any{"session_id": latest.ID, "message_id": message.MessageID, "record_id": record.ID, "standalone": standalone, "payload": redacted})
}

func (a *App) noteUpstreamRetry(id int, err error) {
	retryAfter := telegram.RetryAfter(err)
	if retryAfter <= 0 {
		return
	}
	deadline := time.Now().UTC().Add(retryAfter)
	// Keep the deadline process-local first. A pre-replacement persistence
	// failure must not turn a Telegram rate limit into an immediate retry loop.
	a.signalRetries.Store(id, deadline)
	_, _, stateErr := a.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.UpstreamRetryAt = deadline
	})
	if stateErr != nil {
		_ = a.audit("state.upstream_signal", "retry_failed", map[string]any{"session_id": id, "error": stateErr.Error()})
	}
}

func (a *App) upstreamRetryDeadline(id int, persisted time.Time) time.Time {
	transient, ok := a.signalRetries.Load(id)
	if !ok {
		return persisted
	}
	deadline, ok := transient.(time.Time)
	if ok && deadline.After(persisted) {
		return deadline
	}
	return persisted
}

func (a *App) queueSignalAnchorRecovery(id int) {
	if a.runCtx == nil {
		return
	}
	if a.snapshotAnchors() {
		a.queueManualRefresh(id)
		return
	}
	a.queueRefresh(id, true, 0)
}

func isTelegramReplyUnavailable(err error) bool {
	if err == nil {
		return false
	}
	var telegramErr *telegram.Error
	if !errors.As(err, &telegramErr) || (telegramErr.ErrorCode != 400 && telegramErr.StatusCode != 400) {
		return false
	}
	description := strings.ToLower(telegramErr.Description)
	return strings.Contains(description, "message to be replied not found") ||
		strings.Contains(description, "reply message not found") ||
		strings.Contains(description, "replied message not found")
}
