package cue

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	MaxActiveCues        = 64
	MaxCandidates        = 32
	MaxObservations      = 256
	MaxSuppressed        = 128
	MaxFeaturesPerFrame  = 12
	MaxCandidateVariants = 3
	MaxPromptBytes       = 280
	MaxPatternBytes      = 512
	MinimumSupport       = 3
)

var visibleURL = regexp.MustCompile(`https?://[^\s<>"']+`)

type Context struct {
	Text    string
	Program string
	CWD     string
}

type Cue struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Pattern   string    `json:"pattern"`
	Prompt    string    `json:"prompt"`
	CreatedAt time.Time `json:"created_at"`
	UseCount  int       `json:"use_count,omitempty"`
}

type Candidate struct {
	ID                string    `json:"id"`
	Pattern           string    `json:"pattern"`
	Prompt            string    `json:"prompt"`
	Variants          []string  `json:"variants,omitempty"`
	FeatureKind       string    `json:"feature_kind"`
	Support           int       `json:"support"`
	ConfidencePercent int       `json:"confidence_percent"`
	CreatedAt         time.Time `json:"created_at"`
	ProposalChatID    int64     `json:"proposal_chat_id,omitempty"`
	ProposalMessageID int       `json:"proposal_message_id,omitempty"`
}

type Observation struct {
	PromptHash    string    `json:"prompt_hash"`
	FeatureHashes []string  `json:"feature_hashes"`
	At            time.Time `json:"at"`
}

type Match struct {
	CueID       string
	Name        string
	Prompt      string
	MatchHash   string
	MatchedText string
}

type feature struct {
	kind    string
	pattern string
	source  string
	score   int
}

func ExtractFeatures(context Context) []string {
	features := extractFeatures(context, "")
	out := make([]string, 0, len(features))
	for _, feature := range features {
		out = append(out, feature.pattern)
	}
	return out
}

func extractFeatures(context Context, prompt string) []feature {
	seen := make(map[string]bool)
	features := make([]feature, 0, MaxFeaturesPerFrame)
	add := func(candidate feature) {
		if candidate.pattern == "" || len(candidate.pattern) > MaxPatternBytes || seen[candidate.pattern] {
			return
		}
		if prompt != "" && sameVisibleText(candidate.source, prompt) {
			return
		}
		seen[candidate.pattern] = true
		features = append(features, candidate)
	}

	for _, raw := range visibleURL.FindAllString(context.Text, -1) {
		raw = strings.TrimRight(raw, ".,;:!?)]}")
		if pattern, ok := urlPattern(raw); ok {
			add(feature{kind: "url", pattern: pattern, source: raw, score: 10_000 + len(raw)})
		}
	}
	for _, raw := range strings.Split(context.Text, "\n") {
		line := strings.Join(strings.Fields(raw), " ")
		if !distinctiveLine(line) {
			continue
		}
		add(feature{kind: "line", pattern: linePattern(line), source: line, score: lineScore(line)})
	}
	sort.SliceStable(features, func(i, j int) bool {
		if features[i].score != features[j].score {
			return features[i].score > features[j].score
		}
		return features[i].pattern < features[j].pattern
	})
	if len(features) > MaxFeaturesPerFrame {
		features = features[:MaxFeaturesPerFrame]
	}
	return features
}

func urlPattern(raw string) (string, bool) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", false
	}
	path := parsed.EscapedPath()
	if path == "" {
		path = "/"
	}
	return regexp.QuoteMeta(parsed.Scheme+"://"+parsed.Host) + variableLiteralPattern(path), true
}

func linePattern(line string) string {
	return `(?m)^\s*` + variableLiteralPattern(line) + `\s*$`
}

func variableLiteralPattern(value string) string {
	var out strings.Builder
	for index := 0; index < len(value); {
		r, _ := utf8.DecodeRuneInString(value[index:])
		switch {
		case unicode.IsDigit(r):
			for index < len(value) {
				next, nextSize := utf8.DecodeRuneInString(value[index:])
				if !unicode.IsDigit(next) {
					break
				}
				index += nextSize
			}
			out.WriteString(`[0-9]+`)
		case unicode.IsSpace(r):
			for index < len(value) {
				next, nextSize := utf8.DecodeRuneInString(value[index:])
				if !unicode.IsSpace(next) {
					break
				}
				index += nextSize
			}
			out.WriteString(`\s+`)
		default:
			start := index
			for index < len(value) {
				next, nextSize := utf8.DecodeRuneInString(value[index:])
				if unicode.IsDigit(next) || unicode.IsSpace(next) {
					break
				}
				index += nextSize
			}
			out.WriteString(regexp.QuoteMeta(value[start:index]))
		}
	}
	return out.String()
}

func distinctiveLine(line string) bool {
	if len(line) < 16 || len(line) > 180 || strings.HasPrefix(line, "[") && strings.Contains(line, "tokens") {
		return false
	}
	letters, words := 0, 0
	inWord := false
	for _, r := range line {
		if unicode.IsLetter(r) {
			letters++
			if !inWord {
				words++
				inWord = true
			}
		} else {
			inWord = false
		}
	}
	return letters >= 10 && words >= 2
}

func lineScore(line string) int {
	score := len(line)
	if strings.Contains(line, "failed") || strings.Contains(line, "passed") || strings.Contains(line, "merged") || strings.Contains(line, "error") {
		score += 300
	}
	if strings.Contains(line, "github.com") {
		score += 200
	}
	return score
}

func sameVisibleText(feature, prompt string) bool {
	feature = strings.ToLower(strings.Join(strings.Fields(feature), " "))
	prompt = strings.ToLower(strings.Join(strings.Fields(prompt), " "))
	return feature == prompt || len(prompt) >= 16 && strings.Contains(feature, prompt)
}

func promptEligible(prompt string) bool {
	prompt = strings.TrimSpace(prompt)
	if len(prompt) < 8 || len(prompt) > MaxPromptBytes || strings.ContainsRune(prompt, '\x00') || strings.Contains(prompt, "```") {
		return false
	}
	lower := strings.ToLower(prompt)
	return !strings.HasPrefix(lower, "(transcribed)") && !strings.HasPrefix(lower, "(voice message:")
}

func featureHash(feature feature) string {
	return digest(feature.kind + "\x00" + feature.pattern)
}

func promptHash(prompt string) string {
	return digest(strings.TrimSpace(prompt))
}

func pairID(promptHash, featureHash string) string {
	return digest(promptHash + "\x00" + featureHash)[:16]
}

func cueID(pattern, prompt string) string {
	return digest(pattern + "\x00" + prompt)[:16]
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
