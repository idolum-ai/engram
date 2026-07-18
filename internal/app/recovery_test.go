package app

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/recovery"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/tmux"
)

const recoveryTestSessionID = "019f7607-c8b0-74b3-87ca-64a7e6e7ede0"

func recoveryTestApp(t *testing.T) (*App, state.TerminalSession) {
	t.Helper()
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.AllocateSession("main", "@1", "%1", "gleipnir")
	if err != nil {
		t.Fatal(err)
	}
	session, _, err = store.UpdateSession(session.ID, func(current *state.TerminalSession) {
		current.TmuxServerID = "0123456789abcdef0123456789abcdef"
	})
	if err != nil {
		t.Fatal(err)
	}
	return &App{Store: store, Config: config.Config{OpenAIAPIKey: "sk-proj-super-secret-value"}}, session
}

func TestRecoveryLedgerRecordsShellCommandsButNotAgentConversation(t *testing.T) {
	app, session := recoveryTestApp(t)
	app.recordSentRecoveryCommand(session, tmux.Pane{CurrentCmd: "bash", CurrentPath: "/work"}, "codex --token=sk-proj-super-secret-value")
	current, _ := app.Store.FindSession(session.ID)
	if len(current.RecoveryEvents) != 1 || current.RecoveryEvents[0].Program != recovery.ProgramCodex || strings.Contains(current.RecoveryEvents[0].Command, "super-secret") {
		t.Fatalf("shell recovery event = %#v", current.RecoveryEvents)
	}
	app.recordSentRecoveryCommand(current, tmux.Pane{CurrentCmd: "codex", CurrentPath: "/work"}, "please change the implementation")
	current, _ = app.Store.FindSession(session.ID)
	if len(current.RecoveryEvents) != 1 {
		t.Fatalf("agent conversation was recorded: %#v", current.RecoveryEvents)
	}
}

func TestRecoveryPlanSeparatesExactResumeFromAdvisoryCommand(t *testing.T) {
	app, first := recoveryTestApp(t)
	_, _, err := app.Store.UpdateSession(first.ID, func(current *state.TerminalSession) {
		current.State = state.TerminalLost
		current.ResumeProgram = recovery.ProgramCodex
		current.ResumeSessionID = recoveryTestSessionID
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := app.Store.AllocateSession("main", "@2", "%2", "riemann")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = app.Store.UpdateSession(second.ID, func(current *state.TerminalSession) {
		current.State = state.TerminalLost
		current.RecordRecoveryEvent(state.RecoveryEvent{Kind: "command", Command: "lean --server", Validation: "process_observed"})
	})
	if err != nil {
		t.Fatal(err)
	}
	text, ids := app.recoveryPlan()
	if len(ids) != 1 || ids[0] != first.ID || !strings.Contains(text, "/resume 1") || !strings.Contains(text, "Observed launches (not replayed)") || !strings.Contains(text, "lean --server") {
		t.Fatalf("plan ids=%v text=%q", ids, text)
	}
}

type recoveryMetadataRunner struct {
	metadata string
	calls    [][]string
}

func (runner *recoveryMetadataRunner) Run(_ context.Context, args ...string) (string, error) {
	runner.calls = append(runner.calls, append([]string(nil), args...))
	if args[0] == "show-options" {
		return runner.metadata + "\n", nil
	}
	return framedTmuxBindingRecord("$1", "@1", "%1", "main", "0", "0", "1", "/work", "codex"), nil
}

func TestRecoveryReconciliationPersistsProviderHookAndProcessObservation(t *testing.T) {
	app, session := recoveryTestApp(t)
	app.recordSentRecoveryCommand(session, tmux.Pane{CurrentCmd: "bash", CurrentPath: "/work"}, "codex")
	session, _ = app.Store.FindSession(session.ID)
	encoded, err := recovery.Encode(recovery.Metadata{
		Program: recovery.ProgramCodex, SessionID: recoveryTestSessionID, CWD: "/work",
		Source: "startup", Observed: time.Date(2026, 7, 18, 21, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := &recoveryMetadataRunner{metadata: encoded}
	app.Tmux = tmux.New(runner)
	if err := app.reconcileRecoverySession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	current, _ := app.Store.FindSession(session.ID)
	if current.ResumeProgram != recovery.ProgramCodex || current.ResumeSessionID != recoveryTestSessionID {
		t.Fatalf("provider mapping = %#v", current)
	}
	if len(current.RecoveryEvents) != 2 || current.RecoveryEvents[0].Validation != "process_observed" || current.RecoveryEvents[1].Validation != "provider_hook" {
		t.Fatalf("recovery events = %#v", current.RecoveryEvents)
	}
	updatedAt := current.UpdatedAt
	if err := app.reconcileRecoverySession(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	unchanged, _ := app.Store.FindSession(session.ID)
	if !unchanged.UpdatedAt.Equal(updatedAt) || len(unchanged.RecoveryEvents) != 2 {
		t.Fatalf("unchanged reconciliation rewrote state: before=%s after=%s events=%#v", updatedAt, unchanged.UpdatedAt, unchanged.RecoveryEvents)
	}
}
