package app

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
)

const anchorRetirementRetryDelay = 10 * time.Second

func (a *App) reconcileAnchorPresentation(ctx context.Context, id int) {
	lock := a.anchorMutex(id)
	lock.Lock()
	defer lock.Unlock()
	a.finishAnchorRotationLocked(ctx, id)
	ts, ok := a.Store.FindSession(id)
	if !ok || ts.AnchorMessageID == 0 || ts.RetiringAnchorMessageID != 0 {
		return
	}
	formatMismatch := a.snapshotAnchors() && ts.AnchorFormat != "snapshot" || !a.snapshotAnchors() && ts.AnchorFormat == "snapshot"
	if formatMismatch && ts.State == state.TerminalRunning && ts.WatchEnabled {
		a.queueRefresh(id, true, 0)
	}
	if anchorShouldBePinned(ts) {
		a.ensureCurrentAnchorPinnedLocked(ctx, ts)
	} else if !ts.AnchorPinKnown || ts.AnchorPinned {
		a.ensureCurrentAnchorUnpinnedLocked(ctx, ts)
	}
}

func (a *App) reconcileAnchorControls(ctx context.Context, id int) {
	lock := a.anchorMutex(id)
	lock.Lock()
	defer lock.Unlock()
	a.finishAnchorRotationLocked(ctx, id)
	ts, ok := a.Store.FindSession(id)
	if !ok || ts.AnchorMessageID == 0 || ts.RetiringAnchorMessageID != 0 || ts.State == state.TerminalClosed {
		return
	}
	if _, err := a.Telegram.EditReplyMarkup(ctx, ts.AnchorChatID, ts.AnchorMessageID, a.anchorMarkup(ts)); err != nil && !telegram.IsMessageNotModified(err) && !isTelegramAnchorUnavailable(err) {
		_ = a.audit("telegram.anchor_controls", "failed", map[string]any{"session_id": id, "error": err.Error()})
		return
	}
	_ = a.audit("telegram.anchor_controls", "ok", map[string]any{"session_id": id})
}

func (a *App) finishAnchorRotationLocked(ctx context.Context, id int) {
	ts, ok := a.Store.FindSession(id)
	if !ok || ts.AnchorMessageID == 0 || ts.RetiringAnchorMessageID == 0 || ts.RetiringAnchorMessageID == ts.AnchorMessageID {
		return
	}
	oldID := ts.RetiringAnchorMessageID
	if !ts.RetiringAnchorRetryAt.IsZero() && time.Now().Before(ts.RetiringAnchorRetryAt) {
		return
	}
	if anchorShouldBePinned(ts) && !a.ensureCurrentAnchorPinnedLocked(ctx, ts) {
		a.deferAnchorRetirement(id, oldID)
		return
	}
	var retireErr error
	if ts.RetiringAnchorFormat == "snapshot" {
		_, retireErr = a.Telegram.EditCaption(ctx, ts.AnchorChatID, oldID, retiredAnchorText(ts), telegram.ClearMarkup())
	} else {
		_, retireErr = a.editAnchor(ctx, ts.AnchorChatID, oldID, retiredAnchorText(ts), telegram.ClearMarkup())
	}
	if retireErr != nil && !telegram.IsMessageNotModified(retireErr) {
		if !isTelegramAnchorUnavailable(retireErr) {
			_ = a.audit("telegram.anchor_retire", "failed", map[string]any{"session_id": id, "message_id": oldID, "stage": "compact", "error": retireErr.Error()})
			a.deferAnchorRetirement(id, oldID)
			return
		}
	}
	if err := a.Telegram.UnpinChatMessage(ctx, ts.AnchorChatID, oldID); err != nil && !telegram.IsMessageNotPinned(err) && !isTelegramAnchorUnavailable(err) {
		_ = a.audit("telegram.anchor_retire", "failed", map[string]any{"session_id": id, "message_id": oldID, "stage": "unpin", "error": err.Error()})
		a.deferAnchorRetirement(id, oldID)
		return
	}
	if _, _, err := a.Store.UpdateSession(id, func(session *state.TerminalSession) {
		if session.RetiringAnchorMessageID == oldID {
			recordStaleMessage(session, oldID)
			session.RetiringAnchorMessageID = 0
			session.RetiringAnchorFormat = ""
			session.RetiringAnchorRetryAt = time.Time{}
		}
	}); err != nil {
		_ = a.audit("state.anchor_rotation", "failed", map[string]any{"session_id": id, "error": err.Error()})
		return
	}
	_ = a.audit("telegram.anchor_retire", "ok", map[string]any{"session_id": id, "message_id": oldID})
}

func (a *App) deferAnchorRetirement(id, messageID int) {
	if _, _, err := a.Store.UpdateSession(id, func(session *state.TerminalSession) {
		if session.RetiringAnchorMessageID == messageID {
			session.RetiringAnchorRetryAt = time.Now().UTC().Add(anchorRetirementRetryDelay)
		}
	}); err != nil {
		_ = a.audit("state.anchor_retire", "retry_failed", map[string]any{"session_id": id, "message_id": messageID, "error": err.Error()})
	}
}

func (a *App) ensureCurrentAnchorPinnedLocked(ctx context.Context, ts state.TerminalSession) bool {
	if ts.AnchorPinKnown && ts.AnchorPinned {
		return true
	}
	if a.Telegram == nil {
		return false
	}
	if err := a.Telegram.PinChatMessage(ctx, ts.AnchorChatID, ts.AnchorMessageID); err != nil && !telegram.IsMessageAlreadyPinned(err) {
		_ = a.audit("telegram.anchor_pin", "failed", map[string]any{"session_id": ts.ID, "message_id": ts.AnchorMessageID, "error": err.Error()})
		return false
	}
	recorded := false
	if _, _, err := a.Store.UpdateSession(ts.ID, func(session *state.TerminalSession) {
		if session.AnchorMessageID == ts.AnchorMessageID && anchorShouldBePinned(*session) {
			session.AnchorPinned = true
			session.AnchorPinKnown = true
			recorded = true
		}
	}); err != nil {
		_ = a.audit("state.anchor_pin", "failed", map[string]any{"session_id": ts.ID, "error": err.Error()})
		return false
	}
	if !recorded {
		_ = a.Telegram.UnpinChatMessage(ctx, ts.AnchorChatID, ts.AnchorMessageID)
		return false
	}
	_ = a.audit("telegram.anchor_pin", "ok", map[string]any{"session_id": ts.ID, "message_id": ts.AnchorMessageID})
	return true
}

func (a *App) ensureCurrentAnchorUnpinnedLocked(ctx context.Context, ts state.TerminalSession) bool {
	if ts.AnchorPinKnown && !ts.AnchorPinned {
		return true
	}
	if a.Telegram == nil {
		return false
	}
	if err := a.Telegram.UnpinChatMessage(ctx, ts.AnchorChatID, ts.AnchorMessageID); err != nil && !telegram.IsMessageNotPinned(err) && !isTelegramAnchorUnavailable(err) {
		_ = a.audit("telegram.anchor_unpin", "failed", map[string]any{"session_id": ts.ID, "message_id": ts.AnchorMessageID, "error": err.Error()})
		return false
	}
	if _, _, err := a.Store.UpdateSession(ts.ID, func(session *state.TerminalSession) {
		if session.AnchorMessageID == ts.AnchorMessageID {
			session.AnchorPinned = false
			session.AnchorPinKnown = true
		}
	}); err != nil {
		_ = a.audit("state.anchor_pin", "failed", map[string]any{"session_id": ts.ID, "error": err.Error()})
		return false
	}
	_ = a.audit("telegram.anchor_unpin", "ok", map[string]any{"session_id": ts.ID, "message_id": ts.AnchorMessageID})
	return true
}

func (a *App) deactivateProspectiveAnchor(ctx context.Context, chatID int64, messageID int, text string) {
	_, _ = a.editAnchor(ctx, chatID, messageID, text, telegram.ClearMarkup())
	_ = a.Telegram.UnpinChatMessage(ctx, chatID, messageID)
}

func anchorShouldBePinned(ts state.TerminalSession) bool {
	return ts.State == state.TerminalRunning && ts.WatchEnabled && ts.AnchorMessageID != 0
}

func retiredAnchorText(ts state.TerminalSession) string {
	return fmt.Sprintf("[%d] %s\ncontinued in the newer live anchor", ts.ID, firstNonEmpty(ts.Title, "session"))
}

func (a *App) anchorMutex(id int) *sync.Mutex {
	lock, _ := a.anchorLocks.LoadOrStore(id, &sync.Mutex{})
	return lock.(*sync.Mutex)
}
