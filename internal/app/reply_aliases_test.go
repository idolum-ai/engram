package app

import (
	"reflect"
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
	recordAlternateMessage(&session, "upstream", 30)
	recordAlternateMessage(&session, "upstream", 31)
	recordAlternateMessage(&session, "evidence", 40)
	recordAlternateMessage(&session, "evidence", 41)
	if session.SummaryMessageID != 11 || session.SnapshotMessageID != 21 || session.EvidenceMessageID != 41 || session.UpstreamMessageID != 31 {
		t.Fatalf("current aliases = summary %d snapshot %d evidence %d upstream %d", session.SummaryMessageID, session.SnapshotMessageID, session.EvidenceMessageID, session.UpstreamMessageID)
	}
	if len(session.StaleAlternateMessageIDs) != 4 || session.StaleAlternateMessageIDs[0] != 10 || session.StaleAlternateMessageIDs[1] != 20 || session.StaleAlternateMessageIDs[2] != 30 || session.StaleAlternateMessageIDs[3] != 40 {
		t.Fatalf("stale aliases = %#v", session.StaleAlternateMessageIDs)
	}
	for id := 12; id < 40; id++ {
		recordAlternateMessage(&session, "summary", id)
	}
	if len(session.StaleAlternateMessageIDs) != maxStaleAlternateMessages {
		t.Fatalf("stale alias count = %d", len(session.StaleAlternateMessageIDs))
	}
}

func TestRetireAlternateReplyTargetsMakesEveryAliasStale(t *testing.T) {
	session := state.TerminalSession{
		AnchorMessageID:          10,
		SummaryMessageID:         20,
		SnapshotMessageID:        30,
		EvidenceMessageID:        35,
		EvidenceAnchorMessageID:  10,
		LastEvidenceHash:         "hash",
		UpstreamMessageID:        40,
		StaleAlternateMessageIDs: []int{19},
	}

	retireAlternateReplyTargets(&session)

	if session.SummaryMessageID != 0 || session.SnapshotMessageID != 0 || session.EvidenceMessageID != 0 || session.EvidenceAnchorMessageID != 0 || session.LastEvidenceHash != "" || session.UpstreamMessageID != 0 {
		t.Fatalf("current aliases survived retirement: %#v", session)
	}
	want := []int{19, 20, 30, 35, 40}
	if !reflect.DeepEqual(session.StaleAlternateMessageIDs, want) {
		t.Fatalf("stale aliases = %#v, want %#v", session.StaleAlternateMessageIDs, want)
	}
}
