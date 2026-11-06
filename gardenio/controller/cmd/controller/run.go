package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"k8s.io/client-go/kubernetes"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/bridgetokenrotator"
	"gardenio/controller/internal/config"
	"gardenio/controller/internal/coopmux"
	"gardenio/controller/internal/healthsnapshot"
	pursewatcher "gardenio/controller/internal/lands/watcher"
	gardeniometrics "gardenio/controller/internal/metrics"
	"gardenio/controller/internal/microvm"
	"gardenio/controller/internal/pgbudget"
	"gardenio/controller/internal/podmanager"
	"gardenio/controller/internal/poolmanager"
	"gardenio/controller/internal/pvcgc"
	"gardenio/controller/internal/pvgc"
	"gardenio/controller/internal/reconciler"
	"gardenio/controller/internal/secretreconciler"
	"gardenio/controller/internal/spawnguard"
	"gardenio/controller/internal/spendlogs"
	"gardenio/controller/internal/statusreporter"
	"gardenio/controller/internal/subscriber"
)

// run is the main controller loop. It reads beads events and dispatches
// pod operations. Separated from main() for testability.
func run(ctx context.Context, logger *slog.Logger, cfg *config.Config, k8sClient kubernetes.Interface, watcher subscriber.Watcher, pods podmanager.Manager, status statusreporter.Reporter, rec *reconciler.Reconciler, daemon, machineDaemon *beadsapi.Client, secretRec *secretreconciler.Reconciler, pool *poolmanager.Manager, muxClient *coopmux.Client, pvcCollector *pvcgc.Collector, pvCollector *pvgc.Collector, staleReaper *statusreporter.StaleReaper, snapshotTracker *healthsnapshot.Tracker, ft *freezeThawLifecycle, microvmLifecycle *microvm.Lifecycle, microvmSessionVendor *microvm.SessionVendor, eventMetrics *gardeniometrics.EventMetrics, spendMetrics *gardeniometrics.SpendMetrics, resumeMetrics *gardeniometrics.ResumeMetrics, freezeMetrics *gardeniometrics.FreezeMetrics, probeMetrics *gardeniometrics.ProbeMetrics, xcheckMetrics *gardeniometrics.XCheckMetrics, microvmLaunchMetrics *gardeniometrics.MicrovmLaunchMetrics, claudeStatusMetrics *bridge.ClaudeStatusMetrics, tokMetrics *tokenMetrics, rotatorMetrics *bridgetokenrotator.Metrics, toolLatencyMetrics *gardeniometrics.ToolLatencyMetrics) error {
	// GitHub App per-agent installation tokens (nil unless GITHUB_APP_ENABLED).
	// Built before the startup reconcile below: the reconciler is a second
	// pod-creation door (drift/spot-loss recreates, beads created during
	// downtime, retries of spawns the event door refused) and must mint through
	// the same issuer, or those pods would get the project's shared PAT.
	ghApp := newGithubAppIssuer(cfg, k8sClient, logger.With("component", "github-app"), tokMetrics)
	if ghApp != nil && rec != nil {
		rec.SetGitCredentialApplier(ghApp.reconcilerApplier(logger, daemon))
	}
	// The release deployer's read-only token, from the fleet App, refreshed in
	// the background (gd-q48ldwLspl). Nil unless the chart sets its Secret.
	go releaseDeployerTokenFor(ghApp, k8sClient, cfg.Namespace, os.Getenv, logger.With("component", "release-deployer-token")).
		RunLoop(ctx, releaseDeployerTokenInterval)
	// A frozen, paused, gone or unreadable tenant land gets no GitHub token at
	// any door or refresh (gd-Ld5r7Ok2rK).
	if ghApp != nil && daemon != nil {
		ghApp.landHalt = landHaltRefusal(cfg, asSpawnWallDaemon(daemon))
	}
	// The stale reaper reads a dead crew's linked PR state through the fleet
	// App, so a task whose PR closed or merged is reopened rather than flagged
	// task:orphaned-with-pr forever (gd-ovzZSuuHny). Set before
	// runPeriodicSync starts the reap loop below.
	if staleReaper != nil {
		if lookup := ghApp.prStateLookup(); lookup != nil {
			staleReaper.SetPRStateLookup(lookup)
		}
	}

	// Per-agent beads tokens: the reconciler's pod-creation door must mount the
	// agent's own token too (see reconciler.AgentTokenApplier).
	if cfg.PerAgentTokens && rec != nil {
		rec.SetAgentTokenApplier(reconcilerAgentTokenApplier(logger, machineDaemon, daemon, k8sClient, tokMetrics))
	}

	// Lands spawn wall: the reconciler's create-missing door refuses the same
	// tenant spawns the event door does (step 8.3).
	// The per-land spawn rate (step 10.4) is one cap shared by both doors.
	landSpawnRate = spawnguard.NewKeyed(cfg.LandsSpawnsPerHour, time.Hour)
	if rec != nil && daemon != nil {
		rec.SetSpawnWall(reconcilerSpawnWall(cfg, logger, asSpawnWallDaemon(daemon), landSpawnRate))
	}

	// Run reconciler once at startup to catch beads created during downtime.
	if rec != nil {
		logger.Info("running startup reconciliation")
		if err := rec.Reconcile(ctx); err != nil {
			logger.Warn("startup reconciliation failed", "error", err)
		}
	}

	// Start beads watcher in background.
	watcherDone := make(chan error, 1)
	go func() {
		watcherDone <- watcher.Start(ctx)
	}()

	// Start periodic SyncAll reconciliation.
	syncInterval := 60 * time.Second
	if cfg.CoopSyncInterval > 0 {
		syncInterval = cfg.CoopSyncInterval
	}
	// Auth failover handler for budget_exceeded / rate_limited events.
	// Created before runPeriodicSync so the exhausted-account recovery sweep can
	// run in the sync loop.
	failover := newAuthFailover(daemon, machineDaemon, logger, cfg.AuthFailoverCooldown, cfg.AuthMaxFailoverDuration)
	if err := failover.RebuildState(ctx); err != nil {
		logger.Warn("failed to rebuild auth failover runtime state", "error", err)
	}

	// Account-rotation restarter: restarts running agent pods that still carry
	// a newly-exhausted account's baked LiteLLM key (ka-0000007997). Runs once
	// per periodic sync cycle after the account cache is refreshed. It asks
	// coopmux for each session's live account so pods that already switched
	// credentials in-process are not deleted (ka-0000005463); typed-nil guard
	// because a nil *coopmux.Client stored in the interface would still
	// dispatch onto a nil receiver.
	var restarterSessions liveSessionAccounts
	if muxClient != nil {
		restarterSessions = muxClient
	}
	accountRestarter := newAccountRotationRestarter(pods, cfg.Namespace, restarterSessions, logger).
		// Route the live-session deletes through the SAME graceful drain the
		// node-drain path uses, so a rotation restart stops
		// killing agents mid-response (ka-0000001482). Shares the reconciler's
		// tracker so a pod already draining for another reason is not double-nudged.
		withDrainGate(rec.UpgradeTracker(), cfg.UpgradeDrainTimeout).
		// Record the REAL reason on the agent bead before deleting, so the death
		// report stops attributing controller deletes to Spot/node-drain.
		withBeadStamper(daemon)

	// Start NATS listener for account exhaustion events (reactive failover).
	exhaustionListener := newAccountExhaustionListener(accountExhaustionConfig{
		NatsURL:   cfg.NatsURL,
		NatsToken: cfg.NatsToken,
		Failover:  failover,
		Logger:    logger.With("component", "account-exhaustion"),
	})
	if exhaustionListener != nil {
		go func() {
			if err := exhaustionListener.Start(ctx, cfg.NatsURL, cfg.NatsToken, cfg.NatsUser); err != nil {
				logger.Error("account exhaustion listener failed", "error", err)
			}
		}()
	}

	// Start NATS listener for terminal, non-failover credential failures
	// (ka-0000003962). DIAGNOSTIC-only: it persists error_category +
	// error_detail onto the affected agent bead so a first-turn auth failure
	// (e.g. "401 key not allowed to access model") leaves a durable record
	// instead of vanishing with the pod. It NEVER triggers failover — a
	// terminal auth failure on a distinct subject must not rotate credentials.
	terminalAuthListener := newTerminalAuthFailureListener(terminalAuthFailureConfig{
		NatsURL: cfg.NatsURL,
		Daemon:  daemon,
		Logger:  logger.With("component", "terminal-auth-failure"),
	})
	if terminalAuthListener != nil {
		go func() {
			if err := terminalAuthListener.Start(ctx, cfg.NatsURL, cfg.NatsToken, cfg.NatsUser); err != nil {
				logger.Error("terminal auth failure listener failed", "error", err)
			}
		}()
	}

	// Plan-chat assistant-turn persistence, rehomed off the beads-viewer
	// (task ka-0000006891, P3 of plan ka-0000000187). An always-on NATS consumer
	// (+ full-tail JetStream replay) that mints every plan-assistant reply as a
	// durable transcript turn bead — now controller-hosted so it persists
	// independent of whether a viewer replica is up. Nil-safe / best-effort.
	if minter := startAssistantTurnMinter(cfg, daemon, logger); minter != nil {
		defer minter.Close()
	}

	// Tool-latency HOOK_EVENTS consumer (P2, plan
	// gardenio/plans/agent-tool-latency-attribution.md, bead gd-sWwPnwkZtw). A
	// durable JetStream consumer that turns every agent PostToolUse hook event
	// into bounded per-tool/per-cmd-class histogram samples plus (above
	// threshold) a redacted slow_tool_calls ledger row. Nil-safe / best-effort.
	if consumer := startToolLatencyConsumer(cfg, daemon, toolLatencyMetrics, logger); consumer != nil {
		defer consumer.Close()
	}

	// Rotation notifier publishes account rotation events to NATS for the
	// slack-bridge to pick up and post as Slack notifications.
	// Fall back to the controller's own NATS_TOKEN (infra token under
	// nats.auth.subjectIsolation) — the chart never sets COOP_NATS_TOKEN on
	// the controller pod, so without the fallback these publishers connect
	// anonymously and are refused once server auth is enabled (ka-0000006513).
	natsToken := os.Getenv("COOP_NATS_TOKEN")
	natsUser := os.Getenv("COOP_NATS_USER")
	if natsToken == "" {
		natsToken = cfg.NatsToken
		natsUser = cfg.NatsUser
	}
	rotNotifier := newRotationNotifier(cfg.NatsURL, natsToken, natsUser, logger.With("component", "rotation-notifier"), snapshotTracker)
	if rotNotifier != nil {
		failover.rotation = rotNotifier
		defer rotNotifier.Close()
	}

	// Credential sync notifier publishes credential lifecycle events to NATS.
	credNotifier := newCredSyncNotifier(cfg.NatsURL, natsToken, natsUser, logger.With("component", "credsync-notifier"))
	if credNotifier != nil {
		defer credNotifier.Close()
	}

	// Sync account credentials to coopmux at startup (non-fatal).
	if muxClient != nil {
		syncAccountsToCoopmux(ctx, logger, daemon, k8sClient, cfg.Namespace, muxClient, credNotifier, cfg.LiteLLMURL, "startup")
	}

	// Ensure LiteLLM model aliases for Claude Code subagent models (non-fatal).
	// Claude Code sends unprefixed model names (e.g. "claude-sonnet-4-6") for
	// subagents and WebFetch, but LiteLLM's default routing sends these to
	// Bedrock-backed model groups which don't support OAuth token forwarding.
	// This sync adds model_group_alias entries that redirect to OAuth-compatible
	// model groups.
	syncLiteLLMModelAliases(ctx, logger, cfg, k8sClient)

	// Start the served-model sync (ka-0000002790): periodically fetch LiteLLM's
	// served-model set (/v1/models ∪ model_group_alias keys) and publish it to
	// cfg so selectAccount can exclude any account whose baked model is not
	// actually served — a target/undeployed-model account (valid key, unserved
	// model) can never be baked as agent cognition and crash-loop the pod. Gated
	// on the operator kill switch AND a configured LiteLLM (URL + master key); if
	// off/unconfigured the loop never runs, cfg.ServedModels stays nil, and the
	// spread gate is a no-op (fail-open).
	if cfg.SpreadServedModelGateEnabled && cfg.LiteLLMURL != "" && cfg.LiteLLMMasterKey != "" {
		servedSync := newServedModelSync(cfg, cfg.LiteLLMURL, cfg.LiteLLMMasterKey, logger)
		go servedSync.RunLoop(ctx)
	} else if cfg.SpreadServedModelGateEnabled {
		logger.Info("served-model spread gate enabled but LiteLLM not configured (URL/master key) — gate inactive (fail-open)")
	}

	// Start proactive rate-limit probe for all secret-backed accounts, plus its
	// liveness watchdog. The watchdog detects a wedged probe loop — the
	// 2026-07-05 silent-freeze failure mode (ka-0000001889) — and raises a
	// WARN + NATS alert so the freeze no longer goes unnoticed for ~24h.
	probe := newRateLimitProbe(daemon, machineDaemon, k8sClient, cfg.Namespace, rotNotifier, probeMetrics, logger)
	go probe.RunLoop(ctx)
	go probe.WatchdogLoop(ctx)

	// Codex subscription refresh keeper (gd-6ggiM3hiV5): the single writer of the
	// rotating ChatGPT login for provider=openai accounts. Inert when no such
	// account bead exists. Runs under leader election like the probe.
	codexKeeper := newCodexKeeper(daemon, machineDaemon, k8sClient, cfg.Namespace, muxClient, logger)
	if cfg.LeaderElection {
		// A predecessor that lost the lease may still be writing a rotated token back.
		codexKeeper.startDelay = codexHandoverGrace
	}
	go codexKeeper.RunLoop(ctx)
	if esc, ok := codexKeeper.beads.(codexEscalations); ok {
		codexLoginBrokers.Set(newCodexLoginBroker(ctx, codexKeeper, esc))
		go func() { <-ctx.Done(); codexLoginBrokers.Set(nil) }()
	}

	// Start probe-event listener: reacts to account_throttled NATS events from
	// the probe and triggers failover for all affected projects. This is the
	// controller-driven path that supersedes the coopmux-reactive budget_exceeded
	// path (account_exhaustion.go), which remains as a fallback until ka-0000003161.
	probeListener := newProbeEventListener(daemon, failover, logger)
	go func() {
		if err := probeListener.Start(ctx, cfg.NatsURL, cfg.NatsToken, cfg.NatsUser); err != nil {
			logger.Error("probe event listener failed", "error", err)
		}
	}()

	// Start LiteLLM spend sync for account beads (populates total_cost_usd).
	// The per-session spend reconciler shares the same LiteLLM master key; it
	// snapshots a per-agent spend baseline at spawn and reconciles the
	// authoritative session cost onto the agent bead at close (ka-0000001543).
	var spendReconciler *sessionSpendReconciler
	if cfg.LiteLLMURL != "" && cfg.LiteLLMMasterKey != "" {
		ss := newSpendSync(daemon, machineDaemon, cfg.LiteLLMURL, cfg.LiteLLMMasterKey, logger)
		go ss.RunLoop(ctx)

		// Real-bill per-dimension gauge (ka-0000000478): poll LiteLLM /spend/tags
		// across every proxy and expose gardenio_tag_cost_usd{dim,value,env} so the
		// cost-rollup by-role/by-project/by-model views read the real bill — the
		// same treatment the by-account rollup and /costs headline got, for the
		// dimensions the account-cost gauge cannot label.
		//
		// Every /spend/tags reader below shares one snapshot per proxy, read at
		// most every tagSpendReadInterval with one read in flight: each read is
		// a full scan of LiteLLM_SpendLogs, and per-caller reads piled up and
		// held the platform node at 100% CPU (gd-oih6JxjJMM).
		tagReader := newTagSpendReader(cfg.LiteLLMMasterKey, tagSpendReadInterval, logger)
		// Where the totals come from (gd-Um9gnz9FuW): auto reads both
		// /spend/tags and LiteLLM_DailyTagSpend until they agree, then only
		// the aggregate, which spend-log retention leaves intact.
		if src, err := parseTagSpendSource(cfg.LiteLLMTagSpendSource); err != nil {
			logger.Error("LITELLM_TAG_SPEND_SOURCE invalid; using auto", "error", err)
			tagReader.source = tagSourceAuto
		} else {
			tagReader.source = src
		}
		tagSync := newTagSpendSync(daemon, cfg.LiteLLMURL, cfg.LiteLLMMasterKey, cfg.Env, spendMetrics, logger)
		tagSync.tags = tagReader
		go tagSync.RunLoop(ctx)

		spendReconciler = newSessionSpendReconciler(daemon, cfg.LiteLLMURL, cfg.LiteLLMMasterKey, logger)
		spendReconciler.tags = tagReader
		// F2 (ka-0000002904): drive gardenio_agent_spend_usd_total from the
		// reconciled per-session close-total — one increment per session. The
		// agent bead carries neither model nor account, so resolve them from the
		// config caches (same account precedence as the per-turn forwarder; model
		// from the resolved account's default).
		spendReconciler.metrics = spendMetrics
		spendReconciler.resolveLabels = func(project, role string) (model, account string) {
			account = resolveSpendAccount(cfg, project, role)
			cfg.ProjectCacheMu.RLock()
			if acct, ok := cfg.AccountCache[account]; ok {
				model = acct.Model
			}
			cfg.ProjectCacheMu.RUnlock()
			if model == "" {
				model = cfg.ClaudeModel
			}
			return model, account
		}
		// ka-0000003221: resolve the per-account LiteLLM base_url for a session so
		// spend reconciliation queries the proxy that actually minted the account's
		// vkey (e.g. an in-cluster proxy), while the global LITELLM_URL stays
		// external. Same account-precedence as resolveLabels; "" means no override
		// and the reconciler falls back to the global URL.
		//
		// It returns acct.BaseURL and NEVER acct.ProxyURL: the spend read path
		// resolves litellm directly and bypasses claudemore, so metering stays
		// byte-identical whether or not a proxy fronts the write path (transparency
		// A1, ka-0000007533). resolveSpendReadBaseURLForAccount is the single-sourced
		// contract for that rule (podspec.go) — it is a named production function,
		// not an inline closure, so the proxy-transparency e2e drives the exact code
		// path a "route spend through claudemore" regression would touch. See
		// gardenio/docs/claudemore-transparency-proof.md.
		spendReconciler.resolveBaseURL = func(project, role string) string {
			return resolveSpendReadBaseURLForAccount(cfg, project, role)
		}
		// Codex subscription accounts (provider=openai) have no LiteLLM spend: skip
		// baseline capture and close-time reconciliation so total_cost_usd stays 0
		// and no retry budget is burned on a tag that never appears.
		spendReconciler.skipSession = func(project, role string) bool {
			return resolveSpendAccountIsOpenAI(cfg, project, role)
		}
		// Background retry worker: when the inline close-time reconciliation can't
		// land the authoritative value (transient /spend/tags error or tag absent
		// from indexing lag), the session is requeued and retried with backoff so
		// the real cost eventually overwrites the gb estimate (ka-0000005130).
		go spendReconciler.RunRetryLoop(ctx)
		// Durable settlement sweep (A3, ka-0000007288): the retry queue above is
		// in-memory and dies with the controller. The sweep re-derives "unsettled"
		// from bead state (closed without a settlement watermark, or with tag
		// spend past it) and re-attempts settlement, publishing the backlog as
		// gardenio_unsettled_sessions.
		go spendReconciler.RunSettlementSweep(ctx, cfg.SpendSweepInterval)

		// Spend-logs sweep (C3, ka-0000000774 — replace-now step 1, decision
		// ka-0000006903): the NEW stateless per-agent attribution path.
		// Periodically recomputes each recent agent's lifetime cost directly
		// from LiteLLM /spend/logs (no baselines/watermarks), publishing
		// gardenio_agent_spend_logs_usd plus the spend_logs_cost_usd bead field
		// alongside the old path's counter/total_cost_usd. The stateful path
		// above keeps running untouched; the P10 x-check pair compares the two
		// per-agent figures and gates the C4 cutover. Shares the reconciler's
		// label resolver so old and new series land on identical
		// {model,account} labels. Set GARDENIO_SPENDLOGS_SWEEP_INTERVAL=0 to
		// disable.
		if cfg.SpendLogsSweepInterval > 0 {
			slSweep := newSpendLogsSweep(daemon, spendlogs.New(cfg.LiteLLMMasterKey, logger),
				cfg.LiteLLMURL, spendMetrics, cfg.SpendLogsSweepInterval, cfg.SpendLogsSweepLookback, logger)
			slSweep.resolveLabels = spendReconciler.resolveLabels
			go slSweep.RunLoop(ctx)
		} else {
			logger.Info("spend-logs sweep disabled (GARDENIO_SPENDLOGS_SWEEP_INTERVAL=0)")
		}

		// Start LiteLLM virtual-key ACL widen loop: every pass widens each
		// account key's model allow-list to the served Anthropic chat groups, so
		// a new model group needs no hand-edited key ACLs. Shares this block's
		// LiteLLM URL + master key. See gardenio/docs/litellm-key-rotation.md.
		go newKeyACLWidener(daemon, cfg.LiteLLMURL, cfg.LiteLLMMasterKey,
			cfg.LiteLLMKeyRotationInterval, logger).RunLoop(ctx)
	}

	// Start advice expiration sweeper: closes open advice beads whose
	// expires_at timestamp has passed, so they stop appearing in gb advice
	// list and gb prime. Setting GARDENIO_ADVICE_SWEEP_INTERVAL=0 disables.
	go newAdviceSweeper(daemon, cfg.AdviceSweepInterval, cfg.AdviceCompactionEnabled, logger).RunLoop(ctx)

	// Start per-gardener spend roll-up (ka-0000003220): periodically sums each
	// gardener's authoritative agent spend — its iteration runs plus the crew
	// they transitively spawned — onto gardener_spend_usd so /gardeners can surface
	// and sort wasteful gardeners. It only AGGREGATES the per-agent total_cost_usd
	// the session-close reconciler already wrote, so it needs no LiteLLM key and
	// runs unconditionally.
	go newGardenerSpendSync(daemon, logger).RunLoop(ctx)

	// Start the per-plan crew spend roll-up (gd-kcy4IFNzv5): sums the crew the
	// /plans crew card counts onto each plan epic's plan_spend_usd, which the
	// farm document reads as FarmPlan.Cost. Same inputs and cadence as the
	// gardener roll-up above.
	go newPlanSpendSync(daemon, logger).RunLoop(ctx)

	// Start the release reconciler (plan gd-UZKZ9H25UD step 4,
	// release_reconciler.go): once a minute it compares what the gardenio Helm
	// release runs with the newest deployment on prod and records the observed
	// one when they differ (a manual helm upgrade or rollback).
	startReleaseReconciler(ctx, daemon, k8sClient, cfg.KubeConfig, cfg.Namespace, logger)

	// Start the Lands purse watcher (Track B step 8.4, internal/lands/watcher):
	// every minute it nudges, waits the grace and suspends a tenant land's
	// agents at the wall (park_reason=purse_empty), posts the 50/80/100%
	// notices in the land's #general and mails the owner at 80/100%, and on a
	// credit grant posts "purse refilled" and wakes the steward hook. With no
	// tenant land it costs one project list a minute.
	go newPurseWatcher(ctx, daemon, pods, cfg.LiteLLMURL, cfg.LiteLLMMasterKey, logger).Run(ctx, pursewatcher.Interval)

	// Start the Lands provisioner (Track B step 8.6, internal/lands/provision)
	// when LANDS_PROVISION_ENABLED: approved plot applications become lands,
	// and gb lands close takes them apart (lands_provisioner.go).
	startLandsProvisioner(ctx, daemon, k8sClient, landsDynamicClient(cfg.KubeConfig, logger), cfg.Namespace, cfg.LiteLLMURL, cfg.LiteLLMMasterKey, logger)

	// Start the Lands purge sweep (step 8.11, internal/lands/purge) when
	// LANDS_PURGE_ENABLED: a closed land's beads and sessions go after the
	// retention period the privacy policy states (lands_purge.go).
	startLandsPurge(ctx, daemon, cfg, os.Getenv, logger)

	// Keep tenant land namespaces' land Secrets (each land's coopmux token and
	// its own credential's copy) in line with the controller's config when
	// LANDS_NAMESPACES_ENABLED (lands_sweep.go, gd-ApgYe740ps).
	if landNamespacesEnabled(os.Getenv) && k8sClient != nil {
		go runLandSecretSync(ctx, cfg, k8sClient, logger, landSecretSyncInterval)
	}
	// Answer the NATS auth callout for tenant lands' own NATS users
	// (lands_nats.go, gd-sS7bSXOdGs).
	startLandNatsCallout(ctx, cfg, logger)

	// Start the Claude status-page poller — leg B of the Claude-outage detector
	// (task ka-0000003022, epic ka-0000000487). Publishes gardenio_claude_status_*
	// so the ClaudeUpstreamOutage alert can say whether the provider corroborates
	// what leg A (gardenio_agent_upstream_error_active) is already measuring on our
	// side. Read-only, unauthenticated, and it only ever writes metrics — so it
	// runs unconditionally. Set GARDENIO_CLAUDE_STATUS_POLL_INTERVAL=0 to disable.
	if cfg.ClaudeStatusPollInterval > 0 {
		claudeStatus := bridge.NewClaudeStatusPoller(bridge.ClaudeStatusConfig{
			URL:          cfg.ClaudeStatusURL,
			FallbackURLs: cfg.ClaudeStatusFallbackURLs,
			Interval:     cfg.ClaudeStatusPollInterval,
			Logger:       logger,
			// Publish onto the series registered on promRegistry in main() (the
			// one :9092 serves), NOT a fresh unregistered set — otherwise leg B
			// is scraped-but-empty (ka-0000008266). Both this branch and the
			// metrics construction are gated on ClaudeStatusPollInterval > 0, so
			// claudeStatusMetrics is non-nil here.
			Metrics: claudeStatusMetrics,
		})
		go func() { _ = claudeStatus.Run(ctx) }()
	} else {
		logger.Info("claude status poller disabled (GARDENIO_CLAUDE_STATUS_POLL_INTERVAL=0)")
	}

	// Start the cost-audit cross-check sweep (C2, plan ka-0000003747). On a slow
	// cadence it recomputes conservation pairs — the same quantity derived two
	// independent ways — and publishes gardenio_xcheck_value / gardenio_xcheck_agreement
	// (the corroboration score). PromQL-backed pairs read the fleet metrics back
	// out of vmsingle; bead-scan pairs sum the stateful-era cost fields directly.
	// Set GARDENIO_XCHECK_SWEEP_INTERVAL=0 to disable.
	if cfg.XCheckSweepInterval > 0 {
		xcheckVM := &xcheckVMClient{upstream: cfg.MetricsQueryURL, http: &http.Client{Timeout: 15 * time.Second}}
		go newXCheckSweep(daemon, xcheckVM, xcheckMetrics, cfg.XCheckSweepInterval, logger).RunLoop(ctx)
	} else {
		logger.Info("cost-audit cross-check sweep disabled (GARDENIO_XCHECK_SWEEP_INTERVAL=0)")
	}

	// Start the daily Postgres efficiency report (gd-ylgk1jBhLi): it posts to
	// the land's #machines at the configured UTC hour and files a task for a
	// statement or table over budget two days running, at most 3 a day.
	if cfg.PgBudgetReportHour >= 0 && cfg.PgBudgetReportHour < 24 && cfg.MetricsQueryURL != "" {
		budgets, err := pgbudget.ParseBudgets(cfg.PgBudgets)
		if err != nil {
			logger.Warn("postgres efficiency report: bad GARDENIO_PG_BUDGETS; using the defaults", "error", err)
		}
		go (&pgbudget.Runner{
			Query:   &pgbudget.VMClient{BaseURL: cfg.MetricsQueryURL, HTTP: &http.Client{Timeout: 30 * time.Second}},
			Store:   daemon,
			Budgets: budgets,
			Land:    cfg.PgBudgetLand,
			Epic:    cfg.PgBudgetEpic,
			Hour:    cfg.PgBudgetReportHour,
			Actor:   "gardenio-controller",
			Logger:  logger,
		}).RunLoop(ctx)
	} else {
		logger.Info("postgres efficiency report disabled (GARDENIO_PG_BUDGET_REPORT_HOUR unset)")
	}

	// Start the session reaper (gd-c6muILNedN). gb closes an agent's session
	// bead in-pod after coop exits, which a wall-clock end, a kill or a SIGKILL
	// mid-close never reaches; the reaper closes those from here, so open
	// session beads track live agents. Set GARDENIO_SESSION_REAPER_INTERVAL=0 to
	// disable.
	if cfg.SessionReaperInterval > 0 {
		go newSessionReaper(daemon, eventMetrics, cfg.SessionReaperInterval, logger).RunLoop(ctx)
	} else {
		logger.Info("session reaper disabled (GARDENIO_SESSION_REAPER_INTERVAL=0)")
	}

	// Start bridge fleet-token rotator (ka-0000002353). Defaulted OFF + dry-run
	// for safety; preprod opt-in flips DryRun=false after 24h dry-run verify,
	// prod is operator-gated via ka-0000005123.
	if cfg.BridgeTokenRotationEnabled {
		// Fleets in BridgeTokenRotationPrivilegedFleets are designated
		// privileged service-fleet identities — their rotated tokens may
		// bypass per-bead ownership for system-managed graph writes
		// (produces/triggered-by deps, mr_url back-fill). See ka-0000001946.
		privilegedFleets := make(map[string]bool, len(cfg.BridgeTokenRotationPrivilegedFleets))
		for _, name := range cfg.BridgeTokenRotationPrivilegedFleets {
			privilegedFleets[name] = true
		}
		rotatorFleets := make([]bridgetokenrotator.BridgeSpec, 0, len(cfg.BridgeTokenRotationFleets))
		for _, name := range cfg.BridgeTokenRotationFleets {
			rotatorFleets = append(rotatorFleets, bridgetokenrotator.BridgeSpec{
				Fleet:      name,
				Privileged: privilegedFleets[name],
			})
		}
		rotator, rerr := bridgetokenrotator.New(bridgetokenrotator.Config{
			Enabled:     true,
			DryRun:      cfg.BridgeTokenRotationDryRun,
			Interval:    cfg.BridgeTokenRotationInterval,
			Threshold:   cfg.BridgeTokenRotationThreshold,
			AccessTTL:   cfg.BridgeTokenRotationAccessTTL,
			RefreshTTL:  cfg.BridgeTokenRotationRefreshTTL,
			Namespace:   cfg.Namespace,
			SecretName:  cfg.BridgeTokenRotationSecret,
			FleetMapKey: cfg.BridgeTokenRotationFleetMapKey,
			Fleets:      rotatorFleets,
		}, machineDaemon, k8sClient, logger)
		if rerr != nil {
			logger.Error("bridge fleet-token rotator init failed", "error", rerr)
		} else {
			// Observability (ka-0000002353 plan item 3): the expiry/deadman/
			// rotation series are registered in main() on the controller's
			// promRegistry (the one :9092 serves) and passed in here, so
			// BridgeTokenRotationOverdue is fireable. rotatorMetrics is nil when
			// the rotator is disabled — SetMetrics is nil-safe and the alert's
			// absent() clause reports the rotator as off.
			rotator.SetMetrics(rotatorMetrics)
			go rotator.RunLoop(ctx)
		}
	}

	// Per-agent token orphan-sweep janitor: the reconciler safety net for
	// tokens the synchronous revoke-on-done path missed (dropped despawn,
	// controller restart, transient Secret-delete failure). Only active when
	// per-agent tokens are enabled. Records revocation_type=async_janitor.
	var tokJanitor *tokenJanitor
	if cfg.PerAgentTokens || ghApp != nil {
		tokJanitor = newTokenJanitor(daemon, k8sClient, cfg.Namespace, logger.With("component", "token-janitor"), tokMetrics)
		tokJanitor.beadsTokens = cfg.PerAgentTokens
		tokJanitor.github = ghApp
		tokJanitor.githubNamespaces = []string{cfg.Namespace, cfg.GVisorNamespace}
	}

	// Wake-scanner (persistent-agents Phase 2, task ka-0000004902): the
	// counterpart to the in-pod idle-TTL PARK path. It periodically scans CLOSED
	// agent beads carrying successor=same + next_respawn_at ≤ now and reopens each
	// (clearing the wake field idempotently + bumping the respawn budget) so the
	// reconciler recreates the pod under the same name and `claude --resume`s the
	// conversation. Nil in single-binary/dev setups without a daemon.
	wakeScanner := statusreporter.NewWakeScanner(daemon, logger)
	if wakeScanner != nil {
		logger.Info("wake scanner enabled")
	}

	go runPeriodicSync(ctx, logger, status, rec, daemon, cfg, syncInterval, secretRec, failover, muxClient, k8sClient, credNotifier, pvcCollector, pvCollector, staleReaper, snapshotTracker, accountRestarter, pods, microvmLifecycle, tokJanitor, wakeScanner)

	// Start prewarmed agent pool reconcile loop if enabled.
	if pool != nil {
		go pool.RunLoop(ctx)
	}

	// Start the warm runsc placeholder ("warm-B") pool reconcile loop if wired
	// (C2/C3, ka-0000005266). No-op when freeze/thaw is off or
	// WARM_PLACEHOLDER_POOL_ENABLED is off. Interval 0 uses the pool default.
	ft.startPlaceholderPool(ctx, 0)

	// Start the post-thaw credential refresher (bug ka-0000000518). No-op when
	// freeze/thaw is off or POST_THAW_TOKEN_REFRESH_ENABLED is off. Re-mints +
	// re-overlays each live thawed agent's SA/IRSA warm-copy token before its
	// ~1h TTL expires, so a long-lived thawed agent does not lose credentials.
	ft.startTokenRefreshLoop(ctx)

	// Phase 3b/D10b: the scheduler top loop has been removed. Gardeners now
	// fire from `type=trigger` mirror beads through the bridge TriggerEngine
	// in cmd/trigger-engine, which also owns the NATS subscriptions for
	// decision_resolved / bead_closed / mail_received events.

	// Internal event channel: events synthesised by long-running probes
	// (currently the stuck-agent heuristic in statusreporter.SyncAll) feed
	// into the same handleEvent path used for SSE-delivered events.
	// Buffered so a brief handler stall doesn't block SyncAll.
	internalEvents := make(chan subscriber.Event, 16)
	if cfg.StuckAgentTimeout > 0 {
		if hr, ok := status.(*statusreporter.HTTPReporter); ok {
			hr.SetStuckDetection(&internalStuckPublisher{
				ch:     internalEvents,
				logger: logger.With("component", "stuck-publisher"),
			}, cfg.StuckAgentTimeout, cfg.StuckLongLivedMultiplier)
			logger.Info("stuck-agent heuristic enabled",
				"threshold", cfg.StuckAgentTimeout,
				"long_lived_multiplier", cfg.StuckLongLivedMultiplier)
		} else {
			logger.Warn("stuck-agent heuristic configured but reporter is not *HTTPReporter; skipping")
		}
	} else {
		// Off by default (ka-0000000084). Log it so an operator debugging a
		// wedged agent knows nothing will auto-restart it, and so the disabled
		// state is never a silent surprise.
		logger.Info("stuck-agent heuristic DISABLED (no automatic restart of wedged agents)",
			"reason", "STUCK_AGENT_TIMEOUT=0; disabled pending a false-positive-free record",
			"enable_with", "STUCK_AGENT_TIMEOUT=15m")
	}

	logger.Info("controller ready, waiting for beads events",
		"sync_interval", syncInterval)

	dispatch := func(event subscriber.Event) {
		if err := handleEvent(ctx, logger, cfg, event, pods, status, k8sClient, daemon, machineDaemon, ft, microvmLifecycle, microvmSessionVendor, eventMetrics, resumeMetrics, freezeMetrics, spendReconciler, microvmLaunchMetrics, tokMetrics, ghApp); err != nil {
			logger.Error("failed to handle event", "type", event.Type, "agent", event.AgentName, "error", err)
		}
	}

	for {
		select {
		case event := <-internalEvents:
			dispatch(event)

		case event, ok := <-watcher.Events():
			if !ok {
				return nil // channel closed, watcher shut down
			}
			// Budget exhaustion triggers auth failover (separate from pod operations).
			if event.Type == subscriber.BudgetExceeded {
				if err := failover.Handle(ctx, event.Project, event.BeadID); err != nil {
					logger.Error("auth failover failed",
						"project", event.Project, "agent", event.AgentName, "error", err)
				}
				continue
			}
			// Account bead update — push credential change to coopmux and nudge
			// agents using this account to pick up the new credential.
			if event.Type == subscriber.AccountUpdated {
				accountName := event.Metadata["title"]
				if muxClient != nil {
					go syncSingleAccountToCoopmux(ctx, logger, event, k8sClient, cfg.Namespace, muxClient, credNotifier, cfg.LiteLLMURL)
				}
				// NOTE: rebootProjectsForAccount removed — coopmux now distributes
				// credentials (including litellm_key / ANTHROPIC_CUSTOM_HEADERS)
				// directly to sessions via the distributor, no pod restarts needed.
				// SSE-driven auth failover: when throttle_status changes to
				// "rejected" on an account bead, trigger failover for all
				// projects currently using that account. This supplements the
				// NATS probe path (probeEventListener) and handles cases where
				// throttle_status is set externally (e.g., E2E tests, manual
				// operator override) rather than by the rateLimitProbe.
				//
				// The "allowed" edge no longer schedules switchback: primary-account
				// switchback is abolished (Phase 2, ka-0000008240). A recovered
				// account simply rejoins the healthy spread pool when
				// SweepStaleExhausted clears its exhausted status; there is no
				// privileged primary to steer projects back onto.
				if accountName != "" && event.Metadata["throttle_status"] == "rejected" {
					go func(acct string) {
						projects, err := probeListener.findProjectsForAccount(ctx, acct)
						if err != nil {
							logger.Warn("sse failover: failed to find projects", "account", acct, "error", err)
							return
						}
						for _, project := range projects {
							logger.Info("sse failover: triggering for throttled account", "account", acct, "project", project)
							if err := failover.Handle(ctx, project, ""); err != nil {
								logger.Error("sse failover: failed", "account", acct, "project", project, "error", err)
							}
						}
					}(accountName)
				}
				continue
			}
			// Session bead close — metrics-only (gd-5MTDWjwNjm). Increment the
			// exit-code counter directly; this never drives a pod operation, so it
			// bypasses handleEvent/dispatch like the AccountUpdated/BudgetExceeded
			// cases above.
			if event.Type == subscriber.SessionDone {
				if eventMetrics != nil {
					exitCode, err := strconv.Atoi(event.Metadata["exit_code"])
					if err != nil {
						logger.Debug("session_done event with unparseable exit_code",
							"bead", event.BeadID, "exit_code", event.Metadata["exit_code"])
					} else {
						eventMetrics.RecordSessionExitCode(exitCode, event.Project, event.Role)
					}
				}
				continue
			}
			// Config:auth update — refresh project cache before handling so that
			// resolveProjectAccount sees the new account from the config:auth bead
			// rather than the stale value from the previous ticker cycle. Without
			// this, a chained failover (e.g. acme-team→gardenio→backup-acct)
			// causes the ConfigUpdated handler to use the intermediate account's key
			// (gardenio) for new pods instead of the final destination.
			// Also push the newly assigned account's credentials to coopmux.
			if event.Type == subscriber.ConfigUpdated && event.Metadata["title"] == "auth" {
				refreshProjectCache(ctx, logger, daemon, cfg)
				if muxClient != nil {
					go syncAccountsToCoopmux(ctx, logger, daemon, k8sClient, cfg.Namespace, muxClient, credNotifier, cfg.LiteLLMURL, "config_auth_updated")
				}
			}
			dispatch(event)

		case err := <-watcherDone:
			return fmt.Errorf("watcher stopped: %w", err)

		case <-ctx.Done():
			logger.Info("shutting down controller")
			// Bounded thaw drain (ka-0000001355 harden-thaw): a thaw's post-commit
			// critical section (barrier restore + rehydration + reap + Pod B
			// teardown) runs on a ctx DECOUPLED from this (leader-election) ctx, so
			// when SIGTERM / leader-loss cancels ctx those sections are still
			// running in their goroutines. Wait for them to finish before returning
			// (which, under leader election, leads to os.Exit) so a controller roll
			// does not abandon an in-flight restore mid-flight — the exact churn
			// confound this hardening closes. The k8s terminationGracePeriodSeconds
			// must exceed this budget or the kubelet SIGKILLs us anyway. No-op when
			// freeze/thaw is disabled (nil ft → drains instantly).
			if n := ft.inflightThawCount(); n > 0 {
				logger.Info("draining in-flight thaws before shutdown",
					"inflight", n, "budget", cfg.ThawDrainTimeout)
				// ctx is already cancelled here, so the drain runs on a fresh
				// detached ctx bounded by the drain budget.
				remaining := ft.drainThaws(context.WithoutCancel(ctx), cfg.ThawDrainTimeout, 0)
				if remaining > 0 {
					logger.Warn("thaw drain budget expired with thaws still in flight; some may be interrupted by process exit",
						"remaining", remaining)
				} else {
					logger.Info("all in-flight thaws drained cleanly before shutdown")
				}
			}
			return nil
		}
	}
}

// runPeriodicSync runs SyncAll, project cache refresh, and reconciliation at a regular interval.
func runPeriodicSync(ctx context.Context, logger *slog.Logger, status statusreporter.Reporter, rec *reconciler.Reconciler, daemon *beadsapi.Client, cfg *config.Config, interval time.Duration, secretRec *secretreconciler.Reconciler, failover *authFailover, muxClient *coopmux.Client, k8sClient kubernetes.Interface, credNotifier *credSyncNotifier, pvcCollector *pvcgc.Collector, pvCollector *pvgc.Collector, staleReaper *statusreporter.StaleReaper, snapshotTracker *healthsnapshot.Tracker, accountRestarter *accountRotationRestarter, pods podmanager.Manager, microvmLifecycle *microvm.Lifecycle, tokJanitor *tokenJanitor, wakeScanner *statusreporter.WakeScanner) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Token orphan-sweep counter. Run every 5 minutes (every 5th sync cycle at
	// the default 60s interval), matching the stale-reaper cadence.
	tokenSweepCounter := 0
	tokenSweepInterval := 0
	if tokJanitor != nil && interval > 0 {
		tokenSweepInterval = int((5 * time.Minute) / interval)
		if tokenSweepInterval < 1 {
			tokenSweepInterval = 1
		}
	}

	// Sync coopmux credentials every 5 minutes (every 5th sync cycle at
	// the default 60s interval) to catch K8s secret rotations.
	coopmuxSyncCounter := 0
	const coopmuxSyncInterval = 5
	var lastCoopmuxSync time.Time

	// PVC garbage collection counter. Run at PVCGCInterval (default 5m),
	// converted to sync cycles: e.g. 5m / 60s = every 5th cycle.
	pvcGCCounter := 0
	pvcGCInterval := 0
	if pvcCollector != nil && cfg.PVCGCInterval > 0 && interval > 0 {
		pvcGCInterval = int(cfg.PVCGCInterval / interval)
		if pvcGCInterval < 1 {
			pvcGCInterval = 1
		}
	}
	var lastPVCGCStats pvcgc.Stats

	// PV garbage collection counter. Run at PVGCInterval (default 5m),
	// converted to sync cycles.
	pvGCCounter := 0
	pvGCInterval := 0
	if pvCollector != nil && cfg.PVGCInterval > 0 && interval > 0 {
		pvGCInterval = int(cfg.PVGCInterval / interval)
		if pvGCInterval < 1 {
			pvGCInterval = 1
		}
	}
	var lastPVGCStats pvgc.Stats

	// Stale agent reaper counter. Run every 5 minutes (every 5th sync cycle
	// at the default 60s interval) to detect and terminate stale agents.
	staleReapCounter := 0
	staleReapInterval := 0
	if staleReaper != nil && interval > 0 {
		staleReapInterval = int((5 * time.Minute) / interval)
		if staleReapInterval < 1 {
			staleReapInterval = 1
		}
	}
	var lastStaleReapStats statusreporter.StaleReaperStats

	// Wake-scanner counter (persistent-agents Phase 2, ka-0000004902). Runs on
	// the same 1-minute base cadence as the reconciler — a due wake should fire
	// promptly, and the scan is a single server-filtered list over the small
	// parked-agent population, so it is cheap to run every cycle. No-op unless
	// the scanner is wired (daemon present).
	var lastWakeScanStats statusreporter.WakeScannerStats

	// lambda_microvm leak-GC counter (C5, ka-0000001837). Sweep leaked MicroVM
	// runtimes + per-run secrets every 5 minutes (every 5th sync cycle at the
	// default 60s interval), matching the other periodic sweeps — a leak burns
	// its full max-duration budget before AWS reaps it, but detection is not
	// latency-sensitive. No-op unless the MicroVM backend is available.
	microvmGCCounter := 0
	microvmGCInterval := 0
	if microvmLifecycle != nil && microvmLifecycle.Available() && interval > 0 {
		microvmGCInterval = int((5 * time.Minute) / interval)
		if microvmGCInterval < 1 {
			microvmGCInterval = 1
		}
	}

	// Stranded-account recovery sweep counter. The edge-triggered restarter
	// (accountRestarter.Reconcile) only fires on the active→exhausted edge; this
	// level-triggered sweep catches pods baked onto an account that was ALREADY
	// exhausted at spawn (whole-pool exhaustion, no fallback) once the project
	// pool later regains a healthy account (ka-0000005398). Run every 5 minutes
	// (every 5th sync cycle at the default 60s interval), matching the other
	// periodic sweeps — recovery is not latency-sensitive and the sweep lists
	// pods, so it should not run every cycle.
	strandedRecoveryCounter := 0
	strandedRecoveryInterval := 0
	if accountRestarter != nil && interval > 0 {
		strandedRecoveryInterval = int((5 * time.Minute) / interval)
		if strandedRecoveryInterval < 1 {
			strandedRecoveryInterval = 1
		}
	}

	// Run the exhausted-account recovery sweep at the configured interval.
	// exhaustedSweepInterval is the number of sync cycles between sweeps.
	// E.g. with a 60s sync interval and 5m cadence → every 5 cycles.
	//
	// Switchback is abolished (ka-0000008240), so this cadence drives ONLY
	// SweepStaleExhausted — the sole recovery path. It is configured directly by
	// AUTH_EXHAUSTED_SWEEP_INTERVAL (formerly derived from the removed
	// AUTH_SWITCHBACK_INTERVAL). We floor it to at least one cycle whenever the
	// sync loop is running so recovery can never silently stop even if the
	// interval is misconfigured to 0.
	exhaustedSweepCounter := 0
	exhaustedSweepInterval := 0
	if interval > 0 {
		if cfg.AuthExhaustedSweepInterval > 0 {
			exhaustedSweepInterval = int(cfg.AuthExhaustedSweepInterval / interval)
		}
		if exhaustedSweepInterval < 1 {
			exhaustedSweepInterval = 1
		}
	}

	for {
		select {
		case <-ticker.C:
			if err := status.SyncAll(ctx); err != nil {
				logger.Warn("periodic status sync failed", "error", err)
			}
			// Refresh project and role caches from daemon.
			refreshProjectCache(ctx, logger, daemon, cfg)
			refreshRoleCache(ctx, logger, daemon, cfg)
			// Refresh the per-account live-pod snapshot used by spawn-time spread
			// selection (ka-0000004861). Done after the account cache refresh so
			// the LiteLLM-key → account reverse index is current.
			refreshAccountLiveCounts(ctx, logger, pods, cfg.Namespace, cfg)
			// Reconcile ExternalSecrets from project/role bead secrets and
			// account-bead credential references.
			if secretRec != nil {
				cfg.RoleCacheMu.RLock()
				roleSnap := cfg.RoleCache
				cfg.RoleCacheMu.RUnlock()
				cfg.ProjectCacheMu.RLock()
				accountSnap := cfg.AccountCache
				cfg.ProjectCacheMu.RUnlock()
				if err := secretRec.Reconcile(ctx, cfg.ProjectCache, roleSnap, accountSnap); err != nil {
					logger.Warn("ExternalSecret reconciliation failed", "error", err)
				}
			}
			// Restart running agent pods whose assigned account just transitioned
			// to exhausted, so they re-credential against a healthy account
			// instead of looping on 429s (ka-0000007997). Fires only on the
			// active→exhausted edge; the reconciler recreates the deleted pods.
			if accountRestarter != nil {
				cfg.ProjectCacheMu.RLock()
				acctSnap := cfg.AccountCache
				cfg.ProjectCacheMu.RUnlock()
				// Operator kill switch (ACCOUNT_ROTATION_RESTART_ENABLED=false):
				// pod deletion destroys in-flight session context, so during
				// restart-storm incidents (ka-0000005463) the exhaustion-driven
				// restarts can be turned off without a code change.
				if cfg.AccountRotationRestartEnabled {
					accountRestarter.Reconcile(ctx, acctSnap)
				}
				// Also restart pods whose baked LiteLLM key was rotated out
				// from under them (vkey migration with no account status
				// change) — otherwise they 401-crashloop forever (ka-0000003704).
				// Gated separately (STALE_VKEY_RESTART_ENABLED): a rotated-out
				// key is terminal and normally must stay on even while the
				// exhaustion restarts above are disabled.
				if cfg.StaleVKeyRestartEnabled {
					accountRestarter.ReconcileStaleVKeys(ctx, acctSnap)
					// Mirror-image drift: a pod spawned KEYLESS whose account has
					// SINCE gained a litellm_key 401-loops on token_not_found_in_db
					// exactly like a rotated-out key, and no in-process coop switch
					// recovers it (the controller updates coopmux's store but never
					// Distributes a same-account key add). Gate it with the stale-vkey
					// restarts — same terminal 401 class (ka-0000004556).
					accountRestarter.ReconcileKeylessGainedKey(ctx, acctSnap)
				}
				// Periodically sweep for pods stranded on an account that was
				// ALREADY exhausted at spawn (whole-pool exhaustion with no
				// fallback) whose project pool has SINCE regained a healthy
				// account — the edge-triggered Reconcile above never saw an
				// active→exhausted transition for them, so they 429-loop forever
				// (ka-0000005398). The sweep itself takes ProjectCacheMu.RLock,
				// so call it without holding the lock here.
				if cfg.AccountRotationRestartEnabled {
					strandedRecoveryCounter++
					if strandedRecoveryInterval > 0 && strandedRecoveryCounter >= strandedRecoveryInterval {
						strandedRecoveryCounter = 0
						accountRestarter.ReconcileStrandedRecovered(ctx, cfg)
					}
				}
			}
			// Periodically sync account credentials to coopmux.
			if muxClient != nil {
				coopmuxSyncCounter++
				if coopmuxSyncCounter >= coopmuxSyncInterval {
					coopmuxSyncCounter = 0
					if syncAccountsToCoopmux(ctx, logger, daemon, k8sClient, cfg.Namespace, muxClient, credNotifier, cfg.LiteLLMURL, "periodic") {
						lastCoopmuxSync = time.Now()
					}
				}
			}
			// Run reconciler to converge desired vs actual state.
			if rec != nil {
				reconcileStart := time.Now()
				err := rec.Reconcile(ctx)
				snapshotTracker.RecordReconcile(reconcileStart, time.Now(), err)
				if err != nil {
					logger.Warn("periodic reconciliation failed", "error", err)
				}
			}
			// Recover exhausted accounts back into the healthy spread pool. The
			// sweep clears stale `status="exhausted"` on accounts whose rate-limit
			// probe recovered OR whose cooldown_until has elapsed (the primary-free
			// replacement for the old switchback maxFailoverDur auto-reset). This is
			// the SOLE recovery path now that switchback is abolished (Phase 2,
			// ka-0000008240); it is deliberately runtime-map-independent so it still
			// recovers accounts after a controller restart (see ka-0000002663).
			if failover != nil && exhaustedSweepInterval > 0 {
				exhaustedSweepCounter++
				if exhaustedSweepCounter >= exhaustedSweepInterval {
					exhaustedSweepCounter = 0
					failover.SweepStaleExhausted(ctx)
				}
			}
			// Periodically garbage-collect orphaned workspace PVCs.
			if pvcCollector != nil && pvcGCInterval > 0 {
				pvcGCCounter++
				if pvcGCCounter >= pvcGCInterval {
					pvcGCCounter = 0
					lastPVCGCStats = pvcCollector.Collect(ctx)
				}
			}
			// Periodically garbage-collect Released PVs (stuck CSI migration).
			if pvCollector != nil && pvGCInterval > 0 {
				pvGCCounter++
				if pvGCCounter >= pvGCInterval {
					pvGCCounter = 0
					lastPVGCStats = pvCollector.Collect(ctx)
				}
			}
			// Periodically reap stale agent pods.
			if staleReaper != nil && staleReapInterval > 0 {
				staleReapCounter++
				if staleReapCounter >= staleReapInterval {
					staleReapCounter = 0
					lastStaleReapStats = staleReaper.Reap(ctx)
					snapshotTracker.RecordReap(lastStaleReapStats.Checked, lastStaleReapStats.Reaped)
				}
			}
			// Revive parked persistent-agent identities whose wake is due
			// (persistent-agents Phase 2, ka-0000004902). Runs every cycle: a due
			// wake should fire without a multi-minute delay, and the scan is one
			// cheap server-filtered list over the small parked population.
			if wakeScanner != nil {
				lastWakeScanStats = wakeScanner.Scan(ctx)
			}
			// Periodically sweep leaked lambda_microvm runtimes + per-run secrets
			// (C5, ka-0000001837): any runtime/secret whose owning agent bead is no
			// longer open (crash between RunMicrovm and the bead-write, or a
			// Terminate that failed after the bead closed) is torn down. The live
			// set is derived from the current OPEN MicroVM agent beads.
			if microvmLifecycle != nil && microvmLifecycle.Available() && microvmGCInterval > 0 {
				microvmGCCounter++
				if microvmGCCounter >= microvmGCInterval {
					microvmGCCounter = 0
					runMicrovmLeakGC(ctx, logger, daemon, microvmLifecycle)
				}
			}
			// Periodically sweep orphaned per-agent token Secrets (the async
			// revocation safety net, revocation_type=async_janitor).
			if tokJanitor != nil && tokenSweepInterval > 0 {
				tokenSweepCounter++
				if tokenSweepCounter >= tokenSweepInterval {
					tokenSweepCounter = 0
					if st := tokJanitor.sweep(ctx); st.Revoked > 0 {
						logger.Info("token janitor swept orphaned agent tokens",
							"scanned", st.Scanned, "revoked", st.Revoked)
					}
				}
			}
			// Log metrics snapshot after each sync.
			m := status.Metrics()
			logArgs := []any{
				"reports_total", m.StatusReportsTotal,
				"report_errors", m.StatusReportErrors,
				"sync_runs", m.SyncAllRuns,
				"sync_errors", m.SyncAllErrors,
			}
			if !lastCoopmuxSync.IsZero() {
				logArgs = append(logArgs, "last_cred_sync", time.Since(lastCoopmuxSync).Truncate(time.Second).String())
			}
			if lastPVCGCStats.TotalPVCs > 0 || lastPVCGCStats.DeletedPVCs > 0 {
				logArgs = append(logArgs,
					"pvc_total", lastPVCGCStats.TotalPVCs,
					"pvc_orphans", lastPVCGCStats.OrphanPVCs,
					"pvc_deleted", lastPVCGCStats.DeletedPVCs,
				)
			}
			if lastPVGCStats.ReleasedPVs > 0 || lastPVGCStats.DeletedPVs > 0 {
				logArgs = append(logArgs,
					"pv_total", lastPVGCStats.TotalPVs,
					"pv_released", lastPVGCStats.ReleasedPVs,
					"pv_deleted", lastPVGCStats.DeletedPVs,
				)
			}
			if lastStaleReapStats.Reaped > 0 || lastStaleReapStats.OpenRunInstancesClosed > 0 {
				logArgs = append(logArgs,
					"stale_checked", lastStaleReapStats.Checked,
					"stale_reaped", lastStaleReapStats.Reaped,
					"open_run_instances_closed", lastStaleReapStats.OpenRunInstancesClosed,
				)
			}
			if lastWakeScanStats.Woken > 0 || lastWakeScanStats.BudgetExhausted > 0 {
				logArgs = append(logArgs,
					"wake_woken", lastWakeScanStats.Woken,
					"wake_budget_exhausted", lastWakeScanStats.BudgetExhausted,
				)
			}
			logger.Info("metrics", logArgs...)
		case <-ctx.Done():
			return
		}
	}
}
