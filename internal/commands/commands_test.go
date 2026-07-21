package commands

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAllCommandMetadataIsCompleteAndUnique(t *testing.T) {
	t.Parallel()

	seen := map[string]bool{}
	for _, meta := range All() {
		if strings.TrimSpace(meta.Command) == "" {
			t.Fatalf("command metadata has empty command: %#v", meta)
		}
		if strings.TrimSpace(meta.Usage) == "" {
			t.Fatalf("command %q has empty usage", meta.Command)
		}
		if !strings.HasPrefix(meta.Usage, "/"+meta.Command) {
			t.Fatalf("command %q usage = %q, want leading /%s", meta.Command, meta.Usage, meta.Command)
		}
		if strings.TrimSpace(meta.Description) == "" {
			t.Fatalf("command %q has empty description", meta.Command)
		}
		if strings.TrimSpace(meta.Category) == "" {
			t.Fatalf("command %q has empty category", meta.Command)
		}
		if seen[meta.Command] {
			t.Fatalf("duplicate command metadata for %q", meta.Command)
		}
		seen[meta.Command] = true
	}
}

func TestJSONExportsRegistry(t *testing.T) {
	t.Parallel()

	data, err := JSON()
	if err != nil {
		t.Fatal(err)
	}
	var metas []Metadata
	if err := json.Unmarshal(data, &metas); err != nil {
		t.Fatal(err)
	}
	if len(metas) != len(All()) {
		t.Fatalf("JSON exported %d commands, want %d", len(metas), len(All()))
	}
	if _, ok := Find("run"); ok {
		t.Fatal("Find(run) exposed a hidden compatibility alias")
	}
}

func TestHelpTextIncludesPublicCommands(t *testing.T) {
	t.Parallel()

	text := HelpText()
	for _, want := range []string{"/help", "/status", "/sessions", "/attach <tmux-target>", "/templates", "/download <absolute-path>", "//clear sends /clear"} {
		if !strings.Contains(text, want) {
			t.Fatalf("HelpText() missing %q:\n%s", want, text)
		}
	}
	for _, hidden := range []string{"/commands", "/version", "/quit", "/run", "/type", "/stop", "/kill"} {
		if strings.Contains(text, hidden) {
			t.Fatalf("HelpText() includes non-canonical command %s:\n%s", hidden, text)
		}
	}
}

func TestBotCommandsExcludeReservedCommands(t *testing.T) {
	t.Parallel()

	for _, meta := range BotCommands() {
		if strings.Contains(meta.Command, "-") {
			t.Fatalf("BotCommands() includes Telegram-invalid command %q", meta.Command)
		}
	}
}
