package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/keyseq"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
)

func TestKeyComposerExecutesExactPlanOnlyAfterCurrentConfirmation(t *testing.T) {
	app, runner, _ := newAnchorKeyTestApp(t)
	interpreter := &fakeKeyInterpreter{proposal: keyseq.Proposal{
		Kind: keyseq.KindSequence,
		Events: []keyseq.Event{
			{Key: keyseq.KeyUp, Count: 3},
			{Key: keyseq.KeyEnter, Count: 1},
		},
	}}
	app.KeyInterpreter = interpreter
	app.transferSlots = make(chan struct{}, 1)
	app.transferQueue = make(chan struct{}, 1)
	app.guideSlots = make(chan struct{}, 1)

	var mu sync.Mutex
	var confirmationToken string
	sendCount := 0
	app.Telegram.HTTPClient = &http.Client{Transport: anchorKeyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/botTOKEN/answerCallbackQuery":
			return anchorKeyJSONResponse(`true`), nil
		case "/botTOKEN/editMessageReplyMarkup":
			return anchorKeyJSONResponse(`{"message_id":72,"chat":{"id":100}}`), nil
		case "/botTOKEN/sendMessage":
			var body struct {
				ReplyMarkup struct {
					InlineKeyboard [][]telegram.InlineKeyboardButton `json:"inline_keyboard"`
					ForceReply     bool                              `json:"force_reply"`
				} `json:"reply_markup"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				return nil, err
			}
			mu.Lock()
			defer mu.Unlock()
			sendCount++
			messageID := 70 + sendCount
			if sendCount == 1 && !body.ReplyMarkup.ForceReply {
				return nil, fmt.Errorf("composer prompt did not use ForceReply")
			}
			if sendCount == 2 {
				if len(body.ReplyMarkup.InlineKeyboard) != 1 || len(body.ReplyMarkup.InlineKeyboard[0]) != 2 {
					return nil, fmt.Errorf("confirmation markup = %#v", body.ReplyMarkup.InlineKeyboard)
				}
				confirmationToken = strings.TrimPrefix(body.ReplyMarkup.InlineKeyboard[0][0].CallbackData, "keys-send:")
			}
			return anchorKeyJSONResponse(fmt.Sprintf(`{"message_id":%d,"chat":{"id":100}}`, messageID)), nil
		default:
			return nil, fmt.Errorf("unexpected Telegram path %s", req.URL.Path)
		}
	})}

	status := app.handleCallback(context.Background(), telegram.CallbackQuery{
		ID: "keyboard", From: telegram.User{ID: 42},
		Message: &telegram.Message{MessageID: 10, Chat: telegram.Chat{ID: 100}},
		Data:    "keyboard:1",
	})
	if status != "callback_ok" {
		t.Fatalf("keyboard callback status = %q", status)
	}
	status = app.handleUpdate(context.Background(), telegram.Update{
		Message: &telegram.Message{
			MessageID: 80, From: &telegram.User{ID: 42}, Chat: telegram.Chat{ID: 100},
			Text:           "up three times and press enter",
			ReplyToMessage: &telegram.Message{MessageID: 71},
		},
	})
	if status != "key_prompt_ok" {
		t.Fatalf("key prompt status = %q", status)
	}
	app.transferWG.Wait()
	if interpreter.description != "up three times and press enter" {
		t.Fatalf("model description = %q", interpreter.description)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("unconfirmed proposal touched tmux: %#v", runner.calls)
	}
	mu.Lock()
	token := confirmationToken
	mu.Unlock()
	if token == "" {
		t.Fatal("confirmation token was not sent")
	}

	status = app.handleCallback(context.Background(), telegram.CallbackQuery{
		ID: "approve", From: telegram.User{ID: 42},
		Message: &telegram.Message{MessageID: 72, Chat: telegram.Chat{ID: 100}},
		Data:    "keys-send:" + token,
	})
	if status != "callback_ok" {
		t.Fatalf("approval status = %q", status)
	}
	app.transferWG.Wait()
	if len(runner.calls) != 2 || runner.calls[0][0] != "display-message" || runner.calls[1][0] != "if-shell" ||
		!strings.Contains(runner.calls[1][5], "send-keys -t %1 'Up' 'Up' 'Up' 'Enter'") {
		t.Fatalf("tmux calls = %#v", runner.calls)
	}
	status = app.handleCallback(context.Background(), telegram.CallbackQuery{
		ID: "duplicate", From: telegram.User{ID: 42},
		Message: &telegram.Message{MessageID: 72, Chat: telegram.Chat{ID: 100}},
		Data:    "keys-send:" + token,
	})
	if status != "callback_user_error" || len(runner.calls) != 2 {
		t.Fatalf("duplicate approval status=%q tmux=%#v", status, runner.calls)
	}
}

func TestKeyConfirmationGuardsNeverTouchTmux(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*App, string)
		cbID   int
		cancel bool
	}{
		{name: "cancel", cbID: 72, cancel: true},
		{name: "wrong confirmation message", cbID: 73},
		{name: "expired", cbID: 72, mutate: func(a *App, token string) {
			confirmation := a.keyConfirmations[token]
			confirmation.ExpiresAt = time.Now().Add(-time.Second)
			a.keyConfirmations[token] = confirmation
		}},
		{name: "anchor moved", cbID: 72, mutate: func(a *App, _ string) {
			_, _, _ = a.Store.UpdateSession(1, func(session *state.TerminalSession) {
				session.AnchorMessageID = 11
			})
		}},
		{name: "watch stopped", cbID: 72, mutate: func(a *App, _ string) {
			_, _, _ = a.Store.UpdateSession(1, func(session *state.TerminalSession) {
				session.WatchEnabled = false
			})
		}},
		{name: "session lost", cbID: 72, mutate: func(a *App, _ string) {
			_, _, _ = a.Store.UpdateSession(1, func(session *state.TerminalSession) {
				session.State = state.TerminalLost
			})
		}},
		{name: "restart loses memory authority", cbID: 72, mutate: func(a *App, _ string) {
			a.keyConfirmations = map[string]keyConfirmation{}
			a.keyPromptSessions = map[int]keyWorkflow{}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app, runner, _ := newAnchorKeyTestApp(t)
			session, _ := app.Store.FindSession(1)
			proposal := keyseq.Proposal{Kind: keyseq.KindSequence, Events: []keyseq.Event{{Key: keyseq.KeyEnter, Count: 1}}}
			plan, err := keyseq.Compile(proposal)
			if err != nil {
				t.Fatal(err)
			}
			token := "0123456789abcdef"
			app.keyPromptSessions = map[int]keyWorkflow{1: {Token: "workflow", ExpiresAt: time.Now().Add(time.Minute)}}
			app.keyConfirmations = map[string]keyConfirmation{token: {
				WorkflowToken: "workflow", Session: session, UserID: 42, ChatID: 100, MessageID: 72,
				Proposal: proposal, Plan: plan, ExpiresAt: time.Now().Add(time.Minute),
			}}
			if test.mutate != nil {
				test.mutate(app, token)
			}
			app.Telegram.HTTPClient = &http.Client{Transport: anchorKeyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				switch req.URL.Path {
				case "/botTOKEN/answerCallbackQuery":
					return anchorKeyJSONResponse(`true`), nil
				case "/botTOKEN/editMessageReplyMarkup":
					return anchorKeyJSONResponse(`{"message_id":72,"chat":{"id":100}}`), nil
				case "/botTOKEN/sendMessage":
					return anchorKeyJSONResponse(`{"message_id":90,"chat":{"id":100}}`), nil
				default:
					return nil, fmt.Errorf("unexpected path %s", req.URL.Path)
				}
			})}
			action := "keys-send:"
			if test.cancel {
				action = "keys-cancel:"
			}
			app.handleCallback(context.Background(), telegram.CallbackQuery{
				ID: "decision", From: telegram.User{ID: 42},
				Message: &telegram.Message{MessageID: test.cbID, Chat: telegram.Chat{ID: 100}},
				Data:    action + token,
			})
			if len(runner.calls) != 0 {
				t.Fatalf("guarded confirmation touched tmux: %#v", runner.calls)
			}
		})
	}
}

func TestKeyConfirmationKeepsCompletePlanAheadOfBoundedTarget(t *testing.T) {
	proposal := keyseq.Proposal{Kind: keyseq.KindSequence, Events: []keyseq.Event{
		{Key: keyseq.KeyUp, Count: 3},
		{Key: keyseq.KeyC, Modifiers: []keyseq.Modifier{keyseq.ModifierControl}, Count: 1},
	}}
	text := keyConfirmationText(7, strings.Repeat("very-long-title-", 500), proposal)
	if !strings.HasPrefix(text, "Keys:\n↑ ×3  Ctrl+C\n\nTarget: [7] ") {
		t.Fatalf("confirmation does not lead with complete plan: %q", text)
	}
	if len(text) > 256 {
		t.Fatalf("bounded confirmation bytes = %d", len(text))
	}
}

func TestKeyConfirmationReturnsBeforeDelayedPlanCompletes(t *testing.T) {
	app, runner, _ := newAnchorKeyTestApp(t)
	app.transferSlots = make(chan struct{}, 1)
	app.transferQueue = make(chan struct{}, 1)
	session, _ := app.Store.FindSession(1)
	proposal := keyseq.Proposal{Kind: keyseq.KindSequence, Events: []keyseq.Event{
		{Key: keyseq.KeyEscape, Count: 2},
	}}
	plan, err := keyseq.Compile(proposal)
	if err != nil {
		t.Fatal(err)
	}
	token := "0123456789abcdef"
	app.keyPromptSessions = map[int]keyWorkflow{1: {Token: "workflow", ExpiresAt: time.Now().Add(time.Minute)}}
	app.keyConfirmations = map[string]keyConfirmation{token: {
		WorkflowToken: "workflow", Session: session, UserID: 42, ChatID: 100, MessageID: 72,
		Proposal: proposal, Plan: plan, ExpiresAt: time.Now().Add(time.Minute),
	}}
	delayStarted := make(chan struct{}, 1)
	releaseDelay := make(chan struct{})
	defer func() {
		select {
		case <-releaseDelay:
		default:
			close(releaseDelay)
		}
		app.transferWG.Wait()
	}()
	app.sleepHook = func(time.Duration) {
		delayStarted <- struct{}{}
		<-releaseDelay
	}
	app.Telegram.HTTPClient = &http.Client{Transport: anchorKeyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/botTOKEN/answerCallbackQuery":
			return anchorKeyJSONResponse(`true`), nil
		case "/botTOKEN/editMessageReplyMarkup":
			return anchorKeyJSONResponse(`{"message_id":72,"chat":{"id":100}}`), nil
		default:
			return nil, fmt.Errorf("unexpected path %s", req.URL.Path)
		}
	})}

	returned := make(chan string, 1)
	go func() {
		returned <- app.handleCallback(context.Background(), telegram.CallbackQuery{
			ID: "approve", From: telegram.User{ID: 42},
			Message: &telegram.Message{MessageID: 72, Chat: telegram.Chat{ID: 100}},
			Data:    "keys-send:" + token,
		})
	}()
	select {
	case status := <-returned:
		if status != "callback_ok" {
			t.Fatalf("callback status = %q", status)
		}
	case <-time.After(time.Second):
		t.Fatal("confirmation callback waited for the delayed key plan")
	}
	select {
	case <-delayStarted:
	case <-time.After(time.Second):
		t.Fatal("delayed plan did not start")
	}
	if len(runner.calls) != 2 {
		t.Fatalf("tmux calls before delay release = %#v", runner.calls)
	}
	close(releaseDelay)
	app.transferWG.Wait()
	if len(runner.calls) != 4 {
		t.Fatalf("tmux calls after delay release = %#v", runner.calls)
	}
}

func TestExpiredKeyWorkflowCannotCreateFreshConfirmation(t *testing.T) {
	app, _, _ := newAnchorKeyTestApp(t)
	session, _ := app.Store.FindSession(1)
	app.keyPromptSessions = map[int]keyWorkflow{1: {
		Token: "workflow", ExpiresAt: time.Now().Add(-time.Second),
	}}
	if app.keyWorkflowCurrent(1, "workflow") {
		t.Fatal("expired workflow remained current")
	}
	if app.storeKeyConfirmation("0123456789abcdef", keyConfirmation{
		WorkflowToken: "workflow", Session: session, UserID: 42, ChatID: 100, MessageID: 72,
		ExpiresAt: time.Now().Add(time.Minute),
	}) {
		t.Fatal("expired workflow created a fresh confirmation")
	}
}

func TestExpiredKeyConfirmationRetiresVisibleControls(t *testing.T) {
	app, _, _ := newAnchorKeyTestApp(t)
	session, _ := app.Store.FindSession(1)
	app.keyPromptSessions = map[int]keyWorkflow{1: {
		Token: "workflow", ExpiresAt: time.Now().Add(-time.Second),
	}}
	app.keyConfirmations = map[string]keyConfirmation{"0123456789abcdef": {
		WorkflowToken: "workflow", Session: session, UserID: 42, ChatID: 100, MessageID: 72,
		ExpiresAt: time.Now().Add(-time.Second),
	}}
	var edited bool
	app.Telegram.HTTPClient = &http.Client{Transport: anchorKeyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/botTOKEN/editMessageReplyMarkup" {
			return nil, fmt.Errorf("unexpected path %s", req.URL.Path)
		}
		edited = true
		return anchorKeyJSONResponse(`{"message_id":72,"chat":{"id":100}}`), nil
	})}
	app.expireKeyComposer(context.Background())
	if !edited || len(app.keyConfirmations) != 0 || len(app.keyPromptSessions) != 0 {
		t.Fatalf("expired state: edited=%v confirmations=%d workflows=%d", edited, len(app.keyConfirmations), len(app.keyPromptSessions))
	}
}

func TestExpiredKeyPromptDeletesForceReplyMessage(t *testing.T) {
	app, runner, _ := newAnchorKeyTestApp(t)
	session, _ := app.Store.FindSession(1)
	ref := keyPromptRef{ChatID: 100, MessageID: 71}
	app.keyPrompts = map[keyPromptRef]keyPrompt{ref: {
		Token: "workflow", Session: session, UserID: 42, ExpiresAt: time.Now().Add(-time.Second),
	}}
	app.keyPromptSessions = map[int]keyWorkflow{1: {
		Token: "workflow", ExpiresAt: time.Now().Add(-time.Second),
	}}
	var deleted bool
	app.Telegram.HTTPClient = &http.Client{Transport: anchorKeyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/botTOKEN/deleteMessage":
			deleted = true
			return anchorKeyJSONResponse(`true`), nil
		case "/botTOKEN/sendMessage":
			return anchorKeyJSONResponse(`{"message_id":90,"chat":{"id":100}}`), nil
		default:
			return nil, fmt.Errorf("unexpected path %s", req.URL.Path)
		}
	})}
	app.expireKeyComposer(context.Background())
	if !deleted || len(app.keyPrompts) != 0 || len(app.keyPromptSessions) != 0 {
		t.Fatalf("expired prompt state: deleted=%v prompts=%d workflows=%d", deleted, len(app.keyPrompts), len(app.keyPromptSessions))
	}
	status := app.handleUpdate(context.Background(), telegram.Update{Message: &telegram.Message{
		MessageID: 80, From: &telegram.User{ID: 42}, Chat: telegram.Chat{ID: 100},
		Text: "Enter", ReplyToMessage: &telegram.Message{MessageID: 71},
	}})
	if status != "key_prompt_stale" || len(runner.calls) != 0 {
		t.Fatalf("late prompt reply status=%q tmux=%#v", status, runner.calls)
	}
}

func TestInvalidKeyConfirmationAnswersBeforeRetiringControls(t *testing.T) {
	app, _, _ := newAnchorKeyTestApp(t)
	session, _ := app.Store.FindSession(1)
	token := "0123456789abcdef"
	app.keyPromptSessions = map[int]keyWorkflow{1: {
		Token: "workflow", ExpiresAt: time.Now().Add(-time.Second),
	}}
	app.keyConfirmations = map[string]keyConfirmation{token: {
		WorkflowToken: "workflow", Session: session, UserID: 42, ChatID: 100, MessageID: 72,
		ExpiresAt: time.Now().Add(-time.Second),
	}}
	var paths []string
	app.Telegram.HTTPClient = &http.Client{Transport: anchorKeyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.Path)
		switch req.URL.Path {
		case "/botTOKEN/answerCallbackQuery":
			return anchorKeyJSONResponse(`true`), nil
		case "/botTOKEN/editMessageReplyMarkup":
			return anchorKeyJSONResponse(`{"message_id":72,"chat":{"id":100}}`), nil
		default:
			return nil, fmt.Errorf("unexpected path %s", req.URL.Path)
		}
	})}
	status := app.confirmKeys(context.Background(), telegram.CallbackQuery{
		ID: "expired", From: telegram.User{ID: 42},
		Message: &telegram.Message{MessageID: 72, Chat: telegram.Chat{ID: 100}},
	}, token)
	if status != "callback_user_error" {
		t.Fatalf("status = %q", status)
	}
	want := []string{"/botTOKEN/answerCallbackQuery", "/botTOKEN/editMessageReplyMarkup"}
	if fmt.Sprint(paths) != fmt.Sprint(want) {
		t.Fatalf("Telegram call order = %v, want %v", paths, want)
	}
}

func TestKeyCleanupBatchSharesOneDeadline(t *testing.T) {
	app, _, _ := newAnchorKeyTestApp(t)
	var deadlines []time.Time
	app.Telegram.HTTPClient = &http.Client{Transport: anchorKeyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		deadline, ok := req.Context().Deadline()
		if !ok {
			return nil, fmt.Errorf("cleanup request has no deadline")
		}
		deadlines = append(deadlines, deadline)
		switch req.URL.Path {
		case "/botTOKEN/deleteMessage":
			return anchorKeyJSONResponse(`true`), nil
		case "/botTOKEN/editMessageReplyMarkup":
			return anchorKeyJSONResponse(`{"message_id":72,"chat":{"id":100}}`), nil
		default:
			return nil, fmt.Errorf("unexpected path %s", req.URL.Path)
		}
	})}
	app.cleanupKeyMessages(context.Background(), keyMessageRetirements{
		Prompts:       []keyMessageRef{{ChatID: 100, MessageID: 71}},
		Confirmations: []keyMessageRef{{ChatID: 100, MessageID: 72}},
	})
	if len(deadlines) != 2 || !deadlines[0].Equal(deadlines[1]) {
		t.Fatalf("cleanup deadlines = %v, want one shared batch deadline", deadlines)
	}
}

func TestStoppedWatchCannotOpenKeyComposer(t *testing.T) {
	app, _, _ := newAnchorKeyTestApp(t)
	app.KeyInterpreter = &fakeKeyInterpreter{}
	session, _ := app.Store.FindSession(1)
	session.WatchEnabled = false
	var paths []string
	app.Telegram.HTTPClient = &http.Client{Transport: anchorKeyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.Path)
		if req.URL.Path != "/botTOKEN/answerCallbackQuery" {
			return nil, fmt.Errorf("unexpected path %s", req.URL.Path)
		}
		return anchorKeyJSONResponse(`true`), nil
	})}
	status := app.openKeyComposer(context.Background(), telegram.CallbackQuery{
		ID: "keyboard", From: telegram.User{ID: 42},
		Message: &telegram.Message{MessageID: 10, Chat: telegram.Chat{ID: 100}},
	}, session)
	if status != "callback_user_error" || fmt.Sprint(paths) != "[/botTOKEN/answerCallbackQuery]" {
		t.Fatalf("status=%q Telegram paths=%v", status, paths)
	}
}

func TestNewKeyPromptSupersedesPriorWorkflow(t *testing.T) {
	app, _, _ := newAnchorKeyTestApp(t)
	session, _ := app.Store.FindSession(1)
	if _, err := app.issueKeyPrompt(100, 71, 42, session); err != nil {
		t.Fatal(err)
	}
	first, recognized := app.keyPrompts[keyPromptRef{ChatID: 100, MessageID: 71}]
	if !recognized {
		t.Fatal("first prompt missing")
	}
	retired, err := app.issueKeyPrompt(100, 72, 42, session)
	if err != nil {
		t.Fatal(err)
	}
	if len(retired.Prompts) != 1 || retired.Prompts[0] != (keyMessageRef{ChatID: 100, MessageID: 71}) {
		t.Fatalf("superseded prompt retirements = %#v", retired)
	}
	if app.keyWorkflowCurrent(session.ID, first.Token) {
		t.Fatal("superseded workflow remained current")
	}
	if app.storeKeyConfirmation("0123456789abcdef", keyConfirmation{
		WorkflowToken: first.Token, Session: session, UserID: 42, ChatID: 100, MessageID: 73,
		ExpiresAt: time.Now().Add(time.Minute),
	}) {
		t.Fatal("superseded model result created a confirmation")
	}
}

func TestConsumedKeyPromptLeavesReusableBoundedTombstone(t *testing.T) {
	app, _, _ := newAnchorKeyTestApp(t)
	session, _ := app.Store.FindSession(1)
	ref := keyPromptRef{ChatID: 100, MessageID: 71}
	if _, err := app.issueKeyPrompt(ref.ChatID, ref.MessageID, 42, session); err != nil {
		t.Fatal(err)
	}
	if _, current, recognized := app.consumeKeyPrompt(ref); !recognized || !current {
		t.Fatalf("first consumption: current=%v recognized=%v", current, recognized)
	}
	for range 2 {
		if _, current, recognized := app.consumeKeyPrompt(ref); !recognized || current {
			t.Fatalf("late consumption: current=%v recognized=%v", current, recognized)
		}
	}
	if len(app.keyPromptTombstones) != 1 || len(app.keyPromptTombstones) > maxKeyComposerWorkflows {
		t.Fatalf("prompt tombstones = %#v", app.keyPromptTombstones)
	}
}

func TestSupersededKeyPromptReplyDoesNotFallThroughToTerminalRouting(t *testing.T) {
	app, runner, _ := newAnchorKeyTestApp(t)
	session, _ := app.Store.FindSession(1)
	if _, err := app.issueKeyPrompt(100, 71, 42, session); err != nil {
		t.Fatal(err)
	}
	if _, err := app.issueKeyPrompt(100, 72, 42, session); err != nil {
		t.Fatal(err)
	}
	app.Telegram.HTTPClient = &http.Client{Transport: anchorKeyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/botTOKEN/sendMessage" {
			return nil, fmt.Errorf("unexpected path %s", req.URL.Path)
		}
		return anchorKeyJSONResponse(`{"message_id":90,"chat":{"id":100}}`), nil
	})}
	status := app.handleUpdate(context.Background(), telegram.Update{Message: &telegram.Message{
		MessageID: 80, From: &telegram.User{ID: 42}, Chat: telegram.Chat{ID: 100},
		Text: "Enter", ReplyToMessage: &telegram.Message{MessageID: 71},
	}})
	if status != "key_prompt_stale" || len(runner.calls) != 0 {
		t.Fatalf("status=%q tmux=%#v", status, runner.calls)
	}
}

func TestKeyInterpreterClarificationCannotCreateTerminalAuthority(t *testing.T) {
	app, runner, _ := newAnchorKeyTestApp(t)
	app.KeyInterpreter = &fakeKeyInterpreter{proposal: keyseq.Proposal{Kind: keyseq.KindClarification}}
	app.transferSlots = make(chan struct{}, 1)
	app.transferQueue = make(chan struct{}, 1)
	session, _ := app.Store.FindSession(1)
	if _, err := app.issueKeyPrompt(100, 71, 42, session); err != nil {
		t.Fatal(err)
	}
	var messages int
	app.Telegram.HTTPClient = &http.Client{Transport: anchorKeyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/botTOKEN/sendMessage" {
			return nil, fmt.Errorf("unexpected path %s", req.URL.Path)
		}
		messages++
		return anchorKeyJSONResponse(`{"message_id":90,"chat":{"id":100}}`), nil
	})}
	status := app.handleUpdate(context.Background(), telegram.Update{Message: &telegram.Message{
		MessageID: 80, From: &telegram.User{ID: 42}, Chat: telegram.Chat{ID: 100},
		Text: "close it", ReplyToMessage: &telegram.Message{MessageID: 71},
	}})
	if status != "key_prompt_ok" {
		t.Fatalf("status = %q", status)
	}
	app.transferWG.Wait()
	if len(runner.calls) != 0 || len(app.keyConfirmations) != 0 || messages != 1 {
		t.Fatalf("clarification state: tmux=%#v confirmations=%d messages=%d", runner.calls, len(app.keyConfirmations), messages)
	}
}

type fakeKeyInterpreter struct {
	proposal    keyseq.Proposal
	err         error
	description string
}

func (f *fakeKeyInterpreter) InterpretKeys(_ context.Context, description string) (keyseq.Proposal, error) {
	f.description = description
	return f.proposal, f.err
}
