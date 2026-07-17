package app

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

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

func TestGuidedEvidenceCaptionBoundsProseAndKeepsFileBindings(t *testing.T) {
	app := &App{}
	session := state.TerminalSession{ID: 4, State: state.TerminalRunning, Title: "release", LastKnownCWD: "/work/engram"}
	path := "/tmp/release-notes.md"
	caption, files := app.guidedEvidenceCaption(session, strings.Repeat("We are checking a faithful result with café. ", 80), visibleReferences{
		Paths: []string{path}, URLs: []string{"https://example.test/review"},
	})
	if len(caption) > guidedCaptionBytes || !utf8.ValidString(caption) {
		t.Fatalf("caption bytes=%d valid=%v", len(caption), utf8.ValidString(caption))
	}
	if !strings.Contains(caption, "files:\n```\n1. "+path+"\n```") || !reflect.DeepEqual(files, []string{path}) {
		t.Fatalf("caption=%q files=%#v", caption, files)
	}
}

func TestGuidedEvidenceConvertsCanonicalAnchorInPlaceAndUsesPlaceholder(t *testing.T) {
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
		case "/botTOKEN/editMessageMedia":
			if err := request.ParseMultipartForm(1 << 20); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(request.FormValue("media"), `"show_caption_above_media":false`) {
				t.Fatalf("media = %s", request.FormValue("media"))
			}
			return snapshotJSONResponse(`{"message_id":77,"chat":{"id":100}}`), nil
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
	if !a.updateGuidedAnchorWithEvidence(context.Background(), session, first, "Tests passed.", visibleReferences{}, []string{"tests passed successfully"}, true, nil, nil) {
		t.Fatal("guided anchor was not updated")
	}
	current, _ := store.FindSession(session.ID)
	if current.AnchorMessageID != 77 || current.AnchorFormat != anchorFormatGuideEvidence || current.LastRenderHash == "" || renderer.renders != 1 || !renderer.input.Compact {
		t.Fatalf("first evidence state=%#v renderer=%#v", current, renderer)
	}
	if routed, targetState, ok := store.FindReplyTarget(100, 77); !ok || targetState != state.ReplyTargetCurrent || routed.ID != session.ID {
		t.Fatalf("evidence reply target = %#v %q ok=%v", routed, targetState, ok)
	}

	second := first
	second.Text = "context\nnew decisive result\nprompt"
	second.ANSI = second.Text
	if !a.updateGuidedAnchorWithEvidence(context.Background(), current, second, "A result needs inspection.", visibleReferences{}, []string{"missing fabricated evidence"}, true, nil, nil) {
		t.Fatal("placeholder anchor was not updated")
	}
	placeholder, _ := store.FindSession(session.ID)
	if placeholder.AnchorMessageID != 77 || placeholder.AnchorFormat != anchorFormatGuideEvidence || renderer.renders != 2 || renderer.input.ANSI != "No verified terminal excerpt selected for this update." || renderer.input.Footer != "no verified terminal evidence" || !reflect.DeepEqual(paths, []string{"/botTOKEN/editMessageMedia", "/botTOKEN/editMessageMedia"}) {
		t.Fatalf("placeholder state=%#v renderer=%#v paths=%#v", placeholder, renderer, paths)
	}
}
