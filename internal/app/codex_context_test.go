package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/claudeui"
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

func (reader *fixedCodexContextReader) Load(sessionID string, limit int, transforms ...func(string) string) (codexcontext.Context, error) {
	reader.sessionID, reader.limit = sessionID, limit
	result := reader.context
	if len(transforms) > 0 && transforms[0] != nil {
		result.Messages = append([]codexcontext.Message(nil), result.Messages...)
		for index := range result.Messages {
			transformed := transforms[0](result.Messages[index].Text)
			result.Messages[index].Redacted = transformed != result.Messages[index].Text
			result.Messages[index].Text = transformed
		}
	}
	return result, reader.err
}

type mutatingCodexContextReader struct {
	context codexcontext.Context
	mutate  func()
}

func (reader *mutatingCodexContextReader) Load(_ string, _ int, transforms ...func(string) string) (codexcontext.Context, error) {
	reader.mutate()
	result := reader.context
	if len(transforms) > 0 && transforms[0] != nil {
		result.Messages = append([]codexcontext.Message(nil), result.Messages...)
		for index := range result.Messages {
			transformed := transforms[0](result.Messages[index].Text)
			result.Messages[index].Redacted = transformed != result.Messages[index].Text
			result.Messages[index].Text = transformed
		}
	}
	return result, nil
}

type sequenceCodexDetector struct {
	runtimes []codexui.Runtime
	calls    int
}

type fixedClaudeContextReader struct {
	context codexcontext.Context
	path    string
	session string
	limit   int
}

func (reader *fixedClaudeContextReader) Load(path, session string, limit int, transforms ...func(string) string) (codexcontext.Context, error) {
	reader.path, reader.session, reader.limit = path, session, limit
	result := reader.context
	if len(transforms) > 0 && transforms[0] != nil {
		result.Messages = append([]codexcontext.Message(nil), result.Messages...)
		for index := range result.Messages {
			transformed := transforms[0](result.Messages[index].Text)
			result.Messages[index].Redacted = transformed != result.Messages[index].Text
			result.Messages[index].Text = transformed
		}
	}
	return result, nil
}

type sequenceClaudeDetector struct {
	runtimes []claudeui.Runtime
	calls    int
}

func (detector *sequenceClaudeDetector) Detect(context.Context, int, string) (claudeui.Runtime, error) {
	if len(detector.runtimes) == 0 {
		return claudeui.Runtime{}, errors.New("no runtime")
	}
	index := min(detector.calls, len(detector.runtimes)-1)
	detector.calls++
	return detector.runtimes[index], nil
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

func TestCodexContextRejectsSameSecondBindingBeforePreciseProcessStart(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	app.Config.CodexContextTurns = 2
	session, _ := app.Store.FindSession(id)
	second := time.Date(2026, 8, 3, 17, 0, 2, 0, time.UTC)
	app.Tmux = tmux.New(&recoveryMetadataRunner{metadata: encodedCodexContextMetadata(t, second.Add(800*time.Millisecond))})
	app.CodexDetector = &sequenceCodexDetector{runtimes: []codexui.Runtime{{
		Detected: true, Identity: strings.Repeat("c", 64), StartedAt: second.Add(900 * time.Millisecond),
	}}}
	reader := &fixedCodexContextReader{context: codexcontext.Context{Parser: codexcontext.ParserVersion, Messages: []codexcontext.Message{{Role: codexcontext.RoleUser, Text: "old same-second binding"}}}}
	app.CodexContext = reader

	if got := app.codexContextForCapture(context.Background(), session, tmux.StyledCapture{PanePID: 4242, CurrentCmd: "codex"}); got.prompt != "" || reader.sessionID != "" {
		t.Fatalf("same-second stale hook reached transcript: got=%#v reader=%#v", got, reader)
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

func TestCodexContextRejectsRecoveryBindingChangedDuringRolloutRead(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	app.Config.CodexContextTurns = 2
	session, _ := app.Store.FindSession(id)
	observed := time.Date(2026, 8, 3, 17, 0, 2, 0, time.UTC)
	runner := &recoveryMetadataRunner{metadata: encodedCodexContextMetadata(t, observed)}
	app.Tmux = tmux.New(runner)
	runtime := codexui.Runtime{Detected: true, Identity: strings.Repeat("e", 64), StartedAt: observed.Add(-time.Second)}
	app.CodexDetector = &sequenceCodexDetector{runtimes: []codexui.Runtime{runtime, runtime}}
	app.CodexContext = &mutatingCodexContextReader{
		context: codexcontext.Context{Parser: codexcontext.ParserVersion, Messages: []codexcontext.Message{{Role: codexcontext.RoleUser, Text: "session A"}}},
		mutate: func() {
			runner.metadata = encodedCodexContextMetadataForSession(t, "019f7607-c8b0-74b3-87ca-64a7e6e7ede1", observed.Add(time.Second))
		},
	}

	if got := app.codexContextForCapture(context.Background(), session, tmux.StyledCapture{PanePID: 4242, CurrentCmd: "codex"}); got.prompt != "" {
		t.Fatalf("changed recovery binding retained context: %#v", got)
	}
}

func TestConversationTurnRejectsRecoveryBindingChangedBeforePublication(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	app.Config.CodexContextTurns = 2
	session, _ := app.Store.FindSession(id)
	observed := time.Date(2026, 8, 3, 17, 0, 2, 0, time.UTC)
	runner := &recoveryMetadataRunner{metadata: encodedCodexContextMetadata(t, observed)}
	app.Tmux = tmux.New(runner)
	runtime := codexui.Runtime{Detected: true, Identity: strings.Repeat("f", 64), StartedAt: observed.Add(-time.Second)}
	app.CodexDetector = &sequenceCodexDetector{runtimes: []codexui.Runtime{runtime, runtime, runtime}}
	app.CodexContext = &fixedCodexContextReader{context: codexcontext.Context{
		Parser: codexcontext.ParserVersion, Messages: []codexcontext.Message{{Role: codexcontext.RoleUser, Text: "session A"}},
	}}
	capture := testStyledCapture("codex", "active terminal")
	capture.PanePID = 4242
	historical := app.codexContextForCapture(context.Background(), session, capture)
	if historical.prompt == "" {
		t.Fatal("fixture context was not accepted")
	}
	turn := app.prepareConversationTurn(session, capture, capture.JoinedText, historical)
	runner.metadata = encodedCodexContextMetadataForSession(t, "019f7607-c8b0-74b3-87ca-64a7e6e7ede1", observed.Add(time.Second))
	if app.conversationTurnCurrent(session, turn) {
		t.Fatal("rebound provider session remained current at publication guard")
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

func TestClaudeContextUsesHookTranscriptAndSharedIdentityGuards(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	app.Config.ClaudeContextTurns = 3
	session, _ := app.Store.FindSession(id)
	observed := time.Date(2026, 8, 7, 12, 0, 2, 0, time.UTC)
	transcript := "/Users/example/.claude/projects/-work/" + recoveryTestSessionID + ".jsonl"
	app.Tmux = tmux.New(&recoveryMetadataRunner{metadata: encodedClaudeContextMetadata(t, recoveryTestSessionID, transcript, observed)})
	runtime := claudeui.Runtime{Detected: true, Supported: true, Version: "2.1.223", Identity: strings.Repeat("a", 64), StartedAt: observed.Add(-time.Second)}
	app.ClaudeDetector = &sequenceClaudeDetector{runtimes: []claudeui.Runtime{runtime, runtime, runtime}}
	reader := &fixedClaudeContextReader{context: codexcontext.Context{
		Parser: "claude-transcript-v1",
		Messages: []codexcontext.Message{
			{Role: codexcontext.RoleUser, Text: "Explain the worker."},
			{Role: codexcontext.RoleAssistant, Text: "┌────────┐\n│ worker │\n└────────┘"},
		},
	}}
	app.ClaudeContext = reader

	got := app.sessionContextForCapture(context.Background(), session, tmux.StyledCapture{PanePID: 4242, CurrentCmd: "2.1.223"})
	if got.program != recovery.ProgramClaude || got.prompt == "" || got.diagram == "" || reader.path != transcript || reader.session != recoveryTestSessionID || reader.limit != 3 {
		t.Fatalf("context=%#v reader=%#v", got, reader)
	}
	if !app.sessionContextCurrent(context.Background(), session, got) {
		t.Fatal("stable Claude context was not current")
	}
}

func TestClaudeContextRejectsProcessReplacementAfterTranscriptRead(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	app.Config.ClaudeContextTurns = 2
	session, _ := app.Store.FindSession(id)
	observed := time.Date(2026, 8, 7, 12, 0, 2, 0, time.UTC)
	app.Tmux = tmux.New(&recoveryMetadataRunner{metadata: encodedClaudeContextMetadata(t, recoveryTestSessionID, "/tmp/"+recoveryTestSessionID+".jsonl", observed)})
	first := claudeui.Runtime{Detected: true, Supported: true, Identity: strings.Repeat("a", 64), StartedAt: observed.Add(-time.Second)}
	second := claudeui.Runtime{Detected: true, Supported: true, Identity: strings.Repeat("b", 64), StartedAt: observed.Add(-time.Second)}
	app.ClaudeDetector = &sequenceClaudeDetector{runtimes: []claudeui.Runtime{first, second}}
	app.ClaudeContext = &fixedClaudeContextReader{context: codexcontext.Context{Parser: "claude-transcript-v1", Messages: []codexcontext.Message{{Role: codexcontext.RoleUser, Text: "old process"}}}}

	if got := app.sessionContextForCapture(context.Background(), session, tmux.StyledCapture{PanePID: 4242, CurrentCmd: "2.1.223"}); got.prompt != "" {
		t.Fatalf("replaced process retained context: %#v", got)
	}
}

func TestClaudeContextRejectsUnsupportedTranscriptSchemaBeforeRead(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	app.Config.ClaudeContextTurns = 2
	session, _ := app.Store.FindSession(id)
	observed := time.Date(2026, 8, 7, 12, 0, 2, 0, time.UTC)
	app.Tmux = tmux.New(&recoveryMetadataRunner{metadata: encodedClaudeContextMetadata(t, recoveryTestSessionID, "/tmp/"+recoveryTestSessionID+".jsonl", observed)})
	app.ClaudeDetector = &sequenceClaudeDetector{runtimes: []claudeui.Runtime{{
		Detected: true, Supported: false, Version: "2.1.999", Identity: strings.Repeat("a", 64), StartedAt: observed.Add(-time.Second),
	}}}
	reader := &fixedClaudeContextReader{context: codexcontext.Context{Parser: "unexpected", Messages: []codexcontext.Message{{Role: codexcontext.RoleUser, Text: "must not be read"}}}}
	app.ClaudeContext = reader

	if got := app.sessionContextForCapture(context.Background(), session, tmux.StyledCapture{PanePID: 4242, CurrentCmd: "2.1.999"}); got.prompt != "" || reader.path != "" {
		t.Fatalf("unsupported Claude schema reached transcript: got=%#v reader=%#v", got, reader)
	}
}

func encodedCodexContextMetadata(t *testing.T, observed time.Time) string {
	return encodedCodexContextMetadataForSession(t, recoveryTestSessionID, observed)
}

func encodedCodexContextMetadataForSession(t *testing.T, sessionID string, observed time.Time) string {
	t.Helper()
	encoded, err := recovery.Encode(recovery.Metadata{
		Program: recovery.ProgramCodex, SessionID: sessionID,
		Observed: observed, Source: "startup", CWD: "/synthetic",
	})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func encodedClaudeContextMetadata(t *testing.T, sessionID, transcript string, observed time.Time) string {
	t.Helper()
	encoded, err := recovery.Encode(recovery.Metadata{
		Program: recovery.ProgramClaude, SessionID: sessionID, TranscriptPath: transcript,
		Observed: observed, Source: "startup", CWD: "/synthetic",
	})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
