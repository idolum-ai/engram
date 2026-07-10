package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/idolum-ai/engram/internal/anthropic"
	"github.com/idolum-ai/engram/internal/state"
)

const handoffSettlePeriod = 5 * time.Second

type handoffTransition string

const (
	handoffUnchanged handoffTransition = ""
	handoffOpened    handoffTransition = "opened"
	handoffReplaced  handoffTransition = "replaced"
	handoffResolved  handoffTransition = "resolved"
	handoffReopened  handoffTransition = "reopened"
)

func observeHandoff(ts *state.TerminalSession, report anthropic.GuideReport, observationHash string, now time.Time) handoffTransition {
	if strings.EqualFold(report.Confidence, "low") {
		ts.HandoffCandidate = nil
		return handoffUnchanged
	}
	needed := report.HumanNeeded && report.HandoffKey != "" && len(report.Citations) > 0
	sameHandoff := ts.Handoff != nil && handoffCompatible(
		ts.Handoff.Key,
		ts.Handoff.Evidence,
		ts.Handoff.RecommendedAction,
		report.HandoffKey,
		report.Citations,
		report.RecommendedAction,
	)
	kind := ""
	switch {
	case ts.Handoff == nil && needed:
		kind = "open"
	case ts.Handoff == nil:
		ts.HandoffCandidate = nil
		return handoffUnchanged
	case needed && !sameHandoff:
		kind = "replace"
	case needed && !ts.Handoff.AcknowledgedAt.IsZero():
		kind = "reopen"
	case needed:
		ts.Handoff.LastConfirmedAt = now
		ts.HandoffCandidate = nil
		return handoffUnchanged
	default:
		kind = "resolve"
	}

	candidate := ts.HandoffCandidate
	candidateCompatible := candidate != nil && candidate.Kind == kind && (kind == "resolve" || handoffCompatible(
		candidate.Key,
		candidate.Evidence,
		candidate.RecommendedAction,
		report.HandoffKey,
		report.Citations,
		report.RecommendedAction,
	))
	if !candidateCompatible {
		ts.HandoffCandidate = &state.HandoffCandidate{
			Kind:              kind,
			Key:               report.HandoffKey,
			StatusReport:      report.StatusReport,
			RecommendedAction: report.RecommendedAction,
			Evidence:          append([]string(nil), report.Citations...),
			ObservationHash:   observationHash,
			FirstObservedAt:   now,
			LastObservedAt:    now,
			Observations:      1,
		}
		return handoffUnchanged
	}
	if candidate.ObservationHash != observationHash {
		candidate.Observations++
	}
	candidate.ObservationHash = observationHash
	candidate.LastObservedAt = now
	candidate.StatusReport = report.StatusReport
	candidate.RecommendedAction = report.RecommendedAction
	candidate.Evidence = append(candidate.Evidence[:0], report.Citations...)
	if candidate.Observations < 2 || now.Sub(candidate.FirstObservedAt) < handoffSettlePeriod {
		return handoffUnchanged
	}
	return promoteHandoffCandidate(ts, now)
}

func handoffCompatible(keyA string, evidenceA []string, actionA, keyB string, evidenceB []string, actionB string) bool {
	keyMatch := keyA != "" && keyA == keyB
	evidenceMatch := textSetsOverlap(evidenceA, evidenceB)
	actionMatch := lineSupportsHandoffEvidence(actionA, []string{actionB})
	return keyMatch && (evidenceMatch || actionMatch) || evidenceMatch && actionMatch
}

func textSetsOverlap(a, b []string) bool {
	for _, left := range a {
		if lineSupportsHandoffEvidence(left, b) {
			return true
		}
	}
	return false
}

func settleHandoffCandidate(ts *state.TerminalSession, observationHash string, now time.Time) handoffTransition {
	candidate := ts.HandoffCandidate
	if candidate == nil && ts.Handoff != nil && !ts.Handoff.AcknowledgedAt.IsZero() && ts.Handoff.AcknowledgedCaptureHash == observationHash {
		ts.HandoffCandidate = &state.HandoffCandidate{
			Kind:              "reopen",
			Key:               ts.Handoff.Key,
			StatusReport:      "The terminal display did not visibly change after your input, so the earlier handoff still appears to be waiting.",
			RecommendedAction: ts.Handoff.RecommendedAction,
			Evidence:          append([]string(nil), ts.Handoff.Evidence...),
			ObservationHash:   observationHash,
			FirstObservedAt:   now,
			LastObservedAt:    now,
			Observations:      1,
		}
		return handoffUnchanged
	}
	if candidate == nil || candidate.ObservationHash != observationHash || now.Sub(candidate.FirstObservedAt) < handoffSettlePeriod {
		return handoffUnchanged
	}
	return promoteHandoffCandidate(ts, now)
}

func promoteHandoffCandidate(ts *state.TerminalSession, now time.Time) handoffTransition {
	candidate := ts.HandoffCandidate
	if candidate == nil {
		return handoffUnchanged
	}
	ts.HandoffCandidate = nil
	switch candidate.Kind {
	case "resolve":
		ts.Handoff = nil
		return handoffResolved
	case "open", "replace", "reopen":
		transition := handoffOpened
		if candidate.Kind == "replace" {
			transition = handoffReplaced
		} else if candidate.Kind == "reopen" {
			transition = handoffReopened
		}
		ts.Handoff = &state.Handoff{
			Key:               candidate.Key,
			StatusReport:      candidate.StatusReport,
			RecommendedAction: candidate.RecommendedAction,
			Evidence:          append([]string(nil), candidate.Evidence...),
			ObservationHash:   candidate.ObservationHash,
			OpenedAt:          now,
			LastConfirmedAt:   now,
		}
		return transition
	default:
		return handoffUnchanged
	}
}

func acknowledgeHandoff(ts *state.TerminalSession, now time.Time) bool {
	if ts.Handoff == nil || !ts.Handoff.AcknowledgedAt.IsZero() {
		return false
	}
	ts.Handoff.AcknowledgedAt = now
	ts.Handoff.AcknowledgedCaptureHash = ts.LastRawCaptureHash
	ts.HandoffCandidate = nil
	return true
}

func handoffSummary(handoff *state.Handoff) string {
	if handoff == nil {
		return ""
	}
	report := anthropic.GuideReport{
		StatusReport:      handoff.StatusReport,
		RecommendedAction: handoff.RecommendedAction,
		Citations:         handoff.Evidence,
	}
	return report.TelegramText()
}

func (a *App) ensureHandoffDelivery(ctx context.Context, id int) {
	ts, ok := a.Store.FindSession(id)
	if !ok || ts.Handoff == nil || ts.Handoff.NotificationMessageID != 0 || !ts.Handoff.AcknowledgedAt.IsZero() || ts.AnchorMessageID == 0 {
		return
	}
	text := fmt.Sprintf("[%d] %s needs you\n\n%s", ts.ID, firstNonEmpty(ts.Title, "session"), handoffSummary(ts.Handoff))
	msg, err := a.sendAnchor(ctx, ts.AnchorChatID, text, ts.AnchorMessageID, nil)
	if err != nil {
		_ = a.audit("telegram.handoff", "failed", map[string]any{"session_id": id, "error": err.Error()})
		return
	}
	openedAt := ts.Handoff.OpenedAt
	key := ts.Handoff.Key
	if _, _, err := a.Store.UpdateSession(id, func(session *state.TerminalSession) {
		if session.Handoff != nil && session.Handoff.Key == key && session.Handoff.OpenedAt.Equal(openedAt) && session.Handoff.NotificationMessageID == 0 {
			session.Handoff.NotificationMessageID = msg.MessageID
		}
	}); err != nil {
		_ = a.audit("state.handoff", "failed", map[string]any{"session_id": id, "error": err.Error()})
		return
	}
	_ = a.audit("telegram.handoff", "delivered", map[string]any{"session_id": id, "message_id": msg.MessageID, "key": key})
}

func (a *App) auditHandoffTransition(id int, transition handoffTransition, observationHash string) {
	if transition == handoffUnchanged {
		return
	}
	_ = a.audit("handoff.lifecycle", string(transition), map[string]any{"session_id": id, "observation_hash": observationHash})
}
