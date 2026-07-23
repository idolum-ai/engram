package app

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/idolum-ai/engram/internal/guide"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
	"github.com/idolum-ai/engram/internal/tmux"
)

const (
	maxCollapsedShelfBytes   = 1800
	maxCollapsedShelfEntries = 12
	maxCollapsedLineBytes    = 120
	maxCollapsedLineWords    = 16
)

func (a *App) collapseAnchor(ctx context.Context, expected state.TerminalSession) actionResult {
	a.collapsedShelfMu.Lock()
	defer a.collapsedShelfMu.Unlock()
	a.presentationMu.RLock()
	defer a.presentationMu.RUnlock()
	disclosureLock := a.disclosureMutex(expected.ID)
	disclosureLock.Lock()
	sessionLock := a.sessionMutex(expected.ID)
	sessionLock.Lock()
	lock := a.anchorMutex(expected.ID)
	lock.Lock()
	sessionGuardsHeld := true
	defer func() {
		if sessionGuardsHeld {
			lock.Unlock()
			sessionLock.Unlock()
			disclosureLock.Unlock()
		}
	}()
	a.finishAnchorRotationLocked(ctx, expected.ID)

	current, ok := a.Store.FindSession(expected.ID)
	if !ok || current.AnchorChatID != expected.AnchorChatID || current.AnchorMessageID != expected.AnchorMessageID || !sameTerminalBinding(current, expected) {
		return actionResult{Outcome: actionUserError, Message: "anchor moved; use the newer live message"}
	}
	if current.State != state.TerminalRunning || !current.WatchEnabled || current.RetiringAnchorMessageID != 0 {
		return actionResult{Outcome: actionUserError, Message: "this card cannot be collapsed right now"}
	}
	if current.Collapsed {
		return actionResult{Outcome: actionOK, Message: "already collapsed"}
	}
	if current.PendingCollapse {
		return actionResult{Outcome: actionOK, Message: "collapse is waiting for the shelf to become ready"}
	}

	shelf := a.Store.Snapshot().CollapsedShelf
	created := false
	if shelf == nil {
		prospective := []state.TerminalSession{current}
		prospective[0].Collapsed = true
		rendered := a.renderCollapsedShelf(prospective)
		message, err := a.sendSilentAnchor(ctx, a.Config.TelegramChatID, rendered, 0, nil)
		if err != nil {
			_ = a.audit("telegram.collapsed_shelf", "send_failed", map[string]any{"session_id": current.ID, "error": err.Error()})
			return actionResult{Outcome: actionTelegramFailed, Message: "could not create the collapsed shelf"}
		}
		shelf = &state.CollapsedShelf{
			ChatID: message.Chat.ID, MessageID: message.MessageID,
		}
		created = true
	}

	_, committed, stateErr := a.Store.BeginCollapseSessionIntoShelf(current.ID, current, *shelf)
	if !committed {
		if created {
			a.retireProspectiveMessage(ctx, shelf.ChatID, shelf.MessageID)
		}
		return actionResult{Outcome: actionStateFailed, Message: "could not persist the collapsed shelf: " + firstNonEmpty(errorText(stateErr), "session changed")}
	}
	if stateErr != nil {
		_ = a.audit("state.collapsed_shelf", "durability_uncertain", map[string]any{"session_id": current.ID, "error": stateErr.Error()})
	}
	lock.Unlock()
	sessionLock.Unlock()
	disclosureLock.Unlock()
	sessionGuardsHeld = false
	a.reconcileCollapsedShelfLocked(ctx)
	current, _ = a.Store.FindSession(expected.ID)
	if !current.Collapsed {
		return actionResult{Outcome: actionOK, Message: "collapse queued; the current card remains live until the shelf is ready"}
	}
	_ = a.audit("anchor.collapse", "ok", map[string]any{"session_id": expected.ID, "shelf_message_id": shelf.MessageID})
	return actionResult{Outcome: actionOK, Message: "moved to collapsed sessions"}
}

func (a *App) expandCollapsedShelf(ctx context.Context, expected state.CollapsedShelf) actionResult {
	a.collapsedShelfMu.Lock()
	defer a.collapsedShelfMu.Unlock()
	a.presentationMu.RLock()
	defer a.presentationMu.RUnlock()

	snapshot := a.Store.Snapshot()
	if snapshot.CollapsedShelf == nil || snapshot.CollapsedShelf.ChatID != expected.ChatID || snapshot.CollapsedShelf.MessageID != expected.MessageID {
		return actionResult{Outcome: actionUserError, Message: "collapsed shelf moved; use the newer message"}
	}
	members := collapsedShelfSessions(snapshot.TerminalSessions)
	if len(members) == 0 {
		a.reconcileCollapsedShelfLocked(ctx)
		return actionResult{Outcome: actionOK, Message: "nothing is collapsed"}
	}

	restored := make([]int, 0, len(members))
	// Telegram presents the most recently pinned message first. Restore the
	// shelf's highest-priority entry last so navigation preserves shelf order.
	for index := len(members) - 1; index >= 0; index-- {
		member := members[index]
		sessionLock := a.sessionMutex(member.ID)
		sessionLock.Lock()
		lock := a.anchorMutex(member.ID)
		lock.Lock()
		if member.PendingCollapse {
			updated, committed, stateErr := a.Store.FinishCollapseSessionIntoShelf(member.ID, expected.MessageID)
			if !committed {
				if stateErr != nil {
					_ = a.audit("state.collapsed_expand", "finish_pending_collapse_failed", map[string]any{"session_id": member.ID, "error": stateErr.Error()})
				}
				lock.Unlock()
				sessionLock.Unlock()
				continue
			}
			member = updated
		}
		if member.AnchorMessageID != 0 && !a.retireCollapsedSessionAnchorLocked(ctx, member) {
			lock.Unlock()
			sessionLock.Unlock()
			continue
		}
		if latest, ok := a.Store.FindSession(member.ID); ok {
			member = latest
		}
		if member.PendingRestore == nil {
			presented := member
			presented.Collapsed = false
			presented.AnchorFormat = anchorFormatText
			summary := firstNonEmpty(member.LastSummary, compactFallbackSummary(member, tmux.StyledCapture{}))
			summary += "\n\nRestored from cached state. Refreshing now."
			rendered := a.renderLocal(presented, summary)
			message, sendErr := a.sendSilentAnchor(ctx, expected.ChatID, rendered, 0, nil)
			if sendErr != nil {
				_ = a.audit("telegram.collapsed_expand", "send_failed", map[string]any{"session_id": member.ID, "error": sendErr.Error()})
				lock.Unlock()
				sessionLock.Unlock()
				continue
			}
			updated, committed, stateErr := a.Store.BeginExpandSessionFromShelf(member.ID, expected.MessageID, message.Chat.ID, message.MessageID)
			if !committed {
				a.retireProspectiveMessage(ctx, message.Chat.ID, message.MessageID)
				if stateErr != nil {
					_ = a.audit("state.collapsed_expand", "begin_failed", map[string]any{"session_id": member.ID, "error": stateErr.Error()})
				}
				lock.Unlock()
				sessionLock.Unlock()
				continue
			}
			if stateErr != nil {
				_ = a.audit("state.collapsed_expand", "begin_durability_uncertain", map[string]any{"session_id": member.ID, "error": stateErr.Error()})
			}
			member = updated
		}
		restoredMember := a.finishPendingRestoreLocked(ctx, member)
		if restoredMember {
			a.resetConversationEpochLocked(member.ID)
			restored = append(restored, member.ID)
		}
		lock.Unlock()
		sessionLock.Unlock()
		if restoredMember {
			a.queueManualRefresh(member.ID)
		}
	}
	a.reconcileCollapsedShelfLocked(ctx)
	if len(restored) != len(members) {
		return actionResult{
			Outcome: actionTelegramFailed,
			Message: fmt.Sprintf("restored %d of %d sessions; tap ➕ Show to retry the remaining sessions", len(restored), len(members)),
		}
	}
	_ = a.audit("anchor.expand_all", "ok", map[string]any{"session_count": len(restored)})
	return actionResult{Outcome: actionOK, Message: fmt.Sprintf("restored %d sessions", len(restored))}
}

func (a *App) finishPendingRestoreLocked(ctx context.Context, session state.TerminalSession) bool {
	pending := session.PendingRestore
	if pending == nil {
		return !session.Collapsed
	}
	if deadline := a.pendingRestoreRetryDeadline(session.ID, *pending); !deadline.IsZero() && time.Now().Before(deadline) {
		return false
	}
	presented := session
	presented.Collapsed = false
	presented.AnchorChatID = pending.ChatID
	presented.AnchorMessageID = pending.MessageID
	presented.AnchorFormat = anchorFormatText
	if _, err := a.Telegram.EditReplyMarkup(ctx, pending.ChatID, pending.MessageID, a.anchorMarkup(presented)); err != nil && !telegram.IsMessageNotModified(err) {
		a.deferPendingRestoreRetry(session.ID, pending.MessageID, err)
		return false
	}
	if err := a.Telegram.PinChatMessage(ctx, pending.ChatID, pending.MessageID); err != nil && !telegram.IsMessageAlreadyPinned(err) {
		a.deferPendingRestoreRetry(session.ID, pending.MessageID, err)
		return false
	}
	_, committed, stateErr := a.Store.FinishExpandSessionFromShelf(session.ID, pending.ChatID, pending.MessageID)
	if !committed {
		if stateErr != nil {
			_ = a.audit("state.collapsed_expand", "finish_failed", map[string]any{"session_id": session.ID, "error": stateErr.Error()})
			a.deferPendingRestoreRetry(session.ID, pending.MessageID, stateErr)
		}
		return false
	}
	if stateErr != nil {
		_ = a.audit("state.collapsed_expand", "finish_durability_uncertain", map[string]any{"session_id": session.ID, "error": stateErr.Error()})
	}
	a.pendingRestoreRetries.Delete(session.ID)
	return true
}

func (a *App) deferPendingRestoreRetry(id, messageID int, cause error) {
	delay := anchorRetirementRetryDelay
	if retryAfter := telegram.RetryAfter(cause); retryAfter > delay {
		delay = retryAfter
	}
	deadline := time.Now().UTC().Add(delay)
	a.pendingRestoreRetries.Store(id, collapsedAnchorRetry{messageID: messageID, at: deadline})
	if _, _, err := a.Store.UpdateSession(id, func(session *state.TerminalSession) {
		if session.PendingRestore != nil && session.PendingRestore.MessageID == messageID {
			session.PendingRestore.RetryAt = deadline
		}
	}); err != nil {
		_ = a.audit("state.collapsed_expand", "retry_failed", map[string]any{"session_id": id, "error": err.Error()})
	}
}

func (a *App) retireClosedPendingRestoreLocked(ctx context.Context, session state.TerminalSession) bool {
	pending := session.PendingRestore
	if pending == nil {
		return true
	}
	if deadline := a.pendingRestoreRetryDeadline(session.ID, *pending); !deadline.IsZero() && time.Now().Before(deadline) {
		return false
	}
	err := a.Telegram.DeleteMessage(ctx, pending.ChatID, pending.MessageID)
	if err != nil && !isTelegramMessageGone(err) {
		if telegram.IsRateLimited(err) {
			a.deferPendingRestoreRetry(session.ID, pending.MessageID, err)
			return false
		}
		if _, editErr := a.Telegram.EditReplyMarkup(ctx, pending.ChatID, pending.MessageID, telegram.ClearMarkup()); editErr != nil && !telegram.IsMessageNotModified(editErr) && !isTelegramAnchorUnavailable(editErr) {
			a.deferPendingRestoreRetry(session.ID, pending.MessageID, editErr)
			return false
		}
		if unpinErr := a.Telegram.UnpinChatMessage(ctx, pending.ChatID, pending.MessageID); unpinErr != nil && !telegram.IsMessageNotPinned(unpinErr) && !isTelegramAnchorUnavailable(unpinErr) {
			a.deferPendingRestoreRetry(session.ID, pending.MessageID, unpinErr)
			return false
		}
	}
	_, retired, stateErr := a.Store.FinishPendingRestoreRetirement(session.ID, pending.ChatID, pending.MessageID)
	if stateErr != nil {
		_ = a.audit("state.collapsed_expand", "closed_pending_retire_failed", map[string]any{"session_id": session.ID, "error": stateErr.Error()})
	}
	if retired {
		a.pendingRestoreRetries.Delete(session.ID)
	}
	return retired
}

func (a *App) pendingRestoreRetryDeadline(id int, pending state.PendingRestore) time.Time {
	deadline := pending.RetryAt
	if value, ok := a.pendingRestoreRetries.Load(id); ok {
		retry := value.(collapsedAnchorRetry)
		if retry.messageID == pending.MessageID && retry.at.After(deadline) {
			deadline = retry.at
		}
	}
	return deadline
}

func (a *App) reconcileCollapsedShelf(ctx context.Context) {
	a.collapsedShelfMu.Lock()
	defer a.collapsedShelfMu.Unlock()
	a.presentationMu.RLock()
	defer a.presentationMu.RUnlock()
	a.reconcileCollapsedShelfLocked(ctx)
}

func (a *App) reconcileCollapsedShelfLocked(ctx context.Context) {
	snapshot := a.Store.Snapshot()
	for _, session := range snapshot.TerminalSessions {
		if session.State != state.TerminalClosed || session.PendingRestore == nil {
			continue
		}
		sessionLock := a.sessionMutex(session.ID)
		sessionLock.Lock()
		anchorLock := a.anchorMutex(session.ID)
		anchorLock.Lock()
		if latest, ok := a.Store.FindSession(session.ID); ok && latest.State == state.TerminalClosed {
			a.retireClosedPendingRestoreLocked(ctx, latest)
		}
		anchorLock.Unlock()
		sessionLock.Unlock()
	}
	snapshot = a.Store.Snapshot()
	if snapshot.CollapsedShelf != nil {
		for _, member := range collapsedShelfSessions(snapshot.TerminalSessions) {
			if member.PendingRestore == nil {
				continue
			}
			sessionLock := a.sessionMutex(member.ID)
			sessionLock.Lock()
			anchorLock := a.anchorMutex(member.ID)
			anchorLock.Lock()
			restoredMember := false
			if latest, ok := a.Store.FindSession(member.ID); ok && latest.PendingRestore != nil &&
				a.finishPendingRestoreLocked(ctx, latest) {
				a.resetConversationEpochLocked(member.ID)
				restoredMember = true
			}
			anchorLock.Unlock()
			sessionLock.Unlock()
			if restoredMember {
				a.queueManualRefresh(member.ID)
			}
		}
		snapshot = a.Store.Snapshot()
	}
	members := collapsedShelfSessions(snapshot.TerminalSessions)
	if len(members) == 0 {
		if snapshot.CollapsedShelf != nil {
			a.retireCollapsedShelfLocked(ctx, *snapshot.CollapsedShelf)
		}
		return
	}

	rendered := a.renderCollapsedShelf(members)
	renderHash := sha(rendered)
	shelf := snapshot.CollapsedShelf
	if shelf == nil {
		message, err := a.sendSilentAnchor(ctx, a.Config.TelegramChatID, rendered, 0, nil)
		if err != nil {
			_ = a.audit("telegram.collapsed_shelf", "recovery_send_failed", map[string]any{"error": err.Error()})
			return
		}
		prospective := state.CollapsedShelf{
			ChatID: message.Chat.ID, MessageID: message.MessageID,
		}
		stored, committed, stateErr := a.Store.SetCollapsedShelfIfEmpty(prospective)
		if !committed {
			a.retireProspectiveMessage(ctx, message.Chat.ID, message.MessageID)
			if stateErr != nil {
				_ = a.audit("state.collapsed_shelf", "recovery_failed", map[string]any{"error": stateErr.Error()})
			}
			return
		}
		if stateErr != nil {
			_ = a.audit("state.collapsed_shelf", "recovery_durability_uncertain", map[string]any{"error": stateErr.Error()})
		}
		shelf = &stored
	}
	if deadline := a.collapsedShelfRetryDeadline(*shelf); !deadline.IsZero() && time.Now().Before(deadline) {
		return
	}

	if shelf.LastRenderHash != renderHash || !shelf.PinKnown {
		if _, err := a.editAnchor(ctx, shelf.ChatID, shelf.MessageID, rendered, telegram.CollapsedShelfMarkup()); err != nil && !telegram.IsMessageNotModified(err) {
			if isTelegramAnchorUnavailable(err) {
				if shelf.RetiringMessageID != 0 {
					if !a.retireCollapsedShelfPredecessorLocked(ctx, *shelf) {
						return
					}
					refreshed := a.Store.Snapshot().CollapsedShelf
					if refreshed == nil || refreshed.MessageID != shelf.MessageID {
						return
					}
					shelf = refreshed
				}
				a.replaceCollapsedShelfLocked(ctx, *shelf, rendered, renderHash)
			} else {
				_ = a.audit("telegram.collapsed_shelf", "edit_failed", map[string]any{"error": err.Error()})
				a.deferCollapsedShelfRetry(shelf.MessageID, err)
			}
			return
		}
		updated, found, err := a.Store.UpdateCollapsedShelf(shelf.MessageID, func(current *state.CollapsedShelf) {
			current.LastRenderHash = renderHash
			current.RetryAt = time.Time{}
		})
		if !found {
			return
		}
		if err != nil {
			_ = a.audit("state.collapsed_shelf", "render_hash_failed", map[string]any{"error": err.Error()})
		}
		shelf = &updated
	}
	if !a.ensureCollapsedShelfPinnedLocked(ctx, *shelf) {
		return
	}
	for _, member := range members {
		if !member.PendingCollapse {
			continue
		}
		sessionLock := a.sessionMutex(member.ID)
		sessionLock.Lock()
		anchorLock := a.anchorMutex(member.ID)
		anchorLock.Lock()
		if latest, ok := a.Store.FindSession(member.ID); ok && latest.PendingCollapse {
			if _, committed, stateErr := a.Store.FinishCollapseSessionIntoShelf(member.ID, shelf.MessageID); committed {
				a.resetConversationEpochLocked(member.ID)
			} else if stateErr != nil {
				_ = a.audit("state.collapsed_shelf", "finish_membership_failed", map[string]any{"session_id": member.ID, "error": stateErr.Error()})
			}
		}
		anchorLock.Unlock()
		sessionLock.Unlock()
	}
	snapshot = a.Store.Snapshot()
	members = collapsedShelfSessions(snapshot.TerminalSessions)
	if !a.retireCollapsedShelfPredecessorLocked(ctx, *shelf) {
		return
	}
	for _, member := range members {
		if member.AnchorMessageID == 0 {
			continue
		}
		lock := a.anchorMutex(member.ID)
		lock.Lock()
		a.retireCollapsedSessionAnchorLocked(ctx, member)
		lock.Unlock()
	}
}

func (a *App) replaceCollapsedShelfLocked(ctx context.Context, old state.CollapsedShelf, rendered, renderHash string) {
	message, err := a.sendSilentAnchor(ctx, old.ChatID, rendered, 0, nil)
	if err != nil {
		_ = a.audit("telegram.collapsed_shelf", "replacement_failed", map[string]any{"error": err.Error()})
		a.deferCollapsedShelfRetry(old.MessageID, err)
		return
	}
	prospective := state.CollapsedShelf{
		ChatID: message.Chat.ID, MessageID: message.MessageID,
	}
	stored, found, stateErr := a.Store.ReplaceCollapsedShelf(old.MessageID, prospective)
	committed := found && (stateErr == nil || state.PersistenceReachedReplacement(stateErr))
	if !committed {
		a.retireProspectiveMessage(ctx, message.Chat.ID, message.MessageID)
		return
	}
	if stateErr != nil {
		_ = a.audit("state.collapsed_shelf", "replacement_durability_uncertain", map[string]any{"error": stateErr.Error()})
	}
	a.clearCollapsedShelfRetry(old.MessageID)
	if _, err := a.editAnchor(ctx, stored.ChatID, stored.MessageID, rendered, telegram.CollapsedShelfMarkup()); err != nil && !telegram.IsMessageNotModified(err) {
		a.deferCollapsedShelfRetry(stored.MessageID, err)
		return
	}
	updated, _, updateErr := a.Store.UpdateCollapsedShelf(stored.MessageID, func(current *state.CollapsedShelf) {
		current.LastRenderHash = renderHash
		current.RetryAt = time.Time{}
	})
	if updateErr != nil {
		_ = a.audit("state.collapsed_shelf", "replacement_render_failed", map[string]any{"error": updateErr.Error()})
	}
	if !a.ensureCollapsedShelfPinnedLocked(ctx, updated) {
		return
	}
	a.retireCollapsedShelfPredecessorLocked(ctx, updated)
}

func (a *App) ensureCollapsedShelfPinnedLocked(ctx context.Context, shelf state.CollapsedShelf) bool {
	if shelf.PinKnown && shelf.Pinned {
		a.clearCollapsedShelfRetry(shelf.MessageID)
		return true
	}
	if err := a.Telegram.PinChatMessage(ctx, shelf.ChatID, shelf.MessageID); err != nil && !telegram.IsMessageAlreadyPinned(err) {
		_ = a.audit("telegram.collapsed_shelf_pin", "failed", map[string]any{"message_id": shelf.MessageID, "error": err.Error()})
		a.deferCollapsedShelfRetry(shelf.MessageID, err)
		return false
	}
	_, found, err := a.Store.UpdateCollapsedShelf(shelf.MessageID, func(current *state.CollapsedShelf) {
		current.Pinned = true
		current.PinKnown = true
		current.RetryAt = time.Time{}
	})
	if err != nil {
		_ = a.audit("state.collapsed_shelf_pin", "failed", map[string]any{"message_id": shelf.MessageID, "error": err.Error()})
	}
	committed := found && (err == nil || state.PersistenceReachedReplacement(err))
	if committed {
		a.clearCollapsedShelfRetry(shelf.MessageID)
	} else {
		a.deferCollapsedShelfRetry(shelf.MessageID, err)
	}
	return committed
}

func (a *App) retireCollapsedShelfLocked(ctx context.Context, shelf state.CollapsedShelf) {
	if !a.retireCollapsedShelfPredecessorLocked(ctx, shelf) {
		return
	}
	err := a.Telegram.DeleteMessage(ctx, shelf.ChatID, shelf.MessageID)
	if err != nil && !isTelegramMessageGone(err) {
		if telegram.IsRateLimited(err) {
			a.deferCollapsedShelfRetry(shelf.MessageID, err)
			return
		}
		if _, editErr := a.editAnchor(ctx, shelf.ChatID, shelf.MessageID, "No collapsed sessions.", telegram.ClearMarkup()); editErr != nil && !telegram.IsMessageNotModified(editErr) && !isTelegramAnchorUnavailable(editErr) {
			_ = a.audit("telegram.collapsed_shelf", "retire_failed", map[string]any{"message_id": shelf.MessageID, "error": editErr.Error()})
			a.deferCollapsedShelfRetry(shelf.MessageID, editErr)
			return
		}
		if unpinErr := a.Telegram.UnpinChatMessage(ctx, shelf.ChatID, shelf.MessageID); unpinErr != nil && !telegram.IsMessageNotPinned(unpinErr) && !isTelegramAnchorUnavailable(unpinErr) {
			_ = a.audit("telegram.collapsed_shelf", "unpin_failed", map[string]any{"message_id": shelf.MessageID, "error": unpinErr.Error()})
			a.deferCollapsedShelfRetry(shelf.MessageID, unpinErr)
			return
		}
	}
	if _, clearErr := a.Store.ClearCollapsedShelf(shelf.MessageID); clearErr != nil {
		_ = a.audit("state.collapsed_shelf", "clear_failed", map[string]any{"message_id": shelf.MessageID, "error": clearErr.Error()})
		return
	}
	a.clearCollapsedShelfRetry(shelf.MessageID)
}

func (a *App) retireCollapsedShelfPredecessorLocked(ctx context.Context, shelf state.CollapsedShelf) bool {
	if shelf.RetiringMessageID == 0 {
		return true
	}
	if !shelf.RetiringRetryAt.IsZero() && time.Now().Before(shelf.RetiringRetryAt) {
		return false
	}
	err := a.Telegram.DeleteMessage(ctx, shelf.RetiringChatID, shelf.RetiringMessageID)
	if err != nil && !isTelegramMessageGone(err) {
		if telegram.IsRateLimited(err) {
			a.deferCollapsedShelfPredecessorRetry(shelf.MessageID, shelf.RetiringMessageID, err)
			return false
		}
		if _, editErr := a.editAnchor(ctx, shelf.RetiringChatID, shelf.RetiringMessageID, "Collapsed sessions moved.", telegram.ClearMarkup()); editErr != nil && !telegram.IsMessageNotModified(editErr) && !isTelegramAnchorUnavailable(editErr) {
			a.deferCollapsedShelfPredecessorRetry(shelf.MessageID, shelf.RetiringMessageID, editErr)
			return false
		}
		if unpinErr := a.Telegram.UnpinChatMessage(ctx, shelf.RetiringChatID, shelf.RetiringMessageID); unpinErr != nil && !telegram.IsMessageNotPinned(unpinErr) && !isTelegramAnchorUnavailable(unpinErr) {
			a.deferCollapsedShelfPredecessorRetry(shelf.MessageID, shelf.RetiringMessageID, unpinErr)
			return false
		}
	}
	_, retired, stateErr := a.Store.FinishCollapsedShelfRetirement(shelf.MessageID, shelf.RetiringChatID, shelf.RetiringMessageID)
	if stateErr != nil {
		_ = a.audit("state.collapsed_shelf", "predecessor_retire_failed", map[string]any{"message_id": shelf.RetiringMessageID, "error": stateErr.Error()})
	}
	return retired
}

func (a *App) deferCollapsedShelfPredecessorRetry(messageID, retiringMessageID int, cause error) {
	delay := anchorRetirementRetryDelay
	if retryAfter := telegram.RetryAfter(cause); retryAfter > delay {
		delay = retryAfter
	}
	deadline := time.Now().UTC().Add(delay)
	if a.collapsedShelfRetryMessageID != messageID || deadline.After(a.collapsedShelfRetryAt) {
		a.collapsedShelfRetryMessageID = messageID
		a.collapsedShelfRetryAt = deadline
	}
	if _, _, err := a.Store.UpdateCollapsedShelf(messageID, func(current *state.CollapsedShelf) {
		if current.RetiringMessageID == retiringMessageID {
			current.RetiringRetryAt = deadline
		}
	}); err != nil {
		_ = a.audit("state.collapsed_shelf", "predecessor_retry_failed", map[string]any{"message_id": retiringMessageID, "error": err.Error()})
	}
}

func (a *App) retireCollapsedSessionAnchorLocked(ctx context.Context, session state.TerminalSession) bool {
	if !session.Collapsed || session.AnchorMessageID == 0 {
		return true
	}
	if deadline := a.collapsedAnchorRetryDeadline(session); !deadline.IsZero() && time.Now().Before(deadline) {
		return false
	}
	removed := false
	var retireErr error
	if mediaAnchorFormat(session.AnchorFormat) {
		retireErr = a.Telegram.DeleteMessage(ctx, session.AnchorChatID, session.AnchorMessageID)
		if retireErr == nil || isTelegramMessageGone(retireErr) {
			removed = true
			retireErr = nil
		} else if !telegram.IsRateLimited(retireErr) {
			retireErr = a.replaceMediaWithTombstone(ctx, session.AnchorChatID, session.AnchorMessageID, a.collapsedAnchorText(session))
		}
	} else {
		_, retireErr = a.editAnchor(ctx, session.AnchorChatID, session.AnchorMessageID, a.collapsedAnchorText(session), telegram.ClearMarkup())
	}
	if retireErr != nil && !telegram.IsMessageNotModified(retireErr) && !isTelegramAnchorUnavailable(retireErr) {
		_ = a.audit("telegram.collapsed_anchor_retire", "failed", map[string]any{"session_id": session.ID, "error": retireErr.Error()})
		a.deferCollapsedAnchorRetirement(session.ID, session.AnchorMessageID, retireErr)
		return false
	}
	if !removed {
		if err := a.Telegram.UnpinChatMessage(ctx, session.AnchorChatID, session.AnchorMessageID); err != nil && !telegram.IsMessageNotPinned(err) && !isTelegramAnchorUnavailable(err) {
			_ = a.audit("telegram.collapsed_anchor_retire", "unpin_failed", map[string]any{"session_id": session.ID, "error": err.Error()})
			a.deferCollapsedAnchorRetirement(session.ID, session.AnchorMessageID, err)
			return false
		}
	}
	_, retired, err := a.Store.FinishCollapsedAnchorRetirement(session.ID, session.AnchorChatID, session.AnchorMessageID)
	if err != nil {
		_ = a.audit("state.collapsed_anchor_retire", "failed", map[string]any{"session_id": session.ID, "error": err.Error()})
	}
	if retired {
		a.collapsedAnchorRetries.Delete(session.ID)
	}
	return retired
}

func (a *App) deferCollapsedShelfRetry(messageID int, cause error) {
	delay := anchorRetirementRetryDelay
	if retryAfter := telegram.RetryAfter(cause); retryAfter > delay {
		delay = retryAfter
	}
	deadline := time.Now().UTC().Add(delay)
	if a.collapsedShelfRetryMessageID != messageID || deadline.After(a.collapsedShelfRetryAt) {
		a.collapsedShelfRetryMessageID = messageID
		a.collapsedShelfRetryAt = deadline
	}
	_, _, err := a.Store.UpdateCollapsedShelf(messageID, func(current *state.CollapsedShelf) {
		current.RetryAt = deadline
	})
	if err != nil {
		_ = a.audit("state.collapsed_shelf", "retry_failed", map[string]any{"message_id": messageID, "error": err.Error()})
	}
}

func (a *App) collapsedShelfRetryDeadline(shelf state.CollapsedShelf) time.Time {
	deadline := shelf.RetryAt
	if shelf.RetiringRetryAt.After(deadline) {
		deadline = shelf.RetiringRetryAt
	}
	if a.collapsedShelfRetryMessageID == shelf.MessageID && a.collapsedShelfRetryAt.After(deadline) {
		deadline = a.collapsedShelfRetryAt
	}
	return deadline
}

func (a *App) clearCollapsedShelfRetry(messageID int) {
	if a.collapsedShelfRetryMessageID != messageID {
		return
	}
	a.collapsedShelfRetryMessageID = 0
	a.collapsedShelfRetryAt = time.Time{}
}

type collapsedAnchorRetry struct {
	messageID int
	at        time.Time
}

func (a *App) deferCollapsedAnchorRetirement(id, messageID int, cause error) {
	delay := anchorRetirementRetryDelay
	if retryAfter := telegram.RetryAfter(cause); retryAfter > delay {
		delay = retryAfter
	}
	deadline := time.Now().UTC().Add(delay)
	a.collapsedAnchorRetries.Store(id, collapsedAnchorRetry{messageID: messageID, at: deadline})
	if _, _, err := a.Store.UpdateSession(id, func(session *state.TerminalSession) {
		if session.Collapsed && session.AnchorMessageID == messageID {
			session.RetiringAnchorRetryAt = deadline
		}
	}); err != nil {
		_ = a.audit("state.collapsed_anchor_retire", "retry_failed", map[string]any{"session_id": id, "error": err.Error()})
	}
}

func (a *App) collapsedAnchorRetryDeadline(session state.TerminalSession) time.Time {
	deadline := session.RetiringAnchorRetryAt
	if value, ok := a.collapsedAnchorRetries.Load(session.ID); ok {
		retry := value.(collapsedAnchorRetry)
		if retry.messageID == session.AnchorMessageID && retry.at.After(deadline) {
			deadline = retry.at
		}
	}
	return deadline
}

func (a *App) collapsedAnchorText(session state.TerminalSession) string {
	return fmt.Sprintf("[%d] %s\nmoved to Collapsed sessions", session.ID, firstNonEmpty(strings.TrimSpace(session.Title), "terminal"))
}

func (a *App) renderCollapsedShelf(sessions []state.TerminalSession) string {
	members := collapsedShelfSessions(sessions)
	var builder strings.Builder
	fmt.Fprintf(&builder, "Collapsed sessions (%d)\nCached status; terminals may have changed.\n\n", len(members))
	for index, session := range members {
		if index >= maxCollapsedShelfEntries {
			fmt.Fprintf(&builder, "+%d more", len(members)-index)
			break
		}
		line := a.renderCollapsedLine(session)
		remaining := len(members) - index
		reserve := len(fmt.Sprintf("\n+%d more", remaining))
		if builder.Len()+len(line)+1+reserve > maxCollapsedShelfBytes {
			fmt.Fprintf(&builder, "+%d more", remaining)
			break
		}
		builder.WriteString(line)
		if index != len(members)-1 {
			builder.WriteByte('\n')
		}
	}
	return builder.String()
}

func (a *App) renderCollapsedLine(session state.TerminalSession) string {
	a.redactSessionPresentation(&session)
	title := strings.Join(strings.Fields(firstNonEmpty(session.Title, "terminal")), " ")
	title = truncateAtWord(title, 40)
	summary := compactSummaryText(firstNonEmpty(session.LastSummary, compactFallbackSummary(session, tmux.StyledCapture{})))
	if session.State == state.TerminalLost {
		summary = "lost - restore the tmux pane from /sessions"
	}
	line := fmt.Sprintf("[%d] %s", session.ID, title)
	if summary != "" {
		line += " · " + summary
	} else {
		line += " · " + string(session.State)
	}
	return truncateAtWord(a.redactText(line), maxCollapsedLineBytes)
}

func collapsedShelfSessions(sessions []state.TerminalSession) []state.TerminalSession {
	members := make([]state.TerminalSession, 0)
	for _, session := range sessions {
		if (session.Collapsed || session.PendingCollapse) && session.State != state.TerminalClosed {
			members = append(members, session)
		}
	}
	sort.SliceStable(members, func(i, j int) bool {
		left, right := members[i], members[j]
		leftRank, rightRank := sessionPresentationRank(left), sessionPresentationRank(right)
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		leftTime, rightTime := sessionPresentationTime(left), sessionPresentationTime(right)
		if !leftTime.Equal(rightTime) {
			return leftTime.After(rightTime)
		}
		return left.ID < right.ID
	})
	return members
}

func (a *App) isCollapsedShelfMessage(chatID int64, messageID int) bool {
	shelf := a.Store.Snapshot().CollapsedShelf
	return shelf != nil && shelf.ChatID == chatID && shelf.MessageID == messageID
}

func compactSummaryText(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	text = guide.LimitWords(text, maxCollapsedLineWords)
	return truncateAtWord(text, maxCollapsedLineBytes)
}

func compactFallbackSummary(session state.TerminalSession, capture tmux.StyledCapture) string {
	if presentation := terminalPresentationText(session); presentation != "" {
		return compactSummaryText(presentation)
	}
	command := strings.TrimSpace(capture.CurrentCmd)
	cwd := firstNonEmpty(strings.TrimSpace(capture.CurrentPath), strings.TrimSpace(session.LastKnownCWD))
	if command != "" && cwd != "" {
		return compactSummaryText(command + " in " + cwd)
	}
	if command != "" {
		return compactSummaryText(command + " is " + string(session.State))
	}
	if cwd != "" {
		return compactSummaryText(string(session.State) + " in " + cwd)
	}
	return string(session.State)
}

func (a *App) retireProspectiveMessage(ctx context.Context, chatID int64, messageID int) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	// Prospective messages are deliberately inert and unpinned until their
	// identity is durable. One delete is sufficient and avoids amplifying 429s.
	if err := a.Telegram.DeleteMessage(cleanupCtx, chatID, messageID); err != nil && !isTelegramMessageGone(err) {
		_ = a.audit("telegram.prospective_cleanup", "delete_failed", map[string]any{"message_id": messageID, "error": err.Error()})
	}
}
