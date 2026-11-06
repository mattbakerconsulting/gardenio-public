package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"gardenio/controller/internal/bridge"

	"github.com/spf13/cobra"
)

// jira_backlog_pick.go — `gb jira backlog-pick`, the BACKLOG-MODE ticket picker
// (task ka-0000007742, plan ka-0000000093 / narrative ka-0000005434).
//
// This is the operator/gardener-facing surface over the pure picker in
// internal/bridge/jira_backlog_pick.go. The FSM backlog-drain gardener
// (ka-0000004701) runs it once per fire on project=monorepo — the project that
// carries the direct JIRA read creds (advice ka-0000005529) — to answer "which
// ONE ticket in this released wave do I triage next?".
//
// It is the INVERSE of the live trigger (`created >= -2d`): it walks the explicit
// FSM keys named in a released wave step, skips anything already carrying the
// gardenio-triaged sentinel in Jira (the outbox source of truth, read live — not a
// bead scan), and returns the oldest un-triaged key. It only ever ISSUES READS.

var jiraBacklogPickCmd = &cobra.Command{
	Use:   "backlog-pick",
	Short: "Pick the next un-triaged FSM ticket from a released backlog wave",
	Long: `Backlog-mode ticket picker (task ka-0000007742, plan ka-0000000093).

The INVERSE of the live fsm-triage trigger (project = FSM AND created >= -2d):
instead of a rolling created-window, it walks the EXPLICIT FSM keys named in a
released wave step, skips anything already triaged, and returns the oldest
un-triaged ticket to work next.

The wave's keys come from one of (in precedence order):
  --keys FSM-360,FSM-361,...   an explicit comma/space-separated list
  --wave-bead <bead-id>        a wave-step bead — its title + description are
                               parsed for FSM keys (the wave titles name them,
                               e.g. "Wave B — crf-display (9): FSM-360,361,...")

Dedup is on the gardenio-triaged Jira LABEL, read LIVE from Jira — the GFO
outbox sentinel is the source of truth, not a bead scan. Re-running a partial
wave is safe (upsert is idempotent); the skip only avoids paying twice.

Selection: oldest-first within the wave (the first ticket does the deep
root-cause work; the rest confirm-or-differentiate against it), ties broken by
numeric key so a re-run is deterministic.

Requires direct JIRA read creds (JIRA_BASE_URL / JIRA_EMAIL / JIRA_API_TOKEN) —
present on project=monorepo pods, where the backlog-drain gardener runs it.
This command only READS Jira; it never writes.

Output: a one-line human summary (or --json for the full partition:
pick / untriaged / skipped / missing). Exit status is 0 whether or not a pick
exists; an empty pick means the wave is fully drained.`,
	Args: cobra.NoArgs,
	RunE: runJiraBacklogPick,
}

func runJiraBacklogPick(cmd *cobra.Command, _ []string) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), 60*time.Second)
	defer cancel()

	keysFlag, _ := cmd.Flags().GetString("keys")
	waveBead, _ := cmd.Flags().GetString("wave-bead")
	waveFlag, _ := cmd.Flags().GetString("wave")

	// Resolve the wave's named keys and (best-effort) wave label. Explicit
	// --keys wins; otherwise read them from the wave-step bead.
	var namedKeys []string
	wave := strings.TrimSpace(waveFlag)
	switch {
	case strings.TrimSpace(keysFlag) != "":
		namedKeys = bridge.ParseWaveKeys(keysFlag)
		if len(namedKeys) == 0 {
			return fmt.Errorf("--keys %q contained no FSM keys", keysFlag)
		}
	case strings.TrimSpace(waveBead) != "":
		if daemon == nil {
			return fmt.Errorf("no beads daemon configured; cannot read wave bead %s", waveBead)
		}
		bead, err := daemon.GetBead(ctx, strings.TrimSpace(waveBead))
		if err != nil {
			return fmt.Errorf("reading wave bead %s: %w", waveBead, err)
		}
		// Parse BOTH title and description — the title carries the canonical key
		// list and the description repeats it; ParseWaveKeys de-dups the union.
		namedKeys = bridge.ParseWaveKeys(bead.Title + "\n" + bead.Description)
		if len(namedKeys) == 0 {
			return fmt.Errorf("wave bead %s (%q) names no FSM keys", waveBead, bead.Title)
		}
		if wave == "" {
			wave = bridge.ParseWaveLabel(bead.Title)
		}
	default:
		return fmt.Errorf("provide the wave's keys via --keys or --wave-bead")
	}

	// Build a READ-ONLY live JIRA client from the direct creds. These are present
	// on project=monorepo pods (advice ka-0000005529); a project without them
	// legitimately cannot run backlog-pick.
	baseURL := os.Getenv("JIRA_BASE_URL")
	email := os.Getenv("JIRA_EMAIL")
	apiToken := os.Getenv("JIRA_API_TOKEN")
	if baseURL == "" || email == "" || apiToken == "" {
		return fmt.Errorf("backlog-pick needs direct JIRA creds: set JIRA_BASE_URL, JIRA_EMAIL, and JIRA_API_TOKEN " +
			"(present on project=monorepo pods — the backlog-drain gardener runs there)")
	}
	jira := bridge.NewJiraClient(bridge.JiraClientConfig{
		BaseURL:  baseURL,
		Email:    email,
		APIToken: apiToken,
		ReadOnly: true, // belt-and-suspenders: the picker must never mutate Jira
	})

	result, err := bridge.BacklogPick(ctx, jira, wave, namedKeys)
	if err != nil {
		return err
	}

	if jsonOutput {
		printJSON(result)
		return nil
	}
	printBacklogPick(result)
	return nil
}

// printBacklogPick renders the one-line human summary the gardener logs each
// fire, plus the supporting partition. Mirrors the "wave <X> | pick FSM-XXX |
// reason" one-liner the gardener prompt asks for.
func printBacklogPick(r bridge.BacklogPickResult) {
	wave := r.Wave
	if wave == "" {
		wave = "(unlabeled)"
	}
	if r.Pick == "" {
		fmt.Printf("wave %s | pick (none) | wave fully drained — %d named, %d already triaged\n",
			wave, len(r.Skipped)+len(r.Missing), len(r.Skipped))
	} else {
		fmt.Printf("wave %s | pick %s | oldest un-triaged of %d remaining (%d already triaged, %d missing)\n",
			wave, r.Pick, len(r.Untriaged), len(r.Skipped), len(r.Missing))
	}
	if len(r.Untriaged) > 0 {
		fmt.Printf("  untriaged (oldest-first): %s\n", strings.Join(r.Untriaged, ", "))
	}
	if len(r.Skipped) > 0 {
		fmt.Printf("  skipped (gardenio-triaged): %s\n", strings.Join(r.Skipped, ", "))
	}
	if len(r.Missing) > 0 {
		fmt.Printf("  MISSING (no Jira issue — check wave title): %s\n", strings.Join(r.Missing, ", "))
	}
}

func init() {
	jiraBacklogPickCmd.Flags().String("keys", "", "explicit comma/space-separated FSM keys (e.g. FSM-360,FSM-361); overrides --wave-bead")
	jiraBacklogPickCmd.Flags().String("wave-bead", "", "wave-step bead id whose title+description name the wave's FSM keys")
	jiraBacklogPickCmd.Flags().String("wave", "", "wave label for the output line (auto-derived from --wave-bead title when omitted)")

	jiraCmd.AddCommand(jiraBacklogPickCmd)
}
