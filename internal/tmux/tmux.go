package tmux

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type Runner interface {
	Run(ctx context.Context, args ...string) (string, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "tmux", args...)
	var out bytes.Buffer
	var errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("tmux %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errOut.String()))
	}
	return out.String(), nil
}

type Manager struct {
	Runner Runner
}

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

func (m Manager) SendCommand(ctx context.Context, paneID, text string) error {
	if _, err := m.Runner.Run(ctx, "send-keys", "-t", paneID, "-l", "--", text); err != nil {
		return err
	}
	_, err := m.Runner.Run(ctx, "send-keys", "-t", paneID, "C-m")
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

func (m Manager) CaptureVisible(ctx context.Context, paneID string) (string, error) {
	out, err := m.Runner.Run(ctx, "capture-pane", "-p", "-e", "-J", "-t", paneID)
	return strings.TrimRight(out, "\n"), err
}

func (m Manager) CaptureFull(ctx context.Context, paneID string) (string, error) {
	out, err := m.Runner.Run(ctx, "capture-pane", "-p", "-e", "-J", "-S", "-", "-E", "-", "-t", paneID)
	return strings.TrimRight(out, "\n"), err
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
