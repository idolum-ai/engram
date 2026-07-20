package cue

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
)

const IntentSimilarityThreshold = 72

const minLearnablePromptBytes = 16

var absolutePathToken = regexp.MustCompile(`(?:^|\s)(?:~?/|[A-Za-z]:[\\/])\S+`)
var concreteNumberReference = regexp.MustCompile(`(?:^|\s)#[0-9]+\b`)
var externalStateAssertion = regexp.MustCompile(`(?i)\b(?:approved|deployed|merged|published|released)\b`)

var intentStopWords = map[string]bool{
	"a": true, "about": true, "again": true, "all": true, "an": true,
	"and": true, "as": true, "at": true, "be": true, "can": true,
	"could": true, "do": true, "for": true, "from": true, "go": true,
	"how": true, "i": true, "in": true, "is": true, "it": true,
	"me": true, "my": true, "now": true, "of": true, "on": true,
	"please": true, "send": true, "so": true, "that": true, "the": true,
	"then": true, "this": true, "to": true, "we": true, "what": true,
	"with": true, "would": true, "you": true,
}

var intentAliases = map[string]string{
	"checks": "check", "checked": "check", "checking": "check",
	"findings": "finding",
	"prs":      "pullrequest", "pr": "pullrequest",
	"reviewed": "review", "reviewer": "review", "reviewers": "review", "reviewing": "review", "reviews": "review",
	"tests": "test", "tested": "test", "testing": "test",
}

var intentActionWords = map[string]bool{
	"analyze": true, "build": true, "check": true, "comment": true,
	"commit": true, "continue": true, "download": true, "fix": true,
	"inspect": true, "install": true, "merge": true, "push": true,
	"release": true, "report": true, "restart": true, "review": true,
	"run": true, "test": true, "update": true,
}

var intentContextWords = map[string]bool{
	"change": true, "code": true, "current": true, "pullrequest": true,
	"result": true, "thing": true, "work": true,
}

type intentSignature struct {
	normalized string
	tokens     map[string]struct{}
	trigrams   map[string]struct{}
}

func promptLearnable(prompt string) bool {
	if !promptEligible(prompt) || len(strings.TrimSpace(prompt)) < minLearnablePromptBytes {
		return false
	}
	return len(makeIntentSignature(prompt).tokens) >= 2
}

func promptReplaySafe(prompt string) bool {
	return len(visibleURL.FindAllString(prompt, 1)) == 0 &&
		!absolutePathToken.MatchString(prompt) &&
		!concreteNumberReference.MatchString(prompt) &&
		!externalStateAssertion.MatchString(prompt)
}

func makeIntentSignature(prompt string) intentSignature {
	words := intentWords(prompt)
	tokens := make(map[string]struct{}, len(words))
	for _, value := range words {
		tokens[value] = struct{}{}
	}
	ordered := make([]string, 0, len(tokens))
	for value := range tokens {
		ordered = append(ordered, value)
	}
	sort.Strings(ordered)
	normalized := strings.Join(ordered, " ")
	return intentSignature{normalized: normalized, tokens: tokens, trigrams: intentTrigrams(normalized)}
}

func intentWords(prompt string) []string {
	prompt = strings.ToLower(prompt)
	prompt = strings.NewReplacer("pull request", "pullrequest", "pull-request", "pullrequest").Replace(prompt)
	var words []string
	var word strings.Builder
	flush := func() {
		if word.Len() == 0 {
			return
		}
		value := normalizeIntentWord(word.String())
		word.Reset()
		if value == "" || intentStopWords[value] {
			return
		}
		words = append(words, value)
	}
	inNumber := false
	for _, r := range prompt {
		if unicode.IsDigit(r) {
			flush()
			if !inNumber {
				words = append(words, "number")
			}
			inNumber = true
			continue
		}
		inNumber = false
		if unicode.IsLetter(r) {
			word.WriteRune(r)
			continue
		}
		flush()
	}
	flush()

	return words
}

func suggestCueName(prompt string) string {
	words := intentWords(prompt)
	if len(words) == 0 {
		return "cue"
	}
	for index, word := range words {
		if !intentActionWords[word] {
			continue
		}
		partner := ""
		for previous := index - 1; previous >= 0; previous-- {
			if !intentActionWords[words[previous]] && !intentContextWords[words[previous]] {
				partner = words[previous]
				break
			}
		}
		if partner == "" {
			for following := index + 1; following < len(words); following++ {
				if !intentActionWords[words[following]] && !intentContextWords[words[following]] {
					partner = words[following]
					break
				}
			}
		}
		if partner != "" {
			return boundedCueName(word + "-" + partner)
		}
		return boundedCueName(word)
	}
	if len(words) > 1 {
		return boundedCueName(words[0] + "-" + words[1])
	}
	return boundedCueName(words[0])
}

func boundedCueName(value string) string {
	var cleaned strings.Builder
	dash := false
	for _, r := range strings.ToLower(value) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' {
			cleaned.WriteRune(r)
			dash = false
			continue
		}
		if cleaned.Len() > 0 && !dash {
			cleaned.WriteByte('-')
			dash = true
		}
	}
	value = strings.Trim(cleaned.String(), "-")
	if len(value) > 32 {
		value = strings.TrimRight(value[:32], "-")
	}
	if value == "" {
		return "cue"
	}
	return value
}

func normalizeIntentWord(value string) string {
	if alias := intentAliases[value]; alias != "" {
		return alias
	}
	if len(value) > 5 && strings.HasSuffix(value, "s") && !strings.HasSuffix(value, "ss") {
		value = strings.TrimSuffix(value, "s")
	}
	return value
}

func intentTrigrams(value string) map[string]struct{} {
	runes := []rune(value)
	out := make(map[string]struct{})
	if len(runes) < 3 {
		if value != "" {
			out[value] = struct{}{}
		}
		return out
	}
	for index := 0; index+3 <= len(runes); index++ {
		out[string(runes[index:index+3])] = struct{}{}
	}
	return out
}

func intentSimilarity(left, right intentSignature) int {
	if left.normalized == "" || right.normalized == "" {
		return 0
	}
	if left.normalized == right.normalized {
		return 100
	}
	intersection := setIntersection(left.tokens, right.tokens)
	if intersection == 0 {
		return 0
	}
	minimum := min(len(left.tokens), len(right.tokens))
	union := len(left.tokens) + len(right.tokens) - intersection
	overlap := float64(intersection) / float64(minimum)
	jaccard := float64(intersection) / float64(union)
	tokenScore := 0.6*overlap + 0.4*jaccard
	trigramIntersection := setIntersection(left.trigrams, right.trigrams)
	trigramScore := float64(2*trigramIntersection) / float64(len(left.trigrams)+len(right.trigrams))
	return int((0.8*tokenScore + 0.2*trigramScore) * 100)
}

func setIntersection(left, right map[string]struct{}) int {
	if len(left) > len(right) {
		left, right = right, left
	}
	count := 0
	for value := range left {
		if _, ok := right[value]; ok {
			count++
		}
	}
	return count
}
