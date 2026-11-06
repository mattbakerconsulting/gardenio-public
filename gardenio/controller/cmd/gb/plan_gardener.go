// `gb plan gardener` — bind the ONE plan-gardener a /plans plan carries (epic
// ka-0000002022 Step 1, ka-0000002366; decision ka-0000002222 option
// surface-bind).
//
// The gardener-spec builder itself lives in internal/plangardener (task
// ka-0000002863 lifted it out of this package): plangardener.Bind is the ONE
// shared core this command, the beads-viewer POST /plans auto-bind, and the
// viewer's hand-mark path all call, so every binder emits byte-identical
// gardener beads (see TestPlanGardener_SharedBuilderParity). This file is the
// thin CLI wrapper: flag parsing, plan lookup, project defaulting, and
// output rendering.
//
// Idempotent-adopt: if a gardener is already pinned to the plan (gardener-epic
// label or legacy assign_bead), the command reports it and exits 0 WITHOUT
// creating a duplicate — running it across existing /plans plans is the
// backfill path. Adoption is enabled-state-agnostic, so a gardener bound
// PAUSED behind the approve→build gate (task ka-0000005350) is recognised
// too.
//
// DELIBERATELY NOT gated by --i-read-the-gardener-instructions (ka-0000006130,
// follow-up to the ka-0000007415 gate on `gb gardener start`). That gate exists
// to stop a HAND-ROLLED gardener that violates the five non-negotiable rules
// (self-block, cadence, blob-not-flat-fields, repo-less, prompts-as-advice —
// guide ka-0000004752); it echoes the guide so the AUTHOR loads those rules
// before supplying the shape. A plan-gardener has no such author-supplied shape:
// plangardener.Bind BUILDS the whole gardener bead and STRUCTURALLY enforces all
// five rules before persist — ValidateGardenerBlob + ValidateGardenerStandingBlob
// (rule 1: fail-closed backstop/self-block/standing marker), event-mode firing
// with a staggered daily watchdog (or an opt-in staggered-30m cron) + 5m
// cooldown (rule 2), the nested gardener blob (rule 3), a
// fail-loud empty-project guard (rule 4), and a template-rendered prompt (rule
// 5). There is nothing for the operator to get wrong for the guide to correct.
//
// The gate is also un-appliable HERE without making things worse: plangardener.Bind
// is the ONE shared core behind THREE seams — this CLI, the POST /plans auto-bind,
// and the viewer's hand-mark / "Enable gardener" button. Only the CLI can carry a
// --flag; gating just the CLI would herd authors toward the UNGATED browser button.
// The /plans authoring flow (docs/plan-authoring-runbook.md) is itself the guided
// front door for plan-gardeners, and a required flag would also break the
// backfill path (`gb plan gardener <id>` across existing plans). Exemption locked
// by TestPlanGardener_NoReadInstructionsGate.
package main

import (
	"fmt"
	"strings"

	"gardenio/controller/internal/gardenermode"
	"gardenio/controller/internal/plangardener"

	"github.com/spf13/cobra"
)

// planGardenerSpawnRole / planGardenerMarkerLabel / planGardenerDefaultMaxRuns
// are the shared plan-gardener constants, re-exposed under their historical
// cmd/gb names for the flag defaults and output paths below. The canonical
// definitions (and their full rationale) live in internal/plangardener.
const (
	planGardenerSpawnRole      = plangardener.SpawnRole
	planGardenerMarkerLabel    = plangardener.MarkerLabel
	planGardenerDefaultMaxRuns = plangardener.DefaultMaxRuns
)

var (
	planGardenerCron       string
	planGardenerTimed      bool
	planGardenerFallback   string
	planGardenerTZ         string
	planGardenerProject    string
	planGardenerMaxRuns    int
	planGardenerLoopWall   string
	planGardenerTitle      string
	planGardenerSlackCh    string
	planGardenerPromptOvr  string
	planGardenerExecTarget string
)

var planGardenerCmd = &cobra.Command{
	Use:   "gardener <plan-epic-id>",
	Short: "Bind (or adopt) the plan's single watchdog plan-gardener",
	Long: `Idempotently bind ONE plan-gardener to a /plans plan epic.

The plan-gardener is a type=trigger / action=spawn_agent bead pinned to the
plan via a gardener-epic:<plan-id> label (plus a plan-gardener marker label).

It is born in EVENT mode (trigger:multi): any step created, updated or closed
under the plan wakes it (debounced 10m, throttled 15m), and a daily
fallback-watchdog cron, staggered from the plan id and gated on the plan still
having open steps, is the floor that keeps a plan from going silent.
--fallback-cron overrides that watchdog. --timed (or --cron '<expr>') binds a
TIMED gardener (trigger:schedule) instead, on a staggered 30-minute cron (or
the given one). 'gb gardener mode' flips an existing gardener either way. Its spawn role is 'plan-gardener', so every iteration the
engine fires IS the plan's bound agent — one agent lineage that steers
(minting steps with kd create + kd dep add, since the 'gb plan' write
verbs require an oidc:<email> operator), watches for orphaned work, and
answers operator messages from /plans/{id}.

The target epic must carry the 'plan' label (the /plans marker). Mark the
epic as a plan first — on the /plans page, or 'kd label add <id> plan' —
or use 'gb gardener start' for a plain (non-plan) epic gardener.

If a gardener is already pinned to the plan, the command adopts it: it
reports the existing gardener and exits 0 without creating a duplicate.
Running this across existing plans is the backfill path.`,
	Args: cobra.ExactArgs(1),
	RunE: runPlanGardener,
}

func init() {
	planCmd.AddCommand(planGardenerCmd)
	planGardenerCmd.Flags().StringVar(&planGardenerCron, "cron", "", "bind a TIMED gardener on this cron expression (default empty: event mode)")
	planGardenerCmd.Flags().BoolVar(&planGardenerTimed, "timed", false, "bind a TIMED gardener (trigger:schedule) on a deterministic staggered 30m cron seeded from the plan epic id, instead of the default event mode")
	planGardenerCmd.Flags().StringVar(&planGardenerFallback, "fallback-cron", "", "event-mode watchdog cron (default: a deterministic staggered daily cron seeded from the plan epic id)")
	planGardenerCmd.Flags().StringVar(&planGardenerTZ, "tz", "", "timezone (default: America/Los_Angeles)")
	planGardenerCmd.Flags().StringVar(&planGardenerProject, "project", "", "project label (default: inherited from the plan epic)")
	planGardenerCmd.Flags().IntVar(&planGardenerMaxRuns, "max-runs", planGardenerDefaultMaxRuns, "run budget per steering session before loop_end (the resume gate re-arms an unmet goal)")
	planGardenerCmd.Flags().StringVar(&planGardenerLoopWall, "loop-wall-clock", "168h", "hard wall-clock cap for the loop")
	planGardenerCmd.Flags().StringVar(&planGardenerTitle, "title", "", "trigger bead title (default: 'Plan gardener for <id> (<plan title>)')")
	planGardenerCmd.Flags().StringVar(&planGardenerSlackCh, "slack-channel", "", "Slack channel the gardener's iterations report to (e.g. the planner channel)")
	planGardenerCmd.Flags().StringVar(&planGardenerPromptOvr, "prompt", "", "override the plan-gardener playbook prompt (advanced; usually leave unset)")
	planGardenerCmd.Flags().StringVar(&planGardenerExecTarget, "execution-target", "", "runtime backend for the gardener's iterations (e.g. lambda_microvm); empty inherits the role/project default")
	planGardenerCmd.SilenceUsage = true
}

func runPlanGardener(cmd *cobra.Command, args []string) error {
	planID := strings.TrimSpace(args[0])
	if planID == "" {
		return fmt.Errorf("plan epic id is required")
	}
	if strings.TrimSpace(planGardenerFallback) != "" && (planGardenerTimed || strings.TrimSpace(planGardenerCron) != "") {
		return fmt.Errorf("--fallback-cron applies only to an event-mode gardener; it cannot be combined with --timed or --cron")
	}
	ctx := cmd.Context()

	plan, err := daemon.GetBead(ctx, planID)
	if err != nil {
		return fmt.Errorf("looking up plan epic %s: %w", planID, err)
	}

	// Resolve the project HERE (not in the shared core) so the gb-specific
	// environment fallback (defaultProject) stays out of the viewer's path.
	project := strings.TrimSpace(planGardenerProject)
	if project == "" {
		project = inheritProjectFromBead(plan)
	}

	res, err := plangardener.Bind(ctx, daemon, plan, plangardener.BindSpec{
		Project:         project,
		Operator:        operatedBy,
		CreatedBy:       actor,
		Cron:            planGardenerCron,
		Timed:           planGardenerTimed,
		FallbackCron:    planGardenerFallback,
		Timezone:        planGardenerTZ,
		MaxRuns:         planGardenerMaxRuns,
		LoopWallClock:   planGardenerLoopWall,
		Title:           planGardenerTitle,
		SlackChannel:    planGardenerSlackCh,
		Prompt:          planGardenerPromptOvr,
		ExecutionTarget: planGardenerExecTarget,
	})
	if err != nil {
		return err
	}

	if res.Adopted {
		printPlanGardenerAdopted(planID, res)
		return nil
	}

	if jsonOutput {
		printJSON(map[string]any{
			"id":         res.ID,
			"plan":       planID,
			"spawn_role": res.SpawnRole,
			"max_runs":   res.MaxRuns,
			"mode":       string(res.Mode),
			"cron":       res.Cron,
			"timezone":   res.Timezone,
			"project":    res.Project,
		})
		return nil
	}
	fmt.Printf("Bound plan gardener %s to %s\n", res.ID, planID)
	fmt.Printf("  spawn role: %s (iterations resolve as the plan's bound agent)\n", res.SpawnRole)
	if res.Mode == gardenermode.ModeTimed {
		fmt.Printf("  mode:       timed\n")
		fmt.Printf("  cron:       %s (%s)\n", res.Cron, res.Timezone)
	} else {
		fmt.Printf("  mode:       event (wakes on plan activity)\n")
		fmt.Printf("  watchdog:   %s (%s)\n", res.Cron, res.Timezone)
	}
	fmt.Printf("  max_runs:   %d\n", res.MaxRuns)
	if !res.Enabled {
		fmt.Printf("  enabled:    false (born paused behind the approve→build gate — 'Approve & build' on /plans enables it)\n")
	}
	if res.Project != "" {
		fmt.Printf("  project:    %s\n", res.Project)
	}
	return nil
}

// printPlanGardenerAdopted reports a plan that already has a bound gardener,
// noting a spawn role that will not resolve as the plan's agent.
func printPlanGardenerAdopted(planID string, res *plangardener.BindResult) {
	spawnRole := res.SpawnRole
	if jsonOutput {
		printJSON(map[string]any{
			"id":         res.ID,
			"plan":       planID,
			"already":    true,
			"spawn_role": spawnRole,
		})
		return
	}
	fmt.Printf("plan %s already has a bound gardener: %s (%s)\n", planID, res.ID, res.Existing.Title)
	switch spawnRole {
	case planGardenerSpawnRole:
		// Already on the unified role — nothing to note.
	case "plan-assistant":
		// Legacy alias: the viewer resolver's plan-assistant dual-read was
		// removed (decision ka-0000007411), so iterations of a trigger still
		// on this stale spawn role no longer resolve as the plan's agent —
		// update the role field to finish the ka-0000000081 migration.
		fmt.Printf("  note: its spawn role is the removed %q alias — iterations no longer resolve; update the role field to %q\n",
			spawnRole, planGardenerSpawnRole)
	default:
		fmt.Printf("  note: its spawn role is %q, not %q — iterations will not resolve as the plan's agent until the role field is updated\n",
			spawnRole, planGardenerSpawnRole)
	}
}
