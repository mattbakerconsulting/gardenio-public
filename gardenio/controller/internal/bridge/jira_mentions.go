// Package bridge provides the JIRA @mention handler.
//
// JiraMentionHandler detects @gardenio mentions in JIRA comment_created
// webhooks, supporting both plain text (Jira Server) and ADF (Jira Cloud)
// comment bodies. When a mention is detected, it creates a task bead and
// spawns an agent (or nudges an existing one). Modeled after the GitLab
// MentionHandler in gitlab_mentions.go.
package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// jiraUserBackfiller is the optional capability used to persist a resolved
// jira_user=<accountId> back onto a user bead so future mentions resolve
// directly via the jira_user index. *beadsapi.Client satisfies it; the
// JiraMentionBeadClient interface stays minimal and the backfill is skipped
// when the wired daemon does not implement it.
type jiraUserBackfiller interface {
	UpdateBeadFields(ctx context.Context, beadID string, fields map[string]string) error
}

// JiraMentionBeadClient is the subset of beadsapi.Client used by JiraMentionHandler.
type JiraMentionBeadClient interface {
	CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error)
	SpawnAgent(ctx context.Context, agentName, project, taskID, role, customPrompt string, extraFields ...map[string]string) (string, error)
	ListTaskBeadsAll(ctx context.Context) ([]*beadsapi.BeadDetail, error)
	// ListTaskBeadsForJiraKey narrows the lookup to one ticket server-side so
	// dedup/agent-reuse checks do not list the whole task corpus (ka-0000007917).
	ListTaskBeadsForJiraKey(ctx context.Context, jiraKey string) ([]*beadsapi.BeadDetail, error)
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	// AddDependency writes a typed edge from beadID → dependsOnID. Used to
	// stamp a `triggered-by` lineage edge from the mention task bead to the
	// Jira-mirror bead (auto-link epic ka-0000000933, M2c ka-0000003814).
	// All writes are gardenio-side deps rows on the mirror; the read-only
	// mirror contract (advice ka-0000003234) is preserved — no outbound
	// Jira API calls.
	AddDependency(ctx context.Context, beadID, dependsOnID, depType, createdBy string) error
}

// JiraMentionSpawnEvent holds details about a JIRA mention that spawned an agent.
type JiraMentionSpawnEvent struct {
	AgentName   string // spawned agent name
	AgentBeadID string // spawned agent bead ID
	TaskBeadID  string // task bead created for the mention
	JiraKey     string // JIRA issue key (e.g., "PE-1234")
	Summary     string // JIRA issue summary
	Author      string // comment author display name
	Instruction string // instruction text from the mention
	Project     string // gardenio project name
	// Role is the beads role the agent was spawned under (resolveJiraSpawnRole).
	// The spawn-ack acknowledger gates its comment on this role via the shared
	// write policy (JiraSpawnAcknowledger), so it must reflect the actual spawn
	// role, not a default.
	Role string
	// Operator is the resolved operator id (slack:U… / oidc:…@… when a human
	// resolved; jira:/fleet: sentinels attribute no line). OperatorDisplay is
	// the resolved fleet user's display name when known. Both feed the operator
	// attribution line stamped on the spawn-ack comment so the Jira history
	// stays honest about the driving human.
	Operator        string
	OperatorDisplay string
	// Quiet carries the mention-time --quiet intent hint (design-doc F-E, epic
	// ka-0000007401). When true the JiraSpawnAcknowledger suppresses its "👋 On
	// it" note so a low-noise ticket is not bracketed with automatic progress
	// chatter. The SSE-driven forwarders (squawk, completion) read the same fact
	// off the agent bead's jira_quiet field instead, since they have no event.
	Quiet bool
}

// JiraMentionNotifyFunc is called after a successful agent spawn from a JIRA mention.
// Implementations can use this to post Slack notifications, send emails, etc.
type JiraMentionNotifyFunc func(ctx context.Context, event JiraMentionSpawnEvent)

// JiraUserLookup is the subset of *UserDirectory used by JiraMentionHandler to
// resolve a JIRA comment author to a fleet user identity. The mention path is
// gated on it: only an author that resolves to a user bead holding the
// "mention" capability spawns an agent, attributed to that human
// (slack:<id> / oidc:<email>). Nil ⇒ every mention is refused (fail-closed,
// gd-fBBMiDHb07).
//
// Two lookup keys are needed because user beads almost never store the opaque
// Jira Cloud accountId: they carry an email (Keycloak-provisioned) and a
// human-readable jira_user. The handler tries jira_user first (covers Server
// usernames and any beads where jira_user==accountId after backfill), then
// email (the reliable Cloud key). See ka-0000004473.
type JiraUserLookup interface {
	LookupByJiraUser(ctx context.Context, jiraUser string) *UserIdentity
	LookupByEmail(ctx context.Context, email string) *UserIdentity
}

// JiraCommentHistoryClient is the subset of *JiraClient used to fetch a
// ticket's recent comment history so spawned/nudged agents see the prior
// conversation (F4, plan ka-0000007401). Nil ⇒ prompts carry no history.
type JiraCommentHistoryClient interface {
	GetComments(ctx context.Context, key string, maxResults int) ([]JiraCommentEntry, error)
}

// JiraMentionHandler detects @gardenio mentions in JIRA comment bodies and
// spawns agents to handle the instructions.
type JiraMentionHandler struct {
	daemon     JiraMentionBeadClient
	users      JiraUserLookup           // optional: resolves JIRA accountId → fleet human
	history    JiraCommentHistoryClient // optional: recent ticket comments for spawn prompts (F4)
	trigger    string                   // primary mention trigger (e.g., "@gardenio")
	botTrigger string                   // optional secondary trigger
	// botAccountIDs are the Jira accountIds a native @-mention of the bot
	// targets; a structured ADF mention node carrying one of these is detected
	// regardless of display text (epic ka-0000007401). Lowercased at
	// construction for case-insensitive compare.
	botAccountIDs []string
	project       string                // default project for spawned agents
	projectMap    map[string]string     // JIRA prefix (upper) → boat project; overrides project per-ticket
	onSpawn       JiraMentionNotifyFunc // optional: called after successful spawn
	logger        *slog.Logger

	// writePolicy is the config-driven, project-agnostic write policy
	// (ka-0000007252). When non-nil it is the SOLE project-verdict path — FSM is
	// just a configured toggle, not a compiled-in special case. The production
	// jira-bridge wires this. When nil (tests) the legacy projectGate below is
	// used; DefaultWritePolicy reproduces its project verdict byte-for-byte so the
	// swap is behavior-safe. Note: the mention path never reaches a JiraClient
	// write (asserted by TestJiraMention_NoJiraWriteSurface), so only the
	// spawn-eligibility gate (AllowSpawn) is consulted here — NOT the write gate
	// (AllowProject). AllowSpawn admits a Writable project regardless of its
	// outbox SyncToggle, so GBFSM mentions spawn even when JIRA_OUTBOX_ENABLED is
	// off (ka-0000001940).
	writePolicy *WritePolicyStore
	// projectGate is the coordinated deny-by-default project gate. LEGACY
	// fallback — used only when writePolicy is nil (FSM governed by the single
	// JIRA_FSM_SYNC_ENABLED toggle, ka-0000001679). Never nil after construction —
	// NewJiraMentionHandler falls back to the package default gate (FSM off) so
	// behavior is unchanged when unwired.
	projectGate *ProjectSyncGate
}

// JiraMentionHandlerConfig holds configuration for JiraMentionHandler.
type JiraMentionHandlerConfig struct {
	Daemon JiraMentionBeadClient
	// Users resolves a JIRA comment author's accountId / email to a fleet user
	// identity. It is both the authorization gate (the user bead must hold the
	// "mention" capability) and the attribution source for the task bead and
	// spawned agent. Nil ⇒ every mention is refused (gd-fBBMiDHb07).
	Users JiraUserLookup
	// History fetches the ticket's recent comments so the spawned agent's
	// prompt includes the prior conversation (F4, plan ka-0000007401). Nil ⇒
	// prompts carry only the triggering comment.
	History    JiraCommentHistoryClient
	Trigger    string // e.g., "@gardenio" (case-insensitive); defaults to "@gardenio"
	BotTrigger string // optional secondary trigger (e.g., a JIRA user mention)
	// BotAccountIDs are the Jira Cloud accountIds of the bot user(s) a native
	// @-mention targets (JIRA_MENTION_BOT_ACCOUNT_IDS, comma-separated). A
	// native @-mention is an ADF "mention" node carrying the target's
	// accountId; matching on it detects the mention regardless of the bot's
	// display name or whether Jira prefixed the display text with "@" (the real
	// prod GBFSM-85 miss, epic ka-0000007401). Empty ⇒ detection falls back to
	// the display-text trigger match alone, which still works when the mention
	// text carries the trigger token.
	BotAccountIDs []string
	Project       string // default project for spawned agents
	// ProjectMap routes a mention to a boat project by the ticket's JIRA
	// project key (upper, e.g. "PE" → "monorepo"). Same map the JQL poller
	// gets (cfg.jiraProjectMap from BOAT_PROJECTS). An unmapped prefix — and
	// an empty map, the single-project deployment case — falls back to
	// Project, so roster/advice/role resolution follows the mapping when
	// projects diverge (G3, epic ka-0000007401).
	ProjectMap map[string]string
	OnSpawn    JiraMentionNotifyFunc // optional: called after successful spawn
	Logger     *slog.Logger
	// WritePolicy is the config-driven, project-agnostic write policy store
	// (ka-0000007252). When set it is the sole project-verdict path and the legacy
	// ProjectGate below is ignored. The production jira-bridge sets this; nil
	// selects the legacy gate (tests).
	WritePolicy *WritePolicyStore
	// ProjectGate is the coordinated project gate shared across all four
	// outbound Jira paths (ka-0000001679). LEGACY fallback (used only when
	// WritePolicy is nil). nil ⇒ the package default gate (FSM off, legacy
	// deny-by-default) is used, so behavior is unchanged.
	ProjectGate *ProjectSyncGate
}

// JiraMentionResult holds the result of a mention detection check.
type JiraMentionResult struct {
	Detected    bool   // whether a mention was found
	Instruction string // the text after the trigger (plain text)
	// InstructionMarkdown is the instruction with the comment's ADF formatting
	// preserved as markdown — fenced code, inline code, links, lists, headings,
	// bold/italic (H3, epic ka-0000007401). Empty when detection ran on a
	// plain-text body (Jira Server) or the markdown render found no trigger; the
	// caller falls back to Instruction in that case.
	InstructionMarkdown string
	Trigger             string // which trigger matched
}

// blockedJiraProjects is the set of JIRA project keys that agents must never
// write to. FSM is a customer-facing service desk where comments are visible
// to external customers.
var blockedJiraProjects = map[string]bool{
	"FSM": true,
}

// allowedJiraProjects is the set of JIRA project keys authorized for agent
// spawning AND sync-back write-back. This is an enforced allow-list
// (deny-by-default): a project must appear here to be eligible for any
// outbound Jira write.
//   - PE     (Engineering)
//   - DEVOPS (DevOps/platform) — added per DEVOPS-777 (Matt Baker, 2026-06-16)
var allowedJiraProjects = map[string]bool{
	"PE":     true,
	"DEVOPS": true,
}

// jiraDarkLabel marks a bead as belonging to the in-gardenio "dark" universe.
// Dark-marked beads must never flow upstream to the real Jira — the sync-back
// handlers early-return on this label before any project gating
// (R7 ka-0000004670, ka-0000005143).
const jiraDarkLabel = "source:jira-dark"

// defaultProjectSyncGate is the package-level gate used by paths that have not
// been wired with an explicit *ProjectSyncGate (and by syncAllowedProject). It
// has the FSM toggle OFF, so its verdict is the legacy deny-by-default behavior
// (allow={PE,DEVOPS}, block={FSM}). Production wiring constructs its own gate
// from JIRA_FSM_SYNC_ENABLED and shares it across all four outbound paths so
// the FSM cutover is atomic (ka-0000001679).
var defaultProjectSyncGate = DefaultProjectSyncGate(false)

// syncAllowedProject reports whether the JIRA issue key belongs to a project
// that agents are permitted to write back to. It is deny-by-default: the
// project must be on the allow-list, and an explicit block (e.g. FSM) always
// wins even if mistakenly added to the allow-list. Replaces the former
// deny-list-only gate, which left every non-FSM project — including arbitrary
// agent-chosen keys — implicitly syncable (ka-0000005143).
//
// It delegates to defaultProjectSyncGate (FSM toggle off) so there is a single
// source of truth for the project verdict; the behavior is identical to the
// former inline logic, keeping every nil-gate caller and existing test
// unchanged (ka-0000001679).
func syncAllowedProject(jiraKey string) bool {
	return defaultProjectSyncGate.AllowProject(jiraKey)
}

// NewJiraMentionHandler creates a new JiraMentionHandler.
func NewJiraMentionHandler(cfg JiraMentionHandlerConfig) *JiraMentionHandler {
	trigger := cfg.Trigger
	if trigger == "" {
		trigger = "@gardenio"
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	gate := cfg.ProjectGate
	if gate == nil {
		gate = defaultProjectSyncGate
	}
	return &JiraMentionHandler{
		daemon:        cfg.Daemon,
		users:         cfg.Users,
		history:       cfg.History,
		trigger:       strings.ToLower(trigger),
		botTrigger:    strings.ToLower(cfg.BotTrigger),
		botAccountIDs: normalizeAccountIDs(cfg.BotAccountIDs),
		project:       cfg.Project,
		projectMap:    cfg.ProjectMap,
		onSpawn:       cfg.OnSpawn,
		logger:        logger,
		writePolicy:   cfg.WritePolicy,
		projectGate:   gate,
	}
}

// allowSpawn reports whether the bridge may SPAWN an agent for a mention on the
// project owning jiraKey. This is the spawn-eligibility gate, deliberately
// distinct from write-eligibility (AllowProject): the config-driven write policy
// admits a project on Writable alone, IGNORING its outbox SyncToggle, because the
// mention path performs no outbound Jira write (TestJiraMention_NoJiraWriteSurface)
// and so must not be coupled to JIRA_OUTBOX_ENABLED. GBFSM mentions therefore
// spawn in both toggle states; FSM and unknown projects stay denied
// (ka-0000001940). The legacy coordinated project gate is the fallback when the
// write policy is unwired (tests) — it has no GBFSM entry, so its verdict is
// unchanged.
func (h *JiraMentionHandler) allowSpawn(jiraKey string) bool {
	if h.writePolicy != nil {
		return h.writePolicy.AllowSpawn(jiraKey)
	}
	return h.projectGate.AllowProject(jiraKey)
}

// projectGateDiag returns a human-readable description of the spawn-verdict
// source for the "blocked mention" warning logs. For the write policy it lists
// the known projects (the spawnable set is those with Writable, i.e. all of
// them minus any disabled entry); for the legacy gate it reports the FSM toggle.
func (h *JiraMentionHandler) projectGateDiag() any {
	if h.writePolicy != nil {
		return h.writePolicy.Projects()
	}
	return h.projectGate.FSMSyncEnabled()
}

// Check examines a JIRA comment body (raw JSON, either a plain text string
// or an ADF object) for a mention trigger. Returns the detection result
// including the instruction text following the trigger.
//
// Detection runs on the plain-text flattening (robust, formatting-agnostic).
// When a trigger is found, the ADF body is separately re-rendered to markdown
// and the instruction re-extracted so the spawned agent receives the comment's
// code blocks, links, lists and emphasis rather than flattened text (H3, epic
// ka-0000007401). The markdown render is best-effort: any failure to locate the
// trigger in the markdown form leaves InstructionMarkdown empty and the caller
// falls back to the plain-text Instruction.
func (h *JiraMentionHandler) Check(body json.RawMessage) JiraMentionResult {
	// Structured detection first: when the body is ADF and carries a "mention"
	// node targeting one of the bot's accountIds, that IS the @-mention — take
	// everything after the node as the instruction. This is display-name- and
	// "@"-prefix-independent, closing the real prod GBFSM-85 miss where a native
	// @-mention flattened to "Gardenio Agent …" (no "@gardenio" token) and where a
	// multi-word bot display name would otherwise leak into the instruction
	// (epic ka-0000007401).
	if len(h.botAccountIDs) > 0 {
		if instruction, ok := extractMentionInstructionByAccountID(body, h.botAccountIDs); ok {
			return JiraMentionResult{
				Detected:            true,
				Instruction:         instruction,
				InstructionMarkdown: instruction,
				Trigger:             h.trigger,
			}
		}
	}

	text := jiraCommentBodyToText(body)
	result := h.CheckText(text)
	if result.Detected {
		if markdown := jiraCommentBodyToMarkdown(body); markdown != "" {
			result.InstructionMarkdown = extractJiraMentionInstruction(markdown, result.Trigger)
		}
	}
	return result
}

// CheckText examines a plain text comment for a mention trigger. It never sets
// InstructionMarkdown — that requires the structured ADF body, which only Check
// has; callers with plain text fall back to Instruction.
func (h *JiraMentionHandler) CheckText(text string) JiraMentionResult {
	instruction := extractJiraMentionInstruction(text, h.trigger)
	if instruction != "" {
		return JiraMentionResult{
			Detected:    true,
			Instruction: instruction,
			Trigger:     h.trigger,
		}
	}
	if h.botTrigger != "" && h.botTrigger != h.trigger {
		instruction = extractJiraMentionInstruction(text, h.botTrigger)
		if instruction != "" {
			return JiraMentionResult{
				Detected:    true,
				Instruction: instruction,
				Trigger:     h.botTrigger,
			}
		}
	}
	return JiraMentionResult{}
}

// MentionNewlyIntroduced reports whether an edit that changed a text field from
// oldText to newText NEWLY introduced a @gardenio mention: the trigger is present
// in the new text and absent from the old. It is the re-fire guard for the
// jira:issue_updated description path (H5, epic ka-0000007401) — a mention that
// was already present before the edit (or is absent from the new text) returns
// false, so re-editing an already-mentioned ticket never re-spawns. Detection
// uses the same case-insensitive plain-text matching as CheckText, checking both
// the primary and (when configured) secondary triggers.
func (h *JiraMentionHandler) MentionNewlyIntroduced(oldText, newText string) bool {
	return h.CheckText(newText).Detected && !h.CheckText(oldText).Detected
}

// HandleMention checks a JIRA comment for a @gardenio mention and, if found,
// creates a task bead and spawns an agent. Returns true if a mention was
// detected and handled (or blocked for safety). The caller should still
// bridge the comment to existing beads regardless.
func (h *JiraMentionHandler) HandleMention(ctx context.Context, event jiraWebhookEvent) bool {
	if event.Comment == nil || event.Issue == nil {
		return false
	}

	result := h.Check(event.Comment.Body)
	if !result.Detected {
		return false
	}

	jiraKey := event.Issue.Key
	projectKey := jiraProjectKey(jiraKey)

	// Safety: deny-by-default SPAWN gate. This is spawn-eligibility, NOT
	// write-eligibility: the config-driven write policy admits a project on
	// Writable alone and ignores its outbox SyncToggle (ka-0000001940), because
	// the mention path performs no outbound Jira write
	// (TestJiraMention_NoJiraWriteSurface) and so must not be coupled to
	// JIRA_OUTBOX_ENABLED. The verdict is still deny-by-default and
	// mode-independent (always hard-enforced): FSM and unknown projects stay
	// denied, so prod never spawns agents for projects the bot has no business
	// touching, while GBFSM mentions spawn regardless of the outbox flag. Returns
	// true (handled) so the caller does not treat a deliberately-skipped mention
	// as an error.
	if !h.allowSpawn(jiraKey) {
		h.logger.Warn("jira-mention: blocked mention on non-spawnable project",
			"jira_key", jiraKey, "project_key", projectKey,
			"spawnable_projects", h.projectGateDiag())
		return true
	}

	h.logger.Info("jira-mention: detected",
		"jira_key", jiraKey,
		"trigger", result.Trigger,
		"author", event.Comment.Author.DisplayName,
		"instruction", truncateStr(result.Instruction, 100))

	// Authorization: the author must be a mapped fleet human holding the
	// "mention" capability, as on the Slack and GitLab paths. Anyone who can
	// comment on a spawnable project otherwise spawned an agent operated by
	// JIRA_BRIDGE_DEFAULT_OPERATOR (gd-fBBMiDHb07). Checked before dedup so a
	// refused re-mention is not routed to the follow-up loop either. Returns
	// true (handled): the refusal is deliberate, not an error. The mention
	// path has no Jira write surface, so the refusal is only logged.
	author, reason := h.authorizeMentionAuthor(ctx, event.Comment.Author.AccountID, event.Comment.Author.EmailAddress)
	if author == nil {
		h.logger.Warn("jira-mention: rejecting mention — author is not a fleet user with the mention capability",
			"jira_key", jiraKey,
			"author", event.Comment.Author.DisplayName,
			"account_id", event.Comment.Author.AccountID,
			"reason", reason,
			"capability", MentionCapability)
		return true
	}

	// Dedup, windowed (ka-0000007091 — Jira twin of GitLab ka-0000001106). A
	// FRESH open mention task means this is a duplicate webhook delivery of a
	// mention we just acted on: suppress. Only-STALE open task(s) mean a
	// genuine re-mention arrived long after the first agent booted (and may
	// have since stopped/crashed): do NOT suppress and do NOT spawn a second
	// agent — return false so the webhook's own follow-up loop delivers this
	// comment to the assigned agent (nudge if live, respawn if dead;
	// deliverJiraFollowUp / F3). Returning true in the stale case is the
	// permanent-mute bug this task fixes: the webhook skips its follow-up when
	// mentionHandled==true, so a re-mention on a ticket with an orphaned open
	// task was swallowed by neither path.
	switch h.classifyPendingJiraMention(ctx, jiraKey) {
	case jiraMentionFreshDuplicate:
		h.logger.Info("jira-mention: skipping duplicate, fresh open task exists",
			"jira_key", jiraKey)
		return true
	case jiraMentionStaleReMention:
		h.logger.Info("jira-mention: stale open mention task on ticket, routing re-mention to follow-up loop instead of muting",
			"jira_key", jiraKey)
		return false
	case jiraMentionNoPending:
		// Fall through to spawn a first agent for this ticket.
	}

	if err := h.spawnJiraMentionAgent(ctx, event, author, result.Instruction, result.InstructionMarkdown); err != nil {
		h.logger.Error("jira-mention: failed to spawn agent",
			"jira_key", jiraKey, "error", err)
		return false
	}

	return true
}

// spawnJiraMentionAgent creates a task bead and spawns an agent for a JIRA
// mention. author is the authorized fleet user (authorizeMentionAuthor) the
// work is attributed to. instruction is the plain-text instruction (used for the bead title
// and dedup/logging); instructionMarkdown is the same instruction with the
// comment's formatting preserved as markdown, or "" when unavailable — the task
// description and agent prompt prefer it and fall back to instruction (H3, epic
// ka-0000007401).
func (h *JiraMentionHandler) spawnJiraMentionAgent(ctx context.Context, event jiraWebhookEvent, identity *UserIdentity, instruction, instructionMarkdown string) error {
	jiraKey := event.Issue.Key
	// Service-identity writes — see ka-0000005187; bridge runs under
	// fleet:jira-bridge. JIRA mention author is preserved in the task
	// description below.
	summary := event.Issue.Fields.Summary
	author := event.Comment.Author.DisplayName

	// Strip a --fresh flag from the instruction (ka-0000008188, Slack parity).
	// On this FIRST-spawn path the conversation is already empty, so --fresh is a
	// no-op for continuity — but the token must not leak into the task title or
	// the agent prompt. It only takes effect on the RESPAWN path (a follow-up
	// mention routed through the webhook follow-up loop → resolver), which reads
	// the flag off the comment body directly. Mirrors handleThreadSpawn, which
	// likewise only strips --fresh on first spawn.
	if fresh, stripped := parseFreshFlag(instruction); fresh {
		instruction = stripped
		if _, strippedMD := parseFreshFlag(instructionMarkdown); instructionMarkdown != "" {
			instructionMarkdown = strippedMD
		}
		h.logger.Info("jira-mention: stripped --fresh on first spawn (conversation already clean)",
			"jira_key", jiraKey)
	}

	// Mention-time --role intent hint (design-doc F-E, epic ka-0000007401):
	// "@gardenio --role reviewer <text>" lets an operator steer which agent role
	// this mention spawns, mirroring --fresh. parseRoleOverride only consumes a
	// LEADING token, so after the --fresh strip above a canonical front-loaded
	// flag run ("--role reviewer …" or "--fresh --role reviewer …") is honored
	// while a --role buried mid-prose is left as instruction text — identical to
	// the Slack spawn path. The flag is always stripped from both the plain and
	// markdown instruction so it never leaks into the task title or the agent
	// prompt, even when the requested role is later ignored as unconfigured.
	roleHint, roleStripped := parseRoleOverride(instruction)
	if roleHint != "" {
		instruction = roleStripped
		if _, strippedMD := parseRoleOverride(instructionMarkdown); instructionMarkdown != "" {
			instructionMarkdown = strippedMD
		}
		h.logger.Info("jira-mention: parsed --role hint on first spawn",
			"jira_key", jiraKey, "requested_role", roleHint)
	}

	// Mention-time --quiet intent hint (design-doc F-E, epic ka-0000007401):
	// "@gardenio --quiet <text>" asks for a low-noise ticket — the agent still
	// does the work and posts its explicit `gb jira reply` answer, but the three
	// AUTOMATIC progress-note forwarders (spawn-ack 👋, squawk mirrors, and the
	// terminal ✅/❌ completion note) skip the ticket. Like --fresh it is a bare
	// scan-every-word flag, stripped from both the plain and markdown instruction
	// so it never leaks into the task title or the agent prompt. A true value is
	// stamped as jira_quiet=true on the task + agent beads (below) and carried on
	// the spawn event, so all three forwarders gate on the same durable fact.
	quiet, quietStripped := parseQuietFlag(instruction)
	if quiet {
		instruction = quietStripped
		if _, strippedMD := parseQuietFlag(instructionMarkdown); instructionMarkdown != "" {
			instructionMarkdown = strippedMD
		}
		h.logger.Info("jira-mention: parsed --quiet hint on first spawn (suppressing automatic progress notes)",
			"jira_key", jiraKey)
	}

	// Prefer the markdown-rendered instruction (preserves the comment's code
	// blocks, links, lists and emphasis); fall back to the plain-text form when
	// the ADF->markdown render produced nothing (Jira Server, or no trigger in
	// the markdown). The title stays plain — it is a one-line summary and dedup
	// key, not a place for fenced code (H3, epic ka-0000007401).
	instructionBody := instructionMarkdown
	if instructionBody == "" {
		instructionBody = instruction
	}

	title := fmt.Sprintf("JIRA mention on %s: %s", jiraKey, truncateStr(instruction, 80))

	var desc strings.Builder
	fmt.Fprintf(&desc, "JIRA @gardenio mention by %s on [%s](https://acme.atlassian.net/browse/%s)\n\n",
		author, jiraKey, jiraKey)
	if summary != "" {
		fmt.Fprintf(&desc, "**Summary:** %s\n\n", summary)
	}
	fmt.Fprintf(&desc, "**Instruction:**\n%s\n", instructionBody)

	accountID := event.Comment.Author.AccountID

	// The authorized author is the operator (R3, ka-0000002887): slack:<id>,
	// else oidc:<email>, both human-shaped (authorizeMentionAuthor refuses an
	// identity with neither). Stamp it onto the context so the daemon
	// CreateBead/SpawnAgent calls carry X-Operated-By for this person
	// (push-at-create).
	operatedBy := jiraUserToOperator(identity)
	ctx = beadsapi.WithOperator(ctx, operatedBy)

	fields := map[string]string{
		"jira_key":       jiraKey,
		"mention_author": author,
		"spawn_source":   "jira-mention",
	}
	// Persist the originating mention comment id so replies can thread back
	// to it (F3, plan ka-0000007401 — same field the webhook follow-up path
	// stamps).
	if event.Comment.ID != "" {
		fields["jira_comment_id"] = event.Comment.ID
	}
	// Stamp the --quiet intent (design-doc F-E) so the record of the operator's
	// low-noise request lives on the task bead alongside the agent bead's copy.
	if quiet {
		fields[jiraQuietField] = "true"
	}
	// Push operator at create-time so the task bead carries operated_by
	// from the first row (R1 #8 push-at-create). The bridge writes under
	// its fleet token (audit attribution becomes fleet:jira-bridge —
	// ka-0000005187 / decision ka-0000001054), but the operated_by FIELD
	// records the authorized human so spawned agents inherit it via
	// BEADS_OPERATED_BY env injection.
	fields["operated_by"] = operatedBy
	if accountID != "" {
		fields["jira_user_id"] = accountID
	}
	// Mirror the resolved fleet identity so beads-viewer and downstream
	// writes can attribute the work to the human (slack_user_id) and link
	// back to their user bead (user_bead_id).
	if identity.SlackID != "" {
		fields["slack_user_id"] = identity.SlackID
	}
	if identity.BeadID != "" {
		fields["user_bead_id"] = identity.BeadID
	}

	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		return fmt.Errorf("marshalling fields: %w", err)
	}

	project := h.resolveProjectFromJiraKey(jiraKey)
	taskID, err := h.daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:       title,
		Type:        "task",
		Description: desc.String(),
		Labels:      []string{"project:" + project, "jira-mention"},
		Priority:    2,
		CreatedBy:   "jira-bridge",
		Fields:      json.RawMessage(fieldsJSON),
	})
	if err != nil {
		return fmt.Errorf("creating mention task bead: %w", err)
	}

	h.logger.Info("jira-mention: created task bead",
		"bead", taskID, "jira_key", jiraKey, "author", author)

	// Check for an existing agent already working on this ticket.
	existingAgent := h.findExistingJiraAgent(ctx, jiraKey)
	agentName := existingAgent
	if agentName == "" {
		agentName = jiraMentionAgentName(jiraKey)
	}

	// Hand the agent the ticket's recent conversation and the resolved human
	// operator so it replies in context and addresses the right person (F4,
	// plan ka-0000007401). The triggering comment is excluded — it is already
	// the Instruction.
	operatorDisplay := identity.DisplayName
	prompt := buildJiraMentionPrompt(jiraMentionPromptParams{
		JiraKey:         jiraKey,
		Summary:         summary,
		Author:          author,
		Instruction:     instructionBody,
		TaskID:          taskID,
		Operator:        operatedBy,
		OperatorDisplay: operatorDisplay,
		History:         fetchJiraCommentHistory(ctx, h.history, h.logger, jiraKey, event.Comment.ID),
	})

	extraFields := map[string]string{
		"jira_key":     jiraKey,
		"spawn_source": "jira-mention",
		"mention_task": taskID,
	}
	if event.Comment.ID != "" {
		extraFields["jira_comment_id"] = event.Comment.ID
	}
	// Carry the --quiet intent onto the agent bead so the SSE-driven forwarders
	// (JiraSquawkForwarder, JiraCompletionAcknowledger) — which resolve the
	// ticket binding solely from the closing/squawking agent's OWN bead fields —
	// can read jira_quiet and skip their automatic notes (design-doc F-E).
	if quiet {
		extraFields[jiraQuietField] = "true"
	}
	// Mirror operator + JIRA identity onto the agent bead so child writes
	// (the agent's own daemon calls) inherit the resolved human via
	// BEADS_OPERATED_BY env injection (R1 #8 push-at-create). Bridge runs
	// under fleet:jira-bridge for daemon auth (ka-0000005187), but the
	// agent's operated_by field records the originating human. Reuses the
	// operatedBy/identity resolved for the task bead above so the two stay
	// in lockstep.
	extraFields["operated_by"] = operatedBy
	if accountID != "" {
		extraFields["jira_user_id"] = accountID
	}
	if identity.SlackID != "" {
		extraFields["slack_user_id"] = identity.SlackID
	}
	if identity.BeadID != "" {
		extraFields["user_bead_id"] = identity.BeadID
	}

	spawnRole, err := resolveJiraSpawnRoleHinted(ctx, h.daemon, project, roleHint, h.logger)
	if err != nil {
		return fmt.Errorf("resolving spawn role for %q: %w", jiraKey, err)
	}
	agentBeadID, err := h.daemon.SpawnAgent(ctx, agentName, project, taskID, spawnRole, prompt, extraFields)
	if err != nil {
		recordJiraMentionSpawn(jiraSpawnError)
		return fmt.Errorf("spawning agent %q for mention: %w", agentName, err)
	}
	recordJiraMentionSpawn(jiraSpawnOK)

	h.logger.Info("jira-mention: spawned agent",
		"agent", agentName, "agent_bead", agentBeadID,
		"task", taskID, "jira_key", jiraKey, "project", project)

	// Stamp `triggered-by` lineage from the mention task bead to the
	// Jira-mirror bead (label `jira:<key>`). Best-effort: failure is logged
	// but does not fail the mention spawn — the agent has already been
	// dispatched and the mirror linkage is observability, not control flow.
	// Auto-link epic ka-0000000933 M2c; respects read-only mirror contract
	// advice ka-0000003234 (no outbound Jira API calls — gardenio-side deps
	// row only).
	h.writeMirrorTriggeredBy(ctx, taskID, jiraKey)

	// Notify listeners (Slack agent card, Jira spawn-ack, etc.) about the spawn.
	// Role + operator are carried so the Jira spawn-ack acknowledger can gate its
	// comment on the actual spawn role and stamp honest operator attribution
	// (JiraSpawnAcknowledger, plan ka-0000007401).
	if h.onSpawn != nil {
		h.onSpawn(ctx, JiraMentionSpawnEvent{
			AgentName:       agentName,
			AgentBeadID:     agentBeadID,
			TaskBeadID:      taskID,
			JiraKey:         jiraKey,
			Summary:         summary,
			Author:          author,
			Instruction:     instruction,
			Project:         project,
			Role:            spawnRole,
			Operator:        operatedBy,
			OperatorDisplay: operatorDisplay,
			Quiet:           quiet,
		})
	}

	return nil
}

// authorizeMentionAuthor decides whether a JIRA comment author may spawn an
// agent with @gardenio, and as whom. It mirrors the GitLab isAuthorized and
// Slack checkMentionPermission gates and fails closed (gd-fBBMiDHb07):
//
//   - No UserDirectory wired ⇒ refused ("no_user_directory").
//   - The author resolves to no user bead, by jira_user then email
//     (lookupJiraUserIdentity) ⇒ refused ("unmapped_author"). An unmapped
//     author is never attributed to a designated default human: that let any
//     Jira user act as JIRA_BRIDGE_DEFAULT_OPERATOR.
//   - The user bead lacks the "mention" capability ⇒ refused
//     ("missing_capability").
//   - The user bead has neither a Slack id nor an email, so there is no
//     human-shaped operator to attribute the work to ⇒ refused
//     ("no_human_operator").
//
// On success it returns the identity and backfills jira_user=<accountId> on a
// bead matched by email, so the next mention resolves via the jira_user index
// even if the webhook later omits emailAddress for a privacy-restricted
// profile. The backfill runs only for an authorized author, so a refused
// mention never writes to a user bead. reason is empty on success.
func (h *JiraMentionHandler) authorizeMentionAuthor(ctx context.Context, accountID, email string) (identity *UserIdentity, reason string) {
	if h.users == nil {
		return nil, "no_user_directory"
	}
	u := lookupJiraUserIdentity(ctx, h.users, accountID, email)
	if u == nil {
		return nil, "unmapped_author"
	}
	if !u.HasCapability(MentionCapability) {
		return nil, "missing_capability"
	}
	if jiraUserToOperator(u) == "" {
		return nil, "no_human_operator"
	}
	if accountID != "" && u.JiraUser != accountID {
		h.backfillJiraUser(u.BeadID, accountID)
	}
	return u, ""
}

// lookupJiraUserIdentity resolves a Jira user (comment author or ticket
// reporter) to a fleet user bead, trying the jira_user index first (Server
// usernames / backfilled accountIds) then the email index (the reliable Jira
// Cloud key). Returns nil when neither matches or no directory is wired.
//
// Shared by the @-mention handler (authorizeMentionAuthor) and the JQL auto-spawn
// path (newJiraReporterOperatorResolver) so both attribute Jira-triggered work
// to the same fleet human — closing the "auto-spawn stamped fleet:jira-bridge
// instead of the reporter" gap (ka-0000001781).
func lookupJiraUserIdentity(ctx context.Context, users JiraUserLookup, accountID, email string) *UserIdentity {
	if users == nil {
		return nil
	}
	if accountID != "" {
		if u := users.LookupByJiraUser(ctx, accountID); u != nil {
			return u
		}
	}
	if email != "" {
		if u := users.LookupByEmail(ctx, email); u != nil {
			return u
		}
	}
	return nil
}

// jiraUserToOperator maps a resolved fleet UserIdentity to the human-operator
// id the kbeads daemon accepts for an audited write: slack:<id> is preferred
// (the primary fleet identity), then oidc:<email>. Returns "" when the identity
// is nil or carries neither — the caller then falls back to its own default /
// sentinel. The slack-then-oidc preference matches operatorFromGitLabUser.
func jiraUserToOperator(u *UserIdentity) string {
	if u == nil {
		return ""
	}
	if u.SlackID != "" {
		return "slack:" + u.SlackID
	}
	if u.Email != "" {
		return "oidc:" + u.Email
	}
	return ""
}

// backfillJiraUser best-effort stamps jira_user=<accountId> on a user bead so
// future Jira mentions resolve via the jira_user index without depending on
// the webhook exposing emailAddress. No-op when the handler has no backfill-capable daemon
// or beadID is empty; failures are logged, never surfaced.
func (h *JiraMentionHandler) backfillJiraUser(beadID, accountID string) {
	if beadID == "" || accountID == "" {
		return
	}
	bf, ok := h.daemon.(jiraUserBackfiller)
	if !ok {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := bf.UpdateBeadFields(ctx, beadID, map[string]string{"jira_user": accountID}); err != nil {
			h.logger.Warn("jira-mention: failed to backfill jira_user on user bead",
				"bead", beadID, "account_id", accountID, "error", err)
			return
		}
		h.logger.Info("jira-mention: backfilled jira_user on user bead",
			"bead", beadID, "account_id", accountID)
	}()
}

// writeMirrorTriggeredBy looks up the Jira-mirror bead for jiraKey (label
// `jira:<key>`) and writes a `triggered-by` deps row from the spawned mention
// task to the mirror. All operations are gardenio-side reads/writes against
// the beads daemon — no outbound Jira API calls (advice ka-0000003234).
//
// Best-effort: lookup failures, no-match, and write failures are logged but
// not surfaced to the caller. The mention spawn has already succeeded; the
// lineage edge is observability/traversal substrate (auto-link epic
// ka-0000000933 M2c ka-0000003814).
func (h *JiraMentionHandler) writeMirrorTriggeredBy(ctx context.Context, taskID, jiraKey string) {
	if taskID == "" || jiraKey == "" {
		return
	}
	result, err := h.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Labels: []string{"jira:" + jiraKey},
		Limit:  10,
	})
	if err != nil {
		h.logger.Warn("jira-mention: mirror lookup failed for triggered-by edge",
			"jira_key", jiraKey, "task", taskID, "error", err)
		return
	}
	if result == nil || len(result.Beads) == 0 {
		h.logger.Debug("jira-mention: no mirror bead found for triggered-by edge",
			"jira_key", jiraKey, "task", taskID)
		return
	}
	for _, mirror := range result.Beads {
		if mirror == nil || mirror.ID == "" || mirror.ID == taskID {
			continue
		}
		if err := h.daemon.AddDependency(ctx, taskID, mirror.ID, "triggered-by", "jira-bridge"); err != nil {
			h.logger.Warn("jira-mention: failed to add triggered-by dep to mirror",
				"task", taskID, "mirror", mirror.ID, "jira_key", jiraKey, "error", err)
			continue
		}
		h.logger.Info("jira-mention: stamped triggered-by lineage",
			"task", taskID, "mirror", mirror.ID, "jira_key", jiraKey)
	}
}

// jiraMentionDedupWindow bounds how long an open jira-mention task suppresses a
// re-mention on the same ticket. The dedup gate exists only to collapse a burst
// of duplicate webhook deliveries for a single comment while the spawned agent
// boots — NOT to mute a ticket for the lifetime of an open task.
//
// An open task is NOT proof of a live agent: the spawned agent can crash/die
// without closing its task (assignee cleared, status stays open), and before
// this window was introduced (ka-0000007091, the Jira twin of GitLab
// ka-0000001106) such an orphaned task muted every subsequent @gardenio on the
// ticket FOREVER — HandleMention returned true from its dedup branch, so the
// webhook skipped its follow-up loop too. Bounding suppression to a short
// window means a genuine re-mention minutes later is classified stale and
// routed to the webhook follow-up loop (nudge live / respawn dead) instead of
// being dropped, while true webhook-retry bursts (delivered within seconds)
// are still collapsed.
const jiraMentionDedupWindow = 3 * time.Minute

// jiraMentionPendingClass is the verdict of classifyPendingJiraMention.
type jiraMentionPendingClass int

const (
	// jiraMentionNoPending: no open jira-mention task on the ticket. Spawn a
	// first agent.
	jiraMentionNoPending jiraMentionPendingClass = iota
	// jiraMentionFreshDuplicate: a recently-created open mention task exists —
	// this is a duplicate webhook delivery of a comment we just acted on.
	// Suppress (dedup).
	jiraMentionFreshDuplicate
	// jiraMentionStaleReMention: the ticket has open mention task(s) but every
	// one is older than jiraMentionDedupWindow — a genuine re-mention long
	// after the first agent booted. Do NOT suppress and do NOT spawn a second
	// agent; the caller returns false so the webhook follow-up loop delivers
	// the comment to the assigned agent (nudge/respawn).
	jiraMentionStaleReMention
)

// classifyPendingJiraMention decides how HandleMention should treat a mention
// given the ticket's existing open jira-mention task(s). It scans ALL open
// mention tasks before deciding so the outcome is independent of iteration
// order: a ticket carrying both a fresh task (a genuine burst) and a stale
// orphan must still dedup against the fresh one. Suppress only if some open
// task is fresh; if the ticket has open mention tasks but every one is stale,
// return jiraMentionStaleReMention; otherwise jiraMentionNoPending.
func (h *JiraMentionHandler) classifyPendingJiraMention(ctx context.Context, jiraKey string) jiraMentionPendingClass {
	beads, err := h.daemon.ListTaskBeadsForJiraKey(ctx, jiraKey)
	if err != nil {
		h.logger.Warn("jira-mention: failed to list beads for dedup",
			"jira_key", jiraKey, "error", err)
		// Fail safe as "no pending" so a transient list error never wedges a
		// legitimate first spawn; the recentSpawn cooldown on the resolver and
		// the webhook's per-bead nudge throttle still bound duplicate work.
		return jiraMentionNoPending
	}
	var staleOrphan *beadsapi.BeadDetail
	for _, b := range beads {
		if b.Status == "closed" {
			continue
		}
		if b.Fields["jira_key"] != jiraKey ||
			b.Fields["spawn_source"] != "jira-mention" {
			continue
		}
		// Fresh open task ⇒ collapse this as a duplicate webhook delivery. A
		// zero CreatedAt (unknown age) fails safe as "fresh" so we do not
		// spam-spawn when the timestamp is simply missing.
		if b.CreatedAt.IsZero() || time.Since(b.CreatedAt) < jiraMentionDedupWindow {
			return jiraMentionFreshDuplicate
		}
		staleOrphan = b
	}
	if staleOrphan == nil {
		return jiraMentionNoPending
	}
	// Only stale open mention task(s) on the ticket ⇒ a genuine re-mention. Do
	// NOT suppress (that is the permanent-mute bug ka-0000007091) and do NOT
	// spawn a second agent (the ticket already has an agent binding the webhook
	// follow-up loop can nudge/respawn). Surface the previously-silent
	// pass-through so it is observable (agents can't read pod logs).
	recordJiraMentionStaleTaskPassthrough()
	h.logger.Info("jira-mention: stale open mention task on ticket, not suppressing re-mention",
		"jira_key", jiraKey, "stale_task", staleOrphan.ID,
		"stale_assignee", staleOrphan.Assignee,
		"task_age", time.Since(staleOrphan.CreatedAt).Round(time.Second).String())
	return jiraMentionStaleReMention
}

// findExistingJiraAgent looks for a task bead linked to the given JIRA key
// that has an assigned agent. Returns the agent name if found.
func (h *JiraMentionHandler) findExistingJiraAgent(ctx context.Context, jiraKey string) string {
	beads, err := h.daemon.ListTaskBeadsForJiraKey(ctx, jiraKey)
	if err != nil {
		h.logger.Warn("jira-mention: failed to list beads for agent reuse",
			"jira_key", jiraKey, "error", err)
		return ""
	}
	for _, b := range beads {
		if b.Fields["jira_key"] == jiraKey && b.Assignee != "" {
			return b.Assignee
		}
	}
	return ""
}

// resolveProjectFromJiraKey returns the boat project for agents spawned on
// the given ticket: the projectMap entry for the ticket's JIRA project key
// when one is configured (multi-project routing, G3 epic ka-0000007401),
// else the configured default project.
func (h *JiraMentionHandler) resolveProjectFromJiraKey(jiraKey string) string {
	if boatProject, ok := h.projectMap[strings.ToUpper(jiraProjectKey(jiraKey))]; ok {
		return boatProject
	}
	return h.project
}

// jiraProjectKey extracts the project key from a JIRA issue key (e.g., "PE" from "PE-1234").
func jiraProjectKey(issueKey string) string {
	if idx := strings.Index(issueKey, "-"); idx > 0 {
		return issueKey[:idx]
	}
	return issueKey
}

// jiraMentionAgentName generates a unique agent name for a JIRA mention.
func jiraMentionAgentName(jiraKey string) string {
	return fmt.Sprintf("jira-%s-%s", strings.ToLower(jiraKey), randomSuffix(4))
}

// jiraQuietField is the durable bead field that records a mention-time --quiet
// intent hint (design-doc F-E, epic ka-0000007401). It is stamped "true" on both
// the mention task bead and the spawned agent bead when the operator prefixed a
// mention with --quiet, and read back by the SSE-driven progress-note forwarders
// (squawk, completion) — which resolve the ticket solely from the acting agent's
// own bead — so they suppress their automatic notes on a low-noise ticket.
const jiraQuietField = "jira_quiet"

// jiraQuietSuppressed reports whether an agent bead's fields carry the --quiet
// intent, so the automatic progress-note forwarders skip the ticket. The spawn
// -ack path checks the spawn event's Quiet bool directly instead — it runs
// before the agent bead is readable.
func jiraQuietSuppressed(fields map[string]string) bool {
	return fields[jiraQuietField] == "true"
}

// jiraHistoryMaxComments bounds how many recent ticket comments are fetched
// for a spawn/nudge prompt (F4, plan ka-0000007401).
const jiraHistoryMaxComments = 10

// jiraHistoryMaxCommentLen bounds each comment's text in the prompt so one
// giant comment cannot crowd out the instruction.
const jiraHistoryMaxCommentLen = 500

// fetchJiraCommentHistory returns the ticket's recent comments formatted for
// prompt inclusion, or "" when no history client is wired, the fetch fails, or
// there are no comments. Best-effort: a fetch failure is logged and degrades
// to a history-less prompt — it never blocks the spawn/nudge.
func fetchJiraCommentHistory(ctx context.Context, client JiraCommentHistoryClient, logger *slog.Logger, jiraKey, excludeCommentID string) string {
	if client == nil {
		return ""
	}
	if logger == nil {
		logger = slog.Default()
	}
	comments, err := client.GetComments(ctx, jiraKey, jiraHistoryMaxComments)
	if err != nil {
		logger.Warn("jira: failed to fetch comment history for prompt",
			"jira_key", jiraKey, "error", err)
		return ""
	}
	return formatJiraCommentHistory(comments, excludeCommentID)
}

// formatJiraCommentHistory renders comments (API order: newest first) as an
// oldest-first transcript, one line per comment. The triggering comment
// (excludeCommentID) is skipped — it already appears verbatim in the prompt as
// the instruction / follow-up body.
func formatJiraCommentHistory(comments []JiraCommentEntry, excludeCommentID string) string {
	var sb strings.Builder
	for _, c := range slices.Backward(comments) {
		if excludeCommentID != "" && c.ID == excludeCommentID {
			continue
		}
		text := strings.TrimSpace(jiraCommentBodyToText(c.Body))
		if text == "" {
			continue
		}
		author := c.Author.DisplayName
		if author == "" {
			author = "unknown"
		}
		fmt.Fprintf(&sb, "- %s", author)
		if c.Created != "" {
			fmt.Fprintf(&sb, " (%s)", c.Created)
		}
		fmt.Fprintf(&sb, ": %s\n", truncateStr(text, jiraHistoryMaxCommentLen))
	}
	return strings.TrimSpace(sb.String())
}

// jiraOperatorPromptLine renders the resolved operator identity for a
// spawn/nudge prompt so the agent knows which human it acts for and addresses
// replies to them (F4, plan ka-0000007401). Only a human-shaped operator
// (slack:U… / oidc:…@…) earns the line — sentinels would misdirect the agent.
func jiraOperatorPromptLine(operator, display string) string {
	operator = strings.TrimSpace(operator)
	if !IsHumanOperator(operator) {
		return ""
	}
	display = strings.TrimSpace(display)
	who := operator
	if display != "" && display != operator {
		who = display + " (" + operator + ")"
	}
	return "Operator: " + who + " — the human you are acting for; address them in replies."
}

// jiraMentionPromptParams carries the context rendered into a mention-spawned
// agent's prompt. Operator/OperatorDisplay and History are the F4 additions
// (plan ka-0000007401): the resolved human identity and the ticket's recent
// conversation.
type jiraMentionPromptParams struct {
	JiraKey     string
	Summary     string
	Author      string
	Instruction string
	TaskID      string
	// Operator is the resolved operator id (slack:U… / oidc:…@… when a human
	// resolved; jira:/fleet: sentinels render no operator line).
	Operator string
	// OperatorDisplay is the resolved fleet user's display name, when known.
	OperatorDisplay string
	// History is the pre-formatted recent comment transcript ("" ⇒ omitted).
	History string
}

// buildJiraMentionPrompt creates the custom prompt for a JIRA mention-spawned agent.
func buildJiraMentionPrompt(p jiraMentionPromptParams) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "You were spawned by a @gardenio mention on JIRA ticket %s.\n", p.JiraKey)
	if p.Summary != "" {
		fmt.Fprintf(&sb, "Summary: %s\n", p.Summary)
	}
	fmt.Fprintf(&sb, "Author: %s\n", p.Author)
	if line := jiraOperatorPromptLine(p.Operator, p.OperatorDisplay); line != "" {
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	sb.WriteString("\n")
	if p.History != "" {
		fmt.Fprintf(&sb, "Recent comments on %s (oldest first):\n%s\n\n", p.JiraKey, p.History)
	}
	fmt.Fprintf(&sb, "Instruction: %s\n\n", p.Instruction)
	fmt.Fprintf(&sb, "Your task bead: %s — use `kd show %s` for full context.\n", p.TaskID, p.TaskID)
	fmt.Fprintf(&sb, "Reply on the ticket with `gb jira reply '<text>'` (posts an internal note by default; --public is role-gated).\n")
	fmt.Fprintf(&sb, "Re-read the ticket conversation mid-session with `gb jira thread` — the history above is a snapshot and goes stale.\n")
	fmt.Fprintf(&sb, "Re-read the current ticket body/status/fields with `gb jira show %s` — the summary above is a snapshot and may have been edited since.\n", p.JiraKey)
	fmt.Fprintf(&sb, "When done, `kd close %s` and `gb done`.\n", p.TaskID)
	return sb.String()
}

// extractJiraMentionInstruction extracts the instruction text after a mention
// trigger in a JIRA comment. Returns empty string if no mention is found.
// The match is case-insensitive.
func extractJiraMentionInstruction(text, trigger string) string {
	// asciiToLower (not strings.ToLower) so the byte offset from the folded copy
	// indexes safely into the original text: strings.ToLower is not
	// length-preserving for some runes (e.g. U+023A Ⱥ → 3-byte ⱥ), which made
	// text[idx+len(trigger):] panic on attacker-controlled comment text
	// containing an expanding rune before the trigger (ka-0000002216). The
	// trigger is always ASCII, so ASCII-only folding is sufficient.
	lower := asciiToLower(text)
	idx := strings.Index(lower, trigger)
	if idx < 0 {
		return ""
	}
	after := strings.TrimSpace(text[idx+len(trigger):])
	if after == "" {
		return ""
	}
	return after
}

// jiraCommentBodyToText converts a JIRA comment body (json.RawMessage) to
// plain text. Handles both:
//   - Plain text string: JSON string like "some comment text"
//   - ADF object: Atlassian Document Format JSON object from Jira Cloud
//
// Returns the extracted plain text, or an empty string on parse failure.
func jiraCommentBodyToText(body json.RawMessage) string {
	if len(body) == 0 {
		return ""
	}

	// Try to unmarshal as a JSON string first (Jira Server plain text).
	var s string
	if err := json.Unmarshal(body, &s); err == nil {
		return s
	}

	// Try to parse as ADF (Jira Cloud).
	return adfToPlainText(body)
}

// jiraCommentBodyToMarkdown converts a JIRA comment body (json.RawMessage) to
// markdown, preserving the formatting adfToPlainText discards — fenced code,
// inline code, links, lists, headings and bold/italic (H3, epic
// ka-0000007401). A plain-text body (Jira Server) carries no formatting, so the
// raw string is returned unchanged; an ADF body is rendered via adfToMarkdown.
// Returns "" on parse failure or empty input so callers can fall back to the
// plain-text form.
func jiraCommentBodyToMarkdown(body json.RawMessage) string {
	if len(body) == 0 {
		return ""
	}

	// Jira Server plain text: a bare JSON string has no formatting to preserve.
	var s string
	if err := json.Unmarshal(body, &s); err == nil {
		return s
	}

	// Jira Cloud ADF.
	return adfToMarkdown(body)
}

// adfToPlainText converts an Atlassian Document Format JSON blob to plain text.
// Unlike adfToMarkdown, this strips all formatting and returns only the raw text
// content, which is suitable for mention trigger detection.
func adfToPlainText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var doc struct {
		Type    string    `json:"type"`
		Content []adfNode `json:"content"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ""
	}

	var sb strings.Builder
	collectADFText(&sb, doc.Content)
	return strings.TrimSpace(sb.String())
}

// collectADFText recursively collects all text content from ADF nodes.
func collectADFText(sb *strings.Builder, nodes []adfNode) {
	for _, node := range nodes {
		switch node.Type {
		case "text":
			sb.WriteString(node.Text)
		case "mention":
			// A mention node is ALWAYS an @-mention, but Jira does not
			// reliably include the leading "@" in the display text: the ADF
			// attrs.text for a native user @-mention is the account display
			// name and, depending on the payload (webhook vs REST, instance
			// config), can arrive WITHOUT the "@" — e.g. "Gardenio Agent"
			// instead of "@gardenio" (real prod GBFSM-85 case, epic
			// ka-0000007401). Trigger detection then fails to find "@gardenio"
			// in the flattened text and the mention silently never spawns.
			// Normalize to a guaranteed single leading "@" so a native
			// @-mention of the bot user is detectable the same as literal
			// "@gardenio" text.
			sb.WriteString(mentionNodeText(node))
		case "hardBreak":
			sb.WriteString("\n")
		default:
			// Recurse into block-level nodes (paragraph, heading, etc.).
			if len(node.Content) > 0 {
				collectADFText(sb, node.Content)
				// Add spacing between block-level elements.
				switch node.Type {
				case "paragraph", "heading", "codeBlock", "blockquote",
					"bulletList", "orderedList", "listItem":
					sb.WriteString(" ")
				}
			}
		}
	}
}

// normalizeAccountIDs trims and lowercases account ids, dropping blanks, for
// case-insensitive structured mention matching.
func normalizeAccountIDs(ids []string) []string {
	var out []string
	for _, id := range ids {
		if t := strings.ToLower(strings.TrimSpace(id)); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// extractMentionInstructionByAccountID walks an ADF comment body for a "mention"
// node whose attrs.id matches one of accountIDs (the bot's Jira account). On a
// match it returns the plain-text instruction that follows the mention node —
// everything after it in the document, flattened and trimmed — and ok=true.
// This is the structured, display-name-independent detection path: a mention
// node unambiguously IS an @-mention, so it does not depend on Jira prefixing
// the display text with "@" (the prod GBFSM-85 miss, epic ka-0000007401). A
// non-ADF body (Jira Server plain text), a parse failure, or no matching
// mention node yields ("", false) so the caller falls back to text-trigger
// matching. A bare mention with no trailing text yields ("", false), matching
// the text path's "trigger with no instruction does not spawn" contract.
func extractMentionInstructionByAccountID(body json.RawMessage, accountIDs []string) (string, bool) {
	if len(body) == 0 || len(accountIDs) == 0 {
		return "", false
	}
	var doc struct {
		Content []adfNode `json:"content"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", false
	}
	var after strings.Builder
	found := false
	collectADFTextAfterMention(&after, doc.Content, accountIDs, &found)
	if !found {
		return "", false
	}
	instruction := strings.TrimSpace(after.String())
	if instruction == "" {
		return "", false
	}
	return instruction, true
}

// collectADFTextAfterMention walks ADF nodes in order. Once it encounters a
// "mention" node whose attrs.id is in accountIDs it sets *found and, from that
// point on, appends all subsequent text into sb. It recurses depth-first so
// text following the mention in later blocks (paragraphs, list items) is
// captured too.
func collectADFTextAfterMention(sb *strings.Builder, nodes []adfNode, accountIDs []string, found *bool) {
	for _, node := range nodes {
		if !*found && node.Type == "mention" && mentionNodeMatchesAccount(node, accountIDs) {
			*found = true
			continue
		}
		if *found {
			switch node.Type {
			case "text":
				sb.WriteString(node.Text)
			case "mention":
				sb.WriteString(mentionNodeText(node))
			case "hardBreak":
				sb.WriteString("\n")
			default:
				if len(node.Content) > 0 {
					collectADFTextAfterMention(sb, node.Content, accountIDs, found)
					switch node.Type {
					case "paragraph", "heading", "codeBlock", "blockquote",
						"bulletList", "orderedList", "listItem":
						sb.WriteString(" ")
					}
				}
			}
			continue
		}
		// Not yet found: recurse to locate the mention node.
		if len(node.Content) > 0 {
			collectADFTextAfterMention(sb, node.Content, accountIDs, found)
		}
	}
}

// mentionNodeMatchesAccount reports whether an ADF mention node's attrs.id
// matches one of accountIDs (case-insensitive).
func mentionNodeMatchesAccount(node adfNode, accountIDs []string) bool {
	if node.Type != "mention" || len(node.Attrs) == 0 {
		return false
	}
	var attrs struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(node.Attrs, &attrs) != nil {
		return false
	}
	id := strings.ToLower(strings.TrimSpace(attrs.ID))
	if id == "" {
		return false
	}
	return slices.Contains(accountIDs, id)
}

// mentionNodeText renders an ADF "mention" node's display text with a
// guaranteed single leading "@". A mention node is unambiguously an @-mention,
// but Jira does not consistently prefix the display text with "@": for a native
// user @-mention the attrs.text (or the legacy node text) is the account's
// display name and may arrive as "Gardenio Agent" rather than "@gardenio"
// (confirmed on the real prod GBFSM-85 payload, epic ka-0000007401). Emitting it
// verbatim left the trigger token absent from the flattened text, so a native
// @-mention of the bot user never matched and never spawned. Normalizing to a
// single leading "@" makes such a mention detectable identically to literal
// "@gardenio" text, while an already-"@"-prefixed value is left unchanged (no
// double "@"). An empty mention (no text and no attrs.text) yields "".
func mentionNodeText(node adfNode) string {
	text := node.Text
	if text == "" && len(node.Attrs) > 0 {
		var attrs struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(node.Attrs, &attrs) == nil {
			text = attrs.Text
		}
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if !strings.HasPrefix(text, "@") {
		text = "@" + text
	}
	return text
}
