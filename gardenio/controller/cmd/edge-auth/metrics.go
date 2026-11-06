package main

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// metrics.go — Prometheus instrumentation for the edge-auth service
// (ka-0000006312, Customer-1 security-event telemetry epic ka-0000002046).
//
// v1 emits a single security counter, gardenio_forwardauth_403_total, that
// counts the authorization denials (HTTP 403 Forbidden) this edge returns,
// split by a BOUNDED deny reason and a BOUNDED route template. Both labels are
// capped-cardinality by construction (see forwardauth403Reason /
// forwardauth403RouteTemplate) so the series count is fixed regardless of how
// many distinct operators or URLs hit the edge — and, per the telemetry plan
// (gardenio/docs/research/customer-1-telemetry-plan.md) and the v2 contract doc
// (gardenio/docs/research/edge-auth-v2-auth-telemetry-contract.md), there is NO
// operator/email label: an operator-attributed denial series would be an
// unbounded, PII-bearing cardinality bomb.
//
// Fine-grained deny reasons (beyond the coarse in-process enum below) and
// Keycloak login/token events are deferred to v2 as genuinely-new infra — the
// oauth2-proxy X-Oauth2-Deny-Reason header and the Keycloak event-listener
// sidecar respectively. Their contracts are documented in the v2 contract doc;
// no emit is wired here.

// forwardauth403MetricName is the fully-qualified name of the v1 edge-auth
// denial counter. Named as a const so the wiring and the test reference one
// source of truth.
const forwardauth403MetricName = "gardenio_forwardauth_403_total"

// Bounded reason label values. The in-process edge only distinguishes the
// coarse authorization outcomes it can name today; deny-reason granularity is a
// v2 item (oauth2-proxy X-Oauth2-Deny-Reason header). Any deny detail the
// mapper does not recognise collapses to reasonOther so the label stays capped.
const (
	reasonNotInGroup     = "not_in_group"
	reasonEmailDomain    = "email_domain"
	reasonEntraForbidden = "entra_forbidden"
	reasonOther          = "other"
)

// routeTemplateOther is the catch-all bucket for any route key not on the
// allowlist, keeping route_template cardinality bounded (<=15) and never
// admitting a raw request URL as a label value.
const routeTemplateOther = "other"

// edgeAuthMetrics holds the Prometheus instruments emitted by the edge-auth
// service. It owns a dedicated registry so the /metrics endpoint exposes only
// this shim's series (plus the standard process/go collectors) and tests can
// assert against a clean registry without global-registry cross-talk.
type edgeAuthMetrics struct {
	registry *prometheus.Registry

	// forwardauth403 counts HTTP 403 authorization denials the edge returns,
	// split by deny reason and originating route template. Both labels are
	// bounded; there is deliberately NO operator/email label.
	forwardauth403 *prometheus.CounterVec
}

// newEdgeAuthMetrics builds the instruments on a fresh dedicated registry.
func newEdgeAuthMetrics() *edgeAuthMetrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	reg.MustRegister(collectors.NewGoCollector())

	m := &edgeAuthMetrics{
		registry: reg,
		forwardauth403: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: forwardauth403MetricName,
			Help: "Count of HTTP 403 authorization denials returned by the gardenio edge-auth service, by bounded deny reason and route template. No operator label (see the v2 telemetry contract doc).",
		}, []string{"reason", "route_template"}),
	}
	reg.MustRegister(m.forwardauth403)
	return m
}

// record403 increments the denial counter for a 403 the edge returned. routeKey
// is the internal route identity (an auditEndpoint* constant, or a proxy
// upstream name); detail is the auditDetail* deny string already passed to the
// audit sink. Both are normalised to bounded label values here so callers can
// pass their raw internal identifiers without risking cardinality growth. Safe
// to call on a nil receiver (metrics disabled).
func (m *edgeAuthMetrics) record403(routeKey, detail string) {
	if m == nil {
		return
	}
	m.forwardauth403.WithLabelValues(
		forwardauth403Reason(detail),
		forwardauth403RouteTemplate(routeKey),
	).Inc()
}

// forwardauth403Reason maps an auditDetail* deny string to the bounded reason
// label. Unknown details collapse to reasonOther so a new deny path added
// without a mapping cannot silently expand label cardinality.
func forwardauth403Reason(detail string) string {
	switch detail {
	case auditDetailDenyNotInGroup:
		return reasonNotInGroup
	case auditDetailDenyEmailDomain:
		return reasonEmailDomain
	case auditDetailDenyEntraForbidden:
		return reasonEntraForbidden
	default:
		return reasonOther
	}
}

// forwardauth403RouteTemplate maps an internal route key to the bounded
// route_template label. The allowlist covers the edge's authorization
// endpoints (auth_check, forwardauth, entra_forwardauth) and its proxy
// upstreams (kbeads, coopmux, nats) — 6 entries, well under the <=15 ceiling
// the telemetry plan sets. Anything else (an unexpected upstream name, a future
// endpoint added without a mapping) collapses to routeTemplateOther, and a raw
// request URL never reaches this label.
func forwardauth403RouteTemplate(routeKey string) string {
	switch routeKey {
	case auditEndpointAuthCheck,
		auditEndpointForwardauth,
		auditEndpointEntraForwardauth,
		"kbeads", "coopmux", "nats":
		return routeKey
	default:
		return routeTemplateOther
	}
}
