package app

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/state"
)

func TestSwitchAnchorModePersistsOnlyAvailableMode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	app := &App{Store: store, mode: config.AnchorModeGuide, guideAvailable: true, snapshotReady: true}
	result := app.switchAnchorMode(context.Background(), "chromium")
	if !result.OK() || app.anchorMode() != config.AnchorModeSnapshot || store.Snapshot().AnchorMode != config.AnchorModeSnapshot {
		t.Fatalf("switch result=%#v mode=%q state=%q", result, app.anchorMode(), store.Snapshot().AnchorMode)
	}
	if result.Message != "switching to snapshot mode" {
		t.Fatalf("switch message = %q", result.Message)
	}

	app.snapshotReady = false
	result = app.switchAnchorMode(context.Background(), "snapshot")
	if result.OK() || !strings.Contains(result.Message, "unavailable") {
		t.Fatalf("unavailable switch = %#v", result)
	}
	if app.anchorMode() != config.AnchorModeSnapshot || store.Snapshot().AnchorMode != config.AnchorModeSnapshot {
		t.Fatal("failed switch changed persisted mode")
	}
}

func TestSwitchAnchorModeDefersCollapsedPresentationWork(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	if _, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.AnchorChatID = 100
		session.AnchorMessageID = 77
		session.AnchorFormat = anchorFormatText
		session.AnchorPinned = true
		session.AnchorPinKnown = true
		session.Collapsed = true
	}); err != nil {
		t.Fatal(err)
	}
	app.mode = config.AnchorModeGuide
	app.guideAvailable = true
	app.snapshotReady = true

	result := app.switchAnchorMode(context.Background(), config.AnchorModeSnapshot)
	if !result.OK() || app.anchorMode() != config.AnchorModeSnapshot {
		t.Fatalf("switch result=%#v mode=%q", result, app.anchorMode())
	}
	app.summaryMu.Lock()
	queued := app.summaryQueued[id] || app.summaryRunning[id]
	app.summaryMu.Unlock()
	if queued {
		t.Fatal("mode switch queued presentation work for a collapsed anchor")
	}
}

func TestModeTextDistinguishesConfiguredGuideFromReadyChromium(t *testing.T) {
	app := &App{
		Config:         config.Config{LLMProvider: config.LLMProviderAnthropic, AnthropicModel: config.DefaultAnthropicModel},
		mode:           config.AnchorModeGuide,
		guideAvailable: true,
		snapshotReady:  true,
	}
	got := app.modeText()
	for _, want := range []string{
		"guide (anthropic/claude-haiku-4-5-20251001 configured, not probed)",
		"snapshot (Chromium probed and ready)",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("mode text missing %q:\n%s", want, got)
		}
	}
}

func TestAnchorMarkupReflectsDeliverableAlternates(t *testing.T) {
	t.Parallel()
	ts := state.TerminalSession{ID: 7, State: state.TerminalRunning, AnchorFormat: "text"}
	app := &App{mode: config.AnchorModeGuide, snapshotReady: true}
	guide := app.anchorMarkup(ts)
	if got := guide.InlineKeyboard[0]; len(got) != 3 || got[1].CallbackData != "snapshot:7" || got[2].CallbackData != "collapse:7" {
		t.Fatalf("guide actions = %#v", got)
	}
	if len(guide.InlineKeyboard) != 2 {
		t.Fatalf("guide rows = %#v, want no arrow row", guide.InlineKeyboard)
	}
	app.setAnchorMode(config.AnchorModeSnapshot)
	app.guideAvailable = true
	ts.AnchorFormat = "snapshot"
	snapshot := app.anchorMarkup(ts)
	if got := snapshot.InlineKeyboard[0]; len(got) != 4 || got[1].CallbackData != "voice:7" || got[2].CallbackData != "raw:7" || got[3].CallbackData != "collapse:7" {
		t.Fatalf("snapshot actions = %#v", got)
	}
	if len(snapshot.InlineKeyboard) != 3 || snapshot.InlineKeyboard[2][0].CallbackData != "key:7:left" {
		t.Fatalf("snapshot rows = %#v, want directional row", snapshot.InlineKeyboard)
	}
	app.guideAvailable = false
	withoutGuide := app.anchorMarkup(ts)
	if got := withoutGuide.InlineKeyboard[0]; len(got) != 3 || got[1].CallbackData != "raw:7" || got[2].CallbackData != "collapse:7" {
		t.Fatalf("unavailable alternate leaked into markup: %#v", got)
	}
	if len(withoutGuide.InlineKeyboard) != 3 {
		t.Fatalf("snapshot arrows depend on guide availability: %#v", withoutGuide.InlineKeyboard)
	}
	ts.Collapsed = true
	collapsed := app.anchorMarkup(ts)
	if len(collapsed.InlineKeyboard) != 1 || len(collapsed.InlineKeyboard[0]) != 1 || collapsed.InlineKeyboard[0][0].CallbackData != "expand:7" {
		t.Fatalf("collapsed controls = %#v", collapsed)
	}
}
