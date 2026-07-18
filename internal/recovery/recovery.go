// Package recovery defines the small, provider-neutral recovery metadata
// exchanged between terminal lifecycle hooks, tmux, and Engram state.
package recovery

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	ProgramCodex  = "codex"
	ProgramClaude = "claude"
	maxHookBytes  = 64 << 10
)

type Metadata struct {
	Version   int       `json:"version"`
	Program   string    `json:"program"`
	SessionID string    `json:"session_id"`
	CWD       string    `json:"cwd,omitempty"`
	Source    string    `json:"source,omitempty"`
	Observed  time.Time `json:"observed_at"`
}

type codexHookInput struct {
	SessionID     string `json:"session_id"`
	CWD           string `json:"cwd"`
	HookEventName string `json:"hook_event_name"`
	Source        string `json:"source"`
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
		CWD: strings.TrimSpace(event.CWD), Source: source, Observed: now.UTC(),
	}, nil
}

func Encode(metadata Metadata) (string, error) {
	if !ValidProgram(metadata.Program) || !ValidSessionID(metadata.SessionID) {
		return "", fmt.Errorf("invalid recovery metadata")
	}
	metadata.Version = 1
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
	metadata.Program = strings.ToLower(strings.TrimSpace(metadata.Program))
	metadata.SessionID = strings.ToLower(strings.TrimSpace(metadata.SessionID))
	if metadata.Version != 1 || !ValidProgram(metadata.Program) || !ValidSessionID(metadata.SessionID) {
		return Metadata{}, fmt.Errorf("invalid recovery metadata")
	}
	if len(metadata.CWD) > 4096 || strings.ContainsRune(metadata.CWD, '\x00') {
		return Metadata{}, fmt.Errorf("invalid recovery working directory")
	}
	return metadata, nil
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
