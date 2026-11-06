package main

// Per-user gardener ownership (Decision 5, design doc ka-0000003219 §6,
// task ka-0000002818).
//
// A gardener's owner is resolved in priority order:
//   1. GardenerSpec.UserBeadID — resolve the user bead → its operator_id → slug.
//   2. GardenerSpec.SlackUserID — LookupOperatorByAlias("slack", id) → operator → slug.
//   3. Trigger bead operated_by_id (FK) / operated_by — the already-resolved
//      OperatedByDisplay + OperatedBySlug (populated by populateOperatedBySlug).
//
// The result is (ownerDisplay, ownerSlug). An empty slug renders the display
// label without a link, matching the agentRowView.OperatedBySlug convention.
// The first tier that yields a non-empty display wins; the slug it carries may
// still be empty (e.g. a user bead with no provisioned slug) and that is fine —
// the chip degrades to a plain label.

import (
	"context"
	"strconv"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// resolveGardenerOwner resolves a gardener's owner display + slug in the
// three-tier priority order above. It is best-effort: every lookup failure is
// swallowed (logged at debug) and falls through to the next tier, so a gardener
// always renders — with a link when a slug resolves, with a plain label when
// only a display resolves, and with nothing when no tier produces a display.
func (s *Server) resolveGardenerOwner(ctx context.Context, b *beadsapi.BeadDetail, userBeadID, slackUserID string) (display, slug string) {
	// Tier 1 — user bead.
	if d, sl, ok := s.ownerFromUserBead(ctx, userBeadID); ok {
		return d, sl
	}
	// Tier 2 — Slack user id alias.
	if d, sl, ok := s.ownerFromSlackUserID(ctx, slackUserID); ok {
		return d, sl
	}
	// Tier 3 — trigger bead operated_by_id / operated_by (already resolved on
	// the bead surface by populateOperatedBySlug).
	if b != nil {
		return b.OperatedByDisplay, b.OperatedBySlug
	}
	return "", ""
}

// ownerFromUserBead resolves a GardenerSpec.UserBeadID to (display, slug). The
// display is the user bead's display_name / name / email; the slug comes from
// the user bead's operator_id via the shared slug resolver. ok is false when
// the id is empty, the bead is missing, or it yields no display.
//
// Memoized by user bead id (gardenerOwnerCache): the /gardeners index and the
// /u/{slug} their-gardeners section resolve every row's owner, and a handful of
// operators typically own many gardeners, so the first lookup serves the rest
// (perf regression fix ka-0000003665).
func (s *Server) ownerFromUserBead(ctx context.Context, userBeadID string) (display, slug string, ok bool) {
	userBeadID = strings.TrimSpace(userBeadID)
	if userBeadID == "" {
		return "", "", false
	}
	if isScopedCtx(ctx) {
		// Shared cache: never serve or fill it for a scoped principal.
		return s.resolveOwnerFromUserBead(ctx, userBeadID)
	}
	now := s.now()
	if e, hit := s.gardenerOwnerCache.getUserBead(userBeadID, now); hit {
		return e.display, e.slug, e.ok
	}
	display, slug, ok = s.resolveOwnerFromUserBead(ctx, userBeadID)
	s.gardenerOwnerCache.setUserBead(userBeadID, gardenerOwnerResolution{display: display, slug: slug, ok: ok}, now)
	return display, slug, ok
}

// resolveOwnerFromUserBead is the uncached body for ownerFromUserBead.
func (s *Server) resolveOwnerFromUserBead(ctx context.Context, userBeadID string) (display, slug string, ok bool) {
	bead, err := s.daemon.GetBead(ctx, userBeadID)
	if err != nil || bead == nil {
		if err != nil {
			s.logger.Debug("gardener owner: user bead lookup failed", "user_bead_id", userBeadID, "error", err)
		}
		return "", "", false
	}
	display = firstNonEmptyField(bead.Fields, "display_name", "name", "email")
	if display == "" {
		display = strings.TrimSpace(bead.Title)
	}
	if opID := parseOperatorID(bead.Fields["operator_id"]); opID > 0 {
		slug = s.resolveOperatorSlug(ctx, opID)
	}
	if display == "" && slug == "" {
		return "", "", false
	}
	return display, slug, true
}

// ownerFromSlackUserID resolves a GardenerSpec.SlackUserID to (display, slug)
// via the slack alias → operator → slug chain. The display is the operator's
// directory Display / Email, falling back to "slack:<id>" so the chip always
// has a label once the alias resolves.
//
// Memoized by Slack user id (gardenerOwnerCache): both the alias lookup and the
// operator-directory scan it triggers are otherwise re-run for every gardener
// row this owner appears on (perf regression fix ka-0000003665).
func (s *Server) ownerFromSlackUserID(ctx context.Context, slackUserID string) (display, slug string, ok bool) {
	slackUserID = strings.TrimSpace(slackUserID)
	if slackUserID == "" {
		return "", "", false
	}
	if isScopedCtx(ctx) {
		return s.resolveOwnerFromSlackUserID(ctx, slackUserID)
	}
	now := s.now()
	if e, hit := s.gardenerOwnerCache.getSlack(slackUserID, now); hit {
		return e.display, e.slug, e.ok
	}
	display, slug, ok = s.resolveOwnerFromSlackUserID(ctx, slackUserID)
	s.gardenerOwnerCache.setSlack(slackUserID, gardenerOwnerResolution{display: display, slug: slug, ok: ok}, now)
	return display, slug, ok
}

// resolveOwnerFromSlackUserID is the uncached body for ownerFromSlackUserID.
func (s *Server) resolveOwnerFromSlackUserID(ctx context.Context, slackUserID string) (display, slug string, ok bool) {
	resp, err := s.daemon.LookupOperatorByAlias(ctx, "slack", slackUserID)
	if err != nil || resp == nil || !resp.Found || resp.OperatorID == 0 {
		if err != nil {
			s.logger.Debug("gardener owner: slack alias lookup failed", "slack_user_id", slackUserID, "error", err)
		}
		return "", "", false
	}
	slug = s.resolveOperatorSlug(ctx, resp.OperatorID)
	display = s.directoryDisplayForOperator(ctx, resp.OperatorID)
	if display == "" {
		display = "slack:" + slackUserID
	}
	return display, slug, true
}

// directoryDisplayForOperator returns the operator's directory Display (falling
// back to Email) for the given operator id, or "" when the directory lookup
// fails or has no matching entry.
//
// The operator directory is loaded ONCE per TTL window via gardenerOwnerCache and
// shared across rows, so a /gardeners render with many distinct Slack-owned
// gardeners pays a single ListOperators scan rather than one per row (perf
// regression fix ka-0000003665).
func (s *Server) directoryDisplayForOperator(ctx context.Context, operatorID int64) string {
	if isScopedCtx(ctx) {
		return s.loadOperatorDirectory(ctx)[operatorID]
	}
	return s.gardenerOwnerCache.directoryDisplay(ctx, operatorID, s.now(), s.loadOperatorDirectory)
}

// loadOperatorDirectory fetches the operator directory and projects it to an
// (operator id → display) map, where display is the Display field falling back
// to Email. Returns an empty (non-nil) map on any lookup failure so the cache
// records a benign negative rather than retrying every row.
func (s *Server) loadOperatorDirectory(ctx context.Context) map[int64]string {
	out := make(map[int64]string)
	resp, err := s.daemon.ListOperators(ctx, 0)
	if err != nil || resp == nil {
		if err != nil {
			s.logger.Debug("gardener owner: operator directory load failed", "error", err)
		}
		return out
	}
	for _, o := range resp.Operators {
		if d := strings.TrimSpace(o.Display); d != "" {
			out[o.ID] = d
			continue
		}
		out[o.ID] = strings.TrimSpace(o.Email)
	}
	return out
}

// firstNonEmptyField returns the first non-empty trimmed value among the named
// fields, or "" when none are set.
func firstNonEmptyField(fields map[string]string, keys ...string) string {
	if fields == nil {
		return ""
	}
	for _, k := range keys {
		if v := strings.TrimSpace(fields[k]); v != "" {
			return v
		}
	}
	return ""
}

// parseOperatorID parses an operator_id field value to int64, returning 0 on
// any parse failure (the caller treats 0 as "no operator").
func parseOperatorID(raw string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}
