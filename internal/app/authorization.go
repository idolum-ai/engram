package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/lockfile"
	"github.com/idolum-ai/engram/internal/telegram"
	"github.com/idolum-ai/engram/internal/version"
)

type telegramRole uint8

const (
	telegramUnauthorized telegramRole = iota
	telegramOperator
	telegramAdministrator
)

func (a *App) messageRole(message *telegram.Message) telegramRole {
	if message == nil || message.From == nil || message.SenderChat != nil || !a.telegramChatAllowed(message.Chat) {
		return telegramUnauthorized
	}
	return a.telegramUserRole(message.From.ID)
}

func (a *App) callbackRole(callback telegram.CallbackQuery) telegramRole {
	if callback.Message == nil || !a.telegramChatAllowed(callback.Message.Chat) {
		return telegramUnauthorized
	}
	return a.telegramUserRole(callback.From.ID)
}

func (a *App) telegramChatAllowed(chat telegram.Chat) bool {
	if chat.ID != a.Config.TelegramChatID {
		return false
	}
	if !a.Config.TelegramMultiUser() {
		return true
	}
	return chat.Type == "group" || chat.Type == "supergroup"
}

func (a *App) telegramUserRole(userID int64) telegramRole {
	if a.Config.IsTelegramAdministrator(userID) {
		return telegramAdministrator
	}
	if a.Config.IsTelegramOperator(userID) {
		return telegramOperator
	}
	return telegramUnauthorized
}

func (a *App) authorized(message *telegram.Message) bool {
	return a.messageRole(message) != telegramUnauthorized
}

func (a *App) callbackAuthorized(callback telegram.CallbackQuery) bool {
	return a.callbackRole(callback) != telegramUnauthorized
}

func (a *App) handleAuthorizedGitHubUnlockReply(ctx context.Context, message telegram.Message) (string, bool) {
	if a.messageRole(&message) == telegramAdministrator {
		return a.handleGitHubUnlockReply(ctx, message)
	}
	if !a.isGitHubUnlockReply(message) {
		return "", false
	}
	_ = a.audit("auth.reject", "rejected", map[string]any{"kind": "github_unlock_reply"})
	return "github_unlock_unauthorized", true
}

func (a *App) isGitHubUnlockReply(message telegram.Message) bool {
	if message.ReplyToMessage == nil || message.Text == "" {
		return false
	}
	replyToMessageID := message.ReplyToMessage.MessageID
	a.githubMu.Lock()
	defer a.githubMu.Unlock()
	for _, pending := range a.githubPending {
		if pending.State == "unlocking" && pending.UnlockMessageID == replyToMessageID && pending.ExpiresAt.After(a.githubTime()) {
			return true
		}
	}
	return a.githubUnlockTombstones[replyToMessageID].After(a.githubTime())
}

func telegramPollingLockKey(cfg config.Config) string {
	return lockfile.Key("telegram-poller-v1", cfg.TelegramBotToken, cfg.EffectiveTelegramAPIBase())
}

func telegramPollingIdentity(cfg config.Config) string {
	digest := sha256.Sum256([]byte(strings.Join([]string{cfg.TelegramBotToken, cfg.EffectiveTelegramAPIBase()}, "\x00")))
	return hex.EncodeToString(digest[:8])
}

func telegramPollingLockMetadata(cfg config.Config) lockfile.Metadata {
	return lockfile.Metadata{Details: map[string]string{
		"scope":                     "Telegram bot polling",
		"telegram_polling_identity": telegramPollingIdentity(cfg),
		"version":                   version.String(),
	}}
}
