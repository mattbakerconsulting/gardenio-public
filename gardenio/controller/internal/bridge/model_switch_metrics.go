package bridge

// model_switch_metrics.go — downstream observability for the two model-switch /
// refusal signals the Opus-5 cutover epic (ka-0000000949) built plumbing for.
//
// The refusal logger (refusal_log.go, ka-0000006122) and the model-fallback
// flagger (model_fallback.go, ka-0000003127) already persist a durable
// type=rating bead + post a Slack flag on every Anthropic stop_reason=refusal and
// every SILENT served!=requested model switch. But a bead + a Slack message are
// point-in-time human artifacts — neither is a TIME SERIES, so an operator cannot
// ask Grafana "how many refusals in the last hour?", "did the opus-5 cutover
// change the fallback rate?", or alert on a spike. Rate-limit / budget / credits
// errors are already a metric (gardenio_agent_upstream_error_active{category},
// internal/metrics/events.go), but refusals and silent model-switches were DARK
// in downstream observability. This file closes that gap.
//
// WHY promauto HERE (and not the controller registry-swap idiom):
// these counters are incremented from RefusalLogger / ModelFallbackFlagger, which
// are constructed and driven ONLY in the slack-bridge binary
// (cmd/slack-bridge/main.go). slack-bridge serves /metrics from
// prometheus.DefaultGatherer WITHOUT swapping DefaultRegisterer
// (cmd/slack-bridge/helpers.go), and that endpoint is scraped by the slack-bridge
// VMServiceScrape into VictoriaMetrics/Grafana. So a promauto var-init
// registration on the DEFAULT registerer is exactly what the slack-bridge exports
// — the same idiom the sibling bridge metrics use (assistant_event_metrics.go,
// gitlab_mention_metrics.go). This is the OPPOSITE of the leg-B claude-status
// mistake (ka-0000008266): that metric had to reach the CONTROLLER's :9092
// endpoint, which serves a SWAPPED registry, so promauto's var-init landed it on
// a registry :9092 never served. Here the target endpoint IS the default
// gatherer, so promauto is correct. (The controller does not construct these
// flaggers, so it exports these series as a flat zero — harmless and honest: the
// controller genuinely observes no refusals.)
//
// CARDINALITY: every label value is a NORMALIZED model GROUP (modelGroup(): one of
// fable/opus/sonnet/haiku, or the bounded sentinels below), never a raw model id
// or a dated snapshot. modelGroup() is token-based, so a new snapshot flows into
// an existing group without a code change and without inflating cardinality. The
// group set is small and closed, so the series count is bounded by construction.

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// modelGroupUnknown is the bounded sentinel used when modelGroup() cannot map a
// model id to a known Anthropic tier (empty id, or a metered Bedrock/glm model
// with no subscription group). Keeping it an explicit label value — rather than
// the empty string — means a PromQL sum over the metric never silently drops the
// "we could not classify the served model" case, which is itself worth watching
// (a served model outside the known tiers is a red flag on a fallback).
const modelGroupUnknown = "unknown"

// gardenio_model_refusal_total counts Anthropic stop_reason=refusal turns the
// slack-bridge classified, one increment per DE-DUPED refusal (the same
// (agent,session,snippet) latch RefusalLogger uses to write ONE bead — so the
// counter reconstructs the refusal-log bead count, not the raw turn count). This
// is the time-series backing for "how many refusals, and is the rate climbing?"
// that the durable refusal beads (ka-0000006122) cannot answer.
//
// Labels:
//   - served_group: the normalized GROUP of the model that issued the refusal
//     (fable/opus/sonnet/haiku/unknown). A refusal is a model behavior, so the
//     served group is the interesting cut — a spike concentrated in one group is
//     the actionable signal.
//   - served_by_fallback: "true" when the refusal was served by a model that
//     DIVERGES from the one the session requested (a refusal that arrived via a
//     silent fallback), "false" otherwise. Lets an operator separate "our
//     requested model refused" from "a fallback model we didn't ask for refused".
var modelRefusalTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "gardenio_model_refusal_total",
		Help: "Anthropic stop_reason=refusal turns classified by the slack-bridge, one per de-duped refusal (matches the ka-0000006122 refusal-log bead count). Labels: served_group (fable/opus/sonnet/haiku/unknown), served_by_fallback (true/false). rate()/increase() answers 'is the refusal rate climbing?' and 'did the opus-5 cutover move it?' — the time series the durable refusal beads cannot express.",
	},
	[]string{"served_group", "served_by_fallback"},
)

// gardenio_model_fallback_total counts SILENT model switches (served group !=
// requested/expected group at the identity level) the slack-bridge flagged, one
// increment per DE-DUPED divergence (the same per-session latch
// ModelFallbackFlagger uses to write ONE bead). This is the time-series backing
// for "how often is Claude silently serving a different model than we requested,
// and did the opus-5 cutover change it?" — the exact behavior the Opus-5 help
// article warns about (support.claude.com/en/articles/16049681).
//
// Labels:
//   - expected_group: normalized group of the model the pod REQUESTED (baked
//     ANTHROPIC_MODEL/CLAUDE_MODEL).
//   - served_group: normalized group of the model actually SERVED.
//
// The (expected_group -> served_group) pair names the exact tier transition, so a
// dashboard can show "opus -> sonnet" downgrades distinctly from a within-family
// switch. Both are groups, never raw ids, so cardinality stays bounded.
var modelFallbackTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "gardenio_model_fallback_total",
		Help: "Silent model switches (served group != requested group) flagged by the slack-bridge, one per de-duped divergence (matches the ka-0000003127 model-fallback bead count). Labels: expected_group, served_group (each fable/opus/sonnet/haiku/unknown). The (expected->served) pair names the tier transition — e.g. sum by (served_group)(increase(...{expected_group='opus'}[1h])) shows what opus requests were downgraded to. Per the Opus-5 silent-switch article (support.claude.com/en/articles/16049681).",
	},
	[]string{"expected_group", "served_group"},
)

// modelGroupLabel maps a raw model id to a BOUNDED metric label value: the
// normalized model group, or the modelGroupUnknown sentinel when the group cannot
// be determined. This is the single guard that keeps the model-switch counters'
// cardinality closed — no raw model id or dated snapshot ever reaches a label.
func modelGroupLabel(model string) string {
	if g := modelGroup(model); g != "" {
		return g
	}
	return modelGroupUnknown
}

// boolLabel renders a bool as the stable string label value "true"/"false" so the
// served_by_fallback dimension is a small closed set.
func boolLabel(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// recordModelRefusal increments gardenio_model_refusal_total for one de-duped
// refusal. served is the raw served-model id (mapped to its bounded group here);
// fallback is whether the refusal arrived via a silent fallback. Called from
// RefusalLogger.Log at the first-seen decision site, so the counter matches the
// durable refusal-bead count 1:1.
func recordModelRefusal(served string, fallback bool) {
	modelRefusalTotal.WithLabelValues(modelGroupLabel(served), boolLabel(fallback)).Inc()
}

// recordModelFallback increments gardenio_model_fallback_total for one de-duped
// silent model switch. expected/served are the raw model ids (each mapped to its
// bounded group here). Called from ModelFallbackFlagger.Flag at the first-seen
// decision site, so the counter matches the durable model-fallback-bead count
// 1:1.
func recordModelFallback(expected, served string) {
	modelFallbackTotal.WithLabelValues(modelGroupLabel(expected), modelGroupLabel(served)).Inc()
}
