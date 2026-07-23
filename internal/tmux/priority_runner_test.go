package tmux

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type preemptibleRunner struct {
	once       sync.Once
	background chan struct{}
	canceled   chan struct{}
}

func (r *preemptibleRunner) Run(ctx context.Context, args ...string) (string, error) {
	if len(args) > 0 && args[0] == "capture-pane" {
		r.once.Do(func() { close(r.background) })
		<-ctx.Done()
		close(r.canceled)
		return "", ctx.Err()
	}
	return "interactive", nil
}

func TestPriorityRunnerInteractiveWorkPreemptsBackgroundCommand(t *testing.T) {
	t.Parallel()
	inner := &preemptibleRunner{background: make(chan struct{}), canceled: make(chan struct{})}
	runner := NewPriorityRunner(inner)
	backgroundDone := make(chan error, 1)
	go func() {
		_, err := runner.Run(BackgroundContext(context.Background()), "capture-pane")
		backgroundDone <- err
	}()
	<-inner.background

	result := make(chan error, 1)
	go func() {
		out, err := runner.Run(InteractiveContext(context.Background()), "display-message")
		if err == nil && out != "interactive" {
			err = errors.New("unexpected interactive output")
		}
		result <- err
	}()

	select {
	case <-inner.canceled:
	case <-time.After(time.Second):
		t.Fatal("interactive tmux work did not cancel the active background command")
	}
	if err := <-backgroundDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("background error = %v, want context canceled", err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("interactive tmux work remained blocked after preemption")
	}
}

type protectedMutationRunner struct {
	started  chan struct{}
	release  chan struct{}
	canceled chan struct{}
}

func (r *protectedMutationRunner) Run(ctx context.Context, args ...string) (string, error) {
	if len(args) > 0 && args[0] == "new-window" {
		close(r.started)
		select {
		case <-ctx.Done():
			close(r.canceled)
			return "", ctx.Err()
		case <-r.release:
			return "created", nil
		}
	}
	return "interactive", nil
}

func TestPriorityRunnerDoesNotCancelUnmarkedMutation(t *testing.T) {
	t.Parallel()
	inner := &protectedMutationRunner{started: make(chan struct{}), release: make(chan struct{}), canceled: make(chan struct{})}
	runner := NewPriorityRunner(inner)
	mutationDone := make(chan error, 1)
	go func() {
		_, err := runner.Run(context.Background(), "new-window")
		mutationDone <- err
	}()
	<-inner.started

	interactiveDone := make(chan error, 1)
	go func() {
		_, err := runner.Run(InteractiveContext(context.Background()), "display-message")
		interactiveDone <- err
	}()
	select {
	case <-inner.canceled:
		t.Fatal("interactive work canceled an unmarked state-changing command")
	case err := <-interactiveDone:
		t.Fatalf("interactive work bypassed active mutation: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(inner.release)
	if err := <-mutationDone; err != nil {
		t.Fatal(err)
	}
	if err := <-interactiveDone; err != nil {
		t.Fatal(err)
	}
}
