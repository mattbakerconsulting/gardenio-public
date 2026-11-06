package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// storeCreatedBeads wires the fake so every successful CreateBead lands in the
// bead store — the create path's verify-before-return reads the epic back via
// GetBead, so the fixture must persist what it just minted (mirrors the
// thread_compose "seed each minted head" pattern). Field values are
// stringified the way the daemon renders them back on BeadDetail.Fields
// (bools/numbers as their JSON text), so a stored gardener trigger — whose
// create carries enabled:false and integer cooldowns — reads back with a
// non-nil, comparable Fields map.
func storeCreatedBeads(f *fakeDaemon) {
	f.createOnSuccess = func(fd *fakeDaemon, id string, req beadsapi.CreateBeadRequest) {
		var raw map[string]any
		if len(req.Fields) > 0 {
			_ = json.Unmarshal(req.Fields, &raw)
		}
		fields := make(map[string]string, len(raw))
		for k, v := range raw {
			switch t := v.(type) {
			case string:
				fields[k] = t
			default:
				enc, err := json.Marshal(v)
				if err == nil {
					fields[k] = string(enc)
				}
			}
		}
		fd.beads[id] = &beadsapi.BeadDetail{
			ID:          id,
			Title:       req.Title,
			Type:        req.Type,
			Kind:        req.Kind,
			Status:      "open",
			Labels:      append([]string(nil), req.Labels...),
			Description: req.Description,
			CreatedBy:   req.CreatedBy,
			Fields:      fields,
		}
	}
}

// TestCreatePlanEpic_BornOnLabelsAndVerify is the core R2 assertion: the minted
// epic carries BOTH the plan marker and the project scope in ONE atomic
// CreateBead (no follow-up AddLabel), and the helper reads it back and returns
// the verified epic.
func TestCreatePlanEpic_BornOnLabelsAndVerify(t *testing.T) {
	f := newFake()
	storeCreatedBeads(f)
	f.createResp = "gd-newplan"
	srv := NewServer(f, testLogger(), "", "", "", nil)

	epic, err := srv.createPlanEpic(t.Context(), "Ship the widget", "gardenio", "op@acme.example")
	if err != nil {
		t.Fatalf("createPlanEpic: %v", err)
	}
	if epic == nil || epic.ID != "gd-newplan" {
		t.Fatalf("epic = %+v, want id gd-newplan", epic)
	}

	// The labels rode the create transaction — not a best-effort AddLabel.
	if f.addLabelCalls != 0 {
		t.Errorf("createPlanEpic used %d AddLabel calls; labels must be born-on via CreateBeadRequest.Labels (R2)", f.addLabelCalls)
	}
	req := f.createLastReq
	if req.Type != "epic" {
		t.Errorf("create type = %q, want epic", req.Type)
	}
	if !hasLabelInSlice(req.Labels, planLabel) {
		t.Errorf("create labels = %v, want to include %q", req.Labels, planLabel)
	}
	if !hasLabelInSlice(req.Labels, "project:gardenio") {
		t.Errorf("create labels = %v, want to include project:gardenio", req.Labels)
	}
	// The verified epic really is a discoverable plan.
	if !hasLabel(epic, planLabel) || projectFromLabels(epic.Labels) != "gardenio" {
		t.Errorf("verified epic labels = %v, want plan + project:gardenio", epic.Labels)
	}
	// The goal seeds the description verbatim (assistant's first prompt in 3c);
	// the title is the single-line derivation.
	if req.Description != "Ship the widget" {
		t.Errorf("description = %q, want the raw goal", req.Description)
	}
	if req.Title != "Ship the widget" {
		t.Errorf("title = %q, want the goal collapsed to one line", req.Title)
	}
}

// TestCreatePlanEpic_BornOnDraftState asserts the R6 authoring-lifecycle gate:
// the minted epic enters the lifecycle at plan_state=draft in the SAME create
// transaction as the labels (not a post-hoc UpdateBeadFieldsTyped), and the
// helper reads that back on the verified epic. Without this a browser-created
// plan is ungated — the "Approve & build" gate is bypassed and the
// draft→ready→building levers never render.
func TestCreatePlanEpic_BornOnDraftState(t *testing.T) {
	f := newFake()
	storeCreatedBeads(f)
	f.createResp = "gd-newplan"
	srv := NewServer(f, testLogger(), "", "", "", nil)

	epic, err := srv.createPlanEpic(t.Context(), "Ship the widget", "gardenio", "op@acme.example")
	if err != nil {
		t.Fatalf("createPlanEpic: %v", err)
	}

	// plan_state rode the create transaction — not a follow-up typed field write.
	if f.updateFieldsCalls != 0 {
		t.Errorf("createPlanEpic used %d UpdateBeadFieldsTyped calls; plan_state must be born-on via CreateBeadRequest.Fields (R6)", f.updateFieldsCalls)
	}
	var got map[string]string
	if len(f.createLastReq.Fields) > 0 {
		if err := json.Unmarshal(f.createLastReq.Fields, &got); err != nil {
			t.Fatalf("decoding create Fields: %v", err)
		}
	}
	if got[planStateFieldKey] != string(planStateDraft) {
		t.Errorf("create Fields[%q] = %q, want %q (born-on draft state)", planStateFieldKey, got[planStateFieldKey], planStateDraft)
	}
	// The verified epic really entered the lifecycle.
	if st := planStateOf(epic); st != planStateDraft {
		t.Errorf("verified epic plan_state = %q, want %q", st, planStateDraft)
	}
}

// TestCreatePlanEpic_ReadbackFoldedIntoCreate locks in the ka-0000002898 win:
// the daemon reads the labels back inside the create transaction and returns the
// committed bead, so createPlanEpic asserts against the CreateBeadDetail response
// directly and issues NO separate GetBead round-trip. The all-or-nothing
// guarantee is a daemon property, not a second viewer-side check.
func TestCreatePlanEpic_ReadbackFoldedIntoCreate(t *testing.T) {
	f := newFake()
	storeCreatedBeads(f)
	f.createResp = "gd-newplan"
	srv := NewServer(f, testLogger(), "", "", "", nil)

	epic, err := srv.createPlanEpic(t.Context(), "Ship the widget", "gardenio", "op@acme.example")
	if err != nil {
		t.Fatalf("createPlanEpic: %v", err)
	}
	// Exactly one create; zero standalone reads — the create response IS the
	// verify-readback now.
	if f.createCalls != 1 {
		t.Errorf("createPlanEpic issued %d CreateBead calls, want exactly 1", f.createCalls)
	}
	if f.getBeadCalls != 0 {
		t.Errorf("createPlanEpic issued %d GetBead calls, want 0 (readback folded into the create transaction, R2)", f.getBeadCalls)
	}
	// The returned epic still carries the persisted plan shape (from the create
	// response, not a follow-up fetch).
	if !hasLabel(epic, planLabel) || projectFromLabels(epic.Labels) != "gardenio" {
		t.Errorf("epic labels = %v, want plan + project:gardenio off the create response", epic.Labels)
	}
	if planStateOf(epic) != planStateDraft {
		t.Errorf("epic plan_state = %q, want draft off the create response", planStateOf(epic))
	}
}

func TestCreatePlanEpic_TitleCollapsesGoal(t *testing.T) {
	f := newFake()
	storeCreatedBeads(f)
	f.createResp = "gd-p"
	srv := NewServer(f, testLogger(), "", "", "", nil)

	goal := "Make the\n\n  thing   work\tbetter"
	if _, err := srv.createPlanEpic(t.Context(), goal, "gardenio", "op@acme.example"); err != nil {
		t.Fatalf("createPlanEpic: %v", err)
	}
	if got := f.createLastReq.Title; got != "Make the thing work better" {
		t.Errorf("title = %q, want whitespace collapsed to a single line", got)
	}
	// The description keeps the goal verbatim.
	if got := f.createLastReq.Description; got != goal {
		t.Errorf("description = %q, want verbatim goal", got)
	}
}

func TestCreatePlanEpic_IdempotencyKeyIsStableAndScoped(t *testing.T) {
	f := newFake()
	storeCreatedBeads(f)
	f.createResp = "gd-p"
	srv := NewServer(f, testLogger(), "", "", "", nil)

	if _, err := srv.createPlanEpic(t.Context(), "goal", "gardenio", "op@acme.example"); err != nil {
		t.Fatalf("createPlanEpic: %v", err)
	}
	k1 := f.createLastReq.IdempotencyKey
	if k1 == "" || !strings.HasPrefix(k1, "plan-create:") {
		t.Fatalf("idempotency key = %q, want a plan-create: prefix", k1)
	}
	// Same (goal, project, actor) → same key (dedup a double-submit).
	if k2 := planCreateIdempotencyKey("goal", "gardenio", "op@acme.example"); k2 != k1 {
		t.Errorf("key not stable: %q vs %q", k1, k2)
	}
	// A different actor → a different key (two operators, two plans).
	if kOther := planCreateIdempotencyKey("goal", "gardenio", "other@acme.example"); kOther == k1 {
		t.Error("idempotency key must incorporate the actor so distinct operators get distinct plans")
	}
	// A different project → a different key.
	if kProj := planCreateIdempotencyKey("goal", "kbeads", "op@acme.example"); kProj == k1 {
		t.Error("idempotency key must incorporate the project")
	}
}

func TestCreatePlanEpic_RejectsBadInput(t *testing.T) {
	f := newFake()
	srv := NewServer(f, testLogger(), "", "", "", nil)
	cases := []struct {
		name, goal, project string
	}{
		{"empty goal", "   ", "gardenio"},
		{"empty project", "goal", "  "},
		{"goal too long", strings.Repeat("x", maxPlanGoalLen+1), "gardenio"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := srv.createPlanEpic(t.Context(), tc.goal, tc.project, "op@acme.example"); err == nil {
				t.Fatalf("createPlanEpic(%q, %q) = nil error, want a rejection", tc.goal, tc.project)
			}
			if f.createCalls != 0 {
				t.Errorf("bad input must NOT reach CreateBead; got %d calls", f.createCalls)
			}
		})
	}
}

// TestCreatePlanEpic_VerifyFailsOnMissingLabel is the R2 guard: if the epic reads
// back WITHOUT the plan label (a daemon-level anomaly), the helper errors loudly
// naming the epic id, rather than returning an invisible orphan.
func TestCreatePlanEpic_VerifyFailsOnMissingLabel(t *testing.T) {
	f := newFake()
	f.createResp = "gd-orphan"
	// Persist the created epic WITHOUT the plan label to simulate a mislabel.
	f.createOnSuccess = func(fd *fakeDaemon, id string, req beadsapi.CreateBeadRequest) {
		fd.beads[id] = &beadsapi.BeadDetail{
			ID: id, Type: "epic", Kind: "issue",
			Labels: []string{"project:gardenio"}, // plan label dropped
		}
	}
	srv := NewServer(f, testLogger(), "", "", "", nil)

	_, err := srv.createPlanEpic(t.Context(), "goal", "gardenio", "op@acme.example")
	if err == nil {
		t.Fatal("want an error when the epic reads back without the plan label (R2)")
	}
	if !strings.Contains(err.Error(), "gd-orphan") || !strings.Contains(err.Error(), planLabel) {
		t.Errorf("error %q should name the orphan epic id and the missing label", err.Error())
	}
}

func TestCreatePlanEpic_VerifyFailsOnWrongProject(t *testing.T) {
	f := newFake()
	f.createResp = "gd-x"
	f.createOnSuccess = func(fd *fakeDaemon, id string, req beadsapi.CreateBeadRequest) {
		fd.beads[id] = &beadsapi.BeadDetail{
			ID: id, Type: "epic", Kind: "issue",
			Labels: []string{planLabel, "project:WRONG"},
		}
	}
	srv := NewServer(f, testLogger(), "", "", "", nil)

	if _, err := srv.createPlanEpic(t.Context(), "goal", "gardenio", "op@acme.example"); err == nil {
		t.Fatal("want an error when the epic reads back with the wrong project label (R2)")
	}
}

// TestHandleCreatePlan_RedirectsToNewCockpit: a browser form submit mints the
// epic and 303s to /plans/{id} (POST-redirect-GET).
func TestHandleCreatePlan_RedirectsToNewCockpit(t *testing.T) {
	f := newFake()
	storeCreatedBeads(f)
	f.createResp = "gd-newplan"
	mux := plansAssistantMux(t, f, nil)

	form := url.Values{"goal": {"Ship the widget"}, "project": {"gardenio"}}
	req := operatorPost("/plans", form.Encode())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (body: %s)", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); loc != "/plans/gd-newplan" {
		t.Errorf("Location = %q, want /plans/gd-newplan", loc)
	}
	// Three creates now: the plan epic, the chained author assistant (3c),
	// then the auto-bound durable plan-gardener trigger (ka-0000002863).
	if f.createCalls != 3 {
		t.Errorf("expected three CreateBead calls (epic + author assistant + gardener trigger), got %d", f.createCalls)
	}
	// Operator stamped on the daemon ctx (audit fail-closed guard, R2). The
	// LAST create is the gardener bind, which must carry the operator too so
	// its own daemon writes don't fail closed.
	if f.createLastOpBy != "oidc:op@acme.example" {
		t.Errorf("create operator = %q, want oidc:op@acme.example", f.createLastOpBy)
	}
}

// planCreateDistinctIDs returns a createIDForReq hook that mints the epic under
// epicID, the chained author assistant (agent) under asstID, and the auto-bound
// durable gardener (trigger, ka-0000002863) under a fixed gardener id — so a
// create→spawn→bind test can tell the three minted beads apart in the store
// (without this the trigger would collide with the epic's id and overwrite it).
func planCreateDistinctIDs(epicID, asstID string) func(beadsapi.CreateBeadRequest) string {
	return func(req beadsapi.CreateBeadRequest) string {
		switch req.Type {
		case "agent":
			return asstID
		case "trigger":
			return "gd-gardener-" + epicID
		}
		return epicID
	}
}

// TestHandleCreatePlan_DoubleSubmitCollapses proves the emergent idempotency of the
// create→spawn chain: a fat-fingered second POST of the SAME (goal, project, actor)
// must NOT mint a second plan or a second fresh assistant. The epic collapses via
// the daemon's IdempotencyKey dedup (kbeads migration 029), and the chained spawn
// finds the prior assistant and REVIVES IT IN PLACE (wake path, session_resume=true)
// instead of cold-starting a duplicate — so a double-click leaves exactly one plan
// with exactly one assistant (R3).
func TestHandleCreatePlan_DoubleSubmitCollapses(t *testing.T) {
	f := newFake()
	storeCreatedBeads(f)
	f.createIDForReq = planCreateDistinctIDs("gd-newplan", "gd-author")
	mux := plansAssistantMux(t, f, nil)

	post := func() *httptest.ResponseRecorder {
		form := url.Values{"goal": {"Ship the widget"}, "project": {"gardenio"}}
		req := operatorPost("/plans", form.Encode())
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w
	}

	// countMinted tallies how many CreateBead requests of each type actually minted
	// a bead — i.e. were NOT collapsed by the daemon's IdempotencyKey dedup (which
	// returns the existing id and mints nothing). The fake counts a deduped call in
	// createCalls but only records the FIRST id per key in createByIdemKey, so we
	// count distinct minted ids instead of raw call attempts.
	countMinted := func(beadType string) int {
		mintedIDs := map[string]struct{}{}
		for _, req := range f.createReqs {
			if req.Type != beadType {
				continue
			}
			if req.IdempotencyKey != "" {
				// A key we've already seen was a dedup hit → no new bead.
				mintedIDs[f.createByIdemKey[req.IdempotencyKey]] = struct{}{}
			} else {
				// No key (the assistant create) → each call is its own bead.
				mintedIDs[f.createIDForReq(req)] = struct{}{}
			}
		}
		return len(mintedIDs)
	}

	// First submit: mint the epic + a FRESH author assistant.
	w1 := post()
	if w1.Code != http.StatusSeeOther {
		t.Fatalf("first submit status = %d, want 303 (body: %s)", w1.Code, w1.Body.String())
	}

	// Second (duplicate) submit: same operator, same goal, same project.
	w2 := post()
	if w2.Code != http.StatusSeeOther {
		t.Fatalf("second submit status = %d, want 303 (body: %s)", w2.Code, w2.Body.String())
	}

	// Both redirect to the SAME plan cockpit — the epic deduped (R3).
	if loc1, loc2 := w1.Header().Get("Location"), w2.Header().Get("Location"); loc1 != loc2 || loc2 != "/plans/gd-newplan" {
		t.Errorf("double-submit redirected to %q then %q, want both /plans/gd-newplan", loc1, loc2)
	}
	// Exactly one plan epic was minted across both submits (IdempotencyKey dedup).
	if got := countMinted("epic"); got != 1 {
		t.Errorf("double-submit minted %d plan epics, want exactly 1 (IdempotencyKey dedup)", got)
	}
	// Exactly one author assistant was minted: the second spawn took the WAKE path
	// (revive in place), which does not CreateBead a duplicate.
	if got := countMinted("agent"); got != 1 {
		t.Errorf("double-submit minted %d author assistants, want exactly 1 (second is a wake, not a fresh spawn)", got)
	}
	// Exactly one durable gardener was minted: the second bind found the first
	// submit's gardener (plangardener idempotent-adopt) and created nothing.
	if got := countMinted("trigger"); got != 1 {
		t.Errorf("double-submit minted %d gardener triggers, want exactly 1 (second bind must adopt)", got)
	}
	// The second spawn revived the prior assistant in place (session_resume=true),
	// not a cold start — the wake-continuity contract. ONE atomic status+fields
	// write carries session_resume (ka-0000001583).
	if f.updateLastID != "gd-author" {
		t.Errorf("double-submit updated %q, want the prior assistant gd-author (wake in place)", f.updateLastID)
	}
	if got := f.updateLastReq.Fields["session_resume"]; got != "true" {
		t.Errorf("double-submit spawn should wake the prior assistant (session_resume=true); got %v", got)
	}
}

// TestHandleCreatePlan_ChainsAuthorAssistant is the core 3c assertion: creating a
// plan chains spawnPlanAssistant(epic, goal) so the one-line goal becomes the
// author assistant's FIRST prompt. The minted agent bead carries the plan-gardener
// role, is bound to the epic by fields.plan_id, and rides the assigned edge — the
// exact wiring the cockpit lane (S2) and the pod entrypoint (fields.prompt →
// BOAT_PROMPT) depend on to kick the research/draft loop.
func TestHandleCreatePlan_ChainsAuthorAssistant(t *testing.T) {
	f := newFake()
	storeCreatedBeads(f)
	f.createIDForReq = planCreateDistinctIDs("gd-newplan", "gd-author")
	mux := plansAssistantMux(t, f, nil)

	const goal = "Ship the widget end to end"
	form := url.Values{"goal": {goal}, "project": {"gardenio"}}
	req := operatorPost("/plans", form.Encode())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	// The plan still redirects to its live cockpit — the spawn is chained, not a
	// precondition for the response.
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (body: %s)", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); loc != "/plans/gd-newplan" {
		t.Errorf("Location = %q, want /plans/gd-newplan", loc)
	}

	// Exactly three creates: the epic, the chained author assistant, then the
	// auto-bound durable plan-gardener trigger (ka-0000002863).
	if f.createCalls != 3 {
		t.Fatalf("expected 3 CreateBead calls (epic + author assistant + gardener trigger), got %d", f.createCalls)
	}
	// The assistant create is the second one — an agent bead seeded with the
	// goal as its spawn prompt.
	asst := f.createReqs[1]
	if asst.Type != "agent" {
		t.Fatalf("second create type = %q, want the chained agent bead", asst.Type)
	}
	fields := decodeFakeFields(t, asst.Fields)
	if fields["prompt"] != goal {
		t.Errorf("author assistant prompt = %q, want the one-line goal %q", fields["prompt"], goal)
	}
	if fields[planIDField] != "gd-newplan" {
		t.Errorf("author assistant plan_id = %q, want the new epic gd-newplan", fields[planIDField])
	}
	if fields["role"] != planGardenerRole {
		t.Errorf("author assistant role = %q, want %q", fields["role"], planGardenerRole)
	}
	// A brand-new plan has no prior assistant, so this is a FRESH spawn, never a
	// wake — no session_resume.
	if fields["session_resume"] == "true" {
		t.Error("first-contact author spawn must not set session_resume=true")
	}
	// The load-bearing assigned edge (agent -> epic) the cockpit resolves on.
	if f.addDepLastType != "assigned" || f.addDepLastDep != "gd-newplan" {
		t.Errorf("assigned edge = (%s -> %s), want assigned -> gd-newplan", f.addDepLastType, f.addDepLastDep)
	}
	// The assistant's own daemon writes must carry the operator (audit fail-closed
	// guard, R2) — the same ctx the epic create used.
	if f.createLastOpBy != "oidc:op@acme.example" {
		t.Errorf("assistant create operator = %q, want oidc:op@acme.example", f.createLastOpBy)
	}
}

// TestHandleCreatePlan_SpawnFailureIsBestEffort: the plan epic mints and verifies,
// but the chained author-assistant spawn fails. Because the epic is write-through
// (D5) — already persisted + discoverable at /plans/{id} — a spawn failure must
// NOT turn a plan that genuinely exists into an operator-facing error. The request
// still 303s to the live cockpit; the operator can spawn the assistant from the
// lane. The failure is logged, never silent, but never fatal.
func TestHandleCreatePlan_SpawnFailureIsBestEffort(t *testing.T) {
	f := newFake()
	storeCreatedBeads(f)
	f.createIDForReq = planCreateDistinctIDs("gd-newplan", "gd-author")
	// Fail ONLY the assistant (agent) create; the epic create succeeds.
	f.createErrForReq = func(req beadsapi.CreateBeadRequest) error {
		if req.Type == "agent" {
			return errors.New("assistant daemon unavailable")
		}
		return nil
	}
	mux := plansAssistantMux(t, f, nil)

	form := url.Values{"goal": {"Ship the widget"}, "project": {"gardenio"}}
	req := operatorPost("/plans", form.Encode())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	// The plan exists (write-through) → still a 303 to the cockpit, NOT a 5xx.
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 — a spawn failure must not fail a create for a plan that already exists (D5)", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/plans/gd-newplan" {
		t.Errorf("Location = %q, want /plans/gd-newplan", loc)
	}
	// All three creates were attempted (epic ok, assistant failed, and the
	// gardener bind still ran — the two best-effort chains are independent).
	if f.createCalls != 3 {
		t.Errorf("expected 3 CreateBead attempts (epic + failed assistant + gardener trigger), got %d", f.createCalls)
	}
}

func TestHandleCreatePlan_MissingGoalIsBadRequest(t *testing.T) {
	f := newFake()
	mux := plansAssistantMux(t, f, nil)

	form := url.Values{"goal": {"   "}, "project": {"gardenio"}}
	req := operatorPost("/plans", form.Encode())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if f.createCalls != 0 {
		t.Errorf("bad input must not mint a bead; got %d CreateBead calls", f.createCalls)
	}
}

func TestHandleCreatePlan_MissingProjectIsBadRequest(t *testing.T) {
	f := newFake()
	mux := plansAssistantMux(t, f, nil)

	form := url.Values{"goal": {"a real goal"}}
	req := operatorPost("/plans", form.Encode())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestHandleCreatePlan_GoalTooLongIs413(t *testing.T) {
	f := newFake()
	mux := plansAssistantMux(t, f, nil)

	form := url.Values{"goal": {strings.Repeat("x", maxPlanGoalLen+1)}, "project": {"gardenio"}}
	req := operatorPost("/plans", form.Encode())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", w.Code)
	}
}

// TestHandleCreatePlan_CreateFailureIsLoud: when the mint/verify fails, the
// handler surfaces a 502 and does NOT redirect to a blind cockpit (R2).
func TestHandleCreatePlan_CreateFailureIsLoud(t *testing.T) {
	f := newFake()
	f.createErr = &beadsapi.APIError{StatusCode: http.StatusBadGateway, Message: "daemon down"}
	mux := plansAssistantMux(t, f, nil)

	form := url.Values{"goal": {"goal"}, "project": {"gardenio"}}
	req := operatorPost("/plans", form.Encode())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code == http.StatusSeeOther {
		t.Fatal("a create failure must NOT 303 to a cockpit for a plan that may not exist (R2)")
	}
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
}

// TestHandleCreatePlan_Unauthenticated: no operator email → 401 (the operator
// gate at the route layer admits role-less dev traffic, but the handler still
// requires an attributable identity to stamp on the epic).
func TestHandleCreatePlan_Unauthenticated(t *testing.T) {
	f := newFake()
	mux := plansAssistantMux(t, f, nil)

	form := url.Values{"goal": {"goal"}, "project": {"gardenio"}}
	// No X-Auth-Request-Email header, and no operator role in context.
	req := httptest.NewRequest(http.MethodPost, "/plans", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

// TestHandleCreatePlan_RouteIsOperatorGated: a viewer WITHOUT the operator role
// is rejected by the route wrapper before the handler runs (parity with every
// other /plans write).
func TestHandleCreatePlan_RouteIsOperatorGated(t *testing.T) {
	f := newFake()
	mux := plansAssistantMux(t, f, nil)

	form := url.Values{"goal": {"goal"}, "project": {"gardenio"}}
	req := httptest.NewRequest(http.MethodPost, "/plans", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Auth-Request-Email", "viewer@acme.example")
	// A non-operator realm role → the operatorOnly wrapper must reject.
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleViewer}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code == http.StatusSeeOther || w.Code == http.StatusOK {
		t.Fatalf("status = %d, want a rejection: POST /plans must be operator-gated", w.Code)
	}
	if f.createCalls != 0 {
		t.Errorf("a non-operator must not mint a plan; got %d CreateBead calls", f.createCalls)
	}
}

// hasLabelInSlice is a test-local helper (hasLabel takes a *BeadDetail; here we
// assert on the raw request Labels slice).
func hasLabelInSlice(labels []string, want string) bool {
	return slices.Contains(labels, want)
}
