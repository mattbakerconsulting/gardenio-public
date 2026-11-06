// Command landgate is one land's second wall (Lands boundary gd-uYNbyW4a9Z
// §6 step 11, Design B v0): a transparent proxy the edge's land door
// /land/<name>/v1/* forwards to instead of the daemon. It verifies the
// caller's own Keycloak bearer and checks the land's groups, or asks kbeads
// whether a minted land token is this land's (D.18a), checks the edge's route
// denylist again, and forwards the bearer untouched to kbeads, which verifies
// and scopes it a third time. It holds no fleet token, no coopmux token and
// no daemon token: a compromised landgate reaches nothing beyond the bearers
// passing through it.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"gardenio/controller/internal/covflush"
	"gardenio/controller/internal/oidcauth"
)

var (
	version = "dev"
	commit  = "unknown"
)

type config struct {
	land        string
	upstream    *url.URL
	listenAddr  string
	oidc        oidcauth.Config
	logLevelRaw string
}

func main() {
	defer covflush.Flush()

	cfg, err := parseConfig(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "landgate:", err)
		os.Exit(2)
	}
	if landDoorRoutesErr != nil {
		fmt.Fprintln(os.Stderr, "landgate:", landDoorRoutesErr)
		os.Exit(2)
	}
	logger := setupLogger(cfg.logLevelRaw).With("land", cfg.land)
	logger.Info("starting landgate", "version", version, "commit", commit,
		"listen_addr", cfg.listenAddr, "upstream", cfg.upstream.String(), "issuer", cfg.oidc.IssuerURL)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	auth := oidcauth.New(ctx, cfg.oidc, logger)
	srv := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           newGate(cfg.land, cfg.upstream, auth.Middleware, logger).handler(auth.Ready),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		logger.Info("HTTP server listening", "addr", cfg.listenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP server failed", "error", err)
			cancel()
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down landgate")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("HTTP server shutdown error", "error", err)
	}
}

// parseConfig reads the environment. OIDC is required: landgate has no
// unauthenticated or dev mode, because a gate that forwards unverified
// requests is the daemon's door with extra steps.
func parseConfig(getenv func(string) string) (config, error) {
	cfg := config{
		land:        getenv("LANDGATE_LAND"),
		listenAddr:  getenv("LANDGATE_LISTEN_ADDR"),
		logLevelRaw: getenv("LOG_LEVEL"),
		oidc: oidcauth.Config{
			IssuerURL:           getenv("OIDC_ISSUER_URL"),
			ClientID:            getenv("OIDC_CLIENT_ID"),
			AllowedEmailDomains: splitComma(getenv("OIDC_ALLOWED_EMAIL_DOMAINS")),
		},
	}
	if cfg.listenAddr == "" {
		cfg.listenAddr = ":8080"
	}
	if !landNamePattern.MatchString(cfg.land) {
		return config{}, fmt.Errorf("LANDGATE_LAND %q is not a land name (%s)", cfg.land, landNamePattern)
	}
	raw := getenv("LANDGATE_UPSTREAM")
	if raw == "" {
		return config{}, errors.New("LANDGATE_UPSTREAM is required (the beads daemon's HTTP URL)")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return config{}, fmt.Errorf("LANDGATE_UPSTREAM %q is not an http(s) URL", raw)
	}
	cfg.upstream = u
	if cfg.oidc.IssuerURL == "" || cfg.oidc.ClientID == "" {
		return config{}, errors.New("OIDC_ISSUER_URL and OIDC_CLIENT_ID are required")
	}
	if err := oidcauth.Validate(cfg.oidc, oidcauth.ValidateOptions{}); err != nil {
		return config{}, err
	}
	return cfg, nil
}

func splitComma(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
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
