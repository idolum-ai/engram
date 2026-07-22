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

func TestProcessCapturedFrameUsesGenericSemanticsBeforeVersionedCodexFallback(t *testing.T) {
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
	if detector.pid != 0 || detector.command != "" {
		t.Fatalf("generic analysis unnecessarily invoked versioned detector: pid=%d command=%q", detector.pid, detector.command)
	}
	if !strings.Contains(got, "Ran go test") || strings.Contains(got, "Working (") || strings.Contains(got, "Write tests") || strings.Contains(got, "gpt-5.6-sol") {
		t.Fatalf("guide input = %q", got)
	}
	refs := app.visibleReferencesForStyledCapture(observeUpstreamSignal(tmux.StyledCapture{JoinedText: input}).PresentationText, nil)
	if len(refs.URLs) != 1 || refs.URLs[0] != "https://example.test/audit" || strings.Contains(got, refs.URLs[0]) {
		t.Fatalf("reference boundary refs=%#v guide=%q", refs, got)
	}
	current, ok := app.Store.FindSession(id)
	if !ok || current.PresentationProgram != "agent" || current.PresentationVersion != "" || current.PresentationModel != "gpt-5.6-sol" || current.PresentationEffort != "high" || current.PresentationMode != "fast" || current.PresentationActivity != "active" {
		t.Fatalf("session presentation = %#v ok=%v", current, ok)
	}
	card := app.renderLocal(current, "Tests are passing.")
	if !strings.Contains(card, "Agent · gpt-5.6-sol · high · fast · active\n\nTests are passing.") {
		t.Fatalf("card = %q", card)
	}
}

func TestProcessCapturedFrameGenericAnalysisSupportsNonCodexAgentUI(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	detector := &fixedCodexDetector{err: errors.New("must not be called")}
	app.CodexDetector = detector
	session, _ := app.Store.FindSession(id)
	input := "• The refactor is complete.\n\n❯\n\nclaude-sonnet-4-6 · ~/work · main"
	got := app.processCapturedFrame(context.Background(), session, tmux.StyledCapture{
		JoinedText: input, PanePID: 4242, CurrentCmd: "claude", AlternateOn: "on",
	})
	if strings.Contains(got, "claude-sonnet") || strings.Contains(got, "❯") || !strings.Contains(got, "refactor is complete") {
		t.Fatalf("generic guide input = %q", got)
	}
	if detector.pid != 0 {
		t.Fatalf("generic analysis invoked Codex detector with pid %d", detector.pid)
	}
	current, ok := app.Store.FindSession(id)
	if !ok || current.PresentationProgram != "agent" || current.PresentationModel != "claude-sonnet-4-6" || current.PresentationActivity != "idle" {
		t.Fatalf("generic presentation state = %#v ok=%v", current, ok)
	}
	if got := app.renderLocal(current, "Done."); !strings.Contains(got, "Agent · claude-sonnet-4-6 · idle") {
		t.Fatalf("generic card = %q", got)
	}
}

func TestProcessCapturedFrameBoundsTemporalSemanticsToTerminalIdentity(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	app.CodexDetector = nil
	session, _ := app.Store.FindSession(id)
	frame := func(seconds, pane string) tmux.StyledCapture {
		return tmux.StyledCapture{
			JoinedText: "› analyze the fixture\n\n• Starting analysis\n\nIndexing files (" + seconds + "s)\n\ngpt-5.6-sol high · /work",
			ServerID:   session.TmuxServerID, WindowID: session.TmuxWindowID, PaneID: pane,
			CurrentCmd: "agent", Columns: 80, VisibleRows: 24, AlternateOn: "on", PaneInMode: "off",
		}
	}
	first := app.processCapturedFrame(context.Background(), session, frame("2", session.TmuxPaneID))
	if !strings.Contains(first, "Indexing files") {
		t.Fatalf("first frame used nonexistent temporal evidence: %q", first)
	}
	second := app.processCapturedFrame(context.Background(), session, frame("3", session.TmuxPaneID))
	if strings.Contains(second, "Indexing files") {
		t.Fatalf("aligned changing status was not classified as activity: %q", second)
	}
	current, _ := app.Store.FindSession(id)
	if current.PresentationActivity != "active" {
		t.Fatalf("aligned activity = %q", current.PresentationActivity)
	}
	moved := app.processCapturedFrame(context.Background(), session, frame("4", "%different"))
	if !strings.Contains(moved, "Indexing files") {
		t.Fatalf("identity change reused stale temporal evidence: %q", moved)
	}
	current, _ = app.Store.FindSession(id)
	if current.PresentationActivity != "idle" {
		t.Fatalf("activity after identity change = %q", current.PresentationActivity)
	}
	app.agentFrameMu.Lock()
	defer app.agentFrameMu.Unlock()
	if len(app.agentFrames) != 1 {
		t.Fatalf("agent frame cache contains %d entries, want one per session", len(app.agentFrames))
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
