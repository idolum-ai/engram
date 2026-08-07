package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/agentcompat"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
)

func TestRenderAgentDetailIsBoundedAndContainsNoSensitiveLocatorsOrNames(t *testing.T) {
	session := state.TerminalSession{
		ID: 7, Title: "private-project", LastKnownCWD: "/Users/example/private-project",
		ResumeSessionID: "123e4567-e89b-12d3-a456-426614174001",
		AgentCompatibility: agentcompat.Compatibility{
			Provider:   agentcompat.ProviderClaude,
			Process:    agentcompat.Axis{State: agentcompat.StateProven, Contract: agentcompat.ClaudeProcessContract, Version: "2.1.224"},
			Binding:    agentcompat.Axis{State: agentcompat.StateMissing, Contract: agentcompat.ClaudeBindingContract, Reason: agentcompat.ReasonBindingMissing},
			Screen:     agentcompat.Axis{State: agentcompat.StateSupported, Contract: agentcompat.ClaudeScreenContract, Version: "2.1.224"},
			Transcript: agentcompat.Axis{State: agentcompat.StateDisabled, Contract: agentcompat.ClaudeTranscriptContract, Reason: agentcompat.ReasonContextDisabled},
		},
		AgentPresentation: agentcompat.Presentation{
			Model:       agentcompat.Value{Value: "claude-opus-4-8", Provenance: agentcompat.ProvenanceHook},
			Interaction: agentcompat.Value{Value: "manual", Provenance: agentcompat.ProvenanceVisibleUI},
			Activity:    "idle", LastTurnSeconds: 14*60 + 46, AgentTotal: 3, AgentActive: 1,
		},
	}
	got := renderAgentDetail(session)
	for _, want := range []string{"Agent session [7]", "Claude Code", "Opus 4.8 (hook)", "manual", "14m 46s", "3 total · 1 active", "process", "binding missing", "context disabled"} {
		if !strings.Contains(strings.ToLower(got), strings.ToLower(want)) {
			t.Errorf("detail missing %q:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{"private-project", "/Users/", session.ResumeSessionID, "cwd"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("detail disclosed %q:\n%s", forbidden, got)
		}
	}
}

func TestHookDeclaredModelRemainsAvailableWithoutSupportedScreenMetadata(t *testing.T) {
	session := state.TerminalSession{
		State:              state.TerminalRunning,
		AgentCompatibility: agentcompat.Compatibility{Provider: agentcompat.ProviderClaude, Process: agentcompat.Axis{State: agentcompat.StateProven}, Screen: agentcompat.Axis{State: agentcompat.StateLiteral}},
		DeclaredModel:      agentcompat.Value{Value: "claude-opus-4-8", Provenance: agentcompat.ProvenanceHook},
	}
	if got := terminalPresentationText(session); got != "Claude Code · Opus 4.8" {
		t.Fatalf("compact hook presentation = %q", got)
	}
	if got := renderAgentDetail(session); !strings.Contains(got, "Opus 4.8 (hook)") {
		t.Fatalf("detail omitted hook model: %s", got)
	}
}

func TestAgentDetailCreatesRefreshesDeduplicatesAndDismissesOneMessage(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	if _, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.AnchorChatID = 100
		session.AnchorMessageID = 77
		session.AgentCompatibility = agentcompat.Compatibility{Provider: agentcompat.ProviderCodex, Process: agentcompat.Axis{State: agentcompat.StateProven}}
		session.AgentPresentation = agentcompat.Presentation{Model: agentcompat.Value{Value: "gpt-5.6-sol", Provenance: agentcompat.ProvenanceVisibleUI}, Activity: "active", ObservedAt: time.Now()}
	}); err != nil {
		t.Fatal(err)
	}
	var paths []string
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: anchorDeliveryRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		paths = append(paths, request.URL.Path)
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if len(body) != 0 {
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatal(err)
			}
			text, _ := payload["text"].(string)
			if strings.Contains(text, "/Users/") || strings.Contains(text, "123e4567") {
				t.Fatalf("detail request leaked sensitive content: %s", text)
			}
		}
		switch request.URL.Path {
		case "/botTOKEN/sendMessage", "/botTOKEN/editMessageText":
			return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": map[string]any{"message_id": 88, "chat": map[string]any{"id": 100}}}), nil
		case "/botTOKEN/deleteMessage":
			return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": true}), nil
		default:
			t.Fatalf("unexpected Telegram path %s", request.URL.Path)
			return nil, nil
		}
	})}
	app.Telegram = client

	session, _ := app.Store.FindSession(id)
	if result := app.showAgentDetail(context.Background(), session); !result.OK() {
		t.Fatalf("open result = %#v", result)
	}
	current, _ := app.Store.FindSession(id)
	if current.AgentDetailMessageID != 88 {
		t.Fatalf("detail identity = %#v", current)
	}
	if result := app.showAgentDetail(context.Background(), current); !result.OK() {
		t.Fatalf("refresh result = %#v", result)
	}
	callback := telegram.CallbackQuery{Message: &telegram.Message{MessageID: 88, Chat: telegram.Chat{ID: 100}}}
	if result := app.dismissAgentDetail(context.Background(), callback, id); !result.OK() {
		t.Fatalf("dismiss result = %#v", result)
	}
	current, _ = app.Store.FindSession(id)
	if current.AgentDetailMessageID != 0 || strings.Join(paths, ",") != "/botTOKEN/sendMessage,/botTOKEN/editMessageText,/botTOKEN/deleteMessage" {
		t.Fatalf("detail lifecycle paths=%#v state=%#v", paths, current)
	}
}

func TestAgentDetailRefreshFailsClosedWhenAnchorChangesInFlight(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	if _, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.AnchorChatID, session.AnchorMessageID = 100, 77
		session.AgentDetailChatID, session.AgentDetailMessageID, session.AgentDetailAnchorMessageID = 100, 88, 77
		session.AgentCompatibility = agentcompat.Compatibility{Provider: agentcompat.ProviderClaude}
	}); err != nil {
		t.Fatal(err)
	}
	var paths []string
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: anchorDeliveryRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		paths = append(paths, request.URL.Path)
		if request.URL.Path == "/botTOKEN/editMessageText" {
			if _, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) { session.AnchorMessageID = 99 }); err != nil {
				t.Fatal(err)
			}
			return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": map[string]any{"message_id": 88, "chat": map[string]any{"id": 100}}}), nil
		}
		if request.URL.Path == "/botTOKEN/deleteMessage" {
			return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": true}), nil
		}
		t.Fatalf("unexpected path %s", request.URL.Path)
		return nil, nil
	})}
	app.Telegram = client
	session, _ := app.Store.FindSession(id)
	result := app.showAgentDetail(context.Background(), session)
	if result.Outcome != actionStateFailed || strings.Join(paths, ",") != "/botTOKEN/editMessageText,/botTOKEN/deleteMessage" {
		t.Fatalf("race result=%#v paths=%#v", result, paths)
	}
	current, _ := app.Store.FindSession(id)
	if current.AgentDetailMessageID != 0 {
		t.Fatalf("superseded detail survived: %#v", current)
	}
}

func TestDismissAgentDetailRejectsStaleMessageIdentity(t *testing.T) {
	app, _, id := newSafetyApp(t, state.TerminalOriginCreated)
	if _, _, err := app.Store.UpdateSession(id, func(session *state.TerminalSession) {
		session.AgentDetailChatID, session.AgentDetailMessageID = 100, 88
	}); err != nil {
		t.Fatal(err)
	}
	callback := telegram.CallbackQuery{Message: &telegram.Message{MessageID: 87, Chat: telegram.Chat{ID: 100}}}
	if result := app.dismissAgentDetail(context.Background(), callback, id); result.Outcome != actionUserError {
		t.Fatalf("stale dismiss = %#v", result)
	}
	current, _ := app.Store.FindSession(id)
	if current.AgentDetailMessageID != 88 {
		t.Fatalf("stale callback changed detail: %#v", current)
	}
}

func TestRetireAgentDetailKeepsTelegramMessageWhenStateClearDoesNotCommit(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.AllocateSession("main", "@1", "%1", "shell")
	if err != nil {
		t.Fatal(err)
	}
	session = bindTestSession(t, store, session.ID)
	if _, _, err := store.UpdateSession(session.ID, func(current *state.TerminalSession) {
		current.WatchEnabled = true
		current.AgentDetailChatID = 100
		current.AgentDetailMessageID = 88
		current.AgentDetailAnchorMessageID = 77
	}); err != nil {
		t.Fatal(err)
	}
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	deleteCalls := 0
	client.HTTPClient = &http.Client{Transport: anchorDeliveryRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		deleteCalls++
		return telegramTestResponse(t, http.StatusOK, map[string]any{"ok": true, "result": true}), nil
	})}
	app := &App{Store: store, Telegram: client}
	expected, _ := store.FindSession(session.ID)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	app.retireAgentDetail(context.Background(), expected)
	current, _ := store.FindSession(session.ID)
	if deleteCalls != 0 || current.AgentDetailMessageID != 88 || current.AgentDetailChatID != 100 {
		t.Fatalf("uncommitted retirement: deletes=%d state=%#v", deleteCalls, current)
	}
}

func TestAgentDetailRetirementCoversCollapseCloseResumeAndAnchorReplacement(t *testing.T) {
	base := state.TerminalSession{State: state.TerminalRunning, AnchorMessageID: 77, AgentDetailMessageID: 88, AgentDetailAnchorMessageID: 77}
	if agentDetailNeedsRetirement(base) {
		t.Fatal("current running detail was retired")
	}
	cases := []state.TerminalSession{base, base, base, base}
	cases[0].Collapsed = true
	cases[1].State = state.TerminalClosed
	cases[2].PendingResume = &state.PendingResume{}
	cases[3].AnchorMessageID = 99
	for index, session := range cases {
		if !agentDetailNeedsRetirement(session) {
			t.Fatalf("retirement case %d was missed: %#v", index, session)
		}
	}
}
