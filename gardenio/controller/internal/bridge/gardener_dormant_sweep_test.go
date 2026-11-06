package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// dormantSweepNow is the fixed clock the dormant-gardener sweep tests run at.
var dormantSweepNow = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

// dormantSweepEngine wires a TriggerEngine with the dormant-gardener sweep on,
// a recording notifier and a mutable clock, over the given daemon.
func dormantSweepEngine(daemon *mockTriggerDaemon) (*TriggerEngine, *recordingNotifier, *time.Time) {
	rec := &recordingNotifier{}
	now := dormantSweepNow
	clk := &now
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon:                      daemon,
		Notifier:                    rec.notify,
		Logger:                      slog.Default(),
		DormantGardenerSweepEnabled: true,
		DormantGardenerSweepNotify:  true,
		Clock:                       func() time.Time { return *clk },
	})
	return engine, rec, clk
}

// dormantGardener builds an open, enabled gardener trigger on gd-epic-1,
// parked behind gd-esc-1 since the given instant (zero leaves DormantSince
// unset, the shape of a gardener parked before the field existed).
func dormantGardener(t *testing.T, id string, since time.Time) *beadsapi.BeadDetail {
	t.Helper()
	bead := dormancyProbeTrigger(t, GardenerSpec{
		AssignBead:          "gd-epic-1",
		DormantOnEscalation: true,
		DormantBlocker:      "gd-esc-1",
		DormantSince:        since,
	})
	bead.ID = id
	bead.Fields["enabled"] = "true"
	return bead
}

// actionableFrontierDaemon is wedgedFrontierDaemon plus one un-blocked open
// child, so the frontier has one actionable child and the probe reads NOT
// parked: the shape of a gardener that should have woken.
func actionableFrontierDaemon() *mockTriggerDaemon {
	daemon := wedgedFrontierDaemon("gd-esc-1")
	daemon.beads = append(daemon.beads,
		&beadsapi.BeadDetail{ID: "gd-child-2", Type: "task", Status: "open", Title: "actionable", Labels: []string{"project:monorepo"}},
	)
	daemon.parentOf["gd-child-2"] = "gd-epic-1"
	return daemon
}

// escalationsCreated returns the escalation create requests the mock saw.
func escalationsCreated(m *mockTriggerDaemon) []beadsapi.CreateBeadRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []beadsapi.CreateBeadRequest
	for _, r := range m.createdBeads {
		if r.Type == "escalation" {
			out = append(out, r)
		}
	}
	return out
}

// A gardener dormant for two days whose plan has an actionable child is named
// in exactly one operator escalation, attributed to the sweep's system operator.
func TestSweepDormantGardeners_FilesOneEscalationForStuckGardener(t *testing.T) {
	daemon := actionableFrontierDaemon()
	daemon.beads = append(daemon.beads, dormantGardener(t, "gd-gard-1", dormantSweepNow.Add(-48*time.Hour)))
	engine, rec, _ := dormantSweepEngine(daemon)

	engine.SweepDormantGardeners(context.Background())

	escs := escalationsCreated(daemon)
	if len(escs) != 1 {
		t.Fatalf("want 1 escalation filed, got %d", len(escs))
	}
	esc := escs[0]
	if !slices.Contains(esc.Labels, dormantGardenerSweepMarkerLabel) {
		t.Errorf("escalation must carry the dedup marker label, got %v", esc.Labels)
	}
	if !slices.Contains(esc.Labels, "project:monorepo") {
		t.Errorf("escalation must carry the gardener's project label, got %v", esc.Labels)
	}
	if esc.ShortTitle == "" || len([]rune(esc.ShortTitle)) > 40 {
		t.Errorf("short title must be set and at most 40 runes, got %q", esc.ShortTitle)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(esc.Fields, &fields); err != nil {
		t.Fatalf("unmarshal fields: %v", err)
	}
	var opType, audience string
	_ = json.Unmarshal(fields["operation_type"], &opType)
	_ = json.Unmarshal(fields["audience"], &audience)
	if opType != "manual_operator_action" || audience != "operator" {
		t.Errorf("want operation_type=manual_operator_action audience=operator, got %q %q", opType, audience)
	}
	got := dormantGardenerEscalationIDs(&beadsapi.BeadDetail{Fields: map[string]string{"operation_spec": string(fields["operation_spec"])}})
	if !slices.Equal(got, []string{"gd-gard-1"}) {
		t.Errorf("operation_spec.gardeners: want [gd-gard-1], got %v", got)
	}
	if !strings.Contains(esc.Description, "gd-gard-1") || !strings.Contains(esc.Description, "gd-epic-1") {
		t.Errorf("description must name the gardener and its plan, got:\n%s", esc.Description)
	}
	daemon.mu.Lock()
	sysOp := daemon.createSystemOperators[len(daemon.createSystemOperators)-1]
	daemon.mu.Unlock()
	if sysOp != dormantGardenerSweepSystemOperator {
		t.Errorf("create must carry X-System-Operator %q, got %q", dormantGardenerSweepSystemOperator, sysOp)
	}
	if rec.count() != 1 {
		t.Errorf("want 1 Slack notice, got %d", rec.count())
	}
}

// A gardener parked before DormantSince existed falls back to last_run.
func TestSweepDormantGardeners_LegacyParkUsesLastRun(t *testing.T) {
	daemon := actionableFrontierDaemon()
	g := dormantGardener(t, "gd-gard-1", time.Time{})
	g.Fields["last_run"] = dormantSweepNow.Add(-72 * time.Hour).Format(time.RFC3339)
	daemon.beads = append(daemon.beads, g)
	engine, _, _ := dormantSweepEngine(daemon)

	engine.SweepDormantGardeners(context.Background())

	if n := len(escalationsCreated(daemon)); n != 1 {
		t.Fatalf("legacy dormant gardener with last_run 3 days ago: want 1 escalation, got %d", n)
	}
}

// The sweep stays quiet for a gardener dormant under a day, one still parked
// on a fully wedged frontier, and a disabled one.
func TestSweepDormantGardeners_NoHit(t *testing.T) {
	cases := []struct {
		name   string
		daemon func() *mockTriggerDaemon
		bead   func(t *testing.T) *beadsapi.BeadDetail
	}{
		{
			name:   "dormant under 24h",
			daemon: actionableFrontierDaemon,
			bead: func(t *testing.T) *beadsapi.BeadDetail {
				return dormantGardener(t, "gd-gard-1", dormantSweepNow.Add(-6*time.Hour))
			},
		},
		{
			name:   "still parked: zero actionable children",
			daemon: func() *mockTriggerDaemon { return wedgedFrontierDaemon("gd-esc-1") },
			bead: func(t *testing.T) *beadsapi.BeadDetail {
				return dormantGardener(t, "gd-gard-1", dormantSweepNow.Add(-72*time.Hour))
			},
		},
		{
			name:   "disabled gardener",
			daemon: actionableFrontierDaemon,
			bead: func(t *testing.T) *beadsapi.BeadDetail {
				b := dormantGardener(t, "gd-gard-1", dormantSweepNow.Add(-72*time.Hour))
				b.Fields["enabled"] = "false"
				return b
			},
		},
		{
			name:   "not dormant",
			daemon: actionableFrontierDaemon,
			bead: func(t *testing.T) *beadsapi.BeadDetail {
				return dormancyProbeTrigger(t, GardenerSpec{AssignBead: "gd-epic-1"})
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			daemon := tc.daemon()
			daemon.beads = append(daemon.beads, tc.bead(t))
			engine, rec, _ := dormantSweepEngine(daemon)

			engine.SweepDormantGardeners(context.Background())

			if n := len(escalationsCreated(daemon)); n != 0 {
				t.Errorf("want no escalation, got %d", n)
			}
			if rec.count() != 0 {
				t.Errorf("want no Slack notice, got %d", rec.count())
			}
		})
	}
}

// A gardener parked over a day on a quiet plan after spending its resume
// budget is parked by design, so the sweep does not name it (gd-dNxyjFLGRJ).
func TestSweepDormantGardeners_QuietResumeSpentPark_NoHit(t *testing.T) {
	daemon := actionableFrontierDaemon()
	bead := resumeSpentProbeTrigger(t, daemon)
	bead.ID = "gd-gard-1"
	bead.Fields["enabled"] = "true"
	raw := bead.Fields["gardener"]
	var spec GardenerSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		t.Fatal(err)
	}
	spec.DormantSince = dormantSweepNow.Add(-72 * time.Hour)
	b, _ := json.Marshal(spec)
	bead.Fields["gardener"] = string(b)
	daemon.beads = append(daemon.beads, bead)
	engine, _, _ := dormantSweepEngine(daemon)

	engine.SweepDormantGardeners(context.Background())

	if n := len(escalationsCreated(daemon)); n != 0 {
		t.Errorf("quiet resume-spent park: want no escalation, got %d", n)
	}
}

// While an earlier sweep escalation is open, the sweep files no second one: it
// stays silent when the named set is unchanged and comments when it changed.
func TestSweepDormantGardeners_DedupsOnOpenEscalation(t *testing.T) {
	for _, tc := range []struct {
		name        string
		named       []string
		wantComment bool
	}{
		{name: "same gardeners", named: []string{"gd-gard-1"}, wantComment: false},
		{name: "different gardeners", named: []string{"gd-gard-old"}, wantComment: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			daemon := actionableFrontierDaemon()
			spec, _ := json.Marshal(map[string]any{"gardeners": tc.named})
			daemon.beads = append(daemon.beads,
				dormantGardener(t, "gd-gard-1", dormantSweepNow.Add(-48*time.Hour)),
				&beadsapi.BeadDetail{
					ID: "gd-esc-sweep", Type: "escalation", Status: "open",
					Labels: []string{dormantGardenerSweepMarkerLabel},
					Fields: map[string]string{"operation_spec": string(spec)},
				},
			)
			engine, rec, _ := dormantSweepEngine(daemon)

			engine.SweepDormantGardeners(context.Background())

			if n := len(escalationsCreated(daemon)); n != 0 {
				t.Fatalf("an open sweep escalation exists: want no new one, got %d", n)
			}
			daemon.mu.Lock()
			commented := slices.Contains(daemon.commentedIDs, "gd-esc-sweep")
			daemon.mu.Unlock()
			if commented != tc.wantComment {
				t.Errorf("comment on open escalation: want %v, got %v", tc.wantComment, commented)
			}
			if want := map[bool]int{true: 1, false: 0}[tc.wantComment]; rec.count() != want {
				t.Errorf("Slack notices: want %d, got %d", want, rec.count())
			}
		})
	}
}

// The sweep runs once a day: a second pass within 24h does nothing, and the
// pass after 24h runs again.
func TestSweepDormantGardeners_DailyThrottle(t *testing.T) {
	daemon := actionableFrontierDaemon()
	daemon.beads = append(daemon.beads, dormantGardener(t, "gd-gard-1", dormantSweepNow.Add(-48*time.Hour)))
	engine, _, clk := dormantSweepEngine(daemon)

	engine.SweepDormantGardeners(context.Background())
	*clk = clk.Add(time.Hour)
	engine.SweepDormantGardeners(context.Background())
	if n := len(escalationsCreated(daemon)); n != 1 {
		t.Fatalf("second pass inside the day must be throttled: want 1 escalation, got %d", n)
	}

	*clk = clk.Add(24 * time.Hour)
	engine.SweepDormantGardeners(context.Background())
	if n := len(escalationsCreated(daemon)); n != 2 {
		// The mock does not list created beads back, so the dedup read finds
		// nothing and a second create proves the sweep ran.
		t.Fatalf("pass after 24h must run: want 2 creates, got %d", n)
	}
}

// Off by default: an engine without the flag never files.
func TestSweepDormantGardeners_DisabledIsNoop(t *testing.T) {
	daemon := actionableFrontierDaemon()
	daemon.beads = append(daemon.beads, dormantGardener(t, "gd-gard-1", dormantSweepNow.Add(-48*time.Hour)))
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})

	engine.SweepDormantGardeners(context.Background())

	if n := len(escalationsCreated(daemon)); n != 0 {
		t.Fatalf("disabled sweep must not file, got %d", n)
	}
}

// Parking stamps DormantSince into the gardener blob and waking clears it.
func TestGardenerParkDormant_StampsAndWakeClearsDormantSince(t *testing.T) {
	daemon := wedgedFrontierDaemon("gd-esc-1")
	bead := dormancyProbeTrigger(t, GardenerSpec{AssignBead: "gd-epic-1"})
	trig := ParseTriggerDef(bead, "schedule")

	before := time.Now()
	gardenerParkDormant(context.Background(), daemon, slog.Default(), trig, gardenerDormantKindOperatorWedge, "gd-esc-1", gardenerDormantKindOperatorWedge, "parked")
	if trig.Gardener.DormantSince.Before(before) {
		t.Fatalf("park must stamp DormantSince to now, got %v", trig.Gardener.DormantSince)
	}
	if blob := lastGardenerBlob(t, daemon); blob.DormantSince.IsZero() {
		t.Error("park must persist dormant_since in the gardener blob")
	}

	gardenerWakeFromDormant(context.Background(), daemon, slog.Default(), trig)
	if !trig.Gardener.DormantSince.IsZero() {
		t.Errorf("wake must clear DormantSince, got %v", trig.Gardener.DormantSince)
	}
	if blob := lastGardenerBlob(t, daemon); !blob.DormantSince.IsZero() {
		t.Errorf("wake must persist a cleared dormant_since, got %v", blob.DormantSince)
	}
}

// lastGardenerBlob decodes the most recent gardener blob written to the mock.
func lastGardenerBlob(t *testing.T, m *mockTriggerDaemon) GardenerSpec {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.updatedFields) - 1; i >= 0; i-- {
		if raw, ok := m.updatedFields[i]["gardener"]; ok {
			var g GardenerSpec
			if err := json.Unmarshal([]byte(raw), &g); err != nil {
				t.Fatalf("unmarshal gardener blob: %v", err)
			}
			return g
		}
	}
	t.Fatal("no gardener blob written")
	return GardenerSpec{}
}
