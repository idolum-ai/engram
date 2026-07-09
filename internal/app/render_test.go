package app

import (
	"os"
	"path/filepath"
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
	root := t.TempDir()
	pdf := filepath.Join(root, "engram-aphelion-feature-lessons.pdf")
	pdf2 := filepath.Join(root, "engram-aphelion-feature-lessons.pdf2")
	if err := os.WriteFile(pdf, []byte("pdf"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pdf2, []byte("pdf2"), 0o600); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	homePath := filepath.Join(home, "code/github.com/idolum-ai/engram")
	if err := os.MkdirAll(homePath, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	session := state.TerminalSession{
		ID:    7,
		State: state.TerminalRunning,
		Title: "build",
		LastRawCapture: strings.Join([]string{
			`wrote "` + pdf + `"`,
			"next: " + pdf2 + ")",
			"ignore https://example.test/path",
			"drop missing " + filepath.Join(root, "missing.txt"),
			"again " + pdf,
			"home ~/code/github.com/idolum-ai/engram",
		}, "\n"),
	}

	got := renderLocal(session, "status:\nready")
	for _, want := range []string{
		"[Haiku]",
		"\n\nvisible paths:\n```",
		pdf + "\n",
		pdf2 + "\n",
		"~/code/github.com/idolum-ai/engram\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("renderLocal missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "example.test/path") {
		t.Fatalf("renderLocal included URL path:\n%s", got)
	}
	if strings.Contains(got, "missing.txt") {
		t.Fatalf("renderLocal included missing path:\n%s", got)
	}
	if strings.Count(got, pdf+"\n") != 1 {
		t.Fatalf("renderLocal did not dedupe visible path:\n%s", got)
	}
	if strings.Contains(got, "\n---\n") {
		t.Fatalf("renderLocal included literal separator:\n%s", got)
	}
}
