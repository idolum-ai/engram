package app

import (
	"context"
	"fmt"
	"time"

	"github.com/idolum-ai/engram/internal/githubauth"
	"github.com/idolum-ai/engram/internal/state"
)

type githubGrant struct {
	SessionID        int
	SessionCreatedAt time.Time
	Binding          githubauth.Binding
	Enrollment       githubauth.App
	Info             githubauth.GrantInfo
	PrivateKey       []byte
	Generation       uint64
}

func (a *App) createGitHubGrant(
	ctx context.Context,
	session state.TerminalSession,
	pending *githubPendingRequest,
	request githubauth.BrokerRequest,
	enrollment githubauth.App,
	privateKey []byte,
) githubauth.BrokerResponse {
	handle := a.githubGrantLocks.handle(session.ID)
	handle.Lock()
	defer handle.Unlock()
	maximum := a.Config.EffectiveGitHubGrantMaxDuration()
	if request.GrantFor > maximum {
		a.completeGitHubApprovalMessage(pending, "Denied: the requested duration exceeds this Engram instance's renewable-grant ceiling.")
		return githubauth.BrokerResponse{Error: fmt.Sprintf("renewable grant duration exceeds configured maximum %s", maximum)}
	}
	inspectCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	installation, err := a.GitHubMinter.InspectInstallation(inspectCtx, enrollment, privateKey)
	if err == nil {
		err = githubauth.ValidateInstallationScope(installation, request.Repositories, request.Permissions)
	}
	if err == nil {
		err = a.GitHubMinter.ValidateInstallationRepositories(inspectCtx, enrollment, privateKey, installation, request.Repositories)
	}
	cancel()
	if err != nil {
		a.completeGitHubApprovalMessage(pending, "Failed: GitHub rejected the renewable authority envelope.")
		_ = a.audit("github.grant", "validation_failed", githubAuditRequest(session.ID, request))
		return githubauth.BrokerResponse{Error: err.Error()}
	}
	if err := a.validateGitHubBrokerContinuation(ctx, session, request.Binding); err != nil {
		a.completeGitHubApprovalMessage(pending, "Canceled: the requesting tmux pane changed before the grant could be stored.")
		return githubauth.BrokerResponse{Error: err.Error()}
	}
	if _, err := a.reloadMatchingGitHubEnrollment(enrollment); err != nil {
		a.completeGitHubApprovalMessage(pending, "Canceled: the GitHub App enrollment changed before the grant could be stored.")
		return githubauth.BrokerResponse{Error: err.Error()}
	}
	if err := a.validateConfiguredGitHubAppPEM(pending); err != nil {
		a.completeGitHubApprovalMessage(pending, "Canceled: the configured local GitHub App PEM changed before the grant could be stored.")
		return githubauth.BrokerResponse{Error: err.Error()}
	}
	grantID, err := githubRequestID()
	if err != nil {
		return githubauth.BrokerResponse{Error: err.Error()}
	}
	now := a.githubTime()
	if !pending.GrantExpiresAt.After(now) {
		a.completeGitHubApprovalMessage(pending, "Expired: the approved work-session boundary elapsed before the grant could be stored.")
		return githubauth.BrokerResponse{Error: "renewable GitHub grant expired before it could be stored"}
	}
	grant := githubGrant{
		SessionID:        session.ID,
		SessionCreatedAt: session.CreatedAt,
		Binding:          request.Binding,
		Enrollment:       enrollment,
		Info: githubauth.GrantInfo{
			ID:             grantID,
			App:            request.App,
			InstallationID: request.InstallationID,
			Repositories:   append([]string(nil), request.Repositories...),
			Permissions:    copyStringMap(request.Permissions),
			Purpose:        request.Purpose,
			CreatedAt:      now,
			ExpiresAt:      pending.GrantExpiresAt,
		},
		PrivateKey: append([]byte(nil), privateKey...),
	}
	oldLeases := a.storeGitHubGrant(grant)
	a.revokeGitHubLeases(oldLeases)
	a.queueManualRefresh(session.ID)
	a.completeGitHubApprovalMessage(pending, fmt.Sprintf(
		"✓ Active until %s.", grant.Info.ExpiresAt.Local().Format(githubApprovalTimeFormat),
	))
	fields := githubAuditRequest(session.ID, request)
	fields["grant_id"] = grantID
	fields["expires_at"] = grant.Info.ExpiresAt
	_ = a.audit("github.grant", "created", fields)
	return githubauth.BrokerResponse{OK: true, Grants: []githubauth.GrantInfo{copyGitHubGrantInfo(grant.Info)}}
}

func (a *App) consumeGitHubGrant(
	ctx context.Context,
	session state.TerminalSession,
	request githubauth.BrokerRequest,
	enrollment githubauth.App,
) (githubauth.BrokerResponse, bool) {
	candidate, ok := a.matchingGitHubGrant(request, enrollment, session)
	if !ok {
		return githubauth.BrokerResponse{}, false
	}
	githubauth.Zero(candidate.PrivateKey)
	handle := a.githubGrantLocks.handle(session.ID)
	handle.Lock()
	lockOwned := true
	defer func() {
		if lockOwned {
			handle.Unlock()
		}
	}()

	if lease, ok := a.reusableGitHubLease(request, enrollment); ok {
		finalize := func(delivered bool) error {
			defer handle.Unlock()
			if !delivered {
				return nil
			}
			if err := a.validateGitHubBrokerContinuation(ctx, session, request.Binding); err != nil {
				return err
			}
			if _, err := a.reloadMatchingGitHubEnrollment(enrollment); err != nil {
				return err
			}
			a.githubMu.Lock()
			currentGrant, grantActive := a.githubGrants[githubBindingKey(request.Binding)]
			currentLease, leaseActive := a.githubLeases[githubBindingKey(request.Binding)]
			valid := grantActive && leaseActive &&
				currentGrant.Info.ID == lease.Info.GrantID &&
				currentLease.Token == lease.Token &&
				currentGrant.Info.ExpiresAt.After(a.githubTime())
			a.githubMu.Unlock()
			if !valid {
				return fmt.Errorf("renewable GitHub grant changed before token delivery")
			}
			_ = a.audit("github.grant.consume", "lease_reused", githubGrantAuditRequest(session.ID, request, lease.Info.GrantID, lease.Info.Generation))
			return nil
		}
		if err := githubauth.RegisterDeliveryFinalizer(ctx, finalize); err == nil {
			lockOwned = false
		} else {
			lockOwned = false
			if err := finalize(true); err != nil {
				return githubauth.BrokerResponse{Error: err.Error()}, true
			}
		}
		return githubauth.BrokerResponse{OK: true, Token: lease.Token, ExpiresAt: lease.Info.ExpiresAt}, true
	}
	grant, ok := a.matchingGitHubGrant(request, enrollment, session)
	if !ok {
		return githubauth.BrokerResponse{Error: "renewable GitHub grant is no longer active"}, true
	}
	defer githubauth.Zero(grant.PrivateKey)
	if err := a.validateGitHubBrokerContinuation(ctx, session, request.Binding); err != nil {
		a.invalidateGitHubGrant(ctx, request.Binding, "invalidated")
		return githubauth.BrokerResponse{Error: err.Error()}, true
	}
	if _, err := a.reloadMatchingGitHubEnrollment(grant.Enrollment); err != nil {
		a.invalidateGitHubGrant(ctx, request.Binding, "enrollment_changed")
		return githubauth.BrokerResponse{Error: err.Error()}, true
	}

	if current, found := a.currentGitHubLease(request.Binding); found {
		if current.Info.ExpiresAt.After(a.githubTime()) {
			return githubauth.BrokerResponse{Error: "active grant token did not cover an approved subset"}, true
		}
		a.discardGitHubLease(request.Binding, current.Token)
	}

	if err := a.reserveGitHubTokenSlot(); err != nil {
		_ = a.audit("github.grant.consume", "capacity_rejected", githubGrantAuditRequest(session.ID, request, grant.Info.ID, grant.Generation))
		return githubauth.BrokerResponse{Error: err.Error()}, true
	}
	tokenSlotOwned := true
	defer func() {
		if tokenSlotOwned {
			a.releaseGitHubTokenSlot()
		}
	}()
	mintCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	token, err := a.GitHubMinter.Mint(mintCtx, grant.Enrollment, grant.PrivateKey, grant.Info.Repositories, grant.Info.Permissions)
	cancel()
	if err != nil {
		_ = a.audit("github.grant.consume", "mint_failed", githubGrantAuditRequest(session.ID, request, grant.Info.ID, grant.Generation))
		return githubauth.BrokerResponse{Error: err.Error()}, true
	}
	mintedRevocation := githubRevocation{
		Token:          token.Value,
		SessionID:      session.ID,
		App:            request.App,
		InstallationID: request.InstallationID,
		ExpiresAt:      token.ExpiresAt,
	}
	if err := a.validateGitHubBrokerContinuation(ctx, session, request.Binding); err != nil {
		return githubauth.BrokerResponse{Error: a.revokeDiscardedGitHubToken(ctx, mintedRevocation, err).Error()}, true
	}
	if _, err := a.reloadMatchingGitHubEnrollment(grant.Enrollment); err != nil {
		return githubauth.BrokerResponse{Error: a.revokeDiscardedGitHubToken(ctx, mintedRevocation, err).Error()}, true
	}

	finalize := func(delivered bool) error {
		defer handle.Unlock()
		defer a.releaseGitHubTokenSlot()
		if !delivered {
			err := a.revokeDiscardedGitHubToken(context.Background(), mintedRevocation, fmt.Errorf("requester disconnected before GitHub token delivery"))
			_ = a.audit("github.grant.consume", "delivery_rolled_back", githubGrantAuditRequest(session.ID, request, grant.Info.ID, grant.Generation))
			return err
		}
		if err := a.validateGitHubBrokerContinuation(ctx, session, request.Binding); err != nil {
			return a.revokeDiscardedGitHubToken(context.Background(), mintedRevocation, err)
		}
		if _, err := a.reloadMatchingGitHubEnrollment(grant.Enrollment); err != nil {
			return a.revokeDiscardedGitHubToken(context.Background(), mintedRevocation, err)
		}
		a.githubMu.Lock()
		current, active := a.githubGrants[githubBindingKey(request.Binding)]
		if !active || current.Info.ID != grant.Info.ID || !current.Info.ExpiresAt.After(a.githubTime()) ||
			!sameGitHubEnrollment(current.Enrollment, grant.Enrollment) {
			a.githubMu.Unlock()
			return a.revokeDiscardedGitHubToken(context.Background(), mintedRevocation, fmt.Errorf("renewable GitHub grant changed during token delivery"))
		}
		current.Generation++
		a.githubGrants[githubBindingKey(request.Binding)] = current
		generation := current.Generation
		a.githubLeases[githubBindingKey(request.Binding)] = githubLease{
			SessionID:  session.ID,
			Binding:    request.Binding,
			Enrollment: grant.Enrollment,
			Info: githubauth.LeaseInfo{
				App:            request.App,
				InstallationID: request.InstallationID,
				Repositories:   append([]string(nil), grant.Info.Repositories...),
				Permissions:    copyStringMap(grant.Info.Permissions),
				ExpiresAt:      token.ExpiresAt,
				GrantID:        grant.Info.ID,
				Generation:     generation,
			},
			Token: token.Value,
		}
		a.githubMu.Unlock()
		a.queueManualRefresh(session.ID)
		outcome := "minted"
		if generation > 1 {
			outcome = "rotated"
		}
		_ = a.audit("github.grant.consume", outcome, githubGrantAuditRequest(session.ID, request, grant.Info.ID, generation))
		return nil
	}
	if err := githubauth.RegisterDeliveryFinalizer(ctx, finalize); err == nil {
		lockOwned = false
		tokenSlotOwned = false
	} else {
		lockOwned = false
		tokenSlotOwned = false
		if err := finalize(true); err != nil {
			return githubauth.BrokerResponse{Error: err.Error()}, true
		}
	}
	return githubauth.BrokerResponse{OK: true, Token: token.Value, ExpiresAt: token.ExpiresAt}, true
}

func (a *App) matchingGitHubGrant(request githubauth.BrokerRequest, enrollment githubauth.App, session state.TerminalSession) (githubGrant, bool) {
	key := githubBindingKey(request.Binding)
	a.githubMu.Lock()
	defer a.githubMu.Unlock()
	grant, ok := a.githubGrants[key]
	if !ok || !grant.Info.ExpiresAt.After(a.githubTime()) || grant.Info.App != request.App ||
		grant.SessionID != session.ID || !grant.SessionCreatedAt.Equal(session.CreatedAt) ||
		!sameGitHubEnrollment(grant.Enrollment, enrollment) ||
		!githubauth.RepositoriesSubset(request.Repositories, grant.Info.Repositories) ||
		!githubauth.PermissionsSubset(request.Permissions, grant.Info.Permissions) {
		return githubGrant{}, false
	}
	grant.PrivateKey = append([]byte(nil), grant.PrivateKey...)
	grant.Info = copyGitHubGrantInfo(grant.Info)
	return grant, true
}

func (a *App) storeGitHubGrant(grant githubGrant) []githubLease {
	key := githubBindingKey(grant.Binding)
	a.githubMu.Lock()
	defer a.githubMu.Unlock()
	var oldLeases []githubLease
	if previous, ok := a.githubGrants[key]; ok {
		githubauth.Zero(previous.PrivateKey)
	}
	if lease, ok := a.githubLeases[key]; ok {
		oldLeases = append(oldLeases, lease)
		delete(a.githubLeases, key)
	}
	a.githubGrants[key] = grant
	return oldLeases
}

func (a *App) currentGitHubLease(binding githubauth.Binding) (githubLease, bool) {
	a.githubMu.Lock()
	defer a.githubMu.Unlock()
	lease, found := a.githubLeases[githubBindingKey(binding)]
	return lease, found
}

func (a *App) githubGrantInfos(binding githubauth.Binding) []githubauth.GrantInfo {
	a.githubMu.Lock()
	defer a.githubMu.Unlock()
	grant, ok := a.githubGrants[githubBindingKey(binding)]
	if !ok || !grant.Info.ExpiresAt.After(a.githubTime()) {
		return nil
	}
	return []githubauth.GrantInfo{copyGitHubGrantInfo(grant.Info)}
}

func (a *App) invalidateGitHubGrant(ctx context.Context, binding githubauth.Binding, outcome string) {
	key := githubBindingKey(binding)
	a.githubMu.Lock()
	grant, found := a.githubGrants[key]
	if found {
		githubauth.Zero(grant.PrivateKey)
		delete(a.githubGrants, key)
	}
	lease, leased := a.githubLeases[key]
	if leased {
		delete(a.githubLeases, key)
	}
	a.githubMu.Unlock()
	if leased {
		revokeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		if a.GitHubMinter != nil {
			if err := a.GitHubMinter.Revoke(revokeCtx, lease.Token); err != nil {
				a.trackGitHubRevocation(lease.Token, lease.SessionID, lease.Info.App, lease.Info.InstallationID, lease.Info.ExpiresAt)
			}
		}
		cancel()
	}
	if found {
		a.queueManualRefresh(grant.SessionID)
		_ = a.audit("github.grant", outcome, map[string]any{"session_id": grant.SessionID, "app": grant.Info.App, "installation_id": grant.Info.InstallationID, "grant_id": grant.Info.ID})
	}
}

func githubGrantAuditRequest(sessionID int, request githubauth.BrokerRequest, grantID string, generation uint64) map[string]any {
	fields := githubAuditRequest(sessionID, request)
	fields["grant_id"] = grantID
	fields["token_generation"] = generation
	return fields
}

func copyGitHubGrantInfo(info githubauth.GrantInfo) githubauth.GrantInfo {
	info.Repositories = append([]string(nil), info.Repositories...)
	info.Permissions = copyStringMap(info.Permissions)
	return info
}
