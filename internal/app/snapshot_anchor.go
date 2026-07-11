package app

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
	"github.com/idolum-ai/engram/internal/terminalshot"
	"github.com/idolum-ai/engram/internal/tmux"
)

func (a *App) refreshSnapshotAnchor(ctx context.Context, id int, _ bool) {
	manual := a.consumeManualRefresh(id)
	ts, ok := a.Store.FindSession(id)
	if !ok || ts.TmuxPaneID == "" || ts.State != state.TerminalRunning || !ts.WatchEnabled || ts.AnchorMessageID == 0 {
		return
	}
	if !a.finishSnapshotMigration(ctx, id) {
		return
	}
	ts, ok = a.Store.FindSession(id)
	if !ok {
		return
	}
	if !manual && !ts.LastSnapshotAttemptAt.IsZero() && time.Since(ts.LastSnapshotAttemptAt) < 10*time.Second {
		return
	}
	attemptedAt := time.Now().UTC()
	if _, _, err := a.Store.UpdateSession(id, func(session *state.TerminalSession) {
		if session.AnchorMessageID == ts.AnchorMessageID && session.State == state.TerminalRunning && session.WatchEnabled {
			session.LastSnapshotAttemptAt = attemptedAt
		}
	}); err != nil {
		_ = a.audit("state.snapshot_anchor", "attempt_failed", map[string]any{"session_id": id, "error": err.Error()})
		return
	}

	tctx, cancel := tmux.TimeoutContext(ctx)
	defer cancel()
	identityLock := a.sessionMutex(id)
	identityLock.Lock()
	current, currentOK := a.Store.FindSession(id)
	if !currentOK || current.State != state.TerminalRunning || !current.WatchEnabled || current.AnchorMessageID == 0 {
		identityLock.Unlock()
		return
	}
	if err := a.validateSessionPane(tctx, current); err != nil {
		identityLock.Unlock()
		return
	}
	current, currentOK = a.Store.FindSession(id)
	identityLock.Unlock()
	if !currentOK {
		return
	}
	if !acquireSlot(tctx, a.captureSlots) {
		return
	}
	capture, captureErr := a.Tmux.CaptureStyled(tctx, current.TmuxPaneID, terminalshot.TargetRows)
	releaseSlot(a.captureSlots)
	if captureErr != nil {
		_ = a.audit("tmux.snapshot_anchor", "capture_failed", map[string]any{"session_id": id, "pane_id": current.TmuxPaneID, "error": captureErr.Error()})
		return
	}
	presentationText := a.processCapturedFrame(ctx, current, capture)
	presentationCapture := capture
	presentationCapture.Text = presentationText
	captureHash := snapshotAnchorHash(current, capture, a.Config.SnapshotTheme)
	if !a.snapshotAnchors() {
		return
	}
	if captureHash == current.LastSnapshotCaptureHash && current.AnchorFormat == "snapshot" && !manual {
		return
	}
	if !acquireSlot(ctx, a.renderSlots) {
		return
	}
	renderCtx, renderCancel := context.WithTimeout(ctx, snapshotRenderTimeout)
	pngPath, renderErr := a.Snapshots.Render(renderCtx, terminalshot.Input{
		ANSI:        capture.ANSI,
		Title:       firstNonEmpty(current.Title, capture.Title),
		Target:      fmt.Sprintf("[%d]", current.ID),
		CWD:         capture.CurrentPath,
		Columns:     capture.Columns,
		VisibleRows: capture.VisibleRows,
		BufferRows:  capture.BufferRows,
	}, a.Config.ArtifactDir())
	renderCancel()
	releaseSlot(a.renderSlots)
	if renderErr != nil {
		_ = a.audit("terminal.snapshot_anchor", "render_failed", map[string]any{"session_id": id, "error": renderErr.Error()})
		return
	}
	defer os.Remove(pngPath)

	anchorLock := a.anchorMutex(id)
	anchorLock.Lock()
	defer anchorLock.Unlock()
	a.finishAnchorRotationLocked(ctx, id)
	latest, ok := a.Store.FindSession(id)
	if !a.snapshotAnchors() || !ok || latest.State != state.TerminalRunning || !latest.WatchEnabled || latest.AnchorMessageID == 0 || latest.RetiringAnchorMessageID != 0 || latest.TmuxPaneID != current.TmuxPaneID || latest.TmuxWindowID != current.TmuxWindowID {
		return
	}
	caption := a.snapshotAnchorCaption(latest, presentationCapture)
	markup := a.anchorMarkup(latest)
	_, editErr := a.Telegram.EditPhoto(ctx, latest.AnchorChatID, latest.AnchorMessageID, pngPath, caption, markup)
	if telegram.IsMessageNotModified(editErr) {
		editErr = nil
	}
	if editErr != nil {
		if telegram.IsRateLimited(editErr) || !isTelegramAnchorUnavailable(editErr) {
			_ = a.audit("telegram.snapshot_anchor", "edit_failed", map[string]any{"session_id": id, "error": editErr.Error()})
			return
		}
		msg, sendErr := a.Telegram.SendPhotoWithMarkup(ctx, a.Config.TelegramChatID, pngPath, caption, 0, markup)
		if sendErr != nil {
			_ = a.audit("telegram.snapshot_anchor", "replacement_failed", map[string]any{"session_id": id, "error": sendErr.Error()})
			return
		}
		oldID := latest.AnchorMessageID
		oldFormat := firstNonEmpty(latest.AnchorFormat, "text")
		replaced := false
		updated, _, stateErr := a.Store.UpdateSession(id, func(session *state.TerminalSession) {
			if session.AnchorMessageID == oldID && session.RetiringAnchorMessageID == 0 && session.State == state.TerminalRunning && session.WatchEnabled {
				session.AnchorChatID = msg.Chat.ID
				session.AnchorMessageID = msg.MessageID
				session.AnchorFormat = "snapshot"
				session.RetiringAnchorMessageID = oldID
				session.RetiringAnchorFormat = oldFormat
				session.AnchorPinned = false
				session.AnchorPinKnown = false
				session.LastSnapshotCaptureHash = captureHash
				session.LastRenderHash = sha(captureHash + "\x00" + caption)
				session.LastKnownCWD = capture.CurrentPath
				session.LastAnchorEditAt = time.Now().UTC()
				replaced = true
			}
		})
		if stateErr != nil || !replaced {
			a.deactivateProspectiveSnapshotAnchor(ctx, msg.Chat.ID, msg.MessageID)
			_ = a.audit("state.snapshot_anchor", "replacement_failed", map[string]any{"session_id": id, "error": firstNonEmpty(errorText(stateErr), "superseded")})
			return
		}
		if anchorShouldBePinned(updated) {
			a.ensureCurrentAnchorPinnedLocked(ctx, updated)
		}
		a.finishAnchorRotationLocked(ctx, id)
		return
	}

	if _, _, err := a.Store.UpdateSession(id, func(session *state.TerminalSession) {
		if session.AnchorMessageID == latest.AnchorMessageID {
			session.AnchorFormat = "snapshot"
			session.LastSnapshotCaptureHash = captureHash
			session.LastRenderHash = sha(captureHash + "\x00" + caption)
			session.LastKnownCWD = capture.CurrentPath
			session.LastAnchorEditAt = time.Now().UTC()
		}
	}); err != nil {
		_ = a.audit("state.snapshot_anchor", "failed", map[string]any{"session_id": id, "error": err.Error()})
		return
	}
	_ = a.audit("terminal.snapshot_anchor", "updated", map[string]any{"session_id": id, "columns": capture.Columns, "visible_rows": capture.VisibleRows, "buffer_rows": capture.BufferRows})
}

func (a *App) finishSnapshotMigration(ctx context.Context, id int) bool {
	lock := a.anchorMutex(id)
	lock.Lock()
	defer lock.Unlock()
	a.finishAnchorRotationLocked(ctx, id)
	ts, ok := a.Store.FindSession(id)
	return ok && ts.RetiringAnchorMessageID == 0
}

func snapshotAnchorHash(ts state.TerminalSession, capture tmux.StyledCapture, theme string) string {
	return sha(strings.Join([]string{capture.ANSI, capture.Title, capture.CurrentPath, fmt.Sprint(capture.Columns), fmt.Sprint(capture.VisibleRows), fmt.Sprint(capture.BufferRows), ts.Title, theme}, "\x00"))
}

func (a *App) snapshotAnchorCaption(ts state.TerminalSession, capture tmux.StyledCapture) string {
	const safeCaptionBytes = 960
	title := a.redactText(firstNonEmpty(ts.Title, capture.Title, "terminal"))
	cwd := a.redactText(capture.CurrentPath)
	caption := fmt.Sprintf("[%d] %s · %s\n%s\n%d buffer rows · %dx%d visible", ts.ID, ts.State, title, cwd, capture.BufferRows, capture.Columns, capture.VisibleRows)
	if references := renderSnapshotReferences(capture.Text, safeCaptionBytes-len(caption)-2); references != "" {
		caption += "\n\n" + a.redactText(references)
	}
	return caption
}

func (a *App) updateSnapshotAnchorCaptionLocked(ctx context.Context, ts state.TerminalSession, summary string, final bool) {
	caption := fmt.Sprintf("[%d] %s · %s\n%s", ts.ID, ts.State, a.redactText(firstNonEmpty(ts.Title, "terminal")), a.redactText(summary))
	renderHash := sha(caption)
	if renderHash == ts.LastRenderHash && !final {
		return
	}
	if !final && time.Since(ts.LastAnchorEditAt) < 10*time.Second {
		return
	}
	markup := a.anchorMarkup(ts)
	if ts.State == state.TerminalClosed {
		markup = telegram.ClearMarkup()
	}
	_, err := a.Telegram.EditCaption(ctx, ts.AnchorChatID, ts.AnchorMessageID, caption, markup)
	if telegram.IsMessageNotModified(err) {
		err = nil
	}
	if err != nil {
		_ = a.audit("telegram.snapshot_caption", "failed", map[string]any{"session_id": ts.ID, "error": err.Error()})
		return
	}
	if _, _, err := a.Store.UpdateSession(ts.ID, func(session *state.TerminalSession) {
		session.LastRenderHash = renderHash
		session.LastAnchorEditAt = time.Now().UTC()
		if session.State == state.TerminalClosed || session.State == state.TerminalLost {
			session.LastSnapshotCaptureHash = ""
		}
	}); err != nil {
		_ = a.audit("state.snapshot_caption", "failed", map[string]any{"session_id": ts.ID, "error": err.Error()})
	}
}

func (a *App) rotateSnapshotAnchorToTextLocked(ctx context.Context, ts state.TerminalSession, rendered, renderHash string) {
	oldID := ts.AnchorMessageID
	msg, err := a.sendAnchor(ctx, ts.AnchorChatID, rendered, oldID, a.anchorMarkup(ts))
	if err != nil {
		_ = a.audit("telegram.anchor_mode", "guide_send_failed", map[string]any{"session_id": ts.ID, "error": err.Error()})
		return
	}
	rotated := false
	updated, _, stateErr := a.Store.UpdateSession(ts.ID, func(session *state.TerminalSession) {
		if session.AnchorMessageID == oldID && session.AnchorFormat == "snapshot" && session.RetiringAnchorMessageID == 0 {
			session.AnchorMessageID = msg.MessageID
			session.AnchorFormat = "text"
			session.RetiringAnchorMessageID = oldID
			session.RetiringAnchorFormat = "snapshot"
			session.AnchorPinned = false
			session.AnchorPinKnown = false
			session.LastRenderHash = renderHash
			session.LastAnchorEditAt = time.Now().UTC()
			rotated = true
		}
	})
	if stateErr != nil || !rotated {
		a.deactivateProspectiveAnchor(ctx, ts.AnchorChatID, msg.MessageID, retiredAnchorText(ts))
		_ = a.audit("state.anchor_mode", "guide_rotation_failed", map[string]any{"session_id": ts.ID, "error": firstNonEmpty(errorText(stateErr), "superseded")})
		return
	}
	if anchorShouldBePinned(updated) {
		a.ensureCurrentAnchorPinnedLocked(ctx, updated)
	}
	a.finishAnchorRotationLocked(ctx, ts.ID)
}

func (a *App) deactivateProspectiveSnapshotAnchor(ctx context.Context, chatID int64, messageID int) {
	_, _ = a.Telegram.EditCaption(ctx, chatID, messageID, "inactive snapshot anchor", telegram.ClearMarkup())
	_ = a.Telegram.UnpinChatMessage(ctx, chatID, messageID)
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
