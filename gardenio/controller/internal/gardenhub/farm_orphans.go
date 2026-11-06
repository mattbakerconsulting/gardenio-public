package gardenhub

import (
	"context"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

const (
	// The orphaned-PR read (FarmPiles.OrphanMRs): the newest maxOrphanScan open
	// PR beads are checked against the open rows; one not carried by an open bead
	// costs one lookup for the bead that links it, at most maxOrphanLookups per
	// slow tick, and the answer is kept while the PR stays open.
	maxOrphanScan    = 200
	maxOrphanLookups = 10
	maxOrphanMRs     = 24
)

// orphanForge is the forge whose merge-request beads the farm reads for
// orphaned PRs: github-bridge keeps their status current (open while the PR is
// open). GitLab MR beads are left out, since nothing keeps them current now
// that CI and review moved to GitHub.
const orphanForge = "github"

// orphanWorkTypes are the bead types whose mr_url claims a PR.
var orphanWorkTypes = []string{"task", "feature", "bug", "chore", "epic"}

// orphanRef is what the lookup for a PR's work bead found: the newest work
// bead whose mr_url is the PR, or nothing (id "").
type orphanRef struct {
	id, project, status, assignee string
	updated                       int64
}

// readOrphans lists the open PRs that no open work bead carries (the orphaned-PR
// weed). An open PR whose mr_url is on an open row is fine. Any other is looked
// up by mr_url across every status: a closed bead makes it OrphanBeadClosed, no
// bead OrphanNoBead, and an open bead outside the row cap is not an orphan. The
// lookups are cached per PR and bounded per tick; a PR not yet looked up waits
// for a later tick rather than being guessed. Newest PR first.
func (f *Farm) readOrphans(ctx context.Context, open map[string]*beadsapi.BeadDetail) ([]FarmOrphanMR, error) {
	res, err := f.src.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: []string{"merge-request"}, Statuses: []string{"open"}, Fields: map[string]string{"forge": orphanForge},
		Sort: "-updated_at", Limit: maxOrphanScan,
	})
	if err != nil {
		return nil, err
	}
	carried := map[string]bool{}
	for _, b := range open {
		if u := strings.TrimSpace(b.Fields["mr_url"]); u != "" {
			carried[u] = true
		}
	}
	cache, keep, lookups := f.slow.orphans, map[string]orphanRef{}, 0
	out := []FarmOrphanMR{}
	for _, mr := range res.Beads {
		if mr == nil {
			continue
		}
		u := strings.TrimSpace(mr.Fields["mr_url"])
		if u == "" || carried[u] {
			continue
		}
		ref, ok := cache[u]
		if !ok {
			if lookups >= maxOrphanLookups {
				continue
			}
			lookups++
			got, err := f.src.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
				Types: orphanWorkTypes, Fields: map[string]string{"mr_url": u}, Sort: "-updated_at", Limit: 1,
			})
			if err != nil {
				return nil, err
			}
			if len(got.Beads) > 0 && got.Beads[0] != nil {
				b := got.Beads[0]
				ref = orphanRef{id: b.ID, project: projectOf(b), status: b.Status, assignee: b.Assignee, updated: b.UpdatedAt.Unix()}
			}
		}
		keep[u] = ref
		if ref.id != "" && ref.status != "closed" {
			continue // an open bead past the row cap carries it
		}
		o := FarmOrphanMR{URL: u, IID: mr.Fields["mr_iid"], Title: mr.Title, Repo: mr.Fields["project_path"], Why: OrphanNoBead,
			Project: projectOf(mr), Since: mr.CreatedAt.Unix()}
		if ref.id != "" {
			o.Why, o.Bead, o.Assignee, o.Since = OrphanBeadClosed, ref.id, ref.assignee, ref.updated
			if ref.project != "" {
				o.Project = ref.project
			}
		}
		out = append(out, o)
	}
	f.slow.orphans = keep
	return out, nil
}
