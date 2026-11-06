// Package bridge provides the JIRA agent resolver.
//
// JiraAgentResolver handles agent lookup and reuse for JIRA ticket mentions.
// When a @gardenio mention arrives on a JIRA ticket, it finds the original
// agent (or spawns a new one) and delivers the instruction context. Modeled
// after the GitLab AgentResolver in gitlab_agent_resolver.go.
package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// JiraAgentResolverClient is the subset of beadsapi.Client used by JiraAgentResolver.
type JiraAgentResolverClient interface {
	FindAgentBead(ctx context.Context, agentName string) (*beadsapi.BeadDetail, error)
	ListActiveAgentBeadDetails(ctx context.Context) ([]*beadsapi.BeadDetail, error)
	SpawnAgent(ctx context.Context, agentName, project, taskID, role, customPrompt string, extraFields ...map[string]string) (string, error)
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	// GetBead is used to inherit the human operator stamped on the mention
	// task bead onto a respawned agent (human-operator invariant, task
	// ka-0000002839).
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
	// CloseBead is used to close a stale terminal-state agent bead before a
	// same-name respawn. A stopped agent (gb stop / crash) leaves its bead in
	// an active STATUS with a terminal agent_state until the reaper closes it;
	// without the close, SpawnAgent's name-dedup returns the dead bead and no
	// pod is ever minted (F3, plan ka-0000007401 — same fix shape as
	// respawnThreadAgent, ka-0000002089).
	CloseBead(ctx context.Context, beadID string, fields map[string]string) error
	// UpdateBeadFields refreshes the LIVE agent bead's jira_comment_id on a
	// follow-up nudge so the agent's next `gb jira reply` anchors to the
	// comment that woke it, not the original mention (GAP 2, ka-0000007091).
	// The respawn path already stamps jira_comment_id via SpawnAgent
	// extraFields; this closes the same gap for the nudge-a-live-agent path.
	UpdateBeadFields(ctx context.Context, beadID string, fields map[string]string) error
}

// JiraAgentResolver looks up or spawns agents for JIRA ticket mentions.
type JiraAgentResolver struct {
	daemon JiraAgentResolverClient
	logger *slog.Logger

	// nudge delivers live-agent nudges (NudgeAgent's interrupt mail in
	// production). Nil fails every live-agent nudge.
	nudge NudgeFunc

	// requireHumanMode gates whether a respawn that resolves no human operator
	// is allowed (warn) or refused (enforce). Read once from
	// GARDENIO_REQUIRE_HUMAN_OPERATOR. See human_operator.go (epic
	// ka-0000004835, task ka-0000002839).
	requireHumanMode HumanOperatorMode

	// defaultOperator is the designated human operator (slack:U… / oidc:…@…)
	// the bridge attributes a respawn to when the task bead carries no
	// walkable human operated_by. Wired from JIRA_BRIDGE_DEFAULT_OPERATOR;
	// validated human-shaped at construction. Empty ⇒ no fallback (the
	// human-operator gate then decides). Closes the same spawn gap as the
	// mention handler (origin bug ka-0000002128).
	defaultOperator string

	mu          sync.Mutex
	recentSpawn map[string]time.Time // key → last spawn time
}

// JiraAgentResolverConfig holds configuration for JiraAgentResolver.
type JiraAgentResolverConfig struct {
	Daemon JiraAgentResolverClient
	Logger *slog.Logger
	// Nudge delivers live-agent nudges (NudgeAgent in production).
	Nudge NudgeFunc
	// DefaultOperator is the designated human operator the bridge attributes a
	// respawn to when the task bead carries no walkable human operated_by.
	// Wired from JIRA_BRIDGE_DEFAULT_OPERATOR. A non-human value is logged and
	// ignored at construction. Empty ⇒ legacy behavior (origin bug
	// ka-0000002128).
	DefaultOperator string
}

// jiraSpawnCooldown is the minimum interval between spawns for the same JIRA key.
const jiraSpawnCooldown = 10 * time.Minute

// resumeS3KeyField is the field on a worked TASK bead that carries the canonical
// S3 session prefix for a resume chain. gb init's resolveCanonicalS3Key follows
// it to point a respawned pod's coop at the ORIGINAL transcript. The bridge-side
// writer (cmd/gb init_s3_restore.go) owns the same field name; kept as a named
// constant here so the --fresh breadcrumb-clear can never drift from it.
const resumeS3KeyField = "resume_s3_key"

// NewJiraAgentResolver creates a new JiraAgentResolver.
func NewJiraAgentResolver(cfg JiraAgentResolverConfig) *JiraAgentResolver {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	// Validate the designated default operator: it MUST be a human shape or
	// the daemon rejects it downstream (see jira_mentions.go). Drop a
	// malformed value rather than wiring a fallback that re-creates the
	// original refusal (ka-0000002128).
	defaultOperator := strings.TrimSpace(cfg.DefaultOperator)
	if defaultOperator != "" && !IsHumanOperator(defaultOperator) {
		logger.Warn("jira-resolver: ignoring non-human JIRA_BRIDGE_DEFAULT_OPERATOR",
			"value", defaultOperator)
		defaultOperator = ""
	}
	return &JiraAgentResolver{
		daemon:           cfg.Daemon,
		logger:           logger,
		nudge:            cfg.Nudge,
		requireHumanMode: HumanOperatorModeFromEnv(),
		defaultOperator:  defaultOperator,
		recentSpawn:      make(map[string]time.Time),
	}
}

// deliverNudge sends a live-agent nudge via the configured NudgeFunc
// (interrupt mail in production). The delivery
// is wrapped in a bounded transient-daemon retry (retryJiraWake) so a momentary
// daemon/coop blip during a follow-up wake is retried rather than silently
// dropping the wake — the Slack-parity durability gap this closes (GAP 3a,
// ka-0000002721). A terminal failure (retries exhausted or a deterministic
// error) increments the wake-failure counter so a stalled conversational loop
// is visible in a scrape, not just a pod log the agent cannot read.
func (r *JiraAgentResolver) deliverNudge(ctx context.Context, agentName, message string) error {
	err := retryJiraWake(ctx, jiraWakeStageNudge, r.logger, func() error {
		if r.nudge == nil {
			return fmt.Errorf("nudge %q: no nudge configured", agentName)
		}
		return r.nudge(ctx, agentName, message)
	})
	if err != nil {
		jiraFollowUpWakeFailures.WithLabelValues(jiraWakeStageNudge).Inc()
	}
	return err
}

// ResolveAndNudgeFollowUp finds the agent assigned to the given JIRA ticket's
// task bead and delivers a nudge with the mention context. If the agent is dead
// (no active agent bead, or one closed, terminal or stopping), it spawns a new
// agent pre-assigned to the task.
//
// It serves the webhook follow-up path (F3, plan ka-0000007401): commentID is
// the originating Jira comment id, stamped as jira_comment_id on a respawned
// agent bead so the agent's `gb jira reply` can thread back to the comment that
// woke it.
//
// fresh mirrors Slack's `@gardenio --fresh` (ka-0000008188): when the follow-up
// comment carried a --fresh flag, a RESPAWN of the stopped agent must start a
// CLEAN conversation instead of resuming the prior transcript. On the
// live-agent nudge branches --fresh is inert (a running pod cannot forget its
// in-memory context) and only logged. See the respawn branch below for the
// continuity levers it flips.
func (r *JiraAgentResolver) ResolveAndNudgeFollowUp(ctx context.Context, agentName, jiraKey, project, taskID, commentID, prompt string, fresh bool) error {
	return r.resolveAndNudge(ctx, agentName, jiraKey, project, taskID, commentID, prompt, fresh)
}

func (r *JiraAgentResolver) resolveAndNudge(ctx context.Context, agentName, jiraKey, project, taskID, commentID, prompt string, fresh bool) error {
	if agentName == "" {
		return fmt.Errorf("empty agent name for JIRA key %s", jiraKey)
	}

	// Try to find the existing agent bead. A bead in a terminal agent_state
	// (done/failed/stopping/budget_exceeded) is NOT alive: the pod is gone or
	// shutting down, and the bead
	// stays in an active STATUS until the stale-reaper closes it. Treat it as
	// stopped and remember it so the spawn below can close it first — the
	// same-name respawn is what restores session continuity (the entrypoint
	// finds the prior session JSONL / PVC, and for frozen runsc agents the
	// name-keyed thaw fires). Mirrors respawnThreadAgent (ka-0000002089).
	var staleTerminal *beadsapi.BeadDetail
	agentBead, err := r.daemon.FindAgentBead(ctx, agentName)
	if err == nil && agentBead != nil {
		if isAgentTerminal(agentBead.Fields["agent_state"]) {
			staleTerminal = agentBead
			r.logger.Info("jira-resolver: agent bead in terminal state, treating as stopped",
				"agent", agentName, "jira_key", jiraKey,
				"agent_state", agentBead.Fields["agent_state"])
		} else {
			if !agentLooksDead(agentBead) {
				r.logger.Info("jira-resolver: agent alive, nudging",
					"agent", agentName, "jira_key", jiraKey, "task", taskID)
				// --fresh is inert on the live-agent path: the running pod holds
				// the conversation in memory and cannot be told to forget it, so
				// we deliver the follow-up as a normal nudge (mirrors Slack's
				// noteFreshUnsupportedForLiveAgent, ka-0000008188). The webhook
				// already posts an operator note telling them to `@gardenio kill`
				// then re-mention with --fresh to force a clean slate.
				if fresh {
					r.logger.Info("jira-resolver: --fresh ignored for live agent (cannot wipe in-memory context); delivering as normal nudge",
						"agent", agentName, "jira_key", jiraKey)
				}
				// Refresh the LIVE agent bead's jira_comment_id so its next
				// `gb jira reply` anchors to the comment that woke it, not the
				// original mention (GAP 2, ka-0000007091). The respawn path
				// below already stamps this via SpawnAgent extraFields.
				r.refreshLiveAgentCommentID(ctx, agentBead.ID, agentName, jiraKey, commentID)
				recordJiraFollowUpDelivery(jiraDeliveryWarm)
				return r.deliverNudge(ctx, agentName, prompt)
			}
			r.logger.Info("jira-resolver: agent bead exists but is closed or stopping, treating as dead",
				"agent", agentName, "jira_key", jiraKey)
		}
	}

	// Guard: don't respawn if we already spawned for this key recently.
	if r.isRecentSpawn(jiraKey) {
		r.logger.Info("jira-resolver: skipping spawn — already spawned recently",
			"agent", agentName, "jira_key", jiraKey)
		recordJiraFollowUpDelivery(jiraDeliverySkipped)
		return nil
	}

	// Check if another agent is already alive and working on this JIRA ticket.
	if existingAgent, existingBeadID, found := r.FindActiveAgentForJiraKey(ctx, jiraKey); found {
		r.logger.Info("jira-resolver: found active agent for JIRA key, nudging instead of spawning",
			"existing_agent", existingAgent, "jira_key", jiraKey)
		// --fresh is inert here too — this branch nudges a DIFFERENT live agent
		// bound to the ticket, whose in-memory conversation likewise cannot be
		// wiped (ka-0000008188).
		if fresh {
			r.logger.Info("jira-resolver: --fresh ignored for active agent (cannot wipe in-memory context); delivering as normal nudge",
				"existing_agent", existingAgent, "jira_key", jiraKey)
		}
		// Refresh the live agent's reply anchor to the waking comment (GAP 2,
		// ka-0000007091) — same rationale as the named-agent branch above.
		r.refreshLiveAgentCommentID(ctx, existingBeadID, existingAgent, jiraKey, commentID)
		recordJiraFollowUpDelivery(jiraDeliveryWarm)
		return r.deliverNudge(ctx, existingAgent, prompt)
	}

	// Agent is dead or not found — spawn a new one.
	r.logger.Info("jira-resolver: spawning new agent",
		"agent", agentName, "jira_key", jiraKey, "task", taskID, "project", project)

	// Close a lingering terminal-state bead before the same-name respawn.
	// SpawnAgent's name-dedup treats any active-status bead as "already
	// exists" and returns it without minting a pod — the dead bead would
	// swallow the respawn and the conversation would stay dead. Best-effort:
	// a close failure is logged and the spawn proceeds (matching
	// respawnThreadAgent).
	if staleTerminal != nil {
		r.logger.Info("jira-resolver: closing stale terminal-state bead before respawn",
			"agent", agentName, "bead", staleTerminal.ID, "jira_key", jiraKey,
			"agent_state", staleTerminal.Fields["agent_state"])
		if err := r.daemon.CloseBead(ctx, staleTerminal.ID, nil); err != nil {
			r.logger.Warn("jira-resolver: failed to close stale terminal bead; proceeding anyway",
				"agent", agentName, "bead", staleTerminal.ID, "error", err)
		}
	}

	extraFields := map[string]string{
		"jira_key":     jiraKey,
		"spawn_source": "jira-mention",
	}
	// Thread the originating Jira comment id onto the respawned agent so its
	// replies can reference the comment that woke it (F3, plan ka-0000007401).
	if commentID != "" {
		extraFields["jira_comment_id"] = commentID
	}
	// --fresh (ka-0000008188, Slack parity): the operator asked to continue this
	// ticket conversation with a CLEAN context. Unlike Slack's thread respawn —
	// which restores continuity by REVIVING the prior bead in place (same
	// COOP_S3_SESSION_KEY prefix) — this Jira respawn already mints a NEW agent
	// bead id via SpawnAgent, so its S3 prefix is empty and coop cold-starts by
	// default. Two continuity levers still survive a same-name respawn and must
	// be defeated to make --fresh real:
	//
	//   1. The resume_s3_key BREADCRUMB on the worked task bead. It is stamped on
	//      an INVOLUNTARY death (hook.go reopenOrphanedClaimedTask) and adopted at
	//      boot by resolveCanonicalS3Key, which points the new pod's coop at the
	//      ORIGINAL transcript prefix — resurrecting exactly the conversation the
	//      operator asked to drop. Clear it here so the fresh pod keys on its own
	//      (empty) bead-id prefix. Best-effort: a failure only risks a stale
	//      pointer that the next involuntary death would re-stamp anyway.
	//   2. The gVisor name-keyed THAW (shouldThaw) and the PVC-local session .jsonl
	//      (findResumeSession). session_resume=false stamps podspec
	//      SessionResume=false → BOAT_SESSION_RESUME=0, which vetoes shouldThaw and
	//      returns "" from findResumeSession, so a frozen runsc agent starts clean
	//      instead of thawing its old context. Mirrors respawnThreadAgent.
	if fresh {
		extraFields["session_resume"] = "false"
		r.logger.Info("jira-resolver: --fresh respawn — starting a clean conversation (no resume/thaw)",
			"agent", agentName, "jira_key", jiraKey, "task", taskID)
		if taskID != "" {
			if err := r.daemon.UpdateBeadFields(ctx, taskID,
				map[string]string{resumeS3KeyField: ""}); err != nil {
				r.logger.Warn("jira-resolver: --fresh: failed to clear resume_s3_key breadcrumb; proceeding anyway",
					"task", taskID, "jira_key", jiraKey, "error", err)
			}
		}
	}

	// Inherit the human operator stamped on the mention task bead so the
	// respawned agent carries a walkable human via BEADS_OPERATED_BY — this
	// path previously omitted operated_by entirely, booting the agent
	// unattributed (origin bug ka-0000002128). Gate per
	// GARDENIO_REQUIRE_HUMAN_OPERATOR: refuse in enforce when no human is
	// resolvable. Best-effort lookup — a fetch error degrades to "no operator
	// inherited" and the gate decides.
	var inheritedOp string
	if taskID != "" {
		if tb, gerr := r.daemon.GetBead(ctx, taskID); gerr != nil {
			r.logger.Warn("jira-resolver: task bead lookup for operator inheritance failed",
				"task", taskID, "jira_key", jiraKey, "err", gerr)
		} else if tb != nil {
			inheritedOp = tb.Fields["operated_by"]
		}
	}
	// Last-resort: when the task bead carries no walkable human operator,
	// attribute the respawn to the designated default operator so the agent
	// boots with a human via BEADS_OPERATED_BY and can file
	// escalations/squawks/comments (origin bug ka-0000002128). Only applied
	// when the inherited value isn't already human-shaped.
	if !IsHumanOperator(inheritedOp) && r.defaultOperator != "" {
		r.logger.Info("jira-resolver: task bead has no human operator, attributing to designated default operator",
			"task", taskID, "jira_key", jiraKey, "default_operator", r.defaultOperator)
		inheritedOp = r.defaultOperator
	}
	gatedOp, err := gateSpawnOperator(r.requireHumanMode, r.logger,
		"jira-resolver", agentName, inheritedOp)
	if err != nil {
		return err
	}
	if IsHumanOperator(gatedOp) {
		extraFields["operated_by"] = gatedOp
	}

	spawnRole, err := resolveJiraSpawnRole(ctx, r.daemon, project, r.logger)
	if err != nil {
		return fmt.Errorf("resolve spawn role for JIRA key %s: %w", jiraKey, err)
	}
	// Wrap the respawn in the same bounded transient-daemon retry as the
	// live-agent nudge above (GAP 3a, ka-0000002721): a stopped agent whose
	// follow-up wake hits a momentary daemon blip during SpawnAgent would
	// otherwise never be resurrected, and the ticket conversation dies silently
	// (the webhook returns 200, there is no wake re-sync). A terminal failure
	// increments the spawn-stage wake-failure counter.
	var agentBeadID string
	err = retryJiraWake(ctx, jiraWakeStageSpawn, r.logger, func() error {
		var spawnErr error
		agentBeadID, spawnErr = r.daemon.SpawnAgent(ctx, agentName, project, taskID, spawnRole, prompt, extraFields)
		return spawnErr
	})
	if err != nil {
		jiraFollowUpWakeFailures.WithLabelValues(jiraWakeStageSpawn).Inc()
		return fmt.Errorf("spawn agent %q for JIRA key %s: %w", agentName, jiraKey, err)
	}

	r.markSpawned(jiraKey)
	recordJiraFollowUpDelivery(jiraDeliveryCold)
	r.logger.Info("jira-resolver: spawned agent",
		"agent", agentName, "agent_bead", agentBeadID,
		"jira_key", jiraKey, "task", taskID, "project", project)
	return nil
}

// refreshLiveAgentCommentID stamps the waking comment id onto a LIVE agent bead
// so its next `gb jira reply` anchors its focusedCommentId quote-link to the
// comment that woke it, not the stale original mention (GAP 2, ka-0000007091).
// Best-effort: a no-op when the id or bead is missing, and a write failure is
// logged but never fails the nudge — a slightly-stale anchor is far better than
// a dropped follow-up. Only the respawn path stamps this via SpawnAgent; this
// closes the same gap on the nudge-a-live-agent path.
func (r *JiraAgentResolver) refreshLiveAgentCommentID(ctx context.Context, beadID, agentName, jiraKey, commentID string) {
	if commentID == "" || beadID == "" {
		return
	}
	if err := r.daemon.UpdateBeadFields(ctx, beadID, map[string]string{"jira_comment_id": commentID}); err != nil {
		r.logger.Warn("jira-resolver: failed to refresh live agent jira_comment_id",
			"agent", agentName, "bead", beadID, "jira_key", jiraKey,
			"comment_id", commentID, "error", err)
		return
	}
	r.logger.Info("jira-resolver: refreshed live agent jira_comment_id to waking comment",
		"agent", agentName, "bead", beadID, "jira_key", jiraKey, "comment_id", commentID)
}

// FindActiveAgentForJiraKey searches active agent beads for one bound to the
// given JIRA key that is alive (not agentLooksDead). beadID is the matched
// agent bead's id, returned so a follow-up can refresh its jira_comment_id
// anchor (GAP 2, ka-0000007091).
func (r *JiraAgentResolver) FindActiveAgentForJiraKey(ctx context.Context, jiraKey string) (agentName, beadID string, found bool) {
	agents, err := r.daemon.ListActiveAgentBeadDetails(ctx)
	if err != nil {
		r.logger.Warn("jira-resolver: failed to list active agents", "error", err)
		return "", "", false
	}
	for _, a := range agents {
		if a.Fields["jira_key"] != jiraKey {
			continue
		}
		// A terminal-state bead can linger in an active status until the
		// reaper closes it — its pod is gone, so a nudge would silently
		// vanish. Skip it; the caller respawns instead.
		if agentLooksDead(a) {
			continue
		}
		name := a.Fields["agent"]
		if name == "" {
			name = a.Title
		}
		return name, a.ID, true
	}
	return "", "", false
}

// isRecentSpawn returns true if an agent was spawned for this key within the cooldown.
func (r *JiraAgentResolver) isRecentSpawn(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.recentSpawn[key]; ok && time.Since(t) < jiraSpawnCooldown {
		return true
	}
	return false
}

// markSpawned records that an agent was just spawned for this key.
func (r *JiraAgentResolver) markSpawned(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Prune stale entries.
	for k, t := range r.recentSpawn {
		if time.Since(t) > jiraSpawnCooldown {
			delete(r.recentSpawn, k)
		}
	}
	r.recentSpawn[key] = time.Now()
}
