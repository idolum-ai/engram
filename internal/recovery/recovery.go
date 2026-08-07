// Package recovery defines the small, provider-neutral recovery metadata
// exchanged between terminal lifecycle hooks, tmux, and Engram state.
package recovery

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"
)

const (
	ProgramCodex  = "codex"
	ProgramClaude = "claude"
	maxHookBytes  = 64 << 10
)

type Metadata struct {
	Version        int       `json:"version"`
	Program        string    `json:"program"`
	SessionID      string    `json:"session_id"`
	CWD            string    `json:"cwd,omitempty"`
	TranscriptPath string    `json:"transcript_path,omitempty"`
	Model          string    `json:"model,omitempty"`
	Source         string    `json:"source,omitempty"`
	Observed       time.Time `json:"observed_at"`
}

type codexHookInput struct {
	SessionID     string `json:"session_id"`
	CWD           string `json:"cwd"`
	HookEventName string `json:"hook_event_name"`
	Source        string `json:"source"`
}

type claudeHookInput struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	CWD            string `json:"cwd"`
	HookEventName  string `json:"hook_event_name"`
	Source         string `json:"source"`
	Model          string `json:"model"`
}

func ParseCodexSessionStart(input io.Reader, now time.Time) (Metadata, error) {
	data, err := io.ReadAll(io.LimitReader(input, maxHookBytes+1))
	if err != nil {
		return Metadata{}, fmt.Errorf("read Codex hook input: %w", err)
	}
	if len(data) > maxHookBytes {
		return Metadata{}, fmt.Errorf("Codex hook input is too large")
	}
	var event codexHookInput
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	if err := decoder.Decode(&event); err != nil {
		return Metadata{}, fmt.Errorf("decode Codex hook input: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Metadata{}, fmt.Errorf("Codex hook input has trailing data")
	}
	if event.HookEventName != "SessionStart" {
		return Metadata{}, fmt.Errorf("unsupported Codex hook event %q", event.HookEventName)
	}
	if !ValidSessionID(event.SessionID) {
		return Metadata{}, fmt.Errorf("invalid Codex session id")
	}
	if len(event.CWD) > 4096 || strings.ContainsRune(event.CWD, '\x00') {
		return Metadata{}, fmt.Errorf("invalid Codex working directory")
	}
	source := strings.ToLower(strings.TrimSpace(event.Source))
	if source != "startup" && source != "resume" && source != "clear" && source != "compact" {
		source = "startup"
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return Metadata{
		Version: 1, Program: ProgramCodex, SessionID: strings.ToLower(event.SessionID),
		CWD: event.CWD, Source: source, Observed: now.UTC(),
	}, nil
}

func ParseClaudeSessionStart(input io.Reader, now time.Time) (Metadata, error) {
	data, err := io.ReadAll(io.LimitReader(input, maxHookBytes+1))
	if err != nil {
		return Metadata{}, fmt.Errorf("read Claude hook input: %w", err)
	}
	if len(data) > maxHookBytes {
		return Metadata{}, fmt.Errorf("Claude hook input is too large")
	}
	var event claudeHookInput
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	if err := decoder.Decode(&event); err != nil {
		return Metadata{}, fmt.Errorf("decode Claude hook input: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Metadata{}, fmt.Errorf("Claude hook input has trailing data")
	}
	if event.HookEventName != "SessionStart" {
		return Metadata{}, fmt.Errorf("unsupported Claude hook event %q", event.HookEventName)
	}
	if !ValidSessionID(event.SessionID) {
		return Metadata{}, fmt.Errorf("invalid Claude session id")
	}
	if err := validateCWD(event.CWD); err != nil {
		return Metadata{}, fmt.Errorf("invalid Claude working directory")
	}
	transcriptPath, err := validateTranscriptPath(event.TranscriptPath)
	if err != nil {
		return Metadata{}, err
	}
	if strings.ToLower(filepath.Base(transcriptPath)) != strings.ToLower(event.SessionID)+".jsonl" {
		return Metadata{}, fmt.Errorf("Claude transcript path does not match session id")
	}
	model, err := validateModel(event.Model)
	if err != nil {
		return Metadata{}, fmt.Errorf("invalid Claude model")
	}
	source := strings.ToLower(strings.TrimSpace(event.Source))
	if !validSource(source, false) {
		return Metadata{}, fmt.Errorf("unsupported Claude lifecycle source %q", event.Source)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return Metadata{
		Version: 1, Program: ProgramClaude, SessionID: strings.ToLower(event.SessionID),
		CWD: event.CWD, TranscriptPath: transcriptPath, Model: model, Source: source, Observed: now.UTC(),
	}, nil
}

func Encode(metadata Metadata) (string, error) {
	metadata.Version = 1
	metadata, err := validateMetadata(metadata)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(metadata)
	if err != nil {
		return "", err
	}
	if len(data) > 8192 {
		return "", fmt.Errorf("recovery metadata is too large")
	}
	return string(data), nil
}

func Decode(value string) (Metadata, error) {
	if len(value) == 0 || len(value) > 8192 {
		return Metadata{}, fmt.Errorf("invalid recovery metadata size")
	}
	var metadata Metadata
	if err := json.Unmarshal([]byte(value), &metadata); err != nil {
		return Metadata{}, fmt.Errorf("decode recovery metadata: %w", err)
	}
	return validateMetadata(metadata)
}

func validateMetadata(metadata Metadata) (Metadata, error) {
	metadata.Program = strings.ToLower(strings.TrimSpace(metadata.Program))
	metadata.SessionID = strings.ToLower(strings.TrimSpace(metadata.SessionID))
	metadata.Source = strings.ToLower(strings.TrimSpace(metadata.Source))
	if metadata.Version != 1 || !ValidProgram(metadata.Program) || !ValidSessionID(metadata.SessionID) {
		return Metadata{}, fmt.Errorf("invalid recovery metadata")
	}
	if !validSource(metadata.Source, true) {
		return Metadata{}, fmt.Errorf("invalid recovery metadata source")
	}
	if err := validateCWD(metadata.CWD); err != nil {
		return Metadata{}, fmt.Errorf("invalid recovery working directory")
	}
	if metadata.TranscriptPath != "" {
		path, err := validateTranscriptPath(metadata.TranscriptPath)
		if err != nil || metadata.Program != ProgramClaude {
			return Metadata{}, fmt.Errorf("invalid recovery transcript path")
		}
		metadata.TranscriptPath = path
	}
	if metadata.Model != "" {
		model, err := validateModel(metadata.Model)
		if err != nil || metadata.Program != ProgramClaude {
			return Metadata{}, fmt.Errorf("invalid recovery model")
		}
		metadata.Model = model
	}
	return metadata, nil
}

func validateModel(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "", nil
	}
	if len(value) < 2 || len(value) > 96 {
		return "", fmt.Errorf("invalid model")
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || strings.ContainsRune("._-/", r) {
			continue
		}
		return "", fmt.Errorf("invalid model")
	}
	return value, nil
}

func validSource(source string, allowEmpty bool) bool {
	if source == "" {
		return allowEmpty
	}
	switch source {
	case "startup", "resume", "clear", "compact", "fork", "manual":
		return true
	default:
		return false
	}
}

func validateCWD(cwd string) error {
	if len(cwd) > 4096 || strings.ContainsRune(cwd, '\x00') {
		return fmt.Errorf("invalid working directory")
	}
	return nil
}

func validateTranscriptPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" || len(path) > 4096 || strings.ContainsRune(path, '\x00') || !filepath.IsAbs(path) {
		return "", fmt.Errorf("invalid Claude transcript path")
	}
	cleaned := filepath.Clean(path)
	if filepath.Ext(cleaned) != ".jsonl" {
		return "", fmt.Errorf("invalid Claude transcript path")
	}
	return cleaned, nil
}

func ValidProgram(program string) bool {
	program = strings.ToLower(strings.TrimSpace(program))
	return program == ProgramCodex || program == ProgramClaude
}

func ValidSessionID(id string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		return false
	}
	decoded, err := hex.DecodeString(strings.ReplaceAll(id, "-", ""))
	return err == nil && len(decoded) == 16
}

func ResumeCommand(program, sessionID string) string {
	if strings.EqualFold(program, ProgramClaude) {
		return "claude --resume " + strings.ToLower(strings.TrimSpace(sessionID))
	}
	return "codex resume " + strings.ToLower(strings.TrimSpace(sessionID))
}
