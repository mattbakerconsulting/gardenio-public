package main

import (
	"context"
	"html/template"
	"sort"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Detail-page enrichment limits. Sibling/rater-recent caps keep the focused
// rating page predictable when a single bead has dozens of ratings or one
// rater is prolific. References cap is laxer because each row is one line.
const (
	ratingDetailSiblingLimit     = 10
	ratingDetailRaterRecentLimit = 5
	ratingDetailReferenceLimit   = 20
	// ratingDetailRaterScan caps the type=rating list scan used to find a
	// single rater's other recent ratings — there is no daemon-side
	// FieldEquals query for `rater`, so we sort the recent slice and
	// post-filter. The cap bounds the worst-case fan-out.
	ratingDetailRaterScan = 200
)

// enrichRatingDetailView populates Siblings / RaterRecent / References on a
// rating detail view. Each lookup is best-effort — daemon errors degrade the
// section to empty rather than failing the page render.
//
// `mentionedBy` and `revDeps` are passed through from handleBeadShow so the
// references list reuses already-fetched data instead of issuing a second
// round-trip per page load.
func (s *Server) enrichRatingDetailView(
	ctx context.Context,
	rv *ratingView,
	bead *beadsapi.BeadDetail,
	mentionedBy []beadsapi.BeadReferenceSummary,
	revDeps []beadsapi.Dependency,
) {
	if rv == nil || bead == nil {
		return
	}
	now := time.Now()

	if rv.RatedBeadID != "" {
		if siblings, err := s.collectRatings(ctx, rv.RatedBeadID); err != nil {
			s.logger.Debug("collecting sibling ratings", "rated", rv.RatedBeadID, "error", err)
		} else {
			rv.Siblings = buildSiblingViews(siblings, bead.ID, s.renderBeadMarkdown, now, ratingDetailSiblingLimit)
		}
	}

	if rv.Rater != "" {
		res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Types: []string{"rating"},
			Sort:  "-created_at",
			Limit: ratingDetailRaterScan,
		})
		if err != nil {
			s.logger.Debug("listing rater recent ratings", "rater", rv.Rater, "error", err)
		} else if res != nil {
			rv.RaterRecent = buildRaterRecentViews(ctx, res.Beads, rv.Rater, bead.ID, s, now, ratingDetailRaterRecentLimit)
		}
	}

	rv.References = buildRatingReferences(revDeps, mentionedBy, ratingDetailReferenceLimit)
}

// buildSiblingViews flattens rating beads into ratingViews for the siblings
// list, excluding self. Sorted newest-first and capped at limit.
func buildSiblingViews(
	siblings []*beadsapi.BeadDetail,
	selfID string,
	renderMarkdown func(string) template.HTML,
	now time.Time,
	limit int,
) []ratingView {
	out := make([]ratingView, 0, len(siblings))
	for _, rb := range siblings {
		if rb == nil || rb.ID == selfID {
			continue
		}
		v := buildRatingView(rb, "", renderMarkdown, now)
		if v == nil {
			continue
		}
		out = append(out, *v)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// buildRaterRecentViews filters the rating-list scan to ratings authored by
// `rater` (excluding self), enriches each row with the rated-bead title for
// readability, and caps at limit. Rated-title lookup is best-effort.
func buildRaterRecentViews(
	ctx context.Context,
	beads []*beadsapi.BeadDetail,
	rater, selfID string,
	s *Server,
	now time.Time,
	limit int,
) []ratingView {
	out := make([]ratingView, 0, limit)
	for _, rb := range beads {
		if rb == nil || rb.ID == selfID {
			continue
		}
		if rb.Fields["rater"] != rater {
			continue
		}
		ratedTitle := ""
		if rated := rb.Fields["rated_bead_id"]; rated != "" {
			if rt, err := s.daemon.GetBead(ctx, rated); err == nil && rt != nil {
				ratedTitle = rt.Title
			}
		}
		v := buildRatingView(rb, ratedTitle, s.renderBeadMarkdown, now)
		if v == nil {
			continue
		}
		out = append(out, *v)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// buildRatingReferences merges reverse dependencies (excluding `rates`) and
// inbound text mentions into a flat list of inbound references. Capped at
// limit; deps are listed first, then mentions, dedup'd by BeadID.
func buildRatingReferences(
	revDeps []beadsapi.Dependency,
	mentionedBy []beadsapi.BeadReferenceSummary,
	limit int,
) []ratingReference {
	out := make([]ratingReference, 0, len(revDeps)+len(mentionedBy))
	seen := make(map[string]struct{}, len(revDeps)+len(mentionedBy))
	for _, dep := range revDeps {
		if dep.Type == ratingDepType {
			// The self-edge from this rating to its rated bead is shown
			// inline in the focused card; skip it here.
			continue
		}
		if _, ok := seen[dep.BeadID]; ok {
			continue
		}
		seen[dep.BeadID] = struct{}{}
		out = append(out, ratingReference{
			BeadID:  dep.BeadID,
			Kind:    "dep",
			DepType: dep.Type,
		})
	}
	for _, m := range mentionedBy {
		if _, ok := seen[m.ID]; ok {
			continue
		}
		seen[m.ID] = struct{}{}
		out = append(out, ratingReference{
			BeadID:      m.ID,
			Title:       m.Title,
			Type:        m.Type,
			Status:      m.Status,
			Kind:        "mention",
			SourceField: m.SourceField,
		})
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}
