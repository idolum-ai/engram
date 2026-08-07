// Package claudecontext reads a deliberately narrow, versioned subset of an
// exactly hook-bound Claude Code transcript. The JSONL schema is internal to
// Claude Code, so recognized message variants fail closed.
package claudecontext

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/idolum-ai/engram/internal/sessioncontext"
)

const (
	ParserVersion     = "claude-transcript-v1"
	maxTranscriptRead = 32 << 20
	maxJSONLineBytes  = 2 << 20
	maxParsedRecords  = 100000
	maxMessages       = 128
)

type Reader struct{}

func (Reader) Load(path, sessionID string, turnLimit int, transforms ...func(string) string) (sessioncontext.Context, error) {
	if !validSessionID(sessionID) {
		return sessioncontext.Context{}, fmt.Errorf("invalid Claude session identity")
	}
	if turnLimit <= 0 {
		return sessioncontext.Context{}, nil
	}
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) || strings.ToLower(filepath.Base(path)) != strings.ToLower(sessionID)+".jsonl" {
		return sessioncontext.Context{}, fmt.Errorf("Claude transcript path does not match session identity")
	}
	return parseTranscript(path, strings.ToLower(sessionID), turnLimit, transforms...)
}

func parseTranscript(path, sessionID string, turnLimit int, transforms ...func(string) string) (sessioncontext.Context, error) {
	return parseTranscriptWithBudget(path, sessionID, turnLimit, maxTranscriptRead, transforms...)
}

func parseTranscriptWithBudget(path, sessionID string, turnLimit int, readBudget int64, transforms ...func(string) string) (sessioncontext.Context, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || !ownedByCurrentUser(info) || readBudget < 1024 {
		return sessioncontext.Context{}, fmt.Errorf("Claude transcript is not an owned regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return sessioncontext.Context{}, fmt.Errorf("open Claude transcript: %w", err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) || opened.Size() <= 0 || !ownedByCurrentUser(opened) {
		return sessioncontext.Context{}, fmt.Errorf("Claude transcript identity changed")
	}

	size := opened.Size()
	if size <= readBudget {
		return parseRecords(io.NewSectionReader(file, 0, size), sessionID, turnLimit, false, transforms...)
	}
	prefixBudget := min(int64(maxJSONLineBytes), readBudget/4)
	if err := verifyIdentity(io.NewSectionReader(file, 0, prefixBudget), sessionID); err != nil {
		return sessioncontext.Context{}, err
	}
	tailBudget := readBudget - prefixBudget
	tail := bufio.NewReader(io.NewSectionReader(file, size-tailBudget, tailBudget))
	if err := discardPartialRecord(tail); err != nil {
		return sessioncontext.Context{}, err
	}
	return parseRecords(tail, sessionID, turnLimit, true, transforms...)
}

func verifyIdentity(reader io.Reader, sessionID string) error {
	scanner := newScanner(reader)
	for records := 0; scanner.Scan() && records < maxParsedRecords; records++ {
		var envelope struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &envelope); err != nil {
			return fmt.Errorf("parse %s identity record: %w", ParserVersion, err)
		}
		if envelope.SessionID == "" {
			continue
		}
		if strings.ToLower(envelope.SessionID) != sessionID {
			return fmt.Errorf("Claude transcript identity does not match")
		}
		return nil
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan Claude transcript identity: %w", err)
	}
	return fmt.Errorf("Claude transcript has no matching session identity in bounded prefix")
}

func parseRecords(reader io.Reader, sessionID string, turnLimit int, verified bool, transforms ...func(string) string) (sessioncontext.Context, error) {
	scanner := newScanner(reader)
	messages := make([]sessioncontext.Message, 0, min(maxMessages, turnLimit*4))
	assistantIndexes := make(map[string]int)
	assistantParts := make(map[string]map[string]bool)
	records := 0
	for scanner.Scan() {
		records++
		if records > maxParsedRecords {
			return sessioncontext.Context{}, fmt.Errorf("Claude transcript exceeds bounded record count")
		}
		var record transcriptRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return sessioncontext.Context{}, fmt.Errorf("parse %s record: %w", ParserVersion, err)
		}
		if record.SessionID != "" {
			if strings.ToLower(record.SessionID) != sessionID {
				return sessioncontext.Context{}, fmt.Errorf("Claude transcript identity does not match")
			}
			verified = true
		}
		if !verified || record.IsSidechain {
			continue
		}
		switch record.Type {
		case "user":
			message, ok, err := parseUser(record, transforms...)
			if err != nil {
				return sessioncontext.Context{}, err
			}
			if ok {
				messages = append(messages, message)
			}
		case "assistant":
			parts, ok, err := parseAssistant(record, transforms...)
			if err != nil {
				return sessioncontext.Context{}, err
			}
			if !ok {
				continue
			}
			key := strings.TrimSpace(record.Message.ID)
			if key == "" {
				key = strings.TrimSpace(record.UUID)
			}
			if key == "" {
				return sessioncontext.Context{}, fmt.Errorf("%s assistant message has no stable identity", ParserVersion)
			}
			index, found := assistantIndexes[key]
			if !found {
				index = len(messages)
				assistantIndexes[key] = index
				assistantParts[key] = make(map[string]bool)
				messages = append(messages, sessioncontext.Message{Role: sessioncontext.RoleAssistant})
			}
			for _, part := range parts {
				if assistantParts[key][part.text] {
					continue
				}
				assistantParts[key][part.text] = true
				if messages[index].Text != "" {
					messages[index].Text += "\n"
				}
				messages[index].Text += part.text
				messages[index].Redacted = messages[index].Redacted || part.redacted
			}
			messages[index].Text = headUTF8(messages[index].Text, sessioncontext.MaxMessageBytes)
		}
		if len(messages) > maxMessages {
			// Rebuild indexes after bounding so split assistant records cannot
			// mutate an evicted message.
			messages = append([]sessioncontext.Message(nil), messages[len(messages)-maxMessages:]...)
			assistantIndexes = make(map[string]int)
			assistantParts = make(map[string]map[string]bool)
		}
	}
	if err := scanner.Err(); err != nil {
		return sessioncontext.Context{}, fmt.Errorf("scan Claude transcript: %w", err)
	}
	if !verified {
		return sessioncontext.Context{}, fmt.Errorf("Claude transcript has no matching session identity")
	}
	messages = recentTurns(messages, turnLimit)
	messages = boundMessages(messages)
	return sessioncontext.Context{Messages: messages, Parser: ParserVersion}, nil
}

type transcriptRecord struct {
	Type        string `json:"type"`
	SessionID   string `json:"sessionId"`
	UUID        string `json:"uuid"`
	IsMeta      bool   `json:"isMeta"`
	IsSidechain bool   `json:"isSidechain"`
	AgentID     string `json:"agentId"`
	Message     struct {
		ID              string          `json:"id"`
		Role            string          `json:"role"`
		ParentToolUseID string          `json:"parent_tool_use_id"`
		Content         json.RawMessage `json:"content"`
	} `json:"message"`
}

func parseUser(record transcriptRecord, transforms ...func(string) string) (sessioncontext.Message, bool, error) {
	if record.IsMeta || record.AgentID != "" || record.Message.ParentToolUseID != "" || record.Message.Role != "user" || len(record.Message.Content) == 0 {
		return sessioncontext.Message{}, false, nil
	}
	var text string
	if err := json.Unmarshal(record.Message.Content, &text); err != nil {
		// Array-form user messages are tool results or attachments in the
		// supported transcript contract and are deliberately excluded.
		var array []json.RawMessage
		if json.Unmarshal(record.Message.Content, &array) == nil {
			return sessioncontext.Message{}, false, nil
		}
		return sessioncontext.Message{}, false, fmt.Errorf("unsupported %s user content", ParserVersion)
	}
	text = strings.TrimSpace(text)
	if text == "" || generatedMetadata(text) {
		return sessioncontext.Message{}, false, nil
	}
	text, redacted := transform(text, transforms...)
	return sessioncontext.Message{Role: sessioncontext.RoleUser, Text: headUTF8(text, sessioncontext.MaxMessageBytes), Redacted: redacted}, true, nil
}

type textPart struct {
	text     string
	redacted bool
}

func parseAssistant(record transcriptRecord, transforms ...func(string) string) ([]textPart, bool, error) {
	if record.AgentID != "" || record.Message.ParentToolUseID != "" || record.Message.Role != "assistant" || len(record.Message.Content) == 0 {
		return nil, false, nil
	}
	var content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(record.Message.Content, &content); err != nil {
		return nil, false, fmt.Errorf("unsupported %s assistant content", ParserVersion)
	}
	var parts []textPart
	for _, block := range content {
		switch block.Type {
		case "text":
			text := strings.TrimSpace(block.Text)
			if text == "" {
				continue
			}
			text, redacted := transform(text, transforms...)
			parts = append(parts, textPart{text: headUTF8(text, sessioncontext.MaxMessageBytes), redacted: redacted})
		case "thinking", "redacted_thinking", "tool_use", "server_tool_use":
			// Hidden reasoning and tool payloads are deliberately excluded.
		default:
			return nil, false, fmt.Errorf("unsupported %s assistant block type", ParserVersion)
		}
	}
	return parts, len(parts) > 0, nil
}

func transform(text string, transforms ...func(string) string) (string, bool) {
	if len(transforms) == 0 || transforms[0] == nil {
		return text, false
	}
	transformed := transforms[0](text)
	return transformed, transformed != text
}

func recentTurns(messages []sessioncontext.Message, limit int) []sessioncontext.Message {
	users, start := 0, -1
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role != sessioncontext.RoleUser {
			continue
		}
		users++
		start = index
		if users == limit {
			break
		}
	}
	if start < 0 {
		return nil
	}
	return append([]sessioncontext.Message(nil), messages[start:]...)
}

func boundMessages(messages []sessioncontext.Message) []sessioncontext.Message {
	total, start := 0, len(messages)
	for start > 0 {
		cost := len(messages[start-1].Text) + len(messages[start-1].Role) + 4
		if total+cost > sessioncontext.MaxContextBytes {
			break
		}
		total += cost
		start--
	}
	return append([]sessioncontext.Message(nil), messages[start:]...)
}

func generatedMetadata(text string) bool {
	trimmed := strings.TrimSpace(text)
	for _, prefix := range []string{"<environment_context>", "<permissions instructions>", "# AGENTS.md instructions", "<developer_instructions>", "<system-reminder>"} {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return false
}

func discardPartialRecord(reader *bufio.Reader) error {
	consumed := 0
	for {
		fragment, err := reader.ReadSlice('\n')
		consumed += len(fragment)
		if consumed > maxJSONLineBytes {
			return fmt.Errorf("Claude transcript tail starts inside an oversized record")
		}
		switch err {
		case nil:
			return nil
		case bufio.ErrBufferFull:
			continue
		case io.EOF:
			return fmt.Errorf("Claude transcript tail contains no complete record")
		default:
			return fmt.Errorf("align Claude transcript tail: %w", err)
		}
	}
}

func newScanner(reader io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), maxJSONLineBytes)
	return scanner
}

func validSessionID(id string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		return false
	}
	decoded, err := hex.DecodeString(strings.ReplaceAll(id, "-", ""))
	return err == nil && len(decoded) == 16
}

func headUTF8(value string, limit int) string {
	if limit <= 0 || value == "" {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for len(value) > 0 && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
