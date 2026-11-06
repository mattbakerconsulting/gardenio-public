// Engine-side daily sweep for gardeners left dormant while their plan has work
// (gd-kkrCdlf4iO, ask 3 of bug gd-FUC635HJ2E).
//
// THE GAP. A gardener parks (DormantOnEscalation=true) when its whole frontier
// is single-threaded behind an open operator escalation/decision, and its
// pre-fire gate wakes it on the next cycle once the frontier has an actionable
// child again (gardenerDormantStillBlocked). Before gd-FUC635HJ2E the legacy
// wake predicate looked only for "any open escalation in the subtree", so 9 of
// 13 plan gardeners sat parked for days with dozens of actionable children and
// nothing said so.
//
// THE NET. Once a day this sweep lists every open, enabled gardener that has
// been dormant for over dormantGardenerSweepAge and asks the live
// ReconcileDormancyProbe whether it is still parked. A gardener whose probe
// says NOT parked with ActionableChildren > 0 should have woken on its own; a
// hit means the wake path is broken (the gardener is not being evaluated, its
// cron pre-flight never reaches the gate, or the predicate regressed). The
// sweep files ONE operator escalation naming every hit, deduped: while an
// earlier one is still open it only comments on it when the set of gardeners
// changes. It never wakes, spawns or edits a gardener itself.

package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Dormant-gardener sweep tunables.
const (
	// dormantGardenerSweepAge is how long a gardener must have been dormant
	// before a not-parked probe counts as a hit. The wake gate runs on the
	// gardener's own cron, so a just-unblocked gardener can legitimately stay
	// dormant for one cadence; a day is far past any gardener cadence.
	dormantGardenerSweepAge = 24 * time.Hour

	// dormantGardenerSweepThrottle runs the sweep once a day.
	dormantGardenerSweepThrottle = 24 * time.Hour

	// dormantGardenerSweepMarkerLabel tags the escalation the sweep files so
	// the next run finds it and does not file another while it is open.
	dormantGardenerSweepMarkerLabel = "dormant-gardener-sweep"

	// dormantGardenerSweepActor is the CreatedBy / comment author.
	dormantGardenerSweepActor = "dormant-gardener-sweep"

	// dormantGardenerSweepSystemOperator is the X-System-Operator source label
	// for the escalation create and comment, so the daemon attributes this
	// machine-origin write instead of 400ing "operator could not be resolved"
	// (ka-0000007257), as the sibling sweeps do.
	dormantGardenerSweepSystemOperator = "dormant-gardener-sweep"
)

// dormantGardenerHit is one gardener the sweep names: dormant for over a day
// while its live probe says it has work it could do.
type dormantGardenerHit struct {
	triggerID    string
	title        string
	project      string
	frontierEpic string
	since        time.Time // DormantSince, else last_run
	actionable   int
	blocker      string // stamped DormantBlocker
	blockerKind  string // stamped DormantBlockerKind
}

// dormantGardenerSweepState throttles the sweep to once per
// dormantGardenerSweepThrottle. Dedup lives in the daemon (the marker-labelled
// open escalation), so the last-run clock is the only in-memory state.
type dormantGardenerSweepState struct {
	mu        sync.Mutex
	lastRunAt time.Time
}

func newDormantGardenerSweepState() *dormantGardenerSweepState {
	return &dormantGardenerSweepState{}
}

// dueForRun reports whether the throttle interval has elapsed since the last
// run and, if so, records now as the last run. The first call is always due.
func (s *dormantGardenerSweepState) dueForRun(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.lastRunAt.IsZero() && now.Sub(s.lastRunAt) < dormantGardenerSweepThrottle {
		return false
	}
	s.lastRunAt = now
	return true
}

// SweepDormantGardeners runs the daily dormant-gardener check (gd-kkrCdlf4iO).
// See the file header. No-op unless dormantGardenerSweepEnabled; skipped under
// a global triggers-pause or gardeners-pause, when no gardener wakes by design.
// Errors are logged and downgraded so a daemon hiccup cannot wedge the engine.
func (e *TriggerEngine) SweepDormantGardeners(ctx context.Context) {
	if !e.dormantGardenerSweepEnabled {
		return
	}
	now := e.now()
	if !e.dormantGardenerSweepState.dueForRun(now) {
		return
	}
	if gardenersPauseEngaged(ctx, e.daemon, e.logger) {
		e.logger.Info("dormant-gardener sweep: gardeners-pause engaged, skipping")
		return
	}
	if e.loadGateState(ctx).globallyPaused {
		e.logger.Info("dormant-gardener sweep: triggers-pause engaged, skipping")
		return
	}

	ctx = beadsapi.WithSystemOperator(ctx, dormantGardenerSweepSystemOperator)

	res, err := e.openTriggerRoster(ctx)
	if err != nil {
		e.logger.Warn("dormant-gardener sweep: failed to load trigger roster", "error", err)
		return
	}

	var hits []dormantGardenerHit
	dormant := 0
	for _, bead := range res.Beads {
		if bead == nil || bead.Fields["enabled"] == "false" || isE2EFixture(bead) {
			continue
		}
		trig := ParseTriggerDef(bead, strings.TrimSpace(bead.Fields["source"]))
		if trig.Gardener == nil || !trig.Gardener.DormantOnEscalation {
			continue
		}
		dormant++
		since := dormantGardenerSince(bead, trig.Gardener)
		if since.IsZero() || now.Sub(since) < dormantGardenerSweepAge {
			continue
		}
		// The live verdict, from the same predicates the pre-fire gate uses.
		// Fail-safe toward NOT parked, so a read error leaves ActionableChildren
		// at -1 and the gardener is not named.
		probe := ReconcileDormancyProbe(ctx, e.daemon, e.logger, bead)
		if probe == nil || !probe.Evaluated || probe.Parked || probe.ActionableChildren <= 0 {
			continue
		}
		hits = append(hits, dormantGardenerHit{
			triggerID:    bead.ID,
			title:        bead.Title,
			project:      trig.Project,
			frontierEpic: probe.FrontierEpic,
			since:        since,
			actionable:   probe.ActionableChildren,
			blocker:      strings.TrimSpace(trig.Gardener.DormantBlocker),
			blockerKind:  trig.Gardener.DormantBlockerKind,
		})
	}

	// Longest-dormant first, then id, so the escalation reads the same way
	// across runs.
	sort.SliceStable(hits, func(i, j int) bool {
		if !hits[i].since.Equal(hits[j].since) {
			return hits[i].since.Before(hits[j].since)
		}
		return hits[i].triggerID < hits[j].triggerID
	})

	escID, action := "", "none"
	if len(hits) > 0 {
		escID, action = e.fileDormantGardenerEscalation(ctx, hits, now)
	}

	e.logger.Info("dormant-gardener sweep: complete",
		"dormant", dormant, "hits", len(hits), "escalation", escID, "action", action)

	if e.dormantGardenerSweepNotify && e.notifier != nil && (action == "filed" || action == "commented") {
		e.notifier(ctx, "dormant-gardener-sweep",
			fmt.Sprintf("dormant-gardener sweep: %d gardener(s) dormant over 24h while their plan has actionable children, so the dormant wake path is not running for them (gd-kkrCdlf4iO). See %s.", len(hits), escID),
			"")
	}
}

// dormantGardenerSince returns when the gardener went dormant: the stamped
// DormantSince, else last_run (a dormant gardener does not fire, so it has been
// quiet at least since then). Zero when neither is known.
func dormantGardenerSince(bead *beadsapi.BeadDetail, g *GardenerSpec) time.Time {
	if !g.DormantSince.IsZero() {
		return g.DormantSince
	}
	return parseLivenessTimestamp(bead.Fields["last_run"])
}

// fileDormantGardenerEscalation files the one operator escalation naming hits,
// or, when an earlier one is still open, comments on it if the named set has
// changed. Returns the escalation id and what it did: "filed", "commented",
// "unchanged" or "error".
func (e *TriggerEngine) fileDormantGardenerEscalation(ctx context.Context, hits []dormantGardenerHit, now time.Time) (string, string) {
	ids := make([]string, len(hits))
	for i, h := range hits {
		ids[i] = h.triggerID
	}
	body := renderDormantGardenerHits(hits, now)

	open, err := e.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"escalation"},
		Statuses: []string{"open", "in_progress"},
		Labels:   []string{dormantGardenerSweepMarkerLabel},
		Limit:    10,
	})
	if err != nil {
		// Without the dedup read we cannot tell whether one is already open;
		// filing anyway could stack a duplicate every day. Try again tomorrow.
		e.logger.Warn("dormant-gardener sweep: failed to check for an open escalation", "error", err)
		return "", "error"
	}
	if len(open.Beads) > 0 && open.Beads[0] != nil {
		existing := open.Beads[0]
		if slices.Equal(dormantGardenerEscalationIDs(existing), ids) {
			return existing.ID, "unchanged"
		}
		comment := "The dormant-gardener sweep found a different set today.\n\n" + body
		if err := e.daemon.AddComment(ctx, existing.ID, dormantGardenerSweepActor, comment); err != nil {
			e.logger.Warn("dormant-gardener sweep: failed to comment on open escalation", "escalation", existing.ID, "error", err)
			return existing.ID, "error"
		}
		return existing.ID, "commented"
	}

	spec, err := json.Marshal(map[string]any{
		"action":    "investigate_dormant_gardener_wake",
		"gardeners": ids,
	})
	if err != nil {
		e.logger.Warn("dormant-gardener sweep: marshal operation_spec failed", "error", err)
		return "", "error"
	}
	fields, err := json.Marshal(map[string]any{
		"operation_type": "manual_operator_action",
		"operation_spec": json.RawMessage(spec),
		"context":        body,
		"audience":       "operator",
	})
	if err != nil {
		e.logger.Warn("dormant-gardener sweep: marshal fields failed", "error", err)
		return "", "error"
	}

	labels := []string{dormantGardenerSweepMarkerLabel, "trigger-engine"}
	for _, h := range hits {
		project := h.project
		if project == "" {
			project = "gardenio"
		}
		if l := "project:" + project; !slices.Contains(labels, l) {
			labels = append(labels, l)
		}
	}

	id, err := e.daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:       fmt.Sprintf("%d gardener(s) dormant over 24h with actionable children: the wake path is not running", len(hits)),
		ShortTitle:  "wake dormant gardeners with open work",
		Type:        "escalation",
		Description: body,
		Labels:      labels,
		Priority:    1,
		CreatedBy:   dormantGardenerSweepActor,
		Fields:      fields,
	})
	if err != nil {
		e.logger.Warn("dormant-gardener sweep: failed to file escalation", "error", err)
		return "", "error"
	}
	e.logger.Warn("dormant-gardener sweep: filed escalation", "escalation", id, "gardeners", ids)
	return id, "filed"
}

// dormantGardenerEscalationIDs reads back the gardener ids an open sweep
// escalation names (operation_spec.gardeners). Nil when absent or unreadable,
// which compares unequal to any hit set, so the sweep comments.
func dormantGardenerEscalationIDs(esc *beadsapi.BeadDetail) []string {
	var spec struct {
		Gardeners []string `json:"gardeners"`
	}
	if err := json.Unmarshal([]byte(esc.Fields["operation_spec"]), &spec); err != nil {
		return nil
	}
	return spec.Gardeners
}

// renderDormantGardenerHits builds the escalation body and the change comment.
func renderDormantGardenerHits(hits []dormantGardenerHit, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "As of %s, %d gardener(s) have been dormant for over 24h, yet the live dormancy probe "+
		"says each is NOT parked: its plan has actionable children. The engine should have woken them on "+
		"their next cycle (bug gd-FUC635HJ2E), so the wake path is not running for them. Daily safety net, "+
		"gd-kkrCdlf4iO.\n\n", now.UTC().Format(time.RFC3339), len(hits))
	for _, h := range hits {
		kind := gardenerDormantBlockerKindLabel(h.blockerKind)
		blocker := h.blocker
		if blocker == "" {
			blocker = "none stamped"
		}
		fmt.Fprintf(&b, "  • %s %s: plan %s, %d actionable child(ren), dormant since %s (%s), stamped blocker %s (%s)\n",
			h.triggerID, truncateForDecision(h.title, 70), orDash(h.frontierEpic), h.actionable,
			h.since.UTC().Format(time.RFC3339), now.Sub(h.since).Round(time.Hour), blocker, kind)
	}
	b.WriteString("\nFor each gardener: run `gb trigger reconcile <id>` to confirm it reads NOT parked, then check " +
		"the trigger-engine log for why it was not evaluated (cron pre-flight condition, disabled source, " +
		"gate skip). Close this escalation once they run again; the sweep files a new one if any stay dormant.\n")
	return b.String()
}

// orDash renders an empty string as an em dash.
func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
