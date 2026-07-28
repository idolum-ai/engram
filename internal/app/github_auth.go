package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
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

const githubApprovalTTL = 3 * time.Minute
const githubUnlockTombstoneTTL = 10 * time.Minute

type githubApproval struct {
	passphrase []byte
	err        error
}

type githubPendingRequest struct {
	ID                string
	SessionID         int
	BindingKey        string
	Request           githubauth.BrokerRequest
	LocalPassphrase   []byte
	ExpiresAt         time.Time
	ApprovalMessageID int
	ApprovalText      string
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
	Token       string
	App         string
	SessionID   int
	ExpiresAt   time.Time
	NextAttempt time.Time
	Attempts    int
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
	if request.Action == githubauth.ActionExec {
		if lease, ok := a.reusableGitHubLease(request, app); ok && lease.Info.GrantID == "" {
			if err := a.validateGitHubBrokerContinuation(ctx, session, request.Binding); err != nil {
				if a.discardGitHubLease(request.Binding, lease.Token) {
					err = a.revokeDiscardedGitHubToken(ctx, lease.Token, err)
				}
				_ = a.audit("github.lease", "invalidated", githubAuditRequest(session.ID, request))
				return githubauth.BrokerResponse{Error: err.Error()}
			}
			if _, err := a.reloadMatchingGitHubEnrollment(lease.Enrollment); err != nil {
				if a.discardGitHubLease(request.Binding, lease.Token) {
					err = a.revokeDiscardedGitHubToken(ctx, lease.Token, err)
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
	if (!app.TelegramUnlock || request.LocalUnlock) && len(request.Passphrase) == 0 {
		return githubauth.BrokerResponse{Error: "this GitHub App requires local passphrase entry", ErrorCode: githubauth.ErrorCodeLocalPassphraseRequired}
	}
	pending, err := a.beginGitHubApproval(ctx, session, request, app)
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
		a.completeGitHubApprovalMessage(pending, "Expired: no approval was received within three minutes.")
		_ = a.audit("github.approval", "expired", githubAuditRequest(session.ID, request))
		return githubauth.BrokerResponse{Error: "GitHub capability request expired"}
	case approval = <-pending.Result:
	}
	if approval.err != nil {
		a.completeGitHubApprovalMessage(pending, "Denied: no GitHub capability was granted.")
		_ = a.audit("github.approval", "denied", githubAuditRequest(session.ID, request))
		return githubauth.BrokerResponse{Error: approval.err.Error()}
	}
	defer githubauth.Zero(approval.passphrase)
	if err := a.validateGitHubBrokerContinuation(ctx, session, request.Binding); err != nil {
		a.completeGitHubApprovalMessage(pending, "Canceled: the requesting tmux pane is no longer valid.")
		_ = a.audit("github.approval", "invalidated", githubAuditRequest(session.ID, request))
		return githubauth.BrokerResponse{Error: err.Error()}
	}
	if _, err := a.reloadMatchingGitHubEnrollment(pending.Enrollment); err != nil {
		a.completeGitHubApprovalMessage(pending, "Canceled: the GitHub App enrollment changed during approval.")
		_ = a.audit("github.approval", "enrollment_changed", githubAuditRequest(session.ID, request))
		return githubauth.BrokerResponse{Error: err.Error()}
	}

	privateKey, unlockedApp, err := a.GitHubVault.Unlock(request.App, approval.passphrase)
	if err != nil {
		a.completeGitHubApprovalMessage(pending, "Failed: the GitHub App credential could not be unlocked.")
		_ = a.audit("github.unlock", "failed", githubAuditRequest(session.ID, request))
		return githubauth.BrokerResponse{Error: githubauth.ErrUnlock.Error()}
	}
	defer githubauth.Zero(privateKey)
	if a.GitHubMinter == nil {
		return githubauth.BrokerResponse{Error: "GitHub token minting is unavailable"}
	}
	if request.Action == githubauth.ActionGrant {
		return a.createGitHubGrant(ctx, session, pending, request, unlockedApp, privateKey)
	}
	mintCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	token, err := a.GitHubMinter.Mint(mintCtx, unlockedApp, privateKey, request.Repositories, request.Permissions)
	cancel()
	if err != nil {
		a.completeGitHubApprovalMessage(pending, "Failed: GitHub rejected the scoped capability request.")
		_ = a.audit("github.mint", "failed", map[string]any{
			"session_id": session.ID,
			"app":        request.App,
			"error":      err.Error(),
		})
		return githubauth.BrokerResponse{Error: err.Error()}
	}
	lease := githubLease{
		SessionID:  session.ID,
		Binding:    request.Binding,
		Enrollment: unlockedApp,
		Info: githubauth.LeaseInfo{
			App:          request.App,
			Repositories: append([]string(nil), request.Repositories...),
			Permissions:  copyStringMap(request.Permissions),
			ExpiresAt:    token.ExpiresAt,
		},
		Token: token.Value,
	}
	if err := a.validateGitHubBrokerContinuation(ctx, session, request.Binding); err != nil {
		cleanupErr := a.revokeDiscardedGitHubToken(ctx, token.Value, err)
		a.completeGitHubApprovalMessage(pending, "Canceled: the requesting tmux pane changed before the capability could be delivered.")
		_ = a.audit("github.mint", "discarded", map[string]any{
			"session_id": session.ID,
			"app":        request.App,
			"error":      cleanupErr.Error(),
		})
		return githubauth.BrokerResponse{Error: cleanupErr.Error()}
	}
	if _, err := a.reloadMatchingGitHubEnrollment(pending.Enrollment); err != nil {
		cleanupErr := a.revokeDiscardedGitHubToken(ctx, token.Value, err)
		a.completeGitHubApprovalMessage(pending, "Canceled: the GitHub App enrollment changed before the capability could be delivered.")
		_ = a.audit("github.mint", "discarded", map[string]any{
			"session_id": session.ID,
			"app":        request.App,
			"error":      cleanupErr.Error(),
		})
		return githubauth.BrokerResponse{Error: cleanupErr.Error()}
	}
	oldTokens := a.storeGitHubLease(lease)
	a.revokeGitHubTokens(oldTokens)
	a.queueManualRefresh(session.ID)
	a.completeGitHubApprovalMessage(pending, fmt.Sprintf(
		"Approved: %s now has GitHub access to %d %s until %s.",
		sessionLabel(session), len(request.Repositories), plural(len(request.Repositories), "repository", "repositories"),
		token.ExpiresAt.Local().Format("15:04 MST"),
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

func (a *App) revokeDiscardedGitHubToken(ctx context.Context, token string, cause error) error {
	if a.GitHubMinter == nil {
		return cause
	}
	revokeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	err := a.GitHubMinter.Revoke(revokeCtx, token)
	cancel()
	if err != nil {
		a.trackGitHubRevocation(token, 0, "", a.githubTime().Add(time.Hour))
		return fmt.Errorf("%w; revoke discarded GitHub token: %v", cause, err)
	}
	return cause
}

func (a *App) beginGitHubApproval(ctx context.Context, session state.TerminalSession, request githubauth.BrokerRequest, app githubauth.App) (*githubPendingRequest, error) {
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
	text := a.githubApprovalText(session, request, app, grantExpiresAt)
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
		ExpiresAt:       now.Add(githubApprovalTTL),
		ApprovalText:    text,
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

	message, err := a.Telegram.SendMessage(ctx, a.Config.TelegramChatID, text, session.AnchorMessageID, telegram.GitHubApprovalMarkup(requestID))
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

func (a *App) githubApprovalText(session state.TerminalSession, request githubauth.BrokerRequest, app githubauth.App, grantExpiresAt time.Time) string {
	var text strings.Builder
	if request.Action == githubauth.ActionGrant {
		text.WriteString("Renewable GitHub work-session grant requested\n\n")
	} else {
		text.WriteString("GitHub capability requested\n\n")
	}
	fmt.Fprintf(&text, "Window: %s\n", sessionLabel(session))
	fmt.Fprintf(&text, "tmux binding: %s / %s / %s\n", request.Binding.ServerID, request.Binding.WindowID, request.Binding.PaneID)
	fmt.Fprintf(&text, "App: %s (App ID %d, installation %d, fingerprint %s)\n",
		request.App, app.AppID, app.InstallationID, app.PublicFingerprint)
	if len(request.Passphrase) == 0 && app.TelegramUnlock && !request.LocalUnlock {
		text.WriteString("Unlock: Telegram reply (not end-to-end encrypted)\n")
	} else {
		text.WriteString("Unlock: local passphrase\n")
	}
	text.WriteString("Repositories:\n")
	for _, repository := range request.Repositories {
		fmt.Fprintf(&text, "  %s\n", repository)
	}
	text.WriteString("Permissions:\n")
	names := make([]string, 0, len(request.Permissions))
	for name := range request.Permissions {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(&text, "  %s: %s\n", name, request.Permissions[name])
	}
	if request.Action == githubauth.ActionGrant {
		fmt.Fprintf(&text, "Duration: %s (until %s)\n", request.GrantFor, grantExpiresAt.Local().Format("2006-01-02 15:04 MST"))
		fmt.Fprintf(&text, "Purpose: %s\n", request.Purpose)
		text.WriteString("Scope: later commands from this exact pane may use any subset without another approval; each child receives a token at this displayed ceiling.\n")
		text.WriteString("Renewal: unattended short-lived token rotation is enabled.\n")
		text.WriteString("Memory: the unlocked signing capability remains only in Engram memory until this grant ends.\n")
	} else {
		fmt.Fprintf(&text, "Command: %s\n", a.redactText(compactGitHubCommand(request.Command)))
	}
	text.WriteString("Expires unanswered: 03:00")
	return text.String()
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
		pending.Result <- githubApproval{err: fmt.Errorf("GitHub capability request was denied")}
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
		pending.Result <- githubApproval{err: appErr}
		a.githubMu.Unlock()
		a.answerCallback(ctx, cb.ID, "app enrollment changed")
		return "callback_user_error"
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
		pending.Result <- githubApproval{err: fmt.Errorf("this GitHub App requires local passphrase entry")}
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
		a.resolveGitHubPending(pendingID, githubApproval{err: fmt.Errorf("send GitHub unlock prompt: %w", err)})
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
	a.githubMu.Unlock()
	if messageID == 0 {
		return
	}
	editCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if approvalText != "" {
		text = approvalText + "\n\n" + text
	}
	_, _ = a.Telegram.EditMessage(editCtx, a.Config.TelegramChatID, messageID, text, telegram.ClearMarkup())
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
	if !sameGitHubEnrollment(current, expected) {
		return githubauth.App{}, fmt.Errorf("GitHub App %q enrollment changed", expected.Alias)
	}
	return current, nil
}

func sameGitHubEnrollment(left, right githubauth.App) bool {
	return left.Alias == right.Alias &&
		left.AppID == right.AppID &&
		left.InstallationID == right.InstallationID &&
		left.TelegramUnlock == right.TelegramUnlock &&
		left.PublicFingerprint == right.PublicFingerprint &&
		left.CreatedAt.Equal(right.CreatedAt)
}

func (a *App) storeGitHubLease(lease githubLease) []string {
	key := githubBindingKey(lease.Binding)
	a.githubMu.Lock()
	defer a.githubMu.Unlock()
	var old []string
	if previous, ok := a.githubLeases[key]; ok && previous.Token != lease.Token {
		old = append(old, previous.Token)
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
		_ = a.audit("github.lease", "revoked", map[string]any{"session_id": sessionID, "app": lease.Info.App})
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
		_ = a.audit("github.grant", "revoked", map[string]any{"session_id": sessionID, "app": grant.Info.App, "grant_id": grant.Info.ID})
	}
	if leased && a.GitHubMinter != nil {
		revokeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := a.GitHubMinter.Revoke(revokeCtx, lease.Token)
		cancel()
		if err != nil {
			a.trackGitHubRevocation(lease.Token, sessionID, lease.Info.App, lease.Info.ExpiresAt)
			_ = a.audit("github.lease", "revoke_failed", map[string]any{"session_id": sessionID, "app": lease.Info.App, "error": err.Error()})
			return fmt.Errorf("GitHub authority was removed locally, but remote token revocation is pending: %w", err)
		}
	}
	if leased {
		_ = a.audit("github.lease", "revoked", map[string]any{"session_id": sessionID, "app": lease.Info.App})
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
	hasGrants := len(a.githubGrants) != 0
	a.githubMu.Unlock()
	enrollments := map[string]githubauth.App{}
	enrollmentsValid := !hasGrants || a.GitHubVault != nil
	if hasGrants && enrollmentsValid {
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
		reasonInvalid := !bound || active.ID != grant.SessionID || !active.CreatedAt.Equal(grant.SessionCreatedAt) ||
			!enrollmentsValid || !enrolled || !sameGitHubEnrollment(current, grant.Enrollment)
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
		if !lease.Info.ExpiresAt.After(now) {
			expired = append(expired, lease)
			delete(a.githubLeases, key)
		} else if _, active := activeBindings[key]; !active {
			invalidated = append(invalidated, lease)
			delete(a.githubLeases, key)
		}
	}
	a.githubMu.Unlock()
	for _, grant := range expiredGrants {
		a.queueManualRefresh(grant.SessionID)
		_ = a.audit("github.grant", "expired", map[string]any{"session_id": grant.SessionID, "app": grant.Info.App, "grant_id": grant.Info.ID})
	}
	for _, grant := range invalidatedGrants {
		a.queueManualRefresh(grant.SessionID)
		_ = a.audit("github.grant", "invalidated", map[string]any{"session_id": grant.SessionID, "app": grant.Info.App, "grant_id": grant.Info.ID})
	}
	for _, lease := range expired {
		a.queueManualRefresh(lease.SessionID)
		_ = a.audit("github.lease", "expired", map[string]any{"session_id": lease.SessionID, "app": lease.Info.App})
	}
	var tokens []string
	for _, lease := range invalidated {
		tokens = append(tokens, lease.Token)
		a.queueManualRefresh(lease.SessionID)
		_ = a.audit("github.lease", "invalidated", map[string]any{"session_id": lease.SessionID, "app": lease.Info.App})
	}
	a.revokeGitHubTokens(tokens)
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

func (a *App) revokeGitHubTokens(tokens []string) {
	if len(tokens) == 0 || a.GitHubMinter == nil {
		return
	}
	a.transferWG.Add(1)
	go func() {
		defer a.transferWG.Done()
		for _, token := range tokens {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			err := a.GitHubMinter.Revoke(ctx, token)
			cancel()
			if err != nil {
				a.trackGitHubRevocation(token, 0, "", a.githubTime().Add(time.Hour))
			}
		}
	}()
}

func (a *App) trackGitHubRevocation(token string, sessionID int, app string, expiresAt time.Time) {
	if token == "" {
		return
	}
	if !expiresAt.After(a.githubTime()) {
		expiresAt = a.githubTime().Add(time.Hour)
	}
	a.githubMu.Lock()
	if a.githubRevocations == nil {
		a.githubRevocations = map[string]githubRevocation{}
	}
	pending := a.githubRevocations[token]
	pending.Token = token
	pending.SessionID = sessionID
	pending.App = app
	pending.ExpiresAt = expiresAt
	pending.Attempts++
	pending.NextAttempt = a.githubTime().Add(min(time.Duration(pending.Attempts)*5*time.Second, time.Minute))
	a.githubRevocations[token] = pending
	a.githubMu.Unlock()
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
				_ = a.audit("github.lease", "revoke_recovered", map[string]any{"session_id": pending.SessionID, "app": pending.App})
				continue
			}
			a.trackGitHubRevocation(pending.Token, pending.SessionID, pending.App, pending.ExpiresAt)
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
	tokens := make([]string, 0, len(a.githubLeases))
	for key, lease := range a.githubLeases {
		tokens = append(tokens, lease.Token)
		delete(a.githubLeases, key)
	}
	for key, grant := range a.githubGrants {
		githubauth.Zero(grant.PrivateKey)
		delete(a.githubGrants, key)
	}
	for token := range a.githubRevocations {
		tokens = append(tokens, token)
		delete(a.githubRevocations, token)
	}
	a.githubMu.Unlock()
	a.revokeGitHubTokens(tokens)
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
		"session_id":   sessionID,
		"app":          request.App,
		"repositories": append([]string(nil), request.Repositories...),
		"permissions":  copyStringMap(request.Permissions),
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

func plural(count int, singular, plural string) string {
	if count == 1 {
		return singular
	}
	return plural
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
