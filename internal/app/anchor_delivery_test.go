package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
)

func TestAnchorEditClassifiesTelegramFailures(t *testing.T) {
	tests := []struct {
		name            string
		httpStatus      int
		editDescription string
		editCode        int
		wantPaths       []string
		wantAnchorID    int
		wantHash        bool
	}{
		{
			name:            "not modified is success",
			httpStatus:      http.StatusBadRequest,
			editDescription: "Bad Request: message is not modified",
			editCode:        400,
			wantPaths:       []string{"/botTOKEN/editMessageText"},
			wantAnchorID:    77,
			wantHash:        true,
		},
		{
			name:            "rate limit does not create replacement",
			httpStatus:      http.StatusTooManyRequests,
			editDescription: "Too Many Requests: retry later",
			editCode:        429,
			wantPaths:       []string{"/botTOKEN/editMessageText"},
			wantAnchorID:    77,
			wantHash:        false,
		},
		{
			name:            "format failure retries plain edit",
			httpStatus:      http.StatusBadRequest,
			editDescription: "Bad Request: can't parse entities",
			editCode:        400,
			wantPaths:       []string{"/botTOKEN/editMessageText", "/botTOKEN/editMessageText"},
			wantAnchorID:    77,
			wantHash:        true,
		},
		{
			name:            "deleted message creates replacement",
			httpStatus:      http.StatusBadRequest,
			editDescription: "Bad Request: message to edit not found",
			editCode:        400,
			wantPaths:       []string{"/botTOKEN/editMessageText", "/botTOKEN/sendMessage"},
			wantAnchorID:    88,
			wantHash:        true,
		},
		{
			name:            "server failure does not create replacement",
			httpStatus:      http.StatusInternalServerError,
			editDescription: "Internal Server Error",
			editCode:        500,
			wantPaths:       []string{"/botTOKEN/editMessageText"},
			wantAnchorID:    77,
			wantHash:        false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app, paths, id := newAnchorDeliveryApp(t, test.httpStatus, test.editCode, test.editDescription)
			app.updateAnchorLocal(context.Background(), id, "status:\nready", true)
			if !reflect.DeepEqual(*paths, test.wantPaths) {
				t.Fatalf("request paths = %#v, want %#v", *paths, test.wantPaths)
			}
			ts, ok := app.Store.FindSession(id)
			if !ok || ts.AnchorMessageID != test.wantAnchorID {
				t.Fatalf("session after edit = %#v ok=%v", ts, ok)
			}
			if (ts.LastRenderHash != "") != test.wantHash {
				t.Fatalf("LastRenderHash set = %v, want %v", ts.LastRenderHash != "", test.wantHash)
			}
		})
	}
}

func newAnchorDeliveryApp(t *testing.T, httpStatus, editCode int, editDescription string) (*App, *[]string, int) {
	t.Helper()
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	ts, err := store.AllocateSession(100, 42, "main", "@1", "%1", "shell")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.UpdateSession(ts.ID, func(session *state.TerminalSession) {
		session.AnchorChatID = 100
		session.AnchorMessageID = 77
	}); err != nil {
		t.Fatal(err)
	}
	paths := []string{}
	editCalls := 0
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: anchorDeliveryRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.Path)
		if req.URL.Path == "/botTOKEN/editMessageText" {
			editCalls++
			if editCalls == 1 {
				return telegramTestResponse(t, httpStatus, map[string]any{
					"ok":          false,
					"error_code":  editCode,
					"description": editDescription,
				}), nil
			}
			return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": map[string]any{"message_id": 77, "chat": map[string]any{"id": 100}}}), nil
		}
		if req.URL.Path == "/botTOKEN/sendMessage" {
			return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": map[string]any{"message_id": 88, "chat": map[string]any{"id": 100}}}), nil
		}
		t.Fatalf("unexpected Telegram path %s", req.URL.Path)
		return nil, nil
	})}
	return &App{
		Config:   config.Config{TelegramChatID: 100},
		Store:    store,
		Telegram: client,
	}, &paths, ts.ID
}

func telegramTestResponse(t *testing.T, status int, payload map[string]any) *http.Response {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Body:       io.NopCloser(strings.NewReader(string(data))),
		Header:     make(http.Header),
	}
}

type anchorDeliveryRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn anchorDeliveryRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}
