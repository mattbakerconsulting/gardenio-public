package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// releasesFake is a kbeads daemon serving GET /v1/releases[/{id}] and the
// batch bead read from fixtures, so the handlers run on a real beadsapi.Client.
type releasesFake struct {
	releases  []beadsapi.Release // every project's, newest cut first
	beads     map[string]map[string]any
	failBatch bool

	mu       sync.Mutex
	batchIDs [][]string
}

func (f *releasesFake) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/releases", func(w http.ResponseWriter, r *http.Request) {
		project := r.URL.Query().Get("project")
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		out := []beadsapi.Release{}
		for _, rel := range f.releases {
			if (project == "" || rel.Project == project) && (limit <= 0 || len(out) < limit) {
				out = append(out, rel)
			}
		}
		writeJSONStatus(w, http.StatusOK, map[string]any{"releases": out})
	})
	mux.HandleFunc("GET /v1/releases/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, project := r.PathValue("id"), r.URL.Query().Get("project")
		for _, rel := range f.releases {
			if (project != "" && rel.Project == project && rel.Tag == id) || rel.ID == id {
				writeJSONStatus(w, http.StatusOK, rel)
				return
			}
		}
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": "release not found"})
	})
	mux.HandleFunc("GET /v1/batch/beads", func(w http.ResponseWriter, r *http.Request) {
		ids := strings.Split(r.URL.Query().Get("ids"), ",")
		f.mu.Lock()
		f.batchIDs = append(f.batchIDs, ids)
		fail := f.failBatch
		f.mu.Unlock()
		if fail {
			writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "store down"})
			return
		}
		results := []map[string]any{}
		for _, id := range ids {
			if b, ok := f.beads[id]; ok {
				results = append(results, map[string]any{"id": id, "bead": b})
			} else {
				results = append(results, map[string]any{"id": id, "error": "not found"})
			}
		}
		writeJSONStatus(w, http.StatusOK, map[string]any{"results": results})
	})
	return mux
}

func newReleasesFake() *releasesFake {
	contents := func(rows ...map[string]any) json.RawMessage {
		raw, _ := json.Marshal(rows)
		return raw
	}
	return &releasesFake{
		releases: []beadsapi.Release{
			{
				ID: "gd-rel3", Title: "gardenio 2026.304.2", Project: "gardenio", Tag: "2026.304.2",
				SHA: "c3c3c3", Previous: "gd-rel2", PreviousTag: "2026.304.1",
				CutAt: "2026-10-31T16:00:00.123456Z", CutBy: "release-captain", BuiltAt: "2026-10-31T16:20:00Z",
				Contents: contents(
					map[string]any{"pr": 10, "title": "feat: trucks", "url": "https://github.test/pr/10", "merge_sha": "a1",
						"merged_at": "2026-10-31T15:00:00.5+02:00", "step": "gd-step1", "plan": "gd-plan1", "task": "gd-task1", "mr_bead": "gd-mr1", "train": 12},
					map[string]any{"title": "direct commit", "merge_sha": "a2"},
					map[string]any{"pr": 0, "title": "zero", "merge_sha": "a3"},
					map[string]any{"pr": 11, "title": "fix: barn", "url": "https://github.test/pr/11", "merge_sha": "a4", "plan": "gd-plan-gone"},
				),
				Deployments: []beadsapi.ReleaseDeployment{{Target: "prod", Kind: "k3s", Since: "2026-10-31T16:30:00Z", By: "operator"}},
				Status:      beadsapi.ReleaseStatusLive,
			},
			{
				ID: "gd-rel2", Title: "gardenio 2026.304.1", Project: "gardenio", Tag: "2026.304.1",
				SHA: "b2b2b2", Previous: "gd-rel1", PreviousTag: "2026.303.2", CutAt: "2026-10-31T04:00:00Z",
				Contents: contents(
					map[string]any{"pr": 9, "title": "feat: crates", "url": "https://github.test/pr/9", "merge_sha": "b1", "step": "gd-step2", "plan": "gd-plan1"},
				),
				Deployments: []beadsapi.ReleaseDeployment{
					{Target: "staging", Since: "2026-10-31T05:00:00Z", Until: "2026-10-31T16:30:00Z", Revision: "41", Via: "release-deploy", ImageTag: "2026.304.1"},
					{Target: "prod", Since: "2026-10-31T06:00:00Z", Until: "2026-10-31T16:30:00Z"},
				},
				Status: beadsapi.ReleaseStatusSuperseded,
			},
			{
				ID: "gd-rel1", Title: "gardenio 2026.303.2", Project: "gardenio", Tag: "2026.303.2",
				CutAt: "yesterday", Status: beadsapi.ReleaseStatusBuilt,
			},
			{ID: "gd-rel9", Title: "tomato v1.0.0", Project: "tomato", Tag: "v1.0.0", Status: beadsapi.ReleaseStatusCut},
		},
		beads: map[string]map[string]any{
			"gd-step1": {"id": "gd-step1", "title": "Step 6: the releases JSON", "type": "task", "status": "closed", "fields": map[string]any{}},
			"gd-step2": {"id": "gd-step2", "title": "Step 5: the farm document", "type": "task", "status": "closed", "fields": map[string]any{}},
			"gd-plan1": {"id": "gd-plan1", "title": "Releases", "type": "epic", "status": "open", "fields": map[string]any{}},
		},
	}
}

// releasesMux serves the two routes on a Server whose daemon is a real client
// of the fake.
func releasesMux(t *testing.T, f *releasesFake) *http.ServeMux {
	t.Helper()
	ts := httptest.NewServer(f.handler())
	t.Cleanup(ts.Close)
	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: ts.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	s := NewServer(client, testLogger(), "", "", "", &fakeScreenClient{})
	mux := http.NewServeMux()
	s.registerReleasesAPIRoutes(mux)
	return mux
}

func getReleasesJSON(t *testing.T, mux *http.ServeMux, path string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("%s: Content-Type %q, want application/json", path, ct)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: body %q: %v", path, rec.Body.String(), err)
	}
	return rec.Code, body
}

// asMaps is v ([]any of objects) as maps; it fails when v is not an array.
func asMaps(t *testing.T, what string, v any) []map[string]any {
	t.Helper()
	arr, ok := v.([]any)
	if !ok {
		t.Fatalf("%s = %#v, want an array", what, v)
	}
	out := make([]map[string]any, len(arr))
	for i, x := range arr {
		out[i], _ = x.(map[string]any)
	}
	return out
}

func assertKeysAbsent(t *testing.T, what string, m map[string]any, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if v, ok := m[k]; ok {
			t.Errorf("%s has %q = %#v, want it left out", what, k, v)
		}
	}
}

// TestReleasesAPI_ListPages: newest first, limit per page, next_cursor until
// the last page, which has none.
func TestReleasesAPI_ListPages(t *testing.T) {
	mux := releasesMux(t, newReleasesFake())

	code, body := getReleasesJSON(t, mux, "/api/releases?project=gardenio&limit=2")
	if code != http.StatusOK {
		t.Fatalf("status %d: %v", code, body)
	}
	items := asMaps(t, "items", body["items"])
	if len(items) != 2 || items[0]["id"] != "gd-rel3" || items[1]["id"] != "gd-rel2" {
		t.Fatalf("page 1 = %v, want gd-rel3, gd-rel2", items)
	}
	cursor, _ := body["next_cursor"].(string)
	if cursor == "" {
		t.Fatalf("page 1 has no next_cursor: %v", body)
	}
	first := items[0]
	if first["tag"] != "2026.304.2" || first["status"] != "live" || first["project"] != "gardenio" ||
		first["sha"] != "c3c3c3" || first["cut_at"] != "2026-10-31T16:00:00Z" || first["cut_by"] != "release-captain" {
		t.Errorf("release = %v", first)
	}
	assertKeysAbsent(t, "list item", first, "next", "next_tag", "title", "images", "source", "created_at")

	code, body = getReleasesJSON(t, mux, "/api/releases?project=gardenio&limit=2&cursor="+cursor)
	if code != http.StatusOK {
		t.Fatalf("page 2 status %d: %v", code, body)
	}
	items = asMaps(t, "items", body["items"])
	if len(items) != 1 || items[0]["id"] != "gd-rel1" {
		t.Errorf("page 2 = %v, want gd-rel1", items)
	}
	assertKeysAbsent(t, "last page", body, "next_cursor")

	// The default limit holds all three, so one page with no cursor.
	_, body = getReleasesJSON(t, mux, "/api/releases?project=gardenio")
	if items := asMaps(t, "items", body["items"]); len(items) != 3 {
		t.Errorf("default limit: %d items, want 3", len(items))
	}
	assertKeysAbsent(t, "single page", body, "next_cursor")
}

// TestReleasesAPI_ListRefusals: no project, a bad limit or cursor are 400 with
// a JSON error; a project with no releases (or none the caller can read) is an
// empty list.
func TestReleasesAPI_ListRefusals(t *testing.T) {
	mux := releasesMux(t, newReleasesFake())
	for _, path := range []string{
		"/api/releases", "/api/releases?project=%20",
		"/api/releases?project=gardenio&limit=0", "/api/releases?project=gardenio&limit=201",
		"/api/releases?project=gardenio&limit=x", "/api/releases?project=gardenio&cursor=!!",
		"/api/releases?project=gardenio&cursor=" + base64.RawURLEncoding.EncodeToString([]byte("5")),
		"/api/releases?project=gardenio&cursor=" + base64.RawURLEncoding.EncodeToString([]byte("o-1")),
	} {
		code, body := getReleasesJSON(t, mux, path)
		if code != http.StatusBadRequest || body["error"] == "" || body["error"] == nil {
			t.Errorf("%s: %d %v, want 400 with an error", path, code, body)
		}
	}

	code, body := getReleasesJSON(t, mux, "/api/releases?project=nowhere")
	if code != http.StatusOK {
		t.Fatalf("unknown project: %d %v", code, body)
	}
	if items := asMaps(t, "items", body["items"]); len(items) != 0 {
		t.Errorf("unknown project items = %v, want []", items)
	}
	assertKeysAbsent(t, "unknown project", body, "next_cursor")
}

// TestReleasesAPI_GetByIDAndTag: one release by bead id, or by tag with
// ?project=; next/next_tag name the release cut after it; 404 for none.
func TestReleasesAPI_GetByIDAndTag(t *testing.T) {
	mux := releasesMux(t, newReleasesFake())

	for _, path := range []string{"/api/releases/gd-rel2", "/api/releases/2026.304.1?project=gardenio"} {
		code, body := getReleasesJSON(t, mux, path)
		if code != http.StatusOK {
			t.Fatalf("%s: status %d: %v", path, code, body)
		}
		if body["id"] != "gd-rel2" || body["tag"] != "2026.304.1" || body["status"] != "superseded" {
			t.Errorf("%s = %v", path, body)
		}
		if body["next"] != "gd-rel3" || body["next_tag"] != "2026.304.2" {
			t.Errorf("%s next = %v / %v, want gd-rel3 / 2026.304.2", path, body["next"], body["next_tag"])
		}
		// Deployments keep their stored order, oldest first.
		deps := asMaps(t, "deployments", body["deployments"])
		if len(deps) != 2 || deps[0]["target"] != "staging" || deps[1]["target"] != "prod" {
			t.Errorf("%s deployments = %v, want staging then prod", path, deps)
		}
		if d := deps[0]; d["since"] != "2026-10-31T05:00:00Z" || d["until"] != "2026-10-31T16:30:00Z" ||
			d["revision"] != "41" || d["via"] != "release-deploy" || d["image_tag"] != "2026.304.1" {
			t.Errorf("%s deployment = %v", path, d)
		}
	}

	// The newest release has no next.
	_, body := getReleasesJSON(t, mux, "/api/releases/gd-rel3")
	assertKeysAbsent(t, "newest release", body, "next", "next_tag")

	for _, path := range []string{
		"/api/releases/gd-nope", "/api/releases/v9.9.9?project=gardenio",
		// Another project's release id is not this project's.
		"/api/releases/gd-rel9?project=gardenio",
	} {
		if code, body := getReleasesJSON(t, mux, path); code != http.StatusNotFound || body["error"] == nil {
			t.Errorf("%s: %d %v, want 404 with an error", path, code, body)
		}
	}
}

// TestReleasesAPI_OmitsEmptyFields: an empty optional field is left out, never
// null; contents and deployments are arrays even when empty; a time that does
// not parse is dropped; a contents row with no PR is dropped.
func TestReleasesAPI_OmitsEmptyFields(t *testing.T) {
	mux := releasesMux(t, newReleasesFake())

	code, body := getReleasesJSON(t, mux, "/api/releases/gd-rel1")
	if code != http.StatusOK {
		t.Fatalf("status %d: %v", code, body)
	}
	assertKeysAbsent(t, "bare release", body, "sha", "cut_at", "cut_by", "previous", "previous_tag", "built_at")
	for _, k := range []string{"contents", "deployments"} {
		if arr, ok := body[k].([]any); !ok || len(arr) != 0 {
			t.Errorf("%s = %#v, want []", k, body[k])
		}
	}
	for _, k := range []string{"id", "tag", "status", "project"} {
		if _, ok := body[k]; !ok {
			t.Errorf("bare release has no %q", k)
		}
	}

	_, body = getReleasesJSON(t, mux, "/api/releases/gd-rel3")
	rows := asMaps(t, "contents", body["contents"])
	if len(rows) != 2 || rows[0]["pr"] != 10.0 || rows[1]["pr"] != 11.0 {
		t.Fatalf("contents = %v, want PRs 10 and 11 (the rows with no PR dropped)", rows)
	}
	if r := rows[0]; r["merged_at"] != "2026-10-31T13:00:00Z" || r["train"] != 12.0 || r["task"] != "gd-task1" || r["mr_bead"] != "gd-mr1" {
		t.Errorf("row = %v", r)
	}
	assertKeysAbsent(t, "content row", rows[0], "merge_sha")
	assertKeysAbsent(t, "bare content row", rows[1], "step", "merged_at", "task", "mr_bead", "train", "step_title")
	deps := asMaps(t, "deployments", body["deployments"])
	if len(deps) != 1 || deps[0]["kind"] != "k3s" || deps[0]["by"] != "operator" {
		t.Fatalf("deployments = %v", deps)
	}
	assertKeysAbsent(t, "open deployment", deps[0], "until", "revision", "via", "image_tag")
}

// TestReleasesAPI_Titles: step and plan titles (and the plan's status) come
// from one batch read of the distinct ids; a bead the read lacks, or a failed
// read, leaves them out and the request still answers 200.
func TestReleasesAPI_Titles(t *testing.T) {
	f := newReleasesFake()
	mux := releasesMux(t, f)

	_, body := getReleasesJSON(t, mux, "/api/releases?project=gardenio")
	items := asMaps(t, "items", body["items"])
	rows := asMaps(t, "contents", items[0]["contents"])
	if r := rows[0]; r["step_title"] != "Step 6: the releases JSON" || r["plan_title"] != "Releases" || r["plan_status"] != "open" {
		t.Errorf("row = %v, want the step and plan titles and the plan's status", r)
	}
	assertKeysAbsent(t, "row whose plan the read lacks", rows[1], "plan_title", "plan_status")
	if r := asMaps(t, "contents", items[1]["contents"])[0]; r["step_title"] != "Step 5: the farm document" || r["plan_title"] != "Releases" {
		t.Errorf("second release row = %v", r)
	}
	f.mu.Lock()
	calls := f.batchIDs
	f.mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("batch reads = %v, want one", calls)
	}
	got := slices.Sorted(slices.Values(calls[0]))
	if want := []string{"gd-plan-gone", "gd-plan1", "gd-step1", "gd-step2"}; !slices.Equal(got, want) {
		t.Errorf("batch ids = %v, want each distinct id once: %v", got, want)
	}

	f.mu.Lock()
	f.failBatch = true
	f.mu.Unlock()
	code, body := getReleasesJSON(t, mux, "/api/releases/gd-rel3")
	if code != http.StatusOK {
		t.Fatalf("failed title read: status %d, want 200: %v", code, body)
	}
	rows = asMaps(t, "contents", body["contents"])
	if len(rows) != 2 || rows[0]["step"] != "gd-step1" {
		t.Fatalf("contents = %v", rows)
	}
	assertKeysAbsent(t, "row after a failed title read", rows[0], "step_title", "plan_title", "plan_status")
}

// The open release, listed first by the daemon, is left out of the list and
// its pages: the Release page's own card for it is a later step.
func TestReleasesAPI_ListSkipsTheOpenRelease(t *testing.T) {
	f := newReleasesFake()
	f.releases = append([]beadsapi.Release{{ID: "gd-next", Project: "gardenio", Status: beadsapi.ReleaseStatusOpen}}, f.releases...)
	mux := releasesMux(t, f)
	_, body := getReleasesJSON(t, mux, "/api/releases?project=gardenio&limit=2")
	items := asMaps(t, "items", body["items"])
	cursor, _ := body["next_cursor"].(string)
	if len(items) != 2 || items[0]["id"] != "gd-rel3" || items[1]["id"] != "gd-rel2" || cursor == "" {
		t.Fatalf("page 1 = %v (cursor %q), want gd-rel3, gd-rel2 and a next page", items, cursor)
	}
	_, body = getReleasesJSON(t, mux, "/api/releases?project=gardenio&limit=2&cursor="+cursor)
	if items := asMaps(t, "items", body["items"]); len(items) != 1 || items[0]["id"] != "gd-rel1" {
		t.Errorf("page 2 = %v, want gd-rel1", items)
	}
}
