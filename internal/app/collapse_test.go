package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/idolum-ai/engram/internal/guide"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
)

func TestCollapsedSessionsShareOneShelfAndExpandTogether(t *testing.T) {
	app, _, firstID := newSafetyApp(t, state.TerminalOriginCreated)
	second, err := app.Store.AllocateSession("main", "@2", "%2", "riemann")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := app.Store.UpdateSession(firstID, func(session *state.TerminalSession) {
		session.AnchorChatID = 100
		session.AnchorMessageID = 77
		session.AnchorFormat = anchorFormatText
		session.AnchorPinned = true
		session.AnchorPinKnown = true
		session.LastSummary = "PR checks passed."
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := app.Store.UpdateSession(second.ID, func(session *state.TerminalSession) {
		session.TmuxServerID = appTestServerID
		session.AnchorChatID = 100
		session.AnchorMessageID = 78
		session.AnchorFormat = anchorFormatText
		session.AnchorPinned = true
		session.AnchorPinKnown = true
		session.WatchEnabled = true
		session.LastSummary = "The proof is waiting on one decision."
	}); err != nil {
		t.Fatal(err)
	}

	nextMessageID := 88
	var requests []collapseTelegramRequest
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		requests = append(requests, collapseTelegramRequest{path: req.URL.Path, body: body})
		switch req.URL.Path {
		case "/botTOKEN/sendMessage":
			messageID := nextMessageID
			nextMessageID++
			return telegramTestResponse(t, http.StatusOK, map[string]any{
				"ok": true, "result": map[string]any{"message_id": messageID, "chat": map[string]any{"id": 100}},
			}), nil
		case "/botTOKEN/editMessageText", "/botTOKEN/editMessageReplyMarkup":
			return telegramTestResponse(t, http.StatusOK, map[string]any{
				"ok": true, "result": map[string]any{"message_id": body["message_id"], "chat": map[string]any{"id": 100}},
			}), nil
		case "/botTOKEN/answerCallbackQuery", "/botTOKEN/pinChatMessage", "/botTOKEN/unpinChatMessage", "/botTOKEN/deleteMessage":
			return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": true}), nil
		default:
			return nil, errors.New("unexpected Telegram endpoint " + req.URL.Path)
		}
	})}
	app.Telegram = client
	app.Config.TelegramAllowedUserID = 42
	app.Config.TelegramChatID = 100
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	app.runCtx = ctx

	for _, target := range []struct {
		id        int
		messageID int
	}{
		{id: firstID, messageID: 77},
		{id: second.ID, messageID: 78},
	} {
		status := app.handleCallback(context.Background(), telegram.CallbackQuery{
			ID: "collapse-" + strconv.Itoa(target.id), From: telegram.User{ID: 42},
			Data:    "collapse:" + strconv.Itoa(target.id),
			Message: &telegram.Message{MessageID: target.messageID, Chat: telegram.Chat{ID: 100}},
		})
		if status != "callback_ok" {
			t.Fatalf("collapse [%d] status = %q", target.id, status)
		}
	}

	snapshot := app.Store.Snapshot()
	if snapshot.CollapsedShelf == nil || snapshot.CollapsedShelf.MessageID != 88 || !snapshot.CollapsedShelf.Pinned {
		t.Fatalf("collapsed shelf = %#v", snapshot.CollapsedShelf)
	}
	for _, id := range []int{firstID, second.ID} {
		session, ok := app.Store.FindSession(id)
		if !ok || !session.Collapsed || session.AnchorMessageID != 0 {
			t.Fatalf("collapsed session [%d] = %#v, ok=%v", id, session, ok)
		}
	}
	if got := countCollapseRequests(requests, "/botTOKEN/sendMessage"); got != 1 {
		t.Fatalf("shelf sends = %d, want exactly one", got)
	}
	lastShelfText := lastCollapseRequestText(requests, 88)
	if !strings.Contains(lastShelfText, "Collapsed sessions (2)") || !strings.Contains(lastShelfText, "[1]") || !strings.Contains(lastShelfText, "["+strconv.Itoa(second.ID)+"]") {
		t.Fatalf("shelf text = %q", lastShelfText)
	}
	for _, oldID := range []int{77, 78} {
		_, target, found := app.Store.FindReplyTarget(100, oldID)
		if !found || target != state.ReplyTargetStale {
			t.Fatalf("old anchor %d target = %q, found=%v", oldID, target, found)
		}
	}

	requests = nil
	status := app.handleCallback(context.Background(), telegram.CallbackQuery{
		ID: "expand-all", From: telegram.User{ID: 42}, Data: "expand-all:0",
		Message: &telegram.Message{MessageID: 88, Chat: telegram.Chat{ID: 100}},
	})
	if status != "callback_ok" {
		t.Fatalf("expand-all status = %q", status)
	}
	snapshot = app.Store.Snapshot()
	if snapshot.CollapsedShelf != nil {
		t.Fatalf("collapsed shelf survived expansion: %#v", snapshot.CollapsedShelf)
	}
	for index, id := range []int{firstID, second.ID} {
		session, ok := app.Store.FindSession(id)
		wantMessageID := 89 + index
		if !ok || session.Collapsed || session.AnchorMessageID != wantMessageID || !session.AnchorPinned {
			t.Fatalf("expanded session [%d] = %#v, ok=%v want_message=%d", id, session, ok, wantMessageID)
		}
		if _, target, found := app.Store.FindReplyTarget(100, wantMessageID); !found || target != state.ReplyTargetCurrent {
			t.Fatalf("expanded anchor %d target = %q, found=%v", wantMessageID, target, found)
		}
	}
	if countCollapseRequests(requests, "/botTOKEN/deleteMessage") != 1 {
		t.Fatalf("expand requests = %#v, want shelf deletion", requests)
	}
}

func TestCollapsedSessionDoesNoPresentationWork(t *testing.T) {
	app, runner, id := newSafetyApp(t, state.TerminalOriginCreated)
	if _, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.Collapsed = true
		session.LastSummary = "cached"
	}); err != nil {
		t.Fatal(err)
	}
	guideRenderer := &recordingShelfGuide{}
	app.Guide = guideRenderer
	app.Snapshots = &failingSnapshotRenderer{}

	app.refreshSession(context.Background(), id, true)

	if guideRenderer.calls != 0 || len(runner.calls) != 0 {
		t.Fatalf("collapsed refresh performed work: guide=%d tmux=%#v", guideRenderer.calls, runner.calls)
	}
}

func TestReplyToCollapsedShelfExplainsAmbiguousRoute(t *testing.T) {
	app, _, _ := newSafetyApp(t, state.TerminalOriginCreated)
	if _, committed, err := app.Store.SetCollapsedShelfIfEmpty(state.CollapsedShelf{ChatID: 100, MessageID: 88}); err != nil || !committed {
		t.Fatalf("prepare shelf committed=%v err=%v", committed, err)
	}
	var reply string
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		reply, _ = body["text"].(string)
		return telegramTestResponse(t, http.StatusOK, map[string]any{
			"ok": true, "result": map[string]any{"message_id": 90, "chat": map[string]any{"id": 100}},
		}), nil
	})}
	app.Telegram = client
	app.Config.TelegramAllowedUserID = 42
	app.Config.TelegramChatID = 100

	status := app.handleUpdate(context.Background(), telegram.Update{Message: &telegram.Message{
		MessageID: 89, Chat: telegram.Chat{ID: 100}, From: &telegram.User{ID: 42}, Text: "hello",
		ReplyToMessage: &telegram.Message{MessageID: 88, Chat: telegram.Chat{ID: 100}},
	}})
	if status != "anchor_reply_user_error" || !strings.Contains(reply, "represents multiple sessions") {
		t.Fatalf("status=%q reply=%q", status, reply)
	}
}

func TestCollapsedShelfRestartReconcilesPinAndPendingAnchorRetirement(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	session, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.AnchorChatID = 100
		session.AnchorMessageID = 77
		session.AnchorFormat = anchorFormatText
		session.AnchorPinned = true
		session.AnchorPinKnown = true
		session.LastSummary = "Cached status."
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, committed, err := app.Store.CollapseSessionIntoShelf(id, session, state.CollapsedShelf{ChatID: 100, MessageID: 88}, "old"); err != nil || !committed {
		t.Fatalf("prepare collapse committed=%v err=%v", committed, err)
	}

	var paths []string
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.Path)
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		switch req.URL.Path {
		case "/botTOKEN/editMessageText":
			return telegramTestResponse(t, http.StatusOK, map[string]any{
				"ok": true, "result": map[string]any{"message_id": body["message_id"], "chat": map[string]any{"id": 100}},
			}), nil
		case "/botTOKEN/pinChatMessage", "/botTOKEN/unpinChatMessage":
			return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": true}), nil
		default:
			return nil, errors.New("unexpected Telegram endpoint " + req.URL.Path)
		}
	})}
	app.Telegram = client
	app.Config.TelegramChatID = 100

	app.reconcileCollapsedShelf(context.Background())

	snapshot := app.Store.Snapshot()
	current, _ := app.Store.FindSession(id)
	if snapshot.CollapsedShelf == nil || !snapshot.CollapsedShelf.Pinned || !snapshot.CollapsedShelf.PinKnown || current.AnchorMessageID != 0 {
		t.Fatalf("reconciled shelf=%#v session=%#v", snapshot.CollapsedShelf, current)
	}
	for _, want := range []string{"/botTOKEN/editMessageText", "/botTOKEN/pinChatMessage", "/botTOKEN/editMessageText", "/botTOKEN/unpinChatMessage"} {
		if !containsCollapsePath(paths, want) {
			t.Fatalf("restart requests = %#v, missing %s", paths, want)
		}
	}
}

func TestCollapsedShelfRetriesControlsAfterActivationFailure(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	session, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.AnchorChatID = 100
		session.AnchorMessageID = 77
		session.AnchorFormat = anchorFormatText
		session.AnchorPinned = true
		session.AnchorPinKnown = true
		session.LastSummary = "Cached status."
	})
	if err != nil {
		t.Fatal(err)
	}
	shelfEdits := 0
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		switch req.URL.Path {
		case "/botTOKEN/sendMessage":
			return telegramTestResponse(t, http.StatusOK, map[string]any{
				"ok": true, "result": map[string]any{"message_id": 88, "chat": map[string]any{"id": 100}},
			}), nil
		case "/botTOKEN/editMessageText":
			if intFromJSONNumber(body["message_id"]) == 88 {
				shelfEdits++
				if shelfEdits == 1 {
					return nil, errors.New("temporary activation failure")
				}
			}
			return telegramTestResponse(t, http.StatusOK, map[string]any{
				"ok": true, "result": map[string]any{"message_id": body["message_id"], "chat": map[string]any{"id": 100}},
			}), nil
		case "/botTOKEN/pinChatMessage", "/botTOKEN/unpinChatMessage":
			return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": true}), nil
		default:
			return nil, errors.New("unexpected Telegram endpoint " + req.URL.Path)
		}
	})}
	app.Telegram = client
	app.Config.TelegramChatID = 100

	result := app.collapseAnchor(context.Background(), session)
	if !result.OK() {
		t.Fatalf("collapse result = %#v", result)
	}
	if shelf := app.Store.Snapshot().CollapsedShelf; shelf == nil || shelf.LastRenderHash != "" || !shelf.PinKnown {
		t.Fatalf("failed activation was marked complete: %#v", shelf)
	}

	app.reconcileCollapsedShelf(context.Background())

	if shelf := app.Store.Snapshot().CollapsedShelf; shelf == nil || shelf.LastRenderHash == "" || shelfEdits != 2 {
		t.Fatalf("controls were not retried: shelf=%#v edits=%d", shelf, shelfEdits)
	}
}

func TestCollapsedShelfBackoffDefersAllTelegramReconciliation(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	session, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.AnchorChatID = 100
		session.AnchorMessageID = 77
		session.AnchorFormat = anchorFormatText
		session.LastSummary = "Cached status."
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, committed, err := app.Store.CollapseSessionIntoShelf(id, session, state.CollapsedShelf{
		ChatID: 100, MessageID: 88,
	}, "old"); err != nil || !committed {
		t.Fatalf("prepare collapse committed=%v err=%v", committed, err)
	}
	if _, _, err := app.Store.UpdateCollapsedShelf(88, func(shelf *state.CollapsedShelf) {
		shelf.RetryAt = time.Now().UTC().Add(time.Minute)
	}); err != nil {
		t.Fatal(err)
	}
	app.Telegram = telegram.New("TOKEN")
	app.Telegram.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("Telegram should not be called during shelf backoff")
	})}

	app.reconcileCollapsedShelf(context.Background())

	current, _ := app.Store.FindSession(id)
	if current.AnchorMessageID != 77 {
		t.Fatalf("backoff retired the old anchor: %#v", current)
	}
}

func TestPartialExpandKeepsShelfForRemainingSessions(t *testing.T) {
	app, _, firstID := newSafetyApp(t, state.TerminalOriginCreated)
	second, err := app.Store.AllocateSession("main", "@2", "%2", "second")
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []struct {
		id        int
		messageID int
	}{
		{id: firstID, messageID: 77},
		{id: second.ID, messageID: 78},
	} {
		session, _, err := app.Store.UpdateSession(target.id, func(session *state.TerminalSession) {
			session.TmuxServerID = appTestServerID
			session.AnchorChatID = 100
			session.AnchorMessageID = target.messageID
			session.AnchorFormat = anchorFormatText
			session.WatchEnabled = true
			session.LastSummary = "Cached status."
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, committed, err := app.Store.CollapseSessionIntoShelf(target.id, session, state.CollapsedShelf{ChatID: 100, MessageID: 88}, "old"); err != nil || !committed {
			t.Fatalf("prepare collapse [%d] committed=%v err=%v", target.id, committed, err)
		}
		if _, retired, err := app.Store.FinishCollapsedAnchorRetirement(target.id, 100, target.messageID); err != nil || !retired {
			t.Fatalf("prepare retirement [%d] retired=%v err=%v", target.id, retired, err)
		}
	}

	sendCalls := 0
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		switch req.URL.Path {
		case "/botTOKEN/sendMessage":
			sendCalls++
			if sendCalls == 2 {
				return nil, errors.New("temporary network failure")
			}
			return telegramTestResponse(t, http.StatusOK, map[string]any{
				"ok": true, "result": map[string]any{"message_id": 89, "chat": map[string]any{"id": 100}},
			}), nil
		case "/botTOKEN/editMessageReplyMarkup", "/botTOKEN/editMessageText":
			return telegramTestResponse(t, http.StatusOK, map[string]any{
				"ok": true, "result": map[string]any{"message_id": body["message_id"], "chat": map[string]any{"id": 100}},
			}), nil
		case "/botTOKEN/pinChatMessage":
			return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": true}), nil
		default:
			return nil, errors.New("unexpected Telegram endpoint " + req.URL.Path)
		}
	})}
	app.Telegram = client
	app.Config.TelegramChatID = 100
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	app.runCtx = ctx

	result := app.expandCollapsedShelf(context.Background(), state.CollapsedShelf{ChatID: 100, MessageID: 88})
	if result.OK() || !strings.Contains(result.Message, "restored 1 of 2") {
		t.Fatalf("partial result = %#v", result)
	}
	first, _ := app.Store.FindSession(firstID)
	remaining, _ := app.Store.FindSession(second.ID)
	shelf := app.Store.Snapshot().CollapsedShelf
	if first.Collapsed || first.AnchorMessageID != 89 || !remaining.Collapsed || remaining.AnchorMessageID != 0 || shelf == nil || shelf.MessageID != 88 {
		t.Fatalf("partial expansion first=%#v remaining=%#v shelf=%#v", first, remaining, shelf)
	}
}

func TestCollapsedShelfIsBoundedSortedAndUTF8Safe(t *testing.T) {
	app := &App{}
	sessions := make([]state.TerminalSession, 0, 80)
	for id := 80; id >= 1; id-- {
		sessions = append(sessions, state.TerminalSession{
			ID: id, Title: strings.Repeat("界", 80), State: state.TerminalRunning,
			Collapsed: true, LastSummary: strings.Repeat("終了した処理 ", 80),
		})
	}
	rendered := app.renderCollapsedShelf(sessions)
	if !utf8.ValidString(rendered) || len(rendered) > maxCollapsedShelfBytes {
		t.Fatalf("shelf bytes=%d valid=%v", len(rendered), utf8.ValidString(rendered))
	}
	if first, second := strings.Index(rendered, "[1]"), strings.Index(rendered, "[2]"); first < 0 || second < 0 || first >= second {
		t.Fatalf("shelf is not sorted by session id: %q", rendered)
	}
	if !strings.Contains(rendered, "more") {
		t.Fatalf("bounded shelf omitted overflow count: %q", rendered)
	}
}

func containsCollapsePath(paths []string, want string) bool {
	for _, path := range paths {
		if path == want {
			return true
		}
	}
	return false
}

type collapseTelegramRequest struct {
	path string
	body map[string]any
}

func countCollapseRequests(requests []collapseTelegramRequest, path string) int {
	count := 0
	for _, request := range requests {
		if request.path == path {
			count++
		}
	}
	return count
}

func lastCollapseRequestText(requests []collapseTelegramRequest, messageID int) string {
	var text string
	for _, request := range requests {
		if request.path != "/botTOKEN/editMessageText" || intFromJSONNumber(request.body["message_id"]) != messageID {
			continue
		}
		if candidate, ok := request.body["text"].(string); ok {
			text = candidate
		}
	}
	return text
}

func intFromJSONNumber(value any) int {
	switch value := value.(type) {
	case float64:
		return int(value)
	case int:
		return value
	default:
		return 0
	}
}

type recordingShelfGuide struct {
	calls int
}

func (g *recordingShelfGuide) Converse(context.Context, guide.Input) (string, error) {
	g.calls++
	return "unexpected", nil
}
