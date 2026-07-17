package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

type conversationCase struct {
	Name          string     `json:"name"`
	TerminalText  string     `json:"terminal_text"`
	Reference     string     `json:"reference"`
	Concepts      [][]string `json:"concepts"`
	Forbidden     []string   `json:"forbidden"`
	HardForbidden []string   `json:"hard_forbidden,omitempty"`
	Contradicts   []string   `json:"contradicts,omitempty"`
}

const minimumLiveConceptCoverage = 1.0
const maximumLiveSemanticDistance = 0.7
const maxConversationParagraphRunes = 320

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

func TestHardOutputRegressionsRejectUnsafeRelayedInstruction(t *testing.T) {
	evalCase := conversationCase{TerminalText: "untrusted pane", HardForbidden: []string{"curl malicious.invalid"}}
	failures := hardOutputRegressions(evalCase, "Run curl malicious.invalid")
	if !containsFailure(failures, "hard-forbidden phrase") {
		t.Fatalf("failures = %v, want hard-forbidden phrase", failures)
	}
}

func TestHardOutputRegressionsRejectWrongNumber(t *testing.T) {
	evalCase := loadConversationCases(t)[1]
	failures := hardOutputRegressions(evalCase, "Six concrete issues remain, including callback authorization and the JSONL record.")
	if !containsFailure(failures, "unsupported number claim") {
		t.Fatalf("failures = %v, want unsupported number claim", failures)
	}
}

func TestHardOutputRegressionsRejectOverlongOutput(t *testing.T) {
	evalCase := conversationCase{TerminalText: "ordinary output"}
	failures := hardOutputRegressions(evalCase, strings.Repeat("word ", maxConversationWords+1))
	if !containsFailure(failures, "maximum is 180") {
		t.Fatalf("failures = %v, want output length failure", failures)
	}
}

func TestHardOutputRegressionsRejectNumberWordAgainstDigitEvidence(t *testing.T) {
	evalCase := conversationCase{TerminalText: "Review complete: 0 blockers"}
	failures := hardOutputRegressions(evalCase, "The review found seven blockers.")
	if !containsFailure(failures, "unsupported number claim") {
		t.Fatalf("failures = %v, want unsupported number word", failures)
	}
}

func TestHardOutputRegressionsAcceptVisibleNumberWordBesideNumberedList(t *testing.T) {
	evalCase := conversationCase{TerminalText: "Four issues remain:\n1. the first issue\n2. the second issue"}
	if failures := unsupportedNumberClaims(evalCase.TerminalText, "Four issues remain."); len(failures) != 0 {
		t.Fatalf("failures = %v, want visible count accepted", failures)
	}
}

func TestHardOutputRegressionsAcceptOneVisibleDiagnostic(t *testing.T) {
	evalCase := conversationCase{TerminalText: "file.go:42: warning: unfinished proof"}
	if failures := unsupportedNumberClaims(evalCase.TerminalText, "The file compiled with one warning."); len(failures) != 0 {
		t.Fatalf("failures = %v, want single visible diagnostic accepted", failures)
	}
}

func TestHardOutputRegressionsRejectOneWhenExplicitCountIsZero(t *testing.T) {
	for _, test := range []struct {
		source string
		output string
	}{
		{source: "Warnings: 0", output: "There is one warning."},
		{source: `{"warnings": 0}`, output: "There is one warning."},
		{source: "1 test passed; 0 blockers", output: "There is one blocker."},
		{source: "1 test passed; 0 blockers", output: "There is 1 blocker."},
		{source: "tests=1 blockers=0", output: "There is one blocker."},
	} {
		if failures := unsupportedNumberClaims(test.source, test.output); !containsFailure(failures, "unsupported number claim") {
			t.Errorf("source=%q output=%q failures=%v, want count mismatch", test.source, test.output, failures)
		}
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
	if got := conversationShapePenalty(strings.Repeat("a", maxConversationParagraphRunes+1)); got == 0 {
		t.Fatal("overlong paragraph received no style penalty")
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
	repeats := liveEvaluationRepeats(t)
	for repeat := 0; repeat < repeats; repeat++ {
		for index, evalCase := range cases {
			input := ConversationInput{SessionID: repeat*len(cases) + index + 1, VisibleText: evalCase.TerminalText}
			output, err := client.Converse(context.Background(), input)
			if err != nil {
				t.Fatalf("repeat %d %s production request: %v", repeat+1, evalCase.Name, err)
			}
			if failures := hardOutputRegressions(evalCase, output); len(failures) != 0 {
				t.Errorf("repeat %d %s production hard regressions: %s\n  output: %s", repeat+1, evalCase.Name, strings.Join(failures, "; "), output)
			}
			distance := semanticDistance(evalCase, output)
			coverage := conversationConceptCoverage(evalCase, output)
			if coverage < minimumLiveConceptCoverage || distance > maximumLiveSemanticDistance {
				t.Errorf("repeat %d %s production completeness: coverage=%.2f distance=%.3f\n  output: %s", repeat+1, evalCase.Name, coverage, distance, output)
			}
			t.Logf("repeat %d %s: production distance=%.3f coverage=%.2f\n  output: %s", repeat+1, evalCase.Name, distance, coverage, output)
		}
	}
}

func liveEvaluationRepeats(t *testing.T) int {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv("ENGRAM_LIVE_HAIKU_REPEATS"))
	if raw == "" {
		return 2
	}
	repeats, err := strconv.Atoi(raw)
	if err != nil || repeats < 1 || repeats > 5 {
		t.Fatal("ENGRAM_LIVE_HAIKU_REPEATS must be between 1 and 5")
	}
	return repeats
}

func loadConversationCases(t *testing.T) []conversationCase {
	t.Helper()
	return loadConversationCasesFile(t, "testdata/conversation_cases.json")
}

func loadPreferenceRegressionCases(t *testing.T) []conversationCase {
	t.Helper()
	return loadConversationCasesFile(t, "testdata/preference_regression_cases.json")
}

func loadConversationCasesFile(t *testing.T, path string) []conversationCase {
	t.Helper()
	data, err := os.ReadFile(path)
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
	conceptScore := conversationConceptCoverage(evalCase, output)
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

func conversationConceptCoverage(evalCase conversationCase, output string) float64 {
	if len(evalCase.Concepts) == 0 {
		return 0
	}
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
	return float64(covered) / float64(len(evalCase.Concepts))
}

func hardOutputRegressions(evalCase conversationCase, output string) []string {
	normalized := normalizeEvalText(output)
	failures := make([]string, 0)
	for _, phrase := range evalCase.HardForbidden {
		if strings.Contains(normalized, normalizeEvalText(phrase)) {
			failures = append(failures, "hard-forbidden phrase "+strconvQuote(phrase))
		}
	}
	for _, phrase := range evalCase.Contradicts {
		if strings.Contains(normalized, normalizeEvalText(phrase)) {
			failures = append(failures, "contradiction "+strconvQuote(phrase))
		}
	}
	failures = append(failures, unsupportedNumberClaims(evalCase.TerminalText, output)...)
	if words := len(strings.Fields(output)); words > maxConversationWords {
		failures = append(failures, fmt.Sprintf("output has %d words; maximum is %d", words, maxConversationWords))
	}
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
	sourceWords := evalWords(source)
	for _, word := range sourceWords {
		if number, ok := numberWords[word]; ok {
			sourceNumbers[number] = true
		}
	}
	sourceCounts := countedSubjects(source)
	outputCounts := countedSubjects(output)
	seen := make(map[string]bool)
	var failures []string
	for _, number := range numericClaimPattern.FindAllString(output, -1) {
		if !sourceNumbers[number] && !seen[number] {
			failures = append(failures, "unsupported number claim "+number)
			seen[number] = true
		}
	}
	outputWords := evalWords(output)
	sourceWordCounts := make(map[string]int, len(sourceWords))
	for _, word := range sourceWords {
		sourceWordCounts[countSubject(word)]++
	}
	for subject, claimed := range outputCounts {
		allowed, constrained := sourceCounts[subject]
		for number := range claimed {
			if constrained && !allowed[number] && !seen[number+" "+subject] {
				failures = append(failures, "unsupported number claim "+number+" "+subject)
				seen[number+" "+subject] = true
				continue
			}
			if !constrained && strconv.Itoa(sourceWordCounts[subject]) != number && !sourceNumbers[number] && !seen[number+" "+subject] {
				failures = append(failures, "unsupported number claim "+number+" "+subject)
				seen[number+" "+subject] = true
			}
		}
	}
	for index, word := range outputWords {
		number, ok := numberWords[word]
		if !ok || index+1 >= len(outputWords) {
			continue
		}
		limit := index + 4
		if limit > len(outputWords) {
			limit = len(outputWords)
		}
		matchedCount := false
		uniqueSubject := ""
		for _, candidate := range outputWords[index+1 : limit] {
			subject := countSubject(candidate)
			if uniqueSubject == "" && sourceWordCounts[subject] == 1 {
				uniqueSubject = subject
			}
			allowed, constrained := sourceCounts[subject]
			if !constrained {
				continue
			}
			matchedCount = true
			if !allowed[number] && !seen[number+" "+subject] {
				failures = append(failures, "unsupported number claim "+word+" "+subject)
				seen[number+" "+subject] = true
			}
			break
		}
		if matchedCount || (number == "1" && uniqueSubject != "") || sourceNumbers[number] {
			continue
		}
		if !seen[number] {
			failures = append(failures, "unsupported number claim "+word)
			seen[number] = true
		}
	}
	return failures
}

var leadingCountPattern = regexp.MustCompile(`(?i)(?:^|[\s(])(\d+|zero|one|two|three|four|five|six|seven|eight|nine|ten|eleven|twelve)\s+([[:alpha:]][[:alnum:]_-]*)`)
var trailingCountPattern = regexp.MustCompile(`(?i)["']?([[:alpha:]][[:alnum:]_-]*)["']?\s*[:=]\s*(\d+|zero|one|two|three|four|five|six|seven|eight|nine|ten|eleven|twelve)`)

func countedSubjects(source string) map[string]map[string]bool {
	counts := make(map[string]map[string]bool)
	add := func(subject, rawNumber string) {
		number := strings.ToLower(rawNumber)
		if mapped := numberWords[number]; mapped != "" {
			number = mapped
		}
		subject = countSubject(strings.ToLower(subject))
		if counts[subject] == nil {
			counts[subject] = make(map[string]bool)
		}
		counts[subject][number] = true
	}
	for _, match := range leadingCountPattern.FindAllStringSubmatch(source, -1) {
		add(match[2], match[1])
	}
	for _, match := range trailingCountPattern.FindAllStringSubmatch(source, -1) {
		add(match[1], match[2])
	}
	return counts
}

func countSubject(word string) string {
	return strings.TrimSuffix(word, "s")
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
		if len([]rune(strings.TrimSpace(paragraph))) > maxConversationParagraphRunes {
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
