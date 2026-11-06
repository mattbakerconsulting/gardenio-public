// open_children.go — `gb open-children <bead-id>` lists IDs of parent-child
// children of <bead-id> that are OPEN, acceptance-bearing work items.
//
// Why this exists: gardener-bugfix repeatedly re-dispatches umbrella tracker
// bugs (bugs whose acceptance is staged across multiple child beads, not a
// single MR — ka-0000002333 is the poster child). Each cycle spawns a crew
// that correctly stands down or pivots to a child task; the dispatch keeps
// happening because the bug itself has no assignee. The fix per ka-0000003110
// is to teach gardener-bugfix to skip bugs that are parents of any open
// (non-closed) parent-child child.
//
// This command is the testable primitive the gardener-bugfix prompt calls:
//
//	if [[ -n "$(gb open-children "$id")" ]]; then continue; fi
//
// Empty stdout → not an umbrella; non-empty → skip.
//
// A "child" only counts when it is an acceptance-bearing work item —
// task/bug/feature/epic. Auto-created narrative children (type=bundle,
// report, rich-report) and data/session records (type=session, kind=data)
// are NOT staged acceptance; counting them makes ordinary leaf bugs look
// like umbrellas and suppresses dispatch forever (ka-0000001519).
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

var openChildrenCmd = &cobra.Command{
	Use:     "open-children <bead-id>",
	Short:   "List open, acceptance-bearing parent-child children of <bead-id>",
	GroupID: "orchestration",
	Long: `List the IDs of parent-child children of <bead-id> that are open
(status not "closed") AND are acceptance-bearing work items
(task/bug/feature/epic). One ID per line on stdout; empty output means the
bead has no open work children (it is not an umbrella tracker).

Auto-created narrative children (bundle/report/rich-report) and data/session
records (type=session, kind=data) are excluded — they are not staged
acceptance and must not suppress dispatch on an otherwise-leaf bug.

Gardener-bugfix uses this to skip umbrella tracker bugs whose acceptance
is staged across child beads rather than landing as one MR. See
ka-0000003110 (ka-0000002333 is the poster child).`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		beadID := args[0]
		open, unreadable, err := openParentChildren(cmd.Context(), daemon, beadID)
		if err != nil {
			return err
		}
		// stderr, so stdout stays the open-work set callers test for empty.
		printUnreadableChildren(os.Stderr, "", unreadable)
		if jsonOutput {
			printJSON(open)
			return nil
		}
		for _, id := range open {
			fmt.Println(id)
		}
		return nil
	},
}

// acceptanceBearingChildTypes is the set of child bead types that represent
// staged acceptance work — the only reason to treat a parent as an umbrella
// tracker and skip dispatch on it. Narrative/record children (bundle, report,
// rich-report) and data/session records (session, kind=data) are auto-created
// and are NOT staged acceptance; counting them made ordinary leaf bugs look
// like umbrellas and get skipped forever (ka-0000001519).
//
// The allowlist is deliberately narrow: an unknown/unlisted type is excluded,
// which at worst causes one extra crew dispatch (a false negative) — the same
// error-direction the status filter already prefers over silently skipping a
// real bug. This mirrors the canonical work-item type set used elsewhere in
// the controller (mr.go, gardener_block_on_mr.go, trigger-engine).
//
// Note: a plain kind filter is insufficient — bundles are kind=issue, so only
// a type allowlist reliably excludes both session (kind=data) and bundle
// (kind=issue) narrative children.
var acceptanceBearingChildTypes = map[string]bool{
	"task":    true,
	"bug":     true,
	"feature": true,
	"epic":    true,
}

// childInfo carries the two fields the filter needs about a candidate child:
// its bead type and its status. Both are fetched in one daemon round-trip via
// GetBead, so adding the type check does not add an N+1.
type childInfo struct {
	Type   string
	Status string
}

// beadFetcher is the slice of beadsapi.Client we depend on. Pulled out as an
// interface so the filter logic can be unit-tested without a daemon.
type beadFetcher interface {
	GetReverseDependencies(ctx context.Context, beadID string) ([]beadsapi.Dependency, error)
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
}

// openParentChildren queries reverse-deps for beadID and returns the IDs of
// parent-child children that are open, acceptance-bearing work items, and
// the IDs of the children the daemon answered 404 for (outside the caller's
// project scope, gd-BYCG4IGJB2). Returned IDs are in the order the daemon
// emits them.
func openParentChildren(ctx context.Context, c beadFetcher, beadID string) (open, unreadable []string, err error) {
	deps, err := c.GetReverseDependencies(ctx, beadID)
	if err != nil {
		return nil, nil, fmt.Errorf("getting reverse-deps for %s: %w", beadID, err)
	}
	info := make(map[string]childInfo, len(deps))
	for _, d := range deps {
		if d.Type != "parent-child" {
			continue
		}
		if _, seen := info[d.BeadID]; seen {
			continue
		}
		b, err := c.GetBead(ctx, d.BeadID)
		if err != nil || b == nil {
			// Treat fetch failure as unknown — exclude. A flaky daemon
			// should not silently turn an umbrella into a dispatch.
			info[d.BeadID] = childInfo{}
			if isNotFound(err) {
				unreadable = append(unreadable, d.BeadID)
			}
			continue
		}
		info[d.BeadID] = childInfo{Type: b.Type, Status: b.Status}
	}
	return filterOpenParentChildren(deps, info), unreadable, nil
}

// printUnreadableChildren prints one line per child bead the caller's token
// cannot read, so `gb open-children` and `gb plan show` name it rather than
// drop it.
func printUnreadableChildren(w io.Writer, indent string, ids []string) {
	for _, id := range ids {
		fmt.Fprintf(w, "%s%s (unreadable: outside your project scope)\n", indent, id)
	}
}

// filterOpenParentChildren is the pure filter half of `gb open-children`,
// split out so it can be unit-tested without daemon round-trips.
//
// Returns the subset of deps that are (a) parent-child edges, (b) whose type
// (looked up in info) is acceptance-bearing (task/bug/feature/epic), and (c)
// whose status is neither "closed" nor empty. Empty status/type is treated as
// unknown and excluded — we prefer false negatives (one more crew dispatch)
// over false positives (skipping a real bug because we couldn't fetch its
// child, or because a session/bundle child masqueraded as an umbrella).
func filterOpenParentChildren(deps []beadsapi.Dependency, info map[string]childInfo) []string {
	var out []string
	seen := make(map[string]bool, len(deps))
	for _, d := range deps {
		if d.Type != "parent-child" {
			continue
		}
		if seen[d.BeadID] {
			continue
		}
		seen[d.BeadID] = true
		ci := info[d.BeadID]
		if !acceptanceBearingChildTypes[ci.Type] {
			continue
		}
		if ci.Status == "" || ci.Status == "closed" {
			continue
		}
		out = append(out, d.BeadID)
	}
	return out
}
