package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
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
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Body:       io.NopCloser(bytes.NewReader(data)),
		Header:     make(http.Header),
	}
}
