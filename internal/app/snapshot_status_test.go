package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/terminalshot"
)

func TestSnapshotFooterStatusSanitizesOneLineAndUsesPaneDirectory(t *testing.T) {
	dir := t.TempDir()
	runner := &recordingSnapshotFooterStatusRunner{output: "\x1b[32mdisk\x1b[0m\t47G\nfree \x1b]0;hidden\a now"}
	app := &App{
		Config:             config.Config{SnapshotStatusCommand: "local-status", Workdir: "/"},
		footerStatusRunner: runner,
	}
	input := app.withSnapshotFooterStatus(context.Background(), terminalshot.Input{}, dir)
	if input.Status != "disk 47G free now" {
		t.Fatalf("snapshot status = %q", input.Status)
	}
	if runner.command != "local-status" || runner.dir != dir {
		t.Fatalf("status invocation command=%q dir=%q", runner.command, runner.dir)
	}
	if runner.deadline <= 0 || runner.deadline > snapshotFooterStatusTimeout {
		t.Fatalf("status deadline = %v", runner.deadline)
	}
}

func TestSnapshotFooterStatusOmitsFailuresEmptyOutputAndSecrets(t *testing.T) {
	t.Parallel()
	secret := "configured-secret-value"
	for _, test := range []struct {
		name   string
		output string
		err    error
	}{
		{name: "failure", output: "partial", err: errors.New("failed")},
		{name: "empty", output: " \n\t"},
		{name: "configured secret", output: "disk " + secret},
		{name: "recognized secret", output: "token=public-looking-value"},
		{name: "redaction marker", output: "disk <redacted>"},
	} {
		t.Run(test.name, func(t *testing.T) {
			app := &App{
				Config:             config.Config{SnapshotStatusCommand: "local-status", OpenAIAPIKey: secret},
				footerStatusRunner: &recordingSnapshotFooterStatusRunner{output: test.output, err: test.err},
			}
			input := app.withSnapshotFooterStatus(context.Background(), terminalshot.Input{}, "/")
			if input.Status != "" {
				t.Fatalf("unsafe status retained: %q", input.Status)
			}
		})
	}
}

func TestSnapshotFooterStatusFallsBackToConfiguredWorkdirThenRoot(t *testing.T) {
	t.Parallel()
	workdir := t.TempDir()
	if got := snapshotFooterStatusDir(filepath.Join(workdir, "missing"), workdir); got != workdir {
		t.Fatalf("fallback directory = %q, want %q", got, workdir)
	}
	if got := snapshotFooterStatusDir("relative", filepath.Join(workdir, "missing")); got != "/" {
		t.Fatalf("root fallback = %q", got)
	}
	spaceDir := filepath.Join(workdir, "project ")
	if err := os.Mkdir(spaceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := snapshotFooterStatusDir(spaceDir); got != spaceDir {
		t.Fatalf("space-suffixed directory = %q, want %q", got, spaceDir)
	}
}

func TestShellSnapshotFooterStatusUsesBoundedSecretFreeEnvironment(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OPENAI_API_KEY", "must-not-reach-command")
	t.Setenv("TELEGRAM_BOT_TOKEN", "must-not-reach-command-either")
	output, err := (shellSnapshotFooterStatusRunner{}).Run(context.Background(), `pwd; env`, dir)
	if err != nil {
		t.Fatal(err)
	}
	canonicalDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(output, canonicalDir+"\n") {
		t.Fatalf("command did not run from pane directory: %q", output)
	}
	for _, forbidden := range []string{"OPENAI_API_KEY", "TELEGRAM_BOT_TOKEN", "must-not-reach-command"} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("minimal environment exposed %q: %q", forbidden, output)
		}
	}
}

func TestShellSnapshotFooterStatusKillsTimedOutPipeline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := (shellSnapshotFooterStatusRunner{}).Run(ctx, `sleep 5 | cat`, "/")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("timed-out pipeline returned after %v", elapsed)
	}
}

func TestShellSnapshotFooterStatusRejectsOversizedStdout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	_, err := (shellSnapshotFooterStatusRunner{}).Run(ctx, `while :; do printf 'xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx'; done`, "/")
	if !errors.Is(err, errSnapshotFooterStatusTooLarge) {
		t.Fatalf("oversized output error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("oversized producer returned after %v", elapsed)
	}
}

func TestShellSnapshotFooterStatusCleansBackgroundDescendantsAfterSuccess(t *testing.T) {
	output, err := (shellSnapshotFooterStatusRunner{}).Run(context.Background(), `sleep 30 >/dev/null 2>&1 & printf '%d\n' "$!"`, "/")
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(output))
	if err != nil {
		t.Fatalf("background pid output = %q: %v", output, err)
	}
	defer syscall.Kill(pid, syscall.SIGKILL)
	deadline := time.Now().Add(2 * time.Second)
	for processExists(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if processExists(pid) {
		t.Fatalf("background status descendant %d survived successful shell exit", pid)
	}
}

func TestGuidedCropHashIgnoresSnapshotFooterStatus(t *testing.T) {
	t.Parallel()
	input := terminalshot.Input{ANSI: "result", Columns: 71, VisibleRows: 37, BufferRows: 1}
	withoutStatus := guidedCropHash(input, "contrast-dark", guidedEvidenceTail)
	input.Status = "disk 47G free"
	if withStatus := guidedCropHash(input, "contrast-dark", guidedEvidenceTail); withStatus != withoutStatus {
		t.Fatalf("status changed terminal crop hash: %q != %q", withStatus, withoutStatus)
	}
}

func TestSnapshotFooterStatusIsDisabledWithoutACommand(t *testing.T) {
	t.Parallel()
	runner := &recordingSnapshotFooterStatusRunner{output: "disk 47G free"}
	input := (&App{footerStatusRunner: runner}).withSnapshotFooterStatus(context.Background(), terminalshot.Input{}, "/")
	if input.Status != "" || runner.calls != 0 {
		t.Fatalf("disabled status input=%#v calls=%d", input, runner.calls)
	}
}

type recordingSnapshotFooterStatusRunner struct {
	output   string
	err      error
	command  string
	dir      string
	deadline time.Duration
	calls    int
}

func (r *recordingSnapshotFooterStatusRunner) Run(ctx context.Context, command, dir string) (string, error) {
	r.calls++
	r.command = command
	r.dir = dir
	if deadline, ok := ctx.Deadline(); ok {
		r.deadline = time.Until(deadline)
	}
	return r.output, r.err
}

func TestSnapshotFooterStatusBufferBoundsMemory(t *testing.T) {
	t.Parallel()
	overflows := 0
	buffer := &snapshotFooterStatusBuffer{onOverflow: func() { overflows++ }}
	payload := strings.Repeat("x", snapshotFooterStatusMaxOutputBytes+100)
	if n, err := buffer.Write([]byte(payload)); !errors.Is(err, errSnapshotFooterStatusTooLarge) || n != snapshotFooterStatusMaxOutputBytes {
		t.Fatalf("bounded write n=%d err=%v", n, err)
	}
	if n, err := buffer.Write([]byte("again")); !errors.Is(err, errSnapshotFooterStatusTooLarge) || n != 0 {
		t.Fatalf("post-overflow write n=%d err=%v", n, err)
	}
	if !buffer.overflow || len(buffer.String()) != snapshotFooterStatusMaxOutputBytes || overflows != 1 {
		t.Fatalf("bounded buffer bytes=%d overflow=%v callbacks=%d", len(buffer.String()), buffer.overflow, overflows)
	}
}

func TestSanitizeSnapshotFooterStatusDropsInvalidUTF8AndControls(t *testing.T) {
	t.Parallel()
	input := string([]byte{'o', 'k', 0xff, 0x00, ' ', 0x9b}) + "31mnow " + string([]byte{0x9d}) + "private raw title" + string([]byte{0x9c}) + "safe " +
		"\u009b32mnext \x1bPprivate payload\x1b\\after \x1b]private title\u009cend"
	if got := sanitizeSnapshotFooterStatus(input); got != "ok now safe next after end" {
		t.Fatalf("sanitized status = %q", got)
	}
}

func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
