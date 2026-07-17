package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/state"
	"github.com/idolum-ai/engram/internal/telegram"
	"github.com/idolum-ai/engram/internal/terminalshot"
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

func TestGuidedEvidenceConvertsCanonicalAnchorInPlaceAndUsesTailFallback(t *testing.T) {
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
	if !a.updateGuidedAnchorWithEvidence(context.Background(), session, first, conversationFrame{}, first.Text, "Tests passed.", visibleReferences{}, []string{"tests passed successfully"}, true, nil, nil) {
		t.Fatal("guided anchor was not updated")
	}
	current, _ := store.FindSession(session.ID)
	if current.AnchorMessageID != 77 || current.AnchorFormat != anchorFormatGuideEvidence || current.LastRenderHash == "" || renderer.renders != 1 || !renderer.input.Compact {
		t.Fatalf("first evidence state=%#v renderer=%#v", current, renderer)
	}
	if routed, targetState, ok := store.FindReplyTarget(100, 77); !ok || targetState != state.ReplyTargetCurrent || routed.ID != session.ID {
		t.Fatalf("evidence reply target = %#v %q ok=%v", routed, targetState, ok)
	}
	frame, frameOK := a.snapshotTextFrame(current)
	if !frameOK || frame.JoinedText != first.Text || !strings.Contains(string(mustJSON(a.anchorMarkup(current))), "raw:1") {
		t.Fatalf("evidence text companion=%#v ok=%v markup=%s", frame, frameOK, mustJSON(a.anchorMarkup(current)))
	}

	second := first
	second.Text = "context\nnew decisive result\nprompt"
	second.ANSI = second.Text
	if !a.updateGuidedAnchorWithEvidence(context.Background(), current, second, conversationFrame{}, second.Text, "A result needs inspection.", visibleReferences{}, []string{"missing fabricated evidence"}, true, nil, nil) {
		t.Fatal("fallback anchor was not updated")
	}
	fallback, _ := store.FindSession(session.ID)
	if fallback.AnchorMessageID != 77 || fallback.AnchorFormat != anchorFormatGuideEvidence || renderer.renders != 2 || !strings.Contains(renderer.input.ANSI, "new decisive result") || renderer.input.Footer != "current terminal tail" || !reflect.DeepEqual(paths, []string{"/botTOKEN/editMessageMedia", "/botTOKEN/editMessageMedia"}) {
		t.Fatalf("fallback state=%#v renderer=%#v paths=%#v", fallback, renderer, paths)
	}
}

func TestGuidedRecentActivityCropChoosesLastChangedRegion(t *testing.T) {
	previousText := strings.Join([]string{"header", "old early", "stable one", "stable two", "stable three", "old latest", "prompt"}, "\n")
	currentText := strings.Join([]string{"header", "new early", "stable one", "stable two", "stable three", "new latest", "prompt"}, "\n")
	previous := conversationFrame{
		serverID: "server", windowID: "@1", paneID: "%1", command: "codex", columns: 71, visibleRows: 37, physicalText: previousText,
	}
	capture := tmux.StyledCapture{
		Text: currentText, ANSI: currentText, ServerID: "server", WindowID: "@1", PaneID: "%1", CurrentCmd: "codex",
		Columns: 71, VisibleRows: 37, BufferRows: 7, CurrentPath: "/work",
	}
	crop, ok := buildGuidedRecentActivityCrop(state.TerminalSession{ID: 2, Title: "work"}, capture, previous, "contrast-dark")
	if !ok || crop.source != guidedEvidenceChanged || crop.input.Footer != "changed terminal region" || !strings.Contains(crop.plain, "new latest") || strings.Contains(crop.plain, "new early") || len(crop.input.HighlightRows) != 1 {
		t.Fatalf("recent crop=%#v ok=%v", crop, ok)
	}
}

func TestGuidedEvidenceRejectsExcerptOutsideModelInput(t *testing.T) {
	app := &App{Config: config.Config{SnapshotTheme: "contrast-dark"}}
	capture := tmux.StyledCapture{
		Text: "substantive build result\nRun /review on my current changes", ANSI: "substantive build result\nRun /review on my current changes",
		Columns: 71, VisibleRows: 37, BufferRows: 2,
	}
	crop := app.selectGuidedEvidenceCrop(state.TerminalSession{ID: 2}, capture, conversationFrame{}, "substantive build result", []string{"Run /review on my current changes"})
	if crop.source == guidedEvidenceExcerpt {
		t.Fatalf("out-of-evidence excerpt was selected: %#v", crop)
	}
}

func TestGuidedRangeCropCarriesInheritedANSIState(t *testing.T) {
	plain := []string{"red one", "red two", "plain"}
	ansi := []string{"\x1b[31mred one", "red two", "\x1b[39mplain"}
	capture := tmux.StyledCapture{Columns: 71, VisibleRows: 37}
	crop := buildGuidedRangeCrop(state.TerminalSession{ID: 2}, capture, plain, ansi, 1, 1, []int{0}, "quoted terminal text", guidedEvidenceExcerpt, "terminal")
	if !strings.HasPrefix(crop.input.ANSI, "\x1b[31m") || !strings.Contains(crop.input.ANSI, "red two") {
		t.Fatalf("inherited ANSI state was lost: %q", crop.input.ANSI)
	}
}

func TestGuidedEvidenceHorizontallyFramesQuotedText(t *testing.T) {
	line := strings.Repeat(" ", 145) + "decisive result near the right edge"
	capture := tmux.StyledCapture{Text: line, ANSI: line, Columns: 200, VisibleRows: 50, BufferRows: 1}
	crop, ok := buildGuidedEvidenceCrop(state.TerminalSession{ID: 2}, capture, []string{"decisive result near the right edge"}, "terminal")
	if !ok || crop.input.ColumnOffset <= 0 || crop.input.ColumnOffset > 128 {
		t.Fatalf("wide quoted crop=%#v ok=%v", crop, ok)
	}
}

func TestGuidedTailOmitsRecognizedPassiveChrome(t *testing.T) {
	text := "build completed\n\n› Find and fix a bug in @filename\ngpt-5.6-sol · normal · [ready]"
	capture := trimPassiveCapture(tmux.StyledCapture{Text: text, ANSI: text, Columns: 71, VisibleRows: 37, BufferRows: 4})
	crop, ok := buildGuidedTailCrop(state.TerminalSession{ID: 2}, capture, "terminal")
	if !ok || crop.plain != "build completed" {
		t.Fatalf("passive chrome remained in fallback: %#v ok=%v", crop, ok)
	}
}

func TestGuidedTailCropUsesLastMeaningfulBlockWithoutHighlight(t *testing.T) {
	text := "older output\n\nlast result\nnext action\n\n"
	capture := tmux.StyledCapture{Text: text, ANSI: text, Columns: 71, VisibleRows: 37, BufferRows: 5, CurrentPath: "/work"}
	crop, ok := buildGuidedTailCrop(state.TerminalSession{ID: 2}, capture, "terminal")
	if !ok || crop.source != guidedEvidenceTail || crop.input.Footer != "current terminal tail" || crop.plain != "last result\nnext action" || len(crop.input.HighlightRows) != 0 {
		t.Fatalf("tail crop=%#v ok=%v", crop, ok)
	}
}

func TestGuidedFallbackNeverSelectsKnownSecretPixels(t *testing.T) {
	app := &App{Config: config.Config{TelegramBotToken: "known-secret", SnapshotTheme: "contrast-dark"}}
	capture := tmux.StyledCapture{Text: "result known-secret", ANSI: "result known-secret", CurrentPath: "/tmp/known-secret", Columns: 71, VisibleRows: 37, BufferRows: 1}
	crop := app.selectGuidedEvidenceCrop(state.TerminalSession{ID: 2, Title: "known-secret build"}, capture, conversationFrame{}, capture.Text, nil)
	if crop.source != guidedEvidencePlain || crop.input.Footer != "current terminal tail" || strings.Contains(crop.input.ANSI+crop.input.Title+crop.input.CWD, "known-secret") || !strings.Contains(crop.input.ANSI, "<redacted>") {
		t.Fatalf("secret fallback crop=%#v", crop)
	}
}

func TestGuidedFallbackKeepsEmptyFrameQuiet(t *testing.T) {
	app := &App{Config: config.Config{SnapshotTheme: "contrast-dark"}}
	capture := tmux.StyledCapture{Columns: 71, VisibleRows: 37}
	crop := app.selectGuidedEvidenceCrop(state.TerminalSession{ID: 2, Title: "work"}, capture, conversationFrame{}, capture.Text, nil)
	if crop.source != guidedEvidenceGuide || crop.input.Footer != "guided view" || strings.TrimSpace(crop.input.ANSI) != "" {
		t.Fatalf("empty fallback crop=%#v", crop)
	}
}

func TestUnwatchSupersedesBlockedGuidedRender(t *testing.T) {
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
	renderer := &blockingSnapshotRenderer{started: make(chan struct{}), release: make(chan struct{})}
	telegramCalls := 0
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(*http.Request) (*http.Response, error) {
		telegramCalls++
		return nil, errors.New("unwatched render reached Telegram")
	})}
	a := &App{Config: config.Config{Home: dir}, Store: store, Telegram: client, Snapshots: renderer, mode: "guide", snapshotReady: true}
	capture := tmux.StyledCapture{Text: "tests passed", ANSI: "tests passed", Columns: 71, VisibleRows: 37, BufferRows: 1}
	done := make(chan bool, 1)
	go func() {
		done <- a.updateGuidedAnchorWithEvidence(context.Background(), session, capture, conversationFrame{}, capture.Text, "Tests passed.", visibleReferences{}, []string{"tests passed"}, true, nil, nil)
	}()
	<-renderer.started
	if _, _, err := store.UpdateSession(session.ID, func(current *state.TerminalSession) { current.WatchEnabled = false }); err != nil {
		t.Fatal(err)
	}
	close(renderer.release)
	if <-done || telegramCalls != 0 {
		t.Fatalf("unwatched render published: calls=%d", telegramCalls)
	}
}

func TestGuidedEvidenceReplacesUnavailableAnchorAndDeletesMediaPredecessor(t *testing.T) {
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
		current.AnchorFormat = anchorFormatGuideEvidence
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
			return telegramTestResponse(t, http.StatusBadRequest, map[string]any{"ok": false, "error_code": 400, "description": "Bad Request: message can't be edited"}), nil
		case "/botTOKEN/sendPhoto":
			return snapshotJSONResponse(`{"message_id":88,"chat":{"id":100}}`), nil
		case "/botTOKEN/pinChatMessage", "/botTOKEN/deleteMessage":
			return snapshotJSONResponse(`true`), nil
		default:
			return nil, fmt.Errorf("unexpected Telegram endpoint %s", request.URL.Path)
		}
	})}
	a := &App{
		Config: config.Config{Home: dir, TelegramChatID: 100, SnapshotTheme: "contrast-dark"}, Store: store, Telegram: client,
		Snapshots: &fakeSnapshotRenderer{}, mode: "guide", snapshotReady: true,
	}
	capture := tmux.StyledCapture{Text: "tests passed successfully", ANSI: "\x1b[32mtests passed successfully", Columns: 71, VisibleRows: 37, BufferRows: 1, CurrentPath: "/tmp"}
	if !a.updateGuidedAnchorWithEvidence(context.Background(), session, capture, conversationFrame{}, capture.Text, "Tests passed.", visibleReferences{}, []string{"tests passed successfully"}, true, nil, nil) {
		t.Fatal("replacement guided anchor was not accepted")
	}
	got, _ := store.FindSession(session.ID)
	frame, frameOK := a.snapshotTextFrame(got)
	if got.AnchorMessageID != 88 || got.AnchorFormat != anchorFormatGuideEvidence || got.RetiringAnchorMessageID != 0 || !got.AnchorPinned || !reflect.DeepEqual(got.StaleAlternateMessageIDs, []int{77}) || !frameOK || frame.MessageID != 88 {
		t.Fatalf("replacement state=%#v frame=%#v ok=%v", got, frame, frameOK)
	}
	want := []string{"/botTOKEN/editMessageMedia", "/botTOKEN/sendPhoto", "/botTOKEN/pinChatMessage", "/botTOKEN/deleteMessage"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("replacement paths=%#v want=%#v", paths, want)
	}
}

func TestProspectiveMediaCleanupOutlivesCallerCancellation(t *testing.T) {
	calls := 0
	client := telegram.New("TOKEN")
	client.BaseURL = "https://api.telegram.org/botTOKEN"
	client.HTTPClient = &http.Client{Transport: snapshotRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.Context().Err() != nil || request.URL.Path != "/botTOKEN/deleteMessage" {
			t.Fatalf("cleanup request path=%s err=%v", request.URL.Path, request.Context().Err())
		}
		return snapshotJSONResponse(`true`), nil
	})}
	a := &App{Telegram: client}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.deactivateProspectiveMediaAnchor(ctx, 100, 88)
	if calls != 1 {
		t.Fatalf("cleanup calls=%d", calls)
	}
}

type blockingSnapshotRenderer struct {
	started chan struct{}
	release chan struct{}
}

func (r *blockingSnapshotRenderer) Available() (string, error) { return "/usr/bin/chromium", nil }

func (r *blockingSnapshotRenderer) Render(_ context.Context, _ terminalshot.Input, dir string) (string, error) {
	close(r.started)
	<-r.release
	path := filepath.Join(dir, "blocked.png")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return path, os.WriteFile(path, []byte("png"), 0o600)
}
