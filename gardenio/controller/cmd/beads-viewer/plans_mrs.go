package main

// plans_mrs.go — the plan-level "open MRs" signal (Matt's ask: "the plans index
// should show open MRs so we can click through there too, just like w crew").
//
// A plan's steps produce merge requests: a step task carries a FORWARD `produces`
// dep edge to a type=merge-request bead (step --produces--> merge-request; see
// beadsapi.MRBeadForTask / the inboxMRResolver the detail timeline already uses
// per-step). This file aggregates those edges per plan so the /plans index card
// can surface the plan's OPEN MRs as click-through pills — exactly mirroring the
// crew rollup (planCrewBulk) that surfaces the plan's crew on the same card.
//
// "Open" is the MR BEAD's own lifecycle, not a guess from mr_state: a merged /
// closed MR closes its bead (the controller's MR sync), so an OPEN MR is a
// merge-request bead whose Status != "closed" — the same definition the /home
// "Open MRs" section queries (user_home.go: Statuses ["open","in_progress"]).
// mr_state is read only to tint the pill (draft vs opened).
//
// Derived in the SAME batched enrich idiom as the crew + escalation signals: ONE
// forward `produces`-edge batch over the union of every plan's rendered step ids
// + ONE bead hydration batch over the distinct produced targets — no per-row
// fan-out, bounded daemon calls regardless of plan count. Best-effort: any daemon
// error returns an empty map and every card renders no MR pills (never a partial
// or misleading tally).

import (
	"context"
	"sort"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// planMRRef is one open merge request a plan's steps produced, projected for the
// index card's click-through pill.
type planMRRef struct {
	// URL is the canonical GitLab MR URL the pill links to (mr_url). A ref with
	// no URL is dropped upstream — a pill with no href would dead-link.
	URL string
	// IID is the MR's "!1234" number, shown as the pill label when present.
	IID string
	// Title is the MR title, surfaced in the pill tooltip.
	Title string
	// State is the raw mr_state ("opened" / "draft" / …) used to tint the pill
	// and read in the tooltip; Draft is the derived draft flag.
	State string
	Draft bool
	// PipelineStatus is the MR's head-pipeline status (mr_pipeline_status),
	// kept fresh on the MR bead by the gitlab-bridge pipeline webhook
	// (gitlab_sync.go handlePipelineWebhook). Already carried on the hydrated MR
	// bead planOpenMRsBulk fetches, so surfacing it on the pill costs no extra
	// daemon call. Empty when the MR has no pipeline yet (a just-opened branch)
	// — the pill then reads exactly as before (no CI signal).
	PipelineStatus string
	// OpenAge is the humanized "how long has this MR sat open" wait-age
	// ("· 8d"), folded onto the pill FACE (ka-0000005352 iter-40) so the MR
	// pill — the LAST card presence-signal that carried no temporal urgency —
	// surfaces its age like every sibling wait-context signal already does: the
	// decision chip ("· 32d", iter-4), the escalation badge ("· 40d", iter-8),
	// the stall badge ("· 3d", iter-11), and the stuck/queued crew facets
	// ("· 6h" / "· 12m", iter-13/32). A green MR opened 40 minutes ago read
	// identically to one open 3 weeks awaiting a human merge; the age discloses
	// the merge/review latency the CI glyph + title never could. Derived off the
	// MR bead's CreatedAt already hydrated (like the escalation/decision
	// wait-ages, which also key on CreatedAt) — zero extra daemon call, stamped
	// against the handler's s.now() so a test injects one clock. Empty when the
	// MR carries no datable CreatedAt (a fixture/legacy bead with an unset stamp)
	// — the pill then reads exactly as before, no age. OpenStale is true once the
	// wait crosses planMROpenStaleThreshold (>7d), tinting the pill loud so a
	// long-open MR pops out of the board, mirroring the escalation badge's
	// EscalationStale and the freshness stamp's FreshnessStale (both 7d).
	OpenAge   string
	OpenStale bool
}

// pipelineStatusGlyph maps a GitLab pipeline status to a compact status glyph
// for the index MR pill, reusing the SAME status vocabulary the bridge's
// gitlab_mrdesc.pipelineIcon uses so the two surfaces agree. A blank status
// yields no glyph (the caller suppresses the CI badge entirely).
func pipelineStatusGlyph(status string) string {
	switch status {
	case "success":
		return "✓"
	case "failed":
		return "✕"
	case "running":
		return "▸"
	case "pending", "created", "scheduled", "waiting_for_resource", "preparing":
		return "◷"
	case "canceled":
		return "⊘"
	default:
		return "•"
	}
}

// pipelineStatusBucket collapses the many GitLab pipeline statuses into the
// three families the pill styling cares about — "success", "failed", or
// "running" (everything in-flight or unknown-but-present) — so the template
// keys a small, stable set of CSS classes instead of every raw status. A
// failed pipeline is the "needs attention" case the pill tints loud; success
// reads calm-green; anything else in flight reads neutral-blue. Empty for a
// blank status (no CI badge rendered).
func pipelineStatusBucket(status string) string {
	switch status {
	case "":
		return ""
	case "success":
		return "success"
	case "failed":
		return "failed"
	case "canceled":
		return "canceled"
	default:
		// running / pending / created / scheduled / manual / skipped / … — an
		// in-flight or indeterminate pipeline. Reads neutral, not alarming.
		return "running"
	}
}

// CIState returns the pill's CI bucket ("success"/"failed"/"running"/
// "canceled"/"") for the template's class + render gate. Empty ⇒ render no CI
// badge (the MR has no known pipeline).
func (r planMRRef) CIState() string { return pipelineStatusBucket(r.PipelineStatus) }

// CIGlyph returns the compact status glyph for the pill's CI badge. Empty when
// the MR has no known pipeline (paired with CIState == "").
func (r planMRRef) CIGlyph() string {
	if r.PipelineStatus == "" {
		return ""
	}
	return pipelineStatusGlyph(r.PipelineStatus)
}

// planMRInfo is the derived per-plan open-MR signal: the open MRs the plan's
// steps produced, ordered newest-first (by the MR bead's update, most-recently
// touched leads).
type planMRInfo struct {
	MRs []planMRRef
}

// reconcileTerminal quiets the LOUD wait-age tint on a TERMINAL (closed /
// deferred) plan's open-MR pills (ka-0000005352 iter-45) — the MR twin of
// planFreshnessStale's terminal gate (iter-38) and ShowEscalationBadge's
// terminal suppression (iter-39). iter-40 folded the MR's since-opened wait-age
// onto the pill face and, past planMROpenStaleThreshold (7d), fires the LOUD
// amber plan-card-mr-stale tint — a board-topping "this MR is rotting, act NOW"
// alarm. That tint was NOT reconciled against the plan epic's OWN terminal
// status: a CLOSED (or deferred) plan can still carry a leftover child step with
// an open, aged MR bead, so the pill rendered LOUD amber-stale on a DONE plan —
// while the plan's OWN freshness stamp on the same card correctly reads CALM
// history (planFreshnessStale gates on non-terminal). That is a card-vs-truth
// contradiction in the exact terminal-reconcile family iter-24/25/27/30/38/39
// named: a finished plan's age is honest backward-looking history, never a live
// "act now" alarm. Live proof (2026-08-09 pour): closed plans ka-0000005928 +
// ka-0000005311 rendered 3 open-MR pills ALL carrying plan-card-mr-stale (loud
// amber) while both cards' freshness stamps read calm ("5 days ago" / "1 week
// ago", no stale tint).
//
// The reconcile is DISPLAY-ONLY and surgical: it clears OpenStale on every ref
// so the pill drops ONLY the loud amber tint. The pill itself, its click-through
// to GitLab, its CI glyph, and its honest "· 8d" wait-age all STILL render — a
// closed plan's dangling open MR is real backward-looking history the operator
// may still want to reach (merge or close it), exactly like the closed plan's
// task-grid + "N/N steps · 100%" that remain honest history. Only the "act now"
// COLOR is quieted, not the FACT. Returns the info unchanged on a non-terminal
// plan (an active plan's aged MR still tints loud) or when no ref is stale
// (idempotent). Pure over the refs + status, so it is trivially table-testable
// and the page render + the single-row SSE re-render (renderPlanRowWith →
// planIndexEnrich) can never disagree. Zero extra daemon call — status is
// already on the row.
func (i planMRInfo) reconcileTerminal(status string) planMRInfo {
	if !planStatusTerminal(status) || len(i.MRs) == 0 {
		return i
	}
	// Copy-on-write: the refs slice is shared with the enrich map, so mutate a
	// fresh slice rather than the caller's backing array.
	refs := make([]planMRRef, len(i.MRs))
	copy(refs, i.MRs)
	for idx := range refs {
		refs[idx].OpenStale = false
	}
	return planMRInfo{MRs: refs}
}

// Any reports whether the plan has any open MR — the render gate for the index
// card's MR pills (no open MR → no pills, so a plan with none adds no ink).
func (i planMRInfo) Any() bool { return len(i.MRs) > 0 }

// Count is how many open MRs the plan has.
func (i planMRInfo) Count() int { return len(i.MRs) }

// planMRMaxRender caps how many individual MR pills the card renders inline. A
// plan with more open MRs than this shows the first few plus a "+N" pill that
// links through to the cockpit timeline (where every step's MR pill lives). Kept
// modest so the MR row never dominates a dense index card.
const planMRMaxRender = 3

// planMROpenStaleThreshold is the open-duration past which the MR pill's
// wait-age tints loud (ka-0000005352 iter-40) — the mark that an open MR has sat
// awaiting review/merge egregiously long. Set to 7d, matching the escalation
// badge (planEscalationStaleThreshold) and the freshness stamp
// (planFreshnessStaleThreshold): an open MR is code awaiting a human's
// review/merge, the same "an aged approval is blocking" shape as an aged
// escalation, so it earns the loud tint on the same 7-day bar. Below it the pill
// reads calm (the age is context, not an alarm), the same two-tier
// calm-by-default / loud-when-egregious treatment the escalation, stall, and
// stuck/queued crew wait-ages give.
const planMROpenStaleThreshold = 7 * 24 * time.Hour

// Rendered returns the MR refs to render inline (at most planMRMaxRender).
func (i planMRInfo) Rendered() []planMRRef {
	if len(i.MRs) > planMRMaxRender {
		return i.MRs[:planMRMaxRender]
	}
	return i.MRs
}

// HasMore reports whether the plan has more open MRs than the inline pills show.
func (i planMRInfo) HasMore() bool { return len(i.MRs) > planMRMaxRender }

// MoreCount is how many open MRs are elided past the inline render cap.
func (i planMRInfo) MoreCount() int {
	n := len(i.MRs) - planMRMaxRender
	if n < 0 {
		return 0
	}
	return n
}

// elidedMRs returns the MR refs hidden behind the "+N MR" overflow pill — the
// tail past planMRMaxRender. Empty when the plan has planMRMaxRender or fewer
// open MRs (nothing overflows). The per-pill CI signal (iter-5, ka-0000002426)
// only ever reaches the FIRST planMRMaxRender MRs (the ones Rendered() returns);
// these are the MRs whose CI health the overflow pill collapses into a bare
// count, and the source of the FailedHiddenCount() attention signal below.
func (i planMRInfo) elidedMRs() []planMRRef {
	if len(i.MRs) <= planMRMaxRender {
		return nil
	}
	return i.MRs[planMRMaxRender:]
}

// FailedHiddenCount is how many of the ELIDED open MRs (the ones behind the
// "+N MR" overflow pill) have a FAILED head pipeline (ka-0000000387, iter-20).
// Each inline pill already carries its own CI glyph + loud-red failed tint
// (iter-5), but past the render cap the overflow collapses every remaining MR
// into ONE calm-gray count that drops all CI signal — so a plan whose HIDDEN
// MRs are failing reads identically to one whose hidden MRs are all green. This
// counts the failing tail off the SAME mr_pipeline_status planOpenMRsBulk
// already hydrated on each ref (zero extra daemon call), so the template can
// tint the "+N MR" pill loud and fold a "· ✕ K" facet onto it — extending the
// card's "loud red = needs attention" vocabulary (⚠ stall / ⛔ escalation /
// per-pill CI-failed) to the collapsed overflow. Zero when nothing overflows or
// every hidden MR is green/running (the pill then reads calm, exactly as before).
func (i planMRInfo) FailedHiddenCount() int {
	n := 0
	for _, r := range i.elidedMRs() {
		if r.CIState() == "failed" {
			n++
		}
	}
	return n
}

// HasFailedHidden reports whether any MR behind the "+N MR" overflow pill has a
// failed head pipeline — the render gate for the overflow pill's loud-red tint +
// "· ✕ K" facet (ka-0000000387). Paired with FailedHiddenCount so an all-green
// overflow adds no attention ink, exactly like every other zero-valued signal on
// the card.
func (i planMRInfo) HasFailedHidden() bool { return i.FailedHiddenCount() > 0 }

// planOpenMRsBulk resolves the open MRs for every plan on the /plans index in the
// SAME batched idiom as planCrewBulk / planEscalationsBulk. stepIDsByPlan maps a
// plan epic id → its rendered step ids (the SAME set the crew + escalation
// aggregations reuse). Two batched daemon calls across the WHOLE page: ONE forward
// `produces`-edge batch over the union of every plan's step ids, ONE hydration
// batch over the distinct produced targets. A target that is a non-closed
// type=merge-request bead carrying an mr_url becomes an open-MR ref, deduped per
// plan (one MR produced by two steps counts once) and ordered newest-first.
// Best-effort: a daemon error returns an empty map and every card renders no MR
// pills.
func (s *Server) planOpenMRsBulk(ctx context.Context, stepIDsByPlan map[string][]string) map[string]planMRInfo {
	out := make(map[string]planMRInfo, len(stepIDsByPlan))
	if len(stepIDsByPlan) == 0 {
		return out
	}
	// Union of every plan's rendered step ids → one forward produces-edge batch.
	// step --produces--> merge-request, so a step's FORWARD deps surface its MRs.
	union := make([]string, 0, len(stepIDsByPlan)*4)
	seen := make(map[string]struct{})
	for _, ids := range stepIDsByPlan {
		for _, id := range ids {
			if id == "" {
				continue
			}
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			union = append(union, id)
		}
	}
	if len(union) == 0 {
		return out
	}
	depsByStep, err := s.daemon.BatchGetDependencies(ctx, union, "forward")
	if err != nil {
		s.logger.Debug("plan open MRs: produces walk", "error", err)
		return out
	}
	// Distinct produced-target ids across the whole page → one hydration batch.
	targets := make([]string, 0)
	targetSeen := make(map[string]struct{})
	for _, sid := range union {
		for _, d := range depsByStep[sid] {
			if d.Type != "produces" || d.DependsOnID == "" {
				continue
			}
			if _, dup := targetSeen[d.DependsOnID]; dup {
				continue
			}
			targetSeen[d.DependsOnID] = struct{}{}
			targets = append(targets, d.DependsOnID)
		}
	}
	if len(targets) == 0 {
		return out
	}
	mrs, err := s.daemon.BatchGetBeads(ctx, targets)
	if err != nil {
		s.logger.Debug("plan open MRs: MR hydration batch", "error", err)
		return out
	}
	// dated pairs an MR ref with its sort key (the MR bead's update instant) so a
	// single stable sort orders the plan's pills newest-first.
	type dated struct {
		ref planMRRef
		key int64
	}
	// One clock read for the whole page's MR wait-ages (ka-0000005352 iter-40),
	// stamped against s.now() so a test injects it — mirroring how planCrewBulk
	// threads s.now() into oldestOpenCrewAge for the stuck/queued facets.
	now := s.now()
	for planID, ids := range stepIDsByPlan {
		rows := make([]dated, 0, 2)
		mrSeen := make(map[string]struct{})
		for _, sid := range ids {
			for _, d := range depsByStep[sid] {
				if d.Type != "produces" || d.DependsOnID == "" {
					continue
				}
				b := mrs[d.DependsOnID]
				if b == nil || b.Type != "merge-request" {
					continue // hydration miss or a non-MR target on a stray edge
				}
				// Open == the MR bead is not closed (a merged/closed MR closes its
				// bead). Same definition /home's "Open MRs" section queries.
				if b.Status == "closed" {
					continue
				}
				if _, dup := mrSeen[b.ID]; dup {
					continue // one MR produced by two steps counts once
				}
				url := mrFieldString(b.Fields, "mr_url", "url", "gitlab_mr_url")
				if url == "" {
					continue // a pill with no href would dead-link
				}
				mrSeen[b.ID] = struct{}{}
				state := mrFieldString(b.Fields, "mr_state", "state")
				// "How long has this MR sat open" wait-age (ka-0000005352 iter-40):
				// keyed on the MR bead's CreatedAt — when the MR was opened — NOT its
				// UpdatedAt (which the ordering key below reads and which a CI-webhook
				// or comment bumps continuously, so it would answer "last touched", a
				// different question). The escalation + decision wait-ages likewise key
				// on CreatedAt. Stamped against s.now() so a test injects one clock, and
				// tinted loud past planMROpenStaleThreshold; an unset CreatedAt reads
				// count-only (empty age), exactly as before this iter.
				openAge := fmtTimeAgo(b.CreatedAt, now)
				openStale := !b.CreatedAt.IsZero() && now.Sub(b.CreatedAt) >= planMROpenStaleThreshold
				rows = append(rows, dated{
					ref: planMRRef{
						URL:            url,
						IID:            mrFieldString(b.Fields, "mr_iid", "iid"),
						Title:          mrTitleFor(b),
						State:          state,
						Draft:          state == "draft",
						PipelineStatus: mrFieldString(b.Fields, "mr_pipeline_status", "pipeline_status"),
						OpenAge:        openAge,
						OpenStale:      openStale,
					},
					key: b.UpdatedAt.Unix(),
				})
			}
		}
		if len(rows) == 0 {
			continue
		}
		// Newest-first (most-recently touched MR leads), stable URL tie-break.
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i].key != rows[j].key {
				return rows[i].key > rows[j].key
			}
			return rows[i].ref.URL < rows[j].ref.URL
		})
		// Collapse to one pill per canonical GitLab MR (dedup by URL). The mrSeen
		// pass above dedups by MR BEAD id — it guards a single bead reached via two
		// producing steps. But two DISTINCT merge-request beads can carry the SAME
		// mr_url (a re-created / re-linked MR bead: observed live as beads
		// ka-0000007659 + ka-0000002580 both on !6037), and those slip past the
		// bead-id dedup — rendering the SAME MR twice as an identical pill AND
		// inflating the "+N MR" overflow, a card-vs-truth delta on the 375px-tight
		// card. Deduping here (AFTER the newest-first sort) keeps the newest bead's
		// ref for each URL, so its title/state/pipeline win — the same "most-recently
		// touched leads" the ordering already promises. Zero extra daemon call.
		refs := make([]planMRRef, 0, len(rows))
		urlSeen := make(map[string]struct{}, len(rows))
		for _, r := range rows {
			if _, dup := urlSeen[r.ref.URL]; dup {
				continue
			}
			urlSeen[r.ref.URL] = struct{}{}
			refs = append(refs, r.ref)
		}
		// Attention-first promotion (ka-0000005352 iter-21): a stable partition that
		// pulls every FAILED-CI MR ahead of the non-failed ones, preserving the
		// newest-first order WITHIN each tier. This runs AFTER the newest-first sort +
		// URL dedup, so the "newest bead's ref wins per URL" contract above is
		// untouched — only the render ORDER of the final canonical refs changes.
		//
		// Why: iter-20 (ka-0000000387) made a hidden failing MR VISIBLE (the loud
		// "+N MR · ✕ K" overflow tint), but the failing MR itself stayed buried —
		// UN-CLICKABLE — behind the "+N" count: an operator saw the plan had a red MR
		// but had to open the cockpit to reach it (live proof ka-0000003619: 3 green
		// MRs rendered inline while its ONE red MR sat in "+17 MR · ✕ 1"). Promoting a
		// failed MR into the first planMRMaxRender slots makes it a clickable inline
		// pill — the least-reachable MR becomes the most-reachable, matching the card's
		// "loud red = needs attention leads" vocabulary (the stuck-crew facet, the ⚠
		// stall / ⛔ escalation badges). This is the deferred iter-21 the plan narrative
		// (ka-0000002768) named.
		//
		// It is a pure reorder: same pills, same footprint, zero new data. Because MRs
		// is the SINGLE ordering authority (Rendered()/elidedMRs()/FailedHiddenCount()
		// are pure projections off it), promoting a failure out of the elided tail
		// automatically drops it from FailedHiddenCount — so the overflow's ✕ facet now
		// counts only failures that STILL overflow after promotion (more failing MRs
		// than inline slots), never a failure the operator can already click. A plan
		// with no failing MR partitions to a no-op and renders EXACTLY as before.
		sort.SliceStable(refs, func(i, j int) bool {
			return refs[i].CIState() == "failed" && refs[j].CIState() != "failed"
		})
		out[planID] = planMRInfo{MRs: refs}
	}
	return out
}

// mrTitleFor returns the MR's display title: the mr_title field, else the bead's
// own title. Mirrors wrapUpMRViewFromBead's title precedence.
func mrTitleFor(b *beadsapi.BeadDetail) string {
	if t := mrFieldString(b.Fields, "mr_title", "title"); t != "" {
		return t
	}
	return b.Title
}
