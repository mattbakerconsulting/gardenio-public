// Warm runsc placeholder ("warm-B") pool — sub-second-resume-integration.md C3
// (ka-0000004263). Maintains a per-node pool of already-Ready runsc placeholder
// pods so a later gVisor thaw can graft its checkpoint onto a warm placeholder
// instead of paying the kubelet pod-admission cost (create + waitPodReady, the
// dominant multi-second step) on the resume hot path.
//
// The pool is keyed by (NODE, TOPOLOGY): a snapshot's checkpoint image is
// node-local (Snapshot.Node, /var/lib/gardenio/ckpt/<agent>), so a warm
// placeholder can only serve a thaw whose snapshot lives on the same node; AND
// runsc RestoreValidateSpec requires the placeholder's mount-destination set,
// repo, and sidecars to match the checkpoint exactly (ka-0000002219), so a
// placeholder built blind (a bare /work emptyDir) cannot accept an arbitrary
// snapshot. To satisfy both, the reconciler — when a SnapshotProvider is wired —
// builds placeholders FROM existing snapshot templates, bucketed by
// (node, SnapshotTopologyKey), and the wake path only ever claims a placeholder
// whose topology label matches the thawing agent's snapshot. The init-clone in
// the snapshot template runs at fill time, so the repo is warm before the graft.
// Without a provider (or before any snapshot exists) it falls back to the legacy
// per-node blind bare-/work placeholder. It targets every gVisor node (discovered
// via the Ready gvisor-probe pods) and keeps MinSize Ready placeholders per
// bucket, refilling after every claim.
//
// Lifecycle is owned entirely here: warm placeholders carry no agent bead and no
// gardenio.io/agent label, so the reconciler's desired-vs-actual orphan sweep
// skips them. Default-off: a zero MinSize (or a nil manager) keeps the pool
// empty and the blast radius at zero — the wiring in cmd/controller (C2/C5)
// gates construction on WARM_PLACEHOLDER_POOL_ENABLED.
package poolmanager

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"gardenio/controller/internal/podmanager"
)

// DefaultProbeSelectorKey/Value select the per-node gVisor probe pods used to
// discover which nodes can host runsc placeholders. Matches probe-pod.yaml and
// podmanager.ProbeNodeExecutor's default selector.
const (
	DefaultProbeSelectorKey   = "app"
	DefaultProbeSelectorValue = "gvisor-probe"
)

// SnapshotProvider lets the warm-B pool enumerate the topology buckets it must
// keep warm and look up a thawing agent's topology key (ka-0000002219). It is
// satisfied by *podmanager.FreezeThawManager. Nil leaves the pool on its legacy
// blind-placeholder behavior (a bare /work emptyDir), which can only serve a
// thaw whose checkpoint mounts just /work — kept for back-compat and the
// no-snapshot bootstrap window.
type SnapshotProvider interface {
	// WarmBuckets returns the distinct (node, topology) buckets across all
	// persisted snapshots, each with a representative snapshot whose PodTemplate
	// produces a graft-compatible placeholder.
	WarmBuckets(ctx context.Context) ([]podmanager.WarmBucket, error)
	// SnapshotTopology returns the topology key for an agent's snapshot.
	SnapshotTopology(ctx context.Context, agentName string) (string, bool)
}

// PlaceholderPool maintains a per-node pool of Ready runsc placeholder pods.
//
// It is intentionally separate from Manager (the per-(project,role) prewarmed
// AGENT-BEAD pool): this pool manages raw k8s Pods on gVisor nodes, has no
// notion of role or bead, and is keyed by node. Both can run concurrently.
//
// Topology awareness (ka-0000002219): when a SnapshotProvider is wired, the
// pool builds placeholders FROM existing snapshots' templates — bucketed by
// (node, SnapshotTopologyKey) — so each placeholder's mount shape, init-clone
// repo, and sidecars match a real snapshot and a warm graft passes runsc mount
// validation. Without a provider it falls back to the legacy blind bare-/work
// placeholder (back-compat / bootstrap before any snapshot exists).
type PlaceholderPool struct {
	client    kubernetes.Interface
	namespace string
	image     string
	minSize   int
	nodeFloor int
	logger    *slog.Logger

	// snaps enumerates snapshot topology buckets for topology-aware fills. Nil
	// keeps the pool on the legacy blind-placeholder path.
	snaps SnapshotProvider

	// drainRate caps how many stale-image placeholders are cycled per Reconcile
	// pass across all buckets (ka-0000000890). Bounds the blast radius of a
	// fleet-wide image roll so it converges over several passes instead of
	// nuking the pool (or storming the API) at once. Defaults to
	// DefaultStaleDrainRate.
	drainRate int

	// maxConcurrent is the global ceiling on total live warm placeholders across
	// ALL buckets (ka-0000004377). The Reconcile deficit-fill clamps its minting
	// to the remaining budget (maxConcurrent - already-live placeholders) so
	// replenishment maintains depth without exceeding the concurrent-ephemeral cap
	// (ka-0000004129). Defaults to DefaultMaxConcurrent.
	maxConcurrent int

	// cycleRecorder optionally observes the count of stale placeholders cycled,
	// for the cycled-stale metric (ka-0000002975). Nil disables the metric (the
	// log line still fires).
	cycleRecorder CycleRecorder

	// claimRecorder optionally observes claim outcomes (hit/miss/error) on the
	// thaw wake path, for the warm_placeholder_claims_total metric
	// (ka-0000000550). Nil disables the metric (the log lines still fire).
	claimRecorder ClaimRecorder

	// probeSelector selects the per-node gVisor probe pods used for node
	// discovery. Defaults to {app: gvisor-probe}.
	probeSelector map[string]string

	// priorityClass, when non-empty, is stamped on every minted placeholder so a
	// cold real agent preempts an idle placeholder under node pressure
	// (ka-0000000722). Empty leaves placeholders at default priority.
	priorityClass string

	// imagePullSecrets, when non-empty, is stamped on every minted placeholder
	// so the runsc image pull authenticates to the private registry — the SAME
	// registry-pull-secret real agent pods carry via applyCommonConfig
	// (cfg.ImagePullSecrets). Without it the legacy bare builder's pod spec
	// carries no pull secret, so the kubelet falls back to an ANONYMOUS token
	// and 403s on any tag not already node-cached — every warm placeholder
	// ImagePullBackOffs the moment a new release tag lands (ka-0000000402). Empty
	// leaves placeholders unauthenticated (legacy behavior, fine for a public
	// image). Stamped at the pool layer for the same reason as priorityClass and
	// warmImage: it is a cluster-uniform concern, so applying it in createPod
	// keeps the signal uniform across the legacy and snapshot build paths.
	imagePullSecrets []string

	mu      sync.Mutex
	seq     int              // monotonic counter for unique placeholder names
	nowFunc func() time.Time // injectable clock for testing

	// recentlyMinted maps names of placeholders this pool minted on the PRIOR
	// pass — whose immediate post-Create podRejected() Get returned
	// "not-yet-terminal" (ka-0000000088) — to their (node,topology) bucketKey. On
	// the NEXT reconcile pass we reclassify each entry against what actually
	// became of the pod:
	//
	//   - LANDED TERMINAL (still present, Failed/Succeeded): the mint we thought
	//     succeeded actually landed Failed a moment later (the OutOfmemory-lands-
	//     async race) — count it as a delayed rejection so the backoff arms even
	//     though the mint pass already returned.
	//   - VANISHED (no longer present as a warm placeholder AND a confirming Get
	//     returns NotFound): the kubelet rejected it OutOfmemory and the terminal
	//     pod was garbage-collected before this pass could observe it in the
	//     terminal list (ka-0000005929 — the single-fat-node warm-B churn). The
	//     async-terminal reclassification above only fires while the Failed pod
	//     still exists on the API server; when pod-GC beats the 30s reconcile the
	//     rejection is invisible and the bucket re-mints a doomed batch forever.
	//     Counting a confirmed-vanished mint as a delayed rejection closes that
	//     gap so the backoff arms regardless of GC timing. The bucketKey is stored
	//     here precisely because a vanished pod's labels can no longer be read.
	//   - SURVIVED (present as Ready/pending/claimed, or Get finds it promoted to a
	//     live agent pod): cleared admission — pruned, never counted.
	//
	// Bounded to a single-pass lifetime (ka-0000005097): every entry recorded on
	// pass N is either reclassified (terminal or vanished → counted + deleted) or
	// pruned (survived → reached Ready/claimed/healthy-pending) on pass N+1, so
	// the map holds at most one pass's mints. This is what makes the delayed
	// reclassification SOUND: only a failure inside the mint-time race window
	// arms the backoff. A placeholder that survives its first pass has cleared
	// admission; a LATER death (eviction, runtime OOM, node drain) is a genuine
	// late failure, NOT a mint-time admission reject, and — being absent from
	// this map by then — never falsely arms the OOM-race backoff.
	recentlyMinted map[string]string

	// mintBackoff tracks per-(node,topology) bucket admission-failure backoff
	// (ka-0000000239). When a bucket's freshly-minted placeholders keep being
	// rejected by the kubelet (OutOfcpu: a node-pinned snapshot envelope that
	// cannot fit alongside the live agent on its node), minting pauses for an
	// exponentially-growing cooldown instead of re-storming a doomed batch every
	// pass. Cleared once the bucket reaches its Ready target. Guarded by mu.
	mintBackoff map[string]*backoffState
}

// backoffState is one bucket's admission-failure backoff: how many consecutive
// reconcile passes minted only terminal (rejected) placeholders, and the
// earliest time minting may resume.
type backoffState struct {
	failures int
	until    time.Time
}

// maxMintBackoff caps the admission-failure cooldown so a bucket that becomes
// fittable again (the live agent exits, freeing node CPU) is retried within a
// bounded window rather than parked indefinitely.
const maxMintBackoff = 30 * time.Minute

// CycleRecorder observes warm-placeholder cycle events for metrics
// (ka-0000002975). Satisfied by *metrics.PoolMetrics; nil is a no-op.
type CycleRecorder interface {
	// RecordWarmPlaceholderCycled records that count not-in-use stale-image
	// placeholders were cycled in the (node, topology) bucket on upgrade.
	RecordWarmPlaceholderCycled(node, topology string, count int)
}

// ClaimRecorder observes warm-B claim outcomes on the thaw wake path for the
// warm_placeholder_claims_total hit/miss/error metric (ka-0000000550).
// Satisfied by *metrics.PoolMetrics; nil is a no-op. Outcome values are the
// metrics.PoolOutcome* constants.
type ClaimRecorder interface {
	RecordWarmPlaceholderClaim(outcome, node, topology string) bool
}

// DefaultMaxConcurrent is the default global cap on the total number of live
// warm placeholder pods the pool holds across ALL (node,topology) buckets at
// once (ka-0000004377), applied when PlaceholderPoolConfig.MaxConcurrent is <= 0.
// It mirrors the ephemeral-concurrency ceiling the (since-deleted) RWX reaper
// enforced on ephemeral gardenio stacks (ka-0000004129) so the warm pool can never re-create
// the unbounded ephemeral footprint that exhausted the on-demand vCPU quota.
const DefaultMaxConcurrent = 10

// DefaultStaleDrainRate is the default cap on stale-image placeholders cycled
// per Reconcile pass (ka-0000000890) when PlaceholderPoolConfig.StaleDrainRate
// is unset.
const DefaultStaleDrainRate = 2

// PlaceholderPoolConfig configures a PlaceholderPool.
type PlaceholderPoolConfig struct {
	// Namespace is the PSA-privileged gVisor namespace (cfg.GVisorNamespace)
	// where runsc agents and probe pods live. Required.
	Namespace string

	// Image is the agent container image used for the sleep placeholder.
	// Required (a placeholder must pull a real image to reach Ready).
	Image string

	// MinSize is the target number of Ready placeholders per gVisor node.
	// Zero disables the pool (no placeholders are created).
	MinSize int

	// NodeFloor is the number of cheap legacy (blind bare-/work) placeholders to
	// keep on EVERY gVisor node, in ADDITION to the per-(node,topology) snapshot
	// buckets (ka-0000000239 / WS1). It keeps a warm gVisor node always present so
	// Karpenter's WhenEmpty consolidation never scales the warm substrate to zero,
	// which would force cold node provisioning on the next thaw. Zero (default)
	// keeps the legacy bucket as a fallback only — today's behavior.
	NodeFloor int

	// StaleDrainRate caps how many stale-image placeholders are cycled per
	// Reconcile pass on a controller upgrade (ka-0000000890). Zero or negative
	// uses DefaultStaleDrainRate.
	StaleDrainRate int

	// MaxConcurrent caps the TOTAL number of live warm placeholder pods the pool
	// may hold across ALL (node,topology) buckets at once (ka-0000004377). Each
	// placeholder is an ephemeral runsc pod holding real node capacity, so the
	// deficit-fill — which refills every bucket toward MinSize after a thaw
	// consumes a member — must respect a global ceiling or an N-node × M-topology
	// fleet can mint far more ephemeral pods than the cluster's ephemeral budget
	// (the 223-leak footprint, ka-0000004129). Zero or negative uses
	// DefaultMaxConcurrent.
	MaxConcurrent int

	// CycleRecorder optionally observes the cycled-stale-placeholder count for
	// the upgrade-churn metric (ka-0000002975). Nil disables the metric.
	CycleRecorder CycleRecorder

	// ClaimRecorder optionally observes thaw-path claim outcomes for the
	// warm_placeholder_claims_total hit/miss/error metric (ka-0000000550).
	// Nil disables the metric.
	ClaimRecorder ClaimRecorder

	// ProbeSelector optionally overrides the gVisor probe-pod selector used for
	// node discovery. Nil uses {app: gvisor-probe}.
	ProbeSelector map[string]string

	// PriorityClassName, when non-empty, is stamped on every warm placeholder so
	// a cold real agent can preempt an idle placeholder when nodes are tight — a
	// placeholder holds node capacity while idle, same as a parked prewarmed pod
	// (ka-0000000722). Wired from cfg.PrewarmedPriorityClass; the chart sets it
	// only when it renders the negative-priority `gardenio-prewarmed` class, so an
	// empty value (legacy) leaves placeholders at default priority.
	PriorityClassName string

	// ImagePullSecrets, when non-empty, is stamped on every warm placeholder so
	// the runsc image pull authenticates to the private registry, exactly like a
	// real agent pod (ka-0000000402). Wired from cfg.ImagePullSecrets (the same
	// registry-pull-secret list applyCommonConfig puts on agent pods). Empty
	// (legacy) leaves placeholders pulling anonymously.
	ImagePullSecrets []string
}

// NewPlaceholderPool constructs a warm-B placeholder pool. Returns nil when the
// config disables the pool (empty namespace/image or MinSize <= 0), so callers
// can unconditionally construct and nil-guard at the run loop — keeping the
// default-off path a true no-op.
func NewPlaceholderPool(client kubernetes.Interface, cfg PlaceholderPoolConfig, logger *slog.Logger) *PlaceholderPool {
	// Active when either the per-topology snapshot fill (MinSize) or the per-node
	// warm floor (NodeFloor) is positive. Both zero keeps the default-off no-op.
	if client == nil || cfg.Namespace == "" || cfg.Image == "" || (cfg.MinSize <= 0 && cfg.NodeFloor <= 0) {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	sel := cfg.ProbeSelector
	if len(sel) == 0 {
		sel = map[string]string{DefaultProbeSelectorKey: DefaultProbeSelectorValue}
	}
	drainRate := cfg.StaleDrainRate
	if drainRate <= 0 {
		drainRate = DefaultStaleDrainRate
	}
	maxConcurrent := cfg.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = DefaultMaxConcurrent
	}
	return &PlaceholderPool{
		client:           client,
		namespace:        cfg.Namespace,
		image:            cfg.Image,
		minSize:          cfg.MinSize,
		nodeFloor:        cfg.NodeFloor,
		drainRate:        drainRate,
		maxConcurrent:    maxConcurrent,
		cycleRecorder:    cfg.CycleRecorder,
		claimRecorder:    cfg.ClaimRecorder,
		logger:           logger,
		probeSelector:    sel,
		priorityClass:    cfg.PriorityClassName,
		imagePullSecrets: cfg.ImagePullSecrets,
		nowFunc:          time.Now,
		mintBackoff:      make(map[string]*backoffState),
		recentlyMinted:   make(map[string]string),
	}
}

// SetSnapshotProvider wires the snapshot provider that drives topology-aware
// fills (ka-0000002219). Called after the FreezeThawManager is constructed. A
// nil pool or nil provider is a no-op — the pool stays on the legacy blind
// bare-/work placeholder path.
func (p *PlaceholderPool) SetSnapshotProvider(snaps SnapshotProvider) {
	if p == nil {
		return
	}
	p.snaps = snaps
}

func (p *PlaceholderPool) now() time.Time {
	if p.nowFunc != nil {
		return p.nowFunc()
	}
	return time.Now()
}

// gVisorNodes returns the set of node names that have a Ready gVisor probe pod —
// i.e. the nodes that can host runsc placeholders. A node without a Ready probe
// cannot run freeze/thaw node-exec anyway, so it is not a valid placeholder
// target.
func (p *PlaceholderPool) gVisorNodes(ctx context.Context) (map[string]bool, error) {
	probes, err := p.client.CoreV1().Pods(p.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelsSelector(p.probeSelector),
	})
	if err != nil {
		return nil, fmt.Errorf("listing gvisor probe pods: %w", err)
	}
	nodes := make(map[string]bool)
	for i := range probes.Items {
		pod := &probes.Items[i]
		if pod.Spec.NodeName != "" && podIsReady(pod) {
			nodes[pod.Spec.NodeName] = true
		}
	}
	return nodes, nil
}

// bucketPlaceholders partitions one (node, topology) bucket's warm placeholders
// by pool state. A bucket is keyed by node AND the LabelWarmTopology value
// (empty string = legacy blind bucket), so a topology-aware fill and the
// topology-matched claim only ever count/hand out graft-compatible pods
// (ka-0000002219).
//
// Ready and pending are further split by image freshness (ka-0000000890): a
// placeholder whose stamped gardenio.io/warm-image matches the pool's currently
// configured image is "fresh"; one that differs (or is unstamped, i.e. minted
// before the stamp existed) is "stale" and must be cycled on upgrade. Only fresh
// Ready+pending count toward the MinSize fill target, so the deficit-fill mints
// new-image placeholders; stale Ready pods are drained rate-limited only once a
// full MinSize of fresh Ready exists, so a rolling image swap never drops the
// pool below MinSize Ready (no cold-start gap).
type bucketPlaceholders struct {
	node         string
	topology     string
	readyFresh   []string // labeled ready, k8s-Ready, image == pool image
	readyStale   []string // labeled ready, k8s-Ready, image != pool image (cycle)
	pendingFresh []string // labeled ready, not yet k8s-Ready, image == pool image
	pendingStale []string // labeled ready, not yet k8s-Ready, image != pool image
	claimed      []string // labeled warm-placeholder=claimed (consumed; not counted)
	terminal     []string // Failed/Succeeded (e.g. OutOfcpu eviction) — leaked, reaped
}

// bucketKey is the composite (node, topology) map key.
func bucketKey(node, topology string) string { return node + "|" + topology }

// isFreshImage reports whether a placeholder was minted from the pool's
// currently configured agent image (ka-0000000890). The image is stamped on the
// pod at create time as gardenio.io/warm-image (BuildWarmPlaceholderPod /
// BuildWarmPlaceholderFromSnapshot). An unstamped pod (minted before the stamp
// existed) reads as "" and is treated as stale, so it is cycled exactly once on
// the first reconcile after the controller carrying this code is rolled.
func (p *PlaceholderPool) isFreshImage(pod *corev1.Pod) bool {
	return pod.Annotations[podmanager.AnnotationWarmImage] == p.image
}

func (p *PlaceholderPool) listPlaceholders(ctx context.Context) (map[string]*bucketPlaceholders, error) {
	pods, err := p.client.CoreV1().Pods(p.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: podmanager.LabelWarmPlaceholder,
	})
	if err != nil {
		return nil, fmt.Errorf("listing warm placeholders: %w", err)
	}
	byBucket := make(map[string]*bucketPlaceholders)
	for i := range pods.Items {
		pod := &pods.Items[i]
		node := pod.Spec.NodeName
		if node == "" {
			continue
		}
		topo := pod.Labels[podmanager.LabelWarmTopology]
		k := bucketKey(node, topo)
		bp := byBucket[k]
		if bp == nil {
			bp = &bucketPlaceholders{node: node, topology: topo}
			byBucket[k] = bp
		}
		switch pod.Labels[podmanager.LabelWarmPlaceholder] {
		case podmanager.WarmPlaceholderClaimed:
			bp.claimed = append(bp.claimed, pod.Name)
		case podmanager.WarmPlaceholderReady:
			// A terminal (Failed/Succeeded) placeholder is dead weight: it counts
			// neither ready nor pending so the reconciler replaces it, AND it must
			// be REAPED — otherwise it lingers on the API server forever. The
			// kubelet OutOfcpu rejection (a node-pinned bucket whose snapshot
			// envelope cannot fit alongside the live agent on its node) is a Failed
			// phase, so without reaping the bucket shows a permanent deficit and
			// re-mints a fresh batch every pass — a storm of hundreds of dead pods
			// (ka-0000000239).
			if podIsTerminal(pod) {
				bp.terminal = append(bp.terminal, pod.Name)
				continue
			}
			fresh := p.isFreshImage(pod)
			switch {
			case podIsReady(pod) && fresh:
				bp.readyFresh = append(bp.readyFresh, pod.Name)
			case podIsReady(pod):
				bp.readyStale = append(bp.readyStale, pod.Name)
			case fresh:
				bp.pendingFresh = append(bp.pendingFresh, pod.Name)
			default:
				bp.pendingStale = append(bp.pendingStale, pod.Name)
			}
		}
	}
	return byBucket, nil
}

// Reconcile performs a single pass: fill each warm bucket up to MinSize and reap
// terminal/claimed leftovers.
//
// With a SnapshotProvider wired (ka-0000002219) the buckets are the distinct
// (node, topology) shapes across all snapshots — each filled with placeholders
// built from a representative snapshot template, so a warm graft passes runsc
// mount validation and wakes into a populated repo. Without a provider (or while
// no snapshot exists yet) it falls back to the legacy per-node blind bare-/work
// placeholder.
func (p *PlaceholderPool) Reconcile(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	nodes, err := p.gVisorNodes(ctx)
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		return nil
	}

	byBucket, err := p.listPlaceholders(ctx)
	if err != nil {
		return err
	}

	// Reap claimed placeholders in every bucket: once a thaw has claimed a
	// warm-B pod, the FreezeThawManager (C1/C2) renames/relabels it into the live
	// agent pod — but if a claim was never followed by a thaw (miss, crash), the
	// claimed pod is a leftover the pool must clean up so it does not leak.
	// Claimed pods that have since become real agent pods carry the agent label
	// and are filtered out at list time (only LabelWarmPlaceholder pods are
	// listed), so anything still labeled "claimed" here is a stale leftover.
	//
	// Delayed-terminal reclassification (ka-0000000088): a terminal pod seen here
	// that this pool minted on a PRIOR reconcile pass — and whose immediate
	// post-Create podRejected() Get returned "not-yet-terminal" (the OutOfmemory
	// class where Failed lands a moment after Create) — is retroactively
	// classified as a rejection so the backoff arms. Tracked via
	// recentlyMinted (persisted across passes). Terminals we did NOT recently
	// mint (foreign pods, or already-armed cohort we've already counted) are
	// simply reaped without arming, avoiding the double-count that would
	// escalate the backoff during a pure-reap pass.
	//
	// Snapshot the prior-pass mints BEFORE the reap loop touches the map so we
	// can prune the survivors afterward (ka-0000005097). A name in
	// recentlyMinted at pass start was recorded when its mint-time podRejected()
	// Get came up not-yet-terminal on the PRIOR pass, so THIS pass is its single
	// delayed-terminal window (the OutOfmemory-lands-Failed-a-moment-after-Create
	// race). A name that does NOT land terminal this pass has survived — reached
	// Ready, been claimed, or stayed healthily pending — so it is past the
	// mint-time race: a LATER failure is a genuine late death (eviction, runtime
	// crash), NOT a mint-time admission reject, and must not arm the OOM-race
	// backoff. Pruning the survivors each pass both bounds the map (the leak the
	// :117 docstring promised but never implemented — the only deleter was the
	// reclassification below) and prevents the false backoff-arm on delayed
	// failures.
	priorMinted := make([]string, 0, len(p.recentlyMinted))
	for name := range p.recentlyMinted {
		priorMinted = append(priorMinted, name)
	}
	delayedRejections := make(map[string]int) // bucketKey → count
	for _, bp := range byBucket {
		for _, name := range bp.claimed {
			p.logger.Info("reaping stale claimed warm placeholder",
				"pod", name, "node", bp.node, "topology", bp.topology)
			p.deletePod(ctx, name)
		}
		// Reap terminal (Failed/Succeeded) placeholders. The dominant sources are:
		//   - kubelet OutOfcpu rejection (a node-pinned snapshot-topology bucket
		//     whose placeholder requests the snapshot's full agent envelope cannot
		//     be admitted on the one node holding that snapshot once the live agent
		//     is running there — ka-0000000239);
		//   - kubelet OutOfmemory rejection (ka-0000000088 / canary #12: a
		//     placeholder minted with a large memory-fence request lands on a node
		//     that has since been backfilled beyond MemAvailable).
		// Such a pod sticks in Failed forever; without this reap it both leaks on
		// the API server AND leaves the bucket permanently below target so the fill
		// below re-mints a doomed batch every pass.
		bk := bucketKey(bp.node, bp.topology)
		for _, name := range bp.terminal {
			if _, ok := p.recentlyMinted[name]; ok {
				// This pod is the pool's OWN mint from a prior pass that
				// podRejected() missed at Create time. Count it as a delayed
				// rejection so the deficit-fill's noteMintOutcome below arms
				// the backoff on the (0, 0) → treat-as-all-rejected path.
				delayedRejections[bk]++
				delete(p.recentlyMinted, name)
			}
			p.logger.Info("reaping terminal warm placeholder",
				"pod", name, "node", bp.node, "topology", bp.topology)
			p.deletePod(ctx, name)
		}
	}

	// Vanished-mint reclassification (ka-0000005929): the delayed-terminal path
	// above only fires while a rejected placeholder's Failed pod still exists on
	// the API server for this pass's listPlaceholders to observe. On the single
	// full fat gVisor node, kubelet rejects an over-fenced warm-B placeholder
	// OutOfmemory and the terminal pod is garbage-collected within seconds —
	// faster than the 30s reconcile — so the rejection is INVISIBLE to both the
	// synchronous podRejected() Get (the reject lands async, after Create) AND the
	// next pass's terminal list (the pod is already gone). The bucket then reads a
	// permanent deficit with zero observed rejections, so noteMintOutcome clears
	// the backoff and re-mints a fresh doomed batch every pass — the ~225-events/
	// window churn this bug tracks.
	//
	// Close the gap by treating a prior-pass mint that has VANISHED — no longer a
	// tracked warm placeholder AND confirmed gone (Get → NotFound) — as a delayed
	// rejection too. The confirming Get is what keeps this sound: a mint that
	// reached Ready and was claimed+promoted also drops out of the warm-placeholder
	// listing, but its pod still EXISTS under the same name as a live agent (Get →
	// found), so it is NOT counted — only a truly GC'd (rejected) pod arms the
	// backoff. Runs after the reap loop (so terminals already counted are absent
	// from recentlyMinted) and before the prune below (which drops the survivors).
	present := make(map[string]bool)
	for _, bp := range byBucket {
		for _, groups := range [][]string{
			bp.readyFresh, bp.readyStale, bp.pendingFresh, bp.pendingStale, bp.claimed,
		} {
			for _, name := range groups {
				present[name] = true
			}
		}
	}
	for _, name := range priorMinted {
		bk, stillTracked := p.recentlyMinted[name]
		if !stillTracked {
			continue // already reclassified as a landed terminal above
		}
		if present[name] {
			continue // survived as a live/pending/claimed warm placeholder
		}
		// Not a tracked warm placeholder this pass: either GC'd after an
		// OutOfmemory reject, or claimed+promoted to a live agent pod. Confirm
		// with a direct Get — only a NotFound (truly gone) counts as a rejection.
		if p.podVanished(ctx, name) {
			delayedRejections[bk]++
		}
	}

	// Per-pass prune (ka-0000005097): the prior-pass mint cohort had its single
	// delayed-terminal window during the reap loop above — any that landed Failed
	// this pass were reclassified (counted + deleted); the rest survived past the
	// mint-time OutOfmemory race. Drop the whole prior cohort now so (a) the map
	// stays bounded (the leak the :117 docstring promised to prune but never did)
	// and (b) a survivor that fails LATER — a genuine late eviction/runtime death,
	// not a mint-time admission reject — is no longer in recentlyMinted and so
	// cannot be misclassified as a delayed rejection that falsely arms the
	// OOM-race backoff. Runs BEFORE the deficit-fill records THIS pass's fresh
	// mints (below), so only the prior cohort is pruned; this pass's mints carry
	// forward to be evaluated on the next pass.
	for _, name := range priorMinted {
		delete(p.recentlyMinted, name)
	}

	buckets := p.targetBuckets(ctx, nodes)

	// Global concurrent-ephemeral cap (ka-0000004377, ka-0000004129): the warm
	// pool mints ephemeral runsc pods, and the per-bucket deficit-fill below has
	// no cross-bucket ceiling — an N-node × M-topology fleet refilling every
	// bucket toward MinSize can mint far more ephemeral pods than the cluster's
	// ephemeral budget, the exact unbounded footprint that leaked 223 stacks and
	// exhausted the on-demand vCPU quota. Bound the pool's TOTAL live footprint:
	// count every placeholder that already exists (Ready + pending, fresh + stale
	// — all hold node capacity) and derive the remaining global budget, then clamp
	// each bucket's fill to it below. Claimed leftovers are excluded (reaped above)
	// and promoted placeholders have become agent pods (no longer warm-labeled), so
	// neither double-counts against the cap.
	liveTotal := 0
	for _, bp := range byBucket {
		liveTotal += len(bp.readyFresh) + len(bp.readyStale) + len(bp.pendingFresh) + len(bp.pendingStale)
	}
	budget := p.maxConcurrent - liveTotal
	if budget < 0 {
		budget = 0
	}

	liveBuckets := make(map[string]bool, len(buckets))
	for _, b := range buckets {
		k := bucketKey(b.node, b.topology)
		liveBuckets[k] = true
		bp := byBucket[k]
		// Only FRESH (current-image) Ready+pending count toward the MinSize
		// target (ka-0000000890). A stale-image placeholder is serviceable today
		// but runs the OLD agent binary, so it must not satisfy the fill — the
		// deficit-fill mints fresh-image replacements while the stale ones keep
		// the pool warm until the drain pass below cycles them out.
		have := 0
		minted, rejected := 0, 0
		if bp != nil {
			have = len(bp.readyFresh) + len(bp.pendingFresh)
		}
		deficit := b.target - have
		if deficit <= 0 {
			// Bucket is at target: clear any prior admission-failure backoff so a
			// transient capacity crunch does not leave a healthy bucket throttled.
			delete(p.mintBackoff, k)
			continue
		}
		// Admission-failure backoff (ka-0000000239): if this bucket is in a mint
		// cooldown — because earlier passes only ever produced rejected pods — skip
		// minting until the cooldown elapses. This is what stops the OutOfcpu storm
		// at the source: a structurally-unfittable bucket is retried on a bounded
		// exponential schedule, not re-stormed every 30s reconcile.
		if bs := p.mintBackoff[k]; bs != nil && p.now().Before(bs.until) {
			p.logger.Info("warm placeholder bucket in admission-failure backoff, skipping mint",
				"node", b.node, "topology", b.topology, "have", have, "target", b.target,
				"failures", bs.failures, "retry_after", bs.until.Sub(p.now()).Round(time.Second))
			continue
		}
		// Clamp this bucket's fill to the remaining global concurrent budget
		// (ka-0000004377). When the cap is already met, hold depth here — a later
		// thaw claim or reap frees budget and a subsequent pass resumes the refill
		// — without arming the admission backoff (the bucket is cap-throttled, not
		// structurally unfittable).
		if deficit > budget {
			p.logger.Info("warm placeholder fill clamped by global concurrent cap",
				"node", b.node, "topology", b.topology, "want", deficit, "allowed", budget,
				"live", liveTotal, "max_concurrent", p.maxConcurrent)
			deficit = budget
		}
		if deficit <= 0 {
			// Global cap exhausted for this pass — mint nothing here. Still fold in
			// any delayed rejections so the async-OOM backoff evidence is not lost.
			if delayed := delayedRejections[k]; delayed > 0 {
				p.noteMintOutcome(k, delayed, delayed)
			}
			continue
		}
		p.logger.Info("warm placeholder bucket below target, creating",
			"node", b.node, "topology", b.topology, "have", have, "target", b.target, "creating", deficit)
		for range deficit {
			name, err := p.createPlaceholder(ctx, b)
			if err != nil {
				p.logger.Warn("failed to create warm placeholder",
					"node", b.node, "topology", b.topology, "error", err)
				break
			}
			minted++
			// A synchronous kubelet rejection (the common OutOfcpu case for a
			// node-pinned bucket) lands the pod straight in Failed. Count it so the
			// backoff below can tell "minted but doomed" from "minted and pending".
			if name != "" && p.podRejected(ctx, name) {
				rejected++
				continue
			}
			// Not-yet-terminal at Create time — record the name (keyed to this
			// bucket) so a NEXT-pass terminal (the OutOfmemory-lands-Failed-a-
			// moment-later race the synchronous podRejected() Get misses,
			// ka-0000000088) OR a NEXT-pass vanish (the reject GC'd before it could
			// be observed, ka-0000005929) can be retroactively counted as a
			// rejection against this bucket on the following reconcile. Storing the
			// bucketKey (not a bare bool) is what lets the vanished-mint path arm
			// the right bucket after the pod — and its labels — are gone.
			if name != "" {
				p.recentlyMinted[name] = k
			}
		}
		// Charge this pass's mints against the global budget so later buckets in
		// the same pass see the reduced headroom (ka-0000004377). deficit was
		// clamped to budget above, so budget stays >= 0.
		budget -= minted
		// Fold in delayed rejections from prior-pass mints that just landed
		// terminal (ka-0000000088). The this-pass mint is on the SAME node
		// under the SAME conditions that just OOM-killed the prior-pass mint,
		// so it is not credibly "healthy" until it has survived a full pass
		// without landing terminal. Rule:
		//   * ANY delayed rejection in this pass ⇒ ARM the backoff. This is
		//     the exact pre-fix-invisible OutOfmemory-lands-async signature
		//     (canary #12: 29 async terminals over 20 min, one per pass, with
		//     the pool re-minting every 30s because the async rejection slipped
		//     past the synchronous podRejected() Get). One delayed terminal is
		//     enough evidence to slow the next pass.
		//   * NO delayed rejections ⇒ evaluate this-pass mint on its own. The
		//     unchanged noteMintOutcome path: healthy → clear, all-rejected →
		//     escalate, zero-mint → no-op.
		delayed := delayedRejections[k]
		if delayed > 0 {
			// Attribute the delayed cohort AS IF it had been minted+rejected
			// this pass; this-pass mint (even if healthy-so-far) is on
			// probation, so its "success" cannot outweigh the async evidence
			// against the bucket. Total minted = this-pass minted + delayed;
			// total rejected = this-pass rejected + delayed. When mintedThisPass
			// is 0 (backoff-skip path exits above so we do not reach here) the
			// arithmetic collapses to (delayed, delayed) — all-rejected → arm.
			// When mintedThisPass > 0 the arithmetic (m+d, r+d) still triggers
			// all-rejected iff mintedThisPass <= rejected (i.e. this pass also
			// had a synchronous rejection) OR mintedThisPass == 0. To preserve
			// backoff-arming for the persistent-race case where every this-pass
			// mint LOOKS healthy at Create time but is doomed to land Failed
			// async, we call noteMintOutcome with (delayed, delayed) ALONE
			// (ignoring this-pass mint for the arithmetic) — this is the
			// evidence we have; this-pass mint hasn't proven itself yet.
			p.noteMintOutcome(k, delayed, delayed)
		} else {
			p.noteMintOutcome(k, minted, rejected)
		}
	}

	// Drop backoff state for buckets that no longer exist (snapshot deleted, node
	// gone) so the map cannot grow unbounded.
	for k := range p.mintBackoff {
		if !liveBuckets[k] {
			delete(p.mintBackoff, k)
		}
	}

	p.drainStale(ctx, byBucket)
	return nil
}

// noteMintOutcome updates a bucket's admission-failure backoff after a mint pass
// (ka-0000000239). A pass that minted at least one placeholder which was NOT
// immediately rejected is healthy: clear the backoff. A pass where every minted
// placeholder was rejected (the OutOfcpu storm signature) grows an exponential
// cooldown so the next pass skips minting until the bucket has had a chance to
// become fittable again. A pass that minted nothing (deficit met by pending) is
// neutral and leaves any existing backoff untouched.
func (p *PlaceholderPool) noteMintOutcome(key string, minted, rejected int) {
	if minted == 0 {
		return
	}
	if rejected < minted {
		delete(p.mintBackoff, key)
		return
	}
	bs := p.mintBackoff[key]
	if bs == nil {
		bs = &backoffState{}
		p.mintBackoff[key] = bs
	}
	bs.failures++
	// Exponential: 1m, 2m, 4m, … capped at maxMintBackoff.
	cooldown := time.Minute << (bs.failures - 1)
	if cooldown <= 0 || cooldown > maxMintBackoff {
		cooldown = maxMintBackoff
	}
	bs.until = p.now().Add(cooldown)
}

// podRejected reports whether a just-created placeholder has already landed in a
// terminal Failed phase — the kubelet's synchronous admission rejection (e.g.
// OutOfcpu) for a pod with a fixed NodeName that does not fit. A transient Get
// error or a still-pending pod reads as not-yet-rejected (false): the backoff is
// driven only by confirmed rejections, so a missed read at worst delays the
// cooldown by one pass.
func (p *PlaceholderPool) podRejected(ctx context.Context, name string) bool {
	pod, err := p.client.CoreV1().Pods(p.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false
	}
	return podIsTerminal(pod)
}

// podVanished reports whether a placeholder the pool minted on a prior pass no
// longer exists at all (Get → NotFound) — the OutOfmemory-rejected-then-GC'd
// signature the vanished-mint reclassification acts on (ka-0000005929). Only a
// definite NotFound counts: a pod that still exists (e.g. one that reached Ready
// and was claimed+promoted to a live agent under the same name, so it dropped
// the warm-placeholder label but is very much alive) is NOT vanished, and a
// transient Get error is treated as "still there" (false) so a flaky apiserver
// read never falsely arms the backoff — at worst the confirmation slips a pass.
func (p *PlaceholderPool) podVanished(ctx context.Context, name string) bool {
	_, err := p.client.CoreV1().Pods(p.namespace).Get(ctx, name, metav1.GetOptions{})
	return apierrors.IsNotFound(err)
}

// drainStale cycles not-in-use warm placeholders whose stamped image differs
// from the pool's currently configured agent image (ka-0000000890) — the
// deployment-side counterpart to the squawk-from-prewarmed bug (ka-0000006843):
// idle warm pods minted before a controller upgrade keep running the OLD binary
// for life unless the pool proactively recycles them.
//
// Only NOT-in-use placeholders are ever touched here: this operates solely on
// pods still labeled warm-placeholder=ready (the bucket's readyStale/pendingStale
// slices). A pod claimed/promoted into a live agent has dropped the
// warm-placeholder label and is filtered out at list time, so a thread serving a
// real agent is never force-cycled.
//
// Two safety invariants keep an upgrade from regressing cold-start latency:
//
//   - NO-COLD-START-GAP: a stale Ready pod is still serviceable, so it is drained
//     only while total Ready (fresh + stale) in its bucket stays >= MinSize after
//     the delete. The deficit-fill above mints fresh placeholders first; as those
//     reach Ready the surplus grows and the stale ones drain out — the pool never
//     dips below MinSize Ready during the swap.
//   - RATE-LIMIT: at most drainRate pods are cycled across all buckets per pass,
//     so a fleet-wide image roll does not nuke the whole pool (or storm the API)
//     in a single reconcile. Convergence takes a bounded number of passes.
//
// Pending (not-yet-Ready) stale pods are not serving and do not count toward
// Ready, so they are deleted without the surplus guard (still rate-limited): a
// stale-image pod that is merely starting up would only reach Ready running the
// old binary, so there is no value in waiting for it.
func (p *PlaceholderPool) drainStale(ctx context.Context, byBucket map[string]*bucketPlaceholders) {
	remaining := p.drainRate
	for _, bp := range byBucket {
		if remaining <= 0 {
			break
		}
		cycled := 0

		// Pending stale: not Ready, safe to delete freely (rate-limited).
		for _, name := range bp.pendingStale {
			if remaining <= 0 {
				break
			}
			p.logger.Info("cycling stale-image warm placeholder (pending)",
				"pod", name, "node", bp.node, "topology", bp.topology, "want_image", p.image)
			p.deletePod(ctx, name)
			remaining--
			cycled++
		}

		// Ready stale: serviceable, so keep MinSize Ready total in the bucket.
		// surplus is how many Ready pods we can delete and still hold MinSize.
		surplus := len(bp.readyFresh) + len(bp.readyStale) - p.minSize
		for _, name := range bp.readyStale {
			if remaining <= 0 || surplus <= 0 {
				break
			}
			p.logger.Info("cycling stale-image warm placeholder (ready)",
				"pod", name, "node", bp.node, "topology", bp.topology, "want_image", p.image)
			p.deletePod(ctx, name)
			remaining--
			surplus--
			cycled++
		}

		if cycled > 0 && p.cycleRecorder != nil {
			p.cycleRecorder.RecordWarmPlaceholderCycled(bp.node, bp.topology, cycled)
		}
	}
}

// targetBucket is one (node, topology) bucket the pool should keep warm, plus
// the snapshot template that produces its placeholders. A zero-value Snapshot
// (legacy bucket) signals the blind bare-/work builder.
type targetBucket struct {
	node     string
	topology string
	snap     podmanager.Snapshot
	legacy   bool
	target   int // desired Ready count for this bucket (minSize or nodeFloor)
}

// targetBuckets computes the buckets to fill this pass, each with its desired
// Ready count.
//
// With a SnapshotProvider wired it includes the cross-product-free set of
// (snapshot-node, topology) pairs whose node currently has a Ready gVisor probe
// (a placeholder is useless on a node that cannot run the node-exec thaw), each
// targeting MinSize for concurrent-thaw headroom.
//
// In ADDITION, when NodeFloor > 0 it includes a legacy blind (bare-/work) bucket
// on EVERY gVisor node targeting NodeFloor (ka-0000000239 / WS1) — a cheap warm
// floor that keeps every gVisor node non-empty so Karpenter's WhenEmpty
// consolidation never scales the warm substrate to zero. The floor is unioned
// with the snapshot buckets, not a fallback for them, so per-topology
// concurrent-thaw headroom is preserved.
//
// Without a provider, with no snapshot yet, or with NodeFloor == 0 and no
// snapshot buckets landing on a Ready node, it falls back to one legacy bucket
// per gVisor node targeting max(MinSize, NodeFloor) — today's behavior.
func (p *PlaceholderPool) targetBuckets(ctx context.Context, nodes map[string]bool) []targetBucket {
	var out []targetBucket
	if p.snaps != nil {
		wb, err := p.snaps.WarmBuckets(ctx)
		if err != nil {
			p.logger.Warn("warm placeholder: snapshot bucket enumeration failed; falling back to legacy per-node fill", "error", err)
		} else {
			for _, b := range wb {
				if !nodes[b.Node] {
					continue // node has no Ready gVisor probe; cannot serve a thaw
				}
				out = append(out, targetBucket{node: b.Node, topology: b.Topology, snap: b.Snapshot, target: p.minSize})
			}
		}
	}

	// Per-node warm floor: a cheap legacy bucket on every gVisor node, unioned
	// with the snapshot buckets above, so a node with no snapshot still stays warm.
	if p.nodeFloor > 0 {
		for node := range nodes {
			out = append(out, targetBucket{node: node, legacy: true, target: p.nodeFloor})
		}
		return out
	}

	if len(out) > 0 {
		return out
	}

	// No snapshot buckets and no node floor configured: fall back to one legacy
	// bucket per gVisor node at the snapshot MinSize (today's behavior).
	p.logger.Info("warm placeholder: no snapshot buckets land on a Ready gVisor node; falling back to legacy per-node fill")
	fallback := p.minSize
	for node := range nodes {
		out = append(out, targetBucket{node: node, legacy: true, target: fallback})
	}
	return out
}

// createPlaceholder mints one warm placeholder pod for a target bucket and
// returns its name (so the caller can inspect a synchronous kubelet rejection
// for the admission-failure backoff, ka-0000000239). The name is empty only when
// creation errored.
func (p *PlaceholderPool) createPlaceholder(ctx context.Context, b targetBucket) (string, error) {
	p.seq++
	var pod *corev1.Pod
	if b.legacy {
		name := fmt.Sprintf("warm-b-%s-%d-%d", shortNode(b.node), p.now().Unix(), p.seq)
		pod = podmanager.BuildWarmPlaceholderPod(podmanager.WarmPlaceholderSpec{
			Name:      name,
			Namespace: p.namespace,
			Node:      b.node,
			Image:     p.image,
		})
		return name, p.createPod(ctx, pod, b.node)
	}
	name := fmt.Sprintf("warm-b-%s-%s-%d-%d", shortNode(b.node), shortTopo(b.topology), p.now().Unix(), p.seq)
	pod = podmanager.BuildWarmPlaceholderFromSnapshot(name, b.snap, b.node)
	return name, p.createPod(ctx, pod, b.node)
}

// createPod creates a built placeholder pod, tolerating AlreadyExists. It stamps
// AnnotationWarmImage with the pool's currently configured image so the
// upgrade-staleness reaper (ka-0000000890) can tell a placeholder minted by this
// controller from one minted by an older one. Stamping here (rather than in the
// builders) keeps the signal uniform across the legacy and snapshot build paths:
// the snapshot path's container image comes from the template, but the staleness
// question is "did the CURRENT controller mint this", so the pool's own image is
// the right value regardless of which builder produced the pod.
func (p *PlaceholderPool) createPod(ctx context.Context, pod *corev1.Pod, node string) error {
	name := pod.Name
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[podmanager.AnnotationWarmImage] = p.image
	// Stamp the low (negative) priority here — uniform across the legacy and
	// snapshot builders, same rationale as the warm-image annotation above — so a
	// cold real agent can preempt an idle placeholder under node pressure
	// (ka-0000000722). Empty leaves the pod at default priority.
	if p.priorityClass != "" {
		pod.Spec.PriorityClassName = p.priorityClass
	}
	// Stamp the registry pull secret(s) here so the runsc image pull
	// authenticates to the private registry — the SAME registry-pull-secret real
	// agent pods carry (ka-0000000402). Without this the legacy bare builder's pod
	// carries no imagePullSecret, so the kubelet pulls with an ANONYMOUS token and
	// 403s on any tag not already node-cached — every warm placeholder
	// ImagePullBackOffs the moment a new release tag lands. Only stamp when the
	// pod does not already carry secrets: the snapshot builder preserves Pod A's
	// pull secrets verbatim (shapePlaceholderSpec DeepCopies the live template),
	// so this fills the gap for the legacy path without clobbering the snapshot
	// path — mirroring applyCommonConfig's len(spec.ImagePullSecrets)==0 guard.
	if len(p.imagePullSecrets) > 0 && len(pod.Spec.ImagePullSecrets) == 0 {
		for _, name := range p.imagePullSecrets {
			pod.Spec.ImagePullSecrets = append(pod.Spec.ImagePullSecrets,
				corev1.LocalObjectReference{Name: name})
		}
	}
	_, err := p.client.CoreV1().Pods(p.namespace).Create(ctx, pod, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("creating warm placeholder %s: %w", name, err)
	}
	p.logger.Info("created warm placeholder", "pod", name, "node", node,
		"topology", pod.Labels[podmanager.LabelWarmTopology])
	return nil
}

func (p *PlaceholderPool) deletePod(ctx context.Context, name string) {
	if err := p.client.CoreV1().Pods(p.namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		p.logger.Warn("failed to delete warm placeholder", "pod", name, "error", err)
	}
}

// DeletePromoted tears down a placeholder that ClaimWarmPlaceholderForThaw
// already promoted to a live agent pod, for the C2 wake path to call when the
// subsequent thaw graft fails (ka-0000005266). Once promoted the pod no longer
// carries the warm-placeholder label, so the pool's own Reconcile reaper will
// never clean it up — the caller that promoted it owns teardown. NotFound is
// tolerated (a concurrent reconciler delete already removed it). A nil pool is a
// no-op. The pod lives in the pool's namespace (the same gVisor namespace the
// placeholder was claimed from and the snapshot restores into).
func (p *PlaceholderPool) DeletePromoted(ctx context.Context, name string) error {
	if p == nil || name == "" {
		return nil
	}
	if err := p.client.CoreV1().Pods(p.namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// ErrNoWarmPlaceholder is returned by ClaimWarmPlaceholderForThaw when no Ready warm
// placeholder exists on the requested node. The caller falls back to today's
// create-then-restore thaw path (still correct, just not sub-second).
var ErrNoWarmPlaceholder = fmt.Errorf("no warm placeholder available on node")

// readyCandidates returns the Ready placeholder names that can serve a thaw for
// (node, topology). Topology matching is STRICT (ka-0000002219): a placeholder
// whose LabelWarmTopology differs from the snapshot's key is NOT graft-compatible
// (its mount-destination set / repo would fail runsc RestoreValidateSpec), so it
// is never handed out cross-topology. An empty requested topology (a caller with
// no snapshot topology, or the legacy path) matches only the legacy bucket
// (empty-topology placeholders). When the requested topology has no Ready
// placeholder the result is empty and the caller falls back to create-then-
// restore — always correct.
//
// Both fresh- and stale-image Ready placeholders are graft-compatible serving
// candidates (ka-0000000890): a thaw restores the agent's own checkpoint onto
// the placeholder's kubelet-provisioned sandbox, so the placeholder's own binary
// image is irrelevant to what the woken agent runs. Fresh candidates are tried
// first so the pool preferentially consumes new-image pods and the upgrade drain
// has fewer stale ones left to cycle.
func (p *PlaceholderPool) readyCandidates(byBucket map[string]*bucketPlaceholders, node, topology string) []string {
	bp := byBucket[bucketKey(node, topology)]
	if bp == nil {
		return nil
	}
	if len(bp.readyStale) == 0 {
		return bp.readyFresh
	}
	out := make([]string, 0, len(bp.readyFresh)+len(bp.readyStale))
	out = append(out, bp.readyFresh...)
	out = append(out, bp.readyStale...)
	return out
}

// ClaimWarmPlaceholderForThaw atomically claims a Ready warm placeholder
// co-resident with node AND promotes it to a live thawed agent pod in a SINGLE
// k8s Update, returning its name for the C2 wake path to pass as
// podmanager.ThawRequest.PlaceholderPod (ka-0000005266).
//
// Why a single transition (ready → live-agent) rather than claim-then-promote:
// while a pod carries gardenio.io/warm-placeholder (in EITHER "ready" or
// "claimed" state), the pool reaper deletes "claimed" leftovers on its next
// Reconcile. A thaw graft can take up to the rehydrate deadline (default 60s),
// longer than the reconcile interval (default 30s), so a pod parked in "claimed"
// would be reaped MID-RESTORE — tearing down the kubelet sandbox the restore
// grafts onto (the ka-0000007549 failure class). Dropping the warm-placeholder
// label and adding the agent/thaw-placeholder identity in one atomic Update
// hands lifecycle ownership straight from the pool to the reconciler, which
// treats the pod as the agent's live (drift-exempt) pod and never reaps it.
//
// Failure handling mirrors the create-path Pod B exactly: the pod is the agent's
// committed pod from this point on, so if the subsequent thaw graft fails the
// CALLER must tear it down (it holds the returned name) before falling back to a
// cold start — identical to main.go's partial-success teardown.
//
// Returns ErrNoWarmPlaceholder when the node has no Ready placeholder, so the
// wake path falls back to today's create-then-restore thaw (always correct).
func (p *PlaceholderPool) ClaimWarmPlaceholderForThaw(ctx context.Context, node, topology string, promo podmanager.WarmPlaceholderPromotion) (string, error) {
	if p == nil {
		// Pool disabled — not a miss of a real pool, so no metric.
		return "", ErrNoWarmPlaceholder
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	byBucket, err := p.listPlaceholders(ctx)
	if err != nil {
		p.recordClaim("error", node, topology)
		return "", err
	}
	ready := p.readyCandidates(byBucket, node, topology)
	if len(ready) == 0 {
		p.recordClaim("miss", node, topology)
		return "", ErrNoWarmPlaceholder
	}

	for _, name := range ready {
		ok, err := p.markPromoted(ctx, name, promo)
		if err != nil {
			p.logger.Warn("warm placeholder promote flip failed, trying next",
				"pod", name, "node", node, "error", err)
			continue
		}
		if ok {
			p.logger.Info("claimed+promoted warm placeholder for thaw graft",
				"pod", name, "node", node, "topology", topology, "agent", promo.AgentName, "canonical", promo.CanonicalPod)
			p.recordClaim("hit", node, topology)
			return name, nil
		}
	}
	// Candidates existed but every promote flip was lost or errored — the
	// caller falls back to create-then-restore either way, so count it as a
	// miss (the pool failed to serve the thaw).
	p.recordClaim("miss", node, topology)
	return "", ErrNoWarmPlaceholder
}

// recordClaim emits one claim outcome when a recorder is wired. Outcome
// strings mirror metrics.PoolOutcome* ("hit"/"miss"/"error") as literals so
// this package does not import internal/metrics (the CycleRecorder boundary
// rule).
func (p *PlaceholderPool) recordClaim(outcome, node, topology string) {
	if p.claimRecorder != nil {
		p.claimRecorder.RecordWarmPlaceholderClaim(outcome, node, topology)
	}
}

// markPromoted atomically transitions a Ready placeholder to a live thawed agent
// pod: it re-reads the pod, verifies it is still labeled warm-placeholder=ready,
// applies the agent identity (dropping the warm-placeholder label) via
// podmanager.ApplyWarmPlaceholderPromotion, and writes back. A conflict or a
// no-longer-ready pod returns ok=false so the caller tries another candidate —
// the optimistic resourceVersion guard makes concurrent claims of the same node
// safe without a separate lock.
func (p *PlaceholderPool) markPromoted(ctx context.Context, name string, promo podmanager.WarmPlaceholderPromotion) (bool, error) {
	pod, err := p.client.CoreV1().Pods(p.namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if pod.Labels[podmanager.LabelWarmPlaceholder] != podmanager.WarmPlaceholderReady {
		return false, nil // already claimed by a racing caller
	}
	podmanager.ApplyWarmPlaceholderPromotion(pod, promo)
	_, err = p.client.CoreV1().Pods(p.namespace).Update(ctx, pod, metav1.UpdateOptions{})
	if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
		return false, nil // lost the race; caller tries another
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// RunLoop periodically reconciles the placeholder pool until the context is
// cancelled. interval <= 0 defaults to 30s.
func (p *PlaceholderPool) RunLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	p.logger.Info("warm placeholder pool manager started",
		"namespace", p.namespace, "min_size_per_node", p.minSize,
		"max_concurrent", p.maxConcurrent, "interval", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	if err := p.Reconcile(ctx); err != nil {
		p.logger.Warn("initial warm placeholder reconcile failed", "error", err)
	}
	for {
		select {
		case <-ctx.Done():
			p.logger.Info("warm placeholder pool manager stopped")
			return
		case <-ticker.C:
			if err := p.Reconcile(ctx); err != nil {
				p.logger.Warn("warm placeholder reconcile failed", "error", err)
			}
		}
	}
}

// podIsReady reports whether a pod is Running and has a True Ready condition.
func podIsReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// podIsTerminal reports whether a pod has reached a terminal phase.
func podIsTerminal(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded
}

// labelsSelector renders a map as a comma-joined K8s equality label selector.
func labelsSelector(m map[string]string) string {
	out := ""
	for k, v := range m {
		if out != "" {
			out += ","
		}
		out += k + "=" + v
	}
	return out
}

// shortTopo trims a topology key to a short pod-name-safe fragment. The full key
// is a 16-char hex hash (SnapshotTopologyKey); the leading 8 are plenty to keep
// pod names readable and collision-free in practice (the seq counter guarantees
// uniqueness anyway).
func shortTopo(topo string) string {
	const max = 8
	if len(topo) > max {
		return topo[:max]
	}
	return topo
}

// shortNode trims a node name to a label/name-safe fragment for placeholder pod
// names. EKS node names are long DNS names; keep the leading segment.
func shortNode(node string) string {
	const max = 20
	out := node
	for i := 0; i < len(node); i++ {
		if node[i] == '.' {
			out = node[:i]
			break
		}
	}
	if len(out) > max {
		out = out[:max]
	}
	return out
}
