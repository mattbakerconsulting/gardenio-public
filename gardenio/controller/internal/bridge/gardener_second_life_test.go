package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// secondLifeTrigger builds a CLOSED gardener trigger bead carrying the supplied
// gardener spec, plus the mock daemon serving it. The bead starts closed-on-
// max_runs (the "disappointed gardener" cohort gardenerGrantSecondLife revives).
func secondLifeTrigger(cap GardenerSpec) (*mockTriggerDaemon, TriggerDef) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{
				ID:     "gd-cap-1",
				Type:   "trigger",
				Status: "closed",
				Title:  "gardener trig",
				Fields: map[string]string{
					"gardener_terminated_slot": "loop_end",
					"gardener_terminated_kind": "max_runs",
				},
			},
		},
	}
	trig := TriggerDef{
		ID:       "gd-cap-1",
		Title:    "gardener trig",
		Project:  "monorepo",
		Role:     "gardener",
		Gardener: &cap,
	}
	return daemon, trig
}

// TestGardenerSecondLife_Grant_ReopensSameBead is the happy path: a gardener
// closed on max_runs with second-life budget remaining is REVIVED — the SAME
// bead is reopened (status→open), RunsSpawned reset to 0, RunBudget bumped, and
// second_life_count incremented. Audit-trail continuity: no new bead.
func TestGardenerSecondLife_Grant_ReopensSameBead(t *testing.T) {
	daemon, trig := secondLifeTrigger(GardenerSpec{
		RunBudget:       20,
		RunsSpawned:     20, // budget exhausted → why it closed
		MaxSecondLives:  5,
		SecondLifeCount: 1, // one life already granted; 1 < 5 → grant
	})

	granted := gardenerGrantSecondLife(context.Background(), daemon, slog.Default(), trig, "trending toward goal")
	if !granted {
		t.Fatalf("expected second life to be granted (count 1 < max 5)")
	}

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	// Reopen: an UpdateBead with Status=open must have been issued on the bead.
	reopened := false
	for i, id := range daemon.updatedBeadIDs {
		if id == "gd-cap-1" && daemon.updatedBeadReqs[i].Status != nil && *daemon.updatedBeadReqs[i].Status == "open" {
			reopened = true
		}
	}
	if !reopened {
		t.Errorf("expected gardener bead to be reopened (UpdateBead status=open), got reqs=%v", daemon.updatedBeadReqs)
	}

	// The gardener must NOT have been closed again on this path.
	if slices.Contains(daemon.closedIDs, "gd-cap-1") {
		t.Errorf("grant must reopen, not close; got closedIDs=%v", daemon.closedIDs)
	}

	// The persisted gardener blob must carry the reset/bumped counters.
	var blob *GardenerSpec
	for i, id := range daemon.updatedIDs {
		if id != "gd-cap-1" {
			continue
		}
		if raw, ok := daemon.updatedFields[i]["gardener"]; ok {
			var cs GardenerSpec
			if err := json.Unmarshal([]byte(raw), &cs); err != nil {
				t.Fatalf("gardener blob did not round-trip: %v", err)
			}
			blob = &cs
		}
	}
	if blob == nil {
		t.Fatalf("expected a gardener-blob field write, got updatedFields=%v", daemon.updatedFields)
	}
	if blob.SecondLifeCount != 2 {
		t.Errorf("second_life_count: got %d, want 2 (incremented from 1)", blob.SecondLifeCount)
	}
	if blob.RunsSpawned != 0 {
		t.Errorf("runs_spawned should reset to 0 on revive, got %d", blob.RunsSpawned)
	}
	if blob.RunBudget != defaultRunBudgetRefill {
		t.Errorf("run_budget should bump to refill floor %d, got %d", defaultRunBudgetRefill, blob.RunBudget)
	}

	// In-place mutation so later gates this cycle see the revived state.
	if trig.Gardener.SecondLifeCount != 2 || trig.Gardener.RunsSpawned != 0 {
		t.Errorf("trig.Gardener not mutated in place: count=%d runs=%d",
			trig.Gardener.SecondLifeCount, trig.Gardener.RunsSpawned)
	}
}

// TestGardenerSecondLife_Grant_ReArmsEnabled proves the revival counterpart to
// the ka-0000001423 disarm-on-retire fix: because terminal close now sets
// enabled=false, a second-life grant that reopens the SAME closed bead MUST flip
// enabled back to true — otherwise LoadTriggers (which drops enabled=false)
// filters the revived gardener out and it never fires again, a silent revival
// that reopens the bead but leaves it dark. The re-arm must be a real JSON bool
// (enabled is boolean-typed, ka-0000003627).
func TestGardenerSecondLife_Grant_ReArmsEnabled(t *testing.T) {
	daemon, trig := secondLifeTrigger(GardenerSpec{
		RunBudget:       20,
		RunsSpawned:     20,
		MaxSecondLives:  5,
		SecondLifeCount: 1,
	})
	// The closed gardener was disarmed at retirement.
	daemon.beads[0].Fields["enabled"] = "false"

	if !gardenerGrantSecondLife(context.Background(), daemon, slog.Default(), trig, "trending toward goal") {
		t.Fatalf("expected second life to be granted (count 1 < max 5)")
	}

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	rearmed := false
	for i, id := range daemon.updatedIDs {
		if id != "gd-cap-1" || i >= len(daemon.updatedTyped) {
			continue
		}
		if v, ok := daemon.updatedTyped[i]["enabled"]; ok {
			b, isBool := v.(bool)
			if !isBool {
				t.Errorf("enabled must be re-armed as a JSON bool, got %T (%v)", v, v)
			}
			if isBool && b {
				rearmed = true
			}
		}
	}
	if !rearmed {
		t.Errorf("expected second-life grant to re-arm the revived gardener (enabled=true via UpdateBeadFieldsTyped), typed=%v", daemon.updatedTyped)
	}
}

// TestGardenerSecondLife_Grant_StampsWallClockSince proves the second-life grant
// stamps the per-bead wall_clock baseline (WallClockSince) to now (bug
// ka-0000006465). A second life reopens the SAME bead, so without this the
// wall_clock baseline would stay pinned at max(created_at, epoch) and a gardener
// old enough to trip hard_stop wall_clock would re-close on the next reconcile,
// silently undoing the grant.
func TestGardenerSecondLife_Grant_StampsWallClockSince(t *testing.T) {
	daemon, trig := secondLifeTrigger(GardenerSpec{
		RunBudget:       20,
		RunsSpawned:     20,
		MaxSecondLives:  5,
		SecondLifeCount: 1,
		Stop:            json.RawMessage(`{"hard_stop":{"any":[{"kind":"wall_clock","duration":"168h"}]}}`),
	})

	before := time.Now()
	if !gardenerGrantSecondLife(context.Background(), daemon, slog.Default(), trig, "trending toward goal") {
		t.Fatalf("expected second life to be granted (count 1 < max 5)")
	}

	// Persisted blob carries the fresh wall_clock baseline.
	blob := gardenerSpecFromUpdate(t, daemon, "gd-cap-1")
	if blob.WallClockSince.Before(before) {
		t.Errorf("persisted WallClockSince not advanced: got %v, want >= %v", blob.WallClockSince, before)
	}
	// In-place mutation so this cycle's pre-fire gate measures the fresh window.
	if trig.Gardener.WallClockSince.Before(before) {
		t.Errorf("in-memory WallClockSince not advanced: got %v, want >= %v",
			trig.Gardener.WallClockSince, before)
	}
}

// TestGardenerSecondLife_Exhausted_Escalates is the don't-resurrect-the-doomed
// brake: once second_life_count >= max_second_lives the grant does NOT reopen —
// it leaves the bead closed, flags gardener-second-life-exhausted, and comments
// for the operator decision.
func TestGardenerSecondLife_Exhausted_Escalates(t *testing.T) {
	daemon, trig := secondLifeTrigger(GardenerSpec{
		RunBudget:       20,
		RunsSpawned:     20,
		MaxSecondLives:  5,
		SecondLifeCount: 5, // budget fully spent → escalate
	})

	granted := gardenerGrantSecondLife(context.Background(), daemon, slog.Default(), trig, "operator review")
	if granted {
		t.Fatalf("expected second life to be DENIED once budget is spent (count 5 >= max 5)")
	}

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	// Must NOT reopen the bead.
	for i, id := range daemon.updatedBeadIDs {
		if id == "gd-cap-1" && daemon.updatedBeadReqs[i].Status != nil && *daemon.updatedBeadReqs[i].Status == "open" {
			t.Errorf("exhausted budget must not reopen the bead, got reqs=%v", daemon.updatedBeadReqs)
		}
	}

	// Must flag the exhausted label so the watchdog/operator picks it up — via
	// AddLabel (a REAL label), not a fields-blob write the type:trigger schema
	// rejects (ka-0000007275). daemon.mu is already held in this test.
	flagged := false
	for _, l := range daemon.addedLabels {
		if l.BeadID == "gd-cap-1" && l.Label == gardenerSecondLifeExhaustedLabel {
			flagged = true
		}
	}
	if !flagged {
		t.Errorf("expected %q label to be stamped on escalate via AddLabel, got labels=%v",
			gardenerSecondLifeExhaustedLabel, daemon.addedLabels)
	}
	for i, id := range daemon.updatedIDs {
		if id == "gd-cap-1" {
			if _, ok := daemon.updatedFields[i]["labels"]; ok {
				t.Errorf("labels must not go through the fields PATCH (ka-0000007275); got fields=%v", daemon.updatedFields[i])
			}
		}
	}

	// Must comment for the operator.
	if !slices.Contains(daemon.commentedIDs, "gd-cap-1") {
		t.Errorf("expected an escalation comment on the gardener, got %v", daemon.commentedIDs)
	}
}

// TestGardenerSecondLife_Disabled_Escalates verifies the explicit opt-out: a
// negative max_second_lives disables second-life entirely — a closed gardener
// stays closed and the grant escalates immediately, regardless of count.
func TestGardenerSecondLife_Disabled_Escalates(t *testing.T) {
	daemon, trig := secondLifeTrigger(GardenerSpec{
		MaxSecondLives:  -1, // disabled
		SecondLifeCount: 0,
	})

	if gardenerGrantSecondLife(context.Background(), daemon, slog.Default(), trig, "x") {
		t.Fatalf("expected second life disabled (max_second_lives < 0) to deny the grant")
	}

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	for _, id := range daemon.updatedBeadIDs {
		if id == "gd-cap-1" {
			// Only acceptable UpdateBead here would be a non-status patch; we
			// issue none, so any record is a bug.
			t.Errorf("disabled second-life must not touch bead status, got reqs=%v", daemon.updatedBeadReqs)
		}
	}
}

// TestGardenerSecondLife_DefaultBudget verifies the default cap (5) applies when
// max_second_lives is unset (zero): a gardener at count 4 is still granted (4 <
// default 5), and the increment lands at 5.
func TestGardenerSecondLife_DefaultBudget(t *testing.T) {
	daemon, trig := secondLifeTrigger(GardenerSpec{
		RunBudget:       10,
		RunsSpawned:     10,
		MaxSecondLives:  0, // unset → defaultMaxSecondLives (5)
		SecondLifeCount: 4,
	})

	if !gardenerGrantSecondLife(context.Background(), daemon, slog.Default(), trig, "default-budget") {
		t.Fatalf("expected grant at count 4 < default cap 5")
	}
	if trig.Gardener.SecondLifeCount != 5 {
		t.Errorf("second_life_count: got %d, want 5", trig.Gardener.SecondLifeCount)
	}

	// And the very next grant (now count 5 == default 5) must escalate.
	if gardenerGrantSecondLife(context.Background(), daemon, slog.Default(), trig, "now-exhausted") {
		t.Errorf("expected escalate once count reaches the default cap (5 >= 5)")
	}
}

// fieldsFailDaemon embeds the shared mock but fails UpdateBeadFields, so the
// grant path can reopen the bead (UpdateBead) but then fail to persist the
// reset counters — exercising the divergence guard.
type fieldsFailDaemon struct {
	*mockTriggerDaemon
}

func (d *fieldsFailDaemon) UpdateBeadFields(_ context.Context, _ string, _ map[string]string) error {
	return errFieldsWrite
}

var errFieldsWrite = stubErr("simulated UpdateBeadFields failure")

type stubErr string

func (e stubErr) Error() string { return string(e) }

// TestGardenerSecondLife_PersistFailureDoesNotCountGrant verifies the divergence
// guard: if the field/counter persist fails AFTER the reopen, the grant is NOT
// counted (returns false) and the in-memory gardener is NOT mutated — so the
// metric and the cap never observe a life the persisted blob didn't record. The
// bead is left open for the run_budget gate to re-adjudicate.
func TestGardenerSecondLife_PersistFailureDoesNotCountGrant(t *testing.T) {
	base, trig := secondLifeTrigger(GardenerSpec{
		RunBudget:       20,
		RunsSpawned:     20,
		MaxSecondLives:  5,
		SecondLifeCount: 1,
	})
	daemon := &fieldsFailDaemon{mockTriggerDaemon: base}

	granted := gardenerGrantSecondLife(context.Background(), daemon, slog.Default(), trig, "persist-fails")
	if granted {
		t.Fatalf("a failed counter-persist must not count as a granted second life")
	}

	// The reopen itself was still attempted (load-bearing, succeeded).
	base.mu.Lock()
	defer base.mu.Unlock()
	reopened := false
	for i, id := range base.updatedBeadIDs {
		if id == "gd-cap-1" && base.updatedBeadReqs[i].Status != nil && *base.updatedBeadReqs[i].Status == "open" {
			reopened = true
		}
	}
	if !reopened {
		t.Errorf("expected the reopen to have been attempted before the field write failed")
	}

	// In-memory gardener must NOT have advanced — persisted state and observed
	// state stay in lockstep.
	if trig.Gardener.SecondLifeCount != 1 {
		t.Errorf("second_life_count must not advance on persist failure: got %d, want 1", trig.Gardener.SecondLifeCount)
	}
	if trig.Gardener.RunsSpawned != 20 {
		t.Errorf("runs_spawned must not reset on persist failure: got %d, want 20", trig.Gardener.RunsSpawned)
	}
}
