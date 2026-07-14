package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/idolum-ai/engram/internal/anthropic"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/tmux"
)

func TestAlignedConversationDeltaUsesOnlyChangedCurrentLines(t *testing.T) {
	previous := testConversationFrame("codex", "worktree: /tmp/project\nRunning tests\npackage app pending\nCtrl+C to stop")
	current := testConversationFrame("codex", "worktree: /tmp/project\nTests passed\npackage app ok\nCtrl+C to stop")

	changed, stable, ok := alignedConversationDelta(previous, current)
	if !ok {
		t.Fatal("alignedConversationDelta() did not align related frames")
	}
	if changed != "Tests passed\npackage app ok" {
		t.Fatalf("changed = %q", changed)
	}
	if stable != "worktree: /tmp/project\nCtrl+C to stop" {
		t.Fatalf("stable = %q", stable)
	}
}

func TestConversationDeltaRebasesAcrossTruthBoundaries(t *testing.T) {
	base := testConversationFrame("codex", "one\ntwo\nthree\nfour")
	tests := []struct {
		name  string
		frame conversationFrame
	}{
		{name: "program changed", frame: testConversationFrame("bash", "one\ntwo\nchanged\nfour")},
		{name: "server changed", frame: func() conversationFrame {
			frame := testConversationFrame("codex", "one\ntwo\nchanged\nfour")
			frame.serverID = strings.Repeat("b", 32)
			return frame
		}()},
		{name: "pane changed", frame: func() conversationFrame {
			frame := testConversationFrame("codex", "one\ntwo\nchanged\nfour")
			frame.paneID = "%9"
			return frame
		}()},
		{name: "resized", frame: func() conversationFrame {
			frame := testConversationFrame("codex", "one\ntwo\nchanged\nfour")
			frame.columns++
			return frame
		}()},
		{name: "unrelated redraw", frame: testConversationFrame("codex", "alpha\nbeta\ngamma\ndelta")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, ok := alignedConversationDelta(base, tt.frame); ok {
				t.Fatal("alignedConversationDelta() continued across a truth boundary")
			}
		})
	}
}

func TestConversationEpochCarriesLatestInputThenForgetsIt(t *testing.T) {
	app := &App{}
	firstCapture := testStyledCapture("codex", "prompt\n$ go test ./...\nwaiting\nstatus")
	session := testConversationSession(3)
	first := app.prepareConversationTurn(session, firstCapture, firstCapture.JoinedText)
	if first.input.VisibleText == "" || first.input.PreviousRendering != "" {
		t.Fatalf("first turn = %#v", first.input)
	}
	app.commitConversationTurn(3, first, "We are waiting at the prompt.")
	app.noteConversationInput(3, "go test ./...")

	secondCapture := testStyledCapture("codex", "prompt\n$ go test ./...\ntests passed\nstatus")
	second := app.prepareConversationTurn(session, secondCapture, secondCapture.JoinedText)
	if second.input.VisibleText != "" || second.input.PreviousRendering != "We are waiting at the prompt." || second.input.RecentUserInput != "go test ./..." || second.input.ChangedText != "tests passed" {
		t.Fatalf("second turn = %#v", second.input)
	}
	app.commitConversationTurn(3, second, "The tests passed.")

	thirdCapture := testStyledCapture("codex", "prompt\n$ go test ./...\ntests passed\nready")
	third := app.prepareConversationTurn(session, thirdCapture, thirdCapture.JoinedText)
	if third.input.RecentUserInput != "" {
		t.Fatalf("consumed input survived: %#v", third.input)
	}

	app.resetConversationEpoch(3)
	rebased := app.prepareConversationTurn(session, thirdCapture, thirdCapture.JoinedText)
	if rebased.input.VisibleText == "" || rebased.input.PreviousRendering != "" {
		t.Fatalf("manual reset did not rebase: %#v", rebased.input)
	}
}

func TestConversationCommitPreservesInputArrivingDuringModelCall(t *testing.T) {
	app := &App{}
	capture := testStyledCapture("codex", "prompt\nworking\nstatus")
	session := testConversationSession(1)
	turn := app.prepareConversationTurn(session, capture, capture.JoinedText)
	app.noteConversationInput(1, "new command")
	app.commitConversationTurn(1, turn, "Work is in progress.")

	nextCapture := testStyledCapture("codex", "prompt\n> new command\ndone\nstatus")
	next := app.prepareConversationTurn(session, nextCapture, nextCapture.JoinedText)
	if next.input.RecentUserInput != "new command" {
		t.Fatalf("input arriving during request was lost: %#v", next.input)
	}
}

func TestConversationResetRejectsInFlightCommit(t *testing.T) {
	app := &App{}
	session := testConversationSession(2)
	capture := testStyledCapture("codex", "prompt\nworking\nstatus")
	turn := app.prepareConversationTurn(session, capture, capture.JoinedText)
	app.resetConversationEpoch(2)
	app.commitConversationTurn(2, turn, "This result arrived too late.")

	next := app.prepareConversationTurn(session, capture, capture.JoinedText)
	if next.input.PreviousRendering != "" || next.input.VisibleText == "" {
		t.Fatalf("reset accepted an in-flight commit: %#v", next.input)
	}
}

func TestConversationEpochDoesNotForwardHiddenInput(t *testing.T) {
	app := &App{}
	session := testConversationSession(5)
	firstCapture := testStyledCapture("login", "Password:\nwaiting")
	first := app.prepareConversationTurn(session, firstCapture, firstCapture.JoinedText)
	app.commitConversationTurn(5, first, "A password prompt is waiting.")
	app.noteConversationInput(5, "not-visible-secret")
	secondCapture := testStyledCapture("login", "Welcome\nready")
	second := app.prepareConversationTurn(session, secondCapture, secondCapture.JoinedText)
	if second.input.RecentUserInput != "" {
		t.Fatalf("hidden terminal input was forwarded: %#v", second.input)
	}
}

func TestConversationEpochDoesNotForwardOversizedInput(t *testing.T) {
	app := &App{}
	session := testConversationSession(6)
	input := strings.Repeat("x", maxConversationInputBytes+1)
	capture := testStyledCapture("codex", input+"\nready")
	app.noteConversationInput(6, input)
	turn := app.prepareConversationTurn(session, capture, capture.JoinedText)
	if turn.input.RecentUserInput != "" {
		t.Fatalf("oversized input was partially forwarded: bytes=%d", len(turn.input.RecentUserInput))
	}
}

func TestConversationalSummarySendsIncrementalTurn(t *testing.T) {
	var prompts []map[string]any
	client := anthropic.New("key", "claude-haiku-4-5-20251001")
	client.HTTPClient = &http.Client{Transport: conversationEpochRoundTrip(func(request *http.Request) (*http.Response, error) {
		var payload struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		var prompt map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(payload.Messages[0].Content, "TERMINAL_OBSERVATION_JSON\n")), &prompt); err != nil {
			t.Fatal(err)
		}
		prompts = append(prompts, prompt)
		response := `{"stop_reason":"end_turn","content":[{"type":"text","text":"Work is in progress."}]}`
		if len(prompts) == 2 {
			response = `{"stop_reason":"end_turn","content":[{"type":"text","text":"The tests passed."}]}`
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewBufferString(response))}, nil
	})}
	app := &App{Anthropic: client}
	first := testStyledCapture("codex", "project\n$ go test ./...\ntests running\nstatus")
	session := testConversationSession(8)
	if _, err := app.conversationalSummary(context.Background(), session, first, first.JoinedText); err != nil {
		t.Fatal(err)
	}
	app.noteConversationInput(8, "go test ./...")
	second := testStyledCapture("codex", "project\n$ go test ./...\ntests passed\nstatus")
	if _, err := app.conversationalSummary(context.Background(), session, second, second.JoinedText); err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 2 || prompts[0]["observation"] != "full" || prompts[1]["observation"] != "incremental" {
		t.Fatalf("prompts = %#v", prompts)
	}
	if prompts[1]["previous_rendering"] != "Work is in progress." || prompts[1]["recent_user_input"] != "go test ./..." || prompts[1]["changed_terminal_text"] != "tests passed" {
		t.Fatalf("incremental prompt = %#v", prompts[1])
	}
	if _, ok := prompts[1]["terminal_text"]; ok {
		t.Fatalf("incremental prompt retained full frame: %#v", prompts[1])
	}
}

func testConversationFrame(command, text string) conversationFrame {
	return conversationFrame{serverID: appTestServerID, windowID: "@1", paneID: "%1", command: command, columns: 80, visibleRows: 24, text: text}
}

func testStyledCapture(command, text string) tmux.StyledCapture {
	return tmux.StyledCapture{CurrentCmd: command, Columns: 80, VisibleRows: 24, JoinedText: text}
}

func testConversationSession(id int) state.TerminalSession {
	return state.TerminalSession{ID: id, TmuxServerID: appTestServerID, TmuxWindowID: "@1", TmuxPaneID: "%1"}
}

type conversationEpochRoundTrip func(*http.Request) (*http.Response, error)

func (fn conversationEpochRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}
