package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/idolum-ai/engram/internal/recovery"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
	"github.com/idolum-ai/engram/internal/tmux"
)

const maxRecoveryCommandBytes = 512
const recoveryProcessObservationWindow = 2 * time.Minute
const startupRecoveryTimeout = 30 * time.Second
const startupRecoveryRetryDelay = 10 * time.Second
const maxRecoveryPlanEntries = 5

func shellForeground(command string) bool {
	command = strings.ToLower(strings.TrimSpace(command))
	if slash := strings.LastIndexByte(command, '/'); slash >= 0 {
		command = command[slash+1:]
	}
	switch command {
	case "bash", "zsh", "fish", "sh", "dash", "ksh", "nu", "pwsh", "powershell":
		return true
	default:
		return false
	}
}

func commandProgram(command string) string {
	program := commandExecutable(command)
	if recovery.ValidProgram(program) {
		return program
	}
	return ""
}

func commandExecutable(command string) string {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return ""
	}
	program := strings.ToLower(fields[0])
	if slash := strings.LastIndexByte(program, '/'); slash >= 0 {
		program = program[slash+1:]
	}
	return program
}

// recordSentRecoveryCommand records only text that Engram submitted while
// tmux proved the pane was at a shell prompt. Agent conversations are therefore
// excluded, and the command remains advisory until a process transition or a
// provider lifecycle hook corroborates it.
func (a *App) recordSentRecoveryCommand(expected state.TerminalSession, pane tmux.Pane, command string) {
	if !shellForeground(pane.CurrentCmd) {
		return
	}
	preview := strings.TrimSpace(a.redactText(command))
	if len(preview) > maxRecoveryCommandBytes {
		preview = headUTF8(preview, maxRecoveryCommandBytes)
	}
	_, _, _, err := a.updateSessionIfCurrent(expected, func(current *state.TerminalSession) {
		current.RecordRecoveryEvent(state.RecoveryEvent{
			At:               time.Now().UTC(),
			Kind:             "command",
			Command:          preview,
			CommandHash:      sha(command),
			CWD:              pane.CurrentPath,
			ForegroundBefore: pane.CurrentCmd,
			ExpectedProcess:  commandExecutable(command),
			Validation:       "sent_to_shell",
			Program:          commandProgram(command),
		})
	})
	if err != nil {
		_ = a.audit("state.recovery", "failed", map[string]any{"session_id": expected.ID, "operation": "record_command", "error": err.Error()})
	}
}

func (a *App) reconcileRecoverySession(ctx context.Context, expected state.TerminalSession) error {
	if expected.State != state.TerminalRunning {
		return nil
	}
	tmuxCtx, cancel := tmux.TimeoutContext(ctx)
	defer cancel()
	pane, err := a.terminalMechanics().Validate(tmuxCtx, terminalBinding(expected))
	if err != nil {
		if tmux.IsIdentityLoss(err) {
			if markErr := a.markSessionLost(ctx, expected, err); markErr != nil {
				return fmt.Errorf("persist lost session: %w", markErr)
			}
		}
		return err
	}
	metadata, metadataErr := a.Tmux.RecoveryMetadata(tmuxCtx, expected.TmuxPaneID, expected.TmuxWindowID, expected.TmuxServerID)
	if metadataErr != nil && tmux.IsIdentityLoss(metadataErr) {
		if markErr := a.markSessionLost(ctx, expected, metadataErr); markErr != nil {
			return fmt.Errorf("persist lost session: %w", markErr)
		}
		return metadataErr
	}
	processChange := recoveryProcessChange(expected.RecoveryEvents, pane, time.Now())
	metadataChange := recoveryMetadataChange(expected, metadata, metadataErr)
	if !processChange && !metadataChange {
		if metadataErr != nil {
			_ = a.audit("tmux.recovery_metadata", "rejected", map[string]any{"session_id": expected.ID, "error": metadataErr.Error()})
		}
		return metadataErr
	}
	_, found, applied, updateErr := a.updateSessionIfCurrent(expected, func(current *state.TerminalSession) {
		for index := len(current.RecoveryEvents) - 1; index >= 0; index-- {
			event := &current.RecoveryEvents[index]
			if event.Kind != "command" || event.Validation != "sent_to_shell" {
				continue
			}
			age := time.Since(event.At)
			observedProcess := commandExecutable(pane.CurrentCmd)
			if age >= 0 && age <= recoveryProcessObservationWindow && event.ExpectedProcess != "" && observedProcess == event.ExpectedProcess {
				event.ForegroundAfter = pane.CurrentCmd
				event.Validation = "process_observed"
			}
			break
		}
		if metadataErr != nil || metadata.SessionID == "" {
			return
		}
		current.ResumeProgram = metadata.Program
		current.ResumeSessionID = metadata.SessionID
		if metadata.CWD != "" {
			current.LastKnownCWD = metadata.CWD
		}
		for index := len(current.RecoveryEvents) - 1; index >= 0; index-- {
			event := current.RecoveryEvents[index]
			if event.Kind == "provider_session" && event.Program == metadata.Program && event.ProviderSessionID == metadata.SessionID {
				return
			}
		}
		current.RecordRecoveryEvent(state.RecoveryEvent{
			At: metadata.Observed, Kind: "provider_session", CWD: metadata.CWD,
			ForegroundAfter: pane.CurrentCmd, Validation: "provider_hook",
			Program: metadata.Program, ProviderSessionID: metadata.SessionID,
		})
	})
	if updateErr != nil {
		return updateErr
	}
	if !found || !applied {
		return fmt.Errorf("session changed while reconciling recovery metadata")
	}
	if metadataErr != nil {
		_ = a.audit("tmux.recovery_metadata", "rejected", map[string]any{"session_id": expected.ID, "error": metadataErr.Error()})
	}
	return metadataErr
}

func recoveryProcessChange(events []state.RecoveryEvent, pane tmux.Pane, now time.Time) bool {
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.Kind != "command" || event.Validation != "sent_to_shell" {
			continue
		}
		age := now.Sub(event.At)
		return age >= 0 && age <= recoveryProcessObservationWindow && event.ExpectedProcess != "" && commandExecutable(pane.CurrentCmd) == event.ExpectedProcess
	}
	return false
}

func recoveryMetadataChange(session state.TerminalSession, metadata recovery.Metadata, metadataErr error) bool {
	if metadataErr != nil || metadata.SessionID == "" {
		return false
	}
	if session.ResumeProgram != metadata.Program || session.ResumeSessionID != metadata.SessionID || metadata.CWD != "" && session.LastKnownCWD != metadata.CWD {
		return true
	}
	for index := len(session.RecoveryEvents) - 1; index >= 0; index-- {
		event := session.RecoveryEvents[index]
		if event.Kind == "provider_session" && event.Program == metadata.Program && event.ProviderSessionID == metadata.SessionID {
			return false
		}
	}
	return true
}

type recoveryPlanEntry struct {
	session state.TerminalSession
	exact   bool
}

type recoveryPlanPage struct {
	text    string
	actions []telegram.SessionAction
}

func (a *App) recoveryPlanPages() []recoveryPlanPage {
	sessions := a.Store.Snapshot().TerminalSessions
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].ID < sessions[j].ID })
	var exact, advisory []recoveryPlanEntry
	for _, session := range sessions {
		if session.State != state.TerminalLost || session.PendingResume != nil {
			continue
		}
		if recovery.ValidProgram(session.ResumeProgram) && recovery.ValidSessionID(session.ResumeSessionID) {
			exact = append(exact, recoveryPlanEntry{session: session, exact: true})
		} else if latestObservedCommand(session) != "" {
			advisory = append(advisory, recoveryPlanEntry{session: session})
		}
	}
	entries := append(exact, advisory...)
	if len(entries) == 0 {
		return []recoveryPlanPage{{text: "No lost sessions with recovery evidence were found."}}
	}
	pageCount := (len(entries) + maxRecoveryPlanEntries - 1) / maxRecoveryPlanEntries
	pages := make([]recoveryPlanPage, 0, pageCount)
	for start := 0; start < len(entries); start += maxRecoveryPlanEntries {
		end := min(start+maxRecoveryPlanEntries, len(entries))
		pages = append(pages, a.renderRecoveryPlanPage(entries[start:end], len(pages)+1, pageCount))
	}
	return pages
}

func (a *App) renderRecoveryPlanPage(entries []recoveryPlanEntry, page, pageCount int) recoveryPlanPage {
	var text strings.Builder
	text.WriteString("Engram recovery plan")
	if pageCount > 1 {
		fmt.Fprintf(&text, " %d/%d", page, pageCount)
	}
	text.WriteString("\n\n")
	actions := make([]telegram.SessionAction, 0, len(entries))
	if entries[0].exact {
		text.WriteString("Exact provider sessions\n")
		for _, entry := range entries {
			if !entry.exact {
				break
			}
			session := entry.session
			fmt.Fprintf(&text, "[%d] %s — %s\n", session.ID, recoveryTitle(session), session.ResumeProgram)
			actions = append(actions, telegram.SessionAction{ID: session.ID, Token: sessionActionToken(session)})
		}
		text.WriteString("\nCopyable plan\n")
		for _, action := range actions {
			fmt.Fprintf(&text, "/resume %d\n", action.ID)
		}
	}
	firstAdvisory := 0
	for firstAdvisory < len(entries) && entries[firstAdvisory].exact {
		firstAdvisory++
	}
	if firstAdvisory < len(entries) {
		if firstAdvisory > 0 {
			text.WriteString("\n")
		}
		text.WriteString("Observed launches (not replayed)\n")
		for _, entry := range entries[firstAdvisory:] {
			session := entry.session
			fmt.Fprintf(&text, "[%d] %s — %s\n", session.ID, recoveryTitle(session), latestObservedCommand(session))
		}
	}
	return recoveryPlanPage{text: strings.TrimSpace(a.redactText(text.String())), actions: actions}
}

func recoveryTitle(session state.TerminalSession) string {
	title := strings.TrimSpace(session.Title)
	if title == "" {
		return "untitled"
	}
	return headUTF8(strings.ReplaceAll(title, "\n", " "), 80)
}

func latestObservedCommand(session state.TerminalSession) string {
	for index := len(session.RecoveryEvents) - 1; index >= 0; index-- {
		event := session.RecoveryEvents[index]
		if event.Kind == "command" && event.Validation == "process_observed" && event.Command != "" {
			return event.Command
		}
	}
	return ""
}

func (a *App) sendRecoveryPlan(ctx context.Context, chatID int64, replyTo int) error {
	a.recoveryPlanMu.Lock()
	defer a.recoveryPlanMu.Unlock()
	pages := a.recoveryPlanPages()
	return a.publishRecoveryPlanPages(ctx, chatID, pages, replyTo, recoveryPlanHash(pages))
}

func (a *App) publishRecoveryPlanPages(ctx context.Context, chatID int64, pages []recoveryPlanPage, replyTo int, planHash string) error {
	snapshot := a.Store.Snapshot()
	knownMessageIDs := uniqueMessageIDs(append(append([]int(nil), snapshot.RecoveryPlanMessageIDs...), a.pendingRecoveryPlanMessageIDs...))
	if a.pendingRecoveryPlanHash != planHash {
		if len(knownMessageIDs) > 0 && !a.retireRecoveryPlanMessages(ctx, knownMessageIDs) {
			return fmt.Errorf("could not retire the previous recovery plan")
		}
		a.pendingRecoveryPlanMessageIDs = nil
		a.pendingRecoveryPlanHash = planHash
		a.pendingRecoveryPlanNextPage = 0
	} else if err := a.Store.SetRecoveryPlanProgress(planHash, a.pendingRecoveryPlanNextPage, a.pendingRecoveryPlanMessageIDs); err != nil {
		return err
	}
	for index := a.pendingRecoveryPlanNextPage; index < len(pages); index++ {
		page := pages[index]
		pageReplyTo := 0
		if index == 0 {
			pageReplyTo = replyTo
		}
		message, err := a.Telegram.SendMessage(ctx, chatID, page.text, pageReplyTo, telegram.RecoveryPlanMarkup(page.actions))
		if err != nil {
			return err
		}
		a.pendingRecoveryPlanMessageIDs = append(a.pendingRecoveryPlanMessageIDs, message.MessageID)
		a.pendingRecoveryPlanNextPage = index + 1
		if err := a.Store.SetRecoveryPlanProgress(planHash, a.pendingRecoveryPlanNextPage, a.pendingRecoveryPlanMessageIDs); err != nil {
			return err
		}
	}
	a.deliveredRecoveryPlanHash = planHash
	if err := a.Store.CompleteRecoveryPlan(planHash, a.pendingRecoveryPlanMessageIDs); err != nil {
		return err
	}
	a.pendingRecoveryPlanHash = ""
	a.pendingRecoveryPlanNextPage = 0
	return nil
}

func (a *App) deliverStartupRecoveryPlan(ctx context.Context) {
	for {
		if a.attemptStartupRecoveryPlan(ctx) {
			return
		}
		timer := time.NewTimer(startupRecoveryRetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (a *App) attemptStartupRecoveryPlan(ctx context.Context) bool {
	reconcileCtx, cancelReconcile := context.WithTimeout(ctx, startupRecoveryTimeout)
	defer cancelReconcile()
	complete := true
	for _, session := range a.Store.Snapshot().TerminalSessions {
		if reconcileCtx.Err() != nil {
			complete = false
			break
		}
		if session.PendingResume != nil {
			if err := a.reconcilePendingResume(reconcileCtx, session); err != nil {
				complete = false
				_ = a.audit("state.recovery", "failed", map[string]any{"session_id": session.ID, "operation": "reconcile_pending_resume", "error": err.Error()})
			}
			continue
		}
		if session.State == state.TerminalRunning {
			if err := a.reconcileRecoverySession(reconcileCtx, session); err != nil && !tmux.IsIdentityLoss(err) {
				complete = false
			}
		}
	}
	if !complete {
		return false
	}
	pages := a.recoveryPlanPages()
	hasPlan := len(pages) > 0 && (len(pages[0].actions) > 0 || strings.Contains(pages[0].text, "Observed launches"))
	planHash := ""
	if hasPlan {
		planHash = recoveryPlanHash(pages)
	}
	a.recoveryPlanMu.Lock()
	defer a.recoveryPlanMu.Unlock()
	snapshot := a.Store.Snapshot()
	publishPlan := hasPlan && (a.pendingRecoveryBootID != "" || snapshot.LastRecoveryPlanHash != planHash)
	if a.deliveredRecoveryPlanHash == planHash {
		publishPlan = false
	}
	if publishPlan {
		deliveryCtx, cancelDelivery := context.WithTimeout(ctx, startupRecoveryTimeout)
		defer cancelDelivery()
		if err := a.publishRecoveryPlanPages(deliveryCtx, a.Config.TelegramChatID, pages, 0, planHash); err != nil {
			_ = a.audit("telegram.recovery_plan", "failed", map[string]any{"error": err.Error()})
			return false
		}
		snapshot = a.Store.Snapshot()
	} else if !hasPlan {
		knownMessageIDs := uniqueMessageIDs(append(append([]int(nil), snapshot.RecoveryPlanMessageIDs...), a.pendingRecoveryPlanMessageIDs...))
		if len(knownMessageIDs) > 0 && !a.retireRecoveryPlanMessages(ctx, knownMessageIDs) {
			return false
		}
		a.pendingRecoveryPlanMessageIDs = nil
	}
	if snapshot.LastRecoveryPlanHash != planHash {
		if err := a.Store.SetRecoveryPlanHash(planHash); err != nil {
			_ = a.audit("state.recovery", "failed", map[string]any{"operation": "record_plan", "error": err.Error()})
			return false
		}
	}
	if a.pendingRecoveryBootID != "" {
		if err := a.Store.AcknowledgeRecoveryBoot(a.pendingRecoveryBootID); err != nil {
			_ = a.audit("state.recovery", "failed", map[string]any{"operation": "acknowledge_boot", "error": err.Error()})
			return false
		}
		a.pendingRecoveryBootID = ""
	}
	return true
}

func uniqueMessageIDs(messageIDs []int) []int {
	seen := make(map[int]bool, len(messageIDs))
	unique := make([]int, 0, len(messageIDs))
	for _, messageID := range messageIDs {
		if messageID <= 0 || seen[messageID] {
			continue
		}
		seen[messageID] = true
		unique = append(unique, messageID)
	}
	return unique
}

func (a *App) retireRecoveryPlanMessages(ctx context.Context, messageIDs []int) bool {
	cleanupCtx, cancel := context.WithTimeout(ctx, startupRecoveryTimeout)
	defer cancel()
	failed := make([]int, 0, len(messageIDs))
	for _, messageID := range messageIDs {
		if _, err := a.Telegram.EditReplyMarkup(cleanupCtx, a.Config.TelegramChatID, messageID, telegram.ClearMarkup()); err != nil && !telegram.IsMessageNotModified(err) {
			failed = append(failed, messageID)
		}
	}
	if err := a.Store.SetRecoveryPlanProgress("", 0, failed); err != nil {
		_ = a.audit("state.recovery", "failed", map[string]any{"operation": "retire_plan_messages", "error": err.Error()})
		return false
	}
	return len(failed) == 0
}

func (a *App) reconcilePendingResume(ctx context.Context, expected state.TerminalSession) error {
	if expected.PendingResume == nil {
		return nil
	}
	binding := terminalBinding(expected)
	pane, err := a.waitForResumeProcess(ctx, binding, expected.ResumeProgram)
	if err == nil {
		now := time.Now().UTC()
		_, found, applied, updateErr := a.updateSessionIfCurrent(expected, func(session *state.TerminalSession) {
			session.State = state.TerminalRunning
			session.WatchEnabled = true
			session.PendingResume = nil
			session.LastKnownCWD = firstNonEmptyExact(pane.CurrentPath, session.LastKnownCWD)
			session.LastActivityAt = now
			session.RecordRecoveryEvent(state.RecoveryEvent{
				At: now, Kind: "resume", Command: resumeCommand(session.ResumeProgram, session.ResumeSessionID),
				CWD: session.LastKnownCWD, Validation: "process_observed_after_restart",
				Program: session.ResumeProgram, ProviderSessionID: session.ResumeSessionID,
			})
		})
		if updateErr != nil {
			return updateErr
		}
		if !found || !applied {
			return fmt.Errorf("pending resume changed during reconciliation")
		}
		return nil
	}
	if tmux.IsIdentityLoss(err) {
		return a.restorePreparedResume(expected)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !errors.Is(err, errResumeProcessNotObserved) {
		return err
	}
	if cleanupErr := a.closeResumeWindow(binding); cleanupErr != nil {
		return fmt.Errorf("resume process unavailable (%v); cleanup failed: %w", err, cleanupErr)
	}
	return a.restorePreparedResume(expected)
}

func recoveryPlanHash(pages []recoveryPlanPage) string {
	var value strings.Builder
	for _, page := range pages {
		value.WriteString(page.text)
		for _, action := range page.actions {
			fmt.Fprintf(&value, "\x00%d\x00%s", action.ID, action.Token)
		}
	}
	return sha(value.String())
}
