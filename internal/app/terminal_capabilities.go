package app

import (
	"context"

	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/tmux"
)

func (a *App) advertiseTerminalCapabilities(ctx context.Context, session state.TerminalSession) {
	if a.Tmux.Runner == nil || session.State != state.TerminalRunning || !session.WatchEnabled || session.TmuxPaneID == "" {
		return
	}
	tmuxCtx, cancel := tmux.TimeoutContext(ctx)
	defer cancel()
	if err := a.terminalMechanics().AdvertiseEngram(tmuxCtx, terminalBinding(session), session.ID); err != nil {
		a.recordIdentityLoss(ctx, session, err)
		_ = a.audit("tmux.capabilities", "advertise_failed", map[string]any{"session_id": session.ID, "pane_id": session.TmuxPaneID, "error": err.Error()})
		return
	}
	_ = a.audit("tmux.capabilities", "advertised", map[string]any{"session_id": session.ID, "pane_id": session.TmuxPaneID})
}

func (a *App) clearTerminalCapabilities(ctx context.Context, session state.TerminalSession) {
	if a.Tmux.Runner == nil || session.TmuxPaneID == "" {
		return
	}
	tmuxCtx, cancel := tmux.TimeoutContext(ctx)
	defer cancel()
	if err := a.terminalMechanics().ClearEngramAdvertisement(tmuxCtx, terminalBinding(session)); err != nil {
		if !tmux.IsIdentityLoss(err) {
			_ = a.audit("tmux.capabilities", "clear_failed", map[string]any{"session_id": session.ID, "pane_id": session.TmuxPaneID, "error": err.Error()})
		}
		return
	}
	_ = a.audit("tmux.capabilities", "cleared", map[string]any{"session_id": session.ID, "pane_id": session.TmuxPaneID})
}
