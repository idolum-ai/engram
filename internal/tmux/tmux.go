package tmux

import (
	"bytes"
	"context"
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

const paneFormat = "#{session_id}\t#{window_id}\t#{pane_id}\t#{session_name}\t#{window_index}\t#{pane_index}\t#{pane_active}\t#{pane_current_path}\t#{pane_current_command}"

const (
	compatFullMaxBytes = 4 << 20
	compatFullMaxLines = 50_000
)

func New(r Runner) Manager {
	return Manager{Runner: r}
}

func (m Manager) EnsureSession(ctx context.Context, name, workdir string) error {
	if _, err := m.Runner.Run(ctx, "has-session", "-t", name); err == nil {
		return nil
	}
	_, err := m.Runner.Run(ctx, "new-session", "-d", "-s", name, "-c", workdir)
	return err
}

func (m Manager) NewWindow(ctx context.Context, session, workdir, title string) (sessionID, windowID, paneID string, err error) {
	format := "#{session_id}\t#{window_id}\t#{pane_id}"
	out, err := m.Runner.Run(ctx, "new-window", "-P", "-F", format, "-n", title, "-c", workdir, "-t", session)
	if err != nil {
		return "", "", "", err
	}
	parts := strings.Split(strings.TrimSpace(out), "\t")
	if len(parts) != 3 {
		return "", "", "", fmt.Errorf("unexpected tmux new-window output %q", out)
	}
	return parts[0], parts[1], parts[2], nil
}

func (m Manager) ListSessions(ctx context.Context) ([]Session, error) {
	out, err := m.Runner.Run(ctx, "list-sessions", "-F", "#{session_name}\t#{session_id}\t#{session_windows}\t#{session_attached}")
	if err != nil {
		return nil, err
	}
	var sessions []Session
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		session := Session{Name: parts[0]}
		if len(parts) > 1 {
			session.ID = parts[1]
		}
		if len(parts) > 2 {
			session.Windows = parts[2]
		}
		if len(parts) > 3 {
			session.Attached = parts[3]
		}
		sessions = append(sessions, session)
	}
	return sessions, nil
}

func (m Manager) ListWindows(ctx context.Context) ([]Window, error) {
	format := "#{session_name}\t#{window_index}\t#{window_id}\t#{window_name}\t#{window_active}\t#{pane_id}\t#{pane_current_path}\t#{pane_current_command}"
	out, err := m.Runner.Run(ctx, "list-windows", "-a", "-F", format)
	if err != nil {
		return nil, err
	}
	return parseWindows(out), nil
}

func (m Manager) ResolveTarget(ctx context.Context, target string) (Window, error) {
	format := "#{session_name}\t#{window_index}\t#{window_id}\t#{window_name}\t#{window_active}\t#{pane_id}\t#{pane_current_path}\t#{pane_current_command}"
	out, err := m.Runner.Run(ctx, "display-message", "-p", "-t", target, format)
	if err != nil {
		return Window{}, err
	}
	windows := parseWindows(out)
	if len(windows) != 1 {
		return Window{}, fmt.Errorf("unexpected tmux target output %q", out)
	}
	return windows[0], nil
}

func (m Manager) ListPanes(ctx context.Context) ([]Pane, error) {
	out, err := m.Runner.Run(ctx, "list-panes", "-a", "-F", paneFormat)
	if err != nil {
		return nil, err
	}
	return parsePanes(out)
}

func (m Manager) InspectPane(ctx context.Context, target string) (Pane, error) {
	out, err := m.Runner.Run(ctx, "display-message", "-p", "-t", target, paneFormat)
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

// ValidatePane resolves paneID and verifies that it still belongs to windowID.
// Both arguments must be tmux immutable IDs, not names or numeric indexes.
func (m Manager) ValidatePane(ctx context.Context, paneID, windowID string) (Pane, error) {
	if !validImmutableID(paneID, '%') {
		return Pane{}, fmt.Errorf("invalid tmux pane ID %q", paneID)
	}
	if !validImmutableID(windowID, '@') {
		return Pane{}, fmt.Errorf("invalid tmux window ID %q", windowID)
	}
	pane, err := m.InspectPane(ctx, paneID)
	if err != nil {
		return Pane{}, err
	}
	if pane.ID != paneID || pane.WindowID != windowID {
		return Pane{}, fmt.Errorf("tmux pane identity mismatch: got pane %q window %q, want pane %q window %q", pane.ID, pane.WindowID, paneID, windowID)
	}
	return pane, nil
}

// IsIdentityLoss reports errors that prove a persisted immutable pane/window
// identity is no longer usable. Cancellation and generic execution failures do
// not prove that the pane disappeared.
func IsIdentityLoss(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "can't find pane:") ||
		strings.Contains(message, "can't find window:") ||
		strings.Contains(message, "tmux pane identity mismatch") ||
		strings.Contains(message, "invalid tmux pane id") ||
		strings.Contains(message, "invalid tmux window id")
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

// CaptureVisibleSemantic returns model-facing text with wrapped lines joined
// and terminal control sequences removed.
func (m Manager) CaptureVisibleSemantic(ctx context.Context, paneID string) (string, error) {
	out, err := m.Runner.Run(ctx, "capture-pane", "-p", "-J", "-t", paneID)
	return semanticCapture(out), err
}

// CaptureScrollbackTail returns at most maxBytes and maxLines of semantic text.
// ExecRunner streams the full capture through fixed-capacity storage. A legacy
// Runner without StreamRunner support must materialize tmux stdout first, but
// the returned value remains bounded.
func (m Manager) CaptureScrollbackTail(ctx context.Context, paneID string, maxBytes, maxLines int) (string, error) {
	if maxBytes <= 0 {
		return "", fmt.Errorf("max bytes must be positive")
	}
	if maxLines <= 0 {
		return "", fmt.Errorf("max lines must be positive")
	}

	tail := newTailBuffer(maxBytes)
	args := []string{"capture-pane", "-p", "-J", "-S", "-", "-E", "-", "-t", paneID}
	var err error
	if runner, ok := m.Runner.(StreamRunner); ok {
		err = runner.RunToWriter(ctx, tail, args...)
	} else {
		var out string
		out, err = m.Runner.Run(ctx, args...)
		_, _ = tail.Write([]byte(out))
	}
	return tailLines(semanticCapture(string(tail.Bytes())), maxLines), err
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

// CaptureVisible is retained for application compatibility.
func (m Manager) CaptureVisible(ctx context.Context, paneID string) (string, error) {
	return m.CaptureVisibleSemantic(ctx, paneID)
}

// CaptureFull is retained as a bounded compatibility wrapper.
func (m Manager) CaptureFull(ctx context.Context, paneID string) (string, error) {
	return m.CaptureScrollbackTail(ctx, paneID, compatFullMaxBytes, compatFullMaxLines)
}

func (m Manager) KillWindow(ctx context.Context, windowID string) error {
	_, err := m.Runner.Run(ctx, "kill-window", "-t", windowID)
	return err
}

func (m Manager) PaneCWD(ctx context.Context, paneID string) (string, error) {
	out, err := m.Runner.Run(ctx, "display-message", "-p", "-t", paneID, "#{pane_current_path}")
	return strings.TrimSpace(out), err
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

func parseWindows(out string) []Window {
	var windows []Window
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		for len(parts) < 8 {
			parts = append(parts, "")
		}
		windows = append(windows, Window{
			SessionName: parts[0],
			Index:       parts[1],
			ID:          parts[2],
			Name:        parts[3],
			Active:      parts[4],
			PaneID:      parts[5],
			CurrentPath: parts[6],
			CurrentCmd:  parts[7],
		})
	}
	return windows
}

func parsePanes(out string) ([]Pane, error) {
	var panes []Pane
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 9)
		if len(parts) != 9 {
			return nil, fmt.Errorf("unexpected tmux pane output line %q", line)
		}
		if !validImmutableID(parts[0], '$') || !validImmutableID(parts[1], '@') || !validImmutableID(parts[2], '%') {
			return nil, fmt.Errorf("unexpected tmux pane identity in line %q", line)
		}
		if parts[6] != "0" && parts[6] != "1" {
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

func validImmutableID(id string, prefix byte) bool {
	if len(id) < 2 || id[0] != prefix {
		return false
	}
	_, err := strconv.ParseUint(id[1:], 10, 64)
	return err == nil
}

type tailBuffer struct {
	buf   []byte
	start int
	size  int
}

func newTailBuffer(capacity int) *tailBuffer {
	return &tailBuffer{buf: make([]byte, capacity)}
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	written := len(p)
	if len(p) >= len(b.buf) {
		copy(b.buf, p[len(p)-len(b.buf):])
		b.start = 0
		b.size = len(b.buf)
		return written, nil
	}
	if overflow := b.size + len(p) - len(b.buf); overflow > 0 {
		b.start = (b.start + overflow) % len(b.buf)
		b.size -= overflow
	}
	end := (b.start + b.size) % len(b.buf)
	first := min(len(p), len(b.buf)-end)
	copy(b.buf[end:], p[:first])
	copy(b.buf, p[first:])
	b.size += len(p)
	return written, nil
}

func (b *tailBuffer) Bytes() []byte {
	out := make([]byte, b.size)
	first := min(b.size, len(b.buf)-b.start)
	copy(out, b.buf[b.start:b.start+first])
	copy(out[first:], b.buf[:b.size-first])
	return out
}

func tailLines(s string, maxLines int) string {
	if s == "" {
		return ""
	}
	lines := 1
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] != '\n' {
			continue
		}
		lines++
		if lines > maxLines {
			return s[i+1:]
		}
	}
	return s
}

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
