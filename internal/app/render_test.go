package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/state"
)

func TestRenderLocalIsStableForSameInput(t *testing.T) {
	session := state.TerminalSession{
		ID:    7,
		State: state.TerminalRunning,
		Title: "build",
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

func TestRenderLocalIncludesDeterministicCWD(t *testing.T) {
	session := state.TerminalSession{
		ID:           7,
		State:        state.TerminalRunning,
		Title:        "build",
		LastKnownCWD: "/srv/engram",
	}
	got := renderLocal(session, "status:\nworking")
	if !strings.Contains(got, "\ncwd: /srv/engram\n") {
		t.Fatalf("renderLocal omitted cwd:\n%s", got)
	}
}

func TestRenderLocalIncludesDeterministicVisibleReferences(t *testing.T) {
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
			"drop root fragments /2 /p /Venue/BoundaryCountertermKernel.lean /bidi /review",
			"again " + pdf,
			"home ~/code/github.com/idolum-ai/engram",
		}, "\n"),
	}

	got := renderLocal(session, "status:\nready")
	for _, want := range []string{
		"\n\nstatus:\nready",
		"\n\nfiles:\n```\n1. ",
		pdf + "\n",
		pdf2 + "\n",
		"\n\nlinks:\nhttps://example.test/path",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("renderLocal missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "[Haiku]") {
		t.Fatalf("renderLocal exposed model implementation detail:\n%s", got)
	}
	if strings.Contains(got, "missing.txt") {
		t.Fatalf("renderLocal included missing path:\n%s", got)
	}
	if strings.Contains(got, "~/code/github.com/idolum-ai/engram") {
		t.Fatalf("renderLocal included a directory:\n%s", got)
	}
	for _, bogus := range []string{"/2", "/p", "/Venue/BoundaryCountertermKernel.lean", "/bidi", "/review"} {
		if strings.Contains(got, bogus+"\n") {
			t.Fatalf("renderLocal included bogus missing path %q:\n%s", bogus, got)
		}
	}
	if strings.Count(got, pdf+"\n") != 1 {
		t.Fatalf("renderLocal did not dedupe visible path:\n%s", got)
	}
	if strings.Contains(got, "\n---\n") {
		t.Fatalf("renderLocal included literal separator:\n%s", got)
	}
}

func TestRenderLocalRedactsDerivedSecretsWithoutMutatingSession(t *testing.T) {
	t.Parallel()
	const secret = "secret-value-123"
	app := &App{Config: config.Config{OpenAIAPIKey: secret}}
	session := state.TerminalSession{
		ID:             1,
		State:          state.TerminalRunning,
		Title:          "token=" + secret,
		LastKnownCWD:   "/tmp/" + secret,
		LastRawCapture: "artifact https://example.test/report?token=" + secret,
	}
	got := app.renderLocal(session, "summary "+secret)
	if strings.Contains(got, secret) || !strings.Contains(got, "<redacted>") {
		t.Fatalf("rendered secret was not redacted:\n%s", got)
	}
	if !strings.Contains(got, "https://example.test/report?token=REDACTED") {
		t.Fatalf("rendered reference redaction broke URL token:\n%s", got)
	}
	if !strings.Contains(session.Title, secret) || !strings.Contains(session.LastKnownCWD, secret) {
		t.Fatalf("render mutated source session: %#v", session)
	}
}

func TestRenderLocalTruncationPreservesUTF8(t *testing.T) {
	t.Parallel()
	session := state.TerminalSession{ID: 1, State: state.TerminalRunning, Title: strings.Repeat("a", 39) + "界界"}
	got := renderLocal(session, "ok")
	if !utf8.ValidString(got) {
		t.Fatalf("render is invalid UTF-8: %q", got)
	}
	short := preview(strings.Repeat("a", 79) + "界界")
	if short == "" || !utf8.ValidString(short) {
		t.Fatalf("preview truncation produced invalid UTF-8: %q", short)
	}
}
