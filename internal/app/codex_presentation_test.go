package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/agentcompat"
	"github.com/idolum-ai/engram/internal/agentui"
	"github.com/idolum-ai/engram/internal/claudeui"
	"github.com/idolum-ai/engram/internal/codexui"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/tmux"
)

func TestRecordAgentPresentationRedactsAllTerminalDerivedMetadata(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	const secret = "fixture-provider-secret"
	app.Config.OpenAIAPIKey = secret
	session, _ := app.Store.FindSession(id)
	app.recordAgentPresentation(session, agentui.Analysis{
		Applied: true, Conversation: "done", Model: secret + "/gpt-5.6-sol",
		Effort: "high-" + secret, Mode: "fast-" + secret,
		Activity: agentui.Activity("active-" + secret),
	})
	current, _ := app.Store.FindSession(id)
	metadata := strings.Join([]string{current.PresentationModel, current.PresentationEffort, current.PresentationMode, current.PresentationActivity}, "\n")
	if strings.Contains(metadata, secret) || !strings.Contains(metadata, "<redacted>") {
		t.Fatalf("terminal-derived presentation metadata was not redacted: %q", metadata)
	}
}

type fixedCodexDetector struct {
	runtime codexui.Runtime
	err     error
	pid     int
	command string
}

type fixedClaudeDetector struct {
	runtime claudeui.Runtime
	err     error
	pid     int
	command string
}

func (d *fixedClaudeDetector) Detect(_ context.Context, pid int, command string) (claudeui.Runtime, error) {
	d.pid = pid
	d.command = command
	return d.runtime, d.err
}

func (d *fixedCodexDetector) Detect(_ context.Context, pid int, command string) (codexui.Runtime, error) {
	d.pid = pid
	d.command = command
	return d.runtime, d.err
}

func TestProcessCapturedFrameUsesProvenCodexAdapterBeforeGenericFallback(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	detector := &fixedCodexDetector{runtime: codexui.Runtime{Detected: true, Supported: true, Version: codexui.SupportedVersion, Identity: strings.Repeat("a", 64), StartedAt: time.Unix(1_700_000_000, 0)}}
	app.CodexDetector = detector
	session, _ := app.Store.FindSession(id)
	input := strings.Join([]string{
		"• Ran go test ./...",
		"  └ ok example/internal/app",
		"",
		"⚠ Automatic approval review approved: https://example.test/audit",
		"",
		"────────────────────────────────────────",
		"",
		"• Working (12s • esc to interrupt)",
		"",
		"› Write tests for @filename",
		"",
		"gpt-5.6-sol high fast · /work · Main [default]",
	}, "\n")
	got := app.processCapturedFrame(context.Background(), session, tmux.StyledCapture{
		JoinedText: input, PanePID: 4242, CurrentCmd: "node",
	})
	if detector.pid != 4242 || detector.command != "node" {
		t.Fatalf("Codex detector observed pid=%d command=%q", detector.pid, detector.command)
	}
	if !strings.Contains(got, "Ran go test") || strings.Contains(got, "Working (") || strings.Contains(got, "Write tests") || strings.Contains(got, "gpt-5.6-sol") {
		t.Fatalf("guide input = %q", got)
	}
	refs := app.visibleReferencesForStyledCapture(observeUpstreamSignal(tmux.StyledCapture{JoinedText: input}).PresentationText, nil)
	if len(refs.URLs) != 1 || refs.URLs[0] != "https://example.test/audit" || strings.Contains(got, refs.URLs[0]) {
		t.Fatalf("reference boundary refs=%#v guide=%q", refs, got)
	}
	current, ok := app.Store.FindSession(id)
	if !ok || current.PresentationProgram != "codex" || current.PresentationVersion != codexui.SupportedVersion || current.PresentationModel != "gpt-5.6-sol" || current.PresentationEffort != "high" || current.PresentationMode != "fast" || current.PresentationActivity != "working" || current.AgentCompatibility.Process.State != agentcompat.StateProven || current.AgentCompatibility.Screen.State != agentcompat.StateSupported || current.AgentPresentation.Model.Value != "gpt-5.6-sol" || current.SemanticViewport.RuntimeIdentity != detector.runtime.Identity || current.SemanticViewport.Contract != agentcompat.CodexScreenContract {
		t.Fatalf("session presentation = %#v ok=%v", current, ok)
	}
	card := app.renderLocal(current, "Tests are passing.")
	if !strings.Contains(card, "Codex · gpt-5.6-sol · high · fast · active\n\nTests are passing.") {
		t.Fatalf("card = %q", card)
	}
}

func TestProcessCapturedFrameGenericAnalysisSupportsNonCodexAgentUI(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	detector := &fixedCodexDetector{err: errors.New("must not be called")}
	app.CodexDetector = detector
	session, _ := app.Store.FindSession(id)
	input := "• The refactor is complete.\n\n❯\n\nclaude-sonnet-4-6 · ~/work · main"
	got := app.processCapturedFrame(context.Background(), session, tmux.StyledCapture{
		JoinedText: input, PanePID: 4242, CurrentCmd: "claude", AlternateOn: "on",
	})
	if strings.Contains(got, "claude-sonnet") || strings.Contains(got, "❯") || !strings.Contains(got, "refactor is complete") {
		t.Fatalf("generic guide input = %q", got)
	}
	if detector.pid != 4242 {
		t.Fatalf("Codex detector did not inspect ambiguous agent frame: pid=%d", detector.pid)
	}
	current, ok := app.Store.FindSession(id)
	if !ok || current.PresentationProgram != "agent" || current.PresentationModel != "claude-sonnet-4-6" || current.PresentationActivity != "idle" {
		t.Fatalf("generic presentation state = %#v ok=%v", current, ok)
	}
	if got := app.renderLocal(current, "Done."); !strings.Contains(got, "Agent · claude-sonnet-4-6 · idle") {
		t.Fatalf("generic card = %q", got)
	}
}

func TestProcessCapturedFrameRetainsClaudeModelForSameRuntime(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	runtime := claudeui.Runtime{Detected: true, Supported: true, Version: claudeui.SupportedVersion, Identity: strings.Repeat("a", 64)}
	detector := &fixedClaudeDetector{runtime: runtime}
	app.ClaudeDetector = detector
	app.CodexDetector = &fixedCodexDetector{err: errors.New("must not be called")}
	session, _ := app.Store.FindSession(id)
	modelCard := strings.Join([]string{
		"╭──────────────────────────────────╮",
		"│ Opus 4.8 · API Usage Billing     │",
		"╰──────────────────────────────────╯",
		"",
		"⏺ The implementation is ready.",
		"",
		"────────────────────────────────────",
		"❯",
		"────────────────────────────────────",
		"  bypass permissions on · ● high · /effort",
	}, "\n")
	capture := func(text string) tmux.StyledCapture {
		return tmux.StyledCapture{
			JoinedText: text, PanePID: 4242, CurrentCmd: "claude",
			ServerID: session.TmuxServerID, WindowID: session.TmuxWindowID, PaneID: session.TmuxPaneID,
			Columns: 80, VisibleRows: 24, AlternateOn: "on", PaneInMode: "off",
		}
	}
	if got := app.processCapturedFrame(context.Background(), session, capture(modelCard)); strings.Contains(got, "API Usage Billing") {
		t.Fatalf("initial guide input retained model card: %q", got)
	}
	current, _ := app.Store.FindSession(id)
	if current.PresentationProgram != "claude" || current.PresentationVersion != claudeui.SupportedVersion ||
		current.PresentationRuntimeID != runtime.Identity || current.PresentationModel != "claude-opus-4-8" ||
		current.PresentationEffort != "high" || current.PresentationActivity != "idle" {
		t.Fatalf("initial Claude presentation = %#v", current)
	}

	noModel := strings.Join([]string{
		"⏺ Running repository checks.",
		"",
		"✻ Deliberating…",
		"",
		"────────────────────────────────────",
		"❯",
		"────────────────────────────────────",
		"  bypass permissions on · ● high · /effort",
	}, "\n")
	if got := app.processCapturedFrame(context.Background(), current, capture(noModel)); strings.Contains(got, "Deliberating") {
		t.Fatalf("continued guide input retained active chrome: %q", got)
	}
	current, _ = app.Store.FindSession(id)
	if current.PresentationModel != "claude-opus-4-8" || current.PresentationActivity != "active" {
		t.Fatalf("continued Claude presentation = %#v", current)
	}
	if detector.pid != 4242 || detector.command != "claude" {
		t.Fatalf("detector observed pid=%d command=%q", detector.pid, detector.command)
	}
	if got := app.renderLocal(current, "Checks are running."); !strings.Contains(got, "Claude Code · Opus 4.8 · high · active") {
		t.Fatalf("Claude card = %q", got)
	}
}

func TestProcessCapturedFrameRestoresClaudeModelAfterServiceRestart(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	runtime := claudeui.Runtime{Detected: true, Supported: true, Version: claudeui.SupportedVersion, Identity: strings.Repeat("b", 64)}
	if _, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.PresentationProgram = "claude"
		session.PresentationVersion = runtime.Version
		session.PresentationRuntimeID = runtime.Identity
		session.PresentationModel = "claude-opus-4-8"
		session.PresentationEffort = "high"
		session.PresentationActivity = "active"
	}); err != nil {
		t.Fatal(err)
	}
	app.ClaudeDetector = &fixedClaudeDetector{runtime: runtime}
	app.CodexDetector = nil
	app.agentFrames = map[int]agentFrameState{}
	session, _ := app.Store.FindSession(id)
	input := "⏺ Work is complete.\n\n✻ Brewed for 3m 12s\n\n────────────────────\n❯\n────────────────────\n  bypass permissions on · ● high · /effort"
	got := app.processCapturedFrame(context.Background(), session, tmux.StyledCapture{
		JoinedText: input, PanePID: 4242, CurrentCmd: "claude",
		ServerID: session.TmuxServerID, WindowID: session.TmuxWindowID, PaneID: session.TmuxPaneID,
		Columns: 80, VisibleRows: 24, AlternateOn: "on", PaneInMode: "off",
	})
	if strings.Contains(got, "Brewed for") {
		t.Fatalf("restart guide input retained elapsed chrome: %q", got)
	}
	current, _ := app.Store.FindSession(id)
	if current.PresentationModel != "claude-opus-4-8" || current.PresentationActivity != "idle" {
		t.Fatalf("restored Claude presentation = %#v", current)
	}
}

func TestProcessCapturedFrameDoesNotCarryClaudeModelAcrossRuntimeIdentity(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	oldIdentity := strings.Repeat("c", 64)
	newIdentity := strings.Repeat("d", 64)
	if _, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.PresentationProgram = "claude"
		session.PresentationVersion = claudeui.SupportedVersion
		session.PresentationRuntimeID = oldIdentity
		session.PresentationModel = "claude-fable-5"
		session.PresentationEffort = "high"
		session.PresentationActivity = "active"
		session.AgentCompatibility = agentcompat.Compatibility{Provider: agentcompat.ProviderClaude, Binding: agentcompat.Axis{State: agentcompat.StateProven}, Transcript: agentcompat.Axis{State: agentcompat.StateEligible}}
		session.AgentPresentation = agentcompat.Presentation{Model: agentcompat.Value{Value: "claude-fable-5", Provenance: agentcompat.ProvenanceRetainedUI}}
		session.SemanticViewport = agentcompat.Viewport{Applied: true, Contract: agentcompat.ClaudeScreenContract, RuntimeIdentity: oldIdentity, TmuxIdentity: strings.Repeat("e", 64), Boundary: "full_capture"}
		session.DeclaredModel = agentcompat.Value{Value: "claude-fable-5", Provenance: agentcompat.ProvenanceHook}
		session.DeclaredModelObservedAt = time.Now().UTC()
	}); err != nil {
		t.Fatal(err)
	}
	app.ClaudeDetector = &fixedClaudeDetector{runtime: claudeui.Runtime{
		Detected: true, Supported: true, Version: claudeui.SupportedVersion, Identity: newIdentity, PID: 5252, StartedAt: time.Now().Add(-time.Minute),
	}}
	app.CodexDetector = nil
	session, _ := app.Store.FindSession(id)
	input := "⏺ Waiting for input.\n\n────────────────────\n❯\n────────────────────\n  bypass permissions on · ● high · /effort"
	app.processCapturedFrame(context.Background(), session, tmux.StyledCapture{
		JoinedText: input, PanePID: 5252, CurrentCmd: "claude",
		ServerID: session.TmuxServerID, WindowID: session.TmuxWindowID, PaneID: session.TmuxPaneID,
		Columns: 80, VisibleRows: 24, AlternateOn: "on", PaneInMode: "off",
	})
	current, _ := app.Store.FindSession(id)
	if current.PresentationProgram != "claude" || current.PresentationRuntimeID != newIdentity ||
		current.PresentationModel != "" || current.PresentationActivity != "idle" {
		t.Fatalf("replacement Claude presentation = %#v", current)
	}
	if current.SemanticViewport.RuntimeIdentity != newIdentity || current.AgentPresentation.Model.Value != "" || current.DeclaredModel.Value != "" || current.AgentCompatibility.Binding.State == agentcompat.StateProven || current.AgentCompatibility.Transcript.State == agentcompat.StateEligible {
		t.Fatalf("replacement compatibility was not independently invalidated: %#v", current)
	}
	if got := app.renderLocal(current, "Waiting."); !strings.Contains(got, "Claude Code · high · idle") || strings.Contains(got, "fable") {
		t.Fatalf("replacement Claude card = %q", got)
	}
}

func TestProcessCapturedFrameClearsClaudeStateForUnsupportedVersion(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	if _, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.PresentationProgram = "claude"
		session.PresentationVersion = claudeui.SupportedVersion
		session.PresentationRuntimeID = strings.Repeat("e", 64)
		session.PresentationModel = "claude-opus-4-8"
		session.PresentationActivity = "idle"
	}); err != nil {
		t.Fatal(err)
	}
	app.ClaudeDetector = &fixedClaudeDetector{runtime: claudeui.Runtime{Detected: true, Version: "2.1.220", Identity: strings.Repeat("f", 64)}}
	app.CodexDetector = nil
	session, _ := app.Store.FindSession(id)
	input := "future Claude layout"
	if got := app.processCapturedFrame(context.Background(), session, tmux.StyledCapture{JoinedText: input, PanePID: 4242, CurrentCmd: "claude"}); got != input {
		t.Fatalf("unsupported version changed input: %q", got)
	}
	current, _ := app.Store.FindSession(id)
	if current.PresentationProgram != "" || current.PresentationRuntimeID != "" || current.PresentationModel != "" {
		t.Fatalf("unsupported Claude state survived: %#v", current)
	}
}

func TestProcessCapturedFrameClearsStaleClaudeActivityForUnknownLayout(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	identity := strings.Repeat("7", 64)
	if _, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.PresentationProgram = "claude"
		session.PresentationVersion = claudeui.SupportedVersion
		session.PresentationRuntimeID = identity
		session.PresentationModel = "claude-opus-4-8"
		session.PresentationActivity = "active"
	}); err != nil {
		t.Fatal(err)
	}
	app.ClaudeDetector = &fixedClaudeDetector{runtime: claudeui.Runtime{
		Detected: true, Supported: true, Version: claudeui.SupportedVersion, Identity: identity,
	}}
	app.CodexDetector = nil
	session, _ := app.Store.FindSession(id)
	input := "future Claude layout"
	if got := app.processCapturedFrame(context.Background(), session, tmux.StyledCapture{JoinedText: input, PanePID: 4242, CurrentCmd: "claude"}); got != input {
		t.Fatalf("unknown layout changed input: %q", got)
	}
	current, _ := app.Store.FindSession(id)
	if current.PresentationProgram != "" || current.PresentationActivity != "" {
		t.Fatalf("stale Claude activity survived unknown layout: %#v", current)
	}
}

func TestProcessCapturedFrameKeepsClaudeStateOnTransientDetectionFailure(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	identity := strings.Repeat("9", 64)
	if _, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.PresentationProgram = "claude"
		session.PresentationVersion = claudeui.SupportedVersion
		session.PresentationRuntimeID = identity
		session.PresentationModel = "claude-opus-4-8"
		session.PresentationActivity = "idle"
	}); err != nil {
		t.Fatal(err)
	}
	app.ClaudeDetector = &fixedClaudeDetector{
		runtime: claudeui.Runtime{Detected: true, Version: claudeui.SupportedVersion},
		err:     errors.New("process start unavailable"),
	}
	app.CodexDetector = &fixedCodexDetector{err: errors.New("ps unavailable")}
	session, _ := app.Store.FindSession(id)
	input := "ordinary capture"
	if got := app.processCapturedFrame(context.Background(), session, tmux.StyledCapture{JoinedText: input, PanePID: 4242, CurrentCmd: "claude"}); got != input {
		t.Fatalf("transient failure changed input: %q", got)
	}
	current, _ := app.Store.FindSession(id)
	if current.PresentationProgram != "claude" || current.PresentationRuntimeID != identity || current.PresentationModel != "claude-opus-4-8" {
		t.Fatalf("transient failure cleared Claude state: %#v", current)
	}
}

func TestProcessCapturedFrameClearsAgentStateAfterDefinitiveExitToShell(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	if _, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.AgentCompatibility = agentcompat.Compatibility{Provider: agentcompat.ProviderCodex, Process: agentcompat.Axis{State: agentcompat.StateProven}}
		session.AgentPresentation = agentcompat.Presentation{Model: agentcompat.Value{Value: "gpt-5.6-sol", Provenance: agentcompat.ProvenanceVisibleUI}, Activity: "active"}
		session.SemanticViewport = agentcompat.Viewport{Applied: true, Contract: agentcompat.CodexScreenContract, RuntimeIdentity: strings.Repeat("a", 64)}
		session.DeclaredModel = agentcompat.Value{Value: "stale-model", Provenance: agentcompat.ProvenanceHook}
		session.DeclaredModelObservedAt = time.Now().UTC()
		session.PresentationProgram = "codex"
		session.PresentationModel = "gpt-5.6-sol"
		session.PresentationActivity = "working"
	}); err != nil {
		t.Fatal(err)
	}
	app.ClaudeDetector = &fixedClaudeDetector{}
	app.CodexDetector = &fixedCodexDetector{}
	session, _ := app.Store.FindSession(id)
	input := "user@host repo %"
	if got := app.processCapturedFrame(context.Background(), session, tmux.StyledCapture{JoinedText: input, PanePID: 4242, CurrentCmd: "zsh"}); got != input {
		t.Fatalf("shell frame changed: %q", got)
	}
	current, _ := app.Store.FindSession(id)
	if current.AgentCompatibility != (agentcompat.Compatibility{}) || current.AgentPresentation != (agentcompat.Presentation{}) ||
		current.SemanticViewport != (agentcompat.Viewport{}) || current.DeclaredModel != (agentcompat.Value{}) ||
		!current.DeclaredModelObservedAt.IsZero() || current.PresentationProgram != "" || current.PresentationModel != "" {
		t.Fatalf("definitive agent exit retained integration state: %#v", current)
	}
	if got := app.renderLocal(current, input); strings.Contains(got, "Codex") || strings.Contains(got, "gpt-5.6-sol") {
		t.Fatalf("shell card retained agent status: %q", got)
	}
}

func TestProcessCapturedFrameBoundsTemporalSemanticsToTerminalIdentity(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	app.CodexDetector = nil
	session, _ := app.Store.FindSession(id)
	frame := func(seconds, pane string) tmux.StyledCapture {
		return tmux.StyledCapture{
			JoinedText: "› analyze the fixture\n\n• Starting analysis\n\nIndexing files (" + seconds + "s)\n\ngpt-5.6-sol high · /work",
			ServerID:   session.TmuxServerID, WindowID: session.TmuxWindowID, PaneID: pane,
			CurrentCmd: "agent", Columns: 80, VisibleRows: 24, AlternateOn: "on", PaneInMode: "off",
		}
	}
	first := app.processCapturedFrame(context.Background(), session, frame("2", session.TmuxPaneID))
	if !strings.Contains(first, "Indexing files") {
		t.Fatalf("first frame used nonexistent temporal evidence: %q", first)
	}
	second := app.processCapturedFrame(context.Background(), session, frame("3", session.TmuxPaneID))
	if strings.Contains(second, "Indexing files") {
		t.Fatalf("aligned changing status was not classified as activity: %q", second)
	}
	current, _ := app.Store.FindSession(id)
	if current.PresentationActivity != "active" {
		t.Fatalf("aligned activity = %q", current.PresentationActivity)
	}
	moved := app.processCapturedFrame(context.Background(), session, frame("4", "%different"))
	if !strings.Contains(moved, "Indexing files") {
		t.Fatalf("identity change reused stale temporal evidence: %q", moved)
	}
	current, _ = app.Store.FindSession(id)
	if current.PresentationActivity != "idle" {
		t.Fatalf("activity after identity change = %q", current.PresentationActivity)
	}
	app.agentFrameMu.Lock()
	defer app.agentFrameMu.Unlock()
	if len(app.agentFrames) != 1 {
		t.Fatalf("agent frame cache contains %d entries, want one per session", len(app.agentFrames))
	}
}

func TestProcessCapturedFrameFallsBackAndClearsStaleCodexState(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	if _, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.PresentationProgram = "codex"
		session.PresentationVersion = codexui.SupportedVersion
		session.PresentationModel = "gpt-5.6-sol"
		session.PresentationEffort = "high"
		session.PresentationMode = "fast"
		session.PresentationActivity = "working"
	}); err != nil {
		t.Fatal(err)
	}
	app.CodexDetector = &fixedCodexDetector{runtime: codexui.Runtime{Detected: true, Version: "0.145.0"}}
	session, _ := app.Store.FindSession(id)
	input := "answer\ngpt-5.7 low · /work"
	if got := app.processCapturedFrame(context.Background(), session, tmux.StyledCapture{JoinedText: input, PanePID: 4242, CurrentCmd: "node"}); got != input {
		t.Fatalf("fallback changed input: %q", got)
	}
	current, _ := app.Store.FindSession(id)
	if current.PresentationProgram != "" || current.PresentationModel != "" || current.PresentationMode != "" || strings.Contains(app.renderLocal(current, "answer"), "Codex ·") {
		t.Fatalf("stale presentation survived fallback: %#v", current)
	}
}

func TestProcessCapturedFrameKeepsLastCardStateOnTransientDetectionFailure(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	if _, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.PresentationProgram = "codex"
		session.PresentationVersion = codexui.SupportedVersion
		session.PresentationModel = "gpt-5.6-sol"
		session.PresentationEffort = "high"
		session.PresentationActivity = "idle"
	}); err != nil {
		t.Fatal(err)
	}
	app.CodexDetector = &fixedCodexDetector{err: errors.New("ps unavailable")}
	session, _ := app.Store.FindSession(id)
	input := "answer\ngpt-5.6-sol high · /work"
	if got := app.processCapturedFrame(context.Background(), session, tmux.StyledCapture{JoinedText: input, PanePID: 4242, CurrentCmd: "node"}); got != input {
		t.Fatalf("transient fallback changed input: %q", got)
	}
	current, _ := app.Store.FindSession(id)
	if current.PresentationProgram != "codex" || current.PresentationActivity != "idle" {
		t.Fatalf("transient failure cleared card state: %#v", current)
	}
}

func TestCodexPresentationAppearsOnTextGuideAndSnapshotCards(t *testing.T) {
	app := &App{}
	session := state.TerminalSession{
		ID: 4, State: state.TerminalRunning, Title: "review", LastKnownCWD: "/work",
		PresentationProgram: "codex", PresentationVersion: codexui.SupportedVersion,
		PresentationModel: "gpt-5.6-sol", PresentationEffort: "high", PresentationMode: "fast", PresentationActivity: "reviewing approval",
		PresentationNotice: "⚠ Switch to the fast model for additional security review.",
	}
	want := "Codex · gpt-5.6-sol · high · fast · reviewing approval\nnotice: ⚠ Switch to the fast model for additional security review."
	textCard := renderLocal(session, "A command is awaiting review.")
	guideCard, _ := app.guidedEvidenceCaption(session, "A command is awaiting review.", visibleReferences{})
	snapshotCard, _ := app.snapshotAnchorCaption(session, tmux.StyledCapture{Columns: 71, VisibleRows: 37, BufferRows: 64}, visibleReferences{})
	for name, card := range map[string]string{"text": textCard, "guide evidence": guideCard, "snapshot": snapshotCard} {
		if !strings.Contains(card, want) {
			t.Errorf("%s card omitted Codex state: %q", name, card)
		}
	}
}
