package claudeui

import (
	"strings"
	"testing"

	"github.com/idolum-ai/engram/internal/agentui"
)

func supportedRuntime(identity string) Runtime {
	return Runtime{Detected: true, Supported: true, Version: SupportedVersion, Identity: identity}
}

func claudeFrame(text string) agentui.Frame {
	return agentui.Frame{Text: text, CurrentCommand: "claude", Columns: 80, VisibleRows: 24, AlternateScreen: "on", CopyMode: "off"}
}

func TestAnalyzeLearnsVisibleModelAndRemovesClaudeChrome(t *testing.T) {
	input := strings.Join([]string{
		"╭──────────────────────────────────╮",
		"│ Opus 4.8 · API Usage Billing     │",
		"│ /workspace                       │",
		"╰──────────────────────────────────╯",
		"",
		"❯ Review the fixture.",
		"",
		"⏺ The implementation is ready.",
		"",
		"✻ Crunched for 3s",
		"",
		"────────────────────────────────────",
		"❯",
		"────────────────────────────────────",
		"  bypass permissions on · ● high · /effort",
	}, "\n")
	got := Analyze(supportedRuntime("runtime-a"), agentui.Observation{Current: claudeFrame(input)}, "")
	if !got.Applied || got.Model != "claude-opus-4-8" || got.Effort != "high" || got.Activity != agentui.ActivityIdle {
		t.Fatalf("analysis = %#v", got)
	}
	for _, unwanted := range []string{"API Usage Billing", "Crunched for", "bypass permissions", "────"} {
		if strings.Contains(got.Conversation, unwanted) {
			t.Fatalf("conversation retained %q: %q", unwanted, got.Conversation)
		}
	}
}

func TestAnalyzeRetainsVerifiedModelAfterCardScrollsAway(t *testing.T) {
	input := strings.Join([]string{
		"⏺ All repository checks pass.",
		"",
		"✻ Brewed for 8m 54s",
		"",
		"9 tasks (8 done, 1 open)",
		"… +4 completed",
		"new task? /clear to save 484k tokens",
		"",
		"────────────────────────────────────",
		"❯ draft the next contract",
		"────────────────────────────────────",
		"  bypass permissions on · ● high · /effort",
	}, "\n")
	got := Analyze(supportedRuntime("runtime-a"), agentui.Observation{Current: claudeFrame(input)}, "claude-opus-4-8")
	if !got.Applied || got.Model != "claude-opus-4-8" || got.Activity != agentui.ActivityIdle {
		t.Fatalf("analysis = %#v", got)
	}
	for _, unwanted := range []string{"Brewed for", "/clear to save", "bypass permissions", "────"} {
		if strings.Contains(got.Conversation, unwanted) {
			t.Fatalf("conversation retained %q: %q", unwanted, got.Conversation)
		}
	}
	if !strings.Contains(got.Conversation, "draft the next contract") || !strings.Contains(got.Conversation, "All repository checks pass") {
		t.Fatalf("conversation lost evidence: %q", got.Conversation)
	}
}

func TestAnalyzeReportsActivityWithoutGuessingModel(t *testing.T) {
	input := strings.Join([]string{
		"⏺ Running the complete repository gate.",
		"",
		"✻ Deliberating…",
		"",
		"────────────────────────────────────",
		"❯",
		"────────────────────────────────────",
		"  bypass permissions on · ● high · /effort",
	}, "\n")
	got := Analyze(supportedRuntime("runtime-a"), agentui.Observation{Current: claudeFrame(input)}, "")
	if !got.Applied || got.Model != "" || got.Effort != "high" || got.Activity != agentui.ActivityActive {
		t.Fatalf("analysis = %#v", got)
	}
	if strings.Contains(got.Conversation, "Deliberating") {
		t.Fatalf("conversation retained active chrome: %q", got.Conversation)
	}
}

func TestAnalyzeRecognizesClaudeApprovalAndModelSwitch(t *testing.T) {
	approval := strings.Join([]string{
		"⏺ I need to run the release check.",
		"",
		"Do you want to allow this command?",
		"",
		"────────────────────────────────────",
		"❯",
		"────────────────────────────────────",
		"  bypass permissions on · ● high · /effort",
	}, "\n")
	got := Analyze(supportedRuntime("runtime-a"), agentui.Observation{Current: claudeFrame(approval)}, "claude-fable-5")
	if !got.Applied || got.Activity != agentui.ActivityAwaitingApproval || !strings.Contains(got.Conversation, "Do you want to allow") {
		t.Fatalf("approval analysis = %#v", got)
	}

	switched := strings.Join([]string{
		"╭──────────────────────────────────╮",
		"│ Opus 4.8 · API Usage Billing     │",
		"╰──────────────────────────────────╯",
		"",
		"⏺ Continuing with the new model.",
		"",
		"────────────────────────────────────",
		"❯",
		"────────────────────────────────────",
		"  bypass permissions on · ● high · /effort",
	}, "\n")
	got = Analyze(supportedRuntime("runtime-a"), agentui.Observation{Current: claudeFrame(switched)}, "claude-fable-5")
	if !got.Applied || got.Model != "claude-opus-4-8" {
		t.Fatalf("model switch analysis = %#v", got)
	}
}

func TestAnalyzeRecognizesObservedFableModelCard(t *testing.T) {
	input := strings.Join([]string{
		"╭──────────────────────────────────╮",
		"│ Fable 5 · promotional access     │",
		"╰──────────────────────────────────╯",
		"",
		"⏺ Waiting for input.",
		"",
		"────────────────────────────────────",
		"❯",
		"────────────────────────────────────",
		"  bypass permissions on · ● high · /effort",
	}, "\n")
	got := Analyze(supportedRuntime("runtime-a"), agentui.Observation{Current: claudeFrame(input)}, "")
	if !got.Applied || got.Model != "claude-fable-5" || got.Activity != agentui.ActivityIdle {
		t.Fatalf("Fable analysis = %#v", got)
	}
}

func TestAnalyzeFailsClosedForUnsupportedRuntimeAndUnknownLayout(t *testing.T) {
	input := "ordinary terminal\nfuture Claude layout"
	tests := []Runtime{
		{Detected: true, Version: "2.1.220", Identity: "runtime-a"},
		supportedRuntime("runtime-a"),
	}
	for _, runtime := range tests {
		got := Analyze(runtime, agentui.Observation{Current: claudeFrame(input)}, "claude-opus-4-8")
		if got.Applied || got.Conversation != input || got.Activity != agentui.ActivityUnknown {
			t.Fatalf("fallback = %#v", got)
		}
	}
}

func TestAnalyzeRejectsRememberedModelWithoutClaudeStatusControl(t *testing.T) {
	input := "⏺ Confidence report\n\nresult · high confidence\n\n────────────────────\n❯\n────────────────────"
	got := Analyze(supportedRuntime("runtime-a"), agentui.Observation{Current: claudeFrame(input)}, "claude-opus-4-8")
	if got.Applied || got.Conversation != input {
		t.Fatalf("status decoy analysis = %#v", got)
	}
}
