package main

// gb gardener fire-now — spawn ONE gardener cycle immediately, out-of-schedule,
// WITHOUT mutating the gardener's schedule.
//
// Motivation (ka-0000000869, surfaced by the OSS-sanitization revival
// ka-0000005359): proving a revived/newly-armed gardener actually CYCLES used to
// require a manual, error-prone dance — temporarily rewrite the gardener's cron
// to a due-now expression so the engine's cooldown gate bypasses cooldown
// (ScheduleTriggerSource.CronDue → cron-due-window bypass), wait ~30s for the
// next eval tick to fire, then RESTORE the original cron. Done wrong (bad
// minute/hour/dow, or a crash before restore) it double-fires or leaves the
// production schedule broken.
//
// Design — direct one-shot spawn, schedule untouched. `gb` is an out-of-process
// CLI: it cannot synchronously drive the in-process TriggerEngine, but it CAN
// spawn an agent directly (the same daemon.SpawnAgentResult path `gb spawn`
// uses). fire-now resolves the gardener trigger bead, parses its gardener blob +
// spawn config with bridge.ParseTriggerDef, and spawns exactly ONE gardener-cycle
// agent that carries the identical "## Scheduled Loop Context" fields a real
// cron fire would stamp (via stampGardenerLoopContext parity below).
//
// Crucially it NEVER writes `enabled` or `cron` on the schedule bead. So
// "auto-restore the schedule to its prior enable/cron state" (AC #2) is
// satisfied BY CONSTRUCTION — a disabled gardener stays disabled and an enabled
// one keeps its cron loop, because fire-now touches neither field. This removes
// the whole hazard class the origin bead describes: there is no due-now cron to
// write and therefore no restore step that a crash could skip.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"

	"github.com/spf13/cobra"
)

var (
	gardenerFireNowForce  bool
	gardenerFireNowPrompt string
)

var gardenerFireNowCmd = &cobra.Command{
	Use:   "fire-now <gardener-id-or-epic-id>",
	Short: "Spawn ONE gardener cycle immediately, out-of-schedule (schedule untouched)",
	Long: `Spawn exactly ONE cycle of a gardener right now, out of its normal cron
schedule — a first-class, safe replacement for the manual "rewrite the cron to
due-now, wait for the tick, restore the cron" dance used to verify that a
revived or newly-armed gardener actually CYCLES.

The gardener's schedule is NEVER mutated. fire-now spawns the cycle agent
directly (the same spawn path a cron fire uses), stamping the identical
"## Scheduled Loop Context" the agent would get from a scheduled fire. Because
the enabled/cron fields are left untouched, the schedule auto-restores to its
prior state by construction:

  - a DISABLED gardener stays disabled after the one-shot fire;
  - an ENABLED gardener keeps running its cron loop.

There is no temporary due-now cron to leave broken if the command is
interrupted, which is the failure mode the manual procedure risked.

<gardener-id-or-epic-id> may be the gardener trigger bead itself OR the epic it
drives (the same resolution 'gb gardener status' uses). The gardener's singleton
guard is honored: fire-now refuses when a live cycle for this gardener is already
running, unless --force is given.

Examples:
  # Fire one cycle of a gardener by its trigger bead id
  gb gardener fire-now ka-0000007535

  # Fire one cycle by the epic the gardener drives
  gb gardener fire-now ka-0000005359

  # Verify a disabled gardener cycles without re-enabling it, then confirm the
  # cycle landed (gardener-audit reflects it) — the schedule stays disabled:
  gb gardener fire-now ka-0000007535
  gb gardener-audit --schedule-id ka-0000007535 --since 1h

  # Force a second concurrent cycle even though one is already live
  gb gardener fire-now ka-0000007535 --force

  # Machine-readable result (agent bead id, epic, enabled_unchanged flag)
  gb gardener fire-now ka-0000007535 --json`,
	Args: cobra.ExactArgs(1),
	RunE: runGardenerFireNow,
}

func runGardenerFireNow(cmd *cobra.Command, args []string) error {
	beadID := strings.TrimSpace(args[0])
	if beadID == "" {
		return fmt.Errorf("gardener id (or epic id) is required")
	}
	ctx := cmd.Context()

	// Resolve the gardener trigger bead. Accepts the gardener bead id directly OR
	// the epic it drives (disabled/closed gardeners resolve too, so a "fire the
	// disabled gardener once" flow works — resolveGardenerSchedule falls back to
	// any gardener bead for the epic).
	sched, err := resolveGardenerSchedule(ctx, daemon, beadID)
	if err != nil {
		return err
	}
	if sched == nil {
		return fmt.Errorf("no gardener found for %s", beadID)
	}
	if !beadIsGardener(sched) {
		return fmt.Errorf("%s is not a gardener (type=%q) — fire-now only fires gardener loops", sched.ID, sched.Type)
	}

	fired, err := fireGardenerCycle(ctx, sched, gardenerFireNowPrompt, gardenerFireNowForce)
	if err != nil {
		return err
	}
	if fired.EscalationID != "" {
		if jsonOutput {
			printJSON(map[string]any{
				"schedule_id":   sched.ID,
				"epic":          fired.Epic,
				"agent":         fired.AgentName,
				"outcome":       "escalated",
				"escalation_id": fired.EscalationID,
				"spawn_source":  "fire-now",
			})
			return nil
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Gardener cycle for %s needs operator approval: escalation %s filed (role %s is privileged).\n", sched.ID, fired.EscalationID, fired.Role)
		return nil
	}
	res := fired.Spawn
	assignBead := fired.Epic

	enabled := sched.Fields["enabled"] != "false"

	if jsonOutput {
		printJSON(map[string]any{
			"schedule_id":       sched.ID,
			"epic":              assignBead,
			"agent":             res.AgentName,
			"agent_bead":        res.BeadID,
			"outcome":           string(res.Outcome),
			"spawn_source":      "fire-now",
			"enabled":           enabled,
			"enabled_unchanged": true,
			"cron":              sched.Fields["cron"],
		})
		return nil
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Fired ONE cycle of gardener %s\n", sched.ID)
	fmt.Fprintf(cmd.OutOrStdout(), "  agent:   %s (%s)\n", res.AgentName, res.BeadID)
	fmt.Fprintf(cmd.OutOrStdout(), "  epic:    %s\n", assignBead)
	if res.Outcome == beadsapi.SpawnExisting {
		fmt.Fprintf(cmd.OutOrStdout(), "  note:    an equivalent agent already existed (%s); no new cycle spawned\n", res.Reason)
	}
	state := "enabled"
	if !enabled {
		state = "DISABLED"
	}
	fmt.Fprintf(cmd.OutOrStdout(),
		"  schedule: unchanged (still %s, cron %q) — a disabled gardener stays disabled\n",
		state, defaultIfEmpty(sched.Fields["cron"], "<none>"))
	return nil
}

// gardenerFireResult is what fireGardenerCycle spawned: the cycle agent, or
// the escalation filed when the spawn role is privileged.
type gardenerFireResult struct {
	Epic         string
	AgentName    string
	Role         string
	Spawn        beadsapi.SpawnResult
	EscalationID string
}

// fireGardenerCycle spawns ONE cycle of the gardener sched, the core of
// fire-now that gb lands plan answer reuses to start a home-garden plan's
// gardener. It never writes the schedule. Unless force, it refuses while a
// cycle is live.
func fireGardenerCycle(ctx context.Context, sched *beadsapi.BeadDetail, promptOverride string, force bool) (*gardenerFireResult, error) {
	// Parse the gardener trigger the same way the engine does, so the fired
	// cycle inherits identical spawn config (role, project, prompt, gardener
	// blob → loop-context fields). Source "schedule" matches how the trigger
	// engine loads a trigger:schedule gardener.
	trig := bridge.ParseTriggerDef(sched, "schedule")
	if trig.Gardener == nil {
		// A bare-shape gardener (no structured `gardener` blob) is still a valid
		// gardener — it carries assign_bead via the gardener-epic:<id> label and
		// its loop config in the cron pre-flight. We can still fire it; the
		// loop-context stamping below simply omits the blob-derived fields.
		trig.Gardener = &bridge.GardenerSpec{}
	}

	epic := gardenerEpicOf(sched)

	// Singleton guard (parallel to the engine's single-live-instance default,
	// bug ka-0000003097): refuse to fire while a live agent spawned by this
	// gardener is already working, unless --force. This keeps fire-now from
	// double-dispatching a cycle onto a gardener that is already mid-run.
	if !force {
		if live, holder := gardenerHasLiveCycle(ctx, daemon, sched.ID); live {
			return nil, fmt.Errorf(
				"gardener %s already has a live cycle (agent %s) — it is mid-run; "+
					"pass --force to fire a concurrent cycle anyway", sched.ID, holder)
		}
	}

	// Resolve the epic the cycle drives. A gardener-epic-pinned gardener reuses
	// its long-lived epic bead as the task (matches the engine's AssignBead
	// branch in actionSpawnAgent); the agent claims/works the epic's open
	// children. Without an epic we cannot point the cycle at anything.
	assignBead := strings.TrimSpace(trig.Gardener.AssignBead)
	if assignBead == "" {
		assignBead = epic
	}
	if assignBead == "" {
		return nil, fmt.Errorf(
			"gardener %s is not pinned to an epic (no assign_bead and no gardener-epic:<id> label); "+
				"fire-now needs an epic to point the cycle at", sched.ID)
	}

	project := trig.Project
	if project == "" {
		project = inheritProjectFromBead(sched)
	}
	if project == "" {
		return nil, fmt.Errorf("gardener %s has no project; cannot fire a cycle without one", sched.ID)
	}

	role := trig.Role
	if role == "" {
		role = "gardener"
	}

	// Build the cycle prompt: an explicit override wins, else the gardener's own
	// stored prompt (what a scheduled fire would run). Template placeholders are
	// expanded with the fire-now vars so a prompt referencing {{trigger_id}} etc.
	// renders identically to a scheduled fire.
	prompt := strings.TrimSpace(promptOverride)
	if prompt == "" {
		prompt = trig.Prompt
	}
	prompt = bridge.ExpandTemplate(prompt, map[string]string{
		"{{trigger_id}}":    trig.ID,
		"{{trigger_title}}": trig.Title,
		"{{cron}}":          sched.Fields["cron"],
		"{{timezone}}":      sched.Fields["timezone"],
	})

	agentName := gardenerFireNowAgentName(project, trig.Title)

	// Loop-context parity: stamp the SAME schedule_* fields the engine's
	// stampGardenerLoopContext writes, so the fired agent's `gb prime` renders
	// the "## Scheduled Loop Context" block (run N of M, terminal goal, prior
	// wrap-up) exactly like a scheduled run. spawn_source is "fire-now" (not
	// "schedule") so telemetry and audit can tell a manual verification fire
	// apart from a cron fire while everything else stays identical.
	extraFields := gardenerFireNowLoopFields(trig, sched)
	extraFields["spawn_source"] = "fire-now"
	extraFields["fire_now"] = "true"
	if operatedBy != "" {
		extraFields["operated_by"] = operatedBy
	}
	// spawned_by MUST be trigger:<id> — that is the exact value the engine
	// stamps and the value the singleton guard (liveSpawnHolders) and
	// `gb gardener-audit --schedule-id` query on, so a fire-now cycle joins the
	// same lineage as a scheduled fire. The human who fired it is preserved
	// separately in fire_now_by (operated_by carries the operator).
	extraFields["spawned_by"] = "trigger:" + sched.ID
	if actor != "" && actor != "unknown" {
		extraFields["fire_now_by"] = actor
	}

	// Send X-Escalate: a fire from an agent's own credential is a non-human
	// spawn, and a privileged gardener role makes the daemon deny it. Escalating
	// files an approval request instead of a bare 403 (gd-VuYwGGYLLT).
	res, err := daemon.SpawnAgentResult(beadsapi.WithEscalate(ctx), agentName, project, assignBead, role, prompt, extraFields)
	out := &gardenerFireResult{Epic: assignBead, AgentName: agentName, Role: role}
	if err != nil {
		var esc *beadsapi.EscalationCreated
		if errors.As(err, &esc) {
			out.EscalationID = esc.EscalationID
			return out, nil
		}
		return nil, fmt.Errorf("firing gardener cycle for %s: %w", sched.ID, err)
	}
	out.Spawn = res
	return out, nil
}

// gardenerFireNowLoopFields builds the schedule_* loop-context fields the fired
// cycle carries, mirroring bridge.stampGardenerLoopContext (which is unexported)
// so `gb prime`'s "## Scheduled Loop Context" renderer sees the same shape a
// scheduled fire produces. RunsSpawned is bumped by one for THIS run (matching
// the engine's spec.RunsSpawned+1). The gardener blob is not persisted back —
// fire-now adds no durable schedule state (per the tech notes), it only stamps
// the run's own agent bead.
func gardenerFireNowLoopFields(trig bridge.TriggerDef, sched *beadsapi.BeadDetail) map[string]string {
	spec := trig.Gardener
	f := map[string]string{
		"schedule_id":             sched.ID,
		"schedule_title":          trig.Title,
		"schedule_runs_spawned":   strconv.Itoa(spec.RunsSpawned + 1),
		"schedule_runs_completed": strconv.Itoa(spec.RunsCompleted),
		"schedule_max_runs":       strconv.Itoa(spec.MaxRuns),
		"trigger_id":              trig.ID,
		"trigger_title":           trig.Title,
		"source":                  "schedule",
	}
	if spec.TerminalGoal != "" {
		f["schedule_terminal_goal"] = spec.TerminalGoal
	}
	if spec.Name != "" {
		f["schedule_name"] = spec.Name
	}
	if spec.SlackChannel != "" {
		f["schedule_slack_channel"] = spec.SlackChannel
	}
	if spec.SlackUserID != "" {
		f["slack_user_id"] = spec.SlackUserID
	}
	if spec.UserBeadID != "" {
		f["user_bead_id"] = spec.UserBeadID
	}
	if spec.LastAgentID != "" {
		f["schedule_prior_agent_id"] = spec.LastAgentID
		f["prior_agent_id"] = spec.LastAgentID
	}
	if spec.LineageRoot != "" {
		f["lineage_root_id"] = spec.LineageRoot
	}
	return f
}

// gardenerHasLiveCycle reports whether the gardener identified by scheduleID
// already has a live agent working a cycle (spawned_by:trigger:<id>, in a
// non-terminal status). Mirrors the engine's liveSpawnHolders query. Fail-open:
// a daemon error is treated as "no live cycle" so a transient hiccup never
// wedges a legitimate fire (the operator can always --force). Returns the first
// live holder's bead id for the refusal message.
func gardenerHasLiveCycle(ctx context.Context, c *beadsapi.Client, scheduleID string) (bool, string) {
	res, err := c.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"agent"},
		Statuses: []string{"open", "in_progress", "blocked", "deferred"},
		Fields:   map[string]string{"spawned_by": "trigger:" + scheduleID},
	})
	if err != nil || res == nil {
		return false, ""
	}
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		return true, b.ID
	}
	return false, ""
}

// gardenerFireNowAgentName builds a K8s-safe agent name for the fired cycle,
// reusing the CLI's spawn-name generator (project-prefixed, random-suffixed).
// The gardener title seeds the slug and a fire-now marker keeps the name
// distinguishable from a scheduled fire in `gb gardener-audit` listings; the
// random suffix guarantees uniqueness.
func gardenerFireNowAgentName(project, title string) string {
	slug := strings.TrimSpace(title)
	if slug == "" {
		slug = "gardener"
	}
	return spawnGenerateAgentName(project, "fire-now "+slug)
}
