package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// gardenerSpendDaemon is a filter-aware daemon stub for the gardener spend
// roll-up. Unlike the simpler spend_sync daemonStub (which returns all beads
// for every list), this honors the ?type= and ?field= predicates the roll-up
// relies on, so a test can assert the BFS walks exactly the right edges.
type gardenerSpendDaemon struct {
	mu      sync.Mutex
	beads   []map[string]any
	updates []daemonUpdate
	// fieldsOnly records each list's ?fields_only= (gd-6PXLuR3juD); when set,
	// the fake projects the returned fields to those keys like the daemon.
	fieldsOnly []string
	// lists counts GET /v1/beads requests.
	lists int
}

func (d *gardenerSpendDaemon) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/beads/") &&
			!strings.Contains(strings.TrimPrefix(r.URL.Path, "/v1/beads/"), "/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/beads/")
			d.mu.Lock()
			defer d.mu.Unlock()
			for _, b := range d.beads {
				if b["id"] == id {
					_ = json.NewEncoder(w).Encode(b)
					return
				}
			}
			http.NotFound(w, r)

		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			q := r.URL.Query()
			wantType := q.Get("type")
			// Each ?field=key=value predicate must match (AND across predicates).
			fieldPreds := map[string]string{}
			for _, fv := range q["field"] {
				if k, v, ok := strings.Cut(fv, "="); ok {
					fieldPreds[k] = v
				}
			}
			d.mu.Lock()
			var matched []map[string]any
			for _, b := range d.beads {
				if wantType != "" && b["type"] != wantType {
					continue
				}
				fields := beadFields(b)
				ok := true
				for k, v := range fieldPreds {
					if fields[k] != v {
						ok = false
						break
					}
				}
				// Labels predicate (role:gardener) — match against a labels slice.
				if labels := q.Get("labels"); labels != "" && !beadHasLabel(b, labels) {
					ok = false
				}
				if ok {
					matched = append(matched, projectFieldsOnly(b, q.Get("fields_only")))
				}
			}
			d.fieldsOnly = append(d.fieldsOnly, q.Get("fields_only"))
			d.lists++
			d.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": matched, "total": len(matched)})

		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/beads/")
			body, _ := io.ReadAll(r.Body)
			var patch map[string]json.RawMessage
			_ = json.Unmarshal(body, &patch)
			var fields map[string]any
			if raw, ok := patch["fields"]; ok {
				_ = json.Unmarshal(raw, &fields)
			}
			d.mu.Lock()
			d.updates = append(d.updates, daemonUpdate{BeadID: id, Fields: fields})
			d.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})

		default:
			http.NotFound(w, r)
		}
	})
}

func beadFields(b map[string]any) map[string]string {
	out := map[string]string{}
	raw, ok := b["fields"]
	if !ok {
		return out
	}
	switch v := raw.(type) {
	case json.RawMessage:
		var m map[string]any
		_ = json.Unmarshal(v, &m)
		for k, val := range m {
			out[k], _ = val.(string)
		}
	case map[string]any:
		for k, val := range v {
			out[k], _ = val.(string)
		}
	}
	return out
}

// projectFieldsOnly mirrors the daemon's fields_only projection: a copy of b
// whose fields carry only the csv keys (b itself when keys is empty).
func projectFieldsOnly(b map[string]any, keys string) map[string]any {
	if keys == "" {
		return b
	}
	all := beadFields(b)
	kept := map[string]string{}
	for _, k := range strings.Split(keys, ",") {
		if v, ok := all[k]; ok {
			kept[k] = v
		}
	}
	raw, _ := json.Marshal(kept)
	out := maps.Clone(b)
	out["fields"] = json.RawMessage(raw)
	return out
}

func beadHasLabel(b map[string]any, want string) bool {
	raw, ok := b["labels"]
	if !ok {
		return false
	}
	if arr, ok := raw.([]string); ok {
		return slices.Contains(arr, want)
	}
	return false
}

// agentBead builds an agent bead with the given ownership/spawn fields.
func agentBead(id, agentName string, costUSD string, fields map[string]string) map[string]any {
	f := map[string]any{"agent": agentName, "total_cost_usd": costUSD}
	for k, v := range fields {
		f[k] = v
	}
	raw, _ := json.Marshal(f)
	return map[string]any{
		"id": id, "title": id, "type": "agent", "status": "closed",
		"fields": json.RawMessage(raw),
	}
}

func gardenerBead(id string) map[string]any {
	raw, _ := json.Marshal(map[string]any{"role": "gardener"})
	return map[string]any{
		"id": id, "title": id, "type": "trigger", "status": "open",
		"labels": []string{"role:gardener"},
		"fields": json.RawMessage(raw),
	}
}

// mustIndex loads the agent snapshot the roll-up walks.
func mustIndex(t *testing.T, cs *gardenerSpendSync) *agentSpendIndex {
	t.Helper()
	idx, err := cs.loadAgentSpendIndex(context.Background())
	if err != nil {
		t.Fatalf("loadAgentSpendIndex: %v", err)
	}
	return idx
}

func newTestGardenerSpendSync(t *testing.T, d *gardenerSpendDaemon) (*gardenerSpendSync, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(d.handler())
	t.Cleanup(srv.Close)
	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	cs := newGardenerSpendSync(client, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return cs, srv
}

// TestRollupGardenerSpend_IterationsPlusCrew verifies the core contract: a
// gardener's spend is the sum of its iteration runs PLUS the crew (and
// crew-of-crew) those runs transitively spawned.
func TestRollupGardenerSpend_IterationsPlusCrew(t *testing.T) {
	d := &gardenerSpendDaemon{}
	d.beads = []map[string]any{
		gardenerBead("gd-cap"),
		// Two iteration runs owned by the gardener (one via trigger_id, one via
		// the legacy schedule_id key) — both must be counted.
		agentBead("gd-it1", "cap-run-1", "1.50", map[string]string{"trigger_id": "gd-cap"}),
		agentBead("gd-it2", "cap-run-2", "2.00", map[string]string{"schedule_id": "gd-cap"}),
		// Crew spawned by run 1, and crew-of-crew spawned by that crew.
		agentBead("gd-crew1", "crew-a", "0.50", map[string]string{"spawned_by": "cap-run-1"}),
		agentBead("gd-crew2", "crew-b", "0.25", map[string]string{"spawned_by": "crew-a"}),
		// An unrelated agent owned by no gardener in this set — must NOT count.
		agentBead("gd-other", "other", "9.99", map[string]string{"spawned_by": "someone-else"}),
	}
	cs, _ := newTestGardenerSpendSync(t, d)

	spend, walked, capped := mustIndex(t, cs).rollupGardenerSpend("gd-cap")
	if capped {
		t.Fatalf("unexpected cap hit")
	}
	// 1.50 + 2.00 + 0.50 + 0.25 = 4.25
	if spend != 4.25 {
		t.Fatalf("spend = %v, want 4.25", spend)
	}
	if walked != 4 {
		t.Fatalf("walked = %d agents, want 4", walked)
	}
}

// TestRollupGardenerSpend_ReadsOnlyWalkFields pins gd-6PXLuR3juD: every list
// the roll-up and listGardeners send asks for fields_only, and the sum is
// unchanged when the daemon returns nothing but those keys.
func TestRollupGardenerSpend_ReadsOnlyWalkFields(t *testing.T) {
	d := &gardenerSpendDaemon{}
	d.beads = []map[string]any{
		gardenerBead("gd-cap"),
		agentBead("gd-it1", "cap-run-1", "1.50", map[string]string{"trigger_id": "gd-cap", "prompt": "a long prompt"}),
		agentBead("gd-crew1", "crew-a", "0.50", map[string]string{"spawned_by": "cap-run-1"}),
	}
	cs, _ := newTestGardenerSpendSync(t, d)

	if _, err := cs.listGardeners(context.Background()); err != nil {
		t.Fatalf("listGardeners: %v", err)
	}
	spend, walked, _ := mustIndex(t, cs).rollupGardenerSpend("gd-cap")
	if spend != 2.00 || walked != 2 {
		t.Fatalf("spend=%v walked=%d, want 2.00 over 2 agents", spend, walked)
	}
	want := strings.Join(gardenerSpendWalkFields, ",")
	wantGardener := gardenerSpendField + "," + gardenerSpendSyncedAtField
	for i, got := range d.fieldsOnly {
		if i < 4 {
			if got != wantGardener {
				t.Errorf("listGardeners query %d fields_only = %q, want %q", i, got, wantGardener)
			}
			continue
		}
		if got != want {
			t.Errorf("walk query %d fields_only = %q, want %q", i, got, want)
		}
	}
}

// TestRollupGardenerSpend_CycleSafe verifies a spawned_by cycle (agent names
// referencing each other) terminates and double-counts nothing.
func TestRollupGardenerSpend_CycleSafe(t *testing.T) {
	d := &gardenerSpendDaemon{}
	d.beads = []map[string]any{
		gardenerBead("gd-cap"),
		// run "a" is the gardener's iteration; crew "b" spawned by a; crew "a"
		// (name reused) spawned by b — a cycle a→b→a on the name frontier.
		agentBead("gd-it1", "a", "1.00", map[string]string{"trigger_id": "gd-cap"}),
		agentBead("gd-c1", "b", "1.00", map[string]string{"spawned_by": "a"}),
		agentBead("gd-c2", "a", "1.00", map[string]string{"spawned_by": "b"}),
	}
	cs, _ := newTestGardenerSpendSync(t, d)

	spend, walked, _ := mustIndex(t, cs).rollupGardenerSpend("gd-cap")
	// All three beads counted once (deduped by bead id); the cycle on the name
	// frontier must not loop forever or re-count.
	if spend != 3.00 {
		t.Fatalf("spend = %v, want 3.00", spend)
	}
	if walked != 3 {
		t.Fatalf("walked = %d, want 3", walked)
	}
}

// TestSyncAll_ListCountIndependentOfTreeSize pins gd-jd0BGkgmV5: a pass lists
// the gardeners (two types x two match keys) and pages the agents once, however
// many agents each gardener's tree holds; the walk itself sends no list.
func TestSyncAll_ListCountIndependentOfTreeSize(t *testing.T) {
	d := &gardenerSpendDaemon{}
	d.beads = []map[string]any{gardenerBead("gd-cap"), gardenerBead("gd-cap2")}
	for i := range 40 {
		run := fmt.Sprintf("cap-run-%d", i)
		d.beads = append(d.beads,
			agentBead(fmt.Sprintf("gd-it%d", i), run, "1.00", map[string]string{"trigger_id": "gd-cap"}),
			agentBead(fmt.Sprintf("gd-crew%d", i), fmt.Sprintf("crew-%d", i), "0.50", map[string]string{"spawned_by": run}))
	}
	cs, _ := newTestGardenerSpendSync(t, d)

	cs.syncAll(context.Background())

	if d.lists != 5 {
		t.Errorf("syncAll sent %d lists, want 5 (4 gardener lists + 1 agent page)", d.lists)
	}
	var got string
	for _, u := range d.updates {
		if u.BeadID == "gd-cap" {
			got, _ = u.Fields[gardenerSpendField].(string)
		}
	}
	if got != "60.0000" {
		t.Errorf("gd-cap %s = %q, want 60.0000 (40 runs x 1.00 + 40 crew x 0.50)", gardenerSpendField, got)
	}
}

// TestSyncAll_WritesGardenerSpendField verifies syncAll writes gardener_spend_usd
// + gardener_spend_synced_at onto the gardener bead.
func TestSyncAll_WritesGardenerSpendField(t *testing.T) {
	d := &gardenerSpendDaemon{}
	d.beads = []map[string]any{
		gardenerBead("gd-cap"),
		agentBead("gd-it1", "cap-run-1", "3.33", map[string]string{"trigger_id": "gd-cap"}),
	}
	cs, _ := newTestGardenerSpendSync(t, d)

	cs.syncAll(context.Background())

	d.mu.Lock()
	defer d.mu.Unlock()
	var found bool
	for _, u := range d.updates {
		if u.BeadID != "gd-cap" {
			continue
		}
		found = true
		if got, ok := u.Fields[gardenerSpendField]; !ok {
			t.Fatalf("update missing %s field: %+v", gardenerSpendField, u.Fields)
		} else if s, isStr := got.(string); !isStr {
			// gardener_spend_usd is a STRING-typed schema field; the daemon
			// rejects a raw float64 with HTTP 400 (ka-0000006142). Assert the
			// wire value is a string so the write can never regress to a number.
			t.Fatalf("%s must be written as a string, got %T (%v)", gardenerSpendField, got, got)
		} else if s != "3.3300" {
			t.Fatalf("%s = %q, want %q", gardenerSpendField, s, "3.3300")
		}
		if _, ok := u.Fields[gardenerSpendSyncedAtField]; !ok {
			t.Fatalf("update missing %s field", gardenerSpendSyncedAtField)
		}
	}
	if !found {
		t.Fatalf("no update for gd-cap; updates=%+v", d.updates)
	}
}

// TestGardenerSpendNeedsWrite pins gd-7smOFGjlD3: an unchanged figure is
// re-written only as a heartbeat on an open gardener, never on a closed one.
func TestGardenerSpendNeedsWrite(t *testing.T) {
	now := time.Date(2026, 11, 4, 16, 0, 0, 0, time.UTC)
	fresh := now.Add(-10 * time.Minute).Format(time.RFC3339)
	stale := now.Add(-gardenerSpendHeartbeat).Format(time.RFC3339)
	cases := []struct {
		name   string
		status string
		fields map[string]string
		figure string
		want   bool
	}{
		{"closed unchanged skips", "closed", map[string]string{gardenerSpendField: "0.0000", gardenerSpendSyncedAtField: stale}, "0.0000", false},
		{"closed changed writes", "closed", map[string]string{gardenerSpendField: "1.0000", gardenerSpendSyncedAtField: fresh}, "1.2500", true},
		{"open unchanged fresh skips", "open", map[string]string{gardenerSpendField: "1.0000", gardenerSpendSyncedAtField: fresh}, "1.0000", false},
		{"open unchanged stale writes", "open", map[string]string{gardenerSpendField: "1.0000", gardenerSpendSyncedAtField: stale}, "1.0000", true},
		{"open unchanged unstamped writes", "open", map[string]string{gardenerSpendField: "1.0000"}, "1.0000", true},
		{"open changed writes", "open", map[string]string{gardenerSpendField: "1.0000", gardenerSpendSyncedAtField: fresh}, "2.0000", true},
		{"no figure writes", "closed", map[string]string{}, "0.0000", true},
	}
	for _, tc := range cases {
		b := &beadsapi.BeadDetail{ID: "gd-cap", Status: tc.status, Fields: tc.fields}
		if got := gardenerSpendNeedsWrite(b, tc.figure, now); got != tc.want {
			t.Errorf("%s: gardenerSpendNeedsWrite = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestSyncAll_SkipsClosedUnchangedGardener pins gd-7smOFGjlD3 end to end: a pass
// sends no update for a closed gardener whose figure has not moved, and still
// writes one whose figure has.
func TestSyncAll_SkipsClosedUnchangedGardener(t *testing.T) {
	old := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	closedGardener := func(id, figure string) map[string]any {
		b := gardenerBead(id)
		b["status"] = "closed"
		raw, _ := json.Marshal(map[string]any{"role": "gardener", gardenerSpendField: figure, gardenerSpendSyncedAtField: old})
		b["fields"] = json.RawMessage(raw)
		return b
	}
	d := &gardenerSpendDaemon{}
	d.beads = []map[string]any{
		closedGardener("gd-kept", "0.0000"),
		closedGardener("gd-moved", "1.0000"),
		agentBead("gd-it1", "moved-run-1", "1.50", map[string]string{"trigger_id": "gd-moved"}),
	}
	cs, _ := newTestGardenerSpendSync(t, d)

	cs.syncAll(context.Background())

	d.mu.Lock()
	defer d.mu.Unlock()
	var ids []string
	for _, u := range d.updates {
		ids = append(ids, u.BeadID)
	}
	if !slices.Equal(ids, []string{"gd-moved"}) {
		t.Fatalf("updated %v, want only gd-moved", ids)
	}
}

// TestParseGardenerSpendCost covers the cost-field parse edge cases.
func TestParseGardenerSpendCost(t *testing.T) {
	cases := map[string]float64{
		"":      0,
		"1.25":  1.25,
		"-5":    0, // negative clamped to 0
		"junk":  0,
		"0":     0,
		"100.5": 100.5,
	}
	for in, want := range cases {
		if got := parseGardenerSpendCost(in); got != want {
			t.Errorf("parseGardenerSpendCost(%q) = %v, want %v", in, got, want)
		}
	}
}
