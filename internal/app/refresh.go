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
	capture, err := a.Tmux.CaptureVisible(tctx, ts.TmuxPaneID)
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
	report, err := a.Anthropic.Guide(ctx, input)
	if err != nil {
		return "", err
	}
	if !report.WantsFullBuffer() {
		return report.TelegramText(), nil
	}
	tctx, cancel := tmux.TimeoutContext(ctx)
	defer cancel()
	full, err := a.Tmux.CaptureFull(tctx, ts.TmuxPaneID)
	if err != nil || strings.TrimSpace(full) == "" {
		return report.TelegramText(), nil
	}
	input.FullCapture = filterCaptureLines(full, repeatedLines)
	refined, err := a.Anthropic.Guide(ctx, input)
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
	go func() {
		a.refreshWorker(id, delay)
	}()
}

func (a *App) refreshWorker(id int, delay time.Duration) {
	for {
		if delay > 0 {
			a.sleep(delay)
		}
		a.summaryMu.Lock()
		a.ensureSummaryQueuesLocked()
		force := a.summaryForce[id]
		a.summaryQueued[id] = false
		a.summaryForce[id] = false
		a.summaryRunning[id] = true
		a.summaryMu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), 110*time.Second)
		a.runRefresh(ctx, id, force)
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

func (a *App) sleep(delay time.Duration) {
	if a.sleepHook != nil {
		a.sleepHook(delay)
		return
	}
	time.Sleep(delay)
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
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			st := a.Store.Snapshot()
			for _, ts := range st.TerminalSessions {
				if ts.WatchEnabled && ts.State != state.TerminalClosed {
					a.queueRefresh(ts.ID, false, 0)
				}
			}
		}
	}
}
