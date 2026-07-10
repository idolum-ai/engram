package app

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/anthropic"
	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
)

func TestHandoffRequiresSettledGroundedObservations(t *testing.T) {
	now := time.Date(2026, 7, 10, 3, 0, 0, 0, time.UTC)
	ts := state.TerminalSession{ID: 1, State: state.TerminalRunning}
	report := groundedHandoffReport("approve_release")

	if got := observeHandoff(&ts, report, "capture-a", now); got != handoffUnchanged || ts.Handoff != nil || ts.HandoffCandidate == nil {
		t.Fatalf("first observation opened handoff: transition=%q session=%#v", got, ts)
	}
	if got := settleHandoffCandidate(&ts, "capture-a", now.Add(handoffSettlePeriod)); got != handoffOpened || ts.Handoff == nil {
		t.Fatalf("settled observation did not open handoff: transition=%q session=%#v", got, ts)
	}
	if ts.Handoff.ObservationHash != "capture-a" || ts.Handoff.Key != "approve_release" {
		t.Fatalf("opened handoff lost provenance: %#v", ts.Handoff)
	}
}

func TestHandoffAcknowledgesThenResolvesFromLaterEvidence(t *testing.T) {
	now := time.Date(2026, 7, 10, 3, 0, 0, 0, time.UTC)
	ts := openHandoffSession(now)
	if !acknowledgeHandoff(&ts, now.Add(time.Second)) || ts.Handoff == nil || ts.Handoff.AcknowledgedAt.IsZero() {
		t.Fatalf("acknowledgment erased or missed handoff: %#v", ts)
	}
	clear := anthropic.GuideReport{StatusReport: "Deployment is running.", RecommendedAction: "Wait for it to finish.", Confidence: "high"}
	if got := observeHandoff(&ts, clear, "progress-1", now.Add(2*time.Second)); got != handoffUnchanged || ts.HandoffCandidate == nil || ts.HandoffCandidate.Kind != "resolve" {
		t.Fatalf("first resolution observation = %q session=%#v", got, ts)
	}
	if got := observeHandoff(&ts, clear, "progress-2", now.Add(8*time.Second)); got != handoffResolved || ts.Handoff != nil {
		t.Fatalf("second resolution observation = %q session=%#v", got, ts)
	}
}

func TestHandoffReopensWhenInputHasNoVisibleEffect(t *testing.T) {
	now := time.Date(2026, 7, 10, 3, 0, 0, 0, time.UTC)
	ts := openHandoffSession(now)
	ts.LastRawCaptureHash = "prompt"
	acknowledgeHandoff(&ts, now.Add(time.Second))
	if got := settleHandoffCandidate(&ts, "prompt", now.Add(2*time.Second)); got != handoffUnchanged || ts.HandoffCandidate == nil || ts.HandoffCandidate.Kind != "reopen" {
		t.Fatalf("unchanged capture did not stage reopen: transition=%q session=%#v", got, ts)
	}
	if got := settleHandoffCandidate(&ts, "prompt", now.Add(8*time.Second)); got != handoffReopened || ts.Handoff == nil || !ts.Handoff.AcknowledgedAt.IsZero() {
		t.Fatalf("unchanged capture did not reopen: transition=%q session=%#v", got, ts)
	}
	if ts.Handoff.NotificationMessageID != 0 {
		t.Fatalf("reopened handoff inherited delivery identity: %#v", ts.Handoff)
	}
}

func TestLowConfidenceCannotResolveOpenHandoff(t *testing.T) {
	now := time.Date(2026, 7, 10, 3, 0, 0, 0, time.UTC)
	ts := openHandoffSession(now)
	ts.HandoffCandidate = &state.HandoffCandidate{Kind: "resolve", ObservationHash: "old"}
	report := anthropic.GuideReport{StatusReport: "Unclear.", RecommendedAction: "Inspect.", Confidence: "low"}
	if got := observeHandoff(&ts, report, "new", now.Add(time.Minute)); got != handoffUnchanged || ts.Handoff == nil || ts.HandoffCandidate != nil {
		t.Fatalf("low-confidence report changed handoff: transition=%q session=%#v", got, ts)
	}
}

func TestDifferentSettledNeedReplacesHandoff(t *testing.T) {
	now := time.Date(2026, 7, 10, 3, 0, 0, 0, time.UTC)
	ts := openHandoffSession(now)
	report := groundedHandoffReport("choose_region")
	if got := observeHandoff(&ts, report, "region-1", now.Add(time.Second)); got != handoffUnchanged {
		t.Fatalf("first replacement observation = %q", got)
	}
	if got := observeHandoff(&ts, report, "region-2", now.Add(7*time.Second)); got != handoffReplaced || ts.Handoff == nil || ts.Handoff.Key != "choose_region" {
		t.Fatalf("replacement = %q session=%#v", got, ts)
	}
}

func TestHandoffNotificationIsDeliveredOnceAndAcceptsReplies(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.AllocateSession("main", "@1", "%1", "release")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 10, 3, 0, 0, 0, time.UTC)
	if _, _, err := store.UpdateSession(session.ID, func(s *state.TerminalSession) {
		s.AnchorChatID = 100
		s.AnchorMessageID = 77
		s.Handoff = openHandoffSession(now).Handoff
	}); err != nil {
		t.Fatal(err)
	}
	requests := 0
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: appRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		var payload map[string]any
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload["reply_to_message_id"] != float64(77) || payload["reply_markup"] != nil {
			t.Fatalf("handoff payload = %#v", payload)
		}
		return telegramTestResponse(t, http.StatusOK, map[string]any{
			"ok":     true,
			"result": map[string]any{"message_id": 88, "chat": map[string]any{"id": 100}},
		}), nil
	})}
	app := &App{Config: config.Config{TelegramChatID: 100}, Store: store, Telegram: client}
	app.ensureHandoffDelivery(context.Background(), session.ID)
	app.ensureHandoffDelivery(context.Background(), session.ID)
	if requests != 1 {
		t.Fatalf("handoff requests = %d, want one", requests)
	}
	if routed, ok := store.FindByAnchor(100, 88); !ok || routed.ID != session.ID {
		t.Fatalf("handoff reply route = %#v ok=%v", routed, ok)
	}
}

func groundedHandoffReport(key string) anthropic.GuideReport {
	return anthropic.GuideReport{
		StatusReport:      "The release is waiting for approval.",
		RecommendedAction: "Approve or reject the release.",
		Citations:         []string{"Confirm release [y/N]:"},
		HumanNeeded:       true,
		HandoffKey:        key,
		Confidence:        "high",
	}
}

func openHandoffSession(now time.Time) state.TerminalSession {
	return state.TerminalSession{
		ID:                 1,
		State:              state.TerminalRunning,
		LastRawCaptureHash: "prompt",
		Handoff: &state.Handoff{
			Key:               "approve_release",
			StatusReport:      "The release is waiting for approval.",
			RecommendedAction: "Approve or reject the release.",
			Evidence:          []string{"Confirm release [y/N]:"},
			ObservationHash:   "prompt",
			OpenedAt:          now,
			LastConfirmedAt:   now,
		},
	}
}
