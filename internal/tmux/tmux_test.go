package tmux

import (
	"context"
	"reflect"
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

func TestSendCommandSendsLiteralThenEnter(t *testing.T) {
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
