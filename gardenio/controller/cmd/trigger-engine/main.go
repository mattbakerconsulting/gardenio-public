// Command trigger-engine is a standalone service that evaluates webhook-based
// trigger beads and dispatches actions (spawn_agent, create_bead, notify,
// apply_formula) when conditions match.
//
// It runs:
//   - HTTP server with /webhooks/{token} endpoint for inbound webhooks
//   - Trigger engine evaluation loop (periodic + event-driven)
//   - Health endpoints (/healthz, /readyz)
//
// External systems (Alertmanager, PagerDuty, VictoriaMetrics, GitHub) POST
// JSON payloads to /webhooks/{token}. The engine matches them against trigger
// conditions and fires configured actions.
//
// This service has ZERO K8s dependencies and can run as a lightweight
// standalone container alongside the gardenio controller.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/covflush"
	"gardenio/controller/internal/scheduler"
	"gardenio/controller/internal/spawnguard"
	"gardenio/controller/internal/steward"
)

// noticeSource identifies trigger-engine on the trigger-notice beads it
// publishes. slack-bridge picks those beads up via SSE and posts them to
// Slack on its own bot credentials — trigger-engine no longer holds a Slack
// token or channel itself.
const noticeSource = "trigger-engine"

var (
	version = "dev"
	commit  = "unknown"
)

// Engine-loop DEADMAN heartbeat (ka-0000005325). The 30h silent-wedge incident
// (2026-08-09→08-10) exposed that a stalled Evaluate() loop is invisible to
// every existing guard: the old /healthz was a STATIC handler (always ok, so
// k8s liveness never restarts a wedged pod), and every engine-side liveness
// sweep (trigger_starve, SweepDarkLaneByConcurrency, SweepGardenerLiveness, …)
// runs INSIDE Evaluate() — so a stalled Evaluate() runs none of them and none
// can detect the loop itself stalling. These two primitives close that blind
// spot the same way gitlab_webhook_deadman.go closes the receive-path one:
//
//   - triggerEngineEvaluations is a Counter incremented once per COMPLETED
//     Evaluate() cycle. `increase(...[N]) == 0 or absent(...)` = the main loop
//     is not evaluating at all — the fleet-wide schedule going dark. Mirrors
//     gardenio_gitlab_webhook_evaluations_total.
//   - lastEvaluateNano records the wall-clock (UnixNano) of the most recent
//     completed Evaluate(), read by the LIVE /healthz handler so k8s liveness
//     can restart a wedged pod (the watchdog go-live the main.go:335 comment
//     anticipated).
var (
	// gardenio_trigger_engine_evaluations_total counts completed engine
	// evaluation cycles. The deadman leg reads this: increase over the window
	// == 0 (or the series absent) means the trigger-engine main loop is not
	// evaluating at all — a wedged/hung loop or a down pod that leaves every
	// scheduled trigger dark while the pod stays Ready with restartCount=0
	// (the exact 30h-blackout signature of ka-0000005325). Registered against
	// the default gatherer (promauto) so buildMetricsServer exposes it on the
	// internal :9092 /metrics endpoint alongside the gardener-loop metrics.
	triggerEngineEvaluations = promauto.NewCounter(prometheus.CounterOpts{
		Name: "gardenio_trigger_engine_evaluations_total",
		Help: "Completed trigger-engine Evaluate() cycles; the deadman heartbeat for the engine's main evaluation loop (ka-0000005325). increase()==0 or absent() ⇒ the schedule source has gone globally dark.",
	})

	// lastEvaluateNano is the wall-clock (UnixNano) of the most recent COMPLETED
	// Evaluate() cycle. Written by recordEvaluateCompleted; read by the LIVE
	// /healthz handler. Seeded to pod startup so a fresh pod gets one grace
	// window before it can be judged wedged — a restart must not immediately
	// 503 itself back into a crash-loop before the first tick completes.
	lastEvaluateNano atomic.Int64
)

// recordEvaluateCompleted beats the engine deadman heartbeat: it increments the
// evaluations counter and stamps the last-Evaluate wall-clock. Called once per
// completed Evaluate() cycle in runTriggerEngine (the ONLY place a full source
// pass completes) — never in the webhook fast-path EvaluateSource, which is a
// single-source partial pass, not the full loop the deadman guards.
func recordEvaluateCompleted(now time.Time) {
	triggerEngineEvaluations.Inc()
	lastEvaluateNano.Store(now.UnixNano())
}

// liveHealthzHandler builds the LIVE /healthz handler (ka-0000005325). It 503s
// when the most recent COMPLETED Evaluate() cycle is older than staleness, so a
// wedged/hung evaluation loop is caught by the k8s liveness probe and the pod
// restarted — the watchdog go-live the metrics-server comment anticipated. The
// old handler was STATIC (always {"status":"ok"}), which is why the 30h schedule
// blackout stayed invisible: liveness never failed, so the pod was never
// restarted. Reads lastEvaluateNano, which runTriggerEngine stamps once per
// completed cycle (and main seeds to startup for one window of boot grace).
//
// While ready is false the staleness check is SKIPPED (gd-niQPoeR7ja). Before
// the startup catch-up lands, the evaluation loop has not started, so there is
// no cycle to be stale — 503ing there would make the kubelet restart a process
// that is correctly waiting on an unreachable daemon, which is precisely the
// crash-loop this bead removes. Readiness, not liveness, is the signal for "up
// but not serving yet": the pod stays alive, out of the Service, and says so.
func liveHealthzHandler(staleness time.Duration, ready *atomic.Bool) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if ready != nil && !ready.Load() {
			fmt.Fprintf(w, `{"status":"ok","state":"bootstrapping","version":"%s"}`, version)
			return
		}
		last := time.Unix(0, lastEvaluateNano.Load())
		age := time.Since(last)
		if age > staleness {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, `{"status":"unhealthy","reason":"evaluation loop stalled","last_evaluate_age_seconds":%d,"threshold_seconds":%d,"version":"%s"}`,
				int64(age.Seconds()), int64(staleness.Seconds()), version)
			return
		}
		fmt.Fprintf(w, `{"status":"ok","last_evaluate_age_seconds":%d,"version":"%s"}`, int64(age.Seconds()), version)
	}
}

// readyzHandler builds the /readyz handler, gated on the startup catch-up load
// (gd-niQPoeR7ja). It fails CLOSED — 503 until the catch-up lands — mirroring
// the beads-viewer OIDC gate (ka-0000001839): the pod stays up and retries, and
// flips ready without a restart once the daemon answers.
//
// Readiness here IS daemon reachability: the catch-up is a full load of every
// trigger source from the beads daemon, so it cannot succeed while the daemon
// is unreachable, and it cannot fail once the daemon is back.
func readyzHandler(ready *atomic.Bool) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, `{"status":"not_ready","reason":"startup_catch_up_pending","version":"%s"}`, version)
			return
		}
		fmt.Fprintf(w, `{"status":"ok","version":"%s"}`, version)
	}
}

// gateUntilReady wraps a handler so it 503s until the startup catch-up has
// landed (gd-niQPoeR7ja). The webhook listener now binds BEFORE the catch-up so
// the kubelet's probes always have someone to talk to; this keeps that early
// bind from letting a webhook fire triggers against empty seen sets, which is
// the thundering-herd the catch-up exists to prevent. Senders (Alertmanager,
// PagerDuty) retry a 503, and the Service does not route to a not-ready pod
// anyway — this is the belt to that braces.
func gateUntilReady(ready *atomic.Bool, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, `{"status":"not_ready","reason":"startup_catch_up_pending"}`)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// catchUpLoader is the slice of *bridge.TriggerEngine the startup gate needs,
// so the gate is unit-testable without standing up a whole engine.
type catchUpLoader interface {
	CatchUpWithRetry(ctx context.Context, cfg bridge.BackoffConfig) error
}

// initialCatchUpBackoff is the capped exponential backoff for the startup
// catch-up load (gd-niQPoeR7ja). MaxAttempts is deliberately 0 — retry until
// the daemon answers or we are shut down. NEVER give up, never exit: an engine
// that exits on a beads blip turns every blip into CrashLoopBackOff, and once
// it holds NATS subscriptions (gd-BBbXQBZTX0) it would drop every buffered
// event on the way out.
func initialCatchUpBackoff() bridge.BackoffConfig {
	return bridge.BackoffConfig{
		Initial: 1 * time.Second,
		Max:     30 * time.Second,
		Factor:  2,
	}
}

// awaitInitialCatchUp blocks until the startup catch-up load succeeds, then
// flips ready. It returns true when the load landed and the caller should start
// the engine; false ONLY when ctx ended first (SIGTERM / shutdown), in which
// case the caller shuts down gracefully without ever starting the loop.
//
// It never returns false because the daemon is down — that case retries here
// forever, with the pod up, /healthz ok, and /readyz 503.
func awaitInitialCatchUp(ctx context.Context, engine catchUpLoader, cfg bridge.BackoffConfig, ready *atomic.Bool, logger *slog.Logger) bool {
	logger.Info("triggers: startup catch-up starting (not ready until it lands)")
	if err := engine.CatchUpWithRetry(ctx, cfg); err != nil {
		logger.Warn("triggers: startup catch-up abandoned — shutting down before the engine loop started",
			"error", err)
		return false
	}
	// Restart the deadman grace window at the moment we go ready, NOT at
	// process start (gd-niQPoeR7ja). main seeds lastEvaluateNano to startup so
	// a fresh pod gets one health-staleness window before /healthz can 503 it
	// (ka-0000005325), but a catch-up that retried through a long outage has
	// already burned that window: without this re-stamp, ready flips with an
	// expired clock, /healthz 503s on the very next probe, and the kubelet
	// restarts a pod that just recovered — the same crash-loop this bead
	// removes, re-entered through liveness instead of exit 0. The engine loop
	// starts right after this returns, so it gets the full window it needs to
	// complete its first Evaluate.
	lastEvaluateNano.Store(time.Now().UnixNano())
	ready.Store(true)
	logger.Info("triggers: startup catch-up complete — engine ready")
	return true
}

// shutdownServers gracefully stops the webhook and metrics listeners. Shared by
// both exits from main (normal SIGTERM, and a shutdown that arrives while the
// startup catch-up is still retrying) so neither path leaks a listener.
func shutdownServers(logger *slog.Logger, srv, metricsSrv *http.Server) {
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("HTTP server shutdown error", "error", err)
	}
	if err := metricsSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("metrics server shutdown error", "error", err)
	}
}

func main() {
	defer covflush.Flush()
	bridge.NudgeSender = "trigger-engine" // from: label on the mail bridge.NudgeAgent sends

	// Handle --help/-h and --version/-v before doing any real work so build
	// verifications (CI, smoke tests) can confirm the binary runs without
	// starting the engine and hanging on a missing beads daemon
	// (ka-0000006573).
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "-h", "--help", "help":
			printUsage(os.Stdout)
			return
		case "-v", "--version", "version":
			fmt.Printf("trigger-engine version=%s commit=%s\n", version, commit)
			return
		}
	}

	cfg := parseConfig()

	logger := setupLogger(cfg.logLevel)
	logger.Info("starting trigger-engine",
		"version", version,
		"commit", commit,
		"beads_http", cfg.beadsHTTPAddr,
		"poll_interval", cfg.triggerPollInterval,
		"listen_addr", cfg.listenAddr)

	// Beads daemon HTTP client. With the machine principal configured
	// (BEADS_OIDC_* rendered by templates/trigger-engine/deployment.yaml when
	// triggerEngine.machinePrincipal.enabled — gd-Hc34EENbO9, decisions
	// gd-ZsfxvXRiyv "tight" and gd-dI7PfsFlvr "lineage") EVERY daemon call this
	// client makes authenticates as machine:<clientId>: trigger stamps, spawns,
	// run-task creates and closes, the sweeps, formula pours. The daemon no
	// longer consults X-Operated-By / X-System-Operator from this client: a
	// spawn or task create keeps the trigger author's human attribution through
	// the trigger_id field the engine already stamps (kbeads dereferences the
	// trigger bead server-side); everything else is attributed to the machine.
	// The SSE stream (buildSSEStreamConfig) reads as the same principal; the
	// chart no longer injects BEADS_DAEMON_TOKEN when it is enabled. A token
	// fetch failure fails the call closed (auth_unavailable); there is no
	// fallback to the static token.
	machineTS, machineOK, err := beadsapi.MachineTokenSourceFromEnv(context.Background(), logger, beadsapi.WithExpectedRoles("gardenio-machine-trigger-ops"))
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
	if machineOK {
		logger.Info("machine principal enabled", "client", os.Getenv("BEADS_OIDC_CLIENT_ID"))
	} else {
		logger.Info("machine principal disabled")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	// Seed the engine-loop deadman clock to startup so a fresh pod gets one
	// health-staleness window of grace before /healthz can 503 it — a restart
	// must not immediately liveness-fail itself back into a crash-loop before
	// the first Evaluate() tick completes (ka-0000005325).
	lastEvaluateNano.Store(time.Now().UnixNano())

	// healthStaleness is how long the engine loop may go without completing an
	// Evaluate() before /healthz reports unhealthy (ka-0000005325). N× the poll
	// interval: at the 30s default this is 300s, comfortably past a slow cycle
	// (daemon list + all sources + sweeps) but far below the 30h blackout that
	// motivated this. k8s liveness then restarts a wedged pod — the watchdog
	// go-live the main.go metrics comment anticipated.
	healthStaleness := time.Duration(cfg.healthStalenessMultiple) * cfg.triggerPollInterval

	// Set up the webhook + schedule + bead-state + multi + schedule-mirror
	// trigger sources and engine. The schedule-mirror source shares its
	// trigger engine with multiSrc so event-driven mirror entries see the
	// same SSE/NATS event buffer (ka-0000008057).
	webhookSrc := bridge.NewWebhookTriggerSource(logger)
	scheduleSrc := bridge.NewScheduleTriggerSource(logger)
	beadSrc := bridge.NewBeadStateTriggerSource(logger)
	subgraphResolver := bridge.NewSubgraphResolver(daemon, bridge.SubgraphResolverConfig{})
	beadSrc.SetSubgraphResolver(subgraphResolver)
	// Watched-list resolver for CEL expression conditions (F4 ka-0000008047):
	// reuses the bounded subgraph closure + the daemon's GetBead for hydration.
	beadSrc.SetWatchedResolver(bridge.NewWatchedResolver(subgraphResolver, daemon, bridge.WatchedResolverConfig{}))
	multiSrc := bridge.NewMultiTriggerSource(logger, nil)
	mirrorSrc := bridge.NewScheduleMirrorTriggerSource(logger, multiSrc.Engine())
	// Account cost/usage trigger source (ka-0000004826): self-contained,
	// polls account beads via the daemon each evaluation cycle.
	accountSrc := bridge.NewAccountTriggerSource(logger)

	// Shared durability state (ka-0000001444). ONE StateManager backs both the
	// SSE stream's Last-Event-ID and the trigger:bead reconciler watermark, so
	// a restart resumes from the persisted id (cause #1) and the reconciler
	// keeps its high-water mark (cause #3). nil when STATE_FILE is unset — the
	// SSE stream stays volatile and the reconciler stays off, matching the
	// pre-existing behaviour. Sharing one manager (not two on the same path)
	// avoids two writers racing on the file: StateManager serialises writes
	// under its mutex and persists LastEventID + LastReconcileAt in one struct.
	reconcileState, err := newReconcilerState(cfg.stateFile)
	if err != nil {
		logger.Error("failed to load reconciler state file", "path", cfg.stateFile, "error", err)
		os.Exit(1)
	}

	// SSE stream feeds bead lifecycle events into the bead-state source AND
	// the multi-source's underlying engine. Both sources buffer events; the
	// periodic evaluation loop drains them after every Evaluate cycle.
	sseStream := bridge.NewSSEStream(buildSSEStreamConfig(cfg, reconcileState, machineTS, logger))
	// Every plan-gardener trigger matches in_subtree_of, so each event walks
	// the parent chain once per trigger; the parent cache makes that walk
	// mostly free (gd-ZaztjcPrDc). A dependency event drops the stale parent
	// during SSE dispatch; matching runs later, on the engine tick.
	daemon.EnableParentCache(cfg.parentCacheTTL)
	bridge.InvalidateParentsOnEdgeEvents(sseStream, daemon)
	beadSrc.RegisterSSE(sseStream)
	multiSrc.RegisterSSE(sseStream)

	// Trigger-notice publisher: notify-action fires create a closed
	// message bead with the trigger-notice label, which slack-bridge picks
	// up via SSE and posts on its own credentials. trigger-engine therefore
	// holds no Slack token or channel of its own.
	triggerNotifier := bridge.NewNoticeNotifier(daemon, noticeSource, logger)
	logger.Info("notice publisher enabled", "source", noticeSource)

	// One SpawnGuard, shared by every spawn path in this process. The formula
	// step dispatcher below takes the SAME instance as the trigger engine —
	// two independent guards would each grant a full rate budget, so the
	// fleet-wide cap the guard exists to enforce would be double what it says.
	spawnGuard := loadSpawnGuard()

	// Formula step dispatcher (ka-0000001730, bug ka-0000001755): when a
	// formula step bead's last blocking dep closes, spawn an agent matching
	// that step's declared role. Opt-in per step via the formula's
	// suggest_new_agent, so this never mints a pod for the many formulas that
	// intend ONE driver to walk the whole chain in one context.
	//
	// Hosted here rather than in beads-viewer because this process already
	// holds the SSE stream, the SpawnGuard, and the BeadStateReconciler
	// at-least-once floor — and because a missed spawn is far more expensive
	// than the missed nudge the viewer-side plan reactor tolerates.
	stepDispatcher := bridge.NewFormulaStepDispatcher(daemon, spawnGuard, logger).
		// Soft dispatch-time role validation (ka-0000006388): warn + count when a
		// step's role has no backing type:role bead. Fail-open — never blocks a
		// spawn. The daemon client already lists role beads, so this needs no new
		// wiring beyond passing it as the role lister.
		WithRoleValidation(daemon).
		// Fallback human operator for the spawn's X-Operated-By header when a step
		// bead carries no walkable human operator of its own (ka-0000000302). Reads
		// the SAME TRIGGER_ENGINE_DEFAULT_OPERATOR knob the trigger engine's
		// DefaultOperator uses, so both spawn paths in this process attribute
		// header-less autonomous spawns to the same designated operator. Empty when
		// unset — the dispatcher then attributes only from the step's own
		// operated_by field.
		WithDefaultOperator(bridge.TriggerDefaultOperatorFromEnv())
	stepDispatcher.Register(sseStream)

	triggerEngine := bridge.NewTriggerEngine(bridge.TriggerEngineConfig{
		Daemon:                    daemon,
		Notifier:                  triggerNotifier,
		Logger:                    logger,
		Guard:                     spawnGuard,
		LivenessEnabled:           cfg.livenessEnabled,
		LivenessNotify:            cfg.livenessNotify,
		CheckLivenessEnabled:      cfg.checkLivenessEnabled,
		CheckLivenessNotify:       cfg.checkLivenessNotify,
		LivenessCreateBead:        cfg.livenessCreateBead,
		LivenessWatchIDs:          cfg.livenessWatchIDs,
		PouringStallEnabled:       cfg.pouringStallEnabled,
		PouringStallNotify:        cfg.pouringStallNotify,
		PouringStallCreateBead:    cfg.pouringStallCreateBead,
		DarkLaneEnabled:           cfg.darkLaneEnabled,
		DarkLaneNotify:            cfg.darkLaneNotify,
		DarkLaneCreateBead:        cfg.darkLaneCreateBead,
		CredWedgeEnabled:          cfg.credWedgeEnabled,
		CredWedgeNotify:           cfg.credWedgeNotify,
		CredWedgeCreateBead:       cfg.credWedgeCreateBead,
		ClosedEnabledDrainEnabled: cfg.closedEnabledDrainEnabled,
		ClosedEnabledDrainNotify:  cfg.closedEnabledDrainNotify,
		ClosedEpicReaperEnabled:   cfg.closedEpicReaperEnabled,
		ClosedEpicReaperNotify:    cfg.closedEpicReaperNotify,

		UncoveredOpenLaneRollupEnabled: cfg.uncoveredOpenLaneRollupEnabled,
		UncoveredOpenLaneRollupNotify:  cfg.uncoveredOpenLaneRollupNotify,

		StaleRevivalReviewEscalateEnabled: cfg.staleRevivalReviewEscalateEnabled,
		StaleRevivalReviewEscalateNotify:  cfg.staleRevivalReviewEscalateNotify,

		DormantGardenerSweepEnabled: cfg.dormantGardenerSweepEnabled,
		DormantGardenerSweepNotify:  cfg.dormantGardenerSweepNotify,
	})
	logger.Info("gardener-liveness sweep configured",
		"enabled", cfg.livenessEnabled,
		"notify", cfg.livenessNotify,
		"create_bead", cfg.livenessCreateBead,
		"watch_ids", cfg.livenessWatchIDs)
	logger.Info("gardener pouring-stall sweep configured",
		"enabled", cfg.pouringStallEnabled,
		"notify", cfg.pouringStallNotify,
		"create_bead", cfg.pouringStallCreateBead)
	logger.Info("dark-lane-by-concurrency sweep configured",
		"enabled", cfg.darkLaneEnabled,
		"notify", cfg.darkLaneNotify,
		"create_bead", cfg.darkLaneCreateBead)
	logger.Info("credential-wedge sweep configured",
		"enabled", cfg.credWedgeEnabled,
		"notify", cfg.credWedgeNotify,
		"create_bead", cfg.credWedgeCreateBead)
	logger.Info("closed-enabled-drain sweep configured",
		"enabled", cfg.closedEnabledDrainEnabled,
		"notify", cfg.closedEnabledDrainNotify)
	logger.Info("gardener-epic-reaper sweep configured",
		"enabled", cfg.closedEpicReaperEnabled,
		"notify", cfg.closedEpicReaperNotify)
	logger.Info("uncovered-open-lane-rollup sweep configured",
		"enabled", cfg.uncoveredOpenLaneRollupEnabled,
		"notify", cfg.uncoveredOpenLaneRollupNotify)
	logger.Info("stale-revival-review-escalate sweep configured",
		"enabled", cfg.staleRevivalReviewEscalateEnabled,
		"notify", cfg.staleRevivalReviewEscalateNotify)
	logger.Info("dormant-gardener sweep configured",
		"enabled", cfg.dormantGardenerSweepEnabled,
		"notify", cfg.dormantGardenerSweepNotify)
	if cfg.quietGardenerSweepEnabled {
		triggerEngine.EnableQuietGardenerSweep(cfg.quietGardenerSweepNotify)
	}
	logger.Info("quiet-gardener sweep configured",
		"enabled", cfg.quietGardenerSweepEnabled,
		"notify", cfg.quietGardenerSweepNotify)
	triggerEngine.RegisterSource(webhookSrc)
	triggerEngine.RegisterSource(scheduleSrc)
	triggerEngine.RegisterSource(beadSrc)
	triggerEngine.RegisterSource(multiSrc)
	triggerEngine.RegisterSource(mirrorSrc)
	triggerEngine.RegisterSource(accountSrc)
	// A land's steward is rendered with the land's live state at each wake
	// (Lands Track B §4.3, step 9.2).
	triggerEngine.RegisterPromptRenderer(steward.MarkerLabel, steward.TriggerPromptRenderer(daemon))

	// ---- HTTP listeners come up BEFORE the startup catch-up (gd-niQPoeR7ja).
	//
	// The catch-up is a full load of every trigger source from the beads
	// daemon, and it used to run here with nothing listening yet. During the
	// 2026-09-20 cluster-DNS outage it sat blocking on an unreachable daemon,
	// the kubelet's probes hit a closed port, the pod was SIGTERMed mid-load,
	// and the engine logged "catch-up load failed" for every source, "ready",
	// then "shutting down" 0.2 ms later — exit 0, CrashLoopBackOff. Binding the
	// health listener first means a slow or failing daemon is reported as
	// NOT READY on a live, healthy pod instead of killing it.
	ready := &atomic.Bool{}

	mux := http.NewServeMux()
	// LIVE liveness (ka-0000005325): 503 when the last COMPLETED Evaluate() is
	// older than N× poll_interval, so a wedged/hung evaluation loop is caught by
	// k8s liveness and the pod restarted — instead of the old STATIC handler that
	// always returned ok while the schedule source went globally dark for 30h.
	// Bootstrapping (pre-catch-up) reports ok: see liveHealthzHandler.
	mux.HandleFunc("/healthz", liveHealthzHandler(healthStaleness, ready))
	// Readiness reflects daemon reachability: 503 until the catch-up lands.
	mux.HandleFunc("/readyz", readyzHandler(ready))
	// Webhook handler, gated so an early-bound listener cannot fire triggers
	// against seen sets the catch-up has not seeded yet.
	mux.Handle("/webhooks/", gateUntilReady(ready, webhookSrc.HandleWebhook()))

	srv := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		logger.Info("HTTP server starting", "addr", cfg.listenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP server error", "error", err)
			cancel()
		}
	}()

	// Internal-only Prometheus metrics server. Serves the default gatherer,
	// where client_golang auto-registers the Go/process collectors and where
	// the gardener-loop resume metrics land (promauto, internal/bridge/
	// gardener_resume_metrics.go). trigger-engine is the binary that fires
	// gardeners and runs gardenerResumeOrEscalate, so this is where
	// gardener_resume_total / gardener_resumes_without_progress are produced —
	// exposing /metrics here is what makes them scrapeable (decision
	// ka-0000006918: observability before watchdog go-live). Separate port from
	// the public webhook listener so /metrics is never internet-reachable.
	metricsSrv := buildMetricsServer(cfg.metricsAddr)
	go func() {
		logger.Info("metrics server starting", "addr", cfg.metricsAddr)
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics server error", "error", err)
		}
	}()

	// Startup catch-up, retried with capped exponential backoff until it lands
	// (gd-niQPoeR7ja). Blocking here is deliberate: the seen sets MUST be
	// seeded before the first Evaluate, or a restart re-fires every trigger it
	// already handled. A failure is never fatal — the only way out of the retry
	// is success or shutdown.
	if !awaitInitialCatchUp(ctx, triggerEngine, initialCatchUpBackoff(), ready, logger) {
		logger.Info("shutting down trigger-engine")
		shutdownServers(logger, srv, metricsSrv)
		return
	}

	// Start the SSE stream so bead lifecycle events flow into beadSrc.
	go func() {
		if err := sseStream.Start(ctx); err != nil && ctx.Err() == nil {
			logger.Error("SSE event stream stopped", "error", err)
		}
	}()

	// Persistent event buffer for trigger:bead (ka-0000004769). The
	// reconciler periodically polls the daemon for beads with
	// updated_at > watermark and injects synthetic "reconciled" events into
	// beadSrc so missed mutations (bridge crash, postgres failover, SSE ring
	// buffer flush) still fire watcher beads. Disabled when STATE_FILE is
	// unset — the SSE happy path still works, just without the floor. Reuses
	// the SAME StateManager as the SSE stream (ka-0000001444) so the watermark
	// and Last-Event-ID share one serialised writer.
	if reconcileState != nil {
		reconciler := bridge.NewBeadStateReconciler(bridge.ReconcilerConfig{
			Source:   beadSrc,
			State:    reconcileState,
			Daemon:   daemon,
			Interval: cfg.reconcileInterval,
			Logger:   logger,
		})
		go reconciler.Start(ctx)
		logger.Info("trigger:bead reconciler enabled",
			"state_file", cfg.stateFile,
			"interval", cfg.reconcileInterval)
	} else {
		logger.Info("trigger:bead reconciler disabled (STATE_FILE unset)")
	}

	// Phase 3b/B7-c3: start NATS listeners (decision_resolved, bead_closed,
	// mail_sent) feeding multiSrc.Engine(). The same event buffer powers both
	// the multi-source EvaluateTriggers path and bridge's evaluateTriggerSet,
	// so gardeners with non-cron trigger_mode fire from trigger-engine alone
	// (matching the controller-side listeners gated by
	// SCHEDULER_NATS_LISTENERS_ENABLED).
	startSchedulerNATSListeners(ctx, multiSrc.Engine(), cfg.natsURL, cfg.natsToken, cfg.natsUser, logger)

	// Start the trigger engine evaluation loop.
	go runTriggerEngine(ctx, triggerEngine, webhookSrc, beadSrc, multiSrc, cfg.triggerPollInterval, logger)

	// Start the formula step-dispatch backfill sweep (ka-0000006285). The SSE
	// path (OnStepClosed) only re-drives a step when a blocker closes, and in a
	// linear chain that close event happens exactly once — so a spawn refused by
	// the guard, or a close missed during an SSE outage / pod handoff, would
	// strand the step forever. This timer-driven sweep re-considers open,
	// opted-in, un-dispatched steps whose blockers have since cleared, giving
	// each a durable second chance once the guard budget refills. Idempotent
	// (the dispatched_agent stamp + single-claim guard make a repeat a no-op),
	// so it is safe to run unconditionally.
	go runStepDispatchBackfill(ctx, stepDispatcher, cfg.stepBackfillInterval, logger)

	// Start the orphan triage sweep (ka-0000002988). It surfaces open,
	// unassigned, unblocked work left behind by one-shot planning gardeners so
	// it isn't silently orphaned on exit. Off only if explicitly disabled.
	if cfg.orphanSweepEnabled {
		sweeper := scheduler.NewOrphanSweeper(
			orphanStore{daemon: daemon, intakeLabel: cfg.orphanIntakeLabel},
			logger,
			scheduler.OrphanSweepConfig{IntakeLabel: cfg.orphanIntakeLabel, MinAge: cfg.orphanMinAge, DeLaneCooldown: cfg.orphanDeLaneCooldown},
			cfg.orphanSweepInterval,
		)
		go runOrphanSweep(ctx, sweeper, cfg.orphanSweepInterval, logger)
		logger.Info("orphan triage sweep enabled",
			"intake_label", cfg.orphanIntakeLabel,
			"min_age", cfg.orphanMinAge,
			"interval", cfg.orphanSweepInterval,
			"delane_cooldown", cfg.orphanDeLaneCooldown)
	} else {
		logger.Info("orphan triage sweep disabled (ORPHAN_SWEEP_ENABLED=false)")
	}

	// Start the auto-intake sweep (ka-0000001368). The deterministic sweep-OUT
	// complement of the orphan sweep: it STRIPS the intake label from code-ready
	// plan/epic children so they become dispatchable without a human intake pass,
	// replacing the went-dark gardener-intake groomer. Off only if explicitly
	// disabled. Uses the same intake label as the orphan sweep so both halves key
	// off one label.
	if cfg.autoIntakeSweepEnabled {
		autoSweeper := scheduler.NewAutoIntakeSweeper(
			autoIntakeStore{daemon: daemon, intakeLabel: cfg.orphanIntakeLabel},
			logger,
			scheduler.AutoIntakeSweepConfig{IntakeLabel: cfg.orphanIntakeLabel, MinAge: cfg.autoIntakeMinAge},
			cfg.autoIntakeSweepInterval,
		)
		go runAutoIntakeSweep(ctx, autoSweeper, cfg.autoIntakeSweepInterval, logger)
		logger.Info("auto-intake sweep enabled",
			"intake_label", cfg.orphanIntakeLabel,
			"min_age", cfg.autoIntakeMinAge,
			"interval", cfg.autoIntakeSweepInterval)
	} else {
		logger.Info("auto-intake sweep disabled (AUTO_INTAKE_SWEEP_ENABLED=false)")
	}

	// Start the needs-gardener backstop sweep (ka-0000005121). The OPTIONAL,
	// label-only backstop for the at-spawn gardener_id stamp: it surfaces active
	// agent beads with no real gardener by applying a queryable label, never a
	// dependency edge. DISABLED by default — the at-spawn stamp keeps the leak
	// set empty in the common case, so arm this only when the queryable signal
	// is wanted.
	if cfg.needsGardenerSweepEnabled {
		ncSweeper := scheduler.NewNeedsGardenerSweeper(
			needsGardenerStore{daemon: daemon},
			logger,
			scheduler.NeedsGardenerSweepConfig{Label: cfg.needsGardenerLabel, MinAge: cfg.needsGardenerMinAge},
			cfg.needsGardenerSweepInterval,
		)
		go runNeedsGardenerSweep(ctx, ncSweeper, cfg.needsGardenerSweepInterval, logger)
		logger.Info("needs-gardener backstop sweep enabled",
			"label", cfg.needsGardenerLabel,
			"min_age", cfg.needsGardenerMinAge,
			"interval", cfg.needsGardenerSweepInterval)
	} else {
		logger.Info("needs-gardener backstop sweep disabled (NEEDS_GARDENER_SWEEP_ENABLED=false)")
	}

	logger.Info("trigger-engine ready",
		"poll_interval", cfg.triggerPollInterval)

	// Block until shutdown signal.
	<-ctx.Done()
	logger.Info("shutting down trigger-engine")

	// Graceful HTTP server shutdown (both the webhook listener and the
	// internal metrics server).
	shutdownServers(logger, srv, metricsSrv)
}

// buildMetricsServer builds the internal Prometheus metrics HTTP server backed
// by the default gatherer. client_golang auto-registers the Go and process
// collectors there, and it is also where the bridge's promauto-registered
// metrics land — including the gardener-loop resume metrics
// (gardener_resume_total / gardener_resumes_without_progress, bead
// ka-0000005353) that trigger-engine increments as it fires gardeners. The
// server is not started here; the caller invokes ListenAndServe in a
// goroutine. Mirrors slack-bridge's buildMetricsServer (cmd/slack-bridge/
// helpers.go) so every bridge that produces promauto metrics exposes them the
// same way.
func buildMetricsServer(addr string) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(prometheus.DefaultGatherer, promhttp.HandlerOpts{}))
	return &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
}

// newReconcilerState loads the shared durability StateManager for the SSE
// stream's Last-Event-ID and the trigger:bead reconciler watermark
// (ka-0000001444). Returns (nil, nil) when stateFile is empty — that is the
// "no persistence configured" case, not an error, and callers treat a nil
// state as "SSE stays volatile, reconciler stays off". A non-empty path that
// fails to load (e.g. a corrupt or unreadable file) is a hard error: booting
// with a silently-empty state would defeat the whole durability guarantee.
func newReconcilerState(stateFile string) (*bridge.StateManager, error) {
	if stateFile == "" {
		return nil, nil
	}
	return bridge.NewStateManager(stateFile)
}

// buildSSEStreamConfig builds the SSE stream config for trigger-engine. Split
// out so the Last-Event-ID persistence wiring (ka-0000001444) is unit-testable
// without standing up the whole process: passing a non-nil state makes the
// stream restore + persist lastID across restarts (cause #1), and passing nil
// preserves the pre-existing volatile behaviour. The subscribed topics mirror
// the bead lifecycle + edge events both the bead-state and multi sources drain.
func buildSSEStreamConfig(cfg *config, state *bridge.StateManager, machineTS beadsapi.TokenSource, logger *slog.Logger) bridge.SSEStreamConfig {
	return bridge.SSEStreamConfig{
		BeadsHTTPAddr: cfg.beadsHTTPAddr,
		Topics:        append([]string{"beads.bead.created", "beads.bead.updated", "beads.bead.closed"}, bridge.EdgeEventTopics()...),
		Token:         os.Getenv("BEADS_DAEMON_TOKEN"),
		TokenSource:   machineTS,
		Logger:        logger,
		State:         state,
	}
}

func printUsage(w *os.File) {
	fmt.Fprintf(w, `trigger-engine — standalone webhook + schedule + bead trigger evaluator

Usage:
  trigger-engine                 Start the engine (configured via environment)
  trigger-engine --help          Print this message and exit
  trigger-engine --version       Print version and exit

Environment:
  BEADS_HTTP_ADDR        Beads daemon HTTP address (default http://localhost:8080)
  BEADS_DAEMON_TOKEN     Bearer token for beads daemon
  LISTEN_ADDR            HTTP listen address (default :8094)
  METRICS_LISTEN_ADDR    Internal Prometheus /metrics listen address (default :9092)
  TRIGGER_POLL_INTERVAL  Evaluation interval, e.g. 30s (default 30s)
  HEALTH_STALENESS_MULTIPLE  /healthz 503s if no Evaluate() completed in N×poll_interval (default 10)
  STATE_FILE             Persistent watermark for trigger:bead reconciler (ka-0000004769); unset disables
  RECONCILE_INTERVAL     Reconciler poll cadence, e.g. 60s (default 60s)
  STEP_BACKFILL_INTERVAL Formula step-dispatch backfill sweep cadence, e.g. 2m (default 2m)
  PARENT_CACHE_TTL       How long in_subtree_of reuses a bead's parent, e.g. 5m (default 5m; 0 disables)
  LOG_LEVEL              debug|info|warn|error (default info)
  NATS_URL               NATS server URL (optional; enables decision/bead-closed/mail listeners)
  NATS_TOKEN             NATS auth token (optional)
  SPAWN_GUARD_*          See spawnguard package for tunables

Endpoints:
  /healthz               LIVE health check (503 if the Evaluate() loop is stalled)
  /readyz                Readiness check
  /webhooks/{token}      Webhook ingestion

Version: %s (commit %s)
`, version, commit)
}

// config holds parsed environment configuration for the trigger-engine service.
type config struct {
	beadsHTTPAddr       string
	triggerPollInterval time.Duration
	// healthStalenessMultiple is the N in "the engine loop may go without a
	// completed Evaluate() for N× poll_interval before /healthz reports 503"
	// (ka-0000005325). LIVE liveness lets k8s restart a wedged pod. Default 10
	// (300s at the 30s poll default): well past a slow cycle, far below the 30h
	// blackout that motivated it. Overridable via HEALTH_STALENESS_MULTIPLE.
	healthStalenessMultiple int
	listenAddr              string
	// metricsAddr is the internal-only listen address for the Prometheus
	// /metrics endpoint. trigger-engine fires gardeners, so the gardener-loop
	// resume metrics (gardener_resume_total / gardener_resumes_without_progress,
	// bead ka-0000005353) are incremented in THIS process via promauto. Without
	// a served /metrics endpoint they register against the default registry but
	// are never scraped — the Prometheus half of the resume observability layer
	// (decision ka-0000006918) would be invisible. Separate port mirrors the
	// controller (:9092) and slack-bridge (:9093) split so the public webhook
	// listener never exposes internal metrics.
	metricsAddr string
	logLevel    string
	natsURL     string
	natsToken   string
	natsUser    string
	// stateFile persists the trigger:bead reconciler watermark across
	// restarts. Empty disables the reconciler (steady-state SSE only).
	stateFile string
	// reconcileInterval is the cadence at which the reconciler polls the
	// daemon for missed bead updates (ka-0000004769). Defaults to 60s.
	reconcileInterval time.Duration

	// parentCacheTTL is how long the daemon client's IsAncestor reuses a
	// bead's parent-child parent (gd-ZaztjcPrDc). Dependency SSE events
	// invalidate entries; the TTL bounds staleness for a missed event. Zero
	// disables the cache.
	parentCacheTTL time.Duration

	// stepBackfillInterval is the cadence of the formula step-dispatch backfill
	// sweep (ka-0000006285) — the durable retry for steps whose SSE-driven
	// dispatch was refused or missed. Defaults to 2m: brisk enough that a
	// rate-limited step recovers within a couple of guard windows, slow enough
	// that the candidate list-scan is negligible load.
	stepBackfillInterval time.Duration

	// Orphan triage sweep (ka-0000002988): surfaces open/unassigned/unblocked
	// work left behind by one-shot planning gardeners so it isn't silently
	// orphaned on exit. Enabled by default — it only labels, which is
	// idempotent and reversible.
	orphanSweepEnabled   bool
	orphanIntakeLabel    string
	orphanMinAge         time.Duration
	orphanSweepInterval  time.Duration
	orphanDeLaneCooldown time.Duration

	// Auto-intake sweep (ka-0000001368): the deterministic, agent-free sweep-OUT
	// complement of the orphan sweep. On a throttled cadence it STRIPS the intake
	// label from code-ready PLAN/EPIC CHILDREN (parented, unassigned, unblocked,
	// non-Slack-sourced work) so they become dispatchable without a human intake
	// pass — replacing the went-dark gardener-intake groomer (ka-0000006210). It
	// shares orphanIntakeLabel so both halves key off the same label, and is
	// disjoint-by-construction with the orphan sweep (it only de-lanes PARENTED
	// work; the orphan sweep only labels PARENTLESS work). Enabled by default: it
	// only removes a label, which is idempotent and reversible.
	autoIntakeSweepEnabled  bool
	autoIntakeMinAge        time.Duration
	autoIntakeSweepInterval time.Duration

	// needs-gardener backstop sweep (persistent-agents Phase 3, ka-0000005121):
	// the OPTIONAL, label-only backstop for the at-spawn gardener_id stamp. On a
	// throttled cadence it surfaces active agent beads with no real gardener (a
	// synthetic/unowned gardener_id — an at-spawn stamp leak) by applying a
	// queryable label, never a dependency edge. DISABLED by default: the
	// at-spawn stamp already keeps the leak set empty in the common case, so the
	// backstop is armed only when an operator wants the queryable signal.
	needsGardenerSweepEnabled  bool
	needsGardenerLabel         string
	needsGardenerMinAge        time.Duration
	needsGardenerSweepInterval time.Duration

	// Engine-side gardener-liveness heartbeat sweep (B2, ka-0000001541). Runs
	// once per Evaluate cycle and alerts on any enabled gardener whose
	// last_checked has gone stale — the agent-free dead-man's-switch that
	// catches a went-dark gardener/fleet (the 5/24–5/26 incident class). Enabled
	// by default: it is read-only + idempotent (one alert per dark episode) and
	// spawns nothing. livenessNotify routes to Slack via the notice publisher;
	// livenessCreateBead additionally files a durable bug bead under the dark
	// gardener (off by default to avoid bead churn while the alert is observed).
	livenessEnabled    bool
	livenessNotify     bool
	livenessCreateBead bool

	// NON-gardener trigger check-liveness sweep (ka-0000004245). The gardener
	// liveness sweep is gated on isGardenerTrigger, so a plain intake trigger
	// whose last_checked froze was watched by nothing — three prod jira lanes
	// sat that way for 11 days while enabled=true read healthy. Alert-only
	// (WARN + notify), never spawns, files no bead; enabled by default.
	// Env: TRIGGER_CHECK_LIVENESS_ENABLED / TRIGGER_CHECK_LIVENESS_NOTIFY.
	checkLivenessEnabled bool
	checkLivenessNotify  bool
	// Operator-configured gardener-id allow-list swept across ALL statuses every
	// cycle (bug ka-0000004451, SweepGardenerLivenessAllowList). The main liveness
	// sweep is open-only (widening it to closed floods on ~130 retired lineages),
	// and the declarative assert ka-0000002431 is transition-only — a single
	// missed close/disable event leaves a watched gardener dark forever. This
	// explicit list closes that blind spot by polling current status every cycle
	// regardless of any event. Empty (the default) ⇒ the all-status pass is a
	// no-op. Comma-separated env GARDENER_LIVENESS_WATCH_IDS.
	livenessWatchIDs []string

	// Engine-side "pouring-but-never-worked" stall sweep (ka-0000000957). The
	// sibling of the liveness sweep for the ORTHOGONAL failure the FLAT gardener
	// hit: an apply_formula/gardener trigger keeps pouring molecules on cadence
	// (last_checked fresh, so the liveness sweep sees it as alive) but the
	// poured molecules accumulate open with unworked step beads because the
	// driver spawn failed or the driver never made progress. Enabled by
	// default: read-only + idempotent (one alert per stall episode) and spawns
	// nothing. pouringStallNotify routes to Slack via the notice publisher;
	// pouringStallCreateBead additionally files a durable bug bead under the
	// stalled trigger (off by default to avoid bead churn while observed).
	pouringStallEnabled    bool
	pouringStallNotify     bool
	pouringStallCreateBead bool

	// Engine-side dark-lane-by-concurrency detector (ka-0000002023). The third
	// sibling: catches a spawn-producing trigger that is enabled and actively
	// evaluated (last_checked fresh, so the liveness sweep sees it alive) yet has
	// not FIRED in many cadences because a live-but-wedged agent pins its
	// max_concurrent lease — the silent 15h dark bug-drain lane that motivated
	// the bug. Enabled by default: read-only + idempotent (one alert per
	// suppression episode) and spawns nothing. darkLaneNotify routes to Slack via
	// the notice publisher; darkLaneCreateBead additionally files a durable bug
	// bead under the suppressed trigger (off by default to avoid bead churn while
	// observed).
	darkLaneEnabled    bool
	darkLaneNotify     bool
	darkLaneCreateBead bool

	// Credential-wedged-agent detector (bug ka-0000000592). Flags an agent parked
	// in agent_state=rate_limited/budget_exceeded with zero completed turns while
	// SyncAll keeps churning its updated_at fresh — the credential-exhaustion
	// dimension of the ka-0000002023 "invisible to updated_at" gap. WARN-logs +
	// one-shot-comments on the wedged agent bead; never spawns/kills.
	// credWedgeNotify routes to Slack via the notice publisher; credWedgeCreateBead
	// additionally files a durable bug bead under the agent (off by default to
	// avoid bead churn while observed).
	credWedgeEnabled    bool
	credWedgeNotify     bool
	credWedgeCreateBead bool

	// Closed+enabled dark-trigger DRAIN sweep (bug ka-0000002082). The CAPACITY
	// fix (not an alert): the trigger-reviver agent could file only ONE archive
	// decision per 4h cycle against a 137-deep closed+enabled pool, so the
	// operator queue grew unboundedly. This engine sweep auto-archives
	// retired-lineage triggers (gardener-epic CLOSED) — capped, reversible,
	// audited — and files ONE batched archive decision for the unanchored cohort
	// (deduped to one open at a time, executed on resolution). Enabled by
	// default: reversible + idempotent + throttled to once/hour + pause-aware, and
	// it never spawns. closedEnabledDrainNotify routes a per-run summary to Slack
	// via the notice publisher.
	closedEnabledDrainEnabled bool
	closedEnabledDrainNotify  bool

	// uncoveredOpenLaneRollupEnabled turns on the uncovered-open-lane roll-up
	// report (bug ka-0000002082 fix 4). The read-only counterpart of the drain
	// sweep: it surfaces the genuinely-uncovered lanes the drain sweep leaves
	// untouched (closed+enabled on an OPEN epic with live children and no live
	// successor gardener) as ONE living type=doc report, refreshed daily and
	// updated in place. Mutates no triggers and files no decisions, so it is
	// enabled by default (unlike the destructive reaper). notify routes a per-run
	// summary to Slack when the report is non-empty.
	uncoveredOpenLaneRollupEnabled bool
	uncoveredOpenLaneRollupNotify  bool

	// staleRevivalReviewEscalateEnabled turns on age-based escalation of stale
	// revival-review decisions (bug ka-0000002082 fix 3): raise an OPEN
	// `trigger-reviver-revival-review` decision that has gone stale (>7d→P1,
	// >14d→P0) so it surfaces above newer work in the operator queue. Mutates only
	// the priority attribute (reversible + idempotent + throttled to once/hour +
	// pause-aware); creates/closes no decisions. notify routes a per-run summary
	// to Slack when at least one decision was escalated.
	staleRevivalReviewEscalateEnabled bool
	staleRevivalReviewEscalateNotify  bool

	// dormantGardenerSweepEnabled turns on the daily dormant-gardener sweep
	// (gd-kkrCdlf4iO): one deduped operator escalation naming gardeners dormant
	// over 24h that still have actionable children. It files at most one open
	// escalation and mutates no trigger, so it is enabled by default. notify
	// routes a Slack notice when it files or updates the escalation.
	dormantGardenerSweepEnabled bool
	dormantGardenerSweepNotify  bool

	// quietGardenerSweepEnabled turns on the quiet-gardener sweep
	// (gd-bo2RUmtpHb): it wakes an event-mode plan gardener that has not run in
	// 2h while its plan has a ready, unassigned step, notes it on the plan, and
	// files one deduped operator escalation when a gardener needs a second wake
	// within a day. It is the owner's watch on event-mode gardeners
	// (gd-fULNT8wZNF), so it is enabled by default. notify routes a Slack
	// notice when it files or updates the escalation.
	quietGardenerSweepEnabled bool
	quietGardenerSweepNotify  bool

	// closedEpicReaperEnabled turns on the gardener-reaper sweep (task
	// ka-0000000102): auto-close OPEN gardener triggers whose pinned epic is CLOSED
	// (audit ka-0000001071 categories A/C — the enabled-but-inert and
	// dead-disabled gardeners no other sweep reaps). Like the drain sweep it mutates
	// beads, is throttled to once/hour, capped per run, pause-aware, and every close
	// is reversible + audited. Shipped DEFAULT-OFF (unlike the drain sweep) so an
	// operator reads the dry-run "would reap" log lines and confirms the closed-epic
	// + standing guards match only genuine retirements before arming a destructive
	// close path (rollout discipline task ka-0000003021). closedEpicReaperNotify
	// routes a per-run summary to Slack when it reaped something.
	closedEpicReaperEnabled bool
	closedEpicReaperNotify  bool
}

func parseConfig() *config {
	triggerPollInterval := 30 * time.Second
	if v := os.Getenv("TRIGGER_POLL_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			triggerPollInterval = d
		}
	}
	reconcileInterval := 60 * time.Second
	if v := os.Getenv("RECONCILE_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			reconcileInterval = d
		}
	}
	// Engine-loop deadman /healthz staleness multiple (ka-0000005325). Non-
	// positive or unparseable values fall back to the default 10 rather than
	// disabling liveness — a zero would make /healthz 503 the pod on every
	// scrape (age is always > 0), so guard against it explicitly.
	healthStalenessMultiple := 10
	if v := os.Getenv("HEALTH_STALENESS_MULTIPLE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			healthStalenessMultiple = n
		}
	}
	parentCacheTTL := 5 * time.Minute
	if v := os.Getenv("PARENT_CACHE_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			parentCacheTTL = d
		}
	}
	stepBackfillInterval := 2 * time.Minute
	if v := os.Getenv("STEP_BACKFILL_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			stepBackfillInterval = d
		}
	}

	// Orphan triage sweep config (ka-0000002988). Enabled by default; bad
	// duration values fall back to the package defaults rather than failing
	// startup, matching the lenient parsing of the other intervals here.
	orphanMinAge := time.Duration(0) // 0 ⇒ scheduler default
	if v := os.Getenv("ORPHAN_SWEEP_MIN_AGE"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			orphanMinAge = d
		}
	}
	orphanSweepInterval := time.Duration(0) // 0 ⇒ scheduler default
	if v := os.Getenv("ORPHAN_SWEEP_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			orphanSweepInterval = d
		}
	}
	// De-lane cooldown: 0 ⇒ scheduler default (defaultOrphanDeLaneCooldown); a
	// negative duration (e.g. "-1s") disables the cooldown guard so only the
	// parent guard remains. Invalid values fall back to the default.
	orphanDeLaneCooldown := time.Duration(0)
	if v := os.Getenv("ORPHAN_DELANE_COOLDOWN"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			orphanDeLaneCooldown = d
		}
	}

	// Auto-intake sweep config (ka-0000001368). Enabled by default; bad duration
	// values fall back to the package defaults, matching the lenient parsing of
	// the other intervals here. A negative min-age disables the grace guard.
	autoIntakeMinAge := time.Duration(0) // 0 ⇒ scheduler default
	if v := os.Getenv("AUTO_INTAKE_MIN_AGE"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			autoIntakeMinAge = d
		}
	}
	autoIntakeSweepInterval := time.Duration(0) // 0 ⇒ scheduler default
	if v := os.Getenv("AUTO_INTAKE_SWEEP_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			autoIntakeSweepInterval = d
		}
	}

	// needs-gardener backstop sweep config (ka-0000005121). Bad duration values
	// fall back to the package defaults, matching the lenient parsing above. A
	// negative min-age disables the grace guard (surface immediately).
	needsGardenerMinAge := time.Duration(0) // 0 ⇒ scheduler default
	if v := os.Getenv("NEEDS_GARDENER_MIN_AGE"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			needsGardenerMinAge = d
		}
	}
	needsGardenerSweepInterval := time.Duration(0) // 0 ⇒ scheduler default
	if v := os.Getenv("NEEDS_GARDENER_SWEEP_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			needsGardenerSweepInterval = d
		}
	}

	return &config{
		beadsHTTPAddr:              envOrDefault("BEADS_HTTP_ADDR", "http://localhost:8080"),
		triggerPollInterval:        triggerPollInterval,
		healthStalenessMultiple:    healthStalenessMultiple,
		parentCacheTTL:             parentCacheTTL,
		listenAddr:                 envOrDefault("LISTEN_ADDR", ":8094"),
		metricsAddr:                envOrDefault("METRICS_LISTEN_ADDR", ":9092"),
		logLevel:                   envOrDefault("LOG_LEVEL", "info"),
		natsURL:                    os.Getenv("NATS_URL"),
		natsToken:                  os.Getenv("NATS_TOKEN"),
		natsUser:                   os.Getenv("NATS_USER"),
		stateFile:                  os.Getenv("STATE_FILE"),
		reconcileInterval:          reconcileInterval,
		stepBackfillInterval:       stepBackfillInterval,
		orphanSweepEnabled:         envBoolDefault("ORPHAN_SWEEP_ENABLED", true),
		orphanIntakeLabel:          envOrDefault("ORPHAN_INTAKE_LABEL", scheduler.DefaultIntakeLabel),
		orphanMinAge:               orphanMinAge,
		orphanSweepInterval:        orphanSweepInterval,
		orphanDeLaneCooldown:       orphanDeLaneCooldown,
		autoIntakeSweepEnabled:     envBoolDefault("AUTO_INTAKE_SWEEP_ENABLED", true),
		autoIntakeMinAge:           autoIntakeMinAge,
		autoIntakeSweepInterval:    autoIntakeSweepInterval,
		needsGardenerSweepEnabled:  envBoolDefault("NEEDS_GARDENER_SWEEP_ENABLED", false),
		needsGardenerLabel:         envOrDefault("NEEDS_GARDENER_LABEL", scheduler.DefaultNeedsGardenerLabel),
		needsGardenerMinAge:        needsGardenerMinAge,
		needsGardenerSweepInterval: needsGardenerSweepInterval,
		livenessEnabled:            envBoolDefault("GARDENER_LIVENESS_ENABLED", true),
		livenessNotify:             envBoolDefault("GARDENER_LIVENESS_NOTIFY", true),
		checkLivenessEnabled:       envBoolDefault("TRIGGER_CHECK_LIVENESS_ENABLED", true),
		checkLivenessNotify:        envBoolDefault("TRIGGER_CHECK_LIVENESS_NOTIFY", true),
		livenessCreateBead:         envBoolDefault("GARDENER_LIVENESS_CREATE_BEAD", false),
		livenessWatchIDs:           envCSVList("GARDENER_LIVENESS_WATCH_IDS"),
		pouringStallEnabled:        envBoolDefault("GARDENER_POURING_STALL_ENABLED", true),
		pouringStallNotify:         envBoolDefault("GARDENER_POURING_STALL_NOTIFY", true),
		pouringStallCreateBead:     envBoolDefault("GARDENER_POURING_STALL_CREATE_BEAD", false),
		darkLaneEnabled:            envBoolDefault("DARK_LANE_ENABLED", true),
		darkLaneNotify:             envBoolDefault("DARK_LANE_NOTIFY", true),
		darkLaneCreateBead:         envBoolDefault("DARK_LANE_CREATE_BEAD", false),
		credWedgeEnabled:           envBoolDefault("CREDENTIAL_WEDGE_ENABLED", true),
		credWedgeNotify:            envBoolDefault("CREDENTIAL_WEDGE_NOTIFY", true),
		credWedgeCreateBead:        envBoolDefault("CREDENTIAL_WEDGE_CREATE_BEAD", false),
		closedEnabledDrainEnabled:  envBoolDefault("CLOSED_ENABLED_DRAIN_ENABLED", true),
		closedEnabledDrainNotify:   envBoolDefault("CLOSED_ENABLED_DRAIN_NOTIFY", true),
		// Read-only report (no trigger mutation, no decisions) — safe default-on.
		uncoveredOpenLaneRollupEnabled: envBoolDefault("UNCOVERED_OPEN_LANE_ROLLUP_ENABLED", true),
		uncoveredOpenLaneRollupNotify:  envBoolDefault("UNCOVERED_OPEN_LANE_ROLLUP_NOTIFY", true),
		// Reversible priority-only bump (no trigger mutation, no decisions) — safe
		// default-on, like the drain/roll-up legs of the same bug.
		staleRevivalReviewEscalateEnabled: envBoolDefault("STALE_REVIVAL_REVIEW_ESCALATE_ENABLED", true),
		staleRevivalReviewEscalateNotify:  envBoolDefault("STALE_REVIVAL_REVIEW_ESCALATE_NOTIFY", true),
		// One deduped escalation, no trigger mutation — safe default-on.
		dormantGardenerSweepEnabled: envBoolDefault("DORMANT_GARDENER_SWEEP_ENABLED", true),
		dormantGardenerSweepNotify:  envBoolDefault("DORMANT_GARDENER_SWEEP_NOTIFY", true),
		// The owner's watch on event-mode gardeners: default-on.
		quietGardenerSweepEnabled: envBoolDefault("QUIET_GARDENER_SWEEP_ENABLED", true),
		quietGardenerSweepNotify:  envBoolDefault("QUIET_GARDENER_SWEEP_NOTIFY", true),
		// Default OFF — destructive close path, armed by an operator after a
		// dry-run review (task ka-0000003021 discipline).
		closedEpicReaperEnabled: envBoolDefault("GARDENER_EPIC_REAPER_ENABLED", false),
		closedEpicReaperNotify:  envBoolDefault("GARDENER_EPIC_REAPER_NOTIFY", true),
	}
}

// runTriggerEngine runs the trigger engine's evaluation loop until ctx is
// cancelled. It uses a dual-trigger approach: periodic ticker for reloading
// trigger definitions, plus an immediate notify channel for instant evaluation
// when a webhook event is received. Buffered sources (webhook, bead-state) are
// drained after every cycle so events that did not match any trigger this pass
// don't pile up forever.
func runTriggerEngine(ctx context.Context, engine *bridge.TriggerEngine, webhookSrc *bridge.WebhookTriggerSource, beadSrc *bridge.BeadStateTriggerSource, multiSrc *bridge.MultiTriggerSource, interval time.Duration, logger *slog.Logger) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	logger.Info("trigger engine started", "poll_interval", interval)

	// Kick off the first evaluation immediately.
	freezeEventSources(beadSrc, multiSrc)
	engine.Evaluate(ctx)
	webhookSrc.DrainPending()
	beadSrc.DrainPending()
	multiSrc.DrainPending()
	// Beat the engine-loop deadman heartbeat after the completed cycle
	// (ka-0000005325): increments gardenio_trigger_engine_evaluations_total and
	// stamps the /healthz last-Evaluate clock. Only the full-loop path beats it
	// — the webhook fast-path below is a single-source partial pass.
	recordEvaluateCompleted(time.Now())

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	notify := webhookSrc.Notify()
	for {
		select {
		case <-ctx.Done():
			logger.Info("trigger engine stopping")
			return
		case <-ticker.C:
			// Periodic: reload trigger definitions and evaluate all sources.
			freezeEventSources(beadSrc, multiSrc)
			engine.Evaluate(ctx)
			webhookSrc.DrainPending()
			beadSrc.DrainPending()
			multiSrc.DrainPending()
			recordEvaluateCompleted(time.Now())
		case <-notify:
			// Immediate: a webhook was just received, evaluate right away.
			engine.EvaluateSource(ctx, "webhook")
			webhookSrc.DrainPending()
		}
	}
}

// freezeEventSources fixes the events a full Evaluate pass sees, so the drain
// that ends the pass removes only those: an event that arrives mid-pass, after
// the triggers it matches were already visited, is evaluated next pass rather
// than drained unseen (gd-IQcMk16PkH).
func freezeEventSources(beadSrc *bridge.BeadStateTriggerSource, multiSrc *bridge.MultiTriggerSource) {
	beadSrc.FreezePending()
	multiSrc.FreezePending()
}

// runStepDispatchBackfill drives the formula step-dispatch backfill sweep until
// ctx is cancelled (ka-0000006285). The sweep is the durable retry the SSE
// close-event path cannot provide: a linear-chain step whose spawn was refused
// or whose unblocking close was missed gets re-considered here on a timer. A
// nil dispatcher (no daemon wired) makes BackfillReadySteps a no-op, so this is
// safe to start unconditionally.
func runStepDispatchBackfill(ctx context.Context, dispatcher *bridge.FormulaStepDispatcher, interval time.Duration, logger *slog.Logger) {
	if interval <= 0 {
		interval = 2 * time.Minute
	}
	logger.Info("formula step-dispatch backfill sweep started", "interval", interval)

	// Initial sweep on startup so a step stranded by the previous pod's refusal
	// or by the rolling-update handoff recovers promptly rather than waiting a
	// full interval.
	dispatcher.BackfillReadySteps(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Info("formula step-dispatch backfill sweep stopping")
			return
		case <-ticker.C:
			dispatcher.BackfillReadySteps(ctx)
		}
	}
}

// orphanDeLaneEventScan bounds how many of a candidate bead's most-recent
// beads.label.removed events LastDeLane scans for a de-lane of the intake label.
// The fetch asks the daemon for that topic only (GetRecentEventsByTopic,
// gd-cP9tODid8j), so it ships a handful of small label rows rather than the
// bead's last 200 events of every topic; the cap still bounds a bead with a
// large event log (bug ka-0000002256).
const orphanDeLaneEventScan = 200

// orphanStore adapts *beadsapi.Client to scheduler.OrphanStore. Candidates are
// sourced from GET /v1/beads with no_blockers=true (the ready-queue semantics
// shared with `gb ready`), restricted server-side to open work-item types. The
// no-blockers filter keeps the unblocked determination on the daemon, and the
// /v1/beads list view carries created_at, which the sweep needs for its grace
// period — both reasons to prefer ListBeadsFiltered over the bare /v1/ready
// view. The remaining predicates (unassigned, grace period, already-labelled)
// are applied by scheduler.SelectOrphans.
type orphanStore struct {
	daemon *beadsapi.Client
	// intakeLabel is the label the sweep applies (and the groomer removes). It
	// is the label LastDeLane watches for in beads.label.removed events so the
	// de-lane cooldown keys off the actual intake label, not any label removal.
	intakeLabel string
}

// ListReady returns EVERY unblocked open work-item candidate, paginated. It must
// exhaust the match set, not cap it: the candidate population (4779+ fleet-wide,
// bug ka-0000000077) far exceeds any single-page limit, and the daemon returns
// it newest-first, so a fixed cap silently truncated the OLDEST 79% of the input
// — making any bead older than ~6 weeks permanently unreachable by the sweep, no
// matter how often it ran. ListAllBeadsFiltered owns paging (created_at-ascending,
// deduped) and turns a max-pages overrun into a loud WARN rather than a silent
// clean-sweep-over-rows-it-never-fetched (class ka-0000003555).
func (o orphanStore) ListReady(ctx context.Context) ([]scheduler.OrphanBead, error) {
	res, err := o.daemon.ListAllBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:      []string{"task", "bug", "feature", "epic"},
		Statuses:   []string{"open"},
		NoOpenDeps: true,
	})
	if err != nil {
		return nil, err
	}
	out := make([]scheduler.OrphanBead, 0, len(res.Beads))
	for _, b := range res.Beads {
		out = append(out, scheduler.OrphanBead{
			ID:        b.ID,
			Type:      b.Type,
			Status:    b.Status,
			Assignee:  b.Assignee,
			Labels:    b.Labels,
			CreatedAt: b.CreatedAt,
		})
	}
	return out, nil
}

func (o orphanStore) AddLabel(ctx context.Context, beadID, label string) error {
	return o.daemon.AddLabel(ctx, beadID, label)
}

// needsGardenerStore adapts *beadsapi.Client to scheduler.NeedsGardenerStore for
// the needs-gardener backstop sweep (ka-0000005121). It projects the active
// agent beads (ListAgentBeads already returns active-status agents with their
// derived gardener_id) to scheduler.GardenerlessAgent, pre-computing
// HasRealGardener via beadsapi.IsSyntheticGardenerID so the "real gardener vs
// synthetic/unowned" policy stays defined in exactly one place.
type needsGardenerStore struct {
	daemon *beadsapi.Client
}

func (n needsGardenerStore) ListActiveAgents(ctx context.Context) ([]scheduler.GardenerlessAgent, error) {
	agents, err := n.daemon.ListAgentBeads(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]scheduler.GardenerlessAgent, 0, len(agents))
	for _, a := range agents {
		out = append(out, scheduler.GardenerlessAgent{
			ID:              a.ID,
			HasRealGardener: !beadsapi.IsSyntheticGardenerID(a.GardenerID),
			Labels:          a.Labels,
			CreatedAt:       a.CreatedAt,
		})
	}
	return out, nil
}

func (n needsGardenerStore) AddLabel(ctx context.Context, beadID, label string) error {
	return n.daemon.AddLabel(ctx, beadID, label)
}

// orphanSweepCommentAuthor is the author recorded on the sweeper's re-lane
// audit comments. A fixed system identity (not a human email) so re-lane
// comments are attributable to the controller OrphanSweeper.
const orphanSweepCommentAuthor = "orphan-sweep"

// AddComment records the sweeper's re-lane audit comment (scheduler.ReLaneMarker)
// under a fixed system author. See scheduler.OrphanStore.AddComment.
func (o orphanStore) AddComment(ctx context.Context, beadID, text string) error {
	return o.daemon.AddComment(ctx, beadID, orphanSweepCommentAuthor, text)
}

// ParentedBeads returns the subset of beadIDs that have a parent-child owning
// epic/plan. Parent-child edges are stored child-first (deps.bead_id=child,
// deps.depends_on_id=parent — see kbeads cmd/kd/dep.go), so a *forward*
// dependency of type "parent-child" on a bead means that bead IS a child, i.e.
// it has a parent. BatchGetDependencies shards at 50, so this is a couple of
// round-trips even for a large candidate set.
func (o orphanStore) ParentedBeads(ctx context.Context, beadIDs []string) (map[string]bool, error) {
	out := make(map[string]bool, len(beadIDs))
	if len(beadIDs) == 0 {
		return out, nil
	}
	depsByID, err := o.daemon.BatchGetDependencies(ctx, beadIDs, "forward")
	if err != nil {
		return nil, err
	}
	for id, deps := range depsByID {
		for _, d := range deps {
			if d.Type == "parent-child" && d.DependsOnID != "" {
				out[id] = true
				break
			}
		}
	}
	return out, nil
}

// terminalChildStatuses are the parent-child child statuses that do NOT keep a
// parent alive as a tree-root: a child in one of these states carries no live
// work. Everything else (open, in_progress, blocked, deferred, "", or any
// custom in-flight status) counts as an OPEN child. "partial" is
// terminal-for-progression like "closed" (kbeads model.StatusPartial), so a
// subtree of only closed/partial children means the root is drained — a
// parentless drained task is then a legitimate orphan again and keeps surfacing.
var terminalChildStatuses = map[string]bool{
	"closed":  true,
	"partial": true,
}

// HasOpenChildren returns the subset of beadIDs that have at least one OPEN
// (non-terminal) parent-child child — i.e. are tree-roots with live subtree
// work. It reads the daemon's per-parent child-status histogram
// (GET /v1/batch/children-by-status) and treats any non-terminal bucket with a
// positive count as an open child. Using the status histogram (rather than a
// bare reverse parent-child edge check) is deliberate: closing a child never
// removes its parent-child edge, so an all-closed subtree must NOT keep the
// root pinned as a non-orphan (ka-0000003548). IDs with only closed/partial
// children (or no children) are absent from the returned map.
func (o orphanStore) HasOpenChildren(ctx context.Context, beadIDs []string) (map[string]bool, error) {
	out := make(map[string]bool, len(beadIDs))
	if len(beadIDs) == 0 {
		return out, nil
	}
	histByID, err := o.daemon.BatchGetChildrenByStatus(ctx, beadIDs)
	if err != nil {
		return nil, err
	}
	for id, hist := range histByID {
		for status, count := range hist {
			if count > 0 && !terminalChildStatuses[strings.ToLower(strings.TrimSpace(status))] {
				out[id] = true
				break
			}
		}
	}
	return out, nil
}

// LastDeLane returns, per bead id that has one, the timestamp of the most recent
// intake-groomer de-lane. Beads never de-laned are absent from the map. There is
// no batch endpoint for either signal, so this issues per-id daemon calls —
// acceptable because the sweeper only calls it for the small post-pass-1
// candidate set, never the whole ready view.
//
// Two signals feed the cooldown, and the LATEST of the two wins:
//
//   - The authoritative beads.label.removed EVENT for the intake label. Every
//     label removal (bare `kd label remove`, the daemon RemoveLabel RPC, or the
//     HTTP DELETE endpoint) records this durable event via recordAndPublish →
//     RecordEvent (kbeads handleRemoveLabel / server.RemoveLabel). This is the
//     de-lane signal that ALWAYS exists — it does not depend on the groomer
//     leaving any comment.
//   - The legacy intake-groomed COMMENT (scheduler.DeLaneMarker prefix,
//     case-insensitive), kept as a fallback for de-lanes recorded before the
//     event signal existed and for groomers/tools that comment without emitting
//     a matching removal event.
//
// Keying the cooldown off the event closes the residual orphan fight-loop
// (ka-0000007706): the groomer's canonical de-lane is a bare label remove with
// NO comment, so the comment-only signal left DeLanedAt zero and the 30-min
// cooldown was a silent no-op — the sweeper re-laned unclaimed orphans within one
// interval (~79s observed). Reading the removal event makes any de-lane, comment
// or not, start the cooldown and win the race against the next sweep.
func (o orphanStore) LastDeLane(ctx context.Context, beadIDs []string) (map[string]time.Time, error) {
	intakeLabel := strings.TrimSpace(o.intakeLabel)
	if intakeLabel == "" {
		intakeLabel = scheduler.DefaultIntakeLabel
	}
	out := make(map[string]time.Time, len(beadIDs))
	for _, id := range beadIDs {
		var latest time.Time

		// Authoritative signal: the most recent removal event for the intake label.
		// Only label.removed events are fetched (topic filter), bounded rather
		// than the full history so the call caps memory per GetRecentEvents' OOM
		// guidance. The Topic check below stays: a daemon without ?topic=
		// answers with every topic.
		events, err := o.daemon.GetRecentEventsByTopic(ctx, id, beadsEventTopicLabelRemoved, orphanDeLaneEventScan)
		if err != nil {
			return nil, err
		}
		for _, e := range events {
			if e.Topic != beadsEventTopicLabelRemoved {
				continue
			}
			if !eventLabelMatches(e.Payload, intakeLabel) {
				continue
			}
			if e.CreatedAt.After(latest) {
				latest = e.CreatedAt
			}
		}

		// Fallback signal: the legacy intake-groomed comment.
		comments, err := o.daemon.GetComments(ctx, id)
		if err != nil {
			return nil, err
		}
		for _, c := range comments {
			if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(c.Text)), scheduler.DeLaneMarker) {
				continue
			}
			if c.CreatedAt.After(latest) {
				latest = c.CreatedAt
			}
		}

		if !latest.IsZero() {
			out[id] = latest
		}
	}
	return out, nil
}

// beadsEventTopicLabelRemoved is the daemon event topic recorded when a label is
// removed from a bead (kbeads events.TopicLabelRemoved). Duplicated as a local
// const because the controller does not import the kbeads events package.
const beadsEventTopicLabelRemoved = "beads.label.removed"

// eventLabelMatches reports whether a label.added/label.removed event payload
// names the given label (case-insensitive, trimmed). The payload shape is
// {"bead_id":...,"label":...,"fleet":...} (kbeads events.LabelRemoved).
func eventLabelMatches(payload []byte, label string) bool {
	if len(payload) == 0 {
		return false
	}
	var p struct {
		Label string `json:"label"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(p.Label), label)
}

// beadsEventTopicLabelAdded is the daemon event topic recorded when a label is
// added to a bead (kbeads events.TopicLabelAdded). Duplicated as a local const
// because the controller does not import the kbeads events package. Payload
// shape matches the removed event ({"bead_id":...,"label":...,"fleet":...}), so
// eventLabelMatches reads both.
const beadsEventTopicLabelAdded = "beads.label.added"

// autoIntakeStore adapts *beadsapi.Client to scheduler.AutoIntakeStore. It is
// the de-lane (sweep-OUT) complement of orphanStore: candidates are the daemon's
// ready view (no_blockers=true, open work-item types) FILTERED to beads that
// carry the intake label, and the terminal action strips that label rather than
// applying it. Sourcing from the ready view means dependency-unblocked is
// enforced server-side (a parent-child edge to an open epic is NOT a blocking
// dep, so plan children still appear); the label filter narrows the scan to the
// intake lane before any client-side work.
type autoIntakeStore struct {
	daemon *beadsapi.Client
	// intakeLabel is the label the sweep strips; also the label LabelAddedAt
	// watches for in beads.label.added events so the grace window keys off the
	// actual intake label, not any label addition.
	intakeLabel string
}

func (a autoIntakeStore) intakeLabelOrDefault() string {
	l := strings.TrimSpace(a.intakeLabel)
	if l == "" {
		return scheduler.DefaultIntakeLabel
	}
	return l
}

func (a autoIntakeStore) ListIntake(ctx context.Context) ([]scheduler.IntakeBead, error) {
	res, err := a.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:      []string{"task", "bug", "feature"},
		Statuses:   []string{"open"},
		Labels:     []string{a.intakeLabelOrDefault()},
		Unassigned: true,
		NoOpenDeps: true,
		Limit:      autoIntakeReadyLimit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]scheduler.IntakeBead, 0, len(res.Beads))
	for _, b := range res.Beads {
		out = append(out, scheduler.IntakeBead{
			ID:       b.ID,
			Type:     b.Type,
			Status:   b.Status,
			Assignee: b.Assignee,
			Labels:   b.Labels,
		})
	}
	return out, nil
}

func (a autoIntakeStore) RemoveLabel(ctx context.Context, beadID, label string) error {
	return a.daemon.RemoveLabel(ctx, beadID, label)
}

// ParentedBeads mirrors orphanStore.ParentedBeads: a forward parent-child
// dependency edge on a bead means that bead IS a child (edges are stored
// child-first), i.e. it has a parent. Batched via BatchGetDependencies.
func (a autoIntakeStore) ParentedBeads(ctx context.Context, beadIDs []string) (map[string]bool, error) {
	out := make(map[string]bool, len(beadIDs))
	if len(beadIDs) == 0 {
		return out, nil
	}
	depsByID, err := a.daemon.BatchGetDependencies(ctx, beadIDs, "forward")
	if err != nil {
		return nil, err
	}
	for id, deps := range depsByID {
		for _, d := range deps {
			if d.Type == "parent-child" && d.DependsOnID != "" {
				out[id] = true
				break
			}
		}
	}
	return out, nil
}

// LabelAddedAt returns, per bead id that has one, the timestamp of the most
// recent beads.label.added event for the intake label. Beads with no such event
// are absent from the map (the sweep treats absent as zero ⇒ old enough, so a
// bead whose label add predates the bounded scan is never permanently pinned).
// There is no batch endpoint, so this issues per-id GetRecentEventsByTopic
// calls — acceptable because the sweeper only calls it for the small
// post-pass-1 candidate set, never the whole intake view, and each call ships
// only label.added rows (gd-cP9tODid8j). Bounded scan (autoIntakeEventScan)
// caps the per-bead fetch so a bead with a large event log can't OOM the sweep
// (same guard as orphanStore.LastDeLane, bug ka-0000002256).
func (a autoIntakeStore) LabelAddedAt(ctx context.Context, beadIDs []string) (map[string]time.Time, error) {
	intakeLabel := a.intakeLabelOrDefault()
	out := make(map[string]time.Time, len(beadIDs))
	for _, id := range beadIDs {
		events, err := a.daemon.GetRecentEventsByTopic(ctx, id, beadsEventTopicLabelAdded, autoIntakeEventScan)
		if err != nil {
			return nil, err
		}
		var latest time.Time
		for _, e := range events {
			if e.Topic != beadsEventTopicLabelAdded {
				continue
			}
			if !eventLabelMatches(e.Payload, intakeLabel) {
				continue
			}
			if e.CreatedAt.After(latest) {
				latest = e.CreatedAt
			}
		}
		if !latest.IsZero() {
			out[id] = latest
		}
	}
	return out, nil
}

// autoIntakeReadyLimit bounds how many needs-intake candidates the auto-intake
// sweep pulls per pass. High enough to cover a realistic backlogged intake lane.
const autoIntakeReadyLimit = 1000

// autoIntakeEventScan bounds how many of a candidate bead's most-recent
// beads.label.added events LabelAddedAt scans for the intake label. Topic-only
// fetch with a cap on the per-bead response (see orphanDeLaneEventScan).
const autoIntakeEventScan = 200

// runAutoIntakeSweep drives the auto-intake sweep until ctx is cancelled. It
// polls on a fixed cadence and lets the sweeper self-throttle to its configured
// interval, so the poll period is decoupled from the sweep period. Mirrors
// runOrphanSweep.
func runAutoIntakeSweep(ctx context.Context, sweeper *scheduler.AutoIntakeSweeper, interval time.Duration, logger *slog.Logger) {
	poll := time.Minute
	if interval > 0 && interval < poll {
		poll = interval
	}
	// Initial sweep on startup so a code-ready plan child stranded behind the
	// label is de-laned promptly rather than waiting a full interval.
	sweeper.Tick(ctx, time.Now())

	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Info("auto-intake sweep stopping")
			return
		case <-ticker.C:
			sweeper.Tick(ctx, time.Now())
		}
	}
}

// runOrphanSweep drives the orphan triage sweep until ctx is cancelled. It
// polls on a fixed cadence and lets the sweeper self-throttle to its
// configured interval, so the poll period is decoupled from the sweep period.
func runOrphanSweep(ctx context.Context, sweeper *scheduler.OrphanSweeper, interval time.Duration, logger *slog.Logger) {
	// Poll frequently; OrphanSweeper.Tick self-throttles to its own interval.
	poll := time.Minute
	if interval > 0 && interval < poll {
		poll = interval
	}
	// Initial sweep on startup so freshly-orphaned work is surfaced promptly.
	sweeper.Tick(ctx, time.Now())

	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Info("orphan triage sweep stopping")
			return
		case <-ticker.C:
			sweeper.Tick(ctx, time.Now())
		}
	}
}

// runNeedsGardenerSweep drives the needs-gardener backstop sweep until ctx is
// cancelled. It polls on a fixed cadence and lets the sweeper self-throttle to
// its configured interval, mirroring runOrphanSweep.
func runNeedsGardenerSweep(ctx context.Context, sweeper *scheduler.NeedsGardenerSweeper, interval time.Duration, logger *slog.Logger) {
	poll := time.Minute
	if interval > 0 && interval < poll {
		poll = interval
	}
	// Initial sweep on startup so an existing leak is surfaced promptly.
	sweeper.Tick(ctx, time.Now())

	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Info("needs-gardener backstop sweep stopping")
			return
		case <-ticker.C:
			sweeper.Tick(ctx, time.Now())
		}
	}
}

// startSchedulerNATSListeners spawns the three scheduler NATS listeners
// (decision_resolved, bead_closed, mail_sent) against the supplied trigger
// engine. The listeners no-op when natsURL is empty, so tests and local dev
// runs without a NATS server work unchanged.
//
// Subscriptions use core NATS subscribe (not queue subscribe). Until the
// controller-side listeners are torn down (SCHEDULER_NATS_LISTENERS_ENABLED=false),
// both processes receive each event — the dedup happens downstream on the
// per-trigger seen map.
func startSchedulerNATSListeners(ctx context.Context, engine *scheduler.EntryEngine, natsURL, natsToken, natsUser string, logger *slog.Logger) {
	if natsURL == "" {
		logger.Info("scheduler NATS listeners disabled (no NATS_URL)")
		return
	}
	if engine == nil {
		logger.Warn("scheduler NATS listeners disabled (no trigger engine)")
		return
	}

	decisionListener := scheduler.NewDecisionEventListener(engine, logger)
	go func() {
		if err := decisionListener.Start(ctx, natsURL, natsToken, natsUser); err != nil {
			logger.Error("decision event listener failed", "error", err)
		}
	}()

	beadClosedListener := scheduler.NewBeadClosedEventListener(engine, logger)
	go func() {
		if err := beadClosedListener.Start(ctx, natsURL, natsToken, natsUser); err != nil {
			logger.Error("bead-closed event listener failed", "error", err)
		}
	}()

	mailListener := scheduler.NewMailEventListener(engine, logger)
	go func() {
		if err := mailListener.Start(ctx, natsURL, natsToken, natsUser); err != nil {
			logger.Error("mail event listener failed", "error", err)
		}
	}()

	logger.Info("scheduler NATS listeners started", "nats_url", natsURL)
}

// loadSpawnGuard builds a SpawnGuard from SPAWN_GUARD_* env vars, matching
// the controller defaults (30 spawns/min soft, 120 spawns/min trip, 5m cooldown).
func loadSpawnGuard() *spawnguard.SpawnGuard {
	cfg := spawnguard.DefaultConfig()
	if v := os.Getenv("SPAWN_GUARD_RATE_MAX"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.RateMax = n
		}
	}
	if v := os.Getenv("SPAWN_GUARD_RATE_WINDOW"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.RateWindow = d
		}
	}
	if v := os.Getenv("SPAWN_GUARD_TRIP_THRESHOLD"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.TripThreshold = n
		}
	}
	if v := os.Getenv("SPAWN_GUARD_TRIP_WINDOW"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.TripWindow = d
		}
	}
	if v := os.Getenv("SPAWN_GUARD_TRIP_COOLDOWN"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.TripCooldown = d
		}
	}
	return spawnguard.New(cfg)
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envBoolDefault parses a boolean env var, falling back to def when unset or
// unparseable. Accepts the usual strconv.ParseBool forms (1/0/true/false/...).
func envBoolDefault(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

// envCSVList parses a comma-separated env var into a trimmed, de-blanked slice.
// An unset or all-blank value yields nil (the empty-list no-op case). Used for
// GARDENER_LIVENESS_WATCH_IDS; sanitizeWatchIDs in the engine de-dupes on top.
func envCSVList(key string) []string {
	v := os.Getenv(key)
	if v == "" {
		return nil
	}
	var out []string
	for part := range strings.SplitSeq(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
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
