package anthropic

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
	"unicode"
)

type conversationCase struct {
	Name         string     `json:"name"`
	TerminalText string     `json:"terminal_text"`
	Reference    string     `json:"reference"`
	Concepts     [][]string `json:"concepts"`
	Forbidden    []string   `json:"forbidden"`
	Contradicts  []string   `json:"contradicts,omitempty"`
}

func TestConversationFixturesAndScoring(t *testing.T) {
	t.Parallel()
	cases := loadConversationCases(t)
	if len(cases) < 5 {
		t.Fatalf("fixture count = %d, want at least 5", len(cases))
	}
	for _, evalCase := range cases {
		evalCase := evalCase
		t.Run(evalCase.Name, func(t *testing.T) {
			t.Parallel()
			if evalCase.TerminalText == "" || evalCase.Reference == "" || len(evalCase.Concepts) == 0 {
				t.Fatalf("incomplete fixture: %#v", evalCase)
			}
			if distance := semanticDistance(evalCase, evalCase.Reference); distance > 0.001 {
				t.Fatalf("reference distance = %.3f, want 0", distance)
			}
			if failures := hardOutputRegressions(evalCase, evalCase.Reference); len(failures) != 0 {
				t.Fatalf("reference triggered hard regressions: %v", failures)
			}
			irrelevant := "Status: Engram captured a terminal buffer. Recommendation: tap refresh to continue the previous task."
			if got, want := semanticDistance(evalCase, irrelevant), semanticDistance(evalCase, evalCase.Reference); got <= want {
				t.Fatalf("irrelevant distance %.3f <= reference distance %.3f", got, want)
			}
		})
	}
}

func TestHardOutputRegressionsRejectContradiction(t *testing.T) {
	evalCase := loadConversationCases(t)[0]
	failures := hardOutputRegressions(evalCase, "The formatting is clean, but the tests did not pass.")
	if !containsFailure(failures, "contradiction") {
		t.Fatalf("failures = %v, want contradiction", failures)
	}
}

func TestHardOutputRegressionsRejectWrongNumber(t *testing.T) {
	evalCase := loadConversationCases(t)[1]
	failures := hardOutputRegressions(evalCase, "Six concrete issues remain, including callback authorization and the JSONL record.")
	if !containsFailure(failures, "unsupported number claim") {
		t.Fatalf("failures = %v, want unsupported number claim", failures)
	}
}

func containsFailure(failures []string, fragment string) bool {
	for _, failure := range failures {
		if strings.Contains(failure, fragment) {
			return true
		}
	}
	return false
}

func TestConversationShapePrefersCollaborativeReadableProse(t *testing.T) {
	t.Parallel()
	distanced := "You're partway through the change and the tests are green."
	collaborative := "We are partway through the change.\n\nThe tests are green.\n\nWe can move on to review."
	if got := conversationShapePenalty(distanced); got == 0 {
		t.Fatal("distancing opening received no style penalty")
	}
	if got := conversationShapePenalty(collaborative); got != 0 {
		t.Fatalf("collaborative paragraph shape penalty = %.2f", got)
	}
	if got := conversationShapePenalty("The prompt is idle.\n\nWhat should we do next?"); got == 0 {
		t.Fatal("conversational question ending received no style penalty")
	}
}

func TestLiveHaikuConversationEvaluation(t *testing.T) {
	if os.Getenv("ENGRAM_LIVE_HAIKU_EVAL") != "1" {
		t.Skip("set ENGRAM_LIVE_HAIKU_EVAL=1 to evaluate the production prompt")
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
	cases := loadConversationCases(t)
	if name := strings.TrimSpace(os.Getenv("ENGRAM_LIVE_HAIKU_EVAL_CASE")); name != "" {
		filtered := cases[:0]
		for _, evalCase := range cases {
			if evalCase.Name == name {
				filtered = append(filtered, evalCase)
			}
		}
		cases = filtered
		if len(cases) == 0 {
			t.Fatalf("ENGRAM_LIVE_HAIKU_EVAL_CASE did not match a fixture: %s", name)
		}
	}
	for index, evalCase := range cases {
		input := ConversationInput{SessionID: index + 1, VisibleText: evalCase.TerminalText}
		output, err := client.Converse(context.Background(), input)
		if err != nil {
			t.Fatalf("%s production request: %v", evalCase.Name, err)
		}
		if failures := hardOutputRegressions(evalCase, output); len(failures) != 0 {
			t.Errorf("%s production hard regressions: %s\n  output: %s", evalCase.Name, strings.Join(failures, "; "), output)
		}
		distance := semanticDistance(evalCase, output)
		t.Logf("%s: production distance=%.3f\n  output: %s", evalCase.Name, distance, output)
	}
}

func loadConversationCases(t *testing.T) []conversationCase {
	t.Helper()
	data, err := os.ReadFile("testdata/conversation_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []conversationCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	return cases
}

// semanticDistance combines required-idea coverage with lexical similarity and
// explicit style violations. It is deterministic, transparent, and tolerant of
// natural paraphrase; lower scores are better.
func semanticDistance(evalCase conversationCase, output string) float64 {
	normalized := normalizeEvalText(output)
	covered := 0
	for _, aliases := range evalCase.Concepts {
		for _, alias := range aliases {
			if strings.Contains(normalized, normalizeEvalText(alias)) {
				covered++
				break
			}
		}
	}
	conceptScore := float64(covered) / float64(len(evalCase.Concepts))
	lexicalScore := tokenF1(evalCase.Reference, output)
	distance := 1 - (0.75*conceptScore + 0.25*lexicalScore)
	for _, phrase := range evalCase.Forbidden {
		if strings.Contains(normalized, normalizeEvalText(phrase)) {
			distance += 0.12
		}
	}
	if structuredLabel.MatchString(strings.ToLower(output)) {
		distance += 0.25
	}
	distance += conversationShapePenalty(output)
	if distance < 0 {
		return 0
	}
	if distance > 1 {
		return 1
	}
	return distance
}

func hardOutputRegressions(evalCase conversationCase, output string) []string {
	normalized := normalizeEvalText(output)
	failures := make([]string, 0)
	for _, phrase := range evalCase.Forbidden {
		if strings.Contains(normalized, normalizeEvalText(phrase)) {
			failures = append(failures, "forbidden phrase "+strconvQuote(phrase))
		}
	}
	for _, phrase := range evalCase.Contradicts {
		if strings.Contains(normalized, normalizeEvalText(phrase)) {
			failures = append(failures, "contradiction "+strconvQuote(phrase))
		}
	}
	failures = append(failures, unsupportedNumberClaims(evalCase.TerminalText, output)...)
	return failures
}

var numberWords = map[string]string{
	"zero": "0", "one": "1", "two": "2", "three": "3", "four": "4",
	"five": "5", "six": "6", "seven": "7", "eight": "8", "nine": "9",
	"ten": "10", "eleven": "11", "twelve": "12",
}

func unsupportedNumberClaims(source, output string) []string {
	sourceNumbers := make(map[string]bool)
	for _, number := range numericClaimPattern.FindAllString(source, -1) {
		sourceNumbers[number] = true
	}
	checkNumberWords := false
	for _, word := range evalWords(source) {
		if number, ok := numberWords[word]; ok {
			sourceNumbers[number] = true
			checkNumberWords = true
		}
	}
	seen := make(map[string]bool)
	var failures []string
	for _, number := range numericClaimPattern.FindAllString(output, -1) {
		if !sourceNumbers[number] && !seen[number] {
			failures = append(failures, "unsupported number claim "+number)
			seen[number] = true
		}
	}
	if checkNumberWords {
		for _, word := range evalWords(output) {
			number, ok := numberWords[word]
			if ok && !sourceNumbers[number] && !seen[number] {
				failures = append(failures, "unsupported number claim "+word)
				seen[number] = true
			}
		}
	}
	return failures
}

var numericClaimPattern = regexp.MustCompile(`\d+(?:\.\d+)?`)

func evalWords(value string) []string {
	var words []string
	var word strings.Builder
	flush := func() {
		if word.Len() != 0 {
			words = append(words, word.String())
			word.Reset()
		}
	}
	for _, r := range strings.ToLower(value) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			word.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return words
}

func strconvQuote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

var structuredLabel = regexp.MustCompile(`(?m)(^|[,{\n])\s*["']?(status_report|status|recommended_action|recommendation|evidence|citations|confidence)["']?\s*[:=]`)
var distancingOpening = regexp.MustCompile(`(?i)^\s*(you are|you're|you've)\b`)
var paragraphBreak = regexp.MustCompile(`\n\s*\n`)
var conversationalQuestionEnding = regexp.MustCompile(`\?\s*$`)

func conversationShapePenalty(output string) float64 {
	trimmed := strings.TrimSpace(strings.ReplaceAll(output, "\r\n", "\n"))
	if trimmed == "" {
		return 0
	}
	penalty := 0.0
	if distancingOpening.MatchString(trimmed) {
		penalty += 0.10
	}
	if conversationalQuestionEnding.MatchString(trimmed) {
		penalty += 0.08
	}
	paragraphs := paragraphBreak.Split(trimmed, -1)
	if len(paragraphs) > 5 {
		penalty += 0.08
	}
	for _, paragraph := range paragraphs {
		if len([]rune(strings.TrimSpace(paragraph))) > 240 {
			penalty += 0.08
		}
	}
	return penalty
}

func normalizeEvalText(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(value)), " ")
}

func tokenF1(reference, output string) float64 {
	referenceTokens := evalTokens(reference)
	outputTokens := evalTokens(output)
	if len(referenceTokens) == 0 || len(outputTokens) == 0 {
		return 0
	}
	common := 0
	for token := range referenceTokens {
		if _, ok := outputTokens[token]; ok {
			common++
		}
	}
	precision := float64(common) / float64(len(outputTokens))
	recall := float64(common) / float64(len(referenceTokens))
	if precision+recall == 0 {
		return 0
	}
	return 2 * precision * recall / (precision + recall)
}

func evalTokens(value string) map[string]struct{} {
	tokens := make(map[string]struct{})
	var word strings.Builder
	flush := func() {
		if word.Len() >= 3 {
			tokens[word.String()] = struct{}{}
		}
		word.Reset()
	}
	for _, r := range strings.ToLower(value) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			word.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return tokens
}
