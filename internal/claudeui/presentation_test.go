package claudeui

import (
	"strings"
	"testing"

	"github.com/idolum-ai/engram/internal/agentui"
)

func supportedRuntime(identity string) Runtime {
	return Runtime{Detected: true, Supported: true, Version: SupportedVersion, Identity: identity}
}

func TestDeclaredNativeVersionsShareTheFixtureBackedPresentationContract(t *testing.T) {
	frame := claudeFrame(strings.Join([]string{
		"╭──────────────────────────────────╮",
		"│ Opus 4.8 · API Usage Billing     │",
		"╰──────────────────────────────────╯",
		"",
		"⏺ Visible answer.",
		"",
		"────────────────────────────────────",
		"❯ next prompt",
		"────────────────────────────────────",
		"  bypass permissions on · ● high · /effort",
	}, "\n"))
	for _, version := range []string{SupportedVersion, previousSupportedVersion, olderSupportedVersion, legacySupportedVersion, supportedFixtureVersion} {
		t.Run(version, func(t *testing.T) {
			got := Analyze(Runtime{Detected: true, Supported: supportedVersion(version), Version: version, Identity: "runtime-" + version}, agentui.Observation{Current: frame}, "")
			if !got.Applied || got.Model != "claude-opus-4-8" || got.Effort != "high" || !strings.Contains(got.Conversation, "Visible answer") || !strings.Contains(got.Conversation, "next prompt") {
				t.Fatalf("analysis = %#v", got)
			}
		})
	}
}

func TestAnalyzeClaude224SeparatesStartupConversationAndTeamFooter(t *testing.T) {
	input := strings.Join([]string{
		"example@host ~ % claude --bad-option",
		"error: unknown option '--bad-option'",
		"example@host ~ % claude --dangerously-skip-permissions",
		"",
		"────────────────────────────────────────────────────────────────────────────────",
		" Accessing workspace:",
		"",
		" /Users/example",
		"",
		" Quick safety check: Is this a project you created or one you trust? (Like your",
		"",
		"  Please review the fixture behavior.",
		"",
		"✻ Brewed for 1m 21s",
		"",
		"⏺ Agent \"Review the fixture\" finished · 13m 45s",
		"",
		"⏺ The visible result is ready.",
		"",
		"✻ Brewed for 14m 46s",
		"",
		"────────────────────────────────────────────────────────────────── front-end ──",
		"❯ ",
		"────────────────────────────────────────────────────────────────────────────────",
		"  ⏸ manual mode on · ? for shortcuts · ← for agents",
		"  ⧉  project-design · project-front-end · project-try-it",
	}, "\n")
	got := Analyze(supportedRuntime("runtime-224"), agentui.Observation{Current: claudeFrame(input)}, "claude-opus-4-8")
	if !got.Applied || got.Model != "claude-opus-4-8" || got.Mode != "manual" || got.Activity != agentui.ActivityIdle {
		t.Fatalf("analysis = %#v", got)
	}
	if got.ModelObserved || got.LastTurnSeconds != 14*60+46 || got.AgentTotal != 3 || got.AgentActive != 1 || got.ViewportBoundary != "claude_trust_prompt" || got.ViewportStart != 10 {
		t.Fatalf("structured metadata = %#v", got)
	}
	for _, wanted := range []string{"Please review the fixture behavior", "Agent \"Review the fixture\" finished", "The visible result is ready"} {
		if !strings.Contains(got.Conversation, wanted) {
			t.Fatalf("conversation omitted %q: %q", wanted, got.Conversation)
		}
	}
	for _, omitted := range []string{"--bad-option", "Accessing workspace", "Quick safety check", "Brewed for", "front-end", "manual mode on", "project-design"} {
		if strings.Contains(got.Conversation, omitted) {
			t.Fatalf("conversation retained %q: %q", omitted, got.Conversation)
		}
	}
}

func TestAnalyzeClaude224AcceptsChangingInteractionMode(t *testing.T) {
	input := "⏺ Waiting for input.\n\n────────────\n❯\n────────────\n  ⏵ accept-edits mode on · ? for shortcuts · ← for agents"
	got := Analyze(supportedRuntime("runtime-mode"), agentui.Observation{Current: claudeFrame(input)}, "")
	if !got.Applied || got.Mode != "accept-edits" || got.Activity != agentui.ActivityIdle {
		t.Fatalf("analysis = %#v", got)
	}
}

func TestAnalyzeClaude224DoesNotPublishUnknownInteractionTextAsMetadata(t *testing.T) {
	input := "⏺ Waiting for input.\n\n────────────\n❯\n────────────\n  ⏵ private-project mode on · ? for shortcuts · ← for agents"
	got := Analyze(supportedRuntime("runtime-private-mode"), agentui.Observation{Current: claudeFrame(input)}, "")
	if got.Mode != "" || !strings.Contains(got.Conversation, "private-project mode") {
		t.Fatalf("unknown interaction was hidden or published: %#v", got)
	}
}

func TestAnalyzeClaude224DoesNotEraseQuotedSafetyPrompt(t *testing.T) {
	input := strings.Join([]string{
		"⏺ The phrase below is part of the answer:",
		"Quick safety check: Is this a project you created or one you trust?",
		"",
		"────────────",
		"❯",
		"────────────",
		"  ⏸ manual mode on · ? for shortcuts · ← for agents",
	}, "\n")
	got := Analyze(supportedRuntime("runtime-quote"), agentui.Observation{Current: claudeFrame(input)}, "")
	if !got.Applied || !strings.Contains(got.Conversation, "Quick safety check") {
		t.Fatalf("quoted conversation was removed: %#v", got)
	}
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

func TestAnalyzeUsesObservedClaudeComposerWhenStatusRowIsAbsent(t *testing.T) {
	input := strings.Join([]string{
		"⏺ Every candidate was checked against the current tree.",
		"",
		"✻ Cooked for 4m 18s",
		"",
		"9 tasks (8 done, 1 open)",
		"◻ Draft the next target contract",
		"✔ Open the implementation pull request",
		"… +4 completed",
		"",
		"────────────────────────────────────",
		"❯ implement the selected candidate",
		"────────────────────────────────────",
	}, "\n")
	got := Analyze(supportedRuntime("runtime-a"), agentui.Observation{Current: claudeFrame(input)}, "")
	if !got.Applied || got.Model != "" || got.Effort != "" || got.Activity != agentui.ActivityIdle {
		t.Fatalf("analysis = %#v", got)
	}
	for _, unwanted := range []string{"Cooked for", "… +4 completed", "────"} {
		if strings.Contains(got.Conversation, unwanted) {
			t.Fatalf("conversation retained %q: %q", unwanted, got.Conversation)
		}
	}
	for _, wanted := range []string{"Every candidate", "Draft the next target contract", "implement the selected candidate"} {
		if !strings.Contains(got.Conversation, wanted) {
			t.Fatalf("conversation omitted %q: %q", wanted, got.Conversation)
		}
	}
}

func TestAnalyzePreservesComposerPromptContainingEffortCommand(t *testing.T) {
	input := strings.Join([]string{
		"⏺ I can explain the setting before changing it.",
		"",
		"────────────────────────────────────",
		"❯ explain /effort high before changing it",
		"────────────────────────────────────",
	}, "\n")
	got := Analyze(supportedRuntime("runtime-a"), agentui.Observation{Current: claudeFrame(input)}, "claude-opus-4-8")
	if !got.Applied || got.Effort != "" {
		t.Fatalf("analysis = %#v", got)
	}
	if !strings.Contains(got.Conversation, "❯ explain /effort high before changing it") {
		t.Fatalf("conversation lost composer prompt: %q", got.Conversation)
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

func TestAnalyzeDoesNotTreatConversationConfidenceAsClaudeEffort(t *testing.T) {
	input := "⏺ Confidence report\n\nresult · high confidence\n\n────────────────────\n❯\n────────────────────"
	got := Analyze(supportedRuntime("runtime-a"), agentui.Observation{Current: claudeFrame(input)}, "claude-opus-4-8")
	if !got.Applied || got.Effort != "" || !strings.Contains(got.Conversation, "result · high confidence") {
		t.Fatalf("confidence text analysis = %#v", got)
	}
}

func TestAnalyzeRejectsUnframedClaudePromptLookalike(t *testing.T) {
	input := "⏺ Report follows\n\n❯ ordinary quoted prompt\n\n────────────────────"
	got := Analyze(supportedRuntime("runtime-a"), agentui.Observation{Current: claudeFrame(input)}, "claude-opus-4-8")
	if got.Applied || got.Conversation != input {
		t.Fatalf("composer decoy analysis = %#v", got)
	}
}
