package main

// Regression coverage for bug ka-0000007777: the cockpit (plan_agent) and the
// peek drawer must offer "Mark complete" on an UNSET (legacy/ungated) plan, not
// only on a `building` one. The daemon's validatePlanStateTransition admits any
// edge FROM unset (backward-compat carve-out) plus building→complete, so an
// unset plan is legally completable and the index card already surfaces the
// lever; before this fix the cockpit + drawer silently hid it, stranding the
// dominant open-plan shape (~39 of 56 live open plans are unset).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/oidcauth"
)

// TestPlanStateCanComplete pins the legality predicate to the daemon's
// legal-into-complete set: unset and building reach complete; draft, ready, and
// the already-terminal complete do not.
func TestPlanStateCanComplete(t *testing.T) {
	cases := []struct {
		st   planState
		want bool
	}{
		{planStateUnset, true},     // backward-compat carve-out — the bug's case
		{planStateBuilding, true},  // the cockpit's own approve→build→complete edge
		{planStateDraft, false},    // daemon rejects draft→complete
		{planStateReady, false},    // daemon rejects ready→complete
		{planStateComplete, false}, // already terminal — Reopen renders instead
	}
	for _, tc := range cases {
		if got := planStateCanComplete(tc.st); got != tc.want {
			t.Errorf("planStateCanComplete(%q) = %v, want %v", tc.st, got, tc.want)
		}
	}
}

// TestPlanGardenerView_UnsetOffersMarkComplete: the cockpit view flags
// ShowMarkComplete on an unset plan (not only building), so the terminal lever
// renders. A draft/ready plan still does NOT get it (the daemon would 400).
func TestPlanGardenerView_UnsetOffersMarkComplete(t *testing.T) {
	cases := []struct {
		st           planState
		wantComplete bool
	}{
		{planStateUnset, true},
		{planStateBuilding, true},
		{planStateDraft, false},
		{planStateReady, false},
		{planStateComplete, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.st)+"/unset-is-blank", func(t *testing.T) {
			f := newFake()
			seedPlanEpic(f, "gd-plan1")
			cap := seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", "true", "open", "")
			s := mkPlanGardenerServer(f)

			v := s.planGardenerViewFromBead(context.Background(), "gd-plan1", cap, tc.st, true, "")
			if v.ShowMarkComplete != tc.wantComplete {
				t.Errorf("plan_state=%q: ShowMarkComplete=%v, want %v", tc.st, v.ShowMarkComplete, tc.wantComplete)
			}
			// Reopen is unchanged: only the terminal `complete` state offers it here.
			if wantReopen := tc.st == planStateComplete; v.ShowReopen != wantReopen {
				t.Errorf("plan_state=%q: ShowReopen=%v, want %v", tc.st, v.ShowReopen, wantReopen)
			}
		})
	}
}

// TestPlanCockpit_UnsetPlanRendersMarkComplete drives the full cockpit render
// (GET /plans/{id}) for an operator on an UNSET open plan and asserts the
// "Mark complete" button is present — the LifecycleGated wrapper must not hide
// it just because the plan never entered the authoring lifecycle.
func TestPlanCockpit_UnsetPlanRendersMarkComplete(t *testing.T) {
	f := seedPlansFake() // in_progress plan, NO plan_state field → unset
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// Assert on the button's POST target (the class also appears in the page's
	// inline CSS, so match the actual lever markup).
	if !strings.Contains(body, `hx-post="/plans/gd-plan1/complete"`) {
		t.Errorf("cockpit of an UNSET open plan MUST render the Mark complete lever (bug ka-0000007777)")
	}
	// It must NOT offer Reopen — the plan is not terminal.
	if strings.Contains(body, `hx-post="/plans/gd-plan1/reopen"`) {
		t.Errorf("cockpit of a non-terminal unset plan must not render Reopen")
	}
}

// TestPlanPeek_UnsetPlanRendersMarkComplete drives the peek drawer
// (GET /plans/{id}/peek) for an operator on the same UNSET open plan and asserts
// the drawer's Mark complete lever renders.
func TestPlanPeek_UnsetPlanRendersMarkComplete(t *testing.T) {
	f := seedPlansFake() // unset open plan
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/plans/gd-plan1/peek", nil)
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /plans/gd-plan1/peek: expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `hx-post="/plans/gd-plan1/complete"`) {
		t.Errorf("peek drawer of an UNSET open plan MUST render the Mark complete lever (bug ka-0000007777)")
	}
	if strings.Contains(body, `hx-post="/plans/gd-plan1/reopen"`) {
		t.Errorf("peek drawer of a non-terminal unset plan must not render Reopen")
	}
}
