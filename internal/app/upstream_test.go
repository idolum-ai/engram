package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/anthropic"
	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
	"github.com/idolum-ai/engram/internal/tmux"
	"github.com/idolum-ai/engram/internal/upstream"
)

const firstSignalID = "0123456789abcdef0123456789abcdef"
const secondSignalID = "fedcba9876543210fedcba9876543210"

func TestObserveUpstreamSignalUsesJoinedFrameAndRecordIdentity(t *testing.T) {
	capture := tmux.StyledCapture{
		Text:       "[engram:upstream] 012345\nwrapped physical row",
		JoinedText: "before\n[engram:upstream] " + firstSignalID + " tests finished with two failures\nafter",
	}
	got := observeUpstreamSignal(capture)
	if !got.Found || got.Latest != (upstream.Record{ID: firstSignalID, Payload: "tests finished with two failures"}) || got.PresentationText != "before\nafter" {
		t.Fatalf("observation = %#v", got)
	}
}

func TestRefreshHashesSignalStrippedPresentation(t *testing.T) {
	a, runner, id := newSafetyApp(t, state.TerminalOriginCreated)
	runner.capturePhysical = "ordinary output\n[engram:upstream] " + firstSignalID + " build finished\n"
	runner.captureJoined = runner.capturePhysical
	model := anthropic.New("secret", "claude-haiku-4-5-20251001")
	model.BaseURL = "https://anthropic.test/messages"
	model.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"stop_reason":"end_turn","content":[{"type":"text","text":"Ordinary output is visible."}]}`))}, nil
	})}
	a.Anthropic = model

	a.refreshSession(context.Background(), id, true)
	got, _ := a.Store.FindSession(id)
	if got.LastRawCapture != "ordinary output" || got.LastRawCaptureHash != sha("ordinary output") {
		t.Fatalf("capture=%q hash=%q want=%q", got.LastRawCapture, got.LastRawCaptureHash, sha("ordinary output"))
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
			"ok": true, "result": map[string]any{"message_id": nextMessageID, "chat": map[string]any{"id": 100}},
		})
		nextMessageID++
		return response, nil
	})}
	a := &App{Config: config.Config{TelegramBotToken: "bot-secret", AnthropicAPIKey: "anthropic-secret"}, Store: store, Telegram: client}
	firstRecord := upstream.Record{ID: firstSignalID, Payload: "tests finished bot-secret anthropic-secret"}
	secondRecord := upstream.Record{ID: secondSignalID, Payload: "second result"}

	a.deliverUpstreamSignal(context.Background(), session, firstRecord)
	a.deliverUpstreamSignal(context.Background(), session, firstRecord)
	a.deliverUpstreamSignal(context.Background(), session, secondRecord)
	if len(texts) != 1 || strings.Contains(texts[0], "bot-secret") || strings.Contains(texts[0], "anthropic-secret") || !strings.Contains(texts[0], "<redacted>") {
		t.Fatalf("first delivery texts = %#v", texts)
	}
	first, _ := store.FindSession(session.ID)
	if first.UpstreamMessageID != 88 || !reflect.DeepEqual(first.SeenUpstreamSignalIDs, []string{firstSignalID}) || first.LastUpstreamSignalAt.IsZero() {
		t.Fatalf("first signal state = %#v", first)
	}

	if _, _, err := store.UpdateSession(session.ID, func(s *state.TerminalSession) {
		s.LastUpstreamSignalAt = time.Now().UTC().Add(-upstreamSignalInterval)
	}); err != nil {
		t.Fatal(err)
	}
	a.deliverUpstreamSignal(context.Background(), session, secondRecord)
	if len(texts) != 2 || texts[1] != "[1] terminal-authored signal\n\nsecond result" {
		t.Fatalf("deliveries = %#v", texts)
	}
	second, _ := store.FindSession(session.ID)
	if second.UpstreamMessageID != 89 || !reflect.DeepEqual(second.SeenUpstreamSignalIDs, []string{firstSignalID, secondSignalID}) || !reflect.DeepEqual(second.StaleAlternateMessageIDs, []int{88}) {
		t.Fatalf("replacement signal state = %#v", second)
	}
}

func TestDistinctRecordsWithIdenticalPayloadBothDeliver(t *testing.T) {
	store, session := newUpstreamStore(t)
	var calls int
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": map[string]any{"message_id": 87 + calls, "chat": map[string]any{"id": 100}}}), nil
	})}
	a := &App{Store: store, Telegram: client}
	a.deliverUpstreamSignal(context.Background(), session, upstream.Record{ID: firstSignalID, Payload: "build failed"})
	if _, _, err := store.UpdateSession(session.ID, func(s *state.TerminalSession) {
		s.LastUpstreamSignalAt = time.Now().UTC().Add(-upstreamSignalInterval)
	}); err != nil {
		t.Fatal(err)
	}
	a.deliverUpstreamSignal(context.Background(), session, upstream.Record{ID: secondSignalID, Payload: "build failed"})
	if _, _, err := store.UpdateSession(session.ID, func(s *state.TerminalSession) {
		s.LastUpstreamSignalAt = time.Now().UTC().Add(-upstreamSignalInterval)
	}); err != nil {
		t.Fatal(err)
	}
	a.deliverUpstreamSignal(context.Background(), session, upstream.Record{ID: firstSignalID, Payload: "build failed"})
	if calls != 2 {
		t.Fatalf("distinct/reappearing record calls = %d, want 2", calls)
	}
}

func TestDeletedReplyTargetFallsBackToStandaloneRoutableSignal(t *testing.T) {
	store, session := newUpstreamStore(t)
	var bodies []map[string]any
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, body)
		if len(bodies) == 1 {
			return telegramTestResponse(t, http.StatusBadRequest, map[string]any{"ok": false, "error_code": 400, "description": "Bad Request: message to be replied not found"}), nil
		}
		return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": map[string]any{"message_id": 88, "chat": map[string]any{"id": 100}}}), nil
	})}
	recovered := make(chan struct{}, 1)
	a := &App{Store: store, Telegram: client, runCtx: context.Background(), refreshHook: func(_ context.Context, _ int, force bool) {
		if force {
			recovered <- struct{}{}
		}
	}}
	a.deliverUpstreamSignal(context.Background(), session, upstream.Record{ID: firstSignalID, Payload: "needs attention"})
	a.refreshWG.Wait()
	got, targetState, ok := store.FindReplyTarget(100, 88)
	if len(bodies) != 2 || bodies[0]["reply_to_message_id"] != float64(77) || bodies[1]["reply_to_message_id"] != nil || !ok || targetState != state.ReplyTargetCurrent || got.ID != session.ID || len(recovered) != 1 {
		t.Fatalf("fallback bodies=%#v route=%#v %q ok=%v", bodies, got, targetState, ok)
	}
}

func TestRateLimitRetryDeadlinePersistsAndSuppressesSchedulerRetry(t *testing.T) {
	store, session := newUpstreamStore(t)
	var calls int
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return telegramTestResponse(t, http.StatusTooManyRequests, map[string]any{"ok": false, "error_code": 429, "description": "Too Many Requests", "parameters": map[string]any{"retry_after": 31}}), nil
	})}
	a := &App{Store: store, Telegram: client}
	record := upstream.Record{ID: firstSignalID, Payload: "build finished"}
	a.deliverUpstreamSignal(context.Background(), session, record)
	a.deliverUpstreamSignal(context.Background(), session, record)
	got, _ := store.FindSession(session.ID)
	if calls != 1 || time.Until(got.UpstreamRetryAt) < 29*time.Second {
		t.Fatalf("calls=%d retry_at=%s", calls, got.UpstreamRetryAt)
	}
}

func TestRateLimitRetryDeadlineSurvivesPreReplacementStateFailureInMemory(t *testing.T) {
	store, session, dir := newUpstreamStoreWithDir(t)
	movedDir := dir + ".unavailable"
	if err := os.Rename(dir, movedDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Rename(movedDir, dir) })
	var calls int
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return telegramTestResponse(t, http.StatusTooManyRequests, map[string]any{"ok": false, "error_code": 429, "description": "Too Many Requests", "parameters": map[string]any{"retry_after": 31}}), nil
	})}
	a := &App{Store: store, Telegram: client}
	record := upstream.Record{ID: firstSignalID, Payload: "build finished"}
	a.deliverUpstreamSignal(context.Background(), session, record)
	a.deliverUpstreamSignal(context.Background(), session, record)
	got, _ := store.FindSession(session.ID)
	if calls != 1 || !got.UpstreamRetryAt.IsZero() || time.Until(a.upstreamRetryDeadline(session.ID, got.UpstreamRetryAt)) < 29*time.Second {
		t.Fatalf("calls=%d persisted=%s transient=%s", calls, got.UpstreamRetryAt, a.upstreamRetryDeadline(session.ID, got.UpstreamRetryAt))
	}
}

func TestDeliveryTimestampIsRecordedAfterTelegramCompletes(t *testing.T) {
	store, session := newUpstreamStore(t)
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(*http.Request) (*http.Response, error) {
		time.Sleep(30 * time.Millisecond)
		return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": map[string]any{"message_id": 88, "chat": map[string]any{"id": 100}}}), nil
	})}
	a := &App{Store: store, Telegram: client}
	started := time.Now().UTC()
	a.deliverUpstreamSignal(context.Background(), session, upstream.Record{ID: firstSignalID, Payload: "done"})
	got, _ := store.FindSession(session.ID)
	if got.LastUpstreamSignalAt.Before(started.Add(25 * time.Millisecond)) {
		t.Fatalf("delivery time %s predates Telegram completion after %s", got.LastUpstreamSignalAt, started)
	}
}

func TestPersistenceFailureDeletesProspectiveSignalAndRollsBackAlias(t *testing.T) {
	store, session, dir := newUpstreamStoreWithDir(t)
	movedDir := dir + ".unavailable"
	if err := os.Rename(dir, movedDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Rename(movedDir, dir) })
	var paths []string
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.Path)
		if strings.HasSuffix(req.URL.Path, "/deleteMessage") {
			return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": true}), nil
		}
		return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": map[string]any{"message_id": 88, "chat": map[string]any{"id": 100}}}), nil
	})}
	a := &App{Store: store, Telegram: client}
	a.deliverUpstreamSignal(context.Background(), session, upstream.Record{ID: firstSignalID, Payload: "done"})
	got, _ := store.FindSession(session.ID)
	if !reflect.DeepEqual(paths, []string{"/botTOKEN/sendMessage", "/botTOKEN/deleteMessage"}) || got.UpstreamMessageID != 0 || len(got.SeenUpstreamSignalIDs) != 0 {
		t.Fatalf("paths=%#v session=%#v", paths, got)
	}
}

func TestConcurrentUpstreamDeliveryPublishesOneCurrentAlias(t *testing.T) {
	store, session := newUpstreamStore(t)
	var calls atomic.Int32
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": map[string]any{"message_id": 88, "chat": map[string]any{"id": 100}}}), nil
	})}
	a := &App{Store: store, Telegram: client}
	record := upstream.Record{ID: firstSignalID, Payload: "build finished"}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.deliverUpstreamSignal(context.Background(), session, record)
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
	store, session, _ := newUpstreamStoreWithDir(t)
	return store, session
}

func newUpstreamStoreWithDir(t *testing.T) (*state.Store, state.TerminalSession, string) {
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
		s.TmuxServerID = appTestServerID
		s.AnchorChatID = 100
		s.AnchorMessageID = 77
		s.AnchorFormat = "text"
		s.WatchEnabled = true
	}); err != nil {
		t.Fatal(err)
	}
	session, _ = store.FindSession(session.ID)
	return store, session, dir
}
