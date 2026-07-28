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
	if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("sensitive renewable permission error = %v", err)
	}
}

func TestRenewableGrantPurposeRejectsInvisibleAndLineBreakingUnicode(t *testing.T) {
	for name, character := range map[string]string{
		"control":             "\u0007",
		"bidi override":       "\u202e",
		"zero width":          "\u200b",
		"line separator":      "\u2028",
		"paragraph separator": "\u2029",
	} {
		t.Run(name, func(t *testing.T) {
			request := renewableGrantTestRequest()
			request.Purpose = "Review " + character + " changes"
			request.Normalize()
			if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "unsafe Unicode") {
				t.Fatalf("purpose %q error = %v", request.Purpose, err)
			}
		})
	}
}

func TestRenewableGrantWritePermissionAllowlist(t *testing.T) {
	for _, permission := range []string{
		"checks",
		"contents",
		"discussions",
		"issues",
		"pull_requests",
		"repository_projects",
		"statuses",
	} {
		t.Run("allows_"+permission, func(t *testing.T) {
			request := renewableGrantTestRequest()
			request.Permissions = map[string]string{permission: "write"}
			if err := request.Validate(); err != nil {
				t.Fatalf("%s=write error = %v", permission, err)
			}
		})
	}

	for _, permission := range []string{
		"actions",
		"administration",
		"codespaces_lifecycle_admin",
		"codespaces_secrets",
		"deploy_keys",
		"deployments",
		"environments",
		"members",
		"organization_administration",
		"organization_hooks",
		"organization_personal_access_tokens",
		"repository_custom_properties",
		"secrets",
		"variables",
		"webhooks",
		"workflows",
	} {
		t.Run("denies_"+permission, func(t *testing.T) {
			request := renewableGrantTestRequest()
			request.Permissions = map[string]string{permission: "write"}
			if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "allowlist") {
				t.Fatalf("%s=write error = %v", permission, err)
			}
		})
	}

	request := renewableGrantTestRequest()
	request.Permissions = map[string]string{"future_repository_surface": "read"}
	if err := request.Validate(); err != nil {
		t.Fatalf("unknown read permission error = %v", err)
	}
}

func renewableGrantTestRequest() BrokerRequest {
	return BrokerRequest{
		Version:      ProtocolVersion,
		Action:       ActionGrant,
		App:          "idolum",
		Repositories: []string{"idolum-ai/engram"},
		Permissions:  map[string]string{"contents": "read"},
		Binding:      Binding{ServerID: "server", WindowID: "@2", PaneID: "%3"},
		GrantFor:     time.Hour,
		Purpose:      "Review the current change",
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
