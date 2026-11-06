package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// gardenio's release source (releases-design.md §4): calver, one prod target.
const testGardenioReleaseSource = `{"release_tag_pattern":"^20\\d{2}\\.\\d{1,3}\\.\\d+$",` +
	`"release_targets":[{"name":"prod","kind":"k3s"}]}`

// seedReleaseProject puts a project bead named name with fields into ms.
func seedReleaseProject(ms *mockStore, id, name, fields string) {
	ms.beads[id] = &model.Bead{
		ID: id, Title: name, Type: model.TypeProject, Kind: model.KindConfig,
		Status: model.StatusOpen, Fleet: "default", Fields: json.RawMessage(fields),
		CreatedAt: time.Now().UTC(),
	}
}

// decodeRelease reads a releaseView response body.
func decodeRelease(t *testing.T, body []byte) releaseView {
	t.Helper()
	var v releaseView
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("decode release: %v: %s", err, body)
	}
	return v
}

// createReleaseAsAdmin POSTs /v1/releases as the test server's OIDC admin and
// returns the response code and the release.
func createReleaseAsAdmin(t *testing.T, h http.Handler, body map[string]any) (int, releaseView) {
	t.Helper()
	rec := doJSON(t, h, http.MethodPost, "/v1/releases", body)
	if rec.Code/100 != 2 {
		return rec.Code, releaseView{}
	}
	return rec.Code, decodeRelease(t, rec.Body.Bytes())
}

func TestRelease_TypeRegistered(t *testing.T) {
	if !slices.Contains(BuiltinBeadTypeNames(), string(model.TypeRelease)) {
		t.Fatal("release is not a builtin type")
	}
	s, _, _ := newTestServer()
	tc, err := s.resolveTypeConfig(context.Background(), model.TypeRelease)
	if err != nil || tc == nil {
		t.Fatalf("resolveTypeConfig: %v %v", tc, err)
	}
	if tc.Kind != model.KindData || tc.WritePolicy == nil || tc.WritePolicy.MinRole != RoleAdmin {
		t.Errorf("release must be data-kind with an admin write_policy: %+v", tc)
	}
}

func TestReleaseStatus(t *testing.T) {
	for _, tc := range []struct {
		tag   string
		built string
		deps  []releaseDeployment
		want  string
	}{
		{"", "", nil, releaseStatusOpen},
		{"v1", "", nil, releaseStatusCut},
		{"v1", "2026-10-28T05:00:00Z", nil, releaseStatusBuilt},
		{"v1", "", []releaseDeployment{{Target: "prod"}}, releaseStatusLive},
		{"v1", "2026-10-28T05:00:00Z", []releaseDeployment{{Target: "prod", Until: "x"}, {Target: "box"}}, releaseStatusLive},
		{"v1", "2026-10-28T05:00:00Z", []releaseDeployment{{Target: "prod", Until: "x"}}, releaseStatusSuperseded},
	} {
		if got := releaseStatus(tc.tag, tc.built, tc.deps); got != tc.want {
			t.Errorf("releaseStatus(%q, %q, %+v) = %q, want %q", tc.tag, tc.built, tc.deps, got, tc.want)
		}
	}
}

// A release is created closed, parented to its project bead, keyed by
// (project, tag): the second create returns the first.
func TestRelease_CreateIsUniquePerProjectAndTag(t *testing.T) {
	_, ms, h := newTestServer()
	seedReleaseProject(ms, "proj-gardenio", "gardenio", testGardenioReleaseSource)

	code, rel := createReleaseAsAdmin(t, h, map[string]any{
		"project": "gardenio", "tag": "2026.300.1", "sha": "abc123", "cut_at": "2026-10-27T04:00:00Z",
	})
	if code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	if rel.Status != releaseStatusCut || rel.Title != "gardenio 2026.300.1" || rel.CutBy == "" || rel.Source != "manual" {
		t.Errorf("created release = %+v", rel)
	}
	b := ms.beads[rel.ID]
	if b.Status != model.StatusClosed || b.Kind != model.KindData {
		t.Errorf("release bead status=%s kind=%s, want closed data", b.Status, b.Kind)
	}
	if !slices.Contains(ms.labels[rel.ID], "project:gardenio") {
		t.Errorf("release labels = %v", ms.labels[rel.ID])
	}
	parented := false
	for _, d := range ms.deps[rel.ID] {
		if d.Type == model.DepParentChild && d.DependsOnID == "proj-gardenio" {
			parented = true
		}
	}
	if !parented {
		t.Errorf("release is not parented to its project bead: %+v", ms.deps[rel.ID])
	}

	code, again := createReleaseAsAdmin(t, h, map[string]any{"project": "gardenio", "tag": "2026.300.1"})
	if code/100 != 2 || again.ID != rel.ID {
		t.Errorf("second create: %d id %q, want the first release %q", code, again.ID, rel.ID)
	}
}

func TestRelease_CreateRefusals(t *testing.T) {
	_, ms, h := newTestServer()
	seedReleaseProject(ms, "proj-gardenio", "gardenio", testGardenioReleaseSource)
	seedReleaseProject(ms, "proj-quiet", "quiet", `{}`)
	seedReleaseProject(ms, "proj-other", "other", `{"release_tag_pattern":"^v\\d+\\.\\d+\\.\\d+$"}`)
	_, otherRel := createReleaseAsAdmin(t, h, map[string]any{"project": "other", "tag": "v1.0.0"})
	if otherRel.ID == "" {
		t.Fatal("seed release of other project")
	}

	for name, body := range map[string]map[string]any{
		"tag off the pattern":         {"project": "gardenio", "tag": "v1.0.0"},
		"project with no pattern":     {"project": "quiet", "tag": "2026.300.1"},
		"project with no bead":        {"project": "nowhere", "tag": "2026.300.1"},
		"previous of another project": {"project": "gardenio", "tag": "2026.300.2", "previous": otherRel.ID},
		"cut_at not RFC 3339":         {"project": "gardenio", "tag": "2026.300.3", "cut_at": "yesterday"},
		"contents not a list":         {"project": "gardenio", "tag": "2026.300.4", "contents": map[string]any{"pr": 1}},
	} {
		if rec := doJSON(t, h, http.MethodPost, "/v1/releases", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, rec.Code, rec.Body.String())
		}
	}

	// The generic route refuses a client-written status or deployments.
	for _, fields := range []string{
		`{"project":"gardenio","tag":"2026.300.5","status":"live"}`,
		`{"project":"gardenio","tag":"2026.300.6","deployments":[{"target":"prod","since":"2026-10-27T05:00:00Z"}]}`,
	} {
		rec := doJSON(t, h, http.MethodPost, "/v1/beads", createBeadInput{
			Title: "gardenio x", Type: string(model.TypeRelease), Labels: []string{"project:gardenio"},
			Fields: json.RawMessage(fields),
		})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("generic create %s: %d %s, want 400", fields, rec.Code, rec.Body.String())
		}
	}
}

// kd release list: newest cut first, per project, each linked to the one
// before it.
func TestRelease_ListNewestFirstPerProject(t *testing.T) {
	_, ms, h := newTestServer()
	seedReleaseProject(ms, "proj-gardenio", "gardenio", testGardenioReleaseSource)
	seedReleaseProject(ms, "proj-other", "other", `{"release_tag_pattern":"^v\\d+\\.\\d+\\.\\d+$"}`)

	ids := map[string]string{}
	for _, r := range []struct{ tag, cutAt string }{
		{"2026.299.1", "2026-10-26T04:00:00Z"},
		{"2026.299.2", "2026-10-26T16:00:00Z"},
		{"2026.300.1", "2026-10-27T04:00:00Z"},
	} {
		code, rel := createReleaseAsAdmin(t, h, map[string]any{"project": "gardenio", "tag": r.tag, "cut_at": r.cutAt})
		if code != http.StatusCreated {
			t.Fatalf("create %s: %d", r.tag, code)
		}
		ids[r.tag] = rel.ID
	}
	createReleaseAsAdmin(t, h, map[string]any{"project": "other", "tag": "v1.0.0", "cut_at": "2026-10-28T00:00:00Z"})

	rec := doJSON(t, h, http.MethodGet, "/v1/releases?project=gardenio", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Releases []releaseView `json:"releases"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	var tags []string
	for _, r := range resp.Releases {
		tags = append(tags, r.Tag)
	}
	if want := []string{"2026.300.1", "2026.299.2", "2026.299.1"}; !slices.Equal(tags, want) {
		t.Fatalf("list order = %v, want %v", tags, want)
	}
	if got := resp.Releases[0]; got.Previous != ids["2026.299.2"] || got.PreviousTag != "2026.299.2" {
		t.Errorf("2026.300.1 previous = %q/%q, want %s/2026.299.2", got.Previous, got.PreviousTag, ids["2026.299.2"])
	}
	if got := resp.Releases[2]; got.Previous != "" {
		t.Errorf("the first release has previous %q", got.Previous)
	}

	// show by tag with --project, and by id.
	for _, path := range []string{"/v1/releases/2026.299.2?project=gardenio", "/v1/releases/" + ids["2026.299.2"]} {
		rec := doJSON(t, h, http.MethodGet, path, nil)
		if rec.Code != http.StatusOK || decodeRelease(t, rec.Body.Bytes()).ID != ids["2026.299.2"] {
			t.Errorf("GET %s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	if rec := doJSON(t, h, http.MethodGet, "/v1/releases/proj-gardenio", nil); rec.Code != http.StatusNotFound {
		t.Errorf("GET a non-release by id: %d, want 404", rec.Code)
	}
}

// kd release deploy: the deployed release goes live, the release that held
// the target is superseded with its row closed, a repeat changes nothing, and
// a rollback reverses the two.
func TestRelease_DeploySupersedesPreviousOnTarget(t *testing.T) {
	_, ms, h := newTestServer()
	seedReleaseProject(ms, "proj-gardenio", "gardenio", testGardenioReleaseSource)
	_, r1 := createReleaseAsAdmin(t, h, map[string]any{"project": "gardenio", "tag": "2026.299.1", "cut_at": "2026-10-26T04:00:00Z"})
	_, r2 := createReleaseAsAdmin(t, h, map[string]any{"project": "gardenio", "tag": "2026.299.2", "cut_at": "2026-10-26T16:00:00Z"})

	deploy := func(ref string, body map[string]any) (int, releaseView) {
		t.Helper()
		path, query, _ := strings.Cut(ref, "?")
		path = "/v1/releases/" + path + "/deployments"
		if query != "" {
			path += "?" + query
		}
		rec := doJSON(t, h, http.MethodPost, path, body)
		if rec.Code != http.StatusOK {
			return rec.Code, releaseView{}
		}
		return rec.Code, decodeRelease(t, rec.Body.Bytes())
	}
	view := func(id string) releaseView { return releaseViewOf(ms.beads[id]) }

	code, got := deploy(r1.ID, map[string]any{"target": "prod", "since": "2026-10-26T05:00:00Z", "revision": "411"})
	if code != http.StatusOK || got.Status != releaseStatusLive {
		t.Fatalf("deploy r1: %d %+v", code, got)
	}
	if d := got.Deployments; len(d) != 1 || d[0].Kind != "k3s" || d[0].ImageTag != "2026.299.1" || d[0].Via != "script" || d[0].By == "" {
		t.Errorf("r1 row = %+v", d)
	}

	code, got = deploy("2026.299.2?project=gardenio", map[string]any{"target": "prod", "since": "2026-10-26T17:00:00Z", "revision": "412"})
	if code != http.StatusOK || got.ID != r2.ID || got.Status != releaseStatusLive {
		t.Fatalf("deploy r2 by tag: %d %+v", code, got)
	}
	old := view(r1.ID)
	if old.Status != releaseStatusSuperseded || old.Deployments[0].Until != "2026-10-26T17:00:00Z" {
		t.Errorf("r1 after r2 deploy = %s %+v, want superseded, closed at r2's since", old.Status, old.Deployments)
	}

	before := string(ms.beads[r2.ID].Fields)
	if code, _ := deploy(r2.ID, map[string]any{"target": "prod", "revision": "412"}); code != http.StatusOK {
		t.Fatalf("repeat deploy: %d", code)
	}
	if string(ms.beads[r2.ID].Fields) != before {
		t.Errorf("a repeat of the open row rewrote the release")
	}

	// Rollback onto r1.
	if code, got := deploy(r1.ID, map[string]any{"target": "prod", "since": "2026-10-26T18:00:00Z", "revision": "413"}); code != http.StatusOK || got.Status != releaseStatusLive || len(got.Deployments) != 2 {
		t.Fatalf("rollback: %d %+v", code, got)
	}
	if v := view(r2.ID); v.Status != releaseStatusSuperseded {
		t.Errorf("r2 after rollback = %s, want superseded", v.Status)
	}
	open := 0
	for _, id := range []string{r1.ID, r2.ID} {
		for _, d := range view(id).Deployments {
			if d.Target == "prod" && d.Until == "" {
				open++
			}
		}
	}
	if open != 1 {
		t.Errorf("%d open prod rows, want exactly 1", open)
	}

	if code, _ := deploy(r1.ID, map[string]any{"target": "staging"}); code != http.StatusBadRequest {
		t.Errorf("undeclared target: %d, want 400", code)
	}
	if code, _ := deploy(r1.ID, map[string]any{"target": "prod", "via": "carrier-pigeon"}); code != http.StatusBadRequest {
		t.Errorf("unknown via: %d, want 400", code)
	}
	if code, _ := deploy("gd-nope", map[string]any{"target": "prod"}); code != http.StatusNotFound {
		t.Errorf("unknown release: %d, want 404", code)
	}
}

// A via=github deployment to an environment the project does not declare
// appends it to release_targets as a github-environment target and records the
// row; any other via still refuses an undeclared target.
func TestRelease_DeployGitHubDeclaresUnknownEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name, source string
	}{
		{"list", testGardenioReleaseSource},
		{"string", `{"release_tag_pattern":"^v\\d+$","release_targets":"[{\"name\":\"prod\",\"kind\":\"k3s\"}]"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ms, h := newTestServer()
			seedReleaseProject(ms, "proj-gardenio", "gardenio", tc.source)
			tag := "2026.305.1"
			if tc.name == "string" {
				tag = "v1"
			}
			_, rel := createReleaseAsAdmin(t, h, map[string]any{"project": "gardenio", "tag": tag})
			path := "/v1/releases/" + rel.ID + "/deployments"

			if rec := doJSON(t, h, http.MethodPost, path, map[string]any{"target": "staging", "via": "script"}); rec.Code != http.StatusBadRequest {
				t.Fatalf("via=script to an undeclared target: %d, want 400", rec.Code)
			}
			rec := doJSON(t, h, http.MethodPost, path, map[string]any{"target": "staging", "via": "github", "by": "github:octocat", "revision": "77"})
			if rec.Code != http.StatusOK {
				t.Fatalf("via=github to an undeclared target: %d %s", rec.Code, rec.Body.String())
			}
			got := decodeRelease(t, rec.Body.Bytes())
			if d := got.Deployments; len(d) != 1 || d[0].Target != "staging" || d[0].Kind != githubEnvironmentKind || d[0].Via != "github" || d[0].By != "github:octocat" || d[0].Revision != "77" {
				t.Errorf("row = %+v", d)
			}
			src, err := parseReleaseSource(ms.beads["proj-gardenio"].Fields)
			if err != nil {
				t.Fatalf("project release source after the deploy: %v (%s)", err, ms.beads["proj-gardenio"].Fields)
			}
			want := []releaseTarget{{Name: "prod", Kind: "k3s"}, {Name: "staging", Kind: githubEnvironmentKind}}
			if !slices.Equal(src.Targets, want) {
				t.Errorf("release_targets = %+v, want %+v", src.Targets, want)
			}
			if tc.name == "string" {
				if _, ok := releaseFieldMap(ms.beads["proj-gardenio"].Fields)["release_targets"].(string); !ok {
					t.Errorf("release_targets was a string and is now %s", ms.beads["proj-gardenio"].Fields)
				}
			}
			if reason := releaseTargetsShapeError(parseFieldsToStringMap(ms.beads["proj-gardenio"].Fields)["release_targets"]); reason != "" {
				t.Errorf("written release_targets fails the project validator: %s", reason)
			}

			// The second environment deploy finds it declared: no second append.
			before := string(ms.beads["proj-gardenio"].Fields)
			if rec := doJSON(t, h, http.MethodPost, path, map[string]any{"target": "staging", "via": "github", "revision": "78"}); rec.Code != http.StatusOK {
				t.Fatalf("second deploy: %d %s", rec.Code, rec.Body.String())
			}
			if string(ms.beads["proj-gardenio"].Fields) != before {
				t.Errorf("a deploy to a declared target rewrote the project")
			}
		})
	}
}

// A generic PATCH keeps a release's key and leaves status and deployments to
// the daemon; a built_at moves cut to built (gd-Dvour96HjO).
func TestRelease_PatchGuard(t *testing.T) {
	_, ms, h := newTestServer()
	seedReleaseProject(ms, "proj-gardenio", "gardenio", testGardenioReleaseSource)
	_, rel := createReleaseAsAdmin(t, h, map[string]any{"project": "gardenio", "tag": "2026.300.1"})

	for _, fields := range []string{`{"tag":"2026.300.9"}`, `{"project":"other"}`, `{"status":"live"}`, `{"deployments":[]}`} {
		rec := doJSON(t, h, http.MethodPatch, "/v1/beads/"+rel.ID, map[string]any{"fields": json.RawMessage(fields)})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("PATCH %s: %d %s, want 400", fields, rec.Code, rec.Body.String())
		}
	}
	rec := doJSON(t, h, http.MethodPatch, "/v1/beads/"+rel.ID, map[string]any{"fields": json.RawMessage(`{"built_at":"2026-10-27T04:40:00Z"}`)})
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH built_at: %d %s", rec.Code, rec.Body.String())
	}
	if v := releaseViewOf(ms.beads[rel.ID]); v.Status != releaseStatusBuilt {
		t.Errorf("status after built_at = %q, want built", v.Status)
	}
}

// Only the release captain, the operator and gardenio-admin write releases:
// a plain crew's own token is refused on every write route, the release
// captain's is admitted.
func TestRelease_WritersOnly(t *testing.T) {
	const (
		adminToken   = "admin-release-secret"
		crewToken    = "crew-release-tok"
		captainToken = "captain-release-tok"
	)
	fc := NewFleetConfig("gardenio", nil)
	fc.RegisterTokenWithSubject(crewToken, "agent-gd-crew1", "gardenio", nil, nil, time.Time{}, "gardenio-crew-1")
	fc.RegisterTokenWithSubject(captainToken, "agent-gd-captain1", "gardenio", nil, nil, time.Time{}, "gardenio-release-captain-1")
	ts, _, ms := newE2EServer(t, adminToken, fc)
	seedReleaseProject(ms, "proj-gardenio", "gardenio", testGardenioReleaseSource)
	for id, role := range map[string]string{"gd-crew1": "crew", "gd-captain1": "release-captain"} {
		ms.beads[id] = &model.Bead{
			ID: id, Title: id, Type: model.BeadType("agent"), Status: model.StatusOpen, Fleet: "default",
			Fields: json.RawMessage(`{"role":"` + role + `","project":"gardenio"}`),
		}
		ms.labels[id] = []string{"project:gardenio"}
	}

	do := func(token, method, path string, body any) (int, string) {
		t.Helper()
		resp := e2eRequest(t, ts, method, path, token, body)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b)
	}

	release := map[string]any{"project": "gardenio", "tag": "2026.301.1"}
	for _, token := range []string{crewToken, adminToken} {
		if code, body := do(token, http.MethodPost, "/v1/releases", release); code != http.StatusForbidden {
			t.Errorf("create as %s: %d %s, want 403", token, code, body)
		}
		generic := createBeadInput{Title: "gardenio 2026.301.1", Type: string(model.TypeRelease), Labels: []string{"project:gardenio"},
			Fields: json.RawMessage(`{"project":"gardenio","tag":"2026.301.1"}`)}
		if code, body := do(token, http.MethodPost, "/v1/beads", generic); code != http.StatusForbidden {
			t.Errorf("generic create as %s: %d %s, want 403", token, code, body)
		}
	}

	code, body := do(captainToken, http.MethodPost, "/v1/releases", release)
	if code != http.StatusCreated {
		t.Fatalf("create as release captain: %d %s", code, body)
	}
	rel := decodeRelease(t, []byte(body))
	if rel.CutBy != "gardenio-release-captain-1" {
		t.Errorf("cut_by = %q, want the captain's agent name", rel.CutBy)
	}

	deployPath := "/v1/releases/" + rel.ID + "/deployments"
	if code, body := do(crewToken, http.MethodPost, deployPath, map[string]any{"target": "prod"}); code != http.StatusForbidden {
		t.Errorf("deploy as crew: %d %s, want 403", code, body)
	}
	if releaseViewOf(ms.beads[rel.ID]).Status != releaseStatusCut {
		t.Errorf("a refused deploy changed the release")
	}
	if code, body := do(captainToken, http.MethodPost, deployPath, map[string]any{"target": "prod"}); code != http.StatusOK {
		t.Errorf("deploy as release captain: %d %s", code, body)
	}
	repair := map[string]any{"project": "gardenio", "dry_run": true}
	if code, body := do(crewToken, http.MethodPost, "/v1/releases/repair-deployments", repair); code != http.StatusForbidden {
		t.Errorf("repair as crew: %d %s, want 403", code, body)
	}
	if code, body := do(captainToken, http.MethodPost, "/v1/releases/repair-deployments", repair); code != http.StatusOK {
		t.Errorf("repair as release captain: %d %s", code, body)
	}

	// Reads are open to the project's readers.
	if code, body := do(crewToken, http.MethodGet, "/v1/releases?project=gardenio", nil); code != http.StatusOK || !json.Valid([]byte(body)) {
		t.Errorf("list as crew: %d %s", code, body)
	}
	if code, _ := do(crewToken, http.MethodGet, "/v1/releases/"+rel.ID, nil); code != http.StatusOK {
		t.Errorf("show as crew: %d", code)
	}
}

// The release source on a project bead: a pattern that does not compile or a
// target list the deploy route cannot read is refused at write.
func TestProjectReleaseSourceValidation(t *testing.T) {
	for _, tc := range []struct {
		fields string
		ok     bool
	}{
		{testGardenioReleaseSource, true},
		{`{"release_tag_pattern":"^v(\\d+$"}`, false},
		{`{"release_targets":[{"kind":"k3s"}]}`, false},
		{`{"release_targets":[{"name":"prod"},{"name":"prod"}]}`, false},
		{`{"release_targets":{"name":"prod"}}`, false},
		{`{"release_branch":"main"}`, true},
	} {
		res := validateProjectFields(json.RawMessage(tc.fields), "gardenio")
		if (res.RejectReason == "") != tc.ok {
			t.Errorf("validateProjectFields(%s) reject=%q, want ok=%v", tc.fields, res.RejectReason, tc.ok)
		}
	}
}

// A backfilled deployment older than the target's newest row is history: it
// goes in closed at the next row's since, closes the row before it, and the
// release that runs there now keeps its open row. A replay changes nothing.
func TestRelease_DeployBackdatedRowIsHistory(t *testing.T) {
	_, ms, h := newTestServer()
	seedReleaseProject(ms, "proj-gardenio", "gardenio", testGardenioReleaseSource)
	var ids []string
	for _, tag := range []string{"2026.299.1", "2026.299.2", "2026.300.1"} {
		_, r := createReleaseAsAdmin(t, h, map[string]any{"project": "gardenio", "tag": tag})
		ids = append(ids, r.ID)
	}
	deploy := func(id, since, revision string) {
		t.Helper()
		rec := doJSON(t, h, http.MethodPost, "/v1/releases/"+id+"/deployments",
			map[string]any{"target": "prod", "since": since, "revision": revision, "by": "backfill"})
		if rec.Code != http.StatusOK {
			t.Fatalf("deploy %s at %s: %d %s", id, since, rec.Code, rec.Body.String())
		}
	}
	view := func(id string) releaseView { return releaseViewOf(ms.beads[id]) }

	deploy(ids[2], "2026-10-27T05:00:00Z", "414") // what runs now
	deploy(ids[0], "2026-10-26T05:00:00Z", "411") // backfill, oldest first
	deploy(ids[1], "2026-10-26T17:00:00Z", "412")

	for i, want := range []struct {
		status, until string
	}{
		{releaseStatusSuperseded, "2026-10-26T17:00:00Z"},
		{releaseStatusSuperseded, "2026-10-27T05:00:00Z"},
		{releaseStatusLive, ""},
	} {
		v := view(ids[i])
		if v.Status != want.status || len(v.Deployments) != 1 || v.Deployments[0].Until != want.until {
			t.Errorf("%s = %s %+v, want %s until %q", v.Tag, v.Status, v.Deployments, want.status, want.until)
		}
	}

	before := string(ms.beads[ids[0]].Fields)
	deploy(ids[0], "2026-10-26T05:00:00Z", "411")
	if string(ms.beads[ids[0]].Fields) != before {
		t.Errorf("a replayed backfill row rewrote the release")
	}

	// A rollback onto the oldest release, now: it runs on prod again and the
	// newest is superseded; its rows stay in since order.
	deploy(ids[0], "2026-10-28T05:00:00Z", "415")
	if v := view(ids[2]); v.Status != releaseStatusSuperseded || v.Deployments[0].Until != "2026-10-28T05:00:00Z" {
		t.Errorf("after rollback %s = %s %+v, want superseded at the rollback", v.Tag, v.Status, v.Deployments)
	}
	v := view(ids[0])
	if v.Status != releaseStatusLive || len(v.Deployments) != 2 || v.Deployments[1].Revision != "415" || v.Deployments[1].Until != "" {
		t.Errorf("rolled-back release = %s %+v, want live on revision 415", v.Status, v.Deployments)
	}
}

// A helm_deploy escalation closed success with the rolled tag records the
// deployment, via escalation, by its executor. Any other operation, a failure
// or a result with no tag records nothing.
func TestRelease_HelmDeployEscalationCloseRecordsDeployment(t *testing.T) {
	_, ms, h := newTestServer()
	seedReleaseProject(ms, "proj-gardenio", "gardenio", testGardenioReleaseSource)
	_, rel := createReleaseAsAdmin(t, h, map[string]any{"project": "gardenio", "tag": "2026.304.1"})

	seedEsc := func(id, op string) {
		ms.beads[id] = &model.Bead{
			ID: id, Title: "ship it", Type: model.TypeEscalation, Status: model.StatusOpen, Fleet: "default",
			Fields: json.RawMessage(`{"operation_type":"` + op + `","context":"next window"}`),
		}
		ms.labels[id] = []string{"project:gardenio"}
	}
	closeEsc := func(id, status string, result any) map[string]any {
		t.Helper()
		rec := doJSON(t, h, http.MethodPost, "/v1/escalations/"+id+"/result",
			map[string]any{"result_status": status, "executor": "joyousaversion", "result": result})
		if rec.Code != http.StatusOK {
			t.Fatalf("close %s: %d %s", id, rec.Code, rec.Body.String())
		}
		var resp map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		return resp
	}

	seedEsc("gd-esc-other", "k8s_rbac")
	closeEsc("gd-esc-other", "success", map[string]any{"tag": "2026.304.1", "revision": 431})
	seedEsc("gd-esc-fail", "helm_deploy")
	closeEsc("gd-esc-fail", "failure", map[string]any{"tag": "2026.304.1", "revision": 431})
	seedEsc("gd-esc-prose", "helm_deploy")
	closeEsc("gd-esc-prose", "success", "rolled it")
	if v := releaseViewOf(ms.beads[rel.ID]); len(v.Deployments) != 0 {
		t.Fatalf("deployments after non-recording closes = %+v, want none", v.Deployments)
	}

	seedEsc("gd-esc-deploy", "helm_deploy")
	resp := closeEsc("gd-esc-deploy", "success", map[string]any{"tag": "2026.304.1", "revision": 431})
	if dep, _ := resp["deployment"].(map[string]any); dep == nil || dep["release"] != rel.ID || dep["error"] != nil {
		t.Errorf("response deployment = %v, want the release recorded", resp["deployment"])
	}
	v := releaseViewOf(ms.beads[rel.ID])
	if v.Status != releaseStatusLive || len(v.Deployments) != 1 {
		t.Fatalf("release after helm_deploy close = %s %+v, want live on one row", v.Status, v.Deployments)
	}
	if d := v.Deployments[0]; d.Target != "prod" || d.Revision != "431" || d.Via != "escalation" || d.By != "joyousaversion" || d.ImageTag != "2026.304.1" {
		t.Errorf("row = %+v", d)
	}

	seedEsc("gd-esc-unknown", "helm_deploy")
	resp = closeEsc("gd-esc-unknown", "success", map[string]any{"tag": "2026.399.9"})
	if dep, _ := resp["deployment"].(map[string]any); dep == nil || dep["error"] == nil {
		t.Errorf("unrecorded tag: deployment = %v, want an error note", resp["deployment"])
	}
}

// A release that never ran while a newer one went out reads skipped_by, the
// oldest newer release that ran (gd-UbcDWJtcOA); its status stays built. A
// release that ran, or one newer than every release that ran, has none.
func TestRelease_SkippedByNewerReleaseThatRan(t *testing.T) {
	_, ms, h := newTestServer()
	seedReleaseProject(ms, "proj-gardenio", "gardenio", testGardenioReleaseSource)
	ids := map[string]string{}
	for _, r := range []struct{ tag, cutAt string }{
		{"2026.309.4", "2026-11-05T03:11:50Z"},
		{"2026.309.5", "2026-11-05T03:28:12Z"},
		{"2026.309.16", "2026-11-05T13:39:13Z"},
		{"2026.309.17", "2026-11-05T14:34:20Z"},
		{"2026.309.18", "2026-11-05T15:38:31Z"},
	} {
		body := map[string]any{"project": "gardenio", "tag": r.tag, "cut_at": r.cutAt, "built_at": r.cutAt}
		code, rel := createReleaseAsAdmin(t, h, body)
		if code != http.StatusCreated {
			t.Fatalf("create %s: %d", r.tag, code)
		}
		ids[r.tag] = rel.ID
	}
	for _, d := range []struct{ tag, since, rev string }{
		{"2026.309.16", "2026-11-05T14:23:20Z", "285"},
		{"2026.309.17", "2026-11-05T15:20:31Z", "286"},
	} {
		rec := doJSON(t, h, http.MethodPost, "/v1/releases/"+ids[d.tag]+"/deployments",
			map[string]any{"target": "prod", "since": d.since, "revision": d.rev})
		if rec.Code != http.StatusOK {
			t.Fatalf("deploy %s: %d %s", d.tag, rec.Code, rec.Body.String())
		}
	}
	want := map[string]string{"2026.309.4": ids["2026.309.16"], "2026.309.5": ids["2026.309.16"]}

	rec := doJSON(t, h, http.MethodGet, "/v1/releases?project=gardenio", nil)
	var resp struct {
		Releases []releaseView `json:"releases"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("list: %v: %s", err, rec.Body.String())
	}
	if len(resp.Releases) != 5 {
		t.Fatalf("list returned %d releases, want 5", len(resp.Releases))
	}
	for _, v := range resp.Releases {
		if v.SkippedBy != want[v.Tag] {
			t.Errorf("list %s (%s): skipped_by = %q, want %q", v.Tag, v.Status, v.SkippedBy, want[v.Tag])
		}
		if want[v.Tag] != "" && v.Status != releaseStatusBuilt {
			t.Errorf("list %s: status = %s, want built", v.Tag, v.Status)
		}
	}
	for tag, wantBy := range map[string]string{"2026.309.5": want["2026.309.5"], "2026.309.18": "", "2026.309.16": ""} {
		rec := doJSON(t, h, http.MethodGet, "/v1/releases/"+tag+"?project=gardenio", nil)
		if got := decodeRelease(t, rec.Body.Bytes()).SkippedBy; rec.Code != http.StatusOK || got != wantBy {
			t.Errorf("show %s: %d skipped_by = %q, want %q", tag, rec.Code, got, wantBy)
		}
	}
}
