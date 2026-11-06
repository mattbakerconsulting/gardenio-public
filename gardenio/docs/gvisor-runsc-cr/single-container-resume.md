# Spike: resume the agent task on runsc restore via the single-container pod shape (fix path c)

**Bead:** ka-0000000845 (P1) · **Epic:** ka-0000006959 (Phase 3) · **Decision:** ka-0000002982 (chose fix path c)
**Parent:** ka-0000003405 (restore-through-lifecycle, MR!3488 — sandbox half proven)
**Date:** 2026-06-04 · **Substrate:** america-e2e-eks, ns `gvisor-poc`, RuntimeClass `runsc`, runsc `release-20260525.0`, containerd 2.2.3
**Harness:** `single-container-resume.sh` + `single-container-resume-node.sh` + `single-container-thaw-into-b-node.sh`

## TL;DR

**The last open item from `restore-through-lifecycle.md` is solved.** The agent SUB-container resume
failure (`reading mounts file: unexpected end of JSON input`) is **eliminated** by running the agent
as the **runsc sandbox ROOT container** — the agent process IS the sentry's init task (PID 1), so there
is **no sub-container to re-join** on restore. A single `runsc checkpoint <cid>` / `runsc restore <cid>`
round-trips the entire agent task with full warm-state fidelity and **no `mounts-fd` handoff**.

Proven twice on the live gvisor-poc node:

1. **In-place single-container C/R** — checkpoint a single-container agent (counter at 11), restore into
   a new cid; the python init task re-schedules and the counter advances past the checkpoint value
   (RAM/FD/cwd/sentinel all intact). `rc=0`, thaw ~340 ms, **no mounts-fd error**.
2. **Single-container thaw THROUGH the lifecycle** — restore that same checkpoint into placeholder
   **Pod B's fresh, kubelet-provisioned CNI netns**. The restored agent sentry shares B's netns inode
   (`net:[4026532202]`) exactly. This proves fix (c) composes with the MR!3488 placeholder-pod pattern:
   the GC-defeating netns hand-off AND the agent-task resume now both work in one `runsc restore`.

This is the green light for the controller wrap (ka-0000003405 "Remaining work" #4).

## The problem fix (c) removes

A normal CRI runsc pod is **two** runsc containers sharing one sentry (verified live on the node):

| runsc container | `container-type` | init proc | role |
|---|---|---|---|
| `38cc…` (sandbox) | `sandbox` | `pause` (PID 1) | holds the sentry + netns |
| `995b…` (agent)   | `container` | `python3` (PID 2) | the agent SUB-container |

`runsc restore <sandbox-cid>` revives the sentry + `pause`. But the **agent sub-container** needs a
*second* join, and the raw `runsc restore` CLI does not synthesize the gofer FS handoff that the
containerd runsc shim normally builds (the `--mounts-fd` mounts file) — so it dies with
`reading mounts file: error unmarshaling mounts: unexpected end of JSON input`. The python task state
rode along inside `pages.img` but is never re-scheduled (`runsc ps` empty, counter frozen).

**Fix (c) collapses the two containers into one.** When the agent process is the sandbox ROOT container,
the thing `runsc restore` re-schedules **is** the agent task — there is no second container, so there is
no second join, so there is no mounts-fd file to synthesize. The blocker is structurally gone, not
worked around.

## What the harness does (single-container-resume.sh)

Runs entirely on the node via the privileged `gvisor-probe` chroot (same access pattern as
`restore-through-lifecycle.sh`); does **not** touch the kubelet/CRI pods. It builds a self-contained
runsc bundle and drives `runsc run|checkpoint|restore` directly.

1. **Build a single-container OCI bundle.** Assemble a python rootfs by stacking the `python:3.11-slim`
   image's overlay layers (resolved via `ctr snapshot mounts`), mounted **in host-init's mount ns** so
   the nsenter'd gofer can see it. The bundle's `process.args` ARE the warm-state agent (monotonic
   counter + 2000-entry heap + 3 FDs + cwd `/work/repo` + sentinel) — **no pause, no sub-container**.
2. **`runsc run --detach`** the single cid → sandbox boots with `python3` as init **PID 1** (verified
   via `runsc ps`). Accrue warm state.
3. **`runsc checkpoint --leave-running`** the single cid → 8.3 MiB image, freeze ~75 ms.
4. **`runsc restore --detach`** the image into a NEW cid. `rc=0`, **no mounts-fd error**.
5. **PROOF:** `runsc ps <restored>` shows `python3` running as init; the counter has advanced past the
   checkpoint value with `uptime_s ≈ counter` (continuous from the frozen RAM, not a fresh boot);
   sentinel `GVISOR-POC-SC-7f3a9c`, cwd, and FDs all intact.

### Through-lifecycle variant (single-container-thaw-into-b-node.sh)

Restores the **same** single-container checkpoint into placeholder Pod B's netns by rewriting only the
bundle's `network` namespace path to B's CNI netns (`/var/run/netns/cni-…`), then `runsc restore`. The
restored agent sentry's `/proc/<pid>/ns/net` inode matches B's live kubelet-owned sentry's
(`net:[4026532202]`). **Key gotcha:** restore must reuse the *identical* bundle dir whose overlay the
checkpoint's gofer was rooted against — a `cp -a` of an overlay mountpoint diverges the FS tree and
breaks the 9p gofer re-walk (`failed to walk "sys" … in mount "__no_name_0:/"`). Repoint the netns
**in place**; do not copy the bundle.

## Measured result (2026-06-04, live)

| step | result |
|---|---|
| single-container `runsc run` | `python3` as init PID 1; `runsc ps` confirms (counter accruing) |
| `runsc checkpoint --leave-running` | rc=0, **8.3 MiB**, freeze **~75 ms** |
| `runsc restore` (in-place) | rc=0, **no mounts-fd error**; counter 11 → 311 (resumed, advancing) |
| restore-into-B (through lifecycle) | rc=0; restored agent netns `net:[4026532202]` **== B's** netns |
| thaw latency | ~340 ms (single-container restore, node-local image) |

## Implications for the controller (ka-0000003405 #4)

The freeze/thaw verbs become **trivially single-cid** for agents born in the single-container shape:

- **Freeze:** `runsc checkpoint --leave-running --image-path <store> <agent-cid>` — one cid, no sandbox
  vs sub-container bookkeeping.
- **Thaw:** create placeholder Pod B (kubelet provisions netns/volumes/token), repoint the restore
  bundle's `network` ns to B's CNI netns **in place**, `runsc restore --bundle <bundle> --detach <cid>`.
  One restore call brings the agent task back into B's kubelet-owned netns. Then rehydration
  (ka-0000001515) re-reads creds and reconnects NATS.

### Cost of the single-container shape

The agent pod must be authored so the **agent process is the pod's only/root container** (the
`pause`-equivalent runs the agent, à la a `--drop-infra` analogue). This is a **pod-shape change** to
the gardenio agent pod template, not a kubelet/containerd change — kubelet still owns a normal pod.
Multi-container agent pods (sidecars) would reintroduce the sub-container mounts-fd problem; if sidecars
are ever needed, fix path (b) (synthesize the mounts file) remains the fallback, but is not needed for
the single-container agent shape the fleet uses today.

## Reproduce

```sh
# from a checkout of the archive tag (see README.md)
kubectl -n gvisor-poc apply -f probe-pod.yaml        # privileged node probe (escalation ka-0000004674)
kubectl -n gvisor-poc wait --for=condition=Ready pod/gvisor-probe --timeout=180s
./single-container-resume.sh                          # build -> run -> checkpoint -> restore -> proof
# through-lifecycle (optional): create placeholder Pod B, then restore into its netns
kubectl -n gvisor-poc apply -f placeholder-pod-b.yaml
#   (see single-container-thaw-into-b-node.sh; pass B_NETNS + B_SENTRY_PID)
# cleanup:
kubectl -n gvisor-poc delete pod gvisor-probe agentsim-b --grace-period=0 --force
```

## Verdict

- **Agent task resume on raw `runsc restore`: SOLVED** via the single-container pod shape (fix path c).
  The multi-container `mounts-fd` blocker is structurally removed — no sub-container, no second join.
- **Composes with restore-through-lifecycle (MR!3488):** the single-container checkpoint restores into a
  fresh kubelet-owned netns; the restored agent task shares B's netns inode. Thaw ~340 ms, sub-second.
- **Next:** controller wrap (freeze/thaw verbs in `gardenio/controller/internal/podmanager`) + the
  single-container agent pod-shape change + rehydration (ka-0000001515). The Phase-3 core path is now
  fully proven end-to-end.
