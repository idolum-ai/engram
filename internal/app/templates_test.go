package app

import (
	"context"
	"encoding/json"
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
	"github.com/idolum-ai/engram/internal/templates"
)

func TestRememberInspectExpandAndForgetTemplate(t *testing.T) {
	app, runner, refreshed := newAnchorKeyTestApp(t)
	templateStore, err := templates.Open(filepath.Join(t.TempDir(), "templates.json"))
	if err != nil {
		t.Fatal(err)
	}
	app.Templates = templateStore
	var replies []string
	app.Telegram.HTTPClient = &http.Client{Transport: anchorKeyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/botTOKEN/sendMessage" {
			t.Fatalf("path = %s", req.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		replies = append(replies, body["text"].(string))
		return anchorKeyJSONResponse(`{"message_id":120,"chat":{"id":100}}`), nil
	})}

	updates := []telegram.Update{
		textUpdate(101, "/remember review-panel\nReview carefully, then report concrete findings.", 0),
		textUpdate(102, "/remember", 0),
		textUpdate(103, "/remember review-panel", 0),
	}
	for _, update := range updates {
		if status := app.handleUpdate(context.Background(), update); status != "command_ok" {
			t.Fatalf("command status = %q", status)
		}
	}
	if len(replies) != 3 || replies[0] != "Remembered {review-panel}." || !strings.Contains(replies[1], "{review-panel}") || !strings.Contains(replies[2], "Review carefully") {
		t.Fatalf("replies = %#v", replies)
	}

	status := app.handleUpdate(context.Background(), textUpdate(104, "Before {review-panel} After {{review-panel}}.", 10))
	if status != "anchor_reply_ok" {
		t.Fatalf("expanded reply status = %q", status)
	}
	app.refreshWG.Wait()
	select {
	case <-refreshed:
	case <-time.After(time.Second):
		t.Fatal("expanded input did not request refresh")
	}
	if len(runner.calls) != 4 || runner.calls[1][0] != "set-buffer" || runner.calls[1][4] != "Before Review carefully, then report concrete findings. After {review-panel}." {
		t.Fatalf("tmux calls = %#v", runner.calls)
	}

	if status := app.handleUpdate(context.Background(), textUpdate(105, "/forget review-panel", 0)); status != "command_ok" {
		t.Fatalf("forget status = %q", status)
	}
	before := len(runner.calls)
	if status := app.handleUpdate(context.Background(), textUpdate(106, "Use {review-panel}", 10)); status != "anchor_reply_template_error" {
		t.Fatalf("forgotten expansion status = %q", status)
	}
	if len(runner.calls) != before || !strings.Contains(replies[len(replies)-1], "unknown template") {
		t.Fatalf("forgotten template reached tmux=%#v replies=%#v", runner.calls[before:], replies)
	}
}

func TestVoiceTranscriptionDoesNotExpandTemplates(t *testing.T) {
	app, runner, transcriber, _ := newVoiceInputTestApp(t)
	templateStore, err := templates.Open(filepath.Join(t.TempDir(), "templates.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := templateStore.Put("review-panel", "expanded", time.Time{}); err != nil {
		t.Fatal(err)
	}
	app.Templates = templateStore
	transcriber.text = "please use {review-panel}"
	if status := app.handleUpdate(context.Background(), voiceReplyUpdate(201, 77)); status != "voice_reply_ok" {
		t.Fatalf("voice status = %q", status)
	}
	app.transferWG.Wait()
	app.refreshWG.Wait()
	if len(runner.calls) != 4 || runner.calls[1][4] != "(transcribed) please use {review-panel}" {
		t.Fatalf("voice input was expanded: %#v", runner.calls)
	}
}

func TestTypedInputRoutesExpandTemplates(t *testing.T) {
	tests := []struct {
		name       string
		text       string
		replyTo    int
		wantStatus string
		wantInput  string
		wantCalls  int
	}{
		{name: "escaped slash reply", text: "//{review-panel}", replyTo: 10, wantStatus: "anchor_reply_ok", wantInput: "/Review carefully.", wantCalls: 4},
		{name: "send command", text: "/send 1 Before {review-panel}", wantStatus: "command_ok", wantInput: "Before Review carefully.", wantCalls: 4},
		{name: "text command", text: "/text 1 Before {review-panel}", wantStatus: "command_ok", wantInput: "Before Review carefully.", wantCalls: 3},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app, runner, _ := newAnchorKeyTestApp(t)
			templateStore, err := templates.Open(filepath.Join(t.TempDir(), "templates.json"))
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := templateStore.Put("review-panel", "Review carefully.", time.Time{}); err != nil {
				t.Fatal(err)
			}
			app.Templates = templateStore

			if status := app.handleUpdate(context.Background(), textUpdate(301, test.text, test.replyTo)); status != test.wantStatus {
				t.Fatalf("status = %q, want %q", status, test.wantStatus)
			}
			app.refreshWG.Wait()
			if len(runner.calls) != test.wantCalls || runner.calls[1][0] != "set-buffer" || runner.calls[1][4] != test.wantInput {
				t.Fatalf("tmux calls = %#v", runner.calls)
			}
		})
	}
}

func TestTemplatesCommandUploadsPrivateStoreSnapshot(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("TMPDIR", dir)
	store, err := state.Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	templateStore, err := templates.Open(filepath.Join(dir, "templates.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := templateStore.Put("review-panel", "Review carefully.", time.Unix(7, 0)); err != nil {
		t.Fatal(err)
	}

	var uploaded []byte
	var filename, caption string
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: anchorKeyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/botTOKEN/sendDocument" {
			t.Fatalf("path = %s", req.URL.Path)
		}
		if err := req.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		files := req.MultipartForm.File["document"]
		if len(files) != 1 {
			t.Fatalf("documents = %d", len(files))
		}
		filename = files[0].Filename
		caption = req.FormValue("caption")
		file, err := files[0].Open()
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		uploaded, err = io.ReadAll(file)
		if err != nil {
			t.Fatal(err)
		}
		return anchorKeyJSONResponse(`{"message_id":120,"chat":{"id":100}}`), nil
	})}
	app := &App{
		Config: config.Config{Home: dir, TelegramAllowedUserID: 42, TelegramChatID: 100},
		Store:  store, Templates: templateStore, Telegram: client,
		transferSlots: make(chan struct{}, 1),
	}
	app.transferSlots <- struct{}{}
	if err := os.MkdirAll(app.Config.ArtifactDir(), 0o700); err != nil {
		t.Fatal(err)
	}

	if status := app.handleUpdate(context.Background(), textUpdate(401, "/templates", 0)); status != "command_ok" {
		t.Fatalf("status = %q", status)
	}
	if _, _, err := templateStore.Put("review-panel", "A newer body.", time.Unix(8, 0)); err != nil {
		t.Fatal(err)
	}
	<-app.transferSlots
	app.transferWG.Wait()
	if filename != "engram-templates.json" || caption != "Engram remembered input templates" {
		t.Fatalf("filename=%q caption=%q", filename, caption)
	}
	var exported struct {
		Version   int                  `json:"version"`
		Templates []templates.Template `json:"templates"`
	}
	if err := json.Unmarshal(uploaded, &exported); err != nil {
		t.Fatal(err)
	}
	if exported.Version != 1 || len(exported.Templates) != 1 || exported.Templates[0].Body != "Review carefully." {
		t.Fatalf("export = %#v", exported)
	}
	matches, err := filepath.Glob(filepath.Join(app.Config.ArtifactDir(), "engram-templates-export-*.json"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary exports = %#v, error=%v", matches, err)
	}
	audit, err := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(audit), "Review carefully") || strings.Contains(string(audit), "A newer body") {
		t.Fatalf("audit retained a template body: %s", audit)
	}
}

func textUpdate(messageID int, text string, replyTo int) telegram.Update {
	message := &telegram.Message{
		MessageID: messageID,
		Chat:      telegram.Chat{ID: 100},
		From:      &telegram.User{ID: 42},
		Text:      text,
	}
	if replyTo > 0 {
		message.ReplyToMessage = &telegram.Message{MessageID: replyTo, Chat: telegram.Chat{ID: 100}}
	}
	return telegram.Update{Message: message}
}
