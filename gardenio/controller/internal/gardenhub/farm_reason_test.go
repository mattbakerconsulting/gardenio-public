package gardenhub

import (
	"context"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// TestGardenerReason is one fixture agent bead per reason kind (and the ones
// that carry none), with the open decisions and escalations they may wait on.
func TestGardenerReason(t *testing.T) {
	now := time.Unix(baseUnix+100_000, 0)
	ago := func(d time.Duration) string { return now.Add(-d).UTC().Format(time.RFC3339) }
	handle := func(id, typ, title string, fields map[string]string) *beadsapi.BeadDetail {
		b := fb(id, typ, "open", "", 1)
		b.Title, b.CreatedBy = title, "someone@example.com"
		for k, v := range fields {
			b.Fields[k] = v
		}
		return b
	}
	park := buildParkIndex([]*beadsapi.BeadDetail{
		handle("gd-dById", "decision", "Which IdP?", map[string]string{"requesting_agent_bead_id": "gd-r1"}),
		handle("gd-eByName", "escalation", "Merge PR #700", map[string]string{"requesting_agent": "r-name", "audience": "owner"}),
		handle("gd-eByTask", "escalation", "Deploy 2026.293.5", map[string]string{"requesting_task": "gd-T9", "audience": "owner"}),
		// The desk's or the operators' (gd-bmOiteQi36): waiting, but not on the owner, so nobody is parked on it.
		handle("gd-eDesk", "escalation", "Merge PR #701", map[string]string{"requesting_agent": "r-desk", "operation_type": "gitlab_mr_merge"}),
		handle("gd-eOps", "escalation", "Apply the plan", map[string]string{"requesting_task": "gd-T8", "operation_type": "terraform_apply"}),
		// Answered or approved: the human part is over, so nobody is parked on it.
		handle("gd-dDone", "decision", "Answered", map[string]string{"requesting_agent_bead_id": "gd-r4", "chosen": "a"}),
		handle("gd-eOK", "escalation", "Approved", map[string]string{"requesting_agent": "gd-r4", "approved_at": ago(time.Minute)}),
		// Not a handle at all.
		handle("gd-tTask", "task", "Not a handle", map[string]string{"requesting_task": "gd-T4"}),
	})

	agent := func(id, state string, fields map[string]string) *beadsapi.BeadDetail {
		b := agentBead(id, "n-"+id, "crew", state, "", 0)
		b.UpdatedAt = now.Add(-time.Minute)
		for k, v := range fields {
			b.Fields[k] = v
		}
		return b
	}
	none := ""
	cases := []struct {
		name             string
		b                *beadsapi.BeadDetail
		kind, text, href string
	}{
		{"working", agent("gd-w", "working", nil), none, "", ""},
		{"idle", agent("gd-i", "idle", nil), none, "", ""},

		{"parked: decision by agent bead id", agent("gd-r1", "yielding", nil), ReasonParked, "Waiting on Which IdP?", "/garden?bead=gd-dById"},
		{"parked: escalation by agent name", agent("gd-r2", "idle", map[string]string{"agent": "r-name"}), ReasonParked, "Waiting on Merge PR #700", "/garden?bead=gd-eByName"},
		{"parked: handle parks its task", agent("gd-r3", "yielding", map[string]string{"task_id": "gd-T9"}), ReasonParked, "Waiting on Deploy 2026.293.5", "/garden?bead=gd-eByTask"},
		{"working past its handle is not parked", agent("gd-r1", "working", nil), none, "", ""},
		{"working but quiet on its handle is parked", agent("gd-r1", "working", map[string]string{"last_progress_at": ago(2 * time.Hour)}), ReasonParked, "Waiting on Which IdP?", "/garden?bead=gd-dById"},
		{"an answered or approved handle parks nobody", agent("gd-r4", "yielding", nil), none, "", ""},
		{"a task is not a handle", agent("gd-r5", "yielding", map[string]string{"task_id": "gd-T4"}), none, "", ""},
		{"a desk escalation parks nobody", agent("gd-r6", "yielding", map[string]string{"agent": "r-desk"}), none, "", ""},
		{"an operator escalation parks nobody", agent("gd-r7", "yielding", map[string]string{"task_id": "gd-T8"}), none, "", ""},

		{"down: failed", agent("gd-d1", "failed", nil), ReasonDown, "Failed", ""},
		{"down: failed with close_reason", agent("gd-d2", "failed", map[string]string{"close_reason": "OOMKilled (exit 137)"}), ReasonDown, "Failed: OOMKilled (exit 137)", ""},
		{"down: out of budget", agent("gd-d3", "budget_exceeded", nil), ReasonDown, "Out of budget", ""},
		{"down: rate limited", agent("gd-d4", "rate_limited", nil), ReasonDown, "Rate limited", ""},
		{"down: pod failed", agent("gd-d5", "working", map[string]string{"pod_phase": "Failed"}), ReasonDown, "Pod failed", ""},
		{"down: pod lost", agent("gd-d6", "idle", map[string]string{"pod_phase": "unknown"}), ReasonDown, "Pod lost", ""},
		{"down: no room in the cluster", agent("gd-d9", "spawning", map[string]string{"pod_scheduling_blocked": "true"}), ReasonDown, "No room in the cluster", ""},
		{"down: reaped for no room", agent("gd-d10", "failed", map[string]string{"pod_scheduling_blocked": "true", "close_reason": "stale: unschedulable, cluster capacity"}), ReasonDown, "Failed: stale: unschedulable, cluster capacity", ""},
		{"down: wrap-up blockers", agent("gd-d7", "idle", map[string]string{"wrapup": `{"tldr":"x","blockers":"Needs the  RBAC grant\nfor gc-k8s"}`}), ReasonDown, "Blocked: Needs the RBAC grant for gc-k8s", ""},
		{"down beats parked", agent("gd-r1", "failed", nil), ReasonDown, "Failed", ""},
		{"an alive agent is not judged by its last wrap-up", agent("gd-d8", "working", map[string]string{"wrapup": `{"blockers":"old","restart_when":"when X"}`}), none, "", ""},

		{"sleeping: dormant", agent("gd-s1", beadsapi.AgentStateDormant, nil), ReasonSleeping, "Sleeping", ""},
		{"sleeping: restart_when", agent("gd-s2", "idle", map[string]string{"wrapup": `{"tldr":"x","restart_when":"when ka-X merges"}`}), ReasonSleeping, "Sleeping until ka-X merges", ""},
		{"sleeping: restart_when as written", agent("gd-s3", "", map[string]string{"wrapup": `{"restart_when":"the operator resolves gd-Y"}`}), ReasonSleeping, "Sleeping until the operator resolves gd-Y", ""},
		{"parked beats sleeping", agent("gd-r1", "idle", map[string]string{"wrapup": `{"restart_when":"when gd-dById resolves"}`}), ReasonParked, "Waiting on Which IdP?", "/garden?bead=gd-dById"},

		{"stale: last_progress_at", agent("gd-x1", "working", map[string]string{"last_progress_at": ago(3*time.Hour + 59*time.Minute)}), ReasonStale, "No progress for 3h", ""},
		{"stale: updated_at when no progress stamp", func() *beadsapi.BeadDetail {
			b := agent("gd-x2", "yielding", nil)
			b.UpdatedAt = now.Add(-time.Hour)
			return b
		}(), ReasonStale, "No progress for 1h", ""},
		{"fresh progress beats an old updated_at", func() *beadsapi.BeadDetail {
			b := agent("gd-x3", "working", map[string]string{"last_progress_at": ago(time.Minute)})
			b.UpdatedAt = now.Add(-5 * time.Hour)
			return b
		}(), none, "", ""},
		{"only an alive agent goes stale", func() *beadsapi.BeadDetail {
			b := agent("gd-x4", "idle", nil)
			b.UpdatedAt = now.Add(-5 * time.Hour)
			return b
		}(), none, "", ""},
		{"stale beats starting", agent("gd-x5", "spawning", map[string]string{"last_progress_at": ago(2 * time.Hour)}), ReasonStale, "No progress for 2h", ""},

		{"starting: spawning", agent("gd-g1", "spawning", nil), ReasonStarting, "Starting up", ""},
		{"starting: assigning", agent("gd-g2", "assigning", nil), ReasonStarting, "Getting its task", ""},
		{"starting: queued", agent("gd-g3", "queued", nil), ReasonStarting, "Waiting for a key in the account pool", ""},
	}
	home := func(string) *FarmPlot { return nil }
	title := func(string) string { return "" }
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := gardenerOf(c.b, home, title, title, park, now).Reason
			if c.kind == "" {
				if r != nil {
					t.Fatalf("reason = %+v, want null", *r)
				}
				return
			}
			if r == nil || r.Kind != c.kind || r.Text != c.text || r.Href != c.href {
				t.Fatalf("reason = %+v, want {%s %q %q}", r, c.kind, c.text, c.href)
			}
		})
	}
}

// TestGardenerReasonClip cuts a long reason at maxReasonText runes.
func TestGardenerReasonClip(t *testing.T) {
	long := make([]byte, 400)
	for i := range long {
		long[i] = 'x'
	}
	b := agentBead("gd-long", "n", "crew", "idle", "", 0)
	b.Fields["wrapup"] = `{"blockers":"` + string(long) + `"}`
	none := func(string) string { return "" }
	r := gardenerOf(b, func(string) *FarmPlot { return nil }, none, none, parkIndex{}, time.Unix(baseUnix, 0)).Reason
	if r == nil || len([]rune(r.Text)) != maxReasonText {
		t.Fatalf("reason = %+v, want %d runes", r, maxReasonText)
	}
}

// TestFarmGardenerReasons is the fixture farm's reasons on the wire: the
// yielding gardener is parked on the decision it filed, the crash is down,
// the one standing on a phase is starting, and the working ones carry none.
func TestFarmGardenerReasons(t *testing.T) {
	f := newTestFarm(fixtureWorld())
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := decodeSnap(t, f)
	want := map[string]*FarmReason{
		"gd-gAsk":   {Kind: ReasonParked, Text: "Waiting on Where does the chat live?", Href: "/garden?bead=gd-A2f"},
		"gd-gDown":  {Kind: ReasonDown, Text: "Failed"},
		"gd-gPhase": {Kind: ReasonStarting, Text: "Starting up"},
		"gd-gWork1": nil,
		"gd-gIdle":  nil,
	}
	for id, w := range want {
		g := findGardener(s, id)
		if g == nil {
			t.Fatalf("%s missing", id)
		}
		if (g.Reason == nil) != (w == nil) || (w != nil && *g.Reason != *w) {
			t.Errorf("%s reason = %+v, want %+v", id, g.Reason, w)
		}
		// the workbench's agent head reads the same reason (GardenerReason, commons Handler.Reason)
		if kind, text := f.GardenerReason(id); (w == nil && kind+text != "") || (w != nil && (kind != w.Kind || text != w.Text)) {
			t.Errorf("GardenerReason(%s) = %q %q, want %+v", id, kind, text, w)
		}
	}
	if kind, text := f.GardenerReason("gd-nope"); kind+text != "" {
		t.Errorf("GardenerReason of no gardener = %q %q", kind, text)
	}
}
