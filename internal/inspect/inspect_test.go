package inspect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/idolum-ai/engram/internal/state"
)

type fakeRunner struct {
	calls      [][]string
	listErr    error
	captureOut string
}

const testServerID = "0123456789abcdef0123456789abcdef"

func (r *fakeRunner) Run(_ context.Context, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	switch args[0] {
	case "show-options":
		return testServerID + "\n", nil
	case "list-panes":
		if r.listErr != nil {
			return "", r.listErr
		}
		return "$1\t@2\t%3\twork\t0\t0\t1\t/tmp/project\tbash\n", nil
	case "display-message":
		if reflect.DeepEqual(args[len(args)-1], "#{pane_width}\t#{pane_height}") {
			return "80\t24\n", nil
		}
		return "$1\t@2\t%3\twork\t0\t0\t1\t/tmp/project\tbash\n", nil
	case "capture-pane":
		if r.captureOut != "" {
			return r.captureOut, nil
		}
		return "\x1b[31mfailed\x1b[0m\nnext step\n", nil
	default:
		return "", fmt.Errorf("unexpected tmux call: %v", args)
	}
}

func TestInspectHelpAndUsageDoNotReadState(t *testing.T) {
	inspector := Inspector{Home: filepath.Join(t.TempDir(), "missing"), Runner: &fakeRunner{}}
	var out bytes.Buffer
	if err := inspector.Run(context.Background(), []string{"--help"}, &out); err != nil || !strings.Contains(out.String(), "engram inspect frame") {
		t.Fatalf("help output=%q err=%v", out.String(), err)
	}
	if err := inspector.Run(context.Background(), []string{"frame", "nope"}, &out); !IsUsageError(err) {
		t.Fatalf("invalid usage error = %v", err)
	}
}

func TestInspectStatusWritesStateBeforeTmuxFailure(t *testing.T) {
	var out bytes.Buffer
	err := (Inspector{Home: writeState(t), Runner: &fakeRunner{listErr: errors.New("tmux down")}}).Run(context.Background(), []string{"status"}, &out)
	if err == nil || !strings.Contains(out.String(), "state: readable") || !strings.Contains(out.String(), "tmux: unavailable") {
		t.Fatalf("status output=%q err=%v", out.String(), err)
	}
}

func TestInspectFrameStripsFormatControlsAndCapsWholeOutput(t *testing.T) {
	runner := &fakeRunner{captureOut: "safe\u202eevil\u2066\n" + strings.Repeat("x", maxFrameBytes)}
	var out bytes.Buffer
	if err := (Inspector{Home: writeState(t), Runner: runner}).Run(context.Background(), []string{"frame", "1"}, &out); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(out.String(), "\u202e\u2066") || out.Len() > maxFrameBytes {
		t.Fatalf("unsafe or oversized frame: bytes=%d", out.Len())
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
	for _, want := range []string{"state: readable", "schema: 8", "watches: 1", "tmux: available (1 panes)"} {
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
	if len(runner.calls) != 4 || runner.calls[0][0] != "show-options" || runner.calls[1][0] != "display-message" || runner.calls[2][len(runner.calls[2])-1] != "#{pane_width}\t#{pane_height}" || runner.calls[3][0] != "capture-pane" {
		t.Fatalf("tmux calls = %#v", runner.calls)
	}
	if !containsSequence(runner.calls[3], []string{"-S", "-40", "-E", "23", "-t", "%3"}) {
		t.Fatalf("capture bounds = %#v", runner.calls[3])
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
		Version:       8,
		NextSessionID: 2,
		TerminalSessions: []state.TerminalSession{{
			ID:              1,
			TmuxSessionName: "work",
			TmuxWindowID:    "@2",
			TmuxPaneID:      "%3",
			TmuxServerID:    testServerID,
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
