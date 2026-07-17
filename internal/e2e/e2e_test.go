package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
	"github.com/idolum-ai/engram/internal/terminalshot"
	engramtmux "github.com/idolum-ai/engram/internal/tmux"
)

const hostConfigMarker = "HOST_TMUX_CONFIG_LEAKED"

func TestHermeticGoldenPath(t *testing.T) {
	if os.Getenv("ENGRAM_E2E") != "1" {
		t.Skip("set ENGRAM_E2E=1 to run the process-level tmux, Chromium, and Telegram test")
	}
	artifactDir := requiredAbsolutePath(t, "ENGRAM_E2E_ARTIFACT_DIR")
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var processLog bytes.Buffer
	var fake *fakeTelegram
	versions := make(map[string]string)
	assertions := make([]string, 0, 8)
	evidenceWritten := false
	for _, name := range []string{"manifest.json", "process.log", "snapshot.png", "snapshot.txt", "telegram.log", "transcript.html", "transcript.png"} {
		if err := os.Remove(filepath.Join(artifactDir, name)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	processFile, processWriter, err := openProcessLog(artifactDir, &processLog)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if evidenceWritten {
			return
		}
		diagnostic := "fake Telegram did not start"
		if fake != nil {
			diagnostic = fake.diagnostic()
		}
		if err := writeFailureEvidence(artifactDir, assertions, processLog.String(), diagnostic, versions); err != nil {
			t.Errorf("write failure evidence: %v", err)
		}
	}()
	defer processFile.Close()

	binary := requiredAbsolutePath(t, "ENGRAM_E2E_BINARY")
	browser := requiredExecutable(t, "ENGRAM_SNAPSHOT_BROWSER")
	tmuxBinary := requiredExecutable(t, "ENGRAM_E2E_TMUX", "tmux")

	root := t.TempDir()
	stateHome := privateDir(t, root, "state")
	userHome := privateDir(t, root, "user-home")
	configHome := privateDir(t, root, "config")
	cacheHome := privateDir(t, root, "cache")
	workdir := privateDir(t, root, "work")
	runtimeDir := privateDir(t, root, "runtime")
	tmuxDir := privateDir(t, root, "tmux")
	binDir := privateDir(t, root, "bin")
	writeTmuxWrapper(t, binDir, tmuxBinary)
	writeFile(t, filepath.Join(userHome, ".tmux.conf"), "set -g default-command 'printf "+hostConfigMarker+"; sleep 60'\n", 0o600)

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
	writeFile(t, replyScript, fmt.Sprintf(`#!/bin/sh
printf 'REMOTE_INPUT_REACHED_TMUX\n\n'
printf 'Status: reply routed through the canonical anchor\n'
printf 'File: %s\n'
printf 'Link: https://github.com/idolum-ai/engram\n'
printf 'The golden path is complete.\n'
`, outputFile), 0o700)

	fake = newFakeTelegram()
	defer fake.close()
	envPath := filepath.Join(root, ".env")
	writeFile(t, envPath, strings.Join([]string{
		"TELEGRAM_BOT_TOKEN=" + testBotToken,
		"TELEGRAM_API_BASE=" + fake.apiBase(),
		"TELEGRAM_ALLOWED_USER_ID=" + strconv.FormatInt(testUserID, 10),
		"TELEGRAM_CHAT_ID=" + strconv.FormatInt(testChatID, 10),
		"ENGRAM_HOME=" + stateHome,
		"ENGRAM_WORKDIR=" + workdir,
		"ENGRAM_TMUX_SESSION=e2e",
		"ENGRAM_ANCHOR_MODE=snapshot",
		"ENGRAM_SNAPSHOT_BROWSER=" + browser,
		"ENGRAM_SNAPSHOT_THEME=contrast-dark",
		"TELEGRAM_POLL_TIMEOUT_SECONDS=1",
		"VOICE_INPUT_MODE=path",
		"",
	}, "\n"), 0o600)

	processEnv := isolatedEnvironment(binDir, userHome, configHome, cacheHome, runtimeDir, tmuxDir)
	versions = runtimeVersions(browser, processEnv)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "run", "--env", envPath)
	cmd.Env = processEnv
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return signalProcessGroup(cmd.Process, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	cmd.Stdout = processWriter
	cmd.Stderr = processWriter
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	tmuxStopped := false
	tmuxPID := 0
	defer func() {
		if !stopped {
			if err := stopProcessGroup(cmd, 5*time.Second); err != nil {
				t.Errorf("cleanup Engram: %v", err)
			}
		}
		if !tmuxStopped {
			if err := stopTmuxServer(processEnv, tmuxPID); err != nil {
				t.Errorf("cleanup tmux: %v", err)
			}
		}
	}()

	t.Log("creating a tmux window from an authorized Telegram message")
	updateID := 1
	fake.queue(messageUpdate(updateID, 1, "sh "+initialScript, nil))
	anchor := waitForAnchor(t, fake, 45*time.Second, func(message telegramMessage) bool {
		return message.ChatID == testChatID && len(message.Photo) > 0 && strings.Contains(message.Caption, outputFile)
	})
	assertSnapshotAnchor(t, anchor, outputFile)
	eventually(t, 10*time.Second, func() bool { return fake.snapshot().Pinned[anchor.ID] }, "canonical anchor pin")
	waitForOffset(t, fake, updateID)
	assertions = append(assertions, "Telegram message created an isolated tmux window")
	assertions = append(assertions, "canonical anchor became a pinned nonblank Chromium snapshot")

	t.Log("verifying canonical anchor persistence and private tmux identity")
	var persisted state.State
	eventually(t, 10*time.Second, func() bool {
		data, err := os.ReadFile(filepath.Join(stateHome, "state.json"))
		return err == nil && json.Unmarshal(data, &persisted) == nil && len(persisted.TerminalSessions) == 1 && persisted.TerminalSessions[0].AnchorMessageID == anchor.ID && persisted.TerminalSessions[0].AnchorFormat == "snapshot"
	}, "persisted canonical snapshot anchor")
	tmuxPID = waitForTmuxServerPID(t, processEnv)
	initialFrame, err := captureTmux(processEnv, persisted.TerminalSessions[0].TmuxPaneID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(initialFrame, hostConfigMarker) {
		t.Fatal("private tmux loaded the planted user configuration")
	}
	assertions = append(assertions, "tmux ignored ambient configuration and used a private server")

	t.Log("routing a reply through the canonical anchor")
	initialPhoto := append([]byte(nil), anchor.Photo...)
	updateID++
	reply := &telegram.Message{MessageID: anchor.ID, Chat: telegram.Chat{ID: testChatID, Type: "private"}}
	fake.queue(messageUpdate(updateID, 2, "sh "+replyScript, reply))
	var terminalText string
	eventually(t, 20*time.Second, func() bool {
		frame, captureErr := captureTmux(processEnv, persisted.TerminalSessions[0].TmuxPaneID)
		if captureErr != nil {
			return false
		}
		terminalText = frame
		return strings.Contains(frame, "REMOTE_INPUT_REACHED_TMUX") && strings.Contains(frame, outputFile) && !strings.Contains(frame, hostConfigMarker)
	}, "reply routed to the tracked tmux pane")
	eventually(t, 20*time.Second, func() bool {
		latest := fake.snapshot().Messages[anchor.ID]
		return latest.ChatID == testChatID && len(latest.Photo) > 0 && !bytes.Equal(latest.Photo, initialPhoto) && strings.Contains(latest.Caption, outputFile)
	}, "reply-driven canonical snapshot edit")
	terminalText, err = captureTmuxEvidence(processEnv, persisted.TerminalSessions[0].TmuxPaneID)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Status: snapshot rendering ready", "REMOTE_INPUT_REACHED_TMUX", "The golden path is complete."} {
		if !strings.Contains(terminalText, want) {
			t.Fatalf("64-row text evidence omitted %q: %q", want, terminalText)
		}
	}
	waitForOffset(t, fake, updateID)
	assertions = append(assertions, "reply input reached the tracked pane and changed its canonical snapshot")

	t.Log("refreshing the canonical anchor through its callback")
	refreshCallback := callbackWithPrefix(fake.snapshot().Messages[anchor.ID].Markup, "refresh:")
	if refreshCallback == "" {
		t.Fatal("canonical anchor omitted its refresh callback")
	}
	updateID++
	fake.queue(callbackUpdate(updateID, "refresh-e2e", refreshCallback, anchor.ID))
	eventually(t, 20*time.Second, func() bool {
		return editAfterCallback(fake.snapshot(), "refresh-e2e", testChatID, anchor.ID)
	}, "manual refresh edit after callback answer")
	waitForOffset(t, fake, updateID)
	assertions = append(assertions, "manual refresh answered once and then edited the canonical anchor")

	t.Log("downloading an exact file through its numbered callback")
	latest := fake.snapshot().Messages[anchor.ID]
	fileIndex := fileIndexForPath(latest.Caption, outputFile)
	if fileIndex == 0 {
		t.Fatalf("snapshot caption did not enumerate %q: %q", outputFile, latest.Caption)
	}
	fileCallback := callbackForFileIndex(latest.Markup, fileIndex)
	if fileCallback == "" {
		t.Fatalf("snapshot anchor omitted file download callback: %#v", latest.Markup)
	}
	updateID++
	fake.queue(callbackUpdate(updateID, "file-e2e", fileCallback, anchor.ID))
	eventually(t, 20*time.Second, func() bool {
		return exactDocumentCount(fake.snapshot(), testChatID, "release-notes.txt", "Engram hermetic E2E artifact\n") == 1 && callbackAnswerCount(fake.snapshot(), "file-e2e") == 1
	}, "exact file delivered once through its anchor callback")
	waitForOffset(t, fake, updateID)
	finalSnapshot := fake.snapshot()
	if callbackAnswerCount(finalSnapshot, "refresh-e2e") != 1 || callbackAnswerCount(finalSnapshot, "file-e2e") != 1 || countMethod(finalSnapshot.Calls, "sendDocument") != 1 {
		t.Fatalf("callbacks or document delivery were replayed: %s", fake.diagnostic())
	}
	assertions = append(assertions, "numbered file callback delivered exact bytes and filename once to the authorized chat")

	t.Log("stopping Engram and its private tmux server")
	stopErr := stopProcessGroup(cmd, 10*time.Second)
	stopped = true
	if stopErr != nil {
		t.Fatalf("Engram exited unsuccessfully: %v\n%s", stopErr, processLog.String())
	}
	if err := stopTmuxServer(processEnv, tmuxPID); err != nil {
		t.Fatalf("tmux cleanup failed: %v", err)
	}
	tmuxStopped = true
	if len(finalSnapshot.Errors) != 0 {
		t.Fatalf("fake Telegram API errors: %v\nprocess log:\n%s", finalSnapshot.Errors, processLog.String())
	}
	if err := writeEvidence(artifactDir, finalSnapshot, anchor.ID, processLog.String(), terminalText, browser, assertions, versions); err != nil {
		t.Fatal(err)
	}
	evidenceWritten = true
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
	var result telegramMessage
	eventually(t, timeout, func() bool {
		for _, message := range fake.snapshot().Messages {
			if accept(message) {
				result = message
				return true
			}
		}
		return false
	}, "snapshot anchor; "+fake.diagnostic())
	return result
}

func assertSnapshotAnchor(t *testing.T, anchor telegramMessage, outputFile string) {
	t.Helper()
	decoded, err := png.Decode(bytes.NewReader(anchor.Photo))
	if err != nil {
		t.Fatalf("decode snapshot PNG: %v", err)
	}
	bounds := decoded.Bounds()
	if bounds.Dx() != terminalshot.LogicalWidth*terminalshot.PixelRatio || bounds.Dy() != terminalshot.LogicalHeight*terminalshot.PixelRatio {
		t.Fatalf("snapshot dimensions = %dx%d", bounds.Dx(), bounds.Dy())
	}
	assertNonblankImage(t, decoded)
	if anchor.ChatID != testChatID {
		t.Fatalf("snapshot anchor chat = %d", anchor.ChatID)
	}
	if anchor.ReplyTo != 1 {
		t.Fatalf("snapshot anchor reply target = %d, want fixture message 1", anchor.ReplyTo)
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

func assertNonblankImage(t *testing.T, frame image.Image) {
	t.Helper()
	bright := 0
	colors := make(map[uint32]struct{})
	for y := frame.Bounds().Min.Y; y < frame.Bounds().Max.Y; y++ {
		for x := frame.Bounds().Min.X; x < frame.Bounds().Max.X; x++ {
			r, g, b, _ := frame.At(x, y).RGBA()
			if r+g+b > 3*0x3000 {
				bright++
			}
			if len(colors) < 64 {
				colors[uint32(r>>8)<<16|uint32(g>>8)<<8|uint32(b>>8)] = struct{}{}
			}
		}
	}
	if bright < 2_000 || len(colors) < 8 {
		t.Fatalf("snapshot appears blank: bright_pixels=%d distinct_colors=%d", bright, len(colors))
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

func fileIndexForPath(caption, path string) int {
	for _, line := range strings.Split(caption, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "<pre>"), "</pre>"))
		indexText, candidate, found := strings.Cut(line, ". ")
		if !found || candidate != path {
			continue
		}
		index, err := strconv.Atoi(indexText)
		if err == nil && index > 0 {
			return index
		}
	}
	return 0
}

func callbackForFileIndex(markup telegram.InlineKeyboardMarkup, index int) string {
	suffix := ":" + strconv.Itoa(index)
	for _, row := range markup.InlineKeyboard {
		for _, button := range row {
			if strings.HasPrefix(button.CallbackData, "file:") && strings.HasSuffix(button.CallbackData, suffix) {
				return button.CallbackData
			}
		}
	}
	return ""
}

func editAfterCallback(snapshot fakeTelegramSnapshot, callbackID string, chatID int64, messageID int) bool {
	answered := false
	for _, event := range snapshot.Events {
		if event.Method == "answerCallbackQuery" && event.CallbackID == callbackID {
			answered = true
			continue
		}
		if answered && event.Method == "editMessageMedia" && event.ChatID == chatID && event.MessageID == messageID {
			return callbackAnswerCount(snapshot, callbackID) == 1
		}
	}
	return false
}

func callbackAnswerCount(snapshot fakeTelegramSnapshot, callbackID string) int {
	count := 0
	for _, event := range snapshot.Events {
		if event.Method == "answerCallbackQuery" && event.CallbackID == callbackID {
			count++
		}
	}
	return count
}

func exactDocumentCount(snapshot fakeTelegramSnapshot, chatID int64, filename, content string) int {
	count := 0
	for _, message := range snapshot.Messages {
		if message.ChatID == chatID && message.Filename == filename && string(message.Document) == content {
			count++
		}
	}
	return count
}

func waitForOffset(t *testing.T, fake *fakeTelegram, updateID int) {
	t.Helper()
	eventually(t, 10*time.Second, func() bool {
		for _, offset := range fake.snapshot().PollOffsets {
			if offset > updateID {
				return true
			}
		}
		return false
	}, fmt.Sprintf("poll offset beyond update %d", updateID))
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

func captureTmux(env []string, paneID string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tmux", "capture-pane", "-p", "-J", "-t", paneID)
	cmd.Env = env
	data, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("capture private tmux pane: %w: %s", err, strings.TrimSpace(string(data)))
	}
	return string(data), nil
}

type environmentTmuxRunner struct {
	env []string
}

func (r environmentTmuxRunner) Run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "tmux", args...)
	cmd.Env = r.env
	data, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("tmux %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(data)))
	}
	return string(data), nil
}

func captureTmuxEvidence(env []string, paneID string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	capture, err := engramtmux.New(environmentTmuxRunner{env: env}).CaptureStyled(ctx, paneID, 64)
	if err != nil {
		return "", fmt.Errorf("capture production-equivalent tmux evidence: %w", err)
	}
	return capture.Text, nil
}

func waitForTmuxServerPID(t *testing.T, env []string) int {
	t.Helper()
	pid := 0
	eventually(t, 5*time.Second, func() bool {
		var err error
		pid, err = tmuxServerPID(env)
		return err == nil && pid > 1
	}, "private tmux server PID")
	return pid
}

func tmuxServerPID(env []string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tmux", "display-message", "-p", "#{pid}")
	cmd.Env = env
	data, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("inspect tmux server PID: %w: %s", err, strings.TrimSpace(string(data)))
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 {
		return 0, fmt.Errorf("invalid tmux server PID %q", strings.TrimSpace(string(data)))
	}
	return pid, nil
}

func stopTmuxServer(env []string, expectedPID int) error {
	if expectedPID == 0 {
		expectedPID, _ = tmuxServerPID(env)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	cmd := exec.CommandContext(ctx, "tmux", "kill-server")
	cmd.Env = env
	output, killErr := cmd.CombinedOutput()
	cancel()
	if expectedPID > 1 && waitForProcessExit(expectedPID, 2*time.Second) {
		return nil
	}
	if expectedPID <= 1 && killErr != nil && missingTmuxServerText(string(output)) {
		return nil
	}
	if killErr != nil {
		return fmt.Errorf("tmux kill-server: %w: %s", killErr, strings.TrimSpace(string(output)))
	}
	return fmt.Errorf("tmux server PID %d still appears live after kill-server; refusing unsafe raw-PID fallback", expectedPID)
}

func missingTmuxServerText(output string) bool {
	text := strings.ToLower(output)
	return strings.Contains(text, "no server running") || strings.Contains(text, "no such file or directory")
}

func waitForProcessExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return !processAlive(pid)
}

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func stopProcessGroup(cmd *exec.Cmd, timeout time.Duration) error {
	_ = signalProcessGroup(cmd.Process, syscall.SIGTERM)
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-waitCh:
		return err
	case <-timer.C:
		_ = signalProcessGroup(cmd.Process, syscall.SIGKILL)
		select {
		case <-waitCh:
		case <-time.After(2 * time.Second):
		}
		return fmt.Errorf("process group did not stop within %s", timeout)
	}
}

func openProcessLog(dir string, memory *bytes.Buffer) (*os.File, io.Writer, error) {
	file, err := os.OpenFile(filepath.Join(dir, "process.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, nil, err
	}
	return file, io.MultiWriter(memory, file), nil
}

func signalProcessGroup(process *os.Process, signal syscall.Signal) error {
	if process == nil {
		return nil
	}
	err := syscall.Kill(-process.Pid, signal)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func isolatedEnvironment(binDir, home, configHome, cacheHome, runtimeDir, tmuxDir string) []string {
	return []string{
		"PATH=" + binDir + ":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + configHome,
		"XDG_CACHE_HOME=" + cacheHome,
		"XDG_RUNTIME_DIR=" + runtimeDir,
		"TMUX_TMPDIR=" + tmuxDir,
		"SHELL=/bin/sh",
		"PS1=engram-e2e$ ",
		"USER=engram-e2e",
		"LOGNAME=engram-e2e",
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
		"TERM=xterm-256color",
	}
}

func writeTmuxWrapper(t *testing.T, binDir, tmuxBinary string) {
	t.Helper()
	script := "#!/bin/sh\nexec " + shellQuote(tmuxBinary) + " -f /dev/null \"$@\"\n"
	writeFile(t, filepath.Join(binDir, "tmux"), script, 0o700)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func runtimeVersions(browser string, env []string) map[string]string {
	versions := map[string]string{
		"go":            runtime.Version(),
		"platform":      runtime.GOOS + "/" + runtime.GOARCH,
		"runner_os":     os.Getenv("RUNNER_OS"),
		"image_os":      os.Getenv("ImageOS"),
		"image_version": os.Getenv("ImageVersion"),
	}
	versions["tmux"] = commandVersion(env, "tmux", "-V")
	versions["browser"] = commandVersion(env, browser, "--version")
	return versions
}

func commandVersion(env []string, name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	data, err := cmd.CombinedOutput()
	if err != nil {
		return "unavailable: " + err.Error()
	}
	return strings.TrimSpace(string(data))
}

func requiredAbsolutePath(t *testing.T, key string) string {
	t.Helper()
	value := os.Getenv(key)
	if value == "" || !filepath.IsAbs(value) {
		t.Fatalf("%s must be an absolute path", key)
	}
	return value
}

func requiredExecutable(t *testing.T, key string, defaults ...string) string {
	t.Helper()
	value := os.Getenv(key)
	if value == "" && len(defaults) > 0 {
		value = defaults[0]
	}
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
