// Package openai implements Engram's bounded, non-streaming conversational
// guide and voice-note transcription operations with assessed OpenAI models.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/idolum-ai/engram/internal/guide"
	"github.com/idolum-ai/engram/internal/keyseq"
)

type Client struct {
	APIKey     string
	Model      string
	BaseURL    string
	HTTPClient *http.Client
}

// TranscriptionClient implements Engram's one-shot voice-note transcription.
type TranscriptionClient struct {
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

func NewTranscriber(apiKey, model string) *TranscriptionClient {
	return &TranscriptionClient{
		APIKey:  apiKey,
		Model:   model,
		BaseURL: "https://api.openai.com/v1/audio/transcriptions",
		HTTPClient: &http.Client{
			Timeout: 90 * time.Second,
		},
	}
}

func (c *TranscriptionClient) Transcribe(ctx context.Context, path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open voice note: %w", err)
	}
	defer file.Close()

	reader, writer := io.Pipe()
	form := multipart.NewWriter(writer)
	writeDone := make(chan error, 1)
	go func() {
		part, writeErr := form.CreateFormFile("file", "voice.ogg")
		if writeErr == nil {
			_, writeErr = io.Copy(part, file)
		}
		if writeErr == nil {
			writeErr = form.WriteField("model", c.Model)
		}
		if closeErr := form.Close(); writeErr == nil {
			writeErr = closeErr
		}
		_ = writer.CloseWithError(writeErr)
		writeDone <- writeErr
	}()

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL, reader)
	if err != nil {
		_ = reader.CloseWithError(err)
		<-writeDone
		return "", err
	}
	request.Header.Set("Authorization", "Bearer "+c.APIKey)
	request.Header.Set("Content-Type", form.FormDataContentType())
	response, err := c.HTTPClient.Do(request)
	if err != nil {
		_ = reader.CloseWithError(err)
		<-writeDone
		return "", err
	}
	defer response.Body.Close()
	const responseLimit = 1 << 20
	body, readErr := io.ReadAll(io.LimitReader(response.Body, responseLimit+1))
	_ = reader.Close()
	writeErr := <-writeDone
	if readErr != nil {
		return "", readErr
	}
	if len(body) > responseLimit {
		return "", fmt.Errorf("openai transcription response exceeded %d bytes", responseLimit)
	}
	var result struct {
		Text  string `json:"text"`
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if result.Error != nil {
			return "", fmt.Errorf("openai %s: %s", result.Error.Type, result.Error.Message)
		}
		return "", fmt.Errorf("openai status %s", response.Status)
	}
	if writeErr != nil {
		return "", writeErr
	}
	text := strings.TrimSpace(result.Text)
	if text == "" {
		return "", fmt.Errorf("openai returned no transcription")
	}
	return text, nil
}

func (c *Client) Converse(ctx context.Context, in guide.Input) (string, error) {
	result, err := c.ConverseWithEvidence(ctx, in)
	return result.Text, err
}

func (c *Client) ConverseWithEvidence(ctx context.Context, in guide.Input) (guide.Result, error) {
	text, err := c.complete(ctx, guide.SystemPrompt, guide.BuildPrompt(in), guide.MaxTokens, guide.Temperature, nil)
	if err != nil {
		return guide.Result{}, err
	}
	parsed := guide.ParseResult(text)
	parsed.Text = guide.LimitWords(parsed.Text, guide.MaxWords)
	if parsed.Text == "" {
		return guide.Result{}, fmt.Errorf("openai returned no text")
	}
	return parsed, nil
}

func (c *Client) InterpretKeys(ctx context.Context, description string) (keyseq.Proposal, error) {
	text, err := c.complete(ctx, keyseq.SystemPrompt, keyseq.BuildPrompt(description), keyseq.MaxTokens, 0, keyseq.JSONSchema())
	if err != nil {
		return keyseq.Proposal{}, err
	}
	return keyseq.Parse(text)
}

func (c *Client) complete(ctx context.Context, system, prompt string, maxTokens int, temperature float64, schema map[string]any) (string, error) {
	payload := map[string]any{
		"model": c.Model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": prompt},
		},
		"max_completion_tokens": maxTokens,
		"reasoning_effort":      "none",
		"temperature":           temperature,
	}
	if schema != nil {
		payload["response_format"] = map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name": "key_sequence", "strict": true, "schema": schema,
			},
		}
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
	return text, nil
}
