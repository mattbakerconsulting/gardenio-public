package metrics

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"gardenio/controller/internal/beadsapi"
)

// mockQuerier implements BeadQuerier for tests.
type mockQuerier struct {
	agents   []beadsapi.AgentBead
	agentErr error
	beads    []*beadsapi.BeadDetail
	// agentBeadDetails answers the collector's Types:["agent"] window query used
	// to derive the fleet archetype grid (advice_catalog.go). Left nil, the
	// archetype derivation sees no agents and publishes no per-archetype series;
	// set it to the (project, role)-carrying agent beads a test wants audited.
	agentBeadDetails []*beadsapi.BeadDetail
	beadErr          error
	projects         map[string]beadsapi.ProjectInfo
	projectErr       error
	// beadTotal, when > 0, overrides the Total reported for the non-agent
	// (account) query so a test can simulate the daemon truncating a larger
	// population down to len(beads) — the silent-truncation failure mode the
	// collector's Total-vs-len guard must catch (ka-0000005987).
	beadTotal int
	// lastAccountQuery records the ListBeadsQuery the collector issued for the
	// account (non-agent) listing, so a test can assert the status filter and
	// explicit Limit are set.
	lastAccountQuery beadsapi.ListBeadsQuery
}

func (m *mockQuerier) ListAgentBeads(_ context.Context) ([]beadsapi.AgentBead, error) {
	return m.agents, m.agentErr
}

func (m *mockQuerier) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if m.beadErr != nil {
		return nil, m.beadErr
	}
	// Route the agent-bead window query (fleet archetype derivation) to the
	// dedicated fixture so it stays independent of the advice corpus.
	if slices.Contains(q.Types, "agent") {
		return &beadsapi.ListBeadsResult{Beads: m.agentBeadDetails, Total: len(m.agentBeadDetails)}, nil
	}
	m.lastAccountQuery = q
	total := len(m.beads)
	if m.beadTotal > 0 {
		total = m.beadTotal
	}
	return &beadsapi.ListBeadsResult{Beads: m.beads, Total: total}, nil
}

// ListAllBeadsFiltered is the paged fetch the advice-catalog gauges use. The
// advice corpus fits in one page, so the mock returns m.beads directly (routing
// agent-type queries to the archetype fixture, same as ListBeadsFiltered).
func (m *mockQuerier) ListAllBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if m.beadErr != nil {
		return nil, m.beadErr
	}
	if slices.Contains(q.Types, "agent") {
		return &beadsapi.ListBeadsResult{Beads: m.agentBeadDetails, Total: len(m.agentBeadDetails)}, nil
	}
	total := len(m.beads)
	if m.beadTotal > 0 {
		total = m.beadTotal
	}
	return &beadsapi.ListBeadsResult{Beads: m.beads, Total: total}, nil
}

func (m *mockQuerier) ListProjectBeads(_ context.Context) (map[string]beadsapi.ProjectInfo, error) {
	if m.projectErr != nil {
		return nil, m.projectErr
	}
	return m.projects, nil
}

func TestNew(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	c := New(&mockQuerier{}, logger, 30*time.Second)
	if c == nil {
		t.Fatal("New returned nil")
	}
}

func TestRegister(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	c := New(&mockQuerier{}, logger, 30*time.Second)
	reg := prometheus.NewRegistry()
	if err := c.Register(reg); err != nil {
		t.Fatalf("Register failed: %v", err)
	}
}

func TestRecordReconcile_IncrementsCounter(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	c := New(&mockQuerier{}, logger, 30*time.Second)

	// Two successes and one failure for the "agent" controller.
	c.RecordReconcile("agent", true)
	c.RecordReconcile("agent", true)
	c.RecordReconcile("agent", false)

	want := `
# HELP gardenio_reconcile_total Total controller reconcile-loop completions by controller name and result (ok|error). Direct measurement source for prod SLO S2 (reconcile success rate).
# TYPE gardenio_reconcile_total counter
gardenio_reconcile_total{controller="agent",result="ok"} 2
gardenio_reconcile_total{controller="agent",result="error"} 1
`
	if err := testutil.CollectAndCompare(c.reconcileTotal, strings.NewReader(want), "gardenio_reconcile_total"); err != nil {
		t.Fatalf("unexpected reconcile_total series: %v", err)
	}
}

func TestCollectAgents_PopulatesGauges(t *testing.T) {
	q := &mockQuerier{
		agents: []beadsapi.AgentBead{
			{
				ID:         "agent-1",
				Project:    "gardenio",
				Role:       "crew",
				AgentName:  "worker-1",
				AgentState: "working",
				Metadata: map[string]string{
					"total_cost_usd":           "12.345678",
					"total_input_tokens":       "5000",
					"total_output_tokens":      "3000",
					"total_cache_read_tokens":  "1000",
					"total_cache_write_tokens": "500",
					"total_request_count":      "42",
					"session_count":            "3",
					"total_tool_calls":         "100",
					"total_errors":             "2",
					"total_compactions":        "1",
				},
			},
			{
				ID:         "agent-2",
				Project:    "gardenio",
				Role:       "gardener",
				AgentName:  "cap-1",
				AgentState: "working",
				Metadata:   map[string]string{},
			},
		},
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	c := New(q, logger, 30*time.Second)
	reg := prometheus.NewRegistry()
	if err := c.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}

	c.collectAgents(context.Background())

	// Check agents_active gauge.
	assertGaugeValue(t, c.agentsActive, prometheus.Labels{
		"project": "gardenio", "role": "crew", "state": "working",
	}, 1)
	assertGaugeValue(t, c.agentsActive, prometheus.Labels{
		"project": "gardenio", "role": "gardener", "state": "working",
	}, 1)

	// Check agent cost gauge.
	assertGaugeValue(t, c.agentCostUSD, prometheus.Labels{
		"agent": "worker-1", "project": "gardenio", "role": "crew",
	}, 12.345678)

	// Check token gauges.
	assertGaugeValue(t, c.agentInputTokens, prometheus.Labels{
		"agent": "worker-1", "project": "gardenio", "role": "crew",
	}, 5000)
	assertGaugeValue(t, c.agentOutputTokens, prometheus.Labels{
		"agent": "worker-1", "project": "gardenio", "role": "crew",
	}, 3000)

	// Check missing metadata defaults to zero.
	assertGaugeValue(t, c.agentCostUSD, prometheus.Labels{
		"agent": "cap-1", "project": "gardenio", "role": "gardener",
	}, 0)
}

func TestCollectAgents_AgentAge(t *testing.T) {
	createdAt := time.Now().Add(-2 * time.Hour)
	q := &mockQuerier{
		agents: []beadsapi.AgentBead{
			{
				ID:         "agent-1",
				Project:    "gardenio",
				Role:       "crew",
				AgentName:  "worker-1",
				AgentState: "working",
				CreatedAt:  createdAt,
				Metadata:   map[string]string{},
			},
			{
				// No CreatedAt (zero value) — must not report a bogus ~now()
				// age; the gauge stays unset for this series.
				ID:         "agent-2",
				Project:    "gardenio",
				Role:       "gardener",
				AgentName:  "cap-1",
				AgentState: "working",
				Metadata:   map[string]string{},
			},
		},
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	c := New(q, logger, 30*time.Second)
	reg := prometheus.NewRegistry()
	if err := c.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}

	c.collectAgents(context.Background())

	gauge, err := c.agentAge.GetMetricWith(prometheus.Labels{
		"agent": "worker-1", "project": "gardenio", "role": "crew",
	})
	if err != nil {
		t.Fatalf("GetMetricWith: %v", err)
	}
	var m dto.Metric
	if err := gauge.Write(&m); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := m.GetGauge().GetValue()
	if got < 7190 || got > 7210 {
		t.Errorf("agentAge = %v, want ~7200 (2h)", got)
	}
}

func TestCollectAgents_PrewarmedPoolDepth(t *testing.T) {
	q := &mockQuerier{
		agents: []beadsapi.AgentBead{
			// Two ready MicroVM standby members (prewarmed, no thread/task).
			{
				ID: "gd-pw-1", Project: "gardenio", Role: "thread",
				AgentName: "prewarmed-1-1", AgentState: "prewarmed",
				Metadata: map[string]string{"execution_target": "lambda_microvm"},
			},
			{
				ID: "gd-pw-2", Project: "gardenio", Role: "thread",
				AgentName: "prewarmed-1-2", AgentState: "prewarmed",
				Metadata: map[string]string{"execution_target": "lambda_microvm"},
			},
			// One parked (idle) MicroVM member — counts under state=idle.
			{
				ID: "gd-pw-3", Project: "gardenio", Role: "thread",
				AgentName: "prewarmed-1-3", AgentState: "idle",
				Metadata: map[string]string{"execution_target": "lambda_microvm"},
			},
			// A K8s prewarmed member (unstamped target -> "kubernetes").
			{
				ID: "gd-pw-4", Project: "town", Role: "crew",
				AgentName: "prewarmed-2-1", AgentState: "prewarmed",
				Metadata: map[string]string{},
			},
			// An ASSIGNED former pool member: prewarmed- prefix but now carries a
			// thread and a working state -> NOT counted (left the pool).
			{
				ID: "gd-pw-5", Project: "gardenio", Role: "thread",
				AgentName: "prewarmed-1-4", AgentState: "working",
				Metadata: map[string]string{
					"execution_target": "lambda_microvm",
					"slack_thread_ts":  "1785372919.000100",
				},
			},
			// A normal (non-pool) agent: no prewarmed- prefix -> NOT counted.
			{
				ID: "gd-a-1", Project: "gardenio", Role: "crew",
				AgentName: "worker-1", AgentState: "prewarmed",
				Metadata: map[string]string{},
			},
		},
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	c := New(q, logger, 30*time.Second)
	c.defaultEnv = "e2e"
	reg := prometheus.NewRegistry()
	if err := c.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}

	c.collectAgents(context.Background())

	// Two ready MicroVM standby members.
	assertGaugeValue(t, c.prewarmedPoolDepth, prometheus.Labels{
		"project": "gardenio", "role": "thread",
		"execution_target": "lambda_microvm", "state": "prewarmed", "env": "e2e",
	}, 2)
	// One parked idle MicroVM member.
	assertGaugeValue(t, c.prewarmedPoolDepth, prometheus.Labels{
		"project": "gardenio", "role": "thread",
		"execution_target": "lambda_microvm", "state": "idle", "env": "e2e",
	}, 1)
	// One K8s member (unstamped target collapses to kubernetes).
	assertGaugeValue(t, c.prewarmedPoolDepth, prometheus.Labels{
		"project": "town", "role": "crew",
		"execution_target": "kubernetes", "state": "prewarmed", "env": "e2e",
	}, 1)

	// The assigned former member and the non-pool agent produce no depth series.
	if got := countSeries(t, reg, "gardenio_prewarmed_pool_depth"); got != 3 {
		t.Errorf("prewarmed_pool_depth series = %d, want 3 (assigned + non-pool excluded)", got)
	}
}

// countSeries returns how many series a gauge/counter family currently exposes.
func countSeries(t *testing.T, reg *prometheus.Registry, name string) int {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name {
			return len(mf.GetMetric())
		}
	}
	return 0
}

func TestCollectAgents_EmptyState(t *testing.T) {
	q := &mockQuerier{
		agents: []beadsapi.AgentBead{
			{
				ID:         "agent-1",
				Project:    "test",
				Role:       "crew",
				AgentName:  "a1",
				AgentState: "",
				Metadata:   map[string]string{},
			},
		},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	c := New(q, logger, 30*time.Second)
	c.collectAgents(context.Background())

	assertGaugeValue(t, c.agentsActive, prometheus.Labels{
		"project": "test", "role": "crew", "state": "unknown",
	}, 1)
}

func TestCollectAgents_Error(t *testing.T) {
	q := &mockQuerier{agentErr: fmt.Errorf("connection refused")}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	c := New(q, logger, 30*time.Second)
	c.collectAgents(context.Background())

	var m dto.Metric
	if err := c.scrapeErrors.Write(&m); err != nil {
		t.Fatalf("reading scrapeErrors: %v", err)
	}
	if m.GetCounter().GetValue() != 1 {
		t.Errorf("expected scrapeErrors=1, got %v", m.GetCounter().GetValue())
	}
}

func TestCollectAccounts_PopulatesGauges(t *testing.T) {
	q := &mockQuerier{
		beads: []*beadsapi.BeadDetail{
			{
				Title: "anthropic-prod",
				Fields: map[string]string{
					"total_cost_usd":           "150.50",
					"session_count":            "25",
					"total_input_tokens":       "500000",
					"total_output_tokens":      "200000",
					"total_cache_read_tokens":  "100000",
					"total_cache_write_tokens": "50000",
					"total_request_count":      "1200",
					"utilization_5h":           "0.7500",
					"utilization_7d":           "0.4200",
					"throttle_status":          "allowed",
					"soft_failover_state":      "normal",
				},
			},
			{
				Title: "openai-dev",
				Fields: map[string]string{
					"total_cost_usd": "42.00",
					"session_count":  "10",
				},
			},
		},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	c := New(q, logger, 30*time.Second)
	c.collectAccounts(context.Background())

	// No GARDENIO_ENV set and no per-account env field → env="unknown".
	acct := prometheus.Labels{"account": "anthropic-prod", "env": "unknown"}
	assertGaugeValue(t, c.accountCostUSD, acct, 150.50)
	assertGaugeValue(t, c.accountSessions, acct, 25)
	assertGaugeValue(t, c.accountInputTokens, acct, 500000)
	assertGaugeValue(t, c.accountOutputTokens, acct, 200000)
	assertGaugeValue(t, c.accountCacheRead, acct, 100000)
	assertGaugeValue(t, c.accountCacheWrite, acct, 50000)
	assertGaugeValue(t, c.accountRequests, acct, 1200)
	assertGaugeValue(t, c.accountUtilization5h, acct, 0.75)
	assertGaugeValue(t, c.accountUtilization7d, acct, 0.42)
	assertGaugeValue(t, c.accountThrottled, acct, 0) // "allowed" → 0
	assertGaugeValue(t, c.accountWarming, acct, 0)   // "normal" → 0

	assertGaugeValue(t, c.accountCostUSD, prometheus.Labels{"account": "openai-dev", "env": "unknown"}, 42.00)
	// Missing fields default to zero.
	assertGaugeValue(t, c.accountUtilization5h, prometheus.Labels{"account": "openai-dev", "env": "unknown"}, 0)
}

func TestCollectAccounts_ThrottledAndWarming(t *testing.T) {
	q := &mockQuerier{
		beads: []*beadsapi.BeadDetail{
			{
				Title: "throttled-acct",
				Fields: map[string]string{
					"throttle_status":     "rejected",
					"soft_failover_state": "warming",
					"utilization_5h":      "0.9500",
					"utilization_7d":      "0.6000",
				},
			},
		},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	c := New(q, logger, 30*time.Second)
	c.collectAccounts(context.Background())

	acct := prometheus.Labels{"account": "throttled-acct", "env": "unknown"}
	assertGaugeValue(t, c.accountThrottled, acct, 1) // "rejected" → 1
	assertGaugeValue(t, c.accountWarming, acct, 1)   // "warming" → 1
	assertGaugeValue(t, c.accountUtilization5h, acct, 0.95)
	assertGaugeValue(t, c.accountUtilization7d, acct, 0.60)
}

// TestCollectAccounts_EnvLabel verifies the `env` label resolution
// precedence: a per-account `env` field wins; otherwise GARDENIO_ENV (the
// controller's deployment env) is used. The label keeps the same account
// name from aliasing across prod / gardenio-next / e2e (E2.2).
func TestCollectAccounts_EnvLabel(t *testing.T) {
	t.Setenv("GARDENIO_ENV", "prod")
	q := &mockQuerier{
		beads: []*beadsapi.BeadDetail{
			{
				Title:  "shared-acct",
				Fields: map[string]string{"total_cost_usd": "10"},
			},
			{
				Title:  "next-acct",
				Fields: map[string]string{"total_cost_usd": "20", "env": "gardenio-next"},
			},
		},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	c := New(q, logger, 30*time.Second)
	c.collectAccounts(context.Background())

	// No per-account env field → falls back to GARDENIO_ENV.
	assertGaugeValue(t, c.accountCostUSD,
		prometheus.Labels{"account": "shared-acct", "env": "prod"}, 10)
	// Explicit per-account env field overrides the deployment default.
	assertGaugeValue(t, c.accountCostUSD,
		prometheus.Labels{"account": "next-acct", "env": "gardenio-next"}, 20)
}

// TestCollectAccounts_SharedKeyDedupesCost verifies that when two distinct
// account beads intentionally share one litellm_key, the per-key /key/info cost
// (which spendSync stamps onto BOTH beads) is booked exactly once in
// sum(max by(account,env)(gardenio_account_cost_usd)). The lexicographically-first
// account title owns the cost; siblings on the same key emit a 0 cost series so
// their {account} label survives for joins but does not double-count the bill.
// Per-account counters (sessions/tokens/requests) stay per-bead — they are
// genuinely distinct (ka-0000001340).
func TestCollectAccounts_SharedKeyDedupesCost(t *testing.T) {
	q := &mockQuerier{
		beads: []*beadsapi.BeadDetail{
			{
				// fsm-triage account, older; same key as glm-bedrock.
				Title: "litellm-bedrock",
				Fields: map[string]string{
					"litellm_key":    "sk-shared-pX0heA1A",
					"total_cost_usd": "761.50",
					"session_count":  "374",
				},
			},
			{
				// glm-lab account, newer; SAME shared key, SAME per-key cost.
				Title: "glm-bedrock",
				Fields: map[string]string{
					"litellm_key":    "sk-shared-pX0heA1A",
					"total_cost_usd": "761.50",
					"session_count":  "17",
				},
			},
			{
				// Distinct key — unaffected by dedup.
				Title: "openai-dev",
				Fields: map[string]string{
					"litellm_key":    "sk-openai-uniq",
					"total_cost_usd": "42.00",
					"session_count":  "10",
				},
			},
		},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	c := New(q, logger, 30*time.Second)
	c.collectAccounts(context.Background())

	glm := prometheus.Labels{"account": "glm-bedrock", "env": "unknown"}
	litellm := prometheus.Labels{"account": "litellm-bedrock", "env": "unknown"}
	openai := prometheus.Labels{"account": "openai-dev", "env": "unknown"}

	// "glm-bedrock" < "litellm-bedrock" lexically → glm-bedrock owns the cost.
	assertGaugeValue(t, c.accountCostUSD, glm, 761.50)
	// The sibling on the same key emits a 0 cost series (no double-count).
	assertGaugeValue(t, c.accountCostUSD, litellm, 0)
	// Distinct-key account keeps its own cost.
	assertGaugeValue(t, c.accountCostUSD, openai, 42.00)

	// sum(max by(account,env)) counts the shared bill once: 761.50 + 42.00.
	sum := testutil.ToFloat64(c.accountCostUSD.With(glm)) +
		testutil.ToFloat64(c.accountCostUSD.With(litellm)) +
		testutil.ToFloat64(c.accountCostUSD.With(openai))
	if sum != 803.50 {
		t.Errorf("deduped account-cost sum = %v, want 803.50 (761.50 once + 42.00)", sum)
	}

	// Session counts remain per-account — they are genuinely distinct.
	assertGaugeValue(t, c.accountSessions, glm, 17)
	assertGaugeValue(t, c.accountSessions, litellm, 374)
}

// TestCollectAccounts_SharedKeyDistinctEnvNotDeduped verifies the dedup scope is
// (litellm_key, env): the same key in two different envs is NOT a shared bill —
// each env's controller books its own /key/info total, so both keep their cost.
func TestCollectAccounts_SharedKeyDistinctEnvNotDeduped(t *testing.T) {
	q := &mockQuerier{
		beads: []*beadsapi.BeadDetail{
			{
				Title:  "acct-prod",
				Fields: map[string]string{"litellm_key": "sk-k", "total_cost_usd": "100", "env": "prod"},
			},
			{
				Title:  "acct-next",
				Fields: map[string]string{"litellm_key": "sk-k", "total_cost_usd": "200", "env": "gardenio-next"},
			},
		},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	c := New(q, logger, 30*time.Second)
	c.collectAccounts(context.Background())

	assertGaugeValue(t, c.accountCostUSD, prometheus.Labels{"account": "acct-prod", "env": "prod"}, 100)
	assertGaugeValue(t, c.accountCostUSD, prometheus.Labels{"account": "acct-next", "env": "gardenio-next"}, 200)
}

func TestCollectAccounts_SkipsEmptyTitle(t *testing.T) {
	q := &mockQuerier{
		beads: []*beadsapi.BeadDetail{
			{Title: "", Fields: map[string]string{"total_cost_usd": "100"}},
		},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	c := New(q, logger, 30*time.Second)
	c.collectAccounts(context.Background())

	// No metrics should be emitted for unnamed accounts.
	mfs, _ := prometheus.DefaultGatherer.Gather()
	for _, mf := range mfs {
		if mf.GetName() == "gardenio_account_cost_usd" {
			t.Error("expected no account_cost_usd metrics for empty title")
		}
	}
}

// TestCollectAccounts_SkipsE2EAccounts pins the producer-side layer of the
// ka-0000005167 fix: an E2E test account must NOT become a gauge series at all —
// this is what keeps the by-account rollup and Accounts dropdown clean once a
// series would otherwise outlive the closed bead for the retention window. A real
// account named with a legitimate leading "e2e"-ish string is still emitted (no
// false positives — acceptance criterion 4).
func TestCollectAccounts_SkipsE2EAccounts(t *testing.T) {
	q := &mockQuerier{
		beads: []*beadsapi.BeadDetail{
			// E2E harness accounts across every naming convention — must be skipped.
			{Title: "e2e-acct-primary-abc", Labels: []string{"project:e2e-test-abc"}, CreatedBy: "e2e-test",
				Fields: map[string]string{"total_cost_usd": "99"}},
			{Title: "e2e-account-TAG", Labels: []string{"project:gardenio"}, CreatedBy: "e2e-test",
				Fields: map[string]string{"total_cost_usd": "99"}},
			{Title: "probe-acct-999", Labels: []string{"e2e:claudeless"},
				Fields: map[string]string{"total_cost_usd": "99"}},
			{Title: "sneaky-acct", Labels: []string{"e2e:true"},
				Fields: map[string]string{"total_cost_usd": "99"}},
			{Title: "shard-acct", OperatedBy: "oidc:e2e-shard-2@example.com",
				Fields: map[string]string{"total_cost_usd": "99"}},
			// A real account whose name legitimately begins with "e2e" (no hyphen).
			{Title: "e2eflow-prod", Labels: []string{"project:gardenio"}, CreatedBy: "Matthew.Baker",
				Fields: map[string]string{"total_cost_usd": "42"}},
			// A plain real account — the control.
			{Title: "gardenio3", Labels: []string{"project:gardenio"}, CreatedBy: "Matthew.Baker",
				Fields: map[string]string{"total_cost_usd": "500"}},
		},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	c := New(q, logger, 30*time.Second)
	c.collectAccounts(context.Background())

	// Real accounts are present with their real bill.
	assertGaugeValue(t, c.accountCostUSD, prometheus.Labels{"account": "e2eflow-prod", "env": "unknown"}, 42)
	assertGaugeValue(t, c.accountCostUSD, prometheus.Labels{"account": "gardenio3", "env": "unknown"}, 500)

	// Exactly two series remain — every E2E account was skipped. (A per-account
	// GetMetricWith would create-on-read a zero series and mask a leak, so the
	// series COUNT is the authoritative assertion here.)
	if got := testutil.CollectAndCount(c.accountCostUSD); got != 2 {
		t.Errorf("expected 2 account_cost_usd series (real accounts only), got %d", got)
	}
	// The skip must apply to every per-account gauge family, not just cost.
	if got := testutil.CollectAndCount(c.accountUtilization5h); got != 2 {
		t.Errorf("expected 2 account_utilization_5h series, got %d", got)
	}
}

// TestCollectAccounts_E2EAccountNeverWinsCostOwnerElection pins the interaction
// between the E2E skip and the shared-litellm_key cost-owner dedup
// (ka-0000005167 x ka-0000001340). pickCostOwners elects the lexicographically-
// FIRST account title on a (litellm_key, env) as the one that books the shared
// per-key bill; every other bead on that key emits cost = 0.
//
// If an E2E account could win that election it would silently zero REAL spend: the
// e2e winner is skipped (emits nothing at all) and the real sibling sees
// costKeyOwner != its own name, so it emits 0 — the key's entire bill vanishes.
// "e2e-aaa-shared" sorts before "real-zzz-account", so this fixture reproduces the
// exact trap; the real account must still book the full $250.
func TestCollectAccounts_E2EAccountNeverWinsCostOwnerElection(t *testing.T) {
	const sharedKey = "sk-shared-e2e-trap"
	q := &mockQuerier{
		beads: []*beadsapi.BeadDetail{
			{
				// Sorts FIRST and would win the election if e2e beads were eligible.
				Title:  "e2e-aaa-shared",
				Labels: []string{"e2e:trap"},
				Fields: map[string]string{"litellm_key": sharedKey, "total_cost_usd": "250"},
			},
			{
				Title:  "real-zzz-account",
				Fields: map[string]string{"litellm_key": sharedKey, "total_cost_usd": "250"},
			},
		},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	c := New(q, logger, 30*time.Second)
	c.collectAccounts(context.Background())

	// The real account books the shared key's full bill — not 0.
	assertGaugeValue(t, c.accountCostUSD,
		prometheus.Labels{"account": "real-zzz-account", "env": "unknown"}, 250)
	// And it is the ONLY series (the e2e bead emitted nothing), so the shared bill
	// is still booked exactly once.
	if got := testutil.CollectAndCount(c.accountCostUSD); got != 1 {
		t.Errorf("expected exactly 1 account_cost_usd series, got %d", got)
	}
}

func TestCollectAccounts_Error(t *testing.T) {
	q := &mockQuerier{beadErr: fmt.Errorf("timeout")}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	c := New(q, logger, 30*time.Second)
	c.collectAccounts(context.Background())

	var m dto.Metric
	if err := c.scrapeErrors.Write(&m); err != nil {
		t.Fatalf("reading scrapeErrors: %v", err)
	}
	if m.GetCounter().GetValue() != 1 {
		t.Errorf("expected scrapeErrors=1, got %v", m.GetCounter().GetValue())
	}
}

// TestCollectAccounts_QueryIsBounded pins the fix for ka-0000005987: the
// account query must carry an explicit status filter (non-closed only) and an
// explicit Limit, so it can never silently ride the daemon's
// defaultListBeadsLimit and drop overflow accounts from the cost gauge.
func TestCollectAccounts_QueryIsBounded(t *testing.T) {
	q := &mockQuerier{
		beads: []*beadsapi.BeadDetail{
			{Title: "acct", Fields: map[string]string{"total_cost_usd": "1"}},
		},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	c := New(q, logger, 30*time.Second)
	c.collectAccounts(context.Background())

	if q.lastAccountQuery.Limit <= 0 {
		t.Errorf("account query must set an explicit Limit, got %d", q.lastAccountQuery.Limit)
	}
	if len(q.lastAccountQuery.Statuses) == 0 {
		t.Fatal("account query must set an explicit status filter (non-closed only)")
	}
	if slices.Contains(q.lastAccountQuery.Statuses, "closed") {
		t.Errorf("account query must exclude closed beads, got statuses %v", q.lastAccountQuery.Statuses)
	}
	// The status set must match the canonical active-status set the internal
	// account listers use, so metrics and credential sync agree on "active".
	if !slices.Equal(q.lastAccountQuery.Statuses, beadsapi.ActiveStatuses()) {
		t.Errorf("account query statuses %v != beadsapi.ActiveStatuses() %v",
			q.lastAccountQuery.Statuses, beadsapi.ActiveStatuses())
	}
}

// TestCollectAccounts_TruncationIsLoud pins that if the daemon ever reports a
// Total larger than the returned page (silent truncation), the collector logs
// it and bumps scrapeErrors rather than quietly dropping accounts from the
// gauge (ka-0000005987).
func TestCollectAccounts_TruncationIsLoud(t *testing.T) {
	q := &mockQuerier{
		beads: []*beadsapi.BeadDetail{
			{Title: "acct", Fields: map[string]string{"total_cost_usd": "1"}},
		},
		beadTotal: 5000, // daemon says there are far more than the page returned
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	c := New(q, logger, 30*time.Second)
	c.collectAccounts(context.Background())

	var m dto.Metric
	if err := c.scrapeErrors.Write(&m); err != nil {
		t.Fatalf("reading scrapeErrors: %v", err)
	}
	if m.GetCounter().GetValue() != 1 {
		t.Errorf("expected scrapeErrors=1 on truncation, got %v", m.GetCounter().GetValue())
	}
}

// TestCollectAccounts_NoTruncationNoError guards the boundary: when the page is
// complete (Total == len), scrapeErrors must stay at zero.
func TestCollectAccounts_NoTruncationNoError(t *testing.T) {
	q := &mockQuerier{
		beads: []*beadsapi.BeadDetail{
			{Title: "acct", Fields: map[string]string{"total_cost_usd": "1"}},
		},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	c := New(q, logger, 30*time.Second)
	c.collectAccounts(context.Background())

	var m dto.Metric
	if err := c.scrapeErrors.Write(&m); err != nil {
		t.Fatalf("reading scrapeErrors: %v", err)
	}
	if m.GetCounter().GetValue() != 0 {
		t.Errorf("expected scrapeErrors=0 on a complete page, got %v", m.GetCounter().GetValue())
	}
}

func TestCollect_SetsLastScrapeTime(t *testing.T) {
	q := &mockQuerier{}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	c := New(q, logger, 30*time.Second)

	before := float64(time.Now().Unix())
	c.collect(context.Background())

	var m dto.Metric
	if err := c.lastScrapeTime.Write(&m); err != nil {
		t.Fatalf("reading lastScrapeTime: %v", err)
	}
	got := m.GetGauge().GetValue()
	if got < before {
		t.Errorf("lastScrapeTime %v is before test start %v", got, before)
	}
}

func TestStartIdempotent(t *testing.T) {
	q := &mockQuerier{}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	c := New(q, logger, 1*time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately so Start returns quickly.

	c.Start(ctx)
	// Second call should be a no-op (not panic or block).
	c.Start(ctx)
}

func TestParseFloat(t *testing.T) {
	tests := []struct {
		in   string
		want float64
	}{
		{"12.5", 12.5},
		{"0", 0},
		{"", 0},
		{"invalid", 0},
		{"1000000", 1000000},
	}
	for _, tt := range tests {
		got := parseFloat(tt.in)
		if got != tt.want {
			t.Errorf("parseFloat(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestBoolToFloat(t *testing.T) {
	if got := boolToFloat(true); got != 1 {
		t.Errorf("boolToFloat(true) = %v, want 1", got)
	}
	if got := boolToFloat(false); got != 0 {
		t.Errorf("boolToFloat(false) = %v, want 0", got)
	}
}

func TestCollectAgents_ResetClearsOldMetrics(t *testing.T) {
	q := &mockQuerier{
		agents: []beadsapi.AgentBead{
			{ID: "a1", Project: "p", Role: "r", AgentName: "n1", AgentState: "working", Metadata: map[string]string{"total_cost_usd": "10"}},
		},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	c := New(q, logger, 30*time.Second)

	c.collectAgents(context.Background())
	assertGaugeValue(t, c.agentCostUSD, prometheus.Labels{"agent": "n1", "project": "p", "role": "r"}, 10)

	// Now agent disappears.
	q.agents = nil
	c.collectAgents(context.Background())

	// Old metric should be gone (gauge reset).
	gauge, err := c.agentCostUSD.GetMetricWith(prometheus.Labels{"agent": "n1", "project": "p", "role": "r"})
	if err != nil {
		return // GetMetricWith returns error when label set not found after reset — expected
	}
	var m dto.Metric
	if err := gauge.Write(&m); err == nil && m.GetGauge().GetValue() != 0 {
		t.Errorf("expected old metric to be 0 after reset, got %v", m.GetGauge().GetValue())
	}
}

func TestCollectResourceMapping_PopulatesGauge(t *testing.T) {
	q := &mockQuerier{
		projects: map[string]beadsapi.ProjectInfo{
			"acme": {
				Name: "acme",
				ResourceMapping: []beadsapi.ResourceMappingEntry{
					{Env: "prod", Cluster: "acme-ecs", Service: "acme-api", DBInstance: "acme-db"},
					// A project may exercise multiple services / DB instances.
					{Env: "prod", Cluster: "acme-ecs", Service: "acme-worker", DBInstance: "acme-db"},
					// Same project, different env.
					{Env: "staging", Cluster: "acme-ecs-stg", Service: "acme-api", DBInstance: "acme-db-stg"},
				},
			},
			// A project with no mapping declared emits nothing.
			"bare": {Name: "bare"},
		},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	c := New(q, logger, 30*time.Second)
	reg := prometheus.NewRegistry()
	if err := c.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}

	c.collectResourceMapping(context.Background())

	// Each declared tuple becomes a presence series with value 1, conforming
	// to the FSM correlation contract schema {project, env, cluster, service,
	// db_instance}.
	assertGaugeValue(t, c.resourceMapping, prometheus.Labels{
		"project": "acme", "env": "prod", "cluster": "acme-ecs",
		"service": "acme-api", "db_instance": "acme-db",
	}, 1)
	assertGaugeValue(t, c.resourceMapping, prometheus.Labels{
		"project": "acme", "env": "prod", "cluster": "acme-ecs",
		"service": "acme-worker", "db_instance": "acme-db",
	}, 1)
	assertGaugeValue(t, c.resourceMapping, prometheus.Labels{
		"project": "acme", "env": "staging", "cluster": "acme-ecs-stg",
		"service": "acme-api", "db_instance": "acme-db-stg",
	}, 1)

	// Exactly the three declared tuples — the bare project contributes none.
	if got := testutil.CollectAndCount(c.resourceMapping); got != 3 {
		t.Errorf("resource_mapping series count = %d, want 3", got)
	}
}

func TestCollectResourceMapping_SkipsEmptyEnv(t *testing.T) {
	q := &mockQuerier{
		projects: map[string]beadsapi.ProjectInfo{
			"acme": {
				Name: "acme",
				ResourceMapping: []beadsapi.ResourceMappingEntry{
					// env is a join key; a row without it cannot be correlated.
					{Env: "", Cluster: "acme-ecs", Service: "acme-api", DBInstance: "acme-db"},
					{Env: "prod", Cluster: "acme-ecs", Service: "acme-api", DBInstance: "acme-db"},
				},
			},
		},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	c := New(q, logger, 30*time.Second)
	c.collectResourceMapping(context.Background())

	if got := testutil.CollectAndCount(c.resourceMapping); got != 1 {
		t.Errorf("resource_mapping series count = %d, want 1 (empty-env row skipped)", got)
	}
}

func TestCollectResourceMapping_ResetClearsRetiredRows(t *testing.T) {
	q := &mockQuerier{
		projects: map[string]beadsapi.ProjectInfo{
			"acme": {
				Name: "acme",
				ResourceMapping: []beadsapi.ResourceMappingEntry{
					{Env: "prod", Cluster: "acme-ecs", Service: "acme-api", DBInstance: "acme-db"},
				},
			},
		},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	c := New(q, logger, 30*time.Second)

	c.collectResourceMapping(context.Background())
	if got := testutil.CollectAndCount(c.resourceMapping); got != 1 {
		t.Fatalf("first scrape series count = %d, want 1", got)
	}

	// Project removed from the fleet → its mapping series must disappear.
	q.projects = map[string]beadsapi.ProjectInfo{}
	c.collectResourceMapping(context.Background())
	if got := testutil.CollectAndCount(c.resourceMapping); got != 0 {
		t.Errorf("after retire series count = %d, want 0", got)
	}
}

func TestCollectResourceMapping_Error(t *testing.T) {
	q := &mockQuerier{projectErr: fmt.Errorf("connection refused")}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	c := New(q, logger, 30*time.Second)
	c.collectResourceMapping(context.Background())

	var m dto.Metric
	if err := c.scrapeErrors.Write(&m); err != nil {
		t.Fatalf("reading scrapeErrors: %v", err)
	}
	if m.GetCounter().GetValue() != 1 {
		t.Errorf("expected scrapeErrors=1, got %v", m.GetCounter().GetValue())
	}
}

// assertGaugeValue checks a GaugeVec metric has the expected value for given labels.
func assertGaugeValue(t *testing.T, vec *prometheus.GaugeVec, labels prometheus.Labels, expected float64) {
	t.Helper()
	gauge, err := vec.GetMetricWith(labels)
	if err != nil {
		t.Fatalf("GetMetricWith(%v): %v", labels, err)
	}
	var m dto.Metric
	if err := gauge.Write(&m); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := m.GetGauge().GetValue(); got != expected {
		t.Errorf("metric %v = %v, want %v", labels, got, expected)
	}
}
