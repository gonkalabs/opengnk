// Package quality measures inference quality axes on every request passing
// through the opengnk proxy and exposes a per-epoch summary via /quality/stats.
//
// Axes tracked:
//
//	L6 — Cache reuse rate (X-Cache: HIT response header from Gonka DAPI)
//	L8 — Latency consistency (CV = stddev/mean across requests)
//	L9 — Completion rate (HTTP 2xx vs 4xx/5xx)
//	DX — Explicit feedback loop (X-Inference-Feedback request header)
//
// Intended as the measurement foundation for GiP #860 (Inference Quality
// Axis Registry). See https://github.com/gonka-ai/gonka/discussions/860
package quality

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// EpochSummary is returned by GET /quality/stats.
type EpochSummary struct {
	TotalRequests      int64   `json:"total_requests"`
	CacheHits          int64   `json:"cache_hits"`
	CacheMisses        int64   `json:"cache_misses"`
	HitRate            float64 `json:"hit_rate"`
	AvgLatencyMs       float64 `json:"avg_latency_ms"`
	LatencyCV          float64 `json:"latency_cv"`
	CompletionRate     float64 `json:"completion_rate"`
	FeedbackResolved   int64   `json:"feedback_resolved"`
	FeedbackUnresolved int64   `json:"feedback_unresolved"`
}

// Middleware measures quality axes on every proxied request.
type Middleware struct {
	hits        atomic.Int64
	misses      atomic.Int64
	total       atomic.Int64
	completions atomic.Int64
	feedbackOK  atomic.Int64
	feedbackNo  atomic.Int64

	mu        sync.Mutex
	latencies []float64
}

// New returns a Middleware ready for use.
func New() *Middleware {
	return &Middleware{}
}

// Wrap returns an http.Handler that measures every request then delegates to next.
func (m *Middleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		m.total.Add(1)

		rec := &recorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)

		latencyMs := float64(time.Since(start).Milliseconds())
		m.mu.Lock()
		m.latencies = append(m.latencies, latencyMs)
		m.mu.Unlock()

		if rec.status >= 200 && rec.status < 400 {
			m.completions.Add(1)
		}

		if rec.Header().Get("X-Cache") == "HIT" {
			m.hits.Add(1)
		} else {
			m.misses.Add(1)
		}

		if raw := r.Header.Get("X-Inference-Feedback"); raw != "" {
			var fb struct {
				Outcome string `json:"outcome"`
			}
			if json.Unmarshal([]byte(raw), &fb) == nil {
				if fb.Outcome == "resolved" {
					m.feedbackOK.Add(1)
				} else {
					m.feedbackNo.Add(1)
				}
			}
		}
	})
}

// Stats returns a snapshot of the current epoch summary.
func (m *Middleware) Stats() EpochSummary {
	total := m.total.Load()
	hits := m.hits.Load()
	misses := m.misses.Load()
	completions := m.completions.Load()

	var hitRate, completionRate, avgLat, cv float64
	if total > 0 {
		hitRate = float64(hits) / float64(total)
		completionRate = float64(completions) / float64(total)
	}

	m.mu.Lock()
	lats := make([]float64, len(m.latencies))
	copy(lats, m.latencies)
	m.mu.Unlock()

	if len(lats) > 0 {
		var sum float64
		for _, l := range lats {
			sum += l
		}
		avgLat = sum / float64(len(lats))

		if avgLat > 0 && len(lats) > 1 {
			var sqDiff float64
			for _, l := range lats {
				d := l - avgLat
				sqDiff += d * d
			}
			stddev := math.Sqrt(sqDiff / float64(len(lats)-1))
			cv = stddev / avgLat
		}
	}

	return EpochSummary{
		TotalRequests:      total,
		CacheHits:          hits,
		CacheMisses:        misses,
		HitRate:            hitRate,
		AvgLatencyMs:       avgLat,
		LatencyCV:          cv,
		CompletionRate:     completionRate,
		FeedbackResolved:   m.feedbackOK.Load(),
		FeedbackUnresolved: m.feedbackNo.Load(),
	}
}

// StatsHandler returns an http.Handler for GET /quality/stats.
func (m *Middleware) StatsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(m.Stats())
	})
}

// CanonicalPromptHash returns sha256 of the canonical JSON encoding of messages.
// Used for L1 exact-match cache key — identical to the PromptHash in Gonka DAPI.
func CanonicalPromptHash(messages []map[string]string) string {
	b, _ := json.Marshal(messages)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

type recorder struct {
	http.ResponseWriter
	status int
}

func (r *recorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the underlying ResponseWriter so SSE streaming works
// through the quality middleware.
func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
