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
	supervisorModeKey        = "ENGRAM_E2E_SUPERVISOR_HELPER"
	supervisorArgsKey        = "ENGRAM_E2E_SUPERVISOR_ARGS"
	supervisorLogKey         = "ENGRAM_E2E_SUPERVISOR_LOG"
	supervisorReadyKey       = "ENGRAM_E2E_SUPERVISOR_READY"
	supervisorDoneKey        = "ENGRAM_E2E_SUPERVISOR_DONE"
	supervisorTmuxKey        = "ENGRAM_E2E_SUPERVISOR_CLEAN_TMUX"
	supervisorAnchorKey      = "ENGRAM_E2E_SUPERVISOR_ANCHOR"
	supervisorAnchorReadyKey = "ENGRAM_E2E_SUPERVISOR_ANCHOR_READY"
	supervisorControlFD      = 3
)

var supervisorKeys = []string{supervisorModeKey, supervisorArgsKey, supervisorLogKey, supervisorReadyKey, supervisorDoneKey, supervisorTmuxKey, supervisorAnchorKey, supervisorAnchorReadyKey}
var supervisorTermGrace = 5 * time.Second
var supervisorKillGrace = 2 * time.Second

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
	if err := os.WriteFile(startedPath, []byte("starting\n"), 0o600); err != nil {
		t.Fatal(err)
	}
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
		_ = os.Remove(startedPath)
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
	anchorReady := os.Getenv(supervisorReadyKey) + ".anchor"
	anchor := exec.Command(os.Args[0], "-test.run=^TestE2EProcessGroupAnchorHelper$")
	anchor.Env = append(withoutEnvironment(os.Environ(), supervisorKeys...), supervisorAnchorKey+"=1", supervisorAnchorReadyKey+"="+anchorReady)
	anchor.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := anchor.Start(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(anchorReady); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = anchor.Process.Kill()
			_ = anchor.Wait()
			t.Fatal("process-group anchor did not become ready")
		}
	}
	child := exec.Command(args[0], args[1:]...)
	child.Env = withoutEnvironment(os.Environ(), supervisorKeys...)
	child.Stdout, child.Stderr = logFile, logFile
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: anchor.Process.Pid}
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
	waitCh := make(chan error, 1)
	go func() { waitCh <- child.Wait() }()
	var controlData []byte
	var waitErr error
	childExitedEarly := false
	select {
	case controlData = <-dataCh:
	case waitErr = <-waitCh:
		childExitedEarly = true
	case <-time.After(90 * time.Second):
	}
	planned := strings.Contains(string(controlData), "S")
	cleanupErr := cleanupSupervisedGroup(child, anchor, waitCh, waitErr, childExitedEarly, planned)
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

func TestE2EProcessGroupAnchorHelper(t *testing.T) {
	if os.Getenv(supervisorAnchorKey) != "1" {
		t.Skip("subprocess helper")
	}
	if err := os.WriteFile(os.Getenv(supervisorAnchorReadyKey), []byte("ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func cleanupSupervisedGroup(child, anchor *exec.Cmd, waitCh <-chan error, waitErr error, childExitedEarly, planned bool) error {
	pgid := anchor.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	escalated := false
	if !childExitedEarly {
		select {
		case waitErr = <-waitCh:
		case <-time.After(supervisorTermGrace):
			escalated = true
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			select {
			case waitErr = <-waitCh:
			case <-time.After(supervisorKillGrace):
				return errors.New("workload survived SIGKILL")
			}
		}
	}
	// The unreaped anchor reserves the PGID until this final descendant fence.
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	anchorWait := make(chan error, 1)
	go func() { anchorWait <- anchor.Wait() }()
	select {
	case <-anchorWait:
	case <-time.After(supervisorKillGrace):
		return errors.New("process-group anchor survived SIGKILL")
	}
	if childExitedEarly {
		return fmt.Errorf("workload exited before supervisor cleanup: %v", waitErr)
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

func writeCompletionMarker(path string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte("done\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
