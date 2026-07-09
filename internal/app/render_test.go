package app

import (
	"strings"
	"testing"

	"github.com/idolum-ai/engram/internal/state"
)

func TestRenderLocalIsStableForSameInput(t *testing.T) {
	session := state.TerminalSession{
		ID:               7,
		State:            state.TerminalRunning,
		Title:            "build",
		LastInputPreview: "make check",
	}

	first := renderLocal(session, "summary:\n- running")
	second := renderLocal(session, "summary:\n- running")
	if first != second {
		t.Fatalf("renderLocal changed for identical input:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	if strings.Contains(first, "updated:") {
		t.Fatalf("renderLocal includes volatile timestamp:\n%s", first)
	}
}

func TestRenderLocalIncludesDeterministicVisiblePaths(t *testing.T) {
	session := state.TerminalSession{
		ID:    7,
		State: state.TerminalRunning,
		Title: "build",
		LastRawCapture: strings.Join([]string{
			`wrote "/tmp/engram-aphelion-feature-lessons.pdf"`,
			"next: /tmp/engram-aphelion-feature-lessons.pdf2)",
			"ignore https://example.test/path",
			"again /tmp/engram-aphelion-feature-lessons.pdf",
			"home ~/code/github.com/idolum-ai/engram",
		}, "\n"),
	}

	got := renderLocal(session, "status:\nready")
	for _, want := range []string{
		"[Haiku]",
		"\n\nvisible paths:\n```",
		"/tmp/engram-aphelion-feature-lessons.pdf\n",
		"/tmp/engram-aphelion-feature-lessons.pdf2\n",
		"~/code/github.com/idolum-ai/engram\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("renderLocal missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "example.test/path") {
		t.Fatalf("renderLocal included URL path:\n%s", got)
	}
	if strings.Count(got, "/tmp/engram-aphelion-feature-lessons.pdf\n") != 1 {
		t.Fatalf("renderLocal did not dedupe visible path:\n%s", got)
	}
	if strings.Contains(got, "\n---\n") {
		t.Fatalf("renderLocal included literal separator:\n%s", got)
	}
}
