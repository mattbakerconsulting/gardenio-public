package main

import (
	"context"
	"net/http"
	"sort"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// formulaListLimit caps the number of formula recipes pulled from the daemon.
// The catalog is a "what recipes do we have" index — the real fleet carries a
// few dozen formulas, so a 500 cap truncates nothing in practice while bounding
// the response like /active does.
const formulaListLimit = 500

// moleculeRollupLimit caps the open-molecule set the catalog groups by
// formula_id. Live instances of a recipe number in the low dozens; the cap
// keeps the single grouping query bounded on a busy fleet.
const moleculeRollupLimit = 500

// formulaStatusPipCap bounds the per-formula status-pip strip so a recipe with
// a large live fan-out renders a compact strip plus an "+N" overflow chip
// rather than hundreds of pips.
const formulaStatusPipCap = 12

// formulaRow is one row on the /formulas catalog — a single type=formula recipe
// plus its live-molecule rollup. Counts and badges are derived from the
// formula bead's fields; the rollup is joined from open type=molecule beads
// grouped by their formula_id field.
type formulaRow struct {
	ID          string
	Title       string
	Project     string
	FormulaType string // workflow (default) / expansion / aspect
	Phase       string // vapor / liquid (recommended instantiation), may be ""
	StepCount   int
	VarCount    int

	// Live-molecule rollup (open molecules whose formula_id == this recipe).
	LiveCount int
	// Pips is the compact status strip — one glyph per live molecule (capped).
	Pips []formulaStatusPip
	// PipOverflow is LiveCount - len(Pips) when the strip was truncated.
	PipOverflow int
	// Frontier is a compact status breakdown of the live instances
	// (e.g. "3 open · 1 in progress"). It is set ONLY when the pip strip is
	// truncated (PipOverflow > 0): the visible pips already encode each
	// molecule's status glyph-by-glyph and colour-coded, so on an un-truncated
	// row the tally would merely restate the pips and the N-live count. On a
	// truncated row the strip can't show every molecule, so the tally earns its
	// space by accounting for the whole live set. Empty otherwise.
	Frontier string
}

// formulaStatusPip is one glyph in a formula row's live-molecule status strip.
// Status is the molecule's own bead status; MoleculeID links the pip to its
// instance.
type formulaStatusPip struct {
	MoleculeID string
	Status     string
	Symbol     string
}

// formulaTypeOption is one entry in the formula_type filter control.
type formulaTypeOption struct {
	Value    string
	Label    string
	Selected bool
}

// handleFormulas renders /formulas — a catalog of every type=formula recipe with
// a live-molecule rollup per row. Answers Matt's "what formulas do we have?"
// (formula-legibility plan ka-0000007376, step S1 ka-0000000872).
//
// Read-only, behind ambient auth (plain HandleFunc, like /active and /gardeners).
// Filters (all optional query params):
//   - ?project=<name>       — restrict to one project
//   - ?formula_type=<t>     — workflow / expansion / aspect
//   - ?has_live=1           — only recipes with ≥1 open molecule
func (s *Server) handleFormulas(w http.ResponseWriter, r *http.Request) {
	projectFilter := strings.TrimSpace(r.URL.Query().Get("project"))
	typeFilter := strings.TrimSpace(r.URL.Query().Get("formula_type"))
	hasLiveOnly := isTruthy(r.URL.Query().Get("has_live"))

	email := oidcauth.EmailFromContext(r.Context())

	data := map[string]any{
		"ProjectFilter": projectFilter,
		"FormulaType":   typeFilter,
		"HasLiveOnly":   hasLiveOnly,
		"ViewerEmail":   email,
		"InboxCount":    s.inboxCountForViewer(r.Context(), email),
		"Rows":          []formulaRow{},
		"Summary":       formulaCatalogSummary{},
		"TypeOptions":   formulaTypeOptions(typeFilter),
		// Pour… row action (ka-0000000251): render only for operators who can
		// actually succeed at the POST (role + authenticated identity), so
		// read-only viewers see state without a dead button.
		"IsOperator": viewerCanOperateAuthed(r),
	}

	q := beadsapi.ListBeadsQuery{
		Types: []string{"formula"},
		Limit: formulaListLimit,
	}
	if projectFilter != "" {
		q.Labels = []string{"project:" + projectFilter}
	}
	res, err := s.daemon.ListBeadsFiltered(r.Context(), q)
	if err != nil {
		s.logger.Error("listing formula beads", "error", err)
		data["Error"] = "Failed to load formulas."
		s.render(w, r, "formulas.html", data)
		return
	}

	// Join the live-molecule rollup: open molecules grouped by formula_id.
	// Best-effort — a daemon error here only drops the rollup, the recipe rows
	// still render with zero live counts.
	rollup := s.moleculeRollupByFormula(r.Context(), projectFilter)

	rows := make([]formulaRow, 0, len(res.Beads))
	summary := formulaCatalogSummary{}
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		row := buildFormulaRow(b, rollup[b.ID])
		if typeFilter != "" && row.FormulaType != typeFilter {
			continue
		}
		if hasLiveOnly && row.LiveCount == 0 {
			continue
		}
		rows = append(rows, row)
		summary.Formulas++
		summary.LiveMolecules += row.LiveCount
		if row.LiveCount > 0 {
			summary.WithLive++
		}
	}

	// Sort recipes with live instances first (the operator's "what's running"
	// lens), then by title so the catalog is stable and scannable.
	sort.SliceStable(rows, func(i, j int) bool {
		if (rows[i].LiveCount > 0) != (rows[j].LiveCount > 0) {
			return rows[i].LiveCount > 0
		}
		if rows[i].Title != rows[j].Title {
			return rows[i].Title < rows[j].Title
		}
		return rows[i].ID < rows[j].ID
	})

	data["Rows"] = rows
	data["Summary"] = summary
	data["Truncated"] = res.Total > len(res.Beads)
	data["Limit"] = formulaListLimit
	s.render(w, r, "formulas.html", data)
}

// formulaCatalogSummary is the top-of-page counts bar for /formulas.
type formulaCatalogSummary struct {
	Formulas      int // recipes shown (after filters)
	WithLive      int // recipes with ≥1 open molecule
	LiveMolecules int // total open molecules across shown recipes
}

// moleculeRollupByFormula groups open type=molecule beads by their formula_id
// field and returns a map keyed by formula bead id. Best-effort: a daemon error
// yields an empty map so the catalog still renders (with zero live counts).
//
// One daemon query (open molecules, optionally project-scoped) drives every
// row's rollup — the join is done in-process by reading each molecule's
// formula_id field, so the catalog never issues a per-recipe round-trip.
func (s *Server) moleculeRollupByFormula(ctx context.Context, projectFilter string) map[string][]*beadsapi.BeadDetail {
	q := beadsapi.ListBeadsQuery{
		Types:    []string{"molecule"},
		Statuses: []string{"open", "in_progress"},
		Limit:    moleculeRollupLimit,
	}
	if projectFilter != "" {
		q.Labels = []string{"project:" + projectFilter}
	}
	res, err := s.daemon.ListBeadsFiltered(ctx, q)
	if err != nil {
		s.logger.Warn("listing molecules for formula rollup", "error", err)
		return map[string][]*beadsapi.BeadDetail{}
	}
	out := make(map[string][]*beadsapi.BeadDetail)
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		fid := strings.TrimSpace(b.Fields["formula_id"])
		if fid == "" {
			continue
		}
		out[fid] = append(out[fid], b)
	}
	return out
}

// buildFormulaRow projects a formula bead + its live molecules into a catalog
// row. Step and var counts come from the formula's `steps` / `vars` fields
// (stored as JSON arrays); formula_type defaults to "workflow" when unset,
// matching kd formula semantics.
func buildFormulaRow(b *beadsapi.BeadDetail, molecules []*beadsapi.BeadDetail) formulaRow {
	row := formulaRow{
		ID:          b.ID,
		Title:       b.Title,
		Project:     projectFromLabels(b.Labels),
		FormulaType: pickNonEmpty(b.Fields["formula_type"], "workflow"),
		Phase:       strings.TrimSpace(b.Fields["phase"]),
		StepCount:   countJSONArray(b.Fields["steps"]),
		VarCount:    countJSONArray(b.Fields["vars"]),
	}
	row.LiveCount = len(molecules)
	if row.LiveCount == 0 {
		return row
	}

	// Stable pip order: in_progress first (work happening now), then open, then
	// by id so the strip is deterministic across renders.
	sorted := make([]*beadsapi.BeadDetail, len(molecules))
	copy(sorted, molecules)
	sort.SliceStable(sorted, func(i, j int) bool {
		ri, rj := moleculeStatusRank(sorted[i].Status), moleculeStatusRank(sorted[j].Status)
		if ri != rj {
			return ri < rj
		}
		return sorted[i].ID < sorted[j].ID
	})

	statusCounts := map[string]int{}
	for i, m := range sorted {
		statusCounts[m.Status]++
		if i < formulaStatusPipCap {
			row.Pips = append(row.Pips, formulaStatusPip{
				MoleculeID: m.ID,
				Status:     m.Status,
				Symbol:     moleculeStatusSymbol(m.Status),
			})
		}
	}
	if row.LiveCount > len(row.Pips) {
		row.PipOverflow = row.LiveCount - len(row.Pips)
		// Only when the strip is truncated does the status tally add signal the
		// visible pips don't already carry — it summarizes the whole live set,
		// including the +N molecules the strip couldn't render. On an
		// un-truncated row every molecule has its own colour-coded pip, so the
		// tally would just duplicate them (and the N-live count) — drop it.
		row.Frontier = formulaFrontierSummary(statusCounts)
	}
	return row
}

// moleculeStatusRank orders molecule statuses for the pip strip: work-in-flight
// before merely-open before everything else.
func moleculeStatusRank(status string) int {
	switch status {
	case "in_progress":
		return 0
	case "open":
		return 1
	case "deferred":
		return 2
	default:
		return 3
	}
}

// moleculeStatusSymbol maps a molecule's bead status to a compact glyph for the
// status-pip strip. The glyphs are text so they render in every skin without an
// icon font.
func moleculeStatusSymbol(status string) string {
	switch status {
	case "in_progress":
		return "▶"
	case "open":
		return "○"
	case "deferred":
		return "⏸"
	case "closed":
		return "●"
	default:
		return "·"
	}
}

// formulaFrontierSummary renders a compact status breakdown of the live
// instances from a status→count tally, e.g. "3 open · 1 in progress", ordered
// by the pip rank so in-flight work reads first. This is a status TALLY, not a
// per-step "where" line — surfacing which recipe step each live molecule sits
// on would need a per-molecule execution join (buildMoleculeExecView),
// prohibitively expensive at catalog scale. buildFormulaRow therefore only
// renders this when the pip strip is truncated, so it summarizes the molecules
// the strip could not show rather than restating the visible pips.
func formulaFrontierSummary(counts map[string]int) string {
	order := []struct {
		status string
		label  string
	}{
		{"in_progress", "in progress"},
		{"open", "open"},
		{"deferred", "deferred"},
	}
	parts := make([]string, 0, len(counts))
	seen := map[string]bool{}
	for _, o := range order {
		if n := counts[o.status]; n > 0 {
			parts = append(parts, itoa(n)+" "+o.label)
			seen[o.status] = true
		}
	}
	// Surface any non-standard statuses so the summary never silently drops a
	// live instance from its count.
	extras := make([]string, 0)
	for status, n := range counts {
		if seen[status] || n == 0 {
			continue
		}
		extras = append(extras, itoa(n)+" "+status)
	}
	sort.Strings(extras)
	parts = append(parts, extras...)
	return strings.Join(parts, " · ")
}

// formulaTypeOptions builds the formula_type filter dropdown options with the
// current selection marked.
func formulaTypeOptions(selected string) []formulaTypeOption {
	defs := []struct{ value, label string }{
		{"", "All types"},
		{"workflow", "Workflow"},
		{"expansion", "Expansion"},
		{"aspect", "Aspect"},
	}
	out := make([]formulaTypeOption, 0, len(defs))
	for _, d := range defs {
		out = append(out, formulaTypeOption{
			Value:    d.value,
			Label:    d.label,
			Selected: d.value == selected,
		})
	}
	return out
}
