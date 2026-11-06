package main

import (
	"context"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
	"gardenio/controller/internal/plans"
)

// The Plans surface (epic ka-0000001409). A plan is an ordinary epic with the
// `plan` label: its issue children are the steps, its child `type=bundle` doc is
// the narrative, and its clarifying-question decisions live in the subtree. It
// composes existing primitives rather than adding storage. The only new
// mutations (operator-only add/close step, narrative edit) are thin clones of
// existing handlers (create_gardener.go, bead_actions.go, description_edit.go).

// planLabel is the marker label that promotes an epic into a plan. It
// AND-composes with `project:<name>`, so the /plans index is one indexed query.
const planLabel = plans.Label

// plansListLimit caps the plans pulled for the index; overflow shows the
// truncation banner rather than silently dropping rows.
const plansListLimit = 200

// planDecisionStaleThreshold is the wait-age past which the "N to answer" chip
// tints stale (ka-0000004999). An open decision blocks the whole plan on a
// human; the measured backlog had 23 decisions >30d old (ka-0000000932).
const planDecisionStaleThreshold = 14 * 24 * time.Hour

// ShowDecisionChip reports whether the index card's "N to answer" chip renders:
// OpenDecisions > 0 and the plan is not terminal (closed/deferred), the twin of
// ShowEscalationBadge (ka-0000005352 iter-46). Why: a leftover open decision on
// a finished plan advances nothing, yet the chip is the top attention tier
// (planAttentionDecision, plans_attention.go); seen live on closed plans
// ka-0000003452 and ka-0000000492.
//
// Display-only: OpenDecisions is untouched, so planAttention and
// planBoardLaneKey are unaffected and the detail page still lists the decisions.
func (r planRowView) ShowDecisionChip() bool {
	return r.OpenDecisions > 0 && !planStatusTerminal(r.Status)
}

// planFreshnessStaleThreshold is the no-activity age past which an active
// plan's "updated Xh ago" stamp tints loud (ka-0000005352 iter-38). 7d matches
// the escalation badge; only non-terminal plans are measured against it.
const planFreshnessStaleThreshold = 7 * 24 * time.Hour

// planFreshnessStale reports whether an active plan's lastActivityAt (the value
// the "updated" stamp renders, from planLastActivity) is older than
// planFreshnessStaleThreshold. False for a terminal (closed/deferred) plan,
// whose age is history, and for a zero lastActivityAt.
func planFreshnessStale(status string, lastActivityAt, now time.Time) bool {
	if planStatusTerminal(status) || lastActivityAt.IsZero() {
		return false
	}
	return now.Sub(lastActivityAt) >= planFreshnessStaleThreshold
}

// planLastShipStaleThreshold is the delivery-drought age past which the "last
// ship · Nd" facet tints loud (ka-0000005352 iter-59): the shared 7d whole-plan bar.
const planLastShipStaleThreshold = 7 * 24 * time.Hour

// planLastShipAge returns the humanized age since the plan last closed a step
// ("18d") and whether it crossed planLastShipStaleThreshold, for the /plans
// card (ka-0000005352 iter-59). It returns ("", false) unless every gate holds:
//   - non-terminal status (a closed/deferred plan's last ship is history);
//   - a datable lastShip (no closed step means no cadence to age);
//   - stepsOpen > 0 (an all-closed plan gets the "✓ steps done" marker instead);
//   - !freshnessStale (a loud freshness stamp already flags it; this facet is for
//     a plan that looks fresh yet ships nothing, e.g. ka-0000008223).
func planLastShipAge(status string, lastShip time.Time, stepsOpen int, freshnessStale bool, now time.Time) (age string, stale bool) {
	if planStatusTerminal(status) || lastShip.IsZero() || stepsOpen <= 0 || freshnessStale {
		return "", false
	}
	return fmtTimeAgo(lastShip, now), now.Sub(lastShip) >= planLastShipStaleThreshold
}

// planReadyStaleThreshold is the awaiting-approval age past which a READY
// plan's lifecycle chip tints loud (ka-0000005352 iter-47); the shared 7d
// "a human has been the blocker" bar.
const planReadyStaleThreshold = 7 * 24 * time.Hour

// planReadyWaitAge returns the humanized awaiting-approval wait ("4d") and its
// stale flag for a READY plan's lifecycle chip (ka-0000005352 iter-47).
// It ages on the epic's own updatedAt, not the folded lastActivityAt: the
// epic's last write is the plan_state → ready transition, while child/crew
// churn keeps lastActivityAt fresh (ka-0000000093 waited 4d with a fresh stamp).
// Returns ("", false) when chip is not READY or updatedAt is zero.
func planReadyWaitAge(chip planState, updatedAt, now time.Time) (age string, stale bool) {
	if chip != planStateReady || updatedAt.IsZero() {
		return "", false
	}
	return fmtTimeAgo(updatedAt, now), now.Sub(updatedAt) >= planReadyStaleThreshold
}

// planEnrichJoinConcurrency bounds in-flight joins within each /plans enrich
// wave (planIndexEnrichTimed). Waves carry 4 joins today, so this only matters
// if a wave grows (ka-0000004530).
const planEnrichJoinConcurrency = 4

// planRowView is one card on the /plans index.
type planRowView struct {
	ID         string
	Title      string
	ShortTitle string      // shown in place of Title on the card when set (gd-V5nGseRS5f)
	Heraldry   string      // the plan's arms, canonical; "" when none (arms.go)
	Badge      *badgeImage // the plan's badge picture, for the deck cover; nil when none
	Project    string
	Status     string
	// Priority is the epic's 0-4 priority (0 = highest), off the list query.
	// The /deck card renders it; the /plans index does not yet.
	Priority int
	// PlanState is the RAW plan_state field ("" for legacy/ungated plans;
	// planStateOf's vocabulary). The board lane (planBoardLaneKey) and the
	// Approve-vs-pause lever need the true value, so it is never reconciled.
	PlanState string
	// LifecycleChip is plan_state reconciled against terminal status for the
	// display chip (ka-0000005352 iter-27, planLifecycleChipState): a terminal
	// plan drops a stale draft/ready/building chip; a genuine `complete` stays.
	LifecycleChip string
	// ReadyWaitAge / ReadyWaitStale fold the awaiting-approval wait onto the
	// READY chip ("ready · 4d", ka-0000005352 iter-47); Stale past
	// planReadyStaleThreshold. Set by planReadyWaitAge: empty/false unless
	// LifecycleChip is "ready" and the epic's updated_at is datable.
	ReadyWaitAge   string
	ReadyWaitStale bool
	// Ago is the humanized newest activity across the epic and its rendered
	// steps/phases (planLastActivity, ka-0000002873), not the epic's own
	// updated_at. AgoRFC is the precise instant for the tooltip.
	Ago    string
	AgoRFC string
	// AgoCompact is the same instant in the compact age vocabulary ("7h", "2d";
	// fmtTimeAgo), rendered in the card head beside the plan ID (ka-0000005352
	// iter-52). Why: moving it out of the wrapping meta chip strip saves a line
	// per card. Empty for an undatable plan.
	AgoCompact string
	// lastActivityAt is the native instant behind Ago, so sortPlanRows
	// (plans_attention.go) can order within a tier by recency. Zero sinks to the
	// bottom of its tier.
	lastActivityAt time.Time
	// FreshnessStale marks an active plan whose lastActivityAt crossed
	// planFreshnessStaleThreshold (ka-0000005352 iter-38); the template tints
	// .plan-card-updated loud. Set by planFreshnessStale, so it is false for a
	// terminal plan (its age is history) and for an undatable one.
	FreshnessStale bool
	// Step rollup over the epic's issue subtree.
	StepsTotal  int
	StepsClosed int
	StepsOpen   int
	PctDone     int
	// StepsInProgress / StepsBlocked tally the RENDERED steps by status
	// (ka-0000000851) from the same Grid the card draws (tallyPlanGrid), so they
	// agree with StepsTotal/StepsClosed. Zero renders nothing.
	StepsInProgress int
	StepsBlocked    int
	// StepsNotStarted is StepsTotal − StepsClosed − StepsInProgress −
	// StepsBlocked, clamped at 0 (ka-0000007226): the progress bar's fourth
	// segment. Clamped because a step parented under two phases counts twice in
	// StepsTotal but once per status bucket (see renderedStepRollup / tallyPlanGrid).
	StepsNotStarted int
	// StepsAllDone marks a "ghost-done" plan (ka-0000005352 iter-41): every
	// rendered step closed (StepsTotal>0 && StepsClosed==StepsTotal) but the epic
	// not terminal, so it needs a close or wrap-up. Drives the "✓ steps done"
	// marker. False for a terminal plan, and when StepsInProgress or StepsBlocked
	// is non-zero (the double-parent overcount) so it never contradicts the tally.
	StepsAllDone bool
	// CloseoutFlagged carries the daemon's `autoclose-ready` label, which kbeads'
	// auto-close cascade stamps on a drained plan epic instead of closing it,
	// because the operator's Mark-complete is the gate (flagPlanEpicReady,
	// decision ka-0000004595; thread ka-0000001785, decision ka-0000001273). Read
	// off b.Labels at enrich; false for a CLOSED plan. planCloseoutReady /
	// planNeedsCloseout (plans_closeout.go) project it into the Done? lane + badge.
	CloseoutFlagged bool
	// Closeout is the derived "looks finished — retire it?" badge
	// (planCloseoutBadgeFor), including whether plan_state=complete is legally
	// reachable so Mark-complete is never offered on a plan the daemon would
	// reject (plans_closeout.go's legality table). Set at the shared enrich seam.
	Closeout planCloseoutBadge
	// PhaseCur / PhaseTotal are a multi-phase plan's milestone, "phase 2/5"
	// (ka-0000005352 iter-58), from the row's Grid (planPhasePosition).
	// PhaseTotal counts real phase rows (planGridPhase.IsPhase, not the "Other"
	// trailer); PhaseCur is the 1-based earliest phase with a non-closed square,
	// clamped to PhaseTotal when all are done. Both zero for fewer than two
	// phases; the template renders the facet only when PhaseTotal >= 2.
	PhaseCur   int
	PhaseTotal int
	// LastShipAge / LastShipStale are the "last ship · 18d" facet (ka-0000005352
	// iter-59): the age since the newest step close, set by planLastShipAge. Why
	// separate from Ago: lastActivityAt counts any touch, so a plan with churning
	// comments reads fresh while shipping nothing (ka-0000008223: 3.3d fresh,
	// 18.1d since a close). LastShip rides the same batched child walk as the rollup.
	LastShipAge   string
	LastShipStale bool
	// OpenDecisions is the count of open clarifying-question decisions in the
	// plan subtree — the "waiting on you" signal.
	OpenDecisions int
	// OldestDecisionAge is the humanized wait of the oldest open decision
	// ("32d"), folded into the "N to answer" chip (ka-0000004999). From the
	// decision beads planIndexStepData already hydrated (OldestDecisionAt).
	// Empty with no open decision or a zero CreatedAt.
	OldestDecisionAge string
	// OpenDecisionStale is true when the oldest open decision has waited past
	// planDecisionStaleThreshold; it tints the chip.
	OpenDecisionStale bool
	HasBundle         bool
	// OperatedBy / OperatedByDisplay / OperatedByEmail / OperatedBySlug are the
	// plan's attribution (S2), off the list query; rendered via beadOperatorChip.
	// CreatedBy is the chip resolver's fallback when operated_by is empty.
	OperatedBy        string
	OperatedByDisplay string
	OperatedByEmail   string
	OperatedBySlug    string
	CreatedBy         string
	// CurrentOrNextStep is the single "now / next" step on the card (S3); nil
	// when the plan has no open step. Built from the batch-enriched children,
	// not a per-plan planTimeline() call.
	CurrentOrNextStep *planCardStep
	// Grid is the mini task-grid (ka-0000001226): one row per phase, one
	// status-colored square per task, previewed via preview_popover.js. Built
	// from the same batched child walk (buildPlanGrid); nil with no steps.
	Grid []planGridPhase
	// AssistantState is the bound assistant's load-time state (S4): "live",
	// "parked" or "none" ("" is treated as "none"). "waking" never appears; only
	// the detail page's SSE shows that transient state.
	AssistantState string
	// AssistantID is the bound assistant's agent-bead id, for the spawn/message
	// form's HTMX target scoping. Empty when none.
	AssistantID string
	// AssistantURL is where the assistant chip links: the coopmux terminal
	// (<COOPMUX_PUBLIC_URL>#<pod_name>, ka-0000007019) when derivable, else the
	// bead page /{id}. Never /agents/{id}, which 404s. Empty without an assistant.
	AssistantURL string
	// AssistantExternal is true when AssistantURL is the external coopmux page
	// (so the template opens it in a new tab, matching the other coopmux links).
	AssistantExternal bool
	// Stall is the stalled-plan signal (ka-0000005581): open work nothing is
	// watching, or a step in_progress under a dead agent (the ka-0000004457
	// orphaning). Renders a ⚠ badge with Stall.Reason as tooltip.
	Stall planStallInfo
	// Escalations are the open escalations that `blocks` a rendered step or the
	// epic (epic ka-0000005480); Count > 0 renders the ⛔ badge.
	Escalations planEscalationInfo
	// EscalationOldestAge is the humanized wait of the oldest open blocking
	// escalation ("40d") on the ⛔ badge (ka-0000003887, iteration 8), via
	// planEscalationInfo.OldestAge (planEscalationsBulk sorts refs oldest-first).
	// Empty with no escalation or an unset stamp.
	EscalationOldestAge string
	// EscalationStale is true past planEscalationStaleThreshold (7d); it tints
	// the ⛔ badge. Mirrors OpenDecisionStale.
	EscalationStale bool
	// EscalationOldestOp is the oldest blocking escalation's humanized operation
	// type ("merge MR", "AWS IAM", "terraform") on the ⛔ badge, so the operator
	// sees what to do (ka-0000005352 iteration 37). Same ref as the wait-age
	// (OldestOperationLabel). Empty with no escalation, no operation_type, or an
	// unrecognized one (humanizeEscalationOp fails quiet).
	EscalationOldestOp string
	// Crew is the card's live-crew tally (ka-0000004801): crew dispatched onto
	// the plan's steps and how many are working / done / stuck. Built page-wide
	// by planCrewBulk (one edge batch + one hydration batch, no per-row fan-out)
	// and classified by classifyCrewBead like the cockpit roster. Renders when
	// Crew.Any().
	Crew planCrewRollup
	// MRs is the plan's open merge requests (Status != closed), built page-wide
	// by planOpenMRsBulk (one produces-edge batch + one hydration batch). Renders
	// click-through pills when MRs.Any().
	MRs planMRInfo
	// Blocked holds the forward `blocks` blockers of a plan whose own status is
	// "blocked" (ka-0000005352 iter-55), so the status chip can show "· N blocked
	// · age", or "· ✓ cleared" when every blocker is closed. Why: most live
	// blocked plans had only closed blockers. Populated only for status=blocked
	// plans (planBlockedBulk); it never changes the status or the lane.
	Blocked planBlockerInfo
	// BlockedOldestAge / BlockedOldestStale are the oldest open blocker's age and
	// stale flag, precomputed from Blocked against s.now() in the enrich loop
	// because a template cannot call OldestAge(now). Empty/false with no open or
	// datable blocker.
	BlockedOldestAge   string
	BlockedOldestStale bool
	// GardenerID / GardenerState / Chip are the plan's bound plan gardener (the
	// trigger bead labeled gardener-epic:<id>) and its loop state folded with the
	// assistant's liveness into the same planAgentChip the /plans/{id} header
	// renders (epic ka-0000004792 step 1, task ka-0000004720). GardenerID is
	// empty when none is bound; Chip then shows the assistant-only fallback.
	GardenerID    string
	GardenerState planGardenerState
	Chip          planAgentChipView
	// GardenerToggleError is the inline note beside the gardener toggle (step 3,
	// ka-0000003881), set only on the row re-render handlePlanGardenerRowToggle
	// emits when a flip is rejected (the R6 build gate, or a write failure).
	GardenerToggleError string
	// GardenerDispatchNotice is the outcome note beside the Dispatch button
	// (ka-0000001290), set only on the row re-render handlePlanRowDispatch emits,
	// for success, no-open-step and failure alike.
	GardenerDispatchNotice string
	// GardenerDispatchError marks GardenerDispatchNotice as an error so the
	// template tones it red.
	GardenerDispatchError bool
	// SearchText is the lowercased haystack the client-side /plans search box
	// substring-matches (title, id, project, status, attribution, current/next
	// step title), built by planCardSearchText; rendered as data-search.
	SearchText string
	// openDecisionIDs are the plan's open decision ids from the batched child
	// walk (planIndexStepInfo.OpenDecisionIDs), which the Layer-1 unhomed
	// backstop subtracts as already surfaced (plans_unhomed.go, ka-0000004983).
	// row.Escalations already carries the surfaced escalation ids.
	openDecisionIDs []string
}

// planCardStep is the compact current-or-next step on an index card (S3,
// ka-0000002962). Why lighter than planStepView: MR/blocker/timestamp lookups
// would be an N×(MR+blocker) fan-out across plansListLimit cards.
type planCardStep struct {
	// ID lets the "now" chip deep-link to /plans/{plan}#step-{ID}
	// (ka-0000000150); empty renders the chip as plain text.
	ID     string
	Number int
	Title  string
	// ShortTitle is the step's short_title, shown in place of Title on the
	// card face when set; the chip's title= keeps the full title (gd-V5nGseRS5f).
	ShortTitle string
	Status     string
	// updatedAt is the step's last-modified instant, stamped by
	// pickCurrentOrNextStep as the input to setStepAge. Zero means no age.
	updatedAt time.Time
	// NowAge is the humanized running-age of an in_progress ("now") step
	// (ka-0000005352 iter-43), set by setStepAge. Empty for a next/blocked or
	// undatable step.
	NowAge string
	// NowStale is true when the running-age crossed planStallStaleThreshold
	// (24h, the ⚠ stall badge's bar in plans_stall.go); it tints the chip.
	NowStale bool
	// BlockedAge is the "blocked since" age of a status=blocked step
	// (ka-0000005352 iter-48), set by setStepAge off updatedAt (its last
	// transition into blocked). Why: no other card signal shows it — deriveStall
	// (plans_stall.go) counts only in_progress steps, and the freshness stamp
	// shows the plan's newest activity. The "next" arm stays ageless: a ready
	// step is not wedged.
	BlockedAge string
	// BlockedStale is true when the wedge-age crossed planStallStaleThreshold (24h).
	BlockedStale bool
}

// Kind is the now/next chip's state token, the single source for both the CSS
// class (plan-step-kind-{Kind}) and the label (KindLabel). States
// (ka-0000002840, iteration 14):
//   - "now": in_progress.
//   - "blocked": the next open step is blocked and cannot start
//     (pickCurrentOrNextStep returns the first non-closed step, blocked included).
//   - "next": any other non-closed status.
//
// Closed steps never reach here.
func (s planCardStep) Kind() string {
	switch s.Status {
	case "in_progress":
		return "now"
	case "blocked":
		return "blocked"
	default:
		return "next"
	}
}

// KindLabel is the chip's pill text; identical to Kind() so label and color
// cannot drift.
func (s planCardStep) KindLabel() string { return s.Kind() }

// KindTitle is the chip's tooltip lead-in, the long form of Kind().
func (s planCardStep) KindTitle() string {
	switch s.Status {
	case "in_progress":
		return "In progress"
	case "blocked":
		return "Blocked — cannot start"
	default:
		return "Next open step"
	}
}

// setStepAge sets the step's wait-age from updatedAt, by Kind():
//   - "now": NowAge / NowStale, the running-age (ka-0000005352 iter-43).
//   - "blocked": BlockedAge / BlockedStale, the wedge-age (ka-0000005352 iter-48).
//
// It is a no-op on the "next" arm, a nil receiver, and a zero updatedAt, so
// callers may call it on a possibly-nil CurrentOrNextStep. Both stale flags use
// planStallStaleThreshold (24h).
func (s *planCardStep) setStepAge(now time.Time) {
	if s == nil || s.updatedAt.IsZero() {
		return
	}
	stale := now.Sub(s.updatedAt) >= planStallStaleThreshold
	switch s.Kind() {
	case "now":
		s.NowAge = fmtTimeAgo(s.updatedAt, now)
		s.NowStale = stale
	case "blocked":
		s.BlockedAge = fmtTimeAgo(s.updatedAt, now)
		s.BlockedStale = stale
	}
}

// Dispatchable reports whether the step can be handed to a crew now: false
// only for a blocked step (Kind() == "blocked"). It gates the card's
// "Dispatch #N →" button and the handler's refusal (ka-0000005352 iteration
// 44). Why: a blocked step waits on its blocker, so seeding "it's ready to
// start" is wrong; this closes the hole iter-14 (ka-0000002840) noted, as
// iter-25 (ka-0000000857) did for terminal plans.
func (s planCardStep) Dispatchable() bool { return s.Kind() != "blocked" }

// planStepView is one TODO step (a direct issue child of the plan epic),
// carrying the full-row contract (S4 ka-0000001060, Q3 decision ka-0000000700):
// step number, assignee, linked MR, open blockers, and lifecycle timestamps.
type planStepView struct {
	ID         string
	Title      string
	ShortTitle string // shown in place of Title when set (gd-V5nGseRS5f)
	Type       string
	Status     string
	Priority   int
	Closed     bool
	// Number is the stable 1..N step number from the group's display order
	// (the fields.step_order overlay, then creation order; ka-0000007445), so it
	// does not renumber as steps close or reopen.
	Number int
	// IsFirst / IsLast mark the step's position WITHIN its rendered group (the
	// flat list, or one phase's steps) so the reorder ▲▼ controls disable at the
	// boundary. Set by buildStepRows over the display-ordered set.
	IsFirst bool
	IsLast  bool
	// Assignee is the raw assignee name; the template linkifies agent-shaped
	// values via the request-scoped agentLink resolver.
	Assignee string
	// MRState / MRURL surface the step's linked merge-request bead (first
	// forward `produces` edge), resolved via the batched inboxMRResolver.
	// Empty state means "no linked MR" — the pill suppresses itself.
	MRState string
	MRURL   string
	// BlockedBy lists the step's OPEN blockers (forward `blocks`-typed deps
	// whose target bead is not closed). Closed blockers are dropped — the
	// chip is a "waiting on" signal, not history.
	BlockedBy []planStepBlockerView
	// CreatedAgo / ClosedAgo are humanized lifecycle stamps; the *RFC
	// variants feed title tooltips with the precise instant.
	CreatedAgo   string
	CreatedAtRFC string
	ClosedAgo    string
	ClosedAtRFC  string
	// UpdatedAt is the step's last-modified instant, not rendered on the row; it
	// feeds planLastActivity (ka-0000002873) so the plan's freshness reflects
	// step churn, not just the epic's rarely-moving updated_at.
	UpdatedAt time.Time
	// SectionRef is the "§ section" chip linking the step to its part of the
	// plan's narrative bundle (ask 2, ka-0000005926), set when the description
	// has a `see ka-BUNDLE#section` ref to the plan's bundle. Empty Label renders
	// no chip.
	SectionRef planSectionRefView
}

// planStepBlockerView is one open blocker chip on a step row.
type planStepBlockerView struct {
	ID    string
	Title string
	// IsEscalation marks a type=escalation blocker (epic ka-0000005480, WS5),
	// rendered as a distinct hard-stop chip carrying its operation type.
	IsEscalation  bool
	OperationType string
}

// planSectionRefView is the step→bundle-section chip. Label is the anchor text
// ("C1.1"); Href is the viewer-relative link ("ka-BUNDLE#c1-1") appended to
// BasePath. Lifted from the step description at render time, since the daemon
// stores only the bare ka-id on an edge (bundle ka-0000002278).
//
// Slug is the bare fragment ("c1-1"), the same id headingAnchorTransformer
// (anchor.go) mints on the narrative heading, so plan_gutter.js can map a
// highlighted heading to its step via data-section-slug (ka-0000003210).
type planSectionRefView struct {
	Label string
	Href  string
	Slug  string
}

// planDecisionView is one plan-scoped clarifying-question decision with the
// full answering surface (task ka-0000003157), so it can be resolved in place
// on /plans/{id}. PromptHTML/Options/Resolution come from buildDecisionView
// (type_views.go), matching the canonical decision card.
type planDecisionView struct {
	ID          string
	Title       string
	Prompt      string
	PromptHTML  template.HTML
	Status      string
	Ago         string
	Resolved    bool
	RequestedBy string
	// Context is the decision's context block, rendered above the options
	// (gd-PNtilz1bgs).
	Context decisionContextView
	// Evidence is the decision's screenshot strip, under the context
	// (gd-Pi7C83262N).
	Evidence   template.HTML
	Options    []decisionOption
	Resolution decisionResolution
	// CanResolve is true when the viewer can operate and the decision is still
	// open — gates the inline resolve/dismiss chips (mirrors decisionView.CanResolve).
	CanResolve bool
}

// handlePlans renders the /plans index: every plan-labeled epic in scope,
// attention-ranked (sortPlanRows), with its step rollup, open decisions,
// creator, current/next step and assistant state. Per-plan enrichment is one
// batched pass (planIndexEnrich, S0), not a per-row fan-out.
func (s *Server) handlePlans(w http.ResponseWriter, r *http.Request) {
	projectFilter := strings.TrimSpace(r.URL.Query().Get("project"))
	operatedByRaw := strings.TrimSpace(r.URL.Query().Get("operated_by"))
	// status defaults to "active" (open/in_progress/blocked/deferred); group to
	// a flat grid. Both are canonicalized so a hand-typed URL lands.
	statusFilter := normalizePlanStatus(r.URL.Query().Get("status"))
	groupKey := normalizePlanGroup(r.URL.Query().Get("group"))
	// The board is the default; ?group= implies the grid and an explicit ?view=
	// wins (plans_board.go). Group is inert on the board, so drop it.
	viewKey := resolvePlanView(r.URL.Query().Get("view"), r.URL.Query().Has("view"), groupKey)
	if planViewLaned(viewKey) {
		groupKey = planGroupNone
	}
	// ?zoom=canopy|grove|tree for the forest view (plans_tree.go); parsed always
	// so chip URLs preserve it.
	zoomKey := normalizePlanZoom(r.URL.Query().Get("zoom"))

	// S1: default-on operator scoping, as in handleActive. With no operated_by
	// key and an authenticated viewer, scope to their operator id; an explicit
	// value (incl. empty "?operated_by=" = show all) wins. There is no
	// created_by filter; operated_by is the author on a dogfood plan.
	email := oidcauth.EmailFromContext(r.Context())
	operatedBy, isMe := resolveOperatedByParam(operatedByRaw, email)
	effectiveOperatedBy := resolveDefaultOperatedBy(r, operatedBy)
	urlHasOperatedBy := r.URL.Query().Has("operated_by")
	operatedByIsUnowned := operatedByRaw == "null" || strings.EqualFold(operatedByRaw, "(none)")
	// The header chip preserves sibling filters on every link, as /active does.
	chipExtra := r.URL.Query()
	chipExtra.Del("operated_by")

	// filterState builds the control-strip chip URLs; each preserves the other
	// dimensions and operated_by.
	filterState := planFilterState{
		status:        statusFilter,
		project:       projectFilter,
		group:         groupKey,
		view:          viewKey,
		zoom:          zoomKey,
		operatedByRaw: operatedByRaw,
		operatedByKey: urlHasOperatedBy,
		base:          s.basePath + "/plans",
	}

	isOperator := viewerCanOperate(r)
	data := map[string]any{
		// R7 (garden_retire.go): "Open it in the garden" while /plans retires.
		"GardenHref":          s.gardenBanner(r, retirePlans, gardenPlansTarget),
		"ProjectFilter":       projectFilter,
		"StatusFilter":        statusFilter,
		"GroupKey":            groupKey,
		"ViewKey":             viewKey,
		"Plans":               []planRowView{},
		"Groups":              []planGroup(nil),
		"Lanes":               []planBoardLane(nil),
		"Truncated":           false,
		"IsOperator":          isOperator,
		"ViewerEmail":         email,
		"OperatedByFilter":    operatedByRaw,
		"OperatedByMe":        isMe,
		"OperatedByURLHasKey": urlHasOperatedBy,
		"EffectiveOperatedBy": effectiveOperatedBy,
		"OperatedByIsUnowned": operatedByIsUnowned,
		"OperatedByDisplay":   "",
		"ChipBasePath":        s.basePath + "/plans",
		"ChipExtraQuery":      chipExtra.Encode(),
		"Controls":            planControls{},
		"IsFiltered":          filterState.isFiltered(),
		"ClearURL":            filterState.clearURL(),
		// New-plan compose form (task 3b, plan ka-0000005794 Surface A): POSTs
		// /plans; the selector pre-selects ?project=.
		"ProjectOptions": []string(nil),
		"DefaultProject": projectFilter,
		// Deck entry point (ka-0000001498). Defaults here so the error path
		// renders a valid link; the success path sets the count.
		"DeckURL":            deckURLForPlans(s.basePath, projectFilter),
		"DeckAttentionCount": 0,
	}

	ctx := r.Context()

	// One ListProjectBeads round-trip feeds both the New-plan selector and the
	// project-filter chips (shown to every viewer).
	projectOptions := s.planProjectOptions(ctx, projectFilter)
	data["ProjectOptions"] = projectOptions
	data["Controls"] = filterState.controls(projectOptions)

	q := beadsapi.ListBeadsQuery{
		Types:            []string{"epic"},
		Labels:           []string{planLabel},
		Statuses:         planStatusEnum(statusFilter),
		Sort:             "-created_at",
		ExcludeScheduled: true,
		Limit:            plansListLimit,
	}
	if projectFilter != "" {
		q.Labels = append(q.Labels, "project:"+projectFilter)
	}
	// Daemon migration 024 dropped operated_by IS NULL semantics, so
	// operatedByIsUnowned filters nothing (mirrors handleActive, ka-0000003586).
	//
	// Step 1 (ka-0000008064, decision ka-0000000098 → 'involved'): an
	// operator-scoped view shows plans they are involved in (creator ∪
	// step-owner ∪ decision-resolver ∪ gardener-owner) via plansInvolvingOperator,
	// which returns the same shape as ListBeadsFiltered.
	var (
		res *beadsapi.ListBeadsResult
		err error
		// universe is the unscoped plan set for the Step 5 people strip: res when
		// the view is unscoped, otherwise fetched below.
		universe *beadsapi.ListBeadsResult
	)
	// Phase 1 instrumentation (ka-0000005453): time the list query separately
	// from enrich so Server-Timing splits the full /plans cost.
	listStart := time.Now()
	if !operatedByIsUnowned && effectiveOperatedBy != "" {
		res, err = s.plansInvolvingOperator(ctx, q, effectiveOperatedBy)
	} else {
		res, err = s.daemon.ListBeadsFiltered(ctx, q)
		universe = res // the scoped-out query already spans everyone's plans
	}
	listQueryDur := time.Since(listStart)
	if err != nil {
		s.logger.Error("plans: listing plan epics", "error", err)
		data["Error"] = "Failed to load plans."
		s.render(w, r, "plans.html", data)
		return
	}

	// Board Done-lane feed (plans_board.go): the active scope can't see closed
	// plans, so fetch recent closed ones and fold them (dedup by id) into the
	// same enrich pass. The forest (?view=tree) uses the same lanes, so it takes
	// the same feed.
	boardHasClosedLane := planViewLaned(viewKey) && statusFilter == planStatusActive
	boardClosedTruncated := false
	mainTruncated := res.Total > len(res.Beads)
	if boardHasClosedLane {
		closedBeads, truncated := s.planBoardClosedRows(ctx, q, effectiveOperatedBy, operatedByIsUnowned)
		boardClosedTruncated = truncated
		if len(closedBeads) > 0 {
			seen := make(map[string]struct{}, len(res.Beads))
			for _, b := range res.Beads {
				if b != nil {
					seen[b.ID] = struct{}{}
				}
			}
			for _, b := range closedBeads {
				if b == nil {
					continue
				}
				if _, dup := seen[b.ID]; dup {
					continue
				}
				res.Beads = append(res.Beads, b)
			}
		}
	}

	// The Step 5 people strip needs the unscoped universe; on the scoped path,
	// fetch it concurrently with the enrich joins (Phase 3, ka-0000007509).
	// Best-effort: an error just omits the strip. q is read-only from here on,
	// so the concurrent read is safe.
	type universeFetch struct {
		res *beadsapi.ListBeadsResult
		err error
	}
	var universeCh chan universeFetch
	if universe == nil {
		universeCh = make(chan universeFetch, 1)
		go func() {
			u, uerr := s.daemon.ListBeadsFiltered(ctx, q)
			universeCh <- universeFetch{res: u, err: uerr}
		}()
	}

	rows, enrichTiming := s.planIndexEnrichTimed(ctx, res.Beads)
	// ?status=stale (ka-0000004351, epic ka-0000007470) is derived: the daemon
	// has no lastActivityAt column, so the query uses the active set and this
	// keeps only FreshnessStale rows. It runs after enrich and before ranking and
	// laning, so every surface sees the swept set. The truncation banner is
	// turned off for the sweep.
	if statusFilter == planStatusStale {
		swept := rows[:0]
		for _, row := range rows {
			if row.FreshnessStale {
				swept = append(swept, row)
			}
		}
		rows = swept
		mainTruncated = false
	}
	// Attention-first ranking (Step 2, ka-0000004961): decisions, escalations,
	// ready-to-approve, stalled, each oldest-waiting-first, then the rest
	// freshest-first. Done here so the grid, board lanes and ?group= sections
	// all inherit the order.
	sortPlanRows(rows)
	// Phase 1 instrumentation (ka-0000005453): Server-Timing header plus a debug
	// log of list, enrich, per-wave and per-join times, so a reintroduced per-row
	// fan-out shows up as a join that scales with plan count.
	enrichTiming.ListQuery = listQueryDur
	w.Header().Set("Server-Timing", enrichTiming.ServerTimingHeader())
	s.logger.Debug("plans index enrich timing", enrichTiming.LogArgs()...)
	data["Plans"] = rows
	data["Truncated"] = mainTruncated

	// Deck pill (ka-0000001498, epic ka-0000000492): the count applies deckCards'
	// planAttention != None predicate to this view's rows, so it is the
	// current-view count; /deck itself is unscoped, so the two can differ.
	data["DeckURL"] = deckURLForPlans(s.basePath, projectFilter)
	data["DeckAttentionCount"] = countPlansNeedingHuman(rows)

	// Step 5 (ka-0000004302): browse-by-person strip from the unscoped universe;
	// each chip links ?operated_by=<peer>. Collect the overlapped fetch
	// (ka-0000007509); an error omits the strip.
	if universeCh != nil {
		uf := <-universeCh
		if uf.err != nil {
			s.logger.Debug("plans: browse-by-person universe query", "error", uf.err)
		} else {
			universe = uf.res
		}
	}
	if universe != nil {
		strip := buildPlanPeopleStrip(universe.Beads, s.basePath, chipExtra.Encode(), effectiveOperatedBy, email)
		if strip.Show {
			data["PeopleStrip"] = strip
		}
	}
	// Layer 1 unhomed-forks backstop (design ka-0000006833 §3, closes ka-0000003246):
	// open decisions + escalations in scope minus those surfaced on a plan, so a
	// fork belonging to no plan cannot orphan. Known plans are the rendered rows
	// ∪ the universe, so a fork pinned to a plan outside this view is not
	// flagged. Set only when Count > 0.
	planIDs := make(map[string]struct{}, len(rows))
	for i := range rows {
		planIDs[rows[i].ID] = struct{}{}
	}
	if universe != nil {
		for _, b := range universe.Beads {
			if b != nil && b.ID != "" {
				planIDs[b.ID] = struct{}{}
			}
		}
	}
	if unhomed := s.buildUnhomedForks(ctx, rows, planIDs, projectFilter); unhomed.Count > 0 {
		data["UnhomedForks"] = unhomed
	}
	// Master switch + global-pause banner (step 4, ka-0000004048): pause/resume
	// all plan gardeners via the gardeners-pause gate; the banner shows while
	// gardeners-pause or scheduler-pause is engaged.
	for k, v := range s.plansMasterSwitchData(r, isOperator, "") {
		data[k] = v
	}
	// Server-side visual grouping (?group=). nil when flat — the template then
	// renders one grid; otherwise it renders a section per group.
	if groupKey != planGroupNone {
		data["Groups"] = groupPlanRows(rows, groupKey)
	}
	// Board lanes (plans_board.go): a pure re-section of rows, which already
	// carry the Done-lane feed. An empty board keeps Lanes nil so the actionable
	// empty state renders.
	if planViewLaned(viewKey) && len(rows) > 0 {
		lanes := planBoardLanes(rows, nil, statusFilter, boardClosedTruncated)
		if viewKey == planViewTree {
			// The zoomable forest (plans_tree.go), built purely from the lanes and
			// each row's Grid, so it and the board cannot disagree. Lanes stays
			// unset so the template takes the forest branch.
			data["Forest"] = buildPlanForest(lanes, zoomKey, s.basePath, func(z string) string {
				// Grove is the default, so its chip URL omits the key.
				if z == planZoomGrove {
					z = ""
				}
				return filterState.chipURL(map[string]string{"view": planViewTree, "zoom": z})
			})
		} else {
			data["Lanes"] = lanes
		}
	}

	s.render(w, r, "plans.html", data)
}

// countPlansNeedingHuman counts rows in an attention tier, using deckCards'
// predicate (planAttention != planAttentionNone) so the /plans pill matches
// the deck.
func countPlansNeedingHuman(rows []planRowView) int {
	n := 0
	for i := range rows {
		if planAttention(rows[i]) != planAttentionNone {
			n++
		}
	}
	return n
}

// deckURLForPlans builds the /plans header pill's /deck link, carrying
// projectFilter; an empty one yields a bare /deck.
func deckURLForPlans(basePath, projectFilter string) string {
	u := basePath + "/deck"
	if projectFilter != "" {
		u += "?project=" + url.QueryEscape(projectFilter)
	}
	return u
}

// planProjectOptions returns the sorted project names for the New-plan
// selector (task 3b), always including "gardenio" (prepended when absent) and
// active (appended when new) so ?project= can be pre-selected. A daemon error
// degrades to just those.
func (s *Server) planProjectOptions(ctx context.Context, active string) []string {
	projects, err := s.listProjectBeadsCached(ctx)
	if err != nil {
		s.logger.Warn("plans: listing projects for new-plan form", "error", err)
	}
	seen := make(map[string]struct{}, len(projects)+2)
	names := make([]string, 0, len(projects)+2)
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		if _, dup := seen[name]; dup {
			return
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	for name := range projects {
		add(name)
	}
	sort.Strings(names)
	if _, ok := seen["gardenio"]; !ok {
		names = append([]string{"gardenio"}, names...)
		seen["gardenio"] = struct{}{}
	}
	// Fold the active ?project= scope in so the selector can always pre-select it.
	add(active)
	return names
}

// --- small helpers -----------------------------------------------------------

// isIssueType reports whether a bead type is a work item (issue kind): the
// TODO-step candidates. Excludes nested epics from the flat step list — a
// sub-plan renders as its own plan, not as a step.
func isIssueType(t string) bool {
	return plans.IsIssueType(t)
}

// planStepChildHydrate reports whether a plan child of childType is worth
// hydrating for the index step-data walk (ka-0000003113): issue steps, phase
// containers (epic/molecule) and decisions. An empty childType (older daemon or
// a dangling edge) hydrates anyway so a real step is never dropped.
func planStepChildHydrate(childType string) bool {
	if childType == "" {
		return true // unknown ⇒ never risk dropping a step
	}
	return isIssueType(childType) ||
		childType == "epic" || childType == "molecule" ||
		childType == "decision"
}

// isPhaseContainer reports whether a plan's direct child renders as a phase: an
// epic (sub-epic) or a molecule (a poured formula, ka-0000000759). Every
// phase-scan site must accept both (decision ka-0000003759: a poured container
// stays type=molecule to keep its molecule tooling). nil is never a container;
// this is not the check for the plan epic itself. Alias of
// beadsapi.IsPhaseContainer, shared with cmd/gb so the two cannot diverge
// (ka-0000004781).
func isPhaseContainer(c *beadsapi.BeadDetail) bool {
	return beadsapi.IsPhaseContainer(c)
}
