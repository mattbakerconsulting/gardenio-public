package bridge

// plan_badge_unfurl.go holds the plan predicate the link-unfurl card uses to
// pick the plan type badge (beadTypeBadge).
//
// The card used to also carry the plan's generated badge as a slack_file image
// block, uploaded through files.upload v2 without a channel (plan
// ka-0000005980, task ka-0000006440). chat.unfurl rejects any call whose
// unfurls carry such a block with cannot_parse_attachment, so every message
// that linked a plan lost ALL of its previews, not just the plan's. The badge
// path was removed; a future badge needs an image_url Slack can fetch
// anonymously (the viewer's /image proxy is OIDC-gated, so it cannot serve).

import (
	"slices"

	"gardenio/controller/internal/beadsapi"
)

// planBadgeUnfurlLabel is the label a bead must carry (in addition to
// type=epic) to be treated as a plan. Mirrors the beads-viewer planLabel const.
const planBadgeUnfurlLabel = "plan"

// beadIsPlan reports whether bead is a gardenio plan: a type=epic bead carrying
// the "plan" label. This is the same definition the beads-viewer uses to decide
// a bead renders on /plans.
func beadIsPlan(bead *beadsapi.BeadDetail) bool {
	if bead == nil || bead.Type != "epic" {
		return false
	}
	return slices.Contains(bead.Labels, planBadgeUnfurlLabel)
}
