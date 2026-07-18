package app

import (
	"context"
	"fmt"
	"os"
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
			a.markSessionLost(ctx, expected, err)
		}
		return err
	}
	metadata, metadataErr := a.Tmux.RecoveryMetadata(tmuxCtx, expected.TmuxPaneID, expected.TmuxWindowID, expected.TmuxServerID)
	if metadataErr != nil && tmux.IsIdentityLoss(metadataErr) {
		a.markSessionLost(ctx, expected, metadataErr)
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

func readHostBootID() string {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	id := strings.TrimSpace(string(data))
	if !recovery.ValidSessionID(id) {
		return ""
	}
	return strings.ToLower(id)
}

func (a *App) recoveryPlan() (string, []int) {
	sessions := a.Store.Snapshot().TerminalSessions
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].ID < sessions[j].ID })
	var recoverable, advisory []state.TerminalSession
	for _, session := range sessions {
		if session.State != state.TerminalLost {
			continue
		}
		if recovery.ValidProgram(session.ResumeProgram) && recovery.ValidSessionID(session.ResumeSessionID) {
			recoverable = append(recoverable, session)
		} else if latestObservedCommand(session) != "" {
			advisory = append(advisory, session)
		}
	}
	if len(recoverable) == 0 && len(advisory) == 0 {
		return "No lost sessions with recovery evidence were found.", nil
	}
	var text strings.Builder
	text.WriteString("Engram recovery plan\n\n")
	ids := make([]int, 0, len(recoverable))
	if len(recoverable) > 0 {
		text.WriteString("Exact provider sessions\n")
		for _, session := range recoverable {
			fmt.Fprintf(&text, "[%d] %s — %s\n", session.ID, recoveryTitle(session), session.ResumeProgram)
			ids = append(ids, session.ID)
		}
		text.WriteString("\nCopyable plan\n")
		for _, id := range ids {
			fmt.Fprintf(&text, "/resume %d\n", id)
		}
	}
	if len(advisory) > 0 {
		if len(recoverable) > 0 {
			text.WriteString("\n")
		}
		text.WriteString("Observed launches (not replayed)\n")
		for _, session := range advisory {
			fmt.Fprintf(&text, "[%d] %s — %s\n", session.ID, recoveryTitle(session), latestObservedCommand(session))
		}
	}
	return strings.TrimSpace(a.redactText(text.String())), ids
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
	text, ids := a.recoveryPlan()
	_, err := a.Telegram.SendMessage(ctx, chatID, text, replyTo, telegram.RecoveryPlanMarkup(ids))
	return err
}

func (a *App) deliverStartupRecoveryPlan(ctx context.Context) {
	newlyLost := false
	for _, session := range a.Store.Snapshot().TerminalSessions {
		if session.State != state.TerminalRunning {
			continue
		}
		_ = a.reconcileRecoverySession(ctx, session)
		if current, ok := a.Store.FindSession(session.ID); ok && current.State == state.TerminalLost {
			newlyLost = true
		}
	}
	if a.pendingRecoveryBootID == "" && !newlyLost {
		return
	}
	text, ids := a.recoveryPlan()
	if len(ids) > 0 || strings.Contains(text, "Observed launches") {
		if _, err := a.Telegram.SendMessage(ctx, a.Config.TelegramChatID, text, 0, telegram.RecoveryPlanMarkup(ids)); err != nil {
			_ = a.audit("telegram.recovery_plan", "failed", map[string]any{"error": err.Error()})
			return
		}
	}
	if a.pendingRecoveryBootID != "" {
		if err := a.Store.AcknowledgeRecoveryBoot(a.pendingRecoveryBootID); err != nil {
			_ = a.audit("state.recovery", "failed", map[string]any{"operation": "acknowledge_boot", "error": err.Error()})
			return
		}
		a.pendingRecoveryBootID = ""
	}
}
