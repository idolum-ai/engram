package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/idolum-ai/engram/internal/telegram"
)

const (
	testBotToken = "e2e-token"
	testChatID   = int64(424242)
	testUserID   = int64(424241)
)

type telegramMessage struct {
	ID       int
	ChatID   int64
	ReplyTo  int
	Text     string
	Caption  string
	Markup   telegram.InlineKeyboardMarkup
	Photo    []byte
	Document []byte
	Filename string
}

type telegramEvent struct {
	Method     string
	ChatID     int64
	MessageID  int
	CallbackID string
	Offset     int
}

type fakeTelegramSnapshot struct {
	Messages        map[int]telegramMessage
	Calls           []string
	Events          []telegramEvent
	Pinned          map[int]bool
	CallbackAnswers []string
	PollOffsets     []int
	Errors          []string
}

type fakeTelegram struct {
	server *httptest.Server

	mu              sync.Mutex
	updates         []telegram.Update
	wake            chan struct{}
	nextMessageID   int
	messages        map[int]telegramMessage
	events          []telegramEvent
	pinned          map[int]bool
	callbackAnswers []string
	pollOffsets     []int
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
	if update.Message != nil {
		message := update.Message
		f.messages[message.MessageID] = telegramMessage{
			ID:     message.MessageID,
			ChatID: message.Chat.ID,
			Text:   message.Text,
		}
	}
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
	events := append([]telegramEvent(nil), f.events...)
	calls := make([]string, 0, len(events))
	for _, event := range events {
		calls = append(calls, event.Method)
	}
	return fakeTelegramSnapshot{
		Messages:        messages,
		Calls:           calls,
		Events:          events,
		Pinned:          pinned,
		CallbackAnswers: append([]string(nil), f.callbackAnswers...),
		PollOffsets:     append([]int(nil), f.pollOffsets...),
		Errors:          append([]string(nil), f.errors...),
	}
}

func (f *fakeTelegram) diagnostic() string {
	snapshot := f.snapshot()
	ids := make([]int, 0, len(snapshot.Messages))
	for id := range snapshot.Messages {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	messages := make([]string, 0, len(ids))
	for _, id := range ids {
		message := snapshot.Messages[id]
		messages = append(messages, fmt.Sprintf("id=%d chat=%d reply_to=%d text=%q caption=%q photo=%d document=%d", id, message.ChatID, message.ReplyTo, message.Text, message.Caption, len(message.Photo), len(message.Document)))
	}
	counts := make(map[string]int)
	for _, call := range snapshot.Calls {
		counts[call]++
	}
	return fmt.Sprintf("calls=%v offsets=%v messages=[%s] pinned=%v answers=%v errors=%v", counts, snapshot.PollOffsets, strings.Join(messages, "; "), snapshot.Pinned, snapshot.CallbackAnswers, snapshot.Errors)
}

func (f *fakeTelegram) serveHTTP(w http.ResponseWriter, r *http.Request) {
	const prefix = "/telegram/bot" + testBotToken + "/"
	if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, prefix) {
		f.fail(w, fmt.Sprintf("unexpected request %s %s", r.Method, r.URL.Path))
		return
	}
	method := strings.TrimPrefix(r.URL.Path, prefix)
	switch method {
	case "getUpdates":
		f.getUpdates(w, r)
	case "setMyCommands":
		f.record(telegramEvent{Method: method})
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
		f.handlePin(w, r, method, method == "pinChatMessage")
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
	offset, err := strconv.Atoi(r.FormValue("offset"))
	if err != nil && r.FormValue("offset") != "" {
		f.fail(w, "invalid getUpdates offset")
		return
	}
	f.mu.Lock()
	f.pollOffsets = append(f.pollOffsets, offset)
	f.events = append(f.events, telegramEvent{Method: "getUpdates", Offset: offset})
	f.mu.Unlock()
	updates := f.updatesAt(offset)
	if len(updates) == 0 {
		pollDelay := time.Second
		if seconds, parseErr := strconv.Atoi(r.FormValue("timeout")); parseErr == nil && seconds > 0 {
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
		ChatID          int64                         `json:"chat_id"`
		MessageID       int                           `json:"message_id"`
		Text            string                        `json:"text"`
		Caption         string                        `json:"caption"`
		Markup          telegram.InlineKeyboardMarkup `json:"reply_markup"`
		ReplyParameters struct {
			MessageID int `json:"message_id"`
		} `json:"reply_parameters"`
		LinkPreview struct {
			Disabled bool `json:"is_disabled"`
		} `json:"link_preview_options"`
		LegacyReply   json.RawMessage `json:"reply_to_message_id"`
		LegacyPreview json.RawMessage `json:"disable_web_page_preview"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.fail(w, "decode "+method+": "+err.Error())
		return
	}
	if body.ChatID != testChatID {
		f.fail(w, fmt.Sprintf("%s targeted chat %d", method, body.ChatID))
		return
	}
	if (method == "sendMessage" || method == "editMessageText") && (len(body.LegacyPreview) != 0 || !body.LinkPreview.Disabled) {
		f.fail(w, method+" did not use disabled link_preview_options")
		return
	}
	f.mu.Lock()
	id := body.MessageID
	message := telegramMessage{}
	if method == "sendMessage" {
		if id != 0 {
			f.mu.Unlock()
			f.fail(w, "sendMessage supplied message_id")
			return
		}
		id = f.nextMessageID
		f.nextMessageID++
		message = telegramMessage{ID: id, ChatID: body.ChatID}
		if len(body.LegacyReply) != 0 {
			f.mu.Unlock()
			f.fail(w, "sendMessage used legacy reply_to_message_id")
			return
		}
		if body.ReplyParameters.MessageID != 0 {
			target, exists := f.messages[body.ReplyParameters.MessageID]
			if !exists || target.ChatID != body.ChatID {
				f.mu.Unlock()
				f.fail(w, fmt.Sprintf("sendMessage replied to unknown message %d", body.ReplyParameters.MessageID))
				return
			}
			message.ReplyTo = body.ReplyParameters.MessageID
		}
	} else {
		var ok bool
		message, ok = f.messages[id]
		if id <= 0 || !ok || message.ChatID != body.ChatID {
			f.mu.Unlock()
			f.fail(w, fmt.Sprintf("%s targeted unknown message %d", method, id))
			return
		}
	}
	switch method {
	case "sendMessage", "editMessageText":
		message.Text = body.Text
	case "editMessageCaption":
		message.Caption = body.Caption
	}
	if body.Markup.InlineKeyboard != nil {
		message.Markup = body.Markup
	}
	f.messages[id] = message
	f.events = append(f.events, telegramEvent{Method: method, ChatID: body.ChatID, MessageID: id})
	f.mu.Unlock()
	writeResult(w, telegram.Message{MessageID: id, Chat: telegram.Chat{ID: body.ChatID, Type: "private"}})
}

func (f *fakeTelegram) handleEditMedia(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		f.fail(w, "parse editMessageMedia: "+err.Error())
		return
	}
	id, chatID, ok := parseMediaTarget(r)
	if !ok || chatID != testChatID {
		f.fail(w, "editMessageMedia has an invalid target")
		return
	}
	var media struct {
		Type    string `json:"type"`
		Media   string `json:"media"`
		Caption string `json:"caption"`
	}
	if err := json.Unmarshal([]byte(r.FormValue("media")), &media); err != nil {
		f.fail(w, "decode edit media: "+err.Error())
		return
	}
	const attachPrefix = "attach://"
	if media.Type != "photo" || !strings.HasPrefix(media.Media, attachPrefix) || strings.TrimPrefix(media.Media, attachPrefix) == "" {
		f.fail(w, "editMessageMedia has an invalid photo descriptor")
		return
	}
	var markup telegram.InlineKeyboardMarkup
	if err := decodeOptionalMarkup(r.FormValue("reply_markup"), &markup); err != nil {
		f.fail(w, "decode edit markup: "+err.Error())
		return
	}
	photo, _, err := readMultipartFile(r.MultipartForm, strings.TrimPrefix(media.Media, attachPrefix))
	if err != nil {
		f.fail(w, "read edited photo: "+err.Error())
		return
	}
	f.mu.Lock()
	message, exists := f.messages[id]
	if !exists || message.ChatID != chatID {
		f.mu.Unlock()
		f.fail(w, fmt.Sprintf("editMessageMedia targeted unknown message %d", id))
		return
	}
	message.Caption = media.Caption
	message.Markup = markup
	message.Photo = photo
	f.messages[id] = message
	f.events = append(f.events, telegramEvent{Method: "editMessageMedia", ChatID: chatID, MessageID: id})
	f.mu.Unlock()
	writeResult(w, telegram.Message{MessageID: id, Chat: telegram.Chat{ID: chatID, Type: "private"}})
}

func (f *fakeTelegram) handleSendMedia(w http.ResponseWriter, r *http.Request, field string) {
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		f.fail(w, "parse send media: "+err.Error())
		return
	}
	chatID, err := strconv.ParseInt(r.FormValue("chat_id"), 10, 64)
	if err != nil || chatID != testChatID {
		f.fail(w, "send media has an invalid chat")
		return
	}
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
	replyTo, err := f.parseMediaReply(r, chatID)
	if err != nil {
		f.fail(w, "send media: "+err.Error())
		return
	}
	f.mu.Lock()
	id := f.nextMessageID
	f.nextMessageID++
	message := telegramMessage{ID: id, ChatID: chatID, ReplyTo: replyTo, Caption: r.FormValue("caption"), Markup: markup, Filename: filename}
	if field == "photo" {
		message.Photo = data
	} else {
		message.Document = data
	}
	f.messages[id] = message
	f.events = append(f.events, telegramEvent{Method: "send" + strings.ToUpper(field[:1]) + field[1:], ChatID: chatID, MessageID: id})
	f.mu.Unlock()
	writeResult(w, telegram.Message{MessageID: id, Chat: telegram.Chat{ID: chatID, Type: "private"}})
}

func (f *fakeTelegram) parseMediaReply(r *http.Request, chatID int64) (int, error) {
	if legacy := r.FormValue("reply_to_message_id"); legacy != "" {
		return 0, fmt.Errorf("used legacy reply_to_message_id")
	}
	raw := r.FormValue("reply_parameters")
	if raw == "" {
		return 0, nil
	}
	var reply struct {
		MessageID int `json:"message_id"`
	}
	if err := json.Unmarshal([]byte(raw), &reply); err != nil || reply.MessageID <= 0 {
		return 0, fmt.Errorf("invalid reply_parameters")
	}
	f.mu.Lock()
	target, exists := f.messages[reply.MessageID]
	f.mu.Unlock()
	if !exists || target.ChatID != chatID {
		return 0, fmt.Errorf("replied to unknown message %d", reply.MessageID)
	}
	return reply.MessageID, nil
}

func (f *fakeTelegram) handlePin(w http.ResponseWriter, r *http.Request, method string, pinned bool) {
	var body struct {
		ChatID    int64 `json:"chat_id"`
		MessageID int   `json:"message_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.fail(w, "decode pin: "+err.Error())
		return
	}
	f.mu.Lock()
	message, exists := f.messages[body.MessageID]
	if body.ChatID != testChatID || !exists || message.ChatID != body.ChatID {
		f.mu.Unlock()
		f.fail(w, fmt.Sprintf("%s targeted unknown message %d", method, body.MessageID))
		return
	}
	f.pinned[body.MessageID] = pinned
	f.events = append(f.events, telegramEvent{Method: method, ChatID: body.ChatID, MessageID: body.MessageID})
	f.mu.Unlock()
	writeResult(w, true)
}

func (f *fakeTelegram) handleDelete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ChatID    int64 `json:"chat_id"`
		MessageID int   `json:"message_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.fail(w, "decode delete: "+err.Error())
		return
	}
	f.mu.Lock()
	message, exists := f.messages[body.MessageID]
	if body.ChatID != testChatID || !exists || message.ChatID != body.ChatID {
		f.mu.Unlock()
		f.fail(w, fmt.Sprintf("deleteMessage targeted unknown message %d", body.MessageID))
		return
	}
	delete(f.messages, body.MessageID)
	delete(f.pinned, body.MessageID)
	f.events = append(f.events, telegramEvent{Method: "deleteMessage", ChatID: body.ChatID, MessageID: body.MessageID})
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
	if body.ID == "" {
		f.fail(w, "answerCallbackQuery omitted callback_query_id")
		return
	}
	f.mu.Lock()
	f.callbackAnswers = append(f.callbackAnswers, body.ID+":"+body.Text)
	f.events = append(f.events, telegramEvent{Method: "answerCallbackQuery", CallbackID: body.ID})
	f.mu.Unlock()
	writeResult(w, true)
}

func (f *fakeTelegram) record(event telegramEvent) {
	f.mu.Lock()
	f.events = append(f.events, event)
	f.mu.Unlock()
}

func (f *fakeTelegram) fail(w http.ResponseWriter, message string) {
	f.mu.Lock()
	f.errors = append(f.errors, message)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 400, "description": message})
}

func writeResult(w http.ResponseWriter, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

func parseMediaTarget(r *http.Request) (int, int64, bool) {
	id, idErr := strconv.Atoi(r.FormValue("message_id"))
	chatID, chatErr := strconv.ParseInt(r.FormValue("chat_id"), 10, 64)
	return id, chatID, idErr == nil && chatErr == nil && id > 0
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
