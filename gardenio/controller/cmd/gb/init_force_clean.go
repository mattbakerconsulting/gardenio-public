package main

// init_force_clean.go — controller-driven force-clean context-blowup policy
// (ka-0000007242).
//
// THE PROBLEM. Resume is coop-owned: at boot coop downloads the prior session's
// transcript from S3 (keyed by COOP_S3_SESSION_KEY) and hands it to Claude Code
// as `--resume <session.jsonl>`. That is the right default — it is what makes a
// SIGTERM'd / node-reclaimed agent pick up where it left off. But a transcript
// can grow past the point where resuming it is viable: a session that ran for
// hundreds of turns, ballooned its context near the model's window, or has
// already been woken/restarted many times accumulates a JSONL so large that the
// boot-time `coop --resume` parse fails (or the re-primed context immediately
// overflows) — and the pod dies on startup before doing any work, silently,
// only to be restarted onto the SAME doomed transcript. A resume that can never
// succeed is worse than a clean start: the clean start at least makes progress.
//
// THE POLICY. Before the first coop launch, read the prior life's turn / token /
// wake signals off this pod's own agent bead (the statusreporter mirrors coop's
// live counters there: context_tokens, live_turns / total_request_count, and
// session_count as the wake count). If ANY crosses its threshold, latch
// force-clean for this boot: wire COOP_S3_NO_RESTORE=1 onto the coop subprocess
// (coop then skips the S3 restore and starts fresh — see coop run.rs
// force_clean_active) and purge the local PVC session logs so Claude Code's own
// auto-resume picker cannot resurrect the transcript either. The stable S3
// session key is left intact, so THIS fresh life's transcript still uploads under
// the same prefix — only the boot-time rehydration of the oversized prior
// transcript is suppressed.
//
// WAKE TRIGGER IS EVIDENCE-GATED (gd-HPVt3BCPCH). The turn and token signals
// measure the thing that actually breaks a resume. The wake count is only a
// proxy for it, and on a churning agent (OOM / credential-switch / node-reclaim
// restarts, none of which are the transcript's fault) it is the WRONG proxy:
// force-cleaning there means "restart with no memory of the work you already
// did", the pathology the restart-churn epic exists to stop. So crossing the
// wake threshold now force-cleans only when the agent's recent session beads
// show a resume that was ATTEMPTED AND FAILED (resumed=true, then crashed
// quickly — see countFailedResumes). Crossing the threshold ALWAYS alerts
// (log + gardenio_force_clean_wake_threshold_crossed_total), whatever it then
// decides; it is the one place in the codebase that counts wakes.
//
// The decision is latched ONCE per pod boot (resolveForceCleanPolicy, called from
// runInitAgent before runInitLoop) so every restart within this pod agrees, and
// so the read happens before buildCoopEnv wires the coop env. It is a quiet
// no-op when no agent bead / daemon / S3 is in scope (a --local dev run, mock
// mode), and fail-open: any daemon error leaves resume behavior exactly as it is
// today (never worse).

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// forceCleanEnvVar is the coop env var that activates a clean (fresh) session:
// skip the S3 transcript restore and ignore any local --resume hint. Named as a
// constant so the Go writer and the coop reader (config.rs COOP_S3_NO_RESTORE)
// can be grep-linked and never drift.
const forceCleanEnvVar = "COOP_S3_NO_RESTORE"

// forceCleanEnvValue is the value written for forceCleanEnvVar. It MUST be the
// literal "true", not a shell-truthy "1" (ka-0000001975).
//
// coop declares the flag as a clap-derive `bool`. A derive `bool` is an
// ArgAction::SetTrue flag, and when its value arrives through `env` clap parses
// it with the STRICT bool value parser — which accepts only "true" / "false".
// "1" is a usage error, and a usage error is fatal: coop exits 2 during arg
// parsing, before it starts anything. Because buildCoopEnv re-wires the same
// value on every relaunch, that is a deterministic crash loop: the supervisor
// burns all maxRestarts in ~70s and closes the agent bead
// max_restarts_reached. The force-clean policy exists to RECOVER an agent whose
// transcript is too big to resume; wired wrong, it instead killed the first
// agent that ever crossed a threshold (ka-0000003182, 12 wakes).
//
// coop now also accepts boolish spellings (config.rs, BoolishValueParser), so
// the two sides are skew-safe in both directions. This constant keeps the value
// correct even against a coop that predates that change.
const forceCleanEnvValue = "true"

// Default context-blowup thresholds. Each is independently overridable via the
// matching env var so the fleet can tune without a rebuild; a value of 0 (or a
// negative override) disables that individual trigger. Chosen conservatively so
// the policy fires only on genuinely-doomed resumes, not healthy long sessions:
//
//   - turns: a session that has taken this many round-trips has almost certainly
//     compacted repeatedly; its transcript is large and resume is fragile.
//   - tokens: resident context at/above this level is near the model's
//     auto-compaction / window ceiling — re-priming it on resume overflows.
//   - wakes: this many prior spawns for the same agent means the transcript has
//     been re-primed over and over; each wake re-pays (and re-grows) it.
const (
	defaultForceCleanTurnThreshold  = 800
	defaultForceCleanTokenThreshold = 350_000
	defaultForceCleanWakeThreshold  = 12

	// defaultForceCleanWakeMinResumeFailures is the evidence the wake trigger
	// needs on top of the bare count: at least this many consecutive failed
	// resumes among the agent's most recent sessions.
	defaultForceCleanWakeMinResumeFailures = 1

	// failedResumeMaxRuntime bounds "failed quickly": a resumed session that
	// crashed inside this window died before doing meaningful work, which is
	// what an unresumable transcript looks like. A resumed session that ran
	// longer and was then OOM-killed is a churn crash, not a resume failure.
	failedResumeMaxRuntime = 5 * time.Minute

	// failedResumeLookback is how many recent session beads are read to count
	// the failed-resume streak.
	failedResumeLookback = 30
)

// forceClean env-var overrides (all optional; unset → the defaults above).
const (
	forceCleanTurnThresholdEnv  = "GARDENIO_FORCE_CLEAN_TURN_THRESHOLD"
	forceCleanTokenThresholdEnv = "GARDENIO_FORCE_CLEAN_TOKEN_THRESHOLD"
	forceCleanWakeThresholdEnv  = "GARDENIO_FORCE_CLEAN_WAKE_THRESHOLD"
	// forceCleanWakeMinResumeFailuresEnv is how many consecutive recent failed
	// resumes (countFailedResumes) the wake trigger requires before it may fire.
	// Default 1. A value <= 0 restores the legacy bare-wake-count behavior.
	forceCleanWakeMinResumeFailuresEnv = "GARDENIO_FORCE_CLEAN_WAKE_MIN_RESUME_FAILURES"
	// forceCleanDisableEnv is a hard kill-switch: set truthy to disable the whole
	// policy regardless of thresholds (a session never force-cleans). The escape
	// hatch if the heuristic ever mis-fires in prod.
	forceCleanDisableEnv = "GARDENIO_FORCE_CLEAN_DISABLE"
)

// forceCleanLatched holds the boot-time force-clean decision so every restart in
// this pod, and buildCoopEnv, agree on one value. nil until resolveForceCleanPolicy
// runs; a non-nil pointer to false means "resolved, do not force-clean".
var forceCleanLatched atomic.Pointer[bool]

// forceCleanSignals is the prior-life usage read off the agent bead that the
// policy evaluates. Kept as a struct so decideForceClean is a pure function of
// (signals, thresholds) and trivially unit-testable.
type forceCleanSignals struct {
	turns  int64 // prior session round-trips (live_turns, else total_request_count)
	tokens int64 // resident context tokens at last report (context_tokens)
	wakes  int64 // prior spawn/session count for this agent (session_count)

	// failedResumes is the length of the most recent streak of sessions that
	// attempted a resume and died fast (countFailedResumes). Only read when the
	// wake count crosses its threshold; zero otherwise.
	failedResumes int64
}

// forceCleanThresholds is the resolved (post-env-override) trigger set. A
// non-positive field disables that individual trigger.
type forceCleanThresholds struct {
	turns  int64
	tokens int64
	wakes  int64

	// wakeMinFailures is the failed-resume evidence the wakes trigger needs.
	// <= 0 disables the gate (legacy bare wake count).
	wakeMinFailures int64
}

// resolveForceCleanThresholds reads the env overrides on top of the defaults. An
// unparseable or unset override falls back to the default; an explicit <=0
// override disables that trigger.
func resolveForceCleanThresholds() forceCleanThresholds {
	return forceCleanThresholds{
		turns:  int64EnvOr(forceCleanTurnThresholdEnv, defaultForceCleanTurnThreshold),
		tokens: int64EnvOr(forceCleanTokenThresholdEnv, defaultForceCleanTokenThreshold),
		wakes:  int64EnvOr(forceCleanWakeThresholdEnv, defaultForceCleanWakeThreshold),

		wakeMinFailures: int64EnvOr(forceCleanWakeMinResumeFailuresEnv, defaultForceCleanWakeMinResumeFailures),
	}
}

// decideForceClean is the pure policy core: return (true, reason) when any
// signal crosses its (enabled) threshold, else (false, ""). Split from all I/O
// so the threshold logic is unit-tested directly.
func decideForceClean(s forceCleanSignals, t forceCleanThresholds) (bool, string) {
	if t.turns > 0 && s.turns >= t.turns {
		return true, "turns=" + strconv.FormatInt(s.turns, 10) + ">=" + strconv.FormatInt(t.turns, 10)
	}
	if t.tokens > 0 && s.tokens >= t.tokens {
		return true, "context_tokens=" + strconv.FormatInt(s.tokens, 10) + ">=" + strconv.FormatInt(t.tokens, 10)
	}
	if wakeThresholdCrossed(s, t) && (t.wakeMinFailures <= 0 || s.failedResumes >= t.wakeMinFailures) {
		reason := "wakes=" + strconv.FormatInt(s.wakes, 10) + ">=" + strconv.FormatInt(t.wakes, 10)
		if t.wakeMinFailures > 0 {
			reason += " with failed_resumes=" + strconv.FormatInt(s.failedResumes, 10) + ">=" + strconv.FormatInt(t.wakeMinFailures, 10)
		}
		return true, reason
	}
	return false, ""
}

// wakeThresholdCrossed reports whether the bare wake count is at/over its
// (enabled) threshold, independent of the failed-resume evidence gate. This is
// the alert condition; decideForceClean additionally requires evidence to act.
func wakeThresholdCrossed(s forceCleanSignals, t forceCleanThresholds) bool {
	return t.wakes > 0 && s.wakes >= t.wakes
}

// countFailedResumes returns the length of the most recent streak of closed
// sessions that attempted a resume and died fast: resumed=true (an S3 archive
// was observably present, see registerSessionBead), status=crashed, and a
// runtime under failedResumeMaxRuntime. Sessions are expected newest-first and
// still-active ones are skipped; the first closed session that is NOT a failed
// resume ends the streak (the agent resumed or ran fine since).
func countFailedResumes(sessions []*beadsapi.BeadDetail, agentBeadID string) int64 {
	var n int64
	for _, s := range sessions {
		if s == nil || s.Fields["agent_bead_id"] != agentBeadID {
			continue
		}
		status := s.Fields["status"]
		if status == "active" || status == "" {
			continue
		}
		if s.Fields["resumed"] != "true" || status != "crashed" {
			break
		}
		st, err1 := time.Parse(time.RFC3339, s.Fields["started_at"])
		et, err2 := time.Parse(time.RFC3339, s.Fields["ended_at"])
		if err1 != nil || err2 != nil || et.Sub(st) >= failedResumeMaxRuntime {
			break
		}
		n++
	}
	return n
}

// readFailedResumes counts the failed-resume streak off the agent's recent
// session beads. Fail-open: any error or missing context yields 0, which
// withholds (never grants) the wake trigger.
func readFailedResumes(ctx context.Context, agentBeadID string) int64 {
	agentName := os.Getenv("BOAT_AGENT")
	if daemon == nil || agentBeadID == "" || agentName == "" {
		return 0
	}
	sessions, err := daemon.ListSessionBeads(ctx, agentName, failedResumeLookback)
	if err != nil {
		logInit("force-clean: could not list session beads for %s (%v) — treating as no failed-resume evidence", agentName, err)
		return 0
	}
	return countFailedResumes(sessions, agentBeadID)
}

// forceCleanWakeMetricOutcome labels gardenio_force_clean_wake_threshold_crossed_total.
type forceCleanWakeMetricOutcome string

const (
	// wakeOutcomeFired — threshold crossed AND evidence present: force-clean latched
	// (or would have, had a turn/token trigger not already fired first).
	wakeOutcomeFired forceCleanWakeMetricOutcome = "fired"
	// wakeOutcomeNoEvidence — threshold crossed but no failed resume on record, so
	// the agent keeps its transcript. The churning-agent case this gate exists for.
	wakeOutcomeNoEvidence forceCleanWakeMetricOutcome = "no_resume_failure_evidence"
)

// forceCleanWakeAlerter publishes the wake-threshold-crossed signal. A package
// var so tests capture calls without touching vmagent.
var forceCleanWakeAlerter = defaultForceCleanWakeAlert

// defaultForceCleanWakeAlert logs loudly and pushes the counter to vmagent
// (when VMAGENT_URL is set). Best-effort: an observability failure never
// affects boot.
func defaultForceCleanWakeAlert(ctx context.Context, agentBeadID string, s forceCleanSignals, t forceCleanThresholds, outcome forceCleanWakeMetricOutcome) {
	logInit("force-clean: WAKE THRESHOLD CROSSED (wakes=%d >= %d, failed_resumes=%d, outcome=%s) — "+
		"this agent has been restarted %d times; investigate the restart churn",
		s.wakes, t.wakes, s.failedResumes, outcome, s.wakes)
	body := fmt.Sprintf("gardenio_force_clean_wake_threshold_crossed_total{outcome=%q,agent=%q} 1\n",
		string(outcome), agentBeadID)
	pushVMAgentLines(ctx, body, 3*time.Second)
}

// readForceCleanSignals reads the prior-life usage signals off the pod's own
// agent bead. The statusreporter mirrors coop's live counters onto these fields
// while the prior incarnation ran (reporter.go reportLiveUsage: context_tokens,
// live_turns) and the session-close accountant accumulates the cross-session
// rollups (client_session.go usageDeltas: total_request_count, session_count).
// Returns zero-valued signals (which never trip a positive threshold) on any
// daemon error or missing bead — the fail-open contract.
func readForceCleanSignals(ctx context.Context, agentBeadID string) forceCleanSignals {
	if daemon == nil || agentBeadID == "" {
		return forceCleanSignals{}
	}
	bead, err := daemon.GetBead(ctx, agentBeadID)
	if err != nil || bead == nil {
		if err != nil {
			logInit("force-clean: could not read agent bead %s (%v) — leaving resume behavior unchanged", agentBeadID, err)
		}
		return forceCleanSignals{}
	}
	f := bead.Fields
	// Turns: prefer the live in-pod counter; fall back to the authoritative
	// cross-session total when the live field was never written (e.g. a prior
	// life that closed before its first live_turns flush).
	turns := parseFieldInt64(f["live_turns"])
	if turns == 0 {
		turns = parseFieldInt64(f["total_request_count"])
	}
	return forceCleanSignals{
		turns:  turns,
		tokens: parseFieldInt64(f["context_tokens"]),
		wakes:  parseFieldInt64(f["session_count"]),
	}
}

// resolveForceCleanPolicy latches the boot-time force-clean decision for this
// pod. Called from runInitAgent BEFORE runInitLoop so the decision is made once,
// before buildCoopEnv wires the coop env and before the first resume probe.
//
// A quiet no-op (leaves the latch at "do not force-clean") when:
//   - the kill-switch GARDENIO_FORCE_CLEAN_DISABLE is truthy, or
//   - no S3 resume is even intended (s3ResumeIntended false) — there is no
//     transcript to restore, so there is nothing to force-clean away, or
//   - no agent bead / daemon in scope, or the read fails, or
//   - no signal crosses its threshold.
//
// On a positive decision it logs the tripping reason (so the boot is explainable
// in the pod log) and latches true; buildCoopEnv then wires COOP_S3_NO_RESTORE
// and the restart loop purges the local session logs.
func resolveForceCleanPolicy(ctx context.Context) {
	decision := false
	defer func() { forceCleanLatched.Store(&decision) }()

	if truthyEnv(os.Getenv(forceCleanDisableEnv)) {
		logInit("force-clean: disabled by %s — resume behavior unchanged", forceCleanDisableEnv)
		return
	}
	// Nothing to restore ⇒ nothing to force-clean. This also keeps --local / mock
	// runs (no S3) entirely untouched.
	if !s3ResumeIntended() {
		return
	}
	agentBeadID := agentOwnBeadID()
	signals := readForceCleanSignals(ctx, agentBeadID)
	thresholds := resolveForceCleanThresholds()
	if wakeThresholdCrossed(signals, thresholds) {
		// Evidence is only needed (and the extra session-bead read only paid) once
		// the wake count is high; the alert fires either way.
		if thresholds.wakeMinFailures > 0 {
			signals.failedResumes = readFailedResumes(ctx, agentBeadID)
		}
		outcome := wakeOutcomeFired
		if thresholds.wakeMinFailures > 0 && signals.failedResumes < thresholds.wakeMinFailures {
			outcome = wakeOutcomeNoEvidence
		}
		forceCleanWakeAlerter(ctx, agentBeadID, signals, thresholds, outcome)
	}
	if fire, reason := decideForceClean(signals, thresholds); fire {
		decision = true
		logInit("force-clean: context-blowup threshold crossed (%s) — forcing a CLEAN session "+
			"(skipping resume of the oversized prior transcript); this life still uploads under the same key",
			reason)
	}
}

// forceCleanActive reports the latched boot-time decision. Before
// resolveForceCleanPolicy has run (or on a path that never calls it, e.g. `gb
// agent start --local`), it returns false so behavior is exactly today's.
func forceCleanActive() bool {
	if p := forceCleanLatched.Load(); p != nil {
		return *p
	}
	return false
}

// int64EnvOr parses an int64 env override, falling back to def when unset or
// unparseable.
func int64EnvOr(key string, def int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}

// parseFieldInt64 parses a bead field string as int64; returns 0 on empty or a
// parse error. Bead numeric fields serialize as strings in BeadDetail.Fields.
func parseFieldInt64(s string) int64 {
	if s == "" {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// truthyEnv reports whether an env value is a recognized "on" token.
func truthyEnv(v string) bool {
	switch v {
	case "1", "true", "TRUE", "True", "yes", "on":
		return true
	default:
		return false
	}
}
