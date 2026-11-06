package server

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// The deployment repair (gd-yn68eNqnu6): before deployRelease treated a helm
// revision as one roll per target, a late report of a roll already recorded
// landed as a new "now" row and closed whatever really ran there. The repair
// rebuilds a project's deployment history from the rows it has: rows of one
// release with the same (target, revision) collapse into the earliest, and
// each target's rows are re-chained in since order, every row closed at the
// next one's since and the newest left open. A clean history comes out
// unchanged, so a second run writes nothing.

// releaseConflict is a release write that contradicts what is recorded: a
// revision another release of the project already holds on the target. The
// release routes answer it 409.
type releaseConflict string

func (e releaseConflict) Error() string { return string(e) }

// releaseRepairChange is one release the repair rewrote, or would on a dry
// run: its rows before and after, and its status before and after.
type releaseRepairChange struct {
	ID           string              `json:"id"`
	Tag          string              `json:"tag"`
	StatusBefore string              `json:"status_before"`
	Status       string              `json:"status"`
	Removed      []releaseDeployment `json:"removed,omitempty"`
	Before       []releaseDeployment `json:"before"`
	Deployments  []releaseDeployment `json:"deployments"`
}

// releaseRepairResult is the answer of POST /v1/releases/repair-deployments.
// Conflicts names each (target, revision) recorded on two releases, which the
// repair leaves for a human: a revision cannot carry two tags.
type releaseRepairResult struct {
	Project   string                `json:"project"`
	DryRun    bool                  `json:"dry_run"`
	Releases  []releaseRepairChange `json:"releases"`
	Conflicts []string              `json:"conflicts,omitempty"`
}

// repairReleaseDeployments collapses duplicate (target, revision) rows of each
// release of project into the earliest and re-chains every target's rows, then
// recomputes each changed release's status, in one transaction under
// releaseWriteMu. dryRun computes the same answer and writes nothing. A
// release whose deployments do not parse is left out, as deployRelease leaves
// it alone. The caller has passed releaseWriterDenial.
func (s *BeadsServer) repairReleaseDeployments(ctx context.Context, project string, dryRun bool) (*releaseRepairResult, error) {
	s.releaseWriteMu.Lock()
	defer s.releaseWriteMu.Unlock()

	if !beadMatchesTokenProject(ctx, []string{"project:" + project}) {
		return nil, &AuthzDenial{Action: releaseDeployAction, BeadType: string(model.TypeRelease), Message: "token not authorized for this project's releases"}
	}
	if _, _, err := s.releaseProject(ctx, project); err != nil {
		return nil, err
	}
	rels, err := s.projectReleases(ctx, project)
	if err != nil {
		return nil, err
	}

	var hs []*repairHeld
	var entries []*repairEntry
	for _, b := range rels {
		m := releaseFieldMap(b.Fields)
		deps, err := parseReleaseDeployments(m["deployments"])
		if err != nil {
			continue
		}
		for _, d := range deps {
			entries = append(entries, &repairEntry{rel: len(hs), row: d, open: d.Until == ""})
		}
		hs = append(hs, &repairHeld{bead: b, m: m, before: deps})
	}
	res := &releaseRepairResult{Project: project, DryRun: dryRun, Releases: []releaseRepairChange{}}
	res.Conflicts = rechainReleaseHistory(hs, entries)

	var writes []releaseWrite
	for _, h := range hs {
		before, _ := json.Marshal(h.before)
		after, _ := json.Marshal(h.after)
		if string(before) == string(after) {
			continue
		}
		status := releaseStatus(releaseString(h.m, "tag"), releaseString(h.m, "built_at"), h.after)
		res.Releases = append(res.Releases, releaseRepairChange{
			ID: h.bead.ID, Tag: releaseString(h.m, "tag"),
			StatusBefore: releaseStatus(releaseString(h.m, "tag"), releaseString(h.m, "built_at"), h.before), Status: status,
			Removed: h.removed, Before: h.before, Deployments: h.after,
		})
		if dryRun {
			continue
		}
		old := string(h.bead.Fields)
		h.m["deployments"] = h.after
		h.m["status"] = status
		fields, err := json.Marshal(h.m)
		if err != nil {
			return nil, fmt.Errorf("encoding release %s fields: %w", h.bead.ID, err)
		}
		h.bead.Fields = fields
		h.bead.UpdatedAt = time.Now().UTC()
		writes = append(writes, releaseWrite{bead: h.bead, old: old})
	}
	if len(writes) == 0 {
		return res, nil
	}
	if err := s.commitReleaseWrites(ctx, writes); err != nil {
		return nil, err
	}
	return res, nil
}

// repairHeld is one release the repair reads: its fields, its rows as stored
// and as rebuilt, and the duplicate rows it dropped.
type repairHeld struct {
	bead          *model.Bead
	m             map[string]any
	before, after []releaseDeployment
	removed       []releaseDeployment
}

// repairEntry is one deployment row of a project, with the release it is on.
type repairEntry struct {
	rel  int
	row  releaseDeployment
	open bool
	drop bool
}

// rechainReleaseHistory rebuilds each held release's after from entries:
// rows of one release with the same (target, revision) collapse into the
// earliest, and each target's rows are re-chained in since order, every row
// closed at the next one's since and the newest left open. It returns each
// (target, revision) recorded on two releases, which it leaves as they are.
func rechainReleaseHistory(hs []*repairHeld, entries []*repairEntry) []string {
	// Oldest first; of two rows with one since, the one open now sorts last,
	// so it stays the one that runs.
	slices.SortStableFunc(entries, func(a, b *repairEntry) int {
		if c := compareReleaseSince(a.row, b.row); c != 0 {
			return c
		}
		switch {
		case !a.open && b.open:
			return -1
		case a.open && !b.open:
			return 1
		}
		return 0
	})

	var conflicts []string
	first := map[string]*repairEntry{}
	for _, e := range entries {
		if e.row.Revision == "" {
			continue
		}
		key := e.row.Target + "\x00" + e.row.Revision
		f, ok := first[key]
		switch {
		case !ok:
			first[key] = e
		case f.rel != e.rel:
			conflicts = append(conflicts, fmt.Sprintf("revision %s on %s is recorded for both %s and %s",
				e.row.Revision, e.row.Target, releaseString(hs[f.rel].m, "tag"), releaseString(hs[e.rel].m, "tag")))
		default:
			e.drop = true
			hs[e.rel].removed = append(hs[e.rel].removed, e.row)
		}
	}
	newest := map[string]*repairEntry{}
	for _, e := range entries {
		if e.drop {
			continue
		}
		if prev := newest[e.row.Target]; prev != nil {
			prev.row.Until = e.row.Since
		}
		newest[e.row.Target] = e
	}
	for _, e := range newest {
		e.row.Until = ""
	}
	for _, e := range entries {
		if !e.drop {
			hs[e.rel].after = append(hs[e.rel].after, e.row)
		}
	}
	return conflicts
}

// releaseWrite is one bead a release write rewrites, with its fields before.
type releaseWrite struct {
	bead *model.Bead
	old  string
}

// commitReleaseWrites stores writes in one transaction, then publishes and
// records each update. Only the release writes call it (deployRelease,
// repairReleaseDeployments), after releaseWriterDenial: the fields are the
// daemon's fixed keys, deployments and status, and a declared target.
func (s *BeadsServer) commitReleaseWrites(ctx context.Context, writes []releaseWrite) error {
	if err := s.store.RunInTransaction(ctx, func(tx store.Store) error {
		for _, w := range writes {
			if err := tx.UpdateBead(ctx, w.bead); err != nil {
				return fmt.Errorf("updating release %s: %w", w.bead.ID, err)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	actor := actorFromContext(ctx)
	for _, w := range writes {
		changes := map[string]any{"fields": w.bead.Fields}
		s.recordAndPublish(ctx, events.TopicBeadUpdated, w.bead.ID, "", events.BeadUpdated{
			Bead:    w.bead,
			Changes: changes,
			Actor:   actor,
		})
		s.recordUpdates(ctx, w.bead.ID, actor, changes, map[string]any{"fields": w.old})
	}
	return nil
}
