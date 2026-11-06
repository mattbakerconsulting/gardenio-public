package main

import (
	"encoding/json"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Bug ka-0000007772: the gardener goal-state badge was computed ONCE at
// gardener-close from the terminated slot/kind and never re-evaluated when the
// target epic's status flipped afterward. This produced:
//
//   - FALSE-NEGATIVES: "goal not reached" though the target epic CLOSED.
//   - FALSE-POSITIVES: "GOAL ACHIEVED" though the target epic is still OPEN.
//
// The fix re-derives the disposition LIVE from the target epic's CURRENT status
// at render time, treating the epic's OWN status as authoritative (an open epic
// is never "achieved", regardless of a 0-subtree count from a malformed CTE).

// TestGardenerGoalState_LiveReDerive exercises the pure GardenerGoalState
// re-derive across the live-target matrix and the snapshot fallback.
func TestGardenerGoalState_LiveReDerive(t *testing.T) {
	cases := []struct {
		name             string
		gardenerClosed   bool
		slot, kind       string // close-time snapshot provenance
		targetEpicID     string
		targetEpicStatus string
		want             string
	}{
		// --- FALSE-NEGATIVE repro: snapshot says not-achieved, epic is CLOSED. ---
		{
			name:           "closed gardener, budget-exhausted snapshot, but epic CLOSED -> achieved",
			gardenerClosed: true, slot: "loop_end", kind: "max_runs",
			targetEpicID: "gd-epic", targetEpicStatus: "closed",
			want: "achieved",
		},
		{
			name:           "closed gardener, retired snapshot, but epic CLOSED -> achieved",
			gardenerClosed: true, slot: "operator", kind: "retired",
			targetEpicID: "gd-epic", targetEpicStatus: "closed",
			want: "achieved",
		},
		{
			name:           "closed gardener, no provenance, but epic CLOSED -> achieved",
			gardenerClosed: true, slot: "", kind: "",
			targetEpicID: "gd-epic", targetEpicStatus: "closed",
			want: "achieved",
		},

		// --- FALSE-POSITIVE repro: snapshot says achieved, epic still OPEN. ---
		{
			name:           "closed gardener, exit_condition snapshot, but epic OPEN -> unachieved",
			gardenerClosed: true, slot: "loop_end", kind: "exit_condition",
			targetEpicID: "gd-epic", targetEpicStatus: "open",
			want: "unachieved",
		},
		{
			name:           "closed gardener, bead_query snapshot, but epic IN_PROGRESS -> unachieved",
			gardenerClosed: true, slot: "loop_end", kind: "bead_query",
			targetEpicID: "gd-epic", targetEpicStatus: "in_progress",
			want: "unachieved",
		},
		{
			name:           "OPEN gardener, bead_query snapshot, epic still OPEN -> no disposition yet",
			gardenerClosed: false, slot: "loop_end", kind: "bead_query",
			targetEpicID: "gd-epic", targetEpicStatus: "open",
			want: "",
		},
		// blocked is an active status -> not achieved.
		{
			name:           "closed gardener, epic BLOCKED -> unachieved (open is authoritative)",
			gardenerClosed: true, slot: "loop_end", kind: "exit_condition",
			targetEpicID: "gd-epic", targetEpicStatus: "blocked",
			want: "unachieved",
		},

		// --- Live target where snapshot already agreed (no regression). ---
		{
			name:           "OPEN gardener, epic CLOSED -> achieved even though gardener still open",
			gardenerClosed: false, slot: "", kind: "",
			targetEpicID: "gd-epic", targetEpicStatus: "closed",
			want: "achieved",
		},

		// --- Fallback: no resolvable target -> snapshot provenance governs. ---
		{
			name:           "no target id, closed via exit_condition -> achieved (snapshot)",
			gardenerClosed: true, slot: "loop_end", kind: "exit_condition",
			targetEpicID: "", targetEpicStatus: "",
			want: "achieved",
		},
		{
			name:           "no target id, closed via budget -> unachieved (snapshot)",
			gardenerClosed: true, slot: "loop_end", kind: "max_runs",
			targetEpicID: "", targetEpicStatus: "",
			want: "unachieved",
		},
		{
			name:           "no target id, open gardener -> no disposition (snapshot)",
			gardenerClosed: false, slot: "", kind: "",
			targetEpicID: "", targetEpicStatus: "",
			want: "",
		},
		// Target id resolved but status fetch missed (pruned/daemon error):
		// degrade to snapshot rather than guessing.
		{
			name:           "target resolved but status unfetched -> snapshot fallback (achieved)",
			gardenerClosed: true, slot: "loop_end", kind: "exit_condition",
			targetEpicID: "gd-epic", targetEpicStatus: "",
			want: "achieved",
		},
		{
			name:           "target resolved but status unfetched -> snapshot fallback (unachieved)",
			gardenerClosed: true, slot: "loop_end", kind: "max_runs",
			targetEpicID: "gd-epic", targetEpicStatus: "",
			want: "unachieved",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := gardenerView{
				IsClosed:         tc.gardenerClosed,
				TerminatedSlot:   tc.slot,
				TerminatedKind:   tc.kind,
				TargetEpicID:     tc.targetEpicID,
				TargetEpicStatus: tc.targetEpicStatus,
			}
			if got := v.GardenerGoalState(); got != tc.want {
				t.Errorf("GardenerGoalState() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestGardenerStatusIsClosed pins the status vocabulary used by the live
// re-derive: only out-of-active-set statuses (notably "closed") count as
// closed; empty/unknown must NOT be read as done.
func TestGardenerStatusIsClosed(t *testing.T) {
	closed := map[string]bool{
		"closed":      true,
		"resolved":    true, // any non-active status is "done" for badge purposes
		"":            false,
		"open":        false,
		"in_progress": false,
		"blocked":     false,
		"deferred":    false,
	}
	for status, want := range closed {
		if got := gardenerStatusIsClosed(status); got != want {
			t.Errorf("gardenerStatusIsClosed(%q) = %v, want %v", status, got, want)
		}
	}
}

// TestGardenerGoalTargetID covers extracting the authoritative goal target from
// both the modern gardener.stop block and the legacy conditions pre-flight gate
// (the shape the affected gardeners in this bug carry).
func TestGardenerGoalTargetID(t *testing.T) {
	mkStopGardener := func(stop map[string]any) *beadsapi.BeadDetail {
		stopRaw, _ := json.Marshal(stop)
		spec := map[string]any{"name": "c", "stop": json.RawMessage(stopRaw)}
		raw, _ := json.Marshal(spec)
		return &beadsapi.BeadDetail{
			ID: "gd-cap", Type: "trigger", Status: "closed",
			Labels: []string{"trigger:schedule", "role:gardener"},
			Fields: map[string]string{"role": "gardener", "gardener": string(raw)},
		}
	}

	t.Run("stop loop_end bead_query parent", func(t *testing.T) {
		b := mkStopGardener(map[string]any{
			"loop_end": map[string]any{
				"any": []map[string]any{
					{"kind": "bead_query", "op": "==", "value": "0", "parent": "gd-root-epic"},
				},
			},
		})
		if got := gardenerGoalTargetID(b); got != "gd-root-epic" {
			t.Errorf("got %q, want gd-root-epic", got)
		}
	})

	t.Run("stop specific_bead_closed wins over bead_query", func(t *testing.T) {
		b := mkStopGardener(map[string]any{
			"loop_end": map[string]any{
				"any": []map[string]any{
					{"kind": "bead_query", "op": "==", "value": "0", "parent": "gd-root-epic"},
					{"kind": "specific_bead_closed", "bead_id": "gd-target-bead"},
				},
			},
		})
		if got := gardenerGoalTargetID(b); got != "gd-target-bead" {
			t.Errorf("got %q, want gd-target-bead", got)
		}
	})

	t.Run("legacy conditions bead_query parent", func(t *testing.T) {
		// Mirrors the affected gardeners: a `conditions` JSON array with a
		// bead_query carrying `parent`, no gardener.stop block.
		spec := map[string]any{"name": "c"}
		raw, _ := json.Marshal(spec)
		b := &beadsapi.BeadDetail{
			ID: "gd-cap", Type: "trigger", Status: "closed",
			Labels: []string{"trigger:schedule", "role:gardener"},
			Fields: map[string]string{
				"role":       "gardener",
				"gardener":   string(raw),
				"conditions": `[{"type":"bead_query","op":">","value":"0","types":["task","bug","epic"],"statuses":["open","in_progress","blocked"],"parent":"gd-legacy-epic"}]`,
			},
		}
		if got := gardenerGoalTargetID(b); got != "gd-legacy-epic" {
			t.Errorf("got %q, want gd-legacy-epic", got)
		}
	})

	t.Run("no resolvable target", func(t *testing.T) {
		// exit_condition-only gardener — no parent, no specific bead.
		b := mkStopGardener(map[string]any{
			"loop_end": map[string]any{
				"any": []map[string]any{
					{"kind": "consecutive_failures", "value": "3"},
				},
			},
		})
		if got := gardenerGoalTargetID(b); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("nil bead", func(t *testing.T) {
		if got := gardenerGoalTargetID(nil); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}

// TestPopulateLiveGoalState_DetailPath verifies the single-fetch detail-page
// helper resolves the target, fetches its current status, and flips the badge —
// both the false-negative (epic closed) and false-positive (epic open) cases.
func TestPopulateLiveGoalState_DetailPath(t *testing.T) {
	mkLegacyGardener := func(id, epicID string) *beadsapi.BeadDetail {
		spec := map[string]any{"name": id}
		raw, _ := json.Marshal(spec)
		return &beadsapi.BeadDetail{
			ID: id, Type: "trigger", Status: "closed",
			Labels: []string{"trigger:schedule", "role:gardener"},
			Fields: map[string]string{
				"role":                     "gardener",
				"gardener":                 string(raw),
				"conditions":               `[{"type":"bead_query","op":">","value":"0","parent":"` + epicID + `"}]`,
				"gardener_terminated_slot": "loop_end",
				"gardener_terminated_kind": "max_runs", // snapshot: NOT achieved
			},
			UpdatedAt: time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
		}
	}

	t.Run("epic closed flips stale not-achieved -> achieved", func(t *testing.T) {
		f := newFake()
		cap := mkLegacyGardener("gd-cap-fn", "gd-epic-closed")
		f.beads["gd-cap-fn"] = cap
		f.beads["gd-epic-closed"] = &beadsapi.BeadDetail{ID: "gd-epic-closed", Type: "epic", Status: "closed"}
		srv := gardenerTestServer(t, f)

		v, err := srv.gardenerViewFromBead(t.Context(), cap)
		if err != nil {
			t.Fatalf("gardenerViewFromBead: %v", err)
		}
		// Snapshot-only disposition before live re-derive.
		if got := v.GardenerGoalState(); got != "unachieved" {
			t.Fatalf("pre-enrich snapshot = %q, want unachieved", got)
		}
		srv.populateLiveGoalState(t.Context(), &v, cap)
		if v.TargetEpicID != "gd-epic-closed" {
			t.Errorf("TargetEpicID = %q, want gd-epic-closed", v.TargetEpicID)
		}
		if got := v.GardenerGoalState(); got != "achieved" {
			t.Errorf("post-enrich GoalState = %q, want achieved (epic closed)", got)
		}
	})

	t.Run("epic open never reads achieved", func(t *testing.T) {
		f := newFake()
		// Snapshot says achieved (exit_condition), but the epic is still open.
		spec := map[string]any{"name": "gd-cap-fp"}
		raw, _ := json.Marshal(spec)
		cap := &beadsapi.BeadDetail{
			ID: "gd-cap-fp", Type: "trigger", Status: "closed",
			Labels: []string{"trigger:schedule", "role:gardener"},
			Fields: map[string]string{
				"role":                     "gardener",
				"gardener":                 string(raw),
				"conditions":               `[{"type":"bead_query","op":">","value":"0","parent":"gd-epic-open"}]`,
				"gardener_terminated_slot": "loop_end",
				"gardener_terminated_kind": "exit_condition", // snapshot: achieved
			},
		}
		f.beads["gd-cap-fp"] = cap
		f.beads["gd-epic-open"] = &beadsapi.BeadDetail{ID: "gd-epic-open", Type: "epic", Status: "open"}
		srv := gardenerTestServer(t, f)

		v, err := srv.gardenerViewFromBead(t.Context(), cap)
		if err != nil {
			t.Fatalf("gardenerViewFromBead: %v", err)
		}
		if got := v.GardenerGoalState(); got != "achieved" {
			t.Fatalf("pre-enrich snapshot = %q, want achieved (the false positive)", got)
		}
		srv.populateLiveGoalState(t.Context(), &v, cap)
		if got := v.GardenerGoalState(); got != "unachieved" {
			t.Errorf("post-enrich GoalState = %q, want unachieved (epic still open)", got)
		}
	})

	t.Run("missing target bead degrades to snapshot", func(t *testing.T) {
		f := newFake()
		cap := mkLegacyGardener("gd-cap-miss", "gd-epic-gone")
		f.beads["gd-cap-miss"] = cap
		// gd-epic-gone intentionally absent from the fake.
		srv := gardenerTestServer(t, f)

		v, _ := srv.gardenerViewFromBead(t.Context(), cap)
		srv.populateLiveGoalState(t.Context(), &v, cap)
		if v.TargetEpicID != "gd-epic-gone" {
			t.Errorf("TargetEpicID = %q, want gd-epic-gone", v.TargetEpicID)
		}
		if v.TargetEpicStatus != "" {
			t.Errorf("TargetEpicStatus = %q, want empty (fetch missed)", v.TargetEpicStatus)
		}
		// Snapshot (max_runs) governs: unachieved.
		if got := v.GardenerGoalState(); got != "unachieved" {
			t.Errorf("GoalState = %q, want unachieved (snapshot fallback)", got)
		}
	})
}

// TestEnrichGardenerGoalStates_IndexPath verifies the batched index enrichment
// flips both the false-negative and false-positive rows in ONE BatchGetBeads,
// and leaves a no-target row on its snapshot value.
func TestEnrichGardenerGoalStates_IndexPath(t *testing.T) {
	f := newFake()
	f.beads["gd-epic-closed"] = &beadsapi.BeadDetail{ID: "gd-epic-closed", Type: "epic", Status: "closed"}
	f.beads["gd-epic-open"] = &beadsapi.BeadDetail{ID: "gd-epic-open", Type: "epic", Status: "open"}
	srv := gardenerTestServer(t, f)

	rows := []gardenerRowView{
		// FALSE-NEGATIVE: snapshot unachieved, epic closed -> should flip to achieved.
		{ID: "gd-fn", IsClosed: true, GoalState: "unachieved", targetEpicID: "gd-epic-closed"},
		// FALSE-POSITIVE: snapshot achieved, epic open -> should flip to unachieved.
		{ID: "gd-fp", IsClosed: true, GoalState: "achieved", targetEpicID: "gd-epic-open"},
		// No target -> snapshot value preserved.
		{ID: "gd-nt", IsClosed: true, GoalState: "achieved", targetEpicID: ""},
		// Target pruned -> snapshot value preserved.
		{ID: "gd-pruned", IsClosed: true, GoalState: "unachieved", targetEpicID: "gd-epic-gone"},
	}

	before := f.batchGetBeadsCalls
	srv.enrichGardenerGoalStates(t.Context(), rows)
	if got := f.batchGetBeadsCalls - before; got != 1 {
		t.Errorf("expected exactly 1 BatchGetBeads call (no N+1), got %d", got)
	}

	want := map[string]string{
		"gd-fn":     "achieved",
		"gd-fp":     "unachieved",
		"gd-nt":     "achieved",
		"gd-pruned": "unachieved",
	}
	for _, r := range rows {
		if got := r.GoalState; got != want[r.ID] {
			t.Errorf("%s: GoalState = %q, want %q", r.ID, got, want[r.ID])
		}
	}
}
