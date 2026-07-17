package app

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
	"github.com/idolum-ai/engram/internal/tmux"
)

func TestBuildGuidedEvidenceCropMatchesWrappedRowsAndRejectsAmbiguity(t *testing.T) {
	plain := "context before\ntests passed\nsuccessfully in app\ncontext after\nidle prompt"
	capture := tmux.StyledCapture{Text: plain, ANSI: plain, Columns: 71, VisibleRows: 37, BufferRows: 5, CurrentPath: "/tmp"}
	crop, ok := buildGuidedEvidenceCrop(state.TerminalSession{ID: 3, Title: "build"}, capture, []string{"tests passed successfully in app"}, "contrast-dark")
	if !ok || crop.input.BufferRows != 5 || !crop.input.Compact || !reflect.DeepEqual(crop.input.HighlightRows, []int{1, 2}) {
		t.Fatalf("crop = %#v ok=%v", crop, ok)
	}

	capture.Text = "same decisive result\nother\nsame decisive result"
	capture.ANSI = capture.Text
	if _, ok := buildGuidedEvidenceCrop(state.TerminalSession{ID: 3}, capture, []string{"same decisive result"}, "terminal"); ok {
		t.Fatal("ambiguous evidence produced a crop")
	}
}

func TestBuildGuidedEvidenceCropRejectsWidelySeparatedEvidence(t *testing.T) {
	rows := make([]string, 24)
	for i := range rows {
		rows[i] = fmt.Sprintf("ordinary terminal row %02d", i)
	}
	rows[1] = "first uniquely decisive terminal result"
	rows[22] = "second uniquely decisive terminal result"
	text := strings.Join(rows, "\n")
	capture := tmux.StyledCapture{Text: text, ANSI: text, Columns: 71, VisibleRows: 37, BufferRows: len(rows)}
	if _, ok := buildGuidedEvidenceCrop(state.TerminalSession{ID: 1}, capture, []string{rows[1], rows[22]}, "terminal"); ok {
		t.Fatal("widely separated evidence produced a near-full screenshot")
	}
}

func TestGuidedEvidencePhotoIsStableThenRetiredAsStale(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.AllocateSession("main", "@1", "%1", "build")
	if err != nil {
		t.Fatal(err)
	}
	session = bindTestSession(t, store, session.ID)
	session, _, err = store.UpdateSession(session.ID, func(current *state.TerminalSession) {
		current.AnchorChatID = 100
		current.AnchorMessageID = 77
		current.AnchorFormat = "text"
		current.WatchEnabled = true
	})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		paths = append(paths, request.URL.Path)
		switch request.URL.Path {
		case "/botTOKEN/sendPhoto":
			return snapshotJSONResponse(`{"message_id":88,"chat":{"id":100}}`), nil
		case "/botTOKEN/editMessageMedia":
			return snapshotJSONResponse(`{"message_id":88,"chat":{"id":100}}`), nil
		case "/botTOKEN/deleteMessage":
			return snapshotJSONResponse(`true`), nil
		default:
			return nil, fmt.Errorf("unexpected Telegram endpoint %s", request.URL.Path)
		}
	})}
	renderer := &fakeSnapshotRenderer{}
	a := &App{
		Config: config.Config{Home: dir, SnapshotTheme: "contrast-dark"}, Store: store, Telegram: client,
		Snapshots: renderer, mode: "guide", snapshotReady: true, renderSlots: make(chan struct{}, 1),
	}
	first := tmux.StyledCapture{Text: "context\ntests passed successfully\nprompt", ANSI: "context\n\x1b[32mtests passed successfully\x1b[0m\nprompt", Columns: 71, VisibleRows: 37, BufferRows: 3, CurrentPath: "/tmp"}
	a.updateGuidedEvidence(context.Background(), session, first, []string{"tests passed successfully"})
	current, _ := store.FindSession(session.ID)
	if current.EvidenceMessageID != 88 || current.EvidenceAnchorMessageID != 77 || current.LastEvidenceHash == "" || renderer.renders != 1 || !renderer.input.Compact {
		t.Fatalf("first evidence state=%#v renderer=%#v", current, renderer)
	}
	if routed, targetState, ok := store.FindReplyTarget(100, 88); !ok || targetState != state.ReplyTargetCurrent || routed.ID != session.ID {
		t.Fatalf("evidence reply target = %#v %q ok=%v", routed, targetState, ok)
	}

	second := first
	second.Text = "context\nnew decisive result\nprompt"
	second.ANSI = second.Text
	a.updateGuidedEvidence(context.Background(), current, second, []string{"new decisive result"})
	a.updateGuidedEvidence(context.Background(), current, second, []string{"missing fabricated evidence"})
	retired, _ := store.FindSession(session.ID)
	if retired.EvidenceMessageID != 0 || !reflect.DeepEqual(paths, []string{"/botTOKEN/sendPhoto", "/botTOKEN/editMessageMedia", "/botTOKEN/deleteMessage"}) {
		t.Fatalf("retired evidence=%#v paths=%#v", retired, paths)
	}
	if _, targetState, ok := store.FindReplyTarget(100, 88); !ok || targetState != state.ReplyTargetStale {
		t.Fatalf("retired evidence target = %q ok=%v", targetState, ok)
	}
}
