package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSessionListMarkupNilWhenNoSessions(t *testing.T) {
	t.Parallel()

	if got := SessionListMarkup(nil, nil); got != nil {
		t.Fatalf("SessionListMarkup(nil, nil) = %#v, want nil", got)
	}
}

func TestSessionListMarkupWithSessions(t *testing.T) {
	t.Parallel()

	got := SessionListMarkup([]int{1}, nil)
	if got == nil || len(got.InlineKeyboard) != 1 || len(got.InlineKeyboard[0]) != 2 {
		t.Fatalf("SessionListMarkup([1]) = %#v", got)
	}
}

func TestSessionListMarkupWithAttachTargets(t *testing.T) {
	t.Parallel()

	got := SessionListMarkup(nil, []AttachTarget{{Label: "0:1", Target: "0:1"}})
	if got == nil || len(got.InlineKeyboard) != 1 || got.InlineKeyboard[0][0].CallbackData != "attach:0:1" {
		t.Fatalf("SessionListMarkup attach = %#v", got)
	}
}

func TestRefreshMarkupIncludesKeyButtons(t *testing.T) {
	t.Parallel()

	got := RefreshMarkup(7)
	if got == nil || len(got.InlineKeyboard) != 2 {
		t.Fatalf("RefreshMarkup rows = %#v, want refresh row plus key row", got)
	}
	if got.InlineKeyboard[0][0].CallbackData != "refresh:7" {
		t.Fatalf("refresh callback = %q", got.InlineKeyboard[0][0].CallbackData)
	}
	want := []InlineKeyboardButton{
		{Text: "Esc", CallbackData: "key:7:esc"},
		{Text: "Esc Esc", CallbackData: "key:7:esc2"},
		{Text: "Ctrl+C", CallbackData: "key:7:ctrl-c"},
		{Text: "Ctrl+D", CallbackData: "key:7:ctrl-d"},
		{Text: "Enter", CallbackData: "key:7:enter"},
	}
	if len(got.InlineKeyboard[1]) != len(want) {
		t.Fatalf("key button count = %d, want %d", len(got.InlineKeyboard[1]), len(want))
	}
	for i := range want {
		if got.InlineKeyboard[1][i] != want[i] {
			t.Fatalf("key button %d = %#v, want %#v", i, got.InlineKeyboard[1][i], want[i])
		}
	}
}

func TestMarkdownToHTML(t *testing.T) {
	t.Parallel()

	got := MarkdownToHTML("**Status:** `ok` <raw>")
	want := "<b>Status:</b> <code>ok</code> &lt;raw&gt;"
	if got != want {
		t.Fatalf("MarkdownToHTML = %q, want %q", got, want)
	}
}

func TestMarkdownToHTMLCodeFence(t *testing.T) {
	t.Parallel()

	got := MarkdownToHTML("before\n```\n<a>\n```\nafter")
	want := "before\n<pre>&lt;a&gt;</pre>\nafter"
	if got != want {
		t.Fatalf("MarkdownToHTML code fence = %q, want %q", got, want)
	}
}

func TestMarkdownToHTMLBlockquote(t *testing.T) {
	t.Parallel()

	got := MarkdownToHTML("evidence:\n> error: <denied>\n> retry with --force\n\nnext")
	want := "evidence:\n<blockquote>error: &lt;denied&gt;\nretry with --force</blockquote>\nnext"
	if got != want {
		t.Fatalf("MarkdownToHTML blockquote = %q, want %q", got, want)
	}
}

func TestSendHTMLMessagePayload(t *testing.T) {
	t.Parallel()

	var got map[string]any
	client := New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/botTOKEN/sendMessage" {
			t.Fatalf("path = %s", req.URL.Path)
		}
		got = decodeRequestMap(t, req)
		return jsonResponse(t, map[string]any{
			"ok": true,
			"result": map[string]any{
				"message_id": 10,
				"chat":       map[string]any{"id": 5},
			},
		}), nil
	})}

	if _, err := client.SendHTMLMessage(context.Background(), 5, "<b>ok</b>", 7, RefreshMarkup(1)); err != nil {
		t.Fatal(err)
	}
	if got["parse_mode"] != "HTML" {
		t.Fatalf("parse_mode = %#v, want HTML", got["parse_mode"])
	}
	if got["reply_to_message_id"] != float64(7) {
		t.Fatalf("reply_to_message_id = %#v, want 7", got["reply_to_message_id"])
	}
	if _, ok := got["reply_markup"].(map[string]any); !ok {
		t.Fatalf("reply_markup = %#v, want object", got["reply_markup"])
	}
}

func TestEditHTMLMessagePayload(t *testing.T) {
	t.Parallel()

	var got map[string]any
	client := New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/botTOKEN/editMessageText" {
			t.Fatalf("path = %s", req.URL.Path)
		}
		got = decodeRequestMap(t, req)
		return jsonResponse(t, map[string]any{
			"ok": true,
			"result": map[string]any{
				"message_id": 11,
				"chat":       map[string]any{"id": 5},
			},
		}), nil
	})}

	if _, err := client.EditHTMLMessage(context.Background(), 5, 11, "<b>ok</b>", nil); err != nil {
		t.Fatal(err)
	}
	if got["parse_mode"] != "HTML" {
		t.Fatalf("parse_mode = %#v, want HTML", got["parse_mode"])
	}
	if got["message_id"] != float64(11) {
		t.Fatalf("message_id = %#v, want 11", got["message_id"])
	}
	if _, ok := got["reply_markup"]; ok {
		t.Fatalf("reply_markup present = %#v, want omitted", got["reply_markup"])
	}
}

func TestRequestErrorsRedactToken(t *testing.T) {
	t.Parallel()

	const token = "123456:telegram-secret-token"
	documentPath := filepath.Join(t.TempDir(), "report.txt")
	if err := os.WriteFile(documentPath, []byte("report"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		method string
		call   func(*Client) error
	}{
		{
			name:   "getFile",
			method: "getFile",
			call: func(client *Client) error {
				_, err := client.GetFile(context.Background(), "file-1")
				return err
			},
		},
		{
			name:   "sendMessage",
			method: "sendMessage",
			call: func(client *Client) error {
				_, err := client.SendMessage(context.Background(), 1, "hello", 0, nil)
				return err
			},
		},
		{
			name:   "sendDocument",
			method: "sendDocument",
			call: func(client *Client) error {
				_, err := client.SendDocument(context.Background(), 1, documentPath, "report")
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := New(token)
			client.outboundInterval = 0
			client.HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return nil, fmt.Errorf("request to %s with token %s failed", req.URL.String(), token)
			})}

			err := tt.call(client)
			if err == nil {
				t.Fatal("call succeeded, want error")
			}
			var telegramErr *Error
			if !errors.As(err, &telegramErr) {
				t.Fatalf("error type = %T, want *telegram.Error", err)
			}
			if telegramErr.Method != tt.method {
				t.Fatalf("Method = %q, want %q", telegramErr.Method, tt.method)
			}
			got := err.Error()
			if strings.Contains(got, token) || strings.Contains(got, "https://") || strings.Contains(got, "/bot") {
				t.Fatalf("error contains request secret or URL: %q", got)
			}
		})
	}
}

func TestTelegramErrorParsesParameters(t *testing.T) {
	t.Parallel()

	client := New("TOKEN")
	client.HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return jsonResponseStatus(t, http.StatusBadRequest, map[string]any{
			"ok":          false,
			"error_code":  400,
			"description": "Bad Request: group chat was upgraded",
			"parameters": map[string]any{
				"migrate_to_chat_id": int64(-1001234567890),
			},
		}), nil
	})}

	_, err := client.GetFile(context.Background(), "file-1")
	var telegramErr *Error
	if !errors.As(err, &telegramErr) {
		t.Fatalf("error = %v (%T), want *telegram.Error", err, err)
	}
	if telegramErr.Method != "getFile" || telegramErr.StatusCode != 400 || telegramErr.ErrorCode != 400 {
		t.Fatalf("error metadata = %#v", telegramErr)
	}
	if telegramErr.MigrateToChatID != -1001234567890 {
		t.Fatalf("MigrateToChatID = %d", telegramErr.MigrateToChatID)
	}
}

func TestAPIDescriptionRedactsRequestURL(t *testing.T) {
	t.Parallel()

	const token = "123456:telegram-secret-token"
	client := New(token)
	client.outboundInterval = 0
	client.HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return jsonResponseStatus(t, http.StatusBadRequest, map[string]any{
			"ok":          false,
			"error_code":  400,
			"description": fmt.Sprintf("request %s (%s) was rejected", req.URL.String(), req.URL.Path),
		}), nil
	})}

	_, err := client.SendMessage(context.Background(), 5, "hello", 0, nil)
	var telegramErr *Error
	if !errors.As(err, &telegramErr) {
		t.Fatalf("error = %v (%T), want *telegram.Error", err, err)
	}
	if strings.Contains(telegramErr.Description, token) || strings.Contains(telegramErr.Description, "https://") || strings.Contains(telegramErr.Description, "/bot") {
		t.Fatalf("Description leaked request details: %q", telegramErr.Description)
	}
}

func TestRateLimitRetriesOnceAfterRetryAfter(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	var delays []time.Duration
	client := New("TOKEN")
	client.outboundInterval = 0
	client.retrySleep = func(ctx context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		return nil
	}
	client.HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return jsonResponseStatus(t, http.StatusTooManyRequests, map[string]any{
				"ok":          false,
				"error_code":  429,
				"description": "Too Many Requests: retry later",
				"parameters":  map[string]any{"retry_after": 3},
			}), nil
		}
		return jsonResponse(t, map[string]any{
			"ok": true,
			"result": map[string]any{
				"message_id": 12,
				"chat":       map[string]any{"id": 5},
			},
		}), nil
	})}

	msg, err := client.SendMessage(context.Background(), 5, "hello", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if msg.MessageID != 12 || calls.Load() != 2 {
		t.Fatalf("message = %#v, calls = %d", msg, calls.Load())
	}
	if len(delays) != 1 || delays[0] != 3*time.Second {
		t.Fatalf("retry delays = %v, want [3s]", delays)
	}
}

func TestSendDocumentRetryRebuildsMultipartBody(t *testing.T) {
	t.Parallel()

	documentPath := filepath.Join(t.TempDir(), "report.txt")
	if err := os.WriteFile(documentPath, []byte("report-content"), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	client := New("TOKEN")
	client.outboundInterval = 0
	client.retrySleep = func(context.Context, time.Duration) error { return nil }
	client.HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(body, []byte("report-content")) {
			t.Fatalf("multipart attempt %d omitted document content", calls.Load()+1)
		}
		if calls.Add(1) == 1 {
			return jsonResponseStatus(t, http.StatusTooManyRequests, map[string]any{
				"ok":          false,
				"error_code":  429,
				"description": "Too Many Requests",
				"parameters":  map[string]any{"retry_after": 1},
			}), nil
		}
		return jsonResponse(t, map[string]any{
			"ok":     true,
			"result": map[string]any{"message_id": 13, "chat": map[string]any{"id": 5}},
		}), nil
	})}

	msg, err := client.SendDocument(context.Background(), 5, documentPath, "report")
	if err != nil {
		t.Fatal(err)
	}
	if msg.MessageID != 13 || calls.Load() != 2 {
		t.Fatalf("message = %#v, calls = %d", msg, calls.Load())
	}
}

func TestRateLimitAboveBoundIsNotRetried(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	client := New("TOKEN")
	client.outboundInterval = 0
	client.retrySleep = func(context.Context, time.Duration) error {
		t.Fatal("unexpected retry sleep")
		return nil
	}
	client.HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return jsonResponseStatus(t, http.StatusTooManyRequests, map[string]any{
			"ok":          false,
			"error_code":  429,
			"description": "Too Many Requests",
			"parameters":  map[string]any{"retry_after": 31},
		}), nil
	})}

	_, err := client.SendMessage(context.Background(), 5, "hello", 0, nil)
	if !IsRateLimited(err) {
		t.Fatalf("error = %v, want rate limited", err)
	}
	var telegramErr *Error
	if !errors.As(err, &telegramErr) || telegramErr.RetryAfter != 31*time.Second {
		t.Fatalf("error = %#v, want RetryAfter 31s", telegramErr)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
}

func TestMessageNotModifiedClassification(t *testing.T) {
	t.Parallel()

	client := New("TOKEN")
	client.outboundInterval = 0
	client.HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return jsonResponseStatus(t, http.StatusBadRequest, map[string]any{
			"ok":          false,
			"error_code":  400,
			"description": "Bad Request: message is not modified: specified content is unchanged",
		}), nil
	})}

	_, err := client.EditMessage(context.Background(), 5, 11, "same", nil)
	if !IsMessageNotModified(err) {
		t.Fatalf("error = %v, want message-not-modified classification", err)
	}
	var telegramErr *Error
	if !errors.As(err, &telegramErr) || !telegramErr.IsMessageNotModified() {
		t.Fatalf("error = %#v, want classified *telegram.Error", telegramErr)
	}
	if IsMessageNotModified(errors.New("message is not modified")) {
		t.Fatal("plain string error was classified as Telegram not-modified error")
	}
}

func TestParseAndTransportErrorsAreSanitized(t *testing.T) {
	t.Parallel()

	t.Run("parse", func(t *testing.T) {
		client := New("TOKEN")
		client.HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusBadGateway,
				Status:     "502 Bad Gateway",
				Body:       io.NopCloser(strings.NewReader("not-json")),
				Header:     make(http.Header),
			}, nil
		})}

		_, err := client.GetFile(context.Background(), "file-1")
		var telegramErr *Error
		if !errors.As(err, &telegramErr) {
			t.Fatalf("error = %v (%T), want *telegram.Error", err, err)
		}
		if telegramErr.Method != "getFile" || telegramErr.StatusCode != http.StatusBadGateway || telegramErr.Description != "invalid Telegram response" {
			t.Fatalf("parse error = %#v", telegramErr)
		}
	})

	t.Run("transport", func(t *testing.T) {
		const token = "transport-secret-token"
		client := New(token)
		client.HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("dial %s: token=%s", req.URL, token)
		})}

		_, err := client.GetFile(context.Background(), "file-1")
		var telegramErr *Error
		if !errors.As(err, &telegramErr) {
			t.Fatalf("error = %v (%T), want *telegram.Error", err, err)
		}
		if telegramErr.Description != "transport request failed" {
			t.Fatalf("Description = %q", telegramErr.Description)
		}
		if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "https://") {
			t.Fatalf("transport error leaked request details: %q", err)
		}
	})
}

func TestRetrySleepPreservesContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	client := New("TOKEN")
	client.outboundInterval = 0
	client.retrySleep = func(ctx context.Context, delay time.Duration) error {
		cancel()
		return sleepContext(ctx, delay)
	}
	client.HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return jsonResponseStatus(t, http.StatusTooManyRequests, map[string]any{
			"ok":          false,
			"error_code":  429,
			"description": "Too Many Requests",
			"parameters":  map[string]any{"retry_after": 1},
		}), nil
	})}

	_, err := client.SendMessage(ctx, 5, "hello", 0, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestOutboundConcurrencyIsBounded(t *testing.T) {
	t.Parallel()

	var active atomic.Int32
	var maximum atomic.Int32
	started := make(chan struct{}, 8)
	release := make(chan struct{})
	client := New("TOKEN")
	client.outboundInterval = 0
	client.HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		current := active.Add(1)
		for {
			old := maximum.Load()
			if current <= old || maximum.CompareAndSwap(old, current) {
				break
			}
		}
		started <- struct{}{}
		<-release
		active.Add(-1)
		return jsonResponse(t, map[string]any{
			"ok":     true,
			"result": map[string]any{"message_id": 1, "chat": map[string]any{"id": 5}},
		}), nil
	})}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := client.SendMessage(context.Background(), 5, "hello", 0, nil); err != nil {
				t.Errorf("SendMessage: %v", err)
			}
		}()
	}
	for i := 0; i < maxConcurrentOutbound; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for outbound requests")
		}
	}
	select {
	case <-started:
		t.Fatal("more than four outbound requests started concurrently")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	wg.Wait()
	if maximum.Load() != maxConcurrentOutbound {
		t.Fatalf("maximum concurrency = %d, want %d", maximum.Load(), maxConcurrentOutbound)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func decodeRequestMap(t *testing.T, req *http.Request) map[string]any {
	t.Helper()
	data, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func jsonResponse(t *testing.T, payload map[string]any) *http.Response {
	return jsonResponseStatus(t, http.StatusOK, payload)
}

func jsonResponseStatus(t *testing.T, statusCode int, payload map[string]any) *http.Response {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Response{
		StatusCode: statusCode,
		Status:     fmt.Sprintf("%d %s", statusCode, http.StatusText(statusCode)),
		Body:       io.NopCloser(bytes.NewReader(data)),
		Header:     make(http.Header),
	}
}
