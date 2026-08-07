package app

import (
	"context"
	"strings"
	"time"

	"github.com/idolum-ai/engram/internal/agentcompat"
	"github.com/idolum-ai/engram/internal/agentui"
	"github.com/idolum-ai/engram/internal/codexui"
	"github.com/idolum-ai/engram/internal/recovery"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/tmux"
)

func (a *App) recordRuntimeBinding(ctx context.Context, observed state.TerminalSession, provider agentcompat.Provider, processStarted time.Time) {
	metadata, err := a.sessionRecoveryMetadata(ctx, observed)
	binding := bindingAxis(provider, agentcompat.StateMissing, agentcompat.ReasonBindingMissing)
	switch {
	case err != nil && strings.Contains(strings.ToLower(err.Error()), "unsupported"):
		binding = bindingAxis(provider, agentcompat.StateUnsupported, agentcompat.ReasonBindingUnsupported)
	case err != nil:
		return
	case metadata.Program == "":
	case processStarted.IsZero() || metadata.Program != string(provider) || !recovery.ValidSessionID(metadata.SessionID) || metadata.Observed.Before(processStarted):
		binding = bindingAxis(provider, agentcompat.StateStale, agentcompat.ReasonBindingStale)
	default:
		binding = bindingAxis(provider, agentcompat.StateProven, agentcompat.ReasonNone)
	}
	limit := a.contextTurnLimit(string(provider))
	var transcript *agentcompat.Axis
	if limit <= 0 {
		value := transcriptAxis(provider, agentcompat.StateDisabled, agentcompat.ReasonContextDisabled, "")
		transcript = &value
	} else if binding.State != agentcompat.StateProven {
		value := transcriptAxis(provider, agentcompat.StateUnavailable, binding.Reason, "")
		transcript = &value
	}
	a.recordCompatibility(observed, provider, nil, &binding, nil, transcript)
}

func (a *App) recordCompatibility(observed state.TerminalSession, provider agentcompat.Provider, process, binding, screen, transcript *agentcompat.Axis) {
	current, ok := a.Store.FindSession(observed.ID)
	if !ok || !sameTerminalBinding(current, observed) || !current.CreatedAt.Equal(observed.CreatedAt) {
		return
	}
	candidate := mergedCompatibility(current.AgentCompatibility, provider, process, binding, screen, transcript)
	if candidate == current.AgentCompatibility {
		return
	}
	_, found, applied, err := a.updateSessionIfCurrent(observed, func(session *state.TerminalSession) {
		session.AgentCompatibility = mergedCompatibility(session.AgentCompatibility, provider, process, binding, screen, transcript)
	})
	if err != nil || !found || !applied {
		_ = a.audit("state.agent_compatibility", "failed", map[string]any{"session_id": observed.ID, "error": firstNonEmpty(errorText(err), "superseded")})
	}
}

func mergedCompatibility(current agentcompat.Compatibility, provider agentcompat.Provider, process, binding, screen, transcript *agentcompat.Axis) agentcompat.Compatibility {
	if current.Provider != provider {
		current = agentcompat.Compatibility{Provider: provider}
	}
	if process != nil {
		current.Process = *process
	}
	if binding != nil {
		current.Binding = *binding
	}
	if screen != nil {
		current.Screen = *screen
	}
	if transcript != nil {
		current.Transcript = *transcript
	}
	return agentcompat.NormalizeCompatibility(current)
}

func provenProcessAxis(provider agentcompat.Provider, version string) agentcompat.Axis {
	contract := agentcompat.CodexProcessContract
	if provider == agentcompat.ProviderClaude {
		contract = agentcompat.ClaudeProcessContract
	}
	return agentcompat.Axis{State: agentcompat.StateProven, Contract: contract, Version: version}
}

func unavailableProcessAxis(provider agentcompat.Provider, reason agentcompat.Reason, version string) agentcompat.Axis {
	axis := provenProcessAxis(provider, version)
	axis.State = agentcompat.StateUnavailable
	axis.Reason = reason
	return axis
}

func processProbeReason(err error) agentcompat.Reason {
	text := strings.ToLower(errorText(err))
	if strings.Contains(text, "ambiguous") || strings.Contains(text, "multiple") || strings.Contains(text, "more than one") {
		return agentcompat.ReasonProcessAmbiguous
	}
	if strings.Contains(text, "not found") || strings.Contains(text, "no matching") {
		return agentcompat.ReasonProcessNotFound
	}
	return agentcompat.ReasonProbeUnavailable
}

func transcriptProbeReason(err error) agentcompat.Reason {
	text := strings.ToLower(errorText(err))
	if strings.Contains(text, "ambiguous") || strings.Contains(text, "multiple") {
		return agentcompat.ReasonTranscriptAmbiguous
	}
	return agentcompat.ReasonTranscriptMissing
}

func transcriptProbeAxis(provider agentcompat.Provider, err error) agentcompat.Axis {
	text := strings.ToLower(errorText(err))
	if strings.Contains(text, "unsupported") || strings.Contains(text, "unrecognized") || strings.Contains(text, "unexpected") {
		return transcriptAxis(provider, agentcompat.StateUnsupported, agentcompat.ReasonTranscriptUnsupported, "")
	}
	return transcriptAxis(provider, agentcompat.StateUnavailable, transcriptProbeReason(err), "")
}

func screenAxis(provider agentcompat.Provider, version string, supported bool, reason agentcompat.Reason) agentcompat.Axis {
	contract := agentcompat.CodexScreenContract
	if provider == agentcompat.ProviderClaude {
		contract = agentcompat.ClaudeScreenContract
	}
	state := agentcompat.StateLiteral
	if supported {
		state = agentcompat.StateSupported
		reason = agentcompat.ReasonNone
	}
	return agentcompat.Axis{State: state, Contract: contract, Version: version, Reason: reason}
}

func (a *App) recordClaudeStructuredPresentation(observed state.TerminalSession, runtimeIdentity string, runtimeStarted time.Time, analysis agentui.Analysis, capture tmux.StyledCapture) {
	latest, ok := a.Store.FindSession(observed.ID)
	if !ok || !sameTerminalBinding(latest, observed) || !latest.CreatedAt.Equal(observed.CreatedAt) {
		return
	}
	provenance := agentcompat.ProvenanceRetainedUI
	if analysis.ModelObserved {
		provenance = agentcompat.ProvenanceVisibleUI
	} else if latest.SemanticViewport.RuntimeIdentity == runtimeIdentity && latest.AgentPresentation.Model.Value == analysis.Model &&
		(latest.AgentPresentation.Model.Provenance == agentcompat.ProvenanceVisibleUI || latest.AgentPresentation.Model.Provenance == agentcompat.ProvenanceRetainedUI) {
		provenance = agentcompat.ProvenanceRetainedUI
	} else if latest.DeclaredModel.Value != "" && latest.DeclaredModel.Provenance == agentcompat.ProvenanceHook && !latest.DeclaredModelObservedAt.Before(runtimeStarted) && latest.DeclaredModel.Value == analysis.Model {
		provenance = agentcompat.ProvenanceHook
	}
	presentation := structuredPresentation(analysis.Model, analysis.Effort, analysis.Mode, string(analysis.Activity), analysis.LastTurnSeconds, analysis.AgentTotal, analysis.AgentActive, provenance)
	viewport := structuredViewport(agentcompat.ClaudeScreenContract, runtimeIdentity, analysis.ViewportStart, analysis.ViewportBoundary, capture, observed)
	a.recordAgentStructures(observed, presentation, viewport)
}

func (a *App) recordCodexStructuredPresentation(observed state.TerminalSession, runtimeIdentity string, presentation codexui.Presentation, capture tmux.StyledCapture) {
	structured := structuredPresentation(presentation.Model, presentation.Effort, presentation.Mode, normalizeActivity(presentation.Activity), presentation.LastTurnSeconds, 0, 0, agentcompat.ProvenanceVisibleUI)
	viewport := structuredViewport(agentcompat.CodexScreenContract, runtimeIdentity, presentation.ViewportStart, presentation.ViewportBoundary, capture, observed)
	a.recordAgentStructures(observed, structured, viewport)
}

func structuredPresentation(model, effort, interaction, activity string, duration, total, active int, modelProvenance agentcompat.Provenance) agentcompat.Presentation {
	value := agentcompat.Presentation{Activity: activity, LastTurnSeconds: duration, AgentTotal: total, AgentActive: active, ObservedAt: time.Now().UTC()}
	if model != "" {
		value.Model = agentcompat.Value{Value: model, Provenance: modelProvenance}
	}
	if effort != "" {
		value.Effort = agentcompat.Value{Value: effort, Provenance: agentcompat.ProvenanceVisibleUI}
	}
	if interaction != "" {
		value.Interaction = agentcompat.Value{Value: interaction, Provenance: agentcompat.ProvenanceVisibleUI}
	}
	return agentcompat.NormalizePresentation(value)
}

func structuredViewport(contract, runtimeIdentity string, start int, boundary string, capture tmux.StyledCapture, observed state.TerminalSession) agentcompat.Viewport {
	if boundary == "" {
		boundary = "full_capture"
	}
	serverID := firstNonEmpty(capture.ServerID, observed.TmuxServerID)
	windowID := firstNonEmpty(capture.WindowID, observed.TmuxWindowID)
	paneID := firstNonEmpty(capture.PaneID, observed.TmuxPaneID)
	if serverID == "" || windowID == "" || paneID == "" {
		return agentcompat.Viewport{}
	}
	return agentcompat.NormalizeViewport(agentcompat.Viewport{
		Applied: true, Contract: contract, RuntimeIdentity: runtimeIdentity,
		TmuxIdentity: sha(strings.Join([]string{serverID, windowID, paneID}, "\x00")),
		Boundary:     boundary, StartLine: start,
		AlternateScreen: capture.AlternateOn, CopyMode: capture.PaneInMode,
	})
}

func normalizeActivity(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "working", "active":
		return "active"
	case "reviewing approval", "awaiting_approval":
		return "awaiting_approval"
	case "idle":
		return "idle"
	default:
		return "unknown"
	}
}

func (a *App) recordAgentStructures(observed state.TerminalSession, presentation agentcompat.Presentation, viewport agentcompat.Viewport) {
	current, ok := a.Store.FindSession(observed.ID)
	if !ok || !sameTerminalBinding(current, observed) || !current.CreatedAt.Equal(observed.CreatedAt) {
		return
	}
	currentPresentation, nextPresentation := current.AgentPresentation, presentation
	currentPresentation.ObservedAt, nextPresentation.ObservedAt = time.Time{}, time.Time{}
	if currentPresentation == nextPresentation && current.SemanticViewport == viewport {
		return
	}
	_, found, applied, err := a.updateSessionIfCurrent(observed, func(session *state.TerminalSession) {
		session.AgentPresentation = presentation
		session.SemanticViewport = viewport
	})
	if err != nil || !found || !applied {
		_ = a.audit("state.agent_presentation", "failed", map[string]any{"session_id": observed.ID, "error": firstNonEmpty(errorText(err), "superseded")})
	}
}

func (a *App) clearAgentStructures(observed state.TerminalSession) {
	current, ok := a.Store.FindSession(observed.ID)
	if !ok || current.AgentPresentation == (agentcompat.Presentation{}) && current.SemanticViewport == (agentcompat.Viewport{}) {
		return
	}
	_, _, _, _ = a.updateSessionIfCurrent(observed, func(session *state.TerminalSession) {
		session.AgentPresentation = agentcompat.Presentation{}
		session.SemanticViewport = agentcompat.Viewport{}
	})
}

func (a *App) invalidateAgentProcessReplacement(observed state.TerminalSession, provider agentcompat.Provider, runtimeIdentity string) {
	current, ok := a.Store.FindSession(observed.ID)
	if !ok || current.SemanticViewport.RuntimeIdentity == "" || current.SemanticViewport.RuntimeIdentity == runtimeIdentity {
		return
	}
	_, _, _, _ = a.updateSessionIfCurrent(observed, func(session *state.TerminalSession) {
		session.AgentPresentation = agentcompat.Presentation{}
		session.SemanticViewport = agentcompat.Viewport{}
		session.DeclaredModel = agentcompat.Value{}
		session.DeclaredModelObservedAt = time.Time{}
		compatibility := session.AgentCompatibility
		if compatibility.Provider == provider {
			compatibility.Binding.State = agentcompat.StateStale
			compatibility.Binding.Reason = agentcompat.ReasonIdentityChanged
			compatibility.Transcript.State = agentcompat.StateUnavailable
			compatibility.Transcript.Reason = agentcompat.ReasonIdentityChanged
			session.AgentCompatibility = agentcompat.NormalizeCompatibility(compatibility)
		}
	})
}

func resetAgentIntegrationState(session *state.TerminalSession) {
	session.AgentCompatibility = agentcompat.Compatibility{}
	session.AgentPresentation = agentcompat.Presentation{}
	session.SemanticViewport = agentcompat.Viewport{}
	session.DeclaredModel = agentcompat.Value{}
	session.DeclaredModelObservedAt = time.Time{}
	session.PresentationProgram = ""
	session.PresentationVersion = ""
	session.PresentationRuntimeID = ""
	session.PresentationModel = ""
	session.PresentationEffort = ""
	session.PresentationMode = ""
	session.PresentationActivity = ""
	session.PresentationNotice = ""
}
