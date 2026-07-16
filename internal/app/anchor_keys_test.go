package app

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
	"github.com/idolum-ai/engram/internal/tmux"
)

func TestKeyCallbackSendsCtrlC(t *testing.T) {
	app, runner, refreshed := newAnchorKeyTestApp(t)

	status := app.handleCallback(context.Background(), telegram.CallbackQuery{
		ID:   "cb",
		From: telegram.User{ID: 42},
		Message: &telegram.Message{
			MessageID: 10,
			Chat:      telegram.Chat{ID: 100},
		},
		Data: "key:1:ctrl-c",
	})
	if status != "callback_ok" {
		t.Fatalf("handleCallback status = %q", status)
	}
	if len(runner.calls) != 2 || runner.calls[0][0] != "display-message" || runner.calls[1][0] != "if-shell" || !strings.Contains(runner.calls[1][5], "send-keys -t %1 'C-c'") {
		t.Fatalf("tmux calls = %#v", runner.calls)
	}
	ts, ok := app.Store.FindSession(1)
	if !ok || ts.LastActivityAt.IsZero() {
		t.Fatalf("session after key = %#v ok=%v", ts, ok)
	}
	select {
	case <-refreshed:
	case <-time.After(time.Second):
		t.Fatal("key callback did not queue refresh")
	}
}

func TestKeyCallbacksSendDirectionalKeys(t *testing.T) {
	for _, test := range []struct {
		preset string
		key    string
	}{
		{preset: "left", key: "Left"},
		{preset: "up", key: "Up"},
		{preset: "down", key: "Down"},
		{preset: "right", key: "Right"},
	} {
		t.Run(test.preset, func(t *testing.T) {
			app, runner, _ := newAnchorKeyTestApp(t)
			status := app.handleCallback(context.Background(), telegram.CallbackQuery{
				ID: "cb", From: telegram.User{ID: 42},
				Message: &telegram.Message{MessageID: 10, Chat: telegram.Chat{ID: 100}},
				Data:    "key:1:" + test.preset,
			})
			if status != "callback_ok" {
				t.Fatalf("handleCallback status = %q", status)
			}
			if len(runner.calls) != 2 || runner.calls[1][0] != "if-shell" || !strings.Contains(runner.calls[1][5], "send-keys -t %1 '"+test.key+"'") {
				t.Fatalf("tmux calls = %#v", runner.calls)
			}
		})
	}
}

func TestKeyCallbackSendsEscEscWithDelay(t *testing.T) {
	app, runner, refreshed := newAnchorKeyTestApp(t)
	var sleepMu sync.Mutex
	var sleeps []time.Duration
	app.sleepHook = func(delay time.Duration) {
		sleepMu.Lock()
		defer sleepMu.Unlock()
		sleeps = append(sleeps, delay)
	}

	status := app.handleCallback(context.Background(), telegram.CallbackQuery{
		ID:   "cb",
		From: telegram.User{ID: 42},
		Message: &telegram.Message{
			MessageID: 10,
			Chat:      telegram.Chat{ID: 100},
		},
		Data: "key:1:esc2",
	})
	if status != "callback_ok" {
		t.Fatalf("handleCallback status = %q", status)
	}
	want := []string{"display-message", "if-shell", "display-message", "if-shell"}
	if len(runner.calls) != len(want) {
		t.Fatalf("tmux calls = %#v", runner.calls)
	}
	for i, command := range want {
		if runner.calls[i][0] != command {
			t.Fatalf("tmux calls = %#v, want command order %#v", runner.calls, want)
		}
	}
	ts, ok := app.Store.FindSession(1)
	if !ok || ts.LastActivityAt.IsZero() {
		t.Fatalf("session after key = %#v ok=%v", ts, ok)
	}
	select {
	case <-refreshed:
	case <-time.After(time.Second):
		t.Fatal("key callback did not queue refresh")
	}
	sleepMu.Lock()
	defer sleepMu.Unlock()
	if len(sleeps) != 2 || sleeps[0] != 500*time.Millisecond || sleeps[1] <= summaryQuietPeriod-50*time.Millisecond || sleeps[1] > summaryQuietPeriod {
		t.Fatalf("sleeps = %#v, want Esc delay then refresh delay", sleeps)
	}
}

func TestRetiredAnchorCallbacksAreInert(t *testing.T) {
	for _, callbackData := range []string{"refresh:1", "snapshot:1", "key:1:ctrl-c", "key:1:up", "recover:1"} {
		t.Run(callbackData, func(t *testing.T) {
			app, runner, refreshed := newAnchorKeyTestApp(t)
			if _, _, err := app.Store.UpdateSession(1, func(s *state.TerminalSession) {
				s.AnchorMessageID = 20
				if strings.HasPrefix(callbackData, "recover:") {
					s.State = state.TerminalLost
					s.WatchEnabled = false
				}
			}); err != nil {
				t.Fatal(err)
			}
			status := app.handleCallback(context.Background(), telegram.CallbackQuery{
				ID:      "stale",
				From:    telegram.User{ID: 42},
				Message: &telegram.Message{MessageID: 10, Chat: telegram.Chat{ID: 100}},
				Data:    callbackData,
			})
			if status != "callback_user_error" {
				t.Fatalf("status = %q", status)
			}
			if len(runner.calls) != 0 {
				t.Fatalf("retired callback touched tmux: %#v", runner.calls)
			}
			select {
			case <-refreshed:
				t.Fatal("retired callback queued refresh")
			default:
			}
		})
	}
}

func newAnchorKeyTestApp(t *testing.T) (*App, *anchorKeyRunner, <-chan struct{}) {
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
	session = bindTestSession(t, store, session.ID)
	if _, _, err := store.UpdateSession(session.ID, func(s *state.TerminalSession) {
		s.AnchorChatID = 100
		s.AnchorMessageID = 10
	}); err != nil {
		t.Fatal(err)
	}
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: anchorKeyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/botTOKEN/answerCallbackQuery" {
			t.Fatalf("path = %s", req.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":true}`)),
			Header:     make(http.Header),
		}, nil
	})}
	runner := &anchorKeyRunner{}
	refreshed := make(chan struct{}, 1)
	app := &App{
		Config: config.Config{
			TelegramAllowedUserID: 42,
			TelegramChatID:        100,
		},
		Store:          store,
		Telegram:       client,
		Tmux:           tmux.New(runner),
		summaryQueued:  map[int]bool{},
		summaryRunning: map[int]bool{},
		summaryForce:   map[int]bool{},
		sleepHook:      func(time.Duration) {},
		refreshHook: func(context.Context, int, bool) {
			refreshed <- struct{}{}
		},
	}
	return app, runner, refreshed
}

type anchorKeyRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn anchorKeyRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

type anchorKeyRunner struct {
	calls [][]string
}

func (r *anchorKeyRunner) Run(ctx context.Context, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	if len(args) > 0 && args[0] == "show-options" {
		return appTestServerID + "\n", nil
	}
	if len(args) > 0 && args[0] == "display-message" {
		return framedTmuxBindingRecord("$1", "@1", "%1", "main", "0", "0", "1", "/tmp", "bash"), nil
	}
	return "", nil
}
