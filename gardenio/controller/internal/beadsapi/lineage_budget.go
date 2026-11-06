package beadsapi

import (
	"context"
	"fmt"
)

// Per-lineage-root continuation budget (task ka-0000001361).
//
// THE GAP THIS CLOSES. A `successor=new` stop (cmd/gb stop_successor.go
// mintSuccessorAgent) mints a FRESH agent bead that carries the predecessor's
// lineage_root_id forward and then closes the predecessor. Nothing bounds how
// many times that can repeat: an agent that stops with successor=new every run
// mints a new bead + pod forever, an unbounded self-spawn loop. The wake-scanner's
// per-BEAD respawn_count (internal/statusreporter/wake_scanner.go) does NOT bound
// this — respawn_count lives on one bead and resets to zero on every fresh
// successor bead, so it only caps park/wake of a SINGLE identity, never the outer
// mint-a-new-identity chain.
//
// The honest unit for that outer loop is the lineage: every bead in one
// succession chain shares a lineage_root_id (seat_key.go / stop_successor.go), so
// COUNTING agent beads that carry a given lineage_root_id gives the chain's total
// length. LineageContinuationCount does exactly that, server-side and count-only
// (no row bodies), and mintSuccessorAgent gates on it before minting.

// LineageContinuationCount returns how many agent beads carry the given
// lineage_root_id — i.e. the total length of that succession chain across all
// statuses (open + closed), the FIRST occupant included. A root anchors itself
// (the first bead's lineage_root_id is set to its own id by the mint/stamp paths),
// so a lineage that has never continued counts 1, a lineage continued once counts
// 2, and so on.
//
// Count-only (no sort, no projections, no row bodies) so it stays cheap even for
// a long chain, and it counts EVERY status because a spent budget must survive the
// predecessor being closed — the whole point is that closed predecessors still
// count against the ceiling. Returns an error the caller can fail-open on; a zero
// count with a nil error means "no beads carry this root" (an unknown/empty root).
func (c *Client) LineageContinuationCount(ctx context.Context, lineageRootID string) (int, error) {
	if lineageRootID == "" {
		return 0, fmt.Errorf("empty lineage_root_id")
	}
	res, err := c.ListBeadsFiltered(ctx, ListBeadsQuery{
		Types:     []string{"agent"},
		Fields:    map[string]string{FieldLineageRootID: lineageRootID},
		CountOnly: true,
	})
	if err != nil {
		return 0, fmt.Errorf("counting lineage %s continuations: %w", lineageRootID, err)
	}
	return res.Total, nil
}
