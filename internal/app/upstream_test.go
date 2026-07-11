package app

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
	"github.com/idolum-ai/engram/internal/tmux"
)

type upstreamCaptureRunner struct {
	calls [][]string
	text  string
	err   error
}

func (r *upstreamCaptureRunner) Run(_ context.Context, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	return r.text, r.err
}

func TestCaptureUpstreamSignalReconstructsWrappedLogicalLineAndClearsPresence(t *testing.T) {
	store, session := newUpstreamStore(t)
	if _, _, err := store.UpdateSession(session.ID, func(s *state.TerminalSession) {
		s.LastUpstreamSignalHash = "delivered"
	}); err != nil {
		t.Fatal(err)
	}
	runner := &upstreamCaptureRunner{text: "before\n[engram:upstream] tests finished with two failures\nafter\n"}
	a := &App{
		Store:        store,
		Tmux:         tmux.New(runner),
		captureSlots: make(chan struct{}, 1),
	}
	capture := tmux.StyledCapture{
		Text:        "[engram:upstream] tests finished with\ntwo failures",
		VisibleRows: 24,
	}
	payload, ok, presentation, safe := a.captureUpstreamSignal(context.Background(), session, capture)
	if !ok || !safe || payload != "tests finished with two failures" || presentation != "before\nafter" {
		t.Fatalf("signal = %q, %v presentation=%q safe=%v", payload, ok, presentation, safe)
	}
	wantCall := []string{"capture-pane", "-p", "-J", "-S", "-40", "-E", "23", "-t", "%1"}
	if len(runner.calls) != 1 || !reflect.DeepEqual(runner.calls[0], wantCall) {
		t.Fatalf("joined capture calls = %#v", runner.calls)
	}

	if payload, ok, presentation, safe := a.captureUpstreamSignal(context.Background(), session, tmux.StyledCapture{Text: "ordinary output"}); ok || payload != "" || !safe || presentation != "ordinary output" {
		t.Fatalf("ordinary frame signal = %q, %v presentation=%q safe=%v", payload, ok, presentation, safe)
	}
	got, _ := store.FindSession(session.ID)
	if got.LastUpstreamSignalHash != "" {
		t.Fatalf("signal presence hash was not cleared: %#v", got)
	}
}

func TestDeliverUpstreamSignalRedactsCoalescesAndReplacesReplyAlias(t *testing.T) {
	store, session := newUpstreamStore(t)
	var texts []string
	nextMessageID := 88
	client := telegram.New("bot-secret")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		texts = append(texts, body["text"].(string))
		response := telegramTestResponse(t, http.StatusOK, map[string]any{
			"ok":     true,
			"result": map[string]any{"message_id": nextMessageID, "chat": map[string]any{"id": 100}},
		})
		nextMessageID++
		return response, nil
	})}
	a := &App{
		Config: config.Config{
			TelegramBotToken: "bot-secret",
			AnthropicAPIKey:  "anthropic-secret",
		},
		Store:    store,
		Telegram: client,
	}

	a.deliverUpstreamSignal(context.Background(), session, "tests finished bot-secret anthropic-secret")
	a.deliverUpstreamSignal(context.Background(), session, "tests finished bot-secret anthropic-secret")
	a.deliverUpstreamSignal(context.Background(), session, "second result")
	if len(texts) != 1 || strings.Contains(texts[0], "bot-secret") || strings.Contains(texts[0], "anthropic-secret") || !strings.Contains(texts[0], "<redacted>") {
		t.Fatalf("first delivery texts = %#v", texts)
	}
	first, _ := store.FindSession(session.ID)
	if first.UpstreamMessageID != 88 || first.LastUpstreamSignalHash == "" || first.LastUpstreamSignalAt.IsZero() {
		t.Fatalf("first signal state = %#v", first)
	}

	if _, _, err := store.UpdateSession(session.ID, func(s *state.TerminalSession) {
		s.LastUpstreamSignalAt = time.Now().UTC().Add(-upstreamSignalInterval)
	}); err != nil {
		t.Fatal(err)
	}
	a.deliverUpstreamSignal(context.Background(), session, "second result")
	if len(texts) != 2 || texts[1] != "[1] upstream\n\nsecond result" {
		t.Fatalf("deliveries = %#v", texts)
	}
	second, _ := store.FindSession(session.ID)
	if second.UpstreamMessageID != 89 || !reflect.DeepEqual(second.StaleAlternateMessageIDs, []int{88}) {
		t.Fatalf("replacement signal state = %#v", second)
	}
}

func TestConcurrentUpstreamDeliveryPublishesOneCurrentAlias(t *testing.T) {
	store, session := newUpstreamStore(t)
	var calls atomic.Int32
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return telegramTestResponse(t, http.StatusOK, map[string]any{
			"ok":     true,
			"result": map[string]any{"message_id": 88, "chat": map[string]any{"id": 100}},
		}), nil
	})}
	a := &App{Store: store, Telegram: client}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.deliverUpstreamSignal(context.Background(), session, "build finished")
		}()
	}
	wg.Wait()
	got, _ := store.FindSession(session.ID)
	if calls.Load() != 1 || got.UpstreamMessageID != 88 || len(got.StaleAlternateMessageIDs) != 0 {
		t.Fatalf("calls=%d session=%#v", calls.Load(), got)
	}
}

func newUpstreamStore(t *testing.T) (*state.Store, state.TerminalSession) {
	t.Helper()
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.AllocateSession("main", "@1", "%1", "build")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.UpdateSession(session.ID, func(s *state.TerminalSession) {
		s.AnchorChatID = 100
		s.AnchorMessageID = 77
		s.AnchorFormat = "text"
		s.WatchEnabled = true
	}); err != nil {
		t.Fatal(err)
	}
	session, _ = store.FindSession(session.ID)
	return store, session
}
