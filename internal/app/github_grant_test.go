package app

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/githubauth"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
)

func TestGitHubGrantApprovalStoresMemoryOnlyRenewalAuthority(t *testing.T) {
	now := time.Now().UTC()
	expectedExpiry := now.Add(6 * time.Hour)
	app, transport, _ := newLocalGitHubApprovalTestApp(t, &fakeGitHubMinter{})
	app.githubGrants = map[string]githubGrant{}
	app.githubNow = func() time.Time { return now }
	request := testLocalGitHubBrokerRequest()
	request.Action = githubauth.ActionGrant
	request.Command = nil
	request.GrantFor = 6 * time.Hour
	request.Purpose = "Complete the current pull-request batch"
	responseChannel := make(chan githubauth.BrokerResponse, 1)
	go func() {
		responseChannel <- app.handleGitHubBrokerRequest(context.Background(), request)
	}()
	requestID, approvalID := pendingGitHubTestIdentity(t, app)
	approvalMessage := <-transport.sent
	if !strings.Contains(approvalMessage.text, "Renewable GitHub work-session grant") ||
		!strings.Contains(approvalMessage.text, "unattended short-lived token rotation") ||
		!strings.Contains(approvalMessage.text, request.Purpose) ||
		!strings.Contains(approvalMessage.text, expectedExpiry.Local().Format("2006-01-02 15:04 MST")) {
		t.Fatalf("approval text = %q", approvalMessage.text)
	}
	now = now.Add(2 * time.Minute)
	if status := app.handleGitHubApprovalCallback(context.Background(), telegram.CallbackQuery{
		ID: "approve", Message: &telegram.Message{MessageID: approvalID, Chat: telegram.Chat{ID: 100}},
	}, true, requestID); status != "callback_ok" {
		t.Fatalf("approval callback = %q", status)
	}
	response := <-responseChannel
	if !response.OK || len(response.Grants) != 1 || response.Token != "" {
		t.Fatalf("grant response = %#v", response)
	}
	if !response.Grants[0].ExpiresAt.Equal(expectedExpiry) {
		t.Fatalf("grant expiry = %s, want approval boundary %s", response.Grants[0].ExpiresAt, expectedExpiry)
	}
	if len(app.githubGrants) != 1 || len(app.githubLeases) != 0 {
		t.Fatalf("stored grants=%d leases=%d", len(app.githubGrants), len(app.githubLeases))
	}
}

func TestGitHubGrantAndRenewedLeaseStayBoundToSelectedInstallation(t *testing.T) {
	minter := &fakeGitHubMinter{expiresAt: time.Now().UTC().Add(time.Hour)}
	app, transport, sessionID := newLocalGitHubApprovalTestApp(t, minter)
	app.GitHubVault = testGitHubVaultWithInstallations(t, false, 456, 789)
	app.githubGrants = map[string]githubGrant{}
	request := testLocalGitHubBrokerRequest()
	request.Action = githubauth.ActionGrant
	request.InstallationID = 789
	request.Command = nil
	request.GrantFor = time.Hour
	request.Purpose = "Review installation-scoped changes"

	responses := make(chan githubauth.BrokerResponse, 1)
	go func() { responses <- app.handleGitHubBrokerRequest(context.Background(), request) }()
	<-transport.sent
	requestID, approvalID := pendingGitHubTestIdentity(t, app)
	if status := app.handleGitHubApprovalCallback(context.Background(), telegram.CallbackQuery{
		ID:      "approve-installation-grant",
		Message: &telegram.Message{MessageID: approvalID, Chat: telegram.Chat{ID: 100}},
	}, true, requestID); status != "callback_ok" {
		t.Fatalf("approval callback = %q", status)
	}
	grantResponse := <-responses
	if !grantResponse.OK || len(grantResponse.Grants) != 1 || grantResponse.Grants[0].InstallationID != 789 {
		t.Fatalf("grant response = %#v", grantResponse)
	}
	app.expireGitHubLeases(time.Now())
	if len(app.githubGrants) != 1 {
		t.Fatal("background enrollment validation invalidated a non-primary installation grant")
	}

	execRequest := testLocalGitHubBrokerRequest()
	execRequest.InstallationID = 789
	execResponse := app.handleGitHubBrokerRequest(context.Background(), execRequest)
	if !execResponse.OK || execResponse.Token == "" {
		t.Fatalf("grant-backed exec response = %#v", execResponse)
	}
	leases := app.githubLeaseInfos(execRequest.Binding)
	if len(leases) != 1 || leases[0].InstallationID != 789 {
		t.Fatalf("grant-backed leases = %#v", leases)
	}

	session, ok := app.Store.FindSession(sessionID)
	if !ok {
		t.Fatal("session disappeared")
	}
	otherEnrollment, ok := app.GitHubVault.Get("idolum")
	if !ok {
		t.Fatal("enrollment disappeared")
	}
	otherEnrollment, err := otherEnrollment.SelectInstallation(456)
	if err != nil {
		t.Fatal(err)
	}
	otherRequest := execRequest
	otherRequest.InstallationID = 456
	if _, matched := app.matchingGitHubGrant(otherRequest, otherEnrollment, session); matched {
		t.Fatal("grant for installation 789 matched installation 456")
	}
}

func TestGitHubGrantRevokeWaitsForBlockedInspectionThenRemovesGrant(t *testing.T) {
	now := time.Now().UTC()
	expectedExpiry := now.Add(time.Hour)
	minter := &fakeGitHubMinter{
		inspectStart: make(chan struct{}),
		inspectWait:  make(chan struct{}),
	}
	app, transport, sessionID := newLocalGitHubApprovalTestApp(t, minter)
	app.githubGrants = map[string]githubGrant{}
	app.githubNow = func() time.Time { return now }
	request := testLocalGitHubBrokerRequest()
	request.Action = githubauth.ActionGrant
	request.Command = nil
	request.GrantFor = time.Hour
	request.Purpose = "Review"

	responses := make(chan githubauth.BrokerResponse, 1)
	go func() {
		responses <- app.handleGitHubBrokerRequest(context.Background(), request)
	}()
	<-transport.sent
	requestID, approvalID := pendingGitHubTestIdentity(t, app)
	if status := app.handleGitHubApprovalCallback(context.Background(), telegram.CallbackQuery{
		ID: "approve", Message: &telegram.Message{MessageID: approvalID, Chat: telegram.Chat{ID: 100}},
	}, true, requestID); status != "callback_ok" {
		t.Fatalf("approval callback = %q", status)
	}
	<-minter.inspectStart
	now = now.Add(5 * time.Minute)

	revoked := make(chan error, 1)
	go func() {
		revoked <- app.revokeGitHubBindingAuthority(context.Background(), sessionID, request.Binding)
	}()
	select {
	case err := <-revoked:
		t.Fatalf("revoke crossed an in-flight installation inspection: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(minter.inspectWait)
	response := <-responses
	if !response.OK || len(response.Grants) != 1 {
		t.Fatalf("grant response = %#v", response)
	}
	if !response.Grants[0].ExpiresAt.Equal(expectedExpiry) {
		t.Fatalf("post-inspection expiry = %s, want approval boundary %s", response.Grants[0].ExpiresAt, expectedExpiry)
	}
	if err := <-revoked; err != nil {
		t.Fatalf("serialized revoke = %v", err)
	}
	if len(app.githubGrants) != 0 || len(app.githubLeases) != 0 {
		t.Fatalf("revoke returned with grants=%d leases=%d", len(app.githubGrants), len(app.githubLeases))
	}
}

func TestGitHubGrantRejectsPurposeRequiringSecretRedaction(t *testing.T) {
	app, _, sessionID := newLocalGitHubApprovalTestApp(t, &fakeGitHubMinter{})
	app.githubGrants = map[string]githubGrant{}
	request := testLocalGitHubBrokerRequest()
	request.Action = githubauth.ActionGrant
	request.Command = nil
	request.GrantFor = time.Hour
	request.Purpose = "Use BOT_SECRET for this work"
	session, found := app.Store.FindSession(sessionID)
	if !found {
		t.Fatal("missing session")
	}
	pending, err := app.beginGitHubApproval(context.Background(), session, request, githubauth.App{})
	if pending != nil || err == nil || !strings.Contains(err.Error(), "secret material") {
		t.Fatalf("secret-bearing purpose pending=%#v error=%v", pending, err)
	}
}

func TestGitHubGrantConfiguredDurationCeilingFailsBeforeApproval(t *testing.T) {
	app, transport, _ := newLocalGitHubApprovalTestApp(t, &fakeGitHubMinter{})
	app.Config.GitHubGrantMaxDuration = time.Hour
	request := testLocalGitHubBrokerRequest()
	request.Action = githubauth.ActionGrant
	request.Command = nil
	request.GrantFor = 2 * time.Hour
	request.Purpose = "Review"
	response := app.handleGitHubBrokerRequest(context.Background(), request)
	if response.OK || !strings.Contains(response.Error, "configured maximum") {
		t.Fatalf("duration ceiling response = %#v", response)
	}
	select {
	case message := <-transport.sent:
		t.Fatalf("overlong request reached Telegram: %#v", message)
	default:
	}
}

func TestGitHubGrantConsumesSubsetsAndRotatesOneToken(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	minter := &fakeGitHubMinter{expiresAt: now.Add(time.Hour)}
	app, _, sessionID := newLocalGitHubApprovalTestApp(t, minter)
	app.githubGrants = map[string]githubGrant{}
	app.githubNow = func() time.Time { return now }
	request := testLocalGitHubBrokerRequest()
	request.Passphrase = nil
	privateKey, enrollment, err := app.GitHubVault.Unlock("idolum", []byte("correct horse battery staple"))
	if err != nil {
		t.Fatal(err)
	}
	app.storeGitHubGrant(githubGrant{
		SessionID:        sessionID,
		SessionCreatedAt: testGitHubSessionCreatedAt(app, sessionID),
		Binding:          request.Binding,
		Enrollment:       enrollment,
		Info: githubauth.GrantInfo{
			ID: "grant-one", App: "idolum",
			Repositories: []string{"idolum-ai/engram", "idolum-ai/agent-commons"},
			Permissions:  map[string]string{"contents": "write", "pull_requests": "write"},
			Purpose:      "Complete a pull-request batch",
			CreatedAt:    now, ExpiresAt: now.Add(6 * time.Hour),
		},
		PrivateKey: privateKey,
	})

	first := app.handleGitHubBrokerRequest(context.Background(), request)
	if !first.OK || first.Token == "" || minter.mintCount() != 1 {
		t.Fatalf("first grant consumption = %#v, mints=%d", first, minter.mintCount())
	}
	if len(minter.repositories) != 2 || minter.permissions["contents"] != "write" ||
		minter.permissions["pull_requests"] != "write" {
		t.Fatalf("mint did not use approved grant ceiling: repos=%v permissions=%v", minter.repositories, minter.permissions)
	}
	second := app.handleGitHubBrokerRequest(context.Background(), request)
	if !second.OK || second.Token != first.Token || minter.mintCount() != 1 {
		t.Fatalf("lease reuse = %#v, mints=%d", second, minter.mintCount())
	}

	now = now.Add(56 * time.Minute)
	stillActive := app.handleGitHubBrokerRequest(context.Background(), request)
	if !stillActive.OK || stillActive.Token != first.Token || minter.mintCount() != 1 || minter.revokeCount() != 0 {
		t.Fatalf("active lease was rotated: %#v, mints=%d revokes=%d", stillActive, minter.mintCount(), minter.revokeCount())
	}

	now = now.Add(5 * time.Minute)
	minter.mu.Lock()
	minter.expiresAt = now.Add(time.Hour)
	minter.mu.Unlock()
	rotated := app.handleGitHubBrokerRequest(context.Background(), request)
	if !rotated.OK || rotated.Token == first.Token || minter.mintCount() != 2 || minter.revokeCount() != 0 {
		t.Fatalf("rotation = %#v, mints=%d revokes=%d", rotated, minter.mintCount(), minter.revokeCount())
	}
}

func TestGitHubGrantDifferentSubsetsShareCeilingTokenWithoutRevocation(t *testing.T) {
	now := time.Now()
	minter := &fakeGitHubMinter{expiresAt: now.Add(time.Hour)}
	app, _, sessionID := newLocalGitHubApprovalTestApp(t, minter)
	app.githubGrants = map[string]githubGrant{}
	request := testLocalGitHubBrokerRequest()
	request.Passphrase = nil
	privateKey, enrollment, err := app.GitHubVault.Unlock("idolum", []byte("correct horse battery staple"))
	if err != nil {
		t.Fatal(err)
	}
	repositories := []string{"idolum-ai/engram", "idolum-ai/agent-commons"}
	permissions := map[string]string{"contents": "write", "pull_requests": "write"}
	app.storeGitHubGrant(githubGrant{
		SessionID: sessionID, SessionCreatedAt: testGitHubSessionCreatedAt(app, sessionID),
		Binding: request.Binding, Enrollment: enrollment,
		Info: githubauth.GrantInfo{
			ID: "grant-one", App: "idolum", Repositories: repositories,
			Permissions: permissions, Purpose: "Review", CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		},
		PrivateKey: privateKey,
	})

	firstRequest := request
	firstRequest.Repositories = []string{"idolum-ai/engram"}
	firstRequest.Permissions = map[string]string{"contents": "read"}
	secondRequest := request
	secondRequest.Repositories = []string{"idolum-ai/agent-commons"}
	secondRequest.Permissions = map[string]string{"pull_requests": "read"}

	responses := make(chan githubauth.BrokerResponse, 2)
	var wait sync.WaitGroup
	for _, subset := range []githubauth.BrokerRequest{firstRequest, secondRequest} {
		subset := subset
		wait.Add(1)
		go func() {
			defer wait.Done()
			responses <- app.handleGitHubBrokerRequest(context.Background(), subset)
		}()
	}
	wait.Wait()
	close(responses)
	var token string
	for response := range responses {
		if !response.OK || response.Token == "" {
			t.Fatalf("subset response = %#v", response)
		}
		if token == "" {
			token = response.Token
		} else if response.Token != token {
			t.Fatalf("different subsets received different tokens: %q and %q", token, response.Token)
		}
	}
	if minter.mintCount() != 1 || minter.revokeCount() != 0 {
		t.Fatalf("different subsets mints=%d revokes=%d", minter.mintCount(), minter.revokeCount())
	}
	if len(minter.repositories) != 2 || minter.permissions["contents"] != "write" ||
		minter.permissions["pull_requests"] != "write" {
		t.Fatalf("minted scope repos=%v permissions=%v", minter.repositories, minter.permissions)
	}
}

func TestGitHubGrantRejectsWideningAndAnotherPane(t *testing.T) {
	now := time.Now()
	app, _, sessionID := newLocalGitHubApprovalTestApp(t, &fakeGitHubMinter{})
	app.githubGrants = map[string]githubGrant{}
	request := testLocalGitHubBrokerRequest()
	request.Passphrase = nil
	privateKey, enrollment, err := app.GitHubVault.Unlock("idolum", []byte("correct horse battery staple"))
	if err != nil {
		t.Fatal(err)
	}
	app.storeGitHubGrant(githubGrant{
		SessionID: sessionID, SessionCreatedAt: testGitHubSessionCreatedAt(app, sessionID),
		Binding: request.Binding, Enrollment: enrollment,
		Info: githubauth.GrantInfo{
			ID: "grant-one", App: "idolum", Repositories: request.Repositories,
			Permissions: map[string]string{"contents": "read"}, Purpose: "Review",
			CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		},
		PrivateKey: privateKey,
	})
	request.Permissions = map[string]string{"contents": "write"}
	response := app.handleGitHubBrokerRequest(context.Background(), request)
	if response.ErrorCode != githubauth.ErrorCodeLocalPassphraseRequired {
		t.Fatalf("widened grant response = %#v", response)
	}
	request.Permissions = map[string]string{"contents": "read"}
	request.Repositories = []string{"idolum-ai/other"}
	response = app.handleGitHubBrokerRequest(context.Background(), request)
	if response.ErrorCode != githubauth.ErrorCodeLocalPassphraseRequired {
		t.Fatalf("other-repository response = %#v", response)
	}
}

func TestGitHubGrantConcurrentFirstUseMintsOnce(t *testing.T) {
	now := time.Now()
	minter := &fakeGitHubMinter{expiresAt: now.Add(time.Hour)}
	app, _, sessionID := newLocalGitHubApprovalTestApp(t, minter)
	app.githubGrants = map[string]githubGrant{}
	request := testLocalGitHubBrokerRequest()
	request.Passphrase = nil
	privateKey, enrollment, err := app.GitHubVault.Unlock("idolum", []byte("correct horse battery staple"))
	if err != nil {
		t.Fatal(err)
	}
	app.storeGitHubGrant(githubGrant{
		SessionID: sessionID, SessionCreatedAt: testGitHubSessionCreatedAt(app, sessionID),
		Binding: request.Binding, Enrollment: enrollment,
		Info: githubauth.GrantInfo{
			ID: "grant-one", App: "idolum", Repositories: request.Repositories,
			Permissions: request.Permissions, Purpose: "Review", CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		},
		PrivateKey: privateKey,
	})
	var wait sync.WaitGroup
	responses := make(chan githubauth.BrokerResponse, 2)
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			responses <- app.handleGitHubBrokerRequest(context.Background(), request)
		}()
	}
	wait.Wait()
	close(responses)
	for response := range responses {
		if !response.OK || response.Token == "" {
			t.Fatalf("concurrent response = %#v", response)
		}
	}
	if minter.mintCount() != 1 {
		t.Fatalf("concurrent first use minted %d tokens", minter.mintCount())
	}
}

func TestGitHubGrantConcurrentRotationProducesOneAuthoritativeLease(t *testing.T) {
	now := time.Now()
	minter := &fakeGitHubMinter{expiresAt: now.Add(time.Hour)}
	app, _, sessionID := newLocalGitHubApprovalTestApp(t, minter)
	app.githubGrants = map[string]githubGrant{}
	request := testLocalGitHubBrokerRequest()
	request.Passphrase = nil
	privateKey, enrollment, err := app.GitHubVault.Unlock("idolum", []byte("correct horse battery staple"))
	if err != nil {
		t.Fatal(err)
	}
	app.storeGitHubGrant(githubGrant{
		SessionID: sessionID, SessionCreatedAt: testGitHubSessionCreatedAt(app, sessionID),
		Binding: request.Binding, Enrollment: enrollment,
		Info: githubauth.GrantInfo{
			ID: "grant-one", App: "idolum", Repositories: request.Repositories,
			Permissions: request.Permissions, Purpose: "Review", CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		},
		PrivateKey: privateKey,
	})
	app.githubLeases[githubBindingKey(request.Binding)] = githubLease{
		SessionID: sessionID, Binding: request.Binding, Enrollment: enrollment,
		Info: githubauth.LeaseInfo{
			App: "idolum", Repositories: request.Repositories, Permissions: request.Permissions,
			ExpiresAt: now.Add(-time.Second), GrantID: "grant-one", Generation: 1,
		},
		Token: "old-token",
	}
	var wait sync.WaitGroup
	responses := make(chan githubauth.BrokerResponse, 2)
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			responses <- app.handleGitHubBrokerRequest(context.Background(), request)
		}()
	}
	wait.Wait()
	close(responses)
	var authoritative string
	for response := range responses {
		if !response.OK || response.Token == "" {
			t.Fatalf("concurrent rotation response = %#v", response)
		}
		if authoritative == "" {
			authoritative = response.Token
		} else if response.Token != authoritative {
			t.Fatalf("concurrent rotation returned two tokens: %q and %q", authoritative, response.Token)
		}
	}
	if minter.mintCount() != 1 || minter.revokeCount() != 0 {
		t.Fatalf("concurrent rotation mints=%d revokes=%d", minter.mintCount(), minter.revokeCount())
	}
}

func TestGitHubGrantExpiryErasesSigningCapabilityAndRevokesLease(t *testing.T) {
	now := time.Now()
	minter := &fakeGitHubMinter{}
	app, _, sessionID := newLocalGitHubApprovalTestApp(t, minter)
	app.githubGrants = map[string]githubGrant{}
	request := testLocalGitHubBrokerRequest()
	enrollment, found := app.GitHubVault.Get("idolum")
	if !found {
		t.Fatal("missing enrollment")
	}
	key := []byte("decrypted signing capability")
	app.githubGrants[githubBindingKey(request.Binding)] = githubGrant{
		SessionID: sessionID, SessionCreatedAt: testGitHubSessionCreatedAt(app, sessionID),
		Binding: request.Binding, Enrollment: enrollment,
		Info: githubauth.GrantInfo{
			ID: "grant-one", App: "idolum", Repositories: request.Repositories,
			Permissions: request.Permissions, Purpose: "Review", CreatedAt: now.Add(-time.Hour), ExpiresAt: now,
		},
		PrivateKey: key,
	}
	app.githubLeases[githubBindingKey(request.Binding)] = githubLease{
		SessionID: sessionID, Binding: request.Binding, Enrollment: enrollment,
		Info:  githubauth.LeaseInfo{App: "idolum", ExpiresAt: now.Add(time.Hour), GrantID: "grant-one"},
		Token: "token-secret",
	}
	app.expireGitHubLeases(now.Add(time.Second))
	if len(app.githubGrants) != 0 || len(app.githubLeases) != 0 {
		t.Fatalf("expired authority remains: grants=%d leases=%d", len(app.githubGrants), len(app.githubLeases))
	}
	for _, value := range key {
		if value != 0 {
			t.Fatal("expired grant retained decrypted signing capability")
		}
	}
	app.transferWG.Wait()
	if minter.revokeCount() != 1 {
		t.Fatalf("expired grant revoked %d tokens", minter.revokeCount())
	}
}

func TestGitHubGrantDerivedLeaseCannotOutliveGrantBetweenSweeps(t *testing.T) {
	now := time.Now()
	app, _, sessionID := newLocalGitHubApprovalTestApp(t, &fakeGitHubMinter{})
	app.githubGrants = map[string]githubGrant{}
	request := testLocalGitHubBrokerRequest()
	request.Passphrase = nil
	enrollment, _ := app.GitHubVault.Get("idolum")
	app.githubGrants[githubBindingKey(request.Binding)] = githubGrant{
		SessionID: sessionID, SessionCreatedAt: testGitHubSessionCreatedAt(app, sessionID),
		Binding: request.Binding, Enrollment: enrollment,
		Info: githubauth.GrantInfo{
			ID: "grant-one", App: "idolum", Repositories: request.Repositories,
			Permissions: request.Permissions, Purpose: "Review",
			CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(-time.Second),
		},
		PrivateKey: []byte("decrypted signing capability"),
	}
	app.githubLeases[githubBindingKey(request.Binding)] = githubLease{
		SessionID: sessionID, Binding: request.Binding, Enrollment: enrollment,
		Info: githubauth.LeaseInfo{
			App: "idolum", Repositories: request.Repositories, Permissions: request.Permissions,
			ExpiresAt: now.Add(time.Hour), GrantID: "grant-one", Generation: 1,
		},
		Token: "token-secret",
	}
	response := app.handleGitHubBrokerRequest(context.Background(), request)
	if response.OK || response.Token != "" || response.ErrorCode != githubauth.ErrorCodeLocalPassphraseRequired {
		t.Fatalf("expired grant lease response = %#v", response)
	}
}

func TestGitHubGrantEnrollmentRemovalErasesAuthority(t *testing.T) {
	now := time.Now()
	minter := &fakeGitHubMinter{}
	app, _, sessionID := newLocalGitHubApprovalTestApp(t, minter)
	app.githubGrants = map[string]githubGrant{}
	request := testLocalGitHubBrokerRequest()
	enrollment, _ := app.GitHubVault.Get("idolum")
	key := []byte("decrypted signing capability")
	app.githubGrants[githubBindingKey(request.Binding)] = githubGrant{
		SessionID: sessionID, SessionCreatedAt: testGitHubSessionCreatedAt(app, sessionID),
		Binding: request.Binding, Enrollment: enrollment,
		Info: githubauth.GrantInfo{
			ID: "grant-one", App: "idolum", Repositories: request.Repositories,
			Permissions: request.Permissions, Purpose: "Review", CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		},
		PrivateKey: key,
	}
	external, err := githubauth.OpenVault(app.GitHubVault.Path())
	if err != nil {
		t.Fatal(err)
	}
	if removed, err := external.Remove("idolum"); err != nil || !removed {
		t.Fatalf("remove enrollment = %t, %v", removed, err)
	}
	app.expireGitHubLeases(now.Add(time.Second))
	if len(app.githubGrants) != 0 {
		t.Fatal("enrollment removal retained renewable grant")
	}
	for _, value := range key {
		if value != 0 {
			t.Fatal("enrollment removal retained decrypted signing capability")
		}
	}
}

func TestGitHubGrantRejectsSameBindingAfterSessionReplacement(t *testing.T) {
	now := time.Now()
	app, _, sessionID := newLocalGitHubApprovalTestApp(t, &fakeGitHubMinter{})
	app.githubGrants = map[string]githubGrant{}
	request := testLocalGitHubBrokerRequest()
	enrollment, _ := app.GitHubVault.Get("idolum")
	key := []byte("decrypted signing capability")
	app.githubGrants[githubBindingKey(request.Binding)] = githubGrant{
		SessionID: sessionID, SessionCreatedAt: testGitHubSessionCreatedAt(app, sessionID),
		Binding: request.Binding, Enrollment: enrollment,
		Info: githubauth.GrantInfo{
			ID: "grant-one", App: "idolum", Repositories: request.Repositories,
			Permissions: request.Permissions, Purpose: "Review", CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		},
		PrivateKey: key,
	}
	if _, _, err := app.Store.UpdateSession(sessionID, func(session *state.TerminalSession) {
		session.CreatedAt = session.CreatedAt.Add(time.Second)
	}); err != nil {
		t.Fatal(err)
	}
	app.expireGitHubLeases(now.Add(time.Second))
	if len(app.githubGrants) != 0 {
		t.Fatal("replacement session with the same tmux IDs inherited the grant")
	}
	for _, value := range key {
		if value != 0 {
			t.Fatal("session replacement retained decrypted signing capability")
		}
	}
}

func TestGitHubGrantRevokeSerializesWithMintAndRemovesCommittedToken(t *testing.T) {
	now := time.Now()
	minter := &fakeGitHubMinter{
		expiresAt:   now.Add(time.Hour),
		mintStarted: make(chan struct{}),
		mintRelease: make(chan struct{}),
	}
	app, _, sessionID := newLocalGitHubApprovalTestApp(t, minter)
	app.githubGrants = map[string]githubGrant{}
	request := testLocalGitHubBrokerRequest()
	request.Passphrase = nil
	privateKey, enrollment, err := app.GitHubVault.Unlock("idolum", []byte("correct horse battery staple"))
	if err != nil {
		t.Fatal(err)
	}
	app.storeGitHubGrant(githubGrant{
		SessionID: sessionID, SessionCreatedAt: testGitHubSessionCreatedAt(app, sessionID),
		Binding: request.Binding, Enrollment: enrollment,
		Info: githubauth.GrantInfo{
			ID: "grant-one", App: "idolum", Repositories: request.Repositories,
			Permissions: request.Permissions, Purpose: "Review", CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		},
		PrivateKey: privateKey,
	})
	responses := make(chan githubauth.BrokerResponse, 1)
	go func() { responses <- app.handleGitHubBrokerRequest(context.Background(), request) }()
	<-minter.mintStarted
	revoked := make(chan error, 1)
	go func() {
		revoked <- app.revokeGitHubBindingAuthority(context.Background(), sessionID, request.Binding)
	}()
	select {
	case err := <-revoked:
		t.Fatalf("revoke crossed an in-flight mint: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(minter.mintRelease)
	response := <-responses
	if !response.OK || response.Token == "" {
		t.Fatalf("serialized mint response = %#v", response)
	}
	if err := <-revoked; err != nil {
		t.Fatalf("serialized revoke = %v", err)
	}
	if len(app.githubGrants) != 0 || len(app.githubLeases) != 0 || minter.revokeCount() != 1 {
		t.Fatalf("post-revoke grants=%d leases=%d revokes=%d", len(app.githubGrants), len(app.githubLeases), minter.revokeCount())
	}
}

func TestGitHubGrantRequesterDisconnectRollsBackMintedToken(t *testing.T) {
	now := time.Now()
	minter := &fakeGitHubMinter{expiresAt: now.Add(time.Hour)}
	app, _, sessionID := newLocalGitHubApprovalTestApp(t, minter)
	app.githubGrants = map[string]githubGrant{}
	request := testLocalGitHubBrokerRequest()
	request.Passphrase = nil
	privateKey, enrollment, err := app.GitHubVault.Unlock("idolum", []byte("correct horse battery staple"))
	if err != nil {
		t.Fatal(err)
	}
	app.storeGitHubGrant(githubGrant{
		SessionID: sessionID, SessionCreatedAt: testGitHubSessionCreatedAt(app, sessionID),
		Binding: request.Binding, Enrollment: enrollment,
		Info: githubauth.GrantInfo{
			ID: "grant-one", App: "idolum", Repositories: request.Repositories,
			Permissions: request.Permissions, Purpose: "Review", CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		},
		PrivateKey: privateKey,
	})

	dir, err := os.MkdirTemp("/tmp", "eg-app-delivery-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socketPath := filepath.Join(dir, "github.sock")
	server, err := githubauth.Listen(socketPath, app.handleGitHubBrokerRequest)
	if err != nil {
		t.Fatal(err)
	}
	serverCtx, stopServer := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(serverCtx) }()

	connection, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeGitHubGrantTestFrame(connection, request); err != nil {
		t.Fatal(err)
	}
	var response githubauth.BrokerResponse
	if err := readGitHubGrantTestFrame(connection, &response); err != nil {
		t.Fatal(err)
	}
	if !response.OK || !response.DeliveryPending || response.Token == "" {
		t.Fatalf("pre-commit response = %#v", response)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for minter.revokeCount() != 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if minter.revokeCount() != 1 {
		t.Fatal("disconnected requester token was not revoked")
	}
	if _, found := app.currentGitHubLease(request.Binding); found {
		t.Fatal("disconnected requester stored a GitHub lease")
	}
	stopServer()
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestGitHubGrantKeepsActiveLeaseUntilNaturalExpiry(t *testing.T) {
	now := time.Now()
	minter := &fakeGitHubMinter{expiresAt: now.Add(time.Hour), revokeErr: errors.New("network down")}
	app, _, sessionID := newLocalGitHubApprovalTestApp(t, minter)
	app.githubGrants = map[string]githubGrant{}
	request := testLocalGitHubBrokerRequest()
	request.Passphrase = nil
	privateKey, enrollment, err := app.GitHubVault.Unlock("idolum", []byte("correct horse battery staple"))
	if err != nil {
		t.Fatal(err)
	}
	app.storeGitHubGrant(githubGrant{
		SessionID: sessionID, SessionCreatedAt: testGitHubSessionCreatedAt(app, sessionID),
		Binding: request.Binding, Enrollment: enrollment,
		Info: githubauth.GrantInfo{
			ID: "grant-one", App: "idolum", Repositories: request.Repositories,
			Permissions: request.Permissions, Purpose: "Review", CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		},
		PrivateKey: privateKey,
	})
	app.githubLeases[githubBindingKey(request.Binding)] = githubLease{
		SessionID: sessionID, Binding: request.Binding, Enrollment: enrollment,
		Info: githubauth.LeaseInfo{
			App: "idolum", Repositories: request.Repositories, Permissions: request.Permissions,
			ExpiresAt: now.Add(time.Minute), GrantID: "grant-one", Generation: 1,
		},
		Token: "old-token",
	}
	response := app.handleGitHubBrokerRequest(context.Background(), request)
	if !response.OK || response.Token != "old-token" || minter.mintCount() != 0 || minter.revokeCount() != 0 {
		t.Fatalf("active lease response=%#v mints=%d revokes=%d", response, minter.mintCount(), minter.revokeCount())
	}
	if lease, found := app.currentGitHubLease(request.Binding); !found || lease.Token != "old-token" {
		t.Fatal("failed revocation lost track of the old token")
	}
}

func TestGitHubGrantExplicitRevokeFailureRemovesAuthorityAndTracksRetry(t *testing.T) {
	now := time.Now()
	minter := &fakeGitHubMinter{revokeErr: errors.New("network down")}
	app, _, sessionID := newLocalGitHubApprovalTestApp(t, minter)
	app.githubGrants = map[string]githubGrant{}
	app.githubRevocations = map[string]githubRevocation{}
	request := testLocalGitHubBrokerRequest()
	enrollment, _ := app.GitHubVault.Get("idolum")
	key := []byte("decrypted signing capability")
	app.githubGrants[githubBindingKey(request.Binding)] = githubGrant{
		SessionID: sessionID, SessionCreatedAt: testGitHubSessionCreatedAt(app, sessionID),
		Binding: request.Binding, Enrollment: enrollment,
		Info: githubauth.GrantInfo{
			ID: "grant-one", App: "idolum", Repositories: request.Repositories,
			Permissions: request.Permissions, Purpose: "Review", CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		},
		PrivateKey: key,
	}
	app.githubLeases[githubBindingKey(request.Binding)] = githubLease{
		SessionID: sessionID, Binding: request.Binding, Enrollment: enrollment,
		Info: githubauth.LeaseInfo{
			App: "idolum", ExpiresAt: now.Add(time.Hour), GrantID: "grant-one", Generation: 1,
		},
		Token: "token-secret",
	}
	err := app.revokeGitHubBindingAuthority(context.Background(), sessionID, request.Binding)
	if err == nil || !strings.Contains(err.Error(), "revocation is pending") {
		t.Fatalf("explicit revoke error = %v", err)
	}
	if len(app.githubGrants) != 0 || len(app.githubLeases) != 0 || app.githubRevocationCount() != 1 {
		t.Fatalf("post-revoke grants=%d leases=%d pending=%d", len(app.githubGrants), len(app.githubLeases), app.githubRevocationCount())
	}
	for _, value := range key {
		if value != 0 {
			t.Fatal("explicit revoke failure retained signing capability")
		}
	}
	minter.mu.Lock()
	minter.revokeErr = nil
	minter.mu.Unlock()
	app.retryGitHubRevocations(now.Add(10 * time.Second))
	app.transferWG.Wait()
	if app.githubRevocationCount() != 0 || minter.revokeCount() != 2 {
		t.Fatalf("recovered revocation pending=%d calls=%d", app.githubRevocationCount(), minter.revokeCount())
	}
}

func TestGitHubRevocationQueueAndTokenAdmissionAreBounded(t *testing.T) {
	app, _, _ := newLocalGitHubApprovalTestApp(t, &fakeGitHubMinter{})
	app.githubRevocations = map[string]githubRevocation{}
	expiresAt := app.githubTime().Add(time.Hour)
	for index := 0; index < maxTrackedGitHubTokens; index++ {
		token := fmt.Sprintf("pending-token-%03d", index)
		if !app.trackGitHubRevocation(token, 1, "idolum", expiresAt) {
			t.Fatalf("token %d was rejected before capacity", index)
		}
	}
	if app.trackGitHubRevocation("overflow-token", 1, "idolum", expiresAt) {
		t.Fatal("revocation queue accepted a token beyond its bound")
	}
	if got := app.githubRevocationCount(); got != maxTrackedGitHubTokens {
		t.Fatalf("pending revocations = %d, want %d", got, maxTrackedGitHubTokens)
	}
	if err := app.reserveGitHubTokenSlot(); err == nil || !strings.Contains(err.Error(), "token capacity") {
		t.Fatalf("full token budget reservation error = %v", err)
	}
}

func TestGitHubGrantShutdownErasesSigningCapability(t *testing.T) {
	app := &App{
		GitHubMinter: &fakeGitHubMinter{},
		githubGrants: map[string]githubGrant{
			"binding": {PrivateKey: []byte("signing-secret")},
		},
		githubLeases: map[string]githubLease{},
	}
	key := app.githubGrants["binding"].PrivateKey
	app.revokeAllGitHubLeases()
	if len(app.githubGrants) != 0 {
		t.Fatal("shutdown retained renewable grant")
	}
	for _, value := range key {
		if value != 0 {
			t.Fatal("shutdown retained signing capability")
		}
	}
}

func TestGitHubGrantStatusIsDistinctAndContainsNoSigningMaterial(t *testing.T) {
	now := time.Now()
	app, _, sessionID := newLocalGitHubApprovalTestApp(t, &fakeGitHubMinter{})
	app.githubGrants = map[string]githubGrant{}
	request := testLocalGitHubBrokerRequest()
	enrollment, _ := app.GitHubVault.Get("idolum")
	app.githubGrants[githubBindingKey(request.Binding)] = githubGrant{
		SessionID: sessionID, SessionCreatedAt: testGitHubSessionCreatedAt(app, sessionID),
		Binding: request.Binding, Enrollment: enrollment,
		Info: githubauth.GrantInfo{
			ID: "grant-one", App: "idolum", Repositories: request.Repositories,
			Permissions: request.Permissions, Purpose: "Review", CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		},
		PrivateKey: []byte("SECRET-SIGNING-MATERIAL"),
	}
	response := app.handleGitHubBrokerRequest(context.Background(), githubauth.BrokerRequest{
		Version: githubauth.ProtocolVersion, Action: githubauth.ActionStatus, Binding: request.Binding,
	})
	if !response.OK || len(response.Grants) != 1 || len(response.Leases) != 0 {
		t.Fatalf("status response = %#v", response)
	}
	if strings.Contains(response.Grants[0].Purpose, "SECRET") || strings.Contains(app.githubStatusLine(app.Store.Snapshot().TerminalSessions[0]), "SECRET") {
		t.Fatal("status exposed signing capability")
	}
}

func testGitHubSessionCreatedAt(app *App, sessionID int) time.Time {
	for _, session := range app.Store.Snapshot().TerminalSessions {
		if session.ID == sessionID {
			return session.CreatedAt
		}
	}
	return time.Time{}
}

func writeGitHubGrantTestFrame(writer io.Writer, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if _, err := writer.Write(header[:]); err != nil {
		return err
	}
	_, err = writer.Write(payload)
	return err
}

func readGitHubGrantTestFrame(reader io.Reader, value any) error {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return err
	}
	payload := make([]byte, binary.BigEndian.Uint32(header[:]))
	if _, err := io.ReadFull(reader, payload); err != nil {
		return err
	}
	return json.Unmarshal(payload, value)
}
