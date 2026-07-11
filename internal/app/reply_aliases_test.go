package app

import (
	"testing"

	"github.com/idolum-ai/engram/internal/state"
)

func TestRecordAlternateMessageKeepsOnlyLatestAndBoundsStaleIDs(t *testing.T) {
	t.Parallel()
	session := state.TerminalSession{}
	recordAlternateMessage(&session, "summary", 10)
	recordAlternateMessage(&session, "summary", 11)
	recordAlternateMessage(&session, "snapshot", 20)
	recordAlternateMessage(&session, "snapshot", 21)
	if session.SummaryMessageID != 11 || session.SnapshotMessageID != 21 {
		t.Fatalf("current aliases = summary %d snapshot %d", session.SummaryMessageID, session.SnapshotMessageID)
	}
	if len(session.StaleAlternateMessageIDs) != 2 || session.StaleAlternateMessageIDs[0] != 10 || session.StaleAlternateMessageIDs[1] != 20 {
		t.Fatalf("stale aliases = %#v", session.StaleAlternateMessageIDs)
	}
	for id := 12; id < 40; id++ {
		recordAlternateMessage(&session, "summary", id)
	}
	if len(session.StaleAlternateMessageIDs) != maxStaleAlternateMessages {
		t.Fatalf("stale alias count = %d", len(session.StaleAlternateMessageIDs))
	}
}
