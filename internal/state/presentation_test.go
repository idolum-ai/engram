package state

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/agentcompat"
)

func TestStructuredAgentStateAndDetailIdentitySurviveRunningServiceRestart(t *testing.T) {
	dir := t.TempDir()
	statePath, auditPath := filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl")
	store, err := Open(statePath, auditPath)
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.AllocateSession("main", "@1", "%1", "work")
	if err != nil {
		t.Fatal(err)
	}
	identity := strings.Repeat("a", 64)
	if _, _, err := store.UpdateSession(session.ID, func(current *TerminalSession) {
		current.AnchorChatID, current.AnchorMessageID = 100, 77
		current.AgentCompatibility = agentcompat.Compatibility{Provider: agentcompat.ProviderClaude, Process: agentcompat.Axis{State: agentcompat.StateProven, Contract: agentcompat.ClaudeProcessContract}}
		current.AgentPresentation = agentcompat.Presentation{Model: agentcompat.Value{Value: "claude-opus-4-8", Provenance: agentcompat.ProvenanceRetainedUI}, LastTurnSeconds: 42, ObservedAt: time.Now().UTC()}
		current.SemanticViewport = agentcompat.Viewport{Applied: true, Contract: agentcompat.ClaudeScreenContract, RuntimeIdentity: identity, TmuxIdentity: strings.Repeat("c", 64), Boundary: "full_capture", AlternateScreen: "on", CopyMode: "off"}
		current.AgentDetailChatID, current.AgentDetailMessageID, current.AgentDetailAnchorMessageID, current.AgentDetailRenderHash = 100, 88, 77, strings.Repeat("b", 64)
	}); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(statePath, auditPath)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reopened.FindSession(session.ID)
	if !ok || got.AgentCompatibility.Provider != agentcompat.ProviderClaude || got.AgentPresentation.Model.Provenance != agentcompat.ProvenanceRetainedUI || got.SemanticViewport.RuntimeIdentity != identity || got.AgentDetailMessageID != 88 || got.AgentDetailAnchorMessageID != 77 {
		t.Fatalf("reopened structured state = %#v ok=%v", got, ok)
	}
}

func TestCodexPresentationStateSurvivesRestartWithoutSchemaBump(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	auditPath := filepath.Join(dir, "audit.jsonl")
	store, err := Open(statePath, auditPath)
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.AllocateSession("main", "@1", "%1", "work")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.UpdateSession(session.ID, func(current *TerminalSession) {
		current.PresentationProgram = "codex"
		current.PresentationVersion = "0.144.6"
		current.PresentationModel = "gpt-5.6-sol"
		current.PresentationEffort = "high"
		current.PresentationMode = "fast"
		current.PresentationActivity = "working"
		current.PresentationNotice = "model switch available"
	}); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(statePath, auditPath)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reopened.FindSession(session.ID)
	if !ok || reopened.Snapshot().Version != currentStateVersion || got.PresentationProgram != "codex" || got.PresentationVersion != "0.144.6" || got.PresentationModel != "gpt-5.6-sol" || got.PresentationEffort != "high" || got.PresentationMode != "fast" || got.PresentationActivity != "working" || got.PresentationNotice != "model switch available" {
		t.Fatalf("reopened presentation = %#v ok=%v version=%d", got, ok, reopened.Snapshot().Version)
	}
}

func TestGenericAgentPresentationStateSurvivesRestartWithoutSchemaBump(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	auditPath := filepath.Join(dir, "audit.jsonl")
	store, err := Open(path, auditPath)
	if err != nil {
		t.Fatal(err)
	}
	current, err := store.AllocateSession("main", "@1", "%1", "work")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.UpdateSession(current.ID, func(session *TerminalSession) {
		session.PresentationProgram = "agent"
		session.PresentationModel = "claude-sonnet-4-6"
		session.PresentationActivity = "active"
	}); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, auditPath)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reopened.FindSession(current.ID)
	if !ok || got.PresentationProgram != "agent" || got.PresentationModel != "claude-sonnet-4-6" || got.PresentationActivity != "active" {
		t.Fatalf("reopened generic presentation = %#v, ok=%v", got, ok)
	}
}

func TestClaudePresentationStateKeepsOnlyBoundedProcessIdentity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	auditPath := filepath.Join(dir, "audit.jsonl")
	store, err := Open(path, auditPath)
	if err != nil {
		t.Fatal(err)
	}
	current, err := store.AllocateSession("main", "@1", "%1", "work")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.UpdateSession(current.ID, func(session *TerminalSession) {
		session.PresentationProgram = "claude"
		session.PresentationVersion = "2.1.219"
		session.PresentationRuntimeID = strings.Repeat("a", 80)
		session.PresentationModel = "claude-opus-4-8"
		session.PresentationEffort = "high"
		session.PresentationActivity = "active"
	}); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, auditPath)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reopened.FindSession(current.ID)
	if !ok || got.PresentationProgram != "claude" || got.PresentationVersion != "2.1.219" ||
		got.PresentationRuntimeID != strings.Repeat("a", 64) || got.PresentationModel != "claude-opus-4-8" ||
		got.PresentationEffort != "high" || got.PresentationActivity != "active" {
		t.Fatalf("reopened Claude presentation = %#v, ok=%v", got, ok)
	}
}
