package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/guide"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
)

func TestCollapseAndExpandRotateOneCanonicalAnchor(t *testing.T) {
	for _, test := range []struct {
		name             string
		format           string
		collapsedID      int
		collapsePaths    []string
		predecessorStale bool
	}{
		{name: "text", format: anchorFormatText, collapsedID: 77, collapsePaths: []string{"/botTOKEN/answerCallbackQuery", "/botTOKEN/editMessageText"}},
		{name: "media", format: anchorFormatSnapshot, collapsedID: 88, predecessorStale: true, collapsePaths: []string{"/botTOKEN/answerCallbackQuery", "/botTOKEN/sendMessage", "/botTOKEN/editMessageReplyMarkup", "/botTOKEN/pinChatMessage", "/botTOKEN/deleteMessage"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
			if _, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
				session.AnchorChatID = 100
				session.AnchorMessageID = 77
				session.AnchorFormat = test.format
				session.AnchorPinned = true
				session.AnchorPinKnown = true
				session.LastSummary = "Tests passed. Waiting for review."
			}); err != nil {
				t.Fatal(err)
			}
			var paths []string
			var bodies []map[string]any
			client := telegram.New("TOKEN")
			client.BaseURL = "https://api.telegram.org/botTOKEN"
			client.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				paths = append(paths, req.URL.Path)
				var body map[string]any
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					return nil, err
				}
				bodies = append(bodies, body)
				switch req.URL.Path {
				case "/botTOKEN/answerCallbackQuery", "/botTOKEN/pinChatMessage", "/botTOKEN/unpinChatMessage", "/botTOKEN/deleteMessage":
					return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": true}), nil
				case "/botTOKEN/sendMessage":
					return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": map[string]any{"message_id": 88, "chat": map[string]any{"id": 100}}}), nil
				case "/botTOKEN/editMessageText", "/botTOKEN/editMessageReplyMarkup":
					return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": map[string]any{"message_id": body["message_id"], "chat": map[string]any{"id": 100}}}), nil
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

			status := app.handleCallback(context.Background(), telegram.CallbackQuery{
				ID: "collapse", From: telegram.User{ID: 42}, Data: "collapse:" + strconv.Itoa(id),
				Message: &telegram.Message{MessageID: 77, Chat: telegram.Chat{ID: 100}},
			})
			if status != "callback_ok" {
				t.Fatalf("collapse status = %q", status)
			}
			current, ok := app.Store.FindSession(id)
			if !ok || !current.Collapsed || current.AnchorMessageID != test.collapsedID || current.AnchorFormat != anchorFormatText || current.RetiringAnchorMessageID != 0 || !current.AnchorPinned {
				t.Fatalf("collapsed session = %#v, ok=%v", current, ok)
			}
			_, target, routed := app.Store.FindReplyTarget(100, 77)
			if test.predecessorStale && (!routed || target != state.ReplyTargetStale) || !test.predecessorStale && (!routed || target != state.ReplyTargetCurrent) {
				t.Fatalf("predecessor route = %q, ok=%v", target, routed)
			}
			if !reflect.DeepEqual(paths, test.collapsePaths) {
				t.Fatalf("collapse endpoints = %#v, want %#v", paths, test.collapsePaths)
			}
			if test.format == anchorFormatText {
				if !strings.Contains(marshalTestJSON(t, bodies[1]["reply_markup"]), "expand:"+strconv.Itoa(id)) {
					t.Fatalf("collapsed edit payload = %#v", bodies[1])
				}
			} else if bodies[1]["disable_notification"] != true || bodies[1]["reply_parameters"] != nil || bodies[1]["reply_markup"] != nil || !strings.Contains(marshalTestJSON(t, bodies[2]["reply_markup"]), "expand:"+strconv.Itoa(id)) {
				t.Fatalf("collapsed prospective payloads = %#v", bodies)
			}

			paths = nil
			bodies = nil
			status = app.handleCallback(context.Background(), telegram.CallbackQuery{
				ID: "expand", From: telegram.User{ID: 42}, Data: "expand:" + strconv.Itoa(id),
				Message: &telegram.Message{MessageID: test.collapsedID, Chat: telegram.Chat{ID: 100}},
			})
			if status != "callback_ok" {
				t.Fatalf("expand status = %q", status)
			}
			current, _ = app.Store.FindSession(id)
			if current.Collapsed || current.AnchorMessageID != test.collapsedID {
				t.Fatalf("expanded session = %#v", current)
			}
			if !reflect.DeepEqual(paths, []string{"/botTOKEN/answerCallbackQuery", "/botTOKEN/editMessageReplyMarkup"}) || !strings.Contains(marshalTestJSON(t, bodies[1]["reply_markup"]), "collapse:"+strconv.Itoa(id)) {
				t.Fatalf("expand requests paths=%#v bodies=%#v", paths, bodies)
			}
		})
	}
}

func TestCollapsedRefreshUsesCompactGuideWithoutChromiumOrReferences(t *testing.T) {
	app, runner, id := newSafetyApp(t, state.TerminalOriginCreated)
	runner.capturePhysical = "go test ./...\nok example/internal/app\n/tmp/result.txt\n"
	runner.captureJoined = runner.capturePhysical
	if _, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.AnchorChatID = 100
		session.AnchorMessageID = 77
		session.AnchorFormat = anchorFormatText
		session.Collapsed = true
	}); err != nil {
		t.Fatal(err)
	}
	guideRenderer := &recordingCompactGuide{text: "Tests passed and the application package is healthy."}
	app.Guide = guideRenderer
	app.guideAvailable = true
	renderer := &failingSnapshotRenderer{}
	app.Snapshots = renderer
	app.snapshotReady = true
	app.captureSlots = make(chan struct{}, 1)
	app.guideSlots = make(chan struct{}, 1)
	app.Config.TelegramChatID = 100
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
		text, _ := body["text"].(string)
		if strings.Contains(text, "\n") || strings.Contains(text, "result.txt") || !strings.Contains(text, "Tests passed") || !strings.Contains(marshalTestJSON(t, body["reply_markup"]), "expand:"+strconv.Itoa(id)) {
			return nil, errors.New("collapsed card was not a bounded one-line guide: " + text)
		}
		return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": map[string]any{"message_id": 77, "chat": map[string]any{"id": 100}}}), nil
	})}
	app.Telegram = client

	app.refreshSession(context.Background(), id, true)
	if guideRenderer.calls != 1 || !guideRenderer.input.Compact || guideRenderer.input.EvidenceRequested {
		t.Fatalf("compact guide calls=%d input=%#v", guideRenderer.calls, guideRenderer.input)
	}
	if renderer.renders != 0 {
		t.Fatalf("collapsed refresh invoked Chromium %d times", renderer.renders)
	}
	current, _ := app.Store.FindSession(id)
	if current.LastSummary != guideRenderer.text || current.LastRawCaptureHash == "" || !current.Collapsed {
		t.Fatalf("collapsed refresh state = %#v", current)
	}
}

func TestCollapsedGuideFailurePersistsHonestFallback(t *testing.T) {
	app, runner, id := newSafetyApp(t, state.TerminalOriginCreated)
	runner.capturePhysical = "shell output\n"
	runner.captureJoined = runner.capturePhysical
	if _, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.AnchorChatID = 100
		session.AnchorMessageID = 77
		session.AnchorFormat = anchorFormatText
		session.Collapsed = true
		session.LastSummary = "Tests passed."
	}); err != nil {
		t.Fatal(err)
	}
	guideRenderer := &recordingCompactGuide{err: errors.New("provider unavailable")}
	app.Guide = guideRenderer
	app.guideAvailable = true
	app.captureSlots = make(chan struct{}, 1)
	app.guideSlots = make(chan struct{}, 1)
	app.Config.TelegramChatID = 100
	edits := 0
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/botTOKEN/editMessageText" {
			return nil, errors.New("unexpected Telegram endpoint " + req.URL.Path)
		}
		edits++
		return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": map[string]any{"message_id": 77, "chat": map[string]any{"id": 100}}}), nil
	})}
	app.Telegram = client

	app.refreshSession(context.Background(), id, true)
	app.refreshSession(context.Background(), id, false)
	current, _ := app.Store.FindSession(id)
	if guideRenderer.calls != 1 || edits != 1 || current.LastSummary != "bash in /tmp" {
		t.Fatalf("calls=%d edits=%d summary=%q", guideRenderer.calls, edits, current.LastSummary)
	}
}

func TestCollapsedLostSessionReplacesStaleSuccessSummary(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	if _, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.AnchorChatID = 100
		session.AnchorMessageID = 77
		session.AnchorFormat = anchorFormatText
		session.Collapsed = true
		session.State = state.TerminalLost
		session.WatchEnabled = false
		session.LastSummary = "Tests passed."
	}); err != nil {
		t.Fatal(err)
	}
	app.Config.TelegramChatID = 100
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: safetyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		text, _ := body["text"].(string)
		if strings.Contains(text, "Tests passed") || !strings.Contains(text, "no longer matches") {
			return nil, errors.New("collapsed lost card retained stale summary: " + text)
		}
		return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": map[string]any{"message_id": 77, "chat": map[string]any{"id": 100}}}), nil
	})}
	app.Telegram = client

	app.updateAnchorLocal(context.Background(), id, "The tracked tmux pane no longer matches this session.", true)
}

func TestCollapsedRefreshFallsBackWithoutGuide(t *testing.T) {
	app, runner, id := newSafetyApp(t, state.TerminalOriginCreated)
	runner.capturePhysical = "shell output\n"
	runner.captureJoined = runner.capturePhysical
	if _, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.AnchorChatID = 100
		session.AnchorMessageID = 77
		session.AnchorFormat = anchorFormatText
		session.Collapsed = true
	}); err != nil {
		t.Fatal(err)
	}
	app.mode = config.AnchorModeSnapshot
	app.captureSlots = make(chan struct{}, 1)
	app.Config.TelegramChatID = 100
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
		text, _ := body["text"].(string)
		if strings.Contains(text, "\n") || !strings.Contains(text, "bash in /tmp") {
			return nil, errors.New("deterministic compact fallback missing: " + text)
		}
		return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": map[string]any{"message_id": 77, "chat": map[string]any{"id": 100}}}), nil
	})}
	app.Telegram = client

	app.refreshSession(context.Background(), id, true)
	current, _ := app.Store.FindSession(id)
	if current.LastSummary != "bash in /tmp" || !current.Collapsed {
		t.Fatalf("fallback state = %#v", current)
	}
}

func TestCollapsedAnchorIsOneBoundedUTF8Line(t *testing.T) {
	app := &App{}
	line := app.renderCollapsedAnchor(state.TerminalSession{
		ID: 7, Title: strings.Repeat("界", 80), State: state.TerminalRunning,
	}, strings.Repeat("終了した処理 ", 80))
	if !utf8.ValidString(line) || len(line) > maxCollapsedAnchorBytes || strings.ContainsAny(line, "\r\n") {
		t.Fatalf("collapsed line bytes=%d valid=%v text=%q", len(line), utf8.ValidString(line), line)
	}
}

func TestPresentationReconcileDoesNotDuplicateRunningRefresh(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	if _, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.AnchorChatID = 100
		session.AnchorMessageID = 77
		session.AnchorFormat = anchorFormatText
		session.AnchorPinned = true
		session.AnchorPinKnown = true
	}); err != nil {
		t.Fatal(err)
	}
	app.snapshotReady = true
	app.summaryMu.Lock()
	app.ensureSummaryQueuesLocked()
	app.summaryRunning[id] = true
	app.summaryMu.Unlock()

	app.reconcileAnchorPresentation(context.Background(), id)

	app.summaryMu.Lock()
	queued := app.summaryQueued[id]
	app.summaryMu.Unlock()
	if queued {
		t.Fatal("presentation reconcile queued a duplicate refresh")
	}
}

type recordingCompactGuide struct {
	input guide.Input
	text  string
	err   error
	calls int
}

func (g *recordingCompactGuide) Converse(_ context.Context, input guide.Input) (string, error) {
	g.calls++
	g.input = input
	return g.text, g.err
}

func marshalTestJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
