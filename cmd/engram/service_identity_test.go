package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPublishServiceIdentityDescribesRunningProcessAndCleansOnlyItsOwnRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.identity")
	started := time.Date(2026, 8, 7, 12, 30, 0, 0, time.UTC)
	cleanup, err := publishServiceIdentity(path, "engram v2 commit=new", 4242, started)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"pid=4242", "build=engram v2 commit=new", "started=2026-08-07T12:30:00Z"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("identity omitted %q: %s", want, data)
		}
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("identity permissions: info=%v err=%v", info, err)
	}
	if err := os.WriteFile(path, []byte("pid=5252\nbuild=replacement\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cleanup()
	if data, err := os.ReadFile(path); err != nil || !strings.Contains(string(data), "replacement") {
		t.Fatalf("old process cleanup removed replacement identity: %q err=%v", data, err)
	}
}

func TestPublishServiceIdentityRejectsRelativePathAndMultilineBuild(t *testing.T) {
	if _, err := publishServiceIdentity("relative", "engram v1", 1, time.Now()); err == nil {
		t.Fatal("relative identity path accepted")
	}
	if _, err := publishServiceIdentity(filepath.Join(t.TempDir(), "identity"), "engram v1\nforged", 1, time.Now()); err == nil {
		t.Fatal("multiline build accepted")
	}
}
