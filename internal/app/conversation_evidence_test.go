package app

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/idolum-ai/engram/internal/anthropic"
)

func TestConversationEvidenceDropsTrailingIdlePromptChrome(t *testing.T) {
	for _, suggestion := range []string{
		"\u203a Write tests for\n  @filename",
		"\u203a Find and fix a bug in @filename",
		"\u203a Run /review on my current changes",
	} {
		input := "Work completed.\n\n" + suggestion + "\n\n  gpt-5.6-sol high \u00b7 ~ \u00b7 Main [default]\n"
		if got, want := conversationEvidence(input), "Work completed."; got != want {
			t.Fatalf("conversationEvidence(%q) = %q, want %q", suggestion, got, want)
		}
	}
}

func TestConversationEvidenceKeepsChromeWhenItIsTheOnlyIdleEvidence(t *testing.T) {
	input := "\u203a Write tests for @filename\n\n  gpt-5.6-sol high \u00b7 ~/engram \u00b7 Main [default]"
	if got := conversationEvidence(input); got != input {
		t.Fatalf("conversationEvidence() = %q, want complete idle evidence", got)
	}
}

func TestConversationEvidenceKeepsActiveProgress(t *testing.T) {
	input := "\u203a Fix the tests\n\n  Working (1s; esc to interrupt)\n\n  gpt-5.6-sol high \u00b7 ~ \u00b7 Main [default]"
	want := "\u203a Fix the tests\n\n  Working (1s; esc to interrupt)"
	if got := conversationEvidence(input); got != want {
		t.Fatalf("conversationEvidence() = %q, want %q", got, want)
	}
}

func TestConversationEvidenceLeavesOrdinaryTerminalTextAlone(t *testing.T) {
	for _, test := range []struct {
		input string
		want  string
	}{
		{input: "build passed\n$", want: "build passed\n$"},
		{input: "build passed\n\n\u203a Write tests for @filename", want: "build passed\n\n\u203a Write tests for @filename"},
		{input: "build passed\nresult \u00b7 main \u00b7 checks [done]", want: "build passed\nresult \u00b7 main \u00b7 checks [done]"},
		{input: "build failed\n\n> fatal finding\n\n  claude-sonnet \u00b7 ~ \u00b7 Main [default]", want: "build failed\n\n> fatal finding"},
		{input: "build failed\n\n\u203a Fix the actual blocker\n\n  gpt-5.6-sol \u00b7 ~ \u00b7 Main [default]", want: "build failed\n\n\u203a Fix the actual blocker"},
	} {
		if got := conversationEvidence(test.input); got != test.want {
			t.Fatalf("conversationEvidence() = %q, want %q", got, test.want)
		}
	}
}

func TestConversationEvidenceRecognizesSupportedModelFooters(t *testing.T) {
	for _, label := range []string{"gpt-5.6-sol", "claude-sonnet", "gemini-2", "codex", "o1", "o3", "o4"} {
		input := "done\n\n\u203a Write tests for @filename\n\n  " + label + " \u00b7 ~ \u00b7 Main [default]"
		if got := conversationEvidence(input); got != "done" {
			t.Fatalf("conversationEvidence() with %q footer = %q", label, got)
		}
	}
}

func TestLiveConversationEvidenceIgnoresPassiveChrome(t *testing.T) {
	if os.Getenv("ENGRAM_LIVE_HAIKU_CHROME_EVAL") != "1" {
		t.Skip("set ENGRAM_LIVE_HAIKU_CHROME_EVAL=1 to evaluate passive terminal chrome")
	}
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		t.Fatal("ANTHROPIC_API_KEY is required for the live evaluation")
	}
	model := os.Getenv("ANTHROPIC_MODEL")
	if model == "" {
		model = "claude-haiku-4-5-20251001"
	}

	app, session := conversationEpochTestApp(t, 31)
	app.Guide = anthropic.New(apiKey, model)
	text := "The work has completed and pushed to PR #11.\n\n\u203a excellent! show me the map: what we aimed to find, what rivers we crossed, and what lies ahead\n\nWorking (1s)\n\n\u203a Write tests for @filename\n\n  gpt-5.6-sol high \u00b7 ~ \u00b7 Main [default]"
	capture := testStyledCapture("codex", text)
	for attempt := 1; attempt <= 3; attempt++ {
		summary, _, _, err := app.conversationalSummary(context.Background(), session, capture, capture.JoinedText)
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		normalized := strings.ToLower(summary)
		for _, forbidden := range []string{"write tests", "@filename", "next prompt", "gpt-5.6-sol"} {
			if strings.Contains(normalized, forbidden) {
				t.Errorf("attempt %d included passive chrome %q:\n%s", attempt, forbidden, summary)
			}
		}
		if !strings.Contains(normalized, "pr #11") && !strings.Contains(normalized, "pull request #11") {
			t.Errorf("attempt %d omitted pull request #11:\n%s", attempt, summary)
		}
		if !strings.Contains(normalized, "map") {
			t.Errorf("attempt %d omitted map intent:\n%s", attempt, summary)
		}
		if words := len(strings.Fields(summary)); words > 180 {
			t.Errorf("attempt %d returned %d words, want at most 180", attempt, words)
		}
		t.Logf("attempt %d:\n%s", attempt, summary)
	}
}
