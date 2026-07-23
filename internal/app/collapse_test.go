package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/guide"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
	"github.com/idolum-ai/engram/internal/tmux"
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
	defer cancel()
	app.runCtx = ctx
	app.refreshHook = func(context.Context, int, bool) {}

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
		app.transferWG.Wait()
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
	app.transferWG.Wait()
	app.refreshWG.Wait()
	snapshot = app.Store.Snapshot()
	if snapshot.CollapsedShelf != nil {
		t.Fatalf("collapsed shelf survived expansion: %#v", snapshot.CollapsedShelf)
	}
	for _, want := range []struct {
		id        int
		messageID int
	}{
		{id: firstID, messageID: 89},
		{id: second.ID, messageID: 90},
	} {
		session, ok := app.Store.FindSession(want.id)
		wantMessageID := want.messageID
		if !ok || session.Collapsed || session.AnchorMessageID != wantMessageID || !session.AnchorPinned {
			t.Fatalf("expanded session [%d] = %#v, ok=%v want_message=%d", want.id, session, ok, wantMessageID)
		}
		if _, target, found := app.Store.FindReplyTarget(100, wantMessageID); !found || target != state.ReplyTargetCurrent {
			t.Fatalf("expanded anchor %d target = %q, found=%v", wantMessageID, target, found)
		}
	}
	if countCollapseRequests(requests, "/botTOKEN/deleteMessage") != 1 {
		t.Fatalf("expand requests = %#v, want shelf deletion", requests)
	}
	var pins []int
	restoreMessages := 0
	for _, request := range requests {
		switch request.path {
		case "/botTOKEN/pinChatMessage":
			pins = append(pins, intFromJSONNumber(request.body["message_id"]))
		case "/botTOKEN/sendMessage":
			if text, _ := request.body["text"].(string); strings.Contains(text, "Restored from cached state. Refreshing now.") {
				restoreMessages++
			}
		}
	}
	if len(pins) != 2 || pins[0] != 89 || pins[1] != 90 {
		t.Fatalf("restore pin order = %#v, want lower-priority then shelf-first", pins)
	}
	if restoreMessages != 2 {
		t.Fatalf("cached restore notices = %d, want 2", restoreMessages)
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
	if status != "anchor_reply_user_error" || !strings.Contains(reply, "multiple terminals") || !strings.Contains(reply, "➕ Show") {
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
	if _, committed, err := app.Store.CollapseSessionIntoShelf(id, session, state.CollapsedShelf{ChatID: 100, MessageID: 88}); err != nil || !committed {
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
	app.refreshHook = func(context.Context, int, bool) {}

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

func TestCollapsedShelfActivationFailureLeavesCurrentAnchorLive(t *testing.T) {
	app, runner, id := newSafetyApp(t, state.TerminalOriginCreated)
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
	app.refreshHook = func(context.Context, int, bool) {}

	result := app.collapseAnchor(context.Background(), session)
	current, _ := app.Store.FindSession(id)
	if !result.OK() || current.Collapsed || !current.PendingCollapse || current.AnchorMessageID != 77 || app.Store.Snapshot().CollapsedShelf == nil || shelfEdits != 1 {
		t.Fatalf("collapse=%#v session=%#v shelf=%#v edits=%d", result, current, app.Store.Snapshot().CollapsedShelf, shelfEdits)
	}
	if input := app.sendInput(context.Background(), id, "still routed", "command", true); !input.OK() || len(runner.calls) == 0 {
		t.Fatalf("pending collapse lost its live route: input=%#v calls=%#v", input, runner.calls)
	}
	app.refreshWG.Wait()
}

func TestCollapsedShelfPinFailureLeavesCurrentAnchorLive(t *testing.T) {
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
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/botTOKEN/sendMessage":
			return telegramTestResponse(t, http.StatusOK, map[string]any{
				"ok": true, "result": map[string]any{"message_id": 88, "chat": map[string]any{"id": 100}},
			}), nil
		case "/botTOKEN/editMessageText", "/botTOKEN/editMessageReplyMarkup", "/botTOKEN/unpinChatMessage", "/botTOKEN/deleteMessage":
			return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": true}), nil
		case "/botTOKEN/pinChatMessage":
			return nil, errors.New("temporary pin failure")
		default:
			return nil, errors.New("unexpected Telegram endpoint " + req.URL.Path)
		}
	})}
	app.Telegram = client
	app.Config.TelegramChatID = 100

	result := app.collapseAnchor(context.Background(), session)
	current, _ := app.Store.FindSession(id)
	if !result.OK() || current.Collapsed || !current.PendingCollapse || current.AnchorMessageID != 77 || app.Store.Snapshot().CollapsedShelf == nil {
		t.Fatalf("collapse=%#v session=%#v shelf=%#v", result, current, app.Store.Snapshot().CollapsedShelf)
	}
}

func TestExpandPinFailureKeepsMemberOnShelf(t *testing.T) {
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
		ChatID: 100, MessageID: 88, Pinned: true, PinKnown: true,
	}); err != nil || !committed {
		t.Fatalf("prepare collapse committed=%v err=%v", committed, err)
	}
	if _, retired, err := app.Store.FinishCollapsedAnchorRetirement(id, 100, 77); err != nil || !retired {
		t.Fatalf("prepare retirement retired=%v err=%v", retired, err)
	}
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/botTOKEN/sendMessage":
			return telegramTestResponse(t, http.StatusOK, map[string]any{
				"ok": true, "result": map[string]any{"message_id": 89, "chat": map[string]any{"id": 100}},
			}), nil
		case "/botTOKEN/editMessageReplyMarkup", "/botTOKEN/unpinChatMessage", "/botTOKEN/deleteMessage":
			return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": true}), nil
		case "/botTOKEN/pinChatMessage":
			return nil, errors.New("temporary pin failure")
		default:
			return nil, errors.New("unexpected Telegram endpoint " + req.URL.Path)
		}
	})}
	app.Telegram = client
	app.Config.TelegramChatID = 100

	result := app.expandCollapsedShelf(context.Background(), state.CollapsedShelf{ChatID: 100, MessageID: 88})
	current, _ := app.Store.FindSession(id)
	if result.OK() || !current.Collapsed || current.AnchorMessageID != 0 || app.Store.Snapshot().CollapsedShelf == nil {
		t.Fatalf("expand=%#v session=%#v shelf=%#v", result, current, app.Store.Snapshot().CollapsedShelf)
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
	}); err != nil || !committed {
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

func TestCollapsedShelfHonorsTelegramRetryAfter(t *testing.T) {
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
		ChatID: 100, MessageID: 88, Pinned: true, PinKnown: true,
	}); err != nil || !committed {
		t.Fatalf("prepare collapse committed=%v err=%v", committed, err)
	}
	if _, retired, err := app.Store.FinishCollapsedAnchorRetirement(id, 100, 77); err != nil || !retired {
		t.Fatalf("prepare retirement retired=%v err=%v", retired, err)
	}
	calls := 0
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return telegramTestResponse(t, http.StatusTooManyRequests, map[string]any{
			"ok": false, "error_code": 429, "description": "Too Many Requests",
			"parameters": map[string]any{"retry_after": 31},
		}), nil
	})}
	app.Telegram = client

	app.reconcileCollapsedShelf(context.Background())
	app.reconcileCollapsedShelf(context.Background())

	shelf := app.Store.Snapshot().CollapsedShelf
	if calls != 1 || shelf == nil || time.Until(shelf.RetryAt) < 29*time.Second {
		t.Fatalf("calls=%d shelf=%#v", calls, shelf)
	}
}

func TestEmptyCollapsedShelfHonorsPersistedRetryBeforeRetirement(t *testing.T) {
	app, _, _ := newSafetyApp(t, state.TerminalOriginCreated)
	if _, committed, err := app.Store.SetCollapsedShelfIfEmpty(state.CollapsedShelf{
		ChatID: 100, MessageID: 88, Pinned: true, PinKnown: true, RetryAt: time.Now().UTC().Add(time.Minute),
	}); err != nil || !committed {
		t.Fatalf("set shelf committed=%v err=%v", committed, err)
	}
	calls := 0
	app.Telegram = telegram.New("TOKEN")
	app.Telegram.BaseURL = "https://api.telegram.org/botTOKEN"
	app.Telegram.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("Telegram must remain quiet during persisted shelf backoff")
	})}

	app.reconcileCollapsedShelf(context.Background())

	if calls != 0 || app.Store.Snapshot().CollapsedShelf == nil {
		t.Fatalf("calls=%d shelf=%#v", calls, app.Store.Snapshot().CollapsedShelf)
	}
}

func TestReplacementShelfRetainsPredecessorUntilRetirementSucceeds(t *testing.T) {
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
		ChatID: 100, MessageID: 88, Pinned: true, PinKnown: true,
	}); err != nil || !committed {
		t.Fatalf("collapse committed=%v err=%v", committed, err)
	}
	if _, retired, err := app.Store.FinishCollapsedAnchorRetirement(id, 100, 77); err != nil || !retired {
		t.Fatalf("old anchor retirement committed=%v err=%v", retired, err)
	}
	if _, replaced, err := app.Store.ReplaceCollapsedShelf(88, state.CollapsedShelf{ChatID: 100, MessageID: 89}); err != nil || !replaced {
		t.Fatalf("shelf replacement committed=%v err=%v", replaced, err)
	}
	deleteCalls := 0
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		switch req.URL.Path {
		case "/botTOKEN/editMessageText":
			return telegramTestResponse(t, http.StatusOK, map[string]any{
				"ok": true, "result": map[string]any{"message_id": body["message_id"], "chat": map[string]any{"id": 100}},
			}), nil
		case "/botTOKEN/pinChatMessage":
			return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": true}), nil
		case "/botTOKEN/deleteMessage":
			deleteCalls++
			return telegramTestResponse(t, http.StatusTooManyRequests, map[string]any{
				"ok": false, "error_code": 429, "description": "Too Many Requests",
				"parameters": map[string]any{"retry_after": 31},
			}), nil
		default:
			return nil, errors.New("unexpected Telegram endpoint " + req.URL.Path)
		}
	})}
	app.Telegram = client

	app.reconcileCollapsedShelf(context.Background())
	app.reconcileCollapsedShelf(context.Background())

	shelf := app.Store.Snapshot().CollapsedShelf
	if deleteCalls != 1 || shelf == nil || shelf.RetiringMessageID != 88 || time.Until(shelf.RetiringRetryAt) < 29*time.Second {
		t.Fatalf("delete_calls=%d shelf=%#v", deleteCalls, shelf)
	}
	if _, _, err := app.Store.UpdateCollapsedShelf(89, func(current *state.CollapsedShelf) {
		current.PinKnown = false
		current.Pinned = false
	}); err != nil {
		t.Fatal(err)
	}
	restartCalls := 0
	blockedRestart := &App{Config: app.Config, Store: app.Store, Telegram: telegram.New("TOKEN")}
	blockedRestart.Telegram.BaseURL = "https://api.telegram.org/botTOKEN"
	blockedRestart.Telegram.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(*http.Request) (*http.Response, error) {
		restartCalls++
		return nil, errors.New("Telegram must remain quiet during persisted predecessor backoff")
	})}
	blockedRestart.reconcileCollapsedShelf(context.Background())
	if restartCalls != 0 {
		t.Fatalf("restart made %d Telegram request(s) before predecessor retry_after", restartCalls)
	}
	if _, _, err := app.Store.UpdateCollapsedShelf(89, func(current *state.CollapsedShelf) {
		current.RetiringRetryAt = time.Now().Add(-time.Second)
		current.PinKnown = true
		current.Pinned = true
	}); err != nil {
		t.Fatal(err)
	}
	restarted := &App{Config: app.Config, Store: app.Store}
	restarted.Telegram = telegram.New("TOKEN")
	restarted.Telegram.BaseURL = "https://api.telegram.org/botTOKEN"
	restarted.Telegram.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/botTOKEN/deleteMessage" {
			return nil, errors.New("unexpected Telegram endpoint " + req.URL.Path)
		}
		return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": true}), nil
	})}

	restarted.reconcileCollapsedShelf(context.Background())

	shelf = app.Store.Snapshot().CollapsedShelf
	if shelf == nil || shelf.RetiringMessageID != 0 {
		t.Fatalf("predecessor remained after retry: %#v", shelf)
	}
}

func TestRetiringShelfShowCallbackUsesCurrentShelfIdentity(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	session, _ := app.Store.FindSession(id)
	if _, committed, err := app.Store.CollapseSessionIntoShelf(id, session, state.CollapsedShelf{
		ChatID: 100, MessageID: 88, Pinned: true, PinKnown: true,
	}); err != nil || !committed {
		t.Fatalf("collapse committed=%v err=%v", committed, err)
	}
	if _, replaced, err := app.Store.ReplaceCollapsedShelf(88, state.CollapsedShelf{ChatID: 100, MessageID: 89}); err != nil || !replaced {
		t.Fatalf("replacement committed=%v err=%v", replaced, err)
	}
	app.Telegram = telegram.New("TOKEN")
	app.Telegram.BaseURL = "https://api.telegram.org/botTOKEN"
	app.Telegram.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/botTOKEN/answerCallbackQuery" {
			return nil, errors.New("unexpected Telegram endpoint " + req.URL.Path)
		}
		return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": true}), nil
	})}

	shelf, status := app.validateCollapsedShelfCallback(context.Background(), telegram.CallbackQuery{
		ID: "show-predecessor",
		Message: &telegram.Message{
			MessageID: 88,
			Chat:      telegram.Chat{ID: 100},
		},
	})

	if status != "" || shelf.MessageID != 89 || shelf.RetiringMessageID != 88 {
		t.Fatalf("shelf=%#v status=%q", shelf, status)
	}
	if !app.isCollapsedShelfMessage(100, 88) || !app.isCollapsedShelfMessage(100, 89) {
		t.Fatal("current and predecessor shelf messages must share the shelf reply guidance")
	}
}

func TestCollapsedAnchorCallbackNamesTheShelfShowAction(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	if _, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.Collapsed = true
		session.AnchorChatID = 100
		session.AnchorMessageID = 77
	}); err != nil {
		t.Fatal(err)
	}
	var answer string
	app.Telegram = telegram.New("TOKEN")
	app.Telegram.BaseURL = "https://api.telegram.org/botTOKEN"
	app.Telegram.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		answer, _ = body["text"].(string)
		return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": true}), nil
	})}

	_, status := app.validateAnchorCallback(context.Background(), telegram.CallbackQuery{
		ID: "collapsed",
		Message: &telegram.Message{
			MessageID: 77,
			Chat:      telegram.Chat{ID: 100},
		},
	}, id)

	if status != "callback_user_error" || answer != "Tap ➕ Show on the Collapsed sessions shelf" {
		t.Fatalf("status=%q answer=%q", status, answer)
	}
}

func TestUnavailableReplacementShelfRecoversVisiblePredecessor(t *testing.T) {
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
		ChatID: 100, MessageID: 88, Pinned: true, PinKnown: true,
	}); err != nil || !committed {
		t.Fatalf("collapse committed=%v err=%v", committed, err)
	}
	if _, retired, err := app.Store.FinishCollapsedAnchorRetirement(id, 100, 77); err != nil || !retired {
		t.Fatalf("old anchor retirement committed=%v err=%v", retired, err)
	}
	if _, replaced, err := app.Store.ReplaceCollapsedShelf(88, state.CollapsedShelf{ChatID: 100, MessageID: 89}); err != nil || !replaced {
		t.Fatalf("shelf replacement committed=%v err=%v", replaced, err)
	}
	var paths []string
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		messageID := intFromJSONNumber(body["message_id"])
		paths = append(paths, fmt.Sprintf("%s:%d", req.URL.Path, messageID))
		switch req.URL.Path {
		case "/botTOKEN/editMessageText":
			if messageID == 89 {
				return telegramTestResponse(t, http.StatusBadRequest, map[string]any{
					"ok": false, "error_code": 400, "description": "Bad Request: message to edit not found",
				}), nil
			}
			return telegramTestResponse(t, http.StatusOK, map[string]any{
				"ok": true, "result": map[string]any{"message_id": messageID, "chat": map[string]any{"id": 100}},
			}), nil
		case "/botTOKEN/pinChatMessage":
			return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": true}), nil
		case "/botTOKEN/deleteMessage":
			return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": true}), nil
		default:
			return nil, errors.New("unexpected Telegram endpoint " + req.URL.Path)
		}
	})}
	app.Telegram = client
	app.Config.TelegramChatID = 100

	app.reconcileCollapsedShelf(context.Background())
	recovered := app.Store.Snapshot().CollapsedShelf
	if recovered == nil || recovered.MessageID != 88 || recovered.RetiringMessageID != 89 ||
		recovered.PinKnown || recovered.Pinned {
		t.Fatalf("recovered shelf = %#v paths=%#v", recovered, paths)
	}
	app.reconcileCollapsedShelf(context.Background())

	shelf := app.Store.Snapshot().CollapsedShelf
	if shelf == nil || shelf.MessageID != 88 || shelf.RetiringMessageID != 0 || !shelf.Pinned || !shelf.PinKnown {
		t.Fatalf("reconciled shelf = %#v paths=%#v", shelf, paths)
	}
	want := []string{
		"/botTOKEN/editMessageText:89",
		"/botTOKEN/editMessageText:88",
		"/botTOKEN/pinChatMessage:88",
		"/botTOKEN/deleteMessage:89",
	}
	if strings.Join(paths, "|") != strings.Join(want, "|") {
		t.Fatalf("paths=%#v want=%#v", paths, want)
	}
}

func TestInertProspectiveCleanupDoesNotAmplifyRateLimit(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	session, _ := app.Store.FindSession(id)
	if _, committed, err := app.Store.CollapseSessionIntoShelf(id, session, state.CollapsedShelf{
		ChatID: 100, MessageID: 77, Pinned: true, PinKnown: true,
	}); err != nil || !committed {
		t.Fatalf("collapse committed=%v err=%v", committed, err)
	}
	app.Telegram = telegram.New("TOKEN")
	app.Telegram.BaseURL = "https://api.telegram.org/botTOKEN"
	var paths []string
	app.Telegram.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.Path)
		return telegramTestResponse(t, http.StatusTooManyRequests, map[string]any{
			"ok": false, "error_code": 429, "description": "Too Many Requests",
			"parameters": map[string]any{"retry_after": 31},
		}), nil
	})}

	app.retireProspectiveMessage(context.Background(), 100, 88)

	if len(paths) != 1 || paths[0] != "/botTOKEN/deleteMessage" {
		t.Fatalf("prospective cleanup paths = %#v", paths)
	}
	cleanups := app.Store.Snapshot().PendingMessageCleanups
	if len(cleanups) != 1 || cleanups[0].MessageID != 88 || !cleanups[0].RateLimited ||
		time.Until(cleanups[0].RetryAt) < 29*time.Second {
		t.Fatalf("durable prospective cleanup = %#v", cleanups)
	}
	shelf := app.Store.Snapshot().CollapsedShelf
	if shelf == nil || time.Until(shelf.RetryAt) < 29*time.Second {
		t.Fatalf("shelf flood wait = %#v", shelf)
	}

	restartCalls := 0
	restarted := &App{Config: app.Config, Store: app.Store, Telegram: telegram.New("TOKEN")}
	restarted.Telegram.BaseURL = "https://api.telegram.org/botTOKEN"
	restarted.Telegram.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(*http.Request) (*http.Response, error) {
		restartCalls++
		return nil, errors.New("cleanup must honor persisted flood wait")
	})}
	restarted.reconcileCollapsedShelf(context.Background())
	if restartCalls != 0 {
		t.Fatalf("restart made %d cleanup request(s) before retry_after", restartCalls)
	}
	cleanups[0].RetryAt = time.Now().Add(-time.Second)
	if err := app.Store.RememberMessageCleanup(cleanups[0]); err != nil {
		t.Fatal(err)
	}
	restarted.Telegram.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": true}), nil
	})}
	restarted.reconcileCollapsedShelf(context.Background())
	if cleanups := app.Store.Snapshot().PendingMessageCleanups; len(cleanups) != 0 {
		t.Fatalf("cleanup survived successful restart retirement: %#v", cleanups)
	}
}

func TestMissingPendingRestoreBecomesEligibleForReplacement(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	if _, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.Collapsed = true
		session.AnchorChatID = 100
		session.AnchorMessageID = 0
	}); err != nil {
		t.Fatal(err)
	}
	if _, committed, err := app.Store.SetCollapsedShelfIfEmpty(state.CollapsedShelf{ChatID: 100, MessageID: 88, Pinned: true, PinKnown: true}); err != nil || !committed {
		t.Fatalf("set shelf committed=%v err=%v", committed, err)
	}
	if _, begun, err := app.Store.BeginExpandSessionFromShelf(id, 88, 100, 89); err != nil || !begun {
		t.Fatalf("begin restore committed=%v err=%v", begun, err)
	}
	app.Telegram = telegram.New("TOKEN")
	app.Telegram.BaseURL = "https://api.telegram.org/botTOKEN"
	app.Telegram.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return telegramTestResponse(t, http.StatusBadRequest, map[string]any{
			"ok": false, "error_code": 400, "description": "Bad Request: message to edit not found",
		}), nil
	})}

	current, _ := app.Store.FindSession(id)
	if app.finishPendingRestoreLocked(context.Background(), current) {
		t.Fatal("missing prospective restore unexpectedly completed")
	}
	current, _ = app.Store.FindSession(id)
	if !current.Collapsed || current.PendingRestore != nil {
		t.Fatalf("missing restore remained wedged: %#v", current)
	}
	if _, target, found := app.Store.FindReplyTarget(100, 89); !found || target != state.ReplyTargetStale {
		t.Fatalf("missing restore target=%q found=%v", target, found)
	}
}

func TestExpandStopsAfterFirstTelegramFloodWait(t *testing.T) {
	app, _, firstID := newSafetyApp(t, state.TerminalOriginCreated)
	second, err := app.Store.AllocateSession("main", "@2", "%2", "second")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int{firstID, second.ID} {
		session, _ := app.Store.FindSession(id)
		if _, committed, err := app.Store.CollapseSessionIntoShelf(id, session, state.CollapsedShelf{
			ChatID: 100, MessageID: 88, Pinned: true, PinKnown: true,
		}); err != nil || !committed {
			t.Fatalf("collapse [%d] committed=%v err=%v", id, committed, err)
		}
	}
	calls := 0
	app.Telegram = telegram.New("TOKEN")
	app.Telegram.BaseURL = "https://api.telegram.org/botTOKEN"
	app.Telegram.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return telegramTestResponse(t, http.StatusTooManyRequests, map[string]any{
			"ok": false, "error_code": 429, "description": "Too Many Requests",
			"parameters": map[string]any{"retry_after": 31},
		}), nil
	})}

	result := app.expandCollapsedShelf(context.Background(), state.CollapsedShelf{ChatID: 100, MessageID: 88})

	if result.OK() || calls != 1 {
		t.Fatalf("expand=%#v Telegram calls=%d, want one request", result, calls)
	}
	shelf := app.Store.Snapshot().CollapsedShelf
	if shelf == nil || time.Until(shelf.RetryAt) < 29*time.Second {
		t.Fatalf("shelf flood wait = %#v", shelf)
	}
}

func TestExpandStopsAfterPostPromotionControlsFloodWait(t *testing.T) {
	app, _, firstID := newSafetyApp(t, state.TerminalOriginCreated)
	second, err := app.Store.AllocateSession("main", "@2", "%2", "second")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int{firstID, second.ID} {
		session, _ := app.Store.FindSession(id)
		if _, committed, err := app.Store.CollapseSessionIntoShelf(id, session, state.CollapsedShelf{
			ChatID: 100, MessageID: 88, Pinned: true, PinKnown: true,
		}); err != nil || !committed {
			t.Fatalf("collapse [%d] committed=%v err=%v", id, committed, err)
		}
	}
	var paths []string
	app.Telegram = telegram.New("TOKEN")
	app.Telegram.BaseURL = "https://api.telegram.org/botTOKEN"
	app.Telegram.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.Path)
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		switch req.URL.Path {
		case "/botTOKEN/sendMessage":
			return telegramTestResponse(t, http.StatusOK, map[string]any{
				"ok": true, "result": map[string]any{"message_id": 89, "chat": map[string]any{"id": 100}},
			}), nil
		case "/botTOKEN/pinChatMessage":
			return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": true}), nil
		case "/botTOKEN/editMessageReplyMarkup":
			return telegramTestResponse(t, http.StatusTooManyRequests, map[string]any{
				"ok": false, "error_code": 429, "description": "Too Many Requests",
				"parameters": map[string]any{"retry_after": 31},
			}), nil
		default:
			return nil, errors.New("unexpected Telegram endpoint " + req.URL.Path)
		}
	})}
	app.Config.TelegramChatID = 100
	runCtx, cancel := context.WithCancel(context.Background())
	cancel()
	app.runCtx = runCtx

	result := app.expandCollapsedShelf(context.Background(), state.CollapsedShelf{ChatID: 100, MessageID: 88})

	if result.OK() || len(paths) != 3 ||
		strings.Join(paths, "|") != strings.Join([]string{
			"/botTOKEN/sendMessage",
			"/botTOKEN/pinChatMessage",
			"/botTOKEN/editMessageReplyMarkup",
		}, "|") {
		t.Fatalf("expand=%#v Telegram paths=%#v", result, paths)
	}
	shelf := app.Store.Snapshot().CollapsedShelf
	if shelf == nil || time.Until(shelf.RetryAt) < 29*time.Second {
		t.Fatalf("shelf flood wait = %#v", shelf)
	}
	restored := 0
	for _, session := range app.Store.Snapshot().TerminalSessions {
		if !session.Collapsed {
			restored++
		}
	}
	if restored != 0 {
		t.Fatalf("restored sessions = %d, want none after controls rollback", restored)
	}
}

func TestExpandLostSessionUsesRecoveryCopyAndControls(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	session, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.State = state.TerminalLost
		session.LastSummary = "Old running status."
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, committed, err := app.Store.CollapseSessionIntoShelf(id, session, state.CollapsedShelf{
		ChatID: 100, MessageID: 88, Pinned: true, PinKnown: true,
	}); err != nil || !committed {
		t.Fatalf("collapse committed=%v err=%v", committed, err)
	}
	var sentText string
	var controls map[string]any
	app.Telegram = telegram.New("TOKEN")
	app.Telegram.BaseURL = "https://api.telegram.org/botTOKEN"
	app.Telegram.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		switch req.URL.Path {
		case "/botTOKEN/sendMessage":
			sentText, _ = body["text"].(string)
			return telegramTestResponse(t, http.StatusOK, map[string]any{
				"ok": true, "result": map[string]any{"message_id": 89, "chat": map[string]any{"id": 100}},
			}), nil
		case "/botTOKEN/pinChatMessage", "/botTOKEN/deleteMessage":
			return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": true}), nil
		case "/botTOKEN/editMessageReplyMarkup":
			controls, _ = body["reply_markup"].(map[string]any)
			return telegramTestResponse(t, http.StatusOK, map[string]any{
				"ok": true, "result": map[string]any{"message_id": 89, "chat": map[string]any{"id": 100}},
			}), nil
		default:
			return nil, errors.New("unexpected Telegram endpoint " + req.URL.Path)
		}
	})}
	app.Config.TelegramChatID = 100
	runCtx, cancel := context.WithCancel(context.Background())
	cancel()
	app.runCtx = runCtx

	result := app.expandCollapsedShelf(context.Background(), state.CollapsedShelf{ChatID: 100, MessageID: 88})

	if !result.OK() || !strings.Contains(sentText, "tmux pane is lost") ||
		strings.Contains(sentText, "Refreshing now") || controls == nil {
		t.Fatalf("expand=%#v text=%q controls=%#v", result, sentText, controls)
	}
}

func TestUnavailableRestoredControlsReturnSessionToShelf(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	session, _ := app.Store.FindSession(id)
	if _, committed, err := app.Store.CollapseSessionIntoShelf(id, session, state.CollapsedShelf{
		ChatID: 100, MessageID: 88, Pinned: true, PinKnown: true,
	}); err != nil || !committed {
		t.Fatalf("collapse committed=%v err=%v", committed, err)
	}
	app.Telegram = telegram.New("TOKEN")
	app.Telegram.BaseURL = "https://api.telegram.org/botTOKEN"
	app.Telegram.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		switch req.URL.Path {
		case "/botTOKEN/sendMessage":
			return telegramTestResponse(t, http.StatusOK, map[string]any{
				"ok": true, "result": map[string]any{"message_id": 89, "chat": map[string]any{"id": 100}},
			}), nil
		case "/botTOKEN/pinChatMessage":
			return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": true}), nil
		case "/botTOKEN/editMessageReplyMarkup":
			return telegramTestResponse(t, http.StatusBadRequest, map[string]any{
				"ok": false, "error_code": 400, "description": "Bad Request: message to edit not found",
			}), nil
		default:
			return nil, errors.New("unexpected Telegram endpoint " + req.URL.Path)
		}
	})}
	app.Config.TelegramChatID = 100

	result := app.expandCollapsedShelf(context.Background(), state.CollapsedShelf{ChatID: 100, MessageID: 88})

	current, _ := app.Store.FindSession(id)
	shelf := app.Store.Snapshot().CollapsedShelf
	if result.OK() || !current.Collapsed || current.AnchorMessageID != 89 ||
		shelf == nil || shelf.MessageID != 88 || time.Until(shelf.RetryAt) < 8*time.Second {
		t.Fatalf("expand=%#v session=%#v shelf=%#v", result, current, shelf)
	}
	if _, target, found := app.Store.FindReplyTarget(100, 89); !found || target != state.ReplyTargetStale {
		t.Fatalf("failed controls target=%q found=%v", target, found)
	}
}

func TestTransferFailureFallsBackWhenOriginalAnchorDisappears(t *testing.T) {
	app, _, _ := newSafetyApp(t, state.TerminalOriginCreated)
	var replyTargets []int
	app.Telegram = telegram.New("TOKEN")
	app.Telegram.BaseURL = "https://api.telegram.org/botTOKEN"
	app.Telegram.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		replyTo := 0
		if params, ok := body["reply_parameters"].(map[string]any); ok {
			replyTo = intFromJSONNumber(params["message_id"])
		}
		replyTargets = append(replyTargets, replyTo)
		if replyTo != 0 {
			return telegramTestResponse(t, http.StatusBadRequest, map[string]any{
				"ok": false, "error_code": 400, "description": "Bad Request: reply message not found",
			}), nil
		}
		return telegramTestResponse(t, http.StatusOK, map[string]any{
			"ok": true, "result": map[string]any{"message_id": 90, "chat": map[string]any{"id": 100}},
		}), nil
	})}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	app.replyTransferFailure(canceled, telegram.Message{
		MessageID: 77,
		Chat:      telegram.Chat{ID: 100},
	}, "anchor moved")

	if len(replyTargets) != 2 || replyTargets[0] != 77 || replyTargets[1] != 0 {
		t.Fatalf("reply targets = %#v", replyTargets)
	}
}

func TestCollapsedAnchorRetirementHonorsRetryAfterWithoutAmplifyingMediaDelete(t *testing.T) {
	for _, test := range []struct {
		name   string
		format string
		path   string
	}{
		{name: "text", format: anchorFormatText, path: "/botTOKEN/editMessageText"},
		{name: "media", format: anchorFormatSnapshot, path: "/botTOKEN/deleteMessage"},
	} {
		t.Run(test.name, func(t *testing.T) {
			app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
			session, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
				session.Collapsed = true
				session.AnchorChatID = 100
				session.AnchorMessageID = 77
				session.AnchorFormat = test.format
			})
			if err != nil {
				t.Fatal(err)
			}
			var paths []string
			client := telegram.New("TOKEN")
			client.BaseURL = "https://api.telegram.org/botTOKEN"
			client.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				paths = append(paths, req.URL.Path)
				return telegramTestResponse(t, http.StatusTooManyRequests, map[string]any{
					"ok": false, "error_code": 429, "description": "Too Many Requests",
					"parameters": map[string]any{"retry_after": 31},
				}), nil
			})}
			app.Telegram = client

			if app.retireCollapsedSessionAnchorLocked(context.Background(), session) {
				t.Fatal("rate-limited retirement unexpectedly completed")
			}
			if app.retireCollapsedSessionAnchorLocked(context.Background(), session) {
				t.Fatal("retirement ignored its retry deadline")
			}
			current, _ := app.Store.FindSession(id)
			if len(paths) != 1 || paths[0] != test.path || time.Until(current.RetiringAnchorRetryAt) < 29*time.Second {
				t.Fatalf("paths=%#v retry_at=%s", paths, current.RetiringAnchorRetryAt)
			}
		})
	}
}

func TestPendingRestoreReconcilesAfterRestartBoundary(t *testing.T) {
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
		ChatID: 100, MessageID: 88, Pinned: true, PinKnown: true,
	}); err != nil || !committed {
		t.Fatalf("collapse committed=%v err=%v", committed, err)
	}
	if _, retired, err := app.Store.FinishCollapsedAnchorRetirement(id, 100, 77); err != nil || !retired {
		t.Fatalf("old anchor retirement committed=%v err=%v", retired, err)
	}
	if _, begun, err := app.Store.BeginExpandSessionFromShelf(id, 88, 100, 89); err != nil || !begun {
		t.Fatalf("pending restore begun=%v err=%v", begun, err)
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
		case "/botTOKEN/editMessageReplyMarkup":
			if _, target, found := app.Store.FindReplyTarget(100, 89); !found || target != state.ReplyTargetCurrent {
				t.Fatalf("controls became visible before restore identity was current: target=%q found=%v", target, found)
			}
			return telegramTestResponse(t, http.StatusOK, map[string]any{
				"ok": true, "result": map[string]any{"message_id": body["message_id"], "chat": map[string]any{"id": 100}},
			}), nil
		case "/botTOKEN/pinChatMessage":
			pending, _ := app.Store.FindSession(id)
			if pending.PendingRestore == nil || !pending.Collapsed {
				t.Fatalf("restore identity promoted before inert pin: %#v", pending)
			}
			return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": true}), nil
		case "/botTOKEN/deleteMessage":
			return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": true}), nil
		default:
			return nil, errors.New("unexpected Telegram endpoint " + req.URL.Path)
		}
	})}
	app.Telegram = client
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	app.runCtx = ctx

	app.reconcileCollapsedShelf(context.Background())

	current, _ := app.Store.FindSession(id)
	if current.Collapsed || current.PendingRestore != nil || current.AnchorMessageID != 89 || !current.AnchorPinned || app.Store.Snapshot().CollapsedShelf != nil {
		t.Fatalf("reconciled session=%#v shelf=%#v", current, app.Store.Snapshot().CollapsedShelf)
	}
	for _, want := range []string{"/botTOKEN/editMessageReplyMarkup", "/botTOKEN/pinChatMessage", "/botTOKEN/deleteMessage"} {
		if !containsCollapsePath(paths, want) {
			t.Fatalf("requests=%#v missing %s", paths, want)
		}
	}
	if slices.Index(paths, "/botTOKEN/pinChatMessage") > slices.Index(paths, "/botTOKEN/editMessageReplyMarkup") {
		t.Fatalf("restore controls preceded inert pin and durable promotion: %#v", paths)
	}
}

func TestPromotedRestoreControlsReconcileAfterRestartBoundary(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	session, _ := app.Store.FindSession(id)
	if _, committed, err := app.Store.CollapseSessionIntoShelf(id, session, state.CollapsedShelf{
		ChatID: 100, MessageID: 88, Pinned: true, PinKnown: true,
	}); err != nil || !committed {
		t.Fatalf("collapse committed=%v err=%v", committed, err)
	}
	if _, begun, err := app.Store.BeginExpandSessionFromShelf(id, 88, 100, 89); err != nil || !begun {
		t.Fatalf("begin restore committed=%v err=%v", begun, err)
	}
	if _, promoted, err := app.Store.FinishExpandSessionFromShelf(id, 100, 89); err != nil || !promoted {
		t.Fatalf("promote restore committed=%v err=%v", promoted, err)
	}
	var paths []string
	app.Telegram = telegram.New("TOKEN")
	app.Telegram.BaseURL = "https://api.telegram.org/botTOKEN"
	app.Telegram.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.Path)
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		switch req.URL.Path {
		case "/botTOKEN/editMessageReplyMarkup":
			return telegramTestResponse(t, http.StatusOK, map[string]any{
				"ok": true, "result": map[string]any{"message_id": body["message_id"], "chat": map[string]any{"id": 100}},
			}), nil
		case "/botTOKEN/deleteMessage":
			return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": true}), nil
		default:
			return nil, errors.New("unexpected Telegram endpoint " + req.URL.Path)
		}
	})}
	runCtx, cancel := context.WithCancel(context.Background())
	cancel()
	app.runCtx = runCtx

	app.reconcileCollapsedShelf(context.Background())

	current, _ := app.Store.FindSession(id)
	if current.Collapsed || current.PendingRestore != nil || current.AnchorMessageID != 89 ||
		app.Store.Snapshot().CollapsedShelf != nil {
		t.Fatalf("reconciled session=%#v shelf=%#v paths=%#v", current, app.Store.Snapshot().CollapsedShelf, paths)
	}
	want := []string{"/botTOKEN/editMessageReplyMarkup", "/botTOKEN/deleteMessage"}
	if strings.Join(paths, "|") != strings.Join(want, "|") {
		t.Fatalf("paths=%#v want=%#v", paths, want)
	}
}

func TestPendingRestoreRetrySurvivesPreReplacementStateFailureInMemory(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(stateDir, "state.json"), filepath.Join(stateDir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.AllocateSession("main", "@1", "%1", "shell")
	if err != nil {
		t.Fatal(err)
	}
	session = bindTestSession(t, store, session.ID)
	session, _, err = store.UpdateSession(session.ID, func(session *state.TerminalSession) {
		session.AnchorChatID = 100
		session.AnchorMessageID = 77
		session.AnchorFormat = anchorFormatText
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, committed, err := store.CollapseSessionIntoShelf(session.ID, session, state.CollapsedShelf{
		ChatID: 100, MessageID: 88, Pinned: true, PinKnown: true,
	}); err != nil || !committed {
		t.Fatalf("collapse committed=%v err=%v", committed, err)
	}
	if _, retired, err := store.FinishCollapsedAnchorRetirement(session.ID, 100, 77); err != nil || !retired {
		t.Fatalf("old anchor retirement committed=%v err=%v", retired, err)
	}
	pending, begun, err := store.BeginExpandSessionFromShelf(session.ID, 88, 100, 89)
	if err != nil || !begun {
		t.Fatalf("pending restore begun=%v err=%v", begun, err)
	}
	movedDir := stateDir + ".unavailable"
	if err := os.Rename(stateDir, movedDir); err != nil {
		t.Fatal(err)
	}
	defer os.Rename(movedDir, stateDir)
	calls := 0
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.Path == "/botTOKEN/editMessageReplyMarkup" {
			return telegramTestResponse(t, http.StatusOK, map[string]any{
				"ok": true, "result": map[string]any{"message_id": 89, "chat": map[string]any{"id": 100}},
			}), nil
		}
		if req.URL.Path == "/botTOKEN/pinChatMessage" {
			return telegramTestResponse(t, http.StatusTooManyRequests, map[string]any{
				"ok": false, "error_code": 429, "description": "Too Many Requests",
				"parameters": map[string]any{"retry_after": 31},
			}), nil
		}
		return nil, errors.New("unexpected Telegram endpoint " + req.URL.Path)
	})}
	app := &App{Config: config.Config{TelegramChatID: 100}, Store: store, Telegram: client}

	if app.finishPendingRestoreLocked(context.Background(), pending) {
		t.Fatal("rate-limited pending restore unexpectedly completed")
	}
	if app.finishPendingRestoreLocked(context.Background(), pending) {
		t.Fatal("pending restore ignored process-local retry deadline")
	}
	current, _ := store.FindSession(session.ID)
	if calls != 1 || current.PendingRestore == nil || !current.PendingRestore.RetryAt.IsZero() ||
		time.Until(app.pendingRestoreRetryDeadline(session.ID, *current.PendingRestore)) < 29*time.Second {
		t.Fatalf("calls=%d pending=%#v", calls, current.PendingRestore)
	}
}

func TestClosingCollapsedSessionRetiresPendingRestoreAndShelf(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginAttached)
	session, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.AnchorChatID = 100
		session.AnchorMessageID = 77
		session.AnchorFormat = anchorFormatText
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, committed, err := app.Store.CollapseSessionIntoShelf(id, session, state.CollapsedShelf{
		ChatID: 100, MessageID: 88, Pinned: true, PinKnown: true,
	}); err != nil || !committed {
		t.Fatalf("collapse committed=%v err=%v", committed, err)
	}
	if _, retired, err := app.Store.FinishCollapsedAnchorRetirement(id, 100, 77); err != nil || !retired {
		t.Fatalf("old anchor retirement committed=%v err=%v", retired, err)
	}
	if _, begun, err := app.Store.BeginExpandSessionFromShelf(id, 88, 100, 89); err != nil || !begun {
		t.Fatalf("pending restore begun=%v err=%v", begun, err)
	}
	var deleted []int
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		switch req.URL.Path {
		case "/botTOKEN/editMessageReplyMarkup", "/botTOKEN/unpinChatMessage":
			return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": true}), nil
		case "/botTOKEN/deleteMessage":
			deleted = append(deleted, intFromJSONNumber(body["message_id"]))
			return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": true}), nil
		default:
			return nil, errors.New("unexpected Telegram endpoint " + req.URL.Path)
		}
	})}
	app.Telegram = client

	result := app.closeSession(context.Background(), id)

	current, _ := app.Store.FindSession(id)
	if !result.OK() || current.State != state.TerminalClosed || current.Collapsed || current.PendingRestore != nil || app.Store.Snapshot().CollapsedShelf != nil {
		t.Fatalf("close=%#v session=%#v shelf=%#v", result, current, app.Store.Snapshot().CollapsedShelf)
	}
	if len(deleted) != 2 || deleted[0] != 89 || deleted[1] != 88 {
		t.Fatalf("deleted messages = %#v, want pending restore then shelf", deleted)
	}
}

func TestClosingCollapsedSessionRetainsRateLimitedPendingRestoreForRestart(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginAttached)
	session, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.AnchorChatID = 100
		session.AnchorMessageID = 77
		session.AnchorFormat = anchorFormatText
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, committed, err := app.Store.CollapseSessionIntoShelf(id, session, state.CollapsedShelf{
		ChatID: 100, MessageID: 88, Pinned: true, PinKnown: true,
	}); err != nil || !committed {
		t.Fatalf("collapse committed=%v err=%v", committed, err)
	}
	if _, retired, err := app.Store.FinishCollapsedAnchorRetirement(id, 100, 77); err != nil || !retired {
		t.Fatalf("old anchor retirement committed=%v err=%v", retired, err)
	}
	if _, begun, err := app.Store.BeginExpandSessionFromShelf(id, 88, 100, 89); err != nil || !begun {
		t.Fatalf("pending restore begun=%v err=%v", begun, err)
	}
	pendingDeletes := 0
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		if req.URL.Path != "/botTOKEN/deleteMessage" {
			return nil, errors.New("unexpected Telegram endpoint " + req.URL.Path)
		}
		if intFromJSONNumber(body["message_id"]) == 89 {
			pendingDeletes++
			return telegramTestResponse(t, http.StatusTooManyRequests, map[string]any{
				"ok": false, "error_code": 429, "description": "Too Many Requests",
				"parameters": map[string]any{"retry_after": 31},
			}), nil
		}
		return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": true}), nil
	})}
	app.Telegram = client

	result := app.closeSession(context.Background(), id)
	current, _ := app.Store.FindSession(id)
	if !result.OK() || current.State != state.TerminalClosed || current.PendingRestore == nil ||
		time.Until(current.PendingRestore.RetryAt) < 29*time.Second || pendingDeletes != 1 {
		t.Fatalf("close=%#v session=%#v pending_deletes=%d", result, current, pendingDeletes)
	}

	restartCalls := 0
	restarted := &App{Config: app.Config, Store: app.Store, Telegram: telegram.New("TOKEN")}
	restarted.Telegram.BaseURL = "https://api.telegram.org/botTOKEN"
	restarted.Telegram.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(*http.Request) (*http.Response, error) {
		restartCalls++
		return nil, errors.New("pending cleanup must honor persisted retry_after")
	})}
	restarted.reconcileCollapsedShelf(context.Background())
	if restartCalls != 0 {
		t.Fatalf("restart made %d early cleanup request(s)", restartCalls)
	}
	if _, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.PendingRestore.RetryAt = time.Now().Add(-time.Second)
	}); err != nil {
		t.Fatal(err)
	}
	restarted.Telegram.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/botTOKEN/deleteMessage" {
			return nil, errors.New("unexpected Telegram endpoint " + req.URL.Path)
		}
		return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": true}), nil
	})}
	restarted.reconcileCollapsedShelf(context.Background())
	current, _ = app.Store.FindSession(id)
	if current.PendingRestore != nil {
		t.Fatalf("pending restore survived successful restart cleanup: %#v", current.PendingRestore)
	}
}

func TestExpandCallbackDoesNotBlockTelegramUpdateLoop(t *testing.T) {
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
		ChatID: 100, MessageID: 88, Pinned: true, PinKnown: true,
	}); err != nil || !committed {
		t.Fatalf("collapse committed=%v err=%v", committed, err)
	}
	if _, retired, err := app.Store.FinishCollapsedAnchorRetirement(id, 100, 77); err != nil || !retired {
		t.Fatalf("old anchor retirement committed=%v err=%v", retired, err)
	}
	sendStarted := make(chan struct{})
	releaseSend := make(chan struct{})
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		switch req.URL.Path {
		case "/botTOKEN/answerCallbackQuery":
			return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": true}), nil
		case "/botTOKEN/sendMessage":
			close(sendStarted)
			<-releaseSend
			return telegramTestResponse(t, http.StatusOK, map[string]any{
				"ok": true, "result": map[string]any{"message_id": 89, "chat": map[string]any{"id": 100}},
			}), nil
		case "/botTOKEN/editMessageReplyMarkup":
			return telegramTestResponse(t, http.StatusOK, map[string]any{
				"ok": true, "result": map[string]any{"message_id": body["message_id"], "chat": map[string]any{"id": 100}},
			}), nil
		case "/botTOKEN/pinChatMessage", "/botTOKEN/deleteMessage":
			return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": true}), nil
		default:
			return nil, errors.New("unexpected Telegram endpoint " + req.URL.Path)
		}
	})}
	app.Telegram = client
	app.Config.TelegramAllowedUserID = 42
	app.Config.TelegramChatID = 100
	app.runCtx = context.Background()
	app.refreshHook = func(context.Context, int, bool) {}

	returned := make(chan string, 1)
	go func() {
		returned <- app.handleCallback(context.Background(), telegram.CallbackQuery{
			ID: "expand", From: telegram.User{ID: 42}, Data: "expand-all:0",
			Message: &telegram.Message{MessageID: 88, Chat: telegram.Chat{ID: 100}},
		})
	}()
	select {
	case status := <-returned:
		if status != "callback_ok" {
			t.Fatalf("callback status = %q", status)
		}
	case <-time.After(time.Second):
		t.Fatal("callback waited for shelf restoration")
	}
	select {
	case <-sendStarted:
	case <-time.After(time.Second):
		t.Fatal("restore worker did not start")
	}
	close(releaseSend)
	app.transferWG.Wait()
	app.refreshWG.Wait()
}

func TestCollapseCallbackDoesNotBlockTelegramUpdateLoop(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	if _, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.AnchorChatID = 100
		session.AnchorMessageID = 77
		session.AnchorFormat = anchorFormatText
		session.AnchorPinned = true
		session.AnchorPinKnown = true
		session.LastSummary = "Cached status."
	}); err != nil {
		t.Fatal(err)
	}
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		switch req.URL.Path {
		case "/botTOKEN/answerCallbackQuery", "/botTOKEN/pinChatMessage", "/botTOKEN/unpinChatMessage":
			return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": true}), nil
		case "/botTOKEN/sendMessage":
			return telegramTestResponse(t, http.StatusOK, map[string]any{
				"ok": true, "result": map[string]any{"message_id": 88, "chat": map[string]any{"id": 100}},
			}), nil
		case "/botTOKEN/editMessageText":
			return telegramTestResponse(t, http.StatusOK, map[string]any{
				"ok": true, "result": map[string]any{"message_id": body["message_id"], "chat": map[string]any{"id": 100}},
			}), nil
		default:
			return nil, errors.New("unexpected Telegram endpoint " + req.URL.Path)
		}
	})}
	app.Telegram = client
	app.Config.TelegramAllowedUserID = 42
	app.Config.TelegramChatID = 100
	app.runCtx = context.Background()

	disclosure := app.disclosureMutex(id)
	disclosure.Lock()
	returned := make(chan string, 1)
	go func() {
		returned <- app.handleCallback(context.Background(), telegram.CallbackQuery{
			ID: "collapse", From: telegram.User{ID: 42}, Data: "collapse:1",
			Message: &telegram.Message{MessageID: 77, Chat: telegram.Chat{ID: 100}},
		})
	}()
	select {
	case status := <-returned:
		if status != "callback_ok" {
			t.Fatalf("callback status = %q", status)
		}
	case <-time.After(time.Second):
		t.Fatal("callback waited for collapse disclosure lock")
	}
	disclosure.Unlock()
	app.transferWG.Wait()
}

func TestCollapsedShelfQueueSaturationDoesNotSendInlineReply(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	if _, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.AnchorChatID = 100
		session.AnchorMessageID = 77
		session.AnchorFormat = anchorFormatText
	}); err != nil {
		t.Fatal(err)
	}
	app.Config.TelegramAllowedUserID = 42
	app.Config.TelegramChatID = 100
	app.runCtx = context.Background()
	app.transferQueue = make(chan struct{}, 1)
	app.transferQueue <- struct{}{}
	var paths []string
	app.Telegram = telegram.New("TOKEN")
	app.Telegram.BaseURL = "https://api.telegram.org/botTOKEN"
	app.Telegram.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.Path)
		if req.URL.Path != "/botTOKEN/answerCallbackQuery" {
			return nil, errors.New("queue saturation must not send an inline reply")
		}
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		if body["text"] != "Engram is busy; try Hide again" {
			return nil, fmt.Errorf("queue saturation callback text = %q", body["text"])
		}
		return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": true}), nil
	})}

	status := app.handleCallback(context.Background(), telegram.CallbackQuery{
		ID: "collapse", From: telegram.User{ID: 42}, Data: "collapse:1",
		Message: &telegram.Message{MessageID: 77, Chat: telegram.Chat{ID: 100}},
	})

	if status != "callback_state_failed" || len(paths) != 1 || paths[0] != "/botTOKEN/answerCallbackQuery" {
		t.Fatalf("status=%q paths=%#v", status, paths)
	}
	<-app.transferQueue
}

func TestCollapseWaitsForInFlightDisclosure(t *testing.T) {
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

	disclosure := app.disclosureMutex(id)
	disclosure.Lock()
	result := make(chan actionResult, 1)
	go func() {
		result <- app.collapseAnchor(context.Background(), session)
	}()
	select {
	case <-result:
		t.Fatal("collapse crossed an in-flight disclosure")
	case <-time.After(100 * time.Millisecond):
	}
	disclosure.Unlock()
	select {
	case got := <-result:
		if !got.OK() {
			t.Fatalf("collapse after disclosure = %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("collapse did not resume after disclosure completed")
	}
}

func TestCollapseWaitsForInFlightConversationWork(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	session, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.AnchorChatID = 100
		session.AnchorMessageID = 77
		session.AnchorFormat = anchorFormatText
		session.WatchEnabled = true
	})
	if err != nil {
		t.Fatal(err)
	}
	release, acquired := app.acquireConversation(context.Background(), id)
	if !acquired {
		t.Fatal("could not hold conversation gate")
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan actionResult, 1)
	go func() {
		result <- app.collapseAnchor(ctx, session)
	}()
	select {
	case <-result:
		t.Fatal("collapse crossed in-flight conversation work")
	case <-time.After(100 * time.Millisecond):
	}
	writer := make(chan struct{})
	go func() {
		app.presentationMu.Lock()
		close(writer)
		app.presentationMu.Unlock()
	}()
	select {
	case <-writer:
	case <-time.After(time.Second):
		t.Fatal("collapse held presentation lock while waiting for conversation work")
	}
	cancel()
	release()
	select {
	case got := <-result:
		if got.OK() {
			t.Fatalf("canceled collapse = %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled collapse did not return")
	}
	current, _ := app.Store.FindSession(id)
	if current.Collapsed || current.PendingCollapse {
		t.Fatalf("canceled collapse changed membership: %#v", current)
	}
}

func TestPendingCollapseReconciliationWaitsForConversationWork(t *testing.T) {
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
	if _, committed, err := app.Store.BeginCollapseSessionIntoShelf(id, session, state.CollapsedShelf{
		ChatID: 100, MessageID: 88, Pinned: true, PinKnown: true,
	}); err != nil || !committed {
		t.Fatalf("begin collapse committed=%v err=%v", committed, err)
	}
	rendered := app.renderCollapsedShelf(app.Store.Snapshot().TerminalSessions)
	if _, found, err := app.Store.UpdateCollapsedShelf(88, func(shelf *state.CollapsedShelf) {
		shelf.LastRenderHash = sha(rendered)
	}); err != nil || !found {
		t.Fatalf("prepare shelf found=%v err=%v", found, err)
	}
	release, acquired := app.acquireConversation(context.Background(), id)
	if !acquired {
		t.Fatal("could not hold conversation gate")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		app.reconcileCollapsedShelf(ctx)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("pending collapse crossed in-flight conversation work")
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled pending reconciliation did not return")
	}
	release()
	current, _ := app.Store.FindSession(id)
	if current.Collapsed || !current.PendingCollapse || current.AnchorMessageID != 77 {
		t.Fatalf("canceled pending reconciliation changed route: %#v", current)
	}

	disclosure := app.disclosureMutex(id)
	disclosure.Lock()
	ctx, cancel = context.WithCancel(context.Background())
	done = make(chan struct{})
	go func() {
		app.reconcileCollapsedShelf(ctx)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("pending collapse crossed in-flight disclosure work")
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	disclosure.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled disclosure reconciliation did not return")
	}
	current, _ = app.Store.FindSession(id)
	if current.Collapsed || !current.PendingCollapse || current.AnchorMessageID != 77 {
		t.Fatalf("disclosure-canceled reconciliation changed route: %#v", current)
	}
}

func TestExistingShelfDoesNotEditBeforeStateCommit(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(stateDir, "state.json"), filepath.Join(stateDir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.AllocateSession("main", "@1", "%1", "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.AllocateSession("main", "@2", "%2", "second")
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []struct {
		id        int
		messageID int
	}{
		{first.ID, 77},
		{second.ID, 78},
	} {
		if _, _, err := store.UpdateSession(target.id, func(session *state.TerminalSession) {
			session.TmuxServerID = appTestServerID
			session.AnchorChatID = 100
			session.AnchorMessageID = target.messageID
			session.AnchorFormat = anchorFormatText
			session.AnchorPinned = true
			session.AnchorPinKnown = true
			session.WatchEnabled = true
			session.LastSummary = session.Title + " cached"
		}); err != nil {
			t.Fatal(err)
		}
	}
	app := &App{Config: config.Config{TelegramChatID: 100}, Store: store, Tmux: tmux.New(&safetyRunner{identityWindow: "@1"})}
	first, _ = store.FindSession(first.ID)
	if _, committed, err := store.CollapseSessionIntoShelf(first.ID, first, state.CollapsedShelf{
		ChatID: 100, MessageID: 88, Pinned: true, PinKnown: true,
	}); err != nil || !committed {
		t.Fatalf("prepare first collapse committed=%v err=%v", committed, err)
	}
	if _, retired, err := store.FinishCollapsedAnchorRetirement(first.ID, 100, 77); err != nil || !retired {
		t.Fatalf("prepare first retirement retired=%v err=%v", retired, err)
	}
	second, _ = store.FindSession(second.ID)

	movedDir := stateDir + ".unavailable"
	if err := os.Rename(stateDir, movedDir); err != nil {
		t.Fatal(err)
	}
	moved := true
	t.Cleanup(func() {
		if moved {
			_ = os.Rename(movedDir, stateDir)
		}
	})
	var edits []string
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/botTOKEN/editMessageText" {
			return nil, errors.New("unexpected Telegram endpoint " + req.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		edits = append(edits, body["text"].(string))
		return telegramTestResponse(t, http.StatusOK, map[string]any{
			"ok": true, "result": map[string]any{"message_id": 88, "chat": map[string]any{"id": 100}},
		}), nil
	})}
	app.Telegram = client
	result := app.collapseAnchor(context.Background(), second)
	if moved {
		if err := os.Rename(movedDir, stateDir); err != nil {
			t.Fatal(err)
		}
		moved = false
	}
	current, _ := store.FindSession(second.ID)
	if result.OK() || current.Collapsed || len(edits) != 0 {
		t.Fatalf("collapse=%#v session=%#v edits=%#v", result, current, edits)
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
			session.LastActivityAt = time.Unix(int64(10-target.id), 0).UTC()
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, committed, err := app.Store.CollapseSessionIntoShelf(target.id, session, state.CollapsedShelf{ChatID: 100, MessageID: 88}); err != nil || !committed {
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
	if !first.Collapsed || first.AnchorMessageID != 0 || remaining.Collapsed || remaining.AnchorMessageID != 89 || shelf == nil || shelf.MessageID != 88 {
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
	if !strings.Contains(rendered, "+68 more") {
		t.Fatalf("shelf did not enforce its phone-sized entry cap: %q", rendered)
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

func TestCollapsedShelfMatchesSessionOrderAndMakesLossExplicit(t *testing.T) {
	app := &App{}
	now := time.Date(2026, 7, 23, 4, 0, 0, 0, time.UTC)
	rendered := app.renderCollapsedShelf([]state.TerminalSession{
		{ID: 1, Title: "older", State: state.TerminalRunning, Collapsed: true, LastActivityAt: now.Add(-time.Minute), LastSummary: "old"},
		{ID: 2, Title: "recent", State: state.TerminalRunning, Collapsed: true, LastActivityAt: now, LastSummary: "recent"},
		{ID: 3, Title: "lost", State: state.TerminalLost, Collapsed: true, UpdatedAt: now, LastSummary: "checks passed"},
	})
	lost := strings.Index(rendered, "[3]")
	recent := strings.Index(rendered, "[2]")
	older := strings.Index(rendered, "[1]")
	if lost < 0 || recent < 0 || older < 0 || !(lost < recent && recent < older) {
		t.Fatalf("shelf order does not match /sessions: %q", rendered)
	}
	if !strings.Contains(rendered, "[3] lost · lost - tap ➕ Show for recovery controls") ||
		strings.Contains(rendered, "/sessions") || strings.Contains(rendered, "[3] lost · checks passed") {
		t.Fatalf("lost shelf entry retained stale success prose: %q", rendered)
	}
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
