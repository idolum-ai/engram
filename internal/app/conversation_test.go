package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/idolum-ai/engram/internal/anthropic"
	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
	"github.com/idolum-ai/engram/internal/tmux"
)

func TestConversationUsesSnapshotFrameAndRepliesToCanonicalAnchor(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.AllocateSession("main", "@1", "%1", "build")
	if err != nil {
		t.Fatal(err)
	}
	session = bindTestSession(t, store, session.ID)
	session, _, err = store.UpdateSession(session.ID, func(s *state.TerminalSession) {
		s.AnchorChatID = 100
		s.AnchorMessageID = 77
		s.AnchorFormat = "snapshot"
		s.WatchEnabled = true
	})
	if err != nil {
		t.Fatal(err)
	}

	var modelPrompt string
	model := anthropic.New("secret", "claude-haiku-4-5-20251001")
	model.BaseURL = "https://anthropic.test/messages"
	model.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		modelPrompt = body.Messages[0].Content
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"stop_reason":"end_turn","content":[{"type":"text","text":"The build is green and waiting at the prompt."}]}`))}, nil
	})}

	var telegramBody map[string]any
	tg := telegram.New("TOKEN")
	tg.BaseURL = "https://telegram.test/botTOKEN"
	tg.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(req.Body).Decode(&telegramBody); err != nil {
			t.Fatal(err)
		}
		return snapshotJSONResponse(`{"message_id":88,"chat":{"id":100}}`), nil
	})}

	app := &App{
		Store:          store,
		Telegram:       tg,
		Guide:          model,
		Tmux:           tmux.New(snapshotTmuxRunner{}),
		mode:           "snapshot",
		guideAvailable: true,
	}
	app.sendConversation(context.Background(), session)
	if !strings.Contains(modelPrompt, "green") || strings.Count(modelPrompt, "green") != 64 {
		t.Fatalf("model did not receive the 64-row snapshot frame: %q", modelPrompt)
	}
	if telegramReplyMessageID(telegramBody) != 77 || !strings.Contains(telegramBody["text"].(string), "build is green") {
		t.Fatalf("Telegram body = %#v", telegramBody)
	}
	updated, _ := store.FindSession(session.ID)
	if updated.SummaryMessageID != 88 {
		t.Fatalf("summary reply alias = %d, want 88", updated.SummaryMessageID)
	}
	if _, ok := app.conversationEpochs[session.ID]; ok {
		t.Fatal("one-off voice rendering mutated canonical conversation continuity")
	}
}

func TestConversationOmitsUpstreamRecordFromModelInput(t *testing.T) {
	store, session := conversationTestSession(t, "snapshot")
	var modelPrompt string
	model := anthropic.New("secret", "claude-haiku-4-5-20251001")
	model.BaseURL = "https://anthropic.test/messages"
	model.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		modelPrompt = body.Messages[0].Content
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"stop_reason":"end_turn","content":[{"type":"text","text":"The ordinary output is visible."}]}`))}, nil
	})}
	tg := telegram.New("TOKEN")
	tg.BaseURL = "https://telegram.test/botTOKEN"
	tg.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return snapshotJSONResponse(`{"message_id":88,"chat":{"id":100}}`), nil
	})}
	a := &App{Store: store, Telegram: tg, Guide: model, Tmux: tmux.New(conversationSignalRunner{}), mode: "snapshot", guideAvailable: true}
	a.sendConversation(context.Background(), session)
	if !strings.Contains(modelPrompt, "ordinary output") || strings.Contains(modelPrompt, "engram:upstream") || strings.Contains(modelPrompt, "secret signal payload") {
		t.Fatalf("model prompt retained upstream framing or payload: %q", modelPrompt)
	}
}

func TestQueuedConversationStopsWhenSessionCollapsesAfterCapture(t *testing.T) {
	store, session := conversationTestSession(t, anchorFormatSnapshot)
	guide := &recordingShelfGuide{}
	runner := &collapseAfterCaptureRunner{collapse: func() {
		if _, _, err := store.UpdateSession(session.ID, func(current *state.TerminalSession) {
			current.Collapsed = true
		}); err != nil {
			t.Error(err)
		}
	}}
	app := &App{
		Store: store, Guide: guide, Tmux: tmux.New(runner),
		mode: "snapshot", guideAvailable: true,
	}
	app.sendConversation(context.Background(), session)
	if guide.calls != 0 {
		t.Fatalf("collapsed queued Talk made %d model calls", guide.calls)
	}
}

type conversationSignalRunner struct{}

func (conversationSignalRunner) Run(ctx context.Context, args ...string) (string, error) {
	if len(args) > 0 && args[0] == "capture-pane" {
		return framedStyledCaptureMetadata("bash"), nil
	}
	if len(args) > 0 && args[0] == "show-buffer" {
		capture := "ordinary output\n[engram:upstream] " + firstSignalID + " secret signal payload\n"
		return pairedCaptureResult(args, capture, capture), nil
	}
	return (snapshotTmuxRunner{}).Run(ctx, args...)
}

func TestConversationReportsModelFailureToTheAnchor(t *testing.T) {
	store, session := conversationTestSession(t, "snapshot")
	model := anthropic.New("secret", "claude-haiku-4-5-20251001")
	model.BaseURL = "https://anthropic.test/messages"
	model.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusInternalServerError, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"message":"unavailable"}}`))}, nil
	})}

	var notice map[string]any
	tg := telegram.New("TOKEN")
	tg.BaseURL = "https://telegram.test/botTOKEN"
	tg.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(req.Body).Decode(&notice); err != nil {
			t.Fatal(err)
		}
		return snapshotJSONResponse(`{"message_id":89,"chat":{"id":100}}`), nil
	})}

	app := &App{Store: store, Telegram: tg, Guide: model, Tmux: tmux.New(snapshotTmuxRunner{}), mode: "snapshot", guideAvailable: true}
	app.sendConversation(context.Background(), session)
	if telegramReplyMessageID(notice) != 77 || !strings.Contains(notice["text"].(string), "couldn't finish") {
		t.Fatalf("failure notice = %#v", notice)
	}
}

func TestConversationReportsSupersededAnchorPolitely(t *testing.T) {
	store, session := conversationTestSession(t, "snapshot")
	model := anthropic.New("secret", "claude-haiku-4-5-20251001")
	model.BaseURL = "https://anthropic.test/messages"
	model.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(*http.Request) (*http.Response, error) {
		if _, _, err := store.UpdateSession(session.ID, func(s *state.TerminalSession) {
			s.AnchorMessageID = 78
			s.AnchorFormat = "text"
		}); err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"stop_reason":"end_turn","content":[{"type":"text","text":"done"}]}`))}, nil
	})}

	var notice map[string]any
	tg := telegram.New("TOKEN")
	tg.BaseURL = "https://telegram.test/botTOKEN"
	tg.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(req.Body).Decode(&notice); err != nil {
			t.Fatal(err)
		}
		return snapshotJSONResponse(`{"message_id":90,"chat":{"id":100}}`), nil
	})}

	app := &App{Store: store, Telegram: tg, Guide: model, Tmux: tmux.New(snapshotTmuxRunner{}), mode: "snapshot", guideAvailable: true}
	app.sendConversation(context.Background(), session)
	if telegramReplyMessageID(notice) != 78 || !strings.Contains(notice["text"].(string), "newer view") {
		t.Fatalf("superseded notice = %#v", notice)
	}
}

func TestConversationDeletesReplyWhenBindingChangesDuringTelegramSend(t *testing.T) {
	store, session := conversationTestSession(t, "snapshot")
	model := anthropic.New("secret", "claude-haiku-4-5-20251001")
	model.BaseURL = "https://anthropic.test/messages"
	model.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"stop_reason":"end_turn","content":[{"type":"text","text":"The build passed."}]}`))}, nil
	})}
	var paths []string
	tg := telegram.New("TOKEN")
	tg.BaseURL = "https://telegram.test/botTOKEN"
	tg.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.Path)
		if strings.HasSuffix(req.URL.Path, "/sendMessage") {
			if _, _, err := store.UpdateSession(session.ID, func(current *state.TerminalSession) {
				current.TmuxServerID = "abcdef0123456789abcdef0123456789"
			}); err != nil {
				t.Fatal(err)
			}
			return snapshotJSONResponse(`{"message_id":88,"chat":{"id":100}}`), nil
		}
		return snapshotJSONResponse(`true`), nil
	})}
	a := &App{Store: store, Telegram: tg, Guide: model, Tmux: tmux.New(snapshotTmuxRunner{}), mode: "snapshot", guideAvailable: true}
	a.sendConversation(context.Background(), session)
	got, _ := store.FindSession(session.ID)
	if len(paths) != 2 || !strings.HasSuffix(paths[0], "/sendMessage") || !strings.HasSuffix(paths[1], "/deleteMessage") || got.SummaryMessageID != 0 {
		t.Fatalf("paths=%#v session=%#v", paths, got)
	}
}

func TestVoiceCallbackRejectsTextAnchor(t *testing.T) {
	store, session := conversationTestSession(t, "text")
	var answer map[string]any
	tg := telegram.New("TOKEN")
	tg.BaseURL = "https://telegram.test/botTOKEN"
	tg.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(req.Body).Decode(&answer); err != nil {
			t.Fatal(err)
		}
		return snapshotJSONResponse(`true`), nil
	})}
	app := &App{
		Config: config.Config{TelegramAllowedUserID: 42, TelegramChatID: 100},
		Store:  store, Telegram: tg, mode: "snapshot", guideAvailable: true,
	}
	status := app.handleCallback(context.Background(), telegram.CallbackQuery{
		ID: "voice", From: telegram.User{ID: 42}, Data: "voice:1",
		Message: &telegram.Message{MessageID: session.AnchorMessageID, Chat: telegram.Chat{ID: session.AnchorChatID}},
	})
	if status != "callback_user_error" || answer["text"] != "voice is unavailable" {
		t.Fatalf("status = %q, answer = %#v", status, answer)
	}
}

func conversationTestSession(t *testing.T, format string) (*state.Store, state.TerminalSession) {
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
	session = bindTestSession(t, store, session.ID)
	session, _, err = store.UpdateSession(session.ID, func(s *state.TerminalSession) {
		s.AnchorChatID = 100
		s.AnchorMessageID = 77
		s.AnchorFormat = format
		s.WatchEnabled = true
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, session
}
