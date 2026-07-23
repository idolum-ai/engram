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
	"unicode/utf8"

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
	expectedCaptionURL := visibleURL
	expectedTitle := "build"
	expectArtifact := true
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/botTOKEN/sendMessage" {
			signalRequests++
			var body map[string]any
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				return nil, err
			}
			if body["text"] != "[1] terminal-authored signal\n\nbuild needs review https://signal.invalid/hidden" || telegramReplyMessageID(body) != 77 {
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
		if req.FormValue("message_id") != "77" || !strings.Contains(caption, "[1] running · "+expectedTitle) {
			return nil, errors.New("incorrect snapshot anchor identity or caption")
		}
		if expectArtifact != strings.Contains(caption, "files:\n<pre>1. "+artifact+"</pre>") || !strings.Contains(caption, "links:\n"+expectedCaptionURL) {
			return nil, errors.New("snapshot anchor omitted visible references")
		}
		if expectedCaptionURL != visibleURL && strings.Contains(caption, visibleURL) {
			return nil, errors.New("snapshot anchor retained stale joined URL")
		}
		if strings.Contains(caption, "```") || media["parse_mode"] != "HTML" {
			return nil, errors.New("snapshot file references must use HTML code formatting")
		}
		if strings.Contains(caption, "signal.invalid") {
			return nil, errors.New("snapshot references parsed an upstream payload")
		}
		markup := req.FormValue("reply_markup")
		if !strings.Contains(markup, "refresh:1") || strings.Contains(markup, "snapshot:1") || expectArtifact != strings.Contains(markup, "file:1:") {
			return nil, errors.New("incorrect snapshot anchor markup")
		}
		return snapshotJSONResponse(`{"message_id":77,"chat":{"id":100}}`), nil
	})}
	renderer := &countingSnapshotRenderer{}
	tmuxRunner := &snapshotReferenceTmuxRunner{physical: physicalCapture, joined: joinedCapture}
	app := &App{
		Config:             config.Config{AnchorMode: config.AnchorModeSnapshot, SnapshotTheme: "contrast-dark", SnapshotStatusCommand: "local-status", TelegramChatID: 100, Home: dir},
		Store:              store,
		Telegram:           client,
		Tmux:               tmux.New(tmuxRunner),
		Snapshots:          renderer,
		snapshotReady:      true,
		captureSlots:       make(chan struct{}, 1),
		renderSlots:        make(chan struct{}, 1),
		manualRefresh:      map[int]bool{},
		footerStatusRunner: &recordingSnapshotFooterStatusRunner{output: "disk 47G free"},
	}

	app.refreshSnapshotAnchor(context.Background(), session.ID, true)
	got, ok := store.FindSession(session.ID)
	if !ok || got.AnchorMessageID != 77 || got.AnchorFormat != "snapshot" || got.LastSnapshotCaptureHash == "" || got.LastKnownCWD != "/tmp" || got.UpstreamMessageID != 88 {
		t.Fatalf("snapshot session = %#v ok=%v", got, ok)
	}
	if requests != 1 || signalRequests != 1 || renderer.renders != 1 {
		t.Fatalf("first refresh requests=%d signals=%d renders=%d", requests, signalRequests, renderer.renders)
	}
	if renderer.input.Status != "disk 47G free" {
		t.Fatalf("snapshot anchor status = %q", renderer.input.Status)
	}

	ageSnapshotAttempt(t, store, session.ID)
	app.refreshSnapshotAnchor(context.Background(), session.ID, false)
	if requests != 1 || renderer.renders != 1 {
		t.Fatalf("unchanged refresh requests=%d renders=%d", requests, renderer.renders)
	}

	if err := os.Remove(artifact); err != nil {
		t.Fatal(err)
	}
	expectArtifact = false
	ageSnapshotAttempt(t, store, session.ID)
	app.refreshSnapshotAnchor(context.Background(), session.ID, false)
	if requests != 2 || renderer.renders != 2 {
		t.Fatalf("reference-only refresh requests=%d renders=%d", requests, renderer.renders)
	}

	expectedCaptionURL = "https://example.test/build/"
	tmuxRunner.joined = strings.Replace(joinedCapture, visibleURL, "https://example.test/build/\n7", 1)
	ageSnapshotAttempt(t, store, session.ID)
	app.refreshSnapshotAnchor(context.Background(), session.ID, false)
	if requests != 3 || renderer.renders != 3 {
		t.Fatalf("joined-only refresh requests=%d renders=%d", requests, renderer.renders)
	}

	app.manualRefresh[session.ID] = true
	app.refreshSnapshotAnchor(context.Background(), session.ID, true)
	if requests != 4 || renderer.renders != 4 {
		t.Fatalf("manual refresh requests=%d renders=%d", requests, renderer.renders)
	}

	renderer.onRender = func() {
		expectedTitle = "renamed"
		if _, _, err := store.UpdateSession(session.ID, func(current *state.TerminalSession) {
			current.Title = expectedTitle
		}); err != nil {
			t.Fatal(err)
		}
	}
	app.manualRefresh[session.ID] = true
	app.refreshSnapshotAnchor(context.Background(), session.ID, true)
	if requests != 4 || renderer.renders != 5 {
		t.Fatalf("rename-during-render requests=%d renders=%d", requests, renderer.renders)
	}
	ageSnapshotAttempt(t, store, session.ID)
	app.refreshSnapshotAnchor(context.Background(), session.ID, false)
	if requests != 5 || renderer.renders != 6 {
		t.Fatalf("rename retry requests=%d renders=%d", requests, renderer.renders)
	}
}

func ageSnapshotAttempt(t *testing.T, store *state.Store, id int) {
	t.Helper()
	if _, _, err := store.UpdateSession(id, func(session *state.TerminalSession) {
		session.LastSnapshotAttemptAt = time.Now().Add(-11 * time.Second)
	}); err != nil {
		t.Fatal(err)
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

	app := &App{}
	caption, _ := app.snapshotAnchorCaption(session, capture, app.visibleReferences(capture.JoinedText))
	if !strings.Contains(caption, "links:\n"+publicURL) {
		t.Fatalf("snapshot caption omitted exact presentation URL: %q", caption)
	}
	if strings.Contains(caption, apiURL) {
		t.Fatalf("snapshot caption used physical capture URL instead of explicit presentation text: %q", caption)
	}
}

func TestSnapshotAnchorCaptionDisclosesFullWidthWrapping(t *testing.T) {
	t.Parallel()
	capture := tmux.StyledCapture{
		Title:       "wide review",
		CurrentPath: "/tmp",
		Columns:     289,
		VisibleRows: 162,
		BufferRows:  64,
	}
	session := state.TerminalSession{ID: 3, State: state.TerminalRunning, Title: "wide review"}

	caption, _ := (&App{}).snapshotAnchorCaption(session, capture, visibleReferences{})
	if !strings.Contains(caption, "full-width image · rows wrap at 100 columns") {
		t.Fatalf("snapshot caption omitted full-width wrapping disclosure: %q", caption)
	}

	capture.Columns = 96
	caption, _ = (&App{}).snapshotAnchorCaption(session, capture, visibleReferences{})
	if strings.Contains(caption, "rows wrap") {
		t.Fatalf("narrow snapshot caption claimed wrapping: %q", caption)
	}
}

func TestSnapshotAnchorCaptionKeepsRedactionURLSafe(t *testing.T) {
	t.Parallel()
	configuredSecret := "configured-secret-value"
	capture := tmux.StyledCapture{Title: "review", CurrentPath: "/tmp", Columns: 80, VisibleRows: 24, BufferRows: 64}
	session := state.TerminalSession{ID: 3, State: state.TerminalRunning, Title: "review"}
	referenceText := strings.Join([]string{
		"https://example.test/build?mode=fast&access_token=query-secret",
		"https://example.test/artifacts/" + configuredSecret,
		"https://malformed.example/cb?access_token=query-secret;ignored=x",
	}, "\n")

	app := &App{Config: config.Config{OpenAIAPIKey: configuredSecret}}
	caption, _ := app.snapshotAnchorCaption(session, capture, app.visibleReferences(referenceText))
	for _, want := range []string{
		"https://example.test/build?access_token=REDACTED&mode=fast",
		"https://example.test/artifacts/REDACTED",
	} {
		if !strings.Contains(caption, want) {
			t.Fatalf("snapshot caption omitted URL-safe redaction %q: %q", want, caption)
		}
	}
	if strings.Contains(caption, "query-secret") || strings.Contains(caption, configuredSecret) || strings.ContainsAny(caption, "<>") {
		t.Fatalf("snapshot caption leaked or broke a redacted URL: %q", caption)
	}
	if strings.Contains(caption, "malformed.example") {
		t.Fatalf("snapshot caption repaired a malformed sensitive query: %q", caption)
	}
}

func TestSnapshotAnchorCaptionBoundsVisibleTextBeforeHTMLEscaping(t *testing.T) {
	t.Parallel()
	capture := tmux.StyledCapture{
		Title:       strings.Repeat("<&", 800),
		CurrentPath: "/" + strings.Repeat("long&path/", 200),
		Columns:     289,
		VisibleRows: 24,
		BufferRows:  64,
	}
	session := state.TerminalSession{ID: 3, State: state.TerminalRunning, Title: capture.Title}
	caption, files := (&App{}).snapshotAnchorCaption(session, capture, visibleReferences{})
	if len(caption) > 960 || !utf8.ValidString(caption) || len(files) != 0 || !strings.Contains(caption, "full-width image · rows wrap at 100 columns") {
		t.Fatalf("caption bytes=%d valid=%v files=%#v", len(caption), utf8.ValidString(caption), files)
	}
	if html := telegram.MarkdownToHTML(caption); strings.ContainsAny(html, "<>") {
		t.Fatalf("escaped caption contains unsafe HTML text: %q", html)
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
		snapshotReady: true,
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

func TestSnapshotRenderHoldsDisclosureBoundary(t *testing.T) {
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
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/botTOKEN/editMessageMedia" {
			return nil, errors.New("unexpected Telegram endpoint " + req.URL.Path)
		}
		return snapshotJSONResponse(`{"message_id":77,"chat":{"id":100}}`), nil
	})}
	acquired := make(chan struct{})
	attempting := make(chan struct{})
	var app *App
	renderer := &countingSnapshotRenderer{onRender: func() {
		go func() {
			close(attempting)
			lock := app.disclosureMutex(session.ID)
			lock.Lock()
			close(acquired)
			lock.Unlock()
		}()
		<-attempting
		select {
		case <-acquired:
			t.Fatal("snapshot rendering did not hold the disclosure boundary")
		case <-time.After(100 * time.Millisecond):
		}
	}}
	app = &App{
		Config:        config.Config{AnchorMode: config.AnchorModeSnapshot, TelegramChatID: 100, Home: dir},
		Store:         store,
		Telegram:      client,
		Tmux:          tmux.New(snapshotTmuxRunner{}),
		Snapshots:     renderer,
		snapshotReady: true,
		captureSlots:  make(chan struct{}, 1),
		renderSlots:   make(chan struct{}, 1),
		manualRefresh: map[int]bool{},
	}

	app.refreshSnapshotAnchor(context.Background(), session.ID, true)

	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("snapshot disclosure boundary was not released")
	}
	if renderer.renders != 1 {
		t.Fatalf("snapshot renders = %d", renderer.renders)
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
		mode: config.AnchorModeSnapshot, snapshotReady: true, captureSlots: make(chan struct{}, 1), renderSlots: make(chan struct{}, 1), manualRefresh: map[int]bool{},
	}
	a.refreshSnapshotAnchor(context.Background(), session.ID, true)
	got, _ := store.FindSession(session.ID)
	if got.UpstreamMessageID != 88 || got.LastSnapshotCaptureHash != "" || renderer.renders != 1 {
		t.Fatalf("session=%#v renders=%d", got, renderer.renders)
	}
	if a.snapshotAvailable() {
		t.Fatal("canonical browser failure did not enter snapshot recovery")
	}
	ageSnapshotAttempt(t, store, session.ID)
	a.refreshSnapshotAnchor(context.Background(), session.ID, true)
	if renderer.renders != 1 {
		t.Fatalf("degraded canonical renderer retried before recovery: renders=%d", renderer.renders)
	}
}

func TestGuideRendererUsesOnlySelectedConfiguredProvider(t *testing.T) {
	if guideRendererFor(config.Config{AnchorMode: config.AnchorModeSnapshot}) != nil {
		t.Fatal("snapshot mode initialized a guide without a key")
	}
	anthropicGuide, anthropicKeys := modelCapabilitiesFor(config.Config{LLMProvider: config.LLMProviderAnthropic, AnthropicAPIKey: "key", AnthropicModel: config.DefaultAnthropicModel})
	if client, ok := anthropicGuide.(*anthropic.Client); !ok || anthropicKeys != client {
		t.Fatal("Anthropic selection did not initialize Haiku")
	}
	openAIGuide, openAIKeys := modelCapabilitiesFor(config.Config{LLMProvider: config.LLMProviderOpenAI, OpenAIAPIKey: "key", OpenAIModel: config.DefaultOpenAIModel})
	if client, ok := openAIGuide.(*openai.Client); !ok || openAIKeys != client {
		t.Fatal("OpenAI selection did not initialize Luna")
	}
}

func TestSnapshotModeStartsDegradedWhenBrowserProbeFails(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
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
	if err != nil || app == nil {
		t.Fatalf("degraded snapshot startup app=%#v err=%v", app, err)
	}
	defer app.Close()
	if app.snapshotAvailable() || app.anchorMode() != config.AnchorModeSnapshot || !strings.Contains(app.snapshotStatus(), "missing-chromium") {
		t.Fatalf("degraded snapshot state mode=%q status=%q", app.anchorMode(), app.snapshotStatus())
	}
}

func TestNewPrefersAvailablePersistedModeOverEnvironmentMode(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
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

func TestNewSerializesAllStateForSharedHome(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	base := config.Config{
		TelegramBotToken:      "first-token",
		TelegramAllowedUserID: 42,
		TelegramChatID:        42,
		AnthropicAPIKey:       "key",
		AnthropicModel:        config.DefaultAnthropicModel,
		AnchorMode:            config.AnchorModeGuide,
		Home:                  dir,
		Workdir:               dir,
		SnapshotTheme:         "terminal",
	}
	first, err := New(base)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	secondConfig := base
	secondConfig.TelegramBotToken = "second-token"
	secondConfig.TelegramAllowedUserID = 43
	secondConfig.TelegramChatID = 43
	second, err := New(secondConfig)
	if second != nil || err == nil || !strings.Contains(err.Error(), "another Engram process") {
		t.Fatalf("second app=%#v error=%v", second, err)
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
			var body map[string]any
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				return nil, err
			}
			markup := string(mustJSON(body["reply_markup"]))
			if strings.Contains(markup, "key:1:left") {
				return nil, errors.New("prospective guide anchor retained snapshot arrows")
			}
			return snapshotJSONResponse(`{"message_id":88,"chat":{"id":100}}`), nil
		case "/botTOKEN/pinChatMessage", "/botTOKEN/unpinChatMessage", "/botTOKEN/deleteMessage":
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
	want := []string{"/botTOKEN/sendMessage", "/botTOKEN/pinChatMessage", "/botTOKEN/deleteMessage"}
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
			if markup := req.FormValue("reply_markup"); !strings.Contains(markup, "refresh:1") || !strings.Contains(markup, "key:1:left") {
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
		snapshotReady: true,
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
		snapshotReady: true,
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
	renders  int
	onRender func()
	input    terminalshot.Input
}

type failingSnapshotRenderer struct{ renders int }

func (r *failingSnapshotRenderer) Available() (string, error) { return "/usr/bin/chromium", nil }
func (r *failingSnapshotRenderer) Render(context.Context, terminalshot.Input, string) (string, error) {
	r.renders++
	return "", &terminalshot.BrowserError{Err: errors.New("render failed")}
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

func (r *countingSnapshotRenderer) Render(_ context.Context, input terminalshot.Input, dir string) (string, error) {
	r.renders++
	r.input = input
	if onRender := r.onRender; onRender != nil {
		r.onRender = nil
		onRender()
	}
	path := filepath.Join(dir, "snapshot-card.png")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return path, os.WriteFile(path, []byte("png"), 0o600)
}
