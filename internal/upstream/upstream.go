package upstream

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

const Prefix = "[engram:upstream] "
const MaxMessageBytes = 1024
const RecordIDBytes = 16
const RecordIDLength = RecordIDBytes * 2
const MaxPresentationIndent = 8
const MaxPresentationContinuationLines = 32

type Record struct {
	ID      string
	Payload string
}

type Observation struct {
	PresentationText string
	Latest           Record
	Found            bool
}

func Normalize(message string) (string, error) {
	if !utf8.ValidString(message) {
		return "", fmt.Errorf("signal message is not valid UTF-8")
	}
	var normalized strings.Builder
	normalized.Grow(min(len(message), MaxMessageBytes))
	space := false
	for _, r := range message {
		if r == '\n' || r == '\r' || r == '\t' || unicode.IsSpace(r) {
			space = normalized.Len() > 0
			continue
		}
		if unicode.IsControl(r) {
			continue
		}
		if space {
			normalized.WriteByte(' ')
			space = false
		}
		if normalized.Len()+utf8.RuneLen(r) > MaxMessageBytes {
			break
		}
		normalized.WriteRune(r)
	}
	message = strings.TrimSpace(normalized.String())
	if message == "" {
		return "", fmt.Errorf("signal message is empty")
	}
	return message, nil
}

func NewRecordID() (string, error) {
	var id [RecordIDBytes]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("generate signal record ID: %w", err)
	}
	return hex.EncodeToString(id[:]), nil
}

func Write(w io.Writer, message string) error {
	id, err := NewRecordID()
	if err != nil {
		return err
	}
	return WriteRecord(w, Record{ID: id, Payload: message})
}

func WriteRecord(w io.Writer, record Record) error {
	if !validRecordID(record.ID) {
		return fmt.Errorf("invalid signal record ID")
	}
	payload, err := Normalize(record.Payload)
	if err != nil {
		return err
	}
	// CRLF establishes both a physical row and column-zero boundary even when
	// the preceding process left the cursor mid-line or disabled ONLCR.
	_, err = fmt.Fprintf(w, "\a\r\n%s%s %s\r\n", Prefix, record.ID, payload)
	return err
}

func Observe(joinedText string) Observation {
	lines := strings.Split(joinedText, "\n")
	kept := lines[:0]
	var latest Record
	found := false
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		line = strings.TrimLeft(line, "\r")
		recordText, indent, framed := presentationRecordText(line)
		if !framed {
			kept = append(kept, line)
			continue
		}
		if record, ok := parseRecord(recordText); ok {
			if indent > 0 {
				payload := record.Payload
				for continued := 0; continued < MaxPresentationContinuationLines && i+1 < len(lines); continued++ {
					text, ok := presentationContinuation(lines[i+1], indent)
					if !ok {
						break
					}
					i++
					payload += " " + text
				}
				if normalized, err := Normalize(payload); err == nil {
					record.Payload = normalized
				}
			}
			latest = record
			found = true
		}
	}
	return Observation{
		PresentationText: strings.Trim(strings.Join(kept, "\n"), "\n"),
		Latest:           latest,
		Found:            found,
	}
}

func presentationRecordText(line string) (string, int, bool) {
	indent := 0
	for indent < len(line) && line[indent] == ' ' {
		indent++
	}
	if indent > MaxPresentationIndent || !strings.HasPrefix(line[indent:], Prefix) {
		return "", 0, false
	}
	return strings.TrimPrefix(line[indent:], Prefix), indent, true
}

func presentationContinuation(line string, indent int) (string, bool) {
	line = strings.TrimLeft(line, "\r")
	if indent <= 0 || len(line) <= indent || line[:indent] != strings.Repeat(" ", indent) || line[indent] == ' ' {
		return "", false
	}
	text := strings.TrimSpace(line[indent:])
	if text == "" || strings.HasPrefix(text, Prefix) {
		return "", false
	}
	return text, true
}

func parseRecord(text string) (Record, bool) {
	if len(text) <= RecordIDLength || text[RecordIDLength] != ' ' {
		return Record{}, false
	}
	id := text[:RecordIDLength]
	if !validRecordID(id) {
		return Record{}, false
	}
	payload, err := Normalize(text[RecordIDLength+1:])
	if err != nil {
		return Record{}, false
	}
	return Record{ID: id, Payload: payload}, true
}

func validRecordID(id string) bool {
	if len(id) != RecordIDLength {
		return false
	}
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == RecordIDBytes && id == strings.ToLower(id)
}
