package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/codexcontext"
	"github.com/idolum-ai/engram/internal/codexui"
	"github.com/idolum-ai/engram/internal/recovery"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/tmux"
)

type fixedCodexContextReader struct {
	context   codexcontext.Context
	err       error
	sessionID string
	limit     int
}

func (reader *fixedCodexContextReader) Load(sessionID string, limit int) (codexcontext.Context, error) {
	reader.sessionID, reader.limit = sessionID, limit
	return reader.context, reader.err
}

type sequenceCodexDetector struct {
	runtimes []codexui.Runtime
	calls    int
}

func (detector *sequenceCodexDetector) Detect(context.Context, int, string) (codexui.Runtime, error) {
	if len(detector.runtimes) == 0 {
		return codexui.Runtime{}, errors.New("no runtime")
	}
	index := min(detector.calls, len(detector.runtimes)-1)
	detector.calls++
	return detector.runtimes[index], nil
}

func TestCodexContextRequiresExactPaneHookAndStableProcessIncarnation(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	app.Config.CodexContextTurns = 4
	session, _ := app.Store.FindSession(id)
	observed := time.Date(2026, 8, 3, 17, 0, 2, 0, time.UTC)
	app.Tmux = tmux.New(&recoveryMetadataRunner{metadata: encodedCodexContextMetadata(t, observed)})
	runtime := codexui.Runtime{Detected: true, Version: "9.9.9", Identity: strings.Repeat("a", 64), StartedAt: observed.Add(-2 * time.Second)}
	app.CodexDetector = &sequenceCodexDetector{runtimes: []codexui.Runtime{runtime, runtime}}
	reader := &fixedCodexContextReader{context: codexcontext.Context{
		Parser: codexcontext.ParserVersion,
		Messages: []codexcontext.Message{
			{Role: codexcontext.RoleUser, Text: "Explain the queue."},
			{Role: codexcontext.RoleAssistant, Text: "┌──────┐\n│ queue│\n└──────┘"},
		},
	}}
	app.CodexContext = reader

	got := app.codexContextForCapture(context.Background(), session, tmux.StyledCapture{PanePID: 4242, CurrentCmd: "codex"})
	if got.prompt == "" || got.fingerprint == "" || got.diagram == "" || reader.sessionID != recoveryTestSessionID || reader.limit != 4 {
		t.Fatalf("context = %#v reader=%#v", got, reader)
	}

	app.CodexDetector = &sequenceCodexDetector{runtimes: []codexui.Runtime{runtime, {Detected: true, Identity: strings.Repeat("b", 64), StartedAt: runtime.StartedAt}}}
	if changed := app.codexContextForCapture(context.Background(), session, tmux.StyledCapture{PanePID: 4242, CurrentCmd: "codex"}); changed.prompt != "" {
		t.Fatalf("process replacement retained context: %#v", changed)
	}
}

func TestCodexContextRejectsStaleHookFromPreviousProcess(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	app.Config.CodexContextTurns = 2
	session, _ := app.Store.FindSession(id)
	started := time.Date(2026, 8, 3, 17, 0, 2, 0, time.UTC)
	app.Tmux = tmux.New(&recoveryMetadataRunner{metadata: encodedCodexContextMetadata(t, started.Add(-time.Second))})
	app.CodexDetector = &sequenceCodexDetector{runtimes: []codexui.Runtime{{Detected: true, Identity: strings.Repeat("c", 64), StartedAt: started}}}
	reader := &fixedCodexContextReader{context: codexcontext.Context{Parser: codexcontext.ParserVersion, Messages: []codexcontext.Message{{Role: codexcontext.RoleUser, Text: "old pane context"}}}}
	app.CodexContext = reader

	if got := app.codexContextForCapture(context.Background(), session, tmux.StyledCapture{PanePID: 4242, CurrentCmd: "codex"}); got.prompt != "" || reader.sessionID != "" {
		t.Fatalf("stale pane hook reached transcript: got=%#v reader=%#v", got, reader)
	}
}

func TestCodexContextDoesNotCrossTmuxBindings(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	app.Config.CodexContextTurns = 2
	session, _ := app.Store.FindSession(id)
	observed := time.Date(2026, 8, 3, 17, 0, 2, 0, time.UTC)
	app.Tmux = tmux.New(&recoveryMetadataRunner{metadata: encodedCodexContextMetadata(t, observed)})
	runtime := codexui.Runtime{Detected: true, Identity: strings.Repeat("e", 64), StartedAt: observed.Add(-time.Second)}
	app.CodexDetector = &sequenceCodexDetector{runtimes: []codexui.Runtime{runtime}}
	reader := &fixedCodexContextReader{context: codexcontext.Context{Parser: codexcontext.ParserVersion, Messages: []codexcontext.Message{{Role: codexcontext.RoleUser, Text: "pane one only"}}}}
	app.CodexContext = reader
	session.TmuxPaneID = "%2"
	if got := app.codexContextForCapture(context.Background(), session, tmux.StyledCapture{PanePID: 4242, CurrentCmd: "codex"}); got.prompt != "" || reader.sessionID != "" {
		t.Fatalf("cross-binding transcript was read: got=%#v reader=%#v", got, reader)
	}
}

func TestCodexContextRedactsBeforeProviderAndOmitsSensitiveDiagram(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	const secret = "fixture-sensitive-value"
	app.Config.OpenAIAPIKey = secret
	app.Config.CodexContextTurns = 2
	session, _ := app.Store.FindSession(id)
	observed := time.Date(2026, 8, 3, 17, 0, 2, 0, time.UTC)
	app.Tmux = tmux.New(&recoveryMetadataRunner{metadata: encodedCodexContextMetadata(t, observed)})
	runtime := codexui.Runtime{Detected: true, Identity: strings.Repeat("d", 64), StartedAt: observed.Add(-time.Second)}
	app.CodexDetector = &sequenceCodexDetector{runtimes: []codexui.Runtime{runtime, runtime}}
	app.CodexContext = &fixedCodexContextReader{context: codexcontext.Context{
		Parser:   codexcontext.ParserVersion,
		Messages: []codexcontext.Message{{Role: codexcontext.RoleAssistant, Text: "┌─────────────────────────┐\n│ " + secret + " │\n└─────────────────────────┘"}},
	}}
	got := app.codexContextForCapture(context.Background(), session, tmux.StyledCapture{PanePID: 4242, CurrentCmd: "codex"})
	if strings.Contains(got.prompt, secret) || !strings.Contains(got.prompt, "<redacted>") || got.diagram != "" {
		t.Fatalf("sensitive context = %#v", got)
	}
}

func encodedCodexContextMetadata(t *testing.T, observed time.Time) string {
	t.Helper()
	encoded, err := recovery.Encode(recovery.Metadata{
		Program: recovery.ProgramCodex, SessionID: recoveryTestSessionID,
		Observed: observed, Source: "startup", CWD: "/synthetic",
	})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
