package app

import (
	"testing"

	"github.com/idolum-ai/engram/internal/state"
)

const appTestServerID = "0123456789abcdef0123456789abcdef"

func bindTestSession(t *testing.T, store *state.Store, id int) state.TerminalSession {
	t.Helper()
	updated, ok, err := store.UpdateSession(id, func(session *state.TerminalSession) {
		session.TmuxServerID = appTestServerID
	})
	if err != nil || !ok {
		t.Fatalf("bind test session: ok=%v err=%v", ok, err)
	}
	return updated
}
