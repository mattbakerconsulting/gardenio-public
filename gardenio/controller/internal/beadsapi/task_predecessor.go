package beadsapi

import (
	"context"
	"log/slog"
	"sort"
)

// Task-shared succession stamping (laurels/lineage epic ka-0000006664, task
// ka-0000007863; grounded on the 7-day measurement in ka-0000001141 §1.3).
//
// THE GAP THIS CLOSES. Over a 7-day window, 1,472 crew agents carried a
// task_id spanning 702 distinct tasks. 298 of those tasks received MORE THAN
// ONE crew agent — 770 agents arrived at a bead a predecessor had already
// worked — and NOT ONE of them carried prior_agent_id. Meanwhile 631 of those
// predecessors had written non-empty handoff_notes. The handoff channel was
// authored and never delivered: epic ka-0000005352 alone drew 56 crew agents,
// all `prior=-`.
//
// The pointer already existed (prior_agent_id / lineage_root_id, registered on
// type:agent) but was only stamped on paths that ALREADY had continuity by
// other means — gardener iteration and thread respawn, which carry
// schedule_prior_agent_id anyway. The one population with no continuity signal
// at all was the one it skipped.
//
// WHAT THIS DOES. At the single spawn choke point (SpawnAgentResult), when a
// spawn targets a task_id and no live agent holds it, look for the most recent
// CLOSED agent that worked the same task and stamp prior_agent_id +
// lineage_root_id from it. `gb prime` then renders that predecessor's handoff
// (see cmd/gb/prime_task_predecessor.go).
//
// WHAT THIS DELIBERATELY DOES NOT DO. It confers no standing, seniority, or
// credit on the predecessor. The pointer is a positional fact — this bead was
// worked before, by that agent bead — not a claim that the predecessor's work
// was good or that its conclusions should be inherited.
//
// THE FRAMING THAT MATTERS (operator direction, 2026-08-16). Two constraints
// arrived together and the design sits between them:
//
//  1. An agent's self-report must not silently become the next agent's premise.
//     "I have seen agentic systems go from liquid to solid phase on the
//     momentum of agent-perceived accomplishment."
//  2. But in a fleet where ~100% of available context is agent output, tagging
//     that output "unverified" discriminates nothing. A caveat that applies to
//     everything is ritual, and gets skipped.
//
// So the discriminator is NOT authorship — it is whether a claim arrives with
// the record behind it. What this pointer buys the read side is exactly that
// trail: the predecessor's BEAD id, from which its close_reason and closed_at
// are one lookup away. A handoff from an agent that died on
// max_restarts_reached and one from an agent that ran `gb stop` deliberately
// are different objects, and the successor can tell which it holds. The
// hardening failure mode needs a claim that travels with no route back to what
// happened; attaching the route is what prevents it, not a disclaimer.

// taskPredecessorLookbackLimit bounds the predecessor scan. A task with more
// agent beads than this is pathological (the 7d max was 56); the scan takes
// the newest page and picks the latest closed row from it, so the bound costs
// nothing in practice and prevents an unbounded pull on a runaway loop.
const taskPredecessorLookbackLimit = 50

// findTaskPredecessor returns the most recently created CLOSED agent bead that
// worked the same task, or nil when the task has no prior occupant.
//
// Why CLOSED only: an open/in_progress agent on the same task is either the
// live incumbent (which the single-claim guard already returns instead of
// spawning) or a concurrent fan-out sibling. Neither is a predecessor — a
// predecessor is an occupant that has FINISHED and therefore has a terminal
// wrap-up worth reading. Pointing at a still-running sibling would render a
// half-written handoff and imply a succession that isn't one.
//
// Best-effort at the boundary: any daemon error returns (nil, err) and the
// caller logs and proceeds with an unstamped spawn. A missing pointer costs a
// prime section; a failed spawn costs the work.
func (c *Client) findTaskPredecessor(ctx context.Context, taskID, excludeBeadID string) (*BeadDetail, error) {
	if taskID == "" {
		return nil, nil
	}
	res, err := c.ListBeadsFiltered(ctx, ListBeadsQuery{
		Types:    []string{"agent"},
		Statuses: []string{"closed"},
		Fields:   map[string]string{"task_id": taskID},
		Sort:     "-created_at",
		Limit:    taskPredecessorLookbackLimit,
	})
	if err != nil {
		return nil, err
	}
	if res == nil || len(res.Beads) == 0 {
		return nil, nil
	}

	candidates := make([]*BeadDetail, 0, len(res.Beads))
	for _, b := range res.Beads {
		if b == nil || b.ID == "" || b.ID == excludeBeadID {
			continue
		}
		candidates = append(candidates, b)
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	// Re-sort client-side on created_at descending rather than trusting the
	// server sort verbatim: the daemon's sort key is applied before the
	// field-filter projection on some paths, and a wrong pick here is silent
	// (you get AN occupant, just not the LAST one). Ties fall back to bead id
	// so the choice is deterministic across calls — a predecessor pointer that
	// flapped between two beads on identical timestamps would make the prime
	// section non-reproducible.
	sort.SliceStable(candidates, func(i, j int) bool {
		ti, tj := candidates[i].CreatedAt, candidates[j].CreatedAt
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return candidates[i].ID > candidates[j].ID
	})
	return candidates[0], nil
}

// stampTaskPredecessorLineage fills prior_agent_id + lineage_root_id into the
// outgoing spawn fields from the task's last closed occupant. It is a no-op
// when the spawn targets no task, when a caller already supplied an explicit
// prior_agent_id (the successor/gardener/thread paths, which know their own
// predecessor and must win), or when the task has no prior occupant.
//
// Fail-open: a lookup error logs and leaves the fields untouched.
func (c *Client) stampTaskPredecessorLineage(ctx context.Context, taskID string, fields map[string]string) {
	if taskID == "" || fields == nil {
		return
	}
	// An explicit pointer from the caller is authoritative — the successor mint
	// (stop_successor.go), gardener iteration, and thread cold-remint each know
	// the exact bead they continue, which is a stronger claim than "worked the
	// same task at some point". Never overwrite it.
	if fields["prior_agent_id"] != "" {
		return
	}

	pred, err := c.findTaskPredecessor(ctx, taskID, "")
	if err != nil {
		slog.Warn("task-predecessor lookup failed, spawning without lineage pointer",
			"task", taskID, "error", err)
		return
	}
	if pred == nil {
		return
	}

	fields["prior_agent_id"] = pred.ID
	// lineage_root_id anchors the chain: inherit the predecessor's root when it
	// has one, else the predecessor itself becomes the root. Same rule as the
	// successor mint (stop_successor.go), so a task chain and a successor chain
	// are readable by one walk.
	if root := pred.Fields["lineage_root_id"]; root != "" {
		fields["lineage_root_id"] = root
	} else {
		fields["lineage_root_id"] = pred.ID
	}
	slog.Info("stamped task-shared predecessor lineage",
		"task", taskID, "prior_agent", pred.ID, "lineage_root", fields["lineage_root_id"])
}
