package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
	"github.com/idolum-ai/engram/internal/terminalshot"
)

func TestHermeticGoldenPath(t *testing.T) {
	if os.Getenv("ENGRAM_E2E") != "1" {
		t.Skip("set ENGRAM_E2E=1 to run the process-level tmux, Chromium, and Telegram test")
	}
	binary := requiredAbsolutePath(t, "ENGRAM_E2E_BINARY")
	browser := requiredExecutable(t, "ENGRAM_SNAPSHOT_BROWSER")
	artifactDir := requiredAbsolutePath(t, "ENGRAM_E2E_ARTIFACT_DIR")
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"manifest.json", "process.log", "snapshot.png", "transcript.html", "transcript.png"} {
		if err := os.Remove(filepath.Join(artifactDir, name)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}

	root := t.TempDir()
	home := privateDir(t, root, "home")
	workdir := privateDir(t, root, "work")
	runtimeDir := privateDir(t, root, "runtime")
	tmuxDir := privateDir(t, root, "tmux")
	outputFile := filepath.Join(workdir, "release-notes.txt")
	writeFile(t, outputFile, "Engram hermetic E2E artifact\n", 0o600)
	initialScript := filepath.Join(workdir, "initial.sh")
	replyScript := filepath.Join(workdir, "reply.sh")
	writeFile(t, initialScript, fmt.Sprintf(`#!/bin/sh
printf '\033[3J\033[H\033[2J'
printf 'ENGRAM HERMETIC GOLDEN PATH\n\n'
printf 'Status: snapshot rendering ready\n'
printf 'File: %s\n'
printf 'Link: https://github.com/idolum-ai/engram\n'
printf '\nWaiting for remote input.\n'
`, outputFile), 0o700)
	writeFile(t, replyScript, `#!/bin/sh
printf '\033[3J\033[H\033[2J'
printf 'REMOTE_INPUT_REACHED_TMUX\n\n'
printf 'Status: reply routed through the canonical anchor\n'
printf 'The golden path is complete.\n'
`, 0o700)

	fake := newFakeTelegram()
	defer fake.close()
	envPath := filepath.Join(root, ".env")
	writeFile(t, envPath, strings.Join([]string{
		"TELEGRAM_BOT_TOKEN=" + testBotToken,
		"TELEGRAM_API_BASE=" + fake.apiBase(),
		"TELEGRAM_ALLOWED_USER_ID=" + strconv.FormatInt(testUserID, 10),
		"TELEGRAM_CHAT_ID=" + strconv.FormatInt(testChatID, 10),
		"ENGRAM_HOME=" + home,
		"ENGRAM_WORKDIR=" + workdir,
		"ENGRAM_TMUX_SESSION=e2e",
		"ENGRAM_ANCHOR_MODE=snapshot",
		"ENGRAM_SNAPSHOT_BROWSER=" + browser,
		"ENGRAM_SNAPSHOT_THEME=contrast-dark",
		"TELEGRAM_POLL_TIMEOUT_SECONDS=1",
		"VOICE_INPUT_MODE=path",
		"",
	}, "\n"), 0o600)

	processEnv := isolatedEnvironment(runtimeDir, tmuxDir)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "run", "--env", envPath)
	cmd.Env = processEnv
	var processLog bytes.Buffer
	cmd.Stdout = &processLog
	cmd.Stderr = &processLog
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	stopped := false
	defer func() {
		if !stopped && cmd.Process != nil {
			stopProcess(cmd, waitCh)
		}
		killTmuxServer(processEnv)
	}()

	t.Log("creating a tmux window from an authorized Telegram message")
	updateID := 1
	fake.queue(messageUpdate(updateID, 1, "sh "+initialScript, nil))
	anchor := waitForAnchor(t, fake, 45*time.Second, func(message telegramMessage) bool {
		return len(message.Photo) > 0 && strings.Contains(message.Caption, outputFile)
	})
	assertSnapshotAnchor(t, anchor, outputFile)
	if !fake.snapshot().Pinned[anchor.ID] {
		t.Fatalf("canonical anchor %d was not pinned", anchor.ID)
	}

	t.Log("verifying canonical anchor persistence")
	var persisted state.State
	eventually(t, 10*time.Second, func() bool {
		data, err := os.ReadFile(filepath.Join(home, "state.json"))
		return err == nil && json.Unmarshal(data, &persisted) == nil && len(persisted.TerminalSessions) == 1 && persisted.TerminalSessions[0].AnchorMessageID == anchor.ID && persisted.TerminalSessions[0].AnchorFormat == "snapshot"
	}, "persisted canonical snapshot anchor")

	t.Log("routing a reply through the canonical anchor")
	updateID++
	reply := &telegram.Message{MessageID: anchor.ID, Chat: telegram.Chat{ID: testChatID, Type: "private"}}
	fake.queue(messageUpdate(updateID, 2, "sh "+replyScript, reply))
	eventually(t, 20*time.Second, func() bool {
		return strings.Contains(captureTmux(processEnv, persisted.TerminalSessions[0].TmuxPaneID), "REMOTE_INPUT_REACHED_TMUX")
	}, "reply routed to the tracked tmux pane")

	t.Log("refreshing the canonical anchor through its callback")
	previousEdits := countMethod(fake.snapshot().Calls, "editMessageMedia")
	updateID++
	fake.queue(callbackUpdate(updateID, "refresh-e2e", "refresh:1", anchor.ID))
	eventually(t, 20*time.Second, func() bool {
		snapshot := fake.snapshot()
		return countMethod(snapshot.Calls, "editMessageMedia") > previousEdits && containsPrefix(snapshot.CallbackAnswers, "refresh-e2e:")
	}, "manual refresh callback")

	t.Log("downloading an exact file through its numbered callback")
	latest := fake.snapshot().Messages[anchor.ID]
	fileCallback := callbackWithPrefix(latest.Markup, "file:1:")
	if fileCallback == "" {
		t.Fatalf("snapshot anchor omitted file download callback: %#v", latest.Markup)
	}
	updateID++
	fake.queue(callbackUpdate(updateID, "file-e2e", fileCallback, anchor.ID))
	eventually(t, 20*time.Second, func() bool {
		for _, message := range fake.snapshot().Messages {
			if string(message.Document) == "Engram hermetic E2E artifact\n" {
				return true
			}
		}
		return false
	}, "exact file delivered through its anchor callback")

	t.Log("stopping Engram and writing review evidence")
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := waitProcess(waitCh, cmd.Process, 10*time.Second); err != nil {
		t.Fatalf("Engram exited unsuccessfully: %v\n%s", err, processLog.String())
	}
	stopped = true
	if errors := fake.snapshot().Errors; len(errors) != 0 {
		t.Fatalf("fake Telegram API errors: %v\nprocess log:\n%s", errors, processLog.String())
	}
	writeEvidence(t, artifactDir, fake.snapshot(), anchor.ID, processLog.String())
}

func stopProcess(cmd *exec.Cmd, waitCh <-chan error) {
	_ = cmd.Process.Signal(syscall.SIGTERM)
	_ = waitProcess(waitCh, cmd.Process, 5*time.Second)
}

func waitProcess(waitCh <-chan error, process *os.Process, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-waitCh:
		return err
	case <-timer.C:
		_ = process.Kill()
		select {
		case <-waitCh:
		case <-time.After(2 * time.Second):
		}
		return fmt.Errorf("process did not stop within %s", timeout)
	}
}

func messageUpdate(updateID, messageID int, text string, reply *telegram.Message) telegram.Update {
	return telegram.Update{UpdateID: updateID, Message: &telegram.Message{
		MessageID:      messageID,
		From:           &telegram.User{ID: testUserID, FirstName: "Engram E2E"},
		Chat:           telegram.Chat{ID: testChatID, Type: "private"},
		Text:           text,
		ReplyToMessage: reply,
	}}
}

func callbackUpdate(updateID int, callbackID, data string, anchorID int) telegram.Update {
	return telegram.Update{UpdateID: updateID, CallbackQuery: &telegram.CallbackQuery{
		ID:   callbackID,
		From: telegram.User{ID: testUserID, FirstName: "Engram E2E"},
		Data: data,
		Message: &telegram.Message{
			MessageID: anchorID,
			Chat:      telegram.Chat{ID: testChatID, Type: "private"},
		},
	}}
}

func waitForAnchor(t *testing.T, fake *fakeTelegram, timeout time.Duration, accept func(telegramMessage) bool) telegramMessage {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, message := range fake.snapshot().Messages {
			if accept(message) {
				return message
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for snapshot anchor\nTelegram: %s", fake.diagnostic())
	return telegramMessage{}
}

func assertSnapshotAnchor(t *testing.T, anchor telegramMessage, outputFile string) {
	t.Helper()
	config, err := png.DecodeConfig(bytes.NewReader(anchor.Photo))
	if err != nil {
		t.Fatalf("decode snapshot PNG: %v", err)
	}
	if config.Width != terminalshot.LogicalWidth*terminalshot.PixelRatio || config.Height != terminalshot.LogicalHeight*terminalshot.PixelRatio {
		t.Fatalf("snapshot dimensions = %dx%d", config.Width, config.Height)
	}
	for _, want := range []string{"[1]", outputFile, "https://github.com/idolum-ai/engram"} {
		if !strings.Contains(anchor.Caption, want) {
			t.Fatalf("snapshot caption omitted %q: %q", want, anchor.Caption)
		}
	}
	for _, want := range []string{"refresh:1", "key:1:left", "key:1:right", "file:1:"} {
		if callbackWithPrefix(anchor.Markup, want) == "" {
			t.Fatalf("snapshot markup omitted %q: %#v", want, anchor.Markup)
		}
	}
}

func callbackWithPrefix(markup telegram.InlineKeyboardMarkup, prefix string) string {
	for _, row := range markup.InlineKeyboard {
		for _, button := range row {
			if strings.HasPrefix(button.CallbackData, prefix) {
				return button.CallbackData
			}
		}
	}
	return ""
}

func eventually(t *testing.T, timeout time.Duration, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func captureTmux(env []string, paneID string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tmux", "capture-pane", "-p", "-J", "-t", paneID)
	cmd.Env = env
	data, _ := cmd.Output()
	return string(data)
}

func killTmuxServer(env []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tmux", "kill-server")
	cmd.Env = env
	_ = cmd.Run()
}

func isolatedEnvironment(runtimeDir, tmuxDir string) []string {
	env := make([]string, 0, len(os.Environ())+2)
	for _, value := range os.Environ() {
		if strings.HasPrefix(value, "TMUX=") || strings.HasPrefix(value, "TMUX_TMPDIR=") || strings.HasPrefix(value, "XDG_RUNTIME_DIR=") {
			continue
		}
		env = append(env, value)
	}
	return append(env, "TMUX_TMPDIR="+tmuxDir, "XDG_RUNTIME_DIR="+runtimeDir)
}

func requiredAbsolutePath(t *testing.T, key string) string {
	t.Helper()
	value := os.Getenv(key)
	if value == "" || !filepath.IsAbs(value) {
		t.Fatalf("%s must be an absolute path", key)
	}
	return value
}

func requiredExecutable(t *testing.T, key string) string {
	t.Helper()
	value := os.Getenv(key)
	if value == "" {
		t.Fatalf("%s is required", key)
	}
	path, err := exec.LookPath(value)
	if err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return absolute
}

func privateDir(t *testing.T, root, name string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func countMethod(methods []string, target string) int {
	count := 0
	for _, method := range methods {
		if method == target {
			count++
		}
	}
	return count
}

func containsPrefix(values []string, prefix string) bool {
	for _, value := range values {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}
