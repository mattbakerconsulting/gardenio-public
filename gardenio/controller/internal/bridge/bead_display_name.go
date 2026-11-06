package bridge

// bead_display_name.go resolves the short human label a bead is shown under in
// Slack prose and the References footer (slack_link_hydrate.go).
//
// The label is the bead's authored short_title when it has one, else a
// type-aware de-noising of its title (shorttitle.Derive), else the id. The
// de-noising rules live in internal/shorttitle so the bridge's render-time
// fallback and the create-time fills share one source of truth; see the
// thread in gd-8Gxghn6hkt for why heuristics are the fallback, not the
// source of truth.
//
// A bead's arms (BeadDetail.Heraldry) ride beside the label, not in it: where
// it bears them, prose and the footer put its emoji run (armsRun) where
// the type glyph would go (heraldry spec R9, Q5), so the name stays words.

import (
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/shorttitle"
)

// displayNameMax is the longest display name we emit, in runes. Long enough
// for an imperative clause, short enough that six of them fit under a message
// without doubling its height.
const displayNameMax = 42

// shortTitleField is the bead field read as the authored short title when the
// short_title column is empty. The column (gd-V5nGseRS5f) is canonical; this
// fields read is the compatibility path for anything written before the
// column rolled out, and for a daemon that does not yet return the column.
const shortTitleField = "short_title"

// beadDisplayName returns the short label for bead. It never returns an empty
// string: when nothing usable survives de-noising it falls back to the id.
func beadDisplayName(bead *beadsapi.BeadDetail) string {
	if bead == nil {
		return ""
	}
	if st := strings.TrimSpace(bead.ShortTitle); st != "" {
		return shorttitle.Clip(st, displayNameMax)
	}
	if st := strings.TrimSpace(bead.Fields[shortTitleField]); st != "" {
		return shorttitle.Clip(st, displayNameMax)
	}
	if name := shorttitle.Derive(bead.Type, bead.Title); name != "" {
		return shorttitle.Clip(name, displayNameMax)
	}
	return bead.ID
}
