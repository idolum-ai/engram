package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/claudeui"
	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/lockfile"
	"github.com/idolum-ai/engram/internal/recovery"
	"github.com/idolum-ai/engram/internal/tmux"
)

type signalWriteCloser struct {
	bytes.Buffer
	closeErr error
}

type hookRunner struct{ calls [][]string }

func (r *hookRunner) Run(_ context.Context, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	return "", nil
}

type claudeBindRunner struct{ calls [][]string }

func (r *claudeBindRunner) Run(_ context.Context, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	if len(args) > 0 && args[0] == "display-message" {
		return "3:1007:2.1.223\n", nil
	}
	return "", nil
}

type bindProcessRunner struct{}

func (bindProcessRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	if name == "ps" && len(args) > 0 && args[0] == "-axo" {
		return "100 1 bash -bash\n110 100 2.1.223 /Users/example/.local/share/claude/versions/2.1.223\n", nil
	}
	return "", errors.New("unexpected process command")
}

type bindExecutableResolver struct{ path string }

func (r bindExecutableResolver) Resolve(int) (string, error) { return r.path, nil }

type bindVersionResolver struct{ version string }

func (r bindVersionResolver) Resolve(string) (string, error) { return r.version, nil }

type bindStartResolver struct{ at time.Time }

func (r bindStartResolver) Resolve(int) (time.Time, string, error) { return r.at, "fixture-start", nil }

func TestHelpExplainsLocalSurfaces(t *testing.T) {
	stdout, stderr, code := captureCommand(t, func() int {
		return run([]string{"help"})
	})
	if code != 0 || stderr != "" {
		t.Fatalf("help code=%d stderr=%q", code, stderr)
	}
	for _, want := range []string{
		"Service:",
		"Local inspection:",
		"Integration:",
		"Reference:",
		"Validate configuration without network calls or writes",
		"engram commands [--format json|markdown]",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("help missing %q:\n%s", want, stdout)
		}
	}
}

func TestCommandsSupportsJSONAndMarkdown(t *testing.T) {
	for _, test := range []struct {
		args []string
		want string
	}{
		{args: []string{"commands"}, want: `"command": "help"`},
		{args: []string{"commands", "--format", "markdown"}, want: "# Telegram Command Reference"},
	} {
		stdout, stderr, code := captureCommand(t, func() int { return run(test.args) })
		if code != 0 || stderr != "" || !strings.Contains(stdout, test.want) {
			t.Fatalf("run(%v) code=%d stderr=%q stdout=%q", test.args, code, stderr, stdout)
		}
	}

	_, stderr, code := captureCommand(t, func() int {
		return run([]string{"commands", "--format", "xml"})
	})
	if code != 2 || !strings.Contains(stderr, "--format must be json or markdown") {
		t.Fatalf("invalid format code=%d stderr=%q", code, stderr)
	}
}

func TestCodexHookPublishesExactSessionToInheritedPane(t *testing.T) {
	runner := &hookRunner{}
	input := strings.NewReader(`{"session_id":"019f7607-c8b0-74b3-87ca-64a7e6e7ede0","cwd":"/work","hook_event_name":"SessionStart","source":"resume"}`)
	err := runCodexHook(input, "%7", tmux.New(runner), time.Date(2026, 7, 18, 21, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 || len(runner.calls[0]) != 7 || runner.calls[0][0] != "set-option" || runner.calls[0][4] != "%7" || !strings.Contains(runner.calls[0][6], "019f7607-c8b0-74b3-87ca-64a7e6e7ede0") {
		t.Fatalf("calls = %#v", runner.calls)
	}
	if err := runCodexHook(strings.NewReader(`{}`), "", tmux.New(runner), time.Time{}); err == nil {
		t.Fatal("invalid hook input was accepted")
	}
}

func TestClaudeHookPublishesExactSessionAndTranscriptToInheritedPane(t *testing.T) {
	runner := &hookRunner{}
	input := strings.NewReader(`{"session_id":"019f7607-c8b0-74b3-87ca-64a7e6e7ede0","transcript_path":"/Users/example/.claude/projects/-work/019f7607-c8b0-74b3-87ca-64a7e6e7ede0.jsonl","cwd":"/work","hook_event_name":"SessionStart","source":"resume"}`)
	err := runClaudeHook(input, "%7", tmux.New(runner), time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 || len(runner.calls[0]) != 7 || runner.calls[0][4] != "%7" {
		t.Fatalf("calls = %#v", runner.calls)
	}
	metadata, err := recovery.Decode(runner.calls[0][6])
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Program != recovery.ProgramClaude || metadata.TranscriptPath == "" || metadata.Source != "resume" {
		t.Fatalf("metadata = %#v", metadata)
	}
}

func TestClaudeBindProvesProcessRegistryAndExactTranscript(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "projects", "-work"), 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	started := now.Add(-time.Minute)
	registry := fmt.Sprintf(`{"pid":110,"sessionId":"019f7607-c8b0-74b3-87ca-64a7e6e7ede0","cwd":"/work","version":"2.1.223","procStart":%q}`, started.In(time.Local).Format("Mon Jan _2 15:04:05 2006"))
	if err := os.WriteFile(filepath.Join(root, "sessions", "110.json"), []byte(registry), 0o600); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(root, "projects", "-work", "019f7607-c8b0-74b3-87ca-64a7e6e7ede0.jsonl")
	if err := os.WriteFile(transcript, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "share", "claude", "versions", "2.1.223")
	runner := &claudeBindRunner{}
	detector := &claudeui.Detector{
		Runner: bindProcessRunner{}, Executables: bindExecutableResolver{path: executable},
		Versions: bindVersionResolver{version: "2.1.223"}, Starts: bindStartResolver{at: started},
	}
	if err := runClaudeBind("%7", root, tmux.New(runner), detector, now); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 || runner.calls[1][0] != "set-option" {
		t.Fatalf("calls = %#v", runner.calls)
	}
	metadata, err := recovery.Decode(runner.calls[1][6])
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Program != recovery.ProgramClaude || metadata.TranscriptPath != transcript || metadata.Source != "manual" {
		t.Fatalf("metadata = %#v", metadata)
	}

	wrongStart := fmt.Sprintf(`{"pid":110,"sessionId":"019f7607-c8b0-74b3-87ca-64a7e6e7ede0","cwd":"/work","version":"2.1.223","procStart":%q}`, started.Add(-time.Hour).In(time.Local).Format("Mon Jan _2 15:04:05 2006"))
	if err := os.WriteFile(filepath.Join(root, "sessions", "110.json"), []byte(wrongStart), 0o600); err != nil {
		t.Fatal(err)
	}
	before := len(runner.calls)
	if err := runClaudeBind("%7", root, tmux.New(runner), detector, now); err == nil {
		t.Fatal("registry from a different process incarnation was accepted")
	}
	for _, call := range runner.calls[before:] {
		if len(call) > 0 && call[0] == "set-option" {
			t.Fatalf("invalid registry reached tmux: %#v", call)
		}
	}
}

func TestCodexBindPublishesInheritedIdentityWithoutUserArguments(t *testing.T) {
	runner := &hookRunner{}
	now := time.Date(2026, 8, 3, 6, 30, 0, 0, time.UTC)
	err := runCodexBind("019f7607-c8b0-74b3-87ca-64a7e6e7ede0", "%7", "/work", tmux.New(runner), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 || len(runner.calls[0]) != 7 || runner.calls[0][0] != "set-option" || runner.calls[0][4] != "%7" {
		t.Fatalf("calls = %#v", runner.calls)
	}
	metadata, err := recovery.Decode(runner.calls[0][6])
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Program != recovery.ProgramCodex || metadata.SessionID != "019f7607-c8b0-74b3-87ca-64a7e6e7ede0" || metadata.CWD != "/work" || metadata.Source != "manual" || !metadata.Observed.Equal(now) {
		t.Fatalf("metadata = %#v", metadata)
	}

	for _, test := range []struct {
		name      string
		sessionID string
		paneID    string
		want      string
	}{
		{name: "missing session", paneID: "%7", want: "CODEX_THREAD_ID"},
		{name: "invalid session", sessionID: "not-a-uuid", paneID: "%7", want: "CODEX_THREAD_ID"},
		{name: "missing pane", sessionID: "019f7607-c8b0-74b3-87ca-64a7e6e7ede0", want: "TMUX_PANE"},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := len(runner.calls)
			err := runCodexBind(test.sessionID, test.paneID, "/work", tmux.New(runner), now)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("runCodexBind() error = %v, want %q", err, test.want)
			}
			if len(runner.calls) != before {
				t.Fatal("invalid binding reached tmux")
			}
		})
	}
}

func TestCodexBindRejectsArgumentsBeforeInspectingEnvironment(t *testing.T) {
	_, stderr, code := captureCommand(t, func() int {
		return run([]string{"codex-bind", "unexpected"})
	})
	if code != 2 || !strings.Contains(stderr, "usage: engram codex-bind") {
		t.Fatalf("codex-bind code=%d stderr=%q", code, stderr)
	}
}

func (w *signalWriteCloser) Close() error { return w.closeErr }

func TestEmitSignalUsesOnlyControllingTerminal(t *testing.T) {
	tty := &signalWriteCloser{}
	err := emitSignal("tests\nfinished", func() (io.WriteCloser, error) { return tty, nil })
	if err != nil {
		t.Fatal(err)
	}
	got := tty.String()
	if !strings.HasPrefix(got, "\a\r\n[engram:upstream:v1] ") || !strings.HasSuffix(got, " 14:tests finished\r\n") {
		t.Fatalf("signal = %q", got)
	}

	openErr := errors.New("no controlling terminal")
	if err := emitSignal("ignored", func() (io.WriteCloser, error) { return nil, openErr }); !errors.Is(err, openErr) {
		t.Fatalf("open error = %v", err)
	}
}

func TestSignalStdoutEmitsRecordForRelayingTerminalHosts(t *testing.T) {
	stdout, stderr, code := captureCommand(t, func() int {
		return run([]string{"signal", "--stdout", "tests", "finished"})
	})
	if code != 0 || stderr != "" {
		t.Fatalf("signal --stdout code=%d stderr=%q", code, stderr)
	}
	if !strings.HasPrefix(stdout, "\a\r\n[engram:upstream:v1] ") || !strings.HasSuffix(stdout, " 14:tests finished\r\n") {
		t.Fatalf("signal --stdout = %q", stdout)
	}
}

func TestPreflightDoesNotCallTelegramOrGuideProvider(t *testing.T) {
	env := writeTestEnv(t)
	stdout, stderr, code := captureCommand(t, func() int {
		return run([]string{"preflight", "--env", env})
	})
	if code != 0 {
		t.Fatalf("preflight code=%d stderr=%s", code, stderr)
	}
	for _, want := range []string{
		"Engram preflight",
		"telegram_api: not_called",
		"anthropic_api: not_called",
		"openai_api: not_called",
		"polling: not_started",
		"status: ok",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("preflight output missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "tg-secret-token") || strings.Contains(stdout, "anthropic-secret-key") {
		t.Fatalf("preflight leaked secret:\n%s", stdout)
	}
}

func TestRunRejectsMissingTmuxBeforeStarting(t *testing.T) {
	env := writeTestEnv(t)
	t.Setenv("PATH", t.TempDir())
	_, stderr, code := captureCommand(t, func() int {
		return run([]string{"run", "--env", env})
	})
	if code != 1 || !strings.Contains(stderr, "tmux executable not found in PATH") {
		t.Fatalf("run code=%d stderr=%q", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(env), "home")); !os.IsNotExist(err) {
		t.Fatalf("run touched application state before rejecting tmux: %v", err)
	}
}

func TestDryStartCreatesStateWithoutPolling(t *testing.T) {
	env := writeTestEnv(t)
	stdout, stderr, code := captureCommand(t, func() int {
		return run([]string{"dry-start", "--env", env})
	})
	if code != 0 {
		t.Fatalf("dry-start code=%d stderr=%s", code, stderr)
	}
	for _, want := range []string{"Engram dry-start", "polling: not_started", "status: ok"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("dry-start output missing %q:\n%s", want, stdout)
		}
	}
}

func TestDryStartRejectsFutureTemplateSchema(t *testing.T) {
	env := writeTestEnv(t)
	home := filepath.Join(filepath.Dir(env), "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "templates.json")
	data := []byte(`{"version":2,"templates":{"future":"shape"}}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := captureCommand(t, func() int {
		return run([]string{"dry-start", "--env", env})
	})
	if code != 1 || !strings.Contains(stderr, "templates:") || !strings.Contains(stderr, "newer") {
		t.Fatalf("dry-start code=%d stderr=%q", code, stderr)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatal("dry-start modified future template state")
	}
}

func TestDryStartRejectsSharedHomeWhileWriterLockIsHeld(t *testing.T) {
	env := writeTestEnv(t)
	cfg, err := config.Load(env)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.EnsureDirs(cfg); err != nil {
		t.Fatal(err)
	}
	lock, err := lockfile.Acquire(cfg.LockDir(), lockfile.Key("engram-home-state"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	_, stderr, code := captureCommand(t, func() int {
		return run([]string{"dry-start", "--env", env})
	})
	if code != 1 || !strings.Contains(stderr, "home lock:") || !strings.Contains(stderr, "another Engram process") {
		t.Fatalf("dry-start code=%d stderr=%q", code, stderr)
	}
}

func TestSnapshotPreflightRejectsNonBrowserExecutable(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	body := strings.Join([]string{
		"TELEGRAM_BOT_TOKEN=tg-secret-token",
		"TELEGRAM_ALLOWED_USER_ID=123",
		"ENGRAM_ANCHOR_MODE=snapshot",
		"ENGRAM_SNAPSHOT_BROWSER=" + executable,
		"ENGRAM_HOME=" + filepath.Join(dir, "home"),
		"ENGRAM_WORKDIR=" + dir,
	}, "\n")
	if err := os.WriteFile(env, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := captureCommand(t, func() int {
		return run([]string{"preflight", "--env", env})
	})
	if code != 1 || !strings.Contains(stderr, "snapshot probe:") {
		t.Fatalf("snapshot preflight code=%d stderr=%s", code, stderr)
	}
	diagnostics := diagnosticsText(config.Config{AnchorMode: config.AnchorModeSnapshot, SnapshotBrowser: executable, SnapshotTheme: "terminal"}, "preflight")
	for _, want := range []string{"anchor mode: unavailable", "guide: unavailable", "model: unavailable", "anthropic_api: not_called", "openai_api: not_called"} {
		if !strings.Contains(diagnostics, want) {
			t.Fatalf("snapshot diagnostics missing %q:\n%s", want, diagnostics)
		}
	}
}

func TestPreflightUsesPersistedModeBeforeEnvironmentFallback(t *testing.T) {
	env := writeTestEnv(t)
	cfg, err := config.Load(env)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	stateJSON := `{"version":6,"anchor_mode":"guide","next_session_id":1,"terminal_sessions":[],"attachments":[]}`
	if err := os.WriteFile(cfg.StatePath(), []byte(stateJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	withSnapshotMode := strings.ReplaceAll(string(mustRead(t, env)), "ENGRAM_HOME=", "ENGRAM_ANCHOR_MODE=snapshot\nENGRAM_SNAPSHOT_BROWSER=/missing/chromium\nENGRAM_HOME=")
	if err := os.WriteFile(env, []byte(withSnapshotMode), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := captureCommand(t, func() int {
		return run([]string{"preflight", "--env", env})
	})
	if code != 0 || !strings.Contains(stdout, "anchor mode: guide") {
		t.Fatalf("preflight code=%d stderr=%s stdout=%s", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "guide: configured, not probed") {
		t.Fatalf("preflight guide status was not honest:\n%s", stdout)
	}
}

func TestPreflightRecognizesOpenAILunaWithoutCallingIt(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	body := strings.Join([]string{
		"TELEGRAM_BOT_TOKEN=tg-secret-token",
		"TELEGRAM_ALLOWED_USER_ID=123",
		"LLM_PROVIDER=openai",
		"OPENAI_API_KEY=openai-secret-key",
		"OPENAI_MODEL=gpt-5.6-luna",
		"ENGRAM_CODEX_CONTEXT_TURNS=3",
		"ENGRAM_CLAUDE_CONTEXT_TURNS=2",
		"ENGRAM_HOME=" + filepath.Join(dir, "home"),
		"ENGRAM_WORKDIR=" + dir,
	}, "\n")
	if err := os.WriteFile(env, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := captureCommand(t, func() int {
		return run([]string{"preflight", "--env", env})
	})
	if code != 0 || stderr != "" {
		t.Fatalf("preflight code=%d stderr=%s", code, stderr)
	}
	for _, want := range []string{
		"guide: configured, not probed",
		"codex context: enabled, 3 recent visible turns max (exact active session only)",
		"claude context: enabled, 2 recent visible turns max (exact active session only)",
		"provider: openai",
		"model: gpt-5.6-luna",
		"openai_api: not_called",
		"status: ok",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("preflight output missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "openai-secret-key") {
		t.Fatalf("preflight leaked OpenAI key:\n%s", stdout)
	}
}

func TestInspectSessionsNeedsNoTelegramConfiguration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ENGRAM_HOME", home)
	stateJSON := `{"version":7,"next_session_id":2,"terminal_sessions":[{"id":1,"tmux_session_name":"work","tmux_window_id":"@2","tmux_pane_id":"%3","origin":"attached","title":"build","last_known_cwd":"/tmp/project","state":"running"}],"attachments":[]}`
	if err := os.WriteFile(filepath.Join(home, "state.json"), []byte(stateJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := captureCommand(t, func() int {
		return run([]string{"inspect", "sessions"})
	})
	if code != 0 || stderr != "" || !strings.Contains(stdout, "[1] state=running origin=attached pane=%3 window=@2") {
		t.Fatalf("inspect code=%d stderr=%q stdout=%q", code, stderr, stdout)
	}
}

func TestInspectRejectsMissingSubcommandWithoutLoadingConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ENGRAM_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "state.json"), []byte(`{"version":7}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := captureCommand(t, func() int {
		return run([]string{"inspect"})
	})
	if code != 2 || !strings.Contains(stderr, "usage: engram inspect") {
		t.Fatalf("inspect code=%d stderr=%q", code, stderr)
	}
}

func writeTestEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	home := filepath.Join(dir, "home")
	body := strings.Join([]string{
		"TELEGRAM_BOT_TOKEN=tg-secret-token",
		"TELEGRAM_ALLOWED_USER_ID=123",
		"ANTHROPIC_API_KEY=anthropic-secret-key",
		"ENGRAM_HOME=" + home,
		"ENGRAM_WORKDIR=" + dir,
	}, "\n")
	if err := os.WriteFile(env, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return env
}

func TestAgentDoctorRejectsExplicitlyMissingEnvFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "mistyped.env")
	out, stderr, code := captureCommand(t, func() int {
		return run([]string{"doctor", "agent", "--env", missing})
	})
	if code != 1 || out != "" || !strings.Contains(stderr, "doctor config:") || !strings.Contains(stderr, "mistyped.env") {
		t.Fatalf("explicit missing env: code=%d stdout=%q stderr=%q", code, out, stderr)
	}
}

func captureCommand(t *testing.T, fn func() int) (string, string, int) {
	t.Helper()
	oldOut := os.Stdout
	oldErr := os.Stderr
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = outW
	os.Stderr = errW
	code := fn()
	_ = outW.Close()
	_ = errW.Close()
	os.Stdout = oldOut
	os.Stderr = oldErr
	var outBuf bytes.Buffer
	var errBuf bytes.Buffer
	_, _ = io.Copy(&outBuf, outR)
	_, _ = io.Copy(&errBuf, errR)
	return outBuf.String(), errBuf.String(), code
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
