package gardenhub

import (
	"context"
	"time"
)

// ParseHookEvent decodes a hook relay payload. false for a malformed one, one without an agent bead id, or an event
// the feed does not read.
func ParseHookEvent(data []byte, now time.Time) (HookActivity, bool) {
	a, _, ok := parseHookEvent(data, now)
	return a, ok
}

// readBead assembles the panel's document: the bead, then its comments and both dependency directions in parallel,
// then the titles of the related beads (bounded). Only the bead itself is required; the rest degrades to Partial.
func readBead(ctx context.Context, src BeadReader, id string) (*beadDoc, error) {
	return readBeadN(ctx, src, id, beadCommentsMax, nil)
}
