package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/githubauth"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
)

func TestGitHubGrantApprovalStoresMemoryOnlyRenewalAuthority(t *testing.T) {
	now := time.Now()
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
		!strings.Contains(approvalMessage.text, request.Purpose) {
		t.Fatalf("approval text = %q", approvalMessage.text)
	}
	if status := app.handleGitHubApprovalCallback(context.Background(), telegram.CallbackQuery{
		ID: "approve", Message: &telegram.Message{MessageID: approvalID, Chat: telegram.Chat{ID: 100}},
	}, true, requestID); status != "callback_ok" {
		t.Fatalf("approval callback = %q", status)
	}
	response := <-responseChannel
	if !response.OK || len(response.Grants) != 1 || response.Token != "" {
		t.Fatalf("grant response = %#v", response)
	}
	if len(app.githubGrants) != 1 || len(app.githubLeases) != 0 {
		t.Fatalf("stored grants=%d leases=%d", len(app.githubGrants), len(app.githubLeases))
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
	second := app.handleGitHubBrokerRequest(context.Background(), request)
	if !second.OK || second.Token != first.Token || minter.mintCount() != 1 {
		t.Fatalf("lease reuse = %#v, mints=%d", second, minter.mintCount())
	}

	now = now.Add(56 * time.Minute)
	minter.mu.Lock()
	minter.expiresAt = now.Add(time.Hour)
	minter.mu.Unlock()
	rotated := app.handleGitHubBrokerRequest(context.Background(), request)
	if !rotated.OK || rotated.Token == first.Token || minter.mintCount() != 2 || minter.revokeCount() != 1 {
		t.Fatalf("rotation = %#v, mints=%d revokes=%d", rotated, minter.mintCount(), minter.revokeCount())
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
			ExpiresAt: now.Add(time.Minute), GrantID: "grant-one", Generation: 1,
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
	if minter.mintCount() != 1 || minter.revokeCount() != 1 {
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

func TestGitHubGrantInvalidatedDuringMintRevokesDiscardedToken(t *testing.T) {
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
	app.revokeGitHubBindingAuthority(context.Background(), sessionID, request.Binding)
	close(minter.mintRelease)
	response := <-responses
	if response.OK || !strings.Contains(response.Error, "grant changed") {
		t.Fatalf("post-mint invalidation response = %#v", response)
	}
	if len(app.githubLeases) != 0 || minter.revokeCount() != 1 {
		t.Fatalf("discarded lease count=%d revokes=%d", len(app.githubLeases), minter.revokeCount())
	}
}

func TestGitHubGrantRotationRevocationFailureKeepsOldLeaseTracked(t *testing.T) {
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
	if response.OK || !strings.Contains(response.Error, "revoke superseded") || minter.mintCount() != 0 {
		t.Fatalf("revocation failure response=%#v mints=%d", response, minter.mintCount())
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
