package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
)

const closeConfirmationTTL = 2 * time.Minute
const maxCloseConfirmations = 32

type closeConfirmation struct {
	SessionID    int
	TmuxServerID string
	TmuxWindowID string
	TmuxPaneID   string
	ExpiresAt    time.Time
}

func (a *App) handleCallback(ctx context.Context, cb telegram.CallbackQuery) string {
	if !a.callbackAuthorized(cb) {
		_ = a.audit("auth.reject", "rejected", map[string]any{"kind": "callback_query"})
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
	case "collapse":
		id, err := strconv.Atoi(parts[1])
		if err != nil {
			a.answerCallback(ctx, cb.ID, "bad session id")
			return "failed_bad_callback_id"
		}
		ts, status := a.validateAnchorCallback(ctx, cb, id)
		if status != "" {
			return status
		}
		if !a.answerCallback(ctx, cb.ID, "moving to collapsed sessions") {
			return "callback_telegram_failed"
		}
		msg := *cb.Message
		msg.From = &cb.From
		if !a.queueTransfer(func(workerCtx context.Context) {
			result := a.collapseAnchor(workerCtx, ts)
			if !result.OK() {
				a.reply(workerCtx, msg, result.Message)
			}
		}) {
			a.reply(ctx, msg, "Collapse is temporarily unavailable because Engram is stopping or its work queue is full.")
			return "callback_state_failed"
		}
		return "callback_ok"
	case "expand-all":
		shelf, status := a.validateCollapsedShelfCallback(ctx, cb)
		if status != "" {
			return status
		}
		if !a.answerCallback(ctx, cb.ID, "restoring sessions") {
			return "callback_telegram_failed"
		}
		msg := *cb.Message
		msg.From = &cb.From
		if !a.queueTransfer(func(workerCtx context.Context) {
			result := a.expandCollapsedShelf(workerCtx, shelf)
			if !result.OK() {
				a.reply(workerCtx, msg, result.Message)
			}
		}) {
			a.reply(ctx, msg, "Restore is temporarily unavailable because Engram is stopping or its work queue is full.")
			return "callback_state_failed"
		}
		return "callback_ok"
	case "refresh":
		id, err := strconv.Atoi(parts[1])
		if err != nil {
			a.answerCallback(ctx, cb.ID, "bad session id")
			return "failed_bad_callback_id"
		}
		ts, status := a.validateAnchorCallback(ctx, cb, id)
		if status != "" {
			return status
		}
		if ts.State == state.TerminalLost || ts.State == state.TerminalClosed {
			a.answerCallback(ctx, cb.ID, "session is "+string(ts.State))
			return "callback_user_error"
		}
		if !a.answerCallback(ctx, cb.ID, "refreshing") {
			return "callback_telegram_failed"
		}
		a.queueManualRefresh(id)
		return "callback_ok"
	case "snapshot":
		id, err := strconv.Atoi(parts[1])
		if err != nil {
			a.answerCallback(ctx, cb.ID, "bad session id")
			return "failed_bad_callback_id"
		}
		ts, status := a.validateAnchorCallback(ctx, cb, id)
		if status != "" {
			return status
		}
		if ts.State == state.TerminalLost || ts.State == state.TerminalClosed {
			a.answerCallback(ctx, cb.ID, "session is "+string(ts.State))
			return "callback_user_error"
		}
		if a.snapshotAnchors() {
			if !a.answerCallback(ctx, cb.ID, "refreshing live window") {
				return "callback_telegram_failed"
			}
			a.queueManualRefresh(id)
			return "callback_ok"
		}
		result := a.queueSnapshot(ts)
		if !a.answerCallback(ctx, cb.ID, result.Message) {
			return "callback_telegram_failed"
		}
		return result.status("callback")
	case "voice":
		id, err := strconv.Atoi(parts[1])
		if err != nil {
			a.answerCallback(ctx, cb.ID, "bad session id")
			return "failed_bad_callback_id"
		}
		ts, status := a.validateAnchorCallback(ctx, cb, id)
		if status != "" {
			return status
		}
		if ts.State != state.TerminalRunning || !a.snapshotAnchors() || !a.guideAvailable || ts.AnchorFormat != "snapshot" || ts.RetiringAnchorMessageID != 0 {
			a.answerCallback(ctx, cb.ID, "voice is unavailable")
			return "callback_user_error"
		}
		result := a.queueConversation(ts)
		if !a.answerCallback(ctx, cb.ID, result.Message) {
			return "callback_telegram_failed"
		}
		return result.status("callback")
	case "raw":
		id, err := strconv.Atoi(parts[1])
		if err != nil {
			a.answerCallback(ctx, cb.ID, "bad session id")
			return "failed_bad_callback_id"
		}
		ts, status := a.validateAnchorCallback(ctx, cb, id)
		if status != "" {
			return status
		}
		anchorLock := a.anchorMutex(id)
		anchorLock.Lock()
		current, currentOK := a.Store.FindSession(id)
		if !currentOK || current.AnchorChatID != cb.Message.Chat.ID || current.AnchorMessageID != cb.Message.MessageID || !sameTerminalBinding(current, ts) {
			anchorLock.Unlock()
			a.answerCallback(ctx, cb.ID, "anchor moved; use the newer live message")
			return "callback_user_error"
		}
		ts = current
		if ts.State != state.TerminalRunning || !mediaAnchorFormat(ts.AnchorFormat) || ts.RetiringAnchorMessageID != 0 {
			anchorLock.Unlock()
			a.answerCallback(ctx, cb.ID, "raw view is unavailable")
			return "callback_user_error"
		}
		frame, ok := a.snapshotTextFrame(ts)
		anchorLock.Unlock()
		if !ok {
			a.answerCallback(ctx, cb.ID, "raw view is refreshing")
			return "callback_user_error"
		}
		if !a.answerCallback(ctx, cb.ID, "preparing raw view") {
			return "callback_telegram_failed"
		}
		msg := *cb.Message
		msg.From = &cb.From
		return a.queueAccessibleSnapshotFrame(ctx, msg, ts, frame).status("callback")
	case "recover":
		id, err := strconv.Atoi(parts[1])
		if err != nil {
			a.answerCallback(ctx, cb.ID, "bad session id")
			return "failed_bad_callback_id"
		}
		if _, status := a.validateAnchorCallback(ctx, cb, id); status != "" {
			return status
		}
		result := a.watchSession(ctx, id, cb.Message.MessageID)
		message := result.Message
		if result.OK() {
			message = "reattached"
		}
		if !a.answerCallback(ctx, cb.ID, message) {
			return "callback_telegram_failed"
		}
		return result.status("callback")
	case "resume":
		id, err := strconv.Atoi(parts[1])
		if err != nil {
			a.answerCallback(ctx, cb.ID, "bad session id")
			return "failed_bad_callback_id"
		}
		ts, status := a.validateAnchorCallback(ctx, cb, id)
		if status != "" {
			return status
		}
		if ts.State != state.TerminalLost || !validResumeProgram(ts.ResumeProgram) || !validResumeSessionID(ts.ResumeSessionID) {
			a.answerCallback(ctx, cb.ID, "exact resume is unavailable")
			return "callback_user_error"
		}
		if !a.answerCallback(ctx, cb.ID, "resuming") {
			return "callback_telegram_failed"
		}
		result := a.resumeSession(ctx, id, "", "")
		if !result.OK() {
			msg := *cb.Message
			msg.From = &cb.From
			a.reply(ctx, msg, result.Message)
		}
		return result.status("callback")
	case "plan-resume":
		callbackParts := strings.Split(parts[1], ":")
		if len(callbackParts) != 2 {
			a.answerCallback(ctx, cb.ID, "bad session id")
			return "failed_bad_callback_id"
		}
		id, err := strconv.Atoi(callbackParts[0])
		if err != nil {
			a.answerCallback(ctx, cb.ID, "bad session id")
			return "failed_bad_callback_id"
		}
		ts, ok := a.Store.FindSession(id)
		if !ok || ts.State != state.TerminalLost || callbackParts[1] != sessionActionToken(ts) || !validResumeProgram(ts.ResumeProgram) || !validResumeSessionID(ts.ResumeSessionID) {
			a.answerCallback(ctx, cb.ID, "recovery plan is stale; run /recovery again")
			return "callback_user_error"
		}
		if !a.answerCallback(ctx, cb.ID, "resuming") {
			return "callback_telegram_failed"
		}
		result := a.resumeSession(ctx, id, "", "")
		msg := *cb.Message
		msg.From = &cb.From
		a.reply(ctx, msg, result.Message)
		return result.status("callback")
	case "plan-dismiss":
		if _, err := a.Telegram.EditReplyMarkup(ctx, cb.Message.Chat.ID, cb.Message.MessageID, telegram.ClearMarkup()); err != nil && !telegram.IsMessageNotModified(err) {
			a.answerCallback(ctx, cb.ID, "could not dismiss")
			return "callback_telegram_failed"
		}
		return callbackAnswerStatus(a.answerCallback(ctx, cb.ID, "dismissed"), "callback_ok")
	case "key":
		id, preset, ok := parseKeyCallback(parts[1])
		if !ok {
			a.answerCallback(ctx, cb.ID, "bad key")
			return "failed_bad_callback_key"
		}
		validated, status := a.validateAnchorCallback(ctx, cb, id)
		if status != "" {
			return status
		}
		if directionalKeyPreset(preset) && validated.AnchorFormat != "snapshot" {
			a.answerCallback(ctx, cb.ID, "arrows are available in snapshot mode")
			return "callback_user_error"
		}
		action, ok := anchorKeyAction(preset)
		if !ok {
			a.answerCallback(ctx, cb.ID, "unknown key")
			return "failed_unknown_callback_key"
		}
		if !a.answerCallback(ctx, cb.ID, "sending "+action.Label) {
			return "callback_telegram_failed"
		}
		result := a.sendKeyGroupsExpected(ctx, id, action.Groups, action.Label, action.Delay, &validated)
		if !result.OK() {
			msg := *cb.Message
			msg.From = &cb.From
			a.reply(ctx, msg, result.Message)
		}
		return result.status("callback")
	case "file":
		id, token, index, ok := parseFileCallback(parts[1])
		if !ok {
			a.answerCallback(ctx, cb.ID, "bad file reference")
			return "failed_bad_callback_file"
		}
		validated, status := a.validateAnchorCallback(ctx, cb, id)
		if status != "" {
			return status
		}
		path, ok := resolveAnchorFile(validated, token, index)
		if !ok {
			a.answerCallback(ctx, cb.ID, "file list changed; refresh the card")
			return "callback_user_error"
		}
		if !a.answerCallback(ctx, cb.ID, fmt.Sprintf("preparing file %d", index)) {
			return "callback_telegram_failed"
		}
		msg := *cb.Message
		msg.From = &cb.From
		return a.downloadAnchorFile(ctx, msg, validated, token, index, path).status("callback")
	case "session-watch":
		id, _, status := a.validateSessionListCallback(ctx, cb, parts[1])
		if status != "" {
			return status
		}
		result := a.watchSession(ctx, id, cb.Message.MessageID)
		if !a.answerCallback(ctx, cb.ID, result.Message) {
			return "callback_telegram_failed"
		}
		return result.status("callback")
	case "session-close":
		_, ts, status := a.validateSessionListCallback(ctx, cb, parts[1])
		if status != "" {
			return status
		}
		token, err := a.issueCloseConfirmation(ts)
		if err != nil {
			a.answerCallback(ctx, cb.ID, "could not create confirmation")
			return "callback_state_failed"
		}
		if _, err := a.Telegram.SendMessage(ctx, cb.Message.Chat.ID, closeConfirmationText(ts), cb.Message.MessageID, closeConfirmationMarkup(token)); err != nil {
			a.consumeCloseConfirmation(token)
			a.answerCallback(ctx, cb.ID, "could not open confirmation")
			return "callback_telegram_failed"
		}
		return callbackAnswerStatus(a.answerCallback(ctx, cb.ID, "confirm below"), "callback_ok")
	case "close":
		id, err := strconv.Atoi(parts[1])
		if err != nil {
			a.answerCallback(ctx, cb.ID, "bad session id")
			return "failed_bad_callback_id"
		}
		ts, status := a.validateAnchorCallback(ctx, cb, id)
		if status != "" {
			return status
		}
		token, err := a.issueCloseConfirmation(ts)
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
		result := a.closeSessionExpected(ctx, confirmation.SessionID, &confirmation)
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

func (a *App) validateSessionListCallback(ctx context.Context, cb telegram.CallbackQuery, value string) (int, state.TerminalSession, string) {
	parts := strings.Split(value, ":")
	if len(parts) != 2 {
		a.answerCallback(ctx, cb.ID, "bad session action")
		return 0, state.TerminalSession{}, "failed_bad_callback_id"
	}
	id, err := strconv.Atoi(parts[0])
	if err != nil || id <= 0 {
		a.answerCallback(ctx, cb.ID, "bad session id")
		return 0, state.TerminalSession{}, "failed_bad_callback_id"
	}
	ts, ok := a.Store.FindSession(id)
	if !ok || ts.State == state.TerminalClosed || parts[1] != sessionActionToken(ts) {
		a.answerCallback(ctx, cb.ID, "session list changed; run /sessions again")
		return 0, state.TerminalSession{}, "callback_user_error"
	}
	return id, ts, ""
}

func directionalKeyPreset(preset string) bool {
	switch preset {
	case "left", "up", "down", "right":
		return true
	default:
		return false
	}
}

func parseFileCallback(value string) (int, string, int, bool) {
	parts := strings.Split(value, ":")
	if len(parts) != 3 || len(parts[1]) != 16 {
		return 0, "", 0, false
	}
	id, idErr := strconv.Atoi(parts[0])
	index, indexErr := strconv.Atoi(parts[2])
	if idErr != nil || indexErr != nil || id <= 0 || index <= 0 {
		return 0, "", 0, false
	}
	if _, err := hex.DecodeString(parts[1]); err != nil {
		return 0, "", 0, false
	}
	return id, parts[1], index, true
}

func resolveAnchorFile(ts state.TerminalSession, token string, index int) (string, bool) {
	if ts.State != state.TerminalRunning || token == "" || token != ts.AnchorFileToken || index <= 0 || index > len(ts.AnchorFiles) {
		return "", false
	}
	return ts.AnchorFiles[index-1], true
}

func (a *App) callbackAuthorized(cb telegram.CallbackQuery) bool {
	return cb.From.ID == a.Config.TelegramAllowedUserID && cb.Message != nil && cb.Message.Chat.ID == a.Config.TelegramChatID
}

func (a *App) validateAnchorCallback(ctx context.Context, cb telegram.CallbackQuery, id int) (state.TerminalSession, string) {
	ts, ok := a.Store.FindSession(id)
	if !ok {
		if !a.answerCallback(ctx, cb.ID, "session not found") {
			return state.TerminalSession{}, "callback_telegram_failed"
		}
		return state.TerminalSession{}, "callback_user_error"
	}
	if cb.Message == nil || ts.AnchorChatID != cb.Message.Chat.ID || ts.AnchorMessageID != cb.Message.MessageID {
		if !a.answerCallback(ctx, cb.ID, "anchor moved; use the newer live message") {
			return state.TerminalSession{}, "callback_telegram_failed"
		}
		return state.TerminalSession{}, "callback_user_error"
	}
	if ts.Collapsed {
		if !a.answerCallback(ctx, cb.ID, "On the collapsed shelf; restore all sessions first") {
			return state.TerminalSession{}, "callback_telegram_failed"
		}
		return state.TerminalSession{}, "callback_user_error"
	}
	return ts, ""
}

func (a *App) validateCollapsedShelfCallback(ctx context.Context, cb telegram.CallbackQuery) (state.CollapsedShelf, string) {
	shelf := a.Store.Snapshot().CollapsedShelf
	if shelf == nil || cb.Message == nil || shelf.ChatID != cb.Message.Chat.ID || shelf.MessageID != cb.Message.MessageID {
		if !a.answerCallback(ctx, cb.ID, "collapsed shelf moved; use the newer message") {
			return state.CollapsedShelf{}, "callback_telegram_failed"
		}
		return state.CollapsedShelf{}, "callback_user_error"
	}
	return *shelf, ""
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
	return telegram.CloseConfirmationMarkup(token)
}

func (a *App) issueCloseConfirmation(session state.TerminalSession) (string, error) {
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
	a.closeConfirms[token] = closeConfirmation{
		SessionID: session.ID, TmuxServerID: session.TmuxServerID,
		TmuxWindowID: session.TmuxWindowID, TmuxPaneID: session.TmuxPaneID,
		ExpiresAt: now.Add(closeConfirmationTTL),
	}
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
	case "left":
		return anchorKeyPreset{Label: "Left", Groups: [][]string{{"Left"}}}, true
	case "up":
		return anchorKeyPreset{Label: "Up", Groups: [][]string{{"Up"}}}, true
	case "down":
		return anchorKeyPreset{Label: "Down", Groups: [][]string{{"Down"}}}, true
	case "right":
		return anchorKeyPreset{Label: "Right", Groups: [][]string{{"Right"}}}, true
	default:
		return anchorKeyPreset{}, false
	}
}
