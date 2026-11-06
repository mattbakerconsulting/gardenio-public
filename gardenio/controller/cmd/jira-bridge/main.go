// Command jira-bridge is a standalone service that polls JIRA for new issues,
// creates task beads in the beads daemon, and syncs bead updates (MR links,
// closures) back to JIRA as comments, remote links, and transitions.
//
// It runs three subsystems:
//   - JIRA poller: periodic JIRA search → task bead creation
//   - JIRA sync: SSE subscription for bead updates → JIRA sync-back
//   - HTTP server: health/readiness endpoints
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
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/covflush"
	"gardenio/controller/internal/tracing"
)

var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	defer covflush.Flush()
	bridge.NudgeSender = "jira-bridge" // from: label on the mail bridge.NudgeAgent sends
	cfg := parseConfig()

	logger := setupLogger(cfg.logLevel)

	shutdownTracing, err := tracing.Init(context.Background(), logger, tracing.Options{
		ServiceName:    "gardenio-jira-bridge",
		ServiceVersion: version,
		Environment:    os.Getenv("GARDENIO_ENV"),
	})
	if err != nil {
		logger.Error("failed to initialize tracing", "error", err)
		os.Exit(1)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTracing(shutdownCtx); err != nil {
			logger.Warn("tracing shutdown error", "error", err)
		}
	}()

	logger.Info("starting jira-bridge",
		"version", version,
		"commit", commit,
		"beads_http", cfg.beadsHTTPAddr,
		"jira_base_url", cfg.jiraBaseURL,
		"jira_projects", cfg.jiraProjects,
		"jira_fix_versions", cfg.jiraFixVersions,
		"jira_required_labels", cfg.jiraRequiredLabels,
		"jira_excluded_labels", cfg.jiraExcludedLabels,
		"jira_custom_jql", cfg.jiraCustomJQL,
		"jira_skip_assigned", cfg.jiraSkipAssigned,
		"jira_disable_transitions", cfg.jiraDisableTransitions,
		"jira_enable_claim_transition", cfg.jiraEnableClaimTransition,
		"listen_addr", cfg.listenAddr,
		"base_path", cfg.basePath,
		"jira_webhook_secret_set", cfg.jiraWebhookSecret != "")

	// Pre-initialize the conversational-loop CounterVecs to 0 so every labelled
	// series exists on the first /metrics scrape, before any mention/reply/ack/
	// wake/comment-mirror has fired. Without this, the label-bearing counters
	// export no series at all on a quiet-but-healthy loop and their dashboard
	// panels render "No data" instead of a graphable flat 0 (ka-0000004443).
	// Scoped to the jira-bridge binary — these emit paths never run in the
	// controller binary, so the series intentionally stay absent there.
	bridge.PreInitJiraConversationMetrics()

	// Create beads daemon HTTP client.
	daemon, err := beadsapi.New(beadsapi.Config{
		HTTPAddr: cfg.beadsHTTPAddr,
		Token:    os.Getenv("BEADS_DAEMON_TOKEN"),
	})
	if err != nil {
		logger.Error("failed to create beads daemon client", "error", err)
		os.Exit(1)
	}
	defer daemon.Close()

	// Register bead types, views, and context configs with the daemon.
	if err := bridge.EnsureConfigs(context.Background(), daemon, logger); err != nil {
		logger.Warn("failed to ensure beads configs (non-fatal)", "error", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	// State persistence for SSE last-event-ID.
	state, err := bridge.NewStateManager(cfg.statePath)
	if err != nil {
		logger.Error("failed to load state", "path", cfg.statePath, "error", err)
		os.Exit(1)
	}
	logger.Info("state manager loaded", "path", cfg.statePath)

	// Create JIRA client.
	jiraClient := bridge.NewJiraClient(bridge.JiraClientConfig{
		BaseURL:  cfg.jiraBaseURL,
		Email:    cfg.jiraEmail,
		APIToken: cfg.jiraAPIToken,
		Logger:   logger,
	})

	// HTTP server with health endpoints.
	mux := http.NewServeMux()
	registerHealthHandlers(mux, version)

	// Nudges go out as interrupt mail (bridge.NudgeAgent).
	nudgeFunc := func(ctx context.Context, agentName, message string) error {
		return bridge.NudgeAgent(ctx, daemon, logger, agentName, message)
	}

	// User directory resolves a JIRA comment author's accountId to a fleet
	// human so mention beads are attributed to the originating person
	// (slack:<id> / oidc:<email>) rather than a jira:<accountID> sentinel
	// (R3, ka-0000002887). SSE handlers are registered below once the stream
	// exists; an initial refresh primes the cache so the first mention can
	// resolve without waiting for the periodic refresh.
	userDir := bridge.NewUserDirectory(daemon, logger)
	if err := userDir.Refresh(ctx); err != nil {
		logger.Warn("initial user directory refresh failed (will retry on lookups)", "error", err)
	}

	// Single config-driven write policy (ka-0000007252, epic ka-0000003475): ONE
	// *WritePolicyStore is shared by all four outbound Jira paths (mentions,
	// poller, sync-back, squawk-forward). It unifies the two former gates — the
	// coordinated project gate (ka-0000001679) and the per-role field allowlist
	// (ka-0000004904) — into one policy keyed on (project, role, field,
	// visibility), driven by data rather than FSM-specific compiled-in constants.
	//
	// FSM is not a writable project at all (epic ka-0000002067): the bot holds no
	// FSM write permission, and FSM-bound writes flow through Add-Label /
	// Set-Version INTENTS in the internal GBFSM outbox project. GBFSM is the one
	// toggle-gated project, writable only when the "outbox" toggle
	// (JIRA_OUTBOX_ENABLED, default OFF, shadow-first) is on — so prod is inert
	// until the E2E gate (ka-0000000600) passes and the operator flips it on;
	// unsetting it is the rollback. The enforcement mode (enforce drops denied
	// writes; shadow logs-but-allows) comes from JIRA_FIELD_ALLOWLIST_MODE.
	// Building the store once and threading the same value into every path keeps
	// the outbox flip and the field policy atomic and consistent across paths.
	allowlistMode := bridge.AllowlistModeFromString(cfg.jiraFieldAllowlistMode)
	writePolicy := bridge.NewWritePolicyStore(
		func() bridge.WritePolicy {
			wp := bridge.DefaultWritePolicy(cfg.jiraOutboxEnabled)
			wp.Mode = allowlistMode
			// Widen the @gardenio-mention SPAWN surface without widening the
			// WRITE surface: JIRA_SPAWN_PROJECTS folds each listed project in as
			// {Spawnable:true, Writable:false} so mentions there spawn + reply,
			// while the poller/sync-back/squawk write paths (keyed on Writable)
			// still refuse it. Default empty ⇒ spawn set stays {PE,DEVOPS,GBFSM}
			// and the policy is byte-for-byte unchanged (ka-0000005656).
			wp = wp.WithSpawnProjects(cfg.jiraSpawnProjects, map[string]bridge.RolePolicy(bridge.DefaultFieldAllowlist()))
			return wp
		}(),
		logger,
	)
	logger.Info("jira write policy initialized",
		"mode", allowlistMode.String(),
		"outbox_enabled", cfg.jiraOutboxEnabled,
		"analysis_issue_type_configured", cfg.jiraOutboxAnalysisIssueType != "",
		"labels_field_configured", cfg.jiraOutboxLabelsField != "",
		"version_field_configured", cfg.jiraOutboxVersionField != "",
		"spawn_only_projects", cfg.jiraSpawnProjects,
		"writable_projects", writePolicy.Projects())

	// Spawn acknowledger: post an immediate "on it" internal note on the ticket
	// the instant a @gardenio mention spawns an agent — the Jira analog of the
	// Slack agent card (plan ka-0000007401). Without it the ticket stays silent
	// until the agent's first `gb jira reply`, which can be minutes away or never
	// arrive if the boot cycle is interrupted. Routes through the SAME shared
	// write policy as every other outbound path (reply-predicate gates: PE/DEVOPS
	// internal notes allowed, FSM/unknown hard-blocked, decoupled from the outbox
	// toggle). It is a separate collaborator, NOT a field on the mention handler,
	// so the mention path holds no write surface (TestJiraMention_NoJiraWriteSurface).
	spawnAck := bridge.NewJiraSpawnAcknowledger(bridge.JiraSpawnAcknowledgerConfig{
		Jira:        jiraClient,
		WritePolicy: writePolicy,
		Logger:      logger,
	})

	// Completion acknowledger — the back-half of the Slack reaction lifecycle
	// (👀→⏳→✅/❌). It watches a mention-spawned agent bead's terminal CLOSE and
	// posts one gated internal note naming the outcome (✅ Done / ❌ Failed) with
	// the wrap-up tldr, so a ticket is never left silent after the "On it"
	// spawn-ack (GAP 4, ka-0000004899). Same shared write policy (reply
	// predicates: internal notes allowed, FSM/unknown hard-blocked, decoupled
	// from the outbox toggle), same separate-collaborator shape as spawnAck so
	// the mention path holds no write surface. jira_sync.handleClosed defers the
	// close note for these agent beads to this acknowledger (no double-post).
	completionAck := bridge.NewJiraCompletionAcknowledger(bridge.JiraCompletionAcknowledgerConfig{
		Jira:        jiraClient,
		WritePolicy: writePolicy,
		Logger:      logger,
	})

	// Mention handler for @gardenio mentions in JIRA comments. The Jira client
	// supplies recent comment history for spawn prompts (F4, plan
	// ka-0000007401); the spawn acknowledger runs on OnSpawn (Slack-card parity).
	mentionHandler := buildMentionHandler(cfg, daemon, jiraClient, logger, userDir, writePolicy, spawnAck)

	// Agent resolver for follow-up comments (F3, plan ka-0000007401): a live
	// assigned agent is nudged (interrupt mail via nudgeFunc); a STOPPED one is
	// re-spawned under the same name so the entrypoint resumes the prior
	// session and the ticket conversation continues. Re-spawns route through
	// the same shared write policy as every outbound path (F5 spine).
	followUpResolver := bridge.NewJiraAgentResolver(bridge.JiraAgentResolverConfig{
		Daemon:          daemon,
		Logger:          logger,
		Nudge:           nudgeFunc,
		DefaultOperator: cfg.jiraBridgeDefaultOperator,
	})

	// Durable DLQ for inbound comment mirrors (epic ka-0000007401 — Slack parity
	// with thread_reply_dlq.go). The webhook's bounded inline retry absorbs a
	// momentary daemon blip; this second layer catches a mirror write that
	// exhausts that retry (a rollout / restart / failover longer than ~0.9s),
	// persists it to the state file, and replays it when the daemon recovers, so a
	// human follow-up comment is never silently dropped from the bead conversation.
	// Replay is exactly-once via a comment-existence check (the comment-create
	// endpoint has no idempotency key).
	inboundCommentDLQ := bridge.NewJiraInboundCommentDLQ(state, daemon, logger)
	go inboundCommentDLQ.StartDrain(ctx)
	logger.Info("jira inbound-comment DLQ drain loop started")

	// Webhook endpoint for Jira comment events.
	mux.Handle("/webhook", bridge.JiraWebhookHandler(bridge.JiraWebhookConfig{
		Daemon:                daemon,
		WebhookSecret:         cfg.jiraWebhookSecret,
		DevMode:               cfg.jiraWebhookDevMode,
		Nudge:                 nudgeFunc,
		MentionHandler:        mentionHandler,
		Resolver:              followUpResolver,
		WritePolicy:           writePolicy,
		BotAccountID:          cfg.jiraBotAccountID,
		BotAccountIDs:         cfg.jiraBotAccountIDs,
		History:               jiraClient,
		InboundCommentDLQ:     inboundCommentDLQ,
		NonActionableStatuses: cfg.jiraNonActionableStatuses,
		Logger:                logger,
	}))

	// Agent-facing reply endpoint: POST /api/jira/reply lets a Jira-bound
	// agent's `gb jira reply` post a comment on its OWN bound ticket (feature
	// F2, plan ka-0000007401). Routes through the SAME shared write policy as
	// every other outbound path (F5 spine): PE/DEVOPS internal notes allowed,
	// public replies role-gated (none by default), FSM hard-blocked in every
	// mode — do not weaken it.
	// The same handler also serves GET /api/jira/thread (feature G1, epic
	// ka-0000007401): a bound-ticket-scoped READ of the ticket's recent
	// comments so `gb jira thread` can re-read the conversation mid-session
	// without raw Jira credentials in the agent pod. And GET /api/jira/issue
	// (feature G6): the ticket-BODY analog — a bound-ticket-scoped read of the
	// current description/status/labels/fields so `gb jira show` works inside
	// an agent pod (no JIRA_* creds) and reflects operator edits mid-session.
	replyAPI := bridge.NewJiraReplyAPI(bridge.JiraReplyAPIConfig{
		Daemon:      daemon,
		Jira:        jiraClient,
		History:     jiraClient,
		Issues:      jiraClient,
		Logger:      logger,
		WritePolicy: writePolicy,
		AgentToken:  cfg.jiraAgentToken,
	})
	replyAPI.RegisterRoutes(mux)
	if cfg.jiraAgentToken == "" {
		logger.Warn("JIRA_BRIDGE_AGENT_TOKEN not set — /api/jira/reply is unauthenticated; set it in production")
	}

	handler := wrapBasePath(mux, cfg.basePath)
	if cfg.basePath != "" {
		logger.Info("base-path routing enabled", "base_path", cfg.basePath)
	}

	srv := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           otelhttp.NewHandler(handler, "jira-bridge"),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		logger.Info("starting HTTP server", "addr", cfg.listenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP server failed", "error", err)
		}
	}()

	// Internal-only Prometheus metrics server. Serves the default gatherer
	// where client_golang's Go/process collectors and the JQL-count collector's
	// promauto-registered gardenio_jira_* series land (design bundle
	// ka-0000005503, bead ka-0000003681). Separate listener from the public
	// :8091 server so /metrics is never internet-reachable; the port defaults to
	// :9093 because the vmagent-egress NetworkPolicy only permits scrape egress
	// to 9091/9092/9093/3000/8429.
	metricsSrv := buildMetricsServer(cfg.metricsAddr)
	go func() {
		logger.Info("starting metrics server", "addr", cfg.metricsAddr)
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics server failed", "error", err)
		}
	}()

	// Inbound-webhook deadman monitor: periodically folds the "0 authenticated
	// deliveries in the window WHILE upstream activity exists" verdict into
	// gardenio_jira_webhook_dark (jira_webhook_deadman.go). This is the third-leg
	// detector — the receive path is the one jira-bridge subsystem that had no
	// metric and no alert, so an unregistered/mis-secreted webhook was silently
	// dark while every dashboard stayed green (bug ka-0000000426). Always on: it
	// only needs the JIRA client's cheap approximate-count, and the whole point
	// is to page when nothing else can tell the receive path is dead.
	// Scope the deadman's upstream-activity confirmation JQL to the UNION of the
	// poller projects and the mention-spawn projects, so a widened @gardenio
	// spawn surface (JIRA_SPAWN_PROJECTS) keeps its dark-webhook coverage: an
	// out-of-scope project would otherwise be invisible to the alert exactly as
	// it is to the receive path (ka-0000005656). Default (no spawn projects) ⇒
	// union == cfg.jiraProjects, unchanged.
	deadmanProjects := unionProjects(cfg.jiraProjects, cfg.jiraSpawnProjects)
	webhookMonitor := bridge.NewJiraWebhookMonitor(bridge.JiraWebhookMonitorConfig{
		Jira:     jiraClient,
		Projects: deadmanProjects,
		Window:   cfg.jiraWebhookDeadmanWindow,
		Interval: cfg.jiraWebhookDeadmanInterval,
		Logger:   logger,
	})
	go func() {
		if err := webhookMonitor.Run(ctx); err != nil && ctx.Err() == nil {
			logger.Error("JIRA webhook deadman monitor stopped", "error", err)
		}
	}()
	logger.Info("jira-webhook deadman monitor enabled",
		"projects", deadmanProjects,
		"window", cfg.jiraWebhookDeadmanWindow,
		"interval", cfg.jiraWebhookDeadmanInterval)

	// JQL-count metrics collector: periodically runs the configured queries
	// through approximate-count and publishes gardenio_jira_* gauges. Disabled
	// when no queries are configured (JIRA_METRICS_QUERIES unset) so the default
	// deployment is unchanged until the query list is wired in.
	if len(cfg.metricsQueries) > 0 {
		metricsCollector := bridge.NewJiraMetricsCollector(bridge.JiraMetricsConfig{
			Jira:     jiraClient,
			Queries:  cfg.metricsQueries,
			Interval: cfg.metricsInterval,
			Logger:   logger,
		})
		go func() {
			if err := metricsCollector.Run(ctx); err != nil && ctx.Err() == nil {
				logger.Error("JIRA metrics collector stopped", "error", err)
			}
		}()
		logger.Info("jira-metrics collector enabled",
			"queries", len(cfg.metricsQueries), "interval", cfg.metricsInterval)
	}

	// Per-issue AGE collector: pages each configured query's matching issues and
	// republishes their ages as a snapshot histogram + oldest-age gauge, the
	// grounded input for an SLA-age / age-burn-down panel. Disabled when no age
	// queries are configured (JIRA_AGE_QUERIES unset) so the default deployment
	// is unchanged. The JiraAgeMetrics collector is registered on the default
	// registry (the gatherer :9093 /metrics serves) only when the loop runs.
	if len(cfg.ageQueries) > 0 {
		ageMetrics := bridge.NewJiraAgeMetrics()
		if err := ageMetrics.Register(prometheus.DefaultRegisterer); err != nil {
			logger.Error("failed to register jira-age metrics collector", "error", err)
		} else {
			ageCollector := bridge.NewJiraAgeCollector(bridge.JiraAgeConfig{
				Jira:     jiraClient,
				Metrics:  ageMetrics,
				Queries:  cfg.ageQueries,
				Interval: cfg.ageInterval,
				Logger:   logger,
			})
			go func() {
				if err := ageCollector.Run(ctx); err != nil && ctx.Err() == nil {
					logger.Error("JIRA age collector stopped", "error", err)
				}
			}()
			logger.Info("jira-age collector enabled",
				"queries", len(cfg.ageQueries), "interval", cfg.ageInterval)
		}
	}

	// Start JIRA poller goroutine. The config-driven write policy (built above)
	// is the same value threaded into every outbound path (ka-0000007252).
	pollerCfg := buildJiraPollerConfig(cfg, logger)
	pollerCfg.WritePolicy = writePolicy
	poller := bridge.NewJiraPoller(jiraClient, daemon, pollerCfg)

	// Attach JQL trigger evaluator so trigger beads are evaluated after
	// each poll cycle. This enables spawn_agent / apply_formula / notify
	// actions to fire when new JIRA issues match a trigger's JQL query.
	// JIRA_TRIGGER_PROJECTS scopes trigger-bead loading to this bridge's own
	// boat projects so a second bridge sharing the daemon (e.g. a sandbox
	// misconfigured onto prod) can never evaluate — and cooldown-starve —
	// this environment's triggers (ka-0000006237).
	triggerEval := bridge.NewJiraTriggerEvaluator(bridge.JiraTriggerConfig{
		Jira:            jiraClient,
		Daemon:          daemon,
		Logger:          logger,
		TriggerProjects: cfg.jiraTriggerProjects,
		// Resolve a matched ticket's reporter → fleet human so an auto-spawn is
		// attributed to the reporter, not fleet:jira-bridge (ka-0000001781). The
		// same UserDirectory backs the @-mention operator resolution.
		Users: userDir,
	})
	if len(cfg.jiraTriggerProjects) > 0 {
		logger.Info("jira trigger evaluator scoped to projects", "projects", cfg.jiraTriggerProjects)
	}
	poller.SetTriggerEvaluator(triggerEval)
	triggerEval.CatchUp(ctx) // pre-populate seen set to prevent re-firing on restart

	// Config bead watcher: reads jira-poller config bead on startup and
	// subscribes to SSE updates for hot-reload (no pod restart needed).
	configWatcher := bridge.NewJiraConfigWatcher(bridge.JiraConfigWatcherConfig{
		Daemon: daemon,
		Poller: poller,
		Logger: logger,
		EnvCfg: pollerCfg,
	})
	configWatcher.LoadOrSeed(ctx)

	go func() {
		if err := poller.Run(ctx); err != nil && ctx.Err() == nil {
			logger.Error("JIRA poller stopped", "error", err)
		}
	}()

	// Create event deduplicator.
	dedup := bridge.NewDedup(logger)
	go dedup.StartCleanup(ctx)

	// Create SSE event stream for JIRA sync-back and live user-directory
	// updates. `beads.bead.created` is required so newly-created user beads
	// land in the directory without waiting for the 5-minute periodic
	// refresh (same gap that hit slack/gitlab bridges — ka-0000001175).
	sseStream := bridge.NewSSEStream(bridge.SSEStreamConfig{
		BeadsHTTPAddr: cfg.beadsHTTPAddr,
		Topics:        []string{"beads.bead.created", "beads.bead.updated", "beads.bead.closed"},
		Token:         os.Getenv("BEADS_DAEMON_TOKEN"),
		Logger:        logger,
		Dedup:         dedup,
		State:         state,
	})

	// Register config watcher on the SSE stream for hot-reload.
	configWatcher.RegisterHandlers(sseStream)

	// Keep the user directory in sync with user-bead lifecycle events so a
	// just-provisioned JIRA author resolves to their fleet identity without
	// waiting for the periodic refresh (R3, ka-0000002887).
	userDir.RegisterHandlers(sseStream)

	// Register JIRA sync handler on the SSE stream. The daemon client resolves
	// the acting agent's role (assignee/closer) for gating. Every outbound
	// sync-back write is gated by the shared config-driven write policy (built
	// above), which folds the per-role field allowlist (epic ka-0000004904) and
	// the coordinated project gate into one verdict (ka-0000007252).
	jiraSync := bridge.NewJiraSync(bridge.JiraSyncConfig{
		Jira:                  jiraClient,
		Logger:                logger,
		DisableTransitions:    cfg.jiraDisableTransitions,
		EnableClaimTransition: cfg.jiraEnableClaimTransition,
		BotAccountID:          cfg.jiraBotAccountID,
		WritePolicy:           writePolicy,
		Roles:                 daemon,
		AnalysisIssueType:     cfg.jiraOutboxAnalysisIssueType,
		LabelsFieldID:         cfg.jiraOutboxLabelsField,
		VersionFieldID:        cfg.jiraOutboxVersionField,
	})
	jiraSync.RegisterHandlers(sseStream)

	// Pre-populate dedup map with existing Jira beads to prevent duplicate
	// comments/transitions on SSE replay after restart.
	jiraSync.CatchUp(ctx, daemon)

	// Register the JIRA squawk forwarder — posts a JIRA-bound agent's gb squawk
	// onto the ticket as an INTERNAL note, so the agent is visibly present on the
	// ticket it is working (feature F1, plan ka-0000007401). Mirrors the
	// gitlab-bridge wiring (cmd/gitlab-bridge/main.go NewGitLabSquawkForwarder +
	// RegisterHandlers). Every outbound comment routes through the SAME shared
	// config-driven write policy threaded into the poller/sync/mention paths
	// (ka-0000007252): the forwarder folds project + role + visibility into one
	// verdict, so PE/DEVOPS internal notes are allowed while FSM is hard-blocked
	// (F5 spine) — do not weaken it.
	squawkFwd := bridge.NewJiraSquawkForwarder(bridge.JiraSquawkForwarderConfig{
		Daemon:      daemon,
		Jira:        jiraClient,
		Logger:      logger,
		WritePolicy: writePolicy,
	})
	squawkFwd.RegisterHandlers(sseStream)

	// Register the JIRA completion acknowledger — posts the terminal ✅ Done /
	// ❌ Failed internal note when a mention-spawned agent bead closes (Slack
	// reaction-lifecycle back-half, GAP 4 / ka-0000004899). Close-only, like the
	// squawk forwarder; scoped to type=agent + spawn_source=jira-mention beads so
	// it never touches task-bead closes. Same shared write policy as every other
	// outbound path (ka-0000007252).
	completionAck.RegisterHandlers(sseStream)

	// One-shot FSM→GBFSM backfill DRY-RUN (epic ka-0000002637, Phase 5a
	// ka-0000000123). Off by default (JIRA_BACKFILL_DRY_RUN unset) — inert on merge.
	// When on, it enumerates the in-scope FSM backlog READ-ONLY and LOGS the ONE
	// GBFSM ticket each FSM ticket WOULD upsert (via the same buildGBFSMTicket the
	// live sync uses), making ZERO Jira writes. The LIVE backfill run is Phase 6
	// (ka-0000000885), gated on Phase 4 rules + a green E2E pipeline — it is
	// NOT wired here. Runs once at startup in its own goroutine so it never blocks
	// the SSE stream, and only in dry-run mode (GBFSMBackfill.Run refuses live).
	if cfg.jiraBackfillDryRun {
		backfill := bridge.NewGBFSMBackfill(bridge.GBFSMBackfillConfig{
			Sync:   jiraSync,
			Daemon: daemon,
			Logger: logger,
			DryRun: true,
		})
		go func() {
			if _, err := backfill.Run(ctx); err != nil && ctx.Err() == nil {
				logger.Error("gbfsm-backfill dry-run failed", "error", err)
			}
		}()
		logger.Info("gbfsm-backfill dry-run enabled (log-only, zero Jira writes)")
	}

	// Start the SSE stream.
	go func() {
		if err := sseStream.Start(ctx); err != nil && ctx.Err() == nil {
			logger.Error("SSE event stream stopped", "error", err)
		}
	}()

	logger.Info("jira-bridge ready",
		"projects", cfg.jiraProjects,
		"poll_interval", cfg.jiraPollInterval)

	// Block until shutdown signal.
	<-ctx.Done()
	logger.Info("shutting down jira-bridge")

	// Graceful HTTP server shutdown.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("HTTP server shutdown error", "error", err)
	}
	if err := metricsSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("metrics server shutdown error", "error", err)
	}
}

// config holds parsed environment configuration for the jira-bridge service.
type config struct {
	beadsHTTPAddr             string
	jiraBaseURL               string
	jiraEmail                 string
	jiraAPIToken              string
	jiraProjects              []string
	jiraSpawnProjects         []string // Extra @gardenio-mention SPAWN-eligible projects (JIRA_SPAWN_PROJECTS), spawn+reply only — NOT outbound-writable (ka-0000005656)
	jiraStatuses              []string
	jiraNonActionableStatuses []string // Statuses that make gardenio reconsider in-flight work (JIRA_NONACTIONABLE_STATUSES); empty ⇒ built-in default
	jiraIssueTypes            []string
	jiraFixVersions           []string          // JIRA fixVersion filter (e.g., ["2.5.0"])
	jiraRequiredLabels        []string          // JIRA labels that must be present (e.g., ["approved-to-work"])
	jiraExcludedLabels        []string          // JIRA labels that must NOT be present
	jiraFilterProjects        []string          // Projects that fixVersion/requiredLabels apply to (empty = all)
	jiraTriggerProjects       []string          // Boat project labels this bridge's trigger evaluator serves (empty = all; ka-0000006237)
	jiraCustomJQL             string            // Arbitrary JQL clause appended to the filter
	jiraProjectMap            map[string]string // JIRA prefix (upper) → boat project name
	jiraCustomFields          map[string]string // JIRA custom-field id → bead field name
	jiraCaptureAllCustom      bool              // capture every customfield_* into jira_custom_fields
	jiraPollInterval          time.Duration
	jiraSkipAssigned          bool // skip JIRA issues that have a human assignee
	jiraDisableTransitions    bool
	jiraEnableClaimTransition bool   // override: allow "In Progress" on claim even when disable is true
	jiraBotAccountID          string // optional: JIRA account ID for self-assignment
	// jiraFieldAllowlistMode selects the per-role field-write allowlist
	// enforcement mode (epic ka-0000004904). "enforce" drops out-of-policy
	// writes; anything else (default) runs in shadow mode — logs would-be
	// rejects but allows them, for a safe sandbox-first rollout. Wired from
	// JIRA_FIELD_ALLOWLIST_MODE.
	jiraFieldAllowlistMode string
	// jiraOutboxEnabled is the SINGLE coordinated toggle for the GBFSM outbox,
	// shared by all four outbound paths (jira_sync, jira_mentions,
	// jira_squawk_forward, jira_poller). Default false — the outbox is inert until
	// the E2E gate (ka-0000000600) validates the Automation rules end-to-end.
	// Flipping this one var (JIRA_OUTBOX_ENABLED) on is the cutover; unsetting it
	// is the rollback (epic ka-0000002067). FSM is never writable in any state.
	jiraOutboxEnabled bool
	// jiraOutboxAnalysisIssueType is the GBFSM "Analysis" issue-type id for the
	// GBFSM ticket surface (epic ka-0000002067, task ka-0000001294). Empty (default)
	// ⇒ the surface is inert (no GBFSM ticket is created), even when the outbox
	// toggle is on. Set from JIRA_OUTBOX_ANALYSIS_ISSUE_TYPE once the operator
	// scaffolds the "Analysis" issue type in the GBFSM project (10037).
	jiraOutboxAnalysisIssueType string
	// jiraOutboxLabelsField / jiraOutboxVersionField are the GBFSM custom-field ids
	// that carry the N triage labels and the recommended Affects-Version as FIELDS on
	// the ONE upserted GBFSM ticket (1:1 reshape, Phase 2 ka-0000001491). Empty
	// (default) ⇒ those fields are omitted from the ticket (the analysis Description
	// still carries the classification in prose). Set from JIRA_OUTBOX_LABELS_FIELD /
	// JIRA_OUTBOX_VERSION_FIELD once the operator scaffolds the fields in GBFSM.
	jiraOutboxLabelsField  string
	jiraOutboxVersionField string
	// jiraBackfillDryRun runs the one-shot FSM→GBFSM backfill in DRY-RUN /
	// LOG-ONLY mode once at startup (epic ka-0000002637, Phase 5a ka-0000000123).
	// Default false ⇒ inert. When true it enumerates the in-scope FSM backlog
	// READ-ONLY and logs the GBFSM ticket each FSM ticket WOULD upsert, making ZERO
	// Jira writes. The LIVE backfill run is Phase 6 (ka-0000000885), gated on
	// Phase 4 rules + a green E2E pipeline — NOT wired here. Set from
	// JIRA_BACKFILL_DRY_RUN.
	jiraBackfillDryRun bool
	jiraWebhookSecret  string // HMAC secret for Jira webhook signature verification
	jiraWebhookDevMode bool   // skip HMAC verification (dev/testing only)
	// jiraWebhookDeadmanWindow / jiraWebhookDeadmanInterval tune the inbound
	// webhook deadman monitor (jira_webhook_deadman.go, bug ka-0000000426).
	// Window is how long the receive path may go without an authenticated
	// delivery before it is judged dark (given upstream activity exists);
	// Interval is the evaluation/heartbeat cadence. Zero uses the package
	// defaults (2h / 5m). Set from JIRA_WEBHOOK_DEADMAN_WINDOW /
	// JIRA_WEBHOOK_DEADMAN_INTERVAL.
	jiraWebhookDeadmanWindow   time.Duration
	jiraWebhookDeadmanInterval time.Duration
	jiraAgentToken             string   // bearer token gating /api/jira/reply (JIRA_BRIDGE_AGENT_TOKEN); empty = open (local dev)
	jiraMentionDisabled        bool     // disable @gardenio mention handling
	jiraMentionTrigger         string   // mention trigger (default: "@gardenio")
	jiraMentionBotTrigger      string   // optional secondary trigger (e.g., JIRA user mention)
	jiraMentionBotAccountIDs   []string // Jira accountIds a native @-mention of the bot targets (JIRA_MENTION_BOT_ACCOUNT_IDS)
	jiraBotAccountIDs          []string // SET of gardenio bot Jira accountIds whose comments must never wake the assignee (JIRA_BOT_ACCOUNT_IDS)
	jiraMentionProject         string   // default project for spawned agents
	jiraBridgeDefaultOperator  string   // designated human operator for follow-up respawns whose task bead has no human operated_by
	listenAddr                 string
	// metricsAddr is the internal-only listen address for the Prometheus
	// /metrics server. Default :9093 — a port the vmagent-egress NetworkPolicy
	// pre-allows (9091/9092/9093/3000/8429) and that is free on the jira-bridge
	// pod (gitlab-bridge uses :9092). Kept separate from the public :8091
	// listener so /metrics is never internet-reachable.
	metricsAddr     string
	metricsQueries  []bridge.MetricQuery // JQL-count metrics; empty disables the collector
	metricsInterval time.Duration        // base scrape cadence; default 120s
	ageQueries      []bridge.AgeQuery    // per-issue age metrics; empty disables the age collector
	ageInterval     time.Duration        // base age-scrape cadence; default 600s
	logLevel        string
	statePath       string
	basePath        string // optional URL prefix for serving routes under a fan-out path (e.g. "/jira")
}

func parseConfig() *config {
	pollInterval := 60 * time.Second
	if v := os.Getenv("JIRA_POLL_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			pollInterval = d
		}
	}

	metricsInterval := 120 * time.Second
	if v := os.Getenv("JIRA_METRICS_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			metricsInterval = d
		}
	}

	ageInterval := 600 * time.Second
	if v := os.Getenv("JIRA_AGE_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			ageInterval = d
		}
	}

	// Webhook deadman tuning. Zero (unset/malformed) lets NewJiraWebhookMonitor
	// apply its package defaults (2h window / 5m interval).
	var webhookDeadmanWindow time.Duration
	if v := os.Getenv("JIRA_WEBHOOK_DEADMAN_WINDOW"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			webhookDeadmanWindow = d
		}
	}
	var webhookDeadmanInterval time.Duration
	if v := os.Getenv("JIRA_WEBHOOK_DEADMAN_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			webhookDeadmanInterval = d
		}
	}

	skipAssigned := os.Getenv("JIRA_SKIP_ASSIGNED")
	disableTransitions := os.Getenv("JIRA_DISABLE_TRANSITIONS")
	enableClaimTransition := os.Getenv("JIRA_ENABLE_CLAIM_TRANSITION")
	mentionDisabled := os.Getenv("JIRA_MENTION_DISABLED")
	webhookDevMode := os.Getenv("JIRA_WEBHOOK_DEV_MODE")
	captureAllCustom := os.Getenv("JIRA_CAPTURE_ALL_CUSTOM")
	outboxEnabled := os.Getenv("JIRA_OUTBOX_ENABLED")
	backfillDryRun := os.Getenv("JIRA_BACKFILL_DRY_RUN")

	return &config{
		beadsHTTPAddr:               envOrDefault("BEADS_HTTP_ADDR", "http://localhost:8080"),
		jiraBaseURL:                 os.Getenv("JIRA_BASE_URL"),
		jiraEmail:                   os.Getenv("JIRA_EMAIL"),
		jiraAPIToken:                os.Getenv("JIRA_API_TOKEN"),
		jiraProjects:                splitCSV(envOrDefault("JIRA_PROJECTS", "PE,DEVOPS")),
		jiraSpawnProjects:           splitCSV(os.Getenv("JIRA_SPAWN_PROJECTS")),
		jiraStatuses:                splitCSV(envOrDefault("JIRA_STATUSES", "To Do,Ready for Development")),
		jiraNonActionableStatuses:   splitCSV(os.Getenv("JIRA_NONACTIONABLE_STATUSES")),
		jiraIssueTypes:              splitCSV(envOrDefault("JIRA_ISSUE_TYPES", "Bug,Task,Story,Epic")),
		jiraFixVersions:             splitCSV(os.Getenv("JIRA_FIX_VERSIONS")),
		jiraRequiredLabels:          splitCSV(os.Getenv("JIRA_REQUIRED_LABELS")),
		jiraExcludedLabels:          splitCSV(os.Getenv("JIRA_EXCLUDED_LABELS")),
		jiraFilterProjects:          splitCSV(os.Getenv("JIRA_FILTER_PROJECTS")),
		jiraTriggerProjects:         splitCSV(os.Getenv("JIRA_TRIGGER_PROJECTS")),
		jiraCustomJQL:               os.Getenv("JIRA_CUSTOM_JQL"),
		jiraProjectMap:              parseBoatProjects(os.Getenv("BOAT_PROJECTS")),
		jiraCustomFields:            parseCustomFieldsEnv(envOrDefault("JIRA_CUSTOM_FIELDS", "customfield_11976:jira_issue_url")),
		jiraCaptureAllCustom:        captureAllCustom == "true" || captureAllCustom == "1",
		jiraPollInterval:            pollInterval,
		jiraSkipAssigned:            skipAssigned == "true" || skipAssigned == "1",
		jiraDisableTransitions:      disableTransitions == "true" || disableTransitions == "1",
		jiraEnableClaimTransition:   enableClaimTransition == "true" || enableClaimTransition == "1",
		jiraBotAccountID:            os.Getenv("JIRA_BOT_ACCOUNT_ID"),
		jiraBotAccountIDs:           splitCSV(os.Getenv("JIRA_BOT_ACCOUNT_IDS")),
		jiraFieldAllowlistMode:      os.Getenv("JIRA_FIELD_ALLOWLIST_MODE"),
		jiraOutboxEnabled:           outboxEnabled == "true" || outboxEnabled == "1",
		jiraOutboxAnalysisIssueType: os.Getenv("JIRA_OUTBOX_ANALYSIS_ISSUE_TYPE"),
		jiraOutboxLabelsField:       os.Getenv("JIRA_OUTBOX_LABELS_FIELD"),
		jiraOutboxVersionField:      os.Getenv("JIRA_OUTBOX_VERSION_FIELD"),
		jiraBackfillDryRun:          backfillDryRun == "true" || backfillDryRun == "1",
		jiraWebhookSecret:           os.Getenv("JIRA_WEBHOOK_SECRET"),
		jiraWebhookDevMode:          webhookDevMode == "true" || webhookDevMode == "1",
		jiraWebhookDeadmanWindow:    webhookDeadmanWindow,
		jiraWebhookDeadmanInterval:  webhookDeadmanInterval,
		jiraAgentToken:              os.Getenv("JIRA_BRIDGE_AGENT_TOKEN"),
		jiraMentionDisabled:         mentionDisabled == "true" || mentionDisabled == "1",
		jiraMentionTrigger:          envOrDefault("JIRA_MENTION_TRIGGER", "@gardenio"),
		jiraMentionBotTrigger:       os.Getenv("JIRA_MENTION_BOT_TRIGGER"),
		jiraMentionBotAccountIDs:    splitCSV(os.Getenv("JIRA_MENTION_BOT_ACCOUNT_IDS")),
		jiraMentionProject:          envOrDefault("JIRA_MENTION_PROJECT", "gardenio"),
		jiraBridgeDefaultOperator:   os.Getenv("JIRA_BRIDGE_DEFAULT_OPERATOR"),
		listenAddr:                  envOrDefault("JIRA_LISTEN_ADDR", ":8091"),
		metricsAddr:                 envOrDefault("METRICS_LISTEN_ADDR", ":9093"),
		metricsQueries:              parseMetricsQueries(os.Getenv("JIRA_METRICS_QUERIES")),
		metricsInterval:             metricsInterval,
		ageQueries:                  parseAgeQueries(os.Getenv("JIRA_AGE_QUERIES")),
		ageInterval:                 ageInterval,
		logLevel:                    envOrDefault("LOG_LEVEL", "info"),
		statePath:                   envOrDefault("STATE_PATH", "/data/jira-bridge-state.json"),
		basePath:                    normalizeBasePath(os.Getenv("BASE_PATH")),
	}
}

// normalizeBasePath returns an empty string for unset/root input, otherwise a
// leading-slash, no-trailing-slash form (e.g. "/jira"). Mirrors beads-viewer.
func normalizeBasePath(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimRight(s, "/")
	if s == "" {
		return ""
	}
	if !strings.HasPrefix(s, "/") {
		s = "/" + s
	}
	return s
}

// wrapBasePath returns an http.Handler that always serves inner at root (for
// in-cluster probes and strip-prefix proxies) and, when basePath is set, also
// serves inner under basePath/ with the prefix stripped (for prefix-preserving
// proxies). basePath must be in normalized form ("" or "/jira"-shaped).
func wrapBasePath(inner http.Handler, basePath string) http.Handler {
	if basePath == "" {
		return inner
	}
	top := http.NewServeMux()
	top.Handle("/", inner)
	top.Handle(basePath+"/", http.StripPrefix(basePath, inner))
	return top
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// parseBoatProjects parses the BOAT_PROJECTS env var into a JIRA prefix → boat
// project name map. The format is comma-separated entries of the form:
//
//	{project_name}={git_url}:{jira_prefix}
//
// Example: "gardenio=https://github.com/org/gardenio.git:kd,monorepo=https://gitlab.com/org/repo:PE"
// Result:  {"KD": "gardenio", "PE": "monorepo"}
func parseBoatProjects(s string) map[string]string {
	m := make(map[string]string)
	for _, entry := range strings.Split(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		// Split on first '=' to get project name and rest (url:prefix).
		eqIdx := strings.IndexByte(entry, '=')
		if eqIdx < 0 {
			continue
		}
		projectName := entry[:eqIdx]
		rest := entry[eqIdx+1:]
		// The JIRA prefix is after the last ':' in rest.
		colonIdx := strings.LastIndexByte(rest, ':')
		if colonIdx < 0 {
			continue
		}
		jiraPrefix := strings.ToUpper(strings.TrimSpace(rest[colonIdx+1:]))
		if jiraPrefix != "" && projectName != "" {
			m[jiraPrefix] = projectName
		}
	}
	return m
}

// parseCustomFieldsEnv parses the JIRA_CUSTOM_FIELDS env var into a JIRA
// custom-field id → bead field name map. Format is comma-separated
// "id:bead_field" pairs, e.g. "customfield_11976:jira_issue_url".
func parseCustomFieldsEnv(s string) map[string]string {
	m := make(map[string]string)
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		id, field, ok := strings.Cut(pair, ":")
		id = strings.TrimSpace(id)
		field = strings.TrimSpace(field)
		if !ok || id == "" || field == "" {
			continue
		}
		m[id] = field
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

// unionProjects returns the de-duplicated concatenation of two project lists,
// preserving first-seen order (a's entries first). It underpins the deadman
// scope union of poller projects + mention-spawn projects (ka-0000005656).
// Empty extra ⇒ a copy of a (order + membership unchanged).
func unionProjects(a, extra []string) []string {
	seen := make(map[string]struct{}, len(a)+len(extra))
	out := make([]string, 0, len(a)+len(extra))
	for _, list := range [][]string{a, extra} {
		for _, p := range list {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if _, dup := seen[p]; dup {
				continue
			}
			seen[p] = struct{}{}
			out = append(out, p)
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

func init() {
	if v := os.Getenv("VERSION"); v != "" {
		version = v
	}
}

// buildMetricsServer builds the internal Prometheus metrics HTTP server backed
// by the default gatherer. client_golang auto-registers the Go and process
// collectors there, and it is where the JQL-count collector's
// promauto-registered gardenio_jira_* series land (jira_metrics.go). The server
// is not started here; the caller invokes ListenAndServe in a goroutine.
// Mirrors gitlab-bridge's buildMetricsServer (cmd/gitlab-bridge/main.go).
func buildMetricsServer(addr string) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(prometheus.DefaultGatherer, promhttp.HandlerOpts{}))
	return &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
}

// metricQueryJSON is the wire shape of one entry in the JIRA_METRICS_QUERIES
// env JSON array (open decision #2 default: env JSON list, swappable for a
// mounted ConfigMap later without changing this code). interval is an optional
// per-query override parsed as a Go duration string (e.g. "300s").
type metricQueryJSON struct {
	Name     string `json:"name"`
	Help     string `json:"help"`
	JQL      string `json:"jql"`
	Project  string `json:"project"`
	Interval string `json:"interval"`
}

// ageQueryJSON is the wire shape of one entry in the JIRA_AGE_QUERIES env JSON
// array. It mirrors metricQueryJSON with an extra optional maxIssues cap on how
// many matching issues are paged per cycle (0 = collector default).
type ageQueryJSON struct {
	Name      string `json:"name"`
	Help      string `json:"help"`
	JQL       string `json:"jql"`
	Project   string `json:"project"`
	Interval  string `json:"interval"`
	MaxIssues int    `json:"maxIssues"`
}

// parseAgeQueries parses the JIRA_AGE_QUERIES env var (a JSON array of {name,
// help, jql, project, interval, maxIssues}) into the age collector's query list.
// Entries missing a name or jql are skipped. An unset or malformed value yields
// nil, which disables the age collector — the default deployment is unchanged
// until the age query list is wired in.
func parseAgeQueries(s string) []bridge.AgeQuery {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var raw []ageQueryJSON
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil
	}
	queries := make([]bridge.AgeQuery, 0, len(raw))
	for _, q := range raw {
		name := strings.TrimSpace(q.Name)
		jql := strings.TrimSpace(q.JQL)
		if name == "" || jql == "" {
			continue
		}
		var interval time.Duration
		if iv := strings.TrimSpace(q.Interval); iv != "" {
			if d, err := time.ParseDuration(iv); err == nil {
				interval = d
			}
		}
		maxIssues := q.MaxIssues
		if maxIssues < 0 {
			maxIssues = 0
		}
		queries = append(queries, bridge.AgeQuery{
			Name:      name,
			Help:      strings.TrimSpace(q.Help),
			JQL:       jql,
			Project:   strings.TrimSpace(q.Project),
			Interval:  interval,
			MaxIssues: maxIssues,
		})
	}
	if len(queries) == 0 {
		return nil
	}
	return queries
}

// parseMetricsQueries parses the JIRA_METRICS_QUERIES env var (a JSON array of
// {name, help, jql, project, interval}) into the collector's query list.
// Entries missing a name or jql are skipped. An unset or malformed value yields
// nil, which disables the collector — the default deployment is unchanged until
// the query list is wired in.
func parseMetricsQueries(s string) []bridge.MetricQuery {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var raw []metricQueryJSON
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil
	}
	queries := make([]bridge.MetricQuery, 0, len(raw))
	for _, q := range raw {
		name := strings.TrimSpace(q.Name)
		jql := strings.TrimSpace(q.JQL)
		if name == "" || jql == "" {
			continue
		}
		var interval time.Duration
		if iv := strings.TrimSpace(q.Interval); iv != "" {
			if d, err := time.ParseDuration(iv); err == nil {
				interval = d
			}
		}
		queries = append(queries, bridge.MetricQuery{
			Name:     name,
			Help:     strings.TrimSpace(q.Help),
			JQL:      jql,
			Project:  strings.TrimSpace(q.Project),
			Interval: interval,
		})
	}
	if len(queries) == 0 {
		return nil
	}
	return queries
}

// registerHealthHandlers wires /healthz and /readyz onto the given mux. The
// /healthz payload includes the running version so rollouts can be observed
// from outside the pod.
func registerHealthHandlers(mux *http.ServeMux, ver string) {
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","version":"%s"}`, ver)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok"}`)
	})
}

// buildMentionHandler returns a JIRA mention handler for @gardenio comments,
// or nil when mention handling is disabled via JIRA_MENTION_DISABLED.
func buildMentionHandler(cfg *config, daemon *beadsapi.Client, history bridge.JiraCommentHistoryClient, logger *slog.Logger, users bridge.JiraUserLookup, writePolicy *bridge.WritePolicyStore, spawnAck *bridge.JiraSpawnAcknowledger) *bridge.JiraMentionHandler {
	if cfg.jiraMentionDisabled {
		return nil
	}
	h := bridge.NewJiraMentionHandler(buildMentionHandlerConfig(cfg, daemon, history, logger, users, writePolicy, spawnAck))
	logger.Info("jira-mention: handler enabled",
		"trigger", cfg.jiraMentionTrigger,
		"bot_trigger", cfg.jiraMentionBotTrigger,
		"bot_account_ids", cfg.jiraMentionBotAccountIDs,
		"project", cfg.jiraMentionProject,
		"project_map", cfg.jiraProjectMap)
	return h
}

// buildMentionHandlerConfig maps env-parsed config into the mention handler's
// config struct. Extracted from buildMentionHandler so the mapping is
// unit-testable (same pattern as buildJiraPollerConfig). ProjectMap is the
// SAME per-prefix map the poller gets (BOAT_PROJECTS), so a mention on a
// mapped ticket spawns into the mapped boat project (G3, epic ka-0000007401).
func buildMentionHandlerConfig(cfg *config, daemon *beadsapi.Client, history bridge.JiraCommentHistoryClient, logger *slog.Logger, users bridge.JiraUserLookup, writePolicy *bridge.WritePolicyStore, spawnAck *bridge.JiraSpawnAcknowledger) bridge.JiraMentionHandlerConfig {
	c := bridge.JiraMentionHandlerConfig{
		Daemon:        daemon,
		Users:         users,
		History:       history,
		Trigger:       cfg.jiraMentionTrigger,
		BotTrigger:    cfg.jiraMentionBotTrigger,
		BotAccountIDs: cfg.jiraMentionBotAccountIDs,
		Project:       cfg.jiraMentionProject,
		ProjectMap:    cfg.jiraProjectMap,
		Logger:        logger,
		WritePolicy:   writePolicy,
	}
	// Wire the spawn-ack onto OnSpawn (Slack agent-card parity, plan
	// ka-0000007401). A nil acknowledger leaves OnSpawn nil — the legacy
	// silent-until-first-reply behavior — so unit tests that omit it are
	// unaffected.
	if spawnAck != nil {
		c.OnSpawn = spawnAck.OnSpawn()
	}
	return c
}

// buildJiraPollerConfig maps env-parsed config into the poller's config
// struct. Extracted from main() so the mapping is unit-testable.
func buildJiraPollerConfig(cfg *config, logger *slog.Logger) bridge.JiraPollerConfig {
	return bridge.JiraPollerConfig{
		Projects:         cfg.jiraProjects,
		Statuses:         cfg.jiraStatuses,
		IssueTypes:       cfg.jiraIssueTypes,
		FixVersions:      cfg.jiraFixVersions,
		RequiredLabels:   cfg.jiraRequiredLabels,
		ExcludedLabels:   cfg.jiraExcludedLabels,
		FilterProjects:   cfg.jiraFilterProjects,
		CustomJQL:        cfg.jiraCustomJQL,
		ProjectMap:       cfg.jiraProjectMap,
		CustomFields:     cfg.jiraCustomFields,
		CaptureAllCustom: cfg.jiraCaptureAllCustom,
		PollInterval:     cfg.jiraPollInterval,
		SkipAssigned:     cfg.jiraSkipAssigned,
		BotAccountID:     cfg.jiraBotAccountID,
		Logger:           logger,
	}
}
