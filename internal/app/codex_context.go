package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/idolum-ai/engram/internal/codexcontext"
	"github.com/idolum-ai/engram/internal/recovery"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/tmux"
)

type codexContextReader interface {
	Load(string, int) (codexcontext.Context, error)
}

type codexContextSnapshot struct {
	prompt      string
	fingerprint string
	diagram     string
}

func (a *App) codexContextForCapture(ctx context.Context, expected state.TerminalSession, capture tmux.StyledCapture) codexContextSnapshot {
	limit := a.Config.CodexContextTurns
	if limit <= 0 || a.CodexContext == nil || a.CodexDetector == nil || capture.PanePID <= 0 {
		return codexContextSnapshot{}
	}
	runtime, err := a.CodexDetector.Detect(ctx, capture.PanePID, capture.CurrentCmd)
	if err != nil || !runtime.Detected || runtime.Identity == "" || runtime.StartedAt.IsZero() {
		a.recordCodexContextDecision(expected.ID, "unavailable", "process_identity_unproven", 0, false)
		return codexContextSnapshot{}
	}
	tmuxCtx, cancel := tmux.TimeoutContext(ctx)
	metadata, metadataErr := a.Tmux.RecoveryMetadata(tmux.BackgroundContext(tmuxCtx), expected.TmuxPaneID, expected.TmuxWindowID, expected.TmuxServerID)
	cancel()
	if metadataErr != nil || metadata.Program != recovery.ProgramCodex || !recovery.ValidSessionID(metadata.SessionID) || metadata.Observed.Before(runtime.StartedAt) {
		a.recordCodexContextDecision(expected.ID, "unavailable", "session_identity_unproven", 0, false)
		return codexContextSnapshot{}
	}
	loaded, err := a.CodexContext.Load(metadata.SessionID, limit)
	if err != nil {
		a.recordCodexContextDecision(expected.ID, "unavailable", "rollout_unavailable", 0, false)
		return codexContextSnapshot{}
	}

	redacted := make([]codexcontext.Message, 0, len(loaded.Messages))
	for _, message := range loaded.Messages {
		text := headUTF8(a.redactText(message.Text), codexcontext.MaxMessageBytes)
		if strings.TrimSpace(text) != "" {
			redacted = append(redacted, codexcontext.Message{Role: message.Role, Text: text})
		}
	}
	prompt := headUTF8(codexcontext.PromptText(redacted), codexcontext.MaxContextBytes)
	diagram := ""
	if candidate, ok := codexcontext.DetectDiagram(loaded.Messages); ok && a.redactText(candidate.Text) == candidate.Text {
		diagram = candidate.Text
	}

	// The file read is provisional until both the tmux binding and the Codex
	// process incarnation are proven unchanged after it completes.
	after, afterErr := a.CodexDetector.Detect(ctx, capture.PanePID, capture.CurrentCmd)
	latest, tracked := a.Store.FindSession(expected.ID)
	if afterErr != nil || after.Identity == "" || after.Identity != runtime.Identity || !tracked || !sameTerminalBinding(latest, expected) || !latest.CreatedAt.Equal(expected.CreatedAt) {
		a.recordCodexContextDecision(expected.ID, "unavailable", "identity_changed", 0, false)
		return codexContextSnapshot{}
	}
	if prompt == "" {
		a.recordCodexContextDecision(expected.ID, "empty", "no_visible_messages", 0, false)
		return codexContextSnapshot{}
	}
	fingerprint := sha(strings.Join([]string{runtime.Identity, metadata.SessionID, loaded.Parser, prompt, diagram}, "\x00"))
	a.recordCodexContextDecision(expected.ID, "applied", loaded.Parser, len(redacted), diagram != "")
	return codexContextSnapshot{prompt: prompt, fingerprint: fingerprint, diagram: diagram}
}

func (a *App) recordCodexContextDecision(sessionID int, outcome, reason string, messages int, diagram bool) {
	signature := fmt.Sprintf("%s\x00%s\x00%d\x00%t", outcome, reason, messages, diagram)
	if previous, loaded := a.codexContextDiagnostics.Swap(sessionID, signature); loaded && previous == signature {
		return
	}
	_ = a.audit("terminal.codex_context", outcome, map[string]any{
		"session_id": sessionID, "reason": reason, "messages": messages, "diagram": diagram,
	})
}
