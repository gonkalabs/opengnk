package semif

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidateAndMessages(t *testing.T) {
	req := Request{
		ID:       "route-1",
		State:    json.RawMessage(`"Customer cannot access an account after a password reset."`),
		Question: "Which queue should handle this request?",
		Options: []Option{
			{ID: "access", Description: "Account access support."},
			{ID: "billing", Description: "Billing support."},
		},
	}
	msgs, hash, err := DirectMessages(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].Role != "system" || !strings.Contains(msgs[1].Content, `"letter":"A"`) {
		t.Fatalf("messages = %#v", msgs)
	}
	if len(hash) != 64 {
		t.Fatalf("hash = %q", hash)
	}
	if err := Validate(Request{ID: "x", Question: "q", State: json.RawMessage(`{}`), Options: req.Options}); err == nil {
		t.Fatal("expected empty object state to fail")
	}
}

func TestSoftmax(t *testing.T) {
	probs, err := Softmax([]float64{0, 0})
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(probs[0]-0.5) > 1e-9 || math.Abs(probs[1]-0.5) > 1e-9 {
		t.Fatalf("equal logits: %v", probs)
	}
	probs, err = Softmax([]float64{10, 0})
	if err != nil {
		t.Fatal(err)
	}
	if probs[0] <= probs[1] {
		t.Fatalf("expected first to win: %v", probs)
	}
}

func TestExtractChatLogprobs(t *testing.T) {
	body := []byte(`{
		"usage": {"prompt_tokens": 80, "completion_tokens": 1, "total_tokens": 81},
		"choices": [{
			"message": {"content": "A"},
			"logprobs": {
				"content": [{
					"token": "A",
					"logprob": -0.1,
					"top_logprobs": [
						{"token": "A", "logprob": -0.1},
						{"token": " B", "logprob": -2.3}
					]
				}]
			}
		}]
	}`)
	logits, sampled, usage, missing, err := ExtractLogits(body, 2)
	if err != nil {
		t.Fatal(err)
	}
	if sampled != "A" {
		t.Fatalf("sampled = %q", sampled)
	}
	if usage.PromptTokens != 80 || usage.TotalTokens != 81 {
		t.Fatalf("usage = %+v", usage)
	}
	if len(missing) != 0 {
		t.Fatalf("missing = %v", missing)
	}
	if logits[0] != -0.1 || logits[1] != -2.3 {
		t.Fatalf("logits = %v", logits)
	}
}

func TestExtractCompletionsLogprobs(t *testing.T) {
	lp := -0.2
	_ = lp
	body := []byte(`{
		"choices": [{
			"text": "B",
			"logprobs": {
				"tokens": ["B"],
				"token_logprobs": [-0.05],
				"top_logprobs": [{"A": -1.2, "B": -0.05}]
			}
		}]
	}`)
	logits, sampled, _, missing, err := ExtractLogits(body, 2)
	if err != nil {
		t.Fatal(err)
	}
	if sampled != "B" || len(missing) != 0 {
		t.Fatalf("sampled=%q missing=%v", sampled, missing)
	}
	if logits[0] != -1.2 || logits[1] != -0.05 {
		t.Fatalf("logits = %v", logits)
	}
}

func TestDegradedLogits(t *testing.T) {
	logits, missing, err := DegradedLogits(2, "B")
	if err != nil {
		t.Fatal(err)
	}
	if logits[1] != 0 || logits[0] != -20 || len(missing) != 1 || missing[0] != "A" {
		t.Fatalf("logits=%v missing=%v", logits, missing)
	}
}

func TestClientScore(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer testdev" {
			t.Errorf("auth = %s", r.Header.Get("Authorization"))
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload["max_tokens"] != float64(1) || payload["logprobs"] != true {
			t.Fatalf("payload = %#v", payload)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"usage": {"prompt_tokens": 40, "completion_tokens": 1, "total_tokens": 41},
			"choices": [{
				"message": {"content": "A"},
				"logprobs": {"content": [{"token":"A","logprob":-0.2,"top_logprobs":[
					{"token":"A","logprob":-0.2},{"token":"B","logprob":-1.8}
				]}]}
			}]
		}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "testdev", "Qwen/Qwen3-0.6B", 0)
	got, err := c.Score(context.Background(), Request{
		ID:       "route-1",
		State:    json.RawMessage(`"locked out"`),
		Question: "Which queue?",
		Options: []Option{
			{ID: "access", Description: "Access."},
			{ID: "billing", Description: "Billing."},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ChosenID != "access" || got.ChosenLetter != "A" {
		t.Fatalf("chosen = %s %s", got.ChosenID, got.ChosenLetter)
	}
	if got.Backend != "vllm" || got.InputTokens != 40 {
		t.Fatalf("result = %+v", got)
	}
	if got.Probabilities[0] <= got.Probabilities[1] {
		t.Fatalf("probs = %v", got.Probabilities)
	}
}
