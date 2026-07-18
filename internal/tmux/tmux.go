package tmux

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/idolum-ai/engram/internal/recovery"
)

type Runner interface {
	Run(ctx context.Context, args ...string) (string, error)
}

// StreamRunner writes tmux stdout directly to a destination. ExecRunner uses
// the bounded copy buffers in os/exec, although tmux may still construct the
// requested capture internally before writing it to stdout.
type StreamRunner interface {
	RunToWriter(ctx context.Context, dst io.Writer, args ...string) error
}

type ExecRunner struct{}

type commandError struct {
	args   []string
	err    error
	stderr string
}

func (e *commandError) Error() string {
	return fmt.Sprintf("tmux %s: %v: %s", strings.Join(e.args, " "), e.err, strings.TrimSpace(e.stderr))
}

func (e *commandError) Unwrap() error { return e.err }

func (ExecRunner) Run(ctx context.Context, args ...string) (string, error) {
	var out bytes.Buffer
	err := (ExecRunner{}).RunToWriter(ctx, &out, args...)
	return out.String(), err
}

func (ExecRunner) RunToWriter(ctx context.Context, dst io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "tmux", tmuxCommandArguments(args)...)
	var errOut bytes.Buffer
	cmd.Stdout = dst
	cmd.Stderr = &errOut
	err := cmd.Run()
	if err != nil {
		return &commandError{args: append([]string(nil), args...), err: err, stderr: errOut.String()}
	}
	return nil
}

func tmuxCommandArguments(args []string) []string {
	return append([]string{"-u"}, args...)
}

type Manager struct {
	Runner Runner
}

var commandSubmitDelay = 150 * time.Millisecond

type Session struct {
	Name     string
	ID       string
	Windows  string
	Attached string
}

type Window struct {
	SessionID   string
	SessionName string
	Index       string
	ID          string
	Name        string
	Active      string
	PaneID      string
	CurrentPath string
	CurrentCmd  string
}

// Pane identifies a live pane by tmux's immutable server-lifetime IDs.
type Pane struct {
	SessionID   string
	WindowID    string
	ID          string
	SessionName string
	WindowIndex string
	Index       string
	Active      bool
	CurrentPath string
	CurrentCmd  string
}

type StyledCapture struct {
	ANSI        string
	Text        string
	JoinedText  string
	Hyperlinks  []string
	ServerID    string
	WindowID    string
	PaneID      string
	CurrentCmd  string
	AlternateOn string
	PaneInMode  string
	Columns     int
	VisibleRows int
	BufferRows  int
	Title       string
	CurrentPath string
}

const paneRecordFormat = "#{n:session_id}:#{session_id}#{n:window_id}:#{window_id}#{n:pane_id}:#{pane_id}#{n:session_name}:#{session_name}#{n:window_index}:#{window_index}#{n:pane_index}:#{pane_index}#{n:pane_active}:#{pane_active}#{n:pane_current_path}:#{pane_current_path}#{n:pane_current_command}:#{pane_current_command}"

const serverIDOption = "@engram_server_id"
const identityMismatchMarker = "ENGRAM_IDENTITY_MISMATCH"

const (
	EngramPaneOption     = "@engram"
	EngramWatchIDOption  = "@engram_watch_id"
	EngramNotifyOption   = "@engram_notify"
	EngramArtifactOption = "@engram_artifact"
	EngramRecoveryOption = "@engram_recovery"
)

type IdentityError struct {
	Reason string
	Err    error
}

func (e *IdentityError) Error() string {
	if e.Err != nil {
		return e.Reason + ": " + e.Err.Error()
	}
	return e.Reason
}

func (e *IdentityError) Unwrap() error { return e.Err }

func New(r Runner) Manager {
	return Manager{Runner: r}
}

func (m Manager) EnsureSession(ctx context.Context, name, workdir string) (string, error) {
	id, found, lookupErr := m.findSessionID(ctx, name)
	if lookupErr == nil && found {
		return id, nil
	}
	if lookupErr != nil && !missingTmuxServer(lookupErr) {
		return "", lookupErr
	}
	format := recordFormat("session_id", "session_name")
	out, createErr := m.Runner.Run(ctx, "new-session", "-d", "-P", "-F", format, "-s", name, "-c", workdir)
	if createErr != nil {
		id, found, reconcileErr := m.findSessionID(ctx, name)
		if reconcileErr == nil && found {
			return id, nil
		}
		if reconcileErr != nil {
			return "", errors.Join(createErr, fmt.Errorf("reconcile tmux session: %w", reconcileErr))
		}
		return "", createErr
	}
	records, parseErr := parseRecords(out, 2)
	if parseErr != nil || len(records) != 1 || !validSessionID(records[0][0]) {
		return "", fmt.Errorf("unexpected tmux new-session output %q", out)
	}
	parts := records[0]
	if parts[1] != name {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		_, _ = m.Runner.Run(cleanupCtx, "kill-session", "-t", parts[0])
		cancel()
		return "", fmt.Errorf("tmux canonicalized session name %q as %q", name, parts[1])
	}
	return parts[0], nil
}

func (m Manager) findSessionID(ctx context.Context, name string) (string, bool, error) {
	sessions, err := m.ListSessions(ctx)
	if err != nil {
		return "", false, err
	}
	for _, session := range sessions {
		if session.Name == name && validSessionID(session.ID) {
			return session.ID, true, nil
		}
	}
	return "", false, nil
}

func (m Manager) NewWindow(ctx context.Context, sessionID, workdir, title string) (windowID, paneID string, err error) {
	if !validSessionID(sessionID) {
		return "", "", fmt.Errorf("invalid tmux session ID %q", sessionID)
	}
	columns, rows, err := m.defaultWindowSize(ctx)
	if err != nil {
		return "", "", err
	}
	format := recordFormat("window_id", "pane_id")
	out, err := m.Runner.Run(ctx, "new-window", "-P", "-F", format, "-n", title, "-c", workdir, "-t", sessionID+":")
	if err != nil {
		return "", "", err
	}
	records, parseErr := parseRecords(out, 2)
	if parseErr != nil || len(records) != 1 {
		return "", "", fmt.Errorf("unexpected tmux new-window output %q", out)
	}
	parts := records[0]
	if !validImmutableID(parts[0], '@') || !validImmutableID(parts[1], '%') {
		return "", "", fmt.Errorf("unexpected tmux new-window identity %q", out)
	}
	windowID, paneID = parts[0], parts[1]
	if _, err := m.Runner.Run(ctx, "resize-window", "-x", strconv.Itoa(columns), "-y", strconv.Itoa(rows), "-t", windowID); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		_, _ = m.Runner.Run(cleanupCtx, "kill-window", "-t", windowID)
		cancel()
		return "", "", fmt.Errorf("size new tmux window to %dx%d: %w", columns, rows, err)
	}
	return windowID, paneID, nil
}

func (m Manager) defaultWindowSize(ctx context.Context) (columns, rows int, err error) {
	out, err := m.Runner.Run(ctx, "show-options", "-gv", "default-size")
	if err != nil {
		return 0, 0, fmt.Errorf("read tmux default-size: %w", err)
	}
	width, height, found := strings.Cut(strings.TrimSpace(out), "x")
	if !found || strings.Contains(height, "x") {
		return 0, 0, fmt.Errorf("invalid tmux default-size %q", strings.TrimSpace(out))
	}
	columns, widthErr := strconv.Atoi(width)
	rows, heightErr := strconv.Atoi(height)
	if widthErr != nil || heightErr != nil || columns <= 0 || columns > 400 || rows <= 0 || rows > 400 {
		return 0, 0, fmt.Errorf("invalid tmux default-size %q", strings.TrimSpace(out))
	}
	return columns, rows, nil
}

func (m Manager) ListSessions(ctx context.Context) ([]Session, error) {
	out, err := m.Runner.Run(ctx, "list-sessions", "-F", recordFormat("session_name", "session_id", "session_windows", "session_attached"))
	if err != nil {
		return nil, err
	}
	records, err := parseRecords(out, 4)
	if err != nil {
		return nil, fmt.Errorf("parse tmux sessions: %w", err)
	}
	sessions := make([]Session, 0, len(records))
	for _, parts := range records {
		if parts[0] == "" || !validSessionID(parts[1]) || !validNonnegative(parts[2]) || !validNonnegative(parts[3]) {
			return nil, fmt.Errorf("unexpected tmux session record %q", parts)
		}
		sessions = append(sessions, Session{Name: parts[0], ID: parts[1], Windows: parts[2], Attached: parts[3]})
	}
	return sessions, nil
}

func (m Manager) ListWindows(ctx context.Context) ([]Window, error) {
	format := recordFormat("session_id", "session_name", "window_index", "window_id", "window_name", "window_active", "pane_id", "pane_current_path", "pane_current_command")
	out, err := m.Runner.Run(ctx, "list-windows", "-a", "-F", format)
	if err != nil {
		return nil, err
	}
	return parseWindows(out)
}

func (m Manager) ResolveTarget(ctx context.Context, target string) (Window, error) {
	format := recordFormat("session_id", "session_name", "window_index", "window_id", "window_name", "window_active", "pane_id", "pane_current_path", "pane_current_command")
	out, err := m.Runner.Run(ctx, "display-message", "-p", "-t", target, format)
	if err != nil {
		return Window{}, err
	}
	windows, err := parseWindows(out)
	if err != nil {
		return Window{}, err
	}
	if len(windows) != 1 {
		return Window{}, fmt.Errorf("unexpected tmux target output %q", out)
	}
	return windows[0], nil
}

func (m Manager) ListPanes(ctx context.Context) ([]Pane, error) {
	out, err := m.Runner.Run(ctx, "list-panes", "-a", "-F", paneRecordFormat)
	if err != nil {
		return nil, err
	}
	return parsePanes(out)
}

func (m Manager) InspectPane(ctx context.Context, target string) (Pane, error) {
	out, err := m.Runner.Run(ctx, "display-message", "-p", "-t", target, paneRecordFormat)
	if err != nil {
		return Pane{}, err
	}
	panes, err := parsePanes(out)
	if err != nil {
		return Pane{}, err
	}
	if len(panes) != 1 {
		return Pane{}, fmt.Errorf("unexpected tmux pane output %q", out)
	}
	return panes[0], nil
}

func (m Manager) CurrentServerID(ctx context.Context) (string, error) {
	out, err := m.Runner.Run(ctx, "show-options", "-gqv", serverIDOption)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(out)
	if id == "" {
		return "", &IdentityError{Reason: "tmux server has no Engram incarnation"}
	}
	if !validServerID(id) {
		return "", &IdentityError{Reason: fmt.Sprintf("invalid tmux server incarnation %q", id)}
	}
	return id, nil
}

func (m Manager) EnsureServerID(ctx context.Context) (string, error) {
	if id, err := m.CurrentServerID(ctx); err == nil {
		return id, nil
	} else if !IsIdentityLoss(err) {
		return "", err
	}
	id, err := captureNonce()
	if err != nil {
		return "", err
	}
	if _, err := m.Runner.Run(ctx, "set-option", "-go", serverIDOption, id); err != nil {
		if current, currentErr := m.CurrentServerID(ctx); currentErr == nil {
			return current, nil
		}
		return "", err
	}
	return m.CurrentServerID(ctx)
}

// PublishRecoveryMetadata is the narrow terminal-hook ingress. The hook may
// identify only its inherited immutable pane ID; the service later validates
// the full pane/window/server binding before trusting this pane-local option.
func (m Manager) PublishRecoveryMetadata(ctx context.Context, paneID string, metadata recovery.Metadata) error {
	if !validImmutableID(strings.TrimSpace(paneID), '%') {
		return fmt.Errorf("invalid tmux pane id")
	}
	encoded, err := recovery.Encode(metadata)
	if err != nil {
		return err
	}
	_, err = m.Runner.Run(ctx, "set-option", "-p", "-q", "-t", paneID, EngramRecoveryOption, encoded)
	return err
}

// RecoveryMetadata reads hook-published metadata only while the stored
// immutable binding remains valid on both sides of the option read.
func (m Manager) RecoveryMetadata(ctx context.Context, paneID, windowID, serverID string) (recovery.Metadata, error) {
	if _, err := m.ValidateBinding(ctx, paneID, windowID, serverID); err != nil {
		return recovery.Metadata{}, err
	}
	value, err := m.Runner.Run(ctx, "show-options", "-p", "-q", "-v", "-t", paneID, EngramRecoveryOption)
	if err != nil {
		if missingTmuxTarget(err) {
			return recovery.Metadata{}, &IdentityError{Reason: "tmux pane identity is gone", Err: err}
		}
		return recovery.Metadata{}, err
	}
	if _, err := m.ValidateBinding(ctx, paneID, windowID, serverID); err != nil {
		return recovery.Metadata{}, err
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return recovery.Metadata{}, nil
	}
	return recovery.Decode(value)
}

func validServerID(id string) bool {
	if len(id) != 32 || id != strings.ToLower(id) {
		return false
	}
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == 16
}

// ValidatePane resolves paneID and verifies that it still belongs to windowID.
// Both arguments must be tmux immutable IDs, not names or numeric indexes.
func (m Manager) ValidatePane(ctx context.Context, paneID, windowID string) (Pane, error) {
	if !validImmutableID(paneID, '%') {
		return Pane{}, &IdentityError{Reason: fmt.Sprintf("invalid tmux pane ID %q", paneID)}
	}
	if !validImmutableID(windowID, '@') {
		return Pane{}, &IdentityError{Reason: fmt.Sprintf("invalid tmux window ID %q", windowID)}
	}
	pane, err := m.InspectPane(ctx, paneID)
	if err != nil {
		if missingTmuxTarget(err) {
			return Pane{}, &IdentityError{Reason: "tmux pane identity is gone", Err: err}
		}
		return Pane{}, err
	}
	if pane.ID != paneID || pane.WindowID != windowID {
		return Pane{}, &IdentityError{Reason: fmt.Sprintf("tmux pane identity mismatch: got pane %q window %q, want pane %q window %q", pane.ID, pane.WindowID, paneID, windowID)}
	}
	return pane, nil
}

func (m Manager) ValidateBinding(ctx context.Context, paneID, windowID, serverID string) (Pane, error) {
	if !validServerID(serverID) {
		return Pane{}, &IdentityError{Reason: "missing or invalid persisted tmux server incarnation"}
	}
	if !validImmutableID(paneID, '%') {
		return Pane{}, &IdentityError{Reason: fmt.Sprintf("invalid tmux pane ID %q", paneID)}
	}
	if !validImmutableID(windowID, '@') {
		return Pane{}, &IdentityError{Reason: fmt.Sprintf("invalid tmux window ID %q", windowID)}
	}
	format := recordFormat(serverIDOption, "session_id", "window_id", "pane_id", "session_name", "window_index", "pane_index", "pane_active", "pane_current_path", "pane_current_command")
	out, err := m.Runner.Run(ctx, "display-message", "-p", "-t", paneID, format)
	if err != nil {
		if missingTmuxTarget(err) {
			return Pane{}, &IdentityError{Reason: "tmux pane identity is gone", Err: err}
		}
		return Pane{}, err
	}
	records, err := parseRecords(out, 10)
	if err != nil || len(records) != 1 {
		return Pane{}, fmt.Errorf("unexpected tmux binding output %q", out)
	}
	parts := records[0]
	if parts[0] != serverID {
		return Pane{}, &IdentityError{Reason: "tmux server incarnation mismatch"}
	}
	panes, err := panesFromRecords([][]string{parts[1:]})
	if err != nil {
		return Pane{}, err
	}
	pane := panes[0]
	if pane.ID != paneID || pane.WindowID != windowID {
		return Pane{}, &IdentityError{Reason: fmt.Sprintf("tmux pane identity mismatch: got pane %q window %q, want pane %q window %q", pane.ID, pane.WindowID, paneID, windowID)}
	}
	return pane, nil
}

// IsIdentityLoss reports errors that prove a persisted immutable pane/window
// identity is no longer usable. Cancellation and generic execution failures do
// not prove that the pane disappeared.
func IsIdentityLoss(err error) bool {
	var identity *IdentityError
	return errors.As(err, &identity)
}

func missingTmuxTarget(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "can't find pane:") || strings.Contains(message, "can't find window:")
}

func missingTmuxServer(err error) bool {
	var commandErr *commandError
	if !errors.As(err, &commandErr) {
		return false
	}
	stderr := strings.ToLower(commandErr.stderr)
	return strings.Contains(stderr, "no server running") ||
		(strings.Contains(stderr, "error connecting to") && strings.Contains(stderr, "no such file or directory"))
}

func (m Manager) SendCommand(ctx context.Context, paneID, text string) error {
	if _, err := m.Runner.Run(ctx, "send-keys", "-t", paneID, "-l", "--", text); err != nil {
		return err
	}
	if commandSubmitDelay > 0 {
		timer := time.NewTimer(commandSubmitDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	_, err := m.Runner.Run(ctx, "send-keys", "-t", paneID, "Enter")
	return err
}

func (m Manager) SendText(ctx context.Context, paneID, text string) error {
	_, err := m.Runner.Run(ctx, "send-keys", "-t", paneID, "-l", "--", text)
	return err
}

func (m Manager) SendKeys(ctx context.Context, paneID string, keys []string) error {
	args := append([]string{"send-keys", "-t", paneID}, keys...)
	_, err := m.Runner.Run(ctx, args...)
	return err
}

// SendCommandIfBindingMatches keeps each input effect behind a tmux-side
// identity condition. A restart between the literal text and Enter can leave
// text unsubmitted, but it cannot redirect either effect into a new server.
func (m Manager) SendCommandIfBindingMatches(ctx context.Context, paneID, windowID, serverID, text string) error {
	if err := m.SendTextIfBindingMatches(ctx, paneID, windowID, serverID, text); err != nil {
		return err
	}
	if commandSubmitDelay > 0 {
		timer := time.NewTimer(commandSubmitDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return m.SendKeysIfBindingMatches(ctx, paneID, windowID, serverID, []string{"Enter"})
}

func (m Manager) SendTextIfBindingMatches(ctx context.Context, paneID, windowID, serverID, text string) error {
	if err := validateBindingIDs(paneID, windowID, serverID); err != nil {
		return err
	}
	nonce, err := captureNonce()
	if err != nil {
		return err
	}
	buffer := "engram-input-" + nonce
	if _, err := m.Runner.Run(ctx, "set-buffer", "-b", buffer, "--", text); err != nil {
		return err
	}
	command := "paste-buffer -p -r -d -b " + buffer + " -t " + paneID
	if err := m.runIfBindingMatches(ctx, paneID, windowID, serverID, command); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		_, _ = m.Runner.Run(cleanupCtx, "delete-buffer", "-b", buffer)
		cancel()
		return err
	}
	return nil
}

func (m Manager) SendKeysIfBindingMatches(ctx context.Context, paneID, windowID, serverID string, keys []string) error {
	if err := validateBindingIDs(paneID, windowID, serverID); err != nil {
		return err
	}
	if err := ValidKeys(keys); err != nil {
		return err
	}
	parts := []string{"send-keys", "-t", paneID}
	for _, key := range keys {
		parts = append(parts, ShellQuote(key))
	}
	return m.runIfBindingMatches(ctx, paneID, windowID, serverID, strings.Join(parts, " "))
}

// AdvertiseEngramIfBindingMatches publishes a small, terminal-native capability
// description on the watched pane. tmux user options are deliberately used as
// durable, inspectable environment metadata rather than injecting text or shell
// configuration into the pane.
func (m Manager) AdvertiseEngramIfBindingMatches(ctx context.Context, paneID, windowID, serverID string, watchID int) error {
	if watchID <= 0 {
		return fmt.Errorf("invalid Engram watch ID %d", watchID)
	}
	marker := fmt.Sprintf("v1 watch=%d remote=telegram", watchID)
	commands := []string{"set-option -p -q -u -t " + paneID + " " + EngramPaneOption}
	for _, option := range []struct {
		name  string
		value string
	}{
		{EngramWatchIDOption, strconv.Itoa(watchID)},
		{EngramNotifyOption, "run: engram signal --stdout MESSAGE (tool output) or engram signal MESSAGE (interactive TTY)"},
		{EngramArtifactOption, "print a visible file:// URI (OSC 8 optional), then run @engram_notify"},
	} {
		commands = append(commands, "set-option -p -q -t "+paneID+" "+option.name+" "+ShellQuote(option.value))
	}
	// @engram is the commit marker: consumers ignore auxiliary fields while it
	// is absent. Publish it only after every auxiliary value is in place.
	commands = append(commands, "set-option -p -q -t "+paneID+" "+EngramPaneOption+" "+ShellQuote(marker))
	return m.runIfBindingMatches(ctx, paneID, windowID, serverID, strings.Join(commands, " ; "))
}

// ClearEngramAdvertisementIfBindingMatches removes capability metadata without
// affecting the pane's program, environment, title, or other user options.
func (m Manager) ClearEngramAdvertisementIfBindingMatches(ctx context.Context, paneID, windowID, serverID string) error {
	// Clear the commit marker first so stale auxiliary values are never treated
	// as a live capability advertisement if a later clear is interrupted.
	commands := make([]string, 0, 4)
	for _, option := range []string{EngramPaneOption, EngramWatchIDOption, EngramNotifyOption, EngramArtifactOption} {
		commands = append(commands, "set-option -p -q -u -t "+paneID+" "+option)
	}
	return m.runIfBindingMatches(ctx, paneID, windowID, serverID, strings.Join(commands, " ; "))
}

func (m Manager) runIfBindingMatches(ctx context.Context, paneID, windowID, serverID, command string) error {
	condition := bindingCondition(windowID, serverID)
	falseCommand := "display-message -p " + identityMismatchMarker
	out, err := m.Runner.Run(ctx, "if-shell", "-F", "-t", paneID, condition, command, falseCommand)
	if err != nil {
		if missingTmuxTarget(err) {
			return &IdentityError{Reason: "tmux pane identity is gone", Err: err}
		}
		return err
	}
	if strings.TrimSpace(out) == identityMismatchMarker {
		return &IdentityError{Reason: "tmux binding changed before input"}
	}
	return nil
}

func validateBindingIDs(paneID, windowID, serverID string) error {
	if !validImmutableID(paneID, '%') || !validImmutableID(windowID, '@') || !validServerID(serverID) {
		return &IdentityError{Reason: "invalid tmux binding for input"}
	}
	return nil
}

// CaptureVisibleRaw preserves physical wrapped lines, attributes, trailing
// spaces, and tmux's final newline while conditioning the capture on the full
// immutable binding in the same tmux command queue.
func (m Manager) CaptureVisibleRaw(ctx context.Context, paneID, windowID, serverID string) (string, error) {
	command := "capture-pane -p -e -N -t " + paneID
	return m.captureIfBindingMatches(ctx, paneID, windowID, serverID, command)
}

// CaptureLiteral returns a bounded plain-text frame without creating tmux
// paste buffers or preserving terminal control sequences.
func (m Manager) CaptureLiteral(ctx context.Context, paneID, windowID, serverID string, targetRows int) (string, error) {
	if err := validateBindingIDs(paneID, windowID, serverID); err != nil {
		return "", err
	}
	if targetRows <= 0 || targetRows > 400 {
		return "", fmt.Errorf("target rows must be between 1 and 400")
	}
	meta, err := m.Runner.Run(ctx, "display-message", "-p", "-t", paneID, recordFormat("pane_width", "pane_height"))
	if err != nil {
		return "", err
	}
	records, parseErr := parseRecords(meta, 2)
	if parseErr != nil || len(records) != 1 {
		return "", fmt.Errorf("unexpected tmux literal metadata")
	}
	parts := records[0]
	columns, err := strconv.Atoi(parts[0])
	if err != nil || columns <= 0 || columns > 400 {
		return "", fmt.Errorf("invalid tmux pane width %q", parts[0])
	}
	visibleRows, err := strconv.Atoi(parts[1])
	if err != nil || visibleRows <= 0 || visibleRows > 400 {
		return "", fmt.Errorf("invalid tmux pane height %q", parts[1])
	}
	start, end := captureBounds(visibleRows, targetRows)
	command := strings.Join([]string{"capture-pane", "-p", "-N", "-S", strconv.Itoa(start), "-E", strconv.Itoa(end), "-t", paneID}, " ")
	out, err := m.captureIfBindingMatches(ctx, paneID, windowID, serverID, command)
	if err != nil {
		return "", err
	}
	return semanticCapture(out), nil
}

func (m Manager) captureIfBindingMatches(ctx context.Context, paneID, windowID, serverID, command string) (string, error) {
	if err := validateBindingIDs(paneID, windowID, serverID); err != nil {
		return "", err
	}
	nonce, err := captureNonce()
	if err != nil {
		return "", err
	}
	marker := identityMismatchMarker + "-" + nonce
	out, err := m.Runner.Run(ctx, "if-shell", "-F", "-t", paneID, bindingCondition(windowID, serverID), command, "display-message -p "+marker)
	if err != nil {
		if missingTmuxTarget(err) {
			return "", &IdentityError{Reason: "tmux pane identity is gone", Err: err}
		}
		return "", err
	}
	if strings.TrimSpace(out) == marker {
		return "", &IdentityError{Reason: "tmux binding changed while capturing"}
	}
	return out, nil
}

func (m Manager) CaptureStyled(ctx context.Context, paneID string, targetRows int) (StyledCapture, error) {
	if targetRows <= 0 || targetRows > 400 {
		return StyledCapture{}, fmt.Errorf("target rows must be between 1 and 400")
	}
	metaFormat := recordFormat(serverIDOption, "window_id", "pane_id", "pane_width", "pane_height", "pane_title", "pane_current_path", "pane_current_command", "alternate_on", "pane_in_mode")
	meta, err := m.Runner.Run(ctx, "display-message", "-p", "-t", paneID, metaFormat)
	if err != nil {
		return StyledCapture{}, err
	}
	before, err := parseCaptureMetadata(meta)
	if err != nil {
		return StyledCapture{}, err
	}
	columns, visibleRows := before.Columns, before.VisibleRows
	start, end := captureBounds(visibleRows, targetRows)
	nonce, err := captureNonce()
	if err != nil {
		return StyledCapture{}, err
	}
	physicalBuffer := "engram-physical-" + nonce
	joinedBuffer := "engram-joined-" + nonce
	afterText, err := m.Runner.Run(ctx,
		"capture-pane", "-e", "-N", "-S", strconv.Itoa(start), "-E", strconv.Itoa(end), "-t", paneID, "-b", physicalBuffer,
		";", "capture-pane", "-J", "-S", strconv.Itoa(start), "-E", strconv.Itoa(end), "-t", paneID, "-b", joinedBuffer,
		";", "display-message", "-p", "-t", paneID, metaFormat,
	)
	if err != nil {
		m.cleanupCaptureBuffers(ctx, physicalBuffer, joinedBuffer)
		return StyledCapture{}, err
	}
	defer m.cleanupCaptureBuffers(ctx, physicalBuffer, joinedBuffer)
	after, err := parseCaptureMetadata(afterText)
	if err != nil {
		return StyledCapture{}, err
	}
	if !sameCaptureIdentity(before, after) {
		return StyledCapture{}, &IdentityError{Reason: "tmux pane identity changed while capturing"}
	}
	if !sameCaptureBoundary(before, after) {
		return StyledCapture{}, fmt.Errorf("tmux pane changed while capturing")
	}
	ansi, err := m.Runner.Run(ctx, "show-buffer", "-b", physicalBuffer)
	if err != nil {
		return StyledCapture{}, err
	}
	joined, err := m.Runner.Run(ctx, "show-buffer", "-b", joinedBuffer)
	if err != nil {
		return StyledCapture{}, err
	}
	bufferRows := strings.Count(ansi, "\n")
	if ansi != "" && !strings.HasSuffix(ansi, "\n") {
		bufferRows++
	}
	if bufferRows == 0 {
		bufferRows = 1
	}
	return StyledCapture{
		ANSI:        ansi,
		Text:        physicalSemanticCapture(ansi),
		JoinedText:  semanticCapture(joined),
		Hyperlinks:  extractOSC8Hyperlinks(ansi, 16),
		ServerID:    after.ServerID,
		WindowID:    after.WindowID,
		PaneID:      after.PaneID,
		CurrentCmd:  after.CurrentCmd,
		AlternateOn: after.AlternateOn,
		PaneInMode:  after.PaneInMode,
		Columns:     columns,
		VisibleRows: visibleRows,
		BufferRows:  bufferRows,
		Title:       after.Title,
		CurrentPath: after.CurrentPath,
	}, nil
}

func extractOSC8Hyperlinks(input string, limit int) []string {
	if limit <= 0 {
		return nil
	}
	const prefix = "\x1b]8;"
	const maxURIBytes = 2048
	seen := make(map[string]bool)
	links := make([]string, 0, min(limit, 4))
	for offset := 0; offset < len(input) && len(links) < limit; {
		start := strings.Index(input[offset:], prefix)
		if start < 0 {
			break
		}
		start += offset + len(prefix)
		parameterEnd := strings.IndexByte(input[start:], ';')
		if parameterEnd < 0 {
			break
		}
		uriStart := start + parameterEnd + 1
		uriEnd, next, ok := terminalStringEnd(input, uriStart)
		if !ok {
			offset = uriStart
			continue
		}
		uri := input[uriStart:uriEnd]
		if uri != "" && len(uri) <= maxURIBytes && utf8.ValidString(uri) && !strings.ContainsAny(uri, "\x00\r\n") && !seen[uri] {
			seen[uri] = true
			links = append(links, uri)
		}
		offset = next
	}
	return links
}

func terminalStringEnd(input string, start int) (end, next int, ok bool) {
	for i := start; i < len(input); {
		switch {
		case input[i] == '\a':
			return i, i + 1, true
		case input[i] == 0x1b && i+1 < len(input) && input[i+1] == '\\':
			return i, i + 2, true
		}
		r, size := utf8.DecodeRuneInString(input[i:])
		if r == '\u009c' {
			return i, i + size, true
		}
		if r == utf8.RuneError && size == 1 {
			if input[i] == 0x9c {
				return i, i + 1, true
			}
			i++
			continue
		}
		i += size
	}
	return 0, start, false
}

type captureMetadata struct {
	ServerID    string
	WindowID    string
	PaneID      string
	Columns     int
	VisibleRows int
	Title       string
	CurrentPath string
	CurrentCmd  string
	AlternateOn string
	PaneInMode  string
}

func parseCaptureMetadata(out string) (captureMetadata, error) {
	records, err := parseRecords(out, 10)
	if err != nil || len(records) != 1 {
		return captureMetadata{}, fmt.Errorf("unexpected tmux snapshot metadata")
	}
	parts := records[0]
	if !validServerID(parts[0]) || !validImmutableID(parts[1], '@') || !validImmutableID(parts[2], '%') {
		return captureMetadata{}, fmt.Errorf("invalid tmux snapshot identity")
	}
	columns, err := strconv.Atoi(parts[3])
	if err != nil || columns <= 0 || columns > 400 {
		return captureMetadata{}, fmt.Errorf("invalid tmux pane width %q", parts[3])
	}
	visibleRows, err := strconv.Atoi(parts[4])
	if err != nil || visibleRows <= 0 || visibleRows > 400 {
		return captureMetadata{}, fmt.Errorf("invalid tmux pane height %q", parts[4])
	}
	if !validTmuxFlag(parts[8]) || !validTmuxFlag(parts[9]) {
		return captureMetadata{}, fmt.Errorf("invalid tmux pane mode metadata")
	}
	return captureMetadata{
		ServerID: parts[0], WindowID: parts[1], PaneID: parts[2], Columns: columns, VisibleRows: visibleRows,
		Title: parts[5], CurrentPath: parts[6], CurrentCmd: parts[7], AlternateOn: parts[8], PaneInMode: parts[9],
	}, nil
}

func sameCaptureBoundary(before, after captureMetadata) bool {
	return sameCaptureIdentity(before, after) && before.Columns == after.Columns && before.VisibleRows == after.VisibleRows && before.CurrentCmd == after.CurrentCmd &&
		before.AlternateOn == after.AlternateOn && before.PaneInMode == after.PaneInMode
}

func sameCaptureIdentity(before, after captureMetadata) bool {
	return before.ServerID == after.ServerID && before.WindowID == after.WindowID && before.PaneID == after.PaneID
}

func validTmuxFlag(value string) bool { return value == "0" || value == "1" }

func captureBounds(visibleRows, targetRows int) (start, end int) {
	return visibleRows - targetRows, visibleRows - 1
}

func captureNonce() (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("generate tmux capture nonce: %w", err)
	}
	return hex.EncodeToString(nonce[:]), nil
}

func (m Manager) cleanupCaptureBuffers(ctx context.Context, names ...string) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	args := make([]string, 0, len(names)*4)
	for _, name := range names {
		if len(args) != 0 {
			args = append(args, ";")
		}
		args = append(args, "delete-buffer", "-b", name)
	}
	_, _ = m.Runner.Run(cleanupCtx, args...)
}

// DumpScrollback streams the complete retained pane history as readable plain
// text. Soft-wrapped terminal rows are joined back into logical lines, and tmux
// stdout is never buffered in Engram. The runner must implement StreamRunner so
// this memory behavior is explicit.
func (m Manager) DumpScrollback(ctx context.Context, paneID, windowID, serverID string, dst io.Writer) error {
	if dst == nil {
		return fmt.Errorf("missing scrollback destination")
	}
	if err := validateBindingIDs(paneID, windowID, serverID); err != nil {
		return err
	}
	runner, ok := m.Runner.(StreamRunner)
	if !ok {
		return fmt.Errorf("tmux runner does not support streaming")
	}
	nonce, err := captureNonce()
	if err != nil {
		return err
	}
	marker := identityMismatchMarker + "-" + nonce
	guard := &identityGuardWriter{dst: dst, marker: marker}
	command := "capture-pane -p -J -N -S - -E - -t " + paneID
	err = runner.RunToWriter(ctx, guard, "if-shell", "-F", "-t", paneID, bindingCondition(windowID, serverID), command, "display-message -p "+marker)
	if err != nil {
		if missingTmuxTarget(err) {
			return &IdentityError{Reason: "tmux pane identity is gone", Err: err}
		}
		return err
	}
	return guard.finish()
}

type identityGuardWriter struct {
	dst     io.Writer
	marker  string
	prefix  []byte
	decided bool
}

func (w *identityGuardWriter) Write(data []byte) (int, error) {
	if w.decided {
		return w.dst.Write(data)
	}
	w.prefix = append(w.prefix, data...)
	if len(w.prefix) <= len(w.marker)+1 {
		return len(data), nil
	}
	w.decided = true
	if err := writeAll(w.dst, w.prefix); err != nil {
		return 0, err
	}
	w.prefix = nil
	return len(data), nil
}

func (w *identityGuardWriter) finish() error {
	if w.decided {
		return nil
	}
	if strings.TrimSpace(string(w.prefix)) == w.marker {
		return &IdentityError{Reason: "tmux binding changed while capturing"}
	}
	return writeAll(w.dst, w.prefix)
}

func writeAll(dst io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := dst.Write(data)
		if err != nil {
			return err
		}
		if written <= 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

func (m Manager) KillWindow(ctx context.Context, windowID string) error {
	_, err := m.Runner.Run(ctx, "kill-window", "-t", windowID)
	return err
}

// KillWindowIfBindingMatches evaluates identity and closes the window in one
// tmux command queue, so another client cannot move the pane between them.
func (m Manager) KillWindowIfBindingMatches(ctx context.Context, paneID, windowID, serverID string) error {
	if !validImmutableID(paneID, '%') || !validImmutableID(windowID, '@') || !validServerID(serverID) {
		return &IdentityError{Reason: "invalid tmux binding for close"}
	}
	condition := bindingCondition(windowID, serverID)
	trueCommand := "kill-window -t " + windowID
	falseCommand := "display-message -p " + identityMismatchMarker
	out, err := m.Runner.Run(ctx, "if-shell", "-F", "-t", paneID, condition, trueCommand, falseCommand)
	if err != nil {
		if missingTmuxTarget(err) {
			return &IdentityError{Reason: "tmux pane identity is gone", Err: err}
		}
		return err
	}
	if strings.TrimSpace(out) == identityMismatchMarker {
		return &IdentityError{Reason: "tmux binding changed before close"}
	}
	return nil
}

func bindingCondition(windowID, serverID string) string {
	return fmt.Sprintf("#{&&:#{==:#{%s},%s},#{==:#{window_id},%s}}", serverIDOption, serverID, windowID)
}

func SessionName(chatID int64) string {
	return "engram-" + strconv.FormatInt(chatID, 10)
}

func WindowTitle(id int, input string) string {
	title := strings.TrimSpace(input)
	if title == "" {
		title = fmt.Sprintf("engram-%d", id)
	}
	title = strings.ReplaceAll(title, "\n", " ")
	if len(title) > 32 {
		title = title[:32]
	}
	return title
}

func AttachedTitle(w Window) string {
	title := strings.TrimSpace(w.SessionName + ":" + w.Index + " " + w.Name)
	title = strings.ReplaceAll(title, "\n", " ")
	if title == ":" {
		title = w.ID
	}
	if len(title) > 40 {
		title = title[:40]
	}
	return title
}

func parseWindows(out string) ([]Window, error) {
	records, err := parseRecords(out, 9)
	if err != nil {
		return nil, fmt.Errorf("parse tmux windows: %w", err)
	}
	windows := make([]Window, 0, len(records))
	for _, parts := range records {
		if !validSessionID(parts[0]) || parts[1] == "" || !validNonnegative(parts[2]) || !validImmutableID(parts[3], '@') || (parts[5] != "0" && parts[5] != "1") || !validImmutableID(parts[6], '%') {
			return nil, fmt.Errorf("unexpected tmux window record %q", parts)
		}
		windows = append(windows, Window{
			SessionID:   parts[0],
			SessionName: parts[1],
			Index:       parts[2],
			ID:          parts[3],
			Name:        parts[4],
			Active:      parts[5],
			PaneID:      parts[6],
			CurrentPath: parts[7],
			CurrentCmd:  parts[8],
		})
	}
	return windows, nil
}

func parsePanes(out string) ([]Pane, error) {
	records, err := parseRecords(out, 9)
	if err != nil {
		return nil, fmt.Errorf("parse tmux panes: %w", err)
	}
	return panesFromRecords(records)
}

func panesFromRecords(records [][]string) ([]Pane, error) {
	panes := make([]Pane, 0, len(records))
	for _, parts := range records {
		if len(parts) != 9 {
			return nil, fmt.Errorf("unexpected tmux pane field count %d", len(parts))
		}
		if !validImmutableID(parts[0], '$') || !validImmutableID(parts[1], '@') || !validImmutableID(parts[2], '%') {
			return nil, fmt.Errorf("unexpected tmux pane identity in record %q", parts)
		}
		if parts[3] == "" || !validNonnegative(parts[4]) || !validNonnegative(parts[5]) || (parts[6] != "0" && parts[6] != "1") {
			return nil, fmt.Errorf("unexpected tmux pane active value %q", parts[6])
		}
		panes = append(panes, Pane{
			SessionID:   parts[0],
			WindowID:    parts[1],
			ID:          parts[2],
			SessionName: parts[3],
			WindowIndex: parts[4],
			Index:       parts[5],
			Active:      parts[6] == "1",
			CurrentPath: parts[7],
			CurrentCmd:  parts[8],
		})
	}
	return panes, nil
}

// recordFormat emits self-delimiting tmux metadata. Tmux's n: modifier counts
// the rendered UTF-8 bytes, so field values cannot collide with separators.
func recordFormat(fields ...string) string {
	var format strings.Builder
	for _, field := range fields {
		format.WriteString("#{n:")
		format.WriteString(field)
		format.WriteString("}:#{")
		format.WriteString(field)
		format.WriteByte('}')
	}
	return format.String()
}

func parseRecords(out string, fieldCount int) ([][]string, error) {
	if out == "" {
		return nil, nil
	}
	remaining := out
	var records [][]string
	for remaining != "" {
		fields, consumed, err := parseRecord(remaining, fieldCount)
		if err != nil {
			return nil, err
		}
		records = append(records, fields)
		remaining = remaining[consumed:]
		if remaining == "" || remaining[0] != '\n' {
			return nil, fmt.Errorf("record is not newline terminated")
		}
		remaining = remaining[1:]
	}
	return records, nil
}

func parseRecord(input string, fieldCount int) ([]string, int, error) {
	remaining := input
	fields := make([]string, 0, fieldCount)
	for i := 0; i < fieldCount; i++ {
		separator := strings.IndexByte(remaining, ':')
		if separator <= 0 {
			return nil, 0, fmt.Errorf("field %d has no byte length", i)
		}
		length, err := strconv.ParseUint(remaining[:separator], 10, 64)
		if err != nil || length > uint64(len(remaining)-separator-1) {
			return nil, 0, fmt.Errorf("field %d has invalid byte length", i)
		}
		valueStart := separator + 1
		valueEnd := valueStart + int(length)
		value := remaining[valueStart:valueEnd]
		if !utf8.ValidString(value) {
			return nil, 0, fmt.Errorf("field %d is not UTF-8", i)
		}
		fields = append(fields, value)
		remaining = remaining[valueEnd:]
	}
	return fields, len(input) - len(remaining), nil
}

func validNonnegative(value string) bool {
	if value == "" {
		return false
	}
	_, err := strconv.ParseUint(value, 10, 64)
	return err == nil
}

func validImmutableID(id string, prefix byte) bool {
	if len(id) < 2 || id[0] != prefix {
		return false
	}
	_, err := strconv.ParseUint(id[1:], 10, 64)
	return err == nil
}

func validSessionID(id string) bool { return validImmutableID(id, '$') }

func semanticCapture(s string) string {
	clean := stripTerminalControls(s)
	lines := strings.Split(clean, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	return strings.Trim(strings.Join(lines, "\n"), "\n")
}

// physicalSemanticCapture removes terminal controls without changing the row
// coordinates shared with the styled capture. Crop selection relies on this
// alignment, including blank rows at either edge of the pane.
func physicalSemanticCapture(s string) string {
	clean := stripTerminalControls(s)
	lines := strings.Split(clean, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	return strings.Join(lines, "\n")
}

func stripTerminalControls(s string) string {
	const (
		stateText = iota
		stateEscape
		stateEscapeIntermediate
		stateCSI
		stateString
		stateStringEscape
	)

	var out strings.Builder
	out.Grow(len(s))
	state := stateText
	for i := 0; i < len(s); {
		b := s[i]
		switch state {
		case stateEscape:
			i++
			switch b {
			case '[':
				state = stateCSI
			case ']', 'P', 'X', '^', '_':
				state = stateString
			default:
				if b >= 0x20 && b <= 0x2f {
					state = stateEscapeIntermediate
				} else {
					state = stateText
				}
			}
		case stateEscapeIntermediate:
			i++
			if b >= 0x30 && b <= 0x7e {
				state = stateText
			} else if b == 0x1b {
				state = stateEscape
			}
		case stateCSI:
			i++
			if b >= 0x40 && b <= 0x7e {
				state = stateText
			} else if b == 0x1b {
				state = stateEscape
			}
		case stateString:
			i++
			if b == 0x07 {
				state = stateText
			} else if b == 0x1b {
				state = stateStringEscape
			} else if b == 0x9c {
				state = stateText
			} else if b == 0xc2 && i < len(s) && s[i] == 0x9c {
				i++
				state = stateText
			}
		case stateStringEscape:
			i++
			if b == '\\' {
				state = stateText
			} else if b != 0x1b {
				state = stateString
			}
		default:
			switch {
			case b == 0x1b:
				state = stateEscape
				i++
			case b == '\n' || b == '\t':
				out.WriteByte(b)
				i++
			case b < 0x20 || b == 0x7f:
				i++
			case b < utf8.RuneSelf:
				out.WriteByte(b)
				i++
			default:
				r, size := utf8.DecodeRuneInString(s[i:])
				if r == utf8.RuneError && size == 1 {
					switch b {
					case 0x90, 0x98, 0x9d, 0x9e, 0x9f:
						state = stateString
					case 0x9b:
						state = stateCSI
					}
					i++
					continue
				}
				if r >= 0x80 && r <= 0x9f {
					switch r {
					case 0x90, 0x98, 0x9d, 0x9e, 0x9f:
						state = stateString
					case 0x9b:
						state = stateCSI
					}
					i += size
					continue
				}
				out.WriteString(s[i : i+size])
				i += size
			}
		}
	}
	return out.String()
}

func ValidKeys(keys []string) error {
	if len(keys) == 0 {
		return fmt.Errorf("missing keys")
	}
	for _, key := range keys {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("empty key")
		}
		if strings.ContainsAny(key, "\x00\n\r") {
			return fmt.Errorf("invalid key %q", key)
		}
	}
	return nil
}

func ShellQuote(path string) string {
	return "'" + strings.ReplaceAll(path, "'", "'\"'\"'") + "'"
}

func TimeoutContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, 15*time.Second)
}
