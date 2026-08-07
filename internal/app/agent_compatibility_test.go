package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/agentcompat"
	"github.com/idolum-ai/engram/internal/agentui"
	"github.com/idolum-ai/engram/internal/codexui"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/tmux"
)

func TestCompatibilityAxesUpdateIndependently(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	session, _ := app.Store.FindSession(id)
	process := provenProcessAxis(agentcompat.ProviderClaude, "2.1.999")
	screen := screenAxis(agentcompat.ProviderClaude, "2.1.999", false, agentcompat.ReasonScreenVersionUnknown)
	binding := agentcompat.Axis{State: agentcompat.StateProven, Contract: agentcompat.ClaudeBindingContract}
	transcript := agentcompat.Axis{State: agentcompat.StateEligible, Contract: agentcompat.ClaudeTranscriptContract, Version: agentcompat.ClaudeTranscriptContract, Reason: agentcompat.ReasonTranscriptEligible}
	app.recordCompatibility(session, agentcompat.ProviderClaude, &process, nil, &screen, nil)
	app.recordCompatibility(session, agentcompat.ProviderClaude, nil, &binding, nil, &transcript)
	current, _ := app.Store.FindSession(id)
	if current.AgentCompatibility.Process.State != agentcompat.StateProven || current.AgentCompatibility.Screen.State != agentcompat.StateLiteral || current.AgentCompatibility.Binding.State != agentcompat.StateProven || current.AgentCompatibility.Transcript.State != agentcompat.StateEligible {
		t.Fatalf("axes collapsed into one support flag: %#v", current.AgentCompatibility)
	}
}

func TestClaudeModelProvenanceMovesFromHookToVisibleToRetained(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	started := time.Now().Add(-time.Minute).UTC()
	identity := strings.Repeat("c", 64)
	if _, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.DeclaredModel = agentcompat.Value{Value: "claude-opus-4-8", Provenance: agentcompat.ProvenanceHook}
		session.DeclaredModelObservedAt = started.Add(time.Second)
	}); err != nil {
		t.Fatal(err)
	}
	capture := tmux.StyledCapture{AlternateOn: "on", PaneInMode: "off"}
	session, _ := app.Store.FindSession(id)
	app.recordClaudeStructuredPresentation(session, identity, started, agentui.Analysis{Model: "claude-opus-4-8", ViewportBoundary: "full_capture"}, capture)
	current, _ := app.Store.FindSession(id)
	if current.AgentPresentation.Model.Provenance != agentcompat.ProvenanceHook {
		t.Fatalf("hook provenance = %#v", current.AgentPresentation.Model)
	}
	app.recordClaudeStructuredPresentation(current, identity, started, agentui.Analysis{Model: "claude-opus-4-8", ModelObserved: true, ViewportBoundary: "full_capture"}, capture)
	current, _ = app.Store.FindSession(id)
	if current.AgentPresentation.Model.Provenance != agentcompat.ProvenanceVisibleUI {
		t.Fatalf("visible provenance = %#v", current.AgentPresentation.Model)
	}
	app.recordClaudeStructuredPresentation(current, identity, started, agentui.Analysis{Model: "claude-opus-4-8", ViewportBoundary: "full_capture"}, capture)
	current, _ = app.Store.FindSession(id)
	if current.AgentPresentation.Model.Provenance != agentcompat.ProvenanceRetainedUI {
		t.Fatalf("retained provenance = %#v", current.AgentPresentation.Model)
	}
}

func TestCodexSemanticStartupViewportNeverChangesLiteralCapture(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	identity := strings.Repeat("b", 64)
	app.CodexDetector = &fixedCodexDetector{runtime: codexui.Runtime{Detected: true, Supported: true, Version: codexui.SupportedVersion, Identity: identity, StartedAt: time.Now().Add(-time.Minute)}}
	session, _ := app.Store.FindSession(id)
	literal := strings.Join([]string{
		"example@host % codex --bad-option", "error: unexpected argument",
		"╭──────────────────────────────╮", "│ >_ OpenAI Codex (v" + codexui.SupportedVersion + ") │", "│ model: gpt-5.6-sol          │", "╰──────────────────────────────╯", "",
		"• Current work is visible.", "", "gpt-5.6-sol high · /work",
	}, "\n")
	capture := tmux.StyledCapture{JoinedText: literal, PanePID: 4242, CurrentCmd: "codex", AlternateOn: "on", PaneInMode: "off"}
	semantic := app.processCapturedFrame(context.Background(), session, capture)
	if strings.Contains(semantic, "--bad-option") || !strings.Contains(semantic, "Current work is visible") || capture.JoinedText != literal {
		t.Fatalf("semantic=%q literal=%q", semantic, capture.JoinedText)
	}
	current, _ := app.Store.FindSession(id)
	if current.SemanticViewport.Boundary != "codex_startup_card" || current.SemanticViewport.StartLine != 6 || current.SemanticViewport.RuntimeIdentity != identity {
		t.Fatalf("viewport = %#v", current.SemanticViewport)
	}
}

func TestStructuredViewportIsProcessAndTerminalModeBound(t *testing.T) {
	identity := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	observed := state.TerminalSession{TmuxServerID: strings.Repeat("d", 32), TmuxWindowID: "@2", TmuxPaneID: "%4"}
	first := structuredViewport(agentcompat.ClaudeScreenContract, identity, 10, "claude_trust_prompt", tmux.StyledCapture{AlternateOn: "on", PaneInMode: "off"}, observed)
	copyMode := structuredViewport(agentcompat.ClaudeScreenContract, identity, 10, "claude_trust_prompt", tmux.StyledCapture{AlternateOn: "on", PaneInMode: "on"}, observed)
	alternate := structuredViewport(agentcompat.ClaudeScreenContract, identity, 10, "claude_trust_prompt", tmux.StyledCapture{AlternateOn: "off", PaneInMode: "off"}, observed)
	if !first.Applied || first.CopyMode == copyMode.CopyMode || first.AlternateScreen == alternate.AlternateScreen {
		t.Fatalf("viewport modes first=%#v copy=%#v alternate=%#v", first, copyMode, alternate)
	}
}

func TestLifecycleResetClearsAllAgentIntegrationAndRetainedProvenance(t *testing.T) {
	session := state.TerminalSession{
		AgentCompatibility: agentcompat.Compatibility{Provider: agentcompat.ProviderClaude, Process: agentcompat.Axis{State: agentcompat.StateProven}},
		AgentPresentation:  agentcompat.Presentation{Model: agentcompat.Value{Value: "claude-opus-4-8", Provenance: agentcompat.ProvenanceRetainedUI}},
		SemanticViewport:   agentcompat.Viewport{Applied: true, Contract: agentcompat.ClaudeScreenContract, RuntimeIdentity: strings.Repeat("a", 64), TmuxIdentity: strings.Repeat("b", 64), Boundary: "full_capture"},
		DeclaredModel:      agentcompat.Value{Value: "claude-opus-4-8", Provenance: agentcompat.ProvenanceHook}, DeclaredModelObservedAt: time.Now(),
		PresentationProgram: "claude", PresentationRuntimeID: "old", PresentationModel: "claude-opus-4-8",
	}
	resetAgentIntegrationState(&session)
	if session.AgentCompatibility.Provider != "" || session.AgentPresentation.Model.Value != "" || session.SemanticViewport.Applied || session.DeclaredModel.Value != "" || !session.DeclaredModelObservedAt.IsZero() || session.PresentationProgram != "" {
		t.Fatalf("integration survived lifecycle reset: %#v", session)
	}
}
