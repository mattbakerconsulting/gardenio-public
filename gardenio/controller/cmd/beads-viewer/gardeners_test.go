package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/scheduler/progress"
)

func gardenersNow() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) }

// seedGardenersIndexFake builds a fakeDaemon serving two gardeners via the
// type=trigger query: an active "delivery" gardener and a disabled "watchdog".
func seedGardenersIndexFake(t *testing.T) *fakeDaemon {
	t.Helper()
	f := newFake()

	mkGardener := func(id, name, goal string, enabled bool, consecutiveFailures int) *beadsapi.BeadDetail {
		spec := map[string]any{
			"name":                 name,
			"terminal_goal":        goal,
			"runs_spawned":         5,
			"runs_completed":       4,
			"consecutive_failures": consecutiveFailures,
		}
		raw, _ := json.Marshal(spec)
		fields := map[string]string{"role": "gardener", "gardener": string(raw)}
		if !enabled {
			fields["enabled"] = "false"
		}
		return &beadsapi.BeadDetail{
			ID:        id,
			Title:     name,
			Type:      "trigger",
			Status:    "open",
			Labels:    []string{"trigger:schedule", "role:gardener", "project:gardenio"},
			Fields:    fields,
			UpdatedAt: time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
		}
	}

	f.triggerScheduleResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			mkGardener("gd-cap1", "delivery", "drain the ready queue", true, 0),
			mkGardener("gd-cap2", "watchdog", "watch the cycle health", false, 2),
		},
		Total: 2,
	}
	return f
}

func TestHandleGardeners_RendersRowsAndNavLink(t *testing.T) {
	f := seedGardenersIndexFake(t)
	// The "Gardeners" nav link is vibe-only after gb-ux #3 (ka-0000003340);
	// render as a vibe-mode viewer so the nav-link assertion below holds. No
	// page handler content is keyed on VibeMode, so the row assertions are
	// unaffected.
	f.userResp = vibeUserResp()
	mux := mkServerWithNow(t, f, gardenersNow)

	req := httptest.NewRequest("GET", "/gardeners", nil)
	req.Header.Set("X-Auth-Request-Email", authorEmail)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// Both gardeners render with name, goal, and a link to their detail page.
	for _, want := range []string{
		"delivery",
		"drain the ready queue",
		"watchdog",
		"watch the cycle health",
		`href="/gardener/gd-cap1"`,
		`href="/gardener/gd-cap2"`,
		"Gardeners", // page heading + nav link
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}

	// Status chips: cap1 active, cap2 disabled.
	if !strings.Contains(body, "cap-status-active") {
		t.Errorf("missing active status chip")
	}
	if !strings.Contains(body, "cap-status-disabled") {
		t.Errorf("missing disabled status chip")
	}

	// Inline pause/resume toggle (ka-0000007934): every open gardener row carries
	// a toggle posting to /gardener/{id}/pause-toggle. cap1 (enabled) reads
	// "running"; cap2 (disabled) reads "paused".
	for _, want := range []string{
		`id="gardener-pause-gd-cap1"`,
		`id="gardener-pause-gd-cap2"`,
		"/gardener/gd-cap1/pause-toggle",
		"cap-pause-running", // cap1 is enabled
		"cap-pause-paused",  // cap2 is disabled
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing pause-toggle marker %q", want)
		}
	}

	// Last-run outcome column: cap1 (0 consecutive failures, 4 completed) -> ok;
	// cap2 (2 consecutive failures) -> failed.
	if !strings.Contains(body, "last run ok") {
		t.Errorf("missing 'last run ok' outcome for cap1")
	}
	if !strings.Contains(body, "last run failed") {
		t.Errorf("missing 'last run failed' outcome for cap2")
	}

	// gd-JCm3cUwDiF: neither seeded gardener has a fetchable wrap-up headline
	// (LastRunTLDR empty), so the last-run column must fall back to an explicit
	// muted 'no run output' placeholder rather than rendering a silently-blank
	// cell. The column is uniform across every row.
	if got := strings.Count(body, "no run output"); got != 2 {
		t.Errorf("expected 'no run output' placeholder on both blank-TLDR rows, got %d", got)
	}
	// The last-run cell must never be blank: every <td class="cap-col-lastrun">
	// has to carry visible content before its closing tag.
	for _, frag := range strings.Split(body, `<td class="cap-col-lastrun">`)[1:] {
		cell := frag
		if i := strings.Index(cell, "</td>"); i >= 0 {
			cell = cell[:i]
		}
		if strings.TrimSpace(stripTags(cell)) == "" {
			t.Errorf("last-run column rendered blank: %q", frag)
		}
	}

	// Top-nav "Gardeners" link points at /gardeners.
	if !strings.Contains(body, `href="/gardeners"`) {
		t.Errorf("nav link to /gardeners missing")
	}
}

// TestHandleGardeners_SurfacesFieldOnlyGardener guards the ka-0000001950
// resilience path: a gardener created the "wrong way" — role=gardener set only as
// a FIELD, with NO role:gardener label — must still render on /gardeners. The
// label query (triggerScheduleResp) returns nothing here; the gardener is served
// only via the role=gardener field query (gardenerFieldOnlyResp).
func TestHandleGardeners_SurfacesFieldOnlyGardener(t *testing.T) {
	f := newFake()

	spec := map[string]any{
		"name":          "heretic",
		"terminal_goal": "ship the thing",
		"runs_spawned":  3,
	}
	raw, _ := json.Marshal(spec)
	// Note: NO role:gardener label — only the role field. This is exactly the
	// misconfiguration that previously made gardeners invisible.
	fieldOnly := &beadsapi.BeadDetail{
		ID:        "gd-heretic",
		Title:     "heretic",
		Type:      "trigger",
		Status:    "open",
		Labels:    []string{"trigger:schedule", "project:gardenio"},
		Fields:    map[string]string{"role": "gardener", "gardener": string(raw)},
		UpdatedAt: time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
	}
	// Label query yields nothing; field query yields the heretic gardener.
	f.triggerScheduleResp = &beadsapi.ListBeadsResult{}
	f.gardenerFieldOnlyResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{fieldOnly},
		Total: 1,
	}

	mux := mkServerWithNow(t, f, gardenersNow)
	req := httptest.NewRequest("GET", "/gardeners", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `href="/gardener/gd-heretic"`) {
		t.Errorf("field-only gardener gd-heretic not rendered on /gardeners:\n%s", body)
	}
	if !strings.Contains(body, "heretic") {
		t.Errorf("field-only gardener name missing from /gardeners")
	}
}

// TestLoadGardenerRows_DedupsLabelAndFieldMatches guards that a gardener matching
// BOTH the label query and the field query (the canonical, correctly-created
// shape) renders exactly once — the dedup-by-ID in loadGardenerRows.
func TestLoadGardenerRows_DedupsLabelAndFieldMatches(t *testing.T) {
	f := newFake()

	spec := map[string]any{"name": "canon", "terminal_goal": "do it right"}
	raw, _ := json.Marshal(spec)
	canon := &beadsapi.BeadDetail{
		ID:        "gd-canon",
		Title:     "canon",
		Type:      "trigger",
		Status:    "open",
		Labels:    []string{"trigger:schedule", "role:gardener", "project:gardenio"},
		Fields:    map[string]string{"role": "gardener", "gardener": string(raw)},
		UpdatedAt: time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
	}
	// Same bead returned by both the label query and the field query.
	f.triggerScheduleResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{canon}, Total: 1}
	f.gardenerFieldOnlyResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{canon}, Total: 1}

	srv := gardenerTestServer(t, f)
	rows, _, _, err := srv.loadGardenerRows(t.Context())
	if err != nil {
		t.Fatalf("loadGardenerRows: %v", err)
	}
	n := 0
	for _, r := range rows {
		if r.ID == "gd-canon" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("expected gd-canon to appear exactly once, got %d", n)
	}
}

// TestLoadGardenerRows_DerivesLegacyRunCounts guards the index-side fix for
// bug ka-0000005661: a legacy spawn_agent / bare-trigger gardener carries no
// GardenerSpec `gardener` blob, so its RunsSpawned/RunsCompleted decode to zero
// even though its life-thread has real runs. loadGardenerRows must derive the
// Runs column from the gardener's iteration beads (CountOnly) and flag the row
// RunsDerived. A gardener WITH a real engine tally must be left untouched.
func TestLoadGardenerRows_DerivesLegacyRunCounts(t *testing.T) {
	f := newFake()

	// Legacy gardener: action=spawn_agent, role:gardener label, NO `gardener` spec
	// blob → zero engine tally. Iterations carry trigger_id == its id.
	legacy := &beadsapi.BeadDetail{
		ID:        "gd-legacy-cap",
		Title:     "legacy firehose gardener",
		Type:      "trigger",
		Status:    "open",
		Labels:    []string{"trigger:schedule", "role:gardener", "project:gardenio"},
		Fields:    map[string]string{"role": "gardener", "action": "spawn_agent"},
		UpdatedAt: time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
	}
	// Modern gardener WITH a real engine tally — must NOT be re-derived.
	spec := map[string]any{"name": "modern", "terminal_goal": "ship", "runs_spawned": 5, "runs_completed": 4}
	raw, _ := json.Marshal(spec)
	modern := &beadsapi.BeadDetail{
		ID:        "gd-modern-cap",
		Title:     "modern",
		Type:      "trigger",
		Status:    "open",
		Labels:    []string{"trigger:schedule", "role:gardener", "project:gardenio"},
		Fields:    map[string]string{"role": "gardener", "gardener": string(raw)},
		UpdatedAt: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC),
	}
	f.triggerScheduleResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{legacy, modern}, Total: 2}

	// Seed the iteration-count fast path: 94 spawned, 93 closed under trigger_id.
	f.agentCountByField = map[string]int{
		"trigger_id=gd-legacy-cap":        94,
		"trigger_id=gd-legacy-cap|closed": 93,
	}

	srv := gardenerTestServer(t, f)
	rows, _, _, err := srv.loadGardenerRows(t.Context())
	if err != nil {
		t.Fatalf("loadGardenerRows: %v", err)
	}

	var gotLegacy, gotModern *gardenerRowView
	for i := range rows {
		switch rows[i].ID {
		case "gd-legacy-cap":
			gotLegacy = &rows[i]
		case "gd-modern-cap":
			gotModern = &rows[i]
		}
	}
	if gotLegacy == nil || gotModern == nil {
		t.Fatalf("missing rows: legacy=%v modern=%v", gotLegacy != nil, gotModern != nil)
	}

	if !gotLegacy.RunsDerived {
		t.Errorf("legacy gardener RunsDerived = false, want true")
	}
	if gotLegacy.RunsSpawned != 94 || gotLegacy.RunsCompleted != 93 {
		t.Errorf("legacy derived counts = %d/%d, want 94/93",
			gotLegacy.RunsCompleted, gotLegacy.RunsSpawned)
	}

	if gotModern.RunsDerived {
		t.Errorf("modern gardener RunsDerived = true, want false (real engine tally)")
	}
	if gotModern.RunsSpawned != 5 || gotModern.RunsCompleted != 4 {
		t.Errorf("modern engine counts = %d/%d, want 5/4 (must not be re-derived)",
			gotModern.RunsCompleted, gotModern.RunsSpawned)
	}
}

// runCountProbes tallies the CountOnly iteration-count queries a run-count
// derivation issued against a given ownership field key ("trigger_id" /
// "schedule_id"), by owner id. It scans the fake's recorded query log for the
// enrichGardenerRunCounts shape (Types=[agent], CountOnly, the owner field set to
// gardenerID) so a test can assert the fan-out was actually cut, not just that
// the derived count came out right.
func runCountProbes(f *fakeDaemon, key, gardenerID string) int {
	f.listMu.Lock()
	defer f.listMu.Unlock()
	n := 0
	for i := range f.listAllQ {
		q := f.listAllQ[i]
		if !q.CountOnly || !slices.Contains(q.Types, "agent") {
			continue
		}
		if q.Fields[key] == gardenerID {
			n++
		}
	}
	return n
}

// TestDeriveGardenerRunCount_ShortCircuitsOtherKey pins the D2 fan-out reduction
// (bug ka-0000008005): a legacy zero-tally gardener's iterations carry EXACTLY
// ONE ownership key, so once the gardener's own primitive key (trigger_id for a
// type=trigger gardener) yields runs the OTHER key (schedule_id) must not be
// probed at all. The pre-fix code fanned out both keys × total/closed/expired =
// 6 round-trips; the fix issues only the owning key's 3.
func TestDeriveGardenerRunCount_ShortCircuitsOtherKey(t *testing.T) {
	f := newFake()
	legacy := &beadsapi.BeadDetail{
		ID:        "gd-legacy-cap",
		Title:     "legacy trigger gardener",
		Type:      "trigger",
		Status:    "open",
		Labels:    []string{"trigger:schedule", "role:gardener", "project:gardenio"},
		Fields:    map[string]string{"role": "gardener", "action": "spawn_agent"},
		UpdatedAt: time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
	}
	f.triggerScheduleResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{legacy}, Total: 1}
	// Runs live under trigger_id only (the bridge spawn path).
	f.agentCountByField = map[string]int{
		"trigger_id=gd-legacy-cap":        40,
		"trigger_id=gd-legacy-cap|closed": 39,
	}

	srv := gardenerTestServer(t, f)
	rows, _, _, err := srv.loadGardenerRows(t.Context())
	if err != nil {
		t.Fatalf("loadGardenerRows: %v", err)
	}
	var got *gardenerRowView
	for i := range rows {
		if rows[i].ID == "gd-legacy-cap" {
			got = &rows[i]
		}
	}
	if got == nil {
		t.Fatal("gd-legacy-cap row not found")
	}
	if !got.RunsDerived || got.RunsSpawned != 40 || got.RunsCompleted != 39 {
		t.Errorf("derived counts = %d/%d derived=%v, want 39/40 derived=true",
			got.RunsCompleted, got.RunsSpawned, got.RunsDerived)
	}
	// The owning key runs total+closed+expired = 3 probes; the other key is
	// never touched (the trigger_id total was non-zero, so we short-circuit).
	if n := runCountProbes(f, fieldTriggerID, "gd-legacy-cap"); n != 3 {
		t.Errorf("trigger_id probes = %d, want 3 (total+closed+expired)", n)
	}
	if n := runCountProbes(f, fieldScheduleID, "gd-legacy-cap"); n != 0 {
		t.Errorf("schedule_id probes = %d, want 0 (owning key found — no fallback fan-out)", n)
	}
}

// TestDeriveGardenerRunCount_FallsBackToOtherKey guards that when the gardener's
// own primitive key is genuinely empty, the derivation still falls back to the
// other key so a mis-keyed legacy iteration set (runs stamped under the
// non-primary key) is not silently dropped. The empty primary key costs exactly
// ONE probe (its total), then the owning fallback key runs its 3.
func TestDeriveGardenerRunCount_FallsBackToOtherKey(t *testing.T) {
	f := newFake()
	legacy := &beadsapi.BeadDetail{
		ID:        "gd-mis-cap",
		Title:     "legacy gardener keyed on schedule_id",
		Type:      "trigger", // primitive is trigger → probes trigger_id first
		Status:    "open",
		Labels:    []string{"trigger:schedule", "role:gardener", "project:gardenio"},
		Fields:    map[string]string{"role": "gardener", "action": "spawn_agent"},
		UpdatedAt: time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
	}
	f.triggerScheduleResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{legacy}, Total: 1}
	// Runs live under schedule_id, NOT trigger_id (the non-primary key).
	f.agentCountByField = map[string]int{
		"schedule_id=gd-mis-cap":        12,
		"schedule_id=gd-mis-cap|closed": 11,
	}

	srv := gardenerTestServer(t, f)
	rows, _, _, err := srv.loadGardenerRows(t.Context())
	if err != nil {
		t.Fatalf("loadGardenerRows: %v", err)
	}
	var got *gardenerRowView
	for i := range rows {
		if rows[i].ID == "gd-mis-cap" {
			got = &rows[i]
		}
	}
	if got == nil {
		t.Fatal("gd-mis-cap row not found")
	}
	if !got.RunsDerived || got.RunsSpawned != 12 || got.RunsCompleted != 11 {
		t.Errorf("derived counts = %d/%d derived=%v, want 11/12 derived=true (fallback key)",
			got.RunsCompleted, got.RunsSpawned, got.RunsDerived)
	}
	// Primary key: only the empty total probe (no closed/expired on a 0 total).
	if n := runCountProbes(f, fieldTriggerID, "gd-mis-cap"); n != 1 {
		t.Errorf("trigger_id probes = %d, want 1 (empty total only, no closed/expired)", n)
	}
	// Fallback key: the full total+closed+expired = 3.
	if n := runCountProbes(f, fieldScheduleID, "gd-mis-cap"); n != 3 {
		t.Errorf("schedule_id probes = %d, want 3 (owning fallback key)", n)
	}
}

// TestGardenerTriggerSubKind covers the cron/bead_query/event projection that
// backs the /maps trigger-node color (ka-0000003781). It is a pure no-fetch
// classifier over a gardener bead's trigger fields.
func TestGardenerTriggerSubKind(t *testing.T) {
	mk := func(typ string, fields map[string]string, labels ...string) *beadsapi.BeadDetail {
		return &beadsapi.BeadDetail{ID: "gd-x", Type: typ, Fields: fields, Labels: labels}
	}
	beadEventTriggers := `[{"kind":"bead_event","events":["closed"]}]`
	cronTriggers := `[{"kind":"cron","cron":"0 9 * * *"}]`

	cases := []struct {
		name string
		bead *beadsapi.BeadDetail
		want string
	}{
		{"cron field", mk("trigger", map[string]string{"cron": "7,37 * * * *"}), "cron"},
		{"cron wins over conditions gate",
			mk("trigger", map[string]string{"cron": "0 9 * * *", "conditions": `[{"type":"bead_query"}]`}), "cron"},
		{"conditions gate, no cron",
			mk("trigger", map[string]string{"conditions": `[{"type":"bead_query"}]`}), "bead_query"},
		{"bead_event trigger entry, no cron",
			mk("trigger", map[string]string{"triggers": beadEventTriggers}), "bead_query"},
		{"cron trigger entry, no cron field",
			mk("trigger", map[string]string{"triggers": cronTriggers}), "cron"},
		{"bare trigger, no cron/predicate", mk("trigger", map[string]string{"action": "spawn_agent"}), "event"},
		{"legacy schedule bead", mk("schedule", map[string]string{"cron": "0 9 * * *"}), ""},
		// A mode-flipped gardener (gardenermode.Set → event): trigger:multi label,
		// cleared cron, a bead_event entry PLUS a fallback cron entry. It is
		// event-FIRED — the fallback cron is a watchdog, not the cadence
		// authority — so it classifies "event", NOT "bead_query" (which is where
		// a bare bead_event entry with no trigger:multi label lands above) and
		// NOT "cron" (the fallback cron entry must not win). Spec
		// 2026-07-13-gardener-mode-flip-design.md.
		{"flipped event mode", mk("trigger",
			map[string]string{"triggers": `[{"kind":"bead_event","events":["closed"]},{"kind":"cron","cron":"11 2,8,14,20 * * *"}]`},
			"trigger:multi"), "event"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := gardenerTriggerSubKind(tc.bead); got != tc.want {
				t.Errorf("gardenerTriggerSubKind = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLoadGardenerRows_NoIterationsStaysZero guards that a zero-tally gardener
// with NO iteration beads stays 0/0 and unflagged — the derivation only fires
// when the iteration count is genuinely positive, so a brand-new or never-run
// legacy gardener does not get a misleading "~0/0" treatment.
func TestLoadGardenerRows_NoIterationsStaysZero(t *testing.T) {
	f := newFake()
	fresh := &beadsapi.BeadDetail{
		ID:        "gd-fresh-cap",
		Title:     "fresh legacy gardener",
		Type:      "trigger",
		Status:    "open",
		Labels:    []string{"trigger:schedule", "role:gardener", "project:gardenio"},
		Fields:    map[string]string{"role": "gardener", "action": "spawn_agent"},
		UpdatedAt: time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
	}
	f.triggerScheduleResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{fresh}, Total: 1}
	// agentCountByField left nil → every count query returns Total 0.

	srv := gardenerTestServer(t, f)
	rows, _, _, err := srv.loadGardenerRows(t.Context())
	if err != nil {
		t.Fatalf("loadGardenerRows: %v", err)
	}
	for i := range rows {
		if rows[i].ID == "gd-fresh-cap" {
			if rows[i].RunsDerived || rows[i].RunsSpawned != 0 || rows[i].RunsCompleted != 0 {
				t.Errorf("fresh gardener = %d/%d derived=%v, want 0/0 underived",
					rows[i].RunsCompleted, rows[i].RunsSpawned, rows[i].RunsDerived)
			}
			return
		}
	}
	t.Fatal("gd-fresh-cap row not found")
}

// TestLoadGardenerRows_ExcludesExpiredShells pins the index-side half of bug
// ka-0000006805: when a legacy zero-tally gardener's iteration beads include
// never-ran spawn shells (agent_state="expired"), the derived Runs column must
// subtract them so it matches the detail page's run-only count. Here 94 agent
// beads exist but 4 are expired shells → 90 real runs (89 closed-real, since one
// of the expired is also counted in the 93 closed).
func TestLoadGardenerRows_ExcludesExpiredShells(t *testing.T) {
	f := newFake()
	legacy := &beadsapi.BeadDetail{
		ID:        "gd-legacy-cap",
		Title:     "legacy gardener with expired spawns",
		Type:      "trigger",
		Status:    "open",
		Labels:    []string{"trigger:schedule", "role:gardener", "project:gardenio"},
		Fields:    map[string]string{"role": "gardener", "action": "spawn_agent"},
		UpdatedAt: time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
	}
	f.triggerScheduleResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{legacy}, Total: 1}

	// 94 agent beads total, 93 closed, 4 of which are never-ran expired shells.
	// Real runs = 94-4 = 90 spawned; closed-real = 93-4 = 89 completed.
	f.agentCountByField = map[string]int{
		"trigger_id=gd-legacy-cap":         94,
		"trigger_id=gd-legacy-cap|closed":  93,
		"trigger_id=gd-legacy-cap|expired": 4,
	}

	srv := gardenerTestServer(t, f)
	rows, _, _, err := srv.loadGardenerRows(t.Context())
	if err != nil {
		t.Fatalf("loadGardenerRows: %v", err)
	}
	var got *gardenerRowView
	for i := range rows {
		if rows[i].ID == "gd-legacy-cap" {
			got = &rows[i]
		}
	}
	if got == nil {
		t.Fatal("gd-legacy-cap row not found")
	}
	if !got.RunsDerived {
		t.Errorf("RunsDerived = false, want true")
	}
	if got.RunsSpawned != 90 || got.RunsCompleted != 89 {
		t.Errorf("derived counts = %d/%d (completed/spawned), want 89/90 (94/93 minus 4 expired shells)",
			got.RunsCompleted, got.RunsSpawned)
	}
}

func TestHandleGardeners_FilterActive(t *testing.T) {
	f := seedGardenersIndexFake(t)
	mux := mkServerWithNow(t, f, gardenersNow)

	req := httptest.NewRequest("GET", "/gardeners?filter=active", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// Active filter keeps the enabled gardener, drops the disabled one.
	if !strings.Contains(body, "gd-cap1") {
		t.Errorf("active filter dropped the active gardener gd-cap1")
	}
	if strings.Contains(body, `href="/gardener/gd-cap2"`) {
		t.Errorf("active filter should hide disabled gardener gd-cap2")
	}
}

// TestHandleGardeners_LiveDefaultHidesRetired guards the formal-retirement
// lifecycle (ka-0000004655): the DEFAULT /gardeners view is the Live fleet —
// every gardener that has NOT been retired (closed) — and a retired gardener
// surfaces only under the Archived view. The goal fixture seeds two CLOSED
// gardeners (gd-won, gd-spent) and one OPEN one (gd-live).
func TestHandleGardeners_LiveDefaultHidesRetired(t *testing.T) {
	f := seedGardenersGoalFake(t)
	mux := mkServerWithNow(t, f, gardenersNow)

	// Default view (no ?filter=) = Live: the open gardener shows, the two
	// retired (closed) ones are hidden.
	req := httptest.NewRequest("GET", "/gardeners", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `href="/gardener/gd-live"`) {
		t.Errorf("Live default dropped the open gardener gd-live")
	}
	if strings.Contains(body, `href="/gardener/gd-won"`) || strings.Contains(body, `href="/gardener/gd-spent"`) {
		t.Errorf("Live default must hide retired (closed) gardeners gd-won/gd-spent")
	}
	// The Archived chip exists and is the place to find them.
	if !strings.Contains(body, `href="/gardeners?filter=archived"`) {
		t.Errorf("missing Archived chip on the Live view")
	}

	// Archived view shows the retired gardeners and hides the live one.
	req = httptest.NewRequest("GET", "/gardeners?filter=archived", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("archived: expected 200, got %d", w.Code)
	}
	body = w.Body.String()
	if !strings.Contains(body, `href="/gardener/gd-won"`) || !strings.Contains(body, `href="/gardener/gd-spent"`) {
		t.Errorf("Archived view must show retired gardeners gd-won/gd-spent")
	}
	if strings.Contains(body, `href="/gardener/gd-live"`) {
		t.Errorf("Archived view must hide the live gardener gd-live")
	}

	// The legacy ?filter=closed bookmark still resolves to the Archived set.
	req = httptest.NewRequest("GET", "/gardeners?filter=closed", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), `href="/gardener/gd-won"`) {
		t.Errorf("legacy ?filter=closed alias should still show retired gardeners")
	}
}

func TestGardenerLastRunOutcome(t *testing.T) {
	cases := []struct {
		name                string
		runsCompleted       int
		consecutiveFailures int
		want                string
	}{
		{"none yet", 0, 0, ""},
		{"succeeded", 4, 0, "ok"},
		{"failed", 4, 2, "failed"},
		{"failed even with no completed counter", 0, 1, "failed"},
	}
	for _, tc := range cases {
		if got := gardenerLastRunOutcome(tc.runsCompleted, tc.consecutiveFailures); got != tc.want {
			t.Errorf("%s: gardenerLastRunOutcome(%d,%d) = %q, want %q",
				tc.name, tc.runsCompleted, tc.consecutiveFailures, got, tc.want)
		}
	}
}

// TestApplyLastRun_InlineProjection covers the per-row last-run projection: a
// running last-agent bead marks the row running with an inline TLDR but no
// completion time; a closed one yields the real wrap-up outcome + inline TLDR +
// completion time; an empty wrap-up backstops the headline from task title.
func TestApplyLastRun_InlineProjection(t *testing.T) {
	s := NewServer(newFake(), slog.Default(), "", "", "", nil)
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	completed := now.Add(-15 * time.Minute)

	t.Run("running with tldr", func(t *testing.T) {
		row := gardenerRowView{ID: "gd-c1", LastRunOutcome: "ok"}
		b := &beadsapi.BeadDetail{
			ID: "gd-a1", Status: "in_progress",
			Fields:    map[string]string{"wrapup": `{"tldr":"draining the queue"}`},
			UpdatedAt: now.Add(-2 * time.Minute),
		}
		s.applyLastRun(&row, b, now)
		if !row.IsRunning {
			t.Errorf("expected IsRunning for an open last-agent bead")
		}
		if !row.LastCompletedAt.IsZero() {
			t.Errorf("running row should have zero LastCompletedAt, got %v", row.LastCompletedAt)
		}
		if row.LastRunTLDR != "draining the queue" {
			t.Errorf("LastRunTLDR = %q, want %q", row.LastRunTLDR, "draining the queue")
		}
		if !row.lastActivityAt.Equal(b.UpdatedAt) {
			t.Errorf("lastActivityAt = %v, want %v", row.lastActivityAt, b.UpdatedAt)
		}
	})

	t.Run("running with rolled-up spend flags SpendExcludesLiveRun (C3)", func(t *testing.T) {
		// A live run's cost is not yet in the roll-up, so a running gardener that
		// HAS a rolled-up figure must be flagged as under-reporting.
		row := gardenerRowView{ID: "gd-c1b", HasSpend: true}
		b := &beadsapi.BeadDetail{
			ID: "gd-a1b", Status: "in_progress",
			Fields:    map[string]string{"wrapup": `{"tldr":"still running"}`},
			UpdatedAt: now,
		}
		s.applyLastRun(&row, b, now)
		if !row.SpendExcludesLiveRun {
			t.Errorf("a running gardener with a roll-up should flag SpendExcludesLiveRun")
		}
	})

	t.Run("running WITHOUT a roll-up does not flag SpendExcludesLiveRun (C3)", func(t *testing.T) {
		// No figure to qualify → no approximate marker (avoids a "~" on a blank).
		row := gardenerRowView{ID: "gd-c1c", HasSpend: false}
		b := &beadsapi.BeadDetail{
			ID: "gd-a1c", Status: "in_progress",
			Fields:    map[string]string{"wrapup": `{"tldr":"still running"}`},
			UpdatedAt: now,
		}
		s.applyLastRun(&row, b, now)
		if row.SpendExcludesLiveRun {
			t.Errorf("no roll-up figure → nothing to mark approximate")
		}
	})

	t.Run("closed run never flags SpendExcludesLiveRun (C3)", func(t *testing.T) {
		row := gardenerRowView{ID: "gd-c1d", HasSpend: true}
		b := &beadsapi.BeadDetail{
			ID: "gd-a1d", Status: "closed",
			Fields:    map[string]string{"wrapup": `{"tldr":"done"}`},
			UpdatedAt: completed,
		}
		s.applyLastRun(&row, b, now)
		if row.SpendExcludesLiveRun {
			t.Errorf("a closed run's cost is already reconciled — no exclusion marker")
		}
	})

	t.Run("closed ok with tldr + completion", func(t *testing.T) {
		row := gardenerRowView{ID: "gd-c2"}
		b := &beadsapi.BeadDetail{
			ID: "gd-a2", Status: "closed",
			Fields:    map[string]string{"wrapup": `{"tldr":"merged MR 42"}`},
			UpdatedAt: completed,
		}
		s.applyLastRun(&row, b, now)
		if row.IsRunning {
			t.Errorf("closed last-agent bead should not be running")
		}
		if row.LastRunOutcome != "ok" {
			t.Errorf("LastRunOutcome = %q, want ok", row.LastRunOutcome)
		}
		if row.LastRunTLDR != "merged MR 42" {
			t.Errorf("LastRunTLDR = %q, want %q", row.LastRunTLDR, "merged MR 42")
		}
		if !row.LastCompletedAt.Equal(completed) {
			t.Errorf("LastCompletedAt = %v, want %v", row.LastCompletedAt, completed)
		}
		if row.LastCompletedAgo == "" {
			t.Errorf("expected a rendered LastCompletedAgo")
		}
	})

	t.Run("closed failed maps to failed", func(t *testing.T) {
		row := gardenerRowView{ID: "gd-c3"}
		b := &beadsapi.BeadDetail{
			ID: "gd-a3", Status: "closed",
			Fields:    map[string]string{"wrapup": `{"tldr":"could not deploy","blockers":"perms missing"}`},
			UpdatedAt: completed,
		}
		s.applyLastRun(&row, b, now)
		if row.LastRunOutcome != "failed" {
			t.Errorf("LastRunOutcome = %q, want failed (blockers present)", row.LastRunOutcome)
		}
	})

	t.Run("empty wrapup backstops from task title", func(t *testing.T) {
		row := gardenerRowView{ID: "gd-c4"}
		b := &beadsapi.BeadDetail{
			ID: "gd-a4", Status: "closed",
			Fields:    map[string]string{"task_title": "Sweep orphaned beads"},
			UpdatedAt: completed,
		}
		s.applyLastRun(&row, b, now)
		// agentRunOutcome("") == "unknown" -> mapped to ok; derivedRunOutcome with
		// no signals returns "" so the headline falls through to the task title.
		if row.LastRunOutcome != "ok" {
			t.Errorf("LastRunOutcome = %q, want ok", row.LastRunOutcome)
		}
		if row.LastRunTLDR != "Sweep orphaned beads" {
			t.Errorf("LastRunTLDR = %q, want task-title backstop", row.LastRunTLDR)
		}
	})

	// Live-agent overlay (maps2 v2, ka-0000000595): a running gardener's last-agent
	// bead IS the currently-live agent. applyLastRun projects its name + href off
	// that same bead, and gates LiveAgentAlive on the /agents liveness predicate.
	t.Run("live-agent overlay: alive working agent", func(t *testing.T) {
		row := gardenerRowView{ID: "gd-c5"}
		b := &beadsapi.BeadDetail{
			ID: "gd-agentlive", Status: "in_progress",
			Fields: map[string]string{
				"agent":       "gardenio-delivery-drive-abc1",
				"agent_state": "working",
				"wrapup":      `{"tldr":"draining"}`,
			},
			UpdatedAt: now.Add(-2 * time.Minute),
		}
		s.applyLastRun(&row, b, now)
		if row.LiveAgentName != "gardenio-delivery-drive-abc1" {
			t.Errorf("LiveAgentName = %q, want the agent field", row.LiveAgentName)
		}
		if row.LiveAgentHref != "/gd-agentlive" {
			t.Errorf("LiveAgentHref = %q, want /gd-agentlive", row.LiveAgentHref)
		}
		if !row.LiveAgentAlive {
			t.Errorf("expected LiveAgentAlive for a working, fresh agent bead")
		}
	})

	// A run that hasn't closed but whose agent shell has gone stale (alive state
	// but no update within agentStaleThreshold) must NOT read as alive — the card
	// names the agent without the pulsing dot.
	t.Run("live-agent overlay: stale shell is not alive", func(t *testing.T) {
		row := gardenerRowView{ID: "gd-c6"}
		b := &beadsapi.BeadDetail{
			ID: "gd-ehgdh8mbbl", Status: "in_progress",
			Fields:    map[string]string{"agent": "gardenio-wedged-xyz9", "agent_state": "working"},
			UpdatedAt: now.Add(-2 * agentStaleThreshold),
		}
		s.applyLastRun(&row, b, now)
		if row.LiveAgentName != "gardenio-wedged-xyz9" {
			t.Errorf("LiveAgentName = %q, want the agent field", row.LiveAgentName)
		}
		if row.LiveAgentAlive {
			t.Errorf("stale shell should not read as alive")
		}
	})

	// A closed last-agent bead with a terminated state: the name still resolves
	// (so the last run can be attributed), but liveness is off.
	t.Run("live-agent overlay: closed terminated agent not alive", func(t *testing.T) {
		row := gardenerRowView{ID: "gd-c7"}
		b := &beadsapi.BeadDetail{
			ID: "gd-agentdone", Status: "closed",
			Fields:    map[string]string{"agent": "gardenio-done-aaa", "agent_state": "terminated"},
			UpdatedAt: completed,
		}
		s.applyLastRun(&row, b, now)
		if row.LiveAgentName != "gardenio-done-aaa" {
			t.Errorf("LiveAgentName = %q, want the agent field", row.LiveAgentName)
		}
		if row.LiveAgentAlive {
			t.Errorf("terminated agent should not read as alive")
		}
	})

	// Falls back to the bead title when the agent name field is absent.
	t.Run("live-agent overlay: title fallback", func(t *testing.T) {
		row := gardenerRowView{ID: "gd-c8"}
		b := &beadsapi.BeadDetail{
			ID: "gd-agenttitled", Status: "in_progress", Title: "gardenio-titled-bbb",
			Fields:    map[string]string{"agent_state": "spawning"},
			UpdatedAt: now,
		}
		s.applyLastRun(&row, b, now)
		if row.LiveAgentName != "gardenio-titled-bbb" {
			t.Errorf("LiveAgentName = %q, want the bead-title fallback", row.LiveAgentName)
		}
		if !row.LiveAgentAlive {
			t.Errorf("expected LiveAgentAlive for a spawning, fresh agent bead")
		}
	})

	// Live-context occupancy (maps2 v2 cost/context chips, ka-0000001668): a
	// genuinely-live agent reporting context_tokens projects an occupancy chip
	// off the same last-agent bead.
	t.Run("context chip: live agent reports occupancy", func(t *testing.T) {
		row := gardenerRowView{ID: "gd-c9"}
		b := &beadsapi.BeadDetail{
			ID: "gd-agentctx", Status: "in_progress",
			Fields: map[string]string{
				"agent":          "gardenio-busy-ddd",
				"agent_state":    "working",
				"context_tokens": "128000",
			},
			UpdatedAt: now.Add(-1 * time.Minute),
		}
		s.applyLastRun(&row, b, now)
		if !row.HasContext {
			t.Fatalf("expected HasContext for a live agent reporting context_tokens")
		}
		if row.ContextLabel != "128.0k" {
			t.Errorf("ContextLabel = %q, want 128.0k", row.ContextLabel)
		}
		if row.ContextPct != 76 {
			t.Errorf("ContextPct = %d, want 76 (128k of the ~167k auto-compact threshold on a 200k window)", row.ContextPct)
		}
	})

	// A stale shell carries no current context — the chip is suppressed even when
	// the bead still has a context_tokens field, mirroring buildAgentUsageView's
	// live-gating.
	t.Run("context chip: stale shell suppresses occupancy", func(t *testing.T) {
		row := gardenerRowView{ID: "gd-c10"}
		b := &beadsapi.BeadDetail{
			ID: "gd-agentctxstale", Status: "in_progress",
			Fields: map[string]string{
				"agent":          "gardenio-wedged-eee",
				"agent_state":    "working",
				"context_tokens": "150000",
			},
			UpdatedAt: now.Add(-2 * agentStaleThreshold),
		}
		s.applyLastRun(&row, b, now)
		if row.LiveAgentAlive {
			t.Fatalf("precondition: stale shell should not be alive")
		}
		if row.HasContext {
			t.Errorf("stale shell should carry no context chip, got %+v", row)
		}
	})

	// Turns (🔁 the triad's third metric, ka-0000001199): a genuinely-live agent
	// projects the advancing live_turns counter off the same last-agent bead.
	t.Run("turns chip: live agent prefers live_turns", func(t *testing.T) {
		row := gardenerRowView{ID: "gd-c11"}
		b := &beadsapi.BeadDetail{
			ID: "gd-hzkgbtgzsx", Status: "in_progress",
			Fields: map[string]string{
				"agent":               "gardenio-busy-fff",
				"agent_state":         "working",
				"live_turns":          "47",
				"total_request_count": "12",
			},
			UpdatedAt: now.Add(-1 * time.Minute),
		}
		s.applyLastRun(&row, b, now)
		if !row.HasTurns {
			t.Fatalf("expected HasTurns for a live agent reporting live_turns")
		}
		if row.TurnsLabel != "47" {
			t.Errorf("TurnsLabel = %q, want 47 (live_turns preferred over total_request_count)", row.TurnsLabel)
		}
	})

	// Unlike context, turns SURVIVE close: a stale/closed shell has no "current
	// context" (chip suppressed above) but keeps its authoritative
	// total_request_count, so the last run's round-trip count still shows. This
	// is buildAgentUsageView's live→total fallback, projected UNCONDITIONALLY in
	// applyLastRun (not gated on LiveAgentAlive).
	t.Run("turns chip: stale shell keeps total_request_count", func(t *testing.T) {
		row := gardenerRowView{ID: "gd-c12"}
		b := &beadsapi.BeadDetail{
			ID: "gd-agentturnsstale", Status: "in_progress",
			Fields: map[string]string{
				"agent":               "gardenio-wedged-ggg",
				"agent_state":         "working",
				"context_tokens":      "150000",
				"live_turns":          "47",
				"total_request_count": "88",
			},
			UpdatedAt: now.Add(-2 * agentStaleThreshold),
		}
		s.applyLastRun(&row, b, now)
		if row.LiveAgentAlive {
			t.Fatalf("precondition: stale shell should not be alive")
		}
		if row.HasContext {
			t.Errorf("stale shell should carry no context chip, got %+v", row)
		}
		if !row.HasTurns {
			t.Fatalf("expected HasTurns to survive close via total_request_count")
		}
		if row.TurnsLabel != "88" {
			t.Errorf("TurnsLabel = %q, want 88 (total_request_count, not stale live_turns)", row.TurnsLabel)
		}
	})
}

// TestHandleGardeners_InlineLastRunAndRunningFirst exercises the end-to-end batch
// enrichment + run-centric sort + template render: a gardener whose last run is
// live floats to the top with a "running" badge and inline headline, ahead of a
// gardener whose last run completed.
func TestHandleGardeners_InlineLastRunAndRunningFirst(t *testing.T) {
	f := newFake()
	now := time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC)

	mk := func(id, name, lastAgentID string) *beadsapi.BeadDetail {
		spec := map[string]any{"name": name, "runs_spawned": 3, "runs_completed": 2, "last_agent_id": lastAgentID}
		raw, _ := json.Marshal(spec)
		return &beadsapi.BeadDetail{
			ID: id, Title: name, Type: "trigger", Status: "open",
			Labels:    []string{"trigger:schedule", "role:gardener", "project:gardenio"},
			Fields:    map[string]string{"role": "gardener", "gardener": string(raw)},
			UpdatedAt: now,
		}
	}
	f.triggerScheduleResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			mk("gd-done", "completed-cap", "gd-agentDone"),
			mk("gd-live", "running-cap", "gd-agentLive"),
		},
		Total: 2,
	}
	// Last-agent beads served via BatchGetBeads (f.beads).
	f.beads["gd-agentDone"] = &beadsapi.BeadDetail{
		ID: "gd-agentDone", Status: "closed",
		Fields:    map[string]string{"wrapup": `{"tldr":"closed 5 stale beads"}`},
		UpdatedAt: now.Add(-20 * time.Minute),
	}
	f.beads["gd-agentLive"] = &beadsapi.BeadDetail{
		ID: "gd-agentLive", Status: "in_progress",
		Fields:    map[string]string{"wrapup": `{"tldr":"investigating the failure"}`},
		UpdatedAt: now.Add(-1 * time.Minute),
	}

	mux := mkServerWithNow(t, f, func() time.Time { return now })
	req := httptest.NewRequest("GET", "/gardeners", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// One batch call covered both last-agent beads (bounded, not N+1).
	if f.batchGetBeadsCalls != 1 {
		t.Errorf("expected exactly 1 BatchGetBeads call, got %d", f.batchGetBeadsCalls)
	}

	// Inline TLDRs render without expanding the drawer.
	for _, want := range []string{
		"investigating the failure", // running cap inline headline
		"closed 5 stale beads",      // completed cap inline headline
		"cap-running-badge",         // animated running badge present
		"cap-running",               // running row left-border accent class
		"completed 20m ago",         // completed cap relative completion
		"gardeners-sort",            // visible sort-order indicator (ka-0000002827)
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}

	// Running-first sort: the live gardener's row must appear before the completed
	// one in the rendered HTML.
	liveIdx := strings.Index(body, `id="cap-row-gd-live"`)
	doneIdx := strings.Index(body, `id="cap-row-gd-done"`)
	if liveIdx < 0 || doneIdx < 0 {
		t.Fatalf("both rows must render (live=%d done=%d)", liveIdx, doneIdx)
	}
	if liveIdx > doneIdx {
		t.Errorf("running gardener should sort before completed one (live=%d done=%d)", liveIdx, doneIdx)
	}

	// Fleet-health summary strip leads the page with signal (ka-0000007180):
	// the strip renders and reports 1 running of 2 total. The summary appears
	// before the table so the operator reads fleet state first.
	if !strings.Contains(body, "gardeners-summary") {
		t.Errorf("body missing the fleet-health summary strip")
	}
	summaryIdx := strings.Index(body, "gardeners-summary")
	tableIdx := strings.Index(body, "cap-table-wrap")
	if summaryIdx < 0 || tableIdx < 0 || summaryIdx > tableIdx {
		t.Errorf("summary strip should render before the table (summary=%d table=%d)", summaryIdx, tableIdx)
	}
	if !strings.Contains(body, "running now") {
		t.Errorf("summary strip missing the 'running now' tile")
	}
}

// TestDeriveAttentionSignal exercises the inline-attention projection in
// isolation (ka-0000003950): the signal renders ONLY on needs-attention rows,
// an open decision wins over a blocker, the blocker is the fallback, and a
// non-attention row is left untouched.
func TestDeriveAttentionSignal(t *testing.T) {
	t.Run("non-attention row is untouched", func(t *testing.T) {
		row := gardenerRowView{NeedsAttention: false, attentionDecisionID: "gd-dec", lastRunBlocker: "wedged on X"}
		deriveAttentionSignal(&row)
		if row.AttentionKind != "" || row.AttentionSignal != "" || row.AttentionDecisionID != "" {
			t.Errorf("non-attention row must carry no inline signal, got kind=%q signal=%q dec=%q", row.AttentionKind, row.AttentionSignal, row.AttentionDecisionID)
		}
	})

	t.Run("decision wins over blocker", func(t *testing.T) {
		row := gardenerRowView{NeedsAttention: true, attentionDecisionID: "gd-dec", lastRunBlocker: "wedged on X"}
		deriveAttentionSignal(&row)
		if row.AttentionKind != "decision" {
			t.Errorf("AttentionKind = %q, want decision", row.AttentionKind)
		}
		if row.AttentionDecisionID != "gd-dec" {
			t.Errorf("AttentionDecisionID = %q, want gd-dec", row.AttentionDecisionID)
		}
		if row.AttentionSignal == "" {
			t.Errorf("decision row must carry a non-empty placeholder signal")
		}
	})

	t.Run("blocker is the fallback", func(t *testing.T) {
		row := gardenerRowView{NeedsAttention: true, lastRunBlocker: "wedged on X"}
		deriveAttentionSignal(&row)
		if row.AttentionKind != "blocker" {
			t.Errorf("AttentionKind = %q, want blocker", row.AttentionKind)
		}
		if row.AttentionSignal != "wedged on X" {
			t.Errorf("AttentionSignal = %q, want 'wedged on X'", row.AttentionSignal)
		}
		if row.AttentionDecisionID != "" {
			t.Errorf("blocker row must not set AttentionDecisionID, got %q", row.AttentionDecisionID)
		}
	})

	t.Run("attention row with no signal stays empty", func(t *testing.T) {
		row := gardenerRowView{NeedsAttention: true}
		deriveAttentionSignal(&row)
		if row.AttentionKind != "" || row.AttentionSignal != "" {
			t.Errorf("no source => empty chip, got kind=%q signal=%q", row.AttentionKind, row.AttentionSignal)
		}
	})
}

// TestEnrichAttentionDecisionPrompts_ClosedDecisionDowngrade covers the
// regression in ka-0000007231: a row whose decision-attention id was overridden
// by applyLastRun from a stale agent fields.decision_bead_id (no liveness check)
// must NOT keep rendering "blocked on an open decision" once that decision has
// resolved/dismissed. enrichAttentionDecisionPrompts re-checks the decision's
// live status and downgrades closed ones to the blocker fallback.
func TestEnrichAttentionDecisionPrompts_ClosedDecisionDowngrade(t *testing.T) {
	t.Run("open decision keeps the chip and gets the real prompt", func(t *testing.T) {
		f := newFake()
		f.beads["gd-open-dec"] = &beadsapi.BeadDetail{
			ID: "gd-open-dec", Type: "decision", Status: "open",
			Fields: map[string]string{"prompt": "Pick rollback strategy"},
		}
		s := NewServer(f, slog.Default(), "", "", "", nil)
		rows := []gardenerRowView{{
			ID: "gd-c1", AttentionKind: "decision", AttentionDecisionID: "gd-open-dec",
			AttentionSignal: "blocked on an open decision", lastRunBlocker: "wedged on X",
		}}
		s.enrichAttentionDecisionPrompts(context.Background(), rows)
		if rows[0].AttentionKind != "decision" {
			t.Errorf("AttentionKind = %q, want decision (still open)", rows[0].AttentionKind)
		}
		if rows[0].AttentionDecisionID != "gd-open-dec" {
			t.Errorf("AttentionDecisionID = %q, want gd-open-dec", rows[0].AttentionDecisionID)
		}
		if rows[0].AttentionSignal != "Pick rollback strategy" {
			t.Errorf("AttentionSignal = %q, want the live prompt", rows[0].AttentionSignal)
		}
	})

	t.Run("closed decision with a blocker downgrades to blocker", func(t *testing.T) {
		f := newFake()
		f.beads["gd-closed-dec"] = &beadsapi.BeadDetail{
			ID: "gd-closed-dec", Type: "decision", Status: "closed",
			Fields: map[string]string{"prompt": "Stop the gardener?", "chosen": "dismissed"},
		}
		s := NewServer(f, slog.Default(), "", "", "", nil)
		rows := []gardenerRowView{{
			ID: "gd-c2", AttentionKind: "decision", AttentionDecisionID: "gd-closed-dec",
			AttentionSignal: "blocked on an open decision", lastRunBlocker: "deploy creds expired",
		}}
		s.enrichAttentionDecisionPrompts(context.Background(), rows)
		if rows[0].AttentionKind != "blocker" {
			t.Errorf("AttentionKind = %q, want blocker (decision closed)", rows[0].AttentionKind)
		}
		if rows[0].AttentionDecisionID != "" {
			t.Errorf("AttentionDecisionID = %q, want cleared", rows[0].AttentionDecisionID)
		}
		if rows[0].AttentionSignal != "deploy creds expired" {
			t.Errorf("AttentionSignal = %q, want the blocker fallback", rows[0].AttentionSignal)
		}
	})

	t.Run("closed decision with no blocker clears the chip entirely", func(t *testing.T) {
		f := newFake()
		f.beads["gd-closed-dec2"] = &beadsapi.BeadDetail{
			ID: "gd-closed-dec2", Type: "decision", Status: "closed",
			Fields: map[string]string{"prompt": "Stop the gardener?", "chosen": "stop"},
		}
		s := NewServer(f, slog.Default(), "", "", "", nil)
		rows := []gardenerRowView{{
			ID: "gd-c3", AttentionKind: "decision", AttentionDecisionID: "gd-closed-dec2",
			AttentionSignal: "blocked on an open decision",
		}}
		s.enrichAttentionDecisionPrompts(context.Background(), rows)
		if rows[0].AttentionKind != "" || rows[0].AttentionSignal != "" || rows[0].AttentionDecisionID != "" {
			t.Errorf("closed decision + no blocker must clear the chip, got kind=%q signal=%q dec=%q",
				rows[0].AttentionKind, rows[0].AttentionSignal, rows[0].AttentionDecisionID)
		}
	})
}

// TestHandleGardeners_InlineAttentionSignal exercises the end-to-end inline
// triage signal (ka-0000003950): a needs-attention gardener (stuck on consecutive
// failures) whose last run is parked on an open decision surfaces that
// decision's prompt inline on the index row and links to the decision bead; a
// second needs-attention gardener whose last run wrapped up with a blocker
// surfaces that blocker inline; and a healthy gardener carries no inline signal.
func TestHandleGardeners_InlineAttentionSignal(t *testing.T) {
	f := newFake()
	now := time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC)

	mk := func(id, name, lastAgentID string, consecutiveFailures int) *beadsapi.BeadDetail {
		spec := map[string]any{
			"name": name, "runs_spawned": 5, "runs_completed": 5,
			"consecutive_failures": consecutiveFailures, "last_agent_id": lastAgentID,
		}
		raw, _ := json.Marshal(spec)
		return &beadsapi.BeadDetail{
			ID: id, Title: name, Type: "trigger", Status: "open",
			Labels:    []string{"trigger:schedule", "role:gardener", "project:gardenio"},
			Fields:    map[string]string{"role": "gardener", "gardener": string(raw)},
			UpdatedAt: now,
		}
	}

	// cap-dec: stuck (3 consecutive failures) + last run parked on an open
	// decision. cap-blk: stuck + last run wrapped up with a blocker. cap-ok:
	// healthy, no inline signal.
	decCap := mk("gd-cap-dec", "decision-gardener", "gd-agentDec", 3)
	decCap.BlockedOnDecision = true
	decCap.BlockedOnDecisionBeadID = "gd-decision-1"
	f.triggerScheduleResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			decCap,
			mk("gd-cap-blk", "blocker-gardener", "gd-agentBlk", 3),
			mk("gd-cap-ok", "healthy-gardener", "gd-agentOk", 0),
		},
		Total: 3,
	}
	// Last-agent + decision beads served via BatchGetBeads.
	f.beads["gd-agentDec"] = &beadsapi.BeadDetail{
		ID: "gd-agentDec", Status: "closed",
		Fields:    map[string]string{"decision_bead_id": "gd-decision-1", "wrapup": `{"tldr":"yielded on a fork"}`},
		UpdatedAt: now.Add(-5 * time.Minute),
	}
	f.beads["gd-agentBlk"] = &beadsapi.BeadDetail{
		ID: "gd-agentBlk", Status: "closed",
		Fields:    map[string]string{"wrapup": `{"tldr":"could not proceed","blockers":"deploy creds expired — operator must rotate"}`},
		UpdatedAt: now.Add(-6 * time.Minute),
	}
	f.beads["gd-agentOk"] = &beadsapi.BeadDetail{
		ID: "gd-agentOk", Status: "closed",
		Fields:    map[string]string{"wrapup": `{"tldr":"all green"}`},
		UpdatedAt: now.Add(-7 * time.Minute),
	}
	f.beads["gd-decision-1"] = &beadsapi.BeadDetail{
		ID: "gd-decision-1", Type: "decision", Status: "open",
		Fields: map[string]string{"prompt": "Pick rollback strategy: revert vs forward-fix"},
	}

	mux := mkServerWithNow(t, f, func() time.Time { return now })
	req := httptest.NewRequest("GET", "/gardeners", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// Decision-sourced inline signal: the real prompt renders and the chip links
	// to the open decision bead.
	for _, want := range []string{
		"cap-attention-signal",
		"cap-attention-tag-decision",
		"Pick rollback strategy: revert vs forward-fix",
		`href="/b/gd-decision-1"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing decision inline-attention fragment %q", want)
		}
	}

	// Blocker-sourced inline signal: the wrap-up blocker renders inline.
	for _, want := range []string{
		"cap-attention-tag-blocker",
		"deploy creds expired — operator must rotate",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing blocker inline-attention fragment %q", want)
		}
	}

	// The healthy gardener must NOT carry an inline signal: gating to the
	// attention slice keeps the dormant tail uncluttered. Its row should contain
	// no cap-attention-signal block. Isolate the healthy row's tbody.
	okStart := strings.Index(body, `id="cap-row-gd-cap-ok"`)
	if okStart < 0 {
		t.Fatalf("healthy gardener row must render")
	}
	okRow := body[okStart:]
	if end := strings.Index(okRow, "</tbody>"); end >= 0 {
		okRow = okRow[:end]
	}
	if strings.Contains(okRow, "cap-attention-signal") {
		t.Errorf("healthy gardener row must not carry an inline attention signal")
	}
}

func TestGardenerKindIsGoalAchieved(t *testing.T) {
	cases := []struct {
		slot, kind string
		want       bool
	}{
		// Achievement: loop_end (or unset slot, legacy) + a goal-completion kind.
		{"loop_end", "exit_condition", true},
		{"loop_end", "bead_query", true},
		{"loop_end", "specific_bead_closed", true},
		{"", "exit_condition", true},
		// Not achievement: guard rails / gave up.
		{"loop_end", "max_runs", false},
		{"loop_end", "wall_clock", false},
		{"loop_end", "consecutive_failures", false},
		{"loop_end", "no_progress_for", false},
		// Operator pulled the plug — never an achievement, even if the kind would
		// otherwise qualify.
		{"operator", "retired", false},
		{"operator", "exit_condition", false},
		// hard_stop is a kill, not a finish.
		{"hard_stop", "exit_condition", false},
		// Empty / unknown.
		{"", "", false},
		{"loop_end", "future_unknown_kind", false},
	}
	for _, tc := range cases {
		if got := gardenerKindIsGoalAchieved(tc.slot, tc.kind); got != tc.want {
			t.Errorf("gardenerKindIsGoalAchieved(%q,%q) = %v, want %v", tc.slot, tc.kind, got, tc.want)
		}
	}
}

func TestGardenerGoalState(t *testing.T) {
	cases := []struct {
		name       string
		closed     bool
		slot, kind string
		want       string
	}{
		{"open gardener has no disposition", false, "", "", ""},
		{"closed via exit_condition is achieved", true, "loop_end", "exit_condition", "achieved"},
		{"closed via budget is unachieved", true, "loop_end", "max_runs", "unachieved"},
		{"retired is unachieved", true, "operator", "retired", "unachieved"},
		{"closed with no provenance is unachieved", true, "", "", "unachieved"},
	}
	for _, tc := range cases {
		v := gardenerView{IsClosed: tc.closed, TerminatedSlot: tc.slot, TerminatedKind: tc.kind}
		if got := v.GardenerGoalState(); got != tc.want {
			t.Errorf("%s: GardenerGoalState() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestParseGardenersFilter(t *testing.T) {
	cases := map[string]gardenersFilter{
		// Empty / unrecognized now defaults to the LIVE fleet, not All
		// (ka-0000004655): retired gardeners stay hidden until the operator
		// clicks Archived.
		"":                gardenersFilterLive,
		"garbage":         gardenersFilterLive,
		"all":             gardenersFilterAll,
		"active":          gardenersFilterActive,
		"disabled":        gardenersFilterDisabled,
		"archived":        gardenersFilterArchived,
		"closed":          gardenersFilterArchived, // legacy alias for archived
		"needs-attention": gardenersFilterNeedsAttention,
		"goal-achieved":   gardenersFilterGoalAchieved,
		"unachieved":      gardenersFilterUnachieved,
	}
	for in, want := range cases {
		if got := parseGardenersFilter(in); got != want {
			t.Errorf("parseGardenersFilter(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestGardenerVerdictNeedsAttention(t *testing.T) {
	cases := map[string]bool{
		"stalled":  true,
		"stuck":    true,
		"progress": false,
		"neutral":  false,
		"":         false,
	}
	for kind, want := range cases {
		if got := gardenerVerdictNeedsAttention(kind); got != want {
			t.Errorf("gardenerVerdictNeedsAttention(%q) = %v, want %v", kind, got, want)
		}
	}
}

// TestGardenersRowMatches_PourStall: a pouring-but-never-worked gardener (ka-0000000957)
// whose verdict is otherwise neutral must still match the needs-attention filter
// so the chip count, filter, and row badge stay consistent.
func TestGardenersRowMatches_PourStall(t *testing.T) {
	neutral := gardenerRowView{VerdictKind: "neutral"}
	if gardenersRowMatches(neutral, gardenersFilterNeedsAttention) {
		t.Fatal("a neutral, non-pour-stalled gardener must not match needs-attention")
	}
	pourStalled := gardenerRowView{VerdictKind: "neutral", PourStalled: true}
	if !gardenersRowMatches(pourStalled, gardenersFilterNeedsAttention) {
		t.Fatal("a pour-stalled gardener must match needs-attention even with a neutral verdict")
	}
}

// seedGardenersVerdictFake builds three gardeners with distinct health verdicts so
// the index can be checked for the verdict pill + the "needs attention" filter:
//   - gd-prog "progress":  scored runs + a merged-MR event  -> making progress
//   - gd-stall "stalled":  3 completed runs, zero outcomes   -> stalled
//   - gd-stuck "stuck":    3 consecutive failures            -> stuck
func seedGardenersVerdictFake(t *testing.T) *fakeDaemon {
	t.Helper()
	f := newFake()
	now := time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC)

	mk := func(id, name string, spec map[string]any) *beadsapi.BeadDetail {
		raw, _ := json.Marshal(spec)
		return &beadsapi.BeadDetail{
			ID:        id,
			Title:     name,
			Type:      "trigger",
			Status:    "open",
			Labels:    []string{"trigger:schedule", "role:gardener", "project:gardenio"},
			Fields:    map[string]string{"role": "gardener", "gardener": string(raw)},
			UpdatedAt: now,
		}
	}

	f.triggerScheduleResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			mk("gd-prog", "progressing", map[string]any{
				"name":           "progressing",
				"runs_spawned":   3,
				"runs_completed": 3,
				"progress_state": progress.State{
					Runs: []progress.RunRecord{
						{AgentID: "a1", ProgressScore: 2, AgentState: "done"},
						{AgentID: "a2", ProgressScore: 5, AgentState: "done"},
						{AgentID: "a3", ProgressScore: 1, AgentState: "done"},
					},
					Events: []progress.EventRecord{
						{Kind: progress.EventKindMRMerged, TargetID: "gd-mr1", OccurredAt: now},
					},
				},
			}),
			mk("gd-stall", "stalling", map[string]any{
				"name":           "stalling",
				"runs_spawned":   3,
				"runs_completed": 3,
				// Genuine stall: runs neither scored nor completed `done` (blank
				// terminal state), so there is no outcome to credit. All-`done`
				// runs would now read as progress (ka-0000002776).
				"progress_state": progress.State{
					Runs: []progress.RunRecord{
						{AgentID: "a1", ProgressScore: 0, AgentState: ""},
						{AgentID: "a2", ProgressScore: 0, AgentState: ""},
						{AgentID: "a3", ProgressScore: 0, AgentState: ""},
					},
				},
			}),
			mk("gd-stuck", "stuckcap", map[string]any{
				"name":                 "stuckcap",
				"runs_spawned":         4,
				"runs_completed":       3,
				"consecutive_failures": 3,
			}),
		},
		Total: 3,
	}
	return f
}

// TestHandleGardeners_RunawaySpendFlag guards C1 (bug ka-0000001009): an OPEN
// gardener over the dollar-spend threshold gets the "over" chip class AND is
// surfaced by the needs-attention filter, even though its health verdict is
// fine — a quietly-burning gardener must bubble up without the operator having
// to Spend-sort.
func TestHandleGardeners_RunawaySpendFlag(t *testing.T) {
	t.Setenv(gardenerSpendWarnEnv, "100")
	t.Setenv(gardenerSpendOverEnv, "250")
	now := gardenersNow() // fresh roll-up sync relative to the server clock
	f := newFake()

	mk := func(id, name string, spendUSD string) *beadsapi.BeadDetail {
		// A healthy, progressing verdict so the ONLY attention signal in play is
		// dollar spend — otherwise a spec-less gardener reads stalled/stuck and
		// both rows would land in needs-attention regardless of spend.
		spec := map[string]any{
			"name": name, "runs_spawned": 3, "runs_completed": 3,
			"progress_state": progress.State{
				Runs: []progress.RunRecord{
					{AgentID: "a1", ProgressScore: 3, AgentState: "done"},
					{AgentID: "a2", ProgressScore: 4, AgentState: "done"},
				},
				Events: []progress.EventRecord{
					{Kind: progress.EventKindMRMerged, TargetID: "gd-mr1", OccurredAt: now},
				},
			},
		}
		raw, _ := json.Marshal(spec)
		return &beadsapi.BeadDetail{
			ID: id, Title: name, Type: "trigger", Status: "open",
			Labels: []string{"trigger:schedule", "role:gardener", "project:gardenio"},
			Fields: map[string]string{
				"role":                     "gardener",
				"gardener":                 string(raw),
				gardenerSpendField:         spendUSD,
				gardenerSpendSyncedAtField: now.Format(time.RFC3339),
			},
			UpdatedAt: now,
		}
	}
	f.triggerScheduleResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			mk("gd-burner", "burner", "300"), // runaway
			mk("gd-cheap", "cheapo", "5"),    // fine
		},
		Total: 2,
	}
	mux := mkServerWithNow(t, f, gardenersNow)

	// Default view: the burner row carries the "over" chip class; the cheap one does not.
	req := httptest.NewRequest("GET", "/gardeners", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "cap-spend-over") {
		t.Errorf("runaway gardener should render the cap-spend-over chip class")
	}

	// Needs-attention filter keeps the burner (spend runaway) and drops the cheap one.
	req2 := httptest.NewRequest("GET", "/gardeners?filter=needs-attention", nil)
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, req2)
	body2 := w2.Body.String()
	if !strings.Contains(body2, `href="/gardener/gd-burner"`) {
		t.Errorf("needs-attention filter must surface the runaway-spend gardener")
	}
	if strings.Contains(body2, `href="/gardener/gd-cheap"`) {
		t.Errorf("needs-attention filter should not surface the cheap gardener")
	}
}

func TestHandleGardeners_VerdictPillAndCount(t *testing.T) {
	f := seedGardenersVerdictFake(t)
	mux := mkServerWithNow(t, f, gardenersNow)

	req := httptest.NewRequest("GET", "/gardeners", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// Verdict pills render with the per-kind CSS hook for each gardener.
	for _, want := range []string{
		"gardener-verdict-progress",
		"gardener-verdict-stalled",
		"gardener-verdict-stuck",
		"making progress",
		"stalled",
		"stuck",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing verdict marker %q", want)
		}
	}

	// The "needs attention" chip exists and its badge counts the two
	// stalled+stuck gardeners (not the progressing one).
	if !strings.Contains(body, "Needs attention") {
		t.Errorf("missing 'Needs attention' filter chip")
	}
	if !strings.Contains(body, `href="/gardeners?filter=needs-attention"`) {
		t.Errorf("needs-attention chip missing its href")
	}
}

func TestHandleGardeners_FilterNeedsAttention(t *testing.T) {
	f := seedGardenersVerdictFake(t)
	mux := mkServerWithNow(t, f, gardenersNow)

	req := httptest.NewRequest("GET", "/gardeners?filter=needs-attention", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// Keeps the stalled + stuck gardeners, drops the progressing one.
	if strings.Contains(body, `href="/gardener/gd-prog"`) {
		t.Errorf("needs-attention filter should hide the progressing gardener gd-prog")
	}
	for _, want := range []string{`href="/gardener/gd-stall"`, `href="/gardener/gd-stuck"`} {
		if !strings.Contains(body, want) {
			t.Errorf("needs-attention filter dropped %q", want)
		}
	}
}

// seedGardenersGoalFake builds three gardeners exercising goal disposition:
//   - gd-won   : closed via loop_end exit_condition       -> achieved
//   - gd-spent : closed via loop_end max_runs             -> unachieved (closed)
//   - gd-live  : open, still working                      -> unachieved (open)
func seedGardenersGoalFake(t *testing.T) *fakeDaemon {
	t.Helper()
	f := newFake()
	now := time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC)

	mk := func(id, name string, status string, fields map[string]string) *beadsapi.BeadDetail {
		spec := map[string]any{"name": name, "terminal_goal": "ship the thing", "runs_spawned": 3, "runs_completed": 3}
		raw, _ := json.Marshal(spec)
		beadFields := map[string]string{"role": "gardener", "gardener": string(raw)}
		maps.Copy(beadFields, fields)
		return &beadsapi.BeadDetail{
			ID: id, Title: name, Type: "trigger", Status: status,
			Labels:    []string{"trigger:schedule", "role:gardener", "project:gardenio"},
			Fields:    beadFields,
			UpdatedAt: now,
		}
	}

	f.triggerScheduleResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			mk("gd-won", "winner", "closed", map[string]string{
				"gardener_terminated_slot": "loop_end", "gardener_terminated_kind": "exit_condition",
			}),
			mk("gd-spent", "spender", "closed", map[string]string{
				"gardener_terminated_slot": "loop_end", "gardener_terminated_kind": "max_runs",
			}),
			mk("gd-live", "liver", "open", nil),
		},
		Total: 3,
	}
	return f
}

func TestHandleGardeners_GoalAchievedBadgeAndChips(t *testing.T) {
	f := seedGardenersGoalFake(t)
	mux := mkServerWithNow(t, f, gardenersNow)

	// Use ?filter=all: the fixture's achiever/spender are CLOSED, and the
	// default Live view now hides retired (closed) gardeners (ka-0000004655).
	req := httptest.NewRequest("GET", "/gardeners?filter=all", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// The loud achievement badge + accent row class render for the winner.
	if !strings.Contains(body, "GOAL ACHIEVED") {
		t.Errorf("missing loud GOAL ACHIEVED badge for gd-won")
	}
	if !strings.Contains(body, "cap-row-achieved") {
		t.Errorf("achiever row missing the cap-row-achieved accent class")
	}
	// The budget-exhausted closer is loudly marked as NOT having reached goal.
	if !strings.Contains(body, "goal not reached") {
		t.Errorf("missing 'goal not reached' marker for gd-spent")
	}
	// Both new chips exist with their hrefs.
	for _, want := range []string{
		`href="/gardeners?filter=goal-achieved"`,
		`href="/gardeners?filter=unachieved"`,
		"Goal achieved",
		"Goal not reached yet",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing chip marker %q", want)
		}
	}
}

// TestHandleGardeners_ClosedUnachievedShowsNeutralLifecycleChip guards bug
// ka-0000000939: the index status lane used to REPLACE the lifecycle chip with
// a conflated amber "closed · goal not reached" label for any closed gardener
// that missed its goal marker, while the detail page kept a neutral "closed"
// chip plus a SEPARATE goal badge — so the two views disagreed. The lifecycle
// status (active/disabled/closed) and the goal disposition are distinct facts;
// the index must render them as distinct chips, matching the detail page.
func TestHandleGardeners_ClosedUnachievedShowsNeutralLifecycleChip(t *testing.T) {
	f := seedGardenersGoalFake(t)
	mux := mkServerWithNow(t, f, gardenersNow)

	// ?filter=all: the closed-unachieved gardener under test is hidden by the
	// default Live view (ka-0000004655), so query the lifecycle-agnostic view.
	req := httptest.NewRequest("GET", "/gardeners?filter=all", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// gd-spent is closed-via-budget (unachieved). Its row must carry BOTH a
	// neutral lifecycle "closed" chip AND a separate "goal not reached" badge.
	if !strings.Contains(body, `cap-status cap-status-closed`) {
		t.Errorf("closed gardener missing neutral lifecycle 'closed' chip — index conflates lifecycle with goal-state again (bug ka-0000000939)")
	}
	if !strings.Contains(body, `cap-status-unachieved`) || !strings.Contains(body, "goal not reached") {
		t.Errorf("closed-unachieved gardener missing the separate 'goal not reached' badge")
	}
	// The OLD conflated label must be gone — lifecycle is never folded into the
	// goal chip on the index any more.
	if strings.Contains(body, "closed · goal not reached") {
		t.Errorf("index still renders the conflated 'closed · goal not reached' label — lifecycle and goal-state must be separate chips (bug ka-0000000939)")
	}
}

func TestHandleGardeners_FilterGoalAchievedAndUnachieved(t *testing.T) {
	f := seedGardenersGoalFake(t)
	mux := mkServerWithNow(t, f, gardenersNow)

	// goal-achieved: only the winner.
	req := httptest.NewRequest("GET", "/gardeners?filter=goal-achieved", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, `href="/gardener/gd-won"`) {
		t.Errorf("goal-achieved filter dropped the achiever gd-won")
	}
	for _, hidden := range []string{`href="/gardener/gd-spent"`, `href="/gardener/gd-live"`} {
		if strings.Contains(body, hidden) {
			t.Errorf("goal-achieved filter should hide %q", hidden)
		}
	}

	// unachieved: the live one + the budget-spent one, NOT the winner.
	req = httptest.NewRequest("GET", "/gardeners?filter=unachieved", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body = w.Body.String()
	for _, want := range []string{`href="/gardener/gd-spent"`, `href="/gardener/gd-live"`} {
		if !strings.Contains(body, want) {
			t.Errorf("unachieved filter dropped %q", want)
		}
	}
	if strings.Contains(body, `href="/gardener/gd-won"`) {
		t.Errorf("unachieved filter should hide the achiever gd-won")
	}
}

// stripTags removes anything between '<' and '>' so a cell's visible text can be
// checked for emptiness. Good enough for the simple, well-formed markup the
// gardeners template emits.
func stripTags(s string) string {
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch r {
		case '<':
			depth++
		case '>':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// TestFleetBoardAndGardenersQueryAgree is the regression guard for ka-0000000919:
// the LIVE-gardener predicate (fleetGardenerIsLive) over the canonical /gardeners
// query (loadGardenerRows) MUST surface a gardener created the "wrong way" —
// role=gardener as a FIELD only, with NO role:gardener LABEL. The bug: that
// field-only gardener was missed by the label-only audit ground truth, reading
// as node_vs_truth_delta=-4 phantom gardeners. loadGardenerRows unions the label
// and field queries, so it surfaces BOTH the canonical and the field-only
// gardener; fleetGardenerIsLive then classifies them as live.
//
// The old /maps fleet board (buildFleetMap) was retired in ka-0000000247 and
// fleetGardenerIsLive was relocated into gardeners.go; this test now guards the
// relocated predicate directly over loadGardenerRows rather than through the
// deleted board builder. The measurer fix in measure-maps.py (_is_gardener_role:
// label OR field) mirrors the same union.
func TestFleetBoardAndGardenersQueryAgree(t *testing.T) {
	f := newFake()

	mkSpec := func(name string) string {
		raw, _ := json.Marshal(map[string]any{"name": name, "terminal_goal": "do the thing"})
		return string(raw)
	}

	// Canonical gardener: role:gardener LABEL *and* role=gardener field. Served via
	// the label query (triggerScheduleResp).
	canon := &beadsapi.BeadDetail{
		ID:        "gd-canon",
		Title:     "canon",
		Type:      "trigger",
		Status:    "open",
		Labels:    []string{"trigger:schedule", "role:gardener", "project:gardenio"},
		Fields:    map[string]string{"role": "gardener", "gardener": mkSpec("canon")},
		UpdatedAt: time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
	}
	// Field-only gardener: role=gardener FIELD, NO role:gardener label — the exact
	// ka-0000000919 shape. Served ONLY via the role=gardener field query
	// (gardenerFieldOnlyResp); the label query never returns it.
	fieldOnly := &beadsapi.BeadDetail{
		ID:        "gd-fieldonly",
		Title:     "fieldonly",
		Type:      "trigger",
		Status:    "open",
		Labels:    []string{"trigger:schedule", "project:gardenio"},
		Fields:    map[string]string{"role": "gardener", "gardener": mkSpec("fieldonly")},
		UpdatedAt: time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
	}
	f.triggerScheduleResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{canon}, Total: 1}
	f.gardenerFieldOnlyResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{fieldOnly}, Total: 1}

	srv := gardenerTestServer(t, f)

	// The /gardeners projection: every gardener loadGardenerRows surfaces.
	rows, _, _, err := srv.loadGardenerRows(t.Context())
	if err != nil {
		t.Fatalf("loadGardenerRows: %v", err)
	}

	// The /gardeners LIVE set: rows fleetGardenerIsLive classifies as live.
	gardenersLive := map[string]bool{}
	for _, r := range rows {
		if fleetGardenerIsLive(r) {
			gardenersLive[r.ID] = true
		}
	}

	// The live set must include the field-only gardener (the regression): it is
	// surfaced by loadGardenerRows' label∪field union and classified live by the
	// relocated predicate.
	if !gardenersLive["gd-fieldonly"] {
		t.Errorf("field-only gardener gd-fieldonly missing from /gardeners live set — the ka-0000000919 regression")
	}
	if !gardenersLive["gd-canon"] {
		t.Errorf("canonical gardener gd-canon missing from /gardeners live set")
	}
	// Both seeded gardeners are open type=trigger gardeners, so the live set is
	// exactly those two — no phantom additions or drops.
	if len(gardenersLive) != 2 {
		t.Fatalf("expected exactly 2 live gardeners (canon + fieldonly), got %d: %v", len(gardenersLive), gardenersLive)
	}
}

// TestHandleGardeners_SelfCensusBadge verifies the chip-strip self-census
// reconciliation badge (ka-0000001407): when the lifecycle partition
// (active+disabled+closed) sums to the All chip, the page renders a green
// "reconciles" badge so an operator can trust the chip counts at a glance.
func TestHandleGardeners_SelfCensusBadge(t *testing.T) {
	f := seedGardenersIndexFake(t) // 2 gardeners: 1 active, 1 disabled
	mux := mkServerWithNow(t, f, gardenersNow)

	req := httptest.NewRequest("GET", "/gardeners", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// The lifecycle partition (1 active + 1 disabled + 0 closed = 2 == All)
	// reconciles, so the green badge renders and the amber one does not. Assert
	// on the rendered span class (both class names appear in the inline <style>
	// block, so match the full span markup to distinguish CSS from usage).
	if !strings.Contains(body, `class="gardeners-census gardeners-census-ok"`) {
		t.Errorf("missing green self-census badge span; body=%s", body)
	}
	if strings.Contains(body, `class="gardeners-census gardeners-census-warn"`) {
		t.Errorf("unexpected drift badge when chips reconcile")
	}
	if !strings.Contains(body, "2 gardeners · reconciles") {
		t.Errorf("missing reconciles count phrase; body=%s", body)
	}
}

// TestSortGardenerRows_AttentionFirst guards the attention-first pin
// (ka-0000002827): an OPEN gardener flagged NeedsAttention (stalled/stuck) must
// sort above a running gardener, a recently-completed gardener, and a closed one,
// so an on-call operator sees the runaway/wedged loops at the top of the
// default All view without switching filters.
func TestSortGardenerRows_AttentionFirst(t *testing.T) {
	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	rows := []gardenerRowView{
		{ID: "gd-running", IsRunning: true, lastActivityAt: now.Add(-1 * time.Minute)},
		{ID: "gd-recent", LastCompletedAt: now.Add(-5 * time.Minute)},
		{ID: "gd-closed", IsClosed: true, LastCompletedAt: now.Add(-2 * time.Minute)},
		{ID: "gd-attention", NeedsAttention: true, VerdictKind: "stuck"},
	}
	sortGardenerRows(rows)
	if rows[0].ID != "gd-attention" {
		t.Fatalf("needs-attention row should pin to the top; got order %v", rowIDs(rows))
	}
	// The closed gardener must still sink to the bottom.
	if rows[len(rows)-1].ID != "gd-closed" {
		t.Errorf("closed gardener should sink last; got order %v", rowIDs(rows))
	}
}

// TestSortGardenerRows_AttentionTieBreaksByActivity verifies the sub-keys still
// apply WITHIN the needs-attention group: among two attention rows, the running
// one leads (running-first holds inside the pinned band).
func TestSortGardenerRows_AttentionTieBreaksByActivity(t *testing.T) {
	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	rows := []gardenerRowView{
		{ID: "gd-att-idle", NeedsAttention: true, VerdictKind: "stalled", LastCompletedAt: now.Add(-30 * time.Minute)},
		{ID: "gd-att-running", NeedsAttention: true, VerdictKind: "stuck", IsRunning: true, lastActivityAt: now.Add(-1 * time.Minute)},
	}
	sortGardenerRows(rows)
	if rows[0].ID != "gd-att-running" {
		t.Errorf("running attention row should lead the attention band; got %v", rowIDs(rows))
	}
}

func rowIDs(rows []gardenerRowView) []string {
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	return ids
}

// TestBuildGardenersHidden verifies the "N hidden — why" view-model (bug
// ka-0000007605): excluded beads are classified per reason, the per-class
// lists are populated, and the true-population arithmetic reconciles the
// rendered roster against the full role:gardener universe.
func TestBuildGardenersHidden(t *testing.T) {
	excluded := []gardenerExcluded{
		{ID: "gd-fix1", Title: "[E2E fixture] a", Reason: gardenerExcludedFixture, Href: "/gd-fix1"},
		{ID: "gd-mir1", Title: "schedule-mirror: x", Reason: gardenerExcludedScheduleMirror, Href: "/gd-mir1"},
		{ID: "gd-mir2", Title: "schedule-mirror: y", Reason: gardenerExcludedScheduleMirror, Href: "/gd-mir2"},
	}
	h := buildGardenersHidden(136, excluded)

	if h.Total != 3 {
		t.Errorf("Total = %d; want 3", h.Total)
	}
	if h.FixtureCount != 1 || len(h.Fixtures) != 1 {
		t.Errorf("FixtureCount/len = %d/%d; want 1/1", h.FixtureCount, len(h.Fixtures))
	}
	if h.MirrorCount != 2 || len(h.ScheduleMirrors) != 2 {
		t.Errorf("MirrorCount/len = %d/%d; want 2/2", h.MirrorCount, len(h.ScheduleMirrors))
	}
	if h.Rendered != 136 {
		t.Errorf("Rendered = %d; want 136", h.Rendered)
	}
	// The whole-fleet arithmetic the affordance certifies: rendered + hidden.
	if h.TruePopulation != 139 {
		t.Errorf("TruePopulation = %d; want 139 (136 rendered + 3 hidden)", h.TruePopulation)
	}
}

// TestBuildGardenersHidden_NoneExcluded verifies the zero value so the template
// renders no disclosure when the roster is the entire population.
func TestBuildGardenersHidden_NoneExcluded(t *testing.T) {
	h := buildGardenersHidden(105, nil)
	if h.Total != 0 {
		t.Errorf("Total = %d; want 0 when nothing excluded", h.Total)
	}
	if h.TruePopulation != 105 {
		t.Errorf("TruePopulation = %d; want 105", h.TruePopulation)
	}
	if len(h.Fixtures) != 0 || len(h.ScheduleMirrors) != 0 {
		t.Errorf("expected empty per-class lists when nothing excluded")
	}
}

// TestHandleGardeners_HiddenAffordance_BothClasses seeds one fixture and one
// schedule-mirror alongside the real gardeners and asserts the index renders a
// single disclosure accounting for BOTH excluded classes with the right counts
// and per-bead links (bug ka-0000007605, criterion B). This is the end-to-end
// guard that the 9-gardener delta the CAMP audit flagged is now explained rather
// than silent.
func TestHandleGardeners_HiddenAffordance_BothClasses(t *testing.T) {
	f := seedGardenersIndexFake(t) // 2 real gardeners
	f.triggerScheduleResp.Beads = append(f.triggerScheduleResp.Beads,
		mkFixtureGardenerBead(), mkScheduleMirrorGardenerBead())
	f.triggerScheduleResp.Total = len(f.triggerScheduleResp.Beads)

	mux := mkServerWithNow(t, f, gardenersNow)
	req := httptest.NewRequest("GET", "/gardeners", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// Exactly the 2 real gardeners render as rows; the All chip stays at 2.
	if !strings.Contains(body, "2 hidden from this roster") {
		t.Errorf("expected '2 hidden from this roster'; body=%s", body)
	}
	// Each excluded class is named with its count and the bead is linked to its
	// plain bead page (not /gardener/{id}, which 404s for both classes).
	for _, want := range []string{
		"1 E2E / test fixture",
		"1 schedule-mirror artifact",
		`href="/ka-0000004925"`,
		`href="/ka-0000005169"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("hidden disclosure missing %q; body=%s", want, body)
		}
	}
	// Neither excluded bead leaks a /gardener/{id} life-thread link.
	for _, banned := range []string{`href="/gardener/ka-0000004925"`, `href="/gardener/ka-0000005169"`} {
		if strings.Contains(body, banned) {
			t.Errorf("excluded bead leaked a gardener row link: %q", banned)
		}
	}
}

// TestHandleGardeners_NoHiddenAffordanceWhenComplete asserts the disclosure is
// absent when the roster IS the full population — no excluded beads means no
// "N hidden" line to mislead the operator (bug ka-0000007605).
func TestHandleGardeners_NoHiddenAffordanceWhenComplete(t *testing.T) {
	f := seedGardenersIndexFake(t) // 2 real gardeners, nothing excluded
	mux := mkServerWithNow(t, f, gardenersNow)
	req := httptest.NewRequest("GET", "/gardeners", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "hidden from this roster") {
		t.Errorf("did not expect a hidden-affordance when nothing is excluded")
	}
}
