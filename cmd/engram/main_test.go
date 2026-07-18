package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/config"
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
