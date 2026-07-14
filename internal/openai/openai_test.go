package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/idolum-ai/engram/internal/guide"
)

const testModel = "gpt-5.6-luna"

func TestConverseUsesOneBoundedNonStreamingRequest(t *testing.T) {
	requests := 0
	var payload map[string]any
	client := New("openai-key", testModel)
	client.HTTPClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.Header.Get("Authorization") != "Bearer openai-key" {
			t.Fatal("missing bearer token")
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		return completionResponse("The tests passed and the prompt is ready.", "stop"), nil
	})}

	got, err := client.Converse(context.Background(), guide.Input{SessionID: 7, VisibleText: "$ go test ./...\nok example/internal/app"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "The tests passed and the prompt is ready." || requests != 1 {
		t.Fatalf("Converse() = %q requests=%d", got, requests)
	}
	if _, exists := payload["stream"]; exists {
		t.Fatal("request unexpectedly enabled streaming")
	}
	if payload["model"] != testModel || payload["reasoning_effort"] != "none" || payload["temperature"] != guide.Temperature || payload["max_completion_tokens"] != float64(guide.MaxTokens) {
		t.Fatalf("payload = %#v", payload)
	}
	messages, ok := payload["messages"].([]any)
	if !ok || len(messages) != 2 {
		t.Fatalf("messages = %#v", payload["messages"])
	}
	system := messages[0].(map[string]any)
	user := messages[1].(map[string]any)
	if system["role"] != "system" || system["content"] != guide.SystemPrompt {
		t.Fatal("system prompt changed")
	}
	wantPrompt := guide.BuildPrompt(guide.Input{SessionID: 7, VisibleText: "$ go test ./...\nok example/internal/app"})
	if user["role"] != "user" || user["content"] != wantPrompt {
		t.Fatal("terminal evidence changed")
	}
}

func TestConverseRejectsAPIErrorWithoutLeakingKey(t *testing.T) {
	client := New("private-key", testModel)
	client.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		body := `{"error":{"type":"invalid_request_error","message":"model unavailable"}}`
		return &http.Response{StatusCode: http.StatusBadRequest, Status: "400 Bad Request", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	_, err := client.Converse(context.Background(), guide.Input{})
	if err == nil || err.Error() != "openai invalid_request_error: model unavailable" || strings.Contains(err.Error(), "private-key") {
		t.Fatalf("Converse() error = %v", err)
	}
}

func TestConverseRejectsIncompleteAndUnexpectedResponses(t *testing.T) {
	for _, finishReason := range []string{"length", "content_filter", "tool_calls", ""} {
		t.Run(firstNonEmpty(finishReason, "missing"), func(t *testing.T) {
			client := New("key", testModel)
			client.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return completionResponse("partial", finishReason), nil
			})}
			_, err := client.Converse(context.Background(), guide.Input{})
			if err == nil {
				t.Fatalf("accepted finish_reason %q", finishReason)
			}
		})
	}
}

func TestConverseBoundsWords(t *testing.T) {
	words := make([]string, guide.MaxWords+1)
	for index := range words {
		words[index] = fmt.Sprintf("word%d", index+1)
	}
	client := New("key", testModel)
	client.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return completionResponse(strings.Join(words, " "), "stop"), nil
	})}
	got, err := client.Converse(context.Background(), guide.Input{})
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.Fields(got)) != guide.MaxWords || strings.Contains(got, "word181") {
		t.Fatalf("bounded output = %q", got)
	}
}

func completionResponse(text, finishReason string) *http.Response {
	body, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{{
			"message":       map[string]string{"role": "assistant", "content": text},
			"finish_reason": finishReason,
		}},
	})
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}
