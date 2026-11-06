// Command grafana-bridge is a standalone service that syncs Grafana dashboards
// authored as beads (type=dashboard) into Grafana via the grafana_dashboard="1"
// ConfigMap provisioning path. It is modeled on the other standalone bridge
// binaries (jira-bridge, gitlab-bridge, slack-bridge): SSE-subscribe to bead
// events, react, and write to an external system.
//
// W2-impl-a (this binary's MVP) covers the beads→Grafana direction:
//   - register the `dashboard` bead type via EnsureGrafanaBridgeConfigs,
//   - write dashboard models to grafana_dashboard="1" ConfigMaps (no Grafana
//     auth needed — the bridge only touches the k8s API; the sidecar does the
//     Grafana write in-pod),
//   - one-time backfill of the chart-shipped dashboards as beads.
//
// W2-impl-b adds the reverse direction: a Grafana→beads read-back poll that
// reflects Grafana-side dashboard edits back into beads, using the
// dashboard_hash + dashboard_source loop-breaker and a configurable conflict
// policy (beads-wins / grafana-wins / manual). It is enabled when a Grafana URL
// is configured (GRAFANA_URL); without one the bridge runs write-only as before.
//
// Design: gardenio/docs/research/obs-w2-grafana-bridge.md (epic ka-0000006776).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/covflush"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	defer covflush.Flush()
	cfg := parseConfig()
	logger := setupLogger(cfg.logLevel)
	logger.Info("starting grafana-bridge",
		"version", version,
		"commit", commit,
		"beads_http", cfg.beadsHTTPAddr,
		"namespace", cfg.namespace,
		"dashboards_dir", cfg.dashboardsDir,
		"backfill", cfg.backfill,
		"project", cfg.project,
		"listen_addr", cfg.listenAddr,
		"grafana_url", cfg.grafanaURL,
		"grafana_token_set", cfg.grafanaToken != "",
		"readback_interval", cfg.readbackInterval,
		"conflict_policy", cfg.conflictPolicy)

	// Beads daemon HTTP client. With the machine principal configured
	// (BEADS_OIDC_* rendered by templates/grafana-bridge/deployment.yaml when
	// grafanaBridge.machinePrincipal.enabled — gd-Hc34EENbO9, decision
	// gd-ZsfxvXRiyv) EVERY write this client makes authenticates as
	// machine:<clientId>: EnsureGrafanaBridgeConfigs, the dashboard backfill and the
	// readback/sync updates. Nothing here acts for a human, so the machine IS
	// the right actor; the daemon attributes operated_by to the machine subject
	// (X-System-Operator is no longer consulted). The SSE stream in run() reads as
	// the same principal (cfg.machineTS), so the chart no longer injects
	// BEADS_DAEMON_TOKEN when it is enabled. A token fetch failure fails the
	// write closed (auth_unavailable); there is no fallback to the static token.
	// The first token is awaited in run() before any seed write (seedStartup).
	machineTS, machineOK, err := beadsapi.MachineTokenSourceFromEnv(context.Background(), logger, beadsapi.WithExpectedRoles("gardenio-machine-config-seed", "gardenio-machine-dashboard-sync"))
	if err != nil {
		logger.Error("machine principal misconfigured", "error", err)
		os.Exit(1)
	}
	daemon, err := newBeadsClient(cfg.beadsHTTPAddr, os.Getenv("BEADS_DAEMON_TOKEN"), machineTS)
	if err != nil {
		logger.Error("failed to create beads daemon client", "error", err)
		os.Exit(1)
	}
	defer daemon.Close()
	cfg.machineTS = machineTS
	if machineOK {
		logger.Info("machine principal enabled", "client", os.Getenv("BEADS_OIDC_CLIENT_ID"))
	} else {
		logger.Info("machine principal disabled")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	// k8s client for the ConfigMap write path.
	k8sClient, err := buildK8sClient(cfg.kubeconfig)
	if err != nil {
		logger.Error("failed to build k8s client", "error", err)
		os.Exit(1)
	}

	store := bridge.NewDashboardStore(daemon, "grafana-bridge")
	writer := bridge.NewGrafanaConfigMapWriter(k8sClient, cfg.namespace, logger)

	// The startup seed runs inside run(), after the health server is listening
	// (so the liveness probe passes while we wait) and before the backfill:
	// first the machine token, then the `dashboard` bead type. See seedStartup.
	seed := seedStartup(machineTS, daemon)

	if err := run(ctx, cfg, logger, store, writer, seed); err != nil {
		logger.Error("grafana-bridge run failed", "error", err)
		os.Exit(1)
	}
}

// newBeadsClient builds the beads daemon client used for every write this
// bridge makes: EnsureGrafanaBridgeConfigs, the dashboard backfill, and the SSE/catch-up
// dashboard refresh.
//
// It sets a system operator ("grafana-bridge") because those writes are TRUE
// machine origin — there is no human on whose behalf a chart-shipped dashboard
// is provisioned. Post-R2 (ka-0000005772) the daemon fail-closes write paths
// when no operator can be resolved, returning HTTP 400 "operator could not be
// resolved"; per the operator directive (decision ka-0000006432) a service like
// this must carry an explicit system operator rather than rely on a removed
// client-wide default — the same treatment slack-bridge/controller document for
// their machine-origin writes (incl. "dashboard refresh"). The daemon prefixes
// "system:" and binds the narrowly-scoped `system:grafana-bridge` sink ONLY when
// no human resolves from OIDC or X-Operated-By, so it is safe to set
// unconditionally. Without it the double-store-removal deploy provisions 0/18
// dashboards (backfill "created:0 failed:18") — bug ka-0000007346.
//
// machineTS, when non-nil, is the bridge's own Keycloak service-account
// credential (beadsapi.MachineTokenSourceFromEnv) and takes precedence over
// token on every request. nil keeps the static token: today's behavior.
func newBeadsClient(httpAddr, token string, machineTS beadsapi.TokenSource) (*beadsapi.Client, error) {
	daemon, err := beadsapi.New(beadsapi.Config{
		HTTPAddr:    httpAddr,
		Token:       token,
		TokenSource: machineTS,
	})
	if err != nil {
		return nil, err
	}
	daemon.SetSystemOperator("grafana-bridge")
	return daemon, nil
}

// run wires the bridge subsystems (backfill, HTTP health server, SSE sync) and
// blocks until ctx is canceled. It is separated from main() so the wiring is
// testable with injected fakes (store/writer) and a pre-canceled context — the
// happy path then returns promptly after standing everything up.
//
// Order matters for the enabling roll of a machine principal (gd-pj5hT3V1la):
//  1. the health server, so /healthz answers while the token is awaited and
//     the liveness probe never restarts a pod that is merely waiting;
//  2. seed: wait for the first machine token (retrying with backoff while the
//     Keycloak operator reconciles the client), then seed the config beads,
//     retrying on auth_unavailable;
//  3. backfill (the pass is rerun on auth_unavailable) and catch-up (its
//     listing is retried on auth_unavailable; the per-bead syncs are
//     idempotent and run again on the next restart or SSE event);
//  4. SSE and read-back; only then /readyz flips to ready.
//
// A nil seed skips step 2 (tests, or a caller that seeded already).
func run(ctx context.Context, cfg *config, logger *slog.Logger, store *bridge.DashboardStore, writer bridge.DashboardWriter, seed startupSeed) error {
	// One project allow-list fences both directions: sync (beads→Grafana) and
	// the store's uid lookups used by backfill and read-back (gd-eGC3fokT8T).
	projects := []string{cfg.project}
	store.SetProjects(projects)
	sync := bridge.NewGrafanaSync(bridge.GrafanaSyncConfig{
		Store:    store,
		Writer:   writer,
		Logger:   logger,
		Projects: projects,
	})

	// HTTP server with health endpoints. It listens before anything that can
	// wait on the daemon or Keycloak, and /readyz stays 503 until the startup
	// sequence below is through, so a bridge waiting for its machine client
	// shows as Running but not Ready instead of being killed by liveness.
	rdy := &readiness{}
	mux := http.NewServeMux()
	registerHealthHandlers(mux, version, rdy)
	srv := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		logger.Info("starting HTTP server", "addr", cfg.listenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP server failed", "error", err)
		}
	}()
	shutdownHTTP := func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("HTTP server shutdown error", "error", err)
		}
	}

	// Startup seed: machine token, then the `dashboard` bead type.
	if seed != nil {
		if err := seed(ctx, logger); err != nil {
			shutdownHTTP()
			if ctx.Err() != nil {
				// Told to stop while still waiting for the credential: that is
				// a normal shutdown, not a failure.
				logger.Info("shutting down grafana-bridge before startup completed", "reason", err)
				return nil
			}
			return fmt.Errorf("startup seed: %w", err)
		}
	}

	// One-time backfill of chart-shipped dashboards (idempotent on uid).
	if cfg.backfill {
		runBackfill(ctx, store, cfg.dashboardsDir, cfg.project, logger)
	}

	// Startup catch-up: the SSE stream below only carries create/update events
	// from "now" (a fresh pod has an empty Last-Event-ID), so a dashboard bead
	// that already existed before this pod started is never synced by SSE alone.
	// After backfill, walk every active type=dashboard bead and drive one sync
	// per bead directly through SyncDashboard — which is a no-op for dashboards
	// already in sync (dashboard_hash loop-breaker) and for beads with no model
	// doc (len(model)==0 skip). This is what makes a fresh cluster bring-up or a
	// bridge restart self-heal instead of stranding pre-existing bead-authored
	// dashboards as Grafana "Dashboard not found" (ka-0000004760).
	if cfg.catchup {
		runCatchup(ctx, sync, store, cfg.catchupLimit, logger)
	}

	// SSE event stream: react to dashboard bead create/update events.
	state, err := bridge.NewStateManager(cfg.statePath)
	if err != nil {
		shutdownHTTP()
		return fmt.Errorf("loading state from %s: %w", cfg.statePath, err)
	}
	dedup := bridge.NewDedup(logger)
	go dedup.StartCleanup(ctx)

	sseStream := bridge.NewSSEStream(bridge.SSEStreamConfig{
		BeadsHTTPAddr: cfg.beadsHTTPAddr,
		Topics:        []string{"beads.bead.created", "beads.bead.updated"},
		Token:         os.Getenv("BEADS_DAEMON_TOKEN"),
		TokenSource:   cfg.machineTS,
		Logger:        logger,
		Dedup:         dedup,
		State:         state,
	})
	sync.RegisterHandlers(sseStream)

	go func() {
		if err := sseStream.Start(ctx); err != nil && ctx.Err() == nil {
			logger.Error("SSE event stream stopped", "error", err)
		}
	}()

	// Grafana→beads read-back poll (W2-impl-b). Only enabled when a Grafana URL
	// is configured; otherwise the bridge runs write-only.
	if cfg.grafanaURL != "" {
		grafana := bridge.NewGrafanaClient(bridge.GrafanaClientConfig{
			BaseURL: cfg.grafanaURL,
			Token:   cfg.grafanaToken,
			Logger:  logger,
		})
		owners, _ := writer.(bridge.DashboardOwnerReader)
		readback := bridge.NewGrafanaReadback(bridge.GrafanaReadbackConfig{
			Store:    store,
			Grafana:  grafana,
			Owners:   owners,
			Policy:   bridge.ParseConflictPolicy(cfg.conflictPolicy),
			Interval: cfg.readbackInterval,
			Logger:   logger,
		})
		go func() {
			if err := readback.Run(ctx); err != nil && ctx.Err() == nil {
				logger.Error("grafana read-back poller stopped", "error", err)
			}
		}()
		logger.Info("grafana read-back poll enabled",
			"grafana_url", cfg.grafanaURL,
			"interval", cfg.readbackInterval,
			"policy", cfg.conflictPolicy)
	} else {
		logger.Info("grafana read-back poll disabled (GRAFANA_URL unset); running write-only")
	}

	rdy.markReady()
	logger.Info("grafana-bridge ready", "namespace", cfg.namespace)

	<-ctx.Done()
	logger.Info("shutting down grafana-bridge")
	shutdownHTTP()
	return nil
}

// startupSeed is the work run() does once the health server listens and
// before the backfill. It returns only when done or when ctx ends.
type startupSeed func(ctx context.Context, logger *slog.Logger) error

// seedStartup builds the production startupSeed: wait for the machine token
// (a no-op when the machine principal is disabled), then register the
// `dashboard` bead type — only the config entries this bridge owns, because
// its machine principal may write no other config title. Both steps retry
// until they succeed or ctx ends.
//
// Why wait for the token first: on the roll that enables the machine
// principal the bridge starts in the same helm upgrade that creates its
// KeycloakClient, and the Keycloak operator reconciles that asynchronously.
// On 2026.287.9 the first client_credentials fetch got invalid_client two
// seconds before the client existed, every seed write failed auth_unavailable,
// and nothing retried until the operator restarted the pod (gd-pj5hT3V1la).
// A chart ordering hook cannot close that gap; waiting for the credential can.
func seedStartup(machineTS beadsapi.TokenSource, daemon bridge.ConfigSetter) startupSeed {
	return func(ctx context.Context, logger *slog.Logger) error {
		if machineTS != nil {
			logger.Info("waiting for the first machine token before seeding")
			if err := beadsapi.WarmMachineToken(ctx, machineTS); err != nil {
				return err
			}
		}
		return retryAuthUnavailable(ctx, logger, "ensure beads configs", func(ctx context.Context) error {
			return bridge.EnsureGrafanaBridgeConfigs(ctx, daemon, logger)
		})
	}
}

// retryBackoffMin/Max bound retryAuthUnavailable's delays: 1s doubling to 30s.
const (
	retryBackoffMin = time.Second
	retryBackoffMax = 30 * time.Second
)

// retrySleep waits d or until ctx ends. Tests replace it.
var retrySleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// retryAuthUnavailable runs op until it succeeds, fails for a reason other
// than the machine credential, or ctx ends. Only beadsapi.IsAuthUnavailable
// failures are retried (with exponential backoff): a daemon 4xx on a seed
// write is a real answer that a retry would not change, while a missing
// machine token is transient on an enabling roll (gd-pj5hT3V1la). Every
// retried failure is logged at Warn so the wait is visible. A non-retried
// failure is logged at Warn and returned; the caller decides whether it is
// fatal (the startup seed treats it as non-fatal, as before).
func retryAuthUnavailable(ctx context.Context, logger *slog.Logger, what string, op func(ctx context.Context) error) error {
	delay := retryBackoffMin
	for attempt := 1; ; attempt++ {
		err := op(ctx)
		if err == nil {
			if attempt > 1 {
				logger.Info(what+" succeeded after retry", "attempts", attempt)
			}
			return nil
		}
		if !beadsapi.IsAuthUnavailable(err) {
			logger.Warn("failed to "+what+" (non-fatal)", "error", err)
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("%s abandoned after %d attempt(s): %w", what, attempt, err)
		}
		logger.Warn(what+" failed: machine credential unavailable; retrying",
			"attempt", attempt, "retry_in", delay, "error", err)
		if serr := retrySleep(ctx, delay); serr != nil {
			return fmt.Errorf("%s abandoned after %d attempt(s): %w", what, attempt, err)
		}
		delay = min(delay*2, retryBackoffMax)
	}
}

// readiness is the /readyz gate: false until run() has finished its startup
// sequence (machine token, config seed, backfill, catch-up, SSE).
type readiness struct {
	ready atomic.Bool
}

func (r *readiness) markReady() { r.ready.Store(true) }

func (r *readiness) isReady() bool {
	return r == nil || r.ready.Load()
}

// config holds parsed environment configuration for the grafana-bridge.
type config struct {
	beadsHTTPAddr string
	machineTS     beadsapi.TokenSource // SSE stream credential; nil = static BEADS_DAEMON_TOKEN
	namespace     string
	kubeconfig    string
	dashboardsDir string // directory of *.json dashboard models to backfill
	backfill      bool
	catchup       bool // sync pre-existing dashboard beads on startup
	catchupLimit  int  // cap on dashboards synced per catch-up pass
	project       string
	listenAddr    string
	logLevel      string
	statePath     string

	// W2-impl-b: Grafana→beads read-back poll. When grafanaURL is empty the
	// poller is disabled and the bridge runs write-only.
	grafanaURL       string
	grafanaToken     string
	readbackInterval time.Duration
	conflictPolicy   string
}

func parseConfig() *config {
	readbackInterval := 60 * time.Second
	if v := os.Getenv("GRAFANA_READBACK_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			readbackInterval = d
		}
	}
	return &config{
		beadsHTTPAddr: envOrDefault("BEADS_HTTP_ADDR", "http://localhost:8080"),
		namespace:     envOrDefault("POD_NAMESPACE", "default"),
		kubeconfig:    os.Getenv("KUBECONFIG"),
		dashboardsDir: os.Getenv("GRAFANA_DASHBOARDS_DIR"),
		backfill:      envBool("GRAFANA_BACKFILL", false),
		catchup:       envBool("GRAFANA_CATCHUP", true),
		catchupLimit:  envInt("GRAFANA_CATCHUP_LIMIT", 256),
		project:       envOrDefault("GRAFANA_PROJECT", "gardenio"),
		listenAddr:    envOrDefault("GRAFANA_LISTEN_ADDR", ":8093"),
		logLevel:      envOrDefault("LOG_LEVEL", "info"),
		statePath:     envOrDefault("STATE_PATH", "/data/grafana-bridge-state.json"),

		grafanaURL:       strings.TrimSpace(os.Getenv("GRAFANA_URL")),
		grafanaToken:     os.Getenv("GRAFANA_TOKEN"),
		readbackInterval: readbackInterval,
		conflictPolicy:   envOrDefault("GRAFANA_CONFLICT_POLICY", "grafana-wins"),
	}
}

// runBackfill reads *.json dashboard models from dir and imports each as a
// dashboard bead. A missing or empty dir is logged and skipped (non-fatal).
func runBackfill(ctx context.Context, store *bridge.DashboardStore, dir, project string, logger *slog.Logger) {
	if dir == "" {
		logger.Warn("backfill requested but GRAFANA_DASHBOARDS_DIR is unset; skipping")
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		logger.Warn("backfill: reading dashboards dir failed; skipping", "dir", dir, "error", err)
		return
	}
	var dashboards []bridge.BackfillDashboard
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names) // deterministic order
	for _, name := range names {
		model, rerr := os.ReadFile(filepath.Join(dir, name))
		if rerr != nil {
			logger.Warn("backfill: reading dashboard file failed", "file", name, "error", rerr)
			continue
		}
		stem := strings.TrimSuffix(name, filepath.Ext(name))
		dashboards = append(dashboards, bridge.BackfillDashboard{Name: stem, Model: model})
	}
	if len(dashboards) == 0 {
		logger.Info("backfill: no dashboard JSON files found", "dir", dir)
		return
	}
	// The pass is idempotent on uid, so when any dashboard failed only because
	// the machine credential was unavailable the whole pass is rerun after a
	// backoff (gd-pj5hT3V1la); dashboards already imported are refreshed, not
	// duplicated.
	var results []bridge.BackfillResult
	_ = retryAuthUnavailable(ctx, logger, "backfill dashboards", func(ctx context.Context) error {
		results = bridge.BackfillDashboards(ctx, store, dashboards, project, logger)
		for _, r := range results {
			if beadsapi.IsAuthUnavailable(r.Err) {
				return r.Err
			}
		}
		return nil
	})
	created, refreshed, failed := 0, 0, 0
	for _, r := range results {
		switch {
		case r.Err != nil:
			failed++
		case r.Created:
			created++
		default:
			refreshed++
		}
	}
	logger.Info("backfill complete", "created", created, "refreshed", refreshed, "failed", failed, "total", len(results))
}

// runCatchup syncs every active type=dashboard bead to Grafana once at startup,
// closing the gap where the SSE stream (which starts from "now") never delivers
// events for dashboards that already existed. Each SyncDashboard call is
// idempotent: it skips beads whose stored dashboard_hash already matches the
// model and beads that have no model doc, so re-running on every restart is
// cheap and safe. Errors are logged per-bead and never abort the pass. limit
// bounds the fan-out so a restart never issues an unbounded burst of daemon
// reads and ConfigMap writes.
func runCatchup(ctx context.Context, sync *bridge.GrafanaSync, store *bridge.DashboardStore, limit int, logger *slog.Logger) {
	if limit <= 0 {
		limit = 256
	}
	var ids []string
	var err error
	_ = retryAuthUnavailable(ctx, logger, "catch-up: list active dashboard beads", func(ctx context.Context) error {
		ids, err = store.ListActiveDashboardIDs(ctx, limit)
		return err
	})
	if err != nil {
		logger.Warn("catch-up: listing active dashboard beads failed; skipping", "error", err)
		return
	}
	if len(ids) == 0 {
		logger.Info("catch-up: no active dashboard beads to sync")
		return
	}
	logger.Info("catch-up: syncing pre-existing dashboard beads", "count", len(ids), "limit", limit)
	for _, id := range ids {
		if ctx.Err() != nil {
			logger.Warn("catch-up: context canceled mid-pass", "error", ctx.Err())
			return
		}
		sync.SyncDashboard(ctx, id)
	}
	logger.Info("catch-up complete", "count", len(ids))
}

func buildK8sClient(kubeconfig string) (kubernetes.Interface, error) {
	var (
		cfg *rest.Config
		err error
	)
	if kubeconfig != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		cfg, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, fmt.Errorf("building k8s config: %w", err)
	}
	return kubernetes.NewForConfig(cfg)
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func envInt(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func setupLogger(level string) *slog.Logger {
	var logLevel slog.Level
	switch level {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	default:
		logLevel = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))
}

func init() {
	if v := os.Getenv("VERSION"); v != "" {
		version = v
	}
}

// registerHealthHandlers wires /healthz and /readyz onto the given mux.
// /healthz is always ok once the server listens (liveness); /readyz answers
// 503 "starting" until rdy is marked ready (a nil rdy is always ready).
func registerHealthHandlers(mux *http.ServeMux, ver string, rdy *readiness) {
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","version":"%s"}`, ver)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !rdy.isReady() {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, `{"status":"starting"}`)
			return
		}
		fmt.Fprintf(w, `{"status":"ok"}`)
	})
}
