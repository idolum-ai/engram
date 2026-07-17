package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessLogHardExitHelper$")
	cmd.Env = append(withoutEnvironment(os.Environ(), "ENGRAM_E2E_HARD_EXIT_DIR"), "ENGRAM_E2E_HARD_EXIT_DIR="+dir)
	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 23 {
		t.Fatalf("hard-exit helper error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "process.log"))
	if err != nil || string(data) != "output before hard exit\n" {
		t.Fatalf("hard-exit process.log = %q, err=%v", data, err)
	}
}

func TestProcessLogHardExitHelper(t *testing.T) {
	dir := os.Getenv("ENGRAM_E2E_HARD_EXIT_DIR")
	if dir == "" {
		t.Skip("subprocess helper")
	}
	var memory bytes.Buffer
	file, writer, err := openProcessLog(dir, &memory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(writer, "output before hard exit\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	os.Exit(23)
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
