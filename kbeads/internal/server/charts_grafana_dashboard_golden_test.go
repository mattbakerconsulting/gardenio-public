package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// charts_grafana_dashboard_golden_test.go — the offline, hermetic DRIFT GUARD
// (task ka-0000004040, epic ka-0000002763).
//
// Epic ka-0000002763 makes the kbeads chart catalog the single source of truth
// for the Grafana cost dashboards and GENERATES the dashboard model from it
// (AssembleDashboardByUID). The epic's acceptance is that "a drift test fails
// CI if the dashboard model is hand-edited out of sync with the catalog." The
// sibling surfaces cover the two live/structural halves:
//
//   - `kd admin gen-dashboards --check` asserts the LIVE model doc matches the
//     catalog assembly — but it needs a running daemon, so it runs as a
//     scheduled alert / CI job, not a unit test.
//   - TestAssembleDashboard_* / TestDashboardCatalog_* pin individual STRUCTURAL
//     facts (panel count, order, ids, query parity, byte-stability) — a
//     hand-written allowlist that only catches the specific fields someone
//     thought to assert.
//
// This test is the third, hermetic half: it pins the FULL assembled model bytes
// for every catalog-backed uid to a checked-in golden file. Any change to the
// catalog, the panel generator, the assembler, or the marshal — anything that
// alters a byte of what `gen-dashboards` would write to the live doc — fails
// this test with a diff, WITHOUT a daemon, network, or clock. So a one-line
// catalog edit that changes the generated model is a DELIBERATE, reviewed act
// (regenerate the golden + eyeball the diff) rather than a silent divergence.
//
// The committed goldens double as a human-inspectable reference: each
// <uid>.golden.json is exactly the JSON `kd admin gen-dashboards` writes into
// that dashboard's grafana:dashboard-model doc — open it to see what the live
// Grafana panel renders from the catalog.
//
// Regenerate after an INTENTIONAL catalog/generator change:
//
//	GB_UPDATE_GOLDEN=1 go test ./internal/server/ -run TestDashboardModel_Golden
//
// then review the diff in the testdata/dashboards/*.golden.json files before
// committing.

// dashboardGoldenDir holds one <uid>.golden.json per catalog dashboard.
const dashboardGoldenDir = "testdata/dashboards"

// TestDashboardModel_Golden pins the assembled+marshaled model for every
// catalog dashboard uid to a checked-in byte-for-byte baseline. This is the
// drift guard the epic asks for: if the catalog and the committed model diverge
// — a panel added to a spec but the golden never regenerated, a generator tweak
// that shifts a field — CI reds here with the exact JSON diff.
func TestDashboardModel_Golden(t *testing.T) {
	update := os.Getenv("GB_UPDATE_GOLDEN") != ""

	uids := DashboardUIDs()
	if len(uids) == 0 {
		t.Fatal("DashboardUIDs() is empty — the catalog declares no dashboards")
	}

	// Track which golden files a live uid accounts for, so a uid REMOVED from
	// the catalog (leaving a stale golden behind) is caught too — otherwise a
	// deleted dashboard would silently keep a dead baseline on disk.
	live := make(map[string]bool, len(uids))

	for _, uid := range uids {
		live[uid+".golden.json"] = true
		t.Run(uid, func(t *testing.T) {
			d, err := AssembleDashboardByUID(uid)
			if err != nil {
				t.Fatalf("AssembleDashboardByUID(%q): %v", uid, err)
			}
			got, err := MarshalDashboard(d)
			if err != nil {
				t.Fatalf("MarshalDashboard(%q): %v", uid, err)
			}

			path := filepath.Join(dashboardGoldenDir, uid+".golden.json")

			if update {
				if err := os.MkdirAll(dashboardGoldenDir, 0o755); err != nil {
					t.Fatalf("mkdir %s: %v", dashboardGoldenDir, err)
				}
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatalf("update golden %s: %v", path, err)
				}
				t.Logf("updated golden %s (%d bytes)", path, len(got))
				return
			}

			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden %s: %v (regenerate with GB_UPDATE_GOLDEN=1)", path, err)
			}
			if string(got) != string(want) {
				t.Errorf("dashboard %q model drifted from the committed golden %s.\n"+
					"If this catalog/generator change is INTENTIONAL, regenerate with "+
					"GB_UPDATE_GOLDEN=1 and review the diff, then run `kd admin gen-dashboards` "+
					"to reconcile the live model doc.\n%s",
					uid, path, firstJSONDiff(string(want), string(got)))
			}
		})
	}

	// A golden with no corresponding live uid is a stale baseline for a
	// dashboard the catalog no longer declares — fail so it is removed (or the
	// deletion reverted) deliberately.
	entries, err := os.ReadDir(dashboardGoldenDir)
	if err != nil {
		t.Fatalf("read golden dir %s: %v", dashboardGoldenDir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if filepath.Ext(name) != ".json" {
			continue
		}
		if !live[name] {
			t.Errorf("stale golden %s has no matching catalog dashboard uid — "+
				"remove it (or restore the dashboard) and regenerate with GB_UPDATE_GOLDEN=1",
				filepath.Join(dashboardGoldenDir, name))
		}
	}
}

// firstJSONDiff returns a short, line-anchored description of the first line at
// which want and got differ, so a golden failure names the exact drifted line
// instead of dumping two multi-KB JSON blobs.
func firstJSONDiff(want, got string) string {
	wl := strings.Split(want, "\n")
	gl := strings.Split(got, "\n")
	n := len(wl)
	if len(gl) < n {
		n = len(gl)
	}
	for i := 0; i < n; i++ {
		if wl[i] != gl[i] {
			return fmt.Sprintf("first diff at line %d:\n  want: %s\n   got: %s", i+1, wl[i], gl[i])
		}
	}
	if len(wl) != len(gl) {
		return fmt.Sprintf("golden has %d lines, generated has %d (identical up to line %d)", len(wl), len(gl), n)
	}
	return "(no line diff — byte-level difference in otherwise identical lines)"
}
