package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/idolum-ai/engram/internal/keyseq"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
)

const (
	keyComposerTTL          = 2 * time.Minute
	keyComposerModelTimeout = 60 * time.Second
	maxKeyComposerWorkflows = 32
)

type keyPromptRef struct {
	ChatID    int64
	MessageID int
}

type keyPrompt struct {
	Token     string
	Session   state.TerminalSession
	UserID    int64
	ExpiresAt time.Time
}

type keyConfirmation struct {
	WorkflowToken string
	Session       state.TerminalSession
	UserID        int64
	ChatID        int64
	MessageID     int
	Proposal      keyseq.Proposal
	Plan          keyseq.Plan
	ExpiresAt     time.Time
}

func (a *App) openKeyComposer(ctx context.Context, cb telegram.CallbackQuery, session state.TerminalSession) string {
	if a.KeyInterpreter == nil || session.State != state.TerminalRunning || session.RetiringAnchorMessageID != 0 {
		a.answerCallback(ctx, cb.ID, "keyboard composer is unavailable")
		return "callback_user_error"
	}
	title := strings.Join(strings.Fields(a.redactText(firstNonEmpty(session.Title, "terminal"))), " ")
	text := fmt.Sprintf("Describe the exact keys to press in [%d] %s.", session.ID, title)
	prompt, err := a.Telegram.SendForceReply(ctx, cb.Message.Chat.ID, text, cb.Message.MessageID, "up 3 times, Enter, Ctrl+C")
	if err != nil {
		a.answerCallback(ctx, cb.ID, "could not open keyboard")
		return "callback_telegram_failed"
	}
	if err := a.issueKeyPrompt(prompt.Chat.ID, prompt.MessageID, cb.From.ID, session); err != nil {
		_ = a.Telegram.DeleteMessage(ctx, prompt.Chat.ID, prompt.MessageID)
		a.answerCallback(ctx, cb.ID, "could not open keyboard")
		return "callback_state_failed"
	}
	return callbackAnswerStatus(a.answerCallback(ctx, cb.ID, "describe the keys below"), "callback_ok")
}

func (a *App) handleKeyPromptReply(ctx context.Context, msg telegram.Message) (string, bool) {
	if msg.ReplyToMessage == nil {
		return "", false
	}
	prompt, current, recognized := a.consumeKeyPrompt(keyPromptRef{ChatID: msg.Chat.ID, MessageID: msg.ReplyToMessage.MessageID})
	if !recognized {
		return "", false
	}
	if !current {
		a.reply(ctx, msg, "That keyboard prompt expired or was superseded. Open ⌨️ from the latest session card.")
		return "key_prompt_stale", true
	}
	description := strings.TrimSpace(msg.Text)
	if description == "" {
		a.reply(ctx, msg, "Describe the physical keys as text, such as “up three times, then Enter”.")
		a.finishKeyWorkflow(prompt.Session.ID, prompt.Token)
		return "key_prompt_user_error", true
	}
	if a.KeyInterpreter == nil {
		a.reply(ctx, msg, "Keyboard interpretation is no longer available.")
		a.finishKeyWorkflow(prompt.Session.ID, prompt.Token)
		return "key_prompt_user_error", true
	}
	queued := a.queueTransferWithDrop(func(workerCtx context.Context) {
		a.interpretKeyPrompt(workerCtx, msg, prompt, description)
	}, func(workerCtx context.Context) {
		a.finishKeyWorkflow(prompt.Session.ID, prompt.Token)
		a.reply(workerCtx, msg, "Keyboard interpretation stopped before it completed; try again.")
	})
	if !queued {
		a.finishKeyWorkflow(prompt.Session.ID, prompt.Token)
		a.reply(ctx, msg, "Engram is busy; try the keyboard again.")
		return "key_prompt_busy", true
	}
	return "key_prompt_ok", true
}

func (a *App) interpretKeyPrompt(ctx context.Context, msg telegram.Message, prompt keyPrompt, description string) {
	if !acquireSlot(ctx, a.guideSlots) {
		a.finishKeyWorkflow(prompt.Session.ID, prompt.Token)
		a.reply(ctx, msg, "Keyboard interpretation stopped before it completed; try again.")
		return
	}
	defer releaseSlot(a.guideSlots)
	modelCtx, cancel := context.WithTimeout(ctx, keyComposerModelTimeout)
	defer cancel()
	proposal, err := a.KeyInterpreter.InterpretKeys(modelCtx, description)
	if err != nil {
		a.finishKeyWorkflow(prompt.Session.ID, prompt.Token)
		_ = a.audit("keys.interpret", "failed", map[string]any{"session_id": prompt.Session.ID, "error": err.Error()})
		a.reply(ctx, msg, "I could not interpret those keys. Try naming the physical keys more directly.")
		return
	}
	if proposal.Kind == keyseq.KindClarification {
		a.finishKeyWorkflow(prompt.Session.ID, prompt.Token)
		a.reply(ctx, msg, "I need explicit physical keys. Try something like “Up three times, Enter, then Ctrl+C”.")
		return
	}
	plan, err := keyseq.Compile(proposal)
	if err != nil {
		a.finishKeyWorkflow(prompt.Session.ID, prompt.Token)
		_ = a.audit("keys.interpret", "rejected", map[string]any{"session_id": prompt.Session.ID, "error": err.Error()})
		a.reply(ctx, msg, "That key sequence is not supported.")
		return
	}
	if !a.keyWorkflowCurrent(prompt.Session.ID, prompt.Token) || !a.keyTargetCurrent(prompt.Session) {
		a.finishKeyWorkflow(prompt.Session.ID, prompt.Token)
		a.reply(ctx, msg, "The session changed while I interpreted those keys. Open the keyboard from its latest card.")
		return
	}
	token, err := randomKeyToken()
	if err != nil {
		a.finishKeyWorkflow(prompt.Session.ID, prompt.Token)
		a.reply(ctx, msg, "Could not create a keyboard confirmation.")
		return
	}
	title := strings.Join(strings.Fields(a.redactText(firstNonEmpty(prompt.Session.Title, "terminal"))), " ")
	text := fmt.Sprintf("Send to [%d] %s?\n\n%s", prompt.Session.ID, title, keyseq.Format(proposal))
	confirmationMessage, err := a.Telegram.SendMessage(ctx, msg.Chat.ID, text, msg.MessageID, telegram.KeyConfirmationMarkup(token))
	if err != nil {
		a.finishKeyWorkflow(prompt.Session.ID, prompt.Token)
		_ = a.audit("keys.confirmation", "send_failed", map[string]any{"session_id": prompt.Session.ID, "error": err.Error()})
		return
	}
	confirmation := keyConfirmation{
		WorkflowToken: prompt.Token,
		Session:       prompt.Session,
		UserID:        prompt.UserID,
		ChatID:        confirmationMessage.Chat.ID,
		MessageID:     confirmationMessage.MessageID,
		Proposal:      proposal,
		Plan:          plan,
		ExpiresAt:     time.Now().Add(keyComposerTTL),
	}
	if !a.storeKeyConfirmation(token, confirmation) {
		_, _ = a.Telegram.EditReplyMarkup(ctx, confirmation.ChatID, confirmation.MessageID, telegram.ClearMarkup())
	}
}

func (a *App) confirmKeys(ctx context.Context, cb telegram.CallbackQuery, token string) string {
	confirmation, ok := a.consumeKeyConfirmation(token, cb)
	if !ok {
		a.answerCallback(ctx, cb.ID, "confirmation expired")
		return "callback_user_error"
	}
	if !a.answerCallback(ctx, cb.ID, "sending keys") {
		return "callback_telegram_failed"
	}
	groups := make([][]string, len(confirmation.Plan.Groups))
	delays := make([]time.Duration, len(confirmation.Plan.Groups))
	for index, group := range confirmation.Plan.Groups {
		groups[index] = append([]string(nil), group.Keys...)
		delays[index] = group.DelayAfter
	}
	result := a.sendKeyGroupsForAnchorExpected(ctx, confirmation.Session, groups, keyseq.Format(confirmation.Proposal), delays)
	_, _ = a.Telegram.EditReplyMarkup(ctx, confirmation.ChatID, confirmation.MessageID, telegram.ClearMarkup())
	if !result.OK() {
		msg := *cb.Message
		msg.From = &cb.From
		a.reply(ctx, msg, result.Message)
	}
	_ = a.audit("keys.confirm", result.status("keys"), map[string]any{
		"session_id":  confirmation.Session.ID,
		"event_count": confirmation.Plan.EventCount,
	})
	return result.status("callback")
}

func (a *App) cancelKeys(ctx context.Context, cb telegram.CallbackQuery, token string) string {
	confirmation, ok := a.consumeKeyConfirmation(token, cb)
	if !ok {
		a.answerCallback(ctx, cb.ID, "confirmation expired")
		return "callback_user_error"
	}
	if !a.answerCallback(ctx, cb.ID, "canceled") {
		return "callback_telegram_failed"
	}
	_, _ = a.Telegram.EditReplyMarkup(ctx, confirmation.ChatID, confirmation.MessageID, telegram.ClearMarkup())
	return "callback_ok"
}

func (a *App) issueKeyPrompt(chatID int64, messageID int, userID int64, session state.TerminalSession) error {
	token, err := randomKeyToken()
	if err != nil {
		return err
	}
	now := time.Now()
	ref := keyPromptRef{ChatID: chatID, MessageID: messageID}
	a.keyComposerMu.Lock()
	defer a.keyComposerMu.Unlock()
	a.cleanupKeyComposerLocked(now)
	if a.keyPrompts == nil {
		a.keyPrompts = map[keyPromptRef]keyPrompt{}
	}
	if a.keyPromptSessions == nil {
		a.keyPromptSessions = map[int]string{}
	}
	if a.keyConfirmations == nil {
		a.keyConfirmations = map[string]keyConfirmation{}
	}
	for existingToken, confirmation := range a.keyConfirmations {
		if confirmation.Session.ID == session.ID {
			delete(a.keyConfirmations, existingToken)
		}
	}
	a.enforceKeyComposerLimitLocked()
	a.keyPrompts[ref] = keyPrompt{
		Token: token, Session: session, UserID: userID, ExpiresAt: now.Add(keyComposerTTL),
	}
	a.keyPromptSessions[session.ID] = token
	return nil
}

func (a *App) consumeKeyPrompt(ref keyPromptRef) (keyPrompt, bool, bool) {
	a.keyComposerMu.Lock()
	defer a.keyComposerMu.Unlock()
	prompt, ok := a.keyPrompts[ref]
	delete(a.keyPrompts, ref)
	if !ok {
		return keyPrompt{}, false, false
	}
	current := prompt.ExpiresAt.After(time.Now()) && a.keyPromptSessions[prompt.Session.ID] == prompt.Token
	return prompt, current, true
}

func (a *App) storeKeyConfirmation(token string, confirmation keyConfirmation) bool {
	a.keyComposerMu.Lock()
	defer a.keyComposerMu.Unlock()
	a.cleanupKeyComposerLocked(time.Now())
	if a.keyPromptSessions[confirmation.Session.ID] != confirmation.WorkflowToken {
		return false
	}
	a.keyConfirmations[token] = confirmation
	return true
}

func (a *App) consumeKeyConfirmation(token string, cb telegram.CallbackQuery) (keyConfirmation, bool) {
	a.keyComposerMu.Lock()
	defer a.keyComposerMu.Unlock()
	confirmation, ok := a.keyConfirmations[token]
	if !ok || !confirmation.ExpiresAt.After(time.Now()) || cb.Message == nil ||
		cb.From.ID != confirmation.UserID || cb.Message.Chat.ID != confirmation.ChatID ||
		cb.Message.MessageID != confirmation.MessageID ||
		a.keyPromptSessions[confirmation.Session.ID] != confirmation.WorkflowToken {
		return keyConfirmation{}, false
	}
	delete(a.keyConfirmations, token)
	if a.keyPromptSessions[confirmation.Session.ID] == confirmation.WorkflowToken {
		delete(a.keyPromptSessions, confirmation.Session.ID)
	}
	return confirmation, true
}

func (a *App) keyWorkflowCurrent(sessionID int, token string) bool {
	a.keyComposerMu.Lock()
	defer a.keyComposerMu.Unlock()
	return a.keyPromptSessions[sessionID] == token
}

func (a *App) keyTargetCurrent(expected state.TerminalSession) bool {
	current, ok := a.Store.FindSession(expected.ID)
	return ok && current.State == state.TerminalRunning && !current.Collapsed &&
		current.AnchorChatID == expected.AnchorChatID && current.AnchorMessageID == expected.AnchorMessageID &&
		sameTerminalBinding(current, expected)
}

func (a *App) finishKeyWorkflow(sessionID int, token string) {
	a.keyComposerMu.Lock()
	defer a.keyComposerMu.Unlock()
	if a.keyPromptSessions[sessionID] == token {
		delete(a.keyPromptSessions, sessionID)
	}
	for ref, prompt := range a.keyPrompts {
		if prompt.Token == token {
			delete(a.keyPrompts, ref)
		}
	}
	for confirmationToken, confirmation := range a.keyConfirmations {
		if confirmation.WorkflowToken == token {
			delete(a.keyConfirmations, confirmationToken)
		}
	}
}

func (a *App) cleanupKeyComposerLocked(now time.Time) {
	for ref, prompt := range a.keyPrompts {
		if !prompt.ExpiresAt.After(now) {
			delete(a.keyPrompts, ref)
			if a.keyPromptSessions[prompt.Session.ID] == prompt.Token {
				delete(a.keyPromptSessions, prompt.Session.ID)
			}
		}
	}
	for token, confirmation := range a.keyConfirmations {
		if !confirmation.ExpiresAt.After(now) {
			delete(a.keyConfirmations, token)
			if a.keyPromptSessions[confirmation.Session.ID] == confirmation.WorkflowToken {
				delete(a.keyPromptSessions, confirmation.Session.ID)
			}
		}
	}
}

func (a *App) enforceKeyComposerLimitLocked() {
	for len(a.keyPrompts)+len(a.keyConfirmations) >= maxKeyComposerWorkflows {
		var oldestPromptRef keyPromptRef
		var oldestPrompt keyPrompt
		for ref, prompt := range a.keyPrompts {
			if oldestPrompt.ExpiresAt.IsZero() || prompt.ExpiresAt.Before(oldestPrompt.ExpiresAt) {
				oldestPromptRef, oldestPrompt = ref, prompt
			}
		}
		var oldestConfirmationToken string
		var oldestConfirmation keyConfirmation
		for token, confirmation := range a.keyConfirmations {
			if oldestConfirmation.ExpiresAt.IsZero() || confirmation.ExpiresAt.Before(oldestConfirmation.ExpiresAt) {
				oldestConfirmationToken, oldestConfirmation = token, confirmation
			}
		}
		if !oldestPrompt.ExpiresAt.IsZero() && (oldestConfirmation.ExpiresAt.IsZero() || oldestPrompt.ExpiresAt.Before(oldestConfirmation.ExpiresAt)) {
			delete(a.keyPrompts, oldestPromptRef)
			if a.keyPromptSessions[oldestPrompt.Session.ID] == oldestPrompt.Token {
				delete(a.keyPromptSessions, oldestPrompt.Session.ID)
			}
			continue
		}
		if oldestConfirmationToken != "" {
			delete(a.keyConfirmations, oldestConfirmationToken)
			if a.keyPromptSessions[oldestConfirmation.Session.ID] == oldestConfirmation.WorkflowToken {
				delete(a.keyPromptSessions, oldestConfirmation.Session.ID)
			}
			continue
		}
		return
	}
}

func randomKeyToken() (string, error) {
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return hex.EncodeToString(random), nil
}
