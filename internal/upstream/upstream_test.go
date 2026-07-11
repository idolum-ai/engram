package upstream

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"
)

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

func TestWriteAndLatestUseVisibleStableRecord(t *testing.T) {
	var output bytes.Buffer
	if err := Write(&output, "tests\nfinished"); err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), "\a[engram:upstream] tests finished\n"; got != want {
		t.Fatalf("signal output = %q, want %q", got, want)
	}
	text := "old\n[engram:upstream] first\nnoise\n[engram:upstream] latest\n"
	if got, ok := Latest(text); !ok || got != "latest" || !Contains(text) {
		t.Fatalf("Latest = %q, %v", got, ok)
	}
	if got, want := WithoutRecords(text), "old\nnoise"; got != want {
		t.Fatalf("WithoutRecords = %q, want %q", got, want)
	}
}
