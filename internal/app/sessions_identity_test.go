package app

import (
	"context"
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
		return "main\t$1\t1\t1\n", nil
	case "list-panes":
		return "$1\t@1\t%1\tmain\t0\t0\t1\t/tmp\tbash\n", nil
	case "show-options":
		return appTestServerID + "\n", nil
	case "display-message":
		return "main\t0\t@1\tlegacy\t1\t%1\t/tmp\tbash\n", nil
	default:
		return "", nil
	}
}
