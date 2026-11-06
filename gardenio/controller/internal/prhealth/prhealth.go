// Package prhealth reads the open-PR health figures the PR sweeper records
// (plan gd-3F4W22h2nT step 10, gd-Pt8ztuasTE).
//
// The sweeper (trigger gd-7cQvwrgva9, docs/pr-sweeper.md) classifies every
// open PR once an hour. At the end of each run it writes what it counted, as
// one JSON object, to the pr_health field of its own agent bead. The trigger
// stamps last_agent_id with the run it spawned, so "the trigger's last run" is
// the source, and nothing here recomputes the figures from GitHub. Two readers
// share this package: the controller's metrics collector (the Grafana panel
// and the vmalert alerts) and the slack-bridge's pinned Agent Dashboard (the
// open-PR line).
//
// A run that is still going has not written its figures yet, so Latest returns
// nil for it and each reader keeps the last record it saw. The record's At
// says how old the figures are.
//
// The conflict figures come from the PR beads, not from the sweeper's own
// reading: the github-bridge conflict scanner writes mr_merge_state and
// mr_conflict_since (docs/pr-conflicts.md), and the sweeper counts those, using
// GitHub's mergeable only for a PR with no bead or a never-scanned one
// (gd-L9herzgeZF).
package prhealth

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Field is the agent-bead field the sweeper writes its figures to.
const Field = "pr_health"

// TriggerLabel marks the PR sweeper's trigger bead.
const TriggerLabel = "pr-sweeper"

// Train states the sweeper reports for the merge desk's train.
const (
	TrainNone        = "none"        // no open merge-train PR
	TrainPending     = "pending"     // open, checks still running
	TrainGreen       = "green"       // open, every check passed
	TrainRed         = "red"         // open, a check failed
	TrainConflicting = "conflicting" // open, conflicting with main
	TrainRevert      = "revert"      // a merge-train-revert PR is open
)

// TrainStates is every train state, in a fixed order for the state-set gauge.
var TrainStates = []string{TrainNone, TrainPending, TrainGreen, TrainRed, TrainConflicting, TrainRevert}

// Health is one sweeper run's count of the open PRs into main.
type Health struct {
	// At is when the sweeper counted.
	At time.Time `json:"at"`
	// Open is every open PR into main.
	Open int `json:"open"`
	// DeskQueue is the open PRs an open merge escalation names (rule S3).
	DeskQueue int `json:"desk_queue"`
	// Orphans is the open PRs with no merge escalation and no living owner
	// when the run classified them (rules C1, C2 and E1), before it escalated
	// or closed them. A healthy fleet keeps this near zero: gb stop and the
	// kd close guard should leave the sweeper nothing to find.
	Orphans int `json:"orphans"`
	// OldestOrphanHours is the age of the oldest orphan, from its createdAt;
	// 0 with no orphans.
	OldestOrphanHours float64 `json:"oldest_orphan_hours"`
	// OldestOpenHours is the age of the oldest open PR the sweeper considers
	// (every open PR except the desk's and humans', rule S1).
	OldestOpenHours float64 `json:"oldest_open_hours"`
	OldestOpenPR    int     `json:"oldest_open_pr,omitempty"`
	// Conflicting is the open PRs that conflict with main: mr_merge_state
	// conflicting on the PR's bead, or GitHub mergeable CONFLICTING for a PR
	// whose bead is missing or was never scanned. A record from before the
	// field reads as 0.
	Conflicting int `json:"conflicting"`
	// ConflictingUnscanned is how many of Conflicting were read from GitHub
	// because no bead carried mr_merge_state. It should stay near zero while
	// the scanner runs.
	ConflictingUnscanned int `json:"conflicting_unscanned"`
	// OldestConflictHours is the longest any open PR has conflicted, from its
	// bead's mr_conflict_since; 0 with none. A PR read from GitHub has no
	// start time and does not count here.
	OldestConflictHours float64 `json:"oldest_conflict_hours"`
	// Train is one of TrainStates.
	Train   string `json:"train"`
	TrainPR int    `json:"train_pr,omitempty"`
}

// Parse decodes a pr_health field value. Unknown train states read as "".
func Parse(raw string) (*Health, error) {
	var h Health
	if err := json.Unmarshal([]byte(raw), &h); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", Field, err)
	}
	if h.At.IsZero() {
		return nil, fmt.Errorf("parsing %s: no \"at\" time", Field)
	}
	h.Train = strings.ToLower(strings.TrimSpace(h.Train))
	known := false
	for _, s := range TrainStates {
		known = known || s == h.Train
	}
	if !known {
		h.Train = ""
	}
	return &h, nil
}

// Querier is the slice of the beads client Latest needs.
type Querier interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
}

// Latest reads the figures of the sweeper trigger's last run. It returns nil,
// nil when there is no sweeper trigger, it has not run, or its last run has not
// written its figures yet.
func Latest(ctx context.Context, q Querier) (*Health, error) {
	res, err := q.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"trigger"},
		Labels:   []string{TriggerLabel},
		Statuses: []string{"open"},
		Limit:    10,
	})
	if err != nil {
		return nil, fmt.Errorf("listing the PR sweeper trigger: %w", err)
	}
	// One sweeper is expected; with more, the one that fired last wins.
	triggers := res.Beads
	sort.SliceStable(triggers, func(i, j int) bool {
		return triggers[i].Fields["last_run"] > triggers[j].Fields["last_run"]
	})
	for _, t := range triggers {
		agentID := t.Fields["last_agent_id"]
		if agentID == "" {
			continue
		}
		agent, err := q.GetBead(ctx, agentID)
		if err != nil {
			return nil, fmt.Errorf("reading the PR sweeper's last run %s: %w", agentID, err)
		}
		raw := agent.Fields[Field]
		if raw == "" {
			return nil, nil
		}
		return Parse(raw)
	}
	return nil, nil
}

// Line is the open-PR line for a Slack digest, e.g.
// "22 open PRs · 3 in the desk queue · 9 orphaned (oldest 5.2d) · 4 conflicting (oldest 7h) · train green #1570 · as of 14:17 UTC".
// The conflicting part is left out while no PR conflicts.
func (h Health) Line() string {
	parts := []string{
		fmt.Sprintf("%d open PRs", h.Open),
		fmt.Sprintf("%d in the desk queue", h.DeskQueue),
	}
	orphans := fmt.Sprintf("%d orphaned", h.Orphans)
	if h.Orphans > 0 {
		orphans += fmt.Sprintf(" (oldest %s)", Age(h.OldestOrphanHours))
	}
	parts = append(parts, orphans)
	if h.Conflicting > 0 {
		conflicting := fmt.Sprintf("%d conflicting", h.Conflicting)
		if h.OldestConflictHours > 0 {
			conflicting += fmt.Sprintf(" (oldest %s)", Age(h.OldestConflictHours))
		}
		parts = append(parts, conflicting)
	}
	train := h.Train
	if train == "" {
		train = "unknown"
	}
	if h.TrainPR > 0 && train != TrainNone {
		train += fmt.Sprintf(" #%d", h.TrainPR)
	}
	parts = append(parts, "train "+train)
	parts = append(parts, "as of "+h.At.UTC().Format("15:04 UTC"))
	return strings.Join(parts, " · ")
}

// Age formats hours as "5h" under two days and "3.1d" from two days on.
func Age(hours float64) string {
	if hours < 48 {
		return fmt.Sprintf("%.0fh", hours)
	}
	return fmt.Sprintf("%.1fd", hours/24)
}
