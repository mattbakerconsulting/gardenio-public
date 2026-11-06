package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/decisionview"
)

// fakeGrafana is an in-memory GrafanaReader: uid → (model, version).
type fakeGrafana struct {
	models   map[string][]byte
	versions map[string]int
}

func newFakeGrafana() *fakeGrafana {
	return &fakeGrafana{models: map[string][]byte{}, versions: map[string]int{}}
}

func (g *fakeGrafana) set(uid string, model []byte, version int) {
	g.models[uid] = model
	g.versions[uid] = version
}

func (g *fakeGrafana) Search(_ context.Context) ([]GrafanaDashboardRef, error) {
	var refs []GrafanaDashboardRef
	for uid := range g.models {
		refs = append(refs, GrafanaDashboardRef{UID: uid, Type: "dash-db"})
	}
	return refs, nil
}

func (g *fakeGrafana) DashboardByUID(_ context.Context, uid string) ([]byte, int, error) {
	m, ok := g.models[uid]
	if !ok {
		return nil, 0, nil // 404
	}
	return m, g.versions[uid], nil
}

// seedSyncedDashboard creates a dashboard bead whose model doc is in sync with
// the recorded dashboard_hash (i.e. source=beads, no pending bead edit).
func seedSyncedDashboard(t *testing.T, daemon *fakeDashboardDaemon, store *DashboardStore, uid string, model []byte) string {
	t.Helper()
	ctx := context.Background()
	id, _ := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Type:   "dashboard",
		Title:  uid,
		Fields: json.RawMessage(`{"dashboard_uid":"` + uid + `","dashboard_source":"beads"}`),
	})
	if _, err := store.PutModel(ctx, id, model); err != nil {
		t.Fatalf("put model: %v", err)
	}
	hash, _ := HashDashboardModel(model)
	if err := daemon.UpdateBeadFields(ctx, id, map[string]string{"dashboard_hash": hash}); err != nil {
		t.Fatalf("stamp hash: %v", err)
	}
	return id
}

func TestReadback_LoopBreaker_NoOp(t *testing.T) {
	daemon := newFakeDashboardDaemon()
	store := NewDashboardStore(daemon, "test")
	grafana := newFakeGrafana()
	model := []byte(`{"uid":"d1","title":"D","version":1}`)
	id := seedSyncedDashboard(t, daemon, store, "d1", model)
	// Grafana holds the SAME model (just a version bump, which is volatile).
	grafana.set("d1", []byte(`{"uid":"d1","title":"D","version":2}`), 2)

	rb := NewGrafanaReadback(GrafanaReadbackConfig{Store: store, Grafana: grafana, Logger: slog.Default()})
	rb.PollOnce(context.Background())

	b, _ := daemon.GetBead(context.Background(), id)
	if b.Fields["dashboard_source"] != "beads" {
		t.Fatalf("loop-breaker should leave source=beads, got %q", b.Fields["dashboard_source"])
	}
}

func TestReadback_PlainGrafanaEdit_PulledIn(t *testing.T) {
	daemon := newFakeDashboardDaemon()
	store := NewDashboardStore(daemon, "test")
	grafana := newFakeGrafana()
	model := []byte(`{"uid":"d2","title":"D","version":1}`)
	id := seedSyncedDashboard(t, daemon, store, "d2", model)
	// Grafana has a genuinely different model (new panel) → a Grafana-side edit.
	edited := []byte(`{"uid":"d2","title":"D-edited","version":5,"panels":[{"id":1}]}`)
	grafana.set("d2", edited, 5)

	rb := NewGrafanaReadback(GrafanaReadbackConfig{Store: store, Grafana: grafana, Logger: slog.Default()})
	rb.PollOnce(context.Background())

	b, _ := daemon.GetBead(context.Background(), id)
	if b.Fields["dashboard_source"] != "grafana" {
		t.Fatalf("expected source=grafana after pull, got %q", b.Fields["dashboard_source"])
	}
	wantHash, _ := HashDashboardModel(edited)
	if b.Fields["dashboard_hash"] != wantHash {
		t.Fatalf("hash not updated to grafana model")
	}
	if b.Fields["dashboard_version"] != "5" {
		t.Fatalf("expected version 5, got %q", b.Fields["dashboard_version"])
	}
	// The bead's model doc now holds the Grafana model.
	_, got, _ := store.ModelDoc(context.Background(), id)
	gotMeta, _ := ParseDashboardMeta(got)
	if gotMeta.Title != "D-edited" {
		t.Fatalf("model doc not updated, title=%q", gotMeta.Title)
	}
}

func TestReadback_NoBeadForUID_Skipped(t *testing.T) {
	daemon := newFakeDashboardDaemon()
	store := NewDashboardStore(daemon, "test")
	grafana := newFakeGrafana()
	grafana.set("orphan", []byte(`{"uid":"orphan","title":"O"}`), 1)
	rb := NewGrafanaReadback(GrafanaReadbackConfig{Store: store, Grafana: grafana, Logger: slog.Default()})
	rb.PollOnce(context.Background())
	// Nothing created beyond the absence — no panic, no bead.
	if len(daemon.beads) != 0 {
		t.Fatalf("expected no beads created for orphan grafana dashboard, got %d", len(daemon.beads))
	}
}

func TestReadback_Deleted404_LeavesBead(t *testing.T) {
	daemon := newFakeDashboardDaemon()
	store := NewDashboardStore(daemon, "test")
	grafana := newFakeGrafana()
	model := []byte(`{"uid":"d3","title":"D","version":1}`)
	id := seedSyncedDashboard(t, daemon, store, "d3", model)
	// Grafana Search still lists the uid (map key present) but DashboardByUID
	// returns 404 (nil model) — a mid-delete race.
	grafana.models["d3"] = nil
	rb := NewGrafanaReadback(GrafanaReadbackConfig{Store: store, Grafana: grafana, Logger: slog.Default()})
	rb.PollOnce(context.Background())
	if _, err := daemon.GetBead(context.Background(), id); err != nil {
		t.Fatalf("bead should still exist after grafana 404: %v", err)
	}
}

// makeConflict sets up a dashboard where BOTH the bead model and the Grafana
// model differ from the recorded dashboard_hash (a true conflict).
func makeConflict(t *testing.T, daemon *fakeDashboardDaemon, store *DashboardStore, grafana *fakeGrafana, uid string) string {
	t.Helper()
	ctx := context.Background()
	base := []byte(`{"uid":"` + uid + `","title":"base","version":1}`)
	id := seedSyncedDashboard(t, daemon, store, uid, base)
	// Mutate the bead's model doc WITHOUT updating dashboard_hash → pending
	// beads-side edit.
	beadEdit := []byte(`{"uid":"` + uid + `","title":"beads-edit","version":1}`)
	if _, err := store.PutModel(ctx, id, beadEdit); err != nil {
		t.Fatalf("bead edit: %v", err)
	}
	// Grafana also diverged.
	grafanaEdit := []byte(`{"uid":"` + uid + `","title":"grafana-edit","version":9}`)
	grafana.set(uid, grafanaEdit, 9)
	return id
}

func TestReadback_Conflict_GrafanaWins(t *testing.T) {
	daemon := newFakeDashboardDaemon()
	store := NewDashboardStore(daemon, "test")
	grafana := newFakeGrafana()
	id := makeConflict(t, daemon, store, grafana, "c1")

	rb := NewGrafanaReadback(GrafanaReadbackConfig{Store: store, Grafana: grafana, Policy: ConflictGrafanaWins, Logger: slog.Default()})
	rb.PollOnce(context.Background())

	b, _ := daemon.GetBead(context.Background(), id)
	if b.Fields["dashboard_source"] != "grafana" {
		t.Fatalf("grafana-wins: expected source=grafana, got %q", b.Fields["dashboard_source"])
	}
	_, got, _ := store.ModelDoc(context.Background(), id)
	gotMeta, _ := ParseDashboardMeta(got)
	if gotMeta.Title != "grafana-edit" {
		t.Fatalf("grafana-wins: model doc should be grafana model, title=%q", gotMeta.Title)
	}
	// The dropped beads model is archived as a conflict doc.
	if !hasConflictArchive(daemon) {
		t.Fatal("grafana-wins: expected an archived conflict doc")
	}
	if len(daemon.comments[id]) == 0 || !strings.Contains(daemon.comments[id][0], "grafana-wins") {
		t.Fatalf("grafana-wins: expected provenance comment, got %v", daemon.comments[id])
	}
}

func TestReadback_Conflict_BeadsWins(t *testing.T) {
	daemon := newFakeDashboardDaemon()
	store := NewDashboardStore(daemon, "test")
	grafana := newFakeGrafana()
	id := makeConflict(t, daemon, store, grafana, "c2")

	rb := NewGrafanaReadback(GrafanaReadbackConfig{Store: store, Grafana: grafana, Policy: ConflictBeadsWins, Logger: slog.Default()})
	rb.PollOnce(context.Background())

	// beads-wins: the bead model doc is untouched (still beads-edit).
	_, got, _ := store.ModelDoc(context.Background(), id)
	gotMeta, _ := ParseDashboardMeta(got)
	if gotMeta.Title != "beads-edit" {
		t.Fatalf("beads-wins: model doc should remain beads model, title=%q", gotMeta.Title)
	}
	if !hasConflictArchive(daemon) {
		t.Fatal("beads-wins: expected the Grafana model archived as a conflict doc")
	}
	if len(daemon.comments[id]) == 0 || !strings.Contains(daemon.comments[id][0], "beads-wins") {
		t.Fatalf("beads-wins: expected provenance comment, got %v", daemon.comments[id])
	}
}

func TestReadback_Conflict_Manual_OpensDecision(t *testing.T) {
	daemon := newFakeDashboardDaemon()
	store := NewDashboardStore(daemon, "test")
	grafana := newFakeGrafana()
	id := makeConflict(t, daemon, store, grafana, "c3")

	rb := NewGrafanaReadback(GrafanaReadbackConfig{Store: store, Grafana: grafana, Policy: ConflictManual, Logger: slog.Default()})
	rb.PollOnce(context.Background())

	// manual: neither live model changed; a decision bead exists.
	_, got, _ := store.ModelDoc(context.Background(), id)
	gotMeta, _ := ParseDashboardMeta(got)
	if gotMeta.Title != "beads-edit" {
		t.Fatalf("manual: bead model should be untouched, title=%q", gotMeta.Title)
	}
	var decisionFound bool
	for _, b := range daemon.beads {
		if b.Type == "decision" {
			decisionFound = true
			// The background is written where every decision surface reads
			// it: the context field, not only the description (gd-hR3ImbMVhP).
			if got, src := decisionview.Context(b); src != "context" || !strings.Contains(got, "uid=") || !strings.Contains(got, "Grafana wins") {
				t.Errorf("manual: decision context must come from the context field and name the dashboard, got %q from %q", got, src)
			}
		}
	}
	if !decisionFound {
		t.Fatal("manual: expected a decision bead to be opened")
	}
}

// erroringGrafana fails Search, exercising the poll error path.
type erroringGrafana struct{}

func (erroringGrafana) Search(_ context.Context) ([]GrafanaDashboardRef, error) {
	return nil, context.DeadlineExceeded
}
func (erroringGrafana) DashboardByUID(_ context.Context, _ string) ([]byte, int, error) {
	return nil, 0, nil
}

func TestReadback_SearchError_NoOp(t *testing.T) {
	daemon := newFakeDashboardDaemon()
	store := NewDashboardStore(daemon, "test")
	rb := NewGrafanaReadback(GrafanaReadbackConfig{Store: store, Grafana: erroringGrafana{}, Logger: slog.Default()})
	rb.PollOnce(context.Background()) // must not panic; logs + returns
}

func TestReadback_Run_CancelStops(t *testing.T) {
	daemon := newFakeDashboardDaemon()
	store := NewDashboardStore(daemon, "test")
	grafana := newFakeGrafana()
	rb := NewGrafanaReadback(GrafanaReadbackConfig{Store: store, Grafana: grafana, Logger: slog.Default()})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-canceled: Run does one PollOnce then returns ctx.Err()
	if err := rb.Run(ctx); err == nil {
		t.Fatal("expected ctx error from canceled Run")
	}
}

func TestReadback_DedupSuppressesRepeat(t *testing.T) {
	daemon := newFakeDashboardDaemon()
	store := NewDashboardStore(daemon, "test")
	grafana := newFakeGrafana()
	model := []byte(`{"uid":"dd","title":"D","version":1}`)
	id := seedSyncedDashboard(t, daemon, store, "dd", model)
	edited := []byte(`{"uid":"dd","title":"edited","version":2,"panels":[{"id":1}]}`)
	grafana.set("dd", edited, 2)

	rb := NewGrafanaReadback(GrafanaReadbackConfig{Store: store, Grafana: grafana, Logger: slog.Default()})
	rb.PollOnce(context.Background())
	// Revert the bead hash to simulate the stamp not yet observable; the dedup
	// key (bead:hash) should still suppress a second pull within the TTL.
	_ = daemon.UpdateBeadFields(context.Background(), id, map[string]string{"dashboard_hash": "stale"})
	before := len(daemon.comments[id])
	rb.PollOnce(context.Background())
	if len(daemon.comments[id]) != before {
		t.Fatal("dedup should have suppressed a repeat pull within TTL")
	}
}

// hasConflictArchive reports whether any doc bead carries the conflict-archive
// label.
func hasConflictArchive(daemon *fakeDashboardDaemon) bool {
	for _, b := range daemon.beads {
		if b.Type == "doc" && hasLabel(b.Labels, dashboardConflictDocLabel) {
			return true
		}
	}
	return false
}

// fakeOwners is a DashboardOwnerReader: uid → provisioned bead id.
type fakeOwners map[string]string

func (o fakeOwners) ProvisionedBead(_ context.Context, uid string) (string, error) {
	return o[uid], nil
}

// seedProjectDashboard is seedSyncedDashboard for a bead carrying a
// project:<project> label.
func seedProjectDashboard(t *testing.T, daemon *fakeDashboardDaemon, store *DashboardStore, uid, project string, model []byte) string {
	t.Helper()
	id := seedSyncedDashboard(t, daemon, store, uid, model)
	if err := daemon.AddLabel(context.Background(), id, "project:"+project); err != nil {
		t.Fatalf("label: %v", err)
	}
	return id
}

// fenceEdit is a fleet board's Grafana-side model, distinct from the seeded one.
var fenceEdit = []byte(`{"uid":"fleet","title":"Fleet","panels":[{"id":1,"datasource":{"uid":"prom-fleet"}}]}`)

// assertUntouched fails if the read-back wrote the Grafana model into id.
func assertUntouched(t *testing.T, daemon *fakeDashboardDaemon, store *DashboardStore, id string) {
	t.Helper()
	b, _ := daemon.GetBead(context.Background(), id)
	if b.Fields["dashboard_source"] != "beads" {
		t.Fatalf("bead %s: expected source=beads, got %q", id, b.Fields["dashboard_source"])
	}
	_, got, _ := store.ModelDoc(context.Background(), id)
	if strings.Contains(string(got), "prom-fleet") {
		t.Fatalf("bead %s received the fleet model: %s", id, got)
	}
}

// assertPulled fails unless the read-back wrote the Grafana model into id.
func assertPulled(t *testing.T, daemon *fakeDashboardDaemon, store *DashboardStore, id string) {
	t.Helper()
	b, _ := daemon.GetBead(context.Background(), id)
	if b.Fields["dashboard_source"] != "grafana" {
		t.Fatalf("bead %s: expected source=grafana, got %q", id, b.Fields["dashboard_source"])
	}
	_, got, _ := store.ModelDoc(context.Background(), id)
	if !strings.Contains(string(got), "prom-fleet") {
		t.Fatalf("bead %s did not receive the Grafana model: %s", id, got)
	}
}

func TestReadback_Fence_ForeignBeadOnlyGetsNothing(t *testing.T) {
	daemon := newFakeDashboardDaemon()
	store := NewDashboardStore(daemon, "test")
	store.SetProjects([]string{"gardenio"})
	grafana := newFakeGrafana()
	foreign := seedProjectDashboard(t, daemon, store, "fleet", "tenant", []byte(`{"uid":"fleet","title":"Mine"}`))
	unlabelled := seedSyncedDashboard(t, daemon, store, "fleet", []byte(`{"uid":"fleet","title":"Mine"}`))
	grafana.set("fleet", fenceEdit, 4)

	NewGrafanaReadback(GrafanaReadbackConfig{Store: store, Grafana: grafana, Logger: slog.Default()}).PollOnce(context.Background())

	assertUntouched(t, daemon, store, foreign)
	assertUntouched(t, daemon, store, unlabelled)
}

func TestReadback_Fence_FleetBeadWinsOverForeign(t *testing.T) {
	daemon := newFakeDashboardDaemon()
	store := NewDashboardStore(daemon, "test")
	store.SetProjects([]string{"gardenio"})
	grafana := newFakeGrafana()
	seed := []byte(`{"uid":"fleet","title":"Fleet"}`)
	foreign := seedProjectDashboard(t, daemon, store, "fleet", "tenant", seed)
	dual := seedProjectDashboard(t, daemon, store, "fleet", "gardenio", seed)
	_ = daemon.AddLabel(context.Background(), dual, "project:tenant")
	fleet := seedProjectDashboard(t, daemon, store, "fleet", "gardenio", seed)
	grafana.set("fleet", fenceEdit, 4)

	NewGrafanaReadback(GrafanaReadbackConfig{Store: store, Grafana: grafana, Logger: slog.Default()}).PollOnce(context.Background())

	assertPulled(t, daemon, store, fleet)
	assertUntouched(t, daemon, store, foreign)
	assertUntouched(t, daemon, store, dual)
}

func TestReadback_Fence_AnnotationPreferred(t *testing.T) {
	daemon := newFakeDashboardDaemon()
	store := NewDashboardStore(daemon, "test")
	store.SetProjects([]string{"gardenio"})
	grafana := newFakeGrafana()
	seed := []byte(`{"uid":"fleet","title":"Fleet"}`)
	other := seedProjectDashboard(t, daemon, store, "fleet", "gardenio", seed)
	owner := seedProjectDashboard(t, daemon, store, "fleet", "gardenio", seed)
	grafana.set("fleet", fenceEdit, 4)

	NewGrafanaReadback(GrafanaReadbackConfig{
		Store: store, Grafana: grafana, Owners: fakeOwners{"fleet": owner}, Logger: slog.Default(),
	}).PollOnce(context.Background())

	assertPulled(t, daemon, store, owner)
	assertUntouched(t, daemon, store, other)
}

func TestReadback_Fence_AnnotationMismatchWritesNothing(t *testing.T) {
	seed := []byte(`{"uid":"fleet","title":"Fleet"}`)
	cases := []struct {
		name  string
		owner func(t *testing.T, daemon *fakeDashboardDaemon, store *DashboardStore) string
	}{
		{"foreign project", func(t *testing.T, d *fakeDashboardDaemon, s *DashboardStore) string {
			return seedProjectDashboard(t, d, s, "fleet", "tenant", seed)
		}},
		{"other uid", func(t *testing.T, d *fakeDashboardDaemon, s *DashboardStore) string {
			return seedProjectDashboard(t, d, s, "elsewhere", "gardenio", []byte(`{"uid":"elsewhere"}`))
		}},
		{"closed", func(t *testing.T, d *fakeDashboardDaemon, s *DashboardStore) string {
			id := seedProjectDashboard(t, d, s, "fleet", "gardenio", seed)
			d.beads[id].Status = "closed"
			return id
		}},
		{"missing", func(*testing.T, *fakeDashboardDaemon, *DashboardStore) string { return "gd-gone" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			daemon := newFakeDashboardDaemon()
			store := NewDashboardStore(daemon, "test")
			store.SetProjects([]string{"gardenio"})
			grafana := newFakeGrafana()
			fleet := seedProjectDashboard(t, daemon, store, "fleet", "gardenio", seed)
			owner := tc.owner(t, daemon, store)
			grafana.set("fleet", fenceEdit, 4)

			NewGrafanaReadback(GrafanaReadbackConfig{
				Store: store, Grafana: grafana, Owners: fakeOwners{"fleet": owner}, Logger: slog.Default(),
			}).PollOnce(context.Background())

			assertUntouched(t, daemon, store, fleet)
			if _, ok := daemon.beads[owner]; ok {
				assertUntouched(t, daemon, store, owner)
			}
		})
	}
}
