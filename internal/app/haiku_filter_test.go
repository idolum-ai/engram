package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/anthropic"
	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
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

func TestRefreshCallbackClearsHaikuCaptureHistory(t *testing.T) {
	done := make(chan struct{})
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
	if status != "handled_callback" {
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

	if _, err := app.guideSummary(context.Background(), ts, "Run /review on my current changes\nold output"); err != nil {
		t.Fatal(err)
	}
	if _, err := app.guideSummary(context.Background(), ts, "Run /review on my current changes\nfresh compiler error"); err != nil {
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
	if !strings.Contains(second, "visible_capture_filter_note") {
		t.Fatalf("second prompt missing filter note:\n%s", second)
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
