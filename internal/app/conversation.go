package app

import (
	"context"
	"errors"
	"time"

	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/terminalshot"
	"github.com/idolum-ai/engram/internal/tmux"
)

const conversationTimeout = 75 * time.Second
const conversationNoticeTimeout = 15 * time.Second

func (a *App) queueConversation(ts state.TerminalSession) actionResult {
	if !a.guideAvailable || a.Guide == nil {
		return actionResult{Outcome: actionStateFailed, Message: "conversational guide is unavailable"}
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
	releaseConversation, acquired := a.acquireConversation(ctx, requested.ID)
	if !acquired {
		a.conversationNotice(ctx, requested, "I couldn't read that window before the request timed out.")
		return
	}
	defer releaseConversation()
	lock := a.sessionMutex(requested.ID)
	lock.Lock()
	current, ok := a.Store.FindSession(requested.ID)
	if !ok || current.Collapsed || current.State != state.TerminalRunning || !sameTerminalBinding(current, requested) {
		lock.Unlock()
		a.conversationNotice(ctx, requested, "I couldn't read that window because the session moved or closed.")
		return
	}
	tctx, cancel := tmux.TimeoutContext(ctx)
	tctx = tmux.BackgroundContext(tctx)
	if !acquireSlot(tctx, a.captureSlots) {
		cancel()
		lock.Unlock()
		a.conversationNotice(ctx, requested, "I couldn't read that window before the request timed out.")
		return
	}
	capture, err := a.captureStyled(tctx, current, terminalshot.TargetRows)
	releaseSlot(a.captureSlots)
	cancel()
	lock.Unlock()
	if err != nil {
		_ = a.audit("terminal.conversation", "capture_failed", map[string]any{"session_id": current.ID, "error": err.Error()})
		a.conversationNotice(ctx, requested, "I couldn't capture that terminal window. Please try again.")
		return
	}
	presentationText := a.processCapturedFrame(ctx, current, capture)
	summary, err := a.snapshotConversationalSummary(ctx, current, requested.AnchorMessageID, presentationText)
	if err != nil {
		if errors.Is(err, errConversationTurnSuperseded) {
			_ = a.audit("terminal.conversation", "superseded", map[string]any{"session_id": current.ID})
			a.conversationNotice(ctx, requested, "That window changed while I was reading it, so I left the newer view in place.")
			return
		}
		_ = a.Store.NoteGuide(err.Error())
		_ = a.audit("terminal.conversation", "guide_failed", map[string]any{"session_id": current.ID, "error": err.Error()})
		a.conversationNotice(ctx, requested, "I couldn't finish reading that window. Please try again.")
		return
	}
	_ = a.Store.NoteGuide("")
	a.presentationMu.RLock()
	defer a.presentationMu.RUnlock()
	anchorLock := a.anchorMutex(current.ID)
	anchorLock.Lock()
	defer anchorLock.Unlock()
	a.finishAnchorRotationLocked(ctx, current.ID)
	latest, ok := a.Store.FindSession(current.ID)
	if !a.snapshotAnchors() || !ok || latest.Collapsed || latest.State != state.TerminalRunning || !sameTerminalBinding(latest, current) || latest.AnchorMessageID == 0 || latest.AnchorMessageID != requested.AnchorMessageID || latest.AnchorFormat != "snapshot" || latest.RetiringAnchorMessageID != 0 {
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
	updated := false
	if _, _, err := a.Store.UpdateSession(latest.ID, func(session *state.TerminalSession) {
		if a.snapshotAnchors() && !session.Collapsed && session.State == state.TerminalRunning && sameTerminalBinding(*session, latest) && session.AnchorMessageID == latest.AnchorMessageID && session.AnchorFormat == "snapshot" && session.RetiringAnchorMessageID == 0 {
			recordAlternateMessage(session, "summary", message.MessageID)
			updated = true
		}
	}); err != nil {
		if state.PersistenceReachedReplacement(err) && updated {
			_ = a.audit("state.conversation", "durability_uncertain", map[string]any{"session_id": latest.ID, "message_id": message.MessageID, "error": err.Error()})
			return
		}
		_ = a.audit("state.conversation", "failed", map[string]any{"session_id": latest.ID, "error": err.Error()})
		deleteCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), conversationNoticeTimeout)
		_ = a.Telegram.DeleteMessage(deleteCtx, latest.AnchorChatID, message.MessageID)
		cancel()
		return
	}
	if !updated {
		deleteCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), conversationNoticeTimeout)
		_ = a.Telegram.DeleteMessage(deleteCtx, latest.AnchorChatID, message.MessageID)
		cancel()
		_ = a.audit("terminal.conversation", "superseded", map[string]any{"session_id": latest.ID})
		return
	}
	_ = a.audit("terminal.conversation", "sent", map[string]any{"session_id": latest.ID})
}

func (a *App) conversationNotice(ctx context.Context, requested state.TerminalSession, text string) {
	if a.Telegram == nil {
		return
	}
	target := requested
	if latest, ok := a.Store.FindSession(requested.ID); ok {
		if latest.Collapsed {
			return
		}
		if latest.AnchorChatID != 0 && latest.AnchorMessageID != 0 {
			target = latest
		}
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
