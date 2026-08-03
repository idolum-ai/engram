// Package codexcontext reads a deliberately narrow, versioned subset of an
// exactly identified Codex rollout. It never discovers a session by working
// directory or recency.
package codexcontext

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	ParserVersion     = "codex-rollout-v1"
	MaxContextBytes   = 12 << 10
	MaxMessageBytes   = 3 << 10
	maxRolloutBytes   = 32 << 20
	maxJSONLineBytes  = 2 << 20
	maxCandidateFiles = 2
	maxWalkEntries    = 100000
	maxWalkDepth      = 5
	maxParsedMessages = 128
	MaxDiagramRows    = 16
	MaxDiagramColumns = 80
)

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

type Message struct {
	Role Role
	Text string
}

type Context struct {
	Messages []Message
	Parser   string
}

// Reader resolves only a rollout whose filename carries the exact session UUID
// and whose session_meta record repeats it. Ambiguity fails closed.
type Reader struct {
	SessionsRoot string
}

func DefaultSessionsRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex", "sessions")
}

func (r Reader) Load(sessionID string, turnLimit int) (Context, error) {
	if !validSessionID(sessionID) {
		return Context{}, fmt.Errorf("invalid Codex session identity")
	}
	if turnLimit <= 0 {
		return Context{}, nil
	}
	root := filepath.Clean(r.SessionsRoot)
	if root == "." || !filepath.IsAbs(root) {
		return Context{}, fmt.Errorf("Codex sessions root is unavailable")
	}
	path, err := exactRolloutPath(root, strings.ToLower(sessionID))
	if err != nil {
		return Context{}, err
	}
	return parseRollout(path, strings.ToLower(sessionID), turnLimit)
}

func exactRolloutPath(root, sessionID string) (string, error) {
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("Codex sessions root is not a regular directory")
	}
	suffix := "-" + sessionID + ".jsonl"
	candidates := make([]string, 0, 1)
	entries := 0
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		entries++
		if entries > maxWalkEntries {
			return fmt.Errorf("Codex session store exceeds bounded scan")
		}
		if path != root {
			relative, relErr := filepath.Rel(root, path)
			if relErr != nil || strings.Count(filepath.ToSlash(relative), "/")+1 > maxWalkDepth {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		if path != root && entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type().IsRegular() && strings.HasPrefix(entry.Name(), "rollout-") && strings.HasSuffix(strings.ToLower(entry.Name()), suffix) {
			candidates = append(candidates, path)
			if len(candidates) >= maxCandidateFiles {
				return fs.SkipAll
			}
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("inspect Codex session store: %w", err)
	}
	if len(candidates) != 1 {
		return "", fmt.Errorf("exact Codex rollout is unavailable or ambiguous")
	}
	return candidates[0], nil
}

func parseRollout(path, sessionID string, turnLimit int) (Context, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > maxRolloutBytes {
		return Context{}, fmt.Errorf("Codex rollout is not a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return Context{}, fmt.Errorf("open Codex rollout: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), maxJSONLineBytes)
	verified := false
	messages := make([]Message, 0, min(maxParsedMessages, turnLimit*4))
	for scanner.Scan() {
		var record struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return Context{}, fmt.Errorf("parse %s record: %w", ParserVersion, err)
		}
		switch record.Type {
		case "session_meta":
			var metadata struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(record.Payload, &metadata); err != nil || strings.ToLower(metadata.ID) != sessionID {
				return Context{}, fmt.Errorf("Codex rollout identity does not match")
			}
			verified = true
		case "response_item":
			if !verified {
				continue
			}
			message, ok, err := parseVisibleMessage(record.Payload)
			if err != nil {
				return Context{}, err
			}
			if !ok {
				continue
			}
			messages = append(messages, message)
			if len(messages) > maxParsedMessages {
				messages = append([]Message(nil), messages[len(messages)-maxParsedMessages:]...)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return Context{}, fmt.Errorf("scan Codex rollout: %w", err)
	}
	if !verified {
		return Context{}, fmt.Errorf("Codex rollout has no matching session metadata")
	}
	return Context{Messages: boundMessages(recentTurns(messages, turnLimit), maxParsedMessages), Parser: ParserVersion}, nil
}

func recentTurns(messages []Message, limit int) []Message {
	if limit <= 0 || len(messages) == 0 {
		return nil
	}
	users := 0
	start := -1
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role != RoleUser {
			continue
		}
		users++
		start = index
		if users == limit {
			return messages[start:]
		}
	}
	if start < 0 {
		return nil
	}
	return messages[start:]
}

func parseVisibleMessage(raw json.RawMessage) (Message, bool, error) {
	var item struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return Message{}, false, fmt.Errorf("parse %s response item: %w", ParserVersion, err)
	}
	if item.Type != "message" || item.Role != string(RoleUser) && item.Role != string(RoleAssistant) {
		return Message{}, false, nil
	}
	wantType := "input_text"
	role := RoleUser
	if item.Role == string(RoleAssistant) {
		wantType = "output_text"
		role = RoleAssistant
	}
	parts := make([]string, 0, len(item.Content))
	for _, content := range item.Content {
		switch content.Type {
		case wantType:
			if strings.TrimSpace(content.Text) != "" && !generatedMetadata(content.Text) {
				parts = append(parts, content.Text)
			}
		case "input_image", "input_audio", "output_image", "attachment":
			// Known non-text user-visible material is deliberately excluded.
		default:
			return Message{}, false, fmt.Errorf("unsupported %s message content type", ParserVersion)
		}
	}
	text := strings.TrimSpace(strings.Join(parts, "\n"))
	if text == "" {
		return Message{}, false, nil
	}
	return Message{Role: role, Text: headUTF8(text, MaxMessageBytes)}, true, nil
}

func generatedMetadata(text string) bool {
	trimmed := strings.TrimSpace(text)
	for _, prefix := range []string{"<environment_context>", "<permissions instructions>", "# AGENTS.md instructions", "<developer_instructions>"} {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return false
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
		blocks := candidateBlocks(messages[messageIndex].Text)
		for blockIndex := len(blocks) - 1; blockIndex >= 0; blockIndex-- {
			text := strings.Trim(blocks[blockIndex], "\n")
			if diagramBlock(text) {
				return Diagram{Text: text, Message: messageIndex}, true
			}
		}
	}
	return Diagram{}, false
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
		if lineStructural {
			structural++
		}
	}
	box := boxCorners >= 2 && horizontal >= 2 && vertical >= 2 && structural >= 3
	flow := arrows >= 2 && structural >= 2 && (vertical >= 1 || horizontal >= 4)
	return box || flow
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

func validSessionID(id string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		return false
	}
	decoded, err := hex.DecodeString(strings.ReplaceAll(id, "-", ""))
	return err == nil && len(decoded) == 16
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
