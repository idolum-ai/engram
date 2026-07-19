package app

import "strings"

// conversationEvidence removes a model-status footer and its paired known
// placeholder from model input. Raw captures remain unchanged for snapshots,
// references, and local inspection.
func conversationEvidence(text string) string {
	lines := strings.Split(text, "\n")
	end := lastNonBlankLine(lines)
	if end < 0 || !isPassiveTerminalFooter(lines[end]) {
		return text
	}

	end = lastNonBlankLine(lines[:end])
	if start := trailingPassivePromptStart(lines, end); start >= 0 {
		end = start - 1
	}
	for end >= 0 && strings.TrimSpace(lines[end]) == "" {
		end--
	}
	filtered := strings.TrimRight(strings.Join(lines[:end+1], "\n"), "\n")
	if strings.TrimSpace(filtered) == "" {
		return text
	}
	return filtered
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
	separatorCount := strings.Count(line, "\u00b7")
	if separatorCount < 1 {
		return false
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return false
	}
	label := strings.ToLower(fields[0])
	for _, prefix := range []string{"gpt-", "claude", "gemini", "codex", "o1", "o3", "o4"} {
		if strings.HasPrefix(label, prefix) {
			if separatorCount >= 2 && strings.Contains(line, "[") && strings.HasSuffix(line, "]") {
				return true
			}
			tail := strings.TrimSpace(line[strings.LastIndex(line, "\u00b7")+len("\u00b7"):])
			return tail == "~" || strings.HasPrefix(tail, "~/") || strings.HasPrefix(tail, "/")
		}
	}
	return false
}

func trailingPassivePromptStart(lines []string, end int) int {
	if end < 0 {
		return -1
	}
	start := end
	for start > 0 && strings.TrimSpace(lines[start-1]) != "" {
		start--
	}
	first := strings.TrimSpace(lines[start])
	if !strings.HasPrefix(first, "\u203a") {
		return -1
	}
	lines = append([]string(nil), lines[start:end+1]...)
	lines[0] = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[0]), "\u203a"))
	if isPassivePromptSuggestion(strings.ToLower(strings.Join(strings.Fields(strings.Join(lines, " ")), " "))) {
		return start
	}
	return -1
}

func isPassivePromptSuggestion(text string) bool {
	switch text {
	case "find and fix a bug in @filename", "run /review on my current changes", "write tests for @filename":
		return true
	default:
		return false
	}
}
