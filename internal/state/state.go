package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type TerminalState string

const (
	TerminalRunning TerminalState = "running"
	TerminalIdle    TerminalState = "idle"
	TerminalExited  TerminalState = "exited"
	TerminalLost    TerminalState = "lost"
	TerminalClosed  TerminalState = "closed"
	TerminalKilled  TerminalState = "killed"
)

type State struct {
	Version           int               `json:"version"`
	NextSessionID     int               `json:"next_session_id"`
	LastUpdateID      int               `json:"last_update_id"`
	LastPollAt        time.Time         `json:"last_poll_at,omitempty"`
	LastHaikuAt       time.Time         `json:"last_haiku_at,omitempty"`
	LastHaikuError    string            `json:"last_haiku_error,omitempty"`
	TerminalSessions  []TerminalSession `json:"terminal_sessions"`
	Attachments       []Attachment      `json:"attachments"`
	ProcessedMessages map[string]bool   `json:"processed_messages,omitempty"`
}

type TerminalSession struct {
	ID                 int           `json:"id"`
	ChatID             int64         `json:"chat_id"`
	CreatedByUserID    int64         `json:"created_by_user_id"`
	TmuxSessionName    string        `json:"tmux_session_name"`
	TmuxSessionID      string        `json:"tmux_session_id,omitempty"`
	TmuxWindowID       string        `json:"tmux_window_id"`
	TmuxPaneID         string        `json:"tmux_pane_id"`
	Title              string        `json:"title"`
	LastKnownCWD       string        `json:"last_known_cwd,omitempty"`
	State              TerminalState `json:"state"`
	CreatedAt          time.Time     `json:"created_at"`
	UpdatedAt          time.Time     `json:"updated_at"`
	LastActivityAt     time.Time     `json:"last_activity_at"`
	LastInputPreview   string        `json:"last_input_preview,omitempty"`
	LastInputMode      string        `json:"last_input_mode,omitempty"`
	LastRawCaptureHash string        `json:"last_raw_capture_hash,omitempty"`
	LastSummaryHash    string        `json:"last_summary_hash,omitempty"`
	LastRenderHash     string        `json:"last_render_hash,omitempty"`
	LastSummary        string        `json:"last_summary,omitempty"`
	LastSummaryModel   string        `json:"last_summary_model,omitempty"`
	AnchorChatID       int64         `json:"anchor_chat_id,omitempty"`
	AnchorMessageID    int           `json:"anchor_message_id,omitempty"`
	WatchEnabled       bool          `json:"watch_enabled"`
	LastAnchorEditAt   time.Time     `json:"last_anchor_edit_at,omitempty"`
	SummaryInFlight    bool          `json:"-"`
	PendingRefresh     bool          `json:"pending_refresh,omitempty"`
	LastTelegramError  string        `json:"last_telegram_error,omitempty"`
	LastRawCapture     string        `json:"last_raw_capture,omitempty"`
}

type Attachment struct {
	ID                   int       `json:"id"`
	TelegramFileID       string    `json:"telegram_file_id"`
	TelegramUniqueFileID string    `json:"telegram_unique_file_id,omitempty"`
	ChatID               int64     `json:"chat_id"`
	UserID               int64     `json:"user_id"`
	OriginalName         string    `json:"original_name"`
	ContentType          string    `json:"content_type,omitempty"`
	SizeBytes            int64     `json:"size_bytes"`
	SHA256               string    `json:"sha256,omitempty"`
	StoredPath           string    `json:"stored_path"`
	ReceivedAt           time.Time `json:"received_at"`
	BypassRequested      bool      `json:"bypass_requested"`
}

type Store struct {
	mu        sync.Mutex
	path      string
	auditPath string
	state     State
}

func Open(path, auditPath string) (*Store, error) {
	s := &Store{path: path, auditPath: auditPath}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(auditPath), 0o700); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			s.state = State{Version: 1, NextSessionID: 1, ProcessedMessages: map[string]bool{}}
			return s, s.Save()
		}
		return nil, err
	}
	if len(b) == 0 {
		s.state = State{Version: 1, NextSessionID: 1, ProcessedMessages: map[string]bool{}}
		return s, nil
	}
	if err := json.Unmarshal(b, &s.state); err != nil {
		return nil, fmt.Errorf("parse state: %w", err)
	}
	if s.state.Version == 0 {
		s.state.Version = 1
	}
	if s.state.NextSessionID == 0 {
		s.state.NextSessionID = maxSessionID(s.state.TerminalSessions) + 1
	}
	if s.state.ProcessedMessages == nil {
		s.state.ProcessedMessages = map[string]bool{}
	}
	return s, nil
}

func (s *Store) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneState(s.state)
}

func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Store) Audit(eventType, status string, payload any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := map[string]any{
		"at":     time.Now().UTC().Format(time.RFC3339Nano),
		"type":   eventType,
		"status": status,
		"data":   payload,
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.auditPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

func (s *Store) AllocateSession(chatID, userID int64, tmuxSessionName, windowID, paneID, title string) (TerminalSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	id := s.state.NextSessionID
	s.state.NextSessionID++
	ts := TerminalSession{
		ID:              id,
		ChatID:          chatID,
		CreatedByUserID: userID,
		TmuxSessionName: tmuxSessionName,
		TmuxWindowID:    windowID,
		TmuxPaneID:      paneID,
		Title:           title,
		State:           TerminalRunning,
		CreatedAt:       now,
		UpdatedAt:       now,
		LastActivityAt:  now,
		WatchEnabled:    true,
	}
	s.state.TerminalSessions = append(s.state.TerminalSessions, ts)
	return ts, s.saveLocked()
}

func (s *Store) UpdateSession(id int, fn func(*TerminalSession)) (TerminalSession, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.TerminalSessions {
		if s.state.TerminalSessions[i].ID == id {
			fn(&s.state.TerminalSessions[i])
			s.state.TerminalSessions[i].UpdatedAt = time.Now().UTC()
			ts := s.state.TerminalSessions[i]
			return ts, true, s.saveLocked()
		}
	}
	return TerminalSession{}, false, nil
}

func (s *Store) FindSession(id int) (TerminalSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ts := range s.state.TerminalSessions {
		if ts.ID == id {
			return ts, true
		}
	}
	return TerminalSession{}, false
}

func (s *Store) FindByAnchor(chatID int64, messageID int) (TerminalSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ts := range s.state.TerminalSessions {
		if ts.AnchorChatID == chatID && ts.AnchorMessageID == messageID {
			return ts, true
		}
	}
	return TerminalSession{}, false
}

func (s *Store) FindByPane(paneID string) (TerminalSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ts := range s.state.TerminalSessions {
		if ts.TmuxPaneID == paneID && ts.State != TerminalClosed {
			return ts, true
		}
	}
	return TerminalSession{}, false
}

func (s *Store) MarkPoll(updateID int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if updateID > s.state.LastUpdateID {
		s.state.LastUpdateID = updateID
	}
	s.state.LastPollAt = time.Now().UTC()
	return s.saveLocked()
}

func (s *Store) SeenMessage(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.ProcessedMessages[key]
}

func (s *Store) MarkMessage(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.ProcessedMessages[key] = true
	return s.saveLocked()
}

func (s *Store) AddAttachment(a Attachment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a.ID = len(s.state.Attachments) + 1
	s.state.Attachments = append(s.state.Attachments, a)
	return s.saveLocked()
}

func (s *Store) NoteHaiku(errText string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.LastHaikuAt = time.Now().UTC()
	s.state.LastHaikuError = errText
	return s.saveLocked()
}

func maxSessionID(sessions []TerminalSession) int {
	max := 0
	for _, s := range sessions {
		if s.ID > max {
			max = s.ID
		}
	}
	return max
}

func cloneState(in State) State {
	out := in
	out.TerminalSessions = append([]TerminalSession(nil), in.TerminalSessions...)
	out.Attachments = append([]Attachment(nil), in.Attachments...)
	out.ProcessedMessages = make(map[string]bool, len(in.ProcessedMessages))
	for k, v := range in.ProcessedMessages {
		out.ProcessedMessages[k] = v
	}
	return out
}
