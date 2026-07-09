package app

import (
	"context"
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

func newSafetyApp(t *testing.T, origin state.TerminalOrigin) (*App, *safetyRunner, int) {
	t.Helper()
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	ts, err := store.AllocateSession(100, 42, "main", "@1", "%1", "shell")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.UpdateSession(ts.ID, func(session *state.TerminalSession) {
		session.Origin = origin
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
	failKill       bool
}

func (r *safetyRunner) Run(_ context.Context, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	if len(args) > 0 && args[0] == "display-message" {
		return "$1\t" + r.identityWindow + "\t%1\tmain\t0\t0\t1\t/tmp\tbash\n", nil
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
