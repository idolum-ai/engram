package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type TerminalState string
type TerminalOrigin string

const (
	TerminalRunning TerminalState = "running"
	TerminalLost    TerminalState = "lost"
	TerminalClosed  TerminalState = "closed"
)

const (
	TerminalOriginCreated  TerminalOrigin = "created"
	TerminalOriginAttached TerminalOrigin = "attached"
)

type State struct {
	Version            int                `json:"version"`
	AnchorMode         string             `json:"anchor_mode,omitempty"`
	NextSessionID      int                `json:"next_session_id"`
	LastUpdateID       int                `json:"last_update_id"`
	LastPollAt         time.Time          `json:"last_poll_at,omitempty"`
	LastHaikuAt        time.Time          `json:"last_haiku_at,omitempty"`
	LastHaikuError     string             `json:"last_haiku_error,omitempty"`
	TerminalSessions   []TerminalSession  `json:"terminal_sessions"`
	Attachments        []Attachment       `json:"attachments"`
	AttachmentBypasses []AttachmentBypass `json:"attachment_bypasses,omitempty"`
	UpdateJournal      []UpdateEvent      `json:"update_journal,omitempty"`
	ProcessedMessages  map[string]bool    `json:"processed_messages,omitempty"`
}

type TerminalSession struct {
	ID                       int            `json:"id"`
	TmuxSessionName          string         `json:"tmux_session_name"`
	TmuxWindowID             string         `json:"tmux_window_id"`
	TmuxPaneID               string         `json:"tmux_pane_id"`
	Origin                   TerminalOrigin `json:"origin,omitempty"`
	Title                    string         `json:"title"`
	LastKnownCWD             string         `json:"last_known_cwd,omitempty"`
	State                    TerminalState  `json:"state"`
	CreatedAt                time.Time      `json:"created_at"`
	UpdatedAt                time.Time      `json:"updated_at"`
	LastActivityAt           time.Time      `json:"last_activity_at"`
	LastRawCaptureHash       string         `json:"last_raw_capture_hash,omitempty"`
	LastSnapshotCaptureHash  string         `json:"last_snapshot_capture_hash,omitempty"`
	LastSnapshotAttemptAt    time.Time      `json:"last_snapshot_attempt_at,omitempty"`
	LastRenderHash           string         `json:"last_render_hash,omitempty"`
	LastSummary              string         `json:"last_summary,omitempty"`
	SummaryMessageID         int            `json:"summary_message_id,omitempty"`
	SnapshotMessageID        int            `json:"snapshot_message_id,omitempty"`
	StaleAlternateMessageIDs []int          `json:"stale_alternate_message_ids,omitempty"`
	AnchorChatID             int64          `json:"anchor_chat_id,omitempty"`
	AnchorMessageID          int            `json:"anchor_message_id,omitempty"`
	AnchorFormat             string         `json:"anchor_format,omitempty"`
	RetiringAnchorMessageID  int            `json:"retiring_anchor_message_id,omitempty"`
	RetiringAnchorFormat     string         `json:"retiring_anchor_format,omitempty"`
	RetiringAnchorRetryAt    time.Time      `json:"retiring_anchor_retry_at,omitempty"`
	AnchorPinned             bool           `json:"anchor_pinned,omitempty"`
	AnchorPinKnown           bool           `json:"anchor_pin_known,omitempty"`
	WatchEnabled             bool           `json:"watch_enabled"`
	LastAnchorEditAt         time.Time      `json:"last_anchor_edit_at,omitempty"`
	LastRawCapture           string         `json:"last_raw_capture,omitempty"`
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

type AttachmentBypass struct {
	ChatID    int64     `json:"chat_id"`
	UserID    int64     `json:"user_id"`
	SHA256    string    `json:"sha256"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	UsedAt    time.Time `json:"used_at,omitempty"`
}

type UpdateRefs struct {
	ChatID    int64 `json:"chat_id,omitempty"`
	UserID    int64 `json:"user_id,omitempty"`
	MessageID int   `json:"message_id,omitempty"`
}

type UpdateEvent struct {
	UpdateID  int       `json:"update_id"`
	Kind      string    `json:"kind"`
	Status    string    `json:"status"`
	Reason    string    `json:"reason,omitempty"`
	ChatID    int64     `json:"chat_id,omitempty"`
	UserID    int64     `json:"user_id,omitempty"`
	MessageID int       `json:"message_id,omitempty"`
	At        time.Time `json:"at"`
}

type Store struct {
	mu                    sync.Mutex
	path                  string
	auditPath             string
	state                 State
	processedMessageOrder []string
}

const (
	currentStateVersion   = 6
	maxTerminalSessions   = 200
	maxAttachments        = 200
	maxAttachmentBypasses = 100
	maxUpdateJournal      = 200
	maxStaleAlternates    = 16
	maxProcessedMessages  = 2_000
	maxAuditFileBytes     = int64(4 << 20)
	maxAuditRecordBytes   = 64 << 10
)

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
			s.state = newState()
			return s, s.Save()
		}
		return nil, err
	}
	if len(b) == 0 {
		s.state = newState()
		return s, s.Save()
	}
	if err := json.Unmarshal(b, &s.state); err != nil {
		backup := path + ".corrupt-" + time.Now().UTC().Format("20060102T150405.000000000Z")
		if renameErr := os.Rename(path, backup); renameErr != nil {
			return nil, fmt.Errorf("parse state: %w; backup failed: %v", err, renameErr)
		}
		s.state = newState()
		if saveErr := s.Save(); saveErr != nil {
			return nil, fmt.Errorf("parse state: %w; backup at %s; initialize replacement: %v", err, backup, saveErr)
		}
		_ = s.Audit("state.recover", "corrupt_replaced", map[string]any{"backup": backup})
		return s, nil
	}
	if s.state.Version > currentStateVersion {
		return nil, fmt.Errorf("state schema version %d is newer than supported version %d", s.state.Version, currentStateVersion)
	}
	s.state.Version = currentStateVersion
	if s.state.AnchorMode != "guide" && s.state.AnchorMode != "snapshot" {
		s.state.AnchorMode = ""
	}
	normalizeTerminalSessions(s.state.TerminalSessions)
	if s.state.NextSessionID == 0 {
		s.state.NextSessionID = maxSessionID(s.state.TerminalSessions) + 1
	}
	if s.state.ProcessedMessages == nil {
		s.state.ProcessedMessages = map[string]bool{}
	}
	s.initializeProcessedMessageOrderLocked()
	s.pruneStateLocked(time.Now().UTC())
	return s, nil
}

func newState() State {
	return State{Version: currentStateVersion, NextSessionID: 1, ProcessedMessages: map[string]bool{}}
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

func (s *Store) SetAnchorMode(mode string) error {
	if mode != "guide" && mode != "snapshot" {
		return fmt.Errorf("invalid anchor mode %q", mode)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.state.AnchorMode
	s.state.AnchorMode = mode
	if err := s.saveLocked(); err != nil {
		s.state.AnchorMode = previous
		return err
	}
	return nil
}

func (s *Store) saveLocked() error {
	s.pruneStateLocked(time.Now().UTC())
	persisted := cloneState(s.state)
	for i := range persisted.TerminalSessions {
		persisted.TerminalSessions[i].LastRawCapture = ""
	}
	b, err := json.MarshalIndent(persisted, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.path, b)
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create state temp: %w", err)
	}
	tmpPath := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := tmp.Chmod(0o600); err != nil {
		closeErr := tmp.Close()
		return fmt.Errorf("set state temp permissions: %w", errors.Join(err, closeErr))
	}
	if _, err := tmp.Write(data); err != nil {
		closeErr := tmp.Close()
		return fmt.Errorf("write state temp: %w", errors.Join(err, closeErr))
	}
	if err := tmp.Sync(); err != nil {
		closeErr := tmp.Close()
		return fmt.Errorf("sync state temp: %w", errors.Join(err, closeErr))
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close state temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace state: %w", err)
	}
	renamed = true
	if err := syncParentDir(path); err != nil {
		return err
	}
	return nil
}

func syncParentDir(path string) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("open state directory for sync: %w", err)
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	// Darwin filesystems may reject Sync on a directory descriptor even though
	// the regular state file was fully synced before rename.
	if runtime.GOOS == "darwin" && (errors.Is(syncErr, syscall.EINVAL) || errors.Is(syncErr, syscall.ENOTSUP)) {
		syncErr = nil
	}
	if err := errors.Join(syncErr, closeErr); err != nil {
		return fmt.Errorf("sync state directory: %w", err)
	}
	return nil
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
	if len(b)+1 > maxAuditRecordBytes {
		rec["data"] = map[string]any{
			"omitted":        "audit payload exceeded record limit",
			"original_bytes": len(b),
		}
		b, err = json.Marshal(rec)
		if err != nil {
			return err
		}
	}
	line := append(b, '\n')
	if err := rotateAuditIfNeeded(s.auditPath, int64(len(line))); err != nil {
		return err
	}
	f, err := os.OpenFile(s.auditPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	_, err = f.Write(line)
	return err
}

func rotateAuditIfNeeded(path string, incomingBytes int64) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("audit path is not a regular file")
	}
	if info.Size()+incomingBytes <= maxAuditFileBytes {
		return nil
	}
	backup := path + ".1"
	if info.Size() <= maxAuditFileBytes {
		if err := os.Chmod(path, 0o600); err != nil {
			return err
		}
		if err := os.Remove(backup); err != nil && !os.IsNotExist(err) {
			return err
		}
		return os.Rename(path, backup)
	}

	tail, err := boundedAuditTail(path, maxAuditFileBytes)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".audit-rotate-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(tail); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Remove(backup); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(tmpPath, backup); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return nil
}

func boundedAuditTail(path string, maxBytes int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	offset := info.Size() - maxBytes
	if offset < 0 {
		offset = 0
	}
	if _, err := f.Seek(offset, 0); err != nil {
		return nil, err
	}
	tail, err := io.ReadAll(io.LimitReader(f, maxBytes))
	if err != nil {
		return nil, err
	}
	if offset > 0 {
		if newline := bytes.IndexByte(tail, '\n'); newline >= 0 {
			tail = tail[newline+1:]
		} else {
			tail = nil
		}
	}
	if len(tail) > 0 && tail[len(tail)-1] != '\n' {
		if newline := bytes.LastIndexByte(tail, '\n'); newline >= 0 {
			tail = tail[:newline+1]
		} else {
			tail = nil
		}
	}
	return tail, nil
}

func (s *Store) AllocateSession(tmuxSessionName, windowID, paneID, title string) (TerminalSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	id := s.state.NextSessionID
	s.state.NextSessionID++
	ts := TerminalSession{
		ID:              id,
		TmuxSessionName: tmuxSessionName,
		TmuxWindowID:    windowID,
		TmuxPaneID:      paneID,
		Title:           title,
		State:           TerminalRunning,
		CreatedAt:       now,
		UpdatedAt:       now,
		LastActivityAt:  now,
		WatchEnabled:    false,
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
			ts := cloneTerminalSession(s.state.TerminalSessions[i])
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
			return cloneTerminalSession(ts), true
		}
	}
	return TerminalSession{}, false
}

func (s *Store) FindByAnchor(chatID int64, messageID int) (TerminalSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ts := range s.state.TerminalSessions {
		if ts.AnchorChatID == chatID && ts.AnchorMessageID == messageID {
			return cloneTerminalSession(ts), true
		}
	}
	return TerminalSession{}, false
}

type ReplyTargetState string

const (
	ReplyTargetCurrent ReplyTargetState = "current"
	ReplyTargetStale   ReplyTargetState = "stale"
)

func (s *Store) FindReplyTarget(chatID int64, messageID int) (TerminalSession, ReplyTargetState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ts := range s.state.TerminalSessions {
		if ts.AnchorChatID != chatID {
			continue
		}
		if ts.AnchorMessageID == messageID || ts.SummaryMessageID == messageID || ts.SnapshotMessageID == messageID {
			return cloneTerminalSession(ts), ReplyTargetCurrent, true
		}
		if ts.RetiringAnchorMessageID == messageID {
			return cloneTerminalSession(ts), ReplyTargetStale, true
		}
		for _, staleID := range ts.StaleAlternateMessageIDs {
			if staleID == messageID {
				return cloneTerminalSession(ts), ReplyTargetStale, true
			}
		}
	}
	return TerminalSession{}, "", false
}

func (s *Store) FindByPane(paneID string) (TerminalSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ts := range s.state.TerminalSessions {
		if ts.TmuxPaneID == paneID && ts.State != TerminalClosed {
			return cloneTerminalSession(ts), true
		}
	}
	return TerminalSession{}, false
}

// MarkPoll advances the Telegram offset before handling the update. This gives
// shell input at-most-once delivery after a crash instead of risking replayed
// commands into tmux.
func (s *Store) MarkPoll(updateID int, kind string, refs UpdateRefs) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if updateID > s.state.LastUpdateID {
		s.state.LastUpdateID = updateID
	}
	s.state.LastPollAt = time.Now().UTC()
	s.appendUpdateLocked(UpdateEvent{
		UpdateID:  updateID,
		Kind:      kind,
		Status:    "accepted",
		ChatID:    refs.ChatID,
		UserID:    refs.UserID,
		MessageID: refs.MessageID,
		At:        s.state.LastPollAt,
	})
	return s.saveLocked()
}

func (s *Store) RecordUpdate(updateID int, kind string, status string, reason string, refs UpdateRefs) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appendUpdateLocked(UpdateEvent{
		UpdateID:  updateID,
		Kind:      kind,
		Status:    status,
		Reason:    reason,
		ChatID:    refs.ChatID,
		UserID:    refs.UserID,
		MessageID: refs.MessageID,
		At:        time.Now().UTC(),
	})
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
	if !s.state.ProcessedMessages[key] {
		s.state.ProcessedMessages[key] = true
		s.processedMessageOrder = append(s.processedMessageOrder, key)
	}
	return s.saveLocked()
}

func (s *Store) AddAttachment(a Attachment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a.ID = maxAttachmentID(s.state.Attachments) + 1
	s.state.Attachments = append(s.state.Attachments, a)
	return s.saveLocked()
}

func (s *Store) AddAttachmentBypass(bypass AttachmentBypass) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneAttachmentBypassesLocked(time.Now().UTC())
	s.state.AttachmentBypasses = append(s.state.AttachmentBypasses, bypass)
	return s.saveLocked()
}

func (s *Store) FindAttachmentBypass(chatID, userID int64) (AttachmentBypass, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	for _, bypass := range s.state.AttachmentBypasses {
		if bypass.ChatID == chatID && bypass.UserID == userID && bypass.UsedAt.IsZero() && bypass.ExpiresAt.After(now) {
			return bypass, true
		}
	}
	return AttachmentBypass{}, false
}

func (s *Store) ConsumeAttachmentBypass(chatID, userID int64, sha256 string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	for i := range s.state.AttachmentBypasses {
		bypass := &s.state.AttachmentBypasses[i]
		if bypass.ChatID == chatID && bypass.UserID == userID && bypass.SHA256 == sha256 && bypass.UsedAt.IsZero() && bypass.ExpiresAt.After(now) {
			bypass.UsedAt = now
			return s.saveLocked()
		}
	}
	return nil
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

func (s *Store) appendUpdateLocked(event UpdateEvent) {
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	s.state.UpdateJournal = append(s.state.UpdateJournal, event)
	if len(s.state.UpdateJournal) > maxUpdateJournal {
		s.state.UpdateJournal = append([]UpdateEvent(nil), s.state.UpdateJournal[len(s.state.UpdateJournal)-maxUpdateJournal:]...)
	}
}

func (s *Store) pruneAttachmentBypassesLocked(now time.Time) {
	out := s.state.AttachmentBypasses[:0]
	for _, bypass := range s.state.AttachmentBypasses {
		if !bypass.UsedAt.IsZero() || !bypass.ExpiresAt.After(now) {
			continue
		}
		out = append(out, bypass)
	}
	s.state.AttachmentBypasses = out
	if len(s.state.AttachmentBypasses) > maxAttachmentBypasses {
		sort.SliceStable(s.state.AttachmentBypasses, func(i, j int) bool {
			return s.state.AttachmentBypasses[i].CreatedAt.Before(s.state.AttachmentBypasses[j].CreatedAt)
		})
		s.state.AttachmentBypasses = append([]AttachmentBypass(nil), s.state.AttachmentBypasses[len(s.state.AttachmentBypasses)-maxAttachmentBypasses:]...)
	}
}

func (s *Store) pruneStateLocked(now time.Time) {
	if s.state.ProcessedMessages == nil {
		s.state.ProcessedMessages = map[string]bool{}
	}
	if len(s.processedMessageOrder) == 0 && len(s.state.ProcessedMessages) != 0 {
		s.initializeProcessedMessageOrderLocked()
	}
	s.pruneProcessedMessagesLocked()
	s.pruneAttachmentBypassesLocked(now)

	if len(s.state.UpdateJournal) > maxUpdateJournal {
		s.state.UpdateJournal = append([]UpdateEvent(nil), s.state.UpdateJournal[len(s.state.UpdateJournal)-maxUpdateJournal:]...)
	}
	if len(s.state.Attachments) > maxAttachments {
		sort.SliceStable(s.state.Attachments, func(i, j int) bool {
			a, b := s.state.Attachments[i], s.state.Attachments[j]
			if a.ReceivedAt.Equal(b.ReceivedAt) {
				return a.ID < b.ID
			}
			return a.ReceivedAt.Before(b.ReceivedAt)
		})
		s.state.Attachments = append([]Attachment(nil), s.state.Attachments[len(s.state.Attachments)-maxAttachments:]...)
	}
	if len(s.state.TerminalSessions) > maxTerminalSessions {
		s.state.TerminalSessions = pruneTerminalSessions(s.state.TerminalSessions)
	}
}

func (s *Store) initializeProcessedMessageOrderLocked() {
	s.processedMessageOrder = s.processedMessageOrder[:0]
	for key, processed := range s.state.ProcessedMessages {
		if !processed {
			delete(s.state.ProcessedMessages, key)
			continue
		}
		s.processedMessageOrder = append(s.processedMessageOrder, key)
	}
	sort.Slice(s.processedMessageOrder, func(i, j int) bool {
		return processedMessageLess(s.processedMessageOrder[i], s.processedMessageOrder[j])
	})
}

func (s *Store) pruneProcessedMessagesLocked() {
	if len(s.processedMessageOrder) <= maxProcessedMessages {
		return
	}
	remove := len(s.processedMessageOrder) - maxProcessedMessages
	for _, key := range s.processedMessageOrder[:remove] {
		delete(s.state.ProcessedMessages, key)
	}
	s.processedMessageOrder = append([]string(nil), s.processedMessageOrder[remove:]...)
}

func processedMessageLess(a, b string) bool {
	aID, aOK := messageIDFromKey(a)
	bID, bOK := messageIDFromKey(b)
	if aOK && bOK && aID != bID {
		return aID < bID
	}
	if aOK != bOK {
		return !aOK
	}
	return a < b
}

func messageIDFromKey(key string) (int64, bool) {
	separator := strings.LastIndexByte(key, ':')
	if separator < 0 || separator == len(key)-1 {
		return 0, false
	}
	id, err := strconv.ParseInt(key[separator+1:], 10, 64)
	return id, err == nil
}

func pruneTerminalSessions(sessions []TerminalSession) []TerminalSession {
	indices := make([]int, len(sessions))
	for i := range indices {
		indices[i] = i
	}
	sort.SliceStable(indices, func(i, j int) bool {
		a, b := sessions[indices[i]], sessions[indices[j]]
		aActive := a.State == TerminalRunning
		bActive := b.State == TerminalRunning
		if aActive != bActive {
			return aActive
		}
		aTime, bTime := sessionRecency(a), sessionRecency(b)
		if !aTime.Equal(bTime) {
			return aTime.After(bTime)
		}
		return a.ID > b.ID
	})
	keep := make([]bool, len(sessions))
	for _, index := range indices[:maxTerminalSessions] {
		keep[index] = true
	}
	out := make([]TerminalSession, 0, maxTerminalSessions)
	for i, session := range sessions {
		if keep[i] {
			out = append(out, session)
		}
	}
	return out
}

func normalizeTerminalSessions(sessions []TerminalSession) {
	for i := range sessions {
		session := &sessions[i]
		// Pin state is reconciled with Telegram after every process start.
		session.AnchorPinKnown = false
		if session.AnchorMessageID == 0 || session.RetiringAnchorMessageID == session.AnchorMessageID {
			session.RetiringAnchorMessageID = 0
			session.RetiringAnchorFormat = ""
		}
		if session.AnchorMessageID == 0 {
			session.AnchorFormat = ""
		} else if session.AnchorFormat != "text" && session.AnchorFormat != "snapshot" {
			session.AnchorFormat = "text"
		}
		if session.RetiringAnchorMessageID != 0 && session.RetiringAnchorFormat != "snapshot" {
			session.RetiringAnchorFormat = "text"
		}
		switch session.State {
		case TerminalRunning, TerminalLost, TerminalClosed:
		default:
			session.State = TerminalLost
			session.WatchEnabled = false
		}
		if len(session.StaleAlternateMessageIDs) > maxStaleAlternates {
			session.StaleAlternateMessageIDs = append([]int(nil), session.StaleAlternateMessageIDs[len(session.StaleAlternateMessageIDs)-maxStaleAlternates:]...)
		}
	}
}

func sessionRecency(session TerminalSession) time.Time {
	if !session.UpdatedAt.IsZero() {
		return session.UpdatedAt
	}
	if !session.LastActivityAt.IsZero() {
		return session.LastActivityAt
	}
	return session.CreatedAt
}

func maxAttachmentID(attachments []Attachment) int {
	max := 0
	for _, attachment := range attachments {
		if attachment.ID > max {
			max = attachment.ID
		}
	}
	return max
}

func cloneState(in State) State {
	out := in
	out.TerminalSessions = append([]TerminalSession(nil), in.TerminalSessions...)
	for i := range out.TerminalSessions {
		out.TerminalSessions[i] = cloneTerminalSession(out.TerminalSessions[i])
	}
	out.Attachments = append([]Attachment(nil), in.Attachments...)
	out.AttachmentBypasses = append([]AttachmentBypass(nil), in.AttachmentBypasses...)
	out.UpdateJournal = append([]UpdateEvent(nil), in.UpdateJournal...)
	out.ProcessedMessages = make(map[string]bool, len(in.ProcessedMessages))
	for k, v := range in.ProcessedMessages {
		out.ProcessedMessages[k] = v
	}
	return out
}

func cloneTerminalSession(in TerminalSession) TerminalSession {
	out := in
	out.StaleAlternateMessageIDs = append([]int(nil), in.StaleAlternateMessageIDs...)
	return out
}
