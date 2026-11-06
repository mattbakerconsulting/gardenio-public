package bridge

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/decisionview"
)

// decisionEvidenceTimeout bounds the evidence reads a decision card makes: a
// slow daemon costs the evidence line, never the card.
const decisionEvidenceTimeout = 3 * time.Second

// decisionEvidenceTitleMax clips a source bead's title in the evidence line,
// which keeps three of them well inside a context element's 2000 chars.
const decisionEvidenceTitleMax = 120

// decisionEvidenceLine says where a decision's evidence is (gd-Pi7C83262N):
// "<N> screenshots on <source bead link>", one clause per bead the decision
// borrows images from (decisionview.LoadEvidence: its --evidence beads, then
// its requesting task). The card shows no image blocks: Slack fetches them
// anonymously, and the viewer's /image links are private, so a picture would
// render broken. "" when the decision has no evidence or the reads fail.
func (b *Bot) decisionEvidenceLine(ctx context.Context, bead BeadEvent) string {
	if b == nil || b.daemon == nil {
		return ""
	}
	d := &beadsapi.BeadDetail{ID: bead.ID, Fields: bead.Fields}
	if len(decisionview.EvidenceBeads(d)) == 0 {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, decisionEvidenceTimeout)
	defer cancel()
	srcs := decisionview.EvidenceBySource(decisionview.LoadEvidence(ctx, b.daemon, d, 0))
	if len(srcs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(srcs))
	for i, s := range srcs {
		count := fmt.Sprintf("%d", s.N) // "3 screenshots on A · 2 on B"
		if i == 0 && s.N == 1 {
			count += " screenshot"
		} else if i == 0 {
			count += " screenshots"
		}
		parts = append(parts, count+" on "+b.beadTitleLink(s.ID, truncateRunes(s.Title, decisionEvidenceTitleMax)))
	}
	return strings.Join(parts, " · ")
}
