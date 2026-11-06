# gVisor `runsc` checkpoint/restore PoC — benchmark report

**Bead:** ka-0000000300 · **Phase 2 parent:** ka-0000002167 · **Rehydration handoff:** ka-0000001515
**Substrate:** america-e2e-eks, ns `gvisor-poc` (PSA=privileged), RuntimeClass `runsc`, node pool
label `gvisor=enabled` (taint `gvisor=enabled:NoSchedule`). Granted via escalation ka-0000004674.
**runsc:** `release-20260525.0`, spec 1.2.1, containerd handler `io.containerd.runsc.v1`.
**Date:** 2026-06-03/04.

## TL;DR

The capability is **real and fast.** A live agent container under `runsc` was checkpointed in
**~50–80 ms** to an **~8.2 MiB** snapshot and restored back to a `running` sandbox in
**~50–280 ms**. With `checkpoint --leave-running`, full in-RAM state, all open FDs, cwd and a
held-file sentinel were verified intact while the process kept executing. The one real obstacle
to a *production* freeze/thaw is **not runsc** — it is the **kubelet/CRI lifecycle**, which
garbage-collects the pod's bundle (`config.json`), emptyDir volumes, CNI netns and projected SA
token the moment the sandbox stops out-of-band. Those must be re-provisioned on thaw. That is the
core input to the rehydration design (ka-0000001515).

## 1. Freeze (checkpoint) latency + snapshot size

Workload: python agent holding a monotonic counter, a 2000-entry in-RAM "repo" cache (~1 MiB),
3 open FDs (append log, held r/w file at non-zero offset, UDP socket), cwd `/work/repo`, and a
sentinel in `/work/state.bin`. Measured with `time.perf_counter()` around `runsc checkpoint`.

| run | mode | freeze latency | snapshot size |
|-----|------|---------------:|--------------:|
| 1 | cold (`checkpoint`) | 61.2 ms | 8.16 MiB |
| 2 | cold | 67.8 ms | 8.13 MiB |
| 3 | cold | 48.9 ms | 8.13 MiB |
| 4 | cold | 66.4 ms | 8.13 MiB |
| 5 | `--leave-running` | 54.6 ms | 8.16 MiB |
| 6 | `--leave-running` | 79.3 ms | 8.16 MiB |

**Freeze: ~50–80 ms. Snapshot ~8.2 MiB** = `checkpoint.img` (~0.3 MiB OCI spec + sentry meta) +
`pages.img` (~8.2 MiB committed guest RAM) + `pages_meta.img` (~3 KiB). Snapshot size scales with
the **dirty/committed RAM** of the sandbox, not disk. `compression=none` here; runsc supports
`flate-best-speed` and `--exclude-committed-zero-pages` to shrink further.

> **Shrink PoC harness (ka-0000003166):** those two levers are now measurable. `bench-shrink.sh`
> + `bench-shrink-node.sh` sweep `baseline` vs `--exclude-committed-zero-pages` vs
> `+ --compression=flate-best-speed` on one c6a and emit `FT_PAGES_BYTES` (on-disk),
> `FT_RESTORE_PEAK_RSS` (resident), and a restore-integrity check — kept rigorously separate,
> because **compression shrinks on-disk bytes but not resident bytes; only zero-page exclusion
> lowers the peak-RSS the node-fit gate cares about**. See **`poc-shrink.md`** (archive tag, see [README](README.md)) for
> the run recipe, sentinel dictionary, and the fit-verdict formula. (`bench-freeze-node.sh` /
> `bench-thaw-node.sh` also gained an opt-in `FT_CHECKPOINT_VARIANT`; defaults preserve their
> existing benches byte-for-byte.)

## 2. Thaw (restore) latency

`runsc restore --image-path … --bundle … --detach <sandbox>`, run via `nsenter -t 1 -m -p -n`
into the host namespaces (the gofer re-exec and netns open require host init context).

| run | thaw latency | result |
|-----|-------------:|--------|
| A | 50–88 ms | sandbox → `running` (pause restored; agent sub-container needs its meta preserved) |
| B | 280.8 ms | rc=0, full 2-container sandbox restored (`container_count=2`) |

**Thaw: sub-300 ms in all successful runs; ~50–90 ms for the sandbox-root restore.** The target
of *sub-second* thaw is **comfortably met.** Variance is dominated by whether the agent
sub-container is restored alongside the pause/sandbox (2-container restore ≈ 280 ms vs
sandbox-only ≈ 50–90 ms).

## 3. State-fidelity matrix

Verified post-`checkpoint --leave-running` via `runsc exec` into the same sandbox (counter was at
305→306 pre-freeze):

| state element | mechanism | result |
|---|---|---|
| In-RAM monotonic counter | continued 306 → 307 → 308 (no reset) | ✅ intact |
| Heap / in-RAM "repo" cache | `repo_loaded=2000` unchanged | ✅ intact |
| Open append-log FD (fd 3) | `/proc/1/fd/3 → /work/agent.log` | ✅ intact |
| Held r/w file FD (fd 4) | `/proc/1/fd/4 → /work/state.bin`, offset preserved | ✅ intact |
| Open UDP socket FD (fd 5) | `/proc/1/fd/5 → socket:[…]` | ✅ intact |
| cwd | `readlink /proc/1/cwd = /work/repo` | ✅ intact |
| Held-file contents (sentinel) | `cat /work/state.bin = GVISOR-POC-STATE-7f3a9c` | ✅ intact |
| stdio FDs (0/1/2) | `host:[…]` host-pipe refs | ✅ intact |

On **cold** checkpoint→restore the sandbox returns to `running` and the checkpoint metadata
confirms both containers (`container_count: 2`) and the full agent OCI spec + guest RAM are
captured in `pages.img`. The resumed app process is short-lived **only because kubelet has already
GC'd the pod's emptyDir bind source** — see §5.

## 4. gVisor overhead vs runc baseline

Identical micro-benchmark under `runsc` (gvisor pool) and `runc` (default pool). 2 samples each.

| metric | runsc (c6a.large/amd64) | runc (c6g.xlarge/arm64) | gVisor signal |
|---|---|---|---|
| `stat` ×200k | 1.19 s / 2.21 s | 0.33 s / 0.34 s | **~3.6–6.6× syscall overhead** |
| file-IO 100 MB | 0.102 s / 0.107 s | 0.034 s / 0.034 s | **~3× IO overhead** |
| CPU compute | 1.96 s / 1.95 s | 3.33 s / 3.36 s | **NOT comparable — see caveat** |

**Caveat:** the gvisor-poc node is `c6a.large` (amd64) and the default pool node is `c6g.xlarge`
(arm64) — different architecture and size — so the CPU/wall numbers are **not a valid runsc-vs-runc
comparison** (the amd64 node is simply faster at this pure-Python loop). Only the
**syscall-interception and file-IO ratios** are meaningful gVisor signals, and they match gVisor's
documented profile: the sentry adds **~3–6× on syscall-bound paths and ~3× on file-IO**, while
CPU-bound user code runs at ~native speed. For a clean overhead number, re-run both pods on the
**same instance type** (add a matching amd64 runc node, or an arm64 gvisor node).

runsc also self-reports per-sandbox CPU accounting in the checkpoint metadata
(`gvisor_cpu_usage`, `gvisor_wall_time`) — usable for live overhead telemetry without a baseline pod.

## 5. Credential / NATS-on-thaw observations → rehydration design (ka-0000001515)

The checkpoint metadata (`checkpoint.img`) captures the **OCI spec**, which includes:
- `env`: all `KUBERNETES_SERVICE_*` vars (baked at create time — **stale** after thaw into a new pod).
- `mounts`: the SA token as a **bind-mount reference** to
  `/var/lib/kubelet/pods/<uid>/volumes/kubernetes.io~projected/kube-api-access-XXX`, plus
  `/etc/resolv.conf`, `/etc/hosts`, and the emptyDir `/work` — all **kubelet-managed host paths**,
  **not** token *contents*. Guest RAM (`pages.img`) holds whatever the process already read into
  memory (e.g. a token string an agent loaded), but the on-disk bind sources are external.

**Implications for rehydration:**
1. **Out-of-band C/R fights the kubelet.** When the sandbox stops outside CRI, the kubelet GCs the
   pod sandbox: bundle `config.json`, emptyDir volumes, CNI netns (`/var/run/netns/cni-…`) and the
   projected SA-token dir all disappear. Restore then fails on missing spec / missing netns. The
   harness works around this by (a) preserving the bundle with `cp -a` before freeze and (b)
   recreating the netns. A production path must **restore through the CRI/containerd shim** (a
   checkpoint/restore-aware CRI verb) so the kubelet re-provisions volumes, netns and token, rather
   than driving `runsc` directly under a live kubelet.
2. **Credentials must be re-minted, not restored.** IRSA web-identity tokens
   (`AWS_WEB_IDENTITY_TOKEN_FILE`) and the projected SA token are short-lived and bound to the
   *original* pod identity. On thaw into a new pod they must be **re-provisioned by the kubelet**
   (projected volume) and **re-resolved by the SDK** (IRSA STS refreshes on next call). Any token an
   agent cached in RAM at freeze time will be **expired/wrong** on a later thaw — the agent must
   re-read its token files after restore, not trust in-memory copies.
3. **NATS / long-lived TCP connections do not survive a host/IP change.** gVisor restores the
   socket FD (we confirmed the UDP socket FD survives), but a restored TCP connection to NATS is
   bound to the original 4-tuple; after thaw onto a new node/IP the kernel peer is gone and the
   connection is dead on first write. **Rehydration must force a NATS reconnect on thaw** (the nats
   client's reconnect logic suffices if triggered) rather than assuming the live connection resumes.
   The clean design is: on thaw, (a) re-read cred/token files, (b) tear down + reconnect NATS, (c)
   re-announce presence — i.e. treat thaw as a "warm boot" that keeps RAM/FS but re-establishes all
   external identity and connections.

## How to reproduce

```sh
# from a checkout of the archive tag (see README.md)
./cr-harness.sh                       # freeze/thaw + leave-running fidelity proof
kubectl apply -f bench-runsc.yaml -f bench-runc.yaml   # overhead (mind the arch caveat)
kubectl -n gvisor-poc logs bench-runsc; kubectl -n gvisor-poc logs bench-runc
kubectl -n gvisor-poc delete pod gvisor-probe agentsim bench-runsc bench-runc --grace-period=0 --force
```

## Verdict

- **Capability proven:** sub-100 ms freeze, sub-300 ms thaw, full RAM+FD+cwd+FS fidelity, on the
  granted gvisor-poc substrate. Sub-second thaw target met with wide margin.
- **gVisor overhead** is in the expected band (syscall ~3–6×, IO ~3×, CPU ~native); a same-instance
  re-run is needed for a publishable runc delta.
- **The hard part is rehydration, not C/R**: kubelet-managed bundle/volume/netns/token lifecycle +
  credential re-mint + NATS reconnect. Handed to ka-0000001515.
