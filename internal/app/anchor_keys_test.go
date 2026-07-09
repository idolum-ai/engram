package app

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"reflect"
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
	if status != "handled_callback" {
		t.Fatalf("handleCallback status = %q", status)
	}
	want := [][]string{{"send-keys", "-t", "%1", "C-c"}}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("tmux calls = %#v, want %#v", runner.calls, want)
	}
	ts, ok := app.Store.FindSession(1)
	if !ok || ts.LastInputPreview != "Ctrl+C" || ts.LastInputMode != "keys" {
		t.Fatalf("session after key = %#v ok=%v", ts, ok)
	}
	select {
	case <-refreshed:
	case <-time.After(time.Second):
		t.Fatal("key callback did not queue refresh")
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
	if status != "handled_callback" {
		t.Fatalf("handleCallback status = %q", status)
	}
	want := [][]string{
		{"send-keys", "-t", "%1", "Escape"},
		{"send-keys", "-t", "%1", "Escape"},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("tmux calls = %#v, want %#v", runner.calls, want)
	}
	ts, ok := app.Store.FindSession(1)
	if !ok || ts.LastInputPreview != "Esc Esc" || ts.LastInputMode != "keys" {
		t.Fatalf("session after key = %#v ok=%v", ts, ok)
	}
	select {
	case <-refreshed:
	case <-time.After(time.Second):
		t.Fatal("key callback did not queue refresh")
	}
	sleepMu.Lock()
	defer sleepMu.Unlock()
	if !reflect.DeepEqual(sleeps, []time.Duration{500 * time.Millisecond, summaryQuietPeriod}) {
		t.Fatalf("sleeps = %#v, want Esc delay then refresh delay", sleeps)
	}
}

func newAnchorKeyTestApp(t *testing.T) (*App, *anchorKeyRunner, <-chan struct{}) {
	t.Helper()
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AllocateSession(100, 42, "main", "@1", "%1", "shell"); err != nil {
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
	return "", nil
}
