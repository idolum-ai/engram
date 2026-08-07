package agentdoctor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/agentcompat"
	"github.com/idolum-ai/engram/internal/claudeui"
	"github.com/idolum-ai/engram/internal/codexui"
	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/recovery"
	"github.com/idolum-ai/engram/internal/sessioncontext"
)

type fixedCodexDetector struct{ runtime codexui.Runtime }

func (detector fixedCodexDetector) Detect(context.Context, int, string) (codexui.Runtime, error) {
	return detector.runtime, nil
}

type fixedClaudeDetector struct{ runtime claudeui.Runtime }

func (detector fixedClaudeDetector) Detect(context.Context, int, string) (claudeui.Runtime, error) {
	return detector.runtime, nil
}

type doctorRunner struct {
	metadata string
	frame    string
	calls    [][]string
}

func (runner *doctorRunner) Run(_ context.Context, args ...string) (string, error) {
	runner.calls = append(runner.calls, append([]string(nil), args...))
	joined := strings.Join(args, " ")
	switch {
	case args[0] == "display-message" && strings.Contains(joined, "window_name"):
		return doctorRecord("$1", "main", "0", "@2", "agent", "1", "%4", "/private/work", "claude"), nil
	case args[0] == "show-options" && strings.Contains(joined, "-gqv"):
		return strings.Repeat("a", 32) + "\n", nil
	case args[0] == "display-message" && strings.Contains(joined, "#{pane_pid}\x1f"):
		return "4242\x1fclaude\n", nil
	case args[0] == "display-message" && strings.Contains(joined, "pane_pid"):
		return doctorRecord(strings.Repeat("a", 32), "@2", "%4", "4242", "80", "24", "private", "/private/work", "claude", "on", "off"), nil
	case args[0] == "display-message" && strings.Contains(joined, "@engram_server_id"):
		return doctorRecord(strings.Repeat("a", 32), "$1", "@2", "%4", "main", "0", "0", "1", "/private/work", "claude"), nil
	case args[0] == "show-options" && strings.Contains(joined, "@engram_recovery"):
		return runner.metadata + "\n", nil
	case args[0] == "capture-pane":
		return doctorRecord(strings.Repeat("a", 32), "@2", "%4", "4242", "80", "24", "private", "/private/work", "claude", "on", "off"), nil
	case args[0] == "show-buffer":
		return runner.frame, nil
	case args[0] == "delete-buffer":
		return "", nil
	default:
		return "", fmt.Errorf("unexpected tmux call: %v", args)
	}
}

func doctorRecord(values ...string) string {
	var result strings.Builder
	for _, value := range values {
		fmt.Fprintf(&result, "%d:%s", len(value), value)
	}
	result.WriteByte('\n')
	return result.String()
}

type fixedCodexReader struct {
	context sessioncontext.Context
	err     error
}

func TestDoctorMapsAmbiguousAndMissingProbeReasons(t *testing.T) {
	if got := processErrorReason(errors.New("multiple matching processes")); got != agentcompat.ReasonProcessAmbiguous {
		t.Fatalf("process reason = %q", got)
	}
	if got := transcriptErrorReason(errors.New("exact rollout is unavailable or ambiguous")); got != agentcompat.ReasonTranscriptAmbiguous {
		t.Fatalf("transcript reason = %q", got)
	}
	if got := transcriptErrorReason(errors.New("file not found")); got != agentcompat.ReasonTranscriptMissing {
		t.Fatalf("missing transcript reason = %q", got)
	}
	if state, reason := transcriptErrorState(errors.New("unrecognized message structure")); state != agentcompat.StateUnsupported || reason != agentcompat.ReasonTranscriptUnsupported {
		t.Fatalf("unsupported transcript = %q/%q", state, reason)
	}
}

func TestDoctorDistinguishesBindingMissingUnsupportedAndStale(t *testing.T) {
	provider := agentcompat.ProviderClaude
	if got := bindingProbeAxis(provider, recovery.Metadata{}, nil, false); got.State != agentcompat.StateMissing || got.Reason != agentcompat.ReasonBindingMissing {
		t.Fatalf("missing = %#v", got)
	}
	if got := bindingProbeAxis(provider, recovery.Metadata{}, errors.New("unsupported recovery metadata version"), false); got.State != agentcompat.StateUnsupported || got.Reason != agentcompat.ReasonBindingUnsupported {
		t.Fatalf("unsupported = %#v", got)
	}
	if got := bindingProbeAxis(provider, recovery.Metadata{}, errors.New("tmux unavailable"), false); got.State != agentcompat.StateUnavailable || got.Reason != agentcompat.ReasonProbeUnavailable {
		t.Fatalf("unavailable = %#v", got)
	}
	if got := bindingProbeAxis(provider, recovery.Metadata{Program: recovery.ProgramClaude}, nil, false); got.State != agentcompat.StateStale || got.Reason != agentcompat.ReasonBindingStale {
		t.Fatalf("stale = %#v", got)
	}
}

func TestDoctorUsesProductionProbePathForBothProvidersWithoutMutationOrDisclosure(t *testing.T) {
	started := time.Now().Add(-time.Minute).UTC()
	for _, provider := range []agentcompat.Provider{agentcompat.ProviderCodex, agentcompat.ProviderClaude} {
		t.Run(string(provider), func(t *testing.T) {
			metadata := recovery.Metadata{Version: 1, Program: string(provider), SessionID: "019f7607-c8b0-74b3-87ca-64a7e6e7ede0", Observed: started.Add(time.Second)}
			frame := "• Current work is visible.\n\ngpt-5.6-sol high · /private/work"
			if provider == agentcompat.ProviderClaude {
				metadata.TranscriptPath = "/private/019f7607-c8b0-74b3-87ca-64a7e6e7ede0.jsonl"
				metadata.Model = "claude-opus-4-8"
				frame = "⏺ Current work is visible.\n\n────────────\n❯\n────────────\n  ⏸ manual mode on · ? for shortcuts"
			}
			encoded, err := recovery.Encode(metadata)
			if err != nil {
				t.Fatal(err)
			}
			runner := &doctorRunner{metadata: encoded, frame: frame}
			doctor := Doctor{Runner: runner, Options: config.AgentOptions{},
				CodexDetector:  fixedCodexDetector{runtime: codexui.Runtime{Detected: true, Supported: true, Version: codexui.SupportedVersion, Identity: strings.Repeat("b", 64), StartedAt: started}},
				ClaudeDetector: fixedClaudeDetector{runtime: claudeui.Runtime{Detected: true, Supported: true, Version: claudeui.SupportedVersion, Identity: strings.Repeat("c", 64), PID: 4242, StartedAt: started}},
				CodexContext:   fixedCodexReader{}, ClaudeContext: fixedClaudeReader{},
			}
			var out strings.Builder
			if err := doctor.Run(context.Background(), []string{"agent", "--provider", string(provider), "--pane", "%4"}, &out); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), agentcompat.DisplayName(provider)+" integration") {
				t.Fatalf("output = %s", out.String())
			}
			for _, forbidden := range []string{"/private", metadata.SessionID, "Current work is visible", "set-option", "send-keys"} {
				if strings.Contains(out.String(), forbidden) {
					t.Fatalf("doctor output disclosed %q: %s", forbidden, out.String())
				}
			}
			for _, call := range runner.calls {
				command := strings.Join(call, " ")
				if strings.Contains(command, "set-option") || strings.Contains(command, "send-keys") || strings.Contains(command, "new-window") || strings.Contains(command, "kill-") {
					t.Fatalf("doctor mutated tmux: %s", command)
				}
			}
		})
	}
}

func (reader fixedCodexReader) Load(string, int, ...func(string) string) (sessioncontext.Context, error) {
	return reader.context, reader.err
}

type fixedClaudeReader struct {
	context sessioncontext.Context
	err     error
}

func (reader fixedClaudeReader) Load(string, string, int, ...func(string) string) (sessioncontext.Context, error) {
	return reader.context, reader.err
}

func TestProbeTranscriptAxesAreIndependentOfScreenSupport(t *testing.T) {
	sessionID := "019f7607-c8b0-74b3-87ca-64a7e6e7ede0"
	tests := []struct {
		name     string
		provider agentcompat.Provider
		options  config.AgentOptions
		context  sessioncontext.Context
		want     agentcompat.State
	}{
		{"codex disabled", agentcompat.ProviderCodex, config.AgentOptions{}, sessioncontext.Context{}, agentcompat.StateDisabled},
		{"codex eligible", agentcompat.ProviderCodex, config.AgentOptions{CodexContextTurns: 3}, sessioncontext.Context{Parser: agentcompat.CodexTranscriptContract}, agentcompat.StateEligible},
		{"claude eligible", agentcompat.ProviderClaude, config.AgentOptions{ClaudeContextTurns: 4}, sessioncontext.Context{Parser: agentcompat.ClaudeTranscriptContract}, agentcompat.StateEligible},
		{"claude parser unsupported", agentcompat.ProviderClaude, config.AgentOptions{ClaudeContextTurns: 4}, sessioncontext.Context{Parser: "future-parser"}, agentcompat.StateUnsupported},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			doctor := Doctor{Options: test.options, CodexContext: fixedCodexReader{context: test.context}, ClaudeContext: fixedClaudeReader{context: test.context}}
			report := Report{Compatibility: agentcompat.Compatibility{Provider: test.provider, Screen: agentcompat.Axis{State: agentcompat.StateLiteral, Reason: agentcompat.ReasonScreenVersionUnknown}}}
			doctor.probeTranscript(&report, recovery.Metadata{SessionID: sessionID, TranscriptPath: "/private/transcript.jsonl"}, true)
			if report.Compatibility.Transcript.State != test.want || report.Compatibility.Screen.State != agentcompat.StateLiteral {
				t.Fatalf("report = %#v", report)
			}
		})
	}
}

func TestProbeTranscriptDistinguishesMissingBinding(t *testing.T) {
	report := Report{Compatibility: agentcompat.Compatibility{Provider: agentcompat.ProviderClaude}}
	Doctor{Options: config.AgentOptions{ClaudeContextTurns: 4}}.probeTranscript(&report, recovery.Metadata{}, false)
	if got := report.Compatibility.Transcript; got.State != agentcompat.StateUnavailable || got.Reason != agentcompat.ReasonBindingMissing {
		t.Fatalf("transcript axis = %#v", got)
	}
}

func TestFormatIsBoundedAndPrivacySafeForBothProviders(t *testing.T) {
	for _, provider := range []agentcompat.Provider{agentcompat.ProviderCodex, agentcompat.ProviderClaude} {
		report := Report{Compatibility: agentcompat.Compatibility{
			Provider: provider,
			Process:  agentcompat.Axis{State: agentcompat.StateProven, Version: "2.1.224"},
			Screen:   agentcompat.Axis{State: agentcompat.StateSupported}, Binding: agentcompat.Axis{State: agentcompat.StateProven},
			Transcript: agentcompat.Axis{State: agentcompat.StateEligible},
		}, Presentation: agentcompat.Presentation{Model: agentcompat.Value{Value: "claude-opus-4-8", Provenance: agentcompat.ProvenanceHook}}, Parser: "claude-transcript-v1", ContextTurns: 4}
		got := Format(report)
		for _, forbidden := range []string{"019f7607-c8b0-74b3-87ca-64a7e6e7ede0", "/Users/example/repo", "private task", "agent-name"} {
			if strings.Contains(got, forbidden) {
				t.Fatalf("%s output leaked %q: %s", provider, forbidden, got)
			}
		}
		if !strings.Contains(got, agentcompat.DisplayName(provider)+" integration") || !strings.Contains(got, "Transcript") {
			t.Fatalf("output = %s", got)
		}
		if !strings.Contains(got, "Model source") || !strings.Contains(got, "hook") {
			t.Fatalf("model provenance missing: %s", got)
		}
	}
}

func TestParseArgsRequiresExactPaneAndKnownProvider(t *testing.T) {
	if _, _, err := parseArgs([]string{"agent", "--provider", "future"}, "%4"); err == nil {
		t.Fatal("unknown provider accepted")
	}
	if provider, pane, err := parseArgs([]string{"agent", "--provider", "claude"}, "%4"); err != nil || provider != agentcompat.ProviderClaude || pane != "%4" {
		t.Fatalf("parse = %q %q %v", provider, pane, err)
	}
}
