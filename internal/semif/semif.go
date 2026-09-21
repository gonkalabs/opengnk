// Package semif implements a Jev-style typed-decision scorer.
//
// Official SemIf (https://github.com/TheoLeeCJ/SemIf) reads option-letter
// logits in one forward pass. This package reproduces that request/response
// contract and approximates the readout from an OpenAI-compatible vLLM
// server via max_tokens=1 + top_logprobs. Subset-softmax of those logprobs
// is equivalent to subset-softmax of the raw logits.
package semif

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"unicode"
)

const (
	Letters          = "ABCDEFGHIJKLMNOP"
	PromptVersion    = "direct-options-v1"
	DirectSystem     = "Apply the supplied criterion to the supplied evidence. Choose exactly one listed option. Respond with only its uppercase letter, with no explanation or reasoning."
	ReadoutNative    = "vLLM last-token logprobs restricted to declared answer slots"
	ReadoutDegraded  = "sampled letter only; upstream omitted option logprobs"
	ProbabilityNote  = "conditional option score; uncalibrated as decision confidence"
	DefaultModel     = "Qwen/Qwen3-0.6B"
	DefaultMaxTokens = 4096
)

// Option is one typed alternative.
type Option struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}

// Request is the SemIf / Jev decision payload.
type Request struct {
	ID       string          `json:"id"`
	State    json.RawMessage `json:"state"`
	Question string          `json:"question"`
	Options  []Option        `json:"options"`
	Model    string          `json:"model,omitempty"`
}

// Usage is the token count returned by the upstream scorer.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Result is a typed decision with option probabilities.
type Result struct {
	ID                string    `json:"id"`
	Object            string    `json:"object"`
	OptionIDs         []string  `json:"option_ids"`
	Probabilities     []float64 `json:"probabilities"`
	OptionLogits      []float64 `json:"option_logits"`
	ChosenID          string    `json:"chosen_id"`
	ChosenLetter      string    `json:"chosen_letter"`
	InputTokens       int       `json:"input_tokens"`
	CompletionTokens  int       `json:"completion_tokens"`
	TotalTokens       int       `json:"total_tokens"`
	PromptSHA256      string    `json:"prompt_sha256"`
	PromptVersion     string    `json:"prompt_version"`
	Model             string    `json:"model"`
	Backend           string    `json:"backend"`
	Readout           string    `json:"readout"`
	ProbabilityStatus string    `json:"probability_status"`
	MissingSlots      []string  `json:"missing_slots,omitempty"`
}

// Message is one chat-template turn.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Validate checks the SemIf row contract (2–16 unique options).
func Validate(req Request) error {
	if strings.TrimSpace(req.ID) == "" {
		return fmt.Errorf("id is required")
	}
	if strings.TrimSpace(req.Question) == "" {
		return fmt.Errorf("question is required")
	}
	if err := validateState(req.State); err != nil {
		return err
	}
	n := len(req.Options)
	if n < 2 || n > len(Letters) {
		return fmt.Errorf("options must contain 2-%d entries", len(Letters))
	}
	seen := make(map[string]struct{}, n)
	for i, opt := range req.Options {
		if strings.TrimSpace(opt.ID) == "" || strings.TrimSpace(opt.Description) == "" {
			return fmt.Errorf("options[%d] needs string id and description", i)
		}
		if _, ok := seen[opt.ID]; ok {
			return fmt.Errorf("option IDs must be unique")
		}
		seen[opt.ID] = struct{}{}
	}
	return nil
}

func validateState(raw json.RawMessage) error {
	if len(bytesTrimSpace(raw)) == 0 || string(raw) == "null" {
		return fmt.Errorf("state must be a nonempty string, object, or array")
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return fmt.Errorf("state must be finite JSON-compatible data")
	}
	switch t := v.(type) {
	case string:
		if strings.TrimSpace(t) == "" {
			return fmt.Errorf("state must be a nonempty string, object, or array")
		}
	case []any:
		if len(t) == 0 {
			return fmt.Errorf("state must be a nonempty string, object, or array")
		}
	case map[string]any:
		if len(t) == 0 {
			return fmt.Errorf("state must be a nonempty object")
		}
	default:
		return fmt.Errorf("state must be a nonempty string, object, or array")
	}
	return nil
}

func bytesTrimSpace(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}

// DirectMessages builds the frozen SemIf prompt.
func DirectMessages(req Request) ([]Message, string, error) {
	if err := Validate(req); err != nil {
		return nil, "", err
	}
	options := make([]map[string]string, len(req.Options))
	for i, opt := range req.Options {
		options[i] = map[string]string{
			"letter":      string(Letters[i]),
			"description": opt.Description,
		}
	}
	var state any
	if err := json.Unmarshal(req.State, &state); err != nil {
		return nil, "", err
	}
	payload := map[string]any{
		"evidence":  state,
		"criterion": req.Question,
		"options":   options,
	}
	user, err := json.Marshal(payload)
	if err != nil {
		return nil, "", err
	}
	msgs := []Message{
		{Role: "system", Content: DirectSystem},
		{Role: "user", Content: string(user)},
	}
	return msgs, digest(DirectSystem + "\n" + string(user)), nil
}

func digest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// Softmax is numerically stable softmax.
func Softmax(values []float64) ([]float64, error) {
	if len(values) < 2 {
		return nil, fmt.Errorf("need at least two finite scores")
	}
	max := values[0]
	for _, v := range values[1:] {
		if !isFinite(v) {
			return nil, fmt.Errorf("need at least two finite scores")
		}
		if v > max {
			max = v
		}
	}
	if !isFinite(max) {
		return nil, fmt.Errorf("need at least two finite scores")
	}
	weights := make([]float64, len(values))
	var total float64
	for i, v := range values {
		if !isFinite(v) {
			return nil, fmt.Errorf("need at least two finite scores")
		}
		w := math.Exp(v - max)
		weights[i] = w
		total += w
	}
	if total == 0 || !isFinite(total) {
		return nil, fmt.Errorf("need at least two finite scores")
	}
	for i := range weights {
		weights[i] /= total
	}
	return weights, nil
}

func isFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

// LetterAt returns the 0-based option letter.
func LetterAt(i int) string {
	if i < 0 || i >= len(Letters) {
		return ""
	}
	return string(Letters[i])
}

// NormalizeSlot folds tokenizer variants ("A", " A", "A\n") into A–P.
func NormalizeSlot(token string) string {
	token = strings.TrimSpace(token)
	token = strings.TrimPrefix(token, "Ġ")
	token = strings.TrimPrefix(token, "▁")
	if token == "" {
		return ""
	}
	r := []rune(token)[0]
	r = unicode.ToUpper(r)
	if r < 'A' || r > 'P' {
		return ""
	}
	return string(r)
}

// Finish builds probabilities, chosen option, and missing-slot metadata.
func Finish(req Request, logits []float64, missing []string, usage Usage, model, backend, readout, promptHash string) (*Result, error) {
	if err := Validate(req); err != nil {
		return nil, err
	}
	if len(logits) != len(req.Options) {
		return nil, fmt.Errorf("logit count %d != option count %d", len(logits), len(req.Options))
	}
	probs, err := Softmax(logits)
	if err != nil {
		return nil, err
	}
	best := 0
	for i, p := range probs {
		if p > probs[best] {
			best = i
		}
	}
	ids := make([]string, len(req.Options))
	for i, opt := range req.Options {
		ids[i] = opt.ID
	}
	total := usage.TotalTokens
	if total == 0 {
		total = usage.PromptTokens + usage.CompletionTokens
	}
	return &Result{
		ID:                req.ID,
		Object:            "semif.decision",
		OptionIDs:         ids,
		Probabilities:     probs,
		OptionLogits:      logits,
		ChosenID:          req.Options[best].ID,
		ChosenLetter:      LetterAt(best),
		InputTokens:       usage.PromptTokens,
		CompletionTokens:  usage.CompletionTokens,
		TotalTokens:       total,
		PromptSHA256:      promptHash,
		PromptVersion:     PromptVersion,
		Model:             model,
		Backend:           backend,
		Readout:           readout,
		ProbabilityStatus: ProbabilityNote,
		MissingSlots:      missing,
	}, nil
}
