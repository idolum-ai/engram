package app

import (
	"context"
	"errors"
	"fmt"
	"html"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"strings"
	"time"

	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
)

const anchorRetirementRetryDelay = 10 * time.Second

func (a *App) reconcileAnchorPresentation(ctx context.Context, id int) {
	a.presentationMu.RLock()
	defer a.presentationMu.RUnlock()
	lock := a.anchorMutex(id)
	lock.Lock()
	defer lock.Unlock()
	a.finishAnchorRotationLocked(ctx, id)
	ts, ok := a.Store.FindSession(id)
	if !ok || ts.AnchorMessageID == 0 || ts.RetiringAnchorMessageID != 0 {
		return
	}
	formatMismatch := a.snapshotAnchors() && ts.AnchorFormat != anchorFormatSnapshot ||
		!a.snapshotAnchors() && a.snapshotReady && ts.AnchorFormat != anchorFormatGuideEvidence ||
		!a.snapshotAnchors() && !a.snapshotReady && mediaAnchorFormat(ts.AnchorFormat)
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
	a.presentationMu.RLock()
	defer a.presentationMu.RUnlock()
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
	removed := false
	if mediaAnchorFormat(ts.RetiringAnchorFormat) {
		retireErr = a.Telegram.DeleteMessage(ctx, ts.AnchorChatID, oldID)
		if retireErr == nil || isTelegramMessageGone(retireErr) {
			removed = true
			retireErr = nil
		} else {
			_ = a.audit("telegram.anchor_retire", "delete_failed", map[string]any{"session_id": id, "message_id": oldID, "error": retireErr.Error()})
			retireErr = a.replaceMediaWithTombstone(ctx, ts.AnchorChatID, oldID, a.retiredAnchorText(ts))
			if retireErr != nil && isTelegramAnchorUnavailable(retireErr) {
				_ = a.audit("telegram.anchor_retire", "media_retained", map[string]any{"session_id": id, "message_id": oldID, "error": retireErr.Error()})
				_, retireErr = a.Telegram.EditCaption(ctx, ts.AnchorChatID, oldID, a.retiredAnchorText(ts), telegram.ClearMarkup())
			}
		}
	} else {
		_, retireErr = a.editAnchor(ctx, ts.AnchorChatID, oldID, a.retiredAnchorText(ts), telegram.ClearMarkup())
	}
	if retireErr != nil && !telegram.IsMessageNotModified(retireErr) {
		if !isTelegramAnchorUnavailable(retireErr) {
			_ = a.audit("telegram.anchor_retire", "failed", map[string]any{"session_id": id, "message_id": oldID, "stage": "compact", "error": retireErr.Error()})
			a.deferAnchorRetirement(id, oldID)
			return
		}
	}
	if !removed {
		if err := a.Telegram.UnpinChatMessage(ctx, ts.AnchorChatID, oldID); err != nil && !telegram.IsMessageNotPinned(err) && !isTelegramAnchorUnavailable(err) {
			_ = a.audit("telegram.anchor_retire", "failed", map[string]any{"session_id": id, "message_id": oldID, "stage": "unpin", "error": err.Error()})
			a.deferAnchorRetirement(id, oldID)
			return
		}
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
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := a.Telegram.DeleteMessage(cleanupCtx, chatID, messageID); err == nil || isTelegramMessageGone(err) {
		return
	} else {
		_ = a.audit("telegram.prospective_anchor", "delete_failed", map[string]any{"message_id": messageID, "error": err.Error()})
	}
	if _, err := a.editAnchor(cleanupCtx, chatID, messageID, text, telegram.ClearMarkup()); err != nil && !isTelegramAnchorUnavailable(err) {
		_ = a.audit("telegram.prospective_anchor", "deactivate_failed", map[string]any{"message_id": messageID, "error": err.Error()})
	}
	if err := a.Telegram.UnpinChatMessage(cleanupCtx, chatID, messageID); err != nil && !telegram.IsMessageNotPinned(err) && !isTelegramAnchorUnavailable(err) {
		_ = a.audit("telegram.prospective_anchor", "unpin_failed", map[string]any{"message_id": messageID, "error": err.Error()})
	}
}

func (a *App) replaceMediaWithTombstone(ctx context.Context, chatID int64, messageID int, text string) error {
	path, err := neutralAnchorImage(a.Config.ArtifactDir())
	if err != nil {
		return err
	}
	defer os.Remove(path)
	_, err = a.Telegram.EditHTMLPhoto(ctx, chatID, messageID, path, html.EscapeString(text), telegram.ClearMarkup())
	return err
}

func neutralAnchorImage(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create neutral media directory: %w", err)
	}
	file, err := os.CreateTemp(dir, ".engram-neutral-*.png")
	if err != nil {
		return "", fmt.Errorf("create neutral media: %w", err)
	}
	path := file.Name()
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(path)
		}
	}()
	canvas := image.NewRGBA(image.Rect(0, 0, 32, 32))
	draw.Draw(canvas, canvas.Bounds(), &image.Uniform{C: color.RGBA{R: 17, G: 20, B: 24, A: 255}}, image.Point{}, draw.Src)
	if err := png.Encode(file, canvas); err != nil {
		file.Close()
		return "", fmt.Errorf("encode neutral media: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("close neutral media: %w", err)
	}
	keep = true
	return path, nil
}

func anchorShouldBePinned(ts state.TerminalSession) bool {
	return ts.State == state.TerminalRunning && ts.WatchEnabled && ts.AnchorMessageID != 0
}

func (a *App) retiredAnchorText(ts state.TerminalSession) string {
	text := fmt.Sprintf("[%d] %s\ncontinued in the newer live anchor", ts.ID, a.redactText(firstNonEmpty(ts.Title, "session")))
	// The same tombstone is used for text and media predecessors. Keep it below
	// Telegram's parsed caption limit so retirement cannot be blocked by a title.
	return truncateAtWord(text, 900)
}

func isTelegramMessageGone(err error) bool {
	var telegramErr *telegram.Error
	if !errors.As(err, &telegramErr) || (telegramErr.ErrorCode != 400 && telegramErr.StatusCode != 400) {
		return false
	}
	description := strings.ToLower(telegramErr.Description)
	return strings.Contains(description, "message to delete not found") || strings.Contains(description, "message not found")
}

func (a *App) anchorMutex(id int) *keyedMutexHandle {
	return a.anchorLocks.handle(id)
}
