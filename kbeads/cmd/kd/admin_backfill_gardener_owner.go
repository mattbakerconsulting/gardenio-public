package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"text/tabwriter"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
)

// adminBackfillGardenerOwnerCmd is phase 1 of the captain -> gardener rename
// (migration plan gd-5O9QUlA8zr). Phase 0 (gd-Cizer9ykpz) made every write path
// dual-write the old and new marker and every read path prefer-new/fall-back-old;
// this command stamps the NEW marker onto historical beads that predate the
// dual-write code, so phase 2 can start without any bead depending on a lazy,
// incidental future write to gain its gardener marker.
//
// It copies admin_backfill_captain_owner.go's shape: a pure plan step, an
// idempotency guard that skips already-stamped markers, tab-delimited
// PHASE/BEAD_ID/FIELD/OLD/NEW/STATUS rows, and a non-zero exit on any ERROR row.
// Unlike captain-owner it is DRY-RUN BY DEFAULT: it writes only with an explicit
// --apply, so a bare invocation can never mutate the fleet. Despite the "owner" suffix (kept for parity
// with `captain-owner`), it does not touch ownership — it mirrors three markers:
//
//   - label  — role:captain          -> role:gardener
//   - epic   — captain-epic:<plan>   -> gardener-epic:<plan>  (one row per epic label)
//   - field  — `captain` JSON field  -> `gardener` JSON field (type=trigger only —
//     the only bead type kbeads registers the field on, config.go)
//
// A trigger bead whose captain and gardener fields are both set but decode to
// different JSON (CONFLICT) is never overwritten by default — the gardener value
// may be the newer one — and fails the saturation gate. --resync-conflicts opts
// in to overwriting gardener with the captain value.
//
// A bead carrying the new marker but not the old one (NEW_ONLY) is the reverse
// gap: old-code pods cannot see it. The backfill reports it but does not write
// it — phase 0 dual-writes both, so a NEW_ONLY row means something wrote the new
// name alone and needs a human look.
//
// Saturation gate: phase 2 must not start until a dry-run reports zero WOULD_UPDATE,
// zero NEW_ONLY and zero CONFLICT rows (zero beads missing or disagreeing on a name). The command prints a
// SATURATION line and --require-saturated turns a failing gate into a non-zero
// exit so the check is scriptable.
var adminBackfillGardenerOwnerCmd = &cobra.Command{
	Use:   "gardener-owner",
	Short: "Stamp role:gardener / gardener-epic: / gardener field onto beads that only carry the captain names",
	Long: `Phase 1 of the captain -> gardener rename (plan gd-5O9QUlA8zr).

Walks every bead and, for each of three markers, stamps the new gardener name
onto any bead that carries the old captain name but not the new one:

  label  role:captain          -> role:gardener
  epic   captain-epic:<plan>   -> gardener-epic:<plan>   (one row per epic label)
  field  captain (JSON field)  -> gardener (JSON field)  (type=trigger beads)

Already-stamped markers are skipped (idempotent): a re-run after an apply yields
an all-SKIP_IDEMPOTENT plan. A bead with the new marker but not the old one is
reported as NEW_ONLY and left untouched. A trigger bead whose captain and
gardener fields are both set but decode to different JSON (key order and
whitespace ignored) is reported as CONFLICT and left untouched.

DRY-RUN IS THE DEFAULT: with no flags the command prints the proposed
PHASE/BEAD_ID/FIELD/OLD/NEW/STATUS rows and writes nothing. Pass --apply to
stamp the markers. --resync-conflicts (with --apply) additionally overwrites a
CONFLICT bead's gardener field with its captain value.

The final SATURATION line is the phase-2 gate: PASS means zero beads are missing
either name and zero disagree. --require-saturated exits non-zero when the gate
fails (run it without --apply before phase 2). Exit is also non-zero on any
ERROR row.`,
	RunE: runAdminBackfillGardenerOwner,
}

func init() {
	adminBackfillGardenerOwnerCmd.Flags().Bool("apply", false, "write the proposed changes (default is a dry run that writes nothing)")
	adminBackfillGardenerOwnerCmd.Flags().Bool("dry-run", false, "show proposed changes without writing (the default; conflicts with --apply)")
	adminBackfillGardenerOwnerCmd.Flags().Bool("resync-conflicts", false, "with --apply, overwrite a CONFLICT bead's gardener field with its captain value")
	adminBackfillGardenerOwnerCmd.Flags().Int("limit", 0, "cap the number of beads scanned (0 = no cap)")
	adminBackfillGardenerOwnerCmd.Flags().Int("page-size", 200, "beads to fetch per page")
	adminBackfillGardenerOwnerCmd.Flags().Bool("require-saturated", false, "exit non-zero when the saturation gate fails (any bead missing either name or with conflicting values)")
	adminBackfillCmd.AddCommand(adminBackfillGardenerOwnerCmd)
}

const (
	gardenerRoleOld      = "role:captain"
	gardenerRoleNew      = "role:gardener"
	gardenerEpicOldLabel = "captain-epic:"
	gardenerEpicNewLabel = "gardener-epic:"
	gardenerFieldOld     = "captain"
	gardenerFieldNew     = "gardener"
	gardenerFieldType    = "trigger"
)

// gardenerStatus is the STATUS column value for one planned row.
type gardenerStatus string

const (
	gardenerWouldUpdate    gardenerStatus = "WOULD_UPDATE"
	gardenerUpdated        gardenerStatus = "UPDATED"
	gardenerSkipIdempotent gardenerStatus = "SKIP_IDEMPOTENT"
	gardenerNewOnly        gardenerStatus = "NEW_ONLY"
	gardenerConflict       gardenerStatus = "CONFLICT"
	gardenerError          gardenerStatus = "ERROR"
)

// gardenerRow is one planned marker change. Label and epic rows are applied via
// AddLabel; the field row is applied via UpdateBead's field merge.
type gardenerRow struct {
	Phase  string // "label", "epic" or "field"
	BeadID string
	Field  string // role / gardener-epic / gardener
	Old    string // old-name value, "(unset)" when absent
	New    string // new-name value, "(unset)" when absent
	Status gardenerStatus
	Note   string // diagnostic, not printed in the tab table

	// Apply payload (zero for non-WOULD_UPDATE rows).
	label      string          // label to AddLabel (label/epic rows)
	fieldPatch json.RawMessage // {"gardener": <captain value>} (field rows)
}

func runAdminBackfillGardenerOwner(cmd *cobra.Command, _ []string) error {
	apply, _ := cmd.Flags().GetBool("apply")
	explicitDryRun, _ := cmd.Flags().GetBool("dry-run")
	resync, _ := cmd.Flags().GetBool("resync-conflicts")
	limit, _ := cmd.Flags().GetInt("limit")
	pageSize, _ := cmd.Flags().GetInt("page-size")
	requireSaturated, _ := cmd.Flags().GetBool("require-saturated")
	if apply && explicitDryRun {
		return fmt.Errorf("--apply and --dry-run are mutually exclusive")
	}
	if resync && !apply {
		return fmt.Errorf("--resync-conflicts requires --apply")
	}
	dryRun := !apply
	if pageSize <= 0 {
		pageSize = 200
	}

	ctx := context.Background()

	// No filter: a bead with only the old marker is exactly what we are hunting,
	// and the epic label is a prefix (no server-side wildcard), so walk everything.
	beads, err := listAllBeads(ctx, &client.ListBeadsRequest{}, pageSize, limit)
	if err != nil {
		return fmt.Errorf("listing beads: %w", err)
	}

	var rows []gardenerRow
	for _, b := range beads {
		rows = append(rows, planGardenerMarkers(b, resync)...)
	}

	failed := 0
	if !dryRun {
		failed = applyGardenerRows(ctx, rows)
	}
	printGardenerRows(rows, dryRun)
	printGardenerSummary(rows, len(beads), dryRun, failed)

	saturated := gardenerSaturated(rows)
	if dryRun {
		fmt.Fprintln(os.Stderr, "\nDry-run only (default). Re-run with --apply to stamp the gardener markers.")
	}
	if n := countGardenerStatus(rows, gardenerError); n > 0 || failed > 0 {
		return fmt.Errorf("%d planning error(s), %d update(s) failed", n, failed)
	}
	if requireSaturated && !saturated {
		return fmt.Errorf("saturation gate FAILED: %d marker(s) missing the new name, %d missing the old name, %d conflicting",
			countGardenerStatus(rows, gardenerWouldUpdate), countGardenerStatus(rows, gardenerNewOnly), countGardenerStatus(rows, gardenerConflict))
	}
	return nil
}

// planGardenerMarkers plans the label, epic and field rows for one bead. Beads
// carrying none of the markers produce no rows. resync makes a CONFLICT field
// row writable (WOULD_UPDATE with the captain value).
func planGardenerMarkers(b *model.Bead, resync bool) []gardenerRow {
	var rows []gardenerRow

	hasOld := containsString(b.Labels, gardenerRoleOld)
	hasNew := containsString(b.Labels, gardenerRoleNew)
	if r, ok := planGardenerMarker("label", b.ID, "role", gardenerRoleOld, gardenerRoleNew, hasOld, hasNew); ok {
		r.label = gardenerRoleNew
		rows = append(rows, r)
	}

	// Epic labels: pair captain-epic:<id> with gardener-epic:<id> per id.
	oldEpics := labelSuffixes(b.Labels, gardenerEpicOldLabel)
	newEpics := labelSuffixes(b.Labels, gardenerEpicNewLabel)
	for _, id := range oldEpics {
		r, _ := planGardenerMarker("epic", b.ID, "gardener-epic",
			gardenerEpicOldLabel+id, gardenerEpicNewLabel+id, true, containsString(newEpics, id))
		r.label = gardenerEpicNewLabel + id
		rows = append(rows, r)
	}
	for _, id := range newEpics {
		if !containsString(oldEpics, id) {
			r, _ := planGardenerMarker("epic", b.ID, "gardener-epic",
				gardenerEpicOldLabel+id, gardenerEpicNewLabel+id, false, true)
			rows = append(rows, r)
		}
	}

	// Field: only type=trigger registers the gardener field (kbeads config.go).
	if b.Type == gardenerFieldType {
		fields := decodeFields(b.Fields)
		oldVal, newVal := fields[gardenerFieldOld], fields[gardenerFieldNew]
		hasOldField, hasNewField := gardenerFieldPresent(oldVal), gardenerFieldPresent(newVal)
		if r, ok := planGardenerMarker("field", b.ID, gardenerFieldNew, "captain (blob)", "gardener (blob)", hasOldField, hasNewField); ok {
			if r.Status == gardenerSkipIdempotent && !gardenerFieldsEqual(oldVal, newVal) {
				r.Status = gardenerConflict
				r.Note = "captain and gardener fields differ"
				if resync {
					r.Status = gardenerWouldUpdate
				}
			}
			if r.Status == gardenerWouldUpdate {
				patch, err := json.Marshal(map[string]any{gardenerFieldNew: oldVal})
				if err != nil {
					r.Status = gardenerError
					r.Note = "marshalling gardener patch: " + err.Error()
				} else {
					r.fieldPatch = patch
				}
			}
			rows = append(rows, r)
		}
	}
	return rows
}

// planGardenerMarker classifies one marker by which names the bead carries. ok
// is false when it carries neither (no row).
func planGardenerMarker(phase, beadID, field, oldName, newName string, hasOld, hasNew bool) (gardenerRow, bool) {
	r := gardenerRow{Phase: phase, BeadID: beadID, Field: field, Old: orUnset(""), New: orUnset("")}
	switch {
	case hasOld && hasNew:
		r.Old, r.New, r.Status = oldName, newName, gardenerSkipIdempotent
	case hasOld:
		r.Old, r.New, r.Status = oldName, newName, gardenerWouldUpdate
	case hasNew:
		r.New, r.Status = newName, gardenerNewOnly
		r.Note = "new name without old name — old-code pods cannot see this marker"
	default:
		return gardenerRow{}, false
	}
	return r, true
}

// gardenerFieldsEqual compares two present field values by decoded JSON, so key
// order and whitespace do not matter. A string value is decoded as an embedded
// JSON blob when it parses, otherwise compared as a plain string.
func gardenerFieldsEqual(a, b any) bool {
	return reflect.DeepEqual(normalizeGardenerField(a), normalizeGardenerField(b))
}

func normalizeGardenerField(v any) any {
	if s, ok := v.(string); ok {
		var decoded any
		if err := json.Unmarshal([]byte(s), &decoded); err == nil {
			return decoded
		}
		return strings.TrimSpace(s)
	}
	return v
}

// gardenerFieldPresent reports whether a decoded field value is non-empty. The
// daemon surfaces JSON fields either as a string or an already-decoded object.
func gardenerFieldPresent(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(t) != ""
	case map[string]any:
		return len(t) > 0
	default:
		return true
	}
}

// applyGardenerRows writes every WOULD_UPDATE row, flipping it to UPDATED (or
// ERROR on failure). Returns the failure count. A bead's rows are independent:
// a failed label does not stop its field write.
func applyGardenerRows(ctx context.Context, rows []gardenerRow) (failed int) {
	for i := range rows {
		r := &rows[i]
		if r.Status != gardenerWouldUpdate {
			continue
		}
		var err error
		switch r.Phase {
		case "field":
			_, err = beadsClient.UpdateBead(ctx, r.BeadID, &client.UpdateBeadRequest{Fields: r.fieldPatch})
		default:
			_, err = beadsClient.AddLabel(ctx, r.BeadID, r.label)
		}
		if err != nil {
			r.Status, r.Note = gardenerError, err.Error()
			fmt.Fprintf(os.Stderr, "  !! %s: %s update failed: %v\n", r.BeadID, r.Phase, err)
			failed++
			continue
		}
		r.Status = gardenerUpdated
	}
	return failed
}

// gardenerSaturated is the phase-2 gate: no bead is missing or disagrees on a name. After
// a real apply, successfully UPDATED rows count as saturated.
func gardenerSaturated(rows []gardenerRow) bool {
	return countGardenerStatus(rows, gardenerWouldUpdate) == 0 &&
		countGardenerStatus(rows, gardenerNewOnly) == 0 &&
		countGardenerStatus(rows, gardenerConflict) == 0 &&
		countGardenerStatus(rows, gardenerError) == 0
}

func countGardenerStatus(rows []gardenerRow, want gardenerStatus) int {
	n := 0
	for _, r := range rows {
		if r.Status == want {
			n++
		}
	}
	return n
}

func printGardenerRows(rows []gardenerRow, dryRun bool) {
	if len(rows) == 0 {
		fmt.Println("no beads carry captain or gardener markers")
		return
	}
	title := "PROPOSED CHANGES"
	if !dryRun {
		title = "APPLYING CHANGES"
	}
	fmt.Printf("\n%s (%d rows)\n\n", title, len(rows))
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PHASE\tBEAD_ID\tFIELD\tOLD\tNEW\tSTATUS")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Phase, r.BeadID, r.Field, r.Old, r.New, r.Status)
	}
	w.Flush()
}

func printGardenerSummary(rows []gardenerRow, scanned int, dryRun bool, failed int) {
	verb := "would update"
	updated := countGardenerStatus(rows, gardenerWouldUpdate)
	if !dryRun {
		verb = "updated"
		updated = countGardenerStatus(rows, gardenerUpdated)
	}
	fmt.Printf("\nbeads scanned:          %d\n", scanned)
	fmt.Printf("%s (rows):     %d\n", verb, updated)
	fmt.Printf("skip idempotent:        %d\n", countGardenerStatus(rows, gardenerSkipIdempotent))
	fmt.Printf("new only (reverse gap): %d\n", countGardenerStatus(rows, gardenerNewOnly))
	fmt.Printf("conflict (differ):      %d\n", countGardenerStatus(rows, gardenerConflict))
	fmt.Printf("errors:                 %d\n", countGardenerStatus(rows, gardenerError))
	if !dryRun {
		fmt.Printf("failed:                 %d\n", failed)
	}
	gate := "PASS"
	if !gardenerSaturated(rows) {
		gate = "FAIL"
	}
	fmt.Printf("\nSATURATION: %s  (missing new name: %d, missing old name: %d, conflicting: %d)\n",
		gate, countGardenerStatus(rows, gardenerWouldUpdate), countGardenerStatus(rows, gardenerNewOnly), countGardenerStatus(rows, gardenerConflict))
}

func containsString(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// labelSuffixes returns the distinct suffixes of labels carrying prefix, in
// first-seen order.
func labelSuffixes(labels []string, prefix string) []string {
	var out []string
	for _, l := range labels {
		if rest, ok := strings.CutPrefix(l, prefix); ok && rest != "" && !containsString(out, rest) {
			out = append(out, rest)
		}
	}
	return out
}
