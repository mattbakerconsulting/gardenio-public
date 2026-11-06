package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/presence"
)

// decodeSnapshot pulls the response body into a generic map so tests can
// assert on the per-tile {data, spec, meta} envelope without re-declaring
// every nested struct.
func decodeSnapshot(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode snapshot: %v; body=%s", err, string(body))
	}
	return out
}

func TestHandleHealthSnapshot_Empty(t *testing.T) {
	_, _, h := newTestServer()

	rec := doJSON(t, h, http.MethodGet, "/v1/health/snapshot", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	out := decodeSnapshot(t, rec.Body.Bytes())
	if got := out["schema_version"]; got != float64(3) {
		t.Errorf("schema_version: got %v, want 3", got)
	}
	if _, ok := out["now"].(string); !ok {
		t.Errorf("now: missing or not string; got %T", out["now"])
	}

	for _, tile := range []string{"fleet", "decisions", "events", "incidents", "kbeads", "bridges", "gardeners_cycles"} {
		t.Run(tile, func(t *testing.T) {
			env, ok := out[tile].(map[string]any)
			if !ok {
				t.Fatalf("tile %q: missing or wrong shape; got %T", tile, out[tile])
			}
			if _, ok := env["data"]; !ok {
				t.Errorf("tile %q: missing data", tile)
			}
			if _, ok := env["spec"].(map[string]any); !ok {
				t.Errorf("tile %q: missing or wrong spec", tile)
			}
			if _, ok := env["meta"].(map[string]any); !ok {
				t.Errorf("tile %q: missing or wrong meta", tile)
			}
		})
	}

	// Empty fleet → alive=0, stuck=0.
	fleet := out["fleet"].(map[string]any)["data"].(map[string]any)
	if fleet["agents_alive"] != float64(0) || fleet["agents_stuck"] != float64(0) {
		t.Errorf("empty fleet: got %v", fleet)
	}

	// Empty decisions → pending=0, next_3 is empty list (not null).
	dec := out["decisions"].(map[string]any)["data"].(map[string]any)
	if dec["pending"] != float64(0) {
		t.Errorf("empty decisions.pending: got %v", dec["pending"])
	}
	if next3, ok := dec["next_3"].([]any); !ok || len(next3) != 0 {
		t.Errorf("empty decisions.next_3: got %v", dec["next_3"])
	}

	// kbeads tile reflects ping success.
	kb := out["kbeads"].(map[string]any)["data"].(map[string]any)
	if kb["ok"] != true {
		t.Errorf("kbeads.ok: got %v, want true", kb["ok"])
	}
}

func TestHandleHealthSnapshot_Populated(t *testing.T) {
	s, ms, h := newTestServer()

	now := time.Now().UTC()

	// Two open decisions, one closed.
	ms.beads["gd-d1"] = &model.Bead{
		ID:        "gd-d1",
		Title:     "Decide A",
		Type:      model.TypeDecision,
		Status:    model.StatusOpen,
		CreatedAt: now.Add(-2 * time.Minute),
	}
	ms.beads["gd-d2"] = &model.Bead{
		ID:        "gd-d2",
		Title:     "Decide B",
		Type:      model.TypeDecision,
		Status:    model.StatusInProgress,
		CreatedAt: now.Add(-30 * time.Second),
	}
	ms.beads["gd-d3"] = &model.Bead{
		ID:        "gd-d3",
		Title:     "Decide C (closed)",
		Type:      model.TypeDecision,
		Status:    model.StatusClosed,
		CreatedAt: now.Add(-1 * time.Hour),
	}

	// Two release-bug bugs, one agent-crash bug, all in-window.
	since := now.Add(-1 * time.Hour)
	ms.beads["gd-b1"] = &model.Bead{
		ID: "gd-b1", Title: "Release bug 1", Type: model.TypeBug,
		Status: model.StatusOpen, CreatedAt: since,
	}
	ms.labels["gd-b1"] = []string{"release-bug"}
	ms.beads["gd-b2"] = &model.Bead{
		ID: "gd-b2", Title: "Release bug 2", Type: model.TypeBug,
		Status: model.StatusOpen, CreatedAt: since.Add(time.Minute),
	}
	ms.labels["gd-b2"] = []string{"release-bug"}
	ms.beads["gd-b3"] = &model.Bead{
		ID: "gd-b3", Title: "Agent crash", Type: model.TypeBug,
		Status: model.StatusOpen, CreatedAt: since.Add(2 * time.Minute),
	}
	ms.labels["gd-b3"] = []string{"agent-crash"}

	// Old release-bug (>24h ago) — must be excluded.
	ms.beads["gd-b4"] = &model.Bead{
		ID: "gd-b4", Title: "Old bug", Type: model.TypeBug,
		Status: model.StatusOpen, CreatedAt: now.Add(-48 * time.Hour),
	}
	ms.labels["gd-b4"] = []string{"release-bug"}

	// One agent in presence (alive).
	s.Presence.RecordHookEvent(presence.HookEvent{
		Actor: "agent-1", HookType: "PreToolUse",
	})

	// Some events for the histogram.
	ms.events = []*model.Event{
		{ID: 1, Topic: "x", BeadID: "gd-a", CreatedAt: now.Add(-2 * time.Minute)},
		{ID: 2, Topic: "x", BeadID: "gd-a", CreatedAt: now.Add(-1 * time.Minute)},
		{ID: 3, Topic: "x", BeadID: "gd-a", CreatedAt: now.Add(-2 * time.Hour)},
	}

	rec := doJSON(t, h, http.MethodGet, "/v1/health/snapshot", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	out := decodeSnapshot(t, rec.Body.Bytes())

	// Fleet: one alive agent, no stuck.
	fleet := out["fleet"].(map[string]any)["data"].(map[string]any)
	if fleet["agents_alive"] != float64(1) {
		t.Errorf("agents_alive: got %v, want 1", fleet["agents_alive"])
	}
	if fleet["agents_stuck"] != float64(0) {
		t.Errorf("agents_stuck: got %v, want 0", fleet["agents_stuck"])
	}

	// Decisions: 2 pending, next_3 has 2 entries.
	dec := out["decisions"].(map[string]any)["data"].(map[string]any)
	if dec["pending"] != float64(2) {
		t.Errorf("decisions.pending: got %v, want 2", dec["pending"])
	}
	next3 := dec["next_3"].([]any)
	if len(next3) != 2 {
		t.Fatalf("next_3 length: got %d, want 2", len(next3))
	}

	// Incidents: 2 release bugs in window, 1 agent crash; old bug excluded.
	inc := out["incidents"].(map[string]any)["data"].(map[string]any)
	relBugs := inc["recent_release_bugs"].([]any)
	if len(relBugs) != 2 {
		t.Errorf("recent_release_bugs: got %d, want 2", len(relBugs))
	}
	crashes := inc["recent_agent_crashes"].([]any)
	if len(crashes) != 1 {
		t.Errorf("recent_agent_crashes: got %d, want 1", len(crashes))
	}

	// Events: histogram for last 5m present, 24h total ≥ 2 (last-5m events
	// also fall inside the 24h window).
	ev := out["events"].(map[string]any)["data"].(map[string]any)
	if _, ok := ev["histogram"].([]any); !ok {
		t.Errorf("events.histogram: missing or wrong type; got %T", ev["histogram"])
	}
	total, _ := ev["total"].(float64)
	if total < 2 {
		t.Errorf("events.total (24h): got %v, want >= 2", total)
	}
}

func TestHandleHealthSnapshot_Cache(t *testing.T) {
	s, ms, h := newTestServer()

	// First call populates the cache.
	rec1 := doJSON(t, h, http.MethodGet, "/v1/health/snapshot", nil)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first call: %d %s", rec1.Code, rec1.Body.String())
	}
	out1 := decodeSnapshot(t, rec1.Body.Bytes())

	// Inject a new decision that *would* change the count if rebuilt.
	ms.beads["gd-cached-d1"] = &model.Bead{
		ID:        "gd-cached-d1",
		Title:     "After-cache",
		Type:      model.TypeDecision,
		Status:    model.StatusOpen,
		CreatedAt: time.Now().UTC(),
	}

	// Second call within 10s must serve the cached payload (pending unchanged).
	rec2 := doJSON(t, h, http.MethodGet, "/v1/health/snapshot", nil)
	if rec2.Code != http.StatusOK {
		t.Fatalf("second call: %d %s", rec2.Code, rec2.Body.String())
	}
	out2 := decodeSnapshot(t, rec2.Body.Bytes())

	got1 := out1["decisions"].(map[string]any)["data"].(map[string]any)["pending"]
	got2 := out2["decisions"].(map[string]any)["data"].(map[string]any)["pending"]
	if got1 != got2 {
		t.Errorf("cache miss: pending changed across calls (got %v -> %v)", got1, got2)
	}
	if got1 != float64(0) {
		t.Errorf("first-call decisions.pending: got %v, want 0", got1)
	}

	// Force cache invalidation by aging every scope entry past the TTL.
	if s.healthSnapshot != nil {
		s.healthSnapshot.mu.Lock()
		for k, e := range s.healthSnapshot.entries {
			e.cachedAt = time.Time{}
			s.healthSnapshot.entries[k] = e
		}
		s.healthSnapshot.mu.Unlock()
	} else {
		t.Fatalf("healthSnapshot cache not initialised after first call")
	}

	rec3 := doJSON(t, h, http.MethodGet, "/v1/health/snapshot", nil)
	if rec3.Code != http.StatusOK {
		t.Fatalf("third call: %d %s", rec3.Code, rec3.Body.String())
	}
	out3 := decodeSnapshot(t, rec3.Body.Bytes())
	got3 := out3["decisions"].(map[string]any)["data"].(map[string]any)["pending"]
	if got3 != float64(1) {
		t.Errorf("post-invalidation decisions.pending: got %v, want 1", got3)
	}
}

func TestHandleHealthSnapshot_StoreError(t *testing.T) {
	_, ms, h := newTestServer()
	ms.histogramErr = errors.New("db offline")

	rec := doJSON(t, h, http.MethodGet, "/v1/health/snapshot", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleHealthSnapshot_Bridges_Empty pins the bridges-tile contract when
// no bridge has emitted an event: the tile must still render exactly the four
// canonical rows (slack-bridge, jira-bridge, gitlab-bridge, controller) with
// last_event_at = null, so a downstream viewer can render an "unknown" pill
// per row without guessing at the set.
func TestHandleHealthSnapshot_Bridges_Empty(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, http.MethodGet, "/v1/health/snapshot", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	out := decodeSnapshot(t, rec.Body.Bytes())
	br := out["bridges"].(map[string]any)["data"].(map[string]any)
	sources, _ := br["sources"].([]any)
	if len(sources) != 4 {
		t.Fatalf("sources length: got %d, want 4 (one per canonical bridge)", len(sources))
	}
	wantNames := []string{"slack-bridge", "jira-bridge", "gitlab-bridge", "controller"}
	for i, raw := range sources {
		row := raw.(map[string]any)
		if row["name"] != wantNames[i] {
			t.Errorf("sources[%d].name: got %v, want %s", i, row["name"], wantNames[i])
		}
		if got, ok := row["alias"].(string); !ok || got == "" {
			t.Errorf("sources[%d].alias: got %v, want non-empty alias", i, row["alias"])
		}
		if row["last_event_at"] != nil {
			t.Errorf("sources[%d].last_event_at: got %v, want nil (no events)", i, row["last_event_at"])
		}
	}
}

// TestHandleHealthSnapshot_Bridges_LastEventAt verifies that the bridges tile
// reports the most recent events.created_at per bridge alias, resolved through
// operator_aliases. The slack-bridge and controller rows must echo the seeded
// timestamps; the other two rows must remain null.
func TestHandleHealthSnapshot_Bridges_LastEventAt(t *testing.T) {
	_, ms, h := newTestServer()

	// Seed two bridge operators with aliases. Mock store uses kind\x00subject.
	if ms.operators == nil {
		ms.operators = map[int64]struct{ Kind, Subject string }{}
		ms.operatorByPair = map[string]int64{}
	}
	ms.operators[10] = struct{ Kind, Subject string }{"system", "slack-bridge"}
	ms.operators[20] = struct{ Kind, Subject string }{"fleet", "gardenio"}
	if ms.operatorAliases == nil {
		ms.operatorAliases = map[string]int64{}
	}
	ms.operatorAliases["system\x00slack-bridge"] = 10
	ms.operatorAliases["fleet\x00gardenio"] = 20

	// Two slack-bridge events; the controller event is older. The bridges
	// tile must surface the newer timestamps per alias.
	slackOld := time.Now().UTC().Add(-10 * time.Minute)
	slackNew := time.Now().UTC().Add(-1 * time.Minute)
	ctrlAt := time.Now().UTC().Add(-30 * time.Second)
	idSlack := int64(10)
	idCtrl := int64(20)
	ms.events = []*model.Event{
		{ID: 1, Topic: "x", BeadID: "gd-a", CreatedAt: slackOld, OperatedByID: &idSlack},
		{ID: 2, Topic: "x", BeadID: "gd-a", CreatedAt: slackNew, OperatedByID: &idSlack},
		{ID: 3, Topic: "x", BeadID: "gd-b", CreatedAt: ctrlAt, OperatedByID: &idCtrl},
	}

	rec := doJSON(t, h, http.MethodGet, "/v1/health/snapshot", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	out := decodeSnapshot(t, rec.Body.Bytes())
	br := out["bridges"].(map[string]any)["data"].(map[string]any)
	sources := br["sources"].([]any)

	byName := map[string]map[string]any{}
	for _, raw := range sources {
		row := raw.(map[string]any)
		byName[row["name"].(string)] = row
	}

	slack := byName["slack-bridge"]
	if slack == nil {
		t.Fatal("slack-bridge row missing")
	}
	if slack["alias"] != "system:slack-bridge" {
		t.Errorf("slack alias: got %v, want system:slack-bridge", slack["alias"])
	}
	gotSlackAt, ok := slack["last_event_at"].(string)
	if !ok {
		t.Fatalf("slack last_event_at: got %T %v, want string", slack["last_event_at"], slack["last_event_at"])
	}
	parsed, err := time.Parse(time.RFC3339Nano, gotSlackAt)
	if err != nil {
		t.Fatalf("parse slack last_event_at %q: %v", gotSlackAt, err)
	}
	if !parsed.Equal(slackNew) {
		t.Errorf("slack last_event_at: got %s, want %s (newest of two events)", parsed, slackNew)
	}

	ctrl := byName["controller"]
	if ctrl == nil || ctrl["alias"] != "fleet:gardenio" {
		t.Fatalf("controller row missing or wrong alias: %v", ctrl)
	}
	if _, ok := ctrl["last_event_at"].(string); !ok {
		t.Errorf("controller last_event_at: got %v, want non-null", ctrl["last_event_at"])
	}

	// Bridges with no operator_aliases entry → null.
	for _, name := range []string{"jira-bridge", "gitlab-bridge"} {
		row := byName[name]
		if row == nil {
			t.Fatalf("%s row missing", name)
		}
		if row["last_event_at"] != nil {
			t.Errorf("%s last_event_at: got %v, want null", name, row["last_event_at"])
		}
	}
}

// TestHandleHealthSnapshot_GardenersCycles_Empty pins the gardeners_cycles
// tile contract when no schedule beads exist: the tile renders with an
// empty roles list and the standard {data, spec, meta} envelope.
func TestHandleHealthSnapshot_GardenersCycles_Empty(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, http.MethodGet, "/v1/health/snapshot", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	out := decodeSnapshot(t, rec.Body.Bytes())
	cc := out["gardeners_cycles"].(map[string]any)["data"].(map[string]any)
	roles, ok := cc["roles"].([]any)
	if !ok {
		t.Fatalf("gardeners_cycles.data.roles: got %T, want []any", cc["roles"])
	}
	if len(roles) != 0 {
		t.Errorf("gardeners_cycles roles: got %d entries, want 0", len(roles))
	}
}

// TestHandleHealthSnapshot_GardenersCycles_RollsUpByRole verifies the tile
// aggregates run_log entries across schedule beads by their `role` field,
// caps each role at healthGardenersCyclesLimit (6) entries, classifies
// pass/fail via outcome=="done", and sorts roles by most-recent-cycle desc.
func TestHandleHealthSnapshot_GardenersCycles_RollsUpByRole(t *testing.T) {
	_, ms, h := newTestServer()

	now := time.Now().UTC()

	// gardener-delivery: one schedule with 8 cycles (mix of pass/fail). The
	// tile must trim to the most recent 6 newest-first.
	deliveryRunLog := `[
		{"ended_at":"` + now.Add(-8*time.Minute).Format(time.RFC3339Nano) + `","outcome":"done"},
		{"ended_at":"` + now.Add(-7*time.Minute).Format(time.RFC3339Nano) + `","outcome":"done"},
		{"ended_at":"` + now.Add(-6*time.Minute).Format(time.RFC3339Nano) + `","outcome":"failed"},
		{"ended_at":"` + now.Add(-5*time.Minute).Format(time.RFC3339Nano) + `","outcome":"done"},
		{"ended_at":"` + now.Add(-4*time.Minute).Format(time.RFC3339Nano) + `","outcome":"done"},
		{"ended_at":"` + now.Add(-3*time.Minute).Format(time.RFC3339Nano) + `","outcome":"done"},
		{"ended_at":"` + now.Add(-2*time.Minute).Format(time.RFC3339Nano) + `","outcome":"failed"},
		{"ended_at":"` + now.Add(-1*time.Minute).Format(time.RFC3339Nano) + `","outcome":"done"}
	]`
	deliveryFields, _ := json.Marshal(map[string]string{
		"role":    "gardener-delivery",
		"run_log": deliveryRunLog,
	})
	ms.beads["gd-sched-delivery"] = &model.Bead{
		ID:        "gd-sched-delivery",
		Title:     "gardener-delivery schedule",
		Type:      model.BeadType("schedule"),
		Status:    model.StatusOpen,
		CreatedAt: now.Add(-1 * time.Hour),
		Fields:    deliveryFields,
	}

	// gardener-bugfix: split across two schedules — the tile must merge them
	// into one row. Most recent cycle is older than gardener-delivery's, so
	// gardener-bugfix sorts second.
	bugfixRunLog1 := `[
		{"ended_at":"` + now.Add(-20*time.Minute).Format(time.RFC3339Nano) + `","outcome":"failed"},
		{"ended_at":"` + now.Add(-15*time.Minute).Format(time.RFC3339Nano) + `","outcome":"done"}
	]`
	bugfixFields1, _ := json.Marshal(map[string]string{
		"role":    "gardener-bugfix",
		"run_log": bugfixRunLog1,
	})
	ms.beads["gd-sched-bugfix-1"] = &model.Bead{
		ID:        "gd-sched-bugfix-1",
		Title:     "bugfix sched 1",
		Type:      model.BeadType("schedule"),
		Status:    model.StatusOpen,
		CreatedAt: now.Add(-1 * time.Hour),
		Fields:    bugfixFields1,
	}
	bugfixRunLog2 := `[
		{"ended_at":"` + now.Add(-12*time.Minute).Format(time.RFC3339Nano) + `","outcome":"timeout"}
	]`
	bugfixFields2, _ := json.Marshal(map[string]string{
		"role":    "gardener-bugfix",
		"run_log": bugfixRunLog2,
	})
	ms.beads["gd-sched-bugfix-2"] = &model.Bead{
		ID:        "gd-sched-bugfix-2",
		Title:     "bugfix sched 2",
		Type:      model.BeadType("schedule"),
		Status:    model.StatusOpen,
		CreatedAt: now.Add(-1 * time.Hour),
		Fields:    bugfixFields2,
	}

	// A schedule with no `role` field — falls under the "—" placeholder. Has
	// an empty run_log so it surfaces as a role with zero cycles rather than
	// disappearing entirely.
	emptyFields, _ := json.Marshal(map[string]string{
		"run_log": "",
	})
	ms.beads["gd-sched-noroles"] = &model.Bead{
		ID:        "gd-sched-noroles",
		Title:     "no role",
		Type:      model.BeadType("schedule"),
		Status:    model.StatusOpen,
		CreatedAt: now.Add(-1 * time.Hour),
		Fields:    emptyFields,
	}

	// A closed schedule with run_log — must be excluded.
	closedFields, _ := json.Marshal(map[string]string{
		"role":    "gardener-delivery",
		"run_log": `[{"ended_at":"` + now.Format(time.RFC3339Nano) + `","outcome":"done"}]`,
	})
	ms.beads["gd-sched-closed"] = &model.Bead{
		ID:        "gd-sched-closed",
		Title:     "closed sched",
		Type:      model.BeadType("schedule"),
		Status:    model.StatusClosed,
		CreatedAt: now.Add(-1 * time.Hour),
		Fields:    closedFields,
	}

	rec := doJSON(t, h, http.MethodGet, "/v1/health/snapshot", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	out := decodeSnapshot(t, rec.Body.Bytes())
	cc := out["gardeners_cycles"].(map[string]any)["data"].(map[string]any)
	roles := cc["roles"].([]any)
	if len(roles) != 3 {
		t.Fatalf("roles: got %d, want 3 (gardener-delivery, gardener-bugfix, —); roles=%v", len(roles), roles)
	}

	byRole := map[string]map[string]any{}
	for _, raw := range roles {
		r := raw.(map[string]any)
		byRole[r["role"].(string)] = r
	}

	// gardener-delivery: 8 cycles trimmed to 6, newest-first. The newest is
	// the -1m "done" entry; the oldest in the trimmed window is -6m
	// "failed".
	delivery := byRole["gardener-delivery"]
	if delivery == nil {
		t.Fatal("gardener-delivery row missing")
	}
	cycles := delivery["cycles"].([]any)
	if len(cycles) != 6 {
		t.Fatalf("gardener-delivery cycles: got %d, want 6", len(cycles))
	}
	first := cycles[0].(map[string]any)
	if first["outcome"] != "done" || first["pass"] != true {
		t.Errorf("gardener-delivery newest cycle: got outcome=%v pass=%v, want done/true", first["outcome"], first["pass"])
	}
	// Closed-schedule entry must not have leaked in: the newest cycle's
	// ended_at must be < now (the closed sched was the only one with
	// ended_at == now).
	if got, _ := first["ended_at"].(string); got != "" {
		parsed, err := time.Parse(time.RFC3339Nano, got)
		if err != nil {
			t.Errorf("parse newest ended_at %q: %v", got, err)
		} else if !parsed.Before(now) {
			t.Errorf("closed schedule leaked: newest ended_at %s is not < now %s", parsed, now)
		}
	}

	// gardener-bugfix: merged from two schedules, sorted newest-first across
	// both. Most recent is -12m "timeout" (fail), then -15m "done", then
	// -20m "failed".
	bugfix := byRole["gardener-bugfix"]
	if bugfix == nil {
		t.Fatal("gardener-bugfix row missing")
	}
	bcycles := bugfix["cycles"].([]any)
	if len(bcycles) != 3 {
		t.Fatalf("gardener-bugfix cycles: got %d, want 3", len(bcycles))
	}
	if got := bcycles[0].(map[string]any)["outcome"]; got != "timeout" {
		t.Errorf("gardener-bugfix newest outcome: got %v, want timeout", got)
	}
	if pass := bcycles[0].(map[string]any)["pass"]; pass != false {
		t.Errorf("gardener-bugfix newest pass: got %v, want false (timeout != done)", pass)
	}

	// The no-role schedule becomes the "—" row with zero cycles.
	noRole := byRole["—"]
	if noRole == nil {
		t.Fatal("—-role row missing")
	}
	if ncycles, _ := noRole["cycles"].([]any); len(ncycles) != 0 {
		t.Errorf("—-role cycles: got %d, want 0", len(ncycles))
	}

	// Role order: most-recent-cycle desc. gardener-delivery (-1m) before
	// gardener-bugfix (-12m) before "—" (no cycles).
	wantOrder := []string{"gardener-delivery", "gardener-bugfix", "—"}
	for i, want := range wantOrder {
		got := roles[i].(map[string]any)["role"]
		if got != want {
			t.Errorf("role order[%d]: got %v, want %s", i, got, want)
		}
	}
}

// seedCrossProjectHealthBeads plants a pending decision and a release-bug
// incident in each of two distinct projects so a scope regression can assert a
// restricted caller only sees its authorized project's content on the health
// snapshot's decisions + incidents tiles.
func seedCrossProjectHealthBeads(ms *mockStore, now time.Time) {
	ms.beads["gd-dec-gardenio"] = &model.Bead{
		ID: "gd-dec-gardenio", Type: model.TypeDecision, Status: model.StatusOpen,
		Title: "gardenio decision", CreatedAt: now.Add(-2 * time.Hour),
	}
	ms.labels["gd-dec-gardenio"] = []string{"project:gardenio"}
	ms.beads["gd-dec-secret"] = &model.Bead{
		ID: "gd-dec-secret", Type: model.TypeDecision, Status: model.StatusOpen,
		Title: "secretproj decision", CreatedAt: now.Add(-1 * time.Hour),
	}
	ms.labels["gd-dec-secret"] = []string{"project:secretproj"}

	ms.beads["gd-bug-gardenio"] = &model.Bead{
		ID: "gd-bug-gardenio", Type: model.TypeBug, Status: model.StatusOpen,
		Title: "gardenio release bug", CreatedAt: now.Add(-30 * time.Minute),
	}
	ms.labels["gd-bug-gardenio"] = []string{"release-bug", "project:gardenio"}
	ms.beads["gd-bug-secret"] = &model.Bead{
		ID: "gd-bug-secret", Type: model.TypeBug, Status: model.StatusOpen,
		Title: "secretproj release bug", CreatedAt: now.Add(-20 * time.Minute),
	}
	ms.labels["gd-bug-secret"] = []string{"release-bug", "project:secretproj"}
}

func decisionIDsOf(tile healthTile) []string {
	ids := make([]string, 0)
	for _, p := range tile.Data.(decisionsData).Next3 {
		ids = append(ids, p.ID)
	}
	return ids
}

func releaseBugIDsOf(tile healthTile) []string {
	ids := make([]string, 0)
	for _, p := range tile.Data.(incidentsData).RecentReleaseBugs {
		ids = append(ids, p.ID)
	}
	return ids
}

// TestHandleHealthSnapshot_ScopesCrossProjectTiles is the regression for
// ka-0000008145: the decisions + incidents tiles built their ListBeads filters
// WITHOUT scopeListFilter, so a project-scoped RoleViewer received decision and
// bug titles/IDs from projects it is not authorized to read. Both tile builders
// must now flow their filters through s.scopeListFilter. Asserted at the builder
// level (as mywork_test does) with a crafted caller-scope context.
func TestHandleHealthSnapshot_ScopesCrossProjectTiles(t *testing.T) {
	now := time.Now().UTC()

	// A project-restricted OIDC user (allowed_projects=gardenio) must NOT see the
	// secretproj decision or the secretproj release bug.
	t.Run("oidc_restricted", func(t *testing.T) {
		s, ms, _ := newTestServer()
		seedCrossProjectHealthBeads(ms, now)
		makeUserBead(ms, "gd-user-mallory", "mallory@example.com", "gardenio", "crew")

		ctx := oidcCtx("mallory@example.com", []string{RoleViewer})
		dec, err := s.buildDecisionsTile(ctx, now)
		if err != nil {
			t.Fatalf("buildDecisionsTile: %v", err)
		}
		if got := decisionIDsOf(dec); len(got) != 1 || got[0] != "gd-dec-gardenio" {
			t.Errorf("restricted decisions = %v, want [gd-dec-gardenio] (secretproj must be filtered)", got)
		}
		inc, err := s.buildIncidentsTile(ctx, now)
		if err != nil {
			t.Fatalf("buildIncidentsTile: %v", err)
		}
		if got := releaseBugIDsOf(inc); len(got) != 1 || got[0] != "gd-bug-gardenio" {
			t.Errorf("restricted release bugs = %v, want [gd-bug-gardenio] (secretproj must be filtered)", got)
		}
	})

	// A project-bound fleet token scoped to secretproj must ONLY see secretproj.
	t.Run("fleet_token", func(t *testing.T) {
		s, ms, _ := newTestServer()
		seedCrossProjectHealthBeads(ms, now)

		ctx := WithTokenProject(context.Background(), "secretproj")
		dec, err := s.buildDecisionsTile(ctx, now)
		if err != nil {
			t.Fatalf("buildDecisionsTile: %v", err)
		}
		if got := decisionIDsOf(dec); len(got) != 1 || got[0] != "gd-dec-secret" {
			t.Errorf("secretproj token decisions = %v, want [gd-dec-secret] (gardenio must be filtered)", got)
		}
		inc, err := s.buildIncidentsTile(ctx, now)
		if err != nil {
			t.Fatalf("buildIncidentsTile: %v", err)
		}
		if got := releaseBugIDsOf(inc); len(got) != 1 || got[0] != "gd-bug-secret" {
			t.Errorf("secretproj token release bugs = %v, want [gd-bug-secret] (gardenio must be filtered)", got)
		}
	})

	// An unrestricted caller (no token, no OIDC scope) keeps the fleet-wide view.
	t.Run("unrestricted", func(t *testing.T) {
		s, ms, _ := newTestServer()
		seedCrossProjectHealthBeads(ms, now)

		dec, err := s.buildDecisionsTile(context.Background(), now)
		if err != nil {
			t.Fatalf("buildDecisionsTile: %v", err)
		}
		if got := len(decisionIDsOf(dec)); got != 2 {
			t.Errorf("unrestricted decisions count = %d, want 2 (fleet-wide)", got)
		}
		inc, err := s.buildIncidentsTile(context.Background(), now)
		if err != nil {
			t.Fatalf("buildIncidentsTile: %v", err)
		}
		if got := len(releaseBugIDsOf(inc)); got != 2 {
			t.Errorf("unrestricted release bugs count = %d, want 2 (fleet-wide)", got)
		}
	})
}

// TestHandleHealthSnapshot_CacheIsScopeKeyed guards the second half of the
// ka-0000008145 fix: the 10s snapshot cache is a shared store, so keying it by
// caller-scope is what stops a restricted caller from being served an
// unrestricted operator's cached (unscoped) payload — which would silently
// re-open the leak that scopeListFilter closes. An operator populates the cache
// first; a restricted OIDC user hitting it within the TTL must still get only
// its own project's content, not the operator's cached fleet-wide view.
func TestHandleHealthSnapshot_CacheIsScopeKeyed(t *testing.T) {
	now := time.Now().UTC()
	s, ms, _ := newTestServer()
	seedCrossProjectHealthBeads(ms, now)
	makeUserBead(ms, "gd-user-mallory", "mallory@example.com", "gardenio", "crew")

	opCtx := context.Background()
	userCtx := oidcCtx("mallory@example.com", []string{RoleViewer})

	// Distinct scopes must resolve to distinct cache keys.
	opKey := s.healthSnapshotScopeKey(opCtx)
	userKey := s.healthSnapshotScopeKey(userCtx)
	if opKey == userKey {
		t.Fatalf("scope keys collide: operator %q == restricted %q", opKey, userKey)
	}

	// 1. Unrestricted operator populates the cache with the fleet-wide payload.
	opPayload, err := s.buildHealthSnapshot(opCtx, now)
	if err != nil {
		t.Fatalf("operator buildHealthSnapshot: %v", err)
	}
	s.healthSnapshot = &healthSnapshotCache{}
	s.healthSnapshot.write(opKey, now, opPayload)

	// 2. Restricted user within the TTL: a shared slot would hand back the
	// operator's unscoped payload; scope-keyed, this is a miss, so the user
	// rebuilds and sees only gardenio.
	if cached := s.healthSnapshot.read(userKey, now); cached != nil {
		t.Fatalf("restricted caller got the operator's cached payload — cache is not scope-keyed")
	}
	userPayload, err := s.buildHealthSnapshot(userCtx, now)
	if err != nil {
		t.Fatalf("restricted buildHealthSnapshot: %v", err)
	}
	if got := decisionIDsOf(userPayload.Decisions); len(got) != 1 || got[0] != "gd-dec-gardenio" {
		t.Errorf("restricted decisions after operator cached = %v, want [gd-dec-gardenio]", got)
	}
	if got := releaseBugIDsOf(userPayload.Incidents); len(got) != 1 || got[0] != "gd-bug-gardenio" {
		t.Errorf("restricted release bugs after operator cached = %v, want [gd-bug-gardenio]", got)
	}

	// The operator's own key still serves the fleet-wide payload from cache.
	if cached := s.healthSnapshot.read(opKey, now); cached == nil {
		t.Fatalf("operator cache entry evicted unexpectedly")
	} else if got := len(decisionIDsOf(cached.Decisions)); got != 2 {
		t.Errorf("operator cached decisions count = %d, want 2 (fleet-wide)", got)
	}
}

func TestHandleHealthSnapshot_PingFailure_StillReturns200(t *testing.T) {
	_, ms, h := newTestServer()
	ms.pingErr = errors.New("ping failed")

	rec := doJSON(t, h, http.MethodGet, "/v1/health/snapshot", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	out := decodeSnapshot(t, rec.Body.Bytes())
	kb := out["kbeads"].(map[string]any)["data"].(map[string]any)
	if kb["ok"] != false {
		t.Errorf("kbeads.ok: got %v, want false", kb["ok"])
	}
	if kb["error"] != "ping failed" {
		t.Errorf("kbeads.error: got %v", kb["error"])
	}
}
