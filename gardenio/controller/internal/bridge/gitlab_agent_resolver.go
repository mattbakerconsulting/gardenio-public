// Package bridge provides the GitLab MR agent resolver.
//
// AgentResolver handles agent lookup and reuse for MR review comments.
// When a review comment arrives on an agent-authored MR, it finds the original
// agent (or spawns a new one) and delivers the review context.
package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// AgentResolverClient is the subset of beadsapi.Client used by AgentResolver.
type AgentResolverClient interface {
	FindAgentBead(ctx context.Context, agentName string) (*beadsapi.BeadDetail, error)
	ListActiveAgentBeadDetails(ctx context.Context) ([]*beadsapi.BeadDetail, error)
	SpawnAgent(ctx context.Context, agentName, project, taskID, role, customPrompt string, extraFields ...map[string]string) (string, error)
	UpdateBeadFields(ctx context.Context, beadID string, fields map[string]string) error
	// MRBeadForTask walks task → merge-request via the `produces` dep edge.
	// Used to read MR state from the canonical MR bead instead of the legacy
	// task-bead mr_* fields. See epic ka-0000001613.
	MRBeadForTask(ctx context.Context, taskBeadID string) (*beadsapi.BeadDetail, error)
}

// mrInfoFromTask returns the MR URL and merged flag for a task bead by
// reading the linked type=merge-request bead. The legacy task-bead mr_* fields
// are no longer written (epic ka-0000001613 / task ka-0000008134), so the
// only canonical source is the MR bead. Returns empty/false when no MR bead
// is linked yet or the lookup errors; the mr_url field on the task bead is
// still consulted as the trigger value (it is written by the mentions handler
// and JIRA bridge), but merged status is exclusively from the MR bead.
func mrInfoFromTask(ctx context.Context, c AgentResolverClient, taskBead BeadEvent, logger *slog.Logger) (mrURL string, merged bool) {
	mrURL = taskBead.Fields["mr_url"]
	if c == nil {
		return mrURL, false
	}
	mrBead, err := c.MRBeadForTask(ctx, taskBead.ID)
	if err != nil {
		if logger != nil {
			logger.Debug("MR bead lookup failed",
				"task", taskBead.ID, "error", err)
		}
		return mrURL, false
	}
	if mrBead == nil {
		return mrURL, false
	}
	if u := mrBead.Fields["mr_url"]; u != "" {
		mrURL = u
	}
	merged = mrBead.Fields["mr_outcome"] == "merged" || mrBead.Fields["mr_state"] == "merged"
	return mrURL, merged
}

// AgentResolver looks up or spawns agents for MR review comment handling.
type AgentResolver struct {
	daemon AgentResolverClient
	gitlab *GitLabClient
	nudge  NudgeFunc
	logger *slog.Logger

	// requireHumanMode gates whether an MR-review respawn that resolves no
	// human operator is allowed (warn) or refused (enforce). Read once from
	// GARDENIO_REQUIRE_HUMAN_OPERATOR. See human_operator.go (epic
	// ka-0000004835, task ka-0000002839).
	requireHumanMode HumanOperatorMode

	mu          sync.Mutex
	recentSpawn map[string]time.Time // key → last spawn time (task bead ID or "mr:<url>")
}

// AgentResolverConfig holds configuration for the AgentResolver.
type AgentResolverConfig struct {
	Daemon AgentResolverClient
	GitLab *GitLabClient
	Nudge  NudgeFunc // delivers a live agent's nudge (NudgeAgent in production)
	Logger *slog.Logger
}

// NewAgentResolver creates a new AgentResolver.
func NewAgentResolver(cfg AgentResolverConfig) *AgentResolver {
	return &AgentResolver{
		daemon:           cfg.Daemon,
		gitlab:           cfg.GitLab,
		nudge:            cfg.Nudge,
		logger:           cfg.Logger,
		requireHumanMode: HumanOperatorModeFromEnv(),
		recentSpawn:      make(map[string]time.Time),
	}
}

// ResolveAndNudge finds the agent assigned to the given task bead and delivers
// a nudge with the review comment context. If the agent is dead (no active
// agent bead found), it spawns a new agent pre-assigned to the task with the
// MR branch context.
//
// The taskBead must have a non-empty Assignee field (the agent name) and an
// mr_url field pointing to the GitLab MR.
func (r *AgentResolver) ResolveAndNudge(ctx context.Context, taskBead BeadEvent, message string) error {
	agentName := taskBead.Assignee
	if agentName == "" {
		return fmt.Errorf("task bead %s has no assignee", taskBead.ID)
	}

	// Prefer the canonical MR bead (epic ka-0000001613) — falls back to legacy
	// task-bead mr_* fields when no MR bead is linked yet (overlap window).
	mrURL, mrMerged := mrInfoFromTask(ctx, r.daemon, taskBead, r.logger)

	// Try to find the existing agent bead.
	agentBead, err := r.daemon.FindAgentBead(ctx, agentName)
	if err == nil && agentBead != nil {
		if !agentLooksDead(agentBead) {
			r.logger.Info("agent alive, nudging for MR review",
				"agent", agentName, "task", taskBead.ID, "mr_url", mrURL)

			// Store MR binding metadata on the agent bead (best-effort).
			r.setMRBindingFields(ctx, agentBead.ID, taskBead)

			return r.deliverNudge(ctx, agentName, message)
		}
		r.logger.Info("agent bead exists but is terminal or stopping, treating as dead",
			"agent", agentName, "task", taskBead.ID, "agent_state", agentBead.Fields["agent_state"])
	}

	// Guard: don't respawn if the MR is already merged. Source of truth is
	// the MR bead when present (epic ka-0000001613); the legacy mr_merged
	// task field is consulted in the fallback branch of mrInfoFromTask.
	if mrMerged {
		r.logger.Info("skipping agent spawn — MR already merged",
			"agent", agentName, "task", taskBead.ID, "mr_url", mrURL)
		return nil
	}

	// Guard: don't respawn if the task bead is closed.
	if taskBead.Status == "closed" || taskBead.Status == "done" {
		r.logger.Info("skipping agent spawn — task bead is closed",
			"agent", agentName, "task", taskBead.ID, "status", taskBead.Status)
		return nil
	}

	// Guard: don't respawn if we already spawned for this task recently (10 min).
	if r.isRecentSpawn(taskBead.ID) {
		r.logger.Info("skipping agent spawn — already spawned recently for this task",
			"agent", agentName, "task", taskBead.ID)
		return nil
	}

	// Guard: don't respawn if we already spawned for this MR recently (10 min).
	// This prevents multiple agents when different task beads point to the same MR.
	if mrURL != "" && r.isRecentSpawn("mr:"+mrURL) {
		r.logger.Info("skipping agent spawn — already spawned recently for this MR",
			"agent", agentName, "task", taskBead.ID, "mr_url", mrURL)
		return nil
	}

	// Check if another agent is already alive and working on this MR.
	// If so, nudge that agent instead of spawning a new one.
	if mrURL != "" {
		if existingAgent, found := r.FindActiveAgentForMR(ctx, mrURL); found {
			r.logger.Info("found active agent for MR, nudging instead of spawning",
				"existing_agent", existingAgent, "task", taskBead.ID, "mr_url", mrURL)
			return r.deliverNudge(ctx, existingAgent, message)
		}
	}

	// Agent is dead or not found — spawn a new one.
	r.logger.Info("spawning new agent for MR review comment",
		"agent", agentName, "task", taskBead.ID, "mr_url", mrURL)

	if err := r.spawnAgentForMR(ctx, agentName, taskBead); err != nil {
		return err
	}
	r.markSpawned(taskBead.ID)
	if mrURL != "" {
		r.markSpawned("mr:" + mrURL)
	}
	return nil
}

// spawnCooldown is the minimum interval between spawns for the same task bead.
const spawnCooldown = 10 * time.Minute

// isRecentSpawn returns true if an agent was spawned for this task within the cooldown.
func (r *AgentResolver) isRecentSpawn(taskBeadID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.recentSpawn[taskBeadID]; ok && time.Since(t) < spawnCooldown {
		return true
	}
	return false
}

// markSpawned records that an agent was just spawned for this task.
func (r *AgentResolver) markSpawned(taskBeadID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Prune stale entries while we're here.
	for k, t := range r.recentSpawn {
		if time.Since(t) > spawnCooldown {
			delete(r.recentSpawn, k)
		}
	}
	r.recentSpawn[taskBeadID] = time.Now()
}

// FindActiveAgentForMR searches active agent beads for one bound to the given
// MR URL (via the gitlab_mr_url field) that is alive (not agentLooksDead).
// Returns the agent name if found.
func (r *AgentResolver) FindActiveAgentForMR(ctx context.Context, mrURL string) (agentName string, found bool) {
	agents, err := r.daemon.ListActiveAgentBeadDetails(ctx)
	if err != nil {
		r.logger.Warn("failed to list active agents for MR lookup", "error", err)
		return "", false
	}
	for _, a := range agents {
		if a.Fields["gitlab_mr_url"] != mrURL || agentLooksDead(a) {
			continue
		}
		name := a.Fields["agent"]
		if name == "" {
			name = a.Title
		}
		return name, true
	}
	return "", false
}

// deliverNudge hands message to agentName through the configured NudgeFunc.
func (r *AgentResolver) deliverNudge(ctx context.Context, agentName, message string) error {
	if r.nudge == nil {
		return fmt.Errorf("nudge %q: no nudge configured", agentName)
	}
	return r.nudge(ctx, agentName, message)
}

// spawnAgentForMR creates a new agent bead pre-assigned to the task with MR
// context. The agent is spawned with the same name so the entrypoint can find
// existing session data (PVC, JSONL) for session continuity.
func (r *AgentResolver) spawnAgentForMR(ctx context.Context, agentName string, taskBead BeadEvent) error {
	// Resolve project from task bead labels.
	project := projectFromLabels(taskBead.Labels)

	// Build the custom prompt with MR context.
	mrURL := taskBead.Fields["mr_url"]
	prompt := fmt.Sprintf(
		"You have been respawned to address review comments on MR %s (task %s: %s). "+
			"Check out the MR branch, read the review comments, and address the feedback. "+
			"Use `kd show %s` to see the full task context and comments.",
		mrURL, taskBead.ID, taskBead.Title, taskBead.ID)

	// Build extra fields with MR binding metadata.
	extraFields := r.buildMRFields(taskBead)
	extraFields["spawn_source"] = "gitlab-mr-review"
	// Service-identity writes — see ka-0000005187; bridge runs under
	// fleet:gitlab-bridge. Inherit operator from the originating task bead
	// onto the spawned agent's fields so the agent's operated_by still
	// reflects the human who first triggered the review (R1 #10/#21).
	// Human-operator gate (epic ka-0000004835, task ka-0000002839): refuse in
	// enforce mode when the task bead carries no resolvable human, and only
	// stamp a value the daemon will accept (a non-human shape fails closed
	// downstream — origin bug ka-0000002128).
	gatedOp, err := gateSpawnOperator(r.requireHumanMode, r.logger,
		"gitlab-mr-review", agentName, taskBead.Fields["operated_by"])
	if err != nil {
		return err
	}
	if IsHumanOperator(gatedOp) {
		extraFields["operated_by"] = gatedOp
	}
	if u := taskBead.Fields["gitlab_username"]; u != "" {
		extraFields["gitlab_username"] = u
	}

	agentBeadID, err := r.daemon.SpawnAgent(ctx, agentName, project, taskBead.ID, "crew", prompt, extraFields)
	if err != nil {
		return fmt.Errorf("spawn agent %q for MR review: %w", agentName, err)
	}

	r.logger.Info("spawned agent for MR review",
		"agent", agentName, "agent_bead", agentBeadID,
		"task", taskBead.ID, "mr_url", mrURL, "project", project)
	return nil
}

// setMRBindingFields stores GitLab MR metadata on the agent bead so the agent
// knows which MR/branch to work on. Best-effort: errors are logged but not
// returned.
func (r *AgentResolver) setMRBindingFields(ctx context.Context, agentBeadID string, taskBead BeadEvent) {
	fields := r.buildMRFields(taskBead)
	if len(fields) == 0 {
		return
	}
	if err := r.daemon.UpdateBeadFields(ctx, agentBeadID, fields); err != nil {
		r.logger.Warn("failed to set MR binding fields on agent bead",
			"agent_bead", agentBeadID, "error", err)
	}
}

// buildMRFields extracts GitLab MR metadata from a task bead into a field map
// suitable for storing on the agent bead. Sources the canonical values from
// the linked type=merge-request bead (walked via the `produces` dep edge).
// The legacy task-bead mr_* fields are no longer written (epic
// ka-0000001613 / task ka-0000008134); only the task bead's
// `mr_url` and `gitlab_project_id` fields (still written by the mentions
// handler / JIRA bridge) act as fallbacks for early lookups before any MR
// bead exists, and missing fields are backfilled from a live GitLab fetch
// below.
func (r *AgentResolver) buildMRFields(taskBead BeadEvent) map[string]string {
	fields := make(map[string]string)

	// Start with the trigger value from the task bead (still written by
	// the mentions handler / JIRA bridge); the MR bead overrides when
	// present.
	mrURL := taskBead.Fields["mr_url"]
	var projectID, mrIID, sourceBranch string

	if r.daemon != nil {
		if mrBead, err := r.daemon.MRBeadForTask(context.Background(), taskBead.ID); err == nil && mrBead != nil {
			if u := mrBead.Fields["mr_url"]; u != "" {
				mrURL = u
			}
			if pid := mrBead.Fields["gitlab_project_id"]; pid != "" {
				projectID = pid
			}
			if iid := mrBead.Fields["gitlab_mr_iid"]; iid != "" {
				mrIID = iid
			} else if iid := mrBead.Fields["mr_iid"]; iid != "" {
				mrIID = iid
			}
			if sb := mrBead.Fields["gitlab_mr_source_branch"]; sb != "" {
				sourceBranch = sb
			} else if sb := mrBead.Fields["source_branch"]; sb != "" {
				sourceBranch = sb
			}
		} else if err != nil {
			r.logger.Debug("MR bead lookup failed for buildMRFields",
				"task", taskBead.ID, "error", err)
		}
	}

	if mrURL != "" {
		fields["gitlab_mr_url"] = mrURL
	}
	if projectID != "" {
		fields["gitlab_project_id"] = projectID
	}
	if mrIID != "" {
		fields["gitlab_mr_iid"] = mrIID
	}
	if sourceBranch != "" {
		fields["gitlab_mr_source_branch"] = sourceBranch
	}

	// Backfill missing source_branch / project_id / iid from a live GitLab
	// fetch — same behaviour as before, just gated on what the bead path
	// already filled in.
	if r.gitlab != nil && mrURL != "" && (sourceBranch == "" || projectID == "" || mrIID == "") {
		ref := ParseMRURL(mrURL)
		if ref != nil {
			mr, err := r.gitlab.GetMergeRequestByPath(context.Background(), ref.ProjectPath, ref.IID)
			if err == nil && mr != nil {
				if fields["gitlab_mr_source_branch"] == "" {
					fields["gitlab_mr_source_branch"] = mr.SourceBranch
				}
				if fields["gitlab_project_id"] == "" {
					fields["gitlab_project_id"] = strconv.Itoa(mr.ProjectID)
				}
				if fields["gitlab_mr_iid"] == "" {
					fields["gitlab_mr_iid"] = strconv.Itoa(mr.IID)
				}
			} else if err != nil {
				r.logger.Warn("failed to fetch MR details for source branch",
					"mr_url", mrURL, "error", err)
			}
		}
	}

	return fields
}
