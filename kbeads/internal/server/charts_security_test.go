package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/kbeads/internal/metrics"
	"gardenio/kbeads/internal/model"
)

// charts_security_test.go — security-overview panel tests (ka-0000001983).
//
// These panels are identity-free by design (role/action/reason enums, never
// operator_id), so the privacy acceptance here is two-fold:
//   - the PromQL never references operator_id or any PII label
//     (TestSecurityQueries_NoIdentityLabels);
//   - every returned label value is collapsed onto a bounded enum so a
//     mislabeled/new series cannot widen the category set
//     (TestSecurity*_BoundedEnums).
//
// The token-SLO panel is v1-conditional: it must report Available=false (and
// echo the configured target) when the revocation-latency histogram is not yet
// emitted, rather than fabricating a verdict (TestTokenSLO_ConditionalOnMetric).

func TestSecurityQueries_NoIdentityLabels(t *testing.T) {
	queries := []string{
		authzDenialsQuery("7d"),
		decisionsLifecycleQuery("7d"),
		escalationsQuery("7d"),
		tokenRevocationPresenceQuery(),
		tokenRevocationQuantileQuery("0.95", "7d"),
		tokenRevocationWithinBudgetQuery("300", "7d"),
	}
	// operator_id is the engagement dimension; it must NEVER appear in a
	// security query. The PII labels must never appear anywhere either.
	forbidden := []string{"operator_id", "email", "display", "slack_user_id", "slack:", "oidc_email"}
	for _, q := range queries {
		for _, bad := range forbidden {
			if strings.Contains(q, bad) {
				t.Errorf("security query %q must NOT reference identity/PII label %q", q, bad)
			}
		}
	}
	// The grouped panels must group by their bounded dimensions only.
	if !strings.Contains(authzDenialsQuery("7d"), "by (action, reason, bead_type)") {
		t.Error("authz query must group by action,reason,bead_type")
	}
	if !strings.Contains(decisionsLifecycleQuery("7d"), "by (event, urgency)") {
		t.Error("decisions query must group by event,urgency")
	}
	if !strings.Contains(escalationsQuery("7d"), "by (escalated_by_role, outcome)") {
		t.Error("escalations query must group by escalated_by_role,outcome")
	}
}

func TestSecurityAuthz_BoundedEnumsAndOrder(t *testing.T) {
	m := newMockStore()
	s := NewBeadsServer(m, nil)
	s.SetVMClient(fakeVMVector(t, map[string][]VMSample{
		"gardenio_authz_denials_total": {
			// A bogus reason + a user-defined bead type must collapse onto the
			// bounded enums (denied / custom).
			{Metric: map[string]string{"action": "create", "reason": "wat", "bead_type": "weirdtype"}, Value: 3},
			{Metric: map[string]string{"action": "update", "reason": "insufficient_role", "bead_type": "task"}, Value: 10},
		},
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/charts/security/authz?window=7d", nil)
	rec := httptest.NewRecorder()
	s.handleChartsSecurityAuthz(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp SecurityAuthzResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Window != "7d" {
		t.Errorf("window = %q, want 7d", resp.Window)
	}
	if resp.Total != 13 {
		t.Errorf("total = %v, want 13", resp.Total)
	}
	if len(resp.Denials) != 2 {
		t.Fatalf("got %d rows, want 2", len(resp.Denials))
	}
	// Descending by count: update(10) first.
	if resp.Denials[0].Action != "update" || resp.Denials[0].Reason != "insufficient_role" {
		t.Errorf("row[0] = %+v, want update/insufficient_role first", resp.Denials[0])
	}
	// Bogus reason collapsed to "denied", weird type collapsed to "custom".
	if resp.Denials[1].Reason != reasonDenied {
		t.Errorf("unknown reason = %q, want %q", resp.Denials[1].Reason, reasonDenied)
	}
	if resp.Denials[1].BeadType != "custom" {
		t.Errorf("unknown bead_type = %q, want custom", resp.Denials[1].BeadType)
	}
}

func TestSecurityDecisions_BoundedEnums(t *testing.T) {
	m := newMockStore()
	s := NewBeadsServer(m, nil)
	s.SetVMClient(fakeVMVector(t, map[string][]VMSample{
		"gardenio_decisions_lifecycle_total": {
			{Metric: map[string]string{"event": "created", "urgency": "high"}, Value: 5},
			// Unknown event + unknown urgency must collapse to other/unknown.
			{Metric: map[string]string{"event": "frobnicated", "urgency": "spicy"}, Value: 1},
		},
		"gardenio_escalations_total": {
			{Metric: map[string]string{"escalated_by_role": "crew", "outcome": "approved"}, Value: 2},
			// Unknown role + empty outcome → other / requested.
			{Metric: map[string]string{"escalated_by_role": "wizard", "outcome": ""}, Value: 7},
		},
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/charts/security/decisions", nil)
	rec := httptest.NewRecorder()
	s.handleChartsSecurityDecisions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp SecurityDecisionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Window != defaultEngagementWindow {
		t.Errorf("window = %q, want default %q", resp.Window, defaultEngagementWindow)
	}
	// Lifecycle: created(5) first, then the collapsed other/unknown(1).
	if len(resp.Lifecycle) != 2 {
		t.Fatalf("got %d lifecycle rows, want 2", len(resp.Lifecycle))
	}
	if resp.Lifecycle[1].Event != decisionEventOther || resp.Lifecycle[1].Urgency != metrics.UrgencyUnknown {
		t.Errorf("collapsed lifecycle row = %+v, want other/unknown", resp.Lifecycle[1])
	}
	// Escalations: wizard→other, ""→requested; ordered by count desc (7 first).
	if len(resp.Escalations) != 2 {
		t.Fatalf("got %d escalation rows, want 2", len(resp.Escalations))
	}
	if resp.Escalations[0].Role != metrics.RoleOther || resp.Escalations[0].Outcome != metrics.OutcomeRequested {
		t.Errorf("collapsed escalation row = %+v, want other/requested", resp.Escalations[0])
	}
}

func TestTokenSLO_ConditionalOnMetric(t *testing.T) {
	m := newMockStore()
	s := NewBeadsServer(m, nil)
	// Presence probe returns an empty vector → histogram not yet emitted.
	s.SetVMClient(fakeVMVector(t, map[string][]VMSample{
		"count(gardenio_agent_token_revocation_latency_seconds_count)": {},
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/charts/security/token-slo", nil)
	rec := httptest.NewRecorder()
	s.handleChartsSecurityTokenSLO(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp SecurityTokenSLOResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Available {
		t.Error("Available must be false when the histogram is not emitted")
	}
	if resp.TargetSource != "default" {
		t.Errorf("target_source = %q, want default", resp.TargetSource)
	}
	// The default target (95%/5min, 99%/60min) is still echoed so the UI can
	// render the SLO definition and badge it "awaiting data".
	if len(resp.Objectives) != 2 {
		t.Fatalf("got %d objectives, want 2 (default target echoed)", len(resp.Objectives))
	}
	if resp.Objectives[0].Percentile != 0.95 || resp.Objectives[0].BudgetSeconds != 300 {
		t.Errorf("objective[0] = %+v, want 0.95/300", resp.Objectives[0])
	}
	if resp.Objectives[1].Percentile != 0.99 || resp.Objectives[1].BudgetSeconds != 3600 {
		t.Errorf("objective[1] = %+v, want 0.99/3600", resp.Objectives[1])
	}
	// No measured values when unavailable.
	if resp.Objectives[0].FractionWithinBudget != nil || resp.Objectives[0].Met != nil {
		t.Errorf("objective[0] must have no measured values when unavailable: %+v", resp.Objectives[0])
	}
}

func TestTokenSLO_AvailableWithObservations(t *testing.T) {
	m := newMockStore()
	s := NewBeadsServer(m, nil)
	s.SetVMClient(fakeVMVector(t, map[string][]VMSample{
		// Presence probe: one histogram series exists.
		"count(": {{Metric: map[string]string{}, Value: 1}},
		// p95 observed latency = 120s; p99 observed latency = 200s.
		"histogram_quantile(0.95": {{Metric: map[string]string{}, Value: 120}},
		"histogram_quantile(0.99": {{Metric: map[string]string{}, Value: 200}},
		// Within-budget fractions: 0.97 within 300s (>=0.95 → met),
		// 0.98 within 3600s (<0.99 → not met).
		`le="300"`:  {{Metric: map[string]string{}, Value: 0.97}},
		`le="3600"`: {{Metric: map[string]string{}, Value: 0.98}},
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/charts/security/token-slo?window=1d", nil)
	rec := httptest.NewRecorder()
	s.handleChartsSecurityTokenSLO(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp SecurityTokenSLOResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Available {
		t.Fatal("Available must be true when the histogram is present")
	}
	if resp.Window != "1d" {
		t.Errorf("window = %q, want 1d", resp.Window)
	}
	o95 := resp.Objectives[0]
	if o95.ObservedQuantileSeconds == nil || *o95.ObservedQuantileSeconds != 120 {
		t.Errorf("p95 observed = %v, want 120", o95.ObservedQuantileSeconds)
	}
	if o95.FractionWithinBudget == nil || *o95.FractionWithinBudget != 0.97 {
		t.Errorf("p95 fraction = %v, want 0.97", o95.FractionWithinBudget)
	}
	if o95.Met == nil || !*o95.Met {
		t.Errorf("p95 Met = %v, want true (0.97 >= 0.95)", o95.Met)
	}
	o99 := resp.Objectives[1]
	if o99.Met == nil || *o99.Met {
		t.Errorf("p99 Met = %v, want false (0.98 < 0.99)", o99.Met)
	}
}

func TestTokenSLO_TargetFromConfig(t *testing.T) {
	m := newMockStore()
	// Operator-approved target overrides the default via the config key.
	_ = m.SetConfig(t.Context(), &model.Config{
		Key:   tokenRevocationSLOConfigKey,
		Value: json.RawMessage(`{"objectives":[{"percentile":0.9,"budget_seconds":60}]}`),
	})
	s := NewBeadsServer(m, nil)
	s.SetVMClient(fakeVMVector(t, map[string][]VMSample{
		"count(": {}, // not emitted yet — focus is on target provenance
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/charts/security/token-slo", nil)
	rec := httptest.NewRecorder()
	s.handleChartsSecurityTokenSLO(rec, req)

	var resp SecurityTokenSLOResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.TargetSource != "config" {
		t.Errorf("target_source = %q, want config", resp.TargetSource)
	}
	if len(resp.Objectives) != 1 || resp.Objectives[0].Percentile != 0.9 || resp.Objectives[0].BudgetSeconds != 60 {
		t.Errorf("objectives = %+v, want one 0.9/60 from config", resp.Objectives)
	}
}

func TestTokenSLO_MalformedConfigFallsBackToDefault(t *testing.T) {
	m := newMockStore()
	_ = m.SetConfig(t.Context(), &model.Config{
		Key:   tokenRevocationSLOConfigKey,
		Value: json.RawMessage(`{"objectives":[{"percentile":5,"budget_seconds":-1}]}`), // nonsense
	})
	s := NewBeadsServer(m, nil)
	s.SetVMClient(fakeVMVector(t, map[string][]VMSample{"count(": {}}))

	req := httptest.NewRequest(http.MethodGet, "/v1/charts/security/token-slo", nil)
	rec := httptest.NewRecorder()
	s.handleChartsSecurityTokenSLO(rec, req)

	var resp SecurityTokenSLOResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.TargetSource != "default" {
		t.Errorf("nonsense config must fall back: target_source = %q, want default", resp.TargetSource)
	}
	if len(resp.Objectives) != 2 {
		t.Errorf("got %d objectives, want 2 default", len(resp.Objectives))
	}
}

func TestSecurity_503WhenNoVM(t *testing.T) {
	s := NewBeadsServer(newMockStore(), nil)
	cases := map[string]func(http.ResponseWriter, *http.Request){
		"/v1/charts/security/authz":     s.handleChartsSecurityAuthz,
		"/v1/charts/security/decisions": s.handleChartsSecurityDecisions,
		"/v1/charts/security/token-slo": s.handleChartsSecurityTokenSLO,
	}
	for path, h := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		h(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: status = %d, want 503 when VM unconfigured", path, rec.Code)
		}
	}
}

func TestSecurityTokenSLO_DefaultTargetMatchesApprovedSLO(t *testing.T) {
	// Guards against drift from the decision-gate target (ka-0000001248):
	// 95% within 5 minutes, 99% within 1 hour, with budgets aligned to the
	// emitted histogram bucket edges (300s, 3600s).
	if len(defaultTokenRevocationSLO) != 2 {
		t.Fatalf("default SLO has %d objectives, want 2", len(defaultTokenRevocationSLO))
	}
	want := []SLOObjective{{Percentile: 0.95, BudgetSeconds: 300}, {Percentile: 0.99, BudgetSeconds: 3600}}
	for i, w := range want {
		if defaultTokenRevocationSLO[i].Percentile != w.Percentile || defaultTokenRevocationSLO[i].BudgetSeconds != w.BudgetSeconds {
			t.Errorf("default objective[%d] = %+v, want %+v", i, defaultTokenRevocationSLO[i], w)
		}
	}
}
