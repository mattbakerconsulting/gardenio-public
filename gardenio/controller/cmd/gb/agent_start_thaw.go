package main

// agent_start_thaw.go — the gb supervisor's side of NATS reconnect on thaw
// (rehydration-design Stage 3–4, ka-0000001901).
//
// When a checkpointed gVisor agent is thawed into a fresh kubelet-owned pod, the
// gb supervisor process never restarts, so the heartbeat it owns is stale (N6):
// the 5-min timer is intact but the next POST is up to 5 min away, and the
// presence tracker should see the thawed agent immediately.
//
// This file wires a thawepoch.Watcher (ConsumerGB) whose Handler evicts the
// daemon client's stale keep-alive pool, warms the beads VIP route, and fires
// one immediate heartbeat. The cred re-mint stages (Stage 1–2, ka-0000007211)
// plug a separate concern into the same ConsumerGB ack via composition where
// they land; here we own the connection half. The watcher only runs when a
// thaw-state dir is configured (gVisor pods).
//
// It used to restart a loopback nats-bridge (N5) that replayed the AVA cookie
// from the retired gb login cookie handoff; that bridge is gone (gd-bK5sSblLz9).

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"gardenio/controller/internal/thawepoch"
)

// thawRetryBackoffStart/Max bound the per-attempt backoff for a thaw stage's
// retry-under-deadline loop. Small so we re-probe kube-dns reachability often
// as a just-restored pod's netns warms up, capped so we never sleep through a
// large slice of the deadline (mirrors coop's RETRY_BACKOFF_* in
// nats_supervisor.rs).
const (
	thawRetryBackoffStart = 100 * time.Millisecond
	thawRetryBackoffMax   = 1 * time.Second
)

// thawConnectTimeoutDefault clamps each per-attempt connect inside
// retryUntilDeadline so a black-holed SYN to a stale cross-namespace ClusterIP
// during kube-proxy DNAT programming does NOT burn the full ~30s per-attempt
// budget that the underlying beadsapi http.Client carries — at ~30s/attempt the
// gb consumer gets ~2 probes against a 60s rehydrate deadline; at 2s/attempt it
// gets ~25, matching coop's THAW_CONNECT_TIMEOUT and the cycle-24 black-hole
// fail-pattern (ka-0000001637). Mirrors
// coop/crates/cli/src/transport/nats_supervisor.rs:THAW_CONNECT_TIMEOUT.
const thawConnectTimeoutDefault = 2 * time.Second

// thawConnectTimeoutEnv lets an operator widen or disable the per-attempt
// connect clamp without rebuilding gb (mirrors coop's COOP_THAW_NATS_CONNECT_TIMEOUT_MS).
// "0" disables the clamp (single-budget-per-attempt = the full ctx deadline);
// unset or unparseable falls back to thawConnectTimeoutDefault.
const thawConnectTimeoutEnv = "GB_THAW_CONNECT_TIMEOUT_MS"

// thawConnectTimeout returns the clamp resolved from the environment, falling
// back to thawConnectTimeoutDefault on missing or unparseable values. A "0"
// override means "no clamp" — each attempt gets the full remaining deadline
// (the pre-fix behavior), useful for narrow operator escape hatches.
func thawConnectTimeout() time.Duration {
	raw := os.Getenv(thawConnectTimeoutEnv)
	if raw == "" {
		return thawConnectTimeoutDefault
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return thawConnectTimeoutDefault
	}
	if n == 0 {
		return 0
	}
	if n < 0 {
		return thawConnectTimeoutDefault
	}
	return time.Duration(n) * time.Millisecond
}

// retryUntilDeadline retries fn with capped exponential backoff until it
// succeeds or ctx is done (the watcher gives ctx the marker deadline). On
// ctx-expiry it returns the last error so the caller still records a failed
// stage and fail-closes — we never turn a hard failure into a hang. This
// absorbs a transient cold-netns DNS miss on a just-restored pod (bug
// ka-0000001492) without changing the fail-closed contract.
//
// If ctx carries NO deadline, fn runs single-shot: retrying against an
// unbounded context could loop forever, and the production path always wraps
// the handler in `timeout(marker.Deadline())` (thawepoch.Watcher.runHandler),
// so a deadline-less ctx only happens in direct unit calls where single-shot
// is the right (boot-equivalent) semantics.
func retryUntilDeadline(ctx context.Context, fn func(context.Context) error) error {
	if _, ok := ctx.Deadline(); !ok {
		return fn(ctx)
	}
	clamp := thawConnectTimeout()
	backoff := thawRetryBackoffStart
	var lastErr error
	for {
		// Per-attempt context: clamp each attempt to thawConnectTimeout (default
		// 2s) so a black-holed SYN to a stale cross-namespace ClusterIP cannot
		// burn the full overall deadline (ka-0000001637 RANK4). The clamp never
		// extends the overall deadline — context.WithTimeout on an already-
		// deadlined parent honors the earlier of the two deadlines. Building a
		// FRESH per-attempt ctx each iteration is what triggers the live DNS
		// re-resolve on the next dial: Go's http.Transport / net.Resolver path
		// runs resolution under the request's ctx, so a fresh ctx never carries
		// any prior attempt's cached resolution state — coop's per-attempt
		// thaw_dial_url has the same shape (transport/nats_supervisor.rs).
		attemptCtx, attemptCancel := withAttemptClamp(ctx, clamp)
		err := fn(attemptCtx)
		attemptCancel()
		if err == nil {
			return nil
		}
		lastErr = err
		// Stop if the parent deadline has passed; surface the last error so the
		// caller still fail-closes (we never turn a hard failure into a hang).
		if ctx.Err() != nil {
			return lastErr
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return lastErr
		case <-timer.C:
		}
		backoff *= 2
		if backoff > thawRetryBackoffMax {
			backoff = thawRetryBackoffMax
		}
	}
}

// withAttemptClamp returns a per-attempt child context of parent. When clamp is
// > 0 the returned context times out after clamp OR when parent's deadline
// arrives, whichever is sooner; when clamp is 0 (operator override via
// GB_THAW_CONNECT_TIMEOUT_MS=0) the child inherits the parent's deadline
// unchanged. The caller MUST invoke the returned cancel func; the cancel is
// always non-nil even when clamp == 0 so the call shape stays uniform.
func withAttemptClamp(parent context.Context, clamp time.Duration) (context.Context, context.CancelFunc) {
	if clamp <= 0 {
		// Wrap in WithCancel so the caller can release per-attempt resources
		// (DNS lookups, dial goroutines) without waiting for the parent's
		// deadline — symmetric with the clamped branch.
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, clamp)
}

// thawAckHeadroomFraction caps how much of the marker deadline the handler's
// stages may consume, reserving the remainder for the watcher's ack write to
// land while the controller gate is still looking (ka-0000003746 RANK5). The
// gate anchors its window at marker-WRITE time while this handler's window is
// anchored at marker-DETECTION — up to one watcher poll tick later — and
// retryUntilDeadline otherwise retries to the very end of the handler window,
// so a last attempt that succeeds at deadline-epsilon would ack after the gate
// already fail-closed. Mirrors coop's RECONNECT_RETRY_FRACTION
// (coop/crates/cli/src/transport/nats_supervisor.rs), which caps the reconnect
// retry budget for exactly the same reason.
const thawAckHeadroomFraction = 0.85

// withAckHeadroom derives the stage-budget context from the handler ctx: when
// ctx carries a deadline (the watcher's marker-deadline wrap), the stages get
// thawAckHeadroomFraction of the remaining time; a deadline-less ctx (direct
// unit calls) passes through unchanged, preserving retryUntilDeadline's
// single-shot semantics. The caller MUST invoke the returned cancel.
func withAckHeadroom(parent context.Context) (context.Context, context.CancelFunc) {
	dl, ok := parent.Deadline()
	if !ok {
		return context.WithCancel(parent)
	}
	remaining := time.Until(dl)
	if remaining <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, time.Duration(float64(remaining)*thawAckHeadroomFraction))
}

// routeWarmDeadlineFraction is the share of the remaining rehydrate budget the
// VIP-route warm-up (Stage N4.6) is allowed to consume before yielding to the
// heartbeat. Bounding it to a fraction keeps the warm-up a best-effort PRIMER:
// the heartbeat — the one required ack-gate — always retains the majority of the
// budget to land the actual presence POST and to fail-close if the route never
// comes up. Half is generous: a programmed kube-proxy DNAT lands in the first
// probe or two (sub-second), so on the happy path the warm-up returns almost
// immediately and hands nearly the whole budget to the heartbeat anyway.
const routeWarmDeadlineFraction = 2

// routeWarmContext derives the best-effort warm-up budget from parent: when
// parent carries a deadline, the warm-up gets 1/routeWarmDeadlineFraction of the
// time remaining (so the heartbeat keeps the rest); when parent has no deadline
// (direct unit calls), the warm-up inherits parent unchanged and relies on
// retryUntilDeadline's single-shot semantics. The caller MUST invoke the
// returned cancel.
func routeWarmContext(parent context.Context) (context.Context, context.CancelFunc) {
	dl, ok := parent.Deadline()
	if !ok {
		return context.WithCancel(parent)
	}
	remaining := time.Until(dl)
	if remaining <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, remaining/routeWarmDeadlineFraction)
}

// thawDeps carries the live handles the thaw handler needs. Any of them may be
// nil; the handler degrades each stage to "skipped" rather than failing.
type thawDeps struct {
	// heartbeat fires one immediate presence heartbeat (N6). Required.
	heartbeat func(ctx context.Context) error
	// evictIdleConns evicts every idle keep-alive connection in the daemon HTTP
	// client pool. Called ONCE at the top of each thaw epoch BEFORE the first
	// heartbeat, so any pre-freeze conn pooled
	// against Pod-A's now-dead 4-tuple is dropped instead of reused into a
	// 30s-Timeout hang on the restored Pod-B netns (ka-0000001637 RANK3).
	// Optional: when nil (test wiring without a live beadsapi client), the
	// eviction step is skipped — the unit tests under agent_start_thaw_test.go
	// verify the hook fires in handler-level wiring.
	evictIdleConns func()
	// warmRoute makes ONE TCP-connect probe to the in-cluster beads ClusterIP VIP
	// to confirm/seed the cross-namespace route before the first heartbeat
	// (ka-0000002195). It is the cold-VIP-route companion to the DNS-preresolve:
	// the cached ClusterIP literal takes DNS off the cold path, but the first
	// connect to that VIP still black-holes until kube-proxy reprograms its
	// DNAT/conntrack on the just-restored netns. A successful probe proves the
	// route is live and seeds a valid conntrack 4-tuple, so the heartbeat POST
	// lands warm instead of burning the rehydrate deadline on a black-holed SYN.
	// One call == one attempt; the handler wraps it in retryUntilDeadline so a
	// still-cold route is re-probed under the same per-attempt clamp + backoff as
	// the heartbeat. Optional and non-fatal: when nil (test wiring without a live
	// beadsapi client) the warm step is skipped, and a probe that never lands does
	// NOT fail-close the thaw — only the heartbeat is the required ack-gate (so we
	// never turn a route-warm miss into a regression vs the pre-fix behavior).
	warmRoute func(ctx context.Context) error
	logger    *slog.Logger
}

// newGBThawHandler builds the ConsumerGB rehydration handler. On each epoch it
// fires one immediate heartbeat so the presence tracker re-discovers the agent
// without waiting for the 5-min tick (N6 → C4), after evicting idle conns and
// warming the beads VIP route. The heartbeat is required: its failure returns
// OK=false so the controller fails the thaw closed (C5).
func newGBThawHandler(deps thawDeps) thawepoch.Handler {
	log := deps.logger
	if log == nil {
		log = slog.Default()
	}
	return func(ctx context.Context, m thawepoch.Marker) (thawepoch.Ack, error) {
		stages := map[string]string{}
		var failures []string

		// Every retrying stage below runs under an ack-headroom cap: the
		// handler must return early enough that the watcher's ack write still
		// lands inside the controller gate's marker-write-anchored window
		// (ka-0000003746 RANK5, mirrors coop's RECONNECT_RETRY_FRACTION).
		sctx, scancel := withAckHeadroom(ctx)
		defer scancel()

		// Stage N4.5 — evict idle keep-alive conns BEFORE any post-thaw HTTP
		// (the heartbeat). On a
		// checkpoint/restore the gb supervisor process never restarts, so its
		// beadsapi client's keep-alive pool still holds TCP conns pinned to
		// Pod-A's dead 4-tuple. If we let the next request reuse one of those
		// conns into a black-holed (no-RST) peer, it hangs to the full 30s
		// http.Client.Timeout — fatal under a short rehydrate deadline. Eviction
		// is idempotent (a no-op on an empty pool) so the first thaw on a
		// freshly-booted pod is also safe. See ka-0000001637 RANK3.
		if deps.evictIdleConns != nil {
			deps.evictIdleConns()
			stages["evict_idle_conns"] = "ok"
		}

		// Stage N4.6 — warm the cross-namespace beads ClusterIP VIP route BEFORE
		// the first heartbeat (bug ka-0000002195). The DNS-preresolve dialer
		// (beadsapi thaw_dial.go) already takes DNS off the cold path by dialing
		// the cached ClusterIP literal, but a zero-DNS connect to that VIP still
		// black-holes until kube-proxy reprograms its DNAT/conntrack on the
		// just-restored netns — coop's external aws_sts probe acks OK in the same
		// thaw, isolating the gap to this cross-ns ClusterIP route. We make a cheap
		// TCP-connect probe to the VIP, retried under the SAME deadline + 2s
		// per-attempt clamp as the heartbeat, so it returns the instant the route
		// lands and seeds a valid conntrack 4-tuple. The heartbeat that follows
		// then lands on a warm route instead of timing out the rehydrate gate.
		//
		// Best-effort, NOT a required ack-gate: we bound the warm-up to a FRACTION
		// of the remaining deadline (routeWarmDeadlineFraction) so the heartbeat
		// keeps the budget that actually fails the thaw closed, and a warm-up that
		// never lands records "failed" WITHOUT appending to failures — it must
		// never regress a thaw that the heartbeat retry could still carry. On the
		// happy path the warm-up lands in one probe and the heartbeat is instant.
		if deps.warmRoute != nil {
			warmCtx, warmCancel := routeWarmContext(sctx)
			if err := retryUntilDeadline(warmCtx, deps.warmRoute); err != nil {
				stages["warm_vip_route"] = "failed"
				log.Warn("thaw: beads VIP route warm-up did not land in window; heartbeat will still retry",
					"epoch", m.Epoch, "err", err)
			} else {
				stages["warm_vip_route"] = "ok"
			}
			warmCancel()
		}

		// Stage N6 — fire one immediate heartbeat so presence re-arms now. The
		// heartbeat is an HTTP POST to the cross-namespace beads daemon (a bare
		// service name), so on a just-restored pod its resolver may still hit a
		// transient cold-netns DNS miss ("Temporary failure in name resolution")
		// for the first few seconds — the same EAI_AGAIN class that defeats
		// coop's NATS reconnect (bug ka-0000001492). Retry under the handler's
		// headroom-capped deadline (sctx: the watcher's m.Deadline() wrap ×
		// thawAckHeadroomFraction) so a transient miss is absorbed instead of
		// fail-closing the thaw, while the ack write still lands in-window.
		if deps.heartbeat != nil {
			if err := retryUntilDeadline(sctx, deps.heartbeat); err != nil {
				stages["heartbeat"] = "failed"
				failures = append(failures, fmt.Sprintf("heartbeat: %v", err))
				log.Warn("thaw: immediate heartbeat failed", "epoch", m.Epoch, "err", err)
			} else {
				stages["heartbeat"] = "ok"
			}
		} else {
			stages["heartbeat"] = "skipped"
		}

		ack := thawepoch.Ack{OK: len(failures) == 0, Stages: stages}
		if !ack.OK {
			ack.Error = fmt.Sprintf("gb thaw rehydration failed: %v", failures)
		}
		return ack, nil
	}
}

// startK8sGBThawWatcher wires the gb-side ConsumerGB thaw watcher into the k8s
// pod entrypoint (gb init -> runInitLoop and gb agent start --k8s ->
// runAgentStartK8s), mirroring the runLocal call site in agent_start.go.
//
// Without this, the watcher only ever ran on the --local operator path, so in
// every prod gVisor pod thaw-epoch.gb.armed / .done were never written and the
// {gb,coop} rehydrate gate could NEVER release — every thaw fail-closed on
// "failed consumers: coop, gb" (bug ka-0000004330, canary-proven by re-drive r22).
//
// The only required stage is the immediate heartbeat (N6). Arming thaw-epoch.gb.armed at boot also satisfies the
// controller's pre-freeze liveness guard (ka-0000006667) so a freeze taken in a
// prod pod mints a snapshot whose thaw the gb consumer can actually ack.
//
// A no-op when COOP_THAW_STATE_DIR is unset (non-gVisor pods). agentBeadID and
// agentName scope the heartbeat hook; cwd is the agent working directory.
func startK8sGBThawWatcher(ctx context.Context, agentBeadID, agentName, cwd string) {
	thawDir := os.Getenv("COOP_THAW_STATE_DIR")
	if thawDir == "" {
		return
	}
	// Boot prewarm of the in-cluster beads ClusterIP (bug ka-0000000696). We are
	// on a gVisor pod (COOP_THAW_STATE_DIR set) whose boot netns is healthy and
	// kube-DNS is reachable, so resolve the beads host ONCE now and cache the
	// ClusterIP literal in the daemon client's RAM. The gVisor freeze snapshots
	// that cache; on thaw the heartbeat POST dials the cached literal instead of
	// re-resolving the bare name on the cold post-restore netns where kube-DNS is
	// not yet routable (the EAI_AGAIN that times out the first heartbeat and
	// fail-closes the rehydrate gate). A no-op when daemon is nil (test wiring) or
	// the daemon URL is not an in-cluster Service host (EnableThawDNSPrewarm
	// guards that — an external URL already survives the thaw via the egress path
	// coop's aws_sts:ok proves is up). Bounded so a slow boot resolver cannot
	// stall the watcher arming.
	if daemon != nil {
		prewarmCtx, prewarmCancel := context.WithTimeout(ctx, 5*time.Second)
		daemon.EnableThawDNSPrewarm(prewarmCtx)
		prewarmCancel()
		if host, ip, on := daemon.ThawDNSPrewarmTarget(); on {
			slog.Default().Info("thaw: armed in-cluster beads DNS-preresolve",
				"host", host, "cached_ip", ip)
		}
	}
	startGBThawWatcher(ctx, thawDir, thawDeps{
		heartbeat: func(hctx context.Context) error {
			return emitAgentHook(hctx, agentBeadID, agentName, "PreToolUse", cwd)
		},
		// On thaw, evict the daemon HTTP client's keep-alive pool so the
		// heartbeat below dials fresh against Pod-B's live netns instead of
		// trying to reuse a conn pooled against Pod-A's dead 4-tuple. Guarded
		// against a nil daemon (the boot path constructs daemon before the
		// watcher would ever fire, but the test surface can elide it).
		evictIdleConns: func() {
			if daemon != nil {
				daemon.CloseIdleConnections()
			}
		},
		// On thaw, confirm/seed the cross-namespace beads ClusterIP VIP route
		// before the heartbeat (bug ka-0000002195). Probes the cached ClusterIP
		// literal (zero DNS) so a connect lands the instant kube-proxy reprograms
		// the VIP DNAT/conntrack on the cold restored netns — the cold-VIP-route
		// half the DNS-preresolve cannot cover. A no-op (returns nil) when daemon
		// is nil (test wiring) or the dialer is not armed (non-gVisor / external
		// URL), so it never touches the known-good external path.
		warmRoute: func(wctx context.Context) error {
			if daemon == nil {
				return nil
			}
			return daemon.WarmThawRouteOnce(wctx)
		},
		logger: slog.Default(),
	})
}

// startGBThawWatcher launches the ConsumerGB thaw watcher in the background when
// stateDir is non-empty (gVisor agent pods set COOP_THAW_STATE_DIR / the
// controller writes the marker there). A no-op otherwise.
func startGBThawWatcher(ctx context.Context, stateDir string, deps thawDeps) {
	if stateDir == "" {
		return
	}
	log := deps.logger
	if log == nil {
		log = slog.Default()
	}
	w := &thawepoch.Watcher{
		StateDir: stateDir,
		Consumer: thawepoch.ConsumerGB,
		Handler:  newGBThawHandler(deps),
		Logger:   log,
	}
	// Drop the armed sentinel so the controller's pre-freeze guard knows the gb
	// rehydration consumer is live (ka-0000006667): without a live watcher in the
	// pod, a freeze taken now would mint a snapshot whose thaw can never be acked.
	// Best-effort — a sentinel write failure must not stop the watcher arming.
	if err := thawepoch.WriteArmed(stateDir, thawepoch.ConsumerGB); err != nil {
		log.Warn("thaw: gb armed-sentinel write failed (freeze guard may skip this agent)", "err", err)
	}
	go func() {
		if err := w.Run(ctx); err != nil && ctx.Err() == nil {
			log.Warn("thaw: gb watcher stopped", "err", err)
		}
	}()
	log.Info("thaw: gb NATS-reconnect watcher started", "dir", stateDir)
}
