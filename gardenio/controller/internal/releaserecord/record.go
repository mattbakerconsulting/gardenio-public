package releaserecord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Daemon is what the recorder asks of the beads daemon; *beadsapi.Client
// satisfies it.
type Daemon interface {
	GetProjectBead(ctx context.Context, name string) (beadsapi.ProjectInfo, error)
	GetRelease(ctx context.Context, ref, project string) (*beadsapi.Release, error)
	ListReleases(ctx context.Context, project, status string, limit int) ([]beadsapi.Release, error)
	CreateRelease(ctx context.Context, req beadsapi.CreateReleaseRequest) (*beadsapi.Release, error)
	UpdateBeadFieldsTyped(ctx context.Context, beadID string, fields map[string]any) error
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
	GetDependencies(ctx context.Context, beadID string) ([]beadsapi.Dependency, error)
	GetComments(ctx context.Context, beadID string) ([]beadsapi.Comment, error)
	AddComment(ctx context.Context, beadID, author, text string) error
}

var _ Daemon = (*beadsapi.Client)(nil)

// Release sources (releases-design.md §3.1).
const (
	SourceScript   = "script"
	SourceWebhook  = "webhook"
	SourceBackfill = "backfill"
)

// Row is one contents row: a PR (or a direct commit) the release carries,
// joined to its merge-request bead, the task that produced it, that task's
// plan step and the plan. A row with no task is unplanned.
type Row struct {
	PR int `json:"pr,omitempty"`
	// Train is the merge-train PR that carried PR onto the branch, 0 for a
	// PR merged on its own.
	Train    int    `json:"train,omitempty"`
	URL      string `json:"url,omitempty"`
	Title    string `json:"title"`
	MergeSHA string `json:"merge_sha"`
	MergedAt string `json:"merged_at,omitempty"`
	MRBead   string `json:"mr_bead,omitempty"`
	Task     string `json:"task,omitempty"`
	Step     string `json:"step,omitempty"`
	Plan     string `json:"plan,omitempty"`
}

// Record is the release a Recorder would write for one tag.
type Record struct {
	Project  string
	Tag      Tag
	Previous string // previous release tag, "" for the first
	Rows     []Row
	Complete bool
	Source   string
	// Existing is the release already recorded for (project, tag), nil when
	// none is.
	Existing *beadsapi.Release
	// Open is the project's open release the tag is cut onto (with
	// Recorder.CutOpen), nil when the tag is minted as its own release.
	Open *beadsapi.Release
	// Dropped is the rows Open carries that git does not show for the tag:
	// the cut drops them.
	Dropped []Row
}

// Request is the create body for the record.
func (r *Record) Request() (beadsapi.CreateReleaseRequest, error) {
	contents, err := json.Marshal(r.Rows)
	if err != nil {
		return beadsapi.CreateReleaseRequest{}, fmt.Errorf("encoding contents: %w", err)
	}
	complete := r.Complete
	return beadsapi.CreateReleaseRequest{
		Project:          r.Project,
		Tag:              r.Tag.Name,
		SHA:              r.Tag.SHA,
		PreviousTag:      r.Previous,
		CutAt:            normalizeTime(r.Tag.Date),
		CutBy:            r.Tag.Tagger,
		Source:           r.Source,
		Contents:         contents,
		ContentsComplete: &complete,
	}, nil
}

// PlanGroup is the rows of one plan, for the plan comment and the dry run.
type PlanGroup struct {
	Plan string // "" for the unplanned rows
	Rows []Row
}

// GroupByPlan groups rows by plan in order of first appearance, unplanned
// last.
func GroupByPlan(rows []Row) []PlanGroup {
	var groups []PlanGroup
	index := map[string]int{}
	var unplanned []Row
	for _, r := range rows {
		if r.Plan == "" {
			unplanned = append(unplanned, r)
			continue
		}
		i, ok := index[r.Plan]
		if !ok {
			i = len(groups)
			index[r.Plan] = i
			groups = append(groups, PlanGroup{Plan: r.Plan})
		}
		groups[i].Rows = append(groups[i].Rows, r)
	}
	if len(unplanned) > 0 {
		groups = append(groups, PlanGroup{Rows: unplanned})
	}
	return groups
}

// Recorder builds and writes release records for one project.
type Recorder struct {
	Daemon Daemon
	Source Source
	// PRs finds the PR of a commit whose subject names none; nil skips the
	// lookup (the row stays a direct commit).
	PRs PRLookup
	// CommentPlans comments "shipped in <tag>" on each plan the release
	// carries steps of. The cut does; the backfill of old tags does not.
	CommentPlans bool
	// Author is the comment author on the plans ("gb release record").
	Author string
	// CutOpen cuts a new tag onto the project's open release instead of
	// minting a bead, and opens the next release after the write. The cut
	// does; the backfill of old tags does not.
	CutOpen bool

	project  beadsapi.ProjectInfo
	name     string
	pattern  *regexp.Regexp
	webURL   string
	beads    map[string]*beadsapi.BeadDetail
	parents  map[string][]string
	planOf   map[string][2]string // task -> {step, plan}
	tagCache []Tag
}

// ErrNoReleaseSource is returned for a project without a release_tag_pattern:
// it has no releases.
var ErrNoReleaseSource = errors.New("project has no release_tag_pattern")

// Load reads the project's release source; Plan and Write call it once.
func (r *Recorder) Load(ctx context.Context, project string) error {
	if r.name == project && r.pattern != nil {
		return nil
	}
	info, err := r.Daemon.GetProjectBead(ctx, project)
	if err != nil {
		return fmt.Errorf("reading project %s: %w", project, err)
	}
	if info.ReleaseSource == nil || info.ReleaseSource.TagPattern == "" {
		return fmt.Errorf("%s: %w", project, ErrNoReleaseSource)
	}
	re, err := regexp.Compile(info.ReleaseSource.TagPattern)
	if err != nil {
		return fmt.Errorf("%s: release_tag_pattern %q: %w", project, info.ReleaseSource.TagPattern, err)
	}
	r.project, r.name, r.pattern = info, project, re
	r.webURL = RepoWebURL(info.GitURL)
	r.beads = map[string]*beadsapi.BeadDetail{}
	r.parents = map[string][]string{}
	r.planOf = map[string][2]string{}
	r.tagCache = nil
	return nil
}

// Matches reports whether tag is a release tag of the loaded project.
func (r *Recorder) Matches(tag string) bool {
	return r.pattern != nil && r.pattern.MatchString(tag)
}

// ReleaseTags lists the loaded project's release tags, oldest first.
func (r *Recorder) ReleaseTags(ctx context.Context) ([]Tag, error) {
	if r.tagCache == nil {
		tags, err := r.Source.Tags(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing tags: %w", err)
		}
		r.tagCache = releaseTags(tags, r.pattern)
	}
	return r.tagCache, nil
}

// Plan builds the record for tag without writing anything. With CutOpen it
// also finds the open release the tag is cut onto, and the rows that cut
// drops.
func (r *Recorder) Plan(ctx context.Context, project, tagName, source string) (*Record, error) {
	if err := r.Load(ctx, project); err != nil {
		return nil, err
	}
	if !r.Matches(tagName) {
		return nil, fmt.Errorf("tag %s does not match %s's release_tag_pattern %q", tagName, project, r.pattern)
	}
	tag, err := r.Source.Tag(ctx, tagName)
	if err != nil {
		return nil, fmt.Errorf("reading tag %s: %w", tagName, err)
	}
	tags, err := r.ReleaseTags(ctx)
	if err != nil {
		return nil, err
	}
	rec := &Record{Project: project, Tag: tag, Previous: previousTag(tags, tagName), Source: source}

	existing, err := r.Daemon.GetRelease(ctx, tagName, project)
	switch {
	case err == nil:
		rec.Existing = existing
	case !isNotFound(err):
		return nil, fmt.Errorf("reading release %s %s: %w", project, tagName, err)
	}

	base := ""
	if rec.Previous != "" {
		for _, t := range tags {
			if t.Name == rec.Previous {
				base = t.SHA
			}
		}
		if base == "" {
			base = rec.Previous
		}
	}
	head := tag.SHA
	if head == "" {
		head = tagName
	}
	commits, complete, err := r.Source.FirstParent(ctx, base, head)
	if err != nil {
		return nil, fmt.Errorf("listing commits %s..%s: %w", rec.Previous, tagName, err)
	}
	rec.Complete = complete
	for _, c := range commits {
		rows, err := r.commitRows(ctx, c)
		if err != nil {
			return nil, err
		}
		rec.Rows = append(rec.Rows, rows...)
	}
	if r.CutOpen && rec.Existing == nil {
		if err := r.planOpen(ctx, rec); err != nil {
			return nil, err
		}
	}
	return rec, nil
}

// commitRows is the joined contents rows of one first-parent commit: the one
// join both the cut (Plan) and the merge stamp (Board) run, so a PR boarded
// on the open release at its merge is the row the cut's reconcile writes.
func (r *Recorder) commitRows(ctx context.Context, c Commit) ([]Row, error) {
	p := parseCommit(c)
	carried := trainPRs(p)
	if len(carried) == 0 {
		row, err := r.row(ctx, c, p)
		if err != nil {
			return nil, err
		}
		return []Row{row}, nil
	}
	// A merge train is one merge of several PRs: a row per PR it carries,
	// each joined on its own number.
	rows := make([]Row, 0, len(carried))
	for _, n := range carried {
		row, err := r.row(ctx, c, parsedCommit{PR: n, Train: p.PR})
		if err != nil {
			return nil, err
		}
		if row.Title == "" {
			row.Title = fmt.Sprintf("#%d (merge train #%d)", n, p.PR)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// row turns one first-parent commit into a contents row and joins it: the PR
// number to the MR bead by mr_url, else the commit sha to the MR bead's
// merge_sha; the MR bead to its task (triggered-by), else the bead id in the
// branch name or message; the task to its step and plan.
func (r *Recorder) row(ctx context.Context, c Commit, p parsedCommit) (Row, error) {
	row := Row{PR: p.PR, Train: p.Train, Title: p.Title, MergeSHA: c.SHA, MergedAt: normalizeTime(c.Date)}
	if row.PR == 0 && p.BeadID == "" && r.PRs != nil {
		if pr, ok, err := r.PRs.PullForCommit(ctx, c.SHA); err == nil && ok {
			row.PR, row.URL, row.Title = pr.Number, pr.URL, pr.Title
			if pr.MergedAt != "" {
				row.MergedAt = normalizeTime(pr.MergedAt)
			}
		}
	}
	if row.PR != 0 && row.URL == "" && r.webURL != "" {
		row.URL = r.webURL + "/pull/" + strconv.Itoa(row.PR)
	}

	var mr *beadsapi.BeadDetail
	var err error
	if row.URL != "" {
		if mr, err = r.findMR(ctx, "mr_url", row.URL); err != nil {
			return row, err
		}
	}
	// A train's merge sha is the train PR's, never a carried PR's.
	if mr == nil && p.Train == 0 {
		if mr, err = r.findMR(ctx, "merge_sha", c.SHA); err != nil {
			return row, err
		}
	}
	if mr == nil && row.PR != 0 && r.PRs != nil && (p.BeadID == "" || row.Title == "") {
		// No MR bead: the PR itself names its title and, on an agent
		// branch, its bead. A failed read leaves the row as it is.
		if pr, ok, err := r.PRs.Pull(ctx, row.PR); err == nil && ok {
			if row.Title == "" || p.Train != 0 {
				row.Title = pr.Title
			}
			if p.BeadID == "" {
				p.BeadID = branchBeadID(pr.HeadRef)
			}
		}
	}
	task := ""
	if mr != nil {
		row.MRBead = mr.ID
		if t := strings.TrimSpace(mr.Fields["mr_title"]); t != "" {
			row.Title = t
		}
		if at := strings.TrimSpace(mr.Fields["merged_at"]); at != "" {
			row.MergedAt = normalizeTime(at)
		}
		if row.URL == "" {
			row.URL = mr.Fields["mr_url"]
		}
		if task, err = r.taskOfMR(ctx, mr.ID); err != nil {
			return row, err
		}
	}
	if task == "" && p.BeadID != "" {
		b, err := r.bead(ctx, p.BeadID)
		if err != nil {
			return row, err
		}
		if b != nil && workBeadTypes[b.Type] {
			task = b.ID
		}
	}
	if task == "" {
		return row, nil
	}
	row.Task = task
	sp, err := r.stepAndPlan(ctx, task)
	if err != nil {
		return row, err
	}
	row.Step, row.Plan = sp[0], sp[1]
	return row, nil
}

// workBeadTypes are the bead types that can produce a PR.
var workBeadTypes = map[string]bool{"task": true, "bug": true, "feature": true, "epic": true, "chore": true}

// allStatuses lists closed beads too: a merged PR's MR bead and task are
// closed.
var allStatuses = []string{"open", "in_progress", "blocked", "deferred", "closed"}

func (r *Recorder) findMR(ctx context.Context, field, value string) (*beadsapi.BeadDetail, error) {
	res, err := r.Daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"merge-request"},
		Statuses: allStatuses,
		Fields:   map[string]string{field: value},
		Limit:    2,
	})
	if err != nil {
		return nil, fmt.Errorf("finding the MR bead with %s %s: %w", field, value, err)
	}
	if res == nil {
		return nil, nil
	}
	for _, b := range res.Beads {
		if b.Fields[field] == value {
			return b, nil
		}
	}
	return nil, nil
}

// taskOfMR is the work bead the MR bead was triggered by.
func (r *Recorder) taskOfMR(ctx context.Context, mrID string) (string, error) {
	deps, err := r.Daemon.GetDependencies(ctx, mrID)
	if err != nil {
		return "", fmt.Errorf("reading the edges of MR bead %s: %w", mrID, err)
	}
	for _, d := range deps {
		if d.Type != "triggered-by" {
			continue
		}
		b, err := r.bead(ctx, d.DependsOnID)
		if err != nil {
			return "", err
		}
		if b != nil && workBeadTypes[b.Type] {
			return b.ID, nil
		}
	}
	return "", nil
}

// maxPlanDepth bounds the walk from a task up to its plan.
const maxPlanDepth = 6

// stepAndPlan walks up parent-child edges from task, breadth first, to the
// nearest epic labelled plan: the plan, and the bead directly under it on
// the way (the task itself when the task is a step) is the step. Both are ""
// when no plan is within reach.
func (r *Recorder) stepAndPlan(ctx context.Context, task string) ([2]string, error) {
	if sp, ok := r.planOf[task]; ok {
		return sp, nil
	}
	type node struct{ id string }
	frontier := []node{{id: task}}
	seen := map[string]bool{task: true}
	var found [2]string
	for depth := 0; depth < maxPlanDepth && len(frontier) > 0 && found[1] == ""; depth++ {
		var next []node
		for _, n := range frontier {
			parents, err := r.parentsOf(ctx, n.id)
			if err != nil {
				return found, err
			}
			for _, pid := range parents {
				if seen[pid] {
					continue
				}
				seen[pid] = true
				pb, err := r.bead(ctx, pid)
				if err != nil {
					return found, err
				}
				if pb == nil {
					continue
				}
				if pb.Type == "epic" && slices.Contains(pb.Labels, "plan") {
					found = [2]string{n.id, pid}
					break
				}
				next = append(next, node{id: pid})
			}
			if found[1] != "" {
				break
			}
		}
		frontier = next
	}
	r.planOf[task] = found
	return found, nil
}

func (r *Recorder) parentsOf(ctx context.Context, id string) ([]string, error) {
	if p, ok := r.parents[id]; ok {
		return p, nil
	}
	deps, err := r.Daemon.GetDependencies(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("reading the parents of %s: %w", id, err)
	}
	var out []string
	for _, d := range deps {
		if d.Type == "parent-child" && d.BeadID == id {
			out = append(out, d.DependsOnID)
		}
	}
	r.parents[id] = out
	return out, nil
}

// bead reads a bead once per run; nil for one the daemon does not know.
func (r *Recorder) bead(ctx context.Context, id string) (*beadsapi.BeadDetail, error) {
	if b, ok := r.beads[id]; ok {
		return b, nil
	}
	b, err := r.Daemon.GetBead(ctx, id)
	if err != nil && !isNotFound(err) {
		return nil, fmt.Errorf("reading bead %s: %w", id, err)
	}
	r.beads[id] = b
	return b, nil
}

func isNotFound(err error) bool {
	var apiErr *beadsapi.APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

// Result is what Write did.
type Result struct {
	Release *beadsapi.Release
	// Created is true when this call created the release; false when it was
	// already recorded.
	Created bool
	// CutOpen is true when this call cut the tag onto the open release.
	CutOpen bool
	// Next is the project's open release after the write (with
	// Recorder.CutOpen); NextOpened is true when this call opened it.
	Next       *beadsapi.Release
	NextOpened bool
	// Filled names the fields written onto an existing release that lacked
	// them (contents, previous_tag, sha).
	Filled []string
	// Commented lists the plans that got a "shipped in" comment.
	Commented []string
}

// Write records rec: it cuts the tag onto the open release rec.Open, else
// creates the release, or fills the fields an existing release of the same
// tag lacks (a release written by hand has no contents), then, with
// CommentPlans, comments on each plan whose steps shipped, and with CutOpen
// opens the project's next release. It never overwrites a field that is
// already set, the plan comment carries ShippedMarker and a project has one
// open release, so running it twice writes once.
func (r *Recorder) Write(ctx context.Context, rec *Record) (*Result, error) {
	res := &Result{Release: rec.Existing}
	switch {
	case rec.Existing != nil:
	case rec.Open != nil:
		rel, err := r.cutOpen(ctx, rec)
		if err != nil {
			return nil, err
		}
		res.Release, res.CutOpen = rel, true
	default:
		req, err := rec.Request()
		if err != nil {
			return nil, err
		}
		rel, err := r.Daemon.CreateRelease(ctx, req)
		if err != nil {
			return nil, err
		}
		res.Release, res.Created = rel, true
	}
	// Also after a create: a concurrent writer may have won the (project,
	// tag) race with a release that has no contents, and the daemon returned
	// its release instead of ours.
	if err := r.fill(ctx, rec, res.Release, res); err != nil {
		return res, err
	}
	if r.CommentPlans {
		commented, err := r.commentPlans(ctx, rec, res.Release)
		res.Commented = commented
		if err != nil {
			return res, err
		}
	}
	if r.CutOpen {
		next, opened, err := OpenNext(ctx, r.Daemon, rec.Project)
		res.Next, res.NextOpened = next, opened
		if err != nil {
			return res, err
		}
	}
	return res, nil
}

// fill writes the fields rel lacks from rec: contents (with
// contents_complete), previous_tag and sha.
func (r *Recorder) fill(ctx context.Context, rec *Record, rel *beadsapi.Release, res *Result) error {
	fields := map[string]any{}
	if len(rel.Contents) == 0 || string(rel.Contents) == "null" || string(rel.Contents) == "[]" {
		if len(rec.Rows) > 0 {
			fields["contents"] = rec.Rows
			fields["contents_complete"] = rec.Complete
		}
	}
	if rel.PreviousTag == "" && rel.Previous == "" && rec.Previous != "" {
		fields["previous_tag"] = rec.Previous
	}
	if rel.SHA == "" && rec.Tag.SHA != "" {
		fields["sha"] = rec.Tag.SHA
	}
	if len(fields) == 0 {
		return nil
	}
	if err := r.Daemon.UpdateBeadFieldsTyped(ctx, rel.ID, fields); err != nil {
		return fmt.Errorf("filling release %s: %w", rel.ID, err)
	}
	for _, k := range []string{"contents", "previous_tag", "sha"} {
		if _, ok := fields[k]; ok {
			res.Filled = append(res.Filled, k)
		}
	}
	return nil
}

// ShippedMarker is the text that makes a plan's "shipped in" comment
// idempotent: a plan that already has it for the tag gets no second one.
func ShippedMarker(project, tag string) string {
	return fmt.Sprintf("Shipped in %s %s", project, tag)
}

// commentPlans comments on each plan whose steps the release carries.
func (r *Recorder) commentPlans(ctx context.Context, rec *Record, rel *beadsapi.Release) ([]string, error) {
	var done []string
	marker := ShippedMarker(rec.Project, rec.Tag.Name)
	for _, g := range GroupByPlan(rec.Rows) {
		if g.Plan == "" {
			continue
		}
		comments, err := r.Daemon.GetComments(ctx, g.Plan)
		if err != nil {
			return done, fmt.Errorf("reading comments on plan %s: %w", g.Plan, err)
		}
		if slices.ContainsFunc(comments, func(c beadsapi.Comment) bool { return strings.Contains(c.Text, marker) }) {
			continue
		}
		if err := r.Daemon.AddComment(ctx, g.Plan, r.Author, planComment(marker, rel.ID, g.Rows)); err != nil {
			return done, fmt.Errorf("commenting on plan %s: %w", g.Plan, err)
		}
		done = append(done, g.Plan)
	}
	return done, nil
}

// planComment is the "shipped in" comment: the release, then one line per
// step with its PRs.
func planComment(marker, releaseID string, rows []Row) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s (release %s): %d PR%s from this plan.", marker, releaseID, len(rows), plural(len(rows)))
	var steps []string
	byStep := map[string][]string{}
	for _, row := range rows {
		if _, ok := byStep[row.Step]; !ok {
			steps = append(steps, row.Step)
		}
		ref := row.MergeSHA
		if len(ref) > 8 {
			ref = ref[:8]
		}
		if row.PR != 0 {
			ref = "#" + strconv.Itoa(row.PR)
		}
		byStep[row.Step] = append(byStep[row.Step], ref)
	}
	for _, s := range steps {
		fmt.Fprintf(&b, "\n- %s: %s", s, strings.Join(byStep[s], ", "))
	}
	return b.String()
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// normalizeTime renders an RFC 3339 time (with any offset) in UTC, which is
// how the daemon sorts cut_at; a value it cannot parse passes through.
func normalizeTime(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.UTC().Format(time.RFC3339)
}
