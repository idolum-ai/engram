package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/recovery"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
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
	return &App{Store: store, Config: config.Config{Home: dir, OpenAIAPIKey: "fixture-sensitive-value"}}, session
}

func TestRecoveryLedgerRecordsShellCommandsButNotAgentConversation(t *testing.T) {
	app, session := recoveryTestApp(t)
	app.recordSentRecoveryCommand(session, tmux.Pane{CurrentCmd: "bash", CurrentPath: "/work"}, "codex --credential=fixture-sensitive-value")
	current, _ := app.Store.FindSession(session.ID)
	if len(current.RecoveryEvents) != 1 || current.RecoveryEvents[0].Program != recovery.ProgramCodex || strings.Contains(current.RecoveryEvents[0].Command, "fixture-sensitive-value") {
		t.Fatalf("shell recovery event = %#v", current.RecoveryEvents)
	}
	reopened, err := state.ReadSnapshot(app.Config.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(reopened.TerminalSessions[0].RecoveryEvents[0].Command, "fixture-sensitive-value") {
		t.Fatalf("persisted recovery event contains secret: %#v", reopened.TerminalSessions[0].RecoveryEvents)
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
	pages := app.recoveryPlanPages()
	if len(pages) != 1 || len(pages[0].actions) != 1 || pages[0].actions[0].ID != first.ID || pages[0].actions[0].Token == "" || !strings.Contains(pages[0].text, "/resume 1") || !strings.Contains(pages[0].text, "Observed launches (not replayed)") || !strings.Contains(pages[0].text, "lean --server") {
		t.Fatalf("plan pages=%#v", pages)
	}
}

func TestRecoveryPlanPaginatesTextAndControlsTogether(t *testing.T) {
	app, first := recoveryTestApp(t)
	for id := 0; id < maxRecoveryPlanEntries+1; id++ {
		session := first
		if id > 0 {
			var err error
			session, err = app.Store.AllocateSession("main", fmt.Sprintf("@%d", id+1), fmt.Sprintf("%%%d", id+1), fmt.Sprintf("session-%d", id+1))
			if err != nil {
				t.Fatal(err)
			}
		}
		if _, _, err := app.Store.UpdateSession(session.ID, func(current *state.TerminalSession) {
			current.State = state.TerminalLost
			current.ResumeProgram = recovery.ProgramCodex
			current.ResumeSessionID = fmt.Sprintf("019f7607-c8b0-74b3-87ca-%012x", id+1)
		}); err != nil {
			t.Fatal(err)
		}
	}
	pages := app.recoveryPlanPages()
	if len(pages) != 2 || len(pages[0].actions) != maxRecoveryPlanEntries || len(pages[1].actions) != 1 {
		t.Fatalf("paginated recovery plan = %#v", pages)
	}
	for _, page := range pages {
		for _, action := range page.actions {
			if !strings.Contains(page.text, fmt.Sprintf("/resume %d", action.ID)) {
				t.Fatalf("action %d is not represented on its page: %q", action.ID, page.text)
			}
		}
	}
}

func TestRecoveryPlanPublicationResumesAfterPartialPageFailure(t *testing.T) {
	app, _ := recoveryTestApp(t)
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	calls := 0
	client.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 2 {
			return &http.Response{StatusCode: http.StatusInternalServerError, Status: "500 Error", Body: io.NopCloser(strings.NewReader(`{"ok":false,"description":"temporary"}`)), Header: make(http.Header)}, nil
		}
		body := fmt.Sprintf(`{"ok":true,"result":{"message_id":%d,"chat":{"id":100}}}`, 100+calls)
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	app.Telegram = client
	pages := []recoveryPlanPage{{text: "page one"}, {text: "page two"}}
	hash := recoveryPlanHash(pages)
	if err := app.publishRecoveryPlanPages(context.Background(), 100, pages, 0, hash); err == nil {
		t.Fatal("partial publication unexpectedly succeeded")
	}
	progress := app.Store.Snapshot()
	if progress.PendingRecoveryPlanHash != hash || progress.PendingRecoveryPlanNextPage != 1 {
		t.Fatalf("persisted recovery plan progress = hash %q page %d", progress.PendingRecoveryPlanHash, progress.PendingRecoveryPlanNextPage)
	}
	restarted := &App{
		Config:                        app.Config,
		Store:                         app.Store,
		Telegram:                      app.Telegram,
		pendingRecoveryPlanMessageIDs: append([]int(nil), progress.RecoveryPlanMessageIDs...),
		pendingRecoveryPlanHash:       progress.PendingRecoveryPlanHash,
		pendingRecoveryPlanNextPage:   progress.PendingRecoveryPlanNextPage,
	}
	if err := restarted.publishRecoveryPlanPages(context.Background(), 100, pages, 0, hash); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("publication made %d sends, want first page once and second page twice", calls)
	}
	if got := app.Store.Snapshot().RecoveryPlanMessageIDs; len(got) != 2 || got[0] != 101 || got[1] != 103 {
		t.Fatalf("tracked recovery plan messages = %v", got)
	}
	completed := app.Store.Snapshot()
	if completed.PendingRecoveryPlanHash != "" || completed.PendingRecoveryPlanNextPage != 0 {
		t.Fatalf("completed recovery plan retained progress = hash %q page %d", completed.PendingRecoveryPlanHash, completed.PendingRecoveryPlanNextPage)
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

func TestPendingResumeReconciliationFinalizesObservedProvider(t *testing.T) {
	app, session := recoveryTestApp(t)
	runner := &resumeRunner{cwd: "/work", program: recovery.ProgramCodex, resumed: true}
	app.Tmux = tmux.New(runner)
	prepared, _, err := app.Store.UpdateSession(session.ID, func(current *state.TerminalSession) {
		current.State = state.TerminalLost
		current.TmuxSessionName = "main"
		current.TmuxWindowID = "@2"
		current.TmuxPaneID = "%2"
		current.TmuxServerID = appTestServerID
		current.ResumeProgram = recovery.ProgramCodex
		current.ResumeSessionID = recoveryTestSessionID
		current.PendingResume = &state.PendingResume{
			PreviousTmuxSessionName: "old", PreviousTmuxWindowID: "@1", PreviousTmuxPaneID: "%1",
			PreviousTmuxServerID: "0123456789abcdef0123456789abcdef",
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.reconcilePendingResume(context.Background(), prepared); err != nil {
		t.Fatal(err)
	}
	got, _ := app.Store.FindSession(session.ID)
	if got.State != state.TerminalRunning || !got.WatchEnabled || got.PendingResume != nil {
		t.Fatalf("reconciled pending resume = %#v", got)
	}
}

type unavailableRecoveryRunner struct{}

func (unavailableRecoveryRunner) Run(context.Context, ...string) (string, error) {
	return "", errors.New("tmux temporarily unavailable")
}

func TestStartupRecoveryDoesNotAcknowledgeIncompleteReconciliation(t *testing.T) {
	app, _ := recoveryTestApp(t)
	if _, _, err := app.Store.ObserveHostBoot("019f7607-c8b0-74b3-87ca-64a7e6e7ede0"); err != nil {
		t.Fatal(err)
	}
	const nextBoot = "029f7607-c8b0-74b3-87ca-64a7e6e7ede0"
	if _, _, err := app.Store.ObserveHostBoot(nextBoot); err != nil {
		t.Fatal(err)
	}
	app.pendingRecoveryBootID = nextBoot
	app.Tmux = tmux.New(unavailableRecoveryRunner{})
	if app.attemptStartupRecoveryPlan(context.Background()) {
		t.Fatal("incomplete startup reconciliation was acknowledged")
	}
	if got := app.Store.Snapshot().PendingRecoveryBootID; got != nextBoot {
		t.Fatalf("pending recovery boot = %q", got)
	}
}
