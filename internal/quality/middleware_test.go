package quality_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gonkalabs/gonka-proxy-go/internal/quality"
)

func TestCompletionTracking(t *testing.T) {
	m := quality.New()
	handler := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for i := 0; i < 3; i++ {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/chat/completions", nil))
	}

	s := m.Stats()
	if s.TotalRequests != 3 {
		t.Fatalf("want 3 total, got %d", s.TotalRequests)
	}
	if s.CompletionRate != 1.0 {
		t.Fatalf("want completion 1.0, got %f", s.CompletionRate)
	}
}

func TestFailureTracking(t *testing.T) {
	m := quality.New()
	handler := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/chat/completions", nil))

	s := m.Stats()
	if s.CompletionRate != 0.0 {
		t.Fatalf("want completion 0.0, got %f", s.CompletionRate)
	}
}

func TestCacheHitTracking(t *testing.T) {
	m := quality.New()
	handler := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Cache", "HIT")
		w.WriteHeader(http.StatusOK)
	}))

	for i := 0; i < 4; i++ {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/chat/completions", nil))
	}

	s := m.Stats()
	if s.CacheHits != 4 {
		t.Fatalf("want 4 hits, got %d", s.CacheHits)
	}
	if s.HitRate != 1.0 {
		t.Fatalf("want hit_rate 1.0, got %f", s.HitRate)
	}
}

func TestFeedbackTracking(t *testing.T) {
	m := quality.New()
	handler := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("X-Inference-Feedback", `{"outcome":"resolved"}`)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	req2 := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req2.Header.Set("X-Inference-Feedback", `{"outcome":"unresolved"}`)
	handler.ServeHTTP(httptest.NewRecorder(), req2)

	s := m.Stats()
	if s.FeedbackResolved != 1 || s.FeedbackUnresolved != 1 {
		t.Fatalf("want 1/1 feedback, got %d/%d", s.FeedbackResolved, s.FeedbackUnresolved)
	}
}

func TestCanonicalPromptHash(t *testing.T) {
	msgs := []map[string]string{{"role": "user", "content": "hello"}}
	h1 := quality.CanonicalPromptHash(msgs)
	h2 := quality.CanonicalPromptHash(msgs)
	if h1 != h2 {
		t.Fatal("hash not deterministic")
	}
	if len(h1) != 64 {
		t.Fatalf("want 64 hex chars, got %d", len(h1))
	}
}

func TestStatsEndpoint(t *testing.T) {
	m := quality.New()
	handler := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/chat/completions", nil))

	req := httptest.NewRequest("GET", "/quality/stats", nil)
	w := httptest.NewRecorder()
	m.StatsHandler().ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("want application/json, got %s", ct)
	}
}
