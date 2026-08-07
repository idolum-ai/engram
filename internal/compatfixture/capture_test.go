package compatfixture

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/idolum-ai/engram/internal/agentcompat"
)

func TestSanitizeFrameRetainsOnlyBoundedGrammar(t *testing.T) {
	input := "daniel@host ~/secret/repo % claude\nClaude Code v2.1.224\n│ private task for person@example.com 019f7607-c8b0-74b3-87ca-64a7e6e7ede0 │\nBrewed for 14m 46s\nclaude-opus-4-8 · manual mode on\n"
	got := SanitizeFrame(input)
	for _, secret := range []string{"daniel", "secret", "private task", "person@example.com", "019f7607", "manual mode"} {
		if strings.Contains(got, secret) {
			t.Fatalf("sanitized frame leaked %q:\n%s", secret, got)
		}
	}
	for _, structural := range []string{"Claude Code v2.1.224", "Brewed for 14m 46s", "model: claude-opus-4-8", "<redacted-line>"} {
		if !strings.Contains(got, structural) {
			t.Fatalf("sanitized frame omitted %q:\n%s", structural, got)
		}
	}
	if err := ValidateSanitized(got); err != nil {
		t.Fatal(err)
	}
}

func TestValidateSanitizedFailsClosedOnPrivateTokens(t *testing.T) {
	for _, value := range []string{"/Users/example/private\n", "person@example.com\n", "019f7607-c8b0-74b3-87ca-64a7e6e7ede0\n"} {
		if err := ValidateSanitized(value); err == nil {
			t.Fatalf("ValidateSanitized(%q) succeeded", value)
		}
	}
}

func TestCandidatePresentationDropsUnknownHookModelAliases(t *testing.T) {
	private := agentcompat.Presentation{Model: agentcompat.Value{Value: "private-repository-model", Provenance: agentcompat.ProvenanceHook}, Activity: "idle"}
	if got := safePresentation(private); got.Model.Value != "" || got.Activity != "idle" {
		t.Fatalf("safe presentation = %#v", got)
	}
	known := agentcompat.Presentation{Model: agentcompat.Value{Value: "claude-opus-4-8", Provenance: agentcompat.ProvenanceHook}}
	if got := safePresentation(known); got.Model.Value != "claude-opus-4-8" {
		t.Fatalf("known model was dropped: %#v", got)
	}
}

func TestInventoryTranscriptNeverEmitsContentValuesOrUnknownKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	data := `{"type":"private-project-type","userType":"external","provenance":"hook","sessionId":"019f7607-c8b0-74b3-87ca-64a7e6e7ede0","private-project-name":"secret","message":{"content":[{"type":"text","text":"private task"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := InventoryTranscript(path)
	if err != nil {
		t.Fatal(err)
	}
	encoded := strings.Join(append(append(append(got.RootKeys, got.RecordTypes...), got.Provenance...), got.ContentTypes...), " ")
	for _, secret := range []string{"secret", "private-project-name", "private task"} {
		if strings.Contains(encoded, secret) {
			t.Fatalf("inventory leaked %q: %#v", secret, got)
		}
	}
	if !strings.Contains(encoded, "<unknown-key>") || !strings.Contains(encoded, "<unknown-value>") || !strings.Contains(encoded, "text") || !strings.Contains(encoded, "external") || !strings.Contains(encoded, "hook") {
		t.Fatalf("inventory = %#v", got)
	}
}

func TestValidateOutputRefusesExistingAndGitWorktreePaths(t *testing.T) {
	root := t.TempDir()
	if _, err := validateOutput(root); err == nil {
		t.Fatal("existing directory accepted")
	}
	worktree := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(worktree, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := validateOutput(filepath.Join(worktree, "candidate")); err == nil {
		t.Fatal("Git worktree output accepted")
	}
	if got, err := validateOutput(filepath.Join(root, "candidate")); err != nil || got == "" {
		t.Fatalf("safe output = %q, %v", got, err)
	}
}
