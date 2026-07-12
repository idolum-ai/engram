package app

import (
	"context"
	"fmt"

	"github.com/idolum-ai/engram/internal/mechanics"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/tmux"
)

func (a *App) terminalMechanics() mechanics.Controller {
	return mechanics.New(a.Tmux)
}

func terminalBinding(session state.TerminalSession) mechanics.Binding {
	return mechanics.Binding{PaneID: session.TmuxPaneID, WindowID: session.TmuxWindowID, ServerID: session.TmuxServerID}
}

func (a *App) validateSessionPane(ctx context.Context, session state.TerminalSession) error {
	pane, err := a.terminalMechanics().Validate(ctx, terminalBinding(session))
	if err != nil {
		if tmux.IsIdentityLoss(err) {
			a.markSessionLost(ctx, session, err)
		} else {
			_ = a.audit("tmux.identity", "unavailable", map[string]any{
				"session_id": session.ID,
				"pane_id":    session.TmuxPaneID,
				"window_id":  session.TmuxWindowID,
				"error":      err.Error(),
			})
		}
		return err
	}
	return a.recordValidatedPane(session, pane)
}

func (a *App) captureStyled(ctx context.Context, session state.TerminalSession, targetRows int) (tmux.StyledCapture, error) {
	pane, capture, err := a.terminalMechanics().CaptureStyled(ctx, terminalBinding(session), targetRows)
	if err != nil {
		a.recordIdentityLoss(ctx, session, err)
		return tmux.StyledCapture{}, err
	}
	if err := a.recordValidatedPane(session, pane); err != nil {
		return tmux.StyledCapture{}, err
	}
	return capture, nil
}

func (a *App) recordIdentityLoss(ctx context.Context, session state.TerminalSession, err error) {
	if tmux.IsIdentityLoss(err) {
		a.markSessionLost(ctx, session, err)
	}
}

func (a *App) updateSessionIfCurrent(expected state.TerminalSession, fn func(*state.TerminalSession)) (state.TerminalSession, bool, bool, error) {
	applied := false
	updated, found, err := a.Store.UpdateSession(expected.ID, func(current *state.TerminalSession) {
		if current.TmuxPaneID != expected.TmuxPaneID || current.TmuxWindowID != expected.TmuxWindowID || current.TmuxServerID != expected.TmuxServerID || current.State != expected.State {
			return
		}
		fn(current)
		applied = true
	})
	return updated, found, applied, err
}

func (a *App) recordValidatedPane(session state.TerminalSession, pane tmux.Pane) error {
	if session.State == state.TerminalLost {
		_, found, applied, err := a.updateSessionIfCurrent(session, func(current *state.TerminalSession) {
			current.State = state.TerminalRunning
			current.WatchEnabled = true
			current.LastKnownCWD = pane.CurrentPath
		})
		if err != nil {
			_ = a.audit("state.session", "failed", map[string]any{"session_id": session.ID, "error": err.Error()})
			return fmt.Errorf("recover session state: %w", err)
		}
		if !found {
			return fmt.Errorf("recover session state: session no longer tracked")
		}
		if !applied {
			return fmt.Errorf("recover session state: session changed while validating")
		}
		_ = a.audit("tmux.identity", "recovered", map[string]any{
			"session_id": session.ID,
			"pane_id":    session.TmuxPaneID,
			"window_id":  session.TmuxWindowID,
		})
		return nil
	}
	if pane.CurrentPath != "" && pane.CurrentPath != session.LastKnownCWD {
		_, found, applied, err := a.updateSessionIfCurrent(session, func(current *state.TerminalSession) {
			current.LastKnownCWD = pane.CurrentPath
		})
		if err != nil {
			_ = a.audit("state.session", "failed", map[string]any{"session_id": session.ID, "error": err.Error()})
		}
		if !found {
			return fmt.Errorf("record tmux pane: session no longer tracked")
		}
		if !applied {
			return fmt.Errorf("record tmux pane: session changed while validating")
		}
	}
	return nil
}
