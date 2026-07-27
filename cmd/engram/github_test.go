package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParsePermissionFlagsRejectsAmbiguousOrConflictingAuthority(t *testing.T) {
	if _, err := parsePermissionFlags([]string{"contents"}); err == nil {
		t.Fatal("permission without an access level was accepted")
	}
	if _, err := parsePermissionFlags([]string{"contents=read", "contents=write"}); err == nil {
		t.Fatal("conflicting permission levels were accepted")
	}
	got, err := parsePermissionFlags([]string{"contents=read", "pull_requests=write"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"contents": "read", "pull_requests": "write"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("permissions = %#v, want %#v", got, want)
	}
}

func TestGitHubChildEnvironmentReplacesAmbientGitHubCredentials(t *testing.T) {
	got := githubChildEnvironment([]string{
		"PATH=/usr/bin",
		"GH_TOKEN=ambient-gh",
		"GITHUB_TOKEN=ambient-github",
		"HOME=/tmp/home",
	}, "scoped-installation-token")
	want := []string{
		"PATH=/usr/bin",
		"HOME=/tmp/home",
		"GH_TOKEN=scoped-installation-token",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("child environment = %#v, want %#v", got, want)
	}
}

func TestSplitGitHubCommandRequiresExplicitBoundary(t *testing.T) {
	if _, _, ok := splitCommand([]string{"--app", "idolum", "gh"}); ok {
		t.Fatal("command without -- boundary was accepted")
	}
	flags, command, ok := splitCommand([]string{"--app", "idolum", "--", "gh", "pr", "view"})
	if !ok || !reflect.DeepEqual(flags, []string{"--app", "idolum"}) ||
		!reflect.DeepEqual(command, []string{"gh", "pr", "view"}) {
		t.Fatalf("split = %#v %#v %t", flags, command, ok)
	}
}

func TestReadPrivateKeyFileRequiresOwnerOnlyRegularFile(t *testing.T) {
	dir := t.TempDir()
	private := filepath.Join(dir, "private.pem")
	if err := os.WriteFile(private, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if data, err := readPrivateKeyFile(private); err != nil || string(data) != "private" {
		t.Fatalf("private file data = %q, error = %v", data, err)
	}

	public := filepath.Join(dir, "public.pem")
	if err := os.WriteFile(public, []byte("public"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateKeyFile(public); err == nil || !strings.Contains(err.Error(), "private regular file") {
		t.Fatalf("world-readable private key error = %v", err)
	}

	link := filepath.Join(dir, "linked.pem")
	if err := os.Symlink(private, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateKeyFile(link); err == nil {
		t.Fatal("symlinked private key was accepted")
	}
}
