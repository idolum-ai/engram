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
	"github.com/idolum-ai/engram/internal/tmux"
)

const maxStoredVisibleCaptureBytes = 16 * 1024

func (a *App) refreshSession(ctx context.Context, id int, force bool) {
	ts, ok := a.Store.FindSession(id)
	if !ok || ts.TmuxPaneID == "" || ts.State == state.TerminalClosed || ts.State == state.TerminalLost || (!force && !ts.WatchEnabled) {
		return
	}
	if !force && time.Since(ts.LastAnchorEditAt) < 10*time.Second {
		return
	}
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
	ts, _ = a.Store.FindSession(id)
	identityLock.Unlock()
	if !acquireSlot(tctx, a.captureSlots) {
		return
	}
	capture, err := a.Tmux.CaptureVisibleSemantic(tctx, ts.TmuxPaneID)
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
		if found && latest.State != state.TerminalClosed {
			if validationErr := a.validateSessionPane(validationCtx, latest); validationErr == nil {
				_ = a.audit("tmux.capture", "failed", map[string]any{"session_id": id, "pane_id": latest.TmuxPaneID, "error": err.Error()})
			}
		}
		lock.Unlock()
		return
	}
	hash := sha(capture)
	if hash == ts.LastRawCaptureHash {
		transition := handoffUnchanged
		if ts.HandoffCandidate != nil || ts.Handoff != nil && !ts.Handoff.AcknowledgedAt.IsZero() {
			if _, _, stateErr := a.Store.UpdateSession(id, func(s *state.TerminalSession) {
				transition = settleHandoffCandidate(s, hash, time.Now().UTC())
			}); stateErr != nil {
				_ = a.audit("state.handoff", "failed", map[string]any{"session_id": id, "error": stateErr.Error()})
				return
			}
		}
		a.auditHandoffTransition(id, transition, hash)
		if transition != handoffUnchanged {
			a.updateAnchorLocal(ctx, id, "", true)
			a.ensureHandoffDelivery(ctx, id)
			return
		}
		a.ensureHandoffDelivery(ctx, id)
		if !force {
			return
		}
	}
	report, guideErr := a.guideSummary(ctx, ts, capture, ts.LastRawCaptureHash != "", hash != ts.LastRawCaptureHash)
	summary := ""
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
		summary = report.TelegramText()
		if stateErr := a.Store.NoteHaiku(""); stateErr != nil {
			_ = a.audit("state.haiku", "failed", map[string]any{"session_id": id, "error": stateErr.Error()})
		}
	}
	lock := a.sessionMutex(id)
	lock.Lock()
	latest, found := a.Store.FindSession(id)
	if !found || latest.State == state.TerminalClosed || latest.State == state.TerminalLost || latest.TmuxPaneID != ts.TmuxPaneID || latest.TmuxWindowID != ts.TmuxWindowID {
		lock.Unlock()
		return
	}
	transition := handoffUnchanged
	if _, _, err := a.Store.UpdateSession(id, func(s *state.TerminalSession) {
		s.LastRawCapture = tailUTF8(capture, maxStoredVisibleCaptureBytes)
		s.LastRawCaptureHash = hash
		s.LastSummary = summary
		if guideErr == nil {
			transition = observeHandoff(s, report, hash, time.Now().UTC())
		}
	}); err != nil {
		lock.Unlock()
		_ = a.audit("state.session", "failed", map[string]any{"session_id": id, "error": err.Error()})
		a.updateAnchorLocal(ctx, id, "state update error after refresh: "+err.Error(), true)
		return
	}
	lock.Unlock()
	a.auditHandoffTransition(id, transition, hash)
	a.updateAnchorLocal(ctx, id, summary, force)
	a.ensureHandoffDelivery(ctx, id)
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

func (a *App) guideSummary(ctx context.Context, ts state.TerminalSession, capture string, hasPreviousCapture, captureChanged bool) (anthropic.GuideReport, error) {
	var handoffEvidence []string
	if ts.Handoff != nil {
		handoffEvidence = append(handoffEvidence, ts.Handoff.Evidence...)
	}
	if ts.HandoffCandidate != nil {
		handoffEvidence = append(handoffEvidence, ts.HandoffCandidate.Evidence...)
	}
	visibleForHaiku, repeatedLines := a.prepareHaikuVisibleCapturePreserving(ts.ID, capture, handoffEvidence)
	input := anthropic.SummaryInput{
		SessionID:          ts.ID,
		State:              string(ts.State),
		LastInput:          ts.LastInputPreview,
		LastInputMode:      ts.LastInputMode,
		PreviousSummary:    ts.LastSummary,
		HasPreviousCapture: hasPreviousCapture,
		CaptureChanged:     captureChanged,
		VisibleCapture:     visibleForHaiku,
	}
	if ts.Handoff != nil {
		input.OpenHandoff = true
		input.HandoffKey = ts.Handoff.Key
		input.HandoffStatus = ts.Handoff.StatusReport
		input.HandoffAction = ts.Handoff.RecommendedAction
		input.HandoffEvidence = append([]string(nil), ts.Handoff.Evidence...)
		input.HandoffAcknowledged = !ts.Handoff.AcknowledgedAt.IsZero()
	}
	if !acquireSlot(ctx, a.haikuSlots) {
		return anthropic.GuideReport{}, ctx.Err()
	}
	report, err := a.Anthropic.Guide(ctx, input)
	releaseSlot(a.haikuSlots)
	if err != nil {
		return anthropic.GuideReport{}, err
	}
	if !report.WantsFullBuffer() {
		return report, nil
	}
	tctx, cancel := tmux.TimeoutContext(ctx)
	defer cancel()
	if !acquireSlot(tctx, a.captureSlots) {
		return report, nil
	}
	full, err := a.Tmux.CaptureScrollbackTail(tctx, ts.TmuxPaneID, 24_000, 800)
	releaseSlot(a.captureSlots)
	if err != nil || strings.TrimSpace(full) == "" {
		return report, nil
	}
	input.FullCapture = filterCaptureLinesPreserving(full, repeatedLines, handoffEvidence)
	if !acquireSlot(ctx, a.haikuSlots) {
		return report, nil
	}
	refined, err := a.Anthropic.Guide(ctx, input)
	releaseSlot(a.haikuSlots)
	if err != nil {
		return report, nil
	}
	return refined, nil
}

func (a *App) filterHaikuVisibleCapture(sessionID int, capture string) string {
	filtered, _ := a.prepareHaikuVisibleCapture(sessionID, capture)
	return filtered
}

func (a *App) prepareHaikuVisibleCapture(sessionID int, capture string) (string, map[string]bool) {
	return a.prepareHaikuVisibleCapturePreserving(sessionID, capture, nil)
}

func (a *App) prepareHaikuVisibleCapturePreserving(sessionID int, capture string, evidence []string) (string, map[string]bool) {
	a.captureMu.Lock()
	defer a.captureMu.Unlock()
	if a.captureHistory == nil {
		a.captureHistory = map[int][]map[string]bool{}
	}
	history := a.captureHistory[sessionID]
	repeated := map[string]bool{}
	for _, previous := range history {
		for line := range previous {
			repeated[line] = true
		}
	}
	current := map[string]bool{}
	for _, line := range strings.Split(capture, "\n") {
		current[line] = true
	}
	history = append(history, current)
	if len(history) > haikuCaptureHistoryLimit {
		history = history[len(history)-haikuCaptureHistoryLimit:]
	}
	a.captureHistory[sessionID] = history
	return filterCaptureLinesPreserving(capture, repeated, evidence), repeated
}

func filterCaptureLines(capture string, repeated map[string]bool) string {
	return filterCaptureLinesPreserving(capture, repeated, nil)
}

func filterCaptureLinesPreserving(capture string, repeated map[string]bool, evidence []string) string {
	if len(repeated) == 0 {
		return capture
	}
	lines := strings.Split(capture, "\n")
	filtered := make([]string, 0, len(lines))
	for _, line := range lines {
		if repeated[line] && !lineSupportsHandoffEvidence(line, evidence) {
			continue
		}
		filtered = append(filtered, line)
	}
	return strings.Join(filtered, "\n")
}

func lineSupportsHandoffEvidence(line string, evidence []string) bool {
	line = strings.ToLower(strings.Join(strings.Fields(line), " "))
	if line == "" {
		return false
	}
	for _, excerpt := range evidence {
		excerpt = strings.ToLower(strings.Join(strings.Fields(excerpt), " "))
		if excerpt == "" {
			continue
		}
		if strings.Contains(excerpt, line) || strings.Contains(line, excerpt) {
			return true
		}
		significant, matched := 0, 0
		for _, word := range strings.Fields(excerpt) {
			word = strings.Trim(word, "[]():;,.!?`'\"")
			if len(word) < 3 {
				continue
			}
			significant++
			if strings.Contains(line, word) {
				matched++
			}
		}
		if matched >= 2 || significant == 1 && matched == 1 {
			return true
		}
	}
	return false
}

func (a *App) clearHaikuCaptureHistory(sessionID int) {
	a.captureMu.Lock()
	defer a.captureMu.Unlock()
	delete(a.captureHistory, sessionID)
}

func (a *App) updateAnchorLocal(ctx context.Context, id int, summary string, final bool) {
	ts, ok := a.Store.FindSession(id)
	if !ok || ts.AnchorMessageID == 0 {
		return
	}
	if ts.State == state.TerminalClosed || ts.State == state.TerminalLost {
		summary = firstNonEmpty(ts.LastSummary, summary)
		final = true
	}
	rendered := renderLocal(ts, summary)
	renderHash := sha(rendered)
	if renderHash == ts.LastRenderHash && !final {
		return
	}
	if !final && time.Since(ts.LastAnchorEditAt) < 10*time.Second {
		return
	}
	markup := anchorMarkup(ts)
	_, err := a.editAnchor(ctx, ts.AnchorChatID, ts.AnchorMessageID, rendered, markup)
	if err != nil {
		if telegram.IsRateLimited(err) {
			_ = a.audit("telegram.anchor_edit", "rate_limited", map[string]any{"session_id": id, "error": err.Error()})
			return
		}
		if !isTelegramAnchorUnavailable(err) {
			_ = a.audit("telegram.anchor_edit", "failed", map[string]any{"session_id": id, "error": err.Error()})
			return
		}
		msg, sendErr := a.sendAnchor(ctx, a.Config.TelegramChatID, rendered, 0, markup)
		if sendErr == nil {
			if _, _, err := a.Store.UpdateSession(id, func(s *state.TerminalSession) {
				s.AnchorChatID = msg.Chat.ID
				s.AnchorMessageID = msg.MessageID
				s.LastRenderHash = renderHash
				s.LastAnchorEditAt = time.Now().UTC()
			}); err != nil {
				_ = a.audit("state.session", "failed", map[string]any{"session_id": id, "error": err.Error()})
			}
		} else {
			_ = a.audit("telegram.anchor_replacement", "failed", map[string]any{"session_id": id, "error": sendErr.Error()})
		}
		return
	}
	if _, _, err := a.Store.UpdateSession(id, func(s *state.TerminalSession) {
		s.LastRenderHash = renderHash
		s.LastAnchorEditAt = time.Now().UTC()
	}); err != nil {
		_ = a.audit("state.session", "failed", map[string]any{"session_id": id, "error": err.Error()})
	}
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
	return telegram.RefreshMarkup(ts.ID)
}

func (a *App) scheduler(ctx context.Context) {
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
