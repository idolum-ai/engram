package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	backup, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != "{not-json" {
		t.Fatalf("corrupt backup = %q", backup)
	}
	if _, err := Open(path, audit); err != nil {
		t.Fatalf("reopen replacement: %v", err)
	}
}

func TestStoreLoadsLegacyStateAndOmitsRawCaptureOnSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	audit := filepath.Join(dir, "audit.jsonl")
	legacy := `{
  "version": 0,
  "next_session_id": 0,
  "terminal_sessions": [{
    "id": 7,
    "state": "running",
    "last_raw_capture": "sensitive terminal output",
    "last_raw_capture_hash": "capture-hash"
  }],
  "attachments": [],
  "unknown_future_field": true
}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path, audit)
	if err != nil {
		t.Fatal(err)
	}
	st := store.Snapshot()
	if st.Version != 1 || st.NextSessionID != 8 || st.ProcessedMessages == nil {
		t.Fatalf("normalized legacy state = %#v", st)
	}
	if got := st.TerminalSessions[0].LastRawCapture; got != "sensitive terminal output" {
		t.Fatalf("legacy raw capture = %q", got)
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	persisted, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(persisted), "sensitive terminal output") || strings.Contains(string(persisted), "last_raw_capture\"") {
		t.Fatalf("raw capture persisted: %s", persisted)
	}
	if !strings.Contains(string(persisted), "capture-hash") {
		t.Fatalf("raw capture hash missing: %s", persisted)
	}

	reopened, err := Open(path, audit)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reopened.FindSession(7)
	if !ok || got.LastRawCapture != "" || got.LastRawCaptureHash != "capture-hash" {
		t.Fatalf("reopened legacy session = %#v ok=%v", got, ok)
	}
}

func TestStorePrunesProcessedMessagesByNewestMessageID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	audit := filepath.Join(dir, "audit.jsonl")
	processed := make(map[string]bool, maxProcessedMessages+3)
	for id := 1; id <= maxProcessedMessages+2; id++ {
		processed[fmt.Sprintf("-100:%d", id)] = true
	}
	processed["ignored:false"] = false
	writeStateFixture(t, path, State{Version: 1, NextSessionID: 1, ProcessedMessages: processed})

	store, err := Open(path, audit)
	if err != nil {
		t.Fatal(err)
	}
	st := store.Snapshot()
	if len(st.ProcessedMessages) != maxProcessedMessages {
		t.Fatalf("processed messages = %d, want %d", len(st.ProcessedMessages), maxProcessedMessages)
	}
	if st.ProcessedMessages["-100:1"] || st.ProcessedMessages["-100:2"] || st.ProcessedMessages["ignored:false"] {
		t.Fatalf("old or false entries retained: %#v", st.ProcessedMessages)
	}
	latest := fmt.Sprintf("-100:%d", maxProcessedMessages+2)
	if !st.ProcessedMessages[latest] {
		t.Fatalf("latest entry %q was pruned", latest)
	}

	newest := fmt.Sprintf("-100:%d", maxProcessedMessages+3)
	if err := store.MarkMessage(newest); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, audit)
	if err != nil {
		t.Fatal(err)
	}
	st = reopened.Snapshot()
	if len(st.ProcessedMessages) != maxProcessedMessages || !st.ProcessedMessages[newest] || st.ProcessedMessages["-100:3"] {
		t.Fatalf("processed messages after append = %#v", st.ProcessedMessages)
	}
}

func TestStorePrunesStateMetadataPreservingUsefulNewestEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	now := time.Now().UTC().Truncate(time.Second)
	st := State{Version: 1, NextSessionID: maxTerminalSessions + 4, ProcessedMessages: map[string]bool{}}
	st.TerminalSessions = append(st.TerminalSessions, TerminalSession{
		ID: 1, State: TerminalRunning, UpdatedAt: now.Add(-24 * time.Hour),
	})
	for id := 2; id <= maxTerminalSessions+3; id++ {
		st.TerminalSessions = append(st.TerminalSessions, TerminalSession{
			ID: id, State: TerminalClosed, UpdatedAt: now.Add(time.Duration(id) * time.Minute),
		})
	}
	for id := 1; id <= maxAttachments+2; id++ {
		st.Attachments = append(st.Attachments, Attachment{ID: id, ReceivedAt: now.Add(time.Duration(id) * time.Minute)})
	}
	for id := 1; id <= maxAttachmentBypasses+2; id++ {
		st.AttachmentBypasses = append(st.AttachmentBypasses, AttachmentBypass{
			SHA256: fmt.Sprintf("hash-%d", id), CreatedAt: now.Add(time.Duration(id) * time.Minute), ExpiresAt: now.Add(time.Hour),
		})
	}
	st.AttachmentBypasses = append(st.AttachmentBypasses, AttachmentBypass{
		SHA256: "expired", CreatedAt: now, ExpiresAt: now.Add(-time.Minute),
	})
	for id := 1; id <= maxUpdateJournal+2; id++ {
		st.UpdateJournal = append(st.UpdateJournal, UpdateEvent{UpdateID: id, At: now.Add(time.Duration(id) * time.Minute)})
	}
	writeStateFixture(t, path, st)

	store, err := Open(path, filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	got := store.Snapshot()
	if len(got.TerminalSessions) != maxTerminalSessions {
		t.Fatalf("terminal sessions = %d", len(got.TerminalSessions))
	}
	if _, ok := store.FindSession(1); !ok {
		t.Fatal("old active session was pruned")
	}
	if _, ok := store.FindSession(maxTerminalSessions + 3); !ok {
		t.Fatal("newest closed session was pruned")
	}
	if _, ok := store.FindSession(2); ok {
		t.Fatal("oldest closed session was retained")
	}
	if len(got.Attachments) != maxAttachments || got.Attachments[0].ID != 3 || got.Attachments[len(got.Attachments)-1].ID != maxAttachments+2 {
		t.Fatalf("attachments = %#v", got.Attachments)
	}
	if len(got.AttachmentBypasses) != maxAttachmentBypasses || got.AttachmentBypasses[0].SHA256 != "hash-3" || got.AttachmentBypasses[len(got.AttachmentBypasses)-1].SHA256 != fmt.Sprintf("hash-%d", maxAttachmentBypasses+2) {
		t.Fatalf("attachment bypasses = %#v", got.AttachmentBypasses)
	}
	if len(got.UpdateJournal) != maxUpdateJournal || got.UpdateJournal[0].UpdateID != 3 || got.UpdateJournal[len(got.UpdateJournal)-1].UpdateID != maxUpdateJournal+2 {
		t.Fatalf("update journal = %#v", got.UpdateJournal)
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	var persisted State
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted.TerminalSessions) != maxTerminalSessions || len(persisted.Attachments) != maxAttachments || len(persisted.AttachmentBypasses) != maxAttachmentBypasses || len(persisted.UpdateJournal) != maxUpdateJournal {
		t.Fatalf("persisted state is not bounded: %#v", persisted)
	}
}

func TestStoreSaveUsesPrivateModeAndLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	store, err := Open(path, filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("state mode = %o, want 600", got)
	}
	assertNoStateTemps(t, dir)
}

func TestWriteFileAtomicCleansTempWhenRenameFails(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "state.json")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(target, "marker")
	if err := os.WriteFile(marker, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(target, []byte("replacement")); err == nil {
		t.Fatal("writeFileAtomic succeeded with a non-replaceable target")
	}
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "original" {
		t.Fatalf("marker = %q", b)
	}
	assertNoStateTemps(t, dir)
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

func writeStateFixture(t *testing.T, path string, st State) {
	t.Helper()
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertNoStateTemps(t *testing.T, dir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".state.json.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary state files remain: %#v", matches)
	}
}
