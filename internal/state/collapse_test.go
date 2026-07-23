package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCollapsedPresentationPersistsAndLegacyStateDefaultsExpanded(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	audit := filepath.Join(dir, "audit.jsonl")
	store, err := Open(path, audit)
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.AllocateSession("main", "@1", "%1", "build")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.UpdateSession(session.ID, func(current *TerminalSession) { current.Collapsed = true }); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, audit)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reopened.FindSession(session.ID)
	if !ok || !got.Collapsed {
		t.Fatalf("reopened session = %#v, ok=%v", got, ok)
	}
	legacyPath := filepath.Join(dir, "legacy.json")
	if err := os.WriteFile(legacyPath, []byte(`{"version":15,"next_session_id":2,"terminal_sessions":[{"id":1,"state":"running","watch_enabled":true}],"attachments":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	legacy, err := Open(legacyPath, filepath.Join(dir, "legacy-audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	legacySession, ok := legacy.FindSession(1)
	if !ok || legacy.Snapshot().Version != currentStateVersion || legacySession.Collapsed {
		t.Fatalf("legacy session = %#v, ok=%v version=%d", legacySession, ok, legacy.Snapshot().Version)
	}
}
