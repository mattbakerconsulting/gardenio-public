package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestDashboardStore_PutAndReadModel(t *testing.T) {
	daemon := newFakeDashboardDaemon()
	store := NewDashboardStore(daemon, "test")
	ctx := context.Background()

	dashID, _ := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{Type: "dashboard", Title: "D"})
	model := []byte(`{"uid":"d","title":"D"}`)
	docID, err := store.PutModel(ctx, dashID, model)
	if err != nil {
		t.Fatalf("put model: %v", err)
	}
	if docID == "" {
		t.Fatal("empty doc id")
	}

	gotDoc, gotModel, err := store.ModelDoc(ctx, dashID)
	if err != nil {
		t.Fatalf("model doc: %v", err)
	}
	if gotDoc != docID {
		t.Fatalf("doc id mismatch: %q != %q", gotDoc, docID)
	}
	if string(gotModel) != string(model) {
		t.Fatalf("model mismatch: %s", gotModel)
	}

	// Re-put replaces content in the same doc (no second doc created).
	model2 := []byte(`{"uid":"d","title":"D2"}`)
	docID2, err := store.PutModel(ctx, dashID, model2)
	if err != nil {
		t.Fatalf("put model 2: %v", err)
	}
	if docID2 != docID {
		t.Fatalf("expected same doc on re-put, got %q vs %q", docID2, docID)
	}
	_, gotModel2, _ := store.ModelDoc(ctx, dashID)
	if string(gotModel2) != string(model2) {
		t.Fatalf("model not updated: %s", gotModel2)
	}
}

func TestDashboardStore_PutModelRejectsInvalidJSON(t *testing.T) {
	daemon := newFakeDashboardDaemon()
	store := NewDashboardStore(daemon, "test")
	dashID, _ := daemon.CreateBead(context.Background(), beadsapi.CreateBeadRequest{Type: "dashboard"})
	if _, err := store.PutModel(context.Background(), dashID, []byte(`not json`)); err == nil {
		t.Fatal("expected error for invalid JSON model")
	}
}

func TestDashboardStore_FindByUID(t *testing.T) {
	daemon := newFakeDashboardDaemon()
	store := NewDashboardStore(daemon, "test")
	ctx := context.Background()
	dashID, _ := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Type:   "dashboard",
		Fields: json.RawMessage(`{"dashboard_uid":"target"}`),
	})
	got, err := store.FindByUID(ctx, "target")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got != dashID {
		t.Fatalf("find by uid: %q != %q", got, dashID)
	}
	miss, _ := store.FindByUID(ctx, "nope")
	if miss != "" {
		t.Fatalf("expected miss, got %q", miss)
	}
}

func TestBackfillDashboards_CreatesIdempotently(t *testing.T) {
	daemon := newFakeDashboardDaemon()
	store := NewDashboardStore(daemon, "test")
	ctx := context.Background()

	dashboards := []BackfillDashboard{
		{Name: "cost", Model: []byte(`{"uid":"cost","title":"Cost","version":1}`)},
		{Name: "embed", Model: []byte(`{"uid":"embed","title":"Embed","version":1}`)},
	}

	res := BackfillDashboards(ctx, store, dashboards, "gardenio", slog.Default())
	if len(res) != 2 {
		t.Fatalf("expected 2 results, got %d", len(res))
	}
	for _, r := range res {
		if r.Err != nil {
			t.Fatalf("backfill %s err: %v", r.Name, r.Err)
		}
		if !r.Created || r.BeadID == "" || r.ModelDoc == "" {
			t.Fatalf("unexpected result: %+v", r)
		}
	}

	// Re-run: should NOT create new beads (idempotent on uid).
	res2 := BackfillDashboards(ctx, store, dashboards, "gardenio", slog.Default())
	for _, r := range res2 {
		if r.Created {
			t.Fatalf("backfill re-run created a duplicate bead for %s", r.Name)
		}
	}
	// Verify exactly 2 dashboard beads exist.
	list, _ := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{Types: []string{"dashboard"}})
	if list.Total != 2 {
		t.Fatalf("expected 2 dashboard beads after re-run, got %d", list.Total)
	}
}

func TestBackfillDashboards_SkipsNoUID(t *testing.T) {
	daemon := newFakeDashboardDaemon()
	store := NewDashboardStore(daemon, "test")
	res := BackfillDashboards(context.Background(), store,
		[]BackfillDashboard{{Name: "bad", Model: []byte(`{"title":"no uid"}`)}}, "gardenio", slog.Default())
	if len(res) != 1 || res[0].Err == nil {
		t.Fatalf("expected error result for uid-less dashboard: %+v", res)
	}
}

func TestBackfillDashboards_AppliesProjectLabel(t *testing.T) {
	daemon := newFakeDashboardDaemon()
	store := NewDashboardStore(daemon, "test")
	ctx := context.Background()
	res := BackfillDashboards(ctx, store,
		[]BackfillDashboard{{Name: "d", Model: []byte(`{"uid":"d","title":"D"}`)}}, "gardenio", slog.Default())
	b, _ := daemon.GetBead(ctx, res[0].BeadID)
	if !hasLabel(b.Labels, "project:gardenio") {
		t.Fatalf("missing project label: %v", b.Labels)
	}
	if !hasLabel(b.Labels, "grafana:dashboard") {
		t.Fatalf("missing grafana:dashboard label: %v", b.Labels)
	}
}

func TestDashboardStore_FindByUID_Fenced(t *testing.T) {
	daemon := newFakeDashboardDaemon()
	store := NewDashboardStore(daemon, "test")
	ctx := context.Background()
	mk := func(labels ...string) string {
		id, _ := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
			Type:   "dashboard",
			Labels: labels,
			Fields: json.RawMessage(`{"dashboard_uid":"target"}`),
		})
		return id
	}
	mk("project:tenant")
	mk()
	mk("project:gardenio", "project:tenant")

	store.SetProjects([]string{"gardenio"})
	if got, _ := store.FindByUID(ctx, "target"); got != "" {
		t.Fatalf("fenced store found a bead outside the allow-list: %q", got)
	}
	fleet := mk("project:gardenio")
	if got, _ := store.FindByUID(ctx, "target"); got != fleet {
		t.Fatalf("fenced store: got %q, want fleet bead %q", got, fleet)
	}

	store.SetProjects([]string{})
	if got, _ := store.FindByUID(ctx, "target"); got != "" {
		t.Fatalf("empty allow-list matched %q", got)
	}
}

func TestBackfillDashboards_ForeignBeadDoesNotShadowFleetUID(t *testing.T) {
	daemon := newFakeDashboardDaemon()
	store := NewDashboardStore(daemon, "test")
	store.SetProjects([]string{"gardenio"})
	ctx := context.Background()
	foreign, _ := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Type:   "dashboard",
		Labels: []string{"project:tenant"},
		Fields: json.RawMessage(`{"dashboard_uid":"cost"}`),
	})
	dashboards := []BackfillDashboard{{Name: "cost", Model: []byte(`{"uid":"cost","title":"Cost"}`)}}

	res := BackfillDashboards(ctx, store, dashboards, "gardenio", slog.Default())
	if len(res) != 1 || res[0].Err != nil || !res[0].Created || res[0].BeadID == foreign {
		t.Fatalf("backfill should create a fleet bead beside the foreign one: %+v", res)
	}
	fleet := res[0].BeadID
	if _, model, _ := store.ModelDoc(ctx, foreign); len(model) != 0 {
		t.Fatalf("backfill wrote the fleet model into the foreign bead: %s", model)
	}

	res2 := BackfillDashboards(ctx, store, dashboards, "gardenio", slog.Default())
	if len(res2) != 1 || res2[0].Created || res2[0].BeadID != fleet {
		t.Fatalf("re-run should find the fleet bead %s: %+v", fleet, res2)
	}
}
