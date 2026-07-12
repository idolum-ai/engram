// Package inspect provides bounded, read-only local diagnostics over persisted
// Engram watches and tmux. It constructs no network or presentation clients.
package inspect

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/mechanics"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/tmux"
)

const frameRows = 64
const maxFrameBytes = 128 << 10

type Inspector struct {
	Home   string
	Runner tmux.Runner
}

type UsageError struct {
	Message string
}

func (e UsageError) Error() string { return e.Message }

func IsUsageError(err error) bool {
	var usage UsageError
	return errors.As(err, &usage)
}

func HomeFromEnvironment() (string, error) {
	home := config.ExpandPath(strings.TrimSpace(os.Getenv("ENGRAM_HOME")))
	if home == "" {
		home = config.ExpandPath("~/.engram")
	}
	abs, err := filepath.Abs(home)
	if err != nil {
		return "", fmt.Errorf("resolve ENGRAM_HOME: %w", err)
	}
	return filepath.Clean(abs), nil
}

func (i Inspector) Run(ctx context.Context, args []string, out io.Writer) error {
	if out == nil {
		return fmt.Errorf("missing output writer")
	}
	if len(args) == 1 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h") {
		_, err := io.WriteString(out, inspectUsage)
		return err
	}
	command, id, err := parseCommand(args)
	if err != nil {
		return err
	}
	home := strings.TrimSpace(i.Home)
	if home == "" {
		return fmt.Errorf("missing Engram home")
	}
	snapshot, err := state.ReadSnapshot(filepath.Join(home, "state.json"))
	if err != nil {
		return err
	}
	manager := tmux.New(i.Runner)
	switch command {
	case "status":
		return writeStatus(ctx, out, home, snapshot, manager)
	case "sessions":
		return writeSessions(out, snapshot)
	case "frame":
		return writeFrame(ctx, out, snapshot, manager, id)
	}
	return UsageError{Message: "unknown inspect command"}
}

const inspectUsage = `Usage:
  engram inspect status
  engram inspect sessions
  engram inspect frame <watch-id>
`

func parseCommand(args []string) (string, int, error) {
	if len(args) == 0 {
		return "", 0, UsageError{Message: "usage: engram inspect <status|sessions|frame>"}
	}
	switch args[0] {
	case "status", "sessions":
		if len(args) != 1 {
			return "", 0, UsageError{Message: "usage: engram inspect " + args[0]}
		}
		return args[0], 0, nil
	case "frame":
		if len(args) != 2 {
			return "", 0, UsageError{Message: "usage: engram inspect frame <watch-id>"}
		}
		id, err := strconv.Atoi(args[1])
		if err != nil || id <= 0 {
			return "", 0, UsageError{Message: "watch ID must be a positive integer"}
		}
		return args[0], id, nil
	default:
		return "", 0, UsageError{Message: fmt.Sprintf("unknown inspect command %q; use status, sessions, or frame", args[0])}
	}
}

func writeStatus(ctx context.Context, out io.Writer, home string, snapshot state.State, manager tmux.Manager) error {
	tctx, cancel := tmux.TimeoutContext(ctx)
	defer cancel()
	panes, err := manager.ListPanes(tctx)
	if err != nil {
		if _, writeErr := fmt.Fprintf(out, "Engram inspect\nhome: %s\nstate: readable\nschema: %d\nwatches: %d\ntmux: unavailable\n",
			cleanLine(home), snapshot.Version, len(snapshot.TerminalSessions)); writeErr != nil {
			return writeErr
		}
		return fmt.Errorf("inspect tmux: %w", err)
	}
	_, err = fmt.Fprintf(out, "Engram inspect\nhome: %s\nstate: readable\nschema: %d\nwatches: %d\ntmux: available (%d panes)\n",
		cleanLine(home), snapshot.Version, len(snapshot.TerminalSessions), len(panes))
	return err
}

func writeSessions(out io.Writer, snapshot state.State) error {
	sessions := append([]state.TerminalSession(nil), snapshot.TerminalSessions...)
	sort.Slice(sessions, func(a, b int) bool { return sessions[a].ID < sessions[b].ID })
	if _, err := fmt.Fprintln(out, "Engram watches"); err != nil {
		return err
	}
	if len(sessions) == 0 {
		_, err := fmt.Fprintln(out, "none")
		return err
	}
	for _, session := range sessions {
		if _, err := fmt.Fprintf(out, "[%d] state=%s origin=%s pane=%s window=%s title=%s cwd=%s\n",
			session.ID,
			cleanLine(string(session.State)),
			cleanLine(string(session.Origin)),
			cleanLine(session.TmuxPaneID),
			cleanLine(session.TmuxWindowID),
			valueOrDash(cleanLine(session.Title)),
			valueOrDash(cleanLine(session.LastKnownCWD)),
		); err != nil {
			return err
		}
	}
	return nil
}

func writeFrame(ctx context.Context, out io.Writer, snapshot state.State, manager tmux.Manager, id int) error {
	session, ok := findSession(snapshot.TerminalSessions, id)
	if !ok {
		return fmt.Errorf("watch %d not found", id)
	}
	if session.State == state.TerminalClosed {
		return fmt.Errorf("watch %d is closed", id)
	}
	tctx, cancel := tmux.TimeoutContext(ctx)
	defer cancel()
	pane, text, err := mechanics.New(manager).CaptureLiteral(tctx, mechanics.Binding{
		PaneID:   session.TmuxPaneID,
		WindowID: session.TmuxWindowID,
		ServerID: session.TmuxServerID,
	}, frameRows)
	if err != nil {
		return fmt.Errorf("inspect frame: %w", err)
	}
	header := fmt.Sprintf("watch: %d\npane: %s\nwindow: %s\ncwd: %s\n---\n",
		session.ID, cleanLine(pane.ID), cleanLine(pane.WindowID), valueOrDash(cleanLine(pane.CurrentPath)))
	text = cleanDisplayText(text)
	remaining := maxFrameBytes - len(header) - 1
	text = limitUTF8(text, max(remaining, 0))
	if _, err := io.WriteString(out, header); err != nil {
		return err
	}
	if text != "" {
		if _, err := io.WriteString(out, text); err != nil {
			return err
		}
	}
	_, err = io.WriteString(out, "\n")
	return err
}

func findSession(sessions []state.TerminalSession, id int) (state.TerminalSession, bool) {
	for _, session := range sessions {
		if session.ID == id {
			return session, true
		}
	}
	return state.TerminalSession{}, false
}

func cleanLine(value string) string {
	var out strings.Builder
	for _, r := range strings.TrimSpace(value) {
		if unsafeDisplayRune(r) {
			out.WriteByte(' ')
			continue
		}
		out.WriteRune(r)
		if out.Len() >= 512 {
			break
		}
	}
	return strings.Join(strings.Fields(out.String()), " ")
}

func cleanDisplayText(value string) string {
	var out strings.Builder
	out.Grow(len(value))
	for _, r := range value {
		switch {
		case r == '\n' || r == '\t':
			out.WriteRune(r)
		case unsafeDisplayRune(r):
			continue
		default:
			out.WriteRune(r)
		}
	}
	return out.String()
}

func unsafeDisplayRune(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r)
}

func valueOrDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func limitUTF8(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
