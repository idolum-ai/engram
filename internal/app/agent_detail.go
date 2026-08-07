package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/idolum-ai/engram/internal/agentcompat"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
)

func (a *App) showAgentDetail(ctx context.Context, expected state.TerminalSession) actionResult {
	lock := a.anchorMutex(expected.ID)
	lock.Lock()
	defer lock.Unlock()
	current, ok := a.Store.FindSession(expected.ID)
	if !ok || current.State != state.TerminalRunning || current.Collapsed || current.AnchorMessageID == 0 ||
		!sameTerminalBinding(current, expected) || current.AnchorMessageID != expected.AnchorMessageID {
		return actionResult{Outcome: actionUserError, Message: "agent detail is unavailable"}
	}
	text := renderAgentDetail(current)
	markup := telegram.AgentDetailMarkup(current.ID)
	hash := sha(text)
	if current.AgentDetailMessageID != 0 && current.AgentDetailAnchorMessageID != current.AnchorMessageID {
		a.retireProspectiveMessage(ctx, current.AgentDetailChatID, current.AgentDetailMessageID)
		updated, found, applied, stateErr := a.updateSessionIfCurrent(current, func(session *state.TerminalSession) {
			if session.AgentDetailMessageID == current.AgentDetailMessageID {
				clearAgentDetailState(session)
			}
		})
		if stateErr != nil || !found || !applied {
			return actionResult{Outcome: actionStateFailed, Message: "stale agent detail could not be retired"}
		}
		current = updated
	}
	if current.AgentDetailMessageID != 0 && current.AgentDetailAnchorMessageID == current.AnchorMessageID {
		_, err := a.Telegram.EditMessage(ctx, current.AgentDetailChatID, current.AgentDetailMessageID, text, markup)
		if err == nil || telegram.IsMessageNotModified(err) {
			committed := false
			_, found, applied, stateErr := a.updateSessionIfCurrent(current, func(session *state.TerminalSession) {
				if session.AgentDetailMessageID == current.AgentDetailMessageID && session.AnchorMessageID == current.AnchorMessageID && !session.Collapsed && session.State == state.TerminalRunning {
					session.AgentDetailRenderHash = hash
					committed = true
				}
			})
			if stateErr != nil || !found || !applied || !committed {
				a.retireProspectiveMessage(ctx, current.AgentDetailChatID, current.AgentDetailMessageID)
				_, _, _, _ = a.updateSessionIfCurrent(current, func(session *state.TerminalSession) {
					if session.AgentDetailMessageID == current.AgentDetailMessageID {
						clearAgentDetailState(session)
					}
				})
				return actionResult{Outcome: actionStateFailed, Message: "agent detail was superseded"}
			}
			return actionResult{Outcome: actionOK, Message: "agent detail refreshed"}
		}
		if !isTelegramAnchorUnavailable(err) {
			return actionResult{Outcome: actionTelegramFailed, Message: "could not refresh agent detail"}
		}
	}
	message, err := a.Telegram.SendMessage(ctx, current.AnchorChatID, text, current.AnchorMessageID, markup)
	if err != nil {
		return actionResult{Outcome: actionTelegramFailed, Message: "could not open agent detail"}
	}
	committed := false
	_, found, applied, stateErr := a.updateSessionIfCurrent(current, func(session *state.TerminalSession) {
		if session.AnchorMessageID != current.AnchorMessageID || session.Collapsed || session.State != state.TerminalRunning {
			return
		}
		session.AgentDetailChatID = message.Chat.ID
		session.AgentDetailMessageID = message.MessageID
		session.AgentDetailAnchorMessageID = current.AnchorMessageID
		session.AgentDetailRenderHash = hash
		committed = true
	})
	if stateErr != nil || !found || !applied || !committed {
		a.retireProspectiveMessage(ctx, message.Chat.ID, message.MessageID)
		return actionResult{Outcome: actionStateFailed, Message: "agent detail was superseded"}
	}
	_ = a.audit("telegram.agent_detail", "opened", map[string]any{"session_id": current.ID, "message_id": message.MessageID})
	return actionResult{Outcome: actionOK, Message: "agent detail opened"}
}

func (a *App) dismissAgentDetail(ctx context.Context, callback telegram.CallbackQuery, id int) actionResult {
	if callback.Message == nil {
		return actionResult{Outcome: actionUserError, Message: "agent detail is stale"}
	}
	lock := a.anchorMutex(id)
	lock.Lock()
	defer lock.Unlock()
	current, ok := a.Store.FindSession(id)
	if !ok || current.AgentDetailChatID != callback.Message.Chat.ID || current.AgentDetailMessageID != callback.Message.MessageID {
		return actionResult{Outcome: actionUserError, Message: "agent detail is stale"}
	}
	if err := a.Telegram.DeleteMessage(ctx, current.AgentDetailChatID, current.AgentDetailMessageID); err != nil && !isTelegramMessageGone(err) {
		return actionResult{Outcome: actionTelegramFailed, Message: "could not dismiss agent detail"}
	}
	_, _, _, err := a.updateSessionIfCurrent(current, func(session *state.TerminalSession) {
		if session.AgentDetailMessageID == current.AgentDetailMessageID {
			clearAgentDetailState(session)
		}
	})
	if err != nil {
		return actionResult{Outcome: actionStateFailed, Message: "detail dismissed but state could not be recorded"}
	}
	return actionResult{Outcome: actionOK, Message: "dismissed"}
}

func renderAgentDetail(session state.TerminalSession) string {
	compatibility := agentcompat.NormalizeCompatibility(session.AgentCompatibility)
	presentation := agentcompat.NormalizePresentation(session.AgentPresentation)
	if presentation.Model.Value == "" && session.DeclaredModel.Provenance == agentcompat.ProvenanceHook {
		presentation.Model = session.DeclaredModel
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Agent session [%d]\n", session.ID)
	fmt.Fprintf(&out, "Provider       %s\n", agentcompat.DisplayName(compatibility.Provider))
	if presentation.Model.Value != "" {
		fmt.Fprintf(&out, "Model          %s (%s)\n", presentationModelDisplay(compatibility.Provider, presentation.Model.Value), provenanceDisplay(presentation.Model.Provenance))
	}
	if presentation.Effort.Value != "" {
		fmt.Fprintf(&out, "Reasoning      %s\n", presentation.Effort.Value)
	}
	if presentation.Interaction.Value != "" {
		fmt.Fprintf(&out, "Interaction    %s\n", presentation.Interaction.Value)
	}
	if presentation.Activity != "" {
		fmt.Fprintf(&out, "Activity       %s\n", presentation.Activity)
	}
	if presentation.LastTurnSeconds > 0 {
		fmt.Fprintf(&out, "Last turn      %s\n", compactDuration(presentation.LastTurnSeconds))
	}
	if presentation.AgentTotal > 0 {
		fmt.Fprintf(&out, "Agents         %d total · %d active\n", presentation.AgentTotal, presentation.AgentActive)
	}
	out.WriteString("\nSources\n")
	writeDetailAxis(&out, "Process", compatibility.Process)
	writeDetailAxis(&out, "Session hook", compatibility.Binding)
	writeDetailAxis(&out, "Screen grammar", compatibility.Screen)
	writeDetailAxis(&out, "Transcript", compatibility.Transcript)
	return strings.TrimSpace(out.String())
}

func writeDetailAxis(out *strings.Builder, label string, axis agentcompat.Axis) {
	mark := "○"
	if axis.State == agentcompat.StateProven || axis.State == agentcompat.StateSupported || axis.State == agentcompat.StateEligible {
		mark = "✓"
	} else if axis.State == agentcompat.StateMissing || axis.State == agentcompat.StateStale || axis.State == agentcompat.StateUnsupported || axis.State == agentcompat.StateUnavailable {
		mark = "✗"
	}
	stateText := strings.ReplaceAll(string(axis.State), "_", " ")
	if stateText == "" {
		stateText = "not checked"
	}
	if axis.Reason != agentcompat.ReasonNone {
		stateText = strings.ReplaceAll(string(axis.Reason), "_", " ")
	}
	fmt.Fprintf(out, "%s %-14s %s\n", mark, label, stateText)
}

func provenanceDisplay(value agentcompat.Provenance) string {
	switch value {
	case agentcompat.ProvenanceHook:
		return "hook"
	case agentcompat.ProvenanceVisibleUI:
		return "visible UI"
	case agentcompat.ProvenanceRetainedUI:
		return "retained UI"
	default:
		return "unknown"
	}
}

func compactDuration(seconds int) string {
	duration := time.Duration(seconds) * time.Second
	if duration >= time.Hour {
		return fmt.Sprintf("%dh %02dm %02ds", int(duration/time.Hour), int(duration%time.Hour/time.Minute), int(duration%time.Minute/time.Second))
	}
	if duration >= time.Minute {
		return fmt.Sprintf("%dm %02ds", int(duration/time.Minute), int(duration%time.Minute/time.Second))
	}
	return fmt.Sprintf("%ds", seconds)
}

func clearAgentDetailState(session *state.TerminalSession) {
	session.AgentDetailChatID = 0
	session.AgentDetailMessageID = 0
	session.AgentDetailAnchorMessageID = 0
	session.AgentDetailRenderHash = ""
}

func (a *App) retireAgentDetail(ctx context.Context, expected state.TerminalSession) {
	if expected.AgentDetailChatID == 0 || expected.AgentDetailMessageID == 0 {
		return
	}
	cleared := false
	_, _, _, _ = a.updateSessionIfCurrent(expected, func(session *state.TerminalSession) {
		if session.AgentDetailMessageID == expected.AgentDetailMessageID {
			clearAgentDetailState(session)
			cleared = true
		}
	})
	if cleared {
		a.retireProspectiveMessage(ctx, expected.AgentDetailChatID, expected.AgentDetailMessageID)
	}
}
