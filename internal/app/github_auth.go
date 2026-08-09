package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"html"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/idolum-ai/engram/internal/githubauth"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
	"github.com/idolum-ai/engram/internal/tmux"
)

const githubApprovalTTL = githubauth.ApprovalTimeout
const githubUnlockTombstoneTTL = 10 * time.Minute
const githubApprovalTimeFormat = "2006-01-02 15:04 MST"

type githubApproval struct {
	passphrase    []byte
	configuredPEM bool
	failure       githubApprovalFailure
	auditStatus   string
	err           error
}

type githubApprovalFailure string

const (
	githubApprovalDenied   githubApprovalFailure = "denied"
	githubApprovalCanceled githubApprovalFailure = "canceled"
	githubApprovalFailed   githubApprovalFailure = "failed"
)

type configuredGitHubAppPEMState string

const (
	configuredGitHubAppPEMDisabled    configuredGitHubAppPEMState = "disabled"
	configuredGitHubAppPEMReady       configuredGitHubAppPEMState = "ready"
	configuredGitHubAppPEMUnavailable configuredGitHubAppPEMState = "unavailable"
	configuredGitHubAppPEMUnmatched   configuredGitHubAppPEMState = "unmatched"
	configuredGitHubAppPEMAmbiguous   configuredGitHubAppPEMState = "ambiguous"
)

type githubPendingRequest struct {
	ID                string
	SessionID         int
	BindingKey        string
	Request           githubauth.BrokerRequest
	LocalPassphrase   []byte
	ConfiguredPEM     *githubauth.PrivateKeyFileIdentity
	ExpiresAt         time.Time
	ApprovalMessageID int
	ApprovalText      string
	ApprovalSummary   string
	UnlockMessageID   int
	State             string
	Result            chan githubApproval
	Enrollment        githubauth.App
	GrantExpiresAt    time.Time
}

type githubLease struct {
	SessionID  int
	Binding    githubauth.Binding
	Enrollment githubauth.App
	Info       githubauth.LeaseInfo
	Token      string
}

type githubRevocation struct {
	Token          string
	App            string
	InstallationID int64
	SessionID      int
	ExpiresAt      time.Time
	NextAttempt    time.Time
	Attempts       int
}

func (a *App) startGitHubBroker(ctx context.Context) {
	broker, err := githubauth.Listen(a.Config.GitHubBrokerSocketPath(), a.handleGitHubBrokerRequest)
	if err != nil {
		_ = a.audit("github.broker", "unavailable", map[string]any{"error": err.Error()})
		return
	}
	a.githubBroker = broker
	a.schedulerWG.Add(1)
	go func() {
		defer a.schedulerWG.Done()
		defer broker.Close()
		if err := broker.Serve(ctx); err != nil && ctx.Err() == nil {
			_ = a.audit("github.broker", "failed", map[string]any{"error": err.Error()})
		}
	}()
	_ = a.audit("github.broker", "ready", map[string]any{"socket": a.Config.GitHubBrokerSocketPath()})
}

func (a *App) handleGitHubBrokerRequest(ctx context.Context, request githubauth.BrokerRequest) githubauth.BrokerResponse {
	defer githubauth.Zero(request.Passphrase)
	if a.GitHubVault == nil {
		return githubauth.BrokerResponse{Error: "GitHub App capabilities are unavailable"}
	}
	session, err := a.validateGitHubBrokerBinding(ctx, request.Binding)
	if err != nil {
		return githubauth.BrokerResponse{Error: err.Error()}
	}
	switch request.Action {
	case githubauth.ActionStatus:
		return githubauth.BrokerResponse{OK: true, Leases: a.githubLeaseInfos(request.Binding), Grants: a.githubGrantInfos(request.Binding)}
	case githubauth.ActionRevoke:
		if err := a.revokeGitHubBindingAuthority(ctx, session.ID, request.Binding); err != nil {
			return githubauth.BrokerResponse{Error: err.Error()}
		}
		return githubauth.BrokerResponse{OK: true}
	case githubauth.ActionExec, githubauth.ActionGrant:
	default:
		return githubauth.BrokerResponse{Error: "unsupported GitHub broker action"}
	}

	if err := a.GitHubVault.Reload(); err != nil {
		return githubauth.BrokerResponse{Error: "reload GitHub App vault: " + err.Error()}
	}
	app, found := a.GitHubVault.Get(request.App)
	if !found {
		return githubauth.BrokerResponse{Error: fmt.Sprintf("GitHub App %q is not enrolled", request.App)}
	}
	app, err = app.SelectInstallation(request.InstallationID)
	if err != nil {
		return githubauth.BrokerResponse{Error: err.Error()}
	}
	request.InstallationID = app.EffectiveInstallationID()
	if request.Action == githubauth.ActionExec {
		if lease, ok := a.reusableGitHubLease(request, app); ok && lease.Info.GrantID == "" {
			if err := a.validateGitHubBrokerContinuation(ctx, session, request.Binding); err != nil {
				if a.discardGitHubLease(request.Binding, lease.Token) {
					err = a.revokeDiscardedGitHubToken(ctx, githubRevocationForLease(lease), err)
				}
				_ = a.audit("github.lease", "invalidated", githubAuditRequest(session.ID, request))
				return githubauth.BrokerResponse{Error: err.Error()}
			}
			if _, err := a.reloadMatchingGitHubEnrollment(lease.Enrollment); err != nil {
				if a.discardGitHubLease(request.Binding, lease.Token) {
					err = a.revokeDiscardedGitHubToken(ctx, githubRevocationForLease(lease), err)
				}
				_ = a.audit("github.lease", "enrollment_changed", githubAuditRequest(session.ID, request))
				return githubauth.BrokerResponse{Error: err.Error()}
			}
			_ = a.audit("github.lease", "reused", githubAuditRequest(session.ID, request))
			return githubauth.BrokerResponse{OK: true, Token: lease.Token, ExpiresAt: lease.Info.ExpiresAt}
		}
		if response, consumed := a.consumeGitHubGrant(ctx, session, request, app); consumed {
			return response
		}
	}
	if request.Action == githubauth.ActionGrant && request.GrantFor > a.Config.EffectiveGitHubGrantMaxDuration() {
		return githubauth.BrokerResponse{Error: fmt.Sprintf(
			"renewable grant duration exceeds configured maximum %s",
			a.Config.EffectiveGitHubGrantMaxDuration(),
		)}
	}
	configuredPEM, err := a.resolveConfiguredGitHubAppPEM(app)
	if err != nil {
		return githubauth.BrokerResponse{Error: err.Error()}
	}
	if configuredPEM == nil && (!app.TelegramUnlock || request.LocalUnlock) && len(request.Passphrase) == 0 {
		return githubauth.BrokerResponse{Error: "this GitHub App requires local passphrase entry", ErrorCode: githubauth.ErrorCodeLocalPassphraseRequired}
	}
	pending, err := a.beginGitHubApproval(ctx, session, request, app, configuredPEM)
	if err != nil {
		return githubauth.BrokerResponse{Error: err.Error()}
	}
	defer a.finishGitHubPending(pending)

	timer := time.NewTimer(time.Until(pending.ExpiresAt))
	defer timer.Stop()
	var approval githubApproval
	select {
	case <-ctx.Done():
		a.completeGitHubApprovalMessage(pending, "Canceled: Engram stopped before this request completed.")
		return githubauth.BrokerResponse{Error: "GitHub capability request was canceled"}
	case <-timer.C:
		a.completeGitHubApprovalMessage(pending, "Expired: no approval was received within fifteen minutes.")
		_ = a.audit("github.approval", "expired", githubAuditRequest(session.ID, request))
		return githubauth.BrokerResponse{Error: "GitHub capability request expired"}
	case approval = <-pending.Result:
	}
	if approval.err != nil {
		completion := "Failed: the approved GitHub request could not continue."
		if approval.failure == githubApprovalDenied {
			completion = "Denied: no GitHub capability was granted."
		} else if approval.failure == githubApprovalCanceled {
			completion = "Canceled: the approved GitHub request could not continue."
		}
		a.completeGitHubApprovalMessage(pending, completion)
		status := approval.auditStatus
		if status == "" {
			status = string(approval.failure)
		}
		if status == "" {
			status = "failed"
		}
		_ = a.audit("github.approval", status, githubAuditRequest(session.ID, request))
		return githubauth.BrokerResponse{Error: approval.err.Error()}
	}
	defer githubauth.Zero(approval.passphrase)
	if err := a.validateGitHubBrokerContinuation(ctx, session, request.Binding); err != nil {
		a.completeGitHubApprovalMessage(pending, "Canceled: the requesting tmux pane is no longer valid.")
		_ = a.audit("github.approval", "invalidated", githubAuditRequest(session.ID, request))
		return githubauth.BrokerResponse{Error: err.Error()}
	}
	currentEnrollment, err := a.reloadMatchingGitHubEnrollment(pending.Enrollment)
	if err != nil {
		a.completeGitHubApprovalMessage(pending, "Canceled: the GitHub App enrollment changed during approval.")
		_ = a.audit("github.approval", "enrollment_changed", githubAuditRequest(session.ID, request))
		return githubauth.BrokerResponse{Error: err.Error()}
	}

	var privateKey []byte
	var unlockedApp githubauth.App
	if approval.configuredPEM {
		privateKey, err = a.readMatchingConfiguredGitHubAppPEM(pending)
		unlockedApp = currentEnrollment
	} else {
		privateKey, unlockedApp, err = a.GitHubVault.Unlock(request.App, approval.passphrase)
	}
	if err != nil {
		if approval.configuredPEM {
			a.cancelConfiguredGitHubAppPEM(
				pending,
				"github.unlock",
				"Canceled: the configured local GitHub App PEM changed before the credential could be used.",
				session.ID,
				request,
			)
			return githubauth.BrokerResponse{Error: err.Error()}
		}
		a.completeGitHubApprovalMessage(pending, "Failed: the GitHub App credential could not be unlocked.")
		_ = a.audit("github.unlock", "failed", githubAuditRequest(session.ID, request))
		return githubauth.BrokerResponse{Error: githubauth.ErrUnlock.Error()}
	}
	defer githubauth.Zero(privateKey)
	unlockedApp, err = unlockedApp.SelectInstallation(request.InstallationID)
	if err != nil {
		a.completeGitHubApprovalMessage(pending, "Canceled: the selected GitHub App installation changed during approval.")
		return githubauth.BrokerResponse{Error: err.Error()}
	}
	if a.GitHubMinter == nil {
		return githubauth.BrokerResponse{Error: "GitHub token minting is unavailable"}
	}
	if request.Action == githubauth.ActionGrant {
		return a.createGitHubGrant(ctx, session, pending, request, unlockedApp, privateKey)
	}
	if err := a.reserveGitHubTokenSlot(); err != nil {
		a.completeGitHubApprovalMessage(pending, "Failed: Engram's bounded GitHub token budget is full.")
		_ = a.audit("github.mint", "capacity_rejected", githubAuditRequest(session.ID, request))
		return githubauth.BrokerResponse{Error: err.Error()}
	}
	defer a.releaseGitHubTokenSlot()
	mintCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	token, err := a.GitHubMinter.Mint(mintCtx, unlockedApp, privateKey, request.Repositories, request.Permissions)
	cancel()
	if err != nil {
		a.completeGitHubApprovalMessage(pending, "Failed: GitHub rejected the scoped capability request.")
		_ = a.audit("github.mint", "failed", map[string]any{
			"session_id":      session.ID,
			"app":             request.App,
			"installation_id": request.InstallationID,
			"error":           err.Error(),
		})
		return githubauth.BrokerResponse{Error: err.Error()}
	}
	lease := githubLease{
		SessionID:  session.ID,
		Binding:    request.Binding,
		Enrollment: unlockedApp,
		Info: githubauth.LeaseInfo{
			App:            request.App,
			InstallationID: request.InstallationID,
			Repositories:   append([]string(nil), request.Repositories...),
			Permissions:    copyStringMap(request.Permissions),
			ExpiresAt:      token.ExpiresAt,
		},
		Token: token.Value,
	}
	if err := a.validateGitHubBrokerContinuation(ctx, session, request.Binding); err != nil {
		cleanupErr := a.revokeDiscardedGitHubToken(ctx, githubRevocation{
			Token: token.Value, SessionID: session.ID, App: request.App,
			InstallationID: request.InstallationID, ExpiresAt: token.ExpiresAt,
		}, err)
		a.completeGitHubApprovalMessage(pending, "Canceled: the requesting tmux pane changed before the capability could be delivered.")
		_ = a.audit("github.mint", "discarded", map[string]any{
			"session_id":      session.ID,
			"app":             request.App,
			"installation_id": request.InstallationID,
			"error":           cleanupErr.Error(),
		})
		return githubauth.BrokerResponse{Error: cleanupErr.Error()}
	}
	if _, err := a.reloadMatchingGitHubEnrollment(pending.Enrollment); err != nil {
		cleanupErr := a.revokeDiscardedGitHubToken(ctx, githubRevocation{
			Token: token.Value, SessionID: session.ID, App: request.App,
			InstallationID: request.InstallationID, ExpiresAt: token.ExpiresAt,
		}, err)
		a.completeGitHubApprovalMessage(pending, "Canceled: the GitHub App enrollment changed before the capability could be delivered.")
		_ = a.audit("github.mint", "discarded", map[string]any{
			"session_id":      session.ID,
			"app":             request.App,
			"installation_id": request.InstallationID,
			"error":           cleanupErr.Error(),
		})
		return githubauth.BrokerResponse{Error: cleanupErr.Error()}
	}
	if err := a.validateConfiguredGitHubAppPEM(pending); err != nil {
		cleanupErr := a.revokeDiscardedGitHubToken(ctx, githubRevocation{
			Token: token.Value, SessionID: session.ID, App: request.App,
			InstallationID: request.InstallationID, ExpiresAt: token.ExpiresAt,
		}, err)
		a.cancelConfiguredGitHubAppPEM(
			pending,
			"github.mint",
			"Canceled: the configured local GitHub App PEM changed before the capability could be delivered.",
			session.ID,
			request,
		)
		return githubauth.BrokerResponse{Error: cleanupErr.Error()}
	}
	oldTokens := a.storeGitHubLease(lease)
	a.revokeGitHubLeases(oldTokens)
	a.queueManualRefresh(session.ID)
	a.completeGitHubApprovalMessage(pending, fmt.Sprintf(
		"✓ Active until %s.", token.ExpiresAt.Local().Format(githubApprovalTimeFormat),
	))
	_ = a.audit("github.lease", "granted", githubAuditRequest(session.ID, request))
	return githubauth.BrokerResponse{OK: true, Token: token.Value, ExpiresAt: token.ExpiresAt}
}

func (a *App) validateGitHubBrokerBinding(ctx context.Context, binding githubauth.Binding) (state.TerminalSession, error) {
	var matched state.TerminalSession
	found := false
	for _, session := range a.Store.Snapshot().TerminalSessions {
		if session.TmuxServerID == binding.ServerID && session.TmuxWindowID == binding.WindowID && session.TmuxPaneID == binding.PaneID {
			matched = session
			found = true
			break
		}
	}
	if !found || matched.State != state.TerminalRunning || !matched.WatchEnabled {
		return state.TerminalSession{}, fmt.Errorf("requesting tmux pane is not an active Engram session")
	}
	tmuxCtx, cancel := tmux.TimeoutContext(ctx)
	defer cancel()
	if _, err := a.Tmux.ValidateBinding(tmuxCtx, binding.PaneID, binding.WindowID, binding.ServerID); err != nil {
		return state.TerminalSession{}, fmt.Errorf("requesting tmux pane identity is no longer valid")
	}
	return matched, nil
}

func (a *App) validateGitHubBrokerContinuation(ctx context.Context, expected state.TerminalSession, binding githubauth.Binding) error {
	if ctx.Err() != nil {
		return fmt.Errorf("GitHub capability request was canceled")
	}
	current, err := a.validateGitHubBrokerBinding(ctx, binding)
	if err != nil {
		return err
	}
	if current.ID != expected.ID || !current.CreatedAt.Equal(expected.CreatedAt) {
		return fmt.Errorf("requesting tmux pane identity changed during GitHub capability approval")
	}
	return nil
}

func (a *App) revokeDiscardedGitHubToken(ctx context.Context, pending githubRevocation, cause error) error {
	if a.GitHubMinter == nil {
		return cause
	}
	revokeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	err := a.GitHubMinter.Revoke(revokeCtx, pending.Token)
	cancel()
	if err != nil {
		a.trackGitHubRevocation(pending.Token, pending.SessionID, pending.App, pending.InstallationID, pending.ExpiresAt)
		return fmt.Errorf("%w; revoke discarded GitHub token: %v", cause, err)
	}
	return cause
}

func githubRevocationForLease(lease githubLease) githubRevocation {
	return githubRevocation{
		Token: lease.Token, SessionID: lease.SessionID, App: lease.Info.App,
		InstallationID: lease.Info.InstallationID, ExpiresAt: lease.Info.ExpiresAt,
	}
}

func (a *App) beginGitHubApproval(ctx context.Context, session state.TerminalSession, request githubauth.BrokerRequest, app githubauth.App, configuredPEM *githubauth.PrivateKeyFileIdentity) (*githubPendingRequest, error) {
	requestID, err := githubRequestID()
	if err != nil {
		return nil, err
	}
	command := compactGitHubCommand(request.Command)
	if request.Action == githubauth.ActionExec && a.redactText(command) != command {
		return nil, fmt.Errorf("GitHub child command contains secret material that cannot be disclosed safely for approval")
	}
	if request.Action == githubauth.ActionGrant && a.redactText(request.Purpose) != request.Purpose {
		return nil, fmt.Errorf("renewable GitHub grant purpose contains secret material that cannot be disclosed safely")
	}
	now := a.githubTime()
	grantExpiresAt := time.Time{}
	if request.Action == githubauth.ActionGrant {
		grantExpiresAt = now.Add(request.GrantFor)
	}
	text := a.githubApprovalText(session, request, app, grantExpiresAt, configuredPEM != nil)
	if len(text) > 3500 {
		return nil, fmt.Errorf("GitHub capability request is too large to present safely in Telegram")
	}
	pendingRequest := request
	pendingRequest.Passphrase = nil
	pending := &githubPendingRequest{
		ID:              requestID,
		SessionID:       session.ID,
		BindingKey:      githubBindingKey(request.Binding),
		Request:         pendingRequest,
		LocalPassphrase: append([]byte(nil), request.Passphrase...),
		ConfiguredPEM:   configuredPEM,
		ExpiresAt:       now.Add(githubApprovalTTL),
		ApprovalText:    text,
		ApprovalSummary: githubApprovalCompletionSummary(session, request, app),
		State:           "pending",
		Result:          make(chan githubApproval, 1),
		Enrollment:      app,
		GrantExpiresAt:  grantExpiresAt,
	}
	a.githubMu.Lock()
	for _, existing := range a.githubPending {
		if existing.BindingKey == pending.BindingKey && existing.State != "resolved" && existing.ExpiresAt.After(a.githubTime()) {
			a.githubMu.Unlock()
			return nil, fmt.Errorf("this tmux pane already has a pending GitHub capability request")
		}
	}
	a.githubPending[pending.ID] = pending
	a.githubMu.Unlock()

	message, err := a.Telegram.SendHTMLMessage(ctx, a.Config.TelegramChatID, text, session.AnchorMessageID, telegram.GitHubApprovalMarkup(requestID))
	if err != nil {
		a.finishGitHubPending(pending)
		return nil, fmt.Errorf("send GitHub approval request: %w", err)
	}
	a.githubMu.Lock()
	if current := a.githubPending[pending.ID]; current == pending {
		pending.ApprovalMessageID = message.MessageID
	}
	a.githubMu.Unlock()
	a.queueManualRefresh(session.ID)
	_ = a.audit("github.approval", "requested", githubAuditRequest(session.ID, request))
	return pending, nil
}

func (a *App) githubApprovalText(session state.TerminalSession, request githubauth.BrokerRequest, app githubauth.App, grantExpiresAt time.Time, configuredPEM bool) string {
	var text strings.Builder
	text.WriteString(githubApprovalRequestSummary(session, request))
	text.WriteString("\n\n")
	text.WriteString(compactGitHubPermissionLines(request.Permissions))
	if request.Action == githubauth.ActionGrant {
		fmt.Fprintf(&text, "\n<b>For:</b> %s, renewable · until %s", compactGitHubDuration(request.GrantFor),
			html.EscapeString(grantExpiresAt.Local().Format(githubApprovalTimeFormat)))
		fmt.Fprintf(&text, "\n<b>Why:</b> %s", html.EscapeString(request.Purpose))
	} else {
		fmt.Fprintf(&text, "\n<b>Run:</b> <code>%s</code>", html.EscapeString(a.redactText(compactGitHubCommand(request.Command))))
	}
	text.WriteString("\n\nApprove within 15 minutes.")
	if configuredPEM {
		text.WriteString(" The configured local PEM was validated for this request and will be reopened and revalidated after approval.")
	} else if len(request.Passphrase) == 0 && app.TelegramUnlock && !request.LocalUnlock {
		text.WriteString(" The password reply is not end-to-end encrypted.")
	} else {
		text.WriteString(" Unlock happens locally.")
	}

	text.WriteString("\n\n<blockquote expandable><b>Details</b>\n")
	fmt.Fprintf(&text, "Binding: %s / %s / %s\n", html.EscapeString(request.Binding.ServerID),
		html.EscapeString(request.Binding.WindowID), html.EscapeString(request.Binding.PaneID))
	fmt.Fprintf(&text, "App ID: %d\nInstallation: %d\nFingerprint: %s\n",
		app.AppID, app.EffectiveInstallationID(), html.EscapeString(app.PublicFingerprint))
	if configuredPEM {
		text.WriteString("Unlock: configured local PEM\n")
	} else if len(request.Passphrase) == 0 && app.TelegramUnlock && !request.LocalUnlock {
		text.WriteString("Unlock: Telegram reply\n")
	} else {
		text.WriteString("Unlock: local passphrase\n")
	}
	text.WriteString("Repositories:\n")
	for _, repository := range request.Repositories {
		fmt.Fprintf(&text, "  %s\n", html.EscapeString(repository))
	}
	text.WriteString("Permissions:\n")
	names := make([]string, 0, len(request.Permissions))
	for name := range request.Permissions {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(&text, "  %s: %s\n", html.EscapeString(name), html.EscapeString(request.Permissions[name]))
	}
	if request.Action == githubauth.ActionGrant {
		text.WriteString("Scope: Later commands from this pane may use any subset; each child receives a token at this complete ceiling.\n")
		text.WriteString("Renewal: Short-lived tokens may rotate unattended.\n")
		text.WriteString("Memory: The signing capability remains only in Engram memory until this grant ends.\n")
	}
	text.WriteString("Expires unanswered: 15:00</blockquote>")
	return text.String()
}

func githubApprovalRequestSummary(session state.TerminalSession, request githubauth.BrokerRequest) string {
	heading := "GitHub access requested · " + sessionLabel(session)
	return fmt.Sprintf("<b>%s</b>\n\n%s", html.EscapeString(heading), githubApprovalTargetHTML(request.App, request.Repositories))
}

func githubApprovalCompletionSummary(session state.TerminalSession, request githubauth.BrokerRequest, app githubauth.App) string {
	authority := "GitHub lease"
	if request.Action == githubauth.ActionGrant {
		authority = "GitHub grant"
	}
	heading := authority + " · " + sessionLabel(session)
	appLabel := fmt.Sprintf("%s@%d", request.App, app.EffectiveInstallationID())
	var summary strings.Builder
	fmt.Fprintf(&summary, "<b>%s</b>\n\n%s\n\n%s", html.EscapeString(heading),
		githubApprovalTargetHTML(appLabel, request.Repositories), compactGitHubPermissionLines(request.Permissions))
	if request.Action == githubauth.ActionGrant {
		fmt.Fprintf(&summary, "\n<b>Why:</b> %s", html.EscapeString(request.Purpose))
	}
	return summary.String()
}

func githubApprovalTargetHTML(appLabel string, repositories []string) string {
	var targets strings.Builder
	fmt.Fprintf(&targets, "<code>%s</code> → ", html.EscapeString(appLabel))
	for index, repository := range repositories {
		if index > 0 {
			targets.WriteString(", ")
		}
		fmt.Fprintf(&targets, "<code>%s</code>", html.EscapeString(repository))
	}
	return targets.String()
}

func compactGitHubPermissionLines(permissions map[string]string) string {
	labelsByLevel := map[string][]string{"write": {}, "read": {}}
	for name, level := range permissions {
		labelsByLevel[level] = append(labelsByLevel[level], friendlyGitHubPermissionName(name))
	}
	var lines []string
	for _, level := range []string{"write", "read"} {
		labels := labelsByLevel[level]
		if len(labels) == 0 {
			continue
		}
		sort.Strings(labels)
		label := strings.ToUpper(level[:1]) + level[1:]
		lines = append(lines, fmt.Sprintf("<b>%s:</b> %s", label, html.EscapeString(strings.Join(labels, ", "))))
	}
	return strings.Join(lines, "\n")
}

func friendlyGitHubPermissionName(name string) string {
	switch name {
	case "contents":
		return "code"
	case "pull_requests":
		return "pull requests"
	case "repository_projects":
		return "repository projects"
	case "statuses":
		return "commit statuses"
	default:
		return strings.ReplaceAll(name, "_", " ")
	}
}

func compactGitHubDuration(duration time.Duration) string {
	if duration%time.Hour == 0 {
		return fmt.Sprintf("%dh", int64(duration/time.Hour))
	}
	if duration%time.Minute == 0 {
		return strings.TrimSuffix(duration.String(), "0s")
	}
	return duration.String()
}

func (a *App) handleGitHubApprovalCallback(ctx context.Context, cb telegram.CallbackQuery, approve bool, requestID string) string {
	a.githubMu.Lock()
	pending, found := a.githubPending[requestID]
	if !found || pending.ExpiresAt.Before(a.githubTime()) || pending.State == "resolved" {
		a.githubMu.Unlock()
		a.answerCallback(ctx, cb.ID, "request is no longer pending")
		return "callback_user_error"
	}
	if cb.Message == nil || cb.Message.Chat.ID != a.Config.TelegramChatID || cb.Message.MessageID != pending.ApprovalMessageID {
		a.githubMu.Unlock()
		a.answerCallback(ctx, cb.ID, "approval message does not match")
		return "callback_user_error"
	}
	if !approve {
		pending.State = "resolved"
		pending.Result <- githubApproval{failure: githubApprovalDenied, auditStatus: "denied", err: fmt.Errorf("GitHub capability request was denied")}
		a.githubMu.Unlock()
		a.answerCallback(ctx, cb.ID, "denied")
		return "callback_ok"
	}
	if pending.State != "pending" {
		a.githubMu.Unlock()
		a.answerCallback(ctx, cb.ID, "unlock is already pending")
		return "callback_user_error"
	}
	appAlias := pending.Request.App
	app, appErr := a.reloadMatchingGitHubEnrollment(pending.Enrollment)
	if appErr != nil {
		pending.State = "resolved"
		pending.Result <- githubApproval{failure: githubApprovalCanceled, auditStatus: "enrollment_changed", err: appErr}
		a.githubMu.Unlock()
		a.answerCallback(ctx, cb.ID, "app enrollment changed")
		return "callback_user_error"
	}
	if pending.ConfiguredPEM != nil {
		if err := a.validateConfiguredGitHubAppPEM(pending); err != nil {
			pending.State = "resolved"
			pending.Result <- githubApproval{failure: githubApprovalCanceled, auditStatus: "credential_invalidated", err: err}
			a.githubMu.Unlock()
			a.answerCallback(ctx, cb.ID, "configured local PEM changed or is unavailable")
			return "callback_user_error"
		}
		pending.State = "resolved"
		pending.Result <- githubApproval{configuredPEM: true}
		a.githubMu.Unlock()
		a.answerCallback(ctx, cb.ID, "approved")
		return "callback_ok"
	}
	if len(pending.LocalPassphrase) > 0 {
		passphrase := append([]byte(nil), pending.LocalPassphrase...)
		githubauth.Zero(pending.LocalPassphrase)
		pending.LocalPassphrase = nil
		pending.State = "resolved"
		pending.Result <- githubApproval{passphrase: passphrase}
		a.githubMu.Unlock()
		a.answerCallback(ctx, cb.ID, "approved")
		return "callback_ok"
	}
	if !app.TelegramUnlock {
		pending.State = "resolved"
		pending.Result <- githubApproval{failure: githubApprovalFailed, auditStatus: "unlock_route_failed", err: fmt.Errorf("this GitHub App requires local passphrase entry")}
		a.githubMu.Unlock()
		a.answerCallback(ctx, cb.ID, "local passphrase is required")
		return "callback_user_error"
	}
	pending.State = "unlocking"
	pendingID := pending.ID
	approvalMessageID := pending.ApprovalMessageID
	a.githubMu.Unlock()

	prompt, err := a.Telegram.SendForceReply(
		ctx,
		a.Config.TelegramChatID,
		"Reply with the passphrase for GitHub App "+appAlias+". Telegram bot chats are not end-to-end encrypted; this reply will be deleted immediately.",
		approvalMessageID,
		"GitHub App passphrase",
	)
	if err != nil {
		a.resolveGitHubPending(pendingID, githubApproval{failure: githubApprovalFailed, auditStatus: "unlock_prompt_failed", err: fmt.Errorf("send GitHub unlock prompt: %w", err)})
		a.answerCallback(ctx, cb.ID, "could not request passphrase")
		return "callback_telegram_failed"
	}
	a.githubMu.Lock()
	active := false
	if current := a.githubPending[pendingID]; current == pending && pending.State == "unlocking" {
		pending.UnlockMessageID = prompt.MessageID
		active = true
	}
	a.githubMu.Unlock()
	if !active {
		a.rememberGitHubUnlockPrompt(prompt.MessageID)
		a.deleteGitHubUnlockMessages(prompt.MessageID, 0)
		a.answerCallback(ctx, cb.ID, "request was canceled")
		return "callback_user_error"
	}
	a.answerCallback(ctx, cb.ID, "approved; reply with the passphrase")
	return "callback_ok"
}

func (a *App) handleGitHubUnlockReply(ctx context.Context, message telegram.Message) (string, bool) {
	if message.ReplyToMessage == nil || message.Text == "" {
		return "", false
	}
	a.githubMu.Lock()
	var pending *githubPendingRequest
	replyToMessageID := message.ReplyToMessage.MessageID
	for _, candidate := range a.githubPending {
		if candidate.State == "unlocking" && candidate.UnlockMessageID != 0 &&
			candidate.UnlockMessageID == replyToMessageID &&
			candidate.ExpiresAt.After(a.githubTime()) {
			pending = candidate
			candidate.State = "resolved"
			break
		}
	}
	if pending != nil {
		a.rememberGitHubUnlockPromptLocked(replyToMessageID)
	}
	tombstoned := a.githubUnlockTombstones[replyToMessageID].After(a.githubTime())
	a.githubMu.Unlock()
	if pending == nil {
		if tombstoned {
			a.deleteGitHubUnlockMessages(replyToMessageID, message.MessageID)
			return "github_unlock_expired", true
		}
		return "", false
	}

	deleteCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	if err := a.Telegram.DeleteMessage(deleteCtx, message.Chat.ID, message.MessageID); err != nil {
		_ = a.audit("github.unlock_message", "delete_failed", map[string]any{"message_id": message.MessageID, "error": err.Error()})
	}
	if pending.UnlockMessageID != 0 {
		_ = a.Telegram.DeleteMessage(deleteCtx, message.Chat.ID, pending.UnlockMessageID)
	}
	cancel()

	passphrase := []byte(message.Text)
	select {
	case pending.Result <- githubApproval{passphrase: passphrase}:
	default:
		githubauth.Zero(passphrase)
	}
	return "github_unlock_received", true
}

func (a *App) resolveGitHubPending(requestID string, result githubApproval) {
	a.githubMu.Lock()
	pending, ok := a.githubPending[requestID]
	if ok && pending.State != "resolved" {
		pending.State = "resolved"
		select {
		case pending.Result <- result:
		default:
			githubauth.Zero(result.passphrase)
		}
	} else {
		githubauth.Zero(result.passphrase)
	}
	a.githubMu.Unlock()
}

func (a *App) finishGitHubPending(pending *githubPendingRequest) {
	if pending == nil {
		return
	}
	a.githubMu.Lock()
	if current := a.githubPending[pending.ID]; current == pending {
		delete(a.githubPending, pending.ID)
	}
	githubauth.Zero(pending.LocalPassphrase)
	pending.LocalPassphrase = nil
	sessionID := pending.SessionID
	unlockMessageID := pending.UnlockMessageID
	if unlockMessageID != 0 {
		a.rememberGitHubUnlockPromptLocked(unlockMessageID)
	}
	a.githubMu.Unlock()
	if unlockMessageID != 0 {
		a.deleteGitHubUnlockMessages(unlockMessageID, 0)
	}
	a.queueManualRefresh(sessionID)
}

func (a *App) rememberGitHubUnlockPrompt(messageID int) {
	if messageID == 0 {
		return
	}
	a.githubMu.Lock()
	a.rememberGitHubUnlockPromptLocked(messageID)
	a.githubMu.Unlock()
}

func (a *App) rememberGitHubUnlockPromptLocked(messageID int) {
	if messageID == 0 {
		return
	}
	if a.githubUnlockTombstones == nil {
		a.githubUnlockTombstones = map[int]time.Time{}
	}
	now := a.githubTime()
	for id, expiresAt := range a.githubUnlockTombstones {
		if !expiresAt.After(now) {
			delete(a.githubUnlockTombstones, id)
		}
	}
	a.githubUnlockTombstones[messageID] = now.Add(githubUnlockTombstoneTTL)
}

func (a *App) deleteGitHubUnlockMessages(promptMessageID, replyMessageID int) {
	deleteCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if replyMessageID != 0 {
		if err := a.Telegram.DeleteMessage(deleteCtx, a.Config.TelegramChatID, replyMessageID); err != nil {
			_ = a.audit("github.unlock_message", "delete_failed", map[string]any{"message_id": replyMessageID, "error": err.Error()})
		}
	}
	if promptMessageID != 0 {
		_ = a.Telegram.DeleteMessage(deleteCtx, a.Config.TelegramChatID, promptMessageID)
	}
}

func (a *App) completeGitHubApprovalMessage(pending *githubPendingRequest, text string) {
	if pending == nil {
		return
	}
	a.githubMu.Lock()
	messageID := pending.ApprovalMessageID
	approvalText := pending.ApprovalText
	approvalSummary := pending.ApprovalSummary
	a.githubMu.Unlock()
	if messageID == 0 {
		return
	}
	editCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if approvalSummary != "" {
		text = approvalSummary + "\n\n" + html.EscapeString(text)
	} else if approvalText != "" {
		text = approvalText + "\n\n" + html.EscapeString(text)
	} else {
		text = html.EscapeString(text)
	}
	_, _ = a.Telegram.EditHTMLMessage(editCtx, a.Config.TelegramChatID, messageID, text, telegram.ClearMarkup())
}

func (a *App) reusableGitHubLease(request githubauth.BrokerRequest, enrollment githubauth.App) (githubLease, bool) {
	key := githubBindingKey(request.Binding)
	now := a.githubTime()
	a.githubMu.Lock()
	defer a.githubMu.Unlock()
	lease, ok := a.githubLeases[key]
	if !ok || !lease.Info.ExpiresAt.After(now) || lease.Info.App != request.App || !sameGitHubEnrollment(lease.Enrollment, enrollment) ||
		!githubauth.RepositoriesSubset(request.Repositories, lease.Info.Repositories) ||
		!githubauth.PermissionsSubset(request.Permissions, lease.Info.Permissions) {
		return githubLease{}, false
	}
	return lease, true
}

func (a *App) discardGitHubLease(binding githubauth.Binding, token string) bool {
	key := githubBindingKey(binding)
	a.githubMu.Lock()
	defer a.githubMu.Unlock()
	lease, ok := a.githubLeases[key]
	if !ok || lease.Token != token {
		return false
	}
	delete(a.githubLeases, key)
	return true
}

func (a *App) reloadMatchingGitHubEnrollment(expected githubauth.App) (githubauth.App, error) {
	if a.GitHubVault == nil {
		return githubauth.App{}, fmt.Errorf("GitHub App capabilities are unavailable")
	}
	if err := a.GitHubVault.Reload(); err != nil {
		return githubauth.App{}, fmt.Errorf("reload GitHub App vault: %w", err)
	}
	current, found := a.GitHubVault.Get(expected.Alias)
	if !found {
		return githubauth.App{}, fmt.Errorf("GitHub App %q is no longer enrolled", expected.Alias)
	}
	current, matches := matchingCurrentGitHubEnrollment(current, expected)
	if !matches {
		return githubauth.App{}, fmt.Errorf("GitHub App %q enrollment changed", expected.Alias)
	}
	return current, nil
}

func (a *App) resolveConfiguredGitHubAppPEM(selected githubauth.App) (*githubauth.PrivateKeyFileIdentity, error) {
	alias := strings.TrimSpace(a.Config.GitHubAppPEMAlias)
	path := strings.TrimSpace(a.Config.GitHubAppPEMPath)
	if alias == "" && path == "" {
		return nil, nil
	}
	if selected.Alias != alias {
		return nil, nil
	}
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("configured local GitHub App PEM path must be absolute")
	}
	privateKey, identity, err := githubauth.ReadPrivateKeyFile(path)
	if err != nil {
		return nil, fmt.Errorf("configured local GitHub App PEM is unavailable: %w", err)
	}
	defer githubauth.Zero(privateKey)

	state := a.configuredGitHubAppPEMIdentityState(selected, identity)
	if err := configuredGitHubAppPEMStateError(state); err != nil {
		return nil, err
	}
	return &identity, nil
}

func (a *App) readMatchingConfiguredGitHubAppPEM(pending *githubPendingRequest) ([]byte, error) {
	if pending == nil || pending.ConfiguredPEM == nil {
		return nil, fmt.Errorf("configured local GitHub App PEM was not bound to this approval")
	}
	if strings.TrimSpace(a.Config.GitHubAppPEMAlias) != pending.Enrollment.Alias {
		return nil, fmt.Errorf("configured local GitHub App PEM alias changed")
	}
	privateKey, identity, err := githubauth.ReadPrivateKeyFile(a.Config.GitHubAppPEMPath)
	if err != nil {
		return nil, fmt.Errorf("configured local GitHub App PEM is unavailable: %w", err)
	}
	state := a.configuredGitHubAppPEMIdentityState(pending.Enrollment, identity)
	if state != configuredGitHubAppPEMReady || !pending.ConfiguredPEM.Equal(identity) {
		githubauth.Zero(privateKey)
		if stateErr := configuredGitHubAppPEMStateError(state); stateErr != nil {
			return nil, stateErr
		}
		return nil, fmt.Errorf("configured local GitHub App PEM changed")
	}
	return privateKey, nil
}

func (a *App) configuredGitHubAppPEMIdentityState(selected githubauth.App, identity githubauth.PrivateKeyFileIdentity) configuredGitHubAppPEMState {
	if identity.Fingerprint != selected.PublicFingerprint {
		return configuredGitHubAppPEMUnmatched
	}
	matches := 0
	for _, enrollment := range a.GitHubVault.List() {
		if enrollment.PublicFingerprint == identity.Fingerprint {
			matches++
		}
	}
	if matches > 1 {
		return configuredGitHubAppPEMAmbiguous
	}
	if matches != 1 {
		return configuredGitHubAppPEMUnmatched
	}
	return configuredGitHubAppPEMReady
}

func configuredGitHubAppPEMStateError(state configuredGitHubAppPEMState) error {
	switch state {
	case configuredGitHubAppPEMReady:
		return nil
	case configuredGitHubAppPEMAmbiguous:
		return fmt.Errorf("configured local GitHub App PEM matches multiple enrolled Apps")
	case configuredGitHubAppPEMUnmatched:
		return fmt.Errorf("configured local GitHub App PEM does not match the configured enrollment")
	default:
		return fmt.Errorf("configured local GitHub App PEM is unavailable")
	}
}

func (a *App) configuredGitHubAppPEMStatus() string {
	alias := strings.TrimSpace(a.Config.GitHubAppPEMAlias)
	path := strings.TrimSpace(a.Config.GitHubAppPEMPath)
	if alias == "" && path == "" {
		return string(configuredGitHubAppPEMDisabled)
	}
	if alias == "" || path == "" || a.GitHubVault == nil || !filepath.IsAbs(path) {
		return string(configuredGitHubAppPEMUnavailable)
	}
	if err := a.GitHubVault.Reload(); err != nil {
		return string(configuredGitHubAppPEMUnavailable)
	}
	selected, found := a.GitHubVault.Get(alias)
	if !found {
		return string(configuredGitHubAppPEMUnmatched)
	}
	privateKey, identity, err := githubauth.ReadPrivateKeyFile(path)
	githubauth.Zero(privateKey)
	if err != nil {
		return string(configuredGitHubAppPEMUnavailable)
	}
	state := a.configuredGitHubAppPEMIdentityState(selected, identity)
	if state == configuredGitHubAppPEMReady {
		return "ready for alias " + alias
	}
	return string(state)
}

func (a *App) validateConfiguredGitHubAppPEM(pending *githubPendingRequest) error {
	if pending == nil || pending.ConfiguredPEM == nil {
		return nil
	}
	privateKey, err := a.readMatchingConfiguredGitHubAppPEM(pending)
	githubauth.Zero(privateKey)
	return err
}

func (a *App) cancelConfiguredGitHubAppPEM(
	pending *githubPendingRequest,
	eventType string,
	message string,
	sessionID int,
	request githubauth.BrokerRequest,
) {
	a.completeGitHubApprovalMessage(pending, message)
	_ = a.audit(eventType, "credential_invalidated", githubAuditRequest(sessionID, request))
}

func matchingCurrentGitHubEnrollment(current, expected githubauth.App) (githubauth.App, bool) {
	selected, err := current.SelectInstallation(expected.EffectiveInstallationID())
	return selected, err == nil && sameGitHubEnrollment(selected, expected)
}

func sameGitHubEnrollment(left, right githubauth.App) bool {
	return left.Alias == right.Alias &&
		left.AppID == right.AppID &&
		left.InstallationID == right.InstallationID &&
		left.EffectiveInstallationID() == right.EffectiveInstallationID() &&
		sameInt64Set(left.Installations(), right.Installations()) &&
		left.CredentialIdentityVersion == right.CredentialIdentityVersion &&
		left.TelegramUnlock == right.TelegramUnlock &&
		left.PublicFingerprint == right.PublicFingerprint &&
		left.CreatedAt.Equal(right.CreatedAt)
}

func sameInt64Set(left, right []int64) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func (a *App) storeGitHubLease(lease githubLease) []githubLease {
	key := githubBindingKey(lease.Binding)
	a.githubMu.Lock()
	defer a.githubMu.Unlock()
	var old []githubLease
	if previous, ok := a.githubLeases[key]; ok && previous.Token != lease.Token {
		old = append(old, previous)
	}
	a.githubLeases[key] = lease
	return old
}

func (a *App) githubLeaseInfos(binding githubauth.Binding) []githubauth.LeaseInfo {
	now := a.githubTime()
	key := githubBindingKey(binding)
	a.githubMu.Lock()
	defer a.githubMu.Unlock()
	lease, ok := a.githubLeases[key]
	if !ok || !lease.Info.ExpiresAt.After(now) {
		return nil
	}
	info := lease.Info
	info.Repositories = append([]string(nil), info.Repositories...)
	info.Permissions = copyStringMap(info.Permissions)
	return []githubauth.LeaseInfo{info}
}

func (a *App) githubLeaseCount() int {
	now := a.githubTime()
	a.githubMu.Lock()
	defer a.githubMu.Unlock()
	count := 0
	for _, lease := range a.githubLeases {
		if lease.Info.ExpiresAt.After(now) {
			count++
		}
	}
	return count
}

func (a *App) githubGrantCount() int {
	now := a.githubTime()
	a.githubMu.Lock()
	defer a.githubMu.Unlock()
	count := 0
	for _, grant := range a.githubGrants {
		if grant.Info.ExpiresAt.After(now) {
			count++
		}
	}
	return count
}

func (a *App) revokeGitHubBindingLeases(ctx context.Context, sessionID int, binding githubauth.Binding) {
	key := githubBindingKey(binding)
	a.githubMu.Lock()
	lease, found := a.githubLeases[key]
	if found {
		delete(a.githubLeases, key)
	}
	a.githubMu.Unlock()
	if found {
		revokeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		if a.GitHubMinter != nil {
			_ = a.GitHubMinter.Revoke(revokeCtx, lease.Token)
		}
		cancel()
		a.queueManualRefresh(sessionID)
		_ = a.audit("github.lease", "revoked", map[string]any{"session_id": sessionID, "app": lease.Info.App, "installation_id": lease.Info.InstallationID})
	}
}

func (a *App) revokeGitHubBindingAuthority(ctx context.Context, sessionID int, binding githubauth.Binding) error {
	handle := a.githubGrantLocks.handle(sessionID)
	handle.Lock()
	defer handle.Unlock()
	key := githubBindingKey(binding)
	a.githubMu.Lock()
	grant, granted := a.githubGrants[key]
	if granted {
		githubauth.Zero(grant.PrivateKey)
		delete(a.githubGrants, key)
	}
	lease, leased := a.githubLeases[key]
	if leased {
		delete(a.githubLeases, key)
	}
	a.githubMu.Unlock()
	if granted || leased {
		a.queueManualRefresh(sessionID)
	}
	if granted {
		_ = a.audit("github.grant", "revoked", map[string]any{"session_id": sessionID, "app": grant.Info.App, "installation_id": grant.Info.InstallationID, "grant_id": grant.Info.ID})
	}
	if leased && a.GitHubMinter != nil {
		revokeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := a.GitHubMinter.Revoke(revokeCtx, lease.Token)
		cancel()
		if err != nil {
			a.trackGitHubRevocation(lease.Token, sessionID, lease.Info.App, lease.Info.InstallationID, lease.Info.ExpiresAt)
			_ = a.audit("github.lease", "revoke_failed", map[string]any{"session_id": sessionID, "app": lease.Info.App, "installation_id": lease.Info.InstallationID, "error": err.Error()})
			return fmt.Errorf("GitHub authority was removed locally, but remote token revocation is pending: %w", err)
		}
	}
	if leased {
		_ = a.audit("github.lease", "revoked", map[string]any{"session_id": sessionID, "app": lease.Info.App, "installation_id": lease.Info.InstallationID})
	}
	return nil
}

func (a *App) expireGitHubLeases(now time.Time) {
	activeBindings := map[string]state.TerminalSession{}
	for _, session := range a.Store.Snapshot().TerminalSessions {
		if session.State == state.TerminalRunning && session.WatchEnabled {
			activeBindings[githubBindingKey(githubauth.Binding{
				ServerID: session.TmuxServerID,
				WindowID: session.TmuxWindowID,
				PaneID:   session.TmuxPaneID,
			})] = session
		}
	}
	a.githubMu.Lock()
	hasAuthority := len(a.githubGrants) != 0 || len(a.githubLeases) != 0
	a.githubMu.Unlock()
	enrollments := map[string]githubauth.App{}
	enrollmentsValid := !hasAuthority || a.GitHubVault != nil
	if hasAuthority && enrollmentsValid {
		if err := a.GitHubVault.Reload(); err != nil {
			enrollmentsValid = false
		} else {
			for _, enrollment := range a.GitHubVault.List() {
				enrollments[enrollment.Alias] = enrollment
			}
		}
	}
	var expired, invalidated []githubLease
	var expiredGrants, invalidatedGrants []githubGrant
	a.githubMu.Lock()
	for key, grant := range a.githubGrants {
		current, enrolled := enrollments[grant.Enrollment.Alias]
		active, bound := activeBindings[key]
		_, enrollmentMatches := matchingCurrentGitHubEnrollment(current, grant.Enrollment)
		reasonInvalid := !bound || active.ID != grant.SessionID || !active.CreatedAt.Equal(grant.SessionCreatedAt) ||
			!enrollmentsValid || !enrolled || !enrollmentMatches
		if !grant.Info.ExpiresAt.After(now) {
			githubauth.Zero(grant.PrivateKey)
			expiredGrants = append(expiredGrants, grant)
			delete(a.githubGrants, key)
		} else if reasonInvalid {
			githubauth.Zero(grant.PrivateKey)
			invalidatedGrants = append(invalidatedGrants, grant)
			delete(a.githubGrants, key)
		}
		if _, remains := a.githubGrants[key]; !remains {
			if lease, found := a.githubLeases[key]; found {
				invalidated = append(invalidated, lease)
				delete(a.githubLeases, key)
			}
		}
	}
	for key, lease := range a.githubLeases {
		current, enrolled := enrollments[lease.Enrollment.Alias]
		_, enrollmentMatches := matchingCurrentGitHubEnrollment(current, lease.Enrollment)
		enrollmentInvalid := !enrollmentsValid || !enrolled || !enrollmentMatches
		if !lease.Info.ExpiresAt.After(now) {
			expired = append(expired, lease)
			delete(a.githubLeases, key)
		} else if _, active := activeBindings[key]; !active || enrollmentInvalid {
			invalidated = append(invalidated, lease)
			delete(a.githubLeases, key)
		}
	}
	a.githubMu.Unlock()
	for _, grant := range expiredGrants {
		a.queueManualRefresh(grant.SessionID)
		_ = a.audit("github.grant", "expired", map[string]any{"session_id": grant.SessionID, "app": grant.Info.App, "installation_id": grant.Info.InstallationID, "grant_id": grant.Info.ID})
	}
	for _, grant := range invalidatedGrants {
		a.queueManualRefresh(grant.SessionID)
		_ = a.audit("github.grant", "invalidated", map[string]any{"session_id": grant.SessionID, "app": grant.Info.App, "installation_id": grant.Info.InstallationID, "grant_id": grant.Info.ID})
	}
	for _, lease := range expired {
		a.queueManualRefresh(lease.SessionID)
		_ = a.audit("github.lease", "expired", map[string]any{"session_id": lease.SessionID, "app": lease.Info.App, "installation_id": lease.Info.InstallationID})
	}
	for _, lease := range invalidated {
		a.queueManualRefresh(lease.SessionID)
		_ = a.audit("github.lease", "invalidated", map[string]any{"session_id": lease.SessionID, "app": lease.Info.App, "installation_id": lease.Info.InstallationID})
	}
	a.revokeGitHubLeases(invalidated)
}

func (a *App) githubStatusLine(session state.TerminalSession) string {
	now := a.githubTime()
	key := githubBindingKey(githubauth.Binding{ServerID: session.TmuxServerID, WindowID: session.TmuxWindowID, PaneID: session.TmuxPaneID})
	a.githubMu.Lock()
	defer a.githubMu.Unlock()
	if grant, ok := a.githubGrants[key]; ok {
		return githubauth.CompactGrantLine(grant.Info, now)
	}
	if lease, ok := a.githubLeases[key]; ok {
		return githubauth.CompactLeaseLine(lease.Info, now)
	}
	for _, pending := range a.githubPending {
		if pending.BindingKey == key && pending.ExpiresAt.After(now) && pending.State != "resolved" {
			remaining := pending.ExpiresAt.Sub(now).Round(time.Second)
			return fmt.Sprintf("GH approval pending · %d:%02d", int(remaining/time.Minute), int(remaining/time.Second)%60)
		}
	}
	return ""
}

func (a *App) revokeGitHubLeases(leases []githubLease) {
	if len(leases) == 0 || a.GitHubMinter == nil {
		return
	}
	toRevoke := make([]githubLease, 0, len(leases))
	seen := make(map[string]bool, len(leases))
	for _, lease := range leases {
		if lease.Token == "" || seen[lease.Token] {
			continue
		}
		seen[lease.Token] = true
		a.trackGitHubRevocation(lease.Token, lease.SessionID, lease.Info.App, lease.Info.InstallationID, lease.Info.ExpiresAt)
		toRevoke = append(toRevoke, lease)
	}
	if len(toRevoke) == 0 {
		return
	}
	a.transferWG.Add(1)
	go func() {
		defer a.transferWG.Done()
		for _, lease := range toRevoke {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			err := a.GitHubMinter.Revoke(ctx, lease.Token)
			cancel()
			if err == nil {
				a.githubMu.Lock()
				delete(a.githubRevocations, lease.Token)
				a.githubMu.Unlock()
			} else {
				a.trackGitHubRevocation(lease.Token, lease.SessionID, lease.Info.App, lease.Info.InstallationID, lease.Info.ExpiresAt)
			}
		}
	}()
}

func (a *App) trackGitHubRevocation(token string, sessionID int, app string, installationID int64, expiresAt time.Time) bool {
	if token == "" {
		return false
	}
	if !expiresAt.After(a.githubTime()) {
		expiresAt = a.githubTime().Add(time.Hour)
	}
	a.githubMu.Lock()
	if a.githubRevocations == nil {
		a.githubRevocations = map[string]githubRevocation{}
	}
	pending, exists := a.githubRevocations[token]
	if !exists && len(a.githubRevocations) >= maxTrackedGitHubTokens {
		a.githubMu.Unlock()
		_ = a.audit("github.revocation", "capacity_exhausted", map[string]any{
			"session_id":      sessionID,
			"app":             app,
			"installation_id": installationID,
		})
		return false
	}
	pending.Token = token
	if sessionID != 0 {
		pending.SessionID = sessionID
	}
	if app != "" {
		pending.App = app
	}
	if installationID > 0 {
		pending.InstallationID = installationID
	}
	pending.ExpiresAt = expiresAt
	pending.Attempts++
	pending.NextAttempt = a.githubTime().Add(min(time.Duration(pending.Attempts)*5*time.Second, time.Minute))
	a.githubRevocations[token] = pending
	a.githubMu.Unlock()
	return true
}

func (a *App) reserveGitHubTokenSlot() error {
	now := a.githubTime()
	a.githubMu.Lock()
	defer a.githubMu.Unlock()
	for token, pending := range a.githubRevocations {
		if !pending.ExpiresAt.After(now) {
			delete(a.githubRevocations, token)
		}
	}
	tracked := len(a.githubLeases) + len(a.githubRevocations) + a.githubTokenReservations
	if tracked >= maxTrackedGitHubTokens {
		return fmt.Errorf("GitHub token capacity of %d is full; wait for pending revocations or leases to expire", maxTrackedGitHubTokens)
	}
	a.githubTokenReservations++
	return nil
}

func (a *App) releaseGitHubTokenSlot() {
	a.githubMu.Lock()
	defer a.githubMu.Unlock()
	if a.githubTokenReservations > 0 {
		a.githubTokenReservations--
	}
}

func (a *App) retryGitHubRevocations(now time.Time) {
	if a.GitHubMinter == nil {
		return
	}
	var due []githubRevocation
	a.githubMu.Lock()
	for token, pending := range a.githubRevocations {
		if !pending.ExpiresAt.After(now) {
			delete(a.githubRevocations, token)
			continue
		}
		if !pending.NextAttempt.After(now) {
			pending.NextAttempt = now.Add(time.Minute)
			a.githubRevocations[token] = pending
			due = append(due, pending)
		}
	}
	a.githubMu.Unlock()
	if len(due) == 0 {
		return
	}
	a.transferWG.Add(1)
	go func() {
		defer a.transferWG.Done()
		for _, pending := range due {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			err := a.GitHubMinter.Revoke(ctx, pending.Token)
			cancel()
			if err == nil {
				a.githubMu.Lock()
				delete(a.githubRevocations, pending.Token)
				a.githubMu.Unlock()
				_ = a.audit("github.lease", "revoke_recovered", map[string]any{"session_id": pending.SessionID, "app": pending.App, "installation_id": pending.InstallationID})
				continue
			}
			a.trackGitHubRevocation(pending.Token, pending.SessionID, pending.App, pending.InstallationID, pending.ExpiresAt)
		}
	}()
}

func (a *App) githubRevocationCount() int {
	a.githubMu.Lock()
	defer a.githubMu.Unlock()
	return len(a.githubRevocations)
}

func (a *App) revokeAllGitHubLeases() {
	a.githubMu.Lock()
	leases := make([]githubLease, 0, len(a.githubLeases)+len(a.githubRevocations))
	for key, lease := range a.githubLeases {
		leases = append(leases, lease)
		delete(a.githubLeases, key)
	}
	for key, grant := range a.githubGrants {
		githubauth.Zero(grant.PrivateKey)
		delete(a.githubGrants, key)
	}
	for token, pending := range a.githubRevocations {
		leases = append(leases, githubLease{
			SessionID: pending.SessionID,
			Info: githubauth.LeaseInfo{
				App: pending.App, InstallationID: pending.InstallationID, ExpiresAt: pending.ExpiresAt,
			},
			Token: pending.Token,
		})
		delete(a.githubRevocations, token)
	}
	a.githubMu.Unlock()
	a.revokeGitHubLeases(leases)
}

func githubRequestID() (string, error) {
	data := make([]byte, 12)
	if _, err := rand.Read(data); err != nil {
		return "", fmt.Errorf("generate GitHub capability request ID: %w", err)
	}
	return hex.EncodeToString(data), nil
}

func githubBindingKey(binding githubauth.Binding) string {
	return binding.ServerID + "\x00" + binding.WindowID + "\x00" + binding.PaneID
}

func githubAuditRequest(sessionID int, request githubauth.BrokerRequest) map[string]any {
	fields := map[string]any{
		"session_id":      sessionID,
		"app":             request.App,
		"installation_id": request.InstallationID,
		"repositories":    append([]string(nil), request.Repositories...),
		"permissions":     copyStringMap(request.Permissions),
	}
	if len(request.Command) != 0 {
		fields["command"] = filepath.Base(request.Command[0])
	}
	return fields
}

func compactGitHubCommand(command []string) string {
	quoted := make([]string, len(command))
	for index, argument := range command {
		quoted[index] = strconv.Quote(argument)
	}
	return strings.Join(quoted, " ")
}

func sessionLabel(session state.TerminalSession) string {
	title := strings.Join(strings.Fields(firstNonEmpty(session.Title, "terminal")), " ")
	return fmt.Sprintf("[%d] %s", session.ID, title)
}

func copyStringMap(input map[string]string) map[string]string {
	out := make(map[string]string, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

func (a *App) githubTime() time.Time {
	if a.githubNow != nil {
		return a.githubNow()
	}
	return time.Now()
}
