// Package main — `gb gardener mode`: inspect or flip a gardener's firing mode
// (timed ⇄ event). The heavy lifting (ordered writes, the double-fire guard,
// the audit comment) lives in internal/gardenermode; this command is a thin
// CLI wrapper that resolves the gardener trigger bead, attributes the operator,
// and renders the result. Spec:
// docs/superpowers/specs/2026-07-13-gardener-mode-flip-design.md (§T9).
package main

import (
	"context"
	"fmt"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/gardenermode"
	"gardenio/controller/internal/scheduler"

	"github.com/spf13/cobra"
)

var (
	gardenerModeEvent          bool
	gardenerModeTimed          bool
	gardenerModeFallbackCron   string
	gardenerModeNoFallbackCron bool
)

var gardenerModeCmd = &cobra.Command{
	Use:   "mode <epic-or-trigger-id>",
	Short: "Inspect or flip a gardener's firing mode (timed ⇄ event)",
	Long: `Inspect or flip the firing mode of the gardener attached to <epic-or-trigger-id>.

A gardener fires in one of two modes:

  timed  — a cron schedule (label trigger:schedule) fires the gardener on the
           clock; the effective cadence is the cron field.
  event  — subtree bead events (label trigger:multi + a bead_event triggers[]
           entry) fire the gardener when anything inside its epic changes, with
           a slow cron watchdog as a fallback floor. The effective cadence is
           that fallback cron.

With no --event/--timed flag the command prints the current mode and effective
cadence and exits 0. --event or --timed flips to that mode (idempotent: a no-op
when already there, except as below); --fallback-cron overrides the event-mode watchdog cron
(default: a deterministic staggered 6h cron seeded from the epic id — but a PLAN
gardener defaults to NO watchdog). --no-fallback-cron authors the event-mode
gardener with no watchdog at all (purely event-driven, no clock floor); it is
mutually exclusive with --fallback-cron. Passing both --event and --timed is a
usage error.

A plan gardener bound by 'gb plan gardener' or the /plans cockpit is already
born in event mode, with a staggered daily watchdog; this command is for
flipping an existing gardener.

--event on a gardener already in event mode refreshes its event entries to the
current shape (today: plan-activity, then steward-nudge, which wakes it on its
land steward's nudge comment on the plan). It keeps the watchdog cron unless
--fallback-cron or --no-fallback-cron names another, and writes nothing when the
gardener is already current.`,
	Args: cobra.ExactArgs(1),
	RunE: runGardenerMode,
}

func runGardenerMode(cmd *cobra.Command, args []string) error {
	beadID, err := gardenerModeArgs(args)
	if err != nil {
		return err
	}
	ctx := cmd.Context()

	sched, err := resolveGardenerSchedule(ctx, daemon, beadID)
	if err != nil {
		return err
	}

	// Inspect-only: no flag → report the current mode + effective cadence.
	if !gardenerModeEvent && !gardenerModeTimed {
		return printGardenerMode(cmd, sched)
	}

	// Flip path: attribute the operator for the audit comment.
	operator, err := resolveGardenerModeOperator(ctx)
	if err != nil {
		return err
	}
	target := gardenermode.ModeEvent
	if gardenerModeTimed {
		target = gardenermode.ModeTimed
	}

	opts := gardenermode.Opts{
		Operator:       operator,
		FallbackCron:   strings.TrimSpace(gardenerModeFallbackCron),
		NoFallbackCron: gardenerModeNoFallbackCron,
	}

	// A real flip rewrites cron and triggers, which are spawn-seal digest keys:
	// refuse one that would silently void an owner's seal (gd-c4Xz4i95F7). So
	// does an event-mode refresh that changes triggers. Set is a no-op when the
	// gardener is already in the target mode and current, so that path writes
	// nothing and needs no guard.
	sealed := false
	if gardenerModeWrites(sched, target, opts) {
		sealed, err = guardSealedGardenerEdit(ctx, daemon, sched.ID, gardenerModeRetry(sched.ID, target))
		if err != nil {
			return err
		}
	}

	res, err := gardenermode.Set(ctx, daemon, sched, target, opts)
	if err != nil {
		return err
	}
	if sealed && res.Changed {
		if err := checkGardenerSealKept(ctx, daemon, sched.ID); err != nil {
			return err
		}
	}

	printGardenerModeResult(cmd, sched, res)
	return nil
}

// gardenerModeArgs is the bead id of `gb gardener mode`, once its flags are
// checked against each other.
func gardenerModeArgs(args []string) (string, error) {
	beadID := strings.TrimSpace(args[0])
	if beadID == "" {
		return "", fmt.Errorf("bead id is required")
	}
	if gardenerModeEvent && gardenerModeTimed {
		return "", fmt.Errorf("--event and --timed are mutually exclusive")
	}
	if gardenerModeNoFallbackCron && strings.TrimSpace(gardenerModeFallbackCron) != "" {
		return "", fmt.Errorf("--no-fallback-cron and --fallback-cron are mutually exclusive")
	}
	if gardenerModeNoFallbackCron && gardenerModeTimed {
		return "", fmt.Errorf("--no-fallback-cron applies only to event mode, not --timed")
	}
	return beadID, nil
}

// printGardenerMode reports a gardener's current mode and effective cadence
// (no --event/--timed flag).
func printGardenerMode(cmd *cobra.Command, sched *beadsapi.BeadDetail) error {
	cur, err := gardenermode.Detect(sched)
	if err != nil {
		return err
	}
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

// printGardenerModeResult reports what a flip (or refresh) did.
func printGardenerModeResult(cmd *cobra.Command, sched *beadsapi.BeadDetail, res gardenermode.Result) {
	// Set returns the new effective cadence in res.Cron on a real flip; on the
	// idempotent no-op it leaves it empty, so recover it from the bead.
	cadence := res.Cron
	if !res.Changed {
		cadence = gardenerEffectiveCadence(sched, res.Mode)
	}

	if jsonOutput {
		printJSON(map[string]any{
			"id":        sched.ID,
			"mode":      string(res.Mode),
			"cron":      cadence,
			"changed":   res.Changed,
			"refreshed": res.Refreshed,
		})
		return
	}
	out := cmd.OutOrStdout()
	if !res.Changed {
		fmt.Fprintf(out, "gardener %s already in %s mode (cron %s)\n", sched.ID, res.Mode, defaultIfEmpty(cadence, "—"))
		return
	}
	if res.Refreshed {
		fmt.Fprintf(out, "gardener %s already in %s mode; triggers refreshed (cron %s)\n", sched.ID, res.Mode, defaultIfEmpty(cadence, "—"))
		return
	}
	fmt.Fprintf(out, "gardener %s flipped to %s mode (cron %s)\n", sched.ID, res.Mode, defaultIfEmpty(cadence, "—"))
}

// gardenerModeWrites reports whether Set would write to the gardener: a flip
// to another mode, or an event-mode refresh that changes triggers[]. An
// unreadable mode or triggers[] counts as a write, so the seal guard runs.
func gardenerModeWrites(sched *beadsapi.BeadDetail, target gardenermode.Mode, opts gardenermode.Opts) bool {
	cur, err := gardenermode.Detect(sched)
	if err != nil || cur != target {
		return true
	}
	if target != gardenermode.ModeEvent {
		return false
	}
	_, changed, err := gardenermode.RefreshEventTriggers(sched, opts)
	return err != nil || changed
}

// gardenerModeRetry renders the `gb gardener mode` command this run is making,
// for the escalation recipe the spawn-seal guard prints.
func gardenerModeRetry(id string, target gardenermode.Mode) string {
	cmd := fmt.Sprintf("gb gardener mode %s --%s", id, target)
	if fc := strings.TrimSpace(gardenerModeFallbackCron); fc != "" {
		cmd += fmt.Sprintf(" --fallback-cron %q", fc)
	}
	if gardenerModeNoFallbackCron {
		cmd += " --no-fallback-cron"
	}
	return cmd
}

// gardenerEffectiveCadence returns the cron expression that actually governs a
// gardener's firing tempo: the flat cron field in timed mode, or the fallback
// watchdog cron from the triggers[] array in event mode (an event gardener
// wakes on subtree activity, but the watchdog cron is the guaranteed floor).
func gardenerEffectiveCadence(b *beadsapi.BeadDetail, mode gardenermode.Mode) string {
	if mode == gardenermode.ModeTimed {
		return b.Fields["cron"]
	}
	entries, err := scheduler.ParseTriggers(b.Fields["triggers"])
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.Kind == scheduler.TriggerKindCron {
			return e.Cron
		}
	}
	return ""
}

// resolveGardenerModeOperator resolves the human operator to attribute the flip
// audit comment to, using the same identity chain the rest of gb gardener uses:
// resolveGardenerOwner maps $BEADS_OPERATED_BY (--operated-by) to a slack id;
// if that resolves, use it, else fall back to the raw operated-by alias, else
// error (gardenermode.Set requires a non-empty operator).
func resolveGardenerModeOperator(ctx context.Context) (string, error) {
	if slackID, _ := resolveGardenerOwner(ctx, operatedBy); slackID != "" {
		return slackID, nil
	}
	if op := strings.TrimSpace(operatedBy); op != "" {
		return op, nil
	}
	return "", fmt.Errorf("no operator identity: set --operated-by slack:U…/oidc:… or $BEADS_OPERATED_BY (the mode-flip audit comment must be attributed)")
}
