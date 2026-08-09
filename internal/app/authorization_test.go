package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/lockfile"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
)

func TestTelegramAuthorizationRolesFailClosed(t *testing.T) {
	app := &App{Config: multiUserTelegramConfig()}
	group := telegram.Chat{ID: -1001234567890, Type: "supergroup"}
	tests := []struct {
		name string
		msg  *telegram.Message
		want telegramRole
	}{
		{name: "administrator", msg: &telegram.Message{Chat: group, From: &telegram.User{ID: 42}}, want: telegramAdministrator},
		{name: "operator", msg: &telegram.Message{Chat: group, From: &telegram.User{ID: 77}}, want: telegramOperator},
		{name: "unlisted", msg: &telegram.Message{Chat: group, From: &telegram.User{ID: 99}}, want: telegramUnauthorized},
		{name: "another chat", msg: &telegram.Message{Chat: telegram.Chat{ID: -1009, Type: "supergroup"}, From: &telegram.User{ID: 42}}, want: telegramUnauthorized},
		{name: "incoherent private chat", msg: &telegram.Message{Chat: telegram.Chat{ID: group.ID, Type: "private"}, From: &telegram.User{ID: 42}}, want: telegramUnauthorized},
		{name: "anonymous sender", msg: &telegram.Message{Chat: group, SenderChat: &group}, want: telegramUnauthorized},
		{name: "anonymous administrator", msg: &telegram.Message{Chat: group, From: &telegram.User{ID: 42}, SenderChat: &group}, want: telegramUnauthorized},
		{name: "missing sender", msg: &telegram.Message{Chat: group}, want: telegramUnauthorized},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := app.messageRole(test.msg); got != test.want {
				t.Fatalf("message role = %d, want %d", got, test.want)
			}
		})
	}
	dm := &App{Config: config.Config{TelegramAllowedUserID: 42, TelegramChatID: 42}}
	if role := dm.messageRole(&telegram.Message{Chat: telegram.Chat{ID: 42, Type: "private"}, From: &telegram.User{ID: 42}}); role != telegramAdministrator {
		t.Fatalf("backward-compatible DM role = %d", role)
	}
}

func TestTelegramCallbackAuthorizationAllowsOperatorsOnlyInConfiguredGroup(t *testing.T) {
	app := newAuthorizationTestApp(t)
	groupMessage := &telegram.Message{MessageID: 10, Chat: telegram.Chat{ID: app.Config.TelegramChatID, Type: "group"}}
	operator := telegram.CallbackQuery{ID: "operator", From: telegram.User{ID: 77}, Message: groupMessage, Data: "unknown:value"}
	if status := app.handleCallback(context.Background(), operator); status != "skipped_unknown_callback" {
		t.Fatalf("operator callback status = %q", status)
	}
	unlisted := operator
	unlisted.ID = "unlisted"
	unlisted.From.ID = 99
	if status := app.handleCallback(context.Background(), unlisted); status != "rejected_unauthorized_callback" {
		t.Fatalf("unlisted callback status = %q", status)
	}
	otherChat := operator
	otherChat.ID = "other-chat"
	otherChat.Message = &telegram.Message{MessageID: 10, Chat: telegram.Chat{ID: -1009, Type: "group"}}
	if status := app.handleCallback(context.Background(), otherChat); status != "rejected_unauthorized_callback" {
		t.Fatalf("other-chat callback status = %q", status)
	}
}

func TestAdministratorOnlyTelegramControls(t *testing.T) {
	app := newAuthorizationTestApp(t)
	group := telegram.Chat{ID: app.Config.TelegramChatID, Type: "supergroup"}
	operatorMessage := telegram.Message{MessageID: 1, Chat: group, From: &telegram.User{ID: 77}, Text: "/restart"}
	if status := app.handleCommand(context.Background(), operatorMessage, operatorMessage.Text); status != "command_unauthorized" {
		t.Fatalf("operator restart status = %q", status)
	}
	select {
	case <-app.stopCh:
		t.Fatal("operator stopped Engram")
	default:
	}

	pending := &githubPendingRequest{
		ID: "request", ExpiresAt: time.Now().Add(time.Minute), ApprovalMessageID: 50,
		State: "pending", Result: make(chan githubApproval, 1),
	}
	app.githubPending = map[string]*githubPendingRequest{pending.ID: pending}
	operatorCallback := telegram.CallbackQuery{From: telegram.User{ID: 77}, Message: &telegram.Message{MessageID: 50, Chat: group}}
	for _, action := range []string{"github-approve", "github-deny"} {
		operatorCallback.ID = "operator-" + action
		operatorCallback.Data = action + ":" + pending.ID
		if status := app.handleCallback(context.Background(), operatorCallback); status != "rejected_unauthorized_callback" || pending.State != "pending" {
			t.Fatalf("operator %s callback status=%q state=%q", action, status, pending.State)
		}
	}
	operatorUnlock := telegram.Message{
		MessageID: 52, Chat: group, From: &telegram.User{ID: 77}, Text: "not-the-secret",
		ReplyToMessage: &telegram.Message{MessageID: 51, Chat: group},
	}
	pending.State = "unlocking"
	pending.UnlockMessageID = 51
	if status, handled := app.handleAuthorizedGitHubUnlockReply(context.Background(), operatorUnlock); !handled || status != "github_unlock_unauthorized" || pending.State != "unlocking" {
		t.Fatalf("operator unlock reply status=%q handled=%v state=%q", status, handled, pending.State)
	}

	adminCallback := operatorCallback
	adminCallback.ID = "admin-deny"
	adminCallback.From.ID = 42
	adminCallback.Data = "github-deny:" + pending.ID
	if status := app.handleCallback(context.Background(), adminCallback); status != "callback_ok" || pending.State != "resolved" {
		t.Fatalf("administrator GitHub callback status=%q state=%q", status, pending.State)
	}

	adminMessage := operatorMessage
	adminMessage.MessageID = 2
	adminMessage.From = &telegram.User{ID: 42}
	if status := app.handleCommand(context.Background(), adminMessage, adminMessage.Text); status != "command_ok" {
		t.Fatalf("administrator restart status = %q", status)
	}
	select {
	case <-app.stopCh:
	default:
		t.Fatal("administrator restart did not stop Engram")
	}
}

func TestUnauthorizedJournalAndAuditDoNotRetainIdentifiers(t *testing.T) {
	app := newAuthorizationTestApp(t)
	update := telegram.Update{UpdateID: 1, Message: &telegram.Message{
		MessageID: 987654, Chat: telegram.Chat{ID: -100999999, Type: "supergroup"},
		From: &telegram.User{ID: 123456789}, Text: "hello",
	}}
	kind, refs := app.updateJournalRefs(update)
	if kind != "message" || refs != (state.UpdateRefs{}) {
		t.Fatalf("unauthorized journal refs = %#v", refs)
	}
	if status := app.handleUpdate(context.Background(), update); status != "rejected_unauthorized" {
		t.Fatalf("unauthorized update status = %q", status)
	}
	channel := telegram.Update{UpdateID: 2, ChannelPost: update.Message}
	kind, refs = app.updateJournalRefs(channel)
	if kind != "channel_post" || refs != (state.UpdateRefs{}) || app.handleUpdate(context.Background(), channel) != "rejected_unauthorized" {
		t.Fatalf("channel post admission kind=%q refs=%#v", kind, refs)
	}
	b, err := os.ReadFile(app.Config.AuditPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "123456789") || strings.Contains(string(b), "-100999999") || strings.Contains(string(b), "987654") {
		t.Fatalf("audit retained rejected identifiers: %s", b)
	}
	group := telegram.Chat{ID: app.Config.TelegramChatID, Type: "supergroup"}
	operatorCallback := telegram.Update{CallbackQuery: &telegram.CallbackQuery{
		From: telegram.User{ID: 77}, Message: &telegram.Message{MessageID: 50, Chat: group},
	}}
	if kind, refs := app.updateJournalRefs(operatorCallback); kind != "callback_query" || refs.UserID != 77 || refs.ChatID != group.ID || refs.MessageID != 50 {
		t.Fatalf("operator callback journal refs = %#v kind=%q", refs, kind)
	}
	operatorCallback.CallbackQuery.From.ID = 99
	if kind, refs := app.updateJournalRefs(operatorCallback); kind != "callback_query" || refs != (state.UpdateRefs{}) {
		t.Fatalf("unlisted callback journal refs = %#v kind=%q", refs, kind)
	}
}

func TestTelegramPollingIdentityIgnoresLocalAuthorizationConfig(t *testing.T) {
	base := multiUserTelegramConfig()
	base.TelegramBotToken = "123456789:high-entropy-bot-token"
	base.TelegramAPIBase = "https://telegram.example.invalid/root/"
	changed := base
	changed.TelegramAllowedUserID = 900
	changed.TelegramOperatorUserIDs = []int64{901, 902, 903}
	changed.TelegramChatID = -100777
	if telegramPollingLockKey(base) != telegramPollingLockKey(changed) || telegramPollingIdentity(base) != telegramPollingIdentity(changed) {
		t.Fatal("local authorization changes changed the Telegram polling identity")
	}
	differentAPI := base
	differentAPI.TelegramAPIBase = "https://other.example.invalid"
	if telegramPollingLockKey(base) == telegramPollingLockKey(differentAPI) {
		t.Fatal("different effective Telegram API bases shared a polling identity")
	}
	differentBot := base
	differentBot.TelegramBotToken += "-other"
	if telegramPollingLockKey(base) == telegramPollingLockKey(differentBot) {
		t.Fatal("different Telegram bots shared a polling identity")
	}
	metadata := fmt.Sprintf("%#v", telegramPollingLockMetadata(base))
	for _, forbidden := range []string{base.TelegramBotToken, "42", "77", "88", fmt.Sprint(base.TelegramChatID)} {
		if strings.Contains(metadata, forbidden) {
			t.Fatalf("polling lock metadata exposed %q: %s", forbidden, metadata)
		}
	}
	if !strings.Contains(metadata, telegramPollingIdentity(base)) {
		t.Fatalf("polling lock metadata omitted derived identity: %s", metadata)
	}

	dir := t.TempDir()
	lock, err := lockfile.Acquire(dir, telegramPollingLockKey(base), telegramPollingLockMetadata(base))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	onDisk, err := os.ReadFile(lock.Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(onDisk), base.TelegramBotToken) {
		t.Fatalf("polling lock file exposed token material: %s", onDisk)
	}
}

func TestNewPreventsSameBotPollingAcrossHomesAndUserLists(t *testing.T) {
	firstHome := t.TempDir()
	secondHome := t.TempDir()
	base := config.Config{
		TelegramBotToken:      "same-bot-for-cross-home-lock-test",
		TelegramAllowedUserID: 42,
		TelegramChatID:        42,
		AnchorMode:            config.AnchorModeSnapshot,
		Home:                  firstHome,
		Workdir:               firstHome,
		SnapshotBrowser:       filepath.Join(firstHome, "missing-chromium"),
		SnapshotTheme:         "terminal",
	}
	first, err := New(base)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	changed := base
	changed.Home = secondHome
	changed.Workdir = secondHome
	changed.TelegramAllowedUserID = 900
	changed.TelegramOperatorUserIDs = []int64{901}
	changed.TelegramChatID = -100777
	second, err := New(changed)
	if second != nil || err == nil || !strings.Contains(err.Error(), "another Engram process") {
		t.Fatalf("second app=%#v error=%v", second, err)
	}
}

func TestOperatorCannotConsumeAnotherUsersKeyForceReply(t *testing.T) {
	app, _, _ := newAnchorKeyTestApp(t)
	app.Config = multiUserTelegramConfig()
	session, _ := app.Store.FindSession(1)
	ref := keyPromptRef{ChatID: app.Config.TelegramChatID, MessageID: 71}
	if _, err := app.issueKeyPrompt(ref.ChatID, ref.MessageID, 77, session); err != nil {
		t.Fatal(err)
	}
	if _, current, recognized, owned := app.consumeKeyPrompt(ref, 88); !recognized || current || owned {
		t.Fatalf("other operator prompt lookup current=%v recognized=%v owned=%v", current, recognized, owned)
	}
	if _, current, recognized, owned := app.consumeKeyPrompt(ref, 77); !recognized || !current || !owned {
		t.Fatalf("initiating operator prompt lookup current=%v recognized=%v owned=%v", current, recognized, owned)
	}
}

func newAuthorizationTestApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := multiUserTelegramConfig()
	cfg.Home = dir
	client := telegram.New("TOKEN")
	client.HTTPClient = &http.Client{Transport: authorizationRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":true}`)),
		}, nil
	})}
	return &App{Config: cfg, Store: store, Telegram: client, stopCh: make(chan struct{}), githubUnlockTombstones: map[int]time.Time{}}
}

func multiUserTelegramConfig() config.Config {
	return config.Config{
		TelegramAllowedUserID: 42, TelegramOperatorUserIDs: []int64{77, 88},
		TelegramChatID: -1001234567890,
	}
}

type authorizationRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn authorizationRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}
