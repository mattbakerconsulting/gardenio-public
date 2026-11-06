package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// awaits_release: merged work that is done only when it runs (programme
// docs/superpowers/plans/2026-11-02-agent-access-programme.md step P-R3,
// gd-ZM7NEBPAqZ). A crew sets awaits_release=true on its task, or on an
// escalation, instead of filing a helm_deploy escalation for the next window.
// The merge leaves such a task open (awaitsReleaseHold, read by close-on-merge
// and close-on-execute), and a deployment record closes it once the deployed
// release carries its work (closeShippedWork).
//
// "Carries" is read from release contents rows (plan gd-UZKZ9H25UD step 3),
// never from git. A release's contents are the PRs merged on the release branch
// since its previous release, so the deployed release and every release on its
// previous chain hold every merge commit that is an ancestor of its tag. A task
// is carried when each merged merge-request bead it produces is in those rows
// (by mr_bead, merge_sha or url) and none of its PRs is still open; a task with
// no merged merge-request bead is carried when a row names it as task or step.
// An escalation is carried when the PR its operation_spec names (mr_url) is in
// the rows, else when its requesting_task is carried.
//
// A project's open release (plan gd-lgN4U7X5Tb) carries the PRs merged since
// its last cut, but it runs nowhere: it cannot be deployed, and no deployed
// release's previous chain reaches it. Work it carries is "carried, not yet
// deployed": noteBoardedWork says so once on the bead, and nothing closes it.

// awaitsReleaseField is the opt-in field, a boolean on every issue-kind type
// and on type:escalation.
const awaitsReleaseField = "awaits_release"

// awaitsRelease reports whether b opts into closing when its work ships.
func awaitsRelease(b *model.Bead) bool {
	if b == nil {
		return false
	}
	v, err := strconv.ParseBool(strings.TrimSpace(parseFieldsToStringMap(b.Fields)[awaitsReleaseField]))
	return err == nil && v
}

// awaitsReleaseHold is the comment for a task that a merge or an executed
// escalation would have closed but awaits_release keeps open, "" when the task
// does not await a release. by names what would have closed it.
func awaitsReleaseHold(b *model.Bead, by string) string {
	if !awaitsRelease(b) || b.Status == model.StatusClosed {
		return ""
	}
	return fmt.Sprintf("not auto-closed: %s, but this task has awaits_release=true. "+
		"It closes when a deployment of a release that carries its PRs is recorded "+
		"(P-R3, gd-ZM7NEBPAqZ).", by)
}

// releaseCarried is the work a release and its previous chain carry, keyed
// every way a contents row names a PR or its task.
type releaseCarried struct {
	mrBeads, shas, urls, tasks map[string]bool
}

// carriedBy collects the contents rows of rel and of every release on its
// previous chain among releases. A previous that is not in the list, or a
// cycle, ends the walk.
func carriedBy(rel *model.Bead, releases []*model.Bead) releaseCarried {
	c := releaseCarried{mrBeads: map[string]bool{}, shas: map[string]bool{}, urls: map[string]bool{}, tasks: map[string]bool{}}
	byID := make(map[string]*model.Bead, len(releases)+1)
	for _, r := range releases {
		byID[r.ID] = r
	}
	byID[rel.ID] = rel
	seen := map[string]bool{}
	for cur := rel; cur != nil && !seen[cur.ID]; {
		seen[cur.ID] = true
		m := releaseFieldMap(cur.Fields)
		for _, row := range releaseContentsRows(m["contents"]) {
			add := func(set map[string]bool, key string) {
				if v := releaseString(row, key); v != "" {
					set[v] = true
				}
			}
			add(c.mrBeads, "mr_bead")
			add(c.shas, "merge_sha")
			add(c.tasks, "task")
			add(c.tasks, "step")
			if u := normalizePRURL(releaseString(row, "url")); u != "" {
				c.urls[u] = true
			}
		}
		cur = byID[releaseString(m, "previous")]
	}
	return c
}

// releaseContentsRows decodes a stored contents value, a JSON list or the JSON
// text of one. Anything else is no rows.
func releaseContentsRows(v any) []map[string]any {
	if v == nil {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	if s, ok := v.(string); ok {
		raw = []byte(s)
	}
	var rows []map[string]any
	if json.Unmarshal(raw, &rows) != nil {
		return nil
	}
	return rows
}

// normalizePRURL drops surrounding space and a trailing slash, so a PR url
// written by the bridge and by a crew compare equal.
func normalizePRURL(u string) string {
	return strings.TrimRight(strings.TrimSpace(u), "/")
}

// hasMR reports whether the merge-request bead mr is one of c's PRs.
func (c releaseCarried) hasMR(mr *model.Bead) bool {
	f := parseFieldsToStringMap(mr.Fields)
	return c.mrBeads[mr.ID] ||
		(strings.TrimSpace(f["merge_sha"]) != "" && c.shas[strings.TrimSpace(f["merge_sha"])]) ||
		(normalizePRURL(f["mr_url"]) != "" && c.urls[normalizePRURL(f["mr_url"])])
}

// taskCarried reports whether c carries task's work (see the file comment).
func (s *BeadsServer) taskCarried(ctx context.Context, task *model.Bead, c releaseCarried) bool {
	merged := 0
	for _, mr := range s.linkedMRBeads(ctx, task) {
		if !isMRMerged(parseFieldsToStringMap(mr.Fields)) {
			if mr.Status != model.StatusClosed {
				return false // a PR of the task is still open
			}
			continue // closed without merging: carries nothing
		}
		if !c.hasMR(mr) {
			return false
		}
		merged++
	}
	return merged > 0 || c.tasks[task.ID]
}

// escalationCarried reports whether c carries esc's work: the PR its
// operation_spec names, else its requesting task.
func (s *BeadsServer) escalationCarried(ctx context.Context, esc *model.Bead, c releaseCarried) bool {
	f := parseFieldsToStringMap(esc.Fields)
	var spec map[string]any
	if json.Unmarshal([]byte(f["operation_spec"]), &spec) == nil {
		if u := normalizePRURL(releaseString(spec, "mr_url")); u != "" {
			return c.urls[u]
		}
	}
	taskID := strings.TrimSpace(f["requesting_task"])
	if taskID == "" {
		return false
	}
	task, err := s.store.GetBead(ctx, taskID)
	if err != nil || task == nil {
		return false
	}
	return task.Type != model.TypeEscalation && s.taskCarried(ctx, task, c)
}

// awaitsReleaseTypes are the types closeShippedWork closes: the leaf types
// close-on-merge closes, and escalations.
var awaitsReleaseTypes = []model.BeadType{model.TypeTask, model.TypeBug, model.TypeFeature, model.TypeChore, model.TypeEscalation}

// awaitsReleaseStatuses are the statuses closeShippedWork closes from. Unlike
// openSubtreeChildStatuses it includes deferred: awaits_release=true says the
// release closes the bead, and parking it does not revoke that (gd-YApb4Jowu2).
var awaitsReleaseStatuses = []model.Status{
	model.StatusOpen, model.StatusInProgress, model.StatusBlocked, model.StatusDeferred,
}

// closeShippedWork closes every open task and escalation of rel's project with
// awaits_release=true whose work rel carries, with a comment naming the
// release and target; tasks first, so an escalation's close cascade finds its
// requesting task already done. It is best effort: the deployment is recorded
// already, so a failure logs and the bead waits for the next deployment.
func (s *BeadsServer) closeShippedWork(ctx context.Context, rel *model.Bead, target string) {
	m := releaseFieldMap(rel.Fields)
	project, tag := releaseString(m, "project"), releaseString(m, "tag")
	waiting, err := s.awaitingWork(ctx, project)
	if err != nil {
		slog.Warn("awaits_release: listing waiting work failed", "release", rel.ID, "err", err.Error(), "feature", "gd-ZM7NEBPAqZ")
		return
	}
	if len(waiting) == 0 {
		return
	}
	releases, err := s.projectReleases(ctx, project)
	if err != nil {
		slog.Warn("awaits_release: listing releases failed", "release", rel.ID, "err", err.Error(), "feature", "gd-ZM7NEBPAqZ")
		return
	}
	carried := carriedBy(rel, releases)
	slices.SortStableFunc(waiting, func(a, b *model.Bead) int {
		ea, eb := a.Type == model.TypeEscalation, b.Type == model.TypeEscalation
		switch {
		case ea == eb:
			return 0
		case eb:
			return -1
		}
		return 1
	})
	for _, b := range waiting {
		cur, err := s.store.GetBead(ctx, b.ID)
		if err != nil || cur == nil || cur.Status == model.StatusClosed || !awaitsRelease(cur) {
			continue
		}
		if s.workCarried(ctx, cur, carried) {
			s.closeShippedBead(ctx, cur, rel.ID, project, tag, target)
		}
	}
}

// awaitingWork lists project's open tasks and escalations with
// awaits_release=true.
func (s *BeadsServer) awaitingWork(ctx context.Context, project string) ([]*model.Bead, error) {
	waiting, _, err := s.store.ListBeads(ctx, model.BeadFilter{
		Type:   awaitsReleaseTypes,
		Status: awaitsReleaseStatuses,
		Labels: []string{"project:" + project},
		Fields: map[string]string{awaitsReleaseField: "true"},
		Limit:  maxListBeadsLimit,
	})
	return waiting, err
}

// workCarried reports whether c carries b's work, b a task or an escalation.
func (s *BeadsServer) workCarried(ctx context.Context, b *model.Bead, c releaseCarried) bool {
	if b.Type == model.TypeEscalation {
		return s.escalationCarried(ctx, b, c)
	}
	return s.taskCarried(ctx, b, c)
}

// boardedMarker makes noteBoardedWork's comment once per bead and release.
func boardedMarker(releaseID string) string {
	return "carried, not yet deployed: on the next release " + releaseID
}

// noteBoardedWork comments once on each open awaits_release task and
// escalation of the open release rel's project whose work rel carries: it is
// carried, not yet deployed, and stays open until a deployment of the release
// rel is cut into closes it (closeShippedWork). Best effort, after an append
// to rel's contents and outside releaseWriteMu.
func (s *BeadsServer) noteBoardedWork(ctx context.Context, rel *model.Bead) {
	project := releaseString(releaseFieldMap(rel.Fields), "project")
	waiting, err := s.awaitingWork(ctx, project)
	if err != nil {
		slog.Warn("awaits_release: listing waiting work failed", "release", rel.ID, "err", err.Error(), "feature", "gd-6LuAI5uRLC")
		return
	}
	carried, marker := carriedBy(rel, nil), boardedMarker(rel.ID)
	for _, b := range waiting {
		if !s.workCarried(ctx, b, carried) {
			continue
		}
		comments, err := s.store.GetComments(ctx, b.ID)
		if err != nil || slices.ContainsFunc(comments, func(c *model.Comment) bool { return strings.Contains(c.Text, marker) }) {
			continue
		}
		s.addAutoComment(ctx, b.ID, fmt.Sprintf("awaits_release: %s (%s, open). This %s stays open: "+
			"it closes when a deployment of the release this one is cut into is recorded (P-R3, gd-ZM7NEBPAqZ).", marker, project, b.Type))
	}
}

// closeShippedBead closes one shipped bead with the close bookkeeping
// autoCloseMergedLeaf uses, and re-enters afterBeadClosed for its cascade.
func (s *BeadsServer) closeShippedBead(ctx context.Context, b *model.Bead, releaseID, project, tag, target string) {
	prevStatus := b.Status
	closed, err := s.store.CloseBead(ctx, b.ID, autoCloseActor)
	if err != nil || closed == nil {
		slog.Warn("awaits_release: close failed", "bead", b.ID, "release", releaseID, "err", errString(err), "feature", "gd-ZM7NEBPAqZ")
		return
	}
	s.addAutoComment(ctx, b.ID, fmt.Sprintf(
		"auto-closed: shipped in %s %s (release %s), deployed to %s. This %s had awaits_release=true "+
			"and the release carries its work (P-R3, gd-ZM7NEBPAqZ). Reopen it if the work is not live.",
		project, tag, releaseID, target, b.Type))
	s.recordAndPublish(ctx, events.TopicBeadClosed, closed.ID, autoCloseActor, events.BeadClosed{
		Bead:     closed,
		ClosedBy: autoCloseActor,
	})
	s.recordMutation(ctx, closed.ID, autoCloseActor, "status",
		string(prevStatus), string(closed.Status), model.MutationTypeUpdate)
	slog.Info("awaits_release: closed shipped work", "bead", closed.ID, "release", releaseID, "target", target, "feature", "gd-ZM7NEBPAqZ")
	s.afterBeadClosed(ctx, closed)
}
