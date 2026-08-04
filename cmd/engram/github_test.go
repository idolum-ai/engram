package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/githubauth"
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

func TestRepeatedInstallationIDFlagAcceptsPositiveIDs(t *testing.T) {
	var values repeatedInt64Flag
	if err := values.Set("456"); err != nil {
		t.Fatal(err)
	}
	if err := values.Set("789"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual([]int64(values), []int64{456, 789}) {
		t.Fatalf("installation IDs = %#v", values)
	}
	if err := values.Set("0"); err == nil {
		t.Fatal("zero installation ID was accepted")
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

func TestGitHubBrokerWaitBudgetIncludesFullPostApprovalExchange(t *testing.T) {
	if githubBrokerRequestTimeout != githubauth.BrokerExchangeTimeout {
		t.Fatalf("CLI broker timeout = %s, want shared %s", githubBrokerRequestTimeout, githubauth.BrokerExchangeTimeout)
	}
	if reserve := githubBrokerRequestTimeout - githubauth.ApprovalTimeout; reserve < 2*time.Minute {
		t.Fatalf("post-approval exchange reserve = %s, want at least 2m", reserve)
	}
}

func TestGitHubStatusEnumeratesGrantCeilings(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	var output bytes.Buffer
	writeGitHubStatus(&output, githubauth.BrokerResponse{
		Grants: []githubauth.GrantInfo{{
			App:            "idolum",
			InstallationID: 789,
			Repositories:   []string{"idolum-ai/engram", "idolum-ai/agent-commons"},
			Permissions:    map[string]string{"pull_requests": "write", "contents": "read"},
			Purpose:        "Review the release",
			ExpiresAt:      now.Add(6 * time.Hour),
		}},
	}, now)
	text := output.String()
	for _, want := range []string{
		"installation: 789",
		"repo ceiling: idolum-ai/engram",
		"repo ceiling: idolum-ai/agent-commons",
		"permission ceiling: contents=read",
		"permission ceiling: pull_requests=write",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("status output %q does not contain %q", text, want)
		}
	}
	if strings.Index(text, "contents=read") > strings.Index(text, "pull_requests=write") {
		t.Fatalf("permission ceilings are not sorted: %q", text)
	}
}

func TestGitHubStatusJSONUsesDocumentedAuthorityObject(t *testing.T) {
	var output bytes.Buffer
	response := githubauth.BrokerResponse{
		Grants: []githubauth.GrantInfo{{ID: "grant-one", App: "idolum"}},
		Leases: []githubauth.LeaseInfo{{App: "idolum"}},
	}
	if err := writeGitHubStatusJSON(&output, response); err != nil {
		t.Fatal(err)
	}
	var document struct {
		Grants []githubauth.GrantInfo `json:"grants"`
		Leases []githubauth.LeaseInfo `json:"leases"`
	}
	if err := json.Unmarshal(output.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Grants) != 1 || document.Grants[0].ID != "grant-one" ||
		len(document.Leases) != 1 || document.Leases[0].App != "idolum" {
		t.Fatalf("status JSON = %#v", document)
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
