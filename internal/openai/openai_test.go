package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestTranscribeStreamsVoiceNoteAsMultipart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "note.ogg")
	if err := os.WriteFile(path, []byte("ogg-voice-data"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := NewTranscriber("openai-key", "gpt-4o-transcribe")
	client.HTTPClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.String() != client.BaseURL {
			t.Fatalf("request = %s %s", request.Method, request.URL)
		}
		if request.Header.Get("Authorization") != "Bearer openai-key" {
			t.Fatal("missing bearer token")
		}
		mediaType, params, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
		if err != nil || mediaType != "multipart/form-data" {
			t.Fatalf("content type = %q err=%v", request.Header.Get("Content-Type"), err)
		}
		form := multipart.NewReader(request.Body, params["boundary"])
		values := map[string]string{}
		for {
			part, err := form.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(part)
			if err != nil {
				t.Fatal(err)
			}
			values[part.FormName()] = string(body)
			if part.FormName() == "file" && part.FileName() != "voice.ogg" {
				t.Fatalf("filename = %q", part.FileName())
			}
		}
		if values["model"] != "gpt-4o-transcribe" || values["file"] != "ogg-voice-data" {
			t.Fatalf("multipart values = %#v", values)
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(`{"text":"  please run the tests  "}`)), Header: make(http.Header)}, nil
	})}

	got, err := client.Transcribe(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if got != "please run the tests" {
		t.Fatalf("Transcribe() = %q", got)
	}
}

func TestTranscribeRejectsEmptyAndSanitizesAPIError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "note.ogg")
	if err := os.WriteFile(path, []byte("voice"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		code int
		body string
		want string
	}{
		{name: "empty", code: http.StatusOK, body: `{"text":"  "}`, want: "openai returned no transcription"},
		{name: "api", code: http.StatusBadRequest, body: `{"error":{"type":"invalid_request_error","message":"unsupported audio"}}`, want: "openai invalid_request_error: unsupported audio"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := NewTranscriber("private-key", "gpt-4o-transcribe")
			client.HTTPClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				_, _ = io.Copy(io.Discard, request.Body)
				return &http.Response{StatusCode: test.code, Status: http.StatusText(test.code), Body: io.NopCloser(strings.NewReader(test.body)), Header: make(http.Header)}, nil
			})}
			_, err := client.Transcribe(context.Background(), path)
			if err == nil || err.Error() != test.want || strings.Contains(err.Error(), "private-key") {
				t.Fatalf("Transcribe() error = %v", err)
			}
		})
	}
}

func TestTranscribeHandlesEarlyErrorResponseWithoutUploadDeadlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "note.ogg")
	if err := os.WriteFile(path, bytes.Repeat([]byte("voice"), 1024), 0o600); err != nil {
		t.Fatal(err)
	}
	client := NewTranscriber("key", "gpt-4o-transcribe")
	client.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusBadRequest, Status: "400 Bad Request", Body: io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","message":"rejected early"}}`)), Header: make(http.Header)}, nil
	})}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := client.Transcribe(ctx, path)
	if err == nil || err.Error() != "openai invalid_request_error: rejected early" {
		t.Fatalf("Transcribe() error = %v", err)
	}
}

func TestTranscribeRejectsOversizedResponse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "note.ogg")
	if err := os.WriteFile(path, []byte("voice"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := NewTranscriber("key", "gpt-4o-transcribe")
	client.HTTPClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		_, _ = io.Copy(io.Discard, request.Body)
		body := `{"text":"` + strings.Repeat("x", (1<<20)+1) + `"}`
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	_, err := client.Transcribe(context.Background(), path)
	if err == nil || !strings.Contains(err.Error(), "response exceeded") {
		t.Fatalf("Transcribe() error = %v", err)
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
