package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreflightDoesNotCallTelegramOrAnthropic(t *testing.T) {
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

func TestSnapshotPreflightDoesNotRequireAnthropic(t *testing.T) {
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
	stdout, stderr, code := captureCommand(t, func() int {
		return run([]string{"preflight", "--env", env})
	})
	if code != 0 {
		t.Fatalf("snapshot preflight code=%d stderr=%s", code, stderr)
	}
	for _, want := range []string{"anchor mode: snapshot", "model: disabled", "anthropic_api: not_called", "status: ok"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("snapshot preflight output missing %q:\n%s", want, stdout)
		}
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
