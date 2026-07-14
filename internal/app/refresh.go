package app

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/idolum-ai/engram/internal/anthropic"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
	"github.com/idolum-ai/engram/internal/terminalshot"
	"github.com/idolum-ai/engram/internal/tmux"
)

const maxStoredVisibleCaptureBytes = 16 * 1024

var errConversationTurnSuperseded = errors.New("conversation turn superseded")

func (a *App) refreshSession(ctx context.Context, id int, force bool) {
	if a.snapshotAnchors() {
		a.refreshSnapshotAnchor(ctx, id, force)
		return
	}
	ts, ok := a.Store.FindSession(id)
	if !ok || ts.TmuxPaneID == "" || ts.State == state.TerminalClosed || ts.State == state.TerminalLost || (!force && !ts.WatchEnabled) {
		return
	}
	if !force && time.Since(ts.LastAnchorEditAt) < 10*time.Second {
		return
	}
	releaseConversation, acquired := a.acquireConversation(ctx, id)
	if !acquired {
		return
	}
	defer releaseConversation()
	tctx, cancel := tmux.TimeoutContext(ctx)
	defer cancel()
	identityLock := a.sessionMutex(id)
	identityLock.Lock()
	current, currentOK := a.Store.FindSession(id)
	if !currentOK || current.State == state.TerminalClosed || current.State == state.TerminalLost {
		identityLock.Unlock()
		return
	}
	if err := a.validateSessionPane(tctx, current); err != nil {
		identityLock.Unlock()
		return
	}
	ts, ok = a.Store.FindSession(id)
	identityLock.Unlock()
	if !ok {
		return
	}
	if !acquireSlot(tctx, a.captureSlots) {
		return
	}
	capture, err := a.captureStyled(tctx, ts, terminalshot.TargetRows)
	releaseSlot(a.captureSlots)
	if err != nil {
		if ctx.Err() != nil {
			_ = a.audit("tmux.capture", "canceled", map[string]any{"session_id": id, "pane_id": ts.TmuxPaneID})
			return
		}
		validationCtx, cancelValidation := tmux.TimeoutContext(ctx)
		defer cancelValidation()
		lock := a.sessionMutex(id)
		lock.Lock()
		latest, found := a.Store.FindSession(id)
		if found && latest.State != state.TerminalClosed && latest.State != state.TerminalLost {
			if validationErr := a.validateSessionPane(validationCtx, latest); validationErr == nil {
				_ = a.audit("tmux.capture", "failed", map[string]any{"session_id": id, "pane_id": latest.TmuxPaneID, "error": err.Error()})
			}
		}
		lock.Unlock()
		return
	}
	presentationText := a.processCapturedFrame(ctx, ts, capture)
	hash := sha(presentationText)
	if hash == ts.LastRawCaptureHash {
		if !force {
			return
		}
	}
	summary, turn, guideErr := a.conversationalSummary(ctx, ts, capture, presentationText)
	if errors.Is(guideErr, errConversationTurnSuperseded) {
		return
	}
	if a.snapshotAnchors() {
		return
	}
	if !a.conversationTurnCurrent(ts, turn) {
		return
	}
	if guideErr != nil {
		if stateErr := a.Store.NoteHaiku(guideErr.Error()); stateErr != nil {
			_ = a.audit("state.haiku", "failed", map[string]any{"session_id": id, "error": stateErr.Error()})
		}
		if ts.LastSummary != "" {
			summary = ts.LastSummary + "\n\n[summary stale: " + guideErr.Error() + "]"
		} else {
			summary = "summary unavailable: " + guideErr.Error()
		}
	} else {
		if stateErr := a.Store.NoteHaiku(""); stateErr != nil {
			_ = a.audit("state.haiku", "failed", map[string]any{"session_id": id, "error": stateErr.Error()})
		}
	}
	lock := a.sessionMutex(id)
	lock.Lock()
	latest, found := a.Store.FindSession(id)
	if !found || latest.State == state.TerminalClosed || latest.State == state.TerminalLost || latest.TmuxServerID != ts.TmuxServerID || latest.TmuxPaneID != ts.TmuxPaneID || latest.TmuxWindowID != ts.TmuxWindowID {
		lock.Unlock()
		return
	}
	if _, found, applied, err := a.updateSessionIfCurrent(ts, func(s *state.TerminalSession) {
		s.LastRawCapture = tailUTF8(presentationText, maxStoredVisibleCaptureBytes)
	}); err != nil || !found || !applied {
		lock.Unlock()
		if err != nil {
			_ = a.audit("state.session", "failed", map[string]any{"session_id": id, "error": err.Error()})
			a.updateAnchorLocal(ctx, id, "state update error after refresh: "+err.Error(), true)
		}
		return
	}
	lock.Unlock()
	guard := func() bool { return !a.snapshotAnchors() && a.conversationTurnCurrent(ts, turn) }
	accepted := func() bool {
		_, found, applied, err := a.updateSessionIfCurrent(ts, func(s *state.TerminalSession) {
			s.LastRawCaptureHash = hash
			if guideErr == nil {
				s.LastSummary = summary
			}
		})
		committed := found && applied && (err == nil || state.PersistenceReachedReplacement(err))
		if err != nil {
			outcome := "failed"
			if committed {
				outcome = "durability_uncertain"
			}
			_ = a.audit("state.session", outcome, map[string]any{"session_id": id, "error": err.Error()})
		}
		if !committed {
			return false
		}
		return guideErr != nil || a.commitConversationTurn(ts, turn, summary)
	}
	a.updateAnchorLocalGuarded(ctx, id, summary, force, guard, accepted)
}

func tailUTF8(text string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(text) <= maxBytes {
		return text
	}
	start := len(text) - maxBytes
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	return text[start:]
}

func headUTF8(text string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(text) <= maxBytes {
		return text
	}
	end := maxBytes
	for end > 0 && !utf8.ValidString(text[:end]) {
		end--
	}
	return text[:end]
}

func (a *App) conversationalSummary(ctx context.Context, session state.TerminalSession, capture tmux.StyledCapture, presentationText string) (string, conversationTurn, error) {
	turn := a.prepareConversationTurn(session, capture, presentationText)
	if !acquireSlot(ctx, a.haikuSlots) {
		return "", turn, ctx.Err()
	}
	defer releaseSlot(a.haikuSlots)
	identityLock := a.sessionMutex(session.ID)
	identityLock.Lock()
	defer identityLock.Unlock()
	a.presentationMu.RLock()
	defer a.presentationMu.RUnlock()
	latest, ok := a.Store.FindSession(session.ID)
	if a.snapshotAnchors() || !ok || latest.State != state.TerminalRunning || !latest.WatchEnabled || !sameTerminalBinding(latest, session) || !a.conversationTurnCurrent(session, turn) {
		return "", turn, errConversationTurnSuperseded
	}
	summary, err := a.Anthropic.Converse(ctx, turn.input)
	if err != nil {
		return "", turn, err
	}
	summary = a.redactText(summary)
	return summary, turn, nil
}

func (a *App) snapshotConversationalSummary(ctx context.Context, session state.TerminalSession, anchorMessageID int, presentationText string) (string, error) {
	if !acquireSlot(ctx, a.haikuSlots) {
		return "", ctx.Err()
	}
	defer releaseSlot(a.haikuSlots)
	identityLock := a.sessionMutex(session.ID)
	identityLock.Lock()
	defer identityLock.Unlock()
	a.presentationMu.RLock()
	defer a.presentationMu.RUnlock()
	latest, ok := a.Store.FindSession(session.ID)
	if !a.snapshotAnchors() || !ok || latest.State != state.TerminalRunning || !latest.WatchEnabled || !sameTerminalBinding(latest, session) || latest.AnchorMessageID != anchorMessageID || latest.AnchorFormat != "snapshot" || latest.RetiringAnchorMessageID != 0 {
		return "", errConversationTurnSuperseded
	}
	summary, err := a.Anthropic.Converse(ctx, anthropic.ConversationInput{SessionID: session.ID, VisibleText: presentationText})
	if err != nil {
		return "", err
	}
	return a.redactText(summary), nil
}

func (a *App) updateAnchorLocal(ctx context.Context, id int, summary string, final bool) bool {
	return a.updateAnchorLocalGuarded(ctx, id, summary, final, nil, nil)
}

func (a *App) updateAnchorLocalGuarded(ctx context.Context, id int, summary string, final bool, guard, accepted func() bool) bool {
	a.presentationMu.RLock()
	defer a.presentationMu.RUnlock()
	lock := a.anchorMutex(id)
	lock.Lock()
	defer lock.Unlock()
	finish := func() bool {
		if guard != nil && !guard() {
			return false
		}
		return accepted == nil || accepted()
	}
	a.finishAnchorRotationLocked(ctx, id)
	if guard != nil && !guard() {
		return false
	}
	ts, ok := a.Store.FindSession(id)
	if !ok || ts.AnchorMessageID == 0 || ts.RetiringAnchorMessageID != 0 {
		return false
	}
	if ts.State == state.TerminalClosed || ts.State == state.TerminalLost {
		if !a.snapshotAnchors() || ts.AnchorFormat != "snapshot" {
			summary = firstNonEmpty(ts.LastSummary, summary)
		}
		final = true
	}
	if a.snapshotAnchors() && ts.AnchorFormat == "snapshot" {
		a.updateSnapshotAnchorCaptionLocked(ctx, ts, summary, final)
		return false
	}
	rendered := a.renderLocal(ts, summary)
	renderHash := sha(rendered)
	if !a.snapshotAnchors() && ts.AnchorFormat == "snapshot" {
		a.rotateSnapshotAnchorToTextLocked(ctx, ts, rendered, renderHash, guard)
		updated, found := a.Store.FindSession(id)
		return found && updated.AnchorFormat == "text" && updated.LastRenderHash == renderHash && finish()
	}
	if renderHash == ts.LastRenderHash && !final {
		return finish()
	}
	if !final && time.Since(ts.LastAnchorEditAt) < 10*time.Second {
		return false
	}
	markup := a.anchorMarkup(ts)
	_, err := a.editAnchor(ctx, ts.AnchorChatID, ts.AnchorMessageID, rendered, markup)
	if err != nil {
		if telegram.IsRateLimited(err) {
			_ = a.audit("telegram.anchor_edit", "rate_limited", map[string]any{"session_id": id, "error": err.Error()})
			return false
		}
		if !isTelegramAnchorUnavailable(err) {
			_ = a.audit("telegram.anchor_edit", "failed", map[string]any{"session_id": id, "error": err.Error()})
			return false
		}
		msg, sendErr := a.sendAnchor(ctx, a.Config.TelegramChatID, rendered, 0, markup)
		if sendErr == nil {
			if guard != nil && !guard() {
				cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				_ = a.Telegram.DeleteMessage(cleanupCtx, msg.Chat.ID, msg.MessageID)
				cancel()
				return false
			}
			oldID := ts.AnchorMessageID
			oldFormat := firstNonEmpty(ts.AnchorFormat, "text")
			applied := false
			updated, found, err := a.Store.UpdateSession(id, func(s *state.TerminalSession) {
				if s.AnchorMessageID != oldID || s.RetiringAnchorMessageID != 0 || guard != nil && (s.State != state.TerminalRunning || s.TmuxServerID != ts.TmuxServerID || s.TmuxWindowID != ts.TmuxWindowID || s.TmuxPaneID != ts.TmuxPaneID) {
					return
				}
				s.AnchorChatID = msg.Chat.ID
				s.AnchorMessageID = msg.MessageID
				s.AnchorFormat = "text"
				s.RetiringAnchorMessageID = oldID
				s.RetiringAnchorFormat = oldFormat
				s.AnchorPinned = false
				s.AnchorPinKnown = false
				s.LastRenderHash = renderHash
				s.LastAnchorEditAt = time.Now().UTC()
				applied = true
			})
			committed := found && applied && (err == nil || state.PersistenceReachedReplacement(err))
			if err != nil {
				outcome := "failed"
				if committed {
					outcome = "durability_uncertain"
				}
				_ = a.audit("state.session", outcome, map[string]any{"session_id": id, "error": err.Error()})
			}
			if committed && anchorShouldBePinned(updated) {
				a.ensureCurrentAnchorPinnedLocked(ctx, updated)
			}
			if committed {
				a.finishAnchorRotationLocked(ctx, id)
				return finish()
			}
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			_ = a.Telegram.DeleteMessage(cleanupCtx, msg.Chat.ID, msg.MessageID)
			cancel()
		} else {
			_ = a.audit("telegram.anchor_replacement", "failed", map[string]any{"session_id": id, "error": sendErr.Error()})
		}
		return false
	}
	if guard != nil && !guard() {
		return false
	}
	applied := false
	if _, _, err := a.Store.UpdateSession(id, func(s *state.TerminalSession) {
		if s.AnchorMessageID != ts.AnchorMessageID || s.RetiringAnchorMessageID != 0 || guard != nil && (s.State != state.TerminalRunning || s.TmuxServerID != ts.TmuxServerID || s.TmuxWindowID != ts.TmuxWindowID || s.TmuxPaneID != ts.TmuxPaneID) {
			return
		}
		s.LastRenderHash = renderHash
		s.LastAnchorEditAt = time.Now().UTC()
		applied = true
	}); err != nil {
		_ = a.audit("state.session", "failed", map[string]any{"session_id": id, "error": err.Error()})
		return false
	}
	return applied && finish()
}

func (a *App) refreshSoon(id int) {
	a.queueRefresh(id, true, summaryQuietPeriod)
}

func (a *App) queueRefresh(id int, force bool, delay time.Duration) {
	ctx := a.workerContext()
	select {
	case <-ctx.Done():
		return
	default:
	}
	a.summaryMu.Lock()
	a.ensureSummaryQueuesLocked()
	due := time.Now().Add(delay)
	if a.summaryRunning[id] || a.summaryQueued[id] {
		a.summaryQueued[id] = true
		a.summaryForce[id] = a.summaryForce[id] || force
		if force && delay == 0 || delay > 0 && due.After(a.summaryDue[id]) {
			a.summaryDue[id] = due
		}
		a.summaryMu.Unlock()
		return
	}
	a.summaryQueued[id] = true
	a.summaryForce[id] = force
	a.summaryDue[id] = due
	a.summaryMu.Unlock()
	a.refreshWG.Add(1)
	go func() {
		defer a.refreshWG.Done()
		a.refreshWorker(ctx, id)
	}()
}

func (a *App) queueManualRefresh(id int) {
	a.resetConversationEpoch(id)
	if !a.snapshotAnchors() {
		a.queueRefresh(id, true, 0)
		return
	}
	a.summaryMu.Lock()
	a.ensureSummaryQueuesLocked()
	a.manualRefresh[id] = true
	a.summaryMu.Unlock()
	a.queueRefresh(id, true, 0)
}

func (a *App) consumeManualRefresh(id int) bool {
	a.summaryMu.Lock()
	defer a.summaryMu.Unlock()
	a.ensureSummaryQueuesLocked()
	manual := a.manualRefresh[id]
	delete(a.manualRefresh, id)
	return manual
}

func (a *App) refreshWorker(ctx context.Context, id int) {
	for {
		a.summaryMu.Lock()
		a.ensureSummaryQueuesLocked()
		delay := time.Until(a.summaryDue[id])
		a.summaryMu.Unlock()
		if delay > 0 && !a.sleepContext(ctx, delay) {
			a.clearRefreshQueue(id)
			return
		}
		a.summaryMu.Lock()
		a.ensureSummaryQueuesLocked()
		if remaining := time.Until(a.summaryDue[id]); remaining > 0 && a.sleepHook == nil {
			a.summaryMu.Unlock()
			continue
		}
		force := a.summaryForce[id]
		a.summaryQueued[id] = false
		a.summaryForce[id] = false
		a.summaryRunning[id] = true
		a.summaryMu.Unlock()

		refreshCtx, cancel := context.WithTimeout(ctx, 110*time.Second)
		a.runRefresh(refreshCtx, id, force)
		cancel()

		a.summaryMu.Lock()
		a.summaryRunning[id] = false
		if !a.summaryQueued[id] {
			delete(a.summaryQueued, id)
			delete(a.summaryForce, id)
			delete(a.summaryRunning, id)
			delete(a.summaryDue, id)
			delete(a.manualRefresh, id)
			a.summaryMu.Unlock()
			return
		}
		a.summaryMu.Unlock()
	}
}

func (a *App) clearRefreshQueue(id int) {
	a.summaryMu.Lock()
	defer a.summaryMu.Unlock()
	delete(a.summaryQueued, id)
	delete(a.summaryForce, id)
	delete(a.summaryRunning, id)
	delete(a.summaryDue, id)
	delete(a.manualRefresh, id)
}

func (a *App) sleep(delay time.Duration) {
	if a.sleepHook != nil {
		a.sleepHook(delay)
		return
	}
	time.Sleep(delay)
}

func (a *App) sleepContext(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return true
	}
	if a.sleepHook != nil {
		a.sleepHook(delay)
		return ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (a *App) workerContext() context.Context {
	if a.runCtx != nil {
		return a.runCtx
	}
	return context.Background()
}

func acquireSlot(ctx context.Context, slots chan struct{}) bool {
	if slots == nil {
		return true
	}
	select {
	case slots <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func releaseSlot(slots chan struct{}) {
	if slots != nil {
		<-slots
	}
}

func (a *App) runRefresh(ctx context.Context, id int, force bool) {
	if a.refreshHook != nil {
		a.refreshHook(ctx, id, force)
		return
	}
	a.refreshSession(ctx, id, force)
}

func (a *App) ensureSummaryQueuesLocked() {
	if a.summaryQueued == nil {
		a.summaryQueued = map[int]bool{}
	}
	if a.summaryRunning == nil {
		a.summaryRunning = map[int]bool{}
	}
	if a.summaryForce == nil {
		a.summaryForce = map[int]bool{}
	}
	if a.summaryDue == nil {
		a.summaryDue = map[int]time.Time{}
	}
	if a.manualRefresh == nil {
		a.manualRefresh = map[int]bool{}
	}
}

func (a *App) sendAnchor(ctx context.Context, chatID int64, text string, replyTo int, markup *telegram.InlineKeyboardMarkup) (telegram.Message, error) {
	html := telegram.MarkdownToHTML(text)
	msg, err := a.Telegram.SendHTMLMessage(ctx, chatID, html, replyTo, markup)
	if err == nil {
		return msg, nil
	}
	_ = a.audit("telegram.anchor_html", "failed", err.Error())
	if telegram.IsRateLimited(err) || !isTelegramFormattingError(err) {
		return telegram.Message{}, err
	}
	return a.Telegram.SendMessage(ctx, chatID, text, replyTo, markup)
}

func (a *App) editAnchor(ctx context.Context, chatID int64, messageID int, text string, markup *telegram.InlineKeyboardMarkup) (telegram.Message, error) {
	html := telegram.MarkdownToHTML(text)
	msg, err := a.Telegram.EditHTMLMessage(ctx, chatID, messageID, html, markup)
	if err == nil {
		return msg, nil
	}
	if telegram.IsMessageNotModified(err) {
		return telegram.Message{MessageID: messageID, Chat: telegram.Chat{ID: chatID}}, nil
	}
	_ = a.audit("telegram.anchor_html", "failed", err.Error())
	if telegram.IsRateLimited(err) || !isTelegramFormattingError(err) {
		return telegram.Message{}, err
	}
	msg, err = a.Telegram.EditMessage(ctx, chatID, messageID, text, markup)
	if telegram.IsMessageNotModified(err) {
		return telegram.Message{MessageID: messageID, Chat: telegram.Chat{ID: chatID}}, nil
	}
	return msg, err
}

func isTelegramFormattingError(err error) bool {
	var telegramErr *telegram.Error
	if !errors.As(err, &telegramErr) {
		return false
	}
	description := strings.ToLower(telegramErr.Description)
	return strings.Contains(description, "parse entities") || strings.Contains(description, "can't parse") || strings.Contains(description, "unsupported start tag")
}

func isTelegramAnchorUnavailable(err error) bool {
	var telegramErr *telegram.Error
	if !errors.As(err, &telegramErr) || (telegramErr.ErrorCode != 400 && telegramErr.StatusCode != 400) {
		return false
	}
	description := strings.ToLower(telegramErr.Description)
	return strings.Contains(description, "message to edit not found") ||
		strings.Contains(description, "message can't be edited") ||
		strings.Contains(description, "message can not be edited") ||
		strings.Contains(description, "message is too old")
}

func anchorMarkup(ts state.TerminalSession) *telegram.InlineKeyboardMarkup {
	if ts.State == state.TerminalClosed {
		return nil
	}
	if ts.State == state.TerminalLost {
		return telegram.RecoverMarkup(ts.ID)
	}
	return telegram.AnchorMarkup(ts.ID, false, false)
}

func (a *App) anchorMarkup(ts state.TerminalSession) *telegram.InlineKeyboardMarkup {
	if ts.State == state.TerminalRunning {
		return telegram.AnchorMarkup(ts.ID, ts.AnchorFormat != "snapshot" && a.snapshotReady, ts.AnchorFormat == "snapshot" && a.haikuAvailable)
	}
	return anchorMarkup(ts)
}

func (a *App) scheduler(ctx context.Context) {
	for _, ts := range a.Store.Snapshot().TerminalSessions {
		if ts.AnchorMessageID != 0 {
			a.reconcileAnchorControls(ctx, ts.ID)
		}
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	nextCapture := map[int]time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			st := a.Store.Snapshot()
			now := time.Now()
			for _, ts := range st.TerminalSessions {
				if ts.AnchorMessageID != 0 {
					a.reconcileAnchorPresentation(ctx, ts.ID)
				}
				if !ts.WatchEnabled || ts.State == state.TerminalClosed || ts.State == state.TerminalLost {
					delete(nextCapture, ts.ID)
					continue
				}
				if now.Before(nextCapture[ts.ID]) {
					continue
				}
				interval := 10 * time.Second
				if !ts.LastActivityAt.IsZero() && now.Sub(ts.LastActivityAt) > 5*time.Minute {
					interval = 30 * time.Second
				}
				nextCapture[ts.ID] = now.Add(interval)
				a.queueRefresh(ts.ID, false, 0)
			}
		}
	}
}
