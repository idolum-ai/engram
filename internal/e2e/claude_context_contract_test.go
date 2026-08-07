package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/idolum-ai/engram/internal/claudecontext"
	"github.com/idolum-ai/engram/internal/codexcontext"
	"github.com/idolum-ai/engram/internal/guide"
	"github.com/idolum-ai/engram/internal/terminalshot"
)

// This ordinary hermetic contract composes Claude's versioned visible-message
// parser with the provider prompt, shared diagram detector, and provenance
// renderer. It never reads a real Claude configuration directory.
func TestSyntheticClaudeContextContract(t *testing.T) {
	if !strings.Contains(guide.SystemPrompt, "exactly identified active agent session") || strings.Contains(guide.SystemPrompt, "active Codex session") {
		t.Fatal("Claude history provenance is not provider-neutral")
	}
	const sessionID = "123e4567-e89b-12d3-a456-426614174001"
	path := filepath.Join(t.TempDir(), sessionID+".jsonl")
	transcript := strings.Join([]string{
		`{"type":"mode","sessionId":"` + sessionID + `","mode":"default"}`,
		`{"type":"user","sessionId":"` + sessionID + `","uuid":"user-1","message":{"role":"user","content":"Explain the synthetic queue."}}`,
		`{"type":"assistant","sessionId":"` + sessionID + `","uuid":"assistant-1","message":{"id":"message-1","role":"assistant","content":[{"type":"thinking","thinking":"not visible"},{"type":"tool_use","name":"Bash","input":{"command":"private"}},{"type":"text","text":"┌────────┐   ┌────────┐\n│ queued │ → │ active │\n└────────┘   └────────┘"}]}}`,
		`{"type":"user","sessionId":"` + sessionID + `","uuid":"tool-1","message":{"role":"user","content":[{"type":"tool_result","content":"private result"}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}

	context, err := (claudecontext.Reader{}).Load(path, sessionID, 4)
	if err != nil {
		t.Fatal(err)
	}
	historical := codexcontext.PromptText(context.Messages)
	if strings.Contains(historical, "not visible") || strings.Contains(historical, "private") {
		t.Fatalf("Claude context admitted hidden content: %s", historical)
	}
	prompt := guide.BuildPrompt(guide.Input{SessionID: 8, VisibleText: "queue worker is running", HistoricalContext: historical})
	if !strings.Contains(prompt, "historical_session_context") || !strings.Contains(prompt, "Explain the synthetic queue") {
		t.Fatalf("guide prompt omitted historical context: %s", prompt)
	}
	diagram, ok := codexcontext.DetectDiagram(context.Messages)
	if !ok {
		t.Fatal("synthetic Claude diagram was not detected")
	}
	ordinary := terminalshot.RenderHTML(terminalshot.Input{ANSI: "queue worker is running", Columns: 80, VisibleRows: 24, BufferRows: 1, Compact: true}, "contrast-dark")
	if strings.Contains(ordinary, diagram.Text) || strings.Contains(ordinary, "Claude context") {
		t.Fatal("ordinary literal render included Claude transcript context")
	}
	contextCard := terminalshot.RenderHTML(terminalshot.Input{
		ANSI: "queue worker is running", Columns: 80, VisibleRows: 24, BufferRows: 1, Compact: true,
		ContextInset: diagram.Text, ContextLabel: "Claude context · prior visible message, not current terminal",
	}, "contrast-dark")
	if !strings.Contains(contextCard, diagram.Text) || !strings.Contains(contextCard, "not current terminal") {
		t.Fatal("Claude guide context inset lost text or provenance")
	}
}
