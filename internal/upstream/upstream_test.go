package upstream

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

const testRecordID = "0123456789abcdef0123456789abcdef"

func TestNormalizeProducesOneBoundedUTF8Line(t *testing.T) {
	message, err := Normalize("  tests\nfinished\t\x00" + strings.Repeat("é", MaxMessageBytes) + "  ")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(message, "tests finished ") || strings.ContainsAny(message, "\n\r\t\x00") {
		t.Fatalf("normalized message = %q", message)
	}
	if len(message) > MaxMessageBytes || !utf8.ValidString(message) {
		t.Fatalf("normalized bytes=%d valid=%v", len(message), utf8.ValidString(message))
	}
}

func TestNormalizeRejectsEmptyAndInvalidUTF8(t *testing.T) {
	for _, message := range []string{"\n\t\x00", string([]byte{0xff})} {
		if _, err := Normalize(message); err == nil {
			t.Fatalf("Normalize(%q) succeeded", message)
		}
	}
}

func TestWriteRecordEstablishesBoundaryAndObservationKeepsLatestIdentity(t *testing.T) {
	var output bytes.Buffer
	if err := WriteRecord(&output, Record{ID: testRecordID, Payload: "tests\nfinished"}); err != nil {
		t.Fatal(err)
	}
	want := "\a\r\n[engram:upstream] " + testRecordID + " v1:14:tests finished\r\n"
	if got := output.String(); got != want {
		t.Fatalf("signal output = %q, want %q", got, want)
	}
	secondID := "fedcba9876543210fedcba9876543210"
	text := "old\n" + Prefix + testRecordID + " first\nnoise\n" + Prefix + secondID + " latest\n"
	got := Observe(text)
	if !got.Found || got.Latest != (Record{ID: secondID, Payload: "latest"}) || got.PresentationText != "old\nnoise" {
		t.Fatalf("Observe = %#v", got)
	}
}

func TestObserveStripsMalformedFramingWithoutTreatingItAsSignal(t *testing.T) {
	got := Observe("safe\n" + Prefix + "not-an-id injected path /tmp/nope\nafter")
	if got.Found || got.PresentationText != "safe\nafter" {
		t.Fatalf("Observe malformed = %#v", got)
	}
}

func TestObserveAcceptsBoundedPresentationIndent(t *testing.T) {
	line := Prefix + testRecordID + " v1:41:Codex tool output"
	got := Observe("before\n    " + line + "\n    finished through Engram\n\nafter")
	if !got.Found || got.Latest.Payload != "Codex tool output finished through Engram" || got.PresentationText != "before\n\nafter" {
		t.Fatalf("Observe indented = %#v", got)
	}

	tooDeep := Observe(strings.Repeat(" ", MaxPresentationIndent+1) + line)
	if tooDeep.Found || tooDeep.PresentationText == "" {
		t.Fatalf("Observe over-indented = %#v", tooDeep)
	}
}

func TestObserveKeepsAdjacentIndentedOutputOutsideLengthFramedSignal(t *testing.T) {
	payload := "build complete"
	line := "    " + Prefix + testRecordID + " v1:" + strconv.Itoa(len(payload)) + ":" + payload
	got := Observe(line + "\n    /tmp/report.txt\n    ERROR: deployment failed")
	if !got.Found || got.Latest.Payload != payload || got.PresentationText != "    /tmp/report.txt\n    ERROR: deployment failed" {
		t.Fatalf("Observe adjacent output = %#v", got)
	}
}

func TestObserveDoesNotJoinContinuationToColumnZeroRecord(t *testing.T) {
	line := Prefix + testRecordID + " direct signal"
	got := Observe(line + "\nordinary output")
	if !got.Found || got.Latest.Payload != "direct signal" || got.PresentationText != "ordinary output" {
		t.Fatalf("Observe direct = %#v", got)
	}
}

func TestObserveKeepsIncompleteLengthFramedSignalUnrecognized(t *testing.T) {
	got := Observe("    " + Prefix + testRecordID + " v1:20:short")
	if got.Found || got.PresentationText != "" {
		t.Fatalf("Observe incomplete = %#v", got)
	}
}

func TestWriteRecordRejectsInvalidIdentity(t *testing.T) {
	if err := WriteRecord(&bytes.Buffer{}, Record{ID: "short", Payload: "done"}); err == nil {
		t.Fatal("WriteRecord accepted an invalid record ID")
	}
}
