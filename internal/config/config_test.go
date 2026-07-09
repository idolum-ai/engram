package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadValidatesAndDefaults(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	if err := os.WriteFile(env, []byte(`
TELEGRAM_BOT_TOKEN=tg-token
TELEGRAM_ALLOWED_USER_ID=123
LLM_PROVIDER=anthropic
ANTHROPIC_API_KEY=anthropic-key
ANTHROPIC_MODEL=claude-haiku-4-5-20251001
ENGRAM_TMUX_SESSION=main
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(env)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TelegramAllowedUserID != 123 || cfg.TelegramChatID != 123 {
		t.Fatalf("ids = %d %d", cfg.TelegramAllowedUserID, cfg.TelegramChatID)
	}
	if cfg.Home == "" || cfg.Workdir == "" || cfg.AttachmentSoftMaxBytes != DefaultSoftMaxSize {
		t.Fatalf("defaults not applied: %#v", cfg)
	}
	if cfg.TmuxSession != "main" {
		t.Fatalf("TmuxSession = %q, want main", cfg.TmuxSession)
	}
}

func TestLoadRejectsNonHaiku(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	if err := os.WriteFile(env, []byte(`
TELEGRAM_BOT_TOKEN=tg-token
TELEGRAM_ALLOWED_USER_ID=123
LLM_PROVIDER=anthropic
ANTHROPIC_API_KEY=anthropic-key
ANTHROPIC_MODEL=claude-sonnet-4-20250514
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(env); err == nil {
		t.Fatal("Load accepted non-Haiku model")
	}
}
