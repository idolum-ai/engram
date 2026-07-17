package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

func TestTranscriptEscapesCaptionExceptGeneratedPreBlocks(t *testing.T) {
	page := renderTranscript(`<script>alert("unsafe")</script><pre>1. /tmp/file</pre>`, "terminal text", []string{"Enter"})
	if strings.Contains(page, "<script>") || !strings.Contains(page, `&lt;script&gt;alert(&#34;unsafe&#34;)&lt;/script&gt;`) {
		t.Fatalf("caption script was not escaped: %q", page)
	}
	if !strings.Contains(page, "<pre>1. /tmp/file</pre>") || !strings.Contains(page, "Terminal text alternative") {
		t.Fatalf("safe transcript structure was lost: %q", page)
	}
}
