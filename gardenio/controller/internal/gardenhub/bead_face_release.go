package gardenhub

// The release face (gd-j3bQqfZSt7): a type=release bead (gardenio/docs/releases-design.md §3.1) read in the bead view as
// its tag, status and where it runs, its cut and build times, the release before it, and its contents grouped by plan,
// instead of a fields table that dumps the contents and deployments JSON (a 30 KB row at beadFieldMax, cut). The plans
// are refs the panel titles in the hydrate pass; the rows carry the PR's number, link, title and merge time only, so a
// release of a hundred PRs costs one read per plan, never one per PR. web/relface.js paints it.

import (
	"encoding/json"
	"sort"
	"strings"
)

const (
	faceReleaseRows  = 150 // contents rows sent; past it, More says how many were left
	faceReleasePlans = 16  // plans titled (one read each); the rest are grouped under their id untitled
)

type faceRelease struct {
	Tag         string            `json:"tag"`
	Project     string            `json:"project,omitempty"`
	Status      string            `json:"status,omitempty"` // cut, built, live, superseded (the daemon's)
	SHA         string            `json:"sha,omitempty"`
	CutAt       int64             `json:"cut_at,omitempty"`
	CutBy       string            `json:"cut_by,omitempty"`
	Source      string            `json:"source,omitempty"` // webhook, script, backfill, manual
	BuiltAt     int64             `json:"built_at,omitempty"`
	Previous    *beadRel          `json:"previous,omitempty"`
	PreviousTag string            `json:"previous_tag,omitempty"`
	Count       int               `json:"count"`                // every contents row, numbered or not
	Incomplete  bool              `json:"incomplete,omitempty"` // contents_complete false: the compare was cut short
	Deployments []faceDeployment  `json:"deployments,omitempty"`
	Plans       []faceReleasePlan `json:"plans,omitempty"`
	More        int               `json:"more,omitempty"`
}

// faceDeployment is one deployments row: Until is 0 while the release runs on Target.
type faceDeployment struct {
	Target   string `json:"target"`
	Kind     string `json:"kind,omitempty"`
	Since    int64  `json:"since,omitempty"`
	Until    int64  `json:"until,omitempty"`
	Via      string `json:"via,omitempty"`
	Revision string `json:"revision,omitempty"`
}

// faceReleasePlan is the contents of one plan, Plan nil for the rows outside any plan (always last).
type faceReleasePlan struct {
	Plan *beadRel         `json:"plan,omitempty"`
	ID   string           `json:"id,omitempty"` // the plan's id when it is past faceReleasePlans and so untitled
	Rows []faceReleaseRow `json:"rows"`
}

type faceReleaseRow struct {
	PR       int    `json:"pr,omitempty"`
	URL      string `json:"url,omitempty"`
	Title    string `json:"title,omitempty"`
	MergedAt int64  `json:"merged_at,omitempty"`
	Step     string `json:"step,omitempty"` // the step's id, else the task's, plain
}

// releaseContentRow is a contents row as the cut writes it (releases-design.md §3.1).
type releaseContentRow struct {
	PR       int    `json:"pr"`
	URL      string `json:"url"`
	Title    string `json:"title"`
	MergedAt string `json:"merged_at"`
	Plan     string `json:"plan"`
	Step     string `json:"step"`
	Task     string `json:"task"`
}

// releaseDeploymentRow is a deployments row as the deploy writes it.
type releaseDeploymentRow struct {
	Target   string `json:"target"`
	Kind     string `json:"kind"`
	Since    string `json:"since"`
	Until    string `json:"until"`
	Via      string `json:"via"`
	Revision string `json:"revision"`
}

// releaseFaceFields are the fields the face carries whole, so the fields table skips them (bead.go).
var releaseFaceFields = map[string]bool{"contents": true, "deployments": true, "images": true}

// releaseFace reads the face from the bead's fields; ref registers a bead the face names for the hydrate pass.
func releaseFace(fl map[string]string, ref func(string) *beadRel) *faceRelease {
	f := &faceRelease{Tag: strings.TrimSpace(fl["tag"]), Project: strings.TrimSpace(fl["project"]), Status: strings.TrimSpace(fl["status"]),
		SHA: strings.TrimSpace(fl["sha"]), CutAt: fieldUnix(fl["cut_at"]), CutBy: strings.TrimSpace(fl["cut_by"]), Source: strings.TrimSpace(fl["source"]),
		BuiltAt: fieldUnix(fl["built_at"]), Previous: ref(fl["previous"]), PreviousTag: strings.TrimSpace(fl["previous_tag"]),
		Incomplete: strings.TrimSpace(fl["contents_complete"]) == "false"}
	f.Deployments = releaseFaceDeployments(fl["deployments"])
	f.Plans, f.Count, f.More = releaseFacePlans(fl["contents"], ref)
	return f
}

// releaseFaceDeployments reads the deployments rows: the ones still running first, then by start, newest first.
func releaseFaceDeployments(raw string) []faceDeployment {
	var rows []releaseDeploymentRow
	if strings.TrimSpace(raw) == "" || json.Unmarshal([]byte(raw), &rows) != nil {
		return nil
	}
	var out []faceDeployment
	for _, r := range rows {
		if strings.TrimSpace(r.Target) == "" {
			continue
		}
		out = append(out, faceDeployment{Target: r.Target, Kind: r.Kind, Since: fieldUnix(r.Since), Until: fieldUnix(r.Until), Via: r.Via, Revision: r.Revision})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if (out[i].Until == 0) != (out[j].Until == 0) {
			return out[i].Until == 0
		}
		return out[i].Since > out[j].Since
	})
	return out
}

// releaseFacePlans groups the contents rows by plan: the plans with the most PRs first, ties in the order the rows name
// them (newest merge first), the rows outside any plan last. It returns the groups, every row's count and how many rows
// past faceReleaseRows were left out.
func releaseFacePlans(raw string, ref func(string) *beadRel) ([]faceReleasePlan, int, int) {
	var rows []releaseContentRow
	if strings.TrimSpace(raw) == "" || json.Unmarshal([]byte(raw), &rows) != nil {
		return nil, 0, 0
	}
	count, more := len(rows), 0
	if len(rows) > faceReleaseRows {
		more = len(rows) - faceReleaseRows
		rows = rows[:faceReleaseRows]
	}
	by, order := map[string]*faceReleasePlan{}, []string{}
	for _, r := range rows {
		id := strings.TrimSpace(r.Plan)
		g := by[id]
		if g == nil {
			g = &faceReleasePlan{}
			by[id] = g
			order = append(order, id)
		}
		g.Rows = append(g.Rows, faceReleaseRow{PR: r.PR, URL: httpURL(r.URL), Title: clip(r.Title, 200), MergedAt: fieldUnix(r.MergedAt), Step: firstNonEmpty(strings.TrimSpace(r.Step), strings.TrimSpace(r.Task))})
	}
	sort.SliceStable(order, func(i, j int) bool {
		if (order[i] == "") != (order[j] == "") {
			return order[i] != ""
		}
		return len(by[order[i]].Rows) > len(by[order[j]].Rows)
	})
	out := make([]faceReleasePlan, 0, len(order))
	for i, id := range order {
		g := *by[id]
		if id != "" {
			if i < faceReleasePlans {
				g.Plan = ref(id)
			}
			if g.Plan == nil {
				g.ID = id
			}
		}
		out = append(out, g)
	}
	return out, count, more
}
