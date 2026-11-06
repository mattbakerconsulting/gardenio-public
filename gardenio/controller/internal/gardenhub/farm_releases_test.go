package gardenhub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// ReleaseProjects and ListReleases make the fake a ReleaseSource, as the daemon's /v1/releases serves it: newest cut
// first, cut at limit.
func (f *fakeFarmSrc) ReleaseProjects(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.relReads++
	if f.failReleases {
		return nil, errors.New("release projects read failed")
	}
	var out []string
	for p := range f.releases {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

func (f *fakeFarmSrc) ListReleases(_ context.Context, project, status string, limit int) ([]beadsapi.Release, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.relReads++
	if f.failReleases {
		return nil, errors.New("release read failed")
	}
	var out []beadsapi.Release
	for _, r := range f.releases[project] {
		if status == "" || r.Status == status {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CutAt > out[j].CutAt })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func relISO(d int64) string { return time.Unix(baseUnix+d, 0).UTC().Format(time.RFC3339) }

// relContents is a release's contents rows: url, plan ("" = unplanned), merged_at offset from baseUnix.
func relContents(rows ...[3]any) json.RawMessage {
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, map[string]any{"url": r[0], "plan": r[1], "merged_at": relISO(r[2].(int64))})
	}
	raw, _ := json.Marshal(out)
	return raw
}

// addFixtureReleases gives the fixture three gardenio releases (releases-design.md §3): 2026.299.1 ran on prod and was
// replaced (superseded), 2026.300.1 runs there now (live) and carries plan A's merged MR !405 (so plan A has shipped),
// and 2026.301.1 is cut and not yet deployed (the truck is loading it). They are served by the fake's ReleaseSource,
// not as beads, so no other read of the fixture moves.
func addFixtureReleases(f *fakeFarmSrc) {
	f.releases = map[string][]beadsapi.Release{"gardenio": {
		{
			ID: "gd-rel1", Title: "gardenio 2026.299.1", Project: "gardenio", Tag: "2026.299.1", CutAt: relISO(-2 * 86_400), BuiltAt: relISO(-2*86_400 + 1_800),
			Contents: relContents([3]any{"https://example.com/mr/398", "gd-planA", int64(-2*86_400 - 600)}),
			Deployments: []beadsapi.ReleaseDeployment{{Target: "prod", Kind: "k3s", Since: relISO(-2*86_400 + 3_600), Until: relISO(7_200),
				By: "gardenio-release-deployer", Via: "script", Revision: "410", ImageTag: "2026.299.1"}},
			Status: beadsapi.ReleaseStatusSuperseded,
		},
		{
			ID: "gd-rel2", Title: "gardenio 2026.300.1", Project: "gardenio", Tag: "2026.300.1", CutAt: relISO(3_600), BuiltAt: relISO(5_400),
			Contents: relContents(
				[3]any{"https://example.com/mr/405", "gd-planA", int64(21)},
				[3]any{"https://example.com/pr/300", "gd-done1", int64(-600)},
				[3]any{"https://example.com/pr/301", "", int64(-300)}),
			Deployments: []beadsapi.ReleaseDeployment{{Target: "prod", Kind: "k3s", Since: relISO(7_200), By: "gardenio-release-deployer", Via: "script",
				Revision: "411", ImageTag: "2026.300.1"}},
			Status: beadsapi.ReleaseStatusLive,
		},
		{
			ID: "gd-rel3", Title: "gardenio 2026.301.1", Project: "gardenio", Tag: "2026.301.1", CutAt: relISO(9_000),
			Contents: relContents(
				[3]any{"https://example.com/pr/310", "gd-done1", int64(8_000)},
				[3]any{"https://example.com/pr/311", "gd-done1", int64(8_100)},
				[3]any{"https://example.com/pr/312", "gd-seed2", int64(8_200)},
				[3]any{"https://example.com/pr/313", "", int64(8_300)}),
			Deployments: []beadsapi.ReleaseDeployment{},
			Status:      beadsapi.ReleaseStatusCut,
		},
	}}
}

func releasesFarm(t *testing.T, src *fakeFarmSrc) *Farm {
	t.Helper()
	f := newTestFarm(src)
	f.SetReleaseSource(src)
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return f
}

// TestFarmReleases pins the releases blob on the fixture: the newest release on the truck with its plans, what runs on
// each target, the recent list newest cut first, and the plan whose newest merged PR shipped.
func TestFarmReleases(t *testing.T) {
	f := releasesFarm(t, fixtureWorld())
	s := decodeSnap(t, f)
	pr, ok := s.Releases["gardenio"]
	if !ok || len(s.Releases) != 1 {
		t.Fatalf("releases = %+v, want gardenio's only", s.Releases)
	}
	lt := pr.Latest
	if lt == nil || lt.ID != "gd-rel3" || lt.Tag != "2026.301.1" || lt.Status != "cut" || lt.CutAt != baseUnix+9_000 || lt.BuiltAt != 0 || lt.LiveSince != 0 {
		t.Fatalf("latest = %+v", lt)
	}
	if lt.Count != 4 || lt.Unplanned != 1 || !slices.Equal(lt.Plans, []FarmReleasePlan{{ID: "gd-done1", Count: 2}, {ID: "gd-seed2", Count: 1}}) {
		t.Fatalf("latest's load = %d PRs, %d unplanned, plans %+v", lt.Count, lt.Unplanned, lt.Plans)
	}
	if !pr.Shipping {
		t.Fatal("a cut newest release is shipping")
	}
	want := []FarmReleaseDeployment{{Target: "prod", Kind: "k3s", Tag: "2026.300.1", Release: "gd-rel2", Since: baseUnix + 7_200, Revision: "411"}}
	if !slices.Equal(pr.Deployments, want) {
		t.Fatalf("deployments = %+v, want %+v", pr.Deployments, want)
	}
	var tags []string
	for _, r := range pr.Recent {
		tags = append(tags, r.Tag+":"+r.Status+fmt.Sprintf(":%d/%d", r.Count, r.Plans))
	}
	if got := strings.Join(tags, " "); got != "2026.301.1:cut:4/2 2026.300.1:live:3/2 2026.299.1:superseded:1/1" {
		t.Fatalf("recent = %s", got)
	}
	if p := findPlan(s, "gd-planA"); p == nil || p.Shipped != "2026.300.1" {
		t.Fatalf("plan A's newest merged MR !405 shipped in 2026.300.1: %+v", p)
	}
}

// A live newest release is not shipping, and says since when it runs; a newer cut makes it the one on the road again.
func TestFarmReleasesLive(t *testing.T) {
	src := fixtureWorld()
	src.releases["gardenio"] = src.releases["gardenio"][:2]
	f := releasesFarm(t, src)
	pr := decodeSnap(t, f).Releases["gardenio"]
	if pr.Shipping || pr.Latest == nil || pr.Latest.Tag != "2026.300.1" || pr.Latest.LiveSince != baseUnix+7_200 || pr.Latest.BuiltAt != baseUnix+5_400 {
		t.Fatalf("live newest release: %+v %+v", pr.Latest, pr)
	}
}

// A plan whose newest merged PR is in no release has not shipped, even when an older PR of it has.
func TestFarmReleasesShippedNeedsTheNewestPR(t *testing.T) {
	src := fixtureWorld()
	src.mutate("gd-A1x", func(b *beadsapi.BeadDetail) {
		b.UpdatedAt = time.Unix(baseUnix+30, 0) // newer than !405
		b.Fields["mr_url"], b.Fields["mr_iid"], b.Fields["mr_state"] = "https://example.com/mr/420", "!420", "merged"
	})
	f := releasesFarm(t, src)
	if p := findPlan(decodeSnap(t, f), "gd-planA"); p == nil || p.Shipped != "" {
		t.Fatalf("plan A's newest merged MR !420 is in no release: %+v", p)
	}
}

// A plan with no merged MR in its bin falls back to its newest PR in any release; a release of another project never
// answers for it.
func TestShippedIndexFallback(t *testing.T) {
	_, ix := buildReleases(map[string][]beadsapi.Release{
		"gardenio": {
			{Tag: "b", Contents: relContents([3]any{"u/2", "gd-p", int64(2)})},
			{Tag: "a", Contents: relContents([3]any{"u/1", "gd-p", int64(1)}, [3]any{"u/3", "gd-p", int64(3)})},
		},
		"zeta": {{Tag: "z", Contents: relContents([3]any{"u/9", "gd-z", int64(9)})}},
	})
	if got, _ := ix.of("gd-p", "gardenio", nil); got != "a" {
		t.Fatalf("newest merged row u/3 is in a, got %q", got)
	}
	if got, _ := ix.of("gd-z", "gardenio", nil); got != "" {
		t.Fatalf("zeta's release answered for a gardenio plan: %q", got)
	}
	if got, _ := ix.of("gd-p", "gardenio", []FarmProduce{{Kind: ProduceMR, Ready: false, URL: "u/7"}, {Kind: ProduceMR, Ready: true, URL: "u/2/"}}); got != "b" {
		t.Fatalf("the bin's newest merged MR u/2 is in b, got %q", got)
	}

	// A merged PR on the open release is boarded, not shipped; one a cut
	// release carries still answers with its tag.
	ix.board(map[string]beadsapi.Release{"gardenio": {Contents: relContents([3]any{"u/4", "gd-q", int64(4)}, [3]any{"u/5", "gd-p", int64(5)})}})
	for _, tc := range []struct {
		plan    string
		produce []FarmProduce
		tag     string
		boarded bool
	}{
		{"gd-q", nil, "", true},
		{"gd-p", nil, "", true},
		{"gd-p", []FarmProduce{{Kind: ProduceMR, Ready: true, URL: "u/5"}}, "", true},
		{"gd-p", []FarmProduce{{Kind: ProduceMR, Ready: true, URL: "u/2"}}, "b", false},
		{"gd-z", nil, "", false},
	} {
		if tag, boarded := ix.of(tc.plan, "gardenio", tc.produce); tag != tc.tag || boarded != tc.boarded {
			t.Errorf("of(%s, %v) = %q, %v; want %q, %v", tc.plan, tc.produce, tag, boarded, tc.tag, tc.boarded)
		}
	}
}

// The read is bounded: the newest maxReleasesPerProject per project, and a target held only by an older release is not
// listed.
func TestFarmReleasesCap(t *testing.T) {
	src := fixtureWorld()
	var rels []beadsapi.Release
	for i := range maxReleasesPerProject + 3 {
		r := beadsapi.Release{ID: fmt.Sprintf("gd-r%02d", i), Tag: fmt.Sprintf("2026.%03d.1", 200+i), CutAt: relISO(int64(i) * 100), Status: "built", Deployments: []beadsapi.ReleaseDeployment{}}
		if i == 0 {
			r.Status, r.Deployments = "live", []beadsapi.ReleaseDeployment{{Target: "staging", Since: relISO(50)}}
		}
		rels = append(rels, r)
	}
	src.releases = map[string][]beadsapi.Release{"gardenio": rels, "zeta": nil}
	f := releasesFarm(t, src)
	s := decodeSnap(t, f)
	pr := s.Releases["gardenio"]
	if len(pr.Recent) != maxReleasesPerProject || pr.Recent[0].Tag != "2026.214.1" || pr.Recent[maxReleasesPerProject-1].Tag != "2026.203.1" {
		t.Fatalf("recent = %d rows, %+v", len(pr.Recent), pr.Recent)
	}
	if len(pr.Deployments) != 0 || !pr.Shipping {
		t.Fatalf("the staging holder is past the cap; deployments %+v", pr.Deployments)
	}
	if z, ok := s.Releases["zeta"]; !ok || z.Latest != nil || z.Recent == nil || z.Deployments == nil || z.Shipping {
		t.Fatalf("a project with no release yet stands empty: %+v", z)
	}
}

// The releases are read at most every farmReleasesEvery; a failed read keeps the last one; without a source there is
// no read, the blob is {} and no plan has shipped.
func TestFarmReleasesCadenceAndFailure(t *testing.T) {
	src := fixtureWorld()
	f := releasesFarm(t, src)
	reads := src.relReads
	if reads != 2 {
		t.Fatalf("one project list and one release list per project, got %d reads", reads)
	}
	start := f.slow.at
	f.now = func() time.Time { return start.Add(farmSlowEvery + time.Second) }
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if src.relReads != reads {
		t.Fatalf("a slow tick inside farmReleasesEvery re-read the releases (%d reads)", src.relReads-reads)
	}
	src.failReleases = true
	f.now = func() time.Time { return start.Add(farmReleasesEvery + time.Second) }
	if err := f.refresh(context.Background()); err != nil {
		t.Fatalf("a failed release read must not fail the refresh: %v", err)
	}
	if src.relReads == reads {
		t.Fatal("the releases were not re-read after farmReleasesEvery")
	}
	if pr := decodeSnap(t, f).Releases["gardenio"]; pr.Latest == nil || pr.Latest.Tag != "2026.301.1" {
		t.Fatalf("a failed read must keep the last releases: %+v", pr)
	}

	bare := newTestFarm(fixtureWorld())
	if err := bare.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, raw, _ := bare.Snapshot()
	var doc map[string]json.RawMessage
	_ = json.Unmarshal(raw, &doc)
	if string(doc["releases"]) != emptyReleases {
		t.Fatalf("no release source: releases = %s", doc["releases"])
	}
	if p := findPlan(decodeSnap(t, bare), "gd-planA"); p == nil || p.Shipped != "" {
		t.Fatalf("no release source, nothing shipped: %+v", p)
	}
}

// A release going live rides the next patch as the blob's full new value.
func TestFarmReleasesPatch(t *testing.T) {
	src := fixtureWorld()
	f := releasesFarm(t, src)
	_, live, cancel := f.Subscribe(0)
	defer cancel()
	src.mu.Lock()
	r := &src.releases["gardenio"][2]
	r.Status, r.Deployments = beadsapi.ReleaseStatusLive, []beadsapi.ReleaseDeployment{{Target: "prod", Kind: "k3s", Since: relISO(9_500), Revision: "412"}}
	src.mu.Unlock()
	start := f.relSt.at
	f.now = func() time.Time { return start.Add(farmReleasesEvery + time.Second) }
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-live:
		var p struct {
			Releases FarmReleases `json:"releases"`
		}
		if err := json.Unmarshal(msg.Data, &p); err != nil || p.Releases == nil {
			t.Fatalf("patch should carry releases: %v %s", err, msg.Data)
		}
		pr := p.Releases["gardenio"]
		if pr.Shipping || pr.Latest.Status != "live" || len(pr.Deployments) != 1 || pr.Deployments[0].Tag != "2026.301.1" || pr.Deployments[0].Revision != "412" {
			t.Fatalf("after the deploy: %+v", pr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no patch")
	}
}

// A scoped viewer gets only its projects' releases.
func TestFarmReleasesCut(t *testing.T) {
	src := multiLandWorld()
	src.releases["zeta"] = []beadsapi.Release{{ID: "gd-zrel", Project: "zeta", Tag: "v1.0.0", CutAt: relISO(100), Status: "cut", Deployments: []beadsapi.ReleaseDeployment{}}}
	f := releasesFarm(t, src)
	for _, tc := range []struct {
		projects []string
		want     string
	}{{[]string{"zeta"}, "zeta"}, {[]string{"gardenio"}, "gardenio"}, {nil, ""}} {
		_, body, _, _ := f.CutSnapshot(FarmCut{Scoped: true, Projects: tc.projects})
		var doc struct {
			Releases FarmReleases `json:"releases"`
		}
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Fatal(err)
		}
		var keys []string
		for k := range doc.Releases {
			keys = append(keys, k)
		}
		if strings.Join(keys, ",") != tc.want {
			t.Fatalf("cut to %v: releases of %v", tc.projects, keys)
		}
	}
}

// The open release (no tag yet, plan gd-lgN4U7X5Tb) is not one of the truck's
// cut releases: latest, recent and the shipped index stay as they were.
func TestFarmReleasesLeavesTheOpenReleaseOff(t *testing.T) {
	src := fixtureWorld()
	src.releases["gardenio"] = append(src.releases["gardenio"], beadsapi.Release{
		ID: "gd-next", Project: "gardenio", Status: beadsapi.ReleaseStatusOpen,
		Contents: relContents([3]any{"https://example.com/pr/320", "gd-planA", int64(9_500)}),
	})
	s := decodeSnap(t, releasesFarm(t, src))
	pr := s.Releases["gardenio"]
	if pr.Latest == nil || pr.Latest.ID != "gd-rel3" || len(pr.Recent) != 3 {
		t.Fatalf("latest = %+v, recent = %+v, want gd-rel3 and the three cut releases", pr.Latest, pr.Recent)
	}
	if p := findPlan(s, "gd-planA"); p == nil || p.Shipped != "2026.300.1" {
		t.Fatalf("plan A shipped = %+v, want 2026.300.1 (a PR on the open release has not shipped)", p)
	}
}
