package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/idolum-ai/engram/internal/codexcontext"
	"github.com/idolum-ai/engram/internal/guide"
	"github.com/idolum-ai/engram/internal/terminalshot"
)

// This ordinary, hermetic contract test composes the parser, provider prompt,
// diagram detector, and renderer without reading a real Codex home.
func TestSyntheticCodexContextContract(t *testing.T) {
	const sessionID = "123e4567-e89b-12d3-a456-426614174000"
	root := t.TempDir()
	dir := filepath.Join(root, "2026", "08", "03")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	rollout := strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"` + sessionID + `"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Explain the synthetic queue."}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"┌────────┐   ┌────────┐\n│ queued │ → │ active │\n└────────┘   └────────┘"}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "rollout-synthetic-"+sessionID+".jsonl"), []byte(rollout), 0o600); err != nil {
		t.Fatal(err)
	}

	context, err := (codexcontext.Reader{SessionsRoot: root}).Load(sessionID, 4)
	if err != nil {
		t.Fatal(err)
	}
	historical := codexcontext.PromptText(context.Messages)
	prompt := guide.BuildPrompt(guide.Input{SessionID: 7, VisibleText: "queue worker is running", HistoricalContext: historical})
	if !strings.Contains(prompt, "historical_session_context") || !strings.Contains(prompt, "Explain the synthetic queue") {
		t.Fatalf("guide prompt omitted historical context: %s", prompt)
	}
	diagram, ok := codexcontext.DetectDiagram(context.Messages)
	if !ok {
		t.Fatal("synthetic diagram was not detected")
	}
	ordinary := terminalshot.RenderHTML(terminalshot.Input{ANSI: "queue worker is running", Columns: 80, VisibleRows: 24, BufferRows: 1, Compact: true}, "contrast-dark")
	if strings.Contains(ordinary, diagram.Text) || strings.Contains(ordinary, "Codex context") {
		t.Fatal("ordinary literal render included transcript context")
	}
	contextCard := terminalshot.RenderHTML(terminalshot.Input{
		ANSI: "queue worker is running", Columns: 80, VisibleRows: 24, BufferRows: 1, Compact: true,
		ContextInset: diagram.Text, ContextLabel: "Codex context · prior visible message, not current terminal",
	}, "contrast-dark")
	if !strings.Contains(contextCard, diagram.Text) || !strings.Contains(contextCard, "not current terminal") {
		t.Fatal("guide context inset lost text or provenance")
	}
}
