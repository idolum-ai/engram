package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/state"
)

func TestWriteTrackedSessionsOrdersLostCollapsedThenRecentActive(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 10, 2, 0, 0, 0, time.UTC)
	sessions := []state.TerminalSession{
		{ID: 1, State: state.TerminalRunning, Title: "older", LastActivityAt: now.Add(-time.Minute)},
		{ID: 2, State: state.TerminalRunning, Title: "recent", LastActivityAt: now},
		{ID: 3, State: state.TerminalLost, Title: "detached", UpdatedAt: now},
		{ID: 4, State: state.TerminalClosed, Title: "closed"},
		{ID: 5, State: state.TerminalRunning, Title: "quiet", Collapsed: true, LastActivityAt: now},
	}
	var b strings.Builder
	actions := writeTrackedSessions(&b, sessions)
	want := "\nlost\n[3] detached\n\ncollapsed\n[5] quiet\n\nactive\n[2] recent\n[1] older\n"
	if b.String() != want {
		t.Fatalf("tracked sessions:\n%s\nwant:\n%s", b.String(), want)
	}
	if len(actions) != 3 || actions[0].ID != 3 || actions[1].ID != 2 || actions[2].ID != 1 {
		t.Fatalf("actions = %#v", actions)
	}
}

func TestWatchSessionDoesNotBypassCollapsedShelf(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	if _, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.Collapsed = true
		session.AnchorMessageID = 0
	}); err != nil {
		t.Fatal(err)
	}
	result := app.watchSession(context.Background(), id, 0)
	if result.OK() || !strings.Contains(result.Message, "collapsed shelf") {
		t.Fatalf("watch result = %#v", result)
	}
}

func TestCollapsedSessionsStillCountAsTracked(t *testing.T) {
	sessions := []state.TerminalSession{{ID: 1, State: state.TerminalRunning, Collapsed: true}}
	if !hasTrackedSessions(sessions) {
		t.Fatal("collapsed session was reported as no tracked work")
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
