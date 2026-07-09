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
		out: "main\t$1\nother\t$2\n",
	}
	m := New(f)
	got, err := m.ListSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Session{
		{Name: "main", ID: "$1"},
		{Name: "other", ID: "$2"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListSessions = %#v, want %#v", got, want)
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
