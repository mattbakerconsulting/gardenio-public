// Command github-bridge is the GitHub counterpart of gitlab-bridge. This first
// slice is the front door only (plan gd-j5OxXb3ySp, spec gd-RwzAblfqeU):
//
//   - POST /webhook receives GitHub App deliveries. Every request is verified
//     with X-Hub-Signature-256 (HMAC-SHA256, constant-time), failing closed when
//     no secret is configured, then de-duplicated on X-GitHub-Delivery for a TTL.
//   - A token source mints per-repo, bridge-scoped installation tokens using the
//     same githubapp client the controller uses for agent tokens.
//   - /healthz, /readyz and a separate /metrics listener.
//
// With a beads daemon configured, verified events fan out to handlers that map
// them onto beads: pull_request (pullrequest.go: mr_* fields, thread status
// line, agent mail), check_suite/workflow_run (pipeline.go) and the review
// events (review.go: thread quote, agent mail, mr_has_review_comments). A
// squawk poster (squawk.go) subscribes to the bead event stream and posts an
// agent's `--to-mr` squawks as PR comments. The resolver (resolver.go) sweeps
// open PRs and respawns a worker for unresolved review threads when the PR's
// agent is gone. The release recorder (release.go) writes a project's release
// record from a tag webhook and a 5-minute tag poll. The conflict scanner
// (conflicts.go) writes each open PR's merge state against main onto its beads.
// Every event is also logged.
//
// This is the retirement path for the PAT cliff of 2026-10-19: webhook auth is
// the App's shared secret and API auth is an installation token, neither a PAT.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/covflush"
	"gardenio/controller/internal/githubapp"
)

var (
	version = "dev"
	commit  = "unknown"
)

type config struct {
	listenAddr  string
	metricsAddr string
	logLevel    string

	webhookSecret []byte
	replayTTL     time.Duration
	replayMax     int

	// GitHub App. All optional: without an app id and key the bridge still
	// receives and verifies webhooks, it just cannot mint tokens.
	appID          string
	privateKeyFile string
	installationID int64
	apiURL         string

	// Beads daemon. Optional: without it events are logged but not mapped onto
	// beads. Same variable names as gitlab-bridge.
	beadsHTTPAddr string
	beadsToken    string

	// Agent resolver (resolver.go). On by default when a beads daemon and a
	// GitHub App are configured; RESOLVER=off disables it.
	resolver    resolverConfig
	resolverOff bool

	// draftAgentPRs converts a crew PR opened as ready back to a draft
	// (GITHUB_BRIDGE_DRAFT_AGENT_PRS, default false). Needs the GitHub App.
	draftAgentPRs bool

	// Release recorder (release.go). On when a beads daemon, a GitHub App and
	// the machine principal (BEADS_OIDC_*) are configured; RELEASE_RECORDER=off
	// disables it. RELEASE_POLL_INTERVAL is the tag poll's period.
	releaseOff          bool
	releasePollInterval time.Duration

	// CI-wait resolver (ciwait.go). On whenever a beads daemon is configured
	// (it needs no GitHub App); CI_WAIT_RESOLVER=off disables it.
	// CI_WAIT_QUIESCE and CI_WAIT_SWEEP_INTERVAL override the defaults. Its
	// Actions poll fallback (ciwait_poll.go) runs when a GitHub App is
	// configured; CI_WAIT_POLL_INTERVAL and CI_WAIT_POLL_GRACE tune it.
	ciWaitOff          bool
	ciWaitQuiesce      time.Duration
	ciWaitSweep        time.Duration
	ciWaitPollInterval time.Duration
	ciWaitPollGrace    time.Duration

	// PR conflict scanner (conflicts.go). On when CONFLICT_SCAN_REPO names a
	// repo, a beads daemon and a GitHub App are configured and git is on PATH;
	// CONFLICT_SCANNER=off disables it.
	conflictScanOff      bool
	conflictScanRepo     string
	conflictScanDir      string
	conflictScanInterval time.Duration
	// Mechanical conflict resolver (resolve.go), on with the scanner;
	// CONFLICT_RESOLVER=off disables it. CONFLICT_RESOLVER_AUTHOR is the
	// "Name <email>" of its merge commits.
	conflictResolveOff    bool
	conflictResolveAuthor string
}

func main() {
	// git runs this binary as GIT_ASKPASS for the conflict scanner's mirror.
	if runAskpass(os.Args, os.Getenv, os.Stdout) {
		return
	}
	defer covflush.Flush()
	cfg, err := parseConfig()
	logger := setupLogger(os.Getenv("LOG_LEVEL"))
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	if err := run(ctx, cfg, logger); err != nil {
		logger.Error("github-bridge exited with error", "error", err)
		os.Exit(1)
	}
}

// bridgeDaemon is everything the webhook handlers ask of the beads daemon;
// *beadsapi.Client satisfies it and the e2e test substitutes an in-memory one.
type bridgeDaemon interface {
	beadClient
	pipelineBeads
}

var _ bridgeDaemon = (*beadsapi.Client)(nil)

// newBeadSink fans each verified delivery out to the bead-mapping handlers,
// after base (the log sink). run() and the e2e test build the chain here so
// the test exercises the production wiring, not a copy of it. files may be
// nil (no GitHub App): merges then record no test or golden counts. drafter
// may be nil (no GitHub App, or GITHUB_BRIDGE_DRAFT_AGENT_PRS off): agent PRs
// opened as ready then stay ready.
func newBeadSink(base EventSink, daemon bridgeDaemon, files prFilesLister, drafter prDrafter, logger *slog.Logger) EventSink {
	pr := newPRHandler(daemon, logger)
	pr.files = files
	pr.drafter = drafter
	return fanoutSink{base, newPipelineHandler(daemon, logger), pr, newReviewHandler(daemon, logger)}
}

// newCIWaitResolver builds the CI-wait resolver from cfg. runs is the Actions
// listing behind the poll fallback; nil (no GitHub App) leaves it webhook-only.
// run() and the wake-on-ci e2e test both build it here.
func newCIWaitResolver(daemon ciWaitBeads, runs ciRunLister, cfg *config, logger *slog.Logger) *ciWaitHandler {
	ciw := newCIWaitHandler(daemon, cfg.ciWaitQuiesce, cfg.ciWaitSweep, logger)
	if runs != nil {
		ciw.runs = runs
		if cfg.ciWaitPollInterval > 0 {
			ciw.pollInterval = cfg.ciWaitPollInterval
		}
		if cfg.ciWaitPollGrace > 0 {
			ciw.pollGrace = cfg.ciWaitPollGrace
		}
	}
	return ciw
}

func parseConfig() (*config, error) {
	cfg := &config{
		listenAddr:     envOr("LISTEN_ADDR", ":8101"),
		metricsAddr:    envOr("METRICS_ADDR", ":9101"),
		logLevel:       os.Getenv("LOG_LEVEL"),
		replayTTL:      24 * time.Hour,
		replayMax:      100_000,
		appID:          strings.TrimSpace(os.Getenv("GITHUB_APP_ID")),
		privateKeyFile: strings.TrimSpace(os.Getenv("GITHUB_APP_PRIVATE_KEY_FILE")),
		apiURL:         strings.TrimSpace(os.Getenv("GITHUB_API_URL")),
		beadsHTTPAddr:  strings.TrimSpace(os.Getenv("BEADS_HTTP_ADDR")),
		beadsToken:     os.Getenv("BEADS_DAEMON_TOKEN"),
	}
	// The secret is read from a file when GITHUB_WEBHOOK_SECRET_FILE is set (the
	// chart's mode: env vars leak via /proc and `kubectl describe`); the env var
	// is the dev fallback.
	if f := strings.TrimSpace(os.Getenv("GITHUB_WEBHOOK_SECRET_FILE")); f != "" {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("read webhook secret file: %w", err)
		}
		cfg.webhookSecret = []byte(strings.TrimRight(string(raw), "\r\n"))
	} else {
		cfg.webhookSecret = []byte(os.Getenv("GITHUB_WEBHOOK_SECRET"))
	}
	if v := os.Getenv("REPLAY_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("REPLAY_TTL %q is not a positive duration", v)
		}
		cfg.replayTTL = d
	}
	if v := os.Getenv("REPLAY_MAX_ENTRIES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("REPLAY_MAX_ENTRIES %q is not a positive integer", v)
		}
		cfg.replayMax = n
	}
	if err := parseResolverConfig(cfg); err != nil {
		return nil, err
	}
	if err := parseConflictScanConfig(cfg); err != nil {
		return nil, err
	}
	if v := strings.TrimSpace(os.Getenv("GITHUB_BRIDGE_DRAFT_AGENT_PRS")); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("GITHUB_BRIDGE_DRAFT_AGENT_PRS %q is not a boolean", v)
		}
		cfg.draftAgentPRs = b
	}
	cfg.releaseOff = strings.EqualFold(strings.TrimSpace(os.Getenv("RELEASE_RECORDER")), "off")
	if v := os.Getenv("RELEASE_POLL_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("RELEASE_POLL_INTERVAL %q is not a positive duration", v)
		}
		cfg.releasePollInterval = d
	}
	cfg.ciWaitOff = strings.EqualFold(strings.TrimSpace(os.Getenv("CI_WAIT_RESOLVER")), "off")
	for _, d := range []struct {
		env string
		dst *time.Duration
	}{
		{"CI_WAIT_QUIESCE", &cfg.ciWaitQuiesce},
		{"CI_WAIT_SWEEP_INTERVAL", &cfg.ciWaitSweep},
		{"CI_WAIT_POLL_INTERVAL", &cfg.ciWaitPollInterval},
		{"CI_WAIT_POLL_GRACE", &cfg.ciWaitPollGrace},
	} {
		if v := os.Getenv(d.env); v != "" {
			dur, err := time.ParseDuration(v)
			if err != nil || dur <= 0 {
				return nil, fmt.Errorf("%s %q is not a positive duration", d.env, v)
			}
			*d.dst = dur
		}
	}
	if v := strings.TrimSpace(os.Getenv("GITHUB_APP_INSTALLATION_ID")); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("GITHUB_APP_INSTALLATION_ID %q is not a positive integer", v)
		}
		cfg.installationID = id
	}
	return cfg, nil
}

// parseResolverConfig reads RESOLVER (off disables), RESOLVER_INTERVAL,
// RESOLVER_COOLDOWN, RESOLVER_MAX_RESPAWNS and RESOLVER_DRY_RUN. Unset values
// fall to resolverConfig.withDefaults; a malformed one is a startup error, not a
// silent default, because these are the respawn-storm brakes.
func parseResolverConfig(cfg *config) error {
	cfg.resolverOff = strings.EqualFold(strings.TrimSpace(os.Getenv("RESOLVER")), "off")
	for _, d := range []struct {
		env string
		dst *time.Duration
	}{{"RESOLVER_INTERVAL", &cfg.resolver.Interval}, {"RESOLVER_COOLDOWN", &cfg.resolver.Cooldown}} {
		if v := os.Getenv(d.env); v != "" {
			dur, err := time.ParseDuration(v)
			if err != nil || dur <= 0 {
				return fmt.Errorf("%s %q is not a positive duration", d.env, v)
			}
			*d.dst = dur
		}
	}
	if v := os.Getenv("RESOLVER_MAX_RESPAWNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return fmt.Errorf("RESOLVER_MAX_RESPAWNS %q is not a positive integer", v)
		}
		cfg.resolver.MaxRespawns = n
	}
	cfg.resolver.DryRun = strings.EqualFold(strings.TrimSpace(os.Getenv("RESOLVER_DRY_RUN")), "true")
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// buildTokenSource returns nil (and no error) when no App is configured.
func buildTokenSource(cfg *config) (*tokenSource, error) {
	if cfg.appID == "" && cfg.privateKeyFile == "" {
		return nil, nil
	}
	if cfg.appID == "" || cfg.privateKeyFile == "" {
		return nil, errors.New("GITHUB_APP_ID and GITHUB_APP_PRIVATE_KEY_FILE must be set together")
	}
	key, err := os.ReadFile(cfg.privateKeyFile)
	if err != nil {
		return nil, fmt.Errorf("read app private key: %w", err)
	}
	client, err := githubapp.New(githubapp.Config{AppID: cfg.appID, PrivateKey: key, BaseURL: cfg.apiURL})
	if err != nil {
		return nil, err
	}
	return newTokenSource(client, cfg.installationID, nil), nil
}

// newAgentPRDrafter returns the drafter when GITHUB_BRIDGE_DRAFT_AGENT_PRS is
// on and a GitHub App is configured, else nil (agent PRs stay as opened).
func newAgentPRDrafter(cfg *config, tokens *tokenSource) prDrafter {
	if !cfg.draftAgentPRs || tokens == nil {
		return nil
	}
	return newGitHubDrafter(tokens, cfg.apiURL)
}

// bridgeMachineRole is the realm role that covers every bridge daemon call but
// the release writes (kbeads machine_grants.go, gd-tCMXlS9fXW).
const bridgeMachineRole = "gardenio-machine-github-bridge"

// newReleaseRecorderOn returns the release recorder, or nil when it is off
// or the machine principal ts is nil (the daemon refuses a release write on the
// static token).
func newReleaseRecorderOn(cfg *config, daemon *beadsapi.Client, ts beadsapi.TokenSource, tokens *tokenSource, logger *slog.Logger) (*releaseRecorder, error) {
	if cfg.releaseOff {
		logger.Info("release recorder disabled (RELEASE_RECORDER=off)")
		return nil, nil
	}
	if ts == nil {
		logger.Warn("release recorder disabled: no machine principal (BEADS_OIDC_*), and the daemon refuses release writes on the static token")
		return nil, nil
	}
	writer, err := beadsapi.New(beadsapi.Config{HTTPAddr: cfg.beadsHTTPAddr, TokenSource: ts})
	if err != nil {
		return nil, fmt.Errorf("create release writer client: %w", err)
	}
	writer.SetSystemOperator(bridgeActor)
	rel := newReleaseRecorder(releaseDaemon{Daemon: daemon, writer: writer}, daemon, newGitHubCommenter(tokens, cfg.apiURL), cfg.releasePollInterval, logger)
	rel.deploys = writer
	logger.Info("release recorder enabled", "client", os.Getenv("BEADS_OIDC_CLIENT_ID"), "poll_interval", rel.interval)
	return rel, nil
}

// run starts the webhook and metrics listeners and blocks until ctx is done. A
// missing webhook secret is a startup error, not just a per-request 403: a pod
// that cannot authenticate anything should crashloop visibly.
func run(ctx context.Context, cfg *config, logger *slog.Logger) error {
	if len(cfg.webhookSecret) == 0 {
		return errors.New("webhook secret is not configured (GITHUB_WEBHOOK_SECRET_FILE or GITHUB_WEBHOOK_SECRET)")
	}
	tokens, err := buildTokenSource(cfg)
	if err != nil {
		return fmt.Errorf("github app: %w", err)
	}
	logger.Info("starting github-bridge",
		"version", version, "commit", commit,
		"listen_addr", cfg.listenAddr, "metrics_addr", cfg.metricsAddr,
		"replay_ttl", cfg.replayTTL, "app_configured", tokens != nil,
		"installation_pinned", cfg.installationID != 0)

	reg := prometheus.NewRegistry()
	sink := EventSink(logSink{logger: logger})
	if cfg.beadsHTTPAddr != "" {
		var closeDaemon func() error
		sink, closeDaemon, err = wireDaemon(ctx, cfg, sink, tokens, reg, logger)
		if err != nil {
			return err
		}
		defer closeDaemon()
	} else {
		logger.Warn("BEADS_HTTP_ADDR not set: events are logged only, no bead updates")
	}
	return serve(ctx, cfg, sink, reg, logger)
}

// wireDaemon connects to the beads daemon, chains the daemon-backed sinks onto
// sink and starts their background loops. The returned func closes the daemon
// client; the caller runs it once the listeners are down.
func wireDaemon(ctx context.Context, cfg *config, sink EventSink, tokens *tokenSource, reg *prometheus.Registry, logger *slog.Logger) (EventSink, func() error, error) {
	// The machine principal, when configured, authenticates every daemon
	// call and the SSE stream (gd-tCMXlS9fXW): the daemon refuses the
	// shared static token. A fetch failure fails the call closed
	// (auth_unavailable), never falling back to BEADS_DAEMON_TOKEN, which
	// is only the rollback the chart renders with the principal off. A
	// half-set BEADS_OIDC_* is a startup error.
	machineTS, _, err := beadsapi.MachineTokenSourceFromEnv(ctx, logger, beadsapi.WithExpectedRoles(releaseRecorderRole, bridgeMachineRole))
	if err != nil {
		return nil, nil, fmt.Errorf("machine principal: %w", err)
	}
	if machineTS == nil {
		logger.Warn("no machine principal (BEADS_OIDC_*): daemon calls use BEADS_DAEMON_TOKEN")
	}
	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: cfg.beadsHTTPAddr, Token: cfg.beadsToken, TokenSource: machineTS})
	if err != nil {
		return nil, nil, fmt.Errorf("create beads daemon client: %w", err)
	}
	var files prFilesLister
	if tokens != nil {
		files = newGitHubCommenter(tokens, cfg.apiURL)
	}
	drafter := newAgentPRDrafter(cfg, tokens)
	switch {
	case drafter != nil:
		logger.Info("agent PRs opened as ready are converted to drafts")
	case cfg.draftAgentPRs:
		logger.Warn("GITHUB_BRIDGE_DRAFT_AGENT_PRS is on but no GitHub App is configured: agent PRs stay ready")
	}
	sink = newBeadSink(sink, daemon, files, drafter, logger)
	if !cfg.ciWaitOff {
		// First in the chain: it never fails a delivery, and a later
		// sink's error must not hide a run from the ledger.
		var runs ciRunLister
		if tokens != nil {
			runs = newGitHubCommenter(tokens, cfg.apiURL)
		}
		ciw := newCIWaitResolver(daemon, runs, cfg, logger)
		sink = fanoutSink{ciw, sink}
		logger.Info("ci wait resolver enabled", "quiesce", ciw.quiesce, "sweep", ciw.sweep,
			"poll_fallback", ciw.runs != nil, "poll_interval", ciw.pollInterval, "poll_grace", ciw.pollGrace)
		go ciw.Run(ctx)
	}
	if tokens != nil {
		// Squawks ride the bead event stream, not a webhook: an agent's
		// closed `gb squawk --to-mr` bead becomes a comment on its PR.
		go newSquawkPoster(daemon, newGitHubCommenter(tokens, cfg.apiURL), logger).Run(ctx, daemon)
		if !cfg.resolverOff {
			res := newResolver(daemon, newGraphQLThreads(tokens, cfg.apiURL), cfg.resolver, logger)
			logger.Info("agent resolver enabled",
				"interval", res.cfg.Interval, "cooldown", res.cfg.Cooldown,
				"max_respawns", res.cfg.MaxRespawns, "dry_run", res.cfg.DryRun)
			go res.Run(ctx)
		}
		if rel, err := newReleaseRecorderOn(cfg, daemon, machineTS, tokens, logger); err != nil {
			daemon.Close()
			return nil, nil, err
		} else if rel != nil {
			sink = fanoutSink{sink, rel}
			go rel.Run(ctx)
		}
		if cs := newConflictScannerFromConfig(cfg, daemon, tokens, reg, logger); cs != nil {
			sink = fanoutSink{sink, cs}
			go cs.Run(ctx)
		}
	} else {
		logger.Warn("no GitHub App configured: agent squawks will not be posted as PR comments")
	}
	return sink, daemon.Close, nil
}

// serve runs the webhook and metrics listeners until ctx is done or one fails,
// then shuts both down.
func serve(ctx context.Context, cfg *config, sink EventSink, reg *prometheus.Registry, logger *slog.Logger) error {
	deliveries := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gardenio_github_webhook_deliveries_total",
		Help: "GitHub webhook requests by terminal outcome.",
	}, []string{"outcome"})
	reg.MustRegister(deliveries)

	replay := newReplayCache(cfg.replayTTL, cfg.replayMax, nil)
	mux := http.NewServeMux()
	mux.Handle("/webhook", newWebhookHandler(webhookConfig{
		Secret:  cfg.webhookSecret,
		Sink:    sink,
		Replay:  replay,
		Logger:  logger,
		Observe: func(o string) { deliveries.WithLabelValues(o).Inc() },
	}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","version":%q}`, version)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"ok"}`)
	})
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))

	servers := []*http.Server{
		{Addr: cfg.listenAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 60 * time.Second, WriteTimeout: 30 * time.Second},
		{Addr: cfg.metricsAddr, Handler: metricsMux, ReadHeaderTimeout: 10 * time.Second},
	}
	errc := make(chan error, len(servers))
	for _, s := range servers {
		go func() {
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("listen %s: %w", s.Addr, err)
			}
		}()
	}

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errc:
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range servers {
		if err := s.Shutdown(shutdownCtx); err != nil {
			logger.Warn("shutdown error", "addr", s.Addr, "error", err)
		}
	}
	return runErr
}

func setupLogger(level string) *slog.Logger {
	var l slog.Level
	switch level {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l}))
}

func init() {
	if v := os.Getenv("VERSION"); v != "" {
		version = v
	}
}
