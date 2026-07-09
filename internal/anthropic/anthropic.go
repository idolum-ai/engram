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

const SystemPrompt = `Imagine you are a small phone screen re-rendering a terminal. Preserve the meaning, current state, errors, prompts, and actionable output faithfully, but compress the raw buffer into a readable Telegram message. Do not invent success, files, commands, or next steps that are not supported by the visible buffer.`

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
	RecentDelta     string
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

func (c *Client) Summarize(ctx context.Context, in SummaryInput) (string, error) {
	prompt := buildPrompt(in)
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
	fmt.Fprintf(&b, "last_input: %s\n\n", in.LastInput)
	if strings.TrimSpace(in.PreviousSummary) != "" {
		b.WriteString("previous_summary:\n")
		b.WriteString(limit(in.PreviousSummary, 800))
		b.WriteString("\n\n")
	}
	if strings.TrimSpace(in.RecentDelta) != "" {
		b.WriteString("recent_output_delta:\n")
		b.WriteString(limit(in.RecentDelta, 2000))
		b.WriteString("\n\n")
	}
	b.WriteString("visible_terminal_capture:\n")
	b.WriteString(limit(in.VisibleCapture, 6000))
	b.WriteString("\n\nReturn a concise Telegram-ready rendering with these labels when useful: status, summary, next, prompt. Keep it faithful and compact.")
	return b.String()
}

func limit(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
