package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/anthropic"
	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
	"github.com/idolum-ai/engram/internal/tmux"
)

func TestFilterHaikuVisibleCaptureDropsLinesSeenInRecentCaptures(t *testing.T) {
	app := &App{captureHistory: map[int][]map[string]bool{}}

	first := "Run /review on my current changes\nbuild failed\n$"
	if got := app.filterHaikuVisibleCapture(1, first); got != first {
		t.Fatalf("first filter = %q, want original", got)
	}
	got := app.filterHaikuVisibleCapture(1, "Run /review on my current changes\nnew compiler error\n$")
	if strings.Contains(got, "Run /review on my current changes") || strings.Contains(got, "$") {
		t.Fatalf("filter kept repeated lines:\n%s", got)
	}
	if !strings.Contains(got, "new compiler error") {
		t.Fatalf("filter dropped new line:\n%s", got)
	}
}

func TestFilterHaikuVisibleCaptureKeepsLinesOlderThanFiveCaptures(t *testing.T) {
	app := &App{captureHistory: map[int][]map[string]bool{}}

	_ = app.filterHaikuVisibleCapture(1, "old")
	for _, capture := range []string{"one", "two", "three", "four", "five"} {
		_ = app.filterHaikuVisibleCapture(1, capture)
	}
	got := app.filterHaikuVisibleCapture(1, "old\nfresh")
	if got != "old\nfresh" {
		t.Fatalf("filter = %q, want old line restored after history window", got)
	}
}

func TestClearHaikuCaptureHistoryRestoresRepeatedLines(t *testing.T) {
	app := &App{captureHistory: map[int][]map[string]bool{}}

	_ = app.filterHaikuVisibleCapture(1, "sticky\nfirst")
	if got := app.filterHaikuVisibleCapture(1, "sticky\nsecond"); got != "second" {
		t.Fatalf("filter before clear = %q, want only second", got)
	}
	app.clearHaikuCaptureHistory(1)
	if got := app.filterHaikuVisibleCapture(1, "sticky\nthird"); got != "sticky\nthird" {
		t.Fatalf("filter after clear = %q, want full capture", got)
	}
}

func TestHandoffEvidenceSurvivesRepeatedLineFilter(t *testing.T) {
	app := &App{captureHistory: map[int][]map[string]bool{}}
	first := "Run /review on my current changes\nDeploy release? Confirm [y/N]:\n61%"
	if got, _ := app.prepareHaikuVisibleCapturePreserving(1, first, nil); got != first {
		t.Fatalf("first filter = %q", got)
	}
	evidence := []string{"Deploy release? Confirm [y/N]:"}
	got, _ := app.prepareHaikuVisibleCapturePreserving(1, "Run /review on my current changes\nDeploy release? Confirm [y/N]:\n62%", evidence)
	if strings.Contains(got, "Run /review") {
		t.Fatalf("filter retained unrelated boilerplate:\n%s", got)
	}
	if !strings.Contains(got, "Deploy release? Confirm [y/N]:") || !strings.Contains(got, "62%") {
		t.Fatalf("filter removed handoff evidence or fresh output:\n%s", got)
	}
}

func TestRefreshCallbackClearsHaikuCaptureHistory(t *testing.T) {
	done := make(chan struct{})
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.AllocateSession("main", "@1", "%1", "shell")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.UpdateSession(session.ID, func(s *state.TerminalSession) {
		s.AnchorChatID = 100
		s.AnchorMessageID = 10
	}); err != nil {
		t.Fatal(err)
	}
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: appRoundTripFunc(func(req *http.Request) (*http.Response, error) {
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
	app := &App{
		Config: config.Config{
			TelegramAllowedUserID: 42,
			TelegramChatID:        100,
		},
		Store:          store,
		Telegram:       client,
		captureHistory: map[int][]map[string]bool{},
		summaryQueued:  map[int]bool{},
		summaryRunning: map[int]bool{},
		summaryForce:   map[int]bool{},
		refreshHook: func(context.Context, int, bool) {
			close(done)
		},
	}

	_ = app.filterHaikuVisibleCapture(1, "sticky\nfirst")
	if got := app.filterHaikuVisibleCapture(1, "sticky\nsecond"); got != "second" {
		t.Fatalf("filter before callback = %q, want only second", got)
	}
	status := app.handleCallback(context.Background(), telegram.CallbackQuery{
		ID:   "cb",
		From: telegram.User{ID: 42},
		Message: &telegram.Message{
			MessageID: 10,
			Chat:      telegram.Chat{ID: 100},
		},
		Data: "refresh:1",
	})
	if status != "callback_ok" {
		t.Fatalf("handleCallback status = %q", status)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("refresh callback did not queue refresh")
	}
	if got := app.filterHaikuVisibleCapture(1, "sticky\nthird"); got != "sticky\nthird" {
		t.Fatalf("filter after refresh callback = %q, want full capture", got)
	}
}

func TestGuideSummarySendsFilteredVisibleCaptureToHaiku(t *testing.T) {
	var prompts []string
	client := anthropic.New("key", "claude-haiku-4-5-20251001")
	client.HTTPClient = &http.Client{Transport: appRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		var payload struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.Messages) != 1 {
			t.Fatalf("messages = %#v, want one", payload.Messages)
		}
		prompts = append(prompts, payload.Messages[0].Content)
		return appJSONGuideResponse(t, anthropic.GuideReport{
			StatusReport:      "A new compiler error is visible.",
			RecommendedAction: "Fix the cited compiler error.",
			Confidence:        "high",
		}), nil
	})}
	app := &App{
		Anthropic:      client,
		captureHistory: map[int][]map[string]bool{},
	}
	ts := state.TerminalSession{ID: 3, State: state.TerminalRunning}

	if _, err := app.guideSummary(context.Background(), ts, "Run /review on my current changes\nold output", false, true); err != nil {
		t.Fatal(err)
	}
	if _, err := app.guideSummary(context.Background(), ts, "Run /review on my current changes\nfresh compiler error", true, true); err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 2 {
		t.Fatalf("prompt count = %d, want 2", len(prompts))
	}
	second := prompts[1]
	if strings.Contains(second, "Run /review on my current changes") {
		t.Fatalf("second prompt included repeated stale line:\n%s", second)
	}
	if !strings.Contains(second, "fresh compiler error") {
		t.Fatalf("second prompt missing fresh line:\n%s", second)
	}
	if !strings.Contains(second, "capture_filter_note") {
		t.Fatalf("second prompt missing filter note:\n%s", second)
	}
}

func TestGuideSummaryFiltersFullScrollbackRetry(t *testing.T) {
	var prompts []string
	client := anthropic.New("key", "claude-haiku-4-5-20251001")
	client.HTTPClient = &http.Client{Transport: appRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		var payload struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		prompts = append(prompts, payload.Messages[0].Content)
		if len(prompts) == 1 {
			return appJSONGuideResponse(t, anthropic.GuideReport{
				StatusReport:      "The visible pane is ambiguous.",
				RecommendedAction: "Check the full scrollback.",
				Confidence:        "low",
				NeedsFullBuffer:   true,
			}), nil
		}
		return appJSONGuideResponse(t, anthropic.GuideReport{
			StatusReport:      "The full scrollback shows the real failure.",
			RecommendedAction: "Fix the compiler error.",
			Confidence:        "high",
		}), nil
	})}
	app := &App{
		Anthropic:      client,
		Tmux:           tmux.New(fakeCaptureRunner{full: "earlier\nRun /review on my current changes\nfresh compiler error"}),
		captureHistory: map[int][]map[string]bool{},
	}
	ts := state.TerminalSession{ID: 3, State: state.TerminalRunning, TmuxPaneID: "%1"}

	_ = app.filterHaikuVisibleCapture(3, "Run /review on my current changes\nold output")
	if _, err := app.guideSummary(context.Background(), ts, "Run /review on my current changes\nfresh visible", true, true); err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 2 {
		t.Fatalf("prompt count = %d, want 2", len(prompts))
	}
	for i, prompt := range prompts {
		if strings.Contains(prompt, "Run /review on my current changes") {
			t.Fatalf("prompt %d included repeated stale line:\n%s", i+1, prompt)
		}
	}
	if !strings.Contains(prompts[1], "full_scrollback_capture:") || !strings.Contains(prompts[1], "fresh compiler error") {
		t.Fatalf("full retry prompt missing filtered full scrollback:\n%s", prompts[1])
	}
}

func TestRefreshPersistsGroundedHandoffCandidate(t *testing.T) {
	const secret = "anthropic-secret-value"
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.jsonl")
	store, err := state.Open(filepath.Join(dir, "state.json"), auditPath)
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.AllocateSession("main", "@1", "%1", "approval")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.UpdateSession(session.ID, func(s *state.TerminalSession) {
		s.WatchEnabled = true
	}); err != nil {
		t.Fatal(err)
	}
	client := anthropic.New(secret, "claude-haiku-4-5-20251001")
	client.HTTPClient = &http.Client{Transport: appRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return appJSONGuideResponse(t, anthropic.GuideReport{
			StatusReport:      "The process is waiting for approval; token=" + secret,
			RecommendedAction: "Approve or reject with API_KEY=" + secret,
			Citations:         []string{"Apply these changes? [y/N] " + secret},
			HumanNeeded:       true,
			HandoffKey:        "approve_changes",
			Confidence:        "high",
		}), nil
	})}
	app := &App{
		Config:         config.Config{AnthropicAPIKey: secret, AnthropicModel: "claude-haiku-4-5-20251001"},
		Store:          store,
		Anthropic:      client,
		Tmux:           tmux.New(handoffRefreshRunner{}),
		captureHistory: map[int][]map[string]bool{},
	}
	app.refreshSession(context.Background(), session.ID, true)
	got, ok := store.FindSession(session.ID)
	if !ok || got.Handoff != nil || got.HandoffCandidate == nil || got.HandoffCandidate.Kind != "open" || got.HandoffCandidate.Observations != 1 {
		t.Fatalf("session handoff candidate = %#v ok=%v", got, ok)
	}
	if strings.Contains(got.LastSummary, secret) || strings.Contains(got.HandoffCandidate.StatusReport, secret) || strings.Contains(got.HandoffCandidate.RecommendedAction, secret) || strings.Contains(strings.Join(got.HandoffCandidate.Evidence, " "), secret) {
		t.Fatalf("derived secret persisted in session: %#v", got)
	}
	persisted, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(persisted), secret) {
		t.Fatalf("derived secret persisted in state file: %s", persisted)
	}
	audit, err := os.ReadFile(auditPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if strings.Contains(string(audit), `"type":"telegram.anchor_rotation"`) {
		t.Fatalf("unsettled candidate triggered rotation: %s", audit)
	}
}

type appRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn appRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func appJSONGuideResponse(t *testing.T, report anthropic.GuideReport) *http.Response {
	t.Helper()
	reportJSON, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := json.Marshal(map[string]any{
		"type": "message",
		"content": []map[string]string{
			{"type": "text", "text": string(reportJSON)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Body:       io.NopCloser(bytes.NewReader(envelope)),
		Header:     make(http.Header),
	}
}

type fakeCaptureRunner struct {
	full string
}

type handoffRefreshRunner struct{}

func (handoffRefreshRunner) Run(_ context.Context, args ...string) (string, error) {
	if len(args) == 0 {
		return "", nil
	}
	switch args[0] {
	case "display-message":
		return "$1\t@1\t%1\tmain\t0\t0\t1\t/tmp\tbash\n", nil
	case "capture-pane":
		return "Apply these changes? [y/N]\n", nil
	default:
		return "", nil
	}
}

func (r fakeCaptureRunner) Run(ctx context.Context, args ...string) (string, error) {
	if len(args) > 0 && args[0] == "capture-pane" && strings.Contains(strings.Join(args, " "), " -S - ") {
		return r.full, nil
	}
	return "", nil
}
