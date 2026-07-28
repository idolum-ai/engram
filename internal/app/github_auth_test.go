package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/githubauth"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
	"github.com/idolum-ai/engram/internal/tmux"
)

func TestGitHubCapabilityRemoteApprovalMintsLeaseAndDeletesPassphraseReply(t *testing.T) {
	app, _, sessionID := newSafetyApp(t, state.TerminalOriginAttached)
	app.Config.TelegramBotToken = "BOT_SECRET"
	app.GitHubVault = testGitHubVault(t, true)
	minter := &fakeGitHubMinter{expiresAt: time.Now().UTC().Add(42 * time.Minute)}
	app.GitHubMinter = minter
	app.githubPending = map[string]*githubPendingRequest{}
	app.githubLeases = map[string]githubLease{}
	app.githubNow = time.Now
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	app.runCtx = canceled

	transport := newGitHubTelegramTransport()
	client := telegram.New("BOT_SECRET")
	client.HTTPClient = &http.Client{Transport: transport}
	app.Telegram = client

	request := githubauth.BrokerRequest{
		Version:      githubauth.ProtocolVersion,
		Action:       githubauth.ActionExec,
		App:          "idolum",
		Repositories: []string{"idolum-ai/engram"},
		Permissions:  map[string]string{"contents": "read", "pull_requests": "write"},
		Command:      []string{"gh", "pr", "view", "49"},
		Binding:      githubauth.Binding{ServerID: appTestServerID, WindowID: "@1", PaneID: "%1"},
	}
	responseChannel := make(chan githubauth.BrokerResponse, 1)
	go func() {
		responseChannel <- app.handleGitHubBrokerRequest(context.Background(), request)
	}()

	approvalMessage := <-transport.sent
	if approvalMessage.id != 77 || !strings.Contains(approvalMessage.text, "pull_requests: write") ||
		!strings.Contains(approvalMessage.text, "not end-to-end encrypted") {
		t.Fatalf("approval message = %#v", approvalMessage)
	}
	requestID, approvalID := pendingGitHubTestIdentity(t, app)
	status := app.handleCallback(context.Background(), telegram.CallbackQuery{
		ID:   "callback-1",
		From: telegram.User{ID: 42},
		Message: &telegram.Message{
			MessageID: approvalID,
			Chat:      telegram.Chat{ID: 100},
		},
		Data: "github-approve:" + requestID,
	})
	if status != "callback_ok" {
		t.Fatalf("approval callback status = %q", status)
	}
	unlockPrompt := <-transport.sent
	if unlockPrompt.id != 78 || !unlockPrompt.forceReply {
		t.Fatalf("unlock prompt = %#v", unlockPrompt)
	}

	secret := "correct horse battery staple"
	status, handled := app.handleGitHubUnlockReply(context.Background(), telegram.Message{
		MessageID: 500,
		Chat:      telegram.Chat{ID: 100},
		Text:      secret,
		ReplyToMessage: &telegram.Message{
			MessageID: unlockPrompt.id,
			Chat:      telegram.Chat{ID: 100},
		},
	})
	if !handled || status != "github_unlock_received" {
		t.Fatalf("unlock reply = %q, %t", status, handled)
	}
	response := <-responseChannel
	if !response.OK || response.Token != "ghs_fake_token" {
		t.Fatalf("broker response = %#v", response)
	}
	if !transport.deletedMessage(500) || !transport.deletedMessage(78) {
		t.Fatalf("deleted messages = %#v", transport.deleted)
	}
	if minter.mintCalls != 1 || minter.repositories[0] != "idolum-ai/engram" ||
		minter.permissions["contents"] != "read" || minter.permissions["pull_requests"] != "write" {
		t.Fatalf("mint request = %#v", minter)
	}
	session, ok := app.Store.FindSession(sessionID)
	if !ok {
		t.Fatal("session disappeared")
	}
	if got := app.githubStatusLine(session); !strings.Contains(got, "GH idolum · 1R 1W · 1 repo") {
		t.Fatalf("capability line = %q", got)
	}
}

func TestGitHubCapabilityCancellationDuringMintRevokesTokenWithoutStoringLease(t *testing.T) {
	minter := &fakeGitHubMinter{
		expiresAt:   time.Now().UTC().Add(42 * time.Minute),
		mintStarted: make(chan struct{}),
		mintRelease: make(chan struct{}),
	}
	app, transport, _ := newLocalGitHubApprovalTestApp(t, minter)

	request := testLocalGitHubBrokerRequest()
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	responseChannel := make(chan githubauth.BrokerResponse, 1)
	go func() {
		responseChannel <- app.handleGitHubBrokerRequest(requestCtx, request)
	}()
	<-transport.sent
	requestID, approvalID := pendingGitHubTestIdentity(t, app)
	if status := app.handleCallback(context.Background(), telegram.CallbackQuery{
		ID: "callback-cancel-mint", From: telegram.User{ID: 42},
		Message: &telegram.Message{MessageID: approvalID, Chat: telegram.Chat{ID: 100}},
		Data:    "github-approve:" + requestID,
	}); status != "callback_ok" {
		t.Fatalf("approval callback status = %q", status)
	}
	<-minter.mintStarted
	cancelRequest()
	close(minter.mintRelease)

	response := <-responseChannel
	if response.OK || !strings.Contains(strings.ToLower(response.Error), "canceled") {
		t.Fatalf("broker response = %#v", response)
	}
	if len(app.githubLeases) != 0 {
		t.Fatal("canceled request stored a GitHub lease")
	}
	if minter.revokeCount() != 1 {
		t.Fatalf("revoke calls = %d, want 1", minter.revokeCount())
	}
}

func TestGitHubCapabilityBindingInvalidatedDuringMintRevokesTokenWithoutStoringLease(t *testing.T) {
	minter := &fakeGitHubMinter{
		expiresAt:   time.Now().UTC().Add(42 * time.Minute),
		mintStarted: make(chan struct{}),
		mintRelease: make(chan struct{}),
	}
	app, transport, sessionID := newLocalGitHubApprovalTestApp(t, minter)

	responseChannel := make(chan githubauth.BrokerResponse, 1)
	go func() {
		responseChannel <- app.handleGitHubBrokerRequest(context.Background(), testLocalGitHubBrokerRequest())
	}()
	<-transport.sent
	requestID, approvalID := pendingGitHubTestIdentity(t, app)
	if status := app.handleCallback(context.Background(), telegram.CallbackQuery{
		ID: "callback-invalidate-mint", From: telegram.User{ID: 42},
		Message: &telegram.Message{MessageID: approvalID, Chat: telegram.Chat{ID: 100}},
		Data:    "github-approve:" + requestID,
	}); status != "callback_ok" {
		t.Fatalf("approval callback status = %q", status)
	}
	<-minter.mintStarted
	if _, _, err := app.Store.UpdateSession(sessionID, func(session *state.TerminalSession) {
		session.WatchEnabled = false
	}); err != nil {
		t.Fatal(err)
	}
	close(minter.mintRelease)

	response := <-responseChannel
	if response.OK || !strings.Contains(response.Error, "not an active Engram session") {
		t.Fatalf("broker response = %#v", response)
	}
	if len(app.githubLeases) != 0 {
		t.Fatal("invalidated pane stored a GitHub lease")
	}
	if minter.revokeCount() != 1 {
		t.Fatalf("revoke calls = %d, want 1", minter.revokeCount())
	}
}

func TestGitHubCapabilityRevalidatesBindingAfterApprovalBeforeMint(t *testing.T) {
	minter := &fakeGitHubMinter{expiresAt: time.Now().UTC().Add(42 * time.Minute)}
	app, transport, sessionID := newLocalGitHubApprovalTestApp(t, minter)

	responseChannel := make(chan githubauth.BrokerResponse, 1)
	go func() {
		responseChannel <- app.handleGitHubBrokerRequest(context.Background(), testLocalGitHubBrokerRequest())
	}()
	<-transport.sent
	requestID, approvalID := pendingGitHubTestIdentity(t, app)
	if _, _, err := app.Store.UpdateSession(sessionID, func(session *state.TerminalSession) {
		session.WatchEnabled = false
	}); err != nil {
		t.Fatal(err)
	}
	if status := app.handleCallback(context.Background(), telegram.CallbackQuery{
		ID: "callback-invalidate-approved", From: telegram.User{ID: 42},
		Message: &telegram.Message{MessageID: approvalID, Chat: telegram.Chat{ID: 100}},
		Data:    "github-approve:" + requestID,
	}); status != "callback_ok" {
		t.Fatalf("approval callback status = %q", status)
	}

	response := <-responseChannel
	if response.OK || !strings.Contains(response.Error, "not an active Engram session") {
		t.Fatalf("broker response = %#v", response)
	}
	if minter.mintCount() != 0 {
		t.Fatalf("mint calls = %d, want 0", minter.mintCount())
	}
}

func TestGitHubCapabilityReusesOnlySubsetAndRevokesPaneLease(t *testing.T) {
	app, _, sessionID := newSafetyApp(t, state.TerminalOriginAttached)
	app.githubPending = map[string]*githubPendingRequest{}
	app.githubLeases = map[string]githubLease{}
	app.GitHubMinter = &fakeGitHubMinter{}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	app.runCtx = canceled
	binding := githubauth.Binding{ServerID: appTestServerID, WindowID: "@1", PaneID: "%1"}
	enrollment := githubauth.App{
		Alias:             "idolum",
		AppID:             123,
		InstallationID:    456,
		PublicFingerprint: "fingerprint",
		CreatedAt:         time.Unix(1_700_000_000, 0).UTC(),
	}
	app.githubLeases[githubBindingKey(binding)] = githubLease{
		SessionID:  sessionID,
		Binding:    binding,
		Enrollment: enrollment,
		Info: githubauth.LeaseInfo{
			App:          "idolum",
			Repositories: []string{"idolum-ai/engram", "idolum-ai/grimoire"},
			Permissions:  map[string]string{"contents": "read", "pull_requests": "write"},
			ExpiresAt:    time.Now().Add(time.Hour),
		},
		Token: "secret-token",
	}
	subset := githubauth.BrokerRequest{
		App:          "idolum",
		Repositories: []string{"idolum-ai/engram"},
		Permissions:  map[string]string{"pull_requests": "read"},
		Binding:      binding,
	}
	if lease, ok := app.reusableGitHubLease(subset, enrollment); !ok || lease.Token != "secret-token" {
		t.Fatal("same-pane subset did not reuse the lease")
	}
	subset.Permissions["contents"] = "write"
	if _, ok := app.reusableGitHubLease(subset, enrollment); ok {
		t.Fatal("broader write permission reused the lease")
	}
	app.revokeGitHubBindingLeases(context.Background(), sessionID, binding)
	if len(app.githubLeases) != 0 {
		t.Fatal("pane lease survived revocation")
	}
	if app.GitHubMinter.(*fakeGitHubMinter).revokeCalls != 1 {
		t.Fatal("GitHub token was not revoked")
	}
}

func TestGitHubCapabilityUnavailableVaultZeroesSuppliedPassphrase(t *testing.T) {
	app, _, _ := newSafetyApp(t, state.TerminalOriginAttached)
	request := testLocalGitHubBrokerRequest()
	passphrase := request.Passphrase

	response := app.handleGitHubBrokerRequest(context.Background(), request)
	if response.OK || !strings.Contains(response.Error, "capabilities are unavailable") {
		t.Fatalf("broker response = %#v", response)
	}
	requireZeroedGitHubPassphrase(t, passphrase)
}

func TestGitHubCapabilityBindingRejectionZeroesSuppliedPassphrase(t *testing.T) {
	app, _, _ := newSafetyApp(t, state.TerminalOriginAttached)
	app.GitHubVault = testGitHubVault(t, false)
	request := testLocalGitHubBrokerRequest()
	request.Binding.PaneID = "%999"
	passphrase := request.Passphrase

	response := app.handleGitHubBrokerRequest(context.Background(), request)
	if response.OK || !strings.Contains(response.Error, "not an active Engram session") {
		t.Fatalf("broker response = %#v", response)
	}
	requireZeroedGitHubPassphrase(t, passphrase)
}

func TestGitHubCapabilityReuseZeroesSuppliedPassphrase(t *testing.T) {
	app, _, sessionID := newSafetyApp(t, state.TerminalOriginAttached)
	app.GitHubVault = testGitHubVault(t, false)
	app.GitHubMinter = &fakeGitHubMinter{}
	app.githubPending = map[string]*githubPendingRequest{}
	app.githubLeases = map[string]githubLease{}
	enrollment, found := app.GitHubVault.Get("idolum")
	if !found {
		t.Fatal("test enrollment missing")
	}
	request := testLocalGitHubBrokerRequest()
	app.githubLeases[githubBindingKey(request.Binding)] = githubLease{
		SessionID:  sessionID,
		Binding:    request.Binding,
		Enrollment: enrollment,
		Info: githubauth.LeaseInfo{
			App:          request.App,
			Repositories: append([]string(nil), request.Repositories...),
			Permissions:  copyStringMap(request.Permissions),
			ExpiresAt:    time.Now().Add(time.Hour),
		},
		Token: "reused-token",
	}
	passphrase := request.Passphrase

	response := app.handleGitHubBrokerRequest(context.Background(), request)
	if !response.OK || response.Token != "reused-token" {
		t.Fatalf("broker response = %#v", response)
	}
	requireZeroedGitHubPassphrase(t, passphrase)
}

func TestGitHubCapabilityRevalidatesBindingBeforeReturningReusedLease(t *testing.T) {
	app, runner, sessionID := newSafetyApp(t, state.TerminalOriginAttached)
	app.GitHubVault = testGitHubVault(t, false)
	app.GitHubMinter = &fakeGitHubMinter{}
	app.githubPending = map[string]*githubPendingRequest{}
	app.githubLeases = map[string]githubLease{}
	app.githubNow = time.Now
	enrollment, found := app.GitHubVault.Get("idolum")
	if !found {
		t.Fatal("test enrollment missing")
	}
	request := testLocalGitHubBrokerRequest()
	app.githubLeases[githubBindingKey(request.Binding)] = githubLease{
		SessionID:  sessionID,
		Binding:    request.Binding,
		Enrollment: enrollment,
		Info: githubauth.LeaseInfo{
			App:          request.App,
			Repositories: append([]string(nil), request.Repositories...),
			Permissions:  copyStringMap(request.Permissions),
			ExpiresAt:    time.Now().Add(time.Hour),
		},
		Token: "reused-token",
	}
	runner.onIdentity = func() {
		runner.onIdentity = nil
		if _, _, err := app.Store.UpdateSession(sessionID, func(session *state.TerminalSession) {
			session.WatchEnabled = false
		}); err != nil {
			t.Fatal(err)
		}
	}

	response := app.handleGitHubBrokerRequest(context.Background(), request)
	if response.OK || !strings.Contains(response.Error, "not an active Engram session") {
		t.Fatalf("broker response = %#v", response)
	}
	if len(app.githubLeases) != 0 {
		t.Fatal("invalidated reused lease remained active")
	}
	if app.GitHubMinter.(*fakeGitHubMinter).revokeCount() != 1 {
		t.Fatal("invalidated reused token was not revoked")
	}
}

func TestGitHubCapabilityRejectsEnrollmentRemovedDuringMint(t *testing.T) {
	minter := &fakeGitHubMinter{
		expiresAt:   time.Now().UTC().Add(42 * time.Minute),
		mintStarted: make(chan struct{}),
		mintRelease: make(chan struct{}),
	}
	app, transport, _ := newLocalGitHubApprovalTestApp(t, minter)

	responseChannel := make(chan githubauth.BrokerResponse, 1)
	go func() {
		responseChannel <- app.handleGitHubBrokerRequest(context.Background(), testLocalGitHubBrokerRequest())
	}()
	<-transport.sent
	requestID, approvalID := pendingGitHubTestIdentity(t, app)
	if status := app.handleCallback(context.Background(), telegram.CallbackQuery{
		ID: "callback-remove-during-mint", From: telegram.User{ID: 42},
		Message: &telegram.Message{MessageID: approvalID, Chat: telegram.Chat{ID: 100}},
		Data:    "github-approve:" + requestID,
	}); status != "callback_ok" {
		t.Fatalf("approval callback status = %q", status)
	}
	<-minter.mintStarted

	external, err := githubauth.OpenVault(app.GitHubVault.Path())
	if err != nil {
		t.Fatal(err)
	}
	if removed, err := external.Remove("idolum"); err != nil || !removed {
		t.Fatalf("remove enrollment = %t, %v", removed, err)
	}
	close(minter.mintRelease)

	response := <-responseChannel
	if response.OK || !strings.Contains(response.Error, "no longer enrolled") {
		t.Fatalf("broker response = %#v", response)
	}
	if len(app.githubLeases) != 0 {
		t.Fatal("mint-time enrollment removal stored a GitHub lease")
	}
	if minter.revokeCount() != 1 {
		t.Fatalf("revoke calls = %d, want 1", minter.revokeCount())
	}
}

func TestGitHubCapabilityEnrollmentRemovalSweepsExactLease(t *testing.T) {
	now := time.Now()
	minter := &fakeGitHubMinter{}
	app, _, sessionID := newLocalGitHubApprovalTestApp(t, minter)
	request := testLocalGitHubBrokerRequest()
	enrollment, found := app.GitHubVault.Get(request.App)
	if !found {
		t.Fatal("test enrollment missing")
	}
	app.githubLeases[githubBindingKey(request.Binding)] = githubLease{
		SessionID:  sessionID,
		Binding:    request.Binding,
		Enrollment: enrollment,
		Info: githubauth.LeaseInfo{
			App:          request.App,
			Repositories: request.Repositories,
			Permissions:  request.Permissions,
			ExpiresAt:    now.Add(time.Hour),
		},
		Token: "exact-lease-token",
	}
	external, err := githubauth.OpenVault(app.GitHubVault.Path())
	if err != nil {
		t.Fatal(err)
	}
	if removed, err := external.Remove(request.App); err != nil || !removed {
		t.Fatalf("remove enrollment = %t, %v", removed, err)
	}

	app.expireGitHubLeases(now.Add(time.Second))
	app.transferWG.Wait()

	if len(app.githubLeases) != 0 {
		t.Fatal("enrollment removal retained exact-command lease")
	}
	if minter.revokeCount() != 1 {
		t.Fatalf("revocations = %d, want 1", minter.revokeCount())
	}
}

func TestGitHubCapabilityRejectsSameKeyInstallationRetargetBeforeLeaseReuse(t *testing.T) {
	app, runner, sessionID := newSafetyApp(t, state.TerminalOriginAttached)
	app.GitHubVault = testGitHubVault(t, false)
	app.GitHubMinter = &fakeGitHubMinter{}
	app.githubPending = map[string]*githubPendingRequest{}
	app.githubLeases = map[string]githubLease{}
	app.githubNow = time.Now
	enrollment, found := app.GitHubVault.Get("idolum")
	if !found {
		t.Fatal("test enrollment missing")
	}
	passphrase := []byte("correct horse battery staple")
	privateKey, _, err := app.GitHubVault.Unlock("idolum", passphrase)
	if err != nil {
		t.Fatal(err)
	}
	defer githubauth.Zero(privateKey)
	external, err := githubauth.OpenVault(app.GitHubVault.Path())
	if err != nil {
		t.Fatal(err)
	}

	request := testLocalGitHubBrokerRequest()
	app.githubLeases[githubBindingKey(request.Binding)] = githubLease{
		SessionID:  sessionID,
		Binding:    request.Binding,
		Enrollment: enrollment,
		Info: githubauth.LeaseInfo{
			App:          request.App,
			Repositories: append([]string(nil), request.Repositories...),
			Permissions:  copyStringMap(request.Permissions),
			ExpiresAt:    time.Now().Add(time.Hour),
		},
		Token: "old-installation-token",
	}
	identityChecks := 0
	runner.onIdentity = func() {
		identityChecks++
		if identityChecks != 2 {
			return
		}
		replacement, _, err := external.Add(
			"idolum",
			enrollment.AppID,
			enrollment.InstallationID+1,
			privateKey,
			passphrase,
			enrollment.TelegramUnlock,
		)
		if err != nil {
			t.Fatal(err)
		}
		if replacement.PublicFingerprint != enrollment.PublicFingerprint {
			t.Fatal("same private key produced a different public fingerprint")
		}
	}

	response := app.handleGitHubBrokerRequest(context.Background(), request)
	if response.OK || !strings.Contains(response.Error, "enrollment changed") {
		t.Fatalf("broker response = %#v", response)
	}
	if len(app.githubLeases) != 0 {
		t.Fatal("retargeted enrollment retained its old installation lease")
	}
	if app.GitHubMinter.(*fakeGitHubMinter).revokeCount() != 1 {
		t.Fatal("retargeted installation token was not revoked")
	}
}

func TestGitHubCapabilityRejectsEnrollmentRemovedDuringApproval(t *testing.T) {
	minter := &fakeGitHubMinter{expiresAt: time.Now().UTC().Add(42 * time.Minute)}
	app, transport, _ := newLocalGitHubApprovalTestApp(t, minter)

	responseChannel := make(chan githubauth.BrokerResponse, 1)
	go func() {
		responseChannel <- app.handleGitHubBrokerRequest(context.Background(), testLocalGitHubBrokerRequest())
	}()
	<-transport.sent
	requestID, approvalID := pendingGitHubTestIdentity(t, app)

	external, err := githubauth.OpenVault(app.GitHubVault.Path())
	if err != nil {
		t.Fatal(err)
	}
	if removed, err := external.Remove("idolum"); err != nil || !removed {
		t.Fatalf("remove enrollment = %t, %v", removed, err)
	}
	status := app.handleCallback(context.Background(), telegram.CallbackQuery{
		ID: "callback-enrollment-removed", From: telegram.User{ID: 42},
		Message: &telegram.Message{MessageID: approvalID, Chat: telegram.Chat{ID: 100}},
		Data:    "github-approve:" + requestID,
	})
	if status != "callback_user_error" {
		t.Fatalf("approval callback status = %q", status)
	}
	response := <-responseChannel
	if response.OK || !strings.Contains(response.Error, "no longer enrolled") {
		t.Fatalf("broker response = %#v", response)
	}
	if minter.mintCount() != 0 {
		t.Fatalf("mint calls = %d, want 0", minter.mintCount())
	}
}

func TestGitHubUnlockReplyCannotSatisfyDifferentPrompt(t *testing.T) {
	app := &App{
		Config:        configForGitHubTest(),
		Telegram:      telegram.New("TOKEN"),
		githubPending: map[string]*githubPendingRequest{},
		githubLeases:  map[string]githubLease{},
	}
	app.githubPending["one"] = &githubPendingRequest{
		ID: "one", UnlockMessageID: 70, State: "unlocking",
		ExpiresAt: time.Now().Add(time.Minute), Result: make(chan githubApproval, 1),
	}
	if _, handled := app.handleGitHubUnlockReply(context.Background(), telegram.Message{
		MessageID: 80, Chat: telegram.Chat{ID: 100}, Text: "password",
		ReplyToMessage: &telegram.Message{MessageID: 71},
	}); handled {
		t.Fatal("reply to another message satisfied the unlock request")
	}
}

func TestGitHubCanceledUnlockPromptConsumesLatePassphraseReply(t *testing.T) {
	transport := newGitHubTelegramTransport()
	client := telegram.New("TOKEN")
	client.HTTPClient = &http.Client{Transport: transport}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	app := &App{
		Config:                 configForGitHubTest(),
		Telegram:               client,
		runCtx:                 canceled,
		githubPending:          map[string]*githubPendingRequest{},
		githubLeases:           map[string]githubLease{},
		githubUnlockTombstones: map[int]time.Time{},
	}
	pending := &githubPendingRequest{
		ID:              "canceled",
		SessionID:       3,
		UnlockMessageID: 70,
		State:           "unlocking",
		ExpiresAt:       time.Now().Add(time.Minute),
		Result:          make(chan githubApproval, 1),
	}
	app.githubPending[pending.ID] = pending
	app.finishGitHubPending(pending)
	if !transport.deletedMessage(70) {
		t.Fatal("canceled ForceReply prompt was not deleted")
	}

	status, handled := app.handleGitHubUnlockReply(context.Background(), telegram.Message{
		MessageID: 80,
		Chat:      telegram.Chat{ID: 100},
		Text:      "late secret",
		ReplyToMessage: &telegram.Message{
			MessageID: 70,
		},
	})
	if !handled || status != "github_unlock_expired" {
		t.Fatalf("late unlock reply = %q, %t", status, handled)
	}
	if !transport.deletedMessage(80) {
		t.Fatal("late passphrase reply was not deleted")
	}
}

func TestGitHubApprovalUsesLocalUnlockOverrideWithoutTelegramPassphrasePrompt(t *testing.T) {
	transport := newGitHubTelegramTransport()
	client := telegram.New("TOKEN")
	client.HTTPClient = &http.Client{Transport: transport}
	app := &App{
		Config:        configForGitHubTest(),
		Telegram:      client,
		GitHubVault:   testGitHubVault(t, true),
		githubPending: map[string]*githubPendingRequest{},
		githubLeases:  map[string]githubLease{},
	}
	enrollment, found := app.GitHubVault.Get("idolum")
	if !found {
		t.Fatal("test enrollment missing")
	}
	passphrase := []byte("correct horse battery staple")
	pending := &githubPendingRequest{
		ID:                "local-unlock",
		Request:           githubauth.BrokerRequest{App: "idolum"},
		LocalPassphrase:   append([]byte(nil), passphrase...),
		ExpiresAt:         time.Now().Add(time.Minute),
		ApprovalMessageID: 77,
		State:             "pending",
		Result:            make(chan githubApproval, 1),
		Enrollment:        enrollment,
	}
	storedPassphrase := pending.LocalPassphrase
	app.githubPending[pending.ID] = pending

	status := app.handleGitHubApprovalCallback(context.Background(), telegram.CallbackQuery{
		ID:   "callback-local",
		From: telegram.User{ID: 42},
		Message: &telegram.Message{
			MessageID: 77,
			Chat:      telegram.Chat{ID: 100},
		},
	}, true, pending.ID)
	if status != "callback_ok" {
		t.Fatalf("approval callback status = %q", status)
	}
	result := <-pending.Result
	defer githubauth.Zero(result.passphrase)
	if !bytes.Equal(result.passphrase, passphrase) {
		t.Fatal("local passphrase was not delivered to the broker")
	}
	if len(transport.sent) != 0 {
		t.Fatal("local unlock override unexpectedly requested a Telegram passphrase")
	}
	for _, value := range storedPassphrase {
		if value != 0 {
			t.Fatal("pending local passphrase was not zeroed")
		}
	}
}

func TestGitHubApprovalLabelsLocalUnlockOverrideTruthfully(t *testing.T) {
	app, _, sessionID := newSafetyApp(t, state.TerminalOriginAttached)
	session, found := app.Store.FindSession(sessionID)
	if !found {
		t.Fatal("session not found")
	}
	request := testLocalGitHubBrokerRequest()
	text := app.githubApprovalText(session, request, githubauth.App{TelegramUnlock: true}, time.Time{})
	if !strings.Contains(text, "Unlock: local passphrase") || strings.Contains(text, "Telegram reply") {
		t.Fatalf("approval text = %q", text)
	}
}

func TestNewKeepsCoreServiceAvailableWhenGitHubVaultIsCorrupt(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "github-apps.json"), []byte("{not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	app, err := New(config.Config{
		TelegramBotToken:      "token",
		TelegramAllowedUserID: 42,
		TelegramChatID:        42,
		AnchorMode:            config.AnchorModeSnapshot,
		Home:                  dir,
		Workdir:               dir,
		SnapshotBrowser:       filepath.Join(dir, "missing-chromium"),
		SnapshotTheme:         "terminal",
	})
	if err != nil || app == nil {
		t.Fatalf("degraded GitHub startup app=%#v err=%v", app, err)
	}
	defer app.Close()
	if app.GitHubVault != nil || app.githubVaultError == nil {
		t.Fatalf("GitHub vault=%#v error=%v", app.GitHubVault, app.githubVaultError)
	}
	if status := app.statusText(); !strings.Contains(status, "github apps: unavailable") {
		t.Fatalf("status omitted degraded GitHub capability:\n%s", status)
	}
}

func TestGitHubShutdownRevokesEveryLiveLease(t *testing.T) {
	minter := &fakeGitHubMinter{}
	app := &App{
		GitHubMinter: minter,
		githubLeases: map[string]githubLease{
			"one": {Token: "token-one"},
			"two": {Token: "token-two"},
		},
	}
	app.revokeAllGitHubLeases()
	app.transferWG.Wait()
	if len(app.githubLeases) != 0 {
		t.Fatal("shutdown retained GitHub leases")
	}
	minter.mu.Lock()
	revokeCalls := minter.revokeCalls
	minter.mu.Unlock()
	if revokeCalls != 2 {
		t.Fatalf("shutdown revoke calls = %d, want 2", revokeCalls)
	}
}

func TestGitHubApprovalCommandRenderingEscapesControlCharacters(t *testing.T) {
	trailing := "TRAILING_ARGUMENT_MUST_BE_VISIBLE"
	rendered := compactGitHubCommand([]string{"gh", "api", "line one\nPermissions:\ncontents: write", strings.Repeat("x", 400) + trailing})
	if strings.Contains(rendered, "\n") || !strings.Contains(rendered, `\nPermissions:\n`) {
		t.Fatalf("rendered command was ambiguous: %q", rendered)
	}
	if !strings.Contains(rendered, trailing) {
		t.Fatalf("rendered command hid trailing content: %q", rendered)
	}
}

func TestGitHubApprovalRejectsCommandThatCannotBeShownInFull(t *testing.T) {
	transport := newGitHubTelegramTransport()
	client := telegram.New("TOKEN")
	client.HTTPClient = &http.Client{Transport: transport}
	app, _, sessionID := newSafetyApp(t, state.TerminalOriginAttached)
	app.Telegram = client
	app.githubPending = map[string]*githubPendingRequest{}
	app.githubLeases = map[string]githubLease{}
	session, ok := app.Store.FindSession(sessionID)
	if !ok {
		t.Fatal("session not found")
	}
	request := testLocalGitHubBrokerRequest()
	request.Command = []string{"gh", "api", strings.Repeat("x", 4000) + "TRAILING_ARGUMENT"}
	pending, err := app.beginGitHubApproval(context.Background(), session, request, githubauth.App{})
	if pending != nil {
		app.finishGitHubPending(pending)
	}
	if err == nil || !strings.Contains(err.Error(), "too large to present safely") {
		t.Fatalf("beginGitHubApproval error = %v", err)
	}
	if len(transport.sent) != 0 {
		t.Fatal("oversized undisclosed command was sent for approval")
	}
}

func TestGitHubApprovalRejectsCommandThatRedactionWouldHide(t *testing.T) {
	transport := newGitHubTelegramTransport()
	client := telegram.New("TOKEN")
	client.HTTPClient = &http.Client{Transport: transport}
	app, _, sessionID := newSafetyApp(t, state.TerminalOriginAttached)
	app.Telegram = client
	app.githubPending = map[string]*githubPendingRequest{}
	app.githubLeases = map[string]githubLease{}
	session, ok := app.Store.FindSession(sessionID)
	if !ok {
		t.Fatal("session not found")
	}
	request := testLocalGitHubBrokerRequest()
	request.Command = []string{"gh", "api", "--field", "token=ghp_1234567890abcdef"}
	pending, err := app.beginGitHubApproval(context.Background(), session, request, githubauth.App{})
	if pending != nil {
		app.finishGitHubPending(pending)
	}
	if err == nil || !strings.Contains(err.Error(), "cannot be disclosed safely") {
		t.Fatalf("beginGitHubApproval error = %v", err)
	}
	if len(transport.sent) != 0 {
		t.Fatal("redacted command was sent for approval")
	}
}

func TestGitHubLeaseDecorationAppearsAcrossAnchorFormats(t *testing.T) {
	app, _, sessionID := newSafetyApp(t, state.TerminalOriginAttached)
	app.githubPending = map[string]*githubPendingRequest{}
	app.githubLeases = map[string]githubLease{}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	app.runCtx = canceled
	session, ok := app.Store.FindSession(sessionID)
	if !ok {
		t.Fatal("session not found")
	}
	binding := githubauth.Binding{ServerID: session.TmuxServerID, WindowID: session.TmuxWindowID, PaneID: session.TmuxPaneID}
	app.githubLeases[githubBindingKey(binding)] = githubLease{
		SessionID: sessionID,
		Binding:   binding,
		Info: githubauth.LeaseInfo{
			App:          "idolum",
			Repositories: []string{"idolum-ai/engram"},
			Permissions:  map[string]string{"contents": "read"},
			ExpiresAt:    time.Now().Add(42 * time.Minute),
		},
		Token: "secret-token",
	}
	want := "GH idolum · read-only · 1 repo"
	local := app.renderLocal(session, "ready")
	snapshot, _ := app.snapshotAnchorCaption(session, tmux.StyledCapture{Title: "shell", CurrentPath: "/tmp"}, visibleReferences{})
	guided, _ := app.guidedEvidenceCaption(session, "ready", visibleReferences{})
	for name, rendered := range map[string]string{"text": local, "snapshot": snapshot, "guide evidence": guided} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("%s anchor omitted capability line:\n%s", name, rendered)
		}
		if strings.Contains(rendered, "secret-token") {
			t.Fatalf("%s anchor leaked token", name)
		}
	}
}

func testGitHubVault(t *testing.T, telegramUnlock bool) *githubauth.Vault {
	t.Helper()
	vault, err := githubauth.OpenVault(filepath.Join(t.TempDir(), "github-apps.json"))
	if err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	privateKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	defer githubauth.Zero(privateKey)
	passphrase := []byte("correct horse battery staple")
	defer githubauth.Zero(passphrase)
	if _, _, err := vault.Add("idolum", 123, 456, privateKey, passphrase, telegramUnlock); err != nil {
		t.Fatal(err)
	}
	return vault
}

type fakeGitHubMinter struct {
	mu           sync.Mutex
	mintCalls    int
	revokeCalls  int
	repositories []string
	permissions  map[string]string
	expiresAt    time.Time
	inspectOnce  sync.Once
	inspectStart chan struct{}
	inspectWait  chan struct{}
	mintStarted  chan struct{}
	mintRelease  chan struct{}
	revokeErr    error
}

func (m *fakeGitHubMinter) InspectInstallation(ctx context.Context, _ githubauth.App, _ []byte) (githubauth.Installation, error) {
	if m.inspectStart != nil {
		m.inspectOnce.Do(func() { close(m.inspectStart) })
	}
	if m.inspectWait != nil {
		select {
		case <-m.inspectWait:
		case <-ctx.Done():
			return githubauth.Installation{}, ctx.Err()
		}
	}
	installation := githubauth.Installation{
		ID: 456,
		Permissions: map[string]string{
			"actions":       "read",
			"contents":      "write",
			"issues":        "write",
			"pull_requests": "write",
		},
	}
	installation.Account.Login = "idolum-ai"
	return installation, nil
}

func (m *fakeGitHubMinter) Mint(_ context.Context, _ githubauth.App, privateKey []byte, repositories []string, permissions map[string]string) (githubauth.Token, error) {
	if m.mintStarted != nil {
		close(m.mintStarted)
	}
	if m.mintRelease != nil {
		<-m.mintRelease
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !bytes.Contains(privateKey, []byte("RSA PRIVATE KEY")) {
		return githubauth.Token{}, io.ErrUnexpectedEOF
	}
	m.mintCalls++
	mintNumber := m.mintCalls
	m.repositories = append([]string(nil), repositories...)
	m.permissions = copyStringMap(permissions)
	expiresAt := m.expiresAt
	if expiresAt.IsZero() {
		expiresAt = time.Now().Add(time.Hour)
	}
	tokenValue := "ghs_fake_token"
	if mintNumber > 1 {
		tokenValue = fmt.Sprintf("ghs_fake_token_%d", mintNumber)
	}
	token := githubauth.Token{
		Value:       tokenValue,
		ExpiresAt:   expiresAt,
		Permissions: copyStringMap(permissions),
	}
	for _, repository := range repositories {
		token.Repositories = append(token.Repositories, struct {
			FullName string `json:"full_name"`
		}{FullName: repository})
	}
	return token, nil
}

func (m *fakeGitHubMinter) Revoke(context.Context, string) error {
	m.mu.Lock()
	m.revokeCalls++
	err := m.revokeErr
	m.mu.Unlock()
	return err
}

func (m *fakeGitHubMinter) mintCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mintCalls
}

func (m *fakeGitHubMinter) revokeCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.revokeCalls
}

func testLocalGitHubBrokerRequest() githubauth.BrokerRequest {
	return githubauth.BrokerRequest{
		Version:      githubauth.ProtocolVersion,
		Action:       githubauth.ActionExec,
		App:          "idolum",
		Repositories: []string{"idolum-ai/engram"},
		Permissions:  map[string]string{"contents": "read"},
		Command:      []string{"gh", "pr", "view", "51"},
		Binding:      githubauth.Binding{ServerID: appTestServerID, WindowID: "@1", PaneID: "%1"},
		Passphrase:   []byte("correct horse battery staple"),
	}
}

func newLocalGitHubApprovalTestApp(t *testing.T, minter githubauth.Minter) (*App, *githubTelegramTransport, int) {
	t.Helper()
	app, _, sessionID := newSafetyApp(t, state.TerminalOriginAttached)
	app.Config.TelegramBotToken = "BOT_SECRET"
	app.GitHubVault = testGitHubVault(t, false)
	app.GitHubMinter = minter
	app.githubPending = map[string]*githubPendingRequest{}
	app.githubLeases = map[string]githubLease{}
	app.githubNow = time.Now
	stopped, stop := context.WithCancel(context.Background())
	stop()
	app.runCtx = stopped

	transport := newGitHubTelegramTransport()
	client := telegram.New("BOT_SECRET")
	client.HTTPClient = &http.Client{Transport: transport}
	app.Telegram = client
	return app, transport, sessionID
}

type githubTelegramMessage struct {
	id         int
	text       string
	forceReply bool
}

type githubTelegramTransport struct {
	mu      sync.Mutex
	nextID  int
	sent    chan githubTelegramMessage
	deleted []int
}

func newGitHubTelegramTransport() *githubTelegramTransport {
	return &githubTelegramTransport{nextID: 77, sent: make(chan githubTelegramMessage, 8)}
}

func (t *githubTelegramTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	var body map[string]any
	if request.Body != nil {
		_ = json.NewDecoder(request.Body).Decode(&body)
	}
	method := filepath.Base(request.URL.Path)
	var result any = true
	switch method {
	case "sendMessage":
		t.mu.Lock()
		messageID := t.nextID
		t.nextID++
		t.mu.Unlock()
		forceReply := false
		if markup, ok := body["reply_markup"].(map[string]any); ok {
			forceReply, _ = markup["force_reply"].(bool)
		}
		message := githubTelegramMessage{id: messageID, text: stringValue(body["text"]), forceReply: forceReply}
		t.sent <- message
		result = map[string]any{"message_id": messageID, "chat": map[string]any{"id": 100}}
	case "editMessageText":
		result = map[string]any{"message_id": intValue(body["message_id"]), "chat": map[string]any{"id": 100}}
	case "deleteMessage":
		t.mu.Lock()
		t.deleted = append(t.deleted, intValue(body["message_id"]))
		t.mu.Unlock()
	}
	payload, _ := json.Marshal(map[string]any{"ok": true, "result": result})
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(payload)),
		Request:    request,
	}, nil
}

func (t *githubTelegramTransport) deletedMessage(messageID int) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, deleted := range t.deleted {
		if deleted == messageID {
			return true
		}
	}
	return false
}

func pendingGitHubTestIdentity(t *testing.T, app *App) (string, int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		app.githubMu.Lock()
		for requestID, pending := range app.githubPending {
			if pending.ApprovalMessageID != 0 {
				messageID := pending.ApprovalMessageID
				app.githubMu.Unlock()
				return requestID, messageID
			}
		}
		app.githubMu.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatal("GitHub approval message was not recorded")
	return "", 0
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func intValue(value any) int {
	number, _ := value.(float64)
	return int(number)
}

func configForGitHubTest() config.Config {
	return config.Config{TelegramAllowedUserID: 42, TelegramChatID: 100}
}

func requireZeroedGitHubPassphrase(t *testing.T, passphrase []byte) {
	t.Helper()
	for index, value := range passphrase {
		if value != 0 {
			t.Fatalf("passphrase byte %d = %d, want zero", index, value)
		}
	}
}
