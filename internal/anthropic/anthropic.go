package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/idolum-ai/engram/internal/guide"
	"github.com/idolum-ai/engram/internal/keyseq"
)

const SystemPrompt = guide.SystemPrompt
const maxTokens = guide.MaxTokens
const maxConversationWords = guide.MaxWords
const conversationalTemperature = guide.Temperature

type Client struct {
	APIKey     string
	Model      string
	BaseURL    string
	HTTPClient *http.Client
}

type ConversationInput = guide.Input

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

// Converse renders a single terminal observation as conversational prose. It
// deliberately sends no model history and makes exactly one non-streaming call.
func (c *Client) Converse(ctx context.Context, in ConversationInput) (string, error) {
	result, err := c.ConverseWithEvidence(ctx, in)
	return result.Text, err
}

func (c *Client) ConverseWithEvidence(ctx context.Context, in ConversationInput) (guide.Result, error) {
	text, err := c.completeWithTemperature(ctx, guide.SystemPrompt, guide.BuildPrompt(in), guide.MaxTokens, float64Pointer(guide.Temperature))
	if err != nil {
		return guide.Result{}, err
	}
	result := guide.ParseResult(text)
	result.Text = guide.LimitWords(result.Text, guide.MaxWords)
	if result.Text == "" {
		return guide.Result{}, fmt.Errorf("anthropic returned no text")
	}
	return result, nil
}

func (c *Client) InterpretKeys(ctx context.Context, description string) (keyseq.Proposal, error) {
	text, err := c.completeStructured(ctx, keyseq.SystemPrompt, keyseq.BuildPrompt(description), keyseq.MaxTokens, float64Pointer(0), keyseq.JSONSchema())
	if err != nil {
		return keyseq.Proposal{}, err
	}
	return keyseq.Parse(text)
}

func limitWords(text string, maximum int) string {
	return guide.LimitWords(text, maximum)
}

func float64Pointer(value float64) *float64 { return &value }

func (c *Client) complete(ctx context.Context, system, prompt string, tokenLimit int) (string, error) {
	return c.completeWithTemperature(ctx, system, prompt, tokenLimit, nil)
}

func (c *Client) completeWithTemperature(ctx context.Context, system, prompt string, tokenLimit int, temperature *float64) (string, error) {
	return c.completeStructured(ctx, system, prompt, tokenLimit, temperature, nil)
}

func (c *Client) completeStructured(ctx context.Context, system, prompt string, tokenLimit int, temperature *float64, schema map[string]any) (string, error) {
	messages := []map[string]string{
		{"role": "user", "content": prompt},
	}
	payload := map[string]any{
		"model":      c.Model,
		"max_tokens": tokenLimit,
		"system":     system,
		"messages":   messages,
	}
	if temperature != nil {
		payload["temperature"] = *temperature
	}
	if schema != nil {
		payload["output_config"] = map[string]any{
			"format": map[string]any{"type": "json_schema", "schema": schema},
		}
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
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Error      *struct {
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
	switch out.StopReason {
	case "end_turn":
	case "max_tokens":
		return "", fmt.Errorf("anthropic response exceeded its output limit")
	default:
		return "", fmt.Errorf("anthropic response ended with unexpected stop_reason %q", out.StopReason)
	}

	parts := make([]string, 0, len(out.Content))
	for _, content := range out.Content {
		if content.Type == "text" && strings.TrimSpace(content.Text) != "" {
			parts = append(parts, strings.TrimSpace(content.Text))
		}
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("anthropic returned no text")
	}
	return strings.Join(parts, "\n"), nil
}

func buildPrompt(in ConversationInput) string {
	return guide.BuildPrompt(in)
}
