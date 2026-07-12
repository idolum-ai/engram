package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
	"github.com/idolum-ai/engram/internal/terminalshot"
	"github.com/idolum-ai/engram/internal/tmux"
)

func TestSnapshotCallbackCapturesCanonicalPaneAndRepliesWithPhoto(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.AllocateSession("main", "@1", "%1", "build")
	if err != nil {
		t.Fatal(err)
	}
	session = bindTestSession(t, store, session.ID)
	if _, _, err := store.UpdateSession(session.ID, func(s *state.TerminalSession) {
		s.AnchorChatID = 100
		s.AnchorMessageID = 77
		s.WatchEnabled = true
	}); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var paths []string
	var callbackText, caption, replyTo string
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		paths = append(paths, req.URL.Path)
		mu.Unlock()
		switch req.URL.Path {
		case "/botTOKEN/answerCallbackQuery":
			var body map[string]any
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				return nil, err
			}
			callbackText, _ = body["text"].(string)
			return snapshotJSONResponse(`true`), nil
		case "/botTOKEN/sendPhoto":
			if err := req.ParseMultipartForm(1 << 20); err != nil {
				return nil, err
			}
			caption = req.FormValue("caption")
			replyTo = req.FormValue("reply_to_message_id")
			files := req.MultipartForm.File["photo"]
			if len(files) != 1 || files[0].Filename != "engram-window.png" {
				return nil, errors.New("missing terminal photo")
			}
			return snapshotJSONResponse(`{"message_id":88,"chat":{"id":100}}`), nil
		default:
			return nil, errors.New("unexpected Telegram endpoint")
		}
	})}
	renderer := &fakeSnapshotRenderer{}
	app := &App{
		Config:        config.Config{TelegramAllowedUserID: 42, TelegramChatID: 100, Home: dir},
		Store:         store,
		Telegram:      client,
		Tmux:          tmux.New(snapshotTmuxRunner{}),
		Snapshots:     renderer,
		captureSlots:  make(chan struct{}, 1),
		renderSlots:   make(chan struct{}, 1),
		transferSlots: make(chan struct{}, 1),
		transferQueue: make(chan struct{}, 1),
	}
	status := app.handleCallback(context.Background(), telegram.CallbackQuery{
		ID:      "snapshot-callback",
		From:    telegram.User{ID: 42},
		Message: &telegram.Message{MessageID: 77, Chat: telegram.Chat{ID: 100}},
		Data:    "snapshot:1",
	})
	if status != "callback_ok" {
		t.Fatalf("snapshot callback status = %q", status)
	}
	app.transferWG.Wait()
	if callbackText != "printing window" {
		t.Fatalf("callback text = %q", callbackText)
	}
	if renderer.input.Columns != 71 || renderer.input.VisibleRows != 37 || renderer.input.BufferRows != 64 || !strings.Contains(renderer.input.ANSI, "green") {
		t.Fatalf("snapshot input = %#v", renderer.input)
	}
	if replyTo != "77" || !strings.Contains(caption, "[1] build") || !strings.Contains(caption, "64 buffer rows") {
		t.Fatalf("photo reply=%q caption=%q", replyTo, caption)
	}
	updated, _ := store.FindSession(session.ID)
	if updated.SnapshotMessageID != 88 {
		t.Fatalf("snapshot reply alias = %d, want 88", updated.SnapshotMessageID)
	}
	mu.Lock()
	gotPaths := append([]string(nil), paths...)
	mu.Unlock()
	if len(gotPaths) != 2 {
		t.Fatalf("Telegram paths = %#v", gotPaths)
	}
	if renderer.path == "" {
		t.Fatal("snapshot renderer did not run")
	}
	if _, err := os.Stat(renderer.path); !os.IsNotExist(err) {
		t.Fatalf("snapshot PNG was not removed after upload: %v", err)
	}
}

func TestOnDemandSnapshotUsesSharedRenderLimit(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.AllocateSession("main", "@1", "%1", "build")
	if err != nil {
		t.Fatal(err)
	}
	session = bindTestSession(t, store, session.ID)
	if _, _, err := store.UpdateSession(session.ID, func(s *state.TerminalSession) {
		s.AnchorChatID = 100
		s.AnchorMessageID = 77
		s.WatchEnabled = true
	}); err != nil {
		t.Fatal(err)
	}

	renderer := &fakeSnapshotRenderer{}
	notices := 0
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/botTOKEN/sendMessage" {
			notices++
			return snapshotJSONResponse(`{"message_id":87,"chat":{"id":100}}`), nil
		}
		if req.URL.Path != "/botTOKEN/sendPhoto" {
			return nil, errors.New("unexpected Telegram endpoint " + req.URL.Path)
		}
		return snapshotJSONResponse(`{"message_id":88,"chat":{"id":100}}`), nil
	})}
	app := &App{
		Config:       config.Config{TelegramChatID: 100, Home: dir},
		Store:        store,
		Telegram:     client,
		Tmux:         tmux.New(snapshotTmuxRunner{}),
		Snapshots:    renderer,
		captureSlots: make(chan struct{}, 1),
		renderSlots:  make(chan struct{}, 1),
	}
	app.renderSlots <- struct{}{}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	app.sendSnapshot(canceled, session)
	if renderer.renders != 0 {
		t.Fatalf("on-demand render bypassed occupied render slot: renders = %d", renderer.renders)
	}
	if notices != 1 {
		t.Fatalf("render-slot timeout notices = %d, want 1", notices)
	}

	<-app.renderSlots
	app.sendSnapshot(context.Background(), session)
	if renderer.renders != 1 {
		t.Fatalf("on-demand render after slot release = %d, want 1", renderer.renders)
	}
}

type fakeSnapshotRenderer struct {
	input   terminalshot.Input
	path    string
	renders int
}

func (r *fakeSnapshotRenderer) Available() (string, error) {
	return "/usr/bin/chromium", nil
}

func (r *fakeSnapshotRenderer) Render(_ context.Context, input terminalshot.Input, dir string) (string, error) {
	r.renders++
	r.input = input
	r.path = filepath.Join(dir, "snapshot.png")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return r.path, os.WriteFile(r.path, []byte("png"), 0o600)
}

type snapshotTmuxRunner struct{}

func (snapshotTmuxRunner) Run(_ context.Context, args ...string) (string, error) {
	if len(args) == 0 {
		return "", nil
	}
	switch args[0] {
	case "show-options":
		return appTestServerID + "\n", nil
	case "display-message":
		format := args[len(args)-1]
		if strings.Contains(format, "pane_width") {
			return "71\t37\tbuild pane\t/tmp\n", nil
		}
		return "$1\t@1\t%1\tmain\t0\t0\t1\t/tmp\tbash\n", nil
	case "capture-pane":
		return "", nil
	case "show-buffer":
		capture := strings.Repeat("\x1b[32mgreen\x1b[0m\n", 64)
		return pairedCaptureResult(args, capture, capture), nil
	default:
		return "", nil
	}
}

func pairedCaptureResult(args []string, physical, joined string) string {
	if len(args) > 0 && args[0] == "show-buffer" && strings.Contains(args[len(args)-1], "physical") {
		return physical
	}
	if len(args) > 0 && args[0] == "show-buffer" {
		return joined
	}
	return ""
}

type snapshotRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn snapshotRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func snapshotJSONResponse(result string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":` + result + `}`)),
	}
}
