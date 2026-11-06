package main

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
)

var childrenCmd = &cobra.Command{
	Use:     "children <bead-id>",
	Short:   "List beads that depend on this bead (reverse dependencies)",
	GroupID: "views",
	Long: `List beads that depend on this bead (reverse dependencies).

--json emits an array whose row schema aligns with ` + "`kd list --json`" + ` so the
same post-processor works on both:

  id        child bead id (SAME value as bead_id — prefer this)
  bead_id   child bead id (retained for back-compat)
  type      the BEAD type (task/bug/epic/...), as in kd list
  dep_type  the DEPENDENCY edge type (parent-child/blocks/...)
  status    child status (from the fetched bead; omitted on fetch error)
  priority  child priority as an integer, incl. 0 for P0 (omitted on fetch error)
  assignee  child assignee (omitted when empty)
  title     child title (omitted when empty)
  updated_at RFC3339 last-update time from the fetched bead (omitted on fetch
             error) — so a staleness filter like
             ` + "`select(.updated_at > \"2026-07-25\")`" + ` works the same as on kd list
  created_at RFC3339 creation time from the fetched bead (omitted on fetch error)

Filter the direct dependents in place:

  kd children <bead-id> -s open             # open-only children (no grep needed)
  kd children <bead-id> -t parent-child     # only parent-child edges

To enumerate an epic's children as FULL bead rows (every kd list column, at any
subtree depth, filterable by status/type) use the reverse-subtree query instead:

  kd list --parent <epic-id> -s open        # descendants of <epic-id>, open only

` + "`kd list --parent`" + ` returns the same row shape as any other kd list and is the
recommended path for "enumerate my epic's children with status/priority".`,
	Args: cobra.ExactArgs(1),
	RunE: runChildrenCmd,
}

// runChildrenCmd is the shared RunE for both the top-level `kd children` and
// the `kd dep children` pass-through (dep.go). Named (not an inline closure)
// so both command paths resolve to the exact same logic and the dep-group
// alias can never drift from the canonical command.
func runChildrenCmd(cmd *cobra.Command, args []string) error {
	beadID := args[0]
	filterType, _ := cmd.Flags().GetString("type")
	filterStatus, _ := cmd.Flags().GetString("status")

	ctx := context.Background()
	deps, err := beadsClient.GetReverseDependencies(ctx, beadID)
	if err != nil {
		return fmt.Errorf("fetching reverse dependencies: %w", err)
	}

	var types []string
	if filterType != "" {
		types = []string{filterType}
		// Parent-child alias expansion (ka-0000008107): a large body of live
		// edges are typed literally "parent" (pre-canonicalization) rather than
		// the canonical "parent-child". Both encode the same hierarchy edge, so
		// filtering on either must match both — otherwise
		// `kd children <epic> --type parent-child` returns zero for epics whose
		// children hang off legacy "parent" edges (and vice-versa). Widen the
		// filter to the full alias set when the caller asks for either.
		if model.IsParentChildDepType(filterType) {
			types = model.ParentChildDepTypes
		}
	}

	// For reverse deps, resolve the BeadID (the bead that depends on us).
	typeSet := make(map[string]bool, len(types))
	for _, t := range types {
		typeSet[t] = true
	}

	type resolvedChild struct {
		Dep  *model.Dependency
		Bead *model.Bead
	}
	var resolved []resolvedChild
	for _, d := range deps {
		if len(typeSet) > 0 && !typeSet[string(d.Type)] {
			continue
		}
		rc := resolvedChild{Dep: d}
		fetchedBead, err := beadsClient.GetBead(ctx, d.BeadID)
		if err == nil {
			rc.Bead = fetchedBead
		}
		// Status filter (matches `kd list -s` semantics). Only rows whose
		// resolved status differs from the requested one are dropped. A row
		// whose bead failed to fetch has an UNKNOWN status — we cannot prove it
		// fails the filter, so it is kept (surfaced with "(unknown)") rather
		// than silently dropped, which would hide a possibly-matching child.
		if filterStatus != "" && rc.Bead != nil && string(rc.Bead.Status) != filterStatus {
			continue
		}
		resolved = append(resolved, rc)
	}

	if len(resolved) == 0 {
		fmt.Println("No dependents found.")
		return nil
	}

	if jsonOutput {
		// Row schema aligns with `kd list --json` so a caller can post-process
		// children rows the same way it post-processes list rows (ka-0000004855):
		//   - `id`        — the child bead id (SAME value as `bead_id`; use this).
		//   - `bead_id`   — retained for back-compat with pre-alignment consumers.
		//   - `type`      — the BEAD type (task/bug/epic/...), matching `kd list`.
		//   - `dep_type`  — the DEPENDENCY edge type (parent-child/blocks/...).
		//                   Previously `type` carried this; it now lives here so
		//                   `type` can mean the same thing it does in `kd list`.
		//   - status/priority/assignee/title/updated_at/created_at come from the
		//     already-fetched bead (children.go GetBead per row); a fetch error
		//     leaves them zero-valued and omitted. `priority` is a *int so a real
		//     P0 (priority 0) still serializes — a plain int with omitempty would
		//     drop it and re-create the "PNone" trap this fix removes.
		//   - updated_at/created_at are *time.Time so a fetch error OMITS the key
		//     rather than emitting a zero time. This is load-bearing: a bare
		//     `time.Time` with omitempty ALWAYS serializes (encoding/json never
		//     treats a struct as empty), so a nil bead would emit
		//     "0001-01-01T00:00:00Z" — which a staleness filter reads as an
		//     ancient-but-real timestamp. The whole point of this fix (ka-0000004797)
		//     is that a caller can time-filter children rows the same way it
		//     filters `kd list` rows; emitting null (the pre-fix bug) OR a phantom
		//     zero time both silently break `select(.updated_at > "<date>")`.
		type jsonChild struct {
			ID        string     `json:"id"`
			BeadID    string     `json:"bead_id"`
			Type      string     `json:"type,omitempty"`
			DepType   string     `json:"dep_type"`
			Status    string     `json:"status,omitempty"`
			Priority  *int       `json:"priority,omitempty"`
			Assignee  string     `json:"assignee,omitempty"`
			Title     string     `json:"title,omitempty"`
			UpdatedAt *time.Time `json:"updated_at,omitempty"`
			CreatedAt *time.Time `json:"created_at,omitempty"`
		}
		var out []jsonChild
		for _, rd := range resolved {
			jc := jsonChild{
				ID:      rd.Dep.BeadID,
				BeadID:  rd.Dep.BeadID,
				DepType: string(rd.Dep.Type),
			}
			if rd.Bead != nil {
				jc.Type = string(rd.Bead.Type)
				jc.Status = string(rd.Bead.Status)
				p := rd.Bead.Priority
				jc.Priority = &p
				jc.Assignee = rd.Bead.Assignee
				jc.Title = rd.Bead.Title
				// Timestamps carried by-value on the fetched bead. Copy into
				// locals and take their address so each row points at its own
				// value (never the loop variable). A zero UpdatedAt/CreatedAt is
				// left as-is here — the daemon always stamps both on a real bead,
				// so a zero implies a malformed row, which the *time.Time still
				// renders faithfully rather than masquerading as omitted.
				u := rd.Bead.UpdatedAt
				jc.UpdatedAt = &u
				c := rd.Bead.CreatedAt
				jc.CreatedAt = &c
			}
			out = append(out, jc)
		}
		emitJSON(out)
	} else {
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "BEAD_ID\tTYPE\tSTATUS\tTITLE")
		for _, rd := range resolved {
			status := "(unknown)"
			title := "(error fetching)"
			if rd.Bead != nil {
				status = string(rd.Bead.Status)
				title = rd.Bead.Title
				if len(title) > 50 {
					title = title[:47] + "..."
				}
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
				rd.Dep.BeadID,
				string(rd.Dep.Type),
				status,
				title,
			)
		}
		w.Flush()
	}
	return nil
}

// addChildrenFlags registers the `children` flag set on a command. Shared so
// the top-level `kd children` and the `kd dep children` pass-through expose an
// identical flag contract.
func addChildrenFlags(cmd *cobra.Command) {
	cmd.Flags().StringP("type", "t", "", "filter by dependency type")
	cmd.Flags().StringP("status", "s", "", "filter by child status (open/in_progress/closed/...)")
}

func init() {
	addChildrenFlags(childrenCmd)
}
