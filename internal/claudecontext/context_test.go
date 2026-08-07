package claudecontext

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/idolum-ai/engram/internal/codexcontext"
)

const testSessionID = "019f7607-c8b0-74b3-87ca-64a7e6e7ede0"

func TestReaderAdmitsOnlyHumanAndVisibleAssistantText(t *testing.T) {
	path := writeTranscript(t, []string{
		`{"type":"mode","sessionId":"` + testSessionID + `","mode":"default"}`,
		`{"type":"user","sessionId":"` + testSessionID + `","uuid":"u1","message":{"role":"user","content":"first prompt"}}`,
		`{"type":"assistant","sessionId":"` + testSessionID + `","uuid":"a1","message":{"id":"m1","role":"assistant","content":[{"type":"thinking","thinking":"hidden"},{"type":"tool_use","name":"Bash","input":{"command":"secret"}},{"type":"text","text":"visible answer"}]}}`,
		`{"type":"user","sessionId":"` + testSessionID + `","uuid":"tool","message":{"role":"user","content":[{"type":"tool_result","content":"tool secret"}]}}`,
		`{"type":"user","sessionId":"` + testSessionID + `","uuid":"meta","isMeta":true,"message":{"role":"user","content":"generated metadata"}}`,
		`{"type":"assistant","sessionId":"` + testSessionID + `","uuid":"side","isSidechain":true,"message":{"id":"side","role":"assistant","content":[{"type":"text","text":"subagent secret"}]}}`,
		`{"type":"assistant","sessionId":"` + testSessionID + `","uuid":"forwarded","agentId":"agent-1","message":{"id":"forwarded","role":"assistant","parent_tool_use_id":"tool-1","content":[{"type":"text","text":"forwarded subagent secret"}]}}`,
		`{"type":"user","sessionId":"` + testSessionID + `","uuid":"u2","message":{"role":"user","content":"second prompt"}}`,
		`{"type":"assistant","sessionId":"` + testSessionID + `","uuid":"a2","message":{"id":"m2","role":"assistant","content":[{"type":"text","text":"second answer"}]}}`,
	})
	got, err := (Reader{}).Load(path, testSessionID, 2, func(value string) string {
		return strings.ReplaceAll(value, "second", "[redacted]")
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 4 || got.Parser != ParserVersion {
		t.Fatalf("context = %#v", got)
	}
	joined := codexcontext.PromptText(got.Messages)
	for _, absent := range []string{"hidden", "secret", "generated metadata", "subagent"} {
		if strings.Contains(joined, absent) {
			t.Fatalf("context leaked %q: %s", absent, joined)
		}
	}
	for _, present := range []string{"first prompt", "visible answer", "[redacted] prompt", "[redacted] answer"} {
		if !strings.Contains(joined, present) {
			t.Fatalf("context missing %q: %s", present, joined)
		}
	}
	if !got.Messages[2].Redacted || !got.Messages[3].Redacted {
		t.Fatalf("redaction flags = %#v", got.Messages)
	}
}

func TestReaderDeduplicatesSplitAssistantMessages(t *testing.T) {
	path := writeTranscript(t, []string{
		`{"type":"user","sessionId":"` + testSessionID + `","uuid":"u1","message":{"role":"user","content":"prompt"}}`,
		`{"type":"assistant","sessionId":"` + testSessionID + `","uuid":"a1","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"part one"}]}}`,
		`{"type":"assistant","sessionId":"` + testSessionID + `","uuid":"a2","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"part one"},{"type":"text","text":"part two"}]}}`,
	})
	got, err := (Reader{}).Load(path, testSessionID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 2 || got.Messages[1].Text != "part one\npart two" {
		t.Fatalf("messages = %#v", got.Messages)
	}
}

func TestReaderFailsClosedOnIdentityAndRecognizedSchemaChanges(t *testing.T) {
	unknown := writeTranscript(t, []string{
		`{"type":"user","sessionId":"` + testSessionID + `","message":{"role":"user","content":"prompt"}}`,
		`{"type":"assistant","sessionId":"` + testSessionID + `","uuid":"a1","message":{"id":"m1","role":"assistant","content":[{"type":"future_block","text":"unsafe"}]}}`,
	})
	if _, err := (Reader{}).Load(unknown, testSessionID, 1); err == nil {
		t.Fatal("unknown assistant block was accepted")
	}

	mismatch := writeTranscript(t, []string{`{"type":"user","sessionId":"019f7607-c8b0-74b3-87ca-64a7e6e7ede1","message":{"role":"user","content":"wrong"}}`})
	if _, err := (Reader{}).Load(mismatch, testSessionID, 1); err == nil {
		t.Fatal("mismatched transcript identity was accepted")
	}

	wrongName := filepath.Join(t.TempDir(), "other.jsonl")
	if err := os.WriteFile(wrongName, []byte(`{"sessionId":"`+testSessionID+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (Reader{}).Load(wrongName, testSessionID, 1); err == nil {
		t.Fatal("mismatched transcript filename was accepted")
	}
}

func TestReaderRejectsSymlinkAndOversizedRecord(t *testing.T) {
	target := writeTranscript(t, []string{`{"type":"user","sessionId":"` + testSessionID + `","message":{"role":"user","content":"prompt"}}`})
	symlink := filepath.Join(t.TempDir(), testSessionID+".jsonl")
	if err := os.Symlink(target, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := (Reader{}).Load(symlink, testSessionID, 1); err == nil {
		t.Fatal("symlink transcript was accepted")
	}

	oversized := writeTranscript(t, []string{
		`{"type":"user","sessionId":"` + testSessionID + `","message":{"role":"user","content":"prompt"}}`,
		`{"padding":"` + strings.Repeat("x", maxJSONLineBytes) + `"}`,
	})
	if _, err := (Reader{}).Load(oversized, testSessionID, 1); err == nil {
		t.Fatal("oversized transcript record was accepted")
	}
}

func TestReaderUsesBoundedTailEndingAtOpenedSize(t *testing.T) {
	path := writeTranscript(t, []string{
		`{"type":"user","sessionId":"` + testSessionID + `","uuid":"old","message":{"role":"user","content":"old prompt"}}`,
		`{"padding":"` + strings.Repeat("x", 6000) + `"}`,
		`{"type":"user","sessionId":"` + testSessionID + `","uuid":"recent","message":{"role":"user","content":"recent prompt"}}`,
		`{"type":"assistant","sessionId":"` + testSessionID + `","uuid":"answer","message":{"id":"message","role":"assistant","content":[{"type":"text","text":"recent answer"}]}}`,
	})
	got, err := parseTranscriptWithBudget(path, testSessionID, 1, 4096, func(value string) string {
		file, openErr := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if openErr != nil {
			t.Fatal(openErr)
		}
		_, writeErr := file.WriteString(`{"type":"user","sessionId":"wrong-session","message":{"role":"user","content":"appended later"}}` + "\n")
		if closeErr := file.Close(); writeErr != nil || closeErr != nil {
			t.Fatalf("append errors: write=%v close=%v", writeErr, closeErr)
		}
		return value
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := codexcontext.PromptText(got.Messages)
	if !strings.Contains(joined, "recent prompt") || !strings.Contains(joined, "recent answer") || strings.Contains(joined, "old prompt") || strings.Contains(joined, "appended later") {
		t.Fatalf("bounded tail context = %q", joined)
	}
}

func writeTranscript(t *testing.T, records []string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), testSessionID+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(records, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
