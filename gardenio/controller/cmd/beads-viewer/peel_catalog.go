package main

import (
	"slices"
	"sort"
	"strconv"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// The peel catalog is the beads-viewer projection of the R4 pull-on-demand
// "menu" surface (design doc ka-0000006592 §4.2, master epic ka-0000005560).
// The forward-engineering gardener scores every merged monorepo-dark MR on a
// 0–100 peel_score and tags it peel:eligible|parked + risk:*. This surface
// renders the ranked eligible slices as a browsable menu — score, size, risk,
// test-coverage, one-line provenance — and gives each row a "request peel"
// affordance.
//
// PULL, NOT PUSH (design §4.2/§4.4, invariant ka-0000003234): browsing the
// catalog opens NO upstream MR. The "request peel" action writes a
// peel-request DECISION bead (label `peel-request`) linked back to the dark MR
// bead; that decision IS the demand signal, and a bead_event trigger on the
// `peel-request` label wakes the demand-driven peel gardener, which opens ONE
// standalone upstream MR with provenance and stops — the merge stays
// human-gated. The catalog itself never touches upstream and never merges.
//
// This file owns the read/menu half (buildPeelCatalog + row scoring/sorting);
// peel_request.go owns the "request peel" write half.

// peelCatalogLabelEligible / peelCatalogLabelParked are the gardener-written
// tags that classify a scored dark MR. Eligible slices are catalogued; parked
// ones (score below the gardener's floor, or self-containedness broken) are
// surfaced separately so an operator can see WHY a slice is not offered rather
// than having it silently vanish (design §4.1 "Below a floor → parked, not
// catalogued" — we still SHOW parked, we just don't rank them as offerable).
const (
	peelCatalogLabelEligible = "peel:eligible"
	peelCatalogLabelParked   = "peel:parked"
	// peelRequestLabel is the marker the "request peel" action stamps on the
	// decision bead it files. The peel gardener's bead_event trigger matches
	// this label to wake (design §4.2). Kept here so the read surface (which
	// detects an already-requested MR) and the write surface (peel_request.go)
	// agree on the single string.
	peelRequestLabel = "peel-request"
	// peelRequestedMRLabel is stamped on the dark MR bead once a peel has been
	// requested for it, so a second visitor sees "requested" instead of
	// re-filing a duplicate decision. Distinct from peelRequestLabel (which
	// lives on the decision bead).
	peelRequestedMRLabel = "peel:requested"
)

// peelRiskClass classifies a slice by its gardener-assigned risk label. The
// order here is the display/sort order (low first) — a low-risk slice is the
// better first trickle (design §3.1 trickle ordering).
const (
	peelRiskLow     = "low"
	peelRiskMedium  = "medium"
	peelRiskHigh    = "high"
	peelRiskUnknown = "unknown"
)

// peelCatalogListLimit caps the dark-MR fetch backing the menu. Mirrors the
// other list surfaces' explicit cap + truncation banner (escalationsListLimit).
// The eligible+parked population is small by construction (only scored, merged
// dark MRs), so this is a generous ceiling, not an expected boundary.
const peelCatalogListLimit = 300

// peelCatalogRow is one slice in the menu. Fields mirror the design's menu
// spec: "title, score, size, risk, one-line provenance, request-peel
// affordance" (§4.2), plus the test-coverage signal (§4.1) the task's
// acceptance criteria calls out explicitly.
type peelCatalogRow struct {
	// BeadID is the dark MR bead id. The "request peel" action and the row
	// permalink both key on it.
	BeadID string
	// Title is the MR title (the human-facing slice name).
	Title string
	// MRURL / MRShort are the dark GitLab MR link + its "<project>!<iid>"
	// short form. External — this opens the dark instance, not upstream.
	MRURL   string
	MRShort string

	// Score is the 0–100 peel_score. ScoreKnown is false when the gardener has
	// not scored the MR yet (the field is absent/unparsable) — the row then
	// renders "—" rather than a fabricated 0.
	Score      int
	ScoreKnown bool

	// Risk is the normalized risk class (low|medium|high|unknown), derived
	// from the risk:* label or a risk field.
	Risk string

	// SizeLOC / SizeFiles are the slice size signals (net LOC, files touched).
	// SizeKnown is false when neither is populated.
	SizeLOC   int
	SizeFiles int
	SizeKnown bool

	// Coverage is the human-facing test-coverage signal ("tests added, CI
	// green", "no tests", …). CoverageKnown false ⇒ render nothing.
	Coverage      string
	CoverageKnown bool

	// Provenance is the one-line provenance summary (originating ticket +
	// dark-MR chain). Empty ⇒ omitted.
	Provenance string
	// OriginatingTicket is the bead id of the ticket the slice was peeled
	// from, for an in-viewer permalink. Empty ⇒ no link.
	OriginatingTicket string

	// Requested is true when a peel has already been requested for this MR
	// (the MR bead carries peelRequestedMRLabel). The row then shows
	// "requested" and suppresses the request affordance so no duplicate
	// decision is filed.
	Requested bool
	// Eligible mirrors the gardener's peel:eligible tag. Parked rows
	// (Eligible=false) render in a separate, non-requestable section.
	Eligible bool
}

// peelCatalogView is the /peel-catalog page view-model.
type peelCatalogView struct {
	BasePath string

	// Eligible are the ranked offerable slices (peel:eligible), highest score
	// first. Parked are surfaced-but-not-offered slices (peel:parked), so an
	// operator can see what the gardener declined to offer and why.
	Eligible []peelCatalogRow
	Parked   []peelCatalogRow

	// CanRequest gates the "request peel" affordance. False for an
	// unauthenticated viewer (the write is operator-gated at the route, but we
	// also hide the button so a read-only viewer isn't offered a 401).
	CanRequest bool

	// Truncated / Limit drive the "showing first N" banner when the fetch hit
	// the cap.
	Truncated bool
	Limit     int
}

// HasAny reports whether the catalog has anything to render (either section).
func (v *peelCatalogView) HasAny() bool { return len(v.Eligible) > 0 || len(v.Parked) > 0 }

// RiskGlyphClass returns the CSS modifier class for a row's risk chip so the
// template can color low/med/high without a switch in the markup.
func (r peelCatalogRow) RiskGlyphClass() string {
	switch r.Risk {
	case peelRiskLow:
		return "peel-risk-low"
	case peelRiskMedium:
		return "peel-risk-medium"
	case peelRiskHigh:
		return "peel-risk-high"
	default:
		return "peel-risk-unknown"
	}
}

// buildPeelCatalogRows turns a set of dark MR beads into ranked catalog rows,
// split into eligible (offerable, score-desc) and parked (surfaced-only)
// sections. It is a pure function of its inputs so the ranking/scoring is
// unit-testable without a handler. In-viewer permalinks are composed in the
// template from the row's BeadID/OriginatingTicket + the view's BasePath.
func buildPeelCatalogRows(beads []*beadsapi.BeadDetail) (eligible, parked []peelCatalogRow) {
	for _, b := range beads {
		if b == nil {
			continue
		}
		row := peelCatalogRowFromBead(b)
		if row.Eligible {
			eligible = append(eligible, row)
		} else {
			parked = append(parked, row)
		}
	}
	// Eligible slices rank by score descending — the menu is a priority queue
	// the gardener satisfies top-first. Ties break by ascending size (a smaller
	// slice is the safer trickle) then by bead id for a stable order.
	sortPeelRows(eligible)
	// Parked slices are not offerable, but showing the highest-scoring parked
	// slices first tells the operator which are closest to eligible.
	sortPeelRows(parked)
	return eligible, parked
}

// sortPeelRows applies the catalog ranking: score desc, then size asc (unknown
// size sorts last), then bead id asc for determinism.
func sortPeelRows(rows []peelCatalogRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		// Known size ranks ahead of unknown; among known, smaller first.
		if a.SizeKnown != b.SizeKnown {
			return a.SizeKnown
		}
		if a.SizeKnown && b.SizeKnown && a.SizeLOC != b.SizeLOC {
			return a.SizeLOC < b.SizeLOC
		}
		return a.BeadID < b.BeadID
	})
}

// peelCatalogRowFromBead extracts one menu row from a dark MR bead's labels +
// fields. It never fabricates a value — an absent score/size/coverage stays
// unknown so the menu reflects what the gardener actually computed.
func peelCatalogRowFromBead(b *beadsapi.BeadDetail) peelCatalogRow {
	f := b.Fields
	row := peelCatalogRow{
		BeadID:    b.ID,
		Title:     b.Title,
		Eligible:  peelHasLabel(b.Labels, peelCatalogLabelEligible),
		Requested: peelHasLabel(b.Labels, peelRequestedMRLabel),
	}

	row.MRURL = mrFieldString(f, "mr_url", "url")
	if row.MRURL != "" {
		row.MRShort = shortenMRURL(row.MRURL)
	}

	if s, ok := parsePeelScore(f["peel_score"]); ok {
		row.Score = s
		row.ScoreKnown = true
	}

	row.Risk = normalizePeelRisk(b.Labels, f)

	row.SizeLOC, row.SizeFiles, row.SizeKnown = parsePeelSize(f)

	if cov := peelCoverageSummary(f); cov != "" {
		row.Coverage = cov
		row.CoverageKnown = true
	}

	row.OriginatingTicket = firstNonEmpty(f, "originating_ticket", "peel_of", "peel_of_bead", "source_ticket")
	row.Provenance = peelProvenanceSummary(f, row.OriginatingTicket, row.MRShort)

	return row
}

// parsePeelScore parses the peel_score field into a 0–100 int, clamping
// out-of-range values. The bool is false when the field is absent or
// unparsable — the caller then renders the score as unknown rather than 0.
func parsePeelScore(raw string) (int, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	// Accept a float-shaped score ("72.5") by truncating to int.
	if strings.Contains(raw, ".") {
		fv, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return 0, false
		}
		return clampScore(int(fv)), true
	}
	iv, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false
	}
	return clampScore(iv), true
}

// clampScore bounds a raw score to [0,100].
func clampScore(v int) int {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

// normalizePeelRisk resolves a slice's risk class from a risk:* label first
// (the design's canonical carrier, §4.1/§4.2) then a `risk` / `risk_class`
// field, normalized to low|medium|high. Anything else is "unknown".
func normalizePeelRisk(labels []string, f map[string]string) string {
	for _, l := range labels {
		if rest, ok := strings.CutPrefix(l, "risk:"); ok {
			if norm := canonicalRisk(rest); norm != peelRiskUnknown {
				return norm
			}
		}
	}
	if v := firstNonEmpty(f, "risk", "risk_class"); v != "" {
		return canonicalRisk(v)
	}
	return peelRiskUnknown
}

// canonicalRisk maps a raw risk token to the three known classes. "med" and
// "moderate" fold into medium; unrecognized tokens stay unknown.
func canonicalRisk(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "low":
		return peelRiskLow
	case "medium", "med", "moderate":
		return peelRiskMedium
	case "high", "critical":
		return peelRiskHigh
	default:
		return peelRiskUnknown
	}
}

// parsePeelSize reads the slice-size signals. It accepts the canonical
// peel-specific fields first, then falls back to the generic MR diff-stat
// fields the gitlab-bridge stamps. The bool is false when no size signal is
// present at all.
func parsePeelSize(f map[string]string) (loc, files int, known bool) {
	loc, locKnown := parsePeelInt(f, "peel_size_loc", "net_loc", "mr_lines_changed", "lines_changed")
	if !locKnown {
		// Reconstruct net LOC from additions + deletions when only those are
		// present (the bridge stamps them separately).
		add, addKnown := parsePeelInt(f, "mr_additions", "additions")
		del, delKnown := parsePeelInt(f, "mr_deletions", "deletions")
		if addKnown || delKnown {
			loc = add + del
			locKnown = true
		}
	}
	files, filesKnown := parsePeelInt(f, "peel_size_files", "mr_files_changed", "files_changed", "changed_files")
	return loc, files, locKnown || filesKnown
}

// parsePeelInt returns the first present, parseable non-negative int among
// keys. The bool distinguishes "absent" from "explicitly 0".
func parsePeelInt(f map[string]string, keys ...string) (int, bool) {
	for _, k := range keys {
		raw, ok := f[k]
		if !ok {
			continue
		}
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 {
			continue
		}
		return v, true
	}
	return 0, false
}

// peelCoverageSummary renders the test-coverage signal (design §4.1: "Did the
// dark MR add/modify tests AND did dark CI pass?"). It prefers an explicit
// human-facing field, then derives a phrase from the boolean tests-added / CI
// fields. Empty ⇒ no coverage signal to show.
func peelCoverageSummary(f map[string]string) string {
	if v := strings.TrimSpace(firstNonEmpty(f, "peel_coverage", "coverage_summary")); v != "" {
		return v
	}
	testsAdded, testsKnown := boolField(f, "peel_tests_added", "tests_added")
	if !testsKnown {
		return ""
	}
	if !testsAdded {
		return "no tests added"
	}
	// Tests were added — qualify with the dark-CI result when known.
	ci := mrFieldString(f, "peel_ci_result", "mr_pipeline_status", "ci_status")
	switch strings.ToLower(strings.TrimSpace(ci)) {
	case "success", "passed", "green":
		return "tests added · CI green"
	case "failed", "red":
		return "tests added · CI red"
	default:
		return "tests added"
	}
}

// peelProvenanceSummary builds the one-line provenance blurb for a row. It
// prefers an explicit peel_provenance field; otherwise it composes from the
// originating ticket + dark MR short form so the menu still shows lineage on a
// bead the gardener has only partly annotated.
func peelProvenanceSummary(f map[string]string, ticket, mrShort string) string {
	if v := strings.TrimSpace(firstNonEmpty(f, "peel_provenance", "provenance")); v != "" {
		return v
	}
	var parts []string
	if ticket != "" {
		parts = append(parts, "from "+ticket)
	}
	if mrShort != "" {
		parts = append(parts, "dark "+mrShort)
	}
	return strings.Join(parts, " · ")
}

// peelHasLabel reports whether labels contains want (exact match). A slice-of-
// strings variant of the package hasLabel (which takes a *BeadDetail), so the
// pure row builder can be tested against a label slice directly.
func peelHasLabel(labels []string, want string) bool {
	return slices.Contains(labels, want)
}
