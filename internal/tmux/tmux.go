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

func (ExecRunner) Run(ctx context.Context, args ...string) (string, error) {
	var out bytes.Buffer
	err := (ExecRunner{}).RunToWriter(ctx, &out, args...)
	return out.String(), err
}

func (ExecRunner) RunToWriter(ctx context.Context, dst io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "tmux", args...)
	var errOut bytes.Buffer
	cmd.Stdout = dst
	cmd.Stderr = &errOut
	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("tmux %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errOut.String()))
	}
	return nil
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
	Columns     int
	VisibleRows int
	BufferRows  int
	Title       string
	CurrentPath string
}

const paneRecordFormat = "#{n:session_id}:#{session_id}#{n:window_id}:#{window_id}#{n:pane_id}:#{pane_id}#{n:session_name}:#{session_name}#{n:window_index}:#{window_index}#{n:pane_index}:#{pane_index}#{n:pane_active}:#{pane_active}#{n:pane_current_path}:#{pane_current_path}#{n:pane_current_command}:#{pane_current_command}"

const serverIDOption = "@engram_server_id"
const identityMismatchMarker = "ENGRAM_IDENTITY_MISMATCH"

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
	if id, ok := m.findSessionID(ctx, name); ok {
		return id, nil
	}
	format := recordFormat("session_id", "session_name")
	out, createErr := m.Runner.Run(ctx, "new-session", "-d", "-P", "-F", format, "-s", name, "-c", workdir)
	if createErr != nil {
		if id, ok := m.findSessionID(ctx, name); ok {
			return id, nil
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

func (m Manager) findSessionID(ctx context.Context, name string) (string, bool) {
	sessions, err := m.ListSessions(ctx)
	if err != nil {
		return "", false
	}
	for _, session := range sessions {
		if session.Name == name && validSessionID(session.ID) {
			return session.ID, true
		}
	}
	return "", false
}

func (m Manager) NewWindow(ctx context.Context, sessionID, workdir, title string) (windowID, paneID string, err error) {
	if !validSessionID(sessionID) {
		return "", "", fmt.Errorf("invalid tmux session ID %q", sessionID)
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
	return parts[0], parts[1], nil
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
	current, err := m.CurrentServerID(ctx)
	if err != nil {
		return Pane{}, err
	}
	if current != serverID {
		return Pane{}, &IdentityError{Reason: "tmux server incarnation mismatch"}
	}
	return m.ValidatePane(ctx, paneID, windowID)
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

// CaptureVisibleRaw preserves physical wrapped lines, attributes, trailing
// spaces, and tmux's final newline.
func (m Manager) CaptureVisibleRaw(ctx context.Context, paneID string) (string, error) {
	return m.Runner.Run(ctx, "capture-pane", "-p", "-e", "-N", "-t", paneID)
}

// CaptureLiteral returns a bounded plain-text frame without creating tmux
// paste buffers or preserving terminal control sequences.
func (m Manager) CaptureLiteral(ctx context.Context, paneID string, targetRows int) (string, error) {
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
	start := visibleRows - targetRows
	end := visibleRows - 1
	out, err := m.Runner.Run(ctx, "capture-pane", "-p", "-N", "-S", strconv.Itoa(start), "-E", strconv.Itoa(end), "-t", paneID)
	if err != nil {
		return "", err
	}
	return semanticCapture(out), nil
}

func (m Manager) CaptureStyled(ctx context.Context, paneID string, targetRows int) (StyledCapture, error) {
	if targetRows <= 0 || targetRows > 400 {
		return StyledCapture{}, fmt.Errorf("target rows must be between 1 and 400")
	}
	meta, err := m.Runner.Run(ctx, "display-message", "-p", "-t", paneID, recordFormat("pane_width", "pane_height", "pane_title", "pane_current_path"))
	if err != nil {
		return StyledCapture{}, err
	}
	records, parseErr := parseRecords(meta, 4)
	if parseErr != nil || len(records) != 1 {
		return StyledCapture{}, fmt.Errorf("unexpected tmux snapshot metadata")
	}
	parts := records[0]
	columns, err := strconv.Atoi(parts[0])
	if err != nil || columns <= 0 || columns > 400 {
		return StyledCapture{}, fmt.Errorf("invalid tmux pane width %q", parts[0])
	}
	visibleRows, err := strconv.Atoi(parts[1])
	if err != nil || visibleRows <= 0 || visibleRows > 400 {
		return StyledCapture{}, fmt.Errorf("invalid tmux pane height %q", parts[1])
	}
	start := visibleRows - targetRows
	end := visibleRows - 1
	nonce, err := captureNonce()
	if err != nil {
		return StyledCapture{}, err
	}
	physicalBuffer := "engram-physical-" + nonce
	joinedBuffer := "engram-joined-" + nonce
	_, err = m.Runner.Run(ctx,
		"capture-pane", "-e", "-N", "-S", strconv.Itoa(start), "-E", strconv.Itoa(end), "-t", paneID, "-b", physicalBuffer,
		";", "capture-pane", "-J", "-S", strconv.Itoa(start), "-E", strconv.Itoa(end), "-t", paneID, "-b", joinedBuffer,
	)
	if err != nil {
		m.cleanupCaptureBuffers(ctx, physicalBuffer, joinedBuffer)
		return StyledCapture{}, err
	}
	defer m.cleanupCaptureBuffers(ctx, physicalBuffer, joinedBuffer)
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
		Text:        semanticCapture(ansi),
		JoinedText:  semanticCapture(joined),
		Columns:     columns,
		VisibleRows: visibleRows,
		BufferRows:  bufferRows,
		Title:       parts[2],
		CurrentPath: parts[3],
	}, nil
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

// DumpScrollback streams a physical, ANSI-preserving capture to dst. It does
// not join wrapped lines or buffer tmux stdout in Engram. The runner must
// implement StreamRunner so this memory behavior is explicit.
func (m Manager) DumpScrollback(ctx context.Context, paneID string, dst io.Writer) error {
	if dst == nil {
		return fmt.Errorf("missing scrollback destination")
	}
	runner, ok := m.Runner.(StreamRunner)
	if !ok {
		return fmt.Errorf("tmux runner does not support streaming")
	}
	return runner.RunToWriter(ctx, dst, "capture-pane", "-p", "-e", "-N", "-S", "-", "-E", "-", "-t", paneID)
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
	condition := fmt.Sprintf("#{&&:#{==:#{%s},%s},#{==:#{window_id},%s}}", serverIDOption, serverID, windowID)
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
	panes := make([]Pane, 0, len(records))
	for _, parts := range records {
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
	body := strings.TrimSuffix(out, "\n")
	if body == "" || strings.HasSuffix(body, "\n") {
		return nil, fmt.Errorf("unexpected record boundary")
	}
	lines := strings.Split(body, "\n")
	records := make([][]string, 0, len(lines))
	for _, line := range lines {
		fields, err := parseRecord(line, fieldCount)
		if err != nil {
			return nil, err
		}
		records = append(records, fields)
	}
	return records, nil
}

func parseRecord(line string, fieldCount int) ([]string, error) {
	remaining := line
	fields := make([]string, 0, fieldCount)
	for i := 0; i < fieldCount; i++ {
		separator := strings.IndexByte(remaining, ':')
		if separator <= 0 {
			return nil, fmt.Errorf("field %d has no byte length", i)
		}
		length, err := strconv.ParseUint(remaining[:separator], 10, 64)
		if err != nil || length > uint64(len(remaining)-separator-1) {
			return nil, fmt.Errorf("field %d has invalid byte length", i)
		}
		valueStart := separator + 1
		valueEnd := valueStart + int(length)
		value := remaining[valueStart:valueEnd]
		if !utf8.ValidString(value) {
			return nil, fmt.Errorf("field %d is not UTF-8", i)
		}
		fields = append(fields, value)
		remaining = remaining[valueEnd:]
	}
	if remaining != "" {
		return nil, fmt.Errorf("record has trailing bytes")
	}
	return fields, nil
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
