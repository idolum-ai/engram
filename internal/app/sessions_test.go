package app

import (
	"strings"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/state"
)

func TestWriteTrackedSessionsOrdersLostThenRecentActive(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 10, 2, 0, 0, 0, time.UTC)
	sessions := []state.TerminalSession{
		{ID: 1, State: state.TerminalRunning, Title: "older", LastActivityAt: now.Add(-time.Minute)},
		{ID: 2, State: state.TerminalRunning, Title: "recent", LastActivityAt: now},
		{ID: 3, State: state.TerminalLost, Title: "detached", UpdatedAt: now},
		{ID: 4, State: state.TerminalClosed, Title: "closed"},
	}
	var b strings.Builder
	ids := writeTrackedSessions(&b, sessions)
	want := "\nlost\n[3] detached\n\nactive\n[2] recent\n[1] older\n"
	if b.String() != want {
		t.Fatalf("tracked sessions:\n%s\nwant:\n%s", b.String(), want)
	}
	if len(ids) != 3 || ids[0] != 3 || ids[1] != 2 || ids[2] != 1 {
		t.Fatalf("ids = %#v", ids)
	}
}
