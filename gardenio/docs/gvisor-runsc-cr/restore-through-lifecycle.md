# Core: restore a checkpointed gVisor agent THROUGH the CRI/containerd lifecycle

**Bead:** ka-0000003405 (P1) · **Epic:** ka-0000006959 (Phase 3) · **Rehydration handoff:** ka-0000001515
**Inputs:** recon `phase3-cri-recon.md` (ka-0000003467), prior-art `prior-art.md` (ka-0000004052), Phase-2 `report.md` (ka-0000002167)
**Date:** 2026-06-04 · **Substrate:** america-e2e-eks, ns `gvisor-poc`, RuntimeClass `runsc`, gvisor-poc node pool
**Harness:** `restore-through-lifecycle.sh` + `restore-into-b.py` + `placeholder-pod-b.yaml`

## TL;DR

The **production blocker is solved and proven on the substrate.** A checkpointed gVisor agent
sandbox (Pod A) was restored — through the normal Kubernetes lifecycle — into a **fresh,
kubelet-owned pod sandbox** (Pod B), so that **kubelet re-provisioned the CNI netns, emptyDir
volumes, projected SA token, `/etc/resolv.conf` and `/etc/hosts`** instead of the PoC's out-of-band
stop leaving them GC'd. This is exactly the recon's **option (b)** and the **"orchestrator-created
placeholder pod"** pattern validated by Cedana / CRI-O `restore -p` (prior-art.md), now ported to
**containerd + runsc**.

Decisive evidence: the restored runsc sandbox sentry and Pod B's live kubelet-owned sentry **share
the same network-namespace inode** (`net:[4026532540]`), while Pod A's original CNI netns is gone
(GC'd by kubelet on delete). The restored agent's network identity therefore comes from **kubelet**,
not from A's resurrected bindings — which is the entire point of going "through the lifecycle."

**One open item** (the recon predicted it): resuming the **agent sub-container** process tree
(the multi-container half) via raw `runsc restore` fails on the runsc shim's `mounts-fd` handoff
(`reading mounts file: unexpected end of JSON input`). The sandbox sentry restores; the python task
inside it does not yet re-schedule. This is the recon's flagged "BUILD net-new glue" item and is the
remaining work for a controller (details + fix paths in **Remaining work** below).

## What "through the lifecycle" means, concretely

Phase-2 (`cr-harness.sh`) restored a frozen sandbox back into **its own** bundle. But the moment a
sandbox stops outside CRI, kubelet garbage-collects that pod's:

- bundle `config.json`,
- emptyDir volume dirs under `/var/lib/kubelet/pods/<A-uid>/volumes/…`,
- the projected SA token dir (`kube-api-access-XXX`),
- the CNI network namespace `/var/run/netns/cni-…`,
- `/etc/hosts` and the containerd-managed `/etc/resolv.conf`.

So the Phase-2 thaw was *fighting* the kubelet. The production path inverts this: **let kubelet build
a brand-new pod** (so all those bindings are freshly provisioned and owned by kubelet), then graft
the checkpointed process state onto it.

## The mapping that makes it work

Every external binding a runsc bundle references is keyed by one of two identifiers. Restoring A's
image into B's sandbox is, mechanically, **rewriting those two ids** across both the sandbox(pause)
spec and the agent-container spec:

| binding | keyed by | Pod A (GC'd) | Pod B (fresh, kubelet-owned) |
|---|---|---|---|
| CNI network namespace | netns path | `cni-05de9ec1-…` | `cni-82526995-…` |
| emptyDir `/work` | kubelet pod uid | `…/pods/095d488f-…/volumes/…~empty-dir/work` | `…/pods/c38e2800-…/…` |
| projected SA token | kubelet pod uid | `…/095d488f-…/…~projected/kube-api-access-c7qm8` | `…/c38e2800-…/…/kube-api-access-t9xv8` |
| `/etc/hosts` | kubelet pod uid | `…/095d488f-…/etc-hosts` | `…/c38e2800-…/etc-hosts` |
| `/etc/resolv.conf` | containerd sandbox cid | `…/sandboxes/e2811f…/resolv.conf` | `…/sandboxes/088875…/resolv.conf` |
| `/dev/shm` | containerd sandbox cid | `…/sandboxes/e2811f…/shm` | `…/sandboxes/088875…/shm` |

`restore-into-b.py` does this with two string substitutions (A-sandbox-cid → B-sandbox-cid,
A-pod-uid → B-pod-uid) applied to every `mounts[].source` and gVisor mount annotation in both specs,
plus repointing the `network` namespace path to B's CNI netns. The `runsc restore` itself runs under
`nsenter -t 1 -m -p -n` (host init's mount+pid context, as in Phase-2) so the gofer re-exec
(`/proc/self/exe`) succeeds; runsc then opens and joins **B's** netns from the (repointed) spec.

## Step-by-step result (reproducible via the harness)

1. **Pod A** (`agentsim`, runsc) ran with warm state: monotonic counter (reached **82** at freeze),
   2000-entry in-RAM heap cache, 3 open FDs, cwd `/work/repo`, sentinel `GVISOR-POC-STATE-7f3a9c`.
2. **`runsc checkpoint --leave-running`** A's sandbox → **~8.4 MiB** image (`checkpoint.img` +
   `pages.img` + `pages_meta.img`). Preserved A's two OCI bundles (`cp -a`) and A's warm `/work`.
3. **Deleted Pod A through the k8s API.** Verified kubelet GC: A's `…/pods/095d488f-…` dir **gone**,
   A's CNI netns `cni-05de9ec1-…` **gone**, A's runsc sandbox **stopped**. (This is the blocker, live.)
4. **Created placeholder Pod B** (`agentsim-b`, runsc, same shape — see `placeholder-pod-b.yaml`).
   kubelet provisioned a fresh netns (`cni-82526995-…`), a fresh emptyDir `/work`, a fresh projected
   token (`kube-api-access-t9xv8`), fresh resolv.conf/hosts. Seeded B's `/work` with A's warm data.
5. **Restored A's sandbox image into a new sandbox** bound to B's fresh bindings. **Thaw 60–190 ms**,
   rc=0, sandbox **running** (boot pid 37737).
6. **Proof:** `readlink /proc/37737/ns/net` == `readlink /proc/<B-sentry-pid>/ns/net` ==
   `net:[4026532540]`. The restored sandbox lives in **B's kubelet-provisioned netns**; the veth
   `eth0@if11` (AWS VPC CNI, `link-netnsid 0`, carrying pod IP `10.30.138.146`) is the one kubelet
   created for B. A's netns is GC'd. ✅

## Why this is the right architecture (and matches the prior art)

- **No kubelet/containerd fork.** Pod B is a *normal* pod created via the k8s API; we never touch the
  EKS-managed kubelet. This is what made podmigration-operator a non-starter (prior-art §3) and what
  the recon insisted on (§4 "controller around unmodified kubelet").
- **kubelet owns the hard parts.** CNI netns, projected-token rotation, volume lifecycle — all the
  things that are painful and security-sensitive to hand-roll — are done by kubelet because B is a
  real pod. We only graft the *process state* on top. This is precisely Cedana's "sleeping
  placeholder pod" and CRI-O's `restore -p POD-ID` (prior-art §1, §5), ported to containerd+runsc.
- **runsc is the only engine that can checkpoint a gVisor sentry** (prior-art §8); CRIU-based k8s C/R
  projects cannot. So the freeze/thaw stays on raw `runsc` (the recon-proven path; the containerd
  shim Checkpoint RPC still panics — recon §3), and the controller supplies the kubelet-owned-sandbox
  + rehydration glue that runsc deliberately omits.

## Remaining work (the one open item + the productionization)

### 1. Agent sub-container resume (the multi-container half) — ✅ SOLVED (fix path c, ka-0000000845)

> **RESOLVED 2026-06-04 — BOTH fix paths now proven; operator picks pod shape.**
>
> - **Fix path (c)** — `single-container-resume.md` (ka-0000000845, decision ka-0000002982): run the
>   agent as the runsc sandbox ROOT container (agent = sentry init PID 1, no sub-container) so a single
>   `runsc checkpoint|restore` round-trips the task with **no `mounts-fd` handoff**. Simplest controller;
>   costs a fleet-wide pod-shape change (no sidecars). Thaw ~340 ms.
> - **Fix path (b)** — `multi-container-resume.md` (ka-0000000268, decision ka-0000005935): the agent
>   SUB-container resumes on raw `runsc restore` too, keeping the **multi-container / sidecar** shape
>   viable. Crucially, the "mounts-fd" framing below was a mis-diagnosis: `runsc restore <sub-cid>`
>   builds its own `--mounts-fd` via `createGoferProcess` — the real mechanism is a **count-gated
>   barrier** (restore root + sub CONCURRENTLY; root parks at `restoringUnstarted` reading
>   `ContainerCount=N`, the sub join fires the resume). Plus three mechanical fixes: mount-dest
>   `/var/run`→`/run` normalization for `RestoreValidateSpec`; rootfs overlay reconstruction in
>   host-init's ns; a fresh upperdir to dodge the leftover `.gvisor.filestore` collision. Counter
>   advanced 17→23→40 post-thaw; both containers running under one sentry.
>
> The blocker below is removed either way. Remaining work is the controller wrap (#4) for whichever
> pod shape the operator chooses (b-vs-c comparison table in `multi-container-resume.md`).
>
> **DECIDED 2026-06-04 — decision ka-0000007932 → CHOSEN = path (b), multi-container / sidecar.**
> The operator chose to keep today's normal CRI pod shape (pause sandbox + agent sub-container +
> sidecars) and have the controller carry the path-b thaw path (concurrent count-gated barrier
> restore + per-container rootfs overlay reconstruction + `/var/run`→`/run` mount-dest normalization
> + fresh-upper handling). No fleet-wide single-container migration. Path (c) stays a proven fallback,
> not the target. Controller-wrap follow-up: **ka-0000000047** (under Phase-3 epic ka-0000006959).



A runsc pod is **two** containers: the sandbox/pause (holds the sentry + netns) and the agent
sub-container (the python task). Step 5 restores the **sandbox sentry** into B's netns successfully,
but resuming the **agent sub-container** via raw `runsc restore --bundle <agent> <cid>` fails:

```
FATAL ERROR: starting container: reading mounts file: error unmarshaling mounts: unexpected end of JSON input
```

Root cause: in normal operation the **containerd runsc shim** synthesizes a `mounts` file and passes
it to the gofer via `--mounts-fd`; the raw `runsc restore` CLI path does not reconstruct it for a
sub-container join. The python process state IS captured in `pages.img` (it rode along in the
sandbox restore), but without the sub-container's gofer/FS handoff its task is not re-scheduled
(counter stays at the seeded value; `runsc ps` empty).

Fix paths to spike next (cheapest first):
- **(a) Drive the restore through `ctr`/the shim, not raw runsc**, so the shim builds `mounts-fd`
  normally. Recon §3 says the shim's *Checkpoint* RPC panics, but **restore** may be reachable —
  worth a direct `ctr -n k8s.io … restore`-style attempt or a small shim client.
- **(b) Synthesize the mounts file** runsc expects (it's the spec's `mounts[]` in runsc's internal
  format) and pass it on `--mounts-fd` ourselves when invoking the gofer for the sub-container.
- **(c) Single-container sandbox** for agents (run the agent as the pause-equivalent / `--drop-infra`
  analogue) so there is no sub-container to re-join — simplest, but changes the agent pod shape.

Reading CRI-O's restore code (how it rejoins the infra container's namespaces, prior-art §5) and
gVisor's `pkg/shim` restore path will inform which of (a)/(b) is least work.

### 2. Snapshot transport / placement (recon open item)

Freeze and thaw are node-local here (same node, image at `/var/tmp/ckA`). For node-independent thaw,
adopt the **criu-image-streamer** remote-streaming concept (prior-art §9): stream the ~8 MiB image to
S3 on freeze, warm-pull to the target node before restore. (runsc's multi-file layout differs from
CRIU's pipe/splice protocol — the S3 model transfers, the protocol adapter is a spike.)

**Status (Δ5-B2, ka-0000006160):** the **S3 stage model** now lands as
`podmanager.S3SnapshotTransport`, the off-node drop-in for the B1 `SnapshotTransport` seam. At freeze
the controller presigns S3 PUTs with its IRSA identity and a node-side `curl` uploads a single tar
(checkpoint image + OCI bundle) plus a `.sha256` sidecar to
`s3://<bucket>/<prefix>/<agent>/<gen>/`; at thaw, an off-node target presign-GETs both, verifies
sha256 **before** extracting, and unpacks into place. Integrity is end-to-end and every failure surface
fails **closed** to a cold start. It is default-OFF (empty `FREEZE_SNAPSHOT_S3_BUCKET`) — the
node-local no-op stays installed until an operator names the bucket AND the terraform IRSA grant +
bucket lifecycle-expiry are applied. The **fully-streaming** variant (criu-image-streamer, no temp-file
stage at all) remains the deferred optimization above: this stage model transfers today and each side
stages the tar exactly once, never the 3.0G-twice the recon warned of.

### 3. Bundle synthesis vs preservation (recon open item)

This harness **preserves** A's bundles (`cp -a`) then repoints ids. Cleaner long-term: **synthesize**
the restore bundle from **B's** live OCI spec (always-fresh mounts) and only carry over the
runsc-specific bits — removes the id-rewrite step and the bundle-preservation dependency. Spike
whether `runsc restore` tolerates a B-native spec swap.

### 4. Controller shape — follow-up ka-0000000047 (target: path b, per ka-0000007932)

Wrap steps 2–6 in the gardenio controller (`gardenio/controller/internal/podmanager` is the natural
home): a freeze verb (checkpoint + stash + persist snapshot keyed by agent identity) and a thaw verb
(create placeholder Pod B via the k8s API, wait Ready, repoint+restore, then trigger rehydration).
Wrap `runsc` calls in `timeout` with post-state assertions — runsc C/R can hang on process exit
(epic constraint). The freeze/thaw node-access pattern (privileged hostPath probe / DaemonSet) is the
same one this harness and Phase-2 use.

Per decision **ka-0000007932 the controller targets path (b)**: the thaw verb drives the
**concurrent count-gated barrier** restore (root sandbox + agent sub-container), reconstructs each
container's rootfs overlay in host-init's ns, normalizes mount destinations `/var/run`→`/run`, and
handles the fresh-upper / `.gvisor.filestore` collision — the four mechanical fixes proven in
`multi-container-resume.md`. Tracked in follow-up bead **ka-0000000047**.

### 5. Rehydration hand-off (ka-0000001515)

Once the process tree is back, the warm-boot rehydration runs: re-read cred/token files from disk
(never trust in-RAM IRSA/projected tokens — they're stale and bound to A's identity), tear down +
reconnect NATS and any long-lived TCP, re-announce presence, bounded by a deadline. Spec lifted from
AWS Lambda SnapStart's post-restore hook contract (prior-art §10–12). The token/netns B provides are
**valid** (kubelet-minted for B); the cached-in-RAM ones from A are what must be discarded.

## How to reproduce

```sh
# from a checkout of the archive tag (see README.md)
./restore-through-lifecycle.sh                 # full freeze -> GC-proof -> placeholder-B -> restore -> netns proof
# cleanup:
kubectl -n gvisor-poc delete pod agentsim agentsim-b gvisor-probe --grace-period=0 --force
```

## Verdict

- **Restore-through-lifecycle: PROVEN for the sandbox.** A checkpointed gVisor agent restores into a
  fresh kubelet-owned sandbox; kubelet re-provisions netns/volumes/token; the GC problem that blocked
  Phase-2 is defeated. Thaw stays sub-300 ms. The recon's option (b) and the prior-art placeholder-pod
  pattern are validated on containerd+runsc.
- **Agent sub-container resume: OPEN** — the runsc multi-container `mounts-fd` handoff (the recon's
  "BUILD net-new glue"). Three concrete fix paths above; this is the next spike toward a controller.
- **Then:** snapshot transport, bundle synthesis, controller wrap, and rehydration (ka-0000001515).
