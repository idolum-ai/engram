package app

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
	"github.com/idolum-ai/engram/internal/tmux"
)

func TestRepliesRouteOnlyThroughLatestAlternateViews(t *testing.T) {
	dir := t.TempDir()
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
		s.SummaryMessageID = 88
		s.SnapshotMessageID = 89
		s.UpstreamMessageID = 90
		s.StaleAlternateMessageIDs = []int{86, 87}
	}); err != nil {
		t.Fatal(err)
	}

	var replies []string
	tg := telegram.New("TOKEN")
	tg.BaseURL = "https://api.telegram.org/botTOKEN"
	tg.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		replies = append(replies, body["text"].(string))
		return snapshotJSONResponse(`{"message_id":120,"chat":{"id":100}}`), nil
	})}
	runner := &slashEscapeRunner{}
	app := &App{
		Config: config.Config{TelegramAllowedUserID: 42, TelegramChatID: 100},
		Store:  store, Telegram: tg, Tmux: tmux.New(runner),
		summaryQueued: map[int]bool{}, summaryRunning: map[int]bool{}, summaryForce: map[int]bool{},
		sleepHook: func(time.Duration) {}, refreshHook: func(context.Context, int, bool) {},
	}

	for i, targetID := range []int{88, 89, 90} {
		status := app.handleUpdate(context.Background(), telegram.Update{Message: &telegram.Message{
			MessageID: 100 + i, Chat: telegram.Chat{ID: 100}, From: &telegram.User{ID: 42}, Text: "printf routed",
			ReplyToMessage: &telegram.Message{MessageID: targetID, Chat: telegram.Chat{ID: 100}},
		}})
		if status != "anchor_reply_ok" {
			t.Fatalf("current alternate %d status = %q", targetID, status)
		}
	}
	app.refreshWG.Wait()
	if len(runner.calls) != 12 {
		t.Fatalf("current alternates produced %d tmux calls, want 12: %#v", len(runner.calls), runner.calls)
	}

	for i, test := range []struct {
		targetID int
		text     string
	}{{86, "printf stale"}, {87, "//clear"}} {
		status := app.handleUpdate(context.Background(), telegram.Update{Message: &telegram.Message{
			MessageID: 110 + i, Chat: telegram.Chat{ID: 100}, From: &telegram.User{ID: 42}, Text: test.text,
			ReplyToMessage: &telegram.Message{MessageID: test.targetID, Chat: telegram.Chat{ID: 100}},
		}})
		if status != "anchor_reply_stale" {
			t.Fatalf("stale alternate %d status = %q", test.targetID, status)
		}
	}
	if len(runner.calls) != 12 {
		t.Fatalf("stale alternates reached tmux: %#v", runner.calls)
	}
	if len(replies) != 2 || !strings.Contains(replies[0], "no longer current") || !strings.Contains(replies[1], "no longer current") {
		t.Fatalf("stale replies = %#v", replies)
	}
}

func TestReplyToRetiredCollapsedAnchorNamesTheShelfRestoreAction(t *testing.T) {
	app, runner, id := newSafetyApp(t, state.TerminalOriginCreated)
	session, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.AnchorChatID = 100
		session.AnchorMessageID = 77
		session.AnchorFormat = anchorFormatText
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, committed, err := app.Store.CollapseSessionIntoShelf(id, session, state.CollapsedShelf{ChatID: 100, MessageID: 88}, ""); err != nil || !committed {
		t.Fatalf("collapse committed=%v err=%v", committed, err)
	}
	if _, retired, err := app.Store.FinishCollapsedAnchorRetirement(id, 100, 77); err != nil || !retired {
		t.Fatalf("retirement committed=%v err=%v", retired, err)
	}
	var reply string
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		reply, _ = body["text"].(string)
		return snapshotJSONResponse(`{"message_id":120,"chat":{"id":100}}`), nil
	})}
	app.Telegram = client
	app.Config.TelegramAllowedUserID = 42
	app.Config.TelegramChatID = 100

	status := app.handleUpdate(context.Background(), telegram.Update{Message: &telegram.Message{
		MessageID: 100, Chat: telegram.Chat{ID: 100}, From: &telegram.User{ID: 42}, Text: "must not route",
		ReplyToMessage: &telegram.Message{MessageID: 77, Chat: telegram.Chat{ID: 100}},
	}})
	if status != "anchor_reply_stale" || !strings.Contains(reply, "Collapsed sessions") || !strings.Contains(reply, "➕ Show") {
		t.Fatalf("status=%q reply=%q", status, reply)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("collapsed stale reply touched tmux: %#v", runner.calls)
	}
}

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
	ts = bindTestSession(t, store, ts.ID)
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
	if len(runner.calls) != 4 || runner.calls[0][0] != "display-message" || runner.calls[1][0] != "set-buffer" || runner.calls[1][4] != "/clear" || runner.calls[2][0] != "if-shell" || !strings.Contains(runner.calls[2][5], "paste-buffer -p -r -d") || runner.calls[3][0] != "if-shell" || !strings.Contains(runner.calls[3][5], "'Enter'") {
		t.Fatalf("tmux calls = %#v", runner.calls)
	}
	got, ok := store.FindSession(ts.ID)
	if !ok || got.LastActivityAt.IsZero() {
		t.Fatalf("session after input = %#v ok=%v", got, ok)
	}
	select {
	case <-refreshed:
	case <-time.After(time.Second):
		t.Fatal("slash input did not queue refresh")
	}
}

func TestPersistedUpstreamReplyAliasRoutesAfterRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	audit := filepath.Join(dir, "audit.jsonl")
	store, err := state.Open(path, audit)
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
		s.UpstreamMessageID = 90
		s.WatchEnabled = true
	}); err != nil {
		t.Fatal(err)
	}
	reopened, err := state.Open(path, audit)
	if err != nil {
		t.Fatal(err)
	}
	runner := &slashEscapeRunner{}
	a := &App{Config: config.Config{TelegramAllowedUserID: 42, TelegramChatID: 100}, Store: reopened, Tmux: tmux.New(runner), refreshHook: func(context.Context, int, bool) {}, sleepHook: func(time.Duration) {}}
	status := a.handleUpdate(context.Background(), telegram.Update{Message: &telegram.Message{
		MessageID: 101, Chat: telegram.Chat{ID: 100}, From: &telegram.User{ID: 42}, Text: "echo routed",
		ReplyToMessage: &telegram.Message{MessageID: 90, Chat: telegram.Chat{ID: 100}},
	}})
	a.refreshWG.Wait()
	if status != "anchor_reply_ok" || len(runner.calls) != 4 {
		t.Fatalf("status=%q calls=%#v", status, runner.calls)
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

func TestDeferredInputNoticeDoesNotReachReattachedBinding(t *testing.T) {
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"), filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.AllocateSession("main", "@1", "%1", "shell")
	if err != nil {
		t.Fatal(err)
	}
	session = bindTestSession(t, store, session.ID)
	if _, _, err := store.UpdateSession(session.ID, func(current *state.TerminalSession) {
		current.AnchorChatID = 100
		current.AnchorMessageID = 77
	}); err != nil {
		t.Fatal(err)
	}
	runner := &slashEscapeRunner{inputErr: context.DeadlineExceeded}
	app := &App{Store: store, Tmux: tmux.New(runner)}

	lock := app.sessionMutex(session.ID)
	lock.Lock()
	completion := app.sendInputExpectedLocked(context.Background(), session.ID, "input", "reply", true, &session)
	lock.Unlock()
	if completion.anchorNotice == "" {
		t.Fatal("tmux failure did not defer an anchor notice")
	}
	if _, _, err := store.UpdateSession(session.ID, func(current *state.TerminalSession) {
		current.TmuxPaneID = "%2"
		current.TmuxWindowID = "@2"
	}); err != nil {
		t.Fatal(err)
	}

	result := app.finishInput(context.Background(), session.ID, completion)
	if result.OK() {
		t.Fatal("failed tmux input reported success")
	}
	current, ok := store.FindSession(session.ID)
	if !ok || current.TmuxPaneID != "%2" || current.TmuxWindowID != "@2" {
		t.Fatalf("reattached binding changed by deferred notice: %#v ok=%v", current, ok)
	}
}

func TestReplyInputRejectsBindingChangedAfterTargetResolution(t *testing.T) {
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"), filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.AllocateSession("main", "@1", "%1", "shell")
	if err != nil {
		t.Fatal(err)
	}
	session = bindTestSession(t, store, session.ID)
	if _, _, err := store.UpdateSession(session.ID, func(current *state.TerminalSession) {
		current.TmuxPaneID = "%2"
		current.TmuxWindowID = "@2"
	}); err != nil {
		t.Fatal(err)
	}
	runner := &slashEscapeRunner{}
	app := &App{Store: store, Tmux: tmux.New(runner)}
	result := app.sendInputExpected(context.Background(), session.ID, "must not cross", "reply", true, &session)
	if result.OK() || result.Message != "session changed before input could be sent" {
		t.Fatalf("result = %#v", result)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("stale reply reached reattached pane: %#v", runner.calls)
	}
}

func TestCollapsedSessionRejectsDirectInputAndKeys(t *testing.T) {
	app, runner, id := newSafetyApp(t, state.TerminalOriginCreated)
	if _, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.Collapsed = true
	}); err != nil {
		t.Fatal(err)
	}
	input := app.sendInput(context.Background(), id, "must not cross", "command", true)
	keys := app.sendKeys(context.Background(), id, []string{"C-c"})
	if input.OK() || keys.OK() || !strings.Contains(input.Message, "Collapsed sessions") || !strings.Contains(keys.Message, "➕ Show") {
		t.Fatalf("input=%#v keys=%#v", input, keys)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("collapsed direct input touched tmux: %#v", runner.calls)
	}
}

func TestReplyInputRejectsAlternateRetiredAfterTargetResolution(t *testing.T) {
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"), filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.AllocateSession("main", "@1", "%1", "shell")
	if err != nil {
		t.Fatal(err)
	}
	session = bindTestSession(t, store, session.ID)
	if _, _, err := store.UpdateSession(session.ID, func(current *state.TerminalSession) {
		current.AnchorChatID = 100
		current.AnchorMessageID = 77
		current.SummaryMessageID = 88
	}); err != nil {
		t.Fatal(err)
	}
	expected, targetState, found := store.FindReplyTarget(100, 88)
	if !found || targetState != state.ReplyTargetCurrent {
		t.Fatal("initial alternate is not current")
	}
	if _, _, err := store.UpdateSession(session.ID, func(current *state.TerminalSession) {
		recordAlternateMessage(current, "summary", 89)
	}); err != nil {
		t.Fatal(err)
	}
	runner := &slashEscapeRunner{}
	app := &App{Store: store, Tmux: tmux.New(runner)}
	result := app.sendReplyInput(context.Background(), expected, 100, 88, "must not cross")
	if result.OK() || !strings.Contains(result.Message, "no longer current") {
		t.Fatalf("result = %#v", result)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("retired alternate reached tmux: %#v", runner.calls)
	}
}

type slashEscapeRunner struct {
	calls    [][]string
	inputErr error
}

func (r *slashEscapeRunner) Run(_ context.Context, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	if len(args) > 0 && args[0] == "show-options" {
		return appTestServerID + "\n", nil
	}
	if len(args) > 0 && args[0] == "display-message" {
		return framedTmuxBindingRecord("$1", "@1", "%1", "main", "0", "0", "1", "/tmp", "bash"), nil
	}
	if len(args) > 0 && args[0] == "if-shell" && r.inputErr != nil {
		return "", r.inputErr
	}
	return "", nil
}
