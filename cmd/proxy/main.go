package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gonkalabs/gonka-proxy-go/internal/api"
	"github.com/gonkalabs/gonka-proxy-go/internal/config"
	"github.com/gonkalabs/gonka-proxy-go/internal/quality"
	"github.com/gonkalabs/gonka-proxy-go/internal/sanitize"
	"github.com/gonkalabs/gonka-proxy-go/internal/sanitize/llmclassifier"
	"github.com/gonkalabs/gonka-proxy-go/internal/sanitize/ner"
	"github.com/gonkalabs/gonka-proxy-go/internal/signer"
	"github.com/gonkalabs/gonka-proxy-go/internal/upstream"
	"github.com/gonkalabs/gonka-proxy-go/internal/wallet"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	cfg, err := config.Load()
	if err != nil {
		slog.Error("config error", "err", err)
		os.Exit(1)
	}

	// Build the upstream client based on the configured mode.
	var client upstream.Upstream
	switch cfg.UpstreamMode {
	case "devshard":
		client = upstream.NewDevshardClient(upstream.DevshardConfig{
			BaseURL: cfg.GatewayURL,
			APIKey:  cfg.GatewayAPIKey,
		})
		slog.Info("using devshard gateway upstream (external)",
			"gateway_url", cfg.GatewayURL,
			"api_key_set", cfg.GatewayAPIKey != "",
		)

	case "devshard-embedded":
		// In embedded mode the proxy assumes a devshardctl container is running
		// alongside it (via docker-compose.devshard.yml). The proxy connects to
		// it over the internal Docker network.
		gwURL := cfg.GatewayURL
		if gwURL == "" {
			gwURL = "http://devshardctl:8080/v1"
		}
		gwKey := cfg.GatewayAPIKey
		if gwKey == "" {
			slog.Error("devshard-embedded mode requires GATEWAY_API_KEY (the devshard gateway's API key)")
			os.Exit(1)
		}
		client = upstream.NewDevshardClient(upstream.DevshardConfig{
			BaseURL: gwURL,
			APIKey:  gwKey,
		})
		slog.Info("using devshard gateway upstream (embedded)",
			"gateway_url", gwURL,
			"targets", cfg.DevshardTargets,
		)

	default: // "node"
		// Legacy ECDSA-signed requests to Gonka nodes.
		var wallets []wallet.Wallet
		for i, wc := range cfg.Wallets {
			s, err := signer.New(wc.PrivateKey)
			if err != nil {
				slog.Error("signer error", "wallet", i+1, "err", err)
				os.Exit(1)
			}
			wallets = append(wallets, wallet.Wallet{
				Signer:  s,
				Address: wc.Address,
			})
		}

		pool, err := wallet.NewPool(wallets)
		if err != nil {
			slog.Error("wallet pool error", "err", err)
			os.Exit(1)
		}

		nodeClient := upstream.New(cfg.SourceURL, pool, cfg.RetryStrategy, cfg.MaxRetries)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := nodeClient.DiscoverEndpoints(ctx); err != nil {
			slog.Error("endpoint discovery failed", "err", err)
			cancel()
			os.Exit(1)
		}
		cancel()

		client = nodeClient
		slog.Info("using Gonka network node upstream (ECDSA)",
			"source_url", cfg.SourceURL,
			"wallets", pool.Len(),
		)
	}

	// Sanitization (mode-agnostic).
	var san *sanitize.Sanitizer
	if cfg.SanitizeEnabled {
		var classifiers []sanitize.Classifier

		if cfg.SanitizeNER {
			classifiers = append(classifiers, ner.New(cfg.SanitizeNERURL))
			slog.Info("sanitize: NER layer enabled", "url", cfg.SanitizeNERURL)
		}
		if cfg.SanitizeLLM {
			classifiers = append(classifiers, llmclassifier.New(
				cfg.SanitizeLLMURL,
				cfg.SanitizeLLMModel,
				cfg.SanitizeLLMThreshold,
			))
			slog.Info("sanitize: LLM layer enabled",
				"url", cfg.SanitizeLLMURL,
				"model", cfg.SanitizeLLMModel,
			)
		}

		san = sanitize.NewWithClassifiers(classifiers)
		slog.Info("sanitization enabled", "classifiers", len(classifiers))
	}

	handler := api.New(client, cfg.SimulateToolCalls, cfg.NativeToolCalls, san)

	qm := quality.New()

	mux := http.NewServeMux()
	handler.Register(mux)
	mux.Handle("GET /quality/stats", qm.StatsHandler())

	srv := &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      qm.Wrap(mux),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 300 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigCh
		slog.Info("shutting down", "signal", sig)

		shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutCancel()

		if err := srv.Shutdown(shutCtx); err != nil {
			slog.Error("shutdown error", "err", err)
		}
	}()

	maxRetriesLabel := "unlimited"
	if cfg.MaxRetries > 0 {
		maxRetriesLabel = fmt.Sprintf("%d", cfg.MaxRetries)
	}
	slog.Info("starting proxy server",
		"addr", cfg.ListenAddr,
		"upstream_mode", cfg.UpstreamMode,
		"toolSim", cfg.SimulateToolCalls,
		"nativeToolCalls", cfg.NativeToolCalls,
		"sanitize", cfg.SanitizeEnabled,
		"retryStrategy", cfg.RetryStrategy,
		"maxRetries", maxRetriesLabel,
	)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
}
