package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// gardenerCreates returns every recorded CreateBead request that minted a
// gardener trigger bead (type=trigger). The auto-bind path (ka-0000002863)
// mints exactly one per plan; a re-bind must mint zero (idempotent adopt).
func gardenerCreates(f *fakeDaemon) []beadsapi.CreateBeadRequest {
	var out []beadsapi.CreateBeadRequest
	for _, req := range f.createReqs {
		if req.Type == "trigger" {
			out = append(out, req)
		}
	}
	return out
}

// gardenerFieldsFromReq decodes the trigger fields off a captured create.
func gardenerFieldsFromReq(t *testing.T, req beadsapi.CreateBeadRequest) map[string]any {
	t.Helper()
	var fields map[string]any
	if err := json.Unmarshal(req.Fields, &fields); err != nil {
		t.Fatalf("decoding gardener trigger fields: %v", err)
	}
	return fields
}

// TestCreatePlan_AutoBindsBornPausedGardener is the core POST /plans assertion
// (task ka-0000002863): creating a plan (which mints it with plan_state=draft)
// auto-binds a durable plan-gardener born PAUSED (enabled=false), so the
// scheduler cannot fire an unapproved plan — "Approve & build" enables it.
func TestCreatePlan_AutoBindsBornPausedGardener(t *testing.T) {
	f := newFake()
	storeCreatedBeads(f)
	f.createIDForReq = planCreateDistinctIDs("gd-newplan", "gd-author")
	mux := plansAssistantMux(t, f, nil)

	form := url.Values{"goal": {"Ship the widget"}, "project": {"gardenio"}}
	req := operatorPost("/plans", form.Encode())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (body: %s)", w.Code, w.Body.String())
	}
	caps := gardenerCreates(f)
	if len(caps) != 1 {
		t.Fatalf("expected exactly 1 gardener trigger minted, got %d", len(caps))
	}
	fields := gardenerFieldsFromReq(t, caps[0])
	// Born paused: the plan is draft, so the gate forbids enabling.
	if got, ok := fields["enabled"].(bool); !ok || got {
		t.Errorf("gardener fields.enabled = %#v, want JSON bool false (born paused behind the approve→build gate)", fields["enabled"])
	}
	// Spawn role is plan-gardener so iterations resolve as the plan's bound agent.
	if got := fields["role"]; got != planGardenerRole {
		t.Errorf("gardener fields.role = %v, want %q", got, planGardenerRole)
	}
	// Pinned to the plan epic + carries the plan-gardener marker.
	labelSet := map[string]bool{}
	for _, l := range caps[0].Labels {
		labelSet[l] = true
	}
	for _, want := range []string{"trigger:multi", "role:gardener", gardenerEpicLabelFor("gd-newplan"), planGardenerMarkerLabel} {
		if !labelSet[want] {
			t.Errorf("gardener labels missing %q (have %v)", want, caps[0].Labels)
		}
	}
	// The bind carries the operator on ctx (audit fail-closed guard) — the LAST
	// create is the gardener, so createLastOpBy reflects it.
	if f.createLastOpBy != "oidc:op@acme.example" {
		t.Errorf("gardener create operator = %q, want oidc:op@acme.example", f.createLastOpBy)
	}
}

// TestCreatePlan_AutoBindBestEffort: a gardener bind failure must NOT fail the
// create — the plan is write-through (D5). The POST still 303s to the cockpit.
func TestCreatePlan_AutoBindBestEffort(t *testing.T) {
	f := newFake()
	storeCreatedBeads(f)
	f.createIDForReq = planCreateDistinctIDs("gd-newplan", "gd-author")
	// Fail ONLY the gardener (trigger) create; epic + assistant succeed.
	f.createErrForReq = func(req beadsapi.CreateBeadRequest) error {
		if req.Type == "trigger" {
			return errPlanTest
		}
		return nil
	}
	mux := plansAssistantMux(t, f, nil)

	form := url.Values{"goal": {"Ship the widget"}, "project": {"gardenio"}}
	req := operatorPost("/plans", form.Encode())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 — a gardener bind failure must not fail the create (D5)", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/plans/gd-newplan" {
		t.Errorf("Location = %q, want /plans/gd-newplan", loc)
	}
	// The bind was attempted (and failed) — one trigger create recorded.
	if got := len(gardenerCreates(f)); got != 1 {
		t.Errorf("expected 1 gardener create attempt, got %d", got)
	}
}

// TestCreatePlan_AutoBindIdempotent: a second POST of the same (goal, project,
// actor) collapses the epic via IdempotencyKey and must NOT mint a second
// gardener — the bind adopts the first submit's gardener.
func TestCreatePlan_AutoBindIdempotent(t *testing.T) {
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
	if w := post(); w.Code != http.StatusSeeOther {
		t.Fatalf("first submit status = %d, want 303", w.Code)
	}
	if w := post(); w.Code != http.StatusSeeOther {
		t.Fatalf("second submit status = %d, want 303", w.Code)
	}
	// countMinted: distinct minted trigger ids. The stub mints all gardeners
	// under the same id (planCreateDistinctIDs → gd-gardener-gd-newplan), so a
	// second, adopted bind records no NEW create.
	if got := len(gardenerCreates(f)); got != 1 {
		t.Errorf("double-submit minted %d gardener triggers, want exactly 1 (second bind adopts)", got)
	}
}

// TestPlanMark_AutoBindsGardenerOnAdd: hand-marking an epic as a plan binds its
// gardener too. A legacy epic with no plan_state is UNGATED, so the gardener is
// born ENABLED (the historical behaviour).
func TestPlanMark_AutoBindsGardenerOnAdd(t *testing.T) {
	f := seedPlansFake()
	// Start unlabeled + no plan_state so mark ADDS the label on a legacy epic.
	f.beads["gd-plan1"].Labels = []string{"project:gardenio"}
	f.beads["gd-plan1"].Fields = map[string]string{}
	storeCreatedBeads(f)
	mux := mkServer(t, f)

	req := httptest.NewRequest("POST", "/plans/gd-plan1/mark", nil)
	req.Header.Set("X-Auth-Request-Email", "op@acme.example")
	req = withOperatorRole(req)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (body: %s)", w.Code, w.Body.String())
	}
	caps := gardenerCreates(f)
	if len(caps) != 1 {
		t.Fatalf("hand-mark must bind exactly 1 gardener, got %d", len(caps))
	}
	fields := gardenerFieldsFromReq(t, caps[0])
	if got, ok := fields["enabled"].(bool); !ok || !got {
		t.Errorf("legacy (unset plan_state) hand-marked plan: gardener fields.enabled = %#v, want JSON bool true (ungated → born enabled)", fields["enabled"])
	}
}

// TestPlanMark_UnmarkDoesNotBind: removing the plan label (un-mark) must NOT
// bind a gardener — only the ADD direction promotes a plan.
func TestPlanMark_UnmarkDoesNotBind(t *testing.T) {
	f := seedPlansFake() // gd-plan1 already carries the plan label
	storeCreatedBeads(f)
	mux := mkServer(t, f)

	req := httptest.NewRequest("POST", "/plans/gd-plan1/mark", nil)
	req.Header.Set("X-Auth-Request-Email", "op@acme.example")
	req = withOperatorRole(req)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", w.Code)
	}
	if got := len(gardenerCreates(f)); got != 0 {
		t.Errorf("un-mark must NOT bind a gardener; got %d", got)
	}
	if f.removeLabelCalls != 1 {
		t.Errorf("un-mark should remove the plan label once; got %d", f.removeLabelCalls)
	}
}

// TestPlanMark_AutoBindIdempotent: re-marking an epic that already has a gardener
// (e.g. unmark then mark, or a double-submit) adopts rather than duplicating.
func TestPlanMark_AutoBindIdempotent(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Labels = []string{"project:gardenio"}
	f.beads["gd-plan1"].Fields = map[string]string{}
	// A gardener is already pinned to the epic (a prior bind).
	seedPlanGardenerBead(f, "gd-cap-existing", "gd-plan1", "true", "open", "")
	storeCreatedBeads(f)
	mux := mkServer(t, f)

	req := httptest.NewRequest("POST", "/plans/gd-plan1/mark", nil)
	req.Header.Set("X-Auth-Request-Email", "op@acme.example")
	req = withOperatorRole(req)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", w.Code)
	}
	if got := len(gardenerCreates(f)); got != 0 {
		t.Errorf("re-mark over an existing gardener must adopt, not duplicate; got %d creates", got)
	}
}

// errPlanTest is a sentinel bind failure for the best-effort test.
var errPlanTest = errPlanTestErr{}

type errPlanTestErr struct{}

func (errPlanTestErr) Error() string { return "gardener daemon unavailable" }

// withOperatorRole stamps the operator realm role the operatorOnly wrapper
// requires (mirrors operatorPost, but for the header-only mark requests).
func withOperatorRole(req *http.Request) *http.Request {
	return req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
}
