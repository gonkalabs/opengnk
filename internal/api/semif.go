package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gonkalabs/gonka-proxy-go/internal/semif"
	"github.com/gonkalabs/gonka-proxy-go/internal/upstream"
)

// semifRuntime is the optional Jev-style scorer. Nil / disabled is the
// default until a DevNet (or dedicated) vLLM is wired.
type semifRuntime struct {
	backend   string // "vllm" or "chat"
	model     string
	chatModel string
	client    *semif.Client
	chat      upstream.Upstream
	timeout   time.Duration
}

func (s *semifRuntime) enabled() bool {
	if s == nil {
		return false
	}
	if s.backend == "vllm" && s.client != nil {
		return true
	}
	return s.backend == "chat" && s.chat != nil
}

func (rt *semifRuntime) publicModel() string {
	if rt == nil {
		return ""
	}
	if rt.backend == "chat" {
		if rt.chatModel != "" {
			return "semif:" + rt.chatModel
		}
		return "semif:chat"
	}
	if rt.model != "" {
		return rt.model
	}
	return semif.DefaultModel
}

// SetSemIf wires the optional /v1/systemone scorer. Safe before Register.
func (h *Handler) SetSemIf(rt *semifRuntime) {
	h.semif = rt
}

// NewSemIfRuntime builds the scorer from env-derived settings. Returns nil
// when neither a dedicated vLLM nor chat fallback is enabled.
func NewSemIfRuntime(baseURL, apiKey, model string, timeoutSec int, chatFallback bool, chatModel string, chat upstream.Upstream) *semifRuntime {
	timeout := time.Duration(timeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	if strings.TrimSpace(model) == "" {
		model = semif.DefaultModel
	}
	rt := &semifRuntime{
		model:     model,
		chatModel: strings.TrimSpace(chatModel),
		timeout:   timeout,
		chat:      chat,
	}
	if strings.TrimSpace(baseURL) != "" {
		rt.backend = "vllm"
		rt.client = semif.NewClient(baseURL, apiKey, model, timeout)
		return rt
	}
	if chatFallback && chat != nil {
		rt.backend = "chat"
		return rt
	}
	return nil
}

func (h *Handler) semifCapability(w http.ResponseWriter, _ *http.Request) {
	enabled := h.semif != nil && h.semif.enabled()
	backend, model := "", ""
	if enabled {
		backend = h.semif.backend
		model = h.semif.publicModel()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object":      "semif.capability",
		"enabled":     enabled,
		"backend":     backend,
		"model":       model,
		"readout":     semif.ReadoutNative,
		"max_options": len(semif.Letters),
		"prompt":      semif.PromptVersion,
		"path":        "/v1/systemone",
		"alias":       "/v1/decisions",
		"note":        "Typed option scores from last-token logprobs. Off unless SEMIF_BASE_URL or SEMIF_CHAT_FALLBACK is set.",
	})
}

func (h *Handler) systemOne(w http.ResponseWriter, r *http.Request) {
	if h.semif == nil || !h.semif.enabled() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]any{
				"message": "semif scorer is not configured (set SEMIF_BASE_URL to a vLLM that exposes logprobs, typically DevNet Qwen3-0.6B)",
				"type":    "api_error",
				"code":    "semif_not_configured",
			},
		})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "failed to read body")
		return
	}
	var req semif.Request
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := semif.Validate(req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx := r.Context()
	if h.semif.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, h.semif.timeout)
		defer cancel()
	}

	started := time.Now()
	result, err := h.scoreSemIf(ctx, req)
	if err != nil {
		slog.Warn("semif score failed", "id", req.ID, "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]any{
				"message": err.Error(),
				"type":    "api_error",
				"code":    "semif_upstream_error",
			},
		})
		return
	}

	out := map[string]any{
		"id":                 result.ID,
		"object":             result.Object,
		"option_ids":         result.OptionIDs,
		"probabilities":      result.Probabilities,
		"option_logits":      result.OptionLogits,
		"chosen_id":          result.ChosenID,
		"chosen_letter":      result.ChosenLetter,
		"input_tokens":       result.InputTokens,
		"completion_tokens":  result.CompletionTokens,
		"total_tokens":       result.TotalTokens,
		"prompt_sha256":      result.PromptSHA256,
		"prompt_version":     result.PromptVersion,
		"model":              result.Model,
		"backend":            result.Backend,
		"readout":            result.Readout,
		"probability_status": result.ProbabilityStatus,
		"duration_ms":        time.Since(started).Milliseconds(),
	}
	if len(result.MissingSlots) > 0 {
		out["missing_slots"] = result.MissingSlots
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) scoreSemIf(ctx context.Context, req semif.Request) (*semif.Result, error) {
	if h.semif.backend == "vllm" && h.semif.client != nil {
		return h.semif.client.Score(ctx, req)
	}
	return h.scoreSemIfChat(ctx, req)
}

func (h *Handler) scoreSemIfChat(ctx context.Context, req semif.Request) (*semif.Result, error) {
	messages, promptHash, err := semif.DirectMessages(req)
	if err != nil {
		return nil, err
	}
	model := h.semif.chatModel
	if model == "" {
		model = strings.TrimSpace(req.Model)
	}
	if model == "" {
		model = "Qwen/Qwen3-0.6B"
	}
	payload, err := semif.ChatBody(model, messages)
	if err != nil {
		return nil, err
	}
	raw, status, err := h.semif.chat.Do(ctx, http.MethodPost, "/chat/completions", payload)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
		return nil, errStatus(status, msg)
	}
	return semif.DecodeScore(req, raw, model, "chat", promptHash)
}

func errStatus(status int, msg string) error {
	if msg == "" {
		return &httpError{status: status, msg: http.StatusText(status)}
	}
	return &httpError{status: status, msg: msg}
}

type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }
