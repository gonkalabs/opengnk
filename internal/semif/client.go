package semif

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client talks to an OpenAI-compatible vLLM (or fastjev-serve) endpoint.
type Client struct {
	BaseURL    string
	APIKey     string
	Model      string
	HTTPClient *http.Client
}

// NewClient builds a scorer client. baseURL may include or omit /v1.
func NewClient(baseURL, apiKey, model string, timeout time.Duration) *Client {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	if model == "" {
		model = DefaultModel
	}
	return &Client{
		BaseURL: baseURL,
		APIKey:  strings.TrimSpace(apiKey),
		Model:   model,
		HTTPClient: &http.Client{
			Timeout: timeout,
		},
	}
}

// ChatBody is the OpenAI chat/completions payload used for scoring.
func ChatBody(model string, messages []Message) ([]byte, error) {
	payload := map[string]any{
		"model":                 model,
		"messages":              messages,
		"max_tokens":            1,
		"temperature":           0,
		"logprobs":              true,
		"top_logprobs":          20,
		"chat_template_kwargs":  map[string]any{"enable_thinking": false},
		"add_generation_prompt": true,
	}
	return json.Marshal(payload)
}

// Score runs one decision against the dedicated vLLM.
func (c *Client) Score(ctx context.Context, req Request) (*Result, error) {
	if c == nil || c.BaseURL == "" {
		return nil, fmt.Errorf("semif scorer is not configured")
	}
	messages, promptHash, err := DirectMessages(req)
	if err != nil {
		return nil, err
	}
	model := c.Model
	if strings.TrimSpace(req.Model) != "" {
		model = strings.TrimSpace(req.Model)
	}
	body, err := ChatBody(model, messages)
	if err != nil {
		return nil, err
	}
	raw, err := c.post(ctx, body)
	if err != nil {
		return nil, err
	}
	return DecodeScore(req, raw, model, "vllm", promptHash)
}

func (c *Client) post(ctx context.Context, body []byte) ([]byte, error) {
	url := c.BaseURL
	if !strings.HasSuffix(url, "/chat/completions") {
		if strings.HasSuffix(url, "/v1") {
			url += "/chat/completions"
		} else {
			url += "/v1/chat/completions"
		}
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("semif upstream: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("semif upstream read: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 400 {
			msg = msg[:400] + "…"
		}
		return nil, fmt.Errorf("semif upstream %d: %s", resp.StatusCode, msg)
	}
	return raw, nil
}

// DecodeScore turns a chat/completions body into a SemIf result.
func DecodeScore(req Request, raw []byte, model, backend, promptHash string) (*Result, error) {
	logits, sampled, usage, missing, err := ExtractLogits(raw, len(req.Options))
	readout := ReadoutNative
	if err != nil {
		logits, missing, degErr := DegradedLogits(len(req.Options), sampled)
		if degErr != nil {
			if err != nil {
				return nil, err
			}
			return nil, degErr
		}
		readout = ReadoutDegraded
		return Finish(req, logits, missing, usage, model, backend, readout, promptHash)
	}
	if len(missing) == len(req.Options)-1 && sampled != "" {
		readout = ReadoutDegraded
	}
	return Finish(req, logits, missing, usage, model, backend, readout, promptHash)
}
