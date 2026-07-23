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
	sessionLock := a.sessionMutex(expected.ID)
	sessionLock.Lock()
	defer sessionLock.Unlock()
	lock := a.anchorMutex(expected.ID)
	lock.Lock()
	defer lock.Unlock()
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

	snapshot := a.Store.Snapshot()
	prospective := append([]state.TerminalSession(nil), snapshot.TerminalSessions...)
	for index := range prospective {
		if prospective[index].ID == current.ID {
			prospective[index].Collapsed = true
			break
		}
	}
	rendered := a.renderCollapsedShelf(prospective)
	renderHash := sha(rendered)
	shelf := snapshot.CollapsedShelf
	created := false
	if shelf == nil {
		message, err := a.sendSilentAnchor(ctx, a.Config.TelegramChatID, rendered, 0, nil)
		if err != nil {
			_ = a.audit("telegram.collapsed_shelf", "send_failed", map[string]any{"session_id": current.ID, "error": err.Error()})
			return actionResult{Outcome: actionTelegramFailed, Message: "could not create the collapsed shelf"}
		}
		shelf = &state.CollapsedShelf{
			ChatID: message.Chat.ID, MessageID: message.MessageID,
			LastRenderHash: renderHash, Pinned: true, PinKnown: true,
		}
		created = true
		if !a.activateAndPinProspectiveShelf(ctx, *shelf, rendered) {
			a.retireProspectiveMessage(ctx, shelf.ChatID, shelf.MessageID)
			return actionResult{Outcome: actionTelegramFailed, Message: "could not activate the collapsed shelf"}
		}
	} else {
		if !a.ensureCollapsedShelfPinnedLocked(ctx, *shelf) {
			return actionResult{Outcome: actionTelegramFailed, Message: "could not pin the collapsed shelf"}
		}
		if _, err := a.editAnchor(ctx, shelf.ChatID, shelf.MessageID, rendered, telegram.CollapsedShelfMarkup()); err != nil {
			_ = a.audit("telegram.collapsed_shelf", "prospective_edit_failed", map[string]any{"session_id": current.ID, "error": err.Error()})
			return actionResult{Outcome: actionTelegramFailed, Message: "could not update the collapsed shelf"}
		}
	}

	updated, committed, stateErr := a.Store.CollapseSessionIntoShelf(current.ID, current, *shelf, renderHash)
	if !committed {
		if created {
			a.retireProspectiveMessage(ctx, shelf.ChatID, shelf.MessageID)
		} else {
			a.restoreCollapsedShelfAfterFailedCommit(ctx, *shelf, snapshot.TerminalSessions)
		}
		return actionResult{Outcome: actionStateFailed, Message: "could not persist the collapsed shelf: " + firstNonEmpty(errorText(stateErr), "session changed")}
	}
	if stateErr != nil {
		_ = a.audit("state.collapsed_shelf", "durability_uncertain", map[string]any{"session_id": current.ID, "error": stateErr.Error()})
	}
	a.clearCollapsedShelfRetry(shelf.MessageID)
	a.retireCollapsedSessionAnchorLocked(ctx, updated)
	a.resetConversationEpochLocked(expected.ID)
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
	for _, member := range members {
		sessionLock := a.sessionMutex(member.ID)
		sessionLock.Lock()
		lock := a.anchorMutex(member.ID)
		lock.Lock()
		if member.AnchorMessageID != 0 && !a.retireCollapsedSessionAnchorLocked(ctx, member) {
			lock.Unlock()
			sessionLock.Unlock()
			continue
		}
		if latest, ok := a.Store.FindSession(member.ID); ok {
			member = latest
		}
		presented := member
		presented.Collapsed = false
		presented.AnchorFormat = anchorFormatText
		rendered := a.renderLocal(presented, firstNonEmpty(member.LastSummary, compactFallbackSummary(member, tmux.StyledCapture{})))
		message, sendErr := a.sendSilentAnchor(ctx, expected.ChatID, rendered, 0, nil)
		if sendErr != nil {
			_ = a.audit("telegram.collapsed_expand", "send_failed", map[string]any{"session_id": member.ID, "error": sendErr.Error()})
			lock.Unlock()
			sessionLock.Unlock()
			continue
		}
		if !a.activateAndPinProspectiveAnchor(ctx, presented, message.Chat.ID, message.MessageID) {
			a.retireProspectiveMessage(ctx, message.Chat.ID, message.MessageID)
			lock.Unlock()
			sessionLock.Unlock()
			continue
		}
		_, committed, stateErr := a.Store.ExpandSessionFromShelf(member.ID, expected.MessageID, message.Chat.ID, message.MessageID)
		if !committed {
			a.retireProspectiveMessage(ctx, message.Chat.ID, message.MessageID)
			if stateErr != nil {
				_ = a.audit("state.collapsed_expand", "failed", map[string]any{"session_id": member.ID, "error": stateErr.Error()})
			}
			lock.Unlock()
			sessionLock.Unlock()
			continue
		}
		if stateErr != nil {
			_ = a.audit("state.collapsed_expand", "durability_uncertain", map[string]any{"session_id": member.ID, "error": stateErr.Error()})
		}
		a.resetConversationEpochLocked(member.ID)
		restored = append(restored, member.ID)
		lock.Unlock()
		sessionLock.Unlock()
	}
	a.reconcileCollapsedShelfLocked(ctx)
	for _, id := range restored {
		a.queueManualRefresh(id)
	}
	if len(restored) != len(members) {
		return actionResult{
			Outcome: actionTelegramFailed,
			Message: fmt.Sprintf("restored %d of %d sessions; tap + to retry the remaining sessions", len(restored), len(members)),
		}
	}
	_ = a.audit("anchor.expand_all", "ok", map[string]any{"session_count": len(restored)})
	return actionResult{Outcome: actionOK, Message: fmt.Sprintf("restored %d sessions", len(restored))}
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
			LastRenderHash: renderHash, Pinned: true, PinKnown: true,
		}
		if !a.activateAndPinProspectiveShelf(ctx, prospective, rendered) {
			a.retireProspectiveMessage(ctx, prospective.ChatID, prospective.MessageID)
			return
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
		LastRenderHash: renderHash, Pinned: true, PinKnown: true,
	}
	if !a.activateAndPinProspectiveShelf(ctx, prospective, rendered) {
		a.retireProspectiveMessage(ctx, message.Chat.ID, message.MessageID)
		a.deferCollapsedShelfRetry(old.MessageID, nil)
		return
	}
	_, found, stateErr := a.Store.UpdateCollapsedShelf(old.MessageID, func(current *state.CollapsedShelf) {
		current.ChatID = message.Chat.ID
		current.MessageID = message.MessageID
		current.LastRenderHash = renderHash
		current.Pinned = true
		current.PinKnown = true
		current.RetryAt = time.Time{}
	})
	committed := found && (stateErr == nil || state.PersistenceReachedReplacement(stateErr))
	if !committed {
		a.retireProspectiveMessage(ctx, message.Chat.ID, message.MessageID)
		return
	}
	if stateErr != nil {
		_ = a.audit("state.collapsed_shelf", "replacement_durability_uncertain", map[string]any{"error": stateErr.Error()})
	}
	a.clearCollapsedShelfRetry(old.MessageID)
	a.deleteProspectiveMessage(ctx, old.ChatID, old.MessageID)
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

func (a *App) activateAndPinProspectiveShelf(ctx context.Context, shelf state.CollapsedShelf, rendered string) bool {
	if _, err := a.editAnchor(ctx, shelf.ChatID, shelf.MessageID, rendered, telegram.CollapsedShelfMarkup()); err != nil && !telegram.IsMessageNotModified(err) {
		_ = a.audit("telegram.collapsed_shelf", "prospective_controls_failed", map[string]any{"message_id": shelf.MessageID, "error": err.Error()})
		return false
	}
	if err := a.Telegram.PinChatMessage(ctx, shelf.ChatID, shelf.MessageID); err != nil && !telegram.IsMessageAlreadyPinned(err) {
		_ = a.audit("telegram.collapsed_shelf_pin", "prospective_failed", map[string]any{"message_id": shelf.MessageID, "error": err.Error()})
		return false
	}
	return true
}

func (a *App) activateAndPinProspectiveAnchor(ctx context.Context, session state.TerminalSession, chatID int64, messageID int) bool {
	session.Collapsed = false
	session.AnchorChatID = chatID
	session.AnchorMessageID = messageID
	session.AnchorFormat = anchorFormatText
	if _, err := a.Telegram.EditReplyMarkup(ctx, chatID, messageID, a.anchorMarkup(session)); err != nil && !telegram.IsMessageNotModified(err) {
		_ = a.audit("telegram.collapsed_expand", "prospective_controls_failed", map[string]any{"session_id": session.ID, "message_id": messageID, "error": err.Error()})
		return false
	}
	if err := a.Telegram.PinChatMessage(ctx, chatID, messageID); err != nil && !telegram.IsMessageAlreadyPinned(err) {
		_ = a.audit("telegram.collapsed_expand", "prospective_pin_failed", map[string]any{"session_id": session.ID, "message_id": messageID, "error": err.Error()})
		return false
	}
	return true
}

func (a *App) restoreCollapsedShelfAfterFailedCommit(ctx context.Context, shelf state.CollapsedShelf, sessions []state.TerminalSession) {
	rendered := a.renderCollapsedShelf(sessions)
	if _, err := a.editAnchor(ctx, shelf.ChatID, shelf.MessageID, rendered, telegram.CollapsedShelfMarkup()); err != nil && !telegram.IsMessageNotModified(err) {
		_ = a.audit("telegram.collapsed_shelf", "rollback_edit_failed", map[string]any{"message_id": shelf.MessageID, "error": err.Error()})
		_, _, stateErr := a.Store.UpdateCollapsedShelf(shelf.MessageID, func(current *state.CollapsedShelf) {
			current.LastRenderHash = ""
		})
		if stateErr != nil {
			_ = a.audit("state.collapsed_shelf", "rollback_invalidation_failed", map[string]any{"message_id": shelf.MessageID, "error": stateErr.Error()})
		}
		a.deferCollapsedShelfRetry(shelf.MessageID, err)
	}
}

func (a *App) retireCollapsedShelfLocked(ctx context.Context, shelf state.CollapsedShelf) {
	err := a.Telegram.DeleteMessage(ctx, shelf.ChatID, shelf.MessageID)
	if err != nil && !isTelegramMessageGone(err) {
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

func (a *App) retireCollapsedSessionAnchorLocked(ctx context.Context, session state.TerminalSession) bool {
	if !session.Collapsed || session.AnchorMessageID == 0 {
		return true
	}
	if !session.RetiringAnchorRetryAt.IsZero() && time.Now().Before(session.RetiringAnchorRetryAt) {
		return false
	}
	removed := false
	var retireErr error
	if mediaAnchorFormat(session.AnchorFormat) {
		retireErr = a.Telegram.DeleteMessage(ctx, session.AnchorChatID, session.AnchorMessageID)
		if retireErr == nil || isTelegramMessageGone(retireErr) {
			removed = true
			retireErr = nil
		} else {
			retireErr = a.replaceMediaWithTombstone(ctx, session.AnchorChatID, session.AnchorMessageID, a.collapsedAnchorText(session))
		}
	} else {
		_, retireErr = a.editAnchor(ctx, session.AnchorChatID, session.AnchorMessageID, a.collapsedAnchorText(session), telegram.ClearMarkup())
	}
	if retireErr != nil && !telegram.IsMessageNotModified(retireErr) && !isTelegramAnchorUnavailable(retireErr) {
		_ = a.audit("telegram.collapsed_anchor_retire", "failed", map[string]any{"session_id": session.ID, "error": retireErr.Error()})
		a.deferCollapsedAnchorRetirement(session.ID, session.AnchorMessageID)
		return false
	}
	if !removed {
		if err := a.Telegram.UnpinChatMessage(ctx, session.AnchorChatID, session.AnchorMessageID); err != nil && !telegram.IsMessageNotPinned(err) && !isTelegramAnchorUnavailable(err) {
			_ = a.audit("telegram.collapsed_anchor_retire", "unpin_failed", map[string]any{"session_id": session.ID, "error": err.Error()})
			a.deferCollapsedAnchorRetirement(session.ID, session.AnchorMessageID)
			return false
		}
	}
	_, retired, err := a.Store.FinishCollapsedAnchorRetirement(session.ID, session.AnchorChatID, session.AnchorMessageID)
	if err != nil {
		_ = a.audit("state.collapsed_anchor_retire", "failed", map[string]any{"session_id": session.ID, "error": err.Error()})
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

func (a *App) deferCollapsedAnchorRetirement(id, messageID int) {
	if _, _, err := a.Store.UpdateSession(id, func(session *state.TerminalSession) {
		if session.Collapsed && session.AnchorMessageID == messageID {
			session.RetiringAnchorRetryAt = time.Now().UTC().Add(anchorRetirementRetryDelay)
		}
	}); err != nil {
		_ = a.audit("state.collapsed_anchor_retire", "retry_failed", map[string]any{"session_id": id, "error": err.Error()})
	}
}

func (a *App) collapsedAnchorText(session state.TerminalSession) string {
	return fmt.Sprintf("[%d] %s\nmoved to Collapsed sessions", session.ID, firstNonEmpty(strings.TrimSpace(session.Title), "terminal"))
}

func (a *App) renderCollapsedShelf(sessions []state.TerminalSession) string {
	members := collapsedShelfSessions(sessions)
	var builder strings.Builder
	fmt.Fprintf(&builder, "Collapsed sessions (%d)\n\n", len(members))
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
		if session.Collapsed && session.State != state.TerminalClosed {
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

func (a *App) deleteProspectiveMessage(ctx context.Context, chatID int64, messageID int) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := a.Telegram.DeleteMessage(cleanupCtx, chatID, messageID); err != nil && !isTelegramMessageGone(err) {
		_ = a.audit("telegram.prospective_cleanup", "failed", map[string]any{"message_id": messageID, "error": err.Error()})
	}
}

func (a *App) retireProspectiveMessage(ctx context.Context, chatID int64, messageID int) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := a.Telegram.EditReplyMarkup(cleanupCtx, chatID, messageID, telegram.ClearMarkup()); err != nil && !telegram.IsMessageNotModified(err) && !isTelegramAnchorUnavailable(err) {
		_ = a.audit("telegram.prospective_cleanup", "controls_failed", map[string]any{"message_id": messageID, "error": err.Error()})
	}
	if err := a.Telegram.UnpinChatMessage(cleanupCtx, chatID, messageID); err != nil && !telegram.IsMessageNotPinned(err) && !isTelegramAnchorUnavailable(err) {
		_ = a.audit("telegram.prospective_cleanup", "unpin_failed", map[string]any{"message_id": messageID, "error": err.Error()})
	}
	if err := a.Telegram.DeleteMessage(cleanupCtx, chatID, messageID); err != nil && !isTelegramMessageGone(err) {
		_ = a.audit("telegram.prospective_cleanup", "delete_failed", map[string]any{"message_id": messageID, "error": err.Error()})
	}
}
