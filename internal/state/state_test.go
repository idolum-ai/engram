package state

import (
	"path/filepath"
	"testing"
)

func TestStorePersistsSession(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	audit := filepath.Join(dir, "audit.jsonl")
	store, err := Open(path, audit)
	if err != nil {
		t.Fatal(err)
	}
	ts, err := store.AllocateSession(1, 2, "engram-1", "@1", "%1", "title")
	if err != nil {
		t.Fatal(err)
	}
	if ts.ID != 1 {
		t.Fatalf("session id = %d", ts.ID)
	}
	if _, _, err := store.UpdateSession(1, func(s *TerminalSession) { s.LastSummary = "ok" }); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, audit)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reopened.FindSession(1)
	if !ok || got.LastSummary != "ok" {
		t.Fatalf("reopened session = %#v ok=%v", got, ok)
	}
}
