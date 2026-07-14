package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/anthropic"
	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/tmux"
)

func TestAlignedConversationDeltaIncludesChangesAndRemovals(t *testing.T) {
	previous := testConversationFrame("codex", strings.Join([]string{
		"project /tmp/engram", "branch feature", "tests running", "app pending", "status footer", "cwd /tmp", "ready marker",
	}, "\n"))
	current := testConversationFrame("codex", strings.Join([]string{
		"project /tmp/engram", "branch feature", "tests passed", "app ok", "status footer", "cwd /tmp", "ready marker",
	}, "\n"))

	changed, removed, stable, ok := alignedConversationDelta(previous, current)
	if !ok {
		t.Fatal("alignedConversationDelta() did not align related frames")
	}
	if changed != "tests passed\napp ok" || removed != "tests running\napp pending" {
		t.Fatalf("changed=%q removed=%q", changed, removed)
	}
	if stable != "branch feature\nstatus footer" {
		t.Fatalf("stable = %q", stable)
	}
}

func TestConversationDeltaRebasesAcrossTruthBoundaries(t *testing.T) {
	base := testConversationFrame("codex", "one unique\ntwo unique\nthree unique\nfour unique\nfive unique")
	tests := []struct {
		name  string
		frame conversationFrame
	}{
		{name: "program changed", frame: mutateConversationFrame(base, func(frame *conversationFrame) { frame.command = "bash" })},
		{name: "server changed", frame: mutateConversationFrame(base, func(frame *conversationFrame) { frame.serverID = strings.Repeat("b", 32) })},
		{name: "window changed", frame: mutateConversationFrame(base, func(frame *conversationFrame) { frame.windowID = "@9" })},
		{name: "pane changed", frame: mutateConversationFrame(base, func(frame *conversationFrame) { frame.paneID = "%9" })},
		{name: "resized", frame: mutateConversationFrame(base, func(frame *conversationFrame) { frame.columns++ })},
		{name: "alternate screen changed", frame: mutateConversationFrame(base, func(frame *conversationFrame) { frame.alternateOn = "0" })},
		{name: "copy mode changed", frame: mutateConversationFrame(base, func(frame *conversationFrame) { frame.paneInMode = "1" })},
		{name: "unrelated redraw", frame: testConversationFrame("codex", "alpha\nbeta\ngamma\ndelta\nepsilon")},
		{name: "repeated chrome only", frame: testConversationFrame("codex", "border\nborder\nnew body\nborder\nborder")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, _, ok := alignedConversationDelta(base, tt.frame); ok {
				t.Fatal("alignedConversationDelta() continued across a truth boundary")
			}
		})
	}
}

func TestConversationDeltaRejectsThreeSharedLinesAcrossUnrelatedBodies(t *testing.T) {
	oldLines := []string{"project header", "branch header", "status footer"}
	newLines := []string{"project header", "branch header", "status footer"}
	for index := 0; index < 61; index++ {
		oldLines = append(oldLines, fmt.Sprintf("old body line %d", index))
		newLines = append(newLines, fmt.Sprintf("new body line %d", index))
	}
	if _, _, _, ok := alignedConversationDelta(testConversationFrame("codex", strings.Join(oldLines, "\n")), testConversationFrame("codex", strings.Join(newLines, "\n"))); ok {
		t.Fatal("three shared chrome lines aligned unrelated terminal bodies")
	}
}

func TestConversationDeltaRejectsAsymmetricRedraws(t *testing.T) {
	short := testConversationFrame("codex", "project header\nbranch header\nstatus footer")
	longLines := []string{"project header", "branch header", "status footer"}
	for index := 0; index < 61; index++ {
		longLines = append(longLines, fmt.Sprintf("body line %d", index))
	}
	long := testConversationFrame("codex", strings.Join(longLines, "\n"))

	for _, tt := range []struct {
		name     string
		previous conversationFrame
		current  conversationFrame
	}{
		{name: "large frame collapsed", previous: long, current: short},
		{name: "small frame expanded", previous: short, current: long},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, _, ok := alignedConversationDelta(tt.previous, tt.current); ok {
				t.Fatal("alignedConversationDelta() continued across an asymmetric redraw")
			}
		})
	}
}

func TestConversationTurnAlwaysCarriesFullCurrentTruth(t *testing.T) {
	app, session := conversationEpochTestApp(t, 3)
	firstCapture := testStyledCapture("codex", "project\nbranch\ntests running\napp pending\nstatus\ncwd\nready")
	first := app.prepareConversationTurn(session, firstCapture, firstCapture.JoinedText)
	if first.input.VisibleText != firstCapture.JoinedText || first.input.PreviousRendering != "" {
		t.Fatalf("first turn = %#v", first.input)
	}
	if !app.commitConversationTurn(session, first, "We are running tests.") {
		t.Fatal("first turn did not commit")
	}

	secondCapture := testStyledCapture("codex", "project\nbranch\ntests passed\napp ok\nstatus\ncwd\nready")
	second := app.prepareConversationTurn(session, secondCapture, secondCapture.JoinedText)
	if second.input.VisibleText != secondCapture.JoinedText || second.input.PreviousRendering != "We are running tests." || second.input.ChangedText != "tests passed\napp ok" || second.input.RemovedText != "tests running\napp pending" {
		t.Fatalf("second turn = %#v", second.input)
	}
}

func TestConversationResetRejectsInFlightCommit(t *testing.T) {
	app, session := conversationEpochTestApp(t, 2)
	capture := testStyledCapture("codex", "project\nbranch\nworking\nstatus\ncwd\nready")
	turn := app.prepareConversationTurn(session, capture, capture.JoinedText)
	app.resetConversationEpoch(2)
	if app.commitConversationTurn(session, turn, "This result arrived too late.") {
		t.Fatal("reset accepted an in-flight commit")
	}
	next := app.prepareConversationTurn(session, capture, capture.JoinedText)
	if next.input.PreviousRendering != "" || next.input.VisibleText == "" {
		t.Fatalf("reset did not force a full turn: %#v", next.input)
	}
}

func TestConversationRevisionCannotABAAfterPruning(t *testing.T) {
	app, session := conversationEpochTestApp(t, 1)
	capture := testStyledCapture("codex", "project\nbranch\nworking\nstatus\ncwd\nready")
	stale := app.prepareConversationTurn(session, capture, capture.JoinedText)
	app.pruneConversationEpochs(nil)
	app.resetConversationEpoch(session.ID)
	app.pruneConversationEpochs(nil)
	app.prepareConversationTurn(session, capture, capture.JoinedText)
	if app.commitConversationTurn(session, stale, "obsolete") {
		t.Fatal("prune and recreation revalidated an obsolete revision")
	}
}

func TestConversationBoundaryDiscardsOldContextBeforeDelivery(t *testing.T) {
	app, session := conversationEpochTestApp(t, 1)
	firstCapture := testStyledCapture("codex", "project\nbranch\none\ntwo\nthree\nfour\nfive")
	first := app.prepareConversationTurn(session, firstCapture, firstCapture.JoinedText)
	if !app.commitConversationTurn(session, first, "Old context") {
		t.Fatal("first turn did not commit")
	}
	unrelated := testStyledCapture("codex", "alpha\nbeta\ngamma\ndelta\nepsilon\nzeta\neta")
	turn := app.prepareConversationTurn(session, unrelated, unrelated.JoinedText)
	if turn.input.PreviousRendering != "" {
		t.Fatal("boundary-crossing turn retained prior rendering")
	}
	returning := app.prepareConversationTurn(session, firstCapture, firstCapture.JoinedText)
	if returning.input.PreviousRendering != "" {
		t.Fatal("failed boundary turn allowed old context to return")
	}
}

func TestConversationGatesDoNotSerializeUnrelatedSessions(t *testing.T) {
	app := &App{}
	releaseFirst, ok := app.acquireConversation(context.Background(), 1)
	if !ok {
		t.Fatal("failed to acquire first conversation gate")
	}
	defer releaseFirst()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	releaseOther, ok := app.acquireConversation(ctx, 33)
	if !ok {
		t.Fatal("unrelated session was blocked by another conversation")
	}
	releaseOther()
}

func TestConversationCommitRejectsReattachedBinding(t *testing.T) {
	app, session := conversationEpochTestApp(t, 4)
	capture := testStyledCapture("codex", "project\nbranch\nworking\nstatus\ncwd\nready")
	turn := app.prepareConversationTurn(session, capture, capture.JoinedText)
	if _, _, err := app.Store.UpdateSession(session.ID, func(current *state.TerminalSession) {
		current.TmuxServerID = strings.Repeat("b", 32)
	}); err != nil {
		t.Fatal(err)
	}
	if app.commitConversationTurn(session, turn, "Old server output") {
		t.Fatal("old binding committed after reattach")
	}
}

func TestConversationEpochPrunesUntrackedSessions(t *testing.T) {
	app, session := conversationEpochTestApp(t, 1)
	app.ensureConversationEpochsLocked()
	app.conversationEpochs[99] = conversationEpoch{summary: "orphan", frame: testConversationFrame("codex", "orphan")}
	app.pruneConversationEpochs([]state.TerminalSession{session})
	if _, ok := app.conversationEpochs[99]; ok {
		t.Fatal("orphaned conversation epoch survived pruning")
	}
}

func TestConversationalSummaryBuildsTruthGroundedContinuation(t *testing.T) {
	app, session := conversationEpochTestApp(t, 8)
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
	app.Guide = client
	first := testStyledCapture("codex", "project\nbranch\ntests running\napp pending\nstatus\ncwd\nready")
	summary, turn, err := app.conversationalSummary(context.Background(), session, first, first.JoinedText)
	if err != nil || !app.commitConversationTurn(session, turn, summary) {
		t.Fatalf("first summary: %q %v", summary, err)
	}
	second := testStyledCapture("codex", "project\nbranch\ntests passed\napp ok\nstatus\ncwd\nready")
	if _, _, err := app.conversationalSummary(context.Background(), session, second, second.JoinedText); err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 2 || prompts[0]["observation"] != "full" || prompts[1]["observation"] != "incremental" {
		t.Fatalf("prompts = %#v", prompts)
	}
	if prompts[1]["terminal_text"] != second.JoinedText || prompts[1]["previous_rendering"] != "Work is in progress." || prompts[1]["changed_terminal_text"] != "tests passed\napp ok" || prompts[1]["removed_terminal_text"] != "tests running\napp pending" {
		t.Fatalf("incremental prompt = %#v", prompts[1])
	}
}

func TestConversationalSummaryDoesNotDiscloseSupersededFrame(t *testing.T) {
	for _, boundary := range []string{"mode", "binding"} {
		t.Run(boundary, func(t *testing.T) {
			app, session := conversationEpochTestApp(t, 1)
			app.guideSlots = make(chan struct{}, 1)
			app.guideSlots <- struct{}{}
			var requests atomic.Int32
			client := anthropic.New("key", "claude-haiku-4-5-20251001")
			client.HTTPClient = &http.Client{Transport: conversationEpochRoundTrip(func(*http.Request) (*http.Response, error) {
				requests.Add(1)
				return nil, errors.New("request should not be sent")
			})}
			app.Guide = client
			capture := testStyledCapture("codex", "project\nbranch\nworking\nstatus\ncwd\nready")
			type result struct{ err error }
			done := make(chan result, 1)
			started := make(chan struct{})
			go func() {
				close(started)
				_, _, err := app.conversationalSummary(context.Background(), session, capture, capture.JoinedText)
				done <- result{err: err}
			}()
			<-started
			if boundary == "mode" {
				app.setAnchorMode(config.AnchorModeSnapshot)
			} else if _, _, err := app.Store.UpdateSession(session.ID, func(current *state.TerminalSession) {
				current.TmuxServerID = strings.Repeat("b", 32)
			}); err != nil {
				t.Fatal(err)
			}
			<-app.guideSlots
			got := <-done
			if !errors.Is(got.err, errConversationTurnSuperseded) || requests.Load() != 0 {
				t.Fatalf("summary error=%v requests=%d", got.err, requests.Load())
			}
		})
	}
}

func TestSnapshotConversationalSummaryDoesNotDiscloseSupersededFrame(t *testing.T) {
	for _, boundary := range []string{"mode", "binding"} {
		t.Run(boundary, func(t *testing.T) {
			app, session := conversationEpochTestApp(t, 1)
			app.setAnchorMode(config.AnchorModeSnapshot)
			var err error
			session, _, err = app.Store.UpdateSession(session.ID, func(current *state.TerminalSession) {
				current.AnchorChatID = 100
				current.AnchorMessageID = 77
				current.AnchorFormat = "snapshot"
			})
			if err != nil {
				t.Fatal(err)
			}
			app.guideSlots = make(chan struct{}, 1)
			app.guideSlots <- struct{}{}
			var requests atomic.Int32
			client := anthropic.New("key", "claude-haiku-4-5-20251001")
			client.HTTPClient = &http.Client{Transport: conversationEpochRoundTrip(func(*http.Request) (*http.Response, error) {
				requests.Add(1)
				return nil, errors.New("request should not be sent")
			})}
			app.Guide = client
			done := make(chan error, 1)
			started := make(chan struct{})
			go func() {
				close(started)
				_, err := app.snapshotConversationalSummary(context.Background(), session, 77, "private terminal frame")
				done <- err
			}()
			<-started
			if boundary == "mode" {
				app.setAnchorMode(config.AnchorModeGuide)
			} else if _, _, err := app.Store.UpdateSession(session.ID, func(current *state.TerminalSession) {
				current.TmuxServerID = strings.Repeat("b", 32)
			}); err != nil {
				t.Fatal(err)
			}
			<-app.guideSlots
			if err := <-done; !errors.Is(err, errConversationTurnSuperseded) || requests.Load() != 0 {
				t.Fatalf("summary error=%v requests=%d", err, requests.Load())
			}
		})
	}
}

func conversationEpochTestApp(t *testing.T, id int) (*App, state.TerminalSession) {
	t.Helper()
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.AllocateSession("main", "@1", "%1", "test")
	if err != nil {
		t.Fatal(err)
	}
	if session.ID != id {
		// Tests use the ID only for readability; allocate intervening sessions when needed.
		for session.ID < id {
			session, err = store.AllocateSession("main", "@1", "%1", "test")
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	session, _, err = store.UpdateSession(session.ID, func(current *state.TerminalSession) {
		current.TmuxServerID = appTestServerID
		current.State = state.TerminalRunning
		current.WatchEnabled = true
	})
	if err != nil {
		t.Fatal(err)
	}
	return &App{Store: store}, session
}

func testConversationFrame(command, text string) conversationFrame {
	return conversationFrame{serverID: appTestServerID, windowID: "@1", paneID: "%1", command: command, alternateOn: "1", paneInMode: "0", columns: 80, visibleRows: 24, text: text}
}

func mutateConversationFrame(frame conversationFrame, mutate func(*conversationFrame)) conversationFrame {
	mutate(&frame)
	return frame
}

func testStyledCapture(command, text string) tmux.StyledCapture {
	return tmux.StyledCapture{ServerID: appTestServerID, WindowID: "@1", PaneID: "%1", CurrentCmd: command, AlternateOn: "1", PaneInMode: "0", Columns: 80, VisibleRows: 24, JoinedText: text}
}

type conversationEpochRoundTrip func(*http.Request) (*http.Response, error)

func (fn conversationEpochRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}
