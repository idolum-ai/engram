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
ENGRAM_SNAPSHOT_BROWSER=/opt/chromium
ENGRAM_SNAPSHOT_THEME=contrast-dark
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
	if cfg.SnapshotBrowser != "/opt/chromium" {
		t.Fatalf("SnapshotBrowser = %q, want /opt/chromium", cfg.SnapshotBrowser)
	}
	if cfg.SnapshotTheme != "contrast-dark" {
		t.Fatalf("SnapshotTheme = %q, want contrast-dark", cfg.SnapshotTheme)
	}
}

func TestLoadDefaultsSnapshotTheme(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	if err := os.WriteFile(env, []byte(`
TELEGRAM_BOT_TOKEN=tg-token
TELEGRAM_ALLOWED_USER_ID=123
ANTHROPIC_API_KEY=anthropic-key
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(env)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SnapshotTheme != "terminal" {
		t.Fatalf("SnapshotTheme = %q, want terminal", cfg.SnapshotTheme)
	}
}

func TestLoadRejectsUnknownSnapshotTheme(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	if err := os.WriteFile(env, []byte(`
TELEGRAM_BOT_TOKEN=tg-token
TELEGRAM_ALLOWED_USER_ID=123
ANTHROPIC_API_KEY=anthropic-key
ENGRAM_SNAPSHOT_THEME=sepia
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(env); err == nil {
		t.Fatal("Load accepted unknown snapshot theme")
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

func TestLoadRejectsMalformedNumericConfig(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	if err := os.WriteFile(env, []byte(`
TELEGRAM_BOT_TOKEN=tg-token
TELEGRAM_ALLOWED_USER_ID=123
ANTHROPIC_API_KEY=anthropic-key
ENGRAM_ATTACHMENT_SOFT_MAX_BYTES=not-a-number
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(env); err == nil {
		t.Fatal("Load accepted malformed numeric config")
	}
}

func TestLoadRejectsWeakEnvPermissions(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	if err := os.WriteFile(env, []byte(`
TELEGRAM_BOT_TOKEN=tg-token
TELEGRAM_ALLOWED_USER_ID=123
ANTHROPIC_API_KEY=anthropic-key
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(env); err == nil {
		t.Fatal("Load accepted weak env permissions")
	}
}
