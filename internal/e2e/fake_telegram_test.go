package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/idolum-ai/engram/internal/telegram"
)

const (
	testBotToken = "e2e-token"
	testChatID   = int64(424242)
	testUserID   = int64(424242)
)

type telegramMessage struct {
	ID       int
	Text     string
	Caption  string
	Markup   telegram.InlineKeyboardMarkup
	Photo    []byte
	Document []byte
	Filename string
}

type fakeTelegramSnapshot struct {
	Messages        map[int]telegramMessage
	Calls           []string
	Pinned          map[int]bool
	CallbackAnswers []string
	Errors          []string
}

type fakeTelegram struct {
	server *httptest.Server

	mu              sync.Mutex
	updates         []telegram.Update
	wake            chan struct{}
	nextMessageID   int
	messages        map[int]telegramMessage
	calls           []string
	pinned          map[int]bool
	callbackAnswers []string
	errors          []string
}

func newFakeTelegram() *fakeTelegram {
	fake := &fakeTelegram{
		wake:          make(chan struct{}, 1),
		nextMessageID: 100,
		messages:      make(map[int]telegramMessage),
		pinned:        make(map[int]bool),
	}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serveHTTP))
	return fake
}

func (f *fakeTelegram) close() { f.server.Close() }

func (f *fakeTelegram) apiBase() string { return f.server.URL + "/telegram" }

func (f *fakeTelegram) queue(update telegram.Update) {
	f.mu.Lock()
	f.updates = append(f.updates, update)
	f.mu.Unlock()
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

func (f *fakeTelegram) snapshot() fakeTelegramSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	messages := make(map[int]telegramMessage, len(f.messages))
	for id, message := range f.messages {
		message.Photo = append([]byte(nil), message.Photo...)
		message.Document = append([]byte(nil), message.Document...)
		messages[id] = message
	}
	pinned := make(map[int]bool, len(f.pinned))
	for id, value := range f.pinned {
		pinned[id] = value
	}
	return fakeTelegramSnapshot{
		Messages:        messages,
		Calls:           append([]string(nil), f.calls...),
		Pinned:          pinned,
		CallbackAnswers: append([]string(nil), f.callbackAnswers...),
		Errors:          append([]string(nil), f.errors...),
	}
}

func (f *fakeTelegram) diagnostic() string {
	snapshot := f.snapshot()
	var messages []string
	for id, message := range snapshot.Messages {
		messages = append(messages, fmt.Sprintf("id=%d text=%q caption=%q photo=%d document=%d", id, message.Text, message.Caption, len(message.Photo), len(message.Document)))
	}
	return fmt.Sprintf("calls=%v messages=[%s] pinned=%v answers=%v errors=%v", snapshot.Calls, strings.Join(messages, "; "), snapshot.Pinned, snapshot.CallbackAnswers, snapshot.Errors)
}

func (f *fakeTelegram) serveHTTP(w http.ResponseWriter, r *http.Request) {
	const prefix = "/telegram/bot" + testBotToken + "/"
	if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, prefix) {
		f.fail(w, fmt.Sprintf("unexpected request %s %s", r.Method, r.URL.Path))
		return
	}
	method := strings.TrimPrefix(r.URL.Path, prefix)
	f.mu.Lock()
	f.calls = append(f.calls, method)
	f.mu.Unlock()

	switch method {
	case "getUpdates":
		f.getUpdates(w, r)
	case "setMyCommands":
		writeResult(w, true)
	case "sendMessage", "editMessageText", "editMessageCaption", "editMessageReplyMarkup":
		f.handleJSONMessage(w, r, method)
	case "editMessageMedia":
		f.handleEditMedia(w, r)
	case "sendPhoto":
		f.handleSendMedia(w, r, "photo")
	case "sendDocument":
		f.handleSendMedia(w, r, "document")
	case "pinChatMessage", "unpinChatMessage":
		f.handlePin(w, r, method == "pinChatMessage")
	case "deleteMessage":
		f.handleDelete(w, r)
	case "answerCallbackQuery":
		f.handleCallbackAnswer(w, r)
	default:
		f.fail(w, "unexpected Telegram method "+method)
	}
}

func (f *fakeTelegram) getUpdates(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		f.fail(w, "parse getUpdates: "+err.Error())
		return
	}
	offset, _ := strconv.Atoi(r.FormValue("offset"))
	updates := f.updatesAt(offset)
	if len(updates) == 0 {
		pollDelay := time.Second
		if seconds, err := strconv.Atoi(r.FormValue("timeout")); err == nil && seconds > 0 {
			pollDelay = min(time.Duration(seconds)*time.Second, time.Second)
		}
		select {
		case <-f.wake:
		case <-r.Context().Done():
		case <-time.After(pollDelay):
		}
		updates = f.updatesAt(offset)
	}
	writeResult(w, updates)
}

func (f *fakeTelegram) updatesAt(offset int) []telegram.Update {
	f.mu.Lock()
	defer f.mu.Unlock()
	updates := make([]telegram.Update, 0, len(f.updates))
	for _, update := range f.updates {
		if update.UpdateID >= offset {
			updates = append(updates, update)
		}
	}
	return updates
}

func (f *fakeTelegram) handleJSONMessage(w http.ResponseWriter, r *http.Request, method string) {
	var body struct {
		ChatID    int64                         `json:"chat_id"`
		MessageID int                           `json:"message_id"`
		Text      string                        `json:"text"`
		Caption   string                        `json:"caption"`
		Markup    telegram.InlineKeyboardMarkup `json:"reply_markup"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.fail(w, "decode "+method+": "+err.Error())
		return
	}
	f.mu.Lock()
	id := body.MessageID
	if id == 0 {
		id = f.nextMessageID
		f.nextMessageID++
	}
	message := f.messages[id]
	message.ID = id
	if body.Text != "" {
		message.Text = body.Text
	}
	if body.Caption != "" {
		message.Caption = body.Caption
	}
	if body.Markup.InlineKeyboard != nil {
		message.Markup = body.Markup
	}
	f.messages[id] = message
	f.mu.Unlock()
	writeResult(w, telegram.Message{MessageID: id, Chat: telegram.Chat{ID: body.ChatID, Type: "private"}})
}

func (f *fakeTelegram) handleEditMedia(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		f.fail(w, "parse editMessageMedia: "+err.Error())
		return
	}
	id, _ := strconv.Atoi(r.FormValue("message_id"))
	chatID, _ := strconv.ParseInt(r.FormValue("chat_id"), 10, 64)
	var media struct {
		Caption string `json:"caption"`
	}
	var markup telegram.InlineKeyboardMarkup
	if err := json.Unmarshal([]byte(r.FormValue("media")), &media); err != nil {
		f.fail(w, "decode edit media: "+err.Error())
		return
	}
	if err := decodeOptionalMarkup(r.FormValue("reply_markup"), &markup); err != nil {
		f.fail(w, "decode edit markup: "+err.Error())
		return
	}
	photo, _, err := readMultipartFile(r.MultipartForm, "photo")
	if err != nil {
		f.fail(w, "read edited photo: "+err.Error())
		return
	}
	f.mu.Lock()
	message := f.messages[id]
	message.ID = id
	message.Caption = media.Caption
	message.Markup = markup
	message.Photo = photo
	f.messages[id] = message
	f.mu.Unlock()
	writeResult(w, telegram.Message{MessageID: id, Chat: telegram.Chat{ID: chatID, Type: "private"}})
}

func (f *fakeTelegram) handleSendMedia(w http.ResponseWriter, r *http.Request, field string) {
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		f.fail(w, "parse send media: "+err.Error())
		return
	}
	chatID, _ := strconv.ParseInt(r.FormValue("chat_id"), 10, 64)
	data, filename, err := readMultipartFile(r.MultipartForm, field)
	if err != nil {
		f.fail(w, "read "+field+": "+err.Error())
		return
	}
	var markup telegram.InlineKeyboardMarkup
	if err := decodeOptionalMarkup(r.FormValue("reply_markup"), &markup); err != nil {
		f.fail(w, "decode send markup: "+err.Error())
		return
	}
	f.mu.Lock()
	id := f.nextMessageID
	f.nextMessageID++
	message := telegramMessage{ID: id, Caption: r.FormValue("caption"), Markup: markup, Filename: filename}
	if field == "photo" {
		message.Photo = data
	} else {
		message.Document = data
	}
	f.messages[id] = message
	f.mu.Unlock()
	writeResult(w, telegram.Message{MessageID: id, Chat: telegram.Chat{ID: chatID, Type: "private"}})
}

func (f *fakeTelegram) handlePin(w http.ResponseWriter, r *http.Request, pinned bool) {
	var body struct {
		MessageID int `json:"message_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.fail(w, "decode pin: "+err.Error())
		return
	}
	f.mu.Lock()
	f.pinned[body.MessageID] = pinned
	f.mu.Unlock()
	writeResult(w, true)
}

func (f *fakeTelegram) handleDelete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MessageID int `json:"message_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.fail(w, "decode delete: "+err.Error())
		return
	}
	f.mu.Lock()
	delete(f.messages, body.MessageID)
	delete(f.pinned, body.MessageID)
	f.mu.Unlock()
	writeResult(w, true)
}

func (f *fakeTelegram) handleCallbackAnswer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID   string `json:"callback_query_id"`
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.fail(w, "decode callback answer: "+err.Error())
		return
	}
	f.mu.Lock()
	f.callbackAnswers = append(f.callbackAnswers, body.ID+":"+body.Text)
	f.mu.Unlock()
	writeResult(w, true)
}

func (f *fakeTelegram) fail(w http.ResponseWriter, message string) {
	f.mu.Lock()
	f.errors = append(f.errors, message)
	f.mu.Unlock()
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "description": message})
}

func writeResult(w http.ResponseWriter, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

func decodeOptionalMarkup(value string, target *telegram.InlineKeyboardMarkup) error {
	if value == "" {
		return nil
	}
	return json.Unmarshal([]byte(value), target)
}

func readMultipartFile(form *multipart.Form, field string) ([]byte, string, error) {
	files := form.File[field]
	if len(files) != 1 {
		return nil, "", fmt.Errorf("field %s has %d files", field, len(files))
	}
	file, err := files[0].Open()
	if err != nil {
		return nil, "", err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 16<<20))
	return data, files[0].Filename, err
}
