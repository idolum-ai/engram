package tmux

import (
	"bytes"
	"context"
	"io"
	"sync"
)

type interactiveContextKey struct{}
type backgroundContextKey struct{}

// InteractiveContext marks tmux work that originated from a user input. The
// priority runner uses it to stop a background observation before it can hold
// the tmux control plane for the entire command timeout.
func InteractiveContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, interactiveContextKey{}, true)
}

// BackgroundContext explicitly marks read-only observation or reconciliation
// work that may be canceled to make room for interactive input. Unmarked work
// is protected because tmux commands may have already applied side effects.
func BackgroundContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, backgroundContextKey{}, true)
}

func isInteractiveContext(ctx context.Context) bool {
	interactive, _ := ctx.Value(interactiveContextKey{}).(bool)
	return interactive
}

func isBackgroundContext(ctx context.Context) bool {
	background, _ := ctx.Value(backgroundContextKey{}).(bool)
	return background
}

// PriorityRunner serializes tmux client processes and lets interactive work
// preempt an active background client. tmux itself remains the source of truth;
// this only controls how Engram competes for its command queue.
type PriorityRunner struct {
	inner Runner
	token chan struct{}

	mu                     sync.Mutex
	changed                chan struct{}
	waitingProtected       int
	activeBackgroundCancel context.CancelFunc
	activeBackgroundID     uint64
	nextBackgroundID       uint64
}

func NewPriorityRunner(inner Runner) *PriorityRunner {
	runner := &PriorityRunner{
		inner:   inner,
		token:   make(chan struct{}, 1),
		changed: make(chan struct{}),
	}
	runner.token <- struct{}{}
	return runner
}

func (r *PriorityRunner) Run(ctx context.Context, args ...string) (string, error) {
	opCtx, release, err := r.acquire(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	return r.inner.Run(opCtx, args...)
}

func (r *PriorityRunner) RunToWriter(ctx context.Context, dst io.Writer, args ...string) error {
	opCtx, release, err := r.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	if stream, ok := r.inner.(StreamRunner); ok {
		return stream.RunToWriter(opCtx, dst, args...)
	}
	out, err := r.inner.Run(opCtx, args...)
	if err != nil {
		return err
	}
	_, err = io.Copy(dst, bytes.NewBufferString(out))
	return err
}

func (r *PriorityRunner) acquire(ctx context.Context) (context.Context, func(), error) {
	if isBackgroundContext(ctx) {
		return r.acquireBackground(ctx)
	}
	return r.acquireProtected(ctx, isInteractiveContext(ctx))
}

func (r *PriorityRunner) acquireProtected(ctx context.Context, preemptBackground bool) (context.Context, func(), error) {
	r.mu.Lock()
	r.waitingProtected++
	if preemptBackground && r.activeBackgroundCancel != nil {
		r.activeBackgroundCancel()
	}
	r.signalLocked()
	r.mu.Unlock()

	select {
	case <-ctx.Done():
		r.mu.Lock()
		r.waitingProtected--
		r.signalLocked()
		r.mu.Unlock()
		return nil, nil, ctx.Err()
	case <-r.token:
	}

	r.mu.Lock()
	r.waitingProtected--
	r.signalLocked()
	r.mu.Unlock()
	return ctx, func() { r.token <- struct{}{} }, nil
}

func (r *PriorityRunner) acquireBackground(ctx context.Context) (context.Context, func(), error) {
	for {
		r.mu.Lock()
		if r.waitingProtected > 0 {
			changed := r.changed
			r.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			case <-changed:
				continue
			}
		}
		r.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-r.token:
		}

		r.mu.Lock()
		if r.waitingProtected > 0 {
			changed := r.changed
			r.mu.Unlock()
			r.token <- struct{}{}
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			case <-changed:
				continue
			}
		}
		opCtx, cancel := context.WithCancel(ctx)
		r.nextBackgroundID++
		id := r.nextBackgroundID
		r.activeBackgroundID = id
		r.activeBackgroundCancel = cancel
		r.mu.Unlock()

		return opCtx, func() {
			r.mu.Lock()
			if r.activeBackgroundID == id {
				r.activeBackgroundID = 0
				r.activeBackgroundCancel = nil
			}
			cancel()
			r.signalLocked()
			r.mu.Unlock()
			r.token <- struct{}{}
		}, nil
	}
}

func (r *PriorityRunner) signalLocked() {
	close(r.changed)
	r.changed = make(chan struct{})
}
