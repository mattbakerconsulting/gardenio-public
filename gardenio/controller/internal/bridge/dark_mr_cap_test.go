package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// darkMRCapDaemon is a gardenerDaemon stub for the dark MR-generation caps. It
// serves a fixed set of open merge-request beads (for the fleet + per-gardener
// scans) and, when withLister is true, ALSO satisfies producingTaskLister so the
// per-gardener attribution path engages. The kill-switch is always disengaged and
// the bounded-stop guard always passes (bounded gardeners in the helpers), so any
// skip returned by gardenerPreFireCheck in these tests comes from the MR caps.
type darkMRCapDaemon struct {
	openMRs    []*beadsapi.BeadDetail
	listErr    error
	withLister bool
	// producers maps MR-bead-id → the producing task beads returned by
	// ProducingTaskBeadsForMR. Only consulted when withLister is true.
	producers map[string][]*beadsapi.BeadDetail
	prodErr   error
}

func (d *darkMRCapDaemon) GetBead(_ context.Context, _ string) (*beadsapi.BeadDetail, error) {
	return nil, nil
}

func (d *darkMRCapDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	// The global gardeners-pause kill-switch lookup searches by title — return
	// empty so the kill-switch is never engaged in these tests.
	if q.Search == gardenersPauseTitle {
		return &beadsapi.ListBeadsResult{}, nil
	}
	// The MR-cap scan asks for type=merge-request beads.
	for _, t := range q.Types {
		if t == "merge-request" {
			if d.listErr != nil {
				return nil, d.listErr
			}
			return &beadsapi.ListBeadsResult{Beads: d.openMRs}, nil
		}
	}
	return &beadsapi.ListBeadsResult{}, nil
}

func (d *darkMRCapDaemon) UpdateBeadFields(_ context.Context, _ string, _ map[string]string) error {
	return nil
}
func (d *darkMRCapDaemon) UpdateBeadFieldsTyped(_ context.Context, _ string, _ map[string]any) error {
	return nil
}
func (d *darkMRCapDaemon) AddComment(_ context.Context, _, _, _ string) error { return nil }
func (d *darkMRCapDaemon) AddLabel(_ context.Context, _, _ string) error      { return nil }
func (d *darkMRCapDaemon) CloseBead(_ context.Context, _ string, _ map[string]string) error {
	return nil
}
func (d *darkMRCapDaemon) UpdateBead(_ context.Context, _ string, _ beadsapi.UpdateBeadRequest) error {
	return nil
}
func (d *darkMRCapDaemon) MRBeadForTask(_ context.Context, _ string) (*beadsapi.BeadDetail, error) {
	return nil, nil
}

// ProducingTaskBeadsForMR is only present so darkMRCapDaemon CAN satisfy
// producingTaskLister — but the cap code type-asserts on the interface, so a
// daemon that wants to OPT OUT (to test the no-lister degrade path) sets
// withLister=false and we hide this by wrapping it (see noListerDaemon).
func (d *darkMRCapDaemon) ProducingTaskBeadsForMR(_ context.Context, mrBeadID string) ([]*beadsapi.BeadDetail, error) {
	if d.prodErr != nil {
		return nil, d.prodErr
	}
	return d.producers[mrBeadID], nil
}

// noListerDaemon wraps a gardenerDaemon WITHOUT promoting ProducingTaskBeadsForMR,
// so a type-assertion to producingTaskLister fails — exercising the per-gardener
// cap's "daemon doesn't expose the reverse-produces walk" degrade path.
type noListerDaemon struct {
	openMRs []*beadsapi.BeadDetail
}

func (d *noListerDaemon) GetBead(_ context.Context, _ string) (*beadsapi.BeadDetail, error) {
	return nil, nil
}
func (d *noListerDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if q.Search == gardenersPauseTitle {
		return &beadsapi.ListBeadsResult{}, nil
	}
	for _, t := range q.Types {
		if t == "merge-request" {
			return &beadsapi.ListBeadsResult{Beads: d.openMRs}, nil
		}
	}
	return &beadsapi.ListBeadsResult{}, nil
}
func (d *noListerDaemon) UpdateBeadFields(_ context.Context, _ string, _ map[string]string) error {
	return nil
}
func (d *noListerDaemon) UpdateBeadFieldsTyped(_ context.Context, _ string, _ map[string]any) error {
	return nil
}
func (d *noListerDaemon) AddComment(_ context.Context, _, _, _ string) error { return nil }
func (d *noListerDaemon) AddLabel(_ context.Context, _, _ string) error      { return nil }
func (d *noListerDaemon) CloseBead(_ context.Context, _ string, _ map[string]string) error {
	return nil
}
func (d *noListerDaemon) UpdateBead(_ context.Context, _ string, _ beadsapi.UpdateBeadRequest) error {
	return nil
}
func (d *noListerDaemon) MRBeadForTask(_ context.Context, _ string) (*beadsapi.BeadDetail, error) {
	return nil, nil
}

// darkMRURLIID is a monotonic source of numeric MR IIDs for test URLs.
// mrURLPattern requires a numeric IID at the tail, so the URL cannot reuse the
// (string) bead id — we mint a fresh number per bead instead.
var darkMRURLIID int

func nextMRIID() int {
	darkMRURLIID++
	return darkMRURLIID
}

// darkMRBead builds an open type=merge-request bead with a gitlab-dark mr_url
// (so isDarkMRBead classifies it dark) created `age` ago.
func darkMRBead(id string, age time.Duration) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:        id,
		Type:      "merge-request",
		Status:    "open",
		CreatedAt: time.Now().Add(-age),
		Fields: map[string]string{
			"mr_url": fmt.Sprintf("https://gitlab-dark.gardenio-e2e.app.e2e.dev.acme.example/monorepo-dark/monorepo/-/merge_requests/%d", nextMRIID()),
		},
	}
}

// nonDarkMRBead builds an open MR bead on the upstream (gitlab.com) host — NOT
// dark, must never be counted by the caps.
func nonDarkMRBead(id string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:        id,
		Type:      "merge-request",
		Status:    "open",
		CreatedAt: time.Now(),
		Fields: map[string]string{
			"mr_url": fmt.Sprintf("https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/%d", nextMRIID()),
		},
	}
}

// upstreamMirrorMRBead builds an open dark-host MR bead that is flagged as an
// external upstream mirror — must be excluded from the dark count.
func upstreamMirrorMRBead(id string) *beadsapi.BeadDetail {
	b := darkMRBead(id, 0)
	b.Fields["external_mirror"] = "true"
	b.Labels = []string{"external-mirror:true", "upstream:true"}
	return b
}

func darkMRCapGardenerTrigger(t *testing.T) TriggerDef {
	t.Helper()
	// A perfectly bounded dark gardener — so the bounded-stop guard passes and
	// the only thing that can skip the fire is an MR cap.
	spec := GardenerSpec{
		AssignBead: "gd-darkepic",
		RunBudget:  20,
		Stop:       json.RawMessage(boundedStopBlob(t, "168h")),
	}
	return TriggerDef{
		ID:       "gd-darkcap",
		Project:  "gardenio",
		Labels:   []string{"role:gardener", "monorepo-dark"},
		Gardener: &spec,
	}
}

// ── isDarkMRBead classification ────────────────────────────────────────────

func TestIsDarkMRBead(t *testing.T) {
	cases := []struct {
		name string
		bead *beadsapi.BeadDetail
		want bool
	}{
		{"dark by host", darkMRBead("1", 0), true},
		{"non-dark upstream host", nonDarkMRBead("2"), false},
		{"upstream mirror excluded", upstreamMirrorMRBead("3"), false},
		{"nil bead", nil, false},
		{
			"dark by label, no url",
			&beadsapi.BeadDetail{ID: "4", Labels: []string{"monorepo-dark"}, Fields: map[string]string{}},
			true,
		},
		{
			"upstream:true label excludes even on dark host",
			func() *beadsapi.BeadDetail {
				b := darkMRBead("5", 0)
				b.Labels = []string{"upstream:true"}
				return b
			}(),
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDarkMRBead(tc.bead); got != tc.want {
				t.Errorf("isDarkMRBead = %v, want %v", got, tc.want)
			}
		})
	}
}

// ── fleet concurrency cap ──────────────────────────────────────────────────

func TestDarkMRCaps_FleetCapFiresClosed(t *testing.T) {
	// Exactly darkMaxConcurrentOpenDarkMRs open dark MRs ⇒ at the ceiling ⇒ block.
	mrs := make([]*beadsapi.BeadDetail, 0, darkMaxConcurrentOpenDarkMRs)
	for i := 0; i < darkMaxConcurrentOpenDarkMRs; i++ {
		// Created long ago so they DON'T also trip the per-gardener window — we
		// want to prove the FLEET cap specifically. (No producers ⇒ not owned.)
		mrs = append(mrs, darkMRBead(fmt.Sprintf("fleet-%d", i), 48*time.Hour))
	}
	d := &darkMRCapDaemon{openMRs: mrs, withLister: true, producers: map[string][]*beadsapi.BeadDetail{}}
	trig := darkMRCapGardenerTrigger(t)

	skip, reason := gardenerPreFireCheck(context.Background(), d, nil, trig)
	if !skip {
		t.Fatal("expected fire suppressed at the fleet open-dark-MR ceiling")
	}
	if reason != gardenerSkipDarkMRFleetCap {
		t.Errorf("reason = %q, want %q", reason, gardenerSkipDarkMRFleetCap)
	}
}

func TestDarkMRCaps_FleetCapUnderLimitPasses(t *testing.T) {
	// One under the ceiling ⇒ fleet cap does not fire; no producers ⇒ per-gardener
	// cap doesn't fire either, so the dark guard lets the fire proceed.
	mrs := make([]*beadsapi.BeadDetail, 0, darkMaxConcurrentOpenDarkMRs-1)
	for i := 0; i < darkMaxConcurrentOpenDarkMRs-1; i++ {
		mrs = append(mrs, darkMRBead(fmt.Sprintf("fleet-%d", i), 48*time.Hour))
	}
	d := &darkMRCapDaemon{openMRs: mrs, withLister: true, producers: map[string][]*beadsapi.BeadDetail{}}
	trig := darkMRCapGardenerTrigger(t)

	skip, reason := gardenerPreFireCheck(context.Background(), d, nil, trig)
	if skip && (reason == gardenerSkipDarkMRFleetCap || reason == gardenerSkipDarkMRPerGardenerCap) {
		t.Fatalf("under-limit dark gardener wrongly blocked by an MR cap: reason=%q", reason)
	}
}

func TestDarkMRCaps_NonDarkAndMirrorMRsNotCounted(t *testing.T) {
	// A flood of NON-dark + upstream-mirror MRs (well over the ceiling) must NOT
	// trip the fleet cap, because only true dark MRs count.
	var mrs []*beadsapi.BeadDetail
	for i := 0; i < darkMaxConcurrentOpenDarkMRs*2; i++ {
		mrs = append(mrs, nonDarkMRBead(fmt.Sprintf("up-%d", i)))
		mrs = append(mrs, upstreamMirrorMRBead(fmt.Sprintf("mir-%d", i)))
	}
	d := &darkMRCapDaemon{openMRs: mrs, withLister: true, producers: map[string][]*beadsapi.BeadDetail{}}
	trig := darkMRCapGardenerTrigger(t)

	skip, reason := gardenerPreFireCheck(context.Background(), d, nil, trig)
	if skip && (reason == gardenerSkipDarkMRFleetCap || reason == gardenerSkipDarkMRPerGardenerCap) {
		t.Fatalf("non-dark / mirror MRs wrongly counted toward a dark cap: reason=%q", reason)
	}
}

// ── per-gardener rate window ────────────────────────────────────────────────

func TestDarkMRCaps_PerGardenerWindowFiresClosed(t *testing.T) {
	// darkMaxMRsPerGardenerWindow recent dark MRs, all produced by THIS gardener's
	// AssignBead, all inside the window ⇒ per-gardener cap fires. Keep the fleet
	// count under its ceiling so we prove the per-gardener cap specifically.
	var mrs []*beadsapi.BeadDetail
	producers := map[string][]*beadsapi.BeadDetail{}
	for i := 0; i < darkMaxMRsPerGardenerWindow; i++ {
		id := fmt.Sprintf("mine-%d", i)
		mrs = append(mrs, darkMRBead(id, 5*time.Minute)) // inside the 1h window
		producers[id] = []*beadsapi.BeadDetail{
			{ID: "gd-darkepic", Fields: map[string]string{}}, // == gardener AssignBead
		}
	}
	d := &darkMRCapDaemon{openMRs: mrs, withLister: true, producers: producers}
	trig := darkMRCapGardenerTrigger(t)

	skip, reason := gardenerPreFireCheck(context.Background(), d, nil, trig)
	if !skip {
		t.Fatal("expected fire suppressed when gardener hit its per-window MR rate")
	}
	if reason != gardenerSkipDarkMRPerGardenerCap {
		t.Errorf("reason = %q, want %q", reason, gardenerSkipDarkMRPerGardenerCap)
	}
}

func TestDarkMRCaps_PerGardenerAttributedByTriggerID(t *testing.T) {
	// Same as above but attribution is via the producing task's trigger_id
	// (the per-fire task path) rather than AssignBead.
	var mrs []*beadsapi.BeadDetail
	producers := map[string][]*beadsapi.BeadDetail{}
	for i := 0; i < darkMaxMRsPerGardenerWindow; i++ {
		id := fmt.Sprintf("trig-%d", i)
		mrs = append(mrs, darkMRBead(id, 1*time.Minute))
		producers[id] = []*beadsapi.BeadDetail{
			{ID: fmt.Sprintf("gd-task-%d", i), Fields: map[string]string{"trigger_id": "gd-darkcap"}},
		}
	}
	d := &darkMRCapDaemon{openMRs: mrs, withLister: true, producers: producers}
	trig := darkMRCapGardenerTrigger(t)

	skip, reason := gardenerPreFireCheck(context.Background(), d, nil, trig)
	if !skip || reason != gardenerSkipDarkMRPerGardenerCap {
		t.Fatalf("expected per-gardener cap via trigger_id, got skip=%v reason=%q", skip, reason)
	}
}

func TestDarkMRCaps_PerGardenerOldMRsAgeOut(t *testing.T) {
	// Over-cap COUNT of this gardener's dark MRs, but all created OUTSIDE the
	// window ⇒ the gardener has aged out and must NOT be paused.
	var mrs []*beadsapi.BeadDetail
	producers := map[string][]*beadsapi.BeadDetail{}
	for i := 0; i < darkMaxMRsPerGardenerWindow+3; i++ {
		id := fmt.Sprintf("old-%d", i)
		mrs = append(mrs, darkMRBead(id, darkMRPerGardenerWindow+time.Hour)) // outside window
		producers[id] = []*beadsapi.BeadDetail{
			{ID: "gd-darkepic", Fields: map[string]string{}},
		}
	}
	d := &darkMRCapDaemon{openMRs: mrs, withLister: true, producers: producers}
	trig := darkMRCapGardenerTrigger(t)

	skip, reason := gardenerPreFireCheck(context.Background(), d, nil, trig)
	if skip && reason == gardenerSkipDarkMRPerGardenerCap {
		t.Fatal("per-gardener cap fired on MRs that have aged out of the window")
	}
}

func TestDarkMRCaps_PerGardenerOtherGardenersMRsNotCounted(t *testing.T) {
	// A flood of recent dark MRs produced by a DIFFERENT gardener must not pause
	// this gardener.
	var mrs []*beadsapi.BeadDetail
	producers := map[string][]*beadsapi.BeadDetail{}
	for i := 0; i < darkMaxMRsPerGardenerWindow+3; i++ {
		id := fmt.Sprintf("other-%d", i)
		mrs = append(mrs, darkMRBead(id, 2*time.Minute))
		producers[id] = []*beadsapi.BeadDetail{
			{ID: fmt.Sprintf("gd-othertask-%d", i), Fields: map[string]string{"trigger_id": "gd-OTHERCAP"}},
		}
	}
	d := &darkMRCapDaemon{openMRs: mrs, withLister: true, producers: producers}
	trig := darkMRCapGardenerTrigger(t)

	skip, reason := gardenerPreFireCheck(context.Background(), d, nil, trig)
	if skip && reason == gardenerSkipDarkMRPerGardenerCap {
		t.Fatal("per-gardener cap fired on another gardener's MRs")
	}
}

// ── fail-open & degrade paths ──────────────────────────────────────────────

func TestDarkMRCaps_FailOpenOnDaemonError(t *testing.T) {
	// A daemon read error must FAIL-OPEN — the caps never wedge the dark
	// universe (the bounded-stop guard + kill-switch are the backstops).
	d := &darkMRCapDaemon{listErr: fmt.Errorf("boom"), withLister: true}
	trig := darkMRCapGardenerTrigger(t)

	skip, reason := gardenerPreFireCheck(context.Background(), d, nil, trig)
	if skip && (reason == gardenerSkipDarkMRFleetCap || reason == gardenerSkipDarkMRPerGardenerCap) {
		t.Fatalf("MR cap must fail OPEN on a daemon error, got skip=%v reason=%q", skip, reason)
	}
}

func TestDarkMRCaps_PerGardenerDegradesWithoutLister(t *testing.T) {
	// A daemon that does NOT implement producingTaskLister cannot attribute MRs,
	// so the per-gardener cap silently degrades to a no-op — but the FLEET cap
	// still applies. Here we stay under the fleet ceiling, so the fire proceeds.
	var mrs []*beadsapi.BeadDetail
	for i := 0; i < darkMaxConcurrentOpenDarkMRs-1; i++ {
		mrs = append(mrs, darkMRBead(fmt.Sprintf("nl-%d", i), 1*time.Minute))
	}
	d := &noListerDaemon{openMRs: mrs}
	trig := darkMRCapGardenerTrigger(t)

	skip, reason := gardenerPreFireCheck(context.Background(), d, nil, trig)
	if skip && (reason == gardenerSkipDarkMRFleetCap || reason == gardenerSkipDarkMRPerGardenerCap) {
		t.Fatalf("without a lister and under the fleet ceiling, no MR cap should fire: reason=%q", reason)
	}
}

func TestDarkMRCaps_NoListerStillEnforcesFleetCap(t *testing.T) {
	// Even without the per-gardener lister, the fleet cap still bites at the
	// ceiling.
	mrs := make([]*beadsapi.BeadDetail, 0, darkMaxConcurrentOpenDarkMRs)
	for i := 0; i < darkMaxConcurrentOpenDarkMRs; i++ {
		mrs = append(mrs, darkMRBead(fmt.Sprintf("nlf-%d", i), 1*time.Minute))
	}
	d := &noListerDaemon{openMRs: mrs}
	trig := darkMRCapGardenerTrigger(t)

	skip, reason := gardenerPreFireCheck(context.Background(), d, nil, trig)
	if !skip || reason != gardenerSkipDarkMRFleetCap {
		t.Fatalf("fleet cap must hold without a lister, got skip=%v reason=%q", skip, reason)
	}
}

// ── non-dark gardeners are completely unaffected ────────────────────────────

func TestDarkMRCaps_NonDarkGardenerUnaffected(t *testing.T) {
	// A non-dark gardener must never reach the MR caps — even with a flood of
	// open dark MRs well over BOTH ceilings present in the daemon.
	mrs := make([]*beadsapi.BeadDetail, 0, darkMaxConcurrentOpenDarkMRs*2)
	producers := map[string][]*beadsapi.BeadDetail{}
	for i := 0; i < darkMaxConcurrentOpenDarkMRs*2; i++ {
		id := fmt.Sprintf("flood-%d", i)
		mrs = append(mrs, darkMRBead(id, 1*time.Minute))
		producers[id] = []*beadsapi.BeadDetail{
			{ID: "gd-darkepic", Fields: map[string]string{"trigger_id": "gd-normalcap"}},
		}
	}
	d := &darkMRCapDaemon{openMRs: mrs, withLister: true, producers: producers}

	spec := GardenerSpec{
		AssignBead: "gd-kseoxlfx1q",
		// Intentionally non-dark: proves the non-dark path skips the dark-only
		// blast-radius caps (the bounded-stop guard is now global and warn-only
		// at firing, so it never blocks here regardless of labels).
	}
	trig := TriggerDef{
		ID:       "gd-normalcap",
		Project:  "gardenio",
		Labels:   []string{"role:gardener", "project:gardenio"},
		Gardener: &spec,
	}

	skip, reason := gardenerPreFireCheck(context.Background(), d, nil, trig)
	if skip && (reason == gardenerSkipDarkMRFleetCap || reason == gardenerSkipDarkMRPerGardenerCap ||
		reason == gardenerSkipDarkAgentBudget) {
		t.Fatalf("non-dark gardener wrongly blocked by a dark cap: reason=%q", reason)
	}
}

// ── direct evaluateDarkMRCaps unit (no pre-fire wrapper) ───────────────────

func TestEvaluateDarkMRCaps_DirectFleet(t *testing.T) {
	mrs := make([]*beadsapi.BeadDetail, 0, darkMaxConcurrentOpenDarkMRs)
	for i := 0; i < darkMaxConcurrentOpenDarkMRs; i++ {
		mrs = append(mrs, darkMRBead(fmt.Sprintf("d-%d", i), 48*time.Hour))
	}
	d := &darkMRCapDaemon{openMRs: mrs, withLister: true, producers: map[string][]*beadsapi.BeadDetail{}}
	dec := evaluateDarkMRCaps(context.Background(), d, nil, darkMRCapGardenerTrigger(t))
	if !dec.Skip || dec.Reason != gardenerSkipDarkMRFleetCap {
		t.Fatalf("direct fleet cap: got %+v", dec)
	}
}

// pouring-stall dependency-graph stubs (ka-0000000957) — this mock does not
// exercise the sweep.
func (d *darkMRCapDaemon) GetReverseDependencies(_ context.Context, _ string) ([]beadsapi.Dependency, error) {
	return nil, nil
}
func (d *darkMRCapDaemon) BatchGetDependencies(_ context.Context, _ []string, _ string) (map[string][]beadsapi.Dependency, error) {
	return nil, nil
}
func (d *darkMRCapDaemon) BatchGetBeads(_ context.Context, _ []string) (map[string]*beadsapi.BeadDetail, error) {
	return nil, nil
}

// pouring-stall dependency-graph stubs (ka-0000000957) — this mock does not
// exercise the sweep.
func (d *noListerDaemon) GetReverseDependencies(_ context.Context, _ string) ([]beadsapi.Dependency, error) {
	return nil, nil
}
func (d *noListerDaemon) BatchGetDependencies(_ context.Context, _ []string, _ string) (map[string][]beadsapi.Dependency, error) {
	return nil, nil
}
func (d *noListerDaemon) BatchGetBeads(_ context.Context, _ []string) (map[string]*beadsapi.BeadDetail, error) {
	return nil, nil
}
