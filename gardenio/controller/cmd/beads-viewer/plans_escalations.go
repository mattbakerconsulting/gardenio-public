package main

// plans_escalations.go — the plan-level "blocked by escalation" signal
// (epic ka-0000005480, WS2 ka-0000001288). An escalation blocks a plan when a
// non-closed type=escalation bead is a forward `blocks` dependency of any of
// the plan's rendered steps (or of the plan epic itself). The edge is written
// by `gb escalation create --requesting-task` (WS1, ka-0000004626); this file
// aggregates it per plan so the /plans index badge (WS3) and the cockpit
// banner (WS4) can shout it.
//
// Derived in the same batched enrich idiom as the stall signal
// (planStallBulk): ONE forward blocks-edge batch over every candidate id on
// the page + ONE bead hydration batch over the distinct blocker targets — no
// per-row fan-out, bounded daemon calls regardless of plan count.

import (
	"context"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// escalationPinListLimit bounds the ONE page-wide open-escalation query the
// requesting_plan pin pass issues (Layer 2, ka-0000007801). It is a single
// bounded query regardless of how many plans render, so the /plans index enrich
// budget stays plan-count invariant (plans_enrich_measure_test.go). Sized to the
// flat /escalations inbox convention (escalationsListLimit) — open escalations
// fleet-wide are far below this in practice.
const escalationPinListLimit = 200

// planEscalationRef is one open blocking escalation surfaced on a plan card
// badge tooltip or cockpit banner row.
type planEscalationRef struct {
	ID            string
	Title         string
	OperationType string
	Sensitivity   string
	// Audience is who approves it: owner, operator or desk (gd-bmOiteQi36).
	Audience string
	// Ago is the humanized creation stamp ("2 days ago") — escalation age IS
	// the urgency signal, same convention as the /escalations queue.
	Ago string
	// ReviewURL is the bridge-SPA approve/reject deep link
	// (buildEscalationReviewURL); empty when ESCALATION_REVIEW_URL is unset.
	ReviewURL string
	// createdAt orders the refs oldest-first; not rendered.
	createdAt time.Time
}

// planEscalationInfo is the derived per-plan signal.
type planEscalationInfo struct {
	Count       int
	Escalations []planEscalationRef
}

// ShowEscalationBadge reports whether the index card's ⛔ "blocked by escalation"
// badge FACE should render (ka-0000005352 iteration 39). It is
// `Escalations.Count > 0 EXCEPT on a TERMINAL (closed/deferred) plan`, so the
// badge is the escalation twin of the CurrentOrNextStep nil-guard (iter-25) and
// Crew.reconcileTerminal (iter-30): every OTHER forward/act-now signal on the
// card is already reconciled against the plan epic's own terminal status, and
// the ⛔ badge was the LAST one that was not.
//
// The badge is the #2 attention tier — a HARDER stop than a ⚠ stall, "a
// privileged operator must act before the blocked work can move" — so it reads
// as a loud, board-topping "go act NOW" call. On a FINISHED (closed) or
// intentionally-parked (deferred) plan that is a card-vs-truth contradiction: a
// leftover un-closed child step can still carry an open `blocks`-edged
// escalation, but the plan those steps belong to is over, so approving the
// escalation advances nothing — the exact false "act now" iter-25 killed for the
// now/next chip + Dispatch button, iter-30 for the crew live-pulse, and iter-27/28
// for the lifecycle chip. Live proof (2026-08-08 pour): ka-0000000539 (closed
// 2026-08-05) rendered "⛔ blocked by escalation ×3 · merge MR · 3d" — a loud
// act-now badge on a done plan whose 3 escalations block only its leftover open
// steps.
//
// Suppression is DISPLAY-ONLY and surgical: it gates the FACE render, exactly
// like the iter-26 stall ShowBadge. The raw Escalations.Count is UNTOUCHED, so
// the attention ranker (planAttention, plans_attention.go) and the board lane
// (planBoardLaneKey, plans_board.go) — both of which key off Escalations.Count —
// see the true state and behave byte-identically (a closed plan is caught by the
// lane's `case "closed"` first regardless, and its attention tier is moot once it
// sits in the Closed lane). The record is preserved as honest history the same
// way the closed plan's task-grid + "N/N steps · 100%" remain, and the cockpit
// banner (plan_escalations partial) still lists the escalations on the detail
// page — this reconciles only the loud board-scan badge. Pure over the two row
// fields, so it is trivially table-testable and the page render + the single-row
// re-render (renderPlanRowWith → planIndexEnrich) can never disagree.
func (r planRowView) ShowEscalationBadge() bool {
	return r.Escalations.Count > 0 && !planStatusTerminal(r.Status)
}

// planEscalationStaleThreshold is the wait-age past which the "blocked by
// escalation" badge tints loud (ka-0000003887, standing plan ka-0000005352
// iteration 8). An open blocking escalation is a HARDER stop than an open
// decision — a privileged operator must act before ANY of the blocked work can
// move — so the staleness bar is set tighter than the decision chip's 14 days:
// a privileged approval that has sat a full week unanswered is squarely into
// "someone forgot about this" territory (the measured backlog carried 53 open
// escalations, decision ka-0000000932). The badge renders the oldest wait-age
// unconditionally once derivable; crossing this threshold adds the visual "pop"
// so an aged, work-blocking escalation can't hide behind a bare count.
const planEscalationStaleThreshold = 7 * 24 * time.Hour

// OldestAge is the humanized wait-age of the OLDEST open blocking escalation
// ("40d"), folded into the "blocked by escalation" badge so the #2 attention
// signal carries temporal urgency, not just a count (ka-0000003887). The
// escalation refs planEscalationsBulk builds are ALREADY sorted oldest-first
// (age IS urgency — the ranker's planAttentionEscalation comment), each hydrated
// with its createdAt off the SAME bead batch that counts them, so this is a pure
// projection with zero extra daemon call. Empty when the plan has no escalation
// or the oldest ref's stamp is unset (a fresh escalation with a zero CreatedAt
// then reads exactly as before — count only), computed against now so a
// test/handler shares one clock. Mirrors planRowView.OldestDecisionAge.
func (i planEscalationInfo) OldestAge(now time.Time) string {
	if len(i.Escalations) == 0 {
		return ""
	}
	return fmtTimeAgo(i.oldestCreatedAt(), now)
}

// OldestStale reports whether the OLDEST open blocking escalation has waited past
// planEscalationStaleThreshold — the mark that tints the badge loud so an aged,
// work-blocking escalation pops out of the board instead of hiding behind a bare
// count. False for a fresh escalation, an empty list, or an unset stamp (a plan
// with no datable escalation is never "stale"). Mirrors OpenDecisionStale.
func (i planEscalationInfo) OldestStale(now time.Time) bool {
	oldest := i.oldestCreatedAt()
	if oldest.IsZero() {
		return false
	}
	return now.Sub(oldest) >= planEscalationStaleThreshold
}

// oldestCreatedAt returns the creation instant of the oldest open blocking
// escalation. The refs are sorted oldest-first (planEscalationsBulk), so the
// head is the oldest — but a zero-stamp ref sorts LAST there, so a list whose
// only stamped refs sit behind unstamped ones would read the head's zero. Guard
// by scanning for the earliest non-zero stamp rather than trusting refs[0], so
// the age reflects the oldest REAL wait even when a stamp is missing. Returns
// zero when the list is empty or every stamp is unset.
func (i planEscalationInfo) oldestCreatedAt() time.Time {
	var oldest time.Time
	for _, e := range i.Escalations {
		oldest = earlierNonZeroTime(oldest, e.createdAt)
	}
	return oldest
}

// OldestOperationLabel is the compact humanized operation-type of the OLDEST
// open blocking escalation, folded onto the ⛔ badge FACE (ka-0000005352
// iteration 37) so the #2 attention tier finally names WHAT the operator must
// DO — the badge's real discriminator — without hovering the tooltip. The
// escalation badge is a HARDER stop than ⚠ stalled (a privileged operator must
// act before ANY of the blocked work can move), yet it was the last
// attention-tier badge whose FACE still hid its whole discriminator: iter-8
// folded on its wait-age ("· 13h"), but every badge still read an identical
// "⛔ blocked by escalation" whether the remediation was a ~5-second
// gitlab_mr_merge approve or a real terraform_apply / aws_iam infra action
// (measured live: 22 cards carrying the badge, gitlab_mr_merge the single
// largest family). This is the SAME "fold the discriminator onto the badge
// face" density win the stall badge got in iter-10 (BadgeLabel "· no gardener").
//
// It reads the oldest ref's operation_type — the ref pinned to the head of the
// oldest-first sort planEscalationsBulk applies, matching the wait-age the face
// already shows, so the two facets describe the SAME escalation. Empty when the
// plan has no escalation, the oldest ref carries no operation_type (a legacy
// escalation then reads exactly as iter-8 — age + count only), or the type is
// unrecognized-and-noisy (humanizeEscalationOp returns "" for anything it can't
// shorten to a scannable face, so an unknown type never dumps a raw snake_case
// token onto the card). Zero extra daemon call — operation_type is already
// hydrated on every ref (planEscalationsBulk addRef).
func (i planEscalationInfo) OldestOperationLabel() string {
	oldest := i.oldestRef()
	if oldest == nil {
		return ""
	}
	return humanizeEscalationOp(oldest.OperationType)
}

// oldestRef returns the oldest open blocking escalation ref (earliest non-zero
// createdAt), or nil when the list is empty. Mirrors oldestCreatedAt's
// earliest-real-stamp rule so OldestOperationLabel names the SAME escalation the
// wait-age (OldestAge) does even when a stamp is missing: a zero-stamp ref never
// wins over a stamped one, and an all-unstamped list falls back to the first
// ref (planEscalationsBulk's stable oldest-first order).
func (i planEscalationInfo) oldestRef() *planEscalationRef {
	var oldest *planEscalationRef
	for idx := range i.Escalations {
		e := &i.Escalations[idx]
		switch {
		case oldest == nil:
			oldest = e
		case e.createdAt.IsZero():
			// An unstamped ref never displaces a stamped one.
		case oldest.createdAt.IsZero(), e.createdAt.Before(oldest.createdAt):
			oldest = e
		}
	}
	return oldest
}

// humanizeEscalationOp maps a raw escalation operation_type to a compact,
// scannable badge-face label — three words or fewer — or "" for a type it can't
// shorten meaningfully. operation_type is FREE-TEXT (never a closed enum — see
// the escalation-beads review, docs/research/escalation-beads-review-2026-05-05.md),
// so this humanizes the families that actually occur on the live board (a fleet
// snapshot found gitlab_mr_merge/gitlab_merge dominant, then gitlab_admin /
// manual_intervention / aws_iam / infrastructure / credential_* / k8s_rbac /
// terraform_apply) and normalizes their spelling drift (gitlab-mr-approve-merge,
// merge_request, deployment, infrastructure_change, …). An unrecognized type
// returns "" rather than a raw snake_case token, so a novel operation_type
// leaves the badge reading exactly as iter-8 (age + count) instead of dumping
// "some_new_op_type" onto the card — the fail-quiet default the whole escalation
// signal already takes (a false face here would misdescribe a hard stop). Pure;
// case-insensitive; separator-insensitive (— and _ normalized). The label is a
// WHAT-to-do verb-phrase ("merge MR", "grant AWS", "run terraform"), not a
// re-statement of the operation_type, so it reads as an instruction on the face.
func humanizeEscalationOp(op string) string {
	norm := strings.ToLower(strings.TrimSpace(op))
	norm = strings.ReplaceAll(norm, "-", "_")
	switch norm {
	case "":
		return ""
	case "gitlab_mr_merge", "gitlab_merge", "merge_request", "merge_approval",
		"gitlab_mr_approval", "gitlab_mr_approve_merge":
		return "merge MR"
	case "gitlab_admin":
		return "GitLab admin"
	case "aws_iam":
		return "AWS IAM"
	case "aws_ec2", "aws_eks":
		return "AWS access"
	case "terraform_apply":
		return "terraform"
	case "k8s_rbac", "k8s_admission_control":
		return "K8s access"
	case "credential_provisioning", "credential_rotation", "vault_provisioning":
		return "credential"
	case "deploy", "deploy_release", "deployment":
		return "deploy"
	case "db_access":
		return "DB access"
	case "ticket_management", "jira_edit", "jira_admin_delete":
		return "JIRA"
	case "slack_admin":
		return "Slack admin"
	case "trigger_lifecycle":
		return "trigger"
	case "manual_intervention", "infrastructure", "infrastructure_change",
		"configuration", "other":
		// Real hard stops, but the type names no specific action beyond
		// "a human must do something" — which the ⛔ badge already says. Leave
		// the face age-only rather than adding a vague "· manual" that carries
		// no more signal than the badge headline itself.
		return ""
	}
	return ""
}

// planEscalationsBulk resolves the open blocking escalations for every plan in
// idsByPlan — a map of plan epic id → the candidate blocked-work ids to walk
// (the plan's rendered step ids plus the epic id itself). An escalation reaches
// a plan through EITHER of two paths, unioned and deduped per plan by bead id:
//
//   - PATH 1 — an open forward `blocks` edge from one of the plan's candidate
//     ids. Two batched daemon calls across ALL plans: one forward
//     BatchGetDependencies over the union of candidate ids, one BatchGetBeads
//     over the distinct blocks-targets. This is the edge gb escalation create
//     --requesting-task writes; it survives only while the blocked step stays
//     open and parented.
//   - PATH 2 — the requesting_plan pin (Layer 2 of the orphan fix,
//     ka-0000007801 / design ka-0000006833 §3): OPEN escalations whose
//     fields.requesting_plan names a plan on this page. gb escalation create
//     stamps that field to the nearest plan-labeled epic ancestor at CREATE
//     time (escalation.go), so an escalation born inside the plan stays homed to
//     it even after its origin step closes or is reparented — the exact case
//     PATH 1 loses. Resolved via ONE page-wide open-escalation query bucketed by
//     the pin, so it is a SINGLE bounded round-trip regardless of the plan count
//     (the index enrich budget is plan-count invariant).
//
// Targets/pins that are non-closed type=escalation beads become refs, deduped
// per plan and sorted oldest-first. Best-effort: a daemon error on either path
// degrades that path to empty (unlike the stall badge, a false positive here
// would shout a hard stop that doesn't exist).
func (s *Server) planEscalationsBulk(ctx context.Context, idsByPlan map[string][]string) map[string]planEscalationInfo {
	out := make(map[string]planEscalationInfo, len(idsByPlan))
	if len(idsByPlan) == 0 {
		return out
	}

	// Per-plan accumulators persisted across BOTH paths so an escalation
	// surfaced by either (or both, deduped) lands in one refs list.
	refsByPlan := make(map[string][]planEscalationRef, len(idsByPlan))
	seenByPlan := make(map[string]map[string]struct{}, len(idsByPlan))
	for planID := range idsByPlan {
		seenByPlan[planID] = make(map[string]struct{})
	}
	addRef := func(planID string, b *beadsapi.BeadDetail) {
		if b == nil || b.Type != "escalation" || b.Status == "closed" {
			return
		}
		seen := seenByPlan[planID]
		if _, dup := seen[b.ID]; dup {
			return // one escalation blocking two steps / pinned + blocking counts once
		}
		seen[b.ID] = struct{}{}
		refsByPlan[planID] = append(refsByPlan[planID], planEscalationRef{
			ID:            b.ID,
			Title:         b.Title,
			OperationType: b.Fields["operation_type"],
			Sensitivity:   b.Fields["sensitivity"],
			Audience:      beadsapi.EscalationAudience(b.Fields),
			Ago:           humanizeTime(b.CreatedAt),
			ReviewURL:     s.buildEscalationReviewURL(b.ID),
			createdAt:     b.CreatedAt,
		})
	}

	// PATH 1 — the forward blocks-edge walk.
	union := make([]string, 0, len(idsByPlan)*4)
	unionSeen := make(map[string]struct{})
	for _, ids := range idsByPlan {
		for _, id := range ids {
			if id == "" {
				continue
			}
			if _, dup := unionSeen[id]; dup {
				continue
			}
			unionSeen[id] = struct{}{}
			union = append(union, id)
		}
	}
	if len(union) > 0 {
		depsByID, err := s.daemon.BatchGetDependencies(ctx, union, "forward")
		if err != nil {
			s.logger.Debug("plan escalations: blocks walk", "error", err)
		} else {
			targets := make([]string, 0)
			targetSeen := make(map[string]struct{})
			for _, id := range union {
				for _, d := range depsByID[id] {
					if d.Type != "blocks" || d.DependsOnID == "" {
						continue
					}
					if _, dup := targetSeen[d.DependsOnID]; dup {
						continue
					}
					targetSeen[d.DependsOnID] = struct{}{}
					targets = append(targets, d.DependsOnID)
				}
			}
			if len(targets) > 0 {
				blockers, berr := s.daemon.BatchGetBeads(ctx, targets)
				if berr != nil {
					s.logger.Debug("plan escalations: blocker beads", "error", berr)
				} else {
					for planID, ids := range idsByPlan {
						for _, id := range ids {
							for _, d := range depsByID[id] {
								if d.Type != "blocks" || d.DependsOnID == "" {
									continue
								}
								addRef(planID, blockers[d.DependsOnID])
							}
						}
					}
				}
			}
		}
	}

	// PATH 2 — the requesting_plan pin. ONE page-wide open-escalation query,
	// bucketed into the plans on this page by fields.requesting_plan. Runs
	// UNCONDITIONALLY (even when PATH 1 found nothing), since a plan may have a
	// pinned escalation with no blocks-edge at all — the orphan case.
	pinned, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"escalation"},
		Statuses: []string{"open"},
		Limit:    escalationPinListLimit,
	})
	if err != nil {
		s.logger.Debug("plan escalations: requesting_plan pin", "error", err)
	} else if pinned != nil {
		for _, b := range pinned.Beads {
			if b == nil {
				continue
			}
			planID := b.Fields["requesting_plan"]
			if planID == "" {
				continue
			}
			if _, onPage := seenByPlan[planID]; !onPage {
				continue // pinned to a plan not on this page
			}
			addRef(planID, b)
		}
	}

	// Assemble: sort each non-empty plan's refs oldest-first (age is urgency).
	for planID, refs := range refsByPlan {
		if len(refs) == 0 {
			continue
		}
		sort.SliceStable(refs, func(i, j int) bool {
			// Oldest first — age is urgency; zero CreatedAt sorts last.
			if refs[i].createdAt.IsZero() {
				return false
			}
			if refs[j].createdAt.IsZero() {
				return true
			}
			return refs[i].createdAt.Before(refs[j].createdAt)
		})
		out[planID] = planEscalationInfo{Count: len(refs), Escalations: refs}
	}
	return out
}

// planEscalationWalkIDs assembles one plan's candidate blocked-work id set for
// planEscalationsBulk: the epic itself plus its rendered step ids. Shared by
// the detail page (from its materialized timeline) and the SSE fragment path.
func planEscalationWalkIDs(epicID string, steps []planStepView, phases []planPhaseView) []string {
	ids := make([]string, 0, len(steps)+8)
	ids = append(ids, epicID)
	for _, st := range steps {
		ids = append(ids, st.ID)
	}
	for _, p := range phases {
		for _, st := range p.Steps {
			ids = append(ids, st.ID)
		}
	}
	return ids
}

// collectRenderedStepIDs walks the SAME batched child data renderedStepRollup
// walks (phase grandchildren in phase_order, then unphased direct children)
// and returns the rendered step ids — the blocked-work candidate set for the
// index-page escalation aggregation. Kept as a sibling of renderedStepRollup /
// collectInProgressStepRefs so all three mirror the timeline's membership
// rules exactly.
func collectRenderedStepIDs(order, directChildIDs []string, grandchildIDsBySub map[string][]string, direct, grandchildren map[string]*beadsapi.BeadDetail) []string {
	var ids []string
	phaseStepIDs := make(map[string]struct{})
	phaseSet := make(map[string]struct{}, len(order))
	appendIssueIDs := func(cands []string, beads map[string]*beadsapi.BeadDetail, record map[string]struct{}) {
		for _, id := range cands {
			c := beads[id]
			if c == nil || !isIssueType(c.Type) {
				continue
			}
			if record != nil {
				record[id] = struct{}{}
			}
			ids = append(ids, id)
		}
	}
	for _, pid := range order {
		c := direct[pid]
		if !isPhaseContainer(c) {
			continue
		}
		if _, dup := phaseSet[pid]; dup {
			continue
		}
		phaseSet[pid] = struct{}{}
		appendIssueIDs(grandchildIDsBySub[pid], grandchildren, phaseStepIDs)
	}
	for _, cid := range directChildIDs {
		if _, isPhase := phaseSet[cid]; isPhase {
			continue
		}
		if _, inPhase := phaseStepIDs[cid]; inPhase {
			continue
		}
		appendIssueIDs([]string{cid}, direct, nil)
	}
	return ids
}
