package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gonkalabs/gonka-proxy-go/internal/wallet"
)

// Endpoint represents a Gonka network node with its transfer address.
type Endpoint struct {
	URL     string // e.g. http://node2.gonka.ai:8000/v1
	Address string // bech32 address of this host
}

// allowedTransferAgents is the whitelist of nodes that support the
// Transfer Agent feature (v0.2.9+). Only these endpoints can be used
// for proxied inference requests.
var allowedTransferAgents = map[string]bool{
	"gonka1y2a9p56kv044327uycmqdexl7zs82fs5ryv5le": true,
	"gonka1dkl4mah5erqggvhqkpc8j3qs5tyuetgdy552cp": true,
	"gonka1kx9mca3xm8u8ypzfuhmxey66u0ufxhs7nm6wc5": true,
	"gonka1ddswmmmn38esxegjf6qw36mt4aqyw6etvysy5x": true,
	"gonka10fynmy2npvdvew0vj2288gz8ljfvmjs35lat8n": true,
	"gonka1v8gk5z7gcv72447yfcd2y8g78qk05yc4f3nk4w": true,
	"gonka1gndhek2h2y5849wf6tmw6gnw9qn4vysgljed0u": true,
}

// Client talks to the upstream Gonka API with signed requests.
// It discovers active endpoints from the participant list and routes
// each request to a random endpoint, using the next wallet from the
// pool (round-robin) for signing.
type Client struct {
	sourceURL string
	pool      *wallet.Pool

	mu        sync.RWMutex
	endpoints []Endpoint

	http *http.Client
}

// New creates an upstream Client. sourceURL is a bare node URL
// (e.g. http://node2.gonka.ai:8000) used to discover the participant list.
// The wallet pool is used to round-robin requests across wallets.
func New(sourceURL string, pool *wallet.Pool) *Client {
	return &Client{
		sourceURL: strings.TrimRight(sourceURL, "/"),
		pool:      pool,
		http: &http.Client{
			Timeout: 120 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 100,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// DiscoverEndpoints fetches the active participant list from sourceURL.
// Should be called once at startup and optionally periodically.
func (c *Client) DiscoverEndpoints(ctx context.Context) error {
	url := c.sourceURL + "/v1/epochs/current/participants"
	slog.Info("discovering endpoints", "url", url)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("discover: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("discover: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("discover: status %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		ActiveParticipants struct {
			Participants []struct {
				Index        string `json:"index"`
				InferenceURL string `json:"inference_url"`
			} `json:"participants"`
		} `json:"active_participants"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("discover: decode: %w", err)
	}

	var eps []Endpoint
	for _, p := range result.ActiveParticipants.Participants {
		if p.InferenceURL == "" || p.Index == "" {
			continue
		}
		// Only keep nodes on the Transfer Agent whitelist.
		if !allowedTransferAgents[p.Index] {
			continue
		}
		url := strings.TrimRight(p.InferenceURL, "/") + "/v1"
		eps = append(eps, Endpoint{URL: url, Address: p.Index})
	}

	if len(eps) == 0 {
		return fmt.Errorf("discover: no whitelisted transfer-agent endpoints found in active participants")
	}

	c.mu.Lock()
	c.endpoints = eps
	c.mu.Unlock()

	slog.Info("endpoints discovered", "count", len(eps), "whitelisted", len(allowedTransferAgents))
	return nil
}

// pickEndpoint returns a random active endpoint.
func (c *Client) pickEndpoint() (Endpoint, error) {
	return c.pickEndpointExcluding(nil)
}

// pickEndpointExcluding returns a random endpoint not in the excluded set.
func (c *Client) pickEndpointExcluding(exclude map[string]bool) (Endpoint, error) {
	c.mu.RLock()
	eps := c.endpoints
	c.mu.RUnlock()
	if len(eps) == 0 {
		return Endpoint{}, fmt.Errorf("no endpoints available")
	}
	var candidates []Endpoint
	for _, ep := range eps {
		if !exclude[ep.Address] {
			candidates = append(candidates, ep)
		}
	}
	if len(candidates) == 0 {
		// All candidates exhausted; fall back to any endpoint.
		return eps[rand.Intn(len(eps))], nil
	}
	return candidates[rand.Intn(len(candidates))], nil
}

// FetchModels returns the raw model list from upstream.
func (c *Client) FetchModels(ctx context.Context) ([]json.RawMessage, error) {
	ep, err := c.pickEndpoint()
	if err != nil {
		return nil, err
	}

	w := c.pool.Next()
	resp, err := c.doWith(ctx, ep, w, http.MethodGet, "/models", nil)
	if err != nil {
		return nil, fmt.Errorf("fetch models: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("upstream %d: %s", resp.StatusCode, string(b))
	}

	var result struct {
		Models []json.RawMessage `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode models: %w", err)
	}
	return result.Models, nil
}

// Do sends a signed non-streaming request and returns the full response body.
// It retries up to 3 times on different endpoints if the request fails.
func (c *Client) Do(ctx context.Context, method, path string, payload []byte) ([]byte, int, error) {
	var lastErr error
	tried := map[string]bool{}
	for attempt := 0; attempt < 3; attempt++ {
		ep, err := c.pickEndpointExcluding(tried)
		if err != nil {
			break
		}
		tried[ep.Address] = true
		w := c.pool.Next()
		resp, err := c.doWith(ctx, ep, w, method, path, payload)
		if err != nil {
			slog.Warn("upstream: request failed, retrying with different endpoint", "attempt", attempt+1, "err", err)
			lastErr = err
			continue
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		return b, resp.StatusCode, err
	}
	return nil, 0, lastErr
}

// DoStream sends a signed request and returns the raw *http.Response for streaming.
// It retries up to 3 times on different endpoints. The caller must close resp.Body.
func (c *Client) DoStream(ctx context.Context, method, path string, payload []byte) (*http.Response, error) {
	var lastErr error
	tried := map[string]bool{}
	for attempt := 0; attempt < 3; attempt++ {
		ep, err := c.pickEndpointExcluding(tried)
		if err != nil {
			break
		}
		tried[ep.Address] = true
		w := c.pool.Next()
		resp, err := c.doWithNoTimeout(ctx, ep, w, method, path, payload)
		if err != nil {
			slog.Warn("upstream: stream request failed, retrying with different endpoint", "attempt", attempt+1, "err", err)
			lastErr = err
			continue
		}
		return resp, nil
	}
	return nil, lastErr
}

// doWith executes a signed request against a specific endpoint using the given wallet.
func (c *Client) doWith(ctx context.Context, ep Endpoint, w *wallet.Wallet, method, path string, payload []byte) (*http.Response, error) {
	url := ep.URL + path

	sig, ts := w.Signer.Sign(payload, ep.Address)

	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", sig)
	req.Header.Set("X-Requester-Address", w.Address)
	req.Header.Set("X-Timestamp", fmt.Sprintf("%d", ts))

	slog.Info("upstream request", "method", method, "url", url, "endpoint_addr", ep.Address, "wallet", w.Address)
	return c.http.Do(req)
}

// doWithNoTimeout is like doWith but uses a client without a response-body timeout,
// suitable for streaming.
func (c *Client) doWithNoTimeout(ctx context.Context, ep Endpoint, w *wallet.Wallet, method, path string, payload []byte) (*http.Response, error) {
	url := ep.URL + path

	sig, ts := w.Signer.Sign(payload, ep.Address)

	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", sig)
	req.Header.Set("X-Requester-Address", w.Address)
	req.Header.Set("X-Timestamp", fmt.Sprintf("%d", ts))

	slog.Info("upstream stream request", "method", method, "url", url, "endpoint_addr", ep.Address, "wallet", w.Address)

	// No overall timeout on the client -- streaming responses can run for a long time.
	streamClient := &http.Client{
		Transport: c.http.Transport,
	}
	return streamClient.Do(req)
}
