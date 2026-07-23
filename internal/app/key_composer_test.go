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
		{name: "restart loses memory authority", cbID: 72, mutate: func(a *App, _ string) {
			a.keyConfirmations = map[string]keyConfirmation{}
			a.keyPromptSessions = map[int]string{}
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
			app.keyPromptSessions = map[int]string{1: "workflow"}
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

func TestNewKeyPromptSupersedesPriorWorkflow(t *testing.T) {
	app, _, _ := newAnchorKeyTestApp(t)
	session, _ := app.Store.FindSession(1)
	if err := app.issueKeyPrompt(100, 71, 42, session); err != nil {
		t.Fatal(err)
	}
	first, current, recognized := app.consumeKeyPrompt(keyPromptRef{ChatID: 100, MessageID: 71})
	if !recognized || !current {
		t.Fatal("first prompt missing")
	}
	if err := app.issueKeyPrompt(100, 72, 42, session); err != nil {
		t.Fatal(err)
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

func TestSupersededKeyPromptReplyDoesNotFallThroughToTerminalRouting(t *testing.T) {
	app, runner, _ := newAnchorKeyTestApp(t)
	session, _ := app.Store.FindSession(1)
	if err := app.issueKeyPrompt(100, 71, 42, session); err != nil {
		t.Fatal(err)
	}
	if err := app.issueKeyPrompt(100, 72, 42, session); err != nil {
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
	if err := app.issueKeyPrompt(100, 71, 42, session); err != nil {
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
