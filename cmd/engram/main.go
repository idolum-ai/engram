package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/idolum-ai/engram/internal/app"
	"github.com/idolum-ai/engram/internal/commands"
	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/terminalshot"
	"github.com/idolum-ai/engram/internal/upstream"
	"github.com/idolum-ai/engram/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		printHelp()
		return 2
	}
	switch args[0] {
	case "run":
		fs := flag.NewFlagSet("run", flag.ContinueOnError)
		envPath := fs.String("env", config.DefaultEnvPath(), "path to .env")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		cfg, err := config.Load(*envPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "config:", err)
			return 1
		}
		a, err := app.New(cfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, "start:", err)
			return 1
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return a.Run(ctx)
	case "preflight":
		return runDiagnostics(args[1:], "preflight")
	case "status":
		return runDiagnostics(args[1:], "status")
	case "dry-start":
		return runDiagnostics(args[1:], "dry-start")
	case "version", "--version", "-v":
		fmt.Println(version.String())
		return 0
	case "commands":
		data, err := commands.JSON()
		if err != nil {
			fmt.Fprintln(os.Stderr, "commands:", err)
			return 1
		}
		fmt.Println(string(data))
		return 0
	case "signal":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: engram signal <message>")
			return 2
		}
		if err := emitSignal(strings.Join(args[1:], " "), openControllingTerminal); err != nil {
			fmt.Fprintln(os.Stderr, "signal:", err)
			return 1
		}
		return 0
	case "help", "--help", "-h":
		printHelp()
		return 0
	default:
		fmt.Fprintln(os.Stderr, "unknown command:", args[0])
		printHelp()
		return 2
	}
}

func printHelp() {
	fmt.Print(`Usage:
  engram run [--env ~/.engram/.env]
  engram preflight [--env ~/.engram/.env]
  engram status [--env ~/.engram/.env]
  engram dry-start [--env ~/.engram/.env]
  engram commands
  engram signal <message>
  engram version
  engram help
`)
}

func openControllingTerminal() (io.WriteCloser, error) {
	return os.OpenFile("/dev/tty", os.O_WRONLY, 0)
}

func emitSignal(message string, openTTY func() (io.WriteCloser, error)) error {
	tty, err := openTTY()
	if err != nil {
		return fmt.Errorf("open controlling terminal: %w", err)
	}
	writeErr := upstream.Write(tty, message)
	closeErr := tty.Close()
	return errors.Join(writeErr, closeErr)
}

func runDiagnostics(args []string, mode string) int {
	fs := flag.NewFlagSet(mode, flag.ContinueOnError)
	envPath := fs.String("env", config.DefaultEnvPath(), "path to .env")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(*envPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	st, stateErr := readState(cfg.StatePath())
	snapshotPath, snapshotReady, snapshotErr := probeSnapshot(cfg)
	anchorMode, err := cfg.ResolveAnchorMode(st.AnchorMode, config.ModeCapabilities{
		HaikuConfigured: cfg.HaikuConfigured(),
		SnapshotReady:   snapshotReady,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "anchor mode:", err)
		if snapshotErr != nil {
			fmt.Fprintln(os.Stderr, "snapshot probe:", snapshotErr)
		}
		return 1
	}
	if mode == "dry-start" {
		if err := config.EnsureDirs(cfg); err != nil {
			fmt.Fprintln(os.Stderr, "dirs:", err)
			return 1
		}
		store, err := state.Open(cfg.StatePath(), cfg.AuditPath())
		if err != nil {
			fmt.Fprintln(os.Stderr, "state:", err)
			return 1
		}
		_ = store
	}
	fmt.Print(formatDiagnostics(cfg, mode, st, stateErr == nil, anchorMode, snapshotPath))
	return 0
}

func diagnosticsText(cfg config.Config, mode string) string {
	st, stateErr := readState(cfg.StatePath())
	snapshotPath, snapshotReady, _ := probeSnapshot(cfg)
	anchorMode, err := cfg.ResolveAnchorMode(st.AnchorMode, config.ModeCapabilities{
		HaikuConfigured: cfg.HaikuConfigured(),
		SnapshotReady:   snapshotReady,
	})
	if err != nil {
		anchorMode = "unavailable"
	}
	return formatDiagnostics(cfg, mode, st, stateErr == nil, anchorMode, snapshotPath)
}

func probeSnapshot(cfg config.Config) (string, bool, error) {
	path, err := terminalshot.New(cfg.SnapshotBrowser, cfg.SnapshotTheme).Probe(context.Background())
	if err != nil {
		return "unavailable", false, err
	}
	return path + " (" + cfg.SnapshotTheme + ")", true, nil
}

func formatDiagnostics(cfg config.Config, mode string, st state.State, stateReadable bool, anchorMode, snapshotPath string) string {
	tmuxPath, tmuxErr := exec.LookPath("tmux")
	if tmuxErr != nil {
		tmuxPath = "missing"
	}
	stateStatus := "missing"
	if stateReadable {
		stateStatus = "readable"
	}
	model := "unavailable"
	haiku := "unavailable"
	if cfg.HaikuConfigured() {
		model = cfg.AnthropicModel
		haiku = "configured, not probed"
	}
	return fmt.Sprintf("Engram %s\nversion: %s\nenv: %s\nstate: %s (%s)\naudit: %s\nattachments: %s\nworkdir: %s\ntmux: %s\nanchor mode: %s\nhaiku: %s\nsnapshots: %s\ntelegram user: %d\ntelegram chat: %d\nmodel: %s\nsessions: %d\nlast update: %d\nupdate journal: %d\ntelegram_api: not_called\nanthropic_api: not_called\npolling: not_started\nstatus: ok\n",
		mode,
		version.String(),
		cfg.EnvPath,
		cfg.StatePath(),
		stateStatus,
		cfg.AuditPath(),
		cfg.AttachmentDir(),
		cfg.Workdir,
		tmuxPath,
		anchorMode,
		haiku,
		snapshotPath,
		cfg.TelegramAllowedUserID,
		cfg.TelegramChatID,
		model,
		len(st.TerminalSessions),
		st.LastUpdateID,
		len(st.UpdateJournal),
	)
}

func readState(path string) (state.State, error) {
	var st state.State
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return st, err
	}
	if len(b) == 0 {
		return st, nil
	}
	err = json.Unmarshal(b, &st)
	return st, err
}
