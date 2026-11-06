// Command slack-bridge is a standalone service that bridges beads lifecycle
// events to Slack notifications and handles Slack interaction webhooks.
//
// It runs three subsystems:
//   - Decisions watcher: SSE subscription for decision beads → Slack notifications
//   - Mail watcher: SSE subscription for mail beads → agent nudges
//   - HTTP server: Slack interaction webhook handler (/slack/interactions)
//
// This service has ZERO K8s dependencies and can run as a lightweight sidecar
// or standalone container alongside the gardenio controller.
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
	_ "time/tzdata" // embeds tz database so the SLA nudge sweep's default America/Los_Angeles quiet-hours window resolves without a host tzdata package

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/covflush"
	"gardenio/controller/internal/metrics"
	"gardenio/controller/internal/oidcauth"
	"gardenio/controller/internal/tracing"
)

var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	defer covflush.Flush()
	cfg := parseConfig()

	logger := setupLogger(cfg.logLevel)
	logStartup(cfg, version, commit, logger)

	shutdownTracing, err := tracing.Init(context.Background(), logger, tracing.Options{
		ServiceName:    "gardenio-slack-bridge",
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

	// Machine principal (gd-Hc34EENbO9 component 4; see machine_principal.go):
	// the bridge's own Keycloak service account carries ONLY its background
	// writes (the config seed and the dm delivery writes), per call. The fleet
	// token above stays the client's credential for everything else, and there
	// is no operated-by relay: a person's Slack writes ride that person's own
	// token via account linking. Unset BEADS_OIDC_* keeps today's behavior.
	machineTS, machineOK, err := beadsapi.MachineTokenSourceFromEnv(context.Background(), logger,
		beadsapi.WithExpectedRoles("gardenio-machine-slack-config-seed", "gardenio-machine-slack-dm-delivery"))
	if err != nil {
		logger.Error("machine principal misconfigured", "error", err)
		os.Exit(1)
	}
	if machineOK {
		logger.Info("machine principal enabled", "client", os.Getenv("BEADS_OIDC_CLIENT_ID"))
	} else {
		logger.Info("machine principal disabled")
	}

	// Human-operator invariant (epic ka-0000004835, task ka-0000006293): the
	// client-level system-operator default is REMOVED. Previously the bridge
	// stamped a client-wide X-System-Operator default ("slack-bridge") so every
	// operator-less bridge write bound the `system:slack-bridge` sentinel row.
	// The operator directive (decision ka-0000006432) is to eliminate
	// system-operator sentinels: a missing human is a loud refusal, not a
	// silent fallback to a system identity.
	//
	// Attribution now flows entirely from per-call context:
	//   - Slack/GitLab/JIRA mention handlers already stamp the resolved human
	//     via WithOperator (X-Operated-By: slack:U…/oidc:…@…) per call.
	//   - Writes with a resolvable human (e.g. thread auto-close, which prefers
	//     the thread bead's own human operator) carry that human per call.
	//   - TRUE machine-origin bridge writes (startup EnsureConfigs, dashboard
	//     refresh, mail-watcher housekeeping) must carry an explicit per-call
	//     WithSystemOperator so the daemon binds the narrowly-scoped machine
	//     sink — NOT a client-wide default that silently absorbs writes that
	//     should have carried a human.
	//
	// The daemon-side resolver collapse (system:* -> one system:machine sink,
	// drop fleet terminal branch) lands in ka-0000006425; this change is
	// gardener-gated to merge alongside it so service writes don't hard-400
	// before the daemon can absorb machine-origin writes (gasbox RC3
	// fail-closed amplification, ka-0000005858).

	// Edge-auth HMAC key: when set, daemon calls triggered by a Slack
	// interaction sign an edge-auth envelope asserting the human operator,
	// so the daemon's ownership check sees AuthIdentity{Type:"edge",
	// Subject:"slack:U…"} instead of the bridge's fleet token. The key MUST
	// be the same secret configured on the daemon's edge-auth verifier
	// (DAEMON_HMAC_KEY). Empty = signing disabled (pre-rollout safe).
	// See ka-0000007637.
	if key := os.Getenv("SLACK_BRIDGE_EDGE_AUTH_HMAC_KEY"); key != "" {
		daemon.SetEdgeAuthHMACKey([]byte(key))
		logger.Info("edge-auth HMAC signing enabled for slack-bridge")
	}
	// The v2 (role-bearing, act-as-person) envelope signs with its own key
	// when set (DAEMON_HMAC_KEY_V2 on the daemon), so the v1 key alone cannot
	// mint a v2 admin envelope (gd-dMof1PW1oJ). Unset = v2 signs with the key
	// above, as before the split.
	if key := os.Getenv("SLACK_BRIDGE_EDGE_AUTH_HMAC_KEY_V2"); key != "" {
		daemon.SetEdgeAuthHMACKeyV2([]byte(key))
		logger.Info("edge-auth v2 HMAC key set for slack-bridge")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	// Register bead types, views, and context configs with the daemon (the
	// bridge-owned subset, as the machine principal, when it is enabled).
	seedConfigs(ctx, daemon, machineTS, logger)

	// State persistence for Slack message tracking.
	state, err := bridge.NewStateManager(cfg.statePath)
	if err != nil {
		logger.Error("failed to load state", "path", cfg.statePath, "error", err)
		os.Exit(1)
	}
	logger.Info("state manager loaded", "path", cfg.statePath)

	// Slack notifier (optional — decisions still tracked even without Slack).
	var notifier bridge.Notifier
	var bot *bridge.Bot
	mux := http.NewServeMux()

	// Build OIDC handle early so /readyz can be gated on it (see ka-0000004618 F1).
	// Discovery runs in the background bound to ctx; /readyz fails closed (503)
	// until it succeeds, then flips ready without a pod restart (ka-0000001839).
	oidcHandle := oidcauth.New(ctx, oidcauth.Config{
		IssuerURL: cfg.oidcIssuerURL,
		ClientID:  cfg.oidcClientID,
	}, logger)

	// Health endpoints — always available regardless of Slack config.
	// hookStreamPtr is populated below once HookStream is constructed; the
	// /healthz handler reads it lazily so the closure can be installed before
	// the NATS subscription exists.
	var hookStreamPtr atomic.Pointer[bridge.HookStream]
	registerHealthHandlers(mux, version, func() (bool, string) {
		if !oidcHandle.Ready() {
			return false, "oidc_provider_unavailable"
		}
		if bot != nil && !bot.IsConnected() {
			return false, "socket_mode_disconnected"
		}
		return true, ""
	}, func() time.Time {
		if hs := hookStreamPtr.Load(); hs != nil {
			return hs.LastEventAt()
		}
		return time.Time{}
	})

	// Unreleased changes API — same data as the /unreleased Slack command.
	// Build image tracking configs from the gardenio repo using the bridge's
	// own version as the deployed tag (all gardenio images share the calver tag).
	gitlabClient := buildGitLabClient(cfg, logger)
	imageConfigs := buildImageConfigs(cfg.projects, cfg.repos, version)
	userDir, escalationAPI := registerBridgeAPIs(
		mux, cfg, daemon, state, gitlabClient, imageConfigs, version, logger,
	)

	// Initialize Bouncer for IP whitelist management if configured.
	var bouncer *bridge.Bouncer
	if bouncerEnabled(cfg) {
		k8sCfg, err := rest.InClusterConfig()
		if err != nil {
			logger.Warn("bouncer: failed to get in-cluster config (gate disabled)", "error", err)
		} else {
			dynClient, err := dynamic.NewForConfig(k8sCfg)
			if err != nil {
				logger.Warn("bouncer: failed to create K8s dynamic client (gate disabled)", "error", err)
			} else {
				bouncer = bridge.NewBouncer(bridge.BouncerConfig{
					Client:          dynClient,
					Namespace:       cfg.bouncerNamespace,
					MiddlewareNames: cfg.bouncerMiddlewares,
					Logger:          logger,
				})
				logger.Info("bouncer: IP whitelist management enabled",
					"namespace", cfg.bouncerNamespace,
					"middlewares", cfg.bouncerMiddlewares)
			}
		}
	}

	// Build channel router if configured.
	router := buildRouter(cfg, logger)

	// Load bridge notification config from config beads (best-effort).
	notifConfig := loadNotificationConfig(ctx, daemon, logger)

	// Allow env var override for cost reporting suppression.
	notifConfig = applyCostSuppressionOverride(os.Getenv("SUPPRESS_COST_REPORTING"), notifConfig)

	// Load thread-cards runtime config (kill switch + project allow-list).
	threadCardsConfig := loadThreadCardsConfig(ctx, daemon, logger)

	// Initialize LLM name generator for /spawn (optional — disabled when no API key).
	nameGen := buildNameGenerator(cfg, logger)

	if shouldLogSlackAPIOverride(cfg) {
		logger.Info("SLACK_API_URL override active — Slack API calls will be directed to mock",
			"url", cfg.slackAPIURL)
	}

	// Initialize Keycloak role checker for mention permission gate (optional).
	keycloakChecker := buildKeycloakChecker(cfg, logger)

	if socketModeEnabled(cfg) {
		// Socket Mode: real-time WebSocket connection for events, interactions, slash commands.
		bot = bridge.NewBot(buildBotConfig(
			cfg, daemon, state, router, bouncer,
			gitlabClient, keycloakChecker, nameGen,
			imageConfigs, notifConfig, threadCardsConfig, version, logger,
		))
		notifier = bot
		// Wire the unified trigger:slack-reaction path. The reaction source
		// can only live here (not in the standalone trigger-engine) because it
		// needs the bot's reaction_added event stream + reacted-message text.
		// notify-action output is published as a trigger-notice bead, picked up
		// by this same bridge's notice watcher and posted to Slack.
		enableReactionTriggers(bot, daemon, logger)
		// The Commons per-member spawn rate (COMMONS_SPAWNS_PER_HOUR_PER_MEMBER).
		bot.SetCommonsSpawnRate(bridge.CommonsSpawnRate(os.Getenv))
		logger.Info("Slack Socket Mode bot enabled", "channel", cfg.slackChannel)
	} else if cfg.slackBotToken != "" {
		// Webhook fallback: raw HTTP Slack notifier with interaction webhook handler.
		// Fail-closed: webhook mode requires SLACK_SIGNING_SECRET to authenticate
		// incoming /slack/interactions POSTs. Without it any network-adjacent caller
		// could forge block_actions payloads (AUDIT-2026-04-18 §N2, ka-0000005450).
		if webhookModeRequiresSigningSecret(cfg) {
			logger.Error("SLACK_SIGNING_SECRET is required when running in webhook mode " +
				"(SLACK_BOT_TOKEN set, SLACK_APP_TOKEN unset). Set SLACK_APP_TOKEN to use Socket Mode " +
				"or provide SLACK_SIGNING_SECRET to verify webhook callbacks.")
			os.Exit(1)
		}
		slack := bridge.NewSlackNotifier(
			cfg.slackBotToken,
			cfg.slackSigningSecret,
			cfg.slackChannel,
			daemon,
			logger,
		)
		notifier = slack
		mux.HandleFunc("/slack/interactions", slack.HandleInteraction)
		logger.Info("Slack webhook notifier enabled", "channel", cfg.slackChannel)
	} else {
		logger.Warn("SLACK_BOT_TOKEN not set — running without Slack notifications")
	}

	// Person-bound Slack writes (optional, gd-7CDM9gdog0).
	wirePersonRoles(cfg, bot, logger)
	wireSlackLink(cfg, bot, daemon, mux, logger)

	// Register permission callback handler (channel server → Slack buttons).
	if bot != nil {
		permHandler := bridge.NewPermissionCallbackHandler(bot, logger)
		permHandler.SetSharedBearerAgents(cfg.sharedBearerAgents)
		permHandler.RegisterRoutes(mux)
	}

	// Register Slack thread API (read/reply endpoints for agents). The bot
	// (when present) is also passed as the AgentCardResolver so the
	// human-survey API can route surveys to the agent card's channel/thread
	// for agents with no Slack thread coords (ka-0000005949, ka-0000002021).
	if bot != nil {
		threadAPI := registerSlackClientAPIs(mux, bot.API(), daemon, bot, logger)
		// Group-DM (mpim) start endpoint. Requires the bot (Socket Mode) for
		// mpim open + agent binding, so it is only registered here — webhook
		// mode has no bot and cannot bind an agent to a group DM.
		bridge.NewGroupOpenAPI(bot, daemon).RegisterRoutes(mux)
		// Group-DM (mpim) invite endpoint. Slack has no in-place mpim invite, so
		// this re-opens a superset mpim and migrates the agent binding — same
		// Socket-Mode + mpim:write requirement as groups/open.
		bridge.NewGroupInviteAPI(bot, daemon).RegisterRoutes(mux)
		// gb slack route: which channel an agent's cards go to and which
		// routing rule chose it (gd-XahY2dfWYP). Read-only.
		bridge.NewRouteExplainAPI(bot).RegisterRoutes(mux)
		// The Commons' inbound call: a reply written in the Commons wakes the
		// thread's agent (slice 4c, inbound_reply.go).
		bridge.NewConversationInboundAPI(bot, logger).RegisterRoutes(mux)
		// gb say / gb thread: agents name a thread or channel, the Router
		// writes the bead and projects it (slice 5, api_conversation_say.go).
		convAPI := bridge.NewConversationAgentAPI(bot, threadAPI, logger)
		convAPI.SetSharedBearerAgents(cfg.sharedBearerAgents)
		convAPI.RegisterRoutes(mux)
	} else if cfg.slackBotToken != "" {
		// Webhook mode — no bot, so no card-location resolver. Surveys must
		// supply `channel` directly.
		registerSlackClientAPIs(mux, buildWebhookSlackClient(cfg), daemon, nil, logger)
	}

	// Always-on agent-feedback write path (ka-0000002210). Registered
	// unconditionally — it has no Slack dependency and must emit a stable
	// feedback bead regardless of the dark human-survey UX or Slack config.
	bridge.NewAgentFeedbackAPI(daemon, logger).RegisterRoutes(mux)

	// Wrap mux with OIDC authentication and per-route RBAC for /api/ endpoints.
	// When OIDC is not configured (OIDC_ISSUER_URL empty), the OIDC middleware
	// is a no-op and all requests pass through unchanged.
	oidcMW := oidcHandle.Middleware
	oidcConfigured := cfg.oidcIssuerURL != ""
	logAuthMode(cfg.agentToken, oidcConfigured, cfg.insecureOpenAPI, logger)
	if oidcConfigured && cfg.oidcCommonsOnly {
		logger.Info("slack-bridge OIDC scoped to the Commons inbound route (SLACK_BRIDGE_OIDC_COMMONS_ONLY); other /api/* web routes stay closed")
		oidcMW = oidcOnlyOnCommonsInbound(oidcMW)
	}
	// The conversation routes take an agent's identity from its per-agent
	// daemon token, verified by the daemon (gd-P5wjHDweih).
	agentTokens := bridge.NewAgentTokenVerifier(cfg.beadsHTTPAddr, nil)
	handler := bridgeRBAC(mux, oidcMW, cfg.agentToken, agentTokens, oidcConfigured, cfg.insecureOpenAPI, logger)

	// otelhttp wraps the whole bridge handler so each inbound webhook /
	// interaction request becomes a server span (and continues any incoming
	// trace context). No-op until tracing.Init installs a provider.
	handler = otelhttp.NewHandler(handler, "slack-bridge")

	// Start HTTP server (always — serves health endpoints + optional webhook handler).
	srv := buildHTTPServer(cfg.listenAddr, handler)
	go func() {
		logger.Info("starting HTTP server", "addr", cfg.listenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP server failed", "error", err)
		}
	}()

	// The land listener: the agent-token routes alone, for tenant land pods
	// (gd-pXik57wZA9).
	if cfg.landListenAddr != "" {
		landSrv := buildHTTPServer(cfg.landListenAddr, otelhttp.NewHandler(landListener(mux, agentTokens, logger), "slack-bridge-land"))
		go func() {
			logger.Info("starting land listener", "addr", cfg.landListenAddr)
			if err := landSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("land listener failed", "error", err)
			}
		}()
	}

	// Start metrics server on a separate internal-only port.
	metricsAddr := resolveMetricsAddr(os.Getenv("METRICS_LISTEN_ADDR"))
	metricsSrv := buildMetricsServer(metricsAddr)
	go func() {
		logger.Info("starting metrics server", "addr", metricsAddr)
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics server failed", "error", err)
		}
	}()

	// Start Socket Mode bot if configured (auto-reconnect with backoff).
	if bot != nil {
		go func() {
			backoff := time.Second
			const maxBackoff = 30 * time.Second
			for {
				// bot.Run never returns nil (socketmode.RunContext always reports why it
				// stopped), so only the "context still live" half of the check is real.
				if err := bot.Run(ctx); ctx.Err() == nil {
					logger.Error("Socket Mode bot stopped, reconnecting", "error", err, "backoff", backoff)
				}
				if ctx.Err() != nil {
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}
				backoff = min(backoff*2, maxBackoff)
			}
		}()
	}

	// Create event deduplicator for preventing duplicate Slack notifications.
	dedup := bridge.NewDedup(logger)

	// Create SSE event stream for decisions, mail, agents, and jacks watchers.
	sseStream := buildSSEStream(cfg, dedup, state, logger)

	// Cross-operator resolution notifier (ka-0000002131, operator ruling on
	// ka-0000004303): DMs the addressed operator when a decision or escalation
	// is resolved by a different identity. nil when there is no Slack bot.
	crossOperator := bot.CrossOperatorNotify(daemon, logger)
	if crossOperator != nil {
		go crossOperator.Run(ctx)
	}

	// Register decisions handler on the SSE stream.
	decisions := newDecisionsWatcher(daemon, notifier, crossOperator, logger)
	decisions.RegisterHandlers(sseStream)

	// Wire decision respawner to bot so modal submission nudge failures
	// can trigger agent respawn (same dedup logic as SSE watcher path).
	if bot != nil {
		bot.SetDecisionRespawner(decisions)
	}

	// Register escalation execution bridge on the SSE stream.
	// Watches for approved escalations and triggers local agent execution.
	var escalationUsers *bridge.UserDirectory
	if bot != nil {
		escalationUsers = bot.UserDirectory()
		// Wire user directory to the escalation API for authorization checks.
		escalationAPI.SetUserDirectory(escalationUsers)
	} else {
		// No bot → the userDir created above for escalationAPI is the only one.
		escalationUsers = userDir
	}
	// Wire SSE handlers so the directory sees user bead creates/updates
	// immediately rather than waiting up to 5 minutes for the next Refresh.
	// Escalation approval looks up the caller by email; a newly-provisioned
	// user bead must be resolvable within seconds (see ka-0000001175).
	escalationUsers.RegisterHandlers(sseStream)
	// Escalation-creation operator DM (ka-0000002669, operator ask 2026-08-26):
	// DMs the human named in operated_by the moment an escalation is filed for
	// them. The existing card routes by requesting agent, so it reaches that
	// human only if they watch the channel. nil when there is no Slack bot.
	escalationOperatorDM := bot.EscalationOperatorDM(daemon, logger)
	if escalationOperatorDM != nil {
		go escalationOperatorDM.Run(ctx)
	}
	escalations := newEscalationsWatcher(daemon, escalationUsers, notifier, bot, crossOperator, escalationOperatorDM, logger)
	escalations.RegisterHandlers(sseStream)

	// Tenant support (Lands Track B §6.1, gd-UBeJ3kVicQ): a post in a tenant
	// land's #support files a support bead in that land and DMs
	// LANDS_SUPPORT_OPERATOR; without a DM, a pointer with no tenant text goes
	// to LANDS_SUPPORT_POINTER_CHANNEL (else SLACK_CHANNEL). The sweep re-tells
	// requests the operator was not told of.
	landsSupport := bot.LandsSupport(daemon, os.Getenv("LANDS_SUPPORT_OPERATOR"),
		sanitizeChannelID("LANDS_SUPPORT_POINTER_CHANNEL", os.Getenv("LANDS_SUPPORT_POINTER_CHANNEL")), logger)
	landsSupport.RegisterHandlers(sseStream)
	go landsSupport.Run(ctx)

	// Engine-owned escalation re-notification cadence (ka-0000004385):
	// periodically stamps pingback requests on stalled open escalations
	// (12h/24h/48h backoff then doubling; queue alert past 48h). The stamps
	// flow back through the SSE stream to the escalations watcher above,
	// which does the Slack posting. ESCALATION_PINGBACK_SWEEP_INTERVAL=off
	// disables the loop.
	if !cfg.escalationPingbackDisabled {
		pingbackLoop := bridge.NewEscalationPingbackLoop(bridge.EscalationPingbackLoopConfig{
			Daemon:   daemon,
			Logger:   logger,
			Interval: cfg.escalationPingbackInterval,
		})
		go pingbackLoop.Run(ctx)
	}

	// Consolidated staleness digest (ka-0000006001): OFF by default. When opted
	// in, posts ONE message per sweep to the operator queue channel listing every
	// stale open P0/P1 escalation (P0>2h, P1>8h) — the low-threshold,
	// scannable-snapshot complement to the per-item pingback (12h+) and queue
	// alert (48h). Requires a live bot to post; a nil *Bot in a non-nil
	// interface would defeat the constructor's nil check, so guard here.
	if cfg.escalationStaleDigestEnabled && bot != nil {
		digestLoop := bridge.NewEscalationStalenessDigestLoop(bridge.EscalationStalenessDigestConfig{
			Daemon:   daemon,
			Notifier: bot,
			Logger:   logger,
			Interval: cfg.escalationStaleDigestInterval,
		})
		go digestLoop.Run(ctx)
	}

	// SLA-by-priority nudge sweep (gd-Jc3NHOcgDj): periodically walks open
	// escalations and decisions and drives the 3-rung SLA ladder (reflag,
	// operator-channel repeat, overdue) per the event-gardeners phase-2 spec.
	// OFF by default. A nil bot still drives the field/label side effects
	// (rung1 both types, rung2 escalation) via the SSE-watcher reuse paths
	// documented in sla_nudge_sweep.go; only decision-rung2 and rung3 direct
	// Slack posts are skipped without a bot.
	if cfg.slaNudgeSweepEnabled {
		var notifier bridge.SLANudgeNotifier
		if bot != nil {
			notifier = bot
		}
		slaSweep := bridge.NewSLANudgeSweep(bridge.SLANudgeSweepConfig{
			Daemon:   daemon,
			Notifier: notifier,
			Logger:   logger,
			Interval: cfg.slaNudgeSweepInterval,
		})
		go slaSweep.Run(ctx)
	}

	// Register mail handler on the SSE stream.
	mail := newMailWatcher(daemon, logger)
	mail.RegisterHandlers(sseStream)

	// Register agents watcher for crash notifications. The daemon is passed
	// so a successful slack-thread agent close propagates to its parent
	// thread task after the configured grace window (ka-0000003222).
	agents, threadAutoCloser := newAgentsWatcher(bot, daemon, cfg.threadTaskAutoCloseWindow, cfg.threadTaskSweepInterval, cfg.threadTaskSpawnStallGrace, logger)
	agents.RegisterHandlers(sseStream)
	// Sweep the dedup map so it cannot leak for the process lifetime (ka-0000008059).
	go agents.Run(ctx)

	// Durable backfill/reconcile sweep: periodically re-scan open slack-thread
	// task beads and close orphans whose agents are all terminal, independent
	// of the in-memory MaybeSchedule timer (which is lost on restart) and of
	// whether any agent stamped slack_thread_bead_id (ka-0000001874).
	if threadAutoCloser != nil {
		go threadAutoCloser.Run(ctx)
	}

	// Register jacks watcher for jack lifecycle notifications.
	jacks := newJacksWatcher(bot, logger)
	jacks.RegisterHandlers(sseStream)
	// Sweep the dedup maps so they cannot leak for the process lifetime
	// (ka-0000008059) — mirrors the sibling agents/beadActivity watchers.
	go jacks.Run(ctx)

	// Register chat/squawk/comment-forward/DM-delivery handlers on the SSE
	// stream (no-op when bot is nil).
	registerBotSSEHandlers(ctx, bot, daemon, machineTS, state, sseStream, logger)

	if bot != nil {
		// acme-scif gate-alert watcher (ka-0000002770). Posts a pointer-only
		// alert to the gate-alert channel when a acme-scif human gate opens
		// (Gate-2: a finished run awaiting release, signalled by
		// release_status=pending on the investigation-request bead). No-op when
		// the channel is unset. The Gate-1 alert (ka-0000006007) reuses this
		// same channel/client.
		scifGateAlert := bridge.NewScifGateAlert(bridge.ScifGateAlertConfig{
			API:            bot.API(),
			Channel:        cfg.scifGateAlertChannel,
			PortalURL:      cfg.scifGateAlertPortalURL,
			BeadsViewerURL: cfg.beadsViewerURL,
			ThemeFor:       bot.ThemeResolver(),
			Logger:         logger,
		})
		scifGateAlert.RegisterHandlers(sseStream)
	}

	// Start live agent dashboard if configured.
	if dashboardEnabled(cfg, bot) {
		dashCfg := buildDashboardConfig(cfg, notifConfig)
		dash := bridge.NewDashboard(bot.API(), daemon, state, logger, dashCfg)
		dash.RegisterHandlers(sseStream)
		go dash.Run(ctx)
		logger.Info("dashboard enabled", "channel", dashCfg.ChannelID, "interval", dashCfg.Interval)
	}

	// Register claimed bead update watcher — nudges agents when their claimed work is updated.
	claimed := newClaimedWatcher(daemon, logger)
	claimed.RegisterHandlers(sseStream)

	// Register advice nudge watcher — nudges agents when advice beads they subscribe to are updated.
	adviceNudge := newAdviceNudgeWatcher(daemon, logger)
	adviceNudge.RegisterHandlers(sseStream)

	// Register bead activity watcher — posts notifications in agent threads when
	// agents create, claim, or close beads.
	beadActivity := newBeadActivityWatcher(bot, logger)
	beadActivity.RegisterHandlers(sseStream)
	// Sweep the dedup map so it cannot leak for the process lifetime (ka-0000008059).
	go beadActivity.Run(ctx)

	// Start agent stream manager for live tool-use streaming in assistant threads.
	var streamMgr *bridge.AgentStreamManager
	if bot != nil {
		streamMgr = bridge.NewAgentStreamManager(bridge.NewSlackStreamAdapter(bot.API()), logger)
		bot.SetStreamManager(streamMgr)
		decisions.SetStreamManager(streamMgr)
		// Wire the shared resume-latency histogram so a cold thread-agent resume
		// (respawnThreadAgent) tees a gardenio_resume_latency_seconds{path=cold}
		// sample at first proof-of-life. The bridge owns the ONLY measurement of
		// the common non-runsc resume path; the controller's thaw-only call site
		// never covered it, which is why the metric emitted zero prod samples
		// (task ka-0000004119 / C7). Registered on the default registry so the
		// slack-bridge /metrics endpoint serves it into the same VictoriaMetrics
		// the controller's thaw-path series land in. env matches the controller's
		// GARDENIO_ENV so both processes' series share the dashboard $env filter.
		resumeMetrics := metrics.NewResumeMetrics(os.Getenv("GARDENIO_ENV"))
		if err := resumeMetrics.Register(prometheus.DefaultRegisterer); err != nil {
			logger.Warn("failed to register resume-latency metric; cold-resume samples will not emit", "error", err)
		} else {
			streamMgr.SetResumeMetrics(resumeMetrics)
		}
		// Wire the model-fallback flagger: on a usage turn whose SERVED model
		// diverges from the pod's baked EXPECTED model, post a Slack flag (agent
		// thread + ops channel) and write a durable type=rating record on the
		// shared surveys/ratings surface (ka-0000003127).
		if flagger := bridge.NewModelFallbackFlagger(bridge.ModelFallbackConfig{
			API:            bot.API(),
			Daemon:         daemon,
			OpsChannel:     cfg.modelFallbackChannel,
			BeadsViewerURL: cfg.beadsViewerURL,
			ThemeFor:       bot.ThemeResolver(),
			Logger:         logger,
		}); flagger != nil {
			streamMgr.SetModelFallbackFlagger(flagger)
			logger.Info("model-fallback flagger started", "ops_channel", cfg.modelFallbackChannel)
		}
		// Wire the refusal logger: on a coop-classified refusal turn (Anthropic
		// stop_reason=refusal), persist a durable type=rating record on the shared
		// surveys/ratings surface (source=refusal) and post a Slack flag (agent
		// thread + ops channel). Reuses the model-fallback ops channel so refusals
		// ride the same ops route without new config (ka-0000006122).
		if refusalLog := bridge.NewRefusalLogger(bridge.RefusalLogConfig{
			API:            bot.API(),
			Daemon:         daemon,
			OpsChannel:     cfg.modelFallbackChannel,
			BeadsViewerURL: cfg.beadsViewerURL,
			ThemeFor:       bot.ThemeResolver(),
			Logger:         logger,
		}); refusalLog != nil {
			streamMgr.SetRefusalLogger(refusalLog)
			logger.Info("refusal logger started", "ops_channel", cfg.modelFallbackChannel)
		}
		go streamMgr.Run(ctx)
		logger.Info("agent stream manager started")

		// Subscribe to NATS hook events for streaming status updates.
		hookStream := bridge.NewHookStream(buildHookStreamConfig(cfg, streamMgr, logger))
		if hookStream != nil {
			if err := hookStream.Start(); err != nil {
				logger.Warn("hookstream: failed to connect to NATS (streaming disabled)", "error", err)
			} else {
				hookStreamPtr.Store(hookStream)
				defer hookStream.Close()
			}
		}

		// Subscribe to NATS rotation events for account rotation Slack notifications.
		rotation := bridge.NewRotation(buildRotationConfig(cfg, bot, logger))
		if rotation != nil {
			if err := rotation.Start(); err != nil {
				logger.Warn("rotation: failed to connect to NATS (rotation notifications disabled)", "error", err)
			} else {
				defer rotation.Close()
			}
		}
	}

	// Start periodic dedup map cleanup to prevent unbounded growth.
	go dedup.StartCleanup(ctx)

	// Catch-up: notify pending decisions that may have been missed during downtime.
	// Run before SSE stream starts to pre-populate dedup map.
	go dedup.CatchUpDecisions(ctx, daemon, notifier, logger)

	// Catch-up: pre-populate dedup for active agent beads so SSE replay
	// doesn't re-fire created events (prevents state flicker on restart).
	dedup.CatchUpAgents(ctx, daemon, logger)

	// Reconcile thread→agent mappings from active agent beads so thread
	// replies reach agents that were spawned by a previous bridge instance.
	if bot != nil {
		bot.ReconcileThreadAgents(ctx)
	}

	// Start the shared SSE stream (delivers events to all watchers).
	go func() {
		if err := sseStream.Start(ctx); err != nil && ctx.Err() == nil {
			logger.Error("SSE event stream stopped", "error", err)
		}
	}()

	logReady(bot != nil, bot == nil && notifier != nil, logger)

	// Block until shutdown signal.
	<-ctx.Done()
	logger.Info("shutting down slack-bridge")

	// Graceful HTTP server shutdown.
	gracefulShutdown(srv, logger)
}

// config holds parsed environment configuration for the slack-bridge service.
type config struct {
	beadsHTTPAddr      string
	slackBotToken      string
	slackAppToken      string
	slackSigningSecret string
	slackChannel       string
	listenAddr         string
	// landListenAddr (SLACK_BRIDGE_LAND_LISTEN_ADDR) is the land listener's
	// address, the port tenant land pods reach (landListener); "" for none.
	landListenAddr string
	logLevel       string
	statePath      string
	debug          bool

	// Threading
	threadingMode string
	// decisionDMMode: "" (default, DM-first decisions) or "off" (kill switch).
	decisionDMMode string

	// Thread cards: SLACK_THREAD_CARDS env mode ("off" / "owned" / "all",
	// default "off"). Deploy-time master switch for posting agent cards in
	// user-owned Slack threads.
	threadCardsMode string

	// Dashboard
	dashboardEnabled  bool
	dashboardChannel  string
	dashboardInterval time.Duration

	// GitLab /unreleased (primary path)
	gitlabToken   string
	gitlabBaseURL string
	projects      []bridge.ProjectRef
	controllerURL string

	// GitHub /unreleased (deprecated fallback)
	githubToken string
	repos       []bridge.RepoRef

	// Coopmux terminal links
	coopmuxPublicURL string

	// Laptop runner registration URL quoted in /runner invite DMs
	runnerPublicURL string

	// Beads web UI base URL for escalation links
	beadsWebURL string

	// Beads viewer UI base URL for read-only bead links.
	// Distinct from beadsWebURL — see bridge.BotConfig.BeadsViewerURL.
	beadsViewerURL string

	// JIRA base URL + optional allowlist of project keys for linkifying
	// JIRA issue refs (e.g. PIH-1234) in outbound Slack messages.
	jiraBaseURL     string
	jiraProjectKeys []string

	// Default DM project for multi-project setups
	defaultDMProject string

	// Auto-spawn a gardener when the bot is invited to a (private) channel.
	gardenerOnInvite bool

	// Mention permission gate
	mentionGateEnabled bool
	// Slash command permission gate
	slashGateEnabled bool

	// Bare ka-XXX preview (SLACK_BARE_ID_PREVIEW_ENABLED, Phase 2 stretch).
	// Default false: when off the message scan never runs — zero behavior
	// change.
	bareIDPreviewEnabled bool

	// Bead-link hydration (SLACK_LINK_HYDRATE_ENABLED). When true, bare bead
	// ids in outgoing Slack text become glyph + short-name links and messages
	// naming several beads get a References footer. Default false: links keep
	// the bare id as their label.
	linkHydrateEnabled bool

	// Link-unfurl per-channel allowlist (SLACK_LINK_UNFURL_CHANNELS). A pasted
	// gardenio viewer link is unfurled into a rich preview card only in these
	// channels. Empty (default) disables unfurling everywhere.
	linkUnfurlChannels []string

	// acme-scif gate-alert channel (ka-0000002770, epic ka-0000007110). The
	// single source of truth for the dedicated #acme-scif-gate-alerts channel
	// id, used by the pending/open gate watcher (ScifGateAlert). Empty disables
	// gate-alert posting. scifGateAlertPortalURL is the review surface the alert
	// links to (empty omits the link; the scif-portal that used to serve it was
	// deleted 2026-10-31, gd-Y4ZCGr2m2t). slackBridge.scifGateAlert.channel /
	// SCIF_GATE_ALERT_CHANNEL.
	scifGateAlertChannel   string
	scifGateAlertPortalURL string

	// modelFallbackChannel is an ops channel that receives a flag whenever an
	// agent turn is served by a model other than the one baked into its pod
	// (a silent model fallback — ka-0000003127). Empty posts the flag only to
	// the agent's own thread. SLACK_MODEL_FALLBACK_CHANNEL.
	modelFallbackChannel string

	// Escalation re-notification cadence (ka-0000004385). Interval is the
	// sweep frequency of the auto-pingback loop (0 → package default 15m);
	// disabled kills the loop entirely (ESCALATION_PINGBACK_SWEEP_INTERVAL=off).
	// Queue channel/mention route the 48h+ stale-escalation alerts.
	escalationPingbackInterval time.Duration
	escalationPingbackDisabled bool
	escalationQueueChannel     string
	escalationQueueMention     string

	// Consolidated staleness digest (ka-0000006001). OFF by default (opt-in
	// first release): ESCALATION_STALE_DIGEST="on" (or a duration like "30m")
	// enables it; posts ONE message per sweep listing stale P0/P1 escalations to
	// the operator queue channel.
	escalationStaleDigestEnabled  bool
	escalationStaleDigestInterval time.Duration

	// SLA-by-priority nudge sweep (gd-Jc3NHOcgDj). OFF by default (opt-in
	// first release): SLA_NUDGE_SWEEP="on" (or a duration like "5m") enables
	// it at the package-default (5m) or given interval.
	slaNudgeSweepEnabled  bool
	slaNudgeSweepInterval time.Duration

	// Channel router
	routerConfig *bridge.RouterConfig

	// Gate (IP whitelist management)
	bouncerEnabled     bool
	bouncerNamespace   string
	bouncerMiddlewares []string

	// NATS for hook event streaming
	natsURL   string
	natsToken string
	natsUser  string

	// LLM-powered name generation for /spawn (optional)
	nameGenAPIKey  string
	nameGenBaseURL string
	nameGenModel   string

	// Keycloak role check for mention gate
	keycloakURL          string
	keycloakRealm        string
	keycloakClientID     string
	keycloakClientSecret string
	keycloakMentionRole  string
	// actAsPerson (SLACK_BRIDGE_ACT_AS_PERSON): Slack interactions are done as
	// the person the Slack user maps to, with their Keycloak roles
	// (gd-7CDM9gdog0). Off by default.
	actAsPerson bool

	// Slack account linking (gd-OZJ6H2S3xx, SLACK_LINK_*): human Slack writes
	// carry the linked person's own Keycloak token. Off by default.
	slackLinkEnabled      bool
	slackLinkPublicURL    string // SLACK_LINK_PUBLIC_URL: https origin of /slack/link/
	slackLinkIssuerURL    string // SLACK_LINK_ISSUER_URL: Keycloak realm issuer
	slackLinkClientID     string // SLACK_LINK_CLIENT_ID (gardenio-slack-link)
	slackLinkClientSecret string // SLACK_LINK_CLIENT_SECRET
	slackLinkKey          string // SLACK_LINK_KEY: base64 32-byte AEAD key
	slackLinkStorePath    string // SLACK_LINK_STORE_PATH (default next to STATE_PATH)

	// OIDC authentication for web UI API endpoints
	oidcIssuerURL string
	oidcClientID  string

	// Shared bearer token required on agent-facing endpoints
	// (/api/slack/*, /api/permission/*). Env: SLACK_BRIDGE_AGENT_TOKEN.
	// When empty, agent-facing endpoints require OIDC admin role instead.
	agentToken string
	// sharedBearerAgents (SLACK_BRIDGE_SHARED_BEARER_AGENTS, default on) lets
	// agentToken still name an agent on the conversation routes and the
	// permission callback, for operator lands only; "0" or "false" refuses it there, so an agent must send its
	// own daemon token (Lands 10.3, gd-P5wjHDweih).
	sharedBearerAgents bool
	// insecureOpenAPI (SLACK_BRIDGE_INSECURE_OPEN_API=1) is the dev-only
	// opt-out that leaves /api/* open when no OIDC is configured.
	insecureOpenAPI bool
	// oidcCommonsOnly (SLACK_BRIDGE_OIDC_COMMONS_ONLY=true) verifies OIDC
	// bearers on the Commons inbound route alone, so setting OIDC_ISSUER_URL
	// for the /garden card actions leaves every other /api/* web route
	// closed (gd-na5unRTCaP).
	oidcCommonsOnly bool

	// CORS allowed origins for SSE endpoints
	corsAllowedOrigins []string

	// Slack API URL override (for mock Slack in E2E tests)
	slackAPIURL string

	// Grace window between a slack-thread agent close and the parent
	// thread task being idle-hinted (ka-0000003222). Zero falls back to
	// bridge.DefaultThreadTaskCloseWindow (30m).
	threadTaskAutoCloseWindow time.Duration

	// Interval for the durable backfill/reconcile sweep that idle-hints
	// orphaned open slack-thread task beads (ka-0000001874). Zero falls
	// back to bridge.DefaultThreadTaskSweepInterval (15m).
	threadTaskSweepInterval time.Duration

	// Spawn-stall grace (1b-9, ka-0000007017): how long a non-terminal wish
	// member may sit without its updated_at advancing before the backfill sweep
	// stops treating it as a live suppressor. Zero falls back to
	// bridge.DefaultSpawnStallGrace (30m). Wired from
	// SLACK_THREAD_TASK_SPAWN_STALL_GRACE.
	threadTaskSpawnStallGrace time.Duration
}

func parseConfig() *config {
	dashInterval := 15 * time.Second
	if v := os.Getenv("SLACK_DASHBOARD_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			dashInterval = d
		}
	}

	// Grace window before a slack-thread parent task auto-closes after the
	// agent finishes. Override with SLACK_THREAD_TASK_AUTOCLOSE_WINDOW
	// (e.g. "15m", "1h"); empty/invalid falls back to the package default.
	var threadTaskAutoCloseWindow time.Duration
	if v := os.Getenv("SLACK_THREAD_TASK_AUTOCLOSE_WINDOW"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			threadTaskAutoCloseWindow = d
		} else {
			fmt.Fprintf(os.Stderr, "WARNING: invalid SLACK_THREAD_TASK_AUTOCLOSE_WINDOW %q: %v\n", v, err)
		}
	}

	// Interval for the durable backfill sweep that reconciles orphaned open
	// slack-thread task beads. Override with SLACK_THREAD_TASK_SWEEP_INTERVAL
	// (e.g. "15m", "1h"); empty/invalid falls back to the package default.
	var threadTaskSweepInterval time.Duration
	if v := os.Getenv("SLACK_THREAD_TASK_SWEEP_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			threadTaskSweepInterval = d
		} else {
			fmt.Fprintf(os.Stderr, "WARNING: invalid SLACK_THREAD_TASK_SWEEP_INTERVAL %q: %v\n", v, err)
		}
	}

	// Spawn-stall grace (1b-9, ka-0000007017): how long a non-terminal wish
	// member may sit without its updated_at advancing before the backfill sweep
	// stops treating it as a live suppressor. Override with
	// SLACK_THREAD_TASK_SPAWN_STALL_GRACE (e.g. "30m", "1h"); empty/invalid
	// falls back to the package default (bridge.DefaultSpawnStallGrace).
	var threadTaskSpawnStallGrace time.Duration
	if v := os.Getenv("SLACK_THREAD_TASK_SPAWN_STALL_GRACE"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			threadTaskSpawnStallGrace = d
		} else {
			fmt.Fprintf(os.Stderr, "WARNING: invalid SLACK_THREAD_TASK_SPAWN_STALL_GRACE %q: %v\n", v, err)
		}
	}

	dashChannel := sanitizeChannelID("SLACK_DASHBOARD_CHANNEL", os.Getenv("SLACK_DASHBOARD_CHANNEL"))
	dashEnabled := os.Getenv("SLACK_DASHBOARD") == "true"
	// Auto-enable if channel is set.
	if dashChannel != "" && os.Getenv("SLACK_DASHBOARD") == "" {
		dashEnabled = true
	}

	// SLACK_DECISION_DM=off restores channel-only decision posting. Default is
	// DM-first (operator ruling 2026-08-27) — see bridge/bot_decisions_dm.go.
	decisionDMMode := strings.ToLower(strings.TrimSpace(os.Getenv("SLACK_DECISION_DM")))

	threadingMode := os.Getenv("SLACK_THREADING_MODE")
	if threadingMode == "" {
		threadingMode = "agent"
	}

	// Thread-cards deploy-time master switch (default off — today's behavior).
	// "owned" posts cards only in user-owned threads; "all" in every thread
	// spawn. Anything else (including empty) is treated as "off".
	threadCardsMode := os.Getenv("SLACK_THREAD_CARDS")
	switch threadCardsMode {
	case "owned", "all", "off":
		// valid
	case "":
		threadCardsMode = "off"
	default:
		fmt.Fprintf(os.Stderr, "WARNING: invalid SLACK_THREAD_CARDS %q (want off|owned|all): defaulting to off\n", threadCardsMode)
		threadCardsMode = "off"
	}

	// GitLab project list for /unreleased — supports multi-segment paths.
	projects := parseProjectList(os.Getenv("UNRELEASED_PROJECTS"))
	if len(projects) == 0 {
		// Fall back to GITLAB_PROJECT as a single project.
		if glProj := envOrDefault("GITLAB_PROJECT", "example-org/gardenio"); glProj != "" {
			projects = parseProjectList(glProj)
		}
	}

	// Legacy GitHub repo list (deprecated — kept for backward compatibility).
	repos := parseRepoList(os.Getenv("UNRELEASED_REPOS"))

	var routerCfg *bridge.RouterConfig
	if v := os.Getenv("SLACK_ROUTER_CONFIG"); v != "" {
		routerCfg = &bridge.RouterConfig{}
		if err := json.Unmarshal([]byte(v), routerCfg); err != nil {
			fmt.Fprintf(os.Stderr, "WARNING: invalid SLACK_ROUTER_CONFIG JSON: %v\n", err)
			routerCfg = nil
		}
	}

	// Escalation auto-pingback sweep (ka-0000004385). Override with
	// ESCALATION_PINGBACK_SWEEP_INTERVAL (e.g. "30m"); "off" or "0"
	// disables the loop; empty/invalid falls back to the package default.
	var escalationPingbackInterval time.Duration
	escalationPingbackDisabled := false
	switch v := os.Getenv("ESCALATION_PINGBACK_SWEEP_INTERVAL"); v {
	case "", "default":
		// package default
	case "off", "0", "disabled":
		escalationPingbackDisabled = true
	default:
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			escalationPingbackInterval = d
		} else {
			fmt.Fprintf(os.Stderr, "WARNING: invalid ESCALATION_PINGBACK_SWEEP_INTERVAL %q: %v\n", v, err)
		}
	}

	// Consolidated staleness digest (ka-0000006001). OFF unless explicitly
	// opted in via ESCALATION_STALE_DIGEST: "on"/"true" enables it at the
	// package-default sweep interval; a duration ("30m") enables it at that
	// interval; empty/"off"/"0" leaves it disabled.
	escalationStaleDigestEnabled := false
	var escalationStaleDigestInterval time.Duration
	switch v := os.Getenv("ESCALATION_STALE_DIGEST"); v {
	case "", "off", "0", "disabled", "false":
		// disabled (default)
	case "on", "true", "default":
		escalationStaleDigestEnabled = true
	default:
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			escalationStaleDigestEnabled = true
			escalationStaleDigestInterval = d
		} else {
			fmt.Fprintf(os.Stderr, "WARNING: invalid ESCALATION_STALE_DIGEST %q: %v\n", v, err)
		}
	}

	// SLA-by-priority nudge sweep (gd-Jc3NHOcgDj). OFF unless explicitly
	// opted in via SLA_NUDGE_SWEEP: "on"/"true" enables it at the
	// package-default sweep interval (5m); a duration ("2m") enables it at
	// that interval; empty/"off"/"0" leaves it disabled.
	slaNudgeSweepEnabled := false
	var slaNudgeSweepInterval time.Duration
	switch v := os.Getenv("SLA_NUDGE_SWEEP"); v {
	case "", "off", "0", "disabled", "false":
		// disabled (default)
	case "on", "true", "default":
		slaNudgeSweepEnabled = true
	default:
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			slaNudgeSweepEnabled = true
			slaNudgeSweepInterval = d
		} else {
			fmt.Fprintf(os.Stderr, "WARNING: invalid SLA_NUDGE_SWEEP %q: %v\n", v, err)
		}
	}

	modelFallbackChannel := sanitizeChannelID("SLACK_MODEL_FALLBACK_CHANNEL",
		os.Getenv("SLACK_MODEL_FALLBACK_CHANNEL"))

	return &config{
		beadsHTTPAddr:      envOrDefault("BEADS_HTTP_ADDR", "http://localhost:8080"),
		slackBotToken:      os.Getenv("SLACK_BOT_TOKEN"),
		slackAppToken:      os.Getenv("SLACK_APP_TOKEN"),
		slackSigningSecret: os.Getenv("SLACK_SIGNING_SECRET"),
		slackChannel:       sanitizeChannelID("SLACK_CHANNEL", os.Getenv("SLACK_CHANNEL")),
		listenAddr:         envOrDefault("SLACK_LISTEN_ADDR", ":8090"),
		landListenAddr:     strings.TrimSpace(os.Getenv("SLACK_BRIDGE_LAND_LISTEN_ADDR")),
		logLevel:           envOrDefault("LOG_LEVEL", "info"),
		statePath:          envOrDefault("STATE_PATH", "/data/slack-bridge-state.json"),
		debug:              os.Getenv("DEBUG") == "true",

		threadingMode:   threadingMode,
		decisionDMMode:  decisionDMMode,
		threadCardsMode: threadCardsMode,

		dashboardEnabled:  dashEnabled,
		dashboardChannel:  dashChannel,
		dashboardInterval: dashInterval,

		gitlabToken:   os.Getenv("GITLAB_API_TOKEN"),
		gitlabBaseURL: envOrDefault("GITLAB_BASE_URL", "https://gitlab.com"),
		projects:      projects,
		controllerURL: os.Getenv("CONTROLLER_URL"),

		githubToken: os.Getenv("GITHUB_TOKEN"),
		repos:       repos,

		coopmuxPublicURL: os.Getenv("COOPMUX_PUBLIC_URL"),
		runnerPublicURL:  os.Getenv("GARDENIO_RUNNER_PUBLIC_URL"),
		beadsWebURL:      os.Getenv("BEADS_WEB_URL"),
		beadsViewerURL:   strings.TrimRight(os.Getenv("BEADS_VIEWER_URL"), "/"),

		jiraBaseURL:     strings.TrimRight(os.Getenv("JIRA_BASE_URL"), "/"),
		jiraProjectKeys: parseCommaSeparated(os.Getenv("JIRA_PROJECT_KEYS")),

		defaultDMProject:   os.Getenv("DEFAULT_DM_PROJECT"),
		gardenerOnInvite:   os.Getenv("GARDENER_ON_INVITE") == "true",
		mentionGateEnabled: os.Getenv("MENTION_GATE_ENABLED") == "true",
		slashGateEnabled:   os.Getenv("SLASH_GATE_ENABLED") == "true",

		bareIDPreviewEnabled: os.Getenv("SLACK_BARE_ID_PREVIEW_ENABLED") == "true",
		linkHydrateEnabled:   os.Getenv("SLACK_LINK_HYDRATE_ENABLED") == "true",

		linkUnfurlChannels: parseCommaSeparated(os.Getenv("SLACK_LINK_UNFURL_CHANNELS")),

		routerConfig: routerCfg,


		scifGateAlertChannel: sanitizeChannelID("SCIF_GATE_ALERT_CHANNEL",
			os.Getenv("SCIF_GATE_ALERT_CHANNEL")),
		scifGateAlertPortalURL: strings.TrimRight(
			strings.TrimSpace(os.Getenv("SCIF_GATE_ALERT_PORTAL_URL")), "/"),

		modelFallbackChannel: modelFallbackChannel,

		escalationPingbackInterval: escalationPingbackInterval,
		escalationPingbackDisabled: escalationPingbackDisabled,
		escalationQueueChannel: sanitizeChannelID("SLACK_ESCALATION_QUEUE_CHANNEL",
			os.Getenv("SLACK_ESCALATION_QUEUE_CHANNEL")),
		escalationQueueMention: os.Getenv("SLACK_ESCALATION_QUEUE_MENTION"),

		escalationStaleDigestEnabled:  escalationStaleDigestEnabled,
		escalationStaleDigestInterval: escalationStaleDigestInterval,

		slaNudgeSweepEnabled:  slaNudgeSweepEnabled,
		slaNudgeSweepInterval: slaNudgeSweepInterval,

		bouncerEnabled:     os.Getenv("BOUNCER_ENABLED") == "true",
		bouncerNamespace:   envOrDefault("BOUNCER_NAMESPACE", os.Getenv("POD_NAMESPACE")),
		bouncerMiddlewares: parseCommaSeparated(os.Getenv("BOUNCER_MIDDLEWARES")),

		natsURL:   os.Getenv("NATS_URL"),
		natsToken: os.Getenv("COOP_NATS_TOKEN"),
		natsUser:  os.Getenv("COOP_NATS_USER"),

		nameGenAPIKey:  os.Getenv("NAMEGEN_API_KEY"),
		nameGenBaseURL: os.Getenv("NAMEGEN_BASE_URL"),
		nameGenModel:   os.Getenv("NAMEGEN_MODEL"),

		keycloakURL:           os.Getenv("KEYCLOAK_URL"),
		keycloakRealm:         envOrDefault("KEYCLOAK_REALM", "gardenio"),
		keycloakClientID:      os.Getenv("KEYCLOAK_CLIENT_ID"),
		keycloakClientSecret:  os.Getenv("KEYCLOAK_CLIENT_SECRET"),
		keycloakMentionRole:   os.Getenv("KEYCLOAK_MENTION_ROLE"),
		actAsPerson:           os.Getenv("SLACK_BRIDGE_ACT_AS_PERSON") == "true",
		slackLinkEnabled:      os.Getenv("SLACK_LINK_ENABLED") == "true",
		slackLinkPublicURL:    os.Getenv("SLACK_LINK_PUBLIC_URL"),
		slackLinkIssuerURL:    os.Getenv("SLACK_LINK_ISSUER_URL"),
		slackLinkClientID:     os.Getenv("SLACK_LINK_CLIENT_ID"),
		slackLinkClientSecret: os.Getenv("SLACK_LINK_CLIENT_SECRET"),
		slackLinkKey:          os.Getenv("SLACK_LINK_KEY"),
		slackLinkStorePath:    envOrDefault("SLACK_LINK_STORE_PATH", "/data/slack-links.json"),

		oidcIssuerURL: os.Getenv("OIDC_ISSUER_URL"),
		oidcClientID:  os.Getenv("OIDC_CLIENT_ID"),

		agentToken:         os.Getenv("SLACK_BRIDGE_AGENT_TOKEN"),
		sharedBearerAgents: envBoolDefault("SLACK_BRIDGE_SHARED_BEARER_AGENTS", true),
		insecureOpenAPI:    os.Getenv("SLACK_BRIDGE_INSECURE_OPEN_API") == "1",
		oidcCommonsOnly:    envBoolDefault("SLACK_BRIDGE_OIDC_COMMONS_ONLY", false),

		corsAllowedOrigins: parseCommaSeparated(os.Getenv("CORS_ALLOWED_ORIGINS")),

		slackAPIURL: os.Getenv("SLACK_API_URL"),

		threadTaskAutoCloseWindow: threadTaskAutoCloseWindow,
		threadTaskSweepInterval:   threadTaskSweepInterval,
		threadTaskSpawnStallGrace: threadTaskSpawnStallGrace,
	}
}

// parseRepoList parses a comma-separated list of "owner/repo" strings.
// A repo suffixed with "~ext" is marked as an external dependency
// (deployed via rolling tag, not its own release tags).
func parseRepoList(s string) []bridge.RepoRef {
	var repos []bridge.RepoRef
	for _, entry := range strings.Split(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		external := false
		if after, found := strings.CutSuffix(entry, "~ext"); found {
			entry = after
			external = true
		}
		parts := strings.SplitN(entry, "/", 2)
		if len(parts) != 2 {
			continue
		}
		repos = append(repos, bridge.RepoRef{Owner: parts[0], Repo: parts[1], External: external})
	}
	return repos
}

// parseProjectList parses a comma-separated list of GitLab project paths.
// Supports multi-segment paths like "example-org/gardenio".
// A project suffixed with "~ext" is marked as an external dependency.
func parseProjectList(s string) []bridge.ProjectRef {
	if s == "" {
		return nil
	}
	var projects []bridge.ProjectRef
	for _, entry := range strings.Split(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		external := false
		if after, found := strings.CutSuffix(entry, "~ext"); found {
			entry = after
			external = true
		}
		projects = append(projects, bridge.ProjectRef{Path: entry, External: external})
	}
	return projects
}

func parseCommaSeparated(s string) []string {
	if s == "" {
		return nil
	}
	var result []string
	for _, v := range strings.Split(s, ",") {
		v = strings.TrimSpace(v)
		if v != "" {
			result = append(result, v)
		}
	}
	return result
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envBoolDefault reads key as a bool (strconv.ParseBool), fallback when it is
// unset or unparseable.
func envBoolDefault(key string, fallback bool) bool {
	if b, err := strconv.ParseBool(os.Getenv(key)); err == nil {
		return b
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

// loadNotificationConfig loads bridge notification suppression settings from
// "bridge-notifications" config beads. Returns defaults if none are found.
func loadNotificationConfig(ctx context.Context, daemon *beadsapi.Client, logger *slog.Logger) bridge.NotificationConfig {
	cfg := bridge.DefaultNotificationConfig()

	result, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"config"},
		Statuses: []string{"open"},
		Search:   "bridge-notifications",
		Limit:    10,
	})
	if err != nil {
		logger.Debug("bridge-notifications: failed to load config beads", "error", err)
		return cfg
	}

	// Find the most specific matching bead (global < project < role).
	for _, bead := range result.Beads {
		if bead.Title != "bridge-notifications" {
			continue
		}
		// Parse the value field as JSON into NotificationConfig.
		if v, ok := bead.Fields["value"]; ok && v != "" {
			if err := json.Unmarshal([]byte(v), &cfg); err != nil {
				logger.Warn("bridge-notifications: failed to parse config bead value",
					"bead", bead.ID, "error", err)
				continue
			}
			logger.Info("bridge-notifications: loaded config",
				"bead", bead.ID,
				"suppress_bead_activity", cfg.SuppressBeadActivity,
				"suppress_spawn_confirmation", cfg.SuppressSpawnConfirmation,
				"suppress_tool_stream", cfg.SuppressToolStream,
				"suppress_squawk", cfg.SuppressSquawk,
				"hold_assistant_status", cfg.HoldAssistantStatus)
			return cfg
		}
	}

	logger.Debug("bridge-notifications: no config bead found, using defaults")
	return cfg
}

// loadThreadCardsConfig loads the thread-cards runtime kill switch + project
// allow-list from "bridge-thread-cards" config beads. Returns defaults
// (Enabled=true, no allow-list) if none are found, so absence defers to the
// SLACK_THREAD_CARDS env mode (which itself defaults to "off").
func loadThreadCardsConfig(ctx context.Context, daemon *beadsapi.Client, logger *slog.Logger) bridge.ThreadCardsConfig {
	cfg := bridge.DefaultThreadCardsConfig()

	result, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"config"},
		Statuses: []string{"open"},
		Search:   "bridge-thread-cards",
		Limit:    10,
	})
	if err != nil {
		logger.Debug("bridge-thread-cards: failed to load config beads", "error", err)
		return cfg
	}

	// Find the most specific matching bead (global < project < role).
	for _, bead := range result.Beads {
		if bead.Title != "bridge-thread-cards" {
			continue
		}
		if v, ok := bead.Fields["value"]; ok && v != "" {
			if err := json.Unmarshal([]byte(v), &cfg); err != nil {
				logger.Warn("bridge-thread-cards: failed to parse config bead value",
					"bead", bead.ID, "error", err)
				continue
			}
			logger.Info("bridge-thread-cards: loaded config",
				"bead", bead.ID,
				"enabled", cfg.Enabled,
				"projects", cfg.Projects,
				"exclude_channels", cfg.ExcludeChannels)
			return cfg
		}
	}

	logger.Debug("bridge-thread-cards: no config bead found, using defaults")
	return cfg
}

// buildImageConfigs creates image tracking configs for gardenio images.
// It prefers GitLab project paths when available, falling back to the
// legacy GitHub repo list. The deployed version (calver tag) is shared
// across all gardenio images.
func buildImageConfigs(projects []bridge.ProjectRef, repos []bridge.RepoRef, deployedVersion string) []bridge.ImageTrackConfig {
	// Prefer GitLab projects.
	for _, p := range projects {
		if strings.Contains(p.Path, "gardenio") {
			return bridge.GitLabMonorepoImageConfigs(p.Path, deployedVersion)
		}
	}
	// Legacy GitHub fallback.
	//
	// This switch used to distinguish two GitHub repo names under the old
	// project name: the monorepo, and an older single-service layout. Both are
	// now one repo, so the two cases collapsed into a duplicate. gardenio is a
	// monorepo, so the monorepo branch is the correct survivor; the
	// single-service branch is unreachable, since r.Repo can no longer be the
	// old name.
	for _, r := range repos {
		if r.Repo == "gardenio" {
			return bridge.MonorepoImageConfigs(r, deployedVersion)
		}
	}
	return nil
}
