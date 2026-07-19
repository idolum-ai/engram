package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func TestNewAtDerivesMethodAndFileEndpoints(t *testing.T) {
	client, err := NewAt("TOKEN", "http://127.0.0.1:8081/telegram/")
	if err != nil {
		t.Fatal(err)
	}
	if client.BaseURL != "http://127.0.0.1:8081/telegram/botTOKEN" || client.FileBase != "http://127.0.0.1:8081/telegram/file/botTOKEN" {
		t.Fatalf("custom Telegram endpoints = %q, %q", client.BaseURL, client.FileBase)
	}
}

func TestCustomAPIBaseRoutesMethodsAndFilesThroughPrefix(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/telegram/botTOKEN/getUpdates":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ok":true,"result":[]}`)
		case "/telegram/file/botTOKEN/documents/result.txt":
			_, _ = io.WriteString(w, "artifact")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewAt("TOKEN", server.URL+"/telegram/")
	if err != nil {
		t.Fatal(err)
	}
	client.HTTPClient = server.Client()
	if _, err := client.GetUpdates(context.Background(), 0, 1); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "result.txt")
	if _, err := client.DownloadFile(context.Background(), "documents/result.txt", destination, 1024); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(paths, "|"), "/telegram/botTOKEN/getUpdates|/telegram/file/botTOKEN/documents/result.txt"; got != want {
		t.Fatalf("request paths = %q, want %q", got, want)
	}
}

func TestSessionListMarkupNilWhenNoSessions(t *testing.T) {
	t.Parallel()

	if got := SessionListMarkup(nil, nil); got != nil {
		t.Fatalf("SessionListMarkup(nil, nil) = %#v, want nil", got)
	}
}

func TestSessionListMarkupWithSessions(t *testing.T) {
	t.Parallel()

	got := SessionListMarkup([]SessionAction{{ID: 1, Token: "abc"}}, nil)
	if got == nil || len(got.InlineKeyboard) != 1 || len(got.InlineKeyboard[0]) != 2 {
		t.Fatalf("SessionListMarkup([1]) = %#v", got)
	}
	if got.InlineKeyboard[0][0].Text != "▶ 1" || got.InlineKeyboard[0][1].Text != "✕ 1" {
		t.Fatalf("session actions = %#v", got.InlineKeyboard[0])
	}
	if got.InlineKeyboard[0][0].CallbackData != "session-watch:1:abc" || got.InlineKeyboard[0][1].CallbackData != "session-close:1:abc" {
		t.Fatalf("session callback actions = %#v", got.InlineKeyboard[0])
	}
}

func TestSessionListMarkupWithAttachTargets(t *testing.T) {
	t.Parallel()

	got := SessionListMarkup(nil, []AttachTarget{{Label: "0:1", Target: "0:1"}})
	if got == nil || len(got.InlineKeyboard) != 1 || got.InlineKeyboard[0][0].Text != "↪ 0:1" || got.InlineKeyboard[0][0].CallbackData != "attach:0:1" {
		t.Fatalf("SessionListMarkup attach = %#v", got)
	}
}

func TestSnapshotAnchorMarkupIncludesAvailableAlternateAndKeyButtons(t *testing.T) {
	t.Parallel()

	got := AnchorMarkup(7, AnchorMarkupOptions{Image: true, Arrows: true})
	if got == nil || len(got.InlineKeyboard) != 3 || len(got.InlineKeyboard[0]) != 2 {
		t.Fatalf("AnchorMarkup rows = %#v, want action, key, and arrow rows", got)
	}
	if got.InlineKeyboard[0][0].CallbackData != "refresh:7" {
		t.Fatalf("refresh callback = %q", got.InlineKeyboard[0][0].CallbackData)
	}
	if got.InlineKeyboard[0][1].Text != "🖼️ View" || got.InlineKeyboard[0][1].CallbackData != "snapshot:7" {
		t.Fatalf("snapshot callback = %#v", got.InlineKeyboard[0][1])
	}
	want := []InlineKeyboardButton{
		{Text: "Esc", CallbackData: "key:7:esc"},
		{Text: "Escx2", CallbackData: "key:7:esc2"},
		{Text: "^C", CallbackData: "key:7:ctrl-c"},
		{Text: "^D", CallbackData: "key:7:ctrl-d"},
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
	wantArrows := []InlineKeyboardButton{
		{Text: "←", CallbackData: "key:7:left"},
		{Text: "↑", CallbackData: "key:7:up"},
		{Text: "↓", CallbackData: "key:7:down"},
		{Text: "→", CallbackData: "key:7:right"},
	}
	if len(got.InlineKeyboard[2]) != len(wantArrows) {
		t.Fatalf("arrow button count = %d, want %d", len(got.InlineKeyboard[2]), len(wantArrows))
	}
	for i := range wantArrows {
		if got.InlineKeyboard[2][i] != wantArrows[i] {
			t.Fatalf("arrow button %d = %#v, want %#v", i, got.InlineKeyboard[2][i], wantArrows[i])
		}
	}
}

func TestGuideAnchorMarkupOmitsArrowButtons(t *testing.T) {
	t.Parallel()

	got := AnchorMarkup(7, AnchorMarkupOptions{Image: true})
	if got == nil || len(got.InlineKeyboard) != 2 {
		t.Fatalf("AnchorMarkup rows = %#v, want action and key rows", got)
	}
}

func TestSnapshotAnchorMarkupOffersRawTextCompanion(t *testing.T) {
	t.Parallel()

	got := AnchorMarkup(7, AnchorMarkupOptions{Voice: true, Raw: true, Arrows: true})
	if got == nil || len(got.InlineKeyboard[0]) != 3 {
		t.Fatalf("AnchorMarkup actions = %#v", got)
	}
	if want := (InlineKeyboardButton{Text: "🗣️ Talk", CallbackData: "voice:7"}); got.InlineKeyboard[0][1] != want {
		t.Fatalf("explain action = %#v, want %#v", got.InlineKeyboard[0][1], want)
	}
	want := InlineKeyboardButton{Text: "📄 Raw", CallbackData: "raw:7"}
	if got.InlineKeyboard[0][2] != want {
		t.Fatalf("raw action = %#v, want %#v", got.InlineKeyboard[0][2], want)
	}
}

func TestAnchorMarkupAddsNumberedFileButtons(t *testing.T) {
	t.Parallel()

	got := AnchorMarkup(7, AnchorMarkupOptions{FileToken: "0123456789abcdef", FileCount: 2})
	if got == nil || len(got.InlineKeyboard) != 3 {
		t.Fatalf("AnchorMarkup rows = %#v, want actions, files, and keys", got)
	}
	want := []InlineKeyboardButton{
		{Text: "⬇️ 1", CallbackData: "file:7:0123456789abcdef:1"},
		{Text: "⬇️ 2", CallbackData: "file:7:0123456789abcdef:2"},
	}
	if !reflect.DeepEqual(got.InlineKeyboard[1], want) {
		t.Fatalf("file buttons = %#v, want %#v", got.InlineKeyboard[1], want)
	}
}

func TestRecoverMarkupOffersExactReattach(t *testing.T) {
	t.Parallel()

	got := RecoverMarkup(7, false)
	want := InlineKeyboardButton{Text: "🧭 Link", CallbackData: "recover:7"}
	if got == nil || len(got.InlineKeyboard) != 1 || len(got.InlineKeyboard[0]) != 1 || got.InlineKeyboard[0][0] != want {
		t.Fatalf("RecoverMarkup(7, false) = %#v, want %#v", got, want)
	}
}

func TestRecoveryMarkupsOfferExactResumeAndDismiss(t *testing.T) {
	t.Parallel()
	recover := RecoverMarkup(7, true)
	if got := recover.InlineKeyboard[0][0]; got.CallbackData != "resume:7" {
		t.Fatalf("resume button = %#v", got)
	}
	plan := RecoveryPlanMarkup([]SessionAction{{ID: 7, Token: "aaa"}, {ID: 9, Token: "bbb"}})
	if len(plan.InlineKeyboard) != 3 || plan.InlineKeyboard[0][0].CallbackData != "plan-resume:7:aaa" || plan.InlineKeyboard[2][0].CallbackData != "plan-dismiss:all" {
		t.Fatalf("recovery plan markup = %#v", plan)
	}
}

// Do not skip, weaken, replace, or supersede this test for any Telegram button.
// Every button builder belongs here: Telegram truncation makes longer labels an
// application-level usability failure, even when the callback remains valid.
func TestAllInlineButtonLabelsFitCompactBudget(t *testing.T) {
	t.Parallel()

	maxRunes := utf8.RuneCountInString("🖼️ View")
	markups := map[string]*InlineKeyboardMarkup{
		"anchor": AnchorMarkup(123456789, AnchorMarkupOptions{
			Image: true, Voice: true, Raw: true, Arrows: true,
			FileToken: "0123456789abcdef", FileCount: 4,
		}),
		"recover":       RecoverMarkup(123456789, true),
		"recovery plan": RecoveryPlanMarkup([]SessionAction{{ID: 1, Token: "aaa"}, {ID: 123456789, Token: "bbb"}}),
		"sessions": SessionListMarkup(
			[]SessionAction{{ID: 1, Token: "aaa"}, {ID: 123456789, Token: "bbb"}},
			[]AttachTarget{{Label: "long-session:window-name", Target: "long-session:window-name"}},
		),
		"close confirmation": CloseConfirmationMarkup("0123456789abcdef"),
	}
	for name, markup := range markups {
		for rowIndex, row := range markup.InlineKeyboard {
			for buttonIndex, button := range row {
				if runes := utf8.RuneCountInString(button.Text); runes > maxRunes {
					t.Errorf("%s row %d button %d label %q has %d runes, max %d", name, rowIndex, buttonIndex, button.Text, runes, maxRunes)
				}
			}
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

func TestDownloadFileHashesWhileStreaming(t *testing.T) {
	t.Parallel()
	dest := filepath.Join(t.TempDir(), "download.bin")
	client := New("TOKEN")
	client.FileBase = "https://api.telegram.org/file/botTOKEN"
	client.HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       io.NopCloser(strings.NewReader("abc")),
			Header:     make(http.Header),
		}, nil
	})}
	result, err := client.DownloadFileHashed(context.Background(), "docs/a.bin", dest, 10)
	if err != nil {
		t.Fatal(err)
	}
	if result.Size != 3 || result.SHA256 != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("download result = %#v", result)
	}
	data, err := os.ReadFile(dest)
	if err != nil || string(data) != "abc" {
		t.Fatalf("downloaded data = %q, err=%v", data, err)
	}
}

func TestDownloadFileRemovesOversizedPartial(t *testing.T) {
	t.Parallel()
	dest := filepath.Join(t.TempDir(), "partial.bin")
	client := New("TOKEN")
	client.FileBase = "https://api.telegram.org/file/botTOKEN"
	client.HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader("123456")), Header: make(http.Header)}, nil
	})}
	result, err := client.DownloadFileHashed(context.Background(), "docs/a.bin", dest, 5)
	if err == nil || result.Size != 6 {
		t.Fatalf("download result = %#v, err=%v", result, err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("partial download still exists: %v", err)
	}
}

func TestClampTextPreservesUTF8(t *testing.T) {
	t.Parallel()
	text := strings.Repeat("a", 3799) + "é" + strings.Repeat("b", 200)
	got := clampText(text)
	if !strings.Contains(got, "[truncated]") || !utf8.ValidString(got) {
		t.Fatalf("clampText returned invalid truncation")
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

	if _, err := client.SendHTMLMessage(context.Background(), 5, "<b>ok</b>", 7, AnchorMarkup(1, AnchorMarkupOptions{Image: true})); err != nil {
		t.Fatal(err)
	}
	if got["parse_mode"] != "HTML" {
		t.Fatalf("parse_mode = %#v, want HTML", got["parse_mode"])
	}
	reply, ok := got["reply_parameters"].(map[string]any)
	if !ok || reply["message_id"] != float64(7) {
		t.Fatalf("reply_parameters = %#v, want message_id 7", got["reply_parameters"])
	}
	preview, ok := got["link_preview_options"].(map[string]any)
	if !ok || preview["is_disabled"] != true {
		t.Fatalf("link_preview_options = %#v, want disabled", got["link_preview_options"])
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

func TestPinAndUnpinChatMessagePayloads(t *testing.T) {
	t.Parallel()

	var requests []map[string]any
	var paths []string
	client := New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.Path)
		requests = append(requests, decodeRequestMap(t, req))
		return jsonResponse(t, map[string]any{"ok": true, "result": true}), nil
	})}

	if err := client.PinChatMessage(context.Background(), 5, 11); err != nil {
		t.Fatal(err)
	}
	if err := client.UnpinChatMessage(context.Background(), 5, 10); err != nil {
		t.Fatal(err)
	}
	wantPaths := []string{"/botTOKEN/pinChatMessage", "/botTOKEN/unpinChatMessage"}
	if !reflect.DeepEqual(paths, wantPaths) {
		t.Fatalf("paths = %#v, want %#v", paths, wantPaths)
	}
	if requests[0]["chat_id"] != float64(5) || requests[0]["message_id"] != float64(11) || requests[0]["disable_notification"] != true {
		t.Fatalf("pin payload = %#v", requests[0])
	}
	if requests[1]["chat_id"] != float64(5) || requests[1]["message_id"] != float64(10) {
		t.Fatalf("unpin payload = %#v", requests[1])
	}
}

func TestPinErrorClassifiers(t *testing.T) {
	t.Parallel()
	if !IsMessageAlreadyPinned(&Error{Description: "Bad Request: message is already pinned"}) {
		t.Fatal("already-pinned error was not classified")
	}
	if !IsMessageNotPinned(&Error{Description: "Bad Request: message is not pinned"}) {
		t.Fatal("not-pinned error was not classified")
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

func TestSendDocumentNamedUsesVisibleFilename(t *testing.T) {
	t.Parallel()

	snapshotPath := filepath.Join(t.TempDir(), "engram-download-random.bin")
	if err := os.WriteFile(snapshotPath, []byte("proposal"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := New("TOKEN")
	client.outboundInterval = 0
	client.HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if err := req.ParseMultipartForm(1024); err != nil {
			t.Fatal(err)
		}
		files := req.MultipartForm.File["document"]
		if len(files) != 1 || files[0].Filename != "engram-coherence-proposal.md" {
			t.Fatalf("multipart filename = %#v", files)
		}
		return jsonResponse(t, map[string]any{
			"ok":     true,
			"result": map[string]any{"message_id": 13, "chat": map[string]any{"id": 5}},
		}), nil
	})}

	if _, err := client.SendDocumentNamed(context.Background(), 5, snapshotPath, "engram-coherence-proposal.md", "proposal"); err != nil {
		t.Fatal(err)
	}
}

func TestSendPhotoRepliesToCanonicalAnchor(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "snapshot.png")
	if err := os.WriteFile(path, []byte("png-content"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := New("TOKEN")
	client.outboundInterval = 0
	client.HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/botTOKEN/sendPhoto" {
			t.Fatalf("send photo path = %q", req.URL.Path)
		}
		if err := req.ParseMultipartForm(1024); err != nil {
			t.Fatal(err)
		}
		files := req.MultipartForm.File["photo"]
		if len(files) != 1 || files[0].Filename != "engram-window.png" {
			t.Fatalf("photo files = %#v", files)
		}
		var reply map[string]int
		if err := json.Unmarshal([]byte(req.FormValue("reply_parameters")), &reply); err != nil || reply["message_id"] != 77 {
			t.Fatalf("photo reply_parameters = %q, err=%v", req.FormValue("reply_parameters"), err)
		}
		if got := req.FormValue("caption"); got != "terminal snapshot" {
			t.Fatalf("photo caption = %q", got)
		}
		return jsonResponse(t, map[string]any{
			"ok":     true,
			"result": map[string]any{"message_id": 14, "chat": map[string]any{"id": 5}},
		}), nil
	})}
	msg, err := client.SendPhoto(context.Background(), 5, path, "terminal snapshot", 77)
	if err != nil || msg.MessageID != 14 {
		t.Fatalf("SendPhoto message = %#v err=%v", msg, err)
	}
}

func TestSendHTMLPhotoIncludesMarkupParseModeAndStablePlacementWithoutReplyTarget(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "snapshot.png")
	if err := os.WriteFile(path, []byte("png-content"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := New("TOKEN")
	client.outboundInterval = 0
	client.HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if err := req.ParseMultipartForm(1024); err != nil {
			t.Fatal(err)
		}
		if got := req.FormValue("reply_parameters"); got != "" {
			t.Fatalf("photo reply = %q, want empty", got)
		}
		if got := req.FormValue("parse_mode"); got != "HTML" {
			t.Fatalf("photo parse mode = %q, want HTML", got)
		}
		if got := req.FormValue("show_caption_above_media"); got != "false" {
			t.Fatalf("caption placement = %q, want false", got)
		}
		if got := req.FormValue("reply_markup"); !strings.Contains(got, "refresh:7") || strings.Contains(got, "snapshot:7") {
			t.Fatalf("snapshot markup = %q", got)
		}
		return jsonResponse(t, map[string]any{
			"ok":     true,
			"result": map[string]any{"message_id": 14, "chat": map[string]any{"id": 5}},
		}), nil
	})}
	if _, err := client.SendHTMLPhotoWithMarkup(context.Background(), 5, path, "terminal snapshot", 0, AnchorMarkup(7, AnchorMarkupOptions{Voice: true, Arrows: true})); err != nil {
		t.Fatal(err)
	}
}

func TestEditHTMLPhotoUsesAttachedMediaMarkupParseModeAndStablePlacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.png")
	if err := os.WriteFile(path, []byte("png-content"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := New("TOKEN")
	client.outboundInterval = 0
	client.HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/botTOKEN/editMessageMedia" {
			t.Fatalf("edit photo path = %q", req.URL.Path)
		}
		if err := req.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		if req.FormValue("chat_id") != "5" || req.FormValue("message_id") != "77" {
			t.Fatalf("edit identity chat=%q message=%q", req.FormValue("chat_id"), req.FormValue("message_id"))
		}
		var media map[string]any
		if err := json.Unmarshal([]byte(req.FormValue("media")), &media); err != nil {
			t.Fatal(err)
		}
		if media["type"] != "photo" || media["media"] != "attach://photo" || media["caption"] != "live terminal" || media["parse_mode"] != "HTML" || media["show_caption_above_media"] != false {
			t.Fatalf("media = %#v", media)
		}
		if !strings.Contains(req.FormValue("reply_markup"), "refresh:7") || strings.Contains(req.FormValue("reply_markup"), "snapshot:7") {
			t.Fatalf("snapshot markup = %q", req.FormValue("reply_markup"))
		}
		files := req.MultipartForm.File["photo"]
		if len(files) != 1 || files[0].Filename != "engram-window.png" {
			t.Fatalf("photo files = %#v", files)
		}
		return jsonResponse(t, map[string]any{
			"ok":     true,
			"result": map[string]any{"message_id": 77, "chat": map[string]any{"id": 5}},
		}), nil
	})}
	msg, err := client.EditHTMLPhoto(context.Background(), 5, 77, path, "live terminal", AnchorMarkup(7, AnchorMarkupOptions{Voice: true, Arrows: true}))
	if err != nil || msg.MessageID != 77 {
		t.Fatalf("EditPhoto message = %#v err=%v", msg, err)
	}
}

func TestHTMLPhotoCaptionIsNotByteTruncatedAfterEscaping(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.png")
	if err := os.WriteFile(path, []byte("png-content"), 0o600); err != nil {
		t.Fatal(err)
	}
	caption := strings.Repeat("&amp;", 240)
	client := New("TOKEN")
	client.outboundInterval = 0
	client.HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if err := req.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		if got := req.FormValue("caption"); got != caption {
			t.Fatalf("HTML caption bytes=%d suffix=%q, want intact bytes=%d", len(got), tailForTest(got, 20), len(caption))
		}
		return jsonResponse(t, map[string]any{"ok": true, "result": map[string]any{"message_id": 14, "chat": map[string]any{"id": 5}}}), nil
	})}
	if _, err := client.SendHTMLPhotoWithMarkup(context.Background(), 5, path, caption, 0, nil); err != nil {
		t.Fatal(err)
	}
}

func TestHTMLCaptionEditIsNotByteTruncatedAfterEscaping(t *testing.T) {
	caption := strings.Repeat("&amp;", 240)
	client := New("TOKEN")
	client.outboundInterval = 0
	client.HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if got, _ := body["caption"].(string); got != caption {
			t.Fatalf("HTML caption bytes=%d suffix=%q, want intact bytes=%d", len(got), tailForTest(got, 20), len(caption))
		}
		if body["parse_mode"] != "HTML" {
			t.Fatalf("parse mode = %#v", body["parse_mode"])
		}
		return jsonResponse(t, map[string]any{"ok": true, "result": map[string]any{"message_id": 14, "chat": map[string]any{"id": 5}}}), nil
	})}
	if _, err := client.EditHTMLCaption(context.Background(), 5, 14, caption, nil); err != nil {
		t.Fatal(err)
	}
}

func tailForTest(text string, count int) string {
	if len(text) <= count {
		return text
	}
	return text[len(text)-count:]
}

func TestSafeDocumentFilenameRemovesControlCharacters(t *testing.T) {
	t.Parallel()
	if got := safeDocumentFilename("../report\n.md"); got != "report_.md" {
		t.Fatalf("safeDocumentFilename = %q", got)
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
