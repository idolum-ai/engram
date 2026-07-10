package app

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/idolum-ai/engram/internal/anthropic"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
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

func rotatesAnchor(transition handoffTransition) bool {
	return transition == handoffOpened || transition == handoffReplaced || transition == handoffReopened
}

func observeHandoff(ts *state.TerminalSession, report anthropic.GuideReport, observationHash string, now time.Time) handoffTransition {
	if strings.EqualFold(report.Confidence, "low") {
		// A low-confidence observation contributes no evidence either for or
		// against the candidate. Preserve prior settlement progress until a
		// confident incompatible observation supersedes it.
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
	// An unchanged later capture is the second observation even though it does
	// not require another Haiku request.
	candidate.Observations++
	candidate.LastObservedAt = now
	return promoteHandoffCandidate(ts, now)
}

func promoteHandoffCandidate(ts *state.TerminalSession, now time.Time) handoffTransition {
	candidate := ts.HandoffCandidate
	if candidate == nil || candidate.Observations < 2 {
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

func acknowledgeHandoff(ts *state.TerminalSession, now time.Time, preInputCaptureHash string) bool {
	if ts.Handoff == nil || !ts.Handoff.AcknowledgedAt.IsZero() {
		return false
	}
	ts.Handoff.AcknowledgedAt = now
	ts.Handoff.AcknowledgedCaptureHash = preInputCaptureHash
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

func (a *App) ensureHandoffAnchor(ctx context.Context, id int) {
	lock := a.anchorMutex(id)
	lock.Lock()
	defer lock.Unlock()

	a.finishAnchorRotationLocked(ctx, id)
	ts, ok := a.Store.FindSession(id)
	if !ok || ts.RetiringAnchorMessageID != 0 || ts.Handoff == nil || !ts.Handoff.AcknowledgedAt.IsZero() || ts.AnchorMessageID == 0 || ts.State != state.TerminalRunning || !ts.WatchEnabled {
		return
	}
	if ts.Handoff.AnchorMessageID == ts.AnchorMessageID {
		return
	}

	oldAnchorID := ts.AnchorMessageID
	newAnchorID := ts.Handoff.AnchorMessageID
	rendered := a.renderLocal(ts, ts.LastSummary)
	renderHash := sha(rendered)
	prospectiveExists := newAnchorID != 0
	if prospectiveExists {
		if _, err := a.editAnchor(ctx, ts.AnchorChatID, newAnchorID, rendered, anchorMarkup(ts)); err != nil {
			if isTelegramAnchorUnavailable(err) {
				if _, _, stateErr := a.Store.UpdateSession(id, func(session *state.TerminalSession) {
					if session.Handoff != nil && session.Handoff.OpenedAt.Equal(ts.Handoff.OpenedAt) && session.Handoff.AnchorMessageID == newAnchorID {
						session.Handoff.AnchorMessageID = 0
					}
				}); stateErr != nil {
					_ = a.audit("state.anchor_rotation", "failed", map[string]any{"session_id": id, "error": stateErr.Error()})
				}
			} else {
				_ = a.audit("telegram.anchor_rotation", "failed", map[string]any{"session_id": id, "stage": "activate_existing", "error": err.Error()})
			}
			return
		}
	} else {
		msg, err := a.sendAnchor(ctx, ts.AnchorChatID, rendered, oldAnchorID, anchorMarkup(ts))
		if err != nil {
			_ = a.audit("telegram.anchor_rotation", "failed", map[string]any{"session_id": id, "stage": "send", "error": err.Error()})
			return
		}
		newAnchorID = msg.MessageID
	}

	rotated := false
	if _, _, err := a.Store.UpdateSession(id, func(session *state.TerminalSession) {
		if session.State == state.TerminalRunning && session.WatchEnabled && session.AnchorMessageID == oldAnchorID && session.RetiringAnchorMessageID == 0 && session.Handoff != nil && session.Handoff.OpenedAt.Equal(ts.Handoff.OpenedAt) && session.Handoff.AcknowledgedAt.IsZero() {
			session.AnchorMessageID = newAnchorID
			session.Handoff.AnchorMessageID = newAnchorID
			session.RetiringAnchorMessageID = oldAnchorID
			session.AnchorPinned = false
			session.AnchorPinKnown = false
			session.LastRenderHash = renderHash
			session.LastAnchorEditAt = time.Now().UTC()
			rotated = true
		}
	}); err != nil || !rotated {
		status := "superseded"
		if err != nil {
			status = err.Error()
		}
		_ = a.audit("state.anchor_rotation", "failed", map[string]any{"session_id": id, "error": status})
		a.deactivateProspectiveAnchor(ctx, ts.AnchorChatID, newAnchorID, retiredAnchorText(ts))
		return
	}
	_ = a.audit("telegram.anchor_rotation", "activated", map[string]any{"session_id": id, "from_message_id": oldAnchorID, "to_message_id": newAnchorID, "reused": prospectiveExists})
	a.finishAnchorRotationLocked(ctx, id)
}

func (a *App) reconcileAnchorPresentation(ctx context.Context, id int) {
	lock := a.anchorMutex(id)
	lock.Lock()
	defer lock.Unlock()
	a.finishAnchorRotationLocked(ctx, id)
	ts, ok := a.Store.FindSession(id)
	if !ok || ts.AnchorMessageID == 0 {
		return
	}
	if anchorShouldBePinned(ts) {
		a.ensureCurrentAnchorPinnedLocked(ctx, ts)
	} else if !ts.AnchorPinKnown || ts.AnchorPinned {
		a.ensureCurrentAnchorUnpinnedLocked(ctx, ts)
	}
}

func (a *App) finishAnchorRotationLocked(ctx context.Context, id int) {
	ts, ok := a.Store.FindSession(id)
	if !ok || ts.AnchorMessageID == 0 {
		return
	}
	if ts.RetiringAnchorMessageID == 0 || ts.RetiringAnchorMessageID == ts.AnchorMessageID {
		return
	}
	if anchorShouldBePinned(ts) && !a.ensureCurrentAnchorPinnedLocked(ctx, ts) {
		return
	}
	oldID := ts.RetiringAnchorMessageID
	oldUnavailable := false
	if _, err := a.editAnchor(ctx, ts.AnchorChatID, oldID, retiredAnchorText(ts), telegram.ClearMarkup()); err != nil {
		if !isTelegramAnchorUnavailable(err) {
			_ = a.audit("telegram.anchor_retire", "failed", map[string]any{"session_id": id, "message_id": oldID, "stage": "compact", "error": err.Error()})
			return
		}
		oldUnavailable = true
	}
	if !oldUnavailable {
		if err := a.Telegram.UnpinChatMessage(ctx, ts.AnchorChatID, oldID); err != nil && !telegram.IsMessageNotPinned(err) {
			_ = a.audit("telegram.anchor_retire", "failed", map[string]any{"session_id": id, "message_id": oldID, "stage": "unpin", "error": err.Error()})
			return
		}
	}
	if _, _, err := a.Store.UpdateSession(id, func(session *state.TerminalSession) {
		if session.RetiringAnchorMessageID == oldID {
			session.RetiringAnchorMessageID = 0
		}
	}); err != nil {
		_ = a.audit("state.anchor_rotation", "failed", map[string]any{"session_id": id, "error": err.Error()})
		return
	}
	_ = a.audit("telegram.anchor_retire", "ok", map[string]any{"session_id": id, "message_id": oldID})
}

func (a *App) ensureCurrentAnchorPinnedLocked(ctx context.Context, ts state.TerminalSession) bool {
	if ts.AnchorPinKnown && ts.AnchorPinned {
		return true
	}
	if a.Telegram == nil {
		return false
	}
	if err := a.Telegram.PinChatMessage(ctx, ts.AnchorChatID, ts.AnchorMessageID); err != nil && !telegram.IsMessageAlreadyPinned(err) {
		_ = a.audit("telegram.anchor_pin", "failed", map[string]any{"session_id": ts.ID, "message_id": ts.AnchorMessageID, "error": err.Error()})
		return false
	}
	recorded := false
	if _, _, err := a.Store.UpdateSession(ts.ID, func(session *state.TerminalSession) {
		if session.AnchorMessageID == ts.AnchorMessageID && anchorShouldBePinned(*session) {
			session.AnchorPinned = true
			session.AnchorPinKnown = true
			recorded = true
		}
	}); err != nil {
		_ = a.audit("state.anchor_pin", "failed", map[string]any{"session_id": ts.ID, "error": err.Error()})
		return false
	}
	if !recorded {
		_ = a.Telegram.UnpinChatMessage(ctx, ts.AnchorChatID, ts.AnchorMessageID)
		return false
	}
	_ = a.audit("telegram.anchor_pin", "ok", map[string]any{"session_id": ts.ID, "message_id": ts.AnchorMessageID})
	return true
}

func (a *App) ensureCurrentAnchorUnpinnedLocked(ctx context.Context, ts state.TerminalSession) bool {
	if ts.AnchorPinKnown && !ts.AnchorPinned {
		return true
	}
	if a.Telegram == nil {
		return false
	}
	if err := a.Telegram.UnpinChatMessage(ctx, ts.AnchorChatID, ts.AnchorMessageID); err != nil && !telegram.IsMessageNotPinned(err) && !isTelegramAnchorUnavailable(err) {
		_ = a.audit("telegram.anchor_unpin", "failed", map[string]any{"session_id": ts.ID, "message_id": ts.AnchorMessageID, "error": err.Error()})
		return false
	}
	if _, _, err := a.Store.UpdateSession(ts.ID, func(session *state.TerminalSession) {
		if session.AnchorMessageID == ts.AnchorMessageID {
			session.AnchorPinned = false
			session.AnchorPinKnown = true
		}
	}); err != nil {
		_ = a.audit("state.anchor_pin", "failed", map[string]any{"session_id": ts.ID, "error": err.Error()})
		return false
	}
	_ = a.audit("telegram.anchor_unpin", "ok", map[string]any{"session_id": ts.ID, "message_id": ts.AnchorMessageID})
	return true
}

func (a *App) deactivateProspectiveAnchor(ctx context.Context, chatID int64, messageID int, text string) {
	_, _ = a.editAnchor(ctx, chatID, messageID, text, telegram.ClearMarkup())
	_ = a.Telegram.UnpinChatMessage(ctx, chatID, messageID)
}

func anchorShouldBePinned(ts state.TerminalSession) bool {
	return ts.State == state.TerminalRunning && ts.WatchEnabled && ts.AnchorMessageID != 0
}

func retiredAnchorText(ts state.TerminalSession) string {
	return fmt.Sprintf("[%d] %s\ncontinued in the newer live anchor", ts.ID, firstNonEmpty(ts.Title, "session"))
}

func (a *App) anchorMutex(id int) *sync.Mutex {
	lock, _ := a.anchorLocks.LoadOrStore(id, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func (a *App) auditHandoffTransition(id int, transition handoffTransition, observationHash string) {
	if transition == handoffUnchanged {
		return
	}
	_ = a.audit("handoff.lifecycle", string(transition), map[string]any{"session_id": id, "observation_hash": observationHash})
}
