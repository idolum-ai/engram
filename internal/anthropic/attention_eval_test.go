package anthropic

import (
	"context"
	"os"
	"slices"
	"testing"
)

func TestLiveHaikuAttentionEvaluation(t *testing.T) {
	if os.Getenv("ENGRAM_LIVE_HAIKU_EVAL") != "1" {
		t.Skip("set ENGRAM_LIVE_HAIKU_EVAL=1 to run non-deterministic Haiku attention evaluations")
	}
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		t.Fatal("ANTHROPIC_API_KEY is required for live attention evaluation")
	}
	model := os.Getenv("ANTHROPIC_MODEL")
	if model == "" {
		model = "claude-haiku-4-5-20251001"
	}
	fixtures := []struct {
		name    string
		capture string
		want    []string
	}{
		{name: "ordinary progress", capture: "=== RUN TestStore\ncompiling package 14 of 31...", want: []string{"none"}},
		{name: "explicit approval", capture: "Apply these changes?\n  1. Yes\n  2. No\nSelect an option:", want: []string{"act"}},
		{name: "compiler correction", capture: "main.go:42:7: undefined: result\nFAIL\n$", want: []string{"act", "review"}},
		{name: "completed work", capture: "ok ./internal/app\n$", want: []string{"review", "none"}},
		{name: "idle shell", capture: "user@host:~/code$", want: []string{"none"}},
		{name: "ambiguous partial screen", capture: "... 47%\noutput omitted\n", want: []string{"review", "none"}},
	}
	client := New(apiKey, model)
	const trials = 3
	correct := 0
	total := len(fixtures) * trials
	for _, fixture := range fixtures {
		for trial := 0; trial < trials; trial++ {
			report, err := client.Guide(context.Background(), SummaryInput{SessionID: trial + 1, State: "running", VisibleCapture: fixture.capture})
			if err != nil {
				t.Fatalf("%s trial %d: %v", fixture.name, trial+1, err)
			}
			if slices.Contains(fixture.want, report.Attention) {
				correct++
			} else {
				t.Logf("%s trial %d: attention=%s want=%v reason=%s", fixture.name, trial+1, report.Attention, fixture.want, report.Reason)
			}
		}
	}
	accuracy := float64(correct) / float64(total)
	t.Logf("attention accuracy: %d/%d (%.1f%%)", correct, total, accuracy*100)
	if accuracy < 0.80 {
		t.Fatalf("attention accuracy %.1f%% is below 80%% dark-launch threshold", accuracy*100)
	}
}
