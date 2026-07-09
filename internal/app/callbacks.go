package app

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/idolum-ai/engram/internal/telegram"
)

func (a *App) handleCallback(ctx context.Context, cb telegram.CallbackQuery) string {
	if cb.From.ID != a.Config.TelegramAllowedUserID || cb.Message == nil || cb.Message.Chat.ID != a.Config.TelegramChatID {
		return "rejected_unauthorized_callback"
	}
	parts := strings.SplitN(cb.Data, ":", 2)
	if len(parts) != 2 {
		_ = a.Telegram.AnswerCallback(ctx, cb.ID, "unknown action")
		return "skipped_unknown_callback"
	}
	switch parts[0] {
	case "refresh":
		id, err := strconv.Atoi(parts[1])
		if err != nil {
			_ = a.Telegram.AnswerCallback(ctx, cb.ID, "bad session id")
			return "failed_bad_callback_id"
		}
		_ = a.Telegram.AnswerCallback(ctx, cb.ID, "refreshing")
		a.clearHaikuCaptureHistory(id)
		a.queueRefresh(id, true, 0)
	case "key":
		id, preset, ok := parseKeyCallback(parts[1])
		if !ok {
			_ = a.Telegram.AnswerCallback(ctx, cb.ID, "bad key")
			return "failed_bad_callback_key"
		}
		action, ok := anchorKeyAction(preset)
		if !ok {
			_ = a.Telegram.AnswerCallback(ctx, cb.ID, "unknown key")
			return "failed_unknown_callback_key"
		}
		_ = a.Telegram.AnswerCallback(ctx, cb.ID, "sent "+action.Label)
		a.sendKeyGroups(ctx, id, action.Groups, action.Label, action.Delay)
	case "watch":
		id, err := strconv.Atoi(parts[1])
		if err != nil {
			_ = a.Telegram.AnswerCallback(ctx, cb.ID, "bad session id")
			return "failed_bad_callback_id"
		}
		_ = a.Telegram.AnswerCallback(ctx, cb.ID, "watching")
		a.watchSession(ctx, id, cb.Message.MessageID)
	case "close":
		id, err := strconv.Atoi(parts[1])
		if err != nil {
			_ = a.Telegram.AnswerCallback(ctx, cb.ID, "bad session id")
			return "failed_bad_callback_id"
		}
		_ = a.Telegram.AnswerCallback(ctx, cb.ID, "closing")
		a.closeSession(ctx, id)
	case "attach":
		_ = a.Telegram.AnswerCallback(ctx, cb.ID, "attaching")
		msg := *cb.Message
		msg.From = &cb.From
		a.attachTarget(ctx, msg, parts[1])
	default:
		_ = a.Telegram.AnswerCallback(ctx, cb.ID, "unknown action")
		return "skipped_unknown_callback"
	}
	return "handled_callback"
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
