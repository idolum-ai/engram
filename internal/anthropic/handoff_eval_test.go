package anthropic

import (
	"context"
	"os"
	"strings"
	"testing"
)

// This opt-in evaluation uses adjacent observations, because isolated terminal
// screenshots cannot establish whether a handoff is timely, stale, or resolved.
func TestLiveHaikuSequentialHandoffEvaluation(t *testing.T) {
	if os.Getenv("ENGRAM_LIVE_HAIKU_EVAL") != "1" {
		t.Skip("set ENGRAM_LIVE_HAIKU_EVAL=1 to run non-deterministic Haiku handoff evaluations")
	}
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		t.Fatal("ANTHROPIC_API_KEY is required for live handoff evaluation")
	}
	model := os.Getenv("ANTHROPIC_MODEL")
	if model == "" {
		model = "claude-haiku-4-5-20251001"
	}
	timelines := []struct {
		name  string
		steps []handoffEvalStep
	}{
		{name: "approval settles then resumes", steps: []handoffEvalStep{
			{capture: "Building release... 61%", needed: false},
			{capture: "Deploy release to production?\nConfirm [y/N]:", needed: true},
			{capture: "Deploy release to production?\nConfirm [y/N]:", needed: true},
			{capture: "Deploying release... 12%", needed: false, intervention: true},
		}},
		{name: "failure waits for correction", steps: []handoffEvalStep{
			{capture: "go test ./...\n# example/internal/app\napp.go:42:7: undefined: result\nFAIL\nuser@host:~/code$", needed: true},
			{capture: "go test ./...\n# example/internal/app\napp.go:42:7: undefined: result\nFAIL\nuser@host:~/code$", needed: true},
		}},
		{name: "progress never pages", steps: []handoffEvalStep{
			{capture: "=== RUN TestStore\ncompiling package 14 of 31...", needed: false},
			{capture: "=== RUN TestStore\ncompiling package 27 of 31...", needed: false},
		}},
		{name: "bare shell is not a task", steps: []handoffEvalStep{
			{capture: "user@host:~/code$", needed: false},
			{capture: "user@host:~/code$", needed: false},
		}},
		{name: "completed request hands back", steps: []handoffEvalStep{
			{capture: "go test ./...\nok example/internal/app  4.2s\nuser@host:~/engram$", needed: true, lastInput: "go test ./..."},
			{capture: "go test ./...\nok example/internal/app  4.2s\nuser@host:~/engram$", needed: true, lastInput: "go test ./..."},
		}},
	}

	client := New(apiKey, model)
	truePositive, falsePositive, trueNegative, falseNegative := 0, 0, 0, 0
	for _, timeline := range timelines {
		var previous string
		var open GuideReport
		for index, step := range timeline.steps {
			input := SummaryInput{
				SessionID:          1,
				State:              "running",
				LastInput:          step.lastInput,
				LastInputMode:      "command",
				HasPreviousCapture: index > 0,
				CaptureChanged:     index == 0 || step.capture != previous,
				VisibleCapture:     step.capture,
			}
			if open.HumanNeeded {
				input.OpenHandoff = true
				input.HandoffKey = open.HandoffKey
				input.HandoffStatus = open.StatusReport
				input.HandoffAction = open.RecommendedAction
				input.HandoffEvidence = open.Citations
				input.HandoffAcknowledged = step.intervention
			}
			report, err := client.Guide(context.Background(), input)
			if err != nil {
				t.Fatalf("%s step %d: %v", timeline.name, index+1, err)
			}
			switch {
			case report.HumanNeeded && step.needed:
				truePositive++
			case report.HumanNeeded:
				falsePositive++
				t.Logf("false positive: %s step %d key=%q reason=%q", timeline.name, index+1, report.HandoffKey, report.Reason)
			case step.needed:
				falseNegative++
				t.Logf("false negative: %s step %d confidence=%q full=%t reason=%q status=%q", timeline.name, index+1, report.Confidence, report.NeedsFullBuffer, report.Reason, report.StatusReport)
			default:
				trueNegative++
			}
			if report.HumanNeeded && !citationsGrounded(report.Citations, step.capture) {
				t.Errorf("%s step %d returned ungrounded handoff citations: %#v", timeline.name, index+1, report.Citations)
			}
			if report.HumanNeeded {
				open = report
			} else if step.intervention {
				open = GuideReport{}
			}
			previous = step.capture
		}
	}
	precision := ratio(truePositive, truePositive+falsePositive)
	recall := ratio(truePositive, truePositive+falseNegative)
	t.Logf("handoff precision %.2f recall %.2f (tp=%d fp=%d tn=%d fn=%d)", precision, recall, truePositive, falsePositive, trueNegative, falseNegative)
	if precision < 0.90 || recall < 0.80 {
		t.Fatalf("handoff quality below threshold: precision %.2f recall %.2f", precision, recall)
	}
}

type handoffEvalStep struct {
	capture      string
	lastInput    string
	needed       bool
	intervention bool
}

func citationsGrounded(citations []string, capture string) bool {
	if len(citations) == 0 {
		return false
	}
	normalizedCapture := strings.ToLower(strings.Join(strings.Fields(capture), " "))
	for _, citation := range citations {
		words := strings.Fields(strings.ToLower(citation))
		matched := 0
		for _, word := range words {
			word = strings.Trim(word, "[]():;,.!?`'\"")
			if len(word) >= 3 && strings.Contains(normalizedCapture, word) {
				matched++
			}
		}
		if matched >= 2 || matched == len(words) && matched > 0 {
			return true
		}
	}
	return false
}

func ratio(numerator, denominator int) float64 {
	if denominator == 0 {
		return 1
	}
	return float64(numerator) / float64(denominator)
}
