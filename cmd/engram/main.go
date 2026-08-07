package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/idolum-ai/engram/internal/agentcompat"
	"github.com/idolum-ai/engram/internal/agentdoctor"
	"github.com/idolum-ai/engram/internal/app"
	"github.com/idolum-ai/engram/internal/claudeui"
	"github.com/idolum-ai/engram/internal/commands"
	"github.com/idolum-ai/engram/internal/compatfixture"
	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/inspect"
	"github.com/idolum-ai/engram/internal/lockfile"
	"github.com/idolum-ai/engram/internal/recovery"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/templates"
	"github.com/idolum-ai/engram/internal/terminalshot"
	"github.com/idolum-ai/engram/internal/tmux"
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
		if _, err := exec.LookPath("tmux"); err != nil {
			fmt.Fprintln(os.Stderr, "start: tmux executable not found in PATH:", err)
			return 1
		}
		a, err := app.New(cfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, "start:", err)
			return 1
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if statusPath := strings.TrimSpace(os.Getenv(serviceStatusFileEnv)); statusPath != "" {
			cleanup, err := publishServiceIdentity(statusPath, version.String(), os.Getpid(), time.Now().UTC())
			if err != nil {
				fmt.Fprintln(os.Stderr, "start:", err)
				return 1
			}
			defer cleanup()
		}
		return a.Run(ctx)
	case "preflight":
		return runDiagnostics(args[1:], "preflight")
	case "status":
		return runDiagnostics(args[1:], "status")
	case "dry-start":
		return runDiagnostics(args[1:], "dry-start")
	case "inspect":
		home, err := inspect.HomeFromEnvironment()
		if err != nil {
			fmt.Fprintln(os.Stderr, "inspect:", err)
			return 1
		}
		if err := (inspect.Inspector{Home: home, Runner: tmux.ExecRunner{}}).Run(context.Background(), args[1:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "inspect:", err)
			if inspect.IsUsageError(err) {
				return 2
			}
			return 1
		}
		return 0
	case "doctor":
		return runAgentDoctor(args[1:])
	case "compatibility":
		return runCompatibility(args[1:])
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
		stdout := len(args) >= 2 && args[1] == "--stdout"
		messageStart := 1
		if stdout {
			messageStart = 2
		}
		if len(args) <= messageStart {
			fmt.Fprintln(os.Stderr, "usage: engram signal [--stdout] <message>")
			return 2
		}
		message := strings.Join(args[messageStart:], " ")
		var err error
		if stdout {
			err = upstream.Write(os.Stdout, message)
		} else {
			err = emitSignal(message, openControllingTerminal)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "signal:", err)
			return 1
		}
		return 0
	case "codex-hook":
		if err := runCodexHook(os.Stdin, os.Getenv("TMUX_PANE"), tmux.New(tmux.ExecRunner{}), time.Now().UTC()); err != nil {
			fmt.Fprintln(os.Stderr, "codex-hook:", err)
			return 1
		}
		return 0
	case "claude-hook":
		if err := runClaudeHook(os.Stdin, os.Getenv("TMUX_PANE"), tmux.New(tmux.ExecRunner{}), time.Now().UTC()); err != nil {
			fmt.Fprintln(os.Stderr, "claude-hook:", err)
			return 1
		}
		return 0
	case "codex-bind":
		if len(args) != 1 {
			fmt.Fprintln(os.Stderr, "usage: engram codex-bind")
			return 2
		}
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, "codex-bind: resolve current directory:", err)
			return 1
		}
		if err := runCodexBind(os.Getenv("CODEX_THREAD_ID"), os.Getenv("TMUX_PANE"), cwd, tmux.New(tmux.ExecRunner{}), time.Now().UTC()); err != nil {
			fmt.Fprintln(os.Stderr, "codex-bind:", err)
			return 1
		}
		fmt.Println("Codex session binding published for this tmux pane. Engram will verify the active process and exact rollout before using it.")
		return 0
	case "claude-bind":
		if len(args) != 1 {
			fmt.Fprintln(os.Stderr, "usage: engram claude-bind")
			return 2
		}
		if err := runClaudeBind(os.Getenv("TMUX_PANE"), defaultClaudeConfigRoot(), tmux.New(tmux.ExecRunner{}), claudeui.NewDetector(), time.Now().UTC()); err != nil {
			fmt.Fprintln(os.Stderr, "claude-bind:", err)
			return 1
		}
		fmt.Println("Claude session binding published for this tmux pane. Engram will verify the active process and exact transcript before using it.")
		return 0
	case "github":
		return runGitHub(args[1:])
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
  engram inspect status
  engram inspect sessions
  engram inspect frame <watch-id>
  engram doctor agent [--provider codex|claude] [--pane %N] [--env ~/.engram/.env]
  engram compatibility capture --provider codex|claude --pane %N --out /tmp/candidate
  engram commands
  engram signal [--stdout] <message>
  engram codex-hook
  engram codex-bind
  engram claude-hook
  engram claude-bind
  engram github help
  engram version
  engram help
`)
}

func runAgentDoctor(args []string) int {
	envPath := config.DefaultEnvPath()
	explicitEnv := false
	filtered := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		if args[index] == "--env" {
			if index+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "doctor: --env requires a path")
				return 2
			}
			envPath = args[index+1]
			explicitEnv = true
			index++
			continue
		}
		filtered = append(filtered, args[index])
	}
	options := config.AgentOptions{}
	loaded, err := config.LoadAgentOptions(envPath)
	if err == nil {
		options = loaded
	} else if explicitEnv {
		fmt.Fprintln(os.Stderr, "doctor config:", err)
		return 1
	} else if _, statErr := os.Stat(config.ExpandPath(envPath)); statErr == nil || !errors.Is(statErr, fs.ErrNotExist) {
		fmt.Fprintln(os.Stderr, "doctor config:", err)
		return 1
	}
	doctor := agentdoctor.New(tmux.ExecRunner{}, options, os.Getenv("TMUX_PANE"))
	if err := doctor.Run(context.Background(), filtered, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "doctor:", err)
		if agentdoctor.IsUsageError(err) {
			return 2
		}
		return 1
	}
	return 0
}

func runCompatibility(args []string) int {
	if len(args) == 0 || args[0] != "capture" {
		fmt.Fprintln(os.Stderr, "usage: engram compatibility capture --provider codex|claude --pane %N --out /absolute/path")
		return 2
	}
	set := flag.NewFlagSet("compatibility capture", flag.ContinueOnError)
	provider := set.String("provider", "", "agent provider")
	pane := set.String("pane", os.Getenv("TMUX_PANE"), "exact tmux pane")
	out := set.String("out", "", "new output directory outside a Git worktree")
	if err := set.Parse(args[1:]); err != nil || len(set.Args()) != 0 {
		return 2
	}
	options := compatfixture.Options{Provider: agentcompat.Provider(strings.ToLower(strings.TrimSpace(*provider))), PaneID: strings.TrimSpace(*pane), Output: *out}
	if err := compatfixture.Capture(context.Background(), tmux.ExecRunner{}, options); err != nil {
		fmt.Fprintln(os.Stderr, "compatibility capture:", err)
		return 1
	}
	fmt.Println("Sanitized candidate written to", filepath.Clean(*out))
	fmt.Println("Review every file before copying any material into a repository; this command does not declare support.")
	return 0
}

func runCodexHook(input io.Reader, paneID string, manager tmux.Manager, now time.Time) error {
	metadata, err := recovery.ParseCodexSessionStart(input, now)
	return publishHookMetadata(metadata, err, paneID, manager)
}

func runClaudeHook(input io.Reader, paneID string, manager tmux.Manager, now time.Time) error {
	metadata, err := recovery.ParseClaudeSessionStart(input, now)
	return publishHookMetadata(metadata, err, paneID, manager)
}

func publishHookMetadata(metadata recovery.Metadata, parseErr error, paneID string, manager tmux.Manager) error {
	if parseErr != nil {
		return parseErr
	}
	if strings.TrimSpace(paneID) == "" {
		return errors.New("TMUX_PANE is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return manager.PublishRecoveryMetadata(ctx, paneID, metadata)
}

func runCodexBind(sessionID, paneID, cwd string, manager tmux.Manager, now time.Time) error {
	if !recovery.ValidSessionID(sessionID) {
		return errors.New("CODEX_THREAD_ID is missing or invalid; run this command from inside the intended active Codex session")
	}
	if strings.TrimSpace(paneID) == "" {
		return errors.New("TMUX_PANE is not set; run this command from the Codex session inside its watched tmux pane")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	metadata := recovery.Metadata{
		Version: 1, Program: recovery.ProgramCodex, SessionID: strings.ToLower(strings.TrimSpace(sessionID)),
		CWD: cwd, Source: "manual", Observed: now.UTC(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return manager.PublishRecoveryMetadata(ctx, paneID, metadata)
}

func runClaudeBind(paneID, configRoot string, manager tmux.Manager, detector *claudeui.Detector, now time.Time) error {
	if strings.TrimSpace(paneID) == "" {
		return errors.New("TMUX_PANE is not set; run this command from the Claude session inside its watched tmux pane")
	}
	if detector == nil {
		return errors.New("Claude process detector is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	panePID, foreground, err := manager.PaneProcess(ctx, paneID)
	if err != nil {
		return fmt.Errorf("inspect current tmux pane: %w", err)
	}
	runtime, err := detector.Detect(ctx, panePID, foreground)
	if err != nil || !runtime.Detected || runtime.PID <= 0 || runtime.Identity == "" || runtime.StartedAt.IsZero() {
		return errors.New("one exact active Claude process could not be proven; restart Claude after configuring the documented SessionStart hook")
	}
	registry, err := readClaudeRegistry(configRoot, runtime.PID)
	registryStart, startErr := time.ParseInLocation("Mon Jan _2 15:04:05 2006", registry.ProcStart, time.Local)
	if err != nil || startErr != nil || registry.PID != runtime.PID || registry.Version != runtime.Version ||
		registryStart.Unix() != runtime.StartedAt.Unix() || !recovery.ValidSessionID(registry.SessionID) {
		return errors.New("the active Claude session registry could not be proven; restart Claude after configuring the documented SessionStart hook")
	}
	transcript, err := findClaudeTranscript(filepath.Join(configRoot, "projects"), registry.SessionID)
	if err != nil {
		return errors.New("the exact active Claude transcript is unavailable; restart Claude after configuring the documented SessionStart hook")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	metadata := recovery.Metadata{
		Version: 1, Program: recovery.ProgramClaude, SessionID: strings.ToLower(registry.SessionID),
		CWD: registry.CWD, TranscriptPath: transcript, Source: "manual", Observed: now.UTC(),
	}
	return manager.PublishRecoveryMetadata(ctx, paneID, metadata)
}

type claudeRegistry struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	CWD       string `json:"cwd"`
	Version   string `json:"version"`
	ProcStart string `json:"procStart"`
}

func readClaudeRegistry(root string, pid int) (claudeRegistry, error) {
	if pid <= 0 || !filepath.IsAbs(root) {
		return claudeRegistry{}, errors.New("invalid Claude registry identity")
	}
	path := filepath.Join(filepath.Clean(root), "sessions", strconv.Itoa(pid)+".json")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > 64<<10 || !ownedByCurrentUser(info) {
		return claudeRegistry{}, errors.New("Claude registry is not an owned bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return claudeRegistry{}, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) || opened.Size() <= 0 || opened.Size() > 64<<10 || !ownedByCurrentUser(opened) {
		return claudeRegistry{}, errors.New("Claude registry identity changed")
	}
	decoder := json.NewDecoder(io.LimitReader(file, 64<<10))
	var registry claudeRegistry
	if err := decoder.Decode(&registry); err != nil {
		return claudeRegistry{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return claudeRegistry{}, errors.New("Claude registry has trailing data")
	}
	if registry.PID != pid || !filepath.IsAbs(registry.CWD) || len(registry.CWD) > 4096 || strings.ContainsRune(registry.CWD, '\x00') || len(registry.ProcStart) > 64 {
		return claudeRegistry{}, errors.New("Claude registry identity does not match")
	}
	return registry, nil
}

func findClaudeTranscript(root, sessionID string) (string, error) {
	if !filepath.IsAbs(root) || !recovery.ValidSessionID(sessionID) {
		return "", errors.New("invalid Claude transcript lookup")
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("Claude projects root is unavailable")
	}
	want := strings.ToLower(sessionID) + ".jsonl"
	var found string
	ambiguous := false
	entries := 0
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		entries++
		if entries > 100000 {
			return errors.New("Claude project store exceeds bounded scan")
		}
		if path != root && entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() || strings.ToLower(entry.Name()) != want || !entry.Type().IsRegular() {
			return nil
		}
		if found != "" {
			ambiguous = true
			return fs.SkipAll
		}
		info, err := entry.Info()
		if err != nil || !ownedByCurrentUser(info) {
			return errors.New("Claude transcript is not owned by the current user")
		}
		found = path
		return nil
	})
	if err != nil || found == "" || ambiguous {
		return "", errors.New("exact Claude transcript is unavailable or ambiguous")
	}
	return found, nil
}

func defaultClaudeConfigRoot() string {
	if configured := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); configured != "" {
		return config.ExpandPath(configured)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude")
}

func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Getuid())
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
		GuideConfigured: cfg.GuideConfigured(),
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
		homeLock, err := lockfile.Acquire(cfg.LockDir(), lockfile.Key("engram-home-state"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "home lock:", err)
			return 1
		}
		defer homeLock.Close()
		store, err := state.Open(cfg.StatePath(), cfg.AuditPath())
		if err != nil {
			fmt.Fprintln(os.Stderr, "state:", err)
			return 1
		}
		_ = store
		if _, err := templates.Open(cfg.TemplatePath()); err != nil {
			fmt.Fprintln(os.Stderr, "templates:", err)
			return 1
		}
	}
	fmt.Print(formatDiagnostics(cfg, mode, st, stateErr == nil, anchorMode, snapshotPath))
	return 0
}

func diagnosticsText(cfg config.Config, mode string) string {
	st, stateErr := readState(cfg.StatePath())
	snapshotPath, snapshotReady, _ := probeSnapshot(cfg)
	anchorMode, err := cfg.ResolveAnchorMode(st.AnchorMode, config.ModeCapabilities{
		GuideConfigured: cfg.GuideConfigured(),
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
	guideStatus := "unavailable"
	if cfg.GuideConfigured() {
		model = cfg.GuideModel()
		guideStatus = "configured, not probed"
	}
	voiceStatus := "path (local attachment)"
	if cfg.VoiceTranscriptionConfigured() {
		voiceStatus = "transcribe, configured but not probed (openai/" + cfg.OpenAITranscriptionModel + ")"
	}
	codexContextStatus := "disabled"
	if cfg.CodexContextTurns > 0 {
		codexContextStatus = fmt.Sprintf("enabled, %d recent visible turns max (exact active session only)", cfg.CodexContextTurns)
	}
	claudeContextStatus := "disabled"
	if cfg.ClaudeContextTurns > 0 {
		claudeContextStatus = fmt.Sprintf("enabled, %d recent visible turns max (exact active session only)", cfg.ClaudeContextTurns)
	}
	return fmt.Sprintf("Engram %s\nversion: %s\nenv: %s\nstate: %s (%s)\naudit: %s\nattachments: %s\nworkdir: %s\ntmux: %s\nanchor mode: %s\nguide: %s\ncodex context: %s\nclaude context: %s\nvoice input: %s\nsnapshots: %s\ntelegram user: %d\ntelegram chat: %d\nprovider: %s\nmodel: %s\nsessions: %d\nlast update: %d\nupdate journal: %d\ntelegram_api: not_called\nanthropic_api: not_called\nopenai_api: not_called\npolling: not_started\nstatus: ok\n",
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
		guideStatus,
		codexContextStatus,
		claudeContextStatus,
		voiceStatus,
		snapshotPath,
		cfg.TelegramAllowedUserID,
		cfg.TelegramChatID,
		cfg.EffectiveLLMProvider(),
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
