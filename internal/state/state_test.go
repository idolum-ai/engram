package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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
	byPane, ok := reopened.FindByPane("%1")
	if !ok || byPane.ID != 1 {
		t.Fatalf("FindByPane = %#v ok=%v", byPane, ok)
	}
}

func TestStoreRecordsUpdateJournal(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	refs := UpdateRefs{ChatID: 10, UserID: 20, MessageID: 30}
	if err := store.MarkPoll(42, "message", refs); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordUpdate(42, "message", "handled_command", "", refs); err != nil {
		t.Fatal(err)
	}
	st := store.Snapshot()
	if st.LastUpdateID != 42 || len(st.UpdateJournal) != 2 {
		t.Fatalf("state = %#v", st)
	}
	if st.UpdateJournal[0].Status != "accepted" || st.UpdateJournal[1].Status != "handled_command" {
		t.Fatalf("journal = %#v", st.UpdateJournal)
	}
}

func TestStoreRecoversCorruptStateWithBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	audit := filepath.Join(dir, "audit.jsonl")
	if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path, audit)
	if err != nil {
		t.Fatal(err)
	}
	if store.Snapshot().NextSessionID != 1 {
		t.Fatalf("replacement state = %#v", store.Snapshot())
	}
	matches, err := filepath.Glob(path + ".corrupt-*")
	if err != nil || len(matches) != 1 {
		t.Fatalf("corrupt backups = %#v err=%v", matches, err)
	}
}

func TestAttachmentBypassExpiresAndConsumes(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	bypass := AttachmentBypass{
		ChatID:    1,
		UserID:    2,
		SHA256:    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		CreatedAt: time.Now().UTC(),
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}
	if err := store.AddAttachmentBypass(bypass); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.FindAttachmentBypass(1, 2); !ok {
		t.Fatal("FindAttachmentBypass did not find active bypass")
	}
	if err := store.ConsumeAttachmentBypass(1, 2, bypass.SHA256); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.FindAttachmentBypass(1, 2); ok {
		t.Fatal("FindAttachmentBypass found consumed bypass")
	}
}
