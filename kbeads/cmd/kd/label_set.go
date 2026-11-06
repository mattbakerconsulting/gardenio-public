package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"gardenio/kbeads/internal/client"
	"github.com/spf13/cobra"
)

// label_set.go adds `kd label set` / `kd label replace` — the atomic label MOVE
// verbs for ANY bead type (bug ka-0000003820). `gb advice relabel` is the
// advice-specific sibling; this is the general one.
//
// Until now `kd label` exposed only add and remove, so moving a label meant two
// non-atomic round-trips in the right order:
//
//	kd label add    <id> <new>
//	kd label remove <id> <old>
//
// If the second call fails or is forgotten the bead carries BOTH labels. These
// verbs send ONE PATCH: the daemon's updateBead flips labelsSet and calls
// reconcileLabels in the same request, so the add and the remove commit together.

var labelSetCmd = &cobra.Command{
	Use:   "set <bead-id> <label>...",
	Short: "Replace a bead's entire label set in one atomic call",
	Long: `Replace a bead's ENTIRE label set in one server call.

This is declarative: the labels you pass become the labels the bead has. Any
label currently on the bead and NOT listed is REMOVED. Because the add and the
remove commit in the same request, the bead is never left carrying both an old
and a new label — the corruption a ` + "`kd label add`" + ` + ` + "`kd label remove`" + ` pair
leaves behind when the second call fails or is forgotten.

To MOVE one label while leaving the rest alone, prefer ` + "`kd label replace`" + `, which
computes the set for you. Use ` + "`set`" + ` when you want to state the full end state.

Refuses to remove EVERY label unless --allow-empty is passed: an accidental
empty set (an unquoted glob, an empty shell variable) would silently strip a
bead's project: label and drop it out of every scoped query.

Examples:
  # State the exact end state (everything else is removed).
  kd label set ka-abc project:gardenio scope:on-demand

  # Preview without writing.
  kd label set ka-abc project:gardenio --dry-run`,
	Args: cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		beadID := args[0]
		want := args[1:]
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		allowEmpty, _ := cmd.Flags().GetBool("allow-empty")
		return applyLabelSet(cmd, beadID, want, dryRun, allowEmpty)
	},
}

var labelReplaceCmd = &cobra.Command{
	Use:   "replace <bead-id> <old-label> <new-label>",
	Short: "Atomically swap one label for another (add + remove in one call)",
	Long: `Atomically swap ONE label for another, leaving every other label untouched.

This is the narrow, safe form of the label MOVE: it reads the bead's current
labels, substitutes <old-label> with <new-label>, and writes the result in a
single PATCH. Unlike ` + "`kd label set`" + ` you do not have to restate the labels you
want to keep, so there is no way to drop one by omission.

Errors if the bead does not carry <old-label>, so a mistyped label name cannot
read as a successful move (pass --if-exists to just ensure the new label is
present instead).

Examples:
  kd label replace ka-abc scope:project-broadcast scope:on-demand
  kd label replace ka-abc role:crew role:dataplane-fixer`,
	Args: cobra.ExactArgs(3),
	RunE: func(cmd *cobra.Command, args []string) error {
		beadID, oldLabel, newLabel := args[0], args[1], args[2]
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		ifExists, _ := cmd.Flags().GetBool("if-exists")

		current, err := currentLabels(cmd.Context(), beadID)
		if err != nil {
			return err
		}

		if !containsStr(current, oldLabel) {
			if !ifExists {
				return fmt.Errorf("no such label on %s: %s\n"+
					"       nothing was changed — check the spelling, list the real labels with\n"+
					"       `kd show %s`, or pass --if-exists to just ensure %q is present",
					beadID, oldLabel, beadID, newLabel)
			}
			// --if-exists: the swap degrades to "ensure the new label is
			// present", which is the idempotent end state a script wants.
			if containsStr(current, newLabel) {
				fmt.Printf("No label change on %s — %s already present, %s absent\n", beadID, newLabel, oldLabel)
				return nil
			}
			return applyLabelSet(cmd, beadID, append(append([]string(nil), current...), newLabel), dryRun, false)
		}

		// Substitute in place so unrelated labels keep their position and the
		// printed diff reads as a swap.
		next := make([]string, 0, len(current)+1)
		for _, l := range current {
			switch l {
			case oldLabel:
				if !containsStr(next, newLabel) {
					next = append(next, newLabel)
				}
			case newLabel:
				if !containsStr(next, newLabel) {
					next = append(next, l)
				}
			default:
				next = append(next, l)
			}
		}
		return applyLabelSet(cmd, beadID, next, dryRun, false)
	},
}

// currentLabels reads a bead's labels, failing loudly if the bead is missing.
// The label MOVE verbs are all read-then-set, and a silent empty read here would
// turn a typo'd bead id into a label wipe.
func currentLabels(ctx context.Context, beadID string) ([]string, error) {
	bead, err := beadsClient.GetBead(ctx, beadID)
	if err != nil {
		return nil, fmt.Errorf("getting bead %s: %w", beadID, err)
	}
	if bead == nil {
		return nil, fmt.Errorf("bead %s not found", beadID)
	}
	return bead.Labels, nil
}

// applyLabelSet writes `want` as the bead's complete label set via a single
// PATCH, printing the before/after diff. It reports a no-op rather than claiming
// a change, so "already in that state" is never indistinguishable from "moved".
func applyLabelSet(cmd *cobra.Command, beadID string, want []string, dryRun, allowEmpty bool) error {
	// Deduplicate while preserving order: a doubled arg must not produce a
	// duplicate label row.
	seen := make(map[string]struct{}, len(want))
	next := make([]string, 0, len(want))
	for _, l := range want {
		if l == "" {
			continue
		}
		if _, dup := seen[l]; dup {
			continue
		}
		seen[l] = struct{}{}
		next = append(next, l)
	}

	if len(next) == 0 && !allowEmpty {
		return fmt.Errorf("refusing to remove every label from %s\n"+
			"       an empty set here is usually an unquoted glob or an empty shell variable,\n"+
			"       and would strip the bead's project:/targeting labels out of every scoped\n"+
			"       query. Pass --allow-empty if that is genuinely what you want", beadID)
	}

	current, err := currentLabels(cmd.Context(), beadID)
	if err != nil {
		return err
	}

	added, removed := labelSetDiff(current, next)
	if len(added) == 0 && len(removed) == 0 {
		fmt.Printf("No label change on %s — already: %s\n", beadID, strings.Join(current, ", "))
		return nil
	}

	if dryRun {
		fmt.Printf("DRY RUN — %s would become:\n", beadID)
		printLabelSetPlan(current, next, added, removed)
		return nil
	}

	// An empty set cannot go over the wire as `labels: []` — the daemon infers
	// set-ness from a non-empty array, so [] is an ambiguous no-op-or-wipe.
	// Strip the remaining labels individually instead.
	if len(next) == 0 {
		for _, l := range removed {
			if _, err := beadsClient.RemoveLabel(cmd.Context(), beadID, l); err != nil {
				return fmt.Errorf("removing label %q: %w", l, err)
			}
		}
	} else if _, err := beadsClient.UpdateBead(cmd.Context(), beadID, &client.UpdateBeadRequest{Labels: next}); err != nil {
		return fmt.Errorf("setting labels on %s to [%s]: %w", beadID, strings.Join(next, ", "), err)
	}

	fmt.Printf("Set label(s) on %s\n", beadID)
	printLabelSetPlan(current, next, added, removed)
	return nil
}

// labelSetDiff reports which labels the write adds and which it removes, so the
// command can prove what actually changed instead of printing an unfalsifiable
// success line (the ka-0000000424 lesson applied to the set verbs).
func labelSetDiff(current, next []string) (added, removed []string) {
	currentSet := make(map[string]struct{}, len(current))
	for _, l := range current {
		currentSet[l] = struct{}{}
	}
	nextSet := make(map[string]struct{}, len(next))
	for _, l := range next {
		nextSet[l] = struct{}{}
	}
	for _, l := range next {
		if _, had := currentSet[l]; !had {
			added = append(added, l)
		}
	}
	for _, l := range current {
		if _, keep := nextSet[l]; !keep {
			removed = append(removed, l)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}

func printLabelSetPlan(before, after, added, removed []string) {
	fmt.Printf("  before:  %s\n", strings.Join(before, ", "))
	fmt.Printf("  after:   %s\n", strings.Join(after, ", "))
	if len(added) > 0 {
		fmt.Printf("  added:   +%s\n", strings.Join(added, ", +"))
	}
	if len(removed) > 0 {
		fmt.Printf("  removed: -%s\n", strings.Join(removed, ", -"))
	}
}

func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
