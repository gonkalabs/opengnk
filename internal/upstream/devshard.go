package upstream

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

// DevshardConfig configures a devshard-gateway upstream client.
type DevshardConfig struct {
	BaseURL string // e.g. http://localhost:8080/v1 or https://my-gateway.com/v1
	APIKey  string // gateway API key (sk-...)
}

// DevshardClient talks to a devshard gateway with Bearer-token auth. It
// implements the Upstream interface. No ECDSA signing, no endpoint discovery,
// no wallet pool — the gateway handles all routing internally.
type DevshardClient struct {
	apiBase string // normalized to end with /v1
	apiKey  string
	http    *http.Client
	stream  *http.Client
}

// NewDevshardClient creates a Bearer-token devshard gateway client.
func NewDevshardClient(cfg DevshardConfig) *DevshardClient {
	apiBase := normalizeGatewayBase(cfg.BaseURL)
	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
	}
	return &DevshardClient{
		apiBase: apiBase,
		apiKey:  strings.TrimSpace(cfg.APIKey),
		http: &http.Client{
			Timeout:   120 * time.Second,
			Transport: transport,
		},
		stream: &http.Client{Transport: transport},
	}
}

func normalizeGatewayBase(raw string) string {
	base := strings.TrimRight(strings.TrimSpace(raw), "/")
	if base == "" {
		base = "http://localhost:8080"
	}
	if strings.HasSuffix(base, "/v1") {
		return base
	}
	return base + "/v1"
}

func (c *DevshardClient) request(ctx context.Context, client *http.Client, method, path string, payload []byte) (*http.Response, error) {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.apiBase+path, body)
	if err != nil {
		return nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	return client.Do(req)
}

// FetchModels returns the model list from the gateway.
func (c *DevshardClient) FetchModels(ctx context.Context) ([]json.RawMessage, error) {
	resp, err := c.request(ctx, c.http, http.MethodGet, "/models", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("devshard models http %d: %s", resp.StatusCode, string(b))
	}
	var result struct {
		Models []json.RawMessage `json:"models"`
		Data   []json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode devshard models: %w", err)
	}
	if len(result.Models) > 0 {
		return result.Models, nil
	}
	if len(result.Data) > 0 {
		return result.Data, nil
	}
	return nil, fmt.Errorf("devshard gateway returned no models")
}

// Do sends a buffered request and returns the full response body.
func (c *DevshardClient) Do(ctx context.Context, method, path string, payload []byte) ([]byte, int, error) {
	resp, err := c.request(ctx, c.http, method, path, payload)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, readErr := io.ReadAll(resp.Body)
	return b, resp.StatusCode, readErr
}

// DoStream sends a request and returns the streaming response (caller owns Body).
func (c *DevshardClient) DoStream(ctx context.Context, method, path string, payload []byte) (*http.Response, error) {
	return c.request(ctx, c.stream, method, path, payload)
}
