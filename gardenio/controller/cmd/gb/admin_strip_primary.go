package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

// ── gb admin strip-primary ──────────────────────────────────────────
//
// Phase 6 (LAST, irreversible) of epic ka-0000008240 — abolish the "primary
// account" concept from inference billing. This is the ONLY phase that mutates
// persistent bead state, so it runs only after the Phase 1–5 code has soaked
// across at least one release and both binaries (Go controller + Rust coopmux)
// are inert to the field.
//
// Two independent strips, both idempotent and --dry-run-able:
//
//  1. Remove the `primary="true"` marker FIELD from account beads. The field is
//     deleted (RFC 7386 null-delete via UpdateBeadFieldsTyped), so it becomes
//     ABSENT — not empty-string or the literal "null". After Phase 4,
//     projectPrimary/buildProjectAccountOverride already skip a marker on a
//     non-active account and spread already wins over the marker on a healthy
//     pool, so removing it changes nothing about resolution; it only stops the
//     stale steering signal from ever mattering again.
//
//  2. Drop `primary_account` from every config:auth bead's description JSON.
//     Phase 2 self-pins primary_account == account and both languages treat a
//     MISSING primary_account as "no switchback" (Rust projects_failed_over_from
//     is inert; Go no longer acts on it), so dropping the key is safe. The
//     `account` field and any other observability hints are preserved verbatim;
//     only `primary_account` is stripped.
//
// SNAPSHOT FIRST: unless --no-snapshot is passed, the command prints a JSON
// snapshot of every mutation it is ABOUT to make (bead id + the field/value or
// description being removed) BEFORE mutating, so an operator can capture it and
// restore by hand if needed. The restore steps are documented in the snapshot
// header. Capture the snapshot to a file with:
//
//     gb admin strip-primary --dry-run > phase6-snapshot.json   # plan only
//     gb admin strip-primary --snapshot-out phase6-snapshot.json # snapshot + apply
//
// Restore (should it ever be needed — Phases 1–5 make the field inert, so this
// is belt-and-suspenders):
//   - account marker:  kd update <account-bead-id> -f primary=true
//   - config:auth desc: kd update <config-bead-id> -d '<the saved description>'
//
// Idempotent: an account with no `primary` field and a config:auth whose
// description already lacks `primary_account` are both no-ops, so re-running is
// safe and a second run reports 0 mutations.

var adminStripPrimaryCmd = &cobra.Command{
	Use:   "strip-primary",
	Short: "Phase 6: strip primary=true markers + primary_account from bead data",
	Long: `One-shot bead-data migration (Phase 6 of epic ka-0000008240, the LAST
and only irreversible phase). Removes the legacy "primary account" steering data
now that both the Go controller and Rust coopmux are inert to it:

  1. Delete the primary="true" marker FIELD from account beads (RFC 7386
     null-delete, so the field becomes absent — not "" or "null").
  2. Drop primary_account from every config:auth bead's description JSON,
     preserving account and all other fields verbatim.

Idempotent — accounts without a primary field and config:auth descriptions
already lacking primary_account are skipped, so a re-run reports 0 mutations.

Snapshot-first: by default a JSON snapshot of every planned mutation is printed
BEFORE any write (or written to --snapshot-out), with documented restore steps,
so the change is reversible by hand. Use --dry-run to print the plan and the
snapshot without mutating anything.`,
	RunE: runAdminStripPrimary,
}

func init() {
	adminCmd.AddCommand(adminStripPrimaryCmd)
	adminStripPrimaryCmd.Flags().Bool("dry-run", false, "preview actions + snapshot without mutating beads")
	adminStripPrimaryCmd.Flags().Bool("no-snapshot", false, "skip printing the pre-mutation snapshot (not recommended)")
	adminStripPrimaryCmd.Flags().String("snapshot-out", "", "write the pre-mutation snapshot JSON to this file instead of stdout")
}

func runAdminStripPrimary(cmd *cobra.Command, _ []string) error {
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	noSnapshot, _ := cmd.Flags().GetBool("no-snapshot")
	snapshotOut, _ := cmd.Flags().GetString("snapshot-out")
	return executeStripPrimary(cmd.Context(), daemonAdapter{daemon}, os.Stdout, stripPrimaryOpts{
		dryRun:      dryRun,
		noSnapshot:  noSnapshot,
		snapshotOut: snapshotOut,
	})
}

// stripPrimaryClient is the minimal beads-API subset the strip needs — kept
// narrow so tests mock it without a live daemon.
type stripPrimaryClient interface {
	ListAccountBeadsRaw(ctx context.Context) ([]*beadsapi.BeadDetail, error)
	ListAuthConfigBeadsRaw(ctx context.Context) ([]*beadsapi.BeadDetail, error)
	// UnsetField deletes fieldKey from the bead entirely (RFC 7386 null-delete).
	UnsetField(ctx context.Context, beadID, fieldKey string) error
	UpdateBeadDescription(ctx context.Context, beadID, description string) error
}

// daemonAdapter bridges *beadsapi.Client to stripPrimaryClient.
type daemonAdapter struct{ c *beadsapi.Client }

func (d daemonAdapter) ListAuthConfigBeadsRaw(ctx context.Context) ([]*beadsapi.BeadDetail, error) {
	res, err := d.c.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"config"},
		Statuses: []string{"open", "in_progress"},
	})
	if err != nil {
		return nil, err
	}
	return res.Beads, nil
}

func (d daemonAdapter) ListAccountBeadsRaw(ctx context.Context) ([]*beadsapi.BeadDetail, error) {
	res, err := d.c.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"account"},
		Statuses: []string{"open", "in_progress"},
	})
	if err != nil {
		return nil, err
	}
	return res.Beads, nil
}

func (d daemonAdapter) UnsetField(ctx context.Context, beadID, fieldKey string) error {
	// nil value => RFC 7386 delete-on-null (kbeads beads.go:1044). The field
	// becomes absent, distinct from empty-string or a stored "null".
	return d.c.UpdateBeadFieldsTyped(ctx, beadID, map[string]any{fieldKey: nil})
}

func (d daemonAdapter) UpdateBeadDescription(ctx context.Context, beadID, description string) error {
	return d.c.UpdateBeadDescription(ctx, beadID, description)
}

// stripPrimaryOpts groups the command-line switches.
type stripPrimaryOpts struct {
	dryRun      bool
	noSnapshot  bool
	snapshotOut string
}

// accountStrip is one planned account-marker removal.
type accountStrip struct {
	BeadID string `json:"bead_id"`
	Title  string `json:"title"`
	// PrevValue is the current stored value of the primary field (for the
	// snapshot / restore step). Always "true" for a bead in the plan.
	PrevValue string `json:"prev_primary"`
}

// configStrip is one planned config:auth description rewrite.
type configStrip struct {
	BeadID string `json:"bead_id"`
	// PrevDescription is the exact description before the rewrite — the restore
	// payload for `kd update <bead-id> -d '<PrevDescription>'`.
	PrevDescription string `json:"prev_description"`
	// NewDescription is what the migration will write (primary_account dropped).
	NewDescription string `json:"new_description"`
	// PrevPrimaryAccount is the primary_account value being removed (informational).
	PrevPrimaryAccount string `json:"prev_primary_account"`
}

// stripPrimaryPlan is the derived, deterministic set of mutations. Computed by
// planStripPrimary with no I/O so it is unit-testable in isolation.
type stripPrimaryPlan struct {
	Accounts []accountStrip `json:"accounts"`
	Configs  []configStrip  `json:"configs"`
}

// planStripPrimary is the pure core: given the current account and config:auth
// beads, it returns exactly the mutations needed to strip the primary marker
// and primary_account. Only beads that actually carry the legacy data appear in
// the plan (idempotent — a clean bead is omitted). Results are sorted by bead
// id for stable output.
func planStripPrimary(accounts, configs []*beadsapi.BeadDetail) stripPrimaryPlan {
	var plan stripPrimaryPlan

	for _, a := range accounts {
		if a == nil {
			continue
		}
		// Any non-absent primary field is stripped. In practice the only
		// meaningful value is "true", but an account carrying a stale "false"
		// (or any other value) is also legacy steering data we want gone, so
		// we key on presence rather than value == "true".
		v, ok := a.Fields["primary"]
		if !ok || v == "" {
			continue
		}
		plan.Accounts = append(plan.Accounts, accountStrip{
			BeadID:    a.ID,
			Title:     a.Title,
			PrevValue: v,
		})
	}

	for _, c := range configs {
		if c == nil || c.Type != "config" || c.Title != "auth" {
			continue
		}
		newDesc, prevPrimary, changed := stripPrimaryAccountFromDesc(c.Description)
		if !changed {
			continue
		}
		plan.Configs = append(plan.Configs, configStrip{
			BeadID:             c.ID,
			PrevDescription:    c.Description,
			NewDescription:     newDesc,
			PrevPrimaryAccount: prevPrimary,
		})
	}

	sort.Slice(plan.Accounts, func(i, j int) bool { return plan.Accounts[i].BeadID < plan.Accounts[j].BeadID })
	sort.Slice(plan.Configs, func(i, j int) bool { return plan.Configs[i].BeadID < plan.Configs[j].BeadID })
	return plan
}

// stripPrimaryAccountFromDesc removes the primary_account key from a config:auth
// description JSON, preserving every other key and their values. It returns the
// rewritten description, the removed primary_account value, and whether a change
// was made. A description that is empty, malformed, or already lacks
// primary_account is returned unchanged with changed=false (idempotent).
//
// The rewrite is done on a generic map so unknown/future observability keys are
// preserved verbatim — we do NOT round-trip through authConfigDescription,
// which would silently drop any field it doesn't model.
func stripPrimaryAccountFromDesc(description string) (newDesc, prevPrimary string, changed bool) {
	trimmed := strings.TrimSpace(description)
	if trimmed == "" {
		return description, "", false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &m); err != nil {
		// Malformed / non-object description — leave it alone.
		return description, "", false
	}
	raw, ok := m["primary_account"]
	if !ok {
		return description, "", false
	}
	// Decode the removed value for the snapshot (best-effort — a non-string
	// primary_account is unexpected but still reported as its JSON text).
	if err := json.Unmarshal(raw, &prevPrimary); err != nil {
		prevPrimary = string(raw)
	}
	delete(m, "primary_account")
	encoded, err := json.Marshal(m)
	if err != nil {
		return description, "", false
	}
	return string(encoded), prevPrimary, true
}

// snapshotHeader documents how to restore the mutations captured below it.
const snapshotHeader = `Phase 6 (epic ka-0000008240) strip-primary snapshot.
Restore an account marker:   kd update <bead_id> -f primary=<prev_primary>
Restore a config:auth desc:  kd update <bead_id> -d '<prev_description>'
Phases 1-5 make both the Go controller and Rust coopmux inert to this data, so
a restore should never be operationally necessary; this snapshot exists so the
irreversible step is reversible by hand if a regression is discovered.`

// executeStripPrimary computes the plan, emits the pre-mutation snapshot, then
// applies the mutations (unless dryRun). Idempotent and safe to re-run.
func executeStripPrimary(ctx context.Context, client stripPrimaryClient, out io.Writer, opts stripPrimaryOpts) error {
	accounts, err := client.ListAccountBeadsRaw(ctx)
	if err != nil {
		return fmt.Errorf("listing account beads: %w", err)
	}
	configs, err := client.ListAuthConfigBeadsRaw(ctx)
	if err != nil {
		return fmt.Errorf("listing config beads: %w", err)
	}

	plan := planStripPrimary(accounts, configs)

	// Snapshot BEFORE any mutation (unless suppressed). Written to file when
	// --snapshot-out is set, else to the command output.
	if !opts.noSnapshot {
		if err := emitSnapshot(plan, out, opts.snapshotOut); err != nil {
			return err
		}
	}

	if opts.dryRun {
		fmt.Fprintln(out, "DRY RUN — no beads will be modified")
		fmt.Fprintln(out)
	}

	var (
		acctStripped int
		cfgStripped  int
		hardErrors   int
	)

	for _, a := range plan.Accounts {
		if opts.dryRun {
			fmt.Fprintf(out, "[plan] account %s (%q) → remove primary=%s\n", a.BeadID, a.Title, a.PrevValue)
			continue
		}
		if err := client.UnsetField(ctx, a.BeadID, "primary"); err != nil {
			fmt.Fprintf(out, "[error] account %s → remove primary: %v\n", a.BeadID, err)
			hardErrors++
			continue
		}
		fmt.Fprintf(out, "[strip] account %s (%q) → primary marker removed\n", a.BeadID, a.Title)
		acctStripped++
	}

	for _, c := range plan.Configs {
		if opts.dryRun {
			fmt.Fprintf(out, "[plan] config %s → drop primary_account=%q from description\n", c.BeadID, c.PrevPrimaryAccount)
			continue
		}
		if err := client.UpdateBeadDescription(ctx, c.BeadID, c.NewDescription); err != nil {
			fmt.Fprintf(out, "[error] config %s → drop primary_account: %v\n", c.BeadID, err)
			hardErrors++
			continue
		}
		fmt.Fprintf(out, "[strip] config %s → primary_account dropped\n", c.BeadID)
		cfgStripped++
	}

	fmt.Fprintln(out)
	if opts.dryRun {
		fmt.Fprintf(out, "Plan: %d account marker(s), %d config:auth description(s) to strip.\n",
			len(plan.Accounts), len(plan.Configs))
	} else {
		fmt.Fprintf(out, "Done: accounts_stripped=%d configs_stripped=%d errors=%d\n",
			acctStripped, cfgStripped, hardErrors)
	}

	if hardErrors > 0 {
		return fmt.Errorf("strip-primary completed with %d error(s)", hardErrors)
	}
	return nil
}

// emitSnapshot writes the pre-mutation snapshot as pretty JSON, either to a file
// (snapshotOut) or to out. When nothing is being stripped it still emits an
// empty-plan snapshot so an operator sees the migration found nothing to do.
func emitSnapshot(plan stripPrimaryPlan, out io.Writer, snapshotOut string) error {
	body, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling snapshot: %w", err)
	}
	if snapshotOut != "" {
		content := "// " + strings.ReplaceAll(snapshotHeader, "\n", "\n// ") + "\n" + string(body) + "\n"
		if err := os.WriteFile(snapshotOut, []byte(content), 0o600); err != nil {
			return fmt.Errorf("writing snapshot to %s: %w", snapshotOut, err)
		}
		fmt.Fprintf(out, "Snapshot written to %s (%d account(s), %d config(s)).\n\n",
			snapshotOut, len(plan.Accounts), len(plan.Configs))
		return nil
	}
	fmt.Fprintln(out, "── SNAPSHOT (capture this to restore by hand if needed) ──")
	fmt.Fprintln(out, snapshotHeader)
	fmt.Fprintln(out, string(body))
	fmt.Fprintln(out, "── END SNAPSHOT ──")
	fmt.Fprintln(out)
	return nil
}
