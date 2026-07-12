package mechanics

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/tmux"
)

type callRunner struct {
	calls [][]string
}

const testServerID = "0123456789abcdef0123456789abcdef"

func (r *callRunner) Run(_ context.Context, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	switch args[0] {
	case "show-options":
		return testServerID + "\n", nil
	case "display-message":
		return "$1\t@2\t%3\twork\t0\t0\t1\t/tmp/project\tbash\n", nil
	case "if-shell":
		if strings.Contains(args[4], "@9") {
			return "ENGRAM_IDENTITY_MISMATCH\n", nil
		}
		return "", nil
	case "send-keys", "kill-window":
		return "", nil
	default:
		return "", fmt.Errorf("unexpected tmux call: %v", args)
	}
}

func TestCloseWindowValidatesBindingBeforeDestructiveEffect(t *testing.T) {
	runner := &callRunner{}
	controller := New(tmux.New(runner))
	if _, err := controller.CloseWindow(context.Background(), Binding{PaneID: "%3", WindowID: "@2", ServerID: testServerID}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 || runner.calls[0][0] != "if-shell" {
		t.Fatalf("calls = %#v", runner.calls)
	}

	runner.calls = nil
	if _, err := controller.CloseWindow(context.Background(), Binding{PaneID: "%3", WindowID: "@9", ServerID: testServerID}); err == nil {
		t.Fatal("CloseWindow accepted mismatched identity")
	}
	if len(runner.calls) != 1 || runner.calls[0][0] != "if-shell" {
		t.Fatalf("mismatched close calls = %#v", runner.calls)
	}
}

func TestInvalidKeysHaveNoTmuxEffect(t *testing.T) {
	runner := &callRunner{}
	controller := New(tmux.New(runner))
	if _, err := controller.SendKeys(context.Background(), Binding{PaneID: "%3", WindowID: "@2", ServerID: testServerID}, []string{"Enter\nrun-shell"}); err == nil {
		t.Fatal("SendKeys accepted invalid key")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("invalid key calls = %#v", runner.calls)
	}
}

func TestSendTextValidatesImmutableIdentityBeforeEffect(t *testing.T) {
	runner := &callRunner{}
	controller := New(tmux.New(runner))
	pane, err := controller.SendText(context.Background(), Binding{PaneID: "%3", WindowID: "@2", ServerID: testServerID}, "echo ok")
	if err != nil {
		t.Fatal(err)
	}
	if pane.ID != "%3" || pane.WindowID != "@2" || pane.CurrentPath != "/tmp/project" {
		t.Fatalf("pane = %#v", pane)
	}
	want := [][]string{
		{"show-options", "-gqv", "@engram_server_id"},
		{"display-message", "-p", "-t", "%3", "#{session_id}\t#{window_id}\t#{pane_id}\t#{session_name}\t#{window_index}\t#{pane_index}\t#{pane_active}\t#{pane_current_path}\t#{pane_current_command}"},
		{"send-keys", "-t", "%3", "-l", "--", "echo ok"},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("calls = %#v, want %#v", runner.calls, want)
	}
}

func TestIdentityMismatchPreventsInput(t *testing.T) {
	runner := &callRunner{}
	controller := New(tmux.New(runner))
	_, err := controller.SendText(context.Background(), Binding{PaneID: "%3", WindowID: "@9", ServerID: testServerID}, "must not send")
	if err == nil || !tmux.IsIdentityLoss(err) {
		t.Fatalf("error = %v", err)
	}
	if len(runner.calls) != 2 || runner.calls[0][0] != "show-options" || runner.calls[1][0] != "display-message" {
		t.Fatalf("calls after mismatch = %#v", runner.calls)
	}
}

func TestCommandKeepsLiteralAndEnterDistinct(t *testing.T) {
	runner := &callRunner{}
	controller := New(tmux.New(runner))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := controller.SendCommand(ctx, Binding{PaneID: "%3", WindowID: "@2", ServerID: testServerID}, "printf 'a b'"); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 4 {
		t.Fatalf("calls = %#v", runner.calls)
	}
	if got := strings.Join(runner.calls[2], " "); got != "send-keys -t %3 -l -- printf 'a b'" {
		t.Fatalf("literal call = %q", got)
	}
	if got := runner.calls[3]; !reflect.DeepEqual(got, []string{"send-keys", "-t", "%3", "Enter"}) {
		t.Fatalf("enter call = %#v", got)
	}
}
