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

func TestEncodeAndDecodeEnforceTheSameMetadataBounds(t *testing.T) {
	base := Metadata{Version: 1, Program: ProgramCodex, SessionID: "019f7607-c8b0-74b3-87ca-64a7e6e7ede0"}
	for _, cwd := range []string{strings.Repeat("x", 4097), "bad\x00path"} {
		metadata := base
		metadata.CWD = cwd
		if _, err := Encode(metadata); err == nil {
			t.Fatalf("Encode accepted invalid cwd of length %d", len(cwd))
		}
	}
}
