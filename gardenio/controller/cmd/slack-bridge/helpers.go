// Helper functions extracted from main() to keep the entry-point wiring thin
// and the branch-carrying logic unit-testable. Behavior is unchanged from
// the inline forms they replaced.
package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	slackapi "github.com/slack-go/slack"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/namegen"
	"gardenio/controller/internal/spawnguard"
)

// registerHealthHandlers wires /healthz (always 200 with version) and /readyz
// (gated on the caller-supplied readiness probe) onto the given mux.
//
// The readiness closure returns (ok, reason). When ok is false, /readyz
// responds 503 and includes the reason in the JSON payload so health checks
// can report which subsystem is unhealthy (e.g. "oidc_provider_unavailable"
// or "socket_mode_disconnected"). When ok is true, reason is ignored.
//
// lastEvent, if non-nil, is invoked on each /healthz request to obtain the
// timestamp of the most recent NATS event consumed by the bridge. When it
// returns a non-zero time the response includes a "last_event_at" RFC3339
// field — consumed by the platform health page (ka-0000000152 tile 6) to
// detect a stalled bridge.
func registerHealthHandlers(mux *http.ServeMux, ver string, isReady func() (bool, string), lastEvent func() time.Time) {
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if lastEvent != nil {
			if t := lastEvent(); !t.IsZero() {
				fmt.Fprintf(w, `{"status":"ok","version":"%s","last_event_at":"%s"}`, ver, t.UTC().Format(time.RFC3339Nano))
				return
			}
		}
		fmt.Fprintf(w, `{"status":"ok","version":"%s"}`, ver)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if ok, reason := isReady(); !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, `{"status":"not_ready","reason":"%s"}`, reason)
			return
		}
		fmt.Fprintf(w, `{"status":"ok"}`)
	})
}

// buildGitLabClient returns a GitLab client when a token is configured, or
// nil when GITLAB_API_TOKEN is unset (the /unreleased command then falls
// back to the legacy GitHub path).
func buildGitLabClient(cfg *config, logger *slog.Logger) *bridge.GitLabClient {
	if cfg.gitlabToken == "" {
		return nil
	}
	return bridge.NewGitLabClient(bridge.GitLabClientConfig{
		BaseURL: cfg.gitlabBaseURL,
		Token:   cfg.gitlabToken,
		Logger:  logger,
	})
}

// buildRouter returns a Slack channel router built from the parsed
// SLACK_ROUTER_CONFIG JSON, or nil when routing is not configured.
func buildRouter(cfg *config, logger *slog.Logger) *bridge.Router {
	if cfg.routerConfig == nil {
		return nil
	}
	r := bridge.NewRouter(*cfg.routerConfig)
	logger.Info("channel router enabled",
		"patterns", len(cfg.routerConfig.Channels),
		"default_channel", cfg.routerConfig.DefaultChannel)
	return r
}

// applyCostSuppressionOverride applies the SUPPRESS_COST_REPORTING env var
// to a NotificationConfig. An empty value leaves the bead-loaded config
// alone; any value other than "false" or "0" forces cost suppression on.
// Extracted so the three-way branch is unit-testable.
func applyCostSuppressionOverride(envValue string, cfg bridge.NotificationConfig) bridge.NotificationConfig {
	if envValue == "" {
		return cfg
	}
	cfg.SuppressCostReporting = envValue != "false" && envValue != "0"
	return cfg
}

// buildNameGenerator returns an LLM-backed name generator for /spawn when
// NAMEGEN_API_KEY is set, applying optional base-URL and model overrides.
// Returns nil when disabled.
func buildNameGenerator(cfg *config, logger *slog.Logger) bridge.NameGenerator {
	if cfg.nameGenAPIKey == "" {
		return nil
	}
	var opts []namegen.Option
	if cfg.nameGenBaseURL != "" {
		opts = append(opts, namegen.WithBaseURL(cfg.nameGenBaseURL))
	}
	if cfg.nameGenModel != "" {
		opts = append(opts, namegen.WithModel(cfg.nameGenModel))
	}
	gen := namegen.New(cfg.nameGenAPIKey, opts...)
	if gen == nil {
		return nil
	}
	logger.Info("LLM name generation enabled for /spawn", "model", cfg.nameGenModel)
	return gen
}

// registerBridgeAPIs wires the HTTP surface that does not depend on Slack:
// /api/unreleased, /api/decisions/*, /api/escalations/*, and the /ui/ SPA.
// It also primes the user directory in the background. Returns the directory
// and escalation API so the caller can attach additional wiring (user lookup
// from bot, slack-ref lookup) after Slack is initialized.
func registerBridgeAPIs(
	mux *http.ServeMux,
	cfg *config,
	daemon *beadsapi.Client,
	state *bridge.StateManager,
	gitlabClient *bridge.GitLabClient,
	imageConfigs []bridge.ImageTrackConfig,
	ver string,
	logger *slog.Logger,
) (*bridge.UserDirectory, *bridge.EscalationAPI) {
	mux.HandleFunc("/api/unreleased", bridge.HandleUnreleased(
		buildUnreleasedConfig(cfg, gitlabClient, imageConfigs, ver, logger),
	))

	// Decisions web UI and API.
	decisionAPI := bridge.NewDecisionAPI(daemon, logger)
	decisionAPI.RegisterRoutes(mux)
	mux.Handle("/api/decisions/events", bridge.NewDecisionSSEProxy(
		cfg.beadsHTTPAddr, os.Getenv("BEADS_DAEMON_TOKEN"), cfg.corsAllowedOrigins, logger))

	// Escalation approval web UI and API.
	userDir := bridge.NewUserDirectory(daemon, logger)
	go func() {
		if err := userDir.Refresh(context.Background()); err != nil {
			logger.Warn("initial user directory refresh failed", "error", err)
		}
	}()
	escalationAPI := bridge.NewEscalationAPI(daemon, userDir, logger)
	escalationAPI.SetSlackRefLookup(state.GetDecisionMessage)
	escalationAPI.RegisterRoutes(mux)
	mux.Handle("/api/escalations/events", bridge.NewEscalationSSEProxy(
		cfg.beadsHTTPAddr, os.Getenv("BEADS_DAEMON_TOKEN"), cfg.corsAllowedOrigins, logger))

	mux.Handle("/ui/", http.StripPrefix("/ui/", bridge.WebHandler()))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/ui/", http.StatusFound)
			return
		}
		http.NotFound(w, r)
	})
	return userDir, escalationAPI
}

// buildBotConfig assembles the BotConfig passed to bridge.NewBot when Socket
// Mode is enabled. Extracted so the large field list is exercised by tests
// without having to construct a real Slack bot.
func buildBotConfig(
	cfg *config,
	daemon bridge.BeadClient,
	state *bridge.StateManager,
	router *bridge.Router,
	bouncer *bridge.Bouncer,
	gitlabClient *bridge.GitLabClient,
	keycloakChecker *bridge.KeycloakRoleChecker,
	nameGen bridge.NameGenerator,
	imageConfigs []bridge.ImageTrackConfig,
	notifConfig bridge.NotificationConfig,
	threadCardsConfig bridge.ThreadCardsConfig,
	ver string,
	logger *slog.Logger,
) bridge.BotConfig {
	return bridge.BotConfig{
		BotToken:               cfg.slackBotToken,
		AppToken:               cfg.slackAppToken,
		Channel:                cfg.slackChannel,
		ThreadingMode:          cfg.threadingMode,
		DecisionDMMode:         cfg.decisionDMMode,
		ThreadCardsMode:        cfg.threadCardsMode,
		ThreadCards:            threadCardsConfig,
		Daemon:                 daemon,
		State:                  state,
		Router:                 router,
		Bouncer:                bouncer,
		Logger:                 logger,
		Debug:                  cfg.debug,
		GitLabClient:           gitlabClient,
		Projects:               cfg.projects,
		GitHubToken:            cfg.githubToken,
		Repos:                  cfg.repos,
		Version:                ver,
		ControllerURL:          cfg.controllerURL,
		CoopmuxPublicURL:       cfg.coopmuxPublicURL,
		RunnerPublicURL:        cfg.runnerPublicURL,
		BeadsWebURL:            cfg.beadsWebURL,
		BeadsViewerURL:         cfg.beadsViewerURL,
		EscalationQueueChannel: cfg.escalationQueueChannel,
		EscalationQueueMention: cfg.escalationQueueMention,
		JiraBaseURL:            cfg.jiraBaseURL,
		JiraProjectKeys:        cfg.jiraProjectKeys,
		GitlabBaseURL:          cfg.gitlabBaseURL,
		DefaultDMProject:       cfg.defaultDMProject,
		GardenerOnInvite:       cfg.gardenerOnInvite,
		MentionGateEnabled:     cfg.mentionGateEnabled,
		KeycloakChecker:        keycloakChecker,
		KeycloakMentionRole:    cfg.keycloakMentionRole,
		SlashGateEnabled:       cfg.slashGateEnabled,
		BareIDPreviewEnabled:   cfg.bareIDPreviewEnabled,
		LinkHydrateEnabled:     cfg.linkHydrateEnabled,
		LinkUnfurlChannels:     cfg.linkUnfurlChannels,
		ImageConfigs:           imageConfigs,
		Notifications:          notifConfig,
		NameGen:                nameGen,
		APIURL:                 cfg.slackAPIURL,
	}
}

// reactionNoticeSource identifies slack-bridge on the trigger-notice beads its
// reaction-trigger engine publishes. Matches the "from:<source>" label scheme
// the trigger-engine uses (see internal/bridge/notice.go).
const reactionNoticeSource = "slack-bridge-reaction"

// enableReactionTriggers wires the unified trigger:slack-reaction path onto the
// bot. notify-action output is published as a trigger-notice bead via a
// NoticeNotifier — the same bridge's notice watcher (registered in
// registerSSEWatchers) picks it up over SSE and posts it to Slack, so the
// reaction engine needs no Slack credentials of its own. The spawn guard is the
// shared runaway-protection gate, env-tunable via SPAWN_GUARD_*.
func enableReactionTriggers(bot *bridge.Bot, daemon *beadsapi.Client, logger *slog.Logger) {
	notifier := bridge.NewNoticeNotifier(daemon, reactionNoticeSource, logger)
	bot.EnableReactionTriggers(daemon, notifier, loadReactionSpawnGuard())
	logger.Info("trigger:slack-reaction path enabled", "notice_source", reactionNoticeSource)
}

// loadReactionSpawnGuard builds the runaway-protection gate for the reaction
// trigger engine from SPAWN_GUARD_* env vars, mirroring the standalone
// trigger-engine's loadSpawnGuard. Defaults are used when env vars are unset.
func loadReactionSpawnGuard() *spawnguard.SpawnGuard {
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

// buildUnreleasedConfig assembles the dependencies used to answer
// /api/unreleased. Extracted for unit-testability — the handler wiring itself
// is a one-liner.
func buildUnreleasedConfig(cfg *config, gl *bridge.GitLabClient, images []bridge.ImageTrackConfig, ver string, logger *slog.Logger) bridge.UnreleasedConfig {
	return bridge.UnreleasedConfig{
		GitLab:        gl,
		Projects:      cfg.projects,
		GitHub:        bridge.NewGitHubClientIfConfigured(cfg.githubToken, cfg.repos, logger),
		Repos:         cfg.repos,
		ControllerURL: cfg.controllerURL,
		Version:       ver,
		Images:        images,
	}
}

// resolveMetricsAddr returns the metrics listen address, defaulting to ":9093"
// when the env-derived value is empty.
func resolveMetricsAddr(envValue string) string {
	if envValue == "" {
		return ":9093"
	}
	return envValue
}

// logAuthMode emits a startup warning describing which auth modes are active
// on /api/*. Pure helper — no side effects other than the log line.
func logAuthMode(agentToken string, oidcConfigured, insecureOpenAPI bool, logger *slog.Logger) {
	if !oidcConfigured {
		if insecureOpenAPI {
			logger.Error("slack-bridge has no OIDC and SLACK_BRIDGE_INSECURE_OPEN_API=1: /api/* web routes are OPEN; local dev only")
		} else {
			logger.Error("slack-bridge has no OIDC: /api/* web routes are CLOSED (403); set OIDC_ISSUER_URL, or SLACK_BRIDGE_INSECURE_OPEN_API=1 for local dev only")
		}
	}
	if agentToken == "" {
		logger.Warn("SLACK_BRIDGE_AGENT_TOKEN not set — /api/slack/* and /api/permission/* require OIDC admin role")
	}
}

// buildHTTPServer builds the main HTTP server with a sensible header-read
// timeout. The caller is responsible for starting it via ListenAndServe and
// for handling graceful shutdown.
func buildHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
}

// buildMetricsServer builds the internal metrics HTTP server backed by the
// default Prometheus gatherer (Go/process collectors plus the bridge's
// promauto-registered metrics). The server is not started here — caller
// invokes ListenAndServe in a goroutine.
func buildMetricsServer(addr string) *http.Server {
	// Serve the default registry. client_golang auto-registers the Go and
	// process collectors there at init, and it is also where the bridge's
	// promauto-registered metrics (gardenio_bridge_*,
	// gardenio_operator_activity_total — ka-0000006613) land. A separate private
	// registry would never expose those, so the previous bespoke registry that
	// only held duplicate Go/process collectors is replaced with the default
	// gatherer.
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(prometheus.DefaultGatherer, promhttp.HandlerOpts{}))
	return &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
}

// buildSSEStream wires the SSE event stream used by all bead-event watchers.
// Topics are fixed; the token is read from BEADS_DAEMON_TOKEN so callers do not
// need to thread it through their own config.
func buildSSEStream(cfg *config, dedup *bridge.Dedup, state *bridge.StateManager, logger *slog.Logger) *bridge.SSEStream {
	return bridge.NewSSEStream(bridge.SSEStreamConfig{
		BeadsHTTPAddr: cfg.beadsHTTPAddr,
		Topics: []string{
			"beads.bead.created", "beads.bead.closed", "beads.bead.updated",
			"beads.bead.deleted", "beads.bead.transferred",
			"beads.dependency.added", "beads.dependency.removed",
			"beads.label.added", "beads.label.removed",
			"beads.comment.added", "beads.comment.deleted",
			"beads.follower.added", "beads.follower.removed",
		},
		Token:  os.Getenv("BEADS_DAEMON_TOKEN"),
		Logger: logger,
		Dedup:  dedup,
		State:  state,
	})
}

// buildDashboardConfig assembles the DashboardConfig passed to bridge.NewDashboard.
// Falls back to the default Slack channel when dashboardChannel is empty.
func buildDashboardConfig(cfg *config, notif bridge.NotificationConfig) bridge.DashboardConfig {
	ch := cfg.dashboardChannel
	if ch == "" {
		ch = cfg.slackChannel
	}
	return bridge.DashboardConfig{
		Enabled:      true,
		ChannelID:    ch,
		Interval:     cfg.dashboardInterval,
		SuppressCost: notif.SuppressCostReporting,
	}
}

// newDecisionsWatcher wraps bridge.NewDecisions so main() reads as a single line.
func newDecisionsWatcher(
	daemon bridge.BeadClient,
	notifier bridge.Notifier,
	crossOperator *bridge.CrossOperatorNotify,
	logger *slog.Logger,
) *bridge.Decisions {
	return bridge.NewDecisions(bridge.DecisionsConfig{
		Daemon:        daemon,
		Notifier:      notifier,
		CrossOperator: crossOperator,
		Logger:        logger,
	})
}

// newMailWatcher wraps bridge.NewMail.
func newMailWatcher(daemon bridge.BeadClient, logger *slog.Logger) *bridge.Mail {
	return bridge.NewMail(bridge.MailConfig{
		Daemon: daemon,
		Logger: logger,
	})
}

// newClaimedWatcher wraps bridge.NewClaimed.
func newClaimedWatcher(daemon bridge.BeadClient, logger *slog.Logger) *bridge.Claimed {
	return bridge.NewClaimed(bridge.ClaimedConfig{
		Daemon: daemon,
		Logger: logger,
	})
}

// newAdviceNudgeWatcher wraps bridge.NewAdviceNudge.
func newAdviceNudgeWatcher(daemon bridge.BeadClient, logger *slog.Logger) *bridge.AdviceNudge {
	return bridge.NewAdviceNudge(bridge.AdviceNudgeConfig{
		Daemon: daemon,
		Logger: logger,
	})
}

// agentsNotifierFromBot returns the bot as an AgentNotifier when non-nil, or
// nil — centralising the nil-bot guard used by the agents/jacks/beadActivity
// watchers.
func agentsNotifierFromBot(bot *bridge.Bot) bridge.AgentNotifier {
	if bot == nil {
		return nil
	}
	return bot
}

// jackNotifierFromBot returns the bot as a JackNotifier when non-nil.
func jackNotifierFromBot(bot *bridge.Bot) bridge.JackNotifier {
	if bot == nil {
		return nil
	}
	return bot
}

// beadActivityNotifierFromBot returns the bot as a BeadActivityNotifier when non-nil.
func beadActivityNotifierFromBot(bot *bridge.Bot) bridge.BeadActivityNotifier {
	if bot == nil {
		return nil
	}
	return bot
}

// escalationNotifierFromBot returns the bot as an EscalationNotifier when non-nil.
func escalationNotifierFromBot(bot *bridge.Bot) bridge.EscalationNotifier {
	if bot == nil {
		return nil
	}
	return bot
}

// newAgentsWatcher wraps bridge.NewAgents. It returns both the watcher and the
// constructed auto-closer (nil when daemon is nil) so the caller can start the
// auto-closer's durable backfill sweep (ka-0000001874).
func newAgentsWatcher(bot *bridge.Bot, daemon bridge.ThreadTaskAutoCloserDaemon, window, sweepInterval, spawnStallGrace time.Duration, logger *slog.Logger) (*bridge.Agents, *bridge.ThreadTaskAutoCloser) {
	var autoCloser *bridge.ThreadTaskAutoCloser
	if daemon != nil {
		autoCloser = bridge.NewThreadTaskAutoCloser(bridge.ThreadTaskAutoCloserConfig{
			Daemon:          daemon,
			Logger:          logger,
			Window:          window,
			SweepInterval:   sweepInterval,
			SpawnStallGrace: spawnStallGrace,
		})
	}
	return bridge.NewAgents(bridge.AgentsConfig{
		Notifier:             agentsNotifierFromBot(bot),
		Logger:               logger,
		ThreadTaskAutoCloser: autoCloser,
	}), autoCloser
}

// newJacksWatcher wraps bridge.NewJacks.
func newJacksWatcher(bot *bridge.Bot, logger *slog.Logger) *bridge.Jacks {
	return bridge.NewJacks(bridge.JacksConfig{
		Notifier: jackNotifierFromBot(bot),
		Logger:   logger,
	})
}

// newBeadActivityWatcher wraps bridge.NewBeadActivity.
func newBeadActivityWatcher(bot *bridge.Bot, logger *slog.Logger) *bridge.BeadActivity {
	return bridge.NewBeadActivity(bridge.BeadActivityConfig{
		Notifier: beadActivityNotifierFromBot(bot),
		Logger:   logger,
	})
}

// newEscalationsWatcher wraps bridge.NewEscalations.
func newEscalationsWatcher(
	daemon bridge.BeadClient,
	users *bridge.UserDirectory,
	notifier bridge.Notifier,
	bot *bridge.Bot,
	crossOperator *bridge.CrossOperatorNotify,
	operatorDM *bridge.EscalationOperatorDM,
	logger *slog.Logger,
) *bridge.Escalations {
	return bridge.NewEscalations(bridge.EscalationsConfig{
		Daemon:             daemon,
		Users:              users,
		Notifier:           notifier,
		EscalationNotifier: escalationNotifierFromBot(bot),
		CrossOperator:      crossOperator,
		OperatorDM:         operatorDM,
		Logger:             logger,
	})
}

// buildHookStreamConfig assembles the HookStreamConfig for NATS-delivered
// streaming status updates. Extracted so the field list is testable without
// constructing a real AgentStreamManager.
func buildHookStreamConfig(cfg *config, mgr *bridge.AgentStreamManager, logger *slog.Logger) bridge.HookStreamConfig {
	return bridge.HookStreamConfig{
		NatsURL:       cfg.natsURL,
		NatsToken:     cfg.natsToken,
		NatsUser:      cfg.natsUser,
		StreamManager: mgr,
		Logger:        logger,
	}
}

// buildRotationConfig assembles the RotationConfig for account-rotation Slack
// notifications. Extracted for unit-testability.
func buildRotationConfig(cfg *config, notifier bridge.RotationNotifier, logger *slog.Logger) bridge.RotationConfig {
	return bridge.RotationConfig{
		NatsURL:   cfg.natsURL,
		NatsToken: cfg.natsToken,
		NatsUser:  cfg.natsUser,
		Notifier:  notifier,
		Logger:    logger,
	}
}

// logStartup emits the startup info line with identifying config fields.
// Extracted for unit-testability — the field list is easy to forget to update.
func logStartup(cfg *config, ver, com string, logger *slog.Logger) {
	logger.Info("starting slack-bridge",
		"version", ver,
		"commit", com,
		"beads_http", cfg.beadsHTTPAddr,
		"slack_channel", cfg.slackChannel,
		"threading_mode", cfg.threadingMode,
		"listen_addr", cfg.listenAddr)
}

// logReady emits the "slack-bridge ready" line once all subsystems have
// been wired up. The two mode booleans are derived by the caller.
func logReady(socketMode, webhookMode bool, logger *slog.Logger) {
	logger.Info("slack-bridge ready",
		"socket_mode", socketMode,
		"webhook_mode", webhookMode)
}

// buildWebhookSlackClient creates a standalone Slack API client for webhook
// mode. It honours SLACK_API_URL for E2E mocking. Returns nil when no token
// is configured (caller should guard).
func buildWebhookSlackClient(cfg *config) *slackapi.Client {
	if cfg.slackBotToken == "" {
		return nil
	}
	var opts []slackapi.Option
	if cfg.slackAPIURL != "" {
		opts = append(opts, slackapi.OptionAPIURL(cfg.slackAPIURL))
	}
	return slackapi.New(cfg.slackBotToken, opts...)
}

// registerSlackClientAPIs wires thread/channel/human-survey HTTP APIs onto
// the given mux using a Slack client. Shared between Socket Mode (uses
// bot.API()) and webhook mode (uses a standalone client). It returns the
// thread API, whose reply deduper the conversation agent API shares.
func registerSlackClientAPIs(
	mux *http.ServeMux,
	client *slackapi.Client,
	daemon bridge.BeadClient,
	resolver bridge.AgentCardResolver,
	logger *slog.Logger,
) *bridge.SlackThreadAPI {
	threadAPI := bridge.NewSlackThreadAPI(client, logger)
	threadAPI.RegisterRoutes(mux)
	channelAPI := bridge.NewSlackChannelAPI(client, logger)
	channelAPI.RegisterRoutes(mux)
	channelAPI.RegisterManageRoutes(mux)
	userAPI := bridge.NewSlackUserAPI(client, logger)
	userAPI.RegisterRoutes(mux)
	humanSurveyAPI := bridge.NewHumanSurveyAPI(client, daemon, resolver, logger)
	humanSurveyAPI.RegisterRoutes(mux)
	return threadAPI
}

// dashboardEnabled reports whether the dashboard should be started. Requires
// the feature flag on AND a bot for Slack posting.
func dashboardEnabled(cfg *config, bot *bridge.Bot) bool {
	return cfg.dashboardEnabled && bot != nil
}

// isPlaceholderChannelID reports whether s is an obvious operator-injected
// placeholder value (e.g. "PLACEHOLDER_NEEDS_REAL_VALUE_SEE_KD_XXXXXXXXXX")
// rather than a real Slack channel ID. Real channel IDs are alphanumeric
// uppercase strings starting with C/D/G/U; placeholder markers contain
// "PLACEHOLDER", "_NEEDS_", or "REPLACE_ME" tokens. Used to fail-soft when
// a deployment override leaves channel env vars stubbed out (ka-0000003912).
func isPlaceholderChannelID(s string) bool {
	if s == "" {
		return false
	}
	upper := strings.ToUpper(s)
	for _, marker := range []string{"PLACEHOLDER", "_NEEDS_", "REPLACE_ME", "TODO_"} {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	return false
}

// sanitizeChannelID returns "" and logs a stderr warning when s is a
// placeholder value, else returns s unchanged. envName is the source env var
// for the warning message so operators can locate the misconfiguration.
func sanitizeChannelID(envName, s string) string {
	if isPlaceholderChannelID(s) {
		fmt.Fprintf(os.Stderr, "WARNING: %s=%q looks like a placeholder; treating as unset to avoid channel_not_found spam (ka-0000003912)\n", envName, s)
		return ""
	}
	return s
}

// shutdownTimeout is the graceful shutdown deadline. Declared as a helper so
// tests can reference the same constant used by main().
func shutdownTimeout() time.Duration { return 5 * time.Second }

// gracefulShutdown tears down the HTTP server with a bounded timeout, logging
// any error from Shutdown. Extracted so main()'s final block is tested.
func gracefulShutdown(srv *http.Server, logger *slog.Logger) {
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout())
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("HTTP server shutdown error", "error", err)
	}
}

// registerBotSSEHandlers wires chat/squawk/comment-forward/DM-delivery
// handlers onto the SSE stream and back-links DM delivery to the bot. No-op
// when bot is nil (webhook or no-Slack mode).
func registerBotSSEHandlers(
	ctx context.Context,
	bot *bridge.Bot,
	daemon bridge.BeadClient,
	machine beadsapi.TokenSource,
	state *bridge.StateManager,
	sseStream *bridge.SSEStream,
	logger *slog.Logger,
) {
	if bot == nil {
		return
	}
	chat := bridge.NewChat(bridge.ChatConfig{
		Daemon: daemon,
		Bot:    bot,
		State:  state,
		Logger: logger,
	})
	chat.RegisterHandlers(sseStream)

	squawk := bridge.NewSquawk(bridge.SquawkConfig{
		Daemon: daemon,
		Bot:    bot,
		Logger: logger,
	})
	squawk.RegisterHandlers(sseStream)

	// Trigger notices: external services (trigger-engine, gitlab-bridge)
	// publish closed message beads with the trigger-notice label instead of
	// posting to Slack directly. slack-bridge picks them up here and posts
	// on its own bot credentials.
	notice := bridge.NewNotice(bridge.NoticeConfig{
		API:            bot.API(),
		DefaultChannel: bot.DefaultChannel(),
		Logger:         logger,
	})
	notice.RegisterHandlers(sseStream)

	// Commons mirror: a person's message in a Commons channel bound to Slack
	// (mirror=out or both) posts into that Slack channel as "*Name* (via
	// Commons)". Slack messages come the other way in handleMessageEvent.
	// Its sweep posts a message written while the bridge was down.
	// Commons replies into a Slack-bound thread take the thread forwarder below.
	//
	// Their attachments are uploaded into Slack, fetched from beads-viewer's
	// machine file route (BEADS_VIEWER_INTERNAL_URL, the viewer's in-cluster
	// Service) on the bridge's machine principal (gd-qnF6mJZtkQ). Without
	// either they stay viewer links.
	commonsFiles := bridge.NewCommonsFileFetcher(os.Getenv("BEADS_VIEWER_INTERNAL_URL"), machine, logger)
	if commonsFiles != nil {
		logger.Info("commons-mirror: attachments upload into Slack", "viewer", os.Getenv("BEADS_VIEWER_INTERNAL_URL"))
	} else {
		logger.Info("commons-mirror: attachments post as viewer links (no BEADS_VIEWER_INTERNAL_URL or machine principal)")
	}

	// Thread-message forwarder: the beads-viewer /threads composer cannot post
	// to Slack (it holds no bot token). It writes a closed `thread-say`
	// message bead carrying the wish head's slack_thread_channel/ts; this
	// watcher relays it into the Slack thread on the bot's credentials,
	// reusing the shared slackCardLimiter (Phase 2 task ka-0000005643).
	//
	// Bot is passed so that, after the relay, a viewer reply gets the SAME agent
	// outcome an @gardenio mention would — spawn / respawn / nudge (ka-0000005407).
	// A Commons reply's attachments upload into the thread on the Commons
	// mirror's fetcher (gd-WxRuyfdUtR).
	threadMsgFwd := bridge.NewThreadMessageForward(bridge.ThreadMessageForwardConfig{
		Router:  bot.Conversation(),
		Bot:     bot,
		Limiter: bot.SlackCardLimiter(),
		Files:   commonsFiles,
		Logger:  logger,
	})
	threadMsgFwd.RegisterHandlers(sseStream)

	commonsMirror := bridge.NewCommonsMirror(bridge.CommonsMirrorConfig{
		Router:  bot.Conversation(),
		Bot:     bot,
		Limiter: bot.SlackCardLimiter(),
		Files:   commonsFiles,
		Logger:  logger,
	})
	commonsMirror.RegisterHandlers(sseStream)
	go commonsMirror.Start(ctx)

	// Commons DMs: a person's message in a DM channel reaches the agents in
	// it as soon as it is written (the inbound sweep is the safety net).
	bot.RegisterCommonsDMHandlers(sseStream)
	// A DM whose agent is gone, with nothing open or unread in it, closes.
	go bot.CloseDoneDMs(ctx)

	// Herder closes: a fulfilled wish's card reads closed (gd-EfbganTatT).
	bot.RegisterHerderCloseHandlers(sseStream)

	// Herder decisions: the owner's answer about a stale wish closes it,
	// revives it or leaves it waiting (gd-g6lGiK8KXv).
	bot.RegisterHerderDecisionHandlers(sseStream)

	commentFwd := bridge.NewCommentForward(bridge.CommentForwardConfig{
		Daemon: daemon,
		Bot:    bot,
		Logger: logger,
	})
	commentFwd.RegisterHandlers(sseStream)

	dmDelivery := bridge.NewDMDelivery(bridge.DMDeliveryConfig{
		Daemon:  daemon,
		Bot:     bot,
		Logger:  logger,
		Machine: machine,
	})
	dmDelivery.RegisterHandlers(sseStream)
	bot.SetDMDelivery(dmDelivery)
	// The permission bead writes (type:permission is admin-gated; the
	// dm-delivery role grants them to the machine principal).
	bot.SetMachinePrincipal(machine)
	// Sweep the dedup + reply-tracking maps so they cannot leak for the process
	// lifetime (ka-0000003049).
	go dmDelivery.Run(ctx)
}

// bouncerEnabled reports whether every pre-condition for IP-whitelist
// management is met: the feature flag is on, a namespace is set, and at least
// one middleware name is configured. Extracted so the three-way guard at the
// top of main() is unit-testable.
func bouncerEnabled(cfg *config) bool {
	return cfg.bouncerEnabled && cfg.bouncerNamespace != "" && len(cfg.bouncerMiddlewares) > 0
}

// shouldLogSlackAPIOverride returns true when main() should log a visible
// warning about SLACK_API_URL pointing at a mock.
func shouldLogSlackAPIOverride(cfg *config) bool {
	return cfg.slackAPIURL != ""
}

// webhookModeRequiresSigningSecret reports whether webhook mode is selected
// (bot token set, app token unset) AND the signing secret is missing. When
// this returns true, main() must exit 1 (audit AUDIT-2026-04-18 §N2).
func webhookModeRequiresSigningSecret(cfg *config) bool {
	return cfg.slackBotToken != "" && cfg.slackAppToken == "" && cfg.slackSigningSecret == ""
}

// socketModeEnabled reports whether both Slack tokens are configured for
// real-time Socket Mode operation. When false, the bridge falls back to the
// webhook path (or no-Slack path when bot token is also empty).
func socketModeEnabled(cfg *config) bool {
	return cfg.slackBotToken != "" && cfg.slackAppToken != ""
}

// buildKeycloakChecker returns a Keycloak role checker for the mention gate
// when the full Keycloak config + mention role are supplied. Any missing
// field disables the checker (and therefore the mention gate).
func buildKeycloakChecker(cfg *config, logger *slog.Logger) *bridge.KeycloakRoleChecker {
	kcCfg := bridge.KeycloakConfig{
		BaseURL:      cfg.keycloakURL,
		Realm:        cfg.keycloakRealm,
		ClientID:     cfg.keycloakClientID,
		ClientSecret: cfg.keycloakClientSecret,
	}
	if !kcCfg.IsConfigured() || cfg.keycloakMentionRole == "" {
		return nil
	}
	checker := bridge.NewKeycloakRoleChecker(kcCfg, logger)
	logger.Info("keycloak mention role check enabled",
		"realm", cfg.keycloakRealm, "role", cfg.keycloakMentionRole)
	return checker
}

// buildPersonRoleSource returns the Keycloak-backed role source that makes the
// bot act as the person a Slack user maps to (gd-7CDM9gdog0), or nil when
// SLACK_BRIDGE_ACT_AS_PERSON is off. Turning it on is all-or-nothing: with
// Keycloak or the edge-auth HMAC key missing it returns an error so the bridge
// refuses to start, rather than silently falling back to the legacy slack:U…
// envelope that this feature exists to replace.
func buildPersonRoleSource(cfg *config, edgeAuthKeySet bool, logger *slog.Logger) (*bridge.KeycloakRoleChecker, error) {
	if !cfg.actAsPerson {
		return nil, nil
	}
	kcCfg := bridge.KeycloakConfig{
		BaseURL:      cfg.keycloakURL,
		Realm:        cfg.keycloakRealm,
		ClientID:     cfg.keycloakClientID,
		ClientSecret: cfg.keycloakClientSecret,
	}
	if !kcCfg.IsConfigured() {
		return nil, fmt.Errorf("SLACK_BRIDGE_ACT_AS_PERSON needs KEYCLOAK_URL, KEYCLOAK_REALM, KEYCLOAK_CLIENT_ID and KEYCLOAK_CLIENT_SECRET (the roles come from Keycloak)")
	}
	if !edgeAuthKeySet {
		return nil, fmt.Errorf("SLACK_BRIDGE_ACT_AS_PERSON needs SLACK_BRIDGE_EDGE_AUTH_HMAC_KEY (the bridge signs the person's identity for the daemon)")
	}
	logger.Info("slack-bridge acts as the mapped person with Keycloak roles", "realm", cfg.keycloakRealm)
	return bridge.NewKeycloakRoleChecker(kcCfg, logger), nil
}

// wirePersonRoles turns on person-bound Slack writes on the Socket Mode bot
// when SLACK_BRIDGE_ACT_AS_PERSON is set (gd-7CDM9gdog0). It exits the process
// on a half-configured deploy, or when there is no Socket Mode bot to wire
// (webhook mode keeps the legacy envelope), instead of silently reverting to
// slack:U… writes. A no-op when the flag is off.
func wirePersonRoles(cfg *config, bot *bridge.Bot, logger *slog.Logger) {
	personRoles, err := buildPersonRoleSource(cfg, os.Getenv("SLACK_BRIDGE_EDGE_AUTH_HMAC_KEY") != "", logger)
	if err != nil {
		logger.Error("invalid person-roles configuration", "error", err)
		os.Exit(1)
	}
	if personRoles == nil {
		return
	}
	if bot == nil {
		logger.Error("SLACK_BRIDGE_ACT_AS_PERSON is only supported in Socket Mode")
		os.Exit(1)
	}
	bot.SetPersonRoleSource(personRoles)
}

// buildSlackLinker returns the account linker (gd-OZJ6H2S3xx) or nil when
// SLACK_LINK_ENABLED is off. Turning it on is all-or-nothing: a missing
// public URL, issuer, client, key or store is an error so the bridge refuses
// to start rather than silently keeping human Slack writes on its own
// credential, which this feature exists to replace.
func buildSlackLinker(cfg *config, daemon bridge.SlackLinkDaemon, logger *slog.Logger) (*bridge.SlackLinker, error) {
	if !cfg.slackLinkEnabled {
		return nil, nil
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(cfg.slackLinkKey))
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("SLACK_LINK_ENABLED needs SLACK_LINK_KEY, a base64 32-byte key (openssl rand -base64 32)")
	}
	if cfg.slackLinkPublicURL == "" || cfg.slackLinkIssuerURL == "" || cfg.slackLinkClientID == "" || cfg.slackLinkClientSecret == "" {
		return nil, fmt.Errorf("SLACK_LINK_ENABLED needs SLACK_LINK_PUBLIC_URL, SLACK_LINK_ISSUER_URL, SLACK_LINK_CLIENT_ID and SLACK_LINK_CLIENT_SECRET")
	}
	store, err := bridge.NewFileSlackLinkStore(cfg.slackLinkStorePath)
	if err != nil {
		return nil, fmt.Errorf("SLACK_LINK_STORE_PATH: %w", err)
	}
	l, err := bridge.NewSlackLinker(bridge.SlackLinkConfig{
		PublicURL:    cfg.slackLinkPublicURL,
		IssuerURL:    cfg.slackLinkIssuerURL,
		ClientID:     cfg.slackLinkClientID,
		ClientSecret: cfg.slackLinkClientSecret,
		Key:          key,
		StorePath:    cfg.slackLinkStorePath,
	}, store, daemon, logger)
	if err != nil {
		return nil, err
	}
	logger.Info("slack account linking enabled: human Slack writes carry the linked person's own token",
		"public_url", cfg.slackLinkPublicURL, "issuer", cfg.slackLinkIssuerURL, "client_id", cfg.slackLinkClientID, "store", cfg.slackLinkStorePath)
	return l, nil
}

// wireSlackLink turns on account-linked Slack writes on the Socket Mode bot
// and mounts /slack/link/ on the bridge listener. Exits the process on a
// half-configured deploy or in webhook mode. A no-op when the flag is off.
// The daemon client is switched to prefer a per-request user bearer on
// writes so the person's token (WithUserBearer) is what goes on the wire;
// the bridge's autonomous writes stay on its own credential until Task 5
// (gd-Hc34EENbO9) moves them to its machine principal.
func wireSlackLink(cfg *config, bot *bridge.Bot, daemon *beadsapi.Client, mux *http.ServeMux, logger *slog.Logger) {
	linker, err := buildSlackLinker(cfg, daemon, logger)
	if err != nil {
		logger.Error("invalid slack account-linking configuration", "error", err)
		os.Exit(1)
	}
	if linker == nil {
		return
	}
	if bot == nil {
		logger.Error("SLACK_LINK_ENABLED is only supported in Socket Mode")
		os.Exit(1)
	}
	if cfg.actAsPerson {
		logger.Warn("SLACK_BRIDGE_ACT_AS_PERSON is set together with SLACK_LINK_ENABLED; account linking takes precedence")
	}
	daemon.SetPreferUserBearerForWrites(true)
	bot.SetSlackLinker(linker)
	mux.Handle("/slack/link/", linker.Handler())
}
