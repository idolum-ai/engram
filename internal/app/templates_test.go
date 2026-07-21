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
	"github.com/idolum-ai/engram/internal/templates"
	"github.com/idolum-ai/engram/internal/tmux"
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
	if len(replies) != 3 || replies[0] != "Remembered {engram:review-panel}." || !strings.Contains(replies[1], "{engram:review-panel}") || !strings.Contains(replies[2], "Review carefully") {
		t.Fatalf("replies = %#v", replies)
	}
	exactBody := "    if ready:\n        run() \n"
	if status := app.handleUpdate(context.Background(), textUpdate(107, "/remember exact\n"+exactBody, 0)); status != "command_ok" {
		t.Fatalf("exact remember status = %q", status)
	}
	if item, found := templateStore.Get("exact"); !found || item.Body != exactBody {
		t.Fatalf("exact body = %q, found=%v", item.Body, found)
	}

	status := app.handleUpdate(context.Background(), textUpdate(104, "Before {engram:review-panel} After {review-panel}.", 10))
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
	if status := app.handleUpdate(context.Background(), textUpdate(106, "Use {engram:review-panel}", 10)); status != "anchor_reply_template_error" {
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
	if _, _, err := templateStore.Put("review-panel", "expanded"); err != nil {
		t.Fatal(err)
	}
	app.Templates = templateStore
	transcriber.text = "please use {engram:review-panel}"
	if status := app.handleUpdate(context.Background(), voiceReplyUpdate(201, 77)); status != "voice_reply_ok" {
		t.Fatalf("voice status = %q", status)
	}
	app.transferWG.Wait()
	app.refreshWG.Wait()
	if len(runner.calls) != 4 || runner.calls[1][4] != "(transcribed) please use {engram:review-panel}" {
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
		{name: "escaped slash reply", text: "  //{engram:review-panel}  ", replyTo: 10, wantStatus: "anchor_reply_ok", wantInput: "  /Review carefully.  ", wantCalls: 4},
		{name: "literal whitespace reply", text: "  {engram:review-panel}  ", replyTo: 10, wantStatus: "anchor_reply_ok", wantInput: "  Review carefully.  ", wantCalls: 4},
		{name: "send command", text: "/send 1  Before {engram:review-panel}  ", wantStatus: "command_ok", wantInput: " Before Review carefully.  ", wantCalls: 4},
		{name: "text command", text: "/text 1  Before {engram:review-panel}  ", wantStatus: "command_ok", wantInput: " Before Review carefully.  ", wantCalls: 3},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app, runner, _ := newAnchorKeyTestApp(t)
			templateStore, err := templates.Open(filepath.Join(t.TempDir(), "templates.json"))
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := templateStore.Put("review-panel", "Review carefully."); err != nil {
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

func TestNewSessionRoutesPreserveWhitespaceAroundTemplates(t *testing.T) {
	for _, test := range []struct {
		name string
		text string
		want string
	}{
		{name: "plain message", text: "  {engram:review-panel}  ", want: "  Review carefully.  "},
		{name: "new command", text: "/new  {engram:review-panel}  ", want: " Review carefully.  "},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			store, err := state.Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			templateStore, err := templates.Open(filepath.Join(dir, "templates.json"))
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := templateStore.Put("review-panel", "Review carefully."); err != nil {
				t.Fatal(err)
			}
			runner := &newSessionRunner{}
			client := telegram.New("TOKEN")
			client.BaseURL = "https://api.telegram.org/botTOKEN"
			client.HTTPClient = &http.Client{Transport: anchorKeyRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return anchorKeyJSONResponse(`{"message_id":120,"chat":{"id":100}}`), nil
			})}
			app := &App{
				Config: config.Config{
					TelegramAllowedUserID: 42,
					TelegramChatID:        100,
					TmuxSession:           "main",
					Workdir:               dir,
				},
				Store:          store,
				Templates:      templateStore,
				Telegram:       client,
				Tmux:           tmux.New(runner),
				summaryQueued:  map[int]bool{},
				summaryRunning: map[int]bool{},
				summaryForce:   map[int]bool{},
				sleepHook:      func(time.Duration) {},
				refreshHook:    func(context.Context, int, bool) {},
			}
			if status := app.handleUpdate(context.Background(), textUpdate(401, test.text, 0)); status != "new_session_ok" && status != "command_ok" {
				t.Fatalf("status = %q", status)
			}
			app.refreshWG.Wait()
			var got string
			for _, call := range runner.calls {
				if len(call) > 4 && call[0] == "set-buffer" {
					got = call[4]
					break
				}
			}
			if got != test.want {
				t.Fatalf("tmux input = %q, want %q; calls=%#v", got, test.want, runner.calls)
			}
		})
	}
}

func TestExactCommandPayloadParsing(t *testing.T) {
	t.Parallel()
	if got, ok := exactCommandPayload("  {engram:review-panel}  "); !ok || got != " {engram:review-panel}  " {
		t.Fatalf("new payload = %q, ok=%v", got, ok)
	}
	for _, command := range []string{"send", "text"} {
		id, got, ok := parseIDRestExact(" 1  {engram:review-panel}  ")
		if !ok || id != 1 || got != " {engram:review-panel}  " {
			t.Fatalf("%s payload id=%d text=%q ok=%v", command, id, got, ok)
		}
	}
}

func TestNewSessionMessagePreservesWhitespaceAroundTemplate(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	templateStore, err := templates.Open(filepath.Join(dir, "templates.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := templateStore.Put("review-panel", "Review carefully."); err != nil {
		t.Fatal(err)
	}
	runner := &newSessionRunner{}
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: anchorKeyRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network unavailable")
	})}
	app := &App{
		Config: config.Config{
			TelegramAllowedUserID: 42,
			TelegramChatID:        100,
			TmuxSession:           "main",
			Workdir:               dir,
		},
		Store:          store,
		Templates:      templateStore,
		Telegram:       client,
		Tmux:           tmux.New(runner),
		summaryQueued:  map[int]bool{},
		summaryRunning: map[int]bool{},
		summaryForce:   map[int]bool{},
	}
	if status := app.handleUpdate(context.Background(), textUpdate(310, "  {engram:review-panel}  ", 0)); status != "new_session_telegram_failed" {
		t.Fatalf("status = %q", status)
	}
	for _, call := range runner.calls {
		if len(call) == 5 && call[0] == "set-buffer" {
			if call[4] != "  Review carefully.  " {
				t.Fatalf("tmux input = %q", call[4])
			}
			return
		}
	}
	t.Fatalf("tmux set-buffer call missing: %#v", runner.calls)
}

func TestTextInputRejectsLineBreaksBeforeTmux(t *testing.T) {
	app, runner, _ := newAnchorKeyTestApp(t)
	result := app.sendInput(context.Background(), 1, "first\nsecond", "text", false)
	if result.Outcome != actionUserError || !strings.Contains(result.Message, "one line") {
		t.Fatalf("result = %#v", result)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("multiline text reached tmux: %#v", runner.calls)
	}
}

func TestTextCommandRejectsMultilineTemplateBeforeTmux(t *testing.T) {
	app, runner, _ := newAnchorKeyTestApp(t)
	var reply string
	app.Telegram.HTTPClient = &http.Client{Transport: anchorKeyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/botTOKEN/sendMessage" {
			t.Fatalf("path = %s", req.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		reply, _ = body["text"].(string)
		return anchorKeyJSONResponse(`{"message_id":120,"chat":{"id":100}}`), nil
	})}
	templateStore, err := templates.Open(filepath.Join(t.TempDir(), "templates.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := templateStore.Put("multiline", "first\nsecond"); err != nil {
		t.Fatal(err)
	}
	app.Templates = templateStore
	if status := app.handleUpdate(context.Background(), textUpdate(309, "/text 1 {engram:multiline}", 0)); status != "command_user_error" {
		t.Fatalf("status = %q", status)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("multiline template reached tmux: %#v", runner.calls)
	}
	if !strings.Contains(reply, "one line") {
		t.Fatalf("reply = %q", reply)
	}
}

func TestPrepareTypedInputAuditsBeforeDeliveryWithoutBody(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	templateStore, err := templates.Open(filepath.Join(dir, "templates.json"))
	if err != nil {
		t.Fatal(err)
	}
	const body = "sensitive remembered body"
	if _, _, err := templateStore.Put("review-panel", body); err != nil {
		t.Fatal(err)
	}
	app := &App{Store: store, Templates: templateStore}
	expanded, err := app.prepareTypedInput("Use {engram:review-panel}", "send", 7)
	if err != nil || expanded != "Use "+body {
		t.Fatalf("expanded=%q error=%v", expanded, err)
	}
	audit, err := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(audit)
	for _, want := range []string{`"type":"template.expand"`, `"status":"prepared"`, `"session_id":7`, `"route":"send"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("audit missing %q: %s", want, text)
		}
	}
	if strings.Contains(text, body) {
		t.Fatalf("audit retained template body: %s", text)
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
	if _, _, err := templateStore.Put("review-panel", "Review carefully."); err != nil {
		t.Fatal(err)
	}

	var uploaded []byte
	var filename, caption string
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: anchorKeyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/botTOKEN/sendMessage" {
			return anchorKeyJSONResponse(`{"message_id":119,"chat":{"id":100}}`), nil
		}
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

	if status := app.handleUpdate(context.Background(), textUpdate(401, "/templates export", 0)); status != "command_ok" {
		t.Fatalf("status = %q", status)
	}
	if _, _, err := templateStore.Put("review-panel", "A newer body."); err != nil {
		t.Fatal(err)
	}
	<-app.transferSlots
	app.transferWG.Wait()
	if filename != "templates.json" || caption != "templates.json" {
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
	matches, err := filepath.Glob(filepath.Join(app.Config.ArtifactDir(), "engram-download-*.bin"))
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
