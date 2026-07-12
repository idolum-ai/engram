// Package inspect provides bounded, read-only local diagnostics over persisted
// Engram watches and tmux. It constructs no network or presentation clients.
package inspect

import (
	"context"
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
	if len(args) == 0 {
		return fmt.Errorf("usage: engram inspect <status|sessions|frame>")
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
	switch args[0] {
	case "status":
		if len(args) != 1 {
			return fmt.Errorf("usage: engram inspect status")
		}
		return writeStatus(ctx, out, home, snapshot, manager)
	case "sessions":
		if len(args) != 1 {
			return fmt.Errorf("usage: engram inspect sessions")
		}
		return writeSessions(out, snapshot)
	case "frame":
		if len(args) != 2 {
			return fmt.Errorf("usage: engram inspect frame <watch-id>")
		}
		id, err := strconv.Atoi(args[1])
		if err != nil || id <= 0 {
			return fmt.Errorf("watch ID must be a positive integer")
		}
		return writeFrame(ctx, out, snapshot, manager, id)
	default:
		return fmt.Errorf("unknown inspect command %q; use status, sessions, or frame", args[0])
	}
}

func writeStatus(ctx context.Context, out io.Writer, home string, snapshot state.State, manager tmux.Manager) error {
	tctx, cancel := tmux.TimeoutContext(ctx)
	defer cancel()
	panes, err := manager.ListPanes(tctx)
	if err != nil {
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
	}, frameRows)
	if err != nil {
		return fmt.Errorf("inspect frame: %w", err)
	}
	text = limitUTF8(text, maxFrameBytes)
	if _, err := fmt.Fprintf(out, "watch: %d\npane: %s\nwindow: %s\ncwd: %s\n---\n",
		session.ID, cleanLine(pane.ID), cleanLine(pane.WindowID), valueOrDash(cleanLine(pane.CurrentPath))); err != nil {
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
		if unicode.IsControl(r) {
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
