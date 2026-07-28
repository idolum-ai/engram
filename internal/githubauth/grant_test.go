package githubauth

import (
	"strings"
	"testing"
	"time"
)

func TestRenewableGrantRequestRequiresBoundedPurposeAndSafePermissions(t *testing.T) {
	request := BrokerRequest{
		Version:      ProtocolVersion,
		Action:       ActionGrant,
		App:          "idolum",
		Repositories: []string{"idolum-ai/engram"},
		Permissions:  map[string]string{"contents": "write", "actions": "read"},
		Binding:      Binding{ServerID: "server", WindowID: "@2", PaneID: "%3"},
		GrantFor:     6 * time.Hour,
		Purpose:      "Complete the current pull-request batch",
	}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	request.GrantFor = MinGrantDuration - time.Second
	if err := request.Validate(); err == nil {
		t.Fatal("short renewable grant was accepted")
	}
	request.GrantFor = time.Hour
	request.Purpose = strings.Repeat("x", 201)
	if err := request.Validate(); err == nil {
		t.Fatal("oversized purpose was accepted")
	}
	request.Purpose = "administer the repository"
	request.Permissions = map[string]string{"administration": "write"}
	if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "ineligible") {
		t.Fatalf("sensitive renewable permission error = %v", err)
	}
}

func TestCompactGrantLineIsDistinctFromTokenLease(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	line := CompactGrantLine(GrantInfo{
		App:          "idolum",
		Repositories: []string{"idolum-ai/engram"},
		Permissions:  map[string]string{"actions": "read", "contents": "write"},
		ExpiresAt:    now.Add(5*time.Hour + 42*time.Minute),
	}, now)
	if line != "GH grant idolum · 1R 1W · 1 repo · 5h42m" {
		t.Fatalf("grant line = %q", line)
	}
}
