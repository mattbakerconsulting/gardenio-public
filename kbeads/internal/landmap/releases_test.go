package landmap

import (
	"net/http"
	"slices"
	"sort"
	"strconv"
	"testing"

	"gardenio/lands/contract/landsv1"
)

// seedReleases puts acme's three releases and one of another land into d.
// Each carries keys the contract has no word for (cut_by, source, images,
// a row's mr_bead and via), which must never reach the land.
func seedReleases(d *fakeDaemon) {
	row := func(pr int, title, step string) map[string]any {
		return map[string]any{"pr": pr, "title": title, "url": "https://github.com/acme/shop/pull/" + strconv.Itoa(pr),
			"step": step, "plan": "ac-p1", "mr_bead": "ac-mr" + strconv.Itoa(pr), "merge_sha": "abc"}
	}
	release := func(id, project, tag, cutAt, status string, contents, deployments []any) {
		d.add(map[string]any{"id": id, "type": "release", "kind": "data", "status": "closed", "title": project + " " + tag,
			"labels": []any{"project:" + project},
			"fields": map[string]any{"project": project, "tag": tag, "cut_at": cutAt, "status": status,
				"sha": "4b825dc642cb6eb9a060e54bf8d69288fbee4904", "cut_by": "oidc:op@example.com", "source": "webhook",
				"images":   []any{map[string]any{"name": "shop", "ref": "registry.internal/shop:" + tag}},
				"contents": contents, "deployments": deployments}})
	}
	release("ac-r1", "acme", "v1.0.0", "2026-10-20T09:00:00.250Z", "superseded",
		[]any{row(1, "First shelf", "ac-s1")},
		[]any{map[string]any{"target": "prod", "kind": "land", "since": "2026-10-20T10:00:00Z", "until": "2026-10-24T09:30:00Z",
			"by": "oidc:al@example.com", "via": "contract", "revision": "7", "image_tag": "v1.0.0"}})
	release("ac-r2", "acme", "v1.1.0", "2026-10-24T09:00:00Z", "live",
		[]any{
			row(2, "Price label, see zz-Tkt000Abc1 for the old one", "zz-s9"),
			map[string]any{"title": "a direct commit", "merge_sha": "def"},
			map[string]any{"pr": 3, "title": "no address"},
		},
		[]any{map[string]any{"target": "prod", "kind": "land", "since": "2026-10-24T09:30:00Z", "until": "",
			"by": "oidc:al@example.com", "via": "contract", "revision": "8"}})
	release("ac-r3", "acme", "v1.2.0", "2026-10-25T08:00:00Z", "cut", []any{row(4, "Autumn banner", "ac-s1")}, []any{})
	release("zz-r1", "zeta", "v9.0.0", "2026-10-26T08:00:00Z", "cut", []any{}, []any{})
}

// releaseView is a seeded release as the daemon's release routes answer it:
// its fields flattened beside its id and title.
func (d *fakeDaemon) releaseView(b map[string]any) map[string]any {
	v := map[string]any{"id": b["id"], "title": b["title"], "created_at": t0, "updated_at": t0}
	for k, x := range b["fields"].(map[string]any) {
		v[k] = x
	}
	return v
}

// release serves the three release routes. Another land's release is out
// of the caller's scope, as the daemon's read gate answers it.
func (d *fakeDaemon) release(id routeID, w http.ResponseWriter, r *http.Request, body map[string]any) {
	project := r.URL.Query().Get("project")
	find := func(ref string) (map[string]any, int) {
		b, ok := d.beads[ref]
		if !ok && project != "" {
			for _, x := range d.beads {
				if f, _ := x["fields"].(map[string]any); x["type"] == "release" && f["project"] == project && f["tag"] == ref {
					b, ok = x, true
				}
			}
		}
		if !ok || b["type"] != "release" {
			return nil, http.StatusNotFound
		}
		if b["fields"].(map[string]any)["project"] != "acme" {
			return nil, http.StatusForbidden
		}
		return b, http.StatusOK
	}
	switch id {
	case rListReleases:
		var out []map[string]any
		for _, b := range d.beads {
			if f, _ := b["fields"].(map[string]any); b["type"] == "release" && f["project"] == project && project == "acme" {
				out = append(out, d.releaseView(b))
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i]["cut_at"].(string) > out[j]["cut_at"].(string) })
		if lim, _ := strconv.Atoi(r.URL.Query().Get("limit")); lim > 0 && len(out) > lim {
			out = out[:lim]
		}
		if out == nil {
			out = []map[string]any{}
		}
		writeTestJSON(w, http.StatusOK, map[string]any{"releases": out})
	case rGetRelease:
		b, st := find(r.PathValue("id"))
		if st != http.StatusOK {
			http.Error(w, `{"error":"release not found"}`, st)
			return
		}
		writeTestJSON(w, http.StatusOK, d.releaseView(b))
	case rDeployRelease:
		b, st := find(r.PathValue("id"))
		if st != http.StatusOK {
			http.Error(w, `{"error":"release not found"}`, st)
			return
		}
		if body["target"] != "prod" {
			http.Error(w, `{"error":"release deploy: project \"acme\" has no release target"}`, http.StatusBadRequest)
			return
		}
		f := b["fields"].(map[string]any)
		row := map[string]any{"target": "prod", "kind": "land", "since": "2026-10-31T16:00:00Z", "until": "",
			"by": "oidc:al@example.com", "via": body["via"], "revision": body["revision"]}
		deps, _ := f["deployments"].([]any)
		f["deployments"], f["status"] = append(deps, row), "live"
		writeTestJSON(w, http.StatusOK, d.releaseView(b))
	}
}

func TestReleases_ListNewestFirstPagedAndOnlyTheLand(t *testing.T) {
	d, h := newMapFixture(t, fakeLands{})
	seedReleases(d)
	first := decodeAs[landsv1.ReleasePage](t, do(t, h, "GET", "/acme/releases?limit=2", ""), http.StatusOK)
	if tags := releaseTags(first.Items); !slices.Equal(tags, []string{"v1.2.0", "v1.1.0"}) || first.NextCursor == nil {
		t.Fatalf("page 1 = %v next %v, want v1.2.0 v1.1.0 and a cursor", tags, first.NextCursor)
	}
	last := decodeAs[landsv1.ReleasePage](t, do(t, h, "GET", "/acme/releases?limit=2&cursor="+*first.NextCursor, ""), http.StatusOK)
	if tags := releaseTags(last.Items); !slices.Equal(tags, []string{"v1.0.0"}) || last.NextCursor != nil {
		t.Errorf("page 2 = %v next %v, want v1.0.0 and no cursor", tags, last.NextCursor)
	}
	for _, c := range d.calls {
		if c.route == routeTable[rListReleases] && c.query != "limit=3&project=acme" && c.query != "limit=5&project=acme" {
			t.Errorf("list read %q, want the land's project and through the page's end", c.query)
		}
	}
	problemOf(t, do(t, h, "GET", "/zeta/releases", ""), landsv1.NotFound)
	problemOf(t, do(t, h, "GET", "/acme/releases?limit=0", ""), landsv1.InvalidRequest)
}

func releaseTags(rs []landsv1.Release) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Tag
	}
	return out
}

func TestReleases_GetProjectsTheContractOnly(t *testing.T) {
	d, h := newMapFixture(t, fakeLands{})
	seedReleases(d)
	rel := decodeAs[landsv1.Release](t, do(t, h, "GET", "/acme/releases/v1.1.0", ""), http.StatusOK)
	if rel.Id != "ac-r2" || rel.Status != landsv1.Live || rel.Sha == nil || rel.CutAt == nil {
		t.Fatalf("release = %+v", rel)
	}
	// The direct commit and the row with no address are not pull requests;
	// another land's step is not the land's to see, and its id in prose reads [ref].
	if len(rel.Contents) != 1 {
		t.Fatalf("contents = %+v, want the one pull request", rel.Contents)
	}
	it := rel.Contents[0]
	if it.Pr != 2 || it.Step != nil || it.Plan == nil || *it.Plan != "ac-p1" || it.Title != "Price label, see [ref] for the old one" {
		t.Errorf("item = %+v", it)
	}
	if len(rel.Deployments) != 1 {
		t.Fatalf("deployments = %+v", rel.Deployments)
	}
	dep := rel.Deployments[0]
	if dep.Target != "prod" || dep.Until != nil || dep.By == nil || *dep.By != "al" || dep.Revision == nil || *dep.Revision != "8" {
		t.Errorf("deployment = %+v", dep)
	}

	old := decodeAs[landsv1.Release](t, do(t, h, "GET", "/acme/releases/ac-r1", ""), http.StatusOK)
	if old.Status != landsv1.Superseded || len(old.Deployments) != 1 || old.Deployments[0].Until == nil {
		t.Errorf("superseded release = %+v", old)
	}
	if old.CutAt == nil || old.CutAt.Nanosecond() != 0 {
		t.Errorf("cut_at = %v, want whole seconds", old.CutAt)
	}
	empty := decodeAs[landsv1.Release](t, do(t, h, "GET", "/acme/releases/v1.2.0", ""), http.StatusOK)
	if empty.Deployments == nil || len(empty.Deployments) != 0 {
		t.Errorf("a release deployed nowhere has deployments %v, want []", empty.Deployments)
	}

	problemOf(t, do(t, h, "GET", "/acme/releases/v7.0.0", ""), landsv1.NotFound)
	problemOf(t, do(t, h, "GET", "/acme/releases/zz-r1", ""), landsv1.NotFound)
	problemOf(t, do(t, h, "GET", "/acme/releases/ac-t1", ""), landsv1.NotFound)
}

func TestReleases_ReportDeployment(t *testing.T) {
	d, h := newMapFixture(t, fakeLands{})
	seedReleases(d)
	rel := decodeAs[landsv1.Release](t, do(t, h, "POST", "/acme/releases/v1.2.0/deployments", `{"target":"prod","revision":"9","by":"shop-deploy"}`), http.StatusOK)
	if rel.Id != "ac-r3" || rel.Status != landsv1.Live || len(rel.Deployments) != 1 {
		t.Fatalf("deployed = %+v", rel)
	}
	var sent map[string]any
	for _, c := range d.calls {
		if c.route == routeTable[rDeployRelease] {
			sent = c.body
			if c.path != "/v1/releases/ac-r3/deployments" {
				t.Errorf("deployed %s, want the release read by its tag first", c.path)
			}
		}
	}
	if sent["via"] != "contract" || sent["revision"] != "9" || sent["by"] != "shop-deploy" || sent["target"] != "prod" {
		t.Errorf("deploy body = %v", sent)
	}

	problemOf(t, do(t, h, "POST", "/acme/releases/v1.2.0/deployments", `{"target":"staging"}`), landsv1.Conflict)
	problemOf(t, do(t, h, "POST", "/acme/releases/v1.2.0/deployments", `{"target":"prod","since":"2026-10-01T00:00:00Z"}`), landsv1.InvalidRequest)
	problemOf(t, do(t, h, "POST", "/acme/releases/v1.2.0/deployments", `{"target":""}`), landsv1.InvalidRequest)
	problemOf(t, do(t, h, "POST", "/acme/releases/zz-r1/deployments", `{"target":"prod"}`), landsv1.NotFound)
	problemOf(t, do(t, h, "POST", "/acme/releases/v7.0.0/deployments", `{"target":"prod"}`), landsv1.NotFound)
	d.forced[rDeployRelease] = http.StatusForbidden
	problemOf(t, do(t, h, "POST", "/acme/releases/v1.2.0/deployments", `{"target":"prod"}`), landsv1.Forbidden)

	paused, ph := newMapFixture(t, fakeLands{paused: true})
	seedReleases(paused)
	problemOf(t, do(t, ph, "POST", "/acme/releases/v1.2.0/deployments", `{"target":"prod"}`), landsv1.LandPaused)
}

func TestFeed_Releases(t *testing.T) {
	d, c, _, h := newFeedFixture(t)
	seedReleases(d)
	s := openStream(t, h, "/acme/events")

	c.publish(ChangeUpdated, "ac-r3", "project:acme") // an edit with no deployments: nothing
	c.publish(ChangeClosed, "ac-r3", "project:acme")  // born closed: nothing
	c.publish(ChangeCreated, "zz-r1", "project:zeta") // another land
	c.publish(ChangeCreated, "ac-r3", "project:acme") // release.cut
	c.publish(ChangeUpdated, "ac-r2", "project:acme") // release.deployed

	f := s.next(t)
	if r := decodeObject[landsv1.Release](t, f); f.event != string(landsv1.ReleaseCut) || r.Id != "ac-r3" || r.Status != landsv1.Cut {
		t.Errorf("frame 1 = %+v", f)
	}
	f = s.next(t)
	if r := decodeObject[landsv1.Release](t, f); f.event != string(landsv1.ReleaseDeployed) || r.Id != "ac-r2" || len(r.Deployments) != 1 {
		t.Errorf("frame 2 = %+v", f)
	}
	s.quiet(t)
	s.hygiene(t)
}
