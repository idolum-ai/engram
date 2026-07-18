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

	"github.com/idolum-ai/engram/internal/recovery"
)

func tmuxRecord(values ...string) string {
	var out strings.Builder
	for _, value := range values {
		fmt.Fprintf(&out, "%d:%s", len(value), value)
	}
	return out.String() + "\n"
}

func TestTmuxCommandArgumentsForceUTF8WithoutChangingInput(t *testing.T) {
	t.Parallel()
	original := []string{"display-message", "-p", "hello"}
	want := []string{"-u", "display-message", "-p", "hello"}
	if got := tmuxCommandArguments(original); !reflect.DeepEqual(got, want) {
		t.Fatalf("tmux arguments = %#v, want %#v", got, want)
	}
	if !reflect.DeepEqual(original, []string{"display-message", "-p", "hello"}) {
		t.Fatalf("tmux arguments changed caller input: %#v", original)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
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

type identityMismatchRunner struct{}

func (identityMismatchRunner) Run(_ context.Context, args ...string) (string, error) {
	return strings.TrimPrefix(args[len(args)-1], "display-message -p ") + "\n", nil
}

func (identityMismatchRunner) RunToWriter(_ context.Context, dst io.Writer, args ...string) error {
	_, err := io.WriteString(dst, strings.TrimPrefix(args[len(args)-1], "display-message -p ")+"\n")
	return err
}

type styledCaptureRunner struct {
	calls     [][]string
	ansi      string
	joined    string
	metadata  string
	afterMeta string
}

const styledCaptureServerID = "0123456789abcdef0123456789abcdef"

type sequenceRunner struct {
	calls   [][]string
	outputs []string
}

type ensureRaceRunner struct {
	calls     [][]string
	listCalls int
}

type missingServerRunner struct{ calls [][]string }

type windowResizeFailureRunner struct{ calls [][]string }

func (r *windowResizeFailureRunner) Run(_ context.Context, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	switch args[0] {
	case "show-options":
		return "80x24\n", nil
	case "new-window":
		return tmuxRecord("@9", "%9"), nil
	case "resize-window":
		return "", errors.New("resize failed")
	case "kill-window":
		return "", nil
	default:
		return "", fmt.Errorf("unexpected tmux call: %v", args)
	}
}

func (r *missingServerRunner) Run(_ context.Context, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	if len(r.calls) == 1 {
		return "", &commandError{args: args, err: errors.New("exit status 1"), stderr: "no server running on /tmp/tmux/default"}
	}
	if args[0] == "new-session" {
		return tmuxRecord("$4", "main"), nil
	}
	return "", fmt.Errorf("unexpected tmux call: %v", args)
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
		if r.metadata != "" {
			return r.metadata, nil
		}
		return styledCaptureMetadata("bash", "1", "0"), nil
	}
	if len(args) > 0 && args[0] == "capture-pane" {
		if r.afterMeta != "" {
			return r.afterMeta, nil
		}
		if r.metadata != "" {
			return r.metadata, nil
		}
		return styledCaptureMetadata("bash", "1", "0"), nil
	}
	if len(args) > 0 && args[0] == "show-buffer" {
		if strings.Contains(args[len(args)-1], "physical") {
			return r.ansi, nil
		}
		return r.joined, nil
	}
	return "", nil
}

func styledCaptureMetadata(command, alternateOn, paneInMode string) string {
	return styledCaptureMetadataValues(styledCaptureServerID, "@2", "%7", "71", "37", command, alternateOn, paneInMode)
}

func styledCaptureMetadataValues(serverID, windowID, paneID, columns, rows, command, alternateOn, paneInMode string) string {
	return tmuxRecord(serverID, windowID, paneID, columns, rows, "build", "/home/me", command, alternateOn, paneInMode)
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

func TestSendTextIfBindingMatchesUsesOneBracketedPaste(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{}
	text := strings.Repeat("long line ", 300) + "\nsecond paragraph\nthird paragraph"
	err := New(runner).SendTextIfBindingMatches(context.Background(), "%1", "@2", styledCaptureServerID, text)
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("calls = %#v, want set-buffer and one guarded paste", runner.calls)
	}
	if got := runner.calls[0]; len(got) != 5 || got[0] != "set-buffer" || got[4] != text {
		t.Fatalf("set-buffer call = %#v", got)
	}
	if got := runner.calls[1]; got[0] != "if-shell" || !strings.Contains(got[5], "paste-buffer -p -r -d") {
		t.Fatalf("guarded paste call = %#v", got)
	}
}

func TestEngramAdvertisementUsesPaneOptionsBehindBindingGuard(t *testing.T) {
	t.Parallel()
	runner := &fakeRunner{}
	manager := New(runner)
	if err := manager.AdvertiseEngramIfBindingMatches(context.Background(), "%7", "@2", styledCaptureServerID, 42); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls = %#v, want one guarded pane option transaction", runner.calls)
	}
	wantCommands := []string{
		"set-option -p -q -u -t %7 @engram",
		"set-option -p -q -t %7 @engram_watch_id '42'",
		"set-option -p -q -t %7 @engram_notify 'run: engram signal --stdout MESSAGE (tool output) or engram signal MESSAGE (interactive TTY)'",
		"set-option -p -q -t %7 @engram_artifact 'print a visible file:// URI (OSC 8 optional), then run @engram_notify'",
		"set-option -p -q -t %7 @engram 'v1 watch=42 remote=telegram'",
	}
	call := runner.calls[0]
	if len(call) != 7 || call[0] != "if-shell" || call[3] != "%7" {
		t.Fatalf("guarded option call = %#v", call)
	}
	for _, command := range wantCommands {
		if !strings.Contains(call[5], command) {
			t.Fatalf("guarded option transaction = %q, missing %q", call[5], command)
		}
	}
	last := -1
	for _, command := range wantCommands {
		index := strings.Index(call[5], command)
		if index <= last {
			t.Fatalf("guarded option transaction order = %q, %q was not after its predecessor", call[5], command)
		}
		last = index
	}

	runner.calls = nil
	if err := manager.ClearEngramAdvertisementIfBindingMatches(context.Background(), "%7", "@2", styledCaptureServerID); err != nil {
		t.Fatal(err)
	}
	wantCommands = []string{
		"set-option -p -q -u -t %7 @engram",
		"set-option -p -q -u -t %7 @engram_watch_id",
		"set-option -p -q -u -t %7 @engram_notify",
		"set-option -p -q -u -t %7 @engram_artifact",
	}
	if len(runner.calls) != 1 || len(runner.calls[0]) != 7 {
		t.Fatalf("guarded option clear calls = %#v, want one transaction", runner.calls)
	}
	for _, command := range wantCommands {
		if !strings.Contains(runner.calls[0][5], command) {
			t.Fatalf("guarded option clear transaction = %q, missing %q", runner.calls[0][5], command)
		}
	}
}

func TestExtractOSC8HyperlinksAcceptsTerminalTerminatorsAndDeduplicates(t *testing.T) {
	t.Parallel()
	input := strings.Join([]string{
		"\x1b]8;;file:///tmp/report%20one.txt\x1b\\report\x1b]8;;\x1b\\",
		"\x1b]8;id=2;https://example.test/build\aweb\x1b]8;;\a",
		"\x1b]8;;file:///tmp/М-report.txt\x1b\\unicode\x1b]8;;\x1b\\",
		"\x1b]8;;file:///tmp/report%20one.txt\x1b\\duplicate\x1b]8;;\x1b\\",
		"\x1b]8;;file:///tmp/bad\npath\x1b\\bad",
		"\x1b]8;;unterminated",
	}, " ")
	want := []string{"file:///tmp/report%20one.txt", "https://example.test/build", "file:///tmp/М-report.txt"}
	if got := extractOSC8Hyperlinks(input, 8); !reflect.DeepEqual(got, want) {
		t.Fatalf("hyperlinks = %#v, want %#v", got, want)
	}
	if got := extractOSC8Hyperlinks(input, 1); !reflect.DeepEqual(got, want[:1]) {
		t.Fatalf("bounded hyperlinks = %#v, want %#v", got, want[:1])
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
		runner := &sequenceRunner{outputs: []string{"80x24\n", tmuxRecord("@9", "%9"), ""}}
		windowID, paneID, err := New(runner).NewWindow(context.Background(), "$4", "/tmp", "probe")
		if err != nil {
			t.Fatal(err)
		}
		if windowID != "@9" || paneID != "%9" {
			t.Fatalf("window=%q pane=%q", windowID, paneID)
		}
		want := [][]string{
			{"show-options", "-gv", "default-size"},
			{"new-window", "-P", "-F", "#{n:window_id}:#{window_id}#{n:pane_id}:#{pane_id}", "-n", "probe", "-c", "/tmp", "-t", "$4:"},
			{"resize-window", "-x", "80", "-y", "24", "-t", "@9"},
		}
		if !reflect.DeepEqual(runner.calls, want) {
			t.Fatalf("calls = %#v, want %#v", runner.calls, want)
		}
	})

	t.Run("invalid default size fails before creation", func(t *testing.T) {
		runner := &sequenceRunner{outputs: []string{"80 by 24\n"}}
		if _, _, err := New(runner).NewWindow(context.Background(), "$4", "/tmp", "probe"); err == nil || !strings.Contains(err.Error(), "invalid tmux default-size") {
			t.Fatalf("NewWindow error = %v", err)
		}
		if len(runner.calls) != 1 || runner.calls[0][0] != "show-options" {
			t.Fatalf("invalid size caused a tmux mutation: %#v", runner.calls)
		}
	})

	t.Run("resize failure removes created window", func(t *testing.T) {
		runner := &windowResizeFailureRunner{}
		if _, _, err := New(runner).NewWindow(context.Background(), "$4", "/tmp", "probe"); err == nil || !strings.Contains(err.Error(), "size new tmux window") {
			t.Fatalf("NewWindow error = %v", err)
		}
		if len(runner.calls) != 4 || !reflect.DeepEqual(runner.calls[3], []string{"kill-window", "-t", "@9"}) {
			t.Fatalf("resize failure cleanup calls = %#v", runner.calls)
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

	t.Run("malformed session list fails before creation", func(t *testing.T) {
		f := &fakeRunner{out: tmuxRecord("no server running", "bad-id", "1", "0")}
		if _, err := New(f).EnsureSession(context.Background(), "main", "/tmp"); err == nil {
			t.Fatal("EnsureSession accepted malformed session metadata")
		}
		if len(f.calls) != 1 || f.calls[0][0] != "list-sessions" {
			t.Fatalf("malformed metadata caused a tmux mutation: %#v", f.calls)
		}
	})

	t.Run("missing server execution error permits creation", func(t *testing.T) {
		runner := &missingServerRunner{}
		id, err := New(runner).EnsureSession(context.Background(), "main", "/tmp")
		if err != nil || id != "$4" {
			t.Fatalf("EnsureSession ID=%q error=%v", id, err)
		}
		if len(runner.calls) != 2 || runner.calls[1][0] != "new-session" {
			t.Fatalf("calls = %#v", runner.calls)
		}
	})
}

func TestMissingTmuxServerRecognizesSupportedDiagnostics(t *testing.T) {
	for _, stderr := range []string{
		"no server running on /tmp/tmux/default",
		"error connecting to /tmp/tmux-1000/default (No such file or directory)",
	} {
		err := &commandError{args: []string{"list-sessions"}, err: errors.New("exit status 1"), stderr: stderr}
		if !missingTmuxServer(err) {
			t.Fatalf("missing server diagnostic not recognized: %q", stderr)
		}
	}
	for _, stderr := range []string{
		"error connecting to /tmp/tmux-1000/default (Permission denied)",
		"no such file or directory",
	} {
		err := &commandError{args: []string{"list-sessions"}, err: errors.New("exit status 1"), stderr: stderr}
		if missingTmuxServer(err) {
			t.Fatalf("unrelated diagnostic classified as missing server: %q", stderr)
		}
	}
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
		out := tmuxRecord("$7", "main:one_\t雪", "12", "@8", "", "1", "%9", "/tmp/a:b_\t雪\nline", "")
		windows, err := parseWindows(out)
		if err != nil {
			t.Fatal(err)
		}
		if len(windows) != 1 || windows[0].SessionName != "main:one_\t雪" || windows[0].Name != "" || windows[0].CurrentPath != "/tmp/a:b_\t雪\nline" || windows[0].CurrentCmd != "" {
			t.Fatalf("windows = %#v", windows)
		}
	})

	for name, output := range map[string]string{
		"legacy delimiters":  "$7_main_0_@8_build_1_%9_/tmp_bash\n",
		"missing field":      tmuxRecord("$7", "main", "0", "@8", "build", "1", "%9", "/tmp"),
		"short value":        "5:$7",
		"missing terminator": strings.TrimSuffix(tmuxRecord("$7", "main", "0", "@8", "build", "1", "%9", "/tmp", "bash"), "\n"),
		"trailing bytes":     tmuxRecord("$7", "main", "0", "@8", "build", "1", "%9", "/tmp", "bash") + "x",
		"blank record":       tmuxRecord("$7", "main", "0", "@8", "build", "1", "%9", "/tmp", "bash") + "\n",
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

	got, err := New(f).CaptureVisibleRaw(context.Background(), "%7", "@2", styledCaptureServerID)
	if err != nil {
		t.Fatal(err)
	}
	if got != wantOutput {
		t.Fatalf("CaptureVisibleRaw = %q, want %q", got, wantOutput)
	}
	if len(f.calls) != 1 || f.calls[0][0] != "if-shell" || f.calls[0][3] != "%7" || f.calls[0][5] != "capture-pane -p -e -N -t %7" {
		t.Fatalf("calls = %#v", f.calls)
	}
}

func TestCaptureVisibleRawRejectsBindingMismatchWithoutReturningMarker(t *testing.T) {
	if _, err := New(identityMismatchRunner{}).CaptureVisibleRaw(context.Background(), "%7", "@2", styledCaptureServerID); err == nil || !IsIdentityLoss(err) {
		t.Fatalf("CaptureVisibleRaw mismatch error = %v", err)
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
	if got.ServerID != styledCaptureServerID || got.WindowID != "@2" || got.PaneID != "%7" || got.CurrentCmd != "bash" || got.AlternateOn != "1" || got.PaneInMode != "0" || got.Columns != 71 || got.VisibleRows != 37 || got.BufferRows != 64 || got.Title != "build" || got.CurrentPath != "/home/me" {
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
	if len(captureCall) != 28 ||
		!reflect.DeepEqual(captureCall[:10], []string{"capture-pane", "-e", "-N", "-S", "-27", "-E", "36", "-t", "%7", "-b"}) ||
		!strings.HasPrefix(captureCall[10], "engram-physical-") ||
		!reflect.DeepEqual(captureCall[11:21], []string{";", "capture-pane", "-J", "-S", "-27", "-E", "36", "-t", "%7", "-b"}) ||
		!strings.HasPrefix(captureCall[21], "engram-joined-") ||
		!reflect.DeepEqual(captureCall[22:27], []string{";", "display-message", "-p", "-t", "%7"}) ||
		!strings.Contains(captureCall[27], "pane_current_command") {
		t.Fatalf("capture-pane coordinates = %#v", captureCall)
	}
}

func TestCaptureStyledKeepsPlainAndStyledPhysicalRowsAligned(t *testing.T) {
	t.Parallel()
	ansi := "\n\x1b[31museful terminal result\x1b[0m\n\n\n"
	runner := &styledCaptureRunner{ansi: ansi, joined: "useful terminal result\n"}

	got, err := New(runner).CaptureStyled(context.Background(), "%7", 64)
	if err != nil {
		t.Fatal(err)
	}
	plainRows := strings.Split(strings.TrimSuffix(got.Text, "\n"), "\n")
	styledRows := strings.Split(strings.TrimSuffix(got.ANSI, "\n"), "\n")
	if len(plainRows) != len(styledRows) || len(plainRows) != 4 {
		t.Fatalf("physical rows plain=%d styled=%d; text=%q", len(plainRows), len(styledRows), got.Text)
	}
	if plainRows[0] != "" || plainRows[1] != "useful terminal result" || plainRows[2] != "" || plainRows[3] != "" {
		t.Fatalf("plain physical rows = %#v", plainRows)
	}
	if got.JoinedText != "useful terminal result" {
		t.Fatalf("joined text = %q", got.JoinedText)
	}
}

func TestCaptureStyledRejectsBoundaryChange(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		metadata     string
		identityLoss bool
	}{
		"server":     {styledCaptureMetadataValues("abcdef0123456789abcdef0123456789", "@2", "%7", "71", "37", "bash", "1", "0"), true},
		"window":     {styledCaptureMetadataValues(styledCaptureServerID, "@9", "%7", "71", "37", "bash", "1", "0"), true},
		"pane":       {styledCaptureMetadataValues(styledCaptureServerID, "@2", "%9", "71", "37", "bash", "1", "0"), true},
		"dimensions": {styledCaptureMetadataValues(styledCaptureServerID, "@2", "%7", "72", "37", "bash", "1", "0"), false},
		"command":    {styledCaptureMetadata("vim", "1", "0"), false},
		"alternate":  {styledCaptureMetadata("bash", "0", "0"), false},
		"copy mode":  {styledCaptureMetadata("bash", "1", "1"), false},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			runner := &styledCaptureRunner{afterMeta: test.metadata}
			_, err := New(runner).CaptureStyled(context.Background(), "%7", 64)
			if err == nil || !strings.Contains(err.Error(), "changed while capturing") || IsIdentityLoss(err) != test.identityLoss {
				t.Fatalf("CaptureStyled error = %v", err)
			}
			for _, call := range runner.calls {
				if len(call) > 0 && call[0] == "show-buffer" {
					t.Fatalf("unstable capture read a buffer: %#v", runner.calls)
				}
			}
		})
	}
}

func TestCaptureLiteralUsesBoundedRowsWithoutPasteBuffers(t *testing.T) {
	runner := &sequenceRunner{outputs: []string{tmuxRecord("80", "24"), "one\ntwo\n"}}
	got, err := New(runner).CaptureLiteral(context.Background(), "%7", "@2", styledCaptureServerID, 64)
	if err != nil {
		t.Fatal(err)
	}
	if got != "one\ntwo" {
		t.Fatalf("CaptureLiteral = %q", got)
	}
	if len(runner.calls) != 2 || runner.calls[0][0] != "display-message" || runner.calls[1][0] != "if-shell" || runner.calls[1][5] != "capture-pane -p -N -S -40 -E 23 -t %7" {
		t.Fatalf("calls = %#v", runner.calls)
	}
}

func TestCaptureLiteralKeepsCurrentTailForTallPane(t *testing.T) {
	fullVisiblePane := strings.Repeat("old output\n", 36) + "codex\nprompt\n"
	runner := &sequenceRunner{outputs: []string{tmuxRecord("80", "40"), fullVisiblePane}}
	got, err := New(runner).CaptureLiteral(context.Background(), "%7", "@2", styledCaptureServerID, 64)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "codex\nprompt") {
		t.Fatalf("CaptureLiteral = %q", got)
	}
	if len(runner.calls) != 2 || runner.calls[0][0] != "display-message" || runner.calls[1][0] != "if-shell" || runner.calls[1][5] != "capture-pane -p -N -S -24 -E 39 -t %7" {
		t.Fatalf("calls = %#v", runner.calls)
	}
}

func TestCaptureLiteralTallPaneUsesCurrentTailWithoutContentProbe(t *testing.T) {
	t.Parallel()
	runner := &sequenceRunner{outputs: []string{
		tmuxRecord("289", "162"),
		"current prompt\nfailure footer\n",
	}}
	got, err := New(runner).CaptureLiteral(context.Background(), "%7", "@2", styledCaptureServerID, 64)
	if err != nil {
		t.Fatal(err)
	}
	if got != "current prompt\nfailure footer" {
		t.Fatalf("CaptureLiteral = %q", got)
	}
	if len(runner.calls) != 2 || runner.calls[1][0] != "if-shell" || runner.calls[1][5] != "capture-pane -p -N -S 98 -E 161 -t %7" {
		t.Fatalf("current-tail literal capture calls = %#v", runner.calls)
	}
}

func TestCaptureStyledUsesOnePhysicalWindowForANSIAndJoinedText(t *testing.T) {
	ansi := strings.Repeat("\x1b[32mselected physical row\x1b[0m\n", 64)
	joined := strings.Repeat("selected logical line\n", 32)
	metadata := styledCaptureMetadataValues(styledCaptureServerID, "@2", "%7", "80", "100", "bash", "1", "0")
	runner := &styledCaptureRunner{ansi: ansi, joined: joined, metadata: metadata}

	got, err := New(runner).CaptureStyled(context.Background(), "%7", 64)
	if err != nil {
		t.Fatal(err)
	}
	if got.ANSI != ansi || got.JoinedText != strings.TrimSpace(joined) {
		t.Fatalf("misaligned styled capture: ANSI=%q joined=%q", got.ANSI, got.JoinedText)
	}
	if len(runner.calls) != 5 {
		t.Fatalf("calls = %#v", runner.calls)
	}
	finalCapture := runner.calls[1]
	physicalBounds := []string{"-S", "36", "-E", "99"}
	joinedBounds := []string{"-S", "36", "-E", "99"}
	if !containsArgs(finalCapture[:11], physicalBounds) || !containsArgs(finalCapture[11:22], joinedBounds) {
		t.Fatalf("physical and joined captures used different bounds: %#v", finalCapture)
	}
}

func TestCaptureLiteralRejectsOversizedPaneBeforeCapture(t *testing.T) {
	runner := &sequenceRunner{outputs: []string{tmuxRecord("401", "24")}}
	if _, err := New(runner).CaptureLiteral(context.Background(), "%7", "@2", styledCaptureServerID, 64); err == nil || !strings.Contains(err.Error(), "pane width") {
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

func TestPublishRecoveryMetadataUsesPaneLocalOption(t *testing.T) {
	runner := &fakeRunner{}
	metadata := recovery.Metadata{Program: recovery.ProgramCodex, SessionID: "019f7607-c8b0-74b3-87ca-64a7e6e7ede0"}
	if err := New(runner).PublishRecoveryMetadata(context.Background(), "%7", metadata); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls = %#v", runner.calls)
	}
	call := runner.calls[0]
	if len(call) != 7 || !reflect.DeepEqual(call[:6], []string{"set-option", "-p", "-q", "-t", "%7", EngramRecoveryOption}) || !strings.Contains(call[6], metadata.SessionID) {
		t.Fatalf("calls = %#v", runner.calls)
	}
}

func TestRecoveryMetadataValidatesBindingAroundRead(t *testing.T) {
	metadata := recovery.Metadata{Program: recovery.ProgramCodex, SessionID: "019f7607-c8b0-74b3-87ca-64a7e6e7ede0"}
	encoded, err := recovery.Encode(metadata)
	if err != nil {
		t.Fatal(err)
	}
	binding := tmuxRecord(styledCaptureServerID, "$1", "@2", "%7", "main", "0", "0", "1", "/work", "codex")
	runner := &sequenceRunner{outputs: []string{binding, encoded + "\n", binding}}
	got, err := New(runner).RecoveryMetadata(context.Background(), "%7", "@2", styledCaptureServerID)
	if err != nil || got.SessionID != metadata.SessionID {
		t.Fatalf("metadata = %#v, err = %v", got, err)
	}
	if len(runner.calls) != 3 || runner.calls[1][0] != "show-options" {
		t.Fatalf("calls = %#v", runner.calls)
	}
}

func TestDumpScrollbackStreamsPlainLogicalHistory(t *testing.T) {
	f := &fakeStreamRunner{chunks: []string{"chunk one", " continued\nchunk two   \n"}}
	var dst bytes.Buffer
	if err := New(f).DumpScrollback(context.Background(), "%10", "@2", styledCaptureServerID, &dst); err != nil {
		t.Fatal(err)
	}
	if want := "chunk one continued\nchunk two   \n"; dst.String() != want {
		t.Fatalf("dump = %q, want %q", dst.String(), want)
	}
	if len(f.calls) != 1 || f.calls[0][0] != "if-shell" || f.calls[0][3] != "%10" || f.calls[0][5] != "capture-pane -p -J -N -S - -E - -t %10" {
		t.Fatalf("calls = %#v", f.calls)
	}
}

func TestDumpScrollbackRejectsBindingMismatchWithoutWritingMarker(t *testing.T) {
	var dst bytes.Buffer
	err := New(identityMismatchRunner{}).DumpScrollback(context.Background(), "%10", "@2", styledCaptureServerID, &dst)
	if err == nil || !IsIdentityLoss(err) || dst.Len() != 0 {
		t.Fatalf("DumpScrollback mismatch error=%v output=%q", err, dst.String())
	}
}

func TestDumpScrollbackRequiresStreamingRunner(t *testing.T) {
	err := New(&fakeRunner{}).DumpScrollback(context.Background(), "%10", "@2", styledCaptureServerID, io.Discard)
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
