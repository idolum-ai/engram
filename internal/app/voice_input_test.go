package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
	"github.com/idolum-ai/engram/internal/tmux"
)

func TestCurrentVoiceReplyTranscribesAndRoutesOnce(t *testing.T) {
	app, runner, transcriber, telegramCalls := newVoiceInputTestApp(t)
	status := app.handleUpdate(context.Background(), voiceReplyUpdate(101, 77))
	if status != "voice_reply_ok" {
		t.Fatalf("status = %q", status)
	}
	app.transferWG.Wait()
	app.refreshWG.Wait()
	if transcriber.calls != 1 || transcriber.audio != "ogg-voice-data" {
		t.Fatalf("transcriber = calls:%d audio:%q", transcriber.calls, transcriber.audio)
	}
	if _, err := os.Stat(transcriber.path); !os.IsNotExist(err) {
		t.Fatalf("temporary voice file remains: %q err=%v", transcriber.path, err)
	}
	if len(runner.calls) != 4 || runner.calls[1][0] != "set-buffer" || runner.calls[1][4] != "(transcribed) please run the tests" || !strings.Contains(runner.calls[2][5], "paste-buffer -p -r -d") || !strings.Contains(runner.calls[3][5], "'Enter'") {
		t.Fatalf("tmux calls = %#v", runner.calls)
	}
	if got := strings.Join(*telegramCalls, ","); got != "getFile,downloadFile,sendMessage" {
		t.Fatalf("Telegram calls = %q", got)
	}
}

func TestCurrentVoiceReplyRetainsAndRoutesLocalPath(t *testing.T) {
	app, runner, transcriber, telegramCalls := newVoiceInputTestApp(t)
	app.Config.VoiceInputMode = config.VoiceInputModePath
	app.Transcriber = nil
	status := app.handleUpdate(context.Background(), voiceReplyUpdate(106, 77))
	if status != "voice_reply_ok" {
		t.Fatalf("status = %q", status)
	}
	app.transferWG.Wait()
	if transcriber.calls != 0 {
		t.Fatalf("path mode called transcriber %d times", transcriber.calls)
	}
	if len(runner.calls) != 4 || runner.calls[1][0] != "set-buffer" || !strings.HasPrefix(runner.calls[1][4], "(voice message: ") || !strings.HasSuffix(runner.calls[1][4], ".ogg)") {
		t.Fatalf("tmux calls = %#v", runner.calls)
	}
	attachments := app.Store.Snapshot().Attachments
	if len(attachments) != 1 || attachments[0].StoredPath == "" || attachments[0].SizeBytes != int64(len("ogg-voice-data")) {
		t.Fatalf("attachments = %#v", attachments)
	}
	if _, err := os.Stat(attachments[0].StoredPath); err != nil {
		t.Fatalf("retained voice path: %v", err)
	}
	if input := runner.calls[1][4]; input != "(voice message: "+attachments[0].StoredPath+")" {
		t.Fatalf("input = %q, path = %q", input, attachments[0].StoredPath)
	}
	if got := strings.Join(*telegramCalls, ","); got != "getFile,downloadFile" {
		t.Fatalf("Telegram calls = %q", got)
	}
}

func TestVoicePathReplyIsRevalidatedAfterDownload(t *testing.T) {
	app, runner, _, telegramCalls := newVoiceInputTestApp(t)
	app.Config.VoiceInputMode = config.VoiceInputModePath
	app.Transcriber = nil
	base := app.Telegram.HTTPClient.Transport
	app.Telegram.HTTPClient.Transport = snapshotRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if strings.Contains(request.URL.Path, "/file/botTOKEN/") {
			if _, _, err := app.Store.UpdateSession(1, func(s *state.TerminalSession) {
				s.StaleAlternateMessageIDs = append(s.StaleAlternateMessageIDs, s.AnchorMessageID)
				s.AnchorMessageID = 78
			}); err != nil {
				t.Fatal(err)
			}
		}
		return base.RoundTrip(request)
	})
	status := app.handleUpdate(context.Background(), voiceReplyUpdate(107, 77))
	if status != "voice_reply_ok" {
		t.Fatalf("status = %q", status)
	}
	app.transferWG.Wait()
	if len(runner.calls) != 0 || len(app.Store.Snapshot().Attachments) != 0 {
		t.Fatalf("retired path target reached state/tmux: attachments=%#v calls=%#v", app.Store.Snapshot().Attachments, runner.calls)
	}
	entries, err := os.ReadDir(app.Config.AttachmentDir())
	if err != nil || len(entries) != 0 {
		t.Fatalf("retired path left files: entries=%#v err=%v", entries, err)
	}
	if got := strings.Join(*telegramCalls, ","); got != "getFile,downloadFile,sendMessage" {
		t.Fatalf("Telegram calls = %q", got)
	}
}

func TestStaleVoiceReplyNeverDownloadsOrTranscribes(t *testing.T) {
	app, runner, transcriber, telegramCalls := newVoiceInputTestApp(t)
	status := app.handleUpdate(context.Background(), voiceReplyUpdate(102, 76))
	if status != "voice_reply_user_error" {
		t.Fatalf("status = %q", status)
	}
	app.transferWG.Wait()
	if transcriber.calls != 0 || len(runner.calls) != 0 {
		t.Fatalf("stale voice reached transcriber/tmux: calls=%d tmux=%#v", transcriber.calls, runner.calls)
	}
	if got := strings.Join(*telegramCalls, ","); got != "sendMessage" {
		t.Fatalf("Telegram calls = %q", got)
	}
}

func TestVoiceReplyIsRevalidatedAfterTranscription(t *testing.T) {
	app, runner, transcriber, telegramCalls := newVoiceInputTestApp(t)
	transcriber.hook = func() {
		if _, _, err := app.Store.UpdateSession(1, func(s *state.TerminalSession) {
			s.StaleAlternateMessageIDs = append(s.StaleAlternateMessageIDs, s.AnchorMessageID)
			s.AnchorMessageID = 78
		}); err != nil {
			panic(err)
		}
	}
	status := app.handleUpdate(context.Background(), voiceReplyUpdate(103, 77))
	if status != "voice_reply_ok" {
		t.Fatalf("status = %q", status)
	}
	app.transferWG.Wait()
	if len(runner.calls) != 0 {
		t.Fatalf("retired voice target reached tmux: %#v", runner.calls)
	}
	if got := strings.Join(*telegramCalls, ","); got != "getFile,downloadFile,sendMessage" {
		t.Fatalf("Telegram calls = %q", got)
	}
}

func TestVoiceTranscriptionHoldsDisclosureBoundary(t *testing.T) {
	app, _, transcriber, _ := newVoiceInputTestApp(t)
	transcriber.started = make(chan struct{})
	transcriber.release = make(chan struct{})
	if status := app.handleUpdate(context.Background(), voiceReplyUpdate(108, 77)); status != "voice_reply_ok" {
		t.Fatalf("status = %q", status)
	}
	select {
	case <-transcriber.started:
	case <-time.After(time.Second):
		t.Fatal("transcription did not start")
	}
	acquired := make(chan struct{})
	go func() {
		lock := app.disclosureMutex(1)
		lock.Lock()
		close(acquired)
		lock.Unlock()
	}()
	select {
	case <-acquired:
		t.Fatal("voice transcription released the disclosure boundary before model work finished")
	case <-time.After(100 * time.Millisecond):
	}
	close(transcriber.release)
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("disclosure boundary remained locked after transcription")
	}
	app.transferWG.Wait()
	app.refreshWG.Wait()
}

func TestVoiceTranscriptionFailureSendsNoInputAndRemovesTemporaryFile(t *testing.T) {
	app, runner, transcriber, telegramCalls := newVoiceInputTestApp(t)
	transcriber.err = errors.New("provider rejected audio")
	status := app.handleUpdate(context.Background(), voiceReplyUpdate(104, 77))
	if status != "voice_reply_ok" {
		t.Fatalf("status = %q", status)
	}
	app.transferWG.Wait()
	if len(runner.calls) != 0 {
		t.Fatalf("failed transcript reached tmux: %#v", runner.calls)
	}
	if _, err := os.Stat(transcriber.path); !os.IsNotExist(err) {
		t.Fatalf("temporary voice file remains: %q err=%v", transcriber.path, err)
	}
	if got := strings.Join(*telegramCalls, ","); got != "getFile,downloadFile,sendMessage" {
		t.Fatalf("Telegram calls = %q", got)
	}
}

func TestClosedVoiceReplyIsRejectedBeforeDownloadOrTranscription(t *testing.T) {
	app, runner, transcriber, telegramCalls := newVoiceInputTestApp(t)
	if _, _, err := app.Store.UpdateSession(1, func(s *state.TerminalSession) {
		s.State = state.TerminalClosed
		s.WatchEnabled = false
	}); err != nil {
		t.Fatal(err)
	}
	status := app.handleUpdate(context.Background(), voiceReplyUpdate(108, 77))
	if status != "voice_reply_user_error" {
		t.Fatalf("status = %q", status)
	}
	app.transferWG.Wait()
	if transcriber.calls != 0 || len(runner.calls) != 0 {
		t.Fatalf("closed voice reached provider/tmux: transcriber=%d tmux=%#v", transcriber.calls, runner.calls)
	}
	if got := strings.Join(*telegramCalls, ","); got != "sendMessage" {
		t.Fatalf("Telegram calls = %q", got)
	}
}

func TestVoiceTmuxFailureCompletesWithoutAnchorDeadlock(t *testing.T) {
	app, runner, _, telegramCalls := newVoiceInputTestApp(t)
	runner.inputErr = errors.New("tmux unavailable")
	status := app.handleUpdate(context.Background(), voiceReplyUpdate(109, 77))
	if status != "voice_reply_ok" {
		t.Fatalf("status = %q", status)
	}
	done := make(chan struct{})
	go func() {
		app.transferWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("voice transfer deadlocked after tmux failure")
	}
	if got := strings.Join(*telegramCalls, ","); !strings.Contains(got, "editMessageText") || !strings.HasSuffix(got, "sendMessage") {
		t.Fatalf("Telegram calls = %q", got)
	}
}

func TestVoiceDeliveryDoesNotTakeAnchorBeforeSession(t *testing.T) {
	app, _, _, _ := newVoiceInputTestApp(t)
	expected, ok := app.Store.FindSession(1)
	if !ok {
		t.Fatal("missing session")
	}
	sessionLock := app.sessionMutex(1)
	sessionLock.Lock()
	voiceDone := make(chan struct{})
	go func() {
		message := voiceReplyUpdate(110, 77).Message
		_ = app.deliverVoiceInput(context.Background(), *message, expected, 77, "test")
		close(voiceDone)
	}()
	time.Sleep(50 * time.Millisecond)
	anchorAcquired := make(chan struct{})
	go func() {
		anchorLock := app.anchorMutex(1)
		anchorLock.Lock()
		close(anchorAcquired)
		anchorLock.Unlock()
	}()
	select {
	case <-anchorAcquired:
	case <-time.After(time.Second):
		sessionLock.Unlock()
		t.Fatal("voice delivery held anchor while waiting for session lock")
	}
	sessionLock.Unlock()
	select {
	case <-voiceDone:
	case <-time.After(2 * time.Second):
		t.Fatal("voice delivery did not resume after session lock release")
	}
}

func TestOversizedVoiceReplyIsRejectedBeforeDownload(t *testing.T) {
	app, runner, transcriber, telegramCalls := newVoiceInputTestApp(t)
	update := voiceReplyUpdate(105, 77)
	update.Message.Voice.FileSize = telegramCloudDownloadMax + 1
	status := app.handleUpdate(context.Background(), update)
	if status != "voice_reply_user_error" {
		t.Fatalf("status = %q", status)
	}
	app.transferWG.Wait()
	if transcriber.calls != 0 || len(runner.calls) != 0 {
		t.Fatalf("oversized voice reached transcriber/tmux: calls=%d tmux=%#v", transcriber.calls, runner.calls)
	}
	if got := strings.Join(*telegramCalls, ","); got != "sendMessage" {
		t.Fatalf("Telegram calls = %q", got)
	}
}

func TestNormalizeVoiceTranscriptCreatesOneBoundedControlFreeLine(t *testing.T) {
	for _, test := range []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "spaces", input: "  run\n the\ttests  ", want: "run the tests"},
		{name: "escape", input: "run\x1b[31m", wantErr: true},
		{name: "bidi", input: "run\u202ethe tests", wantErr: true},
		{name: "empty", input: "\n\t", wantErr: true},
		{name: "too long", input: strings.Repeat("x", maxVoiceTranscriptBytes+1), wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := normalizeVoiceTranscript(test.input)
			if (err != nil) != test.wantErr || got != test.want {
				t.Fatalf("normalizeVoiceTranscript() = %q, %v", got, err)
			}
		})
	}
}

func newVoiceInputTestApp(t *testing.T) (*App, *slashEscapeRunner, *fakeVoiceTranscriber, *[]string) {
	t.Helper()
	dir := t.TempDir()
	runtimeDir := filepath.Join(dir, "runtime")
	if err := os.Mkdir(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	runtimeDir, err := filepath.EvalSymlinks(runtimeDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	store, err := state.Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.AllocateSession("main", "@1", "%1", "shell")
	if err != nil {
		t.Fatal(err)
	}
	session = bindTestSession(t, store, session.ID)
	if _, _, err := store.UpdateSession(session.ID, func(s *state.TerminalSession) {
		s.AnchorChatID = 100
		s.AnchorMessageID = 77
		s.StaleAlternateMessageIDs = []int{76}
	}); err != nil {
		t.Fatal(err)
	}
	telegramCalls := []string{}
	client := telegram.New("TOKEN")
	client.BaseURL = "https://telegram.test/botTOKEN"
	client.FileBase = "https://telegram.test/file/botTOKEN"
	client.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(request.URL.Path, "/getFile"):
			telegramCalls = append(telegramCalls, "getFile")
			return snapshotJSONResponse(`{"file_id":"voice-1","file_path":"voice/note.ogg"}`), nil
		case strings.Contains(request.URL.Path, "/file/botTOKEN/"):
			telegramCalls = append(telegramCalls, "downloadFile")
			return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader("ogg-voice-data")), Header: make(http.Header)}, nil
		case strings.HasSuffix(request.URL.Path, "/sendMessage"):
			var body map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if _, present := body["reply_markup"]; present {
				telegramCalls = append(telegramCalls, "sendMessageInvalidMarkup")
				return &http.Response{
					StatusCode: http.StatusBadRequest,
					Status:     "400 Bad Request",
					Body: io.NopCloser(strings.NewReader(
						`{"ok":false,"error_code":400,"description":"Bad Request: object expected as reply markup"}`,
					)),
					Header: make(http.Header),
				}, nil
			}
			telegramCalls = append(telegramCalls, "sendMessage")
			return snapshotJSONResponse(`{"message_id":200,"chat":{"id":100}}`), nil
		case strings.HasSuffix(request.URL.Path, "/editMessageText"):
			telegramCalls = append(telegramCalls, "editMessageText")
			return snapshotJSONResponse(`{"message_id":77,"chat":{"id":100}}`), nil
		default:
			t.Fatalf("unexpected Telegram request: %s %s", request.Method, request.URL)
			return nil, nil
		}
	})}
	runner := &slashEscapeRunner{}
	transcriber := &fakeVoiceTranscriber{text: "please run the tests"}
	cfg := config.Config{Home: dir, TelegramAllowedUserID: 42, TelegramChatID: 100, VoiceInputMode: config.VoiceInputModeTranscribe}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := config.EnsureDirs(cfg); err != nil {
		t.Fatal(err)
	}
	app := &App{
		Config: cfg,
		Store:  store, Telegram: client, Tmux: tmux.New(runner), Transcriber: transcriber,
		transferSlots: make(chan struct{}, 1), transferQueue: make(chan struct{}, 2),
		summaryQueued: map[int]bool{}, summaryRunning: map[int]bool{}, summaryForce: map[int]bool{},
		sleepHook: func(time.Duration) {}, refreshHook: func(context.Context, int, bool) {},
	}
	return app, runner, transcriber, &telegramCalls
}

func voiceReplyUpdate(messageID, targetID int) telegram.Update {
	return telegram.Update{Message: &telegram.Message{
		MessageID: messageID, Chat: telegram.Chat{ID: 100}, From: &telegram.User{ID: 42},
		Voice:          &telegram.Document{FileID: "voice-1", FileSize: int64(len("ogg-voice-data")), MimeType: "audio/ogg"},
		ReplyToMessage: &telegram.Message{MessageID: targetID, Chat: telegram.Chat{ID: 100}},
	}}
}

type fakeVoiceTranscriber struct {
	text    string
	audio   string
	path    string
	calls   int
	hook    func()
	err     error
	started chan struct{}
	release chan struct{}
}

func (f *fakeVoiceTranscriber) Transcribe(_ context.Context, path string) (string, error) {
	f.calls++
	f.path = path
	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	f.audio = string(body)
	if f.started != nil {
		close(f.started)
	}
	if f.release != nil {
		<-f.release
	}
	if f.hook != nil {
		f.hook()
	}
	if f.err != nil {
		return "", f.err
	}
	return f.text, nil
}
