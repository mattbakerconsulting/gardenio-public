package podmanager

// freezethaw_thaw.go — the THAW verb: barrier errors, Thaw/thawOnce, placeholder Pod B lifecycle and image reaping.
// Split out of freezethaw.go (gd-Wl6LuCnvlH); see freezethaw.go for the design overview.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
)

// --- THAW -------------------------------------------------------------------

// ThawBarrierError is the typed failure of the count-gated barrier restore: the
// node script ran to completion (exit 0) but a `runsc restore` reported a
// non-zero rc, so the barrier was never satisfied. It carries the per-container
// rcs the script emitted (FT_ROOT_RC / FT_SUB_RC) so the caller can both log the
// diagnostic asymmetry AND decide retryability without string-matching the
// wrapped message.
//
// The asymmetry is load-bearing for the gVisor thaw /sys flake (ka-0000005365):
// a transient agent-rootfs-overlay reconstruction miss (the "failed to walk
// \"sys\" in mount \"agent:/\"" class) fails the AGENT SUB restore (SubRC=128)
// while the pause sandbox — which has no 9p gofer tree to walk — restores clean
// (RootRC=0). That SubRC>0 / RootRC==0 shape is exactly the class a retry-once
// can plausibly clear (the freeze-side overlay collapse is the suspected race,
// per docs/gvisor-runsc-cr/single-container-resume.md). A RootRC>0 failure, by
// contrast, is the netns/lo-only / sandbox-level fail-closed path the thaw
// script deliberately drives (FT_ROOT_RC=128) and which a retry cannot fix, so
// it is NOT retryable.
type ThawBarrierError struct {
	RootRC int
	SubRC  int
	// Detail is the stderr tail the script emitted, preserved for the wrapped
	// error message so the controller log keeps today's diagnosis verbatim.
	Detail string
}

func (e *ThawBarrierError) Error() string {
	if e.SubRC != 0 {
		return fmt.Sprintf("thaw: agent sub-container restore rc=%d (barrier not satisfied): %s", e.SubRC, e.Detail)
	}
	return fmt.Sprintf("thaw: root sandbox restore rc=%d: %s", e.RootRC, e.Detail)
}

// Retryable reports whether this barrier failure is in the narrow class a
// retry-once-then-cold-start (ka-0000005365 step 4) may clear: the AGENT SUB
// restore failed (SubRC != 0) while the pause sandbox restored clean (RootRC ==
// 0). This is the suspected-transient agent-rootfs-overlay reconstruction race
// (the /sys-walk flake). A root-sandbox failure (RootRC != 0) is the
// deterministic netns/lo-only fail-closed path and is never retryable.
//
// EXCLUSION (ka-0000000088 / canary #12): SubRC == 137 is the kernel OOM-killer
// SIGKILL signature of an eager runsc restore that could not fit pages.img in
// available RAM on the pinned node. Retrying on the SAME node deterministically
// re-burns — the placeholder is node-pinned, the checkpoint image is
// node-local, MemAvailable did not increase between the first attempt and the
// retry — so a 137 falls straight through to the caller's cold-start fallback.
// The pre-thaw memory gate (thawOnce) is the load-bearing floor for this
// class; this exclusion is belt-and-braces for the race in which a snapshot
// with a stale PagesBytes (or a backfill landing between gate and restore)
// slips past the gate and OOMs at restore time anyway.
func (e *ThawBarrierError) Retryable() bool {
	if e.SubRC == 137 || e.RootRC == 137 {
		return false
	}
	return e.RootRC == 0 && e.SubRC != 0
}

// ThawRequest identifies the agent to restore. The snapshot (persisted at
// freeze) supplies the node, image path, and A-side ids.
type ThawRequest struct {
	AgentName string

	// PlaceholderName overrides the generated placeholder Pod B name. Empty
	// uses "<podName>-thaw-b". Ignored when PlaceholderPod is set (the supplied
	// pod's own name is authoritative).
	PlaceholderName string

	// PlaceholderPod, when set, names an ALREADY-READY placeholder Pod B (the
	// warm-B pool hot path, sub-second-resume-integration.md C1). The kubelet
	// has already provisioned that pod's CNI netns / emptyDir / projected token
	// ahead of the thaw, so Thaw grafts the checkpoint straight onto it and
	// SKIPS Step 1's Create + waitPodReady — the dominant multi-second cost.
	// B's uid is resolved from the supplied pod; its cids/netns are resolved on
	// the node by the barrier-restore script, identically to the create path.
	// When empty, today's create-then-wait behavior is preserved verbatim —
	// pure additive, zero blast radius when unused.
	PlaceholderPod string

	// SkipRehydration, when true, returns after the restore without firing the
	// rehydration hand-off (caller drives it). Default false.
	SkipRehydration bool
}

// ThawResult reports the outcome of a thaw: the placeholder Pod B, the restored
// sentry pid, and the netns proof (restored sandbox shares B's kubelet-owned
// netns inode).
type ThawResult struct {
	Snapshot       Snapshot
	PlaceholderPod string
	BSandboxCID    string
	BPodUID        string
	BNetns         string
	RestoredPID    string
	RestoredNetns  string
	Rehydrated     bool

	// ImageReaped reports whether the 3.0G checkpoint image at snap.ImagePath was
	// reclaimed after this verified thaw (ka-0000006968). Best-effort: false means
	// the image was left on the node (reap exec failed or did not confirm), which
	// is a disk-space nuisance, not a correctness problem — the thaw still
	// succeeded. The bundle is never reaped here (it backs the live Pod B).
	ImageReaped bool

	// RestorePeakBytes is the ACTUAL peak resident memory the restored container's
	// cgroup reached during the restore (cgroup-v2 memory.peak), read best-effort
	// after the restore verified (ka-0000003398 fix C). Zero means the node script
	// could not read it (legacy script / cgroup-v1 node / scope not resolvable) —
	// it is a MEASUREMENT hook, never a gate. Surfaced so the restore-peak
	// multiplier (modeledRestorePeakLimit / modeledRestoreNeed) can be calibrated
	// against real peaks per medium and sandbox role.
	//
	// TODO(ka-0000003398 fix C): aggregate RestorePeakBytes across mediums (self vs
	// dir) and sandbox roles (root vs sub) — e.g. via a metric or a bead comment on
	// each canary — and use the observed peak/pages ratios to replace the flat
	// defaultRestorePeakLimitMultiplierPermille (3.0×) with a calibrated per-role
	// term. Today the multiplier is derived from a single canary #15 data point.
	RestorePeakBytes int64
}

// Thaw restores a frozen multi-container agent pod through the k8s lifecycle,
// implementing the six path-b thaw steps proven in multi-container-resume.md +
// restore-through-lifecycle.md:
//
//  1. Create placeholder Pod B via the k8s API (kubelet re-provisions a fresh
//     CNI netns / emptyDir / projected token), pinned to the snapshot node;
//     wait Ready; resolve B's sandbox-cid / pod-uid / agent-cid / netns. When
//     req.PlaceholderPod names an already-Ready warm-B pool pod, this entire
//     create+wait step is skipped and the checkpoint is grafted straight onto
//     it (sub-second-resume-integration.md C1) — removing the dominant
//     multi-second kubelet pod-admission cost from the thaw hot path.
//  2. Repoint A-sandbox-cid→B and A-pod-uid→B across both specs; repoint the
//     network ns path to B's CNI netns (restore-into-b.py pattern).
//  3. Drive the COUNT-GATED BARRIER restore under nsenter -t 1 -m -p -n: launch
//     `runsc restore <root/sandbox-cid>` in the background (parks at
//     restoringUnstarted reading ContainerCount=N), wait for it to register the
//     cid, then fire `runsc restore <agent/sub-cid>` to satisfy the barrier and
//     unblock both.
//  4. Per-container rootfs overlay reconstruction in host-init's ns (capture
//     live overlay opts from /proc/mounts before delete; collapse {lower+upper}
//     into a new RO lowerdir; fresh upper/work; whiteout leftover
//     .gvisor.filestore.*).
//  5. Mount-dest normalization /var/run/*→/run/* so RestoreValidateSpec
//     destination sets match.
//  6. Trigger the rehydration hand-off (ka-0000001515), bounded by a deadline.
//
// Steps 2–5 run on the node in one barrier-restore script (the proven
// multi-container-resume-node.sh logic, parameterized + composed with the
// restore-into-b.py netns/uid repoint). Steps 1 and 6 are k8s-API / controller
// concerns. Every runsc call on the node is wrapped in `timeout`, and the
// restore is validated by Go-side post-state assertions before rehydration
// fires.
//
// DEDUP single-flight (design ka-0000005125): Thaw is a thin per-agent
// single-flight wrapper. The FIRST caller for an agent claims the in-flight
// entry and runs the restore (thawOnce); a SECOND concurrent caller for the
// same agent JOINS the first — it blocks until the owner finishes and returns
// the shared (*ThawResult, error) WITHOUT starting a second `runsc restore`.
// This makes "exactly one restore per agent per wake" an invariant of the
// manager, independent of how many spawn events / reconciler passes race
// upstream (two Pod Bs on the same node would kubelet-admit-fail, ka-0000005680).
// A joined caller MUST NOT also CreateAgentPod on success — it shares the
// owner's Pod B; on the owner's failure it shares the error and cold-starts
// once via the caller's existing fallback, same as a lone failed thaw.
func (f *FreezeThawManager) Thaw(ctx context.Context, req ThawRequest) (result *ThawResult, err error) {
	if req.AgentName == "" {
		return nil, errors.New("thaw: AgentName is required")
	}

	// Per-agent single-flight: claim the entry or join an in-flight thaw.
	f.thawSingleMu.Lock()
	if existing, ok := f.thawSingleflight[req.AgentName]; ok {
		// A thaw for this agent is already in flight — JOIN it rather than start
		// a second restore. Release the lock, wait for the owner to finish, and
		// return its shared result. ctx cancellation (e.g. a leader-change) lets
		// the joiner bail early — the owner's restore continues on its own
		// detached ctx and is unaffected.
		f.thawSingleMu.Unlock()
		f.logger.Info("thaw: joining in-flight thaw for agent (single-flight; no second restore)",
			"agent", req.AgentName)
		select {
		case <-existing.done:
			// Share the owner's outcome. On SUCCESS, return its (result, nil) so the
			// joiner's caller adopts the SAME Pod B and does not cold-start a
			// duplicate. On ANY error, return (nil, err): the joiner must NOT also
			// carry the result, because main.go's partial-success branch tears down
			// res.PlaceholderPod — and the OWNER's caller already owns that teardown,
			// so a joiner forwarding the same res would double-delete the shared Pod
			// B. Dropping res routes the joiner to the plain cold-start-on-error
			// fallback instead (design ka-0000005125: "failure → the same error, and
			// the caller cold-starts once").
			if existing.err != nil {
				return nil, existing.err
			}
			return existing.result, nil
		case <-ctx.Done():
			return nil, fmt.Errorf("thaw: joined in-flight thaw for %s but caller ctx cancelled before it completed: %w", req.AgentName, ctx.Err())
		}
	}
	// We are the owner: register the entry before releasing the lock so a racing
	// caller observes it and joins.
	entry := &inflightThaw{done: make(chan struct{})}
	f.thawSingleflight[req.AgentName] = entry
	f.thawSingleMu.Unlock()

	// Run the actual restore; publish the result on the entry and clear it from
	// the registry on return so the NEXT wake (a later spawn after this thaw
	// fully resolves) starts a fresh thaw rather than re-reading a stale result.
	defer func() {
		entry.result, entry.err = result, err
		f.thawSingleMu.Lock()
		delete(f.thawSingleflight, req.AgentName)
		f.thawSingleMu.Unlock()
		close(entry.done)
	}()

	return f.thawOnce(ctx, req)
}

// thawOnce is the actual restore body — see Thaw for the per-agent single-flight
// wrapper and the step-by-step lifecycle documentation. It must only be invoked
// through Thaw, which guarantees one in-flight thawOnce per agent.
func (f *FreezeThawManager) thawOnce(ctx context.Context, req ThawRequest) (result *ThawResult, err error) {
	// Mark this thaw in flight for the entire call — including the rehydration
	// hand-off below — so the reconciler's terminal-pod reaper (IsThawing gate)
	// stands off the restored placeholder Pod B while it rehydrates. Pod B goes
	// phase=Failed within seconds of the restore graft, and reaping it inside
	// the rehydration window tears the netns down under the in-pod watchers
	// (bug ka-0000002058, decision ka-0000005300: exempt_reaper).
	releaseThaw := f.registerThaw(req.AgentName)
	defer releaseThaw()
	snap, err := f.snaps.Get(ctx, req.AgentName)
	if err != nil {
		return nil, fmt.Errorf("thaw: load snapshot for %s: %w", req.AgentName, err)
	}

	// --- N-container barrier restore (ka-0000003561) -----------------------
	//
	// The node-side barrier restore now reconstructs the sandbox (pause) PLUS
	// every captured sub-container (the agent and any always-on DinD/Postgres
	// sidecars): buildThawScript fires the detached root sandbox restore (which
	// parks at restoringUnstarted reading ContainerCount=N) and then one
	// foreground `runsc restore` per sub-container to satisfy the count and
	// unblock the barrier — the proven gvisor-poc spike sequence (ka-0000000268).
	// The old C6 guard that REFUSED ContainerCount>2 (falling back to cold start
	// for sidecar agents) is therefore removed. We still fail closed on a snapshot
	// whose roster is internally inconsistent: a count>2 that carries no
	// sub-container roster is a legacy/partial snapshot the N-restore loop cannot
	// drive (the thaw script's empty-roster fallback restores only the agent, so a
	// >2 barrier would never satisfy and the restore would hang), so cold start
	// instead. A count<=2 snapshot with an empty roster is the legacy bare-agent
	// shape: the script defaults SUBNAMES to "agent" (sandbox + agent = 2), which
	// is correct, so let it proceed. ContainerCount<=0 is a pre-count legacy
	// snapshot — same agent-only fallback applies.
	if snap.ContainerCount > 2 && len(snap.SubContainers) == 0 {
		return nil, fmt.Errorf("thaw: snapshot for %s has container_count=%d but no sub-container roster — the N-container barrier restore cannot drive it; falling back to cold start (ka-0000003561)", req.AgentName, snap.ContainerCount)
	}

	// --- Step 0: teardown-complete barrier (ka-0000005680) -----------------
	//
	// Pod A and the placeholder Pod B are BOTH pinned to snap.Node and share the
	// per-agent thaw-state hostPath (ThawStateHostBase, RWO) and the runsc
	// sandbox/CID family. The ka-0000003702 "defer Pod-A teardown" change made
	// freeze() return only after the image+bundle relocation, so the lifecycle's
	// DeleteAgentPod (Pod A) now fires LATE — and a fast subsequent spawn can
	// reach Thaw while Pod A is still terminating. Creating Pod B on the same
	// node against a not-yet-released hostPath / sandbox makes kubelet admit-fail
	// Pod B IMMEDIATELY (terminal phase Failed in ~2s, upstream of any runsc
	// restore — the exact prod signature on 2026.157.15). Block until Pod A is
	// provably gone from the API before creating Pod B so the two never contend.
	// A nil snap.PodName or an already-absent Pod A returns immediately.
	if err := f.waitPodGone(ctx, snap.Namespace, snap.PodName); err != nil {
		return nil, fmt.Errorf("thaw: Pod A %s not fully torn down before Pod B creation: %w", snap.PodName, err)
	}

	// --- Pre-thaw memory-capacity gate (ka-0000000088) ---------------------
	//
	// Canary #12 (2026-07-02): warm-B graft thaw on a c6a.large node OOM-killed
	// the agent sub-container restore (SubRC=137, external SIGKILL from the
	// kernel OOM-killer). Root cause: `runsc restore` mmaps pages.img EAGERLY,
	// so a 3.0G snapshot demands ~3.0G resident within seconds. A c6a.large
	// (3.05Gi allocatable, ~1.0Gi baseline usage, ~660Mi backfill pod already
	// running before thaw) has ~2.1Gi free — the restore cannot fit and dies.
	// Retrying on the same node deterministically re-burns because the
	// placeholder is node-pinned to the snapshot's node and MemAvailable does
	// not meaningfully improve on the retry timescale.
	//
	// The gate probes the pinned node's MemAvailable + pages.img size BEFORE
	// creating Pod B (or accepting the supplied warm placeholder) and
	// fails-closed with the non-retryable *ThawInsufficientMemoryError so the
	// caller's cold-start fallback fires in milliseconds. Cheap: one HMNT read
	// of /proc/meminfo + one stat, no pod create, no runsc, no image load.
	//
	// PagesBytes source (belt-and-braces): prefer the snapshot's persisted
	// PagesBytes (freeze captured `stat -c %s` on pages.img at checkpoint
	// time); fall back to the node-side stat sentinel FT_PAGES_STAT_BYTES for
	// legacy snapshots (freeze pre-ka-0000000088). If both are zero, the gate
	// is INDETERMINATE (a snapshot on a node whose image path is unreadable)
	// and we PROCEED — better to let the barrier restore surface the real
	// error than to fail-close on a probe artifact. If MemAvailable is
	// unreadable, the gate fails-closed by design: unknown headroom is
	// treated as insufficient (a bug in /proc/meminfo access should not
	// silently degrade into "gate-off" and produce the very OOM we're
	// preventing).
	if perr := f.preThawMemoryGate(ctx, snap, req); perr != nil {
		f.logger.Warn("thaw: pre-thaw memory gate refused restore, cold-starting",
			"agent", req.AgentName, "node", snap.Node, "error", perr)
		return nil, perr
	}

	// --- Step 1: placeholder Pod B via the k8s API -------------------------
	//
	// Two paths share the rest of the thaw (repoint + barrier restore are
	// identical regardless of how Pod B came to be Ready):
	//
	//   * Warm-B graft (req.PlaceholderPod set): the named pod is ALREADY Ready
	//     — the kubelet provisioned its netns/volume/token ahead of the thaw, so
	//     we SKIP Create + waitPodReady (the dominant multi-second cost) and
	//     graft the checkpoint straight onto it (C1). We still Get the pod, both
	//     to resolve its kubelet UID and to fail loudly if the pool handed us a
	//     pod that is absent or no longer Ready.
	//   * Create-then-wait (req.PlaceholderPod empty): today's behavior verbatim
	//     — mint Pod B from the snapshot template, pin it to the node, wait Ready.
	var bName string
	var bUID string
	// Only the create path (req.PlaceholderPod empty) may tear down Pod B on failure:
	// a supplied warm placeholder is owned by the caller (freezeThawLifecycle promotes +
	// tears it down via DeletePromoted), so we must never delete it from here. The
	// create branch below defers createPathPodBTeardown the instant it commits to
	// minting Pod B; the warm-graft branch never does (ka-0000006072 / ka-0000005365).
	if req.PlaceholderPod != "" {
		bName = req.PlaceholderPod
		pod, gerr := f.client.CoreV1().Pods(snap.Namespace).Get(ctx, bName, metav1.GetOptions{})
		if gerr != nil {
			return nil, fmt.Errorf("thaw: supplied placeholder Pod B %s not found: %w", bName, gerr)
		}
		if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
			return nil, fmt.Errorf("thaw: supplied placeholder Pod B %s in terminal phase %s — not a usable graft target", bName, pod.Status.Phase)
		}
		if !podIsReady(pod) {
			return nil, fmt.Errorf("thaw: supplied placeholder Pod B %s is not Ready — the warm-B pool must only hand out Ready pods for the graft hot path", bName)
		}
		if pod.Spec.NodeName != snap.Node {
			return nil, fmt.Errorf("thaw: supplied placeholder Pod B %s is on node %q but the snapshot image is node-local to %q — a cross-node graft cannot reach the checkpoint", bName, pod.Spec.NodeName, snap.Node)
		}
		bUID = string(pod.UID)
		f.logger.Info("thaw: grafting checkpoint onto pre-Ready placeholder Pod B",
			"agent", req.AgentName, "podB", bName, "namespace", snap.Namespace, "node", snap.Node)
	} else {
		bName = req.PlaceholderName
		if bName == "" {
			bName = snap.PodName + "-thaw-b"
		}

		// Register the create-path teardown defer BEFORE Create + waitPodReady
		// (ka-0000006072). The defer must be armed the instant we commit to minting
		// Pod B, so a Ready-wait TIMEOUT return below (now bounded by the generous
		// createRestorePodReadyTimeout — up to 600s for a repo-heavy cold clone, vs
		// the old 180s) tears the pending create-path Pod B down instead of leaking
		// it to the reconciler reaper (map-order-nondeterministic orphan cleanup,
		// ka-0000005365). Previously this defer was registered only AFTER waitPodReady
		// returned Ready, so a Ready-wait failure returned upstream of it and left the
		// pending Pod B for the reaper — a leak window MR !4981 WIDENED from <=180s to
		// <=600s for a genuinely hung/slow clone. bName is set above; a Create that
		// never succeeded is harmless (Delete IsNotFound is ignored). The closure
		// captures the NAMED (result, err) returns via pointers, so it fires on
		// exactly the error-and-no-result exits (see createPathPodBTeardown).
		defer f.createPathPodBTeardown(ctx, &result, &err, req.AgentName, snap.Namespace, bName)()

		podB := f.buildPlaceholderPodB(snap, bName)
		f.logger.Info("thaw: creating placeholder Pod B",
			"agent", req.AgentName, "podB", bName, "namespace", snap.Namespace, "node", snap.Node)

		if _, err := f.client.CoreV1().Pods(snap.Namespace).Create(ctx, podB, metav1.CreateOptions{}); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return nil, fmt.Errorf("thaw: create placeholder Pod B %s: %w", bName, err)
			}
		}
		// The create-then-restore Pod B re-runs the cold init-clone to re-hydrate
		// the workspace volume (buildPlaceholderPodB), so its Ready-wait gets the
		// generous clone-aware budget — NOT the 180s podReadyTimeout that governs
		// no-clone paths — or a repo-heavy clone times out before the restore runs
		// (ka-0000004449).
		if err := f.waitPodReady(ctx, snap.Namespace, bName, f.createRestorePodReadyTimeout()); err != nil {
			// Pod B never reached Ready (the prod signature is a terminal phase
			// Failed in ~2s, before the runsc restore is ever attempted). The
			// gardenio-agent SA cannot read pods/events in the gVisor namespace, so
			// the controller — which runs with cluster-wide RBAC — captures Pod B's
			// container-level terminal cause and recent scheduling events HERE,
			// before the cold-start fallback discards Pod B. This makes the next
			// re-drive self-diagnosing without cluster-admin (ka-0000005680).
			f.logPodBFailureDiagnostics(ctx, req.AgentName, snap.Namespace, bName, snap.Node)
			return nil, fmt.Errorf("thaw: placeholder Pod B %s not Ready: %w", bName, err)
		}
		if got, gerr := f.client.CoreV1().Pods(snap.Namespace).Get(ctx, bName, metav1.GetOptions{}); gerr == nil {
			bUID = string(got.UID)
		}
	}

	// NOTE: the create-path Pod B teardown defer is now registered ABOVE, the instant
	// the create branch commits to minting Pod B — BEFORE Create + waitPodReady — so a
	// Ready-wait timeout also tears the pending Pod B down (ka-0000006072). See
	// createPathPodBTeardown.

	// --- Detach the post-commit critical section from the inbound ctx ------
	//
	// From here on Pod B is COMMITTED (created/Ready or the grafted warm
	// placeholder). The barrier restore, the rehydration hand-off, and the
	// post-restore reap must run to a real conclusion even if the inbound ctx is
	// cancelled mid-flight — which is exactly what a controller leader-change /
	// pod roll does (leader-election ctx with ReleaseOnCancel; the RWX-era e2e stack
	// rolled every ~6-8min on CI deploys). Before this, a roll cancelled the rehydrate
	// gate at ~6.8s with "context canceled" (the ka-0000004088 confound — we
	// never observed whether coop+gb would release within the 60s deadline) AND
	// cancelled the caller's Pod B teardown, leaking on-node runsc containers.
	//
	// dctx is decoupled from ctx's CANCELLATION via context.WithoutCancel but
	// bounded by its own hard deadline (thawHardDeadline) so a wedged node exec or
	// a watcher that never acks can never wedge the goroutine forever. The
	// shutdown drain (cmd/controller) is what actually bounds how long process
	// exit waits on this section; the hard deadline is the backstop cap. Steps 1
	// above (Pod A teardown barrier, Pod B create/waitReady) still honour the
	// inbound ctx, so a roll BEFORE any on-node restore work is a clean abort with
	// no committed Pod B to orphan.
	dctx, dcancel := context.WithTimeout(context.WithoutCancel(ctx), f.thawHardDeadline())
	defer dcancel()

	// Pull the heavy artifacts (checkpoint image + OCI bundle + rootfs lowerdir)
	// onto the target node over the SnapshotTransport seam BEFORE the restore runs
	// (Δ5-B1). Path-b thaw pins Pod B to snap.Node, so the target node is snap.Node
	// today and the default node-local transport is a no-op (the artifacts are
	// already there). An off-node transport (B2) re-materializes them at
	// snap.ImagePath / snap.BundlePath on the target node here, so the thaw script's
	// FT_IMAGE_OK image probe and the barrier restore find them exactly where the
	// node-local case does. A Pull failure fails the thaw closed and the caller
	// cold-starts — the same fail-safe as a reaped image. Run on dctx so it shares
	// the thaw hard-deadline budget and survives an inbound-ctx cancellation, like
	// the rest of this committed critical section.
	if terr := f.transportOrDefault().Pull(dctx, snap, snap.Node); terr != nil {
		return nil, fmt.Errorf("thaw: transport pull snapshot artifacts for %s onto node %s: %w", req.AgentName, snap.Node, terr)
	}

	// --- Steps 2–5: repoint + count-gated barrier restore on the node ------
	natsHost, natsPort := splitEgressTarget(f.NatsURL, "4222")
	beadsHost, beadsPort := splitEgressTarget(f.BeadsHTTPAddr, "8080")
	// Resolve the egress hosts to ClusterIP LITERALS for the [6c-2] in-sentry dial
	// (bug ka-0000002907): a link-only sentry cannot reach kube-dns, so dialing a name
	// gaierrors -> "inconclusive" flake-open (the #11 miss), while a ClusterIP is routed
	// at L3 so it no-routes EHOSTUNREACH -> link-only -> fail-closed. Best-effort: an
	// unresolvable host yields "" and the script SKIPS that dial (the [6c-1] route check
	// stays the floor). Resolved on dctx so it shares the thaw hard-deadline budget.
	natsDialAddr := f.resolveEgressDialAddr(dctx, natsHost)
	beadsDialAddr := f.resolveEgressDialAddr(dctx, beadsHost)
	script := f.buildThawScript(thawScriptParams{
		PlaceholderPod:     bName,
		BPodUID:            bUID,
		ASandboxCID:        snap.SandboxCID,
		AAgentCID:          snap.AgentCID,
		APodUID:            snap.PodUID,
		ImagePath:          snap.ImagePath,
		BundlePath:         snap.BundlePath,
		ContainerCount:     snap.ContainerCount,
		RunscTimeout:       int(f.runscTimeout().Seconds()),
		RestoreOverlayFlag: overlayRestoreFlag(snap.OverlayMedium),
		SpecFiles:          encodeThawSpecFiles(snap.SpecFiles),
		SubContainers:      encodeThawSubContainers(snap.SubContainers),
		// THAW-STATE SOURCE RE-POINT (fix, bug ka-0000000673 / ka-0000001104): the NEW
		// agent's per-agent thaw-state hostPath, so the [3] mount-rewrite forces the
		// restored agent bundle's MountThawState source to the SAME dir the
		// NodeRehydrationTrigger writes the marker into + polls acks from. req.AgentName
		// (not snap.AgentName) is the authoritative new-agent identity for this thaw;
		// they are equal today, but the rehydrate trigger keys off req.AgentName, so we
		// match it exactly to keep the restore mount and the marker/poll dir in lockstep.
		//
		// INVARIANT (ka-0000008265 / canary #7): this restore mount SOURCE and the
		// rehydrate WRITER/POLL dir (freezethaw_rehydrate.go's NodeRehydrationTrigger,
		// stateDir = thawStateHostDir(req.AgentName)) MUST stay keyed off the same
		// agent name. Forking either derivation onto a different name (e.g. snap.AgentName
		// here while the writer stays on req.AgentName) re-points the restored watchers'
		// thaw-state at an inode the controller never writes to — the both-consumer
		// fail-close of bug ka-0000000673. Keep BOTH on req.AgentName via the single
		// thawStateHostDir source of truth; guarded by
		// TestThawStateKey_WriterAndMountSourceAgree.
		ThawStateDir:  thawStateHostDir(req.AgentName),
		ThawStateDest: MountThawState,
		NatsHost:      natsHost,
		NatsPort:      natsPort,
		BeadsHost:     beadsHost,
		BeadsPort:     beadsPort,
		NatsDialAddr:  natsDialAddr,
		BeadsDialAddr: beadsDialAddr,
	})
	// scriptBytes is logged for observability of the rendered thaw body size:
	// it inlines base64 OCI spec files and grows with ContainerCount, and once
	// crossed 128 KiB it fail-closed E2BIG under the old `bash -c <script>`
	// argv transport (bug ka-0000004900). The script is now streamed over stdin
	// (`bash -s`, no MAX_ARG_STRLEN ceiling), so this is a trend metric, not a
	// hard limit — but a body ballooning well past 128 KiB still warrants a look.
	f.logger.Info("thaw: driving count-gated barrier restore on node",
		"agent", req.AgentName, "node", snap.Node, "containerCount", snap.ContainerCount,
		"scriptBytes", len(script))

	res, err := f.exec.Exec(dctx, snap.Node, script)
	if err != nil {
		return nil, fmt.Errorf("thaw: node exec on %s: %w", snap.Node, err)
	}
	fields := parseSentinels(res.Stdout)

	// Post-state assertions on the restore.
	//
	// Pre-restore image assertion (ka-0000003702) is checked FIRST — before the
	// generic non-zero-exit check — so a reaped image surfaces as a distinct,
	// loud image-deletion diagnosis rather than the opaque "node script exited 1".
	// The thaw script probes the 3.0G checkpoint image at the controller-owned
	// safe path BEFORE any runsc restore and emits FT_IMAGE_OK; if it is
	// absent/empty the image was reaped between freeze and thaw (the deletion
	// vector this fix closes) and the script bails early (FT_IMAGE_OK=no,
	// FT_ROOT_RC=128, exit 1). Distinguishing this from the bare runsc "opening
	// state file failed" error makes the failure mode unmistakable in the logs.
	if fields[keyImageOK] != "" && fields[keyImageOK] != "yes" {
		return nil, fmt.Errorf("thaw: checkpoint image absent at restore (FT_IMAGE_OK=%q) — the 3.0G runsc image at %s was deleted between freeze and thaw (Pod-A-teardown reap of the checkpoint dir); cannot restore: %s",
			fields[keyImageOK], snap.ImagePath, tail(res.Stderr, 600))
	}
	// netnsDiag surfaces the netns-graft sentinels (CNI gate / pin / L3 re-assert /
	// iface count / default route) that the thaw script emits on STDOUT but the
	// stderr-tail-only error logging truncated (ka-0000001790 observability gap). The
	// [6a] netns-content fail-closed path sets FT_ROOT_RC=128 + exit 1, so it lands
	// in the generic exit-code / root-rc errors below; appending these stdout signals
	// there makes a lo-only restore failure self-diagnosing (which of the three graft
	// layers broke) without a node-side redrive against a fresh canary.
	netnsDiag := fmt.Sprintf(" [netns: cni-confirmed=%q cni-pod-ip=%q pinned=%q l3-reasserted=%q self-ipam=%q prerestore-routable=%q ifaces=%q default-route=%q sentry-defroute=%q sentry-nonlo=%q sentry-egress=%q thaw-src-provenance=%q]",
		fields[keyCNIConfirmed], fields[keyCNIPodIP], fields[keyNetnsPinned], fields[keyNetnsL3Reasserted], fields[keyNetnsSelfIPAM], fields[keyPreRestoreRoutable], fields[keyNetnsIfaces], fields[keyNetnsDefRoute], fields[keySentryDefRoute], fields[keySentryNonLo], fields[keySentryEgress], fields[keyThawSrcProvenance])
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("thaw: node script exited %d:%s %s", res.ExitCode, netnsDiag, tail(res.Stderr, 600))
	}
	// Barrier rcs. Both are surfaced as a typed *ThawBarrierError carrying RootRC
	// and SubRC so the caller can decide retryability (ka-0000005365 step 4)
	// without string-matching: a SubRC>0/RootRC==0 failure is the suspected
	// transient agent-rootfs-overlay /sys-walk race (retryable), while a RootRC>0
	// failure is the deterministic netns/lo-only fail-closed path (never
	// retryable). A missing/unparseable sentinel is treated as a non-zero rc so a
	// truncated/legacy script output still fails closed. The wrapped Detail keeps
	// today's stderr-tail diagnosis verbatim in the controller log.
	rootRC, rootOK := intField(fields, keyRootRC)
	if !rootOK || rootRC != 0 {
		if !rootOK {
			rootRC = 1
		}
		return nil, &ThawBarrierError{RootRC: rootRC, Detail: netnsDiag + " " + tail(res.Stderr, 600)}
	}
	subRC, subOK := intField(fields, keySubRC)
	if !subOK || subRC != 0 {
		if !subOK {
			subRC = 1
		}
		return nil, &ThawBarrierError{RootRC: rootRC, SubRC: subRC, Detail: tail(res.Stderr, 600)}
	}
	restoredPID := fields[keyRestoredPID]
	if restoredPID == "" {
		return nil, errors.New("thaw: restored sentry pid not reported")
	}
	// netns proof: the restored sandbox must share Pod B's kubelet-owned netns
	// inode (network identity comes from kubelet, not A's resurrected bindings).
	// Both sentinels are net:[inode] strings (ka-0000004146 fix-a re-expressed
	// FT_B_NETNS as the stat-ed inode of the repointed CNI netns at
	// FT_B_NETNS_PATH so this compares like-for-like; the prior path-vs-inode
	// compare never matched).
	if bn, rn := fields[keyBNetns], fields[keyRestoredNS]; bn == "" || rn == "" || bn != rn {
		return nil, fmt.Errorf("thaw: restored sandbox netns %q does not match Pod B netns %q (B netns path %q)",
			rn, bn, fields[keyBNetnsPath])
	}
	// netns-CONTENT assertion (fix A, ka-0000007713): the inode proof above only
	// proves the sentry landed in the repointed netns, NOT that the netns carries
	// a real network. A lo-only netns (CNI eth0 torn down with the placeholder
	// sandbox delete) passes the inode proof but makes BOTH coop and gb fail
	// network-unreachable, fail-closing the rehydrate-ack gate 60s later. The
	// thaw script already fails-closed (FT_ROOT_RC=128) on a lo-only netns, so a
	// 0-iface / no-default-route result should not reach here with rc=0; assert it
	// anyway as defense in depth, and emit a clear lo-only diagnosis if the script
	// ever reports verified-but-network-less. The sentinels are emitted on every
	// restore that gets past the barrier, so treat their ABSENCE (legacy snapshot
	// / older script) as not-asserted rather than a failure.
	if ifaces, ok := intField(fields, keyNetnsIfaces); ok {
		if ifaces < 1 || fields[keyNetnsDefRoute] != "yes" {
			// Surface the netns-graft diagnostics from stdout (ka-0000001790): the
			// stderr tail truncates the gate/pin/L3-reassert/self-ipam sentinels, so
			// include them explicitly to localize the failure to its layer (CNI gate vs.
			// netns pin vs. Fix-B L3 re-assert vs. Fix-C self-IPAM) without a node-side
			// redrive. The two-inode split (ka-0000001099) shows up here as
			// l3-reasserted="no" (sandbox-netns capture found nothing) WITH self-ipam
			// engaged — if self-ipam is "yes" and the netns is STILL lo-only, the
			// restore-target netns lost eth0 itself (deeper than any L3 fix).
			return nil, fmt.Errorf("thaw: restored sandbox netns is lo-only (non-loopback interfaces=%d, default-route=%q; cni-confirmed=%q cni-pod-ip=%q netns-pinned=%q l3-reasserted=%q self-ipam=%q) — the CNI eth0 is absent/unrouted in the restore target, so rehydrate-ack would fail network-unreachable on both coop and gb; failing restore at restore time, not 60s later: %s",
				ifaces, fields[keyNetnsDefRoute], fields[keyCNIConfirmed], fields[keyCNIPodIP], fields[keyNetnsPinned], fields[keyNetnsL3Reasserted], fields[keyNetnsSelfIPAM], tail(res.Stderr, 600))
		}
	}
	// SENTRY-SIDE egress assertion (fix B, ka-0000008207 / bug ka-0000000514),
	// defense-in-depth for the Go side. The [6a] host-view check above is BLIND to a
	// link-only restored sentry — runsc setupNetwork() scrapes the netns into a fresh
	// sentry netstack during the restore RPC, so the sentry can be link-only even when
	// the host's view of that netns inode is routable. That is the exact prod .13 fail
	// (rehydrate "not released; failed consumers: coop, gb"). The thaw script's [6c]
	// reads the SENTRY's OWN netstack (runsc exec) + dials the consumers' endpoints and
	// already fails-closed FT_ROOT_RC=128 on a definitively link-only sentry, so a thaw
	// reaching here with rc=0 should report a routable sentry; assert it anyway. A
	// definitive sentry-side "no default route" (FT_SENTRY_DEFROUTE=no) or an
	// ENETUNREACH dial (FT_SENTRY_EGRESS=link-only) is the bug signature -> reject.
	// POSITIVE-PROOF tightening (bug ka-0000002907): FT_SENTRY_DEFROUTE=unknown is ALSO
	// a reject now — after the script's [6c-1] retry loop, an un-verified route means the
	// sentry was un-joinable across the whole readiness window (the canary #11 flake-open),
	// not a benign t+3s timing blip. "inconclusive"/"skipped" egress (refused/timeout/DNS
	// or no resolved target — a routed SYN, not a no-route) and ABSENT sentinels (legacy
	// script) stay NOT failures — the 60s ack gate remains the backstop for those.
	// PRE-RESTORE routability verdict, defense-in-depth (bug ka-0000002907). The node
	// script's fix-A now fails-closed FT_ROOT_RC=128 when the restore-target netns was
	// NOT routable at scrape time (a late kubelet CNI DEL re-stripped L3 before gVisor
	// setupNetwork() scraped it), so a "no" verdict should never reach here with rc=0.
	// Assert it anyway: a FT_NETNS_PRERESTORE_ROUTABLE=no verdict means gVisor scraped a
	// link-only netns into the sentry, which cannot egress for its whole life. Absent
	// (legacy snapshot / older script) is not-asserted, not a failure.
	if pr := fields[keyPreRestoreRoutable]; pr == "no" {
		return nil, fmt.Errorf("thaw: restore-target netns was NOT routable at scrape time (prerestore-routable=%q; sentry-defroute=%q sentry-egress=%q self-ipam=%q) — a late kubelet CNI DEL re-stripped L3 before gVisor setupNetwork() scraped the netns, so the restored sentry booted link-only and rehydrate-ack would fail network-unreachable on both coop and gb for 60s. Failing restore at restore time: %s",
			pr, fields[keySentryDefRoute], fields[keySentryEgress], fields[keyNetnsSelfIPAM], tail(res.Stderr, 600))
	}
	// POSITIVE-PROOF default-route rule (bug ka-0000002907). The sentry-side read must
	// positively report a default route (FT_SENTRY_DEFROUTE=yes) to be declared
	// restore-verified. A definitive "no" is the link-only bug signature; an "unknown"
	// means the retry-bounded in-sentry probe could NOT positively verify a route across
	// the whole readiness window — the exact canary #11 flake-open the pre-fix code
	// tolerated as benign and then died 60s later on the ack gate. Both now fail-close.
	// ABSENT (legacy snapshot / older script that never emits the sentinel) stays
	// not-asserted — the [6a] inode + content checks still gate it.
	if dr := fields[keySentryDefRoute]; dr == "no" {
		return nil, fmt.Errorf("thaw: restored SENTRY netstack is link-only (sentry default-route=%q sentry-nonlo=%q; prerestore-routable=%q self-ipam=%q) — read from inside the restored sandbox (the gVisor netstack's own view, not the host kernel's), so the host-side netns check passed but the sentry cannot egress; rehydrate-ack would fail network-unreachable on both coop and gb for 60s. Failing restore at restore time: %s",
			dr, fields[keySentryNonLo], fields[keyPreRestoreRoutable], fields[keyNetnsSelfIPAM], tail(res.Stderr, 600))
	}
	if dr := fields[keySentryDefRoute]; dr == "unknown" {
		return nil, fmt.Errorf("thaw: restored SENTRY default route was NOT positively verified (sentry default-route=%q sentry-nonlo=%q; prerestore-routable=%q sentry-egress=%q) — the retry-bounded in-sentry netstack read never returned a definitive yes across the readiness window, i.e. the sentry was un-joinable or routeless (the canary #11 flake-open signature). Positive proof of a default route is required; failing restore at restore time for a clean cold-start rather than a verified-but-network-less resume: %s",
			dr, fields[keySentryNonLo], fields[keyPreRestoreRoutable], fields[keySentryEgress], tail(res.Stderr, 600))
	}
	if eg := fields[keySentryEgress]; eg == "link-only" {
		return nil, fmt.Errorf("thaw: restored SENTRY cannot egress to the rehydrate consumers' endpoints (sentry-egress=%q sentry-defroute=%q; prerestore-routable=%q) — an in-sentry TCP dial to NATS/beads returned network-unreachable (the errno coop+gb hit for 60s on prod .13). Failing restore at restore time: %s",
			eg, fields[keySentryDefRoute], fields[keyPreRestoreRoutable], tail(res.Stderr, 600))
	}
	// THAW-STATE SOURCE-PROVENANCE assertion (ka-0000001208), defense-in-depth for the
	// Go side. The [6d] node check writes a fresh marker into the live per-agent host
	// source $THAWDIR (thawStateHostDir(req.AgentName)) and reads it back from inside the
	// restored sentry at /run/gardenio-thaw: a DEFINITIVE ABSENT (FT_THAW_SRC_PROVENANCE=
	// mismatch) proves the restore re-walked the thaw-state mount to a stale/placeholder/
	// checkpointed 9p-gofer source, so the rehydrate consumers' .done acks would land in a
	// dir the controller never reads and the both-consumer ack gate fail-closes ~60s later
	// as an opaque timeout (canary #7 signature). The script already fails-closed
	// FT_ROOT_RC=128 on a mismatch, so a thaw reaching here with rc=0 should report "ok"
	// (or "unknown"/absent); assert it anyway for a clear controller-log diagnosis. Only a
	// definitive "mismatch" rejects — "ok"/"unknown" (in-sentry exec flaked or legacy
	// snapshot with an empty $THAWDIR) and an ABSENT sentinel (older script) stay
	// not-asserted, with the [3a] pre-restore guard + the ack gate as the backstops.
	if sp := fields[keyThawSrcProvenance]; sp == "mismatch" {
		return nil, fmt.Errorf("thaw: restored /run/gardenio-thaw does NOT resolve to the live per-agent host source thawStateHostDir(%q) (thaw-src-provenance=%q) — a fresh marker written into the host source was not visible inside the restored sentry, so the restore re-walked the thaw-state mount to a stale/placeholder/checkpointed source; the rehydrate consumers' .done acks would land in a dir the controller never reads and the both-consumer ack gate would fail-closed network-... ack-timeout ~60s later (canary #7 signature). Failing restore at restore time: %s",
			req.AgentName, sp, tail(res.Stderr, 600))
	}
	if up := fields[keyContainersUp]; up != "" && up != "yes" {
		return nil, fmt.Errorf("thaw: not all containers running after restore (got %q)", up)
	}

	result = &ThawResult{
		Snapshot:       snap,
		PlaceholderPod: bName,
		BSandboxCID:    fields[keyBSandboxCID],
		BPodUID:        bUID,
		BNetns:         fields[keyBNetns],
		RestoredPID:    restoredPID,
		RestoredNetns:  fields[keyRestoredNS],
	}
	// MEASUREMENT hook (ka-0000003398 fix C): capture the actual restore-peak RSS
	// the node script read from the restored container's cgroup memory.peak.
	// Best-effort — a zero / unparseable value (legacy script, cgroup-v1 node,
	// unresolvable scope) is left at 0 and NEVER fails the verified restore. Log it
	// against the modeled peak so the multiplier can be calibrated over canaries.
	if peak, ok := int64Field(fields, keyRestorePeakBytes); ok && peak > 0 {
		result.RestorePeakBytes = peak
		cc := f.memGateContainerCount(snap.ContainerCount)
		modeledPeak := modeledRestorePeakLimit(snap.PagesBytes, cc)
		f.logger.Info("thaw: restore-peak measurement (ka-0000003398 fix C — for multiplier calibration)",
			"agent", req.AgentName, "podB", bName,
			"restorePeakBytes", peak, "pagesBytes", snap.PagesBytes,
			"containerCount", cc, "modeledPeakLimit", modeledPeak,
			"peakOverPagesPermille", peakOverPagesPermille(peak, snap.PagesBytes),
			"medium", snap.OverlayMedium)
	}
	f.logger.Info("thaw: restore verified",
		"agent", req.AgentName, "podB", bName, "restoredPID", restoredPID, "netns", result.RestoredNetns)

	// --- Step 6: rehydration hand-off (ka-0000001515) ----------------------
	if !req.SkipRehydration && f.rehydr != nil {
		rhErr := f.rehydr.Trigger(dctx, RehydrationRequest{
			AgentName:   req.AgentName,
			Namespace:   snap.Namespace,
			PodName:     bName,
			Node:        snap.Node,
			RestoredPID: restoredPID,
			Deadline:    f.rehydrateDeadline(),
			PodUID:      bUID,
			FrozenAt:    snap.CreatedAt,
		})
		if rhErr != nil {
			// The process tree is back and verified; a rehydration failure is
			// reported but does not unwind the restore (the caller can retry
			// rehydration against the live Pod B). Do NOT reap the image here: the
			// caller's partial-success path (main.go) tears down Pod B and cold-
			// starts, and a clean cold start can only happen if the snapshot — and
			// thus its image — survives. Reaping the image would strand the
			// snapshot un-thawable for any retry.
			return result, fmt.Errorf("thaw: restore succeeded but rehydration failed: %w", rhErr)
		}
		result.Rehydrated = true
		f.logger.Info("thaw: rehydration hand-off complete", "agent", req.AgentName, "podB", bName)
	}

	// --- Step 7: reap the now-consumed checkpoint image (ka-0000006968) ----
	// The restore is verified (and, when this call owns rehydration, acked above —
	// the rehydration-failure path returns early before here, so reaching this
	// point means the thaw fully succeeded): the 3.0G image at snap.ImagePath has
	// been loaded into the live sentry and is dead weight — the restored container
	// runs off the bundle overlay, not the image. Because the ka-0000003702
	// relocation put the image at the controller-owned /var/lib/gardenio/ckpt/<agent>
	// OUTSIDE any pod-lifecycle GC, nothing else ever reclaims it, so a successful
	// single freeze→thaw cycle would leak one 3.0G image per agent on the node.
	// Reap it now, exactly when it is provably safe.
	//
	// SkipRehydration short-circuits this: that caller explicitly drives its own
	// post-restore lifecycle (and may tear down Pod B + retry the thaw from the
	// snapshot), so the image must survive for it — reaping would strand the
	// snapshot un-thawable. The BUNDLE is NEVER reaped here either: $BUNDLE/<c>/
	// rootfs is the live overlay mountpoint and $BUNDLE/.work the live /work source,
	// both backing the running Pod B (multi-cycle re-freeze cleanup is the
	// snapshot-transport follow-up's concern). Best-effort: a leaked image is a
	// disk-space nuisance, never a correctness problem, so a reap failure is logged
	// but never fails the already-succeeded thaw.
	if !req.SkipRehydration {
		result.ImageReaped = f.reapImage(dctx, snap)
		// Mark the snapshot CONSUMED now that this single freeze→thaw cycle is
		// complete (ka-0000001878). The image is gone but the record survives (its
		// bundle backs the live Pod B), so without this flag the record would (a)
		// drive a doomed thaw on the next spawn — FT_IMAGE_OK=no against the reaped
		// image — and (b) hold warm-B capacity for a bucket no live thaw will claim,
		// and a graceful re-stop that ever reached Freeze would re-checkpoint over
		// the live bundle. Persist Consumed=true so Freeze's re-freeze guard refuses
		// + discards it and HasSnapshot/SnapshotNode/SnapshotTopology/WarmBuckets
		// treat it as absent → the next spawn cold-starts directly. Best-effort and
		// SYMMETRIC with the reap: only the rehydration-owning caller marks consumed
		// (SkipRehydration drives its own retry-from-snapshot lifecycle, so its
		// record must stay live+thawable, exactly as its image is not reaped). A Put
		// failure is logged, not fatal — the thaw already succeeded, and the thawed
		// pod's name still misses shouldFreeze's canonical-name Get as a backstop.
		consumed := snap
		consumed.Consumed = true
		if perr := f.snaps.Put(dctx, consumed); perr != nil {
			f.logger.Warn("thaw: failed to mark snapshot consumed after verified thaw; next spawn may attempt a doomed thaw against the reaped image until the record clears",
				"agent", req.AgentName, "node", snap.Node, "error", perr)
		} else {
			f.logger.Info("thaw: marked snapshot consumed (single freeze→thaw cycle complete; re-freeze will degrade to cold start)",
				"agent", req.AgentName, "node", snap.Node)
		}
	}
	return result, nil
}

// reapImage deletes the consumed 3.0G checkpoint image dir for a verified thaw
// (ka-0000006968) and reports whether the reclaim landed. It is best-effort by
// design: the restore has already succeeded, so a failed reap only leaks disk
// and must never propagate as a thaw error. The bundle is left intact — it backs
// the live Pod B (rootfs overlay + /work source).
func (f *FreezeThawManager) reapImage(ctx context.Context, snap Snapshot) bool {
	if snap.ImagePath == "" {
		return false
	}
	return f.reapImageAt(ctx, snap.Node, snap.ImagePath)
}

// reapImageAt deletes the checkpoint image dir at imagePath on node, returning
// whether the reclaim was confirmed (FT_IMAGE_REAPED=yes). It is the node-exec
// primitive both reapImage (post-thaw, by Snapshot) and discardAbortedFreeze
// (mid-checkpoint abort, by freezeImagePath) share. Best-effort: a transport
// error or unconfirmed reap returns false and logs — the caller treats a leaked
// image as a disk-space nuisance, never a correctness failure.
func (f *FreezeThawManager) reapImageAt(ctx context.Context, node, imagePath string) bool {
	if node == "" || imagePath == "" {
		return false
	}
	script := f.buildReapImageScript(reapImageScriptParams{ImagePath: imagePath})
	res, err := f.exec.Exec(ctx, node, script)
	if err != nil {
		f.logger.Warn("checkpoint-image reaper node exec failed (best-effort; image leaked)",
			"node", node, "imagePath", imagePath, "error", err)
		return false
	}
	if parseSentinels(res.Stdout)[keyImageReaped] != "yes" {
		f.logger.Warn("checkpoint-image reaper did not confirm reclaim (best-effort; image may be leaked)",
			"node", node, "imagePath", imagePath, "stderr", tail(res.Stderr, 200))
		return false
	}
	return true
}

// createPathPodBTeardown returns a cleanup closure that tears down a create-path Pod B
// (ka-0000005365) when thawOnce exits with an error AND no *ThawResult. thawOnce `defer`s
// the returned closure the instant it commits to minting Pod B — BEFORE Create +
// waitPodReady (ka-0000006072) — so a Ready-wait TIMEOUT (now bounded by the generous
// createRestorePodReadyTimeout, up to 600s for a repo-heavy cold clone) also tears the
// pending Pod B down instead of leaking it to the reconciler reaper. The closure reads the
// caller's FINAL named returns through resultPtr/errPtr at defer-execution time. (Returning
// a closure the caller defers — rather than deferring here — is required: a defer inside
// this helper would fire on the helper's OWN return, not thawOnce's.)
//
// A create-path Pod B that fails was previously LEAKED: thawOnce returned (nil, err)
// without deleting it, and the reconciler maps "<canonical>-thaw-b" under the CANONICAL
// key — colliding with the cold-start pod the caller then spawns — so the Failed Pod B
// could persist undeleted (map-order-nondeterministic orphan cleanup). The warm-graft path
// owns its analogous teardown (freezeThawLifecycle.DeletePromoted); the create path had no
// equivalent. A non-nil result is the rehydration partial-success path, whose teardown is
// the caller's (main.go deletes res.PlaceholderPod) — deleting here too would double-delete
// and race that branch, so it is deliberately excluded. Detached from the inbound ctx
// (WithoutCancel) so a leader-change / pod roll mid-thaw — exactly what cancels ctx —
// cannot turn the cleanup into a no-op and re-leak the pod (mirrors the dctx rationale and
// main.go's thawTeardownTimeout). A Create that never succeeded is harmless: Delete's
// IsNotFound is ignored.
func (f *FreezeThawManager) createPathPodBTeardown(ctx context.Context, resultPtr **ThawResult, errPtr *error, agent, namespace, bName string) func() {
	return func() {
		if *errPtr != nil && *resultPtr == nil {
			tdCtx, tdCancel := context.WithTimeout(context.WithoutCancel(ctx), f.thawHardDeadline())
			defer tdCancel()
			if derr := f.client.CoreV1().Pods(namespace).Delete(tdCtx, bName, metav1.DeleteOptions{}); derr != nil && !apierrors.IsNotFound(derr) {
				f.logger.Error("thaw: failed to tear down create-path Pod B after failed restore; cold start may orphan it",
					"agent", agent, "podB", bName, "namespace", namespace, "error", derr)
			} else {
				f.logger.Info("thaw: tore down create-path Pod B after failed restore (no leak)",
					"agent", agent, "podB", bName, "namespace", namespace)
			}
		}
	}
}

// waitPodReady polls Pod B until it reports the Ready condition, bounded by the
// supplied timeout. The create-then-restore path passes the clone-aware
// createRestorePodReadyTimeout (a fresh Pod B inlines a cold init-clone into the
// readiness window); the no-clone warm-B/teardown paths use podReadyTimeout.
func (f *FreezeThawManager) waitPodReady(ctx context.Context, namespace, name string, timeout time.Duration) error {
	deadline := f.now().Add(timeout)
	ticker := time.NewTicker(f.podReadyPollInterval())
	defer ticker.Stop()
	for {
		pod, err := f.client.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
				return fmt.Errorf("pod entered terminal phase %s before Ready", pod.Status.Phase)
			}
			if podIsReady(pod) {
				return nil
			}
		}
		if !f.now().Before(deadline) {
			return fmt.Errorf("timed out after %s waiting for Ready", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func podIsReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// waitPodGone blocks until Pod A is provably absent from the API, bounded by
// PodReadyTimeout (ka-0000005680). It is the teardown-complete barrier that
// keeps the soon-to-be-created placeholder Pod B from contending with a
// not-yet-fully-torn-down Pod A on the same node's thaw-state hostPath (RWO)
// and runsc sandbox/CID. An empty name (no Pod A to wait on) or an
// already-NotFound pod returns nil immediately, so the very first thaw — where
// the lifecycle already deleted Pod A long ago — is not slowed. A transient Get
// error is treated as "still present" and retried until the deadline; only a
// timeout (Pod A still listed) is fatal, surfacing as a distinct, actionable
// error instead of the opaque downstream "Pod B Failed before Ready". The last
// non-NotFound Get error is carried into the timeout message so a structural
// failure (e.g. an RBAC Forbidden that can never become NotFound) is diagnosable
// rather than misattributed to a slow teardown.
func (f *FreezeThawManager) waitPodGone(ctx context.Context, namespace, name string) error {
	if name == "" {
		return nil
	}
	deadline := f.now().Add(f.podReadyTimeout())
	ticker := time.NewTicker(f.podReadyPollInterval())
	defer ticker.Stop()
	var lastErr error
	for {
		_, err := f.client.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil // Pod A is gone — the node-scoped resources it held are released.
		}
		if err != nil {
			lastErr = err
		}
		if !f.now().Before(deadline) {
			if lastErr != nil {
				return fmt.Errorf("timed out after %s waiting for Pod A %s to be deleted (last Get error: %w)", f.podReadyTimeout(), name, lastErr)
			}
			return fmt.Errorf("timed out after %s waiting for Pod A %s to be deleted (still present)", f.podReadyTimeout(), name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// logPodBFailureDiagnostics fetches the placeholder Pod B's full status and
// recent events and logs the container-level terminal cause on the "not
// Ready / terminal Failed before Ready" branch (ka-0000005680). The controller
// runs with cluster-wide RBAC, so it CAN read pods/events in the gVisor
// namespace even though the gardenio-agent SA cannot — without this, the actual
// kubelet rejection reason is invisible and the next re-drive needs
// cluster-admin to diagnose. Strictly best-effort: every fetch failure is
// logged and swallowed so diagnostics never mask the original thaw error.
func (f *FreezeThawManager) logPodBFailureDiagnostics(ctx context.Context, agentName, namespace, podName, node string) {
	pod, err := f.client.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		f.logger.Warn("thaw: could not fetch placeholder Pod B status for diagnostics",
			"agent", agentName, "podB", podName, "namespace", namespace, "error", err)
	} else {
		f.logger.Error("thaw: placeholder Pod B failed before Ready — pod-level diagnostics",
			"agent", agentName, "podB", podName, "namespace", namespace, "node", node,
			"phase", string(pod.Status.Phase),
			"reason", pod.Status.Reason,
			"message", pod.Status.Message)
		// Container-level terminal cause: terminated{reason,message,exitCode}
		// and waiting{reason,message} for every init + app container. This is
		// the actual kubelet rejection detail the pod-level phase hides.
		for _, group := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
			for _, cs := range group {
				if t := cs.State.Terminated; t != nil {
					f.logger.Error("thaw: Pod B container terminated",
						"agent", agentName, "podB", podName, "container", cs.Name,
						"terminatedReason", t.Reason, "terminatedMessage", t.Message, "exitCode", t.ExitCode)
				}
				if w := cs.State.Waiting; w != nil {
					f.logger.Error("thaw: Pod B container waiting",
						"agent", agentName, "podB", podName, "container", cs.Name,
						"waitingReason", w.Reason, "waitingMessage", w.Message)
				}
			}
		}
	}

	// Recent involvedObject events for Pod B (FailedScheduling, FailedMount,
	// FailedCreatePodSandBox, etc.) — the kubelet/scheduler narration that
	// explains an immediate admit-failure that leaves no container status.
	selector := fields.AndSelectors(
		fields.OneTermEqualSelector("involvedObject.name", podName),
		fields.OneTermEqualSelector("involvedObject.kind", "Pod"),
	).String()
	events, eerr := f.client.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{FieldSelector: selector})
	if eerr != nil {
		f.logger.Warn("thaw: could not list events for placeholder Pod B diagnostics",
			"agent", agentName, "podB", podName, "namespace", namespace, "error", eerr)
		return
	}
	for _, ev := range events.Items {
		f.logger.Error("thaw: Pod B event",
			"agent", agentName, "podB", podName,
			"type", ev.Type, "reason", ev.Reason, "message", ev.Message,
			"fieldPath", ev.InvolvedObject.FieldPath, "count", ev.Count)
	}
}

// buildPlaceholderPodB mints Pod B from the snapshot's preserved Pod A template:
// same mount shape (workspace, projected SA token, resolv.conf/hosts) so the
// restored container's mount destinations line up, runsc runtime class, pinned
// to the snapshot node (node-local image), but with container commands
// overridden to a sleep placeholder so the body never races the restore. The
// placeholder is a sleeping shell — kubelet provisions the fresh, kubelet-owned
// bindings, and the restore grafts the checkpointed process state on top.
//
// # Repo rehydration (C6, ka-0000007582 — strategy b)
//
// A gVisor checkpoint image captures the agent process RAM plus the agent
// container's rootfs overlay — but NOT the workspace volume's contents. The repo
// lives on a SEPARATE mount (VolumeWorkspace, an emptyDir for ephemeral agents,
// a PVC for persistent ones), and a fresh Pod B re-provisions that volume empty.
// So a grafted agent whose cwd is the repo dir would wake into an EMPTY working
// tree (the ka-0000007582 gap, and the POC "failed to walk … no such file"
// class). The fix is to re-run the preserved init-clone container on Pod B: it
// re-hydrates the workspace (a fresh clone on an emptyDir, a fast git fetch on a
// PVC) BEFORE the placeholder agent container starts, so the repo is present at
// the mount the restored process cwd's into. init-clone is a true init container
// (it runs to completion and exits), so it never joins the running sandbox and
// does not perturb the count-gated barrier restore (still pause + agent = 2).
//
// Native sidecars (DinD/Postgres) are modeled as init containers with
// restartPolicy: Always, so they STAY running and ARE part of the checkpointed
// sandbox's container count. The node-side thaw script now restores the sandbox
// PLUS every captured sub-container in one count-gated barrier (ka-0000003561),
// so Pod B must present the SAME kubelet container set the checkpoint expects —
// the always-on sidecars are KEPT (re-added as sleeping placeholders below) so
// the kubelet provisions their sandbox slots and the N-container restore grafts
// the checkpointed sidecar state on top. Earlier (C6) these were dropped because
// the barrier only reconstructed sandbox+agent; that limitation is now lifted.
func (f *FreezeThawManager) buildPlaceholderPodB(snap Snapshot, name string) *corev1.Pod {
	// shapePlaceholderSpec is the single source of truth for the placeholder
	// spec shape (mount destinations, runsc class, sleep-shell commands, kept
	// init-clone + always-on sidecars) shared with the warm-B pool builder
	// (ka-0000002219). It deep-copies the template, so we never mutate the snap.
	spec := shapePlaceholderSpec(snap.PodTemplate, snap.Node, snap.PagesBytes, snap.ContainerCount)
	if _, sidecars := partitionInitContainers(snap.PodTemplate.InitContainers); len(sidecars) > 0 {
		names := make([]string, 0, len(sidecars))
		for _, s := range sidecars {
			names = append(names, s.Name)
		}
		f.logger.Info("thaw: keeping always-on sidecars on placeholder Pod B for the N-container barrier restore (ka-0000003561)",
			"agent", snap.AgentName, "podB", name, "sidecars", strings.Join(names, ","),
			"containerCount", snap.ContainerCount)
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: snap.Namespace,
			Labels: map[string]string{
				LabelApp:             LabelAppValue,
				LabelAgent:           truncateLabel(snap.AgentName),
				LabelThawPlaceholder: "true",
			},
			Annotations: map[string]string{
				AnnotationBeadID: snap.BeadID,
			},
		},
		Spec: spec,
	}
}

// partitionInitContainers splits a pod's initContainers into (run-to-completion
// init containers, always-on native sidecars). The K8s 1.29+ native sidecar
// pattern models a sidecar as an init container with restartPolicy: Always — it
// starts before the app containers but KEEPS running, so it is part of the
// pod's live container set (and the gVisor checkpoint's container_count). A
// plain init container (nil/Never restartPolicy, e.g. init-clone) runs to
// completion and exits, so it is NOT part of the running sandbox.
//
// buildPlaceholderPodB uses this to keep init-clone on Pod B (repo rehydration,
// C6) while dropping DinD/Postgres sidecars the count=2 barrier restore cannot
// reconstruct yet (ka-0000007582 follow-up). Order is preserved within each
// partition.
func partitionInitContainers(inits []corev1.Container) (keep, sidecars []corev1.Container) {
	for _, c := range inits {
		if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			sidecars = append(sidecars, c)
			continue
		}
		keep = append(keep, c)
	}
	return keep, sidecars
}

// sanitizePodTemplate strips runtime-assigned fields from a live pod spec so it
// can be persisted and replayed as a fresh placeholder pod. NodeName is cleared
// here (Thaw re-pins it to the snapshot node) along with scheduler/host fields
// that must not be carried verbatim.
func sanitizePodTemplate(spec corev1.PodSpec) corev1.PodSpec {
	out := *spec.DeepCopy()
	out.NodeName = ""
	out.Hostname = ""
	out.Subdomain = ""
	out.DeprecatedServiceAccount = ""
	return out
}
