// freezethaw.go implements the PATH-B (multi-container / sidecar) gVisor
// freeze/thaw verbs for agent pods, wrapping steps 2–6 of the proven PoC
// harness (gardenio/docs/gvisor-runsc-cr/restore-through-lifecycle.md and
// multi-container-resume.md) as controller-shaped operations.
//
// Decision ka-0000007932 resolved CHOSEN=path-b on 2026-06-04: agent pods keep
// today's CRI shape (pause sandbox + agent sub-container, sidecars possible);
// there is NO fleet-wide single-container migration. A runsc pod is therefore
// two containers sharing one sentry, and freeze/thaw is a count-gated barrier
// (multi-container-resume.md "The actual blocker").
//
// Division of labour:
//   - The mechanics that MUST run on the node — `runsc checkpoint|restore`,
//     /proc/mounts overlay capture, OCI-spec id rewriting under `nsenter -t 1`,
//     rootfs overlay reconstruction in host-init's mount ns — stay in node-side
//     scripts (freezethaw_node.go), faithful to the proven harness, because
//     that is where /proc and the containerd runtime root live.
//   - The controller owns the parts the PoC could only fake by hand: creating
//     the placeholder Pod B through the k8s API so kubelet re-provisions the
//     netns/volumes/projected token (the GC-defeating placeholder-pod pattern),
//     waiting for it Ready, persisting the snapshot keyed by agent identity,
//     wrapping every runsc invocation in `timeout` + Go-side post-state
//     assertions (runsc C/R can hang on process exit — epic constraint), and
//     triggering the rehydration hand-off (ka-0000001515).
//
// Everything node-side is reached through the NodeExecutor interface (a privileged
// hostPath probe / DaemonSet exec, the same access pattern the PoC harnesses and
// Phase-2 use), which is faked in tests so the orchestration is unit-testable
// without a live gVisor node.
package podmanager

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

// RuntimeClassRunsc is the RuntimeClass name that pins a pod onto the gVisor
// sandboxed runtime. Both Pod A and the placeholder Pod B must use it so the
// checkpoint image and the restore target share a runsc sentry.
const RuntimeClassRunsc = "runsc"

// Sentinel keys emitted by the node scripts as `KEY=value` lines on stdout.
// The Go orchestration parses these instead of re-deriving node state, keeping
// the script the single source of truth for what it observed on the node.
const (
	keySandboxCID     = "FT_SANDBOX_CID"
	keyAgentCID       = "FT_AGENT_CID"
	keyPodUID         = "FT_POD_UID"
	keyContainerCount = "FT_CONTAINER_COUNT"
	keyCheckpointRC   = "FT_CHECKPOINT_RC"
	keyImageSize      = "FT_IMAGE_SIZE"

	// keyPagesBytes is the exact byte size of runsc's pages.img (the RAM image
	// the restore loads EAGERLY, ka-0000000088). Parallel to keyImageSize but a
	// raw integer instead of the human-readable du -sh output: the pre-thaw
	// node memory-capacity gate (thawOnce) needs the number to compare against
	// MemAvailable so a subject cannot be scheduled onto a node that structurally
	// cannot fit the eager restore (a c6a.large / 3.05Gi allocatable node was the
	// canary #12 kernel-OOM SIGKILL). The freeze script emits `stat -c %s` on
	// pages.img; a parse miss / empty sentinel means a legacy snapshot without
	// the field — thawOnce falls back to a node-side stat.
	keyPagesBytes = "FT_PAGES_BYTES"

	// keyPauseRC is the rc of the `runsc pause $SB` that opens the
	// consistent-snapshot window (ka-0000004656, decision ka-0000003069:
	// fix_tmp_consistency). The freeze script pauses the sentry, takes the
	// 9p-gofer warm copies (/work,/tmp,.gofer) while the guest is quiesced, then
	// `runsc checkpoint --leave-running` dumps the SAME paused VFS — so the warm
	// copy and the checkpoint dentry set are byte-identical (no live-Claude
	// TOCTOU). Best-effort/diagnostic only: a non-zero FT_PAUSE_RC means the
	// script degraded to the pre-fix non-atomic copy (still freezes; a quiescent
	// agent thaws clean, a churny one keeps its old rc=128 risk) — it never gates
	// the freeze, so the Go side only records it for observability.
	keyPauseRC      = "FT_PAUSE_RC"
	keySandboxState = "FT_SANDBOX_STATE"
	keyImagePresent = "FT_IMAGE_PRESENT"
	keyBundleOK     = "FT_BUNDLE_OK"

	// keyWorkCPRC / keyTmpCPRC / keyGoferFailed are the freeze-side PAYLOAD-
	// COMPLETENESS sentinels (ka-0000000276). The freeze node script copies three
	// runtime-populated 9p-gofer trees into the GC-safe bundle inside the pause
	// window — /work (.work), /tmp (.tmp), and the data-driven .gofer sweep — so
	// thaw can repoint Pod B at node-local warm copies the fresh kubelet EmptyDirs
	// lack. Pre-fix all three swallowed copy errors (the /work + /tmp cp ran
	// 2>/dev/null with no exit check; the .gofer copytree was wrapped in
	// except Exception: continue), so a mid-copy ENOSPC/EIO silently PUBLISHED an
	// incomplete-payload snapshot marked usable. It fails closed at THAW (the
	// un-preserved mount repoints to Pod B's fresh empty dir and cold-starts — never
	// restores a half tree), so it is an INTEGRITY / wasted-freeze issue, not an
	// availability one; but it emitted ZERO signal, wasting a full freeze silently.
	//   keyWorkCPRC   = the `cp -aL` rc for the /work warm copy (0 clean, non-zero
	//                   the copy was incomplete). Missing => older node script.
	//   keyTmpCPRC    = same for the /tmp warm copy.
	//   keyGoferFailed = count of .gofer sweep mounts whose copytree threw (a genuine
	//                   ENOSPC/EIO), NOT a dangling-symlink skip and NOT a CoW->copy2
	//                   fallback (both of which still complete the bytes). >0 => that
	//                   many mounts have no warm copy and will cold-start at thaw.
	// The Go side WARNs on any non-clean value into the persisted-snapshot log so the
	// incomplete payload is visible in canary + fleet telemetry, but DELIBERATELY does
	// NOT gate the freeze: (a) cp -aL DEREFERENCES symlinks, so a benign dangling link
	// in a real /work repo checkout trips a non-zero rc with no data loss — a hard gate
	// would spuriously cold-start thawable agents fleet-wide; and (b) the affected mount
	// already fails closed at thaw, so gating would only trade a wasted freeze for a
	// lost one. Same fail-closed-at-thaw, WARN-not-gate rationale as the sibling
	// FT_PAUSE_RC fix (ka-0000006783) from the same review bundle (/bundle/ka-0000005112).
	keyWorkCPRC    = "FT_WORK_CP_RC"
	keyTmpCPRC     = "FT_TMP_CP_RC"
	keyGoferFailed = "FT_GOFER_FAILED"

	// keyWorkCPReason / keyTmpCPReason DE-NOISE the /work + /tmp cp rc (ka-0000006933).
	// cp -aL DEREFERENCES symlinks, so a benign dangling symlink in a real /work repo
	// checkout (stale node_modules/.bin, deleted-target link) trips a non-zero rc while
	// copying every other byte completely. Pre-fix the Go side flagged payloadComplete=
	// false + WARN on ANY non-zero cp rc, so that benign noise fired on a large fraction
	// of byte-COMPLETE freezes fleet-wide and drowned the real ENOSPC/EIO signal. The
	// node script now classifies each non-zero cp rc (a metadata-only walk of the source
	// tree that mirrors what cp -aL materializes) into one of:
	//   "clean"    — rc==0 (emitted without running the classifier).
	//   "symlink"  — the payload is COMPLETE: every entry cp -aL would have materialized
	//                is present at matching size and the only thing that tripped the rc
	//                was >=1 benign DANGLING symlink cp skipped. This is the noise.
	//   "copyfail" — a GENUINE truncating failure (ENOSPC/EIO/short write) OR the probe
	//                could not positively prove completeness (walk error/timeout/over
	//                budget). The affected mount cold-starts at thaw.
	// The Go verdict below treats a /work|/tmp leg as a genuine incomplete ONLY when its
	// reason == "copyfail"; "clean"|"symlink" are complete. A MISSING reason (older node
	// script that emits the rc but not the reason) falls back to the pre-fix rc-based rule
	// (rcKnown && rc != 0 => incomplete) so old-node/new-Go keeps today's safe-but-noisy
	// behaviour until the new script ships — never a false-clean. Parsed exact-match: a
	// SIGKILL-truncated/partial reason on an rc!=0 leg is NOT "symlink"/"clean", so it
	// falls through to incomplete, never downgrading a lost freeze to complete.
	keyWorkCPReason = "FT_WORK_CP_REASON"
	keyTmpCPReason  = "FT_TMP_CP_REASON"

	// payloadReason* are the exact node-emitted reason tokens (keyWorkCPReason/
	// keyTmpCPReason). Only these three are valid; anything else on an rc!=0 leg is
	// treated as incomplete (never a false-clean).
	payloadReasonClean    = "clean"
	payloadReasonSymlink  = "symlink"
	payloadReasonCopyFail = "copyfail"

	// keyOverlayMedium is the runsc rootfs-overlay medium probed off the live
	// sandbox boot args at freeze (overlay-dir-medium, ka-0000007239), e.g.
	// "root:dir=/var/lib/gardenio/overlay" on the dir-medium pool or "" for the
	// default root:self. The snapshot self-describes its medium so thaw replays the
	// SAME --overlay2 to runsc restore. Best-effort/diagnostic at freeze: a miss
	// (empty) just means the default medium and a no-op thaw flag — never a gate.
	keyOverlayMedium = "FT_OVERLAY_MEDIUM"

	// keyCheckpointMS / keyRelocationMS are node-side step timers (task
	// ka-0000005147). The freeze node script brackets the runsc page-dump + 3.0G
	// image write (keyCheckpointMS) and the Step-3 rootfs/bundle relocation copy
	// (keyRelocationMS) with `date +%s%3N` so the Go side can attribute the two
	// fused-into-one-exec costs independently — the ~3.0G image write is the
	// suspect, but the relocation copy MUST be measured separately
	// (fast-interruptible-freeze.md §C1). Both are best-effort: a parse miss
	// yields a zero sub-timer (the total still records), never an error.
	keyCheckpointMS = "FT_CHECKPOINT_MS"
	keyRelocationMS = "FT_RELOCATION_MS"

	// keyRelocReflinkHits / keyRelocReflinkFallbacks are the Step-3 rootfs-relocation
	// REFLINK-ENGAGEMENT counters (ka-0000007174), mirroring the .gofer sweep's
	// FT_GOFER_REFLINK_HITS/FALLBACKS. They are emitted PER CONTAINER under the DIR
	// medium (the path the reflink fix rewrote): the relocation now reconstructs the
	// GC-safe lower from the REAL constituent bare-xfs layers (upperdir + image
	// lowerdirs) instead of the overlayfs MERGED mount, and an xfs->xfs layer copy
	// actually reflinks. A "hit" is a stacked layer segment whose cp -a --reflink=auto
	// engaged FICLONE (same device AND a CoW-capable dest fs); a "fallback" is a segment
	// copied by a full byte copy (cross-device EXDEV or a non-CoW fs). The count is
	// per-SEGMENT, not per-file, because the layer copy stays coreutils cp -a (the
	// faithful path for the overlay whiteout char-devices + trusted.overlay.* xattrs the
	// raw layers carry — a python per-file walker would corrupt a 0:0 whiteout into an
	// empty regular file); a per-segment same-device + dest-fs CoW probe is the
	// whiteout-safe equivalent of the gofer per-file tally.
	//
	// This is the smoking-gun signal FT_RELOC_FSTYPE (dest fstype only) could NOT give:
	// the dest is always the bare node xfs, so it read "xfs" even when the pre-fix
	// merged-mount cp SILENTLY no-oped to a full byte copy (the canary #13 false
	// reassurance). hits=0 with a non-empty stack == "the relocation did NOT reflink".
	// Parsed best-effort and log-only, NEVER a gate (all-fallbacks on a cross-device /
	// non-CoW node is expected, not an error; and the merged-cp fallback + self medium
	// legitimately do not emit them). Missing => older node script or self medium.
	keyRelocReflinkHits      = "FT_RELOC_REFLINK_HITS"
	keyRelocReflinkFallbacks = "FT_RELOC_REFLINK_FALLBACKS"

	// keyImageOK is the host-ns re-verify of the 3.0G checkpoint image at the
	// controller-owned safe path (ka-0000003702). Parallel to keyBundleOK: the
	// freeze script emits FT_IMAGE_OK=yes only when `HMNT test -s
	// $IMG/checkpoint.img` passes IN THE HOST NS at /var/lib/gardenio/ckpt — i.e.
	// the image is provably present, non-empty, and outside the containerd /
	// pod-lifecycle GC zone before the snapshot is persisted. The thaw script
	// re-asserts the same key as a loud pre-restore guard.
	keyImageOK = "FT_IMAGE_OK"

	// keySpecFile is the GENERIC, repeated spec-capture sentinel (fix B,
	// ka-0000000301; N-container generalization ka-0000003561). One line per
	// captured bundle-relative spec file, value "<bundle-rel-path>|<base64>".
	// The '|' separator is unambiguous because base64 never contains it and a
	// bundle-relative path never does either. Repeated across sandbox + every
	// sub-container (agent + DinD/Postgres sidecars), so the count of spec files
	// scales with ContainerCount instead of the old four fixed FT_SPEC_* keys.
	keySpecFile = "FT_SPEC"

	// keySubContainer is the repeated sub-container roster sentinel
	// (ka-0000003561). One line per RUNNING sub-container (the agent plus any
	// always-on native sidecar), value "<container-name>|<A-side-cid>". The
	// sandbox/pause root is NOT emitted here (it is the barrier root, not a
	// sub-container). len(FT_SUBC lines) + 1 == FT_CONTAINER_COUNT.
	keySubContainer = "FT_SUBC"

	keyBSandboxCID = "FT_B_SANDBOX_CID"
	keyBAgentCID   = "FT_B_AGENT_CID"
	keyBPodUID     = "FT_B_POD_UID"
	// keyBNetns is Pod B's kubelet-owned network namespace as a net:[inode]
	// string (stat -L of the resolved CNI netns bind-mount), directly comparable
	// to keyRestoredNS (readlink /proc/<sentry>/ns/net). keyBNetnsPath is the
	// filesystem path that was repointed into the restored sandbox spec
	// (ka-0000004146 fix-a); kept distinct so the netns PROOF compares inodes, not
	// a path against an inode (which never matched and would have rejected even a
	// correct restore).
	keyBNetns       = "FT_B_NETNS"
	keyBNetnsPath   = "FT_B_NETNS_PATH"
	keyRootRC       = "FT_ROOT_RC"
	keySubRC        = "FT_SUB_RC"
	keyRestoredPID  = "FT_RESTORED_PID"
	keyRestoredNS   = "FT_RESTORED_NETNS"
	keyContainersUp = "FT_CONTAINERS_RUNNING"

	// keyRestorePeakBytes is the ACTUAL peak resident memory the restored
	// container's cgroup reached during the restore, read best-effort from the
	// cgroup-v2 memory.peak POST-verify (ka-0000003398 fix C). It is a MEASUREMENT
	// hook only — surfaced on ThawResult.RestorePeakBytes and logged against the
	// modeled peak so the restore-peak multiplier can be calibrated per
	// medium/sandbox-role over time. It NEVER gates the thaw (the read runs after
	// the restore is already verified and cannot perturb it).
	keyRestorePeakBytes = "FT_RESTORE_PEAK_BYTES"

	// keyNetnsIfaces / keyNetnsDefRoute are the fix-A netns-CONTENT assertion
	// sentinels (ka-0000007713). The thaw script enters the restored sentry's OWN
	// netns and reports the non-loopback interface count (FT_NETNS_IFACES) and
	// whether a default route is present (FT_NETNS_DEFROUTE=yes|no). The inode
	// proof (keyRestoredNS == keyBNetns) only proves the sentry landed in the
	// repointed netns — NOT that it carries a network. On canary fr84 the restore
	// "verified" against a lo-only netns (CNI eth0 torn down with the placeholder
	// sandbox delete) and the rehydrate-ack gate fail-closed network-unreachable
	// 60s later. The script fails-closed (FT_ROOT_RC=128) on a lo-only netns, so
	// reaching the Go post-state check with non-zero ifaces+default-route is the
	// normal path; the Go side asserts these too as defense in depth + for a clear
	// controller-log diagnosis.
	keyNetnsIfaces   = "FT_NETNS_IFACES"
	keyNetnsDefRoute = "FT_NETNS_DEFROUTE"

	// keyCNIConfirmed / keyNetnsPinned / keyNetnsL3Reasserted are the netns-graft
	// diagnostics the thaw script emits on stdout that the controller never
	// surfaced — it logged only tail(res.Stderr,…) on a thaw failure, truncating
	// these stdout sentinels (ka-0000001790 observability gap). They localize a
	// lo-only restore failure to one of its three layers:
	//   FT_CNI_CONFIRMED=yes|no       — did Pod B's CNI ADD complete before the pin
	//                                    (Fix C gate)? "no" => the placeholder never
	//                                    had a routable netns; not an L3-strip case.
	//   FT_NETNS_PINNED=<path>|""     — did the inode bind-mount pin succeed? "" =>
	//                                    the netns may have been unmounted by the CNI
	//                                    DEL before restore (Fix B pin layer).
	//   FT_NETNS_L3_REASSERTED=yes|no — did the Fix-B (ka-0000001790) static L3
	//                                    re-apply run? "yes" + still lo-only => the
	//                                    strip is deeper than pod-side L3 (e.g. the
	//                                    host-side veth peer was torn down too); "no"
	//                                    => CNI was unconfirmed so nothing was replayed.
	//   FT_CNI_POD_IP=<cidr>|""       — the pod IP read from the AUTHORITATIVE
	//                                    committed CNI result (/var/lib/cni/results,
	//                                    matched by K8S_POD_UID, fix C ka-0000001099).
	//                                    "" => no committed result for Pod B; self-IPAM
	//                                    no-ops and the restore falls back to whatever
	//                                    the netns already carried.
	//   FT_NETNS_SELF_IPAM=yes|no     — did the Fix-C (ka-0000001099) self-IPAM apply
	//                                    that pod IP into the RESTORED sentry's OWN
	//                                    netns (the restore-target inode, decoupled from
	//                                    the sandbox-vs-pause two-inode split that made
	//                                    Fix B's sandbox-netns L3 capture no-op)? "yes" +
	//                                    still lo-only => the restore-target netns lost
	//                                    eth0 itself (deeper than L3); "no" => no
	//                                    committed CNI result IP to apply.
	// Surfaced in the netns-content failure error so the next canary is debuggable
	// without re-deriving them from a truncated stderr tail.
	keyCNIConfirmed      = "FT_CNI_CONFIRMED"
	keyCNIPodIP          = "FT_CNI_POD_IP"
	keyNetnsPinned       = "FT_NETNS_PINNED"
	keyNetnsL3Reasserted = "FT_NETNS_L3_REASSERTED"
	keyNetnsSelfIPAM     = "FT_NETNS_SELF_IPAM"

	// keyPreRestoreRoutable / keySentryDefRoute / keySentryNonLo / keySentryEgress
	// are the fix-A + fix-B sentinels (ka-0000008207 / bug ka-0000000514) for the
	// restored-SENTRY-link-only frontier b714bf11b did NOT close:
	//   FT_NETNS_PRERESTORE_ROUTABLE=yes|no — fix A: was the pinned restore-target
	//     netns routable (global IPv4 + default route) in the few hundred ms BEFORE
	//     'runsc restore' detached and setupNetwork() scraped it into the sentry? "no"
	//     => a late kubelet CNI DEL re-stripped it post-apply; the sentry will likely
	//     come up link-only (the [6c] guards then fail-close).
	//   FT_SENTRY_DEFROUTE=yes|no|unknown — fix B [6c-1]: does the restored SENTRY's
	//     OWN netstack (read via runsc exec, not the host kernel's view) carry a
	//     default route? "no" => the link-only sentry the prod-.13 fail rode; the host-
	//     side [6a] check is BLIND to it. "unknown" => the in-sentry exec flaked.
	//   FT_SENTRY_NONLO=<n> — non-loopback iface count in the sentry netstack; 0 is
	//     the same link-only signature, -1 means the exec flaked (not fail-closed).
	//   FT_SENTRY_EGRESS=ok|link-only|inconclusive|skipped — fix B [6c-2]: an in-sentry
	//     TCP dial to NATS/beads (the consumers' own endpoints). "link-only" => the dial
	//     hit ENETUNREACH/EHOSTUNREACH (the exact "network is unreachable" coop+gb hit for
	//     60s) and the restore fails-closed; "ok"/"inconclusive"/"skipped" never fail.
	// The script fails-closed FT_ROOT_RC=128 on a definitively link-only sentry, so a
	// thaw reaching the Go post-state check has a routable sentry; these are surfaced in
	// the lo-only diagnostic and asserted as defense in depth.
	keyPreRestoreRoutable = "FT_NETNS_PRERESTORE_ROUTABLE"
	keySentryDefRoute     = "FT_SENTRY_DEFROUTE"
	keySentryNonLo        = "FT_SENTRY_NONLO"
	keySentryEgress       = "FT_SENTRY_EGRESS"

	// keyThawSrcProvenance is the [6d] POST-restore thaw-state source-provenance
	// sentinel (ka-0000001208). The thaw script writes a fresh unique marker into the
	// live per-agent host source $THAWDIR (thawStateHostDir(req.AgentName)) and reads
	// it back from INSIDE the restored sentry at /run/gardenio-thaw via runsc exec:
	//   FT_THAW_SRC_PROVENANCE=ok       — the marker WAS visible in the sentry mount,
	//                                     so /run/gardenio-thaw resolves to the live
	//                                     per-agent host source (positive proof).
	//   FT_THAW_SRC_PROVENANCE=mismatch — the read RAN but the marker was ABSENT, so
	//                                     the restore re-walked the mount to a DIFFERENT
	//                                     (stale/placeholder/checkpointed 9p-gofer)
	//                                     source; the rehydrate consumers' .done acks
	//                                     would land in a dir the controller never reads
	//                                     -> both-consumer ack gate fail-closes 60s later
	//                                     (canary #7 signature). The script fails-closed
	//                                     FT_ROOT_RC=128 on this; asserted here too.
	//   FT_THAW_SRC_PROVENANCE=unknown  — the in-sentry exec flaked / $THAWDIR empty
	//                                     (legacy snapshot) — INCONCLUSIVE, never fails.
	// DEFENSE-IN-DEPTH ONLY: the [3a] pre-restore dangling-source guard + the rehydrate
	// ack are the functional gate. A mismatch should never reach the Go post-state check
	// with rc=0 (the script fail-closes first); assert it anyway for a clear controller-
	// log diagnosis. Absent (older script) / "ok" / "unknown" are NOT failures.
	keyThawSrcProvenance = "FT_THAW_SRC_PROVENANCE"

	// keyImageReaped is emitted by the post-thaw image reaper (ka-0000006968):
	// keyNodeMemAvailBytes / keyPagesStatBytes / keyMemGateOK are the pre-thaw
	// node memory-capacity probe sentinels (ka-0000000088). The probe runs BEFORE
	// Pod B is created: it reads MemAvailable from the pinned node's host
	// /proc/meminfo AND stats pages.img so the Go side has a fresh headroom
	// number to compare against Snapshot.PagesBytes (or the stat fallback for
	// legacy snapshots) and can fail-closed in milliseconds rather than
	// committing to a restore that would OOM-SIGKILL ~7s in.
	keyNodeMemAvailBytes = "FT_NODE_MEM_AVAIL_BYTES"
	keyPagesStatBytes    = "FT_PAGES_STAT_BYTES"
	keyMemGateOK         = "FT_MEM_GATE_OK"

	// FT_IMAGE_REAPED=yes once the 3.0G checkpoint image dir at snap.ImagePath has
	// been removed host-namespaced after a verified restore. The image is consumed
	// by `runsc restore` and is dead weight afterward — the live container runs off
	// the bundle overlay, not the image — but the ka-0000003702 relocation
	// deliberately put it OUTSIDE any pod-lifecycle GC, so a successful single
	// freeze→thaw cycle would otherwise leak one 3.0G image per agent on the node.
	keyImageReaped = "FT_IMAGE_REAPED"

	// keyTokenRefreshSA / keyTokenRefreshIRSA / keyTokenRefreshDone are the
	// post-thaw token refresher sentinels (bug ka-0000000518). The refresher
	// re-overlays freshly-minted (TokenRequest API) SA/IRSA token bytes into the
	// thawed agent's warm-copy token files periodically, because thaw's step-[4]
	// Pod-B teardown stops the kubelet rotating those tokens and the once-overlaid
	// warm copy expires ~1h in. Each per-mount sentinel is ok|skip|fail; DONE=yes
	// signals the script ran to completion. All best-effort — a miss leaves the
	// agent on its prior token until the next cycle, never wedges the live pod.
	keyTokenRefreshSA   = "FT_TOKREFRESH_SA"
	keyTokenRefreshIRSA = "FT_TOKREFRESH_IRSA"
	keyTokenRefreshDone = "FT_TOKREFRESH_DONE"
)

// FreezeThawManager wraps the proven path-b harness as freeze/thaw verbs.
//
// It is deliberately separate from K8sManager: K8sManager owns the steady-state
// create/delete CRUD, while FreezeThawManager owns the checkpoint/restore
// lifecycle. They share the k8s client and the same pod-shape conventions.
type FreezeThawManager struct {
	client kubernetes.Interface
	exec   NodeExecutor
	snaps  SnapshotStore
	// transport moves the heavy checkpoint image + OCI bundle + rootfs lowerdir
	// between the freeze and thaw nodes (the SnapshotTransport seam, sibling to
	// snaps). Nil is treated as the node-local no-op via the transport() accessor,
	// so a zero-value manager (or a probe/test that bypasses the constructor) is
	// safe. The constructor installs NewNodeLocalSnapshotTransport() by default;
	// SetSnapshotTransport swaps in an off-node transport (B2) without touching
	// the verbs.
	transport SnapshotTransport
	rehydr    RehydrationTrigger
	logger    *slog.Logger

	nowFunc func() time.Time

	// dialAddrResolver best-effort resolves a rehydrate-consumer host (the NATS /
	// beads service DNS name from NatsURL / BeadsHTTPAddr) to a ClusterIP literal
	// for the thaw script's [6c-2] in-sentry dial (bug ka-0000002907). A ClusterIP
	// is a cluster-global VIP the sentry netstack routes at L3 with no DNS, so a
	// link-only sentry no-routes it EHOSTUNREACH (fail-closed) instead of gaierror-ing
	// on a name (inconclusive flake-open, the #11 miss). It is a field (not a direct
	// net.LookupHost call) so unit tests inject a hermetic resolver; nil / a "" return
	// SKIPS the end-to-end dial for that target and relies on the [6c-1] route check.
	// Default is resolveDialAddrViaDNS (net.DefaultResolver from the controller pod).
	dialAddrResolver func(ctx context.Context, host string) string

	// RunscTimeout bounds each node-side runsc invocation. The node scripts
	// wrap `runsc` in `timeout $RUNSC_TIMEOUT …` because runsc C/R can hang on
	// process exit (epic constraint). A zero value uses DefaultRunscTimeout.
	RunscTimeout time.Duration

	// PodReadyTimeout bounds the wait for the placeholder Pod B to become
	// Ready. Zero uses DefaultPodReadyTimeout.
	PodReadyTimeout time.Duration

	// PodReadyPollInterval is how often Thaw polls Pod B readiness.
	// Zero uses DefaultPodReadyPollInterval.
	PodReadyPollInterval time.Duration

	// CreateRestorePodReadyTimeout bounds the wait for a FRESH
	// create-then-restore placeholder Pod B to become Ready — the warm-B-MISS
	// fallback that mints Pod B via buildPlaceholderPodB and INLINES a full cold
	// init-clone of the workspace inside the readiness window (ka-0000004449).
	// It is deliberately distinct from (and larger than) PodReadyTimeout: that
	// 180s budget governs the warm-B graft freshness checks and the Pod A
	// teardown barrier (waitPodGone), neither of which pays a clone cost, whereas
	// a repo-heavy agent's cold init-clone (e.g. a 7423-file workspace + a
	// reference repo) measured ~2m50s on prod — over the 180s agent-Ready budget,
	// so the create-path thaw timed out and cold-started BEFORE the runsc restore
	// ever ran. Scoping the larger budget to this one seam (rather than bumping
	// DefaultPodReadyTimeout globally) keeps the other Ready/teardown waits tight.
	// Zero uses DefaultCreateRestorePodReadyTimeout. The lifecycle adapter sets it
	// from cfg.ThawCreateRestorePodReadyTimeout when configured.
	CreateRestorePodReadyTimeout time.Duration

	// RehydrateDeadline bounds the post-restore rehydration hand-off
	// (ka-0000001515). Zero uses DefaultRehydrateDeadline.
	RehydrateDeadline time.Duration

	// ThawHardDeadline bounds the DETACHED critical section of a thaw — the
	// node-side barrier restore, the rehydration hand-off, the post-restore image
	// reap — once Pod B has been committed (ka-0000001355, harden-thaw). That
	// section runs on a ctx DECOUPLED from the inbound (leader-election) ctx's
	// cancellation so a controller leader-change / pod roll mid-thaw cannot abort
	// an in-flight restore+rehydrate before its real deadline (the 6.8s
	// "context canceled" confound) or cancel the Pod B teardown the caller owns
	// (orphaned on-node runsc containers — bug ka-0000004088 problem 1). The
	// inbound ctx is still honoured up to the point we commit Pod B (Pod A
	// teardown barrier, Pod B create/waitReady); only the post-commit section is
	// detached, so a roll before any on-node restore work is a clean abort.
	//
	// It is a SAFETY CAP, not the expected duration: a healthy thaw returns in
	// restore-cost + rehydrate, well under it. Zero defers to a computed cap that
	// covers one full restore (runscTimeout) + one rehydration deadline plus a
	// teardown margin (thawHardDeadline()).
	ThawHardDeadline time.Duration

	// FreezeHardDeadline bounds the whole FREEZE node exec — the runsc checkpoint
	// plus the Step-3 relocation copy plus the pre-checkpoint gofer/rootfs copies —
	// with an explicit wall-clock cap, mirroring ThawHardDeadline on the thaw side
	// (bug ka-0000004931). Every runsc call inside the freeze script is already
	// timeout-bounded (RUNSC_TIMEOUT), and Fix A now bounds the read-only host-ns
	// image probes too (HMNTT); but the MUTATING host-ns ops — the relocation cp
	// and the pre-checkpoint gofer copies of a multi-GB rootfs — are DELIBERATELY
	// left unbounded on the node (bounding a mutating copy risks a partial-copy
	// un-thawable snapshot, worse than fail-closed). This Go-side cap is the
	// disease fix for THAT class: a wedge in any unbounded node op makes Freeze()
	// fail closed (the exec ctx times out, no snapshot is persisted, Pod A stays
	// live, the next spawn cold-starts) instead of hanging the controller freeze
	// goroutine forever. It is a SAFETY CAP, not the expected duration: a healthy
	// freeze returns in checkpoint + relocation cost, well under it. Zero defers to
	// a computed cap (freezeHardDeadlineFor()) that comfortably covers one full
	// checkpoint exec (runscTimeout) plus a relocation/copy budget that SCALES with
	// the pod's container count — the relocation + gofer copies are per-container
	// work, so a multi-sidecar (DinD/Postgres) agent gets proportionally more
	// headroom than a bare agent instead of the earlier flat margin that starved it
	// (ka-0000007927). A non-zero value here is an unconditional, count-independent
	// operator override. Only meaningful when FreezeThawEnabled.
	FreezeHardDeadline time.Duration

	// ArmedGuardEnabled opts Freeze into the pre-freeze watcher-liveness guard
	// (ka-0000006667). When false (default), Freeze runs exactly as before — no
	// armed probe, zero blast radius. When true, Freeze refuses to checkpoint a
	// pod whose in-pod rehydration watchers (coop + gb) have not armed their
	// sentinels, returning ErrFreezeWatchersNotArmed so the caller cold-starts
	// instead of minting an un-thawable snapshot. The lifecycle adapter sets this
	// from cfg when freeze/thaw is enabled; default-off keeps it the rollback
	// switch (flip-off = the guard disappears), mirroring InterruptEnabled.
	ArmedGuardEnabled bool

	// RequiredArmedConsumers overrides the set of in-pod rehydration consumers
	// whose armed sentinel must be present before Freeze will checkpoint
	// (ka-0000006667). Empty uses the default {coop, gb} — the same set the
	// rehydration gate waits on. Only consulted when ArmedGuardEnabled is true.
	RequiredArmedConsumers []string

	// ArmedWait is the bounded window the pre-freeze guard WAITS for the required
	// watchers to arm before deciding (ka-0000006667 direction [A]). Zero
	// (default) keeps the single-shot probe — Freeze refuses immediately if the
	// sentinels are absent at request time. A positive value polls the sentinels
	// (at ArmedPollInterval) until every required consumer is armed (freeze
	// proceeds → thawable snapshot, deterministic post-thaw ack) or the window
	// elapses (refuse with ErrFreezeWatchersNotArmed). Only consulted when
	// ArmedGuardEnabled is true. The lifecycle adapter sets it from cfg.
	ArmedWait time.Duration

	// ArmedPollInterval is how often watchersArmedWithin re-probes the armed
	// sentinels while waiting out ArmedWait. Zero uses DefaultArmedPollInterval.
	// Only consulted when ArmedGuardEnabled is true AND ArmedWait > 0.
	ArmedPollInterval time.Duration

	// InterruptEnabled opts Freeze into the interruptible-freeze path (C2,
	// task ka-0000005834; env FREEZE_INTERRUPT_ENABLED, gated default-off in the
	// lifecycle adapter). When false, Freeze runs exactly as before: no in-flight
	// registry bookkeeping, no abort seam — today's behaviour verbatim, zero
	// blast radius. When true, Freeze registers an in-flight cancel func keyed by
	// agent before the node checkpoint exec and a concurrent AbortFreeze can
	// cancel that exec and discard the partial snapshot, leaving the still-live
	// Pod A (--leave-running) to serve an inbound message. Default-off keeps the
	// flag the rollback switch (flip-off = the abort seam disappears).
	InterruptEnabled bool

	// PostPutDelay artificially widens the window between a successful
	// SnapshotStore.Put and the finalize defer that reads the abort verdict
	// (TOCTOU test seam, task ka-0000000996; env FREEZE_POST_PUT_DELAY, default
	// 0). The abort-races-completion invariant — "a snapshot persisted
	// microseconds before an abort is discarded + its image reaped, the agent
	// stays live, never a dead agent without a thaw target" — is only unit-proven
	// with a fake executor (TestFreeze_AbortRacesCompletion's ignoreCancel path).
	// On the real substrate the post-Put window is microseconds wide, so an
	// e2e cannot reliably land an interrupt INSIDE it. A positive delay holds the
	// freeze in the just-persisted state for a bounded window so the e2e can
	// observe the snapshot ConfigMap appear, fire POST /api/v1/freeze/interrupt,
	// and prove the discard path on live infrastructure. Default 0 is exactly
	// today's behaviour — zero blast radius, never set in production. Only
	// consulted when InterruptEnabled is true (the registry/defer are inert
	// otherwise).
	PostPutDelay time.Duration

	// MemGateMargin overrides the KERNEL-headroom term of the pre-thaw memory
	// model (ka-0000000088). Zero or negative uses defaultMemGateMargin (512
	// MiB). Test seam: unit tests can inject a small positive margin (or zero)
	// so the arithmetic is verifiable without allocating gigabytes. NOTE: since
	// ka-0000007579 this is one term of the fattened modeledRestoreNeed, not the
	// only slack over pages.img.
	MemGateMargin int64

	// MemGateRestoreMultiplierPermille overrides the restore-peak multiplier the
	// gate applies to Snapshot.PagesBytes, in PER-MILLE (1500 == 1.5×;
	// ka-0000007579). A value < 1000 (modeling the restore as needing less than
	// the image it holds resident — never fail-safe) uses
	// defaultMemGateRestoreMultiplierPermille (1500). Test seam so the model
	// arithmetic is verifiable with small pages values.
	MemGateRestoreMultiplierPermille int64

	// MemGateSentryOverhead overrides the PER-CONTAINER sentry+gofer +
	// restored-working-set term of the pre-thaw memory model, in bytes
	// (ka-0000007579). Zero or negative uses defaultMemGateSentryOverhead (768
	// MiB). Multiplied by max(Snapshot.ContainerCount, baseline). Test seam.
	MemGateSentryOverhead int64

	// MemGateDisabled skips the pre-thaw node memory-capacity probe
	// (ka-0000000088) entirely — the barrier restore fires without a
	// pre-flight node exec. Default false is the production path (gate
	// engaged). Test seam only: the pre-existing thaw test suite indexes
	// exec.scripts[0] as the barrier-restore script and would break if a
	// mem-gate exec landed there, so newFreezeThawManager sets this to
	// true for the legacy tests. Every gate-behavior test (see
	// freezethaw_thaw_memgate_test.go) explicitly clears it.
	MemGateDisabled bool

	// NatsURL / BeadsHTTPAddr are the rehydrate consumers' egress endpoints used
	// by the thaw script's [6c] in-sentry egress assertion (fix B, ka-0000008207 /
	// bug ka-0000000514). They are the SAME endpoints coop (NATS) and gb (beads
	// daemon) dial from inside the restored sandbox, so the in-sentry TCP probe
	// exercises the exact paths that fail-closed the rehydrate-ack gate on prod .13.
	// The lifecycle adapter sets these from cfg.NatsURL / cfg.BeadsHTTPAddr. Empty
	// leaves the script's end-to-end dial OFF (the in-sentry default-route check
	// still runs, target-free) — so an unset endpoint degrades to the link-only
	// route detector rather than skipping the floor entirely. Parsed to host:port
	// by buildThawScript; a malformed value just disables the dial for that target.
	NatsURL       string
	BeadsHTTPAddr string

	// inflight tracks freezes currently in their node-checkpoint exec, keyed by
	// agent name, so a concurrent AbortFreeze can cancel the right one. Only
	// populated when InterruptEnabled is true. The entry is removed by Freeze's
	// finalize step under inflightMu, which is also where the abort-vs-completion
	// TOCTOU is resolved (the aborted flag is read under the same lock).
	inflightMu sync.Mutex
	inflight   map[string]*inflightFreeze

	// thawInflight tracks thaws currently in flight (from the top of Thaw
	// through its bounded rehydration hand-off), keyed by agent name with a
	// refcount. The reconciler's terminal-pod reaper consults IsThawing to
	// exempt the restored placeholder Pod B while its thaw is still rehydrating:
	// Pod B reports phase=Failed within seconds of the restore graft, and a
	// reconcile pass inside the rehydration window would otherwise DeleteAgentPod
	// it out from under the in-pod watchers — tearing down the netns so both
	// acks fail ENETUNREACH (bug ka-0000002058, decision ka-0000005300:
	// exempt_reaper). The thaw lifecycle already owns Pod B teardown on both the
	// success and fail-closed paths, so the reaper must stand off while a thaw
	// holds the slot. Always populated (no flag gate): the manager only exists
	// when cfg.FreezeThawEnabled is on, and an empty registry reports false.
	thawInflightMu sync.Mutex
	thawInflight   map[string]int

	// thawSingleflight serializes Thaw calls per agent so a second spawn event,
	// reconciler create-missing pass, or post-leader-handoff replay that races
	// an in-flight thaw JOINS the first restore instead of starting a second
	// `runsc restore` of the same snapshot (DEDUP, design ka-0000005125). Two
	// concurrent thaws would mint two Pod Bs contending for the same node-local
	// hostPath / sandbox CID family — the kubelet admit-fail signature
	// (ka-0000005680) at best, two live duplicates at worst (ka-0000007549
	// class). The upstream guards (single-threaded event loop, spawnInFlight,
	// reconciler snapshot-exemption) reduce the RATE of duplicate spawn events;
	// this single-flight makes a duplicate that slips through HARMLESS by making
	// "exactly one restore per agent per wake" an invariant of the manager. The
	// first caller runs the restore and owns the entry; a second caller for the
	// same agent blocks on the entry's done channel and returns the first thaw's
	// shared (*ThawResult, error). Like thawInflight, no flag gate — the manager
	// only exists when FreezeThawEnabled is on.
	thawSingleMu     sync.Mutex
	thawSingleflight map[string]*inflightThaw
}

// inflightThaw is the per-agent single-flight entry: the first Thaw caller for
// an agent creates it and runs the restore; concurrent callers for the same
// agent block on done and read the shared result/err after it is closed (the
// close happens-after the owner sets result/err, so the read is race-free).
type inflightThaw struct {
	done   chan struct{}
	result *ThawResult
	err    error
}

// inflightFreeze is the per-agent registry entry for an in-flight freeze (C2).
// cancel aborts the freeze's node-checkpoint exec ctx; aborted records that an
// AbortFreeze fired, so Freeze's finalize step can discard a snapshot even if it
// raced to completion microseconds before the abort.
type inflightFreeze struct {
	cancel  context.CancelFunc
	aborted bool
}

// Freeze/thaw default tunables. All runsc-touching steps are bounded so a hung
// checkpoint/restore surfaces as a timeout error rather than wedging the
// controller goroutine.
const (
	DefaultRunscTimeout         = 120 * time.Second
	DefaultPodReadyTimeout      = 180 * time.Second
	DefaultPodReadyPollInterval = 2 * time.Second
	DefaultRehydrateDeadline    = 60 * time.Second

	// DefaultCreateRestorePodReadyTimeout bounds the Ready-wait for a FRESH
	// create-then-restore placeholder Pod B (ka-0000004449). Unlike the warm-B
	// graft, this path inlines a full cold init-clone of the workspace volume into
	// the readiness window (the gVisor checkpoint restores the agent rootfs
	// overlay but NOT the separate VolumeWorkspace repo mount, so init-clone must
	// re-hydrate it before the placeholder agent container — and thus Pod
	// readiness — comes up; see buildPlaceholderPodB). A repo-heavy cold clone
	// measured ~2m50s on prod, over the 180s DefaultPodReadyTimeout, so the
	// create-path thaw timed out and cold-started before the runsc restore ran.
	// 600s comfortably covers a repo-heavy cold clone + the post-clone agent-Ready
	// boot with margin, while still bounding a genuinely wedged clone. The
	// downstream barrier restore + rehydration run on their own ThawHardDeadline
	// budget, so a generous Ready-wait here does not uncap the critical section.
	DefaultCreateRestorePodReadyTimeout = 600 * time.Second

	// DefaultArmedPollInterval is how often the pre-freeze armed guard re-probes
	// the watcher sentinels while waiting out ArmedWait (ka-0000006667 direction
	// [A]). Matches thawepoch.DefaultPollInterval — the same cadence the
	// post-thaw rehydration gate reads acks at — so the freeze-side wait and the
	// thaw-side ack observe arming at the same granularity.
	DefaultArmedPollInterval = time.Second

	// thawTeardownMargin pads the computed thawHardDeadline() above the inner
	// restore + rehydrate budgets so the detached critical section has slack to
	// run its own Pod B teardown / image reap after a fail-closed rehydration
	// before the hard cap fires (ka-0000001355, harden-thaw).
	thawTeardownMargin = 30 * time.Second

	// freezeBaselineContainerCount is the checkpoint statefile's FT_CONTAINER_COUNT
	// for a BARE gVisor agent pod: the pause sandbox (1) + the agent sub-container
	// (1). Multi-sidecar agents (DinD/Postgres always-on native sidecars) run ABOVE
	// this. It is the floor the relocation copy budget is sized against, so a bare
	// agent's computed freeze cap stays at today's 240s (no regression) while heavier
	// pods scale up (ka-0000007927).
	freezeBaselineContainerCount = 2

	// freezeRelocationPerContainerMargin is the copy-budget headroom the computed
	// freezeHardDeadlineFor() grants PER checkpointed container for the UNBOUNDED
	// mutating node copies the freeze exec runs — the Step-3 relocation cp of the
	// (post-checkpoint) image base layers and the pre-checkpoint /work,/tmp,.gofer
	// gofer copies, all billed against this cap (bugs ka-0000004931, ka-0000005425).
	// Every always-on sidecar (DinD, Postgres) adds its OWN rootfs relocation +
	// overlay, so the copy cost scales with ContainerCount; the earlier FLAT 120s
	// margin therefore gave the heaviest multi-sidecar agents the LEAST headroom —
	// precisely the agents warm-resume most benefits, and the ones most likely to
	// hit the cap and cold-start despite being thawable (ka-0000007927).
	//
	// Derived from a conservative COPY BUDGET rather than a magic flat number:
	// ~2.4 GB of assumed per-container relocation payload (a sidecar rootfs overlay
	// is a few GB) ÷ a ~40 MB/s worst-case NON-reflink host-ns cp floor ≈ 60s. Real
	// copies run far faster, and the reflink fast-path (ka-0000005425, merged) makes
	// a CoW-node copy O(metadata) — this is only the backstop rate the cap must
	// tolerate, never the SLA. The operator FREEZE_HARD_DEADLINE override tightens it.
	freezeRelocationPerContainerMargin = 60 * time.Second

	// freezeRelocationMargin is the BASELINE relocation copy budget — the margin a
	// bare (count=2) agent's computed freezeHardDeadline() carries above one
	// runsc-checkpoint budget. Kept as a named constant (= per-container * baseline
	// = 120s) so the default cap stays 240s and the derivation reads cleanly; the
	// count-aware freezeHardDeadlineFor() scales ABOVE this for multi-sidecar pods.
	freezeRelocationMargin = freezeRelocationPerContainerMargin * freezeBaselineContainerCount
)

// NewFreezeThawManager constructs a freeze/thaw manager. exec, snaps, and rehydr
// are required collaborators; rehydr may be nil to skip the rehydration
// hand-off (e.g. when the caller drives rehydration itself).
func NewFreezeThawManager(client kubernetes.Interface, exec NodeExecutor, snaps SnapshotStore, rehydr RehydrationTrigger, logger *slog.Logger) *FreezeThawManager {
	if logger == nil {
		logger = slog.Default()
	}
	return &FreezeThawManager{
		client: client,
		exec:   exec,
		snaps:  snaps,
		// Default to the node-local no-op transport so behaviour is unchanged
		// until an off-node transport (B2) is injected via SetSnapshotTransport.
		transport:        NewNodeLocalSnapshotTransport(),
		rehydr:           rehydr,
		logger:           logger,
		nowFunc:          time.Now,
		dialAddrResolver: resolveDialAddrViaDNS,
		inflight:         map[string]*inflightFreeze{},
		thawInflight:     map[string]int{},
		thawSingleflight: map[string]*inflightThaw{},
	}
}

// SetSnapshotTransport swaps the SnapshotTransport used to move the heavy
// checkpoint image + OCI bundle between the freeze and thaw nodes. It follows the
// same post-construction override idiom as the tunable fields (RunscTimeout,
// NatsURL, …): the constructor installs the node-local no-op default, and the
// lifecycle adapter (or a test) calls this to install an off-node transport
// (S3 / criu-image-streamer, B2). A nil argument restores the node-local no-op
// rather than leaving the manager without a transport.
func (f *FreezeThawManager) SetSnapshotTransport(t SnapshotTransport) {
	if t == nil {
		t = NewNodeLocalSnapshotTransport()
	}
	f.transport = t
}

// transport returns the configured SnapshotTransport, falling back to a
// node-local no-op when the field is nil. This keeps a zero-value manager, and
// any probe/test that constructs the struct without the constructor, safe: the
// default preserves today's node-local behaviour.
func (f *FreezeThawManager) transportOrDefault() SnapshotTransport {
	if f.transport == nil {
		return NewNodeLocalSnapshotTransport()
	}
	return f.transport
}

// resolveDialAddrViaDNS is the default dialAddrResolver: it resolves a service host
// (a bare name like "gardenio-nats" or an FQDN, from NatsURL / BeadsHTTPAddr) to its
// ClusterIP literal via DNS from the controller pod, for the thaw script's [6c-2]
// in-sentry dial (bug ka-0000002907). If host is already an IP it is returned verbatim;
// on any resolution failure it returns "" so the dial SKIPS that target and the
// [6c-1] in-sentry default-route check remains the load-bearing floor. Prefers an
// IPv4 result (the sentry netstack + AWS VPC CNI are IPv4). Bounded to 3s so a slow /
// broken resolver never stalls the thaw critical section.
func resolveDialAddrViaDNS(ctx context.Context, host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}
	if net.ParseIP(host) != nil {
		return host // already a literal (an IP endpoint) — dial it directly.
	}
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIPAddr(rctx, host)
	if err != nil || len(ips) == 0 {
		return ""
	}
	// Prefer IPv4 (the pod netstack + AWS VPC CNI are v4); fall back to the first.
	for _, ip := range ips {
		if v4 := ip.IP.To4(); v4 != nil {
			return v4.String()
		}
	}
	return ips[0].IP.String()
}

// resolveEgressDialAddr resolves one egress host to its ClusterIP literal via the
// manager's (injectable) resolver, returning "" (skip the dial) when the host is
// empty or the resolver is unset/failed. Kept as a method so Thaw threads the
// controller's own NatsURL/BeadsHTTPAddr through the same seam the tests stub.
func (f *FreezeThawManager) resolveEgressDialAddr(ctx context.Context, host string) string {
	if host == "" || f.dialAddrResolver == nil {
		return ""
	}
	return f.dialAddrResolver(ctx, host)
}

func (f *FreezeThawManager) now() time.Time {
	if f.nowFunc != nil {
		return f.nowFunc()
	}
	return time.Now()
}

func (f *FreezeThawManager) runscTimeout() time.Duration {
	if f.RunscTimeout > 0 {
		return f.RunscTimeout
	}
	return DefaultRunscTimeout
}

func (f *FreezeThawManager) podReadyTimeout() time.Duration {
	if f.PodReadyTimeout > 0 {
		return f.PodReadyTimeout
	}
	return DefaultPodReadyTimeout
}

func (f *FreezeThawManager) podReadyPollInterval() time.Duration {
	if f.PodReadyPollInterval > 0 {
		return f.PodReadyPollInterval
	}
	return DefaultPodReadyPollInterval
}

// createRestorePodReadyTimeout is the Ready-wait budget for a FRESH
// create-then-restore placeholder Pod B (ka-0000004449). It must cover the cold
// init-clone inlined into Pod B's readiness window, so it is larger than
// podReadyTimeout (which governs the no-clone warm-B/teardown waits). Zero falls
// back to DefaultCreateRestorePodReadyTimeout.
func (f *FreezeThawManager) createRestorePodReadyTimeout() time.Duration {
	if f.CreateRestorePodReadyTimeout > 0 {
		return f.CreateRestorePodReadyTimeout
	}
	return DefaultCreateRestorePodReadyTimeout
}

func (f *FreezeThawManager) rehydrateDeadline() time.Duration {
	if f.RehydrateDeadline > 0 {
		return f.RehydrateDeadline
	}
	return DefaultRehydrateDeadline
}

func (f *FreezeThawManager) armedPollInterval() time.Duration {
	if f.ArmedPollInterval > 0 {
		return f.ArmedPollInterval
	}
	return DefaultArmedPollInterval
}

// RehydrateDeadlineEffective is the exported view of the effective rehydration
// deadline (the field when set, else DefaultRehydrateDeadline). The lifecycle
// startup log uses it so the reported value matches what the gate actually
// enforces — a raw 0 field is a red herring (bug ka-0000007916).
func (f *FreezeThawManager) RehydrateDeadlineEffective() time.Duration {
	return f.rehydrateDeadline()
}

// CreateRestorePodReadyTimeoutEffective is the exported view of the effective
// create-then-restore Pod B Ready-wait budget (the field when set, else
// DefaultCreateRestorePodReadyTimeout). The lifecycle startup log uses it so the
// reported value matches what the create-path wait actually enforces — a raw 0
// field is a red herring (mirrors RehydrateDeadlineEffective, bug ka-0000007916).
func (f *FreezeThawManager) CreateRestorePodReadyTimeoutEffective() time.Duration {
	return f.createRestorePodReadyTimeout()
}

// FreezeHardDeadlineEffective is the exported view of the BASELINE effective
// freeze hard-deadline: the operator override when set to a positive value, else
// the count-aware computed cap evaluated at freezeBaselineContainerCount (a bare
// pause+agent pod). The lifecycle startup log echoes it so the reported value
// matches what a baseline freeze actually enforces — a raw 0 FreezeHardDeadline
// field is a red herring (it means "compute the default", not "no deadline"),
// exactly like RehydrateDeadlineEffective/CreateRestorePodReadyTimeoutEffective
// (bug ka-0000007916). Note the LIVE freeze path sizes the cap per-pod via
// freezeHardDeadlineFor(estimateFreezeContainerCount) — a multi-sidecar agent's
// actual cap is >= this baseline — so this is the floor the operator sees at
// boot, not the exact cap every freeze uses (task ka-0000006783).
func (f *FreezeThawManager) FreezeHardDeadlineEffective() time.Duration {
	return f.freezeHardDeadline()
}

// thawHardDeadline is the safety cap on the detached post-commit critical
// section of a thaw (barrier restore + rehydration + reap + teardown margin).
// When ThawHardDeadline is unset, it is derived to comfortably cover one full
// restore exec (runscTimeout) + one rehydration deadline + a teardown margin, so
// the default never truncates a healthy thaw — it only backstops a wedged node
// exec or a watcher that never acks, which the inner runscTimeout/rehydrate
// deadlines already bound. See ThawHardDeadline.
func (f *FreezeThawManager) thawHardDeadline() time.Duration {
	if f.ThawHardDeadline > 0 {
		return f.ThawHardDeadline
	}
	return f.runscTimeout() + f.rehydrateDeadline() + thawTeardownMargin
}

// freezeHardDeadline is the count-unaware baseline safety cap — the derived cap a
// BARE agent pod (pause + agent, freezeBaselineContainerCount) carries. It is a
// thin wrapper over freezeHardDeadlineFor(baseline) kept for callers/tests that
// reason about the default without a pod shape. The live Freeze path uses the
// count-aware freezeHardDeadlineFor() so multi-sidecar agents get proportional
// headroom. See freezeHardDeadlineFor and FreezeHardDeadline.
func (f *FreezeThawManager) freezeHardDeadline() time.Duration {
	return f.freezeHardDeadlineFor(freezeBaselineContainerCount)
}

// freezeHardDeadlineFor is the safety cap on the whole freeze node exec — runsc
// checkpoint + Step-3 relocation copy + pre-checkpoint gofer/rootfs copies (bugs
// ka-0000004931, ka-0000005425) — SIZED to the pod's container count. It mirrors
// thawHardDeadline: the inner RUNSC_TIMEOUT bounds each runsc call and Fix A's
// HMNTT bounds the read-only image probes, but the MUTATING host-ns copies are
// left unbounded on the node (bounding a mutating copy risks a partial-copy
// un-thawable snapshot). This cap ensures a wedge in ANY of those unbounded ops
// surfaces as a timeout on the freeze exec ctx — Freeze() then fails closed (no
// snapshot persisted, Pod A left live) instead of hanging the freeze goroutine
// forever.
//
// When FreezeHardDeadline is unset, the cap is derived to comfortably cover one
// full checkpoint exec (runscTimeout) plus a relocation/copy budget that SCALES
// with containerCount: runscTimeout + freezeRelocationPerContainerMargin * count.
// The relocation + gofer copies are per-container work (each always-on sidecar
// relocates its own rootfs overlay + gofer trees), so the earlier FLAT margin
// gave the heaviest multi-sidecar agents the LEAST headroom and cold-started the
// very pods warm-resume most benefits (ka-0000007927). count is floored at
// freezeBaselineContainerCount so the bare-agent default stays exactly 240s (no
// regression) and a degenerate below-baseline estimate can never truncate the
// cap. The explicit FreezeHardDeadline operator override wins unconditionally and
// is count-independent — the escape hatch to pin a tighter/looser cap. See
// FreezeHardDeadline.
func (f *FreezeThawManager) freezeHardDeadlineFor(containerCount int) time.Duration {
	if f.FreezeHardDeadline > 0 {
		return f.FreezeHardDeadline
	}
	if containerCount < freezeBaselineContainerCount {
		containerCount = freezeBaselineContainerCount
	}
	return f.runscTimeout() + freezeRelocationPerContainerMargin*time.Duration(containerCount)
}

// estimateFreezeContainerCount derives a PRE-EXEC estimate of the checkpoint
// statefile's FT_CONTAINER_COUNT (pause sandbox + agent + always-on sidecars)
// from the live Pod A spec, so the freeze hard deadline can be sized to the pod
// shape BEFORE the node exec runs (the authoritative count is only baked into the
// statefile the exec produces). The count is 1 (pause sandbox) + the app
// containers + the always-on native sidecars (DinD/Postgres — init containers
// with restartPolicy: Always, the same set partitionInitContainers keeps for the
// N-container barrier restore). Run-to-completion init containers (init-clone)
// are correctly excluded: they exit before the checkpoint and are not part of the
// running sandbox's container_count. Returns freezeBaselineContainerCount for a
// nil/empty spec so the estimate never falls below the bare-agent floor.
func estimateFreezeContainerCount(spec corev1.PodSpec) int {
	_, sidecars := partitionInitContainers(spec.InitContainers)
	count := 1 + len(spec.Containers) + len(sidecars) // 1 = pause sandbox
	if count < freezeBaselineContainerCount {
		return freezeBaselineContainerCount
	}
	return count
}

// HasSnapshot reports whether a THAWABLE snapshot exists for the agent. The
// controller's wake path uses this to choose thaw (a prior freeze exists) over
// a cold-start CreateAgentPod. A store error is treated as "no snapshot" — the
// caller falls back to cold start, which is always safe. A CONSUMED snapshot
// (ka-0000001878) is also reported as absent: its image was reaped at thaw, so a
// thaw would fail-close (FT_IMAGE_OK=no) and the next spawn must cold-start
// directly — the path-b cycle is single-shot.
func (f *FreezeThawManager) HasSnapshot(ctx context.Context, agentName string) bool {
	if agentName == "" {
		return false
	}
	snap, err := f.snaps.Get(ctx, agentName)
	return err == nil && !snap.Consumed
}

// existingSnapshot returns the agent's currently-persisted snapshot, if any. A
// store miss (or empty agent name) yields (nil, false). Used by the
// duplicate-Freeze idempotency guard (ka-0000000194) to decide whether a second
// checkpoint would clobber a good snapshot.
func (f *FreezeThawManager) existingSnapshot(ctx context.Context, agentName string) (*Snapshot, bool) {
	if agentName == "" {
		return nil, false
	}
	snap, err := f.snaps.Get(ctx, agentName)
	if err != nil {
		return nil, false
	}
	return &snap, true
}

// podGoneOrTerminating reports whether the source Pod A is no longer a live,
// checkpointable target: either the Get errored (pod absent/unreadable) or the
// pod carries a DeletionTimestamp (already Terminating). A nil error + nil
// DeletionTimestamp means the pod is still live and a fresh checkpoint is sound.
func podGoneOrTerminating(pod *corev1.Pod, getErr error) bool {
	if getErr != nil || pod == nil {
		return true
	}
	return pod.DeletionTimestamp != nil
}

// SnapshotNode returns the node that holds the agent's node-local checkpoint
// image, or ("", false) when no snapshot exists (or it carries no node). The
// C2 wake path (ka-0000005266) uses it to claim a warm placeholder co-resident
// with the checkpoint: a path-b thaw is node-local, so a warm placeholder can
// only serve a thaw whose snapshot lives on the same node. A store miss or an
// empty node yields ("", false) and the caller takes the create-then-restore
// path, which is always correct.
func (f *FreezeThawManager) SnapshotNode(ctx context.Context, agentName string) (string, bool) {
	if agentName == "" {
		return "", false
	}
	snap, err := f.snaps.Get(ctx, agentName)
	if err != nil || snap.Node == "" || snap.Consumed {
		return "", false
	}
	return snap.Node, true
}

// WarmBucket describes one topology bucket the warm-B pool should keep warm: a
// node, the snapshot topology key, and a representative snapshot whose
// PodTemplate produces a graft-compatible placeholder for that bucket
// (ka-0000002219). Many agents can share a bucket (same project+role+repo
// shape); the pool only needs ONE template per (node, topology) to mint
// placeholders any of them can be grafted onto.
type WarmBucket struct {
	Node     string
	Topology string
	Snapshot Snapshot
}

// SnapshotTopology returns the topology bucket key for an agent's snapshot, or
// ("", false) when no thawable snapshot exists. The wake path uses it to claim
// only a warm placeholder whose LabelWarmTopology matches — so a checkpoint is
// never grafted onto a mount-shape- or repo-incompatible placeholder. A CONSUMED
// snapshot (ka-0000001878) reports ("", false): its single-shot cycle completed
// and the next spawn cold-starts, so no warm-graft target should be sought.
func (f *FreezeThawManager) SnapshotTopology(ctx context.Context, agentName string) (string, bool) {
	if agentName == "" {
		return "", false
	}
	snap, err := f.snaps.Get(ctx, agentName)
	if err != nil || snap.Consumed {
		return "", false
	}
	return SnapshotTopologyKey(snap), true
}

// WarmBuckets enumerates the distinct (node, topology) buckets across all
// persisted snapshots, each with a representative snapshot template. The warm-B
// pool reconcile uses this to decide which topology-aware placeholders to keep
// warm: only a shape that some snapshot actually has can ever be claimed, so the
// pool never wastes capacity on a bucket no agent will thaw into. Snapshots with
// no node are skipped (a path-b thaw is node-local). CONSUMED snapshots
// (ka-0000001878) are also skipped: their image was reaped at thaw, so no live
// thaw will ever claim a placeholder for that bucket — keeping one warm would
// leak pool capacity for a single-shot cycle that already completed.
func (f *FreezeThawManager) WarmBuckets(ctx context.Context) ([]WarmBucket, error) {
	snaps, err := f.snaps.List(ctx)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(snaps))
	out := make([]WarmBucket, 0, len(snaps))
	for _, snap := range snaps {
		if snap.Node == "" || snap.Consumed {
			continue
		}
		topo := SnapshotTopologyKey(snap)
		k := snap.Node + "|" + topo
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, WarmBucket{Node: snap.Node, Topology: topo, Snapshot: snap})
	}
	return out, nil
}

// NodeExecResult is the outcome of a single node-side script invocation.
type NodeExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// NodeExecutor runs a script on a specific node, with host-init access (the
// privileged probe/DaemonSet that chroots /host and reaches the containerd
// runsc root). In production this is a client-go remotecommand exec into the
// probe pod scheduled on that node (see ProbeNodeExecutor); in tests it is a
// fake that returns canned output.
//
// The script is run under `bash -s` on the node's host filesystem, streamed
// over stdin so it is not subject to the MAX_ARG_STRLEN single-argv limit
// (bug ka-0000004900 — the multi-container thaw body crosses 128 KiB). The
// implementation is responsible for selecting the probe pod that lives on
// `node`. Exec returns a non-nil error only for transport/exec failures; a
// non-zero script exit is reported via NodeExecResult.ExitCode so callers can
// apply their own post-state assertions.
type NodeExecutor interface {
	Exec(ctx context.Context, node, script string) (NodeExecResult, error)
}

// SnapshotContainer identifies one running sub-container captured at freeze: its
// CRI container-name (e.g. "agent", "dind", "postgres") and its A-side runsc cid.
// The agent plus any always-on native sidecars are sub-containers of the sandbox;
// the count-gated barrier restore fires one `runsc restore` per SnapshotContainer
// (on top of the sandbox root). Thaw pairs each captured OCI bundle to Pod B's
// freshly-resolved cid BY Name (gVisor matches a checkpointed sub-container to a
// restore call via its container-name annotation, not the cid), so CID here is
// for diagnostics and the fail-closed count cross-check, not for driving restore.
type SnapshotContainer struct {
	Name string // CRI container-name (the per-sub-container bundle subdir)
	CID  string // A-side runsc sub-container cid (diagnostic)
}

// Snapshot is the persisted record of a frozen agent, keyed by agent identity.
// It carries everything Thaw needs to reconstruct the restore on the same node:
// the node-local image path, the A-side cids/pod-uid to rewrite out, the baked
// container_count (the barrier target), and the Pod A template used to mint the
// placeholder Pod B with an identical mount shape.
type Snapshot struct {
	AgentName string
	BeadID    string
	Namespace string
	PodName   string

	// Node is the node that holds the node-local checkpoint image. Path-b thaw
	// is node-local (see "remaining work: snapshot transport" in
	// restore-through-lifecycle.md); the placeholder Pod B is pinned here.
	Node string

	// ImagePath is the node-local directory holding checkpoint.img / pages.img
	// / pages_meta.img written by `runsc checkpoint`.
	ImagePath string

	// BundlePath is the node-local directory holding the preserved {sandbox,agent}
	// OCI bundles (with re-mounted rootfs overlays) the restore reads.
	BundlePath string

	// A-side identifiers that every kubelet/containerd bind source is keyed by.
	// Thaw rewrites these to Pod B's fresh equivalents. SandboxCID + PodUID are
	// pod-wide (every sub-container's bind sources carry them); AgentCID is the
	// agent sub-container's cid, kept for back-compat and diagnostics (the full
	// per-sub-container cid list lives in SubContainers).
	SandboxCID string
	AgentCID   string
	PodUID     string

	// SubContainers lists the running sub-containers captured at freeze — the
	// agent plus any always-on native sidecars (DinD/Postgres) — each by its CRI
	// container-name and A-side runsc cid. The count-gated barrier restore drives
	// one `runsc restore` per entry (plus the sandbox root), so
	// len(SubContainers)+1 == ContainerCount. Thaw does NOT reuse the A-side cids
	// to drive the restore (it resolves Pod B's fresh cids on the node and pairs
	// each to the captured OCI bundle BY container-name — gVisor matches a
	// checkpointed sub-container to a restore call by its container-name
	// annotation, not its cid); they are recorded for diagnostics and the
	// fail-closed count cross-check. Captured per-sidecar bundle + overlay-opts
	// ride in SpecFiles keyed by "<name>/config.json" / "<name>/.overlay-opts".
	SubContainers []SnapshotContainer

	// ContainerCount is the barrier target baked into the statefile metadata
	// (ContainerCountKey); for a bare agent pod this is 2 (pause + agent), and
	// 3–4 for agents with DinD and/or Postgres sidecars.
	ContainerCount int

	// PagesBytes is the size of the runsc pages.img the restore mmaps EAGERLY
	// (ka-0000000088). Captured at freeze from `stat -c %s` and persisted so both
	// the pre-thaw node memory-capacity gate (thawOnce compares against
	// MemAvailable on the pinned node — a c6a.large's ~3.05Gi allocatable
	// STRUCTURALLY cannot fit a 3.0G restore) and the warm-B placeholder memory
	// reservation (shapePlaceholderSpec sets the placeholder REQUEST to the pages
	// bytes so the freed subject RAM is fenced by the scheduler across freeze→thaw)
	// have the number they need without a second node probe. Zero (pre-existing
	// snapshots, freeze script older than ka-0000000088, best-effort stat failure)
	// means "not captured" — the pre-thaw gate falls back to node-side stat on
	// pages.img directly, and the placeholder request falls back to the 256Mi
	// admission floor. Never gates the freeze itself.
	PagesBytes int64

	// OverlayMedium is the runsc rootfs-overlay medium the checkpoint was taken
	// under (overlay-dir-medium, ka-0000007239) — e.g. "root:dir=/var/lib/gardenio/
	// overlay" on the dir-medium gVisor pool, or "" for the default root:self. The
	// freeze node probe reads it off the live sandbox boot args (FT_OVERLAY_MEDIUM)
	// so the SNAPSHOT SELF-DESCRIBES its medium (plan Step 4-A): thaw replays the
	// SAME --overlay2 to every `runsc restore` (a root:dir checkpoint can only
	// restore under root:dir). The rootfs-overlay upper rides INSIDE checkpoint.img
	// (the dir filestore is created-then-unlinked — substrate revalidation
	// ka-0000007329), so nothing extra is preserved; only the medium must match.
	// Empty leaves old self-medium snapshots — and self-medium nodes — untouched.
	OverlayMedium string

	// PodTemplate is a trimmed copy of Pod A's spec used to recreate the
	// placeholder Pod B with an identical mount shape (emptyDir /work, projected
	// SA token, resolv.conf/hosts) so the restored container's mount
	// destinations line up. Container commands are overridden to a sleep
	// placeholder at thaw time.
	PodTemplate corev1.PodSpec

	// SpecFiles carries the small OCI-bundle spec files captured at freeze,
	// keyed by their bundle-relative path (e.g. "sandbox/config.json",
	// "agent/.overlay-opts"). Fix B (ka-0000000301): the host OCI bundle dir
	// (BundleHostBase/<agent> = /var/lib/gardenio/bundle/<agent>; formerly
	// /var/tmp/gb-bundle/<agent> before the ka-0000003702 relocation) could be
	// reaped before thaw — the original vector was Pod A's post-freeze teardown
	// GC'ing the then-/var/tmp bundle dir (containerd `runsc delete` reaps the
	// per-container bundle dir) while the standalone checkpoint image survives,
	// so thaw FetchSpec-ENOENTs (rc=128) on config.json. By persisting these
	// bytes in the snapshot and
	// re-materializing them into the host bundle at thaw start, thaw no longer
	// depends on the bundle dir surviving teardown at all — robust against ANY
	// deletion/GC vector, not just the Pod-A-teardown reap. Values are the raw
	// file bytes; rootfs (the large overlay) is reconstructed separately from
	// .overlay-opts, not carried here.
	SpecFiles map[string][]byte

	CreatedAt time.Time

	// Consumed marks a snapshot whose single freeze→thaw cycle is COMPLETE: a
	// later thaw verified the restore and reaped the 3.0G checkpoint image
	// (Step 7, ka-0000006968), but the SNAPSHOT RECORD survives (its bundle at
	// BundlePath now backs the live Pod B). A path-b checkpoint is node-local and
	// single-shot — re-freezing a thawed agent (a second node-local checkpoint
	// reusing /var/lib/gardenio/{ckpt,bundle}/<agent>) is the snapshot-transport
	// follow-up's concern, NOT this PoC's (restore-through-lifecycle.md "remaining
	// work"). So once a snapshot is consumed it is structurally un-thawable (its
	// image is gone) and must NOT drive another thaw or hold warm-B capacity:
	//   - Freeze refuses a consumed snapshot (ErrFreezeAlreadyThawed) and discards
	//     it, so a graceful re-stop of a thawed agent degrades to a clean cold
	//     start (ka-0000001878).
	//   - HasSnapshot / SnapshotNode / SnapshotTopology / WarmBuckets treat a
	//     consumed snapshot as ABSENT, so the next spawn cold-starts directly
	//     instead of attempting a doomed thaw against the reaped image, and the
	//     warm-B pool wastes no placeholder on a bucket no live thaw will claim.
	// A subsequent genuine freeze of a fresh cold-started pod under the canonical
	// name Put-overwrites the consumed record with Consumed=false, re-opening the
	// agent's single-cycle freeze/thaw for that new generation.
	Consumed bool
}

// SnapshotStore persists and retrieves snapshots keyed by agent identity. The
// default is in-memory (InMemorySnapshotStore); a node-local or S3-backed store
// (the criu-image-streamer transport, restore-through-lifecycle.md "remaining
// work" #2) can be substituted without touching the verbs.
type SnapshotStore interface {
	Put(ctx context.Context, snap Snapshot) error
	Get(ctx context.Context, agentName string) (Snapshot, error)
	// List returns every persisted snapshot. The warm-B pool (ka-0000002219)
	// uses it to enumerate the topology buckets it must keep warm: a snapshot's
	// PodTemplate is the only thing that can produce a graft-compatible
	// placeholder, so the pool builds placeholders FROM existing snapshots rather
	// than blind at fill time. Order is unspecified.
	List(ctx context.Context) ([]Snapshot, error)
	// Delete removes the agent's snapshot. The interruptible-freeze path (C2,
	// ka-0000005834) calls it to discard a snapshot whose freeze was aborted by
	// an inbound message AFTER it raced to persistence — so no thaw targets an
	// agent we just kept live. Deleting an absent snapshot is a no-op (no error):
	// the post-condition "no snapshot for this agent" already holds.
	Delete(ctx context.Context, agentName string) error
}

// SnapshotTransport moves the HEAVY artifacts a Snapshot references — the 3.0G
// runsc checkpoint image (checkpoint.img / pages.img at Snapshot.ImagePath), the
// preserved {sandbox,agent} OCI bundle (Snapshot.BundlePath), and the rootfs
// overlay lowerdir — between the node that FROZE the agent and the node that will
// THAW it. It is the SIBLING of SnapshotStore: SnapshotStore persists the small
// METADATA record (a few KiB, safe for a ConfigMap), while SnapshotTransport
// moves the multi-GB PAYLOAD those node-local paths point at. The two are kept
// separate on purpose — the 3.0G image must NEVER be routed through the metadata
// store / ConfigMap (etcd's ~1MiB object cap alone forbids it); it travels only
// over this transport seam.
//
// The default implementation is a node-local no-op (NodeLocalSnapshotTransport):
// when the thaw target node is the same node that froze the agent — today's only
// behaviour, since Thaw pins the placeholder Pod B to snap.Node — the artifacts
// already live where the restore reads them, so both verbs are the identity. An
// off-node implementation (the S3 / criu-image-streamer transport,
// restore-through-lifecycle.md "remaining work" #2, unblocked by B2) uploads at
// freeze and re-materializes at thaw WITHOUT touching the freeze/thaw verbs.
type SnapshotTransport interface {
	// Push is invoked at FREEZE time, once the checkpoint image + OCI bundle are
	// written to their node-local paths on the origin node (snap.Node), BEFORE
	// the metadata record is persisted — so the metadata only ever points at a
	// payload that has already been made durable/reachable. A node-local
	// transport is the identity no-op (the artifacts already sit on the node that
	// will thaw). An off-node transport uploads snap.ImagePath / snap.BundlePath
	// (and the rootfs lowerdir) to shared storage so a different node can pull
	// them at thaw. A Push error fails the freeze closed: no metadata is
	// persisted and the caller cold-starts (today's safe fallback), rather than
	// recording a snapshot whose payload never arrived.
	Push(ctx context.Context, snap Snapshot) error
	// Pull is invoked at THAW time, once the target node is chosen, BEFORE the
	// restore runs. It makes the heavy artifacts available at snap.ImagePath /
	// snap.BundlePath ON targetNode. When targetNode == snap.Node the payload is
	// already node-local and Pull is the identity no-op (today's behaviour). A
	// Pull error fails the thaw closed, and the caller falls through to a cold
	// start — the same fail-safe as a reaped image.
	Pull(ctx context.Context, snap Snapshot, targetNode string) error
}

// RehydrationRequest carries the context the post-restore warm-boot hook needs
// (ka-0000001515): which restored pod, on which node, and the deadline within
// which rehydration must complete before the agent resumes issuing work.
type RehydrationRequest struct {
	AgentName   string
	Namespace   string
	PodName     string // placeholder Pod B name
	Node        string
	RestoredPID string
	Deadline    time.Duration

	// PodUID is the kubelet pod UID of the restored pod (Pod B). It is stamped
	// into the thaw-epoch marker so the agent-side poison/freshness stages can
	// assert their re-minted token carries Pod B's identity, not Pod A's
	// (rehydration-design C1; thawepoch.Marker.PodUID).
	PodUID string

	// FrozenAt is the wall-clock of the freeze (the snapshot's CreatedAt). The
	// poison stage drops any in-RAM credential whose on-disk source predates
	// this (rehydration-design Stage 1; thawepoch.Marker.FrozenAt).
	FrozenAt time.Time
}

// RehydrationTrigger fires the credential/NATS re-hydration hand-off after a
// successful restore. The actual warm-boot logic (re-read cred/token files,
// reconnect NATS, re-announce presence) lives in the agent runtime; this
// interface is the controller-side trigger contract.
type RehydrationTrigger interface {
	Trigger(ctx context.Context, req RehydrationRequest) error
}

// CheckpointHostBase and BundleHostBase are the CONTROLLER-OWNED node
// directories the freeze/thaw image + OCI bundle live under (ka-0000003702).
//
// They were /var/tmp/gb-ckpt and /var/tmp/gb-bundle, but /var/tmp on a node is
// inside the containerd / pod-lifecycle GC zone: Pod A's post-freeze teardown
// (kubelet/containerd reaping the per-pod scratch) deleted the 3.0G checkpoint
// image and the OCI bundle out from under a not-yet-thawed snapshot, so thaw's
// `runsc restore` ENOENT'd on checkpoint.img (rc=128) even though freeze saw it
// at 3.0G seconds earlier. Relocating both under /var/lib/gardenio — the same
// controller-owned, pod-lifecycle-independent base as the proven thaw-state
// rendezvous (ThawStateHostBase = /var/lib/gardenio/thaw) — puts them OUTSIDE any
// pod's GC scope: nothing keys these dirs to Pod A's lifecycle, so teardown
// cannot reap them. Fix B already made the small spec files snapshot-carried;
// the 3.0G image is too large for that, so relocation (not snapshot-carry) is
// the correct shape for it.
const (
	CheckpointHostBase = "/var/lib/gardenio/ckpt"
	BundleHostBase     = "/var/lib/gardenio/bundle"
)

func freezeImagePath(agent string) string  { return CheckpointHostBase + "/" + sanitizeIdent(agent) }
func freezeBundlePath(agent string) string { return BundleHostBase + "/" + sanitizeIdent(agent) }

// FreezeImagePath is the exported view of the node-local checkpoint-image path
// for an agent (CheckpointHostBase/<sanitized-agent>). The interruptible-freeze
// e2e probe (cmd/freeze-interrupt-probe, ka-0000004367) uses it to drive a real
// `runsc checkpoint --image-path=<this>` and then assert discardAbortedFreeze
// reaped exactly this path — so the probe and the controller address the one
// path, never a divergent literal.
func FreezeImagePath(agent string) string { return freezeImagePath(agent) }

// sanitizeIdent reduces an agent name to a filesystem-safe token for node-local
// image/bundle paths.
func sanitizeIdent(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "agent"
	}
	return b.String()
}
