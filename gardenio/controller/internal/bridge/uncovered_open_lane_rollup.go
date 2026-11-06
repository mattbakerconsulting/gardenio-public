// Engine-side uncovered-open-lane ROLL-UP report (bug ka-0000002082 fix 4).
//
// WHAT THIS COVERS. SweepClosedEnabledDrain (closed_enabled_drain.go) drains the
// low-value rows out of the closed+enabled dark-trigger pool — closed-epic
// lineages (fix 1), engine-retired terminate-marked triggers (fix 3), and the
// unanchored ambiguous cohort via one grouped decision (fix 2). It DELIBERATELY
// leaves one class untouched (see that file's "out of scope"): a closed+enabled
// trigger anchored to an OPEN gardener-epic that still has live children and no
// engine terminate marker. Those are GENUINELY-UNCOVERED lanes — a gardener lane
// whose trigger went dark while the work it drives is still open. Auto-archiving
// them would be actively harmful (it would bury still-live work), and per-trigger
// revival decisions re-created the exact file-faster-than-resolved backlog the
// bug is about (measured 2026-07-29: ~4 filed/day vs ~1 resolved/day).
//
// THE FIX IS ONE SIGNAL, NOT MANY DECISIONS. Rather than one operator decision
// per uncovered lane, this sweep maintains ONE LIVING roll-up doc that lists the
// whole uncovered cohort — open-child count, last child activity, anchor epic —
// refreshed on a daily cadence and updated IN PLACE (never a fresh doc per run,
// which would recreate the spam in doc form). It is decoupled from the archive
// queue: it files NO decisions and mutates NO triggers. It is a pure read +
// one idempotent doc upsert, so it is safe to run default-on. The reviver's
// scarce 1/cycle human-decision budget is then spent reading ONE report instead
// of triaging the cohort one trigger at a time.
//
// GENUINELY-UNCOVERED, precisely (the classifier below):
//   - trigger is closed + enabled + non-manual + not an E2E fixture, and
//   - carries NO gardener_terminated_kind (an engine-retired bead is a CORRECT
//     retirement that SweepClosedEnabledDrain fix 3 archives — never uncovered), and
//   - carries a gardener-epic:<E> label where E is a real type==epic (the survey
//     false positive ka-0000006579 was a gardener-epic label pointing at a
//     type=trigger, not an epic — assert type==epic or it re-surfaces), and
//   - E is OPEN (a CLOSED anchor is fix 1's retired-lineage territory), and
//   - E still has >0 open/in_progress children (a drained epic never fires its
//     cron pre-flight — it is correctly dark, not uncovered), and
//   - NO live successor gardener covers E (no OTHER open+enabled trigger anchored
//     to E — that lane already has a live handle).
//
// DISCIPLINE (mirrors the sibling sweeps). Self-throttled to once per day (a
// report, not an urgent mutation), pause-aware (skips under gardeners/triggers
// pause), row-capped with a loud log on breach (advice ka-0000003555 — a
// truncated report must never read as "fewer uncovered lanes"), and a no-op
// unless uncoveredOpenLaneRollupEnabled.

package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Uncovered-open-lane roll-up tunables.
const (
	// uncoveredOpenLaneRollupRosterLimit bounds the closed+enabled candidate
	// roster pulled per run. Same pool as the drain sweep (~137 today); this cap
	// only guards a runaway query and a breach is logged loudly rather than
	// silently truncated (advice ka-0000003555).
	uncoveredOpenLaneRollupRosterLimit = 1000

	// uncoveredOpenLaneRollupRowCap bounds how many uncovered lanes the report
	// renders. Far above the measured cohort (17–30); a breach means the report
	// is truncated and is logged as WARN so a big cohort never reads as small.
	uncoveredOpenLaneRollupRowCap = 200

	// uncoveredOpenLaneRollupThrottle is the minimum wall-clock between report
	// refreshes. This is a REPORT, not an urgent mutation: a daily cadence keeps
	// the living doc fresh without churning its updated_at every Evaluate cycle.
	uncoveredOpenLaneRollupThrottle = 24 * time.Hour

	// uncoveredOpenLaneRollupMarkerLabel tags the living roll-up doc so the sweep
	// finds and updates its own single report in place (never a fresh doc/run).
	uncoveredOpenLaneRollupMarkerLabel = "uncovered-open-lane-rollup"

	// uncoveredOpenLaneRollupActor is the synthetic author stamped on the report
	// doc, mirroring the sibling sweeps' CreatedBy.
	uncoveredOpenLaneRollupActor = "uncovered-open-lane-rollup-sweep"

	// uncoveredOpenLaneRollupSystemOperator is the X-System-Operator source label
	// stamped on the doc create/update so the daemon attributes this TRUE
	// machine-origin cron write instead of fail-closing on "operator could not be
	// resolved" (ka-0000007257) — same mechanism the drain sweep relies on.
	uncoveredOpenLaneRollupSystemOperator = "uncovered-open-lane-rollup"

	// uncoveredOpenLaneRollupTitle is the STABLE title of the living report. The
	// as-of timestamp and cohort count live in the body, so the title never
	// changes and operators find the same doc every day.
	uncoveredOpenLaneRollupTitle = "trigger-reviver: uncovered-open-lane roll-up (living report, bug ka-0000002082 fix 4)"
)

// uncoveredLane is one row in the roll-up: a genuinely-uncovered lane and the
// evidence that made it qualify.
type uncoveredLane struct {
	triggerID    string
	triggerTitle string
	triggerMode  string
	epicID       string
	epicTitle    string
	openChildren int
	lastChildAt  time.Time
}

// uncoveredOpenLaneRollupState throttles the whole sweep to once per
// uncoveredOpenLaneRollupThrottle. The report is idempotent (a single living doc
// updated in place), so the only in-memory state needed is the last-run clock —
// same shape as the drain sweep's throttle.
type uncoveredOpenLaneRollupState struct {
	mu        sync.Mutex
	lastRunAt time.Time
}

func newUncoveredOpenLaneRollupState() *uncoveredOpenLaneRollupState {
	return &uncoveredOpenLaneRollupState{}
}

// dueForRun reports whether the throttle interval has elapsed since the last
// run, and if so records now as the new last-run time (so concurrent callers
// cannot both pass). A zero lastRunAt (first ever call) is always due.
func (s *uncoveredOpenLaneRollupState) dueForRun(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.lastRunAt.IsZero() && now.Sub(s.lastRunAt) < uncoveredOpenLaneRollupThrottle {
		return false
	}
	s.lastRunAt = now
	return true
}

// SweepUncoveredOpenLaneRollup maintains the living uncovered-open-lane roll-up
// report (bug ka-0000002082 fix 4). Once per throttle interval it classifies the
// closed+enabled trigger pool, keeps only the genuinely-uncovered open lanes (see
// the file header for the exact predicate), and upserts ONE living type=doc
// report listing them. It files no decisions and mutates no triggers.
//
// No-op unless uncoveredOpenLaneRollupEnabled. Skips entirely under a global
// triggers-pause / gardeners-pause. Errors are logged and downgraded so a daemon
// hiccup cannot wedge the engine.
func (e *TriggerEngine) SweepUncoveredOpenLaneRollup(ctx context.Context) {
	if !e.uncoveredOpenLaneRollupEnabled {
		return
	}
	now := e.now()
	if !e.uncoveredOpenLaneRollupState.dueForRun(now) {
		return
	}

	// A deliberate global pause means the operator is holding the fleet; even a
	// report doc write is suppressed under it, mirroring the drain sweep.
	if gardenersPauseEngaged(ctx, e.daemon, e.logger) {
		e.logger.Info("uncovered-open-lane-rollup: gardeners-pause engaged, skipping")
		return
	}
	if e.loadGateState(ctx).globallyPaused {
		e.logger.Info("uncovered-open-lane-rollup: triggers-pause engaged, skipping")
		return
	}

	// Attribute the doc create/update to the machine operator (same reasoning as
	// the drain sweep: a true machine-origin cron write with no human behind it
	// would otherwise 400 on the daemon's fail-closed audit-operator resolver).
	ctx = beadsapi.WithSystemOperator(ctx, uncoveredOpenLaneRollupSystemOperator)

	// The closed+enabled candidate pool via a server-side field filter.
	res, err := e.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"trigger"},
		Statuses: []string{"closed"},
		Fields:   map[string]string{"enabled": "true"},
		Limit:    uncoveredOpenLaneRollupRosterLimit,
	})
	if err != nil {
		e.logger.Warn("uncovered-open-lane-rollup: failed to load closed+enabled roster", "error", err)
		return
	}
	if res.Total > len(res.Beads) {
		// A silent truncation here would under-count the cohort and read as
		// "fewer uncovered lanes" (advice ka-0000003555). Make it loud.
		e.logger.Warn("uncovered-open-lane-rollup: roster truncated by limit — pool larger than one page",
			"returned", len(res.Beads), "total", res.Total, "limit", uncoveredOpenLaneRollupRosterLimit)
	}

	// A lane is covered if SOME open+enabled trigger anchors its epic — that lane
	// has a live successor handle. Build the covered-epic set in one query so the
	// per-candidate check is a map lookup, not an N+1.
	coveredEpics := e.liveGardenerEpics(ctx)

	// Classify. epicMeta memoizes the epic GetBead + open-child count per distinct
	// anchor so two candidates on the same epic cost one epic lookup + one child
	// count.
	epicMeta := make(map[string]uncoveredAnchorEpic)
	var lanes []uncoveredLane
	scanned := 0
	for _, bead := range res.Beads {
		if bead == nil {
			continue
		}
		scanned++
		// Defensive: never surface manual or E2E-fixture triggers (the reviver
		// and the drain sweep both skip them; a fixture is load-bearing, not dark).
		if bead.Fields["trigger_mode"] == "manual" || isE2EFixture(bead) {
			continue
		}
		// An engine terminate marker means the engine retired this gardener on an
		// author-declared stop condition — a CORRECT retirement that the drain
		// sweep (fix 3) archives regardless of anchor-epic status. Never uncovered.
		if strings.TrimSpace(bead.Fields["gardener_terminated_kind"]) != "" {
			continue
		}
		epicID := gardenerEpicLabel(bead)
		if epicID == "" {
			continue // unanchored → the drain sweep's grouped-decision cohort, not uncovered.
		}
		if coveredEpics[epicID] {
			continue // a live open+enabled successor already covers this lane.
		}
		meta, ok := epicMeta[epicID]
		if !ok {
			meta = e.classifyAnchorEpic(ctx, epicID)
			epicMeta[epicID] = meta
		}
		// Must be a real OPEN epic with live children. A closed anchor is fix 1's
		// retired-lineage territory; a drained (0 open children) epic is correctly
		// dark; a non-epic anchor is the ka-0000006579 false positive.
		if !meta.isOpenEpic || meta.openChildren == 0 {
			continue
		}
		lanes = append(lanes, uncoveredLane{
			triggerID:    bead.ID,
			triggerTitle: bead.Title,
			triggerMode:  bead.Fields["trigger_mode"],
			epicID:       epicID,
			epicTitle:    meta.title,
			openChildren: meta.openChildren,
			lastChildAt:  meta.lastChildAt,
		})
	}

	// Largest live-but-undriven lane first (the survey's emphasis), then oldest
	// child activity, then id — deterministic ordering across runs.
	sort.SliceStable(lanes, func(i, j int) bool {
		if lanes[i].openChildren != lanes[j].openChildren {
			return lanes[i].openChildren > lanes[j].openChildren
		}
		if !lanes[i].lastChildAt.Equal(lanes[j].lastChildAt) {
			return lanes[i].lastChildAt.Before(lanes[j].lastChildAt)
		}
		return lanes[i].triggerID < lanes[j].triggerID
	})

	total := len(lanes)
	truncated := false
	if len(lanes) > uncoveredOpenLaneRollupRowCap {
		e.logger.Warn("uncovered-open-lane-rollup: cohort exceeds row cap — report truncated",
			"cohort", total, "cap", uncoveredOpenLaneRollupRowCap)
		lanes = lanes[:uncoveredOpenLaneRollupRowCap]
		truncated = true
	}

	body := renderUncoveredOpenLaneRollup(lanes, total, scanned, truncated, now)
	docID, created, err := e.upsertUncoveredOpenLaneRollupDoc(ctx, body)
	if err != nil {
		e.logger.Warn("uncovered-open-lane-rollup: failed to upsert report doc", "error", err)
		return
	}

	e.logger.Info("uncovered-open-lane-rollup: complete",
		"pool", len(res.Beads),
		"uncovered_lanes", total,
		"report_doc", docID,
		"created", created)

	if e.uncoveredOpenLaneRollupNotify && e.notifier != nil && total > 0 {
		e.notifier(ctx, "uncovered-open-lane-rollup",
			fmt.Sprintf("📋 uncovered-open-lane roll-up refreshed: %d genuinely-uncovered open lane(s) with no live successor gardener — see %s (bug ka-0000002082 fix 4).", total, docID),
			"")
	}
}

// liveGardenerEpics returns the set of epic ids that have at least one OPEN +
// enabled trigger anchored to them (a live successor handle). Built in one query
// so the per-candidate coverage check is a map lookup. A query failure returns an
// empty set — the classifier then conservatively treats every epic as
// uncovered, which over-reports into a REPORT (never mutates), so a transient
// daemon error surfaces MORE lanes to a human rather than hiding one.
func (e *TriggerEngine) liveGardenerEpics(ctx context.Context) map[string]bool {
	covered := map[string]bool{}
	res, err := e.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"trigger"},
		Statuses: []string{"open"},
		Fields:   map[string]string{"enabled": "true"},
		Limit:    uncoveredOpenLaneRollupRosterLimit,
	})
	if err != nil {
		e.logger.Warn("uncovered-open-lane-rollup: failed to load open+enabled triggers for coverage set", "error", err)
		return covered
	}
	for _, bead := range res.Beads {
		if id := gardenerEpicLabel(bead); id != "" {
			covered[id] = true
		}
	}
	return covered
}

// uncoveredAnchorEpic is the memoized classification of one anchor epic.
type uncoveredAnchorEpic struct {
	isOpenEpic   bool
	title        string
	openChildren int
	lastChildAt  time.Time
}

// classifyAnchorEpic loads the anchor epic and its open-child count. A row only
// qualifies when the anchor is a real OPEN type==epic with >0 open/in_progress
// children. Open-child count uses the daemon's transitive-subtree Parent filter
// (CountOnly-style via Total), and the newest child's updated_at is the lane's
// last activity. A missing/unreadable epic returns isOpenEpic=false so the
// candidate is conservatively dropped (never surfaced on a broken lookup).
func (e *TriggerEngine) classifyAnchorEpic(ctx context.Context, epicID string) (info uncoveredAnchorEpic) {
	epic, err := e.daemon.GetBead(ctx, epicID)
	if err != nil || epic == nil {
		return info
	}
	// Assert type==epic: the survey false positive ka-0000006579 was a
	// gardener-epic label pointing at a type=trigger. Without this assertion that
	// lane re-surfaces every run.
	if epic.Type != "epic" || epic.Status != "open" {
		return info
	}
	info.title = epic.Title

	// >0 open/in_progress children (transitive subtree). Sort newest-first with
	// Limit 1 so one query yields both the count (Total) and the last child
	// activity (Beads[0].UpdatedAt).
	res, err := e.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Parent:   epicID,
		Statuses: []string{"open", "in_progress"},
		Sort:     "-updated_at",
		Limit:    1,
	})
	if err != nil {
		e.logger.Warn("uncovered-open-lane-rollup: failed to count open children", "epic", epicID, "error", err)
		return info
	}
	info.isOpenEpic = true
	info.openChildren = res.Total
	if len(res.Beads) > 0 && res.Beads[0] != nil {
		info.lastChildAt = res.Beads[0].UpdatedAt
	}
	return info
}

// renderUncoveredOpenLaneRollup builds the markdown body of the living report.
func renderUncoveredOpenLaneRollup(lanes []uncoveredLane, total, scanned int, truncated bool, now time.Time) string {
	var b strings.Builder
	b.WriteString("# Uncovered-open-lane roll-up\n\n")
	b.WriteString(fmt.Sprintf("_As of %s — living report, refreshed daily by the trigger engine (bug ka-0000002082 fix 4)._\n\n", now.UTC().Format(time.RFC3339)))
	b.WriteString("A **genuinely-uncovered lane** is a `closed`+`enabled` trigger anchored to an **OPEN** " +
		"gardener-epic that still has open children and **no live successor gardener** (no other open+enabled " +
		"trigger on that epic). These lanes should be REVIVED, not archived — they are deliberately left " +
		"untouched by the closed+enabled drain sweep. This report is one signal for the whole cohort, " +
		"decoupled from the per-trigger revival-review decision queue.\n\n")
	b.WriteString(fmt.Sprintf("Scanned %d non-manual closed+enabled trigger(s). ", scanned))

	if total == 0 {
		b.WriteString("**0 uncovered lanes this cycle** — every closed+enabled trigger is either " +
			"retired-lineage (closed anchor), engine-retired (terminate marker), drained (no open children), " +
			"unanchored, or already covered by a live successor gardener. Nothing to revive.\n")
		return b.String()
	}

	b.WriteString(fmt.Sprintf("**%d uncovered lane(s)** need a human keep-or-revive call:\n\n", total))
	b.WriteString("| Trigger | Anchor epic | Open children | Last child activity |\n")
	b.WriteString("|---|---|---|---|\n")
	for _, l := range lanes {
		last := "—"
		if !l.lastChildAt.IsZero() {
			last = l.lastChildAt.UTC().Format("2006-01-02")
		}
		b.WriteString(fmt.Sprintf("| `%s` %s | `%s` %s | %d | %s |\n",
			l.triggerID, truncateForRollup(l.triggerTitle, 60),
			l.epicID, truncateForRollup(l.epicTitle, 40),
			l.openChildren, last))
	}
	if truncated {
		b.WriteString(fmt.Sprintf("\n_Report truncated to %d rows; %d uncovered lanes total this cycle._\n",
			uncoveredOpenLaneRollupRowCap, total))
	}
	b.WriteString("\n**How to act on a row:** to revive a lane, reopen its trigger and `gb trigger enable <trigger-id>` " +
		"(or spawn a fresh gardener on the anchor epic). To retire it instead, archive the trigger " +
		"(`gb trigger disable <trigger-id>`) once you have confirmed the open children are genuinely stale.\n")
	return b.String()
}

// truncateForRollup trims a title for a table cell and escapes the markdown pipe
// so a title containing '|' cannot break the table layout.
func truncateForRollup(s string, max int) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > max {
		return s[:max-1] + "…"
	}
	return s
}

// upsertUncoveredOpenLaneRollupDoc updates the single living report doc in place,
// or creates it on first run. Returns the doc id and whether it was created.
// Finding the doc by its marker label (not by id kept in memory) makes the whole
// flow restart-safe: the living doc survives an engine restart and is picked up
// again by the next run.
func (e *TriggerEngine) upsertUncoveredOpenLaneRollupDoc(ctx context.Context, body string) (string, bool, error) {
	existing, err := e.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"doc"},
		Statuses: []string{"open"},
		Labels:   []string{uncoveredOpenLaneRollupMarkerLabel},
		Limit:    5,
	})
	if err != nil {
		return "", false, fmt.Errorf("look up living report doc: %w", err)
	}
	if existing != nil && len(existing.Beads) > 0 {
		if len(existing.Beads) > 1 {
			// Should never happen (the sweep only ever creates one), but be loud
			// if a duplicate slips in so it can be closed by hand.
			e.logger.Warn("uncovered-open-lane-rollup: multiple living report docs found — updating the first",
				"count", len(existing.Beads), "using", existing.Beads[0].ID)
		}
		docID := existing.Beads[0].ID
		if err := e.daemon.UpdateBeadFields(ctx, docID, map[string]string{"content": body}); err != nil {
			return "", false, fmt.Errorf("update living report doc %s: %w", docID, err)
		}
		return docID, false, nil
	}

	// First run: create the living doc. The body MUST live in fields.content —
	// the bundle renderer never reads description, and the daemon rejects a doc
	// whose body is in -d/--description (advice ka-0000006521).
	fieldsJSON, err := json.Marshal(map[string]any{"format": "md", "content": body})
	if err != nil {
		return "", false, fmt.Errorf("marshal doc fields: %w", err)
	}
	docID, err := e.daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:     uncoveredOpenLaneRollupTitle,
		Type:      "doc",
		Kind:      "data",
		Priority:  2,
		CreatedBy: uncoveredOpenLaneRollupActor,
		Labels:    []string{"project:gardenio", uncoveredOpenLaneRollupMarkerLabel},
		Fields:    fieldsJSON,
	})
	if err != nil {
		return "", false, fmt.Errorf("create living report doc: %w", err)
	}
	return docID, true, nil
}
