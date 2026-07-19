package app

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/tmux"
)

func TestCloseThenStaleIdentityLossDoesNotMarkSessionLost(t *testing.T) {
	app, _, stale := newLifecycleRaceApp(t, state.TerminalOriginAttached)

	if result := app.closeSession(context.Background(), stale.ID); !result.OK() {
		t.Fatalf("close result = %#v", result)
	}
	app.markSessionLost(context.Background(), stale, fmt.Errorf("can't find pane: %s", stale.TmuxPaneID))

	got, ok := app.Store.FindSession(stale.ID)
	if !ok || got.State != state.TerminalClosed {
		t.Fatalf("session after stale loss = %#v ok=%v", got, ok)
	}
}

func TestCloseThenStaleValidationDoesNotRecoverSession(t *testing.T) {
	app, _, initial := newLifecycleRaceApp(t, state.TerminalOriginAttached)
	if _, _, err := app.Store.UpdateSession(initial.ID, func(session *state.TerminalSession) {
		session.State = state.TerminalLost
		session.WatchEnabled = false
	}); err != nil {
		t.Fatal(err)
	}
	stale, ok := app.Store.FindSession(initial.ID)
	if !ok {
		t.Fatal("session not found")
	}

	if result := app.closeSession(context.Background(), initial.ID); !result.OK() {
		t.Fatalf("close result = %#v", result)
	}
	if err := app.recordValidatedPane(stale, tmux.Pane{ID: stale.TmuxPaneID, WindowID: stale.TmuxWindowID, CurrentPath: "/tmp"}); err == nil {
		t.Fatal("stale validation unexpectedly recovered the closed session")
	}

	got, ok := app.Store.FindSession(initial.ID)
	if !ok || got.State != state.TerminalClosed || got.WatchEnabled {
		t.Fatalf("session after stale recovery = %#v ok=%v", got, ok)
	}
}

func TestCapacityAllocationCannotPruneInputTarget(t *testing.T) {
	app, runner, session := newLifecycleRaceApp(t, state.TerminalOriginCreated)
	fillSessionCapacity(t, app, 199)
	runner.onSend = func() {
		_, runner.err = app.Store.AllocateSession("new", "@3", "%999", "new")
	}

	result := app.sendInput(context.Background(), session.ID, "pwd", "command", true)
	if runner.err == nil || !strings.Contains(runner.err.Error(), "capacity") {
		t.Fatalf("competing allocation error = %v, want capacity error", runner.err)
	}
	if !result.OK() {
		t.Fatalf("send result = %#v", result)
	}
	if _, ok := app.Store.FindSession(session.ID); !ok {
		t.Fatal("input target was pruned")
	}
}

func TestCapacityAllocationCannotPruneCloseTarget(t *testing.T) {
	app, runner, session := newLifecycleRaceApp(t, state.TerminalOriginCreated)
	fillSessionCapacity(t, app, 199)
	runner.onKill = func() {
		_, runner.err = app.Store.AllocateSession("new", "@3", "%999", "new")
	}

	result := app.closeSession(context.Background(), session.ID)
	if runner.err == nil || !strings.Contains(runner.err.Error(), "capacity") {
		t.Fatalf("competing allocation error = %v, want capacity error", runner.err)
	}
	if !result.OK() {
		t.Fatalf("close result = %#v", result)
	}
	closed, ok := app.Store.FindSession(session.ID)
	if !ok || closed.State != state.TerminalClosed {
		t.Fatalf("close target = %#v ok=%v", closed, ok)
	}
}

func newLifecycleRaceApp(t *testing.T, origin state.TerminalOrigin) (*App, *lifecycleRaceRunner, state.TerminalSession) {
	t.Helper()
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.AllocateSession("main", "@1", "%1", "shell")
	if err != nil {
		t.Fatal(err)
	}
	session, _, err = store.UpdateSession(session.ID, func(current *state.TerminalSession) {
		current.Origin = origin
		current.WatchEnabled = true
		current.LastKnownCWD = "/tmp"
		current.TmuxServerID = lifecycleServerID
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := &lifecycleRaceRunner{}
	return &App{
		Config: config.Config{TelegramAllowedUserID: 42, TelegramChatID: 100},
		Store:  store,
		Tmux:   tmux.New(runner),
	}, runner, session
}

func fillSessionCapacity(t *testing.T, app *App, count int) {
	t.Helper()
	for i := 0; i < count; i++ {
		if _, err := app.Store.AllocateSession("other", "@2", fmt.Sprintf("%%%d", i+2), "other"); err != nil {
			t.Fatal(err)
		}
	}
}

type lifecycleRaceRunner struct {
	onSend func()
	onKill func()
	err    error
}

const lifecycleServerID = "0123456789abcdef0123456789abcdef"

func (r *lifecycleRaceRunner) Run(_ context.Context, args ...string) (string, error) {
	if len(args) == 0 {
		return "", nil
	}
	switch args[0] {
	case "show-options":
		return lifecycleServerID + "\n", nil
	case "display-message":
		return framedTmuxBindingRecord("$1", "@1", "%1", "main", "0", "0", "1", "/tmp", "bash"), nil
	case "send-keys":
		if r.onSend != nil {
			hook := r.onSend
			r.onSend = nil
			hook()
		}
	case "if-shell":
		if r.onSend != nil && len(args) > 5 && (strings.Contains(args[5], "paste-buffer") || strings.Contains(args[5], "send-keys")) {
			hook := r.onSend
			r.onSend = nil
			hook()
		}
		if r.onKill != nil {
			hook := r.onKill
			r.onKill = nil
			hook()
		}
	}
	return "", nil
}
