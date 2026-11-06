package advice

// overconstraint_baseline.go adds a deterministic, plateau-aware baseline diff to
// the over-constraint directive-density findings. It is the LIVE-fleet analog of
// the frozen-baseline template budget tests (gardener_prompt_over_constraint_budget_test.go):
// where that test freezes the directive count of the *authoring template* new
// gardeners inherit, this lets the nightly over-constraint watch (ka-0000000135)
// compute a tool-derived regression signal over the *live, hand-edited* trigger
// prompt beads the template guard structurally cannot see.
//
// Why this is needed (audit finding D1, gardenio/docs/audits/2026-07-31-differential/):
// the watch prompt asked each night's agent to eyeball a night-over-night delta
// against `kd show <trigger> | jq '.fields.run_log'` — but a plain spawn_agent
// schedule trigger has NO run_log field, so several nights spuriously declared
// "first run / no baseline" and did no diff at all. And even a working
// night-over-night diff is blind to plateau-after-drift: ka-0000001510 climbed
// 13 -> 16 -> 17 directives (its D1 remediation floor was 10), then plateaued —
// and now reads "flat" every night, sitting +7 above its floor, never
// re-surfacing. A persisted, ratcheting baseline fixes both: the floor only ever
// moves DOWN (toward a better-remediated state), so a climb stays flagged as a
// regression until the bead is actually re-remediated below the recorded floor.

import (
	"encoding/json"
	"sort"
)

// OverConstraintBaseline is the persisted per-surface directive-count floor the
// diff compares against. It is a stable, human-diffable JSON snapshot: one entry
// per (bead id, field) directive-density finding, carrying the lowest directive
// count ever observed for that surface. Keyed by BaselineKey.
type OverConstraintBaseline struct {
	// Version is a forward-compat marker for the on-disk format.
	Version int `json:"version"`
	// Entries is the per-surface floor, sorted by key for a stable diff.
	Entries []OverConstraintBaselineEntry `json:"entries"`
}

// OverConstraintBaselineEntry is one surface's recorded floor.
type OverConstraintBaselineEntry struct {
	ID    string `json:"id"`
	Field string `json:"field,omitempty"`
	Title string `json:"title,omitempty"`
	// DirectiveCount is the lowest emphatic-directive count observed for this
	// surface across the runs that contributed to the baseline (the ratchet
	// floor). A live surface exceeding this is a regression.
	DirectiveCount int `json:"directive_count"`
}

// baselineFormatVersion is the current on-disk baseline format version.
const baselineFormatVersion = 1

// BaselineDelta classifies a live finding against the persisted baseline.
type BaselineDelta string

const (
	// BaselineNew marks a surface that has no baseline entry — it newly entered
	// the WARN set (or the baseline predates it).
	BaselineNew BaselineDelta = "new"
	// BaselineClimbed marks a surface whose directive count rose above its
	// recorded floor — a real regression that must stay flagged until
	// re-remediated below the floor.
	BaselineClimbed BaselineDelta = "climbed"
	// BaselineSame marks a surface at or below its recorded floor — a
	// steady-state WARN, not a regression.
	BaselineSame BaselineDelta = "same"
)

// baselineKey is the stable identity of a scanned surface for baseline lookup:
// its bead id plus field (a trigger bead may expose several instruction
// surfaces; advice beads have an empty field).
func baselineKey(id, field string) string {
	return id + "\x00" + field
}

// LoadOverConstraintBaseline parses a baseline JSON blob. An empty input yields
// a zero baseline (every finding reads as new), so a first run with no prior
// file degrades gracefully instead of erroring — the same fail-open posture the
// redundancy check takes with an absent ref corpus.
func LoadOverConstraintBaseline(data []byte) (OverConstraintBaseline, error) {
	var b OverConstraintBaseline
	if len(data) == 0 {
		return b, nil
	}
	if err := json.Unmarshal(data, &b); err != nil {
		return OverConstraintBaseline{}, err
	}
	return b, nil
}

// floorMap indexes a baseline's entries by key for O(1) lookup.
func (b OverConstraintBaseline) floorMap() map[string]int {
	m := make(map[string]int, len(b.Entries))
	for _, e := range b.Entries {
		m[baselineKey(e.ID, e.Field)] = e.DirectiveCount
	}
	return m
}

// DiffOverConstraintBaseline classifies each directive-density finding against
// the baseline floor and returns the findings with their BaselineDelta and the
// prior floor filled in. Non-directive-density findings (rules-over-judgment,
// redundancy) are passed through unclassified (BaselineDelta stays "") — the
// baseline tracks the one metric that has a monotone floor. Input order is
// preserved; callers that want worst-first should Sort first.
func DiffOverConstraintBaseline(findings []OverConstraintFinding, base OverConstraintBaseline) []OverConstraintFinding {
	floor := base.floorMap()
	out := make([]OverConstraintFinding, len(findings))
	for i, f := range findings {
		out[i] = f
		if f.Check != OverConstraintDirectiveDensity {
			continue
		}
		prev, ok := floor[baselineKey(f.ID, f.Field)]
		switch {
		case !ok:
			out[i].BaselineDelta = BaselineNew
		case f.DirectiveCount > prev:
			out[i].BaselineDelta = BaselineClimbed
			out[i].BaselineFloor = prev
		default:
			out[i].BaselineDelta = BaselineSame
			out[i].BaselineFloor = prev
		}
	}
	return out
}

// RatchetOverConstraintBaseline computes the NEXT baseline from the current
// directive-density findings and the prior baseline. The floor for each surface
// is the MINIMUM of its current count and any prior floor — the ratchet only
// ever moves DOWN, toward a better-remediated state. This is what makes a climb
// stay flagged: re-observing a surface at 17 when its floor is 10 does not raise
// the floor to 17, so it keeps reading "climbed" until an edit actually drops it
// below 10.
//
// A surface present in the prior baseline but ABSENT from the current findings
// (it fell out of the WARN set — e.g. was re-remediated so far it no longer
// trips the density check) is DROPPED from the next baseline: its floor is no
// longer meaningful, and keeping it would make a future re-entry read "same"
// against a stale low floor instead of "new". Prior entries are retained only
// while the surface is still a live finding.
func RatchetOverConstraintBaseline(findings []OverConstraintFinding, prior OverConstraintBaseline) OverConstraintBaseline {
	priorFloor := prior.floorMap()

	// best accumulates the ratchet floor for each surface: the minimum of every
	// current observation for it and any prior floor. Keyed by baselineKey.
	best := map[string]*OverConstraintBaselineEntry{}
	for _, f := range findings {
		if f.Check != OverConstraintDirectiveDensity {
			continue
		}
		key := baselineKey(f.ID, f.Field)
		count := f.DirectiveCount
		if pf, ok := priorFloor[key]; ok && pf < count {
			count = pf // ratchet down toward the better-remediated prior floor.
		}
		if cur, ok := best[key]; ok {
			if count < cur.DirectiveCount {
				cur.DirectiveCount = count
			}
			continue
		}
		best[key] = &OverConstraintBaselineEntry{
			ID: f.ID, Field: f.Field, Title: f.Title, DirectiveCount: count,
		}
	}

	next := OverConstraintBaseline{Version: baselineFormatVersion}
	for _, e := range best {
		next.Entries = append(next.Entries, *e)
	}
	sort.Slice(next.Entries, func(i, j int) bool {
		if next.Entries[i].ID != next.Entries[j].ID {
			return next.Entries[i].ID < next.Entries[j].ID
		}
		return next.Entries[i].Field < next.Entries[j].Field
	})
	return next
}

// MarshalOverConstraintBaseline renders a baseline as stable, indented JSON
// (entries already sorted by RatchetOverConstraintBaseline) for a clean
// file-over-file diff.
func MarshalOverConstraintBaseline(b OverConstraintBaseline) ([]byte, error) {
	return json.MarshalIndent(b, "", "  ")
}

// HasBaselineRegression reports whether any finding is a new or climbed
// directive-density regression against the baseline — the gate condition for
// --strict --baseline.
func HasBaselineRegression(findings []OverConstraintFinding) bool {
	for _, f := range findings {
		if f.BaselineDelta == BaselineNew || f.BaselineDelta == BaselineClimbed {
			return true
		}
	}
	return false
}
