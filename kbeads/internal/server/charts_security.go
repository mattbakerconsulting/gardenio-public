package server

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"time"

	"gardenio/kbeads/internal/metrics"
)

// charts_security.go — security-overview panels for the in-house chart backend
// (ka-0000001983, customer-1 telemetry plan §1.3 / §5, Phase 2+3).
//
// Three read-only instant-query panels, all IDENTITY-FREE by design — these
// backbone metrics carry role / action / reason enums, NEVER operator_id, so
// (unlike the engagement panels) there is no server-side identity join here.
// The same no-PII-label discipline still applies: the PromQL strings in this
// file never reference email / display / slack_user_id, and every returned
// label value is collapsed onto a bounded enum so a dashboard category set is
// stable and a mislabeled/new series can never inflate the response.
//
//   - authz     : gardenio_authz_denials_total       by action/reason/bead_type
//   - decisions : gardenio_decisions_lifecycle_total  by event/urgency
//                 + gardenio_escalations_total         by escalated_by_role/outcome
//   - token-slo : gardenio_agent_token_revocation_latency_seconds quantiles vs
//                 the approved SLO target (95%/5min, 99%/60min default).
//
// The authz / decisions backbone metrics already emit (ka-0000006352,
// ka-0000005541). The token-lifecycle histogram is NOT yet emitted (Phase 2 #7,
// ka-0000006565), so the token-SLO panel is V1-CONDITIONAL: it probes for the
// metric's existence and reports Available=false (rather than fabricating an
// SLO verdict from an empty histogram) until the emit sites land — mirroring
// the forwardauth-403 v1-conditional pattern (plan §5, ka-0000006312).

// Backbone metric names. Kept as constants so the security queries and the
// privacy allow-list (gardenio/controller/internal/obspii/allowlist.go) cannot
// drift apart, and a metric rename is a one-line change here.
const (
	authzDenialsMetric           = "gardenio_authz_denials_total"
	decisionsLifecycleMetric     = "gardenio_decisions_lifecycle_total"
	escalationsMetric            = "gardenio_escalations_total"
	tokenRevocationLatencyMetric = "gardenio_agent_token_revocation_latency_seconds"
)

// authzDenialsQuery returns the PromQL for the authz-denial breakdown: count of
// write-authorization rejections in the window, grouped by the three bounded
// label dimensions. increase() (not last_over_time) because this is event
// VOLUME — "how many denials happened" — and increase() accounts for counter
// resets across the range correctly.
func authzDenialsQuery(window string) string {
	return fmt.Sprintf(
		"sum by (action, reason, bead_type)(increase(%s[%s]))",
		authzDenialsMetric, window,
	)
}

// decisionsLifecycleQuery returns the PromQL for the decision-lifecycle panel:
// count of decision lifecycle events in the window by event type and urgency.
func decisionsLifecycleQuery(window string) string {
	return fmt.Sprintf(
		"sum by (event, urgency)(increase(%s[%s]))",
		decisionsLifecycleMetric, window,
	)
}

// escalationsQuery returns the PromQL for the escalation panel: count of
// decision escalations in the window by escalating role and outcome. Role,
// never operator_id — identity-free override-signal analysis (plan §4).
func escalationsQuery(window string) string {
	return fmt.Sprintf(
		"sum by (escalated_by_role, outcome)(increase(%s[%s]))",
		escalationsMetric, window,
	)
}

// tokenRevocationPresenceQuery counts the histogram's _count series. An empty
// result means the histogram has never been observed — i.e. the Phase 2 #7
// emit sites are not yet live — which gates the whole token-SLO panel.
func tokenRevocationPresenceQuery() string {
	return fmt.Sprintf("count(%s_count)", tokenRevocationLatencyMetric)
}

// tokenRevocationQuantileQuery returns the PromQL for an observed revocation
// latency quantile over the window. quantile is a PromQL-literal float in
// [0,1] (e.g. "0.95"); it is always rendered by the caller from a numeric SLO
// objective, never from request input.
func tokenRevocationQuantileQuery(quantile, window string) string {
	return fmt.Sprintf(
		"histogram_quantile(%s, sum by (le)(rate(%s_bucket[%s])))",
		quantile, tokenRevocationLatencyMetric, window,
	)
}

// tokenRevocationWithinBudgetQuery returns the PromQL for the SLO compliance
// fraction: of all revocations in the window, what share completed within the
// budget. budgetLE selects the histogram bucket boundary (e.g. "300" for the
// 5-minute budget) and MUST align to an emitted bucket edge — a budget that
// does not match a bucket le yields an empty result, surfaced as "no data".
func tokenRevocationWithinBudgetQuery(budgetLE, window string) string {
	return fmt.Sprintf(
		"sum(rate(%s_bucket{le=\"%s\"}[%s])) / sum(rate(%s_count[%s]))",
		tokenRevocationLatencyMetric, budgetLE, window,
		tokenRevocationLatencyMetric, window,
	)
}

// AuthzDenialRow is one row of the authz-denials panel: a bounded
// (action, reason, bead_type) tuple and the denial count in the window.
type AuthzDenialRow struct {
	Action   string  `json:"action"`
	Reason   string  `json:"reason"`
	BeadType string  `json:"bead_type"`
	Count    float64 `json:"count"`
}

// SecurityAuthzResponse is the body of GET /v1/charts/security/authz.
type SecurityAuthzResponse struct {
	EvaluatedAt string           `json:"evaluated_at"`
	Window      string           `json:"window"`
	Denials     []AuthzDenialRow `json:"denials"`
	// Total is the summed denial count across all rows, so a dashboard headline
	// number stays honest without re-summing client-side.
	Total float64 `json:"total"`
}

// DecisionEventRow is one row of the decision-lifecycle panel.
type DecisionEventRow struct {
	Event   string  `json:"event"`
	Urgency string  `json:"urgency"`
	Count   float64 `json:"count"`
}

// EscalationRow is one row of the escalation panel.
type EscalationRow struct {
	Role    string  `json:"role"`
	Outcome string  `json:"outcome"`
	Count   float64 `json:"count"`
}

// SecurityDecisionsResponse is the body of GET /v1/charts/security/decisions:
// decision lifecycle + escalation breakdowns over the window.
type SecurityDecisionsResponse struct {
	EvaluatedAt string             `json:"evaluated_at"`
	Window      string             `json:"window"`
	Lifecycle   []DecisionEventRow `json:"lifecycle"`
	Escalations []EscalationRow    `json:"escalations"`
}

// SLOObjective is one revocation-latency objective and its observed compliance.
// Percentile/BudgetSeconds are the TARGET (wired from config or the documented
// default); the Observed* fields are nil when the histogram has data but the
// specific query produced no value (e.g. a budget that does not align to a
// bucket edge), and Met is nil whenever the fraction could not be determined.
type SLOObjective struct {
	Percentile    float64 `json:"percentile"`
	BudgetSeconds float64 `json:"budget_seconds"`
	// ObservedQuantileSeconds is the measured latency at Percentile over the
	// window (e.g. observed p95 latency in seconds). nil when unavailable.
	ObservedQuantileSeconds *float64 `json:"observed_quantile_seconds"`
	// FractionWithinBudget is the share of revocations that completed within
	// BudgetSeconds over the window, in [0,1]. nil when unavailable.
	FractionWithinBudget *float64 `json:"fraction_within_budget"`
	// Met is FractionWithinBudget >= Percentile when the fraction is known.
	Met *bool `json:"met"`
}

// SecurityTokenSLOResponse is the body of GET /v1/charts/security/token-slo.
type SecurityTokenSLOResponse struct {
	EvaluatedAt string `json:"evaluated_at"`
	Window      string `json:"window"`
	// Available is false when the token-revocation histogram is not yet emitted
	// (Phase 2 #7). The objectives still echo the configured TARGET so the UI
	// can render the SLO definition and badge it "awaiting data".
	Available bool `json:"available"`
	// TargetSource is "config" when the SLO target came from the
	// security.token-revocation-slo config key, else "default".
	TargetSource string         `json:"target_source"`
	Objectives   []SLOObjective `json:"objectives"`
}

// tokenRevocationSLOConfigKey is the config key holding the operator-approved
// SLO target. Absent → defaultTokenRevocationSLO is used. The decision gate
// (ka-0000001248) sets the value once approved; this lets the panel reflect the
// APPROVED target instead of a code-frozen guess (plan §5).
const tokenRevocationSLOConfigKey = "security.token-revocation-slo"

// defaultTokenRevocationSLO is the documented default target until the gate
// resolves: 95% within 5 minutes, 99% within 1 hour. Budgets align to emitted
// histogram bucket edges (300s, 3600s) so the within-budget query matches.
var defaultTokenRevocationSLO = []SLOObjective{
	{Percentile: 0.95, BudgetSeconds: 300},
	{Percentile: 0.99, BudgetSeconds: 3600},
}

// tokenRevocationSLOConfig is the JSON shape of the SLO config value.
type tokenRevocationSLOConfig struct {
	Objectives []struct {
		Percentile    float64 `json:"percentile"`
		BudgetSeconds float64 `json:"budget_seconds"`
	} `json:"objectives"`
}

// handleChartsSecurityAuthz handles GET /v1/charts/security/authz.
//
// Query params: ?window=1d|7d|30d (default 7d).
func (s *BeadsServer) handleChartsSecurityAuthz(w http.ResponseWriter, r *http.Request) {
	if s.vmClient == nil {
		writeError(w, http.StatusServiceUnavailable, "charts backend not configured (no VictoriaMetrics endpoint)")
		return
	}
	window := resolveWindow(r.URL.Query().Get("window"))
	at := time.Now().UTC()

	samples, err := s.vmClient.QueryVector(r.Context(), authzDenialsQuery(window), at)
	if err != nil {
		writeServerError(r.Context(), w, "handleChartsSecurityAuthz", err, "authz-denials query failed")
		return
	}

	rows := make([]AuthzDenialRow, 0, len(samples))
	var total float64
	for _, sm := range samples {
		rows = append(rows, AuthzDenialRow{
			Action:   labelOrUnknown(sm.Metric, "action"),
			Reason:   normalizeAuthzReason(sm.Metric["reason"]),
			BeadType: authzBeadTypeLabel(sm.Metric["bead_type"]),
			Count:    sm.Value,
		})
		total += sm.Value
	}
	// Deterministic, dashboard-friendly order: count desc, then the bounded
	// label tuple asc as a stable tiebreak.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Count != rows[j].Count {
			return rows[i].Count > rows[j].Count
		}
		if rows[i].Action != rows[j].Action {
			return rows[i].Action < rows[j].Action
		}
		if rows[i].Reason != rows[j].Reason {
			return rows[i].Reason < rows[j].Reason
		}
		return rows[i].BeadType < rows[j].BeadType
	})

	writeJSON(w, http.StatusOK, SecurityAuthzResponse{
		EvaluatedAt: at.Format(time.RFC3339),
		Window:      window,
		Denials:     rows,
		Total:       total,
	})
}

// handleChartsSecurityDecisions handles GET /v1/charts/security/decisions.
//
// Query params: ?window=1d|7d|30d (default 7d).
func (s *BeadsServer) handleChartsSecurityDecisions(w http.ResponseWriter, r *http.Request) {
	if s.vmClient == nil {
		writeError(w, http.StatusServiceUnavailable, "charts backend not configured (no VictoriaMetrics endpoint)")
		return
	}
	window := resolveWindow(r.URL.Query().Get("window"))
	at := time.Now().UTC()

	lifeSamples, err := s.vmClient.QueryVector(r.Context(), decisionsLifecycleQuery(window), at)
	if err != nil {
		writeServerError(r.Context(), w, "handleChartsSecurityDecisions", err, "decisions-lifecycle query failed")
		return
	}
	escSamples, err := s.vmClient.QueryVector(r.Context(), escalationsQuery(window), at)
	if err != nil {
		writeServerError(r.Context(), w, "handleChartsSecurityDecisions", err, "escalations query failed")
		return
	}

	lifecycle := make([]DecisionEventRow, 0, len(lifeSamples))
	for _, sm := range lifeSamples {
		lifecycle = append(lifecycle, DecisionEventRow{
			Event:   normalizeDecisionEvent(sm.Metric["event"]),
			Urgency: metrics.NormalizeUrgency(sm.Metric["urgency"]),
			Count:   sm.Value,
		})
	}
	sort.Slice(lifecycle, func(i, j int) bool {
		if lifecycle[i].Count != lifecycle[j].Count {
			return lifecycle[i].Count > lifecycle[j].Count
		}
		if lifecycle[i].Event != lifecycle[j].Event {
			return lifecycle[i].Event < lifecycle[j].Event
		}
		return lifecycle[i].Urgency < lifecycle[j].Urgency
	})

	escalations := make([]EscalationRow, 0, len(escSamples))
	for _, sm := range escSamples {
		escalations = append(escalations, EscalationRow{
			Role:    metrics.NormalizeRole(sm.Metric["escalated_by_role"]),
			Outcome: metrics.NormalizeOutcome(sm.Metric["outcome"]),
			Count:   sm.Value,
		})
	}
	sort.Slice(escalations, func(i, j int) bool {
		if escalations[i].Count != escalations[j].Count {
			return escalations[i].Count > escalations[j].Count
		}
		if escalations[i].Role != escalations[j].Role {
			return escalations[i].Role < escalations[j].Role
		}
		return escalations[i].Outcome < escalations[j].Outcome
	})

	writeJSON(w, http.StatusOK, SecurityDecisionsResponse{
		EvaluatedAt: at.Format(time.RFC3339),
		Window:      window,
		Lifecycle:   lifecycle,
		Escalations: escalations,
	})
}

// handleChartsSecurityTokenSLO handles GET /v1/charts/security/token-slo.
//
// Query params: ?window=1d|7d|30d (default 7d). The panel is v1-conditional:
// when the revocation-latency histogram has never been observed it returns
// Available=false with the configured target echoed but no measured values,
// rather than fabricating an SLO verdict from an empty histogram.
func (s *BeadsServer) handleChartsSecurityTokenSLO(w http.ResponseWriter, r *http.Request) {
	if s.vmClient == nil {
		writeError(w, http.StatusServiceUnavailable, "charts backend not configured (no VictoriaMetrics endpoint)")
		return
	}
	window := resolveWindow(r.URL.Query().Get("window"))
	at := time.Now().UTC()

	targets, source := s.tokenRevocationSLOTargets(r.Context())

	available, err := s.tokenRevocationMetricPresent(r.Context(), at)
	if err != nil {
		writeServerError(r.Context(), w, "handleChartsSecurityTokenSLO", err, "token-revocation presence probe failed")
		return
	}

	objectives := make([]SLOObjective, 0, len(targets))
	for _, t := range targets {
		obj := SLOObjective{Percentile: t.Percentile, BudgetSeconds: t.BudgetSeconds}
		if available {
			if err := s.fillSLOObservations(r.Context(), &obj, window, at); err != nil {
				writeServerError(r.Context(), w, "handleChartsSecurityTokenSLO", err, "token-revocation SLO query failed")
				return
			}
		}
		objectives = append(objectives, obj)
	}

	writeJSON(w, http.StatusOK, SecurityTokenSLOResponse{
		EvaluatedAt:  at.Format(time.RFC3339),
		Window:       window,
		Available:    available,
		TargetSource: source,
		Objectives:   objectives,
	})
}

// tokenRevocationMetricPresent reports whether the revocation-latency histogram
// has ever been observed (its _count series exists). This is the v1-conditional
// gate: false means the Phase 2 #7 emit sites are not yet live.
func (s *BeadsServer) tokenRevocationMetricPresent(ctx context.Context, at time.Time) (bool, error) {
	samples, err := s.vmClient.QueryVector(ctx, tokenRevocationPresenceQuery(), at)
	if err != nil {
		return false, err
	}
	for _, sm := range samples {
		if sm.Value > 0 {
			return true, nil
		}
	}
	return false, nil
}

// fillSLOObservations populates an objective's observed quantile, compliance
// fraction, and Met verdict from VictoriaMetrics. A query that yields no value
// (e.g. a budget not aligned to a bucket edge) leaves the corresponding field
// nil rather than coercing a misleading zero.
func (s *BeadsServer) fillSLOObservations(ctx context.Context, obj *SLOObjective, window string, at time.Time) error {
	qStr := strconv.FormatFloat(obj.Percentile, 'f', -1, 64)
	if q, ok, err := s.vmClient.QueryScalar(ctx, tokenRevocationQuantileQuery(qStr, window), at); err != nil {
		return err
	} else if ok && !isNaNOrInf(q) {
		v := q
		obj.ObservedQuantileSeconds = &v
	}

	budgetLE := strconv.FormatFloat(obj.BudgetSeconds, 'f', -1, 64)
	if frac, ok, err := s.vmClient.QueryScalar(ctx, tokenRevocationWithinBudgetQuery(budgetLE, window), at); err != nil {
		return err
	} else if ok && !isNaNOrInf(frac) {
		v := frac
		obj.FractionWithinBudget = &v
		met := frac >= obj.Percentile
		obj.Met = &met
	}
	return nil
}

// tokenRevocationSLOTargets returns the operator-approved SLO objectives from
// the security.token-revocation-slo config key, falling back to the documented
// default. source is "config" or "default" so the response can show provenance.
// A malformed or empty config value falls back to the default (never a panic).
func (s *BeadsServer) tokenRevocationSLOTargets(ctx context.Context) (objectives []SLOObjective, source string) {
	cfg, err := s.store.GetConfig(ctx, tokenRevocationSLOConfigKey)
	if err != nil || cfg == nil || len(cfg.Value) == 0 {
		return defaultTokenRevocationSLO, "default"
	}
	var parsed tokenRevocationSLOConfig
	if err := json.Unmarshal(cfg.Value, &parsed); err != nil || len(parsed.Objectives) == 0 {
		return defaultTokenRevocationSLO, "default"
	}
	out := make([]SLOObjective, 0, len(parsed.Objectives))
	for _, o := range parsed.Objectives {
		// Defend against nonsense targets that would make the panel lie: a
		// percentile must be a real fraction in (0,1] and the budget positive.
		if o.Percentile <= 0 || o.Percentile > 1 || o.BudgetSeconds <= 0 {
			continue
		}
		out = append(out, SLOObjective{Percentile: o.Percentile, BudgetSeconds: o.BudgetSeconds})
	}
	if len(out) == 0 {
		return defaultTokenRevocationSLO, "default"
	}
	return out, "config"
}

// normalizeAuthzReason collapses a reason label onto the bounded emit enum
// (insufficient_role / project_mismatch / escalation_required / denied). The
// emit site only ever produces these four; anything else collapses to the
// catch-all "denied" so a renamed/unknown reason cannot widen the category set.
func normalizeAuthzReason(reason string) string {
	switch reason {
	case reasonInsufficientRole, reasonProjectMismatch, reasonEscalationRequired, reasonDenied:
		return reason
	}
	return reasonDenied
}

// normalizeDecisionEvent collapses an event label onto the bounded lifecycle
// enum. Unknown / absent collapses to a stable "other" bucket so a new or
// misspelled event type can never inflate the panel's category set.
func normalizeDecisionEvent(event string) string {
	switch event {
	case metrics.DecisionEventCreated, metrics.DecisionEventResponded,
		metrics.DecisionEventEscalated, metrics.DecisionEventExpired:
		return event
	}
	return decisionEventOther
}

// decisionEventOther is the catch-all bucket for an unrecognized decision
// event label (the lifecycle enum has no native "other", so it lives here).
const decisionEventOther = "other"

// isNaNOrInf reports whether a VictoriaMetrics scalar is non-finite.
// histogram_quantile over an empty rate range returns NaN, and a
// divide-by-zero compliance fraction returns NaN/Inf; either must surface as
// "no data" (a nil observed field), not as a numeric SLO verdict.
func isNaNOrInf(f float64) bool {
	return math.IsNaN(f) || math.IsInf(f, 0)
}
