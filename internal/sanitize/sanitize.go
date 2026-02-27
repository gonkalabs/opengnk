// Package sanitize provides request/response content sanitization for the
// opengnk proxy. It detects sensitive data in outgoing chat messages using
// classifier plugins (NER sidecar, local LLM), replaces each occurrence with
// a stable placeholder token, and restores the originals when the upstream
// response comes back.
//
// Usage:
//
//	s := sanitize.New()
//	body, tm := s.RedactMessages(body)
//	// send body to upstream
//	respBody = s.RestoreBytes(respBody, tm)
package sanitize

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

// globalCounter generates unique token IDs across all requests in the process.
var globalCounter atomic.Uint64

// TokenMap holds the bidirectional mapping for one request lifecycle.
// It is safe to read from multiple goroutines after all Redact calls are done,
// but Redact itself must not be called concurrently.
type TokenMap struct {
	toToken   map[string]string // original value → «TOKEN_XXXX»
	fromToken map[string]string // «TOKEN_XXXX» → original value
}

func newTokenMap() *TokenMap {
	return &TokenMap{
		toToken:   make(map[string]string),
		fromToken: make(map[string]string),
	}
}

// register records a mapping and returns the placeholder token.
// If the original was already registered, the existing token is returned.
func (m *TokenMap) register(original string) string {
	if tok, ok := m.toToken[original]; ok {
		return tok
	}
	id := globalCounter.Add(1)
	tok := fmt.Sprintf("«TOKEN_%06d»", id)
	m.toToken[original] = tok
	m.fromToken[tok] = original
	return tok
}

// Restore replaces all placeholder tokens in text with their original values.
func (m *TokenMap) Restore(text string) string {
	for tok, orig := range m.fromToken {
		text = strings.ReplaceAll(text, tok, orig)
	}
	return text
}

// IsEmpty reports whether no replacements were recorded.
func (m *TokenMap) IsEmpty() bool {
	return len(m.toToken) == 0
}

// Count returns the number of distinct values that were redacted.
func (m *TokenMap) Count() int {
	return len(m.toToken)
}

// Redaction describes a single redacted value for UI display.
type Redaction struct {
	Token    string `json:"token"`    // e.g. «TOKEN_000001»
	Original string `json:"original"` // the actual sensitive value
}

// Redactions returns all recorded replacements, ordered by token name.
// This is used to populate the X-Sanitize-Redactions response header.
func (m *TokenMap) Redactions() []Redaction {
	out := make([]Redaction, 0, len(m.fromToken))
	for tok, orig := range m.fromToken {
		out = append(out, Redaction{Token: tok, Original: orig})
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Token < out[j-1].Token; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// tokenPlaceholderRe matches our own «TOKEN_XXXXXX» markers so we never
// re-redact an already-replaced placeholder.
var tokenPlaceholderRe = regexp.MustCompile(`«TOKEN_\d+»`)

// Sanitizer is the top-level object created once at startup.
type Sanitizer struct {
	classifiers []Classifier
}

// New creates a Sanitizer that relies solely on the provided classifiers.
func New() *Sanitizer {
	return &Sanitizer{}
}

// NewWithClassifiers creates a Sanitizer with an ordered list of classifiers
// (e.g. NER sidecar, LLM classifier).
func NewWithClassifiers(classifiers []Classifier) *Sanitizer {
	return &Sanitizer{classifiers: classifiers}
}

// classifierBudget is the maximum time we wait for all classifiers to finish.
// Classifiers that miss the deadline are skipped; their goroutines keep running
// in the background but their results are discarded.
// Set high enough to cover a small LLM running on CPU.
const classifierBudget = 120 * time.Second

// runClassifiers runs all Classify calls concurrently and merges results.
// Returns after all classifiers finish or classifierBudget elapses.
func (s *Sanitizer) runClassifiers(text string, classifiers []Classifier) []Span {
	if len(classifiers) == 0 {
		return nil
	}

	type result struct {
		spans []Span
	}
	ch := make(chan result, len(classifiers))

	for _, clf := range classifiers {
		go func(c Classifier) {
			spans, err := c.Classify(text)
			if err != nil {
				slog.Warn("sanitize: classifier error", "err", err)
				ch <- result{}
				return
			}
			ch <- result{spans: spans}
		}(clf)
	}

	ctx, cancel := context.WithTimeout(context.Background(), classifierBudget)
	defer cancel()

	var all []Span
	for range classifiers {
		select {
		case r := <-ch:
			all = append(all, r.spans...)
		case <-ctx.Done():
			slog.Warn("sanitize: classifier budget exceeded, using partial results")
			return all
		}
	}
	return all
}

// redactText runs all classifiers concurrently on the original text and
// applies the detected spans as placeholder replacements.
func (s *Sanitizer) redactText(original string, tm *TokenMap) string {
	allSpans := s.runClassifiers(original, s.classifiers)
	if len(allSpans) == 0 {
		return original
	}

	allSpans = validSpans(original, allSpans)
	sortSpansDesc(allSpans)
	allSpans = deduplicateSpans(allSpans)

	text := original
	for _, sp := range allSpans {
		matched := text[sp.Start:sp.End]
		tok := tm.register(matched)
		slog.Debug("sanitize: redacted", "label", sp.Label, "token", tok)
		text = text[:sp.Start] + tok + text[sp.End:]
	}
	return text
}

// redactTextWithNER runs all classifiers except the LLM (always last).
// Used for history messages to avoid paying full LLM latency on old turns.
func (s *Sanitizer) redactTextWithNER(original string, tm *TokenMap) string {
	classifiers := s.classifiers
	// LLM classifier is always appended last; skip it for history messages.
	if len(classifiers) > 1 {
		classifiers = classifiers[:len(classifiers)-1]
	} else {
		classifiers = nil
	}

	allSpans := s.runClassifiers(original, classifiers)
	if len(allSpans) == 0 {
		return original
	}

	allSpans = validSpans(original, allSpans)
	sortSpansDesc(allSpans)
	allSpans = deduplicateSpans(allSpans)

	text := original
	for _, sp := range allSpans {
		tok := tm.register(text[sp.Start:sp.End])
		text = text[:sp.Start] + tok + text[sp.End:]
	}
	return text
}

// wordBoundaryBytes are bytes that delimit tokens/words.
var wordBoundaryBytes = func() [256]bool {
	var t [256]bool
	for _, b := range []byte(" \t\n\r<>(),;[]{}\"'`") {
		t[b] = true
	}
	return t
}()

func isWordBoundaryByte(b byte) bool { return wordBoundaryBytes[b] }

// validSpans filters out spans with invalid offsets, TOKEN placeholders,
// or spans that land in the middle of a larger word (partial NER matches).
func validSpans(text string, spans []Span) []Span {
	out := make([]Span, 0, len(spans))
	for _, sp := range spans {
		if sp.Start < 0 || sp.End > len(text) || sp.Start >= sp.End {
			continue
		}
		if !isRuneBoundary(text, sp.Start) || !isRuneBoundary(text, sp.End) {
			continue
		}
		if tokenPlaceholderRe.MatchString(text[sp.Start:sp.End]) {
			continue
		}
		// Reject partial word matches. If the character immediately before or
		// after the span is not a delimiter, it is a substring of a longer token.
		if sp.Start > 0 && !isWordBoundaryByte(text[sp.Start-1]) {
			continue
		}
		if sp.End < len(text) && !isWordBoundaryByte(text[sp.End]) {
			continue
		}
		out = append(out, sp)
	}
	return out
}

// deduplicateSpans removes overlapping spans (assumes sorted descending by Start).
func deduplicateSpans(spans []Span) []Span {
	out := make([]Span, 0, len(spans))
	lastStart := -1
	for _, sp := range spans {
		if lastStart == -1 || sp.End <= lastStart {
			out = append(out, sp)
			lastStart = sp.Start
		}
	}
	return out
}

func isRuneBoundary(s string, i int) bool {
	if i == 0 || i == len(s) {
		return true
	}
	return s[i]&0xC0 != 0x80
}

func sortSpansDesc(spans []Span) {
	for i := 1; i < len(spans); i++ {
		for j := i; j > 0 && spans[j].Start > spans[j-1].Start; j-- {
			spans[j], spans[j-1] = spans[j-1], spans[j]
		}
	}
}

// RedactMessages parses the OpenAI-format JSON body and redacts sensitive data.
// History messages (all but the last user message) use NER only for speed.
// The last user message runs the full classifier pipeline.
func (s *Sanitizer) RedactMessages(body []byte) ([]byte, *TokenMap) {
	tm := newTokenMap()

	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		redacted := s.redactText(string(body), tm)
		return []byte(redacted), tm
	}

	messagesRaw, ok := req["messages"]
	if !ok {
		return body, tm
	}

	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(messagesRaw, &messages); err != nil {
		return body, tm
	}

	// Find the index of the last user message.
	lastUserIdx := -1
	for i := len(messages) - 1; i >= 0; i-- {
		roleRaw, hasRole := messages[i]["role"]
		if !hasRole {
			continue
		}
		var role string
		if err := json.Unmarshal(roleRaw, &role); err == nil && role == "user" {
			lastUserIdx = i
			break
		}
	}

	changed := false
	for i, msg := range messages {
		contentRaw, ok := msg["content"]
		if !ok {
			continue
		}

		redactFn := s.redactTextWithNER
		if i == lastUserIdx {
			redactFn = s.redactText
		}

		var strContent string
		if err := json.Unmarshal(contentRaw, &strContent); err == nil {
			redacted := redactFn(strContent, tm)
			if redacted != strContent {
				b, _ := json.Marshal(redacted)
				messages[i]["content"] = b
				changed = true
			}
			continue
		}

		// Array content (vision / multi-modal messages).
		var parts []map[string]json.RawMessage
		if err := json.Unmarshal(contentRaw, &parts); err != nil {
			continue
		}
		partsChanged := false
		for j, part := range parts {
			textRaw, ok := part["text"]
			if !ok {
				continue
			}
			var text string
			if err := json.Unmarshal(textRaw, &text); err != nil {
				continue
			}
			redacted := redactFn(text, tm)
			if redacted != text {
				b, _ := json.Marshal(redacted)
				parts[j]["text"] = b
				partsChanged = true
			}
		}
		if partsChanged {
			b, _ := json.Marshal(parts)
			messages[i]["content"] = b
			changed = true
		}
	}

	if !changed {
		return body, tm
	}

	b, _ := json.Marshal(messages)
	req["messages"] = b
	out, err := json.Marshal(req)
	if err != nil {
		return body, tm
	}
	return out, tm
}

// RestoreBytes scans respBody for placeholder tokens and replaces them with
// their original values using the provided TokenMap.
func (s *Sanitizer) RestoreBytes(respBody []byte, tm *TokenMap) []byte {
	if tm == nil || tm.IsEmpty() {
		return respBody
	}
	return []byte(tm.Restore(string(respBody)))
}
