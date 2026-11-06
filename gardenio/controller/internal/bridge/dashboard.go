// Package bridge provides the live agent activity dashboard.
//
// Dashboard posts and periodically updates a single pinned Slack message
// showing agent roster status: working, idle, and dead agents, plus pending
// decisions. Content hashing prevents redundant Slack API calls when nothing
// has changed. The dashboard message is pinned so it survives pod restarts
// (the pin is used as a recovery mechanism when local state is lost).
package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/prhealth"

	"github.com/slack-go/slack"
)

// DashboardConfig holds configuration for the agent activity dashboard.
type DashboardConfig struct {
	Enabled   bool
	ChannelID string        // Target channel (falls back to bot default).
	Interval  time.Duration // Poll interval (default 15s).

	MaxWorkingShown   int // Max working agents to display (default 10).
	MaxIdleShown      int // Max idle agents to display (default 5).
	MaxDeadShown      int // Max dead agents to display (default 5).
	MaxDecisionsShown int // Max pending decisions to display (default 5).

	// SuppressCost hides cost data from the dashboard when true.
	SuppressCost bool
}

// dashboardMessage tracks the posted dashboard message for later updates.
type dashboardMessage struct {
	ChannelID string
	Timestamp string
	LastHash  string
}

// dashboardMarker is embedded in every dashboard message so we can identify
// our pinned message after a pod restart when local state is lost.
const dashboardMarker = "*Agent Dashboard*"

// Dashboard manages the live agent activity dashboard: a single pinned Slack
// message that is periodically updated with the current agent roster.
type Dashboard struct {
	api    *slack.Client
	daemon *beadsapi.Client
	state  *StateManager
	logger *slog.Logger
	cfg    DashboardConfig

	mu          sync.Mutex
	msg         *dashboardMessage
	lastUpdate  time.Time
	dirty       bool
	minInterval time.Duration

	// agentFilter caches which agents belong to the dashboard channel based
	// on project membership and per-role channel routing. nil means the cache
	// is stale and needs refreshing.
	agentFilter   *dashboardFilter
	agentFilterAt time.Time

	// themes caches the dashboard channel's card theme (theme.go themeCache).
	themes themeCache

	// openPRs is the PR sweeper's last open-PR record, re-read every
	// openPRRefresh (openPRLine). Only the Run goroutine touches it.
	openPRs   *prhealth.Health
	openPRsAt time.Time
}

// openPRRefresh is how often the dashboard re-reads the PR sweeper's record.
// The sweeper writes it once an hour.
const openPRRefresh = 5 * time.Minute

// dashboardFilter determines which agents belong to the dashboard channel.
//
// An agent is included if BOTH:
//   - its project is in `projects` (or the agent has no project), AND
//   - its (project, role) is NOT in `excludedRoles`.
//
// `excludedRoles[project][role] = true` means agents with that (project, role)
// have a project-level channel_roles mapping that routes them to a different
// channel — so they should appear only in that channel's dashboard, not here.
type dashboardFilter struct {
	projects      map[string]bool
	excludedRoles map[string]map[string]bool
}

// NewDashboard creates a Dashboard that posts and updates a roster message.
func NewDashboard(api *slack.Client, daemon *beadsapi.Client, state *StateManager, logger *slog.Logger, cfg DashboardConfig) *Dashboard {
	if cfg.ChannelID == "" {
		logger.Warn("dashboard: no channel configured, disabled")
		cfg.Enabled = false
	}
	return &Dashboard{
		api:         api,
		daemon:      daemon,
		state:       state,
		logger:      logger,
		cfg:         cfg,
		minInterval: 3 * time.Second,
	}
}

// Run starts the periodic dashboard update loop. Blocks until ctx is cancelled.
func (d *Dashboard) Run(ctx context.Context) {
	if !d.cfg.Enabled {
		return
	}
	interval := d.cfg.Interval
	if interval == 0 {
		interval = 15 * time.Second
	}

	// Jitter on startup to avoid thundering herd.
	jitter := time.Duration(rand.IntN(5000)) * time.Millisecond
	select {
	case <-ctx.Done():
		return
	case <-time.After(jitter):
	}

	d.initMessage(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.refresh(ctx)
		}
	}
}

// MarkDirty signals that the dashboard should refresh on the next cycle.
func (d *Dashboard) MarkDirty() {
	d.mu.Lock()
	d.dirty = true
	d.mu.Unlock()
}

// RegisterHandlers registers SSE event handlers to mark the dashboard dirty
// on agent lifecycle events (create/close/update of agent beads).
func (d *Dashboard) RegisterHandlers(stream *SSEStream) {
	handler := func(_ context.Context, data []byte) {
		bead := ParseBeadEvent(data)
		if bead == nil {
			return
		}
		if bead.Type == "agent" || bead.Type == "decision" {
			d.MarkDirty()
		}
	}
	stream.On("beads.bead.created", handler)
	stream.On("beads.bead.closed", handler)
	stream.On("beads.bead.updated", handler)
	d.logger.Info("dashboard registered SSE handlers")
}

// projectFilterCacheTTL controls how often the dashboard re-fetches project
// beads to determine which projects belong to the dashboard channel.
const projectFilterCacheTTL = 60 * time.Second

// refreshAgentFilter resolves which agents should appear in the dashboard
// based on channel association and per-role channel routing.
//
// A project belongs to the dashboard channel if:
//   - its slack_channel list includes the dashboard channel, OR
//   - it has no slack_channel configured (falls through to bot default)
//
// Within an included project, role agents are excluded if the project's
// channel_roles maps that role exclusively to channels other than this one.
// This keeps role-specific agents (e.g. fsm-triage) out of sibling channel
// dashboards even when the project is monitored by both channels.
func (d *Dashboard) refreshAgentFilter(ctx context.Context) *dashboardFilter {
	d.mu.Lock()
	if d.agentFilter != nil && time.Since(d.agentFilterAt) < projectFilterCacheTTL {
		f := d.agentFilter
		d.mu.Unlock()
		return f
	}
	d.mu.Unlock()

	projects, err := d.daemon.ListProjectBeads(ctx)
	if err != nil {
		d.logger.Warn("dashboard: failed to list projects for filter", "error", err)
		// Return nil = show all (no filter applied).
		return nil
	}

	f := buildDashboardFilter(projects, d.cfg.ChannelID)

	d.mu.Lock()
	d.agentFilter = f
	d.agentFilterAt = time.Now()
	d.mu.Unlock()

	return f
}

// buildDashboardFilter computes the project membership and role exclusion
// sets for a dashboard targeting channelID. Pure function for testability.
func buildDashboardFilter(projects map[string]beadsapi.ProjectInfo, channelID string) *dashboardFilter {
	f := &dashboardFilter{
		projects:      make(map[string]bool, len(projects)),
		excludedRoles: make(map[string]map[string]bool),
	}
	for name, info := range projects {
		if len(info.SlackChannels) != 0 && !info.HasChannel(channelID) {
			continue
		}
		f.projects[name] = true

		// Aggregate channel_roles into role → channels mapping. A role with at
		// least one channel matching this dashboard is allowed here; a role
		// whose channels are all elsewhere is excluded. Each channel's role
		// list is flattened so many-role-per-channel entries (ka-0000006075)
		// still let every mapped role's agents surface in that channel's
		// dashboard.
		roleChannels := make(map[string][]string, len(info.ChannelRoles))
		for ch, roles := range info.ChannelRoles {
			for _, role := range roles {
				if role == "" {
					continue
				}
				roleChannels[role] = append(roleChannels[role], ch)
			}
		}
		for role, chs := range roleChannels {
			allowedHere := false
			for _, ch := range chs {
				if ch == channelID {
					allowedHere = true
					break
				}
			}
			if !allowedHere {
				if f.excludedRoles[name] == nil {
					f.excludedRoles[name] = make(map[string]bool)
				}
				f.excludedRoles[name][role] = true
			}
		}
	}
	return f
}

// filterAgents removes agents that don't belong to the dashboard channel,
// based on project membership and per-role channel routing. Thread-bound
// agents (spawned from Slack threads) are always excluded — they have their
// own streaming status inside the thread and should not appear as top-level
// entries in the dashboard.
func filterAgents(agents []beadsapi.AgentBead, f *dashboardFilter) []beadsapi.AgentBead {
	if f == nil {
		return agents
	}
	filtered := make([]beadsapi.AgentBead, 0, len(agents))
	for _, a := range agents {
		// Exclude thread-bound agents — they live inside their spawning thread.
		if isThreadBoundAgent(a) {
			continue
		}
		if a.Project != "" && !f.projects[a.Project] {
			continue
		}
		if excl := f.excludedRoles[a.Project]; excl != nil && excl[a.Role] {
			continue
		}
		filtered = append(filtered, a)
	}
	return filtered
}

// isThreadBoundAgent returns true if the agent was spawned from a Slack thread.
// Thread-bound agents have slack_thread_channel/ts metadata or a spawn_source
// starting with "slack-thread".
func isThreadBoundAgent(a beadsapi.AgentBead) bool {
	if a.Metadata["slack_thread_channel"] != "" && a.Metadata["slack_thread_ts"] != "" {
		return true
	}
	return strings.HasPrefix(a.Metadata["spawn_source"], "slack-thread")
}

// --- Rendering ---

// accountCostData holds aggregated cost data from LiteLLM spend tracking
// across all account beads. This is the authoritative source of cost data.
type accountCostData struct {
	TotalUSD  float64
	DailyUSD  float64
	WeeklyUSD float64
}

// fetchAccountCosts aggregates cost data from account beads. The spend_sync
// controller populates these fields from LiteLLM /key/info and /spend/logs.
func (d *Dashboard) fetchAccountCosts(ctx context.Context) accountCostData {
	accounts, err := d.daemon.ListAccountBeads(ctx)
	if err != nil {
		d.logger.Debug("dashboard: failed to list accounts for cost", "error", err)
		return accountCostData{}
	}

	// spend_sync stamps the per-key /key/info total onto EVERY account bead that
	// shares a litellm_key, so two distinct accounts on one key (e.g. litellm-bedrock
	// + glm-bedrock, ka-0000001340) each report the same total/daily/weekly cost.
	// Summing raw per-account would double-count that shared bill. Book each virtual
	// key's cost once by folding on litellm_key; accounts with an empty key are kept
	// distinct (each contributes) so single-key accounts are unaffected.
	seenKeys := make(map[string]struct{})
	var data accountCostData
	for _, acct := range accounts {
		if key := acct.LiteLLMKey; key != "" {
			if _, dup := seenKeys[key]; dup {
				continue
			}
			seenKeys[key] = struct{}{}
		}
		if v, err := strconv.ParseFloat(acct.Fields["total_cost_usd"], 64); err == nil {
			data.TotalUSD += v
		}
		if v, err := strconv.ParseFloat(acct.Fields["daily_cost_usd"], 64); err == nil {
			data.DailyUSD += v
		}
		if v, err := strconv.ParseFloat(acct.Fields["weekly_cost_usd"], 64); err == nil {
			data.WeeklyUSD += v
		}
	}
	return data
}

// buildBlocks fetches the current roster + decisions and renders Block Kit blocks.
// Returns the blocks and a content hash for change detection.
func (d *Dashboard) buildBlocks(ctx context.Context) ([]slack.Block, string, error) {
	agents, err := d.daemon.ListAgentBeads(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("listing agents: %w", err)
	}
	decisions, err := d.daemon.ListDecisionBeads(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("listing decisions: %w", err)
	}

	// Fetch account-level cost data from LiteLLM spend tracking.
	var costs accountCostData
	if !d.cfg.SuppressCost {
		costs = d.fetchAccountCosts(ctx)
	}

	// Filter agents to only those belonging to projects associated with
	// the dashboard channel. Projects with their own dedicated Slack
	// channels are excluded — their agents appear in their own channel.
	// Within an included project, agents whose role is routed to a different
	// channel via channel_roles are also excluded.
	filter := d.refreshAgentFilter(ctx)
	agents = filterAgents(agents, filter)

	// Also filter decisions to only those from visible agents.
	if filter != nil {
		agentIDs := make(map[string]bool, len(agents))
		for _, a := range agents {
			agentIDs[a.ID] = true
		}
		filtered := make([]*beadsapi.BeadDetail, 0, len(decisions))
		for _, dec := range decisions {
			agentID := dec.Fields["requesting_agent_bead_id"]
			if agentID == "" || agentIDs[agentID] {
				filtered = append(filtered, dec)
			}
		}
		decisions = filtered
	}

	d.refreshOpenPRs(ctx)
	blocks, hash := d.renderBlocks(d.theme(ctx), agents, decisions, costs)
	return blocks, hash, nil
}

// refreshOpenPRs re-reads the PR sweeper's last record (internal/prhealth) at
// most every openPRRefresh. A sweeper run that has not written yet, or a failed
// read, keeps the last record, so the line does not blink out for the part of
// each hour a run is going.
func (d *Dashboard) refreshOpenPRs(ctx context.Context) {
	if d.daemon == nil || (!d.openPRsAt.IsZero() && time.Since(d.openPRsAt) < openPRRefresh) {
		return
	}
	d.openPRsAt = time.Now()
	h, err := prhealth.Latest(ctx, d.daemon)
	if err != nil {
		d.logger.Warn("dashboard: failed to read the PR sweeper's open-PR figures", "error", err)
		return
	}
	if h != nil {
		d.openPRs = h
	}
}

// formatOpenPRLine is the dashboard's open-PR line (plan gd-3F4W22h2nT step
// 10), or "" before the PR sweeper has recorded a run.
func formatOpenPRLine(th Theme, h *prhealth.Health) string {
	if h == nil {
		return ""
	}
	return th.Semantic(SemReview) + " " + h.Line()
}

// theme resolves the card theme of the dashboard channel's owning project, the
// same channel -> project -> card_theme lookup as Bot.themeFor (the dashboard
// holds the daemon client, not the Bot). Garden when the channel has no
// resolvable project or the listing fails.
func (d *Dashboard) theme(ctx context.Context) Theme {
	now := time.Now()
	if t, ok := d.themes.get(d.cfg.ChannelID, now); ok {
		return t
	}
	projects, err := d.daemon.ListProjectBeads(ctx)
	if err != nil {
		d.logger.Error("dashboard: failed to list projects for theme", "channel", d.cfg.ChannelID, "error", err)
		return Garden
	}
	theme := Garden
	if project, conflicts := beadsapi.ProjectForChannel(projects, d.cfg.ChannelID); len(conflicts) == 0 && project != "" {
		theme = themeByName(projects[project].CardTheme)
	}
	d.themes.put(d.cfg.ChannelID, theme, now)
	return theme
}

func (d *Dashboard) renderBlocks(th Theme, agents []beadsapi.AgentBead, decisions []*beadsapi.BeadDetail, costs accountCostData) ([]slack.Block, string) {
	cfg := d.cfg
	if cfg.MaxWorkingShown == 0 {
		cfg.MaxWorkingShown = 10
	}
	if cfg.MaxIdleShown == 0 {
		cfg.MaxIdleShown = 5
	}
	if cfg.MaxDeadShown == 0 {
		cfg.MaxDeadShown = 5
	}
	if cfg.MaxDecisionsShown == 0 {
		cfg.MaxDecisionsShown = 5
	}

	// Build a pending-decision count per agent bead ID.
	pendingByAgent := make(map[string]int, len(decisions))
	for _, dec := range decisions {
		if agentID := dec.Fields["requesting_agent_bead_id"]; agentID != "" {
			pendingByAgent[agentID]++
		}
	}

	// Classify agents into four buckets.
	var working, starting, idle, dead []beadsapi.AgentBead
	for _, a := range agents {
		switch {
		case a.AgentState == "failed" || a.PodPhase == "failed":
			dead = append(dead, a)
		case a.AgentState == "done":
			dead = append(dead, a)
		case a.AgentState == "rate_limited":
			dead = append(dead, a)
		case a.AgentState == "working":
			working = append(working, a)
		case a.AgentState == "spawning":
			starting = append(starting, a)
		default:
			idle = append(idle, a)
		}
	}

	// Sort: working by project/name, others by name.
	sort.Slice(working, func(i, j int) bool {
		if working[i].Project != working[j].Project {
			return working[i].Project < working[j].Project
		}
		return working[i].AgentName < working[j].AgentName
	})
	sort.Slice(starting, func(i, j int) bool { return starting[i].AgentName < starting[j].AgentName })
	sort.Slice(idle, func(i, j int) bool { return idle[i].AgentName < idle[j].AgentName })
	sort.Slice(dead, func(i, j int) bool { return dead[i].AgentName < dead[j].AgentName })

	var blocks []slack.Block

	// Header.
	headerText := fmt.Sprintf("%s %s · %d agents · Updated %s",
		th.Semantic(SemReport), dashboardMarker, len(agents), time.Now().UTC().Format("15:04 UTC"))
	blocks = append(blocks, slack.NewSectionBlock(
		slack.NewTextBlockObject("mrkdwn", headerText, false, false), nil, nil))

	// Summary counters.
	summaryText := fmt.Sprintf("%s %d working  ·  %s %d idle  ·  %s %d stopped",
		th.Glyph(LifecycleWorking), len(working), th.Semantic(SemIdle), len(idle), th.Semantic(SemStop), len(dead))
	if len(starting) > 0 {
		summaryText += fmt.Sprintf("  ·  %s %d starting", th.Glyph(LifecycleSpawning), len(starting))
	}
	if len(decisions) > 0 {
		summaryText += fmt.Sprintf("  ·  %s %d pending decisions", th.Glyph(LifecycleNeedsDecision), len(decisions))
	}
	if !d.cfg.SuppressCost {
		if costLine := formatFleetCostLine(th, costs); costLine != "" {
			summaryText += "  ·  " + costLine
		}
	}
	blocks = append(blocks, slack.NewContextBlock("",
		slack.NewTextBlockObject("mrkdwn", summaryText, false, false)))
	openPRLine := formatOpenPRLine(th, d.openPRs)
	if openPRLine != "" {
		blocks = append(blocks, slack.NewContextBlock("",
			slack.NewTextBlockObject("mrkdwn", openPRLine, false, false)))
	}

	// Working section.
	if len(working) > 0 {
		blocks = append(blocks, slack.NewDividerBlock())
		blocks = append(blocks, slack.NewContextBlock("",
			slack.NewTextBlockObject("mrkdwn", fmt.Sprintf("*Working (%d)*", len(working)), false, false)))

		shown := working
		if len(shown) > cfg.MaxWorkingShown {
			shown = shown[:cfg.MaxWorkingShown]
		}
		for _, a := range shown {
			blocks = append(blocks, dashboardAgentWorkingBlock(th, a, cfg.SuppressCost))
		}
		if overflow := len(working) - cfg.MaxWorkingShown; overflow > 0 {
			blocks = append(blocks, slack.NewContextBlock("",
				slack.NewTextBlockObject("mrkdwn",
					fmt.Sprintf("_+%d more working agents_", overflow), false, false)))
		}
	}

	// Starting section.
	if len(starting) > 0 {
		blocks = append(blocks, slack.NewDividerBlock())
		blocks = append(blocks, slack.NewContextBlock("",
			slack.NewTextBlockObject("mrkdwn", fmt.Sprintf("*Starting (%d)*", len(starting)), false, false)))

		shown := starting
		if len(shown) > cfg.MaxIdleShown {
			shown = shown[:cfg.MaxIdleShown]
		}
		for _, a := range shown {
			blocks = append(blocks, dashboardAgentStartingBlock(th, a, cfg.SuppressCost))
		}
		if overflow := len(starting) - cfg.MaxIdleShown; overflow > 0 {
			blocks = append(blocks, slack.NewContextBlock("",
				slack.NewTextBlockObject("mrkdwn",
					fmt.Sprintf("_+%d more starting agents_", overflow), false, false)))
		}
	}

	// Idle section.
	if len(idle) > 0 {
		blocks = append(blocks, slack.NewDividerBlock())
		blocks = append(blocks, slack.NewContextBlock("",
			slack.NewTextBlockObject("mrkdwn", fmt.Sprintf("*Idle (%d)*", len(idle)), false, false)))

		shown := idle
		if len(shown) > cfg.MaxIdleShown {
			shown = shown[:cfg.MaxIdleShown]
		}
		for _, a := range shown {
			blocks = append(blocks, dashboardAgentIdleBlock(th, a, pendingByAgent[a.ID], cfg.SuppressCost))
		}
		if overflow := len(idle) - cfg.MaxIdleShown; overflow > 0 {
			blocks = append(blocks, slack.NewContextBlock("",
				slack.NewTextBlockObject("mrkdwn",
					fmt.Sprintf("_+%d more idle agents_", overflow), false, false)))
		}
	}

	// Dead section.
	if len(dead) > 0 {
		blocks = append(blocks, slack.NewDividerBlock())
		blocks = append(blocks, slack.NewContextBlock("",
			slack.NewTextBlockObject("mrkdwn", fmt.Sprintf("*Stopped (%d)*", len(dead)), false, false)))

		shown := dead
		if len(shown) > cfg.MaxDeadShown {
			shown = shown[:cfg.MaxDeadShown]
		}
		for _, a := range shown {
			blocks = append(blocks, dashboardAgentDeadBlock(th, a, cfg.SuppressCost))
		}
		if overflow := len(dead) - cfg.MaxDeadShown; overflow > 0 {
			blocks = append(blocks, slack.NewContextBlock("",
				slack.NewTextBlockObject("mrkdwn",
					fmt.Sprintf("_+%d more dead agents_", overflow), false, false)))
		}
	}

	// Pending decisions section.
	if len(decisions) > 0 {
		blocks = append(blocks, slack.NewDividerBlock())
		blocks = append(blocks, slack.NewContextBlock("",
			slack.NewTextBlockObject("mrkdwn",
				fmt.Sprintf("*Pending Decisions (%d)*", len(decisions)), false, false)))

		shown := decisions
		if len(shown) > cfg.MaxDecisionsShown {
			shown = shown[:cfg.MaxDecisionsShown]
		}
		var lines []string
		for _, dec := range shown {
			question := dec.Fields["question"]
			if question == "" {
				question = dec.Title
			}
			if len(question) > 60 {
				question = question[:57] + "..."
			}
			urgency := th.Glyph(LifecycleNeedsDecision)
			for _, label := range dec.Labels {
				if label == "escalated" {
					urgency = th.BeadType["escalation"]
					break
				}
			}
			line := fmt.Sprintf("%s `%s` %s", urgency, dec.ID, question)
			if dec.Assignee != "" {
				// Plain agent name (no coopmux link): the clickable terminal link
				// belongs only on the agent card (ka-0000007955).
				agentName := extractAgentName(dec.Assignee)
				line = fmt.Sprintf("%s `%s` · *%s* · %s", urgency, dec.ID, agentName, question)
			}
			lines = append(lines, line)
		}
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", strings.Join(lines, "\n"), false, false), nil, nil))

		if overflow := len(decisions) - cfg.MaxDecisionsShown; overflow > 0 {
			blocks = append(blocks, slack.NewContextBlock("",
				slack.NewTextBlockObject("mrkdwn",
					fmt.Sprintf("_+%d more pending decisions_", overflow), false, false)))
		}
	}

	hash := buildDashboardHash(agents, decisions)
	if openPRLine != "" {
		hash += "|pr:" + openPRLine
	}
	return blocks, hash
}

// dashboardAgentWorkingBlock renders the agent's plain name (no coopmux link):
// the clickable terminal link belongs only on the agent card (ka-0000007955).
func dashboardAgentWorkingBlock(th Theme, a beadsapi.AgentBead, suppressCost bool) slack.Block {
	line := fmt.Sprintf("%s *%s*", th.Glyph(LifecycleWorking), a.AgentName)
	if a.Project != "" {
		line += fmt.Sprintf(" · %s", a.Project)
	}
	if a.Role != "" {
		line += fmt.Sprintf(" (%s/%s)", a.Mode, a.Role)
	}
	if !suppressCost {
		if costLine := formatCostSummary(a.Metadata); costLine != "" {
			line += fmt.Sprintf("\n%s", costLine)
		}
		// Turns + context (ka-0000003171). Liveness comes from the shared
		// agentStateIsLive predicate rather than the section this block renders in,
		// so every surface agrees about the same agent_state.
		if ct := formatAgentContextTurns(th, a.Metadata, agentStateIsLive(a.AgentState)); ct != "" {
			line += fmt.Sprintf("\n%s", ct)
		}
	}
	return slack.NewSectionBlock(
		slack.NewTextBlockObject("mrkdwn", line, false, false), nil, nil)
}

func dashboardAgentStartingBlock(th Theme, a beadsapi.AgentBead, suppressCost bool) slack.Block {
	line := fmt.Sprintf("%s *%s*", th.Glyph(LifecycleSpawning), a.AgentName)
	if a.Project != "" {
		line += fmt.Sprintf(" · %s", a.Project)
	}
	if !suppressCost {
		if costLine := formatCostSummary(a.Metadata); costLine != "" {
			line += fmt.Sprintf("\n%s", costLine)
		}
		// Turns + context (ka-0000003171). Liveness comes from the shared
		// agentStateIsLive predicate rather than the section this block renders in,
		// so every surface agrees about the same agent_state.
		if ct := formatAgentContextTurns(th, a.Metadata, agentStateIsLive(a.AgentState)); ct != "" {
			line += fmt.Sprintf("\n%s", ct)
		}
	}
	return slack.NewSectionBlock(
		slack.NewTextBlockObject("mrkdwn", line, false, false), nil, nil)
}

// dashboardAgentIdleBlock renders the agent's plain name (no coopmux link):
// the clickable terminal link belongs only on the agent card (ka-0000007955).
func dashboardAgentIdleBlock(th Theme, a beadsapi.AgentBead, pendingCount int, suppressCost bool) slack.Block {
	var indicator, suffix string
	if pendingCount > 0 {
		indicator = th.Glyph(LifecycleNeedsDecision)
		suffix = fmt.Sprintf(" · %d pending", pendingCount)
	} else {
		indicator = th.Semantic(SemIdle)
	}
	line := fmt.Sprintf("%s *%s*", indicator, a.AgentName)
	if a.Project != "" {
		line += fmt.Sprintf(" · %s", a.Project)
	}
	line += suffix
	if !suppressCost {
		if costLine := formatCostSummary(a.Metadata); costLine != "" {
			line += fmt.Sprintf("\n%s", costLine)
		}
		// Turns + context (ka-0000003171). The idle bucket is the dashboard's
		// catch-all `default` branch, so it holds more than just agent_state=idle —
		// read liveness from the state itself rather than assuming the section.
		if ct := formatAgentContextTurns(th, a.Metadata, agentStateIsLive(a.AgentState)); ct != "" {
			line += fmt.Sprintf("\n%s", ct)
		}
	}
	return slack.NewSectionBlock(
		slack.NewTextBlockObject("mrkdwn", line, false, false), nil, nil)
}

// dashboardAgentDeadBlock renders the agent's plain name (no coopmux link):
// the clickable terminal link belongs only on the agent card (ka-0000007955).
func dashboardAgentDeadBlock(th Theme, a beadsapi.AgentBead, suppressCost bool) slack.Block {
	state := a.AgentState
	if state == "" {
		state = a.PodPhase
	}
	var emoji string
	switch state {
	case "done":
		emoji = th.Glyph(LifecycleDone)
	case "rate_limited":
		emoji = th.Semantic(SemWarn)
	default:
		emoji = th.Glyph(LifecycleAborted)
	}
	line := fmt.Sprintf("%s *%s*", emoji, a.AgentName)
	if a.Project != "" {
		line += fmt.Sprintf(" · %s", a.Project)
	}
	line += fmt.Sprintf(" · %s", state)
	if !suppressCost {
		if costLine := formatCostSummary(a.Metadata); costLine != "" {
			line += fmt.Sprintf("\n%s", costLine)
		}
		// Turns + context for a stopped agent, matching the agent card's
		// completed-run rollup (ka-0000003171). formatCostSummary above carries
		// only $/in/out/sessions, so without this the Stopped section showed no
		// turn count and no context figure.
		//
		// Liveness is NOT hardcoded false despite the section name: the dashboard
		// files agent_state=rate_limited under "Stopped", but a rate-limited agent
		// is still RUNNING (agentStateIsLive counts it live), so its context is a
		// current reading and must not be labelled "at exit".
		if ct := formatAgentContextTurns(th, a.Metadata, agentStateIsLive(a.AgentState)); ct != "" {
			line += fmt.Sprintf("\n%s", ct)
		}
	}
	return slack.NewSectionBlock(
		slack.NewTextBlockObject("mrkdwn", line, false, false), nil, nil)
}

// buildDashboardHash produces a content hash for change detection.
func buildDashboardHash(agents []beadsapi.AgentBead, decisions []*beadsapi.BeadDetail) string {
	// Build pending-decision count per agent for hash inclusion.
	pendingByAgent := make(map[string]int, len(decisions))
	for _, dec := range decisions {
		if agentID := dec.Fields["requesting_agent_bead_id"]; agentID != "" {
			pendingByAgent[agentID]++
		}
	}

	var parts []string
	for _, a := range agents {
		var state string
		switch {
		case a.AgentState == "failed" || a.PodPhase == "failed":
			state = "dead"
		case a.AgentState == "done":
			state = "done"
		case a.AgentState == "rate_limited":
			state = "rate_limited"
		case a.AgentState == "working":
			state = "working"
		case a.AgentState == "spawning":
			state = "starting"
		case pendingByAgent[a.ID] > 0:
			state = fmt.Sprintf("decision:%d", pendingByAgent[a.ID])
		default:
			state = "idle"
		}
		cost := a.Metadata["total_cost_usd"]
		parts = append(parts, fmt.Sprintf("%s:%s:%s:%s", a.AgentName, state, a.Project, cost))
	}
	sort.Strings(parts)
	for _, d := range decisions {
		parts = append(parts, fmt.Sprintf("dec:%s", d.ID))
	}
	return strings.Join(parts, "|")
}

// formatFleetCostLine formats account-level cost data from LiteLLM spend
// tracking into a dashboard summary string. Returns "" if no cost data.
// Example output: ":moneybag: $12.50 today · $45.00 this week · $320.00 total"
func formatFleetCostLine(th Theme, costs accountCostData) string {
	if costs.TotalUSD <= 0 && costs.DailyUSD <= 0 && costs.WeeklyUSD <= 0 {
		return ""
	}
	var parts []string
	if costs.DailyUSD > 0 {
		parts = append(parts, fmt.Sprintf("$%.2f today", costs.DailyUSD))
	}
	if costs.WeeklyUSD > 0 {
		parts = append(parts, fmt.Sprintf("$%.2f this week", costs.WeeklyUSD))
	}
	if costs.TotalUSD > 0 {
		parts = append(parts, fmt.Sprintf("$%.2f total", costs.TotalUSD))
	}
	if len(parts) == 0 {
		return ""
	}
	return th.Semantic(SemCost) + " " + strings.Join(parts, " · ")
}

// --- Message Lifecycle ---

// initMessage restores a persisted dashboard message, finds a pinned one, or posts new.
func (d *Dashboard) initMessage(ctx context.Context) {
	// Try restore from state file.
	if d.state != nil {
		if ref, ok := d.state.GetDashboard(); ok && ref.ChannelID != "" && ref.Timestamp != "" {
			blocks, hash, err := d.buildBlocks(ctx)
			if err != nil {
				d.logger.Error("dashboard: init build blocks failed", "error", err)
			} else {
				_, _, _, err = d.api.UpdateMessageContext(ctx, ref.ChannelID, ref.Timestamp,
					slack.MsgOptionBlocks(blocks...))
				if err == nil {
					d.mu.Lock()
					d.msg = &dashboardMessage{ChannelID: ref.ChannelID, Timestamp: ref.Timestamp, LastHash: hash}
					d.lastUpdate = time.Now()
					d.mu.Unlock()
					d.logger.Info("dashboard: restored from state", "channel", ref.ChannelID, "ts", ref.Timestamp)
					return
				}
				d.logger.Warn("dashboard: persisted message gone, checking pins", "error", err)
			}
		}
	}

	// Look for a pinned dashboard message.
	if ts := d.findPinnedDashboard(); ts != "" {
		blocks, hash, err := d.buildBlocks(ctx)
		if err != nil {
			d.logger.Error("dashboard: init build blocks (pin recovery) failed", "error", err)
		} else {
			_, _, _, err = d.api.UpdateMessageContext(ctx, d.cfg.ChannelID, ts,
				slack.MsgOptionBlocks(blocks...))
			if err == nil {
				d.mu.Lock()
				d.msg = &dashboardMessage{ChannelID: d.cfg.ChannelID, Timestamp: ts, LastHash: hash}
				d.lastUpdate = time.Now()
				d.mu.Unlock()
				if d.state != nil {
					_ = d.state.SetDashboard(DashboardRef{ChannelID: d.cfg.ChannelID, Timestamp: ts, LastHash: hash})
				}
				d.logger.Info("dashboard: recovered pinned message", "channel", d.cfg.ChannelID, "ts", ts)
				return
			}
			d.logger.Warn("dashboard: pinned message update failed, posting new", "error", err)
		}
	}

	d.postNew(ctx)
}

func (d *Dashboard) findPinnedDashboard() string {
	items, _, err := d.api.ListPins(d.cfg.ChannelID)
	if err != nil {
		d.logger.Warn("dashboard: list pins failed", "error", err)
		return ""
	}
	for _, item := range items {
		if item.Message == nil {
			continue
		}
		if strings.Contains(item.Message.Text, dashboardMarker) {
			d.logger.Debug("dashboard: found pinned message", "ts", item.Message.Timestamp)
			return item.Message.Timestamp
		}
	}
	return ""
}

func (d *Dashboard) postNew(ctx context.Context) {
	// Clean up old message.
	d.mu.Lock()
	oldMsg := d.msg
	d.mu.Unlock()
	d.cleanupOldMessage(oldMsg)

	blocks, hash, err := d.buildBlocks(ctx)
	if err != nil {
		d.logger.Error("dashboard: build blocks failed", "error", err)
		return
	}

	chID, ts, err := d.api.PostMessageContext(ctx, d.cfg.ChannelID, slack.MsgOptionBlocks(blocks...))
	if err != nil {
		d.logger.Error("dashboard: post message failed", "error", err)
		return
	}

	// Pin the message.
	if pinErr := d.api.AddPin(chID, slack.NewRefToMessage(chID, ts)); pinErr != nil {
		if !strings.Contains(pinErr.Error(), "already_pinned") {
			d.logger.Warn("dashboard: pin failed", "error", pinErr)
		}
	}

	d.mu.Lock()
	d.msg = &dashboardMessage{ChannelID: chID, Timestamp: ts, LastHash: hash}
	d.lastUpdate = time.Now()
	d.mu.Unlock()

	if d.state != nil {
		_ = d.state.SetDashboard(DashboardRef{ChannelID: chID, Timestamp: ts, LastHash: hash})
	}

	d.logger.Info("dashboard: posted new message", "channel", chID, "ts", ts)
}

func (d *Dashboard) cleanupOldMessage(old *dashboardMessage) {
	if old == nil || old.ChannelID == "" || old.Timestamp == "" {
		return
	}
	ref := slack.NewRefToMessage(old.ChannelID, old.Timestamp)
	if err := d.api.RemovePin(old.ChannelID, ref); err != nil {
		if !strings.Contains(err.Error(), "no_pin") {
			d.logger.Warn("dashboard: unpin old message failed", "error", err)
		}
	}
	if _, _, err := d.api.DeleteMessageContext(context.Background(), old.ChannelID, old.Timestamp); err != nil {
		if !strings.Contains(err.Error(), "message_not_found") {
			d.logger.Warn("dashboard: delete old message failed", "error", err)
		}
	}
}

// refresh fetches the current roster and updates the dashboard message if changed.
func (d *Dashboard) refresh(ctx context.Context) {
	d.mu.Lock()
	msg := d.msg
	dirty := d.dirty
	lastUpdate := d.lastUpdate
	d.dirty = false
	d.mu.Unlock()

	if msg == nil {
		d.postNew(ctx)
		return
	}

	// Rate limit: minimum 3s between updates.
	if time.Since(lastUpdate) < d.minInterval {
		if dirty {
			d.mu.Lock()
			d.dirty = true
			d.mu.Unlock()
		}
		return
	}

	blocks, hash, err := d.buildBlocks(ctx)
	if err != nil {
		d.logger.Error("dashboard: refresh build blocks failed", "error", err)
		return
	}

	// Skip if unchanged and updated recently (force refresh every 5 min for timestamp).
	if hash == msg.LastHash && time.Since(lastUpdate) < 5*time.Minute {
		return
	}

	_, _, _, err = d.api.UpdateMessageContext(ctx, msg.ChannelID, msg.Timestamp,
		slack.MsgOptionBlocks(blocks...))
	if err != nil {
		if rateLimitErr, ok := err.(*slack.RateLimitedError); ok {
			d.logger.Warn("dashboard: rate limited", "retry_after", rateLimitErr.RetryAfter)
			d.mu.Lock()
			d.dirty = true
			d.mu.Unlock()
			return
		}
		d.logger.Error("dashboard: update failed", "error", err)
		if strings.Contains(err.Error(), "message_not_found") {
			d.mu.Lock()
			d.msg = nil
			d.mu.Unlock()
		}
		return
	}

	d.mu.Lock()
	d.msg.LastHash = hash
	d.lastUpdate = time.Now()
	d.mu.Unlock()

	if d.state != nil {
		_ = d.state.SetDashboard(DashboardRef{ChannelID: msg.ChannelID, Timestamp: msg.Timestamp, LastHash: hash})
	}
}
