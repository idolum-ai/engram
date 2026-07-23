package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCollapsedShelfLifecyclePersistsAndPreservesReplySafety(t *testing.T) {
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
	session, _, err = store.UpdateSession(session.ID, func(current *TerminalSession) {
		current.TmuxServerID = "server"
		current.AnchorChatID = 100
		current.AnchorMessageID = 77
		current.AnchorFormat = "snapshot"
		current.WatchEnabled = true
		current.SummaryMessageID = 71
		current.SnapshotMessageID = 72
		current.UpstreamMessageID = 73
	})
	if err != nil {
		t.Fatal(err)
	}
	collapsed, committed, err := store.CollapseSessionIntoShelf(session.ID, session, CollapsedShelf{ChatID: 100, MessageID: 88}, "hash")
	if err != nil || !committed || !collapsed.Collapsed {
		t.Fatalf("collapse = %#v committed=%v err=%v", collapsed, committed, err)
	}
	if _, target, found := store.FindReplyTarget(100, 77); !found || target != ReplyTargetStale {
		t.Fatalf("reply target while retirement is pending = %q, found=%v", target, found)
	}
	if _, _, found := store.FindReplyTarget(100, 88); found {
		t.Fatal("shared shelf became an ambiguous terminal reply target")
	}
	for _, messageID := range []int{71, 72, 73} {
		if _, target, found := store.FindReplyTarget(100, messageID); !found || target != ReplyTargetStale {
			t.Fatalf("alternate %d target while collapsed = %q, found=%v", messageID, target, found)
		}
	}
	if _, retired, err := store.FinishCollapsedAnchorRetirement(session.ID, 100, 77); err != nil || !retired {
		t.Fatalf("retirement committed=%v err=%v", retired, err)
	}

	reopened, err := Open(path, audit)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reopened.FindSession(session.ID)
	shelf := reopened.Snapshot().CollapsedShelf
	if !ok || !got.Collapsed || got.AnchorMessageID != 0 || shelf == nil || shelf.MessageID != 88 || shelf.PinKnown {
		t.Fatalf("reopened session=%#v shelf=%#v ok=%v", got, shelf, ok)
	}
	expanded, committed, err := reopened.ExpandSessionFromShelf(session.ID, 88, 100, 99)
	if err != nil || !committed || expanded.Collapsed || expanded.AnchorMessageID != 99 {
		t.Fatalf("expand = %#v committed=%v err=%v", expanded, committed, err)
	}
	if _, target, found := reopened.FindReplyTarget(100, 99); !found || target != ReplyTargetCurrent {
		t.Fatalf("expanded reply target = %q, found=%v", target, found)
	}
	for _, messageID := range []int{71, 72, 73} {
		if _, target, found := reopened.FindReplyTarget(100, messageID); !found || target != ReplyTargetStale {
			t.Fatalf("alternate %d revived after expansion: target=%q found=%v", messageID, target, found)
		}
	}
	if cleared, err := reopened.ClearCollapsedShelf(88); err != nil || !cleared || reopened.Snapshot().CollapsedShelf != nil {
		t.Fatalf("clear shelf cleared=%v err=%v shelf=%#v", cleared, err, reopened.Snapshot().CollapsedShelf)
	}
}

func TestLegacyStateDefaultsToNoCollapsedShelf(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.json")
	if err := os.WriteFile(path, []byte(`{"version":15,"next_session_id":2,"terminal_sessions":[{"id":1,"state":"running","watch_enabled":true}],"attachments":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path, filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session, ok := store.FindSession(1)
	if !ok || store.Snapshot().Version != currentStateVersion || session.Collapsed || store.Snapshot().CollapsedShelf != nil {
		t.Fatalf("legacy session=%#v shelf=%#v ok=%v version=%d", session, store.Snapshot().CollapsedShelf, ok, store.Snapshot().Version)
	}
}
