package app

import (
	"strings"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/state"
)

func TestWriteTrackedSessionsGroupsByAttention(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 10, 2, 0, 0, 0, time.UTC)
	sessions := []state.TerminalSession{
		{ID: 1, State: state.TerminalRunning, Title: "quiet", LastAttention: "none", LastAttentionAt: now},
		{ID: 2, State: state.TerminalRunning, Title: "older act", LastAttention: "act", LastAttentionAt: now.Add(-time.Minute)},
		{ID: 3, State: state.TerminalLost, Title: "detached", UpdatedAt: now},
		{ID: 4, State: state.TerminalRunning, Title: "review", LastAttention: "review", LastAttentionAt: now},
		{ID: 5, State: state.TerminalRunning, Title: "newer act", LastAttention: "act", LastAttentionAt: now},
		{ID: 6, State: state.TerminalClosed, Title: "closed"},
	}
	var b strings.Builder
	ids := writeTrackedSessions(&b, sessions)
	wantText := "\nlost\n[3] detached\n\nneeds you\n[5] newer act\n[2] older act\n\nworth reviewing\n[4] review\n\nworking quietly\n[1] quiet\n"
	if b.String() != wantText {
		t.Fatalf("tracked sessions:\n%s\nwant:\n%s", b.String(), wantText)
	}
	wantIDs := []int{3, 5, 2, 4, 1}
	if len(ids) != len(wantIDs) {
		t.Fatalf("ids = %#v", ids)
	}
	for i := range ids {
		if ids[i] != wantIDs[i] {
			t.Fatalf("ids = %#v, want %#v", ids, wantIDs)
		}
	}
}
