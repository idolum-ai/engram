package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/idolum-ai/engram/internal/atomicfile"
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
	Version                     int                `json:"version"`
	AnchorMode                  string             `json:"anchor_mode,omitempty"`
	CollapsedShelf              *CollapsedShelf    `json:"collapsed_shelf,omitempty"`
	PendingMessageCleanups      []MessageCleanup   `json:"pending_message_cleanups,omitempty"`
	NextSessionID               int                `json:"next_session_id"`
	LastUpdateID                int                `json:"last_update_id"`
	LastPollAt                  time.Time          `json:"last_poll_at,omitempty"`
	LastHaikuAt                 time.Time          `json:"last_haiku_at,omitempty"`
	LastHaikuError              string             `json:"last_haiku_error,omitempty"`
	TerminalSessions            []TerminalSession  `json:"terminal_sessions"`
	Attachments                 []Attachment       `json:"attachments"`
	AttachmentBypasses          []AttachmentBypass `json:"attachment_bypasses,omitempty"`
	UpdateJournal               []UpdateEvent      `json:"update_journal,omitempty"`
	ProcessedMessages           map[string]bool    `json:"processed_messages,omitempty"`
	HostBootID                  string             `json:"host_boot_id,omitempty"`
	PendingRecoveryBootID       string             `json:"pending_recovery_boot_id,omitempty"`
	LastRecoveryPlanHash        string             `json:"last_recovery_plan_hash,omitempty"`
	RecoveryPlanMessageIDs      []int              `json:"recovery_plan_message_ids,omitempty"`
	PendingRecoveryPlanHash     string             `json:"pending_recovery_plan_hash,omitempty"`
	PendingRecoveryPlanNextPage int                `json:"pending_recovery_plan_next_page,omitempty"`
}

type CollapsedShelf struct {
	ChatID            int64     `json:"chat_id"`
	MessageID         int       `json:"message_id"`
	LastRenderHash    string    `json:"last_render_hash,omitempty"`
	Pinned            bool      `json:"pinned,omitempty"`
	PinKnown          bool      `json:"pin_known,omitempty"`
	RetryAt           time.Time `json:"retry_at,omitempty"`
	RetiringChatID    int64     `json:"retiring_chat_id,omitempty"`
	RetiringMessageID int       `json:"retiring_message_id,omitempty"`
	RetiringRetryAt   time.Time `json:"retiring_retry_at,omitempty"`
}

type MessageCleanup struct {
	ChatID      int64     `json:"chat_id"`
	MessageID   int       `json:"message_id"`
	RetryAt     time.Time `json:"retry_at,omitempty"`
	RateLimited bool      `json:"rate_limited,omitempty"`
}

type RecoveryEvent struct {
	At                time.Time `json:"at"`
	Kind              string    `json:"kind"`
	Command           string    `json:"command,omitempty"`
	CommandHash       string    `json:"command_hash,omitempty"`
	CWD               string    `json:"cwd,omitempty"`
	ForegroundBefore  string    `json:"foreground_before,omitempty"`
	ForegroundAfter   string    `json:"foreground_after,omitempty"`
	ExpectedProcess   string    `json:"expected_process,omitempty"`
	Validation        string    `json:"validation"`
	Program           string    `json:"program,omitempty"`
	ProviderSessionID string    `json:"provider_session_id,omitempty"`
}

type PendingResume struct {
	StartedAt               time.Time      `json:"started_at"`
	PreviousTmuxSessionName string         `json:"previous_tmux_session_name"`
	PreviousTmuxWindowID    string         `json:"previous_tmux_window_id"`
	PreviousTmuxPaneID      string         `json:"previous_tmux_pane_id"`
	PreviousTmuxServerID    string         `json:"previous_tmux_server_id"`
	PreviousOrigin          TerminalOrigin `json:"previous_origin,omitempty"`
	PreviousCWD             string         `json:"previous_cwd,omitempty"`
	PreviousResumeProgram   string         `json:"previous_resume_program,omitempty"`
	PreviousResumeSessionID string         `json:"previous_resume_session_id,omitempty"`
}

type PendingRestore struct {
	ChatID    int64     `json:"chat_id"`
	MessageID int       `json:"message_id"`
	RetryAt   time.Time `json:"retry_at,omitempty"`
}

type TerminalSession struct {
	ID                       int             `json:"id"`
	TmuxSessionName          string          `json:"tmux_session_name"`
	TmuxWindowID             string          `json:"tmux_window_id"`
	TmuxPaneID               string          `json:"tmux_pane_id"`
	TmuxServerID             string          `json:"tmux_server_id,omitempty"`
	Origin                   TerminalOrigin  `json:"origin,omitempty"`
	Title                    string          `json:"title"`
	LastKnownCWD             string          `json:"last_known_cwd,omitempty"`
	State                    TerminalState   `json:"state"`
	CreatedAt                time.Time       `json:"created_at"`
	UpdatedAt                time.Time       `json:"updated_at"`
	LastActivityAt           time.Time       `json:"last_activity_at"`
	LastRawCaptureHash       string          `json:"last_raw_capture_hash,omitempty"`
	LastSnapshotCaptureHash  string          `json:"last_snapshot_capture_hash,omitempty"`
	LastSnapshotAttemptAt    time.Time       `json:"last_snapshot_attempt_at,omitempty"`
	LastRenderHash           string          `json:"last_render_hash,omitempty"`
	LastSummary              string          `json:"last_summary,omitempty"`
	PresentationProgram      string          `json:"presentation_program,omitempty"`
	PresentationVersion      string          `json:"presentation_version,omitempty"`
	PresentationModel        string          `json:"presentation_model,omitempty"`
	PresentationEffort       string          `json:"presentation_effort,omitempty"`
	PresentationMode         string          `json:"presentation_mode,omitempty"`
	PresentationActivity     string          `json:"presentation_activity,omitempty"`
	PresentationNotice       string          `json:"presentation_notice,omitempty"`
	SummaryMessageID         int             `json:"summary_message_id,omitempty"`
	SnapshotMessageID        int             `json:"snapshot_message_id,omitempty"`
	UpstreamMessageID        int             `json:"upstream_message_id,omitempty"`
	SeenUpstreamSignalIDs    []string        `json:"seen_upstream_signal_ids,omitempty"`
	LastUpstreamSignalAt     time.Time       `json:"last_upstream_signal_at,omitempty"`
	UpstreamRetryAt          time.Time       `json:"upstream_retry_at,omitempty"`
	StaleAlternateMessageIDs []int           `json:"stale_alternate_message_ids,omitempty"`
	AnchorChatID             int64           `json:"anchor_chat_id,omitempty"`
	AnchorMessageID          int             `json:"anchor_message_id,omitempty"`
	AnchorFormat             string          `json:"anchor_format,omitempty"`
	RetiringAnchorMessageID  int             `json:"retiring_anchor_message_id,omitempty"`
	RetiringAnchorFormat     string          `json:"retiring_anchor_format,omitempty"`
	RetiringAnchorRetryAt    time.Time       `json:"retiring_anchor_retry_at,omitempty"`
	AnchorPinned             bool            `json:"anchor_pinned,omitempty"`
	AnchorPinKnown           bool            `json:"anchor_pin_known,omitempty"`
	WatchEnabled             bool            `json:"watch_enabled"`
	Collapsed                bool            `json:"collapsed,omitempty"`
	PendingCollapse          bool            `json:"pending_collapse,omitempty"`
	PendingRestore           *PendingRestore `json:"pending_restore,omitempty"`
	ResumeProgram            string          `json:"resume_program,omitempty"`
	ResumeSessionID          string          `json:"resume_session_id,omitempty"`
	PendingResume            *PendingResume  `json:"pending_resume,omitempty"`
	RecoveryEvents           []RecoveryEvent `json:"recovery_events,omitempty"`
	LastAnchorEditAt         time.Time       `json:"last_anchor_edit_at,omitempty"`
	LastRawCapture           string          `json:"last_raw_capture,omitempty"`
	AnchorFiles              []string        `json:"-"`
	AnchorFileToken          string          `json:"-"`
}

func (s TerminalSession) HasSeenUpstreamSignal(recordID string) bool {
	for _, seen := range s.SeenUpstreamSignalIDs {
		if seen == recordID {
			return true
		}
	}
	return false
}

func (s *TerminalSession) RecordSeenUpstreamSignal(recordID string) {
	if s.HasSeenUpstreamSignal(recordID) {
		return
	}
	s.SeenUpstreamSignalIDs = append(s.SeenUpstreamSignalIDs, recordID)
	if len(s.SeenUpstreamSignalIDs) > maxSeenUpstreamSignals {
		s.SeenUpstreamSignalIDs = append([]string(nil), s.SeenUpstreamSignalIDs[len(s.SeenUpstreamSignalIDs)-maxSeenUpstreamSignals:]...)
	}
}

func (s *TerminalSession) RecordRecoveryEvent(event RecoveryEvent) {
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	normalizeRecoveryEvent(&event)
	s.RecoveryEvents = append(s.RecoveryEvents, event)
	if len(s.RecoveryEvents) > maxRecoveryEvents {
		s.RecoveryEvents = append([]RecoveryEvent(nil), s.RecoveryEvents[len(s.RecoveryEvents)-maxRecoveryEvents:]...)
	}
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
	currentStateVersion     = 17
	maxTerminalSessions     = 200
	maxAttachments          = 200
	maxAttachmentBypasses   = 100
	maxUpdateJournal        = 200
	maxStaleAlternates      = 16
	maxSeenUpstreamSignals  = 32
	maxRecoveryEvents       = 24
	maxRecoveryPlanMessages = 50
	maxMessageCleanups      = 64
	maxRecoveryCommandBytes = 512
	maxRecoveryFieldBytes   = 4096
	maxProcessedMessages    = 2_000
	maxAuditFileBytes       = int64(4 << 20)
	maxAuditRecordBytes     = 64 << 10
	maxReadOnlyStateBytes   = 16 << 20
)

// ReadSnapshot reads and normalizes persisted state without creating,
// replacing, pruning, chmodding, or otherwise modifying it.
func ReadSnapshot(path string) (State, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return State{}, fmt.Errorf("inspect state: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return State{}, fmt.Errorf("inspect state: stat opened file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return State{}, fmt.Errorf("inspect state: %s is not a regular file", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return State{}, fmt.Errorf("inspect state: %s must not be accessible by group or other users", path)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Geteuid() {
		return State{}, fmt.Errorf("inspect state: %s is not owned by the current user", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxReadOnlyStateBytes+1))
	if err != nil {
		return State{}, fmt.Errorf("inspect state: %w", err)
	}
	if len(b) > maxReadOnlyStateBytes {
		return State{}, fmt.Errorf("inspect state: file exceeds %d bytes", maxReadOnlyStateBytes)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return State{}, fmt.Errorf("inspect state: file is empty")
	}
	var snapshot State
	if err := json.Unmarshal(b, &snapshot); err != nil {
		return State{}, fmt.Errorf("inspect state: parse: %w", err)
	}
	if snapshot.Version > currentStateVersion {
		return State{}, fmt.Errorf("state schema version %d is newer than supported version %d", snapshot.Version, currentStateVersion)
	}
	normalizeCollapsedShelf(&snapshot)
	normalizeTerminalSessions(snapshot.TerminalSessions)
	snapshot.PendingMessageCleanups = validMessageCleanups(snapshot.PendingMessageCleanups)
	for index := range snapshot.TerminalSessions {
		if snapshot.TerminalSessions[index].Origin != TerminalOriginCreated {
			snapshot.TerminalSessions[index].Origin = TerminalOriginAttached
		}
	}
	snapshot.NextSessionID = nextSessionID(snapshot.TerminalSessions)
	if snapshot.ProcessedMessages == nil {
		snapshot.ProcessedMessages = map[string]bool{}
	}
	return cloneState(snapshot), nil
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
	normalizeCollapsedShelf(&s.state)
	normalizeTerminalSessions(s.state.TerminalSessions)
	s.state.NextSessionID = nextSessionID(s.state.TerminalSessions)
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
		if !PersistenceReachedReplacement(err) {
			s.state.AnchorMode = previous
		}
		return err
	}
	return nil
}

// ObserveHostBoot records the current host boot incarnation. A changed boot is
// retained as pending until Engram has successfully delivered its deterministic
// recovery plan, so an interrupted service start retries on the same boot.
func (s *Store) ObserveHostBoot(bootID string) (pending string, changed bool, err error) {
	bootID = strings.TrimSpace(bootID)
	if bootID == "" || len(bootID) > 256 || strings.ContainsRune(bootID, '\x00') {
		return "", false, fmt.Errorf("invalid host boot id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previousHostBootID := s.state.HostBootID
	previousPending := s.state.PendingRecoveryBootID
	if s.state.HostBootID == "" {
		s.state.HostBootID = bootID
	} else if s.state.HostBootID != bootID {
		s.state.HostBootID = bootID
		s.state.PendingRecoveryBootID = bootID
		changed = true
	}
	pending = s.state.PendingRecoveryBootID
	if s.state.HostBootID == previousHostBootID && s.state.PendingRecoveryBootID == previousPending {
		return pending, changed, nil
	}
	if saveErr := s.saveLocked(); saveErr != nil {
		if !PersistenceReachedReplacement(saveErr) {
			s.state.HostBootID = previousHostBootID
			s.state.PendingRecoveryBootID = previousPending
		}
		return pending, changed, saveErr
	}
	return pending, changed, nil
}

func (s *Store) AcknowledgeRecoveryBoot(bootID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.state.PendingRecoveryBootID
	if s.state.PendingRecoveryBootID == strings.TrimSpace(bootID) {
		s.state.PendingRecoveryBootID = ""
	} else {
		return nil
	}
	if err := s.saveLocked(); err != nil {
		if !PersistenceReachedReplacement(err) {
			s.state.PendingRecoveryBootID = previous
		}
		return err
	}
	return nil
}

func (s *Store) SetRecoveryPlanHash(hash string) error {
	hash = strings.TrimSpace(hash)
	if len(hash) > 128 || strings.ContainsRune(hash, '\x00') {
		return fmt.Errorf("invalid recovery plan hash")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.state.LastRecoveryPlanHash
	if previous == hash {
		return nil
	}
	s.state.LastRecoveryPlanHash = hash
	if err := s.saveLocked(); err != nil {
		if !PersistenceReachedReplacement(err) {
			s.state.LastRecoveryPlanHash = previous
		}
		return err
	}
	return nil
}

func (s *Store) SetRecoveryPlanProgress(hash string, nextPage int, messageIDs []int) error {
	hash, err := validateRecoveryPlanProgress(hash, nextPage, messageIDs)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := append([]int(nil), s.state.RecoveryPlanMessageIDs...)
	previousHash := s.state.PendingRecoveryPlanHash
	previousNextPage := s.state.PendingRecoveryPlanNextPage
	s.state.RecoveryPlanMessageIDs = append([]int(nil), messageIDs...)
	s.state.PendingRecoveryPlanHash = hash
	s.state.PendingRecoveryPlanNextPage = nextPage
	if err := s.saveLocked(); err != nil {
		if !PersistenceReachedReplacement(err) {
			s.state.RecoveryPlanMessageIDs = previous
			s.state.PendingRecoveryPlanHash = previousHash
			s.state.PendingRecoveryPlanNextPage = previousNextPage
		}
		return err
	}
	return nil
}

func (s *Store) CompleteRecoveryPlan(hash string, messageIDs []int) error {
	hash, err := validateRecoveryPlanProgress(hash, 0, messageIDs)
	if err != nil || hash == "" {
		if err != nil {
			return err
		}
		return fmt.Errorf("invalid recovery plan hash")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previousHash := s.state.LastRecoveryPlanHash
	previousMessageIDs := append([]int(nil), s.state.RecoveryPlanMessageIDs...)
	previousPendingHash := s.state.PendingRecoveryPlanHash
	previousNextPage := s.state.PendingRecoveryPlanNextPage
	s.state.LastRecoveryPlanHash = hash
	s.state.RecoveryPlanMessageIDs = append([]int(nil), messageIDs...)
	s.state.PendingRecoveryPlanHash = ""
	s.state.PendingRecoveryPlanNextPage = 0
	if err := s.saveLocked(); err != nil {
		if !PersistenceReachedReplacement(err) {
			s.state.LastRecoveryPlanHash = previousHash
			s.state.RecoveryPlanMessageIDs = previousMessageIDs
			s.state.PendingRecoveryPlanHash = previousPendingHash
			s.state.PendingRecoveryPlanNextPage = previousNextPage
		}
		return err
	}
	return nil
}

func validateRecoveryPlanProgress(hash string, nextPage int, messageIDs []int) (string, error) {
	hash = strings.TrimSpace(hash)
	if len(hash) > 128 || strings.ContainsRune(hash, '\x00') || nextPage < 0 || nextPage > maxRecoveryPlanMessages {
		return "", fmt.Errorf("invalid recovery plan progress")
	}
	if len(messageIDs) > maxRecoveryPlanMessages {
		return "", fmt.Errorf("too many recovery plan messages")
	}
	for _, messageID := range messageIDs {
		if messageID <= 0 {
			return "", fmt.Errorf("invalid recovery plan message id")
		}
	}
	return hash, nil
}

func (s *Store) saveLocked() error {
	s.pruneStateLocked(time.Now().UTC())
	s.state.NextSessionID = nextSessionID(s.state.TerminalSessions)
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
	return atomicfile.Write(path, data)
}

type atomicWriteError = atomicfile.WriteError

func PersistenceReachedReplacement(err error) bool {
	return atomicfile.ReachedReplacement(err)
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
	previous := cloneState(s.state)
	now := time.Now().UTC()
	id, replaceIndex, err := sessionAllocationSlot(s.state.TerminalSessions)
	if err != nil {
		return TerminalSession{}, err
	}
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
	if replaceIndex >= 0 {
		s.state.TerminalSessions[replaceIndex] = ts
	} else {
		s.state.TerminalSessions = append(s.state.TerminalSessions, ts)
	}
	s.state.NextSessionID = nextSessionID(s.state.TerminalSessions)
	if err := s.saveLocked(); err != nil {
		if !PersistenceReachedReplacement(err) {
			s.state = previous
		}
		return ts, err
	}
	return ts, nil
}

func (s *Store) UpdateSession(id int, fn func(*TerminalSession)) (TerminalSession, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.TerminalSessions {
		if s.state.TerminalSessions[i].ID == id {
			previous := cloneTerminalSession(s.state.TerminalSessions[i])
			fn(&s.state.TerminalSessions[i])
			s.state.TerminalSessions[i].UpdatedAt = time.Now().UTC()
			ts := cloneTerminalSession(s.state.TerminalSessions[i])
			if err := s.saveLocked(); err != nil {
				var writeErr *atomicWriteError
				if !errors.As(err, &writeErr) || !writeErr.Replaced {
					restored := false
					for j := range s.state.TerminalSessions {
						if s.state.TerminalSessions[j].ID == id {
							s.state.TerminalSessions[j] = previous
							restored = true
							break
						}
					}
					if !restored {
						s.state.TerminalSessions = append(s.state.TerminalSessions, previous)
					}
					return cloneTerminalSession(previous), true, err
				}
				return ts, true, err
			}
			return ts, true, nil
		}
	}
	return TerminalSession{}, false, nil
}

func (s *Store) BeginCollapseSessionIntoShelf(id int, expected TerminalSession, shelf CollapsedShelf) (TerminalSession, bool, error) {
	if shelf.ChatID == 0 || shelf.MessageID <= 0 {
		return TerminalSession{}, false, fmt.Errorf("invalid collapsed shelf")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := cloneState(s.state)
	for i := range s.state.TerminalSessions {
		session := &s.state.TerminalSessions[i]
		if session.ID != id {
			continue
		}
		if session.Collapsed || session.PendingCollapse || session.AnchorChatID != expected.AnchorChatID || session.AnchorMessageID != expected.AnchorMessageID ||
			session.TmuxServerID != expected.TmuxServerID || session.TmuxWindowID != expected.TmuxWindowID || session.TmuxPaneID != expected.TmuxPaneID {
			return cloneTerminalSession(*session), false, nil
		}
		if current := s.state.CollapsedShelf; current != nil && (current.ChatID != shelf.ChatID || current.MessageID != shelf.MessageID) {
			return cloneTerminalSession(*session), false, nil
		}
		if s.state.CollapsedShelf == nil {
			copy := shelf
			s.state.CollapsedShelf = &copy
		}
		// The state transition changes shelf membership. Reconciliation records a
		// render hash only after Telegram has confirmed the resulting text.
		s.state.CollapsedShelf.LastRenderHash = ""
		session.PendingCollapse = true
		session.UpdatedAt = time.Now().UTC()
		updated := cloneTerminalSession(*session)
		if err := s.saveLocked(); err != nil {
			if !PersistenceReachedReplacement(err) {
				s.state = previous
				return cloneTerminalSession(expected), false, err
			}
			return updated, true, err
		}
		return updated, true, nil
	}
	return TerminalSession{}, false, nil
}

func (s *Store) FinishCollapseSessionIntoShelf(id int, shelfMessageID int) (TerminalSession, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := cloneState(s.state)
	if s.state.CollapsedShelf == nil || s.state.CollapsedShelf.MessageID != shelfMessageID {
		return TerminalSession{}, false, nil
	}
	for i := range s.state.TerminalSessions {
		session := &s.state.TerminalSessions[i]
		if session.ID != id {
			continue
		}
		if session.Collapsed && !session.PendingCollapse {
			return cloneTerminalSession(*session), true, nil
		}
		if !session.PendingCollapse || session.Collapsed {
			return cloneTerminalSession(*session), false, nil
		}
		before := cloneTerminalSession(*session)
		recordStaleMessageID(session, session.SummaryMessageID)
		recordStaleMessageID(session, session.SnapshotMessageID)
		recordStaleMessageID(session, session.UpstreamMessageID)
		session.SummaryMessageID = 0
		session.SnapshotMessageID = 0
		session.UpstreamMessageID = 0
		session.Collapsed = true
		session.PendingCollapse = false
		session.PendingRestore = nil
		session.LastRawCaptureHash = ""
		session.LastSnapshotCaptureHash = ""
		session.LastSnapshotAttemptAt = time.Time{}
		session.LastRenderHash = ""
		session.LastAnchorEditAt = time.Time{}
		session.AnchorFiles = nil
		session.AnchorFileToken = ""
		session.UpdatedAt = time.Now().UTC()
		updated := cloneTerminalSession(*session)
		if err := s.saveLocked(); err != nil {
			if !PersistenceReachedReplacement(err) {
				s.state = previous
				return before, false, err
			}
			return updated, true, err
		}
		return updated, true, nil
	}
	return TerminalSession{}, false, nil
}

func (s *Store) CancelPendingCollapse(id int, shelfMessageID int) (TerminalSession, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := cloneState(s.state)
	if s.state.CollapsedShelf == nil || s.state.CollapsedShelf.MessageID != shelfMessageID {
		return TerminalSession{}, false, nil
	}
	for i := range s.state.TerminalSessions {
		session := &s.state.TerminalSessions[i]
		if session.ID != id {
			continue
		}
		if !session.PendingCollapse || session.Collapsed {
			return cloneTerminalSession(*session), false, nil
		}
		before := cloneTerminalSession(*session)
		session.PendingCollapse = false
		session.UpdatedAt = time.Now().UTC()
		s.state.CollapsedShelf.LastRenderHash = ""
		updated := cloneTerminalSession(*session)
		if err := s.saveLocked(); err != nil {
			if !PersistenceReachedReplacement(err) {
				s.state = previous
				return before, false, err
			}
			return updated, true, err
		}
		return updated, true, nil
	}
	return TerminalSession{}, false, nil
}

func (s *Store) CollapseSessionIntoShelf(id int, expected TerminalSession, shelf CollapsedShelf) (TerminalSession, bool, error) {
	pending, begun, beginErr := s.BeginCollapseSessionIntoShelf(id, expected, shelf)
	if !begun {
		return pending, false, beginErr
	}
	if beginErr != nil && !PersistenceReachedReplacement(beginErr) {
		return pending, false, beginErr
	}
	collapsed, finished, finishErr := s.FinishCollapseSessionIntoShelf(id, shelf.MessageID)
	if beginErr != nil && finishErr != nil {
		return collapsed, finished, errors.Join(beginErr, finishErr)
	}
	if finishErr != nil {
		return collapsed, finished, finishErr
	}
	return collapsed, finished, beginErr
}

func (s *Store) FinishCollapsedAnchorRetirement(id int, chatID int64, messageID int) (TerminalSession, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := cloneState(s.state)
	for i := range s.state.TerminalSessions {
		session := &s.state.TerminalSessions[i]
		if session.ID != id {
			continue
		}
		if !session.Collapsed || session.AnchorChatID != chatID || session.AnchorMessageID != messageID {
			return cloneTerminalSession(*session), false, nil
		}
		before := cloneTerminalSession(*session)
		recordStaleMessageID(session, messageID)
		session.AnchorMessageID = 0
		session.AnchorFormat = ""
		session.RetiringAnchorMessageID = 0
		session.RetiringAnchorFormat = ""
		session.RetiringAnchorRetryAt = time.Time{}
		session.AnchorPinned = false
		session.AnchorPinKnown = false
		session.UpdatedAt = time.Now().UTC()
		updated := cloneTerminalSession(*session)
		if err := s.saveLocked(); err != nil {
			if !PersistenceReachedReplacement(err) {
				s.state = previous
				return before, false, err
			}
			return updated, true, err
		}
		return updated, true, nil
	}
	return TerminalSession{}, false, nil
}

func (s *Store) BeginExpandSessionFromShelf(id, shelfMessageID int, chatID int64, anchorMessageID int) (TerminalSession, bool, error) {
	if anchorMessageID <= 0 {
		return TerminalSession{}, false, fmt.Errorf("invalid expanded anchor")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := cloneState(s.state)
	for i := range s.state.TerminalSessions {
		session := &s.state.TerminalSessions[i]
		if session.ID != id {
			continue
		}
		if !session.Collapsed && session.PendingRestore != nil &&
			session.PendingRestore.ChatID == chatID && session.PendingRestore.MessageID == anchorMessageID &&
			session.AnchorChatID == chatID && session.AnchorMessageID == anchorMessageID {
			return cloneTerminalSession(*session), true, nil
		}
		if !session.Collapsed && session.PendingRestore == nil &&
			session.AnchorChatID == chatID && session.AnchorMessageID == anchorMessageID {
			return cloneTerminalSession(*session), true, nil
		}
		if pending := session.PendingRestore; pending != nil {
			if session.Collapsed && pending.ChatID == chatID && pending.MessageID == anchorMessageID {
				return cloneTerminalSession(*session), true, nil
			}
			return cloneTerminalSession(*session), false, nil
		}
		if s.state.CollapsedShelf == nil || s.state.CollapsedShelf.MessageID != shelfMessageID || s.state.CollapsedShelf.ChatID != chatID {
			return cloneTerminalSession(*session), false, nil
		}
		if !session.Collapsed {
			return cloneTerminalSession(*session), false, nil
		}
		before := cloneTerminalSession(*session)
		session.PendingRestore = &PendingRestore{ChatID: chatID, MessageID: anchorMessageID}
		session.UpdatedAt = time.Now().UTC()
		updated := cloneTerminalSession(*session)
		if err := s.saveLocked(); err != nil {
			if !PersistenceReachedReplacement(err) {
				s.state = previous
				return before, false, err
			}
			return updated, true, err
		}
		return updated, true, nil
	}
	return TerminalSession{}, false, nil
}

func (s *Store) FinishExpandSessionFromShelf(id int, chatID int64, anchorMessageID int) (TerminalSession, bool, error) {
	if anchorMessageID <= 0 {
		return TerminalSession{}, false, fmt.Errorf("invalid expanded anchor")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := cloneState(s.state)
	for i := range s.state.TerminalSessions {
		session := &s.state.TerminalSessions[i]
		if session.ID != id {
			continue
		}
		if !session.Collapsed && session.PendingRestore != nil &&
			session.PendingRestore.ChatID == chatID && session.PendingRestore.MessageID == anchorMessageID &&
			session.AnchorChatID == chatID && session.AnchorMessageID == anchorMessageID {
			return cloneTerminalSession(*session), true, nil
		}
		if !session.Collapsed && session.PendingRestore == nil &&
			session.AnchorChatID == chatID && session.AnchorMessageID == anchorMessageID {
			return cloneTerminalSession(*session), true, nil
		}
		if !session.Collapsed || session.PendingRestore == nil ||
			session.PendingRestore.ChatID != chatID || session.PendingRestore.MessageID != anchorMessageID {
			return cloneTerminalSession(*session), false, nil
		}
		before := cloneTerminalSession(*session)
		if session.AnchorMessageID != 0 {
			recordStaleMessageID(session, session.AnchorMessageID)
		}
		session.Collapsed = false
		session.AnchorChatID = chatID
		session.AnchorMessageID = anchorMessageID
		session.AnchorFormat = "text"
		session.RetiringAnchorMessageID = 0
		session.RetiringAnchorFormat = ""
		session.RetiringAnchorRetryAt = time.Time{}
		session.AnchorPinned = true
		session.AnchorPinKnown = true
		session.LastRawCaptureHash = ""
		session.LastSnapshotCaptureHash = ""
		session.LastSnapshotAttemptAt = time.Time{}
		session.LastRenderHash = ""
		session.LastAnchorEditAt = time.Time{}
		session.AnchorFiles = nil
		session.AnchorFileToken = ""
		session.UpdatedAt = time.Now().UTC()
		if s.state.CollapsedShelf != nil {
			s.state.CollapsedShelf.LastRenderHash = ""
		}
		updated := cloneTerminalSession(*session)
		if err := s.saveLocked(); err != nil {
			if !PersistenceReachedReplacement(err) {
				s.state = previous
				return before, false, err
			}
			return updated, true, err
		}
		return updated, true, nil
	}
	return TerminalSession{}, false, nil
}

func (s *Store) FinishExpandedSessionControls(id int, chatID int64, anchorMessageID int) (TerminalSession, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := cloneState(s.state)
	for i := range s.state.TerminalSessions {
		session := &s.state.TerminalSessions[i]
		if session.ID != id {
			continue
		}
		if !session.Collapsed && session.PendingRestore == nil &&
			session.AnchorChatID == chatID && session.AnchorMessageID == anchorMessageID {
			return cloneTerminalSession(*session), true, nil
		}
		if session.Collapsed || session.PendingRestore == nil ||
			session.PendingRestore.ChatID != chatID || session.PendingRestore.MessageID != anchorMessageID ||
			session.AnchorChatID != chatID || session.AnchorMessageID != anchorMessageID {
			return cloneTerminalSession(*session), false, nil
		}
		before := cloneTerminalSession(*session)
		session.PendingRestore = nil
		session.UpdatedAt = time.Now().UTC()
		updated := cloneTerminalSession(*session)
		if err := s.saveLocked(); err != nil {
			if !PersistenceReachedReplacement(err) {
				s.state = previous
				return before, false, err
			}
			return updated, true, err
		}
		return updated, true, nil
	}
	return TerminalSession{}, false, nil
}

func (s *Store) ReturnExpandedSessionToShelf(id, shelfMessageID int, chatID int64, anchorMessageID int) (TerminalSession, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := cloneState(s.state)
	for i := range s.state.TerminalSessions {
		session := &s.state.TerminalSessions[i]
		if session.ID != id {
			continue
		}
		if s.state.CollapsedShelf == nil || s.state.CollapsedShelf.MessageID != shelfMessageID ||
			s.state.CollapsedShelf.ChatID != chatID || session.Collapsed || session.PendingRestore == nil ||
			session.PendingRestore.ChatID != chatID || session.PendingRestore.MessageID != anchorMessageID ||
			session.AnchorChatID != chatID || session.AnchorMessageID != anchorMessageID {
			return cloneTerminalSession(*session), false, nil
		}
		before := cloneTerminalSession(*session)
		session.Collapsed = true
		session.PendingRestore = nil
		session.UpdatedAt = time.Now().UTC()
		s.state.CollapsedShelf.LastRenderHash = ""
		updated := cloneTerminalSession(*session)
		if err := s.saveLocked(); err != nil {
			if !PersistenceReachedReplacement(err) {
				s.state = previous
				return before, false, err
			}
			return updated, true, err
		}
		return updated, true, nil
	}
	return TerminalSession{}, false, nil
}

func (s *Store) AbandonPendingRestore(id int, chatID int64, messageID int) (TerminalSession, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := cloneState(s.state)
	for i := range s.state.TerminalSessions {
		session := &s.state.TerminalSessions[i]
		if session.ID != id {
			continue
		}
		if !session.Collapsed || session.PendingRestore == nil ||
			session.PendingRestore.ChatID != chatID || session.PendingRestore.MessageID != messageID {
			return cloneTerminalSession(*session), false, nil
		}
		before := cloneTerminalSession(*session)
		recordStaleMessageID(session, messageID)
		session.PendingRestore = nil
		session.UpdatedAt = time.Now().UTC()
		updated := cloneTerminalSession(*session)
		if err := s.saveLocked(); err != nil {
			if !PersistenceReachedReplacement(err) {
				s.state = previous
				return before, false, err
			}
			return updated, true, err
		}
		return updated, true, nil
	}
	return TerminalSession{}, false, nil
}

func (s *Store) FinishPendingRestoreRetirement(id int, chatID int64, messageID int) (TerminalSession, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := cloneState(s.state)
	for i := range s.state.TerminalSessions {
		session := &s.state.TerminalSessions[i]
		if session.ID != id {
			continue
		}
		if session.PendingRestore == nil {
			return cloneTerminalSession(*session), true, nil
		}
		if session.State != TerminalClosed || session.PendingRestore.ChatID != chatID || session.PendingRestore.MessageID != messageID {
			return cloneTerminalSession(*session), false, nil
		}
		before := cloneTerminalSession(*session)
		session.PendingRestore = nil
		session.UpdatedAt = time.Now().UTC()
		updated := cloneTerminalSession(*session)
		if err := s.saveLocked(); err != nil {
			if !PersistenceReachedReplacement(err) {
				s.state = previous
				return before, false, err
			}
			return updated, true, err
		}
		return updated, true, nil
	}
	return TerminalSession{}, false, nil
}

func (s *Store) SetCollapsedShelfIfEmpty(shelf CollapsedShelf) (CollapsedShelf, bool, error) {
	if shelf.ChatID == 0 || shelf.MessageID <= 0 {
		return CollapsedShelf{}, false, fmt.Errorf("invalid collapsed shelf")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.CollapsedShelf != nil {
		return *s.state.CollapsedShelf, false, nil
	}
	copy := shelf
	s.state.CollapsedShelf = &copy
	if err := s.saveLocked(); err != nil {
		if !PersistenceReachedReplacement(err) {
			s.state.CollapsedShelf = nil
			return CollapsedShelf{}, false, err
		}
		return copy, true, err
	}
	return copy, true, nil
}

func (s *Store) ReplaceCollapsedShelf(messageID int, replacement CollapsedShelf) (CollapsedShelf, bool, error) {
	if replacement.ChatID == 0 || replacement.MessageID <= 0 ||
		replacement.RetiringChatID != 0 || replacement.RetiringMessageID != 0 {
		return CollapsedShelf{}, false, fmt.Errorf("invalid collapsed shelf replacement")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.CollapsedShelf == nil {
		return CollapsedShelf{}, false, nil
	}
	current := s.state.CollapsedShelf
	if current.ChatID == replacement.ChatID && current.MessageID == replacement.MessageID {
		if current.RetiringMessageID == messageID {
			return *current, true, nil
		}
		return *current, false, nil
	}
	if current.MessageID != messageID || current.RetiringMessageID != 0 {
		return *current, false, nil
	}
	previous := cloneState(s.state)
	oldChatID, oldMessageID := current.ChatID, current.MessageID
	copy := replacement
	copy.RetiringChatID = oldChatID
	copy.RetiringMessageID = oldMessageID
	copy.RetiringRetryAt = time.Time{}
	s.state.CollapsedShelf = &copy
	if err := s.saveLocked(); err != nil {
		if !PersistenceReachedReplacement(err) {
			s.state = previous
			return *previous.CollapsedShelf, false, err
		}
		return copy, true, err
	}
	return copy, true, nil
}

func (s *Store) FinishCollapsedShelfRetirement(messageID int, chatID int64, retiringMessageID int) (CollapsedShelf, bool, error) {
	if messageID <= 0 || chatID == 0 || retiringMessageID <= 0 {
		return CollapsedShelf{}, false, fmt.Errorf("invalid collapsed shelf retirement")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.CollapsedShelf == nil || s.state.CollapsedShelf.MessageID != messageID {
		return CollapsedShelf{}, false, nil
	}
	current := s.state.CollapsedShelf
	if current.RetiringChatID != chatID || current.RetiringMessageID != retiringMessageID {
		return *current, false, nil
	}
	previous := cloneState(s.state)
	current.RetiringChatID = 0
	current.RetiringMessageID = 0
	current.RetiringRetryAt = time.Time{}
	updated := *current
	if err := s.saveLocked(); err != nil {
		if !PersistenceReachedReplacement(err) {
			s.state = previous
			return *previous.CollapsedShelf, false, err
		}
		return updated, true, err
	}
	return updated, true, nil
}

func (s *Store) RecoverCollapsedShelfPredecessor(messageID int) (CollapsedShelf, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.CollapsedShelf == nil || s.state.CollapsedShelf.MessageID != messageID ||
		s.state.CollapsedShelf.RetiringChatID == 0 || s.state.CollapsedShelf.RetiringMessageID <= 0 {
		return CollapsedShelf{}, false, nil
	}
	previous := cloneState(s.state)
	shelf := s.state.CollapsedShelf
	replacementChatID, replacementMessageID := shelf.ChatID, shelf.MessageID
	shelf.ChatID = shelf.RetiringChatID
	shelf.MessageID = shelf.RetiringMessageID
	shelf.LastRenderHash = ""
	shelf.Pinned = false
	shelf.PinKnown = false
	shelf.RetryAt = time.Time{}
	shelf.RetiringChatID = replacementChatID
	shelf.RetiringMessageID = replacementMessageID
	shelf.RetiringRetryAt = time.Time{}
	updated := *shelf
	if err := s.saveLocked(); err != nil {
		if !PersistenceReachedReplacement(err) {
			s.state = previous
			return *previous.CollapsedShelf, false, err
		}
		return updated, true, err
	}
	return updated, true, nil
}

func (s *Store) UpdateCollapsedShelf(messageID int, fn func(*CollapsedShelf)) (CollapsedShelf, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.CollapsedShelf == nil || s.state.CollapsedShelf.MessageID != messageID {
		return CollapsedShelf{}, false, nil
	}
	previous := *s.state.CollapsedShelf
	fn(s.state.CollapsedShelf)
	updated := *s.state.CollapsedShelf
	if err := s.saveLocked(); err != nil {
		if !PersistenceReachedReplacement(err) {
			copy := previous
			s.state.CollapsedShelf = &copy
			return previous, true, err
		}
		return updated, true, err
	}
	return updated, true, nil
}

func (s *Store) ClearCollapsedShelf(messageID int) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.CollapsedShelf == nil || s.state.CollapsedShelf.MessageID != messageID {
		return false, nil
	}
	if s.state.CollapsedShelf.RetiringMessageID != 0 {
		return false, nil
	}
	previous := *s.state.CollapsedShelf
	s.state.CollapsedShelf = nil
	if err := s.saveLocked(); err != nil {
		if !PersistenceReachedReplacement(err) {
			s.state.CollapsedShelf = &previous
		}
		return true, err
	}
	return true, nil
}

func (s *Store) RememberMessageCleanup(cleanup MessageCleanup) error {
	if cleanup.ChatID == 0 || cleanup.MessageID <= 0 {
		return fmt.Errorf("invalid message cleanup")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := append([]MessageCleanup(nil), s.state.PendingMessageCleanups...)
	for i := range s.state.PendingMessageCleanups {
		current := &s.state.PendingMessageCleanups[i]
		if current.ChatID != cleanup.ChatID || current.MessageID != cleanup.MessageID {
			continue
		}
		current.RetryAt = cleanup.RetryAt
		current.RateLimited = cleanup.RateLimited
		if err := s.saveLocked(); err != nil {
			if !PersistenceReachedReplacement(err) {
				s.state.PendingMessageCleanups = previous
			}
			return err
		}
		return nil
	}
	if len(s.state.PendingMessageCleanups) >= maxMessageCleanups {
		return fmt.Errorf("message cleanup capacity of %d reached", maxMessageCleanups)
	}
	s.state.PendingMessageCleanups = append(s.state.PendingMessageCleanups, cleanup)
	if err := s.saveLocked(); err != nil {
		if !PersistenceReachedReplacement(err) {
			s.state.PendingMessageCleanups = previous
		}
		return err
	}
	return nil
}

func (s *Store) FinishMessageCleanup(chatID int64, messageID int) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := append([]MessageCleanup(nil), s.state.PendingMessageCleanups...)
	for i, cleanup := range s.state.PendingMessageCleanups {
		if cleanup.ChatID != chatID || cleanup.MessageID != messageID {
			continue
		}
		s.state.PendingMessageCleanups = append(s.state.PendingMessageCleanups[:i], s.state.PendingMessageCleanups[i+1:]...)
		if err := s.saveLocked(); err != nil {
			if !PersistenceReachedReplacement(err) {
				s.state.PendingMessageCleanups = previous
			}
			return true, err
		}
		return true, nil
	}
	return false, nil
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
		if ts.AnchorMessageID == messageID || ts.SummaryMessageID == messageID || ts.SnapshotMessageID == messageID || ts.UpstreamMessageID == messageID {
			if ts.Collapsed {
				return cloneTerminalSession(ts), ReplyTargetStale, true
			}
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

func (s *Store) FindByBinding(paneID, windowID, serverID string) (TerminalSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ts := range s.state.TerminalSessions {
		if ts.TmuxPaneID == paneID && ts.TmuxWindowID == windowID && ts.TmuxServerID == serverID && ts.State != TerminalClosed {
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
	return s.addAttachmentLocked(a, s.saveLocked)
}

func (s *Store) addAttachmentLocked(a Attachment, save func() error) error {
	previous := append([]Attachment(nil), s.state.Attachments...)
	a.ID = maxAttachmentID(s.state.Attachments) + 1
	s.state.Attachments = append(s.state.Attachments, a)
	if err := save(); err != nil {
		if !PersistenceReachedReplacement(err) {
			s.state.Attachments = previous
		}
		return err
	}
	return nil
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

// NoteGuide retains the legacy JSON field names so existing state files remain
// readable while recording the selected conversational provider's health.
func (s *Store) NoteGuide(errText string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.LastHaikuAt = time.Now().UTC()
	s.state.LastHaikuError = errText
	return s.saveLocked()
}

func sessionAllocationSlot(sessions []TerminalSession) (int, int, error) {
	closedByID := make(map[int]int)
	used := make(map[int]int)
	reusableIndex := -1
	for index, session := range sessions {
		reusable := session.State == TerminalClosed && session.ResumeProgram == "" && session.ResumeSessionID == "" && session.PendingRestore == nil
		if reusable && reusableIndex < 0 {
			reusableIndex = index
		}
		if session.ID > 0 && session.ID <= maxTerminalSessions {
			used[session.ID]++
		}
		if reusable && session.ID > 0 && session.ID <= maxTerminalSessions {
			closedByID[session.ID] = index
		}
	}
	for id := 1; id <= maxTerminalSessions; id++ {
		if index, ok := closedByID[id]; ok && used[id] == 1 {
			return id, index, nil
		}
	}
	// A legacy allocator could fill the table with IDs outside the current
	// 1..maxTerminalSessions namespace. Recycle a closed entry into a free ID
	// without growing the table, but never mistake an ID gap for spare capacity.
	if reusableIndex >= 0 {
		for id := 1; id <= maxTerminalSessions; id++ {
			if used[id] == 0 {
				return id, reusableIndex, nil
			}
		}
	}
	if len(sessions) >= maxTerminalSessions {
		return 0, -1, fmt.Errorf("terminal session capacity of %d reached; close a watch before creating another", maxTerminalSessions)
	}
	for id := 1; id <= maxTerminalSessions; id++ {
		if used[id] == 0 {
			return id, -1, nil
		}
	}
	return 0, -1, fmt.Errorf("terminal session capacity of %d reached; close a watch before creating another", maxTerminalSessions)
}

func nextSessionID(sessions []TerminalSession) int {
	id, _, err := sessionAllocationSlot(sessions)
	if err != nil {
		return maxTerminalSessions + 1
	}
	return id
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
	s.state.PendingMessageCleanups = validMessageCleanups(s.state.PendingMessageCleanups)

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

func validMessageCleanups(cleanups []MessageCleanup) []MessageCleanup {
	out := make([]MessageCleanup, 0, min(len(cleanups), maxMessageCleanups))
	seen := make(map[string]bool, len(cleanups))
	for _, cleanup := range cleanups {
		if cleanup.ChatID == 0 || cleanup.MessageID <= 0 {
			continue
		}
		key := strconv.FormatInt(cleanup.ChatID, 10) + ":" + strconv.Itoa(cleanup.MessageID)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, cleanup)
		if len(out) == maxMessageCleanups {
			break
		}
	}
	return out
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
		aProtected := a.State == TerminalRunning || a.State == TerminalLost || a.PendingCollapse || a.PendingRestore != nil
		bProtected := b.State == TerminalRunning || b.State == TerminalLost || b.PendingCollapse || b.PendingRestore != nil
		if aProtected != bProtected {
			return aProtected
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
		} else if session.AnchorFormat != "text" && session.AnchorFormat != "snapshot" && session.AnchorFormat != "guide-evidence" {
			session.AnchorFormat = "text"
		}
		if session.RetiringAnchorMessageID != 0 && session.RetiringAnchorFormat != "snapshot" && session.RetiringAnchorFormat != "guide-evidence" {
			session.RetiringAnchorFormat = "text"
		}
		switch session.State {
		case TerminalRunning, TerminalLost, TerminalClosed:
		default:
			session.State = TerminalLost
			session.WatchEnabled = false
		}
		if session.State == TerminalClosed {
			session.ResumeProgram = ""
			session.ResumeSessionID = ""
			session.PendingResume = nil
			session.Collapsed = false
			session.PendingCollapse = false
			session.RecoveryEvents = nil
		}
		if session.Collapsed {
			session.PendingCollapse = false
		}
		if pending := session.PendingRestore; pending != nil {
			sameAsAnchor := pending.ChatID == session.AnchorChatID && pending.MessageID == session.AnchorMessageID
			if pending.ChatID == 0 || pending.MessageID <= 0 ||
				((session.State == TerminalClosed || session.Collapsed) && sameAsAnchor) ||
				(session.State != TerminalClosed && !session.Collapsed && !sameAsAnchor) {
				session.PendingRestore = nil
			}
		}
		if session.PresentationProgram != "codex" && session.PresentationProgram != "agent" {
			session.PresentationProgram = ""
			session.PresentationVersion = ""
			session.PresentationModel = ""
			session.PresentationEffort = ""
			session.PresentationMode = ""
			session.PresentationActivity = ""
			session.PresentationNotice = ""
		} else {
			session.PresentationVersion = truncateUTF8(session.PresentationVersion, 32)
			session.PresentationModel = truncateUTF8(session.PresentationModel, 64)
			session.PresentationEffort = truncateUTF8(session.PresentationEffort, 16)
			session.PresentationMode = truncateUTF8(session.PresentationMode, 16)
			session.PresentationActivity = truncateUTF8(session.PresentationActivity, 32)
			session.PresentationNotice = truncateUTF8(session.PresentationNotice, 256)
		}
		if len(session.StaleAlternateMessageIDs) > maxStaleAlternates {
			session.StaleAlternateMessageIDs = append([]int(nil), session.StaleAlternateMessageIDs[len(session.StaleAlternateMessageIDs)-maxStaleAlternates:]...)
		}
		if len(session.SeenUpstreamSignalIDs) > maxSeenUpstreamSignals {
			session.SeenUpstreamSignalIDs = append([]string(nil), session.SeenUpstreamSignalIDs[len(session.SeenUpstreamSignalIDs)-maxSeenUpstreamSignals:]...)
		}
		if len(session.RecoveryEvents) > maxRecoveryEvents {
			session.RecoveryEvents = append([]RecoveryEvent(nil), session.RecoveryEvents[len(session.RecoveryEvents)-maxRecoveryEvents:]...)
		}
		for eventIndex := range session.RecoveryEvents {
			normalizeRecoveryEvent(&session.RecoveryEvents[eventIndex])
		}
		if pending := session.PendingResume; pending != nil {
			pending.PreviousTmuxSessionName = truncateUTF8(pending.PreviousTmuxSessionName, 256)
			pending.PreviousTmuxWindowID = truncateUTF8(pending.PreviousTmuxWindowID, 64)
			pending.PreviousTmuxPaneID = truncateUTF8(pending.PreviousTmuxPaneID, 64)
			pending.PreviousTmuxServerID = truncateUTF8(pending.PreviousTmuxServerID, 128)
			pending.PreviousCWD = truncateUTF8(pending.PreviousCWD, maxRecoveryFieldBytes)
			pending.PreviousResumeProgram = truncateUTF8(pending.PreviousResumeProgram, 32)
			pending.PreviousResumeSessionID = truncateUTF8(pending.PreviousResumeSessionID, 128)
		}
	}
}

func normalizeCollapsedShelf(state *State) {
	if state.CollapsedShelf == nil {
		return
	}
	if state.CollapsedShelf.ChatID == 0 || state.CollapsedShelf.MessageID <= 0 {
		state.CollapsedShelf = nil
		return
	}
	state.CollapsedShelf.PinKnown = false
	state.CollapsedShelf.LastRenderHash = truncateUTF8(state.CollapsedShelf.LastRenderHash, 128)
	if state.CollapsedShelf.RetiringChatID == 0 || state.CollapsedShelf.RetiringMessageID <= 0 ||
		(state.CollapsedShelf.RetiringChatID == state.CollapsedShelf.ChatID &&
			state.CollapsedShelf.RetiringMessageID == state.CollapsedShelf.MessageID) {
		state.CollapsedShelf.RetiringChatID = 0
		state.CollapsedShelf.RetiringMessageID = 0
		state.CollapsedShelf.RetiringRetryAt = time.Time{}
	}
}

func recordStaleMessageID(session *TerminalSession, messageID int) {
	if messageID <= 0 {
		return
	}
	for _, staleID := range session.StaleAlternateMessageIDs {
		if staleID == messageID {
			return
		}
	}
	session.StaleAlternateMessageIDs = append(session.StaleAlternateMessageIDs, messageID)
	if len(session.StaleAlternateMessageIDs) > maxStaleAlternates {
		session.StaleAlternateMessageIDs = append([]int(nil), session.StaleAlternateMessageIDs[len(session.StaleAlternateMessageIDs)-maxStaleAlternates:]...)
	}
}

func normalizeRecoveryEvent(event *RecoveryEvent) {
	event.Kind = truncateUTF8(event.Kind, 64)
	event.Command = truncateUTF8(event.Command, maxRecoveryCommandBytes)
	event.CommandHash = truncateUTF8(event.CommandHash, 128)
	event.CWD = truncateUTF8(event.CWD, maxRecoveryFieldBytes)
	event.ForegroundBefore = truncateUTF8(event.ForegroundBefore, 256)
	event.ForegroundAfter = truncateUTF8(event.ForegroundAfter, 256)
	event.ExpectedProcess = truncateUTF8(event.ExpectedProcess, 256)
	event.Validation = truncateUTF8(event.Validation, 64)
	event.Program = truncateUTF8(event.Program, 32)
	event.ProviderSessionID = truncateUTF8(event.ProviderSessionID, 128)
}

func truncateUTF8(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	cut := maxBytes
	for cut > 0 && !utf8.ValidString(value[:cut]) {
		cut--
	}
	return value[:cut]
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
	if in.CollapsedShelf != nil {
		shelf := *in.CollapsedShelf
		out.CollapsedShelf = &shelf
	}
	out.PendingMessageCleanups = append([]MessageCleanup(nil), in.PendingMessageCleanups...)
	out.RecoveryPlanMessageIDs = append([]int(nil), in.RecoveryPlanMessageIDs...)
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
	if in.PendingResume != nil {
		pending := *in.PendingResume
		out.PendingResume = &pending
	}
	if in.PendingRestore != nil {
		pending := *in.PendingRestore
		out.PendingRestore = &pending
	}
	out.AnchorFiles = append([]string(nil), in.AnchorFiles...)
	out.StaleAlternateMessageIDs = append([]int(nil), in.StaleAlternateMessageIDs...)
	out.SeenUpstreamSignalIDs = append([]string(nil), in.SeenUpstreamSignalIDs...)
	out.RecoveryEvents = append([]RecoveryEvent(nil), in.RecoveryEvents...)
	return out
}
