package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	BaseURL    string
	FileBase   string
	Token      string
	HTTPClient *http.Client
}

func New(token string) *Client {
	return &Client{
		BaseURL:  "https://api.telegram.org/bot" + token,
		FileBase: "https://api.telegram.org/file/bot" + token,
		Token:    token,
		HTTPClient: &http.Client{
			Timeout: 70 * time.Second,
		},
	}
}

type Response[T any] struct {
	OK          bool   `json:"ok"`
	Description string `json:"description,omitempty"`
	Result      T      `json:"result"`
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

func (c *Client) AnswerCallback(ctx context.Context, id string, text string) error {
	body := map[string]any{"callback_query_id": id}
	if text != "" {
		body["text"] = text
	}
	var out bool
	return c.postJSON(ctx, "answerCallbackQuery", body, &out)
}

func (c *Client) GetFile(ctx context.Context, fileID string) (File, error) {
	v := url.Values{}
	v.Set("file_id", fileID)
	var out File
	return out, c.postForm(ctx, "getFile", v, &out)
}

func (c *Client) DownloadFile(ctx context.Context, filePath, dest string, maxBytes int64) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.FileBase+"/"+filePath, nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("telegram file download status %s", resp.Status)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return 0, err
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var r io.Reader = resp.Body
	if maxBytes > 0 {
		r = io.LimitReader(resp.Body, maxBytes+1)
	}
	n, err := io.Copy(f, r)
	if err != nil {
		return n, err
	}
	if maxBytes > 0 && n > maxBytes {
		_ = os.Remove(dest)
		return n, fmt.Errorf("download exceeded max bytes")
	}
	return n, nil
}

func (c *Client) SendDocument(ctx context.Context, chatID int64, path string, caption string) (Message, error) {
	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)
	go func() {
		var err error
		defer func() {
			if err != nil {
				_ = pw.CloseWithError(err)
			} else {
				_ = pw.Close()
			}
		}()
		if err = writer.WriteField("chat_id", strconv.FormatInt(chatID, 10)); err != nil {
			return
		}
		if caption != "" {
			if err = writer.WriteField("caption", clampText(caption)); err != nil {
				return
			}
		}
		var part io.Writer
		part, err = writer.CreateFormFile("document", filepath.Base(path))
		if err != nil {
			return
		}
		var f *os.File
		f, err = os.Open(path)
		if err != nil {
			return
		}
		defer f.Close()
		if _, err = io.Copy(part, f); err != nil {
			return
		}
		err = writer.Close()
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/sendDocument", pr)
	if err != nil {
		return Message{}, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	var out Message
	return out, c.do(req, &out)
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

func RefreshMarkup(sessionID int) *InlineKeyboardMarkup {
	return &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{
		{{Text: "🔄", CallbackData: fmt.Sprintf("refresh:%d", sessionID)}},
		{
			{Text: "Esc", CallbackData: fmt.Sprintf("key:%d:esc", sessionID)},
			{Text: "Esc Esc", CallbackData: fmt.Sprintf("key:%d:esc2", sessionID)},
			{Text: "Ctrl+C", CallbackData: fmt.Sprintf("key:%d:ctrl-c", sessionID)},
			{Text: "Ctrl+D", CallbackData: fmt.Sprintf("key:%d:ctrl-d", sessionID)},
			{Text: "Enter", CallbackData: fmt.Sprintf("key:%d:enter", sessionID)},
		},
	}}
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
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/"+method, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, out)
}

func (c *Client) postForm(ctx context.Context, method string, values url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/"+method, strings.NewReader(values.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return c.do(req, out)
}

func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var envelope Response[json.RawMessage]
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return err
	}
	if !envelope.OK {
		if envelope.Description == "" {
			envelope.Description = resp.Status
		}
		return fmt.Errorf("telegram %s: %s", req.URL.Path, envelope.Description)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(envelope.Result, out)
}

func clampText(text string) string {
	if len(text) <= 3900 {
		return text
	}
	return text[:3800] + "\n\n[truncated]"
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
