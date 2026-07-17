package app

import (
	"context"
	"encoding/json"
	"errors"
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
	if _, _, err := a.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.AnchorChatID = 100
		session.AnchorMessageID = 77
	}); err != nil {
		t.Fatal(err)
	}
	telegramClient := telegram.New("TOKEN")
	telegramClient.BaseURL = "https://api.telegram.org/botTOKEN"
	telegramClient.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": map[string]any{"message_id": 77, "chat": map[string]any{"id": 100}}}), nil
	})}
	a.Telegram = telegramClient
	runner.capturePhysical = "ordinary output\n[engram:upstream] " + firstSignalID + " build finished\n"
	runner.captureJoined = runner.capturePhysical
	model := anthropic.New("secret", "claude-haiku-4-5-20251001")
	model.BaseURL = "https://anthropic.test/messages"
	model.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"stop_reason":"end_turn","content":[{"type":"text","text":"Ordinary output is visible."}]}`))}, nil
	})}
	a.Guide = model

	a.refreshSession(context.Background(), id, true)
	got, _ := a.Store.FindSession(id)
	wantHash := guideCaptureHash("ordinary output", "shell", tmux.StyledCapture{ANSI: runner.capturePhysical, Title: "build pane", CurrentPath: "/tmp", CurrentCmd: "bash", Columns: 71, VisibleRows: 37, AlternateOn: "1", PaneInMode: "0"})
	if got.LastRawCapture != "ordinary output" || got.LastRawCaptureHash != wantHash {
		t.Fatalf("capture=%q hash=%q want=%q", got.LastRawCapture, got.LastRawCaptureHash, wantHash)
	}
}

func TestUnchangedGuideEvidenceRefreshPreservesCanonicalCard(t *testing.T) {
	a, runner, id := newSafetyApp(t, state.TerminalOriginCreated)
	runner.capturePhysical = "ordinary output\n"
	runner.captureJoined = runner.capturePhysical
	wantHash := guideCaptureHash("ordinary output", "shell", tmux.StyledCapture{ANSI: runner.capturePhysical, Title: "build pane", CurrentPath: "/tmp", CurrentCmd: "bash", Columns: 71, VisibleRows: 37, AlternateOn: "1", PaneInMode: "0"})
	if _, _, err := a.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.AnchorChatID = 100
		session.AnchorMessageID = 77
		session.AnchorFormat = anchorFormatGuideEvidence
		session.LastKnownCWD = "/tmp"
		session.LastRawCaptureHash = wantHash
		session.LastSummary = "Ordinary output is visible."
		session.LastAnchorEditAt = time.Now().Add(-time.Minute)
	}); err != nil {
		t.Fatal(err)
	}
	telegramCalls := 0
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(*http.Request) (*http.Response, error) {
		telegramCalls++
		return nil, errors.New("unchanged card should not be edited")
	})}
	a.Telegram = client
	a.snapshotReady = true
	a.mode = config.AnchorModeGuide
	current, _ := a.Store.FindSession(id)
	a.rememberAnchorTextFrame(current, "ordinary output", "coherent-crop")
	caption, _ := a.guidedEvidenceCaption(current, current.LastSummary, visibleReferences{})
	wantRenderHash := sha(caption + "\x00coherent-crop")
	if _, _, err := a.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.LastRenderHash = wantRenderHash
	}); err != nil {
		t.Fatal(err)
	}

	a.refreshSession(context.Background(), id, false)
	got, _ := a.Store.FindSession(id)
	if telegramCalls != 0 || got.LastRenderHash != wantRenderHash {
		t.Fatalf("unchanged refresh degraded card: calls=%d session=%#v", telegramCalls, got)
	}
}

func TestUnchangedGuideEvidenceRefreshRestoresCompanionAfterRestart(t *testing.T) {
	a, runner, id := newSafetyApp(t, state.TerminalOriginCreated)
	runner.capturePhysical = "ordinary output\n"
	runner.captureJoined = runner.capturePhysical
	wantHash := guideCaptureHash("ordinary output", "shell", tmux.StyledCapture{ANSI: runner.capturePhysical, Title: "build pane", CurrentPath: "/tmp", CurrentCmd: "bash", Columns: 71, VisibleRows: 37, AlternateOn: "1", PaneInMode: "0"})
	if _, _, err := a.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.AnchorChatID = 100
		session.AnchorMessageID = 77
		session.AnchorFormat = anchorFormatGuideEvidence
		session.LastKnownCWD = "/tmp"
		session.LastRawCaptureHash = wantHash
		session.LastSummary = "Ordinary output is visible."
		session.LastAnchorEditAt = time.Now().Add(-time.Minute)
	}); err != nil {
		t.Fatal(err)
	}
	modelCalls := 0
	model := anthropic.New("secret", "claude-haiku-4-5-20251001")
	model.BaseURL = "https://anthropic.test/messages"
	model.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(*http.Request) (*http.Response, error) {
		modelCalls++
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"stop_reason":"end_turn","content":[{"type":"text","text":"Ordinary output is visible."}]}`))}, nil
	})}
	a.Guide = model
	renderer := &fakeSnapshotRenderer{}
	a.Snapshots = renderer
	a.Config.Home = t.TempDir()
	a.snapshotReady = true
	a.mode = config.AnchorModeGuide
	a.renderSlots = make(chan struct{}, 1)
	persisted, _ := a.Store.FindSession(id)
	capture := tmux.StyledCapture{
		ANSI: runner.capturePhysical, Text: "ordinary output", JoinedText: "ordinary output",
		ServerID: appTestServerID, WindowID: "@1", PaneID: "%1", CurrentCmd: "bash",
		AlternateOn: "1", PaneInMode: "0", Columns: 71, VisibleRows: 37, BufferRows: 1,
		Title: "build pane", CurrentPath: "/tmp",
	}
	crop := a.selectGuidedEvidenceCrop(persisted, capture, conversationFrame{}, "ordinary output", nil)
	caption, _ := a.guidedEvidenceCaption(persisted, persisted.LastSummary, visibleReferences{})
	if _, _, err := a.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.LastRenderHash = sha(caption + "\x00" + crop.hash)
	}); err != nil {
		t.Fatal(err)
	}
	telegramCalls := 0
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		telegramCalls++
		return nil, errors.New("coherent persisted card should not be edited: " + request.URL.Path)
	})}
	a.Telegram = client

	a.refreshSession(context.Background(), id, false)

	got, _ := a.Store.FindSession(id)
	frame, frameOK := a.snapshotTextFrame(got)
	if modelCalls != 1 || renderer.renders != 1 || telegramCalls != 0 || !frameOK || frame.JoinedText != "ordinary output" {
		t.Fatalf("companion recovery: model=%d renders=%d telegram=%d frame=%#v ok=%v", modelCalls, renderer.renders, telegramCalls, frame, frameOK)
	}
}

func TestGuideCaptureHashIncludesRenderGeometry(t *testing.T) {
	first := tmux.StyledCapture{Columns: 71, VisibleRows: 37, CurrentPath: "/tmp", Title: "build"}
	second := first
	second.Columns = 120
	if guideCaptureHash("same text", "build", first) == guideCaptureHash("same text", "build", second) {
		t.Fatal("pane resize did not change the guide capture fingerprint")
	}
	styled := first
	styled.ANSI = "\x1b[31msame text"
	if guideCaptureHash("same text", "build", first) == guideCaptureHash("same text", "build", styled) {
		t.Fatal("ANSI-only change did not change the guide capture fingerprint")
	}
	if guideCaptureHash("same text", "build", first) == guideCaptureHash("same text", "renamed", first) {
		t.Fatal("Engram title change did not change the guide capture fingerprint")
	}
}

func TestGuideRefreshBindsFilesFromFullFrameBeforeStoredTail(t *testing.T) {
	a, runner, id := newSafetyApp(t, state.TerminalOriginCreated)
	file := filepath.Join(t.TempDir(), "early-report.txt")
	if err := os.WriteFile(file, []byte("report"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.AnchorChatID = 100
		session.AnchorMessageID = 77
		session.AnchorFormat = "text"
	}); err != nil {
		t.Fatal(err)
	}
	runner.capturePhysical = file + "\n" + strings.Repeat("ordinary output\n", 1400)
	runner.captureJoined = runner.capturePhysical
	model := anthropic.New("secret", "claude-haiku-4-5-20251001")
	model.BaseURL = "https://anthropic.test/messages"
	model.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"stop_reason":"end_turn","content":[{"type":"text","text":"A report was produced."}]}`))}, nil
	})}
	a.Guide = model
	var markup string
	tg := telegram.New("TOKEN")
	tg.BaseURL = "https://api.telegram.org/botTOKEN"
	tg.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(body["reply_markup"])
		if err != nil {
			t.Fatal(err)
		}
		markup = string(encoded)
		return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": map[string]any{"message_id": 77, "chat": map[string]any{"id": 100}}}), nil
	})}
	a.Telegram = tg

	a.refreshSession(context.Background(), id, true)
	got, _ := a.Store.FindSession(id)
	if len(got.LastRawCapture) > maxStoredVisibleCaptureBytes || strings.Contains(got.LastRawCapture, file) {
		t.Fatalf("stored tail unexpectedly retained early file: bytes=%d", len(got.LastRawCapture))
	}
	if !reflect.DeepEqual(got.AnchorFiles, []string{file}) || !strings.Contains(markup, "file:1:") {
		t.Fatalf("full-frame file binding=%#v markup=%q", got.AnchorFiles, markup)
	}
}

func TestGuideRefreshUpdatesFileBindingsWithoutAnotherModelRequest(t *testing.T) {
	a, runner, id := newSafetyApp(t, state.TerminalOriginCreated)
	file := filepath.Join(t.TempDir(), "appears-later.txt")
	if _, _, err := a.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.AnchorChatID = 100
		session.AnchorMessageID = 77
		session.AnchorFormat = "text"
	}); err != nil {
		t.Fatal(err)
	}
	runner.capturePhysical = file + "\nordinary output\n"
	runner.captureJoined = runner.capturePhysical
	modelCalls := 0
	model := anthropic.New("secret", "claude-haiku-4-5-20251001")
	model.BaseURL = "https://anthropic.test/messages"
	model.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(*http.Request) (*http.Response, error) {
		modelCalls++
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"stop_reason":"end_turn","content":[{"type":"text","text":"Ordinary output is visible."}]}`))}, nil
	})}
	a.Guide = model
	tg := telegram.New("TOKEN")
	tg.BaseURL = "https://api.telegram.org/botTOKEN"
	tg.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": map[string]any{"message_id": 77, "chat": map[string]any{"id": 100}}}), nil
	})}
	a.Telegram = tg

	a.refreshSession(context.Background(), id, true)
	if got, _ := a.Store.FindSession(id); len(got.AnchorFiles) != 0 {
		t.Fatalf("missing path became a file binding: %#v", got.AnchorFiles)
	}
	if err := os.WriteFile(file, []byte("report"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.LastAnchorEditAt = time.Now().Add(-11 * time.Second)
	}); err != nil {
		t.Fatal(err)
	}
	a.refreshSession(context.Background(), id, false)
	got, _ := a.Store.FindSession(id)
	if !reflect.DeepEqual(got.AnchorFiles, []string{file}) || modelCalls != 1 {
		t.Fatalf("file bindings=%#v model calls=%d", got.AnchorFiles, modelCalls)
	}
}

func TestAccessibleSnapshotCompanionUsesBoundedPlainFrame(t *testing.T) {
	a, runner, id := newSafetyApp(t, state.TerminalOriginCreated)
	runner.capturePhysical = "\x1b[31mhistory and visible\x1b[0m\n"
	runner.captureJoined = "history and visible\n"
	if _, _, err := a.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.AnchorChatID = 100
		session.AnchorMessageID = 77
		session.AnchorFormat = "snapshot"
	}); err != nil {
		t.Fatal(err)
	}
	ts, ok := a.Store.FindSession(id)
	if !ok {
		t.Fatal("session missing")
	}
	var uploaded string
	tg := telegram.New("TOKEN")
	tg.BaseURL = "https://api.telegram.org/botTOKEN"
	tg.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/botTOKEN/sendDocument" {
			t.Fatalf("unexpected endpoint %s", req.URL.Path)
		}
		if err := req.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		file, _, err := req.FormFile("document")
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(file)
		file.Close()
		if err != nil {
			t.Fatal(err)
		}
		uploaded = string(body)
		return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": map[string]any{"message_id": 77, "chat": map[string]any{"id": 100}}}), nil
	})}
	a.Telegram = tg
	a.rememberSnapshotTextFrame(ts, tmux.StyledCapture{JoinedText: strings.TrimSpace(runner.captureJoined)})
	frame, ok := a.snapshotTextFrame(ts)
	if !ok {
		t.Fatal("remembered snapshot frame unavailable")
	}
	before := len(runner.calls)
	a.uploadAccessibleSnapshotFrame(context.Background(), telegram.Message{Chat: telegram.Chat{ID: 100}}, ts, frame)
	if uploaded != strings.TrimSpace(runner.captureJoined) || strings.Contains(uploaded, "\x1b[") {
		t.Fatalf("accessible frame = %q, want joined plain text", uploaded)
	}
	if len(runner.calls) != before {
		t.Fatalf("accessible companion recaptured terminal: before=%d after=%d", before, len(runner.calls))
	}
}

func TestGuideDeliveryFailureDoesNotAdvanceCaptureOrConversation(t *testing.T) {
	a, runner, id := newSafetyApp(t, state.TerminalOriginCreated)
	if _, _, err := a.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.AnchorChatID = 100
		session.AnchorMessageID = 77
		session.AnchorFormat = "text"
	}); err != nil {
		t.Fatal(err)
	}
	runner.capturePhysical = "project\nbranch\ntests passed\napp ok\nstatus\ncwd\nready\n"
	runner.captureJoined = runner.capturePhysical
	modelCalls := 0
	model := anthropic.New("secret", "claude-haiku-4-5-20251001")
	model.BaseURL = "https://anthropic.test/messages"
	model.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(*http.Request) (*http.Response, error) {
		modelCalls++
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"stop_reason":"end_turn","content":[{"type":"text","text":"The tests passed and the prompt is ready."}]}`))}, nil
	})}
	a.Guide = model
	telegramCalls := 0
	telegramClient := telegram.New("TOKEN")
	telegramClient.BaseURL = "https://api.telegram.org/botTOKEN"
	telegramClient.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(*http.Request) (*http.Response, error) {
		telegramCalls++
		if telegramCalls == 1 {
			return telegramTestResponse(t, http.StatusInternalServerError, map[string]any{"ok": false, "error_code": 500, "description": "unavailable"}), nil
		}
		return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": map[string]any{"message_id": 77, "chat": map[string]any{"id": 100}}}), nil
	})}
	a.Telegram = telegramClient

	a.refreshSession(context.Background(), id, true)
	failed, _ := a.Store.FindSession(id)
	if failed.LastRawCaptureHash != "" || failed.LastSummary != "" || a.conversationEpochs[id].summary != "" {
		t.Fatalf("failed delivery advanced state: session=%#v epoch=%#v", failed, a.conversationEpochs[id])
	}
	a.refreshSession(context.Background(), id, true)
	succeeded, _ := a.Store.FindSession(id)
	if modelCalls != 2 || telegramCalls != 2 || succeeded.LastRawCaptureHash == "" || succeeded.LastSummary == "" || a.conversationEpochs[id].summary == "" {
		t.Fatalf("retry did not commit: model=%d telegram=%d session=%#v epoch=%#v", modelCalls, telegramCalls, succeeded, a.conversationEpochs[id])
	}
}

func TestGuideResultCannotCrossAReattachedServerBinding(t *testing.T) {
	a, runner, id := newSafetyApp(t, state.TerminalOriginCreated)
	if _, _, err := a.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.AnchorChatID = 100
		session.AnchorMessageID = 77
		session.AnchorFormat = "text"
	}); err != nil {
		t.Fatal(err)
	}
	runner.capturePhysical = "old server output\n"
	runner.captureJoined = runner.capturePhysical
	model := anthropic.New("secret", "claude-haiku-4-5-20251001")
	model.BaseURL = "https://anthropic.test/messages"
	model.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(*http.Request) (*http.Response, error) {
		if _, _, err := a.Store.UpdateSession(id, func(session *state.TerminalSession) {
			session.TmuxServerID = "abcdef0123456789abcdef0123456789"
		}); err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"stop_reason":"end_turn","content":[{"type":"text","text":"Old server summary."}]}`))}, nil
	})}
	a.Guide = model
	telegramCalls := 0
	telegramClient := telegram.New("TOKEN")
	telegramClient.BaseURL = "https://api.telegram.org/botTOKEN"
	telegramClient.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(*http.Request) (*http.Response, error) {
		telegramCalls++
		return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": map[string]any{"message_id": 77, "chat": map[string]any{"id": 100}}}), nil
	})}
	a.Telegram = telegramClient

	a.refreshSession(context.Background(), id, true)
	got, _ := a.Store.FindSession(id)
	if telegramCalls != 0 || got.LastSummary != "" || got.LastRawCaptureHash != "" || a.conversationEpochs[id].summary != "" {
		t.Fatalf("stale result crossed binding: telegram=%d session=%#v epoch=%#v", telegramCalls, got, a.conversationEpochs[id])
	}
}

func TestManualResetRejectsAnInFlightHaikuFailure(t *testing.T) {
	a, runner, id := newSafetyApp(t, state.TerminalOriginCreated)
	if _, _, err := a.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.AnchorChatID = 100
		session.AnchorMessageID = 77
		session.AnchorFormat = "text"
	}); err != nil {
		t.Fatal(err)
	}
	runner.capturePhysical = "work in progress\n"
	runner.captureJoined = runner.capturePhysical
	modelStarted := make(chan struct{})
	releaseModel := make(chan struct{})
	model := anthropic.New("secret", "claude-haiku-4-5-20251001")
	model.BaseURL = "https://anthropic.test/messages"
	model.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(*http.Request) (*http.Response, error) {
		close(modelStarted)
		<-releaseModel
		return &http.Response{StatusCode: http.StatusInternalServerError, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"message":"unavailable"}}`))}, nil
	})}
	a.Guide = model
	telegramCalls := 0
	telegramClient := telegram.New("TOKEN")
	telegramClient.BaseURL = "https://api.telegram.org/botTOKEN"
	telegramClient.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(*http.Request) (*http.Response, error) {
		telegramCalls++
		return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": map[string]any{"message_id": 77, "chat": map[string]any{"id": 100}}}), nil
	})}
	a.Telegram = telegramClient
	done := make(chan struct{})
	go func() {
		a.refreshSession(context.Background(), id, true)
		close(done)
	}()
	<-modelStarted
	a.resetConversationEpoch(id)
	close(releaseModel)
	<-done
	got, _ := a.Store.FindSession(id)
	if telegramCalls != 0 || got.LastRawCaptureHash != "" || got.LastSummary != "" {
		t.Fatalf("reset accepted stale failure: telegram=%d session=%#v", telegramCalls, got)
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
