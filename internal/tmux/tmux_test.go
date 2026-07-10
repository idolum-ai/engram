package tmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
)

type fakeRunner struct {
	calls [][]string
	out   string
	err   error
}

func (f *fakeRunner) Run(ctx context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	return f.out, f.err
}

type fakeStreamRunner struct {
	calls  [][]string
	chunks []string
	err    error
}

type sequenceRunner struct {
	calls   [][]string
	outputs []string
}

func (r *sequenceRunner) Run(_ context.Context, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	if len(r.outputs) == 0 {
		return "", nil
	}
	out := r.outputs[0]
	r.outputs = r.outputs[1:]
	return out, nil
}

func (f *fakeStreamRunner) Run(ctx context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	return strings.Join(f.chunks, ""), f.err
}

func (f *fakeStreamRunner) RunToWriter(ctx context.Context, dst io.Writer, args ...string) error {
	f.calls = append(f.calls, append([]string(nil), args...))
	for _, chunk := range f.chunks {
		if _, err := io.WriteString(dst, chunk); err != nil {
			return err
		}
	}
	return f.err
}

func TestSendCommandSendsLiteralThenEnter(t *testing.T) {
	oldDelay := commandSubmitDelay
	commandSubmitDelay = 0
	t.Cleanup(func() { commandSubmitDelay = oldDelay })

	f := &fakeRunner{}
	m := New(f)
	if err := m.SendCommand(context.Background(), "%1", "ls -la"); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"send-keys", "-t", "%1", "-l", "--", "ls -la"},
		{"send-keys", "-t", "%1", "Enter"},
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls = %#v", f.calls)
	}
}

func TestListSessionsParsesTmuxOutput(t *testing.T) {
	f := &fakeRunner{
		out: "main\t$1\t3\t1\nother\t$2\t1\t0\n",
	}
	m := New(f)
	got, err := m.ListSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Session{
		{Name: "main", ID: "$1", Windows: "3", Attached: "1"},
		{Name: "other", ID: "$2", Windows: "1", Attached: "0"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListSessions = %#v, want %#v", got, want)
	}
}

func TestListWindowsParsesTmuxOutput(t *testing.T) {
	f := &fakeRunner{
		out: "main\t0\t@1\tcode\t1\t%2\t/home/me\tbash\n",
	}
	m := New(f)
	got, err := m.ListWindows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Window{{
		SessionName: "main",
		Index:       "0",
		ID:          "@1",
		Name:        "code",
		Active:      "1",
		PaneID:      "%2",
		CurrentPath: "/home/me",
		CurrentCmd:  "bash",
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListWindows = %#v, want %#v", got, want)
	}
}

func TestResolveTargetUsesTmuxTarget(t *testing.T) {
	f := &fakeRunner{
		out: "main\t0\t@1\tcode\t1\t%2\t/home/me\tbash\n",
	}
	m := New(f)
	got, err := m.ResolveTarget(context.Background(), "main:0")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "@1" || got.PaneID != "%2" {
		t.Fatalf("ResolveTarget = %#v", got)
	}
	if len(f.calls) != 1 || !reflect.DeepEqual(f.calls[0][0:4], []string{"display-message", "-p", "-t", "main:0"}) {
		t.Fatalf("calls = %#v", f.calls)
	}
}

func TestCaptureVisibleRawPreservesTmuxOutput(t *testing.T) {
	wantOutput := "\x1b[31mred\x1b[0m   \nwrapped   \n"
	f := &fakeRunner{out: wantOutput}

	got, err := New(f).CaptureVisibleRaw(context.Background(), "%7")
	if err != nil {
		t.Fatal(err)
	}
	if got != wantOutput {
		t.Fatalf("CaptureVisibleRaw = %q, want %q", got, wantOutput)
	}
	wantCalls := [][]string{{"capture-pane", "-p", "-e", "-N", "-t", "%7"}}
	if !reflect.DeepEqual(f.calls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", f.calls, wantCalls)
	}
}

func TestCaptureStyledIncludesHistoryAndVisiblePane(t *testing.T) {
	t.Parallel()
	runner := &sequenceRunner{outputs: []string{
		"71\t37\tbuild\t/home/me\n",
		strings.Repeat("\x1b[31mhistory and visible\x1b[0m\n", 64),
	}}
	got, err := New(runner).CaptureStyled(context.Background(), "%7", 64)
	if err != nil {
		t.Fatal(err)
	}
	if got.Columns != 71 || got.VisibleRows != 37 || got.BufferRows != 64 || got.Title != "build" || got.CurrentPath != "/home/me" {
		t.Fatalf("styled capture metadata = %#v", got)
	}
	if strings.Count(got.ANSI, "history and visible") != 64 {
		t.Fatalf("styled capture ANSI rows = %d", strings.Count(got.ANSI, "history and visible"))
	}
	if strings.Contains(got.Text, "\x1b") || strings.Count(got.Text, "history and visible") != 64 {
		t.Fatalf("styled capture text was not derived from ANSI capture: %q", got.Text)
	}
	wantCapture := []string{"capture-pane", "-p", "-e", "-N", "-S", "-27", "-E", "36", "-t", "%7"}
	if len(runner.calls) != 2 || !reflect.DeepEqual(runner.calls[1], wantCapture) {
		t.Fatalf("styled capture calls = %#v", runner.calls)
	}
}

func TestCaptureVisibleSemanticCleansTerminalOutput(t *testing.T) {
	f := &fakeRunner{out: strings.Join([]string{
		"\n",
		"\x1b[31mred\x1b[0m   \r\n",
		"link: \x1b]8;;https://example.com\x07label\x1b]8;;\x1b\\\n",
		"charset \x1b(Bkept\x00\x07\x7f\n",
		"dcs \x1bPignored\x1b\\done\t \n",
		"c1 \u009dignored\u009cdone\n",
	}, "")}

	got, err := New(f).CaptureVisibleSemantic(context.Background(), "%8")
	if err != nil {
		t.Fatal(err)
	}
	want := "red\nlink: label\ncharset kept\ndcs done\nc1 done"
	if got != want {
		t.Fatalf("CaptureVisibleSemantic = %q, want %q", got, want)
	}
	wantCalls := [][]string{{"capture-pane", "-p", "-J", "-t", "%8"}}
	if !reflect.DeepEqual(f.calls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", f.calls, wantCalls)
	}
}

func TestCaptureScrollbackTailBoundsStreamedOutput(t *testing.T) {
	t.Run("lines", func(t *testing.T) {
		f := &fakeStreamRunner{chunks: []string{"one\ntwo\n", "three\n", "four\n"}}
		got, err := New(f).CaptureScrollbackTail(context.Background(), "%9", 128, 2)
		if err != nil {
			t.Fatal(err)
		}
		if want := "three\nfour"; got != want {
			t.Fatalf("CaptureScrollbackTail = %q, want %q", got, want)
		}
		wantCalls := [][]string{{"capture-pane", "-p", "-J", "-S", "-", "-E", "-", "-t", "%9"}}
		if !reflect.DeepEqual(f.calls, wantCalls) {
			t.Fatalf("calls = %#v, want %#v", f.calls, wantCalls)
		}
	})

	t.Run("bytes across chunks", func(t *testing.T) {
		f := &fakeStreamRunner{chunks: []string{"0123", "456", "789"}}
		got, err := New(f).CaptureScrollbackTail(context.Background(), "%9", 5, 10)
		if err != nil {
			t.Fatal(err)
		}
		if want := "56789"; got != want {
			t.Fatalf("CaptureScrollbackTail = %q, want %q", got, want)
		}
		if len(got) > 5 {
			t.Fatalf("CaptureScrollbackTail returned %d bytes", len(got))
		}
	})

	t.Run("invalid limits", func(t *testing.T) {
		f := &fakeStreamRunner{}
		if _, err := New(f).CaptureScrollbackTail(context.Background(), "%9", 0, 1); err == nil {
			t.Fatal("CaptureScrollbackTail accepted zero max bytes")
		}
		if _, err := New(f).CaptureScrollbackTail(context.Background(), "%9", 1, 0); err == nil {
			t.Fatal("CaptureScrollbackTail accepted zero max lines")
		}
		if len(f.calls) != 0 {
			t.Fatalf("invalid limits invoked tmux: %#v", f.calls)
		}
	})
}

func TestDumpScrollbackStreamsRawPhysicalCapture(t *testing.T) {
	f := &fakeStreamRunner{chunks: []string{"\x1b[32mchunk one", "\nchunk two   \n"}}
	var dst bytes.Buffer
	if err := New(f).DumpScrollback(context.Background(), "%10", &dst); err != nil {
		t.Fatal(err)
	}
	if want := "\x1b[32mchunk one\nchunk two   \n"; dst.String() != want {
		t.Fatalf("dump = %q, want %q", dst.String(), want)
	}
	wantCalls := [][]string{{"capture-pane", "-p", "-e", "-N", "-S", "-", "-E", "-", "-t", "%10"}}
	if !reflect.DeepEqual(f.calls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", f.calls, wantCalls)
	}
}

func TestDumpScrollbackRequiresStreamingRunner(t *testing.T) {
	err := New(&fakeRunner{}).DumpScrollback(context.Background(), "%10", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "does not support streaming") {
		t.Fatalf("DumpScrollback error = %v", err)
	}
}

func TestListPanesParsesImmutableIdentity(t *testing.T) {
	f := &fakeRunner{out: strings.Join([]string{
		"$1\t@2\t%3\tmain\t0\t1\t1\t/home/me\tbash\n",
		"$1\t@4\t%5\tmain\t2\t0\t0\t/tmp\tvim\n",
	}, "")}
	got, err := New(f).ListPanes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Pane{
		{SessionID: "$1", WindowID: "@2", ID: "%3", SessionName: "main", WindowIndex: "0", Index: "1", Active: true, CurrentPath: "/home/me", CurrentCmd: "bash"},
		{SessionID: "$1", WindowID: "@4", ID: "%5", SessionName: "main", WindowIndex: "2", Index: "0", CurrentPath: "/tmp", CurrentCmd: "vim"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListPanes = %#v, want %#v", got, want)
	}
	format := "#{session_id}\t#{window_id}\t#{pane_id}\t#{session_name}\t#{window_index}\t#{pane_index}\t#{pane_active}\t#{pane_current_path}\t#{pane_current_command}"
	wantCalls := [][]string{{"list-panes", "-a", "-F", format}}
	if !reflect.DeepEqual(f.calls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", f.calls, wantCalls)
	}
}

func TestInspectAndValidatePaneIdentity(t *testing.T) {
	const output = "$1\t@2\t%3\tmain\t0\t1\t1\t/home/me\tbash\n"
	format := "#{session_id}\t#{window_id}\t#{pane_id}\t#{session_name}\t#{window_index}\t#{pane_index}\t#{pane_active}\t#{pane_current_path}\t#{pane_current_command}"

	t.Run("inspect", func(t *testing.T) {
		f := &fakeRunner{out: output}
		pane, err := New(f).InspectPane(context.Background(), "main:0.1")
		if err != nil {
			t.Fatal(err)
		}
		if pane.ID != "%3" || pane.WindowID != "@2" || pane.SessionID != "$1" {
			t.Fatalf("InspectPane = %#v", pane)
		}
		wantCalls := [][]string{{"display-message", "-p", "-t", "main:0.1", format}}
		if !reflect.DeepEqual(f.calls, wantCalls) {
			t.Fatalf("calls = %#v, want %#v", f.calls, wantCalls)
		}
	})

	t.Run("validate", func(t *testing.T) {
		f := &fakeRunner{out: output}
		pane, err := New(f).ValidatePane(context.Background(), "%3", "@2")
		if err != nil {
			t.Fatal(err)
		}
		if pane.ID != "%3" || pane.WindowID != "@2" {
			t.Fatalf("ValidatePane = %#v", pane)
		}
		wantCalls := [][]string{{"display-message", "-p", "-t", "%3", format}}
		if !reflect.DeepEqual(f.calls, wantCalls) {
			t.Fatalf("calls = %#v, want %#v", f.calls, wantCalls)
		}
	})

	t.Run("mismatch", func(t *testing.T) {
		f := &fakeRunner{out: output}
		if _, err := New(f).ValidatePane(context.Background(), "%3", "@9"); err == nil || !strings.Contains(err.Error(), "identity mismatch") {
			t.Fatalf("ValidatePane error = %v", err)
		}
	})

	for _, tc := range []struct {
		paneID   string
		windowID string
	}{
		{paneID: "main:0.1", windowID: "@2"},
		{paneID: "%3", windowID: "main:0"},
		{paneID: "%x", windowID: "@2"},
	} {
		t.Run(fmt.Sprintf("reject %s %s", tc.paneID, tc.windowID), func(t *testing.T) {
			f := &fakeRunner{out: output}
			if _, err := New(f).ValidatePane(context.Background(), tc.paneID, tc.windowID); err == nil {
				t.Fatal("ValidatePane accepted mutable or malformed identity")
			}
			if len(f.calls) != 0 {
				t.Fatalf("invalid identity invoked tmux: %#v", f.calls)
			}
		})
	}
}

func TestIsIdentityLoss(t *testing.T) {
	t.Parallel()
	tests := []struct {
		err  error
		want bool
	}{
		{err: fmt.Errorf("tmux display-message: exit status 1: can't find pane: %%8"), want: true},
		{err: fmt.Errorf("tmux pane identity mismatch: got pane %%8 window @9"), want: true},
		{err: fmt.Errorf("invalid tmux pane ID %q", "8"), want: true},
		{err: context.Canceled, want: false},
		{err: context.DeadlineExceeded, want: false},
		{err: errors.New("tmux server temporarily unavailable"), want: false},
	}
	for _, test := range tests {
		if got := IsIdentityLoss(test.err); got != test.want {
			t.Errorf("IsIdentityLoss(%q) = %v, want %v", test.err, got, test.want)
		}
	}
}

func TestListPanesRejectsMalformedIdentity(t *testing.T) {
	f := &fakeRunner{out: "$1\tmain:0\t%3\tmain\t0\t0\t1\t/tmp\tbash\n"}
	if _, err := New(f).ListPanes(context.Background()); err == nil {
		t.Fatal("ListPanes accepted mutable window identity")
	}
}

func TestShellQuote(t *testing.T) {
	got := ShellQuote("/tmp/it's here")
	want := "'/tmp/it'\"'\"'s here'"
	if got != want {
		t.Fatalf("ShellQuote = %q want %q", got, want)
	}
}

func TestValidKeysRejectsNewline(t *testing.T) {
	if err := ValidKeys([]string{"C-c\nrm"}); err == nil {
		t.Fatal("ValidKeys accepted newline")
	}
}
