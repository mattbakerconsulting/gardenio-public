package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/obspii"
)

// sessionMetricsLabels holds the label values attached to every pushed metric.
type sessionMetricsLabels struct {
	AgentName string
	Project   string
	Account   string
	SessionID string
	// OperatorID is the operators(id) FK for the human who spawned/operates
	// this agent, resolved via a daemon lookup of the agent bead's
	// operated_by_id (see resolveOperatorID). It is "0" (the unattributed
	// sentinel) for legacy, NULL-operator, or S3-resumed sessions where no
	// human can be resolved. It is attached only to the gardenio_operator_*
	// metrics, never to the per-session metrics.
	OperatorID string
	// Model is the LLM model the session ran under (CLAUDE_MODEL). Empty
	// when unset.
	Model string
}

// operatorIDSentinel is the operator_id value emitted when no human operator
// can be resolved (NULL/legacy/S3-resume). It is an explicit "0" rather than
// an empty label so the operator-attributed metrics always carry a non-empty
// operator_id dimension.
const operatorIDSentinel = "0"

// resolveOperatorID looks up the agent bead via the daemon and returns the
// stringified operators(id) FK (operated_by_id) for the human operating the
// agent. It returns operatorIDSentinel ("0") whenever attribution cannot be
// resolved: no daemon, no agent bead id in the environment, the lookup fails,
// or the bead carries a NULL operated_by_id (legacy / anonymous / S3-resume).
//
// The labels for the pushed metrics are env-derived, but operated_by_id lives
// only on the agent bead — hence the extra daemon round-trip here.
func resolveOperatorID(ctx context.Context) string {
	if daemon == nil {
		return operatorIDSentinel
	}
	agentBeadID := envOr("KD_AGENT_ID", os.Getenv("BOAT_AGENT_BEAD_ID"))
	if agentBeadID == "" {
		return operatorIDSentinel
	}

	lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	bead, err := daemon.GetBead(lookupCtx, agentBeadID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: resolving operator_id for %s: %v\n", agentBeadID, err)
		return operatorIDSentinel
	}
	if bead == nil || bead.OperatedByID == nil {
		return operatorIDSentinel
	}
	return strconv.FormatInt(*bead.OperatedByID, 10)
}

// pushSessionMetrics POSTs session usage metrics to vmagent in Prometheus text
// exposition format via /api/v1/import/prometheus. This ensures metrics land
// even when the pod is torn down before the next scrape interval.
//
// It reads VMAGENT_URL from the environment. If unset the push is silently
// skipped — vmagent is not available in every deployment.
func pushSessionMetrics(ctx context.Context, labels sessionMetricsLabels, usage *beadsapi.SessionUsage, durationSecs float64) {
	if usage == nil {
		return
	}
	pushVMAgentLines(ctx, buildPrometheusPayload(labels, usage, durationSecs), 5*time.Second)
}

// buildPrometheusPayload formats session metrics in Prometheus text exposition
// format. Each line: metric_name{labels} value
func buildPrometheusPayload(labels sessionMetricsLabels, usage *beadsapi.SessionUsage, durationSecs float64) string {
	ls := formatLabels(labels)
	var b strings.Builder

	// Cost is intentionally NOT emitted here. The former gardenio_session_cost_usd
	// (and the operator-keyed gardenio_operator_cost_usd_total below) carried
	// usage.TotalCostUSD — the in-pod JSONL response_cost estimate
	// (session_usage.go). That estimate is non-authoritative: turns missing
	// response_cost contribute 0, and it was never reconciled against LiteLLM
	// /spend/tags, so it could permanently understate cost. The authoritative
	// per-session cost is reconciled controller-side at session close
	// (reconcile_session_spend.go) and drives gardenio_agent_spend_usd_total and
	// gardenio_operator_cost_usd_total there. Publishing a low-biased estimate
	// under cost metric names would let consumers read it as truth, so we stop
	// publishing it (ka-0000000999). Token/duration/request metrics below remain —
	// they are exact, in-pod-derived, and not cost.

	// Tokens by type.
	fmt.Fprintf(&b, "gardenio_session_tokens_total{%s,type=\"input\"} %d\n", ls, usage.InputTokens)
	fmt.Fprintf(&b, "gardenio_session_tokens_total{%s,type=\"output\"} %d\n", ls, usage.OutputTokens)
	fmt.Fprintf(&b, "gardenio_session_tokens_total{%s,type=\"cache_read\"} %d\n", ls, usage.CacheReadTokens)
	fmt.Fprintf(&b, "gardenio_session_tokens_total{%s,type=\"cache_write\"} %d\n", ls, usage.CacheWriteTokens)

	// Duration.
	fmt.Fprintf(&b, "gardenio_session_duration_seconds{%s} %.0f\n", ls, durationSecs)

	// Turn count. request_count already counts turns (one API request = one
	// assistant turn; coop usage.rs::accumulate bumps once per UsageDelta), so
	// publish it under the canonical counter name for rate() queries.
	// gardenio_session_request_count_total is kept as an alias for one release
	// (ka-0000003327); dashboards should move to _turns_total.
	fmt.Fprintf(&b, "gardenio_session_turns_total{%s} %d\n", ls, usage.RequestCount)
	fmt.Fprintf(&b, "gardenio_session_request_count_total{%s} %d\n", ls, usage.RequestCount)

	// Live context-window occupancy (a level, not a sum): input + cache_read +
	// cache_creation of the latest turn. Together with _turns_total this lets
	// Grafana compute the turns × context token-load proxy that cross-checks
	// the authoritative LiteLLM USD series — the product itself is deliberately
	// computed only in PromQL, never here (cost_source_guard, ka-0000008056).
	if usage.ContextTokens > 0 {
		fmt.Fprintf(&b, "gardenio_session_context_tokens{%s} %d\n", ls, usage.ContextTokens)
	}

	// Boot-context-at-start (a LEVEL, per session): the tokenized turn-0 resident
	// context — base system prompt + MCP tool defs + skills + CLAUDE.md + gb
	// prime + thread ctx. This is the ~70-90k "boot" number the context-crisis
	// epic (ka-0000003733) is built to make visible: unlike gardenio_prime_hook_bytes
	// (which only sees the ~7k gb-prime hook slice), this comes from real turn-0
	// usage and therefore captures the MCP/system-prompt slice that dominates the
	// boot. Emitted through the same already-working push path as every other
	// session metric, so it lands in VM whenever VMAGENT_URL is set.
	if usage.BootContextTokens > 0 {
		fmt.Fprintf(&b, "gardenio_session_boot_context_tokens{%s} %d\n", ls, usage.BootContextTokens)
	}

	// Tool calls, errors, compactions for completeness.
	fmt.Fprintf(&b, "gardenio_session_tool_calls_total{%s} %d\n", ls, usage.ToolCallCount)
	fmt.Fprintf(&b, "gardenio_session_errors_total{%s} %d\n", ls, usage.ErrorCount)
	fmt.Fprintf(&b, "gardenio_session_compactions_total{%s} %d\n", ls, usage.CompactionCount)

	// Operator-attributed metric. This deliberately does NOT carry the
	// high-cardinality session_id label: it is keyed on the human operator
	// (operator_id), project, and model so duration can be rolled up per operator
	// over time. operator_id is always non-empty — "0" when no human could be
	// resolved (see resolveOperatorID).
	//
	// Only duration is emitted here. gardenio_operator_cost_usd_total used to be
	// published alongside it from usage.TotalCostUSD, but that was the
	// non-authoritative in-pod estimate (see the cost comment above) and the
	// series — lacking session_id — was last-write-wins rather than cumulative.
	// It is now driven authoritatively from the controller's session-close
	// reconciler (reconcile_session_spend.go) as a real monotonic counter
	// (ka-0000000999). Duration is exact and in-pod-derived, so it stays here.
	ols := formatOperatorLabels(labels)
	fmt.Fprintf(&b, "gardenio_operator_session_duration_seconds{%s} %.0f\n", ols, durationSecs)

	return b.String()
}

// sessionMetricsFlushInterval is how often the in-session flush loop re-parses
// the session log and pushes metrics. 5 minutes keeps rate() windows usable
// (ka-0000003327) without hammering vmagent or re-scanning the JSONL too often.
// var (not const) so tests can shrink it.
var sessionMetricsFlushInterval = 5 * time.Minute

// sessionMetricsFlushLoop periodically parses the live session log and pushes
// session metrics to vmagent while coop is running (ka-0000003327 /
// ka-0000002524, Part B of the context-crisis plan ka-0000003733).
//
// Before this loop, gardenio_session_* landed only once, at session close — a
// single sample per series, so gardenio_session_turns_total and
// gardenio_session_context_tokens were points, not time series, and
// rate(...[5m]) was empty. Flushing every 5 minutes makes turn-count and
// context-size real series over a session's lifetime.
//
// The JSONL re-parse uses the exact same accumulator as session close
// (parseSessionUsage), so the flushed values converge to the close-time values
// by construction; the counters are monotonic within a session because the log
// is append-only. No-ops entirely when VMAGENT_URL is unset. Returns when ctx
// is cancelled (coop exited — the close path pushes the final flush).
func sessionMetricsFlushLoop(ctx context.Context, labels sessionMetricsLabels, startedAt time.Time, logPathFn func() string) {
	if os.Getenv("VMAGENT_URL") == "" || labels.SessionID == "" {
		return
	}

	ticker := time.NewTicker(sessionMetricsFlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			logPath := logPathFn()
			if !canLoadSessionUsage(logPath) {
				continue
			}
			usage, err := loadSessionUsage(logPath)
			if err != nil || usage == nil {
				continue
			}
			pushSessionMetrics(ctx, labels, usage, time.Since(startedAt).Seconds())
		}
	}
}

// formatLabels builds the Prometheus label string for the per-session metrics.
func formatLabels(labels sessionMetricsLabels) string {
	return fmt.Sprintf("agent_name=%q,project=%q,account=%q,session_id=%q",
		labels.AgentName, labels.Project, labels.Account, labels.SessionID)
}

// formatOperatorLabels builds the Prometheus label string for the
// operator-attributed metrics: {operator_id,project,model}. operator_id falls
// back to the "0" sentinel when unresolved; session_id is intentionally absent.
//
// Defense-in-depth: every value is run through obspii.SanitizeOperatorLabelValue
// (W2 §6 catch-all, ka-0000007286) so an accidentally-emitted email — e.g. if a
// resolver regression ever stamped an oidc email into operator_id instead of the
// opaque FK — is HMAC-hashed before it is rendered into the push payload and
// reaches vmsingle. This complements the downstream pii-gateway catalog rule and
// the vmagent metric_relabel backstop. operatorRedactionKey() reads
// $OBS_REDACTION_KEY; an unset key still hashes (fail-closed), never leaks.
func formatOperatorLabels(labels sessionMetricsLabels) string {
	operatorID := labels.OperatorID
	if operatorID == "" {
		operatorID = operatorIDSentinel
	}
	key := operatorRedactionKey()
	operatorID = obspii.SanitizeOperatorLabelValue(key, operatorID)
	project := obspii.SanitizeOperatorLabelValue(key, labels.Project)
	model := obspii.SanitizeOperatorLabelValue(key, labels.Model)
	return fmt.Sprintf("operator_id=%q,project=%q,model=%q",
		operatorID, project, model)
}

// operatorRedactionKey returns the HMAC key for the producer-side PII catch-all
// (doc §3.1). Read from $OBS_REDACTION_KEY; when unset the sanitiser still
// hashes (keyed by nil) rather than passing cleartext through — fail-closed.
func operatorRedactionKey() []byte {
	if k := os.Getenv("OBS_REDACTION_KEY"); k != "" {
		return []byte(k)
	}
	return nil
}
