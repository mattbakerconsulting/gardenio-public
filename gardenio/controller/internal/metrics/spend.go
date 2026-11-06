package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// SpendMetrics holds the per-agent spend counter added by E3 (timeseries
// platform plan §2.E3, bead ka-0000003262). It turns "spend per agent per hour"
// into a first-class monotonic counter rather than a polled gauge: the
// controller's session-close reconciler (cmd/controller/reconcile_session_spend.go)
// increments this counter exactly once per session by the LiteLLM-reconciled
// session cost (close /spend/tags total − spawn baseline). All values are
// LiteLLM-sourced — never tokens×rate.
//
// Per-hour spend is then increase(gardenio_agent_spend_usd_total[1h]); the 5-min
// spendSync against LiteLLM /key/info remains the account-level reconciliation
// backstop and the session-close reconcile is the per-session truth.
//
// History: a per-turn NATS spend forwarder (cmd/controller/spend_forwarder.go)
// once fed this counter from coop usage events, but coop never emits
// response_cost so it dropped 100% of events; it was removed in I1
// (ka-0000000717, epic ka-0000000758).
//
// The counter is NEVER Reset() — resetting breaks rate()/increase() (those
// functions read a drop as a counter reset and double-count). It accumulates for
// the controller process lifetime; restarts are handled by the standard
// counter-reset detection in PromQL.
type SpendMetrics struct {
	// agentSpend counts cumulative agent spend in USD. Label set
	// {agent,project,role,model,account} matches the catalog
	// (timeseries-metrics-catalog.md §gardenio_agent_spend_usd_total) and the
	// downstream chart catalog queries (kbeads charts_catalog.go,
	// beads-viewer/charts.go). model is the model id that served the turn;
	// account is the LiteLLM credential account the turn was billed to.
	agentSpend *prometheus.CounterVec

	// operatorSpend is the human-operator rollup of the same reconciled session
	// cost, keyed {operator_id,project,model} (NO agent/session label). It feeds
	// the kbeads engagement spend panel —
	// sum by(operator_id)(gardenio_operator_cost_usd_total) (charts_engagement.go).
	//
	// Why it lives here and not in the gb session push (ka-0000000999): the in-pod
	// push wrote usage.TotalCostUSD (the non-authoritative JSONL response_cost
	// estimate) via vmagent /import/prometheus — an absolute-value remote write.
	// Because that series carries no session_id, every session for a given
	// {operator_id,project,model} OVERWROTE the prior one instead of accumulating,
	// so the "counter" was last-write-wins AND low-biased (turns with no
	// response_cost contributed 0). Emitting it here as a real in-process
	// monotonic counter, incremented once per session from the LiteLLM-reconciled
	// session cost, makes it both authoritative and correctly cumulative.
	//
	// operator_id is the opaque operators(id) FK (an integer), never an email, so
	// — unlike the gb push — it needs no producer-side PII hashing. The "0"
	// sentinel marks an unattributed session (no resolvable human), matching the
	// kbeads unattributedOperatorID bucket.
	operatorSpend *prometheus.CounterVec

	// tagCost is the REAL-BILL per-dimension spend gauge (ka-0000000478). Unlike
	// agentSpend (a counter that only ticks at session close and reads multiples
	// LOW versus the actual LiteLLM bill), this gauge is the lifetime-cumulative
	// real bill broken down by a single attribution dimension. It is the
	// dimensional sibling of gardenio_account_cost_usd: where that gauge slices the
	// real bill by {account}, this one slices the SAME real bill by {dim, value}
	// for dim ∈ {role, project, model}.
	//
	// How the breakdown stays real-bill-accurate: every agent request is tagged in
	// LiteLLM with role:<role>, project:<project>, model:<model> (alongside the
	// existing agent:<name>) via x-litellm-tags (podspec.go). LiteLLM's GET
	// /spend/tags reports the FULL per-request cost under EVERY one of a request's
	// tags — verified live: a 4-tagged request books identical spend under all four
	// tags, no splitting — so sum(role:*) == sum(project:*) == sum(model:*) == the
	// real account bill. The tagSpendSync poller (cmd/controller/tag_spend_sync.go)
	// reads /spend/tags from every LiteLLM proxy, sums each (dim,value) across
	// proxies, and Sets this gauge. Windowed spend is therefore
	// increase(gardenio_tag_cost_usd{dim="role"}[1h]) — the same shape the
	// account-cost gauge uses, reconciling with the /costs headline fleet rate.
	//
	// Cardinality is bounded: role/project/model each have O(10) distinct values,
	// so the whole gauge is O(35) series. agent: is deliberately NOT a dimension
	// here — it has thousands of distinct values (catalog §3.5), and by-agent spend
	// stays on agentSpend (which also carries the bead-linking the rollup uses).
	//
	// Like account_cost, the gauge value for a (dim,value) is lifetime-cumulative
	// and monotonic (LiteLLM tag totals never decrease), so increase()/rate() are
	// valid. The poller Sets absolute values (Reset+Set each cycle), and the env
	// label disambiguates prod / gardenio-next / e2e the same way the
	// account gauge does (E2.2).
	tagCost *prometheus.GaugeVec

	// unsettledSessions is the durable-settlement-sweep backlog (A3,
	// ka-0000007288): closed agent beads inside the sweep window whose LiteLLM
	// settlement has not landed (no watermark, or tag spend past the watermark).
	// Set each sweep cycle BEFORE re-attempts, so it is the honest backlog the
	// sweep is working off, and it is alertable — a persistently non-zero value
	// means settlements are being lost faster than the sweep can recover them.
	unsettledSessions prometheus.Gauge

	// spendLogsCost is the NEW stateless per-agent attribution gauge (C3, plan
	// ka-0000003747 / ka-0000000774, operator decision ka-0000006903
	// "replace-now"). Where agentSpend is the OLD stateful path — baseline
	// captured at spawn, delta booked at close, watermark for idempotency —
	// this gauge holds each tracked agent's lifetime spend RECOMPUTED every
	// sweep directly from the LiteLLM /spend/logs request rows
	// (internal/spendlogs, cmd/controller/spendlogs_sweep.go): no baselines,
	// no watermarks, re-derivable after the fact. The label set
	// {agent,project,role,model,account} deliberately mirrors agentSpend so
	// dashboards can overlay old against new per agent during the transition.
	//
	// Only agents whose bead was created inside the sweep's lookback window
	// carry a series — that is the population whose WHOLE life the projection
	// covers, so the value is a true lifetime figure, never a silent partial
	// sum (an agent predating the window is skipped, not under-read). The
	// sweep Reset+Sets the gauge each cycle, so series age out with their
	// agents and cardinality stays bounded by the live-ish population rather
	// than accumulating for the process lifetime like the counter.
	spendLogsCost *prometheus.GaugeVec

	// attributedCost / unattributedCost split the WHOLE LiteLLM bill into the
	// part the per-agent settlement pipeline can ever attribute and the part it
	// can't (Approach B, ka-0000003988). Both are lifetime-cumulative gauges
	// published together each tagSpendSync tick from the same per-proxy reads:
	//
	//   attributedCost   = Σ_proxies Σ agent:* tag totals   (/spend/tags)
	//   unattributedCost = Σ_proxies (proxy bill − Σ agent:*)  (/global/spend)
	//
	// so attributed + unattributed ≡ the full bill BY CONSTRUCTION every tick —
	// requests that carry no agent: tag (untagged consumers, or tags lost on
	// credential rotation, ka-0000000333) land in unattributed instead of
	// silently vanishing from the books. Windowed residual is
	// increase(gardenio_unattributed_cost_usd[1h]); the UnattributedSpendHigh
	// vmalert gap-alert arms it as a quality signal. The ABSOLUTE gauge value is
	// not meaningful (it includes all pre-tagging historical spend) — only
	// increase()/delta() windows are.
	attributedCost   *prometheus.GaugeVec
	unattributedCost *prometheus.GaugeVec
}

// TagCostDims is the closed set of attribution dimensions carried by
// gardenio_tag_cost_usd. Exported so the poller and the rollup consumer agree on
// the dim label values without duplicating string literals. agent and account
// are intentionally excluded: agent: has prohibitive cardinality (catalog §3.5),
// and account already has its own real-bill gauge gardenio_account_cost_usd.
var TagCostDims = []string{"role", "project", "model"}

// NewSpendMetrics constructs the spend counter vector. Register() must be called
// to expose it on the controller :9092 /metrics endpoint.
func NewSpendMetrics() *SpendMetrics {
	return &SpendMetrics{
		agentSpend: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "agent_spend_usd_total",
			Help:      "Cumulative agent spend in USD, incremented once per session at close from the LiteLLM-reconciled session cost (reconcile_session_spend.go). Labels {agent,project,role,model,account}. Monotonic counter — query per-hour spend with increase(gardenio_agent_spend_usd_total[1h]).",
		}, []string{"agent", "project", "role", "model", "account"}),
		operatorSpend: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "operator_cost_usd_total",
			Help:      "Cumulative human-operator spend in USD, incremented once per session at close from the LiteLLM-reconciled session cost (reconcile_session_spend.go). Labels {operator_id,project,model}; operator_id is the opaque operators(id) FK (\"0\" = unattributed). Monotonic counter superseding the former non-authoritative gb session-push estimate (ka-0000000999).",
		}, []string{"operator_id", "project", "model"}),
		tagCost: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "tag_cost_usd",
			Help:      "Real-bill spend in USD broken down by a single attribution dimension (dim ∈ {role,project,model}), sourced from LiteLLM GET /spend/tags across all proxies (tag_spend_sync.go). Lifetime-cumulative per (dim,value) — query windowed spend with increase(gardenio_tag_cost_usd{dim=\"role\"}[1h]). The real-bill, dimensional sibling of gardenio_account_cost_usd; accurate where gardenio_agent_spend_usd_total (at-close counter) reads low (ka-0000000478).",
		}, []string{"dim", "value", "env"}),
		spendLogsCost: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "agent_spend_logs_usd",
			Help:      "Per-agent lifetime spend in USD recomputed statelessly from LiteLLM /spend/logs rows by the periodic spend-logs sweep (spendlogs_sweep.go) — the NEW attribution path replacing the baseline/watermark counter gardenio_agent_spend_usd_total (plan ka-0000003747 C3). Labels {agent,project,role,model,account} mirror the old counter. Only agents whose bead was created inside the sweep lookback carry a series (full-life coverage); absent = not derived, never a partial sum.",
		}, []string{"agent", "project", "role", "model", "account"}),
		unsettledSessions: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "unsettled_sessions",
			Help:      "Closed agent beads inside the settlement-sweep window whose LiteLLM session cost has not been settled (spend_settlement_sweep.go). Set once per sweep cycle before re-attempts. Persistently non-zero means settlements are being lost faster than the sweep recovers them.",
		}),
		attributedCost: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "agent_attributed_cost_usd",
			Help:      "Lifetime-cumulative LiteLLM spend carrying an agent: request tag, summed across all proxies (tag_spend_sync.go, ka-0000003988). The portion of the real bill the per-agent settlement pipeline can attribute. attributed + unattributed ≡ the full bill by construction. Only increase()/delta() windows are meaningful.",
		}, []string{"env"}),
		unattributedCost: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "unattributed_cost_usd",
			Help:      "Lifetime-cumulative LiteLLM spend carrying NO agent: request tag (per-proxy /global/spend bill minus the agent:* tag totals), summed across all proxies (tag_spend_sync.go, ka-0000003988). The residual no per-agent settlement can ever book — untagged consumers and tag-loss bugs land here instead of vanishing from the books. Windowed residual: increase(gardenio_unattributed_cost_usd[1h]); armed by the UnattributedSpendHigh gap-alert.",
		}, []string{"env"}),
	}
}

// Register registers the spend counter with reg, served alongside the poller
// Collector and EventMetrics on the controller :9092 /metrics endpoint.
func (s *SpendMetrics) Register(reg prometheus.Registerer) error {
	if err := reg.Register(s.agentSpend); err != nil {
		return err
	}
	if err := reg.Register(s.operatorSpend); err != nil {
		return err
	}
	if err := reg.Register(s.unsettledSessions); err != nil {
		return err
	}
	if err := reg.Register(s.attributedCost); err != nil {
		return err
	}
	if err := reg.Register(s.unattributedCost); err != nil {
		return err
	}
	if err := reg.Register(s.spendLogsCost); err != nil {
		return err
	}
	return reg.Register(s.tagCost)
}

// EnsureSpendSeries zero-initializes the gardenio_agent_spend_usd_total series
// for one agent at spawn time (A2, ka-0000007288). Without this the series of
// a single-session agent is born at its final value by the close-time
// RecordSpend, and increase() over any window reads it as 0 — the agent's
// entire spend is invisible to every chart. Add(0) creates the series at 0 so
// the close increment becomes an observable delta. Idempotent and harmless on
// respawn (the series already exists; Add(0) is a no-op).
func (s *SpendMetrics) EnsureSpendSeries(agent, project, role, model, account string) {
	if s == nil {
		return
	}
	s.agentSpend.WithLabelValues(agent, project, role, model, account).Add(0)
}

// EnsureOperatorSeries zero-initializes the gardenio_operator_cost_usd_total
// series for one operator at spawn time — same increase()-visibility fix as
// EnsureSpendSeries, for the operator rollup. operatorID is the opaque
// operators(id) FK as a string ("0" = unattributed).
func (s *SpendMetrics) EnsureOperatorSeries(operatorID, project, model string) {
	if s == nil {
		return
	}
	s.operatorSpend.WithLabelValues(operatorID, project, model).Add(0)
}

// SetUnsettledSessions publishes the settlement-sweep backlog gauge. No-op on a
// nil receiver so the sweep works metrics-less in tests.
func (s *SpendMetrics) SetUnsettledSessions(n int) {
	if s == nil {
		return
	}
	s.unsettledSessions.Set(float64(n))
}

// UnsettledSessionsGauge exposes the backlog gauge for test assertions via
// prometheus/testutil, mirroring TagCostGauge. Read-only for callers.
func (s *SpendMetrics) UnsettledSessionsGauge() prometheus.Gauge {
	if s == nil {
		return nil
	}
	return s.unsettledSessions
}

// RecordSpend adds costUSD to gardenio_agent_spend_usd_total for one turn. A
// non-positive cost (missing, zero, or negative — e.g. a usage event that
// carried no response_cost) is dropped so the counter is not advanced by a
// no-op and so a clock-skew/parse glitch can never push it backwards. Returns
// true if the counter was incremented.
func (s *SpendMetrics) RecordSpend(agent, project, role, model, account string, costUSD float64) bool {
	if s == nil {
		return false
	}
	if costUSD <= 0 {
		return false
	}
	s.agentSpend.WithLabelValues(agent, project, role, model, account).Add(costUSD)
	return true
}

// RecordOperatorSpend adds costUSD to gardenio_operator_cost_usd_total for one
// session, keyed on the human operator. Same non-positive-cost guard as
// RecordSpend: a missing/zero/negative cost is dropped so the counter is never
// advanced by a no-op nor pushed backwards. operatorID is the opaque
// operators(id) FK as a string ("0" for unattributed); callers must never pass
// an email here (the label is identity-resolved server-side, never off the
// metric). Returns true if the counter was incremented.
func (s *SpendMetrics) RecordOperatorSpend(operatorID, project, model string, costUSD float64) bool {
	if s == nil {
		return false
	}
	if costUSD <= 0 {
		return false
	}
	s.operatorSpend.WithLabelValues(operatorID, project, model).Add(costUSD)
	return true
}

// TagCostGauge returns the underlying gardenio_tag_cost_usd gauge vector so the
// poller's tests can assert series values/counts via prometheus/testutil without
// exporting the field. It is read-only for callers (use SetTagCosts to mutate).
func (s *SpendMetrics) TagCostGauge() *prometheus.GaugeVec {
	if s == nil {
		return nil
	}
	return s.tagCost
}

// TagCost is one (dim, value) → cumulative-USD datapoint for the real-bill tag
// gauge. The poller builds a fresh slice each cycle from the summed-across-proxies
// LiteLLM /spend/tags totals and hands it to SetTagCosts.
type TagCost struct {
	Dim   string  // one of TagCostDims (role|project|model)
	Value string  // the tag value, e.g. "gardener", "monorepo", "claude-opus-4-8"
	USD   float64 // lifetime-cumulative real spend for this (dim,value)
}

// SetTagCosts replaces the entire gardenio_tag_cost_usd gauge with the supplied
// snapshot, stamping every series with env. It Reset()s then Sets so a (dim,value)
// that vanished from /spend/tags (in practice never — tags are permanent) does
// not linger as a stale series. Unlike the agentSpend COUNTER (never reset),
// this is a gauge holding an absolute lifetime-cumulative value, so Reset+Set is
// safe: VictoriaMetrics sees the re-Set value (always ≥ the prior, since tag
// totals only grow), and increase()/rate() stay correct. A non-positive USD is
// skipped so a $0 tag never creates a noise series. No-op on a nil receiver.
//
// The poller is responsible for only calling this with a COMPLETE snapshot
// (every reachable proxy folded in, failed proxies carried forward from their
// last good read) — a partial snapshot would momentarily drop a proxy's
// contribution and dent increase() over that window.
func (s *SpendMetrics) SetTagCosts(costs []TagCost, env string) {
	if s == nil {
		return
	}
	s.tagCost.Reset()
	for _, c := range costs {
		if c.USD <= 0 || c.Dim == "" || c.Value == "" {
			continue
		}
		s.tagCost.WithLabelValues(c.Dim, c.Value, env).Set(c.USD)
	}
}

// SetAttributionSplit publishes the agent-attributed / unattributed split of
// the full LiteLLM bill in one call, so the two gauges always move together
// and their sum stays the bill for every scrape (ka-0000003988). Values are
// lifetime-cumulative dollars merged across proxies by the tagSpendSync
// poller; negative inputs are clamped to 0 (both sides of the split are sums
// of non-negative per-proxy terms, so a negative here is a caller bug).
// No-op on a nil receiver.
func (s *SpendMetrics) SetAttributionSplit(attributedUSD, unattributedUSD float64, env string) {
	if s == nil {
		return
	}
	s.attributedCost.WithLabelValues(env).Set(max(attributedUSD, 0))
	s.unattributedCost.WithLabelValues(env).Set(max(unattributedUSD, 0))
}

// SpendLogsCost is one agent's stateless lifetime-spend datapoint for the
// gardenio_agent_spend_logs_usd gauge. The spend-logs sweep builds a fresh
// slice each cycle from the /spend/logs projection and hands it to
// SetSpendLogsCosts.
type SpendLogsCost struct {
	Agent   string
	Project string
	Role    string
	Model   string
	Account string
	USD     float64 // lifetime spend recomputed from /spend/logs rows
}

// SetSpendLogsCosts replaces the entire gardenio_agent_spend_logs_usd gauge
// with the supplied snapshot. Reset+Set is the right shape here (unlike the
// never-reset agentSpend counter): each value is an absolute lifetime
// recompute, monotonic per agent while tracked, and an agent that ages out of
// the sweep's lookback must have its series retired rather than frozen at a
// stale value. A non-positive USD is skipped — an agent whose rows sum to
// nothing has nothing to attribute yet, matching RecordSpend's non-positive
// drop on the old path. No-op on a nil receiver.
//
// The sweep is responsible for only calling this with a COMPLETE snapshot of
// the tracked population — on a failed projection it must skip the call
// entirely (keeping the previous cycle's series) rather than publish a
// partial picture.
func (s *SpendMetrics) SetSpendLogsCosts(costs []SpendLogsCost) {
	if s == nil {
		return
	}
	s.spendLogsCost.Reset()
	for _, c := range costs {
		if c.USD <= 0 || c.Agent == "" {
			continue
		}
		s.spendLogsCost.WithLabelValues(c.Agent, c.Project, c.Role, c.Model, c.Account).Set(c.USD)
	}
}

// SpendLogsCostGauge exposes gardenio_agent_spend_logs_usd for test assertions
// via prometheus/testutil, mirroring TagCostGauge. Read-only for callers (use
// SetSpendLogsCosts to mutate).
func (s *SpendMetrics) SpendLogsCostGauge() *prometheus.GaugeVec {
	if s == nil {
		return nil
	}
	return s.spendLogsCost
}

// AttributedCostGauge exposes gardenio_agent_attributed_cost_usd for test
// assertions via prometheus/testutil, mirroring TagCostGauge. Read-only.
func (s *SpendMetrics) AttributedCostGauge() *prometheus.GaugeVec {
	if s == nil {
		return nil
	}
	return s.attributedCost
}

// UnattributedCostGauge exposes gardenio_unattributed_cost_usd for test
// assertions via prometheus/testutil, mirroring TagCostGauge. Read-only.
func (s *SpendMetrics) UnattributedCostGauge() *prometheus.GaugeVec {
	if s == nil {
		return nil
	}
	return s.unattributedCost
}
