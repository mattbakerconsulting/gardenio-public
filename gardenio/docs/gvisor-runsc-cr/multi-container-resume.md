# Spike: resume the agent SUB-container on raw `runsc restore` — multi-container pod shape (fix path b)

**Bead:** ka-0000000268 (P1) · **Epic:** ka-0000006959 (Phase 3) · **Decision:** ka-0000005935 (chose prove-path-b)
**Sibling (proven):** ka-0000000845 / `single-container-resume.md` (fix path c) · **Parent:** ka-0000003405 (restore-through-lifecycle)
**Date:** 2026-06-04 · **Substrate:** america-e2e-eks, ns `gvisor-poc`, RuntimeClass `runsc`, runsc `release-20260525.0`, containerd 2.2.3, shim 2.1.5
**Harness:** `multi-container-resume-node.sh` (driven on the node via the `gvisor-probe` chroot)

## TL;DR

**Multi-container / sidecar agent pods are VIABLE for gVisor freeze/thaw.** The agent SUB-container
task — the half that fix path (c) sidestepped by collapsing the pod to a single container — **can be
resumed on a raw `runsc restore`, with no containerd shim and no hand-built mounts file.** Proven
twice on the live gvisor-poc node: a 2-container runsc pod (pause + python agent, one shared sentry)
was checkpointed as a whole, the originals torn down, and both containers restored from the single
image; the python agent task re-scheduled and its monotonic counter advanced **17 → 23 → 40** past the
checkpoint value with full RAM/FD/cwd/sentinel fidelity. `runsc ps` shows `python3` running as the
re-scheduled sub-container task; both containers report `running` under one restored sentry.

This is the key result the operator needs to choose pod shape before the controller wrap (ka-0000003405
"Remaining work" #4): **the sidecar shape no longer forces the fleet-wide single-container redesign.**

## The big reframe: it was never "a mounts file the shim builds that the CLI can't"

The Phase-3 core writeup (`restore-through-lifecycle.md`) flagged the open item as:

> resuming the agent sub-container via raw `runsc restore` fails on the runsc shim's `mounts-fd`
> handoff (`reading mounts file: error unmarshaling mounts: unexpected end of JSON input`) … the
> containerd runsc shim synthesizes a mounts file passed to the gofer via `--mounts-fd`; the raw
> `runsc restore` CLI path does not reconstruct it.

**That root-cause attribution was wrong.** Source-grounded against gVisor `release-20260525.0`:

- `runsc restore <sub-cid>` and `runsc start <sub-cid>` share **one engine**, `Container.startImpl`
  (`runsc/container/container.go:454`). For a sub-container (`container-type=container`) it runs
  `createGoferProcess()` (`container.go:478-515`), which **spawns the sub-container's OWN gofer**,
  creates an **internal `os.Pipe()`**, donates the write end to that gofer as `--mounts-fd`
  (`container.go:1467`, `donations.DonateAndClose("mounts-fd", mountsGofer)`), and reads the resolved
  mount list back with `specutils.ReadMounts()` (`container.go:500`).
- The shim builds **nothing** mounts-related. `Runsc.Restore` (`pkg/shim/v1/runsccmd/runsc.go:286`)
  only ever execs `runsc restore --image-path --detach [--direct] [--background] <id>`. The mounts-fd
  pipe is constructed *inside* runsc, not handed in by the shim.

So `--mounts-fd` is an internal runsc detail the CLI already drives. The famous
`unexpected end of JSON input` (`specutils.ReadMounts`, `specutils.go:343`) means the gofer pipe
closed with **zero bytes** — the sub-container's gofer died **before** it reached `WriteMounts`,
because the restore was driven out of order or its rootfs/mount sources were not reconstructable. The
error was a *symptom of mis-driving the restore*, not a missing shim artifact.

## The actual blocker: multi-container restore is a COUNT-GATED BARRIER

A CRI runsc pod is **two** runsc containers sharing one sentry (verified live):

| runsc container | `container-type` | init | role |
|---|---|---|---|
| `<sandbox-cid>` | `sandbox`   | `pause` (PID 1)  | holds sentry + netns |
| `<agent-cid>`   | `container` | `python3` (PID 2)| the agent SUB-container |

`runsc checkpoint <sandbox-cid>` writes **one** image for the whole pod. Its statefile metadata
carries `container_count: "2"` (confirmed live in `checkpoint.img`). Restore is then a barrier
(`runsc/boot/controller.go`, `runsc/boot/restore.go`):

1. `runsc restore <root/sandbox-cid>` reads `ContainerCountKey` → `restorer.totalContainers = 2`,
   registers **only** the root, and parks the loader in state `restoringUnstarted`
   (`controller.go:645`, `:673`). **With `--detach` this call does NOT return early** — the detached
   sandbox raises its start-sync only *after* the whole pod restore completes, which needs all N
   containers. So the root restore **blocks at the barrier**.
2. `runsc restore <agent/sub-cid>` takes the sub-container engine path: spawns its gofer, the gofer
   `ResolveMounts()` + `WriteMounts()` to the internal `--mounts-fd` pipe, runsc `ReadMounts()`
   succeeds, then `Sandbox.RestoreSubcontainer` → `ContMgrRestoreSubcontainer`. The loader requires
   `state == restoringUnstarted` (`controller.go:815`, the source of `sandbox is not being restored,
   cannot restore subcontainer`).
3. When `len(r.containers) == r.totalContainers` the loader fires the real restore and resumes the
   kernel (`restore.go:171`); **both** the blocked root call and the sub-container call then return.

**Therefore the two `runsc restore` calls must run CONCURRENTLY** — exactly as the shim issues
`ContMgrRestore` + `ContMgrRestoreSubcontainer` as concurrent async RPCs. The harness launches the
root restore in the background, waits for it to register the cid, then fires the sub-container restore
to satisfy the count and unblock both.

## Four fixes, in the order the spike hit them

Driving the restore correctly surfaced three more concrete obstacles. All are mechanical and solved:

| # | Symptom | Root cause | Fix |
|---|---|---|---|
| 1 | `sandbox is not being restored … state=started` | sub restored against the **live** original sandbox (or before the root restore) | tear down originals; restore root **concurrently**, sub joins while root is `restoringUnstarted` |
| 2 | `Mounts does not match across checkpoint restore for container: agent` | `RestoreValidateSpec`/`validateMounts` keys mounts by **Destination** and requires the sets to match; gVisor stores the projected SA-token dest **symlink-resolved** (`/run/secrets/…`) but the OCI bundle has the raw `/var/run/secrets/…` | rewrite the restore bundle's `/var/run/*` mount destinations to `/run/*` so the destination sets match (Source is explicitly ignored by the validator — `restore.go` clears it before compare) |
| 3 | `failed to walk "tmp" … in mount "agent:/": no such file or directory` (in `vfs.CompleteRestore` 9p re-walk) | the agent's rootfs is an **overlay mounted in host-init's mount ns**; a `cp -a` of `$T/<cid>/rootfs` from the probe's ns copies only the empty mountpoint (writable upper: `dev/`, `etc/`), not the merged python image | reconstruct the rootfs overlay in **host-init's ns**: capture the live overlay opts from `/proc/mounts` before delete, re-mount the same image layers onto the preserved bundle rootfs |
| 4 | `mount source already has a filestore file … repeated submounts are not supported with overlay optimizations` | the re-mounted overlay reused the original **upperdir**, which still held `.gvisor.filestore.<cid>` | collapse original `{lower+upper}` into the new overlay's **lowerdir** (read-only, preserves written files) and give a **fresh** upper/work; whiteout any leftover `.gvisor.filestore.*` |

After all four, `agent sub restore rc=0`, `ROOT_RC=0`, and the agent task re-schedules.

## Measured result (2026-06-04, live, runsc release-20260525.0, containerd 2.2.3)

| step | result |
|---|---|
| pod `runsc checkpoint --leave-running` (sandbox cid) | rc=0, **8.3–8.4 MiB**, freeze **~80–110 ms**, `container_count=2` in statefile |
| root sandbox restore (background, blocks at barrier) | parks at `restoringUnstarted`; `ROOT_RC=0` after barrier fires |
| agent SUB-container restore (satisfies barrier) | **rc=0**, ~100–280 ms, **no mounts-fd error** |
| proof | `runsc ps` → `python3` re-scheduled (PID 2); counter **17 → 23 → 40**; sentinel `GVISOR-POC-MC-7f3a9c`, cwd `/work/repo`, FDs `{log:3,state:4,sock:5}` intact; both containers `running` under one sentry (pid 61539); restored netns `net:[4026532202]` |

## Path (b) vs path (c) — the operator's pod-shape choice

Both paths are now **proven on the substrate**. They differ only in pod shape and the freeze/thaw glue
the controller must carry.

| dimension | **path (b)** — multi-container / sidecar (this spike) | **path (c)** — single-container (ka-0000000845) |
|---|---|---|
| **Pod shape** | normal CRI shape: pause sandbox + agent sub-container (+ any sidecars) | agent process IS the sandbox ROOT container (no pause, no sub-container) |
| **Fleet impact** | **none** — agents keep today's pod template; sidecars (log shippers, proxies, coop) remain possible | **fleet-wide pod-shape change** — agent template must run the agent as pod root/only container; no sidecars |
| **Freeze** | `runsc checkpoint <sandbox-cid>` — one image for the whole pod (count=N baked in) | `runsc checkpoint <agent-cid>` — one image, single container |
| **Thaw** | **two concurrent** `runsc restore` calls (root parks at barrier; sub joins → resume); rootfs overlay reconstruction per container; mount-dest normalization | **one** `runsc restore` — no barrier, no sub-container join, no second gofer |
| **Controller complexity** | higher: must order N restores concurrently, reconstruct each container's rootfs overlay, normalize mount dests, manage the filestore-collision/fresh-upper detail | lower: single-cid freeze/thaw verbs, no barrier bookkeeping |
| **Thaw latency** | ~0.3–0.4 s (root + sub, node-local image) | ~0.34 s (single restore, node-local image) |
| **Composes with restore-through-lifecycle (Pod B)** | yes — same netns-repoint pattern applies to the sandbox spec; sub-container binds repoint by pod-uid | yes (proven in `single-container-resume.md`) |
| **Risk surface** | more moving parts in the thaw path (overlay re-mount, barrier timing) → more controller code to get right and to test | fewer moving parts, but a real migration cost + a permanent "no sidecars" constraint on agent pods |

### Decision: CHOSEN = path (b) — multi-container / sidecar (operator, 2026-06-04)

> **RESOLVED — decision ka-0000007932 → `path-b`.** The operator chose to target the
> **multi-container / sidecar** pod shape for the Phase-3 controller wrap (ka-0000003405
> "Remaining work" #4). Agent pods keep today's normal CRI shape (pause sandbox + agent
> sub-container + any sidecars); there is **no fleet-wide single-container migration**. The
> controller carries the more intricate thaw path: the concurrent count-gated barrier restore
> (root + sub), per-container rootfs overlay reconstruction in host-init's ns, mount-dest
> `/var/run`→`/run` normalization, and the fresh-upper / `.gvisor.filestore` collision handling
> detailed above. Path (c) (single-container, `single-container-resume.md`) remains a proven
> fallback but is **not** the target. Controller-wrap follow-up: **ka-0000000047** (filed under
> Phase-3 epic ka-0000006959).

For the record, the trade-off the operator weighed:

- **path (b)** preserves **sidecars** on agent pods (log shippers, credential proxies, a coop
  sidecar, service-mesh injection) and is now proven to work — at the cost of a more intricate thaw
  path in the controller. **(chosen)**
- **path (c)** would have been the simpler, lower-risk controller (single-cid verbs, no barrier, no
  per-container overlay reconstruction) but costs a one-time fleet-wide pod-shape migration and a
  permanent "no sidecars on agent pods" constraint. **(not chosen)**

Both restore into a placeholder Pod B's fresh kubelet-owned netns (the GC-defeating pattern from
`restore-through-lifecycle.md` / MR!3488), so the choice is purely shape + controller-complexity, not
capability. **This spike removes "sidecars are impossible under freeze/thaw" from the decision** — the
operator can now choose pod shape on its merits.

## Reproduce

```sh
# from a checkout of the archive tag (see README.md)
kubectl -n gvisor-poc apply -f probe-pod.yaml                 # privileged node probe (escalation ka-0000004674)
kubectl -n gvisor-poc wait --for=condition=Ready pod/gvisor-probe --timeout=180s
# a normal 2-container runsc pod with warm agent state (same body as agentsim-pod.yaml, renamed):
kubectl -n gvisor-poc apply -f mc-agentsim-pod.yaml           # name: mc-agentsim
kubectl -n gvisor-poc wait --for=condition=Ready pod/mc-agentsim --timeout=180s
# drive on the node (copy the script in, run via chroot):
kubectl -n gvisor-poc exec -i gvisor-probe -- chroot /host bash -c 'cat > /var/tmp/multi-container-resume-node.sh' < multi-container-resume-node.sh
kubectl -n gvisor-poc exec gvisor-probe -- chroot /host bash -c 'SBNAME=mc-agentsim timeout 240 bash /var/tmp/multi-container-resume-node.sh'
# cleanup:
kubectl -n gvisor-poc delete pod mc-agentsim gvisor-probe --grace-period=0 --force
```

## Verdict

- **Agent SUB-container resume on raw `runsc restore`: SOLVED (fix path b).** No containerd shim, no
  hand-built mounts file — runsc's own per-sub-container gofer synthesizes `--mounts-fd` once the
  restore is driven as a concurrent, count-gated barrier (root + sub), with the rootfs overlay
  reconstructed in host-init's ns and mount destinations normalized to the checkpoint's resolved form.
- **Multi-container / sidecar agent pods are viable for freeze/thaw.** The operator's pod-shape choice
  (b vs c) is now about controller complexity and the sidecar requirement, not about capability.
- **Decision (ka-0000007932): CHOSEN = path (b).** The operator picked the multi-container / sidecar
  shape — agent pods keep today's shape, sidecars stay possible, no fleet-wide migration.
- **Next:** the controller wrap (ka-0000003405 #4, tracked in follow-up **ka-0000000047**) implements
  the path-b freeze/thaw verbs (concurrent count-gated barrier restore + per-container overlay
  reconstruction + mount-dest normalization) in `gardenio/controller/internal/podmanager`, composing
  with the placeholder-Pod-B netns hand-off and rehydration (ka-0000001515).
