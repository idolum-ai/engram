package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
	"github.com/idolum-ai/engram/internal/tmux"
)

func TestCloseAttachedSessionOnlyUntracks(t *testing.T) {
	app, runner, id := newSafetyApp(t, state.TerminalOriginAttached)

	result := app.closeSession(context.Background(), id)
	if !result.OK() || result.Message != "untracked; tmux remains open" {
		t.Fatalf("close result = %#v", result)
	}
	if len(runner.calls) != 1 || runner.calls[0][0] != "if-shell" || !strings.Contains(runner.calls[0][5], "-u -t %1 @engram") || !strings.Contains(runner.calls[0][5], "-u -t %1 @engram_watch_id") || !strings.Contains(runner.calls[0][5], "-u -t %1 @engram_notify") || !strings.Contains(runner.calls[0][5], "-u -t %1 @engram_artifact") {
		t.Fatalf("attached close did not only clear Engram pane metadata: %#v", runner.calls)
	}
	got, ok := app.Store.FindSession(id)
	if !ok || got.State != state.TerminalClosed || got.WatchEnabled {
		t.Fatalf("session after untrack = %#v ok=%v", got, ok)
	}
}

func TestTerminalCapabilityReconciliationConvergesActiveAndInactiveSessions(t *testing.T) {
	app, runner, id := newSafetyApp(t, state.TerminalOriginAttached)
	session, ok := app.Store.FindSession(id)
	if !ok {
		t.Fatal("session not found")
	}

	app.reconcileTerminalCapabilities(context.Background(), id)
	if len(runner.calls) != 1 || runner.calls[0][0] != "if-shell" {
		t.Fatalf("active capability reconciliation = %#v, want one guarded advertisement", runner.calls)
	}
	for _, option := range []string{"@engram ", "@engram_watch_id ", "@engram_notify ", "@engram_artifact "} {
		if !strings.Contains(runner.calls[0][5], option) {
			t.Fatalf("active capability transaction missing %q: %#v", option, runner.calls)
		}
	}

	if !strings.HasSuffix(runner.calls[0][5], "set-option -p -q -t %1 @engram 'v1 watch=1 remote=telegram'") {
		t.Fatalf("active capability marker was not committed last: %#v", runner.calls)
	}

	_, _, err := app.Store.UpdateSession(id, func(current *state.TerminalSession) {
		current.WatchEnabled = false
	})
	if err != nil {
		t.Fatal(err)
	}
	runner.calls = nil
	// The caller still holds the pre-unwatch snapshot. Reconciliation must read
	// current state and clear rather than republishing that stale watch.
	app.advertiseTerminalCapabilities(context.Background(), session)
	if len(runner.calls) != 1 || runner.calls[0][0] != "if-shell" {
		t.Fatalf("inactive capability reconciliation = %#v, want one guarded clear", runner.calls)
	}
	for _, option := range []string{"@engram", "@engram_watch_id", "@engram_notify", "@engram_artifact"} {
		if !strings.Contains(runner.calls[0][5], "-u -t %1 "+option) {
			t.Fatalf("inactive capability transaction missing clear for %q: %#v", option, runner.calls)
		}
	}
}

func TestTerminalCapabilityFailuresRetryOnlyDirtySession(t *testing.T) {
	app, runner, id := newSafetyApp(t, state.TerminalOriginAttached)
	session, _ := app.Store.FindSession(id)
	runner.capabilityErr = errors.New("temporary tmux failure")
	app.advertiseTerminalCapabilities(context.Background(), session)

	app.capabilityRetryMu.Lock()
	retry, pending := app.capabilityRetries[id]
	app.capabilityRetryMu.Unlock()
	if !pending || retry.delay != terminalCapabilityInitialRetry {
		t.Fatalf("capability retry = %#v pending=%v", retry, pending)
	}

	runner.capabilityErr = nil
	runner.calls = nil
	app.reconcileDueTerminalCapabilities(context.Background(), time.Now().Add(terminalCapabilityMaxRetry))
	app.capabilityRetryMu.Lock()
	_, pending = app.capabilityRetries[id]
	app.capabilityRetryMu.Unlock()
	if pending || len(runner.calls) != 1 {
		t.Fatalf("successful retry pending=%v calls=%#v", pending, runner.calls)
	}
}

func TestOlderCapabilitySuccessCannotEraseNewerFailedClearRetry(t *testing.T) {
	app, runner, id := newSafetyApp(t, state.TerminalOriginAttached)
	active, _ := app.Store.FindSession(id)
	firstFinished := make(chan struct{})
	releaseFirst := make(chan struct{})
	var hookMu sync.Mutex
	hookCalls := 0
	app.capabilityFinishHook = func(_ int, _ error) {
		hookMu.Lock()
		hookCalls++
		call := hookCalls
		hookMu.Unlock()
		if call == 1 {
			close(firstFinished)
			<-releaseFirst
		}
	}

	oldDone := make(chan struct{})
	go func() {
		app.advertiseTerminalCapabilities(context.Background(), active)
		close(oldDone)
	}()
	<-firstFinished
	if _, _, err := app.Store.UpdateSession(id, func(current *state.TerminalSession) {
		current.WatchEnabled = false
	}); err != nil {
		t.Fatal(err)
	}
	runner.capabilityErr = errors.New("new clear failed")
	app.clearTerminalCapabilities(context.Background(), active)

	app.capabilityRetryMu.Lock()
	_, pendingBeforeOldFinish := app.capabilityRetries[id]
	app.capabilityRetryMu.Unlock()
	close(releaseFirst)
	<-oldDone
	app.capabilityRetryMu.Lock()
	_, pendingAfterOldFinish := app.capabilityRetries[id]
	app.capabilityRetryMu.Unlock()
	if !pendingBeforeOldFinish || !pendingAfterOldFinish {
		t.Fatalf("newer clear retry pending before=%v after older finish=%v", pendingBeforeOldFinish, pendingAfterOldFinish)
	}
}

func TestCapabilityAdvertisementIdentityLossMarksCurrentWatchLost(t *testing.T) {
	app, runner, id := newSafetyApp(t, state.TerminalOriginAttached)
	session, _ := app.Store.FindSession(id)
	runner.capabilityErr = &tmux.IdentityError{Reason: "pane disappeared"}
	app.advertiseTerminalCapabilities(context.Background(), session)

	got, ok := app.Store.FindSession(id)
	if !ok || got.State != state.TerminalLost || got.WatchEnabled {
		t.Fatalf("session after capability identity loss = %#v ok=%v", got, ok)
	}
	app.capabilityRetryMu.Lock()
	_, pending := app.capabilityRetries[id]
	app.capabilityRetryMu.Unlock()
	if pending {
		t.Fatal("identity loss retained a capability retry for a dead binding")
	}
}

func TestStaleCapabilityIdentityLossDoesNotOverrideCommittedUnwatch(t *testing.T) {
	app, runner, id := newSafetyApp(t, state.TerminalOriginAttached)
	session, _ := app.Store.FindSession(id)
	runner.capabilityErr = &tmux.IdentityError{Reason: "pane disappeared"}
	atFinish := make(chan struct{})
	release := make(chan struct{})
	app.capabilityFinishHook = func(_ int, _ error) {
		close(atFinish)
		<-release
	}
	done := make(chan struct{})
	go func() {
		app.advertiseTerminalCapabilities(context.Background(), session)
		close(done)
	}()
	<-atFinish
	if _, _, err := app.Store.UpdateSession(id, func(current *state.TerminalSession) {
		current.WatchEnabled = false
	}); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done

	got, ok := app.Store.FindSession(id)
	if !ok || got.State != state.TerminalRunning || got.WatchEnabled {
		t.Fatalf("session after unwatch won identity race = %#v ok=%v", got, ok)
	}
}

func TestFailedCreatedWindowCloseDoesNotClaimClosed(t *testing.T) {
	app, runner, id := newSafetyApp(t, state.TerminalOriginCreated)
	runner.failKill = true

	result := app.closeSession(context.Background(), id)
	if result.Outcome != actionTmuxFailed || !strings.Contains(result.Message, "close failed") {
		t.Fatalf("close result = %#v", result)
	}
	got, ok := app.Store.FindSession(id)
	if !ok || got.State == state.TerminalClosed || !got.WatchEnabled {
		t.Fatalf("session after failed close = %#v ok=%v", got, ok)
	}
	if len(runner.calls) != 1 || runner.calls[0][0] != "if-shell" {
		t.Fatalf("tmux calls = %#v", runner.calls)
	}
}

func TestPaneIdentityMismatchMarksSessionLostBeforeInput(t *testing.T) {
	app, runner, id := newSafetyApp(t, state.TerminalOriginCreated)
	runner.identityWindow = "@9"

	result := app.sendInput(context.Background(), id, "pwd", "command", true)
	if result.Outcome != actionTmuxFailed {
		t.Fatalf("send result = %#v", result)
	}
	got, ok := app.Store.FindSession(id)
	if !ok || got.State != state.TerminalLost || got.WatchEnabled {
		t.Fatalf("session after identity mismatch = %#v ok=%v", got, ok)
	}
	if len(runner.calls) != 1 || runner.calls[0][0] != "display-message" {
		t.Fatalf("tmux calls = %#v, want identity check only", runner.calls)
	}
}

func TestTransientIdentityFailureDoesNotMarkSessionLost(t *testing.T) {
	app, runner, id := newSafetyApp(t, state.TerminalOriginCreated)
	runner.identityErr = context.Canceled

	result := app.sendInput(context.Background(), id, "pwd", "command", true)
	if result.Outcome != actionTmuxFailed {
		t.Fatalf("send result = %#v", result)
	}
	got, ok := app.Store.FindSession(id)
	if !ok || got.State != state.TerminalRunning || !got.WatchEnabled {
		t.Fatalf("session after transient identity failure = %#v ok=%v", got, ok)
	}
}

func TestLostSessionRecoversWhenImmutableIdentityMatches(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	if _, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.State = state.TerminalLost
		session.WatchEnabled = false
	}); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	cancel()
	app.runCtx = runCtx

	result := app.sendInput(context.Background(), id, "pwd", "command", true)
	if !result.OK() {
		t.Fatalf("send result = %#v", result)
	}
	got, ok := app.Store.FindSession(id)
	if !ok || got.State != state.TerminalRunning || !got.WatchEnabled {
		t.Fatalf("recovered session = %#v ok=%v", got, ok)
	}
}

func TestCaptureFailureWithLiveIdentityDoesNotMarkSessionLost(t *testing.T) {
	app, runner, id := newSafetyApp(t, state.TerminalOriginCreated)
	runner.captureErr = errors.New("temporary capture failure")

	app.refreshSession(context.Background(), id, true)
	got, ok := app.Store.FindSession(id)
	if !ok || got.State != state.TerminalRunning || !got.WatchEnabled {
		t.Fatalf("session after capture failure = %#v ok=%v", got, ok)
	}
}

func TestRefreshStopsWhenSessionIsPrunedAfterIdentityValidation(t *testing.T) {
	app, runner, id := newSafetyApp(t, state.TerminalOriginCreated)
	if _, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.LastKnownCWD = "/tmp"
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 199; i++ {
		if _, err := app.Store.AllocateSession("other", "@2", fmt.Sprintf("%%%d", i+2), "other"); err != nil {
			t.Fatal(err)
		}
	}
	runner.onIdentity = func() {
		runner.onIdentity = nil
		if _, err := app.Store.AllocateSession("new", "@3", "%999", "new"); err != nil {
			t.Fatal(err)
		}
	}

	app.refreshSession(context.Background(), id, true)
	if _, ok := app.Store.FindSession(id); ok {
		t.Fatal("oldest session was not pruned during validation")
	}
	for _, call := range runner.calls {
		if len(call) > 0 && call[0] == "capture-pane" {
			t.Fatalf("refresh captured after session was pruned: %#v", runner.calls)
		}
	}
}

func TestCloseCallbackRequiresSecondConfirmation(t *testing.T) {
	app, runner, id := newSafetyApp(t, state.TerminalOriginCreated)
	if _, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.AnchorChatID = 100
		session.AnchorMessageID = 80
	}); err != nil {
		t.Fatal(err)
	}
	var paths []string
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.Path)
		body := `{"ok":true,"result":true}`
		if req.URL.Path == "/botTOKEN/sendMessage" {
			body = `{"ok":true,"result":{"message_id":90,"chat":{"id":100}}}`
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	app.Telegram = client

	status := app.handleCallback(context.Background(), telegram.CallbackQuery{
		ID:   "cb",
		From: telegram.User{ID: 42},
		Message: &telegram.Message{
			MessageID: 80,
			Chat:      telegram.Chat{ID: 100},
		},
		Data: "close:" + strconv.Itoa(id),
	})
	if status != "callback_ok" {
		t.Fatalf("callback status = %q", status)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("first close tap mutated tmux: %#v", runner.calls)
	}
	got, _ := app.Store.FindSession(id)
	if got.State == state.TerminalClosed {
		t.Fatalf("first close tap closed session: %#v", got)
	}
	wantPaths := []string{"/botTOKEN/sendMessage", "/botTOKEN/answerCallbackQuery"}
	if !reflect.DeepEqual(paths, wantPaths) {
		t.Fatalf("Telegram paths = %#v, want %#v", paths, wantPaths)
	}
}

func TestCloseCallbackRejectsRetiredAnchor(t *testing.T) {
	app, runner, id := newSafetyApp(t, state.TerminalOriginCreated)
	if _, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.AnchorChatID = 100
		session.AnchorMessageID = 81
	}); err != nil {
		t.Fatal(err)
	}
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":true}`)), Header: make(http.Header)}, nil
	})}
	app.Telegram = client

	status := app.handleCallback(context.Background(), telegram.CallbackQuery{
		ID: "stale-close", From: telegram.User{ID: 42},
		Message: &telegram.Message{MessageID: 80, Chat: telegram.Chat{ID: 100}},
		Data:    "close:" + strconv.Itoa(id),
	})
	if status != "callback_user_error" || len(runner.calls) != 0 || len(app.closeConfirms) != 0 {
		t.Fatalf("status=%q tmux=%#v confirmations=%d", status, runner.calls, len(app.closeConfirms))
	}
}

func TestCloseConfirmationCannotCrossBindingGeneration(t *testing.T) {
	app, runner, id := newSafetyApp(t, state.TerminalOriginCreated)
	session, _ := app.Store.FindSession(id)
	confirmation := closeConfirmation{
		SessionID: session.ID, TmuxServerID: session.TmuxServerID,
		TmuxWindowID: session.TmuxWindowID, TmuxPaneID: session.TmuxPaneID,
		ExpiresAt: time.Now().Add(time.Minute),
	}
	if _, _, err := app.Store.UpdateSession(id, func(current *state.TerminalSession) {
		current.TmuxServerID = strings.Repeat("b", 32)
	}); err != nil {
		t.Fatal(err)
	}

	result := app.closeSessionExpected(context.Background(), id, &confirmation)
	got, _ := app.Store.FindSession(id)
	if result.OK() || !strings.Contains(result.Message, "stale") || got.State != state.TerminalRunning || len(runner.calls) != 0 {
		t.Fatalf("result=%#v session=%#v tmux=%#v", result, got, runner.calls)
	}
}

func TestUnauthorizedAndStaleCallbacksAreAlwaysAnswered(t *testing.T) {
	app, runner, _ := newSafetyApp(t, state.TerminalOriginCreated)
	var paths []string
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.Path)
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":true}`)), Header: make(http.Header)}, nil
	})}
	app.Telegram = client

	unauthorized := app.handleCallback(context.Background(), telegram.CallbackQuery{
		ID:      "unauthorized",
		From:    telegram.User{ID: 99},
		Message: &telegram.Message{MessageID: 80, Chat: telegram.Chat{ID: 100}},
		Data:    "refresh:1",
	})
	stale := app.handleCallback(context.Background(), telegram.CallbackQuery{
		ID:      "stale",
		From:    telegram.User{ID: 42},
		Message: &telegram.Message{MessageID: 80, Chat: telegram.Chat{ID: 100}},
		Data:    "key:999:ctrl-c",
	})
	expired := app.handleCallback(context.Background(), telegram.CallbackQuery{
		ID:      "expired",
		From:    telegram.User{ID: 42},
		Message: &telegram.Message{MessageID: 80, Chat: telegram.Chat{ID: 100}},
		Data:    "close-confirm:expired",
	})
	if unauthorized != "rejected_unauthorized_callback" || stale != "callback_user_error" || expired != "callback_user_error" {
		t.Fatalf("callback statuses = %q, %q, %q", unauthorized, stale, expired)
	}
	want := []string{"/botTOKEN/answerCallbackQuery", "/botTOKEN/answerCallbackQuery", "/botTOKEN/answerCallbackQuery"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("Telegram paths = %#v, want %#v", paths, want)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("rejected callbacks touched tmux: %#v", runner.calls)
	}
}

func TestUnauthorizedOrdinaryMessagesCannotReachDeviceCapabilities(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("TMPDIR", filepath.Join(dir, "tmp"))
	auditPath := filepath.Join(dir, "audit.jsonl")
	store, err := state.Open(filepath.Join(dir, "state.json"), auditPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AllocateSession("main", "@1", "%1", "shell"); err != nil {
		t.Fatal(err)
	}
	runner := &safetyRunner{identityWindow: "@1"}
	app := &App{
		Config: config.Config{TelegramAllowedUserID: 42, TelegramChatID: 100},
		Store:  store,
		Tmux:   tmux.New(runner),
	}
	before := store.Snapshot()
	updates := []telegram.Update{
		{Message: &telegram.Message{MessageID: 1, Chat: telegram.Chat{ID: 100}, From: &telegram.User{ID: 987654321}, Text: "/send 1 id"}},
		{Message: &telegram.Message{MessageID: 2, Chat: telegram.Chat{ID: 876543219}, From: &telegram.User{ID: 42}, Text: "id"}},
		{Message: &telegram.Message{MessageID: 3, Chat: telegram.Chat{ID: 100}, From: &telegram.User{ID: 987654321}, Document: &telegram.Document{FileID: "attacker-file", FileName: "payload"}}},
	}
	for _, update := range updates {
		if status := app.handleUpdate(context.Background(), update); status != "rejected_unauthorized" {
			t.Fatalf("unauthorized update status = %q", status)
		}
		_, refs := app.updateJournalRefs(update)
		if refs != (state.UpdateRefs{}) {
			t.Fatalf("unauthorized update retained journal identity: %#v", refs)
		}
	}
	callback := telegram.CallbackQuery{
		ID:      "attacker-callback",
		From:    telegram.User{ID: 987654321},
		Message: &telegram.Message{MessageID: 4, Chat: telegram.Chat{ID: 100}},
		Data:    "key:1:enter",
	}
	if status := app.handleCallback(context.Background(), callback); status != "rejected_unauthorized_callback_answer_failed" {
		t.Fatalf("unauthorized callback status = %q", status)
	}
	_, refs := app.updateJournalRefs(telegram.Update{CallbackQuery: &callback})
	if refs != (state.UpdateRefs{}) {
		t.Fatalf("unauthorized callback retained journal identity: %#v", refs)
	}
	after := store.Snapshot()
	if len(runner.calls) != 0 {
		t.Fatalf("unauthorized messages touched tmux: %#v", runner.calls)
	}
	if !reflect.DeepEqual(after.TerminalSessions, before.TerminalSessions) || len(after.Attachments) != 0 || len(after.ProcessedMessages) != 0 {
		t.Fatalf("unauthorized messages mutated capability state: before=%#v after=%#v", before, after)
	}
	if _, err := os.Stat(app.Config.AttachmentDir()); !os.IsNotExist(err) {
		t.Fatalf("unauthorized attachment created storage: %v", err)
	}
	audit, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes := string(audit); strings.Contains(bytes, "987654321") || strings.Contains(bytes, "876543219") || !strings.Contains(bytes, `"kind":"message"`) || !strings.Contains(bytes, `"kind":"callback_query"`) {
		t.Fatalf("unauthorized audit disclosed identity or omitted rejection: %s", bytes)
	}
}

func TestClosedSessionCannotRefreshAndHasNoControls(t *testing.T) {
	app, runner, id := newSafetyApp(t, state.TerminalOriginCreated)
	if _, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.State = state.TerminalClosed
		session.WatchEnabled = false
	}); err != nil {
		t.Fatal(err)
	}
	app.refreshSession(context.Background(), id, true)
	if len(runner.calls) != 0 {
		t.Fatalf("closed refresh touched tmux: %#v", runner.calls)
	}
	ts, _ := app.Store.FindSession(id)
	if anchorMarkup(ts) != nil {
		t.Fatal("closed anchor retained controls")
	}
}

func TestLostSessionOffersOnlyReattach(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	if _, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.State = state.TerminalLost
		session.WatchEnabled = false
	}); err != nil {
		t.Fatal(err)
	}
	ts, _ := app.Store.FindSession(id)
	markup := anchorMarkup(ts)
	if markup == nil || len(markup.InlineKeyboard) != 1 || len(markup.InlineKeyboard[0]) != 1 {
		t.Fatalf("lost anchor markup = %#v", markup)
	}
	button := markup.InlineKeyboard[0][0]
	if button.Text != "🧭 Link" || button.CallbackData != "recover:"+strconv.Itoa(id) {
		t.Fatalf("lost anchor button = %#v", button)
	}
}

func TestLostSessionWithExactProviderMappingOffersResume(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	if _, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.State = state.TerminalLost
		session.WatchEnabled = false
		session.ResumeProgram = "codex"
		session.ResumeSessionID = "019f7607-c8b0-74b3-87ca-64a7e6e7ede0"
	}); err != nil {
		t.Fatal(err)
	}
	ts, _ := app.Store.FindSession(id)
	markup := anchorMarkup(ts)
	if markup == nil || len(markup.InlineKeyboard[0]) != 2 || markup.InlineKeyboard[0][0].CallbackData != "resume:"+strconv.Itoa(id) {
		t.Fatalf("resumable lost anchor markup = %#v", markup)
	}
}

func TestRecoverCallbackRestoresExactLivePane(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	if _, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.State = state.TerminalLost
		session.WatchEnabled = false
		session.AnchorChatID = 100
		session.AnchorMessageID = 77
	}); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	cancel()
	app.runCtx = runCtx
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":true}`)), Header: make(http.Header)}, nil
	})}
	app.Telegram = client

	status := app.handleCallback(context.Background(), telegram.CallbackQuery{
		ID:   "cb",
		From: telegram.User{ID: 42},
		Message: &telegram.Message{
			MessageID: 77,
			Chat:      telegram.Chat{ID: 100},
		},
		Data: "recover:" + strconv.Itoa(id),
	})
	if status != "callback_ok" {
		t.Fatalf("recover callback status = %q", status)
	}
	got, ok := app.Store.FindSession(id)
	if !ok || got.State != state.TerminalRunning || !got.WatchEnabled {
		t.Fatalf("recovered session = %#v ok=%v", got, ok)
	}
}

func TestStaleAnchorUpdateUsesClosedSummary(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	if _, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.State = state.TerminalClosed
		session.WatchEnabled = false
		session.AnchorChatID = 100
		session.AnchorMessageID = 77
		session.LastSummary = "status:\nclosed truthfully"
	}); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":77,"chat":{"id":100}}}`)), Header: make(http.Header)}, nil
	})}
	app.Telegram = client
	app.updateAnchorLocal(context.Background(), id, "status:\nstale running output", true)
	text, _ := payload["text"].(string)
	if !strings.Contains(text, "closed truthfully") || strings.Contains(text, "stale running output") {
		t.Fatalf("closed edit text = %q", text)
	}
	if _, ok := payload["reply_markup"]; ok {
		t.Fatalf("closed edit retained reply markup: %#v", payload)
	}
}

func TestInitialAnchorFailureLeavesSessionUnwatched(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network unavailable")
	})}
	app := &App{
		Config:         config.Config{TelegramChatID: 100, TmuxSession: "main", Workdir: dir},
		Store:          store,
		Tmux:           tmux.New(&newSessionRunner{}),
		Telegram:       client,
		summaryQueued:  map[int]bool{},
		summaryRunning: map[int]bool{},
		summaryForce:   map[int]bool{},
	}
	result := app.newSession(context.Background(), telegram.Message{MessageID: 1, Chat: telegram.Chat{ID: 100}, From: &telegram.User{ID: 42}}, "pwd")
	if result.Outcome != actionTelegramFailed {
		t.Fatalf("new session outcome = %q, want %q", result.Outcome, actionTelegramFailed)
	}
	sessions := store.Snapshot().TerminalSessions
	if len(sessions) != 1 || sessions[0].WatchEnabled || sessions[0].AnchorMessageID != 0 {
		t.Fatalf("session after anchor failure = %#v", sessions)
	}
	app.summaryMu.Lock()
	defer app.summaryMu.Unlock()
	if len(app.summaryQueued) != 0 || len(app.summaryRunning) != 0 {
		t.Fatalf("anchor failure queued refresh: %#v %#v", app.summaryQueued, app.summaryRunning)
	}
}

func newSafetyApp(t *testing.T, origin state.TerminalOrigin) (*App, *safetyRunner, int) {
	t.Helper()
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	ts, err := store.AllocateSession("main", "@1", "%1", "shell")
	if err != nil {
		t.Fatal(err)
	}
	ts = bindTestSession(t, store, ts.ID)
	if _, _, err := store.UpdateSession(ts.ID, func(session *state.TerminalSession) {
		session.Origin = origin
		session.WatchEnabled = true
	}); err != nil {
		t.Fatal(err)
	}
	runner := &safetyRunner{identityWindow: "@1"}
	return &App{
		Config: config.Config{TelegramAllowedUserID: 42, TelegramChatID: 100},
		Store:  store,
		Tmux:   tmux.New(runner),
	}, runner, ts.ID
}

type safetyRunner struct {
	calls           [][]string
	identityWindow  string
	identityErr     error
	captureErr      error
	capturePhysical string
	captureJoined   string
	failKill        bool
	capabilityErr   error
	onIdentity      func()
}

type newSessionRunner struct{}

func (*newSessionRunner) Run(_ context.Context, args ...string) (string, error) {
	if len(args) > 0 && args[0] == "list-sessions" {
		return framedTmuxRecord("main", "$1", "1", "0"), nil
	}
	if len(args) > 0 && args[0] == "show-options" {
		if args[len(args)-1] == "default-size" {
			return "80x24\n", nil
		}
		return appTestServerID + "\n", nil
	}
	if len(args) > 0 && args[0] == "new-window" {
		return framedTmuxRecord("@1", "%1"), nil
	}
	if len(args) > 0 && args[0] == "display-message" {
		return framedTmuxBindingRecord("$1", "@1", "%1", "main", "0", "0", "1", "/tmp", "bash"), nil
	}
	return "", nil
}

func (r *safetyRunner) Run(_ context.Context, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	if len(args) > 0 && args[0] == "if-shell" && r.capabilityErr != nil {
		return "", r.capabilityErr
	}
	if len(args) > 0 && args[0] == "show-options" {
		return appTestServerID + "\n", nil
	}
	if len(args) > 0 && args[0] == "display-message" {
		if strings.Contains(args[len(args)-1], "pane_width") {
			return framedStyledCaptureMetadata("bash"), nil
		}
		if r.identityErr != nil {
			return "", r.identityErr
		}
		if r.onIdentity != nil {
			r.onIdentity()
		}
		return framedTmuxBindingRecord("$1", r.identityWindow, "%1", "main", "0", "0", "1", "/tmp", "bash"), nil
	}
	if len(args) > 0 && args[0] == "capture-pane" {
		if r.captureErr != nil {
			return "", r.captureErr
		}
		return framedStyledCaptureMetadata("bash"), nil
	}
	if len(args) > 0 && args[0] == "show-buffer" {
		return pairedCaptureResult(args, r.capturePhysical, r.captureJoined), nil
	}
	if len(args) > 0 && (args[0] == "kill-window" || args[0] == "if-shell") && r.failKill {
		return "", errors.New("tmux refused kill")
	}
	return "", nil
}

type safetyRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn safetyRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}
