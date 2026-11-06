// Package main — `gb gardener cadence`: set or clear a gardener's firing cadence
// WITHOUT flipping its mode. The mode-aware write lives in
// internal/gardenermode.SetCadence (flat `cron` field in timed mode; the
// fallback-watchdog triggers[] entry in event mode, with the bead's
// `conditions` gate carried on as a per-entry preflight); this command is a
// thin CLI wrapper that resolves the gardener trigger bead, attributes the
// operator, and renders the result — the same shape as `gb gardener mode`.
//
// Why this exists (ka-0000005992, follow-up to ka-0000008237): SetCadence
// shipped with exactly ONE caller, the operator-only viewer route
// POST /plans/{id}/gardener/cadence. That left two holes:
//
//   - `gb gardener mode --event --no-fallback-cron` is a NO-OP on a gardener that
//     is ALREADY in event mode — gardenermode.Set early-returns Changed:false
//     when cur == mode, before NoFallbackCron is consulted. So the flag cannot
//     remediate a gardener flipped to event BEFORE the no-default-cron change
//     landed; those still carry the legacy unconditional 6h watchdog.
//   - The cockpit route is operatorOnly (OIDC email), so an agent could not
//     remediate one at all — and a gardener not bound to a plan has no UI path
//     whatsoever.
//
// `gb gardener cadence <id> --none` is the missing verb for both.
package main

import (
	"fmt"
	"strings"

	"gardenio/controller/internal/gardenermode"
	"gardenio/controller/internal/scheduler"

	"github.com/spf13/cobra"
)

var (
	gardenerCadenceCron string
	gardenerCadenceNone bool
)

var gardenerCadenceCmd = &cobra.Command{
	Use:   "cadence <epic-or-trigger-id>",
	Short: "Set or clear a gardener's firing cadence (mode-aware; does not flip mode)",
	Long: `Set or clear the firing cadence of the gardener attached to <epic-or-trigger-id>.

What the cadence MEANS depends on the gardener's current firing mode:

  timed  — the flat cron schedule that fires the gardener. It cannot be cleared:
           a timed gardener with no cron never fires (use ` + "`gb gardener mode --event`" + `
           to make it purely activity-driven instead).
  event  — the fallback-watchdog cron: a slow clock floor under the subtree
           bead_event trigger, bounding staleness if the event stream drops.
           It is OPTIONAL. --none removes it entirely, leaving the gardener
           purely event-driven (the default for PLAN gardeners, ka-0000008237).

With no flag the command prints the current mode + effective cadence and exits 0.
--cron sets it; --none clears it (event mode only). The two are mutually exclusive.

Use --none to remediate a gardener that was flipped to event mode BEFORE plan
gardeners stopped getting a default watchdog: ` + "`gb gardener mode --no-fallback-cron`" + `
is a no-op on a gardener already in event mode, so it cannot strip a legacy cron.

An event-mode gardener's watchdog carries the bead's ` + "`conditions`" + ` gate as a
per-entry preflight, so a pre-flight-gated gardener stays gated.

On a gardener sealed to an owner's approval (spawn_lineage), the cadence is part of
what the seal covers. Only a gardenio-admin OIDC session (gb login) keeps the seal
through the change; from any other credential the command refuses and prints the
escalation to file instead, because an unsealed privileged gardener is refused at
every fire until it hard-stops. ` + "`gb gardener mode`" + ` is guarded the same way.`,
	Args: cobra.ExactArgs(1),
	RunE: runGardenerCadence,
}

func runGardenerCadence(cmd *cobra.Command, args []string) error {
	beadID := strings.TrimSpace(args[0])
	if beadID == "" {
		return fmt.Errorf("bead id is required")
	}
	cron := strings.TrimSpace(gardenerCadenceCron)
	if gardenerCadenceNone && cron != "" {
		return fmt.Errorf("--none and --cron are mutually exclusive")
	}
	ctx := cmd.Context()

	sched, err := resolveGardenerSchedule(ctx, daemon, beadID)
	if err != nil {
		return err
	}
	cur, err := gardenermode.Detect(sched)
	if err != nil {
		return err
	}

	// Inspect-only: no flag → report the current mode + effective cadence, the
	// same projection `gb gardener mode` prints with no flag.
	if !gardenerCadenceNone && cron == "" {
		cadence := gardenerEffectiveCadence(sched, cur)
		if jsonOutput {
			printJSON(map[string]any{
				"id":   sched.ID,
				"mode": string(cur),
				"cron": cadence,
			})
			return nil
		}
		fmt.Fprintf(cmd.OutOrStdout(), "mode: %s  cron: %s\n", cur, defaultIfEmpty(cadence, "—"))
		return nil
	}

	// Validate BEFORE any write, with the SAME parser the trigger engine uses to
	// evaluate the expression at fire time (scheduler.ParseCron, via
	// scheduler/triggers.go for cron entries and trigger_source_schedule.go for
	// the flat field). A typo must fail here rather than persist dead config
	// that silently never fires.
	if cron != "" {
		if _, perr := scheduler.ParseCron(cron); perr != nil {
			return fmt.Errorf("invalid cron expression %q: %w", cron, perr)
		}
	}

	operator, err := resolveGardenerModeOperator(ctx)
	if err != nil {
		return err
	}

	// cron and triggers are spawn-seal digest keys: refuse an edit that would
	// silently void an owner's seal (gd-c4Xz4i95F7).
	retry := fmt.Sprintf("gb gardener cadence %s --none", sched.ID)
	if cron != "" {
		retry = fmt.Sprintf("gb gardener cadence %s --cron %q", sched.ID, cron)
	}
	sealed, err := guardSealedGardenerEdit(ctx, daemon, sched.ID, retry)
	if err != nil {
		return err
	}

	res, err := gardenermode.SetCadence(ctx, daemon, sched, cron, sched.Fields["timezone"], gardenermode.Opts{
		Operator: operator,
	})
	if err != nil {
		return err
	}
	if sealed && res.Changed {
		if err := checkGardenerSealKept(ctx, daemon, sched.ID); err != nil {
			return err
		}
	}

	if jsonOutput {
		printJSON(map[string]any{
			"id":      sched.ID,
			"mode":    string(res.Mode),
			"cron":    res.Cron,
			"changed": res.Changed,
		})
		return nil
	}
	out := cmd.OutOrStdout()
	if res.Cron == "" {
		fmt.Fprintf(out, "gardener %s cadence cleared — purely event-driven, no clock floor\n", sched.ID)
		return nil
	}
	fmt.Fprintf(out, "gardener %s cadence set to %s (%s mode)\n", sched.ID, res.Cron, res.Mode)
	return nil
}
