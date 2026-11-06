package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/releaserecord"
)

const (
	relRepo  = "acme/widget"
	relPlan  = "gd-PlanWidg001"
	relStep  = "gd-StepAx00001"
	relTask  = "gd-TaskBx00002" // a child of relStep
	shaV100  = "a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0"
	shaMerge = "c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1"
	shaV110  = "c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3"
)

// relDaemon is an in-memory daemon for the recorder: projects, releases, MR
// beads, work beads with their edges, and plan comments.
type relDaemon struct {
	mu       sync.Mutex
	projects map[string]beadsapi.ProjectInfo
	releases map[string]*beadsapi.Release // project/tag
	beads    map[string]*beadsapi.BeadDetail
	deps     map[string][]beadsapi.Dependency
	comments map[string][]beadsapi.Comment
	creates  int
	updates  int
}

func newRelDaemon() *relDaemon {
	d := &relDaemon{
		projects: map[string]beadsapi.ProjectInfo{
			"widget": {Name: "widget", GitURL: "https://github.com/acme/widget.git",
				ReleaseSource: &beadsapi.ReleaseSource{TagPattern: `^v\d+\.\d+\.\d+$`, Branch: "main"}},
			// Same repo, no release_tag_pattern: no releases.
			"widget-docs": {Name: "widget-docs", GitURL: "https://github.com/acme/widget"},
			// Not on GitHub: out of scope.
			"lab": {Name: "lab", GitURL: "https://gitlab.com/acme/lab.git",
				ReleaseSource: &beadsapi.ReleaseSource{TagPattern: `^v.*$`}},
		},
		releases: map[string]*beadsapi.Release{},
		beads:    map[string]*beadsapi.BeadDetail{},
		deps:     map[string][]beadsapi.Dependency{},
		comments: map[string][]beadsapi.Comment{},
	}
	d.bead(relPlan, "epic", "plan")
	d.bead(relStep, "task")
	d.bead(relTask, "task")
	d.parent(relStep, relPlan)
	d.parent(relTask, relStep)
	d.mr("gd-MRbead0021", 21, "Add the widget frame", relStep)
	d.mr("gd-MRbead0022", 22, "Fix the hinge", relTask)
	return d
}

func (d *relDaemon) bead(id, typ string, labels ...string) {
	d.beads[id] = &beadsapi.BeadDetail{ID: id, Type: typ, Status: "closed", Labels: labels, Fields: map[string]string{}}
}

func (d *relDaemon) parent(child, parent string) {
	d.deps[child] = append(d.deps[child], beadsapi.Dependency{BeadID: child, DependsOnID: parent, Type: "parent-child"})
}

func (d *relDaemon) mr(id string, pr int, title, task string) {
	d.beads[id] = &beadsapi.BeadDetail{ID: id, Type: "merge-request", Status: "closed", Fields: map[string]string{
		"mr_url": fmt.Sprintf("https://github.com/%s/pull/%d", relRepo, pr), "mr_title": title}}
	d.deps[id] = append(d.deps[id], beadsapi.Dependency{BeadID: id, DependsOnID: task, Type: "triggered-by"})
}

func relNotFound() error {
	return &beadsapi.APIError{StatusCode: http.StatusNotFound, Message: "not found"}
}

func (d *relDaemon) ListProjectBeads(context.Context) (map[string]beadsapi.ProjectInfo, error) {
	return d.projects, nil
}

func (d *relDaemon) GetProjectBead(_ context.Context, name string) (beadsapi.ProjectInfo, error) {
	if p, ok := d.projects[name]; ok {
		return p, nil
	}
	return beadsapi.ProjectInfo{}, fmt.Errorf("project %q not found", name)
}

func (d *relDaemon) ListReleases(_ context.Context, project, _ string, _ int) ([]beadsapi.Release, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []beadsapi.Release
	for _, r := range d.releases {
		if r.Project == project {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (d *relDaemon) GetRelease(_ context.Context, ref, project string) (*beadsapi.Release, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for key, r := range d.releases {
		if key == project+"/"+ref || (project == "" && r.ID == ref) {
			cp := *r
			return &cp, nil
		}
	}
	return nil, relNotFound()
}

func (d *relDaemon) CreateRelease(_ context.Context, req beadsapi.CreateReleaseRequest) (*beadsapi.Release, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	key := req.Project + "/" + req.Tag
	if r, ok := d.releases[key]; ok {
		if req.Tag == "" {
			return nil, &beadsapi.APIError{StatusCode: http.StatusConflict, Message: "already has an open release"}
		}
		return r, nil
	}
	d.creates++
	r := &beadsapi.Release{ID: "gd-rel-" + req.Tag, Project: req.Project, Tag: req.Tag, SHA: req.SHA,
		PreviousTag: req.PreviousTag, CutAt: req.CutAt, Source: req.Source, Contents: req.Contents, Status: "cut"}
	if req.Tag == "" {
		r.ID, r.Status = fmt.Sprintf("gd-rel-open-%d", d.creates), beadsapi.ReleaseStatusOpen
	}
	d.releases[key] = r
	return r, nil
}

func (d *relDaemon) UpdateBeadFieldsTyped(_ context.Context, id string, f map[string]any) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for key, r := range d.releases {
		if r.ID != id {
			continue
		}
		d.updates++
		if tag, ok := f["tag"].(string); ok && r.Tag == "" {
			delete(d.releases, key)
			d.releases[r.Project+"/"+tag] = r
			r.Tag, r.Status = tag, "cut"
			r.Source, _ = f["source"].(string)
			r.CutAt, _ = f["cut_at"].(string)
		}
		if v, ok := f["contents"]; ok {
			r.Contents, _ = json.Marshal(v)
		}
		if v, ok := f["sha"].(string); ok {
			r.SHA = v
		}
		if v, ok := f["previous_tag"].(string); ok {
			r.PreviousTag = v
		}
		return nil
	}
	return relNotFound()
}

func (d *relDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	res := &beadsapi.ListBeadsResult{}
	for _, b := range d.beads {
		if len(q.Types) > 0 && b.Type != q.Types[0] {
			continue
		}
		match := true
		for k, v := range q.Fields {
			match = match && b.Fields[k] == v
		}
		if match {
			res.Beads = append(res.Beads, b)
		}
	}
	return res, nil
}

func (d *relDaemon) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	if b, ok := d.beads[id]; ok {
		return b, nil
	}
	return nil, relNotFound()
}

func (d *relDaemon) GetDependencies(_ context.Context, id string) ([]beadsapi.Dependency, error) {
	return d.deps[id], nil
}

func (d *relDaemon) GetComments(_ context.Context, id string) ([]beadsapi.Comment, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.comments[id], nil
}

func (d *relDaemon) AddComment(_ context.Context, id, author, text string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.comments[id] = append(d.comments[id], beadsapi.Comment{Author: author, Text: text})
	return nil
}

func (d *relDaemon) AppendReleaseContents(_ context.Context, ref string, rows any) (*beadsapi.Release, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, r := range d.releases {
		if r.ID == ref && r.Tag == "" {
			have, _ := releaserecord.ParseRows(r.Contents)
			raw, _ := json.Marshal(rows)
			add, _ := releaserecord.ParseRows(raw)
			r.Contents, _ = json.Marshal(append(have, add...))
			cp := *r
			return &cp, nil
		}
	}
	return nil, &beadsapi.APIError{StatusCode: http.StatusConflict, Message: "release is cut"}
}

// staticView is the bridge's own (static-token) client: the daemon refuses
// it every release write, so the recorder must route those to the writer.
type staticView struct{ *relDaemon }

var errStaticRelease = errors.New("HTTP 403: only the release captain, the operator and gardenio-admin may write a release")

func (staticView) CreateRelease(context.Context, beadsapi.CreateReleaseRequest) (*beadsapi.Release, error) {
	return nil, errStaticRelease
}

func (staticView) UpdateBeadFieldsTyped(context.Context, string, map[string]any) error {
	return errStaticRelease
}

// fakeGitHub serves the REST calls releaserecord.GitHub makes for acme/widget:
// tags v1.0.0 and v1.1.0 (and nightly-1, not a release), and two first-parent
// commits between them, merge #21 and squash #22.
func fakeGitHub(t *testing.T) (*githubCommenter, map[string]int) {
	t.Helper()
	commit := func(sha, msg string, parents ...string) map[string]any {
		var ps []map[string]string
		for _, p := range parents {
			ps = append(ps, map[string]string{"sha": p})
		}
		return map[string]any{"sha": sha, "parents": ps,
			"commit": map[string]any{"message": msg, "committer": map[string]string{"date": "2026-10-30T11:00:00Z"}}}
	}
	routes := map[string]any{
		"/repos/acme/widget/tags": []map[string]any{
			{"name": "nightly-1", "commit": map[string]string{"sha": shaV110}},
			{"name": "v1.1.0", "commit": map[string]string{"sha": shaV110}},
			{"name": "v1.0.0", "commit": map[string]string{"sha": shaV100}},
		},
		"/repos/acme/widget/git/ref/tags/v1.1.0":    map[string]any{"object": map[string]string{"type": "commit", "sha": shaV110}},
		"/repos/acme/widget/git/ref/tags/v1.0.0":    map[string]any{"object": map[string]string{"type": "commit", "sha": shaV100}},
		"/repos/acme/widget/git/commits/" + shaV110: map[string]any{"committer": map[string]string{"date": "2026-10-30T12:00:00+02:00"}},
		"/repos/acme/widget/git/commits/" + shaV100: map[string]any{"committer": map[string]string{"date": "2026-10-20T12:00:00Z"}},
		"/repos/acme/widget/commits/" + shaMerge:    commit(shaMerge, "Merge pull request #21 from acme/agent/frame/gd-StepAx00001\n\nAdd the widget frame", shaV100),
		"/repos/acme/widget/compare/" + shaV100 + "..." + shaV110: map[string]any{
			"total_commits": 2,
			"commits": []any{
				commit(shaMerge, "Merge pull request #21 from acme/agent/frame/gd-StepAx00001\n\nAdd the widget frame",
					shaV100, "b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1"),
				commit(shaV110, "Fix the hinge (#22)", shaMerge),
			},
		},
	}
	var mu sync.Mutex
	hits := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path]++
		mu.Unlock()
		if r.Header.Get("Authorization") == "" {
			t.Errorf("%s without an installation token", r.URL.Path)
		}
		body, ok := routes[r.URL.Path]
		if !ok {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return &githubCommenter{tokens: stubTokens{}, baseURL: srv.URL, hc: srv.Client()}, hits
}

func newTestRecorder(t *testing.T) (*releaseRecorder, *relDaemon, map[string]int) {
	t.Helper()
	d := newRelDaemon()
	gh, hits := fakeGitHub(t)
	return newReleaseRecorder(releaseDaemon{Daemon: staticView{d}, writer: d}, d, gh, 0, quiet()), d, hits
}

func tagEvent(t *testing.T, typ, fixtureName string) Event {
	ev := Event{DeliveryID: typ + "-" + fixtureName, Type: typ, Payload: fixture(t, fixtureName)}
	fillEnvelope(&ev)
	return ev
}

// A tag push on a semver tenant repo writes the release: previous v1.0.0, two
// PRs each joined to its MR bead and task, both under the plan, which gets
// one "shipped in" comment. The write goes through the machine principal.
func TestRelease_TagPushRecordsTwoPRsAndAPlan(t *testing.T) {
	h, d, _ := newTestRecorder(t)
	if err := h.Handle(context.Background(), tagEvent(t, "push", "push_tag_semver")); err != nil {
		t.Fatal(err)
	}
	rel := d.releases["widget/v1.1.0"]
	if rel == nil {
		t.Fatalf("no release written; releases = %v", d.releases)
	}
	if rel.SHA != shaV110 || rel.PreviousTag != "v1.0.0" || rel.Source != releaserecord.SourceWebhook || rel.CutAt != "2026-10-30T10:00:00Z" {
		t.Errorf("release = %+v", rel)
	}
	var rows []releaserecord.Row
	if err := json.Unmarshal(rel.Contents, &rows); err != nil {
		t.Fatal(err)
	}
	got := fmt.Sprint(len(rows))
	for _, r := range rows {
		got += fmt.Sprintf(" #%d %s %s %s %s|", r.PR, r.MRBead, r.Task, r.Step, r.Plan)
	}
	want := "2 #22 gd-MRbead0022 gd-TaskBx00002 gd-StepAx00001 gd-PlanWidg001|" +
		" #21 gd-MRbead0021 gd-StepAx00001 gd-StepAx00001 gd-PlanWidg001|"
	if got != want {
		t.Errorf("rows =\n%s\nwant\n%s", got, want)
	}
	if c := d.comments[relPlan]; len(c) != 1 || !strings.Contains(c[0].Text, releaserecord.ShippedMarker("widget", "v1.1.0")) || c[0].Author != bridgeActor {
		t.Errorf("plan comments = %+v", c)
	}
	if len(d.releases) != 2 || d.releases["widget/"] == nil {
		t.Errorf("releases = %v, want widget/v1.1.0 and widget's next open release (widget-docs and lab have no GitHub release source)", d.releases)
	}
}

// A tag pushed while the project has an open release is cut onto that bead:
// no release is minted for the tag, the git rows replace the ones merges
// stamped (a row git does not show is dropped), and the next release opens.
func TestRelease_TagCutsOntoTheOpenRelease(t *testing.T) {
	h, d, _ := newTestRecorder(t)
	d.releases["widget/"] = &beadsapi.Release{ID: "gd-rel-next", Project: "widget", Status: beadsapi.ReleaseStatusOpen,
		Contents: json.RawMessage(`[{"pr":21,"title":"stamped","merge_sha":"` + shaMerge + `"},{"pr":30,"title":"reverted","merge_sha":"gone"}]`)}
	if err := h.Handle(context.Background(), tagEvent(t, "push", "push_tag_semver")); err != nil {
		t.Fatal(err)
	}
	rel := d.releases["widget/v1.1.0"]
	if rel == nil || rel.ID != "gd-rel-next" || rel.Source != releaserecord.SourceWebhook || rel.SHA != shaV110 || rel.PreviousTag != "v1.0.0" {
		t.Fatalf("widget/v1.1.0 = %+v, want the open bead gd-rel-next cut by the webhook", rel)
	}
	rows, _ := releaserecord.ParseRows(rel.Contents)
	if len(rows) != 2 || rows[0].PR != 22 || rows[1].PR != 21 || rows[1].MRBead != "gd-MRbead0021" {
		t.Errorf("contents = %s, want git's #22 and #21 and no #30", rel.Contents)
	}
	next := d.releases["widget/"]
	if next == nil || next.ID == "gd-rel-next" || d.creates != 1 {
		t.Errorf("next = %+v, creates = %d, want one new open release", next, d.creates)
	}
}

// The same tag delivered twice (GitHub sends `create` and `push` for one tag
// push) writes once, comments once, and the second reads nothing from GitHub.
func TestRelease_SameTagTwiceWritesOnce(t *testing.T) {
	h, d, hits := newTestRecorder(t)
	ctx := context.Background()
	if err := h.Handle(ctx, tagEvent(t, "create", "create_tag_semver")); err != nil {
		t.Fatal(err)
	}
	before := fmt.Sprint(hits)
	if err := h.Handle(ctx, tagEvent(t, "push", "push_tag_semver")); err != nil {
		t.Fatal(err)
	}
	if d.creates != 2 || d.updates != 0 || len(d.comments[relPlan]) != 1 {
		t.Errorf("creates = %d updates = %d comments = %d, want 2 (the release and the next), 0, 1", d.creates, d.updates, len(d.comments[relPlan]))
	}
	if after := fmt.Sprint(hits); after != before {
		t.Errorf("the second delivery read GitHub: %s -> %s", before, after)
	}
}

// A tag that does not match the release_tag_pattern is ignored before any
// GitHub read; so are a deleted tag, a branch push and a branch create.
func TestRelease_NonMatchingTagIgnored(t *testing.T) {
	h, d, hits := newTestRecorder(t)
	ctx := context.Background()
	for _, payload := range []string{
		`{"ref":"refs/tags/nightly-1","deleted":false,"repository":{"full_name":"acme/widget"}}`,
		`{"ref":"refs/tags/v1.1.0","deleted":true,"repository":{"full_name":"acme/widget"}}`,
		`{"ref":"refs/heads/main","deleted":false,"repository":{"full_name":"acme/widget"}}`,
	} {
		ev := Event{Type: "push", Payload: []byte(payload)}
		fillEnvelope(&ev)
		if err := h.Handle(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	ev := Event{Type: "create", Payload: []byte(`{"ref":"v1.1.0","ref_type":"branch","repository":{"full_name":"acme/widget"}}`)}
	fillEnvelope(&ev)
	if err := h.Handle(ctx, ev); err != nil {
		t.Fatal(err)
	}
	if len(d.releases) != 0 || len(hits) != 0 {
		t.Errorf("releases = %v, GitHub hits = %v, want none", d.releases, hits)
	}
}

// The poll records only tags above the project's newest recorded release; a
// project with none records only its newest tag; a second pass writes nothing.
func TestRelease_PollRecordsOnlyNewTags(t *testing.T) {
	ctx := context.Background()

	h, d, _ := newTestRecorder(t)
	h.poll(ctx)
	if len(d.releases) != 2 || d.releases["widget/v1.1.0"] == nil || d.releases["widget/"] == nil {
		t.Fatalf("first poll with nothing recorded = %v, want only the newest tag v1.1.0 and the next open release", d.releases)
	}
	h.poll(ctx)
	if d.creates != 2 || d.updates != 0 {
		t.Errorf("second poll: creates = %d updates = %d, want 2 and 0", d.creates, d.updates)
	}

	h, d, hits := newTestRecorder(t)
	d.releases["widget/v1.0.0"] = &beadsapi.Release{ID: "gd-rel-v1.0.0", Project: "widget", Tag: "v1.0.0", SHA: shaV100, Contents: json.RawMessage(`[{"title":"initial"}]`)}
	h.poll(ctx)
	if d.creates != 2 || d.releases["widget/v1.1.0"].PreviousTag != "v1.0.0" {
		t.Errorf("poll after v1.0.0: creates = %d, releases = %v", d.creates, d.releases)
	}
	if hits["/repos/acme/widget/git/ref/tags/v1.0.0"] != 0 {
		t.Error("the poll re-read the recorded tag v1.0.0")
	}
}

func TestTagOfEvent(t *testing.T) {
	for _, tc := range []struct {
		typ, payload, tag string
		ok                bool
	}{
		{"create", `{"ref":"v2.0.0","ref_type":"tag"}`, "v2.0.0", true},
		{"create", `{"ref":"main","ref_type":"branch"}`, "", false},
		{"push", `{"ref":"refs/tags/v2.0.0"}`, "v2.0.0", true},
		{"push", `{"ref":"refs/tags/v2.0.0","deleted":true}`, "", false},
		{"push", `{"ref":"refs/heads/main"}`, "", false},
		{"delete", `{"ref":"v2.0.0","ref_type":"tag"}`, "", false},
		{"push", `not json`, "", false},
	} {
		tag, ok := tagOfEvent(Event{Type: tc.typ, Payload: []byte(tc.payload)})
		if ok != tc.ok || (ok && tag != tc.tag) {
			t.Errorf("%s %s = (%q, %v), want (%q, %v)", tc.typ, tc.payload, tag, ok, tc.tag, tc.ok)
		}
	}
}

func TestGitHubRepoOf(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/acme/widget.git": "acme/widget",
		"git@github.com:acme/widget.git":     "acme/widget",
		"https://gitlab.com/acme/lab.git":    "",
		"":                                   "",
	} {
		if got := githubRepoOf(beadsapi.ProjectInfo{GitURL: in}); got != want {
			t.Errorf("githubRepoOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// A PR merged on the release branch boards the open release on the machine
// principal, joined to its MR bead, step and plan as the cut joins it; one
// merged on another branch boards nothing.
func TestRelease_MergedPRBoardsTheOpenRelease(t *testing.T) {
	h, d, _ := newTestRecorder(t)
	d.releases["widget/"] = &beadsapi.Release{ID: "gd-rel-next", Project: "widget", Status: beadsapi.ReleaseStatusOpen}
	merged := func(base string) Event {
		ev := Event{Type: "pull_request", Payload: json.RawMessage(`{"action":"closed","repository":{"full_name":"acme/widget"},
			"pull_request":{"number":21,"merged":true,"merge_commit_sha":"` + shaMerge + `","base":{"ref":"` + base + `"}}}`)}
		fillEnvelope(&ev)
		return ev
	}
	for _, base := range []string{"main", "feature/x"} {
		if err := h.Handle(context.Background(), merged(base)); err != nil {
			t.Fatal(err)
		}
	}
	rows, _ := releaserecord.ParseRows(d.releases["widget/"].Contents)
	if len(rows) != 1 || rows[0].PR != 21 || rows[0].MRBead != "gd-MRbead0021" || rows[0].Step != relStep || rows[0].Plan != relPlan || rows[0].MergeSHA != shaMerge {
		t.Errorf("open release contents = %s, want #21 joined to its MR bead, step and plan", d.releases["widget/"].Contents)
	}
}
