package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
)

// adminMigrateAdviceTargetingCmd rewrites existing advice beads'
// targeting labels to the canonical form now required by the
// validator. The rewrite is a dry-run by default; pass --apply to
// actually update beads.
//
// Rules (see validate.go for the matching validator):
//
//  1. global + project:X         → drop `global`, keep `project:X`
//  2. global + rig:X             → drop `global`, keep `project:X`
//  3. global + agent:X           → drop `global`, keep `agent:X`
//  4. global + role:X            → drop `role:X`, keep `global`
//     (role: has no pre-filter; `global` was
//     already delivering to everyone)
//  5. rig:X alone                → rewrite to `project:X`
//  6. rig:X + project:X (same)   → drop `rig:X`, keep `project:X`
//  7. rig:X + project:Y (diff)   → leave untouched, add label
//     `advice-migration:ambiguous` for human
//     review — there is no safe default here
//  8. gN: group variants apply   → rules run on the stripped form; labels
//     keep their gN: prefix where applicable
//
// Re-running the migration on already-normalized data is a no-op.
var adminMigrateAdviceTargetingCmd = &cobra.Command{
	Use:   "advice-targeting",
	Short: "Normalize advice labels to the canonical project:/role:/agent: form",
	Long: `Normalize advice label targeting onto the canonical project: prefix
and remove redundant "global" combinations.

By default the command is a dry-run: it prints what would change.
Pass --apply to update the beads.

Ambiguous cases (rig:X + project:Y with different values) are tagged
"advice-migration:ambiguous" rather than mutated; a human should
resolve them manually.`,
	RunE: runAdminMigrateAdviceTargeting,
}

func init() {
	adminMigrateAdviceTargetingCmd.Flags().Bool("apply", false, "actually mutate beads (default is dry-run)")
	adminMigrateAdviceTargetingCmd.Flags().Int("limit", 500, "max advice beads to fetch per page")
}

// migrationPlan describes a single bead's proposed label change.
type migrationPlan struct {
	BeadID       string
	Title        string
	BeforeLabels []string
	AfterLabels  []string
	Rule         string
	Ambiguous    bool
}

func (p migrationPlan) changed() bool {
	return !stringsEqualSorted(p.BeforeLabels, p.AfterLabels)
}

func runAdminMigrateAdviceTargeting(cmd *cobra.Command, _ []string) error {
	apply, _ := cmd.Flags().GetBool("apply")
	limit, _ := cmd.Flags().GetInt("limit")
	if limit <= 0 {
		limit = 500
	}
	ctx := context.Background()

	// Fetch all open + in_progress advice. Closed advice isn't delivered,
	// so it's not worth touching.
	resp, err := beadsClient.ListBeads(ctx, &client.ListBeadsRequest{
		Type:   []string{"advice"},
		Status: []string{"open", "in_progress"},
		Limit:  limit,
	})
	if err != nil {
		return fmt.Errorf("listing advice: %w", err)
	}

	var plans []migrationPlan
	for _, b := range resp.Beads {
		plan := planAdviceMigration(b.ID, b.Title, b.Labels)
		if plan.changed() || plan.Ambiguous {
			plans = append(plans, plan)
		}
	}

	if len(plans) == 0 {
		fmt.Printf("0 beads to change (scanned %d)\n", len(resp.Beads))
		return nil
	}

	printPlans(plans, apply)

	if !apply {
		fmt.Fprintln(os.Stderr, "\nDry-run only. Re-run with --apply to mutate beads.")
		return nil
	}

	// Apply. Each bead is updated via UpdateBead (sends a Labels
	// replacement) so SSE events flow naturally. Errors are reported
	// but don't abort the run — migration is idempotent.
	var applied, skipped, failed int
	for _, p := range plans {
		if p.Ambiguous {
			// Tag with an operator-review label rather than mutating.
			if err := tagAmbiguous(ctx, p.BeadID); err != nil {
				fmt.Fprintf(os.Stderr, "  !! %s: tag-ambiguous failed: %v\n", p.BeadID, err)
				failed++
			} else {
				fmt.Printf("  ~~ %s: tagged advice-migration:ambiguous\n", p.BeadID)
				skipped++
			}
			continue
		}
		newLabels := append([]string(nil), p.AfterLabels...)
		_, err := beadsClient.UpdateBead(ctx, p.BeadID, &client.UpdateBeadRequest{
			Labels: newLabels,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "  !! %s: update failed: %v\n", p.BeadID, err)
			failed++
			continue
		}
		fmt.Printf("  ✓ %s: %v → %v\n", p.BeadID, p.BeforeLabels, p.AfterLabels)
		applied++
	}

	fmt.Printf("\napplied=%d  tagged-ambiguous=%d  failed=%d\n", applied, skipped, failed)
	if failed > 0 {
		return fmt.Errorf("%d bead(s) failed to update", failed)
	}
	return nil
}

// planAdviceMigration computes the canonical label set for a single
// advice bead. The function is pure so it can be unit-tested without
// a running daemon.
func planAdviceMigration(id, title string, labels []string) migrationPlan {
	plan := migrationPlan{BeadID: id, Title: title, BeforeLabels: append([]string(nil), labels...)}

	// Detect the ambiguous rig+project conflict first; leave labels alone
	// and tag for operator review.
	var rigVals, projectVals []string
	for _, l := range labels {
		clean := model.StripGroupPrefix(l)
		switch {
		case strings.HasPrefix(clean, "rig:"):
			rigVals = append(rigVals, strings.TrimPrefix(clean, "rig:"))
		case strings.HasPrefix(clean, "project:"):
			projectVals = append(projectVals, strings.TrimPrefix(clean, "project:"))
		}
	}
	if hasDistinctValues(rigVals, projectVals) {
		plan.Ambiguous = true
		plan.Rule = "rig/project conflict — manual review"
		plan.AfterLabels = plan.BeforeLabels
		return plan
	}

	// Walk each label, rewriting or dropping as the rules say.
	hasGlobal := false
	hasProject := false
	hasAgent := false
	hasRole := false
	for _, l := range labels {
		clean := model.StripGroupPrefix(l)
		switch {
		case clean == "global":
			hasGlobal = true
		case strings.HasPrefix(clean, "project:"):
			hasProject = true
		case strings.HasPrefix(clean, "rig:"):
			hasProject = true
		case strings.HasPrefix(clean, "agent:"):
			hasAgent = true
		case strings.HasPrefix(clean, "role:"):
			hasRole = true
		}
	}

	var out []string
	var rules []string
	for _, l := range labels {
		prefix, clean := splitGroupPrefix(l)

		// Rule 5/6: rewrite rig:X → project:X.
		if strings.HasPrefix(clean, "rig:") {
			proj := "project:" + strings.TrimPrefix(clean, "rig:")
			// Rule 6 dedupe: if a project:X with the same value is
			// already present in the label set, drop the rig: form.
			if labelsContain(labels, "project:"+strings.TrimPrefix(clean, "rig:")) ||
				labelsContain(labels, "project:"+strings.TrimPrefix(clean, "rig:")+"") {
				// Drop duplicate rig:X.
				rules = append(rules, "dropped duplicate rig:X")
				continue
			}
			out = append(out, prefix+proj)
			rules = append(rules, "rig: → project:")
			continue
		}

		// Rule 1/2/3: drop `global` when a targeting label is present.
		if clean == "global" && (hasProject || hasAgent) {
			rules = append(rules, "dropped redundant global (targeting present)")
			continue
		}

		// Rule 4: drop `role:X` when `global` is present — the advice
		// already delivers to everyone, so role: is misleading decoration.
		if strings.HasPrefix(clean, "role:") && hasGlobal && !hasProject && !hasAgent {
			rules = append(rules, "dropped role: under global (OR already wins)")
			continue
		}

		out = append(out, l)
	}
	_ = hasRole // kept for readability parity with the validator

	plan.AfterLabels = out
	plan.Rule = strings.Join(uniqueStrings(rules), "; ")
	if plan.Rule == "" {
		plan.Rule = "no change"
	}
	return plan
}

// splitGroupPrefix peels off a gN: prefix if present; returns (prefix, rest).
// The prefix retains its trailing colon when non-empty.
func splitGroupPrefix(label string) (prefix, rest string) {
	if !strings.HasPrefix(label, "g") {
		return "", label
	}
	idx := strings.IndexByte(label, ':')
	if idx <= 1 {
		return "", label
	}
	digits := label[1:idx]
	for _, c := range digits {
		if c < '0' || c > '9' {
			return "", label
		}
	}
	return label[:idx+1], label[idx+1:]
}

func hasDistinctValues(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	for _, x := range a {
		for _, y := range b {
			if x != y {
				return true
			}
		}
	}
	return false
}

func labelsContain(labels []string, target string) bool {
	for _, l := range labels {
		if model.StripGroupPrefix(l) == target {
			return true
		}
	}
	return false
}

func uniqueStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// stringsEqualSorted compares two label lists order-independently.
func stringsEqualSorted(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	aSet := make(map[string]int, len(a))
	for _, s := range a {
		aSet[s]++
	}
	for _, s := range b {
		aSet[s]--
		if aSet[s] < 0 {
			return false
		}
	}
	return true
}

func tagAmbiguous(ctx context.Context, beadID string) error {
	_, err := beadsClient.AddLabel(ctx, beadID, "advice-migration:ambiguous")
	return err
}

func printPlans(plans []migrationPlan, apply bool) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	title := "PROPOSED CHANGES"
	if apply {
		title = "APPLYING CHANGES"
	}
	fmt.Fprintf(w, "\n%s (%d beads)\n\n", title, len(plans))
	fmt.Fprintln(w, "ID\tRULE\tBEFORE → AFTER")
	for _, p := range plans {
		before := truncateDisplay(labelsOneLine(p.BeforeLabels), 40)
		after := truncateDisplay(labelsOneLine(p.AfterLabels), 40)
		fmt.Fprintf(w, "%s\t%s\t%s → %s\n", p.BeadID, p.Rule, before, after)
	}
	w.Flush()
}

func labelsOneLine(labels []string) string {
	if len(labels) == 0 {
		return "[]"
	}
	return "[" + strings.Join(labels, ", ") + "]"
}

func truncateDisplay(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
