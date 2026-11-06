package main

// gb spawn — Spawn a new agent with auto-generated name.
// gb start — Start a named agent (explicit name required).
//
// Usage:
//
//	gb spawn [flags]                            (auto-name, project from env)
//	gb spawn --task <bead-id>                   (auto-name from task title)
//	gb start <name> <project> [flags]           (explicit name)
//	gb start <project> --task <bead-id>         (task-first: auto-generate name)
//
// Flags:
//
//	--role <role>       Agent role (default: crew)
//	--task <bead-id>    Pre-assign a task bead
//	--prompt <text>     Custom prompt injected at startup
//	--confirm           Spawn even with no intent flags (a bare idle agent)

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"

	"gardenio/clicreds/credstore"

	"github.com/spf13/cobra"
)

var spawnCmd = &cobra.Command{
	Use:   "spawn [flags]",
	Short: "Spawn a new agent (auto-named)",
	Long: `Spawn a new agent with an auto-generated name. The project is taken from
BOAT_PROJECT or inferred from the task's labels.

  gb spawn                                    # auto-name, project from env
  gb spawn --task ka-abc123                   # name from task title
  gb spawn --task ka-abc123 --role engineer   # with role
  gb spawn --prompt 'Review PR #42'           # with custom prompt

Use 'gb start' if you need to specify an explicit agent name.

SCRIPTING (confirming a spawn from a gardener loop):
A successful spawn prints a stable first line and exits 0; a failure prints no
such line and exits non-zero. Grep the token, not the prose:

  gb spawn --task ka-abc123 --role crew | grep '^SPAWNED'
  SPAWNED agent=<name> bead=<id> status=created task=ka-abc123

  gb spawn --task ka-abc123 --role crew --jq '.status'   # created | existing

status/'.status' distinguishes a REAL dispatch from an idempotent no-op:
  created   a new agent was created by this call.
  existing  no agent was created — a live one already existed and was returned
            (reason=name_dedup: that name is taken; reason=single_claim: another
            agent already holds this task). agent=/.name is that INCUMBENT.

Count only status=created as a new dispatch; treat existing as "already covered".`,
	GroupID: "agent",
	Args:    spawnNoPositionalArgs,
	RunE:    runSpawn,
}

// maxEchoedStrayArg caps how much of an unexpected positional arg the argv-guard
// echoes back, so a full mis-quoted prompt body (which can be kilobytes) does
// not flood the terminal.
const maxEchoedStrayArg = 80

// spawnNoPositionalArgs is a cobra Args validator for `gb spawn`, which takes no
// positional arguments. Cobra's built-in cobra.NoArgs rejects a stray positional
// with a generic `unknown command %q for %q`, which misdirects the caller toward
// a bad-subcommand theory. In practice the stray arg is almost always a
// mis-quoted `--prompt` body: a single-quoted `--prompt '...'` closes early on a
// nested `'`, or an unclosed backtick swallows the flag, so the prompt text lands
// as a positional. This validator emits a targeted hint pointing at shell quoting
// with the heredoc recipe instead (bug ka-0000002137; advice ka-0000006477).
func spawnNoPositionalArgs(_ *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	stray := args[0]
	if len(stray) > maxEchoedStrayArg {
		stray = stray[:maxEchoedStrayArg] + "…"
	}
	return fmt.Errorf(`gb spawn takes no positional arguments; got %d unexpected arg (%q).
Did you mean --prompt? A prompt containing backticks or a nested quoted 'gb stop --wrapup {...}'
can break shell quoting so its body becomes a stray arg. Build it with a quoted heredoc:
  prompt=$(cat <<'HEREDOC'
  ...
  HEREDOC
  )
  gb spawn ... --prompt "$prompt"`, len(args), stray)
}

var startCmd = &cobra.Command{
	Use:   "start <name> <project> [flags]",
	Short: "Start a named agent",
	Long: `Start a new agent with an explicit name. This is the power-user variant
of 'gb spawn' for when you need control over the agent name.

  gb start fix-auth monorepo --role engineer --task ka-abc123
  gb start review-pr gardenio --prompt 'Review PR #42 for security issues'

Task-first mode (auto-generate agent name from task title):
  gb start gardenio --task ka-abc123

When --task is given (a gd- bead ID or a ticket reference like PE-1234), the
project is inferred from the task bead's project: label if not specified.`,
	GroupID: "agent",
	Args:    cobra.RangeArgs(1, 2),
	RunE:    runStart,
}

// registerSpawnFlags registers the flags shared by `gb spawn`. Kept as a
// standalone helper so tests can build a fresh command with an identical flag
// set (no global cobra state leaking between runs) without drifting from the
// real registration.
func registerSpawnFlags(cmd *cobra.Command) {
	cmd.Flags().String("role", "crew", "Agent role(s), comma-separated (e.g. crew, thread,crew)")
	cmd.Flags().String("task", "", "Pre-assign a task bead (bead ID or ticket reference)")
	cmd.Flags().String("prompt", "", "Custom prompt injected at agent startup")
	cmd.Flags().String("slack-thread-channel", "", "Override slack_thread_channel inherited from task")
	cmd.Flags().String("slack-thread-ts", "", "Override slack_thread_ts inherited from task")
	cmd.Flags().String("slack-spawn-channel", "", "Pin the spawned agent's reporting channel (ID or name); wins over gardener→crew inheritance")
	cmd.Flags().Bool("confirm", false, "Spawn a bare idle agent even when no --task/--prompt/--role/--slack-thread-* flag is given")
	cmd.Flags().Bool("escalate", false, "If the role is privileged and the spawn is denied, file an escalation instead of failing (crew/non-human callers)")
	cmd.Flags().String("harness", "", "Agent CLI for this one spawn: claude, codex or gemini. Overrides the role/project/default harness (agent > role > project > default)")
	cmd.Flags().String("owner-approved", "", "Lands: the owner's decision that lets a lands-gardener step spawn past its cost estimate")
}

func init() {
	registerSpawnFlags(spawnCmd)

	startCmd.Flags().String("role", "crew", "Agent role(s), comma-separated (e.g. crew, thread,crew)")
	startCmd.Flags().String("task", "", "Pre-assign a task bead (bead ID or ticket reference)")
	startCmd.Flags().String("prompt", "", "Custom prompt injected at agent startup")
	startCmd.Flags().String("slack-thread-channel", "", "Override slack_thread_channel inherited from task")
	startCmd.Flags().String("slack-thread-ts", "", "Override slack_thread_ts inherited from task")
	startCmd.Flags().String("slack-spawn-channel", "", "Pin the spawned agent's reporting channel (ID or name); wins over gardener→crew inheritance")
	startCmd.Flags().Bool("escalate", false, "If the role is privileged and the spawn is denied, file an escalation instead of failing (crew/non-human callers)")
	startCmd.Flags().String("harness", "", "Agent CLI for this one spawn: claude, codex or gemini. Overrides the role/project/default harness (agent > role > project > default)")
	startCmd.Flags().String("owner-approved", "", "Lands: the owner's decision that lets a lands-gardener step spawn past its cost estimate")
}

// spawnIntentFlags are the flags whose presence signals the operator actually
// wants to spawn a *specific* agent, not the fully-defaulted bare one. An
// explicit --role (even --role crew) counts as intent, so cmd.Flags().Changed
// is used rather than comparing against the default value.
var spawnIntentFlags = []string{"task", "prompt", "role", "slack-thread-channel", "slack-thread-ts", "slack-spawn-channel", "escalate"}

// spawnHasIntent reports whether any intent-signalling flag was set on the
// command line. A bare `gb spawn` (zero flags) has no intent and would spawn an
// unnamed idle crew agent — the accidental-mutation this guard blocks
// (ka-0000003691 / bug ka-0000003560).
func spawnHasIntent(cmd *cobra.Command) bool {
	return slices.ContainsFunc(spawnIntentFlags, cmd.Flags().Changed)
}

// runSpawn handles 'gb spawn' — auto-generates agent name, project from env/task.
func runSpawn(cmd *cobra.Command, _ []string) error {
	// Guard against a bare `gb spawn` (no flags at all). Unlike every other
	// mutating verb, spawn defaults every input, so zero args = a fully-armed
	// mutation that silently creates a task-less, auto-named idle crew agent
	// (observed live in real session ka-0000003805). Require at least one intent
	// flag, or an explicit --confirm escape hatch. A flagged invocation
	// (`gb spawn --task ...`, `--prompt ...`, `--role ...`) is unaffected, so
	// gardener-driven non-interactive spawns keep working.
	confirm, _ := cmd.Flags().GetBool("confirm")
	if !confirm && !spawnHasIntent(cmd) {
		_ = cmd.Help()
		return fmt.Errorf("\ngb spawn with no flags would start an unnamed idle agent; " +
			"pass --task/--prompt/--role, or --confirm to spawn anyway")
	}

	role, _ := cmd.Flags().GetString("role")
	taskID, _ := cmd.Flags().GetString("task")
	customPrompt, _ := cmd.Flags().GetString("prompt")
	escalate, _ := cmd.Flags().GetBool("escalate")

	ctx := cmd.Context()

	// Resolve the task bead (following ticket references) and infer its
	// project: label. The bead's title seeds the agent name below.
	resolvedTaskID, taskProject, taskBead, err := resolveTaskProject(ctx, taskID)
	if err != nil {
		return err
	}
	taskID = resolvedTaskID

	// Resolve project first so it can be included in the agent name.
	project := taskProject
	if project == "" {
		project = defaultProject()
	}
	if project == "" {
		return fmt.Errorf("project is required: set BOAT_PROJECT or use --task with a project-labeled bead")
	}

	// Auto-generate agent name (includes project prefix for disambiguation).
	agentName := ""
	if taskBead != nil {
		agentName = spawnGenerateAgentName(project, taskBead.Title)
	}

	// Fall back to project-based name if no task.
	if agentName == "" {
		agentName = spawnGenerateSpawnName(project)
	}

	return spawnAndPrint(cmd, agentName, project, taskID, role, customPrompt, escalate)
}

// runStart handles 'gb start' — explicit agent name required (old 'gb spawn' behavior).
func runStart(cmd *cobra.Command, args []string) error {
	role, _ := cmd.Flags().GetString("role")
	taskID, _ := cmd.Flags().GetString("task")
	customPrompt, _ := cmd.Flags().GetString("prompt")
	escalate, _ := cmd.Flags().GetBool("escalate")

	ctx := cmd.Context()

	// Resolve the task bead (following ticket references) and infer its
	// project: label.
	resolvedTaskID, taskProject, taskBead, err := resolveTaskProject(ctx, taskID)
	if err != nil {
		return err
	}
	taskID = resolvedTaskID

	var agentName, project string

	switch len(args) {
	case 2:
		// gb start <name> <project>
		agentName = args[0]
		project = args[1]
	case 1:
		if taskID != "" {
			// Task-first mode: gb start <project> --task <id>
			project = args[0]
			agentName = spawnGenerateAgentName(project, taskBead.Title)
		} else {
			// gb start <name> — project from env.
			agentName = args[0]
			project = defaultProject()
		}
	}

	// If task had a project label and no project was specified, use it.
	if project == "" && taskProject != "" {
		project = taskProject
	}

	// Fall back to env project.
	if project == "" {
		project = defaultProject()
	}

	if project == "" {
		return fmt.Errorf("project is required: specify as second argument or set BOAT_PROJECT")
	}

	// Validate agent name.
	if !spawnIsValidAgentName(agentName) {
		return fmt.Errorf("invalid agent name %q — use lowercase letters, digits, and hyphens only", agentName)
	}

	return spawnAndPrint(cmd, agentName, project, taskID, role, customPrompt, escalate)
}

// spawnAndPrint creates the agent bead and prints the result.
func spawnAndPrint(cmd *cobra.Command, agentName, project, taskID, role, customPrompt string, escalate bool) error {
	var extraFields []map[string]string

	// --harness: validate before any daemon call so a typo fails here with the
	// allowed values, not as a 400 from the create. The bead's harness field is
	// the agent-level (highest) layer of the controller's resolution.
	rawHarness, _ := cmd.Flags().GetString("harness")
	harness, err := beadsapi.ParseHarness(rawHarness)
	if err != nil {
		return fmt.Errorf("--harness: %w", err)
	}
	if harness != "" {
		extraFields = append(extraFields, map[string]string{"harness": harness})
	}

	// A lands-gardener spawns only within the purse (Lands step 9.5): the
	// wall, an unknown purse, a full max_live_agents or a step estimate past
	// what remains above the margin refuse it here, before anything exists.
	stepFields, err := gateStepSpawn(cmd, stepSpawnDaemon(), project, role, taskID, time.Now())
	if err != nil {
		return err
	}
	if len(stepFields) > 0 {
		extraFields = append(extraFields, stepFields)
	}

	// Inherit slack-thread metadata from the task bead when present so the
	// gardener-spawn / gb spawn --task path matches the slack thread-spawn
	// behavior in bridge/bot_mentions.go. Without this, an agent spawned
	// against a slack-thread task bead cannot run `gb slack thread` or
	// `gb slack reply` because its Fields lack the binding (ka-0000007355).
	explicitChannel, _ := cmd.Flags().GetString("slack-thread-channel")
	explicitTs, _ := cmd.Flags().GetString("slack-thread-ts")
	slackFields := resolveSlackFields(cmd.Context(), taskID, explicitChannel, explicitTs)
	if len(slackFields) > 0 {
		extraFields = append(extraFields, slackFields)
	}

	// Spawn-channel resolution (ka-0000004297 / ka-0000004174). resolveChannel
	// honors slack_spawn_channel FIRST (before channel_roles / project primary),
	// so whatever lands here pins the spawned agent's reporting channel.
	// Precedence, highest first:
	//   1. explicit --slack-spawn-channel flag (operator or gardener intent)
	//   2. slack_spawn_channel inherited from a slack-bound task bead
	//   3. the spawner's own resolved channel (gardener→crew propagation)
	//
	// The flag closes the manual-gardener-fire gap: a gardener fired via `gb
	// spawn` carries no schedule_slack_channel, so without it resolveChannel
	// falls through to channel_roles[role:gardener] and misroutes. It also gives
	// a gardener prompt an explicit way to pass its channel down to crew.
	// Appending after the task-inherited slackFields entry lets the flag win via
	// SpawnAgentResult's last-write-wins field merge.
	explicitSpawnChannel, _ := cmd.Flags().GetString("slack-spawn-channel")
	switch {
	case explicitSpawnChannel != "":
		// Accept a channel ID or a name; resolve names to IDs best-effort so the
		// stored field matches the ID form the scheduler/thread paths write.
		ch := explicitSpawnChannel
		if bridgeURL := resolveBridgeURL(); bridgeURL != "" {
			if resolved, err := resolveChannelArg(bridgeURL, explicitSpawnChannel); err == nil && resolved != "" {
				ch = resolved
			}
		}
		extraFields = append(extraFields, map[string]string{"slack_spawn_channel": ch})
	case slackFields["slack_spawn_channel"] == "":
		if ch := spawnerSlackChannel(cmd.Context()); ch != "" {
			// Mark the channel as inherited and record the spawner's role so
			// the bridge lets the child's own channel_roles mapping win when
			// the roles differ (same-role children stay with their parent).
			// A human running gb spawn outside a pod has no BOAT_ROLE; their
			// SLACK_SPAWN_CHANNEL is an explicit pin, so no marker is written.
			fields := map[string]string{"slack_spawn_channel": ch}
			if spawnerRole := os.Getenv("BOAT_ROLE"); spawnerRole != "" {
				fields[beadsapi.FieldSlackSpawnChannelInheritedFromRole] = spawnerRole
			}
			extraFields = append(extraFields, fields)
		}
	}

	// Determine spawner identity: prefer credential store user (OIDC), fall back to actor.
	spawnedBy := ""
	if store, err := credstore.Load(""); err == nil {
		if cur := store.Current(); cur != nil && cur.User != nil && cur.User.Email != "" {
			spawnedBy = cur.User.Email
		}
	}
	if spawnedBy == "" && actor != "" && actor != "unknown" {
		spawnedBy = actor
	}
	if spawnedBy != "" {
		extraFields = append(extraFields, map[string]string{"spawned_by": spawnedBy})
	}

	// Push operated_by onto the new agent bead so the spawned pod boots with
	// BEADS_OPERATED_BY set (operatedByFromMetadata reads Fields["operated_by"])
	// and downstream kd/gb calls inherit the operator transparently. R2 §3
	// hybrid push+pull. The CLI's --operated-by flag defaults to the caller's
	// BEADS_OPERATED_BY env, so this picks up the operator naturally for human
	// CLI runs and for in-pod gb spawn calls (gardener → crew). Sentinel actors
	// (system:*, fleet:*) propagate verbatim — no strip; the dibs rule on the
	// server keeps them transparent.
	if operatedBy != "" {
		extraFields = append(extraFields, map[string]string{"operated_by": operatedBy})
	}
	if f := spawnOwnerFields(operatedBy); f != nil {
		extraFields = append(extraFields, f)
	}

	// Soft role validation (ka-0000002020): warn — but do NOT block — when the
	// requested role is neither a builtin nor backed by a type:role bead. Roles
	// are validated free strings by design (beadsapi.ValidateAgentRoles), so a
	// typo'd role still spawns; this one-line stderr signal stops it from
	// silently producing a pod that runs on global instruction defaults.
	// roleWarning fails open on any daemon error, so spawning never depends on
	// it.
	if w := roleWarning(cmd.Context(), daemon, role); w != "" {
		fmt.Fprintln(os.Stderr, w)
	}

	// Single-claim guard (ka-0000004654): a `gb spawn --task <id>` is a
	// single-claim crew assignment — one agent owns that task bead at a time.
	// Opt the daemon-side guard in so a second spawn against a task a live agent
	// already holds returns the existing agent instead of racing it (the
	// duplicate `terraform apply` collision this bug describes). No-op when no
	// task is targeted (ad-hoc / thread spawns).
	//
	// The same opt-in also arms the SYNCHRONOUS PRE-CLAIM (ka-0000002756): the
	// target bead is stamped in_progress + assignee at spawn time, so it drops out
	// of `gb ready` immediately instead of sitting open+unassigned for the ~3m
	// pod-boot window in which a second gardener lane could double-dispatch it.
	if taskID != "" {
		extraFields = append(extraFields, map[string]string{"single_claim_task": "1"})
	}

	createCtx := cmd.Context()
	if escalate {
		createCtx = beadsapi.WithEscalate(createCtx)
	}
	spawnRes, err := daemon.SpawnAgentResult(createCtx, agentName, project, taskID, role, customPrompt, extraFields...)
	if err != nil {
		// --escalate: the daemon denied this create as an escalatable
		// privileged-role spawn and, seeing X-Escalate: true, auto-filed an
		// escalation bead in the same round trip instead of a bare 403
		// (kbeads/internal/server/write_authz.go handleEscalation). This is
		// the success path for `gb spawn --escalate`, not a failure.
		var esc *beadsapi.EscalationCreated
		if errors.As(err, &esc) {
			return reportSpawnEscalation(cmd, esc, agentName, project, role, taskID)
		}
		errMsg := err.Error()
		switch {
		case strings.Contains(errMsg, "401") || strings.Contains(errMsg, "Unauthorized"):
			return fmt.Errorf("authentication failed — run 'gb login' to authenticate")
		case strings.Contains(errMsg, "403") || strings.Contains(errMsg, "Forbidden") || strings.Contains(errMsg, "spawn denied"):
			if !escalate {
				return fmt.Errorf("permission denied — you don't have permission to spawn agents on project %q with role %q (role may be privileged: retry with --escalate)", project, role)
			}
			return fmt.Errorf("permission denied — you don't have permission to spawn agents on project %q with role %q", project, role)
		default:
			return fmt.Errorf("spawning agent %q: %w", agentName, err)
		}
	}

	beadID := spawnRes.BeadID
	// Report the name of the agent the daemon actually returned. On a
	// single-claim hit that is the INCUMBENT holding the task, not the name
	// generated for this call — which was never created. Scripted callers read
	// this name back (`--jq '.name'`), so naming a nonexistent agent here would
	// hand them a phantom.
	if spawnRes.AgentName != "" {
		agentName = spawnRes.AgentName
	}

	// Lands spawn wall (step 8.3): in a tenant land, wait for the controller's
	// verdict so a refusal (frozen, tenant spawns off, purse at the wall or
	// unknown) is printed here instead of leaving a silent agent bead.
	// Operator lands return at once.
	if spawnRes.Outcome != beadsapi.SpawnExisting {
		if r, refused := awaitSpawnWall(cmd.Context(), spawnWallDaemon(), project, beadID, spawnWallWait, spawnWallPoll); refused {
			return reportSpawnWallRefusal(agentName, beadID, project, role, taskID, r)
		}
	}

	if jsonOutput {
		result := map[string]string{
			"id":      beadID,
			"name":    agentName,
			"project": project,
			"role":    role,
			// status is the machine contract: "created" when this call created the
			// agent, "existing" when an idempotence guard returned a live incumbent.
			// Always present, so `--jq '.status'` never yields null on success.
			"status": string(spawnRes.Outcome),
		}
		if spawnRes.Reason != "" {
			result["reason"] = spawnRes.Reason
		}
		if taskID != "" {
			result["task"] = taskID
		}
		if spawnedBy != "" {
			result["spawned_by"] = spawnedBy
		}
		printJSON(result)
		return nil
	}

	// Machine-greppable success line, printed FIRST so a scripted caller can
	// confirm a spawn with a single stable anchor (`grep '^SPAWNED '`) instead of
	// parsing human prose. The previous first line was "Spawning agent <name>" —
	// a present participle that reads as "in progress, outcome unknown" and
	// matches no obvious success token, so a gardener grepping '^Spawned' got an
	// empty match on a SUCCESSFUL spawn and re-dispatched three times
	// (bug ka-0000001243). status= distinguishes a real dispatch from a
	// guard-deduped no-op; errors return non-zero and print no SPAWNED line.
	fmt.Printf("SPAWNED agent=%s bead=%s status=%s", agentName, beadID, spawnRes.Outcome)
	if taskID != "" {
		fmt.Printf(" task=%s", taskID)
	}
	if spawnRes.Reason != "" {
		fmt.Printf(" reason=%s", spawnRes.Reason)
	}
	fmt.Println()

	if spawnRes.Outcome == beadsapi.SpawnExisting {
		fmt.Printf("Agent %s already exists — returned the live agent, nothing new was spawned.\n", agentName)
	} else {
		fmt.Printf("Spawning agent %s\n", agentName)
	}
	fmt.Printf("  Bead:    %s\n", beadID)
	fmt.Printf("  Project: %s\n", project)
	fmt.Printf("  Role:    %s\n", role)
	if spawnedBy != "" {
		fmt.Printf("  User:    %s\n", spawnedBy)
	}
	if taskID != "" {
		fmt.Printf("  Task:    %s\n", taskID)
	}
	if customPrompt != "" {
		preview := customPrompt
		if len(preview) > 60 {
			preview = preview[:57] + "..."
		}
		fmt.Printf("  Prompt:  %s\n", preview)
	}
	if spawnRes.Outcome == beadsapi.SpawnExisting {
		fmt.Println("\nNo new pod will be scheduled. Use 'gb agent roster' to check the existing agent.")
	} else {
		fmt.Println("\nThe reconciler will schedule a pod shortly. Use 'gb agent roster' to check status.")
	}

	return nil
}

// reportSpawnEscalation handles the `gb spawn --escalate` outcome: the
// daemon denied the create because role is privileged for this (non-human)
// caller, but — seeing X-Escalate: true — auto-filed an escalation bead in
// the same round trip (kbeads/internal/server/write_authz.go
// handleEscalation) instead of returning a bare 403. This is the "files the
// escalation in one step" half of the acceptance criteria (gd-lsgF8rL4Zm):
// no second `gb escalation create` call is needed.
//
// It prints the escalation id, links the escalation back to the CALLER's own
// task bead (not taskID, which is the target being assigned to the
// not-yet-spawned agent) via requesting_task + a blocks-edge — mirroring
// `gb escalation create`'s homing (escalation_create.go) — so the escalation
// surfaces on /plans and `gb news` instead of landing as an orphan in the
// flat /escalations inbox, then comments on that task bead so anything
// reading the caller's work sees the pending approval. Both are best-effort:
// the escalation exists and must not be lost to a follow-up write failing.
//
// Whether a spawn_policy rule (spawn_policy.go, gd-vmLhgqydEa) already
// auto-approved and replayed the create synchronously is not visible here —
// handleEscalation always responds 202 pointing at the escalation bead
// regardless. `gb escalation show <id>` or `gb agent roster` is the
// follow-up to confirm the gc-dogfood no-human-in-the-loop case actually
// finished spawning.
func reportSpawnEscalation(cmd *cobra.Command, esc *beadsapi.EscalationCreated, agentName, project, role, taskID string) error {
	ctx := cmd.Context()

	callerTask := deriveRequestingTask(ctx, "")
	if callerTask != "" {
		if err := daemon.UpdateBeadFields(ctx, esc.EscalationID, map[string]string{
			"requesting_task": callerTask,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "WARNING: escalation %s created but could not set requesting_task %s: %v\n", esc.EscalationID, callerTask, err)
		} else if err := daemon.AddDependency(ctx, callerTask, esc.EscalationID, "blocks", actor); err != nil {
			fmt.Fprintf(os.Stderr, "WARNING: escalation %s created but could not link blocks-edge from %s: %v\n", esc.EscalationID, callerTask, err)
		}
		if planID := resolveOwningPlan(ctx, callerTask); planID != "" {
			if err := daemon.UpdateBeadFields(ctx, esc.EscalationID, map[string]string{
				"requesting_plan": planID,
			}); err != nil {
				fmt.Fprintf(os.Stderr, "WARNING: could not record requesting_plan %s on escalation %s: %v\n", planID, esc.EscalationID, err)
			}
		}

		comment := fmt.Sprintf("gb spawn --escalate: role %q is privileged; filed escalation %s to spawn agent %q on project %q",
			role, esc.EscalationID, agentName, project)
		if taskID != "" {
			comment += fmt.Sprintf(" for task %s", taskID)
		}
		comment += ". Waiting on operator approval or an auto-approving spawn_policy rule."
		if err := daemon.AddComment(ctx, callerTask, actor, comment); err != nil {
			fmt.Fprintf(os.Stderr, "WARNING: escalation %s created but could not comment on caller task %s: %v\n", esc.EscalationID, callerTask, err)
		}
	} else {
		fmt.Fprintf(os.Stderr, "WARNING: escalation %s has no caller task to link — running outside an agent pod (no BOAT_AGENT_BEAD_ID/task_id)? It is visible only in the flat /escalations inbox.\n", esc.EscalationID)
	}

	if jsonOutput {
		result := map[string]string{
			"status":     "escalated",
			"escalation": esc.EscalationID,
			"agent":      agentName,
			"project":    project,
			"role":       role,
		}
		if taskID != "" {
			result["task"] = taskID
		}
		if callerTask != "" {
			result["requesting_task"] = callerTask
		}
		printJSON(result)
		return nil
	}

	// Machine-greppable line, mirroring the SPAWNED contract above: a
	// successful escalation-file prints this and exits 0 — this outcome IS
	// success for `gb spawn --escalate`, not a failure to be grepped away.
	fmt.Printf("ESCALATED escalation=%s agent=%s role=%s project=%s", esc.EscalationID, agentName, role, project)
	if taskID != "" {
		fmt.Printf(" task=%s", taskID)
	}
	fmt.Println()
	fmt.Printf("  Role %q is privileged; the spawn was denied and an escalation was filed instead.\n", role)
	fmt.Printf("  Escalation: %s\n", esc.EscalationID)
	if callerTask != "" {
		fmt.Printf("  Commented on caller task: %s\n", callerTask)
	}
	fmt.Println("\nApproval (human, or an auto-approving spawn_policy rule) will let the agent spawn — no further action needed here.")
	return nil
}

// --- helpers (local to spawn, mirroring bridge/bot_commands.go) ---

// spawnTicketRefRe matches external ticket references like "PE-1234".
var spawnTicketRefRe = regexp.MustCompile(`^[A-Za-z]+-\d+$`)

// spawnIsValidAgentName reports whether s is a valid agent name.
func spawnIsValidAgentName(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
			return false
		}
	}
	return true
}

// spawnGenerateAgentName creates a slug from a task title.
// When project is non-empty it is prepended for disambiguation across projects.
func spawnGenerateAgentName(project, title string) string {
	words := strings.Fields(strings.ToLower(title))
	var slugWords []string
	for _, w := range words {
		var clean strings.Builder
		for _, c := range w {
			if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
				clean.WriteRune(c)
			}
		}
		if clean.Len() > 0 {
			slugWords = append(slugWords, clean.String())
		}
		if len(slugWords) == 3 {
			break
		}
	}
	if len(slugWords) == 0 {
		slugWords = []string{"agent"}
	}
	slug := strings.Join(slugWords, "-") + "-" + spawnRandomSuffix(3)
	if project != "" {
		return project + "-" + slug
	}
	return slug
}

// spawnGenerateSpawnName creates a name from a project with a random suffix.
func spawnGenerateSpawnName(project string) string {
	prefix := project
	if prefix == "" {
		prefix = "agent"
	}
	return prefix + "-" + spawnRandomSuffix(4)
}

// spawnRandomSuffix returns a random string of n lowercase alphanumeric characters.
func spawnRandomSuffix(n int) string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = chars[rand.IntN(len(chars))]
	}
	return string(b)
}

// spawnProjectFromLabels extracts the project name from a bead's labels.
// resolveTaskProject resolves a --task reference to a canonical gd- bead ID,
// fetches the bead, and infers its project from a project: label. A ticket
// reference (e.g. PE-1234) is first resolved to a bead ID via ResolveTicket;
// a gd- bead ID is fetched directly. The returned bead (nil when taskRef is
// empty) lets callers reuse the title for agent-name generation without a
// second round-trip. This is the single source of project inference for both
// `gb spawn` and `gb start` so a project-labeled task bead never trips the
// "project is required" error (ka-0000007269).
func resolveTaskProject(ctx context.Context, taskRef string) (taskID, project string, bead *beadsapi.BeadDetail, err error) {
	taskID = taskRef
	if taskID == "" {
		return "", "", nil, nil
	}

	// Resolve ticket references (e.g. PE-1234) to bead IDs.
	if !(strings.HasPrefix(taskID, "gd-") || strings.HasPrefix(taskID, "ka-")) {
		if spawnTicketRefRe.MatchString(taskID) {
			resolved, rerr := daemon.ResolveTicket(ctx, taskID)
			if rerr != nil {
				return "", "", nil, fmt.Errorf("resolving ticket %q: %w", taskID, rerr)
			}
			taskID = resolved.ID
		}
	}

	bead, err = daemon.GetBead(ctx, taskID)
	if err != nil {
		return "", "", nil, fmt.Errorf("looking up task %q: %w", taskID, err)
	}
	if err := refuseLocallyPlaced(taskID, bead); err != nil {
		return "", "", nil, err
	}
	return taskID, spawnProjectFromLabels(bead.Labels), bead, nil
}

// refuseLocallyPlaced refuses to pod-spawn a task placed on a local runner
// (placement=local, local-agents spec §7): the runner's own agent picks it up.
func refuseLocallyPlaced(taskID string, bead *beadsapi.BeadDetail) error {
	if bead == nil || bead.Fields["placement"] != "local" {
		return nil
	}
	runner := bead.Fields["runner_id"]
	if runner == "" && bead.Fields["runner_tag"] != "" {
		runner = "tagged " + bead.Fields["runner_tag"]
	}
	return fmt.Errorf("task %s is placed on runner %s; it will be picked up by gb runner join there", taskID, runner)
}

func spawnProjectFromLabels(labels []string) string {
	for _, l := range labels {
		if v, ok := strings.CutPrefix(l, "project:"); ok {
			return v
		}
	}
	return ""
}

// slackFieldsToInherit are the task-bead fields that, when present, should be
// copied onto the spawned agent bead so the agent's Slack tools (`gb slack
// thread`, `gb slack reply`) can re-bind to the originating thread without
// re-walking the lineage chain. Mirrors the field set written by the
// thread-spawn path in bridge/bot_mentions.go and bridge/thread_bead.go.
//
// thread_id (and its legacy compatibility alias slack_thread_bead_id) carry the
// origin-agnostic wish-pointer: the ka-id of the thread (wish) head this task
// belongs to. Inheriting it lets non-Slack wishes — those carrying only
// thread_id, with no slack_* fields — pass their thread binding to the agent.
// See bridge/thread_bead.go for the wish-pointer invariant.
var slackFieldsToInherit = []string{
	"slack_thread_channel",
	"slack_thread_ts",
	"slack_spawn_channel",
	"slack_opener_user_id",
	"slack_permalink",
	"thread_id",
	"slack_thread_bead_id",
}

// taskHasSlackBinding reports whether the given task bead represents (or was
// raised from) a thread. A task qualifies when it carries the `source:slack`
// label, OR has a non-empty `slack_thread_channel` field (covers older beads
// that predate the canonical label), OR has a non-empty `thread_id` field
// (covers origin-agnostic wishes that bind to a thread head by ka-id without
// any slack_* fields).
func taskHasSlackBinding(labels []string, fields map[string]string) bool {
	if slices.Contains(labels, "source:slack") {
		return true
	}
	if fields["slack_thread_channel"] != "" {
		return true
	}
	return fields["thread_id"] != ""
}

// extractSlackInheritFields returns the subset of slackFieldsToInherit that
// are present (non-empty) on the task bead's Fields map. Returns nil when the
// task does not carry a Slack binding (see taskHasSlackBinding) or when none
// of the inheritable fields are populated.
func extractSlackInheritFields(labels []string, fields map[string]string) map[string]string {
	if !taskHasSlackBinding(labels, fields) {
		return nil
	}
	out := make(map[string]string, len(slackFieldsToInherit))
	for _, k := range slackFieldsToInherit {
		if v := fields[k]; v != "" {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// resolveSlackFields combines task-bead inheritance with explicit --slack-thread-*
// flag overrides. Explicit flags win over inherited values. Returns nil when
// neither source contributes anything.
func resolveSlackFields(ctx context.Context, taskID, explicitChannel, explicitTs string) map[string]string {
	out := map[string]string{}
	if taskID != "" {
		maps.Copy(out, inheritSlackFieldsFromTask(ctx, taskID))
	}
	if explicitChannel != "" {
		out["slack_thread_channel"] = explicitChannel
	}
	if explicitTs != "" {
		out["slack_thread_ts"] = explicitTs
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// spawnerSlackChannel returns the Slack channel that the spawning agent (this
// process) reports to, so a gardener spawning crew can propagate its channel
// down the tree. Resolution order mirrors the controller-side resolveChannel
// precedence for an agent's own routing:
//
//  1. SLACK_SPAWN_CHANNEL env — set when the agent itself was spawned with a
//     pinned channel (manual gb spawn from a channel, or thread spawn channel).
//  2. The spawner's own agent bead (KD_AGENT_ID / BOAT_AGENT_BEAD_ID):
//     slack_spawn_channel, then schedule_slack_channel (the scheduler copies a
//     gardener's configured channel here on each scheduled fire), then
//     slack_thread_channel.
//
// Returns "" when none resolve (e.g. running outside a pod, or an agent with no
// channel binding) so the caller falls through to the existing channel_roles /
// project-primary routing unchanged.
func spawnerSlackChannel(ctx context.Context) string {
	if ch := os.Getenv("SLACK_SPAWN_CHANNEL"); ch != "" {
		return ch
	}
	selfBeadID := envOr("KD_AGENT_ID", os.Getenv("BOAT_AGENT_BEAD_ID"))
	if selfBeadID == "" || daemon == nil {
		return ""
	}
	bead, err := daemon.GetBead(ctx, selfBeadID)
	if err != nil || bead == nil {
		return ""
	}
	for _, k := range []string{"slack_spawn_channel", "schedule_slack_channel", "slack_thread_channel"} {
		if v := bead.Fields[k]; v != "" {
			return v
		}
	}
	return ""
}

// inheritSlackFieldsFromTask fetches the task bead and returns the slack-
// thread fields that should be inherited onto a gardener-spawned agent. Best-
// effort: any daemon error or a non-slack task yields nil so spawn continues
// without slack binding.
func inheritSlackFieldsFromTask(ctx context.Context, taskID string) map[string]string {
	if daemon == nil || taskID == "" {
		return nil
	}
	bead, err := daemon.GetBead(ctx, taskID)
	if err != nil || bead == nil {
		return nil
	}
	return extractSlackInheritFields(bead.Labels, bead.Fields)
}

// spawnOwnerFields names a human operator as the spawned agent's owner, as the
// trigger engine and the bridge stamp the human an agent was spawned for, so a
// gardener's crew is the owner's too. A sentinel operator (system:*, fleet:*)
// owns nothing: nil.
func spawnOwnerFields(operatedBy string) map[string]string {
	if !isHumanOperatorID(operatedBy) {
		return nil
	}
	return map[string]string{"owner": operatedBy}
}
