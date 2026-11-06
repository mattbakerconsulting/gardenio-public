// push-bridge is a standalone bead → APNs notification bridge.
//
// It subscribes to the kbeads SSE event stream and, for each new
// decision or escalation bead, sends an APNs alert to the iOS devices of
// the human the bead is addressed to (bridge.PushNotifier). The device
// registry lives in kbeads, one row per install owned by the person who
// registered it; this binary reads it via /v1/devices and writes nothing
// but delete-on-410-Gone pruning. Those device calls authenticate as the
// bridge's own Keycloak service account (machine:gardenio-push-bridge,
// BEADS_OIDC_TOKEN_URL / BEADS_OIDC_CLIENT_ID / BEADS_OIDC_CLIENT_SECRET_FILE):
// kbeads refuses the registry to the static, fleet and per-agent tokens. Bead
// reads and the event stream stay on BEADS_DAEMON_TOKEN.
//
// Running this as its own pod (rather than wedging it into
// slack-bridge) keeps the notification bridges loosely coupled: push
// can scale, redeploy, and take an APNs outage without touching the
// Slack integration, and vice versa.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/covflush"
)

var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	defer covflush.Flush()
	cfg := parseConfig(os.Args[1:])
	logger := setupLogger(cfg.logLevel)

	logger.Info("starting push-bridge",
		"version", version,
		"commit", commit,
		"beads_http", cfg.beadsHTTPAddr,
		"listen_addr", cfg.listenAddr,
		"apns_bundle", cfg.apns.BundleID,
		"apns_production", cfg.apns.Production)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	if err := run(ctx, cfg, logger); err != nil {
		logger.Error("push-bridge failed", "error", err)
		os.Exit(1)
	}
}

// run contains the push-bridge orchestration, separated from main so it
// can be exercised in tests (and to keep os.Exit out of the happy path).
// It returns when ctx is cancelled or a fatal init error occurs.
func run(ctx context.Context, cfg config, logger *slog.Logger) error {
	if !cfg.apns.Enabled() {
		logger.Error("apns: config incomplete — nothing to do",
			"have_key_path", cfg.apns.KeyPath != "",
			"have_key_pem", cfg.apns.KeyPEM != "",
			"have_key_id", cfg.apns.KeyID != "",
			"have_team_id", cfg.apns.TeamID != "",
			"have_bundle_id", cfg.apns.BundleID != "")
		return errAPNsDisabled
	}

	// The device registry answers only machine:gardenio-push-bridge (or an
	// OIDC admin), so without the machine principal every alert would be
	// dropped on a 403. Fail at startup instead, with a clear log line.
	machineTS, machineOK, err := beadsapi.MachineTokenSourceFromEnv(ctx, logger)
	if err != nil {
		return fmt.Errorf("machine principal: %w", err)
	}
	if !machineOK {
		logger.Error("machine principal not configured: the device registry refuses the daemon token",
			"need", "BEADS_OIDC_TOKEN_URL, BEADS_OIDC_CLIENT_ID=gardenio-push-bridge, BEADS_OIDC_CLIENT_SECRET_FILE")
		return errMachinePrincipalMissing
	}
	logger.Info("machine principal enabled", "client", os.Getenv("BEADS_OIDC_CLIENT_ID"))

	daemon, err := beadsapi.New(beadsapi.Config{
		HTTPAddr: cfg.beadsHTTPAddr,
		Token:    os.Getenv("BEADS_DAEMON_TOKEN"),
	})
	if err != nil {
		return fmt.Errorf("beads daemon client: %w", err)
	}
	defer daemon.Close()

	pusher, err := bridge.NewPushNotifier(cfg.apns, deviceRegistry{Client: daemon, machine: machineTS}, logger)
	if err != nil {
		return fmt.Errorf("apns notifier: %w", err)
	}

	sseStream := bridge.NewSSEStream(bridge.SSEStreamConfig{
		BeadsHTTPAddr: cfg.beadsHTTPAddr,
		Topics:        []string{"beads.bead.created", "beads.bead.updated", "beads.bead.closed"},
		Token:         os.Getenv("BEADS_DAEMON_TOKEN"),
		Logger:        logger,
	})

	decisions := bridge.NewDecisions(bridge.DecisionsConfig{
		Daemon:   daemon,
		Notifier: pusher,
		Logger:   logger,
	})
	decisions.RegisterHandlers(sseStream)

	mux := newHealthMux(version)

	srv := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		logger.Info("http listen", "addr", cfg.listenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http server stopped", "error", err)
			cancel()
		}
	}()

	go func() {
		if err := sseStream.Start(runCtx); err != nil && runCtx.Err() == nil {
			logger.Error("sse stream stopped", "error", err)
			cancel()
		}
	}()

	<-runCtx.Done()
	logger.Info("push-bridge shutting down")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = srv.Shutdown(shutdownCtx)
	return nil
}

// errAPNsDisabled is returned by run when APNs is not fully configured.
// Exported as a sentinel so tests can assert on it without string match.
var errAPNsDisabled = fmt.Errorf("apns config incomplete")

// errMachinePrincipalMissing is returned by run when the bridge has no
// machine principal to read the device registry with.
var errMachinePrincipalMissing = fmt.Errorf("machine principal not configured")

// deviceRegistry is the push notifier's daemon: bead reads use the client's
// own token, and the /v1/devices calls carry the bridge's machine principal
// (beadsapi.WithMachinePrincipal), the one non-human identity kbeads lets
// read and prune any person's devices.
type deviceRegistry struct {
	*beadsapi.Client
	machine beadsapi.TokenSource
}

func (d deviceRegistry) ListDevices(ctx context.Context, userEmails []string, platform string) ([]*beadsapi.Device, error) {
	return d.Client.ListDevices(beadsapi.WithMachinePrincipal(ctx, d.machine), userEmails, platform)
}

func (d deviceRegistry) DeleteDevice(ctx context.Context, token string) error {
	return d.Client.DeleteDevice(beadsapi.WithMachinePrincipal(ctx, d.machine), token)
}

// MARK: - Config

type config struct {
	beadsHTTPAddr string
	listenAddr    string
	logLevel      string
	apns          bridge.PushNotifierConfig
}

func parseConfig(args []string) config {
	var cfg config
	fs := flag.NewFlagSet("push-bridge", flag.ExitOnError)
	fs.StringVar(&cfg.beadsHTTPAddr, "beads-http", envOr("BEADS_HTTP_ADDR", "http://localhost:8080"), "beads daemon HTTP URL")
	fs.StringVar(&cfg.listenAddr, "listen", envOr("PUSH_BRIDGE_LISTEN", ":8091"), "HTTP listen address for health probes")
	fs.StringVar(&cfg.logLevel, "log-level", envOr("LOG_LEVEL", "info"), "log level (debug, info, warn, error)")
	_ = fs.Parse(args)

	cfg.apns = bridge.PushNotifierConfig{
		KeyPath:    os.Getenv("APNS_KEY_PATH"),
		KeyPEM:     os.Getenv("APNS_KEY_PEM"),
		KeyID:      os.Getenv("APNS_KEY_ID"),
		TeamID:     os.Getenv("APNS_TEAM_ID"),
		BundleID:   os.Getenv("APNS_BUNDLE_ID"),
		Production: strings.EqualFold(os.Getenv("APNS_PRODUCTION"), "true"),
		GardenURL:  os.Getenv("BEADS_VIEWER_URL"),
	}
	return cfg
}

// newHealthMux returns an HTTP mux with /healthz and /readyz handlers.
// Push dispatch is best-effort — we don't fail readiness because APNs
// is slow.
func newHealthMux(ver string) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"status":"ok","version":%q}`, ver)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"status":"ok"}`)
	})
	return mux
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

// Kept to suppress the unused import warning if we ever drop the SSE
// setup. Leaving the JSON import here guards against future payload
// decoding needs (the existing decisions watcher already handles it).
var _ = json.Unmarshal
