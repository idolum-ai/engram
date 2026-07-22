package state

import (
	"path/filepath"
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
	if !ok || reopened.Snapshot().Version != currentStateVersion || got.PresentationProgram != "codex" || got.PresentationVersion != "0.144.6" || got.PresentationModel != "gpt-5.6-sol" || got.PresentationEffort != "high" || got.PresentationActivity != "working" || got.PresentationNotice != "model switch available" {
		t.Fatalf("reopened presentation = %#v ok=%v version=%d", got, ok, reopened.Snapshot().Version)
	}
}
