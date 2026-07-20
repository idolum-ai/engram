package app

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/cue"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
	"github.com/idolum-ai/engram/internal/tmux"
)

func TestCueMatchRendersAsExactCurrentAnchorSuggestion(t *testing.T) {
	t.Parallel()
	cues, err := cue.Open(filepath.Join(t.TempDir(), "cues.json"))
	if err != nil {
		t.Fatal(err)
	}
	created, err := cues.Add("review", `github\.com/idolum-ai/engram/pull/[0-9]+`, "Review this pull request and report concrete findings.", time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	a := &App{Cues: cues}
	ts := a.bindAnchorSuggestions(state.TerminalSession{ID: 7, State: state.TerminalRunning}, "https://github.com/idolum-ai/engram/pull/38", "codex", "/tmp/engram")
	if len(ts.AnchorSuggestions) != 1 || ts.AnchorSuggestions[0].CueID != created.ID || ts.AnchorSuggestionToken == "" {
		t.Fatalf("bound suggestions = %#v token=%q", ts.AnchorSuggestions, ts.AnchorSuggestionToken)
	}
	rendered := appendAnchorSuggestions("[7] running", ts.AnchorSuggestions)
	if !strings.Contains(rendered, "suggested:\n```\n1. Review this pull request and report concrete findings.\n```") {
		t.Fatalf("rendered suggestion = %q", rendered)
	}
	markup := a.anchorMarkup(ts)
	if len(markup.InlineKeyboard) != 3 || markup.InlineKeyboard[1][0].CallbackData != "cue-send:7:"+ts.AnchorSuggestionToken+":1" {
		t.Fatalf("cue markup = %#v", markup)
	}

	snapshotCaption, _ := a.snapshotAnchorCaption(ts, testCueCapture(), visibleReferences{})
	guidedCaption, _ := a.guidedEvidenceCaption(ts, "The pull request is ready.", visibleReferences{})
	for name, caption := range map[string]string{"snapshot": snapshotCaption, "guide": guidedCaption} {
		if !strings.Contains(caption, "Review this pull request and report concrete findings.") {
			t.Fatalf("%s caption omitted cue:\n%s", name, caption)
		}
	}
}

func TestCueCallbackSendsOnlyCurrentBoundSuggestionOnce(t *testing.T) {
	app, runner, refreshed := newAnchorKeyTestApp(t)
	cues, err := cue.Open(filepath.Join(t.TempDir(), "cues.json"))
	if err != nil {
		t.Fatal(err)
	}
	created, err := cues.Add("review", `ready for review`, "Review this pull request and report concrete findings.", time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	app.Cues = cues
	suggestion := state.AnchorSuggestion{CueID: created.ID, Prompt: created.Prompt, MatchHash: "match-hash"}
	var token string
	if _, _, err := app.Store.UpdateSession(1, func(session *state.TerminalSession) {
		setAnchorSuggestions(session, []state.AnchorSuggestion{suggestion})
		token = session.AnchorSuggestionToken
	}); err != nil {
		t.Fatal(err)
	}

	callback := telegram.CallbackQuery{
		ID: "cb", From: telegram.User{ID: 42},
		Message: &telegram.Message{MessageID: 10, Chat: telegram.Chat{ID: 100}},
		Data:    "cue-send:1:" + token + ":1",
	}
	if status := app.handleCallback(context.Background(), callback); status != "callback_ok" {
		t.Fatalf("first callback status = %q", status)
	}
	if len(runner.calls) != 4 || runner.calls[1][0] != "set-buffer" || runner.calls[1][4] != created.Prompt {
		t.Fatalf("cue tmux calls = %#v", runner.calls)
	}
	select {
	case <-refreshed:
	case <-time.After(time.Second):
		t.Fatal("cue input did not queue refresh")
	}
	if status := app.handleCallback(context.Background(), callback); status != "callback_user_error" {
		t.Fatalf("repeated callback status = %q", status)
	}
	if len(runner.calls) != 4 {
		t.Fatalf("repeated callback reached tmux: %#v", runner.calls)
	}
	if got := cues.Snapshot().Cues[0].UseCount; got != 1 {
		t.Fatalf("cue use count = %d, want 1", got)
	}
}

func TestSuccessfulAnchorRepliesLearnAndProposeCue(t *testing.T) {
	app, runner, _ := newAnchorKeyTestApp(t)
	app.refreshHook = func(context.Context, int, bool) {}
	cues, err := cue.Open(filepath.Join(t.TempDir(), "cues.json"))
	if err != nil {
		t.Fatal(err)
	}
	app.Cues = cues
	session, ok := app.Store.FindSession(1)
	if !ok {
		t.Fatal("session missing")
	}
	frameText := "Pull request https://github.com/idolum-ai/engram/pull/38 is ready for review."
	app.rememberAnchorTextFrame(session, frameText, "frame-hash")

	var proposalText string
	app.Telegram.HTTPClient = &http.Client{Transport: anchorKeyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/botTOKEN/sendMessage" {
			t.Fatalf("path = %s", req.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		proposalText, _ = body["text"].(string)
		return anchorKeyJSONResponse(`{"message_id":90,"chat":{"id":100}}`), nil
	})}
	prompt := "Review this pull request and report concrete findings."
	for index := 0; index < 2; index++ {
		status := app.handleUpdate(context.Background(), telegram.Update{Message: &telegram.Message{
			MessageID: 80 + index, Chat: telegram.Chat{ID: 100}, From: &telegram.User{ID: 42}, Text: prompt,
			ReplyToMessage: &telegram.Message{MessageID: 10, Chat: telegram.Chat{ID: 100}},
		}})
		if status != "anchor_reply_ok" {
			t.Fatalf("reply %d status = %q", index+1, status)
		}
	}
	app.refreshWG.Wait()
	if len(runner.calls) != 8 {
		t.Fatalf("reply tmux calls = %#v", runner.calls)
	}
	snapshot := cues.Snapshot()
	if len(snapshot.Candidates) != 1 || snapshot.Candidates[0].ProposalMessageID != 90 {
		t.Fatalf("learned candidates = %#v", snapshot.Candidates)
	}
	if !strings.Contains(proposalText, "Possible cue") || !strings.Contains(proposalText, prompt) || !strings.Contains(proposalText, `pull/[0-9]+`) {
		t.Fatalf("proposal text = %q", proposalText)
	}
}

func TestCueProposalCallbackPromotesOnlyItsExactMessage(t *testing.T) {
	app, _, _ := newAnchorKeyTestApp(t)
	cues, err := cue.Open(filepath.Join(t.TempDir(), "cues.json"))
	if err != nil {
		t.Fatal(err)
	}
	contextFrame := cue.Context{Text: "Pull request https://github.com/idolum-ai/engram/pull/38 is ready for review."}
	prompt := "Review this pull request and report concrete findings."
	_, _ = cues.Observe(contextFrame, prompt, time.Unix(1, 0))
	candidate, err := cues.Observe(contextFrame, prompt, time.Unix(2, 0))
	if err != nil || candidate == nil {
		t.Fatalf("candidate=%#v error=%v", candidate, err)
	}
	if err := cues.BindProposal(candidate.ID, 100, 90); err != nil {
		t.Fatal(err)
	}
	app.Cues = cues
	var paths []string
	app.Telegram.HTTPClient = &http.Client{Transport: anchorKeyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.Path)
		return anchorKeyJSONResponse(`true`), nil
	})}

	stale := telegram.CallbackQuery{ID: "stale", From: telegram.User{ID: 42}, Message: &telegram.Message{MessageID: 89, Chat: telegram.Chat{ID: 100}}, Data: "cue-save:" + candidate.ID}
	if status := app.handleCallback(context.Background(), stale); status != "callback_user_error" {
		t.Fatalf("stale proposal status = %q", status)
	}
	if len(cues.Snapshot().Cues) != 0 {
		t.Fatal("stale proposal promoted cue")
	}

	current := stale
	current.ID = "current"
	current.Message.MessageID = 90
	if status := app.handleCallback(context.Background(), current); status != "callback_ok" {
		t.Fatalf("current proposal status = %q", status)
	}
	snapshot := cues.Snapshot()
	if len(snapshot.Cues) != 1 || len(snapshot.Candidates) != 0 || snapshot.Cues[0].Prompt != prompt {
		t.Fatalf("cue snapshot = %#v", snapshot)
	}
	if !containsTestString(paths, "/botTOKEN/answerCallbackQuery") || !containsTestString(paths, "/botTOKEN/deleteMessage") {
		t.Fatalf("Telegram paths = %#v", paths)
	}
}

func containsTestString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func testCueCapture() tmux.StyledCapture {
	return tmux.StyledCapture{Title: "engram", CurrentPath: "/tmp/engram", Columns: 80, VisibleRows: 24, BufferRows: 24}
}
