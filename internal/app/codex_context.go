package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/idolum-ai/engram/internal/agentcompat"
	"github.com/idolum-ai/engram/internal/recovery"
	"github.com/idolum-ai/engram/internal/sessioncontext"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/tmux"
)

// The provider-neutral session-context pipeline deliberately retains these
// legacy names at its app boundary so stored behavior and focused Codex race
// tests remain stable while Claude uses the identical publication guards.
type codexContextReader interface {
	Load(string, int, ...func(string) string) (sessioncontext.Context, error)
}

type claudeContextReader interface {
	Load(string, string, int, ...func(string) string) (sessioncontext.Context, error)
}

type sessionContextSnapshot struct {
	prompt          string
	fingerprint     string
	diagram         string
	program         string
	runtimeIdentity string
	bindingIdentity string
	panePID         int
	currentCommand  string
}

type sessionContextRuntime struct {
	detected  bool
	supported bool
	version   string
	identity  string
	startedAt time.Time
}

func (a *App) codexContextForCapture(ctx context.Context, expected state.TerminalSession, capture tmux.StyledCapture) sessionContextSnapshot {
	return a.sessionContextForCapture(ctx, expected, capture)
}

func (a *App) sessionContextForCapture(ctx context.Context, expected state.TerminalSession, capture tmux.StyledCapture) sessionContextSnapshot {
	if capture.PanePID <= 0 {
		return sessionContextSnapshot{}
	}
	metadata, metadataErr := a.sessionRecoveryMetadata(ctx, expected)
	if metadataErr != nil {
		provider := expected.AgentCompatibility.Provider
		if agentcompat.ValidProvider(provider) && strings.Contains(strings.ToLower(metadataErr.Error()), "unsupported") {
			binding := bindingAxis(provider, agentcompat.StateUnsupported, agentcompat.ReasonBindingUnsupported)
			a.recordCompatibility(expected, provider, nil, &binding, nil, nil)
		}
		return sessionContextSnapshot{}
	}
	if !recovery.ValidProgram(metadata.Program) {
		provider := expected.AgentCompatibility.Provider
		if agentcompat.ValidProvider(provider) {
			binding := bindingAxis(provider, agentcompat.StateMissing, agentcompat.ReasonBindingMissing)
			a.recordCompatibility(expected, provider, nil, &binding, nil, nil)
		}
		return sessionContextSnapshot{}
	}
	provider := agentcompat.Provider(metadata.Program)
	limit := a.contextTurnLimit(metadata.Program)
	runtime, err := a.detectContextRuntime(ctx, metadata.Program, capture)
	if err != nil || !runtime.detected || runtime.identity == "" || runtime.startedAt.IsZero() {
		binding := bindingAxis(provider, agentcompat.StateStale, agentcompat.ReasonProcessIdentityUnproven)
		a.recordCompatibility(expected, provider, nil, &binding, nil, nil)
		a.recordSessionContextDecision(expected.ID, metadata.Program, runtime.version, "unavailable", "process_identity_unproven", "", 0, false)
		return sessionContextSnapshot{}
	}
	if !recovery.ValidSessionID(metadata.SessionID) || metadata.Observed.Before(runtime.startedAt) {
		binding := bindingAxis(provider, agentcompat.StateStale, agentcompat.ReasonBindingStale)
		a.recordCompatibility(expected, provider, nil, &binding, nil, nil)
		a.recordSessionContextDecision(expected.ID, metadata.Program, runtime.version, "unavailable", "session_identity_unproven", "", 0, false)
		return sessionContextSnapshot{}
	}
	binding := bindingAxis(provider, agentcompat.StateProven, agentcompat.ReasonNone)
	a.recordCompatibility(expected, provider, nil, &binding, nil, nil)
	if limit <= 0 {
		transcript := transcriptAxis(provider, agentcompat.StateDisabled, agentcompat.ReasonContextDisabled, "")
		a.recordCompatibility(expected, provider, nil, nil, nil, &transcript)
		a.recordSessionContextDecision(expected.ID, metadata.Program, runtime.version, "disabled", "context_disabled", "", 0, false)
		return sessionContextSnapshot{}
	}
	loaded, err := a.loadSessionContext(metadata, limit)
	if err != nil {
		transcript := transcriptProbeAxis(provider, err)
		a.recordCompatibility(expected, provider, nil, nil, nil, &transcript)
		a.recordSessionContextDecision(expected.ID, metadata.Program, runtime.version, "unavailable", "transcript_unavailable", "", 0, false)
		return sessionContextSnapshot{}
	}
	if !supportedTranscriptParser(metadata.Program, loaded.Parser) {
		transcript := transcriptAxis(provider, agentcompat.StateUnsupported, agentcompat.ReasonTranscriptUnsupported, loaded.Parser)
		a.recordCompatibility(expected, provider, nil, nil, nil, &transcript)
		a.recordSessionContextDecision(expected.ID, metadata.Program, runtime.version, "unavailable", "schema_unsupported", loaded.Parser, 0, false)
		return sessionContextSnapshot{}
	}

	redacted := make([]sessioncontext.Message, 0, len(loaded.Messages))
	for _, message := range loaded.Messages {
		text := headUTF8(a.redactText(message.Text), sessioncontext.MaxMessageBytes)
		if strings.TrimSpace(text) != "" {
			redacted = append(redacted, sessioncontext.Message{Role: message.Role, Text: text, Redacted: message.Redacted || text != message.Text})
		}
	}
	prompt := headUTF8(sessioncontext.PromptText(redacted), sessioncontext.MaxContextBytes)
	diagram := ""
	diagramConflict := false
	if candidate, ok := sessioncontext.DetectDiagram(loaded.Messages); ok {
		if loaded.Messages[candidate.Message].Redacted || a.redactText(candidate.Text) != candidate.Text {
			diagramConflict = true
		} else {
			diagram = candidate.Text
		}
	}

	// The transcript read remains provisional until process, provider binding,
	// and watched tmux identity are proven unchanged after I/O.
	after, afterErr := a.detectContextRuntime(ctx, metadata.Program, capture)
	latestMetadata, latestMetadataErr := a.sessionRecoveryMetadata(ctx, expected)
	latest, tracked := a.Store.FindSession(expected.ID)
	if afterErr != nil || after.identity == "" || after.identity != runtime.identity || latestMetadataErr != nil || recoveryMetadataIdentity(latestMetadata) != recoveryMetadataIdentity(metadata) || !tracked || !sameTerminalBinding(latest, expected) || !latest.CreatedAt.Equal(expected.CreatedAt) {
		transcript := transcriptAxis(provider, agentcompat.StateUnavailable, agentcompat.ReasonIdentityChanged, loaded.Parser)
		a.recordCompatibility(expected, provider, nil, nil, nil, &transcript)
		a.recordSessionContextDecision(expected.ID, metadata.Program, runtime.version, "unavailable", "identity_changed", loaded.Parser, 0, false)
		return sessionContextSnapshot{}
	}
	if prompt == "" {
		transcript := transcriptAxis(provider, agentcompat.StateEligible, agentcompat.ReasonTranscriptEligible, loaded.Parser)
		a.recordCompatibility(expected, provider, nil, nil, nil, &transcript)
		a.recordSessionContextDecision(expected.ID, metadata.Program, runtime.version, "empty", "no_visible_messages", loaded.Parser, 0, false)
		return sessionContextSnapshot{}
	}
	bindingIdentity := recoveryMetadataIdentity(metadata)
	fingerprint := sha(strings.Join([]string{metadata.Program, runtime.identity, bindingIdentity, loaded.Parser, prompt, diagram}, "\x00"))
	reason := "visible_messages"
	if diagramConflict {
		reason = "redaction_conflict"
	}
	a.recordSessionContextDecision(expected.ID, metadata.Program, runtime.version, "applied", reason, loaded.Parser, len(redacted), diagram != "")
	transcript := transcriptAxis(provider, agentcompat.StateEligible, agentcompat.ReasonTranscriptEligible, loaded.Parser)
	a.recordCompatibility(expected, provider, nil, nil, nil, &transcript)
	return sessionContextSnapshot{
		prompt: prompt, fingerprint: fingerprint, diagram: diagram, program: metadata.Program,
		runtimeIdentity: runtime.identity, bindingIdentity: bindingIdentity,
		panePID: capture.PanePID, currentCommand: capture.CurrentCmd,
	}
}

func supportedTranscriptParser(program, parser string) bool {
	switch program {
	case recovery.ProgramCodex:
		return parser == agentcompat.CodexTranscriptContract
	case recovery.ProgramClaude:
		return parser == agentcompat.ClaudeTranscriptContract
	default:
		return false
	}
}

func bindingAxis(provider agentcompat.Provider, stateValue agentcompat.State, reason agentcompat.Reason) agentcompat.Axis {
	contract := agentcompat.CodexBindingContract
	if provider == agentcompat.ProviderClaude {
		contract = agentcompat.ClaudeBindingContract
	}
	return agentcompat.Axis{State: stateValue, Contract: contract, Version: "1", Reason: reason}
}

func transcriptAxis(provider agentcompat.Provider, stateValue agentcompat.State, reason agentcompat.Reason, parser string) agentcompat.Axis {
	contract := agentcompat.CodexTranscriptContract
	if provider == agentcompat.ProviderClaude {
		contract = agentcompat.ClaudeTranscriptContract
	}
	return agentcompat.Axis{State: stateValue, Contract: contract, Version: parser, Reason: reason}
}

func (a *App) contextTurnLimit(program string) int {
	switch program {
	case recovery.ProgramCodex:
		return a.Config.CodexContextTurns
	case recovery.ProgramClaude:
		return a.Config.ClaudeContextTurns
	default:
		return 0
	}
}

func (a *App) loadSessionContext(metadata recovery.Metadata, limit int) (sessioncontext.Context, error) {
	switch metadata.Program {
	case recovery.ProgramCodex:
		if a.CodexContext == nil {
			return sessioncontext.Context{}, fmt.Errorf("Codex context reader unavailable")
		}
		return a.CodexContext.Load(metadata.SessionID, limit, a.redactText)
	case recovery.ProgramClaude:
		if a.ClaudeContext == nil || metadata.TranscriptPath == "" {
			return sessioncontext.Context{}, fmt.Errorf("Claude context reader unavailable")
		}
		return a.ClaudeContext.Load(metadata.TranscriptPath, metadata.SessionID, limit, a.redactText)
	default:
		return sessioncontext.Context{}, fmt.Errorf("unsupported context provider")
	}
}

func (a *App) detectContextRuntime(ctx context.Context, program string, capture tmux.StyledCapture) (sessionContextRuntime, error) {
	switch program {
	case recovery.ProgramCodex:
		if a.CodexDetector == nil {
			return sessionContextRuntime{}, fmt.Errorf("Codex detector unavailable")
		}
		runtime, err := a.CodexDetector.Detect(ctx, capture.PanePID, capture.CurrentCmd)
		return sessionContextRuntime{detected: runtime.Detected, supported: runtime.Supported, version: runtime.Version, identity: runtime.Identity, startedAt: runtime.StartedAt}, err
	case recovery.ProgramClaude:
		if a.ClaudeDetector == nil {
			return sessionContextRuntime{}, fmt.Errorf("Claude detector unavailable")
		}
		runtime, err := a.ClaudeDetector.Detect(ctx, capture.PanePID, capture.CurrentCmd)
		return sessionContextRuntime{detected: runtime.Detected, supported: runtime.Supported, version: runtime.Version, identity: runtime.Identity, startedAt: runtime.StartedAt}, err
	default:
		return sessionContextRuntime{}, fmt.Errorf("unsupported context provider")
	}
}

func (a *App) sessionRecoveryMetadata(ctx context.Context, expected state.TerminalSession) (recovery.Metadata, error) {
	tmuxCtx, cancel := tmux.TimeoutContext(ctx)
	defer cancel()
	return a.Tmux.RecoveryMetadata(tmux.BackgroundContext(tmuxCtx), expected.TmuxPaneID, expected.TmuxWindowID, expected.TmuxServerID)
}

func recoveryMetadataIdentity(metadata recovery.Metadata) string {
	return sha(strings.Join([]string{
		fmt.Sprintf("%d", metadata.Version), metadata.Program, metadata.SessionID, metadata.CWD,
		metadata.TranscriptPath, metadata.Model, metadata.Source,
		metadata.Observed.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
	}, "\x00"))
}

func (a *App) codexContextCurrent(ctx context.Context, expected state.TerminalSession, snapshot sessionContextSnapshot) bool {
	return a.sessionContextCurrent(ctx, expected, snapshot)
}

func (a *App) sessionContextCurrent(ctx context.Context, expected state.TerminalSession, snapshot sessionContextSnapshot) bool {
	if snapshot.bindingIdentity == "" && snapshot.runtimeIdentity == "" {
		return true
	}
	if snapshot.program == "" || snapshot.bindingIdentity == "" || snapshot.runtimeIdentity == "" || snapshot.panePID <= 0 {
		return false
	}
	runtime, err := a.detectContextRuntime(ctx, snapshot.program, tmux.StyledCapture{PanePID: snapshot.panePID, CurrentCmd: snapshot.currentCommand})
	if err != nil || runtime.identity == "" || runtime.identity != snapshot.runtimeIdentity {
		return false
	}
	metadata, err := a.sessionRecoveryMetadata(ctx, expected)
	if err != nil || recoveryMetadataIdentity(metadata) != snapshot.bindingIdentity {
		return false
	}
	latest, ok := a.Store.FindSession(expected.ID)
	return ok && latest.State == state.TerminalRunning && sameTerminalBinding(latest, expected) && latest.CreatedAt.Equal(expected.CreatedAt)
}

func (a *App) recordSessionContextDecision(sessionID int, program, version, outcome, reason, parser string, messages int, diagram bool) {
	signature := fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s\x00%d\x00%t", program, version, outcome, reason, parser, messages, diagram)
	if previous, loaded := a.sessionContextDiagnostics.Swap(sessionID, signature); loaded && previous == signature {
		return
	}
	event := "terminal.session_context"
	if program == recovery.ProgramCodex {
		event = "terminal.codex_context"
	} else if program == recovery.ProgramClaude {
		event = "terminal.claude_context"
	}
	_ = a.audit(event, outcome, map[string]any{
		"session_id": sessionID, "program": program, "version": version, "reason": reason, "parser": parser, "messages": messages, "diagram": diagram,
	})
}
