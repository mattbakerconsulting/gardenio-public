package wall

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/lands/purse"
)

// The land's live-agent cap (Track B spec §3.2 max_live_agents, §4.2 item 3,
// step 9.5). A tenant land runs at most max_live_agents agents at once, the
// steward included. LiveCap refuses a new spawn past it with
// CodeMaxLiveAgents, the same way the other wall codes refuse: the bead stays
// open at budget_exceeded and is re-decided each reconcile pass, so it starts
// when a slot frees.
//
// The steward's own slot is always held for it, live or not, and the steward
// is never refused here: it has to be able to wake to watch and kill the
// land's gardeners (steward amendment 2026-10-23 §5), and there is one of it.
// So gardeners fill at most max_live_agents - 1 slots. The land namespace's
// pod quota (max_live_agents + 1, internal/lands/landns) is the backstop for
// a race between two admissions.

const (
	// CodeMaxLiveAgents refuses a spawn on a land already running its cap.
	CodeMaxLiveAgents = "max_live_agents"

	// FieldMaxLiveAgents is the project bead field holding a land's cap.
	FieldMaxLiveAgents = "max_live_agents"
	// DefaultMaxLiveAgents is the cap of a land whose field is unset or not
	// a positive number. It matches provision.DefaultMaxLiveAgents, what the
	// provisioner writes.
	DefaultMaxLiveAgents = 3

	// StewardRole is the land's steward role (internal/steward Role).
	StewardRole = "steward"

	liveScan = 1000
)

// notLive are the agent states that hold no slot: finished, refused by the
// wall (not started), queued behind a cap, or parked by the purse watcher
// (stopping is the suspend path).
var notLive = map[string]bool{
	"done": true, "failed": true, "killed": true, AgentState: true, "queued": true, "stopping": true,
}

// MaxLiveAgents reads a land's cap from its project bead fields.
func MaxLiveAgents(fields map[string]string) int {
	if n := purse.FieldInt(fields, FieldMaxLiveAgents); n > 0 {
		return n
	}
	return DefaultMaxLiveAgents
}

// IsSteward reports whether a comma-joined agent role names the steward.
func IsSteward(role string) bool {
	for r := range strings.SplitSeq(role, ",") {
		if strings.TrimSpace(r) == StewardRole {
			return true
		}
	}
	return false
}

// Live counts a land's live agents other than the bead exclude (the one being
// decided): gardeners and any other non-steward agent in others, and whether
// a steward (StewardSpawn) is among them. A "crew,steward" agent holds a slot.
func Live(ctx context.Context, src purse.Source, land, exclude string) (others int, steward bool, err error) {
	res, err := src.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: []string{"agent"}, Labels: []string{"project:" + land},
		Statuses: []string{"open", "in_progress", "blocked", "deferred"}, Limit: liveScan,
	})
	if err != nil {
		return 0, false, fmt.Errorf("reading %s's live agents: %w", land, err)
	}
	for _, b := range res.Beads {
		if b == nil || b.Type != "agent" || b.ID == exclude || b.Status == "closed" {
			continue
		}
		if p := strings.TrimSpace(b.Fields["project"]); p != land && (p != "" || !slices.Contains(b.Labels, "project:"+land)) {
			continue
		}
		if notLive[b.Fields["agent_state"]] {
			continue
		}
		if StewardSpawn(b.Fields["role"]) {
			steward = true
			continue
		}
		others++
	}
	return others, steward, nil
}

// LiveCapFull is the pure half of LiveCap: whether a non-steward spawn fits
// beside others live non-steward agents under limit, the steward's slot held.
func LiveCapFull(others, limit int) bool {
	return others+1 >= limit
}

// LiveCap decides a new spawn of role on a tenant land against its
// max_live_agents. fields are the land's project bead fields; beadID is the
// agent being decided, which does not count against itself. A steward spawn
// (exactly StewardSpawn, as Check holds it; a "crew,steward" list is not one,
// gd-U9AV7jG8hF) always passes. A land whose agents cannot be read is refused: fail closed,
// as for the purse.
func LiveCap(ctx context.Context, src purse.Source, project string, fields map[string]string, role, beadID string) Refusal {
	if StewardSpawn(role) {
		return Refusal{Tenant: true}
	}
	limit := MaxLiveAgents(fields)
	others, _, err := Live(ctx, src, project, beadID)
	if err != nil {
		return Refusal{Code: CodeMaxLiveAgents, Tenant: true, Reason: fmt.Sprintf(
			"land %q's live agents could not be counted against its max_live_agents (%d): %v", project, limit, err)}
	}
	if LiveCapFull(others, limit) {
		return Refusal{Code: CodeMaxLiveAgents, Tenant: true, Reason: fmt.Sprintf(
			"land %q runs at most %d agents at once, its steward included, and %d gardener(s) are live; this one starts when one finishes",
			project, limit, others)}
	}
	return Refusal{Tenant: true}
}
