// freezethaw_lifecycle.go wires the gVisor freeze/thaw verbs
// (podmanager.FreezeThawManager) into the controller's agent lifecycle:
// Freeze on graceful stop (dehydrate) and Thaw on spawn (wake), opt-in per
// agent via the runsc RuntimeClass override and gated behind
// cfg.FreezeThawEnabled.
//
// Epic ka-0000007417 (Phase 4: Productionize gVisor agent freeze/thaw),
// task ka-0000003688. Before this, FreezeThawManager.Freeze/.Thaw had zero
// production callers — they were proven library code (Phase 3, MRs
// !3485/!3488/!3490) with nothing in the live lifecycle invoking them.
//
// Design choices (deliberately conservative, because this is opt-in until a
// separate go/no-go):
//
//   - DEFAULT OFF. With cfg.FreezeThawEnabled=false the manager is nil and
//     every agent uses today's CreateAgentPod/DeleteAgentPod cold-start
//     lifecycle untouched. Flipping the flag off is the documented rollback
//     (epic success criterion 5).
//
//   - OPT-IN PER AGENT. Even with the flag on, an agent is only frozen/thawed
//     if it is pinned to the runsc RuntimeClass (RuntimeClassRunsc) via the
//     existing podspec.go RuntimeClassName plumbing (role/project/agent
//     override). Non-runsc agents never touch the freeze/thaw path.
//
//   - GRACEFUL FALLBACK. A freeze failure falls through to the normal pod
//     delete; a thaw failure falls through to a cold CreateAgentPod. A flipped
//     flag or a flaky node never wedges an agent — the worst case is a
//     cold start, which is exactly today's behaviour.
//
//   - SINGLE CYCLE. The path-b PoC is node-local and single-shot (snapshot
//     transport / multi-cycle re-freeze is tracked as "remaining work" in
//     restore-through-lifecycle.md). One freeze→thaw cycle per agent is
//     within scope (epic success criterion 4); re-freezing a thawed Pod B is
//     not, and is left to the snapshot-transport follow-up.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"gardenio/controller/internal/config"
	gardeniometrics "gardenio/controller/internal/metrics"
	"gardenio/controller/internal/podmanager"
	"gardenio/controller/internal/poolmanager"
	"gardenio/controller/internal/statusreporter"
)

// snapshotAWSConfigTimeout caps AWS credential/config resolution when installing
// the off-node S3 snapshot transport so a broken IRSA/IMDS environment surfaces
// as a warn + node-local fallback rather than wedging controller startup.
// Mirrors claudemore's awsConfigTimeout discipline.
const snapshotAWSConfigTimeout = 8 * time.Second

// installS3SnapshotTransport builds the S3-backed off-node SnapshotTransport on
// the controller's IRSA credentials (the identity the terraform grant attaches
// s3:PutObject/GetObject to) and installs it on the manager. Reuses
// CoopS3Region/CoopS3Endpoint the same way the rest of the controller resolves
// S3 (region + optional MinIO path-style endpoint). Any failure is returned so
// the caller keeps the node-local default — this never partially installs.
func installS3SnapshotTransport(cfg *config.Config, mgr *podmanager.FreezeThawManager, nodeExec podmanager.NodeExecutor, logger *slog.Logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), snapshotAWSConfigTimeout)
	defer cancel()

	var opts []func(*awsconfig.LoadOptions) error
	if cfg.CoopS3Region != "" {
		opts = append(opts, awsconfig.WithRegion(cfg.CoopS3Region))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return fmt.Errorf("load aws config: %w", err)
	}
	client := s3.NewFromConfig(awsCfg, snapshotS3EndpointOpts(cfg.CoopS3Endpoint)...)
	transport, err := podmanager.NewS3SnapshotTransportFromClient(nodeExec, client, cfg.SnapshotS3Bucket, cfg.SnapshotS3Prefix, 0, logger)
	if err != nil {
		return err
	}
	mgr.SetSnapshotTransport(transport)
	return nil
}

// snapshotS3EndpointOpts mirrors the controller's other S3 endpoint handling: a
// non-empty COOP_S3_ENDPOINT targets a custom endpoint with path-style
// addressing (in-box MinIO); empty leaves the default AWS S3 endpoint untouched.
func snapshotS3EndpointOpts(endpoint string) []func(*s3.Options) {
	if endpoint == "" {
		return nil
	}
	return []func(*s3.Options){
		func(o *s3.Options) {
			o.BaseEndpoint = &endpoint
			o.UsePathStyle = true
		},
	}
}

// freezeThawLifecycle adapts the podmanager freeze/thaw verbs to the
// controller's event handlers. A nil *freezeThawLifecycle (when the feature
// flag is off) means "freeze/thaw disabled" — all methods are safe no-ops that
// tell the caller to use the cold-start path.
type freezeThawLifecycle struct {
	mgr    *podmanager.FreezeThawManager
	logger *slog.Logger

	// pool is the warm runsc placeholder ("warm-B") pool (C2/C3,
	// ka-0000005266 / ka-0000004263). Nil when WARM_PLACEHOLDER_POOL_ENABLED is
	// off — the wake path then always takes the create-then-restore thaw
	// (today's behavior, always correct). When set, thaw() first tries to claim
	// a placeholder co-resident with the snapshot node and graft onto it,
	// skipping the multi-second kubelet pod-admission cost.
	pool *poolmanager.PlaceholderPool

	// roleAllowed is the WS3 canary role allowlist predicate (cfg.FreezeRoleAllowed;
	// task ka-0000000138). shouldFreeze consults it so a runsc agent whose role is
	// not in FREEZE_THAW_ROLE_ALLOWLIST is NOT frozen even though the substrate +
	// lifecycle flags are on — the per-role dial that lets the operator widen
	// freeze/thaw one role at a time. Nil (never set) or an empty allowlist means
	// "no role filter": every runsc role freezes, preserving pre-WS3 behaviour.
	// Mirrors the spawn-time isFreezeEligible check so the two seams never drift.
	roleAllowed func(role string) bool

	// tokenRefreshEnabled / tokenRefreshInterval / tokenRefreshNamespaces drive
	// the post-thaw credential refresher (bug ka-0000000518). When
	// tokenRefreshEnabled is true (cfg.PostThawTokenRefreshEnabled AND
	// freeze/thaw on), startTokenRefreshLoop runs RefreshThawedTokens on the
	// interval, re-minting + re-overlaying each live thawed agent's SA/IRSA warm-
	// copy token before its ~1h TTL expires. Default-off: the loop never starts
	// and no serviceaccounts/token RBAC is exercised.
	tokenRefreshEnabled    bool
	tokenRefreshInterval   time.Duration
	tokenRefreshNamespaces []string
}

// newFreezeThawLifecycle constructs the lifecycle adapter when the feature flag
// is enabled, wiring a probe-backed node executor and the default in-memory
// snapshot store. Returns nil when cfg.FreezeThawEnabled is false so callers can
// cheaply branch on `ft == nil`.
//
// The RehydrationTrigger is the NodeRehydrationTrigger (C3, ka-0000004017): it
// writes the thaw-epoch marker onto the agent's node-side thaw-state dir over
// the same probe NodeExecutor the verbs use, then gates Thaw on the agent-side
// gb+coop acks under cfg.ThawRehydrateDeadline (fail-closed, design C5). With it
// wired, a successful thaw is a warm boot — the restored agent re-mints
// credentials and reconnects NATS before resuming work, rather than running on
// stale Pod-A identity. (Before C3 this seam was nil: thaw restored the process
// tree but never re-credentialed it.)
func newFreezeThawLifecycle(cfg *config.Config, k8sClient kubernetes.Interface, nodeExec podmanager.NodeExecutor, logger *slog.Logger) *freezeThawLifecycle {
	if cfg == nil || !cfg.FreezeThawEnabled {
		return nil
	}
	ftLogger := logger.With("component", "freeze-thaw")
	rehydr := podmanager.NewNodeRehydrationTrigger(nodeExec, ftLogger)
	// Durable snapshot store (ka-0000001694): a ConfigMap-backed store so the
	// snapshot record HasSnapshot()/shouldThaw() consult survives a controller
	// pod restart between freeze and thaw. With the prior in-memory store, a
	// restart dropped the record and the frozen agent silently cold-started even
	// though the on-node checkpoint image + bundle were intact at
	// /var/lib/gardenio/{ckpt,bundle}/<agent>. The ConfigMaps live in the
	// controller namespace; the heavy artifacts stay on the node.
	snaps := podmanager.NewConfigMapSnapshotStore(k8sClient, cfg.Namespace)
	mgr := podmanager.NewFreezeThawManager(
		k8sClient,
		nodeExec,
		snaps,
		rehydr, // RehydrationTrigger wired by C3 (ka-0000004017)
		ftLogger,
	)
	if cfg.ThawRehydrateDeadline > 0 {
		mgr.RehydrateDeadline = cfg.ThawRehydrateDeadline
	}
	// Clone-aware Ready-wait budget for the FRESH create-then-restore Pod B
	// (ka-0000004449). The warm-B-MISS fallback inlines a cold init-clone into
	// Pod B's readiness window, which a repo-heavy clone (~2m50s on prod) blew
	// past the 180s default, timing out the thaw before the runsc restore ran.
	// Zero lets the manager use DefaultCreateRestorePodReadyTimeout (600s).
	if cfg.ThawCreateRestorePodReadyTimeout > 0 {
		mgr.CreateRestorePodReadyTimeout = cfg.ThawCreateRestorePodReadyTimeout
	}
	// Hard cap on the detached post-commit critical section (ka-0000001355
	// harden-thaw). Zero lets the manager compute runscTimeout + rehydrateDeadline
	// + margin, which already covers a healthy thaw.
	if cfg.ThawHardDeadline > 0 {
		mgr.ThawHardDeadline = cfg.ThawHardDeadline
	}
	// Hard cap on the whole freeze node exec (bug ka-0000004931). Bounds a wedge in
	// the unbounded MUTATING node copies (relocation cp / pre-checkpoint gofer
	// copies) so Freeze() fails closed instead of hanging the freeze goroutine.
	// Zero lets the manager compute runscTimeout + relocation margin, which covers
	// a healthy freeze.
	if cfg.FreezeHardDeadline > 0 {
		mgr.FreezeHardDeadline = cfg.FreezeHardDeadline
	}
	// In-sentry egress assertion targets (fix B, ka-0000008207 / bug ka-0000000514).
	// The thaw script's [6c] check TCP-dials these — the SAME endpoints the rehydrate
	// consumers reach (coop -> NATS, gb -> beads daemon) — from INSIDE the restored
	// sandbox to reject a link-only sentry at restore time instead of 180s later on
	// the ack gate. These are the controller's own NATS/beads endpoints, which in a
	// single-cluster gardenio deployment are the same services the agent pods consume.
	// Empty (NATS unconfigured) leaves the end-to-end dial off; the in-sentry default-
	// route check still runs target-free.
	mgr.NatsURL = cfg.NatsURL
	mgr.BeadsHTTPAddr = cfg.BeadsHTTPAddr
	// C2 interruptible freeze (ka-0000005834): only arm the abort seam when the
	// dedicated flag is on. Default-off keeps the in-flight registry inert and
	// AbortFreeze a no-op — freezes run to completion exactly as in Phase 4.
	mgr.InterruptEnabled = cfg.FreezeInterruptEnabled
	// TOCTOU e2e test seam (ka-0000000996): a positive FREEZE_POST_PUT_DELAY
	// widens the post-Put window so the abort-races-completion e2e can land an
	// interrupt inside it on the real substrate. Default 0 is inert (today's
	// behaviour); only consulted when InterruptEnabled is on.
	mgr.PostPutDelay = cfg.FreezePostPutDelay
	// Pre-freeze watcher-liveness guard (ka-0000006667): only refuse to freeze a
	// not-yet-armed pod when the dedicated flag is on. Default-off preserves the
	// Phase-4 behaviour (every runsc stop freezes), so this can be rolled back by
	// flipping the env without a redeploy of the guard logic.
	mgr.ArmedGuardEnabled = cfg.FreezeArmedGuardEnabled
	// Direction [A] (ka-0000006667): a positive FREEZE_ARMED_WAIT turns the guard
	// from a single-shot probe into a bounded wait-for-armed gate, so a graceful
	// stop that arrives before a slow-booting agent loop has armed its watchers
	// waits for liveness rather than cold-starting. Zero (default) preserves the
	// single-shot probe. Only meaningful when ArmedGuardEnabled is also on.
	mgr.ArmedWait = cfg.FreezeArmedWait
	// A fat-fingered FREEZE_HARD_DEADLINE (missing unit, negative) is dropped to
	// the 0 "compute default" sentinel by config parsing, and the manager's `> 0`
	// override guard treats a non-positive value as unset — so the override
	// silently reverts to the computed cap. Echo that explicitly at startup so an
	// operator who set the knob but got the default sees WHY (task ka-0000006783).
	if cfg.FreezeHardDeadlineInvalid {
		logger.Warn("gVisor freeze: FREEZE_HARD_DEADLINE set but ineffective (unparseable or non-positive); reverting to computed cap",
			"freezeHardDeadlineEffective", mgr.FreezeHardDeadlineEffective())
	}
	// Off-node snapshot transport (Δ5-B2, ka-0000006160). When a bucket is
	// configured, swap the node-local no-op for the S3-backed transport so a
	// frozen agent can thaw on a DIFFERENT node (Push uploads image+bundle at
	// freeze, Pull re-materializes them at thaw, sha256 end-to-end, fail-closed).
	// On ANY S3-config failure we log and KEEP the node-local default — freeze/
	// thaw stay node-local and an off-node thaw degrades to a cold start, never a
	// wedge. Empty bucket (default) leaves the seam exactly as B1 shipped it.
	if cfg.SnapshotS3Bucket != "" {
		if err := installS3SnapshotTransport(cfg, mgr, nodeExec, ftLogger); err != nil {
			ftLogger.Warn("gVisor freeze/thaw: S3 snapshot transport requested but not installed; staying node-local (off-node thaw will cold-start)",
				"bucket", cfg.SnapshotS3Bucket, "error", err)
		} else {
			ftLogger.Info("gVisor freeze/thaw: off-node S3 snapshot transport installed",
				"bucket", cfg.SnapshotS3Bucket, "prefix", cfg.SnapshotS3Prefix)
		}
	}
	logger.Info("gVisor freeze/thaw lifecycle enabled (opt-in per runsc agent)",
		// Log the EFFECTIVE deadline (the getter falls back to
		// DefaultRehydrateDeadline when the field is 0), not the raw field — a
		// raw 0 here is a red herring that read as "deadline zeroed" and sent
		// prior crews chasing a phantom (bug ka-0000007916).
		"rehydrateDeadline", mgr.RehydrateDeadlineEffective(),
		"createRestorePodReadyTimeout", mgr.CreateRestorePodReadyTimeoutEffective(),
		// Effective BASELINE freeze hard-deadline (operator override when
		// positive, else the count-aware computed cap at freezeBaselineContainerCount).
		// The live path sizes it per-pod above this floor; echoing the baseline +
		// whether an override is in force mirrors the rehydrate/create-restore
		// timeouts, which previously had a startup echo and this one did not
		// (task ka-0000006783). freezeHardDeadlineOverride distinguishes a computed
		// cap from an operator-pinned one at a glance.
		"freezeHardDeadline", mgr.FreezeHardDeadlineEffective(),
		"freezeHardDeadlineOverride", cfg.FreezeHardDeadline > 0,
		"interruptEnabled", mgr.InterruptEnabled,
		"postPutDelay", mgr.PostPutDelay,
		"armedGuardEnabled", mgr.ArmedGuardEnabled,
		"armedWait", mgr.ArmedWait,
		// WS3 canary allowlist (ka-0000000138): echo which roles freeze so an
		// operator sees at a glance whether the fleet-wide default or a canary
		// subset is in force. Empty = no filter (every runsc role freezes).
		"roleAllowlist", cfg.FreezeThawRoleAllowlist)
	// Post-thaw token refresher namespaces (bug ka-0000000518): the controller
	// namespace plus, when configured, the gVisor namespace — the same set the
	// reconciler/status reporter enumerate thawed placeholder pods in.
	tokenRefreshNamespaces := []string{cfg.Namespace}
	if cfg.GVisorNamespace != "" && cfg.GVisorNamespace != cfg.Namespace {
		tokenRefreshNamespaces = append(tokenRefreshNamespaces, cfg.GVisorNamespace)
	}
	tokenRefreshInterval := cfg.PostThawTokenRefreshInterval
	if tokenRefreshInterval <= 0 {
		tokenRefreshInterval = 30 * time.Minute
	}
	if cfg.PostThawTokenRefreshEnabled {
		logger.Info("gVisor post-thaw token refresher enabled (bug ka-0000000518): periodically re-mints + re-overlays live thawed agents' SA/IRSA warm-copy tokens so they do not expire ~1h post-thaw",
			"interval", tokenRefreshInterval, "namespaces", tokenRefreshNamespaces)
	}

	return &freezeThawLifecycle{
		mgr:                    mgr,
		logger:                 ftLogger,
		roleAllowed:            cfg.FreezeRoleAllowed,
		tokenRefreshEnabled:    cfg.PostThawTokenRefreshEnabled,
		tokenRefreshInterval:   tokenRefreshInterval,
		tokenRefreshNamespaces: tokenRefreshNamespaces,
	}
}

// setPlaceholderPool wires the warm-B placeholder pool into the wake path (C2,
// ka-0000005266). Called from cmd/controller after the pool is constructed (only
// when WARM_PLACEHOLDER_POOL_ENABLED is on). A nil pool leaves thaw() on the
// create-then-restore path. Safe on a nil lifecycle (flag off).
func (f *freezeThawLifecycle) setPlaceholderPool(pool *poolmanager.PlaceholderPool) {
	if f == nil {
		return
	}
	f.pool = pool
}

// snapshotProvider exposes the FreezeThawManager as the warm-B pool's
// SnapshotProvider (ka-0000002219), so the pool can enumerate snapshot topology
// buckets and build graft-compatible placeholders. Returns nil on a nil
// lifecycle (flag off), leaving the pool on its legacy blind-placeholder path.
func (f *freezeThawLifecycle) snapshotProvider() poolmanager.SnapshotProvider {
	if f == nil {
		return nil
	}
	return f.mgr
}

// startPlaceholderPool launches the warm-B pool reconcile loop in a goroutine
// when a pool is wired. Safe to call on a nil lifecycle (flag off) or with no
// pool (WARM_PLACEHOLDER_POOL_ENABLED off) — both are no-ops. interval <= 0 lets
// the pool pick its default.
func (f *freezeThawLifecycle) startPlaceholderPool(ctx context.Context, interval time.Duration) {
	if f == nil || f.pool == nil {
		return
	}
	go f.pool.RunLoop(ctx, interval)
}

// startTokenRefreshLoop launches the post-thaw credential refresher in a
// goroutine (bug ka-0000000518). Safe on a nil lifecycle (freeze/thaw off) and a
// no-op when POST_THAW_TOKEN_REFRESH_ENABLED is off — both simply return, so the
// loop and its serviceaccounts/token RBAC are never exercised. The loop ticks on
// tokenRefreshInterval and re-mints + re-overlays every live thawed agent's
// SA/IRSA warm-copy token before its ~1h TTL expires; each cycle is best-effort
// (RefreshThawedTokens never returns fatal — a per-agent miss is logged and the
// agent keeps its prior token until the next tick).
func (f *freezeThawLifecycle) startTokenRefreshLoop(ctx context.Context) {
	if f == nil || !f.tokenRefreshEnabled {
		return
	}
	go func() {
		ticker := time.NewTicker(f.tokenRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				n := f.mgr.RefreshThawedTokens(ctx, f.tokenRefreshNamespaces)
				if n > 0 {
					f.logger.Info("post-thaw token refresh cycle complete",
						"agentsRefreshed", n)
				}
			}
		}
	}()
}

// shouldThaw reports whether a spawn should be served by thawing a prior
// snapshot rather than a cold CreateAgentPod. True iff freeze/thaw is enabled,
// the agent is pinned to the runsc RuntimeClass, and a snapshot exists for it
// (i.e. the agent was previously frozen). On the very first spawn there is no
// snapshot, so this is false and the agent cold-starts as usual.
//
// An explicit SessionResume=false (BOAT_SESSION_RESUME=0) VETOES the thaw: a
// gVisor thaw restores the frozen PROCESS — the exact in-memory conversation the
// caller asked to drop. The Slack `--fresh` flow stamps session_resume=false on
// the respawn bead precisely so a frozen runsc thread agent starts a clean
// session instead of thawing its old context (task ka-0000008188). A nil
// SessionResume (the default) leaves the thaw path unchanged, so this is a
// no-op for every spawn that does not explicitly opt out.
func (f *freezeThawLifecycle) shouldThaw(ctx context.Context, spec podmanager.AgentPodSpec) bool {
	if f == nil {
		return false
	}
	if spec.RuntimeClassName != podmanager.RuntimeClassRunsc {
		return false
	}
	if spec.SessionResume != nil && !*spec.SessionResume {
		return false
	}
	return f.mgr.HasSnapshot(ctx, spec.AgentName)
}

// hasSnapshot reports whether a persisted snapshot exists for the agent,
// independent of RuntimeClass. The reconciler's leaked-bead reaper uses this
// to exempt frozen-awaiting-thaw agents from being closed (bug ka-0000000114).
// A nil lifecycle (flag off) reports false.
func (f *freezeThawLifecycle) hasSnapshot(ctx context.Context, agentName string) bool {
	if f == nil {
		return false
	}
	return f.mgr.HasSnapshot(ctx, agentName)
}

// abortFreeze interrupts an in-flight freeze for the agent (C2, ka-0000005834),
// driven by an inbound user message targeting that agent. Returns true if a
// freeze was in flight and signalled (the agent will be left live to serve the
// message), false if nothing was in flight (no freeze to interrupt → the caller
// proceeds with its normal cold-start / route decision). Safe on a nil lifecycle
// (flag off) and a no-op when FreezeInterruptEnabled is off (the registry is
// never populated, so IsFreezing/AbortFreeze report false).
func (f *freezeThawLifecycle) abortFreeze(agentName string) bool {
	if f == nil {
		return false
	}
	return f.mgr.AbortFreeze(agentName)
}

// isFreezing reports whether a freeze for the agent is currently in flight.
// Safe on a nil lifecycle (flag off) → false. Used by the message-ingress seam
// to decide route-to-live vs. cold-start before committing.
func (f *freezeThawLifecycle) isFreezing(agentName string) bool {
	if f == nil {
		return false
	}
	return f.mgr.IsFreezing(agentName)
}

// isThawing reports whether a thaw for the agent is currently in flight (from
// the top of Thaw through its bounded rehydration hand-off). The reconciler's
// terminal-pod reaper uses this to exempt the restored placeholder Pod B while
// it is still rehydrating (bug ka-0000002058). Safe on a nil lifecycle (flag
// off) → false.
func (f *freezeThawLifecycle) isThawing(agentName string) bool {
	if f == nil {
		return false
	}
	return f.mgr.IsThawing(agentName)
}

// drainThaws blocks until every in-flight thaw's detached critical section has
// finished or the bounded drain budget expires, returning the count still in
// flight (0 = fully drained). The controller calls this on graceful shutdown
// (SIGTERM / leader-loss) so a roll does not abandon an in-flight
// restore/rehydrate mid-flight (ka-0000001355 harden-thaw). Safe on a nil
// lifecycle (flag off) → drains instantly.
func (f *freezeThawLifecycle) drainThaws(ctx context.Context, budget, poll time.Duration) int {
	if f == nil {
		return 0
	}
	if budget <= 0 {
		return f.mgr.InflightThawCount()
	}
	dctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	return f.mgr.DrainThaws(dctx, poll)
}

// inflightThawCount reports how many thaws are in flight. Safe on a nil
// lifecycle (flag off) → 0.
func (f *freezeThawLifecycle) inflightThawCount() int {
	if f == nil {
		return 0
	}
	return f.mgr.InflightThawCount()
}

// thaw restores the agent from its snapshot. Returns nil on success (the caller
// must NOT also CreateAgentPod) or an error (the caller falls back to cold
// start). The placeholder Pod B becomes the live agent pod.
//
// C2 (ka-0000005266) warm-B fast path: when a placeholder pool is wired, thaw
// first tries to claim a Ready warm placeholder CO-RESIDENT with the snapshot
// node (a path-b thaw is node-local) and graft the checkpoint straight onto it,
// skipping the dominant multi-second kubelet pod-admission cost (Create +
// waitPodReady). On a pool MISS — no Ready placeholder on that node — or any
// claim error, it falls back to the create-then-restore path verbatim, which is
// always correct (just not sub-second). The warm path is a pure latency
// optimization, never a correctness dependency.
//
// The claimed placeholder is promoted to the agent's live pod ATOMICALLY at
// claim time (drop warm-placeholder label + add agent identity), so the C3 pool
// reaper can never delete it mid-graft. From that point the placeholder is the
// agent's committed pod: if the graft then fails, Thaw returns a *ThawResult so
// the caller's partial-success path tears the pod down before cold-starting,
// identical to the create-path Pod B.
// The returned resumePath reports which mechanism served the wake — thaw-warm
// (R4, grafted onto a pre-Ready warm placeholder) or thaw-fresh (R3, restored
// onto a freshly-created Pod B) — so the caller can label the resume_latency
// histogram with the regime that actually ran (task ka-0000003577). It is always
// one of metrics.ResumePathThawWarm / ResumePathThawFresh; the cold fallback the
// caller takes on error is labelled by the caller, not here.
func (f *freezeThawLifecycle) thaw(ctx context.Context, spec podmanager.AgentPodSpec, beadID string) (*podmanager.ThawResult, string, error) {
	resumePath := gardeniometrics.ResumePathThawFresh
	if f.pool != nil {
		node, ok := f.mgr.SnapshotNode(ctx, spec.AgentName)
		// The warm placeholder must be graft-compatible with THIS agent's
		// snapshot — same mount shape, repo, and sidecars — or runsc mount
		// validation rejects it (ka-0000002219). Claim only within the snapshot's
		// topology bucket; a topology miss falls through to create-then-restore.
		topo, _ := f.mgr.SnapshotTopology(ctx, spec.AgentName)
		if ok {
			promo := podmanager.WarmPlaceholderPromotion{
				AgentName:    spec.AgentName,
				BeadID:       beadID,
				CanonicalPod: spec.PodName(),
			}
			podB, err := f.pool.ClaimWarmPlaceholderForThaw(ctx, node, topo, promo)
			switch {
			case err == nil:
				f.logger.Info("warm-B fast path: grafting onto pre-Ready placeholder",
					"agent", spec.AgentName, "node", node, "podB", podB)
				res, terr := f.mgr.Thaw(ctx, podmanager.ThawRequest{
					AgentName:      spec.AgentName,
					PlaceholderPod: podB,
				})
				// The placeholder was promoted to the agent's live pod ATOMICALLY
				// at claim time (it no longer carries the warm-placeholder label,
				// so the pool reaper will never clean it up). If the graft then
				// failed WITHOUT returning a result, main.go's partial-success
				// teardown — which only fires when res != nil — cannot see the pod,
				// and it would leak as a running duplicate alongside the cold-start
				// fallback (the ka-0000007549 class). Since this lifecycle owns the
				// promotion, it also owns teardown for that case: delete the
				// promoted pod here before returning the error so the caller cold-
				// starts cleanly. When res != nil (rehydration partial success),
				// leave teardown to main.go's existing branch (it deletes
				// res.PlaceholderPod, which is this same pod).
				if terr != nil && res == nil {
					f.logger.Error("warm-B graft failed before producing a result; tearing down promoted placeholder before cold start",
						"agent", spec.AgentName, "podB", podB, "error", terr)
					if derr := f.pool.DeletePromoted(ctx, podB); derr != nil {
						f.logger.Error("failed to delete promoted warm placeholder; cold start may orphan it",
							"agent", spec.AgentName, "podB", podB, "error", derr)
					}
				}
				return res, gardeniometrics.ResumePathThawWarm, terr
			case errors.Is(err, poolmanager.ErrNoWarmPlaceholder):
				f.logger.Info("warm-B pool miss; falling back to create-then-restore thaw",
					"agent", spec.AgentName, "node", node)
				// fall through to create-path thaw below (thaw-fresh).
			default:
				f.logger.Warn("warm-B claim errored; falling back to create-then-restore thaw",
					"agent", spec.AgentName, "node", node, "error", err)
				// fall through to create-path thaw below (thaw-fresh).
			}
		}
	}
	res, terr := f.mgr.Thaw(ctx, podmanager.ThawRequest{AgentName: spec.AgentName})
	return res, resumePath, terr
}

// shouldFreeze reports whether a graceful stop should checkpoint the agent
// before tearing down, by inspecting the live pod's RuntimeClass. True iff
// freeze/thaw is enabled, the running pod is pinned to runsc, AND the pod's role
// is permitted by the WS3 canary allowlist (f.roleAllowed; empty allowlist =
// every role allowed, the pre-WS3 behaviour). A missing pod or any lookup error
// yields false (nothing to freeze → normal teardown).
func (f *freezeThawLifecycle) shouldFreeze(ctx context.Context, k8sClient kubernetes.Interface, podName, namespace string) bool {
	if f == nil {
		return false
	}
	pod, err := k8sClient.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		// Pod already gone or unreadable — nothing to checkpoint.
		return false
	}
	if pod.Spec.RuntimeClassName == nil || *pod.Spec.RuntimeClassName != podmanager.RuntimeClassRunsc {
		return false
	}
	// WS3 canary allowlist (ka-0000000138): a runsc pod whose role is not
	// allow-listed keeps the cold-start lifecycle. roleAllowed is nil only in
	// tests that build the lifecycle by hand; treat nil as "no filter" so those
	// paths keep freezing exactly as before.
	if f.roleAllowed != nil && !f.roleAllowed(pod.Labels[podmanager.LabelRole]) {
		return false
	}
	return true
}

// freeze checkpoints the live agent pod and persists the snapshot. Returns the
// snapshot on success or an error. `runsc checkpoint --leave-running` keeps
// Pod A alive through the checkpoint, so the caller still performs its normal
// pod delete afterward to free the slot; the persisted snapshot is what a later
// spawn thaws from. The returned FreezeTiming carries the per-step attribution
// (runsc-checkpoint / relocation / persist) the caller feeds into the
// freeze_latency histogram (task ka-0000005147).
func (f *freezeThawLifecycle) freeze(ctx context.Context, agentName, beadID, podName, namespace string) (*podmanager.Snapshot, podmanager.FreezeTiming, error) {
	return f.mgr.Freeze(ctx, podmanager.FreezeRequest{
		AgentName: agentName,
		BeadID:    beadID,
		PodName:   podName,
		Namespace: namespace,
	})
}

// reportThawed writes a Running/Ready pod status for a successfully thawed
// agent, mirroring what CreateAgentPod's spawn reporting would have produced so
// downstream bead state stays consistent.
func (f *freezeThawLifecycle) reportThawed(ctx context.Context, status statusreporter.Reporter, agentBeadID string, res *podmanager.ThawResult) {
	if res == nil {
		return
	}
	_ = status.ReportPodStatus(ctx, agentBeadID, statusreporter.PodStatus{
		PodName:   res.PlaceholderPod,
		Namespace: res.Snapshot.Namespace,
		Phase:     string("Running"),
		Ready:     true,
	}, false)
}
