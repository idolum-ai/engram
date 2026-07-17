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

type snapshotTextFrame struct {
	ChatID     int64
	MessageID  int
	ServerID   string
	WindowID   string
	PaneID     string
	FrameHash  string
	JoinedText string
}

func (a *App) rememberSnapshotTextFrame(ts state.TerminalSession, capture tmux.StyledCapture) {
	a.snapshotTextFrames.Store(ts.ID, snapshotTextFrame{
		ChatID: ts.AnchorChatID, MessageID: ts.AnchorMessageID,
		ServerID: ts.TmuxServerID, WindowID: ts.TmuxWindowID, PaneID: ts.TmuxPaneID,
		FrameHash:  sha(capture.ANSI + "\x00" + capture.JoinedText),
		JoinedText: capture.JoinedText,
	})
}

func (a *App) snapshotTextFrame(ts state.TerminalSession) (snapshotTextFrame, bool) {
	value, ok := a.snapshotTextFrames.Load(ts.ID)
	if !ok {
		return snapshotTextFrame{}, false
	}
	frame, ok := value.(snapshotTextFrame)
	return frame, ok && frame.ChatID == ts.AnchorChatID && frame.MessageID == ts.AnchorMessageID &&
		frame.ServerID == ts.TmuxServerID && frame.WindowID == ts.TmuxWindowID && frame.PaneID == ts.TmuxPaneID
}

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
	capture, captureErr := a.captureStyled(tctx, current, terminalshot.TargetRows)
	releaseSlot(a.captureSlots)
	if captureErr != nil {
		_ = a.audit("tmux.snapshot_anchor", "capture_failed", map[string]any{"session_id": id, "pane_id": current.TmuxPaneID, "error": captureErr.Error()})
		return
	}
	presentationText := a.processCapturedFrame(ctx, current, capture)
	refs := a.visibleReferences(presentationText)
	caption, files := a.snapshotAnchorCaption(current, capture, refs)
	captureHash := snapshotAnchorHash(current, capture, presentationText, caption, a.Config.SnapshotTheme)
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

	a.presentationMu.RLock()
	defer a.presentationMu.RUnlock()
	disclosureLock := a.disclosureMutex(id)
	disclosureLock.Lock()
	defer disclosureLock.Unlock()
	anchorLock := a.anchorMutex(id)
	anchorLock.Lock()
	defer anchorLock.Unlock()
	a.finishAnchorRotationLocked(ctx, id)
	latest, ok := a.Store.FindSession(id)
	if !a.snapshotAnchors() || !ok || latest.State != state.TerminalRunning || !latest.WatchEnabled || latest.AnchorMessageID == 0 || latest.RetiringAnchorMessageID != 0 || !sameTerminalBinding(latest, current) {
		return
	}
	// The image was rendered with current.Title. A concurrent rename must retry
	// the whole render instead of persisting a hash for mismatched media.
	if latest.Title != current.Title {
		return
	}
	caption, files = a.snapshotAnchorCaption(latest, capture, refs)
	captureHash = snapshotAnchorHash(latest, capture, presentationText, caption, a.Config.SnapshotTheme)
	presented := bindAnchorFiles(latest, files)
	presented.AnchorFormat = "snapshot"
	markup := a.anchorMarkup(presented)
	_, editErr := a.Telegram.EditHTMLPhoto(ctx, latest.AnchorChatID, latest.AnchorMessageID, pngPath, telegram.MarkdownToHTML(caption), markup)
	if telegram.IsMessageNotModified(editErr) {
		editErr = nil
	}
	if editErr != nil {
		if telegram.IsRateLimited(editErr) || !isTelegramAnchorUnavailable(editErr) {
			_ = a.audit("telegram.snapshot_anchor", "edit_failed", map[string]any{"session_id": id, "error": editErr.Error()})
			return
		}
		msg, sendErr := a.Telegram.SendHTMLPhotoWithMarkup(ctx, a.Config.TelegramChatID, pngPath, telegram.MarkdownToHTML(caption), 0, markup)
		if sendErr != nil {
			_ = a.audit("telegram.snapshot_anchor", "replacement_failed", map[string]any{"session_id": id, "error": sendErr.Error()})
			return
		}
		oldID := latest.AnchorMessageID
		oldFormat := firstNonEmpty(latest.AnchorFormat, "text")
		replaced := false
		updated, _, stateErr := a.Store.UpdateSession(id, func(session *state.TerminalSession) {
			if a.snapshotAnchors() && session.AnchorMessageID == oldID && session.RetiringAnchorMessageID == 0 && session.State == state.TerminalRunning && session.WatchEnabled && sameTerminalBinding(*session, latest) {
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
				setAnchorFiles(session, files)
				replaced = true
			}
		})
		committed := replaced && (stateErr == nil || state.PersistenceReachedReplacement(stateErr))
		if !committed {
			a.deactivateProspectiveMediaAnchor(ctx, msg.Chat.ID, msg.MessageID)
			_ = a.audit("state.snapshot_anchor", "replacement_failed", map[string]any{"session_id": id, "error": firstNonEmpty(errorText(stateErr), "superseded")})
			return
		}
		if stateErr != nil {
			_ = a.audit("state.snapshot_anchor", "durability_uncertain", map[string]any{"session_id": id, "error": stateErr.Error()})
		}
		if anchorShouldBePinned(updated) {
			a.ensureCurrentAnchorPinnedLocked(ctx, updated)
		}
		a.finishAnchorRotationLocked(ctx, id)
		if currentAnchor, found := a.Store.FindSession(id); found {
			a.rememberSnapshotTextFrame(currentAnchor, capture)
		}
		return
	}
	// Telegram has published this exact image. Advance its process-local text
	// companion before persistence so a state-write failure cannot pair the new
	// image with the previous frame.
	a.rememberSnapshotTextFrame(latest, capture)

	updated := false
	stored, _, err := a.Store.UpdateSession(id, func(session *state.TerminalSession) {
		if a.snapshotAnchors() && session.AnchorMessageID == latest.AnchorMessageID && session.State == state.TerminalRunning && session.WatchEnabled && sameTerminalBinding(*session, latest) {
			session.AnchorFormat = "snapshot"
			session.LastSnapshotCaptureHash = captureHash
			session.LastRenderHash = sha(captureHash + "\x00" + caption)
			session.LastKnownCWD = capture.CurrentPath
			session.LastAnchorEditAt = time.Now().UTC()
			setAnchorFiles(session, files)
			updated = true
		}
	})
	if err != nil {
		_ = a.audit("state.snapshot_anchor", "failed", map[string]any{"session_id": id, "error": err.Error()})
		return
	}
	if !updated {
		_ = a.audit("terminal.snapshot_anchor", "superseded", map[string]any{"session_id": id})
		return
	}
	a.rememberSnapshotTextFrame(stored, capture)
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

func snapshotAnchorHash(ts state.TerminalSession, capture tmux.StyledCapture, presentationText, caption, theme string) string {
	return sha(strings.Join([]string{capture.ANSI, presentationText, capture.Title, capture.CurrentPath, fmt.Sprint(capture.Columns), fmt.Sprint(capture.VisibleRows), fmt.Sprint(capture.BufferRows), ts.Title, caption, theme}, "\x00"))
}

func (a *App) snapshotAnchorCaption(ts state.TerminalSession, capture tmux.StyledCapture, refs visibleReferences) (string, []string) {
	const safeCaptionBytes = 960
	title := strings.Join(strings.Fields(a.redactText(firstNonEmpty(ts.Title, capture.Title, "terminal"))), " ")
	cwd := strings.Join(strings.Fields(a.redactText(capture.CurrentPath)), " ")
	caption := fmt.Sprintf("[%d] %s · %s\n%s\n%d buffer rows · %dx%d visible", ts.ID, ts.State, title, cwd, capture.BufferRows, capture.Columns, capture.VisibleRows)
	if len(caption) > safeCaptionBytes {
		return headUTF8(caption, safeCaptionBytes), nil
	}
	if references, files := renderSnapshotReferenceSetWithFiles(refs, safeCaptionBytes-len(caption)-2); references != "" {
		caption += "\n\n" + references
		return caption, files
	}
	return caption, nil
}

func (a *App) updateMediaAnchorCaptionLocked(ctx context.Context, ts state.TerminalSession, summary string, final bool) {
	caption := fmt.Sprintf("[%d] %s · %s\n%s", ts.ID, ts.State, a.redactText(firstNonEmpty(ts.Title, "terminal")), a.redactText(summary))
	renderHash := sha(caption)
	if renderHash == ts.LastRenderHash && !final {
		return
	}
	if !final && time.Since(ts.LastAnchorEditAt) < 10*time.Second {
		return
	}
	presented := bindAnchorFiles(ts, nil)
	markup := a.anchorMarkup(presented)
	if ts.State == state.TerminalClosed {
		markup = telegram.ClearMarkup()
	}
	_, err := a.Telegram.EditCaption(ctx, ts.AnchorChatID, ts.AnchorMessageID, caption, markup)
	if telegram.IsMessageNotModified(err) {
		err = nil
	}
	if err != nil {
		_ = a.audit("telegram.anchor_caption", "failed", map[string]any{"session_id": ts.ID, "error": err.Error()})
		return
	}
	if _, _, err := a.Store.UpdateSession(ts.ID, func(session *state.TerminalSession) {
		session.LastRenderHash = renderHash
		session.LastAnchorEditAt = time.Now().UTC()
		setAnchorFiles(session, nil)
		if session.State == state.TerminalClosed || session.State == state.TerminalLost {
			session.LastSnapshotCaptureHash = ""
		}
	}); err != nil {
		_ = a.audit("state.anchor_caption", "failed", map[string]any{"session_id": ts.ID, "error": err.Error()})
	}
}

func (a *App) rotateMediaAnchorToTextLocked(ctx context.Context, ts state.TerminalSession, rendered, renderHash string, guard func() bool) {
	oldID := ts.AnchorMessageID
	presented := ts
	presented.AnchorFormat = "text"
	msg, err := a.sendAnchor(ctx, ts.AnchorChatID, rendered, oldID, a.anchorMarkup(presented))
	if err != nil {
		_ = a.audit("telegram.anchor_mode", "guide_send_failed", map[string]any{"session_id": ts.ID, "error": err.Error()})
		return
	}
	if guard != nil && !guard() {
		a.deactivateProspectiveAnchor(ctx, ts.AnchorChatID, msg.MessageID, retiredAnchorText(ts))
		return
	}
	rotated := false
	updated, _, stateErr := a.Store.UpdateSession(ts.ID, func(session *state.TerminalSession) {
		if session.AnchorMessageID == oldID && mediaAnchorFormat(session.AnchorFormat) && session.RetiringAnchorMessageID == 0 && !a.snapshotAnchors() {
			session.AnchorMessageID = msg.MessageID
			session.AnchorFormat = "text"
			session.RetiringAnchorMessageID = oldID
			session.RetiringAnchorFormat = "snapshot"
			session.AnchorPinned = false
			session.AnchorPinKnown = false
			session.LastRenderHash = renderHash
			session.LastAnchorEditAt = time.Now().UTC()
			setAnchorFiles(session, ts.AnchorFiles)
			rotated = true
		}
	})
	committed := rotated && (stateErr == nil || state.PersistenceReachedReplacement(stateErr))
	if !committed {
		a.deactivateProspectiveAnchor(ctx, ts.AnchorChatID, msg.MessageID, retiredAnchorText(ts))
		_ = a.audit("state.anchor_mode", "guide_rotation_failed", map[string]any{"session_id": ts.ID, "error": firstNonEmpty(errorText(stateErr), "superseded")})
		return
	}
	if stateErr != nil {
		_ = a.audit("state.anchor_mode", "durability_uncertain", map[string]any{"session_id": ts.ID, "error": stateErr.Error()})
	}
	if anchorShouldBePinned(updated) {
		a.ensureCurrentAnchorPinnedLocked(ctx, updated)
	}
	a.finishAnchorRotationLocked(ctx, ts.ID)
}

func (a *App) deactivateProspectiveMediaAnchor(ctx context.Context, chatID int64, messageID int) {
	_, _ = a.Telegram.EditCaption(ctx, chatID, messageID, "inactive snapshot anchor", telegram.ClearMarkup())
	_ = a.Telegram.UnpinChatMessage(ctx, chatID, messageID)
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
