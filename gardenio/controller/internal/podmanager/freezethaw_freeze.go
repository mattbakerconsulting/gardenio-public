package podmanager

// freezethaw_freeze.go — the FREEZE verb: in-flight freeze/thaw tracking, abort, watcher-armed checks and Freeze itself.
// Split out of freezethaw.go (gd-Wl6LuCnvlH); see freezethaw.go for the design overview.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/thawepoch"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// --- FREEZE -----------------------------------------------------------------

// ErrFreezeAborted is returned by Freeze when an inbound-message interrupt
// (AbortFreeze, C2 task ka-0000005834) cancelled the freeze before it durably
// persisted a usable snapshot — OR persisted one that was immediately discarded
// because the abort raced checkpoint-completion. It is NOT a failure: because
// `runsc checkpoint --leave-running` keeps Pod A alive through the checkpoint,
// the abort is a clean no-op on the live agent. The caller MUST treat it as
// "freeze deliberately abandoned, agent is still live" — specifically it must
// NOT tear Pod A down (the agent must stay up to serve the message that
// triggered the abort) and must NOT cold-start. Distinguishing this from a real
// freeze error is the whole point: a real error falls through to teardown +
// cold-start (today's safe fallback), an abort keeps the warm agent.
var ErrFreezeAborted = errors.New("freeze: aborted by inbound-message interrupt (agent left live)")

// ErrFreezeWatchersNotArmed is returned by Freeze when the agent's in-pod
// rehydration watchers (coop + gb) have not yet armed — i.e. their armed
// sentinels are absent from the per-agent thaw-state dir on the node
// (thawepoch.WriteArmed). A runsc pod is k8s-health-ready well before its agent
// loop finishes booting; checkpointing in that window produces a snapshot with
// no live watcher inside it, so on thaw the controller's rehydration gate can
// never be acked (the watcher that would ack does not exist in the restored
// image) and the thaw fail-closes to a cold start even though `runsc restore`
// returned rc=0 (bug ka-0000006667 / ka-0000007918). This is NOT a checkpoint
// failure: it is a deliberate refusal to mint a structurally un-thawable
// snapshot. The caller treats it like any other non-abort freeze miss — fall
// through to a normal teardown so the next spawn cold-starts cleanly.
var ErrFreezeWatchersNotArmed = errors.New("freeze: in-pod rehydration watchers not armed (snapshot would be un-thawable)")

// ErrFreezeAlreadySnapshotted is returned by Freeze when a checkpoint is
// requested for an agent that ALREADY has a persisted snapshot AND whose source
// Pod A is gone or already Terminating. This is the defense-in-depth guard for
// the duplicate-Freeze bug ka-0000000194: a duplicate AgentStop (or any second
// emission) drove a second Freeze ~2s after the first; by then Pod A was
// half-torn-down, so re-checkpointing captured a degenerate image (268M→432K,
// EMPTY agent overlay-opts) that OVERWROTE the good snapshot and made the bundle
// un-thawable. A checkpoint of a dying pod can never improve on a snapshot taken
// while the pod was fully live, so Freeze refuses and returns the existing good
// snapshot unchanged. Like ErrFreezeWatchersNotArmed this is NOT a failure: the
// caller keeps the good snapshot and proceeds to the normal teardown.
var ErrFreezeAlreadySnapshotted = errors.New("freeze: snapshot already exists and source pod is gone/Terminating (refusing duplicate checkpoint that would overwrite a good snapshot)")

// ErrFreezeAlreadyThawed is returned by Freeze when the agent's persisted
// snapshot is already CONSUMED — a prior freeze→thaw cycle completed and reaped
// the checkpoint image (Snapshot.Consumed, ka-0000001878). A path-b checkpoint
// is node-local and single-shot: re-freezing a thawed agent would mint a second
// node-local checkpoint reusing /var/lib/gardenio/{ckpt,bundle}/<agent>, whose
// bundle now backs the LIVE thawed Pod B — clobbering the live overlay and
// persisting a stale record (multi-cycle re-freeze is the snapshot-transport
// follow-up's concern, restore-through-lifecycle.md "remaining work"). So Freeze
// REFUSES and DISCARDS the consumed snapshot. Like ErrFreezeWatchersNotArmed and
// ErrFreezeAlreadySnapshotted this is NOT a failure: the caller falls through to
// the normal teardown and the next spawn cold-starts cleanly (today's safe
// fallback). This is the explicit, asserted re-freeze guard the production
// readiness review required — replacing the prior IMPLICIT safety that relied
// only on the thawed pod's name (<canonical>-thaw-b) missing shouldFreeze's
// canonical-name pod Get.
var ErrFreezeAlreadyThawed = errors.New("freeze: snapshot already consumed by a prior freeze→thaw cycle (path-b is single-shot; refusing node-local re-freeze of a thawed agent, degrading to cold start)")

// IsFreezing reports whether a freeze for the agent is currently in flight (its
// node-checkpoint exec is running). The message-ingress path consults this
// before deciding cold-start vs. route-to-live: a true here means an AbortFreeze
// can interrupt the checkpoint and hand the message to the still-warm agent.
// Always false when InterruptEnabled is off (the registry is never populated).
func (f *FreezeThawManager) IsFreezing(agentName string) bool {
	if f == nil || agentName == "" {
		return false
	}
	f.inflightMu.Lock()
	defer f.inflightMu.Unlock()
	_, ok := f.inflight[agentName]
	return ok
}

// IsThawing reports whether a thaw for the agent is currently in flight — from
// the top of Thaw through its bounded rehydration hand-off. The reconciler's
// terminal-pod reaper consults this to exempt the restored placeholder Pod B
// while it is still rehydrating: Pod B reports phase=Failed within seconds of
// the restore graft, and reaping it inside the rehydration window tears down the
// netns out from under the in-pod watchers (bug ka-0000002058). Always false on
// a nil manager or empty agent name; the registry is refcounted so concurrent or
// retried thaws for the same agent never prematurely clear the slot.
func (f *FreezeThawManager) IsThawing(agentName string) bool {
	if f == nil || agentName == "" {
		return false
	}
	f.thawInflightMu.Lock()
	defer f.thawInflightMu.Unlock()
	return f.thawInflight[agentName] > 0
}

// registerThaw marks a thaw for the agent in flight and returns a release func
// the caller defers. It refcounts so a retried or racing thaw for the same
// agent keeps the slot held until the last release. The slot is held for the
// entire Thaw call — including the rehydration hand-off — so the reconciler's
// terminal-pod reaper stands off Pod B for the whole window (bug ka-0000002058).
func (f *FreezeThawManager) registerThaw(agentName string) func() {
	if f == nil || agentName == "" {
		return func() {}
	}
	f.thawInflightMu.Lock()
	f.thawInflight[agentName]++
	f.thawInflightMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			f.thawInflightMu.Lock()
			if f.thawInflight[agentName] <= 1 {
				delete(f.thawInflight, agentName)
			} else {
				f.thawInflight[agentName]--
			}
			f.thawInflightMu.Unlock()
		})
	}
}

// InflightThawCount reports how many thaws are currently in flight across all
// agents. The controller shutdown drain (cmd/controller) polls this to wait for
// detached thaw critical sections to finish before the process exits, so a
// controller roll does not abandon an in-flight restore/rehydrate (ka-0000001355
// harden-thaw). Zero on a nil manager.
func (f *FreezeThawManager) InflightThawCount() int {
	if f == nil {
		return 0
	}
	f.thawInflightMu.Lock()
	defer f.thawInflightMu.Unlock()
	n := 0
	for _, c := range f.thawInflight {
		if c > 0 {
			n += c
		}
	}
	return n
}

// DrainThaws blocks until every in-flight thaw's detached critical section has
// finished (InflightThawCount hits 0) or the supplied ctx (a bounded drain
// budget) expires. It is the controller's graceful-shutdown hook: a thaw's
// post-commit section (barrier restore + rehydration + reap + Pod B teardown)
// runs on a ctx DECOUPLED from the leader-election ctx (see Thaw), so on
// SIGTERM / leader-loss those sections keep running while the run loop has
// already observed ctx.Done(). Without this drain the process would exit (and
// k8s SIGKILL after the grace period) mid-restore, re-introducing the exact
// confound + orphaned-container failure this hardening closes. Returns the
// number of thaws still in flight when it returned (0 = fully drained). A nil
// manager / freeze-thaw-disabled controller drains instantly (count 0).
func (f *FreezeThawManager) DrainThaws(ctx context.Context, poll time.Duration) int {
	if f == nil {
		return 0
	}
	if poll <= 0 {
		poll = 250 * time.Millisecond
	}
	if n := f.InflightThawCount(); n == 0 {
		return 0
	}
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return f.InflightThawCount()
		case <-tick.C:
			if n := f.InflightThawCount(); n == 0 {
				return 0
			}
		}
	}
}

// AbortFreeze interrupts an in-flight freeze for the agent: it flips the entry's
// aborted flag and cancels the freeze's node-checkpoint exec ctx, so the
// blocking `runsc checkpoint` exec returns promptly (ProbeNodeExecutor.Exec
// streams under ctx; the node-side runsc is also `timeout`-bounded as a backstop
// — epic constraint). Returns true if an in-flight freeze was found and
// signalled, false if none was in flight (nothing to abort — the freeze already
// finished, or never started). Setting aborted under the lock BEFORE cancelling
// closes the abort-vs-completion TOCTOU: if Freeze has already raced to its
// finalize step, it reads aborted under the same lock and discards the snapshot;
// if it has not, the cancel aborts the exec. Either way the freeze returns
// ErrFreezeAborted and the agent is left live. No-op (false) when
// InterruptEnabled is off.
func (f *FreezeThawManager) AbortFreeze(agentName string) bool {
	if f == nil || agentName == "" {
		return false
	}
	f.inflightMu.Lock()
	entry, ok := f.inflight[agentName]
	if ok {
		entry.aborted = true
	}
	f.inflightMu.Unlock()
	if !ok {
		return false
	}
	f.logger.Info("freeze: aborting in-flight checkpoint on inbound-message interrupt",
		"agent", agentName)
	entry.cancel()
	return true
}

// registerInflight records an in-flight freeze for the agent and returns the
// derived ctx the node-checkpoint exec must run under, a finalize func the
// caller defers, and whether registration happened (false when InterruptEnabled
// is off — the caller then runs the original, un-abortable path). The returned
// ctx is cancelled either by the parent ctx or by a concurrent AbortFreeze.
//
// finalize(persisted) removes the registry entry and reports whether the freeze
// was aborted; the caller passes whether a snapshot was persisted so finalize
// can drive the post-Put discard (lazy reap) when an abort raced completion. It
// is safe to call finalize exactly once; calling it returns the final aborted
// verdict under the lock so the caller resolves the TOCTOU deterministically.
func (f *FreezeThawManager) registerInflight(ctx context.Context, agentName string) (context.Context, func() bool, bool) {
	if !f.InterruptEnabled {
		return ctx, func() bool { return false }, false
	}
	cctx, cancel := context.WithCancel(ctx)
	entry := &inflightFreeze{cancel: cancel}
	f.inflightMu.Lock()
	// A prior entry for the same agent should not exist (one freeze per agent at
	// a time — graceful stop is serialized per agent), but if it does, signal it
	// aborted and replace it so the new freeze owns the slot.
	if old, ok := f.inflight[agentName]; ok {
		old.aborted = true
		old.cancel()
	}
	f.inflight[agentName] = entry
	f.inflightMu.Unlock()

	finalize := func() bool {
		f.inflightMu.Lock()
		aborted := entry.aborted
		// Only delete if WE still own the slot — a replacement (above) must not
		// be clobbered by an older freeze's finalize.
		if cur, ok := f.inflight[agentName]; ok && cur == entry {
			delete(f.inflight, agentName)
		}
		f.inflightMu.Unlock()
		cancel() // release the derived ctx's resources
		return aborted
	}
	return cctx, finalize, true
}

// FreezeRequest identifies the live agent pod to checkpoint.
type FreezeRequest struct {
	AgentName string // agent identity (snapshot key)
	BeadID    string // agent bead id (recorded on the snapshot)
	PodName   string // live Pod A name
	Namespace string

	// Node hosting Pod A. If empty, Freeze resolves it from the pod's
	// spec.nodeName via the k8s API.
	Node string
}

// FreezeTiming carries the per-step freeze attribution (task ka-0000005147). The
// controller freeze path feeds these into the gardenio_freeze_latency_seconds
// histogram so the dashboard learns WHERE the freeze seconds go — the ~3.0G image
// write vs. the Step-3 relocation copy vs. persist — before C3 optimizes. The
// node sub-timers (Checkpoint, Relocation) come from the freeze script's FT_*_MS
// sentinels; Persist is measured Go-side around the SnapshotStore.Put. All are
// best-effort: a sentinel parse miss leaves that field zero, the freeze still
// succeeds, and only that one sub-step's sample is skipped by the caller. The
// TOTAL (freeze requested → snapshot persisted) is measured by the caller around
// the whole Freeze call, not here, so it captures the control-plane glue too.
type FreezeTiming struct {
	// Checkpoint is the runsc page-dump + 3.0G image write (node script [2/8]).
	Checkpoint time.Duration
	// Relocation is the Step-3 rootfs/bundle relocation copy (node script [3/8]).
	Relocation time.Duration
	// Persist is the SnapshotStore.Put (Go side).
	Persist time.Duration
	// Payload carries the workspace payload-completeness verdict (ka-0000006933)
	// so the caller can record it as gardenio_freeze_payload_* metrics from the
	// same freeze-success branch that records freeze_latency — NOT inside Freeze,
	// which would double-count a snapshot the abort-discard defer then discards
	// (an inbound-message interrupt racing completion). Zero-valued on any early
	// error; only meaningful when Freeze returns a non-nil snapshot and nil error.
	Payload PayloadVerdict
}

// PayloadVerdict is the parsed workspace payload-completeness result the freeze
// path attaches to FreezeTiming for caller-side metric recording (ka-0000006933,
// finding #4). The freeze is published regardless (payloadComplete is WARN-not-
// gate, invariant I1); this just carries WHAT the completeness telemetry saw so
// the caller can quantify benign dangling-symlink noise vs a real ENOSPC/EIO
// wasted freeze at fleet scale.
type PayloadVerdict struct {
	// WorkCopyFailed / TmpCopyFailed: the /work | /tmp warm copy suffered a
	// GENUINE truncating failure (reason == copyfail, or — older node script —
	// a non-zero cp rc with no reason). Drives the {reason=work_copy|tmp_copy}
	// incomplete counter.
	WorkCopyFailed bool
	TmpCopyFailed  bool
	// WorkSymlinkSkip / TmpSymlinkSkip: the /work | /tmp cp rc was tripped ONLY
	// by a benign dangling symlink cp skipped (reason == symlink); the payload is
	// COMPLETE. Drives the {leg=work|tmp} symlink-skip counter — the quantified
	// noise the alert metric deliberately excludes.
	WorkSymlinkSkip bool
	TmpSymlinkSkip  bool
	// GoferFailed: >0 .gofer sweep mounts had a genuine copytree failure. Drives
	// the {reason=gofer} incomplete counter.
	GoferFailed bool
	// GoferMalformed: the FT_GOFER_FAILED sentinel was present but unparseable
	// (corrupt telemetry). Drives the {reason=gofer_malformed} incomplete counter.
	GoferMalformed bool
}

// Freeze checkpoints a running multi-container agent pod and persists the
// snapshot keyed by agent identity. It implements step 2 of
// restore-through-lifecycle.md / the freeze half of multi-container-resume.md:
//
//	runsc checkpoint --leave-running <sandbox-cid>   (one image, container_count=N baked in)
//	+ preserve both OCI bundles and the warm /work + SA token
//	+ capture each container's live overlay mount opts for thaw-time reconstruction
//
// The runsc invocation is wrapped in `timeout` on the node and the result is
// validated by Go-side post-state assertions (rc==0, image present, sandbox
// still running because of --leave-running, container_count baked in) before
// the snapshot is persisted.
//
// Returns a FreezeTiming alongside the snapshot for the freeze_latency histogram
// (task ka-0000005147); the timing is zero-valued on any early error.
//
// INTERRUPTIBLE FREEZE (C2, task ka-0000005834): when InterruptEnabled is set,
// Freeze registers an in-flight cancel func keyed by agent before the node
// checkpoint exec, so a concurrent AbortFreeze (driven by an inbound user
// message targeting this agent) can cancel the exec and discard the partial
// snapshot. Because the checkpoint is `--leave-running`, Pod A stays alive
// through the whole operation, so an abort is a clean no-op on the live agent —
// Freeze returns ErrFreezeAborted (NOT a real failure) and the caller leaves
// Pod A up to serve the message. The defer below resolves the abort-vs-
// completion race: if the abort lands AFTER a snapshot was already persisted, it
// lazily discards that snapshot (Delete + image reap) and still returns
// ErrFreezeAborted, so we never end up advertising a thaw target for an agent we
// just decided to keep live. When InterruptEnabled is off the registry and defer
// are inert — today's behaviour verbatim.
func (f *FreezeThawManager) Freeze(ctx context.Context, req FreezeRequest) (snap *Snapshot, timing FreezeTiming, err error) {
	if req.AgentName == "" {
		return nil, timing, errors.New("freeze: AgentName is required")
	}
	if req.PodName == "" || req.Namespace == "" {
		return nil, timing, errors.New("freeze: PodName and Namespace are required")
	}

	// Re-freeze guard (ka-0000001878): refuse to re-checkpoint an agent whose
	// snapshot is already CONSUMED by a prior freeze→thaw cycle. A path-b
	// checkpoint is node-local and single-shot — a second checkpoint would reuse
	// the deterministic per-agent paths /var/lib/gardenio/{ckpt,bundle}/<agent>,
	// whose bundle now backs the LIVE thawed Pod B (only the 3.0G image was
	// reaped at thaw; the bundle is the live rootfs-overlay lowerdir + /work
	// source), so re-freezing would clobber the live overlay and persist a stale
	// record. Multi-cycle re-freeze needs the snapshot-transport follow-up
	// (criu-image-streamer), explicitly out of scope here. This guard runs BEFORE
	// registerInflight so a refused re-freeze takes no side effects (no inflight
	// slot, no node exec). It is checked ahead of the duplicate-Freeze guard
	// below because a consumed snapshot is refused regardless of Pod A's liveness
	// — the live thawed pod is a perfectly healthy, NON-terminating pod, so the
	// podGoneOrTerminating gate would never catch it. Discard the now-un-thawable
	// record (its image is gone) so the next spawn cold-starts directly instead of
	// attempting a doomed thaw; treated as a deliberate skip by the caller, not a
	// failure (it falls through to the normal teardown → clean cold start).
	if existing, ok := f.existingSnapshot(ctx, req.AgentName); ok && existing.Consumed {
		f.logger.Warn("freeze: refusing re-freeze of a thawed agent — snapshot already consumed by a prior freeze→thaw cycle; discarding stale record and degrading to cold start (path-b is single-shot)",
			"agent", req.AgentName, "pod", req.PodName, "namespace", req.Namespace,
			"consumedSnapshotNode", existing.Node, "consumedAt", existing.CreatedAt)
		if delErr := f.snaps.Delete(ctx, req.AgentName); delErr != nil {
			f.logger.Warn("freeze: failed to discard consumed snapshot (next spawn would attempt a doomed thaw against the reaped image until this clears)",
				"agent", req.AgentName, "error", delErr)
		}
		return existing, timing, ErrFreezeAlreadyThawed
	}

	// Register the in-flight freeze and run the node exec under a derived ctx an
	// AbortFreeze can cancel (no-op when InterruptEnabled is off). The defer
	// resolves the outcome: if an abort fired at ANY point — including
	// microseconds after a successful Put — discard the (partial or just-
	// persisted) snapshot, reap its image, and return ErrFreezeAborted, leaving
	// Pod A live. This is the only place the abort verdict is read post-exec, so
	// the TOCTOU with completion is closed here, under registerInflight's lock.
	// resolvedNode is captured by the abort defer so a mid-checkpoint abort can
	// reap the partial image at freezeImagePath on the RIGHT node. It is assigned
	// once node resolution succeeds below; while still empty (abort before node
	// resolution) no checkpoint ran, so there is nothing to reap.
	var resolvedNode string
	cctx, finalize, tracked := f.registerInflight(ctx, req.AgentName)
	if tracked {
		defer func() {
			if aborted := finalize(); aborted {
				f.discardAbortedFreeze(req.AgentName, resolvedNode, snap)
				snap, err = nil, ErrFreezeAborted
			}
		}()
	}

	pod, gerr := f.client.CoreV1().Pods(req.Namespace).Get(cctx, req.PodName, metav1.GetOptions{})
	// Duplicate-Freeze idempotency guard (defense-in-depth for ka-0000000194).
	// If Pod A is gone or already Terminating AND we already hold a persisted
	// snapshot for this agent, a second checkpoint can only capture a degenerate
	// image of a dying pod (empty agent overlay-opts) that would OVERWRITE the
	// good snapshot and make the bundle un-thawable. Refuse: return the existing
	// good snapshot and ErrFreezeAlreadySnapshotted (treated as a deliberate skip
	// by the caller, not a failure). This holds even if the watcher-side dedup
	// (subscriber.SSEWatcher.markStop, fix-A) is bypassed — e.g. across a watcher
	// reconnect that resets its in-memory marker.
	if existing, ok := f.existingSnapshot(ctx, req.AgentName); ok {
		if podGoneOrTerminating(pod, gerr) {
			f.logger.Warn("freeze: refusing duplicate checkpoint — snapshot exists and source pod is gone/Terminating; keeping good snapshot",
				"agent", req.AgentName, "pod", req.PodName, "namespace", req.Namespace,
				"existingSandboxCID", existing.SandboxCID, "existingContainerCount", existing.ContainerCount)
			return existing, timing, ErrFreezeAlreadySnapshotted
		}
	}
	if gerr != nil {
		return nil, timing, fmt.Errorf("freeze: get pod %s/%s: %w", req.Namespace, req.PodName, gerr)
	}
	node := req.Node
	if node == "" {
		node = pod.Spec.NodeName
	}
	if node == "" {
		return nil, timing, fmt.Errorf("freeze: pod %s/%s has no assigned node", req.Namespace, req.PodName)
	}
	// Surface the resolved node to the abort defer so a mid-checkpoint abort reaps
	// the partial image on this node (ka-0000004367).
	resolvedNode = node

	// Pre-freeze liveness guard (ka-0000006667, opt-in via ArmedGuardEnabled):
	// refuse to checkpoint a pod whose in-pod rehydration watchers never armed. A
	// runsc pod becomes health-ready long before its agent loop (coop + gb) boots
	// and arms its thaw watchers; a snapshot taken in that window has no live
	// watcher inside it, so its thaw can never be acked (the rehydration gate
	// fail-closes to a cold start). Skipping the freeze here yields the SAME cold
	// start on next spawn — minus a wasted 3.0G checkpoint and a guaranteed-doomed
	// thaw. Fail-safe: if the armed check itself errors, watchersArmed returns
	// true so we proceed with the freeze (today's behaviour) — a flaky node probe
	// never silently disables checkpointing.
	//
	// Direction [A] (ArmedWait > 0): rather than refuse the instant the
	// sentinels are absent, WAIT a bounded window for the watchers to arm before
	// deciding. A graceful stop frequently arrives before a slow-booting agent
	// loop has armed; the single-shot probe would then cold-start every such
	// agent. Waiting out the window gates the freeze on watcher liveness — proof
	// the gb/coop watchers are live — so the snapshot we mint is thawable and the
	// post-thaw rehydration ack is deterministic, instead of racing the
	// checkpoint against the agent boot. If the watchers never arm within the
	// window, we fall back to the same refusal (un-thawable snapshot avoided).
	if f.ArmedGuardEnabled {
		if armed, why := f.watchersArmedWithin(cctx, node, req.AgentName, f.ArmedWait); !armed {
			f.logger.Warn("freeze: in-pod rehydration watchers not armed; skipping checkpoint (agent will cold-start on next spawn)",
				"agent", req.AgentName, "pod", req.PodName, "namespace", req.Namespace, "node", node, "armedWait", f.ArmedWait, "reason", why)
			return nil, timing, ErrFreezeWatchersNotArmed
		}
	}

	// Pin the image + bundle to the controller-owned safe paths the snapshot
	// records, so the freeze script writes the 3.0G checkpoint image exactly
	// where thaw reads it (ka-0000003702). Before this, Freeze omitted these and
	// the script fell back to the literal "$IMG"/"$BUNDLE" placeholders, writing
	// the image to a path that did NOT match snap.ImagePath — a guaranteed
	// thaw-time ENOENT on top of the GC vector. Now both sides address one path.
	imagePath := freezeImagePath(req.AgentName)
	bundlePath := freezeBundlePath(req.AgentName)
	script := f.buildFreezeScript(freezeScriptParams{
		PodName:      req.PodName,
		RunscTimeout: int(f.runscTimeout().Seconds()),
		ImagePath:    imagePath,
		BundlePath:   bundlePath,
	})

	f.logger.Info("freeze: checkpointing agent pod",
		"agent", req.AgentName, "pod", req.PodName, "namespace", req.Namespace, "node", node)

	// FREEZE HARD DEADLINE (Fix C, bug ka-0000004931): bound the whole node exec
	// with an explicit wall-clock cap, mirroring the thaw side's thawHardDeadline.
	// The inner RUNSC_TIMEOUT bounds each runsc call and Fix A's HMNTT bounds the
	// read-only image probes, but the MUTATING host-ns copies (the Step-3
	// relocation cp, the pre-checkpoint gofer/rootfs copies) are DELIBERATELY left
	// unbounded on the node — bounding a mutating copy risks a partial-copy
	// un-thawable snapshot. This cap is the disease fix for that class: a wedge in
	// ANY unbounded node op makes the exec ctx time out, so Freeze() returns an
	// error here (no snapshot persisted, Pod A left live, next spawn cold-starts)
	// instead of hanging the freeze goroutine forever. Derived from cctx (NOT
	// context.WithoutCancel) so an in-flight AbortFreeze still cancels the exec —
	// unlike the thaw path, freeze has no committed Pod B to protect from inbound
	// cancellation, so the abort seam must keep reaching the exec. A deadline-fire
	// surfaces as a ctx error through the same path as any exec failure; the
	// finalize defer reads entry.aborted (false on a pure timeout) and returns the
	// wrapped error, so the caller cold-starts — the correct fail-closed outcome.
	//
	// Size the cap to the pod's container count (ka-0000007927): the relocation +
	// gofer copies are PER-container work, so a multi-sidecar agent's copy budget
	// scales above a bare agent's. The authoritative FT_CONTAINER_COUNT is only
	// baked into the statefile the exec produces (parsed below), so estimate it
	// pre-exec from the live Pod A spec (pause + app containers + always-on
	// sidecars). The estimate only sizes the SAFETY cap — a wrong guess never
	// changes correctness, only how much slack a wedged copy gets before fail-close.
	freezeContainers := estimateFreezeContainerCount(pod.Spec)
	freezeDeadline := f.freezeHardDeadlineFor(freezeContainers)
	f.logger.Info("freeze: hard-deadline sized to pod shape",
		"agent", req.AgentName, "estimatedContainerCount", freezeContainers,
		"freezeHardDeadline", freezeDeadline, "operatorOverride", f.FreezeHardDeadline > 0)
	fctx, fcancel := context.WithTimeout(cctx, freezeDeadline)
	defer fcancel()
	res, xerr := f.exec.Exec(fctx, node, script)
	if xerr != nil {
		// An abort cancels cctx (=> fctx), and the freeze hard deadline cancels
		// fctx; either surfaces here as a ctx error. The defer maps an abort to
		// ErrFreezeAborted (agent left live); a hard-deadline timeout (aborted=false)
		// or a genuine exec failure is returned as-is and the caller cold-starts
		// (today's safe fallback) — no snapshot is persisted on any of these paths.
		return nil, timing, fmt.Errorf("freeze: node exec on %s: %w", node, xerr)
	}
	fields := parseSentinels(res.Stdout)

	// Node-side step attribution (task ka-0000005147). The freeze script brackets
	// the runsc page-dump (FT_CHECKPOINT_MS) and the Step-3 relocation copy
	// (FT_RELOCATION_MS) with a millisecond clock. Parse them best-effort: a miss
	// leaves the sub-timer zero (the total still records) — these feed the
	// freeze_latency histogram's per-step breakdown, never a control-flow gate.
	if ms, ok := intField(fields, keyCheckpointMS); ok && ms >= 0 {
		timing.Checkpoint = time.Duration(ms) * time.Millisecond
	}
	if ms, ok := intField(fields, keyRelocationMS); ok && ms >= 0 {
		timing.Relocation = time.Duration(ms) * time.Millisecond
	}

	// Pause-window atomicity attribution (task ka-0000006783). The freeze script
	// opens a `runsc pause` consistent-snapshot window (ka-0000004656) before the
	// gofer warm copies + checkpoint, and emits FT_PAUSE_RC. A non-zero rc means
	// the pause FAILED and the script DEGRADED to the pre-fix non-atomic copy
	// (warm copy taken UNPAUSED, then checkpoint) — the snapshot is still
	// published and a quiescent agent thaws clean, but a churny one keeps its old
	// rc=128 TOCTOU risk. This is DELIBERATELY log-only, never a gate: a lost
	// freeze is strictly worse than a degraded-but-usable one, and the review that
	// filed this fix explicitly flagged not gating on it. Parse best-effort: the
	// value is threaded into the post-state persisted log so operators can tell a
	// cleanly-paused freeze (pauseRC=0, atomic) from a degraded one (pauseRC!=0).
	// A missing sentinel (older node script) yields pauseRC=-1 / pauseAtomic=false
	// so "unknown" never masquerades as "clean".
	pauseRC, pauseRCKnown := intField(fields, keyPauseRC)
	if !pauseRCKnown {
		pauseRC = -1
	}
	pauseAtomic := pauseRCKnown && pauseRC == 0
	if pauseRCKnown && pauseRC != 0 {
		f.logger.Warn("freeze: consistent-snapshot pause degraded — payload copy ran UNPAUSED (non-atomic); snapshot published but a churny agent keeps rc=128 thaw risk",
			"agent", req.AgentName, "pod", req.PodName, "namespace", req.Namespace,
			"node", node, "pauseRC", pauseRC)
	}

	// Payload-completeness attribution (task ka-0000000276). The freeze script now
	// emits the rc of each warm-copy step: FT_WORK_CP_RC / FT_TMP_CP_RC for the
	// /work + /tmp cp, and FT_GOFER_FAILED (a count) for the data-driven .gofer
	// sweep. Pre-fix these copies swallowed a mid-copy ENOSPC/EIO and published an
	// INCOMPLETE-payload snapshot with no signal; the mount fails closed at thaw
	// (cold-start on the missing warm data), so this is an integrity / wasted-freeze
	// issue, never an availability one. Parse best-effort (a miss => older node
	// script => "unknown", never a false clean) and WARN so the wasted freeze is
	// visible in canary + fleet telemetry. DELIBERATELY log-only, never a gate: a
	// non-zero cp rc is dominated by benign dangling symlinks in a real /work repo
	// checkout (cp -aL dereferences links), so gating would spuriously cold-start
	// thawable agents fleet-wide — the opposite of hardening — and the mount already
	// fails closed at thaw. Same not-gating stance as the FT_PAUSE_RC sibling above.
	workCPRC, workCPKnown := intField(fields, keyWorkCPRC)
	tmpCPRC, tmpCPKnown := intField(fields, keyTmpCPRC)
	goferFailed, goferKnown := intField(fields, keyGoferFailed)
	// Payload-completeness DE-NOISE (ka-0000006933). The node script now classifies
	// each non-zero cp rc via FT_WORK_CP_REASON / FT_TMP_CP_REASON so the benign
	// dangling-symlink noise (cp -aL dereferences links, so a real /work repo
	// checkout routinely trips a non-zero rc with zero data loss) no longer flags
	// payloadComplete=false. classifyCopyLeg returns the per-leg verdict:
	//   failed=true  => a GENUINE truncating failure (reason==copyfail, OR — older
	//                   node script with no reason — the pre-fix rc-based rule:
	//                   rcKnown && rc!=0). This is the incomplete-payload signal.
	//   symlinkSkip=true => reason==symlink: the rc was tripped ONLY by a benign
	//                   dangling symlink; the payload is COMPLETE (the de-noised
	//                   case). Quantified separately, excluded from the alert metric.
	// A missing reason on an rc!=0 leg falls back to failed=true (today's behaviour,
	// safe-but-noisy) so old-node/new-Go never regresses to a false-clean; a
	// present-but-unrecognized reason (SIGKILL truncation) on an rc!=0 leg is also
	// failed=true (M6 — never downgrade a lost freeze to complete).
	workFailed, workSymlinkSkip := classifyCopyLeg(workCPRC, workCPKnown, fields[keyWorkCPReason])
	tmpFailed, tmpSymlinkSkip := classifyCopyLeg(tmpCPRC, tmpCPKnown, fields[keyTmpCPReason])
	// FT_GOFER_FAILED now retains its intField known bool, mirroring the workCPKnown/
	// tmpCPKnown handling above (ka-0000000735). intField reports known=false for BOTH
	// a MISSING sentinel (older node script => clean/unknown) and a PRESENT-but-
	// unparseable one (e.g. a python traceback line bleeding into the value, or a
	// non-integer count). Those mean different things: a miss is clean, but a corrupt-
	// but-present sentinel must NOT silently collapse to goferFailed==0 and suppress the
	// wasted-freeze WARN. Disambiguate on the raw sentinel value: a NON-EMPTY value that
	// intField could not parse is malformed. An empty value ("FT_GOFER_FAILED=") is
	// treated as unknown/clean, identical to the sibling workCPRC/tmpCPRC empty semantics,
	// so only a genuinely corrupt count trips the parse-error path. A malformed count is
	// counted as incomplete for telemetry; a genuine non-zero count (goferKnown &&
	// goferFailed != 0) is the pre-existing incomplete-payload case.
	goferMalformed := fields[keyGoferFailed] != "" && !goferKnown
	// warmCopyFailed is the "a warm-copy step suffered a GENUINE failure" verdict. Post
	// de-noise (ka-0000006933) the /work + /tmp legs count as failed ONLY when their
	// reason classified a real truncation (workFailed/tmpFailed) — a benign dangling-
	// symlink-only rc (workSymlinkSkip/tmpSymlinkSkip) is NOT a failure, the payload is
	// complete. The .gofer sweep term is unchanged: its copytree already excludes
	// dangling-symlink skips + CoW->copy2 fallbacks (ignore_dangling_symlinks=True), so
	// a non-zero FT_GOFER_FAILED is already a clean genuine-failure count keyed on
	// goferFailed (never a reason). warmCopyFailed excludes the malformed-sentinel case
	// (goferFailed is 0 when unparseable), which is surfaced separately below so a
	// corrupt count is never misreported as goferFailed=0.
	warmCopyFailed := workFailed || tmpFailed || (goferKnown && goferFailed != 0)
	payloadComplete := !warmCopyFailed && !goferMalformed
	// Payload verdict for caller-side metric recording (ka-0000006933 finding #4). The
	// caller reads timing.Payload from the freeze-success branch and increments the
	// gardenio_freeze_payload_* counters — done caller-side (not here) so a snapshot the
	// abort-discard defer later discards is never counted (M3).
	timing.Payload = PayloadVerdict{
		WorkCopyFailed:  workFailed,
		TmpCopyFailed:   tmpFailed,
		WorkSymlinkSkip: workSymlinkSkip,
		TmpSymlinkSkip:  tmpSymlinkSkip,
		GoferFailed:     goferKnown && goferFailed != 0,
		GoferMalformed:  goferMalformed,
	}
	if goferMalformed {
		// The completeness telemetry itself is corrupt, so we cannot tell whether the
		// .gofer sweep failed — surface a dedicated WARN (kept distinct from the copy-
		// failure WARN below so a corrupt sentinel is not misreported as an observed
		// warm-copy failure) and count it as incomplete. LOG-ONLY, never a gate: the
		// affected mount still fails closed at thaw, exactly like the sibling fields.
		f.logger.Warn("freeze: FT_GOFER_FAILED sentinel present but unparseable — corrupt payload-completeness telemetry; snapshot published, marking payload incomplete so the wasted-freeze signal is not silently suppressed",
			"agent", req.AgentName, "pod", req.PodName, "namespace", req.Namespace,
			"node", node, "rawGoferFailed", fields[keyGoferFailed])
	}
	if warmCopyFailed {
		f.logger.Warn("freeze: workspace payload copy INCOMPLETE — a warm-copy step failed (ENOSPC/EIO or a dereferenced dangling symlink); snapshot published but the affected mount(s) will cold-start at thaw on the missing warm data",
			"agent", req.AgentName, "pod", req.PodName, "namespace", req.Namespace,
			"node", node, "workCPRC", workCPRC, "tmpCPRC", tmpCPRC, "goferFailed", goferFailed,
			// Reasons thread the de-noise classification into the WARN so an operator sees
			// WHY a leg was flagged (copyfail) vs the benign symlink noise that no longer
			// flags. Empty when the older node script emitted no reason (rc-rule fallback).
			"workCPReason", fields[keyWorkCPReason], "tmpCPReason", fields[keyTmpCPReason])
	}

	// Post-state assertions (epic constraint: runsc C/R can hang / partially
	// fail; never trust a snapshot we did not positively verify).
	if res.ExitCode != 0 {
		return nil, timing, fmt.Errorf("freeze: node script exited %d: %s", res.ExitCode, tail(res.Stderr, 400))
	}
	if rc, ok := intField(fields, keyCheckpointRC); !ok || rc != 0 {
		return nil, timing, fmt.Errorf("freeze: runsc checkpoint rc=%s (sandbox=%s): %s",
			fields[keyCheckpointRC], fields[keySandboxCID], tail(res.Stderr, 400))
	}
	if fields[keyImagePresent] != "yes" {
		return nil, timing, fmt.Errorf("freeze: checkpoint image absent after runsc checkpoint (sandbox=%s)", fields[keySandboxCID])
	}
	if st := fields[keySandboxState]; st != "running" {
		// --leave-running must keep Pod A alive; anything else means the
		// checkpoint killed the live pod and the freeze is not safe to keep.
		return nil, timing, fmt.Errorf("freeze: sandbox not running after --leave-running checkpoint (state=%q)", st)
	}
	count, ok := intField(fields, keyContainerCount)
	if !ok || count < 1 {
		return nil, timing, fmt.Errorf("freeze: container_count not baked into statefile (got %q)", fields[keyContainerCount])
	}
	// Bundle-completeness guard (ka-0000006614): the node script asserts that
	// both {sandbox,agent}/config.json and .overlay-opts exist and are non-empty
	// in the preserved OCI bundle. If they do not, the snapshot is structurally
	// un-thawable — thaw reads snap.BundlePath directly and `runsc restore`
	// FetchSpec-ENOENTs (rc=128). FAIL the freeze here so NO snapshot is
	// persisted and the controller cold-starts cleanly instead of recording a
	// success it can never thaw.
	if fields[keyBundleOK] != "yes" {
		return nil, timing, fmt.Errorf("freeze: incomplete OCI bundle (FT_BUNDLE_OK=%q) — sandbox/agent config.json or overlay-opts missing; refusing to persist un-thawable snapshot: %s",
			fields[keyBundleOK], tail(res.Stderr, 400))
	}
	// Image-relocation guard (ka-0000003702, fix A+C). The 3.0G runsc checkpoint
	// image is too large to carry in the snapshot blob (the fix-B shape used for
	// the small spec files), so thaw still reads it from a node-local path. The
	// remaining rc=128 was that path (/var/tmp/gb-ckpt) being reaped by Pod A's
	// post-freeze teardown, identically to the original bundle deletion vector.
	// The freeze script now writes the image to a CONTROLLER-OWNED safe dir
	// (/var/lib/gardenio/ckpt, outside the containerd / pod-lifecycle GC zone) and
	// re-verifies it host-namespaced (HMNT test -s checkpoint.img) AT the safe
	// path, emitting FT_IMAGE_OK. Fail closed exactly like the bundle guard: a
	// snapshot whose image is absent / empty / un-relocated is un-thawable, so
	// refuse to persist it and let the controller cold-start cleanly. Because the
	// snapshot is the gate the lifecycle's DeleteAgentPod (Pod A teardown) waits
	// on, a missing FT_IMAGE_OK means Pod A is NOT torn down behind an image we
	// could never restore from.
	if fields[keyImageOK] != "yes" {
		return nil, timing, fmt.Errorf("freeze: checkpoint image not safely relocated (FT_IMAGE_OK=%q) — %s/checkpoint.img absent or empty at the controller-owned safe path after copy; refusing to persist un-thawable snapshot (Pod A NOT torn down behind a reaped image): %s",
			fields[keyImageOK], imagePath, tail(res.Stderr, 400))
	}

	// Decode the sub-container roster (ka-0000003561): the agent plus any
	// always-on native sidecars, each by CRI container-name + A-side cid. The
	// node script emits one FT_SUBC line per running sub-container; the barrier
	// restore drives one `runsc restore` per entry on top of the sandbox root.
	subs, subErr := decodeSubContainers(parseSentinelList(res.Stdout, keySubContainer))
	if subErr != nil {
		return nil, timing, fmt.Errorf("freeze: sub-container roster capture failed: %w: %s", subErr, tail(res.Stderr, 400))
	}
	// Fail-closed count cross-check: the statefile barrier target must equal
	// sandbox(1) + every running sub-container. A mismatch means the roster and
	// the checkpoint disagree on the pod shape — thaw would drive the wrong
	// number of restores and hang at the barrier, so refuse the snapshot.
	if count != len(subs)+1 {
		return nil, timing, fmt.Errorf("freeze: container_count=%d but captured %d sub-containers (+sandbox=%d) — roster/checkpoint shape mismatch; refusing un-thawable snapshot", count, len(subs), len(subs)+1)
	}

	// Capture the spec files into the snapshot (fix B, ka-0000000301; N-container
	// generalization ka-0000003561). These were base64-emitted by [3c/8] from the
	// host bundle that FT_BUNDLE_OK just proved complete — one config.json +
	// .overlay-opts per container subdir (sandbox + each sub-container). Thaw
	// re-materializes them, so the persisted snapshot — not the GC'able host
	// bundle dir — is the source of truth for the OCI spec. Fail closed: a
	// snapshot missing any spec file is un-thawable, so refuse to persist it.
	specFiles, specErr := decodeSpecFiles(parseSentinelList(res.Stdout, keySpecFile))
	if specErr != nil {
		return nil, timing, fmt.Errorf("freeze: spec-file capture failed (fix B): %w: %s", specErr, tail(res.Stderr, 400))
	}
	if cErr := assertBundleComplete(specFiles, subs); cErr != nil {
		return nil, timing, fmt.Errorf("freeze: %w — refusing un-thawable snapshot: %s", cErr, tail(res.Stderr, 400))
	}

	pagesBytes, _ := int64Field(fields, keyPagesBytes)
	built := Snapshot{
		AgentName:      req.AgentName,
		BeadID:         req.BeadID,
		Namespace:      req.Namespace,
		PodName:        req.PodName,
		Node:           node,
		ImagePath:      imagePath,
		BundlePath:     bundlePath,
		SandboxCID:     fields[keySandboxCID],
		AgentCID:       fields[keyAgentCID],
		PodUID:         fields[keyPodUID],
		SubContainers:  subs,
		ContainerCount: count,
		// PagesBytes best-effort: a zero value (freeze script older than
		// ka-0000000088, sentinel missing, stat failed) signals "unknown" — the
		// pre-thaw memory gate falls back to a node-side stat and the warm-B
		// placeholder request falls back to the 256Mi admission floor.
		PagesBytes: pagesBytes,
		// Self-describe the rootfs-overlay medium (overlay-dir-medium, ka-0000007239).
		// Best-effort: empty ⇒ default self medium ⇒ thaw adds no --overlay2 flag.
		OverlayMedium: fields[keyOverlayMedium],
		PodTemplate:   sanitizePodTemplate(pod.Spec),
		SpecFiles:     specFiles,
		CreatedAt:     f.now().UTC(),
	}
	if built.SandboxCID == "" || built.AgentCID == "" {
		return nil, timing, fmt.Errorf("freeze: could not resolve sandbox/agent cids (sandbox=%q agent=%q)", built.SandboxCID, built.AgentCID)
	}

	// Push the heavy artifacts (checkpoint image + OCI bundle + rootfs lowerdir)
	// over the SnapshotTransport seam BEFORE persisting the metadata, so the
	// record only ever points at a payload that has already been made durable /
	// reachable (Δ5-B1). The default node-local transport is a no-op — the
	// artifacts already sit on snap.Node where the thaw restores them — so this is
	// behaviour-neutral until an off-node transport (B2) is installed. A Push
	// failure fails the freeze closed: no metadata is persisted and the caller
	// cold-starts, exactly like the un-thawable-snapshot guards above, rather than
	// recording a snapshot whose payload never arrived. Pushed under the parent
	// ctx (not cctx) for the same reason the Put below is: the checkpoint already
	// succeeded, so an inbound abort must not tear the transport mid-upload.
	if terr := f.transportOrDefault().Push(ctx, built); terr != nil {
		return nil, timing, fmt.Errorf("freeze: transport push snapshot artifacts for %s: %w", req.AgentName, terr)
	}

	persistStart := f.now()
	// Persist under the parent ctx, not cctx: once the checkpoint succeeded the
	// snapshot is worth recording even if an abort is landing — the defer reads
	// the abort verdict AFTER this returns and lazily discards the snapshot, so a
	// cctx-cancelled Put would only create a confusing half-persist mid-race.
	if perr := f.snaps.Put(ctx, built); perr != nil {
		return nil, timing, fmt.Errorf("freeze: persist snapshot for %s: %w", req.AgentName, perr)
	}
	timing.Persist = f.now().Sub(persistStart)
	// Rootfs-relocation reflink-engagement verdict (ka-0000007174). The freeze script
	// emits per-container FT_RELOC_REFLINK_HITS_<dstn>/FALLBACKS_<dstn> under the dir
	// medium; sum across containers so the persisted log carries a single fleet-visible
	// signal. relocReflinkKnown is false for a self-medium freeze or an older node
	// script (neither emits the counters) — distinct from a known all-fallbacks run.
	// Diagnostic-only, NEVER a gate: this surfaces the canary #13 signature
	// (reflinkHits==0 with a non-empty relocation stack == a full byte copy on the
	// freeze critical path) into fleet telemetry, but a full-copy relocation still
	// produced a correct snapshot — gating would trade a slow freeze for a lost one.
	relocReflinkHits, relocReflinkFallbacks, relocReflinkKnown := sumSuffixedSentinels(fields, keyRelocReflinkHits, keyRelocReflinkFallbacks)
	f.logger.Info("freeze: snapshot persisted",
		"agent", req.AgentName, "sandboxCID", built.SandboxCID, "agentCID", built.AgentCID,
		"containerCount", built.ContainerCount, "imageSize", fields[keyImageSize],
		"pagesBytes", built.PagesBytes, "node", node,
		"checkpointMs", timing.Checkpoint.Milliseconds(),
		"relocationMs", timing.Relocation.Milliseconds(),
		"persistMs", timing.Persist.Milliseconds(),
		// Rootfs-relocation reflink verdict (ka-0000007174): under the dir medium the
		// relocation reconstructs the GC-safe lower from the real bare-xfs layers so it
		// reflinks (O(metadata)) instead of full-copying the overlay merged mount.
		// relocReflinkKnown=false => self medium / older script (no counters emitted);
		// known with reflinkHits=0 & reflinkFallbacks>0 => the copy fell back to a full
		// byte copy (cross-device / non-CoW node) — the canary #13 signature.
		"relocReflinkKnown", relocReflinkKnown,
		"relocReflinkHits", relocReflinkHits,
		"relocReflinkFallbacks", relocReflinkFallbacks,
		// Consistent-snapshot pause verdict (task ka-0000006783): pauseAtomic=true
		// means the whole payload was captured inside the pause window (clean
		// freeze); false means the pause failed and the copy degraded to non-atomic
		// (pauseRC carries the runsc rc, or -1 when the node script emitted no
		// FT_PAUSE_RC). Purely diagnostic — the freeze is published either way.
		"pauseRC", pauseRC,
		"pauseAtomic", pauseAtomic,
		// Workspace-payload completeness verdict (task ka-0000000276):
		// payloadComplete=true means every warm copy (/work, /tmp, .gofer sweep)
		// finished; false means a copy step failed and the affected mount will
		// cold-start at thaw. goferFailed carries the count of .gofer mounts whose
		// copytree threw. Diagnostic — the freeze is published either way.
		"payloadComplete", payloadComplete,
		"goferFailed", goferFailed,
		// Per-leg cp rc + de-noise reason threaded into the PERSISTED log for parity
		// with pauseRC (finding #3, ka-0000006933): an operator querying only this line
		// can now attribute an incomplete payload to the /work vs /tmp leg (and tell a
		// benign symlink skip from a genuine copyfail) without joining the WARN. Empty
		// reason => older node script (rc-rule fallback). rc -1-style unknowns show as
		// the raw sentinel absence via workCPKnown/tmpCPKnown in the WARN, not here.
		"workCPRC", workCPRC, "tmpCPRC", tmpCPRC,
		"workCPReason", fields[keyWorkCPReason], "tmpCPReason", fields[keyTmpCPReason])
	// Rootfs-relocation NON-REFLINK WARN (WP7, ka-0000001867). Promote the exact
	// canary #13 signature — reflinkHits==0 with a NON-empty relocation stack — from a
	// value buried in the 12-field persisted Info line to its own WARN, so a freeze that
	// full-copied the multi-GB rootfs off the critical path (the failure this whole fix
	// exists to prevent) is not a silent soft-PASS in fleet telemetry. relocReflinkFallbacks>0
	// is the precise "non-empty stack" witness: the STACK-empty merged-cp fallback and the
	// self medium emit 0/0 (so no false WARN), and an older node script leaves relocReflinkKnown
	// false — so this fires ONLY when the dir-medium stack loop ran, reflinked ZERO segments,
	// and fell back to a full byte copy on every one. DELIBERATELY log-only, NEVER a gate
	// (same stance as the FT_PAUSE_RC / payload-completeness siblings above): an all-fallbacks
	// relocation still produced a CORRECT snapshot — it was just slow, and on a genuinely
	// non-CoW / cross-device node it is expected, not an error. Gating would trade a slow
	// freeze for a lost one. This is a DIAGNOSTIC promotion of visibility, not a behaviour
	// change: the timing truth remains FT_RELOCATION_MS and canary #14 judges on HITS AND a
	// small FT_RELOCATION_MS together (HITS>0 is a structural same-device+CoW inference, not a
	// per-file FICLONE tally — see keyRelocReflinkHits, so it is necessary-not-sufficient).
	if relocReflinkKnown && relocReflinkHits == 0 && relocReflinkFallbacks > 0 {
		f.logger.Warn("freeze: rootfs relocation did NOT reflink — every stacked layer full-copied off the freeze critical path (the canary #13 signature); snapshot published and correct but the relocation was slow (expected on a non-CoW/cross-device node, else investigate)",
			"agent", req.AgentName, "pod", req.PodName, "namespace", req.Namespace,
			"node", node, "relocReflinkHits", relocReflinkHits,
			"relocReflinkFallbacks", relocReflinkFallbacks,
			"relocationMs", timing.Relocation.Milliseconds())
	}
	// TOCTOU test seam (ka-0000000996): hold the freeze in the just-persisted
	// state for a bounded window so a live-substrate e2e can observe the
	// snapshot ConfigMap appear and fire an interrupt INSIDE the post-Put window
	// — the microsecond-wide race the discard path exists to close. Default 0 is
	// today's behaviour. The window honours cctx so a freeze ctx cancellation
	// (which is exactly what AbortFreeze does) ends the wait immediately rather
	// than burning the whole delay; the finalize defer then reads aborted=true
	// and discards the snapshot just as it would in the natural race.
	if f.InterruptEnabled && f.PostPutDelay > 0 {
		f.logger.Info("freeze: post-Put TOCTOU window open (test seam)",
			"agent", req.AgentName, "postPutDelay", f.PostPutDelay)
		t := time.NewTimer(f.PostPutDelay)
		select {
		case <-cctx.Done():
			t.Stop()
		case <-t.C:
		}
	}
	// Assign the named return so the abort defer can discard it if an abort raced
	// this completion (TOCTOU resolved under registerInflight's lock).
	snap = &built
	return snap, timing, nil
}

// armedConsumers is the set of in-pod rehydration consumers whose armed
// sentinel must be present before Freeze will checkpoint. It mirrors the
// rehydration gate's required set (NodeRehydrationTrigger.required): the exact
// consumers whose post-thaw ack the controller waits on. Overridable for tests.
func (f *FreezeThawManager) armedConsumers() []string {
	if len(f.RequiredArmedConsumers) > 0 {
		return f.RequiredArmedConsumers
	}
	return []string{thawepoch.ConsumerCoop, thawepoch.ConsumerGB}
}

// watchersArmedWithin gates the freeze on watcher liveness over a bounded
// window (ka-0000006667 direction [A]). With wait <= 0 it is exactly the
// single-shot watchersArmed probe (the Phase-4 / !3817 behaviour). With wait >
// 0 it polls the armed sentinels at armedPollInterval until every required
// consumer (coop + gb) has armed — returning (true, "") the moment they all are
// — or the window elapses, returning the last (false, reason) so the caller
// refuses with ErrFreezeWatchersNotArmed.
//
// Why wait instead of refuse-immediately: a graceful stop routinely arrives
// before a slow-booting agent loop has armed its thaw watchers. The single-shot
// probe cold-starts every such agent; waiting out the window lets the watchers
// come live so the snapshot is thawable and its post-thaw rehydration ack is
// deterministic. Fail-safe is preserved end to end: watchersArmed returns
// (true, reason) on any probe error, so a flaky node short-circuits the wait and
// proceeds with the freeze rather than burning the whole window before
// cold-starting.
//
// The wait honours ctx cancellation (the freeze's own bounded context): a
// cancelled/expired ctx ends the loop and returns the last observation, so the
// guard can never outlive the freeze budget.
func (f *FreezeThawManager) watchersArmedWithin(ctx context.Context, node, agentName string, wait time.Duration) (bool, string) {
	armed, why := f.watchersArmed(ctx, node, agentName)
	if armed || wait <= 0 {
		return armed, why
	}

	f.logger.Info("freeze: watchers not armed yet; waiting for arm before checkpoint (ka-0000006667 direction A)",
		"agent", agentName, "node", node, "armedWait", wait, "reason", why)

	deadline := f.now().Add(wait)
	tick := time.NewTicker(f.armedPollInterval())
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return false, fmt.Sprintf("context ended while waiting for watchers to arm: %v (last: %s)", ctx.Err(), why)
		case <-tick.C:
			if armed, why = f.watchersArmed(ctx, node, agentName); armed {
				f.logger.Info("freeze: watchers armed within wait window; proceeding with checkpoint",
					"agent", agentName, "node", node)
				return true, ""
			}
			if !f.now().Before(deadline) {
				return false, fmt.Sprintf("watchers still not armed after %s: %s", wait, why)
			}
		}
	}
}

// watchersArmed reports whether every required in-pod rehydration consumer has
// dropped its armed sentinel in the agent's node-side thaw-state dir
// (thawepoch.WriteArmed). It is the pre-freeze liveness guard for
// ka-0000006667: only a pod whose coop + gb thaw watchers are live can be
// checkpointed into a thawable snapshot, because only a live watcher in the
// restored image can ack the controller's post-thaw rehydration gate.
//
// The sentinels live on the same per-agent hostPath the rehydrate trigger reads
// (thawStateHostDir under ThawStateHostBase), so we probe them through the same
// NodeExecutor. Returns (true, "") when all sentinels are present. On any node
// exec / probe error it returns (true, reason): FAIL-SAFE — a flaky probe must
// never silently disable checkpointing, so we proceed with the freeze exactly
// as before the guard existed. It returns (false, reason) ONLY on a positive
// observation that a required sentinel is absent.
func (f *FreezeThawManager) watchersArmed(ctx context.Context, node, agentName string) (bool, string) {
	stateDir := thawStateHostDir(agentName)
	consumers := f.armedConsumers()

	// One probe lists which of the expected sentinels exist, so a single node
	// round-trip covers every consumer. Each present sentinel echoes a
	// FT_ARMED=<consumer> line; absent ones echo nothing.
	var b strings.Builder
	for _, c := range consumers {
		path := stateDir + "/" + thawepoch.ArmedName(c)
		fmt.Fprintf(&b, "if [ -f %s ]; then echo FT_ARMED=%s; fi\n", shellQuote(path), shellQuote(c))
	}
	res, err := f.exec.Exec(ctx, node, b.String())
	if err != nil {
		return true, fmt.Sprintf("armed probe exec failed (proceeding fail-safe): %v", err)
	}
	if res.ExitCode != 0 {
		return true, fmt.Sprintf("armed probe rc=%d (proceeding fail-safe): %s", res.ExitCode, tail(res.Stderr, 200))
	}

	var missing []string
	for _, c := range consumers {
		if !strings.Contains(res.Stdout, "FT_ARMED="+c) {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		return false, fmt.Sprintf("watchers not armed: %s (state dir %s)", strings.Join(missing, ", "), stateDir)
	}
	return true, ""
}

// discardAbortedFreeze undoes a freeze that an inbound-message interrupt aborted
// (C2). It covers BOTH abort shapes:
//
//   - Raced completion (persisted != nil): a snapshot was already persisted
//     before the abort verdict was read, so delete it from the store and reap
//     its now-orphaned checkpoint image — the controller must never advertise a
//     thaw target for an agent we just decided to keep live.
//   - Mid-checkpoint (persisted == nil): the abort cancelled the node exec
//     before the snapshot was persisted. The freeze script writes the runsc
//     checkpoint image DIRECTLY to freezeImagePath(agent) (the controller-owned
//     safe path /var/lib/gardenio/ckpt/<agent>, OUTSIDE any pod-lifecycle GC
//     zone), so a checkpoint that had started writing before the cancel can
//     leave a partial — and potentially multi-GB — image dir orphaned there.
//     Nothing else reaps it: it is not in a pod's GC scope, and it only self-
//     heals on the NEXT freeze of the same agent (the script wipes $IMG before
//     re-checkpointing). On the live gvisor substrate that orphan is exactly the
//     "NO orphaned checkpoint image left on the node" invariant the interruptible
//     -freeze e2e asserts (ka-0000004367 / ka-0000005834). So when we know the
//     node the checkpoint ran on, reap freezeImagePath(agent) directly. node is
//     empty only if the abort landed before node resolution (no checkpoint ran,
//     nothing to reap), so we skip the reap then.
//
// Either way Pod A is untouched (`--leave-running`). Best-effort throughout: a
// failed Delete/reap is a disk-space nuisance (a lazily-reapable orphan), never
// a correctness problem — the agent is live and the snapshot, if any, is gone
// from the store so no thaw will target it.
func (f *FreezeThawManager) discardAbortedFreeze(agentName, node string, persisted *Snapshot) {
	// Use a fresh, short ctx: the freeze ctx is cancelled (that is what aborted
	// us), so reusing it would fail the Delete/reap immediately.
	ctx, cancel := context.WithTimeout(context.Background(), f.runscTimeout())
	defer cancel()

	if persisted == nil {
		// Mid-checkpoint abort: no snapshot to delete, but a partial image may sit
		// at the controller-owned safe path. Reap it directly when we know the node.
		if node == "" {
			f.logger.Info("freeze: aborted before checkpoint started; no image to reap, agent left live",
				"agent", agentName)
			return
		}
		imagePath := freezeImagePath(agentName)
		if f.reapImageAt(ctx, node, imagePath) {
			f.logger.Info("freeze: aborted mid-checkpoint; reaped partial checkpoint image, agent left live",
				"agent", agentName, "node", node, "imagePath", imagePath)
		} else {
			f.logger.Warn("freeze: aborted mid-checkpoint; partial checkpoint image reap unconfirmed (lazily-reapable orphan; agent still live)",
				"agent", agentName, "node", node, "imagePath", imagePath)
		}
		return
	}
	if delErr := f.snaps.Delete(ctx, agentName); delErr != nil {
		f.logger.Warn("freeze: abort raced completion; failed to delete just-persisted snapshot (lazily-reapable orphan; agent still live)",
			"agent", agentName, "error", delErr)
	} else {
		f.logger.Info("freeze: abort raced completion; deleted just-persisted snapshot, agent left live",
			"agent", agentName)
	}
	// Reap the orphaned image best-effort (it was relocated to the safe path by
	// the completed checkpoint). reapImage logs its own outcome.
	f.reapImage(ctx, *persisted)
}
