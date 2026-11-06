package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

func seedPurgeBead(ms *mockStore, id string, typ model.BeadType, status model.Status, labels []string, fields map[string]any) {
	raw, _ := json.Marshal(fields)
	ms.beads[id] = &model.Bead{ID: id, Title: id, Type: typ, Status: status, Fields: raw, Labels: labels}
	ms.labels[id] = labels
}

// purgeClosedAt is when seedClosedLand's land closed.
var purgeClosedAt = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

// setPurgeClock sets the clock the retention floors are read against to
// purgeClosedAt plus days.
func setPurgeClock(t *testing.T, days int) {
	t.Helper()
	prev := landPurgeNow
	landPurgeNow = func() time.Time { return purgeClosedAt.AddDate(0, 0, days) }
	t.Cleanup(func() { landPurgeNow = prev })
}

// seedClosedLand is a closed tenant land "acme" with a task, an agent, a
// message, a credit grant, a bead it shares with the operator's project and,
// outside it, a bead of another project.
func seedClosedLand(ms *mockStore, fields map[string]any) {
	if fields == nil {
		fields = map[string]any{"land_kind": "tenant", "land_closed_at": "2026-09-01T00:00:00Z"}
	}
	b := &model.Bead{ID: "p-acme", Title: "acme", Type: "project", Status: model.StatusClosed, Labels: []string{"project:acme"}}
	b.Fields, _ = json.Marshal(fields)
	ms.beads[b.ID] = b
	ms.labels[b.ID] = b.Labels
	in := []string{"project:acme"}
	seedPurgeBead(ms, "t-1", "task", model.StatusClosed, in, nil)
	seedPurgeBead(ms, "a-1", "agent", model.StatusClosed, in, nil)
	seedPurgeBead(ms, "m-1", "message", model.StatusOpen, in, nil)
	seedPurgeBead(ms, "g-1", typeCreditGrant, model.StatusClosed, in, map[string]any{"land": "acme"})
	seedPurgeBead(ms, "s-1", "agent", model.StatusClosed, []string{"project:acme", "project:gardenio"}, nil)
	seedPurgeBead(ms, "o-1", "task", model.StatusOpen, []string{"project:other"}, nil)
}

func purgeRequest(ctx context.Context, body any) *http.Request {
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/lands/purge", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	return req.WithContext(ctx)
}

var provisionerCtx = func() context.Context {
	return machineCtx("gardenio-controller", "gardenio-machine-lands-provisioner")
}

func doPurge(t *testing.T, s *BeadsServer, ctx context.Context, body any, want int) landPurgeResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleLandPurge(rec, purgeRequest(ctx, body))
	requireStatus(t, rec, want)
	var resp landPurgeResponse
	if want == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return resp
}

func TestLandPurge_PurgesTheLandButKeepsProjectAndLedger(t *testing.T) {
	setPurgeClock(t, 31)
	s, ms, _ := newTestServer()
	seedClosedLand(ms, nil)

	resp := doPurge(t, s, provisionerCtx(), map[string]any{"land": "acme", "dry_run": true}, http.StatusOK)
	if want := []string{"a-1", "m-1", "t-1"}; !slices.Equal(resp.Purged, want) {
		t.Fatalf("dry run purged = %v, want %v", resp.Purged, want)
	}
	if resp.Types["a-1"] != "agent" {
		t.Fatalf("types = %v, want a-1 agent", resp.Types)
	}
	if ms.beads["t-1"] == nil {
		t.Fatal("a dry run deleted t-1")
	}

	resp = doPurge(t, s, provisionerCtx(), map[string]any{"land": "acme"}, http.StatusOK)
	if len(resp.Purged) != 3 {
		t.Fatalf("purged = %v, want 3 beads", resp.Purged)
	}
	for _, id := range []string{"t-1", "a-1", "m-1"} {
		if ms.beads[id] != nil {
			t.Errorf("%s survived the purge", id)
		}
	}
	for _, id := range []string{"p-acme", "g-1", "s-1", "o-1"} {
		if ms.beads[id] == nil {
			t.Errorf("%s was purged; it must be kept", id)
		}
	}

	// The ledger stays for the records period, dry run or not.
	setPurgeClock(t, 729)
	doPurge(t, s, provisionerCtx(), map[string]any{"land": "acme", "include_records": true, "dry_run": true}, http.StatusConflict)
	doPurge(t, s, provisionerCtx(), map[string]any{"land": "acme", "include_records": true}, http.StatusConflict)
	if ms.beads["g-1"] == nil {
		t.Fatal("the ledger went before the records period was over")
	}
	setPurgeClock(t, 731)
	resp = doPurge(t, s, provisionerCtx(), map[string]any{"land": "acme", "include_records": true}, http.StatusOK)
	if !slices.Equal(resp.Purged, []string{"g-1"}) {
		t.Fatalf("records purge = %v, want [g-1]", resp.Purged)
	}
	if ms.beads["p-acme"] == nil {
		t.Fatal("the records purge deleted the project tombstone")
	}
}

func TestLandPurge_RefusesALandInsideRetention(t *testing.T) {
	for name, tc := range map[string]struct {
		env  string
		days int
		want int
	}{
		"29 days, default 30":       {"", 29, http.StatusConflict},
		"30 days, default 30":       {"", 30, http.StatusOK},
		"8 days, floor 7":           {"7", 8, http.StatusOK},
		"6 days, floor 7":           {"7", 6, http.StatusConflict},
		"29 days, invalid floor 0":  {"0", 29, http.StatusConflict},
		"29 days, invalid floor x":  {"x", 29, http.StatusConflict},
		"31 days, invalid floor -5": {"-5", 31, http.StatusOK},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(landsRetentionDaysEnv, tc.env)
			setPurgeClock(t, tc.days)
			s, ms, _ := newTestServer()
			seedClosedLand(ms, nil)
			// The dry run is held to the floor too: the controller deletes
			// session objects between its dry run and the purge.
			doPurge(t, s, provisionerCtx(), map[string]any{"land": "acme", "dry_run": true}, tc.want)
			doPurge(t, s, provisionerCtx(), map[string]any{"land": "acme"}, tc.want)
			if gone := ms.beads["t-1"] == nil; gone != (tc.want == http.StatusOK) {
				t.Fatalf("t-1 gone = %v, want %v", gone, tc.want == http.StatusOK)
			}
		})
	}
}

func TestLandPurge_RefusesWhatIsNotAClosedTenantLand(t *testing.T) {
	setPurgeClock(t, 400)
	for name, fields := range map[string]map[string]any{
		"operator land":     {"land_kind": "operator", "land_closed_at": "2026-09-01T00:00:00Z"},
		"no land_kind":      {"land_closed_at": "2026-09-01T00:00:00Z"},
		"no land_closed_at": {"land_kind": "tenant"},
	} {
		t.Run(name, func(t *testing.T) {
			s, ms, _ := newTestServer()
			seedClosedLand(ms, fields)
			doPurge(t, s, provisionerCtx(), map[string]any{"land": "acme"}, http.StatusConflict)
			if ms.beads["t-1"] == nil {
				t.Fatal("a refused purge deleted t-1")
			}
		})
	}

	s, ms, _ := newTestServer()
	seedClosedLand(ms, nil)
	ms.beads["p-acme"].Status = model.StatusOpen
	doPurge(t, s, provisionerCtx(), map[string]any{"land": "acme"}, http.StatusConflict)
	doPurge(t, s, provisionerCtx(), map[string]any{"land": "nope"}, http.StatusNotFound)
	doPurge(t, s, provisionerCtx(), map[string]any{"land": "lands-intake"}, http.StatusBadRequest)
	doPurge(t, s, provisionerCtx(), map[string]any{}, http.StatusBadRequest)
	doPurge(t, s, provisionerCtx(), map[string]any{"land": "acme", "application_id": "x"}, http.StatusBadRequest)
}

func TestLandPurge_OnlyAdminsAndTheProvisioner(t *testing.T) {
	setPurgeClock(t, 31)
	for name, ctx := range map[string]context.Context{
		"static token":         WithAuthIdentity(withAuthMethod(context.Background(), authMethodStatic), AuthIdentity{Type: "static", Subject: "static"}),
		"other machine":        machineCtx("gardenio-controller", "gardenio-machine-accounts"),
		"oidc operator":        oidcCtx("op@example.com", []string{RoleOperator}),
		"fleet token":          WithAuthIdentity(withAuthMethod(context.Background(), authMethodStatic), AuthIdentity{Type: "fleet", Subject: "agent-x"}),
		"land principal":       WithAuthIdentity(withAuthMethod(context.Background(), authMethodLand), AuthIdentity{Type: "land", Subject: "land:acme"}),
		"machine with no role": machineCtx("gardenio-authz-canary"),
	} {
		t.Run(name, func(t *testing.T) {
			s, ms, _ := newTestServer()
			seedClosedLand(ms, nil)
			doPurge(t, s, ctx, map[string]any{"land": "acme"}, http.StatusForbidden)
			if ms.beads["t-1"] == nil {
				t.Fatal("a refused caller purged t-1")
			}
		})
	}

	s, ms, _ := newTestServer()
	seedClosedLand(ms, nil)
	doPurge(t, s, oidcCtx("admin@example.com", []string{RoleAdmin}), map[string]any{"land": "acme"}, http.StatusOK)
	if ms.beads["t-1"] != nil {
		t.Fatal("an OIDC admin's purge left t-1")
	}
}

func TestLandPurge_Application(t *testing.T) {
	setPurgeClock(t, 91)
	s, ms, _ := newTestServer()
	intake := []string{"project:lands-intake"}
	decided := purgeClosedAt.Format(time.RFC3339)
	seedPurgeBead(ms, "pa-denied", typePlotApplication, model.StatusClosed, intake, map[string]any{"state": "denied", "decided_at": decided})
	seedPurgeBead(ms, "pa-withdrawn", typePlotApplication, model.StatusClosed, intake, map[string]any{"state": "withdrawn"})
	closed := purgeClosedAt
	ms.beads["pa-withdrawn"].ClosedAt = &closed
	seedPurgeBead(ms, "pa-closed", typePlotApplication, model.StatusClosed, intake, map[string]any{"state": "closed", "decided_at": decided})
	seedPurgeBead(ms, "pa-elsewhere", typePlotApplication, model.StatusClosed, []string{"project:acme"}, map[string]any{"state": "denied", "decided_at": decided})
	seedPurgeBead(ms, "t-1", "task", model.StatusClosed, intake, map[string]any{"state": "denied", "decided_at": decided})
	// Turned down 89 days ago, and one with no decision time at all.
	seedPurgeBead(ms, "pa-recent", typePlotApplication, model.StatusClosed, intake,
		map[string]any{"state": "denied", "decided_at": purgeClosedAt.AddDate(0, 0, 2).Format(time.RFC3339)})
	seedPurgeBead(ms, "pa-undated", typePlotApplication, model.StatusClosed, intake, map[string]any{"state": "denied"})

	for _, id := range []string{"pa-denied", "pa-withdrawn"} {
		resp := doPurge(t, s, provisionerCtx(), map[string]any{"application_id": id}, http.StatusOK)
		if !slices.Equal(resp.Purged, []string{id}) || ms.beads[id] != nil {
			t.Fatalf("purging %s: purged %v, still there = %v", id, resp.Purged, ms.beads[id] != nil)
		}
	}
	// An approved land's application is its agreement record: kept.
	doPurge(t, s, provisionerCtx(), map[string]any{"application_id": "pa-closed"}, http.StatusConflict)
	doPurge(t, s, provisionerCtx(), map[string]any{"application_id": "pa-elsewhere"}, http.StatusConflict)
	doPurge(t, s, provisionerCtx(), map[string]any{"application_id": "t-1"}, http.StatusConflict)
	doPurge(t, s, provisionerCtx(), map[string]any{"application_id": "gone"}, http.StatusNotFound)
	doPurge(t, s, provisionerCtx(), map[string]any{"application_id": "pa-recent", "dry_run": true}, http.StatusConflict)
	doPurge(t, s, provisionerCtx(), map[string]any{"application_id": "pa-recent"}, http.StatusConflict)
	doPurge(t, s, provisionerCtx(), map[string]any{"application_id": "pa-undated"}, http.StatusConflict)
	for _, id := range []string{"pa-closed", "pa-elsewhere", "t-1", "pa-recent", "pa-undated"} {
		if ms.beads[id] == nil {
			t.Errorf("%s was purged", id)
		}
	}
}

// A land that shares its name with an older project (a fleet project known
// only by its label, or a second project bead) is not purged: the purge
// deletes by label and would take the other project's beads with it
// (gd-i74esUHCTg item 3).
func TestLandPurge_RefusesALabelNotTheLandsAlone(t *testing.T) {
	landBorn := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		seed func(ms *mockStore)
	}{
		{"a bead older than the land", func(ms *mockStore) {
			seedPurgeBead(ms, "f-1", "task", model.StatusOpen, []string{"project:acme"}, nil)
			ms.beads["f-1"].CreatedAt = landBorn.Add(-time.Hour)
		}},
		{"another project bead carrying the label", func(ms *mockStore) {
			seedPurgeBead(ms, "p-fleet", "project", model.StatusOpen, []string{"project:acme"}, nil)
			ms.beads["p-fleet"].Title = "fleet acme"
			ms.beads["p-fleet"].CreatedAt = landBorn.Add(time.Hour)
		}},
		{"a second project bead with the name", func(ms *mockStore) {
			seedPurgeBead(ms, "p-fleet", "project", model.StatusOpen, nil, nil)
			ms.beads["p-fleet"].Title = "Project: acme"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setPurgeClock(t, 31)
			s, ms, _ := newTestServer()
			seedClosedLand(ms, nil)
			for _, id := range []string{"p-acme", "t-1", "a-1", "m-1"} {
				ms.beads[id].CreatedAt = landBorn
			}
			tc.seed(ms)

			doPurge(t, s, provisionerCtx(), map[string]any{"land": "acme", "dry_run": true}, http.StatusConflict)
			doPurge(t, s, provisionerCtx(), map[string]any{"land": "acme"}, http.StatusConflict)
			for _, id := range []string{"t-1", "a-1", "m-1", "f-1", "p-fleet"} {
				if _, seeded := ms.labels[id]; seeded && ms.beads[id] == nil {
					t.Errorf("%s was purged by a refused purge", id)
				}
			}
		})
	}
}
