package bridge

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// GitLab inbound-webhook receive-path metrics (ka-0000005632).
//
// WHY THIS EXISTS. Inbound Jira/GitLab webhooks are a named, permanent NON-AVA
// carve-out: third-party POSTers cannot do AWS Verified Access browser OIDC, so
// the two bridge /webhook endpoints are reachable outside AVA and gated only by
// an HMAC/shared-secret header plus an ingress IP allowlist (operator decision,
// Matt 2026-06-03). The carve-out is accepted ONLY because it is compensated by
// (1) logs and (2) metrics fronted on Grafana / the beads-viewer Health page.
// The Jira side already emits gardenio_jira_webhook_deliveries_total{outcome} and
// a full deadman suite (jira_webhook_deadman.go); the GitLab side emitted NO
// receive-path metric at all, so "the GitLab webhook is receiving nothing" was
// indistinguishable from "healthy and quiet" — the exact blind spot that left
// the Jira hook dark in prod for 8 days (bug ka-0000000426). This restores
// metrics parity for the GitLab door of the carve-out.
//
// See gardenio/docs/security/non-ava-webhook-carveout.md.

// Terminal outcomes for gardenio_gitlab_webhook_deliveries_total. Every terminal
// path in GitLabWebhookHandlerWithConfig records exactly one, so a still-
// arriving-but-rejected stream (token_failed / bad_request) is distinguishable
// from silence. Mirrors the Jira outcome vocabulary (jira_webhook_deadman.go)
// so a single Grafana panel can chart both bridges with one label set.
const (
	gitlabWebhookOutcomeProcessed   = "processed"    // event dispatched to a handler (200 processed)
	gitlabWebhookOutcomeIgnored     = "ignored"      // authenticated, decoded, but unhandled kind/action (200 ignored/no_match)
	gitlabWebhookOutcomeTokenFailed = "token_failed" // X-Gitlab-Token missing/mismatched, or secret not configured (403)
	gitlabWebhookOutcomeBadRequest  = "bad_request"  // wrong method, oversized/unreadable body, or undecodable payload (400/405)
	gitlabWebhookOutcomeError       = "error"        // authenticated and decoded, but downstream processing failed (500)
)

// gardenio_gitlab_webhook_deliveries_total counts inbound GitLab webhook
// deliveries by terminal outcome. This is BOTH the observability surface (a
// rejected stream shows as token_failed/bad_request instead of silence) and the
// raw receive-liveness signal for the GitLab door of the non-AVA webhook
// carve-out. In the fully-dark case (deregistered hook / wrong secret at the
// GitLab console producing no delivery at all) nothing increments it — that is
// exactly the state to detect — so an alert keys on absent()/increase()==0 over
// the authenticated outcomes rather than on a single sample.
var gitlabWebhookDeliveries = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "gardenio_gitlab_webhook_deliveries_total",
	Help: "Inbound GitLab webhook deliveries by terminal outcome (processed|ignored|token_failed|bad_request|error); the receive-path liveness signal for the GitLab door of the non-AVA webhook carve-out (ka-0000005632).",
}, []string{"outcome"})

// recordGitLabWebhookOutcome accounts one terminal receive-path outcome and, for
// the authenticated-and-decoded outcomes (processed|ignored|error), stamps the
// receive-liveness clock the dark-hook deadman monitor reads
// (gitlab_webhook_deadman.go, GAP-C). token_failed and bad_request deliberately
// do NOT stamp it: a delivery that failed the X-Gitlab-Token check or was
// malformed does not prove the webhook is registered and correctly secreted, so
// it must not keep the deadman quiet — keying "alive" on authenticated
// deliveries is what makes a wrong-secret hook (token_failed only) page rather
// than read as healthy (mirrors the Jira ACCEPTANCE clause).
//
// error DOES stamp the clock even though it is a 500: it is recorded only AFTER
// the token check passed and the payload fully decoded (a downstream bead-list
// failure), so it proves GitLab is delivering correctly-secreted, well-formed
// webhooks — the receive path is demonstrably alive; only downstream processing
// failed (which has its own signals). Excluding it would let a daemon outage
// (every delivery errors) false-page as "webhook dark," misattributing the
// outage to a hook-registration problem. This is the one GitLab-specific arm
// with no Jira analog (the Jira handler has no error outcome).
//
// started is when the handler began serving this delivery; it feeds the handling
// -latency histogram (webhook_latency.go), threaded through this funnel for the
// same reason as the Jira side — every terminal path already calls here exactly
// once, so the histogram counts the same population as the counter by
// construction. Pass the zero Time outside a real request (tests) to skip the
// observation.
func recordGitLabWebhookOutcome(outcome string, started time.Time) {
	gitlabWebhookDeliveries.WithLabelValues(outcome).Inc()
	observeWebhookHandling(webhookSourceGitLab, outcome, started)
	switch outcome {
	case gitlabWebhookOutcomeProcessed, gitlabWebhookOutcomeIgnored, gitlabWebhookOutcomeError:
		lastGitLabWebhookReceivedNano.Store(time.Now().UnixNano())
	}
}

// InitGitLabWebhookMetrics touches every outcome series to its zero baseline so
// increase()/absent()/rate() have a sample the instant the pod is first
// scraped — following the initWebhookMetrics() precedent (jira_webhook_deadman.go).
// Called once at bridge startup (gitlab-bridge/main.go).
func InitGitLabWebhookMetrics() {
	gitlabWebhookDeliveries.WithLabelValues(gitlabWebhookOutcomeProcessed).Add(0)
	gitlabWebhookDeliveries.WithLabelValues(gitlabWebhookOutcomeIgnored).Add(0)
	gitlabWebhookDeliveries.WithLabelValues(gitlabWebhookOutcomeTokenFailed).Add(0)
	gitlabWebhookDeliveries.WithLabelValues(gitlabWebhookOutcomeBadRequest).Add(0)
	gitlabWebhookDeliveries.WithLabelValues(gitlabWebhookOutcomeError).Add(0)
	initWebhookHandlingMetrics(webhookSourceGitLab,
		gitlabWebhookOutcomeProcessed, gitlabWebhookOutcomeIgnored,
		gitlabWebhookOutcomeTokenFailed, gitlabWebhookOutcomeBadRequest,
		gitlabWebhookOutcomeError)
}
