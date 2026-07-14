package app

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/idolum-ai/engram/internal/anthropic"
)

func TestConversationEvidenceDropsTrailingIdlePromptChrome(t *testing.T) {
	input := "Work completed.\n\n\u203a Write tests for\n  @filename\n\n  gpt-5.6-sol high \u00b7 ~ \u00b7 Main [default]\n"
	if got, want := conversationEvidence(input), "Work completed."; got != want {
		t.Fatalf("conversationEvidence() = %q, want %q", got, want)
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
	for _, input := range []string{
		"build passed\n$",
		"build passed\nresult \u00b7 main \u00b7 checks [done]",
	} {
		if got := conversationEvidence(input); got != input {
			t.Fatalf("conversationEvidence() = %q, want %q unchanged", got, input)
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
	app.Anthropic = anthropic.New(apiKey, model)
	text := "The work has completed and pushed to PR #11.\n\n\u203a excellent! show me the map: what we aimed to find, what rivers we crossed, and what lies ahead\n\nWorking (1s)\n\n\u203a Write tests for @filename\n\n  gpt-5.6-sol high \u00b7 ~ \u00b7 Main [default]"
	capture := testStyledCapture("codex", text)
	for attempt := 1; attempt <= 3; attempt++ {
		summary, _, err := app.conversationalSummary(context.Background(), session, capture, capture.JoinedText)
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		normalized := strings.ToLower(summary)
		for _, forbidden := range []string{"write tests", "@filename", "next prompt", "gpt-5.6-sol"} {
			if strings.Contains(normalized, forbidden) {
				t.Errorf("attempt %d included passive chrome %q:\n%s", attempt, forbidden, summary)
			}
		}
		for _, required := range []string{"pr #11", "map"} {
			if !strings.Contains(normalized, required) {
				t.Errorf("attempt %d omitted %q:\n%s", attempt, required, summary)
			}
		}
		if words := len(strings.Fields(summary)); words > 180 {
			t.Errorf("attempt %d returned %d words, want at most 180", attempt, words)
		}
		t.Logf("attempt %d:\n%s", attempt, summary)
	}
}
