// Package mechanics owns pane-bound terminal operations whose truth comes from
// tmux. It deliberately knows nothing about Telegram, presentation, or state.
package mechanics

import (
	"context"
	"fmt"
	"io"

	"github.com/idolum-ai/engram/internal/tmux"
)

// Binding identifies one pane for the lifetime of its tmux server.
type Binding struct {
	PaneID   string
	WindowID string
}

// Controller validates immutable identity immediately before every pane-bound
// observation or effect.
type Controller struct {
	tmux tmux.Manager
}

func New(manager tmux.Manager) Controller {
	return Controller{tmux: manager}
}

func (c Controller) Validate(ctx context.Context, binding Binding) (tmux.Pane, error) {
	return c.tmux.ValidatePane(ctx, binding.PaneID, binding.WindowID)
}

func (c Controller) SendCommand(ctx context.Context, binding Binding, text string) (tmux.Pane, error) {
	pane, err := c.Validate(ctx, binding)
	if err != nil {
		return tmux.Pane{}, err
	}
	if err := c.tmux.SendCommand(ctx, binding.PaneID, text); err != nil {
		return tmux.Pane{}, err
	}
	return pane, nil
}

func (c Controller) SendText(ctx context.Context, binding Binding, text string) (tmux.Pane, error) {
	pane, err := c.Validate(ctx, binding)
	if err != nil {
		return tmux.Pane{}, err
	}
	if err := c.tmux.SendText(ctx, binding.PaneID, text); err != nil {
		return tmux.Pane{}, err
	}
	return pane, nil
}

func (c Controller) SendKeys(ctx context.Context, binding Binding, keys []string) (tmux.Pane, error) {
	if err := tmux.ValidKeys(keys); err != nil {
		return tmux.Pane{}, err
	}
	pane, err := c.Validate(ctx, binding)
	if err != nil {
		return tmux.Pane{}, err
	}
	if err := c.tmux.SendKeys(ctx, binding.PaneID, keys); err != nil {
		return tmux.Pane{}, err
	}
	return pane, nil
}

func (c Controller) CaptureStyled(ctx context.Context, binding Binding, targetRows int) (tmux.Pane, tmux.StyledCapture, error) {
	pane, err := c.Validate(ctx, binding)
	if err != nil {
		return tmux.Pane{}, tmux.StyledCapture{}, err
	}
	capture, err := c.tmux.CaptureStyled(ctx, binding.PaneID, targetRows)
	if err != nil {
		return tmux.Pane{}, tmux.StyledCapture{}, err
	}
	return pane, capture, nil
}

func (c Controller) CaptureLiteral(ctx context.Context, binding Binding, targetRows int) (tmux.Pane, string, error) {
	pane, err := c.Validate(ctx, binding)
	if err != nil {
		return tmux.Pane{}, "", err
	}
	text, err := c.tmux.CaptureLiteral(ctx, binding.PaneID, targetRows)
	if err != nil {
		return tmux.Pane{}, "", err
	}
	return pane, text, nil
}

func (c Controller) CaptureVisibleRaw(ctx context.Context, binding Binding) (tmux.Pane, string, error) {
	pane, err := c.Validate(ctx, binding)
	if err != nil {
		return tmux.Pane{}, "", err
	}
	text, err := c.tmux.CaptureVisibleRaw(ctx, binding.PaneID)
	if err != nil {
		return tmux.Pane{}, "", err
	}
	return pane, text, nil
}

func (c Controller) DumpScrollback(ctx context.Context, binding Binding, dst io.Writer) (tmux.Pane, error) {
	if dst == nil {
		return tmux.Pane{}, fmt.Errorf("missing scrollback destination")
	}
	pane, err := c.Validate(ctx, binding)
	if err != nil {
		return tmux.Pane{}, err
	}
	if err := c.tmux.DumpScrollback(ctx, binding.PaneID, dst); err != nil {
		return tmux.Pane{}, err
	}
	return pane, nil
}

func (c Controller) CloseWindow(ctx context.Context, binding Binding) (tmux.Pane, error) {
	pane, err := c.Validate(ctx, binding)
	if err != nil {
		return tmux.Pane{}, err
	}
	if err := c.tmux.KillWindow(ctx, binding.WindowID); err != nil {
		return tmux.Pane{}, err
	}
	return pane, nil
}
