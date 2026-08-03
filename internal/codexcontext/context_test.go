package codexcontext

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixtureSessionID = "123e4567-e89b-12d3-a456-426614174000"

func TestCheckedInCodexRolloutV1FixtureIsSanitizedAndComposable(t *testing.T) {
	root, err := filepath.Abs("testdata")
	if err != nil {
		t.Fatal(err)
	}
	got, err := (Reader{SessionsRoot: root}).Load(fixtureSessionID, 2)
	if err != nil {
		t.Fatal(err)
	}
	prompt := PromptText(got.Messages)
	if !strings.Contains(prompt, "Please explain the synthetic queue") || strings.Contains(prompt, "hidden system") || strings.Contains(prompt, "never-real-secret") || strings.Contains(prompt, "tool result") || strings.Contains(prompt, "environment_context") {
		t.Fatalf("checked-in fixture boundary = %q", prompt)
	}
	if _, ok := DetectDiagram(got.Messages); !ok {
		t.Fatal("checked-in fixture lost its synthetic diagram")
	}
}

func TestReaderUsesExactSessionAndVisibleMessagesOnly(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, fixtureSessionID, strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"123e4567-e89b-12d3-a456-426614174000","cwd":"/synthetic"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"system","content":[{"type":"input_text","text":"hidden system policy"}]}}`,
		`{"type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"credential=fixture-secret"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Please explain the queue."},{"type":"input_image","image_url":"fixture-attachment"}]}}`,
		`{"type":"response_item","payload":{"type":"function_call_output","output":"fixture tool result"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"The queue drains in order."}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>generated metadata</environment_context>"}]}}`,
	}, "\n")+"\n")

	got, err := (Reader{SessionsRoot: root}).Load(fixtureSessionID, 4)
	if err != nil {
		t.Fatal(err)
	}
	if got.Parser != ParserVersion || len(got.Messages) != 2 {
		t.Fatalf("context = %#v", got)
	}
	prompt := PromptText(got.Messages)
	for _, want := range []string{"User:\nPlease explain the queue.", "Assistant:\nThe queue drains in order."} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q: %q", want, prompt)
		}
	}
	for _, forbidden := range []string{"hidden system", "fixture-secret", "tool result", "fixture-attachment", "environment_context"} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("prompt exposed %q: %q", forbidden, prompt)
		}
	}
}

func TestReaderFailsClosedOnAmbiguousOrMismatchedRollout(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, fixtureSessionID, `{"type":"session_meta","payload":{"id":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"}}`+"\n")
	if _, err := (Reader{SessionsRoot: root}).Load(fixtureSessionID, 4); err == nil {
		t.Fatal("mismatched session metadata was accepted")
	}
	other := filepath.Join(root, "duplicate")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "rollout-copy-"+fixtureSessionID+".jsonl"), []byte(`{"type":"session_meta","payload":{"id":"`+fixtureSessionID+`"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (Reader{SessionsRoot: root}).Load(fixtureSessionID, 4); err == nil {
		t.Fatal("ambiguous exact session was accepted")
	}
}

func TestReaderBoundsRecentMessagesBeforeReturn(t *testing.T) {
	root := t.TempDir()
	lines := []string{`{"type":"session_meta","payload":{"id":"` + fixtureSessionID + `"}}`}
	for index := 0; index < 12; index++ {
		lines = append(lines, `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"message `+string(rune('a'+index))+`"}]}}`)
	}
	writeFixture(t, root, fixtureSessionID, strings.Join(lines, "\n")+"\n")
	got, err := (Reader{SessionsRoot: root}).Load(fixtureSessionID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 3 || got.Messages[0].Text != "message j" || got.Messages[2].Text != "message l" || len(PromptText(got.Messages)) > MaxContextBytes {
		t.Fatalf("bounded messages = %#v", got.Messages)
	}
}

func TestReaderTurnLimitKeepsUserTurnAndFollowingAssistantMessages(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, fixtureSessionID, strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"` + fixtureSessionID + `"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"older question"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"older answer"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"current question"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"working note"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"current answer"}]}}`,
	}, "\n")+"\n")
	got, err := (Reader{SessionsRoot: root}).Load(fixtureSessionID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 3 || got.Messages[0].Text != "current question" || got.Messages[2].Text != "current answer" {
		t.Fatalf("one recent turn = %#v", got.Messages)
	}
}

func TestReaderUsesBoundedTailForLongLivedRollout(t *testing.T) {
	root := t.TempDir()
	path := writeFixture(t, root, fixtureSessionID, strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"` + fixtureSessionID + `"}}`,
		strings.TrimSuffix(strings.Repeat(`{"type":"event_msg","payload":{"type":"token_count","text":"synthetic padding"}}`+"\n", 160), "\n"),
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"recent question"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"recent answer"}]}}`,
	}, "\n")+"\n")

	got, err := parseRolloutWithBudget(path, fixtureSessionID, 1, 8<<10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 2 || got.Messages[0].Text != "recent question" || got.Messages[1].Text != "recent answer" {
		t.Fatalf("bounded tail messages = %#v", got.Messages)
	}
}

func TestReaderDoesNotTrustSessionMetadataFoundOnlyInTail(t *testing.T) {
	root := t.TempDir()
	path := writeFixture(t, root, fixtureSessionID, strings.Repeat(
		`{"type":"event_msg","payload":{"type":"token_count","text":"synthetic padding"}}`+"\n", 160)+
		`{"type":"session_meta","payload":{"id":"`+fixtureSessionID+`"}}`+"\n"+
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"must not be admitted"}]}}`+"\n")

	if _, err := parseRolloutWithBudget(path, fixtureSessionID, 1, 8<<10); err == nil {
		t.Fatalf("tail-only identity error = %v", err)
	}
}

func TestDetectDiagramConservativelyRecognizesBoxesAndFlows(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{name: "unicode box", text: "Architecture:\n┌──────┐   ┌──────┐\n│ pane │ → │ bot  │\n└──────┘   └──────┘", want: true},
		{name: "ascii flow", text: "source -----> parse\n       |        |\n       +-----> send", want: true},
		{name: "source code", text: "```go\nif err != nil {\n    return err\n}\n```", want: false},
		{name: "ordinary prose", text: "This is a paragraph.\nIt has several lines.\nNothing here is a diagram.", want: false},
		{name: "single arrow", text: "Use A -> B.\nThen verify B.\nThat is all.", want: false},
		{name: "too wide", text: "+" + strings.Repeat("-", MaxDiagramColumns) + "+\n| wide |\n+---+", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, got := DetectDiagram([]Message{{Role: RoleAssistant, Text: test.text}})
			if got != test.want {
				t.Fatalf("DetectDiagram() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestDetectDiagramCountsUnicodeDisplayWidth(t *testing.T) {
	text := "┌──┐\n│界│\n└──┘"
	diagram, ok := DetectDiagram([]Message{{Role: RoleAssistant, Text: text}})
	if !ok || diagram.Text != text {
		t.Fatalf("diagram = %#v, %v", diagram, ok)
	}
}

func writeFixture(t *testing.T, root, sessionID, contents string) string {
	t.Helper()
	dir := filepath.Join(root, "2026", "08", "03")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-08-03T00-00-00-"+sessionID+".jsonl")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
