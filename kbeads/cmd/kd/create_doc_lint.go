package main

import (
	"fmt"
	"io"
)

// lintOrphanDoc warns when `kd create --type=doc` is run without the author
// saying where the doc goes — no explicit --parent-task/--parent on the
// command line and no --no-parent (bug ka-0000000755).
//
// THE TRAP this closes. A type=doc bead is nearly always meant to render
// inside a type=bundle at /bundle/<id>. The bundle viewer collects its docs
// by walking parent-child reverse-deps ON THE BUNDLE (buildBundleView in
// gardenio/controller/cmd/beads-viewer/bundle.go) — so a doc that is not a
// parent-child child of the bundle simply does not appear in the TOC.
//
// Meanwhile the server-side auto-parent step (maybeAddAutoParent in
// internal/server/auto_parent.go) fires for every agent-created bead and
// wires it to the AGENT'S CURRENT TASK. That default is correct for work
// beads, but for a doc it silently steals the edge the bundle needed: the
// doc lands under the agent's task, the bundle renders "No documents
// attached to this bundle yet", and NOTHING in either the create output or
// the bundle URL says so. An agent then shares a bundle link that reads as
// empty to the human who opens it — measured 2026-08-15, a blog draft posted
// to Slack as an empty bundle.
//
// The warning is advisory (stderr, exit 0) because a bare `kd create
// --type=doc` is legal — a standalone doc not destined for a bundle is a real
// use. It only fires when the author expressed no parent intent at all, which
// is exactly the case where the auto-parent default silently wins.
//
// parentExplicit must be flags.Changed("parent-task"), NOT `parentTask != ""`.
// The flag DEFAULTS to $BOAT_TASK_ID, so in an agent pod it is non-empty on
// every invocation — and a doc wired to the agent's task is precisely the
// broken outcome this warns about. Only an explicit --parent-task/--parent on
// the command line counts as the author having chosen a destination.
//
// Returns the number of warnings emitted (0 or 1).
func lintOrphanDoc(w io.Writer, beadType string, parentExplicit, noParent bool) int {
	if beadType != "doc" || parentExplicit || noParent {
		return 0
	}
	fmt.Fprint(w,
		"warning: creating a type=doc bead with no --parent-task.\n"+
			"         The server auto-parents it to YOUR CURRENT TASK, not to a bundle — a doc that is\n"+
			"         not a parent-child child of a bundle never renders in that bundle's TOC.\n"+
			"         For a bundle doc, prefer `gb bundle add <bundle-id> --doc <path>` (creates + links\n"+
			"         + closes in one step), or pass `--parent-task <bundle-id>` here.\n"+
			"         Pass --no-parent to silence this for a deliberately standalone doc.\n")
	return 1
}
