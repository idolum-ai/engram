// Package codexcontext reads a deliberately narrow, versioned subset of an
// exactly identified Codex rollout. It never discovers a session by working
// directory or recency.
package codexcontext

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/idolum-ai/engram/internal/sessioncontext"
)

const (
	ParserVersion   = "codex-rollout-v1"
	MaxContextBytes = sessioncontext.MaxContextBytes
	MaxMessageBytes = sessioncontext.MaxMessageBytes
	maxRolloutBytes = 32 << 20
	// Codex 0.147.0 can place generated environment and instruction metadata
	// above 2 MiB in one user-message record. Keep the per-record ceiling well
	// below the independent 32 MiB rollout budget while admitting that observed
	// shape so generated metadata can be discarded before context composition.
	maxJSONLineBytes  = 4 << 20
	maxCandidateFiles = 2
	maxWalkEntries    = 100000
	maxWalkDepth      = 5
	maxParsedMessages = 128
	MaxDiagramRows    = sessioncontext.MaxDiagramRows
	MaxDiagramColumns = sessioncontext.MaxDiagramColumns
)

type Role = sessioncontext.Role

const (
	RoleUser      = sessioncontext.RoleUser
	RoleAssistant = sessioncontext.RoleAssistant
)

type Message = sessioncontext.Message
type Context = sessioncontext.Context

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

// ExactRolloutPath resolves one hook-bound rollout without reading message
// content. It is exposed for the opt-in compatibility fixture inventory.
func ExactRolloutPath(root, sessionID string) (string, error) {
	if !validSessionID(sessionID) {
		return "", fmt.Errorf("invalid Codex session identity")
	}
	root = filepath.Clean(root)
	if root == "." || !filepath.IsAbs(root) {
		return "", fmt.Errorf("Codex sessions root is unavailable")
	}
	return exactRolloutPath(root, strings.ToLower(sessionID))
}

func (r Reader) Load(sessionID string, turnLimit int, transforms ...func(string) string) (Context, error) {
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
	return parseRollout(path, strings.ToLower(sessionID), turnLimit, transforms...)
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

func parseRollout(path, sessionID string, turnLimit int, transforms ...func(string) string) (Context, error) {
	return parseRolloutWithBudget(path, sessionID, turnLimit, maxRolloutBytes, transforms...)
}

// parseRolloutWithBudget keeps reading bounded even when a long-lived Codex
// session has grown beyond the context budget. Identity is proven from the
// beginning of the exact file, while recent messages are parsed from a fixed
// tail ending at the size observed when the file was opened.
func parseRolloutWithBudget(path, sessionID string, turnLimit int, readBudget int64, transforms ...func(string) string) (Context, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || readBudget < 1024 {
		return Context{}, fmt.Errorf("Codex rollout is not a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return Context{}, fmt.Errorf("open Codex rollout: %w", err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) || opened.Size() <= 0 {
		return Context{}, fmt.Errorf("Codex rollout identity changed")
	}

	size := opened.Size()
	if size <= readBudget {
		return parseRolloutRecords(io.NewSectionReader(file, 0, size), sessionID, turnLimit, false, transforms...)
	}

	identityBudget := min(int64(maxJSONLineBytes), readBudget/16)
	if err := verifyRolloutIdentity(io.NewSectionReader(file, 0, identityBudget), sessionID); err != nil {
		return Context{}, err
	}
	tailBudget := readBudget - identityBudget
	tail := bufio.NewReader(io.NewSectionReader(file, size-tailBudget, tailBudget))
	if err := discardPartialRecord(tail); err != nil {
		return Context{}, err
	}
	return parseRolloutRecords(tail, sessionID, turnLimit, true, transforms...)
}

func verifyRolloutIdentity(reader io.Reader, sessionID string) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), maxJSONLineBytes)
	for scanner.Scan() {
		var record struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return fmt.Errorf("parse %s identity record: %w", ParserVersion, err)
		}
		if record.Type != "session_meta" {
			continue
		}
		var metadata struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(record.Payload, &metadata); err != nil || strings.ToLower(metadata.ID) != sessionID {
			return fmt.Errorf("Codex rollout identity does not match")
		}
		return nil
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan Codex rollout identity: %w", err)
	}
	return fmt.Errorf("Codex rollout has no matching session metadata in bounded prefix")
}

func discardPartialRecord(reader *bufio.Reader) error {
	consumed := 0
	for {
		fragment, err := reader.ReadSlice('\n')
		consumed += len(fragment)
		if consumed > maxJSONLineBytes {
			return fmt.Errorf("Codex rollout tail starts inside an oversized record")
		}
		switch err {
		case nil:
			return nil
		case bufio.ErrBufferFull:
			continue
		case io.EOF:
			return fmt.Errorf("Codex rollout tail contains no complete record")
		default:
			return fmt.Errorf("align Codex rollout tail: %w", err)
		}
	}
}

func parseRolloutRecords(reader io.Reader, sessionID string, turnLimit int, verified bool, transforms ...func(string) string) (Context, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), maxJSONLineBytes)
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
			message, ok, err := parseVisibleMessage(record.Payload, transforms...)
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

func parseVisibleMessage(raw json.RawMessage, transforms ...func(string) string) (Message, bool, error) {
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
	redacted := false
	if len(transforms) > 0 && transforms[0] != nil {
		transformed := transforms[0](text)
		redacted = transformed != text
		text = transformed
	}
	return Message{Role: role, Text: headUTF8(text, MaxMessageBytes), Redacted: redacted}, true, nil
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
	return sessioncontext.PromptText(messages)
}

type Diagram = sessioncontext.Diagram

// DetectDiagram returns the latest conservative box/arrow diagram. It never
// asks a model to select or repair transcript text.
func DetectDiagram(messages []Message) (Diagram, bool) {
	return sessioncontext.DetectDiagram(messages)
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
