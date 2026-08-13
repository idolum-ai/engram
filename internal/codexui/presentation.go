package codexui

import (
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

var elapsedDecoration = regexp.MustCompile(`^[─━═]+ Worked for (?:[0-9]+h )?(?:[0-9]+m )?[0-9]+s [─━═]+$`)
var turnDuration = regexp.MustCompile(`\bWorked for ((?:[0-9]+h )?(?:[0-9]+m )?[0-9]+s)\b`)

type Presentation struct {
	Text             string
	Applied          bool
	Version          string
	Model            string
	Effort           string
	Mode             string
	Activity         string
	Notice           string
	LastTurnSeconds  int
	ViewportStart    int
	ViewportBoundary string
}

func Present(runtime Runtime, text string) Presentation {
	fallback := Presentation{Text: text}
	if !runtime.Detected || !runtime.Supported || !supportedVersion(runtime.Version) || strings.TrimSpace(text) == "" {
		return fallback
	}
	lines := strings.Split(text, "\n")
	footer, model, effort, mode, ok := findFooter(lines)
	if !ok {
		return fallback
	}
	remove := make([]bool, len(lines))
	viewportStart, viewportBoundary := 0, ""
	if boundary, found := codexStartupBoundary(lines, runtime.Version, model); found {
		viewportStart = boundary + 1
		viewportBoundary = "codex_startup_card"
		for index := 0; index <= boundary; index++ {
			remove[index] = true
		}
	}
	remove[footer] = true
	activity := "idle"
	for i := max(0, footer-8); i < footer; i++ {
		trimmed := strings.TrimSpace(lines[i])
		if codexWorkingLine(trimmed) {
			activity = "working"
			remove[i] = true
		} else if codexApprovalReviewLine(trimmed) {
			activity = "reviewing approval"
			remove[i] = true
		}
	}
	removeKnownPlaceholder(lines, remove, footer)
	notice := ""
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		switch {
		case separatorLine(trimmed), elapsedLine(trimmed), collapsedTranscriptLine(trimmed):
			remove[i] = true
		case strings.HasPrefix(trimmed, "⚠ Automatic approval review approved"):
			markBlock(lines, remove, i)
		case strings.HasPrefix(trimmed, "✔ Auto-reviewer approved codex to run"):
			markBlock(lines, remove, i)
		case i >= max(0, footer-16) && strings.HasPrefix(trimmed, "⚠"):
			block, end := textBlock(lines, i)
			lower := strings.ToLower(block)
			if strings.Contains(lower, "switch") && strings.Contains(lower, "model") && (strings.Contains(lower, "security") || strings.Contains(lower, "review")) {
				notice = truncateUTF8(strings.Join(strings.Fields(block), " "), 180)
				for index := i; index < end; index++ {
					remove[index] = true
				}
				i = end - 1
			}
		}
	}
	kept := make([]string, 0, len(lines))
	for i, line := range lines {
		if !remove[i] {
			kept = append(kept, line)
		}
	}
	cleaned := strings.Trim(strings.Join(kept, "\n"), "\n")
	if strings.TrimSpace(cleaned) == "" {
		return fallback
	}
	return Presentation{
		Text: cleaned, Applied: true, Version: runtime.Version, Model: model,
		Effort: effort, Mode: mode, Activity: activity, Notice: notice,
		LastTurnSeconds: lastTurnSeconds(lines), ViewportStart: viewportStart, ViewportBoundary: viewportBoundary,
	}
}

func codexStartupBoundary(lines []string, version, model string) (int, bool) {
	for start := 0; start < len(lines) && start < 12; start++ {
		trimmed := strings.TrimSpace(lines[start])
		if !strings.HasPrefix(trimmed, "╭") || !strings.HasSuffix(trimmed, "╮") {
			continue
		}
		identified := false
		modelIdentified := false
		for end := start + 1; end < len(lines) && end <= start+10; end++ {
			line := strings.TrimSpace(lines[end])
			if strings.Contains(line, ">_ OpenAI Codex") && strings.Contains(line, "(v"+version+")") {
				identified = true
			}
			if strings.HasPrefix(strings.TrimSpace(strings.Trim(line, "│")), "model:") && strings.Contains(line, model) {
				modelIdentified = true
			}
			if strings.HasPrefix(line, "╰") && strings.HasSuffix(line, "╯") {
				if identified && modelIdentified {
					return end, true
				}
				break
			}
		}
	}
	return 0, false
}

func lastTurnSeconds(lines []string) int {
	for index := len(lines) - 1; index >= 0; index-- {
		trimmed := strings.TrimSpace(lines[index])
		if !elapsedLine(trimmed) {
			continue
		}
		match := turnDuration.FindStringSubmatch(trimmed)
		if len(match) != 2 {
			continue
		}
		duration, err := time.ParseDuration(strings.ReplaceAll(match[1], " ", ""))
		if err == nil && duration > 0 && duration <= 7*24*time.Hour {
			return int(duration / time.Second)
		}
	}
	return 0
}

func findFooter(lines []string) (index int, model, effort, mode string, ok bool) {
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-10; i-- {
		parts := strings.Split(strings.TrimSpace(lines[i]), " · ")
		if len(parts) < 2 {
			continue
		}
		identity := strings.Fields(parts[0])
		if len(identity) < 2 || len(identity) > 3 || !validModel(identity[0]) || !validEffort(identity[1]) || strings.TrimSpace(parts[1]) == "" {
			continue
		}
		if len(identity) == 3 {
			if identity[2] != "fast" {
				continue
			}
			mode = identity[2]
		}
		return i, identity[0], identity[1], mode, true
	}
	return 0, "", "", "", false
}

func validModel(model string) bool {
	if len(model) < 2 || len(model) > 64 {
		return false
	}
	for _, r := range model {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

func validEffort(effort string) bool {
	switch effort {
	case "minimal", "low", "medium", "high", "xhigh":
		return true
	default:
		return false
	}
}

func codexWorkingLine(line string) bool {
	line = strings.TrimSpace(strings.TrimLeft(line, "•◦"))
	return strings.HasPrefix(line, "Working (") && strings.Contains(line, "esc to interrupt")
}

func codexApprovalReviewLine(line string) bool {
	line = strings.TrimSpace(strings.TrimLeft(line, "•◦"))
	if strings.HasPrefix(line, "Reviewing approval request (") {
		return strings.Contains(line, "esc to interrupt")
	}
	const prefix = "Reviewing "
	const suffix = " approval requests ("
	if !strings.HasPrefix(line, prefix) || !strings.Contains(line, suffix) || !strings.Contains(line, "esc to interrupt") {
		return false
	}
	count := strings.TrimSuffix(strings.TrimPrefix(strings.SplitN(line, suffix, 2)[0], prefix), " ")
	if count == "" || len(count) > 3 || count[0] == '0' {
		return false
	}
	for _, digit := range count {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

func removeKnownPlaceholder(lines []string, remove []bool, footer int) {
	end := footer - 1
	for end >= 0 && (strings.TrimSpace(lines[end]) == "" || remove[end]) {
		end--
	}
	if end < 0 {
		return
	}
	start := end
	for start > 0 && end-start < 3 && strings.TrimSpace(lines[start-1]) != "" && !remove[start-1] {
		start--
	}
	first := strings.TrimSpace(lines[start])
	if !strings.HasPrefix(first, "›") {
		return
	}
	parts := append([]string(nil), lines[start:end+1]...)
	parts[0] = strings.TrimSpace(strings.TrimPrefix(first, "›"))
	prompt := strings.Join(strings.Fields(strings.Join(parts, " ")), " ")
	switch prompt {
	case "Write tests for @filename", "Run /review on my current changes", "Find and fix a bug in @filename", "Summarize recent commits", "Implement {feature}", "Explain this codebase":
		for i := start; i <= end; i++ {
			remove[i] = true
		}
	}
}

func separatorLine(line string) bool {
	if utf8.RuneCountInString(line) < 8 {
		return false
	}
	for _, r := range line {
		if r != '─' && r != '━' && r != '═' {
			return false
		}
	}
	return true
}

func elapsedLine(line string) bool {
	return elapsedDecoration.MatchString(line)
}

func collapsedTranscriptLine(line string) bool {
	return strings.HasPrefix(line, "… +") && strings.Contains(line, "lines") && strings.Contains(line, "view transcript")
}

func markBlock(lines []string, remove []bool, start int) {
	_, end := textBlock(lines, start)
	for i := start; i < end; i++ {
		remove[i] = true
	}
}

func textBlock(lines []string, start int) (string, int) {
	end := start
	for end < len(lines) && strings.TrimSpace(lines[end]) != "" {
		end++
	}
	return strings.Join(lines[start:end], "\n"), end
}

func truncateUTF8(text string, maxBytes int) string {
	if len(text) <= maxBytes {
		return text
	}
	end := maxBytes
	for end > 0 && !utf8.ValidString(text[:end]) {
		end--
	}
	return strings.TrimSpace(text[:end])
}
