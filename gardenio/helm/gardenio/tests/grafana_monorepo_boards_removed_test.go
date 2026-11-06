package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The three monorepo Grafana boards were removed from gardenio's chart tree in
// phase S3 (task gd-6w3BQNBx3Q, parent bug gd-mTfRlbbGom, decision
// gd-3C9GbR9qMR = split): they show monorepo/ACME-product telemetry, not
// gardenio-platform telemetry, so they were never gardenio's to host on its
// /charts hub. Definitions were handed to the monorepo project (bead
// gd-GvXtUsZIwf) and preserved in git history.
//
// The removal has to be DURABLE / backfill-proof. Closing the dashboard bead
// alone is NOT enough: the grafana-bridge bakes every top-level
// dashboards/*.json into its image (images.yml grafana-bridge) and
// runBackfill (cmd/grafana-bridge: os.ReadDir over the top-level dir → DashboardStore.FindByUID)
// recreates a fresh type=dashboard bead for any shipping uid it cannot find in an
// ACTIVE status. So a closed bead + a still-shipping JSON silently comes back on
// the next bridge restart. The only durable removal is deleting the JSON at the
// source, which is exactly what these tests lock in: if anyone re-adds one of
// these boards to the top-level glob, backfill would resurrect it and this test
// reds first.
var removedMonorepoUIDs = []string{
	"monorepo-service-health",
	"monorepo-distributed-traces",
	"monorepo-db-performance",
}

// dashboardsGlobDir is the top-level directory whose *.json files the
// grafana-bridge image bakes and runBackfill provisions. A subdirectory
// (e.g. dashboards/_parked/) is intentionally NOT matched by that glob.
func dashboardsGlobDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(chartDir(t), "dashboards")
}

// TestMonorepoBoards_NotInBackfillGlob asserts that none of the removed
// monorepo boards ship in the top-level dashboards/*.json glob — neither as a
// standalone file nor as the uid of any other shipping board. This is the
// backfill-proof guard: anything the glob enumerates is what runBackfill can
// recreate, so an empty result here is a proof the boards cannot be resurrected
// on a bridge restart.
func TestMonorepoBoards_NotInBackfillGlob(t *testing.T) {
	t.Parallel()
	dir := dashboardsGlobDir(t)
	entries, err := os.ReadDir(dir) // mirrors runBackfill: top level only
	if err != nil {
		t.Fatalf("read dashboards dir %s: %v", dir, err)
	}
	shipped := map[string]string{} // uid -> file
	for _, e := range entries {
		// runBackfill skips subdirs and non-.json; mirror that exactly.
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
			continue
		}
		raw, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			t.Fatalf("read %s: %v", e.Name(), rerr)
		}
		var d struct {
			UID string `json:"uid"`
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatalf("dashboards/%s is not valid JSON: %v", e.Name(), err)
		}
		if d.UID != "" {
			shipped[d.UID] = e.Name()
		}
	}
	for _, uid := range removedMonorepoUIDs {
		if f, ok := shipped[uid]; ok {
			t.Errorf("removed monorepo board uid %q is shipping again in the backfill glob (file %s) — the grafana-bridge would recreate its bead on the next restart; keep it out of the top-level dashboards/*.json (S3 gd-6w3BQNBx3Q)", uid, f)
		}
		// Also guard the filename convention, so a re-add under the expected
		// name reds even if the uid were later changed.
		if _, err := os.Stat(filepath.Join(dir, uid+".json")); err == nil {
			t.Errorf("dashboards/%s.json is back in the top-level glob — delete it; it belongs to the monorepo project (S3 gd-6w3BQNBx3Q)", uid)
		}
	}
}

// TestMonorepoBoards_NoDeadNavLinks guards that no shipping board links to a
// removed monorepo uid via a /d/<uid> or /grafana/d/<uid> nav link — such a link
// would 404 now that the target board is gone. perf-overview.json and
// perf-traces.json used to carry these links; the S3 change stripped them.
func TestMonorepoBoards_NoDeadNavLinks(t *testing.T) {
	t.Parallel()
	dir := dashboardsGlobDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dashboards dir %s: %v", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
			continue
		}
		raw, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			t.Fatalf("read %s: %v", e.Name(), rerr)
		}
		s := string(raw)
		for _, uid := range removedMonorepoUIDs {
			if strings.Contains(s, "/d/"+uid) {
				t.Errorf("dashboards/%s links to removed board /d/%s — that nav link now 404s; drop it (S3 gd-6w3BQNBx3Q)", e.Name(), uid)
			}
		}
	}
}
