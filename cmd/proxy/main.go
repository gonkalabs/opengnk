package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gonkalabs/gonka-proxy-go/internal/api"
	"github.com/gonkalabs/gonka-proxy-go/internal/config"
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

	client := upstream.New(cfg.SourceURL, pool)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := client.DiscoverEndpoints(ctx); err != nil {
		slog.Error("endpoint discovery failed", "err", err)
		cancel()
		os.Exit(1)
	}
	cancel()

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

	handler := api.New(client, cfg.SimulateToolCalls, san)

	mux := http.NewServeMux()
	handler.Register(mux)

	srv := &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      mux,
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

	slog.Info("starting proxy server",
		"addr", cfg.ListenAddr,
		"wallets", pool.Len(),
		"toolSim", cfg.SimulateToolCalls,
		"sanitize", cfg.SanitizeEnabled,
	)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
}
