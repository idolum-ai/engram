package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const SystemPrompt = `You are Engram's terminal guide for a technical user reading Telegram on a phone. Explain the terminal state in plain English, point out likely blockers or prompts, and recommend one concrete next action. Do not pretend to be the process, do not invent success, and mark uncertainty clearly. Return JSON only.`

type Client struct {
	APIKey     string
	Model      string
	BaseURL    string
	HTTPClient *http.Client
}

type SummaryInput struct {
	SessionID       int
	State           string
	LastInput       string
	LastInputMode   string
	PreviousSummary string
	VisibleCapture  string
	FullCapture     string
}

type GuideReport struct {
	StatusReport      string `json:"status_report"`
	RecommendedAction string `json:"recommended_action"`
	Confidence        string `json:"confidence"`
	NeedsFullBuffer   bool   `json:"needs_full_buffer"`
	Reason            string `json:"reason"`
}

func New(apiKey, model string) *Client {
	return &Client{
		APIKey:  apiKey,
		Model:   model,
		BaseURL: "https://api.anthropic.com/v1/messages",
		HTTPClient: &http.Client{
			Timeout: 45 * time.Second,
		},
	}
}

func (c *Client) Guide(ctx context.Context, in SummaryInput) (GuideReport, error) {
	prompt := buildPrompt(in)
	text, err := c.complete(ctx, prompt)
	if err != nil {
		return GuideReport{}, err
	}
	return parseGuideReport(text)
}

func (c *Client) Summarize(ctx context.Context, in SummaryInput) (string, error) {
	report, err := c.Guide(ctx, in)
	if err != nil {
		return "", err
	}
	return report.TelegramText(), nil
}

func (c *Client) complete(ctx context.Context, prompt string) (string, error) {
	payload := map[string]any{
		"model":      c.Model,
		"max_tokens": 700,
		"system":     SystemPrompt,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL, bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if out.Error != nil {
			return "", fmt.Errorf("anthropic %s: %s", out.Error.Type, out.Error.Message)
		}
		return "", fmt.Errorf("anthropic status %s", resp.Status)
	}
	var parts []string
	for _, c := range out.Content {
		if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
			parts = append(parts, strings.TrimSpace(c.Text))
		}
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("anthropic returned no text")
	}
	return strings.Join(parts, "\n"), nil
}

func buildPrompt(in SummaryInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "terminal_session: [%d]\n", in.SessionID)
	fmt.Fprintf(&b, "state: %s\n", in.State)
	fmt.Fprintf(&b, "last_input_mode: %s\n", in.LastInputMode)
	fmt.Fprintf(&b, "last_input_preview: %s\n", in.LastInput)
	b.WriteString("last_input_preview_note: this is a shortened metadata preview; do not treat truncation here as user-visible truncation.\n\n")
	if strings.TrimSpace(in.PreviousSummary) != "" {
		b.WriteString("previous_summary:\n")
		b.WriteString(limit(in.PreviousSummary, 800))
		b.WriteString("\n\n")
	}
	b.WriteString("visible_terminal_capture:\n")
	b.WriteString(limit(in.VisibleCapture, 6000))
	b.WriteString("\n\n")
	if strings.TrimSpace(in.FullCapture) != "" {
		b.WriteString("full_scrollback_capture:\n")
		b.WriteString(limit(in.FullCapture, 24000))
		b.WriteString("\n\n")
	}
	b.WriteString(`Return exactly one JSON object:
{
  "status_report": "one or two short plain-English paragraphs explaining what the session appears to be doing and any blocker/prompt/error",
  "recommended_action": "one clear sentence recommending the user's next action",
  "confidence": "high|medium|low",
  "needs_full_buffer": false,
  "reason": "hidden one-sentence reason for confidence and whether full scrollback is needed"
}

Use needs_full_buffer=true when the visible pane is ambiguous, mid-scroll, or missing earlier context needed for a useful recommendation. Treat last_input_preview as a shortened metadata hint, not proof that the user's message was cut off. If a prompt appears at the bottom, describe it as the current visible prompt only; do not merge it into unrelated work.`)
	return b.String()
}

func parseGuideReport(text string) (GuideReport, error) {
	text = extractJSONObject(strings.TrimSpace(text))
	var report GuideReport
	if err := json.Unmarshal([]byte(text), &report); err != nil {
		if strings.TrimSpace(text) == "" {
			return GuideReport{}, err
		}
		return GuideReport{
			StatusReport:      strings.TrimSpace(text),
			RecommendedAction: "Review the raw terminal output before acting.",
			Confidence:        "low",
			NeedsFullBuffer:   true,
			Reason:            "model returned non-JSON text",
		}, nil
	}
	report.StatusReport = strings.TrimSpace(report.StatusReport)
	report.RecommendedAction = strings.TrimSpace(report.RecommendedAction)
	report.Confidence = normalizeConfidence(report.Confidence)
	report.Reason = strings.TrimSpace(report.Reason)
	if report.StatusReport == "" {
		report.StatusReport = "The terminal state is unclear from the current capture."
		report.Confidence = "low"
		report.NeedsFullBuffer = true
	}
	if report.RecommendedAction == "" {
		report.RecommendedAction = "Review the raw terminal output before acting."
	}
	return report, nil
}

func extractJSONObject(text string) string {
	if strings.HasPrefix(text, "```") {
		lines := strings.Split(text, "\n")
		if len(lines) >= 3 {
			text = strings.Join(lines[1:len(lines)-1], "\n")
		}
	}
	start := strings.IndexByte(text, '{')
	end := strings.LastIndexByte(text, '}')
	if start >= 0 && end >= start {
		return text[start : end+1]
	}
	return text
}

func normalizeConfidence(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "high", "medium", "low":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return "low"
	}
}

func (r GuideReport) WantsFullBuffer() bool {
	return r.NeedsFullBuffer || normalizeConfidence(r.Confidence) == "low"
}

func (r GuideReport) TelegramText() string {
	status := strings.TrimSpace(r.StatusReport)
	if status == "" {
		status = "The terminal state is unclear from the current capture."
	}
	action := strings.TrimSpace(r.RecommendedAction)
	if action == "" {
		action = "Review the raw terminal output before acting."
	}
	return "status:\n" + status + "\n\nrecommendation:\n" + action
}

func limit(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
