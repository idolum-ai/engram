package app

import "strings"

// conversationEvidence removes a trailing idle prompt and status footer from
// model input. They are terminal controls, not evidence about the work. Raw
// captures remain unchanged for snapshots, references, and local inspection.
func conversationEvidence(text string) string {
	lines := strings.Split(text, "\n")
	end := lastNonBlankLine(lines)
	if end < 0 || !isPassiveTerminalFooter(lines[end]) {
		return text
	}

	end = lastNonBlankLine(lines[:end])
	if start := trailingIdlePromptStart(lines, end); start >= 0 {
		end = start - 1
	}
	for end >= 0 && strings.TrimSpace(lines[end]) == "" {
		end--
	}
	return strings.TrimRight(strings.Join(lines[:end+1], "\n"), "\n")
}

func lastNonBlankLine(lines []string) int {
	for index := len(lines) - 1; index >= 0; index-- {
		if strings.TrimSpace(lines[index]) != "" {
			return index
		}
	}
	return -1
}

func isPassiveTerminalFooter(line string) bool {
	line = strings.TrimSpace(line)
	if strings.Count(line, "\u00b7") < 2 || !strings.Contains(line, "[") || !strings.HasSuffix(line, "]") {
		return false
	}
	label := strings.ToLower(strings.Fields(line)[0])
	for _, prefix := range []string{"gpt-", "claude", "gemini", "codex", "o1", "o3", "o4"} {
		if strings.HasPrefix(label, prefix) {
			return true
		}
	}
	return false
}

func isIdlePromptLine(line string) bool {
	line = strings.TrimSpace(line)
	return strings.HasPrefix(line, "\u203a") || strings.HasPrefix(line, ">")
}

func trailingIdlePromptStart(lines []string, end int) int {
	if end < 0 {
		return -1
	}
	start := end
	for start > 0 && strings.TrimSpace(lines[start-1]) != "" {
		start--
	}
	if isIdlePromptLine(lines[start]) {
		return start
	}
	return -1
}
