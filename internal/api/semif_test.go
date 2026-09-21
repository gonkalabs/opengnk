package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gonkalabs/gonka-proxy-go/internal/semif"
)

func TestSemIfCapabilityDisabled(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()
	h.semifCapability(rec, httptest.NewRequest(http.MethodGet, "/v1/systemone", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["enabled"] != false {
		t.Fatalf("got %+v", got)
	}
}

func TestSystemOneNotConfigured(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()
	h.systemOne(rec, httptest.NewRequest(http.MethodPost, "/v1/systemone", bytes.NewReader([]byte(`{"id":"x"}`))))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSystemOneScoresFromVLLM(t *testing.T) {
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"usage": {"prompt_tokens": 32, "completion_tokens": 1, "total_tokens": 33},
			"choices": [{
				"message": {"content": "B"},
				"logprobs": {"content": [{"token":"B","logprob":-0.05,"top_logprobs":[
					{"token":"A","logprob":-2.0},{"token":"B","logprob":-0.05}
				]}]}
			}]
		}`))
	}))
	defer upstreamSrv.Close()

	h := &Handler{}
	h.SetSemIf(NewSemIfRuntime(upstreamSrv.URL, "", "Qwen/Qwen3-0.6B", 5, false, "", nil))

	body := []byte(`{
		"id":"route-1",
		"state":"Customer cannot access an account after a password reset.",
		"question":"Which queue should handle this request?",
		"options":[
			{"id":"access","description":"Account access support."},
			{"id":"billing","description":"Billing support."}
		]
	}`)
	rec := httptest.NewRecorder()
	h.systemOne(rec, httptest.NewRequest(http.MethodPost, "/v1/systemone", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["chosen_id"] != "billing" || got["chosen_letter"] != "B" {
		t.Fatalf("got %+v", got)
	}
	if got["backend"] != "vllm" || got["input_tokens"] != float64(32) {
		t.Fatalf("got %+v", got)
	}
}

func TestSystemOneRejectsBadRow(t *testing.T) {
	h := &Handler{}
	h.SetSemIf(NewSemIfRuntime("http://127.0.0.1:9", "", "Qwen/Qwen3-0.6B", 1, false, "", nil))
	rec := httptest.NewRecorder()
	h.systemOne(rec, httptest.NewRequest(http.MethodPost, "/v1/systemone", bytes.NewReader([]byte(`{"id":"x","state":"s","question":"q","options":[{"id":"a","description":"A"}]}`))))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

type stubSemIfChat struct {
	body   []byte
	status int
}

func (s stubSemIfChat) Do(context.Context, string, string, []byte) ([]byte, int, error) {
	return s.body, s.status, nil
}
func (stubSemIfChat) DoStream(context.Context, string, string, []byte) (*http.Response, error) {
	return nil, nil
}
func (stubSemIfChat) FetchModels(context.Context) ([]json.RawMessage, error) { return nil, nil }

func TestSystemOneChatFallbackDegraded(t *testing.T) {
	h := &Handler{}
	h.SetSemIf(NewSemIfRuntime("", "", "", 5, true, "Qwen/Qwen3-0.6B", stubSemIfChat{
		status: 200,
		body:   []byte(`{"choices":[{"message":{"content":"A"}}],"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}}`),
	}))
	body := []byte(`{
		"id":"route-1",
		"state":"locked out",
		"question":"Which queue?",
		"options":[
			{"id":"access","description":"Access."},
			{"id":"billing","description":"Billing."}
		]
	}`)
	rec := httptest.NewRecorder()
	h.systemOne(rec, httptest.NewRequest(http.MethodPost, "/v1/systemone", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["chosen_id"] != "access" || got["readout"] != semif.ReadoutDegraded {
		t.Fatalf("got %+v", got)
	}
}

func TestListModelsIncludesSemIf(t *testing.T) {
	h := &Handler{
		models: []json.RawMessage{json.RawMessage(`{"id":"MiniMaxAI/MiniMax-M2.7"}`)},
	}
	h.SetSemIf(NewSemIfRuntime("http://127.0.0.1:9", "", "Qwen/Qwen3-0.6B", 1, false, "", nil))
	rec := httptest.NewRecorder()
	h.listModels(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	var got struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range got.Data {
		if m.ID == "Qwen/Qwen3-0.6B" && m.OwnedBy == "semif" {
			found = true
		}
	}
	if !found {
		t.Fatalf("models = %+v", got.Data)
	}
}
