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
	"strings"
	"testing"

	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
	"github.com/idolum-ai/engram/internal/terminalshot"
	"github.com/idolum-ai/engram/internal/tmux"
)

func TestSnapshotAnchorConvertsInPlaceDeduplicatesAndRefreshesManually(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.AllocateSession("main", "@1", "%1", "build")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.UpdateSession(session.ID, func(s *state.TerminalSession) {
		s.AnchorChatID = 100
		s.AnchorMessageID = 77
		s.AnchorFormat = "text"
		s.WatchEnabled = true
	}); err != nil {
		t.Fatal(err)
	}

	requests := 0
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/botTOKEN/editMessageMedia" {
			return nil, errors.New("unexpected Telegram endpoint " + req.URL.Path)
		}
		requests++
		if err := req.ParseMultipartForm(1 << 20); err != nil {
			return nil, err
		}
		var media map[string]any
		if err := json.Unmarshal([]byte(req.FormValue("media")), &media); err != nil {
			return nil, err
		}
		if req.FormValue("message_id") != "77" || !strings.Contains(media["caption"].(string), "[1] running · build") {
			return nil, errors.New("incorrect snapshot anchor identity or caption")
		}
		markup := req.FormValue("reply_markup")
		if !strings.Contains(markup, "refresh:1") || strings.Contains(markup, "snapshot:1") {
			return nil, errors.New("incorrect snapshot anchor markup")
		}
		return snapshotJSONResponse(`{"message_id":77,"chat":{"id":100}}`), nil
	})}
	renderer := &countingSnapshotRenderer{}
	app := &App{
		Config:        config.Config{AnchorMode: config.AnchorModeSnapshot, SnapshotTheme: "contrast-dark", TelegramChatID: 100, Home: dir},
		Store:         store,
		Telegram:      client,
		Tmux:          tmux.New(snapshotTmuxRunner{}),
		Snapshots:     renderer,
		captureSlots:  make(chan struct{}, 1),
		renderSlots:   make(chan struct{}, 1),
		manualRefresh: map[int]bool{},
	}

	app.refreshSnapshotAnchor(context.Background(), session.ID, true)
	got, ok := store.FindSession(session.ID)
	if !ok || got.AnchorMessageID != 77 || got.AnchorFormat != "snapshot" || got.LastSnapshotCaptureHash == "" || got.LastKnownCWD != "/tmp" {
		t.Fatalf("snapshot session = %#v ok=%v", got, ok)
	}
	if requests != 1 || renderer.renders != 1 {
		t.Fatalf("first refresh requests=%d renders=%d", requests, renderer.renders)
	}

	app.refreshSnapshotAnchor(context.Background(), session.ID, false)
	if requests != 1 || renderer.renders != 1 {
		t.Fatalf("unchanged refresh requests=%d renders=%d", requests, renderer.renders)
	}
	app.manualRefresh[session.ID] = true
	app.refreshSnapshotAnchor(context.Background(), session.ID, true)
	if requests != 2 || renderer.renders != 2 {
		t.Fatalf("manual refresh requests=%d renders=%d", requests, renderer.renders)
	}
}

func TestSnapshotModeDoesNotInitializeAnthropic(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	app, err := New(config.Config{
		TelegramBotToken:           "token",
		TelegramAllowedUserID:      42,
		TelegramChatID:             42,
		AnchorMode:                 config.AnchorModeSnapshot,
		Home:                       dir,
		Workdir:                    dir,
		SnapshotBrowser:            executable,
		SnapshotTheme:              "terminal",
		AttachmentSoftMaxBytes:     1,
		TelegramPollTimeoutSeconds: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if app.Anthropic != nil {
		t.Fatal("snapshot mode initialized Anthropic")
	}
}

func TestSnapshotModeRequiresAvailableBrowser(t *testing.T) {
	dir := t.TempDir()
	app, err := New(config.Config{
		TelegramBotToken:      "token",
		TelegramAllowedUserID: 42,
		TelegramChatID:        42,
		AnchorMode:            config.AnchorModeSnapshot,
		Home:                  dir,
		Workdir:               dir,
		SnapshotBrowser:       filepath.Join(dir, "missing-chromium"),
		SnapshotTheme:         "terminal",
	})
	if app != nil || err == nil || !strings.Contains(err.Error(), "requires Chromium") {
		t.Fatalf("snapshot browser requirement app=%#v err=%v", app, err)
	}
}

func TestGuideModeRotatesSnapshotAnchorBackToText(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.AllocateSession("main", "@1", "%1", "build")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.UpdateSession(session.ID, func(s *state.TerminalSession) {
		s.AnchorChatID = 100
		s.AnchorMessageID = 77
		s.AnchorFormat = "snapshot"
		s.WatchEnabled = true
	}); err != nil {
		t.Fatal(err)
	}
	var paths []string
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.Path)
		switch req.URL.Path {
		case "/botTOKEN/sendMessage":
			return snapshotJSONResponse(`{"message_id":88,"chat":{"id":100}}`), nil
		case "/botTOKEN/pinChatMessage", "/botTOKEN/unpinChatMessage":
			return snapshotJSONResponse(`true`), nil
		case "/botTOKEN/editMessageCaption":
			return snapshotJSONResponse(`{"message_id":77,"chat":{"id":100}}`), nil
		default:
			return nil, errors.New("unexpected Telegram endpoint")
		}
	})}
	app := &App{Config: config.Config{AnchorMode: config.AnchorModeGuide, TelegramChatID: 100}, Store: store, Telegram: client}
	app.updateAnchorLocal(context.Background(), session.ID, "ready", true)
	got, ok := store.FindSession(session.ID)
	if !ok || got.AnchorMessageID != 88 || got.AnchorFormat != "text" || got.RetiringAnchorMessageID != 0 || !got.AnchorPinned {
		t.Fatalf("guide migration = %#v ok=%v", got, ok)
	}
	want := []string{"/botTOKEN/sendMessage", "/botTOKEN/pinChatMessage", "/botTOKEN/editMessageCaption", "/botTOKEN/unpinChatMessage"}
	if strings.Join(paths, "|") != strings.Join(want, "|") {
		t.Fatalf("guide migration paths = %#v, want %#v", paths, want)
	}
}

func TestSnapshotAnchorReplacesUnavailableTextAnchor(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.AllocateSession("main", "@1", "%1", "build")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.UpdateSession(session.ID, func(s *state.TerminalSession) {
		s.AnchorChatID = 100
		s.AnchorMessageID = 77
		s.AnchorFormat = "text"
		s.WatchEnabled = true
	}); err != nil {
		t.Fatal(err)
	}
	var paths []string
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.Path)
		switch req.URL.Path {
		case "/botTOKEN/editMessageMedia", "/botTOKEN/editMessageText":
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Header:     make(http.Header),
				Body: io.NopCloser(strings.NewReader(
					`{"ok":false,"error_code":400,"description":"Bad Request: message to edit not found"}`,
				)),
			}, nil
		case "/botTOKEN/sendPhoto":
			if err := req.ParseMultipartForm(1 << 20); err != nil {
				return nil, err
			}
			if !strings.Contains(req.FormValue("reply_markup"), "refresh:1") {
				return nil, errors.New("replacement snapshot omitted controls")
			}
			return snapshotJSONResponse(`{"message_id":88,"chat":{"id":100}}`), nil
		case "/botTOKEN/pinChatMessage":
			return snapshotJSONResponse(`true`), nil
		default:
			return nil, errors.New("unexpected Telegram endpoint " + req.URL.Path)
		}
	})}
	app := &App{
		Config:        config.Config{AnchorMode: config.AnchorModeSnapshot, SnapshotTheme: "terminal", TelegramChatID: 100, Home: dir},
		Store:         store,
		Telegram:      client,
		Tmux:          tmux.New(snapshotTmuxRunner{}),
		Snapshots:     &countingSnapshotRenderer{},
		captureSlots:  make(chan struct{}, 1),
		renderSlots:   make(chan struct{}, 1),
		manualRefresh: map[int]bool{},
	}

	app.refreshSnapshotAnchor(context.Background(), session.ID, true)
	got, ok := store.FindSession(session.ID)
	if !ok || got.AnchorMessageID != 88 || got.AnchorFormat != "snapshot" || got.RetiringAnchorMessageID != 0 || !got.AnchorPinned {
		t.Fatalf("replacement snapshot session = %#v ok=%v", got, ok)
	}
	want := []string{"/botTOKEN/editMessageMedia", "/botTOKEN/sendPhoto", "/botTOKEN/pinChatMessage", "/botTOKEN/editMessageText"}
	if strings.Join(paths, "|") != strings.Join(want, "|") {
		t.Fatalf("replacement paths = %#v, want %#v", paths, want)
	}
}

func TestSnapshotTerminalStateCaptionInvalidatesFrameAndControls(t *testing.T) {
	for _, test := range []struct {
		name        string
		terminal    state.TerminalState
		wantRecover bool
	}{
		{name: "lost offers recovery", terminal: state.TerminalLost, wantRecover: true},
		{name: "closed clears controls", terminal: state.TerminalClosed},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			store, err := state.Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			session, err := store.AllocateSession("main", "@1", "%1", "build")
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.UpdateSession(session.ID, func(s *state.TerminalSession) {
				s.State = test.terminal
				s.AnchorChatID = 100
				s.AnchorMessageID = 77
				s.AnchorFormat = "snapshot"
				s.LastSnapshotCaptureHash = "running-frame"
				s.WatchEnabled = false
			}); err != nil {
				t.Fatal(err)
			}
			client := telegram.New("TOKEN")
			client.BaseURL = "https://api.telegram.org/botTOKEN"
			client.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path != "/botTOKEN/editMessageCaption" {
					return nil, errors.New("unexpected Telegram endpoint " + req.URL.Path)
				}
				var body struct {
					ReplyMarkup telegram.InlineKeyboardMarkup `json:"reply_markup"`
				}
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					return nil, err
				}
				markup := string(mustJSON(body.ReplyMarkup))
				if test.wantRecover != strings.Contains(markup, "recover:1") {
					return nil, fmt.Errorf("terminal state markup = %s", markup)
				}
				if !test.wantRecover && len(body.ReplyMarkup.InlineKeyboard) != 0 {
					return nil, fmt.Errorf("closed terminal retained controls: %s", markup)
				}
				return snapshotJSONResponse(`{"message_id":77,"chat":{"id":100}}`), nil
			})}
			app := &App{Config: config.Config{AnchorMode: config.AnchorModeSnapshot}, Store: store, Telegram: client}
			app.updateAnchorLocal(context.Background(), session.ID, "state changed", true)
			got, ok := store.FindSession(session.ID)
			if !ok || got.LastSnapshotCaptureHash != "" {
				t.Fatalf("terminal state snapshot = %#v ok=%v", got, ok)
			}
		})
	}
}

func mustJSON(value any) []byte {
	encoded, _ := json.Marshal(value)
	return encoded
}

type countingSnapshotRenderer struct {
	renders int
}

func (r *countingSnapshotRenderer) Available() (string, error) { return "/usr/bin/chromium", nil }

func (r *countingSnapshotRenderer) Render(_ context.Context, _ terminalshot.Input, dir string) (string, error) {
	r.renders++
	path := filepath.Join(dir, "snapshot-card.png")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return path, os.WriteFile(path, []byte("png"), 0o600)
}
