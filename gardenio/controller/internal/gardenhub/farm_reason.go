package gardenhub

// farm_reason.go — why a gardener is not simply working (gd-TvJosTKfAt). The
// design is gardenio/docs/homestead-agents-as-people.md section 2.3: the words
// are built here so every client says the same thing. Everything comes from
// the agent bead and the open decisions and escalations the farm already
// read; nothing comes from kubectl.

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Reason kinds (FarmReason.Kind). A working gardener has no reason (null).
const (
	ReasonDown     = "down"     // stuck state, a failed, lost or unschedulable pod, or a wrap-up with blockers
	ReasonParked   = "parked"   // waiting on an open decision or escalation it filed
	ReasonSleeping = "sleeping" // dormant, or stopped with a wake condition
	ReasonStale    = "stale"    // alive in name, but no progress for GardenerStaleAfter
	ReasonStarting = "starting" // spawning, assigning, or queued for a key
)

// GardenerStaleAfter is beads-viewer's agentStaleThreshold, the age past which
// /agents calls an alive agent stale. TestAgentBucketMatchesGardenhub there
// pins the copy.
const GardenerStaleAfter = time.Hour

const maxReasonText = 140 // runes of FarmReason.Text

var (
	stuckWords = map[string]string{"failed": "Failed", "rate_limited": "Rate limited", "budget_exceeded": "Out of budget"}
	podWords   = map[string]string{"failed": "Pod failed", "unknown": "Pod lost"}
	startWords = map[string]string{"spawning": "Starting up", "assigning": "Getting its task", "queued": "Waiting for a key in the account pool"}
)

// parkIndex is the open decisions and escalations still waiting on a human,
// by the agent that filed them (bead id or name) and by the task they park
// (requesting_task). Where several match, the oldest wins.
type parkIndex struct {
	byAgent, byTask map[string]*beadsapi.BeadDetail
}

// buildParkIndex indexes the open work rows the farm read. A handle past the
// row cap is not seen, and its gardener carries no parked reason.
func buildParkIndex(rows []*beadsapi.BeadDetail) parkIndex {
	idx := parkIndex{byAgent: map[string]*beadsapi.BeadDetail{}, byTask: map[string]*beadsapi.BeadDetail{}}
	put := func(m map[string]*beadsapi.BeadDetail, key string, b *beadsapi.BeadDetail) {
		if key = strings.TrimSpace(key); key == "" {
			return
		}
		if old := m[key]; old == nil || b.CreatedAt.Before(old.CreatedAt) || (b.CreatedAt.Equal(old.CreatedAt) && b.ID < old.ID) {
			m[key] = b
		}
	}
	for _, b := range rows {
		if !waitsOnHuman(b) {
			continue
		}
		f := b.Fields
		for _, ref := range []string{f["requesting_agent_bead_id"], f["requesting_agent"], f["requested_by"], b.CreatedBy} {
			put(idx.byAgent, ref, b)
		}
		put(idx.byTask, f["requesting_task"], b)
	}
	return idx
}

// waitsOnHuman says whether b is an open decision or escalation waiting on the
// owner. Only such a wait parks a gardener: one on an escalation the desk or
// the operators have is no "needs you" (gd-bmOiteQi36).
func waitsOnHuman(b *beadsapi.BeadDetail) bool {
	if b == nil || (b.Type != "decision" && b.Type != "escalation") {
		return false
	}
	w := waitOf(b, func(string) (string, int) { return "", partyPerson })
	return (w.State == WaitUnanswered || w.State == WaitPending) && w.Human
}

// handleFor is the open handle a gardener waits on: one it filed, else one
// parking its task.
func (p parkIndex) handleFor(id, name, task string) *beadsapi.BeadDetail {
	if h := p.byAgent[id]; h != nil {
		return h
	}
	if h := p.byAgent[name]; name != "" && h != nil {
		return h
	}
	if task != "" {
		return p.byTask[task]
	}
	return nil
}

// reasonOf says why the gardener g (projected from b) is not simply working,
// or nil. The first that holds wins: down, parked, sleeping, stale, starting.
// A wrap-up counts only while the agent is not alive, so a respawned agent is
// not judged by its last run's blockers or wake condition.
func reasonOf(b *beadsapi.BeadDetail, g FarmGardener, park parkIndex, now time.Time) *FarmReason {
	var wrap struct {
		Blockers    string `json:"blockers"`
		RestartWhen string `json:"restart_when"`
	}
	if g.Bucket != GardenerAlive {
		_ = json.Unmarshal([]byte(strings.TrimSpace(b.Fields[beadsapi.WrapUpFieldName])), &wrap)
	}
	blockers, restart := strings.TrimSpace(wrap.Blockers), strings.TrimSpace(wrap.RestartWhen)

	head := stuckWords[g.State]
	if head == "" {
		head = podWords[lower(b.Fields["pod_phase"])]
	}
	if head == "" && b.Fields["pod_scheduling_blocked"] == "true" {
		head = "No room in the cluster" // its pod is Pending, unschedulable (statusreporter); the stale reaper ends it
	}
	if head != "" || blockers != "" {
		detail := firstNonEmpty(blockers, strings.TrimSpace(b.Fields[beadsapi.FieldCloseReason]))
		switch {
		case head == "":
			head = "Blocked"
		case detail == "":
			return &FarmReason{Kind: ReasonDown, Text: head}
		}
		return &FarmReason{Kind: ReasonDown, Text: clip(head+": "+detail, maxReasonText)}
	}

	idle := progressIdle(b, now)
	stale := g.Bucket == GardenerAlive && idle >= GardenerStaleAfter

	// A working agent that filed a handle has moved on, unless it has gone
	// quiet since: then it is waiting on it all the same.
	if h := park.handleFor(b.ID, g.Agent, g.Task); h != nil && (g.State != "working" || stale) {
		return &FarmReason{Kind: ReasonParked, Text: clip("Waiting on "+h.Title, maxReasonText), Href: beadHref("", h.ID)}
	}

	if g.State == beadsapi.AgentStateDormant || restart != "" {
		text := "Sleeping"
		if restart != "" {
			text = "Sleeping until " + trimWakePrefix(restart)
		}
		return &FarmReason{Kind: ReasonSleeping, Text: clip(text, maxReasonText)}
	}
	if stale {
		return &FarmReason{Kind: ReasonStale, Text: fmt.Sprintf("No progress for %dh", int(idle/time.Hour))}
	}
	if w := startWords[g.State]; w != "" {
		return &FarmReason{Kind: ReasonStarting, Text: w}
	}
	return nil
}

// progressIdle is how long agent bead b has gone without progress: since last_progress_at, else its last update, else 0.
// The farm calls an alive gardener idle this long stale (reasonOf); the garden's Restart checks it again (wake.go).
func progressIdle(b *beadsapi.BeadDetail, now time.Time) time.Duration {
	last := fieldUnix(b.Fields["last_progress_at"])
	if last <= 0 && !b.UpdatedAt.IsZero() {
		last = b.UpdatedAt.Unix()
	}
	if last <= 0 {
		return 0
	}
	return now.Sub(time.Unix(last, 0))
}

// trimWakePrefix drops a leading "when " or "until " from a restart_when, so
// "when ka-X merges" reads "Sleeping until ka-X merges".
func trimWakePrefix(s string) string {
	for _, p := range []string{"when ", "until "} {
		if len(s) > len(p) && strings.EqualFold(s[:len(p)], p) {
			return strings.TrimSpace(s[len(p):])
		}
	}
	return s
}
