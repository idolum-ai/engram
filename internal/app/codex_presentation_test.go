package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/idolum-ai/engram/internal/codexui"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/tmux"
)

type fixedCodexDetector struct {
	runtime codexui.Runtime
	err     error
	pid     int
	command string
}

func (d *fixedCodexDetector) Detect(_ context.Context, pid int, command string) (codexui.Runtime, error) {
	d.pid = pid
	d.command = command
	return d.runtime, d.err
}

func TestProcessCapturedFrameCleansCodexGuideInputAndRecordsCardState(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	detector := &fixedCodexDetector{runtime: codexui.Runtime{Detected: true, Supported: true, Version: codexui.SupportedVersion}}
	app.CodexDetector = detector
	session, _ := app.Store.FindSession(id)
	input := strings.Join([]string{
		"• Ran go test ./...",
		"  └ ok example/internal/app",
		"",
		"⚠ Automatic approval review approved: https://example.test/audit",
		"",
		"────────────────────────────────────────",
		"",
		"• Working (12s • esc to interrupt)",
		"",
		"› Write tests for @filename",
		"",
		"gpt-5.6-sol high fast · /work · Main [default]",
	}, "\n")
	got := app.processCapturedFrame(context.Background(), session, tmux.StyledCapture{
		JoinedText: input, PanePID: 4242, CurrentCmd: "node",
	})
	if detector.pid != 4242 || detector.command != "node" {
		t.Fatalf("detector input pid=%d command=%q", detector.pid, detector.command)
	}
	if !strings.Contains(got, "Ran go test") || strings.Contains(got, "Working (") || strings.Contains(got, "Write tests") || strings.Contains(got, "gpt-5.6-sol") {
		t.Fatalf("guide input = %q", got)
	}
	refs := app.visibleReferencesForStyledCapture(observeUpstreamSignal(tmux.StyledCapture{JoinedText: input}).PresentationText, nil)
	if len(refs.URLs) != 1 || refs.URLs[0] != "https://example.test/audit" || strings.Contains(got, refs.URLs[0]) {
		t.Fatalf("reference boundary refs=%#v guide=%q", refs, got)
	}
	current, ok := app.Store.FindSession(id)
	if !ok || current.PresentationProgram != "codex" || current.PresentationVersion != codexui.SupportedVersion || current.PresentationModel != "gpt-5.6-sol" || current.PresentationEffort != "high" || current.PresentationMode != "fast" || current.PresentationActivity != "working" {
		t.Fatalf("session presentation = %#v ok=%v", current, ok)
	}
	card := app.renderLocal(current, "Tests are passing.")
	if !strings.Contains(card, "Codex · gpt-5.6-sol · high · fast · working\n\nTests are passing.") {
		t.Fatalf("card = %q", card)
	}
}

func TestProcessCapturedFrameFallsBackAndClearsStaleCodexState(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	if _, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.PresentationProgram = "codex"
		session.PresentationVersion = codexui.SupportedVersion
		session.PresentationModel = "gpt-5.6-sol"
		session.PresentationEffort = "high"
		session.PresentationMode = "fast"
		session.PresentationActivity = "working"
	}); err != nil {
		t.Fatal(err)
	}
	app.CodexDetector = &fixedCodexDetector{runtime: codexui.Runtime{Detected: true, Version: "0.145.0"}}
	session, _ := app.Store.FindSession(id)
	input := "answer\ngpt-5.7 low · /work"
	if got := app.processCapturedFrame(context.Background(), session, tmux.StyledCapture{JoinedText: input, PanePID: 4242, CurrentCmd: "node"}); got != input {
		t.Fatalf("fallback changed input: %q", got)
	}
	current, _ := app.Store.FindSession(id)
	if current.PresentationProgram != "" || current.PresentationModel != "" || current.PresentationMode != "" || strings.Contains(app.renderLocal(current, "answer"), "Codex ·") {
		t.Fatalf("stale presentation survived fallback: %#v", current)
	}
}

func TestProcessCapturedFrameKeepsLastCardStateOnTransientDetectionFailure(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	if _, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.PresentationProgram = "codex"
		session.PresentationVersion = codexui.SupportedVersion
		session.PresentationModel = "gpt-5.6-sol"
		session.PresentationEffort = "high"
		session.PresentationActivity = "idle"
	}); err != nil {
		t.Fatal(err)
	}
	app.CodexDetector = &fixedCodexDetector{err: errors.New("ps unavailable")}
	session, _ := app.Store.FindSession(id)
	input := "answer\ngpt-5.6-sol high · /work"
	if got := app.processCapturedFrame(context.Background(), session, tmux.StyledCapture{JoinedText: input, PanePID: 4242, CurrentCmd: "node"}); got != input {
		t.Fatalf("transient fallback changed input: %q", got)
	}
	current, _ := app.Store.FindSession(id)
	if current.PresentationProgram != "codex" || current.PresentationActivity != "idle" {
		t.Fatalf("transient failure cleared card state: %#v", current)
	}
}

func TestCodexPresentationAppearsOnTextGuideAndSnapshotCards(t *testing.T) {
	app := &App{}
	session := state.TerminalSession{
		ID: 4, State: state.TerminalRunning, Title: "review", LastKnownCWD: "/work",
		PresentationProgram: "codex", PresentationVersion: codexui.SupportedVersion,
		PresentationModel: "gpt-5.6-sol", PresentationEffort: "high", PresentationMode: "fast", PresentationActivity: "reviewing approval",
		PresentationNotice: "⚠ Switch to the fast model for additional security review.",
	}
	want := "Codex · gpt-5.6-sol · high · fast · reviewing approval\nnotice: ⚠ Switch to the fast model for additional security review."
	textCard := renderLocal(session, "A command is awaiting review.")
	guideCard, _ := app.guidedEvidenceCaption(session, "A command is awaiting review.", visibleReferences{})
	snapshotCard, _ := app.snapshotAnchorCaption(session, tmux.StyledCapture{Columns: 71, VisibleRows: 37, BufferRows: 64}, visibleReferences{})
	for name, card := range map[string]string{"text": textCard, "guide evidence": guideCard, "snapshot": snapshotCard} {
		if !strings.Contains(card, want) {
			t.Errorf("%s card omitted Codex state: %q", name, card)
		}
	}
}
