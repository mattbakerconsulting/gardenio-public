// Command edge-auth is the gardenio-edge-auth service: a non-AVA edge that
// fronts in-cluster daemons (kbeads, coopmux, ...) with GitLab-PAT-rooted
// auth so CLI tools (kd, coop) can drive the daemons from outside the
// cluster without going through AVA's browser-only OIDC flow.
//
// This file is the W-B-7 scaffold (ka-0000002895): bootstrap, config,
// signal handling, and health endpoints only. PAT validation, group
// membership cache, reverse proxy, and OCSF audit logging land in
// follow-up tasks (W-B-8..W-B-13) under epic ka-0000003384 per
// gardenio/plans/stage3-ava-cli-eb-rollout.md.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"gardenio/controller/internal/covflush"
)

var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	defer covflush.Flush()

	cfg := parseConfig()
	logger := setupLogger(cfg.logLevel)
	logger.Info("starting edge-auth",
		"version", version,
		"commit", commit,
		"listen_addr", cfg.listenAddr,
	)

	srv := newServer(cfg, version, logger)

	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	httpSrv := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Metrics server on a SEPARATE listener (default :9092) so the Prometheus
	// surface — gardenio_forwardauth_403_total et al. — is scraped in-cluster
	// and never exposed on the public auth edge that Traefik fronts. Mirrors
	// the controller's metrics-server pattern (cmd/controller/main.go).
	metricsAddr := envOrDefault("METRICS_LISTEN_ADDR", ":9092")
	metricsMux := http.NewServeMux()
	metricsMux.Handle("GET /metrics", promhttp.HandlerFor(srv.metrics.registry, promhttp.HandlerOpts{}))
	metricsSrv := &http.Server{
		Addr:              metricsAddr,
		Handler:           metricsMux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	go func() {
		logger.Info("HTTP server listening", "addr", cfg.listenAddr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP server failed", "error", err)
		}
	}()

	go func() {
		logger.Info("metrics server listening", "addr", metricsAddr)
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics server failed", "error", err)
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down edge-auth")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("HTTP server shutdown error", "error", err)
	}
	if err := metricsSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("metrics server shutdown error", "error", err)
	}
}

func setupLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
