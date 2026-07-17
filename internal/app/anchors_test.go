package app

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
)

func TestReconcileAnchorControlsAddsAvailableVoiceWithoutRendering(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.AllocateSession("main", "@1", "%1", "shell")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.UpdateSession(session.ID, func(s *state.TerminalSession) {
		s.AnchorChatID = 100
		s.AnchorMessageID = 77
		s.AnchorFormat = "snapshot"
		s.WatchEnabled = true
	}); err != nil {
		t.Fatal(err)
	}
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/botTOKEN/editMessageReplyMarkup" {
			t.Fatalf("unexpected path %s", req.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		markup := string(mustJSON(body["reply_markup"]))
		if !strings.Contains(markup, "voice:1") || strings.Contains(markup, "snapshot:1") {
			t.Fatalf("controls = %s", markup)
		}
		return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": map[string]any{"message_id": 77, "chat": map[string]any{"id": 100}}}), nil
	})}
	app := &App{Store: store, Telegram: client, mode: "snapshot", guideAvailable: true}
	app.reconcileAnchorControls(context.Background(), session.ID)
}

func TestAnchorMarkupUsesActualAnchorFormat(t *testing.T) {
	app := &App{mode: "snapshot", snapshotReady: true, guideAvailable: true}
	for _, test := range []struct {
		name       string
		format     string
		wantAction string
		reject     string
	}{
		{name: "text offers image", format: "text", wantAction: "snapshot:1", reject: "voice:1"},
		{name: "guided media offers image", format: anchorFormatGuideEvidence, wantAction: "snapshot:1", reject: "voice:1"},
		{name: "snapshot offers voice", format: "snapshot", wantAction: "voice:1", reject: "snapshot:1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			markup := string(mustJSON(app.anchorMarkup(state.TerminalSession{ID: 1, State: state.TerminalRunning, AnchorFormat: test.format})))
			if !strings.Contains(markup, test.wantAction) || strings.Contains(markup, test.reject) {
				t.Fatalf("markup for %s anchor = %s", test.format, markup)
			}
		})
	}
}

func TestAnchorPinReconciliationTracksWatchLifecycle(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.AllocateSession("main", "@1", "%1", "shell")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.UpdateSession(session.ID, func(s *state.TerminalSession) {
		s.AnchorChatID = 100
		s.AnchorMessageID = 77
		s.WatchEnabled = true
	}); err != nil {
		t.Fatal(err)
	}
	var paths []string
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.Path)
		return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": true}), nil
	})}
	app := &App{Store: store, Telegram: client}
	app.reconcileAnchorPresentation(context.Background(), session.ID)
	pinned, _ := store.FindSession(session.ID)
	if !pinned.AnchorPinKnown || !pinned.AnchorPinned {
		t.Fatalf("active anchor was not pinned: %#v", pinned)
	}
	if _, _, err := store.UpdateSession(session.ID, func(s *state.TerminalSession) { s.WatchEnabled = false }); err != nil {
		t.Fatal(err)
	}
	app.reconcileAnchorPresentation(context.Background(), session.ID)
	unpinned, _ := store.FindSession(session.ID)
	if !unpinned.AnchorPinKnown || unpinned.AnchorPinned {
		t.Fatalf("inactive anchor was not unpinned: %#v", unpinned)
	}
	want := []string{"/botTOKEN/pinChatMessage", "/botTOKEN/unpinChatMessage"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %#v, want %#v", paths, want)
	}
}

func TestAnchorRetirementRetriesAfterUnpinFailureAndAcceptsNotModified(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.AllocateSession("main", "@1", "%1", "shell")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.UpdateSession(session.ID, func(s *state.TerminalSession) {
		s.AnchorChatID = 100
		s.AnchorMessageID = 88
		s.AnchorFormat = "snapshot"
		s.RetiringAnchorMessageID = 77
		s.RetiringAnchorFormat = "snapshot"
		s.AnchorPinKnown = true
		s.AnchorPinned = true
		s.WatchEnabled = true
	}); err != nil {
		t.Fatal(err)
	}
	captionEdits := 0
	unpins := 0
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/botTOKEN/editMessageCaption":
			captionEdits++
			if captionEdits == 1 {
				return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": map[string]any{"message_id": 77, "chat": map[string]any{"id": 100}}}), nil
			}
			return telegramTestResponse(t, http.StatusBadRequest, map[string]any{"ok": false, "error_code": 400, "description": "Bad Request: message is not modified"}), nil
		case "/botTOKEN/unpinChatMessage":
			unpins++
			if unpins == 1 {
				return telegramTestResponse(t, http.StatusInternalServerError, map[string]any{"ok": false, "error_code": 500, "description": "injected unpin failure"}), nil
			}
			return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": true}), nil
		default:
			t.Fatalf("unexpected path %s", req.URL.Path)
			return nil, nil
		}
	})}
	app := &App{Store: store, Telegram: client}

	app.finishAnchorRotationLocked(context.Background(), session.ID)
	pending, _ := store.FindSession(session.ID)
	if pending.RetiringAnchorMessageID != 77 {
		t.Fatalf("retirement was not retained after unpin failure: %#v", pending)
	}
	app.finishAnchorRotationLocked(context.Background(), session.ID)
	if captionEdits != 1 || unpins != 1 {
		t.Fatalf("retirement ignored retry backoff: edits=%d unpins=%d", captionEdits, unpins)
	}
	if _, _, err := store.UpdateSession(session.ID, func(s *state.TerminalSession) {
		s.RetiringAnchorRetryAt = time.Now().Add(-time.Second)
	}); err != nil {
		t.Fatal(err)
	}
	app.finishAnchorRotationLocked(context.Background(), session.ID)
	retired, _ := store.FindSession(session.ID)
	if retired.RetiringAnchorMessageID != 0 || !reflect.DeepEqual(retired.StaleAlternateMessageIDs, []int{77}) || captionEdits != 2 || unpins != 2 {
		t.Fatalf("retry retirement = %#v edits=%d unpins=%d", retired, captionEdits, unpins)
	}
}

func TestUneditableTextAnchorReplacementRecordsStaleAndUnpins(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.AllocateSession("main", "@1", "%1", "shell")
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
	var paths []string
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.Path)
		switch req.URL.Path {
		case "/botTOKEN/editMessageText":
			return telegramTestResponse(t, http.StatusBadRequest, map[string]any{"ok": false, "error_code": 400, "description": "Bad Request: message can't be edited"}), nil
		case "/botTOKEN/sendMessage":
			return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": map[string]any{"message_id": 88, "chat": map[string]any{"id": 100}}}), nil
		case "/botTOKEN/pinChatMessage", "/botTOKEN/unpinChatMessage":
			return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": true}), nil
		default:
			t.Fatalf("unexpected path %s", req.URL.Path)
			return nil, nil
		}
	})}
	app := &App{Config: config.Config{TelegramChatID: 100}, Store: store, Telegram: client}

	app.updateAnchorLocal(context.Background(), session.ID, "ready", true)
	got, ok := store.FindSession(session.ID)
	if !ok || got.AnchorMessageID != 88 || got.RetiringAnchorMessageID != 0 || !reflect.DeepEqual(got.StaleAlternateMessageIDs, []int{77}) {
		t.Fatalf("replacement session = %#v ok=%v", got, ok)
	}
	if routed, targetState, ok := store.FindReplyTarget(100, 77); !ok || targetState != state.ReplyTargetStale || routed.ID != session.ID {
		t.Fatalf("predecessor reply target = %#v %q ok=%v", routed, targetState, ok)
	}
	want := []string{"/botTOKEN/editMessageText", "/botTOKEN/sendMessage", "/botTOKEN/pinChatMessage", "/botTOKEN/editMessageText", "/botTOKEN/unpinChatMessage"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %#v, want %#v", paths, want)
	}
}
