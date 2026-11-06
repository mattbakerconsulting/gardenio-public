package main

import (
	"fmt"
	"sort"
	"strings"

	"gardenio/controller/internal/advice"

	"github.com/spf13/cobra"
)

// advice_relabel.go implements `gb advice relabel` — the atomic label MOVE verb
// (bug ka-0000003820).
//
// Why this command exists: moving a targeting/scope label is a high-frequency
// advice-hygiene operation (every scope promotion/demotion, every project
// re-target), but until now it had no first-class verb. Authors kept writing
// `gb advice relabel <id> ...` in task text as if it existed — it reads like it
// should — and cobra answered a nonexistent subcommand by printing the PARENT
// `gb advice` help to stdout and exiting 0. So the teach was doubly misleading:
// the command was absent AND its absence was silent. (Same silent-exit-0 family
// as `kd dep <unknown>`, ka-0000005587; the fix there is depCmd's NoArgs+RunE.)
//
// Forced to improvise, agents reconstructed the move as a NON-ATOMIC pair:
//
//	kd label add    <id> <new-label>
//	kd label remove <id> <old-label>...
//
// If the second call fails or is forgotten, the bead is left double-scoped —
// precisely the broken state the worked task (ka-0000005100) existed to repair.
// `gb advice relabel` collapses both halves into ONE PATCH whose add+remove
// commit together (see beadsapi.SetLabels).

// scopePrefix is the label namespace `--scope` operates on. A bead carries at
// most one meaningful scope: label (scope:on-demand and scope:project-broadcast
// are mutually exclusive delivery dispositions — advice.IsDeniedBroadcastLabels
// vs the lint's isOnDemand), so "swap the scope" is the single most common
// relabel and gets a dedicated flag that needs no --remove bookkeeping.
const scopePrefix = "scope:"

var adviceRelabelCmd = &cobra.Command{
	Use:   "relabel <id> [<new-label>...]",
	Short: "Atomically move labels on an advice bead (add + remove in one call)",
	Long: `Atomically add and remove labels on an advice bead in ONE server call.

This is the verb to reach for when RE-TARGETING advice: demoting a broadcast to
on-demand, promoting an on-demand doc to a role, or moving it between projects.

Why not ` + "`kd label add`" + ` + ` + "`kd label remove`" + `: that pair is two round-trips.
If the second one fails or is forgotten the bead is left carrying BOTH the old
and the new label — double-scoped, the exact corruption this verb prevents.
relabel sends one PATCH whose add and remove commit together.

Labels are always compared/stored in their literal form, including any gN:
group prefix — ` + "`g0:role:crew`" + ` and ` + "`role:crew`" + ` are DIFFERENT labels here, so
copy the exact string from ` + "`gb advice show <id>`" + `.

Examples:
  # Swap the scope: label — no --remove bookkeeping, the old scope: goes away.
  gb advice relabel ka-abc --scope on-demand

  # Explicit move: add scope:on-demand, drop two stale targeting labels.
  gb advice relabel ka-abc scope:on-demand --remove scope:project-broadcast --remove project:gardenio

  # Re-target a role (add + remove in one call).
  gb advice relabel ka-abc role:dataplane-fixer --remove role:crew

  # Preview the resulting label set without writing.
  gb advice relabel ka-abc --scope on-demand --dry-run`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]
		add := args[1:]
		remove, _ := cmd.Flags().GetStringSlice("remove")
		scope, _ := cmd.Flags().GetString("scope")
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		force, _ := cmd.Flags().GetBool("force")

		if len(add) == 0 && len(remove) == 0 && scope == "" {
			return fmt.Errorf("nothing to do: pass at least one <new-label>, --remove <label>, or --scope <name>\n" +
				"       e.g. gb advice relabel <id> --scope on-demand")
		}

		// --scope is sugar over the general form: add scope:<name> and remove
		// every OTHER scope: label the bead carries. The removals can only be
		// computed from the bead's CURRENT labels, so they are resolved in
		// relabelledLabels below rather than desugared here.
		if scope != "" {
			// --scope takes a NAME, and tolerates a redundant "scope:" prefix
			// since that is what a caller copying from `gb advice show` will
			// paste. Anything else containing a colon is rejected: a foreign
			// namespace (--scope role:crew) means the caller wanted a positional
			// label, and a leftover colon after the prefix (--scope
			// scope:foo:bar) is a malformed name. Both used to sail through and
			// write a nonsense label that no delivery predicate reads.
			name := strings.TrimPrefix(scope, scopePrefix)
			if strings.Contains(name, ":") {
				return fmt.Errorf("--scope %q is not a bare scope name; pass just the name (e.g. --scope on-demand),\n"+
					"       or give a full label as a positional arg to add it verbatim", scope)
			}
			if name == "" {
				return fmt.Errorf("--scope needs a name (e.g. --scope on-demand)")
			}
			add = append(add, scopePrefix+name)
		}

		bead, err := daemon.GetBead(cmd.Context(), id)
		if err != nil {
			return fmt.Errorf("getting advice %s: %w", id, err)
		}
		// Guard the verb to advice beads unless forced. relabel REPLACES the
		// whole label set, and non-advice beads (agent/trigger/project beads)
		// carry labels other machinery reconciles — a stray relabel there is a
		// silent config change, not an advice-hygiene edit.
		if bead.Type != "advice" && !force {
			return fmt.Errorf("%s is type %q, not advice — relabel replaces the whole label set,\n"+
				"       which on a non-advice bead can clobber labels other machinery owns.\n"+
				"       Use `kd label add`/`kd label remove` there, or pass --force if you are sure",
				id, bead.Type)
		}

		next, res := relabelledLabels(bead.Labels, add, remove, scope)

		// A no-op is reported as such rather than burning a write. The whole
		// point of the verb is a VERIFIABLE move, so "already in that state"
		// must not be indistinguishable from "moved".
		if !res.changed() {
			fmt.Printf("No label change on %s — already: %s\n", id, strings.Join(bead.Labels, ", "))
			return nil
		}

		// Requested removals that the bead never carried are surfaced (not
		// fatal): the end state is still correct, but a typo'd label name that
		// silently "succeeded" is how a caller believes it dropped a label it
		// did not. Same reasoning as `kd label remove`'s absent-label error
		// (ka-0000000424), softened to a warning because the add half of the
		// move did land.
		if len(res.absent) > 0 {
			fmt.Fprintf(cmd.ErrOrStderr(),
				"warning: not present on %s (nothing to remove): %s\n",
				id, strings.Join(res.absent, ", "))
		}

		// A positional label the bead already carries is almost always the
		// `--remove a b` mis-parse: cobra binds ONE value per --remove
		// occurrence, so "b" silently became an ADD and is NOT removed. Left
		// unsaid, the caller reads "Relabelled" and believes both labels went
		// away. The --scope value is excluded — re-asserting the current scope is
		// a legitimate idempotent call, not a mis-parse.
		if noop := positionalNoopAdds(res.noopAdds, scope); len(noop) > 0 {
			fmt.Fprintf(cmd.ErrOrStderr(),
				"warning: already on %s, nothing added: %s\n"+
					"         if you meant to REMOVE these, note that --remove takes ONE label per flag:\n"+
					"         use --remove %s\n",
				id, strings.Join(noop, ", "), strings.Join(noop, " --remove "))
		}

		if dryRun {
			fmt.Printf("DRY RUN — %s would become:\n", id)
			printRelabelPlan(bead.Labels, next, res)
			return nil
		}

		if err := daemon.SetLabels(cmd.Context(), id, next); err != nil {
			return err
		}

		fmt.Printf("Relabelled %s\n", id)
		printRelabelPlan(bead.Labels, next, res)

		// Delivery disposition is the reason this verb is usually invoked, so
		// report the label set's EFFECT, not just the strings. A move that
		// leaves the bead undeliverable is a legitimate end state
		// (scope:on-demand) but a silent one otherwise.
		warnRelabelDelivery(cmd, next)
		return nil
	},
}

// relabelResult records what a relabel actually changed, so the caller can
// distinguish a real move from a no-op and warn about removals that matched
// nothing.
type relabelResult struct {
	added   []string // labels the bead did not carry and now will
	removed []string // labels the bead carried and now will not
	absent  []string // explicitly-requested removals the bead never carried
	// noopAdds are positional labels the bead ALREADY carried. Almost always a
	// mis-parse rather than an intent: `--remove a b` gives --remove only "a"
	// (cobra takes one value per occurrence) and leaves "b" a positional ADD, so
	// a caller who meant "remove both" silently keeps b. Surfaced as a warning.
	noopAdds []string
}

func (r relabelResult) changed() bool { return len(r.added) > 0 || len(r.removed) > 0 }

// positionalNoopAdds filters the already-present additions down to the ones worth
// warning about: everything except the label --scope itself contributed.
// Re-asserting the current scope (`--scope on-demand` on an already-on-demand
// bead) is a legitimate idempotent call; a POSITIONAL label that is already
// present is the `--remove a b` mis-parse.
func positionalNoopAdds(noopAdds []string, scope string) []string {
	if scope == "" {
		return noopAdds
	}
	scopeLabel := scopePrefix + strings.TrimPrefix(scope, scopePrefix)
	var out []string
	for _, l := range noopAdds {
		if l != scopeLabel {
			out = append(out, l)
		}
	}
	return out
}

// relabelledLabels computes the bead's complete NEXT label set from its current
// one. It is pure so the semantics are testable without a daemon.
//
// Order of operations matters: removals are applied BEFORE additions, so a label
// named in both --remove and <new-label> survives (an explicit add wins over an
// explicit remove) rather than depending on map iteration order. `current` order
// is preserved for the labels that stay, with additions appended — the resulting
// diff reads as a move rather than a reshuffle.
//
// When scope is non-empty, every scope: label OTHER than the incoming
// scope:<name> is dropped, which is what makes `--scope` a true swap without the
// caller having to enumerate the old value.
func relabelledLabels(current, add, remove []string, scope string) ([]string, relabelResult) {
	var res relabelResult

	// Additions are deduplicated so a doubled arg cannot produce a duplicate
	// label row, and so `added` reports each new label once.
	addSet := make(map[string]struct{}, len(add))
	var addOrder []string
	for _, l := range add {
		if l == "" {
			continue
		}
		if _, dup := addSet[l]; dup {
			continue
		}
		addSet[l] = struct{}{}
		addOrder = append(addOrder, l)
	}

	removeSet := make(map[string]struct{}, len(remove))
	for _, l := range remove {
		if l == "" {
			continue
		}
		// An explicit add of the same label wins — see the order note above.
		if _, keep := addSet[l]; keep {
			continue
		}
		removeSet[l] = struct{}{}
	}

	currentSet := make(map[string]struct{}, len(current))
	for _, l := range current {
		currentSet[l] = struct{}{}
	}

	// Report removals the bead never carried. Only labels the CALLER named are
	// eligible: the implicit scope: sweep below is a "make it so" reconcile, and
	// warning that a scope label was already absent would be noise.
	for _, l := range remove {
		if l == "" {
			continue
		}
		if _, had := currentSet[l]; !had {
			if _, keep := addSet[l]; !keep {
				res.absent = append(res.absent, l)
			}
		}
	}

	var next []string
	for _, l := range current {
		if _, drop := removeSet[l]; drop {
			res.removed = append(res.removed, l)
			continue
		}
		// --scope swap: any other scope: label is the one being moved away
		// from. Compared on the stripped form so a grouped `g0:scope:x` is
		// swept too, matching how the delivery-side predicates read scope.
		if scope != "" && strings.HasPrefix(advice.StripGroupPrefix(l), scopePrefix) {
			if _, keep := addSet[l]; !keep {
				res.removed = append(res.removed, l)
				continue
			}
		}
		next = append(next, l)
	}
	for _, l := range addOrder {
		// A label already on the bead survived the loop above and is already in
		// `next` — both the explicit removeSet and the scope sweep skip anything
		// in addSet, so an addition can never have been dropped. Re-appending
		// here would duplicate it and report a phantom addition.
		if _, had := currentSet[l]; had {
			res.noopAdds = append(res.noopAdds, l)
			continue
		}
		next = append(next, l)
		res.added = append(res.added, l)
	}

	return next, res
}

// printRelabelPlan renders the before/after label sets plus the +/- diff, so the
// operator can confirm the move landed exactly as intended — the "leaves EXACTLY
// scope:on-demand, no residue" check from the work order.
func printRelabelPlan(before, after []string, res relabelResult) {
	fmt.Printf("  before:  %s\n", strings.Join(before, ", "))
	fmt.Printf("  after:   %s\n", strings.Join(after, ", "))
	if len(res.added) > 0 {
		sorted := append([]string(nil), res.added...)
		sort.Strings(sorted)
		fmt.Printf("  added:   +%s\n", strings.Join(sorted, ", +"))
	}
	if len(res.removed) > 0 {
		sorted := append([]string(nil), res.removed...)
		sort.Strings(sorted)
		fmt.Printf("  removed: -%s\n", strings.Join(sorted, ", -"))
	}
}

// warnRelabelDelivery reports the delivery consequence of the new label set,
// using the SAME predicates as the delivery path (advice.IsDeniedBroadcastLabels
// / advice.HasTargetingLabel) so the warning cannot drift from what gb prime
// actually does. A relabel that quietly makes advice undeliverable is the
// failure mode behind the undeliverable-capability lint.
func warnRelabelDelivery(cmd *cobra.Command, labels []string) {
	w := cmd.ErrOrStderr()
	switch {
	case advice.IsDeniedBroadcastLabels(labels):
		fmt.Fprintf(w, "warning: bare-project broadcast without %s — gb prime will NOT deliver this bead;\n"+
			"         add --scope project-broadcast to opt in, or narrow with a role:/agent: label\n",
			advice.BroadcastOptInLabel)
	case !advice.HasTargetingLabel(labels):
		fmt.Fprintf(w, "warning: no targeting label (global/project:/role:/agent:) — this bead now matches no agent.\n"+
			"         That is expected for an on-demand doc fetched via `gb advice show`; otherwise add a target.\n")
	}
}

func init() {
	adviceRelabelCmd.Flags().StringSlice("remove", nil,
		"label to remove — ONE per flag; repeat or comma-join for several ('--remove a,b'). "+
			"'--remove a b' removes only 'a' and treats 'b' as a positional label to ADD")
	adviceRelabelCmd.Flags().String("scope", "",
		"swap the scope: label to this name (e.g. on-demand, project-broadcast) — drops any other scope: label")
	adviceRelabelCmd.Flags().Bool("dry-run", false,
		"print the resulting label set without writing")
	adviceRelabelCmd.Flags().Bool("force", false,
		"allow relabelling a non-advice bead (relabel replaces the whole label set)")
}
