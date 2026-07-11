package upstream

import (
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

const Prefix = "[engram:upstream] "
const MaxMessageBytes = 1024

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

func Write(w io.Writer, message string) error {
	message, err := Normalize(message)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "\a%s%s\n", Prefix, message)
	return err
}

func Contains(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimLeft(line, "\r"), Prefix) {
			return true
		}
	}
	return false
}

func Latest(text string) (string, bool) {
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimLeft(lines[i], "\r")
		if !strings.HasPrefix(line, Prefix) {
			continue
		}
		message, err := Normalize(strings.TrimPrefix(line, Prefix))
		if err == nil {
			return message, true
		}
	}
	return "", false
}

func WithoutRecords(text string) string {
	lines := strings.Split(text, "\n")
	kept := lines[:0]
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimLeft(line, "\r"), Prefix) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Trim(strings.Join(kept, "\n"), "\n")
}
