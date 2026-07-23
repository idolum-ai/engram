package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/idolum-ai/engram/internal/guide"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
	"github.com/idolum-ai/engram/internal/tmux"
)

const maxCollapsedAnchorBytes = 320

func (a *App) collapseAnchor(ctx context.Context, expected state.TerminalSession) actionResult {
	a.presentationMu.RLock()
	defer a.presentationMu.RUnlock()
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

	summary := compactSummaryText(firstNonEmpty(current.LastSummary, compactFallbackSummary(current, tmux.StyledCapture{})))
	rendered := a.renderCollapsedAnchor(current, summary)
	var updated state.TerminalSession
	var committed bool
	var err error
	if mediaAnchorFormat(current.AnchorFormat) {
		updated, committed, err = a.rotateAnchorToCollapsedTextLocked(ctx, current, rendered, sha(rendered))
	} else {
		updated, committed, err = a.collapseTextAnchorLocked(ctx, current, rendered, sha(rendered))
	}
	if err != nil {
		return actionResult{Outcome: actionStateFailed, Message: "could not collapse card: " + err.Error()}
	}
	if !committed {
		return actionResult{Outcome: actionTelegramFailed, Message: "could not collapse card"}
	}
	a.resetConversationEpochLocked(expected.ID)
	_ = a.audit("anchor.collapse", "ok", map[string]any{"session_id": expected.ID, "message_id": updated.AnchorMessageID})
	return actionResult{Outcome: actionOK, Message: "collapsed"}
}

func (a *App) expandAnchor(ctx context.Context, expected state.TerminalSession) actionResult {
	a.presentationMu.RLock()
	defer a.presentationMu.RUnlock()
	lock := a.anchorMutex(expected.ID)
	lock.Lock()
	defer lock.Unlock()
	a.finishAnchorRotationLocked(ctx, expected.ID)

	current, ok := a.Store.FindSession(expected.ID)
	if !ok || current.AnchorChatID != expected.AnchorChatID || current.AnchorMessageID != expected.AnchorMessageID || !sameTerminalBinding(current, expected) {
		return actionResult{Outcome: actionUserError, Message: "anchor moved; use the newer live message"}
	}
	if current.State != state.TerminalRunning || !current.WatchEnabled || current.RetiringAnchorMessageID != 0 {
		return actionResult{Outcome: actionUserError, Message: "this card cannot be expanded right now"}
	}
	if !current.Collapsed {
		return actionResult{Outcome: actionOK, Message: "already expanded"}
	}

	applied := false
	updated, found, err := a.Store.UpdateSession(current.ID, func(session *state.TerminalSession) {
		if session.AnchorMessageID != current.AnchorMessageID || session.AnchorChatID != current.AnchorChatID || session.RetiringAnchorMessageID != 0 || !sameTerminalBinding(*session, current) || !session.Collapsed {
			return
		}
		session.Collapsed = false
		session.LastRawCaptureHash = ""
		session.LastSnapshotCaptureHash = ""
		session.LastSnapshotAttemptAt = time.Time{}
		session.LastRenderHash = ""
		session.LastAnchorEditAt = time.Time{}
		setAnchorFiles(session, nil)
		applied = true
	})
	committed := found && applied && (err == nil || state.PersistenceReachedReplacement(err))
	if !committed {
		return actionResult{Outcome: actionStateFailed, Message: "could not expand card: " + firstNonEmpty(errorText(err), "session changed")}
	}
	if err != nil {
		_ = a.audit("state.anchor_expand", "durability_uncertain", map[string]any{"session_id": current.ID, "error": err.Error()})
	}
	a.resetConversationEpochLocked(current.ID)
	if _, markupErr := a.Telegram.EditReplyMarkup(ctx, updated.AnchorChatID, updated.AnchorMessageID, a.anchorMarkup(updated)); markupErr != nil && !telegram.IsMessageNotModified(markupErr) && !isTelegramAnchorUnavailable(markupErr) {
		_ = a.audit("telegram.anchor_expand", "controls_failed", map[string]any{"session_id": current.ID, "error": markupErr.Error()})
	}
	_ = a.audit("anchor.expand", "ok", map[string]any{"session_id": current.ID, "message_id": updated.AnchorMessageID, "mode": a.anchorMode()})
	return actionResult{Outcome: actionOK, Message: "expanding"}
}

func (a *App) collapseTextAnchorLocked(ctx context.Context, current state.TerminalSession, rendered, renderHash string) (state.TerminalSession, bool, error) {
	applied := false
	updated, found, stateErr := a.Store.UpdateSession(current.ID, func(session *state.TerminalSession) {
		if session.AnchorChatID != current.AnchorChatID || session.AnchorMessageID != current.AnchorMessageID || session.RetiringAnchorMessageID != 0 || session.Collapsed || !sameTerminalBinding(*session, current) {
			return
		}
		session.Collapsed = true
		session.AnchorFormat = anchorFormatText
		session.LastRawCaptureHash = ""
		session.LastSnapshotCaptureHash = ""
		session.LastRenderHash = ""
		session.LastAnchorEditAt = time.Time{}
		setAnchorFiles(session, nil)
		applied = true
	})
	committed := found && applied && (stateErr == nil || state.PersistenceReachedReplacement(stateErr))
	if !committed {
		if stateErr != nil {
			_ = a.audit("state.anchor_collapse", "failed", map[string]any{"session_id": current.ID, "error": stateErr.Error()})
		}
		return current, false, stateErr
	}
	if stateErr != nil {
		_ = a.audit("state.anchor_collapse", "durability_uncertain", map[string]any{"session_id": current.ID, "error": stateErr.Error()})
	}
	if _, editErr := a.editAnchor(ctx, updated.AnchorChatID, updated.AnchorMessageID, rendered, a.anchorMarkup(updated)); editErr != nil {
		_ = a.audit("telegram.anchor_collapse", "edit_failed", map[string]any{"session_id": current.ID, "error": editErr.Error()})
		return updated, true, nil
	}
	stored, _, hashErr := a.Store.UpdateSession(current.ID, func(session *state.TerminalSession) {
		if session.Collapsed && session.AnchorChatID == updated.AnchorChatID && session.AnchorMessageID == updated.AnchorMessageID && session.RetiringAnchorMessageID == 0 && sameTerminalBinding(*session, updated) {
			session.LastRenderHash = renderHash
			session.LastAnchorEditAt = time.Now().UTC()
		}
	})
	if hashErr != nil {
		_ = a.audit("state.anchor_collapse", "render_hash_failed", map[string]any{"session_id": current.ID, "error": hashErr.Error()})
		return updated, true, nil
	}
	return stored, true, nil
}

func (a *App) rotateAnchorToCollapsedTextLocked(ctx context.Context, current state.TerminalSession, rendered, renderHash string) (state.TerminalSession, bool, error) {
	// Telegram may accept a send even when its response is lost. Publish the
	// prospective message without controls so an untracked orphan is inert;
	// activate it only after state owns the returned message ID.
	message, err := a.sendSilentAnchor(ctx, current.AnchorChatID, rendered, 0, nil)
	if err != nil {
		_ = a.audit("telegram.anchor_collapse", "send_failed", map[string]any{"session_id": current.ID, "error": err.Error()})
		return current, false, nil
	}

	oldID := current.AnchorMessageID
	oldFormat := firstNonEmpty(current.AnchorFormat, anchorFormatText)
	applied := false
	updated, found, stateErr := a.Store.UpdateSession(current.ID, func(session *state.TerminalSession) {
		if session.AnchorChatID != current.AnchorChatID || session.AnchorMessageID != oldID || session.RetiringAnchorMessageID != 0 || session.Collapsed || !sameTerminalBinding(*session, current) {
			return
		}
		session.Collapsed = true
		session.AnchorMessageID = message.MessageID
		session.AnchorFormat = anchorFormatText
		session.RetiringAnchorMessageID = oldID
		session.RetiringAnchorFormat = oldFormat
		session.AnchorPinned = false
		session.AnchorPinKnown = false
		session.LastRawCaptureHash = ""
		session.LastSnapshotCaptureHash = ""
		session.LastRenderHash = renderHash
		session.LastAnchorEditAt = time.Now().UTC()
		setAnchorFiles(session, nil)
		applied = true
	})
	committed := found && applied && (stateErr == nil || state.PersistenceReachedReplacement(stateErr))
	if !committed {
		a.deactivateProspectiveAnchor(ctx, current.AnchorChatID, message.MessageID, a.retiredAnchorText(current))
		if stateErr != nil {
			_ = a.audit("state.anchor_collapse", "failed", map[string]any{"session_id": current.ID, "error": stateErr.Error()})
		}
		return current, false, stateErr
	}
	if stateErr != nil {
		_ = a.audit("state.anchor_collapse", "durability_uncertain", map[string]any{"session_id": current.ID, "error": stateErr.Error()})
	}
	if _, markupErr := a.Telegram.EditReplyMarkup(ctx, updated.AnchorChatID, updated.AnchorMessageID, a.anchorMarkup(updated)); markupErr != nil && !telegram.IsMessageNotModified(markupErr) {
		_ = a.audit("telegram.anchor_collapse", "controls_failed", map[string]any{"session_id": current.ID, "error": markupErr.Error()})
	}
	if anchorShouldBePinned(updated) {
		a.ensureCurrentAnchorPinnedLocked(ctx, updated)
	}
	a.finishAnchorRotationLocked(ctx, current.ID)
	return updated, true, nil
}

func (a *App) renderCollapsedAnchor(session state.TerminalSession, summary string) string {
	a.redactSessionPresentation(&session)
	summary = a.redactText(summary)
	title := strings.Join(strings.Fields(firstNonEmpty(session.Title, "terminal")), " ")
	title = truncateAtWord(title, 56)
	line := fmt.Sprintf("[%d] %s", session.ID, title)
	if summary = compactSummaryText(summary); summary != "" {
		prefix := line + " · "
		return prefix + truncateAtWord(summary, maxCollapsedAnchorBytes-len(prefix))
	} else {
		line += " · " + string(session.State)
	}
	return truncateAtWord(line, maxCollapsedAnchorBytes)
}

func compactSummaryText(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	text = guide.LimitWords(text, guide.CompactMaxWords)
	return truncateAtWord(text, maxCollapsedAnchorBytes)
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
