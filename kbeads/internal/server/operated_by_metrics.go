package server

import (
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// Prometheus metrics for the human-attribution (operated_by) rollout. R9
// (docs/research/human-attribution-rollout.md §4.1) calls out five metrics
// — these surfaced the rollout to the operator before flipping the
// feature on. Post-R2 (ka-0000005772) live-state writes fail-closed when
// no operator can be attributed, so M2/M3/M4 are now post-mortem
// observability for resolver behavior rather than rollout gates. They
// are registered with the metrics handler in MetricsHandler.
//
// Cardinality budget (R9 §4.1): M1 ≤ 8 series, M2 ≤ 15 series, M3 ≤ 4,
// M4 ≤ 2, M5 = 1. No unbounded labels. Do not add user-id, bead-id, or
// raw header values — they belong in structured logs only.
var (
	// M1: rate of inbound requests carrying X-Operated-By, by auth_type.
	// Incremented at request entry — BEFORE the feature flag short-circuits
	// the resolver — so the operator can see "is the CLI/bridge actually
	// sending the header?" while the flag is still OFF (the pre-flip gate,
	// R9 §3.2 step 1). Bounded series: auth_type ∈ {oidc, static, fleet,
	// anonymous} × header_present ∈ {true, false} = 8.
	operatedByHeaderArrivalTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gardenio_beads_operated_by_header_arrival_total",
			Help: "Number of inbound requests by auth_type and X-Operated-By header presence. Drives the pre-flip arrival-rate gate (R9 §3.2 step 1). Incremented before the feature flag is consulted.",
		},
		[]string{"auth_type", "header_present"},
	)

	// M2: classification of every audit-row write. Incremented once per
	// resolver call — i.e. once per mutation that touches operated_by.
	// kind ∈ {real, sentinel, empty}; provider ∈ {slack, oidc, system,
	// fleet, ""}. The "real / total" ratio is the soak threshold (R9 §3.3).
	// Pre-flip the ratio is 0 by design — the alert at §4.3 is gated on
	// the env or the flag state.
	operatedByWritesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gardenio_beads_operated_by_writes_total",
			Help: "Audit-row writes classified by operated_by kind and provider. kind ∈ {real,sentinel,empty}; provider ∈ {slack,oidc,system,fleet,\"\"}. Drives the soak threshold (R9 §3.3).",
		},
		[]string{"kind", "provider"},
	)

	// M3: resolver error counter. Direct rollback signal (R9 §4.3
	// OperatedByResolverErrors). error_type is bounded:
	//   - sanitize_reject: a raw header failed sanitization (control char,
	//     too long, empty after trim). The interceptor refused to forward.
	//   - agent_lookup_fail: the static-token fallback could not load the
	//     agent's bead (GetBead returned error).
	//   - unexpected: a path that the resolver does not anticipate (id.Type
	//     unrecognized AND non-empty). Reserved as the "something is very
	//     wrong" bucket.
	operatedByResolverErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gardenio_beads_operated_by_resolver_errors_total",
			Help: "Resolver error counts. error_type ∈ {sanitize_reject, agent_lookup_fail, unexpected}. Powers OperatedByResolverErrors alert (R9 §4.3).",
		},
		[]string{"error_type"},
	)

	// M4: outcome of the static-token agent-bead inheritance fallback.
	// Incremented every time step 3 of the resolver ladder fires (a
	// static-token call without an X-Operated-By header). result is
	// "resolved" when the bead carries a usable slack_user_id, "empty"
	// otherwise. Sustained "empty" → the CLI plumbing is broken (R9 §4.1).
	operatedByFallbackTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gardenio_beads_operated_by_fallback_total",
			Help: "Agent-bead inheritance fallback outcomes. result ∈ {resolved, empty}. Tracks how often the static-token fallback path fires and whether it produces a usable operator (R9 §4.1).",
		},
		[]string{"result"},
	)

	// M5: ambiguous-agent-name detection. R7 §6.6 — must always be 0.
	// Pages immediately when it increments. The increment site lives in
	// the offline backfill tool today; the metric is exposed here so the
	// alert (R9 §4.3 OperatedByAmbiguousAgent) can scrape it via the same
	// /metrics endpoint as the rest of the rollout signals.
	operatedByAmbiguousAgentTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "gardenio_beads_operated_by_ambiguous_agent_total",
			Help: "Detections of an agent name re-used across spawns (R7 §6.6). Must be 0 — pages immediately via OperatedByAmbiguousAgent (R9 §4.3).",
		},
	)

	// M6: writes whose resolver settled on an agent-shaped operator id
	// (anything that is neither a human-shape "slack:U…" / "oidc:…@…"
	// nor a sentinel "system:…" / "fleet:…"). Per the ka-0000006809
	// Invariant the steady-state value MUST be zero — every non-zero
	// increment indicates a spawn path that pushed an agent identity
	// into Fields[operated_by] or supplied it via X-Operated-By. Drives
	// the C2 acceptance signal (ka-0000002601): counter at zero in
	// the e2e namespace after deploy.
	writesWithAgentOperatorTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "gardenio_beads_writes_with_agent_operator_total",
			Help: "Writes whose resolved operated_by is an agent identity (non-human, non-sentinel). MUST be zero in steady state per ka-0000006809 Invariant §1.",
		},
	)

	// M7: writes whose resolver settled on the system:legacy-pre-attribution
	// sentinel (R6 cut-over, ka-0000006798, epic ka-0000006809 surface 6). The
	// outcome label is bounded to two values:
	//   - accepted: the cut-over is not yet active (before the declared date /
	//     BEADS_LEGACY_SENTINEL_CUTOVER off) and the row bound the sentinel.
	//   - rejected: the cut-over is active and the write was refused with a 4xx
	//     (resolver_source=legacy_sentinel_cutover) rather than binding a new
	//     row to the historical sink.
	// Per the R6 declaration ANY non-zero increment AFTER the cut-over date is
	// actionable: every legacy row was migrated by the ka-0000001880 cleanup
	// track before the flip, so a caller still routing to the sentinel is an
	// unmigrated write path that must be found and fixed. The
	// LegacySentinelWriteAfterCutover alert (helm) pages on it.
	operatedByLegacySentinelTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gardenio_beads_operated_by_legacy_sentinel_total",
			Help: "Writes whose resolved operated_by is system:legacy-pre-attribution. outcome ∈ {accepted,rejected}. Any non-zero increment after the R6 cut-over date pages the operator (ka-0000006798).",
		},
		[]string{"outcome"},
	)

	// M8: write classification cross-tabbed by RESOLVER SOURCE. M2 answers
	// "what fraction of writes carry a real operator?"; M8 answers the
	// follow-up the rollout needs before flipping the authoring gate to
	// enforce (task ka-0000003187): "for the writes that STILL settle on a
	// sentinel, WHICH resolver branch produced them?" — the residual-sentinel
	// attribution the dashboard's residual panel reads.
	//
	// source ∈ the R9 §4.4 operated_by_source vocabulary: oidc_subject,
	// static_header, agent_bead_fallback, fleet_token, empty (the exact set
	// resolveOperatedByPure emits). kind ∈ {real,sentinel,empty} — the same
	// classifyOperatedBy buckets M2 uses, so `kind="sentinel"` selects exactly
	// the residual writes an operator must chase down to a caller. Cardinality:
	// 5 sources × 3 kinds = 15 series max, no unbounded labels (R9 §4.1 budget).
	operatedByWritesBySourceTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gardenio_beads_operated_by_writes_by_source_total",
			Help: "Audit-row writes cross-tabbed by resolver source and operated_by kind. source ∈ {oidc_subject,static_header,agent_bead_fallback,fleet_token,empty}; kind ∈ {real,sentinel,empty}. Filter kind=sentinel to attribute residual sentinel writes to the resolver branch that produced them (ka-0000003187).",
		},
		[]string{"source", "kind"},
	)

	// M9: authoring-time human-operator gate outcomes for AUTONOMOUS bead
	// types (trigger/schedule; gardener loop-specs ride on those). This is the
	// observability half of the staged warn→enforce rollout (task
	// ka-0000003187): in `warn` mode the gate ALLOWS an autonomous bead whose
	// resolved operator is non-human but records action=allowed_warn here, so
	// the operator can see the exact residual authoring the flip to `enforce`
	// would start REFUSING (action=refused) before committing to the flip.
	// action=allowed_human is the healthy steady state (a real human author).
	// Cardinality: bead_type ∈ {trigger,schedule} × action ∈
	// {allowed_human,allowed_warn,refused} = 6 series max.
	authoringHumanGateTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gardenio_beads_authoring_human_gate_total",
			Help: "Authoring-time human-operator gate outcomes for autonomous bead types. bead_type ∈ {trigger,schedule}; action ∈ {allowed_human,allowed_warn,refused}. action=allowed_warn counts the residual non-human autonomous authoring a flip to enforce would refuse (ka-0000003187).",
		},
		[]string{"bead_type", "action"},
	)

	// short_title gate outcomes at create (plan 2026-10-14-short-titles D2,
	// gd-V5nGseRS5f). action=allowed_warn with caller=agent is the residual a
	// flip of BEADS_SHORT_TITLE_GATE to enforce would start refusing — the
	// go/no-go signal for that flip (plan Task 10). Cardinality: bead_type ∈
	// {task,bug,feature,epic,decision,escalation} × caller ∈ {human,agent} ×
	// action ∈ {supplied,allowed_warn,refused} = 36 series max.
	shortTitleGateTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gardenio_beads_short_title_gate_total",
			Help: "short_title gate outcomes at create for human-facing bead types. bead_type ∈ {task,bug,feature,epic,decision,escalation}; caller ∈ {human,agent}; action ∈ {supplied,allowed_warn,refused}. caller=agent,action=allowed_warn counts the residual a flip of BEADS_SHORT_TITLE_GATE to enforce would refuse (gd-V5nGseRS5f).",
		},
		[]string{"bead_type", "caller", "action"},
	)

	// heraldry gate outcomes at an epic create (badges plan step 2,
	// gd-fn7uPujOqj). action=allowed_warn with caller=agent is the residual a
	// flip of BEADS_HERALDRY_GATE to enforce could start refusing (enforce
	// refuses only plan and phase epics, so it is an upper bound).
	// Cardinality: caller ∈ {human,agent} × action ∈
	// {supplied,allowed_warn,refused} = 6 series max.
	heraldryGateTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gardenio_beads_heraldry_gate_total",
			Help: "heraldry gate outcomes at an epic create. caller ∈ {human,agent}; action ∈ {supplied,allowed_warn,refused}. caller=agent,action=allowed_warn bounds the residual a flip of BEADS_HERALDRY_GATE to enforce would refuse (gd-fn7uPujOqj).",
		},
		[]string{"caller", "action"},
	)

	// M10: writes that fell back to a FLEET source override at write time,
	// labeled by the fleet service that produced the fallback (task
	// ka-0000002480). M8 (writes_by_source, kind=sentinel × source=fleet_token)
	// already counts residual fleet-sentinel writes in aggregate, but it
	// collapses every bridge into one series — it cannot answer "WHICH bridge
	// is still stamping fleet:<name>?" This counter is the point-in-time,
	// per-source signal: it increments in the fleet-sentinel terminal branch of
	// resolveOperatedByPure the moment a header-less fleet-token write settles
	// on fleet:<subject>, so an operator sees the leak live instead of hand-
	// querying beads (ka-0000005200 is the paired rolling-acceptance query).
	//
	// source is normalized to the bounded fleet-service vocabulary
	// (slack-bridge, jira-bridge, gitlab-bridge, scheduler) with every other
	// subject collapsed to "other", so an unexpected fleet name can never
	// inflate cardinality: 5 series max (R9 §4.1 budget).
	operatorFleetFallbackTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gardenio_beads_operator_fleet_fallback_total",
			Help: "Writes that fell back to a fleet source override (operated_by = fleet:<source>) at write time, by fleet service. source ∈ {slack-bridge,jira-bridge,gitlab-bridge,scheduler,other}. Live per-source signal for residual fleet-sentinel attribution (ka-0000002480); pairs with the rolling acceptance query (ka-0000005200).",
		},
		[]string{"source"},
	)

	// M11: authenticated requests by the credential TYPE that authenticated
	// them, cross-tabbed with read vs write. M1 also carries auth_type, but it
	// only exists to answer "is the header arriving?" and collapses edge into
	// anonymous; M8's source label cannot answer "who is still on the static
	// token?" because resolveOperatedByPure labels BOTH id.Type=static and
	// id.Type=fleet calls that carry X-Operated-By as static_header (per-agent
	// tokens are fleet tokens and agents send the header — gd-M4pFzv6bUq).
	// This counter is the deprecation-gate signal: the static-token retirement
	// gate (gd-ke6PC7bRto / gd-5QYwXtV7IA) is 24h of zero
	// auth_type="static" series growth from in-cluster sources.
	//
	// Incremented once per authenticated request on HTTP (loggingMiddleware,
	// which sits below every auth path) and gRPC (LoggingInterceptor and the
	// stream interceptor). Requests rejected by auth never reach it.
	// Cardinality: auth_type ∈ {static,fleet,oidc,machine,edge,other} ×
	// method_class ∈ {read,write} = 12 series max.
	requestsByAuthTypeTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gardenio_beads_requests_by_auth_type_total",
			Help: "Authenticated requests by credential type and method class. auth_type ∈ {static,fleet,oidc,machine,edge,other}; method_class ∈ {read,write}. auth_type=static is the primary shared bearer token only (a per-agent fleet token carrying X-Operated-By is auth_type=fleet). Drives the static-token deprecation gate (gd-M4pFzv6bUq).",
		},
		[]string{"auth_type", "method_class"},
	)
)

// recordRequestByAuthType increments M11 once per authenticated request.
// write is true for state-changing requests. Unknown auth types collapse to
// "other" so a new AuthIdentity.Type cannot silently inflate cardinality.
func recordRequestByAuthType(authType string, write bool) {
	class := "read"
	if write {
		class = "write"
	}
	requestsByAuthTypeTotal.WithLabelValues(normalizeRequestAuthType(authType), class).Inc()
}

// normalizeRequestAuthType bounds the M11 auth_type label. Unlike M1's
// normalizeAuthTypeLabel it keeps edge distinct and maps unknown/anonymous
// identities (auth disabled, refresh) to "other" rather than pretending they
// are an anonymous caller.
func normalizeRequestAuthType(authType string) string {
	switch authType {
	case "static", "fleet", "oidc", "machine", "edge":
		return authType
	}
	return "other"
}

// httpMethodIsWrite reports whether an HTTP method changes state.
func httpMethodIsWrite(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

// grpcMethodIsWrite classifies a full gRPC method ("/pkg.Service/Method") as a
// write unless its name starts with a read verb. Unknown names count as writes
// so a new mutating RPC is never under-reported.
func grpcMethodIsWrite(fullMethod string) bool {
	name := fullMethod[strings.LastIndex(fullMethod, "/")+1:]
	for _, p := range []string{"Get", "List", "Search", "SemanticSearch", "MyWorkSummary", "Whoami", "Health"} {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	return true
}

// recordFleetFallback increments M10 once per resolver call that settles on
// the fleet-sentinel terminal branch (operated_by = fleet:<subject>). subject
// is the fleet-token service name (id.Subject); it is normalized to the
// bounded fleet-service vocabulary so a novel fleet name cannot expand the
// series count.
func recordFleetFallback(subject string) {
	operatorFleetFallbackTotal.WithLabelValues(normalizeFleetSourceLabel(subject)).Inc()
}

// normalizeFleetSourceLabel bounds the M10 source label to the known fleet
// services. Anything else collapses to "other" — the label must never carry a
// raw, unbounded subject (R9 §4.1 cardinality budget).
func normalizeFleetSourceLabel(subject string) string {
	switch subject {
	case "slack-bridge", "jira-bridge", "gitlab-bridge", "scheduler":
		return subject
	}
	return "other"
}

// recordOperatedByWriteBySource increments M8 once per resolver call, cross-
// tabbing the resolved value's kind against the resolver source label. source
// is the R9 §4.4 label resolveOperatedByPure returned alongside value; it is
// normalized to the bounded vocabulary so a novel source can never inflate
// cardinality.
func recordOperatedByWriteBySource(value, source string) {
	kind, _ := classifyOperatedBy(value)
	operatedByWritesBySourceTotal.WithLabelValues(normalizeSourceLabel(source), kind).Inc()
}

// normalizeSourceLabel keeps the M8 source label bounded to the R9 §4.4
// operated_by_source vocabulary. Anything else collapses to "empty" — the
// resolver only ever emits the enumerated set, so an unknown value indicates a
// new source that must be added here deliberately rather than silently
// expanding the series count.
func normalizeSourceLabel(source string) string {
	switch source {
	case operatedBySourceOIDC, operatedBySourceHeader, operatedBySourceFallback,
		operatedBySourceFleet, operatedBySourceMachine, operatedBySourceTriggerLineage,
		operatedBySourceEmpty:
		return source
	}
	return operatedBySourceEmpty
}

// recordAuthoringGate increments M9 once per createBead call for an autonomous
// bead type. action is one of authoringGateAction* below.
func recordAuthoringGate(beadType, action string) {
	authoringHumanGateTotal.WithLabelValues(beadType, action).Inc()
}

const (
	// authoringGateActionAllowedHuman: the autonomous bead carried a real
	// human operator — the healthy steady state, allowed in every mode.
	authoringGateActionAllowedHuman = "allowed_human"
	// authoringGateActionAllowedWarn: warn mode allowed an autonomous bead
	// whose resolved operator is non-human. Enforce would have refused it.
	authoringGateActionAllowedWarn = "allowed_warn"
	// authoringGateActionRefused: enforce mode refused an autonomous bead
	// with a non-human operator.
	authoringGateActionRefused = "refused"
)

// recordShortTitleGate increments the short_title gate counter once per
// createBead call for a gated bead type while the gate is not off.
func recordShortTitleGate(beadType, caller, action string) {
	shortTitleGateTotal.WithLabelValues(beadType, caller, action).Inc()
}

const (
	shortTitleGateCallerHuman = "human"
	shortTitleGateCallerAgent = "agent"

	// shortTitleGateActionSupplied: the create carried a short_title.
	shortTitleGateActionSupplied = "supplied"
	// shortTitleGateActionAllowedWarn: no short_title; allowed with a warning
	// (warn mode, or a human caller in enforce mode).
	shortTitleGateActionAllowedWarn = "allowed_warn"
	// shortTitleGateActionRefused: enforce mode refused a non-human caller.
	shortTitleGateActionRefused = "refused"
)

// recordHeraldryGate increments the heraldry gate counter once per epic
// createBead call while the gate is not off.
func recordHeraldryGate(caller, action string) {
	heraldryGateTotal.WithLabelValues(caller, action).Inc()
}

const (
	heraldryGateCallerHuman = "human"
	heraldryGateCallerAgent = "agent"

	// heraldryGateActionSupplied: the epic create carried arms.
	heraldryGateActionSupplied = "supplied"
	// heraldryGateActionAllowedWarn: no arms; allowed with a warning.
	heraldryGateActionAllowedWarn = "allowed_warn"
	// heraldryGateActionRefused: enforce mode refused a non-human caller's
	// plan or phase epic.
	heraldryGateActionRefused = "refused"
)

// recordLegacySentinelWrite increments M7 once per resolver call that settles
// on the system:legacy-pre-attribution sentinel. rejected is true when the
// cut-over is active and the write is being refused; false when the sentinel
// bind is still permitted (pre-cut-over).
func recordLegacySentinelWrite(rejected bool) {
	outcome := "accepted"
	if rejected {
		outcome = "rejected"
	}
	operatedByLegacySentinelTotal.WithLabelValues(outcome).Inc()
}

// recordHeaderArrival increments M1 once per inbound request. authType is
// the bound label; presence is true when the caller sent a non-empty
// X-Operated-By header (regardless of whether it survived sanitization).
// Called from both the gRPC and HTTP auth interceptors before the flag is
// consulted, so the operator can read M1 even while the rollout flag is OFF.
func recordHeaderArrival(authType string, present bool) {
	at := normalizeAuthTypeLabel(authType)
	hp := "false"
	if present {
		hp = "true"
	}
	operatedByHeaderArrivalTotal.WithLabelValues(at, hp).Inc()
}

// recordOperatedByWrite increments M2 once per resolver call. value is the
// resolved operated_by string (possibly empty). The kind/provider labels are
// derived from the value to keep the call sites simple — pass the raw
// resolver output and this function does the classification.
//
// Classification:
//   - "" → kind=empty, provider=""
//   - "system:…", "fleet:…" → kind=sentinel, provider=that prefix (the
//     bare sentinels that exist before the flag flip)
//   - "slack:…", "oidc:…" → kind=real, provider=that prefix
//   - any other shape → kind=sentinel, provider="" (do not let novel
//     shapes inflate cardinality; the resolver should not emit them).
func recordOperatedByWrite(value string) {
	kind, provider := classifyOperatedBy(value)
	operatedByWritesTotal.WithLabelValues(kind, provider).Inc()
	if isAgentOperator(value) {
		writesWithAgentOperatorTotal.Inc()
	}
}

// isAgentOperator reports whether v is a resolver output that does not
// match a well-formed human shape ("slack:U…", "oidc:…@…") AND is not a
// recognized sentinel ("system:…", "fleet:…"). The empty string is not
// counted — empty is handled separately by the fail-closed branch in
// resolveAuditOperatorRequired.
//
// Bare agent ids ("gardenio-p1-…", "sched-…", "thread-…", "static") and
// any other non-prefixed identity fall into this bucket. Used as the
// increment guard for gardenio_beads_writes_with_agent_operator_total.
func isAgentOperator(v string) bool {
	if v == "" {
		return false
	}
	if isSentinelOperator(v) {
		return false
	}
	return !isHumanOperator(v)
}

// classifyOperatedBy parses the resolver output into bounded kind/provider
// label values. Pure function — exported lower-case for use by tests.
func classifyOperatedBy(value string) (kind, provider string) {
	if value == "" {
		return "empty", ""
	}
	prefix, _, hasColon := strings.Cut(value, ":")
	if !hasColon {
		return "sentinel", ""
	}
	switch prefix {
	case "slack", "oidc":
		return "real", prefix
	case "system", "fleet":
		return "sentinel", prefix
	}
	return "sentinel", ""
}

// recordResolverError increments M3 with one of the bound error types.
// Callers must use the named buckets below — passing an unknown error
// type bumps the unexpected bucket so cardinality stays bounded.
//
// Buckets:
//   - sanitize_reject: raw header rejected by sanitizeOperatedByHeader.
//   - agent_lookup_fail: GetBead failed for an agent during the resolver
//     fallback (initial subject or any spawned_by chain hop).
//   - spawn_chain_overflow: the bounded X-Spawned-By chain walk hit
//     maxSpawnChainHops without resolving to a human operator
//     (ka-0000006147 acceptance signal).
//   - unexpected: a path the resolver does not anticipate.
func recordResolverError(errorType string) {
	switch errorType {
	case "sanitize_reject", "agent_lookup_fail", "trigger_lookup_fail", "spawn_chain_overflow", "unexpected":
		operatedByResolverErrorsTotal.WithLabelValues(errorType).Inc()
	default:
		operatedByResolverErrorsTotal.WithLabelValues("unexpected").Inc()
	}
}

// recordFallback increments M4 with result ∈ {resolved, empty}. Called
// once at the end of every static-token agent-bead fallback attempt.
func recordFallback(resolved bool) {
	result := "empty"
	if resolved {
		result = "resolved"
	}
	operatedByFallbackTotal.WithLabelValues(result).Inc()
}

// normalizeAuthTypeLabel keeps the M1 auth_type label bounded to the
// four values R9 §4.1 enumerates. Anything else collapses to anonymous,
// which is also the AuthIdentityFromContext fallback when no identity
// is in the context. This guards against future AuthIdentity.Type
// values silently expanding the cardinality of M1.
func normalizeAuthTypeLabel(authType string) string {
	switch authType {
	case "oidc", "static", "fleet", "machine", runnerIdentityType, "anonymous":
		return authType
	}
	return "anonymous"
}
