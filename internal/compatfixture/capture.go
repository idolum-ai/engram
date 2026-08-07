// Package compatfixture creates privacy-reviewed compatibility candidates.
// It never copies live terminal or transcript content verbatim.
package compatfixture

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/idolum-ai/engram/internal/agentcompat"
	"github.com/idolum-ai/engram/internal/agentdoctor"
	"github.com/idolum-ai/engram/internal/codexcontext"
	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/recovery"
	"github.com/idolum-ai/engram/internal/tmux"
)

const maxInventoryBytes = 32 << 20

type Options struct {
	Provider agentcompat.Provider
	PaneID   string
	Output   string
}

type Candidate struct {
	Format       string                    `json:"format"`
	Provider     agentcompat.Provider      `json:"provider"`
	Process      string                    `json:"process_contract"`
	Binding      string                    `json:"binding_contract"`
	Screen       string                    `json:"screen_contract"`
	Transcript   string                    `json:"transcript_contract"`
	Observed     agentcompat.Compatibility `json:"observed_compatibility"`
	Presentation agentcompat.Presentation  `json:"observed_presentation,omitempty"`
	Review       string                    `json:"review"`
}

type Inventory struct {
	Format       string   `json:"format"`
	Records      int      `json:"records"`
	RootKeys     []string `json:"root_keys"`
	RecordTypes  []string `json:"record_types,omitempty"`
	Provenance   []string `json:"provenance_values,omitempty"`
	ContentTypes []string `json:"content_types,omitempty"`
}

func Capture(ctx context.Context, runner tmux.Runner, options Options) error {
	if !agentcompat.ValidProvider(options.Provider) || strings.TrimSpace(options.PaneID) == "" {
		return fmt.Errorf("provider and exact pane are required")
	}
	out, err := validateOutput(options.Output)
	if err != nil {
		return err
	}
	manager := tmux.New(runner)
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	window, err := manager.ResolveTarget(probeCtx, options.PaneID)
	if err != nil || window.PaneID != options.PaneID {
		return fmt.Errorf("exact tmux pane is unavailable")
	}
	serverID, err := manager.CurrentServerID(probeCtx)
	if err != nil {
		return fmt.Errorf("tmux server identity is unavailable")
	}
	metadata, err := manager.RecoveryMetadata(probeCtx, options.PaneID, window.ID, serverID)
	if err != nil || metadata.Program != string(options.Provider) || !recovery.ValidSessionID(metadata.SessionID) {
		return fmt.Errorf("exact provider binding is unavailable")
	}
	frame, err := manager.CaptureStyled(probeCtx, options.PaneID, 96)
	if err != nil {
		return fmt.Errorf("capture bounded frame: %w", err)
	}
	sanitized := SanitizeFrame(frame.JoinedText)
	if err := ValidateSanitized(sanitized); err != nil {
		return fmt.Errorf("sanitization failed closed: %w", err)
	}
	transcriptPath := metadata.TranscriptPath
	if options.Provider == agentcompat.ProviderCodex {
		transcriptPath, err = codexcontext.ExactRolloutPath(codexcontext.DefaultSessionsRoot(), metadata.SessionID)
		if err != nil {
			return fmt.Errorf("exact transcript is unavailable")
		}
	}
	inventory, err := InventoryTranscript(transcriptPath)
	if err != nil {
		return err
	}
	doctor := agentdoctor.New(runner, config.AgentOptions{CodexContextTurns: 1, ClaudeContextTurns: 1}, options.PaneID)
	report, err := doctor.Probe(probeCtx, options.Provider, options.PaneID)
	if err != nil {
		return fmt.Errorf("compatibility report: %w", err)
	}
	candidate := declaration(options.Provider, report.Compatibility, report.Presentation)
	if err := os.Mkdir(out, 0o700); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	if err := os.WriteFile(filepath.Join(out, ".incomplete"), []byte("capture incomplete\n"), 0o600); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(out, "compatibility.json"), candidate); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(out, "transcript-inventory.json"), inventory); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, "frame.sanitized.txt"), []byte(sanitized), 0o600); err != nil {
		return err
	}
	review := "REVIEW REQUIRED BEFORE CHECK-IN\n\nThis directory is a sanitized candidate, not a support declaration. Inspect every file, remove the .incomplete marker, and copy only reviewed material into the repository.\n"
	if err := os.WriteFile(filepath.Join(out, "REVIEW_REQUIRED"), []byte(review), 0o600); err != nil {
		return err
	}
	return os.Remove(filepath.Join(out, ".incomplete"))
}

func validateOutput(path string) (string, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || !filepath.IsAbs(path) {
		return "", fmt.Errorf("output must be an absolute, new directory")
	}
	if _, err := os.Lstat(path); err == nil || !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("output must not already exist")
	}
	for current := filepath.Dir(path); ; current = filepath.Dir(current) {
		if info, err := os.Lstat(filepath.Join(current, ".git")); err == nil && (info.IsDir() || info.Mode().IsRegular()) {
			return "", fmt.Errorf("output must be outside a Git worktree")
		}
		next := filepath.Dir(current)
		if next == current {
			break
		}
	}
	return path, nil
}

var (
	codexHeader  = regexp.MustCompile(`OpenAI Codex \(v([0-9]+\.[0-9]+\.[0-9]+)\)`)
	claudeHeader = regexp.MustCompile(`Claude Code(?:\s+v)?([0-9]+\.[0-9]+\.[0-9]+)`)
	durationLine = regexp.MustCompile(`(?i)\b(Worked|Brewed|Cooked|Crunched) for\s+([0-9]+(?:h|m|s)(?:\s*[0-9]+(?:m|s))*)`)
	modelToken   = regexp.MustCompile(`\b(?:claude-(?:opus|sonnet|haiku|fable)-[0-9][a-z0-9.-]*|gpt-[a-z0-9.-]+|o[0-9][a-z0-9.-]*)\b`)
	borderOnly   = regexp.MustCompile(`^[\s─━═│┃║╭╮╰╯┌┐└┘┏┓┗┛┬┴├┤┼╔╗╚╝]+$`)
	unsafePath   = regexp.MustCompile(`(?:^|\s)(?:/[^\s]+|~[/\\][^\s]+)`)
	unsafeEmail  = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	unsafeUUID   = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
)

func SanitizeFrame(text string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r", ""), "\n")
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			result = append(result, "")
		case borderOnly.MatchString(line):
			result = append(result, line)
		case codexHeader.MatchString(line):
			result = append(result, ">_ OpenAI Codex (v"+codexHeader.FindStringSubmatch(line)[1]+")")
		case claudeHeader.MatchString(line):
			result = append(result, "Claude Code v"+claudeHeader.FindStringSubmatch(line)[1])
		case durationLine.MatchString(line):
			match := durationLine.FindStringSubmatch(line)
			result = append(result, match[1]+" for "+match[2])
		case modelToken.MatchString(strings.ToLower(line)):
			result = append(result, "model: "+modelToken.FindString(strings.ToLower(line)))
		default:
			result = append(result, "<redacted-line>")
		}
	}
	return strings.TrimRight(strings.Join(result, "\n"), "\n") + "\n"
}

func ValidateSanitized(text string) error {
	if !utf8.ValidString(text) || len(text) > 256<<10 {
		return fmt.Errorf("invalid or oversized sanitized frame")
	}
	if unsafePath.MatchString(text) || unsafeEmail.MatchString(text) || unsafeUUID.MatchString(text) {
		return fmt.Errorf("recognized private token remains")
	}
	return nil
}

func InventoryTranscript(path string) (Inventory, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > maxInventoryBytes {
		return Inventory{}, fmt.Errorf("transcript must be a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return Inventory{}, fmt.Errorf("open transcript inventory: %w", err)
	}
	defer file.Close()
	keys, types, provenance, contentTypes := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	allowedKeys := map[string]bool{"type": true, "timestamp": true, "sessionId": true, "uuid": true, "parentUuid": true, "isSidechain": true, "isMeta": true, "userType": true, "provenance": true, "message": true, "payload": true, "content": true, "role": true, "model": true, "cwd": true, "version": true, "gitBranch": true, "requestId": true}
	allowedRecordTypes := tokens("session_meta", "response_item", "event_msg", "turn_context", "user", "assistant", "system", "progress", "queue-operation")
	allowedProvenance := tokens("external", "internal", "human", "hook", "visible_ui", "retained_same_incarnation_ui")
	allowedContentTypes := tokens("text", "input_text", "output_text", "thinking", "redacted_thinking", "tool_use", "tool_result", "image", "document")
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	records := 0
	for scanner.Scan() {
		records++
		if records > 100000 {
			return Inventory{}, fmt.Errorf("transcript exceeds bounded record count")
		}
		var record map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return Inventory{}, fmt.Errorf("transcript contains invalid JSONL")
		}
		for key := range record {
			if allowedKeys[key] {
				keys[key] = true
			} else {
				keys["<unknown-key>"] = true
			}
		}
		collectEnum(record["type"], types, allowedRecordTypes)
		collectEnum(record["userType"], provenance, allowedProvenance)
		collectEnum(record["provenance"], provenance, allowedProvenance)
		collectContentTypes(record["message"], contentTypes, allowedContentTypes)
		collectContentTypes(record["payload"], contentTypes, allowedContentTypes)
	}
	if err := scanner.Err(); err != nil || records == 0 {
		return Inventory{}, fmt.Errorf("read transcript inventory")
	}
	return Inventory{Format: "engram-transcript-inventory-v1", Records: records, RootKeys: sorted(keys), RecordTypes: sorted(types), Provenance: sorted(provenance), ContentTypes: sorted(contentTypes)}, nil
}

func collectEnum(value any, result, allowed map[string]bool) {
	if token, ok := value.(string); ok {
		if allowed[token] {
			result[token] = true
		} else {
			result["<unknown-value>"] = true
		}
	}
}

func collectContentTypes(value any, result, allowed map[string]bool) {
	object, ok := value.(map[string]any)
	if !ok {
		return
	}
	content, ok := object["content"].([]any)
	if !ok {
		return
	}
	for _, item := range content {
		if block, ok := item.(map[string]any); ok {
			collectEnum(block["type"], result, allowed)
		}
	}
}

func tokens(values ...string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		result[value] = true
	}
	return result
}

func sorted(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func declaration(provider agentcompat.Provider, observed agentcompat.Compatibility, presentation agentcompat.Presentation) Candidate {
	result := Candidate{Format: "engram-compatibility-candidate-v1", Provider: provider, Observed: agentcompat.NormalizeCompatibility(observed), Presentation: safePresentation(presentation), Review: "required_before_check_in"}
	if provider == agentcompat.ProviderClaude {
		result.Process, result.Binding, result.Screen, result.Transcript = agentcompat.ClaudeProcessContract, agentcompat.ClaudeBindingContract, agentcompat.ClaudeScreenContract, agentcompat.ClaudeTranscriptContract
	} else {
		result.Process, result.Binding, result.Screen, result.Transcript = agentcompat.CodexProcessContract, agentcompat.CodexBindingContract, agentcompat.CodexScreenContract, agentcompat.CodexTranscriptContract
	}
	return result
}

func safePresentation(value agentcompat.Presentation) agentcompat.Presentation {
	value = agentcompat.NormalizePresentation(value)
	model := strings.ToLower(value.Model.Value)
	if model == "" || modelToken.FindString(model) != model {
		value.Model = agentcompat.Value{}
	}
	return value
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}
