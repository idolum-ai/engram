package app

import (
	"bytes"
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

type fileCaptureRunner struct{ calls int }

func (r *fileCaptureRunner) Run(_ context.Context, _ ...string) (string, error) {
	r.calls++
	return "", errors.New("unexpected tmux capture")
}

type successfulFileCaptureRunner struct{}

func (successfulFileCaptureRunner) Run(_ context.Context, args ...string) (string, error) {
	switch args[0] {
	case "display-message":
		return framedTmuxBindingRecord("$1", "@1", "%1", "main", "0", "0", "1", "/tmp", "bash"), nil
	case "if-shell":
		return "visible terminal\n", nil
	default:
		return "", errors.New("unexpected tmux call")
	}
}

func TestQueuedCaptureDoesNotCrossBindingBoundary(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.AllocateSession("main", "@1", "%1", "shell")
	if err != nil {
		t.Fatal(err)
	}
	session, _, err = store.UpdateSession(session.ID, func(current *state.TerminalSession) {
		current.TmuxServerID = appTestServerID
		current.WatchEnabled = true
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.UpdateSession(session.ID, func(current *state.TerminalSession) {
		current.TmuxServerID = strings.Repeat("b", 32)
	}); err != nil {
		t.Fatal(err)
	}
	var reply string
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: fileRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		var payload map[string]any
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		reply, _ = payload["text"].(string)
		return fileJSONResponse(t, map[string]any{"ok": true, "result": map[string]any{"message_id": 2, "chat": map[string]any{"id": 100}}}), nil
	})}
	runner := &fileCaptureRunner{}
	app := &App{Config: config.Config{Home: dir, TelegramChatID: 100}, Store: store, Telegram: client, Tmux: tmux.New(runner)}

	app.captureSessionFile(context.Background(), telegram.Message{Chat: telegram.Chat{ID: 100}}, session, false)

	if runner.calls != 0 || !strings.Contains(reply, "session changed") {
		t.Fatalf("tmux calls=%d reply=%q", runner.calls, reply)
	}
}

func TestCaptureUploadDoesNotHoldTerminalInputMutex(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.AllocateSession("main", "@1", "%1", "shell")
	if err != nil {
		t.Fatal(err)
	}
	session, _, err = store.UpdateSession(session.ID, func(current *state.TerminalSession) {
		current.TmuxServerID = appTestServerID
		current.WatchEnabled = true
	})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: fileRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/botTOKEN/sendDocument" {
			return nil, errors.New("unexpected Telegram endpoint")
		}
		close(started)
		<-release
		return fileJSONResponse(t, map[string]any{"ok": true, "result": map[string]any{"message_id": 2, "chat": map[string]any{"id": 100}}}), nil
	})}
	app := &App{Config: config.Config{Home: dir, TelegramChatID: 100}, Store: store, Telegram: client, Tmux: tmux.New(successfulFileCaptureRunner{})}
	if err := os.MkdirAll(app.Config.ArtifactDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		app.captureSessionFile(context.Background(), telegram.Message{Chat: telegram.Chat{ID: 100}}, session, false)
		close(done)
	}()
	<-started
	inputReady := make(chan struct{})
	go func() {
		lock := app.sessionMutex(session.ID)
		lock.Lock()
		lock.Unlock()
		close(inputReady)
	}()
	select {
	case <-inputReady:
	case <-time.After(time.Second):
		t.Fatal("Telegram upload retained the terminal input mutex")
	}
	close(release)
	<-done
}

func TestHandleAttachmentEnforcesSoftLimitDuringDownload(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, "tmp")
	t.Setenv("TMPDIR", tmp)
	store, err := state.Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var replies []string
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.FileBase = "https://api.telegram.org/file/botTOKEN"
	client.HTTPClient = &http.Client{Transport: fileRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/botTOKEN/getFile":
			return fileJSONResponse(t, map[string]any{
				"ok": true,
				"result": map[string]any{
					"file_id":   "file-1",
					"file_path": "docs/file.bin",
				},
			}), nil
		case "/file/botTOKEN/docs/file.bin":
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Body:       io.NopCloser(strings.NewReader("123456")),
				Header:     make(http.Header),
			}, nil
		case "/botTOKEN/sendMessage":
			var payload map[string]any
			if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			replies = append(replies, payload["text"].(string))
			return fileJSONResponse(t, map[string]any{
				"ok":     true,
				"result": map[string]any{"message_id": 2, "chat": map[string]any{"id": 100}},
			}), nil
		default:
			t.Fatalf("unexpected request path %s", req.URL.Path)
			return nil, nil
		}
	})}
	app := &App{
		Config: config.Config{
			Home:                   dir,
			AttachmentSoftMaxBytes: 5,
			TelegramChatID:         100,
		},
		Store:    store,
		Telegram: client,
	}

	app.handleAttachment(context.Background(), telegram.Message{
		MessageID: 1,
		Chat:      telegram.Chat{ID: 100},
		From:      &telegram.User{ID: 42},
	}, telegram.Document{
		FileID:   "file-1",
		FileName: "file.bin",
		FileSize: 0,
	})
	app.transferWG.Wait()

	if len(replies) != 2 || replies[0] != "receiving attachment..." || !strings.Contains(replies[1], "attachment too large") {
		t.Fatalf("replies = %#v, want too-large reply", replies)
	}
	if got := store.Snapshot().Attachments; len(got) != 0 {
		t.Fatalf("attachments = %#v, want none", got)
	}
	if entries, err := os.ReadDir(app.Config.AttachmentDir()); err == nil {
		for _, entry := range entries {
			if strings.Contains(entry.Name(), "file-1") {
				t.Fatalf("oversized partial attachment remained: %s", entry.Name())
			}
		}
	}
}

func TestTailFileReturnsOnlySuffix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	if err := os.WriteFile(path, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := tailFile(path, 4)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "6789" {
		t.Fatalf("tailFile = %q, want 6789", got)
	}
}

func TestTailFileMissingIsEmpty(t *testing.T) {
	got, err := tailFile(filepath.Join(t.TempDir(), "missing.jsonl"), 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("tailFile missing = %q, want empty", got)
	}
}

func TestTailAuditLogSpansRotatedAndCurrentFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	if err := os.WriteFile(path+".1", []byte("previous-"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("current"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := tailAuditLog(path, 12)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "ious-current" {
		t.Fatalf("tailAuditLog = %q, want ious-current", got)
	}
}

func TestDownloadRejectsFileAboveTelegramCloudLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.bin")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(telegramCloudUploadMax + 1); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	var replies []string
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: fileRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		var payload map[string]any
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		replies = append(replies, payload["text"].(string))
		return fileJSONResponse(t, map[string]any{"ok": true, "result": map[string]any{"message_id": 2, "chat": map[string]any{"id": 100}}}), nil
	})}
	app := &App{Config: config.Config{TelegramChatID: 100}, Telegram: client}
	result := app.download(context.Background(), telegram.Message{MessageID: 1, Chat: telegram.Chat{ID: 100}}, path)
	if result.Outcome != actionUserError {
		t.Fatalf("download outcome = %q, want %q", result.Outcome, actionUserError)
	}
	if len(replies) != 1 || !strings.Contains(replies[0], "exceeds Telegram's") {
		t.Fatalf("replies = %#v", replies)
	}
}

func TestAttachmentBypassStillHonorsHardLimit(t *testing.T) {
	var replies []string
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: fileRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/botTOKEN/sendMessage" {
			t.Fatalf("unexpected request %s", req.URL.Path)
		}
		var payload map[string]any
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		replies = append(replies, payload["text"].(string))
		return fileJSONResponse(t, map[string]any{"ok": true, "result": map[string]any{"message_id": 2, "chat": map[string]any{"id": 100}}}), nil
	})}
	app := &App{
		Config:   config.Config{Home: t.TempDir(), TelegramChatID: 100, AttachmentSoftMaxBytes: 1 * 1024 * 1024},
		Store:    mustOpenTestStore(t),
		Telegram: client,
	}
	app.handleAttachment(context.Background(), telegram.Message{
		MessageID: 1,
		Chat:      telegram.Chat{ID: 100},
		From:      &telegram.User{ID: 42},
		Caption:   "/attachment_bypass sha256:" + strings.Repeat("a", 64),
	}, telegram.Document{FileID: "large", FileSize: 5 * 1024 * 1024})
	if len(replies) != 1 || !strings.Contains(replies[0], "hard limit") {
		t.Fatalf("replies = %#v", replies)
	}
}

func TestDownloadSnapshotUsesOpenedFileAfterPathReplacement(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	path := filepath.Join(dir, "source.txt")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, _, err := openDownloadSource(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	app := &App{Config: config.Config{Home: dir}}
	snapshot, err := app.snapshotDownloadSource(source)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(snapshot)
	data, err := os.ReadFile(snapshot)
	if err != nil || string(data) != "original" {
		t.Fatalf("snapshot = %q, err=%v", data, err)
	}
}

func TestDownloadPreservesTelegramVisibleFilename(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	path := filepath.Join(dir, "engram-coherence-proposal.md")
	if err := os.WriteFile(path, []byte("proposal"), 0o600); err != nil {
		t.Fatal(err)
	}
	var visibleFilename string
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: fileRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/botTOKEN/sendMessage":
			return fileJSONResponse(t, map[string]any{"ok": true, "result": map[string]any{"message_id": 2, "chat": map[string]any{"id": 100}}}), nil
		case "/botTOKEN/sendDocument":
			if err := req.ParseMultipartForm(1024); err != nil {
				t.Fatal(err)
			}
			files := req.MultipartForm.File["document"]
			if len(files) == 1 {
				visibleFilename = files[0].Filename
			}
			return fileJSONResponse(t, map[string]any{"ok": true, "result": map[string]any{"message_id": 3, "chat": map[string]any{"id": 100}}}), nil
		default:
			t.Fatalf("unexpected Telegram path %s", req.URL.Path)
			return nil, nil
		}
	})}
	app := &App{Config: config.Config{Home: dir, TelegramChatID: 100}, Telegram: client}
	result := app.download(context.Background(), telegram.Message{MessageID: 1, Chat: telegram.Chat{ID: 100}}, path)
	if !result.OK() {
		t.Fatalf("download result = %#v", result)
	}
	app.transferWG.Wait()
	if visibleFilename != "engram-coherence-proposal.md" {
		t.Fatalf("visible filename = %q", visibleFilename)
	}
}

func TestBoundedWriterStopsAtUploadLimit(t *testing.T) {
	var dst bytes.Buffer
	writer := &boundedWriter{Writer: &dst, Remaining: 5}
	written, err := writer.Write([]byte("123456"))
	if written != 5 || !errors.Is(err, errArtifactTooLarge) || dst.String() != "12345" {
		t.Fatalf("bounded write = %d, %v, %q", written, err, dst.String())
	}
}

func TestCreatePredictableArtifactDoesNotTruncateOrFollow(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "engram-raw-1-stamp.txt")
	if err := os.WriteFile(original, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}

	file, created, err := createPredictableArtifact(dir, filepath.Base(original))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("new capture"); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if got, want := created, filepath.Join(dir, "engram-raw-1-stamp-2.txt"); got != want {
		t.Fatalf("created path = %q, want %q", got, want)
	}
	data, err := os.ReadFile(original)
	if err != nil || string(data) != "keep me" {
		t.Fatalf("preexisting artifact = %q, err=%v", data, err)
	}
	info, err := os.Stat(created)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("created artifact mode = %v", info.Mode())
	}

	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "engram-logs-stamp.jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	linkedFile, linkedCreated, err := createPredictableArtifact(dir, filepath.Base(link))
	if err != nil {
		t.Fatal(err)
	}
	if err := linkedFile.Close(); err != nil {
		t.Fatal(err)
	}
	if got, want := linkedCreated, filepath.Join(dir, "engram-logs-stamp-2.jsonl"); got != want {
		t.Fatalf("created path beside symlink = %q, want %q", got, want)
	}
	data, err = os.ReadFile(target)
	if err != nil || string(data) != "target" {
		t.Fatalf("symlink target = %q, err=%v", data, err)
	}
}

func TestTransferQueueRejectsExcessWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	app := &App{
		runCtx:        ctx,
		transferSlots: make(chan struct{}, 1),
		transferQueue: make(chan struct{}, 1),
	}
	app.transferSlots <- struct{}{}
	ran := make(chan struct{}, 1)
	if !app.queueTransfer(func(context.Context) { ran <- struct{}{} }) {
		t.Fatal("first transfer was rejected")
	}
	if app.queueTransfer(func(context.Context) {}) {
		t.Fatal("excess transfer was queued")
	}
	<-app.transferSlots
	app.transferWG.Wait()
	select {
	case <-ran:
	default:
		t.Fatal("accepted transfer did not run")
	}
}

func mustOpenTestStore(t *testing.T) *state.Store {
	t.Helper()
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

type fileRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn fileRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func fileJSONResponse(t *testing.T, payload map[string]any) *http.Response {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Body:       io.NopCloser(bytes.NewReader(data)),
		Header:     make(http.Header),
	}
}
