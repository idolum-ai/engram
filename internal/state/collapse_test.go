package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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
	collapsed, committed, err := store.CollapseSessionIntoShelf(session.ID, session, CollapsedShelf{ChatID: 100, MessageID: 88})
	if err != nil || !committed || !collapsed.Collapsed {
		t.Fatalf("collapse = %#v committed=%v err=%v", collapsed, committed, err)
	}
	if shelf := store.Snapshot().CollapsedShelf; shelf == nil || shelf.LastRenderHash != "" {
		t.Fatalf("collapse claimed an unconfirmed shelf render: %#v", shelf)
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
	pending, committed, err := reopened.BeginExpandSessionFromShelf(session.ID, 88, 100, 99)
	if err != nil || !committed || !pending.Collapsed || pending.AnchorMessageID != 0 || pending.PendingRestore == nil ||
		pending.PendingRestore.ChatID != 100 || pending.PendingRestore.MessageID != 99 {
		t.Fatalf("begin expand = %#v committed=%v err=%v", pending, committed, err)
	}
	pending.PendingRestore.MessageID = 1234
	got, _ = reopened.FindSession(session.ID)
	if got.PendingRestore == nil || got.PendingRestore.MessageID != 99 {
		t.Fatalf("begin result was not deeply cloned: %#v", got.PendingRestore)
	}
	if _, target, found := reopened.FindReplyTarget(100, 99); found {
		t.Fatalf("inert pending restore became reply target %q", target)
	}
	if repeated, repeatedCommit, repeatErr := reopened.BeginExpandSessionFromShelf(session.ID, 88, 100, 99); repeatErr != nil || !repeatedCommit ||
		repeated.PendingRestore == nil || repeated.PendingRestore.MessageID != 99 {
		t.Fatalf("repeat begin = %#v committed=%v err=%v", repeated, repeatedCommit, repeatErr)
	}
	if conflicting, conflictingCommit, conflictErr := reopened.BeginExpandSessionFromShelf(session.ID, 88, 100, 101); conflictErr != nil || conflictingCommit ||
		conflicting.PendingRestore == nil || conflicting.PendingRestore.MessageID != 99 {
		t.Fatalf("conflicting begin = %#v committed=%v err=%v", conflicting, conflictingCommit, conflictErr)
	}

	reopened, err = Open(path, audit)
	if err != nil {
		t.Fatal(err)
	}
	pending, ok = reopened.FindSession(session.ID)
	if !ok || !pending.Collapsed || pending.AnchorMessageID != 0 || pending.PendingRestore == nil || pending.PendingRestore.MessageID != 99 {
		t.Fatalf("reopened pending restore = %#v ok=%v", pending, ok)
	}
	expanded, committed, err := reopened.FinishExpandSessionFromShelf(session.ID, 100, 99)
	if err != nil || !committed || expanded.Collapsed || expanded.AnchorMessageID != 99 {
		t.Fatalf("finish expand = %#v committed=%v err=%v", expanded, committed, err)
	}
	if expanded.PendingRestore != nil {
		t.Fatalf("finish retained pending restore: %#v", expanded.PendingRestore)
	}
	if repeated, repeatedCommit, repeatErr := reopened.FinishExpandSessionFromShelf(session.ID, 100, 99); repeatErr != nil || !repeatedCommit ||
		repeated.Collapsed || repeated.AnchorMessageID != 99 {
		t.Fatalf("repeat finish = %#v committed=%v err=%v", repeated, repeatedCommit, repeatErr)
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

func TestPendingRestoreRetryPersistsAndBeginRollsBackOnSaveFailure(t *testing.T) {
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
		current.AnchorChatID = 100
		current.AnchorMessageID = 77
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, committed, err := store.CollapseSessionIntoShelf(session.ID, session, CollapsedShelf{ChatID: 100, MessageID: 88}); err != nil || !committed {
		t.Fatalf("collapse committed=%v err=%v", committed, err)
	}
	if _, retired, err := store.FinishCollapsedAnchorRetirement(session.ID, 100, 77); err != nil || !retired {
		t.Fatalf("retire committed=%v err=%v", retired, err)
	}

	store.path = filepath.Join(dir, "missing", "state.json")
	if got, committed, err := store.BeginExpandSessionFromShelf(session.ID, 88, 100, 99); err == nil || committed || got.PendingRestore != nil {
		t.Fatalf("failed begin = %#v committed=%v err=%v", got, committed, err)
	}
	got, _ := store.FindSession(session.ID)
	if !got.Collapsed || got.PendingRestore != nil || got.AnchorMessageID != 0 {
		t.Fatalf("failed begin did not roll back: %#v", got)
	}

	store.path = path
	if _, committed, err := store.BeginExpandSessionFromShelf(session.ID, 88, 100, 99); err != nil || !committed {
		t.Fatalf("begin committed=%v err=%v", committed, err)
	}
	retryAt := time.Now().UTC().Add(time.Minute).Truncate(time.Nanosecond)
	if _, found, err := store.UpdateSession(session.ID, func(current *TerminalSession) {
		current.PendingRestore.RetryAt = retryAt
	}); err != nil || !found {
		t.Fatalf("set retry found=%v err=%v", found, err)
	}
	reopened, err := Open(path, audit)
	if err != nil {
		t.Fatal(err)
	}
	got, _ = reopened.FindSession(session.ID)
	if got.PendingRestore == nil || !got.PendingRestore.RetryAt.Equal(retryAt) {
		t.Fatalf("reopened retry = %#v", got.PendingRestore)
	}
	reopened.path = filepath.Join(dir, "still-missing", "state.json")
	if failed, committed, err := reopened.FinishExpandSessionFromShelf(session.ID, 100, 99); err == nil || committed ||
		failed.PendingRestore == nil || failed.PendingRestore.MessageID != 99 {
		t.Fatalf("failed finish = %#v committed=%v err=%v", failed, committed, err)
	}
	got, _ = reopened.FindSession(session.ID)
	if !got.Collapsed || got.AnchorMessageID != 0 || got.PendingRestore == nil || got.PendingRestore.MessageID != 99 {
		t.Fatalf("failed finish did not roll back: %#v", got)
	}
}

func TestExpandedSessionCanReturnToItsShelfWhenControlsFail(t *testing.T) {
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
	if _, committed, err := store.CollapseSessionIntoShelf(session.ID, session, CollapsedShelf{
		ChatID: 100, MessageID: 88,
	}); err != nil || !committed {
		t.Fatalf("collapse committed=%v err=%v", committed, err)
	}
	if _, begun, err := store.BeginExpandSessionFromShelf(session.ID, 88, 100, 99); err != nil || !begun {
		t.Fatalf("begin committed=%v err=%v", begun, err)
	}
	if _, finished, err := store.FinishExpandSessionFromShelf(session.ID, 100, 99); err != nil || !finished {
		t.Fatalf("finish committed=%v err=%v", finished, err)
	}

	returned, committed, err := store.ReturnExpandedSessionToShelf(session.ID, 88, 100, 99)
	if err != nil || !committed || !returned.Collapsed || returned.PendingRestore != nil ||
		returned.AnchorMessageID != 99 {
		t.Fatalf("return = %#v committed=%v err=%v", returned, committed, err)
	}
	if _, target, found := store.FindReplyTarget(100, 99); !found || target != ReplyTargetStale {
		t.Fatalf("returned anchor target=%q found=%v", target, found)
	}
	reopened, err := Open(path, audit)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := reopened.FindSession(session.ID)
	if !got.Collapsed || got.AnchorMessageID != 99 || reopened.Snapshot().CollapsedShelf == nil {
		t.Fatalf("reopened returned session=%#v shelf=%#v", got, reopened.Snapshot().CollapsedShelf)
	}
}

func TestCollapsedShelfReplacementPersistsPredecessorUntilRetired(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	audit := filepath.Join(dir, "audit.jsonl")
	store, err := Open(path, audit)
	if err != nil {
		t.Fatal(err)
	}
	if _, committed, err := store.SetCollapsedShelfIfEmpty(CollapsedShelf{
		ChatID: 100, MessageID: 88, LastRenderHash: "old", Pinned: true, PinKnown: true,
	}); err != nil || !committed {
		t.Fatalf("set shelf committed=%v err=%v", committed, err)
	}
	replacement := CollapsedShelf{
		ChatID: 100, MessageID: 99, LastRenderHash: "new", Pinned: true, PinKnown: true,
	}
	store.path = filepath.Join(dir, "missing", "state.json")
	if failed, committed, err := store.ReplaceCollapsedShelf(88, replacement); err == nil || committed ||
		failed.MessageID != 88 || failed.RetiringMessageID != 0 {
		t.Fatalf("failed replace = %#v committed=%v err=%v", failed, committed, err)
	}
	shelf := store.Snapshot().CollapsedShelf
	if shelf == nil || shelf.MessageID != 88 || shelf.RetiringMessageID != 0 {
		t.Fatalf("failed replacement did not roll back: %#v", shelf)
	}
	store.path = path
	replaced, committed, err := store.ReplaceCollapsedShelf(88, replacement)
	if err != nil || !committed || replaced.MessageID != 99 || replaced.RetiringChatID != 100 || replaced.RetiringMessageID != 88 {
		t.Fatalf("replace = %#v committed=%v err=%v", replaced, committed, err)
	}
	if repeated, repeatedCommit, repeatErr := store.ReplaceCollapsedShelf(88, replacement); repeatErr != nil || !repeatedCommit ||
		repeated.RetiringMessageID != 88 {
		t.Fatalf("repeat replace = %#v committed=%v err=%v", repeated, repeatedCommit, repeatErr)
	}
	retryAt := time.Now().UTC().Add(time.Minute).Truncate(time.Nanosecond)
	if _, found, err := store.UpdateCollapsedShelf(99, func(current *CollapsedShelf) {
		current.RetiringRetryAt = retryAt
	}); err != nil || !found {
		t.Fatalf("set retiring retry found=%v err=%v", found, err)
	}
	if cleared, err := store.ClearCollapsedShelf(99); err != nil || cleared {
		t.Fatalf("clear discarded pending predecessor: cleared=%v err=%v", cleared, err)
	}

	reopened, err := Open(path, audit)
	if err != nil {
		t.Fatal(err)
	}
	shelf = reopened.Snapshot().CollapsedShelf
	if shelf == nil || shelf.MessageID != 99 || shelf.RetiringChatID != 100 || shelf.RetiringMessageID != 88 ||
		!shelf.RetiringRetryAt.Equal(retryAt) || shelf.PinKnown {
		t.Fatalf("reopened replacement = %#v", shelf)
	}
	retired, committed, err := reopened.FinishCollapsedShelfRetirement(99, 100, 88)
	if err != nil || !committed || retired.RetiringChatID != 0 || retired.RetiringMessageID != 0 || !retired.RetiringRetryAt.IsZero() {
		t.Fatalf("finish retirement = %#v committed=%v err=%v", retired, committed, err)
	}
	if cleared, err := reopened.ClearCollapsedShelf(99); err != nil || !cleared || reopened.Snapshot().CollapsedShelf != nil {
		t.Fatalf("clear replacement cleared=%v err=%v shelf=%#v", cleared, err, reopened.Snapshot().CollapsedShelf)
	}
}

func TestCollapsedShelfPredecessorCanBeRecoveredAfterReplacementLoss(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	audit := filepath.Join(dir, "audit.jsonl")
	store, err := Open(path, audit)
	if err != nil {
		t.Fatal(err)
	}
	if _, committed, err := store.SetCollapsedShelfIfEmpty(CollapsedShelf{
		ChatID: 100, MessageID: 88, LastRenderHash: "old", Pinned: true, PinKnown: true,
	}); err != nil || !committed {
		t.Fatalf("set shelf committed=%v err=%v", committed, err)
	}
	if _, committed, err := store.ReplaceCollapsedShelf(88, CollapsedShelf{
		ChatID: 100, MessageID: 99, LastRenderHash: "new",
	}); err != nil || !committed {
		t.Fatalf("replace committed=%v err=%v", committed, err)
	}

	recovered, committed, err := store.RecoverCollapsedShelfPredecessor(99)
	if err != nil || !committed || recovered.ChatID != 100 || recovered.MessageID != 88 ||
		recovered.RetiringChatID != 0 || recovered.RetiringMessageID != 0 ||
		recovered.LastRenderHash != "" || recovered.Pinned || recovered.PinKnown {
		t.Fatalf("recover = %#v committed=%v err=%v", recovered, committed, err)
	}
	reopened, err := Open(path, audit)
	if err != nil {
		t.Fatal(err)
	}
	shelf := reopened.Snapshot().CollapsedShelf
	if shelf == nil || shelf.MessageID != 88 || shelf.RetiringMessageID != 0 ||
		shelf.LastRenderHash != "" || shelf.PinKnown {
		t.Fatalf("reopened recovered shelf = %#v", shelf)
	}
}

func TestPendingCollapseKeepsAnchorLiveUntilShelfIsReady(t *testing.T) {
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
		current.AnchorChatID = 100
		current.AnchorMessageID = 77
		current.SummaryMessageID = 78
	})
	if err != nil {
		t.Fatal(err)
	}
	retryAt := time.Now().UTC().Add(time.Minute)
	pending, committed, err := store.BeginCollapseSessionIntoShelf(session.ID, session, CollapsedShelf{ChatID: 100, MessageID: 88, RetryAt: retryAt})
	if err != nil || !committed || pending.Collapsed || !pending.PendingCollapse || pending.AnchorMessageID != 77 || pending.SummaryMessageID != 78 {
		t.Fatalf("pending=%#v committed=%v err=%v", pending, committed, err)
	}
	if shelf := store.Snapshot().CollapsedShelf; shelf == nil || !shelf.RetryAt.Equal(retryAt) {
		t.Fatalf("pending collapse erased shelf retry deadline: %#v", shelf)
	}
	if routed, target, found := store.FindReplyTarget(100, 77); !found || target != ReplyTargetCurrent || routed.ID != session.ID {
		t.Fatalf("pending anchor target=%q found=%v session=%#v", target, found, routed)
	}
	reopened, err := Open(path, audit)
	if err != nil {
		t.Fatal(err)
	}
	pending, _ = reopened.FindSession(session.ID)
	if pending.Collapsed || !pending.PendingCollapse || pending.AnchorMessageID != 77 {
		t.Fatalf("reopened pending collapse = %#v", pending)
	}
	canceled, committed, err := reopened.CancelPendingCollapse(session.ID, 88)
	if err != nil || !committed || canceled.Collapsed || canceled.PendingCollapse || canceled.AnchorMessageID != 77 {
		t.Fatalf("canceled=%#v committed=%v err=%v", canceled, committed, err)
	}
	pending, committed, err = reopened.BeginCollapseSessionIntoShelf(session.ID, canceled, CollapsedShelf{ChatID: 100, MessageID: 88})
	if err != nil || !committed || !pending.PendingCollapse {
		t.Fatalf("restarted pending=%#v committed=%v err=%v", pending, committed, err)
	}
	collapsed, committed, err := reopened.FinishCollapseSessionIntoShelf(session.ID, 88)
	if err != nil || !committed || !collapsed.Collapsed || collapsed.PendingCollapse || collapsed.SummaryMessageID != 0 {
		t.Fatalf("collapsed=%#v committed=%v err=%v", collapsed, committed, err)
	}
	if _, target, found := reopened.FindReplyTarget(100, 78); !found || target != ReplyTargetStale {
		t.Fatalf("retired summary target=%q found=%v", target, found)
	}
}

func TestClosedPendingRestoreOwnershipPersistsUntilRetired(t *testing.T) {
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
		current.State = TerminalClosed
		current.Collapsed = false
		current.PendingRestore = &PendingRestore{ChatID: 100, MessageID: 99, RetryAt: time.Now().UTC().Add(time.Minute)}
	})
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, audit)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := reopened.FindSession(session.ID)
	if got.PendingRestore == nil || got.PendingRestore.MessageID != 99 {
		t.Fatalf("closed cleanup identity was lost on reopen: %#v", got.PendingRestore)
	}
	retired, committed, err := reopened.FinishPendingRestoreRetirement(session.ID, 100, 99)
	if err != nil || !committed || retired.PendingRestore != nil {
		t.Fatalf("retired=%#v committed=%v err=%v", retired, committed, err)
	}
}

func TestMissingPendingRestoreCanBeAbandonedAndRetried(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.AllocateSession("main", "@1", "%1", "build")
	if err != nil {
		t.Fatal(err)
	}
	session, _, err = store.UpdateSession(session.ID, func(current *TerminalSession) {
		current.Collapsed = true
		current.AnchorChatID = 100
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, committed, err := store.SetCollapsedShelfIfEmpty(CollapsedShelf{ChatID: 100, MessageID: 88}); err != nil || !committed {
		t.Fatalf("set shelf committed=%v err=%v", committed, err)
	}
	if _, begun, err := store.BeginExpandSessionFromShelf(session.ID, 88, 100, 99); err != nil || !begun {
		t.Fatalf("begin restore committed=%v err=%v", begun, err)
	}
	abandoned, committed, err := store.AbandonPendingRestore(session.ID, 100, 99)
	if err != nil || !committed || abandoned.PendingRestore != nil || !abandoned.Collapsed {
		t.Fatalf("abandoned=%#v committed=%v err=%v", abandoned, committed, err)
	}
	if _, target, found := store.FindReplyTarget(100, 99); !found || target != ReplyTargetStale {
		t.Fatalf("missing prospective target=%q found=%v", target, found)
	}
	retried, begun, err := store.BeginExpandSessionFromShelf(session.ID, 88, 100, 100)
	if err != nil || !begun || retried.PendingRestore == nil || retried.PendingRestore.MessageID != 100 {
		t.Fatalf("retried=%#v begun=%v err=%v", retried, begun, err)
	}
}

func TestMessageCleanupOwnershipPersistsUntilFinished(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	audit := filepath.Join(dir, "audit.jsonl")
	store, err := Open(path, audit)
	if err != nil {
		t.Fatal(err)
	}
	retryAt := time.Now().UTC().Add(time.Minute)
	if err := store.RememberMessageCleanup(MessageCleanup{
		ChatID: 100, MessageID: 91, RetryAt: retryAt, RateLimited: true,
	}); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, audit)
	if err != nil {
		t.Fatal(err)
	}
	cleanups := reopened.Snapshot().PendingMessageCleanups
	if len(cleanups) != 1 || cleanups[0].MessageID != 91 || !cleanups[0].RetryAt.Equal(retryAt) || !cleanups[0].RateLimited {
		t.Fatalf("reopened cleanups = %#v", cleanups)
	}
	if found, err := reopened.FinishMessageCleanup(100, 91); err != nil || !found {
		t.Fatalf("finish found=%v err=%v", found, err)
	}
	if cleanups := reopened.Snapshot().PendingMessageCleanups; len(cleanups) != 0 {
		t.Fatalf("finished cleanups = %#v", cleanups)
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
