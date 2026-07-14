package app

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
	"github.com/idolum-ai/engram/internal/tmux"
)

func TestSessionsOffersExplicitReattachForLegacyBinding(t *testing.T) {
	store, legacy := legacyBindingStore(t)
	app := &App{Store: store, Tmux: tmux.New(identitySessionRunner{})}
	var output strings.Builder
	targets := app.writeTmuxSessions(context.Background(), &output)
	if !strings.Contains(output.String(), "reattach:[1]") || len(targets) != 1 || targets[0].Target != legacy.TmuxPaneID {
		t.Fatalf("sessions output=%q targets=%#v", output.String(), targets)
	}
}

func TestAttachExplicitlyRebindsLegacyIdentityWithoutKillAuthority(t *testing.T) {
	store, legacy := legacyBindingStore(t)
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return snapshotJSONResponse(`{"message_id":80,"chat":{"id":100}}`), nil
	})}
	app := &App{
		Config: config.Config{TelegramChatID: 100}, Store: store, Telegram: client,
		Tmux: tmux.New(identitySessionRunner{}), refreshHook: func(context.Context, int, bool) {},
		summaryQueued: map[int]bool{}, summaryRunning: map[int]bool{}, summaryForce: map[int]bool{}, sleepHook: func(time.Duration) {},
	}
	result := app.attachTarget(context.Background(), telegram.Message{MessageID: 70, Chat: telegram.Chat{ID: 100}}, legacy.TmuxPaneID)
	app.refreshWG.Wait()
	if !result.OK() {
		t.Fatalf("attach result = %#v", result)
	}
	got, ok := store.FindSession(legacy.ID)
	if !ok || got.TmuxServerID != appTestServerID || got.Origin != state.TerminalOriginAttached || got.State != state.TerminalRunning || !got.WatchEnabled {
		t.Fatalf("rebound session = %#v ok=%v", got, ok)
	}
}

func TestReattachWaitsForAnchorDeliveryAndNeutralizesOldView(t *testing.T) {
	store, session := legacyBindingStore(t)
	if _, _, err := store.UpdateSession(session.ID, func(current *state.TerminalSession) {
		current.TmuxServerID = "abcdef0123456789abcdef0123456789"
		current.AnchorChatID = 100
		current.AnchorMessageID = 77
		current.AnchorFormat = "text"
		current.State = state.TerminalLost
		current.SummaryMessageID = 74
		current.SnapshotMessageID = 75
		current.UpstreamMessageID = 76
		current.LastRawCapture = "old frame"
		current.LastSnapshotAttemptAt = time.Now().UTC()
		current.SeenUpstreamSignalIDs = []string{"old-signal"}
		current.LastUpstreamSignalAt = time.Now().UTC()
		current.UpstreamRetryAt = time.Now().UTC().Add(time.Minute)
	}); err != nil {
		t.Fatal(err)
	}
	var paths []string
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.Path)
		return snapshotJSONResponse(`{"message_id":80,"chat":{"id":100}}`), nil
	})}
	app := &App{
		Config: config.Config{TelegramChatID: 100}, Store: store, Telegram: client,
		Tmux: tmux.New(identitySessionRunner{}), refreshHook: func(context.Context, int, bool) {},
		summaryQueued: map[int]bool{}, summaryRunning: map[int]bool{}, summaryForce: map[int]bool{}, sleepHook: func(time.Duration) {},
	}
	app.signalRetries.Store(session.ID, time.Now().UTC().Add(time.Minute))
	held := app.anchorMutex(session.ID)
	held.Lock()
	done := make(chan actionResult, 1)
	go func() {
		done <- app.attachTarget(context.Background(), telegram.Message{MessageID: 70, Chat: telegram.Chat{ID: 100}}, session.TmuxPaneID)
	}()
	select {
	case result := <-done:
		t.Fatalf("reattach crossed in-flight anchor delivery: %#v", result)
	case <-time.After(20 * time.Millisecond):
	}
	held.Unlock()
	result := <-done
	app.refreshWG.Wait()
	if !result.OK() || len(paths) < 2 || paths[0] != "/botTOKEN/editMessageText" {
		t.Fatalf("result=%#v paths=%#v", result, paths)
	}
	got, _ := store.FindSession(session.ID)
	if got.TmuxServerID != appTestServerID || got.LastSummary != "" || got.LastRawCapture != "" || !got.LastSnapshotAttemptAt.IsZero() || len(got.SeenUpstreamSignalIDs) != 0 || !got.LastUpstreamSignalAt.IsZero() || !got.UpstreamRetryAt.IsZero() || got.SummaryMessageID != 0 || got.SnapshotMessageID != 0 || got.UpstreamMessageID != 0 || !reflect.DeepEqual(got.StaleAlternateMessageIDs, []int{74, 75, 76}) {
		t.Fatalf("reattached session = %#v", got)
	}
	if _, ok := app.signalRetries.Load(session.ID); ok {
		t.Fatal("reattach retained process-local upstream retry deadline")
	}
	for _, messageID := range []int{74, 75, 76} {
		if routed, targetState, ok := store.FindReplyTarget(100, messageID); !ok || targetState != state.ReplyTargetStale || routed.ID != session.ID {
			t.Fatalf("retired alternate %d target = %#v %q ok=%v", messageID, routed, targetState, ok)
		}
	}
}

func TestAttachPreservesImmutableIdentityThroughFirstInput(t *testing.T) {
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"), filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	runner := &attachSendRunner{}
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return snapshotJSONResponse(`{"message_id":80,"chat":{"id":100}}`), nil
	})}
	app := &App{
		Config: config.Config{TelegramChatID: 100}, Store: store, Telegram: client, Tmux: tmux.New(runner),
		refreshHook: func(context.Context, int, bool) {}, summaryQueued: map[int]bool{}, summaryRunning: map[int]bool{}, summaryForce: map[int]bool{}, sleepHook: func(time.Duration) {},
	}
	result := app.attachTarget(context.Background(), telegram.Message{MessageID: 70, Chat: telegram.Chat{ID: 100}}, "main:openclaw")
	if !result.OK() {
		t.Fatalf("attach result = %#v", result)
	}
	if len(runner.calls) < 3 || runner.calls[0][0] != "show-options" || runner.calls[1][0] != "display-message" || runner.calls[2][0] != "show-options" {
		t.Fatalf("attach did not bracket target resolution with server identity: %#v", runner.calls)
	}
	sessions := store.Snapshot().TerminalSessions
	if len(sessions) != 1 || sessions[0].TmuxWindowID != "@17" || sessions[0].TmuxPaneID != "%23" {
		t.Fatalf("attached session = %#v", sessions)
	}
	result = app.sendInput(context.Background(), sessions[0].ID, "echo exact", "command", true)
	if !result.OK() {
		t.Fatalf("send result = %#v", result)
	}
	if !runner.sentLiteral || !runner.sentEnter {
		t.Fatalf("send calls did not target immutable pane: %#v", runner.calls)
	}
}

func TestAttachRejectsTmuxRestartDuringResolution(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	runner := &attachRestartRunner{}
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return snapshotJSONResponse(`{"message_id":81,"chat":{"id":100}}`), nil
	})}
	app := &App{Config: config.Config{TelegramChatID: 100}, Store: store, Telegram: client, Tmux: tmux.New(runner)}
	result := app.attachTarget(context.Background(), telegram.Message{MessageID: 70, Chat: telegram.Chat{ID: 100}}, "main:openclaw")
	if result.OK() || result.Message != "tmux changed while attaching" {
		t.Fatalf("attach result = %#v", result)
	}
	if sessions := store.Snapshot().TerminalSessions; len(sessions) != 0 {
		t.Fatalf("tmux restart persisted a binding: %#v", sessions)
	}
}

func legacyBindingStore(t *testing.T) (*state.Store, state.TerminalSession) {
	t.Helper()
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.AllocateSession("main", "@1", "%1", "legacy")
	if err != nil {
		t.Fatal(err)
	}
	return store, session
}

type identitySessionRunner struct{}

func (identitySessionRunner) Run(_ context.Context, args ...string) (string, error) {
	switch args[0] {
	case "list-sessions":
		return framedTmuxRecord("main", "$1", "1", "1"), nil
	case "list-panes":
		return framedTmuxRecord("$1", "@1", "%1", "main", "0", "0", "1", "/tmp", "bash"), nil
	case "show-options":
		return appTestServerID + "\n", nil
	case "display-message":
		return framedTmuxRecord("$1", "main", "0", "@1", "legacy", "1", "%1", "/tmp", "bash"), nil
	default:
		return "", nil
	}
}

type attachSendRunner struct {
	calls       [][]string
	sentLiteral bool
	sentEnter   bool
}

type attachRestartRunner struct{ showCalls int }

func (r *attachRestartRunner) Run(_ context.Context, args ...string) (string, error) {
	switch args[0] {
	case "show-options":
		r.showCalls++
		if r.showCalls == 1 {
			return appTestServerID + "\n", nil
		}
		return "abcdef0123456789abcdef0123456789\n", nil
	case "display-message":
		return framedTmuxRecord("$11", "main", "4", "@17", "openclaw", "1", "%23", "/tmp", "bash"), nil
	default:
		return "", fmt.Errorf("unexpected tmux call: %v", args)
	}
}

func (r *attachSendRunner) Run(_ context.Context, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	switch args[0] {
	case "show-options":
		return appTestServerID + "\n", nil
	case "display-message":
		format := args[len(args)-1]
		if strings.Contains(format, "window_active") {
			return framedTmuxRecord("$11", "main", "4", "@17", "openclaw", "1", "%23", "/tmp", "bash"), nil
		}
		return framedTmuxBindingRecord("$11", "@17", "%23", "main", "4", "0", "1", "/tmp", "bash"), nil
	case "send-keys":
		if len(args) >= 6 && args[2] == "%23" && args[3] == "-l" && args[5] == "echo exact" {
			r.sentLiteral = true
		} else if len(args) == 4 && args[2] == "%23" && args[3] == "Enter" {
			r.sentEnter = true
		} else {
			return "", fmt.Errorf("unexpected send-keys: %v", args)
		}
		return "", nil
	case "set-buffer":
		if len(args) == 5 && args[4] == "echo exact" {
			r.sentLiteral = true
		}
		return "", nil
	case "if-shell":
		if len(args) > 5 && strings.Contains(args[5], "'Enter'") {
			r.sentEnter = true
		}
		return "", nil
	default:
		return "", fmt.Errorf("unexpected tmux call: %v", args)
	}
}
