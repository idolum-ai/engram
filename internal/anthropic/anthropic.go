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

const SystemPrompt = `You are Engram's terminal guide for a technical user reading Telegram on a phone. Explain the terminal state in plain English and recommend one concrete next action. Decide whether the pane has reached a handoff: a boundary in an apparent recent or pending task where work cannot usefully advance until the user supplies judgment, input, approval, correction, credentials, or chooses what happens next. A completed requested command is such a boundary; a bare shell with no apparent task is not. Ground every handoff in captured terminal evidence. Do not explain Engram, pretend to be the process, invent success or citation text, or page the user merely because output changed. Return JSON only.`

type Client struct {
	APIKey     string
	Model      string
	BaseURL    string
	HTTPClient *http.Client
}

type SummaryInput struct {
	SessionID           int
	State               string
	LastInput           string
	LastInputMode       string
	PreviousSummary     string
	HasPreviousCapture  bool
	CaptureChanged      bool
	OpenHandoff         bool
	HandoffKey          string
	HandoffStatus       string
	HandoffAction       string
	HandoffEvidence     []string
	HandoffAcknowledged bool
	VisibleCapture      string
	FullCapture         string
}

type GuideReport struct {
	StatusReport      string   `json:"status_report"`
	RecommendedAction string   `json:"recommended_action"`
	Citations         []string `json:"citations"`
	HumanNeeded       bool     `json:"human_needed"`
	HandoffKey        string   `json:"handoff_key"`
	Confidence        string   `json:"confidence"`
	NeedsFullBuffer   bool     `json:"needs_full_buffer"`
	Reason            string   `json:"reason"`
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
	if in.HasPreviousCapture {
		fmt.Fprintf(&b, "visible_capture_changed_since_previous_observation: %t\n", in.CaptureChanged)
		b.WriteString("observation_change_note: if the capture did not change after input, say that no visible effect was established; do not claim the requested outcome occurred.\n")
	}
	if in.OpenHandoff {
		b.WriteString("\nopen_handoff:\n")
		fmt.Fprintf(&b, "key: %s\n", in.HandoffKey)
		fmt.Fprintf(&b, "status: %s\n", limit(in.HandoffStatus, 600))
		fmt.Fprintf(&b, "recommended_action: %s\n", limit(in.HandoffAction, 300))
		fmt.Fprintf(&b, "acknowledged_by_later_user_input: %t\n", in.HandoffAcknowledged)
		if len(in.HandoffEvidence) > 0 {
			fmt.Fprintf(&b, "evidence: %s\n", strings.Join(in.HandoffEvidence, " | "))
		}
		b.WriteString("open_handoff_note: this is a durable earlier observation, not proof that the current pane still needs the user. Judge it against current evidence.\n")
	}
	b.WriteString("last_input_preview_note: this is a shortened metadata preview; do not treat truncation here as user-visible truncation.\n\n")
	b.WriteString("capture_filter_note: repeated lines identical to lines in recent visible captures for this same session may have been omitted before this prompt, including from the optional full-scrollback retry; treat missing repeated boilerplate as intentional, not as terminal corruption.\n\n")
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
  "citations": ["zero to two short reconstructed excerpts from the terminal text that support the status or recommendation"],
	  "human_needed": false,
	  "handoff_key": "empty unless human_needed is true; otherwise a short stable snake_case name for the specific decision or intervention",
  "confidence": "high|medium|low",
  "needs_full_buffer": false,
  "reason": "hidden one-sentence reason for confidence and whether full scrollback is needed"
}

Citation rules:
- Use citations for the terminal text most relevant to the user's next action: prompts, errors, commands waiting for input, failing checks, file paths, or completion messages.
- Reconstruct citation text only from the terminal captures. You may repair broken line wraps, repeated whitespace, and obvious terminal-control artifacts, but do not add facts or words that are not supported by the capture.
- Keep each citation under 280 characters. Use plain text, not Markdown.
- Leave citations empty when no reliable excerpt is available.

Handoff rules:
- Set human_needed=true only when the apparent work cannot usefully advance until the user intervenes or chooses what happens next.
- Explicit prompts, failed commands awaiting correction, completed requested work awaiting the next choice, and blocked credential or approval requests can be handoffs.
- Ordinary progress, transient output, an ambiguous partial screen, and a bare idle shell with no apparent pending work are not handoffs.
- When last_input_preview and the capture establish that a requested command completed, hand the result back even if it succeeded and no blocker exists. Absence of an error is not absence of a handoff.
- Do not turn a bare prompt into a handoff merely because a shell always permits the user to choose another command. There must be evidence of a recent or pending task boundary.
- A handoff requires at least one citation that directly establishes the boundary. If reliable evidence is missing, set human_needed=false and request the full buffer when it could resolve the uncertainty.
- handoff_key identifies the intervention, not the session phase. Keep the same key when the same specific handoff remains; use a different key only when current evidence requires a materially different intervention.
- Reassess the open handoff from current evidence. It may remain needed, be replaced by a different handoff, or no longer be needed. Do not preserve it from inertia.

Use needs_full_buffer=true when the visible pane is ambiguous, mid-scroll, or missing earlier context needed for a useful recommendation. Treat last_input_preview as a shortened metadata preview, not proof that the user's message was cut off. If a prompt appears at the bottom, describe it as the current visible prompt only; do not merge it into unrelated work.`)
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
	report.Citations = normalizeCitations(report.Citations)
	report.HandoffKey = normalizeHandoffKey(report.HandoffKey)
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
	if report.HumanNeeded && (report.HandoffKey == "" || len(report.Citations) == 0) {
		report.HumanNeeded = false
		report.HandoffKey = ""
		report.Confidence = "low"
		report.NeedsFullBuffer = true
	}
	if !report.HumanNeeded {
		report.HandoffKey = ""
	}
	return report, nil
}

func normalizeHandoffKey(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	underscore := false
	for _, r := range value {
		if b.Len() >= 80 {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			underscore = false
		case b.Len() > 0 && !underscore:
			b.WriteByte('_')
			underscore = true
		}
	}
	return strings.Trim(b.String(), "_")
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
	text := "status:\n" + status + "\n\nrecommendation:\n" + action
	if citations := normalizeCitations(r.Citations); len(citations) > 0 {
		text += "\n\nevidence:\n" + renderCitationBlocks(citations)
	}
	return text
}

func limit(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func normalizeCitations(values []string) []string {
	out := make([]string, 0, min(len(values), 2))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		value = strings.Join(strings.Fields(value), " ")
		value = limitHead(value, 280)
		out = append(out, value)
		if len(out) == 2 {
			break
		}
	}
	return out
}

func renderCitationBlocks(values []string) string {
	var b strings.Builder
	for i, value := range values {
		if i > 0 {
			b.WriteString("\n")
		}
		for _, line := range strings.Split(value, "\n") {
			b.WriteString("> ")
			b.WriteString(strings.TrimSpace(line))
			b.WriteString("\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func limitHead(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 12 {
		return s[:n]
	}
	return s[:n-12] + " [truncated]"
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
