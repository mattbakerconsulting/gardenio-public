package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"gardenio/controller/internal/beadsapi"
)

func TestBuildActiveAgents_FiltersTerminalStates(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-30 * time.Second) // within the freshness window
	beads := []*beadsapi.BeadDetail{
		{ID: "running", UpdatedAt: fresh, Fields: map[string]string{"agent": "Agent-A", "agent_state": "working", "spawned_by": "gardener"}},
		{ID: "done", UpdatedAt: fresh, Fields: map[string]string{"agent": "Agent-B", "agent_state": "done"}},
		{ID: "failed", UpdatedAt: fresh, Fields: map[string]string{"agent": "Agent-C", "agent_state": "failed"}},
		{ID: "rate-limited", UpdatedAt: fresh, Fields: map[string]string{"agent": "Agent-D", "agent_state": "rate_limited"}},
		{ID: "pod-failed", UpdatedAt: fresh, Fields: map[string]string{"agent": "Agent-E", "pod_phase": "Failed"}},
		{ID: "pod-succeeded", UpdatedAt: fresh, Fields: map[string]string{"agent": "Agent-F", "pod_phase": "Succeeded"}},
		{ID: "spawning", UpdatedAt: fresh, Fields: map[string]string{"agent": "Agent-G", "agent_state": "spawning"}},
		{ID: "no-state", UpdatedAt: fresh, Fields: map[string]string{"agent": "Agent-H"}},
		{ID: "no-name", UpdatedAt: fresh, Fields: map[string]string{"agent_state": "working"}},
	}

	active, spawners := buildActiveAgents(beads, now, newsAliveFreshnessWindow)

	// Only Agent-A (working), Agent-G (spawning), and Agent-H (no state) should be active.
	wantActive := map[string]bool{
		"agent-a": true,
		"agent-g": true,
		"agent-h": true,
	}
	if len(active) != len(wantActive) {
		t.Fatalf("active agents: got %v, want %v", active, wantActive)
	}
	for name := range wantActive {
		if !active[name] {
			t.Errorf("expected %q to be active", name)
		}
	}
	// Terminal agents must not appear.
	for _, name := range []string{"agent-b", "agent-c", "agent-d", "agent-e", "agent-f"} {
		if active[name] {
			t.Errorf("expected %q to be filtered out (terminal state)", name)
		}
	}
	// Spawner should be recorded for agent-a.
	if spawners["agent-a"] != "gardener" {
		t.Errorf("spawner for agent-a: got %q, want %q", spawners["agent-a"], "gardener")
	}
}

// TestBuildActiveAgents_DespawnedAgentNotAlive_WithLiveControl is the
// regression test for ka-0000000637: an agent that has despawned (its bead
// lingers open at agent_state=working because its pod died without a clean
// in-pod gb stop, so the controller stopped refreshing updated_at) must NOT be
// reported alive once its bead ages past the freshness window.
//
// Per advice ka-0000002586, a field that reads the same on working and broken
// instances is not evidence. The assertion therefore includes a KNOWN-LIVE
// CONTROL agent in the SAME call: a freshly-updated working agent. The test
// only passes if the check DISCRIMINATES — control alive, despawned not — so it
// cannot pass by reporting everything dead (a fixed `return false` fails the
// control assertion) or everything alive (the pre-fix agent_state-only code
// fails the despawned assertion).
func TestBuildActiveAgents_DespawnedAgentNotAlive_WithLiveControl(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	window := newsAliveFreshnessWindow

	beads := []*beadsapi.BeadDetail{
		// CONTROL — genuinely live: working, refreshed by the controller within
		// the last sync cadence. Ground truth: pod exists.
		{ID: "gd-live", UpdatedAt: now.Add(-30 * time.Second),
			Fields: map[string]string{"agent": "live-control", "agent_state": "working", "spawned_by": "gardener"}},
		// SUBJECT — despawned: still open at agent_state=working (the pod died
		// without closing the bead), but the controller has not touched it since
		// well before the freshness window. Ground truth: pod is gone. This is
		// the exact shape observed in ka-0000000637.
		{ID: "gd-despawned", UpdatedAt: now.Add(-30 * time.Minute),
			Fields: map[string]string{"agent": "despawned-ghost", "agent_state": "working", "spawned_by": "gardener"}},
	}

	active, _ := buildActiveAgents(beads, now, window)

	// The control MUST read alive — otherwise the check is vacuously "everything
	// is dead" and proves nothing.
	if !active["live-control"] {
		t.Fatalf("known-live control agent must be reported alive; got active=%v", active)
	}
	// The despawned agent MUST NOT read alive — this is the bug.
	if active["despawned-ghost"] {
		t.Errorf("despawned agent (open+working but stale updated_at, no pod) must not be reported alive; got active=%v", active)
	}
}

// TestBuildActiveAgents_FreshnessWindowBoundary pins the exact reconciliation
// bound documented on newsAliveFreshnessWindow: an agent updated exactly at the
// window edge is still alive; one past it is not. This is the "bounded by a
// stated, short reconciliation window" guarantee (acceptance item 1).
func TestBuildActiveAgents_FreshnessWindowBoundary(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	window := newsAliveFreshnessWindow

	beads := []*beadsapi.BeadDetail{
		{ID: "gd-atedge", UpdatedAt: now.Add(-window),
			Fields: map[string]string{"agent": "at-edge", "agent_state": "working"}},
		{ID: "gd-pastedge", UpdatedAt: now.Add(-window - time.Second),
			Fields: map[string]string{"agent": "past-edge", "agent_state": "working"}},
	}

	active, _ := buildActiveAgents(beads, now, window)
	if !active["at-edge"] {
		t.Errorf("agent updated exactly at the window edge must still be alive; got active=%v", active)
	}
	if active["past-edge"] {
		t.Errorf("agent updated past the window edge must not be alive; got active=%v", active)
	}
}

// TestBuildActiveAgents_FreshnessDisabledAndFailOpen verifies the two
// safety valves: freshWindow <= 0 disables the bound entirely (restores the
// prior agent_state-only behavior), and a zero/absent updated_at fails OPEN
// (treated as fresh) so a daemon-side timestamp glitch never hides a live
// agent.
func TestBuildActiveAgents_FreshnessDisabledAndFailOpen(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)

	stale := &beadsapi.BeadDetail{ID: "gd-stale", UpdatedAt: now.Add(-30 * time.Minute),
		Fields: map[string]string{"agent": "stale-agent", "agent_state": "working"}}
	noTimestamp := &beadsapi.BeadDetail{ID: "gd-nots", // zero UpdatedAt
		Fields: map[string]string{"agent": "no-timestamp-agent", "agent_state": "working"}}

	// freshWindow <= 0 disables the bound: even a very stale bead is alive.
	activeDisabled, _ := buildActiveAgents([]*beadsapi.BeadDetail{stale}, now, 0)
	if !activeDisabled["stale-agent"] {
		t.Errorf("freshWindow<=0 must disable the bound (stale agent alive); got active=%v", activeDisabled)
	}

	// A zero updated_at must fail open (alive) rather than be dropped.
	activeFailOpen, _ := buildActiveAgents([]*beadsapi.BeadDetail{noTimestamp}, now, newsAliveFreshnessWindow)
	if !activeFailOpen["no-timestamp-agent"] {
		t.Errorf("zero updated_at must fail open (agent alive); got active=%v", activeFailOpen)
	}
}

// TestBuildActiveAgents_ProgressBound verifies the progress-liveness drop
// (ka-0000000286): an agent whose bead is FRESHLY updated (so the updated_at
// freshWindow passes — exactly what a livelock produces, since the rate-limit
// park cycle rewrites the bead every backoff) but whose last_progress_at is old
// must NOT be reported alive, so the gardener-side b2 dedup stops suppressing
// re-dispatch of the bead it squats. A working agent WITH fresh progress stays
// alive, and an agent with no last_progress_at fails open (alive).
func TestBuildActiveAgents_ProgressBound(t *testing.T) {
	now := time.Date(2026, 8, 7, 18, 0, 0, 0, time.UTC)

	// Livelocked zombie: updated_at is fresh (park cycle keeps touching the bead)
	// but no genuine turn has landed in 12h.
	zombie := &beadsapi.BeadDetail{ID: "gd-zombie", UpdatedAt: now.Add(-30 * time.Second),
		Fields: map[string]string{"agent": "zombie-agent", "agent_state": "working",
			"last_progress_at": now.Add(-12 * time.Hour).Format(time.RFC3339)}}
	// Healthy crew: fresh updated_at AND fresh progress.
	healthy := &beadsapi.BeadDetail{ID: "gd-healthy", UpdatedAt: now.Add(-30 * time.Second),
		Fields: map[string]string{"agent": "healthy-agent", "agent_state": "working",
			"last_progress_at": now.Add(-2 * time.Minute).Format(time.RFC3339)}}
	// No progress signal (older controller / first turn pending): fails open.
	noSignal := &beadsapi.BeadDetail{ID: "gd-nosig", UpdatedAt: now.Add(-30 * time.Second),
		Fields: map[string]string{"agent": "nosignal-agent", "agent_state": "working"}}

	active, _ := buildActiveAgents([]*beadsapi.BeadDetail{zombie, healthy, noSignal}, now, newsAliveFreshnessWindow)

	if active["zombie-agent"] {
		t.Errorf("a progress-stalled agent (fresh updated_at, 12h-old last_progress_at) must NOT be alive; got active=%v", active)
	}
	if !active["healthy-agent"] {
		t.Errorf("a working agent with fresh progress must be alive; got active=%v", active)
	}
	if !active["nosignal-agent"] {
		t.Errorf("an agent with no last_progress_at must fail open (alive); got active=%v", active)
	}
}

func TestSplitActiveStale(t *testing.T) {
	active := map[string]bool{"alive-agent": true, "another-alive": true}
	beads := []*beadsapi.BeadDetail{
		{ID: "live-1", Assignee: "alive-agent"},
		{ID: "live-2", Assignee: "Alive-Agent"},        // case-insensitive match
		{ID: "unassigned", Assignee: "", Type: "task"}, // non-formula → unassigned bucket
		{ID: "stale-1", Assignee: "ghost-agent"},
		{ID: "stale-2", Assignee: "another-ghost"},
		{ID: "live-3", Assignee: "another-alive"},
	}

	liveIP, staleIP, unassignedIP := splitActiveStale(beads, active)

	gotLive := map[string]bool{}
	for _, b := range liveIP {
		gotLive[b.ID] = true
	}
	wantLive := map[string]bool{"live-1": true, "live-2": true, "live-3": true}
	if len(gotLive) != len(wantLive) {
		t.Fatalf("live: got %v, want %v", gotLive, wantLive)
	}
	for id := range wantLive {
		if !gotLive[id] {
			t.Errorf("expected %q in live set", id)
		}
	}

	gotStale := map[string]bool{}
	for _, b := range staleIP {
		gotStale[b.ID] = true
	}
	wantStale := map[string]bool{"stale-1": true, "stale-2": true}
	if len(gotStale) != len(wantStale) {
		t.Fatalf("stale: got %v, want %v", gotStale, wantStale)
	}
	for id := range wantStale {
		if !gotStale[id] {
			t.Errorf("expected %q in stale set", id)
		}
	}

	if len(unassignedIP) != 1 || unassignedIP[0].ID != "unassigned" {
		t.Errorf("expected unassigned bead in unassigned set: got %+v", unassignedIP)
	}
}

func TestSplitActiveStale_EmptyInputs(t *testing.T) {
	liveIP, staleIP, unassignedIP := splitActiveStale(nil, nil)
	if len(liveIP) != 0 || len(staleIP) != 0 || len(unassignedIP) != 0 {
		t.Fatalf("empty input: got live=%d stale=%d unassigned=%d, want 0/0/0", len(liveIP), len(staleIP), len(unassignedIP))
	}

	// No active agents — every assigned bead is stale; unassigned goes to
	// its own bucket and is NOT silently flagged as "in-progress by others".
	beads := []*beadsapi.BeadDetail{
		{ID: "a", Assignee: "x"},
		{ID: "b", Assignee: "", Type: "task"},
	}
	liveIP, staleIP, unassignedIP = splitActiveStale(beads, map[string]bool{})
	if len(liveIP) != 0 {
		t.Errorf("expected no live beads: got %v", liveIP)
	}
	if len(staleIP) != 1 || staleIP[0].ID != "a" {
		t.Errorf("expected assigned bead in stale: got %v", staleIP)
	}
	if len(unassignedIP) != 1 || unassignedIP[0].ID != "b" {
		t.Errorf("expected unassigned bead in unassigned bucket: got %v", unassignedIP)
	}
}

// captureStdout runs fn with os.Stdout redirected and returns what was written.
func captureStdout(t *testing.T, fn func() error) []byte {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = orig })

	done := make(chan []byte, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.Bytes()
	}()

	if err := fn(); err != nil {
		t.Fatalf("fn: %v", err)
	}
	_ = w.Close()
	return <-done
}

// rosterFromMaps is a test helper that reconstructs a newsAgentInfo roster from
// the legacy (active-set, spawners) maps emitNewsJSON used to take. Every entry
// is marked alive=true (the active set only ever held live agents) so the JSON
// agents contract is preserved.
func rosterFromMaps(active map[string]bool, spawners map[string]string) []newsAgentInfo {
	var roster []newsAgentInfo
	for name := range active {
		roster = append(roster, newsAgentInfo{Name: name, Alive: true, SpawnedBy: spawners[name]})
	}
	return roster
}

func TestEmitNewsJSON_Shape(t *testing.T) {
	active := map[string]bool{"alive-agent": true, "gardener": true}
	spawners := map[string]string{"alive-agent": "gardener"}
	activeIP := []*beadsapi.BeadDetail{
		{ID: "gd-1", Type: "task", Title: "live work", Assignee: "alive-agent"},
	}
	staleIP := []*beadsapi.BeadDetail{
		{ID: "gd-2", Type: "task", Title: "zombie", Assignee: "ghost-agent"},
	}
	closed := []*beadsapi.BeadDetail{
		{ID: "gd-3", Type: "bug", Title: "recent fix", Assignee: "alive-agent"},
	}
	decisions := []*beadsapi.BeadDetail{
		{ID: "gd-d1", Title: "fallback", Fields: map[string]string{
			"prompt":       "do A or B?",
			"chosen":       "a",
			"options":      `[{"id":"a","short":"Option A"},{"id":"b","short":"Option B"}]`,
			"responded_by": "matt",
		}},
	}
	reports := []*beadsapi.BeadDetail{
		{ID: "gd-r1", Title: "post-mortem", CreatedBy: "alice", Fields: map[string]string{
			"report_type": "report",
			"decision_id": "gd-d1",
		}},
	}

	staleClass := classifyStale(staleIP, rosterFromMaps(active, spawners), nil)
	out := captureStdout(t, func() error {
		return emitNewsJSON("2h", rosterFromMaps(active, spawners), spawners, activeIP, staleIP, staleClass, nil, closed, decisions, reports, nil, 0, nil, 0, newsSectionTotals{}, nil, false, false, false)
	})

	var got newsJSONOutput
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}

	if got.Window != "2h" {
		t.Errorf("window: got %q, want 2h", got.Window)
	}

	// Agents: gardener + alive-agent (alive=true), ghost-agent (alive=false).
	aliveByName := map[string]bool{}
	for _, a := range got.Agents {
		aliveByName[a.Name] = a.Alive
	}
	if !aliveByName["alive-agent"] || !aliveByName["gardener"] {
		t.Errorf("expected alive-agent and gardener alive=true, got %+v", got.Agents)
	}
	if v, ok := aliveByName["ghost-agent"]; !ok || v {
		t.Errorf("expected ghost-agent alive=false, got %+v", got.Agents)
	}

	// Downstream gardener query contract:
	// `jq -r '.agents[] | select(.alive) | .name'`
	aliveNames := map[string]bool{}
	for _, a := range got.Agents {
		if a.Alive {
			aliveNames[a.Name] = true
		}
	}
	if !aliveNames["alive-agent"] || !aliveNames["gardener"] || aliveNames["ghost-agent"] {
		t.Errorf("alive-name set wrong: %v", aliveNames)
	}

	if len(got.InProgress) != 1 || got.InProgress[0].ID != "gd-1" || !got.InProgress[0].Alive {
		t.Errorf("in_progress: %+v", got.InProgress)
	}
	if got.InProgress[0].SpawnedBy != "gardener" {
		t.Errorf("spawned_by for live bead: %q", got.InProgress[0].SpawnedBy)
	}
	if len(got.Stale) != 1 || got.Stale[0].ID != "gd-2" || got.Stale[0].Alive {
		t.Errorf("stale: %+v", got.Stale)
	}
	if len(got.Closed) != 1 || got.Closed[0].ID != "gd-3" {
		t.Errorf("closed: %+v", got.Closed)
	}
	if len(got.Decisions) != 1 || got.Decisions[0].Chosen != "Option A" || got.Decisions[0].RespondedBy != "matt" {
		t.Errorf("decisions: %+v", got.Decisions)
	}
	if len(got.Artifacts) != 1 || got.Artifacts[0].DecisionID != "gd-d1" || got.Artifacts[0].CreatedBy != "alice" {
		t.Errorf("artifacts: %+v", got.Artifacts)
	}
}

func TestEmitNewsJSON_FilterFlags(t *testing.T) {
	active := map[string]bool{"a": true}
	activeIP := []*beadsapi.BeadDetail{{ID: "gd-live", Assignee: "a"}}
	staleIP := []*beadsapi.BeadDetail{{ID: "gd-dead", Assignee: "ghost"}}
	closed := []*beadsapi.BeadDetail{{ID: "gd-closed"}}
	decisions := []*beadsapi.BeadDetail{{ID: "gd-dec", Title: "x"}}
	reports := []*beadsapi.BeadDetail{{ID: "gd-rep", Title: "y"}}

	t.Run("stale-only", func(t *testing.T) {
		out := captureStdout(t, func() error {
			return emitNewsJSON("2h", rosterFromMaps(active, nil), nil, activeIP, staleIP, nil, nil, closed, decisions, reports, nil, 0, nil, 0, newsSectionTotals{}, nil, true, false, false)
		})
		var got newsJSONOutput
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		if len(got.InProgress) != 0 {
			t.Errorf("stale-only should zero in_progress: %+v", got.InProgress)
		}
		if len(got.Stale) != 1 {
			t.Errorf("stale-only should keep stale: %+v", got.Stale)
		}
		if len(got.Closed) != 0 || len(got.Decisions) != 0 || len(got.Artifacts) != 0 {
			t.Errorf("stale-only should zero other sections: closed=%d dec=%d art=%d", len(got.Closed), len(got.Decisions), len(got.Artifacts))
		}
	})

	t.Run("active-only", func(t *testing.T) {
		out := captureStdout(t, func() error {
			return emitNewsJSON("2h", rosterFromMaps(active, nil), nil, activeIP, staleIP, nil, nil, closed, decisions, reports, nil, 0, nil, 0, newsSectionTotals{}, nil, false, true, false)
		})
		var got newsJSONOutput
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		if len(got.InProgress) != 1 {
			t.Errorf("active-only should keep in_progress: %+v", got.InProgress)
		}
		if len(got.Stale) != 0 {
			t.Errorf("active-only should zero stale: %+v", got.Stale)
		}
		if len(got.Closed) != 0 || len(got.Decisions) != 0 {
			t.Errorf("active-only should zero other sections")
		}
	})
}

// TestSplitActiveStale_DropsFormulaScaffolding is the regression for
// ka-0000002078: unassigned in_progress beads from a formula (type=molecule
// or with source_formula/formula_id) are "ready but not newsworthy" and must
// be dropped entirely from `gb news`. Genuinely-orphaned non-formula work
// still surfaces.
func TestSplitActiveStale_DropsFormulaScaffolding(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		{ID: "gd-mol", Type: "molecule", Assignee: ""},
		{ID: "gd-step-task", Type: "task", Assignee: "", Fields: map[string]string{"source_formula": "gd-fABC"}},
		{ID: "gd-step-formula-id", Type: "task", Assignee: "", Fields: map[string]string{"formula_id": "gd-fXYZ"}},
		{ID: "gd-orphan", Type: "task", Assignee: ""},
	}
	live, stale, unassigned := splitActiveStale(beads, nil)
	if len(live) != 0 || len(stale) != 0 {
		t.Fatalf("live/stale buckets unexpectedly populated: live=%v stale=%v", live, stale)
	}
	if len(unassigned) != 1 || unassigned[0].ID != "gd-orphan" {
		t.Errorf("expected only gd-orphan in unassigned, got %+v", unassigned)
	}
}

// TestEmitNewsJSON_UnassignedBucket asserts the JSON shape preserves the
// `unassigned` array for non-formula orphans. Formula scaffolding is dropped
// upstream by splitActiveStale (see TestSplitActiveStale_DropsFormulaScaffolding).
func TestEmitNewsJSON_UnassignedBucket(t *testing.T) {
	unassigned := []*beadsapi.BeadDetail{
		{ID: "gd-orphan", Type: "task", Title: "manually-spawned task", Assignee: ""},
	}
	out := captureStdout(t, func() error {
		return emitNewsJSON("2h", nil, nil, nil, nil, nil, unassigned, nil, nil, nil, nil, 0, nil, 0, newsSectionTotals{}, nil, false, false, false)
	})
	var got newsJSONOutput
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if len(got.InProgress) != 0 {
		t.Errorf("unassigned must not pollute in_progress: %+v", got.InProgress)
	}
	if len(got.Stale) != 0 {
		t.Errorf("unassigned must not appear under stale: %+v", got.Stale)
	}
	if len(got.Unassigned) != 1 || got.Unassigned[0].ID != "gd-orphan" {
		t.Errorf("unassigned bucket: got %+v, want gd-orphan", got.Unassigned)
	}
	if got.Unassigned[0].Alive {
		t.Errorf("unassigned bead should have alive=false: %+v", got.Unassigned[0])
	}
}

func TestEmitNewsJSON_AlwaysValidJSONWhenEmpty(t *testing.T) {
	out := captureStdout(t, func() error {
		return emitNewsJSON("2h", nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, 0, nil, 0, newsSectionTotals{}, nil, false, false, false)
	})
	var got newsJSONOutput
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("empty output must still be valid JSON: %v\n%s", err, out)
	}
	if got.Agents == nil || got.InProgress == nil || got.Stale == nil || got.Unassigned == nil || got.Closed == nil || got.Decisions == nil || got.Artifacts == nil || got.OpenDecisions == nil {
		t.Errorf("empty slices should serialize as [] not null: %+v", got)
	}
}

func TestDecisionBucketKey(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"mr id only", "MR !2130 merge ok?", "ids:!2130"},
		{"mr id with punctuation", "MR !2130 ok!?.", "ids:!2130"},
		{"mr id with whitespace", "MR  \t!2130\n\nready  to  merge", "ids:!2130"},
		{
			"mr id and bead id are sorted+deduped",
			"MR !2130 (kbeads migration 015 operators tables ka-0000008070) green and mergeable.",
			"ids:!2130,ka-0000008070",
		},
		{
			"multiple mr ids sorted",
			"merge-gate on 2 db-migration MRs (!2173 and !2172) ready",
			"ids:!2172,!2173",
		},
		{
			"bead id only (schedule reference)",
			"reaper-revival-review for ka-0000003531 (Gardenio Log Monitor Gardener)",
			"ids:ka-0000003531",
		},
		{
			"no ids falls back to prefix",
			"investigate the gardenio fanout phenomenon please?",
			"investigate the gardenio fanout phenomenon please",
		},
		{
			"no ids prefix truncated to 60 chars",
			"this prompt is intentionally extremely long to verify the prefix-fallback truncation still applies in the absence of any extractable identifier tokens",
			"this prompt is intentionally extremely long to verify the pr",
		},
		{
			"github-style hash id",
			"PR #4321 needs review before the freeze",
			"ids:#4321",
		},
		{
			"duplicate ids collapse to single token",
			"MR !2130 — MR !2130 again",
			"ids:!2130",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decisionBucketKey(c.in)
			if got != c.want {
				t.Errorf("decisionBucketKey(%q):\n  got:  %q\n  want: %q", c.in, got, c.want)
			}
		})
	}
}

// TestDecisionBucketKey_RephrasingCollidesByID is the regression case for
// ka-0000008159. The previous prefix-only key let gardeners fan out duplicate
// open decisions about the same MR / bead / schedule whenever the lead-in
// phrasing changed. Each row below is a real or realistic gardener rephrasing
// for one logical target — they MUST share a single bucket.
func TestDecisionBucketKey_RephrasingCollidesByID(t *testing.T) {
	groups := []struct {
		name    string
		prompts []string
	}{
		{
			// gd-9ERMGPsT2F / gd-k9Wz8FfXnB and ~10 siblings (2026-05-06).
			name: "gardener-delivery merge-gate on MR !2173",
			prompts: []string{
				"two green kbeads db-migration MRs ready (orphan crew despawned). MR !2173 needs operator merge.",
				"gardener-delivery: merge-gate on db-migration MR (!2173)",
				"MR !2173 green & mergeable — please merge",
				"Operator approval needed: !2173 (db migration) is ready to merge",
			},
		},
		{
			// gd-nU1i7urKnS / gd-A5vp999mmV revival-review siblings.
			name: "gardener-reaper revival-review for ka-0000003531",
			prompts: []string{
				"reaper-revival-review ka-0000003531 (Gardenio Log Monitor Gardener)",
				"reaper-revival-review for ka-0000003531 (Gardenio Log Monitor)",
				"Should we revive gardener ka-0000003531? It's been disabled.",
			},
		},
		{
			// Same logical target referenced with mixed ID casing.
			name: "bead id case-insensitive collision",
			prompts: []string{
				"merge-gate ka-0000008070 — please review",
				"merge-gate ka-0000008070 — please review (lowercased ref)",
			},
		},
		{
			// Multi-MR batch, prompts list MRs in different orders.
			name: "batch merge-gate with reordered MR list",
			prompts: []string{
				"merge-gate batch on MRs !2172 and !2173 (db-migration)",
				"two db-migration MRs ready: !2173, !2172 — operator approval needed",
			},
		},
	}
	for _, g := range groups {
		t.Run(g.name, func(t *testing.T) {
			keys := make(map[string]int)
			for _, p := range g.prompts {
				keys[decisionBucketKey(p)]++
			}
			if len(keys) != 1 {
				t.Errorf("expected all %d rephrasings to share one bucket; got %d buckets: %v",
					len(g.prompts), len(keys), keys)
			}
		})
	}
}

func TestGroupOpenDecisions(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		{ID: "gd-a", Title: "MR !2130 merge gate", Fields: map[string]string{"prompt": "MR !2130 merge gate"}},
		{ID: "gd-b", Title: "MR !2130 merge gate", Fields: map[string]string{"prompt": "MR !2130 merge gate"}},
		{ID: "gd-c", Title: "Coverage epic 2a path", Fields: map[string]string{"prompt": "Coverage epic 2a path"}},
		{ID: "gd-d", Title: "MR !2130 merge gate", Fields: map[string]string{"prompt": "MR !2130 merge gate"}},
	}
	got := groupOpenDecisions(beads)
	if len(got) != 4 {
		t.Fatalf("expected 4 entries, got %d", len(got))
	}
	byID := map[string]openDecisionGroup{}
	for _, g := range got {
		byID[g.Bead.ID] = g
	}
	// gd-a, gd-b, gd-d share a bucket of size 3.
	for _, id := range []string{"gd-a", "gd-b", "gd-d"} {
		g := byID[id]
		if g.DupCount != 3 {
			t.Errorf("%s: expected DupCount=3, got %d", id, g.DupCount)
		}
		if len(g.DupIDs) != 2 {
			t.Errorf("%s: expected 2 sibling DupIDs, got %v", id, g.DupIDs)
		}
		for _, sid := range g.DupIDs {
			if sid == id {
				t.Errorf("%s: dup IDs should not include self, got %v", id, g.DupIDs)
			}
		}
	}
	// gd-c is alone.
	if byID["gd-c"].DupCount != 1 {
		t.Errorf("gd-c: expected DupCount=1, got %d", byID["gd-c"].DupCount)
	}
	if len(byID["gd-c"].DupIDs) != 0 {
		t.Errorf("gd-c: expected empty DupIDs, got %v", byID["gd-c"].DupIDs)
	}
}

// TestGroupOpenDecisions_SameSubjectCluster is the AC1 regression for
// ka-0000000551: four open decisions that each cite the same live trigger
// (ka-0000001007) plus one distinct extra id land in FOUR different exact-set
// buckets (dup_count=1 each), so the shipped detector reported zero clusters.
// After the fix they must all group into ONE same-subject cluster on the shared
// token, while remaining non-duplicates.
func TestGroupOpenDecisions_SameSubjectCluster(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		{ID: "ka-0000005373", Fields: map[string]string{"prompt": "gardener-bugfix budget ka-0000003109 self-starving dispatch on ka-0000001007"}},
		{ID: "gd-zG6zIdj2jL", Fields: map[string]string{"prompt": "drop in_progress from gate statuses gd-wOmR1trEQz on ka-0000001007"}},
		{ID: "ka-0000003573", Fields: map[string]string{"prompt": "release_owner_live coin-flip gate ka-0000005808 on trigger ka-0000001007"}},
		{ID: "gd-J8HL0S7Fuf", Fields: map[string]string{"prompt": "a0 docs-only-MR false-positive ka-0000006854 on ka-0000001007"}},
	}
	got := groupOpenDecisions(beads)
	if len(got) != 4 {
		t.Fatalf("expected 4 entries, got %d", len(got))
	}
	for _, g := range got {
		// AC3: these are NOT exact duplicates — dup_count stays 1.
		if g.DupCount != 1 {
			t.Errorf("%s: expected DupCount=1 (not a duplicate), got %d", g.Bead.ID, g.DupCount)
		}
		if !g.SharesSubject() {
			t.Errorf("%s: expected same-subject cluster membership, got RelatedIDs=%v", g.Bead.ID, g.RelatedIDs)
		}
		if len(g.RelatedIDs) != 3 {
			t.Errorf("%s: expected 3 related siblings, got %v", g.Bead.ID, g.RelatedIDs)
		}
		if g.SubjectToken != "ka-0000001007" {
			t.Errorf("%s: expected subject token ka-0000001007, got %q", g.Bead.ID, g.SubjectToken)
		}
		for _, id := range g.RelatedIDs {
			if id == g.Bead.ID {
				t.Errorf("%s: related IDs should not include self, got %v", g.Bead.ID, g.RelatedIDs)
			}
		}
	}
}

// TestGroupOpenDecisions_HubTokenDoesNotChain is the AC2 regression: 20
// decisions that all cite one common epic id (but nothing else in common) must
// NOT collapse into a single same-subject mega-cluster, because a token that
// appears in that many decisions is a hub, not a shared subject.
func TestGroupOpenDecisions_HubTokenDoesNotChain(t *testing.T) {
	epic := "gd-EPICHUB0001"
	var beads []*beadsapi.BeadDetail
	for i := 0; i < 20; i++ {
		// Each cites the shared epic plus a unique per-row id.
		unique := fmt.Sprintf("gd-unique%06d", i)
		beads = append(beads, &beadsapi.BeadDetail{
			ID:     fmt.Sprintf("gd-dec%03d", i),
			Fields: map[string]string{"prompt": fmt.Sprintf("fork %d re %s specific %s", i, epic, unique)},
		})
	}
	got := groupOpenDecisions(beads)
	if len(got) != 20 {
		t.Fatalf("expected 20 entries, got %d", len(got))
	}
	for _, g := range got {
		if g.SharesSubject() {
			t.Errorf("%s: hub epic token must not chain unrelated decisions; got RelatedIDs=%v (subject %q)",
				g.Bead.ID, g.RelatedIDs, g.SubjectToken)
		}
	}
}

// TestGroupOpenDecisions_ExactDupsNotRelated confirms AC3 semantics: true
// duplicates (identical id set) still surface via DupCount/DupIDs and are NOT
// double-reported in the same-subject RelatedIDs.
func TestGroupOpenDecisions_ExactDupsNotRelated(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		{ID: "gd-a", Fields: map[string]string{"prompt": "MR !2130 merge gate"}},
		{ID: "gd-b", Fields: map[string]string{"prompt": "please merge !2130 now"}},
	}
	got := groupOpenDecisions(beads)
	byID := map[string]openDecisionGroup{}
	for _, g := range got {
		byID[g.Bead.ID] = g
	}
	for _, id := range []string{"gd-a", "gd-b"} {
		g := byID[id]
		if g.DupCount != 2 {
			t.Errorf("%s: expected DupCount=2, got %d", id, g.DupCount)
		}
		// The exact duplicate is surfaced as a dup, not as a same-subject sibling.
		if g.SharesSubject() {
			t.Errorf("%s: exact duplicate must not also be reported as same-subject, got RelatedIDs=%v", id, g.RelatedIDs)
		}
	}
}

func TestGroupOpenDecisions_EmptyInput(t *testing.T) {
	if got := groupOpenDecisions(nil); got != nil {
		t.Errorf("nil input: expected nil, got %+v", got)
	}
	if got := groupOpenDecisions([]*beadsapi.BeadDetail{}); got != nil {
		t.Errorf("empty input: expected nil, got %+v", got)
	}
}

func TestGroupOpenDecisions_FallsBackToTitleWhenNoPrompt(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		{ID: "gd-1", Title: "shared title", Fields: map[string]string{}},
		{ID: "gd-2", Title: "shared title"}, // nil Fields map
	}
	got := groupOpenDecisions(beads)
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(got))
	}
	if got[0].DupCount != 2 || got[1].DupCount != 2 {
		t.Errorf("expected both rows DupCount=2, got %d / %d", got[0].DupCount, got[1].DupCount)
	}
}

func TestEmitNewsJSON_OpenDecisionsShape(t *testing.T) {
	created := time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)
	openDec := []openDecisionGroup{
		{
			Bead: &beadsapi.BeadDetail{
				ID:        "ka-0000001005",
				CreatedBy: "sched-gardenio-gardener-delivery-l3jq",
				CreatedAt: created,
				Fields:    map[string]string{"prompt": "MR !2130 merge gate"},
			},
			BucketKey: "mr !2130 merge gate",
			DupCount:  4,
			DupIDs:    []string{"ka-0000005609", "ka-0000005297", "ka-0000005896"},
		},
	}
	out := captureStdout(t, func() error {
		return emitNewsJSON("2h", nil, nil, nil, nil, nil, nil, nil, nil, nil, openDec, 0, nil, 0, newsSectionTotals{}, nil, false, false, false)
	})
	var got newsJSONOutput
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if len(got.OpenDecisions) != 1 {
		t.Fatalf("expected 1 open decision, got %d", len(got.OpenDecisions))
	}
	od := got.OpenDecisions[0]
	if od.ID != "ka-0000001005" {
		t.Errorf("id: %q", od.ID)
	}
	if od.DupGroup != "mr !2130 merge gate" {
		t.Errorf("dup_group: %q", od.DupGroup)
	}
	if od.DupCount != 4 {
		t.Errorf("dup_count: %d", od.DupCount)
	}
	if od.CreatedBy != "sched-gardenio-gardener-delivery-l3jq" {
		t.Errorf("created_by: %q", od.CreatedBy)
	}
	if od.CreatedAt != "2026-05-05T12:00:00Z" {
		t.Errorf("created_at: %q", od.CreatedAt)
	}
}

// The rendered path prints "… and N more open decision(s) not shown" when
// capBeads trims the section (ka-0000005518), but the JSON path shipped without
// any equivalent — so a consumer could not tell "50 open decisions exist" from
// "50 of 75 shown" and silently under-counted the queue by a third
// (ka-0000007375). Assert the machine path carries the same signal.
func TestEmitNewsJSON_OpenDecisionsTruncation(t *testing.T) {
	openDec := []openDecisionGroup{
		{Bead: &beadsapi.BeadDetail{ID: "gd-one", Type: "decision", Title: "a"}},
		{Bead: &beadsapi.BeadDetail{ID: "gd-two", Type: "decision", Title: "b"}},
	}

	t.Run("truncated", func(t *testing.T) {
		out := captureStdout(t, func() error {
			return emitNewsJSON("2h", nil, nil, nil, nil, nil, nil, nil, nil, nil, openDec, 75, nil, 0, newsSectionTotals{}, nil, false, false, false)
		})
		var got newsJSONOutput
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("invalid JSON: %v\n%s", err, out)
		}
		if !got.OpenDecisionsTruncated {
			t.Error("open_decisions_truncated: want true when total exceeds shown rows")
		}
		if got.OpenDecisionsTotal != 75 {
			t.Errorf("open_decisions_total: got %d, want 75", got.OpenDecisionsTotal)
		}
	})

	// When nothing was dropped the fields are omitempty, so an untruncated
	// digest keeps its existing shape rather than growing a "truncated": false.
	t.Run("not truncated", func(t *testing.T) {
		out := captureStdout(t, func() error {
			return emitNewsJSON("2h", nil, nil, nil, nil, nil, nil, nil, nil, nil, openDec, len(openDec), nil, 0, newsSectionTotals{}, nil, false, false, false)
		})
		var got newsJSONOutput
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("invalid JSON: %v\n%s", err, out)
		}
		if got.OpenDecisionsTruncated {
			t.Error("open_decisions_truncated: want false when all rows are shown")
		}
		if got.OpenDecisionsTotal != 0 {
			t.Errorf("open_decisions_total: got %d, want 0 (omitted)", got.OpenDecisionsTotal)
		}
		if bytes.Contains(out, []byte("open_decisions_total")) {
			t.Errorf("untruncated digest should omit open_decisions_total:\n%s", out)
		}
	})
}

// TestEmitNewsJSON_SectionTruncation mirrors the open_decisions truncation
// contract onto every capBeads-truncated section (ka-0000005034): a --json
// consumer must be able to tell "N exist" from "N of M shown" for in_progress,
// stale, unassigned, closed, decisions, and artifacts. Each <section>_total is
// the section's pre-cap in-scope count; the <section>_truncated flag (and the
// total) appear only when the cap dropped rows, and stay omitted otherwise.
func TestEmitNewsJSON_SectionTruncation(t *testing.T) {
	active := map[string]bool{"alive": true}
	activeIP := []*beadsapi.BeadDetail{{ID: "gd-a", Type: "task", Title: "live", Assignee: "alive"}}
	staleIP := []*beadsapi.BeadDetail{{ID: "gd-s", Type: "task", Title: "zombie", Assignee: "ghost"}}
	unassigned := []*beadsapi.BeadDetail{{ID: "gd-u", Type: "task", Title: "orphan"}}
	closed := []*beadsapi.BeadDetail{{ID: "gd-c", Type: "bug", Title: "fix"}}
	decisions := []*beadsapi.BeadDetail{{ID: "gd-d", Title: "d", Fields: map[string]string{"prompt": "?"}}}
	reports := []*beadsapi.BeadDetail{{ID: "gd-r", Title: "r", Fields: map[string]string{"report_type": "report"}}}
	staleClass := classifyStale(staleIP, rosterFromMaps(active, nil), nil)

	t.Run("truncated", func(t *testing.T) {
		// Every section's pre-cap total exceeds the single rendered row, so each
		// mirrors the open_decisions pair.
		totals := newsSectionTotals{InProgress: 4, Stale: 3, Unassigned: 5, Closed: 9, Decisions: 6, Artifacts: 7}
		out := captureStdout(t, func() error {
			return emitNewsJSON("2h", rosterFromMaps(active, nil), nil, activeIP, staleIP, staleClass, unassigned, closed, decisions, reports, nil, 0, nil, 0, totals, nil, false, false, false)
		})
		var got newsJSONOutput
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("invalid JSON: %v\n%s", err, out)
		}
		checks := []struct {
			name           string
			total, wantTot int
			trunc, wantTr  bool
		}{
			{"in_progress", got.InProgressTotal, 4, got.InProgressTruncated, true},
			{"stale", got.StaleTotal, 3, got.StaleTruncated, true},
			{"unassigned", got.UnassignedTotal, 5, got.UnassignedTruncated, true},
			{"closed", got.ClosedTotal, 9, got.ClosedTruncated, true},
			{"decisions", got.DecisionsTotal, 6, got.DecisionsTruncated, true},
			{"artifacts", got.ArtifactsTotal, 7, got.ArtifactsTruncated, true},
		}
		for _, c := range checks {
			if c.total != c.wantTot {
				t.Errorf("%s_total: got %d, want %d", c.name, c.total, c.wantTot)
			}
			if c.trunc != c.wantTr {
				t.Errorf("%s_truncated: got %v, want %v", c.name, c.trunc, c.wantTr)
			}
		}
	})

	t.Run("not truncated omits fields", func(t *testing.T) {
		// Pre-cap totals equal the rendered row counts (one each) — nothing was
		// dropped, so every pair is omitted and the shape is unchanged.
		totals := newsSectionTotals{InProgress: 1, Stale: 1, Unassigned: 1, Closed: 1, Decisions: 1, Artifacts: 1}
		out := captureStdout(t, func() error {
			return emitNewsJSON("2h", rosterFromMaps(active, nil), nil, activeIP, staleIP, staleClass, unassigned, closed, decisions, reports, nil, 0, nil, 0, totals, nil, false, false, false)
		})
		for _, key := range []string{"in_progress_total", "in_progress_truncated", "stale_total", "stale_truncated", "unassigned_total", "unassigned_truncated", "closed_total", "closed_truncated", "decisions_total", "decisions_truncated", "artifacts_total", "artifacts_truncated"} {
			if bytes.Contains(out, []byte(key)) {
				t.Errorf("untruncated digest should omit %q:\n%s", key, out)
			}
		}
	})

	t.Run("filtered section never reads phantom truncation", func(t *testing.T) {
		// --stale-only suppresses the in_progress/unassigned/closed/decisions/
		// artifacts arrays; a leftover pre-cap total for a suppressed section
		// must NOT manufacture a truncated:true against its empty array.
		totals := newsSectionTotals{InProgress: 9, Unassigned: 9, Closed: 9, Decisions: 9, Artifacts: 9, Stale: 3}
		out := captureStdout(t, func() error {
			return emitNewsJSON("2h", rosterFromMaps(active, nil), nil, activeIP, staleIP, staleClass, unassigned, closed, decisions, reports, nil, 0, nil, 0, totals, nil, true, false, false)
		})
		var got newsJSONOutput
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("invalid JSON: %v\n%s", err, out)
		}
		if got.InProgressTruncated || got.UnassignedTruncated || got.ClosedTruncated || got.DecisionsTruncated || got.ArtifactsTruncated {
			t.Errorf("stale-only suppressed sections must not read truncated: %+v", got)
		}
		// The stale section IS populated under --stale-only, so its truncation
		// signal must still fire.
		if !got.StaleTruncated || got.StaleTotal != 3 {
			t.Errorf("stale section under --stale-only: truncated=%v total=%d, want true/3", got.StaleTruncated, got.StaleTotal)
		}
	})
}

func TestPrintNewsOpenDecision_DupTagOnlyWhenDup(t *testing.T) {
	cases := []struct {
		name      string
		group     openDecisionGroup
		wantDupIn bool
	}{
		{
			"singleton has no DUP tag",
			openDecisionGroup{
				Bead:     &beadsapi.BeadDetail{ID: "gd-1", Title: "x", Fields: map[string]string{"prompt": "x"}},
				DupCount: 1,
			},
			false,
		},
		{
			"dup gets DUP tag",
			openDecisionGroup{
				Bead:     &beadsapi.BeadDetail{ID: "gd-1", Title: "x", Fields: map[string]string{"prompt": "x"}},
				DupCount: 3,
				DupIDs:   []string{"gd-2", "gd-3"},
			},
			true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := captureStdout(t, func() error {
				printNewsOpenDecisionTo(os.Stdout, c.group)
				return nil
			})
			hasDup := strings.Contains(string(out), "DUP×")
			if hasDup != c.wantDupIn {
				t.Errorf("hasDup=%v want %v\n%s", hasDup, c.wantDupIn, out)
			}
		})
	}
}

func TestBuildNewsJSONAgents_MergesStaleAssignees(t *testing.T) {
	roster := []newsAgentInfo{
		{Name: "alice", Alive: true, SpawnedBy: "gardener"},
		{Name: "bob", Alive: true},
	}
	// Ghost is a stale-bead assignee whose agent bead has closed (absent from the
	// roster) — it must still surface as a dead agent row.
	staleClass := []staleEntry{
		{Bead: &beadsapi.BeadDetail{ID: "x", Assignee: "Ghost"}, Class: staleClaimable}, // mixed case → lowercase
		{Bead: &beadsapi.BeadDetail{ID: "y", Assignee: "alice"}, Class: staleClaimable}, // duplicate of live → not re-added
		{Bead: &beadsapi.BeadDetail{ID: "z", Assignee: ""}, Class: staleClaimable},      // no assignee → skipped
	}
	got := buildNewsJSONAgents(roster, staleClass, false)

	byName := map[string]newsJSONAgent{}
	for _, a := range got {
		byName[a.Name] = a
	}
	if len(byName) != 3 {
		t.Fatalf("expected 3 unique agents, got %d: %+v", len(byName), got)
	}
	if !byName["alice"].Alive || byName["alice"].SpawnedBy != "gardener" {
		t.Errorf("alice: %+v", byName["alice"])
	}
	if !byName["bob"].Alive {
		t.Errorf("bob should be alive: %+v", byName["bob"])
	}
	if byName["ghost"].Alive {
		t.Errorf("ghost should NOT be alive: %+v", byName["ghost"])
	}
}

// TestNewsExcludeLabels_DefaultHidesRunInstances pins that the default news
// queries exclude the trigger-engine run-instance labels server-side — the
// "[N:N] Multi[cron]: ..." / "Schedule: ..." per-firing beads that made up
// over half of the closed section at a 24h window (ka-0000000723, rubric
// NWS-4 CLOSED-NOISE).
func TestNewsExcludeLabels_DefaultHidesRunInstances(t *testing.T) {
	got := newsExcludeLabels(false)
	want := map[string]bool{"source:schedule": true, "source:multi": true, "source:schedule-mirror": true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want labels %v", got, want)
	}
	for _, l := range got {
		if !want[l] {
			t.Errorf("unexpected exclude label %q", l)
		}
	}
}

// TestNewsExcludeLabels_IncludeCronRunsOptsBackIn guards the --include-cron-runs
// escape hatch: opting in must clear the exclusion entirely so the queries
// return every bead again.
func TestNewsExcludeLabels_IncludeCronRunsOptsBackIn(t *testing.T) {
	if got := newsExcludeLabels(true); got != nil {
		t.Fatalf("with includeCronRuns=true expected nil exclusions, got %v", got)
	}
}

// TestTruncTitle covers the audit-fix (ka-0000007728) properties for the news
// title truncator: no mid-word cut, rune-safety on multi-byte input, a hard cut
// when a single word overflows, and pass-through when the title fits.
func TestTruncTitle(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"fits untouched", "short title", 72, "short title"},
		{"exact length untouched", "abcde", 5, "abcde"},
		{
			// Old code cut mid-word at byte 71 → "…tion". The word-boundary
			// truncator backs off to the last space so no word is split.
			"backs off to word boundary",
			"gb news prompt hygiene read the digest before irreversible deploy actions now",
			40,
			"gb news prompt hygiene read the digest…",
		},
		{
			// A single word longer than the budget must still be cut (hard), not
			// dropped whole — otherwise a long token yields a bare ellipsis.
			"hard cut when first word overflows",
			"supercalifragilisticexpialidocious",
			10,
			"supercali…",
		},
		{"tiny budget yields ellipsis", "anything", 1, "…"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := truncTitle(c.in, c.max)
			if got != c.want {
				t.Errorf("truncTitle(%q, %d) = %q, want %q", c.in, c.max, got, c.want)
			}
			if got != c.in && utf8.RuneCountInString(got) > c.max {
				t.Errorf("truncTitle(%q, %d) = %q exceeds max %d runes", c.in, c.max, got, c.max)
			}
			if !utf8.ValidString(got) {
				t.Errorf("truncTitle(%q, %d) = %q is not valid UTF-8", c.in, c.max, got)
			}
		})
	}
}

// TestTruncTitle_MultibyteRuneSafe pins that a cut never splits a multi-byte
// rune into invalid UTF-8 — the failure mode of the old byte-slice truncator.
func TestTruncTitle_MultibyteRuneSafe(t *testing.T) {
	// Each "é" is 2 bytes; "★" is 3. A byte-slice cut mid-rune would corrupt.
	in := "café ★ résumé naïve façade"
	for max := 2; max <= utf8.RuneCountInString(in)+2; max++ {
		got := truncTitle(in, max)
		if !utf8.ValidString(got) {
			t.Fatalf("truncTitle(%q, %d) = %q produced invalid UTF-8", in, max, got)
		}
	}
}

// TestPrintNewsIDLine pins the machine format for --ids-only: "ID<TAB>title"
// on one line, with the title whitespace-collapsed so an embedded newline can
// never break the one-row-per-line contract (audit rec 7c, ka-0000001955).
func TestPrintNewsIDLine(t *testing.T) {
	b := &beadsapi.BeadDetail{ID: "gd-abc", Title: "fix the\n  news   digest"}
	out := string(captureStdout(t, func() error {
		printNewsIDLine(b)
		return nil
	}))
	if out != "gd-abc\tfix the news digest\n" {
		t.Fatalf("printNewsIDLine = %q, want %q", out, "gd-abc\tfix the news digest\n")
	}
	// cut -f1 must still recover the bare ID.
	if id := strings.SplitN(strings.TrimRight(out, "\n"), "\t", 2)[0]; id != "gd-abc" {
		t.Errorf("first tab-field = %q, want gd-abc", id)
	}
}

// TestBuildNewsAgents_PopulatesRoleAndStatus is the core M4 regression
// (ka-0000000465): the JSON agents array carried NO role/status, so the one
// field release gardeners query was null on 100% of rows across three audit
// sessions. Every retained roster row must now carry role/state/heartbeat.
func TestBuildNewsAgents_PopulatesRoleAndStatus(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	beads := []*beadsapi.BeadDetail{
		{ID: "gd-a", UpdatedAt: now.Add(-30 * time.Second),
			Fields: map[string]string{"agent": "crew-a", "role": "crew", "agent_state": "working", "spawned_by": "gardener-x"}},
	}
	roster := buildNewsAgents(beads, now, newsAliveFreshnessWindow, "")
	if len(roster) != 1 {
		t.Fatalf("expected 1 roster row, got %d: %+v", len(roster), roster)
	}
	got := roster[0]
	if got.Role != "crew" {
		t.Errorf("role: got %q, want crew", got.Role)
	}
	if got.State != "working" {
		t.Errorf("state: got %q, want working", got.State)
	}
	if !got.Alive {
		t.Errorf("fresh working agent must be alive: %+v", got)
	}

	// The JSON projection must carry them through as non-empty role/status/heartbeat.
	ja := buildNewsJSONAgents(roster, nil, false)
	if len(ja) != 1 || ja[0].Role != "crew" || ja[0].Status != "working" {
		t.Fatalf("JSON agent role/status not populated: %+v", ja)
	}
	if ja[0].LastHeartbeat == "" {
		t.Errorf("last_heartbeat must be populated from updated_at: %+v", ja[0])
	}
}

// TestBuildNewsAgents_ExcludesPrewarmedAndSelf pins the roster-junk fix: an
// unassigned prewarmed-pool placeholder and the caller's own row are dropped,
// while a real teammate and an ENGAGED (task-assigned) former-pool agent stay.
func TestBuildNewsAgents_ExcludesPrewarmedAndSelf(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-30 * time.Second)
	beads := []*beadsapi.BeadDetail{
		{ID: "gd-self", UpdatedAt: fresh, Fields: map[string]string{"agent": "gardenio-me-abc", "role": "crew", "agent_state": "working"}},
		{ID: "gd-pw-state", UpdatedAt: fresh, Fields: map[string]string{"agent": "gardenio-pool-1", "agent_state": "prewarmed"}},
		{ID: "gd-pw-label", UpdatedAt: fresh, Labels: []string{"prewarmed"}, Fields: map[string]string{"agent": "gardenio-pool-2", "agent_state": "spawning"}},
		{ID: "gd-pw-engaged", UpdatedAt: fresh, Labels: []string{"prewarmed"}, Fields: map[string]string{"agent": "gardenio-pool-3", "agent_state": "working", "task_id": "gd-work"}},
		{ID: "gd-mate", UpdatedAt: fresh, Fields: map[string]string{"agent": "gardenio-teammate-xyz", "role": "crew", "agent_state": "working"}},
	}
	// selfActor passed in the leaked "project/agent" form to prove normalization.
	roster := buildNewsAgents(beads, now, newsAliveFreshnessWindow, "gardenio/gardenio-me-abc")
	names := map[string]bool{}
	selfTagged := false
	for _, a := range roster {
		names[a.Name] = true
		if a.Name == "gardenio-me-abc" && a.IsSelf {
			selfTagged = true
		}
	}
	// Self is KEPT in the roster (tagged) so its liveness is known — but tagged
	// so the display layer can hide it.
	if !selfTagged {
		t.Errorf("caller's own row must be kept and tagged IsSelf: %+v", roster)
	}
	if names["gardenio-pool-1"] || names["gardenio-pool-2"] {
		t.Errorf("unassigned prewarmed placeholders must be excluded: %+v", names)
	}
	if !names["gardenio-pool-3"] {
		t.Errorf("an engaged former-pool agent (has task_id) must be kept: %+v", names)
	}
	if !names["gardenio-teammate-xyz"] {
		t.Errorf("a real teammate must be kept: %+v", names)
	}

	// The DISPLAYED agents array hides self by default, shows it under --all.
	hidden := buildNewsJSONAgents(roster, nil, false)
	for _, a := range hidden {
		if a.Name == "gardenio-me-abc" {
			t.Errorf("self must be hidden from the default agents array: %+v", hidden)
		}
	}
	shown := buildNewsJSONAgents(roster, nil, true)
	found := false
	for _, a := range shown {
		if a.Name == "gardenio-me-abc" {
			found = true
		}
	}
	if !found {
		t.Errorf("self must appear in the agents array under --all: %+v", shown)
	}
}

// TestClassifyStale_ThreeWay is the M4 stale-semantics regression: a stale bead
// whose assignee is gone must be split into claimable / hard-claimed /
// respawned-successor-active. The respawned-successor case is the dangerous one
// that nearly misled a pre-prod-apply session (lj9).
func TestClassifyStale_ThreeWay(t *testing.T) {
	roster := []newsAgentInfo{
		// A live successor sharing the gone predecessor's SEAT.
		{Name: "succ-agent", Alive: true, SeatKey: "gardenio/crew/seat-A", TaskID: "gd-seatwork"},
		// A live agent that IS the "gone" assignee (truncated out of active set).
		{Name: "still-live", Alive: true},
		// The gone predecessor's own (now-dead) roster row, carrying its seat_key.
		{Name: "dead-pred", Alive: false, SeatKey: "gardenio/crew/seat-A"},
		// A live agent assigned directly to a stale bead's own id (task successor).
		{Name: "task-succ", Alive: true, TaskID: "gd-taskwork"},
	}
	staleIP := []*beadsapi.BeadDetail{
		{ID: "gd-seatwork2", Assignee: "dead-pred"}, // successor holds same seat → DANGER
		{ID: "gd-taskwork", Assignee: "gone-guy"},   // live agent assigned this very bead → DANGER
		{ID: "gd-hard", Assignee: "still-live"},     // assignee itself still live → hard-claimed
		{ID: "gd-free", Assignee: "truly-gone"},     // nobody live → claimable
	}
	entries := classifyStale(staleIP, roster, nil)
	byID := map[string]staleEntry{}
	for _, e := range entries {
		byID[e.Bead.ID] = e
	}
	if byID["gd-seatwork2"].Class != staleRespawnedSuccessorActive || byID["gd-seatwork2"].Successor != "succ-agent" {
		t.Errorf("gd-seatwork2 should be respawned-successor-active via seat, successor succ-agent: %+v", byID["gd-seatwork2"])
	}
	if byID["gd-taskwork"].Class != staleRespawnedSuccessorActive || byID["gd-taskwork"].Successor != "task-succ" {
		t.Errorf("gd-taskwork should be respawned-successor-active via task, successor task-succ: %+v", byID["gd-taskwork"])
	}
	if byID["gd-hard"].Class != staleHardClaimed || byID["gd-hard"].Successor != "still-live" {
		t.Errorf("gd-hard should be hard-claimed by still-live: %+v", byID["gd-hard"])
	}
	if byID["gd-free"].Class != staleClaimable {
		t.Errorf("gd-free should be claimable: %+v", byID["gd-free"])
	}
}

// TestEmitNewsJSON_StaleClassOnRows asserts the stale array carries stale_class
// and successor on each row so a machine consumer can triage without a second
// lookup.
func TestEmitNewsJSON_StaleClassOnRows(t *testing.T) {
	roster := []newsAgentInfo{
		{Name: "succ", Alive: true, TaskID: "gd-danger"},
	}
	staleIP := []*beadsapi.BeadDetail{
		{ID: "gd-danger", Type: "task", Title: "shared state", Assignee: "gone-pred"},
	}
	staleClass := classifyStale(staleIP, roster, nil)
	out := captureStdout(t, func() error {
		return emitNewsJSON("2h", roster, nil, nil, staleIP, staleClass, nil, nil, nil, nil, nil, 0, nil, 0, newsSectionTotals{}, nil, false, false, false)
	})
	var got newsJSONOutput
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if len(got.Stale) != 1 {
		t.Fatalf("expected 1 stale row, got %+v", got.Stale)
	}
	if got.Stale[0].StaleClass != "respawned-successor-active" || got.Stale[0].Successor != "succ" {
		t.Errorf("stale row must carry stale_class + successor: %+v", got.Stale[0])
	}
}

// TestPrintStaleSections_Grouped verifies the pretty output splits the four
// buckets under distinct headings and marks the dangerous / parked ones.
func TestPrintStaleSections_Grouped(t *testing.T) {
	entries := []staleEntry{
		{Bead: &beadsapi.BeadDetail{ID: "gd-c", Title: "free"}, Class: staleClaimable},
		{Bead: &beadsapi.BeadDetail{ID: "gd-h", Title: "held"}, Class: staleHardClaimed, Successor: "holder"},
		{Bead: &beadsapi.BeadDetail{ID: "gd-d", Title: "dangerous"}, Class: staleRespawnedSuccessorActive, Successor: "successor"},
		{Bead: &beadsapi.BeadDetail{ID: "gd-p", Title: "parked"}, Class: staleEscalationParked, Handle: "gd-esc1"},
		{Bead: &beadsapi.BeadDetail{ID: "gd-r", Title: "shipped"}, Class: staleAwaitsRelease},
	}
	out := string(captureStdout(t, func() error {
		printStaleSections(os.Stdout, entries, nil)
		return nil
	}))
	for _, want := range []string{
		"Stale — claimable", "Stale — hard-claimed", "Stale — respawned-successor-active (DANGER)",
		"Stale — parked (awaiting operator or release, do NOT reap)",
		"⚠ successor @successor is ACTIVE", "still held by live @holder",
		"parked on operator handle gd-esc1 (not closed)",
		"awaits its release (awaits_release=true) — do NOT reap",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stale output missing %q\n%s", want, out)
		}
	}
}

// TestClassifyStale_AwaitsRelease: a stale bead with awaits_release=true is
// done and closes with its release, so it must not be listed as claimable
// (gd-aaGqLb2cUk). The same bead without the field stays claimable.
func TestClassifyStale_AwaitsRelease(t *testing.T) {
	stale := []*beadsapi.BeadDetail{
		{ID: "gd-shipped", Assignee: "gone-crew", Fields: map[string]string{"awaits_release": "true"}},
		{ID: "gd-free", Assignee: "gone-crew"},
	}
	got := classifyStale(stale, nil, nil)
	if got[0].Class != staleAwaitsRelease || staleClassLabel(got[0].Class) != "awaits-release" {
		t.Errorf("awaits_release bead classified %q, want awaits-release", staleClassLabel(got[0].Class))
	}
	if got[1].Class != staleClaimable {
		t.Errorf("plain stale bead classified %q, want claimable", staleClassLabel(got[1].Class))
	}
}

// TestClassifyStale_EscalationParked is the ka-0000007805 regression: a stale
// bead parked behind an open operator handle (its id appears in
// parkedByBead) must classify as escalation-parked — NOT claimable — so
// `gb news` stops advertising it as reapable and restarting the documented
// reap->respawn->re-escalate loop. It also asserts precedence: a genuinely-live
// successor/holder still wins over the parked signal, so a live row is never
// masked by a stale handle.
func TestClassifyStale_EscalationParked(t *testing.T) {
	roster := []newsAgentInfo{
		{Name: "live-succ", Alive: true, TaskID: "gd-parked-but-live"},
	}
	staleIP := []*beadsapi.BeadDetail{
		{ID: "gd-parked", Type: "task", Title: "MR merge parked", Assignee: "gone-agent"},
		{ID: "gd-free", Type: "bug", Title: "genuinely free", Assignee: "also-gone"},
		{ID: "gd-parked-but-live", Type: "task", Title: "parked yet live successor", Assignee: "dead-pred"},
	}
	// Both gd-parked and gd-parked-but-live are named by an open handle, but
	// only gd-parked has no live successor, so only it should land in parked.
	parked := map[string]string{
		"gd-parked":          "gd-escP",
		"gd-parked-but-live": "gd-escL",
	}
	entries := classifyStale(staleIP, roster, parked)
	byID := map[string]staleEntry{}
	for _, e := range entries {
		byID[e.Bead.ID] = e
	}
	if byID["gd-parked"].Class != staleEscalationParked || byID["gd-parked"].Handle != "gd-escP" {
		t.Errorf("gd-parked should be escalation-parked on handle gd-escP: %+v", byID["gd-parked"])
	}
	if byID["gd-free"].Class != staleClaimable {
		t.Errorf("gd-free should be claimable: %+v", byID["gd-free"])
	}
	if byID["gd-parked-but-live"].Class != staleRespawnedSuccessorActive || byID["gd-parked-but-live"].Successor != "live-succ" {
		t.Errorf("a live successor must win over the parked signal: %+v", byID["gd-parked-but-live"])
	}
}

// TestEmitNewsJSON_ParkedRow asserts an escalation-parked stale row carries
// stale_class=escalation-parked and parked_on=<handle> so a machine consumer
// (e.g. a reap sweep) can subtract it and link to the operator handle.
func TestEmitNewsJSON_ParkedRow(t *testing.T) {
	staleIP := []*beadsapi.BeadDetail{
		{ID: "gd-parked", Type: "task", Title: "MR merge parked", Assignee: "gone-agent"},
	}
	staleClass := classifyStale(staleIP, nil, map[string]string{"gd-parked": "gd-escP"})
	out := captureStdout(t, func() error {
		return emitNewsJSON("2h", nil, nil, nil, staleIP, staleClass, nil, nil, nil, nil, nil, 0, nil, 0, newsSectionTotals{}, nil, false, false, false)
	})
	var got newsJSONOutput
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if len(got.Stale) != 1 {
		t.Fatalf("expected 1 stale row, got %+v", got.Stale)
	}
	if got.Stale[0].StaleClass != "escalation-parked" || got.Stale[0].ParkedOn != "gd-escP" {
		t.Errorf("stale row must carry stale_class + parked_on: %+v", got.Stale[0])
	}
}
