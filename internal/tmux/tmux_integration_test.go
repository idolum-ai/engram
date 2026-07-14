package tmux

import (
	"bytes"
	"context"
	"fmt"
	"io"
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

func (r socketRunner) RunToWriter(ctx context.Context, dst io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "tmux", append([]string{"-L", r.socket}, args...)...)
	var stderr bytes.Buffer
	cmd.Stdout = dst
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tmux: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func TestTmuxIntegrationCaptureStyledJoinsMarkerInNarrowRealPane(t *testing.T) {
	if os.Getenv("ENGRAM_TMUX_INTEGRATION") != "1" {
		t.Skip("set ENGRAM_TMUX_INTEGRATION=1 to run isolated real-tmux coverage")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	setIntegrationTmuxTmpDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	runner := socketRunner{socket: fmt.Sprintf("engram-test-%d", os.Getpid())}
	_, _ = runner.Run(context.Background(), "kill-server")
	t.Cleanup(func() { _, _ = runner.Run(context.Background(), "kill-server") })
	if _, err := runner.Run(ctx, "new-session", "-d", "-x", "12", "-y", "10", "-s", "signal", "cat"); err != nil {
		t.Fatal(err)
	}
	manager := New(runner)
	serverID, err := manager.EnsureServerID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	window, err := manager.ResolveTarget(ctx, "signal:0.0")
	if err != nil {
		t.Fatal(err)
	}
	if !validImmutableID(window.SessionID, '$') || !validImmutableID(window.ID, '@') || !validImmutableID(window.PaneID, '%') {
		t.Fatalf("resolved mutable or empty identity before use: %#v", window)
	}
	panes, err := manager.ListPanes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(panes) != 1 || panes[0].SessionID != window.SessionID || panes[0].WindowID != window.ID || panes[0].ID != window.PaneID {
		t.Fatalf("pane identity does not match target: window=%#v panes=%#v", window, panes)
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
	literal, err := manager.CaptureLiteral(ctx, window.PaneID, window.ID, serverID, 64)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(literal, "\x1b") || !strings.Contains(strings.ReplaceAll(literal, "\n", ""), record) {
		t.Fatalf("literal capture = %q", literal)
	}
	raw, err := manager.CaptureVisibleRaw(ctx, window.PaneID, window.ID, serverID)
	if err != nil || !strings.Contains(strings.ReplaceAll(raw, "\n", ""), record) {
		t.Fatalf("binding-guarded raw capture=%q error=%v", raw, err)
	}
	var dump strings.Builder
	if err := manager.DumpScrollback(ctx, window.PaneID, window.ID, serverID, &dump); err != nil || !strings.Contains(strings.ReplaceAll(dump.String(), "\n", ""), record) {
		t.Fatalf("binding-guarded scrollback capture=%q error=%v", dump.String(), err)
	}
	if buffers, err := runner.Run(ctx, "list-buffers", "-F", "#{buffer_name}"); err == nil && strings.Contains(buffers, "engram-") {
		t.Fatalf("capture buffers leaked: %q", buffers)
	}
	validated, err := manager.ValidateBinding(ctx, window.PaneID, window.ID, serverID)
	if err != nil || validated.ID != window.PaneID || validated.WindowID != window.ID {
		t.Fatalf("single-call binding validation: pane=%#v error=%v", validated, err)
	}
	if err := manager.SendCommandIfBindingMatches(ctx, window.PaneID, window.ID, serverID, "atomic input"); err != nil {
		t.Fatalf("binding-guarded input: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	atomicCapture, err := manager.CaptureLiteral(ctx, window.PaneID, window.ID, serverID, 64)
	if err != nil || !strings.Contains(atomicCapture, "atomic input") {
		t.Fatalf("binding-guarded input capture=%q error=%v", atomicCapture, err)
	}
	if err := manager.SendTextIfBindingMatches(ctx, window.PaneID, "@999", serverID, "must not route"); err == nil || !IsIdentityLoss(err) {
		t.Fatalf("binding-guarded input accepted wrong window: %v", err)
	}
	if buffers, err := runner.Run(ctx, "list-buffers", "-F", "#{buffer_name}"); err == nil && strings.Contains(buffers, "engram-input-") {
		t.Fatalf("conditional input buffer leaked: %q", buffers)
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

func TestTmuxIntegrationMetadataFramingPreservesComplexValues(t *testing.T) {
	if os.Getenv("ENGRAM_TMUX_INTEGRATION") != "1" {
		t.Skip("set ENGRAM_TMUX_INTEGRATION=1 to run isolated real-tmux coverage")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	setIntegrationTmuxTmpDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	runner := socketRunner{socket: fmt.Sprintf("engram-metadata-test-%d", os.Getpid())}
	_, _ = runner.Run(context.Background(), "kill-server")
	t.Cleanup(func() { _, _ = runner.Run(context.Background(), "kill-server") })
	workdir := t.TempDir() + "/path_under_score_雪\nline"
	if err := os.Mkdir(workdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(ctx, "new-session", "-d", "-s", "meta_under_score", "-c", workdir, "cat"); err != nil {
		t.Fatal(err)
	}
	windowName := "build: under_score 雪"
	if _, err := runner.Run(ctx, "rename-window", "-t", "meta_under_score:0", windowName); err != nil {
		t.Fatal(err)
	}
	manager := New(runner)
	window, err := manager.ResolveTarget(ctx, "meta_under_score:0.0")
	if err != nil {
		t.Fatal(err)
	}
	if window.SessionName != "meta_under_score" || window.Name != windowName || window.CurrentPath != workdir || !validImmutableID(window.SessionID, '$') || !validImmutableID(window.ID, '@') || !validImmutableID(window.PaneID, '%') {
		t.Fatalf("complex metadata = %#v", window)
	}
}

func TestTmuxIntegrationSessionNamesResolveExactlyBeforeNewWindow(t *testing.T) {
	if os.Getenv("ENGRAM_TMUX_INTEGRATION") != "1" {
		t.Skip("set ENGRAM_TMUX_INTEGRATION=1 to run isolated real-tmux coverage")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	setIntegrationTmuxTmpDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	runner := socketRunner{socket: fmt.Sprintf("engram-numeric-test-%d", os.Getpid())}
	_, _ = runner.Run(context.Background(), "kill-server")
	t.Cleanup(func() { _, _ = runner.Run(context.Background(), "kill-server") })
	if _, err := runner.Run(ctx, "new-session", "-d", "-s", "01", "cat"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"foo", "=foo", "$0"} {
		if _, err := runner.Run(ctx, "new-session", "-d", "-s", name, "cat"); err != nil {
			t.Fatal(err)
		}
	}
	manager := New(runner)
	for _, name := range []string{"0", "=foo", "$0"} {
		sessionID, err := manager.EnsureSession(ctx, name, t.TempDir())
		if err != nil {
			t.Fatalf("ensure session %q: %v", name, err)
		}
		_, paneID, err := manager.NewWindow(ctx, sessionID, t.TempDir(), "engram-exact")
		if err != nil {
			t.Fatalf("new window in %q: %v", name, err)
		}
		pane, err := manager.InspectPane(ctx, paneID)
		if err != nil {
			t.Fatal(err)
		}
		if pane.SessionName != name {
			t.Fatalf("new pane session = %q, want exact session %q", pane.SessionName, name)
		}
	}
}

func setIntegrationTmuxTmpDir(t *testing.T) {
	t.Helper()
	dir, err := os.MkdirTemp("", "et-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("TMUX_TMPDIR", dir)
}
