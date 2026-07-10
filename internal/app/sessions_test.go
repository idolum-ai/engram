package app

import (
	"strings"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/state"
)

func TestWriteTrackedSessionsPrioritizesDurableHandoffs(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 10, 2, 0, 0, 0, time.UTC)
	sessions := []state.TerminalSession{
		{ID: 1, State: state.TerminalRunning, Title: "quiet", LastActivityAt: now},
		{ID: 2, State: state.TerminalRunning, Title: "older handoff", Handoff: &state.Handoff{Key: "approval", OpenedAt: now.Add(-time.Minute), RecommendedAction: "Approve or reject the deployment."}},
		{ID: 3, State: state.TerminalLost, Title: "detached", UpdatedAt: now},
		{ID: 4, State: state.TerminalRunning, Title: "observing", Handoff: &state.Handoff{Key: "fix", OpenedAt: now, AcknowledgedAt: now, RecommendedAction: "Fix it."}},
		{ID: 5, State: state.TerminalRunning, Title: "newer handoff", Handoff: &state.Handoff{Key: "choice", OpenedAt: now, RecommendedAction: "Choose the release target."}},
		{ID: 6, State: state.TerminalClosed, Title: "closed"},
	}
	var b strings.Builder
	ids := writeTrackedSessions(&b, sessions)
	wantText := "\nlost\n[3] detached\n\nneeds you\n[2] older handoff — Approve or reject the deployment.\n[5] newer handoff — Choose the release target.\n\nquiet\n[1] quiet\n[4] observing — observing\n"
	if b.String() != wantText {
		t.Fatalf("tracked sessions:\n%s\nwant:\n%s", b.String(), wantText)
	}
	wantIDs := []int{3, 2, 5, 1, 4}
	if len(ids) != len(wantIDs) {
		t.Fatalf("ids = %#v", ids)
	}
	for i := range ids {
		if ids[i] != wantIDs[i] {
			t.Fatalf("ids = %#v, want %#v", ids, wantIDs)
		}
	}
}
