// Package llmclassifier provides a Classifier that uses a local
// OpenAI-compatible LLM (e.g. Ollama with qwen3:4b) to detect sensitive
// spans that NER cannot catch -- things like API keys and passwords.
//
// We ask the model to return the sensitive strings verbatim rather than byte
// offsets, because small models get offsets wrong. Go code locates all
// occurrences in the original text itself.
package llmclassifier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gonkalabs/gonka-proxy-go/internal/sanitize"
)

const systemPrompt = `Extract sensitive data from the text. Return a JSON array of exact strings that are sensitive. Return [] if nothing sensitive found.

Sensitive data includes:
- API keys and tokens: strings starting with sk-, pk-, ghp_, Bearer, or any alphanumeric string that looks like a credential (e.g. sk123123123, sk-abc123, ghp_xyz789)
- Passwords and secrets mentioned explicitly
- Email addresses (e.g. user@example.com)
- Phone numbers (e.g. +79997899900, 8-800-555-35-35)
- Full person names with first+last (e.g. John Smith, Иван Иванов, Виктор Александрович)
- Credit card numbers, IBANs, bank account numbers
- Private keys (long hex or base64 strings)

Do NOT flag: «TOKEN_» placeholders, city names alone, common words, dates, regular numbers.

Return ONLY a valid JSON array of the exact sensitive strings. No explanation.

Examples:
Input: "my api key is sk-abc123xyz789"
Output: ["sk-abc123xyz789"]

Input: "call me at +79997899900, John Smith"
Output: ["+79997899900", "John Smith"]

Input: "ключ апи sk123123123"
Output: ["sk123123123"]

Input: "how are you?"
Output: []`

// Classifier calls a local LLM to detect semantically sensitive values.
type Classifier struct {
	url   string
	model string
	http  *http.Client
}

// New creates a Classifier.
// baseURL is the Ollama (or any OpenAI-compatible) server, e.g. "http://ollama:11434".
// threshold is not used currently but kept for interface compatibility.
func New(baseURL, model string, threshold float32) *Classifier {
	return &Classifier{
		url:   strings.TrimRight(baseURL, "/") + "/v1/chat/completions",
		model: model,
		http: &http.Client{
			Timeout: 125 * time.Second,
		},
	}
}

type openAIRequest struct {
	Model       string    `json:"model"`
	Messages    []message `json:"messages"`
	Temperature float64   `json:"temperature"`
	MaxTokens   int       `json:"max_tokens"`
	// Hint to disable chain-of-thought thinking (Qwen3 and some others support this).
	// stripThinkBlock handles models that ignore it.
	Think bool `json:"think"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIResponse struct {
	Choices []struct {
		Message struct {
			Content          string `json:"content"`
			Reasoning        string `json:"reasoning"`         // Qwen3 via Ollama
			ReasoningContent string `json:"reasoning_content"` // Qwen3 direct API
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

// Classify sends text to the LLM and returns sensitive spans.
// It is safe for concurrent use.
func (c *Classifier) Classify(text string) ([]sanitize.Span, error) {
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	slog.Info("llmclassifier: classifying", "url", c.url, "model", c.model, "text_len", len(text))

	reqBody := openAIRequest{
		Model: c.model,
		Messages: []message{
			{Role: "system", Content: systemPrompt},
			// /no_think is Qwen3's control token to skip thinking and go straight to the answer.
			{Role: "user", Content: "Text to classify:\n" + text + "\n/no_think"},
		},
		Temperature: 0,
		MaxTokens:   10000,
		Think:       false,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("llmclassifier: marshal: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("llmclassifier: request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		slog.Warn("llmclassifier: LLM unreachable, skipping", "err", err)
		return nil, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errBody [512]byte
		n, _ := resp.Body.Read(errBody[:])
		slog.Warn("llmclassifier: unexpected status", "code", resp.StatusCode, "body", string(errBody[:n]))
		return nil, nil
	}

	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		slog.Warn("llmclassifier: read body", "err", err)
		return nil, nil
	}
	slog.Info("llmclassifier: full response body", "body", string(rawBody))

	var oaiResp openAIResponse
	if err := json.Unmarshal(rawBody, &oaiResp); err != nil {
		slog.Warn("llmclassifier: decode response", "err", err)
		return nil, nil
	}

	if len(oaiResp.Choices) == 0 {
		return nil, nil
	}

	choice := oaiResp.Choices[0]
	msg := choice.Message
	slog.Info("llmclassifier: raw response",
		"content", msg.Content,
		"reasoning", msg.Reasoning,
		"finish_reason", choice.FinishReason,
	)

	if choice.FinishReason == "length" {
		slog.Warn("llmclassifier: response truncated by token limit, increase MaxTokens or shorten prompt")
	}

	// Qwen3 via Ollama puts thinking in "reasoning" and the answer in "content".
	// If content is empty the model ran out of tokens before answering; fall
	// back to the reasoning field and dig the JSON array out of it.
	raw := strings.TrimSpace(msg.Content)
	if raw == "" {
		raw = strings.TrimSpace(msg.Reasoning)
		if raw == "" {
			raw = strings.TrimSpace(msg.ReasoningContent)
		}
	}

	content := stripThinkBlock(raw)
	content = stripCodeFence(content)
	// Last resort: try to pull a JSON array out of wherever it is in the text.
	if !strings.Contains(content, "[") {
		content = extractJSONArray(content)
	}
	slog.Info("llmclassifier: parsed content", "content", content)

	// Parse the array of sensitive strings.
	var sensitiveValues []string
	if err := json.Unmarshal([]byte(content), &sensitiveValues); err != nil {
		slog.Warn("llmclassifier: could not parse LLM output", "content", content, "err", err)
		return nil, nil
	}

	if len(sensitiveValues) == 0 {
		return nil, nil
	}

	// Find every occurrence of each sensitive value in the original text.
	// Skip matches that land in the middle of a longer word.
	var spans []sanitize.Span
	for _, val := range sensitiveValues {
		val = strings.TrimSpace(val)
		if val == "" {
			continue
		}
		if strings.HasPrefix(val, "«TOKEN_") {
			continue
		}
		start := 0
		for {
			idx := strings.Index(text[start:], val)
			if idx < 0 {
				break
			}
			abs := start + idx
			end := abs + len(val)
			if isInsideToken(text, abs, end) {
				start = end
				continue
			}
			spans = append(spans, sanitize.Span{
				Start: abs,
				End:   end,
				Label: "LLM",
				Score: 1.0,
			})
			start = end
		}
	}

	if len(spans) > 0 {
		slog.Info("llmclassifier: detected sensitive spans", "count", len(spans), "values", len(sensitiveValues))
	}
	return spans, nil
}

// isInsideToken reports whether span [start,end) sits inside a larger word.
// For example "sd@yandex.ru" inside "asd@yandex.ru" would return true.
func isInsideToken(text string, start, end int) bool {
	if start > 0 && !isBoundary(text[start-1]) {
		return true
	}
	if end < len(text) && !isBoundary(text[end]) {
		return true
	}
	return false
}

// isBoundary reports whether byte b is a word-boundary character.
func isBoundary(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '<', '>', ',', ';', '(', ')', '[', ']', '{', '}', '"', '\'', '`':
		return true
	}
	return false
}

// extractJSONArray finds the first [...] substring in s.
func extractJSONArray(s string) string {
	start := strings.Index(s, "[")
	if start < 0 {
		return s
	}
	end := strings.LastIndex(s, "]")
	if end < start {
		return s
	}
	return s[start : end+1]
}

// stripThinkBlock removes Qwen3's <think>...</think> block that appears before
// the actual answer when thinking mode is active.
func stripThinkBlock(s string) string {
	const open, close = "<think>", "</think>"
	start := strings.Index(s, open)
	if start < 0 {
		return s
	}
	end := strings.Index(s, close)
	if end < 0 {
		// Unclosed block - drop everything from <think> onwards.
		return strings.TrimSpace(s[:start])
	}
	return strings.TrimSpace(s[:start] + s[end+len(close):])
}

// stripCodeFence removes ```json ... ``` or ``` ... ``` wrappers.
func stripCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		if idx := strings.Index(s, "\n"); idx >= 0 {
			s = s[idx+1:]
		}
		if idx := strings.LastIndex(s, "```"); idx >= 0 {
			s = s[:idx]
		}
		s = strings.TrimSpace(s)
	}
	return s
}
