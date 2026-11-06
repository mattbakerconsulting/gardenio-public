package gardenhub

// farm_tasks.go — the row helpers the farm model is built with: a work bead's
// labels, project and phase order, its farm state and MR, the per-state
// counts, and the two orders a bed keeps its tasks in (moved out of farm.go,
// gd-QboM9sPYCg).

import (
	"encoding/json"
	"sort"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

func hasLabel(b *beadsapi.BeadDetail, want string) bool {
	for _, l := range b.Labels {
		if l == want {
			return true
		}
	}
	return false
}

func projectOf(b *beadsapi.BeadDetail) string {
	for _, l := range b.Labels {
		if strings.HasPrefix(l, "project:") {
			return strings.TrimPrefix(l, "project:")
		}
	}
	return ""
}

func phaseOrderOf(b *beadsapi.BeadDetail) []string {
	raw := strings.TrimSpace(b.Fields["phase_order"])
	if raw == "" {
		return nil
	}
	var ids []string
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return nil
	}
	seen := map[string]bool{}
	out := ids[:0]
	for _, id := range ids {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// taskState derives the farm state of a work bead.
func taskState(b *beadsapi.BeadDetail, mr *FarmMR) string {
	switch b.Status {
	case "deferred":
		return TaskDeferred
	case "blocked":
		return TaskBlocked
	}
	if mr != nil && mr.State != "merged" && mr.State != "closed" {
		return TaskReview
	}
	if b.Status == "in_progress" || b.Status == "partial" {
		return TaskInProgress
	}
	return TaskOpen
}

func mrOf(b *beadsapi.BeadDetail) *FarmMR {
	u := strings.TrimSpace(b.Fields["mr_url"])
	if u == "" {
		return nil
	}
	return &FarmMR{URL: u, IID: b.Fields["mr_iid"], State: b.Fields["mr_state"]}
}

func (c *FarmCounts) add(state string) {
	switch state {
	case TaskInProgress:
		c.InProgress++
	case TaskReview:
		c.Review++
	case TaskBlocked:
		c.Blocked++
	case TaskDeferred:
		c.Deferred++
	default:
		c.Open++
	}
}

func (c *FarmCounts) sum(o FarmCounts) {
	c.InProgress += o.InProgress
	c.Review += o.Review
	c.Blocked += o.Blocked
	c.Open += o.Open
	c.Deferred += o.Deferred
	c.Closed += o.Closed
}

var stateRank = map[string]int{TaskInProgress: 0, TaskReview: 1, TaskBlocked: 2, TaskOpen: 3, TaskDeferred: 4}

// sortTasks ranks tasks in flight first (state, priority, newest change, id):
// the order the per-phase cap keeps them in, so a moving task is never the one
// left out (FarmPhase.More). It is not the order the bed plants them in; see
// sortPlanted.
func sortTasks(ts []FarmTask) {
	sort.Slice(ts, func(i, j int) bool {
		a, b := ts[i], ts[j]
		if ra, rb := stateRank[a.State], stateRank[b.State]; ra != rb {
			return ra < rb
		}
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		if a.Updated != b.Updated {
			return a.Updated > b.Updated
		}
		return a.ID < b.ID
	})
}

// sortPlanted orders the listed tasks as the bed plants them: by created, then
// id. Neither changes while a bead lives, so a task keeps its cell through
// every status, priority and updated_at change and moves only when an older
// sibling leaves the bed (gd-wDdun2AVwq). world.js plants the listed closed
// steps among them by the same key.
func sortPlanted(ts []FarmTask) {
	sort.Slice(ts, func(i, j int) bool {
		a, b := ts[i], ts[j]
		if a.Created != b.Created {
			return a.Created < b.Created
		}
		return a.ID < b.ID
	})
}
