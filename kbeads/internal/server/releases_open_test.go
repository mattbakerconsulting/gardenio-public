package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// openReleaseFor opens project's next release as the test server's admin and
// returns it.
func openReleaseFor(t *testing.T, h http.Handler, project string) releaseView {
	t.Helper()
	code, rel := createReleaseAsAdmin(t, h, map[string]any{"project": project})
	if code != http.StatusCreated {
		t.Fatalf("open %s release: %d", project, code)
	}
	return rel
}

// patchRelease PATCHes a release's fields through the generic bead route.
func patchRelease(t *testing.T, h http.Handler, id string, fields map[string]any) (int, string) {
	t.Helper()
	rec := doJSON(t, h, http.MethodPatch, "/v1/beads/"+id, map[string]any{"fields": fields})
	return rec.Code, rec.Body.String()
}

// A create with no tag opens the project's next release: kept open, keyed
// release:<project>/next, stamped opened_at and opened_by, status open, and
// read as open by kd show (GET /v1/beads/{id}). A second open one of the same
// project is a 409; another project opens its own.
func TestOpenRelease_CreateOnePerProject(t *testing.T) {
	_, ms, h := newTestServer()
	seedReleaseProject(ms, "proj-gardenio", "gardenio", testGardenioReleaseSource)
	seedReleaseProject(ms, "proj-other", "other", `{"release_tag_pattern":"^v\\d+\\.\\d+\\.\\d+$"}`)

	rel := openReleaseFor(t, h, "gardenio")
	if rel.Status != releaseStatusOpen || rel.Tag != "" || rel.OpenedAt == "" || rel.OpenedBy == "" || rel.Title != "gardenio next release" {
		t.Errorf("open release = %+v", rel)
	}
	b := ms.beads[rel.ID]
	if b.Status != model.StatusOpen || b.ClosedAt != nil {
		t.Errorf("open release bead status=%s closed_at=%v, want open", b.Status, b.ClosedAt)
	}
	if key := releaseString(releaseFieldMap(b.Fields), idempotencyKeyField); key != "release:gardenio/next" {
		t.Errorf("idempotency_key = %q", key)
	}

	rec := doJSON(t, h, http.MethodGet, "/v1/beads/"+rel.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("kd show: %d %s", rec.Code, rec.Body.String())
	}
	var shown model.Bead
	if err := json.Unmarshal(rec.Body.Bytes(), &shown); err != nil {
		t.Fatal(err)
	}
	if shown.Status != model.StatusOpen || releaseString(releaseFieldMap(shown.Fields), "status") != releaseStatusOpen {
		t.Errorf("kd show: bead status %s, fields.status %q, want open/open", shown.Status, releaseString(releaseFieldMap(shown.Fields), "status"))
	}

	if rec := doJSON(t, h, http.MethodPost, "/v1/releases", map[string]any{"project": "gardenio"}); rec.Code != http.StatusConflict {
		t.Errorf("second open release: %d %s, want 409", rec.Code, rec.Body.String())
	}
	other := openReleaseFor(t, h, "other")
	if other.ID == rel.ID {
		t.Error("another project's open release is the first project's")
	}

	// An open release takes none of the cut's, a build's or a deployment's fields.
	seedReleaseProject(ms, "proj-third", "third", testGardenioReleaseSource)
	for _, key := range []string{"sha", "cut_at", "built_at", "previous_tag"} {
		body := map[string]any{"project": "third", key: "2026-10-27T04:00:00Z"}
		if rec := doJSON(t, h, http.MethodPost, "/v1/releases", body); rec.Code != http.StatusBadRequest {
			t.Errorf("open release with %s: %d, want 400", key, rec.Code)
		}
	}
}

// The cut writes the tag onto the open release once: the bead closes, status
// is derived cut, cut_at/cut_by/source are filled, previous links the newest
// cut release, and the key moves to the tag's, so the release resolves by tag
// and the project can open its next one. A later tag write is refused, as is
// a tag off the pattern, one another release holds, and a close with no tag.
func TestOpenRelease_TagWriteCuts(t *testing.T) {
	_, ms, h := newTestServer()
	seedReleaseProject(ms, "proj-gardenio", "gardenio", testGardenioReleaseSource)
	_, prev := createReleaseAsAdmin(t, h, map[string]any{"project": "gardenio", "tag": "2026.306.1", "cut_at": "2026-11-02T04:00:00Z"})
	rel := openReleaseFor(t, h, "gardenio")

	// Before the cut: the cut's fields go with the tag, and a close is refused.
	if code, body := patchRelease(t, h, rel.ID, map[string]any{"sha": "abc"}); code != http.StatusBadRequest {
		t.Errorf("sha without a tag: %d %s, want 400", code, body)
	}
	if rec := doJSON(t, h, http.MethodPost, "/v1/beads/"+rel.ID+"/close", nil); rec.Code != http.StatusConflict {
		t.Errorf("close of an open release: %d %s, want 409", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, h, http.MethodPatch, "/v1/beads/"+rel.ID, map[string]any{"status": "closed"}); rec.Code/100 == 2 {
		t.Errorf("PATCH status closed with no tag: %d, want refused", rec.Code)
	}
	if code, _ := patchRelease(t, h, rel.ID, map[string]any{"tag": "v9"}); code != http.StatusBadRequest {
		t.Errorf("tag off the pattern: %d, want 400", code)
	}
	if code, _ := patchRelease(t, h, rel.ID, map[string]any{"tag": "2026.306.1"}); code != http.StatusConflict {
		t.Errorf("tag another release holds: %d, want 409", code)
	}
	if rec := doJSON(t, h, http.MethodPost, "/v1/releases/"+rel.ID+"/deployments", map[string]any{"target": "prod"}); rec.Code != http.StatusConflict {
		t.Errorf("deploy of an open release: %d, want 409", rec.Code)
	}
	if b := ms.beads[rel.ID]; b.Status != model.StatusOpen || releaseString(releaseFieldMap(b.Fields), "tag") != "" {
		t.Fatalf("refused writes changed the open release: %s %s", b.Status, b.Fields)
	}

	code, body := patchRelease(t, h, rel.ID, map[string]any{"tag": "2026.307.1", "sha": "def456", "source": "script"})
	if code != http.StatusOK {
		t.Fatalf("cut: %d %s", code, body)
	}
	b := ms.beads[rel.ID]
	v := releaseViewOf(b)
	if b.Status != model.StatusClosed || b.ClosedAt == nil {
		t.Errorf("cut release bead status=%s closed_at=%v, want closed", b.Status, b.ClosedAt)
	}
	if v.Status != releaseStatusCut || v.Tag != "2026.307.1" || v.SHA != "def456" || v.Source != "script" || v.CutAt == "" || v.CutBy == "" {
		t.Errorf("cut release = %+v", v)
	}
	if v.Previous != prev.ID || v.PreviousTag != "2026.306.1" {
		t.Errorf("cut release previous = %q/%q, want %s/2026.306.1", v.Previous, v.PreviousTag, prev.ID)
	}
	if key := releaseString(releaseFieldMap(b.Fields), idempotencyKeyField); key != "release:gardenio:2026.307.1" {
		t.Errorf("cut release idempotency_key = %q", key)
	}
	if rec := doJSON(t, h, http.MethodGet, "/v1/releases/2026.307.1?project=gardenio", nil); rec.Code != http.StatusOK || decodeRelease(t, rec.Body.Bytes()).ID != rel.ID {
		t.Errorf("GET by tag after the cut: %d %s", rec.Code, rec.Body.String())
	}

	// Written once: the tag and the cut's other fields are fixed now.
	for key, val := range map[string]string{"tag": "2026.307.2", "sha": "fff", "cut_by": "someone"} {
		if code, _ := patchRelease(t, h, rel.ID, map[string]any{key: val}); code != http.StatusBadRequest {
			t.Errorf("%s write on a cut release: %d, want 400", key, code)
		}
	}
	if releaseViewOf(ms.beads[rel.ID]).Tag != "2026.307.1" {
		t.Error("a refused tag write changed the tag")
	}

	next := openReleaseFor(t, h, "gardenio")
	if next.ID == rel.ID {
		t.Error("the cut did not free the project's next key")
	}
}

// GET /v1/releases puts the open release first, then the cut ones newest
// first; an open release is never a cut release's previous.
func TestOpenRelease_ListedFirst(t *testing.T) {
	_, ms, h := newTestServer()
	seedReleaseProject(ms, "proj-gardenio", "gardenio", testGardenioReleaseSource)
	createReleaseAsAdmin(t, h, map[string]any{"project": "gardenio", "tag": "2026.306.1", "cut_at": "2026-11-02T04:00:00Z"})
	open := openReleaseFor(t, h, "gardenio")
	_, newer := createReleaseAsAdmin(t, h, map[string]any{"project": "gardenio", "tag": "2026.306.2", "cut_at": "2026-11-02T16:00:00Z"})
	if newer.PreviousTag != "2026.306.1" {
		t.Errorf("a release cut beside an open one links previous %q, want 2026.306.1", newer.PreviousTag)
	}

	rec := doJSON(t, h, http.MethodGet, "/v1/releases?project=gardenio", nil)
	var resp struct {
		Releases []releaseView `json:"releases"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range resp.Releases {
		got = append(got, r.ID)
	}
	if len(got) != 3 || got[0] != open.ID || resp.Releases[0].Status != releaseStatusOpen || resp.Releases[1].Tag != "2026.306.2" {
		t.Errorf("list = %+v, want the open release first", resp.Releases)
	}
	rec = doJSON(t, h, http.MethodGet, "/v1/releases?project=gardenio&status=open", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Releases) != 1 || resp.Releases[0].ID != open.ID {
		t.Errorf("status=open lists %+v", resp.Releases)
	}
}

// POST /v1/releases/{id}/contents appends rows the open release does not
// carry: a repeat of a (merge_sha, pr) changes nothing, a train's PRs sharing
// a merge_sha each board, a malformed row is a 400 and a cut release a 409.
func TestOpenRelease_ContentsAppendIdempotent(t *testing.T) {
	_, ms, h := newTestServer()
	seedReleaseProject(ms, "proj-gardenio", "gardenio", testGardenioReleaseSource)
	rel := openReleaseFor(t, h, "gardenio")
	path := "/v1/releases/" + rel.ID + "/contents"
	row := func(pr int, sha string) map[string]any {
		return map[string]any{"pr": pr, "merge_sha": sha, "url": "https://github.com/o/r/pull/1", "task": "gd-x"}
	}
	add := func(rows ...map[string]any) (int, string, releaseView) {
		t.Helper()
		rec := doJSON(t, h, http.MethodPost, path, map[string]any{"contents": rows})
		if rec.Code != http.StatusOK {
			return rec.Code, "", releaseView{}
		}
		return rec.Code, rec.Header().Get("X-Release-Contents-Added"), decodeRelease(t, rec.Body.Bytes())
	}
	contents := func(v releaseView) []map[string]any {
		var rows []map[string]any
		if err := json.Unmarshal(v.Contents, &rows); err != nil {
			t.Fatalf("contents %s: %v", v.Contents, err)
		}
		return rows
	}

	code, added, v := add(row(1983, "aaa"))
	if code != http.StatusOK || added != "1" || len(contents(v)) != 1 {
		t.Fatalf("first append: %d added=%s %s", code, added, v.Contents)
	}
	before := string(ms.beads[rel.ID].Fields)
	if code, added, v = add(row(1983, "aaa")); code != http.StatusOK || added != "0" || len(contents(v)) != 1 {
		t.Errorf("repeat append: %d added=%s %s", code, added, v.Contents)
	}
	if string(ms.beads[rel.ID].Fields) != before {
		t.Error("a repeated row rewrote the release")
	}
	// A merge train: two PRs on one merge commit, each a row; the repeat in
	// the same request boards once.
	if code, added, v = add(row(1984, "bbb"), row(1985, "bbb"), row(1984, "bbb")); code != http.StatusOK || added != "2" || len(contents(v)) != 3 {
		t.Errorf("train append: %d added=%s %s", code, added, v.Contents)
	}
	if b := ms.beads[rel.ID]; b.Status != model.StatusOpen || releaseViewOf(b).Status != releaseStatusOpen {
		t.Errorf("an append moved the open release: %s %s", b.Status, releaseViewOf(b).Status)
	}

	for name, body := range map[string]any{
		"not a list":     map[string]any{"contents": map[string]any{"pr": 1}},
		"empty":          map[string]any{"contents": []any{}},
		"row names none": map[string]any{"contents": []any{map[string]any{"title": "x"}}},
		"row not object": map[string]any{"contents": []any{"x"}},
	} {
		if rec := doJSON(t, h, http.MethodPost, path, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, rec.Code, rec.Body.String())
		}
	}

	if code, body := patchRelease(t, h, rel.ID, map[string]any{"tag": "2026.307.1"}); code != http.StatusOK {
		t.Fatalf("cut: %d %s", code, body)
	}
	if code, _, _ := add(row(1990, "ccc")); code != http.StatusConflict {
		t.Errorf("append to a cut release: %d, want 409", code)
	}
	if code, _, _ := add(row(1990, "ccc")); code == http.StatusNotFound {
		t.Error("cut release not found")
	}
	if rec := doJSON(t, h, http.MethodPost, "/v1/releases/gd-nope/contents", map[string]any{"contents": []any{row(1, "a")}}); rec.Code != http.StatusNotFound {
		t.Errorf("unknown release: %d, want 404", rec.Code)
	}
}

// The contents route is a release write: the release recorder's machine
// principal (github-bridge) passes the writer check, a plain machine does not.
func TestOpenRelease_ContentsWriterCheck(t *testing.T) {
	s, ms, _ := newTestServer()
	seedReleaseProject(ms, "proj-gardenio", "gardenio", testGardenioReleaseSource)
	if d := s.releaseWriterDenial(machineCtx("gardenio-github-bridge", "gardenio-machine-release-recorder"), "update"); d != nil {
		t.Errorf("release recorder refused: %s", d.Message)
	}
	if d := s.releaseWriterDenial(machineCtx("gardenio-release-deployer", "gardenio-operator", "gardenio-machine-release-deploy"), "update"); d == nil {
		t.Error("the deploy-only machine may append contents")
	}
}

// cutRaceStore runs onFindProject once, the next time the server looks up a
// project: a cut does that in checkOpenReleaseCut, after it read the open
// release and before releaseWriteMu, which is where a merged PR's append can
// land.
type cutRaceStore struct {
	*mockStore
	onFindProject func()
}

func (r *cutRaceStore) FindProjectByName(ctx context.Context, name string) (*model.Bead, error) {
	if f := r.onFindProject; f != nil {
		r.onFindProject = nil
		f()
	}
	return r.mockStore.FindProjectByName(ctx, name)
}

// A contents append that lands between the cut's read and its write is not
// lost (gd-k8Z0vMaohu): a cut that writes no contents carries the appended
// row, and one that writes contents is a 409 that leaves the release open, so
// its writer re-reads and cuts again with the row.
func TestOpenRelease_CutKeepsContentsAppendedInFlight(t *testing.T) {
	ms := newMockStore()
	rs := &cutRaceStore{mockStore: ms}
	s := NewBeadsServer(rs, &events.NoopPublisher{})
	s.userProvisioner = nil
	h := defaultBearerMiddleware(s.NewHTTPHandler(&bearerSubjectVerifier{}))
	seedReleaseProject(ms, "proj-gardenio", "gardenio", testGardenioReleaseSource)
	row := func(pr int, sha string) map[string]any {
		return map[string]any{"pr": pr, "merge_sha": sha}
	}
	rowsOf := func(id string) []map[string]any {
		t.Helper()
		var rows []map[string]any
		if err := json.Unmarshal(releaseViewOf(ms.beads[id]).Contents, &rows); err != nil {
			t.Fatalf("contents of %s: %v", id, err)
		}
		return rows
	}
	appendInFlight := func(id string, r map[string]any) {
		rs.onFindProject = func() {
			rec := doJSON(t, h, http.MethodPost, "/v1/releases/"+id+"/contents", map[string]any{"contents": []any{r}})
			if rec.Code != http.StatusOK {
				t.Errorf("append in flight: %d %s", rec.Code, rec.Body.String())
			}
		}
	}

	// A cut with no contents of its own takes the row appended in flight.
	rel := openReleaseFor(t, h, "gardenio")
	if rec := doJSON(t, h, http.MethodPost, "/v1/releases/"+rel.ID+"/contents", map[string]any{"contents": []any{row(2001, "aaa")}}); rec.Code != http.StatusOK {
		t.Fatalf("first append: %d %s", rec.Code, rec.Body.String())
	}
	appendInFlight(rel.ID, row(2002, "bbb"))
	if code, body := patchRelease(t, h, rel.ID, map[string]any{"tag": "2026.307.1", "sha": "abc"}); code != http.StatusOK {
		t.Fatalf("cut: %d %s", code, body)
	}
	if rs.onFindProject != nil {
		t.Fatal("the append did not run in flight")
	}
	if rows := rowsOf(rel.ID); len(rows) != 2 || rows[1]["merge_sha"] != "bbb" {
		t.Errorf("cut release contents = %v, want the row appended in flight kept", rows)
	}
	if v := releaseViewOf(ms.beads[rel.ID]); v.Tag != "2026.307.1" || ms.beads[rel.ID].Status != model.StatusClosed {
		t.Errorf("cut release = %+v, status %s", v, ms.beads[rel.ID].Status)
	}

	// A cut that writes contents read before the append is a 409; its retry
	// re-reads the release and carries the row.
	next := openReleaseFor(t, h, "gardenio")
	appendInFlight(next.ID, row(2003, "ccc"))
	stale := []any{row(2004, "ddd")}
	if code, body := patchRelease(t, h, next.ID, map[string]any{"tag": "2026.307.2", "contents": stale}); code != http.StatusConflict {
		t.Fatalf("cut with stale contents: %d %s, want 409", code, body)
	}
	if b := ms.beads[next.ID]; b.Status != model.StatusOpen || releaseViewOf(b).Tag != "" || len(rowsOf(next.ID)) != 1 {
		t.Fatalf("the refused cut changed the open release: %s %s", b.Status, b.Fields)
	}
	retry := append(stale, rowsOf(next.ID)[0])
	if code, body := patchRelease(t, h, next.ID, map[string]any{"tag": "2026.307.2", "contents": retry}); code != http.StatusOK {
		t.Fatalf("cut retry: %d %s", code, body)
	}
	if rows := rowsOf(next.ID); len(rows) != 2 || rows[1]["merge_sha"] != "ccc" {
		t.Errorf("retried cut contents = %v, want the appended row", rows)
	}
}

// gb release open names the release's gardener in gardener_id
// (gd-BS9fpWa1bx): the type:release schema admits it, and the release API
// returns it. A key the schema lacks is still refused, so this is the schema
// speaking, not a server that takes any field.
func TestOpenRelease_NamesItsGardener(t *testing.T) {
	_, ms, h := newTestServer()
	seedReleaseProject(ms, "proj-gardenio", "gardenio", testGardenioReleaseSource)
	rel := openReleaseFor(t, h, "gardenio")
	if code, body := patchRelease(t, h, rel.ID, map[string]any{"gardener_id": "gd-trig"}); code != http.StatusOK {
		t.Fatalf("gardener_id write: %d %s", code, body)
	}
	rec := doJSON(t, h, http.MethodGet, "/v1/releases/"+rel.ID, nil)
	var got releaseView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.GardenerID != "gd-trig" {
		t.Errorf("release API: %d %s, want gardener_id gd-trig", rec.Code, rec.Body.String())
	}
	if code, _ := patchRelease(t, h, rel.ID, map[string]any{"no_such_release_field": "x"}); code != http.StatusBadRequest {
		t.Errorf("unknown field write: %d, want 400", code)
	}
}
