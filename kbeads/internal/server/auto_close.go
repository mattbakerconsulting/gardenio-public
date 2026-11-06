package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// Auto-close of completed parents (feature ka-0000001587).
//
// Parent epics and escalation-wrapper tasks are routinely left OPEN after all
// their children merge / their blocker escalation executes. That makes finished
// plans read as stalled on /plans, clutters the frontier, and burns gardener
// cycles re-verifying "is anything left here?". This file wires two daemon-side
// auto-transitions at the single bead-close choke point (afterBeadClosed, called
// from every CloseBead site) so no API caller can bypass them:
//
//  1. maybeAutoCloseParents — when ANY bead closes, walk UP its parent-child
//     edges to each parent epic and, if that epic's whole subtree is drained
//     (every issue-kind descendant closed AND any MR-linked work merged),
//     transition the epic. The walk runs regardless of the closed bead's kind:
//     a non-issue-kind child (e.g. a type=trigger, kind=data gardener bead) can
//     be the LAST child to close, and gating the walk on issue-kind left the
//     epic open forever in that case (ka-0000001346). A NON-plan epic is hard-closed; a PLAN epic
//     (one carrying fields.plan_state) is FLAGGED-for-close instead — the
//     `autoclose-ready` label plus an audit comment — because a plan's terminal
//     close is the operator's deliberate "Mark complete" gate (plan_state →
//     complete, POST /plans/{id}/complete). Auto-closing a plan epic would skip
//     that gate, so we surface readiness and let the operator (or plan gardener)
//     pull the trigger.
//
//  2. maybeCloseEscalationWrappers — when an ESCALATION closes, close the task
//     that (a) is its requesting_task, blocked-by it via a `blocks` edge, AND
//     (b) opts into the contract (the escalation's close_requesting_task field,
//     or the legacy `close-on-escalation-execute` label). Other beads blocked-by
//     the escalation are only waiting on it and are left alone. Those
//     wrapper tasks declare in prose "this task closes when the escalation
//     executes"; the label makes that contract machine-enforced. A requesting
//     task with no `blocks` edge closes only when the escalation's resolver
//     may close it by hand, and a result saying merged=false closes nothing
//     (gd-MgicCcY2rj).
//
//  3. maybeCloseMergedLeaves — when a MERGED merge-request bead closes, close the
//     LEAF issue beads (task/bug/feature/chore — never an epic) that produced it,
//     gated on EVERY MR each leaf links being merged (never first-merge-wins).
//     Steps 1-2 only ever transition PARENTS, so a non-epic bead whose own MR
//     merged had no close-on-merge path and stayed open forever, churning in
//     `gb ready` every gardener cycle (ka-0000005910). An epic reached from an MR
//     bead is left to step 1's subtree-drain — close-on-merge is leaf-only.
//
//  4. maybeParkDecisionGatedBeads — when a DECISION closes (resolved or
//     dismissed), transition the still-open beads it was gating (each carrying a
//     `blocks`→decision edge) to `blocked`. A closed decision's `blocks` edge no
//     longer suppresses its bead in FilterNoOpenDeps, so the gated work fell back
//     into `gb ready` the instant the operator answered and the next bulk-groomer
//     re-laned it — the fix for the intake thrash was itself the cause of the next
//     thrash (ka-0000005620). Parking automates the proven manual workaround. The
//     `blocks` edge is the agent's own "this bead is gated" statement, so no
//     opt-in label is required; a bead opts OUT with `release-on-decision-close`.
//
//  5. maybeCloseBlockedBugsOnFixMerge — when a bead closes, close every OPEN bug
//     that (a) is blocked-by it via a `blocks` edge, (b) opts into the contract
//     with the `close-on-blocking-fix-merge` label, AND (c) has EVERY one of its
//     blocking beads closed with at least one carrying a MERGED MR. This closes
//     the gap the CED remediation lane exposed (ka-0000003106): that lane fixes a
//     bug not in place but via a SCOPED FIX-TASK that `blocks` the bug, with the
//     MR linked to the fix-task. When the fix merges the fix-task closes, but the
//     bug does NOT — steps 1-4 only follow parent-child (drain) or the
//     escalation/decision-specific reverse edges, never a bug's `blocks`→fix-task.
//     So a merged-and-fixed bug sat open indefinitely, re-offered as a ready
//     candidate every gardener cycle until closed manually. Like the
//     escalation-wrapper contract (step 2), the `blocks` edge is overloaded
//     ("this fixes the bug" vs "this must precede the bug"), so an OPT-IN label on
//     the bug — not a bare blocked-by relationship — is required, keeping the
//     transition inert until a lane deliberately opts a bug in (zero fleet-wide
//     blast radius). The merged-MR gate (c) is what distinguishes a real merged
//     fix from a blocker that closed wontfix/duplicate: an all-blockers-closed bug
//     whose blockers carried NO merged MR still needs a real fix and is left open.
//
// SHARED ENUMERATION (honoring the ka-0000008241 note): the subtree drain check
// reuses the SAME authoritative reverse-dep enumeration as the meta-gardener
// stop guard — model.BeadFilter.ParentSubtreeID, the cycle-safe recursive CTE
// (internal/store/postgres/queries.go) that `kd list --parent` / the gardener
// `parent=` query walk. openSubtreeChildTypes / openSubtreeChildStatuses mirror
// gb gardener stop's gardenerStopOpenChildTypes / gardenerStopOpenChildStatuses so
// the two features can never disagree on "what keeps an epic live".
//
// All steps are BEST-EFFORT: every failure path logs and returns without
// disturbing the close that triggered it. An auto-transition is a convenience,
// never a precondition for the originating close to succeed.

// autoCloseReadyLabel flags a plan epic whose subtree has fully drained but
// whose terminal close is gated on the operator's deliberate "Mark complete"
// action. The /plans cockpit and plan gardeners can surface / act on it; the
// daemon never force-closes a plan epic out from under that gate.
const autoCloseReadyLabel = "autoclose-ready"

// closeOnEscalationLabel is the DEPRECATED opt-in machine contract for an
// escalation wrapper task: "close this task when its blocker escalation
// executes". It is stamped on the TASK, which makes it sticky — once a task is
// ever wrapped by a --close-on-execute escalation, the label never comes off,
// so ANY LATER escalation's success (even one filed without the flag) closes
// the task, regardless of that later escalation's own intent (bug
// gd-91b7zMOR2z, observed on gd-Kebd0XJHZ2/#39 among others: a CONDITIONAL
// review verdict auto-closed a task before its PR merged). closeRequestingTaskField
// below is the durable replacement — the opt-in now lives on the ESCALATION,
// not the task, so it can never outlive the specific escalation that set it.
// This label is read only as a ONE-RELEASE BACK-COMPAT FALLBACK, for an
// escalation created before the fix (one that carries no
// closeRequestingTaskField at all) — see maybeCloseEscalationWrappers.
const closeOnEscalationLabel = "close-on-escalation-execute"

// closeRequestingTaskField is the opt-in machine contract for an escalation
// wrapper task, stored on the ESCALATION bead itself (gd-91b7zMOR2z fix a):
// "close the requesting task when THIS escalation executes". `gb escalation
// create --close-on-execute` stamps this field "true" (and its absence-of-flag
// path stamps "false") on every escalation it creates, so the opt-in is scoped
// to the one escalation that declared it and can never leak onto an unrelated
// later escalation against the same task — unlike the legacy task label,
// which persisted across every escalation that ever touched the task.
const closeRequestingTaskField = "close_requesting_task"

// neverCascadeCloseOperationTypes are escalation operation_type values whose
// success must NEVER auto-close the requesting task, regardless of
// closeRequestingTaskField or the legacy label (gd-91b7zMOR2z fix c).
// `github_pr_merge` is the agent-review epic's reviewer-verdict escalation
// (gd-3UQKfj0nA8): a reviewer's `result_status=success` records ITS VERDICT
// (e.g. "mergeable once rebased and green" — a CONDITIONAL pass), not that the
// PR has merged. The PR merge is the actual terminal event for that task
// ("NOT DONE UNTIL MERGED"), so cascading on the verdict closes the task while
// the PR is still open. Checked case-insensitively, matching operation_type
// being free-text (docs/research/escalation-beads-review-2026-05-05.md).
//
// The pure verdict types github_pr_review and doc_review never cascade either
// (spec D9, P7 Task 14). gitlab_mr_merge is a verdict type too
// (isVerdictOperation) but deliberately cascades: `gb pr ready` files it with
// --close-on-execute, and the desk's success result after the merge is what
// closes the requesting task (escalationResultSaysNotMerged guards a
// success that did not merge).
var neverCascadeCloseOperationTypes = map[string]bool{
	"github_pr_merge":  true,
	"github_pr_review": true,
	"doc_review":       true,
}

// autoCloseActor is the closed_by / comment author attributed to a daemon
// auto-transition, matching the `system:` sentinel convention used by
// audit-retention (system:audit-retention).
const autoCloseActor = "system:auto-close"

// decisionGateActor is the actor attributed to the daemon parking a
// decision-gated bead when its gating decision closes (ka-0000005620). Distinct
// from autoCloseActor so the audit trail names the transition it performed
// (park-to-blocked, not close), matching the `system:` sentinel convention.
const decisionGateActor = "system:decision-gate"

// closeOnBlockingFixMergeLabel is the opt-in machine contract for a bug fixed
// out-of-band by a SCOPED FIX-TASK that `blocks` it (the CED remediation lane
// shape, ka-0000003106): "close this bug when every blocking fix-task is closed
// AND the fix merged". The bug's `blocks`→fix-task edge alone is NOT enough —
// `blocks` is overloaded (a fix-that-resolves vs an ordering must-precede), so
// as with closeOnEscalationLabel the lane must stamp this label to arm the
// close. Absent it, a bug merely blocked-by some task is left for its owner.
const closeOnBlockingFixMergeLabel = "close-on-blocking-fix-merge"

// decisionGateReopenLabel opts a decision-gated bead OUT of the automatic
// park-to-blocked on decision close. A bead carrying it is left in whatever
// status it holds when its gating decision resolves — for the (rare) case where
// resolving the decision is meant to RELEASE the bead straight into the ready
// lane rather than park it for follow-up triage. Absent the label, the daemon
// parks (the safe, thrash-free default).
const decisionGateReopenLabel = "release-on-decision-close"

// mergeRequestType is the canonical type of a first-class merge-request bead
// (config.go type:merge-request). A leaf issue bead links to it via a
// `produces` edge, and the bridge closes it (mr_outcome=merged) when its MR
// merges — the event that fires leaf close-on-merge (ka-0000005910).
const mergeRequestType = model.BeadType("merge-request")

// producesDepType is the dep edge from an issue bead to the merge-request bead
// it produced (issue --produces--> merge-request). The reverse of this edge,
// walked from a just-merged MR bead, yields the leaf issue beads to consider
// for close-on-merge. Mirrors beadsapi.ProducingTaskBeadsForMR.
const producesDepType = model.DependencyType("produces")

// leafCloseOnMergeTypes is the set of issue-kind LEAF types eligible for
// close-on-merge. Epics are DELIBERATELY excluded: an epic legitimately stays
// open until its whole subtree drains (that is drainParentEpic's job, gated on
// closed descendants + merged MRs), and the audit behind ka-0000005910 found 7
// merged-MR epics that are correctly still open. Close-on-merge is a LEAF-only
// transition; an epic reached from an MR bead is handled by the parent walk,
// never here.
var leafCloseOnMergeTypes = map[model.BeadType]bool{
	model.TypeTask:    true,
	model.TypeBug:     true,
	model.TypeFeature: true,
	model.TypeChore:   true,
}

// openSubtreeChildTypes is the set of issue-kind types whose open members keep
// a parent epic "live". Mirrors gb gardener stop's gardenerStopOpenChildTypes
// (gardenio/controller/cmd/gb/gardener.go) so the auto-close drain check and the
// gardener-stop guard agree bead-for-bead on remaining work. Bundles (a plan's
// type=bundle narrative child) are intentionally excluded: a bundle is a doc
// container, not a unit of work, and never closes on its own.
var openSubtreeChildTypes = []model.BeadType{
	model.TypeTask, model.TypeBug, model.TypeEpic, model.TypeFeature, model.TypeChore,
}

// openSubtreeChildStatuses are the non-terminal statuses that mark a child as
// still-open work. Mirrors gardenerStopOpenChildStatuses: deferred is omitted (a
// deferred child is parked, not active, so it must not hold the parent open).
var openSubtreeChildStatuses = []model.Status{
	model.StatusOpen, model.StatusInProgress, model.StatusBlocked,
}

// liveGardenerChildTypes are the gardener-carrying trigger types whose OPEN +
// enabled members keep a parent epic live REGARDLESS of whether every
// issue-kind descendant has closed. They are deliberately absent from
// openSubtreeChildTypes (Gate A) — a trigger/schedule is data-kind, not a unit
// of work — so Gate A alone cannot see them, and a mid-loop gardener's epic was
// hard-closed the instant its last audit step drained (bug ka-0000001525,
// observed twice in one day on the Users-CAMP lane).
//
// Why OPEN + enabled == "live gardener, terminal goal unmet": a gardener that
// reaches its terminal goal CLOSES ITS OWN trigger bead — gardenerTerminate ->
// CloseBead (gardenio gardener_evaluator.go). So a still-open trigger has, by
// construction, NOT reached its goal. The trigger-engine reads liveness as
// `fields.enabled != "false"` (triggers.go evalOne), which firstLiveGardenerChild
// mirrors exactly: only an explicitly-disabled trigger is inert.
var liveGardenerChildTypes = []model.BeadType{
	model.BeadType("trigger"),
	model.BeadType("schedule"),
}

// noAutoCloseLabel is an explicit opt-out an epic (or any continual-improvement
// lane) can stamp to suppress the daemon hard-close entirely, mirroring how a
// plan epic's fields.plan_state downgrades the close to a flag. One
// `kd update <epic> -l no-auto-close` lets a periodic-pour lane self-protect
// without depending on the live-gardener heuristic (bug ka-0000001525).
const noAutoCloseLabel = "no-auto-close"

// closedKidsPageSize is the page window for the closed-descendant enumeration in
// drainParentEpic. A single-shot `Limit: 500` fetch silently truncated at 500
// (bug ka-0000008187): firstUnmergedMR (Gate C) only inspected the first 500
// closed descendants, so an unmerged MR on descendant #501+ was ignored and the
// epic auto-closed with unmerged work still on a branch. Paging removes the cap
// while keeping peak memory bounded to one page (advice ka-0000003555 — a big
// Limit is both an OOM and a silent-truncation bug). 500 mirrors the walk in
// advice.FleetArchetypesPaged.
const closedKidsPageSize = 500

// closedKidsMaxPages caps the paged closed-descendant walk so a store that
// ignores Offset (or a pathologically huge epic) cannot spin forever. 200 pages
// × 500 = 100k closed descendants — far beyond any real epic's subtree, but
// finite. Mirrors advice.fleetArchetypeMaxPages.
const closedKidsMaxPages = 200

// afterBeadClosed runs the daemon-side auto-transitions triggered by a bead
// close. It is the single choke point invoked from every server-layer CloseBead
// call site (CloseBead RPC, handleCloseBead, updateBead status→closed,
// escalation approve/reject/result, decision resolve + cancel). closedBead is
// the freshly-closed bead as returned by store.CloseBead / updateBead.
//
// The cascade PROPAGATES: an epic this step auto-closes re-enters here so its
// own parent (e.g. a plan epic whose last phase just drained) is drained in
// turn, and a wrapper it closes drains that wrapper's parent epic. Termination
// is guaranteed because the parent-child graph is acyclic (the dep-add cycle
// guard, ka-0000006899) and every already-closed parent is skipped, so the
// upward walk strictly shrinks the open frontier.
//
// Best-effort: this never returns an error and never blocks the originating
// close. A nil bead / nil server / nil store short-circuits.
func (s *BeadsServer) afterBeadClosed(ctx context.Context, closedBead *model.Bead) {
	if s == nil || s.store == nil || closedBead == nil {
		return
	}

	// Park orphaned children FIRST, before any type-specific transition
	// (ka-0000000504). A bead of ANY type may be a parent-child parent, and
	// closing it out from under its still-open children silently strands them:
	// they keep status=open with a dead parent and stay claimable in `gb ready`
	// forever, because the parent walk below only DRAINS a parent when its
	// subtree is already empty — it never touches the children of the bead being
	// closed. maybeAutoCloseParents' `parent.Type != TypeEpic` skip compounds it:
	// a closed task/bug/molecule parent's children are doubly invisible. This
	// down-walk is the close-path counterpart of the edge-add closed-parent guard
	// (ka-0000005756), which only covers a NEW edge onto an already-closed parent.
	s.maybeParkOrphanedChildren(ctx, closedBead)

	// Taint propagation §5.2 enforcement (gd-Kebd0XJHZ2): an agent bead's
	// close is the actual bound on any taint_grants row assignment
	// propagation wrote for it (resolveAssignTaints / applyPendingAssignGrants)
	// — revoke them now rather than relying solely on the stored fallback TTL.
	// Runs unconditionally alongside whichever type-specific case below fires.
	if closedBead.Type == "agent" {
		s.revokeAssignTaintGrants(ctx, closedBead)
	}

	// A gardener idled on this bead wakes now (gardener_self_idle.go).
	s.wakeGardenersAwaiting(ctx, closedBead)

	switch closedBead.Type {
	case model.TypeEscalation:
		// An executed escalation releases its close-on-escalation wrapper tasks.
		s.maybeCloseEscalationWrappers(ctx, closedBead)
	case model.TypeDecision:
		// A resolved/dismissed operator decision PARKS the work it was gating.
		// A gated bead carries a `blocks`→decision edge (the exact edge
		// `gb decision create --requesting-task` writes), and FilterNoOpenDeps
		// only suppresses it while the blocker is non-terminal — so the instant
		// the operator closes the decision, the bead falls back into `gb ready`
		// and the next bulk-groomer re-lanes it. Parking each still-open gated
		// bead at `blocked` automates the proven manual workaround (ka-0000005620).
		// The parent walk still runs so a decision parented under an epic can
		// drain it as the last child, exactly as before.
		s.maybeParkDecisionGatedBeads(ctx, closedBead)
		s.maybeAutoCloseParents(ctx, closedBead)
	case mergeRequestType:
		// A MERGED merge-request bead closes the LEAF issue beads that produced
		// it (leaf close-on-merge, ka-0000005910). Before this, only PARENT epics
		// were ever auto-transitioned: a bug/task whose own MR merged stayed
		// status=open forever and churned in `gb ready` every gardener cycle. We
		// ALSO run the standard parent walk so an MR bead that is (unusually)
		// parented under an epic still drains it — preserving the pre-existing
		// default-case behavior for merge-request beads exactly, so epic drain is
		// unchanged.
		s.maybeCloseMergedLeaves(ctx, closedBead)
		s.maybeAutoCloseParents(ctx, closedBead)
	default:
		// Any closed bead may be the last remaining child of a parent epic, so
		// walk UP its parent-child edges regardless of kind. This deliberately
		// does NOT gate on Kind == KindIssue: non-issue-kind children — most
		// notably a `type=trigger, kind=data` gardener bead attached under an
		// issue-kind epic (the standard gardener-under-epic pattern) — are
		// routinely the LAST child to close, and gating the walk on issue-kind
		// left the epic open forever when they were (ka-0000001346). The walk is
		// safe for any kind: maybeAutoCloseParents filters to TypeEpic parents
		// and drainParentEpic re-enumerates the subtree from scratch, so it is
		// idempotent and cheap — a childless / non-epic / already-closed parent
		// is a fast no-op.
		s.maybeAutoCloseParents(ctx, closedBead)
		// A closed fix-task (type=task) is the trigger for the reverse-`blocks`
		// bug close: a bug fixed via a SCOPED FIX-TASK that `blocks` it is closed
		// once every blocking fix-task is closed and the fix merged (ka-0000003106).
		// This lives in the default case because a fix-task lands here on ALL three
		// of its close paths — manual close, escalation-wrapper close (step 2), and
		// leaf close-on-merge (step 3) — each of which re-enters afterBeadClosed
		// with the freshly-closed task.
		s.maybeCloseBlockedBugsOnFixMerge(ctx, closedBead)
	}
}

// maybeAutoCloseParents walks UP the parent-child edges of the just-closed bead
// to each direct parent epic and drains it when eligible. Only parent epics are
// considered (a task parent is not a container to close); each is deduped so a
// diamond in the graph is checked once.
func (s *BeadsServer) maybeAutoCloseParents(ctx context.Context, closedBead *model.Bead) {
	deps, err := s.store.GetDependencies(ctx, closedBead.ID)
	if err != nil {
		slog.Warn("auto-close: failed to fetch parent edges",
			"bead", closedBead.ID, "err", err.Error(), "feature", "ka-0000001587")
		return
	}

	seen := make(map[string]bool, len(deps))
	for _, d := range deps {
		if d == nil || !model.IsParentChildDepType(string(d.Type)) {
			continue
		}
		// child-first orientation: the closed bead is bead_id, the parent is
		// depends_on_id. Skip a self-edge and any parent already handled.
		parentID := d.DependsOnID
		if parentID == "" || parentID == closedBead.ID || seen[parentID] {
			continue
		}
		seen[parentID] = true

		parent, err := s.store.GetBead(ctx, parentID)
		if err != nil || parent == nil {
			continue
		}
		if parent.Type != model.TypeEpic || parent.Status == model.StatusClosed {
			continue
		}
		s.drainParentEpic(ctx, parent)
	}
}

// drainParentEpic checks whether epic's transitive subtree has fully drained —
// no open/in_progress/blocked issue-kind descendants remain AND every MR-linked
// descendant's work is merged — and, if so, transitions the epic: a plan epic is
// FLAGGED (autoclose-ready label + comment), a non-plan epic is hard-closed.
func (s *BeadsServer) drainParentEpic(ctx context.Context, epic *model.Bead) {
	// Reuse the SAME authoritative reverse-dep subtree enumeration as the
	// meta-gardener stop guard (ka-0000008241): ParentSubtreeID walks the
	// cycle-safe recursive CTE down from the epic, cross-project by construction
	// (no implicit project-label scoping at this layer).
	open, _, err := s.store.ListBeads(ctx, model.BeadFilter{
		ParentSubtreeID: epic.ID,
		Type:            openSubtreeChildTypes,
		Status:          openSubtreeChildStatuses,
		Limit:           500,
	})
	if err != nil {
		slog.Warn("auto-close: failed to enumerate epic subtree",
			"epic", epic.ID, "err", err.Error(), "feature", "ka-0000001587")
		return
	}
	if len(open) > 0 {
		// Still-live work under the epic — nothing to do.
		return
	}

	// Live-gardener guard (bug ka-0000001525): an OPEN + enabled trigger/schedule
	// descendant is a gardener still mid-loop with its terminal goal UNMET — a
	// gardener that reached its goal closes its own trigger. Gate A's type
	// allowlist (openSubtreeChildTypes) cannot see data-kind triggers, so without
	// this a continual-improvement lane's epic was hard-closed the moment its last
	// audit step drained, only to be reopened a gardener-cycle later. Defer while
	// any live gardener child remains: closing the epic makes the lane invisible to
	// /plans and to the gardener preflight that enumerates its own children, so the
	// loop can silently go dormant — the exact failure this class of gardener
	// exists to avoid.
	if live := s.firstLiveGardenerChild(ctx, epic.ID); live != "" {
		slog.Info("auto-close: epic subtree drained but a live gardener child remains; deferring",
			"epic", epic.ID, "live_gardener", live, "feature", "ka-0000001587", "bug", "ka-0000001525")
		return
	}

	// A childless epic (no parent-child descendants at all) is NOT a completed
	// plan — it is an empty/orphan container that never had work. Auto-closing
	// it would be the inverse of the intended behavior, so require at least one
	// closed issue-kind descendant to prove the epic actually held (and drained)
	// real work.
	//
	// Merged-work gate (Gate C): any descendant that linked an MR must have that
	// MR merged before the epic counts as done. Self-merge already gates on
	// CI-green + comments-resolved, so a merged MR is the authoritative "work
	// landed" signal. A descendant with an OPEN / non-merged MR blocks the drain
	// even though its bead is closed — the code is not actually on main yet.
	//
	// MR state lives on the canonical type=merge-request bead, NOT on the issue
	// bead: the gitlab-bridge stopped stamping mr_state/mr_merged onto issue
	// beads (taskBeadBanFields; ka-0000000586 / ka-0000008134), so reading them
	// off the issue bead sees them absent by design and defers forever
	// (ka-0000002273). Resolve through the MR bead exactly as gb/mr.go's
	// filterMRBeads does, keeping the issue-bead read as a legacy fallback.
	//
	// Both the childless guard and Gate C run over the FULL closed-descendant
	// set via a paged walk. The prior single-shot `Limit: 500` fetch silently
	// truncated Gate C at 500 (bug ka-0000008187): an unmerged MR on descendant
	// #501+ was invisible, so a >500-closed-descendant epic auto-closed with
	// unmerged work still on a branch. Paging removes the cap while keeping peak
	// memory to one page (advice ka-0000003555).
	scan, err := s.scanClosedDescendants(ctx, epic.ID)
	if err != nil {
		slog.Warn("auto-close: failed to enumerate closed subtree",
			"epic", epic.ID, "err", err.Error(), "feature", "ka-0000001587")
		return
	}
	if scan.firstUnmerged != "" {
		slog.Info("auto-close: epic subtree drained but has unmerged MR work; deferring",
			"epic", epic.ID, "unmerged_child", scan.firstUnmerged, "feature", "ka-0000001587",
			"bug", "ka-0000008187")
		return
	}
	if scan.count == 0 {
		return
	}

	// Explicit opt-out (bug ka-0000001525): an epic carrying the no-auto-close
	// label suppresses the hard close entirely. This is the belt-and-suspenders
	// escape hatch for a continual-improvement lane that does not want the daemon
	// closing it out from under a periodic pour — one `kd update` self-protects,
	// independent of the live-gardener heuristic above. Mirrors how a plan epic's
	// plan_state downgrades the close; here we simply leave the epic untouched.
	if hasLabel(epic.Labels, noAutoCloseLabel) {
		slog.Info("auto-close: epic subtree drained but carries no-auto-close; leaving open",
			"epic", epic.ID, "feature", "ka-0000001587", "bug", "ka-0000001525")
		return
	}

	// Distinguish a plan epic (operator-gated terminal close) from a plain
	// epic. A plan epic opted into the authoring lifecycle by carrying a
	// concrete fields.plan_state; its close is the operator's "Mark complete"
	// action, so we FLAG rather than force-close.
	if planStateFromFields(epic.Fields) != planStateUnset {
		s.flagPlanEpicReady(ctx, epic)
		return
	}

	s.autoCloseEpic(ctx, epic, scan.count)
}

// closedDescendantScan is the reduction the paged closed-descendant walk
// produces: how many closed/partial issue-kind descendants the epic has (the
// childless-epic guard), and the id of the first descendant carrying an
// unmerged MR (Gate C), or "" when every MR-linked descendant is merged.
type closedDescendantScan struct {
	count         int
	firstUnmerged string
}

// scanClosedDescendants walks the epic's closed/partial issue-kind descendants
// PAGE BY PAGE, folding each page into a count + the merged-work gate before
// dropping it. It replaces the single-shot `Limit: 500` fetch that silently
// truncated Gate C at 500 closed descendants (bug ka-0000008187): an unmerged
// MR on descendant #501+ was never inspected, so a large drained epic
// auto-closed with unmerged work still on a branch.
//
// Paging follows advice ka-0000003555: page size 500, sort by the immutable
// created_at so pages stay stable while beads are created underneath the walk,
// a max-page cap so a store that ignores Offset cannot spin forever, and each
// page is reduced then dropped so peak memory is one page — not the whole
// subtree. The merged-work gate short-circuits: the first unmerged MR found on
// any page ends the walk immediately (the epic will defer regardless of the
// remaining pages).
func (s *BeadsServer) scanClosedDescendants(ctx context.Context, epicID string) (closedDescendantScan, error) {
	var scan closedDescendantScan
	for page := range closedKidsMaxPages {
		closedKids, _, err := s.store.ListBeads(ctx, model.BeadFilter{
			ParentSubtreeID: epicID,
			Type:            openSubtreeChildTypes,
			Status:          []model.Status{model.StatusClosed, model.StatusPartial},
			Sort:            "created_at",
			Limit:           closedKidsPageSize,
			Offset:          page * closedKidsPageSize,
		})
		if err != nil {
			return closedDescendantScan{}, err
		}
		scan.count += len(closedKids)
		// Gate C over this page: the first unmerged MR anywhere in the subtree
		// defers the epic, so short-circuit the walk the moment one is found.
		if unmerged := s.firstUnmergedMR(ctx, closedKids); unmerged != "" {
			scan.firstUnmerged = unmerged
			return scan, nil
		}
		if len(closedKids) < closedKidsPageSize {
			break // short page = last page
		}
	}
	return scan, nil
}

// autoCloseEpic hard-closes a fully-drained non-plan epic, mirroring the event
// + mutation-history bookkeeping the CloseBead RPC performs so live subscribers
// and the audit trail observe the transition identically. The freshly-closed
// epic re-enters afterBeadClosed so its own parent (a super-epic / plan) drains
// in turn.
func (s *BeadsServer) autoCloseEpic(ctx context.Context, epic *model.Bead, closedKids int) {
	prevStatus := epic.Status
	closed, err := s.store.CloseBead(ctx, epic.ID, autoCloseActor)
	if err != nil || closed == nil {
		slog.Warn("auto-close: failed to close drained epic",
			"epic", epic.ID, "err", errString(err), "feature", "ka-0000001587")
		return
	}
	s.addAutoComment(ctx, epic.ID, fmt.Sprintf(
		"auto-closed: all %d issue-kind descendant(s) closed and MR-linked work merged (feature ka-0000001587).",
		closedKids))
	s.recordAndPublish(ctx, events.TopicBeadClosed, closed.ID, autoCloseActor, events.BeadClosed{
		Bead:     closed,
		ClosedBy: autoCloseActor,
	})
	s.recordMutation(ctx, closed.ID, autoCloseActor, "status",
		string(prevStatus), string(closed.Status), model.MutationTypeUpdate)
	slog.Info("auto-closed drained epic",
		"epic", epic.ID, "closed_children", closedKids, "feature", "ka-0000001587")
	// Cascade upward: a closed sub-epic may complete its parent super-epic/plan.
	s.afterBeadClosed(ctx, closed)
}

// flagPlanEpicReady marks a fully-drained plan epic as ready for the operator's
// deliberate completion, without closing it. Idempotent: a plan already
// carrying the flag is left untouched so a burst of child closes does not spam
// duplicate comments.
func (s *BeadsServer) flagPlanEpicReady(ctx context.Context, epic *model.Bead) {
	if hasLabel(epic.Labels, autoCloseReadyLabel) {
		return
	}
	if err := s.store.AddLabel(ctx, epic.ID, autoCloseReadyLabel); err != nil {
		slog.Warn("auto-close: failed to flag plan epic ready",
			"epic", epic.ID, "err", err.Error(), "feature", "ka-0000001587")
		return
	}
	s.addAutoComment(ctx, epic.ID,
		"plan subtree fully drained (all issue-kind children closed, MR work merged). "+
			"Flagged `autoclose-ready` — awaiting operator Mark-complete (plan_state → complete) "+
			"per the plan-lifecycle gate (feature ka-0000001587).")
	// Reflect the label on the in-memory bead and publish an update so live
	// /plans subscribers see the flag without a refetch.
	epic.Labels = appendIfAbsent(epic.Labels, autoCloseReadyLabel)
	epic.UpdatedAt = time.Now().UTC()
	s.recordAndPublish(ctx, events.TopicBeadUpdated, epic.ID, autoCloseActor, events.BeadUpdated{
		Bead:    epic,
		Changes: map[string]any{"labels": epic.Labels},
		Actor:   autoCloseActor,
	})
	slog.Info("flagged drained plan epic ready for operator completion",
		"epic", epic.ID, "feature", "ka-0000001587")
}

// maybeCloseEscalationWrappers closes the open requesting task of the
// just-closed escalation when it opts into the close-on-escalation contract. The
// escalation is on the depends_on_id side of a `blocks` edge whose bead_id is
// the wrapper, so the wrapper appears in the escalation's REVERSE
// dependencies. gb escalation create --requesting-task writes exactly this
// edge (wrapper blocks→ escalation).
//
// The opt-in check (gd-91b7zMOR2z): closeRequestingTaskField on THIS
// escalation is authoritative when present — set explicitly true/false by
// every `gb escalation create` post-fix, so it can never be haunted by a
// LATER, unrelated escalation against the same task the way the legacy task
// label was (that label persisted forever once stamped, so any later
// escalation's success closed the task regardless of ITS OWN intent). Only
// when the field is entirely absent — an escalation created before this fix —
// do we fall back to the deprecated task label, for one release.
//
// When the requesting task has no `blocks` edge on the escalation, the opt-in
// still closes it, but only if the caller who resolved the escalation may
// close that task by hand (closeRequestingTaskWithoutEdge, gd-MgicCcY2rj).
func (s *BeadsServer) maybeCloseEscalationWrappers(ctx context.Context, escalation *model.Bead) {
	escFields := parseFieldsToStringMap(escalation.Fields)

	// Fix (c): some operation types name a verdict, not the terminal event,
	// and must never cascade-close the requesting task no matter what the
	// escalation's own opt-in field or the legacy label say.
	if neverCascadeCloseOperationTypes[strings.ToLower(strings.TrimSpace(escFields["operation_type"]))] {
		slog.Info("auto-close: escalation operation_type never cascades to its requesting task; skipping",
			"escalation", escalation.ID, "operation_type", escFields["operation_type"], "bug", "gd-91b7zMOR2z")
		return
	}
	// A result that says the PR did not merge has not done the requesting
	// task's work, whatever result_status the resolver chose (gd-MgicCcY2rj).
	if escalationResultSaysNotMerged(escFields["result"]) {
		slog.Info("auto-close: escalation result says merged=false; leaving its requesting task open",
			"escalation", escalation.ID, "bug", "gd-MgicCcY2rj")
		return
	}

	closeRequestingRaw, hasField := escFields[closeRequestingTaskField]
	closeRequesting := strings.EqualFold(strings.TrimSpace(closeRequestingRaw), "true")
	// The opt-in names ONE bead: this escalation's requesting_task
	// (gd-ifFUTNHbqm). Any other bead with a `blocks` edge on the escalation
	// is only waiting on it ("merge me after #1315") — another merge
	// escalation, or a task that wants CI fixed first — and its own PR has
	// not merged. Closing those made one merge close two unmerged plan steps
	// and drop two PRs off the merge desk.
	requestingTask := strings.TrimSpace(escFields["requesting_task"])

	rev, err := s.store.GetReverseDependencies(ctx, escalation.ID)
	if err != nil {
		slog.Warn("auto-close: failed to fetch escalation reverse deps",
			"escalation", escalation.ID, "err", err.Error(), "feature", "ka-0000001587")
		return
	}

	seen := make(map[string]bool, len(rev))
	for _, d := range rev {
		if d == nil || d.Type != model.DepBlocks {
			continue
		}
		wrapperID := d.BeadID // the blocked side
		if wrapperID == "" || wrapperID == escalation.ID || seen[wrapperID] {
			continue
		}
		seen[wrapperID] = true

		if requestingTask != "" && wrapperID != requestingTask {
			continue
		}

		wrapper, err := s.store.GetBead(ctx, wrapperID)
		if err != nil || wrapper == nil {
			continue
		}
		if wrapper.Status == model.StatusClosed {
			continue
		}
		// An escalation or decision closes on its own execution or
		// resolution, never because a bead it waits on closed.
		if wrapper.Type == model.TypeEscalation || wrapper.Type == model.TypeDecision {
			continue
		}
		if hasField {
			if !closeRequesting || requestingTask == "" {
				// This escalation explicitly did NOT opt in (or names no
				// requesting task to close) — a plain blocked-by task is left
				// for its owner, regardless of any stale label a DIFFERENT
				// earlier escalation left behind.
				continue
			}
		} else if !hasLabel(wrapper.Labels, closeOnEscalationLabel) {
			// Back-compat fallback for a pre-fix escalation with no opt-in
			// field at all: honor the legacy task label for one release.
			continue
		} else {
			slog.Warn("auto-close: escalation has no close_requesting_task field; falling back to deprecated task label",
				"escalation", escalation.ID, "wrapper", wrapper.ID, "feature", "ka-0000001587", "bug", "gd-91b7zMOR2z")
		}
		s.closeEscalationWrapper(ctx, wrapper, escalation.ID)
	}

	if closeRequesting && requestingTask != "" && !seen[requestingTask] {
		s.closeRequestingTaskWithoutEdge(ctx, escalation.ID, requestingTask)
	}
}

// closeRequestingTaskWithoutEdge closes an opted-in requesting task that has
// no `blocks` edge on its escalation (gd-MgicCcY2rj). gb escalation create
// writes that edge as its filer, and the daemon refuses it when the filer does
// not own the task: a thread agent filing the merge escalation for a plan
// step another agent created. The escalation still carried
// close_requesting_task=true and the task stayed open after the merge
// (gd-wQqqAnagJp, gd-QFxiQF1DWs), so the desk closed it by hand.
//
// requesting_task is free text anyone on the shared queue may edit
// (escalation_close_grant.go), so without the edge the field alone is no
// authority to close anything. The close goes ahead only when the caller who
// resolved the escalation (ctx) passes the same write and ownership gates a
// hand close of the task would: the cascade then does what that caller could
// already do, and never widens who may close whose task.
func (s *BeadsServer) closeRequestingTaskWithoutEdge(ctx context.Context, escalationID, taskID string) {
	task, err := s.store.GetBead(ctx, taskID)
	if err != nil || task == nil || task.Status == model.StatusClosed {
		return
	}
	if task.Type == model.TypeEscalation || task.Type == model.TypeDecision {
		return
	}
	// authorizeOwnership passes an identity-less context, but here that is no
	// resolver at all: the spawn-policy sweeper replays and closes an
	// escalation on a context.Background()-derived ctx. Only a daemon serving
	// with auth disabled (authMethodNone) has no subject to check.
	var denial *AuthzDenial
	if AuthIdentityFromContext(ctx).Subject == "" && authMethodFromContext(ctx) != authMethodNone {
		denial = &AuthzDenial{Action: "close", Message: "no resolver identity on the closing context"}
	} else {
		denial = s.authorizeWriteForBead(ctx, "close", task)
	}
	if denial != nil {
		slog.Info("auto-close: requesting task has no blocks edge and the resolver may not close it; leaving it open",
			"escalation", escalationID, "task", taskID, "denial", denial.Message, "bug", "gd-MgicCcY2rj")
		s.addAutoComment(ctx, taskID, fmt.Sprintf(
			"not auto-closed: escalation %s executed with close_requesting_task, but this task has no blocks "+
				"edge on it and whoever resolved the escalation may not close this task. Close it by hand "+
				"once the work is on main.", escalationID))
		return
	}
	s.closeEscalationWrapper(ctx, task, escalationID)
}

// escalationResultSaysNotMerged reports whether an escalation's result object
// carries "merged": false (or "false"). Any other shape, including no result
// and no merged key, says nothing.
func escalationResultSaysNotMerged(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	var result map[string]any
	if json.Unmarshal([]byte(raw), &result) != nil {
		return false
	}
	switch v := result["merged"].(type) {
	case bool:
		return !v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "false")
	}
	return false
}

// closeEscalationWrapper closes one opted-in wrapper task, mirroring the
// CloseBead event + mutation bookkeeping. The freshly-closed wrapper re-enters
// afterBeadClosed so its parent epic drains if the wrapper was the last child.
func (s *BeadsServer) closeEscalationWrapper(ctx context.Context, wrapper *model.Bead, escalationID string) {
	// An epic closes when its subtree drains (maybeAutoCloseParents), never
	// because an escalation it was parked behind executed. A plan gardener's
	// merge escalation for one step's PR named the plan epic as its
	// requesting task, and the merge closed the plan with 7 steps open.
	if wrapper.Type == model.TypeEpic {
		slog.Warn("auto-close: escalation's requesting task is an epic; leaving it open",
			"epic", wrapper.ID, "escalation", escalationID, "bug", "gd-0SbA6Iyclm")
		return
	}
	if hold := awaitsReleaseHold(wrapper, "escalation "+escalationID+" executed"); hold != "" {
		s.addAutoComment(ctx, wrapper.ID, hold)
		return
	}
	if held := s.wrapperStillHeld(ctx, wrapper, escalationID); held != "" {
		slog.Info("auto-close: requesting task has other open work; leaving it open",
			"wrapper", wrapper.ID, "escalation", escalationID, "held_by", held, "bug", "gd-ed4jSP0ssQ")
		s.addAutoComment(ctx, wrapper.ID, fmt.Sprintf(
			"not auto-closed: escalation %s executed, but %s is still open on this task.", escalationID, held))
		return
	}
	prevStatus := wrapper.Status
	closed, err := s.store.CloseBead(ctx, wrapper.ID, autoCloseActor)
	if err != nil || closed == nil {
		slog.Warn("auto-close: failed to close escalation wrapper",
			"wrapper", wrapper.ID, "escalation", escalationID, "err", errString(err), "feature", "ka-0000001587")
		return
	}
	s.addAutoComment(ctx, wrapper.ID, fmt.Sprintf(
		"auto-closed: blocker escalation %s executed — closing per the close-on-escalation-execute "+
			"contract (feature ka-0000001587).", escalationID))
	s.recordAndPublish(ctx, events.TopicBeadClosed, closed.ID, autoCloseActor, events.BeadClosed{
		Bead:     closed,
		ClosedBy: autoCloseActor,
	})
	s.recordMutation(ctx, closed.ID, autoCloseActor, "status",
		string(prevStatus), string(closed.Status), model.MutationTypeUpdate)
	slog.Info("auto-closed escalation wrapper on blocker execution",
		"wrapper", wrapper.ID, "escalation", escalationID, "feature", "ka-0000001587")
	// Cascade: the wrapper may have been the last open child of a parent epic.
	s.afterBeadClosed(ctx, closed)
}

// wrapperStillHeld returns the first piece of work that keeps a requesting
// task open after one of its escalations executes, or "" when none does
// (gd-ed4jSP0ssQ). A task split into several slice PRs files one merge
// escalation per slice, each with --close-on-execute, and the first merge
// closed the task while the other slices were still open. The task stays open
// while another open escalation or decision blocks it or names it as its
// requesting_task, or while a PR linked to it (a produces edge to an unmerged,
// unclosed merge-request bead) is still open. The executed escalation's own PR
// is skipped: its merge-request bead may not have caught up with the merge yet.
// If a slice's PR is only linked, its merge-request bead closing on merge
// closes the task through maybeCloseMergedLeaf.
func (s *BeadsServer) wrapperStillHeld(ctx context.Context, wrapper *model.Bead, escalationID string) string {
	deps, _ := s.store.GetDependencies(ctx, wrapper.ID)
	for _, d := range deps {
		if d == nil || d.Type != model.DepBlocks || d.DependsOnID == escalationID {
			continue
		}
		b, err := s.store.GetBead(ctx, d.DependsOnID)
		if err == nil && b != nil && b.Status != model.StatusClosed &&
			(b.Type == model.TypeEscalation || b.Type == model.TypeDecision) {
			return string(b.Type) + " " + b.ID
		}
	}
	named, _, _ := s.store.ListBeads(ctx, model.BeadFilter{
		Type:   []model.BeadType{model.TypeEscalation, model.TypeDecision},
		Status: openSubtreeChildStatuses,
		Fields: map[string]string{"requesting_task": wrapper.ID},
		Limit:  10,
	})
	for _, b := range named {
		if b != nil && b.ID != escalationID &&
			strings.TrimSpace(parseFieldsToStringMap(b.Fields)["requesting_task"]) == wrapper.ID {
			return string(b.Type) + " " + b.ID
		}
	}
	var own string
	if esc, err := s.store.GetBead(ctx, escalationID); err == nil && esc != nil {
		own = esc.Title + " " + string(esc.Fields)
	}
	for _, mr := range s.linkedMRBeads(ctx, wrapper) {
		f := parseFieldsToStringMap(mr.Fields)
		url := strings.TrimSpace(f["mr_url"])
		if mr.Status == model.StatusClosed || isMRMerged(f) || (url != "" && strings.Contains(own, url)) {
			continue
		}
		if url == "" {
			url = mr.ID
		}
		return "PR " + url
	}
	return ""
}

// maybeParkDecisionGatedBeads parks the still-open beads a just-closed decision
// was gating, transitioning each open/in_progress bead to `blocked`
// (ka-0000005620).
//
// The failure this fixes: an agent finds an operator-gated bead thrashing in the
// intake lane, does the right thing — files a decision and links it as a
// `blocks` edge so FilterNoOpenDeps stops surfacing the bead — the operator
// resolves the decision, the decision CLOSES, and the blocks-edge from a closed
// decision no longer suppresses anything. The bead falls straight back into
// `gb ready` and the next bulk-groomer re-lanes it. The fix for the thrash was
// itself the cause of the next thrash. Both confirmed instances (ka-0000004652,
// ka-0000004466) were manually parked at `blocked` as a workaround; this
// automates that workaround at the single bead-close choke point so the gate is
// durable, discoverable (the blocked-by badge), and reversible (reopen the bead).
//
// The decision sits on the depends_on_id side of a `blocks` edge whose bead_id
// is the gated bead, so gated beads appear in the decision's REVERSE
// dependencies — the same shape maybeCloseEscalationWrappers walks. Unlike the
// escalation-wrapper contract, NO opt-in label is required: the `blocks`→
// decision edge is itself the agent's explicit statement "this bead is gated on
// this decision", so parking is the default. A bead can opt OUT with
// decisionGateReopenLabel for the rare release-on-resolve case.
func (s *BeadsServer) maybeParkDecisionGatedBeads(ctx context.Context, decision *model.Bead) {
	rev, err := s.store.GetReverseDependencies(ctx, decision.ID)
	if err != nil {
		slog.Warn("decision-gate: failed to fetch decision reverse deps",
			"decision", decision.ID, "err", err.Error(), "bug", "ka-0000005620")
		return
	}

	// The decision's own requesting task (the `requesting_task` field
	// `gb decision create --requesting-task` stamps) must NOT be parked
	// (ka-0000000025). Two edge shapes share the `blocks`→decision edge but mean
	// opposite things on resolve:
	//
	//   - INTAKE-GATE (ka-0000005620): a groomer files a decision ABOUT a
	//     thrashing bead it does not own; `requesting_task` is empty (the gated
	//     bead is someone else's). Resolving usually means hold/dismiss, so the
	//     bead must stay OUT of the ready lane — park to `blocked` is correct.
	//
	//   - REQUESTING-TASK: an agent gates its OWN task until an answer, per the
	//     `--requesting-task` contract (advice ka-0000001746) — `requesting_task`
	//     IS the gated bead. Resolving FREES that work: the Slack-bridge respawn
	//     path (respawnAgentForDecision) revives an agent to continue it. Parking
	//     that bead to `blocked` re-hides the just-unblocked work behind a CLOSED
	//     decision (open_blockers[] == []), which every open-gated lane is blind
	//     to — the decision cost is paid and no throughput comes back. Measured
	//     2026-08-27: 7 such decisions resolved, all 7 requesting bugs flipped
	//     open→blocked; 4 only recovered because the respawn won a race against
	//     this park, and the label-clean bug ka-0000004809 stayed stuck-blocked.
	//     Leaving it untouched (status stays `open`) lets a lane pick it up and
	//     makes the outcome independent of that race.
	requestingTask := parseFieldsToStringMap(decision.Fields)["requesting_task"]

	seen := make(map[string]bool, len(rev))
	for _, d := range rev {
		if d == nil || d.Type != model.DepBlocks {
			continue
		}
		gatedID := d.BeadID // the blocked side
		if gatedID == "" || gatedID == decision.ID || seen[gatedID] {
			continue
		}
		seen[gatedID] = true

		if gatedID == requestingTask {
			slog.Info("decision-gate: not parking the decision's own requesting task — resolving frees it to continue (respawn path handles it)",
				"decision", decision.ID, "requesting_task", gatedID, "bug", "ka-0000000025")
			continue
		}

		gated, err := s.store.GetBead(ctx, gatedID)
		if err != nil || gated == nil {
			continue
		}
		// Only park work that is actively surfaceable. A bead already blocked,
		// deferred, closed, or partial neither thrashes in `gb ready` nor needs
		// re-parking, so leave it untouched (and idempotent on a re-close).
		if gated.Status != model.StatusOpen && gated.Status != model.StatusInProgress {
			continue
		}
		if hasLabel(gated.Labels, decisionGateReopenLabel) {
			// Explicit opt-out: this bead is meant to release into the ready
			// lane when its gating decision resolves, not park for follow-up.
			slog.Info("decision-gate: gated bead opts out of park (release-on-decision-close); leaving as-is",
				"decision", decision.ID, "gated", gatedID, "bug", "ka-0000005620")
			continue
		}
		s.parkDecisionGatedBead(ctx, gated, decision.ID)
	}
}

// parkDecisionGatedBead transitions one open/in_progress decision-gated bead to
// `blocked`, mirroring the event + mutation bookkeeping the CloseBead sites
// perform so live subscribers and the audit trail observe the transition
// identically. It does NOT re-enter afterBeadClosed — a park-to-blocked is not a
// close, so there is no parent to drain.
func (s *BeadsServer) parkDecisionGatedBead(ctx context.Context, gated *model.Bead, decisionID string) {
	prevStatus := gated.Status
	gated.Status = model.StatusBlocked
	gated.UpdatedAt = time.Now().UTC()
	if err := s.store.UpdateBead(ctx, gated); err != nil {
		slog.Warn("decision-gate: failed to park gated bead",
			"gated", gated.ID, "decision", decisionID, "err", err.Error(), "bug", "ka-0000005620")
		return
	}
	s.addDecisionGateComment(ctx, gated.ID, fmt.Sprintf(
		"auto-parked `blocked`: gating decision %s resolved. A closed decision's `blocks` edge no longer "+
			"suppresses this bead, so it would otherwise fall back into the ready lane and thrash "+
			"(ka-0000005620). Reopen (status → open) when this work is actually ready to pick up.", decisionID))
	s.recordAndPublish(ctx, events.TopicBeadUpdated, gated.ID, decisionGateActor, events.BeadUpdated{
		Bead:    gated,
		Changes: map[string]any{"status": string(model.StatusBlocked)},
		Actor:   decisionGateActor,
	})
	s.recordMutation(ctx, gated.ID, decisionGateActor, "status",
		string(prevStatus), string(model.StatusBlocked), model.MutationTypeUpdate)
	slog.Info("auto-parked decision-gated bead on decision close",
		"gated", gated.ID, "decision", decisionID, "prev_status", string(prevStatus), "bug", "ka-0000005620")
}

// addDecisionGateComment records a best-effort system audit comment attributed
// to decisionGateActor. Failures are logged and swallowed — the comment is
// provenance, not a precondition (mirrors addAutoComment).
func (s *BeadsServer) addDecisionGateComment(ctx context.Context, beadID, text string) {
	comment := &model.Comment{
		BeadID:    beadID,
		Author:    decisionGateActor,
		Text:      text,
		CreatedAt: time.Now().UTC(),
	}
	s.inheritCommentOperator(ctx, comment)
	if err := s.store.AddComment(ctx, comment); err != nil {
		slog.Warn("decision-gate: failed to add audit comment",
			"bead", beadID, "err", err.Error(), "bug", "ka-0000005620")
	}
}

// orphanParkActor is the actor attributed to the daemon parking a child whose
// parent-child parent was just closed out from under it (ka-0000000504).
// Distinct from autoCloseActor/decisionGateActor so the audit trail names the
// exact transition (park-orphan-to-blocked), matching the `system:` convention.
const orphanParkActor = "system:orphan-park"

// maybeParkOrphanedChildren parks the still-open parent-child CHILDREN of a
// just-closed bead, transitioning each surfaceable child to `blocked`
// (ka-0000000504).
//
// The failure this fixes: closing a bead that still has OPEN parent-child
// children is accepted silently. Those children stay status=open with a dead
// parent and remain claimable in `gb ready` indefinitely — the auto-close
// cascade (maybeAutoCloseParents/drainParentEpic) can only ever DRAIN a parent
// whose subtree is already empty; it never inspects the children of the bead
// being closed, and its `parent.Type != TypeEpic` skip means a non-epic parent
// (measured: 43 of 56 stranded rows) is doubly invisible. Measured on the live
// gardenio queue, the close path was the generator behind 46 of 56 stuck
// closed-parent rows (82%), while the just-merged edge-add guard (ka-0000005756)
// covered only the 10 edge-onto-dead-parent rows.
//
// The children sit on the bead_id side of a `parent-child` edge whose
// depends_on_id is the closed bead — so they are its REVERSE dependencies, the
// same shape maybeParkDecisionGatedBeads and maybeCloseMergedLeaves walk. Each
// is deduped so a diamond in the graph is handled once.
//
// Parking (status → blocked) rather than closing is deliberate: it mirrors the
// news-ready-relevance rubric §4.1 corollary (prefer a status/field-derived
// exclusion over destructive per-bead mutation) and the decision-gate precedent
// (ka-0000005620). `gb ready` lists only status=open beads (handleGetReady), so
// a parked child leaves the claimable lane while staying intact, findable, and
// reopenable (status → open) the instant a human decides the work still matters.
// Closing the child would instead destroy the legitimate "epic is done, its
// stragglers are moot" workflow that produced most of these rows — the close of
// the parent must still succeed.
//
// A child with ANOTHER still-live parent-child parent is NOT orphaned and is
// left untouched: parking it would hide work that a live parent legitimately
// still tracks. Only claimable WORK is ever parked — non-deliverable issue-kind
// children; every non-issue-kind child (advice/doc/report/escalation/decision)
// and every deliverable container (bundle/rich-report) is skipped, because none
// are `gb ready` work and parking them to `blocked` only causes durable loss:
// advice is delivered by open status, receipts render on /plans, and
// escalations/decisions are operator-queue questions (ka-0000002225 carved out
// only the last two; ka-0000000182 measured that the class is the whole set).
// Best-effort throughout: a store error logs and returns without disturbing the
// close that triggered it.
func (s *BeadsServer) maybeParkOrphanedChildren(ctx context.Context, closedBead *model.Bead) {
	rev, err := s.store.GetReverseDependencies(ctx, closedBead.ID)
	if err != nil {
		slog.Warn("orphan-park: failed to fetch reverse deps",
			"bead", closedBead.ID, "err", err.Error(), "bug", "ka-0000000504")
		return
	}

	seen := make(map[string]bool, len(rev))
	for _, d := range rev {
		if d == nil || !model.IsParentChildDepType(string(d.Type)) {
			continue
		}
		// child-first orientation: the child is bead_id, the closed parent is
		// depends_on_id. Skip a self-edge and any child already handled.
		childID := d.BeadID
		if childID == "" || childID == closedBead.ID || seen[childID] {
			continue
		}
		seen[childID] = true

		child, err := s.store.GetBead(ctx, childID)
		if err != nil || child == nil {
			continue
		}
		// Park ONLY claimable WORK. Parking exists for exactly one purpose —
		// to pull a stale, dead-parented item out of the `gb ready` claim lane
		// — so it earns its keep only for beads that lane actually surfaces:
		// non-deliverable issue-kind work (task/bug/feature/epic/chore/molecule).
		// Every OTHER bead is HARMED by a park with no upside, because none of
		// them are claimable `gb ready` work in the first place:
		//
		//   - data-kind beads (advice, doc, report, escalation, decision, …).
		//     ADVICE IS DELIVERED BY STATUS: an OPEN advice bead is pushed into
		//     every agent's boot advice list; parked to `blocked` it reaches
		//     ZERO agents (ka-0000000182 measured this twice, on the project's
		//     only two advice beads). doc/report receipts render on /plans by
		//     their open status — parking buries the evidence layer. An
		//     escalation/decision is an un-answered question to a HUMAN that the
		//     operator queues (`gb escalation list` / `gb decision list`) filter
		//     to open/in_progress, so parking silently drops it from view while
		//     leaving the ask un-actioned (ka-0000002225) — and it fires
		//     constantly for scheduled gardeners, whose `--requesting-task` is
		//     their own ephemeral per-cycle bead: closing it at `gb stop` parks
		//     the escalation/decision they just filed. This very bug buried its
		//     OWN fix's `gitlab_mr_merge` escalation that way.
		//   - deliverable containers (bundle/rich-report/dashboard/mr): though
		//     issue-KIND, these are finished, human-facing artifacts, never
		//     claimable work — they classify `artifact` (isDeliverableType /
		//     derive_bead_category rule 3) precisely so they stay OUT of the ready
		//     lane. Parking a delivered bundle buries the receipt Matt asked
		//     for.
		//
		// The prior scope carved out only escalation/decision (ka-0000002225);
		// that missed all 7 measured non-decision/non-escalation cases
		// (feature/task/bug parked legitimately as orphans, plus doc/bundle/
		// advice buried, ka-0000000182). The kind+deliverable predicate below
		// subsumes the old carve-out and covers the whole class, while the
		// issue-kind work that motivated the parking path (ka-0000000504) still
		// parks unchanged.
		if child.Kind != model.KindIssue || isDeliverableType(child.Type) {
			continue
		}
		// Only park work that is actively surfaceable in `gb ready`. A child
		// already blocked, deferred, closed, or partial neither thrashes in the
		// ready lane nor needs re-parking, so leave it (idempotent on a re-close).
		if child.Status != model.StatusOpen && child.Status != model.StatusInProgress {
			continue
		}
		// A child with another still-live parent-child parent is NOT orphaned —
		// that live parent legitimately tracks it, so parking would hide real work.
		if s.hasOtherLiveParent(ctx, child, closedBead.ID) {
			continue
		}
		s.parkOrphanedChild(ctx, child, closedBead.ID)
	}
}

// hasOtherLiveParent reports whether child has a parent-child parent OTHER than
// excludeParentID that is not terminal-for-progression (i.e. still open,
// in_progress, deferred, or blocked). Such a live parent legitimately keeps the
// child in play, so the just-closed parent did not orphan it. Best-effort: a
// read error is treated as "no other live parent" (fail toward parking, the
// safe, reversible default) so a transient failure never leaves a truly-orphaned
// child claimable.
func (s *BeadsServer) hasOtherLiveParent(ctx context.Context, child *model.Bead, excludeParentID string) bool {
	deps, err := s.store.GetDependencies(ctx, child.ID)
	if err != nil {
		return false
	}
	for _, d := range deps {
		if d == nil || !model.IsParentChildDepType(string(d.Type)) {
			continue
		}
		parentID := d.DependsOnID
		if parentID == "" || parentID == child.ID || parentID == excludeParentID {
			continue
		}
		parent, err := s.store.GetBead(ctx, parentID)
		if err != nil || parent == nil {
			continue
		}
		if !parent.Status.IsTerminalForProgression() {
			return true
		}
	}
	return false
}

// parkOrphanedChild transitions one open/in_progress orphaned child to
// `blocked`, mirroring the event + mutation bookkeeping the CloseBead sites
// perform so live subscribers and the audit trail observe the transition
// identically. It does NOT re-enter afterBeadClosed — a park-to-blocked is not a
// close, so there is nothing further to cascade.
func (s *BeadsServer) parkOrphanedChild(ctx context.Context, child *model.Bead, parentID string) {
	prevStatus := child.Status
	child.Status = model.StatusBlocked
	child.UpdatedAt = time.Now().UTC()
	if err := s.store.UpdateBead(ctx, child); err != nil {
		slog.Warn("orphan-park: failed to park orphaned child",
			"child", child.ID, "parent", parentID, "err", err.Error(), "bug", "ka-0000000504")
		return
	}
	s.addOrphanParkComment(ctx, child.ID, fmt.Sprintf(
		"auto-parked `blocked`: parent-child parent %s was closed while this child was still open. A dead "+
			"parent's children stay claimable in `gb ready` forever (the auto-close cascade only drains an "+
			"already-empty parent), so this bead is parked out of the ready lane (ka-0000000504). It stays "+
			"intact and findable — reopen (status → open), or reparent to a live epic, when the work still matters.",
		parentID))
	s.recordAndPublish(ctx, events.TopicBeadUpdated, child.ID, orphanParkActor, events.BeadUpdated{
		Bead:    child,
		Changes: map[string]any{"status": string(model.StatusBlocked)},
		Actor:   orphanParkActor,
	})
	s.recordMutation(ctx, child.ID, orphanParkActor, "status",
		string(prevStatus), string(model.StatusBlocked), model.MutationTypeUpdate)
	slog.Info("auto-parked orphaned child on parent close",
		"child", child.ID, "parent", parentID, "prev_status", string(prevStatus), "bug", "ka-0000000504")
}

// addOrphanParkComment records a best-effort system audit comment attributed to
// orphanParkActor. Failures are logged and swallowed — the comment is
// provenance, not a precondition (mirrors addDecisionGateComment).
func (s *BeadsServer) addOrphanParkComment(ctx context.Context, beadID, text string) {
	comment := &model.Comment{
		BeadID:    beadID,
		Author:    orphanParkActor,
		Text:      text,
		CreatedAt: time.Now().UTC(),
	}
	s.inheritCommentOperator(ctx, comment)
	if err := s.store.AddComment(ctx, comment); err != nil {
		slog.Warn("orphan-park: failed to add audit comment",
			"bead", beadID, "err", err.Error(), "bug", "ka-0000000504")
	}
}

// maybeCloseMergedLeaves closes the LEAF issue beads that PRODUCED the
// just-merged MR bead, when every MR each leaf links is merged. It is the leaf
// counterpart of maybeAutoCloseParents — which only ever transitions parent
// EPICS (the `parent.Type != model.TypeEpic { continue }` skip) — and closes
// the missing close-on-merge path for non-epic beads: before this, a bug/task
// whose own MR merged stayed status=open forever and churned in `gb ready`
// every gardener cycle (ka-0000005910).
//
// The producing leaves are the reverse `produces` edges of the MR bead
// (issue --produces--> merge-request, so the leaf is on the bead_id side),
// mirroring beadsapi.ProducingTaskBeadsForMR. Each is deduped so a diamond
// (one MR produced by several leaves, or reached twice) is checked once.
//
// Best-effort: a store error logs and returns without disturbing the MR-bead
// close that triggered it.
func (s *BeadsServer) maybeCloseMergedLeaves(ctx context.Context, mrBead *model.Bead) {
	// A merge-request bead that closed WITHOUT merging (mr_outcome=closed) can
	// never newly-qualify a leaf — the leaf gate requires every linked MR
	// merged, and this one is not. Short-circuit before walking reverse deps.
	if !isMRMerged(parseFieldsToStringMap(mrBead.Fields)) {
		return
	}

	rev, err := s.store.GetReverseDependencies(ctx, mrBead.ID)
	if err != nil {
		slog.Warn("auto-close: failed to fetch MR reverse deps",
			"mr", mrBead.ID, "err", err.Error(), "feature", "ka-0000005910")
		return
	}

	seen := make(map[string]bool, len(rev))
	for _, d := range rev {
		if d == nil || d.Type != producesDepType {
			continue
		}
		leafID := d.BeadID // the producing (issue) side of issue--produces-->mr
		if leafID == "" || leafID == mrBead.ID || seen[leafID] {
			continue
		}
		seen[leafID] = true

		leaf, err := s.store.GetBead(ctx, leafID)
		if err != nil || leaf == nil {
			continue
		}
		s.maybeCloseMergedLeaf(ctx, leaf)
	}
}

// maybeCloseMergedLeaf closes one LEAF issue bead iff every gate passes: it is
// an OPEN, non-epic issue-kind leaf of an eligible type, it does not opt out via
// no-auto-close, it has no open issue-kind child work (so it is genuinely a
// leaf, not an umbrella tracker), it links at least one MR, and EVERY linked MR
// is merged. Any failed gate leaves the bead untouched.
func (s *BeadsServer) maybeCloseMergedLeaf(ctx context.Context, leaf *model.Bead) {
	// LEAF-only: task/bug/feature/chore. Epics are excluded on purpose — an epic
	// drains via drainParentEpic once its whole subtree is done, and the audit
	// behind ka-0000005910 found 7 merged-MR epics that are correctly still open.
	// A merge-request bead parented under an epic still drains it through the
	// parent walk in afterBeadClosed, never here.
	if !leafCloseOnMergeTypes[leaf.Type] {
		return
	}
	if leaf.Status == model.StatusClosed {
		return // already terminal — idempotent no-op
	}

	// Explicit opt-out, mirroring the epic hard-close escape hatch (ka-0000001525):
	// one `kd update <id> -l no-auto-close` suppresses close-on-merge for a leaf
	// whose fix genuinely needs more than its merged MR before it is done.
	if hasLabel(leaf.Labels, noAutoCloseLabel) {
		slog.Info("auto-close: leaf has merged MR(s) but carries no-auto-close; leaving open",
			"leaf", leaf.ID, "feature", "ka-0000005910")
		return
	}

	// A task that awaits its release stays open until a deployment carries
	// it (closeShippedWork, gd-ZM7NEBPAqZ).
	if hold := awaitsReleaseHold(leaf, "its linked merge-request(s) merged"); hold != "" {
		s.addAutoComment(ctx, leaf.ID, hold)
		return
	}

	// Umbrella guard: a bead with any OPEN issue-kind parent-child descendant is
	// a tracker whose acceptance is staged across children, not a single-MR unit.
	// Closing it on one child's MR merge would abandon that open work. A true
	// leaf has no such descendants, so this is a no-op for the common case; it
	// defends a misclassified non-epic that was (mis)used as a container.
	if s.hasOpenSubtreeWork(ctx, leaf.ID) {
		slog.Info("auto-close: bead with merged MR has open child work; not a leaf, skipping",
			"bead", leaf.ID, "feature", "ka-0000005910")
		return
	}

	// Gate: >=1 linked MR AND every one merged (never first-merge-wins). The
	// authoritative signal is the canonical type=merge-request bead reached via
	// `produces`, NOT the issue bead's own mr_state — the gitlab-bridge stopped
	// stamping mr_state onto issue beads, so it is absent/stale by design
	// (ka-0000000586). linkedMRBeads enumerates ALL produces edges so a leaf
	// producing several MRs (e.g. ka-0000003811 produced !6154 + !6155) is gated
	// on the whole set, not just the one that happened to fire this event.
	mrs := s.linkedMRBeads(ctx, leaf)
	if len(mrs) == 0 {
		return // no canonical MR bead linked — nothing to gate on
	}
	for _, mr := range mrs {
		if !isMRMerged(parseFieldsToStringMap(mr.Fields)) {
			slog.Info("auto-close: leaf has an unmerged linked MR; deferring",
				"leaf", leaf.ID, "unmerged_mr", mr.ID, "feature", "ka-0000005910")
			return
		}
	}

	s.autoCloseMergedLeaf(ctx, leaf, len(mrs))
}

// hasOpenSubtreeWork reports whether beadID has any open/in_progress/blocked
// issue-kind parent-child descendant, reusing the SAME subtree enumeration +
// type/status allowlists as drainParentEpic so "what counts as remaining work"
// can never disagree between the leaf and epic paths. Limit 1: existence, not a
// count. Fails SAFE — a store error returns true (has work) so a leaf is never
// closed while its childlessness is unproven.
func (s *BeadsServer) hasOpenSubtreeWork(ctx context.Context, beadID string) bool {
	open, _, err := s.store.ListBeads(ctx, model.BeadFilter{
		ParentSubtreeID: beadID,
		Type:            openSubtreeChildTypes,
		Status:          openSubtreeChildStatuses,
		Limit:           1,
	})
	if err != nil {
		slog.Warn("auto-close: failed to check leaf subtree; not closing",
			"bead", beadID, "err", err.Error(), "feature", "ka-0000005910")
		return true
	}
	return len(open) > 0
}

// linkedMRBeads returns the canonical type=merge-request beads a leaf links via
// its forward `produces` edges (issue --produces--> merge-request), deduped.
//
// This is the MULTI-MR-aware signal the close-on-merge gate needs. The issue
// bead's legacy single `mr_url` field cannot express a leaf that produced more
// than one MR, and closing on the first merge would abandon the rest; walking
// produces edges enumerates the full set so "every linked MR merged" is exact.
// The trigger for this path is an MR-bead close, and the gitlab-bridge writes
// the produces edge alongside the MR bead, so a merged leaf reaches here with
// its produces edge already present (verified on the ka-0000005910 evidence
// beads). Best-effort: a dep-fetch error logs and yields no MRs, so the gate
// treats the leaf as "no MR linked" and leaves it open.
func (s *BeadsServer) linkedMRBeads(ctx context.Context, leaf *model.Bead) []*model.Bead {
	deps, err := s.store.GetDependencies(ctx, leaf.ID)
	if err != nil {
		slog.Warn("auto-close: failed to fetch leaf produces edges",
			"leaf", leaf.ID, "err", err.Error(), "feature", "ka-0000005910")
		return nil
	}
	var out []*model.Bead
	seen := map[string]bool{}
	for _, d := range deps {
		if d == nil || d.Type != producesDepType || d.DependsOnID == "" || seen[d.DependsOnID] {
			continue
		}
		mr, err := s.store.GetBead(ctx, d.DependsOnID)
		if err != nil || mr == nil || mr.Type != mergeRequestType {
			continue
		}
		seen[mr.ID] = true
		out = append(out, mr)
	}
	return out
}

// autoCloseMergedLeaf closes one eligible leaf whose every linked MR merged,
// mirroring autoCloseEpic's event + mutation bookkeeping so live subscribers and
// the audit trail observe the transition identically. The freshly-closed leaf
// re-enters afterBeadClosed so a parent epic it was the last open child of
// drains in turn.
func (s *BeadsServer) autoCloseMergedLeaf(ctx context.Context, leaf *model.Bead, mergedMRs int) {
	prevStatus := leaf.Status
	closed, err := s.store.CloseBead(ctx, leaf.ID, autoCloseActor)
	if err != nil || closed == nil {
		slog.Warn("auto-close: failed to close merged leaf",
			"leaf", leaf.ID, "err", errString(err), "feature", "ka-0000005910")
		return
	}
	s.addAutoComment(ctx, leaf.ID, fmt.Sprintf(
		"auto-closed: all %d linked merge-request(s) merged (leaf close-on-merge, ka-0000005910). "+
			"Reopen if the fix is incomplete, or stamp the no-auto-close label to opt out.", mergedMRs))
	s.recordAndPublish(ctx, events.TopicBeadClosed, closed.ID, autoCloseActor, events.BeadClosed{
		Bead:     closed,
		ClosedBy: autoCloseActor,
	})
	s.recordMutation(ctx, closed.ID, autoCloseActor, "status",
		string(prevStatus), string(closed.Status), model.MutationTypeUpdate)
	slog.Info("auto-closed merged leaf",
		"leaf", leaf.ID, "merged_mrs", mergedMRs, "feature", "ka-0000005910")
	// Cascade upward: a closed leaf may have been the last open child of an epic.
	s.afterBeadClosed(ctx, closed)
}

// maybeCloseBlockedBugsOnFixMerge closes every OPEN bug that a just-closed
// fix-task fixed out-of-band: the bug carries a `blocks`→fix-task edge (so the
// bug appears in the fix-task's REVERSE dependencies, the same shape steps 2 & 4
// walk), opts into the contract with closeOnBlockingFixMergeLabel, and — the
// gate — has EVERY one of its blocking beads closed with at least one carrying a
// MERGED MR (ka-0000003106).
//
// The failure this fixes: the CED remediation lane (epic ka-0000004371) does not
// fix a bug in place; it files a SCOPED FIX-TASK that `blocks` the bug and links
// the MR to the fix-task. When the fix merges the fix-task closes, but the
// parent bug does not — the daemon cascade steps 1-4 follow parent-child (drain)
// or the escalation/decision-specific reverse edges, never a bug's `blocks`→
// fix-task. So a merged-and-fixed bug sat OPEN indefinitely, re-offered as a
// ready candidate every gardener cycle until closed manually (~15m of churn per
// bug, observed live 2026-08-06 on ka-0000004133).
//
// Why an OPT-IN label and not a bare blocked-by: `blocks` is overloaded — it can
// mean "this fix-task resolves the bug" (the CED lane) OR "X must happen before
// the bug can start" (ordering). Auto-closing every bug whose blocker closed
// would wrongly close the latter. As with the escalation-wrapper contract
// (closeOnEscalationLabel), the lane stamps the label on the bug to declare
// "this bug is fixed via its blocking fix-task(s)", keeping the transition inert
// until deliberately armed — zero fleet-wide blast radius.
func (s *BeadsServer) maybeCloseBlockedBugsOnFixMerge(ctx context.Context, fixTask *model.Bead) {
	rev, err := s.store.GetReverseDependencies(ctx, fixTask.ID)
	if err != nil {
		slog.Warn("auto-close: failed to fetch fix-task reverse deps",
			"fix_task", fixTask.ID, "err", err.Error(), "bug", "ka-0000003106")
		return
	}

	seen := make(map[string]bool, len(rev))
	for _, d := range rev {
		if d == nil || d.Type != model.DepBlocks {
			continue
		}
		bugID := d.BeadID // the blocked side of bug --blocks--> fix-task
		if bugID == "" || bugID == fixTask.ID || seen[bugID] {
			continue
		}
		seen[bugID] = true

		bug, err := s.store.GetBead(ctx, bugID)
		if err != nil || bug == nil {
			continue
		}
		s.maybeCloseBlockedBug(ctx, bug)
	}
}

// maybeCloseBlockedBug closes one blocked bug iff every gate passes: it is an
// OPEN/in_progress bug, opts in via closeOnBlockingFixMergeLabel, does not opt
// OUT via no-auto-close, EVERY one of its `blocks`-blockers is closed, and at
// least one closed blocker carried a MERGED MR. Any failed gate leaves the bug
// untouched. Restricting the type to bug matches the CED lane (it files bugs)
// and keeps the contract narrow; the opt-in label is the real guard, so widening
// the type later is a one-line change if another lane needs it.
func (s *BeadsServer) maybeCloseBlockedBug(ctx context.Context, bug *model.Bead) {
	if bug.Type != model.TypeBug {
		return
	}
	// Only close still-surfaceable work; a bug already closed/deferred/blocked is
	// idempotent-safe to skip (and a re-close event will not re-fire the close).
	if bug.Status != model.StatusOpen && bug.Status != model.StatusInProgress {
		return
	}
	if !hasLabel(bug.Labels, closeOnBlockingFixMergeLabel) {
		// Not opted in — a bug merely blocked-by some task is left for its owner.
		return
	}
	// Explicit opt-out, mirroring the leaf close-on-merge escape hatch: one
	// `kd update <id> -l no-auto-close` suppresses the close for a bug whose fix
	// genuinely needs more than its blocking fix-task's merged MR before it is done.
	if hasLabel(bug.Labels, noAutoCloseLabel) {
		slog.Info("auto-close: blocked bug has merged fix but carries no-auto-close; leaving open",
			"bug", bug.ID, "feature", "ka-0000003106")
		return
	}

	// Enumerate the bug's OWN `blocks`-blockers (forward deps: bug --blocks-->
	// blocker). EVERY blocker must be closed — a still-open sibling fix-task means
	// the bug is not fully fixed yet — AND at least one closed blocker must carry a
	// MERGED MR, which is what distinguishes a real merged fix from a blocker that
	// closed wontfix/duplicate (acceptance #2).
	deps, err := s.store.GetDependencies(ctx, bug.ID)
	if err != nil {
		slog.Warn("auto-close: failed to fetch blocked-bug forward deps; not closing",
			"bug", bug.ID, "err", err.Error(), "feature", "ka-0000003106")
		return
	}
	blockers := 0
	mergedFix := ""
	for _, d := range deps {
		if d == nil || d.Type != model.DepBlocks || d.DependsOnID == "" {
			continue
		}
		blockers++
		blocker, err := s.store.GetBead(ctx, d.DependsOnID)
		if err != nil || blocker == nil {
			// Cannot prove this blocker is closed — fail SAFE, leave the bug open.
			slog.Info("auto-close: blocked bug has an unresolvable blocker; deferring",
				"bug", bug.ID, "blocker", d.DependsOnID, "feature", "ka-0000003106")
			return
		}
		if blocker.Status != model.StatusClosed {
			// A still-open blocking fix-task: the bug is not fully fixed. Defer.
			slog.Info("auto-close: blocked bug still has an open blocker; deferring",
				"bug", bug.ID, "open_blocker", blocker.ID, "feature", "ka-0000003106")
			return
		}
		if mergedFix == "" && s.blockerCarriesMergedMR(ctx, blocker) {
			mergedFix = blocker.ID
		}
	}
	if blockers == 0 {
		return // no `blocks`-blocker at all — nothing to gate on
	}
	if mergedFix == "" {
		// Every blocker closed, but none carried a merged MR — a wontfix/duplicate
		// close, not a merged fix. The bug still needs a real fix (acceptance #2).
		slog.Info("auto-close: blocked bug's blockers all closed but none merged an MR; leaving open",
			"bug", bug.ID, "feature", "ka-0000003106")
		return
	}

	s.autoCloseBlockedBug(ctx, bug, mergedFix)
}

// blockerCarriesMergedMR reports whether a closed blocking fix-task landed a
// MERGED MR — either via a `produces`→ merge-request bead that is merged (the
// canonical signal, reusing linkedMRBeads + isMRMerged exactly as leaf
// close-on-merge does), or via a legacy merged marker on the fix-task's own
// fields (the pre-canonical-MR-bead fallback firstUnmergedMR keeps). Best-effort:
// a dep-fetch failure inside linkedMRBeads yields no MRs, so this returns false
// and the bug is left open rather than closed on unproven merge state.
func (s *BeadsServer) blockerCarriesMergedMR(ctx context.Context, blocker *model.Bead) bool {
	for _, mr := range s.linkedMRBeads(ctx, blocker) {
		if isMRMerged(parseFieldsToStringMap(mr.Fields)) {
			return true
		}
	}
	// Legacy fallback: a pre-migration fix-task may carry the merged marker on its
	// own fields rather than a canonical MR bead (mirrors firstUnmergedMR).
	return isMRMerged(parseFieldsToStringMap(blocker.Fields))
}

// autoCloseBlockedBug closes one eligible bug fixed via its blocking fix-task(s),
// mirroring autoCloseMergedLeaf's event + mutation bookkeeping so live
// subscribers and the audit trail observe the transition identically. The
// freshly-closed bug re-enters afterBeadClosed so a parent epic it was the last
// open child of drains in turn.
func (s *BeadsServer) autoCloseBlockedBug(ctx context.Context, bug *model.Bead, mergedFixID string) {
	prevStatus := bug.Status
	closed, err := s.store.CloseBead(ctx, bug.ID, autoCloseActor)
	if err != nil || closed == nil {
		slog.Warn("auto-close: failed to close blocked bug on fix merge",
			"bug", bug.ID, "err", errString(err), "feature", "ka-0000003106")
		return
	}
	s.addAutoComment(ctx, bug.ID, fmt.Sprintf(
		"auto-closed: blocking fix-task %s closed with a merged MR, and every `blocks`-blocker of this "+
			"bug is closed (close-on-blocking-fix-merge contract, ka-0000003106). Reopen if the fix is "+
			"incomplete, or stamp the no-auto-close label to opt out.", mergedFixID))
	s.recordAndPublish(ctx, events.TopicBeadClosed, closed.ID, autoCloseActor, events.BeadClosed{
		Bead:     closed,
		ClosedBy: autoCloseActor,
	})
	s.recordMutation(ctx, closed.ID, autoCloseActor, "status",
		string(prevStatus), string(closed.Status), model.MutationTypeUpdate)
	slog.Info("auto-closed blocked bug on fix merge",
		"bug", bug.ID, "merged_fix", mergedFixID, "feature", "ka-0000003106")
	// Cascade upward: a closed bug may have been the last open child of an epic.
	s.afterBeadClosed(ctx, closed)
}

// addAutoComment records a best-effort system audit comment on beadID. Failures
// are logged and swallowed — the comment is provenance, not a precondition.
func (s *BeadsServer) addAutoComment(ctx context.Context, beadID, text string) {
	comment := &model.Comment{
		BeadID:    beadID,
		Author:    autoCloseActor,
		Text:      text,
		CreatedAt: time.Now().UTC(),
	}
	s.inheritCommentOperator(ctx, comment)
	if err := s.store.AddComment(ctx, comment); err != nil {
		slog.Warn("auto-close: failed to add audit comment",
			"bead", beadID, "err", err.Error(), "feature", "ka-0000001587")
	}
}

// inheritCommentOperator copies the commented bead's operator onto a daemon
// audit comment. comments.operated_by_id is NOT NULL (migration 022), so a
// comment with no operator fails to insert and the auto-transition leaves no
// trace (gd-ifFUTNHbqm: three cascade-closed beads carried no comment). The
// bead's own operator is the same FK the auto-parent edge inherits
// (ka-0000007722).
func (s *BeadsServer) inheritCommentOperator(ctx context.Context, comment *model.Comment) {
	if comment.OperatedByID != nil {
		return
	}
	bead, err := s.store.GetBead(ctx, comment.BeadID)
	if err != nil || bead == nil {
		return
	}
	comment.OperatedBy = bead.OperatedBy
	comment.OperatedByID = bead.OperatedByID
}

// firstLiveGardenerChild returns the id of the first OPEN + enabled
// trigger/schedule descendant of epicID (a live gardener whose terminal goal is
// unmet), or "" when none remains.
//
// It enumerates the SAME cycle-safe parent-child subtree as drainParentEpic
// (ParentSubtreeID), narrowed to the gardener-carrying types and the open
// statuses, then keeps only rows whose enabled field is not the literal string
// "false" — precisely the trigger-engine's own liveness predicate
// (`fields.enabled != "false"`, gardenio triggers.go). A trigger's enabled field
// is boolean-typed on the schema, so it serializes as JSON true/false;
// parseFieldsToStringMap renders that back to "true"/"false", and an absent
// field (rendered "") is treated as live — matching the engine, which defaults
// an unset enabled to on.
//
// The enumeration is PAGED (bug ka-0000005183), reusing the closed-descendant
// walk's page window + max-page cap. A prior single-shot `Limit: 500` fetch had
// the same silent-truncation class as Gate C (ka-0000008187): with no Sort,
// postgres defaulted to `created_at DESC`, so the cap kept the 500 NEWEST open
// gardener descendants and dropped the oldest. If the ONLY enabled (live)
// gardener were among the dropped oldest, this returned "" and the epic
// hard-closed with a live gardener mid-loop — the exact failure mode of
// ka-0000001525. Paging over `created_at` ASC reads every gardener regardless of
// age; the walk short-circuits on the first live one, so peak memory stays one
// page and the common (1-few gardeners, 1 page) case is unchanged.
//
// Best-effort: a store error logs and returns "" (do not block the close on an
// enumeration failure — the other gates still apply).
func (s *BeadsServer) firstLiveGardenerChild(ctx context.Context, epicID string) string {
	for page := range closedKidsMaxPages {
		gardeners, _, err := s.store.ListBeads(ctx, model.BeadFilter{
			ParentSubtreeID: epicID,
			Type:            liveGardenerChildTypes,
			Status:          openSubtreeChildStatuses,
			Sort:            "created_at",
			Limit:           closedKidsPageSize,
			Offset:          page * closedKidsPageSize,
		})
		if err != nil {
			slog.Warn("auto-close: failed to enumerate gardener children",
				"epic", epicID, "err", err.Error(), "feature", "ka-0000001587", "bug", "ka-0000001525")
			return ""
		}
		for _, c := range gardeners {
			if c == nil {
				continue
			}
			// Mirror the trigger-engine liveness read exactly: enabled is live
			// unless explicitly the string "false" (an absent/unset field is on).
			enabled := strings.TrimSpace(parseFieldsToStringMap(c.Fields)["enabled"])
			if strings.EqualFold(enabled, "false") {
				continue
			}
			return c.ID
		}
		if len(gardeners) < closedKidsPageSize {
			break // short page = last page
		}
	}
	return ""
}

// firstUnmergedMR returns the id of the first bead in kids that linked an MR
// (mr_url present) whose MR is NOT merged, or "" when every MR-linked bead is
// merged (and beads without an MR are ignored).
//
// The authoritative merged signal lives on the canonical type=merge-request
// bead keyed by mr_url, NOT on the issue bead. The gitlab-bridge deliberately
// stopped stamping mr_state/mr_merged onto issue beads (ka-0000000586 /
// ka-0000008134; the ban is asserted by gitlab_sync_taskbead_banlist_test.go),
// so a closed child's own fields carry no merge marker by design. We look up
// the MR bead once for the whole batch and prefer it, mirroring
// filterMRBeads (gardenio gb/mr.go). The issue-bead fields remain a legacy
// fallback for pre-migration rows that predate the canonical MR bead.
func (s *BeadsServer) firstUnmergedMR(ctx context.Context, kids []*model.Bead) string {
	mrByURL := s.mrBeadsByURL(ctx, kids)
	for _, b := range kids {
		if b == nil {
			continue
		}
		f := parseFieldsToStringMap(b.Fields)
		url := strings.TrimSpace(f["mr_url"])
		if url == "" {
			continue // no MR linked — nothing to gate on
		}
		// Prefer the canonical MR bead's fields; fall back to the (legacy)
		// issue-bead fields when no MR bead is linked yet.
		merged := isMRMerged(f)
		if mr := mrByURL[url]; mr != nil {
			merged = isMRMerged(parseFieldsToStringMap(mr.Fields))
		}
		if merged {
			continue
		}
		return b.ID
	}
	return ""
}

// mrBeadsByURL resolves the canonical type=merge-request beads for the mr_urls
// carried by kids, returning a mr_url → MR bead index.
//
// Each distinct mr_url is looked up EXACTLY via the indexed fields->>'mr_url'
// equality filter — NOT a `Limit: N` list of all merge-request beads. A bounded
// batch would silently miss the target bead once prod holds more MR beads than
// the window (ka-0000003555: a big Limit is a silent-truncation bug), which
// would re-trigger this very bug by falling back to the empty issue-bead field.
// The mr_url set here is small (one per MR-linked child of a single epic), so a
// handful of point lookups is cheaper and correct. Best-effort per URL: a
// failed lookup is logged and skipped, so firstUnmergedMR degrades to the
// legacy issue-bead read for that child rather than crashing the close.
func (s *BeadsServer) mrBeadsByURL(ctx context.Context, kids []*model.Bead) map[string]*model.Bead {
	out := map[string]*model.Bead{}
	for _, b := range kids {
		if b == nil {
			continue
		}
		u := strings.TrimSpace(parseFieldsToStringMap(b.Fields)["mr_url"])
		if u == "" || out[u] != nil {
			continue // no MR, or this url already resolved
		}
		mr, err := s.mrBeadForURL(ctx, u)
		if err != nil {
			slog.Warn("auto-close: failed to resolve merge-request bead; falling back to issue-bead MR fields",
				"mr_url", u, "err", err.Error(), "feature", "ka-0000001587")
			continue
		}
		if mr != nil {
			out[u] = mr
		}
	}
	return out
}

// mrBeadForURL returns the canonical type=merge-request bead for mrURL, or nil
// when there is none. Point lookup: type=merge-request AND fields->>'mr_url' =
// mrURL. All statuses — a merged MR's canonical bead is status=closed
// (config.go type:merge-request), so closed MUST be reachable. Limit 2 only to
// detect an unexpected duplicate without over-fetching.
func (s *BeadsServer) mrBeadForURL(ctx context.Context, mrURL string) (*model.Bead, error) {
	mrBeads, _, err := s.store.ListBeads(ctx, model.BeadFilter{
		Type:   []model.BeadType{mergeRequestType},
		Fields: map[string]string{"mr_url": mrURL},
		Limit:  2,
	})
	if err != nil {
		return nil, err
	}
	for _, mr := range mrBeads {
		if mr != nil {
			return mr, nil
		}
	}
	return nil, nil
}

// isMRMerged reports whether the MR-tracking fields mark the linked MR as
// merged. The canonical type=merge-request bead records the outcome as
// mr_merged=="true", mr_outcome=="merged", or mr_state=="merged" — ALL three
// must be checked, not just mr_state: a merged MR can carry mr_state=="opened"
// alongside mr_merged/mr_outcome (ka-0000005427 / !5404). This matches the
// merged predicate in gardenio gb/mr.go (filterMRBeads) and gitlab_sync.go.
func isMRMerged(f map[string]string) bool {
	if strings.EqualFold(strings.TrimSpace(f["mr_merged"]), "true") {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(f["mr_outcome"]), "merged") {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(f["mr_state"]), "merged")
}

// errString renders an error for a log field, tolerating nil.
func errString(err error) string {
	if err == nil {
		return "<nil bead>"
	}
	return err.Error()
}

// appendIfAbsent returns labels with want appended only if not already present.
func appendIfAbsent(labels []string, want string) []string {
	if hasLabel(labels, want) {
		return labels
	}
	return append(labels, want)
}
