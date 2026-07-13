package mechanics

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/idolum-ai/engram/internal/tmux"
)

type callRunner struct {
	calls [][]string
}

func framedRecord(values ...string) string {
	var out strings.Builder
	for _, value := range values {
		fmt.Fprintf(&out, "%d:%s", len(value), value)
	}
	return out.String() + "\n"
}

const testServerID = "0123456789abcdef0123456789abcdef"

func (r *callRunner) Run(_ context.Context, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	switch args[0] {
	case "show-options":
		return testServerID + "\n", nil
	case "display-message":
		return framedRecord(testServerID, "$1", "@2", "%3", "work", "0", "0", "1", "/tmp/project", "bash"), nil
	case "if-shell":
		if strings.Contains(args[4], "@9") {
			return "ENGRAM_IDENTITY_MISMATCH\n", nil
		}
		return "", nil
	case "set-buffer", "send-keys", "kill-window":
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
	if len(runner.calls) != 3 || runner.calls[0][0] != "display-message" || runner.calls[1][0] != "set-buffer" || runner.calls[1][4] != "echo ok" || runner.calls[2][0] != "if-shell" || !strings.Contains(runner.calls[2][5], "paste-buffer -r -d") {
		t.Fatalf("calls = %#v", runner.calls)
	}
}

func TestIdentityMismatchPreventsInput(t *testing.T) {
	runner := &callRunner{}
	controller := New(tmux.New(runner))
	_, err := controller.SendText(context.Background(), Binding{PaneID: "%3", WindowID: "@9", ServerID: testServerID}, "must not send")
	if err == nil || !tmux.IsIdentityLoss(err) {
		t.Fatalf("error = %v", err)
	}
	if len(runner.calls) != 1 || runner.calls[0][0] != "display-message" {
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
	if runner.calls[1][0] != "set-buffer" || runner.calls[1][4] != "printf 'a b'" || runner.calls[2][0] != "if-shell" || !strings.Contains(runner.calls[2][5], "paste-buffer -r -d") {
		t.Fatalf("literal calls = %#v", runner.calls)
	}
	if got := runner.calls[3]; got[0] != "if-shell" || !strings.Contains(got[5], "send-keys -t %3 'Enter'") {
		t.Fatalf("enter call = %#v", got)
	}
}
