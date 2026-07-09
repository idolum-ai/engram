package app

import (
	"context"
	"strings"
	"time"

	"github.com/idolum-ai/engram/internal/anthropic"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
	"github.com/idolum-ai/engram/internal/tmux"
)

func (a *App) refreshSession(ctx context.Context, id int, force bool) {
	ts, ok := a.Store.FindSession(id)
	if !ok || ts.TmuxPaneID == "" {
		return
	}
	if !force && time.Since(ts.LastAnchorEditAt) < 10*time.Second {
		return
	}
	tctx, cancel := tmux.TimeoutContext(ctx)
	defer cancel()
	if !acquireSlot(tctx, a.captureSlots) {
		return
	}
	capture, err := a.Tmux.CaptureVisible(tctx, ts.TmuxPaneID)
	releaseSlot(a.captureSlots)
	if err != nil {
		if _, _, stateErr := a.Store.UpdateSession(id, func(s *state.TerminalSession) {
			s.State = state.TerminalLost
			s.LastTelegramError = err.Error()
		}); stateErr != nil {
			_ = a.audit("state.session", "failed", map[string]any{"session_id": id, "error": stateErr.Error()})
		}
		a.updateAnchorLocal(ctx, id, "lost: "+err.Error(), true)
		return
	}
	hash := sha(capture)
	if hash == ts.LastRawCaptureHash && !force {
		return
	}
	summary, err := a.guideSummary(ctx, ts, capture)
	if err != nil {
		if stateErr := a.Store.NoteHaiku(err.Error()); stateErr != nil {
			_ = a.audit("state.haiku", "failed", map[string]any{"session_id": id, "error": stateErr.Error()})
		}
		if ts.LastSummary != "" {
			summary = ts.LastSummary + "\n\n[summary stale: " + err.Error() + "]"
		} else {
			summary = "summary unavailable: " + err.Error()
		}
	} else {
		if stateErr := a.Store.NoteHaiku(""); stateErr != nil {
			_ = a.audit("state.haiku", "failed", map[string]any{"session_id": id, "error": stateErr.Error()})
		}
	}
	if _, _, err := a.Store.UpdateSession(id, func(s *state.TerminalSession) {
		s.LastRawCapture = capture
		s.LastRawCaptureHash = hash
		s.LastSummary = summary
		s.LastSummaryHash = sha(summary)
		s.LastSummaryModel = a.Config.AnthropicModel
		s.PendingRefresh = false
	}); err != nil {
		_ = a.audit("state.session", "failed", map[string]any{"session_id": id, "error": err.Error()})
		a.updateAnchorLocal(ctx, id, "state update error after refresh: "+err.Error(), true)
		return
	}
	a.updateAnchorLocal(ctx, id, summary, force)
}

func (a *App) guideSummary(ctx context.Context, ts state.TerminalSession, capture string) (string, error) {
	visibleForHaiku, repeatedLines := a.prepareHaikuVisibleCapture(ts.ID, capture)
	input := anthropic.SummaryInput{
		SessionID:       ts.ID,
		State:           string(ts.State),
		LastInput:       ts.LastInputPreview,
		LastInputMode:   ts.LastInputMode,
		PreviousSummary: ts.LastSummary,
		VisibleCapture:  visibleForHaiku,
	}
	if !acquireSlot(ctx, a.haikuSlots) {
		return "", ctx.Err()
	}
	report, err := a.Anthropic.Guide(ctx, input)
	releaseSlot(a.haikuSlots)
	if err != nil {
		return "", err
	}
	if !report.WantsFullBuffer() {
		return report.TelegramText(), nil
	}
	tctx, cancel := tmux.TimeoutContext(ctx)
	defer cancel()
	if !acquireSlot(tctx, a.captureSlots) {
		return report.TelegramText(), nil
	}
	full, err := a.Tmux.CaptureFull(tctx, ts.TmuxPaneID)
	releaseSlot(a.captureSlots)
	if err != nil || strings.TrimSpace(full) == "" {
		return report.TelegramText(), nil
	}
	input.FullCapture = filterCaptureLines(full, repeatedLines)
	if !acquireSlot(ctx, a.haikuSlots) {
		return report.TelegramText(), nil
	}
	refined, err := a.Anthropic.Guide(ctx, input)
	releaseSlot(a.haikuSlots)
	if err != nil {
		return report.TelegramText(), nil
	}
	return refined.TelegramText(), nil
}

func (a *App) filterHaikuVisibleCapture(sessionID int, capture string) string {
	filtered, _ := a.prepareHaikuVisibleCapture(sessionID, capture)
	return filtered
}

func (a *App) prepareHaikuVisibleCapture(sessionID int, capture string) (string, map[string]bool) {
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
	return filterCaptureLines(capture, repeated), repeated
}

func filterCaptureLines(capture string, repeated map[string]bool) string {
	if len(repeated) == 0 {
		return capture
	}
	lines := strings.Split(capture, "\n")
	filtered := make([]string, 0, len(lines))
	for _, line := range lines {
		if repeated[line] {
			continue
		}
		filtered = append(filtered, line)
	}
	return strings.Join(filtered, "\n")
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
	rendered := renderLocal(ts, summary)
	renderHash := sha(rendered)
	if renderHash == ts.LastRenderHash && !final {
		return
	}
	if !final && time.Since(ts.LastAnchorEditAt) < 10*time.Second {
		return
	}
	_, err := a.editAnchor(ctx, ts.AnchorChatID, ts.AnchorMessageID, rendered, telegram.RefreshMarkup(id))
	if err != nil {
		msg, sendErr := a.sendAnchor(ctx, a.Config.TelegramChatID, rendered, 0, telegram.RefreshMarkup(id))
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
	if a.summaryRunning[id] || a.summaryQueued[id] {
		a.summaryQueued[id] = true
		a.summaryForce[id] = a.summaryForce[id] || force
		a.summaryMu.Unlock()
		return
	}
	a.summaryQueued[id] = true
	a.summaryForce[id] = force
	a.summaryMu.Unlock()
	a.refreshWG.Add(1)
	go func() {
		defer a.refreshWG.Done()
		a.refreshWorker(ctx, id, delay)
	}()
}

func (a *App) refreshWorker(ctx context.Context, id int, delay time.Duration) {
	for {
		if delay > 0 && !a.sleepContext(ctx, delay) {
			a.clearRefreshQueue(id)
			return
		}
		a.summaryMu.Lock()
		a.ensureSummaryQueuesLocked()
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
			a.summaryMu.Unlock()
			return
		}
		a.summaryMu.Unlock()
		delay = summaryQuietPeriod
	}
}

func (a *App) clearRefreshQueue(id int) {
	a.summaryMu.Lock()
	defer a.summaryMu.Unlock()
	delete(a.summaryQueued, id)
	delete(a.summaryForce, id)
	delete(a.summaryRunning, id)
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
}

func (a *App) sendAnchor(ctx context.Context, chatID int64, text string, replyTo int, markup *telegram.InlineKeyboardMarkup) (telegram.Message, error) {
	html := telegram.MarkdownToHTML(text)
	msg, err := a.Telegram.SendHTMLMessage(ctx, chatID, html, replyTo, markup)
	if err == nil {
		return msg, nil
	}
	_ = a.audit("telegram.anchor_html", "failed", err.Error())
	return a.Telegram.SendMessage(ctx, chatID, text, replyTo, markup)
}

func (a *App) editAnchor(ctx context.Context, chatID int64, messageID int, text string, markup *telegram.InlineKeyboardMarkup) (telegram.Message, error) {
	html := telegram.MarkdownToHTML(text)
	msg, err := a.Telegram.EditHTMLMessage(ctx, chatID, messageID, html, markup)
	if err == nil {
		return msg, nil
	}
	_ = a.audit("telegram.anchor_html", "failed", err.Error())
	return a.Telegram.EditMessage(ctx, chatID, messageID, text, markup)
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
