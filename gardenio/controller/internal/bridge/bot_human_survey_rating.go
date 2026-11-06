package bridge

// bot_human_survey_rating.go — Single-writer of `type=rating` beads from
// Slack human_survey button clicks / feedback-modal submissions. Phase 6
// of plans/beads-viewer-human-ratings-slack-fold.md cut over from the
// earlier dual-write path; the bridge no longer writes `type=human_survey`
// rows (the type is still registered in init.go for read-back compat).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

const (
	// ratingBeadType matches the kbeads server's registered type:rating
	// (kbeads/internal/server/config.go).
	ratingBeadType = "rating"

	// ratingDepType is the dependency edge from rating → rated bead.
	// Mirrors beads-viewer/ratings.go.
	ratingDepType = "rates"

	// slackRaterSentinelPrefix is used as the `rater` value when the
	// Slack users.info email lookup fails — keyed off the Slack user ID
	// so a later resolution can detect the sentinel and rewrite it.
	slackRaterSentinelPrefix = "slack:"

	// slackEmailCacheMax bounds the in-process Slack user-id → email
	// LRU cache. Eviction is approximate (FIFO on map iteration order).
	slackEmailCacheMax = 1024

	// slackEmailCacheTTL is how long a successful email resolution
	// stays cached. Sentinel (failed) entries use the shorter
	// slackEmailSentinelTTL so identity drift recovers quickly.
	slackEmailCacheTTL    = 24 * time.Hour
	slackEmailSentinelTTL = 5 * time.Minute
)

// slackEmailCacheEntry caches a resolved Slack user-id → email mapping.
// `email == ""` indicates a negative cache entry (lookup failed).
type slackEmailCacheEntry struct {
	email     string
	expiresAt time.Time
}

// mapNativeToStars converts the canonical native rating (-1/0/+1) into the
// 1-to-5 star scale stored in `type:rating.score`. Per plan §2:
//
//	-1 → 1   (worst)
//	 0 → 3   (neutral)
//	+1 → 5   (best)
//
// Out-of-range native values are clamped to the nearest legal output so the
// returned value is always a valid `score` for the rating bead type.
func mapNativeToStars(native int) int {
	switch {
	case native <= -1:
		return 1
	case native == 0:
		return 3
	default: // native >= 1
		return 5
	}
}

// operatorFromSlackUser returns the operated_by identity to stamp on
// daemon calls originating from a Slack user. Resolution order (R3,
// ka-0000002887; mirrors resolveJiraOperator / resolveGitLabOperator):
//
//  1. UserDirectory.LookupBySlackID(slackUserID): if a fleet user bead
//     resolves the Slack id, return `oidc:<email>`. This is the strongest
//     attribution and matches the jira/gitlab bridge fixes (ka-0000006070,
//     ka-0000004627) — the local user directory is authoritative when it
//     has a match, no Slack API round-trip needed.
//  2. Slack users.info lookup (resolveSlackRater): fall back to the
//     profile email when UserDirectory has no match — returned as
//     `oidc:<email>` so the daemon accepts it. Cached.
//  3. `slack:<user-id>` sentinel: identity is captured but unresolved;
//     a later resolution can rewrite it.
//  4. `fleet:slack-bridge`: truly anonymous — no Slack user id at all.
//
// Wraps resolveSlackRater so the email-resolution cache stays shared
// with the rating flow.
func (b *Bot) operatorFromSlackUser(ctx context.Context, slackUserID string) string {
	if slackUserID == "" {
		return "fleet:slack-bridge"
	}
	if b.userDir != nil {
		if u := b.UserDirectory().LookupBySlackID(ctx, slackUserID); u != nil && u.Email != "" {
			return "oidc:" + u.Email
		}
	}
	op, sentinelUsed := b.resolveSlackRater(ctx, slackUserID)
	if op == "" {
		return "fleet:slack-bridge"
	}
	// resolveSlackRater returns a BARE email (no prefix) when users.info
	// resolves one (sentinelUsed=false), but the `slack:U…` sentinel when it
	// cannot. The daemon's isHumanOperator only accepts `oidc:<email>` or
	// `slack:U…` — a bare email is rejected with HTTP 400 "operator could not
	// be resolved", silently killing mention-spawn on boxes whose UserDirectory
	// has no matching user bead. Stamp the oidc: prefix on the resolved-email
	// case so this fallback matches the UserDirectory branch above and step (2)
	// of the doc comment actually returns `oidc:<email>` (ka-0000001053).
	if !sentinelUsed {
		return "oidc:" + op
	}
	return op
}

// resolveSlackRater returns (rater, sentinelUsed). When the Slack users.info
// lookup succeeds and yields a non-empty email, rater is that email and
// sentinelUsed is false. On failure (no API client, lookup error, or empty
// email), rater is `slack:<user-id>` and sentinelUsed is true. A bounded
// in-process cache holds resolutions for slackEmailCacheTTL (or
// slackEmailSentinelTTL for negative entries) so repeat clicks don't hit
// Slack on every event.
func (b *Bot) resolveSlackRater(ctx context.Context, slackUserID string) (string, bool) {
	if slackUserID == "" {
		return "", true
	}
	if email, ok := b.lookupCachedSlackEmail(slackUserID); ok {
		if email != "" {
			return email, false
		}
		return slackRaterSentinelPrefix + slackUserID, true
	}
	if b.api == nil {
		b.cacheSlackEmail(slackUserID, "")
		return slackRaterSentinelPrefix + slackUserID, true
	}
	user, err := b.api.GetUserInfoContext(ctx, slackUserID)
	if err != nil || user == nil || strings.TrimSpace(user.Profile.Email) == "" {
		b.cacheSlackEmail(slackUserID, "")
		return slackRaterSentinelPrefix + slackUserID, true
	}
	email := strings.TrimSpace(user.Profile.Email)
	b.cacheSlackEmail(slackUserID, email)
	return email, false
}

func (b *Bot) lookupCachedSlackEmail(slackUserID string) (string, bool) {
	b.slackEmailCacheMu.Lock()
	defer b.slackEmailCacheMu.Unlock()
	entry, ok := b.slackEmailCache[slackUserID]
	if !ok {
		return "", false
	}
	if time.Now().After(entry.expiresAt) {
		delete(b.slackEmailCache, slackUserID)
		return "", false
	}
	return entry.email, true
}

func (b *Bot) cacheSlackEmail(slackUserID, email string) {
	b.slackEmailCacheMu.Lock()
	defer b.slackEmailCacheMu.Unlock()
	if b.slackEmailCache == nil {
		b.slackEmailCache = make(map[string]slackEmailCacheEntry)
	}
	// Best-effort bound: if at cap, drop one arbitrary entry. The cache
	// only needs to keep us off Slack for high-frequency repeat clicks;
	// an exact-LRU is overkill.
	if len(b.slackEmailCache) >= slackEmailCacheMax {
		for k := range b.slackEmailCache {
			delete(b.slackEmailCache, k)
			break
		}
	}
	ttl := slackEmailCacheTTL
	if email == "" {
		ttl = slackEmailSentinelTTL
	}
	b.slackEmailCache[slackUserID] = slackEmailCacheEntry{
		email:     email,
		expiresAt: time.Now().Add(ttl),
	}
}

// findExistingRating looks up an existing `type=rating` bead for this
// (agent_bead_id, rater) pair. It checks both `rater == email` and
// `slack_user_id == slackUserID` so that a rater whose email resolved
// after an earlier sentinel click is still found by the slack_user_id
// fallback path. Caller is responsible for any rater-rewrite (sentinel →
// email transition) — see writeRatingFromSlackClick.
func (b *Bot) findExistingRating(ctx context.Context, agentBeadID, email, slackUserID string) *beadsapi.BeadDetail {
	if agentBeadID == "" {
		return nil
	}
	res, err := b.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:  []string{ratingBeadType},
		Labels: []string{humanSurveyAgentLabelPrefix + agentBeadID},
		Limit:  100,
	})
	if err != nil || res == nil {
		return nil
	}
	for _, bead := range res.Beads {
		if bead == nil || bead.Fields == nil {
			continue
		}
		if bead.Fields["rated_bead_id"] != agentBeadID {
			continue
		}
		if email != "" && bead.Fields["rater"] == email {
			return bead
		}
		if slackUserID != "" && bead.Fields["slack_user_id"] == slackUserID {
			return bead
		}
	}
	return nil
}

// ratingWriteRequest carries the subset of state needed to dual-write a
// rating bead alongside the legacy human_survey path.
type ratingWriteRequest struct {
	agentBead     *beadsapi.BeadDetail
	agentBeadID   string
	scoreNative   int
	source        string // slack-stop | slack-manual | slack-feedback-only
	slackChannel  string
	slackThreadTS string
	slackUserID   string
	feedback      string // empty unless feedback-modal path
	feedbackRaw   string
	answeredAt    time.Time
}

// writeRatingFromSlackClick writes (or updates) the `type=rating` bead for
// a Slack human-survey rating or feedback submission. The
// (rated_bead_id, rater) idempotency contract is enforced both client-side
// (this function's findExistingRating lookup) and server-side
// (preCreateRating in kbeads). When an earlier sentinel-rater entry is now
// resolvable to an email, the existing bead's `rater` is rewritten in place;
// this is the only place where rater mutates.
func (b *Bot) writeRatingFromSlackClick(ctx context.Context, req ratingWriteRequest) {
	if req.agentBeadID == "" || req.agentBead == nil {
		return
	}
	score := mapNativeToStars(req.scoreNative)
	if req.source == "slack-feedback-only" {
		// Feedback-only: server requires score=0 for this source.
		score = 0
	}

	rater, sentinelUsed := b.resolveSlackRater(ctx, req.slackUserID)
	existing := b.findExistingRating(ctx, req.agentBeadID, raterIfNotSentinel(rater, sentinelUsed), req.slackUserID)

	answeredAt := req.answeredAt.UTC().Format(time.RFC3339)

	updateFields := map[string]any{
		"rated_bead_id":   req.agentBeadID,
		"rater":           rater,
		"slack_user_id":   req.slackUserID,
		"slack_channel":   req.slackChannel,
		"slack_thread_ts": req.slackThreadTS,
		"source":          req.source,
		"answered_at":     answeredAt,
	}
	if req.source != "slack-feedback-only" {
		updateFields["score"] = score
		updateFields["score_native"] = req.scoreNative
	} else {
		updateFields["score"] = 0
	}
	if req.feedback != "" {
		updateFields["feedback"] = req.feedback
	}
	if req.feedbackRaw != "" {
		updateFields["feedback_raw"] = req.feedbackRaw
	}

	if existing != nil {
		// Sentinel→email transition: an earlier click landed before the
		// email was resolvable. Rewrite the rater in place so future
		// queries by email find this row.
		if !sentinelUsed && strings.HasPrefix(existing.Fields["rater"], slackRaterSentinelPrefix) {
			updateFields["rater"] = rater
		} else if sentinelUsed && existing.Fields["rater"] != "" &&
			!strings.HasPrefix(existing.Fields["rater"], slackRaterSentinelPrefix) {
			// Existing has an email; do not regress to a sentinel.
			delete(updateFields, "rater")
		}
		if err := b.daemon.UpdateBeadFieldsTyped(ctx, existing.ID, updateFields); err != nil {
			b.logger.Error("rating dual-write: update existing failed",
				"bead", existing.ID, "agent_bead_id", req.agentBeadID, "error", err)
		}
		return
	}

	createFields := updateFields
	fieldsJSON, err := json.Marshal(createFields)
	if err != nil {
		b.logger.Error("rating dual-write: marshal fields failed",
			"agent_bead_id", req.agentBeadID, "error", err)
		return
	}

	labels := []string{humanSurveyAgentLabelPrefix + req.agentBeadID}
	if project := req.agentBead.Fields["project"]; project != "" {
		labels = append(labels, "project:"+project)
	}

	title := fmt.Sprintf("Rating: %s native=%d", req.agentBeadID, req.scoreNative)
	if req.source == "slack-feedback-only" {
		title = fmt.Sprintf("Rating: %s (feedback)", req.agentBeadID)
	}

	newID, err := b.daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:     title,
		Type:      ratingBeadType,
		Labels:    labels,
		CreatedBy: "slack-bridge",
		Fields:    fieldsJSON,
	})
	if err != nil {
		b.logger.Error("rating dual-write: create bead failed",
			"agent_bead_id", req.agentBeadID, "error", err)
		return
	}

	if err := b.daemon.AddDependency(ctx, newID, req.agentBeadID, ratingDepType, "slack-bridge"); err != nil {
		b.logger.Warn("rating dual-write: add rates dep failed",
			"bead", newID, "agent_bead_id", req.agentBeadID, "error", err)
	}
}

// raterIfNotSentinel returns rater for findExistingRating's email-lookup
// arg only when sentinelUsed is false, so a fresh sentinel call doesn't
// match prior sentinel beads via the email path (the slack_user_id path
// handles that case).
func raterIfNotSentinel(rater string, sentinelUsed bool) string {
	if sentinelUsed {
		return ""
	}
	return rater
}
