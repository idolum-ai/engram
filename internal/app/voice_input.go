package app

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
)

type voiceTranscriber interface {
	Transcribe(context.Context, string) (string, error)
}

// Leave room for the exact post-delivery verification reply within Telegram's
// message bound; the acknowledged transcript must never differ from pane input.
const maxVoiceTranscriptBytes = 3600

func (a *App) handleVoiceReply(ctx context.Context, msg telegram.Message) actionResult {
	mode := a.Config.EffectiveVoiceInputMode()
	if mode == config.VoiceInputModeTranscribe && a.Transcriber == nil {
		a.reply(ctx, msg, "Voice transcription is unavailable. Check VOICE_INPUT_MODE and OPENAI_API_KEY, then restart Engram.")
		return actionResult{Outcome: actionUserError, Message: "voice input unavailable"}
	}
	ts, targetState, found := a.Store.FindReplyTarget(msg.Chat.ID, msg.ReplyToMessage.MessageID)
	if found && targetState == state.ReplyTargetStale {
		a.reply(ctx, msg, staleAlternateReply(ts.ID))
		return actionResult{Outcome: actionUserError, Message: "stale voice reply"}
	}
	if !found || targetState != state.ReplyTargetCurrent {
		a.reply(ctx, msg, "Session not found for this voice reply. Reply to a session's latest view or live anchor.")
		return actionResult{Outcome: actionUserError, Message: "voice reply target unavailable"}
	}
	if ts.State == state.TerminalClosed {
		a.reply(ctx, msg, "This session is closed. Reply to a current running session view.")
		return actionResult{Outcome: actionUserError, Message: "voice reply target closed"}
	}
	hardMax := minInt64(telegramCloudDownloadMax, a.attachmentHardMax())
	if msg.Voice.FileSize > hardMax {
		a.reply(ctx, msg, fmt.Sprintf("Voice note rejected: %d bytes exceeds the available %d-byte download limit.", msg.Voice.FileSize, hardMax))
		return actionResult{Outcome: actionUserError, Message: "voice note exceeds hard limit"}
	}
	targetMessageID := msg.ReplyToMessage.MessageID
	if !a.queueTransfer(func(transferCtx context.Context) {
		if mode == config.VoiceInputModeTranscribe {
			a.transcribeVoiceReply(transferCtx, msg, ts, targetMessageID)
			return
		}
		a.routeVoicePathReply(transferCtx, msg, ts, targetMessageID)
	}) {
		a.reply(ctx, msg, "Voice input is temporarily unavailable because Engram is stopping or its transfer queue is full.")
		return actionResult{Outcome: actionStateFailed, Message: "voice transfer unavailable"}
	}
	return actionResult{Outcome: actionOK, Message: "voice input queued"}
}

func (a *App) routeVoicePathReply(ctx context.Context, msg telegram.Message, expected state.TerminalSession, targetMessageID int) {
	if !a.voiceReplyStillDeliverable(msg.Chat.ID, targetMessageID, expected) {
		a.reply(ctx, msg, staleAlternateReply(expected.ID))
		return
	}
	file, err := a.Telegram.GetFile(ctx, msg.Voice.FileID)
	if err != nil {
		a.reply(ctx, msg, "Could not retrieve the voice note: "+err.Error())
		return
	}
	path, err := reserveVoicePath(a.Config.AttachmentDir())
	if err != nil {
		a.reply(ctx, msg, "Could not prepare the voice note: "+err.Error())
		return
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(path)
		}
	}()
	download, err := a.Telegram.DownloadFileHashed(ctx, file.FilePath, path, minInt64(telegramCloudDownloadMax, a.attachmentHardMax()))
	if err != nil {
		a.reply(ctx, msg, "Could not download the voice note: "+err.Error())
		return
	}
	sessionLock := a.sessionMutex(expected.ID)
	sessionLock.Lock()
	anchorLock := a.anchorMutex(expected.ID)
	anchorLock.Lock()
	if !a.voiceReplyStillDeliverableLocked(msg.Chat.ID, targetMessageID, expected) {
		anchorLock.Unlock()
		sessionLock.Unlock()
		a.reply(ctx, msg, staleAlternateReply(expected.ID))
		return
	}
	attachmentErr := a.Store.AddAttachment(state.Attachment{
		TelegramFileID:       msg.Voice.FileID,
		TelegramUniqueFileID: msg.Voice.FileUniqueID,
		ChatID:               msg.Chat.ID,
		UserID:               msg.From.ID,
		OriginalName:         "voice.ogg",
		ContentType:          msg.Voice.MimeType,
		SizeBytes:            download.Size,
		SHA256:               download.SHA256,
		StoredPath:           path,
		ReceivedAt:           time.Now().UTC(),
	})
	attachmentCommitted := attachmentErr == nil || state.PersistenceReachedReplacement(attachmentErr)
	if attachmentErr != nil && attachmentCommitted {
		_ = a.audit("voice.path", "durability_uncertain", map[string]any{"session_id": expected.ID, "path": path, "error": attachmentErr.Error()})
	}
	if !attachmentCommitted {
		anchorLock.Unlock()
		sessionLock.Unlock()
		a.reply(ctx, msg, "Could not retain the voice note: "+attachmentErr.Error())
		return
	}
	keep = true
	completion := a.sendInputExpectedLocked(ctx, expected.ID, "(voice message: "+path+")", "voice", true, &expected)
	anchorLock.Unlock()
	sessionLock.Unlock()
	result := a.finishInput(ctx, expected.ID, completion)
	if !result.OK() {
		a.reply(ctx, msg, result.Message)
		return
	}
	_ = a.audit("voice.path", "ok", map[string]any{"session_id": expected.ID, "path": path})
}

func (a *App) transcribeVoiceReply(ctx context.Context, msg telegram.Message, expected state.TerminalSession, targetMessageID int) {
	if !a.voiceReplyStillDeliverable(msg.Chat.ID, targetMessageID, expected) {
		a.reply(ctx, msg, staleAlternateReply(expected.ID))
		return
	}
	file, err := a.Telegram.GetFile(ctx, msg.Voice.FileID)
	if err != nil {
		a.reply(ctx, msg, "Could not retrieve the voice note: "+err.Error())
		return
	}
	path, err := reserveVoicePath(a.Config.ArtifactDir())
	if err != nil {
		a.reply(ctx, msg, "Could not prepare the voice note: "+err.Error())
		return
	}
	defer os.Remove(path)
	if _, err := a.Telegram.DownloadFile(ctx, file.FilePath, path, minInt64(telegramCloudDownloadMax, a.attachmentHardMax())); err != nil {
		a.reply(ctx, msg, "Could not download the voice note: "+err.Error())
		return
	}
	if !a.voiceReplyStillDeliverable(msg.Chat.ID, targetMessageID, expected) {
		a.reply(ctx, msg, staleAlternateReply(expected.ID))
		return
	}
	transcript, err := a.Transcriber.Transcribe(ctx, path)
	if err != nil {
		_ = a.audit("voice.transcribe", "failed", map[string]any{"session_id": expected.ID, "error": err.Error()})
		a.reply(ctx, msg, "The voice note could not be transcribed; no input was sent.")
		return
	}
	transcript, err = normalizeVoiceTranscript(transcript)
	if err != nil {
		_ = a.audit("voice.transcribe", "rejected", map[string]any{"session_id": expected.ID, "error": err.Error()})
		a.reply(ctx, msg, "The voice transcript was not safe to send as terminal input; no input was sent.")
		return
	}
	input := "(transcribed) " + transcript
	result := a.deliverVoiceInput(ctx, msg, expected, targetMessageID, input)
	if !result.OK() {
		return
	}
	a.reply(ctx, msg, fmt.Sprintf("Sent to [%d]:\n\n%s", expected.ID, input))
	_ = a.audit("voice.transcribe", "ok", map[string]any{"session_id": expected.ID})
}

func (a *App) voiceReplyStillDeliverable(chatID int64, messageID int, expected state.TerminalSession) bool {
	sessionLock := a.sessionMutex(expected.ID)
	sessionLock.Lock()
	anchorLock := a.anchorMutex(expected.ID)
	anchorLock.Lock()
	ok := a.voiceReplyStillDeliverableLocked(chatID, messageID, expected)
	anchorLock.Unlock()
	sessionLock.Unlock()
	return ok
}

func (a *App) voiceReplyStillDeliverableLocked(chatID int64, messageID int, expected state.TerminalSession) bool {
	current, targetState, found := a.Store.FindReplyTarget(chatID, messageID)
	return found && targetState == state.ReplyTargetCurrent && current.State != state.TerminalClosed && current.PendingResume == nil && sameTerminalBinding(current, expected)
}

func (a *App) deliverVoiceInput(ctx context.Context, msg telegram.Message, expected state.TerminalSession, targetMessageID int, text string) actionResult {
	sessionLock := a.sessionMutex(expected.ID)
	sessionLock.Lock()
	anchorLock := a.anchorMutex(expected.ID)
	anchorLock.Lock()
	if !a.voiceReplyStillDeliverableLocked(msg.Chat.ID, targetMessageID, expected) {
		anchorLock.Unlock()
		sessionLock.Unlock()
		a.reply(ctx, msg, staleAlternateReply(expected.ID))
		return actionResult{Outcome: actionUserError, Message: "voice reply target changed"}
	}
	completion := a.sendInputExpectedLocked(ctx, expected.ID, text, "voice", true, &expected)
	anchorLock.Unlock()
	sessionLock.Unlock()
	result := a.finishInput(ctx, expected.ID, completion)
	if !result.OK() {
		a.reply(ctx, msg, result.Message)
	}
	return result
}

func reserveVoicePath(dir string) (string, error) {
	file, err := os.CreateTemp(dir, "engram-voice-*.ogg")
	if err != nil {
		return "", err
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	if err := os.Remove(path); err != nil {
		return "", err
	}
	return path, nil
}

func normalizeVoiceTranscript(text string) (string, error) {
	if !utf8.ValidString(text) {
		return "", fmt.Errorf("transcript is not valid UTF-8")
	}
	for _, r := range text {
		if (unicode.IsControl(r) && !unicode.IsSpace(r)) || unicode.Is(unicode.Cf, r) {
			return "", fmt.Errorf("transcript contains terminal control characters")
		}
	}
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		return "", fmt.Errorf("transcript is empty")
	}
	if len(text) > maxVoiceTranscriptBytes {
		return "", fmt.Errorf("transcript exceeds %d bytes", maxVoiceTranscriptBytes)
	}
	return text, nil
}
