// claudemore is a thin Anthropic Messages-API recording reverse-proxy that
// FRONTS (never replaces) litellm.
//
// Phase 1 (ka-0000007035 + ka-0000003571, epic ka-0000005323): the binary is a
// strict LIVE pass-through. It forwards every Messages-API request to the
// upstream litellm Service byte-for-byte and streams the response back
// unchanged, so agents pointed at ANTHROPIC_BASE_URL=claudemore behave
// identically to agents pointed straight at litellm. This proves transparency
// (acceptance A1): litellm still sees every request, tags spend by agent, and
// reconciles at session close with zero change. claudemore meters NO spend
// itself — the cost fence (R5 §5.2).
//
// Reversibility (A2): claudemore is selected purely by the account-bead
// `proxy_url` field. Reverting that field falls back to the account's litellm
// base_url within ~60s (next pod spawn). No code change, no redeploy. See the
// rollback runbook at gardenio/docs/claudemore-rollback-runbook.md.
//
// The record path (task 1.2, ka-0000003571) attaches a cassette recorder at the
// proxy's Recorder seam when an S3 bucket is configured (COOP_S3_BUCKET). It
// captures each round-trip to a redacted, canonicalized cassette in S3 (A3/A4)
// WITHOUT altering the bytes forwarded to the client — recording is best-effort
// and non-blocking, so it never affects a request (the cost fence). With no
// bucket configured the recorder is nil and the proxy is a pure pass-through,
// so the default deploy still changes nothing. The cassette schema, request
// canonicalization/hashing, redaction (F16), and S3 store live in the reusable
// internal/claudemore package so the Phase-2 REPLAY path shares them.
// Orchestration (PTY/FIFO/NATS, running the real `claude`) stays in coop —
// claudemore composes with coop, it does not absorb it (§2.1).
//
// Phase 2 (task 2.1, ka-0000005558): CLAUDEMORE_MODE=replay switches the request
// path from the LIVE proxy to a cassette SERVER (replay.go). REPLAY keys each
// request through the same canonicalization+hash the recorder used, serves the
// stored response byte-for-byte at $0 with litellm bypassed entirely, and treats
// a cassette miss as a loud failure (fail_on_miss) rather than falling back to
// LIVE — the property that makes the CI e2e tier $0 and deterministic.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

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

	logger.Info("starting claudemore",
		"version", version,
		"commit", commit,
		"listen", cfg.listenAddr,
		"upstream", cfg.upstreamURL,
		"mode", cfg.mode)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	if err := run(ctx, cfg, logger); err != nil {
		logger.Error("claudemore failed", "error", err)
		os.Exit(1)
	}
}

// run wires the request handler (LIVE proxy or REPLAY server) and HTTP server.
// Separated from main so it can be exercised in tests and to keep os.Exit out of
// the happy path.
func run(ctx context.Context, cfg config, logger *slog.Logger) error {
	handler, cleanup, err := buildHandler(ctx, cfg, logger)
	if err != nil {
		return err
	}
	if cleanup != nil {
		defer cleanup()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"status":"ok","version":%q,"mode":%q}`, version, cfg.mode)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"status":"ok"}`)
	})
	// Everything else is handled by the mode's request path (LIVE proxy to
	// litellm, or REPLAY serve-from-cassette).
	mux.Handle("/", handler)

	srv := &http.Server{
		Addr:    cfg.listenAddr,
		Handler: mux,
		// No write timeout: Messages-API streaming responses can run for
		// minutes. ReadHeaderTimeout guards against slowloris on the header.
		ReadHeaderTimeout: 30 * time.Second,
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		logger.Info("http listen", "addr", cfg.listenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server stopped", "error", err)
			cancel()
		}
	}()

	<-runCtx.Done()
	logger.Info("claudemore shutting down")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = srv.Shutdown(shutdownCtx)
	return nil
}

// buildHandler constructs the mode-specific request handler and an optional
// cleanup func (nil when there is nothing to drain). LIVE builds the pass-through
// proxy (optionally recording); REPLAY builds the cassette server. An unknown
// mode is a loud error so a typo'd CLAUDEMORE_MODE never silently degrades to
// pass-through — the opposite of what a REPLAY CI run wants.
func buildHandler(ctx context.Context, cfg config, logger *slog.Logger) (http.Handler, func(), error) {
	// Normalize defensively: parseConfig already defaults an empty env to LIVE,
	// but a directly-constructed config (tests, embedders) may leave mode blank —
	// an empty mode is LIVE, not an error.
	switch normalizeMode(cfg.mode) {
	case modeLive:
		return buildLiveHandler(ctx, cfg, logger)
	case modeReplay:
		return buildReplayHandler(ctx, cfg, logger)
	default:
		return nil, nil, fmt.Errorf("claudemore: unknown mode %q (want %q or %q)", cfg.mode, modeLive, modeReplay)
	}
}

// buildLiveHandler wires the LIVE pass-through proxy, attaching the cassette
// recorder (Phase-1 task 1.2, ka-0000003571) when an S3 bucket is configured.
// With no bucket — local dev, or a deploy that has not opted into recording —
// the recorder is nil and claudemore stays a strict pass-through with zero
// behavior change (the A1 transparency invariant).
func buildLiveHandler(ctx context.Context, cfg config, logger *slog.Logger) (http.Handler, func(), error) {
	var recorder Recorder
	var cleanup func()
	if cfg.hasCassetteStore() {
		rec, err := buildRecorder(ctx, cfg, logger)
		if err != nil {
			// Recording is best-effort: a broken store must never take down the
			// proxy (that would break the cost fence). Log and run pass-through.
			logger.Error("claudemore: cassette recorder disabled (record path unavailable)", "error", err)
		} else {
			cleanup = rec.Close
			recorder = rec
			logger.Info("claudemore: cassette recorder attached",
				"bucket", cfg.s3Bucket, "cassette_dir", cfg.cassetteDir, "prefix", cfg.s3Prefix)
		}
	} else {
		logger.Info("claudemore: no cassette store configured, running strict pass-through (no record)")
	}

	p, err := newProxy(cfg.upstreamURL, recorder, logger)
	if err != nil {
		if cleanup != nil {
			cleanup()
		}
		return nil, nil, fmt.Errorf("build proxy: %w", err)
	}
	return p, cleanup, nil
}

// buildReplayHandler wires the REPLAY cassette server (Phase-2 task 2.1,
// ka-0000005558). REPLAY REQUIRES a cassette store — there is nothing to serve
// without one — so an empty bucket is a hard error rather than a silent
// pass-through. litellm is never contacted in REPLAY; a cassette miss fails
// loudly (fail_on_miss), which is what makes the CI tier deterministic and $0.
func buildReplayHandler(ctx context.Context, cfg config, logger *slog.Logger) (http.Handler, func(), error) {
	if !cfg.hasCassetteStore() {
		return nil, nil, fmt.Errorf("claudemore: REPLAY mode requires a cassette store (set COOP_S3_BUCKET or CLAUDEMORE_CASSETTE_DIR)")
	}
	store, err := buildCassetteStore(ctx, cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("build cassette store: %w", err)
	}
	logger.Info("claudemore: REPLAY mode — serving cassettes ($0, deterministic, fail_on_miss)",
		"bucket", cfg.s3Bucket, "cassette_dir", cfg.cassetteDir, "prefix", cfg.s3Prefix)
	return newReplayServer(store, logger), nil, nil
}

// Mode constants select claudemore's request path. LIVE (default) is the Phase-1
// pass-through-plus-record posture; REPLAY (Phase-2 task 2.1, ka-0000005558)
// serves cassettes from the store at $0 with litellm bypassed entirely.
const (
	modeLive   = "live"
	modeReplay = "replay"
)

// config holds claudemore's runtime configuration.
type config struct {
	listenAddr  string
	upstreamURL string
	logLevel    string
	// mode is "live" (default) or "replay". LIVE forwards to litellm (and
	// records when an S3 bucket is set). REPLAY serves cassettes from the store
	// deterministically at $0 and NEVER falls back to LIVE — a cassette miss is
	// a loud failure (fail_on_miss), which is the whole point of the CI tier.
	mode string
	// Cassette store config. s3Bucket empty => recording disabled (strict
	// pass-through). The COOP_S3_* names are shared with coop/gb/beads-viewer so
	// cassettes live alongside the other coop S3 artifacts under one convention.
	// In REPLAY the bucket is REQUIRED (there is nothing to serve without it).
	s3Bucket   string
	s3Prefix   string
	s3Region   string
	s3Endpoint string
	// cassetteDir, when non-empty, selects a FILESYSTEM cassette store rooted at
	// that directory INSTEAD of S3 (CLAUDEMORE_CASSETTE_DIR). It exists for two
	// stores-that-share-a-layout reasons:
	//   - Gasbox / BYO (ka-0000005379): the in-box single-EC2 product has no S3;
	//     a local dir lets claudemore REPLAY there with zero cloud deps.
	//   - The Phase-2 hooks e2e (ka-0000004452) records+replays the REAL `claude`
	//     binary against a temp dir hermetically in CI (no S3/MinIO, $0).
	// When set it takes precedence over the S3 bucket for BOTH record and replay,
	// and satisfies REPLAY's "a store is configured" requirement.
	cassetteDir string
}

// hasCassetteStore reports whether any cassette store is configured — either the
// S3 bucket or the filesystem directory. REPLAY requires this to be true (there
// is nothing to serve otherwise); LIVE uses it to decide whether to attach the
// recorder.
func (c config) hasCassetteStore() bool {
	return c.s3Bucket != "" || c.cassetteDir != ""
}

func parseConfig() config {
	return config{
		listenAddr:  envOr("CLAUDEMORE_LISTEN", ":8100"),
		upstreamURL: envOr("CLAUDEMORE_UPSTREAM_URL", "http://gardenio-litellm:4000"),
		logLevel:    envOr("LOG_LEVEL", "info"),
		mode:        normalizeMode(os.Getenv("CLAUDEMORE_MODE")),
		s3Bucket:    strings.TrimSpace(os.Getenv("COOP_S3_BUCKET")),
		s3Prefix:    strings.TrimSpace(os.Getenv("COOP_S3_PREFIX")),
		s3Region:    strings.TrimSpace(os.Getenv("COOP_S3_REGION")),
		s3Endpoint:  strings.TrimSpace(os.Getenv("COOP_S3_ENDPOINT")),
		cassetteDir: strings.TrimSpace(os.Getenv("CLAUDEMORE_CASSETTE_DIR")),
	}
}

// normalizeMode lower-cases + trims the mode env, defaulting to LIVE. An
// unrecognized value falls through to run()'s validation, which fails loudly
// rather than silently defaulting a typo'd "replya" to pass-through.
func normalizeMode(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" {
		return modeLive
	}
	return v
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func setupLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
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
