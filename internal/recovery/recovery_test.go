package recovery

import (
	"strings"
	"testing"
	"time"
)

func TestParseCodexSessionStart(t *testing.T) {
	now := time.Date(2026, 7, 18, 21, 0, 0, 0, time.UTC)
	metadata, err := ParseCodexSessionStart(strings.NewReader(`{
  "session_id":"019f7607-c8b0-74b3-87ca-64a7e6e7ede0",
  "cwd":"/work/gleipnir ",
  "hook_event_name":"SessionStart",
  "source":"resume"
}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Program != ProgramCodex || metadata.Source != "resume" || metadata.CWD != "/work/gleipnir " || !metadata.Observed.Equal(now) {
		t.Fatalf("metadata = %#v", metadata)
	}
	encoded, err := Encode(metadata)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(encoded)
	if err != nil || decoded.SessionID != metadata.SessionID || decoded.CWD != metadata.CWD {
		t.Fatalf("decoded = %#v err=%v", decoded, err)
	}
}

func TestParseCodexSessionStartRejectsOtherEventsAndInvalidIDs(t *testing.T) {
	for _, input := range []string{
		`{"session_id":"019f7607-c8b0-74b3-87ca-64a7e6e7ede0","hook_event_name":"Stop"}`,
		`{"session_id":"not-a-uuid","hook_event_name":"SessionStart"}`,
		`{"session_id":"019f7607-c8b0-74b3-87ca-64a7e6e7ede0","hook_event_name":"SessionStart"} {}`,
	} {
		if _, err := ParseCodexSessionStart(strings.NewReader(input), time.Time{}); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
	oversized := strings.Repeat(" ", maxHookBytes+1)
	if _, err := ParseCodexSessionStart(strings.NewReader(oversized), time.Time{}); err == nil {
		t.Fatal("oversized hook input was accepted")
	}
}

func TestParseClaudeSessionStart(t *testing.T) {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	metadata, err := ParseClaudeSessionStart(strings.NewReader(`{
  "session_id":"019f7607-c8b0-74b3-87ca-64a7e6e7ede0",
  "transcript_path":"/Users/example/.claude/projects/-work/019f7607-c8b0-74b3-87ca-64a7e6e7ede0.jsonl",
  "cwd":"/work",
  "hook_event_name":"SessionStart",
  "source":"fork",
  "model":"claude-opus-4-8"
}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Program != ProgramClaude || metadata.Source != "fork" || metadata.Model != "claude-opus-4-8" || metadata.CWD != "/work" || metadata.TranscriptPath == "" || !metadata.Observed.Equal(now) {
		t.Fatalf("metadata = %#v", metadata)
	}
	encoded, err := Encode(metadata)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(encoded)
	if err != nil || decoded != metadata {
		t.Fatalf("decoded = %#v err=%v", decoded, err)
	}
}

func TestParseClaudeSessionStartRejectsUnsafeInput(t *testing.T) {
	validID := "019f7607-c8b0-74b3-87ca-64a7e6e7ede0"
	for _, input := range []string{
		`{"session_id":"` + validID + `","transcript_path":"relative.jsonl","hook_event_name":"SessionStart","source":"startup"}`,
		`{"session_id":"` + validID + `","transcript_path":"/tmp/session.txt","hook_event_name":"SessionStart","source":"startup"}`,
		`{"session_id":"` + validID + `","transcript_path":"/tmp/019f7607-c8b0-74b3-87ca-64a7e6e7ede1.jsonl","hook_event_name":"SessionStart","source":"startup"}`,
		`{"session_id":"` + validID + `","transcript_path":"/tmp/session.jsonl","hook_event_name":"Stop","source":"startup"}`,
		`{"session_id":"` + validID + `","transcript_path":"/tmp/session.jsonl","hook_event_name":"SessionStart","source":"unknown"}`,
		`{"session_id":"` + validID + `","transcript_path":"/tmp/` + validID + `.jsonl","hook_event_name":"SessionStart","source":"startup","model":"claude opus secret"}`,
		`{"session_id":"bad","transcript_path":"/tmp/session.jsonl","hook_event_name":"SessionStart","source":"startup"}`,
	} {
		if _, err := ParseClaudeSessionStart(strings.NewReader(input), time.Time{}); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
}

func TestEncodeAndDecodeEnforceTheSameMetadataBounds(t *testing.T) {
	base := Metadata{Version: 1, Program: ProgramCodex, SessionID: "019f7607-c8b0-74b3-87ca-64a7e6e7ede0"}
	for _, cwd := range []string{strings.Repeat("x", 4097), "bad\x00path"} {
		metadata := base
		metadata.CWD = cwd
		if _, err := Encode(metadata); err == nil {
			t.Fatalf("Encode accepted invalid cwd of length %d", len(cwd))
		}
	}
	invalidSource := base
	invalidSource.Source = "caller-controlled"
	if _, err := Encode(invalidSource); err == nil {
		t.Fatal("Encode accepted an unknown recovery metadata source")
	}
}
