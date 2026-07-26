package state

import (
	"path/filepath"
	"strings"
	"testing"
)

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
