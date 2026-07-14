// Package openai implements Engram's conversational guide with an assessed
// OpenAI model. It deliberately exposes only the bounded, non-streaming guide
// operation Engram needs.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/idolum-ai/engram/internal/guide"
)

type Client struct {
	APIKey     string
	Model      string
	BaseURL    string
	HTTPClient *http.Client
}

func New(apiKey, model string) *Client {
	return &Client{
		APIKey:  apiKey,
		Model:   model,
		BaseURL: "https://api.openai.com/v1/chat/completions",
		HTTPClient: &http.Client{
			Timeout: 45 * time.Second,
		},
	}
}

func (c *Client) Converse(ctx context.Context, in guide.Input) (string, error) {
	payload := map[string]any{
		"model": c.Model,
		"messages": []map[string]string{
			{"role": "system", "content": guide.SystemPrompt},
			{"role": "user", "content": guide.BuildPrompt(in)},
		},
		"max_completion_tokens": guide.MaxTokens,
		"reasoning_effort":      "none",
		"temperature":           guide.Temperature,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", "Bearer "+c.APIKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.HTTPClient.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return "", err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if result.Error != nil {
			return "", fmt.Errorf("openai %s: %s", result.Error.Type, result.Error.Message)
		}
		return "", fmt.Errorf("openai status %s", response.Status)
	}
	if len(result.Choices) != 1 {
		return "", fmt.Errorf("openai returned %d choices", len(result.Choices))
	}
	switch result.Choices[0].FinishReason {
	case "stop":
	case "length":
		return "", fmt.Errorf("openai response exceeded its output limit")
	default:
		return "", fmt.Errorf("openai response ended with unexpected finish_reason %q", result.Choices[0].FinishReason)
	}
	text := strings.TrimSpace(result.Choices[0].Message.Content)
	if text == "" {
		return "", fmt.Errorf("openai returned no text")
	}
	return guide.LimitWords(text, guide.MaxWords), nil
}
