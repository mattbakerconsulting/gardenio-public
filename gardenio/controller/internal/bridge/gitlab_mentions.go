// Package bridge provides the GitLab @mention handler.
//
// MentionHandler detects @gardenio mentions in GitLab MR comments and spawns
// a new agent to address the request. The agent is created with full MR context
// (branch, file/line for inline comments, instruction text) and posts responses
// back via the squawk forwarder.
package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// MentionBeadClient is the subset of beadsapi.Client used by MentionHandler.
type MentionBeadClient interface {
	CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error)
	SpawnAgent(ctx context.Context, agentName, project, taskID, role, customPrompt string, extraFields ...map[string]string) (string, error)
	ListTaskBeadsAll(ctx context.Context) ([]*beadsapi.BeadDetail, error)
	ListProjectBeads(ctx context.Context) (map[string]beadsapi.ProjectInfo, error)
	// MRBeadByURL + ProducingTaskBeadsForMR back the preferred MR-bead path
	// for findExistingMRAgent and hasPendingMention — see epic ka-0000001613.
	MRBeadByURL(ctx context.Context, mrURL string) (*beadsapi.BeadDetail, error)
	ProducingTaskBeadsForMR(ctx context.Context, mrBeadID string) ([]*beadsapi.BeadDetail, error)
	// AddDependency links the mention task to the MR bead (and discussion
	// bead, when present) via a `triggered-by` edge so the auto-link graph
	// can join the work bead back to the upstream trigger. See M2b on epic
	// ka-0000000933.
	AddDependency(ctx context.Context, beadID, dependsOnID, depType, createdBy string) error
	// AddComment records an operator-visible note on the task bead. The bridge
	// forwards bead comments to Slack, so a dispatch-failure comment posted
	// here surfaces the drop to operators (Track A2, plan ka-0000004007) —
	// mirroring the Slack mention path's visible "nudge failed…" card
	// (bot_mentions.go) that the GitLab path previously lacked.
	AddComment(ctx context.Context, beadID, author, text string) error
	// UpdateBeadFields stamps fields on the task bead. Used by the
	// nudge-success path (Track A3) to record which live agent the mention was
	// delivered to, so the A1 catchup watchdog can tell a genuinely-orphaned
	// task from one already covered by a nudge and not re-dispatch it.
	UpdateBeadFields(ctx context.Context, beadID string, fields map[string]string) error
}

// GitLabMentionCapability is the capability string checked on the commenter's
// user bead when gating a GitLab @mention. Grant at runtime via, e.g.:
//
//	kd update <user-bead> --field gitlab_username=<name> \
//	    --field capabilities='["mention"]'
const GitLabMentionCapability = "mention"

// mentionDispatchActor is the author stamped on the operator-visible
// dispatch-failure comment (Track A2). It matches the catchup watchdog's actor
// family so the two rescue/failure signals read as one system in the bead
// history and Slack forward.
const mentionDispatchActor = "gitlab-mention"

// GitLabUserLookup is the subset of *UserDirectory used by MentionHandler to
// resolve a GitLab user to a user bead for capability gating.
type GitLabUserLookup interface {
	LookupByGitLabUsername(ctx context.Context, username string) *UserIdentity
	LookupByGitLabUserID(ctx context.Context, id string) *UserIdentity
}

// lookupGitLabPerson resolves a GitLab user, as a forge event names them, to
// the user bead for that person, or nil. userID is GitLab's numeric user id
// from the same event (0 when the event has none).
//
// A username is a soft key: GitLab lets a person rename or delete their
// account, and someone else can then register the freed login. So the numeric
// id decides whenever it can (gd-yrnMQbmLgp):
//   - a bead whose gitlab_user_id equals userID is the person, even when the
//     login on the bead is stale after a rename;
//   - a bead found by username alone is the person only when it pins no
//     gitlab_user_id, or pins exactly userID. A bead pinning a different id
//     (or an event without an id) is someone else holding the old login, and
//     resolves to nobody.
//
// A bead with no gitlab_user_id keeps matching by username (case-insensitive),
// so existing beads keep working until an admin pins the id.
func lookupGitLabPerson(ctx context.Context, users GitLabUserLookup, username string, userID int) *UserIdentity {
	if users == nil {
		return nil
	}
	id := ""
	if userID > 0 {
		id = strconv.Itoa(userID)
		if u := users.LookupByGitLabUserID(ctx, id); u != nil {
			return u
		}
	}
	u := users.LookupByGitLabUsername(ctx, username)
	if u == nil || (u.GitLabUserID != "" && u.GitLabUserID != id) {
		return nil
	}
	return u
}

// MentionHandler processes @gardenio mentions in GitLab MR comments.
type MentionHandler struct {
	daemon     MentionBeadClient
	gitlab     *GitLabClient
	resolver   *AgentResolver   // optional; when set, enables MR-level agent consolidation
	nudge      NudgeFunc        // delivers a nudge to an existing agent (NudgeAgent in production)
	users      GitLabUserLookup // user directory for capability gate; nil ⇒ fail-closed
	trigger    string           // primary mention trigger (e.g., "@gardenio")
	botTrigger string           // secondary trigger for the actual GitLab bot username (e.g., "@group_14176856_bot_...")
	project    string           // default project for spawned agents
	logger     *slog.Logger
}

// MentionHandlerConfig holds configuration for the MentionHandler.
type MentionHandlerConfig struct {
	Daemon   MentionBeadClient
	GitLab   *GitLabClient
	Resolver *AgentResolver // optional; when set, mentions on an MR with an active agent nudge instead of spawning
	Nudge    NudgeFunc      // optional; delivers a nudge to an existing agent
	// Users is the GitLab-username-indexed user directory that backs the
	// mention permission gate. Authors without a matching type=user,
	// kind=config bead, or with one that lacks the "mention" capability,
	// are rejected. Nil ⇒ every mention is rejected fail-closed.
	Users       GitLabUserLookup
	Trigger     string // e.g., "@gardenio" (case-insensitive)
	BotUsername string // actual GitLab bot username; mentions of this also trigger spawn
	Project     string // default project for spawned agents
	Logger      *slog.Logger
}

// NewMentionHandler creates a new MentionHandler.
func NewMentionHandler(cfg MentionHandlerConfig) *MentionHandler {
	trigger := cfg.Trigger
	if trigger == "" {
		trigger = "@gardenio"
	}
	var botTrigger string
	if cfg.BotUsername != "" {
		botTrigger = strings.ToLower("@" + cfg.BotUsername)
	}
	return &MentionHandler{
		daemon:     cfg.Daemon,
		gitlab:     cfg.GitLab,
		resolver:   cfg.Resolver,
		nudge:      cfg.Nudge,
		users:      cfg.Users,
		trigger:    strings.ToLower(trigger),
		botTrigger: botTrigger,
		project:    cfg.Project,
		logger:     cfg.Logger,
	}
}

// isAuthorized gates mention processing on the user-bead capability
// `"mention"`. It returns the resolved *UserIdentity (when the author maps
// to a known user bead) alongside the authorization decision so the caller
// can reuse the identity for operator attribution without a second lookup
// (ka-0000004627). Semantics (fail-closed):
//   - No UserDirectory wired ⇒ (nil, false). Prevents silent bypass if the
//     bridge is misconfigured.
//   - Unknown GitLab user (no type=user,kind=config bead matches, see
//     lookupGitLabPerson) ⇒ (nil, false). An outside collaborator
//     commenting on a watched MR cannot trigger agent execution, nor can a
//     new owner of a login whose bead pins a different gitlab_user_id.
//   - Bead present but lacking the "mention" capability ⇒ (u, false). The
//     identity is returned for logging, but the gate denies.
func (h *MentionHandler) isAuthorized(ctx context.Context, nc noteContext) (*UserIdentity, bool) {
	if h.users == nil {
		h.logger.Error("mention: no UserDirectory wired — rejecting fail-closed")
		return nil, false
	}
	u := lookupGitLabPerson(ctx, h.users, nc.Author, nc.AuthorID)
	if u == nil {
		return nil, false
	}
	return u, u.HasCapability(GitLabMentionCapability)
}

// operatorFromGitLabUser builds a human-operator id from a resolved
// UserIdentity, matching the two shapes isHumanOperator (kbeads audit.go)
// accepts: "slack:U…" then "oidc:<email>". Returns "" when the identity
// carries neither a Slack id nor an email — the caller falls back to the
// gitlab:<username> field / fleet sentinel for truly unresolvable authors
// (ka-0000004627 R3). The Slack id is preferred so the operator joins the
// same identity used by the Slack mention path (operatorFromSlackUser).
func operatorFromGitLabUser(u *UserIdentity) string {
	if u == nil {
		return ""
	}
	if id := strings.TrimSpace(u.SlackID); id != "" {
		return "slack:" + id
	}
	if email := strings.TrimSpace(u.Email); email != "" {
		return "oidc:" + email
	}
	return ""
}

// operatorFromGitLabUsername resolves a GitLab user (username plus the numeric
// user id from the same event, 0 when unknown) to a human-operator id for
// MR-bead attribution. It resolves the person with lookupGitLabPerson and
// prefers the human-shaped slack:/oidc: id (operatorFromGitLabUser); when the
// user does not resolve to a user bead (outside collaborator, bot account with
// no bead, a re-registered login) it falls back to gitlab:<username> so the MR
// bead still records a concrete creator rather than an empty operated_by.
// Returns "" only for an empty username. users may be nil (no directory wired)
// — then the gitlab:<u> fallback is used directly.
func operatorFromGitLabUsername(ctx context.Context, users GitLabUserLookup, username string, userID int) string {
	username = strings.TrimSpace(username)
	if username == "" {
		return ""
	}
	if u := lookupGitLabPerson(ctx, users, username, userID); u != nil {
		if op := operatorFromGitLabUser(u); op != "" {
			return op
		}
	}
	return "gitlab:" + username
}

// HandleMention checks if a note contains a @gardenio mention (or a mention of
// the actual GitLab bot username) and spawns an agent if so. Returns true if a
// mention was detected and handled (caller should skip normal review-comment
// processing).
func (h *MentionHandler) HandleMention(ctx context.Context, nc noteContext) bool {
	matchedTrigger := h.trigger
	instruction := extractMentionInstruction(nc.Note, h.trigger)
	if instruction == "" && h.botTrigger != "" && h.botTrigger != h.trigger {
		instruction = extractMentionInstruction(nc.Note, h.botTrigger)
		if instruction != "" {
			matchedTrigger = h.botTrigger
		}
	}
	if instruction == "" {
		return false
	}

	if nc.MR == nil || nc.MR.URL == "" {
		h.logger.Debug("webhook: mention detected but no MR context")
		return false
	}

	author, ok := h.isAuthorized(ctx, nc)
	if !ok {
		h.logger.Warn("webhook: rejecting mention — author lacks mention capability on user bead",
			"trigger", matchedTrigger,
			"mr_url", nc.MR.URL,
			"author", nc.Author,
			"capability", GitLabMentionCapability)
		return true
	}

	// Resolve the human operator from the authorized user identity and stamp
	// it on the context BEFORE any daemon write, so the audit operated_by
	// column lands on the human (slack:U… / oidc:<email>) rather than
	// fleet:gitlab-bridge (ka-0000004627). Falls back to the gitlab:<user>
	// field / fleet sentinel inside spawnMentionAgent for authors whose user
	// bead carries neither a Slack id nor an email.
	if op := operatorFromGitLabUser(author); op != "" {
		ctx = beadsapi.WithOperator(ctx, op)
	}

	h.logger.Info("webhook: mention detected",
		"trigger", matchedTrigger, "mr_url", nc.MR.URL, "author", nc.Author, "instruction", truncateStr(instruction, 100))

	// Dedup: check if an open mention task already exists for this discussion.
	if nc.DiscussionID != "" {
		if h.hasPendingMention(ctx, nc.MR.URL, nc.DiscussionID) {
			h.logger.Info("webhook: skipping duplicate mention, open task exists",
				"mr_url", nc.MR.URL, "discussion_id", nc.DiscussionID)
			return true
		}
	}

	if err := h.spawnMentionAgent(ctx, nc, instruction, author); err != nil {
		h.logger.Error("webhook: failed to spawn agent for mention",
			"mr_url", nc.MR.URL, "error", err)
		return false
	}

	return true
}

// spawnMentionAgent creates a task bead and spawns an agent for a GitLab
// mention. author is the resolved *UserIdentity for the comment author (nil
// only when the caller could not resolve one — in practice always non-nil
// here since HandleMention rejects unresolved authors before calling). It is
// used to stamp the human operator on the task + agent beads (ka-0000004627).
func (h *MentionHandler) spawnMentionAgent(ctx context.Context, nc noteContext, instruction string, author *UserIdentity) error {
	// Service-identity writes — see ka-0000005187; bridge runs under
	// fleet:gitlab-bridge for daemon auth. The audit operated_by column is
	// resolved from the human operator stamped on ctx by HandleMention
	// (beadsapi.WithOperator) and mirrored into the operated_by field below
	// so spawned agents inherit it via BEADS_OPERATED_BY.

	// Build task title.
	title := fmt.Sprintf("GitLab mention on MR !%d: %s", nc.MR.IID, truncateStr(instruction, 80))

	// Build description with full context.
	var desc strings.Builder
	fmt.Fprintf(&desc, "GitLab @gardenio mention by @%s on [MR !%d](%s)\n\n", nc.Author, nc.MR.IID, nc.MR.URL)
	if nc.Position != nil && nc.Position.NewPath != "" {
		fmt.Fprintf(&desc, "**File:** `%s:%d`\n\n", nc.Position.NewPath, nc.Position.NewLine)
	}
	fmt.Fprintf(&desc, "**Instruction:**\n%s\n", instruction)
	if nc.DiscussionID != "" {
		fmt.Fprintf(&desc, "\n**Discussion ID:** %s\n", nc.DiscussionID)
	}

	// Build fields for the task bead.
	fields := map[string]string{
		"mr_url":         nc.MR.URL,
		"gitlab_mr_iid":  strconv.Itoa(nc.MR.IID),
		"mention_author": nc.Author,
		"spawn_source":   "gitlab-mention",
		// Persist the raw instruction so the catchup watchdog can rebuild an
		// equivalent agent prompt (buildMentionPrompt) when it respawns a crew
		// for a task whose webhook-time SpawnAgent was lost — the prompt is not
		// otherwise recoverable from the task bead alone (ka-0000000279 / A1).
		"mention_instruction": instruction,
	}
	// Push operator at create-time so the task bead carries operated_by from
	// the first row (R1 #10 push-at-create). HandleMention stamps the human
	// operator (slack:U… / oidc:<email>) on ctx via beadsapi.WithOperator
	// when the author resolves to a known user, so the audit operated_by
	// column AND this field both record the human (ka-0000004627). Only when
	// the author does not resolve to a Slack id / email do we fall back to
	// gitlab:<user>, and finally to the fleet sentinel via the empty case.
	if op := beadsapi.OperatorFromContext(ctx); op != "" {
		fields["operated_by"] = op
	} else if nc.Author != "" {
		fields["operated_by"] = "gitlab:" + nc.Author
	}
	// Mirror the Slack mention path: record slack_user_id + user_bead_id when
	// the resolved identity carries them, so decision notifications can
	// @mention the right person and downstream writes can hop to the user
	// bead. gitlab_username is kept below for provenance regardless.
	if author != nil {
		if id := strings.TrimSpace(author.SlackID); id != "" {
			fields["slack_user_id"] = id
		}
		if author.BeadID != "" {
			fields["user_bead_id"] = author.BeadID
		}
	}
	if nc.Author != "" {
		fields["gitlab_username"] = nc.Author
	}
	if nc.DiscussionID != "" {
		fields["gitlab_discussion_id"] = nc.DiscussionID
	}
	if nc.Position != nil && nc.Position.NewPath != "" {
		fields["mention_file"] = nc.Position.NewPath
		fields["mention_line"] = strconv.Itoa(nc.Position.NewLine)
	}

	// Resolve MR details for source branch + project ID.
	var sourceBranch string
	ref := ParseMRURL(nc.MR.URL)
	if ref != nil && h.gitlab != nil {
		mr, err := h.gitlab.GetMergeRequestByPath(ctx, ref.ProjectPath, ref.IID)
		if err == nil && mr != nil {
			sourceBranch = mr.SourceBranch
			fields["gitlab_project_id"] = strconv.Itoa(mr.ProjectID)
			fields["gitlab_mr_source_branch"] = sourceBranch
		} else if err != nil {
			h.logger.Warn("webhook: failed to fetch MR details for mention",
				"mr_url", nc.MR.URL, "error", err)
		}
	}

	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		return fmt.Errorf("marshalling fields: %w", err)
	}

	// Resolve project from MR URL by matching against project beads' git_url.
	// Falls back to the static h.project if no match is found.
	project := h.projectFromMRURL(ctx, nc.MR.URL)
	taskID, err := h.daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:       title,
		Type:        "task",
		Description: desc.String(),
		Labels:      []string{"project:" + project, "gitlab-mention"},
		Priority:    2,
		CreatedBy:   "gitlab-bridge",
		Fields:      json.RawMessage(fieldsJSON),
	})
	if err != nil {
		return fmt.Errorf("creating mention task bead: %w", err)
	}

	h.logger.Info("webhook: created mention task bead",
		"bead", taskID, "mr_url", nc.MR.URL, "author", nc.Author)

	// Auto-link M2b (epic ka-0000000933): write a `triggered-by` edge from
	// the freshly-created mention task → MR bead, so reverse-graph queries
	// can find every work bead spawned off an MR. Best-effort — if the MR
	// bead does not yet exist (fresh-MR race), the MR-sync webhook path
	// will backfill the edge on first upsert. See design ka-0000000827.
	h.linkMentionTaskToMR(ctx, taskID, nc.MR.URL)

	// Build the agent prompt.
	prompt := buildMentionPrompt(nc, instruction, taskID, sourceBranch)

	// Check if an active agent is already working on this MR.
	// If so, nudge it with the new mention instead of spawning another agent.
	if h.resolver != nil && h.nudge != nil {
		if existingAgent, found := h.resolver.FindActiveAgentForMR(ctx, nc.MR.URL); found {
			h.logger.Info("webhook: nudging existing agent for MR mention instead of spawning",
				"agent", existingAgent, "task", taskID, "mr_url", nc.MR.URL)
			if err := h.nudge(ctx, existingAgent, prompt); err != nil {
				h.logger.Warn("webhook: failed to nudge existing agent, will spawn new one",
					"agent", existingAgent, "error", err)
				// Fall through to spawn below.
			} else {
				// Track A3 (plan ka-0000004007): the nudge succeeded, so NO new
				// agent bead was created for this task — the work is covered by
				// the live agent we nudged. But the task bead we just created
				// (line ~340) now has no linked agent bead, which is exactly the
				// shape the A1 catchup watchdog (gitlab_mention_catchup.go) reads
				// as an orphan: spawn_source=gitlab-mention, active, no live
				// linked agent. Left unmarked, the watchdog would later respawn a
				// redundant crew and post a duplicate MR comment. Stamp the task
				// with the agent it was delivered to so the watchdog's orphan
				// predicate can subtract it — and link the task under the live
				// agent's own task_id so hasLiveAgent-style liveness reads true.
				h.recordMentionNudgeDelivery(ctx, taskID, existingAgent, nc.MR.URL)
				return nil
			}
		}
	}

	// Reuse the original agent name if one is already assigned to a bead for
	// this MR. This ensures session continuity — the entrypoint finds the
	// existing PVC/JSONL and resumes the agent's conversation.
	agentName := h.findExistingMRAgent(ctx, nc.MR.URL)
	if agentName != "" {
		h.logger.Info("webhook: reusing existing agent name for MR mention",
			"agent", agentName, "mr_url", nc.MR.URL)
	} else {
		agentName = mentionAgentName(nc.MR.IID)
	}

	// Extra fields for MR binding on the agent bead.
	extraFields := map[string]string{
		"gitlab_mr_url": nc.MR.URL,
		"gitlab_mr_iid": strconv.Itoa(nc.MR.IID),
		"spawn_source":  "gitlab-mention",
		"mention_task":  taskID,
	}
	// Mirror operator + GitLab identity onto the agent bead so child writes
	// (the agent's own daemon calls) inherit the human via BEADS_OPERATED_BY
	// env injection (R1 #10 push-at-create; the !2710 pattern). Bridge runs
	// under fleet:gitlab-bridge for daemon auth (ka-0000005187), but the
	// agent's operated_by field records the originating human (slack:U… /
	// oidc:<email>) resolved by HandleMention — falling back to gitlab:<user>
	// only when the author has no Slack id / email.
	if op := beadsapi.OperatorFromContext(ctx); op != "" {
		extraFields["operated_by"] = op
	} else if nc.Author != "" {
		extraFields["operated_by"] = "gitlab:" + nc.Author
	}
	if author != nil {
		if id := strings.TrimSpace(author.SlackID); id != "" {
			extraFields["slack_user_id"] = id
		}
		if author.BeadID != "" {
			extraFields["user_bead_id"] = author.BeadID
		}
	}
	if nc.Author != "" {
		extraFields["gitlab_username"] = nc.Author
	}
	if fields["gitlab_project_id"] != "" {
		extraFields["gitlab_project_id"] = fields["gitlab_project_id"]
	}
	if sourceBranch != "" {
		extraFields["gitlab_mr_source_branch"] = sourceBranch
	}

	agentBeadID, err := h.daemon.SpawnAgent(ctx, agentName, project, taskID, "crew", prompt, extraFields)
	if err != nil {
		// Track A2 (plan ka-0000004007): this is the exact hole that dropped all
		// three of Patrick's !1160 review comments on 2026-07-22 — CreateBead
		// succeeded, SpawnAgent failed, and the drop was INVISIBLE to operators
		// (the GitLab path had no equivalent of the Slack path's "nudge failed…"
		// card). The A1 catchup watchdog (gitlab_mention_catchup.go) is the
		// durable auto-recovery; this signal makes the failure operator-VISIBLE
		// in the meantime so a human is never left wondering why the bot went
		// silent. Best-effort — a notification failure must not mask the spawn
		// error, so we log and still return the error.
		h.notifyMentionDispatchFailed(ctx, nc, taskID, err)
		return fmt.Errorf("spawning agent %q for mention: %w", agentName, err)
	}

	h.logger.Info("webhook: spawned agent for GitLab mention",
		"agent", agentName, "agent_bead", agentBeadID,
		"task", taskID, "mr_url", nc.MR.URL, "project", project)

	return nil
}

// notifyMentionDispatchFailed makes a lost GitLab-mention dispatch
// operator-VISIBLE (Track A2, plan ka-0000004007). When SpawnAgent fails after
// the task bead was already created, the request would otherwise sit orphaned
// with no pod and no signal — the exact silence Patrick hit on !1160. This
// posts two best-effort notifications:
//
//  1. A bead comment on the task, which the bridge forwards to Slack, so the
//     operator who triggered the mention sees the failure in their thread.
//  2. A GitLab MR note, so the reviewer who @-mentioned the bot on the MR sees
//     it in-context on the same MR — mirroring the Slack path's visible
//     "nudge failed…" card that the GitLab path previously lacked.
//
// Both are best-effort and never returned: the caller still returns the spawn
// error (the A1 catchup watchdog is the durable auto-recovery). A comment/note
// failure is logged, not propagated, so a flaky notify path cannot itself
// become a new silent-drop vector.
func (h *MentionHandler) notifyMentionDispatchFailed(ctx context.Context, nc noteContext, taskID string, spawnErr error) {
	// Bead comment → Slack forward. The task bead carries the operator, so the
	// forward reaches the human who triggered the mention.
	if taskID != "" {
		comment := fmt.Sprintf(
			"⚠️ GitLab-mention dispatch FAILED: created this task but could not spawn a crew agent (%v). "+
				"The request is NOT lost — the dispatch-catchup watchdog (ka-0000000279) will respawn a crew "+
				"automatically within a few minutes. This note is so the drop is never silent (Track A2, ka-0000004007).",
			spawnErr)
		if err := h.daemon.AddComment(ctx, taskID, mentionDispatchActor, comment); err != nil {
			h.logger.Warn("webhook: failed to post dispatch-failure comment on mention task",
				"task", taskID, "error", err)
		}
	}

	// GitLab MR note — surface it on the MR itself so the reviewer sees it in
	// context. Best-effort: needs a live GitLab client and a parseable MR URL.
	if h.gitlab == nil || nc.MR == nil || nc.MR.URL == "" {
		return
	}
	ref := ParseMRURL(nc.MR.URL)
	if ref == nil {
		return
	}
	note := fmt.Sprintf(
		"⚠️ @%s — I created a task for your request but hit an error spawning the crew agent to handle it. "+
			"It will be picked up automatically by the dispatch-catchup watchdog within a few minutes"+
			"%s. (You can re-mention to retry immediately.)",
		nc.Author, taskLinkSuffix(taskID))
	if _, err := h.gitlab.PostMRNote(ctx, ref.ProjectPath, ref.IID, note); err != nil {
		h.logger.Warn("webhook: failed to post dispatch-failure MR note",
			"mr_url", nc.MR.URL, "error", err)
	}
}

// taskLinkSuffix renders a " (tracking: <id>)" suffix when a task id is known,
// or the empty string otherwise, so the MR note reads cleanly in both cases.
func taskLinkSuffix(taskID string) string {
	if taskID == "" {
		return ""
	}
	return fmt.Sprintf(" (tracking: %s)", taskID)
}

// recordMentionNudgeDelivery marks a mention task whose request was delivered
// by NUDGING a live agent (rather than spawning a new one) so the A1 catchup
// watchdog does not later mistake it for an orphan and respawn a redundant crew
// (Track A3, plan ka-0000004007). The task bead created for the mention has no
// linked agent bead in this branch — the work is covered by the already-running
// agent we nudged — which is exactly the "no live linked agent" shape the
// watchdog's orphan predicate keys on.
//
// The stamp is defensive-in-depth:
//   - nudged_agent records which live agent the mention was handed to, for
//     provenance and operator debugging.
//   - nudge_delivered_at gives the watchdog a positive "this was covered by a
//     nudge, not dropped" signal it can subtract without a GitLab round-trip.
//
// Best-effort: a stamp failure is logged, not returned — the nudge already
// succeeded, so the request is delivered regardless. The worst case of a failed
// stamp is one redundant watchdog respawn later, which is idempotent-once and
// far cheaper than the silent-drop this whole plan exists to prevent.
func (h *MentionHandler) recordMentionNudgeDelivery(ctx context.Context, taskID, agentName, mrURL string) {
	if taskID == "" {
		return
	}
	fields := map[string]string{
		"nudged_agent":       agentName,
		"nudge_delivered_at": timeNowRFC3339(),
	}
	if err := h.daemon.UpdateBeadFields(ctx, taskID, fields); err != nil {
		h.logger.Warn("webhook: failed to stamp nudge-delivery on mention task",
			"task", taskID, "agent", agentName, "mr_url", mrURL, "error", err)
	}
}

// timeNowRFC3339 returns the current time as an RFC3339 string. Split out so
// tests can assert the stamp is present without pinning a wall-clock value.
func timeNowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// linkMentionTaskToMR writes a `triggered-by` dep from the mention task
// bead → MR bead so reverse-graph queries can join work beads back to the
// upstream MR. Best-effort: if the MR bead lookup fails or returns nil (the
// fresh-MR race where the bead has not yet been upserted), the edge is
// dropped and the MR-sync webhook path backfills it on next upsert. See
// M2b on epic ka-0000000933 and design ka-0000000827.
func (h *MentionHandler) linkMentionTaskToMR(ctx context.Context, taskID, mrURL string) {
	if taskID == "" || mrURL == "" {
		return
	}
	mrBead, err := h.daemon.MRBeadByURL(ctx, mrURL)
	if err != nil {
		h.logger.Warn("webhook: failed to lookup MR bead for triggered-by dep",
			"task", taskID, "mr_url", mrURL, "error", err)
		return
	}
	if mrBead == nil {
		h.logger.Debug("webhook: MR bead not yet upserted, deferring triggered-by dep to MR-sync",
			"task", taskID, "mr_url", mrURL)
		return
	}
	if err := h.daemon.AddDependency(ctx, taskID, mrBead.ID, "triggered-by", "gitlab-bridge"); err != nil {
		h.logger.Warn("webhook: failed to add triggered-by dep on mention task",
			"task", taskID, "mr_bead", mrBead.ID, "mr_url", mrURL, "error", err)
	}
}

// projectFromMRURL resolves the project name from a GitLab MR URL by matching
// the URL's host+path against project beads' git_url AND repos[].url fields.
// For example, MR URL "https://gitlab.com/example-org/monorepo/-/merge_requests/422"
// matches a project bead with git_url "https://gitlab.com/example-org/monorepo".
// MRs in repos listed under repos[] on a project bead (e.g. site-deployment,
// k6-tests) also resolve to that project rather than falling back to the
// default. Falls back to h.project (the static default) only when no project's
// git_url or repos[] entry matches.
func (h *MentionHandler) projectFromMRURL(ctx context.Context, mrURL string) string {
	ref := ParseMRURL(mrURL)
	if ref == nil {
		h.logger.Debug("webhook: could not parse MR URL for project resolution, using default",
			"mr_url", mrURL, "default_project", h.project)
		return h.project
	}

	// Extract scheme+host from the MR URL to reconstruct the repo base URL.
	parsed, err := url.Parse(mrURL)
	if err != nil {
		h.logger.Debug("webhook: could not parse MR URL, using default project",
			"mr_url", mrURL, "error", err)
		return h.project
	}
	// Build the expected git_url: scheme://host/ProjectPath
	repoURL := fmt.Sprintf("%s://%s/%s", parsed.Scheme, parsed.Host, ref.ProjectPath)

	projects, err := h.daemon.ListProjectBeads(ctx)
	if err != nil {
		h.logger.Warn("webhook: failed to list project beads for MR URL resolution, using default",
			"error", err, "default_project", h.project)
		return h.project
	}

	// First pass: prefer a project whose top-level git_url matches. Collect
	// ALL git_url matches, then pick a single deterministic winner. Iterating
	// a Go map and returning the first match is nondeterministic when two
	// project beads share a git_url (the aws-cost-management/pooltest→gardenio
	// latent case): identical MRs would split-route across a mention burst
	// depending on map iteration order (bugs ka-0000002164, ka-0000005075).
	var gitURLMatches []beadsapi.ProjectInfo
	for _, info := range projects {
		if matchGitURL(info.GitURL, repoURL) {
			gitURLMatches = append(gitURLMatches, info)
		}
	}
	if len(gitURLMatches) > 0 {
		winner := pickGitURLWinner(gitURLMatches)
		if len(gitURLMatches) > 1 {
			h.logger.Warn("webhook: multiple projects claim the same git_url; picking deterministic winner",
				"mr_url", mrURL, "repo_url", repoURL,
				"candidates", projectNames(gitURLMatches),
				"winner", winner.Name, "winner_bead", winner.BeadID)
		}
		h.logger.Info("webhook: resolved project from MR URL via git_url",
			"mr_url", mrURL, "project", winner.Name, "git_url", winner.GitURL)
		return winner.Name
	}

	// Second pass: match against repos[] entries. The monorepo project bead
	// lists multiple GitLab repos (site-deployment, gardenio, k6-tests) under
	// `repos`; MRs in those should route to that project rather than the
	// default fallback. Collect ALL repos[] matches (with the matched entry's
	// role) then pick an ownership-aware, deterministic winner so a repo that
	// is referenced by several projects (e.g. a satellite that only holds a
	// reference to it) never split-routes.
	var reposMatches []reposMatch
	for _, info := range projects {
		for _, r := range info.Repos {
			if matchGitURL(r.URL, repoURL) {
				reposMatches = append(reposMatches, reposMatch{Info: info, Role: r.Role})
				break // one match per project is enough to make it a candidate
			}
		}
	}
	if len(reposMatches) > 0 {
		winner := pickReposWinner(reposMatches)
		if len(reposMatches) > 1 {
			h.logger.Warn("webhook: multiple projects list the same repo in repos[]; picking ownership-aware winner",
				"mr_url", mrURL, "repo_url", repoURL,
				"candidates", reposMatchNames(reposMatches),
				"winner", winner.Name, "winner_bead", winner.BeadID)
		}
		h.logger.Info("webhook: resolved project from MR URL via repos[]",
			"mr_url", mrURL, "project", winner.Name, "repo_url", repoURL)
		return winner.Name
	}

	h.logger.Info("webhook: no project matched MR URL, using default",
		"mr_url", mrURL, "repo_url", repoURL, "default_project", h.project)
	return h.project
}

// reposMatch pairs a project that matched an MR repo URL via its repos[] list
// with the role of the specific repo entry that matched ("primary" owns the
// repo, "reference" merely clones it). pickReposWinner uses Role to prefer the
// owning project.
type reposMatch struct {
	Info beadsapi.ProjectInfo
	Role string
}

// pickGitURLWinner selects a single deterministic winner among projects whose
// top-level git_url matched the MR repo URL. matchGitURL already normalizes
// both sides, so all matches are equally exact; the tie-break is simply the
// lexicographically smallest BeadID. BeadID (not Name) is used because it is
// guaranteed unique — two beads could theoretically share a Name. This mirrors
// the ListProjectBeads duplicate-name winner rule ("lowest bead ID wins").
// Assumes len(matches) > 0.
func pickGitURLWinner(matches []beadsapi.ProjectInfo) beadsapi.ProjectInfo {
	winner := matches[0]
	for _, m := range matches[1:] {
		if m.BeadID < winner.BeadID {
			winner = m
		}
	}
	return winner
}

// pickReposWinner selects a single deterministic winner among projects that
// matched the MR repo URL via their repos[] list, using an ownership-aware
// priority ladder:
//  1. Prefer a project whose matched repo entry has Role=="primary" (it OWNS
//     the repo) over a "reference" entry.
//  2. Among remaining ties, prefer a project that owns some repo (non-empty
//     GitURL) over a pure-reference satellite (empty GitURL) — e.g. monorepo
//     (GitURL set) outranks devsecops (GitURL empty) when both merely list a
//     shared repo as a reference.
//  3. Final stable tie-break: lexicographically smallest BeadID.
//
// Assumes len(matches) > 0.
func pickReposWinner(matches []reposMatch) beadsapi.ProjectInfo {
	winner := matches[0]
	for _, m := range matches[1:] {
		if reposMatchLess(m, winner) {
			winner = m
		}
	}
	return winner.Info
}

// reposMatchLess reports whether candidate a outranks candidate b under the
// pickReposWinner priority ladder (primary role > owner > lowest BeadID).
func reposMatchLess(a, b reposMatch) bool {
	aPrimary := a.Role == "primary"
	bPrimary := b.Role == "primary"
	if aPrimary != bPrimary {
		return aPrimary // primary role wins
	}
	aOwner := a.Info.GitURL != ""
	bOwner := b.Info.GitURL != ""
	if aOwner != bOwner {
		return aOwner // owner (non-empty git_url) wins over pure-reference satellite
	}
	return a.Info.BeadID < b.Info.BeadID // stable tie-break
}

// projectNames returns the names of the given projects for conflict logging.
func projectNames(infos []beadsapi.ProjectInfo) []string {
	names := make([]string, len(infos))
	for i, info := range infos {
		names[i] = info.Name
	}
	return names
}

// reposMatchNames returns the names of the projects in the given repos matches
// for conflict logging.
func reposMatchNames(matches []reposMatch) []string {
	names := make([]string, len(matches))
	for i, m := range matches {
		names[i] = m.Info.Name
	}
	return names
}

// matchGitURL compares two Git URLs for equivalence, normalizing trailing
// slashes and ".git" suffixes. This handles the common case where a project
// bead has git_url "https://gitlab.com/org/repo" or "https://gitlab.com/org/repo.git".
// It delegates to beadsapi.NormalizeGitURL so MR-URL resolution and the
// GitURLConflicts/ReposConflicts detectors share one canonical normalization
// rule — a collision the resolver tie-breaks is exactly the one the detectors
// flag.
func matchGitURL(a, b string) bool {
	return beadsapi.NormalizeGitURL(a) == beadsapi.NormalizeGitURL(b)
}

// mentionDedupWindow bounds how long an open mention task suppresses a
// re-mention on the same discussion. The dedup gate exists only to collapse a
// burst of identical webhook deliveries for a single comment while the spawned
// agent boots — NOT to mute a discussion for the lifetime of an open task.
//
// An open task is NOT proof of a live agent: the spawned agent can crash/die
// without closing its task (assignee cleared, status stays open), and before
// this window was introduced (ka-0000001106) such an orphaned task muted every
// subsequent @gardenio on the discussion FOREVER. Bounding the suppression to a
// short window means a genuine follow-up minutes later always falls through to
// spawnMentionAgent, which itself nudges a live MR agent or respawns a dead one
// (findExistingMRAgent / FindActiveAgentForMR) — so no re-mention is ever
// silently dropped, while true webhook-retry bursts (delivered within seconds)
// are still collapsed.
const mentionDedupWindow = 3 * time.Minute

// hasPendingMention checks whether a *recently-created* open mention task bead
// already exists for the same MR + discussion ID, i.e. whether this note is a
// duplicate webhook delivery of a comment we just acted on. Walks the MR bead's
// reverse `produces` deps (epic ka-0000001613) — the legacy task-bead mr_url
// scan was retired in task ka-0000008134. Returns false when no MR bead is
// linked yet (e.g. a fresh mention the MR webhook has not observed).
//
// A matching open task only suppresses when it was created within
// mentionDedupWindow. Older open tasks are treated as NOT pending (ka-0000001106):
// their agent may be long dead, and continuing to suppress would permanently
// mute the discussion. Letting the note through lands it in spawnMentionAgent,
// which nudges a still-live agent or respawns the dead one under the same name.
func (h *MentionHandler) hasPendingMention(ctx context.Context, mrURL, discussionID string) bool {
	tasks := h.taskBeadsViaMR(ctx, mrURL)
	// Scan ALL open mention tasks for this discussion before deciding, so the
	// outcome is independent of iteration order: a discussion carrying both a
	// fresh task (a genuine burst) and a stale orphan must still dedup against
	// the fresh one. Suppress only if some matching open task is fresh; if the
	// discussion has open mention tasks but every one is stale, fall through.
	var staleOrphan *beadsapi.BeadDetail
	for _, b := range tasks {
		if b.Status == "closed" {
			continue
		}
		if b.Fields["gitlab_discussion_id"] != discussionID ||
			b.Fields["spawn_source"] != "gitlab-mention" {
			continue
		}
		// Fresh open task ⇒ collapse this as a duplicate webhook delivery.
		// A zero CreatedAt (unknown age) fails safe as "fresh" so we do not
		// spam-spawn when the timestamp is simply missing.
		if b.CreatedAt.IsZero() || time.Since(b.CreatedAt) < mentionDedupWindow {
			return true
		}
		staleOrphan = b
	}
	if staleOrphan == nil {
		return false
	}
	// Only stale open mention task(s) on the discussion ⇒ do NOT suppress. The
	// agent is likely orphaned (crashed without closing its task); muting here
	// is the permanent-mute bug (ka-0000001106). Fall through so the re-mention
	// re-spawns or nudges via spawnMentionAgent. Surface the previously-silent
	// pass-through so it is observable (agents can't read pod logs).
	recordMentionStaleTaskPassthrough()
	h.logger.Info("webhook: stale open mention task on discussion, not suppressing re-mention",
		"mr_url", mrURL, "discussion_id", discussionID,
		"stale_task", staleOrphan.ID, "stale_assignee", staleOrphan.Assignee,
		"task_age", time.Since(staleOrphan.CreatedAt).Round(time.Second).String())
	return false
}

// findExistingMRAgent looks for a task bead linked to the given MR URL that
// has an assigned agent. Returns the agent name if found, empty string
// otherwise. Includes closed beads so that a re-mention on an MR after the
// original task is done still resumes the same agent. Walks the MR bead's
// reverse `produces` deps (epic ka-0000001613) — the legacy task-bead
// mr_url scan was retired in task ka-0000008134.
func (h *MentionHandler) findExistingMRAgent(ctx context.Context, mrURL string) string {
	tasks := h.taskBeadsViaMR(ctx, mrURL)
	for _, b := range tasks {
		if b.Assignee != "" {
			return b.Assignee
		}
	}
	return ""
}

// taskBeadsViaMR returns the producing task beads for an MR URL by walking
// MR bead → reverse `produces` deps. Returns nil when no MR bead exists for
// the URL yet (e.g. a fresh mention that the MR webhook has not observed),
// or the lookup errors — callers treat this as "no producing task beads
// found" rather than falling back to a legacy field scan, since the
// task-bead mr_* writes were dropped in task ka-0000008134.
func (h *MentionHandler) taskBeadsViaMR(ctx context.Context, mrURL string) []*beadsapi.BeadDetail {
	if mrURL == "" {
		return nil
	}
	mrBead, err := h.daemon.MRBeadByURL(ctx, mrURL)
	if err != nil {
		h.logger.Debug("webhook: MR bead lookup failed",
			"mr_url", mrURL, "error", err)
		return nil
	}
	if mrBead == nil {
		return nil
	}
	tasks, err := h.daemon.ProducingTaskBeadsForMR(ctx, mrBead.ID)
	if err != nil {
		h.logger.Debug("webhook: reverse-dep lookup failed",
			"mr_bead", mrBead.ID, "error", err)
		return nil
	}
	return tasks
}

// extractMentionInstruction extracts the instruction text after a @gardenio
// mention trigger. Returns empty string if no mention is found.
// The match is case-insensitive and requires the trigger to be a whole
// username token — e.g., trigger "@gardenio" must NOT match inside "@gardeniobot".
func extractMentionInstruction(note, trigger string) string {
	// Use ASCII-only case folding, NOT strings.ToLower: the mention trigger is
	// always ASCII, and strings.ToLower is not length-preserving for some
	// runes (e.g. U+023A Ⱥ is 2 bytes but its lowercase ⱥ is 3), so byte
	// offsets computed on strings.ToLower(note) could exceed len(note) and
	// panic when slicing the original note (ka-0000002216). asciiToLower keeps
	// every byte position identical to note.
	lower := asciiToLower(note)
	search := 0
	for search < len(lower) {
		rel := strings.Index(lower[search:], trigger)
		if rel < 0 {
			return ""
		}
		idx := search + rel
		end := idx + len(trigger)
		// Require a boundary after the trigger: end of string, or a rune
		// that is not a valid GitLab username character. GitLab usernames
		// allow [a-zA-Z0-9._-]; anything else marks the end of the mention.
		if end < len(note) && isUsernameChar(note[end]) {
			search = end
			continue
		}
		after := strings.TrimSpace(note[end:])
		if after == "" {
			return ""
		}
		return after
	}
	return ""
}

// isUsernameChar reports whether b is a valid character inside a GitLab
// username (alnum, '.', '_', '-').
func isUsernameChar(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z':
		return true
	case b >= 'A' && b <= 'Z':
		return true
	case b >= '0' && b <= '9':
		return true
	case b == '.' || b == '_' || b == '-':
		return true
	}
	return false
}

// asciiToLower lowercases ASCII letters A-Z and leaves every other byte
// untouched. Unlike strings.ToLower it is byte-length-preserving, so offsets
// computed on the result index into the original string safely. This is exactly
// what mention parsing needs: the trigger token is ASCII, and we must slice the
// original (possibly multi-byte UTF-8) note using offsets from the folded copy.
func asciiToLower(s string) string {
	// Fast path: nothing to fold.
	hasUpper := false
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'A' && c <= 'Z' {
			hasUpper = true
			break
		}
	}
	if !hasUpper {
		return s
	}
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// mentionAgentName generates a unique agent name for a GitLab mention.
func mentionAgentName(mrIID int) string {
	return fmt.Sprintf("mr-%d-%s", mrIID, randomSuffix(6))
}

// buildMentionPrompt creates the custom prompt for a mention-spawned agent.
func buildMentionPrompt(nc noteContext, instruction, taskID, sourceBranch string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "You were spawned by a @gardenio mention on GitLab MR !%d (%s).\n", nc.MR.IID, nc.MR.URL)
	fmt.Fprintf(&sb, "Author: @%s\n\n", nc.Author)

	if nc.Position != nil && nc.Position.NewPath != "" {
		fmt.Fprintf(&sb, "This is an inline comment on `%s:%d`.\n\n", nc.Position.NewPath, nc.Position.NewLine)
	}

	fmt.Fprintf(&sb, "Instruction: %s\n\n", instruction)

	if sourceBranch != "" {
		fmt.Fprintf(&sb, "Check out branch `%s` to work on this MR.\n", sourceBranch)
	}

	fmt.Fprintf(&sb, "Your task bead: %s — use `kd show %s` for full context.\n", taskID, taskID)
	// Opt-in MR mirror (ka-0000005748): plain `gb squawk` is Slack-only. Only a
	// reply the reviewer needs to see on the MR should use `--to-mr`; routine
	// progress must NOT, or it floods the human review thread.
	fmt.Fprintf(&sb, "Post progress via `gb squawk` (Slack-only). To reply to the reviewer ON the MR, use `gb squawk --to-mr '<reply>'` — reserve it for a direct answer or your final summary, not routine progress.\n")
	fmt.Fprintf(&sb, "When done, `kd close %s` and `gb stop`.\n", taskID)

	return sb.String()
}

// truncateStr shortens a string to maxLen, appending "..." if truncated.
func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}
