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
	"time"

	"github.com/idolum-ai/engram/internal/anthropic"
	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/openai"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
	"github.com/idolum-ai/engram/internal/terminalshot"
	"github.com/idolum-ai/engram/internal/tmux"
)

func TestSnapshotAnchorConvertsInPlaceDeduplicatesAndRefreshesManually(t *testing.T) {
	dir := t.TempDir()
	artifact := filepath.Join(dir, "artifact.pdf")
	if err := os.WriteFile(artifact, []byte("artifact"), 0o600); err != nil {
		t.Fatal(err)
	}
	visibleURL := "https://example.test/build/7"
	physicalCapture := strings.Repeat("\x1b[32mgreen\x1b[0m\n", 60) + artifact + "\nhttps://example.test/build/\n7\n[engram:upstream] " + firstSignalID + " build needs review https://signal.invalid/hidden\n"
	joinedCapture := strings.Repeat("green\n", 60) + artifact + "\n" + visibleURL + "\n[engram:upstream] " + firstSignalID + " build needs review https://signal.invalid/hidden\n"
	store, err := state.Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.AllocateSession("main", "@1", "%1", "build")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.UpdateSession(session.ID, func(s *state.TerminalSession) {
		s.TmuxServerID = appTestServerID
		s.AnchorChatID = 100
		s.AnchorMessageID = 77
		s.AnchorFormat = "text"
		s.WatchEnabled = true
	}); err != nil {
		t.Fatal(err)
	}

	requests := 0
	signalRequests := 0
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/botTOKEN/sendMessage" {
			signalRequests++
			var body map[string]any
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				return nil, err
			}
			if body["text"] != "[1] terminal-authored signal\n\nbuild needs review https://signal.invalid/hidden" || body["reply_to_message_id"] != float64(77) {
				return nil, errors.New("incorrect upstream notification")
			}
			return snapshotJSONResponse(`{"message_id":88,"chat":{"id":100}}`), nil
		}
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
		caption, _ := media["caption"].(string)
		if req.FormValue("message_id") != "77" || !strings.Contains(caption, "[1] running · build") {
			return nil, errors.New("incorrect snapshot anchor identity or caption")
		}
		if !strings.Contains(caption, "paths:\n"+artifact) || !strings.Contains(caption, "links:\n"+visibleURL) {
			return nil, errors.New("snapshot anchor omitted visible references")
		}
		if strings.Contains(caption, "```") || media["parse_mode"] != nil {
			return nil, errors.New("snapshot references must remain a plain clickable caption")
		}
		if strings.Contains(caption, "signal.invalid") {
			return nil, errors.New("snapshot references parsed an upstream payload")
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
		Tmux:          tmux.New(snapshotReferenceTmuxRunner{physical: physicalCapture, joined: joinedCapture}),
		Snapshots:     renderer,
		captureSlots:  make(chan struct{}, 1),
		renderSlots:   make(chan struct{}, 1),
		manualRefresh: map[int]bool{},
	}

	app.refreshSnapshotAnchor(context.Background(), session.ID, true)
	got, ok := store.FindSession(session.ID)
	if !ok || got.AnchorMessageID != 77 || got.AnchorFormat != "snapshot" || got.LastSnapshotCaptureHash == "" || got.LastKnownCWD != "/tmp" || got.UpstreamMessageID != 88 {
		t.Fatalf("snapshot session = %#v ok=%v", got, ok)
	}
	if requests != 1 || signalRequests != 1 || renderer.renders != 1 {
		t.Fatalf("first refresh requests=%d signals=%d renders=%d", requests, signalRequests, renderer.renders)
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

func TestSnapshotAnchorCaptionUsesExplicitPresentationTextWithoutURLRewrite(t *testing.T) {
	t.Parallel()
	publicURL := "https://github.com/idolum-ai/kenogram/pull/19"
	apiURL := "https://api.github.com/repos/idolum-ai/kenogram/pulls/19"
	capture := tmux.StyledCapture{
		Text:        "physical command " + apiURL,
		JoinedText:  "joined output " + publicURL,
		Title:       "review",
		CurrentPath: "/tmp",
		Columns:     80,
		VisibleRows: 24,
		BufferRows:  64,
	}
	session := state.TerminalSession{ID: 3, State: state.TerminalRunning, Title: "review"}

	caption := (&App{}).snapshotAnchorCaption(session, capture, capture.JoinedText)
	if !strings.Contains(caption, "links:\n"+publicURL) {
		t.Fatalf("snapshot caption omitted exact presentation URL: %q", caption)
	}
	if strings.Contains(caption, apiURL) {
		t.Fatalf("snapshot caption used physical capture URL instead of explicit presentation text: %q", caption)
	}
}

func TestFailedSnapshotMigrationIsThrottledBeforeRendering(t *testing.T) {
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
		s.TmuxServerID = appTestServerID
		s.AnchorChatID = 100
		s.AnchorMessageID = 77
		s.AnchorFormat = "text"
		s.WatchEnabled = true
	}); err != nil {
		t.Fatal(err)
	}

	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Status:     "500 Internal Server Error",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"ok":false,"description":"Internal Server Error"}`)),
		}, nil
	})}
	renderer := &countingSnapshotRenderer{}
	app := &App{
		Config:        config.Config{AnchorMode: config.AnchorModeSnapshot, TelegramChatID: 100, Home: dir},
		Store:         store,
		Telegram:      client,
		Tmux:          tmux.New(snapshotTmuxRunner{}),
		Snapshots:     renderer,
		captureSlots:  make(chan struct{}, 1),
		renderSlots:   make(chan struct{}, 1),
		manualRefresh: map[int]bool{},
	}

	app.refreshSnapshotAnchor(context.Background(), session.ID, true)
	app.refreshSnapshotAnchor(context.Background(), session.ID, true)
	if renderer.renders != 1 {
		t.Fatalf("failed migration rendered %d times inside throttle window, want 1", renderer.renders)
	}
	updated, _ := store.FindSession(session.ID)
	if updated.LastSnapshotAttemptAt.IsZero() {
		t.Fatal("failed migration did not persist its attempt time")
	}
}

func TestUpstreamSignalDeliversWhenSnapshotRenderingFails(t *testing.T) {
	store, session := newUpstreamStore(t)
	if _, _, err := store.UpdateSession(session.ID, func(s *state.TerminalSession) {
		s.AnchorFormat = "snapshot"
	}); err != nil {
		t.Fatal(err)
	}
	capture := "[engram:upstream] " + firstSignalID + " renderer independent\n"
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/botTOKEN/sendMessage" {
			t.Fatalf("unexpected Telegram path %s", req.URL.Path)
		}
		return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": map[string]any{"message_id": 88, "chat": map[string]any{"id": 100}}}), nil
	})}
	renderer := &failingSnapshotRenderer{}
	a := &App{
		Config: config.Config{AnchorMode: config.AnchorModeSnapshot, TelegramChatID: 100, Home: t.TempDir()},
		Store:  store, Telegram: client, Tmux: tmux.New(snapshotReferenceTmuxRunner{capture: capture}), Snapshots: renderer,
		mode: config.AnchorModeSnapshot, captureSlots: make(chan struct{}, 1), renderSlots: make(chan struct{}, 1), manualRefresh: map[int]bool{},
	}
	a.refreshSnapshotAnchor(context.Background(), session.ID, true)
	got, _ := store.FindSession(session.ID)
	if got.UpstreamMessageID != 88 || got.LastSnapshotCaptureHash != "" || renderer.renders != 1 {
		t.Fatalf("session=%#v renders=%d", got, renderer.renders)
	}
}

func TestGuideRendererUsesOnlySelectedConfiguredProvider(t *testing.T) {
	if guideRendererFor(config.Config{AnchorMode: config.AnchorModeSnapshot}) != nil {
		t.Fatal("snapshot mode initialized a guide without a key")
	}
	if _, ok := guideRendererFor(config.Config{LLMProvider: config.LLMProviderAnthropic, AnthropicAPIKey: "key", AnthropicModel: config.DefaultAnthropicModel}).(*anthropic.Client); !ok {
		t.Fatal("Anthropic selection did not initialize Haiku")
	}
	if _, ok := guideRendererFor(config.Config{LLMProvider: config.LLMProviderOpenAI, OpenAIAPIKey: "key", OpenAIModel: config.DefaultOpenAIModel}).(*openai.Client); !ok {
		t.Fatal("OpenAI selection did not initialize Luna")
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
	if app != nil || err == nil || !strings.Contains(err.Error(), "snapshot requires probed Chromium") {
		t.Fatalf("snapshot browser requirement app=%#v err=%v", app, err)
	}
}

func TestNewPrefersAvailablePersistedModeOverEnvironmentMode(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetAnchorMode(config.AnchorModeGuide); err != nil {
		t.Fatal(err)
	}

	a, err := New(config.Config{
		TelegramBotToken:      "token",
		TelegramAllowedUserID: 42,
		TelegramChatID:        42,
		AnthropicAPIKey:       "key",
		AnthropicModel:        config.DefaultAnthropicModel,
		AnchorMode:            config.AnchorModeSnapshot,
		Home:                  dir,
		Workdir:               dir,
		SnapshotBrowser:       filepath.Join(dir, "missing-chromium"),
		SnapshotTheme:         "terminal",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if a.anchorMode() != config.AnchorModeGuide {
		t.Fatalf("anchor mode = %q, want persisted guide", a.anchorMode())
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
		s.TmuxServerID = appTestServerID
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
	if len(got.StaleAlternateMessageIDs) != 1 || got.StaleAlternateMessageIDs[0] != 77 {
		t.Fatalf("retired snapshot anchor was not marked stale: %#v", got.StaleAlternateMessageIDs)
	}
	if routed, targetState, ok := store.FindReplyTarget(100, 77); !ok || targetState != state.ReplyTargetStale || routed.ID != session.ID {
		t.Fatalf("retired snapshot reply = %#v %q ok=%v", routed, targetState, ok)
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
		s.TmuxServerID = appTestServerID
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
		case "/botTOKEN/pinChatMessage", "/botTOKEN/unpinChatMessage":
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
	want := []string{"/botTOKEN/editMessageMedia", "/botTOKEN/sendPhoto", "/botTOKEN/pinChatMessage", "/botTOKEN/editMessageText", "/botTOKEN/unpinChatMessage"}
	if strings.Join(paths, "|") != strings.Join(want, "|") {
		t.Fatalf("replacement paths = %#v, want %#v", paths, want)
	}
}

func TestSnapshotRefreshBacksOffPendingRetirementBeforeRendering(t *testing.T) {
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
		s.TmuxServerID = appTestServerID
		s.AnchorChatID = 100
		s.AnchorMessageID = 88
		s.AnchorFormat = "snapshot"
		s.RetiringAnchorMessageID = 77
		s.RetiringAnchorFormat = "text"
		s.AnchorPinKnown = true
		s.AnchorPinned = true
		s.WatchEnabled = true
	}); err != nil {
		t.Fatal(err)
	}
	retireAttempts := 0
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/botTOKEN/editMessageText" {
			return nil, errors.New("unexpected Telegram endpoint " + req.URL.Path)
		}
		retireAttempts++
		return telegramTestResponse(t, http.StatusInternalServerError, map[string]any{"ok": false, "error_code": 500, "description": "injected compact failure"}), nil
	})}
	renderer := &countingSnapshotRenderer{}
	app := &App{
		Config:        config.Config{AnchorMode: config.AnchorModeSnapshot, SnapshotTheme: "terminal", TelegramChatID: 100, Home: dir},
		Store:         store,
		Telegram:      client,
		Tmux:          tmux.New(snapshotTmuxRunner{}),
		Snapshots:     renderer,
		captureSlots:  make(chan struct{}, 1),
		renderSlots:   make(chan struct{}, 1),
		manualRefresh: map[int]bool{},
	}

	app.refreshSnapshotAnchor(context.Background(), session.ID, true)
	app.refreshSnapshotAnchor(context.Background(), session.ID, true)
	got, _ := store.FindSession(session.ID)
	if retireAttempts != 1 || renderer.renders != 0 || got.RetiringAnchorMessageID != 77 || !got.RetiringAnchorRetryAt.After(time.Now()) {
		t.Fatalf("migration attempts=%d renders=%d session=%#v", retireAttempts, renderer.renders, got)
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

type failingSnapshotRenderer struct{ renders int }

func (r *failingSnapshotRenderer) Available() (string, error) { return "/usr/bin/chromium", nil }
func (r *failingSnapshotRenderer) Render(context.Context, terminalshot.Input, string) (string, error) {
	r.renders++
	return "", errors.New("render failed")
}

type snapshotReferenceTmuxRunner struct {
	capture  string
	physical string
	joined   string
}

func (r snapshotReferenceTmuxRunner) Run(ctx context.Context, args ...string) (string, error) {
	if len(args) > 0 && args[0] == "capture-pane" {
		return framedStyledCaptureMetadata("bash"), nil
	}
	if len(args) > 0 && args[0] == "show-buffer" {
		physical := firstNonEmpty(r.physical, r.capture)
		joined := firstNonEmpty(r.joined, r.capture)
		return pairedCaptureResult(args, physical, joined), nil
	}
	return (snapshotTmuxRunner{}).Run(ctx, args...)
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
