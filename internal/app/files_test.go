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

	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
)

func TestHandleAttachmentEnforcesSoftLimitDuringDownload(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, "tmp")
	t.Setenv("TMPDIR", tmp)
	store, err := state.Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var replies []string
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.FileBase = "https://api.telegram.org/file/botTOKEN"
	client.HTTPClient = &http.Client{Transport: fileRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/botTOKEN/getFile":
			return fileJSONResponse(t, map[string]any{
				"ok": true,
				"result": map[string]any{
					"file_id":   "file-1",
					"file_path": "docs/file.bin",
				},
			}), nil
		case "/file/botTOKEN/docs/file.bin":
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Body:       io.NopCloser(strings.NewReader("123456")),
				Header:     make(http.Header),
			}, nil
		case "/botTOKEN/sendMessage":
			var payload map[string]any
			if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			replies = append(replies, payload["text"].(string))
			return fileJSONResponse(t, map[string]any{
				"ok":     true,
				"result": map[string]any{"message_id": 2, "chat": map[string]any{"id": 100}},
			}), nil
		default:
			t.Fatalf("unexpected request path %s", req.URL.Path)
			return nil, nil
		}
	})}
	app := &App{
		Config: config.Config{
			Home:                   dir,
			AttachmentSoftMaxBytes: 5,
			TelegramChatID:         100,
		},
		Store:    store,
		Telegram: client,
	}

	app.handleAttachment(context.Background(), telegram.Message{
		MessageID: 1,
		Chat:      telegram.Chat{ID: 100},
		From:      &telegram.User{ID: 42},
	}, telegram.Document{
		FileID:   "file-1",
		FileName: "file.bin",
		FileSize: 0,
	})

	if len(replies) != 1 || !strings.Contains(replies[0], "attachment too large") {
		t.Fatalf("replies = %#v, want too-large reply", replies)
	}
	if got := store.Snapshot().Attachments; len(got) != 0 {
		t.Fatalf("attachments = %#v, want none", got)
	}
	if entries, err := os.ReadDir(app.Config.AttachmentDir()); err == nil {
		for _, entry := range entries {
			if strings.Contains(entry.Name(), "file-1") {
				t.Fatalf("oversized partial attachment remained: %s", entry.Name())
			}
		}
	}
}

func TestTailFileReturnsOnlySuffix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	if err := os.WriteFile(path, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := tailFile(path, 4)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "6789" {
		t.Fatalf("tailFile = %q, want 6789", got)
	}
}

func TestTailFileMissingIsEmpty(t *testing.T) {
	got, err := tailFile(filepath.Join(t.TempDir(), "missing.jsonl"), 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("tailFile missing = %q, want empty", got)
	}
}

type fileRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn fileRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func fileJSONResponse(t *testing.T, payload map[string]any) *http.Response {
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
