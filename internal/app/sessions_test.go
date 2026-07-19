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
	actions := writeTrackedSessions(&b, sessions)
	want := "\nlost\n[3] detached\n\nactive\n[2] recent\n[1] older\n"
	if b.String() != want {
		t.Fatalf("tracked sessions:\n%s\nwant:\n%s", b.String(), want)
	}
	if len(actions) != 3 || actions[0].ID != 3 || actions[1].ID != 2 || actions[2].ID != 1 {
		t.Fatalf("actions = %#v", actions)
	}
}

func TestSessionActionTokenChangesWhenAnIDIsRebound(t *testing.T) {
	created := time.Date(2026, 7, 18, 20, 0, 0, 0, time.UTC)
	first := state.TerminalSession{ID: 1, CreatedAt: created, TmuxServerID: "server-a", TmuxWindowID: "@1", TmuxPaneID: "%1"}
	rebound := first
	rebound.CreatedAt = created.Add(time.Second)
	rebound.TmuxWindowID = "@2"
	rebound.TmuxPaneID = "%2"
	if sessionActionToken(first) == sessionActionToken(rebound) {
		t.Fatal("rebound session retained a stale action token")
	}
}
