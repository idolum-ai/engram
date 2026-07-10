package app

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
	"github.com/idolum-ai/engram/internal/tmux"
)

func TestDoubleSlashReplySendsSingleSlashToAnchor(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	ts, err := store.AllocateSession("main", "@1", "%1", "shell")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.UpdateSession(ts.ID, func(session *state.TerminalSession) {
		session.AnchorChatID = 100
		session.AnchorMessageID = 77
	}); err != nil {
		t.Fatal(err)
	}

	runner := &slashEscapeRunner{}
	refreshed := make(chan struct{}, 1)
	app := &App{
		Config: config.Config{
			TelegramAllowedUserID: 42,
			TelegramChatID:        100,
		},
		Store:          store,
		Tmux:           tmux.New(runner),
		summaryQueued:  map[int]bool{},
		summaryRunning: map[int]bool{},
		summaryForce:   map[int]bool{},
		sleepHook:      func(time.Duration) {},
		refreshHook: func(context.Context, int, bool) {
			refreshed <- struct{}{}
		},
	}

	status := app.handleUpdate(context.Background(), telegram.Update{Message: &telegram.Message{
		MessageID: 88,
		Chat:      telegram.Chat{ID: 100},
		From:      &telegram.User{ID: 42},
		Text:      "//clear",
		ReplyToMessage: &telegram.Message{
			MessageID: 77,
			Chat:      telegram.Chat{ID: 100},
		},
	}})
	if status != "anchor_reply_ok" {
		t.Fatalf("handleUpdate status = %q, want anchor_reply_ok", status)
	}
	want := [][]string{
		{"send-keys", "-t", "%1", "-l", "--", "/clear"},
		{"send-keys", "-t", "%1", "Enter"},
	}
	if len(runner.calls) != 3 || runner.calls[0][0] != "display-message" || !reflect.DeepEqual(runner.calls[1:], want) {
		t.Fatalf("tmux calls = %#v, want validation then %#v", runner.calls, want)
	}
	got, ok := store.FindSession(ts.ID)
	if !ok || got.LastInputPreview != "/clear" || got.LastInputMode != "command" {
		t.Fatalf("session after input = %#v ok=%v", got, ok)
	}
	select {
	case <-refreshed:
	case <-time.After(time.Second):
		t.Fatal("slash input did not queue refresh")
	}
}

func TestEscapedSlashInputRemovesExactlyOneSlash(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  string
		ok    bool
	}{
		{input: "//clear", want: "/clear", ok: true},
		{input: "///clear", want: "//clear", ok: true},
		{input: "/clear", ok: false},
		{input: "clear", ok: false},
	}
	for _, test := range tests {
		got, ok := escapedSlashInput(test.input)
		if got != test.want || ok != test.ok {
			t.Errorf("escapedSlashInput(%q) = %q, %v; want %q, %v", test.input, got, ok, test.want, test.ok)
		}
	}
}

type slashEscapeRunner struct {
	calls [][]string
}

func (r *slashEscapeRunner) Run(_ context.Context, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	if len(args) > 0 && args[0] == "display-message" {
		return "$1\t@1\t%1\tmain\t0\t0\t1\t/tmp\tbash\n", nil
	}
	return "", nil
}
