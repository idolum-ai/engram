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
	"testing"

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
	if len(runner.calls) != 0 {
		t.Fatalf("attached close called tmux: %#v", runner.calls)
	}
	got, ok := app.Store.FindSession(id)
	if !ok || got.State != state.TerminalClosed || got.WatchEnabled {
		t.Fatalf("session after untrack = %#v ok=%v", got, ok)
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
	if len(runner.calls) != 2 || runner.calls[0][0] != "show-options" || runner.calls[1][0] != "display-message" {
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
	if button.Text != "🧭 Reattach" || button.CallbackData != "recover:"+strconv.Itoa(id) {
		t.Fatalf("lost anchor button = %#v", button)
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
	onIdentity      func()
}

type newSessionRunner struct{}

func (*newSessionRunner) Run(_ context.Context, args ...string) (string, error) {
	if len(args) > 0 && args[0] == "list-sessions" {
		return framedTmuxRecord("main", "$1", "1", "0"), nil
	}
	if len(args) > 0 && args[0] == "show-options" {
		return appTestServerID + "\n", nil
	}
	if len(args) > 0 && args[0] == "new-window" {
		return framedTmuxRecord("@1", "%1"), nil
	}
	if len(args) > 0 && args[0] == "display-message" {
		return framedTmuxRecord("$1", "@1", "%1", "main", "0", "0", "1", "/tmp", "bash"), nil
	}
	return "", nil
}

func (r *safetyRunner) Run(_ context.Context, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	if len(args) > 0 && args[0] == "show-options" {
		return appTestServerID + "\n", nil
	}
	if len(args) > 0 && args[0] == "display-message" {
		if strings.Contains(args[len(args)-1], "pane_width") {
			return framedTmuxRecord("71", "37", "shell", "/tmp"), nil
		}
		if r.identityErr != nil {
			return "", r.identityErr
		}
		if r.onIdentity != nil {
			r.onIdentity()
		}
		return framedTmuxRecord("$1", r.identityWindow, "%1", "main", "0", "0", "1", "/tmp", "bash"), nil
	}
	if len(args) > 0 && args[0] == "capture-pane" && r.captureErr != nil {
		return "", r.captureErr
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
