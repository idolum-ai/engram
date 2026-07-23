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

type keyWorkflow struct {
	Token     string
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

type keyMessageRef struct {
	ChatID    int64
	MessageID int
}

type keyMessageRetirements struct {
	Prompts       []keyMessageRef
	Confirmations []keyMessageRef
}

func (a *App) openKeyComposer(ctx context.Context, cb telegram.CallbackQuery, session state.TerminalSession) string {
	if a.KeyInterpreter == nil || session.State != state.TerminalRunning || !session.WatchEnabled ||
		session.Collapsed || session.RetiringAnchorMessageID != 0 {
		a.answerCallback(ctx, cb.ID, "keyboard composer is unavailable")
		return "callback_user_error"
	}
	if !a.answerCallback(ctx, cb.ID, "opening keyboard") {
		return "callback_telegram_failed"
	}
	title := keyComposerTitle(a.redactText(firstNonEmpty(session.Title, "terminal")))
	text := fmt.Sprintf("Describe the exact keys to press in [%d] %s.", session.ID, title)
	prompt, err := a.Telegram.SendForceReply(ctx, cb.Message.Chat.ID, text, cb.Message.MessageID, "up 3 times, Enter, Ctrl+C")
	if err != nil {
		_ = a.audit("keys.prompt", "send_failed", map[string]any{"session_id": session.ID, "error": err.Error()})
		return "callback_telegram_failed"
	}
	retired, err := a.issueKeyPrompt(prompt.Chat.ID, prompt.MessageID, cb.From.ID, session)
	if err != nil {
		_ = a.Telegram.DeleteMessage(ctx, prompt.Chat.ID, prompt.MessageID)
		return "callback_state_failed"
	}
	a.cleanupKeyMessages(ctx, retired)
	return "callback_ok"
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
	workflowCtx, workflowCancel := context.WithDeadline(ctx, prompt.ExpiresAt)
	defer workflowCancel()
	if !acquireSlot(workflowCtx, a.guideSlots) {
		a.finishKeyWorkflow(prompt.Session.ID, prompt.Token)
		a.reply(ctx, msg, "Keyboard interpretation stopped before it completed; try again.")
		return
	}
	defer releaseSlot(a.guideSlots)
	if workflowCtx.Err() != nil || !a.keyWorkflowCurrent(prompt.Session.ID, prompt.Token) {
		a.finishKeyWorkflow(prompt.Session.ID, prompt.Token)
		a.reply(ctx, msg, "That keyboard prompt expired or was superseded. Open ⌨️ from the latest session card.")
		return
	}
	modelCtx, cancel := context.WithTimeout(workflowCtx, keyComposerModelTimeout)
	defer cancel()
	proposal, err := a.KeyInterpreter.InterpretKeys(modelCtx, description)
	if err != nil {
		a.finishKeyWorkflow(prompt.Session.ID, prompt.Token)
		_ = a.audit("keys.interpret", "failed", map[string]any{"session_id": prompt.Session.ID})
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
		_ = a.audit("keys.interpret", "rejected", map[string]any{"session_id": prompt.Session.ID})
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
	title := keyComposerTitle(a.redactText(firstNonEmpty(prompt.Session.Title, "terminal")))
	text := keyConfirmationText(prompt.Session.ID, title, proposal)
	confirmationMessage, err := a.Telegram.SendMessage(ctx, msg.Chat.ID, text, msg.MessageID, telegram.KeyConfirmationMarkup(token))
	if err != nil {
		a.finishKeyWorkflow(prompt.Session.ID, prompt.Token)
		_ = a.audit("keys.confirmation", "send_failed", map[string]any{"session_id": prompt.Session.ID})
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
		ExpiresAt:     prompt.ExpiresAt,
	}
	if !a.storeKeyConfirmation(token, confirmation) {
		_, _ = a.Telegram.EditReplyMarkup(ctx, confirmation.ChatID, confirmation.MessageID, telegram.ClearMarkup())
	}
}

func (a *App) confirmKeys(ctx context.Context, cb telegram.CallbackQuery, token string) string {
	confirmation, ok := a.consumeKeyConfirmation(token, cb)
	if !ok {
		answered := a.answerCallback(ctx, cb.ID, "confirmation expired")
		a.retireKeyConfirmation(ctx, cb.Message)
		if !answered {
			return "callback_telegram_failed"
		}
		return "callback_user_error"
	}
	msg := *cb.Message
	msg.From = &cb.From
	proceed := make(chan bool, 1)
	if !a.queueTransferWithDrop(func(workerCtx context.Context) {
		if <-proceed {
			a.executeKeyConfirmation(workerCtx, msg, confirmation)
		} else {
			a.retireKeyConfirmation(workerCtx, &msg)
		}
	}, func(workerCtx context.Context) {
		if <-proceed {
			a.retireKeyConfirmation(workerCtx, &msg)
			a.replyTransferFailure(workerCtx, msg, "Key delivery stopped before it completed; open ⌨️ and try again after Engram restarts.")
		} else {
			a.retireKeyConfirmation(workerCtx, &msg)
		}
	}) {
		answered := a.answerCallback(ctx, cb.ID, "Engram is busy; sequence canceled")
		a.retireKeyConfirmation(ctx, &msg)
		if !answered {
			return "callback_telegram_failed"
		}
		return "callback_state_failed"
	}
	answered := a.answerCallback(ctx, cb.ID, "sending keys")
	proceed <- answered
	if !answered {
		return "callback_telegram_failed"
	}
	return "callback_ok"
}

func (a *App) cancelKeys(ctx context.Context, cb telegram.CallbackQuery, token string) string {
	_, ok := a.consumeKeyConfirmation(token, cb)
	if !ok {
		answered := a.answerCallback(ctx, cb.ID, "confirmation expired")
		a.retireKeyConfirmation(ctx, cb.Message)
		if !answered {
			return "callback_telegram_failed"
		}
		return "callback_user_error"
	}
	if !a.answerCallback(ctx, cb.ID, "canceled") {
		return "callback_telegram_failed"
	}
	a.retireKeyConfirmation(ctx, cb.Message)
	return "callback_ok"
}

func (a *App) executeKeyConfirmation(ctx context.Context, msg telegram.Message, confirmation keyConfirmation) {
	if !confirmation.ExpiresAt.After(time.Now()) {
		a.retireKeyConfirmation(ctx, &msg)
		a.reply(ctx, msg, "That keyboard confirmation expired before delivery. Open ⌨️ and try again.")
		_ = a.audit("keys.confirm", "expired", map[string]any{"session_id": confirmation.Session.ID})
		return
	}
	deliveryCtx, cancel := context.WithDeadline(ctx, confirmation.ExpiresAt)
	defer cancel()
	groups := make([][]string, len(confirmation.Plan.Groups))
	delays := make([]time.Duration, len(confirmation.Plan.Groups))
	for index, group := range confirmation.Plan.Groups {
		groups[index] = append([]string(nil), group.Keys...)
		delays[index] = group.DelayAfter
	}
	result := a.sendKeyGroupsForAnchorExpected(deliveryCtx, confirmation.Session, groups, keyseq.Format(confirmation.Proposal), delays)
	a.retireKeyConfirmation(ctx, &msg)
	if !result.OK() {
		a.reply(ctx, msg, result.Message)
	}
	_ = a.audit("keys.confirm", result.status("keys"), map[string]any{
		"session_id":  confirmation.Session.ID,
		"event_count": confirmation.Plan.EventCount,
	})
}

func (a *App) retireKeyConfirmation(ctx context.Context, message *telegram.Message) {
	if message == nil || a.Telegram == nil {
		return
	}
	a.cleanupKeyMessages(ctx, keyMessageRetirements{Confirmations: []keyMessageRef{{
		ChatID: message.Chat.ID, MessageID: message.MessageID,
	}}})
}

func (a *App) cleanupKeyMessages(ctx context.Context, retired keyMessageRetirements) {
	if a.Telegram == nil || len(retired.Prompts)+len(retired.Confirmations) == 0 {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	for _, ref := range retired.Prompts {
		_ = a.Telegram.DeleteMessage(cleanupCtx, ref.ChatID, ref.MessageID)
	}
	for _, ref := range retired.Confirmations {
		_, _ = a.Telegram.EditReplyMarkup(cleanupCtx, ref.ChatID, ref.MessageID, telegram.ClearMarkup())
	}
}

func (a *App) issueKeyPrompt(chatID int64, messageID int, userID int64, session state.TerminalSession) (keyMessageRetirements, error) {
	token, err := randomKeyToken()
	if err != nil {
		return keyMessageRetirements{}, err
	}
	now := time.Now()
	ref := keyPromptRef{ChatID: chatID, MessageID: messageID}
	a.keyComposerMu.Lock()
	defer a.keyComposerMu.Unlock()
	if a.keyPrompts == nil {
		a.keyPrompts = map[keyPromptRef]keyPrompt{}
	}
	if a.keyPromptTombstones == nil {
		a.keyPromptTombstones = map[keyPromptRef]time.Time{}
	}
	if a.keyPromptSessions == nil {
		a.keyPromptSessions = map[int]keyWorkflow{}
	}
	if a.keyConfirmations == nil {
		a.keyConfirmations = map[string]keyConfirmation{}
	}
	var retired keyMessageRetirements
	for existingRef, prompt := range a.keyPrompts {
		if prompt.Session.ID == session.ID {
			retired.Prompts = append(retired.Prompts, keyMessageRef{ChatID: existingRef.ChatID, MessageID: existingRef.MessageID})
			delete(a.keyPrompts, existingRef)
			a.addKeyPromptTombstoneLocked(existingRef, now)
		}
	}
	for existingToken, confirmation := range a.keyConfirmations {
		if confirmation.Session.ID == session.ID {
			retired.Confirmations = append(retired.Confirmations, keyMessageRef{ChatID: confirmation.ChatID, MessageID: confirmation.MessageID})
			delete(a.keyConfirmations, existingToken)
		}
	}
	retired.append(a.enforceKeyComposerLimitLocked())
	a.keyPrompts[ref] = keyPrompt{
		Token: token, Session: session, UserID: userID, ExpiresAt: now.Add(keyComposerTTL),
	}
	delete(a.keyPromptTombstones, ref)
	a.keyPromptSessions[session.ID] = keyWorkflow{Token: token, ExpiresAt: now.Add(keyComposerTTL)}
	return retired, nil
}

func (a *App) consumeKeyPrompt(ref keyPromptRef) (keyPrompt, bool, bool) {
	a.keyComposerMu.Lock()
	defer a.keyComposerMu.Unlock()
	prompt, ok := a.keyPrompts[ref]
	delete(a.keyPrompts, ref)
	if !ok {
		expiresAt, stale := a.keyPromptTombstones[ref]
		if stale && expiresAt.After(time.Now()) {
			return keyPrompt{}, false, true
		}
		delete(a.keyPromptTombstones, ref)
		return keyPrompt{}, false, false
	}
	a.addKeyPromptTombstoneLocked(ref, time.Now())
	workflow := a.keyPromptSessions[prompt.Session.ID]
	current := prompt.ExpiresAt.After(time.Now()) && workflow.Token == prompt.Token && workflow.ExpiresAt.After(time.Now())
	return prompt, current, true
}

func (a *App) storeKeyConfirmation(token string, confirmation keyConfirmation) bool {
	a.keyComposerMu.Lock()
	defer a.keyComposerMu.Unlock()
	workflow := a.keyPromptSessions[confirmation.Session.ID]
	if workflow.Token != confirmation.WorkflowToken || !workflow.ExpiresAt.After(time.Now()) {
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
		a.keyPromptSessions[confirmation.Session.ID].Token != confirmation.WorkflowToken {
		return keyConfirmation{}, false
	}
	delete(a.keyConfirmations, token)
	if a.keyPromptSessions[confirmation.Session.ID].Token == confirmation.WorkflowToken {
		delete(a.keyPromptSessions, confirmation.Session.ID)
	}
	return confirmation, true
}

func (a *App) keyWorkflowCurrent(sessionID int, token string) bool {
	a.keyComposerMu.Lock()
	defer a.keyComposerMu.Unlock()
	workflow := a.keyPromptSessions[sessionID]
	return workflow.Token == token && workflow.ExpiresAt.After(time.Now())
}

func (a *App) keyTargetCurrent(expected state.TerminalSession) bool {
	current, ok := a.Store.FindSession(expected.ID)
	return ok && current.State == state.TerminalRunning && current.WatchEnabled && !current.Collapsed &&
		current.AnchorChatID == expected.AnchorChatID && current.AnchorMessageID == expected.AnchorMessageID &&
		sameTerminalBinding(current, expected)
}

func (a *App) finishKeyWorkflow(sessionID int, token string) {
	a.keyComposerMu.Lock()
	defer a.keyComposerMu.Unlock()
	if a.keyPromptSessions[sessionID].Token == token {
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

func (a *App) expireKeyComposer(ctx context.Context) {
	now := time.Now()
	a.keyComposerMu.Lock()
	var retired keyMessageRetirements
	for ref, prompt := range a.keyPrompts {
		if !prompt.ExpiresAt.After(now) {
			retired.Prompts = append(retired.Prompts, keyMessageRef{ChatID: ref.ChatID, MessageID: ref.MessageID})
			delete(a.keyPrompts, ref)
			a.addKeyPromptTombstoneLocked(ref, now)
			if a.keyPromptSessions[prompt.Session.ID].Token == prompt.Token {
				delete(a.keyPromptSessions, prompt.Session.ID)
			}
		}
	}
	for token, confirmation := range a.keyConfirmations {
		workflow := a.keyPromptSessions[confirmation.Session.ID]
		if !confirmation.ExpiresAt.After(now) || workflow.Token != confirmation.WorkflowToken || !workflow.ExpiresAt.After(now) {
			retired.Confirmations = append(retired.Confirmations, keyMessageRef{ChatID: confirmation.ChatID, MessageID: confirmation.MessageID})
			delete(a.keyConfirmations, token)
			if a.keyPromptSessions[confirmation.Session.ID].Token == confirmation.WorkflowToken {
				delete(a.keyPromptSessions, confirmation.Session.ID)
			}
		}
	}
	for sessionID, workflow := range a.keyPromptSessions {
		if !workflow.ExpiresAt.After(now) {
			delete(a.keyPromptSessions, sessionID)
		}
	}
	for ref, expiresAt := range a.keyPromptTombstones {
		if !expiresAt.After(now) {
			delete(a.keyPromptTombstones, ref)
		}
	}
	a.keyComposerMu.Unlock()
	a.cleanupKeyMessages(ctx, retired)
}

func (a *App) enforceKeyComposerLimitLocked() keyMessageRetirements {
	var retired keyMessageRetirements
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
			retired.Prompts = append(retired.Prompts, keyMessageRef{ChatID: oldestPromptRef.ChatID, MessageID: oldestPromptRef.MessageID})
			delete(a.keyPrompts, oldestPromptRef)
			a.addKeyPromptTombstoneLocked(oldestPromptRef, time.Now())
			if a.keyPromptSessions[oldestPrompt.Session.ID].Token == oldestPrompt.Token {
				delete(a.keyPromptSessions, oldestPrompt.Session.ID)
			}
			continue
		}
		if oldestConfirmationToken != "" {
			retired.Confirmations = append(retired.Confirmations, keyMessageRef{ChatID: oldestConfirmation.ChatID, MessageID: oldestConfirmation.MessageID})
			delete(a.keyConfirmations, oldestConfirmationToken)
			if a.keyPromptSessions[oldestConfirmation.Session.ID].Token == oldestConfirmation.WorkflowToken {
				delete(a.keyPromptSessions, oldestConfirmation.Session.ID)
			}
			continue
		}
		return retired
	}
	return retired
}

func randomKeyToken() (string, error) {
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return hex.EncodeToString(random), nil
}

func keyComposerTitle(title string) string {
	title = strings.Join(strings.Fields(title), " ")
	return headUTF8(firstNonEmpty(title, "terminal"), 80)
}

func keyConfirmationText(sessionID int, title string, proposal keyseq.Proposal) string {
	return fmt.Sprintf("Keys:\n%s\n\nTarget: [%d] %s", keyseq.Format(proposal), sessionID, keyComposerTitle(title))
}

func (r *keyMessageRetirements) append(other keyMessageRetirements) {
	r.Prompts = append(r.Prompts, other.Prompts...)
	r.Confirmations = append(r.Confirmations, other.Confirmations...)
}

func (a *App) addKeyPromptTombstoneLocked(ref keyPromptRef, now time.Time) {
	if a.keyPromptTombstones == nil {
		a.keyPromptTombstones = map[keyPromptRef]time.Time{}
	}
	a.keyPromptTombstones[ref] = now.Add(keyComposerTTL)
	for len(a.keyPromptTombstones) > maxKeyComposerWorkflows {
		var oldestRef keyPromptRef
		var oldestExpiry time.Time
		for candidate, expiry := range a.keyPromptTombstones {
			if oldestExpiry.IsZero() || expiry.Before(oldestExpiry) {
				oldestRef, oldestExpiry = candidate, expiry
			}
		}
		delete(a.keyPromptTombstones, oldestRef)
	}
}
