package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"gardenio/controller/internal/beadsapi"
	gardeniometrics "gardenio/controller/internal/metrics"
)

// spendCounterValue gathers the gardenio_agent_spend_usd_total series matching
// the given label set from reg and returns its value (0 if absent). The counter
// is unexported on SpendMetrics, so a main-package test reads it through the
// registry rather than the struct.
func spendCounterValue(t *testing.T, reg *prometheus.Registry, want map[string]string) float64 {
	t.Helper()
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range fams {
		if f.GetName() != "gardenio_agent_spend_usd_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			match := true
			for k, v := range want {
				if labels[k] != v {
					match = false
					break
				}
			}
			if match {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// reconcilerWithMetrics builds a reconciler wired to a fresh SpendMetrics
// registered on reg, with a static label resolver, for the F2 counter tests.
func reconcilerWithMetrics(t *testing.T, daemon *daemonStub, entries []tagSpendEntry, model, account string) (*sessionSpendReconciler, *prometheus.Registry) {
	t.Helper()
	r, _ := newTestReconciler(t, daemon, tagsRouter(entries, nil))
	sm := gardeniometrics.NewSpendMetrics()
	reg := prometheus.NewRegistry()
	if err := sm.Register(reg); err != nil {
		t.Fatalf("register spend metrics: %v", err)
	}
	r.metrics = sm
	r.resolveLabels = func(_, _ string) (string, string) { return model, account }
	return r, reg
}

// tagsRouter serves GET /spend/tags returning the captured LiteLLM shape:
//
//	[{"individual_request_tag":"agent:<name>","log_count":N,"total_spend":F}, ...]
//
// It deliberately ignores any start_date/end_date/tags query params, matching
// the live endpoint's lifetime-cumulative, filter-ignoring behavior
// (ka-0000002571). callCount records how many times it was hit.
func tagsRouter(entries []tagSpendEntry, callCount *int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/spend/tags") {
			http.NotFound(w, r)
			return
		}
		if callCount != nil {
			*callCount++
		}
		// Authorization header must carry the master key.
		if got := r.Header.Get("Authorization"); got != "Bearer test-master-key" {
			http.Error(w, "missing master key", http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(entries)
	})
}

func newTestReconciler(t *testing.T, daemon *daemonStub, litellmHandler http.Handler) (*sessionSpendReconciler, *httptest.Server) {
	t.Helper()

	daemonSrv := httptest.NewServer(daemon.handler())
	t.Cleanup(daemonSrv.Close)

	litellmSrv := httptest.NewServer(litellmHandler)
	t.Cleanup(litellmSrv.Close)

	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: daemonSrv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return newSessionSpendReconciler(client, litellmSrv.URL, "test-master-key", slog.Default()), litellmSrv
}

func TestBaseURLFor(t *testing.T) {
	r := &sessionSpendReconciler{litellmURL: "https://global.example"}

	// No resolver wired → global URL.
	if got := r.baseURLFor("acme", "crew"); got != "https://global.example" {
		t.Errorf("nil resolver should yield global URL, got %q", got)
	}

	// Resolver returns "" (no override) → global URL.
	r.resolveBaseURL = func(_, _ string) string { return "" }
	if got := r.baseURLFor("acme", "crew"); got != "https://global.example" {
		t.Errorf("empty override should yield global URL, got %q", got)
	}

	// Resolver returns a per-account base_url → that URL wins, trailing slash trimmed.
	r.resolveBaseURL = func(project, role string) string {
		if project == "e2e" {
			return "http://gardenio-e2e-litellm:4000/"
		}
		return ""
	}
	if got := r.baseURLFor("e2e", "crew"); got != "http://gardenio-e2e-litellm:4000" {
		t.Errorf("per-account override should win and be trimmed, got %q", got)
	}
	// A different project with no override still falls back to global.
	if got := r.baseURLFor("acme", "crew"); got != "https://global.example" {
		t.Errorf("non-overridden project should yield global URL, got %q", got)
	}
}

// TestReconcile_PerAccountBaseURLRouting verifies the close-time reconcile
// queries the per-account proxy resolved from the bead's project/role
// (ka-0000003221), so an in-cluster vkey reconciles against the in-cluster proxy
// instead of the global one that would 404 it.
func TestReconcile_PerAccountBaseURLRouting(t *testing.T) {
	// In-cluster proxy reports the agent tag's cumulative spend.
	inclusterEntries := []tagSpendEntry{
		{IndividualRequestTag: "agent:alpha", LogCount: 5, TotalSpend: 0.000034},
	}
	inclusterSrv := httptest.NewServer(tagsRouter(inclusterEntries, nil))
	t.Cleanup(inclusterSrv.Close)

	// Global proxy has NO record of this agent tag (like the external prod proxy).
	globalSrv := httptest.NewServer(tagsRouter([]tagSpendEntry{}, nil))
	t.Cleanup(globalSrv.Close)

	daemon := &daemonStub{
		beads: []map[string]any{makeAgentBead("crew-e2e-crew-alpha", map[string]any{
			"project":          "e2e",
			"role":             "crew",
			spendBaselineField: "0",
		})},
	}
	daemonSrv := httptest.NewServer(daemon.handler())
	t.Cleanup(daemonSrv.Close)

	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: daemonSrv.URL})
	if err != nil {
		t.Fatal(err)
	}
	// Global URL is the external proxy; the in-cluster account routes elsewhere.
	r := newSessionSpendReconciler(client, globalSrv.URL, "test-master-key", slog.Default())
	r.resolveBaseURL = func(project, _ string) string {
		if project == "e2e" {
			return inclusterSrv.URL
		}
		return ""
	}

	bead, err := client.GetBead(context.Background(), "crew-e2e-crew-alpha")
	if err != nil {
		t.Fatal(err)
	}
	r.reconcileAtClose(context.Background(), bead, "alpha")

	var total float64
	var found bool
	for _, u := range daemon.getUpdates() {
		if v, ok := u.Fields["total_cost_usd"].(float64); ok {
			total = v
			found = true
		}
	}
	if !found {
		t.Fatal("expected total_cost_usd to be written from the in-cluster proxy")
	}
	if total <= 0 {
		t.Errorf("total_cost_usd = %v, want >0 (reconciled from in-cluster proxy)", total)
	}
}

// makeAgentBead builds an agent bead with the given fields as the daemon returns it.
func makeAgentBead(id string, fields map[string]any) map[string]any {
	if fields == nil {
		fields = map[string]any{}
	}
	fieldsRaw, _ := json.Marshal(fields)
	return map[string]any{
		"id":     id,
		"title":  id,
		"type":   "agent",
		"status": "open",
		"fields": json.RawMessage(fieldsRaw),
	}
}

func TestQueryTagSpend_MatchesClientSideOnTag(t *testing.T) {
	entries := []tagSpendEntry{
		{IndividualRequestTag: "Credential: anthropic", LogCount: 67409, TotalSpend: 295.13},
		{IndividualRequestTag: "agent:alpha", LogCount: 12, TotalSpend: 4.25},
		{IndividualRequestTag: "agent:beta", LogCount: 3, TotalSpend: 0.50},
	}
	daemon := &daemonStub{}
	r, _ := newTestReconciler(t, daemon, tagsRouter(entries, nil))

	spend, found, err := r.queryTagSpend(context.Background(), r.litellmURL, "agent:alpha")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Fatal("expected agent:alpha tag to be found")
	}
	if spend != 4.25 {
		t.Errorf("spend = %v, want 4.25", spend)
	}
}

func TestQueryTagSpend_TagAbsent(t *testing.T) {
	entries := []tagSpendEntry{
		{IndividualRequestTag: "agent:other", LogCount: 1, TotalSpend: 1.00},
	}
	daemon := &daemonStub{}
	r, _ := newTestReconciler(t, daemon, tagsRouter(entries, nil))

	spend, found, err := r.queryTagSpend(context.Background(), r.litellmURL, "agent:missing")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Fatal("expected agent:missing tag to be absent")
	}
	if spend != 0 {
		t.Errorf("spend = %v, want 0 for absent tag", spend)
	}
}

func TestCaptureBaseline_StashesOnAgentBead(t *testing.T) {
	entries := []tagSpendEntry{
		{IndividualRequestTag: "agent:alpha", LogCount: 12, TotalSpend: 4.25},
	}
	daemon := &daemonStub{
		beads: []map[string]any{makeAgentBead("crew-acme-crew-alpha", nil)},
	}
	r, _ := newTestReconciler(t, daemon, tagsRouter(entries, nil))

	r.captureBaseline(context.Background(), "crew-acme-crew-alpha", "alpha", "acme", "crew")

	updates := daemon.getUpdates()
	if len(updates) != 2 {
		t.Fatalf("expected 2 updates (baseline + write-once first baseline), got %d", len(updates))
	}
	got, ok := updates[0].Fields[spendBaselineField].(float64)
	if !ok {
		t.Fatalf("%s should be float64, got %T", spendBaselineField, updates[0].Fields[spendBaselineField])
	}
	if got != 4.25 {
		t.Errorf("%s = %v, want 4.25", spendBaselineField, got)
	}
	if got := updates[1].Fields[spendFirstBaselineField].(float64); got != 4.25 {
		t.Errorf("%s = %v, want 4.25", spendFirstBaselineField, got)
	}
}

func TestCaptureBaseline_ZeroInitsSpendSeries(t *testing.T) {
	// A2 (ka-0000007288): the counter series must exist at 0 from spawn time.
	// Without it a one-shot agent's series is born at its final value by the
	// close increment and increase() reads its whole spend as 0.
	entries := []tagSpendEntry{
		{IndividualRequestTag: "agent:alpha", LogCount: 12, TotalSpend: 4.25},
	}
	daemon := &daemonStub{
		beads: []map[string]any{makeAgentBead("crew-acme-crew-alpha", nil)},
	}
	r, reg := reconcilerWithMetrics(t, daemon, entries, "claude-opus-4-8", "litellm-prod")

	r.captureBaseline(context.Background(), "crew-acme-crew-alpha", "alpha", "acme", "crew")

	// The series exists (registry returns it) and reads exactly 0. The label set
	// must match what the close-time RecordSpend will use, or the zero-init
	// would create a sibling series and fix nothing.
	fams, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range fams {
		if f.GetName() == "gardenio_agent_spend_usd_total" {
			found = len(f.GetMetric()) > 0
		}
	}
	if !found {
		t.Fatal("expected the spend series to exist after captureBaseline")
	}
	want := map[string]string{
		"agent": "alpha", "project": "acme", "role": "crew",
		"model": "claude-opus-4-8", "account": "litellm-prod",
	}
	if got := spendCounterValue(t, reg, want); got != 0 {
		t.Errorf("spend counter at spawn = %v, want 0", got)
	}
	// Operator rollup gets the same treatment ("0" sentinel here — the stub bead
	// has no resolved operator).
	if got := operatorSpendCounterValue(t, reg, map[string]string{"operator_id": "0", "project": "acme", "model": "claude-opus-4-8"}); got != 0 {
		t.Errorf("operator counter at spawn = %v, want 0", got)
	}
}

func TestCaptureBaseline_FirstBaselineIsWriteOnce(t *testing.T) {
	// A respawn re-captures the per-session baseline but must never move the
	// lifetime anchor: a bead that already carries the first baseline gets only
	// the baseline patch.
	entries := []tagSpendEntry{
		{IndividualRequestTag: "agent:alpha", LogCount: 30, TotalSpend: 9.00},
	}
	daemon := &daemonStub{
		beads: []map[string]any{makeAgentBead("crew-acme-crew-alpha", map[string]any{
			spendFirstBaselineField: "4.25",
		})},
	}
	r, _ := newTestReconciler(t, daemon, tagsRouter(entries, nil))

	r.captureBaseline(context.Background(), "crew-acme-crew-alpha", "alpha", "acme", "crew")

	updates := daemon.getUpdates()
	if len(updates) != 1 {
		t.Fatalf("expected 1 update (baseline only), got %d", len(updates))
	}
	if got := updates[0].Fields[spendBaselineField].(float64); got != 9.00 {
		t.Errorf("%s = %v, want 9.00", spendBaselineField, got)
	}
	if _, present := updates[0].Fields[spendFirstBaselineField]; present {
		t.Errorf("first baseline must not be re-written on respawn")
	}
}

func TestCaptureBaseline_AbsentTagRecordsZero(t *testing.T) {
	// Agent hasn't spent yet — tag absent. Baseline must be recorded as 0 so the
	// close delta starts from a clean zero rather than an unset field.
	daemon := &daemonStub{
		beads: []map[string]any{makeAgentBead("crew-acme-crew-alpha", nil)},
	}
	r, _ := newTestReconciler(t, daemon, tagsRouter([]tagSpendEntry{}, nil))

	r.captureBaseline(context.Background(), "crew-acme-crew-alpha", "alpha", "acme", "crew")

	updates := daemon.getUpdates()
	if len(updates) != 2 {
		t.Fatalf("expected 2 updates (baseline + write-once first baseline), got %d", len(updates))
	}
	if got := updates[0].Fields[spendBaselineField].(float64); got != 0 {
		t.Errorf("%s = %v, want 0", spendBaselineField, got)
	}
	if got := updates[1].Fields[spendFirstBaselineField].(float64); got != 0 {
		t.Errorf("%s = %v, want 0", spendFirstBaselineField, got)
	}
}

func TestReconcileAtClose_WritesDeltaToTotalCost(t *testing.T) {
	// Baseline 4.25 at start; tag now reads 7.75 → session cost 3.50. The bead
	// predates the first-baseline field, so it is back-derived: start (4.25) −
	// already-booked (0) = 4.25, written in a separate follow-up patch.
	entries := []tagSpendEntry{
		{IndividualRequestTag: "agent:alpha", LogCount: 20, TotalSpend: 7.75},
	}
	daemon := &daemonStub{
		beads: []map[string]any{makeAgentBead("crew-acme-crew-alpha", map[string]any{spendBaselineField: "4.25"})},
	}
	r, _ := newTestReconciler(t, daemon, tagsRouter(entries, nil))

	bead := &beadsapi.BeadDetail{
		ID:     "crew-acme-crew-alpha",
		Fields: map[string]string{spendBaselineField: "4.25"},
	}
	r.reconcileAtClose(context.Background(), bead, "alpha")

	updates := daemon.getUpdates()
	if len(updates) != 2 {
		t.Fatalf("expected 2 updates (settlement + first-baseline back-fill), got %d", len(updates))
	}
	cost, ok := updates[0].Fields["total_cost_usd"].(float64)
	if !ok {
		t.Fatalf("total_cost_usd should be float64, got %T", updates[0].Fields["total_cost_usd"])
	}
	if cost != 3.50 {
		t.Errorf("total_cost_usd = %v, want 3.50", cost)
	}
	if got := updates[0].Fields[spendCloseField].(float64); got != 7.75 {
		t.Errorf("%s = %v, want 7.75", spendCloseField, got)
	}
	if got := updates[1].Fields[spendFirstBaselineField].(float64); got != 4.25 {
		t.Errorf("%s = %v, want 4.25", spendFirstBaselineField, got)
	}
}

func TestReconcileAtClose_RespawnerAccumulatesAcrossSessions(t *testing.T) {
	// The L2/L3 fix (ka-0000007288): a respawning agent books EVERY session, not
	// just its first. Session 1 settled at watermark 7.75 with first-baseline
	// 4.25 and booked 3.50; session 2 spawned with a fresh baseline 7.75 and the
	// tag now reads 10.00 → lifetime becomes 5.75 (not the old overwrite's 2.25).
	entries := []tagSpendEntry{
		{IndividualRequestTag: "agent:alpha", LogCount: 30, TotalSpend: 10.00},
	}
	daemon := &daemonStub{
		beads: []map[string]any{makeAgentBead("crew-acme-crew-alpha", map[string]any{
			spendBaselineField:      "7.75",
			spendCloseField:         "7.75",
			spendFirstBaselineField: "4.25",
			"total_cost_usd":        "3.50",
		})},
	}
	r, _ := newTestReconciler(t, daemon, tagsRouter(entries, nil))

	bead := &beadsapi.BeadDetail{
		ID: "crew-acme-crew-alpha",
		Fields: map[string]string{
			spendBaselineField:      "7.75",
			spendCloseField:         "7.75",
			spendFirstBaselineField: "4.25",
			"total_cost_usd":        "3.50",
		},
	}
	r.reconcileAtClose(context.Background(), bead, "alpha")

	updates := daemon.getUpdates()
	if len(updates) != 1 {
		t.Fatalf("expected 1 update, got %d", len(updates))
	}
	if cost := updates[0].Fields["total_cost_usd"].(float64); cost != 5.75 {
		t.Errorf("total_cost_usd = %v, want 5.75 (lifetime, not last-session overwrite)", cost)
	}
	if got := updates[0].Fields[spendCloseField].(float64); got != 10.00 {
		t.Errorf("%s = %v, want 10.00", spendCloseField, got)
	}
	if _, present := updates[0].Fields[spendFirstBaselineField]; present {
		t.Errorf("first baseline must not be re-written once set")
	}
}

func TestReconcileAtClose_MissingBaselineFallsBackToWatermark(t *testing.T) {
	// captureBaseline failed on a respawn (baseline empty) but the bead settled
	// before (watermark 7.75). The watermark is the honest start point: settle
	// 10.00 − 7.75 = 2.25 instead of refusing (old behavior lost the session).
	entries := []tagSpendEntry{
		{IndividualRequestTag: "agent:alpha", LogCount: 30, TotalSpend: 10.00},
	}
	daemon := &daemonStub{
		beads: []map[string]any{makeAgentBead("crew-acme-crew-alpha", map[string]any{
			spendCloseField:         "7.75",
			spendFirstBaselineField: "4.25",
		})},
	}
	r, _ := newTestReconciler(t, daemon, tagsRouter(entries, nil))

	bead := &beadsapi.BeadDetail{
		ID: "crew-acme-crew-alpha",
		Fields: map[string]string{
			spendCloseField:         "7.75",
			spendFirstBaselineField: "4.25",
		},
	}
	r.reconcileAtClose(context.Background(), bead, "alpha")

	updates := daemon.getUpdates()
	if len(updates) != 1 {
		t.Fatalf("expected 1 update, got %d", len(updates))
	}
	if cost := updates[0].Fields["total_cost_usd"].(float64); cost != 5.75 {
		t.Errorf("total_cost_usd = %v, want 5.75", cost)
	}
}

func TestReconcileAtClose_MissingBaselineDoesNotWrite(t *testing.T) {
	// ka-0000000046: a missing baseline (captureBaseline failed, or a controller
	// restart landed between spawn and close) must NOT be treated as 0. The agent
	// tag is lifetime-cumulative, so subtracting a 0 baseline would attribute the
	// agent's whole lifetime spend (2.00 here) to this single session. With no
	// baseline to subtract we refuse to attribute and leave the bead untouched.
	entries := []tagSpendEntry{
		{IndividualRequestTag: "agent:alpha", LogCount: 5, TotalSpend: 2.00},
	}
	daemon := &daemonStub{
		beads: []map[string]any{makeAgentBead("crew-acme-crew-alpha", nil)},
	}
	r, _ := newTestReconciler(t, daemon, tagsRouter(entries, nil))

	bead := &beadsapi.BeadDetail{ID: "crew-acme-crew-alpha", Fields: map[string]string{}}
	r.reconcileAtClose(context.Background(), bead, "alpha")

	if updates := daemon.getUpdates(); len(updates) != 0 {
		t.Fatalf("expected no updates when baseline is missing, got %d", len(updates))
	}
}

func TestReconcileAtClose_CapturedZeroBaselineReconciles(t *testing.T) {
	// A genuinely-captured 0 baseline (fresh agent, no prior tag spend) is distinct
	// from a missing baseline: it must reconcile normally, attributing the full
	// close total to this — the agent's first — session.
	entries := []tagSpendEntry{
		{IndividualRequestTag: "agent:alpha", LogCount: 5, TotalSpend: 2.00},
	}
	daemon := &daemonStub{
		beads: []map[string]any{makeAgentBead("crew-acme-crew-alpha", map[string]any{spendBaselineField: "0"})},
	}
	r, _ := newTestReconciler(t, daemon, tagsRouter(entries, nil))

	bead := &beadsapi.BeadDetail{ID: "crew-acme-crew-alpha", Fields: map[string]string{spendBaselineField: "0"}}
	r.reconcileAtClose(context.Background(), bead, "alpha")

	updates := daemon.getUpdates()
	if len(updates) != 2 {
		t.Fatalf("expected 2 updates (settlement + first-baseline back-fill), got %d", len(updates))
	}
	if cost := updates[0].Fields["total_cost_usd"].(float64); cost != 2.00 {
		t.Errorf("total_cost_usd = %v, want 2.00", cost)
	}
	if got := updates[1].Fields[spendFirstBaselineField].(float64); got != 0 {
		t.Errorf("%s = %v, want 0", spendFirstBaselineField, got)
	}
}

func TestReconcileAtClose_UnparseableBaselineDoesNotWrite(t *testing.T) {
	// A corrupt/unparseable baseline is as untrustworthy as a missing one — refuse
	// to attribute rather than silently subtracting 0.
	entries := []tagSpendEntry{
		{IndividualRequestTag: "agent:alpha", LogCount: 5, TotalSpend: 2.00},
	}
	daemon := &daemonStub{
		beads: []map[string]any{makeAgentBead("crew-acme-crew-alpha", map[string]any{spendBaselineField: "garbage"})},
	}
	r, _ := newTestReconciler(t, daemon, tagsRouter(entries, nil))

	bead := &beadsapi.BeadDetail{ID: "crew-acme-crew-alpha", Fields: map[string]string{spendBaselineField: "garbage"}}
	r.reconcileAtClose(context.Background(), bead, "alpha")

	if updates := daemon.getUpdates(); len(updates) != 0 {
		t.Fatalf("expected no updates when baseline is unparseable, got %d", len(updates))
	}
}

func TestReconcileAtClose_NegativeDeltaClampedToZero(t *testing.T) {
	// Baseline higher than close total (e.g. a stale/blended baseline) must
	// never write a negative cost.
	entries := []tagSpendEntry{
		{IndividualRequestTag: "agent:alpha", LogCount: 5, TotalSpend: 1.00},
	}
	daemon := &daemonStub{
		beads: []map[string]any{makeAgentBead("crew-acme-crew-alpha", map[string]any{spendBaselineField: "5.00"})},
	}
	r, _ := newTestReconciler(t, daemon, tagsRouter(entries, nil))

	bead := &beadsapi.BeadDetail{ID: "crew-acme-crew-alpha", Fields: map[string]string{spendBaselineField: "5.00"}}
	r.reconcileAtClose(context.Background(), bead, "alpha")

	updates := daemon.getUpdates()
	if len(updates) != 2 {
		t.Fatalf("expected 2 updates (settlement + first-baseline back-fill), got %d", len(updates))
	}
	if cost := updates[0].Fields["total_cost_usd"].(float64); cost != 0 {
		t.Errorf("total_cost_usd = %v, want 0 (clamped)", cost)
	}
}

func TestReconcileAtClose_AbsentTagDoesNotWrite(t *testing.T) {
	// No tag for this agent at close (no billable requests) → leave the bead
	// untouched rather than writing a spurious 0.
	daemon := &daemonStub{}
	r, _ := newTestReconciler(t, daemon, tagsRouter([]tagSpendEntry{}, nil))

	bead := &beadsapi.BeadDetail{ID: "crew-acme-crew-alpha", Fields: map[string]string{spendBaselineField: "4.25"}}
	r.reconcileAtClose(context.Background(), bead, "alpha")

	if updates := daemon.getUpdates(); len(updates) != 0 {
		t.Fatalf("expected no updates for absent tag, got %d", len(updates))
	}
}

// TestReconcileAtClose_NilGuards: reconcileAtClose is a no-op on a nil
// receiver, a nil bead or an empty agent name (ka-0000007328).
func TestReconcileAtClose_NilGuards(t *testing.T) {
	// Must not panic on nil receiver / bead / empty agent name.
	var r *sessionSpendReconciler
	r.reconcileAtClose(context.Background(), nil, "alpha")

	daemon := &daemonStub{}
	rr, _ := newTestReconciler(t, daemon, tagsRouter(nil, nil))
	rr.reconcileAtClose(context.Background(), nil, "alpha")
	rr.reconcileAtClose(context.Background(), &beadsapi.BeadDetail{ID: "x"}, "")
	if updates := daemon.getUpdates(); len(updates) != 0 {
		t.Fatalf("expected no updates from nil-guard paths, got %d", len(updates))
	}
}

func TestParseBaselineField(t *testing.T) {
	cases := []struct {
		in     string
		want   float64
		wantOk bool
	}{
		{"", 0, false},        // missing — must NOT be treated as a 0 baseline
		{"garbage", 0, false}, // corrupt — equally untrustworthy
		{"0", 0, true},        // genuinely-captured 0 (fresh agent) reconciles
		{"4.25", 4.25, true},
		{"-1.5", -1.5, true},
	}
	for _, c := range cases {
		got, ok := parseBaselineField(c.in)
		if got != c.want || ok != c.wantOk {
			t.Errorf("parseBaselineField(%q) = (%v, %v), want (%v, %v)", c.in, got, ok, c.want, c.wantOk)
		}
	}
}

func TestAgentTag(t *testing.T) {
	if got := agentTag("alpha"); got != "agent:alpha" {
		t.Errorf("agentTag = %q, want agent:alpha", got)
	}
}

func TestReconcileAtClose_IncrementsSpendCounter(t *testing.T) {
	// F2: the reconciled session cost (close 7.75 − baseline 4.25 = 3.50) is
	// pushed into gardenio_agent_spend_usd_total, labeled with the resolved
	// model/account and the bead's project/role.
	entries := []tagSpendEntry{
		{IndividualRequestTag: "agent:alpha", LogCount: 20, TotalSpend: 7.75},
	}
	daemon := &daemonStub{
		beads: []map[string]any{makeAgentBead("crew-acme-crew-alpha", map[string]any{spendBaselineField: "4.25"})},
	}
	r, reg := reconcilerWithMetrics(t, daemon, entries, "claude-opus-4-8", "litellm-prod")

	bead := &beadsapi.BeadDetail{
		ID:     "crew-acme-crew-alpha",
		Fields: map[string]string{spendBaselineField: "4.25", "project": "acme", "role": "crew"},
	}
	r.reconcileAtClose(context.Background(), bead, "alpha")

	want := map[string]string{
		"agent": "alpha", "project": "acme", "role": "crew",
		"model": "claude-opus-4-8", "account": "litellm-prod",
	}
	if got := spendCounterValue(t, reg, want); got != 3.50 {
		t.Errorf("spend counter = %v, want 3.50", got)
	}
}

func TestReconcileAtClose_CounterIncrementsOncePerSession(t *testing.T) {
	// A re-delivered close (NATS at-least-once) or a stop→done transition must
	// not double-count. The first reconcile advances the settlement watermark to
	// the close total; a re-delivery computes a 0 not-yet-booked delta and the
	// counter does not move.
	entries := []tagSpendEntry{
		{IndividualRequestTag: "agent:alpha", LogCount: 20, TotalSpend: 7.75},
	}
	daemon := &daemonStub{
		beads: []map[string]any{makeAgentBead("crew-acme-crew-alpha", map[string]any{spendBaselineField: "4.25"})},
	}
	r, reg := reconcilerWithMetrics(t, daemon, entries, "m", "acct")

	first := &beadsapi.BeadDetail{
		ID:     "crew-acme-crew-alpha",
		Fields: map[string]string{spendBaselineField: "4.25", "project": "acme", "role": "crew"},
	}
	r.reconcileAtClose(context.Background(), first, "alpha")

	// Second delivery: the bead now carries the first write's landed state
	// (watermark at the close total), so the counter must not advance again.
	second := &beadsapi.BeadDetail{
		ID: "crew-acme-crew-alpha",
		Fields: map[string]string{
			spendBaselineField: "4.25", spendCloseField: "7.75",
			spendFirstBaselineField: "4.25", "total_cost_usd": "3.5",
			"project": "acme", "role": "crew",
		},
	}
	r.reconcileAtClose(context.Background(), second, "alpha")

	want := map[string]string{"agent": "alpha", "project": "acme", "role": "crew", "model": "m", "account": "acct"}
	if got := spendCounterValue(t, reg, want); got != 3.50 {
		t.Errorf("spend counter = %v, want 3.50 (one increment only)", got)
	}
}

func TestReconcileAtClose_ZeroCostDoesNotIncrementCounter(t *testing.T) {
	// A $0 session (close == baseline) writes total_cost_usd=0 but must not
	// advance the monotonic counter — RecordSpend drops non-positive costs.
	entries := []tagSpendEntry{
		{IndividualRequestTag: "agent:alpha", LogCount: 0, TotalSpend: 4.25},
	}
	daemon := &daemonStub{
		beads: []map[string]any{makeAgentBead("crew-acme-crew-alpha", map[string]any{spendBaselineField: "4.25"})},
	}
	r, reg := reconcilerWithMetrics(t, daemon, entries, "m", "acct")

	bead := &beadsapi.BeadDetail{
		ID:     "crew-acme-crew-alpha",
		Fields: map[string]string{spendBaselineField: "4.25", "project": "acme", "role": "crew"},
	}
	r.reconcileAtClose(context.Background(), bead, "alpha")

	want := map[string]string{"agent": "alpha", "project": "acme", "role": "crew", "model": "m", "account": "acct"}
	if got := spendCounterValue(t, reg, want); got != 0 {
		t.Errorf("spend counter = %v, want 0 (zero-cost session not counted)", got)
	}

	// ka-0000006190 — the intentional bead-vs-counter asymmetry. The SAME $0
	// session that left the counter at 0 above DID write total_cost_usd=0 onto
	// the bead. The two cost views therefore differ in series presence (a zero
	// counter series is never created) but agree in dollars ($0 == $0). This
	// asserts both halves so a future change that starts counting $0 sessions —
	// or stops writing the $0 bead field — fails loudly.
	updates := daemon.getUpdates()
	if len(updates) != 2 {
		t.Fatalf("expected 2 bead updates for the $0 session (settlement + first-baseline back-fill), got %d", len(updates))
	}
	if cost, ok := updates[0].Fields["total_cost_usd"].(float64); !ok || cost != 0 {
		t.Errorf("bead total_cost_usd = %v (ok=%v), want 0 written to the bead", updates[0].Fields["total_cost_usd"], ok)
	}
}

func TestReconcileAtClose_NilMetricsSkipsCounter(t *testing.T) {
	// A reconciler with no metrics sink (LiteLLM configured but SpendMetrics
	// unset) must still write total_cost_usd and never panic.
	entries := []tagSpendEntry{
		{IndividualRequestTag: "agent:alpha", LogCount: 5, TotalSpend: 2.00},
	}
	daemon := &daemonStub{
		beads: []map[string]any{makeAgentBead("crew-acme-crew-alpha", map[string]any{spendBaselineField: "0"})},
	}
	r, _ := newTestReconciler(t, daemon, tagsRouter(entries, nil)) // metrics nil

	bead := &beadsapi.BeadDetail{ID: "crew-acme-crew-alpha", Fields: map[string]string{spendBaselineField: "0", "project": "acme", "role": "crew"}}
	r.reconcileAtClose(context.Background(), bead, "alpha")

	if updates := daemon.getUpdates(); len(updates) != 2 {
		t.Fatalf("expected settlement + first-baseline back-fill, got %d", len(updates))
	}
}

// --- operator-keyed counter (ka-0000000999) ---

// operatorSpendCounterValue gathers the gardenio_operator_cost_usd_total series
// matching the given label set from reg and returns its value (0 if absent).
func operatorSpendCounterValue(t *testing.T, reg *prometheus.Registry, want map[string]string) float64 {
	t.Helper()
	fams, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range fams {
		if f.GetName() != "gardenio_operator_cost_usd_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			match := true
			for k, v := range want {
				if labels[k] != v {
					match = false
					break
				}
			}
			if match {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func int64Ptr(v int64) *int64 { return &v }

func TestReconcileAtClose_IncrementsOperatorCounter(t *testing.T) {
	// The same reconciled session cost (close 7.75 − baseline 4.25 = 3.50) drives
	// the operator-keyed counter, labeled {operator_id,project,model}. operator_id
	// is the bead's OperatedByID FK; model comes from the resolver.
	entries := []tagSpendEntry{
		{IndividualRequestTag: "agent:alpha", LogCount: 20, TotalSpend: 7.75},
	}
	daemon := &daemonStub{
		beads: []map[string]any{makeAgentBead("crew-acme-crew-alpha", map[string]any{spendBaselineField: "4.25"})},
	}
	r, reg := reconcilerWithMetrics(t, daemon, entries, "claude-opus-4-8", "litellm-prod")

	bead := &beadsapi.BeadDetail{
		ID:           "crew-acme-crew-alpha",
		OperatedByID: int64Ptr(42),
		Fields:       map[string]string{spendBaselineField: "4.25", "project": "acme", "role": "crew"},
	}
	r.reconcileAtClose(context.Background(), bead, "alpha")

	want := map[string]string{"operator_id": "42", "project": "acme", "model": "claude-opus-4-8"}
	if got := operatorSpendCounterValue(t, reg, want); got != 3.50 {
		t.Errorf("operator spend counter = %v, want 3.50", got)
	}
}

func TestReconcileAtClose_OperatorCounterUnattributedSentinel(t *testing.T) {
	// A bead with no resolved human operator (OperatedByID nil) must emit the "0"
	// sentinel rather than dropping the sample or rendering an empty label —
	// matching the kbeads unattributed bucket.
	entries := []tagSpendEntry{
		{IndividualRequestTag: "agent:alpha", LogCount: 5, TotalSpend: 2.00},
	}
	daemon := &daemonStub{
		beads: []map[string]any{makeAgentBead("crew-acme-crew-alpha", map[string]any{spendBaselineField: "0"})},
	}
	r, reg := reconcilerWithMetrics(t, daemon, entries, "m", "acct")

	bead := &beadsapi.BeadDetail{
		ID: "crew-acme-crew-alpha",
		// genuinely-captured 0 baseline (fresh agent), OperatedByID nil → "0" sentinel
		Fields: map[string]string{spendBaselineField: "0", "project": "acme", "role": "crew"},
	}
	r.reconcileAtClose(context.Background(), bead, "alpha")

	want := map[string]string{"operator_id": "0", "project": "acme", "model": "m"}
	if got := operatorSpendCounterValue(t, reg, want); got != 2.00 {
		t.Errorf("operator spend counter (sentinel) = %v, want 2.00", got)
	}
}

func TestReconcileAtClose_OperatorCounterOncePerSession(t *testing.T) {
	// Shares the watermark idempotency with the agent counter: a re-delivered
	// close computes a 0 delta and must not double-count the operator rollup.
	entries := []tagSpendEntry{
		{IndividualRequestTag: "agent:alpha", LogCount: 20, TotalSpend: 7.75},
	}
	daemon := &daemonStub{
		beads: []map[string]any{makeAgentBead("crew-acme-crew-alpha", map[string]any{spendBaselineField: "4.25"})},
	}
	r, reg := reconcilerWithMetrics(t, daemon, entries, "m", "acct")

	first := &beadsapi.BeadDetail{
		ID:           "crew-acme-crew-alpha",
		OperatedByID: int64Ptr(7),
		Fields:       map[string]string{spendBaselineField: "4.25", "project": "acme", "role": "crew"},
	}
	r.reconcileAtClose(context.Background(), first, "alpha")

	second := &beadsapi.BeadDetail{
		ID:           "crew-acme-crew-alpha",
		OperatedByID: int64Ptr(7),
		Fields: map[string]string{
			spendBaselineField: "4.25", spendCloseField: "7.75",
			spendFirstBaselineField: "4.25", "total_cost_usd": "3.5",
			"project": "acme", "role": "crew",
		},
	}
	r.reconcileAtClose(context.Background(), second, "alpha")

	want := map[string]string{"operator_id": "7", "project": "acme", "model": "m"}
	if got := operatorSpendCounterValue(t, reg, want); got != 3.50 {
		t.Errorf("operator spend counter = %v, want 3.50 (one increment only)", got)
	}
}
