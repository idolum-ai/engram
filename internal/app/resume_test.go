package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/mechanics"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/tmux"
)

func TestParseResumeRequest(t *testing.T) {
	t.Parallel()
	const sessionID = "019f5245-5070-7eb3-996c-e284e7cb222c"
	tests := []struct {
		input              string
		id                 int
		program, sessionID string
		ok                 bool
	}{
		{input: "[5]", id: 5, ok: true},
		{input: "5 CODEX " + sessionID, id: 5, program: "codex", sessionID: sessionID, ok: true},
		{input: "4 claude 479e8b39-ff64-4bf8-a6f6-75688d2815f0", id: 4, program: "claude", sessionID: "479e8b39-ff64-4bf8-a6f6-75688d2815f0", ok: true},
		{input: "5 shell " + sessionID},
		{input: "5 codex not-a-uuid"},
		{input: "5 codex"},
	}
	for _, test := range tests {
		id, program, gotSessionID, ok := parseResumeRequest(test.input)
		if id != test.id || program != test.program || gotSessionID != test.sessionID || ok != test.ok {
			t.Errorf("parseResumeRequest(%q) = (%d, %q, %q, %v)", test.input, id, program, gotSessionID, ok)
		}
	}
}

func TestResumeSessionRebindsExistingWatchAndPersistsMapping(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.AllocateSession("old", "@9", "%9", "kenogram")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.UpdateSession(session.ID, func(current *state.TerminalSession) {
		current.State = state.TerminalLost
		current.WatchEnabled = false
		current.TmuxServerID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		current.LastKnownCWD = dir
	}); err != nil {
		t.Fatal(err)
	}
	runner := &resumeRunner{cwd: dir}
	app := &App{
		Config: config.Config{TelegramChatID: 100, TmuxSession: "main", Workdir: dir},
		Store:  store,
		Tmux:   tmux.New(runner),
	}
	const codexID = "019f5245-5070-7eb3-996c-e284e7cb222c"
	result := app.resumeSession(context.Background(), session.ID, "codex", codexID)
	if !result.OK() {
		t.Fatalf("resume result = %#v", result)
	}
	got, ok := store.FindSession(session.ID)
	if !ok || got.ID != session.ID || got.State != state.TerminalRunning || !got.WatchEnabled || got.Origin != state.TerminalOriginCreated {
		t.Fatalf("resumed session = %#v ok=%v", got, ok)
	}
	if got.TmuxSessionName != "main" || got.TmuxWindowID != "@2" || got.TmuxPaneID != "%2" || got.TmuxServerID != appTestServerID {
		t.Fatalf("resumed binding = %#v", got)
	}
	if got.ResumeProgram != "codex" || got.ResumeSessionID != codexID || got.LastKnownCWD != dir {
		t.Fatalf("resumed metadata = %#v", got)
	}
	if !runner.calledWith("set-buffer", "codex resume "+codexID) {
		t.Fatalf("resume command not sent literally: %#v", runner.calls)
	}
}

func TestResumeSessionUsesPersistedMappingAndRejectsRunningWatch(t *testing.T) {
	app, _, id := newResumeTestApp(t, state.TerminalLost)
	if _, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.ResumeProgram = "claude"
		session.ResumeSessionID = "479e8b39-ff64-4bf8-a6f6-75688d2815f0"
	}); err != nil {
		t.Fatal(err)
	}
	if result := app.resumeSession(context.Background(), id, "", ""); !result.OK() {
		t.Fatalf("stored resume result = %#v", result)
	}
	if result := app.resumeSession(context.Background(), id, "", ""); result.Outcome != actionUserError || !strings.Contains(result.Message, "already running") {
		t.Fatalf("running resume result = %#v", result)
	}
}

func TestResumeSessionRejectsClosedWatch(t *testing.T) {
	app, runner, id := newResumeTestApp(t, state.TerminalClosed)
	if _, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.ResumeProgram = "codex"
		session.ResumeSessionID = "019f5245-5070-7eb3-996c-e284e7cb222c"
	}); err != nil {
		t.Fatal(err)
	}

	result := app.resumeSession(context.Background(), id, "", "")
	if result.Outcome != actionUserError || !strings.Contains(result.Message, "only lost sessions can be resumed") {
		t.Fatalf("closed resume result = %#v", result)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("closed resume touched tmux: %#v", runner.calls)
	}
	got, ok := app.Store.FindSession(id)
	if !ok || got.State != state.TerminalClosed {
		t.Fatalf("closed session changed: %#v ok=%v", got, ok)
	}
}

func TestClearRecoveryMetadataMakesClosedWatchReusable(t *testing.T) {
	session := state.TerminalSession{
		ResumeProgram:   "codex",
		ResumeSessionID: "019f5245-5070-7eb3-996c-e284e7cb222c",
		RecoveryEvents:  []state.RecoveryEvent{{Kind: "provider_session"}},
	}
	clearRecoveryMetadata(&session)
	if session.ResumeProgram != "" || session.ResumeSessionID != "" || len(session.RecoveryEvents) != 0 {
		t.Fatalf("recovery metadata remained after close: %#v", session)
	}
}

func TestResumeSessionRollsBackWhenProviderDoesNotStart(t *testing.T) {
	app, runner, id := newResumeTestApp(t, state.TerminalLost)
	runner.neverResume = true
	const sessionID = "019f5245-5070-7eb3-996c-e284e7cb222c"
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	result := app.resumeSession(ctx, id, "codex", sessionID)
	if result.Outcome != actionTmuxFailed || !strings.Contains(result.Message, "did not start") {
		t.Fatalf("resume result = %#v", result)
	}
	got, ok := app.Store.FindSession(id)
	if !ok || got.State != state.TerminalLost || got.TmuxWindowID != "@9" || got.TmuxPaneID != "%9" {
		t.Fatalf("failed resume did not restore original watch: %#v ok=%v", got, ok)
	}
	if !runner.calledContaining("kill-window -t @2") {
		t.Fatalf("failed resume did not clean up replacement window: %#v", runner.calls)
	}
}

func TestResumeSessionRefusesToDuplicateLiveOriginalPane(t *testing.T) {
	app, runner, id := newResumeTestApp(t, state.TerminalLost)
	runner.existingPane = true
	if _, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.TmuxServerID = appTestServerID
	}); err != nil {
		t.Fatal(err)
	}
	result := app.resumeSession(context.Background(), id, "codex", "019f5245-5070-7eb3-996c-e284e7cb222c")
	if result.Outcome != actionUserError || !strings.Contains(result.Message, "original pane still exists") {
		t.Fatalf("live-pane resume result = %#v", result)
	}
	if runner.called("new-window") {
		t.Fatalf("live-pane resume created a duplicate window: %#v", runner.calls)
	}
}

func TestAbortPreparedResumePreservesLinkedBindingWhenCleanupFails(t *testing.T) {
	app, runner, id := newResumeTestApp(t, state.TerminalLost)
	runner.failCleanup = true
	original, _ := app.Store.FindSession(id)
	prepared, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.PendingResume = &state.PendingResume{
			PreviousTmuxSessionName: original.TmuxSessionName,
			PreviousTmuxWindowID:    original.TmuxWindowID, PreviousTmuxPaneID: original.TmuxPaneID,
			PreviousTmuxServerID: original.TmuxServerID, PreviousOrigin: original.Origin,
		}
		session.TmuxWindowID = "@2"
		session.TmuxPaneID = "%2"
		session.TmuxServerID = appTestServerID
	})
	if err != nil {
		t.Fatal(err)
	}
	result := app.abortPreparedResume(prepared, mechanics.Binding{PaneID: "%2", WindowID: "@2", ServerID: appTestServerID}, "start failed", actionTmuxFailed)
	if result.Outcome != actionStateFailed || !strings.Contains(result.Message, "cleanup failed") {
		t.Fatalf("abort result = %#v", result)
	}
	got, _ := app.Store.FindSession(id)
	if got.PendingResume == nil || got.TmuxPaneID != "%2" {
		t.Fatalf("failed cleanup discarded actionable replacement binding: %#v", got)
	}
}

func newResumeTestApp(t *testing.T, terminalState state.TerminalState) (*App, *resumeRunner, int) {
	t.Helper()
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.AllocateSession("old", "@9", "%9", "agent")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.UpdateSession(session.ID, func(current *state.TerminalSession) {
		current.State = terminalState
		current.TmuxServerID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		current.LastKnownCWD = dir
	}); err != nil {
		t.Fatal(err)
	}
	runner := &resumeRunner{cwd: dir}
	return &App{
		Config: config.Config{TelegramChatID: 100, TmuxSession: "main", Workdir: dir},
		Store:  store,
		Tmux:   tmux.New(runner),
	}, runner, session.ID
}

type resumeRunner struct {
	calls        [][]string
	cwd          string
	program      string
	resumed      bool
	neverResume  bool
	existingPane bool
	failCleanup  bool
}

func (r *resumeRunner) Run(_ context.Context, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	switch args[0] {
	case "list-sessions":
		return framedTmuxRecord("main", "$1", "1", "0"), nil
	case "show-options":
		if args[len(args)-1] == "default-size" {
			return "80x24\n", nil
		}
		return appTestServerID + "\n", nil
	case "new-window":
		return framedTmuxRecord("@2", "%2"), nil
	case "list-panes":
		if r.existingPane {
			return framedTmuxRecord("$1", "@9", "%9", "main", "9", "0", "1", r.cwd, "codex"), nil
		}
		return "", nil
	case "display-message":
		program := "bash"
		if r.resumed {
			program = r.program
		}
		return framedTmuxBindingRecord("$1", "@2", "%2", "main", "1", "0", "1", r.cwd, program), nil
	case "set-buffer":
		r.program = strings.Fields(args[len(args)-1])[0]
		return "", nil
	case "if-shell":
		if r.failCleanup && strings.Contains(strings.Join(args, " "), "kill-window -t @2") {
			return "", errors.New("cleanup failed")
		}
		r.resumed = !r.neverResume
		return "", nil
	case "resize-window", "kill-window", "delete-buffer":
		return "", nil
	default:
		return "", fmt.Errorf("unexpected tmux call: %v", args)
	}
}

func (r *resumeRunner) called(command string) bool {
	for _, call := range r.calls {
		if len(call) > 0 && call[0] == command {
			return true
		}
	}
	return false
}

func (r *resumeRunner) calledContaining(fragment string) bool {
	for _, call := range r.calls {
		for _, argument := range call {
			if strings.Contains(argument, fragment) {
				return true
			}
		}
	}
	return false
}

func (r *resumeRunner) calledWith(command, literal string) bool {
	for _, call := range r.calls {
		if len(call) > 0 && call[0] == command && reflect.DeepEqual(call[len(call)-1], literal) {
			return true
		}
	}
	return false
}
