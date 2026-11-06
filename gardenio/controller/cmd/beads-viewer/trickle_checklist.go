package main

import (
	"encoding/json"
	"sort"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// The trickle-MR checklist is the beads-viewer projection of the R3 quality
// affordances catalog + trickle-eligibility bar (bundle ka-0000005053 §5/§7,
// master epic ka-0000005560). It renders, on a type=merge-request bead-show
// page, the ordered HARD/ADVISORY bar a monorepo-dark MR must clear before it
// is eligible to trickle upstream — as a green/red checkbox grid — plus a
// live "merge status" group and the ancestry links back to the dark MR(s) and
// the originating ticket.
//
// HONEST FRAMING (bundle §5/§6): the enforcing gate — `gb mr trickle-gate <id>
// --json` returning `{Eligible, Reason, Checklist []GateItem}`, modeled 1:1 on
// `gb mr relevance-gate` (bridge.EvaluateTrickleEligibility, task ka-0000004967)
// — NOW SHIPS and writes its structured output into the `trickle_checklist`
// bead field via `--write`. This view is the *rendering* half: it reads that
// field and, when absent (the gate has not run against this MR yet), renders
// the canonical bar with every item PENDING so operators can see the bar shape.
// What does not exist yet is the dark CI pipeline that would run the gate
// automatically. The live merge-status group is derived from the real
// gitlab-bridge MR fields, so the section is useful on genuine MR beads today.
//
// The ancestry constellation (parent/child/blocks + typed-mention edges) is
// already rendered by buildConstellation; this checklist is its textual /
// linked companion, spelling out "peeled from ticket X · dark MR Y" beneath
// the graph.

// trickleGateItemKind classifies a bar item as blocking (HARD) or
// surfacing-only (ADVISORY). Mirrors the catalog's per-affordance
// classification in §2.
const (
	trickleHard     = "HARD"
	trickleAdvisory = "ADVISORY"
)

// trickleItemStatus is the evaluated state of one bar item.
//
//   - pass:    the affordance's gate is green (or the item is N/A, which does
//     not block eligibility).
//   - fail:    the gate is red — a HARD fail blocks trickle-eligibility.
//   - pending: no evidence yet — the trickle-gate has not run against this MR.
//     The default for every item until the gate lands.
//   - na:      the affordance does not apply to this MR (e.g. the perf gate is
//     glob-triggered and no perf-sensitive path was touched). Does NOT block.
const (
	trickleStatusPass    = "pass"
	trickleStatusFail    = "fail"
	trickleStatusPending = "pending"
	trickleStatusNA      = "na"
)

// trickleGateItem is one row of the trickle bar. It intentionally mirrors the
// `GateItem{ID, Kind, Pass, Evidence}` shape §5 specifies for
// `EvaluateTrickleEligibility`'s Checklist so the future gate's --json output
// maps onto it 1:1 — plus a static Label/Note the renderer owns.
type trickleGateItem struct {
	// ID is the stable slug the gate keys evidence by ("code-review",
	// "coverage", …). Matched case-insensitively against the JSON checklist.
	ID string
	// Label is the human-facing row title.
	Label string
	// Kind is HARD | ADVISORY.
	Kind string
	// Status is pass | fail | pending | na (see the trickleStatus* consts).
	Status string
	// Evidence is the gate-supplied links/output backing the status. Empty
	// until the gate runs.
	Evidence string
	// Note is a one-line description of what the affordance checks — static,
	// renderer-owned, so the bar is legible even with no gate evidence.
	Note string
}

// Glyph returns the checkbox glyph for the item's status: ✓ pass, ✗ fail,
// ☐ pending, – na. Template-facing.
func (it trickleGateItem) Glyph() string {
	switch it.Status {
	case trickleStatusPass:
		return "✓"
	case trickleStatusFail:
		return "✗"
	case trickleStatusNA:
		return "–"
	default:
		return "☐"
	}
}

// StatusLabel is the short human word for the status, used in the row and for
// the aria-label so the grid is screen-reader legible.
func (it trickleGateItem) StatusLabel() string {
	switch it.Status {
	case trickleStatusPass:
		return "pass"
	case trickleStatusFail:
		return "fail"
	case trickleStatusNA:
		return "n/a"
	default:
		return "pending"
	}
}

// trickleMergeStatus is the live "merge status" group — the real gitlab-bridge
// MR fields (mr_state / mr_pipeline_status / mr_approved) that make the section
// useful on a genuine MR bead before the trickle-gate exists.
type trickleMergeStatus struct {
	State         string // opened | merged | closed | draft (raw mr_state)
	Pipeline      string // success | failed | running | "" (mr_pipeline_status)
	PipelineURL   string
	Approved      bool
	ApprovedKnown bool // false when the mr_approved field is absent
	HasConflicts  bool
}

// Any reports whether the merge-status group carries anything worth rendering.
func (m trickleMergeStatus) Any() bool {
	return m.State != "" || m.Pipeline != "" || m.ApprovedKnown || m.HasConflicts
}

// trickleAncestryLink is one ancestry pointer — the originating ticket, or a
// dark MR the peel was validated against. External=true when Href is an
// off-viewer GitLab URL (dark MR) rather than a bead permalink.
type trickleAncestryLink struct {
	Label    string
	Href     string
	Text     string
	External bool
}

// trickleChecklistView is the full section view-model. nil (section hidden)
// for any non-merge-request bead, so no other view is affected (AC-3).
type trickleChecklistView struct {
	BeadID   string
	BasePath string

	// HardItems / Advisory are the ordered bar, split by kind so the template
	// can render HARD (blocking) rows above the ADVISORY (surfacing) rows.
	HardItems []trickleGateItem
	Advisory  []trickleGateItem

	// GateRan is true when structured trickle-gate output was present on the
	// bead. When false the bar renders all-pending and the eligibility verdict
	// is suppressed (we don't claim "blocked" just because the gate is absent).
	GateRan bool
	// Eligible / Reason are the gate verdict (only meaningful when GateRan).
	Eligible bool
	Reason   string
	// HardPass / HardTotal drive the "N of M HARD checks passing" summary.
	HardPass  int
	HardTotal int

	// Merge is the live merge-status group.
	Merge trickleMergeStatus

	// Ancestry links back to the dark MR(s) and the originating ticket.
	Ancestry []trickleAncestryLink
}

// HasAncestry reports whether any ancestry link resolved.
func (v *trickleChecklistView) HasAncestry() bool { return len(v.Ancestry) > 0 }

// trickleGateJSON is the on-bead structured form the future `gb mr
// trickle-gate --json` writes into the `trickle_checklist` field. Field names
// mirror §5's decision struct exactly.
type trickleGateJSON struct {
	Eligible  bool                  `json:"eligible"`
	Reason    string                `json:"reason"`
	Checklist []trickleGateJSONItem `json:"checklist"`
}

type trickleGateJSONItem struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Pass is a tri-state: nil = pending/unevaluated, so absence is distinct
	// from an explicit false (a real fail). Mirrors the coverage `.bypass`
	// omitempty contract §2.2 relies on.
	Pass     *bool  `json:"pass"`
	NA       bool   `json:"na"`
	Evidence string `json:"evidence"`
}

// trickleBarSpec is the canonical, ordered HARD/ADVISORY bar from catalog §2.
// The renderer owns Label/Kind/Note; the gate supplies per-item Status/Evidence
// keyed by ID. Kept as a function (not a package var) so each call gets a fresh,
// mutable copy — the builder stamps Status onto these items in place. Every item
// starts pending (a concrete status the template's CSS classes need) and stays
// pending until the gate supplies a verdict.
func trickleBarSpec() []trickleGateItem {
	spec := []trickleGateItem{
		{ID: "code-review", Label: "Automated Claude review", Kind: trickleHard,
			Note: "/code-review --effort high; every ≥80-confidence finding resolved or dismissed (§2.1)"},
		{ID: "coverage", Label: "Tests + patch coverage", Kind: trickleHard,
			Note: "patch-cov-result.json .pass && no .bypass; e2e summary clean (§2.2)"},
		{ID: "security", Label: "Security review", Kind: trickleHard,
			Note: "gitleaks + govulncheck (new reachable crit/high) + cargo audit/deny; findings triaged (§2.3)"},
		{ID: "perf", Label: "Performance (call-count gate)", Kind: trickleHard,
			Note: "deterministic daemon-call-count guard; glob-triggered on viewer/store paths (§2.4)"},
		{ID: "small-diff", Label: "Small-diff / single concern", Kind: trickleHard,
			Note: "≤400 net LOC, ≤10 files, 1 concern; standalone-build-green on upstream HEAD (§2.5)"},
		{ID: "rationale", Label: "Human-readable rationale", Kind: trickleHard,
			Note: "trust-dossier sections present & non-trivial (Why ≠ Summary) (§2.6)"},
		{ID: "provenance", Label: "Provenance / linked beads", Kind: trickleHard,
			Note: "mr_url + Operator: trailer + typed edge to dark MR + per-leg run IDs (§2.7)"},
		{ID: "perf-benchmark", Label: "Benchmark (ns/op)", Kind: trickleAdvisory,
			Note: "bench.txt compiles; no benchstat threshold until a main baseline exists (§2.4)"},
		{ID: "rationale-depth", Label: "Rationale depth", Kind: trickleAdvisory,
			Note: "qualitative prose quality — surfaced, human-adjudicated (§2.6)"},
		{ID: "test-quality", Label: "Test meaningfulness", Kind: trickleAdvisory,
			Note: "assert-density + diff-scoped mutation testing — DEFERRED, pilot-gated (§2.2)"},
	}
	for i := range spec {
		spec[i].Status = trickleStatusPending
	}
	return spec
}

// buildTrickleChecklist assembles the trickle-MR checklist view for a bead.
// Returns nil for any bead that is not a type=merge-request, so the section is
// MR-only and can never break a non-MR page (AC-3).
//
// deps are the bead's forward dependencies (this MR --produces/parent-->), used
// to resolve the originating ticket when no explicit field points at it.
func buildTrickleChecklist(bead *beadsapi.BeadDetail, deps []beadsapi.Dependency, basePath string) *trickleChecklistView {
	if bead == nil || bead.Type != "merge-request" {
		return nil
	}
	f := bead.Fields
	items := trickleBarSpec()

	gate, gateRan := parseTrickleGate(f["trickle_checklist"])
	if gateRan {
		// Capture the return: applyTrickleGate may APPEND unknown gate items, so
		// the reassignment is load-bearing (dropping it would silently discard a
		// check the gate grew that the canonical bar doesn't know).
		items = applyTrickleGate(items, gate)
	}

	v := &trickleChecklistView{
		BeadID:   bead.ID,
		BasePath: basePath,
		GateRan:  gateRan,
		Eligible: gateRan && gate.Eligible,
		Reason:   strings.TrimSpace(gate.Reason),
		Merge:    trickleMergeStatusFromFields(f),
		Ancestry: trickleAncestryFromBead(bead, deps, basePath),
	}
	for _, it := range items {
		if it.Kind == trickleHard {
			v.HardTotal++
			// N/A does not block, so it counts toward "passing".
			if it.Status == trickleStatusPass || it.Status == trickleStatusNA {
				v.HardPass++
			}
			v.HardItems = append(v.HardItems, it)
		} else {
			v.Advisory = append(v.Advisory, it)
		}
	}
	return v
}

// parseTrickleGate decodes the `trickle_checklist` field. The bool return is
// false (and the zero decision returned) when the field is absent or unparsable
// — the builder then renders the bar all-pending rather than fabricating a
// verdict from noise.
func parseTrickleGate(raw string) (trickleGateJSON, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return trickleGateJSON{}, false
	}
	var g trickleGateJSON
	if err := json.Unmarshal([]byte(raw), &g); err != nil {
		return trickleGateJSON{}, false
	}
	return g, true
}

// applyTrickleGate stamps Status/Evidence from the gate's checklist onto the
// canonical bar items (matched case-insensitively by ID). Any gate item whose
// ID is not in the canonical bar is appended as a HARD row so a gate that grows
// new checks still surfaces them rather than dropping them silently.
func applyTrickleGate(items []trickleGateItem, g trickleGateJSON) []trickleGateItem {
	byID := make(map[string]int, len(items))
	for i := range items {
		byID[strings.ToLower(items[i].ID)] = i
	}
	for _, gi := range g.Checklist {
		status := trickleStatusPending
		switch {
		case gi.NA:
			status = trickleStatusNA
		case gi.Pass != nil && *gi.Pass:
			status = trickleStatusPass
		case gi.Pass != nil && !*gi.Pass:
			status = trickleStatusFail
		}
		if idx, ok := byID[strings.ToLower(strings.TrimSpace(gi.ID))]; ok {
			items[idx].Status = status
			items[idx].Evidence = strings.TrimSpace(gi.Evidence)
			continue
		}
		// Unknown item id — the gate added a check the renderer doesn't know.
		// Surface it (HARD by default unless the gate marked it advisory) so a
		// growing gate is never silently truncated.
		kind := trickleHard
		if strings.EqualFold(strings.TrimSpace(gi.Kind), trickleAdvisory) {
			kind = trickleAdvisory
		}
		label := strings.TrimSpace(gi.ID)
		if label == "" {
			label = "(unnamed check)"
		}
		items = append(items, trickleGateItem{
			ID: gi.ID, Label: label, Kind: kind, Status: status,
			Evidence: strings.TrimSpace(gi.Evidence),
		})
	}
	return items
}

// trickleMergeStatusFromFields reads the live gitlab-bridge MR fields into the
// merge-status group. Field aliases mirror mrFieldString / type_mr.html so the
// checklist agrees with the rest of the viewer on which fields count.
func trickleMergeStatusFromFields(f map[string]string) trickleMergeStatus {
	m := trickleMergeStatus{
		State:       mrFieldString(f, "mr_state", "state"),
		Pipeline:    mrFieldString(f, "mr_pipeline_status", "pipeline_status", "ci_status"),
		PipelineURL: mrFieldString(f, "mr_pipeline_url", "pipeline_url"),
	}
	// A draft MR reports mr_state=opened + a draft flag; surface "draft" as the
	// state so the operator reads the true posture.
	if truthyField(f, "mr_draft", "draft") && (m.State == "" || m.State == "opened") {
		m.State = "draft"
	}
	if v, ok := boolField(f, "mr_approved", "approved"); ok {
		m.Approved = v
		m.ApprovedKnown = true
	}
	m.HasConflicts = truthyField(f, "mr_has_conflicts", "has_conflicts")
	return m
}

// trickleAncestryFromBead resolves the ancestry links: the originating ticket
// (the peel target) and the dark MR(s) the peel was validated against. Explicit
// fields (populated by the future Dark-MR:/Peel-Of: trailer ingestion, §7 #7)
// win; the originating ticket falls back to the MR's parent-child forward dep so
// the link resolves on real MR beads today.
func trickleAncestryFromBead(bead *beadsapi.BeadDetail, deps []beadsapi.Dependency, basePath string) []trickleAncestryLink {
	f := bead.Fields
	var out []trickleAncestryLink
	seen := map[string]struct{}{}
	add := func(l trickleAncestryLink) {
		key := l.Label + "|" + l.Href
		if _, dup := seen[key]; dup || l.Href == "" {
			return
		}
		seen[key] = struct{}{}
		out = append(out, l)
	}

	// Originating ticket: explicit field, else the first parent-child forward dep.
	ticket := firstNonEmpty(f, "originating_ticket", "peel_of", "peel_of_bead", "source_ticket")
	if ticket == "" {
		// Stable order: parent-child edges first, then any other forward dep.
		sorted := append([]beadsapi.Dependency(nil), deps...)
		sort.SliceStable(sorted, func(i, j int) bool {
			return depTypeRank(sorted[i].Type) < depTypeRank(sorted[j].Type)
		})
		for _, d := range sorted {
			if d.Type == "parent-child" && d.DependsOnID != "" {
				ticket = d.DependsOnID
				break
			}
		}
	}
	if ticket != "" {
		add(trickleAncestryLink{Label: "originating ticket", Href: basePath + "/" + ticket, Text: ticket})
	}

	// Dark MR(s): explicit URL field (external GitLab link on the dark instance)
	// and/or a dark-MR bead id.
	if u := firstNonEmpty(f, "dark_mr_url", "dark_mr"); u != "" {
		if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
			add(trickleAncestryLink{Label: "dark MR", Href: u, Text: shortenMRURL(u), External: true})
		} else {
			add(trickleAncestryLink{Label: "dark MR", Href: basePath + "/" + u, Text: u})
		}
	}
	if id := strings.TrimSpace(f["dark_mr_bead"]); id != "" {
		add(trickleAncestryLink{Label: "dark MR", Href: basePath + "/" + id, Text: id})
	}
	return out
}

// depTypeRank orders forward deps so parent-child edges are considered first
// when resolving the originating ticket.
func depTypeRank(t string) int {
	if t == "parent-child" {
		return 0
	}
	return 1
}

// shortenMRURL renders a GitLab MR URL as "<project>!<iid>" when it matches the
// canonical /-/merge_requests/<iid> shape, else returns the URL unchanged.
func shortenMRURL(u string) string {
	const marker = "/-/merge_requests/"
	prefix, iidPart, found := strings.Cut(u, marker)
	if !found {
		return u
	}
	iid := strings.TrimRight(iidPart, "/")
	proj := strings.TrimPrefix(prefix, "https://")
	proj = strings.TrimPrefix(proj, "http://")
	if _, rest, ok := strings.Cut(proj, "/"); ok {
		proj = rest // drop the host
	}
	if iid == "" || proj == "" {
		return u
	}
	return proj + "!" + iid
}

// truthyField reports whether any of keys holds a truthy string ("true"/"1"/
// "yes"). Used for boolean MR flags stored as strings.
func truthyField(f map[string]string, keys ...string) bool {
	v, ok := boolField(f, keys...)
	return ok && v
}

// boolField parses the first present key as a bool. The second return is false
// when no key is present (distinguishing "absent" from "explicitly false").
func boolField(f map[string]string, keys ...string) (bool, bool) {
	for _, k := range keys {
		if raw, ok := f[k]; ok {
			switch strings.ToLower(strings.TrimSpace(raw)) {
			case "true", "1", "yes":
				return true, true
			case "false", "0", "no", "":
				return false, true
			}
		}
	}
	return false, false
}
