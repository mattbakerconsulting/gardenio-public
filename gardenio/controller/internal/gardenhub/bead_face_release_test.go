package gardenhub

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// releaseBeads adds the release face's beads (bead_face_release.go; gd-j3bQqfZSt7) to the bead view's reader: a release
// live on prod after a spell on staging, five PRs over two plans and one outside any plan, one of them with no PR number
// and one whose link is no link, and the release before it, which the panel titles.
func releaseBeads(beads *fakeBeads, now time.Time) {
	at := func(ago time.Duration) string { return now.Add(-ago).UTC().Format(time.RFC3339) }
	add := func(id, title string, ago time.Duration, fields map[string]string) {
		beads.beads[id] = &beadsapi.BeadDetail{ID: id, Type: "release", Status: "closed", Title: title, Labels: []string{"project:gardenio"}, Fields: fields, CreatedBy: "release-captain", CreatedAt: now.Add(-ago), UpdatedAt: now.Add(-ago)}
	}
	add("gd-FaceRelPrev1", "gardenio 2026.299.1", 30*time.Hour, map[string]string{"tag": "2026.299.1", "project": "gardenio", "status": "superseded"})
	add("gd-FaceRelease1", "gardenio 2026.299.2", 2*time.Hour, map[string]string{
		"tag": "2026.299.2", "project": "gardenio", "status": "live", "sha": "f883ae05576000a13123695ec69cce2e5097edaf", "source": "script", "cut_by": "release-captain",
		"cut_at": at(3 * time.Hour), "built_at": at(150 * time.Minute), "previous": "gd-FaceRelPrev1", "previous_tag": "2026.299.1", "contents_complete": "true",
		"images":      `[{"name":"controller","ref":"example/controller:2026.299.2"}]`,
		"deployments": `[{"target":"staging","kind":"k3s","since":"` + at(2*time.Hour) + `","until":"` + at(90*time.Minute) + `","via":"reconciler","revision":"88"},{"target":"prod","kind":"k3s","since":"` + at(time.Hour) + `","until":"","via":"reconciler","revision":"412","by":"observed"}]`,
		"contents": `[{"pr":57,"url":"https://github.com/acme/garden/pull/57","title":"feat(gardenhub): typed faces","merged_at":"` + at(4*time.Hour) + `","plan":"gd-FacePlan01","step":"gd-FaceWork01","task":"gd-FaceWork01"},` +
			`{"pr":56,"url":"https://github.com/acme/garden/pull/56","title":"fix(kbeads): close waits for every slice","merged_at":"` + at(5*time.Hour) + `","plan":"gd-FaceEpic01","task":"gd-FaceEpicK1"},` +
			`{"pr":55,"url":"https://github.com/acme/garden/pull/55","title":"fix(gardenhub): a golden for every face","merged_at":"` + at(6*time.Hour) + `","plan":"gd-FacePlan01","step":"gd-FaceWork01"},` +
			`{"merge_sha":"0ab3e780","merged_at":"` + at(7*time.Hour) + `","title":"deploy-values: turn on the Lands App setup leg"},{"pr":54,"url":"javascript:alert(1)","title":"chore: a row whose link is no link","merged_at":"` + at(8*time.Hour) + `"}]`})
}

// The release face: every row counted, grouped by plan with the biggest plan first and the unplanned rows last, the plans
// titled in the hydrate pass, a row's link only when it is http(s), the running deployment first, and the contents,
// deployments and images fields kept out of the fields table while the scalar ones stay.
func TestBeadFaceRelease(t *testing.T) {
	doc, err := readBead(context.Background(), faceFixture(), "gd-FaceRelease1")
	if err != nil || doc.Face == nil || doc.Face.Release == nil {
		t.Fatalf("release: %v %+v", err, doc)
	}
	r, keys := doc.Face.Release, []string{}
	for _, fd := range doc.Fields {
		if releaseFaceFields[fd.K] || fd.K == "tag" || fd.K == "sha" {
			keys = append(keys, fd.K)
		}
	}
	got := fmt.Sprintf("%s %s n=%d more=%d incomplete=%v cut=%v built=%v prev=%s fields=%v", r.Tag, r.Status, r.Count, r.More, r.Incomplete, r.CutAt > 0, r.BuiltAt > 0, r.Previous.Title, keys)
	if want := "2026.299.2 live n=5 more=0 incomplete=false cut=true built=true prev=gardenio 2026.299.1 fields=[sha tag]"; got != want {
		t.Errorf("release face:\n got %s\nwant %s", got, want)
	}
	var plans, deps []string
	for _, g := range r.Plans {
		rows := []string{}
		for _, x := range g.Rows {
			rows = append(rows, fmt.Sprintf("#%d:%s:%s:%v", x.PR, x.URL, x.Step, x.MergedAt > 0))
		}
		plan := "none"
		if g.Plan != nil {
			plan = g.Plan.ID + "(" + g.Plan.Title + ")"
		}
		plans = append(plans, plan+g.ID+" "+strings.Join(rows, " "))
	}
	for _, d := range r.Deployments {
		deps = append(deps, fmt.Sprintf("%s:%v", d.Target, d.Until > 0))
	}
	want := []string{"gd-FacePlan01(Lands: the game, Track A) #57:https://github.com/acme/garden/pull/57:gd-FaceWork01:true #55:https://github.com/acme/garden/pull/55:gd-FaceWork01:true",
		"gd-FaceEpic01(5. Workbench: beads, plans cockpit, bundles) #56:https://github.com/acme/garden/pull/56:gd-FaceEpicK1:true", "none #0:::true #54::" + ":true"}
	if !reflect.DeepEqual(plans, want) || !reflect.DeepEqual(deps, []string{"prod:false", "staging:true"}) {
		t.Errorf("release plans and deployments:\n%s\n%v", strings.Join(plans, "\n"), deps)
	}
	if rows, _, _ := releaseFacePlans(`[{"pr":1,"plan":"p"},{"pr":2},{"pr":3,"plan":"p"},{"pr":4,"plan":"q"}]`, func(id string) *beadRel { return &beadRel{ID: id} }); len(rows) != 3 || rows[0].Plan.ID != "p" || rows[1].Plan.ID != "q" || rows[2].Plan != nil {
		t.Errorf("plans by size, unplanned last: %+v", rows)
	}
}

// web/relface.js under node: the face says it is live and where, the ages of its cut and build, the release before it as a
// bead link, the PRs counted and listed by plan with a javascript: url left unlinked, and the changelog link only once the
// release kind registers (PR #1898).
func TestReleaseFaceUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	doc, err := readBead(context.Background(), faceFixture(), "gd-FaceRelease1")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(doc)
	script := `
var V = require(process.argv[1]), assert = require("assert"), D = JSON.parse(process.argv[2]);
require(process.argv[3]).install(V);
assert.ok(V.FACES.release, "the release face installed");
function ctx(has) { return { id: "gd-FaceRelease1", now: 1790000000, base: "/v", has: function (k) { return (has || []).indexOf(k) >= 0; } }; }
function text(parts) { return (parts || []).map(function (p) { return typeof p === "string" ? p : p.t || p.sm || p.b || ""; }).join(""); }
function row(v, k) { var r = (v.rows || []).filter(function (r) { return r[0] === k; })[0]; return r ? text(r[1]) : null; }
var rl = V.faceView(D, ctx());
assert.deepStrictEqual([rl.h, rl.state, rl.tone], ["Release 2026.299.2", "Live. Runs on prod since 1h ago.", "ok"]);
assert.deepStrictEqual([row(rl, "Cut"), row(rl, "Built"), row(rl, "Previous"), row(rl, "Ran on"), row(rl, "Contents")],
  ["3h ago by release-captainvia script", "3h ago", "gardenio 2026.299.1", "staging for 30m until 2h ago", "5 PRs from 2 plans"]);
assert.strictEqual(row(rl, "By plan"), "Lands: the game, Track A (2) · 5. Workbench: beads, plans cockpit, bundles (1) · Outside any plan (2)");
assert.deepStrictEqual([rl.items.length, text(rl.items[0].parts), text(rl.items[4].parts)], [5, "#57 feat(gardenhub): typed facesLands: the game, Track A", "#54 chore: a row whose link is no linkno plan"]);
assert.deepStrictEqual(rl.items.map(function (it) { return (it.parts[0] || {}).url || ""; }), ["https://github.com/acme/garden/pull/57", "https://github.com/acme/garden/pull/56", "https://github.com/acme/garden/pull/55", "", ""], "newest merge first");
assert.strictEqual(rl.links, undefined, "no changelog link before the release kind registers");
assert.strictEqual(V.faceView(D, ctx(["release"])).links[0].go, "/v/garden?release=gd-FaceRelease1");
assert.strictEqual(V.kindWord(D, "release"), "release");
console.log("ok");
`
	js, _ := filepath.Abs(filepath.Join("web", "workbench.js"))
	rel, _ := filepath.Abs(filepath.Join("web", "relface.js"))
	out, err := exec.Command(node, "-e", script, js, string(raw), rel).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
