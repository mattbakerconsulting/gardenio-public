// plan_write.go — daemon-direct WRITE composition for `gb plan` (epic
// ka-0000005932, task ka-0000006355). This is the write-side twin of
// plan_read.go: it ports the beads-viewer's operator-gated plan WRITE handlers
// (POST /plans, /plans/{id}/steps, /plans/{id}/phases, …/reorder,
// /plans/{id}/complete, /plans/{id}/reopen) into package main so every `gb plan`
// write verb can originate straight against the beads daemon — no viewer URL, no
// X-Auth-Request-Email header, no 303-is-error edge dance.
//
// WHY (ka-0000006389, epic ka-0000005932): the viewer POST verbs 401 from an
// agent pod against an OIDC-gated deployment, because the viewer pod fronts every
// /plans write with its OWN Keycloak-OIDC middleware — a browser-session gate an
// agent's BEADS_AUTH_TOKEN can never satisfy. The daemon, by contrast, accepts
// that same token. A plan is a plain bead graph (an epic + parent-child children +
// a phase_order/step_order overlay in fields), so every write verb decomposes into
// the daemon primitives the *beadsapi.Client already carries — mint a bead, add a
// parent-child edge, grow/permute an ordered array, close/reopen the epic. Moving
// the write path here closes the OIDC-401 blocker for good.
//
// CORRECTNESS RAIL (ka-0000000988, MR !5901): the two ORDER-array mutations —
// growing phase_order (add-phase) and permuting it (reorder-phase) — do NOT use
// GetBead → splice → UpdateBeadFieldsTyped, which would silently drop a concurrent
// writer's edit under a blind read-modify-write. They use the daemon's race-safe
// primitives instead: AppendBeadArrayField (append-if-absent, one SQL UPDATE) for
// the grow, and ReorderBeadArrayField (compare-and-swap) for the permute, with
// reorderArrayFieldWithRetry re-reading + re-deriving on a 409 conflict. These are
// the SAME primitives + retry shape the viewer's plan_edit.go / plan_phases.go use,
// ported here so the CLI and browser are byte-for-byte equivalent dual writers.
//
// SCOPE. This file is the composition LAYER: pure, daemon-only helpers behind a
// narrow planDaemonWriter interface, unit-testable with a fake (mirroring
// plan_read.go's planDaemonReader). Repointing the cobra commands off the viewer
// onto these helpers, deleting the X-Auth-Request-Email / viewer-URL machinery,
// and re-homing the reactive add-step nudge are the sibling tasks (ka-0000004615,
// ka-0000007841, ka-0000000697) that hang off this one. Keeping the mint/link/grow
// logic here — one place, one shape — is why those follow-ups are thin.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/plangardener"
	"gardenio/controller/internal/shorttitle"
)

// planLabel is the /plans marker label. A plan is an epic carrying it; the viewer
// index is `plan`-gated (cmd/beads-viewer/plans.go). Mirrors the viewer's constant
// (package main there); redeclared here because cmd/gb cannot import cmd/beads-viewer.
const planLabel = "plan"

// planState mirrors the viewer's authoring-lifecycle enum
// (cmd/beads-viewer/plans_lifecycle.go). A plan created here is born
// plan_state=draft so the R6 approve→build gate applies from the first instant
// (EnabledOnBind reads the same field to born the gardener PAUSED); complete/reopen
// stamp the terminal / restored states. Redeclared as a local type (cmd/gb cannot
// import cmd/beads-viewer) but the string values are identical, so the field a
// browser and the CLI write is the same.
type planState string

const (
	planStateDraft     planState = "draft"
	planStateBuilding  planState = "building"
	planStateComplete  planState = "complete"
	planStateAbandoned planState = "abandoned"
)

// maxPlan*Len mirror the viewer's server-side input caps
// (cmd/beads-viewer/plans_create.go, plans.go, plan_edit.go) so a CLI write is
// rejected at exactly the same boundary the browser POST enforced. Redeclared
// locally (no cross-package import) with identical values.
const (
	maxPlanGoalLen       = 2000
	maxPlanTitleLen      = 120
	maxPlanStepTitleLen  = 500
	maxPlanPhaseTitleLen = maxPlanStepTitleLen
)

// maxCASReorderAttempts bounds the compare-and-swap retry loop reorderPlanPhase
// runs when a concurrent writer keeps invalidating the expected snapshot. Matches
// the viewer's maxCASReorderAttempts (cmd/beads-viewer/plan_edit.go): a handful of
// rounds is plenty for the rare CLI+browser contention window, and the loop still
// surfaces an exhausted-retry conflict as beadsapi.IsCASConflict so the caller can
// distinguish contention from a hard write error.
const maxCASReorderAttempts = 4

// planDaemonWriter is the narrow slice of *beadsapi.Client the plan WRITE path
// needs — the write-side twin of planDaemonReader (plan_read.go). Pulling it out
// as an interface lets each verb be unit-tested with an in-memory fake (no daemon)
// and documents the EXACT surface a decoupled plan write requires:
//
//   - GetBead / GetReverseDependencies / BatchGetBeads — resolve the plan epic +
//     its phase membership (the reorder path re-derives the rendered phase subset).
//   - CreateBead + AddDependency — mint a step / phase / plan epic and link it.
//   - AppendBeadArrayField / ReorderBeadArrayField — the race-safe phase_order
//     grow + permute (ka-0000000988), NOT a read-modify-write.
//   - UpdateBead / UpdateBeadFieldsTyped / CloseBeadAs — the epic status +
//     plan_state transitions behind complete / reopen.
//   - AddComment — best-effort audit trail for the lifecycle transitions.
//   - ListBeadsFiltered — resolve the bound plan-gardener to disable it on complete
//     (satisfies plangardener.Client, so plangardener.FindAnyGardenerForEpic drives it).
type planDaemonWriter interface {
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
	GetReverseDependencies(ctx context.Context, beadID string) ([]beadsapi.Dependency, error)
	BatchGetBeads(ctx context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error)
	CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error)
	AddDependency(ctx context.Context, beadID, dependsOnID, depType, createdBy string) error
	AppendBeadArrayField(ctx context.Context, beadID, field, element string) error
	ReorderBeadArrayField(ctx context.Context, beadID, field string, expected, next []string) error
	UpdateBead(ctx context.Context, beadID string, req beadsapi.UpdateBeadRequest) error
	UpdateBeadFieldsTyped(ctx context.Context, beadID string, fields map[string]any) error
	CloseBeadAs(ctx context.Context, beadID, actor string, fields map[string]string) error
	AddComment(ctx context.Context, beadID, author, text string) error
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
}

// *beadsapi.Client satisfies planDaemonWriter — asserted at compile time so a
// client signature drift surfaces here, not at the (untyped) call site. It also
// satisfies plangardener.Client (the ListBeadsFiltered + CreateBead subset), which
// completePlan relies on to resolve + disable the bound gardener.
var (
	_ planDaemonWriter    = (*beadsapi.Client)(nil)
	_ plangardener.Client = (*beadsapi.Client)(nil)
)

// planWriteResult is the outcome of a single plan write verb. Verbs that mint a
// bead (create / add-step / add-phase) return its ID; verbs that mutate the plan's
// lifecycle (complete / reopen) return the resulting (PlanState, Status); the
// reorder returns the Order that actually landed. Unset fields are simply empty —
// each verb's caller reads only what it produced. This is the CLI counterpart of
// the JSON bodies the viewer handlers wrote (…encode{"id":…} / {"plan_state":…}).
type planWriteResult struct {
	ID        string   `json:"id,omitempty"`
	Plan      string   `json:"plan,omitempty"`
	Parent    string   `json:"parent,omitempty"`
	Phase     string   `json:"phase,omitempty"`
	Title     string   `json:"title,omitempty"`
	PlanState string   `json:"plan_state,omitempty"`
	Status    string   `json:"status,omitempty"`
	Order     []string `json:"order,omitempty"`
}

// withPlanOperator stamps the canonical oidc:<email> operator on ctx so every
// daemon write the verb makes carries X-Operated-By. The kbeads audit resolver
// fail-closes with HTTP 400 on a write whose operator can't be resolved (R2,
// ka-0000005772) — the SAME guard the viewer honoured by stamping the ctx in each
// handler (beadsapi.WithOperator(canonicalOIDC(email))). email is a bare address
// (the caller validated it via operatorEmailForViewer); canonicalPlanOperator
// re-adds the oidc: prefix + lowercases, matching the viewer's canonicalOIDC.
func withPlanOperator(ctx context.Context, email string) context.Context {
	return beadsapi.WithOperator(ctx, canonicalPlanOperator(email))
}

// canonicalPlanOperator normalizes a bare or prefixed operator email to the
// canonical oidc:<lowercased-email> id the daemon stamps + the audit resolver
// keys on. Mirrors the viewer's canonicalOIDC (cmd/beads-viewer/gardener_ops.go):
// lowercase, add the oidc: prefix if absent, leave a slack: id untouched (though
// the plan verbs reject those upstream in operatorEmailForViewer). A blank input
// stays blank so a caller never stamps a bare "oidc:" prefix on nothing.
func canonicalPlanOperator(email string) string {
	email = strings.TrimSpace(email)
	if email == "" {
		return ""
	}
	low := strings.ToLower(email)
	if strings.HasPrefix(low, "slack:") {
		return email
	}
	if strings.HasPrefix(low, "oidc:") {
		return low
	}
	return "oidc:" + low
}

// planProjectLabel returns the "gardenio" from the first "project:gardenio" label,
// or "" when the slice carries none — the daemon-direct port of the viewer's
// projectFromLabels (used to stamp a born-on project on new step/phase children).
// A blank result means "don't stamp a project label", matching handlePlanAddStep /
// handlePlanCreatePhase, which only add the label when the parent carries one.
// Distinct from inheritProjectFromBead (gardener.go), which falls back to the gb
// default project — a new plan child must inherit the plan's EXACT project or
// nothing, never a mismatched default.
func planProjectLabel(labels []string) string {
	for _, l := range labels {
		if rest, ok := strings.CutPrefix(l, "project:"); ok {
			return rest
		}
	}
	return ""
}

// loadPlanEpic fetches a plan epic for a write verb and asserts it is a real
// epic. Mirrors the viewer's loadPlanEpicForMutation (minus the HTTP mapping —
// the CLI surfaces a plain error). A non-epic (or a missing bead) is a hard error
// so a write can never land on the wrong bead type.
func loadPlanEpic(ctx context.Context, w planDaemonWriter, planID string) (*beadsapi.BeadDetail, error) {
	epic, err := w.GetBead(ctx, planID)
	if err != nil {
		return nil, fmt.Errorf("looking up plan %s: %w", planID, err)
	}
	if epic == nil {
		return nil, fmt.Errorf("plan %s not found", planID)
	}
	if epic.Type != "epic" {
		return nil, fmt.Errorf("%s is a %s, not an epic — gb plan writes to a plan epic (mark one with 'kd label add <id> plan')", planID, epic.Type)
	}
	return epic, nil
}

// ── create ───────────────────────────────────────────────────────────────────

// createPlanEpicDaemon mints a new plan epic from a one-line goal and returns the
// VERIFIED epic id — the daemon-direct port of the viewer's createPlanEpic
// (cmd/beads-viewer/plans_create.go). It is the invisible-orphan-safe create (R2):
// the `plan` + `project:<project>` labels AND plan_state=draft are born-on in ONE
// CreateBead transaction (never a post-hoc AddLabel that could half-fail into an
// epic invisible to the `plan`-gated /plans index), an idempotency key collapses a
// double-submit to exactly one epic, and it reads the epic back and asserts the
// shape BEFORE returning so a caller never prints a bogus success for a mis-shaped
// epic.
//
// It does NOT spawn the author assistant or bind the gardener — those chainings
// live in the viewer's handleCreatePlan and are the reactive-collaboration seam a
// sibling task re-homes (ka-0000007841); this helper is the mint+verify spine.
//
// The caller must have validated the operator (operatorEmailForViewer) and pass
// its bare email; withPlanOperator stamps ctx so the CreateBead carries the
// operator the audit resolver requires.
func createPlanEpicDaemon(ctx context.Context, w planDaemonWriter, goal, project, email string) (*beadsapi.BeadDetail, error) {
	goal = strings.TrimSpace(goal)
	project = strings.TrimSpace(project)
	if goal == "" {
		return nil, fmt.Errorf("a plan goal is required")
	}
	if project == "" {
		return nil, fmt.Errorf("a project is required")
	}
	if len(goal) > maxPlanGoalLen {
		return nil, fmt.Errorf("plan goal is too long (max %d bytes)", maxPlanGoalLen)
	}

	ctx = withPlanOperator(ctx, email)

	// Born-on labels + plan_state=draft ride the SAME CreateBead transaction, so
	// there is never a moment where the epic exists but is invisible to /plans
	// (R2) or momentarily ungated (R6). plan_state is a declared enum field on
	// type:epic in kbeads config.go, so the born-on write is accepted, not 400'd.
	fields, err := json.Marshal(map[string]string{planStateFieldKey: string(planStateDraft)})
	if err != nil {
		return nil, fmt.Errorf("marshaling plan_state for new plan epic: %w", err)
	}
	req := beadsapi.CreateBeadRequest{
		Title:          planTitleFromGoal(goal),
		ShortTitle:     shorttitle.Clip("plan: "+goal, shorttitle.Max),
		Type:           "epic",
		Kind:           "issue",
		Description:    goal,
		CreatedBy:      email,
		Labels:         []string{planLabel, "project:" + project},
		Fields:         fields,
		IdempotencyKey: planCreateIdempotencyKey(goal, project, email),
	}
	epicID, err := w.CreateBead(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("creating plan epic: %w", err)
	}

	// Verify BEFORE returning (design §4.2 step 3): read the epic back and assert
	// it is a discoverable, gated plan. Born-on labels make a failure here a
	// daemon-level anomaly rather than a race, but a silent mislabel would still be
	// an invisible orphan — surface it loudly, naming the epic id so nothing is lost.
	epic, err := w.GetBead(ctx, epicID)
	if err != nil {
		return nil, fmt.Errorf("verifying plan epic %s: %w", epicID, err)
	}
	if epic == nil || epic.Type != "epic" {
		return nil, fmt.Errorf("plan epic %s did not read back as an epic (invisible-orphan guard, R2)", epicID)
	}
	if !slices.Contains(epic.Labels, planLabel) {
		return nil, fmt.Errorf("plan epic %s is missing the %q label (invisible-orphan guard, R2)", epicID, planLabel)
	}
	if got := planProjectLabel(epic.Labels); got != project {
		return nil, fmt.Errorf("plan epic %s project label = %q, want %q (invisible-orphan guard, R2)", epicID, got, project)
	}
	if got := planState(strings.TrimSpace(epic.Fields[planStateFieldKey])); got != planStateDraft {
		return nil, fmt.Errorf("plan epic %s plan_state = %q, want %q (authoring-lifecycle gate, R6)", epicID, got, planStateDraft)
	}
	return epic, nil
}

// planTitleFromGoal derives a single-line plan title from the (possibly
// multi-line) goal by collapsing whitespace runs and truncating to maxPlanTitleLen.
// The goal itself is preserved verbatim as the epic Description. Mirrors the
// viewer's planTitleFromGoal.
func planTitleFromGoal(goal string) string {
	return truncate(strings.Join(strings.Fields(goal), " "), maxPlanTitleLen)
}

// planCreateIdempotencyKey derives the create dedup key — a project-scoped
// lookup-or-return on (goal, project, actor) so a double-submit collapses to
// exactly ONE plan epic (design §4.2, R3). Mirrors the viewer's
// planCreateIdempotencyKey byte-for-byte (same sha256(goal\x00project\x00actor)
// prefix), so a `gb plan create` and a browser create of the same goal into the
// same project by the same operator converge on the same epic.
func planCreateIdempotencyKey(goal, project, actor string) string {
	sum := sha256.Sum256([]byte(goal + "\x00" + project + "\x00" + actor))
	return "plan-create:" + hex.EncodeToString(sum[:16])
}

// ── add-step ───────────────────────────────────────────────────────────────

// addPlanStep creates a new TODO step (an issue child) under the plan epic — or
// under one of its phases when phaseID is non-empty — and links it parent-child.
// Daemon-direct port of the viewer's handlePlanAddStep (cmd/beads-viewer/plans.go)
// minus the reactive-collaboration nudge, which is the interim seam a sibling task
// re-homes (ka-0000007841); this helper is the create+link spine the nudge hangs off.
//
// A blank phaseID keeps the pre-phase behaviour: a flat direct child of the plan.
// A non-blank phaseID MUST be a current phase of THIS plan (a direct-child epic
// listed in phase_order) — verified by resolvePlanStepPhase, which fail-closes so
// a stray id can never adopt the step under an unrelated epic (membership rule,
// ka-0000001034). The step inherits its parent's project label born-on.
func addPlanStep(ctx context.Context, w planDaemonWriter, planID, title, phaseID, email string) (planWriteResult, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return planWriteResult{}, fmt.Errorf("step title is required")
	}
	if len(title) > maxPlanStepTitleLen {
		return planWriteResult{}, fmt.Errorf("step title is too long (max %d bytes)", maxPlanStepTitleLen)
	}

	ctx = withPlanOperator(ctx, email)

	epic, err := loadPlanEpic(ctx, w, planID)
	if err != nil {
		return planWriteResult{}, err
	}

	// Resolve the parent: the plan epic for a flat step, or the verified phase
	// sub-epic for a phased step. parentLabels sources the born-on project label.
	parentID := epic.ID
	parentLabels := epic.Labels
	if phaseID = strings.TrimSpace(phaseID); phaseID != "" {
		phaseEpic, perr := resolvePlanStepPhase(ctx, w, epic, phaseID)
		if perr != nil {
			return planWriteResult{}, perr
		}
		parentID = phaseEpic.ID
		parentLabels = phaseEpic.Labels
	}

	req := beadsapi.CreateBeadRequest{
		Title:     title,
		Type:      "task",
		CreatedBy: email,
	}
	if project := planProjectLabel(parentLabels); project != "" {
		req.Labels = []string{"project:" + project}
	}
	stepID, err := w.CreateBead(ctx, req)
	if err != nil {
		return planWriteResult{}, fmt.Errorf("creating step under plan %s: %w", planID, err)
	}
	// The parent-child edge is what makes the new bead a step of THIS plan (or of
	// one of its phases). Surface a link failure rather than silently orphaning.
	if err := w.AddDependency(ctx, stepID, parentID, "parent-child", email); err != nil {
		return planWriteResult{}, fmt.Errorf("step %s created but could not be linked to %s: %w", stepID, parentID, err)
	}
	// phaseID is "" for a flat step (Phase omitempty-drops it) and the phase id for
	// a phased one — it was trimmed above, so no extra plumbing is needed.
	return planWriteResult{ID: stepID, Plan: planID, Parent: parentID, Phase: phaseID, Title: title}, nil
}

// resolvePlanStepPhase verifies phaseID is a current phase of the plan `epic` — a
// direct-child type=epic whose id is listed in phase_order — and returns the phase
// sub-epic. Daemon-direct port of the viewer's resolvePlanPhaseEpic: it fail-closes
// so an add-step phase target can never adopt a step under an unrelated bead.
func resolvePlanStepPhase(ctx context.Context, w planDaemonWriter, epic *beadsapi.BeadDetail, phaseID string) (*beadsapi.BeadDetail, error) {
	if !slices.Contains(planPhaseOrder(epic), phaseID) {
		return nil, fmt.Errorf("phase %s is not a phase of this plan (not in phase_order)", phaseID)
	}
	phaseEpic, err := w.GetBead(ctx, phaseID)
	if err != nil || phaseEpic == nil || phaseEpic.Type != "epic" {
		return nil, fmt.Errorf("phase %s is not a valid phase epic", phaseID)
	}
	return phaseEpic, nil
}

// ── add-phase ──────────────────────────────────────────────────────────────

// addPlanPhase mints a new type=epic child of the plan, links it parent-child,
// and APPENDS its id to the plan's phase_order so it renders as an inline (empty)
// phase. Daemon-direct port of the viewer's handlePlanCreatePhase
// (cmd/beads-viewer/plan_edit.go).
//
// The phase_order grow uses the daemon's atomic AppendBeadArrayField
// (append-if-absent, one SQL UPDATE, ka-0000000988) — NOT GetBead → splice →
// UpdateBeadFieldsTyped — so a concurrent phase add/reorder can't clobber it and a
// double-submit is idempotent. The phase epic inherits the plan's project label
// born-on.
func addPlanPhase(ctx context.Context, w planDaemonWriter, planID, title, email string) (planWriteResult, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return planWriteResult{}, fmt.Errorf("phase title is required")
	}
	if len(title) > maxPlanPhaseTitleLen {
		return planWriteResult{}, fmt.Errorf("phase title is too long (max %d bytes)", maxPlanPhaseTitleLen)
	}

	ctx = withPlanOperator(ctx, email)

	epic, err := loadPlanEpic(ctx, w, planID)
	if err != nil {
		return planWriteResult{}, err
	}

	// ParentBeadID has the daemon wire the parent-child edge in the create
	// transaction, so the create is born a direct child of the plan — and
	// create-time gates (BEADS_HERALDRY_GATE on a plan's phase epic) see it as a
	// phase, which a follow-up AddDependency hid from them (gd-kcCrpdrlm2). The
	// edge makes the epic a direct child of the plan; presence in phase_order is
	// what makes it RENDER as an inline phase (membership rule, ka-0000001034).
	create := beadsapi.CreateBeadRequest{
		Title:        title,
		Type:         "epic",
		CreatedBy:    email,
		ParentBeadID: planID,
	}
	if project := planProjectLabel(epic.Labels); project != "" {
		create.Labels = []string{"project:" + project}
	}
	phaseID, err := w.CreateBead(ctx, create)
	if err != nil {
		return planWriteResult{}, fmt.Errorf("creating phase under plan %s: %w", planID, err)
	}
	// GROW phase_order atomically (append-if-absent) — the race-safe primitive,
	// not a read-modify-write. A failure here leaves a linked-but-unlisted epic
	// (renders as a freestanding sub-plan, not an inline phase), so surface it.
	if err := w.AppendBeadArrayField(ctx, planID, planPhaseFieldKey, phaseID); err != nil {
		return planWriteResult{}, fmt.Errorf("phase %s created but could not be added to plan %s phase order: %w", phaseID, planID, err)
	}
	return planWriteResult{ID: phaseID, Plan: planID, Title: title}, nil
}

// ── reorder-phase ────────────────────────────────────────────────────────────

// reorderPlanPhase moves one phase sub-epic up or down within the plan's
// phase_order and returns the order that landed. Daemon-direct port of the
// viewer's handlePlanPhaseReorder (cmd/beads-viewer/plan_phases.go).
//
// A reorder is a blind full-array rewrite, so it goes through the daemon's
// compare-and-swap primitive (ReorderBeadArrayField, ka-0000000988) via
// reorderArrayFieldWithRetry rather than GetBead → UpdateBeadFieldsTyped: expected
// is the CURRENTLY stored phase_order, next is the reordered array. On a 409 the
// retry re-reads + re-derives so a concurrent add-phase isn't dropped.
//
// The swap runs over the RESOLVED (rendered) phase subset — phase_order ids that
// currently resolve to a direct-child epic — not the raw array, so a stale id
// interleaved between two rendered phases can't absorb a ▲/▼ move and make it a
// visible no-op (ka-0000002075). ok=false (no write) means the phase isn't a
// rendered phase or the move fell off the boundary — a harmless no-op the caller
// reports as such.
func reorderPlanPhase(ctx context.Context, w planDaemonWriter, planID, phaseID, dir, email string) (planWriteResult, error) {
	if dir != "up" && dir != "down" {
		return planWriteResult{}, fmt.Errorf("dir must be up or down")
	}
	ctx = withPlanOperator(ctx, email)

	epic, err := loadPlanEpic(ctx, w, planID)
	if err != nil {
		return planWriteResult{}, err
	}

	// finalNext captures the order that actually lands so the caller can echo it —
	// the derive closure re-runs on every CAS retry, so we snapshot the winning next.
	var finalNext []string
	written, err := reorderArrayFieldWithRetry(ctx, w, planID, planPhaseFieldKey, epic,
		func(fresh *beadsapi.BeadDetail) (expected, next []string, ok bool) {
			order := planPhaseOrder(fresh)
			// Resolve phase_order against the plan's CURRENT direct children so the
			// swap runs over the same RENDERED subset the browser numbers (▲▼ /
			// IsFirst / IsLast). On a fetch failure fall back to the raw order — no
			// worse than the pre-ka-0000002075 raw-neighbor swap, and the common
			// no-stale-id case is unaffected.
			resolved := order
			if revDeps, derr := w.GetReverseDependencies(ctx, planID); derr == nil {
				if children, berr := w.BatchGetBeads(ctx, planParentChildIDs(revDeps)); berr == nil {
					resolved = resolvePlanPhaseIDs(order, children)
				}
			}
			next, ok = reorderPhaseResolved(order, resolved, phaseID, dir)
			finalNext = next
			return order, next, ok // expected is the stored raw phase_order the CAS compares against
		})
	if err != nil {
		return planWriteResult{}, fmt.Errorf("reordering phase %s in plan %s: %w", phaseID, planID, err)
	}
	// On a landed write finalNext is the order the CAS wrote; on a boundary/absent
	// no-op (written=false) it is nil, so Status="noop" + an empty Order tell the
	// caller a double-click at the edge was harmless — no order changed.
	return planWriteResult{Plan: planID, Phase: phaseID, Order: finalNext, Status: reorderStatus(written)}, nil
}

// reorderStatus maps whether a reorder landed a write to the result Status the
// caller prints ("ok" for a landed permute, "noop" for a boundary/absent move).
func reorderStatus(written bool) string {
	if written {
		return "ok"
	}
	return "noop"
}

// reorderArrayFieldWithRetry compare-and-swaps the string-array field `field` on
// beadID, using derive to (re)compute (expected, next) from a freshly-read bead.
// `initial` is the already-loaded bead used for the first attempt (so the common
// no-conflict path costs no extra read); on a CAS conflict the bead is re-read and
// derive re-runs against the fresh array, so a concurrent append is picked up
// rather than clobbered. derive returns ok=false for a boundary/absent no-op.
// Returns (written, err): written=false + err=nil is the no-op; written=true is a
// landed write; a non-nil err is a hard failure (an exhausted-retry err still
// reports beadsapi.IsCASConflict, so the caller can distinguish contention). This
// is the daemon-direct port of the viewer's reorderArrayFieldWithRetry
// (cmd/beads-viewer/plan_edit.go).
func reorderArrayFieldWithRetry(ctx context.Context, w planDaemonWriter, beadID, field string, initial *beadsapi.BeadDetail, derive func(fresh *beadsapi.BeadDetail) (expected, next []string, ok bool)) (bool, error) {
	fresh := initial
	var lastErr error
	for attempt := 0; attempt < maxCASReorderAttempts; attempt++ {
		if fresh == nil {
			reread, ferr := w.GetBead(ctx, beadID)
			if ferr != nil {
				return false, fmt.Errorf("reorder %s: re-read %s: %w", field, beadID, ferr)
			}
			if reread == nil {
				return false, fmt.Errorf("reorder %s: bead %s not found on re-read", field, beadID)
			}
			fresh = reread
		}
		expected, next, ok := derive(fresh)
		if !ok {
			return false, nil // boundary/absent — a harmless no-op
		}
		err := w.ReorderBeadArrayField(ctx, beadID, field, expected, next)
		if err == nil {
			return true, nil
		}
		if !beadsapi.IsCASConflict(err) {
			return false, err
		}
		lastErr = err // conflict: re-read + re-derive against the fresh array
		fresh = nil
	}
	return false, lastErr
}

// reorderPhaseResolved moves phaseID one position within the RESOLVED phase subset
// (the ids that actually render as phases, in display order), then splices the
// result back into the RAW phase_order array, leaving every stale/non-rendering id
// pinned at its original index. Daemon-direct port of the viewer's
// reorderPhaseResolved (cmd/beads-viewer/plan_phases.go) — the ka-0000002075 fix:
// the swap must run over the same subset the browser numbers, or a stale id
// interleaved between two rendered phases silently absorbs a ▲/▼ click.
//
// Returns (nil, false) when phaseID is not a rendered phase or the move falls off
// the resolved boundary — a harmless no-op. `order` is never mutated; `resolved`
// must be an order-preserving sub-sequence of `order` (resolvePlanPhaseIDs
// guarantees this). When resolved == order this reduces to a raw-neighbor swap.
func reorderPhaseResolved(order, resolved []string, phaseID, dir string) ([]string, bool) {
	ridx := slices.Index(resolved, phaseID)
	if ridx < 0 {
		return nil, false
	}
	rswap := ridx - 1
	if dir == "down" {
		rswap = ridx + 1
	}
	if rswap < 0 || rswap >= len(resolved) {
		return nil, false
	}
	swapID := resolved[rswap]

	// Map the two resolved ids back to their FIRST positions in the raw array and
	// swap them there, preserving every other slot (including interleaved stale
	// ids). First occurrence wins, matching resolvePlanPhaseIDs's dedup.
	iA, iB := -1, -1
	for i, id := range order {
		if id == phaseID && iA < 0 {
			iA = i
		}
		if id == swapID && iB < 0 {
			iB = i
		}
	}
	if iA < 0 || iB < 0 {
		return nil, false // shouldn't happen (resolved ⊆ order); degrade to no-op
	}
	next := make([]string, len(order))
	copy(next, order)
	next[iA], next[iB] = next[iB], next[iA]
	return next, true
}

// ── complete / reopen ──────────────────────────────────────────────────────

// completePlan marks a plan complete: it DISABLES the bound plan-gardener (so its
// loop stops firing), stamps plan_state=complete, and CLOSES the plan epic.
// Daemon-direct port of the viewer's handlePlanComplete (cmd/beads-viewer/
// plans_lifecycle.go), preserving its ordering + best-effort semantics:
//
//   - Disable the gardener FIRST, then stamp complete, then close. Disabling first
//     means even if a later write fails the loop is already quiet — the safe
//     failure direction. A gardener disable failure is logged-shape (returned as a
//     best-effort miss, not a hard error) since the gardener's own cron pre-flight
//     goes dormant on a closed epic anyway.
//   - Idempotent: a plan already complete AND closed re-reports without a second
//     close/disable.
//   - No block on open children — an operator can always retire a plan.
func completePlan(ctx context.Context, w planDaemonWriter, planID, email string) (planWriteResult, error) {
	ctx = withPlanOperator(ctx, email)

	epic, err := loadPlanEpic(ctx, w, planID)
	if err != nil {
		return planWriteResult{}, err
	}
	// Already terminal — idempotent re-report, no second close/disable.
	if planState(strings.TrimSpace(epic.Fields[planStateFieldKey])) == planStateComplete && epic.Status == "closed" {
		return planWriteResult{ID: planID, PlanState: string(planStateComplete), Status: "closed"}, nil
	}

	// Disable the bound gardener first (best-effort — a plan may have none). The
	// loop's cron pre-flight also goes dormant on a closed epic, so a disable miss
	// is not fatal: closing the epic below is what makes the plan terminal.
	disablePlanGardener(ctx, w, planID, email)

	// Stamp the terminal plan_state (best-effort audit comment inside). This
	// precedes the close so a Done-lane reader sees plan_state=complete.
	if err := setPlanStateDaemon(ctx, w, planID, planStateComplete, email); err != nil {
		return planWriteResult{}, fmt.Errorf("marking plan %s complete: %w", planID, err)
	}
	// Close the plan epic — the observable "done" the board's Done lane keys on.
	// Attribute the close to the operator so the audit trail reads correctly.
	if err := w.CloseBeadAs(ctx, planID, canonicalPlanOperator(email), nil); err != nil {
		return planWriteResult{}, fmt.Errorf("marked plan %s complete but closing it failed: %w", planID, err)
	}
	return planWriteResult{ID: planID, PlanState: string(planStateComplete), Status: "closed"}, nil
}

// abandonPlan marks a plan ABANDONED — the DISTINCT terminal "cheap exit" (task
// ka-0000001377, epic ka-0000007470): a plan is dropped WITHOUT finishing its
// work. Structurally identical to completePlan (disable the bound gardener, stamp
// the terminal plan_state, close the epic) but stamps plan_state=abandoned so the
// board and cockpit can render "gave up on this" distinctly from a proud
// "complete". The daemon accepts building→abandoned, ready→abandoned and
// draft→abandoned, so a plan can be abandoned from any non-terminal working state
// without first walking it forward to building.
//
//   - Disable the gardener FIRST, then stamp abandoned, then close (same safe
//     failure ordering as completePlan — even a later failure leaves the loop
//     quiet).
//   - Idempotent: a plan already abandoned AND closed re-reports without a second
//     close/disable.
//   - No block on open children — abandoning is precisely the "give up on the
//     leftover work" exit, so leftover open steps are expected.
func abandonPlan(ctx context.Context, w planDaemonWriter, planID, email string) (planWriteResult, error) {
	ctx = withPlanOperator(ctx, email)

	epic, err := loadPlanEpic(ctx, w, planID)
	if err != nil {
		return planWriteResult{}, err
	}
	// Already terminal (abandoned + closed) — idempotent re-report.
	if planState(strings.TrimSpace(epic.Fields[planStateFieldKey])) == planStateAbandoned && epic.Status == "closed" {
		return planWriteResult{ID: planID, PlanState: string(planStateAbandoned), Status: "closed"}, nil
	}

	// Disable the bound gardener first (best-effort — a plan may have none).
	disablePlanGardener(ctx, w, planID, email)

	// Stamp the terminal plan_state (best-effort audit comment inside). This
	// precedes the close so a board reader sees plan_state=abandoned.
	if err := setPlanStateDaemon(ctx, w, planID, planStateAbandoned, email); err != nil {
		return planWriteResult{}, fmt.Errorf("marking plan %s abandoned: %w", planID, err)
	}
	// Close the plan epic — the observable "gone" the board keys on. Attribute
	// the close to the operator so the audit trail reads correctly.
	if err := w.CloseBeadAs(ctx, planID, canonicalPlanOperator(email), nil); err != nil {
		return planWriteResult{}, fmt.Errorf("marked plan %s abandoned but closing it failed: %w", planID, err)
	}
	return planWriteResult{ID: planID, PlanState: string(planStateAbandoned), Status: "closed"}, nil
}

// reopenPlan is the inverse of completePlan / abandonPlan: it REOPENS a terminal
// plan (status=open, plan_state=building) so work can resume. Daemon-direct port
// of the viewer's handlePlanReopen (cmd/beads-viewer/plans_lifecycle.go).
//
// The bound gardener is left AS-IS (paused) — reopening restores the buildable
// state without silently restarting the loop; the operator resumes it deliberately
// (R6: enabling never happens behind the operator's back). building (not ready) is
// the target because a terminal plan was, by definition, past the approve→build
// gate (both complete and abandoned reopen to building — the single back-edge). An
// already-open, non-terminal plan re-reports as-is (a stale double-click).
func reopenPlan(ctx context.Context, w planDaemonWriter, planID, email string) (planWriteResult, error) {
	ctx = withPlanOperator(ctx, email)

	epic, err := loadPlanEpic(ctx, w, planID)
	if err != nil {
		return planWriteResult{}, err
	}
	// Only a closed/terminal plan can be reopened. An open, non-terminal plan
	// re-reports as-is rather than erroring (a stale click after another writer
	// reopened it).
	if st := planState(strings.TrimSpace(epic.Fields[planStateFieldKey])); epic.Status != "closed" && st != planStateComplete && st != planStateAbandoned {
		return planWriteResult{ID: planID, PlanState: epic.Fields[planStateFieldKey], Status: epic.Status}, nil
	}
	// Reopen the epic (status=open) so the board moves it out of Done and the
	// gardener's cron pre-flight can fire again.
	open := "open"
	if err := w.UpdateBead(ctx, planID, beadsapi.UpdateBeadRequest{Status: &open}); err != nil {
		return planWriteResult{}, fmt.Errorf("reopening plan %s: %w", planID, err)
	}
	// Restore the buildable lifecycle state. The gardener stays paused — the
	// operator resumes it deliberately (R6): reopening must not restart the loop.
	if err := setPlanStateDaemon(ctx, w, planID, planStateBuilding, email); err != nil {
		return planWriteResult{}, fmt.Errorf("plan %s reopened but restoring its build state failed: %w", planID, err)
	}
	return planWriteResult{ID: planID, PlanState: string(planStateBuilding), Status: "open"}, nil
}

// setPlanStateDaemon writes the plan epic's plan_state field and records a
// best-effort audit comment — the daemon-direct port of the viewer's setPlanState
// (cmd/beads-viewer/plans_lifecycle.go). The typed write sends the enum value as a
// plain JSON string the field accepts. A hard write failure is returned; the audit
// comment failing is swallowed (it is not load-bearing).
func setPlanStateDaemon(ctx context.Context, w planDaemonWriter, planID string, next planState, email string) error {
	if err := w.UpdateBeadFieldsTyped(ctx, planID, map[string]any{planStateFieldKey: string(next)}); err != nil {
		return err
	}
	// Best-effort audit — mirrors the viewer, which Warn-logs a comment failure.
	_ = w.AddComment(ctx, planID, email, "plan_state → "+string(next)+" by "+email)
	return nil
}

// disablePlanGardener resolves the plan's bound gardener (if any) and flips its
// `enabled` field to false — the daemon-direct port of the viewer's
// setGardenerEnabledCore(disable) as driven by handlePlanComplete. Best-effort by
// design: a plan may have no gardener, and the gardener's own cron pre-flight goes
// dormant on a closed epic, so a resolve/disable miss is swallowed (mirroring the
// viewer's Warn-only treatment) — completePlan's close is what makes the plan
// terminal. `enabled` is a declared boolean field on the trigger bead, so the
// typed false write is accepted.
func disablePlanGardener(ctx context.Context, w planDaemonWriter, planID, email string) {
	gardener, err := plangardener.FindAnyGardenerForEpic(ctx, w, planID)
	if err != nil || gardener == nil {
		return
	}
	if err := w.UpdateBeadFieldsTyped(ctx, gardener.ID, map[string]any{"enabled": false}); err != nil {
		return
	}
	_ = w.AddComment(ctx, gardener.ID, email, "enabled → false by "+email)
}
