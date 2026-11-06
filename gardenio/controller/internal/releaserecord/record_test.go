package releaserecord

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

const (
	repoURL = "https://github.com/acme/widget"
	planA   = "gd-PlanAAA001"
	planB   = "gd-PlanBBB002"
	stepA1  = "gd-StepA1x001"
	stepB1  = "gd-StepB1x002"
	taskA2  = "gd-TaskA2x003" // a child of step A1
)

// fakeDaemon is an in-memory Daemon: a project, its releases, MR beads, work
// beads with parent-child edges, and plan comments.
type fakeDaemon struct {
	project  beadsapi.ProjectInfo
	releases map[string]*beadsapi.Release // by tag
	beads    map[string]*beadsapi.BeadDetail
	deps     map[string][]beadsapi.Dependency
	comments map[string][]beadsapi.Comment
	creates  int
	updates  int
	// cutConflicts is how many cut PATCHes of an open release answer 409
	// (gd-k8Z0vMaohu: its contents changed under the write).
	cutConflicts int
}

func newFakeDaemon() *fakeDaemon {
	d := &fakeDaemon{
		project: beadsapi.ProjectInfo{
			Name:          "widget",
			GitURL:        repoURL + ".git",
			ReleaseSource: &beadsapi.ReleaseSource{TagPattern: `^20\d{2}\.\d{1,3}\.\d+$`, Branch: "main"},
		},
		releases: map[string]*beadsapi.Release{},
		beads:    map[string]*beadsapi.BeadDetail{},
		deps:     map[string][]beadsapi.Dependency{},
		comments: map[string][]beadsapi.Comment{},
	}
	d.addBead(planA, "epic", "plan")
	d.addBead(planB, "epic", "plan")
	d.addBead(stepA1, "task")
	d.addBead(stepB1, "task")
	d.addBead(taskA2, "bug")
	d.parent(stepA1, planA)
	d.parent(stepB1, planB)
	d.parent(stepB1, "gd-Thread0001") // a Slack thread parent is not a plan
	d.addBead("gd-Thread0001", "epic")
	d.parent(taskA2, stepA1)
	// PR #10 has an MR bead triggered by step A1; PR #11 is found by its
	// merge sha only (set per test, the sha is the fixture's).
	d.addMR("gd-MRbead0010", repoURL+"/pull/10", "Add the widget frame", stepA1)
	return d
}

func (d *fakeDaemon) addBead(id, typ string, labels ...string) {
	d.beads[id] = &beadsapi.BeadDetail{ID: id, Type: typ, Labels: labels, Fields: map[string]string{}}
}

func (d *fakeDaemon) parent(child, parent string) {
	d.deps[child] = append(d.deps[child], beadsapi.Dependency{BeadID: child, DependsOnID: parent, Type: "parent-child"})
}

func (d *fakeDaemon) addMR(id, url, title, task string) *beadsapi.BeadDetail {
	b := &beadsapi.BeadDetail{ID: id, Type: "merge-request", Fields: map[string]string{"mr_url": url, "mr_title": title}}
	d.beads[id] = b
	d.deps[id] = append(d.deps[id], beadsapi.Dependency{BeadID: id, DependsOnID: task, Type: "triggered-by"})
	return b
}

func notFound() error {
	return &beadsapi.APIError{StatusCode: http.StatusNotFound, Message: "not found"}
}

func (d *fakeDaemon) GetProjectBead(_ context.Context, name string) (beadsapi.ProjectInfo, error) {
	return d.project, nil
}

func (d *fakeDaemon) GetRelease(_ context.Context, ref, _ string) (*beadsapi.Release, error) {
	for _, r := range d.releases {
		if (r.Tag != "" && r.Tag == ref) || r.ID == ref {
			cp := *r
			return &cp, nil
		}
	}
	return nil, notFound()
}

// ListReleases lists the open release (keyed "") first, then the rest.
func (d *fakeDaemon) ListReleases(_ context.Context, _, status string, _ int) ([]beadsapi.Release, error) {
	var out []beadsapi.Release
	if r, ok := d.releases[""]; ok {
		out = append(out, *r)
	}
	for tag, r := range d.releases {
		if tag != "" && (status == "" || status == r.Status) {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (d *fakeDaemon) CreateRelease(_ context.Context, req beadsapi.CreateReleaseRequest) (*beadsapi.Release, error) {
	if req.Tag == "" {
		if _, ok := d.releases[""]; ok {
			return nil, &beadsapi.APIError{StatusCode: http.StatusConflict, Message: "already has an open release"}
		}
		d.creates++
		r := &beadsapi.Release{ID: "gd-rel-open-" + strconv.Itoa(d.creates), Project: req.Project, Status: beadsapi.ReleaseStatusOpen}
		d.releases[""] = r
		return r, nil
	}
	if r, ok := d.releases[req.Tag]; ok {
		return r, nil
	}
	d.creates++
	r := &beadsapi.Release{
		ID: "gd-rel-" + req.Tag, Project: req.Project, Tag: req.Tag, SHA: req.SHA, PreviousTag: req.PreviousTag,
		CutAt: req.CutAt, CutBy: req.CutBy, Source: req.Source, Contents: req.Contents, Status: "cut",
	}
	if p, ok := d.releases[req.PreviousTag]; ok {
		r.Previous = p.ID
	}
	d.releases[req.Tag] = r
	return r, nil
}

func (d *fakeDaemon) UpdateBeadFieldsTyped(_ context.Context, id string, f map[string]any) error {
	d.updates++
	for key, r := range d.releases {
		if r.ID != id {
			continue
		}
		if tag, ok := f["tag"].(string); ok && key == "" {
			if d.cutConflicts > 0 {
				d.cutConflicts--
				return &beadsapi.APIError{StatusCode: http.StatusConflict, Message: "contents changed"}
			}
			delete(d.releases, "")
			d.releases[tag] = r
			r.Tag, r.Status = tag, "cut"
		} else if ok {
			return &beadsapi.APIError{StatusCode: http.StatusBadRequest, Message: "tag was written by the cut and cannot change"}
		}
		for k, v := range f {
			raw, _ := json.Marshal(v)
			switch k {
			case "contents":
				r.Contents = raw
			case "images":
				r.Images = raw
			case "previous_tag":
				r.PreviousTag = v.(string)
			case "sha":
				r.SHA = v.(string)
			case "source":
				r.Source = v.(string)
			case "cut_by":
				r.CutBy = v.(string)
			case "built_at":
				r.BuiltAt, r.Status = v.(string), "built"
			}
		}
		return nil
	}
	return notFound()
}

func (d *fakeDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	res := &beadsapi.ListBeadsResult{}
	for _, b := range d.beads {
		if len(q.Types) > 0 && b.Type != q.Types[0] {
			continue
		}
		match := true
		for k, v := range q.Fields {
			if b.Fields[k] != v {
				match = false
			}
		}
		if match {
			res.Beads = append(res.Beads, b)
		}
	}
	return res, nil
}

func (d *fakeDaemon) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	if b, ok := d.beads[id]; ok {
		return b, nil
	}
	return nil, notFound()
}

func (d *fakeDaemon) GetDependencies(_ context.Context, id string) ([]beadsapi.Dependency, error) {
	return d.deps[id], nil
}

func (d *fakeDaemon) GetComments(_ context.Context, id string) ([]beadsapi.Comment, error) {
	return d.comments[id], nil
}

func (d *fakeDaemon) AddComment(_ context.Context, id, author, text string) error {
	d.comments[id] = append(d.comments[id], beadsapi.Comment{Author: author, Text: text})
	return nil
}

// fixtureRepo builds a repo with three release tags and one non-release tag:
//
//	2026.300.1  initial
//	            Merge pull request #10 from acme/agent/a1/gd-StepA1x001 (PR title in the body)
//	            Fix the hinge (#11)                     squash, joined on merge_sha
//	            Merge pull request #12 from acme/laptop/tidy-up (bead id in the body)
//	            chore: bump a pin                       direct commit, no PR
//	2026.300.2  (annotated)
//	            Merge pull request #13 from acme/agent/b1/gd-StepB1x002
//	v-not-a-release
//	2026.300.10
//
// It returns the repo dir and the squash commit's sha.
func fixtureRepo(t *testing.T) (string, map[string]string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	env := append(os.Environ(),
		"GIT_AUTHOR_NAME=Ada", "GIT_AUTHOR_EMAIL=ada@example.com",
		"GIT_COMMITTER_NAME=Ada", "GIT_COMMITTER_EMAIL=ada@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = dir, env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	file := 0
	commit := func(msg string) {
		file++
		if err := os.WriteFile(filepath.Join(dir, "f"+string(rune('a'+file))), []byte(msg), 0o644); err != nil {
			t.Fatal(err)
		}
		run("add", ".")
		run("commit", "-q", "-m", msg)
	}
	merge := func(branch, msg string) {
		run("checkout", "-q", "-b", branch)
		commit("work on " + branch)
		run("checkout", "-q", "main")
		run("merge", "-q", "--no-ff", branch, "-m", msg)
	}
	run("init", "-q", "-b", "main")
	commit("initial")
	run("tag", "2026.300.1")
	merge("agent/a1/gd-StepA1x001", "Merge pull request #10 from acme/agent/a1/gd-StepA1x001\n\nAdd the widget frame")
	commit("Fix the hinge (#11)")
	shas := map[string]string{"squash": run("rev-parse", "HEAD")}
	merge("laptop/tidy-up", "Merge pull request #12 from acme/laptop/tidy-up\n\nTidy the hinge for gd-TaskA2x003")
	commit("chore: bump a pin")
	shas["direct"] = run("rev-parse", "HEAD")
	run("tag", "-a", "2026.300.2", "-m", "release 2026.300.2")
	merge("agent/b1/gd-StepB1x002", "Merge pull request #13 from acme/agent/b1/gd-StepB1x002\n\nPaint the widget")
	run("tag", "v-not-a-release")
	run("tag", "-a", "2026.300.10", "-m", "release 2026.300.10")
	return dir, shas
}

func newRecorder(d *fakeDaemon, dir string) *Recorder {
	return &Recorder{Daemon: d, Source: GitSource{Dir: dir}, CommentPlans: true, Author: "test"}
}

// The contents of 2026.300.2 are the four first-parent commits after
// 2026.300.1 (never the branch commits a merge brought in), each joined: #10
// by its MR bead's mr_url to step A1 and plan A; #11 by merge_sha; #12 by the
// bead id in its body to task A2, whose step is A1; the direct commit
// unplanned.
func TestPlan_ContentsJoinedAndGroupedByPlan(t *testing.T) {
	dir, shas := fixtureRepo(t)
	d := newFakeDaemon()
	mr11 := d.addMR("gd-MRbead0011", repoURL+"/pull/999", "The hinge, fixed", stepB1)
	mr11.Fields["merge_sha"] = shas["squash"]
	r := newRecorder(d, dir)

	rec, err := r.Plan(context.Background(), "widget", "2026.300.2", SourceScript)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Previous != "2026.300.1" || !rec.Complete || rec.Tag.Tagger != "Ada" || rec.Tag.Date == "" {
		t.Fatalf("record = previous %q complete %v tagger %q date %q", rec.Previous, rec.Complete, rec.Tag.Tagger, rec.Tag.Date)
	}
	type want struct {
		pr                int
		title, task, plan string
		mrBead, step, sha string
	}
	wants := []want{ // newest first
		{0, "chore: bump a pin", "", "", "", "", shas["direct"]},
		{12, "Tidy the hinge for gd-TaskA2x003", taskA2, planA, "", stepA1, ""},
		{11, "The hinge, fixed", stepB1, planB, "gd-MRbead0011", stepB1, shas["squash"]},
		{10, "Add the widget frame", stepA1, planA, "gd-MRbead0010", stepA1, ""},
	}
	if len(rec.Rows) != len(wants) {
		t.Fatalf("rows = %+v, want %d", rec.Rows, len(wants))
	}
	for i, w := range wants {
		got := rec.Rows[i]
		if got.PR != w.pr || got.Title != w.title || got.Task != w.task || got.Plan != w.plan || got.MRBead != w.mrBead || got.Step != w.step {
			t.Errorf("row %d = %+v, want %+v", i, got, w)
		}
		if w.sha != "" && got.MergeSHA != w.sha {
			t.Errorf("row %d merge_sha = %s, want %s", i, got.MergeSHA, w.sha)
		}
		if got.PR != 0 && got.URL != repoURL+"/pull/"+strconv.Itoa(got.PR) && got.MRBead == "" {
			t.Errorf("row %d url = %s", i, got.URL)
		}
	}

	groups := GroupByPlan(rec.Rows)
	if len(groups) != 3 || groups[0].Plan != planA || len(groups[0].Rows) != 2 || groups[1].Plan != planB || groups[2].Plan != "" {
		t.Errorf("groups = %+v, want plan A (2 rows), plan B, unplanned last", groups)
	}
}

// The previous tag is by version, not by name: 2026.300.10 follows
// 2026.300.2, and a tag outside the pattern is never a release.
func TestPlan_PreviousTagByVersion(t *testing.T) {
	dir, _ := fixtureRepo(t)
	r := newRecorder(newFakeDaemon(), dir)
	rec, err := r.Plan(context.Background(), "widget", "2026.300.10", SourceScript)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Previous != "2026.300.2" || len(rec.Rows) != 1 || rec.Rows[0].PR != 13 || rec.Rows[0].Plan != planB {
		t.Errorf("record = previous %q rows %+v, want 2026.300.2 and #13 in plan B", rec.Previous, rec.Rows)
	}
	if _, err := r.Plan(context.Background(), "widget", "v-not-a-release", SourceScript); err == nil {
		t.Error("a tag outside release_tag_pattern was planned")
	}
}

// Write creates the release with its contents and comments once on each plan;
// a second write of the same tag changes nothing.
func TestWrite_CreatesOnceAndCommentsPlansOnce(t *testing.T) {
	dir, _ := fixtureRepo(t)
	d := newFakeDaemon()
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		r := newRecorder(d, dir)
		rec, err := r.Plan(ctx, "widget", "2026.300.2", SourceScript)
		if err != nil {
			t.Fatal(err)
		}
		res, err := r.Write(ctx, rec)
		if err != nil {
			t.Fatal(err)
		}
		if (i == 0) != res.Created {
			t.Errorf("run %d: created = %v", i, res.Created)
		}
	}
	if d.creates != 1 || d.updates != 0 {
		t.Errorf("creates = %d updates = %d, want 1 and 0", d.creates, d.updates)
	}
	rel := d.releases["2026.300.2"]
	var rows []Row
	if err := json.Unmarshal(rel.Contents, &rows); err != nil || len(rows) != 4 {
		t.Fatalf("contents = %s (%v), want 4 rows", rel.Contents, err)
	}
	if rel.Source != SourceScript || rel.PreviousTag != "2026.300.1" || rel.CutBy != "Ada" {
		t.Errorf("release = %+v", rel)
	}
	if n := len(d.comments[planA]); n != 1 {
		t.Fatalf("plan A comments = %d, want 1", n)
	}
	if c := d.comments[planA][0].Text; !strings.Contains(c, "Shipped in widget 2026.300.2") || !strings.Contains(c, "#10") || !strings.Contains(c, "#12") {
		t.Errorf("plan A comment = %q", c)
	}
	if len(d.comments[planB]) != 0 {
		t.Errorf("plan B got a comment for a release that carries none of its steps: %v", d.comments[planB])
	}
}

// A release written by hand before the cut wrote it (no contents, no
// previous) gets those filled, and nothing it already has is touched.
func TestWrite_FillsAHandWrittenRelease(t *testing.T) {
	dir, _ := fixtureRepo(t)
	d := newFakeDaemon()
	d.releases["2026.300.2"] = &beadsapi.Release{ID: "gd-byhand", Tag: "2026.300.2", SHA: "keep-me", Source: "manual"}
	r := newRecorder(d, dir)
	rec, err := r.Plan(context.Background(), "widget", "2026.300.2", SourceScript)
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Write(context.Background(), rec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Created || strings.Join(res.Filled, ",") != "contents,previous_tag" {
		t.Errorf("result = created %v filled %v, want filled contents,previous_tag", res.Created, res.Filled)
	}
	if rel := d.releases["2026.300.2"]; rel.SHA != "keep-me" || rel.PreviousTag != "2026.300.1" || len(rel.Contents) == 0 {
		t.Errorf("release = %+v", rel)
	}
}

// The backfill writes each release tag once, oldest first, and a second run
// writes nothing.
func TestBackfill_TwiceWritesOnce(t *testing.T) {
	dir, _ := fixtureRepo(t)
	d := newFakeDaemon()
	ctx := context.Background()
	for run := 0; run < 2; run++ {
		r := &Recorder{Daemon: d, Source: GitSource{Dir: dir}}
		tags, err := r.BackfillTags(ctx, "widget", "", 0)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, tg := range tags {
			names = append(names, tg.Name)
		}
		if strings.Join(names, " ") != "2026.300.1 2026.300.2 2026.300.10" {
			t.Fatalf("backfill tags = %v", names)
		}
		if _, err := r.Backfill(ctx, "widget", tags, true); err != nil {
			t.Fatal(err)
		}
	}
	if d.creates != 3 || d.updates != 0 {
		t.Errorf("creates = %d updates = %d, want 3 and 0", d.creates, d.updates)
	}
	if rel := d.releases["2026.300.10"]; rel.Previous != d.releases["2026.300.2"].ID || rel.Source != SourceBackfill {
		t.Errorf("2026.300.10 = %+v, want previous linked and source backfill", rel)
	}
	if len(d.comments) != 0 {
		t.Errorf("the backfill commented on plans: %v", d.comments)
	}

	r := &Recorder{Daemon: d, Source: GitSource{Dir: dir}}
	tags, err := r.BackfillTags(ctx, "widget", "2026.300.2", 0)
	if err != nil || len(tags) != 2 {
		t.Errorf("--since 2026.300.2 = %v (%v), want 2 tags", tags, err)
	}
}

// A dry run plans and writes nothing.
func TestBackfill_DryRunWritesNothing(t *testing.T) {
	dir, _ := fixtureRepo(t)
	d := newFakeDaemon()
	r := &Recorder{Daemon: d, Source: GitSource{Dir: dir}}
	tags, _ := r.BackfillTags(context.Background(), "widget", "", 2)
	items, err := r.Backfill(context.Background(), "widget", tags, false)
	if err != nil || len(items) != 2 {
		t.Fatalf("items = %d (%v)", len(items), err)
	}
	if d.creates+d.updates != 0 {
		t.Errorf("a dry run wrote: creates %d updates %d", d.creates, d.updates)
	}
}

func TestMarkBuilt_OnceOnly(t *testing.T) {
	d := newFakeDaemon()
	d.releases["2026.300.2"] = &beadsapi.Release{ID: "gd-r", Tag: "2026.300.2"}
	ctx := context.Background()
	_, changed, err := MarkBuilt(ctx, d, "widget", "2026.300.2", "2026-10-27T10:00:00-07:00", []Image{{Name: "kbeads", Ref: "ecr/kbeads:2026.300.2"}})
	if err != nil || !changed {
		t.Fatalf("first MarkBuilt = %v %v", changed, err)
	}
	if rel := d.releases["2026.300.2"]; rel.BuiltAt != "2026-10-27T17:00:00Z" || !strings.Contains(string(rel.Images), "kbeads") {
		t.Errorf("release = %+v", rel)
	}
	if _, changed, _ := MarkBuilt(ctx, d, "widget", "2026.300.2", "", nil); changed {
		t.Error("a second MarkBuilt rewrote built_at")
	}
}

func TestParseCommit(t *testing.T) {
	for _, tc := range []struct {
		c    Commit
		want parsedCommit
	}{
		{Commit{Subject: "Merge pull request #1310 from mattbakerconsulting/agent/2-backend/gd-Dvour96HjO", Body: "feat(kbeads): release bead"},
			parsedCommit{PR: 1310, Title: "feat(kbeads): release bead", BeadID: "gd-Dvour96HjO"}},
		{Commit{Subject: "Merge pull request #1123 from mattbakerconsulting/agent/glassask-code-font", Body: "Glass ask font"},
			parsedCommit{PR: 1123, Title: "Glass ask font"}},
		{Commit{Subject: "fix(gardenio/bridge): handle nil (#88)"},
			parsedCommit{PR: 88, Title: "fix(gardenio/bridge): handle nil"}},
		{Commit{Subject: "docs: re-evaluating the plan", Body: "for gd-hls6BkIEiO"},
			parsedCommit{Title: "docs: re-evaluating the plan", BeadID: "gd-hls6BkIEiO"}},
	} {
		if got := parseCommit(tc.c); got != tc.want {
			t.Errorf("parseCommit(%q) = %+v, want %+v", tc.c.Subject, got, tc.want)
		}
	}
}

func TestVersionLessAndRepoURLs(t *testing.T) {
	re := regexp.MustCompile(`^v\d+\.\d+\.\d+$`)
	tags := releaseTags([]Tag{{Name: "v1.10.0"}, {Name: "v1.9.2"}, {Name: "latest"}, {Name: "v1.2.0"}}, re)
	if len(tags) != 3 || tags[0].Name != "v1.2.0" || tags[2].Name != "v1.10.0" {
		t.Errorf("semver order = %v", tags)
	}
	if p := previousTag(tags, "v1.10.0"); p != "v1.9.2" {
		t.Errorf("previous of v1.10.0 = %q", p)
	}
	if p := previousTag(tags, "v1.2.0"); p != "" {
		t.Errorf("previous of the first = %q", p)
	}
	for in, want := range map[string]string{
		"git@github.com:acme/widget.git":       "https://github.com/acme/widget",
		"https://github.com/acme/widget/":      "https://github.com/acme/widget",
		"ssh://git@github.com/acme/widget.git": "https://github.com/acme/widget",
	} {
		if got := RepoWebURL(in); got != want {
			t.Errorf("RepoWebURL(%q) = %q, want %q", in, got, want)
		}
	}
	if got := RepoFullName("https://github.com/acme/widget"); got != "acme/widget" {
		t.Errorf("RepoFullName = %q", got)
	}
}

// memSource is a Source over a fixed tag list and one commit range.
type memSource struct {
	tags    []Tag
	commits []Commit
}

func (m memSource) Tags(context.Context) ([]Tag, error) { return m.tags, nil }

func (m memSource) Tag(_ context.Context, name string) (Tag, error) {
	for _, t := range m.tags {
		if t.Name == name {
			return t, nil
		}
	}
	return Tag{}, notFound()
}

func (m memSource) FirstParent(context.Context, string, string) ([]Commit, bool, error) {
	return m.commits, true, nil
}

// A merge train's merge commit becomes one row per PR it carries, each
// joined on its own number and never on the train's merge sha.
func TestPlan_MergeTrainRowPerCarriedPR(t *testing.T) {
	d := newFakeDaemon()
	train := d.addMR("gd-MRtrain015", repoURL+"/pull/15", "merge train", stepB1)
	train.Fields["merge_sha"] = "trainsha"
	src := memSource{
		tags: []Tag{{Name: "2026.300.1", SHA: "a"}, {Name: "2026.300.2", SHA: "trainsha"}},
		commits: []Commit{{SHA: "trainsha", Subject: "Merge pull request #15 from acme/desk/train-20261030T0742Z",
			Body: "merge train 2026-10-30T07:42Z: #10 #14"}},
	}
	r := &Recorder{Daemon: d, Source: src}
	rec, err := r.Plan(context.Background(), "widget", "2026.300.2", SourceScript)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Rows) != 2 {
		t.Fatalf("rows = %+v, want #10 and #14", rec.Rows)
	}
	if got := rec.Rows[0]; got.PR != 10 || got.Train != 15 || got.Plan != planA || got.Title != "Add the widget frame" || got.MergeSHA != "trainsha" {
		t.Errorf("row #10 = %+v", got)
	}
	if got := rec.Rows[1]; got.PR != 14 || got.Train != 15 || got.MRBead != "" || got.Plan != "" || got.Title != "#14 (merge train #15)" {
		t.Errorf("row #14 = %+v, want unplanned and not joined to the train's MR bead", got)
	}
}

// fakePRs is a PRLookup over a fixed set of PRs.
type fakePRs map[int]PR

func (f fakePRs) PullForCommit(context.Context, string) (PR, bool, error) { return PR{}, false, nil }

func (f fakePRs) Pull(_ context.Context, n int) (PR, bool, error) {
	pr, ok := f[n]
	return pr, ok, nil
}

// A PR with no MR bead is read from the forge: its title, and the bead its
// agent branch names.
func TestPlan_PRWithoutMRBeadJoinsThroughItsBranch(t *testing.T) {
	d := newFakeDaemon()
	src := memSource{
		tags: []Tag{{Name: "2026.300.1", SHA: "a"}, {Name: "2026.300.2", SHA: "trainsha"}},
		commits: []Commit{{SHA: "trainsha", Subject: "Merge pull request #15 from acme/desk/train-20261030T0742Z",
			Body: "merge train 2026-10-30T07:42Z: #14"}},
	}
	r := &Recorder{Daemon: d, Source: src, PRs: fakePRs{14: {Number: 14, Title: "Polish the hinge", HeadRef: "agent/polish/gd-TaskA2x003"}}}
	rec, err := r.Plan(context.Background(), "widget", "2026.300.2", SourceScript)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Rows[0]; len(rec.Rows) != 1 || got.Title != "Polish the hinge" || got.Task != taskA2 || got.Step != stepA1 || got.Plan != planA {
		t.Errorf("rows = %+v, want #14 joined to task A2 in plan A", rec.Rows)
	}
}

// The poll records only tags above the newest recorded release, in version
// order; with none recorded, only the newest tag.
func TestNewTags(t *testing.T) {
	dir, _ := fixtureRepo(t)
	ctx := context.Background()
	for _, tc := range []struct {
		recorded []string
		want     string
	}{
		{nil, "2026.300.10"},
		{[]string{"v-not-a-release"}, "2026.300.10"},
		{[]string{"2026.300.1"}, "2026.300.2 2026.300.10"},
		{[]string{"2026.300.2", "2026.300.1"}, "2026.300.10"},
		{[]string{"2026.300.10"}, ""},
	} {
		r := &Recorder{Daemon: newFakeDaemon(), Source: GitSource{Dir: dir}}
		tags, err := r.NewTags(ctx, "widget", tc.recorded)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, tg := range tags {
			names = append(names, tg.Name)
		}
		if got := strings.Join(names, " "); got != tc.want {
			t.Errorf("NewTags(%v) = %q, want %q", tc.recorded, got, tc.want)
		}
	}
}
