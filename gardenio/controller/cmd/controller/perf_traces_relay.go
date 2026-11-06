package main

import (
	"context"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gardenio/controller/internal/perftracesrelay"
)

// runPerfTracesRelay is the `controller perf-traces-relay` subcommand: the
// PULL/relay transport that copies perf spans into the in-chart gardenio
// VictoriaTraces (epic ka-0000000598, task ka-0000004875). It reuses the
// controller image rather than introducing a new one, and is launched as its
// own Deployment by the chart (templates/vtsingle/relay-deployment.yaml).
//
// Configuration is entirely env-driven so the chart wires it without flags:
//
//	PERF_TRACES_SOURCE_URL            perf VT base URL (e.g. https://perf.acme.example/victoria-traces)
//	PERF_TRACES_SOURCE_USERNAME_FILE  file with the source basic-auth username (mounted ExternalSecret)
//	PERF_TRACES_SOURCE_PASSWORD_FILE  file with the source basic-auth password
//	PERF_TRACES_SOURCE_USERNAME       inline username (fallback when *_FILE unset)
//	PERF_TRACES_SOURCE_PASSWORD       inline password
//	PERF_TRACES_DEST_URL              in-chart VT base URL (e.g. http://gardenio-victoria-traces:10428)
//	PERF_TRACES_SERVICES              comma-separated service allowlist (empty = discover each cycle)
//	PERF_TRACES_INTERVAL              poll interval (Go duration, default 30s)
//	PERF_TRACES_LOOKBACK_OVERLAP      window overlap to catch late spans (default 1m)
//	PERF_TRACES_INITIAL_LOOKBACK      first-cycle window (default 15m)
//	PERF_TRACES_MAX_TRACES_PER_SERVICE per-service per-cycle trace cap (default 200)
//	PERF_TRACES_HTTP_TIMEOUT          per-request timeout (default 30s)
func runPerfTracesRelay() {
	logger := setupLogger(os.Getenv("LOG_LEVEL"))

	username := readFileOrEnv("PERF_TRACES_SOURCE_USERNAME_FILE", "PERF_TRACES_SOURCE_USERNAME")
	password := readFileOrEnv("PERF_TRACES_SOURCE_PASSWORD_FILE", "PERF_TRACES_SOURCE_PASSWORD")

	cfg := perftracesrelay.Config{
		SourceURL:           strings.TrimRight(os.Getenv("PERF_TRACES_SOURCE_URL"), "/"),
		SourceUsername:      username,
		SourcePassword:      password,
		DestURL:             strings.TrimRight(os.Getenv("PERF_TRACES_DEST_URL"), "/"),
		Services:            parseRelayServices(os.Getenv("PERF_TRACES_SERVICES")),
		Interval:            relayDuration("PERF_TRACES_INTERVAL", 30*time.Second),
		LookbackOverlap:     relayDuration("PERF_TRACES_LOOKBACK_OVERLAP", time.Minute),
		InitialLookback:     relayDuration("PERF_TRACES_INITIAL_LOOKBACK", 15*time.Minute),
		MaxTracesPerService: relayInt("PERF_TRACES_MAX_TRACES_PER_SERVICE", 200),
		HTTPTimeout:         relayDuration("PERF_TRACES_HTTP_TIMEOUT", 30*time.Second),
	}

	if cfg.SourceURL == "" || cfg.DestURL == "" {
		logger.Error("perf-traces-relay requires PERF_TRACES_SOURCE_URL and PERF_TRACES_DEST_URL")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	relay := perftracesrelay.New(cfg, logger)
	if err := relay.Run(ctx); err != nil && ctx.Err() == nil {
		logger.Error("perf-traces-relay exited with error", "error", err)
		os.Exit(1)
	}
	logger.Info("perf-traces-relay stopped")
}

func parseRelayServices(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func relayDuration(env string, fallback time.Duration) time.Duration {
	if v := os.Getenv(env); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

func relayInt(env string, fallback int) int {
	if v := os.Getenv(env); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

// readFileOrEnv returns the trimmed contents of the file named by fileEnv when
// it is set and readable, otherwise the value of valueEnv. The file form is
// preferred so rotated ExternalSecret credentials are picked up without baking
// secrets into the pod env.
func readFileOrEnv(fileEnv, valueEnv string) string {
	if path := os.Getenv(fileEnv); path != "" {
		if b, err := os.ReadFile(path); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return os.Getenv(valueEnv)
}
