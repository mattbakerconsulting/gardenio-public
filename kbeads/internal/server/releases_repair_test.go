package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

// releaseRows is a test release's deployments, as (revision, since, until, by).
type releaseRows [][4]string

// seedReleaseRows writes rows into release id's deployments as stored, past
// the deploy route: the bogus history a late duplicate report left before
// deployRelease refused one.
func seedReleaseRows(t *testing.T, ms *mockStore, id string, rows releaseRows) {
	t.Helper()
	deps := make([]releaseDeployment, 0, len(rows))
	for _, r := range rows {
		deps = append(deps, releaseDeployment{Target: "prod", Kind: "k3s", Revision: r[0], Since: r[1], Until: r[2], By: r[3], Via: "script"})
	}
	m := releaseFieldMap(ms.beads[id].Fields)
	m["deployments"] = deps
	m["status"] = releaseStatus(releaseString(m, "tag"), "", deps)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	ms.beads[id].Fields = b
}

// wantReleaseRows checks release id's (revision, since, until) rows and status.
func wantReleaseRows(t *testing.T, ms *mockStore, id, status string, rows releaseRows) {
	t.Helper()
	v := releaseViewOf(ms.beads[id])
	ok := v.Status == status && len(v.Deployments) == len(rows)
	for i := 0; ok && i < len(rows); i++ {
		d := v.Deployments[i]
		ok = d.Revision == rows[i][0] && d.Since == rows[i][1] && d.Until == rows[i][2]
	}
	if !ok {
		t.Errorf("%s = %s %+v, want %s %v", v.Tag, v.Status, v.Deployments, status, rows)
	}
}

// A helm revision is one roll on a target: a late report of a revision the
// release already records, closed or open, changes nothing; one another
// release records is a 409 and changes nothing; with no revision the old rule
// stands, so a later report is a new roll.
func TestRelease_DeployOneRowPerRevision(t *testing.T) {
	_, ms, h := newTestServer()
	seedReleaseProject(ms, "proj-gardenio", "gardenio", testGardenioReleaseSource)
	_, r1 := createReleaseAsAdmin(t, h, map[string]any{"project": "gardenio", "tag": "2026.305.1", "cut_at": "2026-11-01T04:00:00Z"})
	_, r2 := createReleaseAsAdmin(t, h, map[string]any{"project": "gardenio", "tag": "2026.305.2", "cut_at": "2026-11-01T16:00:00Z"})
	deploy := func(id string, body map[string]any) int {
		t.Helper()
		body["target"] = "prod"
		return doJSON(t, h, http.MethodPost, "/v1/releases/"+id+"/deployments", body).Code
	}
	fields := func() [2]string { return [2]string{string(ms.beads[r1.ID].Fields), string(ms.beads[r2.ID].Fields)} }

	if code := deploy(r1.ID, map[string]any{"since": "2026-11-01T04:33:00Z", "revision": "263", "by": "observed", "via": "reconciler"}); code != http.StatusOK {
		t.Fatalf("deploy 305.1: %d", code)
	}
	if code := deploy(r2.ID, map[string]any{"since": "2026-11-01T17:14:35Z", "revision": "264", "by": "observed", "via": "reconciler"}); code != http.StatusOK {
		t.Fatalf("deploy 305.2: %d", code)
	}
	before := fields()

	// The owner's late reports at 17:40Z of both rolls.
	for _, tc := range []struct {
		id, revision string
	}{{r1.ID, "263"}, {r2.ID, "264"}} {
		if code := deploy(tc.id, map[string]any{"since": "2026-11-01T17:40:24Z", "revision": tc.revision, "by": "backfill"}); code != http.StatusOK {
			t.Errorf("late report of rev %s: %d, want 200", tc.revision, code)
		}
	}
	if fields() != before {
		t.Errorf("a late report of a recorded revision rewrote a release")
	}
	wantReleaseRows(t, ms, r1.ID, releaseStatusSuperseded, releaseRows{{"263", "2026-11-01T04:33:00Z", "2026-11-01T17:14:35Z"}})
	wantReleaseRows(t, ms, r2.ID, releaseStatusLive, releaseRows{{"264", "2026-11-01T17:14:35Z", ""}})

	// A revision carries one tag.
	if code := deploy(r2.ID, map[string]any{"since": "2026-11-01T18:00:00Z", "revision": "263"}); code != http.StatusConflict {
		t.Errorf("rev 263 reported for 305.2: %d, want 409", code)
	}
	if fields() != before {
		t.Errorf("a refused revision rewrote a release")
	}

	// No revision: a later report is a new roll (a rollback onto 305.1).
	if code := deploy(r1.ID, map[string]any{"since": "2026-11-01T19:00:00Z"}); code != http.StatusOK {
		t.Fatalf("deploy 305.1 without a revision: %d", code)
	}
	wantReleaseRows(t, ms, r2.ID, releaseStatusSuperseded, releaseRows{{"264", "2026-11-01T17:14:35Z", "2026-11-01T19:00:00Z"}})
	wantReleaseRows(t, ms, r1.ID, releaseStatusLive, releaseRows{
		{"263", "2026-11-01T04:33:00Z", "2026-11-01T17:14:35Z"},
		{"", "2026-11-01T19:00:00Z", ""},
	})
	again := fields()
	if code := deploy(r1.ID, map[string]any{}); code != http.StatusOK || fields() != again {
		t.Errorf("a repeat of the open row with no revision: %d, changed %v", code, fields() != again)
	}
}

// The repair collapses prod's duplicate rows of 11-01 (late reports of rolls
// the reconciler had recorded) into the earliest of each revision, reopens the
// row that runs now and recomputes status; a dry run writes nothing and a
// second run changes nothing. A revision on two releases is left alone.
func TestRelease_RepairDeployments(t *testing.T) {
	_, ms, h := newTestServer()
	seedReleaseProject(ms, "proj-gardenio", "gardenio", testGardenioReleaseSource)
	ids := map[string]string{}
	for _, tag := range []string{"2026.302.2", "2026.303.2", "2026.305.1", "2026.305.2"} {
		_, r := createReleaseAsAdmin(t, h, map[string]any{"project": "gardenio", "tag": tag})
		ids[tag] = r.ID
	}
	seedReleaseRows(t, ms, ids["2026.303.2"], releaseRows{
		{"259", "2026-10-30T16:23:47Z", "2026-10-31T01:20:10Z", "backfill"},
		{"259", "2026-10-31T01:20:10Z", "2026-11-01T04:33:00Z", "owner"},
	})
	seedReleaseRows(t, ms, ids["2026.305.1"], releaseRows{
		{"263", "2026-11-01T04:33:00Z", "2026-11-01T17:14:35Z", "observed"},
		{"263", "2026-11-01T17:40:24Z", "2026-11-01T17:40:25Z", "backfill"},
	})
	seedReleaseRows(t, ms, ids["2026.305.2"], releaseRows{
		{"264", "2026-11-01T17:14:35Z", "2026-11-01T17:40:24Z", "observed"},
		{"264", "2026-11-01T17:40:25Z", "", "backfill"},
	})
	snapshot := func() map[string]string {
		out := map[string]string{}
		for tag, id := range ids {
			out[tag] = string(ms.beads[id].Fields)
		}
		return out
	}
	repair := func(dryRun bool) releaseRepairResult {
		t.Helper()
		rec := doJSON(t, h, http.MethodPost, "/v1/releases/repair-deployments", map[string]any{"project": "gardenio", "dry_run": dryRun})
		if rec.Code != http.StatusOK {
			t.Fatalf("repair (dry run %v): %d %s", dryRun, rec.Code, rec.Body.String())
		}
		var res releaseRepairResult
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		return res
	}

	seeded := snapshot()
	dry := repair(true)
	if !dry.DryRun || len(dry.Releases) != 3 || len(dry.Conflicts) != 0 {
		t.Fatalf("dry run = %+v, want 3 releases to change", dry)
	}
	for tag, f := range snapshot() {
		if f != seeded[tag] {
			t.Errorf("a dry run rewrote %s", tag)
		}
	}

	res := repair(false)
	if len(res.Releases) != 3 {
		t.Fatalf("repair changed %d releases, want 3: %+v", len(res.Releases), res)
	}
	for _, c := range res.Releases {
		if len(c.Removed) != 1 {
			t.Errorf("%s: %d rows dropped, want 1", c.Tag, len(c.Removed))
		}
	}
	wantReleaseRows(t, ms, ids["2026.303.2"], releaseStatusSuperseded, releaseRows{{"259", "2026-10-30T16:23:47Z", "2026-11-01T04:33:00Z"}})
	wantReleaseRows(t, ms, ids["2026.305.1"], releaseStatusSuperseded, releaseRows{{"263", "2026-11-01T04:33:00Z", "2026-11-01T17:14:35Z"}})
	wantReleaseRows(t, ms, ids["2026.305.2"], releaseStatusLive, releaseRows{{"264", "2026-11-01T17:14:35Z", ""}})

	repaired := snapshot()
	if again := repair(false); len(again.Releases) != 0 {
		t.Errorf("a second repair changed %+v", again.Releases)
	}
	for tag, f := range snapshot() {
		if f != repaired[tag] {
			t.Errorf("a second repair rewrote %s", tag)
		}
	}

	// The backfill with no --since then records 302.2's roll as history.
	rec := doJSON(t, h, http.MethodPost, "/v1/releases/"+ids["2026.302.2"]+"/deployments",
		map[string]any{"target": "prod", "since": "2026-10-29T16:13:00Z", "revision": "257", "by": "backfill"})
	if rec.Code != http.StatusOK {
		t.Fatalf("backfill 302.2: %d %s", rec.Code, rec.Body.String())
	}
	wantReleaseRows(t, ms, ids["2026.302.2"], releaseStatusSuperseded, releaseRows{{"257", "2026-10-29T16:13:00Z", "2026-10-30T16:23:47Z"}})
	wantReleaseRows(t, ms, ids["2026.305.2"], releaseStatusLive, releaseRows{{"264", "2026-11-01T17:14:35Z", ""}})

	// One revision on two releases: reported, not touched.
	seedReleaseRows(t, ms, ids["2026.302.2"], releaseRows{
		{"257", "2026-10-29T16:13:00Z", "2026-10-30T16:23:47Z", "backfill"},
		{"264", "2026-11-01T18:00:00Z", "", "owner"},
	})
	conflicted := repair(true)
	if len(conflicted.Conflicts) != 1 {
		t.Errorf("conflicts = %v, want rev 264 on 305.2 and 302.2", conflicted.Conflicts)
	}

	if rec := doJSON(t, h, http.MethodPost, "/v1/releases/repair-deployments", map[string]any{}); rec.Code != http.StatusBadRequest {
		t.Errorf("repair without a project: %d, want 400", rec.Code)
	}
}
