package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
)

const closeConfirmationTTL = 2 * time.Minute
const maxCloseConfirmations = 32

type closeConfirmation struct {
	SessionID int
	ExpiresAt time.Time
}

func (a *App) handleCallback(ctx context.Context, cb telegram.CallbackQuery) string {
	if cb.From.ID != a.Config.TelegramAllowedUserID || cb.Message == nil || cb.Message.Chat.ID != a.Config.TelegramChatID {
		if !a.answerCallback(ctx, cb.ID, "not authorized") {
			return "rejected_unauthorized_callback_answer_failed"
		}
		return "rejected_unauthorized_callback"
	}
	parts := strings.SplitN(cb.Data, ":", 2)
	if len(parts) != 2 {
		a.answerCallback(ctx, cb.ID, "unknown action")
		return "skipped_unknown_callback"
	}
	switch parts[0] {
	case "refresh":
		id, err := strconv.Atoi(parts[1])
		if err != nil {
			a.answerCallback(ctx, cb.ID, "bad session id")
			return "failed_bad_callback_id"
		}
		ts, ok := a.Store.FindSession(id)
		if !ok {
			a.answerCallback(ctx, cb.ID, "session not found")
			return "callback_user_error"
		}
		if ts.State == state.TerminalLost || ts.State == state.TerminalClosed {
			a.answerCallback(ctx, cb.ID, "session is "+string(ts.State))
			return "callback_user_error"
		}
		if !a.answerCallback(ctx, cb.ID, "refreshing") {
			return "callback_telegram_failed"
		}
		a.clearHaikuCaptureHistory(id)
		a.queueRefresh(id, true, 0)
		return "callback_ok"
	case "key":
		id, preset, ok := parseKeyCallback(parts[1])
		if !ok {
			a.answerCallback(ctx, cb.ID, "bad key")
			return "failed_bad_callback_key"
		}
		action, ok := anchorKeyAction(preset)
		if !ok {
			a.answerCallback(ctx, cb.ID, "unknown key")
			return "failed_unknown_callback_key"
		}
		result := a.sendKeyGroups(ctx, id, action.Groups, action.Label, action.Delay)
		if !a.answerCallback(ctx, cb.ID, result.Message) {
			return "callback_telegram_failed"
		}
		return result.status("callback")
	case "watch":
		id, err := strconv.Atoi(parts[1])
		if err != nil {
			a.answerCallback(ctx, cb.ID, "bad session id")
			return "failed_bad_callback_id"
		}
		result := a.watchSession(ctx, id, cb.Message.MessageID)
		if !a.answerCallback(ctx, cb.ID, result.Message) {
			return "callback_telegram_failed"
		}
		return result.status("callback")
	case "close":
		id, err := strconv.Atoi(parts[1])
		if err != nil {
			a.answerCallback(ctx, cb.ID, "bad session id")
			return "failed_bad_callback_id"
		}
		ts, ok := a.Store.FindSession(id)
		if !ok {
			a.answerCallback(ctx, cb.ID, "session not found")
			return "callback_user_error"
		}
		token, err := a.issueCloseConfirmation(id)
		if err != nil {
			a.answerCallback(ctx, cb.ID, "could not create confirmation")
			return "callback_state_failed"
		}
		if _, err := a.Telegram.SendMessage(ctx, cb.Message.Chat.ID, closeConfirmationText(ts), cb.Message.MessageID, closeConfirmationMarkup(token)); err != nil {
			a.consumeCloseConfirmation(token)
			a.answerCallback(ctx, cb.ID, "could not open confirmation")
			return "callback_telegram_failed"
		}
		if !a.answerCallback(ctx, cb.ID, "confirm below") {
			return "callback_telegram_failed"
		}
		return "callback_ok"
	case "close-confirm":
		confirmation, ok := a.consumeCloseConfirmation(parts[1])
		if !ok {
			a.answerCallback(ctx, cb.ID, "confirmation expired")
			return "callback_user_error"
		}
		result := a.closeSession(ctx, confirmation.SessionID)
		if !a.answerCallback(ctx, cb.ID, result.Message) {
			return "callback_telegram_failed"
		}
		return result.status("callback")
	case "close-cancel":
		a.consumeCloseConfirmation(parts[1])
		return callbackAnswerStatus(a.answerCallback(ctx, cb.ID, "canceled"), "callback_ok")
	case "attach":
		msg := *cb.Message
		msg.From = &cb.From
		result := a.attachTarget(ctx, msg, parts[1])
		if !a.answerCallback(ctx, cb.ID, result.Message) {
			return "callback_telegram_failed"
		}
		return result.status("callback")
	default:
		a.answerCallback(ctx, cb.ID, "unknown action")
		return "skipped_unknown_callback"
	}
}

func callbackAnswerStatus(answered bool, status string) string {
	if !answered {
		return "callback_telegram_failed"
	}
	return status
}

func (a *App) answerCallback(ctx context.Context, id, text string) bool {
	if strings.TrimSpace(id) == "" || a.Telegram == nil {
		return false
	}
	if err := a.Telegram.AnswerCallback(ctx, id, text); err != nil {
		_ = a.audit("telegram.callback_answer", "failed", map[string]any{"error": err.Error()})
		return false
	}
	return true
}

func closeConfirmationMarkup(token string) *telegram.InlineKeyboardMarkup {
	return &telegram.InlineKeyboardMarkup{InlineKeyboard: [][]telegram.InlineKeyboardButton{{
		{Text: "Confirm", CallbackData: "close-confirm:" + token},
		{Text: "Cancel", CallbackData: "close-cancel:" + token},
	}}}
}

func (a *App) issueCloseConfirmation(sessionID int) (string, error) {
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	now := time.Now()
	token := hex.EncodeToString(random)
	a.closeConfirmMu.Lock()
	defer a.closeConfirmMu.Unlock()
	if a.closeConfirms == nil {
		a.closeConfirms = map[string]closeConfirmation{}
	}
	for key, confirmation := range a.closeConfirms {
		if !confirmation.ExpiresAt.After(now) {
			delete(a.closeConfirms, key)
		}
	}
	if len(a.closeConfirms) >= maxCloseConfirmations {
		var oldestKey string
		var oldestExpiry time.Time
		for key, confirmation := range a.closeConfirms {
			if oldestKey == "" || confirmation.ExpiresAt.Before(oldestExpiry) {
				oldestKey = key
				oldestExpiry = confirmation.ExpiresAt
			}
		}
		delete(a.closeConfirms, oldestKey)
	}
	a.closeConfirms[token] = closeConfirmation{SessionID: sessionID, ExpiresAt: now.Add(closeConfirmationTTL)}
	return token, nil
}

func (a *App) consumeCloseConfirmation(token string) (closeConfirmation, bool) {
	a.closeConfirmMu.Lock()
	defer a.closeConfirmMu.Unlock()
	confirmation, ok := a.closeConfirms[token]
	delete(a.closeConfirms, token)
	if !ok || !confirmation.ExpiresAt.After(time.Now()) {
		return closeConfirmation{}, false
	}
	return confirmation, true
}

type anchorKeyPreset struct {
	Label  string
	Groups [][]string
	Delay  time.Duration
}

func parseKeyCallback(value string) (int, string, bool) {
	idText, preset, ok := strings.Cut(value, ":")
	if !ok {
		return 0, "", false
	}
	id, err := strconv.Atoi(idText)
	if err != nil || id <= 0 || strings.TrimSpace(preset) == "" {
		return 0, "", false
	}
	return id, preset, true
}

func anchorKeyAction(preset string) (anchorKeyPreset, bool) {
	switch preset {
	case "esc":
		return anchorKeyPreset{Label: "Esc", Groups: [][]string{{"Escape"}}}, true
	case "esc2":
		return anchorKeyPreset{Label: "Escx2", Groups: [][]string{{"Escape"}, {"Escape"}}, Delay: 500 * time.Millisecond}, true
	case "ctrl-c":
		return anchorKeyPreset{Label: "^C", Groups: [][]string{{"C-c"}}}, true
	case "ctrl-d":
		return anchorKeyPreset{Label: "^D", Groups: [][]string{{"C-d"}}}, true
	case "enter":
		return anchorKeyPreset{Label: "Enter", Groups: [][]string{{"Enter"}}}, true
	default:
		return anchorKeyPreset{}, false
	}
}
