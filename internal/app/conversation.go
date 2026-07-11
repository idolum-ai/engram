package app

import (
	"context"
	"time"

	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/terminalshot"
	"github.com/idolum-ai/engram/internal/tmux"
)

const conversationTimeout = 75 * time.Second
const conversationNoticeTimeout = 15 * time.Second

func (a *App) queueConversation(ts state.TerminalSession) actionResult {
	if !a.haikuAvailable || a.Anthropic == nil {
		return actionResult{Outcome: actionStateFailed, Message: "Haiku is unavailable"}
	}
	if !a.queueTransfer(func(ctx context.Context) {
		conversationCtx, cancel := context.WithTimeout(ctx, conversationTimeout)
		defer cancel()
		a.sendConversation(conversationCtx, ts)
	}) {
		return actionResult{Outcome: actionStateFailed, Message: "voice queue is full"}
	}
	_ = a.audit("terminal.conversation", "queued", map[string]any{"session_id": ts.ID})
	return actionResult{Outcome: actionOK, Message: "reading window"}
}

func (a *App) sendConversation(ctx context.Context, requested state.TerminalSession) {
	lock := a.sessionMutex(requested.ID)
	lock.Lock()
	current, ok := a.Store.FindSession(requested.ID)
	if !ok || current.State != state.TerminalRunning || current.TmuxPaneID != requested.TmuxPaneID {
		lock.Unlock()
		a.conversationNotice(ctx, requested, "I couldn't read that window because the session moved or closed.")
		return
	}
	tctx, cancel := tmux.TimeoutContext(ctx)
	if err := a.validateSessionPane(tctx, current); err != nil {
		cancel()
		lock.Unlock()
		a.conversationNotice(ctx, requested, "I couldn't read that window because its tmux pane is unavailable.")
		return
	}
	if !acquireSlot(tctx, a.captureSlots) {
		cancel()
		lock.Unlock()
		a.conversationNotice(ctx, requested, "I couldn't read that window before the request timed out.")
		return
	}
	capture, err := a.Tmux.CaptureStyled(tctx, current.TmuxPaneID, terminalshot.TargetRows)
	releaseSlot(a.captureSlots)
	observation := observeUpstreamSignal(capture)
	cancel()
	lock.Unlock()
	if err != nil {
		_ = a.audit("terminal.conversation", "capture_failed", map[string]any{"session_id": current.ID, "error": err.Error()})
		a.conversationNotice(ctx, requested, "I couldn't capture that terminal window. Please try again.")
		return
	}
	if observation.Found {
		a.deliverUpstreamSignal(ctx, current, observation.Latest)
	}
	summary, err := a.conversationalSummary(ctx, current.ID, observation.PresentationText)
	if err != nil {
		_ = a.Store.NoteHaiku(err.Error())
		_ = a.audit("terminal.conversation", "haiku_failed", map[string]any{"session_id": current.ID, "error": err.Error()})
		a.conversationNotice(ctx, requested, "I couldn't finish reading that window. Please try again.")
		return
	}
	_ = a.Store.NoteHaiku("")
	anchorLock := a.anchorMutex(current.ID)
	anchorLock.Lock()
	defer anchorLock.Unlock()
	a.finishAnchorRotationLocked(ctx, current.ID)
	latest, ok := a.Store.FindSession(current.ID)
	if !a.snapshotAnchors() || !ok || latest.State != state.TerminalRunning || latest.TmuxPaneID != current.TmuxPaneID || latest.AnchorMessageID == 0 || latest.AnchorMessageID != requested.AnchorMessageID || latest.AnchorFormat != "snapshot" || latest.RetiringAnchorMessageID != 0 {
		_ = a.audit("terminal.conversation", "superseded", map[string]any{"session_id": current.ID})
		a.conversationNotice(ctx, requested, "That window changed while I was reading it, so I left the newer view in place.")
		return
	}
	message, err := a.sendAnchor(ctx, latest.AnchorChatID, summary, latest.AnchorMessageID, nil)
	if err != nil {
		_ = a.audit("telegram.conversation", "failed", map[string]any{"session_id": latest.ID, "error": err.Error()})
		a.conversationNotice(ctx, requested, "I read the window, but couldn't deliver the result. Please try again.")
		return
	}
	if _, _, err := a.Store.UpdateSession(latest.ID, func(session *state.TerminalSession) {
		if session.AnchorMessageID == latest.AnchorMessageID {
			recordAlternateMessage(session, "summary", message.MessageID)
		}
	}); err != nil {
		_ = a.audit("state.conversation", "failed", map[string]any{"session_id": latest.ID, "error": err.Error()})
		return
	}
	_ = a.audit("terminal.conversation", "sent", map[string]any{"session_id": latest.ID})
}

func (a *App) conversationNotice(ctx context.Context, requested state.TerminalSession, text string) {
	if a.Telegram == nil {
		return
	}
	target := requested
	if latest, ok := a.Store.FindSession(requested.ID); ok && latest.AnchorChatID != 0 && latest.AnchorMessageID != 0 {
		target = latest
	}
	if target.AnchorChatID == 0 || target.AnchorMessageID == 0 {
		return
	}
	noticeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), conversationNoticeTimeout)
	defer cancel()
	if _, err := a.Telegram.SendMessage(noticeCtx, target.AnchorChatID, text, target.AnchorMessageID, nil); err != nil {
		_ = a.audit("telegram.conversation_notice", "failed", map[string]any{"session_id": requested.ID, "error": err.Error()})
	}
}
