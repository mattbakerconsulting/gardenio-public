package metrics

import (
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// EventMetrics holds the event-driven controller metrics added by E2 thread 2
// (timeseries platform plan §2.E2, beads ka-0000003837 / ka-0000007731 /
// ka-0000002051). Unlike the poller-backed Collector gauges in metrics.go,
// these are monotonic counters and a histogram driven by real agent-lifecycle
// transitions — the controller increments them as events flow through
// handleEvent (spawn/done/stop/stuck) and statusreporter.SyncAll (pod phase
// changes + OOM detection). They are served on the same controller :9092
// /metrics endpoint via Register().
//
// Counters are NEVER Reset() — resetting a counter is a Prometheus
// anti-pattern that breaks rate()/increase() (those functions interpret a drop
// as a counter reset and double-count). The poller gauges in metrics.go reset
// each scrape because a departed agent's gauge must disappear; these counters
// instead accumulate for the process lifetime and rely on rate()/increase()
// windows downstream.
type EventMetrics struct {
	// env is baked in at construction (GARDENIO_ENV). It is the cross-estate
	// coarse join key the FSM correlation contract mandates
	// (FSM_CORRELATION_CONTRACT.md §1.1) and is attached to every lifecycle
	// event series so the composite OOM↔ECS alert can join on it.
	env string

	// lifecycleEvents counts agent lifecycle transitions by kind. The
	// label set {event, env, project, role, session_id} is bound by the FSM
	// composite alert (gardenio/helm/gardenio/templates/fsm-composite-alert.yaml
	// + tests/fsm_composite_alert_rules_test.yaml), which reads
	// gardenio_agent_lifecycle_events_total{event="oom_killed"} grouped by
	// (env, project) and drills into session_id for the S3 session-recording
	// deep link. session_id is populated ONLY for oom_killed (the one event
	// that needs the drill-down); every other event carries session_id="" so
	// the per-(event,env,project,role) cardinality stays bounded — session ids
	// are high-cardinality and short-lived (catalog §3.5), so attaching one to
	// every spawn/stop would explode the series count.
	lifecycleEvents *prometheus.CounterVec

	// podPhaseTransitions counts K8s pod phase changes observed by the status
	// reporter, e.g. Pending→Running, Running→Succeeded, Running→Failed. Labels
	// {phase_from, phase_to, project, role} (catalog: controller
	// internal/statusreporter, on pod phase change).
	podPhaseTransitions *prometheus.CounterVec

	// podsPending is the live count of agent pods currently in K8s phase
	// Pending, by {project, role}. Unlike its sibling counters above, this is a
	// GAUGE — a current-state snapshot the status reporter re-derives on every
	// pod-phase sweep (statusreporter.SyncAll). It exists because the incident-
	// direct GAP-1 alert AgentPodsPendingTooLong (ka-0000005418, epic
	// ka-0000000770) needs "how many pods are stuck Pending RIGHT NOW", which
	// gardenio_agent_pod_phase_transitions_total (a transition counter) cannot
	// answer, and kube_pod_status_phase (KSM) is NOT scraped by the in-chart
	// vmagent on new prod — that vmagent scrapes only annotated gardenio-system
	// pods, so a controller-emitted gauge is the only source. SetPodsPending
	// Reset()s the whole vector each sweep so a (project,role) that no longer
	// has Pending pods disappears and sum(gardenio_agent_pods_pending) falls back
	// to no-data (alert clears). Resetting a gauge is safe — it is the poller-
	// gauge pattern in metrics.go, not the counter anti-pattern the struct doc
	// warns about (that warning is about rate()/increase() over counters).
	podsPending *prometheus.GaugeVec

	// sessionDuration observes the wall-clock lifetime of an agent session,
	// recorded once on session close (bead closed). Labels {project, role,
	// outcome}. Buckets span a few seconds (fast jobs) to a day (long-lived
	// gardeners): 5s, 30s, 1m, 5m, 15m, 1h, 4h, 12h, 1d (catalog buckets plus a
	// low 5s bucket for the sub-30s tail the bead calls out).
	sessionDuration *prometheus.HistogramVec

	// involuntaryDeaths counts agent sessions that closed with an INVOLUNTARY
	// mid-work death — the "killed mid-work" class Matt asked to stop and chart
	// (ka-0000008189). The signal is the agent bead's close_reason carrying the
	// `_involuntary` marker that hook.go / init_agent_loop.go stamp when Claude
	// exited (stop-gate cooldown/escape_hatch/rate_limited) or the pod was
	// SIGTERM'd (context_cancelled) while a claimed task was still in flight —
	// plus the context_cancelled_unclaimed reason (no marker) for a SIGTERM'd
	// agent that had done work but held no claimed issue (C9, ka-0000001146),
	// and the context_cancelled_claim_lookup_failed reason (no marker) for a
	// SIGTERM'd agent whose claimed-task lookup failed (gd-TYjMaxCMZR).
	//
	// This is a DISTINCT signal from sessionDuration's `outcome` label, which is
	// derived purely from the SSE close event TYPE (AgentDone→done,
	// AgentKill→failed, AgentStop→stopped) and therefore mislabels a stop-gate
	// involuntary close — which arrives as AgentDone — as outcome="done". A
	// working agent killed mid-turn was invisible in the charts until this
	// counter; the whole point is a series that goes UP on every kill so its
	// rate()/increase() proves the regression is (or is not) fixed.
	//
	// Labels {family, env, project, role}: family is the bounded close-reason
	// class (stop_gate_cooldown / stop_gate_escape_hatch / stop_gate_rate_limited
	// / context_cancelled / context_cancelled_unclaimed /
	// context_cancelled_claim_lookup_failed / other), env is the
	// cross-estate join key. No session_id (high-cardinality; the agent bead id
	// is the drill-down, found by close_reason in the daemon).
	involuntaryDeaths *prometheus.CounterVec

	// orphanSessions is the session reaper's per-cycle tally of OPEN session
	// beads whose agent bead is closed or gone (gd-c6muILNedN). phase="found" is
	// how many the cycle saw before it closed any; phase="left" is how many were
	// still open after it. In steady state "found" is the number of sessions
	// the in-pod gb close missed in one interval (wall-clock end, kill, SIGKILL
	// mid-close) and "left" is 0. A "left" above 0 that holds means the reaper
	// cannot close them (daemon rejection, per-cycle cap): the backlog that grew
	// to 3,577 unseen before this gauge existed.
	orphanSessions *prometheus.GaugeVec

	// upstreamErrorActive is the CURRENT-STATE count of agents sitting on an
	// upstream provider error, by {category,project,role} — the "Claude is
	// failing us right now" signal (leg A of the outage detector, epic
	// ka-0000000487). SetUpstreamErrorActive Reset()s the whole vector each
	// sweep so a category that has recovered DISAPPEARS rather than lingering
	// at a stale value; same poller-gauge idiom as podsPending above.
	//
	// A gauge, not just a counter, because the status reporter OBSERVES this
	// state on a fixed poll cadence: one agent stuck on one error screen for
	// ten minutes is seen erroring on every sweep, so a counter incremented per
	// observation would measure "how long agents were broken × poll rate", not
	// how many errors happened. The level-triggered gauge is immune to that
	// poll-rate skew, and "N agents are erroring right now" is exactly what the
	// alert needs to say.
	upstreamErrorActive *prometheus.GaugeVec

	// landSpawnToWorking observes, once per tenant-land agent, the seconds from
	// its agent bead's creation to the first time the status reporter writes
	// agent_state=working for it. Labels {land}: the tenant land (= project),
	// bounded by the number of lands. It is the Lands SLI behind the
	// tenant_spawn_to_working objective (chart
	// agents.metrics.prometheusRule.landsSLO, Track B §5.4, gd-Vuu1J7y4J0),
	// which reads the 300s bucket.
	landSpawnToWorking *prometheus.HistogramVec

	// upstreamErrorTransitions counts EDGE-triggered entries into an upstream
	// error category — incremented once when an agent first enters the state,
	// not once per observation. The independent second derivation of the same
	// quantity as upstreamErrorActive (advice ka-0000005924): if both are
	// correct they reconstruct each other, and a persistent divergence is a bug
	// in one of them. Sustained gauge > 0 with a flat counter means the
	// transition detector is broken; a climbing counter with a zero gauge means
	// agents are recovering fast (healthy, not alertable).
	upstreamErrorTransitions *prometheus.CounterVec

	// sessionExitCode counts coop process exit codes observed at SESSION bead
	// close (one increment per coop restart-loop iteration, not per agent
	// lifetime — distinct from involuntaryDeaths, which is keyed off the AGENT
	// bead's close_reason and fires once per agent death). This is the
	// queryable-over-time form of the "143/10/130 split" that previously
	// required a manual scan of the bead list (gd-5MTDWjwNjm). Labels
	// {code,project,role}; code is the bounded sessionExitCodeValid() enum so a
	// long tail of rare exit codes cannot inflate cardinality.
	sessionExitCode *prometheus.CounterVec

	// accountLayer counts, once per genuine spawn attempt, WHICH layer of
	// applyAccountConfig produced (or refused) the pod's LLM account — the Phase 0
	// instrumentation of the account-fallback-chain deprecation (ka-0000002142,
	// epic ka-0000007694). Labels {layer,project,role}; layer is the bounded
	// AccountLayer* enum below. Incremented at the spawn-decision sites (the event
	// door + the reconciler backstop) from spec.AccountLayer, so it counts one
	// series per pod the controller actually decided to create — NOT the throwaway
	// spec builds the upgrade-diff path makes.
	//
	// Its whole purpose was to prove empirically that the project-cache and
	// DefaultAccount/redirect layers essentially never win on prod
	// (layer="project-cache"/"default-account"/"redirect-changed" stay ~0 over a
	// few days), which is the evidence that de-risked Phases 2–3 of the deprecation
	// (docs/account-routing.md §7 Phase 0). Those layers are now removed (Phase 3,
	// ka-0000007959 dropped the default-account layer); the remaining series are no
	// longer stamped. No behavior change — recording only.
	accountLayer *prometheus.CounterVec
}

// Account-layer enum (Phase 0 instrumentation, ka-0000002142, epic
// ka-0000007694). Each constant names exactly ONE exit of applyAccountConfig
// (cmd/controller/podspec.go) — the layer that produced (or refused) a pod's
// account. Stamped on AgentPodSpec.AccountLayer at resolution time and counted
// as gardenio_agent_account_layer_total at the spawn-decision site. Bounded
// enum: RecordAccountLayer drops anything outside it, so a future caller cannot
// inflate the metric's cardinality (mirrors the Lifecycle*/Category* enums above).
const (
	// AccountLayerRolePin: resolveRoleAccount pinned the account by role
	// (layer 1, e.g. fsm-triage -> litellm-bedrock). Wins over spread.
	AccountLayerRolePin = "role-pin"
	// AccountLayerSpread: selectAccount picked from the project's healthy
	// pool by live-pod count (layer 2, the common path, ka-0000004861).
	AccountLayerSpread = "spread"
	// AccountLayerRolePinSpill: a role-pinned account was capacity-exhausted at
	// spawn time, so instead of parking the agent queued indefinitely, the
	// resolver SPILLED to the project's healthy spread pool and baked a healthy
	// account (ka-0000001307). Only fires for a non-exclusive role pin whose role
	// does not set no_account_spill; the pin's chain being exhausted while the
	// project has other healthy accounts is the release-lane stall this prevents.
	AccountLayerRolePinSpill = "role-pin-spill"
	// AccountLayerOpusFallback: the requested model group's pool was fully gated
	// but an opus-capable account had capacity, so fallbackModelGroupAccount
	// landed there and downgraded the model (ka-0000003806, decision ka-0000003299).
	AccountLayerOpusFallback = "opus-fallback"
	// AccountLayerProjectCache: the empty-pool fall-through onto
	// cfg.ProjectCache[project].Account (layer 3). Phase 2 removes this reader;
	// Phase 0 proves it essentially never wins. Retained as a historical-series
	// enum member; no longer stamped.
	AccountLayerProjectCache = "project-cache"
	// Layer 4 (the cfg.DefaultAccount fall-through, formerly "default-account")
	// was removed in account-routing.md §7 Phase 3 (ka-0000007959): the config,
	// env, and enum member are gone. It was unset in prod and Phase 0 telemetry
	// proved the layer never won.
	//
	// AccountLayerRedirectChanged: resolveNonExhaustedAccount walked the fallback
	// chain and redirected to a DIFFERENT account than the one first resolved
	// (layer 5, the exhausted-account guard actually changed the outcome).
	AccountLayerRedirectChanged = "redirect-changed"
	// AccountLayerEscapeHatchBlocked: the whole pool was capacity-exhausted, so
	// the escape hatch set SpawnBlocked and baked no credential (ka-0000002560).
	AccountLayerEscapeHatchBlocked = "escape-hatch-blocked"
	// AccountLayerWhitelistBlocked: the finally-resolved account had no overlap
	// with the role's account_whitelist, so no credential was baked (fail-closed,
	// ka-0000003061).
	AccountLayerWhitelistBlocked = "whitelist-blocked"
	// AccountLayerNoCredentialWarn: no account resolved, but the project DOES have
	// credentialed account beads — the loud WARN case (ka-0000007343). Pods spawn
	// with no ANTHROPIC_* env.
	AccountLayerNoCredentialWarn = "no-credential-warn"
	// AccountLayerNoCredentialBenign: no account resolved and the project has no
	// account beads at all — the benign direct-provider-auth case (Debug log).
	AccountLayerNoCredentialBenign = "no-credential-benign"
	// AccountLayerUnknownAccount: an account name resolved but is absent from
	// cfg.AccountCache — a config drift the function warns on and bakes nothing.
	AccountLayerUnknownAccount = "unknown-account"
	// AccountLayerUnlabeledPin: no account resolved and no account bead carries
	// project:<name>, BUT the project bead pins an `account:` that DOES exist in
	// cfg.AccountCache — dead config, not direct-provider auth (ka-0000006314).
	// Phase 2 (ka-0000004094) retired the layer-3 reader of that field, so the pin
	// stopped resolving and pods spawned with NO ANTHROPIC_* env under the benign
	// Debug branch. Split out from AccountLayerNoCredentialBenign so the condition
	// is countable and warned rather than invisible; the fix is to add the
	// project:<name> label to the pinned account.
	AccountLayerUnlabeledPin = "unlabeled-pin"
)

func accountLayerValid(layer string) bool {
	switch layer {
	case AccountLayerRolePin, AccountLayerSpread, AccountLayerRolePinSpill,
		AccountLayerOpusFallback,
		AccountLayerProjectCache,
		AccountLayerRedirectChanged, AccountLayerEscapeHatchBlocked,
		AccountLayerWhitelistBlocked, AccountLayerNoCredentialWarn,
		AccountLayerNoCredentialBenign, AccountLayerUnknownAccount,
		AccountLayerUnlabeledPin:
		return true
	default:
		return false
	}
}

// Upstream provider error-category enum — the bounded set allowed as the
// `category` label on the upstream-error metrics. These mirror coop's
// ErrorCategory::as_str() wire strings (coop/crates/cli/src/driver/
// error_category.rs), which is what reaches the controller on coop's
// GET /api/v1/agent as `error_category`.
//
// Bounded deliberately: the sibling `error_detail` field is unbounded
// free-text provider prose and must NEVER become a label. An unrecognised
// category collapses to CategoryOther so a coop-side addition cannot silently
// inflate cardinality — keep this list in sync when coop gains a category.
const (
	CategoryServerError      = "server_error"
	CategoryRateLimited      = "rate_limited"
	CategoryModelRateLimited = "model_rate_limited"
	CategoryNoInternet       = "no_internet"
	CategoryOutOfCredits     = "out_of_credits"
	CategoryBudgetExceeded   = "budget_exceeded"
	CategoryUnauthorized     = "unauthorized"
	CategoryOther            = "other"
)

// NormalizeUpstreamErrorCategory maps a coop-reported error_category onto the
// bounded enum above, collapsing anything unrecognised (including a novel
// coop-side category) to CategoryOther. Empty input returns "" so callers can
// distinguish "no error" from "an error we could not name".
func NormalizeUpstreamErrorCategory(category string) string {
	switch category {
	case "":
		return ""
	case CategoryServerError, CategoryRateLimited, CategoryModelRateLimited,
		CategoryNoInternet, CategoryOutOfCredits, CategoryBudgetExceeded,
		CategoryUnauthorized, CategoryOther:
		return category
	default:
		return CategoryOther
	}
}

// Lifecycle event-name enum. The only values allowed as the `event` label —
// RecordLifecycleEvent drops anything outside this set so a future caller
// cannot inflate cardinality. Mirrors the catalog enum
// (timeseries-metrics-catalog.md §gardenio_agent_lifecycle_events_total).
const (
	LifecycleSpawned   = "spawned"
	LifecycleStarted   = "started"
	LifecycleStopped   = "stopped"
	LifecycleOOMKilled = "oom_killed"
	LifecycleStuck     = "stuck"
	LifecycleDrained   = "drained"
	LifecycleDespawned = "despawned"
)

func lifecycleEventValid(e string) bool {
	switch e {
	case LifecycleSpawned, LifecycleStarted, LifecycleStopped,
		LifecycleOOMKilled, LifecycleStuck, LifecycleDrained, LifecycleDespawned:
		return true
	default:
		return false
	}
}

// Session-close outcome enum — the only values allowed as the `outcome` label
// on sessionDuration. RecordSessionDuration drops anything else.
const (
	OutcomeDone    = "done"
	OutcomeFailed  = "failed"
	OutcomeStopped = "stopped"
)

func sessionOutcomeValid(o string) bool {
	switch o {
	case OutcomeDone, OutcomeFailed, OutcomeStopped:
		return true
	default:
		return false
	}
}

// Session exit-code label enum — the only values allowed as the `code` label
// on sessionExitCode. Mirrors the coop process exit-code convention already
// used by describeCoopExitCode (gardenio/controller/cmd/gb/init_agent_loop.go)
// and beadsapi.SessionStatusForExit: 0=clean, 10=credential switch
// (beadsapi.ExitCodeCredentialSwitch), 143=128+SIGTERM, 130=128+SIGINT,
// 129=128+SIGHUP, 137=128+SIGKILL (the kernel OOM-killer's signal — see
// restartCauseOther's oomkill note). Anything outside this fixed set
// normalizes to "other" so a long tail of rare/unexpected exit codes cannot
// inflate the series count.
const (
	ExitCodeLabelClean            = "0"
	ExitCodeLabelCredentialSwitch = "10"
	ExitCodeLabelSIGHUP           = "129"
	ExitCodeLabelSIGINT           = "130"
	ExitCodeLabelSIGTERM          = "143"
	ExitCodeLabelSIGKILL          = "137"
	ExitCodeLabelOther            = "other"
)

// normalizeSessionExitCode maps a raw coop process exit code to its bounded
// label value.
func normalizeSessionExitCode(exitCode int) string {
	switch exitCode {
	case 0:
		return ExitCodeLabelClean
	case 10:
		return ExitCodeLabelCredentialSwitch
	case 129:
		return ExitCodeLabelSIGHUP
	case 130:
		return ExitCodeLabelSIGINT
	case 137:
		return ExitCodeLabelSIGKILL
	case 143:
		return ExitCodeLabelSIGTERM
	default:
		return ExitCodeLabelOther
	}
}

// Involuntary-death family enum — the bounded set of values allowed as the
// `family` label on involuntaryDeaths. Each maps to a close_reason prefix the
// stop-gate (hook.go) or the SIGTERM restart-loop path (init_agent_loop.go)
// stamps when an agent dies mid-work. Keeping it a fixed enum bounds the series
// cardinality; a close_reason that carries `_involuntary` but matches none of
// the known prefixes falls to FamilyOther so a novel kill shape still counts.
const (
	// FamilyStopGateCooldown: Claude exited without `gb done` and the stop gate
	// closed the bead on the cooldown path (account rotation / churn) — the
	// canonical ka-0000004720 shape.
	FamilyStopGateCooldown = "stop_gate_cooldown"
	// FamilyStopGateEscapeHatch: stop gate escape-hatch close path.
	FamilyStopGateEscapeHatch = "stop_gate_escape_hatch"
	// FamilyStopGateRateLimited: stop gate rate-limited close path.
	FamilyStopGateRateLimited = "stop_gate_rate_limited"
	// FamilyContextCancelled: pod SIGTERM'd mid-turn (node drain / rollout /
	// Spot reclaim) before `gb done`.
	FamilyContextCancelled = "context_cancelled"
	// FamilyContextCancelledUnclaimed: pod SIGTERM'd while the agent bead was
	// still open with tool calls recorded but NO claimed issue task (C9,
	// ka-0000001146). Deliberately a SEPARATE family — not a widening of the
	// `_involuntary` marker — so FamilyContextCancelled keeps its ka-0000008254
	// precision ("left a claimed issue orphaned") and the node-reclaim sentinel
	// that exact-matches it stays unpolluted. This is the one counted family
	// whose close_reason does NOT carry `_involuntary`. It is an upper bound on
	// taskless mid-turn kills: total_tool_calls is bead-lifetime cumulative, so
	// a warm-parked thread agent SIGTERM'd between turns lands here too.
	FamilyContextCancelledUnclaimed = "context_cancelled_unclaimed"
	// FamilyContextCancelledClaimLookupFailed: pod SIGTERM'd while the
	// claimed-task lookup (ListAssignedTask) failed every retry, so the teardown
	// could neither reopen a claim nor confirm there was none (gd-k3kzZeIlT0,
	// gd-TYjMaxCMZR). Like FamilyContextCancelledUnclaimed its close_reason does
	// NOT carry `_involuntary`, and it is a SEPARATE family for the same reason:
	// the node-reclaim sentinel exact-matches context_cancelled. It must count —
	// this is the shape that orphaned real claimed work (gd-1802PlhMZG), and
	// before it had its own reason it counted as context_cancelled_unclaimed.
	FamilyContextCancelledClaimLookupFailed = "context_cancelled_claim_lookup_failed"
	// FamilyUpstreamAPIError: the agent died because the Claude upstream was
	// failing — a 5xx / overloaded / 529 / api_error / transient network drop
	// that the in-place recovery (init_agent.go server_error / no_internet
	// branches) could not outlast before the pod was SIGTERM'd or the restart
	// loop gave up (ka-0000000202, epic ka-0000000487). Its close_reason is
	// stamped `upstream_api_error_involuntary: …` by init_agent_loop.go when a
	// teardown fires while an upstream outage was still active. Deliberately a
	// SEPARATE family so a post-incident "what killed the fleet" query can tell
	// "Claude was down" apart from context_cancelled (node churn) / other (a
	// novel bug) / a broken retry loop — the same misattribution the plain
	// context_cancelled + max_restarts families used to absorb.
	FamilyUpstreamAPIError = "upstream_api_error"
	// FamilyRateLimitLivelock: the agent gave up after monitorExit parked on the
	// SAME account/model rate limit rateLimitLivelockBound times in a row with no
	// successful turn in between — an account-monthly-spend cap that no re-inject,
	// backoff, or LiteLLM fallback arm can clear (ka-0000001209, the deferred
	// harness-side bound of ka-0000007458). init_agent.go's
	// failAgentRateLimitLivelock stamps its close_reason
	// `rate_limit_livelock_involuntary: …` and requeues the claimed task.
	// Deliberately a SEPARATE family from upstream_api_error (a provider outage)
	// so a post-incident query can tell "we hit our own spend cap" — an
	// operator/budget signal — apart from "Claude was down". It IS involuntary
	// (work lost through no fault of the agent), so it counts here.
	FamilyRateLimitLivelock = "rate_limit_livelock"
	// FamilyOther: close_reason carries `_involuntary` but matches no known
	// prefix — a novel kill shape we still want counted, not dropped.
	FamilyOther = "other"
)

// InvoluntaryDeathFamily classifies a close_reason string into its bounded
// death family, or returns ("", false) when the reason does NOT denote an
// involuntary mid-work death (a clean idle-out, a graceful stop, a genuine
// crash). The primary discriminator is the `_involuntary` marker hook.go /
// init_agent_loop.go stamp ONLY when the agent had real work in flight; a
// zero-work idle keeps the plain reason and is correctly not counted here.
// The two exceptions are FamilyContextCancelledUnclaimed and
// FamilyContextCancelledClaimLookupFailed, whose close_reasons deliberately
// omit the marker (see the enum doc) but are still counted.
//
// Exported because it is the SHARED definition of "counts as an involuntary
// death": the controller's emit path (RecordInvoluntaryDeath) and the P9
// interrupt-parity x-check's bead scan (xcheck_sweep.go side B) must agree on
// exactly this predicate, or P9 diverges by construction.
func InvoluntaryDeathFamily(closeReason string) (string, bool) {
	if strings.HasPrefix(closeReason, FamilyContextCancelledUnclaimed) {
		return FamilyContextCancelledUnclaimed, true
	}
	if strings.HasPrefix(closeReason, FamilyContextCancelledClaimLookupFailed) {
		return FamilyContextCancelledClaimLookupFailed, true
	}
	if !strings.Contains(closeReason, "_involuntary") {
		return "", false
	}
	switch {
	case strings.HasPrefix(closeReason, FamilyStopGateCooldown+"_involuntary"):
		return FamilyStopGateCooldown, true
	case strings.HasPrefix(closeReason, FamilyStopGateEscapeHatch+"_involuntary"):
		return FamilyStopGateEscapeHatch, true
	case strings.HasPrefix(closeReason, FamilyStopGateRateLimited+"_involuntary"):
		return FamilyStopGateRateLimited, true
	case strings.HasPrefix(closeReason, FamilyContextCancelled+"_involuntary"):
		return FamilyContextCancelled, true
	case strings.HasPrefix(closeReason, FamilyUpstreamAPIError+"_involuntary"):
		// An outage-driven death: work was lost through no fault of the agent
		// (the Claude upstream was returning 5xx/overloaded), so it IS an
		// involuntary mid-work death and counts here. init_agent_loop.go stamps
		// this prefix in place of the generic context_cancelled/max_restarts
		// reason when a teardown fires while an upstream outage was still active.
		return FamilyUpstreamAPIError, true
	case strings.HasPrefix(closeReason, FamilyRateLimitLivelock+"_involuntary"):
		// A rate-limit livelock give-up (ka-0000001209): monitorExit parked on the
		// same account/model spend cap past the bound and failed the agent after
		// requeuing its task. Work was lost through no fault of the agent, so it
		// counts as an involuntary death under its own family.
		return FamilyRateLimitLivelock, true
	default:
		return FamilyOther, true
	}
}

// NewEventMetrics constructs the event-driven metric vectors. env is the
// deploy environment (GARDENIO_ENV), attached to every lifecycle series as the
// cross-estate join key; an empty env still emits (the label is just "").
func NewEventMetrics(env string) *EventMetrics {
	return &EventMetrics{
		env: env,

		lifecycleEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "agent_lifecycle_events_total",
			Help:      "Agent lifecycle transitions by kind (enum: spawned/started/stopped/oom_killed/stuck/drained/despawned). Labels {event,env,project,role,session_id}; session_id is populated only for oom_killed (FSM composite-alert drill-down, FSM_CORRELATION_CONTRACT §5a). env is the cross-estate join key.",
		}, []string{"event", "env", "project", "role", "session_id"}),

		podPhaseTransitions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "agent_pod_phase_transitions_total",
			Help:      "K8s pod phase changes for agent pods (e.g. Pending->Running, Running->Failed), observed by the status reporter. Labels {phase_from,phase_to,project,role}.",
		}, []string{"phase_from", "phase_to", "project", "role"}),

		podsPending: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "agent_pods_pending",
			Help:      "Live count of agent pods currently in K8s phase=Pending, by {project,role}, re-derived on every controller pod-phase sweep. Current-state gauge backing the GAP-1 AgentPodsPendingTooLong alert (sum(gardenio_agent_pods_pending) > 0 for 10m); the incident-direct signal for agent-spawn stalls that gardenio_agent_pod_phase_transitions_total (a counter) cannot express.",
		}, []string{"project", "role"}),

		sessionDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "agent_session_duration_seconds",
			Help:      "Wall-clock duration of an agent session, recorded on session close. Labels {project,role,outcome}.",
			// 5s, 30s, 1m, 5m, 15m, 1h, 4h, 12h, 1d.
			Buckets: []float64{5, 30, 60, 300, 900, 3600, 14400, 43200, 86400},
		}, []string{"project", "role", "outcome"}),

		involuntaryDeaths: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "agent_involuntary_deaths_total",
			Help:      "Agent sessions that closed with an involuntary mid-work death (close_reason carries the `_involuntary` marker stamped when Claude exited or the pod was SIGTERM'd while a claimed task was in flight, or the context_cancelled_unclaimed prefix for a SIGTERM'd busy-but-taskless agent, or the context_cancelled_claim_lookup_failed prefix for a SIGTERM'd agent whose claimed-task lookup failed). Labels {family,env,project,role}; family enum: stop_gate_cooldown/stop_gate_escape_hatch/stop_gate_rate_limited/context_cancelled/context_cancelled_unclaimed/context_cancelled_claim_lookup_failed/upstream_api_error/rate_limit_livelock/other. This is the 'killed mid-work' signal (ka-0000008189) — its rate()/increase() proves the regression is fixed; slice family=\"upstream_api_error\" to see outage-driven deaths distinctly (ka-0000000487), or family=\"rate_limit_livelock\" to see agents that gave up on an account monthly-spend cap (ka-0000001209). Distinct from agent_session_duration_seconds outcome, which is event-type-derived and mislabels a stop-gate involuntary close as done.",
		}, []string{"family", "env", "project", "role"}),

		upstreamErrorActive: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "agent_upstream_error_active",
			Help:      "Live count of agents currently sitting on an upstream provider error, by {category,project,role}, re-derived on every controller pod sweep. category mirrors coop's error_category wire enum (server_error/rate_limited/model_rate_limited/no_internet/out_of_credits/budget_exceeded/unauthorized/other). Current-state gauge and leg A of the Claude-outage detector (ka-0000000487): sum(...{category=\"server_error\"}) > N is 'Claude is failing our agents right now'. Level-triggered on purpose — the reporter observes this on a poll cadence, so a per-observation counter would measure outage DURATION x poll rate rather than error count.",
		}, []string{"category", "project", "role"}),

		upstreamErrorTransitions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "agent_upstream_error_transitions_total",
			Help:      "Edge-triggered count of agents ENTERING an upstream provider error category (incremented once per entry, not once per observation), by {category,project,role}. The independent second derivation of gardenio_agent_upstream_error_active (advice ka-0000005924): the two must reconstruct each other, so a sustained gauge with a flat counter means the transition detector is broken, while a climbing counter with a zero gauge means agents are recovering quickly (healthy). Use rate()/increase() over this; use the gauge for alerting.",
		}, []string{"category", "project", "role"}),

		landSpawnToWorking: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "lands_spawn_to_working_seconds",
			Help:      "Seconds from a tenant-land agent bead's creation to the first time the controller reports agent_state=working for it, observed once per agent. Labels {land}. The Lands tenant_spawn_to_working SLI (threshold 300s).",
			// 10s, 30s, 1m, 2m, 5m (the SLO threshold), 10m, 20m, 1h.
			Buckets: []float64{10, 30, 60, 120, 300, 600, 1200, 3600},
		}, []string{"land"}),

		sessionExitCode: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "session_exit_code_total",
			Help:      "Coop process exit codes observed at session-bead close, one increment per coop restart-loop iteration. Labels {code,project,role}; code enum: 0 (clean), 10 (credential switch), 129 (SIGHUP), 130 (SIGINT), 137 (SIGKILL, the kernel OOM-killer's signal), 143 (SIGTERM), other. Makes the exit-code split (previously a manual bead-list scan) queryable over time via rate()/increase() (gd-5MTDWjwNjm).",
		}, []string{"code", "project", "role"}),

		orphanSessions: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "orphan_open_sessions",
			Help:      "Open session beads whose agent bead is closed or missing, per session-reaper cycle (gd-c6muILNedN). phase=found: seen before the cycle closed any (in steady state, the sessions the in-pod gb close missed in one interval); phase=left: still open after it. A sustained phase=left above 0 means the reaper cannot close them and the open-session backlog is regrowing.",
		}, []string{"phase"}),

		accountLayer: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "agent_account_layer_total",
			Help:      "Which layer of applyAccountConfig produced (or refused) a pod's LLM account, counted once per genuine spawn attempt. Labels {layer,project,role}; layer enum: role-pin/spread/opus-fallback/project-cache/default-account/redirect-changed/escape-hatch-blocked/whitelist-blocked/no-credential-warn/no-credential-benign/unknown-account. Phase 0 instrumentation of the account-fallback-chain deprecation (ka-0000002142, epic ka-0000007694, docs/account-routing.md §7): a flat-~0 layer=\"project-cache\"/\"default-account\"/\"redirect-changed\" over a few prod days is the evidence that those layers never change the outcome and can be removed in Phases 2-3. Monotonic counter — query per-hour picks with increase(gardenio_agent_account_layer_total[1h]).",
		}, []string{"layer", "project", "role"}),
	}
}

// Register registers the event-driven metrics with reg. Served alongside the
// poller Collector on the controller :9092 /metrics endpoint.
func (e *EventMetrics) Register(reg prometheus.Registerer) error {
	collectors := []prometheus.Collector{
		e.lifecycleEvents,
		e.podPhaseTransitions,
		e.podsPending,
		e.sessionDuration,
		e.involuntaryDeaths,
		e.upstreamErrorActive,
		e.upstreamErrorTransitions,
		e.landSpawnToWorking,
		e.sessionExitCode,
		e.accountLayer,
		e.orphanSessions,
	}
	for _, col := range collectors {
		if err := reg.Register(col); err != nil {
			return err
		}
	}
	return nil
}

// RecordLifecycleEvent increments gardenio_agent_lifecycle_events_total for one
// transition. env is supplied from construction; sessionID should be non-empty
// ONLY for oom_killed (the drill-down case) — pass "" otherwise. An
// out-of-enum event is dropped (returns false) so cardinality stays bounded.
func (e *EventMetrics) RecordLifecycleEvent(event, project, role, sessionID string) bool {
	if e == nil {
		return false
	}
	if !lifecycleEventValid(event) {
		return false
	}
	e.lifecycleEvents.WithLabelValues(event, e.env, project, role, sessionID).Inc()
	return true
}

// RecordPodPhaseTransition increments gardenio_agent_pod_phase_transitions_total
// for one pod phase change. A no-op transition (from == to) is dropped.
func (e *EventMetrics) RecordPodPhaseTransition(from, to, project, role string) bool {
	if e == nil {
		return false
	}
	if from == to || from == "" || to == "" {
		return false
	}
	e.podPhaseTransitions.WithLabelValues(from, to, project, role).Inc()
	return true
}

// SetPodsPending replaces the entire gardenio_agent_pods_pending gauge with a
// fresh snapshot: it Reset()s the vector, then sets one series per
// (project,role) key to its observed count. The status reporter calls this once
// per pod-phase sweep with the current tally of Pending agent pods.
//
// The full Reset each sweep is deliberate and correct for a current-state
// gauge: a (project,role) that had Pending pods last sweep but none now must
// DISAPPEAR from the exposition, not linger at its stale value — otherwise the
// alert would never clear once fired. This is the poller-gauge idiom
// (metrics.go collectAgents Reset()), not the counter Reset anti-pattern the
// EventMetrics struct doc warns about; that warning applies only to counters
// feeding rate()/increase().
//
// The key is [2]string{project, role}; an empty count map clears the gauge to
// no-data (no Pending pods anywhere), which is the healthy steady state. A nil
// receiver is a clean no-op so an unwired reporter does not panic.
func (e *EventMetrics) SetPodsPending(counts map[[2]string]int) {
	if e == nil {
		return
	}
	e.podsPending.Reset()
	for key, n := range counts {
		e.podsPending.WithLabelValues(key[0], key[1]).Set(float64(n))
	}
}

// SetOrphanSessions publishes one session-reaper cycle's tally: found is the
// number of open sessions whose agent had ended, left the number still open
// after the cycle's closes. No-op on a nil receiver.
func (e *EventMetrics) SetOrphanSessions(found, left int) {
	if e == nil {
		return
	}
	e.orphanSessions.WithLabelValues("found").Set(float64(found))
	e.orphanSessions.WithLabelValues("left").Set(float64(left))
}

// SetUpstreamErrorActive replaces the entire gardenio_agent_upstream_error_active
// gauge with a fresh snapshot: Reset() then one series per (category,project,role)
// key. The status reporter calls this once per sweep with the tally of agents
// currently sitting on an upstream provider error.
//
// The full Reset each sweep is what makes the alert CLEAR: a category that had
// erroring agents last sweep but none now must disappear from the exposition
// rather than linger at its stale value, or ClaudeUpstreamOutage would latch
// firing forever after a single outage. Same poller-gauge idiom as
// SetPodsPending; not the counter Reset anti-pattern the struct doc warns about.
//
// An empty map clears the gauge to no-data — the healthy steady state, where no
// agent anywhere is erroring upstream. Keys are normalized by the caller via
// NormalizeUpstreamErrorCategory; an unrecognised category is dropped here as a
// second cardinality guard so a mis-wired caller cannot inflate the series set.
func (e *EventMetrics) SetUpstreamErrorActive(counts map[[3]string]int) {
	if e == nil {
		return
	}
	e.upstreamErrorActive.Reset()
	for key, n := range counts {
		if NormalizeUpstreamErrorCategory(key[0]) != key[0] {
			continue
		}
		e.upstreamErrorActive.WithLabelValues(key[0], key[1], key[2]).Set(float64(n))
	}
}

// RecordUpstreamErrorTransition increments
// gardenio_agent_upstream_error_transitions_total for one agent ENTERING an
// upstream error category. Call it only on the transition edge (the caller owns
// the per-agent dedup) — calling it per observation would turn the counter into
// a duration proxy and destroy its value as the independent cross-check of
// SetUpstreamErrorActive.
//
// An empty or unrecognised category is dropped (returns false); anything
// outside the bounded enum is folded to CategoryOther rather than becoming its
// own series. Reports whether a series was incremented so the caller can
// log/assert.
func (e *EventMetrics) RecordUpstreamErrorTransition(category, project, role string) bool {
	if e == nil {
		return false
	}
	normalized := NormalizeUpstreamErrorCategory(category)
	if normalized == "" {
		return false
	}
	e.upstreamErrorTransitions.WithLabelValues(normalized, project, role).Inc()
	return true
}

// ObserveLandSpawnToWorking observes one tenant-land agent's spawn-to-working
// time in seconds. The status reporter owns the once-per-agent dedup. An empty
// land or a negative duration (clock skew) is dropped.
func (e *EventMetrics) ObserveLandSpawnToWorking(land string, seconds float64) bool {
	if e == nil {
		return false
	}
	if land == "" || seconds < 0 {
		return false
	}
	e.landSpawnToWorking.WithLabelValues(land).Observe(seconds)
	return true
}

// RecordSessionDuration observes one session's lifetime in seconds. A negative
// duration (clock skew / missing timestamp) or out-of-enum outcome is dropped.
func (e *EventMetrics) RecordSessionDuration(seconds float64, project, role, outcome string) bool {
	if e == nil {
		return false
	}
	if seconds < 0 || !sessionOutcomeValid(outcome) {
		return false
	}
	e.sessionDuration.WithLabelValues(project, role, outcome).Observe(seconds)
	return true
}

// RecordInvoluntaryDeath increments gardenio_agent_involuntary_deaths_total for a
// session whose close_reason denotes an involuntary mid-work death. It parses
// the family from closeReason (InvoluntaryDeathFamily) and increments only when
// the reason actually denotes a counted death — a clean idle-out, graceful
// stop, or genuine crash returns false and increments nothing. Reports whether
// a series was incremented so the caller can log/assert.
func (e *EventMetrics) RecordInvoluntaryDeath(closeReason, project, role string) bool {
	if e == nil {
		return false
	}
	family, ok := InvoluntaryDeathFamily(closeReason)
	if !ok {
		return false
	}
	e.involuntaryDeaths.WithLabelValues(family, e.env, project, role).Inc()
	return true
}

// RecordSessionExitCode increments gardenio_session_exit_code_total for one
// session-bead close. exitCode is normalized to the bounded code enum
// (normalizeSessionExitCode) so a rare/unexpected code lands under "other"
// rather than inflating cardinality. Reports whether a series was
// incremented (always true for a non-nil receiver — the code label always
// resolves to a valid enum value) so the caller can log/assert consistently
// with the other Record* methods.
func (e *EventMetrics) RecordSessionExitCode(exitCode int, project, role string) bool {
	if e == nil {
		return false
	}
	e.sessionExitCode.WithLabelValues(normalizeSessionExitCode(exitCode), project, role).Inc()
	return true
}

// RecordAccountLayer increments gardenio_agent_account_layer_total for one
// spawn attempt, recording which applyAccountConfig layer produced (or refused)
// the pod's account (Phase 0 instrumentation, ka-0000002142). layer is the
// value stamped on AgentPodSpec.AccountLayer; an empty or out-of-enum layer is
// dropped (returns false) so a mis-wired caller cannot inflate cardinality.
// Call it exactly once per genuine spawn decision (the event door + the
// reconciler backstop), never on throwaway/upgrade-diff spec builds. Reports
// whether a series was incremented so the caller can log/assert.
func (e *EventMetrics) RecordAccountLayer(layer, project, role string) bool {
	if e == nil {
		return false
	}
	if !accountLayerValid(layer) {
		return false
	}
	e.accountLayer.WithLabelValues(layer, project, role).Inc()
	return true
}
