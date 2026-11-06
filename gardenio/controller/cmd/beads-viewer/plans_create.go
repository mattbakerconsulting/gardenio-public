package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// plans_create.go — task 3a of the from-scratch plan authoring flow
// (plan-of-record ka-0000005794, Phase 3 epic ka-0000005096, task ka-0000006081;
// design of record ka-0000005773 §4). It adds the ONE thing the /plans cockpit
// could never do: originate a plan from the browser. Every other /plans route
// operates on a plan that already exists; there was no bare POST /plans, no
// handler that mints a type=epic (design §1, §9-A). This file is that create
// verb.
//
// The load-bearing contract is TRANSACTIONAL LABELS (design §4.2, risk R2). A
// plan is an epic carrying the `plan` label; the index query is `plan`-gated
// (handlePlans, plans.go:269). If an epic were created and only THEN labelled
// with a best-effort follow-up AddLabel (the handlePlanMark path, plans.go:2014,
// and spawnPlanAssistant's Warn-only label adds), any failure in that second
// write would leave an epic that exists but is invisible to /plans forever — an
// orphan the operator can neither see nor recover from the cockpit. createPlanEpic
// closes that window by making BOTH labels born-on: they ride the CreateBead
// transaction via CreateBeadRequest.Labels (applied server-side at create time,
// exactly as handlePlanAddStep stamps project: at plans.go:1886-1888), then it
// reads the epic back and asserts the shape BEFORE any caller redirects.
//
// Write-through (D5, ka-0000003301): the epic is persisted + discoverable the
// instant this returns — there is no draft buffer. The only later gate in the
// authoring flow is the gardener build (task 3f, ka-0000005350).

const (
	// maxPlanGoalLen caps the one-line goal that seeds a new plan. Wide enough
	// for a rich one-paragraph intent, bounded so a paste accident round-trips
	// via kd instead of minting a runaway epic. Enforced server-side in
	// createPlanEpic so both callers (the browser POST and gb plan create, which
	// reaches this same helper over HTTP) share one guard.
	maxPlanGoalLen = 2000
	// maxPlanTitleLen caps the title derived from the goal. The assistant
	// proposes a crisper title during research (3c); this is only the seed.
	maxPlanTitleLen = 120
)

// Input sentinels so handleCreatePlan can map a bad request to the right HTTP
// status without string-matching the error text.
var (
	errPlanGoalRequired    = errors.New("a plan goal is required")
	errPlanProjectRequired = errors.New("a project is required")
	errPlanGoalTooLong     = fmt.Errorf("plan goal is too long (max %d bytes)", maxPlanGoalLen)
)

// planTitleFromGoal derives a single-line plan title from the (possibly
// multi-line) goal by collapsing runs of whitespace and truncating. The goal
// itself is preserved verbatim as the epic Description (the assistant's seed).
func planTitleFromGoal(goal string) string {
	title := strings.Join(strings.Fields(goal), " ")
	return truncate(title, maxPlanTitleLen)
}

// planCreateIdempotencyKey derives the dedup key for a create (design §4.2, R3).
// The daemon does a project-scoped lookup-or-return on this key and a per-fleet
// partial unique index backstops the concurrent-insert race (kbeads migration
// 029), so a double-submit — a fat-fingered second click or a retried POST —
// collapses to exactly ONE plan epic. Keyed on (goal, project, actor): the same
// human re-submitting the same goal into the same project reuses the epic; two
// DIFFERENT operators with an identical goal each get their own plan (their
// actor differs), which is the intended behaviour.
func planCreateIdempotencyKey(goal, project, actor string) string {
	sum := sha256.Sum256([]byte(goal + "\x00" + project + "\x00" + actor))
	return "plan-create:" + hex.EncodeToString(sum[:16])
}

// createPlanEpic mints a new plan epic from a one-line goal and returns the
// VERIFIED epic. It is the shared create path (D2 = both, ka-0000002854): the
// HTTP handler handleCreatePlan calls it directly, and gb plan create reaches it
// over HTTP by POSTing to POST /plans — one create path, two callers.
//
// The epic is born labelled `plan` + `project:<project>` in ONE atomic write
// (CreateBeadRequest.Labels), so there is never a moment where the epic exists
// but is invisible to the `plan`-gated /plans index (R2). The daemon now folds
// the label read-back INTO the create transaction (ka-0000002898): the bead the
// CreateBeadDetail response carries reflects exactly the persisted labels, so
// this helper asserts type + both labels + plan_state against that authoritative
// return value directly — no second GetBead round-trip. A caller therefore never
// redirects an operator to a blind cockpit for a mis-shaped epic, and the
// all-or-nothing guarantee is enforced at the daemon rather than re-checked here.
//
// It does NOT spawn the author assistant itself — that chaining (task 3c,
// ka-0000004399) lives one level up in handleCreatePlan, which hands this verified
// return value to spawnPlanAuthorAssistant → spawnPlanAssistant(ctx, actor, epic,
// goal). Keeping the mint+verify helper spawn-free is deliberate: gb plan create
// reaches createPlanEpic over HTTP through the same handler, so the spawn belongs
// on the shared handler path, not duplicated in the helper. The returned
// *BeadDetail is exactly what spawnPlanAssistant requires (it derefs epic.ID and
// reads projectFromLabels(epic.Labels)).
//
// The caller MUST stamp the operator on ctx (beadsapi.WithOperator) before
// calling: the kbeads audit resolver fail-closes with HTTP 400 on a create whose
// operator can't be resolved (R2, ka-0000005772), the same guard handlePlanAddStep
// honours at plans.go:1863.
func (s *Server) createPlanEpic(ctx context.Context, goal, project, actor string) (*beadsapi.BeadDetail, error) {
	goal = strings.TrimSpace(goal)
	project = strings.TrimSpace(project)
	if goal == "" {
		return nil, errPlanGoalRequired
	}
	if project == "" {
		return nil, errPlanProjectRequired
	}
	if len(goal) > maxPlanGoalLen {
		return nil, errPlanGoalTooLong
	}

	// Born-on labels: BOTH the plan marker and the project scope are part of the
	// create transaction, so the orphan window R2 describes cannot open. Contrast
	// handlePlanMark's post-hoc AddLabel (plans.go:2014), which this create path
	// deliberately does NOT use.
	//
	// Born-on plan_state=draft: a browser-originated plan MUST enter the authoring
	// lifecycle at mint time so the R6 build-gate applies from the first instant
	// (task 3f, ka-0000005350; design §6.4; plans_lifecycle.go file header). Without
	// this, plan_state reads back unset, planStateOf → planStateUnset, and
	// gardenerEnableGatedByState treats the plan as UNGATED — a bound gardener could
	// be enabled to auto-build a plan the assistant is still drafting, and the
	// draft→ready→building levers never render (plans_gardener.go gates them on a set
	// plan_state). It rides the SAME CreateBead transaction as the labels (plan_state
	// is a declared enum field on type:epic in kbeads config.go, so the born-on write
	// is accepted, not 400'd) rather than a post-hoc UpdateBeadFieldsTyped, so a
	// browser-created plan is never momentarily ungated (mirrors the born-on-labels
	// R2 reasoning). This is the write the file header's "stamps plan_state=draft at
	// mint time" contract refers to.
	fields, err := json.Marshal(map[string]string{planStateFieldKey: string(planStateDraft)})
	if err != nil {
		return nil, fmt.Errorf("marshaling plan_state for new plan epic: %w", err)
	}
	req := beadsapi.CreateBeadRequest{
		Title:          planTitleFromGoal(goal),
		Type:           "epic",
		Kind:           "issue",
		Description:    goal,
		CreatedBy:      actor,
		Labels:         []string{planLabel, "project:" + project},
		Fields:         fields,
		IdempotencyKey: planCreateIdempotencyKey(goal, project, actor),
	}
	// CreateBeadDetail returns the epic exactly as it was committed — the daemon
	// reads the labels back inside the create transaction (ka-0000002898), so the
	// response IS the verify-readback. No separate GetBead round-trip, and no
	// window in which the returned bead echoes the input labels instead of the
	// persisted set.
	epic, err := s.daemon.CreateBeadDetail(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("creating plan epic: %w", err)
	}

	// Verify BEFORE any redirect (design §4.2 step 3): assert the committed epic
	// is a discoverable plan. Born-on labels make a failure here a daemon-level
	// anomaly rather than an expected race, but a silent mislabel would still be
	// an invisible orphan — so we surface it loudly and name the epic id so the
	// operator can recover it, instead of 303-ing to a cockpit that would render
	// nothing. These assertions now run against the create response itself.
	if epic == nil || epic.Type != "epic" {
		return nil, fmt.Errorf("plan epic did not read back as an epic (invisible-orphan guard, R2)")
	}
	if !hasLabel(epic, planLabel) {
		return nil, fmt.Errorf("plan epic %s is missing the %q label (invisible-orphan guard, R2)", epic.ID, planLabel)
	}
	if got := projectFromLabels(epic.Labels); got != project {
		return nil, fmt.Errorf("plan epic %s project label = %q, want %q (invisible-orphan guard, R2)", epic.ID, got, project)
	}
	// Verify the plan entered the authoring lifecycle: a plan minted here without
	// plan_state=draft would read back UNGATED, silently bypassing the R6 build-gate
	// and never rendering the draft→ready→building levers. Surface that loudly rather
	// than 303-ing the operator to an ungated cockpit.
	if got := planStateOf(epic); got != planStateDraft {
		return nil, fmt.Errorf("plan epic %s plan_state = %q, want %q (authoring-lifecycle gate, R6)", epic.ID, got, planStateDraft)
	}
	return epic, nil
}

// handleCreatePlan is the POST /plans create verb — the browser-native cold-start
// that originates a plan from a one-line goal (design §4.1). Operator-gated at
// the route layer (operatorOnly wrapper in RegisterRoutes), it mints the epic +
// labels transactionally via createPlanEpic, then responds per content
// negotiation:
//
//   - HTML (a browser form submit): 303 See Other → /plans/{id}, the classic
//     POST-redirect-GET so a refresh doesn't re-submit and the operator lands in
//     the live cockpit for the plan they just created.
//   - JSON (gb plan create, which sends Accept: application/json): 200 with
//     {"id": ...} so the CLI can print the new plan id.
//
// A create OR verify failure is surfaced loudly (never a redirect to a blind
// cockpit) so an invisible-orphan epic can't slip through silently (R2).
//
// Browser write-proxy (carve-out ka-0000004319; canonical banner in
// plans_lifecycle.go). RETAINED, not deleted — the browser cannot reach the
// daemon directly, so "+ New plan" must POST here. It owns no plan invariant:
// createPlanEpic is the byte-for-byte SAME born-on-label mint + read-back verify
// `gb plan create` performs daemon-direct (cmd/gb/plan_write.go
// createPlanEpicDaemon), and the daemon folds the label read-back INTO the create
// transaction (R2). The author-assistant spawn + gardener bind below are REACTIVE
// collaboration (the controller-reactor layer), not plan-state writes, and are
// best-effort — the plan exists the instant createPlanEpic returns.
func (s *Server) handleCreatePlan(w http.ResponseWriter, r *http.Request) {
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return
	}
	goal := strings.TrimSpace(r.PostForm.Get("goal"))
	project := strings.TrimSpace(r.PostForm.Get("project"))

	// Stamp the operator on the daemon ctx so the epic's CreateBead carries
	// X-Operated-By. operatorMiddleware already stamps browser traffic, but
	// re-stamping here mirrors handlePlanAddStep (plans.go:1863) so the create
	// path stays correct on any call path — without it the kbeads audit resolver
	// fail-closes with HTTP 400 "operator could not be resolved" (R2,
	// ka-0000005772).
	ctx := beadsapi.WithOperator(r.Context(), canonicalOIDC(email))

	epic, err := s.createPlanEpic(ctx, goal, project, email)
	if err != nil {
		switch {
		case errors.Is(err, errPlanGoalRequired), errors.Is(err, errPlanProjectRequired):
			s.logger.Warn("create plan: bad input", "actor", email, "project", project, "error", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
		case errors.Is(err, errPlanGoalTooLong):
			s.logger.Warn("create plan: goal too long", "actor", email, "error", err)
			http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		default:
			// Mint or verify failed — surface it (do NOT redirect to a possibly
			// invisible plan, R2). The message names the epic id when the create
			// itself succeeded, so nothing is silently lost.
			s.logger.Error("create plan: mint/verify", "actor", email, "project", project, "error", err)
			http.Error(w, "Failed to create plan: "+err.Error(), http.StatusBadGateway)
		}
		return
	}

	// Write-through (D5): the epic is persisted + discoverable at /plans/{id} now.
	// 3c (ka-0000004399) chains spawnPlanAssistant HERE so the one-line goal becomes
	// the author assistant's first prompt and kicks the research/draft loop — 3a
	// landed the create + verify + response spine this hangs off.
	s.spawnPlanAuthorAssistant(ctx, epic, goal, email)

	// Bind the DURABLE plan-gardener loop alongside the one-shot author
	// assistant (task ka-0000002863). The epic was minted with
	// plan_state=draft, so the gardener is born PAUSED behind the R6
	// approve→build gate; handlePlanBuild enables it. Best-effort like the
	// spawn above — a bind failure never fails the create.
	s.bindPlanGardener(ctx, epic, email)

	http.Redirect(w, r, s.basePath+"/plans/"+url.PathEscape(epic.ID), http.StatusSeeOther)
}

// spawnPlanAuthorAssistant chains create → spawnPlanAssistant(epic, goal) — task
// 3c (ka-0000004399), design §4.2 step 4. The one-line goal becomes the author
// assistant's first prompt (fields.prompt → the pod's BOAT_PROMPT), so the moment
// the operator lands in the /plans/{id} cockpit the assistant is already waking to
// research and draft the plan from that seed — no seed epic, no manual first
// message. spawnPlanAssistant does the rest of the work S1 already built: it mints
// the type=agent plan-gardener bead, stamps fields.plan_id = epic.ID + the assigned
// edge, and — because a brand-new plan has no prior assistant — always takes the
// FRESH-spawn branch (never the wake path), so this is a clean cold-start.
//
// BEST-EFFORT by design. The epic is already write-through (D5): persisted and
// discoverable at /plans/{id} the instant createPlanEpic returned. A spawn failure
// here must NOT turn a plan that genuinely exists into an operator-facing error —
// the cockpit renders the "none" assistant chip and the operator's very first lane
// message (handlePlanAssistantPrompt) spawns the assistant on the identical path.
// So a failure is logged (Warn — visible, never silent) and the request still
// succeeds; the caller has already committed to the 303/JSON response for the live
// plan. This mirrors spawnPlanAssistant's own Warn-only treatment of its label/edge
// writes and the PlanStepReactor's reactive-spawn Warn: the plan's existence never
// hinges on the assistant's spawn.
//
// The forced "waking" chip that this seeds against is 3b/S2's concern
// (handlePlanAssistantPrompt renders planAssistantStateWaking on the synchronous
// POST); on the create-redirect the cockpit derives the chip from the freshly
// minted agent bead's own state, which starts as spawning → live.
func (s *Server) spawnPlanAuthorAssistant(ctx context.Context, epic *beadsapi.BeadDetail, goal, actor string) {
	if epic == nil {
		return
	}
	id, name, err := s.spawnPlanAssistant(ctx, actor, epic, goal)
	if err != nil {
		// Loud, not fatal: the plan exists (write-through, D5); the operator can
		// spawn the assistant from the cockpit lane. Never a request failure. Warn
		// (not Error) matches the swallowed-spawn convention — spawnPlanAssistant's
		// own label/edge writes and the PlanStepReactor's reactive spawn both Warn;
		// Error is reserved for promptPlanAssistant, which ALSO returns the failure
		// as a 502 (i.e. is not best-effort).
		s.logger.Warn("create plan: spawn author assistant",
			"plan", epic.ID, "actor", actor, "error", err)
		return
	}
	s.logger.Info("create plan: author assistant spawned",
		"plan", epic.ID, "assistant", id, "agent", name, "actor", actor)
}
