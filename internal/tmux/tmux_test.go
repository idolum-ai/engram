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

func tmuxRecord(values ...string) string {
	var out strings.Builder
	for _, value := range values {
		fmt.Fprintf(&out, "%d:%s", len(value), value)
	}
	return out.String() + "\n"
}

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

type styledCaptureRunner struct {
	calls  [][]string
	ansi   string
	joined string
}

type sequenceRunner struct {
	calls   [][]string
	outputs []string
}

type ensureRaceRunner struct {
	calls     [][]string
	listCalls int
}

func (r *ensureRaceRunner) Run(_ context.Context, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	if args[0] == "list-sessions" {
		r.listCalls++
		if r.listCalls > 1 {
			return tmuxRecord("0", "$7", "1", "0"), nil
		}
		return "", nil
	}
	if args[0] == "new-session" {
		return "", errors.New("duplicate session: 0")
	}
	return "", fmt.Errorf("unexpected tmux call: %v", args)
}

func (r *sequenceRunner) Run(_ context.Context, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	if len(r.outputs) == 0 {
		return "", fmt.Errorf("unexpected tmux call: %v", args)
	}
	out := r.outputs[0]
	r.outputs = r.outputs[1:]
	return out, nil
}

func (r *styledCaptureRunner) Run(_ context.Context, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	if len(args) > 0 && args[0] == "display-message" {
		return tmuxRecord("71", "37", "build", "/home/me"), nil
	}
	if len(args) > 0 && args[0] == "show-buffer" {
		if strings.Contains(args[len(args)-1], "physical") {
			return r.ansi, nil
		}
		return r.joined, nil
	}
	return "", nil
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
		out: tmuxRecord("main", "$1", "3", "1") + tmuxRecord("other", "$2", "1", "0"),
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

func TestSessionNamesResolveToImmutableSessionIDs(t *testing.T) {
	t.Run("existing session", func(t *testing.T) {
		f := &fakeRunner{out: tmuxRecord("0", "$4", "1", "0")}
		id, err := New(f).EnsureSession(context.Background(), "0", "/tmp")
		if err != nil {
			t.Fatal(err)
		}
		if id != "$4" {
			t.Fatalf("session ID = %q", id)
		}
		want := [][]string{{"list-sessions", "-F", "#{n:session_name}:#{session_name}#{n:session_id}:#{session_id}#{n:session_windows}:#{session_windows}#{n:session_attached}:#{session_attached}"}}
		if !reflect.DeepEqual(f.calls, want) {
			t.Fatalf("calls = %#v, want %#v", f.calls, want)
		}
	})

	t.Run("new window", func(t *testing.T) {
		f := &fakeRunner{out: tmuxRecord("@9", "%9")}
		windowID, paneID, err := New(f).NewWindow(context.Background(), "$4", "/tmp", "probe")
		if err != nil {
			t.Fatal(err)
		}
		if windowID != "@9" || paneID != "%9" {
			t.Fatalf("window=%q pane=%q", windowID, paneID)
		}
		want := []string{"new-window", "-P", "-F", "#{n:window_id}:#{window_id}#{n:pane_id}:#{pane_id}", "-n", "probe", "-c", "/tmp", "-t", "$4:"}
		if len(f.calls) != 1 || !reflect.DeepEqual(f.calls[0], want) {
			t.Fatalf("calls = %#v, want %#v", f.calls, want)
		}
	})

	t.Run("creation race reconciles exact postcondition", func(t *testing.T) {
		runner := &ensureRaceRunner{}
		id, err := New(runner).EnsureSession(context.Background(), "0", "/tmp")
		if err != nil || id != "$7" {
			t.Fatalf("session ID = %q, error = %v", id, err)
		}
		if len(runner.calls) != 3 || runner.calls[1][0] != "new-session" {
			t.Fatalf("calls = %#v", runner.calls)
		}
	})

	t.Run("canonicalized name is rejected and removed", func(t *testing.T) {
		runner := &sequenceRunner{outputs: []string{"", tmuxRecord("$2", "foo_bar"), ""}}
		if _, err := New(runner).EnsureSession(context.Background(), "foo:bar", "/tmp"); err == nil || !strings.Contains(err.Error(), "canonicalized") {
			t.Fatalf("EnsureSession error = %v", err)
		}
		if len(runner.calls) != 3 || !reflect.DeepEqual(runner.calls[2], []string{"kill-session", "-t", "$2"}) {
			t.Fatalf("calls = %#v", runner.calls)
		}
	})

	t.Run("reject mutable session target", func(t *testing.T) {
		f := &fakeRunner{out: tmuxRecord("@9", "%9")}
		if _, _, err := New(f).NewWindow(context.Background(), "0", "/tmp", "probe"); err == nil {
			t.Fatal("NewWindow accepted a session name instead of an immutable ID")
		}
		if len(f.calls) != 0 {
			t.Fatalf("invalid session ID invoked tmux: %#v", f.calls)
		}
	})
}

func TestListWindowsParsesTmuxOutput(t *testing.T) {
	f := &fakeRunner{
		out: tmuxRecord("$7", "main", "0", "@1", "code", "1", "%2", "/home/me", "bash"),
	}
	m := New(f)
	got, err := m.ListWindows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Window{{
		SessionID:   "$7",
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
		out: tmuxRecord("$7", "main", "0", "@1", "code", "1", "%2", "/home/me", "bash"),
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

func TestMetadataRecordsPreserveValuesAndRejectPartialIdentity(t *testing.T) {
	t.Run("delimiter-like and Unicode values", func(t *testing.T) {
		out := tmuxRecord("$7", "main:one_\t雪", "12", "@8", "build: tab\t雪", "1", "%9", "/tmp/a:b_\t雪", "bash")
		windows, err := parseWindows(out)
		if err != nil {
			t.Fatal(err)
		}
		if len(windows) != 1 || windows[0].SessionName != "main:one_\t雪" || windows[0].Name != "build: tab\t雪" || windows[0].CurrentPath != "/tmp/a:b_\t雪" {
			t.Fatalf("windows = %#v", windows)
		}
	})

	for name, output := range map[string]string{
		"legacy delimiters": "$7_main_0_@8_build_1_%9_/tmp_bash\n",
		"missing field":     tmuxRecord("$7", "main", "0", "@8", "build", "1", "%9", "/tmp"),
		"short value":       "5:$7",
		"trailing bytes":    tmuxRecord("$7", "main", "0", "@8", "build", "1", "%9", "/tmp", "bash") + "x",
		"blank record":      tmuxRecord("$7", "main", "0", "@8", "build", "1", "%9", "/tmp", "bash") + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseWindows(output); err == nil {
				t.Fatalf("parseWindows accepted %q", output)
			}
		})
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
	ansi := strings.Repeat("\x1b[31mhistory and visible\x1b[0m\n", 64)
	runner := &styledCaptureRunner{ansi: ansi, joined: "joined logical line\n"}
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
	if got.JoinedText != "joined logical line" {
		t.Fatalf("joined text = %q", got.JoinedText)
	}
	if len(runner.calls) != 5 || runner.calls[2][0] != "show-buffer" || runner.calls[3][0] != "show-buffer" || runner.calls[4][0] != "delete-buffer" {
		t.Fatalf("styled capture calls = %#v", runner.calls)
	}
	captureCall := runner.calls[1]
	if len(captureCall) != 22 ||
		!reflect.DeepEqual(captureCall[:10], []string{"capture-pane", "-e", "-N", "-S", "-27", "-E", "36", "-t", "%7", "-b"}) ||
		!strings.HasPrefix(captureCall[10], "engram-physical-") ||
		!reflect.DeepEqual(captureCall[11:21], []string{";", "capture-pane", "-J", "-S", "-27", "-E", "36", "-t", "%7", "-b"}) ||
		!strings.HasPrefix(captureCall[21], "engram-joined-") {
		t.Fatalf("capture-pane coordinates = %#v", captureCall)
	}
}

func TestCaptureLiteralUsesBoundedRowsWithoutPasteBuffers(t *testing.T) {
	runner := &sequenceRunner{outputs: []string{tmuxRecord("80", "24"), "one\ntwo\n"}}
	got, err := New(runner).CaptureLiteral(context.Background(), "%7", 64)
	if err != nil {
		t.Fatal(err)
	}
	if got != "one\ntwo" {
		t.Fatalf("CaptureLiteral = %q", got)
	}
	want := [][]string{
		{"display-message", "-p", "-t", "%7", "#{n:pane_width}:#{pane_width}#{n:pane_height}:#{pane_height}"},
		{"capture-pane", "-p", "-N", "-S", "-40", "-E", "23", "-t", "%7"},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("calls = %#v, want %#v", runner.calls, want)
	}
}

func TestCaptureLiteralRejectsOversizedPaneBeforeCapture(t *testing.T) {
	runner := &sequenceRunner{outputs: []string{tmuxRecord("401", "24")}}
	if _, err := New(runner).CaptureLiteral(context.Background(), "%7", 64); err == nil || !strings.Contains(err.Error(), "pane width") {
		t.Fatalf("CaptureLiteral error = %v", err)
	}
	if len(runner.calls) != 1 || runner.calls[0][0] != "display-message" {
		t.Fatalf("calls = %#v", runner.calls)
	}
}

func containsArgs(args, sequence []string) bool {
	for i := 0; i+len(sequence) <= len(args); i++ {
		if reflect.DeepEqual(args[i:i+len(sequence)], sequence) {
			return true
		}
	}
	return false
}

func TestSemanticCaptureCleansTerminalOutput(t *testing.T) {
	input := strings.Join([]string{
		"\n",
		"\x1b[31mred\x1b[0m   \r\n",
		"link: \x1b]8;;https://example.com\x07label\x1b]8;;\x1b\\\n",
		"charset \x1b(Bkept\x00\x07\x7f\n",
		"dcs \x1bPignored\x1b\\done\t \n",
		"c1 \u009dignored\u009cdone\n",
	}, "")
	got := semanticCapture(input)
	want := "red\nlink: label\ncharset kept\ndcs done\nc1 done"
	if got != want {
		t.Fatalf("semanticCapture = %q, want %q", got, want)
	}
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
	f := &fakeRunner{out: tmuxRecord("$1", "@2", "%3", "main", "0", "1", "1", "/home/me", "bash") +
		tmuxRecord("$1", "@4", "%5", "main", "2", "0", "0", "/tmp", "vim")}
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
	format := paneRecordFormat
	wantCalls := [][]string{{"list-panes", "-a", "-F", format}}
	if !reflect.DeepEqual(f.calls, wantCalls) {
		t.Fatalf("calls = %#v, want %#v", f.calls, wantCalls)
	}
}

func TestInspectAndValidatePaneIdentity(t *testing.T) {
	output := tmuxRecord("$1", "@2", "%3", "main", "0", "1", "1", "/home/me", "bash")
	format := paneRecordFormat

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
		{err: &IdentityError{Reason: "gone", Err: fmt.Errorf("can't find pane")}, want: true},
		{err: fmt.Errorf("wrapped: %w", &IdentityError{Reason: "mismatch"}), want: true},
		{err: fmt.Errorf("tmux display-message: exit status 1: can't find pane: %%8"), want: false},
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

func TestKillWindowChecksCompleteBindingInOneTmuxCall(t *testing.T) {
	runner := &fakeRunner{}
	err := New(runner).KillWindowIfBindingMatches(context.Background(), "%3", "@2", "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 || runner.calls[0][0] != "if-shell" || runner.calls[0][3] != "%3" ||
		!strings.Contains(runner.calls[0][4], "@engram_server_id") || runner.calls[0][5] != "kill-window -t @2" {
		t.Fatalf("close calls = %#v", runner.calls)
	}

	runner = &fakeRunner{out: identityMismatchMarker + "\n"}
	if err := New(runner).KillWindowIfBindingMatches(context.Background(), "%3", "@2", "0123456789abcdef0123456789abcdef"); err == nil || !IsIdentityLoss(err) {
		t.Fatalf("mismatch error = %v", err)
	}
}

func TestListPanesRejectsMalformedIdentity(t *testing.T) {
	f := &fakeRunner{out: tmuxRecord("$1", "main:0", "%3", "main", "0", "0", "1", "/tmp", "bash")}
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
