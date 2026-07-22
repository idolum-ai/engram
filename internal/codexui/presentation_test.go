package codexui

import (
	"strings"
	"testing"
)

func TestPresentRemovesObservedCodexChromeAndKeepsConversation(t *testing.T) {
	input := strings.Join([]string{
		"• Ran go test ./...",
		"  └ ok example/internal/app",
		"",
		"⚠ Automatic approval review approved (risk: low): This is a bounded test",
		"  requested by the user.",
		"",
		"✔ Auto-reviewer approved codex to run go test ./... this time",
		"",
		"────────────────────────────────────────────────────────",
		"",
		"• Tests pass and the implementation is complete.",
		"",
		"─ Worked for 1m 08s ────────────────────────────────────",
		"",
		"› Write tests for @filename",
		"",
		"• Working (34s • esc to interrupt)",
		"",
		"gpt-5.6-sol high · ~/code/github.com/idolum-ai · Main [default]",
	}, "\n")
	got := Present(Runtime{Detected: true, Supported: true, Version: SupportedVersion}, input)
	if !got.Applied || got.Model != "gpt-5.6-sol" || got.Effort != "high" || got.Activity != "working" {
		t.Fatalf("presentation metadata = %#v", got)
	}
	for _, noise := range []string{"Automatic approval", "Auto-reviewer", "Worked for", "Write tests for", "Working (", "gpt-5.6-sol", "────"} {
		if strings.Contains(got.Text, noise) {
			t.Fatalf("cleaned text retained %q: %q", noise, got.Text)
		}
	}
	for _, evidence := range []string{"Ran go test ./...", "ok example/internal/app", "Tests pass and the implementation is complete."} {
		if !strings.Contains(got.Text, evidence) {
			t.Fatalf("cleaned text dropped %q: %q", evidence, got.Text)
		}
	}
}

func TestPresentExtractsSupportedModelSwitchNotice(t *testing.T) {
	input := strings.Join([]string{
		"• The current operation is waiting.",
		"",
		"⚠ Switch to the fast model while this request receives additional security review.",
		"",
		"gpt-5.6-sol high · ~/code/github.com/idolum-ai",
	}, "\n")
	got := Present(Runtime{Detected: true, Supported: true, Version: SupportedVersion}, input)
	if !got.Applied || got.Activity != "idle" || !strings.Contains(got.Notice, "Switch to the fast model") || strings.Contains(got.Text, "Switch to") {
		t.Fatalf("presentation = %#v", got)
	}
}

func TestPresentDoesNotPromoteStaleModelSwitchNotice(t *testing.T) {
	lines := []string{"⚠ Switch to the fast model while this request receives additional security review.", ""}
	for i := 0; i < 20; i++ {
		lines = append(lines, "• Historical result line")
	}
	lines = append(lines, "", "gpt-5.6-sol high · /work")
	input := strings.Join(lines, "\n")
	got := Present(Runtime{Detected: true, Supported: true, Version: SupportedVersion}, input)
	if !got.Applied || got.Notice != "" || !strings.Contains(got.Text, "Switch to the fast model") {
		t.Fatalf("presentation = %#v", got)
	}
}

func TestPresentPreservesSemanticWorkedForCollision(t *testing.T) {
	input := strings.Join([]string{
		"Worked for Acme ─ keep this evidence",
		"",
		"─ Worked for 1m 08s ─────────────────────",
		"",
		"gpt-5.6-sol high · /work",
	}, "\n")
	got := Present(Runtime{Detected: true, Supported: true, Version: SupportedVersion}, input)
	if !got.Applied || !strings.Contains(got.Text, "Worked for Acme ─ keep this evidence") || strings.Contains(got.Text, "Worked for 1m 08s") {
		t.Fatalf("presentation = %#v", got)
	}
}

func TestPresentPreservesUnknownInputAndUnsupportedVersionsExactly(t *testing.T) {
	tests := []struct {
		name    string
		runtime Runtime
		text    string
	}{
		{name: "not detected", text: "ordinary terminal\ngpt-5.6-sol high · /tmp"},
		{name: "unsupported", runtime: Runtime{Detected: true, Version: "0.145.0"}, text: "answer\ngpt-5.6-sol high · /tmp"},
		{name: "unknown layout", runtime: Runtime{Detected: true, Supported: true, Version: SupportedVersion}, text: "answer\nfuture codex footer"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := Present(test.runtime, test.text)
			if got.Applied || got.Text != test.text {
				t.Fatalf("fallback = %#v, want exact %q", got, test.text)
			}
		})
	}
}

func TestPresentDoesNotRemoveRealPromptThatOnlyLooksLikeInput(t *testing.T) {
	input := "› implement the parser requested by the user\n\n• Working (2s • esc to interrupt)\n\ngpt-5.6-sol high · /work"
	got := Present(Runtime{Detected: true, Supported: true, Version: SupportedVersion}, input)
	if !got.Applied || !strings.Contains(got.Text, "implement the parser requested by the user") {
		t.Fatalf("presentation = %#v", got)
	}
}

func TestPresentRemovesWrappedKnownPlaceholder(t *testing.T) {
	input := "Completed result.\n\n› Write tests for\n  @filename\n\ngpt-5.6-sol high · /work"
	got := Present(Runtime{Detected: true, Supported: true, Version: SupportedVersion}, input)
	if !got.Applied || got.Text != "Completed result." {
		t.Fatalf("presentation = %#v", got)
	}
}
