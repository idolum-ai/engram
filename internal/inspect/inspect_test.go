package inspect

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/idolum-ai/engram/internal/state"
)

type fakeRunner struct {
	calls [][]string
}

func (r *fakeRunner) Run(_ context.Context, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	switch args[0] {
	case "list-panes":
		return "$1\t@2\t%3\twork\t0\t0\t1\t/tmp/project\tbash\n", nil
	case "display-message":
		if reflect.DeepEqual(args[len(args)-1], "#{pane_width}\t#{pane_height}") {
			return "80\t24\n", nil
		}
		return "$1\t@2\t%3\twork\t0\t0\t1\t/tmp/project\tbash\n", nil
	case "capture-pane":
		return "\x1b[31mfailed\x1b[0m\nnext step\n", nil
	default:
		return "", fmt.Errorf("unexpected tmux call: %v", args)
	}
}

func TestInspectStatusAndSessionsAreBoundedReadOnly(t *testing.T) {
	home := writeState(t)
	runner := &fakeRunner{}
	inspector := Inspector{Home: home, Runner: runner}
	var out bytes.Buffer
	if err := inspector.Run(context.Background(), []string{"status"}, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"state: readable", "schema: 7", "watches: 1", "tmux: available (1 panes)"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("status missing %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	if err := inspector.Run(context.Background(), []string{"sessions"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "[1] state=running origin=attached pane=%3 window=@2") || strings.Contains(out.String(), "\x1b") {
		t.Fatalf("sessions output = %q", out.String())
	}
	if len(runner.calls) != 1 || runner.calls[0][0] != "list-panes" {
		t.Fatalf("unexpected tmux calls = %#v", runner.calls)
	}
}

func TestInspectFrameValidatesIdentityThenCapturesBoundedLiteralText(t *testing.T) {
	home := writeState(t)
	runner := &fakeRunner{}
	var out bytes.Buffer
	if err := (Inspector{Home: home, Runner: runner}).Run(context.Background(), []string{"frame", "1"}, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "\x1b") || !strings.Contains(out.String(), "---\nfailed\nnext step\n") {
		t.Fatalf("frame output = %q", out.String())
	}
	if len(runner.calls) != 3 || runner.calls[0][0] != "display-message" || runner.calls[1][len(runner.calls[1])-1] != "#{pane_width}\t#{pane_height}" || runner.calls[2][0] != "capture-pane" {
		t.Fatalf("tmux calls = %#v", runner.calls)
	}
	if !containsSequence(runner.calls[2], []string{"-S", "-40", "-E", "23", "-t", "%3"}) {
		t.Fatalf("capture bounds = %#v", runner.calls[2])
	}
}

func TestInspectDoesNotLoadEnvironmentFileOrModifyState(t *testing.T) {
	home := writeState(t)
	path := filepath.Join(home, "state.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".env"), []byte("TELEGRAM_BOT_TOKEN=must-not-be-read\n"), 0); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := (Inspector{Home: home, Runner: &fakeRunner{}}).Run(context.Background(), []string{"sessions"}, &out); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("inspect modified state")
	}
}

func writeState(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	snapshot := state.State{
		Version:       7,
		NextSessionID: 2,
		TerminalSessions: []state.TerminalSession{{
			ID:              1,
			TmuxSessionName: "work",
			TmuxWindowID:    "@2",
			TmuxPaneID:      "%3",
			Origin:          state.TerminalOriginAttached,
			Title:           "build\npoison",
			LastKnownCWD:    "/tmp/project",
			State:           state.TerminalRunning,
		}},
	}
	body, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "state.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func containsSequence(args, sequence []string) bool {
	for i := 0; i+len(sequence) <= len(args); i++ {
		if reflect.DeepEqual(args[i:i+len(sequence)], sequence) {
			return true
		}
	}
	return false
}
