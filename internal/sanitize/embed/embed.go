// Package embed is a legacy stub kept for backward compatibility.
// The Classifier interface and Span type now live in the parent sanitize package.
// Classifier implementations are in:
//   - internal/sanitize/ner             (NER sidecar, Natasha + spaCy)
//   - internal/sanitize/llmclassifier   (local LLM via Ollama)
//
// This package may be removed in a future version.
package embed
