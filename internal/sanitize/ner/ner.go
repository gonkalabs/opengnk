// Package ner provides a Classifier that calls the sanitize-ner Python sidecar
// over HTTP. If the sidecar is unreachable, it logs a warning and returns no
// spans so the rest of the sanitization pipeline can still run.
package ner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/gonkalabs/gonka-proxy-go/internal/sanitize"
)

// Client calls the NER sidecar's /classify endpoint.
type Client struct {
	url  string
	http *http.Client
}

// New creates a NER Client pointing at the given base URL
// (e.g. "http://sanitize-ner:8001").
func New(baseURL string) *Client {
	return &Client{
		url: baseURL + "/classify",
		http: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

type classifyRequest struct {
	Text string `json:"text"`
}

type classifyResponse struct {
	Spans []nerSpan `json:"spans"`
}

type nerSpan struct {
	Start int    `json:"start"`
	End   int    `json:"end"`
	Label string `json:"label"`
	Text  string `json:"text"`
}

// Classify sends text to the NER sidecar and returns sensitive spans.
// It is safe for concurrent use.
func (c *Client) Classify(text string) ([]sanitize.Span, error) {
	body, err := json.Marshal(classifyRequest{Text: text})
	if err != nil {
		return nil, fmt.Errorf("ner: marshal: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ner: request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		slog.Warn("sanitize-ner: sidecar unreachable, skipping NER layer", "err", err)
		return nil, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		slog.Warn("sanitize-ner: unexpected status", "code", resp.StatusCode)
		return nil, nil
	}

	var result classifyResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("ner: decode: %w", err)
	}

	spans := make([]sanitize.Span, 0, len(result.Spans))
	for _, s := range result.Spans {
		spans = append(spans, sanitize.Span{
			Start: s.Start,
			End:   s.End,
			Label: s.Label,
			Score: 1.0,
		})
	}
	return spans, nil
}
