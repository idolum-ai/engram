package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestAuditRotationBoundsCurrentAndPreviousFiles(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.jsonl")
	store, err := Open(filepath.Join(dir, "state.json"), auditPath)
	if err != nil {
		t.Fatal(err)
	}
	seedLine := []byte(`{"at":"old","type":"seed","status":"ok","data":null}` + "\n")
	seed := bytes.Repeat(seedLine, int(2*maxAuditFileBytes/int64(len(seedLine)))+1)
	seed = append(seed, []byte(`{"type":"torn"}`)...)
	if err := os.WriteFile(auditPath, seed, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := store.Audit("new.event", "ok", map[string]any{"value": "kept"}); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	previous, err := os.ReadFile(auditPath + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(current)) > maxAuditFileBytes || int64(len(previous)) > maxAuditFileBytes {
		t.Fatalf("audit sizes current=%d previous=%d", len(current), len(previous))
	}
	if !bytes.Contains(current, []byte(`"type":"new.event"`)) || !bytes.Contains(previous, []byte(`"type":"seed"`)) || bytes.Contains(previous, []byte(`"type":"torn"`)) {
		t.Fatalf("rotation content current=%q previous prefix=%q", current, previous[:min(len(previous), 80)])
	}
	if len(previous) > 0 && (previous[0] != '{' || previous[len(previous)-1] != '\n') {
		t.Fatalf("rotated audit is not complete JSONL: prefix=%q suffix=%q", previous[:min(len(previous), 20)], previous[max(0, len(previous)-20):])
	}
	for _, path := range []string{auditPath, auditPath + ".1"} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o, want 600", path, info.Mode().Perm())
		}
	}
}

func TestAuditRejectsSymlinkPath(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.jsonl")
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, auditPath); err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(dir, "state.json"), auditPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Audit("event", "ok", nil); err == nil {
		t.Fatal("Audit followed a symlink path")
	}
	b, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "unchanged" {
		t.Fatalf("symlink target changed: %q", b)
	}
}

func TestAuditOmitsOversizedPayload(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.jsonl")
	store, err := Open(filepath.Join(dir, "state.json"), auditPath)
	if err != nil {
		t.Fatal(err)
	}
	payload := strings.Repeat("sensitive-volume", maxAuditRecordBytes)
	if err := store.Audit("large.event", "failed", payload); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > maxAuditRecordBytes || bytes.Contains(b, []byte(payload[:1024])) || !bytes.Contains(b, []byte("audit payload exceeded record limit")) {
		t.Fatalf("oversized audit record was not bounded: bytes=%d content=%q", len(b), b)
	}
}

func TestAuditRotationKeepsOnlyOnePredecessor(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.jsonl")
	store, err := Open(filepath.Join(dir, "state.json"), auditPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(auditPath+".1", []byte(`{"type":"obsolete"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	line := []byte(`{"at":"recent","type":"recent","status":"ok","data":null}` + "\n")
	seed := bytes.Repeat(line, int(maxAuditFileBytes/int64(len(line))))
	if err := os.WriteFile(auditPath, seed, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Audit("latest", "ok", strings.Repeat("x", 1024)); err != nil {
		t.Fatal(err)
	}
	previous, err := os.ReadFile(auditPath + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(previous, []byte("obsolete")) || !bytes.Contains(previous, []byte(`"type":"recent"`)) {
		t.Fatalf("rotation did not replace predecessor: prefix=%q", previous[:min(len(previous), 80)])
	}
	current, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(current, []byte(`"type":"latest"`)) {
		t.Fatalf("latest audit event missing: %q", current)
	}
}

func TestStorePersistsSession(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	audit := filepath.Join(dir, "audit.jsonl")
	store, err := Open(path, audit)
	if err != nil {
		t.Fatal(err)
	}
	ts, err := store.AllocateSession("engram-1", "@1", "%1", "title")
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
	if _, ok := reopened.FindByBinding("%1", "@1", "wrong-server"); ok {
		t.Fatal("FindByBinding matched a different server incarnation")
	}
}

func TestStorePersistsAnchorMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	audit := filepath.Join(dir, "audit.jsonl")
	store, err := Open(path, audit)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetAnchorMode("guide"); err != nil {
		t.Fatal(err)
	}
	if got := store.Snapshot().AnchorMode; got != "guide" {
		t.Fatalf("anchor mode = %q, want guide", got)
	}
	if err := store.SetAnchorMode("snapshot"); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path, audit)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Snapshot().AnchorMode; got != "snapshot" {
		t.Fatalf("reopened anchor mode = %q, want snapshot", got)
	}
	if err := reopened.SetAnchorMode("unsupported"); err == nil {
		t.Fatal("SetAnchorMode accepted an unsupported mode")
	}
	if got := reopened.Snapshot().AnchorMode; got != "snapshot" {
		t.Fatalf("invalid update changed anchor mode to %q", got)
	}
}

func TestSetAnchorModeRollsBackWhenPersistenceFails(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetAnchorMode("guide"); err != nil {
		t.Fatal(err)
	}
	store.path = filepath.Join(dir, "missing", "state.json")
	if err := store.SetAnchorMode("snapshot"); err == nil {
		t.Fatal("SetAnchorMode succeeded with an unwritable state path")
	}
	if got := store.Snapshot().AnchorMode; got != "guide" {
		t.Fatalf("mode after failed save = %q, want guide", got)
	}
}

func TestUpdateSessionRollsBackWhenReplacementDidNotOccur(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.AllocateSession("main", "@1", "%1", "original")
	if err != nil {
		t.Fatal(err)
	}
	store.path = filepath.Join(dir, "missing", "state.json")
	updated, ok, err := store.UpdateSession(session.ID, func(s *TerminalSession) {
		s.Title = "must roll back"
		s.UpstreamMessageID = 88
	})
	if err == nil || !ok || updated.Title != "original" || updated.UpstreamMessageID != 0 {
		t.Fatalf("failed update = %#v ok=%v err=%v", updated, ok, err)
	}
	got, _ := store.FindSession(session.ID)
	if got.Title != "original" || got.UpstreamMessageID != 0 {
		t.Fatalf("in-memory state did not roll back: %#v", got)
	}
}

func TestPersistenceReachedReplacementDistinguishesAtomicWriteOutcomes(t *testing.T) {
	if PersistenceReachedReplacement(&atomicWriteError{Err: errors.New("directory sync failed")}) {
		t.Fatal("pre-replacement write error reported a committed replacement")
	}
	if !PersistenceReachedReplacement(&atomicWriteError{Err: errors.New("directory sync failed"), Replaced: true}) {
		t.Fatal("post-replacement sync error did not report a committed replacement")
	}
}

func TestAddAttachmentRollsBackBeforeReplacementAndKeepsCommittedState(t *testing.T) {
	store := &Store{state: newState()}
	attachment := Attachment{StoredPath: "/tmp/voice.ogg"}
	preReplace := &atomicWriteError{Err: errors.New("write failed")}
	if err := store.addAttachmentLocked(attachment, func() error { return preReplace }); !errors.Is(err, preReplace) {
		t.Fatalf("pre-replacement error = %v", err)
	}
	if len(store.state.Attachments) != 0 {
		t.Fatalf("pre-replacement failure retained attachment: %#v", store.state.Attachments)
	}

	postReplace := &atomicWriteError{Err: errors.New("directory sync failed"), Replaced: true}
	if err := store.addAttachmentLocked(attachment, func() error { return postReplace }); !errors.Is(err, postReplace) {
		t.Fatalf("post-replacement error = %v", err)
	}
	if len(store.state.Attachments) != 1 || store.state.Attachments[0].StoredPath != attachment.StoredPath {
		t.Fatalf("post-replacement failure lost committed attachment: %#v", store.state.Attachments)
	}
}

func TestStateV6MigratesUpstreamDefaultsAndPersistsV7(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	audit := filepath.Join(dir, "audit.jsonl")
	fixture := `{"version":6,"next_session_id":2,"terminal_sessions":[{"id":1,"state":"running","anchor_chat_id":100,"anchor_message_id":10,"watch_enabled":true}],"attachments":[]}`
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path, audit)
	if err != nil {
		t.Fatal(err)
	}
	session, ok := store.FindSession(1)
	if !ok || store.Snapshot().Version != currentStateVersion || session.UpstreamMessageID != 0 || len(session.SeenUpstreamSignalIDs) != 0 || !session.LastUpstreamSignalAt.IsZero() || !session.UpstreamRetryAt.IsZero() {
		t.Fatalf("migrated v6 state = %#v session=%#v ok=%v", store.Snapshot(), session, ok)
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, audit)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Snapshot().Version != currentStateVersion {
		t.Fatalf("persisted version = %d", reopened.Snapshot().Version)
	}
}

func TestStoreClearsInvalidPersistedAnchorMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte(`{
  "version": 4,
  "anchor_mode": "unsupported",
  "next_session_id": 1,
  "terminal_sessions": [],
  "attachments": []
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path, filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if got := store.Snapshot().AnchorMode; got != "" {
		t.Fatalf("normalized anchor mode = %q, want empty", got)
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
	if st.Version != currentStateVersion || st.NextSessionID != 8 || st.ProcessedMessages == nil {
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

func TestAnchorFileBindingsRemainProcessLocalAndDeepCloned(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	store, err := Open(path, filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.AllocateSession("main", "@1", "%1", "shell")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.UpdateSession(session.ID, func(current *TerminalSession) {
		current.AnchorFiles = []string{"/tmp/report.txt"}
		current.AnchorFileToken = "0123456789abcdef"
	}); err != nil {
		t.Fatal(err)
	}
	snapshot := store.Snapshot()
	snapshot.TerminalSessions[0].AnchorFiles[0] = "/tmp/mutated.txt"
	current, _ := store.FindSession(session.ID)
	if current.AnchorFiles[0] != "/tmp/report.txt" {
		t.Fatalf("snapshot aliased anchor files: %#v", current.AnchorFiles)
	}
	persisted, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(persisted), "report.txt") || strings.Contains(string(persisted), "0123456789abcdef") {
		t.Fatalf("anchor file binding persisted: %s", persisted)
	}
	reopened, err := Open(path, filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	loaded, _ := reopened.FindSession(session.ID)
	if len(loaded.AnchorFiles) != 0 || loaded.AnchorFileToken != "" {
		t.Fatalf("process-local binding survived restart: %#v", loaded)
	}
}

func TestStoreNormalizesLegacyAnchorFormats(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte(`{
  "version": 4,
  "next_session_id": 2,
  "terminal_sessions": [{
    "id": 1,
    "state": "running",
    "anchor_message_id": 10,
    "retiring_anchor_message_id": 9
  }],
  "attachments": []
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path, filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session, ok := store.FindSession(1)
	if !ok || session.AnchorFormat != "text" || session.RetiringAnchorFormat != "text" {
		t.Fatalf("normalized anchor formats = %#v ok=%v", session, ok)
	}
}

func TestStoreRejectsNewerSchemaWithoutRewritingIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	audit := filepath.Join(dir, "audit.jsonl")
	future := []byte(fmt.Sprintf(`{"version":%d,"next_session_id":99,"future_field":"keep-me"}`, currentStateVersion+1))
	if err := os.WriteFile(path, future, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, audit); err == nil || !strings.Contains(err.Error(), "newer than supported") {
		t.Fatalf("Open future schema error = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(future) {
		t.Fatalf("future schema was rewritten:\n%s", got)
	}
}

func TestStoreNormalizesLegacyConceptsAndDropsWriteOnlyFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	audit := filepath.Join(dir, "audit.jsonl")
	legacy := `{
  "version": 1,
  "next_session_id": 2,
  "terminal_sessions": [{
    "id": 1,
    "state": "idle",
    "watch_enabled": true,
    "chat_id": 100,
    "created_by_user_id": 42,
    "tmux_session_id": "$1",
    "last_summary_hash": "unused",
    "last_summary_model": "unused",
    "pending_refresh": true,
    "last_telegram_error": "stale"
  }],
  "attachments": []
}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path, audit)
	if err != nil {
		t.Fatal(err)
	}
	session, ok := store.FindSession(1)
	if !ok || session.State != TerminalLost || session.WatchEnabled {
		t.Fatalf("normalized legacy session = %#v ok=%v", session, ok)
	}
	if store.Snapshot().Version != currentStateVersion {
		t.Fatalf("state version = %d, want %d", store.Snapshot().Version, currentStateVersion)
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	persisted, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, removed := range []string{"chat_id", "created_by_user_id", "tmux_session_id", "last_summary_hash", "last_summary_model", "pending_refresh", "last_telegram_error"} {
		if strings.Contains(string(persisted), `"`+removed+`"`) {
			t.Fatalf("removed field %q remained in state: %s", removed, persisted)
		}
	}
}

func TestStoreIgnoresLegacyHandoffAndPreservesAnchorLifecycle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	legacy := `{
  "version": 3,
  "next_session_id": 2,
  "terminal_sessions": [{
    "id": 1,
    "state": "running",
    "watch_enabled": true,
    "anchor_chat_id": 100,
    "anchor_message_id": 10,
    "anchor_format": "snapshot",
    "retiring_anchor_message_id": 9,
    "retiring_anchor_format": "text",
    "anchor_pinned": true,
    "handoff": {
      "key": "approval",
      "status_report": "Waiting for approval.",
      "recommended_action": "Approve or reject.",
      "evidence": ["Confirm [y/N]"],
      "observation_hash": "hash",
      "opened_at": "2026-07-10T03:00:00Z",
      "last_confirmed_at": "2026-07-10T03:00:00Z",
      "notification_message_id": 11
    }
  }]
}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path, filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session, ok := store.FindSession(1)
	if !ok || session.AnchorChatID != 100 || session.AnchorMessageID != 10 || session.AnchorFormat != "snapshot" || session.RetiringAnchorMessageID != 9 || session.RetiringAnchorFormat != "text" || session.AnchorPinKnown {
		t.Fatalf("schema 3 anchor migration = %#v ok=%v", session, ok)
	}
	if store.Snapshot().Version != currentStateVersion {
		t.Fatalf("state version = %d, want %d", store.Snapshot().Version, currentStateVersion)
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	persisted, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(persisted), `"handoff"`) {
		t.Fatalf("legacy handoff persisted: %s", persisted)
	}
}

func TestOnlyCanonicalAnchorRoutes(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.AllocateSession("main", "@1", "%1", "release")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.UpdateSession(session.ID, func(s *TerminalSession) {
		s.AnchorChatID = 100
		s.AnchorMessageID = 10
	}); err != nil {
		t.Fatal(err)
	}
	got, ok := store.FindByAnchor(100, 11)
	if ok {
		t.Fatalf("noncanonical message routed = %#v", got)
	}
	got, ok = store.FindByAnchor(100, 10)
	if !ok || got.ID != session.ID {
		t.Fatalf("canonical anchor route = %#v ok=%v", got, ok)
	}
}

func TestReplyTargetsDistinguishCurrentAndStaleAlternates(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.AllocateSession("main", "@1", "%1", "release")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.UpdateSession(session.ID, func(s *TerminalSession) {
		s.AnchorChatID = 100
		s.AnchorMessageID = 10
		s.SummaryMessageID = 20
		s.SnapshotMessageID = 30
		s.EvidenceMessageID = 35
		s.UpstreamMessageID = 40
		s.StaleAlternateMessageIDs = []int{18, 28}
	}); err != nil {
		t.Fatal(err)
	}
	for _, messageID := range []int{10, 20, 30, 35, 40} {
		got, targetState, ok := store.FindReplyTarget(100, messageID)
		if !ok || targetState != ReplyTargetCurrent || got.ID != session.ID {
			t.Fatalf("current reply target %d = %#v %q ok=%v", messageID, got, targetState, ok)
		}
	}
	for _, messageID := range []int{18, 28} {
		got, targetState, ok := store.FindReplyTarget(100, messageID)
		if !ok || targetState != ReplyTargetStale || got.ID != session.ID {
			t.Fatalf("stale reply target %d = %#v %q ok=%v", messageID, got, targetState, ok)
		}
	}
	if _, _, ok := store.FindReplyTarget(100, 99); ok {
		t.Fatal("unrelated message resolved as a reply target")
	}
}

func TestUpstreamSignalStatePersistsWithoutPayload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	audit := filepath.Join(dir, "audit.jsonl")
	store, err := Open(path, audit)
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.AllocateSession("main", "@1", "%1", "release")
	if err != nil {
		t.Fatal(err)
	}
	deliveredAt := time.Now().UTC().Truncate(time.Nanosecond)
	if _, _, err := store.UpdateSession(session.ID, func(s *TerminalSession) {
		s.AnchorChatID = 100
		s.AnchorMessageID = 10
		s.UpstreamMessageID = 40
		s.SeenUpstreamSignalIDs = []string{"0123456789abcdef0123456789abcdef"}
		s.LastUpstreamSignalAt = deliveredAt
		s.LastRawCapture = "[engram:upstream] signal payload"
	}); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, audit)
	if err != nil {
		t.Fatal(err)
	}
	got, targetState, ok := reopened.FindReplyTarget(100, 40)
	if !ok || targetState != ReplyTargetCurrent || !reflect.DeepEqual(got.SeenUpstreamSignalIDs, []string{"0123456789abcdef0123456789abcdef"}) || !got.LastUpstreamSignalAt.Equal(deliveredAt) {
		t.Fatalf("reopened upstream state = %#v %q ok=%v", got, targetState, ok)
	}
	persisted, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(persisted, []byte("signal payload")) {
		t.Fatalf("state retained an upstream payload: %s", persisted)
	}
}

func TestSeenUpstreamRecordsRemainBounded(t *testing.T) {
	session := TerminalSession{}
	for i := 0; i < maxSeenUpstreamSignals+5; i++ {
		session.RecordSeenUpstreamSignal(fmt.Sprintf("%032x", i))
	}
	if len(session.SeenUpstreamSignalIDs) != maxSeenUpstreamSignals || session.SeenUpstreamSignalIDs[0] != fmt.Sprintf("%032x", 5) || !session.HasSeenUpstreamSignal(fmt.Sprintf("%032x", maxSeenUpstreamSignals+4)) {
		t.Fatalf("seen records = %#v", session.SeenUpstreamSignalIDs)
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

func TestReadSnapshotDoesNotModifyOrRecoverState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	body := []byte(`{"version":7,"next_session_id":2,"terminal_sessions":[{"id":1,"tmux_window_id":"@2","tmux_pane_id":"%3","state":"running"}],"attachments":[]}`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := ReadSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.TerminalSessions) != 1 || snapshot.TerminalSessions[0].Origin != TerminalOriginAttached {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	afterBody, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterBody, body) || after.Mode() != before.Mode() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("ReadSnapshot modified persisted state")
	}

	corrupt := []byte(`{"version":`)
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSnapshot(path); err == nil {
		t.Fatal("ReadSnapshot accepted corrupt state")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, corrupt) {
		t.Fatal("ReadSnapshot replaced corrupt state")
	}
	if matches, _ := filepath.Glob(path + ".corrupt-*"); len(matches) != 0 {
		t.Fatalf("ReadSnapshot created recovery files: %v", matches)
	}
}

func TestReadSnapshotRejectsSymlinkAndFutureSchema(t *testing.T) {
	dir := t.TempDir()
	realPath := filepath.Join(dir, "real.json")
	linkPath := filepath.Join(dir, "state.json")
	if err := os.WriteFile(realPath, []byte(`{"version":7}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSnapshot(linkPath); err == nil || (!strings.Contains(err.Error(), "symbolic links") && !strings.Contains(err.Error(), "too many levels")) {
		t.Fatalf("symlink error = %v", err)
	}
	if err := os.Remove(linkPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(linkPath, []byte(`{"version":999}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSnapshot(linkPath); err == nil || !strings.Contains(err.Error(), "newer than supported") {
		t.Fatalf("future schema error = %v", err)
	}
}

func TestReadSnapshotRejectsFIFOAndPublicPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSnapshot(path); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("FIFO error = %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version":7}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSnapshot(path); err == nil || !strings.Contains(err.Error(), "group or other") {
		t.Fatalf("permissions error = %v", err)
	}
}
