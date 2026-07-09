package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestGuideUsesNonStreamingMessagesRequest(t *testing.T) {
	var payload map[string]any
	client := New("key", "claude-haiku-4-5-20251001")
	client.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("x-api-key") != "key" {
			t.Fatalf("missing api key")
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewBufferString(`{"type":"message","content":[{"type":"text","text":"{\"status_report\":\"tests are running\",\"recommended_action\":\"wait for the check to finish\",\"citations\":[\"go test ./...\"],\"confidence\":\"high\",\"needs_full_buffer\":false,\"reason\":\"visible output is clear\"}"}]}`)),
			Header:     make(http.Header),
		}, nil
	})}
	got, err := client.Guide(context.Background(), SummaryInput{SessionID: 1, State: "running", VisibleCapture: "$ ls"})
	if err != nil {
		t.Fatal(err)
	}
	if got.StatusReport != "tests are running" || got.RecommendedAction != "wait for the check to finish" {
		t.Fatalf("report = %#v", got)
	}
	if got.WantsFullBuffer() {
		t.Fatalf("GuideReport unexpectedly wants full buffer: %#v", got)
	}
	if _, ok := payload["stream"]; ok {
		t.Fatalf("payload included stream: %#v", payload["stream"])
	}
	if payload["max_tokens"].(float64) != 700 {
		t.Fatalf("max_tokens = %#v", payload["max_tokens"])
	}
}

func TestPromptTreatsLastInputAsPreview(t *testing.T) {
	prompt := buildPrompt(SummaryInput{
		SessionID:      1,
		State:          "running",
		LastInput:      "merged! review the logs for summaries made by Haiku, specially as they might rel",
		LastInputMode:  "command",
		VisibleCapture: "visible terminal says work is running\n\n› Find and fix a bug in @filename",
	})
	if strings.Contains(prompt, "last_input:") {
		t.Fatalf("prompt still exposes last_input as full input:\n%s", prompt)
	}
	for _, want := range []string{
		"last_input_preview:",
		"shortened metadata preview",
		"do not treat truncation",
		"capture_filter_note",
		"recent visible captures",
		"do not merge it into unrelated work",
		"citations",
		"Reconstruct citation text only from the terminal captures",
		"needs_full_buffer",
		"recommended_action",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestParseGuideReportFallbackRequestsFullBuffer(t *testing.T) {
	got, err := parseGuideReport("plain status text")
	if err != nil {
		t.Fatal(err)
	}
	if got.StatusReport != "plain status text" || !got.WantsFullBuffer() {
		t.Fatalf("fallback report = %#v", got)
	}
	if !strings.Contains(got.TelegramText(), "recommendation:") {
		t.Fatalf("telegram text = %q", got.TelegramText())
	}
}

func TestGuideReportTelegramTextRendersCitations(t *testing.T) {
	got := GuideReport{
		StatusReport:      "The build failed on a missing generated file.",
		RecommendedAction: "Run the generator, then retry the build.",
		Citations: []string{
			"  error: missing generated file internal/foo.go  ",
			strings.Repeat("x", 400),
			"",
		},
		Confidence: "high",
	}.TelegramText()
	for _, want := range []string{
		"evidence:",
		"> error: missing generated file internal/foo.go",
		"> " + strings.Repeat("x", 268) + " [truncated]",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("telegram text missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "> \n") {
		t.Fatalf("telegram text included empty citation:\n%s", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}
