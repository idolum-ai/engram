package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	supervisorModeKey     = "ENGRAM_E2E_SUPERVISOR_HELPER"
	supervisorArgsKey     = "ENGRAM_E2E_SUPERVISOR_ARGS"
	supervisorLogKey      = "ENGRAM_E2E_SUPERVISOR_LOG"
	supervisorReadyKey    = "ENGRAM_E2E_SUPERVISOR_READY"
	supervisorTmuxKey     = "ENGRAM_E2E_SUPERVISOR_CLEAN_TMUX"
	supervisorControlFD   = 3
	supervisorReadyWindow = 10 * time.Second
)

type supervisedProcess struct {
	cmd      *exec.Cmd
	control  *os.File
	childPID int
}

func startSupervisedProcess(t *testing.T, env, args []string, logPath string, cleanupTmux bool) *supervisedProcess {
	t.Helper()
	if len(args) == 0 {
		t.Fatal("supervised command is required")
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	readyPath := filepath.Join(t.TempDir(), "ready")
	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	helper := exec.Command(os.Args[0], "-test.run=^TestE2EProcessSupervisorHelper$", "-test.v")
	helper.Env = append(withoutEnvironment(env, supervisorModeKey, supervisorArgsKey, supervisorLogKey, supervisorReadyKey, supervisorTmuxKey),
		supervisorModeKey+"=1",
		supervisorArgsKey+"="+string(encoded),
		supervisorLogKey+"="+logPath,
		supervisorReadyKey+"="+readyPath,
		supervisorTmuxKey+"="+strconv.FormatBool(cleanupTmux),
	)
	helper.ExtraFiles = []*os.File{controlRead}
	if err := helper.Start(); err != nil {
		_ = controlRead.Close()
		_ = controlWrite.Close()
		t.Fatal(err)
	}
	_ = controlRead.Close()
	var childPID int
	eventually(t, supervisorReadyWindow, func() bool {
		data, readErr := os.ReadFile(readyPath)
		if readErr != nil {
			return false
		}
		childPID, readErr = strconv.Atoi(strings.TrimSpace(string(data)))
		return readErr == nil && childPID > 1
	}, "external E2E supervisor readiness")
	return &supervisedProcess{cmd: helper, control: controlWrite, childPID: childPID}
}

func stopSupervisedProcess(process *supervisedProcess, timeout time.Duration) error {
	if process == nil || process.cmd == nil {
		return nil
	}
	if process.control != nil {
		_ = process.control.Close()
		process.control = nil
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- process.cmd.Wait() }()
	select {
	case err := <-waitCh:
		return err
	case <-time.After(timeout):
		_ = process.cmd.Process.Kill()
		select {
		case <-waitCh:
		case <-time.After(2 * time.Second):
		}
		return fmt.Errorf("external E2E supervisor did not stop within %s", timeout)
	}
}

func TestE2EProcessSupervisorHelper(t *testing.T) {
	if os.Getenv(supervisorModeKey) != "1" {
		t.Skip("subprocess helper")
	}
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv(supervisorArgsKey)), &args); err != nil || len(args) == 0 {
		t.Fatalf("decode supervised command: %v", err)
	}
	logFile, err := os.OpenFile(os.Getenv(supervisorLogKey), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	child := exec.Command(args[0], args[1:]...)
	child.Env = withoutEnvironment(os.Environ(), supervisorModeKey, supervisorArgsKey, supervisorLogKey, supervisorReadyKey, supervisorTmuxKey)
	child.Stdout = logFile
	child.Stderr = logFile
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv(supervisorReadyKey), []byte(strconv.Itoa(child.Process.Pid)+"\n"), 0o600); err != nil {
		_ = signalProcessGroup(child.Process, syscall.SIGKILL)
		_ = child.Wait()
		t.Fatal(err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- child.Wait() }()
	control := os.NewFile(supervisorControlFD, "engram-e2e-supervisor-control")
	if control == nil {
		t.Fatal("supervisor control pipe is unavailable")
	}
	defer control.Close()
	controlClosed := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, control)
		close(controlClosed)
	}()

	var childErr error
	select {
	case childErr = <-waitCh:
		if os.Getenv(supervisorTmuxKey) == "true" {
			_ = stopTmuxServer(child.Env, 0)
		}
		t.Fatalf("supervised process exited before cleanup: %v", childErr)
	case <-controlClosed:
		_ = signalProcessGroup(child.Process, syscall.SIGTERM)
		select {
		case <-waitCh:
		case <-time.After(5 * time.Second):
			_ = signalProcessGroup(child.Process, syscall.SIGKILL)
			select {
			case <-waitCh:
			case <-time.After(2 * time.Second):
				t.Error("supervised process did not exit after SIGKILL")
			}
		}
	case <-time.After(90 * time.Second):
		_ = signalProcessGroup(child.Process, syscall.SIGKILL)
		select {
		case <-waitCh:
		case <-time.After(2 * time.Second):
		}
		t.Error("supervised process exceeded its lifetime")
	}
	if os.Getenv(supervisorTmuxKey) == "true" {
		if err := stopTmuxServer(child.Env, 0); err != nil {
			t.Errorf("supervisor tmux cleanup: %v", err)
		}
	}
}
