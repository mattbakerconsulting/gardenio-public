package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"gardenio/controller/internal/beadsapi"
	gardeniometrics "gardenio/controller/internal/metrics"
)

const (
	// anthropicProbeURL is the Anthropic messages endpoint probed for rate-limit headers.
	anthropicProbeURL = "https://api.anthropic.com/v1/messages"

	// probeModel is the cheapest model for probe calls (~$0.000003/call at max_tokens=1).
	probeModel = "claude-haiku-4-5-20251001"

	// probeInterval is the default interval between probes per account.
	probeInterval = 5 * time.Minute

	// probeIntervalHighUtil is the interval used when utilization_5h > 0.9.
	// Backing off avoids the probe consuming from a quota it is measuring.
	probeIntervalHighUtil = 15 * time.Minute

	// probeSafetyNet is the fallback probe interval when the account has been
	// throttled and probing continues to fail (network/auth errors).
	probeSafetyNet = 30 * time.Minute

	// defaultSoftFailoverThreshold is the default utilization_5h at which an
	// account is considered "warming" — new sessions are steered away, but
	// existing sessions are not disturbed.
	defaultSoftFailoverThreshold = 0.80

	// warmingCooldownOffset is subtracted from the warming threshold to create
	// hysteresis. An account must drop to threshold - offset before being
	// marked as cooled, preventing flapping around the threshold.
	warmingCooldownOffset = 0.05

	// probeTickInterval is how often RunLoop wakes to probe due accounts and
	// emit a heartbeat. Kept at 1 minute (the historical cadence).
	probeTickInterval = 1 * time.Minute

	// probeWatchdogInterval is how often the watchdog samples the loop's
	// last-tick timestamp.
	probeWatchdogInterval = 1 * time.Minute

	// probeStallThreshold is how stale the loop's last tick may get before the
	// watchdog raises a WARN + NATS alert. The 2026-07-05 freeze went undetected
	// for ~24h; at 10 minutes (10× the tick interval) a real wedge is flagged
	// well before either recovery path (SweepStaleExhausted needs a probe within
	// probeStaleThreshold=20min) silently degrades.
	probeStallThreshold = 10 * time.Minute
)

// modelGroupLimit is the per-model-group slice of a probe result: the
// utilization/reset/status Anthropic reports for ONE model family in ONE window
// (e.g. window="7d", group="fable"). Parsed generically from the group-suffixed
// unified headers so gardenio tracks Fable capacity separately from Opus/Sonnet
// (ka-0000003806). Fields are zero when the corresponding header is absent —
// Anthropic does not always emit all three metrics for every group.
type modelGroupLimit struct {
	// Window is the rate-limit window this slice belongs to: "5h" or "7d".
	Window string `json:"window"`
	// Group is the model family: e.g. "opus", "sonnet", "fable". Taken verbatim
	// from the header so a new group Anthropic introduces flows through untouched.
	Group string `json:"group"`
	// Utilization is the 0.0–1.0 fractional utilization of this group's window.
	Utilization float64 `json:"utilization"`
	// Reset is the unix timestamp at which this group's window resets (0 if absent).
	Reset int64 `json:"reset,omitempty"`
	// Status is the per-group window decision ("allowed"/"allowed_warning"/
	// "rejected"), or "" when Anthropic does not emit a group-scoped status.
	Status string `json:"status,omitempty"`
}

// probeResult holds the parsed rate-limit state from a single Anthropic probe call.
type probeResult struct {
	// Status is the top-level rate-limit decision: "allowed" or "rejected".
	Status string

	// Status5h / Status7d are per-window decisions.
	Status5h string
	Status7d string

	// Utilization5h / Utilization7d are 0.0–1.0 fractional utilization.
	Utilization5h float64
	Utilization7d float64

	// Reset5h / Reset7d are unix timestamps of per-window resets.
	Reset5h int64
	Reset7d int64

	// Reset is the binding window's reset timestamp (same value as Reset5h or Reset7d
	// depending on which window caused the rejection).
	Reset int64

	// RepresentativeClaim is "five_hour" or "seven_day" — which window is binding.
	RepresentativeClaim string

	// ModelGroups holds the PER-MODEL-GROUP rate-limit state Anthropic now emits
	// alongside the aggregate windows. Anthropic splits subscription limits per
	// model family (an all-models/opus weekly window, a sonnet-only window, and a
	// fable-only window — ka-0000003806), surfaced on the SAME probe response as
	// group-suffixed unified headers, e.g.
	// anthropic-ratelimit-unified-7d_fable-utilization. The aggregate
	// Utilization5h/7d above collapse every group to one number, so an account can
	// be fable-exhausted while opus is idle and the aggregate looks fine. This map
	// preserves the split. Keyed "<window>_<group>" (e.g. "7d_fable"); nil/empty
	// when the account's response carries no group-suffixed headers (older
	// accounts, or a group Anthropic has not split). Populated generically by
	// parseProbeHeaders so a new group needs no code change.
	ModelGroups map[string]modelGroupLimit

	// OverageStatus is "allowed" or "rejected".
	OverageStatus string

	// OverageReason is the human-readable reason overage is disabled (e.g. "out_of_credits").
	OverageReason string

	// RetryAfter is the retry-after duration on a 429 response.
	RetryAfter time.Duration

	// HTTPStatus is the raw HTTP status code.
	HTTPStatus int

	// ProbeError is set when the probe itself failed (network error, auth failure, etc.).
	// A nil ProbeError with HTTPStatus==429 means a clean throttle response.
	ProbeError error

	// CheckedAt is when this result was observed.
	CheckedAt time.Time
}

// Throttled returns true when the account should be considered unavailable.
// This covers two cases:
//   - Anthropic has explicitly rejected requests (status="rejected").
//   - Either rolling window has reached 100% utilization. Anthropic may still
//     serve requests in this state by granting overage capacity, but using it
//     costs real money — see ka-0000002794. We treat 100% util as effectively
//     throttled so auth_failover rotates off the account.
//
// It deliberately does NOT treat overage_status=rejected (even
// reason=out_of_credits) as throttled. The overage header is ambiguous as a
// PROACTIVE health signal: an out-of-credits subscription account can still
// return HTTP 200 to a real request (the GIB box matthewcbaker0122 —
// ka-0000004929), so condemning every out_of_credits account here would strand
// single-account boxes exactly as the reverted overageReasonBlocks hard-block
// did. The out-of-credits case is instead handled REACTIVELY, keyed on the real
// billing 429 that auth_failover already observes: overageOutOfCredits blocks
// the exhausted-clear paths (reconcileLegacyStatusFields, SweepStaleExhausted,
// switchback) so an account a genuine 429 marked exhausted is not oscillated
// back to active every probe (ka-0000008164).
func (r probeResult) Throttled() bool {
	if r.Status == "rejected" {
		return true
	}
	if r.Utilization5h >= exhaustedUtilization || r.Utilization7d >= exhaustedUtilization {
		return true
	}
	return false
}

// probeAuthStatus classifies a probe result's TERMINAL-auth verdict, distinct
// from the transient rate-limit Throttled() signal (ka-0000002085):
//
//   - "rejected": the probe got an HTTP 401 or 403 — the credential can no
//     longer authenticate at all (revoked OAuth token, or the org has disabled
//     Claude Code subscription access). This is terminal: no rate-limit window
//     reset will heal it, so switch-target selection must exclude the account.
//   - "ok": the probe authenticated cleanly. Both a 200 and a 429 (rate-limit
//     throttle) prove the credential is LIVE — a 429 is a rate limit, not an
//     auth failure — so both read "ok".
//   - "" (unknown): any OTHER outcome — a network failure, a 5xx, a timeout, or
//     an unclassified non-2xx — is transient probe-infrastructure noise that
//     must NOT condemn the account. Callers leave the persisted value untouched
//     on "".
//
// It reads HTTPStatus directly (set on every response, including the 401/403
// that also populate ProbeError) rather than inspecting ProbeError text.
func probeAuthStatus(r probeResult) string {
	switch r.HTTPStatus {
	case http.StatusUnauthorized, http.StatusForbidden:
		return "rejected"
	case http.StatusOK, http.StatusTooManyRequests:
		return "ok"
	default:
		return ""
	}
}

// desiredSoftFailoverState computes the soft_failover_state the bead should
// hold given its current persisted state and the latest probe result. It is
// pure (no in-memory state) so it recovers correctly across controller
// restarts even when the prior process left the bead in `warming`
// (ka-0000004575). prevState is the bead's current `soft_failover_state`
// field; threshold is the per-account warming threshold (default 0.80).
func desiredSoftFailoverState(prevState string, result probeResult, threshold float64) string {
	switch {
	case result.Throttled():
		// Hard failover owns the account while throttled; leave the soft
		// state alone so we don't fight the throttle/recovery path.
		return prevState
	case prevState == "warming":
		// Hysteresis: stay warming until utilization drops below
		// threshold - cooldown offset.
		if result.Utilization5h < (threshold - warmingCooldownOffset) {
			return "normal"
		}
		return "warming"
	default:
		if result.Utilization5h >= threshold {
			return "warming"
		}
		return "normal"
	}
}

// rateLimitProbe is a background reconciler that periodically probes the
// Anthropic API for each account bead that uses a secret-backed credential.
// It stores results on the account bead and publishes NATS events on transitions.
type rateLimitProbe struct {
	daemon *beadsapi.Client
	// accounts is the client every account-bead WRITE goes through. In
	// production it authenticates as the controller's own machine principal
	// (gd-s7aPIf0Bgp) so kbeads admits update/account through the
	// gardenio-machine-accounts grant; when the machine principal is disabled
	// it is the same client as daemon. Reads and non-account writes stay on
	// daemon: a machine principal is ownership-gated for non-config beads.
	accounts   *beadsapi.Client
	k8sClient  kubernetes.Interface
	namespace  string
	notifier   *rotationNotifier // for NATS events; may be nil
	httpClient *http.Client
	logger     *slog.Logger
	metrics    *gardeniometrics.ProbeMetrics // liveness metrics; nil-safe

	mu        sync.Mutex
	lastProbe map[string]time.Time   // account name → last probe time
	lastState map[string]probeResult // account name → last result

	// lastProbeTick is the Unix-nano timestamp of the last COMPLETED RunLoop
	// tick — the heartbeat the watchdog reads to distinguish "loop alive but
	// nothing due" from "loop dead" (ka-0000001889). It is written by tick() and
	// read by checkLiveness() across goroutines, so it is an atomic rather than
	// mu-guarded to keep the watchdog off the probe's hot-path lock.
	lastProbeTick atomic.Int64
	// tickCount counts completed ticks, surfaced in the heartbeat log.
	tickCount atomic.Int64
	// panicsRecovered counts panics swallowed inside the loop, surfaced in the
	// heartbeat log so a faulting iteration is visible even without metrics.
	panicsRecovered atomic.Int64
	// stallsDetected counts watchdog stall detections — in-process observability
	// parity with the probe_loop_stalls_detected_total metric.
	stallsDetected atomic.Int64

	// --- Test seams (production defaults set by newRateLimitProbe). ---

	// tickInterval / watchdogInterval / stallThreshold let tests drive the loop
	// and watchdog on sub-second cadences.
	tickInterval     time.Duration
	watchdogInterval time.Duration
	stallThreshold   time.Duration
	// now is the clock, overridable in tests to simulate a stalled loop.
	now func() time.Time
	// probeHook, when non-nil, runs at the top of each per-account probe. Tests
	// use it to inject a panic and prove RunLoop survives it.
	probeHook func(account string)
}

// accounts is the client for account-bead writes (the machine principal when
// enabled); pass daemon itself to keep today's single-client behavior.
func newRateLimitProbe(daemon, accounts *beadsapi.Client, k8sClient kubernetes.Interface, namespace string, notifier *rotationNotifier, metrics *gardeniometrics.ProbeMetrics, logger *slog.Logger) *rateLimitProbe {
	if accounts == nil {
		accounts = daemon
	}
	return &rateLimitProbe{
		daemon:           daemon,
		accounts:         accounts,
		k8sClient:        k8sClient,
		namespace:        namespace,
		notifier:         notifier,
		httpClient:       &http.Client{Timeout: 15 * time.Second},
		logger:           logger.With("component", "ratelimit-probe"),
		metrics:          metrics,
		lastProbe:        make(map[string]time.Time),
		lastState:        make(map[string]probeResult),
		tickInterval:     probeTickInterval,
		watchdogInterval: probeWatchdogInterval,
		stallThreshold:   probeStallThreshold,
		now:              time.Now,
	}
}

// RunLoop runs the probe loop until ctx is cancelled. It ticks every
// tickInterval (1 minute in production) and probes accounts whose per-account
// interval has elapsed.
//
// Hardening (ka-0000001889): a single per-account panic — or a panic anywhere in
// the probeAll scaffolding — can no longer kill the loop. tick() runs each
// iteration under recover() and always records a heartbeat, so the loop keeps
// ticking (and keeps SweepStaleExhausted's recovery data fresh) even when a
// probe faults. On 2026-07-05 the loop went silent for ~24h and nothing noticed;
// now every tick advances lastProbeTick and emits a mandatory heartbeat log +
// metric that the watchdog (WatchdogLoop) reads to detect a wedge.
func (p *rateLimitProbe) RunLoop(ctx context.Context) {
	ticker := time.NewTicker(p.tickInterval)
	defer ticker.Stop()

	// Probe immediately at startup.
	p.tick(ctx)

	for {
		select {
		case <-ticker.C:
			p.tick(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// tick runs one probe pass under panic recovery and always records the
// heartbeat that proves the loop is alive. A panic that escapes probeAll (e.g.
// from the bead-listing scaffolding) is caught here so RunLoop keeps ticking;
// per-account panics are already isolated inside probeAll by probeOne. Either
// way the heartbeat still fires — a live-but-idle loop is now distinguishable
// from a dead one.
func (p *rateLimitProbe) tick(ctx context.Context) {
	func() {
		defer func() {
			if r := recover(); r != nil {
				p.panicsRecovered.Add(1)
				p.metrics.RecordPanicRecovered()
				p.logger.Error("ratelimit-probe: recovered panic in probe pass; loop continues",
					"panic", r, "stack", string(debug.Stack()))
			}
		}()
		p.probeAll(ctx)
	}()

	// Heartbeat: record the tick AFTER the (recovered) probe pass so a stall
	// inside probeAll can't advance the liveness clock. This is the mandatory
	// per-tick signal — mirrors auth_failover.go's "sweep-exhausted: tick
	// complete" — that makes "loop alive but nothing due" distinguishable from
	// "loop dead" (ka-0000001889).
	now := p.now()
	p.lastProbeTick.Store(now.UnixNano())
	ticks := p.tickCount.Add(1)
	p.metrics.RecordTick()
	p.logger.Info("ratelimit-probe: tick complete",
		"tick", ticks,
		"panics_recovered", p.panicsRecovered.Load())
}

// probeAll fetches account beads and probes any that are due. Each per-account
// probe runs under its own recover() (probeOne) so one panicking account cannot
// abort the pass for its siblings (ka-0000001889).
func (p *rateLimitProbe) probeAll(ctx context.Context) {
	accounts, err := p.daemon.ListAccountBeads(ctx)
	if err != nil {
		p.logger.Warn("failed to list account beads", "error", err)
		return
	}

	for name, acct := range accounts {
		p.probeOne(ctx, name, acct)
	}
}

// probeOne probes a single account, isolated under recover() so a panic in one
// account's probe/handleResult does not abort the whole pass. It returns after
// logging + counting the panic; the next account is still probed.
func (p *rateLimitProbe) probeOne(ctx context.Context, name string, acct beadsapi.AccountInfo) {
	defer func() {
		if r := recover(); r != nil {
			p.panicsRecovered.Add(1)
			p.metrics.RecordPanicRecovered()
			p.logger.Error("ratelimit-probe: recovered panic probing account; skipping to next",
				"account", name, "panic", r, "stack", string(debug.Stack()))
		}
	}()

	if p.probeHook != nil {
		p.probeHook(name)
	}

	// Only probe accounts backed by a k8s secret (provider == claude or unset).
	credType, secretName, ok := strings.Cut(acct.Credential, ":")
	if !ok || credType != "secret" {
		return
	}
	provider := acct.Provider
	if provider != "" && provider != "claude" && provider != "anthropic" {
		return
	}

	// Skip accounts whose model is routed to a non-Anthropic backend (e.g.
	// bedrock-fronted via litellm). The probe targets api.anthropic.com
	// directly with the account's secret token; tokens for non-Anthropic
	// backends consistently return 401 there. (ka-0000003306)
	if isBedrockBackedAccount(acct) {
		return
	}

	// Skip accounts marked status=disabled. Disabled accounts are not used
	// for routing, so probing them just doubles the cost (and the chance of
	// contributing to a shared upstream's quota when two beads alias the
	// same OAuth credential — ka-0000005148).
	if isDisabledAccount(acct) {
		return
	}

	if !p.isDue(name) {
		return
	}

	token, err := p.readToken(ctx, secretName)
	if err != nil {
		p.logger.Warn("failed to read secret for account",
			"account", name, "secret", secretName, "error", err)
		return
	}

	result := p.probe(ctx, token)
	p.handleResult(ctx, name, acct, result)
}

// WatchdogLoop supervises RunLoop's liveness. Every watchdogInterval it checks
// how long since the loop last ticked; if that exceeds stallThreshold it raises
// a WARN log + stall metric + NATS alert (ka-0000001889). This is the detection
// that was missing on 2026-07-05, when the probe froze silently for ~24h and
// both exhausted-account recovery paths degraded with no signal. Run it in its
// own goroutine alongside RunLoop.
func (p *rateLimitProbe) WatchdogLoop(ctx context.Context) {
	ticker := time.NewTicker(p.watchdogInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			p.checkLiveness()
		case <-ctx.Done():
			return
		}
	}
}

// checkLiveness compares the last-tick heartbeat against stallThreshold and
// alerts on a wedge. Before the first tick lands (lastProbeTick==0) it is a
// no-op — there is nothing to be stale relative to yet. It publishes the
// current staleness to the metric on every sample so a dashboard sees the
// freshness climb even below the alert threshold.
func (p *rateLimitProbe) checkLiveness() {
	lastNano := p.lastProbeTick.Load()
	if lastNano == 0 {
		return // loop hasn't completed its first tick yet
	}

	staleFor := p.now().Sub(time.Unix(0, lastNano))
	p.metrics.SetSecondsSinceLastTick(staleFor.Seconds())

	if staleFor < p.stallThreshold {
		return
	}

	p.stallsDetected.Add(1)
	p.metrics.RecordStallDetected()
	p.logger.Warn("ratelimit-probe: WATCHDOG detected stalled probe loop — last tick is stale",
		"stale_for", staleFor.Round(time.Second).String(),
		"stall_threshold", p.stallThreshold.String(),
		"ticks_completed", p.tickCount.Load())
	if p.notifier != nil {
		p.notifier.NotifyProbeStalled(
			staleFor.Round(time.Second).String(),
			p.stallThreshold.String())
	}
}

// isDue returns true if the account is due for a probe based on its last result.
func (p *rateLimitProbe) isDue(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	last, probed := p.lastProbe[name]
	if !probed {
		return true
	}

	// Back off when utilization is high or probe is consistently failing.
	prev := p.lastState[name]
	interval := probeInterval
	if prev.ProbeError != nil {
		interval = probeSafetyNet
	} else if prev.Utilization5h > 0.9 {
		interval = probeIntervalHighUtil
	}

	return time.Since(last) >= interval
}

// readToken reads the OAuth token from a k8s secret in the controller namespace.
func (p *rateLimitProbe) readToken(ctx context.Context, secretName string) (string, error) {
	secret, err := p.k8sClient.CoreV1().Secrets(p.namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get secret %s: %w", secretName, err)
	}
	token := strings.TrimSpace(string(secret.Data["token"]))
	if token == "" {
		return "", fmt.Errorf("secret %s has empty token field", secretName)
	}
	return token, nil
}

// probe makes a single max_tokens=1 call to the Anthropic API and parses the
// rate-limit headers. It never returns an error for clean API responses
// (including 429s); errors indicate probe infrastructure failures only.
func (p *rateLimitProbe) probe(ctx context.Context, token string) probeResult {
	result := probeResult{CheckedAt: time.Now()}

	body, _ := json.Marshal(map[string]any{
		"model":      probeModel,
		"max_tokens": 1,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicProbeURL, bytes.NewReader(body))
	if err != nil {
		result.ProbeError = fmt.Errorf("build request: %w", err)
		return result
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		result.ProbeError = fmt.Errorf("request failed: %w", err)
		return result
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); resp.Body.Close() }()

	result.HTTPStatus = resp.StatusCode
	parseProbeHeaders(resp.Header, &result)

	// Non-2xx, non-429 responses are probe errors (auth failure, server error, etc.).
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusTooManyRequests {
		result.ProbeError = fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	return result
}

// parseProbeHeaders extracts all anthropic-ratelimit-unified-* headers into result.
func parseProbeHeaders(h http.Header, result *probeResult) {
	result.Status = h.Get("anthropic-ratelimit-unified-status")
	result.Status5h = h.Get("anthropic-ratelimit-unified-5h-status")
	result.Status7d = h.Get("anthropic-ratelimit-unified-7d-status")
	result.RepresentativeClaim = h.Get("anthropic-ratelimit-unified-representative-claim")
	result.OverageStatus = h.Get("anthropic-ratelimit-unified-overage-status")
	result.OverageReason = h.Get("anthropic-ratelimit-unified-overage-disabled-reason")

	result.Utilization5h = parseFloat(h.Get("anthropic-ratelimit-unified-5h-utilization"))
	result.Utilization7d = parseFloat(h.Get("anthropic-ratelimit-unified-7d-utilization"))
	result.Reset5h = parseInt64(h.Get("anthropic-ratelimit-unified-5h-reset"))
	result.Reset7d = parseInt64(h.Get("anthropic-ratelimit-unified-7d-reset"))
	result.Reset = parseInt64(h.Get("anthropic-ratelimit-unified-reset"))

	if ra := h.Get("retry-after"); ra != "" {
		if secs, err := strconv.ParseInt(ra, 10, 64); err == nil {
			result.RetryAfter = time.Duration(secs) * time.Second
		}
	}

	result.ModelGroups = parseModelGroupHeaders(h)
}

// modelGroupHeaderPrefix is the shared prefix of every unified rate-limit header.
// A per-model-group header appends "<window>_<group>-<metric>", e.g.
// "anthropic-ratelimit-unified-7d_fable-utilization".
const modelGroupHeaderPrefix = "anthropic-ratelimit-unified-"

// parseModelGroupHeaders scans all response headers for the group-suffixed
// unified rate-limit headers Anthropic emits per model family and returns a map
// keyed "<window>_<group>" (e.g. "7d_fable"). It is GENERIC by design: it does
// not enumerate window or group names, so a new model group (or a renamed one)
// is captured with no code change — the one thing that would silently no-op if we
// hardcoded a guessed key (ka-0000003806).
//
// Recognized shape (case-insensitive, canonicalized by net/http):
//
//	anthropic-ratelimit-unified-<window>_<group>-<metric>
//
// where <window> is a rolling-window token containing no underscore (e.g. "5h",
// "7d"), <group> is the model family (e.g. "opus", "sonnet", "fable"), and
// <metric> is one of utilization | reset | status. The underscore between window
// and group is what distinguishes a per-group header from the aggregate
// per-window headers (unified-7d-utilization has no underscore and is skipped
// here — it is already handled above). Headers that do not match the shape, or
// carry an unknown metric, are ignored. Returns nil when no group headers are
// present so the common (older-account) path allocates nothing.
func parseModelGroupHeaders(h http.Header) map[string]modelGroupLimit {
	var groups map[string]modelGroupLimit
	for name, vals := range h {
		if len(vals) == 0 {
			continue
		}
		// net/http canonicalizes header names (Title-Case); lower-case for the
		// prefix/suffix match against the all-lowercase Anthropic header names.
		lname := strings.ToLower(name)
		suffix, ok := strings.CutPrefix(lname, modelGroupHeaderPrefix)
		if !ok {
			continue
		}
		// suffix is "<window>_<group>-<metric>". Split off the metric first.
		key, metric, ok := strings.Cut(suffix, "-")
		if !ok {
			continue // no metric segment — not a per-group header
		}
		// key is "<window>_<group>". The underscore is mandatory: its absence
		// means this is an aggregate per-window header (e.g. "7d-utilization"),
		// already parsed by the caller.
		window, group, ok := strings.Cut(key, "_")
		if !ok || window == "" || group == "" {
			continue
		}
		if groups == nil {
			groups = make(map[string]modelGroupLimit)
		}
		g, seen := groups[key]
		if !seen {
			g = modelGroupLimit{Window: window, Group: group}
		}
		val := vals[0]
		switch metric {
		case "utilization":
			g.Utilization = parseFloat(val)
		case "reset":
			g.Reset = parseInt64(val)
		case "status":
			g.Status = val
		default:
			// Unknown metric (e.g. a future header) — record the group so its
			// presence is visible, but do not fail. Skip storing an unknown field.
			if !seen {
				groups[key] = g
			}
			continue
		}
		groups[key] = g
	}
	return groups
}

// encodeModelGroupLimits serializes the per-model-group probe slices to the
// compact JSON stored in the account bead's `model_group_limits` field. The
// object is keyed "<window>_<group>" (e.g. "7d_fable") so a consumer can look up
// a specific group/window directly. Returns "" for a nil/empty map so the caller
// leaves the field unset (older accounts unchanged). Marshal of a map produces
// keys in sorted order (encoding/json), so the same probe state always yields the
// same string — no spurious bead-field churn between identical probes.
func encodeModelGroupLimits(groups map[string]modelGroupLimit) string {
	if len(groups) == 0 {
		return ""
	}
	b, err := json.Marshal(groups)
	if err != nil {
		return ""
	}
	return string(b)
}

// handleResult processes a probe result: updates the account bead, detects
// status transitions, and fires NATS events.
func (p *rateLimitProbe) handleResult(ctx context.Context, name string, acct beadsapi.AccountInfo, result probeResult) {
	p.mu.Lock()
	prev, hadPrev := p.lastState[name]
	p.lastProbe[name] = result.CheckedAt
	p.lastState[name] = result
	p.mu.Unlock()

	log := p.logger.With("account", name)

	if result.ProbeError != nil {
		log.Warn("probe failed", "error", result.ProbeError, "http_status", result.HTTPStatus)
		// A 401/403 is a TERMINAL auth rejection, not a transient blip: the
		// OAuth token is revoked or the org has disabled Claude Code access
		// (the matthewbaker1110 hard error, ka-0000002085). Persist
		// probe_auth_status="rejected" so switch-target selection excludes the
		// account — a rate-limit-healthy account can still be auth-dead. Any
		// OTHER probe error (network failure, 5xx, timeout) is transient and
		// must NOT condemn the account, so we leave probe_auth_status untouched.
		if s := probeAuthStatus(result); s == "rejected" {
			beadID, err := p.findAccountBeadID(ctx, name)
			if err != nil {
				log.Warn("could not find account bead ID, skipping probe_auth_status update", "error", err)
			} else if beadID != "" {
				if err := p.accounts.UpdateBeadFields(ctx, beadID, map[string]string{"probe_auth_status": s}); err != nil {
					log.Warn("failed to persist probe_auth_status=rejected", "error", err)
				} else {
					log.Warn("account probe returned a hard auth rejection; marking probe_auth_status=rejected so rotation avoids it",
						"http_status", result.HTTPStatus)
				}
			}
		}
		return
	}

	log.Info("probe complete",
		"status", result.Status,
		"utilization_5h", result.Utilization5h,
		"utilization_7d", result.Utilization7d,
		"representative_claim", result.RepresentativeClaim,
		"http_status", result.HTTPStatus)

	// Update account bead fields with current probe data. probe_auth_status
	// records the read-only probe's terminal-auth verdict — "ok" here because a
	// nil ProbeError means the probe authenticated cleanly (HTTP 200 or a 429
	// throttle, both of which prove the credential is live). This CLEARS any
	// stale "rejected" left by an earlier hard-auth failure once the account can
	// authenticate again (ka-0000002085).
	fields := map[string]string{
		"throttle_status":         result.Status,
		"probe_auth_status":       "ok",
		"utilization_5h":          strconv.FormatFloat(result.Utilization5h, 'f', 4, 64),
		"utilization_7d":          strconv.FormatFloat(result.Utilization7d, 'f', 4, 64),
		"representative_claim":    result.RepresentativeClaim,
		"overage_status":          result.OverageStatus,
		"overage_disabled_reason": result.OverageReason,
		"probe_checked_at":        result.CheckedAt.UTC().Format(time.RFC3339),
	}
	if result.Reset > 0 {
		resetAt := time.Unix(result.Reset, 0).UTC().Format(time.RFC3339)
		fields["throttle_reset_at"] = resetAt
	}
	if result.Reset5h > 0 {
		fields["throttle_reset_5h_at"] = time.Unix(result.Reset5h, 0).UTC().Format(time.RFC3339)
	}
	if result.Reset7d > 0 {
		fields["throttle_reset_7d_at"] = time.Unix(result.Reset7d, 0).UTC().Format(time.RFC3339)
	}
	// Persist the per-model-group split (ka-0000003806) as a single self-describing
	// JSON field rather than N flat suffixed fields. One field is fully replaced on
	// every probe, so a group that drops out of a response leaves no stale
	// per-group field behind (the flat-field alternative accumulates dead keys the
	// spread selector would keep reading). Downstream consumers (gb account limits,
	// the fable-aware spread gate) enumerate the map without guessing which group
	// keys exist. Empty/absent groups → the field is left unset so older accounts
	// are unchanged.
	if enc := encodeModelGroupLimits(result.ModelGroups); enc != "" {
		fields["model_group_limits"] = enc
	}

	// Reconcile the legacy `status`/`exhausted_at` fields with the probe state.
	// Without this writeback, status="exhausted" set by auth_failover.Handle
	// persists indefinitely after the throttle resets — buildProjectAccountOverride
	// then excludes actually-healthy accounts as candidates, leading to the
	// agent-looping incident on 2026-04-29 (ka-0000005198).
	reconcileLegacyStatusFields(acct.Fields, result, fields)

	// Find the account bead ID so we can update fields.
	beadID, err := p.findAccountBeadID(ctx, name)
	if err != nil {
		log.Warn("could not find account bead ID, skipping field update", "error", err)
	} else if beadID != "" {
		if err := p.accounts.UpdateBeadFields(ctx, beadID, fields); err != nil {
			log.Warn("failed to update account bead fields", "error", err)
		}
	}

	// Detect transitions and publish NATS events.
	if !hadPrev {
		return
	}
	wasThrottled := prev.Throttled()
	isThrottled := result.Throttled()

	if !wasThrottled && isThrottled {
		// allowed → rejected: failover signal.
		log.Info("account transitioned to throttled, publishing failover event")
		if p.notifier != nil {
			resetAt := ""
			if result.Reset > 0 {
				resetAt = time.Unix(result.Reset, 0).UTC().Format(time.RFC3339)
			}
			p.notifier.NotifyThrottled(name, resetAt)
		}
	} else if wasThrottled && result.Status == "allowed" {
		// rejected → allowed (clean): switchback signal. Only fire when
		// status is fully "allowed" — "allowed_warning" means high
		// utilization and the account could be rejected again immediately.
		// Stay failed over until usage actually resets.
		log.Info("account transitioned to allowed, publishing switchback event")
		if p.notifier != nil {
			p.notifier.NotifyRecovered(name)
		}
	} else if wasThrottled && result.Status == "allowed_warning" {
		// rejected → allowed_warning: still at high utilization, do NOT
		// trigger recovery. Log for observability but stay in failover.
		log.Info("account moved to allowed_warning but still at high utilization, staying in failover",
			"status", result.Status,
			"utilization_5h", result.Utilization5h)
	}

	// Soft failover: detect warming threshold crossing.
	// Read per-account threshold from bead field (default 0.80).
	threshold := defaultSoftFailoverThreshold
	if t := acct.Fields["soft_failover_threshold"]; t != "" {
		if parsed, err := strconv.ParseFloat(t, 64); err == nil && parsed > 0 && parsed < 1 {
			threshold = parsed
		}
	}

	prevSoftState := acct.Fields["soft_failover_state"]
	desiredSoftState := desiredSoftFailoverState(prevSoftState, result, threshold)

	if desiredSoftState != prevSoftState {
		switch desiredSoftState {
		case "warming":
			log.Info("account crossed soft failover threshold, publishing warming event",
				"utilization_5h", result.Utilization5h, "threshold", threshold)
			if p.notifier != nil {
				p.notifier.NotifyWarming(name, result.Utilization5h, threshold)
			}
		case "normal":
			log.Info("account dropped below soft failover threshold, publishing cooled event",
				"utilization_5h", result.Utilization5h, "threshold", threshold)
			if p.notifier != nil {
				p.notifier.NotifyCooled(name, result.Utilization5h, threshold)
			}
		}
		if beadID != "" {
			updateFields := map[string]string{"soft_failover_state": desiredSoftState}
			if err := p.accounts.UpdateBeadFields(ctx, beadID, updateFields); err != nil {
				log.Warn("failed to update soft_failover_state", "error", err, "desired", desiredSoftState)
			}
		}
	}

}

// findAccountBeadID returns the bead ID for the account with the given title.
func (p *rateLimitProbe) findAccountBeadID(ctx context.Context, name string) (string, error) {
	result, err := p.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"account"},
		Statuses: []string{"open"},
		Limit:    50,
	})
	if err != nil {
		return "", err
	}
	for _, b := range result.Beads {
		if b.Title == name {
			return b.ID, nil
		}
	}
	return "", nil
}

// exhaustedUtilization is the 7-day utilization at which an account is
// considered exhausted even if Anthropic is still serving requests
// (throttle_status != "rejected"). At 1.0 the account is at or above its
// rolling 7-day budget and any further usage will hit a rejection.
const exhaustedUtilization = 1.0

// billingTypeSubscription and billingTypeMetered are the two recognized values
// of the account bead's `billing_type` field. Subscription is the default for
// the whole gardenio fleet (Max OAuth tokens): overage billing being off is a
// benign steady-state, so out_of_credits must NEVER hold such an account in
// `exhausted` (ka-0000003350). Metered denotes a genuine pay-as-you-go account
// where out_of_credits means real inference 429s until credits are topped up —
// there the reactive hold is correct.
const (
	billingTypeSubscription = "subscription"
	billingTypeMetered      = "metered"
)

// billingType returns an account bead's billing model from its `billing_type`
// field, defaulting to "subscription" when the field is empty or unset.
//
// The default direction is load-bearing and operator-approved (ka-0000003350,
// 2026-07-06 Slack): the entire gardenio fleet is subscription-billed, and every
// existing account bead predates this field, so a missing value MUST resolve to
// subscription. Defaulting to metered would re-strand every account carrying
// out_of_credits — the exact fleet-wide rate-limit incident this fix closes.
// The sole metered account (litellm-bedrock) is Bedrock-backed and structurally
// probe-skipped (isBedrockBackedAccount), so it never reaches the clear gates
// regardless; the field exists so a future non-Bedrock metered account can opt
// back into the reactive hold explicitly.
func billingType(fields map[string]string) string {
	if bt := fields["billing_type"]; bt != "" {
		return bt
	}
	return billingTypeSubscription
}

// overageOutOfCredits reports whether an account's overage headers indicate it
// is rejected specifically because it is OUT OF BILLING CREDITS AND it is a
// metered (pay-as-you-go) account for which that state is genuinely terminal —
// a billing-action-required condition, distinct from a transient rate-limit
// window that resets on its own.
//
// The billing-type gate is what makes the reactive hold SAFE (ka-0000003350).
// For a SUBSCRIPTION account, overage_disabled_reason=out_of_credits is a benign
// permanent property orthogonal to whether the account can serve — the fleet
// primary matthewbaker1110 carries it while serving 200s at ~50% utilization
// (ka-0000004929). Holding such an account in `exhausted` on a single transient
// 429 stranded the one healthy account and starved the whole fleet. So the hold
// fires ONLY for metered accounts; subscription oscillation is instead prevented
// by the pre-existing re-mark debounce (tryAcquireCooldown, 5-min), NOT a
// permanent freeze.
//
// This is also deliberately NARROWER than "overage_status == rejected". A
// healthy subscription account with pay-as-you-go overage simply switched off
// reports overage_status=rejected with reason=org_spend_cap_reached PERMANENTLY,
// while serving 200s at low utilization. See overageReasonBlocks for the same
// subscription-vs-metered distinction on the spawn path.
//
// It is used REACTIVELY: it never condemns an account on its own (an
// out_of_credits account can still serve 200s to a cheap probe call —
// ka-0000004929), it only blocks the probe from CLEARING an `exhausted` flag
// that a real billing 429 already earned on a metered account, which is what
// drives the active↔exhausted oscillation in ka-0000008164. Once credits are
// restored the header flips back to overage_status=allowed and every clear path
// resumes normally.
func overageOutOfCredits(billingType, overageStatus, overageReason string) bool {
	return billingType == billingTypeMetered &&
		overageStatus == "rejected" && overageReason == "out_of_credits"
}

// reconcileLegacyStatusFields updates `fields` to keep the legacy `status` and
// `exhausted_at` fields on an account bead in sync with the probe's view of
// rate-limit state. The probe is the single source of truth for account
// health; without this writeback the legacy fields drift relative to
// throttle_status and starve buildProjectAccountOverride of healthy
// candidates.
//
// Transitions:
//   - throttle_status="allowed" with status="exhausted" → clear status,
//     exhausted_at, and cooldown_until (account has fully recovered) — UNLESS
//     the account is out of billing credits (see below).
//   - throttle_status="rejected" or 7-day utilization >= 1.0, with
//     status != "exhausted" → set status="exhausted" and a fresh
//     exhausted_at if one isn't already recorded.
//
// Out-of-credits guard (ka-0000008164 / ka-0000003350): a METERED account can
// pass the rate-limit axis (throttle_status="allowed", utilization well under
// 1.0) while being OUT OF BILLING CREDITS (overage_status="rejected",
// reason="out_of_credits"). Real traffic still 429s on billing, so auth_failover
// marks it exhausted; without this guard the very next probe cleared status back
// to active, and the account oscillated active↔exhausted every ~minute, thrashing
// rotation_count/exhausted_at and advertising a misleading `active` status for a
// genuinely unusable account. While a metered account's overage is out_of_credits
// we hold the exhausted flag so it stays parked until credits are restored (a
// billing action). Scoped to out_of_credits AND metered: org_spend_cap_reached is
// a benign steady-state on healthy subscription accounts (overageReasonBlocks,
// ka-0000004929), and for a SUBSCRIPTION account out_of_credits is likewise benign
// (ka-0000003350) — both must clear normally, or the guard freezes the healthy
// fleet primary and starves every project (the incident this fix closes).
//
// "allowed_warning" is intentionally not treated as recovered (matches
// throttleRecovered in auth_failover.go) and not treated as exhausted on
// its own — the caller can still serve traffic.
func reconcileLegacyStatusFields(current map[string]string, result probeResult, fields map[string]string) {
	currentStatus := current["status"]
	switch {
	case result.Status == "allowed" && currentStatus == "exhausted" &&
		!overageOutOfCredits(billingType(current), result.OverageStatus, result.OverageReason):
		fields["status"] = ""
		fields["exhausted_at"] = ""
		fields["cooldown_until"] = ""
	case (result.Status == "rejected" || result.Utilization7d >= exhaustedUtilization) && currentStatus != "exhausted":
		fields["status"] = "exhausted"
		if current["exhausted_at"] == "" {
			fields["exhausted_at"] = result.CheckedAt.UTC().Format(time.RFC3339)
		}
	}
}

// isDisabledAccount reports whether an account bead is marked status=disabled.
// Disabled accounts are excluded from the probe loop — see ka-0000005148.
func isDisabledAccount(acct beadsapi.AccountInfo) bool {
	return acct.Status == "disabled"
}

// isBedrockBackedAccount reports whether an account routes through AWS Bedrock
// (typically via a litellm proxy). The Anthropic-shaped probe call cannot
// authenticate against Bedrock-backed accounts, so they are excluded from the
// probe loop to avoid recurring WARN noise.
func isBedrockBackedAccount(acct beadsapi.AccountInfo) bool {
	if strings.HasPrefix(acct.Model, "bedrock-") {
		return true
	}
	if strings.HasPrefix(acct.Fields["probe_model"], "bedrock-") {
		return true
	}
	return false
}

// parseFloat parses a header value as float64; returns 0 on empty or error.
func parseFloat(s string) float64 {
	if s == "" {
		return 0
	}
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

// parseInt64 parses a header value as int64; returns 0 on empty or error.
func parseInt64(s string) int64 {
	if s == "" {
		return 0
	}
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}
