// main.go — tokencut entrypoint: flags → config → pipeline wiring → HTTP
// server with graceful shutdown. `--mock-upstream` swaps the upstream for a
// built-in reference solver so the full pipeline (compress → forward →
// stream back) demos offline with zero API keys.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"syscall"
	"time"

	"tokencut/internal/budget"
	"tokencut/internal/compress"
	"tokencut/internal/config"
	"tokencut/internal/mockupstream"
	"tokencut/internal/proxy"
	"tokencut/internal/stats"
	"tokencut/internal/tokenest"
)

const version = "0.1.0"

func main() {
	configPath := flag.String("config", "", "path to config.yaml (default: ./config.yaml if present)")
	mockUpstream := flag.Bool("mock-upstream", false, "use the built-in offline mock upstream (demo/test mode)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("tokencut", version)
		return
	}

	cfg := config.Default()
	if *configPath != "" {
		loaded, err := config.Load(*configPath)
		if err != nil {
			fatal("load config", err)
		}
		cfg = loaded
	} else if _, err := os.Stat("config.yaml"); err == nil {
		loaded, err := config.Load("config.yaml")
		if err != nil {
			fatal("load ./config.yaml", err)
		}
		cfg = loaded
	}
	cfg.ApplyEnv()
	for kind, v := range cfg.TokenKindConstants {
		tokenest.ConfigureKind(kind, v, 0)
	}
	tokenest.ConfigureKind("text", 0, cfg.TokenWidePerToken)

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: parseLevel(cfg.LogLevel)}))

	pricing := map[string]stats.ModelPricing{}
	for k, v := range cfg.Pricing {
		pricing[k] = stats.ModelPricing{Input: v.Input, Output: v.Output}
	}
	rec, err := stats.NewRecorder(version, pricing, cfg.StatsJSONL)
	if err != nil {
		fatal("init stats", err)
	}

	hv := make(map[string]struct{}, len(cfg.HighValueKeys))
	for _, k := range cfg.HighValueKeys {
		hv[k] = struct{}{}
	}
	bcfg := budget.Config{
		Tier:           cfg.Tier,
		Targets:        cfg.Targets,
		ExemptSystem:   cfg.ExemptSystem,
		ExemptLastUser: cfg.ExemptLastUser,
		DiffMode:       cfg.DiffMode,
		LatencyBudget:  time.Duration(cfg.LatencyBudgetMS) * time.Millisecond,
		Prune: compress.JSONPruneConfig{
			LongStringCap: cfg.LongStringCap,
			KeywordSample: cfg.KeywordSample,
			HighValueKeys: hv,
		},
		Drain: compress.DefaultDrainConfig(),
	}
	if cfg.CacheEnabled {
		bcfg.Cache = compress.NewLRU(cfg.CacheCapacity)
	}
	ctrl := budget.NewController(bcfg)

	upstream := cfg.Upstream
	if *mockUpstream {
		srv := httptest.NewServer(mockupstream.Handler())
		defer srv.Close()
		upstream = srv.URL
		logger.Info("mock upstream enabled (offline demo mode)")
	}
	cfg.Upstream = upstream

	srv, err := proxy.New(cfg, ctrl, rec, logger)
	if err != nil {
		fatal("init proxy", err)
	}
	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		logger.Info("shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(ctx)
	}()

	logger.Info("tokencut listening",
		"addr", cfg.Listen, "upstream", upstream, "tier", cfg.Tier, "version", version)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fatal("serve", err)
	}
	rec.Close()
}

func fatal(what string, err error) {
	fmt.Fprintf(os.Stderr, "tokencut: %s: %v\n", what, err)
	os.Exit(1)
}

func parseLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
