package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/state"
)

func TestAuditRedactsConfiguredSecrets(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.jsonl")
	store, err := state.Open(filepath.Join(dir, "state.json"), auditPath)
	if err != nil {
		t.Fatal(err)
	}
	app := &App{
		Config: config.Config{
			TelegramBotToken:        "tg-secret-token",
			TelegramAllowedUserID:   42000001,
			TelegramOperatorUserIDs: []int64{77000001, 88000001},
			TelegramChatID:          -1001234567890,
			AnthropicAPIKey:         "anthropic-secret-key",
			OpenAIAPIKey:            "openai-secret-key",
		},
		Store: store,
	}

	err = app.audit("telegram.anchor_html", "failed", map[string]any{
		"error": "Post \"https://api.telegram.org/bottg-secret-token/editMessageText\": context canceled",
		"nested": []any{
			"anthropic-secret-key",
			map[string]any{"env": "ANTHROPIC_API_KEY=anthropic-secret-key OPENAI_API_KEY=openai-secret-key users=42000001,77000001,88000001 chat=-1001234567890"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, secret := range []string{"tg-secret-token", "anthropic-secret-key", "openai-secret-key"} {
		if strings.Contains(got, secret) {
			t.Fatalf("audit log contains secret %q: %s", secret, got)
		}
	}
	if !strings.Contains(got, "redacted") {
		t.Fatalf("audit log was not redacted: %s", got)
	}
	for _, identifier := range []string{"42000001", "77000001", "88000001", "-1001234567890"} {
		if !strings.Contains(got, identifier) {
			t.Fatalf("global redactor removed numeric routing identifier %q: %s", identifier, got)
		}
	}
}

func TestClaudeContextAuditIsBoundedPrivateAndCoalesced(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.jsonl")
	store, err := state.Open(filepath.Join(dir, "state.json"), auditPath)
	if err != nil {
		t.Fatal(err)
	}
	app := &App{Store: store}
	app.recordSessionContextDecision(42, "claude", "2.1.223", "applied", "visible_messages", "claude-transcript-v1", 4, true)
	app.recordSessionContextDecision(42, "claude", "2.1.223", "applied", "visible_messages", "claude-transcript-v1", 4, true)

	b, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if strings.Count(got, `"type":"terminal.claude_context"`) != 1 {
		t.Fatalf("Claude context decision was not coalesced: %s", got)
	}
	for _, want := range []string{`"program":"claude"`, `"version":"2.1.223"`, `"reason":"visible_messages"`, `"parser":"claude-transcript-v1"`, `"messages":4`, `"diagram":true`} {
		if !strings.Contains(got, want) {
			t.Fatalf("Claude context audit omitted %s: %s", want, got)
		}
	}
	for _, forbidden := range []string{"session_id\":\"", "transcript_path", "cwd", "message_text"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("Claude context audit contains forbidden locator/content field %q: %s", forbidden, got)
		}
	}
}
