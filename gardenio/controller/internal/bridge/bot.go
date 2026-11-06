// Package bridge provides the Slack Socket Mode bot for the slack-bridge.
//
// Bot wraps the SlackNotifier and adds Socket Mode for real-time events,
// slash commands, and interactive modals. It runs alongside the SSE stream
// for bead lifecycle events.
//
// The Bot implementation is split across several files:
//   - bot.go — core struct, event dispatch, helpers
//   - bot_agents.go — agent card management and lifecycle operations
//   - bot_assistant.go — Slack AI Apps framework (assistant thread events, status, prompts)
//   - bot_commands.go — slash command handlers (/spawn, /decisions, /roster)
//   - bot_decisions.go — decision notifications, modals, resolve/dismiss
//   - bot_decisions_modal.go — decision modal rendering
//   - bot_mentions.go — @mention handling in agent threads
//   - bot_notifications.go — agent crash, jack on/off/expired alerts
package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
	"golang.org/x/time/rate"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"
	"gardenio/controller/internal/spawnguard"
)

// NameGenerator derives a kebab-case slug and a human-readable title from
// long prompt text (LLM or heuristic; see internal/namegen).
type NameGenerator interface {
	GenerateName(ctx context.Context, text string) (slug, title string, err error)
}

// Bot is the Slack Socket Mode bot that handles interactive events.
type Bot struct {
	api    *slack.Client
	socket *socketmode.Client
	state  *StateManager
	daemon BeadClient
	router *Router
	logger *slog.Logger

	// taskRuns caches the agent cards' task line arms (heraldry_cards.go).
	taskRuns taskRuns

	// conv is the conversation Router over api and daemon (conversation.go);
	// built on first use, so it is unrelated to router (the agent router).
	conv conversationSeam

	// respawnHook, when set, replaces watchMailDelivery's respawn (a test
	// seam; production leaves it nil).
	respawnHook func(agent string)

	// themes caches channel -> card theme for themeFor (theme.go).
	themes themeCache

	// channelBeads caches Slack channel -> bound channel bead for the
	// channel_id dual-write (channel_stamp.go).
	channelBeads channelBeadCache
	// channelBackfillRefused holds thread heads the daemon refused a
	// channel_id stamp on; only the backfill goroutine touches it.
	channelBackfillRefused map[string]bool

	channel   string // default channel ID
	botUserID string // bot's own user ID (set on connect)

	// Health state.
	connected      atomic.Bool
	numConnections atomic.Int32

	// Threading mode: "" / "flat" = flat messages, "agent" = threaded under agent cards.
	threadingMode string
	// decisionDMMode controls DM-first decision delivery (bot_decisions_dm.go).
	// Empty means the default, ON; "off" is the kill switch.
	decisionDMMode string

	// Git provider clients for /unreleased command.
	github        *GitHubClient
	gitlab        *GitLabClient
	repos         []RepoRef
	projects      []ProjectRef
	version       string
	controllerURL string
	imageConfigs  []ImageTrackConfig

	// IP whitelist management (optional).
	bouncer *Bouncer

	// Coopmux terminal link support.
	coopmuxPublicURL string // e.g., "https://gardenio.e2e.example.com/mux"

	// Beads web UI base URL for escalation links.
	beadsWebURL string // e.g., "https://gardenio.e2e.example.com/beads"

	// Read-only beads-viewer base URL (/b/<bead-id>); beadsWebURL is the daemon.
	beadsViewerURL string // e.g., "https://gardenio.e2e.example.com/beads"

	// Public URL a laptop runner registers against, quoted in the /runner
	// invite DM (GARDENIO_RUNNER_PUBLIC_URL); empty says ask the operator.
	runnerPublicURL string

	// runnerHosts caches runner id -> hostname for the laptop badge on agent
	// cards and /roster (runnerHostnames).
	runnerHosts runnerHostCache

	// operatorResolver overrides operatorFromSlackUser for /runner invite's
	// invitee and revoke's caller lookup (tests). Nil uses operatorFromSlackUser.
	operatorResolver func(ctx context.Context, slackUserID string) string

	// Operator queue channel for stale-escalation alerts (ka-0000004385).
	// Empty falls back to the default channel. Mention is an optional Slack
	// mention token rendered verbatim inside <> (e.g. "@U123", "!subteam^S123").
	escalationQueueChannel string
	escalationQueueMention string

	// In-memory decision tracking (augments StateManager).
	mu       sync.Mutex
	messages map[string]MessageRef // bead ID → Slack message ref (hot cache)
	// messageMirrors holds additional Slack messages rendering the same
	// decision beyond the primary in `messages` (decision_message_mirrors.go).
	messageMirrors map[string][]MessageRef

	// operatorIdx resolves a decision's `operated_by` to a Slack id
	// (ka-0000000024). Built on first use; guarded by mu.
	operatorIdx            *operatorSlackIndex
	agentCards             map[string]MessageRef // agent identity → status card ref (hot cache)
	agentPending           map[string]int        // agent identity → pending decision count
	agentState             map[string]string     // agent identity → last known agent_state
	agentSchedulingBlocked map[string]bool       // agent identity → pod_scheduling_blocked (true when Pending+Unschedulable)
	agentSeen              map[string]time.Time  // agent identity → last activity timestamp
	agentPodName           map[string]string     // agent identity → pod hostname (coopmux session ID)
	agentImageTag          map[string]string     // agent identity → deployed image tag
	agentRole              map[string]string     // agent identity → role (e.g., "crew", "lead", "ops")
	agentProject           map[string]string     // agent identity → project name (for channel routing)
	agentScheduleTitle     map[string]string     // agent identity → schedule bead title (for scheduled agents)
	agentSpawnedAt         map[string]time.Time  // agent identity → spawn timestamp (for duration)
	agentSpawnChannel      map[string]string     // agent identity → Slack channel where /spawn was issued
	agentSpawnInheritedBy  map[string]string     // agent identity → spawner role whose channel was inherited (absent when pinned explicitly)
	agentThreadBound       map[string]bool       // agent identity → true if spawned from a Slack thread

	// Trigger-spawned agent context (spawn_agent / apply_formula), shown on the agent card.
	agentTriggerID       map[string]string // agent identity → trigger bead id (ka-XXX)
	agentTriggerTitle    map[string]string // agent identity → trigger bead title
	agentTriggerSource   map[string]string // agent identity → trigger source ("jira", "gitlab", "schedule", …)
	agentTriggerMatchKey map[string]string // agent identity → source-specific match key (e.g. "FOO-123")
	agentSpawnVectorLine map[string]string // agent identity → pre-rendered iconified spawn-vector line (ka-0000004046)
	agentResolvedBy      map[string]string // agent identity → resolved gardener display name (ka-0000003131)
	agentOwner           map[string]string // agent identity → human owner id spawned on behalf of (ka-0000003131)

	// triggerRunCountCache: TTL cache of a trigger's fire count (beads labeled
	// "trigger:<id>") for the card's "run #N".
	triggerRunCountCache   map[string]triggerRunCountEntry
	triggerRunCountCacheMu sync.Mutex

	// TTL-cached project→primary channel mapping (refreshed every projectChannelCacheTTL).
	projectChannelCache   map[string]string // project name → primary Slack channel ID
	projectChannelCacheAt time.Time         // when cache was last refreshed

	// TTL-cached project→(role→channel) mapping, filled with
	// projectChannelCache; resolveChannel routes role-scoped agents by it.
	projectRoleChannelCache map[string]map[string]string

	// Switchback alert dedup: project+account → last alert time, so a flapping
	// primary alerts once per cooldown.
	switchbackAlertSent map[string]time.Time

	// Death-storm volume gate (ka-0000004746, bot_agent_death.go): a windowed
	// budget of individual death posts; the rest are counted (Suppressed) and
	// named (Held) for the roll-up.
	deathReportWindowStart time.Time
	deathReportPosted      int
	deathReportSuppressed  int
	deathReportHeld        []heldDeath

	threadSpawnMsgs map[string]MessageRef // agent identity → spawn confirmation message ref (for in-place update)
	beadMsgs        map[string]MessageRef // "agent:beadID" → Slack message ref for inline bead status updates
	spawnInFlight   map[string]bool       // "{channel}:{thread_ts}" → true while spawn is in progress (race guard)
	notedAt         map[string]time.Time  // noteOnce key → when its one-line reason was last posted

	// Assistant thread context: "channel:thread_ts" → the channel the user was
	// viewing; read by handleThreadSpawn for project resolution.
	assistantCtxChannel map[string]string

	// Nudge throttling for thread reply forwarding.
	// Key: "agent:thread_ts", value: last nudge time.
	lastThreadNudge map[string]time.Time

	// Default project for DM-spawned agents in multi-project setups.
	defaultDMProject string

	// Auto-spawn a gardener when invited to a channel (member_joined_channel).
	gardenerOnInvite bool

	// DM channel cache: userID → DM channel ID with TTL.
	dmChannelCache map[string]dmChannelEntry

	// Concierge mode debouncer to prevent button spam on rapid messages.
	conciergeDebouncer *conciergeDebouncer

	// Reopen debouncer: coalesces a burst of restart requests (button or
	// `@gardenio restart`) into one kill+remint (ka-0000000034).
	reopenDebouncer *reopenDebouncer

	// Commons per-member spawn rate (gd-qBvHX9jigO): new wish heads one
	// member of a tenant land may start per hour. nil admits every spawn.
	commonsSpawnRate *spawnguard.Keyed

	// Slack event dedup on event_id, so a redelivered event cannot re-nudge
	// the bound agent (ka-0000005413).
	eventDedup *slackEventDedup

	// TTL-cached concierge channel→project mapping (refreshed every projectChannelCacheTTL).
	conciergeChannelCache   map[string]string // channel ID → project name (concierge channels only)
	conciergeChannelCacheAt time.Time         // when cache was last refreshed

	// Agent streaming status manager (optional). When set, agent spawn/close
	// events in assistant threads start/stop Slack streaming messages.
	streamMgr *AgentStreamManager

	// Reaction lifecycle manager: adds/removes emoji reactions on messages
	// to reflect agent state transitions (👀 → ⏳ → ✅/❌).
	reactions *ReactionLifecycle

	// Notification suppression config (from "bridge-notifications" config beads).
	notifConfig NotificationConfig

	// Thread-cards gate, resolved by threadCardsEnabled: env mode
	// ("off" / "owned" / "all", default "off") plus the config-bead kill switch.
	threadCardsMode   string
	threadCardsConfig ThreadCardsConfig

	// Canvas creator for long-form report documents (optional).
	canvas *CanvasCreator

	// User identity directory: cross-platform identity resolution
	// (Slack ↔ GitLab ↔ GitHub ↔ email). Lazily initialized.
	userDir *UserDirectory

	// slackScopes is the bot token's OAuth scopes as Slack last reported
	// them; nil reads as none (commons_persona.go). avatars caches Slack
	// profile images for Commons posts made as their author.
	slackScopes *slackScopes
	avatars     avatarCache

	// personRoles, when set, makes Slack-initiated writes act as the mapped
	// person with their Keycloak roles (gd-7CDM9gdog0). Guarded by mu.
	personRoles PersonRoleSource

	// slackLink, when set, makes Slack-initiated writes carry the linked
	// person's own Keycloak token (gd-OZJ6H2S3xx); unlinked users get the link
	// prompt. Takes precedence over personRoles. Guarded by mu.
	slackLink *SlackLinker

	// Mention gate: only users whose user bead has the "mention" capability
	// may @mention the bot; users without a bead are denied.
	mentionGateEnabled bool

	// Keycloak role checker: when set, the mention gate checks the user's
	// Keycloak realm role instead of (or in addition to) user bead capabilities.
	keycloakChecker     *KeycloakRoleChecker
	keycloakMentionRole string

	// Slash gate: gated commands (/spawn, /kill, /decisions, /start,
	// /kill-thread) need the "slash_command" capability.
	slashGateEnabled bool

	// Bare bead-id preview cards (SLACK_BARE_ID_PREVIEW_ENABLED, default off;
	// bare_id_preview.go). bareIDPreview throttles per (channel, bead).
	bareIDPreviewEnabled bool
	bareIDPreview        *bareIDPreviewThrottle

	// linkUnfurlChannels is the channel allowlist for viewer-link unfurling
	// (SLACK_LINK_UNFURL_CHANNELS, link_unfurl.go); empty disables it.
	linkUnfurlChannels map[string]bool

	// slackEmailCache caches Slack user-id → email lookups for the rating
	// write path. Bounded by slackEmailCacheMax; entries are evicted FIFO.
	slackEmailCacheMu sync.Mutex
	slackEmailCache   map[string]slackEmailCacheEntry

	// Decision respawner: nudgeDecisionAgent's fallback when nudging a dead agent fails.
	decisionRespawner DecisionRespawner

	// DM delivery watcher for question reply handling.
	dmDelivery *DMDelivery

	// machine is the bridge's machine principal (SetMachinePrincipal). The
	// permission bead writes ride it: type:permission is admin-gated and the
	// bridge's fleet token is refused (gd-RuI4Ze6vAg).
	machine beadsapi.TokenSource

	// Per-agent card update debouncing: at most one chat.update per agent per
	// cardUpdateMinInterval (Slack allows ~50/min).
	agentCardLastUpdate map[string]time.Time // agent → last Slack API update
	agentCardDirty      map[string]bool      // agent → needs deferred update

	// Global rate limiter for Slack chat.update calls (agent card updates).
	// Caps API calls to stay under Slack's Tier 3 limit (~50 req/min).
	slackCardLimiter *rate.Limiter

	// slackRateLimitUntil pauses all card flushes after a Slack Retry-After.
	// Protected by mu.
	slackRateLimitUntil time.Time

	// Reaction-rule handling: cached rules and spawn throttle.
	reactionRuleCache *reactionRuleCache
	reactionThrottle  *reactionSpawnThrottle

	// Unified trigger:slack-reaction path, additive to the legacy rules above:
	// reaction_added events are buffered into reactionTrigger (SkipLegacy) and
	// triggerEngine evaluates them. Nil disables it.
	reactionTrigger *SlackReactionTriggerSource
	triggerEngine   *TriggerEngine

	// LLM-powered name generator for deriving short agent names and bead
	// titles from long prompt text. Nil when disabled (no API key).
	nameGen NameGenerator

	// handlerWg tracks in-flight event handlers for Run's graceful shutdown.
	handlerWg sync.WaitGroup
}

// BotConfig holds configuration for the Socket Mode bot.
type BotConfig struct {
	BotToken      string
	AppToken      string
	Channel       string
	ThreadingMode string // "agent" (default) or "flat" — controls decision threading
	// DecisionDMMode controls DM-first decision delivery: "" (default, on) or
	// "off" to restore channel-only posting. See bot_decisions_dm.go.
	DecisionDMMode string
	Daemon         BeadClient
	State          *StateManager
	Router         *Router // optional channel router; nil = all to Channel
	Logger         *slog.Logger
	Debug          bool

	// /unreleased command support.
	GitHubToken   string // DEPRECATED: use GitLabClient
	GitLabClient  *GitLabClient
	Repos         []RepoRef    // DEPRECATED: use Projects
	Projects      []ProjectRef // GitLab project paths
	Version       string
	ControllerURL string
	ImageConfigs  []ImageTrackConfig

	// Bouncer is the optional IP whitelist manager for Traefik middlewares.
	Bouncer *Bouncer

	// CoopmuxPublicURL is the coopmux dashboard base URL; when set, agent
	// names link to their terminal sessions.
	CoopmuxPublicURL string

	// RunnerPublicURL is the daemon URL a laptop runner registers against,
	// quoted in the /runner invite DM (env GARDENIO_RUNNER_PUBLIC_URL).
	RunnerPublicURL string

	// BeadsWebURL is the beads web UI base URL for escalation links.
	BeadsWebURL string

	// BeadsViewerURL is the read-only beads-viewer base URL (/b/<bead-id>);
	// not BeadsWebURL, which points at the daemon.
	BeadsViewerURL string

	// EscalationQueueChannel receives stale-escalation alerts (default 48h
	// threshold). Empty falls back to Channel (ka-0000004385).
	EscalationQueueChannel string

	// EscalationQueueMention is an optional mention token rendered inside <>
	// ("@U123ABC", "!subteam^S123ABC").
	EscalationQueueMention string

	// JiraBaseURL, when set, linkifies JIRA keys to <JiraBaseURL>/browse/<KEY>.
	JiraBaseURL string

	// JiraProjectKeys allowlists JIRA project keys; empty matches any 2–10
	// char uppercase token followed by `-<num>`.
	JiraProjectKeys []string

	// GitlabBaseURL, when set, linkifies `group/project!123` MR refs.
	GitlabBaseURL string

	// DefaultDMProject is the project for DM spawns without --project in a
	// multi-project setup; empty requires --project.
	DefaultDMProject string

	// GardenerOnInvite auto-spawns a gardener bound to a channel the bot is
	// invited to (member_joined_channel). Off by default.
	GardenerOnInvite bool

	// Notifications controls which Slack notifications are emitted
	// ("bridge-notifications" config beads).
	Notifications NotificationConfig

	// ThreadCardsMode is SLACK_THREAD_CARDS ("off" / "owned" / "all", default
	// "off"), gating cards in user-owned threads (Bot.threadCardsEnabled).
	ThreadCardsMode string

	// ThreadCards is the "bridge-thread-cards" config-bead kill switch +
	// project allow-list, layered on ThreadCardsMode.
	ThreadCards ThreadCardsConfig

	// NameGen is the optional name generator; nil falls back to the first 3 words.
	NameGen NameGenerator

	// MentionGateEnabled enables the @mention gate (Bot.mentionGateEnabled). Default off.
	MentionGateEnabled bool

	// SlashGateEnabled enables the slash command gate (Bot.slashGateEnabled). Default off.
	SlashGateEnabled bool

	// BareIDPreviewEnabled enables bare bead-id preview cards (bare_id_preview.go). Default off.
	BareIDPreviewEnabled bool

	// LinkHydrateEnabled renders outgoing bead ids as glyph + short-name links
	// plus a References footer (slack_link_hydrate.go). Default off.
	LinkHydrateEnabled bool

	// LinkUnfurlChannels is the viewer-link unfurl allowlist (link_unfurl.go);
	// empty disables unfurling.
	LinkUnfurlChannels []string

	// KeycloakChecker, with KeycloakMentionRole set, makes the mention gate
	// check a Keycloak realm role instead of user bead capabilities.
	KeycloakChecker *KeycloakRoleChecker

	// KeycloakMentionRole is the Keycloak realm role required to @mention
	// the bot (e.g. "gardenio-mention"). Only used when KeycloakChecker is set.
	KeycloakMentionRole string

	// APIURL overrides the Slack API base URL (slackmock E2E).
	APIURL string
}

// NewBot creates a new Socket Mode bot.
func NewBot(cfg BotConfig) *Bot {
	opts := []slack.Option{
		slack.OptionAppLevelToken(cfg.AppToken),
	}
	if cfg.APIURL != "" {
		opts = append(opts, slack.OptionAPIURL(cfg.APIURL))
	}
	// Hard HTTP timeout on every Slack call: slack-go sets none and some call
	// sites ignore ctx, and one hung handler stalls SSE dispatch's wg.Wait (sse.go).
	baseHTTP := &http.Client{Timeout: slackHTTPTimeout}
	// hydrator is wired to the Bot's daemon-backed lookups below, once b
	// exists; the transport only runs on Slack calls made after NewBot returns.
	var hydrator *linkHydrator
	if cfg.LinkHydrateEnabled && cfg.BeadsViewerURL != "" {
		hydrator = newLinkHydrator(cfg.BeadsViewerURL, cfg.Logger)
	}
	var httpClient slackHTTPClient = baseHTTP
	if rules := buildLinkRules(LinkifyConfig{
		BeadsViewerURL:  cfg.BeadsViewerURL,
		JiraBaseURL:     cfg.JiraBaseURL,
		JiraProjectKeys: cfg.JiraProjectKeys,
		GitlabBaseURL:   cfg.GitlabBaseURL,
	}); len(rules) > 0 {
		lt := newSlackLinkifyClient(baseHTTP, rules)
		lt.hydrate = hydrator
		httpClient = lt
	}
	// Outermost, so it sees every Web API response's X-OAuth-Scopes.
	scopes := &slackScopes{}
	opts = append(opts, slack.OptionHTTPClient(scopes.recorder(httpClient)))
	api := slack.New(cfg.BotToken, opts...)

	socket := socketmode.New(
		api,
		socketmode.OptionDebug(cfg.Debug),
	)

	var gh *GitHubClient
	if cfg.GitHubToken != "" || len(cfg.Repos) > 0 {
		gh = NewGitHubClient(cfg.GitHubToken, cfg.Logger)
	}

	b := &Bot{
		api:                    api,
		slackScopes:            scopes,
		socket:                 socket,
		state:                  cfg.State,
		daemon:                 cfg.Daemon,
		router:                 cfg.Router,
		bouncer:                cfg.Bouncer,
		logger:                 cfg.Logger,
		channel:                cfg.Channel,
		threadingMode:          cfg.ThreadingMode,
		decisionDMMode:         cfg.DecisionDMMode,
		coopmuxPublicURL:       strings.TrimRight(cfg.CoopmuxPublicURL, "/"),
		runnerPublicURL:        strings.TrimRight(cfg.RunnerPublicURL, "/"),
		beadsWebURL:            strings.TrimRight(cfg.BeadsWebURL, "/"),
		beadsViewerURL:         strings.TrimRight(cfg.BeadsViewerURL, "/"),
		escalationQueueChannel: cfg.EscalationQueueChannel,
		escalationQueueMention: cfg.EscalationQueueMention,
		defaultDMProject:       cfg.DefaultDMProject,
		gardenerOnInvite:       cfg.GardenerOnInvite,
		messages:               make(map[string]MessageRef),
		messageMirrors:         make(map[string][]MessageRef),
		agentCards:             make(map[string]MessageRef),
		agentPending:           make(map[string]int),
		agentState:             make(map[string]string),
		agentSchedulingBlocked: make(map[string]bool),
		agentSeen:              make(map[string]time.Time),
		agentPodName:           make(map[string]string),
		agentImageTag:          make(map[string]string),
		agentRole:              make(map[string]string),
		agentProject:           make(map[string]string),
		agentScheduleTitle:     make(map[string]string),
		agentSpawnedAt:         make(map[string]time.Time),
		agentSpawnChannel:      make(map[string]string),
		agentSpawnInheritedBy:  make(map[string]string),
		agentThreadBound:       make(map[string]bool),
		agentTriggerID:         make(map[string]string),
		agentTriggerTitle:      make(map[string]string),
		agentTriggerSource:     make(map[string]string),
		agentTriggerMatchKey:   make(map[string]string),
		agentSpawnVectorLine:   make(map[string]string),
		agentResolvedBy:        make(map[string]string),
		agentOwner:             make(map[string]string),
		triggerRunCountCache:   make(map[string]triggerRunCountEntry),
		threadSpawnMsgs:        make(map[string]MessageRef),
		beadMsgs:               make(map[string]MessageRef),
		agentCardLastUpdate:    make(map[string]time.Time),
		agentCardDirty:         make(map[string]bool),
		slackCardLimiter:       rate.NewLimiter(rate.Limit(40.0/60.0), 3),
		spawnInFlight:          make(map[string]bool),
		assistantCtxChannel:    make(map[string]string),
		lastThreadNudge:        make(map[string]time.Time),
		conciergeDebouncer:     newConciergeDebouncer(),
		reopenDebouncer:        newReopenDebouncer(),
		eventDedup:             newSlackEventDedup(),
		github:                 gh,
		gitlab:                 cfg.GitLabClient,
		repos:                  cfg.Repos,
		projects:               cfg.Projects,
		version:                cfg.Version,
		controllerURL:          cfg.ControllerURL,
		nameGen:                cfg.NameGen,
		imageConfigs:           cfg.ImageConfigs,
		reactions:              NewReactionLifecycle(api, cfg.Logger),
		notifConfig:            cfg.Notifications,
		threadCardsMode:        cfg.ThreadCardsMode,
		threadCardsConfig:      cfg.ThreadCards,
		canvas:                 NewCanvasCreator(api, cfg.Logger),
		mentionGateEnabled:     cfg.MentionGateEnabled,
		keycloakChecker:        cfg.KeycloakChecker,
		keycloakMentionRole:    cfg.KeycloakMentionRole,
		slashGateEnabled:       cfg.SlashGateEnabled,
		bareIDPreviewEnabled:   cfg.BareIDPreviewEnabled,
		bareIDPreview:          newBareIDPreviewThrottle(bareIDPreviewWindow),
		linkUnfurlChannels:     newStringSet(cfg.LinkUnfurlChannels),
		slackEmailCache:        make(map[string]slackEmailCacheEntry),
	}

	// Hydrate hot caches from persisted state, keyed by short agent name.
	if cfg.State != nil {
		for id, ref := range cfg.State.AllDecisionMessages() {
			b.messages[id] = ref
		}
		for id, refs := range cfg.State.AllDecisionMessageMirrors() {
			b.messageMirrors[id] = refs
		}
		for agent, ref := range cfg.State.AllAgentCards() {
			// Skip already-compressed cards: rehydrating them would make every
			// restart re-compress the whole backlog and starve live cards of
			// Slack tokens (ka-0000008156). findAgentCardByThread still reads state.
			if ref.Compressed {
				continue
			}
			b.agentCards[extractAgentName(agent)] = ref
		}
	}

	// Count pending decisions per agent from hydrated messages.
	if b.agentThreadingEnabled() {
		for _, ref := range b.messages {
			if ref.Agent != "" {
				b.agentPending[extractAgentName(ref.Agent)]++
			}
		}
	}

	// Lifecycle reactions resolve their emoji names through the channel theme.
	b.reactions.theme = b.themeFor

	// Bead-link hydration's lookups are safe inside the Slack transport: neither posts to Slack.
	if hydrator != nil {
		hydrator.fetch = b.batchGetBeads
		hydrator.theme = b.themeFor
		b.logger.Info("slack bead-link hydration enabled", "viewer_url", hydrator.viewerURL)
	}

	return b
}

// ReconcileThreadAgents restores thread→agent mappings lost on restart from
// agent beads' slack_thread_channel/slack_thread_ts fields.
func (b *Bot) ReconcileThreadAgents(ctx context.Context) {
	if b.state == nil || b.daemon == nil {
		return
	}
	agents, err := b.daemon.ListAgentBeads(ctx)
	if err != nil {
		b.logger.Warn("reconcile thread agents: failed to list agents", "error", err)
		return
	}
	var restored int
	for _, a := range agents {
		ch := a.Metadata["slack_thread_channel"]
		ts := a.Metadata["slack_thread_ts"]
		if ch == "" || ts == "" {
			continue
		}
		agentName := extractAgentName(a.AgentName)
		// Cache the thread-bound flag so card paths skip a daemon call.
		b.mu.Lock()
		b.markThreadBound(agentName)
		b.mu.Unlock()
		// Only set if not already mapped (don't overwrite fresh mappings).
		if _, ok := b.state.GetThreadAgent(ch, ts); !ok {
			_ = b.state.SetThreadAgent(ch, ts, agentName)
			restored++
		}
	}
	if restored > 0 {
		b.logger.Info("reconciled thread agents from daemon", "restored", restored)
	}
}

// API returns the underlying Slack API client for direct API calls.
func (b *Bot) API() *slack.Client {
	return b.api
}

// DefaultChannel returns the default Slack channel ID (SLACK_CHANNEL), the
// fallback for publishers without their own destination.
func (b *Bot) DefaultChannel() string {
	return b.channel
}

// SlackCardLimiter returns the shared Tier-3 limiter for the bot's
// non-terminal posts; forwarders on the bot's credentials must reuse it.
func (b *Bot) SlackCardLimiter() *rate.Limiter {
	return b.slackCardLimiter
}

// UserDirectory returns the user identity directory, created on first use
// with a Slack email resolver for SlackEmailHint (a hint only, never a mapping).
func (b *Bot) UserDirectory() *UserDirectory {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.userDir == nil {
		b.userDir = NewUserDirectory(b.daemon, b.logger)
		if b.api != nil {
			b.userDir.SetSlackEmailResolver(&slackAPIEmailResolver{api: b.api})
		}
	}
	return b.userDir
}

// slackAPIEmailResolver adapts *slack.Client to SlackEmailResolver (users.info).
type slackAPIEmailResolver struct {
	api *slack.Client
}

func (r *slackAPIEmailResolver) GetUserEmail(ctx context.Context, slackUserID string) (string, error) {
	user, err := r.api.GetUserInfoContext(ctx, slackUserID)
	if err != nil {
		return "", err
	}
	return user.Profile.Email, nil
}

// SetDMDelivery sets the watcher that checks DM thread replies for question answers.
func (b *Bot) SetDMDelivery(d *DMDelivery) {
	b.dmDelivery = d
}

// SetMachinePrincipal sets the bridge's machine principal, which carries the
// permission bead writes (writePermissionBead, closePermissionBead). nil leaves
// them on the daemon's own token, where kbeads refuses them.
func (b *Bot) SetMachinePrincipal(ts beadsapi.TokenSource) {
	b.machine = ts
}

// SetDecisionRespawner sets nudgeDecisionAgent's respawner for dead agents.
func (b *Bot) SetDecisionRespawner(r DecisionRespawner) {
	b.decisionRespawner = r
}

// SetStreamManager sets the assistant-thread streaming manager and registers
// the bot as its status setter, card refresher, poster and TTFT recorder.
func (b *Bot) SetStreamManager(mgr *AgentStreamManager) {
	b.streamMgr = mgr
	if mgr != nil {
		mgr.SetThreadStatusSetter(b)
		mgr.SetAgentCardRefresher(b)
		mgr.SetThreadMessagePoster(b)
		mgr.SetTTFTRecorder(b)
	}
}

// RecordTTFT implements TTFTRecorder: stamps boot→first-token latency as
// ttft_ms on the agent bead, once; non-positive values are ignored (ka-0000003911).
func (b *Bot) RecordTTFT(ctx context.Context, agentName string, ttftMs int64) {
	if ttftMs <= 0 || b.daemon == nil {
		return
	}
	detail, err := b.daemon.FindAgentBeadAnyStatus(ctx, extractAgentName(agentName))
	if err != nil || detail == nil {
		return
	}
	// Keep the first reading; a restart or respawn may re-run this.
	if v := parseTurnField(detail.Fields["ttft_ms"]); v > 0 {
		return
	}
	if err := b.daemon.UpdateBeadFields(ctx, detail.ID, map[string]string{
		"ttft_ms": strconv.FormatInt(ttftMs, 10),
	}); err != nil {
		b.logger.Warn("failed to record ttft_ms on agent bead",
			"agent", agentName, "ttft_ms", ttftMs, "error", err)
		return
	}
	// Refresh now so the TTFT chip appears without waiting for the next flush.
	b.updateAgentCard(ctx, agentName)
}

// RefreshAgentCard implements AgentCardRefresher (stream flush loop).
func (b *Bot) RefreshAgentCard(ctx context.Context, agentName string) {
	b.updateAgentCard(ctx, agentName)
}

// IsConnected returns the bot's connection status.
func (b *Bot) IsConnected() bool {
	return b.connected.Load()
}

// NumConnections returns the number of active socket connections.
func (b *Bot) NumConnections() int {
	return int(b.numConnections.Load())
}

// Run starts the Socket Mode event loop. Blocks until ctx is cancelled.
func (b *Bot) Run(ctx context.Context) error {
	// Get bot user ID.
	auth, err := b.api.AuthTest()
	if err != nil {
		return fmt.Errorf("Slack auth test: %w", err)
	}
	b.botUserID = auth.UserID
	b.logger.Info("Slack bot authenticated", "user_id", b.botUserID, "team", auth.Team, "team_id", auth.TeamID)

	// Streaming calls require recipient_team_id.
	if b.streamMgr != nil && auth.TeamID != "" {
		b.streamMgr.SetTeamID(auth.TeamID)
	}

	// The first prune runs in the goroutine so a stale-card backlog cannot
	// delay Socket Mode and /readyz (ka-0000000496).
	go b.startPeriodicPrune(ctx)

	// Start the card update flusher to process debounced agent card updates.
	go b.startCardUpdateFlusher(ctx)

	// Periodically clean up the concierge debouncer to prevent unbounded growth.
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				b.conciergeDebouncer.Cleanup()
				b.reopenDebouncer.Cleanup()
				b.eventDedup.Cleanup()
			}
		}
	}()

	// Drop thread-agent bindings for deleted threads or archived channels.
	go b.startPeriodicThreadValidation(ctx)

	// Dispatch Commons replies the inbound call never delivered (the bridge
	// was down when the Commons wrote them; inbound_reply.go).
	go b.startCommonsInboundSweep(ctx)

	// Refresh slack-kind operators' display/email from user beads
	// (gd-iRFGU6xSK7; docs/adr/human-attribution-identity-v2.md §7).
	go b.startSlackOperatorResync(ctx)

	// Alias a user's Slack and OIDC identities to one operator row so
	// `?operated_by=me` matches either login (ka-0000003795, ka-0000000483,
	// ka-0000007320).
	go b.startSlackOIDCOperatorAliasResync(ctx)

	// Unified trigger:slack-reaction loop; no-op unless EnableReactionTriggers ran.
	go b.runReactionTriggerLoop(ctx)

	// Replay thread replies parked in the durable DLQ (ka-0000000612).
	go b.startThreadReplyDLQDrain(ctx)

	// Stamp channel_id on open thread heads minted before the Commons
	// dual-write, and on those of lands seeded since (channel_stamp.go).
	go b.startChannelBackfill(ctx)

	// handleEvents' drain path calls handlerWg.Add(1); track its completion so
	// awaitShutdownDrain joins it before Wait (Add must happen-before Wait).
	handleEventsDone := make(chan struct{})
	go func() {
		defer close(handleEventsDone)
		b.handleEvents(ctx)
	}()

	err = b.socket.RunContext(ctx)
	b.connected.Store(false)

	b.awaitShutdownDrain(handleEventsDone)

	return err
}

// awaitShutdownDrain joins the event drain goroutine (handleEventsDone) and
// only then waits on handlerWg, since the drain calls handlerWg.Add(1) and Add
// concurrent with Wait is a WaitGroup misuse. Both waits are bounded.
func (b *Bot) awaitShutdownDrain(handleEventsDone <-chan struct{}) {
	// The drain loop is bounded by shutdownDrainTimeout; +2s is slack.
	select {
	case <-handleEventsDone:
	case <-time.After(shutdownDrainTimeout + 2*time.Second):
		b.logger.Warn("timed out waiting for event drain goroutine to finish")
	}

	// Let in-flight handlers (nudges, bead writes) finish.
	handlerDone := make(chan struct{})
	go func() {
		b.handlerWg.Wait()
		close(handlerDone)
	}()
	select {
	case <-handlerDone:
		b.logger.Info("all in-flight event handlers completed")
	case <-time.After(shutdownDrainTimeout + 5*time.Second):
		b.logger.Warn("timed out waiting for in-flight event handlers")
	}
}

// shutdownDrainTimeout bounds processing of queued Socket Mode events after
// ctx is cancelled (SIGTERM), so late thread replies are not dropped.
const shutdownDrainTimeout = 5 * time.Second

// slackHTTPTimeout bounds every Slack Web API call; it sits below the 60s SSE
// handler hctx (sse.go) so the client deadline fires first.
const slackHTTPTimeout = 30 * time.Second

// handleEvents processes Socket Mode events; after ctx is cancelled it drains
// queued events for up to shutdownDrainTimeout.
func (b *Bot) handleEvents(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			b.logger.Info("handleEvents: context cancelled, draining queued events",
				"drain_timeout", shutdownDrainTimeout)
			drainCtx, drainCancel := context.WithTimeout(context.Background(), shutdownDrainTimeout)
			defer drainCancel()
			b.drainEvents(drainCtx)
			return
		case evt, ok := <-b.socket.Events:
			if !ok {
				return
			}
			b.handleEvent(ctx, evt)
		}
	}
}

// drainEvents dispatches queued events until the channel is empty or
// drainCtx expires; handlers get drainCtx, not the cancelled parent.
func (b *Bot) drainEvents(drainCtx context.Context) {
	for {
		select {
		case <-drainCtx.Done():
			return
		case evt, ok := <-b.socket.Events:
			if !ok {
				return
			}
			b.handleEvent(drainCtx, evt)
		default:
			return
		}
	}
}

func (b *Bot) handleEvent(ctx context.Context, evt socketmode.Event) {
	b.logger.Debug("socket mode event received", "type", string(evt.Type))

	switch evt.Type {
	case socketmode.EventTypeConnecting:
		b.logger.Info("Slack Socket Mode connecting")

	case socketmode.EventTypeHello:
		if evt.Request != nil && evt.Request.NumConnections > 0 {
			b.numConnections.Store(int32(evt.Request.NumConnections))
			if evt.Request.NumConnections > 1 {
				b.logger.Warn("multiple Socket Mode connections detected",
					"num_connections", evt.Request.NumConnections)
			}
		}

	case socketmode.EventTypeConnected:
		b.connected.Store(true)
		b.logger.Info("Slack Socket Mode connected")

	case socketmode.EventTypeConnectionError:
		b.connected.Store(false)
		b.logger.Error("Slack Socket Mode connection error")

	case socketmode.EventTypeEventsAPI:
		eventsAPIEvent, ok := evt.Data.(slackevents.EventsAPIEvent)
		if !ok {
			return
		}
		// Ack first, then dedup on event_id: an in-flight redelivery or a
		// message+app_mention fan-out would otherwise re-nudge (ka-0000005413).
		b.socket.Ack(*evt.Request)
		if evt.Request != nil && b.eventDedup != nil {
			eventID := eventIDFromRequestPayload(evt.Request.Payload)
			if !b.eventDedup.MarkSeen(eventID) {
				b.logger.Debug("dropping duplicate Slack event",
					"event_id", eventID,
					"retry_attempt", evt.Request.RetryAttempt,
					"retry_reason", evt.Request.RetryReason)
				return
			}
		}
		// Detached ctx so handlers can finish after shutdown cancels the parent.
		handlerCtx, handlerCancel := context.WithTimeout(context.Background(), 30*time.Second)
		b.handlerWg.Add(1)
		go func() {
			defer b.handlerWg.Done()
			defer handlerCancel()
			b.dispatchHandler("events_api", func() {
				b.handleEventsAPI(handlerCtx, eventsAPIEvent)
			})
		}()

	case socketmode.EventTypeInteractive:
		callback, ok := evt.Data.(slack.InteractionCallback)
		if !ok {
			return
		}
		// Ack within Slack's 3s, then handle off the event loop.
		b.socket.Ack(*evt.Request)
		handlerCtx, handlerCancel := context.WithTimeout(context.Background(), 30*time.Second)
		b.handlerWg.Add(1)
		go func() {
			defer b.handlerWg.Done()
			defer handlerCancel()
			b.dispatchHandler("interactive", func() {
				b.handleInteraction(handlerCtx, callback)
			})
		}()

	case socketmode.EventTypeSlashCommand:
		cmd, ok := evt.Data.(slack.SlashCommand)
		if !ok {
			return
		}
		// slack.SlashCommand drops thread_ts; re-parse it from the raw payload.
		var threadTS string
		if evt.Request != nil {
			threadTS = parseSlashThreadTS(evt.Request.Payload)
		}
		b.socket.Ack(*evt.Request)
		handlerCtx, handlerCancel := context.WithTimeout(context.Background(), 30*time.Second)
		if threadTS != "" {
			handlerCtx = WithSlashThreadTS(handlerCtx, threadTS)
		}
		b.handlerWg.Add(1)
		go func() {
			defer b.handlerWg.Done()
			defer handlerCancel()
			b.dispatchHandler("slash_command", func() {
				b.handleSlashCommand(handlerCtx, cmd)
			})
		}()
	}
}

// handleEventsAPI processes Events API events received via Socket Mode.
func (b *Bot) handleEventsAPI(ctx context.Context, event slackevents.EventsAPIEvent) {
	switch event.Type {
	case slackevents.CallbackEvent:
		innerEvent := event.InnerEvent
		switch ev := innerEvent.Data.(type) {
		case *slackevents.MessageEvent:
			b.handleMessageEvent(ctx, ev)
		case *slackevents.AppMentionEvent:
			// In assistant threads handleAssistantMessage already handles the
			// paired MessageEvent. AssistantThread is also set on top-level
			// mentions, which have no such counterpart, so skip only in threads.
			if ev.AssistantThread != nil && ev.ThreadTimeStamp != "" {
				b.logger.Debug("skipping app_mention in assistant thread (handled by message event)",
					"channel", ev.Channel, "thread_ts", ev.ThreadTimeStamp)
				break
			}
			b.handleAppMention(ctx, ev)
		case *appHomeOpenedEvent:
			// agent_view successor to assistant_thread_started (agent_view_events.go).
			b.handleAppHomeOpened(ctx, ev)
		case *appContextChangedEvent:
			// agent_view successor to assistant_thread_context_changed.
			b.handleAppContextChanged(ctx, ev)
		case *slackevents.ReactionAddedEvent:
			b.handleReactionAdded(ctx, ev)
		case *slackevents.MemberJoinedChannelEvent:
			b.handleMemberJoinedChannel(ctx, ev)
		case *slackevents.LinkSharedEvent:
			b.handleLinkShared(ctx, ev)
		}
	}
}

// handleMessageEvent processes Slack message events (for thread replies and chat forwarding).
func (b *Bot) handleMessageEvent(ctx context.Context, ev *slackevents.MessageEvent) {
	// Ignore bot's own messages and message subtypes that are not something a
	// person said (edits, deletes, joins, etc.; see isUtteranceSubtype).
	if ev.User == b.botUserID || ev.User == "" {
		return
	}
	if !isUtteranceSubtype(ev.SubType) {
		return
	}
	// Stamp the operator (email, else `slack:<user-id>`) on downstream daemon calls.
	operator := b.operatorFromSlackUser(ctx, ev.User)
	ctx = beadsapi.WithOperator(ctx, operator)

	// Bare bead-id preview (bare_id_preview.go): additive, async, and a no-op
	// when disabled; runs for every message shape before routing.
	b.maybePreviewBareBeadIDs(ctx, ev)

	// Legacy assistant_view threads (pre-agent_view); kept for workspaces still
	// on assistant_view. It records its own smoke-check counter.
	if isAssistantThread(ev) && ev.ThreadTimeStamp != "" {
		b.handleAssistantMessage(ctx, ev)
		return
	}

	// Smoke-check "message" counter: under agent_view every genuine user DM
	// passes here. app_home_opened climbing while this stays flat means Slack
	// stopped delivering message.im (ka-0000000892,
	// docs/assistant-pane-smoke-check.md).
	if ev.ChannelType == "im" {
		b.recordAssistantEvent(assistantEventMessage)
	}

	// Thread reply handling.
	if ev.ThreadTimeStamp != "" && ev.ThreadTimeStamp != ev.TimeStamp {
		// Refresh LastActive before routing so the 24h inactivity sweep
		// spares active threads (ka-0000006280).
		if b.state != nil {
			_ = b.state.TouchThreadAgent(ev.Channel, ev.ThreadTimeStamp)
		}

		// Slice 4c (2026-10-19-lands-commons-design.md §3.4): a plain reply is
		// dispatched as Inbound{Reply}. A mention or IM reply is a compose and
		// stays on the mention path (until slice 4d); only decision resolution runs.
		in := slackReplyInbound(ev, operator)
		isMention := strings.Contains(ev.Text, fmt.Sprintf("<@%s>", b.botUserID))
		if !isMention && ev.ChannelType != "im" {
			b.dispatchInbound(ctx, in)
			// Two-way mirroring (commons_mirror.go): after the dispatch, so a
			// reply the thread forward recorded is not written twice.
			b.mirrorSlackMessage(ctx, ev, operator)
			return
		}
		b.answerSlack(ctx, in, b.resolveDecisionByReply(ctx, in))

		// Question DM replies (gb say --ask, gb dm ask; dm_replies.go) before
		// synthesizing a mention.
		if ev.ChannelType == "im" && b.captureAskReply(ctx, ev) {
			return
		}
		if ev.ChannelType == "im" && b.dmDelivery != nil {
			if b.dmDelivery.HandleReply(ctx, ev.Channel, ev.ThreadTimeStamp, ev.Text, ev.User) {
				return
			}
		}

		// group, mpim and im get no app_mention event; synthesize one.
		if isMention && (ev.ChannelType == "group" || ev.ChannelType == "mpim" || ev.ChannelType == "im") {
			b.handleAppMention(ctx, &slackevents.AppMentionEvent{
				User:            ev.User,
				Text:            ev.Text,
				TimeStamp:       ev.TimeStamp,
				ThreadTimeStamp: ev.ThreadTimeStamp,
				Channel:         ev.Channel,
				BotID:           ev.BotID,
			})
			return
		}

		// An im thread reply is directed at the bot even without a mention.
		if !isMention && ev.ChannelType == "im" {
			text := fmt.Sprintf("<@%s> %s", b.botUserID, ev.Text)
			b.handleAppMention(ctx, &slackevents.AppMentionEvent{
				User:            ev.User,
				Text:            text,
				TimeStamp:       ev.TimeStamp,
				ThreadTimeStamp: ev.ThreadTimeStamp,
				Channel:         ev.Channel,
				BotID:           ev.BotID,
			})
			return
		}

		return
	}

	// Private channels (group) get no app_mention; synthesize one.
	if ev.ChannelType == "group" && strings.Contains(ev.Text, fmt.Sprintf("<@%s>", b.botUserID)) {
		b.handleAppMention(ctx, &slackevents.AppMentionEvent{
			User:      ev.User,
			Text:      ev.Text,
			TimeStamp: ev.TimeStamp,
			Channel:   ev.Channel,
			BotID:     ev.BotID,
		})
		return
	}

	// Group DMs (mpim) get no app_mention either, but have several humans, so
	// only an explicit @mention is answered (ka-0000006051, ka-0000003709).
	if ev.ChannelType == "mpim" && strings.Contains(ev.Text, fmt.Sprintf("<@%s>", b.botUserID)) {
		b.handleAppMention(ctx, &slackevents.AppMentionEvent{
			User:      ev.User,
			Text:      ev.Text,
			TimeStamp: ev.TimeStamp,
			Channel:   ev.Channel,
			BotID:     ev.BotID,
		})
		return
	}

	// Every im message is directed at the bot.
	if ev.ChannelType == "im" {
		text := ev.Text
		// Prepend a mention so handleAppMention parses it uniformly.
		if !strings.Contains(text, fmt.Sprintf("<@%s>", b.botUserID)) {
			text = fmt.Sprintf("<@%s> %s", b.botUserID, text)
		}
		b.handleAppMention(ctx, &slackevents.AppMentionEvent{
			User:      ev.User,
			Text:      text,
			TimeStamp: ev.TimeStamp,
			Channel:   ev.Channel,
			BotID:     ev.BotID,
		})
		return
	}

	// A top-level message in a mirror=both channel becomes a message bead, so
	// the Commons shows it (commons_mirror.go). Additive, as the preview is.
	b.mirrorSlackMessage(ctx, ev, operator)

	// Concierge channels get Start/Dismiss buttons threaded under the message.
	if ev.BotID == "" { // skip other bots' messages
		if project, ok := b.conciergeChannelInfo(ctx, ev.Channel); ok {
			if b.conciergeDebouncer.Allow(ev.User, ev.Channel) {
				b.handleConciergeMessage(ctx, ev, project)
			}
			return
		}
	}

	// Other top-level non-mention messages are ignored.
}

// hintMentionRequired posts an "@mention me" hint in an agent card thread, at
// most once per thread per 10 minutes. Every path logs at Info so a throttled
// (silently dropped) reply is visible (ka-0000006280).
func (b *Bot) hintMentionRequired(ctx context.Context, channel, threadTS, agent string) {
	if b.notifConfig.SuppressMentionHint {
		b.logger.Info("hintMentionRequired: suppressed by config",
			"channel", channel, "thread_ts", threadTS, "agent", agent)
		return
	}
	key := "hint:" + channel + ":" + threadTS
	const hintInterval = 10 * time.Minute

	// Use persistent state when available.
	if b.state != nil {
		throttled, _ := b.state.CheckAndSetNudgeThrottle(key, hintInterval)
		if throttled {
			b.logger.Info("hintMentionRequired: throttled (no hint posted, message silently dropped)",
				"channel", channel, "thread_ts", threadTS, "agent", agent,
				"hint_interval", hintInterval)
			return
		}
	} else {
		// Fallback to in-memory map when state is nil.
		now := time.Now()
		b.mu.Lock()
		if last, ok := b.lastThreadNudge[key]; ok && now.Sub(last) < hintInterval {
			b.mu.Unlock()
			b.logger.Info("hintMentionRequired: throttled (no hint posted, message silently dropped)",
				"channel", channel, "thread_ts", threadTS, "agent", agent,
				"hint_interval", hintInterval, "last_hint_ago", now.Sub(last))
			return
		}
		b.lastThreadNudge[key] = now
		b.mu.Unlock()
	}

	hint := fmt.Sprintf(":bulb: _Tip: @mention me so *%s* sees your message._", agent)
	if b.api != nil {
		root := conversation.Ref{Surface: conversation.SurfaceSlack, Channel: channel, TS: threadTS}
		if _, err := b.conversationSlack().Reply(ctx, root, conversation.Message{Text: hint}, false); err != nil {
			b.logger.Warn("hintMentionRequired: failed to post hint",
				"channel", channel, "thread_ts", threadTS, "agent", agent, "error", err)
			return
		}
	}
	b.logger.Info("hintMentionRequired: posted hint",
		"channel", channel, "thread_ts", threadTS, "agent", agent)
}

// extractAgentName returns the last segment of an agent identity.
// "gardenio/crew/test-bot" → "test-bot", "test-bot" → "test-bot"
func extractAgentName(identity string) string {
	if i := strings.LastIndex(identity, "/"); i >= 0 {
		return identity[i+1:]
	}
	return identity
}

// beadTitle returns title, or id when title is empty.
func beadTitle(id, title string) string {
	if title != "" {
		return title
	}
	return id
}

// beadIDPattern whitelists bead IDs safe to embed in a URL path.
var beadIDPattern = regexp.MustCompile(`^(?:gd|ka)-[A-Za-z0-9]+$`)

// slackMrkdwnLabelEscaper escapes the three characters Slack mrkdwn treats
// specially inside a link label: `&`, `<`, and `>`.
var slackMrkdwnLabelEscaper = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
)

// beadLink returns a mrkdwn link to id's beads-viewer page labelled text, or
// the escaped label alone when there is no viewer URL or id fails beadIDPattern.
func (b *Bot) beadLink(id, text string) string {
	label := slackMrkdwnLabelEscaper.Replace(text)
	if b == nil || b.beadsViewerURL == "" {
		return label
	}
	if !beadIDPattern.MatchString(id) {
		return label
	}
	base := strings.TrimRight(b.beadsViewerURL, "/")
	return fmt.Sprintf("<%s/%s|%s>", base, id, label)
}

// beadTitleLink is beadLink labelled with beadTitle.
func (b *Bot) beadTitleLink(id, title string) string {
	return b.beadLink(id, beadTitle(id, title))
}

// formatAge formats the duration since t as a compact human-readable string.
// Examples: "3:04 PM PDT", "2m ago", "1h ago", "3d ago".
func formatAge(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return t.Local().Format("3:04 PM MST")
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// truncateText truncates s to maxLen, appending "..." if truncated.
func truncateText(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}

// resolveDecisionByReply is the decision half of HandleReply: a reply in a
// decision's Slack thread resolves it with the reply text (Slack-only until
// slice 3). A refusal comes back as the Answer's ephemeral.
func (b *Bot) resolveDecisionByReply(ctx context.Context, in conversation.Inbound) conversation.Answer {
	root, ok := in.Root(conversation.SurfaceSlack)
	if !ok || in.Surface != conversation.SurfaceSlack {
		return conversation.Answer{}
	}
	user := in.Actor.SlackUser
	// Find which decision this thread belongs to.
	beadID := b.getDecisionByThread(root.Channel, root.TS)
	if beadID == "" {
		return conversation.Answer{} // Not a decision thread we're tracking
	}
	// Stamp the operator (operatorFromSlackUser) on downstream daemon calls.
	if user != "" {
		ctx = beadsapi.WithOperator(ctx, b.operatorFromSlackUser(ctx, user))
		// Resolving is an approval, so act as the mapped person (gd-7CDM9gdog0).
		// An unmapped user's replies in a tracked decision thread are refused.
		if b.actsAsPerson() {
			actorCtx, refusal, ok := b.withSlackActor(ctx, user)
			if !ok {
				b.postSlackRefusal(ctx, root.Channel, user, "", root.TS, refusal)
				return conversation.Answer{}
			}
			ctx = actorCtx
		}
	}

	// Get user info for attribution.
	info, err := b.api.GetUserInfo(user)
	username := user
	if err == nil {
		if info.RealName != "" {
			username = info.RealName
		} else if info.Name != "" {
			username = info.Name
		}
	}

	// Try to resolve the decision with the thread reply text.
	bead, err := b.daemon.GetBead(ctx, beadID)
	if err != nil {
		b.logger.Error("failed to get decision for thread reply", "bead", beadID, "error", err)
		return conversation.Answer{}
	}

	// If decision is still open, resolve it with the reply text.
	if bead.Status == "open" || bead.Status == "in_progress" {
		// Resolve gate shared with the four button/modal paths; the policy is
		// in checkDecisionResolvePermission. Keep it wired here so a policy
		// change covers all five paths (ka-0000001906, ka-0000000024).
		if perm := b.checkDecisionResolvePermission(ctx, beadID, user); !perm.Allowed {
			b.logger.Info("decision resolve denied via thread reply: user lacks permission",
				"bead", beadID, "user", user, "owner", perm.OwnerUserID)
			msg := ":lock: This decision is for someone else — you don't have permission to respond to it."
			if perm.OwnerUserID != "" {
				msg = fmt.Sprintf(":lock: This decision is for <@%s> — you don't have permission to respond to it.", perm.OwnerUserID)
			}
			return conversation.Answer{Ephemeral: []conversation.Message{{Text: msg}}}
		}
		// A reply naming an option sets `chosen` to its id; free text goes to
		// `response_text` with `chosen` empty (ka-0000000003).
		fields := decisionResolutionFieldsFor(bead.Fields["options"], in.Text)
		fields["rationale"] = fmt.Sprintf("Thread reply by %s via Slack", username)
		if err := b.daemon.CloseBead(ctx, beadID, fields); err != nil {
			b.logger.Error("failed to resolve decision via thread reply",
				"bead", beadID, "error", err)
			return conversation.Answer{}
		}
		// Unrecorded Slack-only notice: no thread head bead until slice 3.
		_, _ = b.conversationSlack().Reply(ctx, root, conversation.Message{
			Text: fmt.Sprintf(":white_check_mark: Decision resolved by %s", username),
		}, false)
		b.logger.Info("decision resolved via thread reply",
			"bead", beadID, "user", username)
	}
	return conversation.Answer{}
}

// getDecisionByThread reverse-maps (channel, thread_ts) to a bead ID.
func (b *Bot) getDecisionByThread(channelID, threadTS string) string {
	if b.state == nil {
		return ""
	}
	for id, ref := range b.state.AllDecisionMessages() {
		if ref.ChannelID == channelID && ref.Timestamp == threadTS {
			return id
		}
	}
	return ""
}

// handleInteraction processes interactive component callbacks (buttons, modals).
// Called in its own goroutine — the Ack has already been sent by the event loop.
func (b *Bot) handleInteraction(ctx context.Context, callback slack.InteractionCallback) {
	// The link confirm is pressed by a not-yet-linked user, so it bypasses
	// withSlackActor; the linker authorizes the press itself.
	if action, ok := isSlackLinkConfirmAction(callback); ok {
		b.handleSlackLinkConfirm(ctx, action, callback)
		return
	}
	// Stamp the operator and assert the Slack user via the HMAC edge-auth
	// envelope, so ownership checks match bead.OperatedBy (ka-0000007637). With
	// a PersonRoleSource an unmapped user is refused (gd-7CDM9gdog0).
	if callback.User.ID != "" {
		actorCtx, refusal, ok := b.withSlackActor(ctx, callback.User.ID)
		if !ok {
			b.refuseSlackUser(ctx, callback, refusal)
			return
		}
		ctx = actorCtx
	}
	switch callback.Type {
	case slack.InteractionTypeBlockActions:
		b.handleBlockActions(ctx, callback)

	case slack.InteractionTypeViewSubmission:
		b.handleViewSubmission(ctx, callback)

	default:
		b.logger.Debug("unhandled interaction type", "type", callback.Type)
	}
}

// lookupMessage finds a tracked decision message by bead ID.
// Checks hot cache first, then falls back to state manager.
func (b *Bot) lookupMessage(beadID string) (MessageRef, bool) {
	b.mu.Lock()
	ref, ok := b.messages[beadID]
	b.mu.Unlock()
	if ok {
		return ref, true
	}
	if b.state != nil {
		return b.state.GetDecisionMessage(beadID)
	}
	return MessageRef{}, false
}

// updateMessageResolved rewrites the decision's Slack message as resolved,
// locating it by channelID/messageTS, else the hot cache, else state.
func (b *Bot) updateMessageResolved(ctx context.Context, beadID, chosen, rationale, channelID, messageTS string) {
	// Try hot cache first.
	if messageTS == "" {
		b.mu.Lock()
		if ref, ok := b.messages[beadID]; ok {
			messageTS = ref.Timestamp
			channelID = ref.ChannelID
		}
		b.mu.Unlock()
	}
	// Fall back to state manager.
	if messageTS == "" && b.state != nil {
		if ref, ok := b.state.GetDecisionMessage(beadID); ok {
			messageTS = ref.Timestamp
			channelID = ref.ChannelID
		}
	}

	if messageTS == "" || channelID == "" {
		b.logger.Debug("no Slack message found for resolved decision", "bead", beadID)
		return
	}

	text := fmt.Sprintf(":white_check_mark: *Resolved*: %s", chosen)
	if rationale != "" {
		text += fmt.Sprintf("\n_%s_", rationale)
	}

	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", text, false, false),
			nil, nil,
		),
	}

	_, _, _, err := b.api.UpdateMessageContext(ctx, channelID, messageTS,
		slack.MsgOptionText(fmt.Sprintf("Decision resolved: %s", chosen), false),
		slack.MsgOptionBlocks(blocks...),
	)
	if err != nil {
		b.logger.Error("failed to update Slack message", "bead", beadID, "error", err)
		return
	}

	// Decrement the pending count once: clearing Agent stops the later SSE
	// close from decrementing again; the ref stays for PostReport threading.
	b.mu.Lock()
	ref, hadRef := b.messages[beadID]
	agent := extractAgentName(ref.Agent)
	if hadRef && b.agentThreadingEnabled() && agent != "" {
		if b.agentPending[agent] > 0 {
			b.agentPending[agent]--
		}
		ref.Agent = ""
		b.messages[beadID] = ref
	}
	b.mu.Unlock()

	// Persist with Agent cleared so a restart does not re-inflate the count.
	if b.state != nil && hadRef {
		persistRef := ref
		persistRef.Agent = ""
		_ = b.state.SetDecisionMessage(beadID, persistRef)
	}

	if hadRef && b.agentThreadingEnabled() && agent != "" {
		b.updateAgentCard(ctx, agent)
	}

	// Rewrite every mirror too (decision_message_mirrors.go), or it stays open forever.
	b.updateMirrorsResolved(ctx, beadID, chosen, rationale)
}

// agentDisplayName returns the agent's short name as plain text; the
// clickable link belongs only on the agent card (ka-0000007955). Thread-safe.
func (b *Bot) agentDisplayName(agent string) string {
	return extractAgentName(agent)
}

// agentThreadLink returns the plain word "agent" for thread replies; no link
// (see agentDisplayName, ka-0000007955).
func (b *Bot) agentThreadLink(agent string) string {
	return "agent"
}

// agentCardHref is the agent card's name link (GL-13, G-R2, step 6.8): the
// Glass <viewerURL>/garden?glass=<agent bead> when known, else the coopmux
// <coopmuxURL>#<pod> (G-D6). No pod means no link ("").
func agentCardHref(viewerURL, agentID, coopmuxURL, podName string) string {
	switch {
	case podName == "":
		return ""
	case viewerURL != "" && beadIDPattern.MatchString(agentID):
		return viewerURL + "/garden?glass=" + agentID
	case coopmuxURL != "":
		return coopmuxURL + "#" + podName
	}
	return ""
}

// agentCardLink returns a mrkdwn link to href labelled agentName, or the
// plain name when href is "". Agent card only (ka-0000007955).
func agentCardLink(href, agentName string) string {
	if href != "" {
		return fmt.Sprintf("<%s|%s>", href, agentName)
	}
	return agentName
}

// Ensure Bot implements Notifier, AgentNotifier, JackNotifier, BeadActivityNotifier, and RotationNotifier.
var _ Notifier = (*Bot)(nil)
var _ AgentNotifier = (*Bot)(nil)
var _ JackNotifier = (*Bot)(nil)
var _ BeadActivityNotifier = (*Bot)(nil)
var _ RotationNotifier = (*Bot)(nil)
