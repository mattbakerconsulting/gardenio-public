package main

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
)

var depCmd = &cobra.Command{
	Use:     "dep",
	Short:   "Manage bead dependencies",
	GroupID: "beads",
	// depCmd is a parent group, but it MUST be runnable (have a RunE) so that
	// cobra reaches the Args validator below. A non-runnable parent short-
	// circuits to flag.ErrHelp BEFORE ValidateArgs runs (cobra command.go:
	// `if !c.Runnable() { return flag.ErrHelp }`), which prints help to STDOUT
	// and exits 0 — the silent-exit-0 bug from ka-0000005587, where a stray
	// `kd dep tree <id>` returned a false-empty result and a `|| fallback`
	// never fired. With a RunE present, `cobra.NoArgs` is reached and rejects
	// any UNKNOWN subcommand (e.g. `kd dep bogus`) with a non-zero exit +
	// stderr msg, matching how the ROOT command already rejects unknown
	// commands. (KNOWN subcommands — add/remove/list and the tree/children
	// pass-throughs below — match first in cobra's Find and are validated by
	// their own Args, never reaching depCmd.NoArgs.) A bare `kd dep` (no args)
	// passes NoArgs and falls through to RunE → help, exit 0.
	//
	// Side effect of becoming runnable: a bare `kd dep` now runs the root
	// PersistentPreRunE (token/credstore/client setup) before RunE, like every
	// other leaf command — a non-runnable group skipped it. Normal `kd dep`
	// still prints help and exits 0; only an invalid global flag (e.g.
	// `--transport=bogus`) now errors here instead of being silently ignored.
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return cmd.Help()
	},
}

// depTreeCmd / depChildrenCmd are pass-through aliases that make the natural
// `kd dep tree <id>` / `kd dep children <id>` invocations WORK (rather than
// erroring), reusing the canonical run + flag wiring from tree.go / children.go
// so behavior can never diverge from the top-level `kd tree` / `kd children`.
var depTreeCmd = &cobra.Command{
	Use:   "tree <bead-id>",
	Short: "Show dependency tree for a bead (alias of `kd tree`)",
	Args:  cobra.ExactArgs(1),
	RunE:  runTreeCmd,
}

var depChildrenCmd = &cobra.Command{
	Use:   "children <bead-id>",
	Short: "List beads that depend on this bead (alias of `kd children`)",
	// Reuse the canonical Long verbatim (children.go) so `kd dep children --help`
	// carries the SAME JSON-schema doc, the `-s`/`-t` in-place filters, and the
	// `kd list --parent -s open` recommendation as `kd children --help`. Cobra
	// prints only the Short line when Long is empty, so an empty Long here would
	// silently drop that guidance from the alias — the exact divergence
	// ka-0000002550 hit. Assigned in init() (below), after childrenCmd.Long is set.
	Args: cobra.ExactArgs(1),
	RunE: runChildrenCmd,
}

var depAddCmd = &cobra.Command{
	Use: "add <bead-id> <depends-on-id>",
	// The direction is in Short — not only Long — because `kd dep --help` lists
	// subcommands by Short alone, and that group listing is the FIRST place
	// someone looks. The reported failure was needing three separate help probes
	// (`kd dep`, `kd dep add`, `kd dep remove`) to reconstruct the direction.
	Short: "Add a dependency between beads (dependent first: `add A B` = A depends on / is blocked by B)",
	Long: `Add a dependency between beads.

DIRECTION: the DEPENDENT bead comes FIRST, the thing it depends on SECOND
(stored as bead_id=<bead-id>, depends_on_id=<depends-on-id>).

For the default --type blocks that means:

  kd dep add A B --type blocks   →   A is BLOCKED BY B   (B must land first)

It does NOT mean "A blocks B" — that is the reverse edge, ` + "`kd dep add B A`" + `.
Misreading this order has stored a P0 as blocked-by its own parent and deadlocked
a release. A reversed blocks-edge cannot be auto-CORRECTED server-side (either
orientation is legitimate, so there is no canonical direction to flip to), so the
non-JSON output echoes the stored direction back in prose — read it, and fix a
wrong edge with ` + "`kd dep remove`" + `. To check edges already stored, run
` + "`kd dep list <bead-id>`" + ` — it lists that bead's OUTGOING edges, so a
blocks/blocked-by/depends-on row there is something that bead is waiting on.

The one reversal the server does catch is a blocks CYCLE (A blocked-by B and B
blocked-by A) — that edge is rejected at insert, because it would make both
beads permanently undispatchable.

For --type parent-child the canonical order is CHILD first, PARENT second; the
server auto-canonicalizes the common epic-first mistake and the add says so when
it does.`,
	Example: `  # The blocked bead comes first: the P0 waits on its prerequisite.
  # Said the other way round, "prereq blocks the P0" is this SAME edge:
  kd dep add ka-p0bead ka-prereq --type blocks

  # blocked-by and depends-on are accepted spellings of the same direction:
  kd dep add ka-p0bead ka-prereq -t blocked-by

  # Parent-child: CHILD first, EPIC second:
  kd dep add ka-child ka-epic --type parent-child`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		beadID := args[0]
		dependsOnID := args[1]
		depType, _ := cmd.Flags().GetString("type")

		// Client-side hygiene warning (ka-0000006880): the canonical
		// parent-child direction is CHILD first, PARENT second
		// (bead_id=child, depends_on_id=parent). A common mistake is
		// `kd dep add <epic> <child> -t parent-child`, which stores the
		// row REVERSED and makes the child invisible to the subtree CTE.
		// The server now auto-canonicalizes this shape, but warn here so
		// the operator notices the swapped arguments. Best-effort: a
		// lookup failure is silent so the add still proceeds.
		warnReversedParentChild(cmd, beadID, dependsOnID, depType)

		ctx := context.Background()
		// --force overrides the server's closed-parent guard (ka-0000005756):
		// a parent-child edge onto an already-closed epic is normally rejected
		// because the child becomes a claimable orphan the auto-close cascade
		// can never drain. Pass --force to deliberately re-parent new work onto
		// a recently-closed epic.
		if force, _ := cmd.Flags().GetBool("force"); force {
			ctx = client.WithForceParentEdge(ctx)
		}

		dep, err := beadsClient.AddDependency(ctx, &client.AddDependencyRequest{
			BeadID:      beadID,
			DependsOnID: dependsOnID,
			Type:        depType,
			CreatedBy:   actor,
		})
		if err != nil {
			return fmt.Errorf("adding dependency: %w", err)
		}

		// Did the server auto-canonicalize a reversed parent-child edge?
		// The add echoes the STORED orientation (dep.BeadID/DependsOnID),
		// which is swapped relative to what the caller typed when the guard
		// fired (ka-0000006880 / server parent_child_guard.go). Detect it by
		// comparing requested vs returned endpoints so we can shout about it.
		flipped := depWasCanonicalized(beadID, dependsOnID, dep)

		if jsonOutput {
			// Emit the dependency plus a top-level "canonicalized" flag so
			// machine consumers (and `... | jq .canonicalized`) can see the
			// flip without re-deriving it. We wrap rather than mutate
			// model.Dependency so its schema stays untouched.
			out := struct {
				*model.Dependency
				Canonicalized bool `json:"canonicalized"`
			}{Dependency: dep, Canonicalized: flipped}
			emitJSON(out)
		} else {
			fmt.Printf("Bead:        %s\n", dep.BeadID)
			fmt.Printf("Depends On:  %s\n", dep.DependsOnID)
			fmt.Printf("Type:        %s\n", dep.Type)
			fmt.Printf("Created By:  %s\n", dep.CreatedBy)
			if !dep.CreatedAt.IsZero() {
				fmt.Printf("Created At:  %s\n", dep.CreatedAt.Format("2006-01-02 15:04:05"))
			}
			// EXACTLY ONE trailing NOTE line, printed LAST so it survives the
			// ubiquitous `kd dep add ... 2>&1 | tail -N` truncation that
			// otherwise keeps only Created By/At and drops the meaningful lines
			// (ka-0000007975). The "which note" decision is made structurally in
			// trailingDepNote — a single string, so two notes can never both
			// print and silently steal each other's last line (ka-0000002186).
			if note := trailingDepNote(dep, flipped); note != "" {
				fmt.Println(note)
			}
		}
		return nil
	},
}

// trailingDepNote returns the SINGLE NOTE line printed last in the non-JSON
// `dep add` success output, or "" when no note applies. Collapsing the choice
// into one function makes the mutual exclusivity STRUCTURAL rather than a prose
// invariant spread across two call sites: whatever fires, at most one string is
// ever returned, so a later change that makes both conditions true (e.g.
// canonicalization extended to another dep type, or isWaitEdgeType gaining
// parent-child) can no longer have two notes silently steal each other's last
// line and void a `| tail -N` guarantee (ka-0000002186).
//
// The flip note wins over the blocks echo when both would apply: a
// canonicalized parent-child edge is not a wait edge today, but the ordering
// makes the priority explicit rather than accidental, and the flip note is the
// one that reports a surprise the caller did not ask for.
func trailingDepNote(dep *model.Dependency, flipped bool) string {
	switch {
	case flipped:
		// dep.BeadID is the stored CHILD, dep.DependsOnID the stored PARENT.
		return fmt.Sprintf("NOTE: orientation auto-canonicalized — stored %s child-of %s "+
			"(you asked for the reverse; see ka-0000006880)",
			dep.BeadID, dep.DependsOnID)
	case isWaitEdgeType(dep.Type):
		// Direction echo for wait edges (ka-0000004954). Either orientation of a
		// blocks edge is legitimate, so the server cannot auto-correct a
		// reversed one; restating the STORED direction is the only add-time
		// defense against the shape that deadlocked a release — a P0 stored as
		// blocked-by its own parent.
		return fmt.Sprintf("NOTE: %s is now BLOCKED BY %s (%s must land first)",
			dep.BeadID, dep.DependsOnID, dep.DependsOnID)
	}
	return ""
}

// isWaitEdgeType reports whether a dep type means "bead_id is WAITING ON
// depends_on_id" — the direction the add-time echo describes. It mirrors
// blockingDepTypes in internal/store/postgres/queries.go, the allow-list that
// `kd list --no-blockers` / `gb ready` use to decide what suppresses readiness:
// all three spellings store the same orientation, so all three want the same
// echo. Scoping the echo to the literal "blocks" would silently skip
// `-t blocked-by`, which is exactly the spelling someone unsure of the
// direction reaches for.
func isWaitEdgeType(t model.DependencyType) bool {
	switch t {
	case model.DepBlocks, "blocked-by", "depends-on":
		return true
	}
	return false
}

// depWasCanonicalized reports whether the server swapped the endpoints of a
// parent-child dependency relative to what the caller requested — i.e. the
// reversed-shape auto-canonicalization in server parent_child_guard.go fired
// (ka-0000006880). It is true iff the stored edge is the exact reverse of the
// request: stored bead_id == requested depends_on_id AND stored depends_on_id
// == requested bead_id. A nil dep, or an unchanged orientation, returns false.
func depWasCanonicalized(reqBeadID, reqDependsOnID string, dep *model.Dependency) bool {
	if dep == nil {
		return false
	}
	if dep.Type != model.DepParentChild {
		return false
	}
	return dep.BeadID == reqDependsOnID && dep.DependsOnID == reqBeadID &&
		reqBeadID != reqDependsOnID
}

var depRemoveCmd = &cobra.Command{
	Use:   "remove <bead-id> <depends-on-id>",
	Short: "Remove a dependency between beads",
	Long: `Remove a dependency between beads.

Removing an edge that does not exist is reported as an error (exit 1) rather
than a bare "Removed dependency", so a mistyped --type or bead id cannot read
as success. The most common way to hit this is fat-fingering --type during a
reparent: the wrong-type remove looks like it worked, the real edge survives,
and the bead ends up with BOTH edges.

Pass --if-exists for scripted/idempotent removal, where "already gone" is the
desired end state and should exit 0.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		beadID := args[0]
		dependsOnID := args[1]
		depType, _ := cmd.Flags().GetString("type")
		ifExists, _ := cmd.Flags().GetBool("if-exists")

		removed, err := beadsClient.RemoveDependency(context.Background(), beadID, dependsOnID, depType)
		if err != nil {
			return fmt.Errorf("removing dependency: %w", err)
		}

		// Name the concrete edge in BOTH outcomes. The old bare "Removed
		// dependency" was unfalsifiable — it printed the same text whether the
		// requested triple matched a row or nothing at all.
		edge := formatDepEdge(beadID, dependsOnID, depType)
		if !removed {
			if ifExists {
				fmt.Printf("No such dependency: %s (nothing to remove)\n", edge)
				return nil
			}
			// stderr + non-zero: the whole point is that `... || fallback` and
			// `set -e` see this as the failure it is.
			return fmt.Errorf("no such dependency: %s\n"+
				"       nothing was removed — check --type (default: blocks) and the bead ids,\n"+
				"       list the real edges with `kd dep list %s`, or pass --if-exists to treat\n"+
				"       an already-absent edge as success", edge, beadID)
		}

		fmt.Printf("Removed dependency: %s\n", edge)
		return nil
	},
}

// formatDepEdge renders a dependency triple as `child -type-> parent` — the
// stored orientation, matching how `kd dep list` reports the edge, so the
// message can be compared against it without re-deriving which end is which.
// An empty depType (the "remove regardless of type" wire form) degrades to
// `child --> parent` naturally: no special case needed.
func formatDepEdge(beadID, dependsOnID, depType string) string {
	return fmt.Sprintf("%s -%s-> %s", beadID, depType, dependsOnID)
}

var depListCmd = &cobra.Command{
	Use:   "list <bead-id>",
	Short: "List dependencies of a bead",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		beadID := args[0]

		deps, err := beadsClient.GetDependencies(context.Background(), beadID)
		if err != nil {
			return fmt.Errorf("listing dependencies: %w", err)
		}

		if jsonOutput {
			emitJSON(deps)
		} else {
			if len(deps) == 0 {
				// A bead with zero OUTGOING deps still commonly has INCOMING
				// parent-child edges (its children/dependents) — the exact
				// shape an epic takes after `kd dep add <epic> <child>` is
				// canonicalized. The bare "No dependencies found." read as a
				// dead end and hid the children entirely (ka-0000003337). Point
				// the reader at `kd dep children`, and when a cheap reverse-dep
				// lookup succeeds, tell them exactly how many incoming edges
				// exist. The lookup is best-effort: a transport that can't do
				// reverse deps (e.g. gRPC) falls back to the static hint.
				n, ok := countIncomingDeps(context.Background(), beadID)
				switch {
				case ok && n > 0:
					fmt.Printf("No dependencies found. (%d incoming: see `kd dep children %s`)\n", n, beadID)
				default:
					fmt.Printf("No dependencies found. (incoming edges, if any: `kd dep children %s`)\n", beadID)
				}
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "DEPENDS_ON\tTYPE\tCREATED_BY\tCREATED_AT")
			for _, d := range deps {
				createdAt := ""
				if !d.CreatedAt.IsZero() {
					createdAt = d.CreatedAt.Format("2006-01-02 15:04:05")
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
					d.DependsOnID,
					d.Type,
					d.CreatedBy,
					createdAt,
				)
			}
			w.Flush()
		}
		return nil
	},
}

// countIncomingDeps returns the number of reverse dependencies (edges where
// some other bead depends on beadID — its children/dependents) and whether the
// lookup succeeded. It is best-effort: any transport error (e.g. gRPC, which
// does not implement reverse-dep lookup) or a nil client returns ok=false so
// callers can fall back to a static hint rather than surfacing an error on what
// is otherwise a successful `kd dep list`.
func countIncomingDeps(ctx context.Context, beadID string) (int, bool) {
	if beadsClient == nil {
		return 0, false
	}
	rev, err := beadsClient.GetReverseDependencies(ctx, beadID)
	if err != nil {
		return 0, false
	}
	return len(rev), true
}

// warnReversedParentChild prints a stderr warning when a parent-child
// dep is being added with the higher-level bead (an epic) given FIRST —
// i.e. `kd dep add <epic> <child> -t parent-child`, which stores the row
// reversed (ka-0000006880). The canonical direction is CHILD first. The
// check is best-effort: any lookup failure is silent so the add still
// proceeds, and the server-side guard auto-canonicalizes regardless.
func warnReversedParentChild(cmd *cobra.Command, beadID, dependsOnID, depType string) {
	if depType != "parent-child" || beadID == "" || dependsOnID == "" || beadID == dependsOnID {
		return
	}
	ctx := context.Background()
	first, err := beadsClient.GetBead(ctx, beadID)
	if err != nil || first == nil || string(first.Type) != "epic" {
		return
	}
	second, err := beadsClient.GetBead(ctx, dependsOnID)
	if err != nil || second == nil || string(second.Type) == "epic" {
		return
	}
	fmt.Fprintf(cmd.ErrOrStderr(),
		"warning: parent-child args look REVERSED — %s is an epic (parent) but was given as the first (child) arg.\n"+
			"         Canonical order is CHILD first: `kd dep add %s %s -t parent-child`.\n"+
			"         The server will auto-canonicalize, but check your arguments (ka-0000006880).\n",
		beadID, dependsOnID, beadID)
}

func init() {
	// The direction clause is part of the FLAG help, not just the command Long,
	// because `--type blocks` is the default: the arg order is most often read
	// (and misread) by someone who never passed --type at all (ka-0000004954).
	// Phrased with the <bead-id>/<depends-on-id> placeholders rather than a
	// worked `add` example because this string is shared with `dep remove`,
	// whose argument order is the same.
	// NOTE: no backticks in this string — cobra's UnquoteUsage() lifts the first
	// backquoted span out as the flag's value PLACEHOLDER, so a quoted example
	// here renders as `--type <bead-id> is blocked BY …` instead of
	// `--type string`.
	depTypeHelp := "dependency type (blocks, parent-child, related, duplicates, supersedes, triggered-by, rates); " +
		"dependent first — for blocks, <bead-id> is blocked BY <depends-on-id>"
	depAddCmd.Flags().StringP("type", "t", "blocks", depTypeHelp)
	depAddCmd.Flags().Bool("force", false,
		"override the closed-parent guard: allow a parent-child edge onto an already-closed epic (ka-0000005756)")
	depRemoveCmd.Flags().StringP("type", "t", "blocks", depTypeHelp)
	depRemoveCmd.Flags().Bool("if-exists", false,
		"exit 0 when the edge is already absent (idempotent/scripted removal)")

	depCmd.AddCommand(depAddCmd)
	depCmd.AddCommand(depRemoveCmd)
	depCmd.AddCommand(depListCmd)

	// Working pass-throughs: `kd dep tree` / `kd dep children` resolve to the
	// same logic as the top-level commands (ka-0000005587 discoverability fix).
	// Mirror the canonical Long help onto the children alias so `kd dep children
	// --help` never drops the schema doc / filter hints / `kd list --parent`
	// recommendation that `kd children --help` carries (ka-0000002550). treeCmd
	// has no Long, so there is nothing analogous to mirror onto depTreeCmd.
	depChildrenCmd.Long = childrenCmd.Long
	addTreeFlags(depTreeCmd)
	addChildrenFlags(depChildrenCmd)
	depCmd.AddCommand(depTreeCmd)
	depCmd.AddCommand(depChildrenCmd)
}
