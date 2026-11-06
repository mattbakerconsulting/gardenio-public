package reconciler

// The named steps of one Reconcile pass, in the order Reconcile runs them.
// Split out of the single 880-line Reconcile (gd-ubMxMUx6zQ); behaviour,
// ordering of side effects, and every log line are unchanged.

import (
	"context"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
	"gardenio/controller/internal/lands/wall"
	"gardenio/controller/internal/podmanager"
)

// reconcilePass is the state one Reconcile pass builds up front and the
// per-bead create loop reads and updates.
type reconcilePass struct {
	desired   map[string]beadsapi.AgentBead
	actualMap map[string]corev1.Pod

	// gvisorNamespace is cfg.GVisorNamespace; "" means no separate budget.
	gvisorNamespace string

	// Active-pod counters for concurrency limiting (see countActivePods).
	activePods           int
	activePodsPerProject map[string]int
	activeGVisorPods     int

	microvmHandled map[string]struct{} // bead IDs the MicroVM pass reconciled
	closedThisPass map[string]struct{} // bead IDs the leaked-bead closer closed

	burstLimit       int
	created          int
	deferred         []desiredEntry                // agents deferred this pass (for age alerting)
	deferredThisPass map[string]beadsapi.AgentBead // bead ID → bead (deferred this pass)

	// Per-pass cache of node-doom lookups (WS1, ka-0000007969). Many agent pods
	// commonly share a node, so this collapses N GetNode calls per node to one
	// per reconcile pass. Nil value = looked up and node is fine (or lookup
	// failed); presence of the key = already looked up.
	nodeDoomed map[string]bool

	// Per-pass cache of spawn wall verdicts by project and role: the wall
	// reads the land and its purse from the daemon, once per project and role
	// per pass (the role matters only for the steward, gd-KOAoUELMWo).
	wallVerdicts map[wallKey]wall.Refusal
}

// newReconcilePass builds the pass state from the desired/actual maps left
// after orphan cleanup and counts the active pods.
func newReconcilePass(cfg *config.Config, desired map[string]beadsapi.AgentBead, actualMap map[string]corev1.Pod) *reconcilePass {
	burstLimit := cfg.CoopBurstLimit
	if burstLimit <= 0 {
		burstLimit = 3 // safety default
	}
	p := &reconcilePass{
		desired:              desired,
		actualMap:            actualMap,
		gvisorNamespace:      cfg.GVisorNamespace,
		activePodsPerProject: make(map[string]int),
		microvmHandled:       make(map[string]struct{}),
		closedThisPass:       make(map[string]struct{}),
		burstLimit:           burstLimit,
		deferredThisPass:     make(map[string]beadsapi.AgentBead),
		nodeDoomed:           make(map[string]bool),
		wallVerdicts:         make(map[wallKey]wall.Refusal),
	}
	p.countActivePods()
	return p
}

// inGVisorNamespace reports whether ns is the separate gVisor namespace.
func (p *reconcilePass) inGVisorNamespace(ns string) bool {
	return p.gvisorNamespace != "" && ns == p.gvisorNamespace
}

// countActivePods counts active (non-terminal, non-orphan) pods for
// concurrency limiting. Only count pods that are in the desired set — orphans
// were just deleted. Exclude both Failed and Succeeded pods — they are
// terminal and will be deleted+recreated below.
//
// gVisor-namespace pods (runsc freeze/thaw agents + the runsc prewarmed
// sub-pool) are counted against a SEPARATE budget (activeGVisorPods /
// CoopMaxPodsGVisor), never the per-project cap. They live in their own
// PSA-privileged namespace; charging them to the spawning project's cap
// (saturated by the thread pool + active agents) meant a runsc sub-pool
// could never schedule even at min_size=1 (ka-0000004152). They still count
// toward the global CoopMaxPods ceiling via activePods.
func (p *reconcilePass) countActivePods() {
	for name, pod := range p.actualMap {
		if bead, inDesired := p.desired[name]; inDesired && !isTerminalPod(pod) {
			p.charge(pod.Namespace, bead.Project)
		}
	}
}

// charge counts one active pod against the global count and against exactly
// one of the gVisor budget or its project's cap, routed by namespace.
func (p *reconcilePass) charge(ns, project string) {
	p.activePods++
	if p.inGVisorNamespace(ns) {
		p.activeGVisorPods++
	} else {
		p.activePodsPerProject[project]++
	}
}

// isTerminalPod reports whether the pod is Failed or Succeeded.
func isTerminalPod(pod corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded
}

// desiredPods builds the desired pod name set from the agent beads.
func desiredPods(beads []beadsapi.AgentBead) map[string]beadsapi.AgentBead {
	desired := make(map[string]beadsapi.AgentBead)
	for _, b := range beads {
		// A local agent runs on its owner's laptop (execution_target=laptop):
		// it never has a pod, so it is not desired state here. Leaving it in
		// would cold-start a pod for it, or close it as a podless leak.
		if podmanager.IsLaptopTarget(b.Metadata["execution_target"]) {
			continue
		}
		// Strip project prefix from agent name to match PodName() in podmanager.
		agentName := strings.TrimPrefix(b.AgentName, b.Project+"-")
		podName := fmt.Sprintf("%s-%s-%s-%s", b.Mode, b.Project, podmanager.SanitizeRole(b.Role), agentName)
		desired[podName] = b
	}
	return desired
}

// listActualPods gets actual state from K8s across the primary namespace and
// any extra namespaces (e.g. the PSA-privileged gVisor namespace where runsc
// freeze/thaw agents run, ka-0000008076), then the namespaces the source names
// this pass (the tenant land namespaces, gd-NDJIOutLsv). Each pod carries its
// own namespace, so create/delete below stay namespace-correct. A land
// namespace not yet made, or just deleted, is not found or not bound to the
// controller; that is the land's lifecycle, so it is skipped.
func (r *Reconciler) listActualPods(ctx context.Context) (map[string]corev1.Pod, error) {
	actualMap := make(map[string]corev1.Pod)
	namespaces := append([]string{r.cfg.Namespace}, r.extraNamespaces...)
	fixed := len(namespaces)
	if r.namespaceSource != nil {
		for _, ns := range r.namespaceSource() {
			if ns != "" && !slices.Contains(namespaces, ns) {
				namespaces = append(namespaces, ns)
			}
		}
	}
	for i, ns := range namespaces {
		actual, err := r.pods.ListAgentPods(ctx, ns, map[string]string{
			podmanager.LabelApp: podmanager.LabelAppValue,
		})
		if i >= fixed && (apierrors.IsNotFound(err) || apierrors.IsForbidden(err)) {
			r.logger.Debug("skipping land namespace not ready for listing", "namespace", ns, "error", err)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("listing agent pods in namespace %s: %w", ns, err)
		}
		for _, p := range actual {
			// Only consider pods with the gardenio.io/agent label — this
			// excludes the controller itself and other infrastructure pods
			// that share the app.kubernetes.io/name=gardenio label.
			if _, ok := p.Labels[podmanager.LabelAgent]; !ok {
				continue
			}
			actualMap[actualPodKey(p)] = p
		}
	}
	return actualMap, nil
}

// actualPodKey is the name a live pod is registered under in the actual map.
//
// Freeze/thaw placeholder Pod B (ka-0000007549 / ka-0000004146) is
// named "<canonical>-thaw-b" — it never matches the desired pod name
// "<canonical>" the bead loop builds. Without remapping, the
// desired-vs-actual sweep below would (a) delete Pod B as an orphan —
// including MID-RESTORE, tearing down the kubelet-owned netns + runsc
// sandbox state the on-node `runsc restore` repoints into, the exact
// thaw rc=128 — and (b) treat the desired "<canonical>" pod as missing
// and cold-start a DUPLICATE racing the thaw. Post-thaw, reportThawed
// keeps Pod B under its -thaw-b name as the permanent live pod, so the
// same mismatch would kill the thawed pod every steady-state pass.
// Register Pod B under its CANONICAL name so it satisfies the agent's
// desired entry: not an orphan, not missing.
func actualPodKey(p corev1.Pod) string {
	if p.Labels[podmanager.LabelThawPlaceholder] != "true" {
		return p.Name
	}
	// A create-path Pod B is "<canonical>-thaw-b" → trim the suffix.
	// A warm-B pod grafted onto by the wake path (C2, ka-0000005266)
	// keeps its pool name "warm-b-<node>-…", which the suffix-trim
	// cannot map back; the wake path stamps AnnotationCanonicalPod at
	// promotion, so prefer it. Without this the promoted warm pod is
	// orphan-reaped and a duplicate cold-starts (ka-0000007549).
	if canonical := p.Annotations[podmanager.AnnotationCanonicalPod]; canonical != "" {
		return canonical
	}
	return strings.TrimSuffix(p.Name, podmanager.ThawPlaceholderSuffix)
}

// orphanPod is a live pod with no desired bead, keyed as in the actual map.
type orphanPod struct {
	key string
	pod corev1.Pod
}

// findOrphans lists the pods that exist in K8s but not in desired.
func findOrphans(desired map[string]beadsapi.AgentBead, actualMap map[string]corev1.Pod) []orphanPod {
	var orphans []orphanPod
	for name, pod := range actualMap {
		if _, ok := desired[name]; !ok {
			orphans = append(orphans, orphanPod{key: name, pod: pod})
		}
	}
	return orphans
}

// deleteOrphanPods deletes orphan pods (exist in K8s but not in desired).
// Guard: if daemon returned zero beads but pods exist, this could be a
// transient daemon issue (restart, query race, etc.). Wait for 3 consecutive
// empty cycles before proceeding — transient failures recover within 1-2.
func (r *Reconciler) deleteOrphanPods(ctx context.Context, desired map[string]beadsapi.AgentBead, actualMap map[string]corev1.Pod) error {
	if len(desired) == 0 && len(actualMap) > 0 {
		r.emptyDesiredCycles++
		if r.emptyDesiredCycles < 3 {
			r.logger.Warn("desired state empty, deferring orphan cleanup",
				"actual_pods", len(actualMap), "empty_cycles", r.emptyDesiredCycles)
			return nil
		}
		r.logger.Info("desired state empty for 3+ cycles, proceeding with orphan cleanup",
			"actual_pods", len(actualMap))
		for name, pod := range actualMap {
			// Delete by pod.Name, not the map key: a thaw placeholder is
			// mapped under its canonical name (…-thaw-b suffix-trim, or the
			// gardenio.io/canonical-pod annotation for a warm-B graft) while its
			// real k8s object keeps its own name. Deleting by the key would
			// NotFound-noop and leak the running pod (ka-0000007549 /
			// ka-0000005266). For every non-placeholder pod pod.Name == key.
			r.logger.Info("deleting orphan pod", "pod", pod.Name, "key", name)
			if err := r.pods.DeleteAgentPod(ctx, pod.Name, pod.Namespace); err != nil {
				return fmt.Errorf("deleting orphan pod %s: %w", pod.Name, err)
			}
		}
		return nil
	}

	r.emptyDesiredCycles = 0
	// Collect orphans first so a MASS orphaning can be recognized before
	// any deletion happens.
	orphans := r.guardMassOrphans(findOrphans(desired, actualMap), len(actualMap))
	for _, o := range orphans {
		// Delete by pod.Name, not the map key — see the canonical-name note
		// in the empty-desired branch above (ka-0000007549 / ka-0000005266).
		r.logger.Info("deleting orphan pod", "pod", o.pod.Name, "key", o.key)
		if err := r.pods.DeleteAgentPod(ctx, o.pod.Name, o.pod.Namespace); err != nil {
			return fmt.Errorf("deleting orphan pod %s: %w", o.pod.Name, err)
		}
	}
	return nil
}

// guardMassOrphans returns the orphans to delete this pass, or nil while a
// mass orphaning is being deferred.
//
// Guard: a NON-empty but PARTIAL daemon response (e.g. a beads replica
// answering mid-rollout before its cache warms) slips past the
// empty-desired guard above and would orphan-delete every pod whose
// bead was dropped from the response. Genuine orphaning is incremental
// — agents finish one at a time — so a single pass wanting to delete
// most of the fleet is overwhelmingly more likely to be a bad read.
// Same shape as the empty-desired guard: require 3 consecutive cycles
// before acting (ka-0000005463 hardening; preventive, not observed).
func (r *Reconciler) guardMassOrphans(orphans []orphanPod, actualPods int) []orphanPod {
	if len(orphans) < 3 || len(orphans)*2 <= actualPods {
		r.massOrphanCycles = 0
		return orphans
	}
	r.massOrphanCycles++
	if r.massOrphanCycles < 3 {
		r.logger.Warn("mass orphan detection, deferring orphan cleanup",
			"orphans", len(orphans), "actual_pods", actualPods,
			"mass_cycles", r.massOrphanCycles)
		return nil
	}
	r.logger.Info("mass orphan state persisted 3+ cycles, proceeding with orphan cleanup",
		"orphans", len(orphans), "actual_pods", actualPods)
	return orphans
}

// reconcileMicrovmAgents is the lambda_microvm lifecycle reconcile (C5,
// ka-0000001837). A MicroVM agent runs as a MicroVM, not a K8s pod, so it
// legitimately has NO pod — the pod-presence rules below (podless-leak close,
// create-missing cold start) would both misfire on it. Reconcile every MicroVM
// agent against its runtime status FIRST (independent of the beadCloser
// wiring): the coordinator closes a bead whose runtime is gone/terminal and
// leaves a live one open. Beads handled here are recorded so the pod-based
// passes below skip them. No-op unless the MicroVM backend is wired.
func (r *Reconciler) reconcileMicrovmAgents(ctx context.Context, desired map[string]beadsapi.AgentBead) map[string]struct{} {
	microvmHandled := make(map[string]struct{})
	if r.microvm != nil {
		for _, bead := range desired {
			if r.reconcileMicrovmAgent(ctx, bead) {
				microvmHandled[bead.ID] = struct{}{}
			}
		}
	}
	return microvmHandled
}

// isFreezeRestingState reports whether agent_state is one a podless bead can
// hold while a freeze/thaw snapshot awaits thaw: the terminal states, plus
// "stopping", the frozen-awaiting-thaw resting state.
func isFreezeRestingState(state string) bool {
	switch state {
	case "done", "failed", "rate_limited", "stopping":
		return true
	}
	return false
}

// closeLeakedBeads closes leaked beads: agent beads with terminal agent_state
// ("done", "failed", "rate_limited") that have no corresponding pod. These are
// agents whose pods terminated without properly closing their beads.
// Closing them here prevents them from consuming pod slots in the
// concurrency count and waiting 2+ hours for the stale reaper.
//
// We only close terminal-state beads (not "working") because "working"
// beads without pods may be in a brief race: the pod was just deleted
// for restart and will be recreated below in the same pass.
// Terminal states are definitive — the agent finished but the bead
// was never closed.
//
// It returns the beads it closed this pass. The create-missing loop below
// iterates the SAME desired map, so without this a just-closed podless
// terminal bead would immediately be cold-started again — spawning a live
// pod behind a now-closed bead, which the next pass then orphan-reaps:
// pointless create→delete churn every cycle (and, for rate_limited, a spawn
// onto a known-exhausted account). The pre-existing failed-state skip
// (below) only covered "failed"; extend the guard to every state the closer
// acts on by tracking exactly which beads it closed.
func (r *Reconciler) closeLeakedBeads(ctx context.Context, desired map[string]beadsapi.AgentBead, actualMap map[string]corev1.Pod, microvmHandled map[string]struct{}) map[string]struct{} {
	closedThisPass := make(map[string]struct{})
	if r.beadCloser == nil {
		return closedThisPass
	}
	for name, bead := range desired {
		if _, hasPod := actualMap[name]; hasPod {
			continue // pod exists, not a leak
		}
		// lambda_microvm agents were already reconciled by runtime status
		// above (C5); never subject them to the pod-presence leak rule.
		if _, ok := microvmHandled[bead.ID]; ok {
			continue
		}
		if isFreezeRestingState(bead.AgentState) && r.closeLeakedBead(ctx, bead) {
			closedThisPass[bead.ID] = struct{}{}
		}
	}
	return closedThisPass
}

// closeLeakedBead closes one podless terminal bead and reports whether it did.
func (r *Reconciler) closeLeakedBead(ctx context.Context, bead beadsapi.AgentBead) bool {
	// Exempt frozen-awaiting-thaw agents: a freeze checkpoints and
	// tears down Pod A, leaving the bead podless but with a persisted
	// snapshot. A SUCCESSFUL freeze leaves the bead OPEN at
	// agent_state=stopping (suspendAgent's frozen-awaiting-thaw signal,
	// which the controller no longer overwrites to "done" — bug
	// ka-0000007439); a freeze FAILURE reports "done" as before. Either
	// way, closing a bead that still has a snapshot would race the
	// in-flight thaw and cold-start over it (bug ka-0000000114), so the
	// snapshot exemption covers both. Only once the snapshot is gone
	// (thaw consumed it, or it was lost) is the podless bead a genuine
	// leak to close. The checker is only wired when cfg.FreezeThawEnabled
	// is on, so this is a no-op under the default cold-start lifecycle.
	if r.hasSnapshot != nil && r.hasSnapshot(ctx, bead.AgentName) {
		r.logger.Info("skipping leaked-bead close: agent has freeze/thaw snapshot (awaiting thaw)",
			"bead", bead.ID, "agent", bead.AgentName,
			"agent_state", bead.AgentState, "project", bead.Project)
		return false
	}
	r.logger.Warn("closing leaked agent bead: terminal state but no pod",
		"bead", bead.ID, "agent", bead.AgentName,
		"agent_state", bead.AgentState, "project", bead.Project)
	if err := r.beadCloser.CloseBead(ctx, bead.ID, map[string]string{
		"agent_state":  bead.AgentState,
		"close_reason": fmt.Sprintf("podless_leak: agent_state=%s but no pod found", bead.AgentState),
	}); err != nil {
		r.logger.Warn("failed to close leaked bead", "bead", bead.ID, "error", err)
		return false
	}
	return true
}

// cleanStaleDrains clears drain entries whose pod is gone. The node-drain
// branch opens a draining entry then continue's without ever calling
// DrainReady (which is the only path to clearDrain), so a force-reclaimed
// pod's entry would otherwise leak and later poison the graceful drain of a
// same-named recreated pod (ka-0000000604).
func (r *Reconciler) cleanStaleDrains(actualMap map[string]corev1.Pod) {
	livePodNames := make(map[string]struct{}, len(actualMap))
	for name := range actualMap {
		livePodNames[name] = struct{}{}
	}
	r.upgradeTracker.CleanStaleDrains(livePodNames)
}

// sortedByCreation sorts desired pods by bead creation time (oldest first) so
// that when the concurrency cap is reached, longer-waiting agents are
// prioritized. Without this, Go map iteration is random and agents can be
// starved.
func sortedByCreation(desired map[string]beadsapi.AgentBead) []desiredEntry {
	sortedDesired := make([]desiredEntry, 0, len(desired))
	for name, bead := range desired {
		sortedDesired = append(sortedDesired, desiredEntry{name, bead})
	}
	slices.SortFunc(sortedDesired, func(a, b desiredEntry) int {
		return a.bead.CreatedAt.Compare(b.bead.CreatedAt)
	})
	return sortedDesired
}

// reconcileDesired converges one desired agent: it skips, replaces, or leaves
// its live pod, then admits and creates a missing one. stop ends the pass's
// create loop (the creation rate limit was hit); err aborts the pass.
func (r *Reconciler) reconcileDesired(ctx context.Context, p *reconcilePass, entry desiredEntry) (stop bool, err error) {
	if r.skipCreate(ctx, p, entry) {
		return false, nil
	}
	if pod, exists := p.actualMap[entry.name]; exists {
		recreate, err := r.reconcileLivePod(ctx, p, entry, pod)
		if err != nil || !recreate {
			return false, err
		}
		// Fall through to create.
	}
	return r.admitAndCreate(ctx, p, entry), nil
}

// skipCreate reports whether the agent is not this pass's to create.
func (r *Reconciler) skipCreate(ctx context.Context, p *reconcilePass, entry desiredEntry) bool {
	name, bead := entry.name, entry.bead
	// Skip beads the leaked-bead closer just closed this pass (podless +
	// terminal agent_state: done/failed/rate_limited/stopping). They are no
	// longer live work — recreating a pod here would back a CLOSED bead with a
	// running agent, which the next pass orphan-reaps, churning create→delete
	// every cycle. Tracking the closer's actual closes (rather than re-testing
	// agent_state) keeps this exactly in step with what it acted on: a bead it
	// skipped (snapshot exemption) or failed to close is NOT skipped here.
	if _, closed := p.closedThisPass[bead.ID]; closed {
		return true
	}
	// Skip beads previously hard-failed by permanent K8s validation errors.
	// Without this, beads with invalid pod names would retry forever on
	// every reconcile pass. The leaked-bead closer above will eventually
	// close these; until then, do not attempt another CreateAgentPod.
	// (When the closer is wired, a podless failed bead is already handled by
	// the closedThisPass skip above; this remains the guard when the closer is
	// absent — e.g. single-binary dev — or the close failed transiently.)
	if bead.AgentState == "failed" {
		return true
	}
	// Exempt lambda_microvm agents from the create-missing / cold-start path
	// (C5, ka-0000001837). A MicroVM agent runs as a MicroVM, never a pod, so
	// it has no pod to create-if-missing; its lifecycle was already
	// reconciled against its runtime status in the MicroVM pass above.
	// Cold-starting a pod here would spawn the WRONG backend for the bead
	// (the handoff invariant: one spawn event, exactly one runtime). No-op
	// unless the MicroVM backend is wired.
	if _, ok := p.microvmHandled[bead.ID]; ok {
		return true
	}
	// Exempt frozen-awaiting-thaw agents from the create-missing path
	// (ka-0000006427, decision ka-0000004662: fix_race_too). A freeze
	// checkpoints and tears down Pod A, leaving the bead terminal+podless
	// but with a persisted snapshot; the wake path (AgentSpawn -> ft.thaw)
	// owns its re-creation through the placeholder-Pod-B lifecycle. If this
	// path COLD-STARTS a pod under the canonical name first, that pod is a
	// live duplicate that (a) wakes the agent into a cold start discarding
	// the snapshot's restored state, and (b) makes the in-flight thaw's
	// teardown-barrier (ka-0000003702) wait out its full timeout for a
	// "Pod A" that will never delete, then fall back to cold start — the
	// exact re-drive race observed on prod 2026.158.17. Mirror the
	// leaked-bead closer's snapshot exemption above: when a snapshot exists,
	// skip create-missing and let the thaw drive. The checker is only wired
	// when cfg.FreezeThawEnabled is on (hasSnapshot nil otherwise), so this
	// is a no-op under the default cold-start lifecycle.
	//
	// agent_state=stopping is the frozen-awaiting-thaw resting state a
	// successful freeze now leaves the bead in (suspendAgent; the controller
	// no longer overwrites it to "done" — bug ka-0000007439). Like the
	// terminal states, a podless bead with a live snapshot must NOT be
	// cold-created over — the wake path (AgentSpawn -> ft.thaw) owns its
	// re-creation, and a duplicate cold pod re-triggers the re-drive race
	// (ka-0000006427) this exemption exists to prevent.
	if _, hasPod := p.actualMap[name]; !hasPod && r.hasSnapshot != nil &&
		isFreezeRestingState(bead.AgentState) &&
		r.hasSnapshot(ctx, bead.AgentName) {
		r.logger.Info("skipping create-missing: agent has freeze/thaw snapshot (thaw owns re-creation)",
			"pod", name, "agent", bead.AgentName, "agent_state", bead.AgentState)
		return true
	}
	return false
}

// reconcileLivePod handles a desired agent whose pod exists. recreate reports
// that the pod was deleted and must be created again this pass.
func (r *Reconciler) reconcileLivePod(ctx context.Context, p *reconcilePass, entry desiredEntry, pod corev1.Pod) (recreate bool, err error) {
	// Pod exists. Check if it's in a terminal state (Failed or Succeeded).
	if isTerminalPod(pod) {
		return r.reapTerminalPod(ctx, entry, pod)
	}
	if r.nodeDrainInProgress(ctx, &pod, p.nodeDoomed) {
		r.nudgeNodeDrain(ctx, entry, pod)
		// Nothing more to do for this pod this pass: it either drains and
		// its node deletes it, or the node drain force-kills it after the
		// grace period. Either way the controller must not recreate it here.
		return false, nil
	}
	return false, nil
}

// reapTerminalPod deletes a Failed/Succeeded pod so it can be recreated.
func (r *Reconciler) reapTerminalPod(ctx context.Context, entry desiredEntry, pod corev1.Pod) (recreate bool, err error) {
	name, bead := entry.name, entry.bead
	// Exempt the restored placeholder Pod B from the terminal-delete
	// reaper while its thaw is still in flight (bug ka-0000002058,
	// decision ka-0000005300: exempt_reaper). After a successful
	// `runsc restore` graft, Pod B's k8s phase flips to Failed within
	// seconds — the sleep placeholder PID it was admitted on has been
	// replaced by the restored sentry, so the kubelet sees its original
	// container exit — but the restored sandbox is LIVE and the in-pod
	// gb+coop watchers are mid-rehydration. Deleting Pod B here (5s
	// after restore, well before the 60s rehydrate deadline) makes the
	// node tear down the kubelet-owned netns, so both watcher acks fail
	// "Network is unreachable" and the thaw fail-closes. The thaw
	// lifecycle owns Pod B teardown on BOTH the success and fail-closed
	// paths, so the reaper must stand off while a thaw holds the slot.
	// The exemption is narrow: it fires only for an actual thaw
	// placeholder (LabelThawPlaceholder) whose agent IsThawing right
	// now. A Pod B that genuinely outlived its thaw (controller crash,
	// abandoned thaw) is no longer in flight, so IsThawing is false and
	// it is reaped normally on the next pass — no permanent leak. This
	// mirrors the create-missing snapshot exemption in skipCreate.
	if r.isThawing != nil &&
		pod.Labels[podmanager.LabelThawPlaceholder] == "true" &&
		r.isThawing(bead.AgentName) {
		r.logger.Info("skipping terminal-delete: thaw placeholder Pod B is rehydrating (thaw owns teardown)",
			"pod", pod.Name, "agent", bead.AgentName, "phase", pod.Status.Phase)
		return false, nil
	}
	// Delete by pod.Name (not the desired key): a thaw-placeholder Pod
	// B is mapped under its canonical name but its real k8s object is
	// "<canonical>-thaw-b" (ka-0000007549). For every other pod
	// pod.Name == name, so this is a no-op rename.
	r.logger.Info("deleting terminal pod for recreation",
		"pod", pod.Name, "phase", pod.Status.Phase)
	// A pod failed by a node reclaim still holds its node-drain
	// entry; end it so the recreated session does not get the mail.
	r.upgradeTracker.EndDrain(ctx, name)
	if err := r.pods.DeleteAgentPod(ctx, pod.Name, pod.Namespace); err != nil {
		return false, fmt.Errorf("deleting terminal pod %s: %w", pod.Name, err)
	}
	return true, nil
}

// nudgeNodeDrain handles a pod whose node is being force-reclaimed (WS1,
// ka-0000007969). Every agent pod carries do-not-disrupt and no
// NodePool sets terminationGracePeriod, so Karpenter never
// voluntarily drains a node under a running agent — a disrupted
// taint / DeletionTimestamp here means AWS is taking the instance
// (spot / health / interruption-queue) and the kubelet will SIGTERM
// this pod shortly. Route the agent through the SAME graceful
// commit+push drain account rotation uses, so it lands its
// WIP before the node drain reaches it. We do NOT delete the pod
// ourselves — the node teardown owns that; we only nudge and then
// let the loop fall through (no DeleteAgentPod).
// The nudge is interrupt mail to the agent, not a dial to the pod.
func (r *Reconciler) nudgeNodeDrain(ctx context.Context, entry desiredEntry, pod corev1.Pod) {
	name, bead := entry.name, entry.bead
	if pod.Status.Phase != corev1.PodRunning {
		return
	}
	// One "drained" lifecycle event per drain episode: the
	// tracker keeps the entry until the pod goes away, so
	// IsDraining dedups the per-pass re-entry (ka-0000000550).
	wasDraining := r.upgradeTracker.IsDraining(name)
	if r.upgradeTracker.StartDrainWithMsg(ctx, name, bead.AgentName, nodeDrainNudgeMsg) {
		if !wasDraining && r.lifecycleRecorder != nil {
			r.lifecycleRecorder.RecordLifecycleEvent("drained", bead.Project, bead.Role, "")
		}
		r.logger.Info("node reclaim detected, mailed agent to commit+push before teardown",
			"pod", name, "node", pod.Spec.NodeName)
	}
}

// admitAndCreate builds the agent's pod spec, runs it through the refusal,
// cap and credential gates, and creates the pod. It reports whether the pass
// must stop creating (the creation rate limit was hit).
func (r *Reconciler) admitAndCreate(ctx context.Context, p *reconcilePass, entry desiredEntry) (stop bool) {
	name, bead := entry.name, entry.bead

	// Build the desired pod spec up front so the cap checks can route by the
	// pod's TARGET namespace (the spec builder applies routeGVisorNamespace,
	// so a runsc agent's spec.Namespace is the gVisor namespace). The spec is
	// reused for CreateAgentPod below — built once per admitted bead.
	spec := r.specBuilder(r.cfg, bead.Project, bead.Mode, bead.Role, bead.AgentName, bead.Metadata)
	spec.BeadID = bead.ID
	isGVisor := p.inGVisorNamespace(spec.Namespace)

	if r.refuseSpec(ctx, name, bead, spec) {
		return false
	}

	// Pool-exhaustion escape hatch (ka-0000002560): the spec builder set
	// SpawnBlocked because EVERY account in this project's pool is
	// simultaneously capacity-exhausted, so there is no healthy key to bake.
	// Defer (NOT hard-fail) so the bead returns to the queue and a later pass
	// spawns it once a rate-limit window resets — instead of baking onto a
	// known-dead account. Mirrors the event-door guard in cmd/controller;
	// this door defers + marks queued (the per-pass deferral machinery also
	// emits stale-deferral warnings if the pool stays dead too long).
	if spec.SpawnBlocked {
		r.logger.Warn("deferring pod create — project account pool fully exhausted (will re-attempt next sync)",
			"pod", name, "bead", bead.ID, "agent", bead.AgentName,
			"project", bead.Project, "reason", spec.SpawnBlockReason)
		r.deferBead(ctx, p, entry, "pool_exhausted")
		return false
	}

	if r.spawnWallRefuses(ctx, p, name, bead) {
		return false
	}

	if reason, stop := r.capDeferral(p, name, bead, isGVisor); reason != "" {
		r.deferBead(ctx, p, entry, reason)
		return stop
	}

	releaseToken, ok := r.applyAgentCredentials(ctx, p, entry, &spec)
	if !ok {
		return false
	}

	// Record the resolved harness on the bead before the pod boots, so gb
	// init / coop read it instead of re-resolving (ADR 0004 section 4).
	r.stampResolvedHarness(ctx, bead, spec.Harness)
	r.claimSpawnTask(ctx, bead, spec)

	// Create the pod (spec built above).
	r.logger.Info("creating pod", "pod", name)
	if err := r.pods.CreateAgentPod(ctx, spec); err != nil {
		r.handleCreateError(ctx, p, entry, spec, releaseToken, err)
		return false
	}
	r.recordPodCreated(p, bead, spec, isGVisor)
	return false
}

// refuseSpec hard-fails a bead whose built spec must never become a pod, and
// reports whether it did.
func (r *Reconciler) refuseSpec(ctx context.Context, name string, bead beadsapi.AgentBead, spec podmanager.AgentPodSpec) bool {
	// Execution-backend routing (epic ka-0000005010): an agent resolved to a
	// non-Kubernetes backend must never be realized as a pod here. This mirrors
	// the event-door guard in handleEvent; the reconciler is the backstop for
	// beads created during controller downtime. lambda_microvm agents are
	// handled by the C5 MicroVM lifecycle exemption at the top of this loop
	// (reconciled by runtime status, never pod-created), so a lambda_microvm
	// bead only reaches HERE when the MicroVM backend is unavailable
	// (default-off / misconfigured). Fail closed in that case rather than
	// cold-starting a pod against the wrong backend; the same fail-closed
	// stance the event door (handleNonK8sSpawn) takes.
	if !podmanager.IsKubernetesTarget(spec.ExecutionTarget) {
		r.logger.Warn("refusing to create pod for non-kubernetes agent bead — MicroVM backend unavailable (default-off or misconfigured)",
			"pod", name, "bead", bead.ID, "agent", bead.AgentName,
			"execution_target", spec.ExecutionTarget)
		r.markAgentFailed(ctx, bead, errNonK8sBackendUnimplemented)
		return true
	}

	// Fail-closed credential backstop (ka-0000007608, guard #2): a project-less
	// agent bead resolves no account/credential (applyAccountConfig keys off
	// project), so it would spawn a loginless pod with no CLAUDE_CODE_OAUTH_TOKEN.
	// The event-driven spawn path (handleEvent) refuses + escalates this, but a
	// bead created during controller downtime reaches the reconciler without
	// having passed that gate. Hard-fail it here so it is never loginlessly
	// spawned and stops re-driving every pass. Escalation emission lives on the
	// event door; this door just refuses.
	if spec.Project == "" {
		r.logger.Error("refusing to create pod for project-less agent bead — would spawn a loginless pod (no Claude OAuth credential)",
			"pod", name, "bead", bead.ID, "agent", bead.AgentName)
		r.markAgentFailed(ctx, bead, errLoginlessNoProject)
		return true
	}

	// Harness misconfiguration (gd-helNySn0WA): the spec builder could not
	// resolve a valid harness (unknown value, or one the resolved account's
	// provider cannot serve). Creating the pod anyway would silently boot a
	// Claude pod for a codex agent. Permanent, like the event-door guard
	// (evaluateSpawnCredentials): hard-fail the bead with the reason, which
	// already names the bead edit that fixes it.
	if spec.HarnessError != "" {
		r.logger.Error("refusing to create pod for agent bead — invalid agent harness",
			"pod", name, "bead", bead.ID, "agent", bead.AgentName,
			"project", bead.Project, "role", bead.Role, "reason", spec.HarnessError)
		r.markAgentFailed(ctx, bead, fmt.Errorf("invalid agent harness: %s", spec.HarnessError))
		return true
	}
	return false
}

// spawnWallRefuses applies the Lands spawn wall (Track B §3.5, step 8.3),
// mirroring the event door: a new spawn into a tenant land that is frozen,
// has tenant spawns off, or whose purse is at the wall or unknown is not
// created. The bead stays open at agent_state=budget_exceeded (not deferred,
// so queue expiry leaves it alone) and is re-decided next pass. Checked before
// the caps so a refused bead is never also marked queued.
//
// The pod of an agent that already runs is recreated unless the land's kill
// switches are on (SpawnWall.Halted): the purse wall does not stop a running
// agent, a freeze or lands-pause does (gd-Nc7yMpW7U6).
func (r *Reconciler) spawnWallRefuses(ctx context.Context, p *reconcilePass, name string, bead beadsapi.AgentBead) bool {
	verdict := r.wallVerdict(ctx, p, bead)
	if !verdict.Refused() {
		return false
	}
	r.logger.Warn("refusing pod create — Lands spawn wall",
		"pod", name, "bead", bead.ID, "agent", bead.AgentName, "agent_state", bead.AgentState,
		"project", bead.Project, "code", verdict.Code, "reason", verdict.Reason)
	if r.spawnWall.Record != nil {
		r.spawnWall.Record(ctx, bead, verdict)
	}
	return true
}

// wallVerdict is the spawn wall's verdict on creating bead's pod: Check, then
// Rate, for a new spawn; Halted for the recreate of an agent that already
// runs. Check and Halted are memoised for the pass.
func (r *Reconciler) wallVerdict(ctx context.Context, p *reconcilePass, bead beadsapi.AgentBead) wall.Refusal {
	memo := func(key wallKey, ask func() wall.Refusal) wall.Refusal {
		v, seen := p.wallVerdicts[key]
		if !seen {
			v = ask()
			p.wallVerdicts[key] = v
		}
		return v
	}
	if !wall.NewSpawn(bead.AgentState) {
		if r.spawnWall.Halted == nil {
			return wall.Refusal{}
		}
		return memo(wallKey{project: bead.Project, halted: true}, func() wall.Refusal {
			return r.spawnWall.Halted(ctx, bead.Project)
		})
	}
	if r.spawnWall.Check == nil {
		return wall.Refusal{}
	}
	v := memo(wallKey{project: bead.Project, role: bead.Role}, func() wall.Refusal {
		return r.spawnWall.Check(ctx, bead.Project, bead.Role)
	})
	if !v.Refused() && v.Tenant && r.spawnWall.Rate != nil {
		v = r.spawnWall.Rate(ctx, bead)
	}
	return v
}

// capDeferral checks the burst limit, the pod caps and the creation rate
// limiter in order. It returns the deferral reason of the first one that
// holds the pod back ("" when none does) and whether the pass must stop
// creating entirely.
func (r *Reconciler) capDeferral(p *reconcilePass, name string, bead beadsapi.AgentBead, isGVisor bool) (reason string, stop bool) {
	// Check burst limit.
	if p.created >= p.burstLimit {
		r.logger.Info("spawn burst limit reached, deferring remaining pods",
			"limit", p.burstLimit, "deferred", name)
		return "burst_limit", false
	}

	// Check max concurrent pods.
	if r.cfg.CoopMaxPods > 0 && p.activePods >= r.cfg.CoopMaxPods {
		r.logger.Info("max concurrent pods reached, deferring pod",
			"limit", r.cfg.CoopMaxPods, "active", p.activePods, "deferred", name)
		return "max_pods", false
	}

	if isGVisor {
		// gVisor-namespace pods are capped by the distinct gVisor budget,
		// never the per-project cap (ka-0000004152). A zero budget means no
		// separate gVisor ceiling — bounded only by the global CoopMaxPods
		// check above — so a runsc sub-pool can stand up regardless of how
		// saturated the spawning project's per-project cap is.
		if r.cfg.CoopMaxPodsGVisor > 0 && p.activeGVisorPods >= r.cfg.CoopMaxPodsGVisor {
			r.logger.Info("max concurrent gVisor pods reached, deferring pod",
				"limit", r.cfg.CoopMaxPodsGVisor, "namespace", r.cfg.GVisorNamespace,
				"active", p.activeGVisorPods, "deferred", name)
			return "max_pods_gvisor", false
		}
	} else if r.cfg.CoopMaxPodsPerProject > 0 && p.activePodsPerProject[bead.Project] >= r.cfg.CoopMaxPodsPerProject {
		// Check per-project max concurrent pods (non-gVisor pods only).
		r.logger.Info("max concurrent pods per project reached, deferring pod",
			"limit", r.cfg.CoopMaxPodsPerProject, "project", bead.Project,
			"active", p.activePodsPerProject[bead.Project], "deferred", name)
		return "max_pods_per_project", false
	}

	// Check rate limiter — prevent stampede by capping creations per window.
	if r.rateLimiter != nil && !r.rateLimiter.Allow() {
		r.logger.Warn("pod creation rate limit exceeded, pausing all creation this pass",
			"window", r.cfg.CoopRateLimitWindow,
			"max", r.cfg.CoopRateLimitMax,
			"deferred", name)
		return "rate_limit", true // stop creating entirely this pass
	}
	return "", false
}

// deferBead records the agent as deferred this pass and marks its bead queued.
func (r *Reconciler) deferBead(ctx context.Context, p *reconcilePass, entry desiredEntry, reason string) {
	r.recordDeferred(entry.bead, reason)
	p.deferred = append(p.deferred, entry)
	p.deferredThisPass[entry.bead.ID] = entry.bead
	r.markQueued(ctx, entry.bead, reason)
}

// applyAgentCredentials applies the per-agent beads token and git credential
// to spec. ok=false means the pod must not be created this pass (the bead was
// deferred). releaseToken undoes a token mint made in THIS pass when the pod
// is then not created.
//
// Per-agent git credential (GitHub App token). Fail closed: a refusal
// defers the bead for the next sync and never creates the pod with the
// shared project credential. Runs after every cap/rate check so a
// deferred bead never has a token minted for it.
// Per-agent beads token (PER_AGENT_TOKENS): fail closed, never create the
// pod on the shared token.
func (r *Reconciler) applyAgentCredentials(ctx context.Context, p *reconcilePass, entry desiredEntry, spec *podmanager.AgentPodSpec) (releaseToken func(), ok bool) {
	name, bead := entry.name, entry.bead
	tokenMinted := false
	if r.agentTokens.Apply != nil {
		minted, err := r.agentTokens.Apply(ctx, spec, bead.ID, bead.AgentName)
		if err != nil {
			r.logger.Warn("per-agent beads token unavailable, deferring pod create (fail closed; will re-attempt next sync)",
				"pod", name, "bead", bead.ID, "agent", bead.AgentName, "error", err)
			r.deferBead(ctx, p, entry, "agent_token_refused")
			return nil, false
		}
		tokenMinted = minted
	}
	releaseToken = func() {
		if tokenMinted && r.agentTokens.Release != nil {
			r.agentTokens.Release(ctx, *spec, bead.ID)
		}
	}

	if r.gitCreds.Apply != nil {
		if refuse, reason := r.gitCreds.Apply(ctx, spec, bead.ID); refuse {
			releaseToken()
			r.logger.Warn("per-agent git credential unavailable, deferring pod create (fail closed; will re-attempt next sync)",
				"pod", name, "bead", bead.ID, "agent", bead.AgentName, "reason", reason)
			r.deferBead(ctx, p, entry, "git_credential_refused")
			return nil, false
		}
	}
	return releaseToken, true
}

// handleCreateError undoes the per-agent credentials of a failed
// CreateAgentPod and either hard-fails or defers the bead.
func (r *Reconciler) handleCreateError(ctx context.Context, p *reconcilePass, entry desiredEntry, spec podmanager.AgentPodSpec, releaseToken func(), err error) {
	name, bead := entry.name, entry.bead
	if r.gitCreds.Release != nil {
		r.gitCreds.Release(ctx, spec, bead.ID)
	}
	releaseToken()
	if isPermanentCreateError(err) {
		// Permanent error (e.g. DNS-1123 pod name violation) — hard-fail
		// the bead so it doesn't silently retry forever and so the
		// bridge can surface a crash notification.
		r.logger.Error("pod creation failed with permanent error, marking bead failed",
			"pod", name, "bead", bead.ID, "error", err)
		r.markAgentFailed(ctx, bead, err)
		return
	}
	// Transient create error (e.g. MissingSecretsError while a
	// project/gVisor-namespace ExternalSecret is mid-sync, a stale
	// pod-name conflict, or a daemon/API blip). Retry next sync — but
	// DEFER the bead the same way the cap/pool paths do, so the wedge is
	// visible and self-healing instead of a silent black hole
	// (ka-0000006128). Without this the bead's agent_state stayed empty
	// and it was tracked nowhere: invisible on /plans, un-nudgeable, no
	// deferred metric or stale-deferral warning, and never subject to
	// queue-expiry — so it sat open until the 2h stale reaper closed it
	// as failed with runs_completed=0. That is exactly how every runsc
	// plan-gardener wedged during a transient gVisor-namespace
	// nats-agent-creds sync gap (same failure class as ka-0000004273).
	// Marking it queued + recording it deferred routes it through
	// expireDeferredBeads, which auto-closes as agent_state=expired after
	// CoopQueueExpiryTimeout (default 1h) — visible, metered, bounded.
	r.logger.Warn("pod create failed with transient error, deferring for retry next sync",
		"pod", name, "bead", bead.ID, "error", err)
	r.deferBead(ctx, p, entry, "create_error")
}

// recordPodCreated does the bookkeeping for a pod created this pass.
func (r *Reconciler) recordPodCreated(p *reconcilePass, bead beadsapi.AgentBead, spec podmanager.AgentPodSpec, isGVisor bool) {
	// Optimistically bump the per-account live-pod count so the next pod
	// created this pass keeps spreading across the account pool
	// (ka-0000004861).
	r.cfg.BumpAccountLiveCount(spec.ResolvedAccount)
	// Phase 0 (ka-0000002142): count which applyAccountConfig layer produced
	// this pod's account, mirroring the event-door spawn path so the metric
	// covers reconciler-created (create-missing backstop) pods too.
	if r.lifecycleRecorder != nil {
		r.lifecycleRecorder.RecordAccountLayer(spec.AccountLayer, bead.Project, bead.Role)
	}
	if r.rateLimiter != nil {
		r.rateLimiter.Record()
	}
	if r.circuitBreaker != nil {
		r.circuitBreaker.Record()
	}
	p.created++
	p.activePods++
	if isGVisor {
		p.activeGVisorPods++
	} else {
		p.activePodsPerProject[bead.Project]++
	}
}

// logPassSummary logs the pass result when it created pods or is short of
// desired.
func (r *Reconciler) logPassSummary(p *reconcilePass) {
	if p.created == 0 && len(p.desired) <= len(p.actualMap) {
		return
	}
	// idleActive summary makes the Phase 0 measurement (ka-0000006778)
	// visible in logs alongside the gauge: fleet-wide active/idle agent
	// counts and the idle reserved-memory total for this pass.
	activeAgents, idleAgents, idleMemBytes := idleActiveSummary(p.desired, p.actualMap)
	r.logger.Info("reconcile pass complete",
		"created", p.created, "active", p.activePods,
		"desired", len(p.desired), "deferred", len(p.deferred),
		"burst_limit", p.burstLimit,
		"active_agents", activeAgents, "idle_agents", idleAgents,
		"idle_reserved_mem_bytes", idleMemBytes)
}
