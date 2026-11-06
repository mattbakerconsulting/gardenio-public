package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// outputPriorWorkOnBead writes the "## Prior work on this bead" block when the
// agent was re-dispatched onto a task a predecessor already worked. It is the
// read half of Shape A (ka-0000006664): the fleet authors ~631 handoffs/week on
// shared-task re-dispatch and delivered zero of them, because the only prior-run
// injection (outputScheduledLoopContext) is gated on schedule_id, not task_id.
// This generalizes that block to the 770/week crew-on-shared-task cohort.
//
// Mechanism: if this agent bead carries a task_id, find the most recent CLOSED
// agent bead sharing that task_id (preferring an explicit prior_agent_id pointer
// stamped by A1, ka-0000007863) and render its predecessor's handoff_notes +
// tldr — the two wrap-up fields authored specifically for the next occupant.
//
// No-op (silent) when: no agent bead, no task_id, no prior closed occupant, or
// the predecessor left no usable handoff/tldr. All lookups are best-effort — a
// daemon error just skips the block; it never blocks prime.
func outputPriorWorkOnBead(w io.Writer, agentID string) {
	if agentID == "" || daemon == nil {
		return
	}
	ctx := context.Background()
	agentBead, err := daemon.GetBead(ctx, agentID)
	if err != nil || agentBead == nil {
		return
	}
	taskID := strings.TrimSpace(agentBead.Fields["task_id"])
	if taskID == "" {
		return
	}

	prior := findPriorOccupant(ctx, agentID, taskID, agentBead.Fields["prior_agent_id"])
	if prior == nil {
		return
	}
	handoff, tldr := priorHandoffAndTLDR(prior)
	if handoff == "" && tldr == "" {
		return
	}

	fmt.Fprintln(w, "\n## Prior work on this bead")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "You were re-dispatched onto a task a previous agent (`%s`) already worked and\n", prior.ID)
	fmt.Fprintln(w, "closed. Read their handoff before starting — it exists so you don't rediscover")
	fmt.Fprintln(w, "what they already learned.")
	fmt.Fprintln(w)
	if tldr != "" {
		fmt.Fprintln(w, "**Predecessor tl;dr:**")
		for line := range strings.SplitSeq(strings.TrimRight(tldr, "\n"), "\n") {
			fmt.Fprintf(w, "> %s\n", line)
		}
		fmt.Fprintln(w)
	}
	if handoff != "" {
		fmt.Fprintln(w, "**Handoff notes from your predecessor:**")
		for line := range strings.SplitSeq(strings.TrimRight(handoff, "\n"), "\n") {
			fmt.Fprintf(w, "> %s\n", line)
		}
		fmt.Fprintln(w)
	}
}

// findPriorOccupant returns the most recent CLOSED agent bead that worked the
// same task_id as the current agent, or nil if none exists. When an explicit
// prior_agent_id pointer is set (stamped by A1 on task-shared re-dispatch), that
// pointer wins — it is the authoritative predecessor. Otherwise the task_id is
// queried directly, so A2 delivers handoffs even before A1 is deployed (the
// 770/week cohort carries no pointer today).
//
// selfID is excluded so a resume of the same bead never renders its own handoff
// back to itself.
func findPriorOccupant(ctx context.Context, selfID, taskID, priorAgentID string) *beadsapi.BeadDetail {
	if pid := strings.TrimSpace(priorAgentID); pid != "" && pid != selfID {
		if prior, err := daemon.GetBead(ctx, pid); err == nil && prior != nil {
			return prior
		}
		// Fall through to the task_id discovery path if the pointer is stale.
	}

	result, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"agent"},
		Statuses: []string{"closed"},
		Fields:   map[string]string{"task_id": taskID},
		Sort:     "-created_at",
		Limit:    10,
	})
	if err != nil || result == nil {
		return nil
	}
	// Walk newest-first and return the first closed occupant that is not us and
	// carries a usable handoff/tldr, so an empty-wrapup predecessor doesn't mask
	// an earlier one that actually authored a handoff.
	for _, b := range result.Beads {
		if b == nil || b.ID == selfID {
			continue
		}
		if handoff, tldr := priorHandoffAndTLDR(b); handoff != "" || tldr != "" {
			return b
		}
	}
	return nil
}

// priorHandoffAndTLDR extracts the handoff_notes and tldr strings from a prior
// agent bead's stored wrap-up. Both empty on no wrap-up or parse failure.
func priorHandoffAndTLDR(bead *beadsapi.BeadDetail) (handoff, tldr string) {
	if bead == nil {
		return "", ""
	}
	raw := bead.Fields[WrapUpFieldName]
	if raw == "" || raw == "null" {
		return "", ""
	}
	var wu struct {
		HandoffNotes string `json:"handoff_notes"`
		TLDR         string `json:"tldr"`
	}
	if err := json.Unmarshal([]byte(raw), &wu); err != nil {
		return "", ""
	}
	return strings.TrimSpace(wu.HandoffNotes), strings.TrimSpace(wu.TLDR)
}
