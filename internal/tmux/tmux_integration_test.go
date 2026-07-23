package tmux

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/keyseq"
)

type socketRunner struct{ socket string }

func TestTmuxIntegrationCompiledKeyPlanPreservesOrderAndRepetition(t *testing.T) {
	if os.Getenv("ENGRAM_TMUX_INTEGRATION") != "1" {
		t.Skip("set ENGRAM_TMUX_INTEGRATION=1 to run isolated real-tmux coverage")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	setIntegrationTmuxEnvironment(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	runner := socketRunner{socket: fmt.Sprintf("engram-keys-test-%d", os.Getpid())}
	_, _ = runner.Run(context.Background(), "kill-server")
	t.Cleanup(func() { _, _ = runner.Run(context.Background(), "kill-server") })

	outputPath := filepath.Join(t.TempDir(), "keys.bin")
	if _, err := runner.Run(ctx, "new-session", "-d", "-x", "40", "-y", "10", "-s", "keys", "cat > "+ShellQuote(outputPath)); err != nil {
		t.Fatal(err)
	}
	manager := New(runner)
	serverID, err := manager.EnsureServerID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	window, err := manager.ResolveTarget(ctx, "keys:")
	if err != nil {
		t.Fatal(err)
	}
	proposal := keyseq.Proposal{Kind: keyseq.KindSequence, Events: []keyseq.Event{
		{Key: keyseq.KeyUp, Count: 3},
		{Key: keyseq.KeyEnter, Count: 1},
	}}
	plan, err := keyseq.Compile(proposal)
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range plan.Groups {
		if err := manager.SendKeysIfBindingMatches(ctx, window.PaneID, window.ID, serverID, group.Keys); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		got, readErr := os.ReadFile(outputPath)
		if readErr == nil && bytes.Equal(got, []byte("\x1b[A\x1b[A\x1b[A\n")) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("compiled key bytes = %x error=%v", got, readErr)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestTmuxIntegrationVerifiedChordSubsetEmitsExactBytes(t *testing.T) {
	if os.Getenv("ENGRAM_TMUX_INTEGRATION") != "1" {
		t.Skip("set ENGRAM_TMUX_INTEGRATION=1 to run isolated real-tmux coverage")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	setIntegrationTmuxEnvironment(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	runner := socketRunner{socket: fmt.Sprintf("engram-chords-test-%d", os.Getpid())}
	_, _ = runner.Run(context.Background(), "kill-server")
	t.Cleanup(func() { _, _ = runner.Run(context.Background(), "kill-server") })

	outputPath := filepath.Join(t.TempDir(), "chords.bin")
	command := "stty raw -echo; cat > " + ShellQuote(outputPath)
	if _, err := runner.Run(ctx, "new-session", "-d", "-x", "40", "-y", "10", "-s", "chords", command); err != nil {
		t.Fatal(err)
	}
	manager := New(runner)
	serverID, err := manager.EnsureServerID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	window, err := manager.ResolveTarget(ctx, "chords:")
	if err != nil {
		t.Fatal(err)
	}
	proposal := keyseq.Proposal{Kind: keyseq.KindSequence, Events: []keyseq.Event{
		{Key: "a", Modifiers: []keyseq.Modifier{keyseq.ModifierShift}, Count: 1},
		{Key: "c", Modifiers: []keyseq.Modifier{keyseq.ModifierControl}, Count: 1},
		{Key: "a", Modifiers: []keyseq.Modifier{keyseq.ModifierAlt}, Count: 1},
		{Key: keyseq.KeyTab, Modifiers: []keyseq.Modifier{keyseq.ModifierShift}, Count: 1},
		{Key: keyseq.KeyF4, Modifiers: []keyseq.Modifier{keyseq.ModifierAlt}, Count: 1},
	}}
	plan, err := keyseq.Compile(proposal)
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range plan.Groups {
		if err := manager.SendKeysIfBindingMatches(ctx, window.PaneID, window.ID, serverID, group.Keys); err != nil {
			t.Fatal(err)
		}
	}
	want := []byte("A\x03\x1ba\x1b[Z\x1b[1;3S")
	deadline := time.Now().Add(2 * time.Second)
	for {
		got, readErr := os.ReadFile(outputPath)
		if readErr == nil && bytes.Equal(got, want) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("verified chord bytes = %x, want %x, error=%v", got, want, readErr)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func (r socketRunner) Run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "tmux", integrationTmuxArgs(r.socket, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("tmux: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func (r socketRunner) RunToWriter(ctx context.Context, dst io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "tmux", integrationTmuxArgs(r.socket, args...)...)
	var stderr bytes.Buffer
	cmd.Stdout = dst
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tmux: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func integrationTmuxArgs(socket string, args ...string) []string {
	out := []string{"-f", os.DevNull, "-L", socket}
	return append(out, args...)
}

func TestTmuxIntegrationCaptureStyledJoinsMarkerInNarrowRealPane(t *testing.T) {
	if os.Getenv("ENGRAM_TMUX_INTEGRATION") != "1" {
		t.Skip("set ENGRAM_TMUX_INTEGRATION=1 to run isolated real-tmux coverage")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	setIntegrationTmuxEnvironment(t)
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

func TestTmuxIntegrationTallWrappedCaptureKeepsPresentationsAligned(t *testing.T) {
	if os.Getenv("ENGRAM_TMUX_INTEGRATION") != "1" {
		t.Skip("set ENGRAM_TMUX_INTEGRATION=1 to run isolated real-tmux coverage")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	setIntegrationTmuxEnvironment(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	runner := socketRunner{socket: fmt.Sprintf("engram-frame-test-%d", os.Getpid())}
	_, _ = runner.Run(context.Background(), "kill-server")
	t.Cleanup(func() { _, _ = runner.Run(context.Background(), "kill-server") })

	inputPath := filepath.Join(t.TempDir(), "frame.txt")
	var input strings.Builder
	for i := 0; i < 50; i++ {
		fmt.Fprintf(&input, "logical-line-%02d-xxxx\n", i)
	}
	if err := os.WriteFile(inputPath, []byte(input.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(ctx, "new-session", "-d", "-x", "12", "-y", "100", "-s", "frame", "cat"); err != nil {
		t.Fatal(err)
	}
	manager := New(runner)
	if _, err := manager.EnsureServerID(ctx); err != nil {
		t.Fatal(err)
	}
	window, err := manager.ResolveTarget(ctx, "frame:")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(ctx, "load-buffer", "-b", "frame-input", inputPath, ";", "paste-buffer", "-b", "frame-input", "-t", window.PaneID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	capture, err := manager.CaptureStyled(ctx, window.PaneID, 64)
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"physical": capture.Text, "joined": capture.JoinedText} {
		compact := strings.ReplaceAll(text, "\n", "")
		if strings.Contains(compact, "logical-line-00") || !strings.Contains(compact, "logical-line-49") {
			t.Fatalf("%s frame is not aligned to the selected physical window: %q", name, text)
		}
	}
	if capture.BufferRows != 64 {
		t.Fatalf("buffer rows = %d, want 64", capture.BufferRows)
	}
}

func TestTmuxIntegrationExecRunnerForcesUTF8WithoutChangingLocale(t *testing.T) {
	if os.Getenv("ENGRAM_TMUX_INTEGRATION") != "1" {
		t.Skip("set ENGRAM_TMUX_INTEGRATION=1 to run isolated real-tmux coverage")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	setIntegrationTmuxEnvironment(t)
	t.Setenv("LC_ALL", "C")
	t.Setenv("LANG", "C")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	runner := ExecRunner{}
	socket := fmt.Sprintf("engram-utf8-test-%d", os.Getpid())
	_, _ = runner.Run(context.Background(), integrationTmuxArgs(socket, "kill-server")...)
	t.Cleanup(func() { _, _ = runner.Run(context.Background(), integrationTmuxArgs(socket, "kill-server")...) })

	localePath := filepath.Join(t.TempDir(), "locale.txt")
	scriptPath := filepath.Join(t.TempDir(), "utf8.sh")
	script := "#!/bin/sh\nprintf '%s|%s\\n' \"$LC_ALL\" \"$LANG\" > " + ShellQuote(localePath) + "\nexec cat\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(ctx, integrationTmuxArgs(socket, "new-session", "-d", "-x", "40", "-y", "10", "-s", "utf8", scriptPath)...); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(ctx, integrationTmuxArgs(socket, "send-keys", "-l", "-t", "utf8:", "unicode-雪")...); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(ctx, integrationTmuxArgs(socket, "send-keys", "-t", "utf8:", "Enter")...); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	capture, err := runner.Run(ctx, integrationTmuxArgs(socket, "capture-pane", "-p", "-t", "utf8:")...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(capture, "unicode-雪") {
		t.Fatalf("UTF-8 tmux capture = %q", capture)
	}
	var locale []byte
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		locale, err = os.ReadFile(localePath)
		if err == nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(locale)); got != "C|C" {
		t.Fatalf("Engram-created pane locale = %q, want inherited C|C", got)
	}
}

func TestTmuxIntegrationBracketedPasteSubmitsOneMultilineInput(t *testing.T) {
	if os.Getenv("ENGRAM_TMUX_INTEGRATION") != "1" {
		t.Skip("set ENGRAM_TMUX_INTEGRATION=1 to run isolated real-tmux coverage")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	setIntegrationTmuxEnvironment(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	runner := socketRunner{socket: fmt.Sprintf("engram-paste-test-%d", os.Getpid())}
	_, _ = runner.Run(context.Background(), "kill-server")
	t.Cleanup(func() { _, _ = runner.Run(context.Background(), "kill-server") })

	payload := "first line\nsecond line"
	expected := "\x1b[200~" + payload + "\x1b[201~\n"
	outputPath := t.TempDir() + "/input.bin"
	helperPath := t.TempDir() + "/capture-input.sh"
	script := fmt.Sprintf("#!/bin/sh\nprintf '\\033[?2004h'\ndd bs=1 count=%d of=%q 2>/dev/null\nsleep 5\n", len(expected), outputPath)
	if err := os.WriteFile(helperPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(ctx, "new-session", "-d", "-s", "paste", helperPath); err != nil {
		t.Fatal(err)
	}
	manager := New(runner)
	serverID, err := manager.EnsureServerID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	window, err := manager.ResolveTarget(ctx, "paste:0.0")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := manager.SendCommandIfBindingMatches(ctx, window.PaneID, window.ID, serverID, payload); err != nil {
		t.Fatal(err)
	}
	var got []byte
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		got, err = os.ReadFile(outputPath)
		if err == nil && len(got) == len(expected) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if string(got) != expected {
		t.Fatalf("PTY input = %q, want %q (read error %v)", got, expected, err)
	}
}

func TestTmuxIntegrationMetadataFramingPreservesComplexValues(t *testing.T) {
	if os.Getenv("ENGRAM_TMUX_INTEGRATION") != "1" {
		t.Skip("set ENGRAM_TMUX_INTEGRATION=1 to run isolated real-tmux coverage")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	setIntegrationTmuxEnvironment(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	runner := socketRunner{socket: fmt.Sprintf("engram-metadata-test-%d", os.Getpid())}
	_, _ = runner.Run(context.Background(), "kill-server")
	t.Cleanup(func() { _, _ = runner.Run(context.Background(), "kill-server") })
	workdir := t.TempDir() + "/path_under_score_雪\nline"
	if err := os.Mkdir(workdir, 0o700); err != nil {
		t.Fatal(err)
	}
	canonicalWorkdir, err := filepath.EvalSymlinks(workdir)
	if err != nil {
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
	if window.SessionName != "meta_under_score" || window.Name != windowName || window.CurrentPath != canonicalWorkdir || !validImmutableID(window.SessionID, '$') || !validImmutableID(window.ID, '@') || !validImmutableID(window.PaneID, '%') {
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
	setIntegrationTmuxEnvironment(t)
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
	if _, err := runner.Run(ctx, "set-option", "-g", "default-size", "91x33"); err != nil {
		t.Fatal(err)
	}
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
		geometry, err := runner.Run(ctx, "display-message", "-p", "-t", paneID, "#{pane_width}x#{pane_height}")
		if err != nil {
			t.Fatal(err)
		}
		windowSize, err := runner.Run(ctx, "show-options", "-wv", "-t", paneID, "window-size")
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(geometry) != "91x33" || strings.TrimSpace(windowSize) != "manual" {
			t.Fatalf("new pane geometry = %q window-size = %q, want 91x33 and manual", strings.TrimSpace(geometry), strings.TrimSpace(windowSize))
		}
	}
}

func TestTmuxIntegrationCapabilityOptionsConvergeAsOneGuardedTransaction(t *testing.T) {
	if os.Getenv("ENGRAM_TMUX_INTEGRATION") != "1" {
		t.Skip("set ENGRAM_TMUX_INTEGRATION=1 to run isolated real-tmux coverage")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	setIntegrationTmuxEnvironment(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	runner := socketRunner{socket: fmt.Sprintf("engram-capability-test-%d", os.Getpid())}
	_, _ = runner.Run(context.Background(), "kill-server")
	t.Cleanup(func() { _, _ = runner.Run(context.Background(), "kill-server") })
	if _, err := runner.Run(ctx, "new-session", "-d", "-s", "capability", "cat"); err != nil {
		t.Fatal(err)
	}
	manager := New(runner)
	serverID, err := manager.EnsureServerID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	window, err := manager.ResolveTarget(ctx, "capability:")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.AdvertiseEngramIfBindingMatches(ctx, window.PaneID, window.ID, serverID, 42); err != nil {
		t.Fatal(err)
	}
	format := "#{@engram}\x1f#{@engram_watch_id}\x1f#{@engram_notify}\x1f#{@engram_artifact}"
	advertised, err := runner.Run(ctx, "display-message", "-p", "-t", window.PaneID, format)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"v1 watch=42 remote=telegram", "42", "engram signal --stdout MESSAGE", "visible file:// URI"} {
		if !strings.Contains(advertised, want) {
			t.Fatalf("advertised options = %q, missing %q", advertised, want)
		}
	}
	if err := manager.ClearEngramAdvertisementIfBindingMatches(ctx, window.PaneID, window.ID, serverID); err != nil {
		t.Fatal(err)
	}
	cleared, err := runner.Run(ctx, "display-message", "-p", "-t", window.PaneID, format)
	if err != nil {
		t.Fatal(err)
	}
	for _, stale := range []string{"watch=42", "engram signal", "file://"} {
		if strings.Contains(cleared, stale) {
			t.Fatalf("cleared options = %q, retained %q", cleared, stale)
		}
	}
}

func setIntegrationTmuxEnvironment(t *testing.T) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "et-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("TMUX_TMPDIR", dir)
	home := t.TempDir()
	config := "set-option -g base-index 1\nset-window-option -g pane-base-index 1\n"
	if err := os.WriteFile(filepath.Join(home, ".tmux.conf"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
}
