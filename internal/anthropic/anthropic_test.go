package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func TestSummarizeUsesNonStreamingMessagesRequest(t *testing.T) {
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
			Body:       io.NopCloser(bytes.NewBufferString(`{"type":"message","content":[{"type":"text","text":"summary:\n- ok"}]}`)),
			Header:     make(http.Header),
		}, nil
	})}
	got, err := client.Summarize(context.Background(), SummaryInput{SessionID: 1, State: "running", VisibleCapture: "$ ls"})
	if err != nil {
		t.Fatal(err)
	}
	if got == "" {
		t.Fatal("empty summary")
	}
	if _, ok := payload["stream"]; ok {
		t.Fatalf("payload included stream: %#v", payload["stream"])
	}
	if payload["max_tokens"].(float64) != 700 {
		t.Fatalf("max_tokens = %#v", payload["max_tokens"])
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}
