package app

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/idolum-ai/engram/internal/telegram"
)

func (a *App) handleCallback(ctx context.Context, cb telegram.CallbackQuery) string {
	if cb.From.ID != a.Config.TelegramAllowedUserID || cb.Message == nil || cb.Message.Chat.ID != a.Config.TelegramChatID {
		a.answerCallback(ctx, cb.ID, "not authorized")
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
		if _, ok := a.Store.FindSession(id); !ok {
			a.answerCallback(ctx, cb.ID, "session not found")
			return "callback_user_error"
		}
		a.answerCallback(ctx, cb.ID, "refreshing")
		a.clearHaikuCaptureHistory(id)
		a.queueRefresh(id, true, 0)
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
		a.answerCallback(ctx, cb.ID, result.Message)
		return result.status("callback")
	case "watch":
		id, err := strconv.Atoi(parts[1])
		if err != nil {
			a.answerCallback(ctx, cb.ID, "bad session id")
			return "failed_bad_callback_id"
		}
		result := a.watchSession(ctx, id, cb.Message.MessageID)
		a.answerCallback(ctx, cb.ID, result.Message)
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
		if _, err := a.Telegram.SendMessage(ctx, cb.Message.Chat.ID, closeConfirmationText(ts), cb.Message.MessageID, closeConfirmationMarkup(id)); err != nil {
			a.answerCallback(ctx, cb.ID, "could not open confirmation")
			return "callback_telegram_failed"
		}
		a.answerCallback(ctx, cb.ID, "confirm below")
		return "callback_ok"
	case "close-confirm":
		id, err := strconv.Atoi(parts[1])
		if err != nil {
			a.answerCallback(ctx, cb.ID, "bad session id")
			return "failed_bad_callback_id"
		}
		result := a.closeSession(ctx, id)
		a.answerCallback(ctx, cb.ID, result.Message)
		return result.status("callback")
	case "close-cancel":
		a.answerCallback(ctx, cb.ID, "canceled")
		return "callback_ok"
	case "attach":
		msg := *cb.Message
		msg.From = &cb.From
		result := a.attachTarget(ctx, msg, parts[1])
		a.answerCallback(ctx, cb.ID, result.Message)
		return result.status("callback")
	default:
		a.answerCallback(ctx, cb.ID, "unknown action")
		return "skipped_unknown_callback"
	}
	return "handled_callback"
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

func closeConfirmationMarkup(id int) *telegram.InlineKeyboardMarkup {
	return &telegram.InlineKeyboardMarkup{InlineKeyboard: [][]telegram.InlineKeyboardButton{{
		{Text: "Confirm", CallbackData: fmt.Sprintf("close-confirm:%d", id)},
		{Text: "Cancel", CallbackData: fmt.Sprintf("close-cancel:%d", id)},
	}}}
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
		return anchorKeyPreset{Label: "Esc Esc", Groups: [][]string{{"Escape"}, {"Escape"}}, Delay: 500 * time.Millisecond}, true
	case "ctrl-c":
		return anchorKeyPreset{Label: "Ctrl+C", Groups: [][]string{{"C-c"}}}, true
	case "ctrl-d":
		return anchorKeyPreset{Label: "Ctrl+D", Groups: [][]string{{"C-d"}}}, true
	case "enter":
		return anchorKeyPreset{Label: "Enter", Groups: [][]string{{"Enter"}}}, true
	default:
		return anchorKeyPreset{}, false
	}
}
