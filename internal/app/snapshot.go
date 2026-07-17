package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/terminalshot"
	"github.com/idolum-ai/engram/internal/tmux"
)

const snapshotRenderTimeout = 30 * time.Second
const snapshotNoticeTimeout = 15 * time.Second

type snapshotRenderer interface {
	Available() (string, error)
	Render(context.Context, terminalshot.Input, string) (string, error)
}

func (a *App) snapshotStatus() string {
	if a.Snapshots == nil || !a.snapshotReady {
		return "unavailable"
	}
	path, err := a.Snapshots.Available()
	if err != nil {
		return "unavailable"
	}
	return "ready (" + filepath.Base(path) + ", " + a.Config.SnapshotTheme + ")"
}

func (a *App) queueSnapshot(ts state.TerminalSession) actionResult {
	if a.Snapshots == nil {
		return actionResult{Outcome: actionStateFailed, Message: "image renderer unavailable"}
	}
	if _, err := a.Snapshots.Available(); err != nil {
		_ = a.audit("terminal.snapshot", "unavailable", map[string]any{"session_id": ts.ID, "error": err.Error()})
		return actionResult{Outcome: actionStateFailed, Message: "image renderer unavailable; check /status"}
	}
	if !a.queueTransfer(func(ctx context.Context) {
		a.sendSnapshot(ctx, ts)
	}) {
		return actionResult{Outcome: actionStateFailed, Message: "image queue is full"}
	}
	_ = a.audit("terminal.snapshot", "queued", map[string]any{"session_id": ts.ID, "pane_id": ts.TmuxPaneID})
	return actionResult{Outcome: actionOK, Message: "printing window"}
}

func (a *App) sendSnapshot(ctx context.Context, requested state.TerminalSession) {
	lock := a.sessionMutex(requested.ID)
	lock.Lock()
	current, ok := a.Store.FindSession(requested.ID)
	if !ok || current.State == state.TerminalClosed || current.State == state.TerminalLost || !sameTerminalBinding(current, requested) {
		lock.Unlock()
		a.snapshotNotice(ctx, requested.ID, "Could not print the window because the session moved or closed.")
		return
	}
	tctx, cancel := tmux.TimeoutContext(ctx)
	if !acquireSlot(tctx, a.captureSlots) {
		cancel()
		lock.Unlock()
		a.snapshotNotice(ctx, requested.ID, "Could not capture the tmux window before the request timed out.")
		return
	}
	capture, captureErr := a.captureStyled(tctx, current, terminalshot.TargetRows)
	releaseSlot(a.captureSlots)
	cancel()
	lock.Unlock()
	if captureErr != nil {
		_ = a.audit("terminal.snapshot", "capture_failed", map[string]any{"session_id": current.ID, "pane_id": current.TmuxPaneID, "error": captureErr.Error()})
		a.snapshotNotice(ctx, current.ID, "Could not capture the tmux window.")
		return
	}
	a.processCapturedFrame(ctx, current, capture)

	if !acquireSlot(ctx, a.renderSlots) {
		a.snapshotNotice(ctx, current.ID, "Could not render the terminal image before the request timed out.")
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
		_ = a.audit("terminal.snapshot", "render_failed", map[string]any{"session_id": current.ID, "error": renderErr.Error()})
		a.snapshotNotice(ctx, current.ID, "Could not render the terminal image; check /status and /logs.")
		return
	}
	defer os.Remove(pngPath)

	a.presentationMu.RLock()
	defer a.presentationMu.RUnlock()
	anchorLock := a.anchorMutex(current.ID)
	anchorLock.Lock()
	defer anchorLock.Unlock()
	a.finishAnchorRotationLocked(ctx, current.ID)
	latest, ok := a.Store.FindSession(current.ID)
	if a.snapshotAnchors() || !ok || latest.State != state.TerminalRunning || !sameTerminalBinding(latest, current) || latest.AnchorMessageID == 0 || latest.RetiringAnchorMessageID != 0 {
		_ = a.audit("terminal.snapshot", "superseded", map[string]any{"session_id": current.ID})
		return
	}
	caption := fmt.Sprintf("[%d] %s\n%s", latest.ID, a.redactText(firstNonEmpty(latest.Title, "terminal")), snapshotFrameDescription(capture, false))
	message, err := a.Telegram.SendPhoto(ctx, latest.AnchorChatID, pngPath, caption, latest.AnchorMessageID)
	if err != nil {
		_ = a.audit("telegram.snapshot", "failed", map[string]any{"session_id": latest.ID, "error": err.Error()})
		a.snapshotNotice(ctx, latest.ID, "Rendered the terminal image, but Telegram could not receive it.")
		return
	}
	updated := false
	if _, _, err := a.Store.UpdateSession(latest.ID, func(session *state.TerminalSession) {
		if !a.snapshotAnchors() && session.State == state.TerminalRunning && sameTerminalBinding(*session, latest) && session.AnchorMessageID == latest.AnchorMessageID && guideAnchorFormat(session.AnchorFormat) && session.RetiringAnchorMessageID == 0 {
			recordAlternateMessage(session, "snapshot", message.MessageID)
			updated = true
		}
	}); err != nil {
		if state.PersistenceReachedReplacement(err) && updated {
			_ = a.audit("state.snapshot", "durability_uncertain", map[string]any{"session_id": latest.ID, "message_id": message.MessageID, "error": err.Error()})
			return
		}
		_ = a.audit("state.snapshot", "failed", map[string]any{"session_id": latest.ID, "error": err.Error()})
		deleteCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), snapshotNoticeTimeout)
		_ = a.Telegram.DeleteMessage(deleteCtx, latest.AnchorChatID, message.MessageID)
		cancel()
		return
	}
	if !updated {
		deleteCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), snapshotNoticeTimeout)
		_ = a.Telegram.DeleteMessage(deleteCtx, latest.AnchorChatID, message.MessageID)
		cancel()
		_ = a.audit("terminal.snapshot", "superseded", map[string]any{"session_id": latest.ID})
		return
	}
	_ = a.audit("terminal.snapshot", "sent", map[string]any{"session_id": latest.ID, "columns": capture.Columns, "visible_rows": capture.VisibleRows, "buffer_rows": capture.BufferRows})
}

func snapshotFrameDescription(capture tmux.StyledCapture, _ bool) string {
	description := fmt.Sprintf("%d buffer rows · %dx%d visible", capture.BufferRows, capture.Columns, capture.VisibleRows)
	if rendered := terminalshot.RenderedColumns(capture.Columns); rendered < capture.Columns {
		description += fmt.Sprintf("\nfull-width image · rows wrap at %d columns", rendered)
	}
	return description
}

func (a *App) deleteSnapshotReply(ctx context.Context, chatID int64, messageID int, reason string) {
	deleteCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), snapshotNoticeTimeout)
	err := a.Telegram.DeleteMessage(deleteCtx, chatID, messageID)
	cancel()
	status := "ok"
	data := map[string]any{"message_id": messageID, "reason": reason}
	if err != nil && !isTelegramAnchorUnavailable(err) {
		status = "failed"
		data["error"] = err.Error()
	}
	_ = a.audit("telegram.snapshot_cleanup", status, data)
}

func (a *App) snapshotNotice(ctx context.Context, id int, text string) {
	ts, ok := a.Store.FindSession(id)
	if !ok || ts.AnchorChatID == 0 || ts.AnchorMessageID == 0 || a.Telegram == nil {
		return
	}
	noticeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), snapshotNoticeTimeout)
	defer cancel()
	if _, err := a.Telegram.SendMessage(noticeCtx, ts.AnchorChatID, text, ts.AnchorMessageID, nil); err != nil {
		_ = a.audit("telegram.snapshot_notice", "failed", map[string]any{"session_id": id, "error": err.Error()})
	}
}
