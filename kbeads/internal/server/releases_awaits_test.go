package server

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

const testPR = "https://github.com/mattbakerconsulting/gardenio/pull/"

// seedAwaitingBead puts an open bead of type typ in project gardenio with
// fields into ms.
func seedAwaitingBead(ms *mockStore, id string, typ model.BeadType, fields string) {
	ms.beads[id] = &model.Bead{
		ID: id, Title: id, Type: typ, Kind: model.KindIssue, Status: model.StatusInProgress,
		Fleet: "default", Fields: json.RawMessage(fields),
	}
	ms.labels[id] = []string{"project:gardenio"}
}

// seedMR puts a merge-request bead for PR n into ms, produced by task.
func seedMR(ms *mockStore, id, task string, n string, merged bool, sha string) {
	f := map[string]any{"mr_url": testPR + n, "merge_sha": sha}
	status := model.StatusOpen
	if merged {
		f["mr_merged"] = "true"
		status = model.StatusClosed
	}
	raw, _ := json.Marshal(f)
	ms.beads[id] = &model.Bead{ID: id, Title: "PR " + n, Type: mergeRequestType, Kind: model.KindData, Status: status, Fleet: "default", Fields: raw}
	ms.deps[task] = append(ms.deps[task], &model.Dependency{BeadID: task, DependsOnID: id, Type: producesDepType})
}

func TestAwaitsRelease_FieldDeclared(t *testing.T) {
	s, _, _ := newTestServer()
	for _, typ := range []model.BeadType{model.TypeTask, model.TypeBug, model.TypeFeature, model.TypeChore, model.TypeEscalation} {
		tc, err := s.resolveTypeConfig(context.Background(), typ)
		if err != nil || tc == nil {
			t.Fatalf("%s: resolveTypeConfig: %v", typ, err)
		}
		if !slices.ContainsFunc(tc.Fields, func(f model.FieldDef) bool {
			return f.Name == awaitsReleaseField && f.Type == model.FieldTypeBoolean
		}) {
			t.Errorf("%s does not declare boolean %s", typ, awaitsReleaseField)
		}
	}
}

// A deployment closes the awaits_release work that the deployed release or a
// release on its previous chain carries, and nothing else.
func TestAwaitsRelease_DeploymentClosesCarriedWork(t *testing.T) {
	_, ms, h := newTestServer()
	seedReleaseProject(ms, "proj-gardenio", "gardenio", testGardenioReleaseSource)
	row := func(n, sha, mrBead, task string) map[string]any {
		return map[string]any{"pr": n, "url": testPR + n, "merge_sha": sha, "mr_bead": mrBead, "task": task}
	}
	_, r1 := createReleaseAsAdmin(t, h, map[string]any{"project": "gardenio", "tag": "2026.300.1", "cut_at": "2026-10-27T04:00:00Z",
		"contents": []any{row("1", "sha1", "", "gd-t1")}})
	_, r2 := createReleaseAsAdmin(t, h, map[string]any{"project": "gardenio", "tag": "2026.300.2", "cut_at": "2026-10-27T16:00:00Z",
		"contents": []any{row("2", "sha2", "gd-mr2", "gd-t2"), row("5", "sha5", "", "gd-t5")}})
	_, r3 := createReleaseAsAdmin(t, h, map[string]any{"project": "gardenio", "tag": "2026.301.1", "cut_at": "2026-10-28T04:00:00Z",
		"contents": []any{row("3", "sha3", "", "gd-t3")}})
	if r1.ID == "" || r2.Previous != r1.ID || r3.Previous != r2.ID {
		t.Fatalf("release chain: r1=%q r2.previous=%q r3.previous=%q", r1.ID, r2.Previous, r3.Previous)
	}

	awaiting := `{"awaits_release":true}`
	seedAwaitingBead(ms, "gd-t1", model.TypeTask, awaiting) // in r1 by merge_sha, r1 is r2's previous
	seedMR(ms, "gd-mr1", "gd-t1", "1", true, "sha1")
	seedAwaitingBead(ms, "gd-t2", model.TypeBug, awaiting) // in r2 by mr_bead
	seedMR(ms, "gd-mr2", "gd-t2", "2", true, "")
	seedAwaitingBead(ms, "gd-t3", model.TypeTask, awaiting)          // only in r3, which is newer
	seedAwaitingBead(ms, "gd-t4", model.TypeTask, `{}`)              // carried, but does not await
	seedAwaitingBead(ms, "gd-t5", model.TypeTask, awaiting)          // carried, but a second PR is open
	seedMR(ms, "gd-mr6", "gd-t5", "6", false, "")                    //
	seedAwaitingBead(ms, "gd-t6", model.TypeTask, awaiting)          // no MR bead, no row
	seedAwaitingBead(ms, "gd-t7", model.TypeTask, awaiting)          // a merged PR no release carries yet
	seedMR(ms, "gd-mr7", "gd-t7", "7", true, "sha7")                 //
	ms.deps["gd-t7"] = append(ms.deps["gd-t7"], ms.deps["gd-t2"]...) // and one r2 carries
	escFields := func(spec, task string) string {
		return `{"awaits_release":true,"operation_type":"apply","operation_spec":` + spec + `,"requesting_task":"` + task + `"}`
	}
	seedAwaitingBead(ms, "gd-e1", model.TypeEscalation, escFields(`{"mr_url":"`+testPR+`5/"}`, ""))
	seedAwaitingBead(ms, "gd-e2", model.TypeEscalation, escFields(`{}`, "gd-t3"))
	seedAwaitingBead(ms, "gd-e3", model.TypeEscalation, escFields(`{}`, "gd-t1"))

	rec := doJSON(t, h, http.MethodPost, "/v1/releases/"+r2.ID+"/deployments", map[string]any{"target": "prod", "revision": "500"})
	if rec.Code != http.StatusOK {
		t.Fatalf("deploy: %d %s", rec.Code, rec.Body.String())
	}

	for id, wantClosed := range map[string]bool{
		"gd-t1": true, "gd-t2": true, "gd-t3": false, "gd-t4": false, "gd-t5": false,
		"gd-t6": false, "gd-t7": false, "gd-e1": true, "gd-e2": false, "gd-e3": true,
	} {
		if closed := ms.beads[id].Status == model.StatusClosed; closed != wantClosed {
			t.Errorf("%s closed=%v, want %v", id, closed, wantClosed)
		}
	}
	comments := ms.comments["gd-t1"]
	if len(comments) == 0 || !strings.Contains(comments[len(comments)-1].Text, "gardenio 2026.300.2 (release "+r2.ID+"), deployed to prod") {
		t.Errorf("gd-t1 comments = %+v, want one naming the release and target", comments)
	}

	// A repeat of the open row writes nothing and closes nothing more.
	if rec := doJSON(t, h, http.MethodPost, "/v1/releases/"+r2.ID+"/deployments", map[string]any{"target": "prod", "revision": "500"}); rec.Code != http.StatusOK {
		t.Fatalf("repeat deploy: %d", rec.Code)
	}
	if ms.beads["gd-t3"].Status == model.StatusClosed {
		t.Error("a repeat deploy closed work its release does not carry")
	}

	// Deploying r3 carries gd-t3, and through it gd-e2.
	if rec := doJSON(t, h, http.MethodPost, "/v1/releases/"+r3.ID+"/deployments", map[string]any{"target": "prod", "revision": "501"}); rec.Code != http.StatusOK {
		t.Fatalf("deploy r3: %d", rec.Code)
	}
	for _, id := range []string{"gd-t3", "gd-e2"} {
		if ms.beads[id].Status != model.StatusClosed {
			t.Errorf("%s still open after r3 deployed", id)
		}
	}
}

// A deferred awaits_release task is still closed by the release that carries
// its merged PR: parking it does not revoke the contract (gd-YApb4Jowu2).
func TestAwaitsRelease_DeploymentClosesDeferredWork(t *testing.T) {
	_, ms, h := newTestServer()
	seedReleaseProject(ms, "proj-gardenio", "gardenio", testGardenioReleaseSource)
	_, rel := createReleaseAsAdmin(t, h, map[string]any{"project": "gardenio", "tag": "2026.308.1", "cut_at": "2026-11-04T04:00:00Z",
		"contents": []any{map[string]any{"pr": "11", "url": testPR + "11", "merge_sha": "sha11", "task": "gd-parked"}}})
	seedAwaitingBead(ms, "gd-parked", model.TypeTask, `{"awaits_release":true}`)
	ms.beads["gd-parked"].Status = model.StatusDeferred
	seedMR(ms, "gd-mr-parked", "gd-parked", "11", true, "sha11")

	rec := doJSON(t, h, http.MethodPost, "/v1/releases/"+rel.ID+"/deployments", map[string]any{"target": "prod", "revision": "600"})
	if rec.Code != http.StatusOK {
		t.Fatalf("deploy: %d %s", rec.Code, rec.Body.String())
	}
	if ms.beads["gd-parked"].Status != model.StatusClosed {
		t.Fatalf("deferred awaits_release task status = %s, want closed", ms.beads["gd-parked"].Status)
	}
	cs := ms.comments["gd-parked"]
	if len(cs) == 0 || !strings.Contains(cs[len(cs)-1].Text, "gardenio 2026.308.1 (release "+rel.ID+"), deployed to prod") {
		t.Errorf("gd-parked comments = %+v, want one naming the release and target", cs)
	}
}

// A task that awaits its release is held open by close-on-merge and by
// close-on-execute, with a comment saying why.
func TestAwaitsRelease_MergeAndExecuteHoldTheTask(t *testing.T) {
	s, ms, _ := newTestServer()
	ctx := context.Background()
	seedAwaitingBead(ms, "gd-held", model.TypeTask, `{"awaits_release":true}`)
	seedMR(ms, "gd-mr-held", "gd-held", "9", true, "sha9")
	seedAwaitingBead(ms, "gd-plain", model.TypeTask, `{}`)
	seedMR(ms, "gd-mr-plain", "gd-plain", "10", true, "sha10")

	s.maybeCloseMergedLeaves(ctx, ms.beads["gd-mr-held"])
	s.maybeCloseMergedLeaves(ctx, ms.beads["gd-mr-plain"])
	s.closeEscalationWrapper(ctx, ms.beads["gd-held"], "gd-esc-merge")

	if ms.beads["gd-held"].Status == model.StatusClosed {
		t.Fatal("an awaits_release task closed on merge")
	}
	if ms.beads["gd-plain"].Status != model.StatusClosed {
		t.Error("a task without awaits_release stayed open after its MR merged")
	}
	cs := ms.comments["gd-held"]
	if len(cs) != 2 || !strings.Contains(cs[0].Text, "awaits_release=true") || !strings.Contains(cs[1].Text, "escalation gd-esc-merge executed") {
		t.Errorf("held task comments = %+v", cs)
	}
}

// A merged PR boarding the open release carries its awaits_release task
// without deploying it: the task gets one "carried, not yet deployed" note,
// stays open, and a repeat or a later append notes it no more. A task the
// rows do not carry gets nothing.
func TestAwaitsRelease_OpenReleaseCarriesWithoutClosing(t *testing.T) {
	_, ms, h := newTestServer()
	seedReleaseProject(ms, "proj-gardenio", "gardenio", testGardenioReleaseSource)
	rel := openReleaseFor(t, h, "gardenio")
	seedAwaitingBead(ms, "gd-t1", model.TypeTask, `{"awaits_release":true}`)
	seedMR(ms, "gd-mr1", "gd-t1", "1", true, "sha1")
	seedAwaitingBead(ms, "gd-t2", model.TypeTask, `{"awaits_release":true}`)
	seedMR(ms, "gd-mr2", "gd-t2", "2", true, "sha2")
	board := func(n, sha string) {
		t.Helper()
		row := map[string]any{"pr": n, "url": testPR + n, "merge_sha": sha, "task": "gd-t" + n}
		if rec := doJSON(t, h, http.MethodPost, "/v1/releases/"+rel.ID+"/contents", map[string]any{"contents": []any{row}}); rec.Code != http.StatusOK {
			t.Fatalf("board #%s: %d %s", n, rec.Code, rec.Body.String())
		}
	}
	board("1", "sha1")
	board("1", "sha1")
	board("3", "sha3")
	notes := func(id string) (n int) {
		for _, c := range ms.comments[id] {
			if strings.Contains(c.Text, boardedMarker(rel.ID)) {
				n++
			}
		}
		return n
	}
	if ms.beads["gd-t1"].Status == model.StatusClosed || notes("gd-t1") != 1 {
		t.Errorf("gd-t1 status %s, %d notes; want open with one note: %+v", ms.beads["gd-t1"].Status, notes("gd-t1"), ms.comments["gd-t1"])
	}
	if ms.beads["gd-t2"].Status == model.StatusClosed || notes("gd-t2") != 0 {
		t.Errorf("gd-t2, not on the release: status %s, %d notes", ms.beads["gd-t2"].Status, notes("gd-t2"))
	}
}
