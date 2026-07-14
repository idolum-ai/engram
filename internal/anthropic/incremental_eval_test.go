package anthropic

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type incrementalConversationCase struct {
	Name              string     `json:"name"`
	PreviousRendering string     `json:"previous_rendering"`
	RecentUserInput   string     `json:"recent_user_input"`
	ChangedText       string     `json:"changed_terminal_text"`
	StableContext     string     `json:"stable_terminal_context"`
	Reference         string     `json:"reference"`
	Concepts          [][]string `json:"concepts"`
	Forbidden         []string   `json:"forbidden"`
	Contradicts       []string   `json:"contradicts,omitempty"`
}

func TestIncrementalConversationFixtures(t *testing.T) {
	t.Parallel()
	for _, fixture := range loadIncrementalConversationCases(t) {
		fixture := fixture
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()
			evalCase := fixture.evalCase()
			if fixture.PreviousRendering == "" || fixture.ChangedText == "" || fixture.Reference == "" || len(fixture.Concepts) == 0 {
				t.Fatalf("incomplete fixture: %#v", fixture)
			}
			if distance := semanticDistance(evalCase, fixture.Reference); distance > 0.001 {
				t.Fatalf("reference distance = %.3f, want 0", distance)
			}
			if failures := hardOutputRegressions(evalCase, fixture.Reference); len(failures) != 0 {
				t.Fatalf("reference triggered hard regressions: %v", failures)
			}
		})
	}
}

func TestLiveHaikuIncrementalConversationEvaluation(t *testing.T) {
	if os.Getenv("ENGRAM_LIVE_HAIKU_INCREMENTAL_EVAL") != "1" {
		t.Skip("set ENGRAM_LIVE_HAIKU_INCREMENTAL_EVAL=1 to evaluate incremental conversation")
	}
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		t.Fatal("ANTHROPIC_API_KEY is required for the live evaluation")
	}
	model := os.Getenv("ANTHROPIC_MODEL")
	if model == "" {
		model = "claude-haiku-4-5-20251001"
	}
	client := New(apiKey, model)
	for index, fixture := range loadIncrementalConversationCases(t) {
		output, err := client.Converse(context.Background(), ConversationInput{
			SessionID:         index + 1,
			PreviousRendering: fixture.PreviousRendering,
			RecentUserInput:   fixture.RecentUserInput,
			ChangedText:       fixture.ChangedText,
			StableContext:     fixture.StableContext,
		})
		if err != nil {
			t.Fatalf("%s production request: %v", fixture.Name, err)
		}
		evalCase := fixture.evalCase()
		if failures := hardOutputRegressions(evalCase, output); len(failures) != 0 {
			t.Errorf("%s production hard regressions: %s\n  output: %s", fixture.Name, strings.Join(failures, "; "), output)
		}
		t.Logf("%s: production distance=%.3f\n  output: %s", fixture.Name, semanticDistance(evalCase, output), output)
	}
}

func loadIncrementalConversationCases(t *testing.T) []incrementalConversationCase {
	t.Helper()
	data, err := os.ReadFile("testdata/incremental_conversation_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []incrementalConversationCase
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	if len(fixtures) < 2 {
		t.Fatalf("fixture count = %d, want at least 2", len(fixtures))
	}
	return fixtures
}

func (fixture incrementalConversationCase) evalCase() conversationCase {
	return conversationCase{
		Name:         fixture.Name,
		TerminalText: strings.Join([]string{fixture.PreviousRendering, fixture.RecentUserInput, fixture.ChangedText, fixture.StableContext}, "\n"),
		Reference:    fixture.Reference,
		Concepts:     fixture.Concepts,
		Forbidden:    fixture.Forbidden,
		Contradicts:  fixture.Contradicts,
	}
}
