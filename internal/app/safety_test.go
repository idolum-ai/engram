package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
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
	wantLast := []string{"kill-window", "-t", "@1"}
	if len(runner.calls) != 2 || runner.calls[0][0] != "display-message" || !reflect.DeepEqual(runner.calls[1], wantLast) {
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
	calls          [][]string
	identityWindow string
	identityErr    error
	captureErr     error
	failKill       bool
}

type newSessionRunner struct{}

func (*newSessionRunner) Run(_ context.Context, args ...string) (string, error) {
	if len(args) > 0 && args[0] == "new-window" {
		return "@1\t%1\n", nil
	}
	return "", nil
}

func (r *safetyRunner) Run(_ context.Context, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	if len(args) > 0 && args[0] == "display-message" {
		if r.identityErr != nil {
			return "", r.identityErr
		}
		return "$1\t" + r.identityWindow + "\t%1\tmain\t0\t0\t1\t/tmp\tbash\n", nil
	}
	if len(args) > 0 && args[0] == "capture-pane" && r.captureErr != nil {
		return "", r.captureErr
	}
	if len(args) > 0 && args[0] == "kill-window" && r.failKill {
		return "", errors.New("tmux refused kill")
	}
	return "", nil
}

type safetyRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn safetyRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}
