package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gonkalabs/gonka-proxy-go/internal/sanitize"
	"github.com/gonkalabs/gonka-proxy-go/internal/toolsim"
	"github.com/gonkalabs/gonka-proxy-go/internal/upstream"
)

// Handler implements all HTTP endpoints.
type Handler struct {
	client            *upstream.Client
	simulateToolCalls bool
	sanitizer         *sanitize.Sanitizer // nil when sanitization is disabled

	mu     sync.RWMutex
	models []json.RawMessage // cached raw model objects from upstream
}

// New creates a Handler and kicks off initial model loading.
// Pass a non-nil sanitizer to enable request/response sanitization.
func New(client *upstream.Client, simulateToolCalls bool, san *sanitize.Sanitizer) *Handler {
	h := &Handler{
		client:            client,
		simulateToolCalls: simulateToolCalls,
		sanitizer:         san,
	}
	go h.loadModels()
	return h
}

// Register mounts routes on the given mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /health", h.health)
	mux.HandleFunc("GET /v1/models", h.listModels)
	mux.HandleFunc("POST /v1/chat/completions", h.chatCompletions)
	mux.HandleFunc("GET /", h.serveUI)
}

// ---------- endpoints ----------

func (h *Handler) health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (h *Handler) listModels(w http.ResponseWriter, _ *http.Request) {
	h.mu.RLock()
	models := h.models
	h.mu.RUnlock()

	type modelEntry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}

	var entries []modelEntry
	for _, raw := range models {
		var m struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw, &m) == nil && m.ID != "" {
			entries = append(entries, modelEntry{
				ID:      m.ID,
				Object:  "model",
				Created: 1677610602,
				OwnedBy: "gonka",
			})
		}
	}
	if len(entries) == 0 {
		entries = []modelEntry{{
			ID:      "gonka-model",
			Object:  "model",
			Created: 1677610602,
			OwnedBy: "gonka",
		}}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   entries,
	})
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "failed to read body: "+err.Error())
		return
	}
	defer r.Body.Close()

	// Redact sensitive data from outgoing messages.
	var tm *sanitize.TokenMap
	if h.sanitizer != nil {
		body, tm = h.sanitizer.RedactMessages(body)
		if tm != nil && !tm.IsEmpty() {
			slog.Info("sanitize: redacted tokens in request", "count", tm.Count())
		}
	}

	// Check if tool simulation is needed.
	if h.simulateToolCalls && toolsim.NeedsSimulation(body) {
		h.toolSimResponse(w, r, body, tm)
		return
	}

	// Peek at stream flag
	var peek struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &peek)

	slog.Info("chat completions", "stream", peek.Stream, "bodyLen", len(body))

	if peek.Stream {
		h.streamResponse(w, r, body, tm)
	} else {
		h.nonStreamResponse(w, r, body, tm)
	}
}

// toolSimResponse handles requests with tools by rewriting the prompt,
// sending a non-stream request, and converting the response back.
func (h *Handler) toolSimResponse(w http.ResponseWriter, r *http.Request, body []byte, tm *sanitize.TokenMap) {
	rewritten, tools, _, err := toolsim.RewriteRequest(body)
	if err != nil {
		slog.Error("toolsim rewrite error", "err", err)
		writeErr(w, http.StatusBadRequest, "tool simulation rewrite failed: "+err.Error())
		return
	}

	slog.Info("toolsim: sending rewritten request", "bodyLen", len(rewritten))

	// Always use non-streaming for tool simulation so we can parse the full response.
	respBody, status, err := h.client.Do(r.Context(), http.MethodPost, "/chat/completions", rewritten)
	if err != nil {
		slog.Error("toolsim upstream error", "err", err)
		writeErr(w, http.StatusBadGateway, "upstream error: "+err.Error())
		return
	}

	if status >= 400 {
		slog.Error("toolsim upstream status", "code", status, "body", string(respBody))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(respBody)
		return
	}

	// Extract model from request for response.
	var peek struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &peek)

	// Try to parse tool calls from the response.
	result := toolsim.ParseResponse(respBody, tools, peek.Model)

	// Restore any redacted tokens before returning to the client.
	if h.sanitizer != nil && tm != nil {
		result = h.sanitizer.RestoreBytes(result, tm)
	}

	setSanitizeHeader(w, tm)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(result)
}

func (h *Handler) nonStreamResponse(w http.ResponseWriter, r *http.Request, body []byte, tm *sanitize.TokenMap) {
	respBody, status, err := h.client.Do(r.Context(), http.MethodPost, "/chat/completions", body)
	if err != nil {
		slog.Error("upstream error", "err", err)
		writeErr(w, http.StatusBadGateway, "upstream error: "+err.Error())
		return
	}

	// Restore any redacted tokens before returning to the client.
	if h.sanitizer != nil && tm != nil {
		respBody = h.sanitizer.RestoreBytes(respBody, tm)
	}

	setSanitizeHeader(w, tm)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(respBody)
}

func (h *Handler) streamResponse(w http.ResponseWriter, r *http.Request, body []byte, tm *sanitize.TokenMap) {
	resp, err := h.client.DoStream(r.Context(), http.MethodPost, "/chat/completions", body)
	if err != nil {
		slog.Error("upstream stream error", "err", err)
		writeErr(w, http.StatusBadGateway, "upstream error: "+err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(resp.Body)
		slog.Error("upstream stream status", "code", resp.StatusCode, "body", string(errBody))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(errBody)
		return
	}

	// SSE headers
	setSanitizeHeader(w, tm)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		slog.Warn("response writer does not support flushing")
	}

	// Wrap the response body with a restoring reader when sanitization is on.
	src := sanitize.NewRestoringReader(resp.Body, tm)

	buf := make([]byte, 4096)
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			_, writeErr := w.Write(buf[:n])
			if writeErr != nil {
				slog.Error("client write error", "err", writeErr)
				return
			}
			if ok {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				slog.Error("upstream read error", "err", readErr)
			}
			return
		}
	}
}

func (h *Handler) serveUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, "web/index.html")
}

// setSanitizeHeader encodes the redaction list into the X-Sanitize-Redactions
// response header so the web UI can display what was redacted and restored.
// The JSON is base64-encoded so UTF-8 characters (like «TOKEN») survive
// HTTP header transmission without corruption.
// It is a no-op when tm is nil or empty.
func setSanitizeHeader(w http.ResponseWriter, tm *sanitize.TokenMap) {
	if tm == nil || tm.IsEmpty() {
		return
	}
	b, err := json.Marshal(tm.Redactions())
	if err != nil {
		return
	}
	w.Header().Set("X-Sanitize-Redactions", base64.StdEncoding.EncodeToString(b))
}

// ---------- helpers ----------

func (h *Handler) loadModels() {
	for attempt := 1; attempt <= 3; attempt++ {
		models, err := h.client.FetchModels(context.Background())
		if err != nil {
			slog.Warn("model load failed", "attempt", attempt, "err", err)
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
			continue
		}
		h.mu.Lock()
		h.models = models
		h.mu.Unlock()
		slog.Info("models loaded", "count", len(models))
		return
	}
	slog.Error("could not load models after retries")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
