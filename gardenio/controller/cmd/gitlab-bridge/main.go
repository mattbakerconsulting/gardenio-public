// Command gitlab-bridge is a standalone service that watches GitLab for
// merge request merges and updates bead fields (mr_merged, mr_state, etc.).
//
// It runs several subsystems:
//   - Webhook handler: receives GitLab MR events via POST /webhook
//   - Polling fallback: periodic check for recently merged MRs
//   - SSE watcher: listens for bead updates with mr_url → checks MR status
//   - Trigger engine: evaluates GitLab trigger beads (MR / pipeline) and
//     dispatches notify / spawn_agent / apply_formula / create_bead actions
//
// This service has ZERO K8s dependencies and can run as a lightweight
// standalone container alongside the gardenio controller.
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
	slackapi "github.com/slack-go/slack"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/covflush"
	"gardenio/controller/internal/spawnguard"
	"gardenio/controller/internal/tracing"
	"gardenio/controller/internal/trustmetrics"
)

var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	defer covflush.Flush()
	bridge.NudgeSender = "gitlab-bridge" // from: label on the mail bridge.NudgeAgent sends
	cfg := parseConfig()

	logger := setupLogger(cfg.logLevel)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	if err := run(ctx, cfg, logger); err != nil {
		logger.Error("gitlab-bridge exited with error", "error", err)
		os.Exit(1)
	}
}

// run wires up every gitlab-bridge subsystem and blocks until ctx is cancelled,
// then shuts the HTTP listeners down gracefully. It is split out of main so the
// startup/shutdown wiring is unit-testable: a test can pass an already-cancelled
// (or short-lived) context plus a config that points the listeners at ephemeral
// ports and the beads daemon at an unreachable address — every goroutine then
// starts, fails gracefully against the dead daemon, and run returns once ctx is
// done. Fatal setup errors are returned (main maps them to os.Exit(1)) instead
// of calling os.Exit directly, so tests can assert on the error path too.
func run(ctx context.Context, cfg *config, logger *slog.Logger) error {
	shutdownTracing, err := tracing.Init(context.Background(), logger, tracing.Options{
		ServiceName:    "gardenio-gitlab-bridge",
		ServiceVersion: version,
		Environment:    os.Getenv("GARDENIO_ENV"),
	})
	if err != nil {
		return fmt.Errorf("initialize tracing: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTracing(shutdownCtx); err != nil {
			logger.Warn("tracing shutdown error", "error", err)
		}
	}()

	logger.Info("starting gitlab-bridge",
		"version", version,
		"commit", commit,
		"beads_http", cfg.beadsHTTPAddr,
		"gitlab_base_url", cfg.gitlabBaseURL,
		"gitlab_group_id", cfg.gitlabGroupID,
		"poll_interval", cfg.pollInterval,
		"trigger_poll_interval", cfg.triggerPollInterval,
		"listen_addr", cfg.listenAddr)

	// Create beads daemon HTTP client.
	daemon, err := beadsapi.New(beadsapi.Config{
		HTTPAddr: cfg.beadsHTTPAddr,
		Token:    os.Getenv("BEADS_DAEMON_TOKEN"),
	})
	if err != nil {
		return fmt.Errorf("create beads daemon client: %w", err)
	}
	defer daemon.Close()

	// Register bead types, views, and context configs with the daemon.
	if err := bridge.EnsureConfigs(context.Background(), daemon, logger); err != nil {
		logger.Warn("failed to ensure beads configs (non-fatal)", "error", err)
	}

	// State persistence for SSE last-event-ID.
	state, err := bridge.NewStateManager(cfg.statePath)
	if err != nil {
		return fmt.Errorf("load state from %s: %w", cfg.statePath, err)
	}
	logger.Info("state manager loaded", "path", cfg.statePath)

	// Create GitLab client.
	gitlabClient := bridge.NewGitLabClient(bridge.GitLabClientConfig{
		BaseURL: cfg.gitlabBaseURL,
		Token:   cfg.gitlabAPIToken,
		Logger:  logger,
	})

	// HTTP server with health endpoints and webhook handler.
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","version":"%s"}`, version)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok"}`)
	})

	// Nudges go out as interrupt mail (bridge.NudgeAgent).
	nudgeFunc := func(ctx context.Context, agentName, message string) error {
		return bridge.NudgeAgent(ctx, daemon, logger, agentName, message)
	}

	// Agent resolver for MR review comment agent lookup/spawn.
	agentResolver := bridge.NewAgentResolver(bridge.AgentResolverConfig{
		Daemon: daemon,
		GitLab: gitlabClient,
		Nudge:  nudgeFunc,
		Logger: logger,
	})

	// User directory — indexes type=user,kind=config beads by gitlab_username
	// for the mention permission gate. Auto-refreshes every 5 minutes so
	// `kd update <user-bead>` grants propagate without a redeploy.
	userDir := bridge.NewUserDirectory(daemon, logger)
	if err := userDir.Refresh(ctx); err != nil {
		logger.Warn("initial user directory refresh failed — lookups will retry on demand", "error", err)
	}

	// Mention handler for @gardenio mentions in GitLab comments.
	// Triggers on the friendly name (e.g., "@gardenio") AND the actual GitLab
	// bot username (e.g., "@group_14176856_bot_...") so both autocomplete forms work.
	// Authorization is gated on the commenter's user bead having the "mention"
	// capability — see bridge.GitLabMentionCapability.
	mentionHandler := bridge.NewMentionHandler(bridge.MentionHandlerConfig{
		Daemon:      daemon,
		GitLab:      gitlabClient,
		Trigger:     cfg.mentionTrigger,
		BotUsername: cfg.gitlabBotUsername,
		Project:     cfg.mentionProject,
		Users:       userDir,
		Logger:      logger,
	})

	// Reverse-provenance acceptance reconciler (E-PROV FW-3, ka-0000002343). It
	// emits the R5 acceptance signal: after an upstream peel MR bead is closed
	// (by the webhook merge path or the scoped backstop poller), it walks the
	// reverse `produces` edge to the peel-request bead(s) and stamps
	// upstream_accepted_at / upstream_mr_url / upstream_merged_by — and NEVER
	// closes a dark work bead. Built only on the upstream-pointed bridge (same
	// opt-in as the backstop poller); the dark bridge leaves it nil. Its scope
	// guard (UPSTREAM_BACKSTOP_LABEL, the FW-5 marker) means even if it sees a
	// non-upstream MR close it skips it. Honors read-only-mirror ka-0000003234.
	var acceptanceReconciler *bridge.UpstreamAcceptanceReconciler
	if cfg.upstreamBackstopEnabled {
		acceptanceReconciler = bridge.NewUpstreamAcceptanceReconciler(bridge.UpstreamAcceptanceReconcilerConfig{
			Daemon:     daemon,
			Logger:     logger,
			ScopeLabel: cfg.upstreamBackstopLabel,
		})
	}

	// AI-authored MR human-approval enforcer (DEVOPS-730). nil when disabled —
	// the webhook handler treats a nil enforcer as "feature off".
	aiApproval := bridge.NewAIApprovalEnforcer(gitlabClient, bridge.AIApprovalConfig{
		Enabled:           cfg.aiApprovalEnabled,
		BotUsernames:      cfg.aiApprovalBotUsernames,
		RuleName:          cfg.aiApprovalRuleName,
		ApprovalsRequired: cfg.aiApprovalRequired,
		ReviewerGroupIDs:  cfg.aiApprovalReviewerGroupIDs,
		ReviewerUserIDs:   cfg.aiApprovalReviewerUserIDs,
	}, logger)
	if aiApproval != nil {
		logger.Info("ai-approval enforcer enabled",
			"bot_usernames", cfg.aiApprovalBotUsernames,
			"approvals_required", cfg.aiApprovalRequired,
			"reviewer_group_ids", cfg.aiApprovalReviewerGroupIDs,
			"reviewer_user_ids", cfg.aiApprovalReviewerUserIDs)
	}

	// Seed the inbound-webhook delivery counter to its zero baseline so
	// increase()/absent()/rate() have a sample the instant the pod is first
	// scraped — the receive-path liveness signal for the GitLab door of the
	// non-AVA webhook carve-out (ka-0000005632; mirrors the jira-bridge deadman
	// suite, gardenio/docs/security/non-ava-webhook-carveout.md).
	bridge.InitGitLabWebhookMetrics()

	// Inbound-webhook DARK-HOOK DEADMAN monitor (GAP-C of ka-0000005632; the
	// GitLab twin of the jira-bridge deadman). Periodically folds the "0
	// authenticated deliveries in the window WHILE upstream MR activity exists"
	// verdict into gardenio_gitlab_webhook_dark. This is the detector the
	// deliveries counter alone cannot be: in the fully-dark case (deregistered /
	// mis-secreted hook) nothing increments the counter, so a COUNTER cannot tell
	// dark from genuinely-quiet — the exact blind spot that left the Jira door
	// silently dark in prod for 8 days (bug ka-0000000426). Needs a group scope
	// for the upstream-activity confirmation arm, so it is disabled when
	// GITLAB_GROUP_ID is 0 (the confirmation arm would always read empty and
	// never page).
	if cfg.gitlabGroupID != 0 {
		webhookMonitor := bridge.NewGitLabWebhookMonitor(bridge.GitLabWebhookMonitorConfig{
			GitLab:   gitlabClient,
			GroupID:  cfg.gitlabGroupID,
			Window:   cfg.gitlabWebhookDeadmanWindow,
			Interval: cfg.gitlabWebhookDeadmanInterval,
			Logger:   logger,
		})
		go func() {
			if err := webhookMonitor.Run(ctx); err != nil && ctx.Err() == nil {
				logger.Error("GitLab webhook deadman monitor stopped", "error", err)
			}
		}()
		logger.Info("gitlab-webhook deadman monitor enabled",
			"group_id", cfg.gitlabGroupID,
			"window", cfg.gitlabWebhookDeadmanWindow,
			"interval", cfg.gitlabWebhookDeadmanInterval)
	} else {
		logger.Info("gitlab-webhook deadman monitor disabled (GITLAB_GROUP_ID is 0; the upstream-activity confirmation arm needs a group scope)")
	}

	// Webhook endpoint for GitLab MR events.
	mux.Handle("/webhook", bridge.GitLabWebhookHandlerWithConfig(bridge.GitLabWebhookConfig{
		GitLab:               gitlabClient,
		Daemon:               daemon,
		WebhookSecret:        cfg.gitlabWebhookSecret,
		BotUsername:          cfg.gitlabBotUsername,
		Nudge:                nudgeFunc,
		AgentResolver:        agentResolver,
		MentionHandler:       mentionHandler,
		Users:                userDir,
		AcceptanceReconciler: acceptanceReconciler,
		AIApproval:           aiApproval,
		Logger:               logger,
	}))

	srv := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           otelhttp.NewHandler(mux, "gitlab-bridge"),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		logger.Info("starting HTTP server", "addr", cfg.listenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP server failed", "error", err)
		}
	}()

	// Internal-only Prometheus metrics server. Serves the default gatherer
	// where client_golang's Go/process collectors and the bridge's
	// promauto-registered metrics land — including the gardener-loop resume
	// metrics this binary's TriggerEngine increments (gardener_resume_total /
	// gardener_resumes_without_progress, bead ka-0000005353). Exposing /metrics
	// here makes them scrapeable (decision ka-0000006918). Separate port from
	// the public /webhook listener so /metrics is never internet-reachable.
	metricsSrv := buildMetricsServer(cfg.metricsAddr)
	go func() {
		logger.Info("starting metrics server", "addr", cfg.metricsAddr)
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics server failed", "error", err)
		}
	}()

	// Tear the HTTP listeners down on every return path — the normal
	// ctx-cancelled shutdown below AND any fatal setup error returned after
	// this point (e.g. trust-metrics registration). Registering the shutdown as
	// a defer here, right after both servers start, means an early error return
	// no longer leaks the two listeners. http.Server.Shutdown is idempotent, so
	// the explicit ctx-done shutdown sequencing below still works unchanged.
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("HTTP server shutdown error", "error", err)
		}
		if err := metricsSrv.Shutdown(shutdownCtx); err != nil {
			logger.Error("metrics server shutdown error", "error", err)
		}
	}()

	// Start GitLab MR poller goroutine (fallback for webhooks).
	poller := bridge.NewGitLabPoller(bridge.GitLabPollerConfig{
		GitLab:       gitlabClient,
		Daemon:       daemon,
		Logger:       logger,
		GroupID:      cfg.gitlabGroupID,
		PollInterval: cfg.pollInterval,
		Users:        userDir,
	})
	go func() {
		if err := poller.Run(ctx); err != nil && ctx.Err() == nil {
			logger.Error("GitLab poller stopped", "error", err)
		}
	}()

	// Scoped upstream MR backstop poller (E-PROV FW-2, ka-0000007488). This is
	// the missed-webhook BACKSTOP for upstream peel MRs — NOT the primary path.
	// The real-monorepo `merge_request` webhook into the /webhook handler above
	// is primary (payload-driven, host-blind). This poller enumerates only the
	// small set of OPEN upstream-mirror beads (label-scoped) and does a single
	// per-MR GET each, so it touches nothing but human-owned peel MRs and never
	// pushes outbound (honors read-only-mirror invariant ka-0000003234).
	// Opt-in: disabled unless UPSTREAM_BACKSTOP_POLL=true (the upstream-pointed
	// bridge enables it; the dark-pointed bridge leaves it off). See
	// deploy-values/gitlab-dark/bridge/UPSTREAM-WEBHOOK.md.
	if cfg.upstreamBackstopEnabled {
		upstreamPoller := bridge.NewGitLabUpstreamPoller(bridge.GitLabUpstreamPollerConfig{
			GitLab:       gitlabClient,
			Daemon:       daemon,
			Logger:       logger,
			PollInterval: cfg.upstreamBackstopInterval,
			ScopeLabel:   cfg.upstreamBackstopLabel,
			Host:         gitlabClient.Host(),
			Acceptance:   acceptanceReconciler,
		})
		go func() {
			if err := upstreamPoller.Run(ctx); err != nil && ctx.Err() == nil {
				logger.Error("upstream backstop poller stopped", "error", err)
			}
		}()
	}

	// Escalation auto-reaper (ka-0000004105 + ka-0000007203). Periodically closes
	// open escalations a human already satisfied out-of-band — the reverse edge
	// the escalation data plane never had, so the operator queue stops carrying
	// ~15% dead-oldest phantoms that age-ordering sorts to the top. Two
	// detectors: (1) MR-witness — the approve/merge ask was merged/approved in
	// the GitLab UI; verb-allowlist guarded so it NEVER reaps an apply/deploy/
	// provision/mint ask even one referencing a merged MR (the completion of such
	// an ask is the apply, which no MR state can attest to). (2) Orchestration-
	// layer — the operator recorded the resolution on the bead (terminal
	// result_status on a still-open escalation, or a human-operator resolve-
	// comment) without closing it; no GitLab call, so it covers the non-MR asks
	// detector 1 cannot. (3) MR-landed review flag (ka-0000002563) — the
	// COMPLEMENT: a NON-reapable ask (apply/deploy/mint, or a merge-then-apply
	// shape) whose referenced MR has now merged/closed is flipped to needs-review
	// instead of closed, so the operator gets one signal instead of the endless
	// stale nag; it NEVER closes. ON by default; ESCALATION_REAP=off disables the
	// reaper, ESCALATION_REVIEW_FLAG=off disables detector 3 alone.
	// Host-scoped like the pollers above so it never GETs a foreign-host MR.
	if cfg.escalationReapEnabled {
		reaper := bridge.NewEscalationReaper(bridge.EscalationReaperConfig{
			GitLab:         gitlabClient,
			Daemon:         daemon,
			Logger:         logger,
			Interval:       cfg.escalationReapInterval,
			Host:           gitlabClient.Host(),
			DryRun:         cfg.escalationReapDryRun,
			ReviewFlag:     cfg.escalationReviewFlag,
			GroupNamespace: cfg.escalationMRGroupNamespace,
		})
		go func() {
			if err := reaper.Run(ctx); err != nil && ctx.Err() == nil {
				logger.Error("escalation reaper stopped", "error", err)
			}
		}()
		logger.Info("escalation auto-reaper enabled",
			"interval", cfg.escalationReapInterval, "dry_run", cfg.escalationReapDryRun,
			"review_flag", cfg.escalationReviewFlag)
	}

	// GitLab-mention dispatch catchup watchdog (ka-0000000279, plan ka-0000004007
	// Track A). Periodically respawns crew for spawn_source=gitlab-mention task
	// beads that sat open past a grace window with no live linked agent bead —
	// the "webhook created the task, then SpawnAgent failed / the bridge died and
	// nothing retried" hole that silently dropped all three of Patrick's !1160
	// review comments. Idempotent (a respawned task gains a live agent bead and
	// drops out of the next sweep) and single-claim guarded (SpawnAgent's
	// findLiveAgentForTask refuses a duplicate if a re-mention raced in). ON by
	// default; MENTION_CATCHUP=off disables it.
	if cfg.mentionCatchupEnabled {
		watchdog := bridge.NewMentionCatchupWatchdog(bridge.MentionCatchupConfig{
			Daemon: daemon,
			// GitLab lets the sweep re-verify a task's linked MR state and skip
			// respawning crew onto a merged/closed MR (ka-0000007740). The
			// watchdog fails open when this is nil, so an unconfigured GitLab
			// client degrades to the pre-guard behaviour rather than dropping
			// real asks.
			GitLab:   gitlabClient,
			Logger:   logger,
			Interval: cfg.mentionCatchupInterval,
			Grace:    cfg.mentionCatchupGrace,
			Project:  cfg.mentionProject,
			DryRun:   cfg.mentionCatchupDryRun,
		})
		go func() {
			if err := watchdog.Run(ctx); err != nil && ctx.Err() == nil {
				logger.Error("gitlab-mention catchup watchdog stopped", "error", err)
			}
		}()
		logger.Info("gitlab-mention catchup watchdog enabled",
			"interval", cfg.mentionCatchupInterval, "grace", cfg.mentionCatchupGrace,
			"dry_run", cfg.mentionCatchupDryRun)
	}

	// Start the reverse MR→bead reconciler (bug ka-0000004261). Where the
	// forward poller above watches beads that already carry mr_url, this scans
	// GitLab for recently-updated MRs that have no MR bead yet and back-links
	// each to its owning bead via a PE-/FSM-/gd- key — self-healing a forgotten
	// `gb mr link`. It never guesses on ambiguity. Disabled when group_id is 0.
	backlinker := bridge.NewMRBacklinkReconciler(bridge.MRBacklinkConfig{
		GitLab:              gitlabClient,
		Daemon:              daemon,
		Logger:              logger,
		GroupID:             cfg.gitlabGroupID,
		Interval:            cfg.mrBacklinkInterval,
		Window:              cfg.mrBacklinkWindow,
		DryRun:              cfg.mrBacklinkDryRun,
		FileUnlinkedBeads:   cfg.mrBacklinkFileUnlinked,
		UnlinkedBeadProject: cfg.mentionProject,
	})
	go func() {
		if err := backlinker.Run(ctx); err != nil && ctx.Err() == nil {
			logger.Error("reverse MR→bead reconciler stopped", "error", err)
		}
	}()

	// Start the MR-reopen drift sweep (ka-0000000267, direction 2 of
	// ka-0000003953). The inverse of the backlinker: where that links orphan
	// MRs to beads, this re-opens CLOSED issue beads whose linked MR is still
	// `opened` (not merged), repairing pre-existing close-drift that the
	// close-time guard (MR !6190) can only prevent going forward. ON by default
	// as the repair half of that guard; MR_REOPEN_DRIFT=off disables it.
	// Disabled when group_id is 0.
	if cfg.mrReopenDriftEnabled {
		reopenSweep := bridge.NewMRReopenDriftReconciler(bridge.MRReopenDriftConfig{
			GitLab:   gitlabClient,
			Daemon:   daemon,
			Logger:   logger,
			GroupID:  cfg.gitlabGroupID,
			Interval: cfg.mrReopenDriftInterval,
			Window:   cfg.mrReopenDriftWindow,
			DryRun:   cfg.mrReopenDriftDryRun,
		})
		go func() {
			if err := reopenSweep.Run(ctx); err != nil && ctx.Err() == nil {
				logger.Error("MR-reopen drift sweep stopped", "error", err)
			}
		}()
	}

	// Start the MR-terminalize drift sweep (ka-0000001293, residual half of
	// ka-0000001223 / ka-0000002394). The inverse of the reopen sweep above:
	// where that re-opens closed beads whose MR is still open, this terminalizes
	// stale MR beads (mr_state=opened, not updated within the stale window) whose
	// MR has actually merged/closed on GitLab. It drains the historical drift
	// backlog — MRs that terminalized months ago while sync was down or before
	// they were linked — that no windowed group scan can reach and that the live
	// webhook/poll path never backfilled. Bounded: stale-gated + oldest-first +
	// capped fan-out per pass. ON by default; MR_TERMINALIZE_DRIFT=off disables.
	if cfg.mrTerminalizeDriftEnabled {
		terminalizeSweep := bridge.NewMRTerminalizeDriftReconciler(bridge.MRTerminalizeDriftConfig{
			GitLab:     gitlabClient,
			Daemon:     daemon,
			Logger:     logger,
			Interval:   cfg.mrTerminalizeDriftInterval,
			StaleAge:   cfg.mrTerminalizeDriftStaleAge,
			MaxPerScan: cfg.mrTerminalizeDriftMaxScan,
			DryRun:     cfg.mrTerminalizeDriftDryRun,
		})
		go func() {
			if err := terminalizeSweep.Run(ctx); err != nil && ctx.Err() == nil {
				logger.Error("MR-terminalize drift sweep stopped", "error", err)
			}
		}()
	}

	// Start the MR schema-flag bot (G5, ka-0000007779). A review-time yellow
	// flag that complements the authoritative CI drift guard (G1): scans open
	// MRs and comments once on any that add a type:* field to the bridge's
	// init.go configs() while leaving the daemon's kbeads/config.go untouched —
	// the exact shape of the four schema-drift outages (ka-0000001549,
	// ka-0000004509, ka-0000000811, ka-0000003673). It only comments + labels;
	// it never merges. ON by default; MR_SCHEMA_FLAG=off disables it. Disabled
	// when group_id is 0.
	if cfg.mrSchemaFlagEnabled {
		schemaFlagBot := bridge.NewMRSchemaFlagReconciler(bridge.MRSchemaFlagConfig{
			GitLab:   gitlabClient,
			Logger:   logger,
			GroupID:  cfg.gitlabGroupID,
			Interval: cfg.mrSchemaFlagInterval,
			Window:   cfg.mrSchemaFlagWindow,
			DryRun:   cfg.mrSchemaFlagDryRun,
		})
		go func() {
			if err := schemaFlagBot.Run(ctx); err != nil && ctx.Err() == nil {
				logger.Error("MR schema-flag bot stopped", "error", err)
			}
		}()
	}

	// R5 Trusted-Throughput metrics pipeline (ka-0000000493, bundle
	// ka-0000008221 §3.4). Opt-in: TRUST_METRICS_ENABLED=true. Joins dark-group
	// trickle MRs (diff stats, timestamps, approvals, notes from the GitLab MR
	// API) with beads (peel-request timestamp + provenance bead links) and
	// publishes R5 metrics 1-10 + their balancing metrics as gardenio_trust_*
	// gauges on the default registry, served by the internal /metrics endpoint
	// above. This is the blocking infra for the trust dashboard (ka-0000007085)
	// and the terseness gate (ka-0000007571). Disabled when group_id is 0.
	if cfg.trustMetricsEnabled && cfg.gitlabGroupID != 0 {
		trustCollector := trustmetrics.NewCollector()
		if err := trustCollector.Register(prometheus.DefaultRegisterer); err != nil {
			return fmt.Errorf("register R5 trust metrics: %w", err)
		}
		trustPeriodic := trustmetrics.NewPeriodicCollector(trustmetrics.CollectorConfig{
			GitLab:   bridge.NewTrustMetricsGitLabSource(gitlabClient, cfg.gitlabGroupID, cfg.trustMetricsBranches),
			Beads:    bridge.NewTrustMetricsBeadSource(daemon),
			Metrics:  trustCollector,
			Logger:   logger,
			Interval: cfg.trustMetricsInterval,
			Window:   cfg.trustMetricsWindow,
			BotUser:  cfg.gitlabBotUsername,
		})
		go func() {
			if err := trustPeriodic.Run(ctx); err != nil && ctx.Err() == nil {
				logger.Error("R5 trust-metrics collector stopped", "error", err)
			}
		}()
		logger.Info("R5 trust-metrics collector started",
			"interval", cfg.trustMetricsInterval, "window", cfg.trustMetricsWindow,
			"target_branches", cfg.trustMetricsBranches)
	}

	// Create event deduplicator.
	dedup := bridge.NewDedup(logger)
	go dedup.StartCleanup(ctx)

	// Create SSE event stream for MR status checking, squawk forwarding, and
	// live user-directory updates. `beads.bead.created` is required so
	// newly-created user beads land in the directory without waiting for the
	// 5-minute periodic refresh — same gap that hit slack-bridge under
	// ka-0000001175.
	sseStream := bridge.NewSSEStream(bridge.SSEStreamConfig{
		BeadsHTTPAddr: cfg.beadsHTTPAddr,
		Topics:        []string{"beads.bead.created", "beads.bead.updated", "beads.bead.closed", "beads.comment.added"},
		Token:         os.Getenv("BEADS_DAEMON_TOKEN"),
		Logger:        logger,
		Dedup:         dedup,
		State:         state,
	})

	// Keep the user directory in sync with bead lifecycle events so a
	// just-created or just-edited user bead is reflected without waiting
	// for the periodic refresh. Mirrors the slack-bridge wiring under
	// ka-0000001175; without this, the @mention authorization gate stays
	// blind to new users for up to 5 minutes (and indefinitely if the
	// startup Refresh failed silently).
	userDir.RegisterHandlers(sseStream)

	// Host scope (FW-1 / ka-0000006221): when this bridge points at a specific
	// GitLab host, scope all SSE-driven MR handling to that host so multiple
	// bridges (e.g. prod + gitlab-dark) can share ONE beads daemon + SSE stream
	// without reacting to each other's mr_urls. Derived from GITLAB_BASE_URL;
	// empty base URL → empty host → match-all (single-bridge behavior).
	bridgeHost := gitlabClient.Host()
	if bridgeHost != "" {
		logger.Info("scoping MR handling to GitLab host", "host", bridgeHost)
	}

	// Register GitLab sync handler on the SSE stream (reuses agentResolver from above).
	gitlabSync := bridge.NewGitLabSync(bridge.GitLabSyncConfig{
		GitLab:        gitlabClient,
		Daemon:        daemon,
		Logger:        logger,
		Resolver:      agentResolver,
		Nudge:         nudgeFunc,
		Users:         userDir,
		ViewerBaseURL: cfg.beadsViewerURL,
		JIRABaseURL:   cfg.jiraBaseURL,
		Host:          bridgeHost,
	})
	gitlabSync.RegisterHandlers(sseStream)

	// Startup catch-up: the SSE stream starts from the persisted Last-Event-ID
	// (or "now" on a fresh-state boot), so a task bead that already carried an
	// mr_url before this pod started never gets a bead.updated event and its MR
	// misses the managed "Agent Context" description block until some unrelated
	// tick touches the bead. Re-sync every active task bead with an mr_url now.
	// syncMRDescription is an idempotent PUT of just the managed block (no
	// external-corruption risk), so this is safe on every boot — the gap this
	// closes is bug ka-0000002703 (Finding 2). Runs before the SSE stream starts
	// so its dedup map is seeded first.
	gitlabSync.CatchUpDescriptions(ctx, daemon, cfg.mrDescCatchupLimit)

	// Register MR-URL comment autolinker — when a new bead comment contains a
	// GitLab MR URL (e.g. an agent posting "MR opened: <url>"), it back-fills
	// the parent bead's mr_url field, which in turn drives gitlabSync above to
	// project the type=merge-request bead + produces/triggered-by edges. Closes
	// the comment->mr_url gap (task ka-0000003369).
	mrCommentLinker := bridge.NewMRCommentLinker(bridge.MRCommentLinkerConfig{
		Daemon: daemon,
		Logger: logger,
	})
	mrCommentLinker.RegisterHandlers(sseStream)

	// Register GitLab squawk forwarder — posts agent squawk messages as MR notes.
	squawkFwd := bridge.NewGitLabSquawkForwarder(bridge.GitLabSquawkForwarderConfig{
		Daemon: daemon,
		GitLab: gitlabClient,
		Logger: logger,
		Host:   bridgeHost,
	})
	squawkFwd.RegisterHandlers(sseStream)

	// Start the SSE stream.
	go func() {
		if err := sseStream.Start(ctx); err != nil && ctx.Err() == nil {
			logger.Error("SSE event stream stopped", "error", err)
		}
	}()

	// Notifier for trigger "notify" actions — never nil (ka-0000008124).
	triggerNotifier := buildTriggerNotifier(cfg, daemon, logger)

	// Start the shared trigger engine for GitLab triggers (MRs and pipelines).
	// CRITICAL: CatchUp must run BEFORE the evaluation loop so the engine's
	// seen map is populated with pre-existing matches — otherwise gitlab-bridge
	// restarts would re-fire actions for every still-failed pipeline / open
	// MR on boot. This mirrors cmd/jira-bridge/main.go:166-172.
	triggerEngine := bridge.NewTriggerEngine(bridge.TriggerEngineConfig{
		Daemon:   daemon,
		Notifier: triggerNotifier,
		Logger:   logger,
		Guard:    loadSpawnGuard(),
	})
	triggerEngine.RegisterSource(bridge.NewGitLabTriggerSourceWithLogger(gitlabClient, logger))
	triggerEngine.CatchUp(ctx)
	go runTriggerEngine(ctx, triggerEngine, cfg.triggerPollInterval, logger)

	// CI/CD compute-minutes metrics collector (Phase G, ka-0000002378). Opt-in
	// per series: CICD_GITLAB_METRICS_ENABLED lights up the job-duration series
	// on the existing GAT; CICD_GITLAB_FIRSTPARTY_ENABLED lights up the
	// first-party GraphQL usage/quota series (needs a group-owner token,
	// escalation ka-0000006982 — dark until that lands). Both series live in the
	// same gitlab-bridge binary and publish gardenio_cicd_* on the shared
	// internal /metrics endpoint above. Disabled when neither flag is set.
	if cfg.cicdGitLabMetricsEnabled || cfg.cicdGitLabFirstPartyEnabled {
		cicdCollector := bridge.NewCICDMetricsCollector(bridge.CICDMetricsConfig{
			Pipelines:                gitlabClient,
			Usage:                    gitlabClient,
			Projects:                 cfg.cicdMetricsProjects,
			GitLabJobDurationEnabled: cfg.cicdGitLabMetricsEnabled,
			GitLabFirstPartyEnabled:  cfg.cicdGitLabFirstPartyEnabled,
			FirstPartyNamespaceID:    cfg.cicdFirstPartyNamespaceID,
			FirstPartyGroupPath:      cfg.cicdFirstPartyGroupPath,
			LookbackMonths:           cfg.cicdLookbackMonths,
			Interval:                 cfg.cicdMetricsInterval,
			Window:                   cfg.cicdMetricsWindow,
			Logger:                   logger,
		})
		go func() {
			if err := cicdCollector.Run(ctx); err != nil && ctx.Err() == nil {
				logger.Error("CI/CD-minutes collector stopped", "error", err)
			}
		}()
		logger.Info("cicd-metrics collector enabled",
			"job_duration", cfg.cicdGitLabMetricsEnabled,
			"first_party", cfg.cicdGitLabFirstPartyEnabled,
			"projects", cfg.cicdMetricsProjects,
			"interval", cfg.cicdMetricsInterval)
	}

	// Release-drought deadman watchdog (ka-0000002594): alerts when calver
	// tags are pushed but no GitLab Release lands for the threshold period.
	// Deliberately independent of the release pipeline's own notify legs
	// (P0 ka-0000007176): it polls GitLab directly and alerts via the
	// trigger-notice bead relay (slack-bridge posts on its own credentials),
	// a P0 bug bead, and Prometheus gauges on this binary's /metrics.
	if cfg.releaseDroughtEnabled {
		// Reuse the direct-Slack notifier when SLACK_BOT_TOKEN + SLACK_CHANNEL
		// are configured; otherwise give the watchdog its OWN notice-relay
		// notifier so its notices stay attributed to release-drought-watchdog,
		// not gitlab-bridge (the trigger-engine source). Gated on the Slack env
		// rather than nil-ness because buildTriggerNotifier now never returns
		// nil (ka-0000008124) — it already falls back to the notice relay.
		droughtNotifier := triggerNotifier
		if cfg.slackBotToken == "" || cfg.slackChannel == "" {
			droughtNotifier = bridge.NewNoticeNotifier(daemon, "release-drought-watchdog", logger)
		}
		watchdog := bridge.NewReleaseDroughtWatchdog(bridge.ReleaseDroughtConfig{
			GitLab:      gitlabClient,
			Daemon:      daemon,
			Notifier:    droughtNotifier,
			Logger:      logger,
			Project:     cfg.releaseDroughtProject,
			Threshold:   cfg.releaseDroughtThreshold,
			Interval:    cfg.releaseDroughtInterval,
			Repeat:      cfg.releaseDroughtRepeat,
			CreateBead:  cfg.releaseDroughtCreateBead,
			BeadProject: cfg.releaseDroughtBeadProject,
		})
		go func() {
			if err := watchdog.Run(ctx); err != nil && ctx.Err() == nil {
				logger.Error("release-drought watchdog stopped", "error", err)
			}
		}()
	}

	logger.Info("gitlab-bridge ready",
		"group_id", cfg.gitlabGroupID,
		"poll_interval", cfg.pollInterval,
		"trigger_poll_interval", cfg.triggerPollInterval)

	// Block until shutdown signal. The deferred shutdown above (registered right
	// after the listeners start) gracefully stops both HTTP servers on the way
	// out — on this normal path and on any earlier fatal-error return alike.
	<-ctx.Done()
	logger.Info("shutting down gitlab-bridge")
	return nil
}

// buildTriggerNotifier selects how GitLab trigger "notify" actions reach Slack,
// and is guaranteed to return a non-nil TriggerNotifier (ka-0000008124).
//
// When SLACK_BOT_TOKEN + SLACK_CHANNEL are both set, it posts directly to that
// channel on gitlab-bridge's own token. Otherwise — the prod default, since the
// gitlab-bridge Deployment sets neither env var — it falls back to the
// trigger-notice bead relay: notify actions create a closed type=message bead
// labeled trigger-notice, which slack-bridge picks up via SSE and posts on its
// OWN bot credentials. Before this fallback, an unconfigured gitlab-bridge left
// the engine notifier nil and TriggerEngine.actionNotify silently no-opped, so
// `gb trigger create gitlab-pipeline --action notify` never reached Slack even
// though last_run advanced as if it worked. Mirrors cmd/trigger-engine/main.go
// (always NewNoticeNotifier) and the release-drought watchdog fallback in run().
func buildTriggerNotifier(cfg *config, daemon bridge.NoticePublisherClient, logger *slog.Logger) bridge.TriggerNotifier {
	if cfg.slackBotToken != "" && cfg.slackChannel != "" {
		slackClient := slackapi.New(cfg.slackBotToken)
		logger.Info("trigger slack notifications enabled", "channel", cfg.slackChannel)
		return func(_ context.Context, _, message, channel string) {
			// A per-trigger channel (from the trigger bead's slack_channel
			// field) overrides the env-configured default (ka-0000008204).
			dest := channel
			if dest == "" {
				dest = cfg.slackChannel
			}
			_, _, _ = slackClient.PostMessage(dest,
				slackapi.MsgOptionText(message, false))
		}
	}
	logger.Info("trigger notifications enabled via trigger-notice relay",
		"source", "gitlab-bridge")
	return bridge.NewNoticeNotifier(daemon, "gitlab-bridge", logger)
}

// buildMetricsServer builds the internal Prometheus metrics HTTP server backed
// by the default gatherer. client_golang auto-registers the Go and process
// collectors there, and it is also where the bridge's promauto-registered
// metrics land — including the gardener-loop resume metrics
// (gardener_resume_total / gardener_resumes_without_progress, bead
// ka-0000005353) that this binary's TriggerEngine increments as it fires
// gardeners. The server is not started here; the caller invokes ListenAndServe
// in a goroutine. Mirrors slack-bridge's buildMetricsServer (cmd/slack-bridge/
// helpers.go).
func buildMetricsServer(addr string) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(prometheus.DefaultGatherer, promhttp.HandlerOpts{}))
	return &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
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

// config holds parsed environment configuration for the gitlab-bridge service.
type config struct {
	beadsHTTPAddr       string
	gitlabBaseURL       string
	gitlabAPIToken      string
	gitlabWebhookSecret string
	gitlabBotUsername   string // GitLab username of bot; notes from this user are ignored
	gitlabGroupID       int
	pollInterval        time.Duration
	triggerPollInterval time.Duration // interval for TriggerEngine evaluation loop
	// Inbound-webhook dark-hook deadman monitor tuning (GAP-C of ka-0000005632).
	// Zero (unset/malformed) lets NewGitLabWebhookMonitor apply its own defaults
	// (2h window / 5m interval). Set from GITLAB_WEBHOOK_DEADMAN_WINDOW /
	// GITLAB_WEBHOOK_DEADMAN_INTERVAL. The monitor itself only runs when
	// gitlabGroupID != 0 (the upstream-activity confirmation arm needs a group).
	gitlabWebhookDeadmanWindow   time.Duration
	gitlabWebhookDeadmanInterval time.Duration
	listenAddr                   string
	// metricsAddr is the internal-only listen address for the Prometheus
	// /metrics endpoint. gitlab-bridge runs a gardener-firing TriggerEngine, so
	// the gardener-loop resume metrics (gardener_resume_total /
	// gardener_resumes_without_progress, bead ka-0000005353) can be incremented
	// in THIS process via promauto. Without a served /metrics endpoint they
	// register against the default registry but are never scraped. Separate
	// port from the public webhook listener mirrors the controller (:9092) and
	// slack-bridge (:9093) split.
	metricsAddr    string
	logLevel       string
	statePath      string
	slackBotToken  string // optional: enables Slack notifications for trigger actions
	slackChannel   string // optional: default channel for trigger notifications
	mentionTrigger string // e.g., "@gardenio" (default)
	mentionProject string // default project for mention-spawned agents
	// Deep-link bases for the managed MR-description "Agent Context" block.
	// Empty falls back to plain-text refs. See docs/design/bead-linking-in-gitlab.md.
	beadsViewerURL string // e.g. "https://gardenio.e2e.example.com/beads"
	jiraBaseURL    string // e.g. "https://acme.atlassian.net"
	// mrDescCatchupLimit bounds how many active task beads the startup MR-
	// description catch-up (GitLabSync.CatchUpDescriptions) re-syncs, so a
	// restart never issues an unbounded burst of GitLab description PUTs. See
	// bug ka-0000002703 (Finding 2). MR_DESC_CATCHUP_LIMIT; default 512.
	mrDescCatchupLimit int
	// Reverse MR→bead reconciler (bug ka-0000004261).
	mrBacklinkInterval     time.Duration // scan cadence; default 15m
	mrBacklinkWindow       time.Duration // only MRs updated within this window; default 14d (336h)
	mrBacklinkDryRun       bool          // log intended links without writing
	mrBacklinkFileUnlinked bool          // file low-priority "unlinked MR" beads for unresolvable MRs

	// MR-reopen drift sweep (ka-0000000267, direction 2 of ka-0000003953). The
	// inverse of the backlink reconciler: scans the group's OPEN MRs and
	// re-opens any CLOSED issue bead that links one, so pre-existing
	// close-while-MR-open drift self-heals. ON by default (it is the repair
	// half of the close-time guard); MR_REOPEN_DRIFT=off disables it.
	mrReopenDriftEnabled  bool          // MR_REOPEN_DRIFT != "off"
	mrReopenDriftInterval time.Duration // MR_REOPEN_DRIFT_INTERVAL; default 15m
	mrReopenDriftWindow   time.Duration // MR_REOPEN_DRIFT_WINDOW; default 14d
	mrReopenDriftDryRun   bool          // MR_REOPEN_DRIFT_DRY_RUN=true

	// MR-terminalize drift sweep (ka-0000001293, residual half of ka-0000001223).
	// The inverse of the reopen sweep: walks STALE non-terminal MR beads
	// (mr_state=opened, not updated within the stale window) oldest-first,
	// re-fetches each MR, and terminalizes the ones GitLab reports merged/closed
	// — draining the historical drift backlog the live webhook/poll path never
	// backfilled. ON by default; MR_TERMINALIZE_DRIFT=off disables it.
	mrTerminalizeDriftEnabled  bool          // MR_TERMINALIZE_DRIFT != "off"
	mrTerminalizeDriftInterval time.Duration // MR_TERMINALIZE_DRIFT_INTERVAL; default 1h
	mrTerminalizeDriftStaleAge time.Duration // MR_TERMINALIZE_DRIFT_STALE_AGE; default 7d
	mrTerminalizeDriftMaxScan  int           // MR_TERMINALIZE_DRIFT_MAX_PER_SCAN; default 200
	mrTerminalizeDriftDryRun   bool          // MR_TERMINALIZE_DRIFT_DRY_RUN=true

	// MR schema-flag bot (G5, ka-0000007779). A review-time yellow flag: scans
	// open MRs and comments once on any that add a type:* field to the bridge's
	// init.go configs() while leaving the daemon's kbeads/config.go untouched —
	// the exact shape of the four schema-drift outages (ka-0000001549 et al.).
	// Belt-and-suspenders for the authoritative CI guard (G1). ON by default;
	// MR_SCHEMA_FLAG=off disables it. Disabled when group_id is 0.
	mrSchemaFlagEnabled  bool          // MR_SCHEMA_FLAG != "off"
	mrSchemaFlagInterval time.Duration // MR_SCHEMA_FLAG_INTERVAL; default 15m
	mrSchemaFlagWindow   time.Duration // MR_SCHEMA_FLAG_WINDOW; default 14d
	mrSchemaFlagDryRun   bool          // MR_SCHEMA_FLAG_DRY_RUN=true
	// Scoped upstream MR backstop poller (E-PROV FW-2, ka-0000007488). Opt-in;
	// the upstream-pointed bridge enables it as the missed-webhook backstop for
	// human-owned peel MRs. The webhook into /webhook remains the primary path.
	upstreamBackstopEnabled  bool          // UPSTREAM_BACKSTOP_POLL=true
	upstreamBackstopInterval time.Duration // UPSTREAM_BACKSTOP_INTERVAL; default 10m
	upstreamBackstopLabel    string        // UPSTREAM_BACKSTOP_LABEL; default "upstream:true" (FW-5 marker)

	// Escalation auto-reaper (ka-0000004105). Periodically closes open
	// escalations whose approve/merge ask a human satisfied out-of-band (merged
	// or approved the MR in the GitLab UI), so the operator queue stops carrying
	// ~15% dead-oldest phantoms. ON by default; ESCALATION_REAP=off disables it.
	// Verb-allowlist guarded — never reaps an apply/deploy/provision/mint ask.
	escalationReapEnabled  bool          // ESCALATION_REAP != "off"
	escalationReapInterval time.Duration // ESCALATION_REAP_INTERVAL; default 10m
	escalationReapDryRun   bool          // ESCALATION_REAP_DRY_RUN=true
	// escalationReviewFlag enables the MR-landed review flag (ka-0000002563): the
	// reaper's third detector, which flips a NON-reapable escalation whose
	// referenced MR has merged/closed to needs-review instead of closing it, so
	// the operator gets one signal rather than an endless stale nag. ON by
	// default; ESCALATION_REVIEW_FLAG=off disables it. Honors ESCALATION_REAP_DRY_RUN.
	escalationReviewFlag bool // ESCALATION_REVIEW_FLAG != "off"
	// escalationMRGroupNamespace expands a bare short repo name in a merge
	// escalation's title ("gardenio !7656") into a full GitLab project path so the
	// reaper can GET the MR (bug ka-0000006851). Empty → the reaper's built-in
	// default (example-org). ESCALATION_MR_GROUP_NAMESPACE overrides it for a
	// re-namespaced / self-hosted instance.
	escalationMRGroupNamespace string // ESCALATION_MR_GROUP_NAMESPACE

	// GitLab-mention dispatch catchup watchdog (ka-0000000279, A1). Periodically
	// respawns crew for spawn_source=gitlab-mention task beads whose webhook-time
	// SpawnAgent was lost (created the task, then failed to spawn / bridge died) —
	// so a dropped @gardenio request is never orphaned forever. ON by default;
	// MENTION_CATCHUP=off disables it.
	mentionCatchupEnabled  bool          // MENTION_CATCHUP != "off"
	mentionCatchupInterval time.Duration // MENTION_CATCHUP_INTERVAL; default 5m
	mentionCatchupGrace    time.Duration // MENTION_CATCHUP_GRACE; default 10m
	mentionCatchupDryRun   bool          // MENTION_CATCHUP_DRY_RUN=true

	// R5 Trusted-Throughput metrics pipeline (ka-0000000493, bundle
	// ka-0000008221 §3.4). Opt-in; off unless TRUST_METRICS_ENABLED=true. When
	// on, a periodic collector joins dark-group trickle MRs (diff stats,
	// timestamps, approvals, notes) with beads (peel-request ts, provenance
	// links, trust survey) and publishes the R5 metric table as gardenio_trust_*
	// gauges on the same internal /metrics endpoint.
	trustMetricsEnabled  bool          // TRUST_METRICS_ENABLED=true
	trustMetricsInterval time.Duration // TRUST_METRICS_INTERVAL; default 10m
	trustMetricsWindow   time.Duration // TRUST_METRICS_WINDOW; default 28d
	trustMetricsBranches []string      // TRUST_METRICS_TARGET_BRANCHES csv; default "main"

	// AI-authored MR human-approval enforcement (DEVOPS-730). Opt-in; off
	// unless AI_APPROVAL_ENABLED=true. When on, every MR opened by one of the
	// configured bot users gets an idempotent approval rule requiring N human
	// approvals from the configured reviewer group/users. Policy values
	// (reviewer count, group) are config-driven so the team can land the
	// mechanism dark and flip it on once the policy is agreed.
	aiApprovalEnabled          bool   // AI_APPROVAL_ENABLED=true
	aiApprovalRuleName         string // AI_APPROVAL_RULE_NAME; default "AI-generated change review"
	aiApprovalRequired         int    // AI_APPROVAL_REQUIRED; default 1
	aiApprovalBotUsernames     []string
	aiApprovalReviewerGroupIDs []int
	aiApprovalReviewerUserIDs  []int

	// CI/CD compute-minutes metrics collector (Phase G, ka-0000002378). Opt-in
	// per series; off unless enabled. The job-duration series runs on the
	// existing GAT; the first-party GraphQL series needs a group-owner token
	// (escalation ka-0000006982) and stays dark until it lands.
	cicdGitLabMetricsEnabled    bool          // CICD_GITLAB_METRICS_ENABLED=true (job-duration)
	cicdGitLabFirstPartyEnabled bool          // CICD_GITLAB_FIRSTPARTY_ENABLED=true (GraphQL usage/quota)
	cicdMetricsProjects         []string      // CICD_METRICS_PROJECTS csv; default = GITLAB_METRICS_PROJECTS/none
	cicdMetricsInterval         time.Duration // CICD_METRICS_INTERVAL; default 120s
	cicdMetricsWindow           time.Duration // CICD_METRICS_WINDOW; default 60m
	cicdFirstPartyNamespaceID   string        // CICD_FIRSTPARTY_NAMESPACE_ID (GraphQL global id)
	cicdFirstPartyGroupPath     string        // CICD_FIRSTPARTY_GROUP_PATH (REST group path)
	cicdLookbackMonths          int           // CICD_LOOKBACK_MONTHS; default 12

	// Release-drought deadman watchdog (ka-0000002594). Opt-in; off unless
	// RELEASE_DROUGHT_ENABLED=true — only ONE bridge per release lane should
	// run it (prod), or staging/ephemeral envs polling the same GitLab
	// project would duplicate every alert.
	releaseDroughtEnabled     bool          // RELEASE_DROUGHT_ENABLED=true
	releaseDroughtProject     string        // RELEASE_DROUGHT_PROJECT; default "example-org/gardenio"
	releaseDroughtThreshold   time.Duration // RELEASE_DROUGHT_THRESHOLD; default 6h
	releaseDroughtInterval    time.Duration // RELEASE_DROUGHT_INTERVAL; default 15m
	releaseDroughtRepeat      time.Duration // RELEASE_DROUGHT_REPEAT; default = threshold (negative disables re-alerts)
	releaseDroughtCreateBead  bool          // RELEASE_DROUGHT_CREATE_BEAD; default true
	releaseDroughtBeadProject string        // RELEASE_DROUGHT_BEAD_PROJECT; default "gardenio"
}

func parseConfig() *config {
	pollInterval := 5 * time.Minute
	if v := os.Getenv("GITLAB_POLL_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			pollInterval = d
		}
	}

	triggerPollInterval := 60 * time.Second
	if v := os.Getenv("TRIGGER_POLL_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			triggerPollInterval = d
		}
	}

	groupID := 0
	if v := os.Getenv("GITLAB_GROUP_ID"); v != "" {
		if id, err := strconv.Atoi(v); err == nil {
			groupID = id
		}
	}

	// GitLab-webhook deadman monitor cadence + window (GAP-C of ka-0000005632).
	// Zero values let NewGitLabWebhookMonitor apply its own defaults (2h/5m).
	var gitlabWebhookDeadmanWindow, gitlabWebhookDeadmanInterval time.Duration
	if v := os.Getenv("GITLAB_WEBHOOK_DEADMAN_WINDOW"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			gitlabWebhookDeadmanWindow = d
		}
	}
	if v := os.Getenv("GITLAB_WEBHOOK_DEADMAN_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			gitlabWebhookDeadmanInterval = d
		}
	}

	mrDescCatchupLimit := 512
	if v := os.Getenv("MR_DESC_CATCHUP_LIMIT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			mrDescCatchupLimit = n
		}
	}

	mrBacklinkInterval := 15 * time.Minute
	if v := os.Getenv("MR_BACKLINK_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			mrBacklinkInterval = d
		}
	}
	mrBacklinkWindow := 14 * 24 * time.Hour
	if v := os.Getenv("MR_BACKLINK_WINDOW"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			mrBacklinkWindow = d
		}
	}

	mrReopenDriftInterval := 15 * time.Minute
	if v := os.Getenv("MR_REOPEN_DRIFT_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			mrReopenDriftInterval = d
		}
	}
	mrReopenDriftWindow := 30 * 24 * time.Hour
	if v := os.Getenv("MR_REOPEN_DRIFT_WINDOW"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			mrReopenDriftWindow = d
		}
	}

	mrTerminalizeDriftInterval := time.Hour
	if v := os.Getenv("MR_TERMINALIZE_DRIFT_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			mrTerminalizeDriftInterval = d
		}
	}
	mrTerminalizeDriftStaleAge := 7 * 24 * time.Hour
	if v := os.Getenv("MR_TERMINALIZE_DRIFT_STALE_AGE"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			mrTerminalizeDriftStaleAge = d
		}
	}
	mrTerminalizeDriftMaxScan := 200
	if v := os.Getenv("MR_TERMINALIZE_DRIFT_MAX_PER_SCAN"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			mrTerminalizeDriftMaxScan = n
		}
	}

	mrSchemaFlagInterval := 15 * time.Minute
	if v := os.Getenv("MR_SCHEMA_FLAG_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			mrSchemaFlagInterval = d
		}
	}
	mrSchemaFlagWindow := 14 * 24 * time.Hour
	if v := os.Getenv("MR_SCHEMA_FLAG_WINDOW"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			mrSchemaFlagWindow = d
		}
	}

	upstreamBackstopInterval := 10 * time.Minute
	if v := os.Getenv("UPSTREAM_BACKSTOP_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			upstreamBackstopInterval = d
		}
	}

	escalationReapInterval := 10 * time.Minute
	if v := os.Getenv("ESCALATION_REAP_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			escalationReapInterval = d
		}
	}

	// GitLab-mention catchup watchdog cadence + grace (ka-0000000279). Zero
	// values let NewMentionCatchupWatchdog apply its own defaults (5m/10m).
	var mentionCatchupInterval, mentionCatchupGrace time.Duration
	if v := os.Getenv("MENTION_CATCHUP_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			mentionCatchupInterval = d
		}
	}
	if v := os.Getenv("MENTION_CATCHUP_GRACE"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			mentionCatchupGrace = d
		}
	}

	// Release-drought watchdog durations. Zero values let the watchdog apply
	// its own defaults (6h threshold, 15m interval, repeat = threshold).
	var releaseDroughtThreshold, releaseDroughtInterval, releaseDroughtRepeat time.Duration
	if v := os.Getenv("RELEASE_DROUGHT_THRESHOLD"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			releaseDroughtThreshold = d
		}
	}
	if v := os.Getenv("RELEASE_DROUGHT_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			releaseDroughtInterval = d
		}
	}
	if v := os.Getenv("RELEASE_DROUGHT_REPEAT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			releaseDroughtRepeat = d
		}
	}

	trustMetricsInterval := 10 * time.Minute
	if v := os.Getenv("TRUST_METRICS_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			trustMetricsInterval = d
		}
	}
	trustMetricsWindow := 28 * 24 * time.Hour
	if v := os.Getenv("TRUST_METRICS_WINDOW"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			trustMetricsWindow = d
		}
	}
	trustMetricsBranches := []string{"main"}
	if v := os.Getenv("TRUST_METRICS_TARGET_BRANCHES"); v != "" {
		var branches []string
		for b := range strings.SplitSeq(v, ",") {
			if t := strings.TrimSpace(b); t != "" {
				branches = append(branches, t)
			}
		}
		// An explicit "*" / "all" disables the branch filter (consider every MR).
		if len(branches) == 1 && (branches[0] == "*" || branches[0] == "all") {
			branches = nil
		}
		trustMetricsBranches = branches
	}

	cicdMetricsInterval := 120 * time.Second
	if v := os.Getenv("CICD_METRICS_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cicdMetricsInterval = d
		}
	}
	cicdMetricsWindow := 60 * time.Minute
	if v := os.Getenv("CICD_METRICS_WINDOW"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cicdMetricsWindow = d
		}
	}
	cicdLookbackMonths := 12
	if v := os.Getenv("CICD_LOOKBACK_MONTHS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cicdLookbackMonths = n
		}
	}
	var cicdMetricsProjects []string
	for p := range strings.SplitSeq(os.Getenv("CICD_METRICS_PROJECTS"), ",") {
		if t := strings.TrimSpace(p); t != "" {
			cicdMetricsProjects = append(cicdMetricsProjects, t)
		}
	}

	aiApprovalRequired := 1
	if v := os.Getenv("AI_APPROVAL_REQUIRED"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			aiApprovalRequired = n
		}
	}
	var aiApprovalBots []string
	for b := range strings.SplitSeq(os.Getenv("AI_APPROVAL_BOT_USERNAMES"), ",") {
		if t := strings.TrimSpace(b); t != "" {
			aiApprovalBots = append(aiApprovalBots, t)
		}
	}
	// Default the AI-author identity to the configured bridge bot username
	// when no explicit list is provided — that is the account opening Gardenio
	// MRs in the common single-bot deployment.
	if len(aiApprovalBots) == 0 {
		if bot := strings.TrimSpace(os.Getenv("GITLAB_BOT_USERNAME")); bot != "" {
			aiApprovalBots = []string{bot}
		}
	}

	return &config{
		beadsHTTPAddr:                envOrDefault("BEADS_HTTP_ADDR", "http://localhost:8080"),
		gitlabBaseURL:                os.Getenv("GITLAB_BASE_URL"),
		gitlabAPIToken:               os.Getenv("GITLAB_API_TOKEN"),
		gitlabWebhookSecret:          os.Getenv("GITLAB_WEBHOOK_SECRET"),
		gitlabBotUsername:            os.Getenv("GITLAB_BOT_USERNAME"),
		gitlabGroupID:                groupID,
		pollInterval:                 pollInterval,
		triggerPollInterval:          triggerPollInterval,
		gitlabWebhookDeadmanWindow:   gitlabWebhookDeadmanWindow,
		gitlabWebhookDeadmanInterval: gitlabWebhookDeadmanInterval,
		listenAddr:                   envOrDefault("LISTEN_ADDR", ":8092"),
		metricsAddr:                  envOrDefault("METRICS_LISTEN_ADDR", ":9092"),
		logLevel:                     envOrDefault("LOG_LEVEL", "info"),
		statePath:                    envOrDefault("STATE_PATH", "/data/gitlab-bridge-state.json"),
		slackBotToken:                os.Getenv("SLACK_BOT_TOKEN"),
		slackChannel:                 os.Getenv("SLACK_CHANNEL"),
		mentionTrigger:               envOrDefault("GITLAB_MENTION_TRIGGER", "@gardenio"),
		mentionProject:               envOrDefault("GITLAB_MENTION_PROJECT", "gardenio"),
		beadsViewerURL:               os.Getenv("BEADS_VIEWER_URL"),
		jiraBaseURL:                  os.Getenv("JIRA_BASE_URL"),
		mrDescCatchupLimit:           mrDescCatchupLimit,
		mrBacklinkInterval:           mrBacklinkInterval,
		mrBacklinkWindow:             mrBacklinkWindow,
		mrBacklinkDryRun:             os.Getenv("MR_BACKLINK_DRY_RUN") == "true",
		mrBacklinkFileUnlinked:       os.Getenv("MR_BACKLINK_FILE_UNLINKED") == "true",

		mrReopenDriftEnabled:  os.Getenv("MR_REOPEN_DRIFT") != "off",
		mrReopenDriftInterval: mrReopenDriftInterval,
		mrReopenDriftWindow:   mrReopenDriftWindow,
		mrReopenDriftDryRun:   os.Getenv("MR_REOPEN_DRIFT_DRY_RUN") == "true",

		mrTerminalizeDriftEnabled:  os.Getenv("MR_TERMINALIZE_DRIFT") != "off",
		mrTerminalizeDriftInterval: mrTerminalizeDriftInterval,
		mrTerminalizeDriftStaleAge: mrTerminalizeDriftStaleAge,
		mrTerminalizeDriftMaxScan:  mrTerminalizeDriftMaxScan,
		mrTerminalizeDriftDryRun:   os.Getenv("MR_TERMINALIZE_DRIFT_DRY_RUN") == "true",

		mrSchemaFlagEnabled:  os.Getenv("MR_SCHEMA_FLAG") != "off",
		mrSchemaFlagInterval: mrSchemaFlagInterval,
		mrSchemaFlagWindow:   mrSchemaFlagWindow,
		mrSchemaFlagDryRun:   os.Getenv("MR_SCHEMA_FLAG_DRY_RUN") == "true",

		upstreamBackstopEnabled:  os.Getenv("UPSTREAM_BACKSTOP_POLL") == "true",
		upstreamBackstopInterval: upstreamBackstopInterval,
		upstreamBackstopLabel:    envOrDefault("UPSTREAM_BACKSTOP_LABEL", "upstream:true"),

		escalationReapEnabled:      os.Getenv("ESCALATION_REAP") != "off",
		escalationReapInterval:     escalationReapInterval,
		escalationReapDryRun:       os.Getenv("ESCALATION_REAP_DRY_RUN") == "true",
		escalationReviewFlag:       os.Getenv("ESCALATION_REVIEW_FLAG") != "off",
		escalationMRGroupNamespace: os.Getenv("ESCALATION_MR_GROUP_NAMESPACE"),

		mentionCatchupEnabled:  os.Getenv("MENTION_CATCHUP") != "off",
		mentionCatchupInterval: mentionCatchupInterval,
		mentionCatchupGrace:    mentionCatchupGrace,
		mentionCatchupDryRun:   os.Getenv("MENTION_CATCHUP_DRY_RUN") == "true",

		trustMetricsEnabled:  os.Getenv("TRUST_METRICS_ENABLED") == "true",
		trustMetricsInterval: trustMetricsInterval,
		trustMetricsWindow:   trustMetricsWindow,
		trustMetricsBranches: trustMetricsBranches,

		cicdGitLabMetricsEnabled:    os.Getenv("CICD_GITLAB_METRICS_ENABLED") == "true",
		cicdGitLabFirstPartyEnabled: os.Getenv("CICD_GITLAB_FIRSTPARTY_ENABLED") == "true",
		cicdMetricsProjects:         cicdMetricsProjects,
		cicdMetricsInterval:         cicdMetricsInterval,
		cicdMetricsWindow:           cicdMetricsWindow,
		cicdFirstPartyNamespaceID:   os.Getenv("CICD_FIRSTPARTY_NAMESPACE_ID"),
		cicdFirstPartyGroupPath:     os.Getenv("CICD_FIRSTPARTY_GROUP_PATH"),
		cicdLookbackMonths:          cicdLookbackMonths,

		aiApprovalEnabled:          os.Getenv("AI_APPROVAL_ENABLED") == "true",
		aiApprovalRuleName:         os.Getenv("AI_APPROVAL_RULE_NAME"),
		aiApprovalRequired:         aiApprovalRequired,
		aiApprovalBotUsernames:     aiApprovalBots,
		aiApprovalReviewerGroupIDs: bridge.ParseIntCSV(os.Getenv("AI_APPROVAL_REVIEWER_GROUP_IDS")),
		aiApprovalReviewerUserIDs:  bridge.ParseIntCSV(os.Getenv("AI_APPROVAL_REVIEWER_USER_IDS")),

		releaseDroughtEnabled:     os.Getenv("RELEASE_DROUGHT_ENABLED") == "true",
		releaseDroughtProject:     os.Getenv("RELEASE_DROUGHT_PROJECT"),
		releaseDroughtThreshold:   releaseDroughtThreshold,
		releaseDroughtInterval:    releaseDroughtInterval,
		releaseDroughtRepeat:      releaseDroughtRepeat,
		releaseDroughtCreateBead:  os.Getenv("RELEASE_DROUGHT_CREATE_BEAD") != "false",
		releaseDroughtBeadProject: os.Getenv("RELEASE_DROUGHT_BEAD_PROJECT"),
	}
}

// runTriggerEngine runs the trigger engine's evaluation loop on a ticker until
// ctx is cancelled. It is expected to be launched as a goroutine after
// triggerEngine.CatchUp has populated the engine's dedup state. An initial
// Evaluate is dispatched on start so that triggers created after boot do not
// have to wait a full poll interval for their first check.
func runTriggerEngine(ctx context.Context, engine *bridge.TriggerEngine, interval time.Duration, logger *slog.Logger) {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	logger.Info("trigger engine started", "poll_interval", interval)

	// Kick off the first evaluation immediately so newly-created triggers are
	// picked up without waiting for a full poll interval.
	engine.Evaluate(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	evalCount := 0
	for {
		select {
		case <-ctx.Done():
			logger.Info("trigger engine stopping")
			return
		case <-ticker.C:
			engine.Evaluate(ctx)
			evalCount++
			// Log seen map size every 10 evaluations for memory observability.
			if evalCount%10 == 0 {
				triggers, keys := engine.SeenMapSize()
				logger.Info("trigger engine stats",
					"seen_triggers", triggers, "seen_keys", keys)
			}
		}
	}
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
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
