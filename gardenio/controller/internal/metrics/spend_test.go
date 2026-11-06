package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestSpendMetrics_RegisterAndRecord(t *testing.T) {
	sm := NewSpendMetrics()
	reg := prometheus.NewRegistry()
	if err := sm.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Two turns for the same agent accumulate into one series. Values chosen to
	// sum exactly in float64 (0.25 + 0.25 = 0.5) so the assertion is precise.
	if !sm.RecordSpend("agent-a", "acme", "crew", "claude-opus-4-8", "litellm-prod", 0.25) {
		t.Fatal("RecordSpend returned false for first turn")
	}
	if !sm.RecordSpend("agent-a", "acme", "crew", "claude-opus-4-8", "litellm-prod", 0.25) {
		t.Fatal("RecordSpend returned false for second turn")
	}
	// A distinct model is a distinct series.
	if !sm.RecordSpend("agent-a", "acme", "crew", "claude-sonnet-4-6", "litellm-prod", 0.02) {
		t.Fatal("RecordSpend returned false for distinct-model turn")
	}

	if got := testutil.CollectAndCount(sm.agentSpend); got != 2 {
		t.Fatalf("expected 2 spend series, got %d", got)
	}

	want := `
# HELP gardenio_agent_spend_usd_total Cumulative agent spend in USD, incremented once per session at close from the LiteLLM-reconciled session cost (reconcile_session_spend.go). Labels {agent,project,role,model,account}. Monotonic counter — query per-hour spend with increase(gardenio_agent_spend_usd_total[1h]).
# TYPE gardenio_agent_spend_usd_total counter
gardenio_agent_spend_usd_total{account="litellm-prod",agent="agent-a",model="claude-opus-4-8",project="acme",role="crew"} 0.5
gardenio_agent_spend_usd_total{account="litellm-prod",agent="agent-a",model="claude-sonnet-4-6",project="acme",role="crew"} 0.02
`
	if err := testutil.CollectAndCompare(sm.agentSpend, strings.NewReader(want)); err != nil {
		t.Fatalf("unexpected metric output: %v", err)
	}
}

func TestSpendMetrics_DropsNonPositiveCost(t *testing.T) {
	sm := NewSpendMetrics()

	// Zero, negative, and missing-cost sessions must not advance the counter —
	// a $0 (or absent-LiteLLM-spend) session is never silently counted as spend.
	if sm.RecordSpend("agent-a", "acme", "crew", "m", "acct", 0) {
		t.Fatal("RecordSpend should drop a $0 turn")
	}
	if sm.RecordSpend("agent-a", "acme", "crew", "m", "acct", -1.5) {
		t.Fatal("RecordSpend should drop a negative cost")
	}

	if got := testutil.CollectAndCount(sm.agentSpend); got != 0 {
		t.Fatalf("expected no series after non-positive costs, got %d", got)
	}
}

func TestSpendMetrics_NilReceiver(t *testing.T) {
	var sm *SpendMetrics
	if sm.RecordSpend("a", "p", "r", "m", "acct", 1.0) {
		t.Fatal("RecordSpend on nil receiver should return false")
	}
	if sm.RecordOperatorSpend("42", "p", "m", 1.0) {
		t.Fatal("RecordOperatorSpend on nil receiver should return false")
	}
}

func TestSpendMetrics_RecordOperatorSpend(t *testing.T) {
	sm := NewSpendMetrics()
	reg := prometheus.NewRegistry()
	if err := sm.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Two sessions for the same operator accumulate into one series.
	if !sm.RecordOperatorSpend("42", "acme", "claude-opus-4-8", 0.25) {
		t.Fatal("RecordOperatorSpend returned false for first session")
	}
	if !sm.RecordOperatorSpend("42", "acme", "claude-opus-4-8", 0.25) {
		t.Fatal("RecordOperatorSpend returned false for second session")
	}
	// The unattributed "0" bucket is a distinct series, not dropped.
	if !sm.RecordOperatorSpend("0", "acme", "claude-opus-4-8", 0.10) {
		t.Fatal("RecordOperatorSpend returned false for unattributed session")
	}
	// Non-positive cost is dropped, same contract as RecordSpend.
	if sm.RecordOperatorSpend("42", "acme", "claude-opus-4-8", 0) {
		t.Fatal("RecordOperatorSpend should drop a $0 session")
	}

	if got := testutil.CollectAndCount(sm.operatorSpend); got != 2 {
		t.Fatalf("expected 2 operator-spend series, got %d", got)
	}

	want := `
# HELP gardenio_operator_cost_usd_total Cumulative human-operator spend in USD, incremented once per session at close from the LiteLLM-reconciled session cost (reconcile_session_spend.go). Labels {operator_id,project,model}; operator_id is the opaque operators(id) FK ("0" = unattributed). Monotonic counter superseding the former non-authoritative gb session-push estimate (ka-0000000999).
# TYPE gardenio_operator_cost_usd_total counter
gardenio_operator_cost_usd_total{model="claude-opus-4-8",operator_id="0",project="acme"} 0.1
gardenio_operator_cost_usd_total{model="claude-opus-4-8",operator_id="42",project="acme"} 0.5
`
	if err := testutil.CollectAndCompare(sm.operatorSpend, strings.NewReader(want)); err != nil {
		t.Fatalf("unexpected metric output: %v", err)
	}
}

func TestSpendMetrics_SetTagCosts(t *testing.T) {
	sm := NewSpendMetrics()
	reg := prometheus.NewRegistry()
	if err := sm.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// A complete snapshot across the three attribution dims. Non-positive and
	// blank cells are dropped so they never create noise series.
	sm.SetTagCosts([]TagCost{
		{Dim: "role", Value: "gardener", USD: 12.50},
		{Dim: "role", Value: "thread", USD: 3.25},
		{Dim: "project", Value: "monorepo", USD: 9.00},
		{Dim: "model", Value: "claude-opus-4-8", USD: 15.75},
		{Dim: "role", Value: "free", USD: 0},  // dropped: non-positive
		{Dim: "project", Value: "", USD: 1.0}, // dropped: blank value
		{Dim: "", Value: "x", USD: 1.0},       // dropped: blank dim
	}, "prod")

	if got := testutil.CollectAndCount(sm.tagCost); got != 4 {
		t.Fatalf("expected 4 tag-cost series, got %d", got)
	}

	want := `
# HELP gardenio_tag_cost_usd Real-bill spend in USD broken down by a single attribution dimension (dim ∈ {role,project,model}), sourced from LiteLLM GET /spend/tags across all proxies (tag_spend_sync.go). Lifetime-cumulative per (dim,value) — query windowed spend with increase(gardenio_tag_cost_usd{dim="role"}[1h]). The real-bill, dimensional sibling of gardenio_account_cost_usd; accurate where gardenio_agent_spend_usd_total (at-close counter) reads low (ka-0000000478).
# TYPE gardenio_tag_cost_usd gauge
gardenio_tag_cost_usd{dim="model",env="prod",value="claude-opus-4-8"} 15.75
gardenio_tag_cost_usd{dim="project",env="prod",value="monorepo"} 9
gardenio_tag_cost_usd{dim="role",env="prod",value="gardener"} 12.5
gardenio_tag_cost_usd{dim="role",env="prod",value="thread"} 3.25
`
	if err := testutil.CollectAndCompare(sm.tagCost, strings.NewReader(want)); err != nil {
		t.Fatalf("unexpected tag-cost metric output: %v", err)
	}

	// A subsequent snapshot REPLACES the gauge (Reset+Set): a (dim,value) that
	// dropped out of /spend/tags must not linger, and a grown total overwrites.
	sm.SetTagCosts([]TagCost{
		{Dim: "role", Value: "gardener", USD: 20.00}, // grew
		// thread/monorepo/opus dropped from this snapshot
	}, "prod")
	if got := testutil.CollectAndCount(sm.tagCost); got != 1 {
		t.Fatalf("expected 1 tag-cost series after replacement, got %d", got)
	}
	if got := testutil.ToFloat64(sm.tagCost.WithLabelValues("role", "gardener", "prod")); got != 20.00 {
		t.Fatalf("expected gardener=20.00 after replacement, got %v", got)
	}

	// Nil receiver is a no-op (never panics).
	var nilSM *SpendMetrics
	nilSM.SetTagCosts([]TagCost{{Dim: "role", Value: "x", USD: 1}}, "prod")
}

func TestSpendMetrics_SetAttributionSplit(t *testing.T) {
	sm := NewSpendMetrics()
	reg := prometheus.NewRegistry()
	if err := sm.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}

	sm.SetAttributionSplit(135.0, 15.0, "prod")

	want := `
# HELP gardenio_agent_attributed_cost_usd Lifetime-cumulative LiteLLM spend carrying an agent: request tag, summed across all proxies (tag_spend_sync.go, ka-0000003988). The portion of the real bill the per-agent settlement pipeline can attribute. attributed + unattributed ≡ the full bill by construction. Only increase()/delta() windows are meaningful.
# TYPE gardenio_agent_attributed_cost_usd gauge
gardenio_agent_attributed_cost_usd{env="prod"} 135
`
	if err := testutil.CollectAndCompare(sm.attributedCost, strings.NewReader(want)); err != nil {
		t.Fatalf("unexpected attributed metric output: %v", err)
	}
	want = `
# HELP gardenio_unattributed_cost_usd Lifetime-cumulative LiteLLM spend carrying NO agent: request tag (per-proxy /global/spend bill minus the agent:* tag totals), summed across all proxies (tag_spend_sync.go, ka-0000003988). The residual no per-agent settlement can ever book — untagged consumers and tag-loss bugs land here instead of vanishing from the books. Windowed residual: increase(gardenio_unattributed_cost_usd[1h]); armed by the UnattributedSpendHigh gap-alert.
# TYPE gardenio_unattributed_cost_usd gauge
gardenio_unattributed_cost_usd{env="prod"} 15
`
	if err := testutil.CollectAndCompare(sm.unattributedCost, strings.NewReader(want)); err != nil {
		t.Fatalf("unexpected unattributed metric output: %v", err)
	}

	// A later tick overwrites (gauge semantics); negative inputs clamp to 0.
	sm.SetAttributionSplit(200.0, -3.0, "prod")
	if got := testutil.ToFloat64(sm.attributedCost.WithLabelValues("prod")); got != 200.0 {
		t.Fatalf("expected attributed=200 after overwrite, got %v", got)
	}
	if got := testutil.ToFloat64(sm.unattributedCost.WithLabelValues("prod")); got != 0.0 {
		t.Fatalf("expected unattributed=0 after negative clamp, got %v", got)
	}

	// Nil receiver is a no-op (never panics).
	var nilSM *SpendMetrics
	nilSM.SetAttributionSplit(1, 1, "prod")
}

func TestSpendMetrics_SetSpendLogsCosts(t *testing.T) {
	sm := NewSpendMetrics()
	reg := prometheus.NewRegistry()
	if err := sm.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// A complete snapshot of the tracked population. Non-positive figures and
	// blank agent names are dropped — nothing to attribute yet.
	sm.SetSpendLogsCosts([]SpendLogsCost{
		{Agent: "agent-a", Project: "acme", Role: "crew", Model: "claude-opus-4-8", Account: "litellm-prod", USD: 2.5},
		{Agent: "agent-b", Project: "acme", Role: "gardener", Model: "claude-opus-4-8", Account: "litellm-prod", USD: 7.0},
		{Agent: "agent-idle", Project: "acme", Role: "crew", USD: 0}, // dropped: non-positive
		{Agent: "", Project: "acme", Role: "crew", USD: 1.0},         // dropped: blank agent
	})

	want := `
# HELP gardenio_agent_spend_logs_usd Per-agent lifetime spend in USD recomputed statelessly from LiteLLM /spend/logs rows by the periodic spend-logs sweep (spendlogs_sweep.go) — the NEW attribution path replacing the baseline/watermark counter gardenio_agent_spend_usd_total (plan ka-0000003747 C3). Labels {agent,project,role,model,account} mirror the old counter. Only agents whose bead was created inside the sweep lookback carry a series (full-life coverage); absent = not derived, never a partial sum.
# TYPE gardenio_agent_spend_logs_usd gauge
gardenio_agent_spend_logs_usd{account="litellm-prod",agent="agent-a",model="claude-opus-4-8",project="acme",role="crew"} 2.5
gardenio_agent_spend_logs_usd{account="litellm-prod",agent="agent-b",model="claude-opus-4-8",project="acme",role="gardener"} 7
`
	if err := testutil.CollectAndCompare(sm.spendLogsCost, strings.NewReader(want)); err != nil {
		t.Fatalf("unexpected spend-logs metric output: %v", err)
	}

	// A later snapshot REPLACES the gauge (Reset+Set): an agent that aged out
	// of the sweep lookback must retire its series, and a grown lifetime
	// figure overwrites.
	sm.SetSpendLogsCosts([]SpendLogsCost{
		{Agent: "agent-a", Project: "acme", Role: "crew", Model: "claude-opus-4-8", Account: "litellm-prod", USD: 3.75},
	})
	if got := testutil.CollectAndCount(sm.spendLogsCost); got != 1 {
		t.Fatalf("expected 1 series after replacement snapshot, got %d", got)
	}
	if got := testutil.ToFloat64(sm.spendLogsCost.WithLabelValues("agent-a", "acme", "crew", "claude-opus-4-8", "litellm-prod")); got != 3.75 {
		t.Fatalf("expected agent-a=3.75 after overwrite, got %v", got)
	}

	// Nil receiver is a no-op (never panics), matching the other setters.
	var nilSM *SpendMetrics
	nilSM.SetSpendLogsCosts([]SpendLogsCost{{Agent: "x", USD: 1}})
	if nilSM.SpendLogsCostGauge() != nil {
		t.Fatal("nil receiver gauge accessor must return nil")
	}
}
