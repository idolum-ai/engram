package app

import (
	"context"
	"strings"
	"time"

	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/tmux"
)

func (a *App) sendInput(ctx context.Context, id int, text, mode string, enter bool) {
	ts, ok := a.Store.FindSession(id)
	if !ok {
		return
	}
	tctx, cancel := tmux.TimeoutContext(ctx)
	defer cancel()
	var err error
	if enter {
		err = a.Tmux.SendCommand(tctx, ts.TmuxPaneID, text)
	} else {
		err = a.Tmux.SendText(tctx, ts.TmuxPaneID, text)
	}
	if err != nil {
		_ = a.audit("tmux.send", "failed", map[string]any{"session_id": id, "pane_id": ts.TmuxPaneID, "mode": mode, "enter": enter, "error": err.Error()})
		a.updateAnchorLocal(ctx, id, "tmux send error: "+err.Error(), true)
		return
	}
	_ = a.audit("tmux.send", "ok", map[string]any{"session_id": id, "pane_id": ts.TmuxPaneID, "mode": mode, "enter": enter})
	if _, _, err := a.Store.UpdateSession(id, func(s *state.TerminalSession) {
		s.LastInputPreview = preview(text)
		s.LastInputMode = mode
		s.LastActivityAt = time.Now().UTC()
		s.PendingRefresh = true
	}); err != nil {
		_ = a.audit("state.session", "failed", map[string]any{"session_id": id, "mode": mode, "error": err.Error()})
		a.updateAnchorLocal(ctx, id, "state update error after tmux input: "+err.Error(), true)
		return
	}
	a.refreshSoon(id)
}

func (a *App) sendKeys(ctx context.Context, id int, keys []string) {
	a.sendKeyGroups(ctx, id, [][]string{keys}, strings.Join(keys, " "), 0)
}

func (a *App) sendKeyGroups(ctx context.Context, id int, groups [][]string, preview string, delay time.Duration) {
	if len(groups) == 0 {
		a.updateAnchorLocal(ctx, id, "missing keys", true)
		return
	}
	for _, keys := range groups {
		if err := tmux.ValidKeys(keys); err != nil {
			a.updateAnchorLocal(ctx, id, err.Error(), true)
			return
		}
	}
	ts, ok := a.Store.FindSession(id)
	if !ok {
		return
	}
	tctx, cancel := tmux.TimeoutContext(ctx)
	defer cancel()
	for i, keys := range groups {
		if err := a.Tmux.SendKeys(tctx, ts.TmuxPaneID, keys); err != nil {
			a.updateAnchorLocal(ctx, id, "tmux key error: "+err.Error(), true)
			return
		}
		if delay > 0 && i < len(groups)-1 {
			a.sleep(delay)
		}
	}
	if _, _, err := a.Store.UpdateSession(id, func(s *state.TerminalSession) {
		s.LastInputPreview = firstNonEmpty(strings.TrimSpace(preview), flattenKeyPreview(groups))
		s.LastInputMode = "keys"
		s.LastActivityAt = time.Now().UTC()
		s.PendingRefresh = true
	}); err != nil {
		_ = a.audit("state.session", "failed", map[string]any{"session_id": id, "mode": "keys", "error": err.Error()})
		a.updateAnchorLocal(ctx, id, "state update error after tmux keys: "+err.Error(), true)
		return
	}
	a.refreshSoon(id)
}

func flattenKeyPreview(groups [][]string) string {
	var keys []string
	for _, group := range groups {
		keys = append(keys, group...)
	}
	return strings.Join(keys, " ")
}
