package deathreport

import (
	"fmt"
	"strconv"
	"strings"

	gardeniometrics "gardenio/controller/internal/metrics"
)

// DeathFamily is a coarse classification of WHY an agent stopped, derived from
// its close_reason. It is a SUPERSET of the involuntary-death families the
// metric emitter counts (gardeniometrics.InvoluntaryDeathFamily): that predicate
// answers "does this death increment gardenio_agent_involuntary_deaths_total?"
// and only recognises the stop-gate + context-cancelled shapes. The forensic
// report needs to classify EVERY terminal reason a human might be looking at —
// including graceful stops, idle-outs, reaper sweeps, OOM kills, and pod
// evictions — so the report can say "this was NOT an involuntary death, it was
// a clean idle-out" instead of a blank.
type DeathFamily string

const (
	// FamilyStopGateCooldown: the stop-gate cooldown debounce closed the bead
	// after the agent ended its turn (often parked on an operator decision)
	// inside the cooldown window. The canonical ka-0000002026 shape.
	FamilyStopGateCooldown DeathFamily = "stop_gate_cooldown"
	// FamilyStopGateEscapeHatch: the stop-gate escape hatch fired after
	// StopGateMaxBlocks consecutive turn-ends without `gb done`.
	FamilyStopGateEscapeHatch DeathFamily = "stop_gate_escape_hatch"
	// FamilyStopGateRateLimited: the stop-gate closed the bead while the agent
	// was rate-limited.
	FamilyStopGateRateLimited DeathFamily = "stop_gate_rate_limited"
	// FamilyContextCancelled: the pod was SIGTERM'd mid-turn before `gb done`,
	// orphaning a claimed task. The interruption SOURCE (node drain, rollout,
	// eviction, or a controller-initiated pod delete) is NOT recorded on this
	// close_reason — see the honest summary in ClassifyWithFields; the historical text that
	// asserted "Spot reclaim" was a false attribution on an all-on-demand cluster
	// (ka-0000002985 DEFECT 1).
	FamilyContextCancelled DeathFamily = "context_cancelled"
	// FamilyContextCancelledUnclaimed: pod SIGTERM'd mid-turn with work in
	// flight but no claimed task (C9, ka-0000001146).
	FamilyContextCancelledUnclaimed DeathFamily = "context_cancelled_unclaimed"
	// FamilyContextCancelledClaimLookupFailed: pod SIGTERM'd mid-turn while the
	// claimed-task lookup failed, so whether a claimed task was orphaned is
	// unknown (gd-TYjMaxCMZR). Mirrors
	// gardeniometrics.FamilyContextCancelledClaimLookupFailed.
	FamilyContextCancelledClaimLookupFailed DeathFamily = "context_cancelled_claim_lookup_failed"
	// FamilyCredentialFailure: the fields on the dead bead show the agent's
	// credential was rejected (error_category=unauthorized / error_detail=
	// authentication_failed), or it completed no successful turn at all (issued
	// requests but produced zero output tokens at zero cost). Derived from the
	// dead bead's FIELDS, not its close_reason — it OVERRIDES a generic
	// context_cancelled close_reason whose "interruption source not determined"
	// summary would otherwise send triage to the wrong place (ka-0000002985). It
	// is not a metric family: gardenio_agent_involuntary_deaths_total still counts
	// the death under whatever family the close_reason names.
	FamilyCredentialFailure DeathFamily = "credential_failure"
	// FamilyOOMKilled: the agent container was OOM-killed (K8s reason
	// OOMKilled). Not stamped on close_reason today — the status reporter emits
	// it only as a lifecycle metric — but recognised here so a transcript/pod
	// signal that carries it classifies precisely.
	FamilyOOMKilled DeathFamily = "oom_killed"
	// FamilyPodEvicted: the pod was evicted (node-pressure eviction, preemption).
	FamilyPodEvicted DeathFamily = "pod_evicted"
	// FamilyReaper: the stale-reaper closed the bead ("reaper: …" / "stale: …").
	FamilyReaper DeathFamily = "reaper"
	// FamilyTimeout: a watchdog/spawn timeout closed the bead ("timeout…",
	// "spawn expired before claim").
	FamilyTimeout DeathFamily = "timeout"
	// FamilyMaxRestarts: the restart loop gave up after max restarts.
	FamilyMaxRestarts DeathFamily = "max_restarts"
	// FamilyNotLoggedIn: the agent's credential broke (close_reason=not_logged_in).
	FamilyNotLoggedIn DeathFamily = "not_logged_in"
	// FamilyIdleOut: a clean idle-out (idle_ttl_expired, or a plain stop-gate
	// reason with no `_involuntary` marker — the agent had no work in flight).
	FamilyIdleOut DeathFamily = "idle_out"
	// FamilyGracefulStop: a voluntary, clean stop (`gb stop` / `gb done`).
	FamilyGracefulStop DeathFamily = "graceful_stop"
	// FamilyUpstreamAPIError: the agent died because the Claude upstream was
	// failing — a 5xx / overloaded / 529 / api_error / transient network drop
	// the in-place recovery could not outlast before the pod was torn down. This
	// is the "Claude was down" family (ka-0000000202, epic ka-0000000487): an
	// outage-driven death, INVOLUNTARY (work lost through no fault of the agent),
	// grouped separately so a post-incident query can tell a provider outage
	// apart from node churn (context_cancelled), a broken retry loop
	// (max_restarts), or a genuine bug (unknown/other) instead of having all
	// three collapse into the same misattributed bucket. Mirrors
	// gardeniometrics.FamilyUpstreamAPIError so the report and the metric agree.
	FamilyUpstreamAPIError DeathFamily = "upstream_api_error"
	// FamilyUnknown: no close_reason recorded, or a reason that matches nothing
	// known. A death we still want surfaced, never silently dropped.
	FamilyUnknown DeathFamily = "unknown"
)

// Classification is the full verdict on a terminal close_reason.
type Classification struct {
	// Family is the coarse death-family bucket.
	Family DeathFamily `json:"family"`
	// Involuntary reports whether this death would increment
	// gardenio_agent_involuntary_deaths_total — i.e. it is a mid-work kill, not a
	// clean exit. This is the SHARED predicate the metric emitter uses
	// (gardeniometrics.InvoluntaryDeathFamily), so the report and the metric can
	// never disagree about what counts as an involuntary death.
	Involuntary bool `json:"involuntary"`
	// MetricFamily is the bounded family label the metric emitter assigns
	// (stop_gate_cooldown / … / context_cancelled_unclaimed /
	// context_cancelled_claim_lookup_failed / other), or "" when
	// this death does not count as involuntary. Included so a report can be
	// cross-checked against the gardenio_agent_involuntary_deaths_total series.
	MetricFamily string `json:"metric_family,omitempty"`
	// Summary is a single human-readable sentence explaining the family.
	Summary string `json:"summary"`
}

// ClassifyWithFields maps a close_reason string, with the dead bead's fields
// (nil allowed), to its death family and involuntary-death verdict. It is the
// report's classification entry point.
//
// The involuntary verdict and MetricFamily come straight from the metric
// emitter's predicate (gardeniometrics.InvoluntaryDeathFamily) so the two can
// never drift. The coarse Family is then chosen to be maximally informative for
// a human: an involuntary death maps to its precise stop-gate / context-cancel
// family; a non-involuntary reason is further classified into idle-out, reaper,
// timeout, OOM, evict, graceful-stop, etc. rather than collapsed to "other".
//
// The fields let the verdict be corrected by EVIDENCE rather than resting on the
// close_reason's generic guess (ka-0000002985 DEFECT 1).
//
// The whole lesson of the 2026-08 incident: an agent was closed
// context_cancelled_involuntary — whose summary historically asserted "node
// drain, rollout, or Spot reclaim" — while the SAME bead carried
// error_category=unauthorized / error_detail=authentication_failed and a
// never-authenticated token shape (requests issued, zero output tokens, zero
// cost). The honest cause was an HTTP 401, sitting in the fields the whole time,
// on a cluster with zero Spot nodes. When the fields carry that signature it
// OVERRIDES the close_reason family, so the report names the credential failure
// instead of a plausible-but-false infra cause. When the fields do NOT support a
// cause, the summary says the interruption source was not determined rather than
// naming one.
//
// The Involuntary verdict and MetricFamily are still driven purely by the
// close_reason (the shared metric predicate), so the report and
// gardenio_agent_involuntary_deaths_total never disagree about what counts.
func ClassifyWithFields(closeReason string, fields map[string]string) Classification {
	reason := strings.TrimSpace(closeReason)
	lower := strings.ToLower(reason)

	metricFamily, involuntary := gardeniometrics.InvoluntaryDeathFamily(reason)

	c := Classification{Involuntary: involuntary, MetricFamily: metricFamily}

	// Evidence beats the generic guess. When the close_reason is a GENERIC
	// teardown (context_cancelled / context_cancelled_unclaimed /
	// context_cancelled_claim_lookup_failed — the families
	// whose interruption source is not recorded) but the fields carry a
	// credential-failure signature, override the family/summary to name the real
	// cause. We deliberately do NOT override a specific close_reason family
	// (stop-gate, upstream_api_error, rate_limit_livelock) — those already carry a
	// precise, evidenced cause.
	if authCause := AuthFailureCause(fields); authCause != "" &&
		(metricFamily == gardeniometrics.FamilyContextCancelled ||
			metricFamily == gardeniometrics.FamilyContextCancelledUnclaimed ||
			metricFamily == gardeniometrics.FamilyContextCancelledClaimLookupFailed) {
		c.Family = FamilyCredentialFailure
		c.Summary = authCause
		return c
	}

	// Involuntary mid-work deaths: adopt the metric emitter's precise family so
	// the report's family label matches the series label exactly.
	if involuntary {
		switch metricFamily {
		case gardeniometrics.FamilyStopGateCooldown:
			c.Family = FamilyStopGateCooldown
			c.Summary = "stop-gate cooldown killed the agent mid-work — it ended two turns inside one cooldown window and the debounce closed the bead. NOT a parked/awaiting-decision agent (the yield fast-path already exempts those), and NOT a hang: quiet turn-ends are ordinary for a working agent"
		case gardeniometrics.FamilyStopGateEscapeHatch:
			c.Family = FamilyStopGateEscapeHatch
			c.Summary = "stop-gate escape-hatch closed the agent mid-work after repeated turn-ends without `gb done`"
		case gardeniometrics.FamilyStopGateRateLimited:
			c.Family = FamilyStopGateRateLimited
			c.Summary = "stop-gate closed the agent mid-work while it was rate-limited"
		case gardeniometrics.FamilyContextCancelled:
			c.Family = FamilyContextCancelled
			c.Summary = contextCancelledSummary("leaving a claimed task orphaned")
		case gardeniometrics.FamilyContextCancelledUnclaimed:
			c.Family = FamilyContextCancelledUnclaimed
			c.Summary = contextCancelledSummary("with work in flight but no claimed task")
		case gardeniometrics.FamilyContextCancelledClaimLookupFailed:
			c.Family = FamilyContextCancelledClaimLookupFailed
			c.Summary = contextCancelledSummary("and the claimed-task lookup failed, so a claimed task may have been left orphaned")
		case gardeniometrics.FamilyUpstreamAPIError:
			c.Family = FamilyUpstreamAPIError
			c.Summary = "the Claude upstream was failing (5xx / overloaded / 529 / transient network drop) and the agent was torn down before it recovered — an outage-driven death, NOT a node-churn context-cancel or a broken retry loop"
		default: // metrics.FamilyOther — an `_involuntary` reason we don't otherwise recognise
			c.Family = classifyByKeyword(lower, FamilyUnknown)
			c.Summary = "involuntary mid-work death: " + reasonOrUnknown(reason)
		}
		return c
	}

	// Not counted as an involuntary death. Classify the terminal reason so the
	// report can positively state what it WAS (idle-out, reaper, graceful stop…)
	// rather than leave the operator guessing.
	c.Family = classifyByKeyword(lower, FamilyUnknown)
	c.Summary = nonInvoluntarySummary(c.Family, reason)
	return c
}

// classifyByKeyword picks a death family from substrings of a lower-cased
// close_reason. Used both for the metric emitter's FamilyOther fallthrough and
// for every non-involuntary reason. Ordered most-specific first. Returns
// fallback when nothing matches.
func classifyByKeyword(lower string, fallback DeathFamily) DeathFamily {
	switch {
	case lower == "":
		return FamilyUnknown
	case matchesUpstreamErrorShape(lower):
		// ORDERING HAZARD (ka-0000000202): this MUST precede the `timeout` and
		// `max_restarts` arms below. An outage-driven death is stamped
		// `upstream_api_error_involuntary: … after N restarts` / `… timed out`,
		// so the generic `restarts`/`timeout`/`expired` substrings would also
		// match — and since those arms come later, matching upstream FIRST is
		// what makes the specific attribution win over the incidental proximate
		// cause. Placed at the top (before oom/evict/reaper/idle too) because no
		// pod-kill reason shape carries an upstream substring, so there is no
		// competing match to lose to. TestClassify_UpstreamPrecedence pins this.
		return FamilyUpstreamAPIError
	case strings.Contains(lower, "oom") || strings.Contains(lower, "out of memory"):
		return FamilyOOMKilled
	case strings.Contains(lower, "evict") || strings.Contains(lower, "preempt"):
		return FamilyPodEvicted
	case strings.HasPrefix(lower, "reaper:") || strings.HasPrefix(lower, "stale:") ||
		strings.Contains(lower, "reaper") || strings.Contains(lower, "stranded"):
		return FamilyReaper
	case strings.Contains(lower, "idle_ttl") || strings.Contains(lower, "idle-out") ||
		strings.Contains(lower, "idled out") || strings.Contains(lower, "yielded_then_woken_then_idle"):
		return FamilyIdleOut
	case strings.Contains(lower, "timeout") || strings.Contains(lower, "expired"):
		return FamilyTimeout
	case strings.Contains(lower, "max_restarts"):
		return FamilyMaxRestarts
	case strings.Contains(lower, "not_logged_in"):
		return FamilyNotLoggedIn
	case strings.Contains(lower, "spot") || strings.Contains(lower, "drain") ||
		strings.Contains(lower, "context_cancelled"):
		return FamilyContextCancelled
	case strings.HasPrefix(lower, "stop_gate_"):
		// A plain stop-gate reason with NO `_involuntary` marker means the agent
		// had no work in flight — a clean checkpoint-less idle-out, not a kill.
		return FamilyIdleOut
	case isGracefulStop(lower):
		return FamilyGracefulStop
	default:
		return fallback
	}
}

// isGracefulStop reports whether a reason denotes a voluntary, clean exit.
func isGracefulStop(lower string) bool {
	switch lower {
	case "stop", "stop_requested", "wrapup_complete", "human_assigned", "done":
		return true
	}
	return false
}

// matchesUpstreamErrorShape reports whether a lower-cased close_reason carries a
// Claude-upstream-outage signature (5xx / overloaded / 529 / api_error /
// transient upstream network drop).
//
// KEEP IN SYNC with coop's classifier — coop/crates/cli/src/driver/error_category.rs
// (the `ErrorCategory::ServerError` arm, ~lines 280-297). coop is what decides,
// in the live agent pod, that an error IS an upstream outage (and drives the
// in-place server_error/no_internet recovery in cmd/gb/init_agent.go); this Go
// side is the POST-MORTEM classifier that must agree with that live verdict, or
// a death coop treated as an outage would be classified as something else here.
// The two substring sets are intentionally identical; if you add a shape to one,
// add it to the other. (error_category.rs carries the mirror-image reminder.)
func matchesUpstreamErrorShape(lower string) bool {
	for _, s := range upstreamErrorSubstrings {
		if strings.Contains(lower, s) {
			return true
		}
	}
	return false
}

// upstreamErrorSubstrings is the shared substring set — see matchesUpstreamErrorShape.
var upstreamErrorSubstrings = []string{
	"api_error",
	"overloaded_error",
	"overloaded",
	"internal_error",
	"internal server error",
	"server_error",
	"500",
	"502",
	"503",
	"529",
	"api error: terminated",
	"socket connection was closed",
	"connection was closed unexpectedly",
	"api error: connection error",
	"connection closed mid-response",
	"response above may be incomplete",
}

// nonInvoluntarySummary renders a one-sentence explanation for a death that does
// NOT count as involuntary.
func nonInvoluntarySummary(family DeathFamily, reason string) string {
	switch family {
	case FamilyIdleOut:
		return "clean idle-out — the agent had no work in flight when its turn ended or its idle-TTL expired; NOT an involuntary mid-work death"
	case FamilyReaper:
		return "the stale-reaper closed this bead after it sat inactive past the staleness threshold: " + reasonOrUnknown(reason)
	case FamilyTimeout:
		return "a watchdog/spawn timeout closed the bead: " + reasonOrUnknown(reason)
	case FamilyOOMKilled:
		return "the agent container was OOM-killed (K8s OOMKilled)"
	case FamilyPodEvicted:
		return "the pod was evicted (node-pressure eviction or preemption)"
	case FamilyMaxRestarts:
		return "the restart loop gave up after hitting the max-restart ceiling"
	case FamilyNotLoggedIn:
		return "the agent's credential broke (not_logged_in) and it could not continue"
	case FamilyGracefulStop:
		return "voluntary, clean stop (`gb stop` / `gb done`) — NOT an involuntary death"
	case FamilyUpstreamAPIError:
		return "the Claude upstream was failing (5xx / overloaded / 529 / transient network drop) when this agent was torn down — an outage-driven death: " + reasonOrUnknown(reason)
	default:
		if reason == "" {
			return "no close_reason was recorded; the death family could not be determined from the reason alone"
		}
		return "terminal reason did not match any known death family: " + reason
	}
}

func reasonOrUnknown(reason string) string {
	if reason == "" {
		return "(no close_reason recorded)"
	}
	return reason
}

// contextCancelledSummary renders the honest one-line cause for a generic
// mid-turn teardown. The interruption SOURCE is not recorded on the close_reason
// — the pod just observed its context cancel — so we say so plainly and list the
// possibilities as possibilities, rather than asserting one as fact. The old
// text hard-named "node drain, rollout, or Spot reclaim", which sent triage to
// AWS/Karpenter for deaths that were really a rejected credential or a
// controller-initiated pod delete, on a cluster with zero Spot nodes
// (ka-0000002985 DEFECT 1). tail describes the task state (orphaned vs no claim).
func contextCancelledSummary(tail string) string {
	return "pod was SIGTERM'd mid-turn before `gb done`, " + tail +
		" — interruption source not determined (a node drain, rollout, node-pressure eviction, or a controller-initiated pod delete are all possible; the close_reason does not record which, and no credential-failure signature was present in the fields)"
}

// Dead-bead field keys the classifier reads to derive the TRUE cause from
// evidence. total_* are coop's cumulative per-session accounting, stamped on the
// agent bead; error_category / error_detail are coop's live upstream-error
// verdict, stamped by the terminal-auth give-up path (init_agent.go) and the
// status reporter.
const (
	fieldErrorCategory     = "error_category"
	fieldErrorDetail       = "error_detail"
	fieldTotalOutputTokens = "total_output_tokens"
	fieldTotalRequestCount = "total_request_count"
	fieldTotalCostUSD      = "total_cost_usd"
)

// AuthFailureCause returns an honest one-line credential-failure cause when the
// dead bead's fields carry the auth-failure signature, and "" otherwise. It is
// the shared field-based deriver used by both the forensic report
// (ClassifyWithFields) and the Slack death notice (bridge.deathCause), so the two
// name the same cause from the same evidence.
//
// Two independent signals, either of which is sufficient, and which corroborate
// each other when both present:
//
//   - EXPLICIT: error_category=unauthorized OR error_detail=authentication_failed
//     — coop's own live verdict that the credential was rejected (an HTTP 401/403
//     key ACL denial). This is ground truth.
//   - NEVER-AUTHENTICATED token shape: the agent issued requests
//     (total_request_count>0) but produced zero output tokens
//     (total_output_tokens==0) at zero cost (total_cost_usd==0). A successful
//     turn always produces output tokens and costs money, so this shape means
//     every request was rejected before doing any work — no productive work was
//     interrupted, so a mid-work infra-interruption story is wrong.
//
// The token shape alone does not prove the failure was specifically auth (a total
// upstream outage looks the same), so with only that signal the cause says "no
// successful turn" rather than naming a credential; the explicit category is what
// lets it name the credential.
func AuthFailureCause(fields map[string]string) string {
	if fields == nil {
		return ""
	}
	cat := strings.TrimSpace(strings.ToLower(fields[fieldErrorCategory]))
	detail := strings.TrimSpace(strings.ToLower(fields[fieldErrorDetail]))
	explicit := cat == gardeniometrics.CategoryUnauthorized || detail == "authentication_failed"

	requests, haveReq := atoiField(fields[fieldTotalRequestCount])
	outputTokens, haveTok := atoiField(fields[fieldTotalOutputTokens])
	cost, haveCost := atofField(fields[fieldTotalCostUSD])
	neverAuthenticated := haveReq && haveTok && haveCost &&
		requests > 0 && outputTokens == 0 && cost == 0

	switch {
	case explicit && neverAuthenticated:
		return fmt.Sprintf("the agent's credential was rejected (error_category=%s / %s) and it completed no successful turn — %d request(s) issued, 0 output tokens, $0 spent. This is a CREDENTIAL failure, not an infra interruption",
			catOrUnknown(cat), detailOrUnknown(detail), requests)
	case explicit:
		return fmt.Sprintf("the agent's credential was rejected (error_category=%s / %s) — a CREDENTIAL failure, not an infra interruption",
			catOrUnknown(cat), detailOrUnknown(detail))
	case neverAuthenticated:
		return fmt.Sprintf("the agent issued %d request(s) but produced 0 output tokens at $0 — it completed no successful turn, so no productive work was interrupted; consistent with a rejected credential or a total upstream outage rather than a mid-work infra interruption",
			requests)
	default:
		return ""
	}
}

func catOrUnknown(cat string) string {
	if cat == "" {
		return "unauthorized"
	}
	return cat
}

func detailOrUnknown(detail string) string {
	if detail == "" {
		return "authentication_failed"
	}
	return detail
}

// atoiField parses an integer bead field, reporting whether it was present and
// well-formed. A missing or malformed field returns (0,false) so the caller does
// not mistake "absent" for "zero" — critical for the never-authenticated shape,
// which must not fire on a bead that simply never recorded token accounting.
func atoiField(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

// atofField parses a float bead field (total_cost_usd), reporting presence.
func atofField(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}
