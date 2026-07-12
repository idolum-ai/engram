package tmux

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

type socketRunner struct{ socket string }

func (r socketRunner) Run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "tmux", append([]string{"-L", r.socket}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("tmux: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func TestCaptureStyledJoinsMarkerInNarrowRealPane(t *testing.T) {
	if os.Getenv("ENGRAM_TMUX_INTEGRATION") != "1" {
		t.Skip("set ENGRAM_TMUX_INTEGRATION=1 to run isolated real-tmux coverage")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	runner := socketRunner{socket: fmt.Sprintf("engram-test-%d", os.Getpid())}
	_, _ = runner.Run(context.Background(), "kill-server")
	t.Cleanup(func() { _, _ = runner.Run(context.Background(), "kill-server") })
	if _, err := runner.Run(ctx, "new-session", "-d", "-x", "12", "-y", "10", "-s", "signal", "cat"); err != nil {
		t.Fatal(err)
	}
	manager := New(runner)
	window, err := manager.ResolveTarget(ctx, "signal:0.0")
	if err != nil {
		t.Fatal(err)
	}
	record := "[engram:upstream] 0123456789abcdef0123456789abcdef narrow works"
	if err := manager.SendText(ctx, window.PaneID, record); err != nil {
		t.Fatal(err)
	}
	if err := manager.SendKeys(ctx, window.PaneID, []string{"Enter"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	capture, err := manager.CaptureStyled(ctx, window.PaneID, 64)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(capture.Text, record) || !strings.Contains(capture.JoinedText, record) {
		t.Fatalf("physical=%q joined=%q", capture.Text, capture.JoinedText)
	}
	literal, err := manager.CaptureLiteral(ctx, window.PaneID, 64)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(literal, "\x1b") || !strings.Contains(strings.ReplaceAll(literal, "\n", ""), record) {
		t.Fatalf("literal capture = %q", literal)
	}
	if buffers, err := runner.Run(ctx, "list-buffers", "-F", "#{buffer_name}"); err == nil && strings.Contains(buffers, "engram-") {
		t.Fatalf("capture buffers leaked: %q", buffers)
	}
	serverID, err := manager.EnsureServerID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.KillWindowIfBindingMatches(ctx, window.PaneID, "@999", serverID); err == nil || !IsIdentityLoss(err) {
		t.Fatalf("atomic close accepted wrong window: %v", err)
	}
	if _, err := manager.InspectPane(ctx, window.PaneID); err != nil {
		t.Fatalf("mismatched close changed pane: %v", err)
	}
	if err := manager.KillWindowIfBindingMatches(ctx, window.PaneID, window.ID, serverID); err != nil {
		t.Fatalf("atomic close of matching window: %v", err)
	}
}

func TestNewWindowTargetsNumericSessionName(t *testing.T) {
	if os.Getenv("ENGRAM_TMUX_INTEGRATION") != "1" {
		t.Skip("set ENGRAM_TMUX_INTEGRATION=1 to run isolated real-tmux coverage")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	runner := socketRunner{socket: fmt.Sprintf("engram-numeric-test-%d", os.Getpid())}
	_, _ = runner.Run(context.Background(), "kill-server")
	t.Cleanup(func() { _, _ = runner.Run(context.Background(), "kill-server") })
	if _, err := runner.Run(ctx, "new-session", "-d", "-s", "0", "cat"); err != nil {
		t.Fatal(err)
	}
	manager := New(runner)
	_, paneID, err := manager.NewWindow(ctx, "0", t.TempDir(), "engram-numeric")
	if err != nil {
		t.Fatal(err)
	}
	pane, err := manager.InspectPane(ctx, paneID)
	if err != nil {
		t.Fatal(err)
	}
	if pane.SessionName != "0" {
		t.Fatalf("new pane session = %q, want numeric session 0", pane.SessionName)
	}
}
