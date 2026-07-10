package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/idolum-ai/engram/internal/app"
	"github.com/idolum-ai/engram/internal/commands"
	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/terminalshot"
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
  engram version
  engram help
`)
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
	if cfg.SnapshotAnchors() {
		if _, err := terminalshot.New(cfg.SnapshotBrowser, cfg.SnapshotTheme).Available(); err != nil {
			fmt.Fprintln(os.Stderr, "snapshot anchor mode:", err)
			return 1
		}
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
	fmt.Print(diagnosticsText(cfg, mode))
	return 0
}

func diagnosticsText(cfg config.Config, mode string) string {
	tmuxPath, tmuxErr := exec.LookPath("tmux")
	if tmuxErr != nil {
		tmuxPath = "missing"
	}
	snapshotPath, snapshotErr := terminalshot.New(cfg.SnapshotBrowser, cfg.SnapshotTheme).Available()
	if snapshotErr != nil {
		snapshotPath = "unavailable"
	} else {
		snapshotPath += " (" + cfg.SnapshotTheme + ")"
	}
	stateStatus := "missing"
	var sessions int
	var lastUpdate int
	var journal int
	if st, err := readState(cfg.StatePath()); err == nil {
		stateStatus = "readable"
		sessions = len(st.TerminalSessions)
		lastUpdate = st.LastUpdateID
		journal = len(st.UpdateJournal)
	}
	model := cfg.AnthropicModel
	if cfg.SnapshotAnchors() {
		model = "disabled"
	}
	return fmt.Sprintf("Engram %s\nversion: %s\nenv: %s\nstate: %s (%s)\naudit: %s\nattachments: %s\nworkdir: %s\ntmux: %s\nanchor mode: %s\nsnapshots: %s\ntelegram user: %d\ntelegram chat: %d\nmodel: %s\nsessions: %d\nlast update: %d\nupdate journal: %d\ntelegram_api: not_called\nanthropic_api: not_called\npolling: not_started\nstatus: ok\n",
		mode,
		version.String(),
		cfg.EnvPath,
		cfg.StatePath(),
		stateStatus,
		cfg.AuditPath(),
		cfg.AttachmentDir(),
		cfg.Workdir,
		tmuxPath,
		cfg.EffectiveAnchorMode(),
		snapshotPath,
		cfg.TelegramAllowedUserID,
		cfg.TelegramChatID,
		model,
		sessions,
		lastUpdate,
		journal,
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
