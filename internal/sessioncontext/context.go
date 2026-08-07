// Package sessioncontext defines the provider-neutral visible conversation
// context passed to Engram's summarization and diagram pipeline.
package sessioncontext

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	MaxContextBytes   = 12 << 10
	MaxMessageBytes   = 3 << 10
	MaxDiagramRows    = 16
	MaxDiagramColumns = 80
)

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

type Message struct {
	Role     Role
	Text     string
	Redacted bool
}

type Context struct {
	Messages []Message
	Parser   string
}

func PromptText(messages []Message) string {
	var out strings.Builder
	for _, message := range boundMessages(messages, len(messages)) {
		if out.Len() > 0 {
			out.WriteString("\n\n")
		}
		if message.Role == RoleUser {
			out.WriteString("User:\n")
		} else {
			out.WriteString("Assistant:\n")
		}
		out.WriteString(message.Text)
	}
	return headUTF8(out.String(), MaxContextBytes)
}

type Diagram struct {
	Text    string
	Message int
}

// DetectDiagram returns the latest conservative box/arrow diagram. It never
// asks a model to select or repair transcript text.
func DetectDiagram(messages []Message) (Diagram, bool) {
	for messageIndex := len(messages) - 1; messageIndex >= 0; messageIndex-- {
		blocks := candidateDiagramExtents(messages[messageIndex].Text)
		for blockIndex := len(blocks) - 1; blockIndex >= 0; blockIndex-- {
			text := strings.Trim(blocks[blockIndex], "\n")
			if diagramBlock(text) {
				return Diagram{Text: text, Message: messageIndex}, true
			}
		}
	}
	return Diagram{}, false
}

func boundMessages(messages []Message, limit int) []Message {
	if limit <= 0 || len(messages) == 0 {
		return nil
	}
	if len(messages) > limit {
		messages = messages[len(messages)-limit:]
	}
	total := 0
	start := len(messages)
	for start > 0 {
		cost := len(messages[start-1].Text) + len(messages[start-1].Role) + 4
		if total+cost > MaxContextBytes {
			break
		}
		total += cost
		start--
	}
	return append([]Message(nil), messages[start:]...)
}

func candidateDiagramExtents(text string) []string {
	blocks := candidateBlocks(text)
	extents := make([]string, 0, len(blocks))
	for _, block := range blocks {
		var rows []string
		flush := func() {
			if len(rows) >= 3 {
				extents = append(extents, strings.Join(rows, "\n"))
			}
			rows = nil
		}
		for _, line := range strings.Split(block, "\n") {
			if diagramStructuralLine(line) {
				rows = append(rows, line)
				continue
			}
			flush()
		}
		flush()
	}
	return extents
}

func candidateBlocks(text string) []string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	blocks := make([]string, 0, 4)
	var current []string
	flush := func() {
		if len(current) >= 3 {
			blocks = append(blocks, strings.Join(current, "\n"))
		}
		current = nil
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || trimmed == "" {
			flush()
			continue
		}
		current = append(current, strings.TrimRight(line, " \t"))
	}
	flush()
	return blocks
}

func diagramBlock(text string) bool {
	lines := strings.Split(text, "\n")
	if len(lines) < 3 || len(lines) > MaxDiagramRows || len(text) > 8<<10 {
		return false
	}
	boxCorners, horizontal, vertical, arrows, structural := 0, 0, 0, 0, 0
	for _, line := range lines {
		width := terminalCells(line)
		if width == 0 || width > MaxDiagramColumns || strings.ContainsRune(line, '\t') {
			return false
		}
		lineStructural := false
		for _, r := range line {
			switch {
			case strings.ContainsRune("┌┐└┘╭╮╰╯┏┓┗┛+", r):
				boxCorners++
				lineStructural = true
			case strings.ContainsRune("─━═-", r):
				horizontal++
				lineStructural = true
			case strings.ContainsRune("│┃║|", r):
				vertical++
				lineStructural = true
			case strings.ContainsRune("→←↑↓↔↕⇒⇐⇄⇆", r):
				arrows++
				lineStructural = true
			}
		}
		arrows += strings.Count(line, "->") + strings.Count(line, "<-") + strings.Count(line, "=>")
		if strings.Contains(line, "->") || strings.Contains(line, "<-") || strings.Contains(line, "=>") {
			lineStructural = true
		}
		if !lineStructural {
			return false
		}
		structural++
	}
	box := boxCorners >= 2 && horizontal >= 2 && vertical >= 2 && structural >= 3
	flow := arrows >= 2 && structural >= 2 && (vertical >= 1 || horizontal >= 4)
	return box || flow
}

func diagramStructuralLine(line string) bool {
	for _, r := range line {
		if strings.ContainsRune("┌┐└┘╭╮╰╯┏┓┗┛─━═│┃║→←↑↓↔↕⇒⇐⇄⇆", r) {
			return true
		}
	}
	if strings.Contains(line, "->") || strings.Contains(line, "<-") || strings.Contains(line, "=>") {
		return true
	}
	trimmed := strings.TrimSpace(line)
	if trimmed == "|" {
		return true
	}
	pipes := strings.Count(trimmed, "|")
	pluses := strings.Count(trimmed, "+")
	hyphens := strings.Count(trimmed, "-")
	return pipes >= 2 || pluses >= 2 && hyphens >= 2
}

func terminalCells(text string) int {
	cells := 0
	for _, r := range text {
		switch {
		case r == 0 || unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || unicode.Is(unicode.Cf, r):
		case r < 0x20 || r == 0x7f:
			return MaxDiagramColumns + 1
		case wideRune(r):
			cells += 2
		default:
			cells++
		}
	}
	return cells
}

func wideRune(r rune) bool {
	return r >= 0x1100 && (r <= 0x115f || r == 0x2329 || r == 0x232a ||
		r >= 0x2e80 && r <= 0xa4cf && r != 0x303f || r >= 0xac00 && r <= 0xd7a3 ||
		r >= 0xf900 && r <= 0xfaff || r >= 0xfe10 && r <= 0xfe19 ||
		r >= 0xfe30 && r <= 0xfe6f || r >= 0xff00 && r <= 0xff60 ||
		r >= 0xffe0 && r <= 0xffe6 || r >= 0x1f300 && r <= 0x1faff ||
		r >= 0x20000 && r <= 0x3fffd)
}

func headUTF8(text string, maximum int) string {
	if len(text) <= maximum {
		return text
	}
	end := maximum
	for end > 0 && !utf8.ValidString(text[:end]) {
		end--
	}
	return text[:end]
}
