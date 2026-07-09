package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestCitationEvalFixturesRunThroughGuidePipeline(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		capture  string
		citation string
	}{
		{
			name: "dependency error with wrapped lines",
			capture: strings.Join([]string{
				"npm ERR! code ERESOLVE",
				"npm ERR! Could not resolve dependency:",
				"peer react@\"^18\"",
				"found react@19",
			}, "\n"),
			citation: `npm ERR! Could not resolve dependency: peer react@"^18" found react@19`,
		},
		{
			name: "word split by terminal wrap",
			capture: strings.Join([]string{
				"error: permis",
				"sion denied: /tmp/engram/logs",
				"press y/N to continue",
			}, "\n"),
			citation: "error: permission denied: /tmp/engram/logs press y/N to continue",
		},
		{
			name:     "ansi colored failure",
			capture:  "\x1b[31mFAILED\x1b[0m tests/integration\nexit status 1",
			citation: "FAILED tests/integration exit status 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := New("key", "claude-haiku-4-5-20251001")
			client.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				var payload struct {
					Messages []struct {
						Content string `json:"content"`
					} `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Fatal(err)
				}
				if len(payload.Messages) != 1 || !strings.Contains(payload.Messages[0].Content, tt.capture) {
					t.Fatalf("prompt did not include visible capture:\n%#v", payload.Messages)
				}
				report := GuideReport{
					StatusReport:      "The visible terminal output shows a blocked command.",
					RecommendedAction: "Read the cited error and retry with the missing fix.",
					Citations:         []string{tt.citation},
					Confidence:        "high",
				}
				return jsonGuideResponse(t, report), nil
			})}

			report, err := client.Guide(context.Background(), SummaryInput{
				SessionID:      1,
				State:          "running",
				VisibleCapture: tt.capture,
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Citations) != 1 || report.Citations[0] != tt.citation {
				t.Fatalf("citations = %#v, want %q", report.Citations, tt.citation)
			}
			if !citationSupportedByCapture(tt.citation, tt.capture) {
				t.Fatalf("citation is not supported by capture\ncitation: %q\ncapture:\n%s", tt.citation, tt.capture)
			}
			if !strings.Contains(report.TelegramText(), "> "+tt.citation) {
				t.Fatalf("telegram text did not render citation block:\n%s", report.TelegramText())
			}
		})
	}
}

func TestCitationEvalRejectsUnsupportedText(t *testing.T) {
	t.Parallel()

	capture := "make: *** [check] Error 1"
	citation := "make check succeeded"
	if citationSupportedByCapture(citation, capture) {
		t.Fatalf("unsupported citation passed evaluator: %q over %q", citation, capture)
	}
}

func jsonGuideResponse(t *testing.T, report GuideReport) *http.Response {
	t.Helper()
	reportJSON, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := json.Marshal(map[string]any{
		"type": "message",
		"content": []map[string]string{
			{"type": "text", "text": string(reportJSON)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Body:       io.NopCloser(bytes.NewReader(envelope)),
		Header:     make(http.Header),
	}
}

func citationSupportedByCapture(citation, capture string) bool {
	needle := canonicalEvalText(citation)
	haystack := canonicalEvalText(capture)
	return needle != "" && strings.Contains(haystack, needle)
}

func canonicalEvalText(text string) string {
	text = stripANSIEval(text)
	var b strings.Builder
	for _, r := range strings.ToLower(text) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func stripANSIEval(text string) string {
	var b strings.Builder
	for i := 0; i < len(text); i++ {
		if text[i] != 0x1b {
			b.WriteByte(text[i])
			continue
		}
		if i+1 < len(text) && text[i+1] == '[' {
			i += 2
			for i < len(text) && (text[i] < '@' || text[i] > '~') {
				i++
			}
		}
	}
	return b.String()
}
