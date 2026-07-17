package e2e

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestEarlySetupFailureRetainsEvidence(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHermeticGoldenPath$", "-test.v")
	cmd.Env = append(withoutEnvironment(os.Environ(),
		"ENGRAM_E2E",
		"ENGRAM_E2E_BINARY",
		"ENGRAM_E2E_ARTIFACT_DIR",
		"ENGRAM_SNAPSHOT_BROWSER",
	),
		"ENGRAM_E2E=1",
		"ENGRAM_E2E_BINARY=/definitely/missing/engram",
		"ENGRAM_E2E_ARTIFACT_DIR="+dir,
		"ENGRAM_SNAPSHOT_BROWSER=definitely-missing-browser",
	)
	if err := cmd.Run(); err == nil {
		t.Fatal("early-failure subprocess unexpectedly passed")
	}
	for _, name := range []string{"manifest.json", "process.log", "telegram.log"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("early failure omitted %s: %v", name, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest evidenceManifest
	if err := json.Unmarshal(data, &manifest); err != nil || manifest.Status != "failed" {
		t.Fatalf("early failure manifest = %#v, err=%v", manifest, err)
	}
}

func TestProcessLogSurvivesHardExit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSupervisorOwnerHardExitHelper$")
	cmd.Env = append(withoutEnvironment(os.Environ(), "ENGRAM_E2E_HARD_EXIT_DIR"), "ENGRAM_E2E_HARD_EXIT_DIR="+dir)
	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 23 {
		t.Fatalf("hard-exit helper error = %v", err)
	}
	pidData, err := os.ReadFile(filepath.Join(dir, "child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, func() bool {
		data, readErr := os.ReadFile(filepath.Join(dir, "process.log"))
		return readErr == nil && strings.Contains(string(data), "output after owner hard exit") && !processAlive(childPID)
	}, "supervisor child cleanup and direct log after owner hard exit")
	supervisorData, err := os.ReadFile(filepath.Join(dir, "supervisor.pid"))
	if err != nil {
		t.Fatal(err)
	}
	supervisorPID, err := strconv.Atoi(strings.TrimSpace(string(supervisorData)))
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, func() bool { return !processAlive(supervisorPID) }, "orphaned supervisor exit")
	tmuxData, err := os.ReadFile(filepath.Join(dir, "tmux.pid"))
	if err != nil {
		t.Fatal(err)
	}
	tmuxPID, err := strconv.Atoi(strings.TrimSpace(string(tmuxData)))
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, func() bool { return !processAlive(tmuxPID) }, "private tmux cleanup after owner hard exit")
}

func TestSupervisorOwnerHardExitHelper(t *testing.T) {
	dir := os.Getenv("ENGRAM_E2E_HARD_EXIT_DIR")
	if dir == "" {
		t.Skip("subprocess helper")
	}
	logPath := filepath.Join(dir, "process.log")
	childReady := filepath.Join(dir, "child.ready")
	tmuxBinary := requiredExecutable(t, "ENGRAM_E2E_TMUX", "tmux")
	binDir := privateDir(t, dir, "hard-exit-bin")
	writeTmuxWrapper(t, binDir, tmuxBinary)
	env := append(isolatedEnvironment(binDir, privateDir(t, dir, "hard-exit-home"), privateDir(t, dir, "hard-exit-config"), privateDir(t, dir, "hard-exit-cache"), privateDir(t, dir, "hard-exit-runtime"), privateDir(t, dir, "hard-exit-tmux")),
		"ENGRAM_E2E_SUPERVISED_CHILD=1",
		"ENGRAM_E2E_SUPERVISED_CHILD_READY="+childReady,
		"ENGRAM_E2E_SUPERVISED_TMUX_PID="+filepath.Join(dir, "tmux.pid"),
	)
	process := startSupervisedProcess(t, env, []string{os.Args[0], "-test.run=^TestSupervisedChildHelper$", "-test.v"}, logPath, true)
	eventually(t, 5*time.Second, func() bool {
		_, err := os.Stat(childReady)
		return err == nil
	}, "supervised child signal readiness")
	if err := os.WriteFile(filepath.Join(dir, "child.pid"), []byte(strconv.Itoa(process.childPID)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "supervisor.pid"), []byte(strconv.Itoa(process.cmd.Process.Pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Exit(23)
}

func TestSupervisedChildHelper(t *testing.T) {
	if os.Getenv("ENGRAM_E2E_SUPERVISED_CHILD") != "1" {
		t.Skip("subprocess helper")
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)
	defer signal.Stop(signals)
	if err := exec.Command("tmux", "new-session", "-d", "-s", "hard-exit", "sleep 30").Run(); err != nil {
		t.Fatal(err)
	}
	pidOutput, err := exec.Command("tmux", "display-message", "-p", "#{pid}").Output()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("ENGRAM_E2E_SUPERVISED_TMUX_PID"), pidOutput, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("ENGRAM_E2E_SUPERVISED_CHILD_READY"), []byte("ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	<-signals
	println("output after owner hard exit")
}

func TestPlannedSupervisorShutdownRejectsNonzeroExit(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	child := exec.Command("/bin/sh", "-c", "trap 'exit 7' TERM; : > \"$READY\"; while :; do sleep 1; done")
	child.Env = append(os.Environ(), "READY="+ready)
	anchor, waitCh := startAnchoredTestChild(t, child)
	eventually(t, 3*time.Second, func() bool { _, err := os.Stat(ready); return err == nil }, "nonzero child readiness")
	if err := cleanupSupervisedGroup(child, anchor, waitCh, nil, false, true); err == nil || !strings.Contains(err.Error(), "exit status 7") {
		t.Fatalf("planned nonzero cleanup error = %v", err)
	}
}

func TestPlannedSupervisorShutdownRejectsForcedKill(t *testing.T) {
	oldTerm, oldKill := supervisorTermGrace, supervisorKillGrace
	supervisorTermGrace, supervisorKillGrace = 100*time.Millisecond, time.Second
	defer func() { supervisorTermGrace, supervisorKillGrace = oldTerm, oldKill }()
	child := exec.Command("/bin/sh", "-c", "trap '' TERM; while :; do sleep 1; done")
	anchor, waitCh := startAnchoredTestChild(t, child)
	time.Sleep(100 * time.Millisecond)
	if err := cleanupSupervisedGroup(child, anchor, waitCh, nil, false, true); err == nil || !strings.Contains(err.Error(), "required SIGKILL") {
		t.Fatalf("forced cleanup error = %v", err)
	}
}

func TestSupervisorRemovesDescendantAfterLeaderExit(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "descendant.pid")
	child := exec.Command("/bin/sh", "-c", "sh -c 'trap \"\" TERM; sleep 30' & echo $! > \"$PID_PATH\"")
	child.Env = append(os.Environ(), "PID_PATH="+pidPath)
	anchor, waitCh := startAnchoredTestChild(t, child)
	var descendantPID int
	eventually(t, 3*time.Second, func() bool {
		data, err := os.ReadFile(pidPath)
		descendantPID, err = strconv.Atoi(strings.TrimSpace(string(data)))
		return err == nil && descendantPID > 1
	}, "TERM-ignoring descendant readiness")
	var waitErr error
	childExited := false
	select {
	case waitErr = <-waitCh:
		childExited = true
	case <-time.After(time.Second):
	}
	err := cleanupSupervisedGroup(child, anchor, waitCh, waitErr, childExited, true)
	if err == nil || !strings.Contains(err.Error(), "before supervisor cleanup") {
		t.Fatalf("descendant cleanup error = %v", err)
	}
	eventually(t, 3*time.Second, func() bool { return !processAlive(descendantPID) }, "TERM-ignoring descendant exit")
}

func startAnchoredTestChild(t *testing.T, child *exec.Cmd) (*exec.Cmd, <-chan error) {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "anchor.ready")
	anchor := exec.Command(os.Args[0], "-test.run=^TestE2EProcessGroupAnchorHelper$")
	anchor.Env = append(os.Environ(), supervisorAnchorKey+"=1", supervisorAnchorReadyKey+"="+ready)
	anchor.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := anchor.Start(); err != nil {
		t.Fatal(err)
	}
	eventually(t, 3*time.Second, func() bool { _, err := os.Stat(ready); return err == nil }, "test process-group anchor")
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: anchor.Process.Pid}
	if err := child.Start(); err != nil {
		_ = anchor.Process.Kill()
		_ = anchor.Wait()
		t.Fatal(err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- child.Wait() }()
	return anchor, waitCh
}

func TestSupervisorFailureIsRetained(t *testing.T) {
	dir := t.TempDir()
	process := startSupervisedProcess(t, os.Environ(), []string{"/bin/sh", "-c", "trap 'exit 7' TERM; while :; do sleep 1; done"}, filepath.Join(dir, "process.log"), false)
	time.Sleep(100 * time.Millisecond)
	if err := stopSupervisedProcess(process, 10*time.Second); err == nil {
		t.Fatal("failing supervisor unexpectedly passed")
	}
	data, err := os.ReadFile(filepath.Join(dir, "supervisor.log"))
	if err != nil || !strings.Contains(string(data), "exit status 7") {
		t.Fatalf("supervisor.log = %q, err=%v", data, err)
	}
}

func TestSupervisorRejectsEarlyCleanExit(t *testing.T) {
	dir := t.TempDir()
	process := startSupervisedProcess(t, os.Environ(), []string{"/bin/true"}, filepath.Join(dir, "process.log"), false)
	time.Sleep(100 * time.Millisecond)
	if err := stopSupervisedProcess(process, 10*time.Second); err == nil {
		t.Fatal("early clean exit unexpectedly passed")
	}
	data, err := os.ReadFile(filepath.Join(dir, "supervisor.log"))
	if err != nil || !strings.Contains(string(data), "before supervisor cleanup") {
		t.Fatalf("supervisor.log = %q, err=%v", data, err)
	}
}

func withoutEnvironment(environment []string, keys ...string) []string {
	blocked := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		blocked[key] = struct{}{}
	}
	filtered := make([]string, 0, len(environment))
	for _, entry := range environment {
		key, _, _ := strings.Cut(entry, "=")
		if _, found := blocked[key]; !found {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func TestFailureEvidenceRemainsInspectable(t *testing.T) {
	dir := t.TempDir()
	assertions := []string{"first boundary passed"}
	if err := writeFailureEvidence(dir, assertions, "process stopped\n", "calls=map[getUpdates:1]", map[string]string{"go": "fixture"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"manifest.json", "process.log", "telegram.log"} {
		if info, err := os.Stat(filepath.Join(dir, name)); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("failure artifact %s: info=%v err=%v", name, info, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest evidenceManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Status != "failed" || len(manifest.Assertions) != 1 || manifest.Assertions[0] != assertions[0] || manifest.Failure == "" {
		t.Fatalf("failure manifest = %#v", manifest)
	}
}

func TestArtifactHashIsStableAndContentSensitive(t *testing.T) {
	t.Parallel()
	const expected = "3a6eb0790f39ac87c94f3856b2dd2c5d110e6811602261a9a923d3bb23adc8b7"
	if got := sha256Hex([]byte("data")); got != expected {
		t.Fatalf("sha256Hex(data) = %q", got)
	}
	if sha256Hex([]byte("data")) == sha256Hex([]byte("different")) {
		t.Fatal("different evidence artifacts produced the same test hash")
	}
}

func TestTranscriptEscapesCaptionExceptGeneratedPreBlocks(t *testing.T) {
	page := renderTranscript(`<script>alert("unsafe")</script><pre>1. /tmp/file</pre>`, "terminal text", []string{"Enter"})
	if strings.Contains(page, "<script>") || !strings.Contains(page, `&lt;script&gt;alert(&#34;unsafe&#34;)&lt;/script&gt;`) {
		t.Fatalf("caption script was not escaped: %q", page)
	}
	if !strings.Contains(page, "<pre>1. /tmp/file</pre>") || !strings.Contains(page, "Terminal text alternative") {
		t.Fatalf("safe transcript structure was lost: %q", page)
	}
}
