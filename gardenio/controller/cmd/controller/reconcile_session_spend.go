package main

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	gardeniometrics "gardenio/controller/internal/metrics"
)

// spendBaselineField is the agent-bead field where the session-start LiteLLM
// spend baseline is stashed. Persisting it on the bead (rather than in
// controller memory) lets the close-time reconciliation survive a controller
// restart between spawn and close. Re-captured at every (re)spawn, so it is
// always the CURRENT session's start point.
const spendBaselineField = "litellm_spend_baseline"

// spendCloseField is the settlement watermark: the agent tag's cumulative
// spend through the last successful settlement. Every settle advances it to
// the just-read close total, so closeTotal − max(watermark, baseline) is the
// not-yet-booked delta — which makes both the counter increment and the bead
// write idempotent under re-delivered closes (ka-0000007288; formerly this
// field was write-once and gated a first-session-only counter increment,
// dropping every later session of a respawning agent).
const spendCloseField = "litellm_spend_close"

// spendFirstBaselineField is the agent tag's cumulative spend at the bead's
// FIRST session spawn, written once and never overwritten. The bead's lifetime
// settled spend is then always closeTotal − firstBaseline, a pure overwrite
// that is idempotent, self-heals past lost settlements (the next close catches
// the total up), and wipes any non-authoritative gb-estimate increments that
// landed on total_cost_usd in between (ka-0000007288). Legacy beads that
// predate the field get it back-derived at settle time (see attemptReconcile).
const spendFirstBaselineField = "litellm_spend_first_baseline"

// sessionSettledCostField is the SESSION-bead field the reconciler writes the
// settled cost of that session into. Deliberately distinct from the session's
// total_cost_usd, which the in-pod gb close writes with its non-authoritative
// JSONL estimate and could clobber a concurrent authoritative write — separate
// fields cannot race (ka-0000007288 / audit F3).
const sessionSettledCostField = "settled_cost_usd"

// Retry/requeue defaults for close-time reconciliation. When the first inline
// attempt at session close fails to land the authoritative value — a transient
// /spend/tags error, a LiteLLM 5xx, or the agent tag not yet visible because of
// LiteLLM spend-log indexing lag — the session is enqueued onto a bounded
// background queue and retried with exponential backoff so the real cost
// eventually overwrites the non-authoritative in-pod gb estimate (ka-0000005130).
const (
	// defaultSpendMaxRetries bounds the number of background retries after the
	// inline attempt. ~5 attempts across the backoff schedule below covers
	// typical indexing lag (seconds–minutes) without retrying forever for a
	// session that genuinely made no billable requests.
	defaultSpendMaxRetries = 5
	// defaultSpendRetryBase is the first background backoff. Each subsequent
	// attempt doubles it up to defaultSpendRetryMax.
	defaultSpendRetryBase = 15 * time.Second
	// defaultSpendRetryMax caps the exponential backoff.
	defaultSpendRetryMax = 4 * time.Minute
	// spendRetryQueueSize bounds in-flight retry jobs; a full queue drops the
	// oldest-failing path (logged) rather than blocking session teardown.
	spendRetryQueueSize = 256
)

// sessionSpendReconciler captures a per-agent LiteLLM spend baseline at session
// start and reconciles the authoritative session cost at session close, using
// the static "agent:<name>" request tag that the podspec sets on every agent
// request (T2, ka-0000007328).
//
// Why baseline-delta instead of a windowed sum: GET /spend/tags is
// lifetime-cumulative per tag and ignores both start_date/end_date and the
// tags= filter param (confirmed live on LiteLLM v1.82 — see ka-0000002571).
// We therefore cannot window-sum a session; instead we snapshot the tag's
// cumulative total at start and subtract it at close. Because exactly one coop
// session runs per agent pod, the agent-tag delta over the pod lifetime is the
// session cost.
//
// This controller-side reconciliation is the authoritative agent-bead spend
// writer. It superseded a dead gb-side close-time hook
// (syncAccountSpendFromLiteLLM) that early-returned in agent pods because they
// have no LiteLLM master key — only the controller does. That hook was removed
// in I1 (ka-0000000717); the in-pod JSONL response_cost accumulation in
// cmd/gb/session_usage.go remains as a non-authoritative display estimate.
//
// F2 (ka-0000002904): besides writing total_cost_usd, reconcileAtClose also
// advances the gardenio_agent_spend_usd_total counter by the reconciled session
// cost — exactly one increment per session. This drives the spend charts at
// session-close granularity from real LiteLLM spend. A per-turn NATS spend
// forwarder once existed but was structurally dead (coop never emits
// response_cost, so it dropped every turn) and was removed in the same change.
// Per-turn real-time resolution is out of scope (would need a proxy callback /
// audit option A).
type sessionSpendReconciler struct {
	daemon     *beadsapi.Client
	litellmURL string
	logger     *slog.Logger

	// tags reads lifetime tag totals (GET /spend/tags, or LiteLLM_DailyTagSpend
	// per its source, gd-Um9gnz9FuW). main shares one cached reader between
	// this reconciler and tagSpendSync, so no path here scans LiteLLM_SpendLogs
	// on its own (gd-oih6JxjJMM). The constructor's default is uncached and
	// reads /spend/tags.
	tags *tagSpendReader

	// metrics is the spend counter the close-total is pushed into (nil-safe via
	// RecordSpend's nil-receiver guard). resolveLabels supplies the {model,
	// account} labels the agent bead does not itself carry; project/role are read
	// from the bead. Both are optional — a nil metrics sink or resolver simply
	// skips the counter increment, leaving the total_cost_usd write intact.
	metrics       *gardeniometrics.SpendMetrics
	resolveLabels spendLabelResolver

	// resolveBaseURL supplies the per-account LiteLLM base URL for a session's
	// project/role. When an account carries a non-empty base_url (e.g. a vkey
	// minted on an in-cluster proxy that is only resolvable there), both the
	// spawn-time baseline capture and the close-time reconcile must query that
	// SAME proxy — the agent-tag total is per-proxy, so mixing URLs would yield a
	// meaningless delta. Returns "" to fall back to the global litellmURL.
	// Optional: nil resolver or "" result preserves the prior global-URL behavior
	// (ka-0000003221).
	resolveBaseURL spendBaseURLResolver

	// skipSession reports whether a session's project/role resolves to an account
	// with no LiteLLM spend to reconcile — a provider=openai Codex subscription
	// login (billing_type=subscription, total_cost_usd stays 0; ADR 0004 §C).
	// Optional: nil never skips.
	skipSession func(project, role string) bool

	// retryQueue receives sessions whose inline close-time reconciliation could
	// not land the authoritative value (query error or tag absent from indexing
	// lag). A background worker (RunRetryLoop) drains it with exponential
	// backoff. nil until the worker is started, in which case enqueue is a no-op
	// (the inline attempt is then the only attempt — preserving the prior,
	// best-effort behavior under tests that never start the loop).
	retryQueue chan spendRetryJob
	// retry knobs, overridable in tests; default to the package constants.
	maxRetries int
	retryBase  time.Duration
	retryMax   time.Duration
	// sleep is the backoff sleeper, injectable so tests don't wall-clock wait.
	// Returns false if the context was cancelled during the wait.
	sleep func(ctx context.Context, d time.Duration) bool
}

// spendRetryJob carries the minimal state a background retry needs. Only the
// bead ID is kept (not the BeadDetail) so processRetry re-fetches fresh fields
// each attempt — the baseline / spendCloseField then reflect any concurrent
// write (e.g. a re-delivered close that already landed) rather than a stale
// snapshot captured at enqueue time.
type spendRetryJob struct {
	beadID    string
	agentName string
	attempt   int // background attempt number (1-based); inline attempt is 0
}

// spendLabelResolver supplies the model and account labels for the spend
// counter from a session's project and role. The agent bead carries neither
// model nor account, so the controller resolves them from its config caches
// (account name via project/role→account precedence, model via the account's
// default model). Injected so the reconciler stays testable without a full
// Config. Returns "" for either when nothing resolves — the series is still
// valid, just less attributed.
type spendLabelResolver func(project, role string) (model, account string)

// spendBaseURLResolver supplies the per-account LiteLLM base URL for a session's
// project and role. It returns "" when the resolved account has no base_url
// override, in which case the reconciler falls back to its global litellmURL.
// Injected so the reconciler stays testable without a full Config.
type spendBaseURLResolver func(project, role string) string

func newSessionSpendReconciler(daemon *beadsapi.Client, litellmURL, masterKey string, logger *slog.Logger) *sessionSpendReconciler {
	return &sessionSpendReconciler{
		daemon:     daemon,
		litellmURL: strings.TrimRight(litellmURL, "/"),
		tags:       newTagSpendReader(masterKey, 0, logger),
		logger:     logger.With("component", "session-spend"),
		maxRetries: defaultSpendMaxRetries,
		retryBase:  defaultSpendRetryBase,
		retryMax:   defaultSpendRetryMax,
		sleep:      sleepCtx,
	}
}

// sleepCtx sleeps for d or until ctx is cancelled, returning false on
// cancellation. The package-level default for sessionSpendReconciler.sleep.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// agentTag returns the LiteLLM request tag for an agent name. It must match the
// tag the podspec injects via x-litellm-tags (cmd/controller/podspec.go).
func agentTag(agentName string) string { return "agent:" + agentName }

// tagSpendEntry mirrors one element of the GET /spend/tags response array.
// Captured live shape: {"individual_request_tag":"agent:<name>","log_count":N,"total_spend":F}.
type tagSpendEntry struct {
	IndividualRequestTag string  `json:"individual_request_tag"`
	LogCount             int64   `json:"log_count"`
	TotalSpend           float64 `json:"total_spend"`
}

// baseURLFor resolves the LiteLLM base URL for a session's project/role via the
// optional resolveBaseURL hook, falling back to the global litellmURL when no
// resolver is wired or the account has no base_url override. The result is
// trimmed of a trailing slash so callers can concatenate "/spend/tags".
func (r *sessionSpendReconciler) baseURLFor(project, role string) string {
	if r.resolveBaseURL != nil {
		if override := strings.TrimRight(strings.TrimSpace(r.resolveBaseURL(project, role)), "/"); override != "" {
			return override
		}
	}
	return r.litellmURL
}

// skipSpend reports whether the session's account carries no LiteLLM spend (see
// skipSession). False when no hook is wired.
func (r *sessionSpendReconciler) skipSpend(project, role string) bool {
	return r.skipSession != nil && r.skipSession(project, role)
}

// queryTagSpend returns the lifetime cumulative spend for a single request tag
// from GET /spend/tags. The endpoint ignores start_date/end_date and the tags=
// filter param, so we read all tags and match client-side on
// individual_request_tag. Returns (0, false, nil) when the tag is not present
// yet (e.g. the agent has made no billable requests). baseURL is the per-account
// LiteLLM proxy resolved by baseURLFor (ka-0000003221).
//
// The reader may serve the same shape from LiteLLM_DailyTagSpend instead (see
// tagSpendReader.source); it switches only when the two sources agree, so a
// baseline taken from /spend/tags stays comparable.
//
// It reads the shared snapshot without waiting for a refresh (see
// tagSpendReader): the spawn and close paths call it inline, and a total up to
// one read interval old is safe because the settlement sweep books whatever
// a stale close-time read missed.
func (r *sessionSpendReconciler) queryTagSpend(ctx context.Context, baseURL, tag string) (spend float64, found bool, err error) {
	if r.tags == nil {
		return 0, false, fmt.Errorf("no /spend/tags reader wired")
	}
	entries, err := r.tags.Read(ctx, baseURL, false)
	if err != nil {
		return 0, false, err
	}
	for _, e := range entries {
		if e.IndividualRequestTag == tag {
			return e.TotalSpend, true, nil
		}
	}
	return 0, false, nil
}

// queryAllTagSpend returns the full GET /spend/tags entry list from one proxy,
// waiting for a refresh when the shared snapshot is due one. The settlement
// sweep and the historical backfill use it directly — one read per proxy per
// cycle — instead of a per-bead queryTagSpend round-trip.
func (r *sessionSpendReconciler) queryAllTagSpend(ctx context.Context, baseURL string) ([]tagSpendEntry, error) {
	if r.tags == nil {
		return nil, fmt.Errorf("no /spend/tags reader wired")
	}
	return r.tags.Read(ctx, baseURL, true)
}

// captureBaseline snapshots the agent tag's current cumulative spend and stashes
// it on the agent bead so the close hook can compute the session delta even
// across a controller restart. A missing tag records a 0 baseline (the agent has
// not spent yet). Best-effort: all errors are logged and never block the spawn.
//
// It also zero-initializes the spend counter series (A2, ka-0000007288): the
// counter is otherwise incremented exactly once at close, so a one-shot agent's
// series would be BORN at its final value and increase() over any window would
// read it as 0 — the dominant reason the spend charts under-read the bill. An
// Add(0) at spawn makes the close increment a real observable delta. Harmless
// on respawns (the series already exists). Labels are resolved from the same
// project/role config the close path uses, so spawn and close hit one series.
func (r *sessionSpendReconciler) captureBaseline(ctx context.Context, agentBeadID, agentName, project, role string) {
	if r == nil || agentBeadID == "" || agentName == "" {
		return
	}
	if r.skipSpend(project, role) {
		return
	}
	// The bead is fetched up front for the operator label and the write-once
	// first-baseline check below. A fetch failure degrades those two extras but
	// must not lose the baseline capture itself.
	bead, beadErr := r.daemon.GetBead(ctx, agentBeadID)
	if r.metrics != nil {
		model, account := "", ""
		if r.resolveLabels != nil {
			model, account = r.resolveLabels(project, role)
		}
		r.metrics.EnsureSpendSeries(agentName, project, role, model, account)
		if beadErr == nil && bead != nil {
			r.metrics.EnsureOperatorSeries(operatorIDLabel(bead), project, model)
		}
	}
	// Resolve the per-account proxy from the session's project/role so the
	// baseline is snapshotted on the SAME proxy the close-time reconcile will
	// query (ka-0000003221). The agent-tag total is per-proxy.
	spend, _, err := r.queryTagSpend(ctx, r.baseURLFor(project, role), agentTag(agentName))
	if err != nil {
		r.logger.Warn("spend baseline: query failed", "agent", agentName, "error", err)
		return
	}
	fields := map[string]any{spendBaselineField: spend}
	if err := r.daemon.UpdateBeadFieldsTyped(ctx, agentBeadID, fields); err != nil {
		r.logger.Warn("spend baseline: update failed", "agent", agentName, "bead", agentBeadID, "error", err)
		return
	}
	// Stamp the write-once first-session baseline (lifetime anchor for the
	// bead's total_cost_usd overwrite — see spendFirstBaselineField). Done as a
	// SEPARATE best-effort patch, never bundled with the baseline write above:
	// during a mixed-version rollout an older daemon rejects the whole patch on
	// an unknown field, and losing the baseline would make the session refuse
	// attribution entirely.
	if beadErr == nil && bead != nil && bead.Fields[spendFirstBaselineField] == "" {
		if err := r.daemon.UpdateBeadFieldsTyped(ctx, agentBeadID, map[string]any{spendFirstBaselineField: spend}); err != nil {
			r.logger.Warn("spend baseline: first-baseline update failed", "agent", agentName, "bead", agentBeadID, "error", err)
		}
	}
	r.logger.Debug("spend baseline captured", "agent", agentName, "bead", agentBeadID, "baseline", spend)
}

// reconcileAtClose reads the agent tag's cumulative spend at session close,
// subtracts the start baseline stashed on the bead, and writes the resulting
// session cost as total_cost_usd onto the agent bead (the agent rollup). The
// bead has already been fetched by the caller (emitSessionCloseMetrics), so the
// baseline is read from it directly. Best-effort: errors are logged, never
// returned — cost reconciliation must not perturb session teardown.
func (r *sessionSpendReconciler) reconcileAtClose(ctx context.Context, bead *beadsapi.BeadDetail, agentName string) {
	if r == nil || bead == nil || agentName == "" {
		return
	}
	// Inline attempt (attempt 0) on the teardown path, using the bead the caller
	// already fetched. On a transient failure — a /spend/tags error, a LiteLLM
	// 5xx, or the tag not yet visible because of indexing lag — enqueue a bounded
	// background retry so the authoritative value eventually overwrites the
	// non-authoritative gb estimate (ka-0000005130) rather than being abandoned
	// permanently.
	if landed := r.attemptReconcile(ctx, bead, agentName, 0); !landed {
		r.enqueueRetry(spendRetryJob{beadID: bead.ID, agentName: agentName, attempt: 1})
	}
}

// attemptReconcile performs one reconciliation attempt using the supplied bead
// (carrying the start baseline and current spendCloseField). Returns true when
// the authoritative value was successfully written OR the result is terminal and
// needs no retry; returns false only for transient conditions worth retrying (a
// /spend/tags query error, a daemon write error, or the tag being absent — which
// may still be indexing lag). The `attempt` arg is 0 for the inline call and
// 1-based for background retries; it only feeds logging.
func (r *sessionSpendReconciler) attemptReconcile(ctx context.Context, bead *beadsapi.BeadDetail, agentName string, attempt int) (landed bool) {
	if r == nil || bead == nil || agentName == "" {
		return true // nothing to retry
	}
	if r.skipSpend(bead.Fields["project"], bead.Fields["role"]) {
		return true // subscription account: no LiteLLM spend, nothing to retry
	}

	// Resolve the per-account proxy from the bead's project/role so the close
	// query hits the SAME proxy the baseline was captured on (ka-0000003221).
	closeTotal, found, err := r.queryTagSpend(ctx, r.baseURLFor(bead.Fields["project"], bead.Fields["role"]), agentTag(agentName))
	if err != nil {
		r.logger.Warn("spend reconcile: close query failed", "agent", agentName, "bead", bead.ID, "attempt", attempt, "error", err)
		return false // transient: retry
	}
	if !found {
		// No tag present. This is ambiguous: either the agent made no billable
		// requests this session (terminal — nothing to reconcile), or the spend
		// log hasn't indexed yet (transient). We cannot distinguish the two from
		// this endpoint alone, so we treat it as transient and let the bounded
		// retry budget decide: if the tag never appears across all attempts, the
		// session truly had no spend and total_cost_usd is correctly left
		// untouched. Returning false here is what gives indexing lag a chance to
		// resolve instead of permanently abandoning the authoritative write.
		r.logger.Debug("spend reconcile: tag absent (no billable spend yet or indexing lag)",
			"agent", agentName, "bead", bead.ID, "attempt", attempt)
		return false
	}
	return r.settle(ctx, bead, agentName, closeTotal, attempt)
}

// settle books the settlement math for a known close-total: the not-yet-booked
// delta onto the counters and session bead, and the lifetime total onto the
// agent bead. Split from attemptReconcile so the durable settlement sweep
// (spend_settlement_sweep.go) can reuse it with a per-cycle cached tag total
// instead of one /spend/tags round-trip per bead. Returns false only for a
// transient daemon write failure worth retrying.
func (r *sessionSpendReconciler) settle(ctx context.Context, bead *beadsapi.BeadDetail, agentName string, closeTotal float64, attempt int) (landed bool) {
	// The agent tag is lifetime-cumulative and reused across every session a pod
	// with this name runs (gardeners/scheduled agents respawn under one name).
	// Two per-bead anchors recover per-session and lifetime costs from it:
	//
	//   baseline  — tag total at THIS session's spawn (re-captured every spawn)
	//   watermark — tag total through the LAST settlement (spendCloseField)
	//
	// The not-yet-booked delta is closeTotal − max(baseline, watermark): the max
	// covers a stale baseline (captureBaseline failed on a respawn — the
	// watermark is then the honest start point) and a stale watermark (normal
	// respawn: the baseline is newer). A re-delivered close (NATS at-least-once,
	// stop→done) sees watermark == closeTotal → delta 0 → nothing double-books.
	// If NEITHER anchor exists we refuse to attribute: treating the absence as 0
	// would charge the agent's entire lifetime tag spend to this one session.
	baseline, baselineOK := parseBaselineField(bead.Fields[spendBaselineField])
	watermark, watermarkOK := parseBaselineField(bead.Fields[spendCloseField])
	if !baselineOK && !watermarkOK {
		r.logger.Warn("spend reconcile: no baseline or settlement watermark, skipping attribution to avoid charging lifetime spend to one session",
			"agent", agentName, "bead", bead.ID, "close", closeTotal,
			"baseline_raw", bead.Fields[spendBaselineField])
		return
	}
	start := baseline
	if !baselineOK || (watermarkOK && watermark > start) {
		start = watermark
	}
	newlySettled := closeTotal - start
	if newlySettled < 0 {
		// Defensive: a blended/late anchor (e.g. a mid-life proxy switch) must
		// never book a negative cost. Clamp to 0 and log so it is visible.
		r.logger.Warn("spend reconcile: negative settlement delta clamped to 0",
			"agent", agentName, "bead", bead.ID, "close", closeTotal, "start", start)
		newlySettled = 0
	}

	// Lifetime settled spend for the bead: closeTotal − first-session baseline,
	// written as a pure overwrite. Idempotent, self-healing (a close after a
	// lost settlement catches the total back up), and it wipes the
	// non-authoritative gb-estimate increments that land on total_cost_usd at
	// every in-pod session close — those survive only for sessions whose
	// settlement never lands, which is exactly the fallback they are for.
	// Legacy beads without the field get it back-derived as start − bookedSoFar,
	// which makes this close book bookedSoFar + newlySettled — accumulate
	// semantics that preserve what was already on the bead.
	firstBaseline, firstOK := parseBaselineField(bead.Fields[spendFirstBaselineField])
	deriveFirst := false
	if !firstOK {
		booked, _ := parseBaselineField(bead.Fields["total_cost_usd"])
		if booked < 0 {
			booked = 0
		}
		firstBaseline = start - booked
		if firstBaseline < 0 {
			// booked can exceed the anchor when gb-estimate increments inflated
			// total_cost_usd; a negative anchor would book more than the tag's
			// lifetime, so floor it at 0 (books at most the full lifetime).
			firstBaseline = 0
		}
		deriveFirst = true
	}
	lifetime := closeTotal - firstBaseline
	if lifetime < 0 {
		lifetime = 0
	}

	fields := map[string]any{
		"total_cost_usd": lifetime,
		spendCloseField:  closeTotal,
	}
	if err := r.daemon.UpdateBeadFieldsTyped(ctx, bead.ID, fields); err != nil {
		r.logger.Warn("spend reconcile: update failed", "agent", agentName, "bead", bead.ID, "attempt", attempt, "error", err)
		return false // transient: the daemon write failed, retry the whole attempt
	}
	if deriveFirst {
		// Separate best-effort patch (never bundled with the settlement write):
		// an older daemon in a mixed-version rollout rejects the whole patch on
		// an unknown field, and that must not fail the settlement itself.
		if err := r.daemon.UpdateBeadFieldsTyped(ctx, bead.ID, map[string]any{spendFirstBaselineField: firstBaseline}); err != nil {
			r.logger.Warn("spend reconcile: first-baseline back-fill failed", "agent", agentName, "bead", bead.ID, "error", err)
		}
	}
	r.logger.Info("session spend reconciled", "agent", agentName, "bead", bead.ID,
		"newly_settled", newlySettled, "lifetime", lifetime, "start", start, "close_total", closeTotal, "attempt", attempt)

	// Book the settled cost of THIS session onto the session bead (audit F3 —
	// per-session cost was previously never populated). Best-effort: a failure
	// never fails the settlement. Written to a dedicated field the in-pod gb
	// close cannot clobber (see sessionSettledCostField).
	if newlySettled > 0 {
		r.settleSessionBead(ctx, bead.ID, agentName, newlySettled)
	}

	// F2: drive gardenio_agent_spend_usd_total by the newly settled delta —
	// EVERY settlement, not just the bead's first (the old first-reconcile gate
	// dropped sessions 2..N of every respawning agent). Idempotency comes from
	// the watermark: a re-delivered close computes delta 0 and RecordSpend drops
	// non-positive costs, so a $0 session never advances the counter either.
	// The tiny read-check-write race between two concurrent closes of one bead
	// predates this change (the first-reconcile gate had the same window).
	//
	// ka-0000000999: also drive the operator-keyed
	// gardenio_operator_cost_usd_total ({operator_id,project,model}) from the
	// SAME settled delta. operator_id is the opaque operators(id) FK ("0" when
	// no human resolved — matches the kbeads unattributed bucket); no PII
	// hashing is needed, the FK is an integer, never an email.
	if r.metrics != nil {
		model, account := "", ""
		if r.resolveLabels != nil {
			model, account = r.resolveLabels(bead.Fields["project"], bead.Fields["role"])
		}
		if r.metrics.RecordSpend(agentName, bead.Fields["project"], bead.Fields["role"], model, account, newlySettled) {
			r.logger.Debug("spend reconcile: counter incremented",
				"agent", agentName, "bead", bead.ID, "cost_usd", newlySettled,
				"model", model, "account", account)
		}
		if r.metrics.RecordOperatorSpend(operatorIDLabel(bead), bead.Fields["project"], model, newlySettled) {
			r.logger.Debug("spend reconcile: operator counter incremented",
				"agent", agentName, "bead", bead.ID, "cost_usd", newlySettled,
				"operator_id", operatorIDLabel(bead), "model", model)
		}
	}
	return true
}

// settleSessionBead writes the settled cost of the just-closed session onto its
// session bead (dedicated field, see sessionSettledCostField). The session bead
// is resolved as the agent bead's most recent session — sessions register at
// spawn with an agent_bead_id field, so at close time the newest one is the
// session being settled. When intermediate settlements were lost the delta may
// span more than one session; booking it to the latest is the closest honest
// attribution without a per-request ledger. Best-effort by design.
func (r *sessionSpendReconciler) settleSessionBead(ctx context.Context, agentBeadID, agentName string, settled float64) {
	result, err := r.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:  []string{"session"},
		Fields: map[string]string{"agent_bead_id": agentBeadID},
		Sort:   "-created_at",
		Limit:  5,
	})
	if err != nil || result == nil {
		r.logger.Debug("spend reconcile: session bead list failed",
			"agent", agentName, "bead", agentBeadID, "error", err)
		return
	}
	// Re-apply the filter and pick the newest client-side — a lenient daemon
	// (or test stub) may return unfiltered/unsorted rows.
	var session *beadsapi.BeadDetail
	for _, b := range result.Beads {
		if b.Type != "session" || b.Fields["agent_bead_id"] != agentBeadID {
			continue
		}
		if session == nil || b.CreatedAt.After(session.CreatedAt) {
			session = b
		}
	}
	if session == nil {
		r.logger.Debug("spend reconcile: no session bead to settle",
			"agent", agentName, "bead", agentBeadID)
		return
	}
	if err := r.daemon.UpdateBeadFieldsTyped(ctx, session.ID, map[string]any{sessionSettledCostField: settled}); err != nil {
		r.logger.Warn("spend reconcile: session bead settle failed",
			"agent", agentName, "session", session.ID, "error", err)
		return
	}
	r.logger.Debug("session bead settled", "agent", agentName, "session", session.ID, "cost_usd", settled)
}

// enqueueRetry pushes a retry job onto the background queue. It is a no-op when
// no retry worker is running (retryQueue nil) — in that case the inline attempt
// was the only attempt, matching the prior best-effort contract. A full queue
// drops the job (logged) rather than blocking the session-teardown goroutine.
func (r *sessionSpendReconciler) enqueueRetry(job spendRetryJob) {
	if r == nil || r.retryQueue == nil {
		return
	}
	select {
	case r.retryQueue <- job:
	default:
		r.logger.Warn("spend reconcile: retry queue full, dropping job",
			"agent", job.agentName, "bead", job.beadID)
	}
}

// RunRetryLoop drains the background retry queue until ctx is cancelled. Each job
// waits an exponential backoff, then re-attempts reconciliation; a still-transient
// failure re-enqueues with an incremented attempt until maxRetries is exhausted.
// Call once from main in its own goroutine; it allocates the queue so enqueueRetry
// becomes active. Exactly one worker is expected (jobs are processed serially),
// which keeps backoff sleeps from starving the daemon and bounds concurrent
// /spend/tags load.
func (r *sessionSpendReconciler) RunRetryLoop(ctx context.Context) {
	if r == nil {
		return
	}
	if r.retryQueue == nil {
		r.retryQueue = make(chan spendRetryJob, spendRetryQueueSize)
	}
	r.logger.Info("session-spend retry worker started",
		"max_retries", r.maxRetries, "base", r.retryBase, "max_backoff", r.retryMax)
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-r.retryQueue:
			r.processRetry(ctx, job)
		}
	}
}

// processRetry sleeps the backoff for job.attempt, runs one reconciliation
// attempt, and re-enqueues with attempt+1 if it is still transient and the retry
// budget is not exhausted. Split out from RunRetryLoop for direct testing.
func (r *sessionSpendReconciler) processRetry(ctx context.Context, job spendRetryJob) {
	if !r.sleep(ctx, r.backoff(job.attempt)) {
		return // context cancelled during backoff
	}
	// Re-fetch the bead so the baseline / spendCloseField reflect any concurrent
	// write (e.g. a re-delivered close that already landed) rather than a stale
	// snapshot. A fetch failure is itself transient — fall through to the retry
	// budget below.
	bead, err := r.daemon.GetBead(ctx, job.beadID)
	if err == nil && bead != nil && r.attemptReconcile(ctx, bead, job.agentName, job.attempt) {
		return // landed (or terminal) — done
	}
	if err != nil || bead == nil {
		r.logger.Warn("spend reconcile: bead refetch failed during retry",
			"agent", job.agentName, "bead", job.beadID, "attempt", job.attempt, "error", err)
	}
	if job.attempt >= r.maxRetries {
		r.logger.Warn("spend reconcile: giving up after exhausting retries; total_cost_usd left at gb estimate",
			"agent", job.agentName, "bead", job.beadID, "attempts", job.attempt)
		return
	}
	r.enqueueRetry(spendRetryJob{beadID: job.beadID, agentName: job.agentName, attempt: job.attempt + 1})
}

// backoff returns the exponential backoff for a 1-based background attempt,
// capped at retryMax: attempt 1 → base, 2 → 2×base, 3 → 4×base, …
func (r *sessionSpendReconciler) backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := r.retryBase
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= r.retryMax {
			return r.retryMax
		}
	}
	if d > r.retryMax {
		d = r.retryMax
	}
	return d
}

// operatorIDLabel renders the operator_id metric label from an agent bead. It
// returns the opaque operators(id) FK as a decimal string, or the "0" sentinel
// when the bead has no resolved human operator (legacy/anonymous/S3-resume) —
// mirroring the gb session-push sentinel and the kbeads unattributedOperatorID
// bucket so the unattributed series is consistent across producers.
func operatorIDLabel(bead *beadsapi.BeadDetail) string {
	if bead == nil || bead.OperatedByID == nil {
		return "0"
	}
	return strconv.FormatInt(*bead.OperatedByID, 10)
}

// parseBaselineField parses a stashed spend baseline, distinguishing a genuinely
// captured value (including a captured 0) from an empty/unparseable field. It
// returns ok=false when the field is empty or cannot be parsed — the caller must
// NOT treat that as a 0 baseline, because the agent tag is lifetime-cumulative
// and a 0 baseline would attribute the agent's whole lifetime spend to one
// session (ka-0000000046). A genuinely-captured 0 (fresh agent, no prior spend)
// returns (0, true) and reconciles normally.
func parseBaselineField(s string) (float64, bool) {
	if s == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}
