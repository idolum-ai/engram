package e2e

import (
	"encoding/json"
	"errors"
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
	supervisorModeKey   = "ENGRAM_E2E_SUPERVISOR_HELPER"
	supervisorArgsKey   = "ENGRAM_E2E_SUPERVISOR_ARGS"
	supervisorLogKey    = "ENGRAM_E2E_SUPERVISOR_LOG"
	supervisorReadyKey  = "ENGRAM_E2E_SUPERVISOR_READY"
	supervisorDoneKey   = "ENGRAM_E2E_SUPERVISOR_DONE"
	supervisorTmuxKey   = "ENGRAM_E2E_SUPERVISOR_CLEAN_TMUX"
	supervisorControlFD = 3
)

var supervisorKeys = []string{supervisorModeKey, supervisorArgsKey, supervisorLogKey, supervisorReadyKey, supervisorDoneKey, supervisorTmuxKey}
var supervisorTermGrace = 5 * time.Second
var supervisorKillGrace = 2 * time.Second
var supervisorGroupGrace = time.Second

type supervisedProcess struct {
	cmd      *exec.Cmd
	control  *os.File
	childPID int
	donePath string
}

func startSupervisedProcess(t *testing.T, env, args []string, logPath string, cleanupTmux bool) *supervisedProcess {
	t.Helper()
	encoded, err := json.Marshal(args)
	if err != nil || len(args) == 0 {
		t.Fatalf("encode supervised command: %v", err)
	}
	readyPath := filepath.Join(t.TempDir(), "ready")
	donePath := filepath.Join(filepath.Dir(logPath), ".supervisor-done")
	startedPath := filepath.Join(filepath.Dir(logPath), ".supervisor-started")
	_ = os.Remove(donePath)
	_ = os.Remove(startedPath)
	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	supervisorLog, err := os.OpenFile(filepath.Join(filepath.Dir(logPath), "supervisor.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	helper := exec.Command(os.Args[0], "-test.run=^TestE2EProcessSupervisorHelper$")
	helper.Env = append(withoutEnvironment(env, supervisorKeys...), supervisorModeKey+"=1", supervisorArgsKey+"="+string(encoded), supervisorLogKey+"="+logPath, supervisorReadyKey+"="+readyPath, supervisorDoneKey+"="+donePath, supervisorTmuxKey+"="+strconv.FormatBool(cleanupTmux))
	helper.ExtraFiles = []*os.File{controlRead}
	helper.Stdout, helper.Stderr = supervisorLog, supervisorLog
	if err := helper.Start(); err != nil {
		_ = controlRead.Close()
		_ = controlWrite.Close()
		_ = supervisorLog.Close()
		t.Fatal(err)
	}
	_ = controlRead.Close()
	_ = supervisorLog.Close()
	var childPID int
	eventually(t, 10*time.Second, func() bool {
		data, readErr := os.ReadFile(readyPath)
		childPID, readErr = strconv.Atoi(strings.TrimSpace(string(data)))
		return readErr == nil && childPID > 1
	}, "external E2E supervisor readiness")
	if err := os.WriteFile(startedPath, []byte("started\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &supervisedProcess{cmd: helper, control: controlWrite, childPID: childPID, donePath: donePath}
}

func stopSupervisedProcess(process *supervisedProcess, timeout time.Duration) error {
	if process == nil || process.cmd == nil {
		return nil
	}
	if process.control != nil {
		_, _ = process.control.Write([]byte("S"))
		_ = process.control.Close()
		process.control = nil
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- process.cmd.Wait() }()
	select {
	case err := <-waitCh:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("external E2E supervisor did not stop within %s; cleanup remains externally owned", timeout)
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
	child := exec.Command(args[0], args[1:]...)
	child.Env = withoutEnvironment(os.Environ(), supervisorKeys...)
	child.Stdout, child.Stderr = logFile, logFile
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv(supervisorReadyKey), []byte(strconv.Itoa(child.Process.Pid)+"\n"), 0o600); err != nil {
		_ = signalProcessGroup(child.Process, syscall.SIGKILL)
		_ = child.Wait()
		t.Fatal(err)
	}
	control := os.NewFile(supervisorControlFD, "engram-e2e-supervisor-control")
	dataCh := make(chan []byte, 1)
	go func() { data, _ := io.ReadAll(control); dataCh <- data }()
	var controlData []byte
	select {
	case controlData = <-dataCh:
	case <-time.After(90 * time.Second):
	}
	planned := strings.Contains(string(controlData), "S")
	cleanupErr := cleanupSupervisedChild(child, planned)
	if os.Getenv(supervisorTmuxKey) == "true" {
		if err := stopTmuxServer(child.Env, 0); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("tmux cleanup: %w", err))
		}
	}
	_ = control.Close()
	_ = logFile.Close()
	if cleanupErr != nil {
		fmt.Fprintln(os.Stderr, "supervisor cleanup failed:", cleanupErr)
	}
	if err := writeCompletionMarker(os.Getenv(supervisorDoneKey)); err != nil {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	if cleanupErr != nil {
		os.Exit(1)
	}
}

func cleanupSupervisedChild(child *exec.Cmd, planned bool) error {
	_ = signalProcessGroup(child.Process, syscall.SIGTERM)
	waitCh := make(chan error, 1)
	go func() { waitCh <- child.Wait() }()
	var waitErr error
	escalated := false
	select {
	case waitErr = <-waitCh:
	case <-time.After(supervisorTermGrace):
		escalated = true
		_ = signalProcessGroup(child.Process, syscall.SIGKILL)
		select {
		case waitErr = <-waitCh:
		case <-time.After(supervisorKillGrace):
			return errors.New("leader survived SIGKILL")
		}
	}
	if waitForProcessGroupExit(child.Process.Pid, supervisorGroupGrace) == false {
		escalated = true
		_ = syscall.Kill(-child.Process.Pid, syscall.SIGKILL)
		if !waitForProcessGroupExit(child.Process.Pid, supervisorKillGrace) {
			return errors.New("process group survived SIGKILL")
		}
	}
	if planned {
		if escalated {
			return errors.New("planned shutdown required SIGKILL")
		}
		if waitErr != nil && !terminatedBy(waitErr, syscall.SIGTERM) {
			return fmt.Errorf("planned shutdown: %w", waitErr)
		}
	}
	return nil
}

func terminatedBy(err error, signal syscall.Signal) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && status.Signal() == signal
}

func waitForProcessGroupExit(pgid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH)
}

func writeCompletionMarker(path string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte("done\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
