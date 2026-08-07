// Package agentdoctor provides read-only, provider-neutral local diagnostics.
package agentdoctor

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/idolum-ai/engram/internal/agentcompat"
	"github.com/idolum-ai/engram/internal/agentui"
	"github.com/idolum-ai/engram/internal/claudecontext"
	"github.com/idolum-ai/engram/internal/claudeui"
	"github.com/idolum-ai/engram/internal/codexcontext"
	"github.com/idolum-ai/engram/internal/codexui"
	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/recovery"
	"github.com/idolum-ai/engram/internal/sessioncontext"
	"github.com/idolum-ai/engram/internal/tmux"
)

const captureRows = 96

type codexDetector interface {
	Detect(context.Context, int, string) (codexui.Runtime, error)
}

type claudeDetector interface {
	Detect(context.Context, int, string) (claudeui.Runtime, error)
}

type codexReader interface {
	Load(string, int, ...func(string) string) (sessioncontext.Context, error)
}

type claudeReader interface {
	Load(string, string, int, ...func(string) string) (sessioncontext.Context, error)
}

type Doctor struct {
	Runner         tmux.Runner
	CodexDetector  codexDetector
	ClaudeDetector claudeDetector
	CodexContext   codexReader
	ClaudeContext  claudeReader
	Options        config.AgentOptions
	InheritedPane  string
}

type Report struct {
	Compatibility agentcompat.Compatibility
	Presentation  agentcompat.Presentation
	Parser        string
	ContextTurns  int
}

type UsageError struct{ Message string }

func (e UsageError) Error() string { return e.Message }
func IsUsageError(err error) bool {
	var usage UsageError
	return errors.As(err, &usage)
}

func New(runner tmux.Runner, options config.AgentOptions, pane string) Doctor {
	return Doctor{
		Runner: runner, Options: options, InheritedPane: pane,
		CodexDetector: codexui.NewDetector(), ClaudeDetector: claudeui.NewDetector(),
		CodexContext: codexcontext.Reader{SessionsRoot: codexcontext.DefaultSessionsRoot()}, ClaudeContext: claudecontext.Reader{},
	}
}

func (doctor Doctor) Run(ctx context.Context, args []string, out io.Writer) error {
	if out == nil || doctor.Runner == nil {
		return fmt.Errorf("agent doctor is unavailable")
	}
	provider, pane, err := parseArgs(args, doctor.InheritedPane)
	if err != nil {
		return err
	}
	report, err := doctor.Probe(ctx, provider, pane)
	if err != nil {
		return err
	}
	_, err = io.WriteString(out, Format(report))
	return err
}

func parseArgs(args []string, inheritedPane string) (agentcompat.Provider, string, error) {
	if len(args) == 0 || args[0] != "agent" {
		return "", "", UsageError{Message: "usage: engram doctor agent [--provider codex|claude] [--pane %N]"}
	}
	set := flag.NewFlagSet("doctor agent", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	providerValue := set.String("provider", "", "provider")
	pane := set.String("pane", strings.TrimSpace(inheritedPane), "tmux pane")
	if err := set.Parse(args[1:]); err != nil || len(set.Args()) != 0 {
		return "", "", UsageError{Message: "usage: engram doctor agent [--provider codex|claude] [--pane %N]"}
	}
	provider := agentcompat.Provider(strings.ToLower(strings.TrimSpace(*providerValue)))
	if provider != "" && !agentcompat.ValidProvider(provider) {
		return "", "", UsageError{Message: "--provider must be codex or claude"}
	}
	if strings.TrimSpace(*pane) == "" {
		return "", "", UsageError{Message: "--pane is required outside tmux"}
	}
	return provider, strings.TrimSpace(*pane), nil
}

func (doctor Doctor) Probe(ctx context.Context, requested agentcompat.Provider, paneID string) (Report, error) {
	manager := tmux.New(doctor.Runner)
	probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	window, err := manager.ResolveTarget(probeCtx, paneID)
	if err != nil || window.PaneID != paneID {
		return Report{}, fmt.Errorf("doctor agent: exact tmux pane is unavailable")
	}
	serverID, err := manager.CurrentServerID(probeCtx)
	if err != nil {
		return Report{}, fmt.Errorf("doctor agent: tmux server identity is unavailable")
	}
	panePID, foreground, err := manager.PaneProcess(probeCtx, paneID)
	if err != nil {
		return Report{}, fmt.Errorf("doctor agent: pane process metadata is unavailable")
	}
	provider, codexRuntime, claudeRuntime, detectErr := doctor.detect(probeCtx, requested, panePID, foreground)
	if provider == "" {
		if requested != "" {
			provider = requested
		} else {
			return Report{}, fmt.Errorf("doctor agent: no supported provider process was identified")
		}
	}
	report := Report{Compatibility: agentcompat.Compatibility{Provider: provider}}
	startedAt := time.Time{}
	identity := ""
	version := ""
	if provider == agentcompat.ProviderClaude {
		startedAt, identity, version = claudeRuntime.StartedAt, claudeRuntime.Identity, claudeRuntime.Version
	} else {
		startedAt, identity, version = codexRuntime.StartedAt, codexRuntime.Identity, codexRuntime.Version
	}
	if detectErr != nil {
		report.Compatibility.Process = processAxis(provider, agentcompat.StateUnavailable, processErrorReason(detectErr), version)
	} else if identity == "" || startedAt.IsZero() {
		report.Compatibility.Process = processAxis(provider, agentcompat.StateUnavailable, agentcompat.ReasonProcessIdentityUnproven, version)
	} else {
		report.Compatibility.Process = processAxis(provider, agentcompat.StateProven, agentcompat.ReasonNone, version)
	}

	metadata, metadataErr := manager.RecoveryMetadata(probeCtx, paneID, window.ID, serverID)
	bindingProven := metadataErr == nil && metadata.Program == string(provider) && recovery.ValidSessionID(metadata.SessionID) && !startedAt.IsZero() && !metadata.Observed.Before(startedAt)
	report.Compatibility.Binding = bindingProbeAxis(provider, metadata, metadataErr, bindingProven)

	capture, captureErr := manager.CaptureStyled(probeCtx, paneID, captureRows)
	if captureErr != nil || report.Compatibility.Process.State != agentcompat.StateProven {
		report.Compatibility.Screen = screenAxis(provider, version, agentcompat.StateLiteral, agentcompat.ReasonProbeUnavailable)
	} else if provider == agentcompat.ProviderClaude {
		if !claudeRuntime.Supported {
			report.Compatibility.Screen = screenAxis(provider, version, agentcompat.StateLiteral, agentcompat.ReasonScreenVersionUnknown)
		} else {
			model := ""
			if bindingProven {
				model = metadata.Model
			}
			analysis := claudeui.Analyze(claudeRuntime, agentui.Observation{Current: frame(capture)}, model)
			if analysis.Applied {
				report.Compatibility.Screen = screenAxis(provider, version, agentcompat.StateSupported, agentcompat.ReasonNone)
				report.Presentation = presentationFromAnalysis(analysis, model)
			} else {
				report.Compatibility.Screen = screenAxis(provider, version, agentcompat.StateLiteral, agentcompat.ReasonScreenLayoutUnknown)
			}
		}
	} else {
		presentation := codexui.Present(codexRuntime, capture.JoinedText)
		if presentation.Applied {
			report.Compatibility.Screen = screenAxis(provider, version, agentcompat.StateSupported, agentcompat.ReasonNone)
			report.Presentation = agentcompat.NormalizePresentation(agentcompat.Presentation{
				Model: value(presentation.Model, agentcompat.ProvenanceVisibleUI), Effort: value(presentation.Effort, agentcompat.ProvenanceVisibleUI),
				Interaction: value(presentation.Mode, agentcompat.ProvenanceVisibleUI), Activity: normalizeActivity(presentation.Activity), LastTurnSeconds: presentation.LastTurnSeconds,
			})
		} else {
			reason := agentcompat.ReasonScreenLayoutUnknown
			if !codexRuntime.Supported {
				reason = agentcompat.ReasonScreenVersionUnknown
			}
			report.Compatibility.Screen = screenAxis(provider, version, agentcompat.StateLiteral, reason)
		}
	}
	doctor.probeTranscript(&report, metadata, bindingProven)
	report.Compatibility = agentcompat.NormalizeCompatibility(report.Compatibility)
	return report, nil
}

func (doctor Doctor) detect(ctx context.Context, requested agentcompat.Provider, panePID int, foreground string) (agentcompat.Provider, codexui.Runtime, claudeui.Runtime, error) {
	if requested == agentcompat.ProviderClaude || requested == "" {
		runtime, err := doctor.ClaudeDetector.Detect(ctx, panePID, foreground)
		if runtime.Detected || requested == agentcompat.ProviderClaude {
			return agentcompat.ProviderClaude, codexui.Runtime{}, runtime, err
		}
	}
	if requested == agentcompat.ProviderCodex || requested == "" {
		runtime, err := doctor.CodexDetector.Detect(ctx, panePID, foreground)
		if runtime.Detected || requested == agentcompat.ProviderCodex {
			return agentcompat.ProviderCodex, runtime, claudeui.Runtime{}, err
		}
	}
	return "", codexui.Runtime{}, claudeui.Runtime{}, nil
}

func (doctor Doctor) probeTranscript(report *Report, metadata recovery.Metadata, bindingProven bool) {
	provider := report.Compatibility.Provider
	limit := doctor.Options.CodexContextTurns
	if provider == agentcompat.ProviderClaude {
		limit = doctor.Options.ClaudeContextTurns
	}
	report.ContextTurns = limit
	if limit <= 0 {
		report.Compatibility.Transcript = transcriptAxis(provider, agentcompat.StateDisabled, agentcompat.ReasonContextDisabled, "")
		return
	}
	if !bindingProven {
		reason := report.Compatibility.Binding.Reason
		if reason == agentcompat.ReasonNone {
			reason = agentcompat.ReasonBindingMissing
		}
		report.Compatibility.Transcript = transcriptAxis(provider, agentcompat.StateUnavailable, reason, "")
		return
	}
	var context sessioncontext.Context
	var err error
	if provider == agentcompat.ProviderClaude {
		context, err = doctor.ClaudeContext.Load(metadata.TranscriptPath, metadata.SessionID, limit)
	} else {
		context, err = doctor.CodexContext.Load(metadata.SessionID, limit)
	}
	if err != nil {
		state, reason := transcriptErrorState(err)
		report.Compatibility.Transcript = transcriptAxis(provider, state, reason, "")
		return
	}
	want := agentcompat.CodexTranscriptContract
	if provider == agentcompat.ProviderClaude {
		want = agentcompat.ClaudeTranscriptContract
	}
	if context.Parser != want {
		report.Compatibility.Transcript = transcriptAxis(provider, agentcompat.StateUnsupported, agentcompat.ReasonTranscriptUnsupported, context.Parser)
		return
	}
	report.Parser = context.Parser
	report.Compatibility.Transcript = transcriptAxis(provider, agentcompat.StateEligible, agentcompat.ReasonTranscriptEligible, context.Parser)
}

func Format(report Report) string {
	provider := report.Compatibility.Provider
	var out strings.Builder
	fmt.Fprintf(&out, "%s integration\n%s\n", agentcompat.DisplayName(provider), strings.Repeat("─", 40))
	writeAxis(&out, "Process", report.Compatibility.Process)
	if report.Compatibility.Process.Version != "" {
		fmt.Fprintf(&out, "Version          %s\n", report.Compatibility.Process.Version)
	}
	writeAxis(&out, "Screen grammar", report.Compatibility.Screen)
	if line := presentationLine(provider, report.Presentation); line != "" {
		fmt.Fprintf(&out, "Presentation     %s\n", line)
	}
	if report.Presentation.Model.Value != "" {
		fmt.Fprintf(&out, "Model source     %s\n", provenanceDisplay(report.Presentation.Model.Provenance))
	}
	out.WriteByte('\n')
	writeAxis(&out, "Session hook", report.Compatibility.Binding)
	writeAxis(&out, "Transcript", report.Compatibility.Transcript)
	if report.Parser != "" {
		fmt.Fprintf(&out, "Parser           %s\n", report.Parser)
	}
	if report.Compatibility.Transcript.State == agentcompat.StateDisabled {
		fmt.Fprintf(&out, "Context          disabled (%d turns)\n", report.ContextTurns)
	} else if report.Compatibility.Transcript.State == agentcompat.StateEligible {
		fmt.Fprintf(&out, "Context          ✓ enabled (%d turns)\n", report.ContextTurns)
	}
	if report.Compatibility.Binding.State == agentcompat.StateMissing {
		fmt.Fprintf(&out, "\nSuggested action:\nInstall the SessionStart hook, then restart %s.\n", agentcompat.DisplayName(provider))
	}
	return out.String()
}

func writeAxis(out *strings.Builder, label string, axis agentcompat.Axis) {
	mark := "—"
	if axis.State == agentcompat.StateProven || axis.State == agentcompat.StateSupported || axis.State == agentcompat.StateEligible {
		mark = "✓"
	} else if axis.State == agentcompat.StateUnavailable || axis.State == agentcompat.StateMissing || axis.State == agentcompat.StateStale || axis.State == agentcompat.StateUnsupported {
		mark = "✗"
	} else if axis.State == agentcompat.StateLiteral || axis.State == agentcompat.StateDisabled {
		mark = "○"
	}
	text := strings.ReplaceAll(string(axis.State), "_", " ")
	if axis.Reason != agentcompat.ReasonNone {
		text = strings.ReplaceAll(string(axis.Reason), "_", " ")
	}
	if text == "" {
		text = "not checked"
	}
	fmt.Fprintf(out, "%-16s %s %s\n", label, mark, text)
}

func presentationLine(provider agentcompat.Provider, presentation agentcompat.Presentation) string {
	parts := make([]string, 0, 4)
	if presentation.Model.Value != "" {
		parts = append(parts, modelDisplay(provider, presentation.Model.Value))
	}
	if presentation.Effort.Value != "" {
		parts = append(parts, presentation.Effort.Value)
	}
	if presentation.Interaction.Value != "" {
		parts = append(parts, presentation.Interaction.Value)
	}
	if presentation.Activity != "" {
		parts = append(parts, presentation.Activity)
	}
	return strings.Join(parts, " · ")
}

func modelDisplay(provider agentcompat.Provider, model string) string {
	if provider != agentcompat.ProviderClaude {
		return model
	}
	parts := strings.Split(strings.TrimPrefix(model, "claude-"), "-")
	if len(parts) < 3 {
		return model
	}
	family := parts[0]
	if family != "opus" && family != "sonnet" && family != "haiku" && family != "fable" {
		return model
	}
	return strings.ToUpper(family[:1]) + family[1:] + " " + strings.Join(parts[1:], ".")
}

func processAxis(provider agentcompat.Provider, state agentcompat.State, reason agentcompat.Reason, version string) agentcompat.Axis {
	contract := agentcompat.CodexProcessContract
	if provider == agentcompat.ProviderClaude {
		contract = agentcompat.ClaudeProcessContract
	}
	return agentcompat.Axis{State: state, Contract: contract, Version: version, Reason: reason}
}

func bindingAxis(provider agentcompat.Provider, state agentcompat.State, reason agentcompat.Reason) agentcompat.Axis {
	contract := agentcompat.CodexBindingContract
	if provider == agentcompat.ProviderClaude {
		contract = agentcompat.ClaudeBindingContract
	}
	return agentcompat.Axis{State: state, Contract: contract, Version: "1", Reason: reason}
}

func bindingProbeAxis(provider agentcompat.Provider, metadata recovery.Metadata, err error, proven bool) agentcompat.Axis {
	if proven {
		return bindingAxis(provider, agentcompat.StateProven, agentcompat.ReasonNone)
	}
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "unsupported") {
		return bindingAxis(provider, agentcompat.StateUnsupported, agentcompat.ReasonBindingUnsupported)
	}
	if err != nil {
		return bindingAxis(provider, agentcompat.StateUnavailable, agentcompat.ReasonProbeUnavailable)
	}
	if metadata.Program == "" {
		return bindingAxis(provider, agentcompat.StateMissing, agentcompat.ReasonBindingMissing)
	}
	return bindingAxis(provider, agentcompat.StateStale, agentcompat.ReasonBindingStale)
}

func screenAxis(provider agentcompat.Provider, version string, state agentcompat.State, reason agentcompat.Reason) agentcompat.Axis {
	contract := agentcompat.CodexScreenContract
	if provider == agentcompat.ProviderClaude {
		contract = agentcompat.ClaudeScreenContract
	}
	return agentcompat.Axis{State: state, Contract: contract, Version: version, Reason: reason}
}

func transcriptAxis(provider agentcompat.Provider, state agentcompat.State, reason agentcompat.Reason, parser string) agentcompat.Axis {
	contract := agentcompat.CodexTranscriptContract
	if provider == agentcompat.ProviderClaude {
		contract = agentcompat.ClaudeTranscriptContract
	}
	return agentcompat.Axis{State: state, Contract: contract, Version: parser, Reason: reason}
}

func frame(capture tmux.StyledCapture) agentui.Frame {
	return agentui.Frame{Text: capture.JoinedText, CurrentCommand: capture.CurrentCmd, Columns: capture.Columns, VisibleRows: capture.VisibleRows, AlternateScreen: capture.AlternateOn, CopyMode: capture.PaneInMode}
}

func value(text string, provenance agentcompat.Provenance) agentcompat.Value {
	if text == "" {
		return agentcompat.Value{}
	}
	return agentcompat.Value{Value: text, Provenance: provenance}
}

func presentationFromAnalysis(analysis agentui.Analysis, declaredModel string) agentcompat.Presentation {
	provenance := agentcompat.ProvenanceRetainedUI
	if analysis.ModelObserved {
		provenance = agentcompat.ProvenanceVisibleUI
	} else if declaredModel != "" && analysis.Model == declaredModel {
		provenance = agentcompat.ProvenanceHook
	}
	return agentcompat.NormalizePresentation(agentcompat.Presentation{
		Model: value(analysis.Model, provenance), Effort: value(analysis.Effort, agentcompat.ProvenanceVisibleUI),
		Interaction: value(analysis.Mode, agentcompat.ProvenanceVisibleUI), Activity: string(analysis.Activity),
		LastTurnSeconds: analysis.LastTurnSeconds, AgentTotal: analysis.AgentTotal, AgentActive: analysis.AgentActive,
	})
}

func normalizeActivity(value string) string {
	if value == "working" {
		return "active"
	}
	if value == "reviewing approval" {
		return "awaiting_approval"
	}
	return value
}

func processErrorReason(err error) agentcompat.Reason {
	text := strings.ToLower(errorText(err))
	if strings.Contains(text, "ambiguous") || strings.Contains(text, "multiple") || strings.Contains(text, "more than one") {
		return agentcompat.ReasonProcessAmbiguous
	}
	if strings.Contains(text, "not found") || strings.Contains(text, "no ") {
		return agentcompat.ReasonProcessNotFound
	}
	return agentcompat.ReasonProbeUnavailable
}

func transcriptErrorReason(err error) agentcompat.Reason {
	text := strings.ToLower(errorText(err))
	if strings.Contains(text, "ambiguous") || strings.Contains(text, "multiple") {
		return agentcompat.ReasonTranscriptAmbiguous
	}
	return agentcompat.ReasonTranscriptMissing
}

func transcriptErrorState(err error) (agentcompat.State, agentcompat.Reason) {
	text := strings.ToLower(errorText(err))
	if strings.Contains(text, "unsupported") || strings.Contains(text, "unrecognized") || strings.Contains(text, "unexpected") {
		return agentcompat.StateUnsupported, agentcompat.ReasonTranscriptUnsupported
	}
	return agentcompat.StateUnavailable, transcriptErrorReason(err)
}

func provenanceDisplay(value agentcompat.Provenance) string {
	switch value {
	case agentcompat.ProvenanceHook:
		return "hook"
	case agentcompat.ProvenanceVisibleUI:
		return "visible UI"
	case agentcompat.ProvenanceRetainedUI:
		return "retained same-incarnation UI"
	default:
		return "unknown"
	}
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
