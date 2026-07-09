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
			TelegramBotToken: "tg-secret-token",
			AnthropicAPIKey:  "anthropic-secret-key",
		},
		Store: store,
	}

	err = app.audit("telegram.anchor_html", "failed", map[string]any{
		"error": "Post \"https://api.telegram.org/bottg-secret-token/editMessageText\": context canceled",
		"nested": []any{
			"anthropic-secret-key",
			map[string]any{"env": "ANTHROPIC_API_KEY=anthropic-secret-key"},
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
	if strings.Contains(got, "tg-secret-token") || strings.Contains(got, "anthropic-secret-key") {
		t.Fatalf("audit log contains secret: %s", got)
	}
	if !strings.Contains(got, "redacted") {
		t.Fatalf("audit log was not redacted: %s", got)
	}
}
