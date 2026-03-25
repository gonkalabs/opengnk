package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

// RetryStrategy controls which node is used when retrying a failed request.
type RetryStrategy string

const (
	// RetrySameNode retries the request on the same node that failed.
	RetrySameNode RetryStrategy = "same_node"
	// RetryOtherNodes picks a different node for each retry attempt.
	RetryOtherNodes RetryStrategy = "other_nodes"
)

// WalletCfg holds the credentials for a single wallet.
type WalletCfg struct {
	PrivateKey string // hex secp256k1 private key (with or without 0x)
	Address    string // bech32 requester address (derived if empty)
}

// Cfg holds all runtime configuration loaded from environment variables.
type Cfg struct {
	// Wallets holds one or more signing credentials.
	// Populated from GONKA_WALLETS (multi) or GONKA_PRIVATE_KEY (single, backward compat).
	Wallets []WalletCfg

	// Source node URL used to discover active participants.
	// Falls back to GONKA_ENDPOINT for backward compat.
	SourceURL string // e.g. http://node2.gonka.ai:8000

	// Features
	SimulateToolCalls bool // rewrite tool-call requests into plain prompts + parse JSON back
	NativeToolCalls   bool // forward tool_calls natively; normalizes array content for Gonka nodes

	// Sanitization middleware
	SanitizeEnabled bool // SANITIZE=true enables request/response redaction

	// NER sidecar layer
	SanitizeNER    bool   // SANITIZE_NER=true enables NER sidecar
	SanitizeNERURL string // SANITIZE_NER_URL=http://sanitize-ner:8001

	// LLM semantic classifier layer
	SanitizeLLM          bool    // SANITIZE_LLM=true enables LLM classifier
	SanitizeLLMURL       string  // SANITIZE_LLM_URL=http://ollama:11434
	SanitizeLLMModel     string  // SANITIZE_LLM_MODEL=qwen3:4b-instruct-2507-q4_K_M
	SanitizeLLMThreshold float32 // SANITIZE_LLM_THRESHOLD=0 (0 = accept all)

	// Retry behaviour for 429 / 5xx upstream errors.
	// RetryStrategy controls whether to retry on the same node or rotate to others.
	RetryStrategy RetryStrategy // "same_node" or "other_nodes" (default)
	// MaxRetries is the maximum number of retry attempts (0 = unlimited).
	MaxRetries int

	// Server
	ListenAddr string // e.g. :8080
}

// Load reads .env (if present) then environment variables and returns Cfg.
func Load() (*Cfg, error) {
	// Best-effort: load .env from current directory
	_ = godotenv.Load()

	wallets, err := loadWallets()
	if err != nil {
		return nil, err
	}

	// Source URL: prefer GONKA_SOURCE_URL, fall back to GONKA_ENDPOINT
	// (strip /v1 suffix so we have a bare node URL)
	sourceURL := strings.TrimSpace(os.Getenv("GONKA_SOURCE_URL"))
	if sourceURL == "" {
		sourceURL = strings.TrimSpace(os.Getenv("GONKA_ENDPOINT"))
	}
	if sourceURL == "" {
		sourceURL = "http://node2.gonka.ai:8000"
	}
	sourceURL = strings.TrimRight(sourceURL, "/")
	sourceURL = strings.TrimSuffix(sourceURL, "/v1")

	simTools := strings.TrimSpace(os.Getenv("SIMULATE_TOOL_CALLS"))
	simulateToolCalls := simTools == "1" || strings.EqualFold(simTools, "true")

	nativeTools := strings.TrimSpace(os.Getenv("NATIVE_TOOL_CALLS"))
	nativeToolCalls := nativeTools == "1" || strings.EqualFold(nativeTools, "true")

	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = "8080"
	}

	sanitizeRaw := strings.TrimSpace(os.Getenv("SANITIZE"))
	sanitizeEnabled := sanitizeRaw == "1" || strings.EqualFold(sanitizeRaw, "true")

	nerRaw := strings.TrimSpace(os.Getenv("SANITIZE_NER"))
	sanitizeNER := nerRaw == "1" || strings.EqualFold(nerRaw, "true")
	sanitizeNERURL := strings.TrimSpace(os.Getenv("SANITIZE_NER_URL"))
	if sanitizeNERURL == "" {
		sanitizeNERURL = "http://sanitize-ner:8001"
	}

	llmRaw := strings.TrimSpace(os.Getenv("SANITIZE_LLM"))
	sanitizeLLM := llmRaw == "1" || strings.EqualFold(llmRaw, "true")
	sanitizeLLMURL := strings.TrimSpace(os.Getenv("SANITIZE_LLM_URL"))
	if sanitizeLLMURL == "" {
		sanitizeLLMURL = "http://ollama:11434"
	}
	sanitizeLLMModel := strings.TrimSpace(os.Getenv("SANITIZE_LLM_MODEL"))
	if sanitizeLLMModel == "" {
		sanitizeLLMModel = "qwen2.5:0.5b"
	}
	var sanitizeLLMThreshold float32
	if raw := strings.TrimSpace(os.Getenv("SANITIZE_LLM_THRESHOLD")); raw != "" {
		var f float64
		if _, err := fmt.Sscanf(raw, "%f", &f); err == nil {
			sanitizeLLMThreshold = float32(f)
		}
	}

	retryStrategy := RetryOtherNodes
	if raw := strings.TrimSpace(os.Getenv("GONKA_RETRY_STRATEGY")); raw != "" {
		switch strings.ToLower(raw) {
		case "same_node", "same":
			retryStrategy = RetrySameNode
		case "other_nodes", "others":
			retryStrategy = RetryOtherNodes
		default:
			return nil, fmt.Errorf("invalid GONKA_RETRY_STRATEGY %q: must be same_node or other_nodes", raw)
		}
	}

	maxRetries := 0 // unlimited by default
	if raw := strings.TrimSpace(os.Getenv("GONKA_MAX_RETRIES")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid GONKA_MAX_RETRIES %q: must be a non-negative integer", raw)
		}
		maxRetries = n
	}

	return &Cfg{
		Wallets:              wallets,
		SourceURL:            sourceURL,
		SimulateToolCalls:    simulateToolCalls,
		NativeToolCalls:      nativeToolCalls,
		SanitizeEnabled:      sanitizeEnabled,
		SanitizeNER:          sanitizeNER,
		SanitizeNERURL:       sanitizeNERURL,
		SanitizeLLM:          sanitizeLLM,
		SanitizeLLMURL:       sanitizeLLMURL,
		SanitizeLLMModel:     sanitizeLLMModel,
		SanitizeLLMThreshold: sanitizeLLMThreshold,
		RetryStrategy:        retryStrategy,
		MaxRetries:           maxRetries,
		ListenAddr:           ":" + port,
	}, nil
}

// loadWallets builds the wallet list from environment variables.
//
// Multi-wallet format (GONKA_WALLETS):
//
//	GONKA_WALLETS=privkey1:addr1,privkey2:addr2,privkey3
//
// Each entry is "private_key" or "private_key:address" separated by commas.
// The address part is optional and will be derived if omitted.
//
// Single-wallet fallback (backward compat):
//
//	GONKA_PRIVATE_KEY=... GONKA_ADDRESS=...
func loadWallets() ([]WalletCfg, error) {
	multi := strings.TrimSpace(os.Getenv("GONKA_WALLETS"))
	if multi != "" {
		return parseMultiWallets(multi)
	}

	// Fallback: single wallet from GONKA_PRIVATE_KEY
	pk := strings.TrimSpace(os.Getenv("GONKA_PRIVATE_KEY"))
	if pk == "" {
		return nil, fmt.Errorf("either GONKA_WALLETS or GONKA_PRIVATE_KEY must be set")
	}
	addr := strings.TrimSpace(os.Getenv("GONKA_ADDRESS"))
	return []WalletCfg{{PrivateKey: pk, Address: addr}}, nil
}

// parseMultiWallets parses "key1:addr1,key2:addr2,key3" into WalletCfg slices.
func parseMultiWallets(raw string) ([]WalletCfg, error) {
	parts := strings.Split(raw, ",")
	var wallets []WalletCfg
	for i, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// Split on first colon only (private keys may have 0x prefix but no colons)
		var pk, addr string
		if idx := strings.Index(part, ":"); idx >= 0 {
			pk = strings.TrimSpace(part[:idx])
			addr = strings.TrimSpace(part[idx+1:])
		} else {
			pk = part
		}
		if pk == "" {
			return nil, fmt.Errorf("wallet entry %d has empty private key", i+1)
		}
		wallets = append(wallets, WalletCfg{PrivateKey: pk, Address: addr})
	}
	if len(wallets) == 0 {
		return nil, fmt.Errorf("GONKA_WALLETS is set but contains no valid entries")
	}
	return wallets, nil
}
