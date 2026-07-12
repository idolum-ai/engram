package telegram

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	maxRateLimitRetries   = 1
	maxRetryAfter         = 30 * time.Second
	maxConcurrentOutbound = 4
	defaultSendInterval   = 35 * time.Millisecond
)

type Client struct {
	BaseURL    string
	FileBase   string
	Token      string
	HTTPClient *http.Client

	outboundOnce     sync.Once
	outboundSlots    chan struct{}
	outboundMu       sync.Mutex
	nextOutbound     time.Time
	outboundInterval time.Duration
	retrySleep       func(context.Context, time.Duration) error
}

func New(token string) *Client {
	return NewAt(token, "https://api.telegram.org")
}

func NewAt(token, apiBase string) *Client {
	apiBase = strings.TrimRight(apiBase, "/")
	return &Client{
		BaseURL:  apiBase + "/bot" + token,
		FileBase: apiBase + "/file/bot" + token,
		Token:    token,
		HTTPClient: &http.Client{
			Timeout: 70 * time.Second,
		},
		outboundInterval: defaultSendInterval,
	}
}

type Response[T any] struct {
	OK          bool               `json:"ok"`
	ErrorCode   int                `json:"error_code,omitempty"`
	Description string             `json:"description,omitempty"`
	Parameters  ResponseParameters `json:"parameters,omitempty"`
	Result      T                  `json:"result"`
}

type ResponseParameters struct {
	RetryAfter      int   `json:"retry_after,omitempty"`
	MigrateToChatID int64 `json:"migrate_to_chat_id,omitempty"`
}

// Error is a sanitized Telegram Bot API or request failure.
type Error struct {
	Method          string
	StatusCode      int
	ErrorCode       int
	Description     string
	RetryAfter      time.Duration
	MigrateToChatID int64
}

func (e *Error) Error() string {
	if e == nil {
		return "telegram request failed"
	}
	detail := make([]string, 0, 2)
	if e.StatusCode != 0 {
		detail = append(detail, fmt.Sprintf("HTTP %d", e.StatusCode))
	}
	if e.ErrorCode != 0 && e.ErrorCode != e.StatusCode {
		detail = append(detail, fmt.Sprintf("error %d", e.ErrorCode))
	}
	message := "telegram request failed"
	if e.Method != "" {
		message = "telegram " + e.Method + " failed"
	}
	if len(detail) != 0 {
		message += " (" + strings.Join(detail, ", ") + ")"
	}
	if e.Description != "" {
		message += ": " + e.Description
	}
	return message
}

func (e *Error) IsRateLimited() bool {
	return e != nil && (e.StatusCode == http.StatusTooManyRequests || e.ErrorCode == http.StatusTooManyRequests)
}

func (e *Error) IsMessageNotModified() bool {
	return e != nil && strings.Contains(strings.ToLower(e.Description), "message is not modified")
}

// IsRateLimited reports whether err is a Telegram HTTP or Bot API 429 error.
func IsRateLimited(err error) bool {
	var telegramErr *Error
	return errors.As(err, &telegramErr) && telegramErr.IsRateLimited()
}

func RetryAfter(err error) time.Duration {
	var telegramErr *Error
	if errors.As(err, &telegramErr) {
		return telegramErr.RetryAfter
	}
	return 0
}

// IsMessageNotModified reports whether Telegram rejected an edit because its
// content and markup already match the existing message.
func IsMessageNotModified(err error) bool {
	var telegramErr *Error
	return errors.As(err, &telegramErr) && telegramErr.IsMessageNotModified()
}

func IsMessageAlreadyPinned(err error) bool {
	var telegramErr *Error
	return errors.As(err, &telegramErr) && strings.Contains(strings.ToLower(telegramErr.Description), "already pinned")
}

func IsMessageNotPinned(err error) bool {
	var telegramErr *Error
	return errors.As(err, &telegramErr) && strings.Contains(strings.ToLower(telegramErr.Description), "not pinned")
}

type Update struct {
	UpdateID      int            `json:"update_id"`
	Message       *Message       `json:"message,omitempty"`
	CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
}

type CallbackQuery struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message,omitempty"`
	Data    string   `json:"data,omitempty"`
}

type User struct {
	ID        int64  `json:"id"`
	Username  string `json:"username,omitempty"`
	FirstName string `json:"first_name,omitempty"`
}

type Chat struct {
	ID    int64  `json:"id"`
	Type  string `json:"type,omitempty"`
	Title string `json:"title,omitempty"`
}

type Message struct {
	MessageID      int       `json:"message_id"`
	From           *User     `json:"from,omitempty"`
	Chat           Chat      `json:"chat"`
	Date           int64     `json:"date,omitempty"`
	Text           string    `json:"text,omitempty"`
	Caption        string    `json:"caption,omitempty"`
	ReplyToMessage *Message  `json:"reply_to_message,omitempty"`
	Document       *Document `json:"document,omitempty"`
	Photo          []Photo   `json:"photo,omitempty"`
	Video          *Document `json:"video,omitempty"`
	Audio          *Document `json:"audio,omitempty"`
	Voice          *Document `json:"voice,omitempty"`
	Animation      *Document `json:"animation,omitempty"`
	Sticker        *Document `json:"sticker,omitempty"`
}

type Document struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id,omitempty"`
	FileName     string `json:"file_name,omitempty"`
	MimeType     string `json:"mime_type,omitempty"`
	FileSize     int64  `json:"file_size,omitempty"`
}

type Photo struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id,omitempty"`
	FileSize     int64  `json:"file_size,omitempty"`
	Width        int    `json:"width,omitempty"`
	Height       int    `json:"height,omitempty"`
}

type File struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id,omitempty"`
	FileSize     int64  `json:"file_size,omitempty"`
	FilePath     string `json:"file_path,omitempty"`
}

type DownloadResult struct {
	Size   int64
	SHA256 string
}

type InlineKeyboardMarkup struct {
	InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
}

type InlineKeyboardButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

type AttachTarget struct {
	Label  string
	Target string
}

type BotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

func (c *Client) SetMyCommands(ctx context.Context, commands []BotCommand) error {
	body := map[string]any{"commands": commands}
	var out bool
	return c.postJSON(ctx, "setMyCommands", body, &out)
}

func (c *Client) GetUpdates(ctx context.Context, offset int, timeout int) ([]Update, error) {
	v := url.Values{}
	if offset > 0 {
		v.Set("offset", strconv.Itoa(offset))
	}
	v.Set("timeout", strconv.Itoa(timeout))
	v.Set("allowed_updates", `["message","callback_query"]`)
	var out []Update
	if err := c.postForm(ctx, "getUpdates", v, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) SendMessage(ctx context.Context, chatID int64, text string, replyTo int, markup *InlineKeyboardMarkup) (Message, error) {
	return c.sendMessage(ctx, chatID, text, replyTo, markup, "")
}

func (c *Client) SendHTMLMessage(ctx context.Context, chatID int64, text string, replyTo int, markup *InlineKeyboardMarkup) (Message, error) {
	return c.sendMessage(ctx, chatID, text, replyTo, markup, "HTML")
}

func (c *Client) DeleteMessage(ctx context.Context, chatID int64, messageID int) error {
	body := map[string]any{"chat_id": chatID, "message_id": messageID}
	var out bool
	return c.postJSON(ctx, "deleteMessage", body, &out)
}

func (c *Client) sendMessage(ctx context.Context, chatID int64, text string, replyTo int, markup *InlineKeyboardMarkup, parseMode string) (Message, error) {
	body := map[string]any{"chat_id": chatID, "text": clampText(text), "disable_web_page_preview": true}
	if replyTo != 0 {
		body["reply_to_message_id"] = replyTo
	}
	if markup != nil {
		body["reply_markup"] = markup
	}
	if parseMode != "" {
		body["parse_mode"] = parseMode
	}
	var out Message
	return out, c.postJSON(ctx, "sendMessage", body, &out)
}

func (c *Client) EditMessage(ctx context.Context, chatID int64, messageID int, text string, markup *InlineKeyboardMarkup) (Message, error) {
	return c.editMessage(ctx, chatID, messageID, text, markup, "")
}

func (c *Client) EditHTMLMessage(ctx context.Context, chatID int64, messageID int, text string, markup *InlineKeyboardMarkup) (Message, error) {
	return c.editMessage(ctx, chatID, messageID, text, markup, "HTML")
}

func (c *Client) editMessage(ctx context.Context, chatID int64, messageID int, text string, markup *InlineKeyboardMarkup, parseMode string) (Message, error) {
	body := map[string]any{"chat_id": chatID, "message_id": messageID, "text": clampText(text), "disable_web_page_preview": true}
	if markup != nil {
		body["reply_markup"] = markup
	}
	if parseMode != "" {
		body["parse_mode"] = parseMode
	}
	var out Message
	return out, c.postJSON(ctx, "editMessageText", body, &out)
}

func (c *Client) EditReplyMarkup(ctx context.Context, chatID int64, messageID int, markup *InlineKeyboardMarkup) (Message, error) {
	body := map[string]any{"chat_id": chatID, "message_id": messageID}
	if markup != nil {
		body["reply_markup"] = markup
	}
	var out Message
	return out, c.postJSON(ctx, "editMessageReplyMarkup", body, &out)
}

func (c *Client) AnswerCallback(ctx context.Context, id string, text string) error {
	body := map[string]any{"callback_query_id": id}
	if text != "" {
		body["text"] = text
	}
	var out bool
	return c.postJSON(ctx, "answerCallbackQuery", body, &out)
}

func (c *Client) PinChatMessage(ctx context.Context, chatID int64, messageID int) error {
	body := map[string]any{
		"chat_id":              chatID,
		"message_id":           messageID,
		"disable_notification": true,
	}
	var out bool
	return c.postJSON(ctx, "pinChatMessage", body, &out)
}

func (c *Client) UnpinChatMessage(ctx context.Context, chatID int64, messageID int) error {
	body := map[string]any{"chat_id": chatID, "message_id": messageID}
	var out bool
	return c.postJSON(ctx, "unpinChatMessage", body, &out)
}

func (c *Client) GetFile(ctx context.Context, fileID string) (File, error) {
	v := url.Values{}
	v.Set("file_id", fileID)
	var out File
	return out, c.postForm(ctx, "getFile", v, &out)
}

func (c *Client) DownloadFile(ctx context.Context, filePath, dest string, maxBytes int64) (int64, error) {
	result, err := c.DownloadFileHashed(ctx, filePath, dest, maxBytes)
	return result.Size, err
}

func (c *Client) DownloadFileHashed(ctx context.Context, filePath, dest string, maxBytes int64) (DownloadResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.FileBase+"/"+filePath, nil)
	if err != nil {
		return DownloadResult{}, c.requestError("downloadFile", "could not create request")
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		if ctxErr := contextError(ctx, err); ctxErr != nil {
			return DownloadResult{}, ctxErr
		}
		return DownloadResult{}, c.requestError("downloadFile", "transport request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return DownloadResult{}, &Error{
			Method:      "downloadFile",
			StatusCode:  resp.StatusCode,
			Description: http.StatusText(resp.StatusCode),
		}
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return DownloadResult{}, err
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return DownloadResult{}, err
	}
	defer f.Close()
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(dest)
		}
	}()
	var r io.Reader = resp.Body
	if maxBytes > 0 {
		r = io.LimitReader(resp.Body, maxBytes+1)
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hash), r)
	if err != nil {
		if ctxErr := contextError(ctx, err); ctxErr != nil {
			return DownloadResult{Size: n}, ctxErr
		}
		return DownloadResult{Size: n}, c.requestError("downloadFile", "response copy failed")
	}
	if maxBytes > 0 && n > maxBytes {
		return DownloadResult{Size: n}, fmt.Errorf("download exceeded max bytes")
	}
	keep = true
	return DownloadResult{Size: n, SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}

func (c *Client) SendDocument(ctx context.Context, chatID int64, path string, caption string) (Message, error) {
	return c.SendDocumentNamed(ctx, chatID, path, filepath.Base(path), caption)
}

func (c *Client) SendDocumentNamed(ctx context.Context, chatID int64, path, filename, caption string) (Message, error) {
	var out Message
	filename = safeDocumentFilename(filename)
	err := c.do(ctx, "sendDocument", true, func() (*http.Request, error) {
		return c.documentRequest(ctx, chatID, path, filename, caption)
	}, &out)
	return out, err
}

func (c *Client) SendPhoto(ctx context.Context, chatID int64, path, caption string, replyTo int) (Message, error) {
	return c.SendPhotoWithMarkup(ctx, chatID, path, caption, replyTo, nil)
}

func (c *Client) SendPhotoWithMarkup(ctx context.Context, chatID int64, path, caption string, replyTo int, markup *InlineKeyboardMarkup) (Message, error) {
	var out Message
	err := c.do(ctx, "sendPhoto", true, func() (*http.Request, error) {
		return c.mediaRequest(ctx, "sendPhoto", "photo", chatID, path, "engram-window.png", caption, replyTo, markup)
	}, &out)
	return out, err
}

func (c *Client) EditPhoto(ctx context.Context, chatID int64, messageID int, path, caption string, markup *InlineKeyboardMarkup) (Message, error) {
	var out Message
	err := c.do(ctx, "editMessageMedia", true, func() (*http.Request, error) {
		return c.editPhotoRequest(ctx, chatID, messageID, path, caption, markup)
	}, &out)
	return out, err
}

func (c *Client) EditCaption(ctx context.Context, chatID int64, messageID int, caption string, markup *InlineKeyboardMarkup) (Message, error) {
	body := map[string]any{"chat_id": chatID, "message_id": messageID, "caption": clampCaption(caption)}
	if markup != nil {
		body["reply_markup"] = markup
	}
	var out Message
	return out, c.postJSON(ctx, "editMessageCaption", body, &out)
}

func (m Message) FileAttachment() (Document, bool) {
	if m.Document != nil {
		return *m.Document, true
	}
	if m.Video != nil {
		return *m.Video, true
	}
	if m.Audio != nil {
		return *m.Audio, true
	}
	if m.Voice != nil {
		return *m.Voice, true
	}
	if m.Animation != nil {
		return *m.Animation, true
	}
	if m.Sticker != nil {
		return *m.Sticker, true
	}
	if len(m.Photo) > 0 {
		p := m.Photo[len(m.Photo)-1]
		return Document{FileID: p.FileID, FileUniqueID: p.FileUniqueID, FileSize: p.FileSize, FileName: "photo.jpg"}, true
	}
	return Document{}, false
}

func AnchorMarkup(sessionID int, includeImage, includeVoice bool) *InlineKeyboardMarkup {
	actions := []InlineKeyboardButton{{Text: "🔄", CallbackData: fmt.Sprintf("refresh:%d", sessionID)}}
	if includeImage {
		actions = append(actions, InlineKeyboardButton{Text: "🖼️", CallbackData: fmt.Sprintf("snapshot:%d", sessionID)})
	}
	if includeVoice {
		actions = append(actions, InlineKeyboardButton{Text: "🗣️", CallbackData: fmt.Sprintf("voice:%d", sessionID)})
	}
	return &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{
		actions,
		{
			{Text: "Esc", CallbackData: fmt.Sprintf("key:%d:esc", sessionID)},
			{Text: "Escx2", CallbackData: fmt.Sprintf("key:%d:esc2", sessionID)},
			{Text: "^C", CallbackData: fmt.Sprintf("key:%d:ctrl-c", sessionID)},
			{Text: "^D", CallbackData: fmt.Sprintf("key:%d:ctrl-d", sessionID)},
			{Text: "Enter", CallbackData: fmt.Sprintf("key:%d:enter", sessionID)},
		},
	}}
}

func ClearMarkup() *InlineKeyboardMarkup {
	return &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{}}
}

func RecoverMarkup(sessionID int) *InlineKeyboardMarkup {
	return &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{{
		{Text: "🧭 Reattach", CallbackData: fmt.Sprintf("recover:%d", sessionID)},
	}}}
}

func SessionListMarkup(ids []int, attachTargets []AttachTarget) *InlineKeyboardMarkup {
	if len(ids) == 0 && len(attachTargets) == 0 {
		return nil
	}
	rows := make([][]InlineKeyboardButton, 0, len(ids)+len(attachTargets))
	for _, id := range ids {
		rows = append(rows, []InlineKeyboardButton{
			{Text: fmt.Sprintf("Watch [%d]", id), CallbackData: fmt.Sprintf("watch:%d", id)},
			{Text: fmt.Sprintf("Close [%d]", id), CallbackData: fmt.Sprintf("close:%d", id)},
		})
	}
	for _, target := range attachTargets {
		rows = append(rows, []InlineKeyboardButton{{
			Text:         "Attach " + target.Label,
			CallbackData: "attach:" + target.Target,
		}})
	}
	return &InlineKeyboardMarkup{InlineKeyboard: rows}
}

func (c *Client) postJSON(ctx context.Context, method string, payload any, out any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return c.requestError(method, "could not encode request")
	}
	return c.do(ctx, method, isOutboundMethod(method), func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/"+method, bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		return req, nil
	}, out)
}

func (c *Client) postForm(ctx context.Context, method string, values url.Values, out any) error {
	encoded := values.Encode()
	return c.do(ctx, method, false, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/"+method, strings.NewReader(encoded))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return req, nil
	}, out)
}

func (c *Client) do(ctx context.Context, method string, outbound bool, newRequest func() (*http.Request, error), out any) error {
	if outbound {
		if err := c.acquireOutbound(ctx); err != nil {
			return err
		}
		defer c.releaseOutbound()
	}

	for attempt := 0; ; attempt++ {
		if outbound {
			if err := c.waitOutboundTurn(ctx); err != nil {
				return err
			}
		}
		req, err := newRequest()
		if err != nil {
			return c.requestError(method, "could not create request")
		}
		err = c.doOnce(ctx, method, req, out)
		var telegramErr *Error
		if !errors.As(err, &telegramErr) || !telegramErr.IsRateLimited() || attempt >= maxRateLimitRetries {
			return err
		}
		if telegramErr.RetryAfter <= 0 || telegramErr.RetryAfter > maxRetryAfter {
			return err
		}
		if err := c.sleepRetry(ctx, telegramErr.RetryAfter); err != nil {
			return err
		}
	}
}

func (c *Client) doOnce(ctx context.Context, method string, req *http.Request, out any) error {
	resp, err := c.HTTPClient.Do(req)
	if req.Body != nil {
		_ = req.Body.Close()
	}
	if err != nil {
		if ctxErr := contextError(ctx, err); ctxErr != nil {
			return ctxErr
		}
		return c.requestError(method, "transport request failed")
	}
	defer resp.Body.Close()
	var envelope Response[json.RawMessage]
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return &Error{
			Method:      method,
			StatusCode:  resp.StatusCode,
			Description: "invalid Telegram response",
		}
	}
	if !envelope.OK || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if envelope.Description == "" {
			envelope.Description = http.StatusText(resp.StatusCode)
		}
		return &Error{
			Method:          method,
			StatusCode:      resp.StatusCode,
			ErrorCode:       envelope.ErrorCode,
			Description:     c.sanitize(envelope.Description, req),
			RetryAfter:      retryAfterDuration(envelope.Parameters.RetryAfter),
			MigrateToChatID: envelope.Parameters.MigrateToChatID,
		}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(envelope.Result, out); err != nil {
		return &Error{
			Method:      method,
			StatusCode:  resp.StatusCode,
			Description: "invalid Telegram result",
		}
	}
	return nil
}

func (c *Client) documentRequest(ctx context.Context, chatID int64, path, filename, caption string) (*http.Request, error) {
	return c.mediaRequest(ctx, "sendDocument", "document", chatID, path, filename, caption, 0, nil)
}

func (c *Client) mediaRequest(ctx context.Context, method, field string, chatID int64, path, filename, caption string, replyTo int, markup *InlineKeyboardMarkup) (*http.Request, error) {
	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/"+method, pr)
	if err != nil {
		_ = pr.Close()
		_ = pw.Close()
		return nil, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	go func() {
		var writeErr error
		defer func() {
			if writeErr != nil {
				_ = pw.CloseWithError(writeErr)
			} else {
				_ = pw.Close()
			}
		}()
		if writeErr = writer.WriteField("chat_id", strconv.FormatInt(chatID, 10)); writeErr != nil {
			return
		}
		if caption != "" {
			if writeErr = writer.WriteField("caption", clampCaption(caption)); writeErr != nil {
				return
			}
		}
		if replyTo > 0 {
			if writeErr = writer.WriteField("reply_to_message_id", strconv.Itoa(replyTo)); writeErr != nil {
				return
			}
		}
		if markup != nil {
			encoded, err := json.Marshal(markup)
			if err != nil {
				writeErr = err
				return
			}
			if writeErr = writer.WriteField("reply_markup", string(encoded)); writeErr != nil {
				return
			}
		}
		part, err := writer.CreateFormFile(field, filename)
		if err != nil {
			writeErr = err
			return
		}
		f, err := os.Open(path)
		if err != nil {
			writeErr = err
			return
		}
		defer f.Close()
		if _, writeErr = io.Copy(part, f); writeErr != nil {
			return
		}
		writeErr = writer.Close()
	}()
	return req, nil
}

func (c *Client) editPhotoRequest(ctx context.Context, chatID int64, messageID int, path, caption string, markup *InlineKeyboardMarkup) (*http.Request, error) {
	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/editMessageMedia", pr)
	if err != nil {
		_ = pr.Close()
		_ = pw.Close()
		return nil, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	go func() {
		var writeErr error
		defer func() {
			if writeErr != nil {
				_ = pw.CloseWithError(writeErr)
			} else {
				_ = pw.Close()
			}
		}()
		fields := map[string]string{
			"chat_id":    strconv.FormatInt(chatID, 10),
			"message_id": strconv.Itoa(messageID),
		}
		media, err := json.Marshal(map[string]any{"type": "photo", "media": "attach://photo", "caption": clampCaption(caption)})
		if err != nil {
			writeErr = err
			return
		}
		fields["media"] = string(media)
		if markup != nil {
			encoded, err := json.Marshal(markup)
			if err != nil {
				writeErr = err
				return
			}
			fields["reply_markup"] = string(encoded)
		}
		for _, name := range []string{"chat_id", "message_id", "media", "reply_markup"} {
			value, ok := fields[name]
			if !ok {
				continue
			}
			if writeErr = writer.WriteField(name, value); writeErr != nil {
				return
			}
		}
		part, err := writer.CreateFormFile("photo", "engram-window.png")
		if err != nil {
			writeErr = err
			return
		}
		f, err := os.Open(path)
		if err != nil {
			writeErr = err
			return
		}
		defer f.Close()
		if _, writeErr = io.Copy(part, f); writeErr != nil {
			return
		}
		writeErr = writer.Close()
	}()
	return req, nil
}

func safeDocumentFilename(filename string) string {
	filename = filepath.Base(strings.TrimSpace(filename))
	filename = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '/' || r == '\\' {
			return '_'
		}
		return r
	}, filename)
	if filename == "" || filename == "." {
		return "document"
	}
	return filename
}

func isOutboundMethod(method string) bool {
	switch method {
	case "sendMessage", "editMessageText", "editMessageCaption", "editMessageMedia", "sendDocument", "sendPhoto", "pinChatMessage", "unpinChatMessage", "deleteMessage":
		return true
	default:
		return false
	}
}

func (c *Client) acquireOutbound(ctx context.Context) error {
	c.outboundOnce.Do(func() {
		c.outboundSlots = make(chan struct{}, maxConcurrentOutbound)
	})
	select {
	case c.outboundSlots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) releaseOutbound() {
	<-c.outboundSlots
}

func (c *Client) waitOutboundTurn(ctx context.Context) error {
	if c.outboundInterval <= 0 {
		return nil
	}
	for {
		c.outboundMu.Lock()
		now := time.Now()
		if !now.Before(c.nextOutbound) {
			c.nextOutbound = now.Add(c.outboundInterval)
			c.outboundMu.Unlock()
			return nil
		}
		wait := time.Until(c.nextOutbound)
		c.outboundMu.Unlock()
		if err := sleepContext(ctx, wait); err != nil {
			return err
		}
	}
}

func (c *Client) sleepRetry(ctx context.Context, delay time.Duration) error {
	if c.retrySleep != nil {
		return c.retrySleep(ctx, delay)
	}
	return sleepContext(ctx, delay)
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func contextError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return nil
}

func (c *Client) requestError(method, description string) *Error {
	return &Error{Method: method, Description: c.sanitize(description, nil)}
}

func (c *Client) sanitize(description string, req *http.Request) string {
	if req != nil && req.URL != nil {
		description = strings.ReplaceAll(description, req.URL.String(), "[request URL]")
		description = strings.ReplaceAll(description, req.URL.Path, "[request path]")
		description = strings.ReplaceAll(description, req.URL.EscapedPath(), "[request path]")
	}
	if c.Token != "" {
		description = strings.ReplaceAll(description, c.Token, "[REDACTED]")
		description = strings.ReplaceAll(description, url.QueryEscape(c.Token), "[REDACTED]")
	}
	return description
}

func retryAfterDuration(seconds int) time.Duration {
	const maxDuration = time.Duration(1<<63 - 1)
	if seconds <= 0 {
		return 0
	}
	if int64(seconds) > int64(maxDuration/time.Second) {
		return maxDuration
	}
	return time.Duration(seconds) * time.Second
}

func clampText(text string) string {
	if len(text) <= 3900 {
		return text
	}
	cut := 3800
	for cut > 0 && !utf8.ValidString(text[:cut]) {
		cut--
	}
	return text[:cut] + "\n\n[truncated]"
}

func clampCaption(text string) string {
	if len(text) <= 1000 {
		return text
	}
	cut := 960
	for cut > 0 && !utf8.ValidString(text[:cut]) {
		cut--
	}
	return text[:cut] + "\n[truncated]"
}

func MarkdownToHTML(text string) string {
	var b strings.Builder
	for i := 0; i < len(text); {
		if strings.HasPrefix(text[i:], "```") {
			end := strings.Index(text[i+3:], "```")
			if end < 0 {
				b.WriteString(escapeHTML(text[i:]))
				break
			}
			code := text[i+3 : i+3+end]
			code = strings.TrimPrefix(code, "\n")
			code = strings.TrimSuffix(code, "\n")
			b.WriteString("<pre>")
			b.WriteString(escapeHTML(code))
			b.WriteString("</pre>")
			i += 3 + end + 3
			continue
		}
		if atLineStart(text, i) && text[i] == '>' {
			next, block := consumeBlockquote(text, i)
			b.WriteString("<blockquote>")
			b.WriteString(escapeHTML(block))
			b.WriteString("</blockquote>")
			i = next
			continue
		}
		if strings.HasPrefix(text[i:], "**") {
			if end := strings.Index(text[i+2:], "**"); end >= 0 {
				b.WriteString("<b>")
				b.WriteString(escapeHTML(text[i+2 : i+2+end]))
				b.WriteString("</b>")
				i += 2 + end + 2
				continue
			}
		}
		if text[i] == '`' {
			if end := strings.IndexByte(text[i+1:], '`'); end >= 0 {
				b.WriteString("<code>")
				b.WriteString(escapeHTML(text[i+1 : i+1+end]))
				b.WriteString("</code>")
				i += 1 + end + 1
				continue
			}
		}
		if text[i] == '*' {
			if end := strings.IndexByte(text[i+1:], '*'); end >= 0 && end > 0 {
				b.WriteString("<i>")
				b.WriteString(escapeHTML(text[i+1 : i+1+end]))
				b.WriteString("</i>")
				i += 1 + end + 1
				continue
			}
		}
		b.WriteString(escapeHTML(text[i : i+1]))
		i++
	}
	return b.String()
}

func atLineStart(text string, i int) bool {
	return i == 0 || text[i-1] == '\n'
}

func consumeBlockquote(text string, start int) (int, string) {
	var lines []string
	i := start
	for i < len(text) && atLineStart(text, i) && text[i] == '>' {
		lineEnd := strings.IndexByte(text[i:], '\n')
		end := len(text)
		if lineEnd >= 0 {
			end = i + lineEnd
		}
		line := strings.TrimPrefix(text[i:end], ">")
		line = strings.TrimPrefix(line, " ")
		lines = append(lines, line)
		i = end
		if i < len(text) && text[i] == '\n' {
			i++
		}
	}
	return i, strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

func escapeHTML(text string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return replacer.Replace(text)
}
