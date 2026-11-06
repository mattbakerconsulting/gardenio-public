# Research: in-box Kubernetes distro choice (k3s vs k0s vs kind vs MicroK8s)

**Bead:** ka-0000000624 · **Parent epic:** ka-0000001295 (Gardenio in a Box: single-EC2 full
install, in-box kube, zero external cloud deps) · **Type:** RESEARCH ONLY.

## TL;DR — Recommendation: **k3s**

Use **k3s** (Rancher/SUSE), pinned to the **v1.33 line** (e.g. `v1.33.12+k3s1`), single-node
**SQLite/kine** datastore. Runner-up: **k0s**. Reject **kind** (CI-only) and treat **MicroK8s**
as a weak third.

k3s is the only one of the four that satisfies all of the decisive priorities at once with the
least add-on surface, and it lines up cleanly with gardenio's *existing, hard-won* runsc
operational lesson (see [§4](#4-gvisorrunsc-critical) and `gvisor-runsc-prod.md`).

### Decisive priorities (in order)

1. **gVisor/runsc RuntimeClass + `runsc checkpoint|restore` freeze/thaw must work and stay
   maintainable.** Non-negotiable — this is gardenio's agent-sandboxing substrate.
2. **Reuse of built-in components** (ingress/Traefik, default StorageClass to replace EBS/EFS
   CSI, built-in LoadBalancer) to minimize add-ons.
3. **Restart durability** on a rebooting EC2.
4. **Small footprint + simple cloud-init/air-gap install.**
5. **Backup / DR** on a single node.

## Score matrix

| Distro | Footprint | Install / cloud-init | Built-ins reuse | gVisor/runsc + ckpt/restore | Restart durability | Backup / DR | Overall |
|---|---|---|---|---|---|---|---|
| **k3s** | A — single ~70 MB binary, SQLite | A — curl one-liner, air-gap baked AMI | **A — ingress + storage + LB + helm-controller all bundled** | A− — works; one template gotcha | A — systemd + SQLite, clean reboot | B+ — `etcd-snapshot save` + cron/S3; PV data manual | **WINNER** |
| k0s | B+ — static binary ~160–300 MB | A − few shell lines, air-gap bundle | C — minimal; no ingress / default-SC / LB | **A — cleanest `containerd.d` drop-in** | A− — systemd; pick kine vs etcd explicitly | A — native `k0s backup`/`restore`; PV manual | runner-up |
| MicroK8s | C+ — ~526–685 MB idle, snap services | C — needs snapd (absent on minimal AMIs) | C — add-on model; NGINX not Traefik; MetalLB glue | C — snap containerd opaque; bug #5093 drops runsc opts | B − snapd auto-refresh must be held; dqlite corruption risk | C — manual dqlite tarball/reconfigure | weak |
| kind | D — double-containerd nesting, needs host Docker | C — needs Docker; recreate-to-upgrade | C — default-SC only; no ingress/LB | **D — nested containerd; ckpt/restore high-risk/unproven** | **F — Docker re-IPs node, breaks apiserver/kubeconfig** | D — manual etcd dance, IP-sensitive | reject |

## Why k3s

- **(a) gVisor/runsc — the gate.** k3s runs vanilla embedded **containerd**, so the runsc
  RuntimeClass mechanism is identical to a standard containerd host. Critically, gardenio's
  freeze/thaw does **not** go through the Kubernetes CRI: the runsc containerd shim returns
  `ErrNotImplemented` for `CheckpointContainer`, and containerd's CRI service likewise lacks it
  for runsc (gvisor#11810, containerd#12280). Gardenio's *correct, already-implemented* design is
  a **privileged hostPID DaemonSet that exec's raw `runsc checkpoint --image-path` /
  `runsc restore`** against the node's containerd runsc `--root` (see
  `controller/internal/podmanager/freezethaw_exec.go`). That path works on k3s exactly as on a
  standard host. The only k3s-specific care is injecting the runsc handler via a containerd
  **config template** (the rendered `config.toml` is overwritten on restart) and pointing
  `--root` at k3s's containerd state dir. **On a single node the CPU-feature compatibility check
  for thaw is always satisfied (same host), making freeze/thaw strictly easier than on EKS.**

  This matches gardenio's existing prod runbook almost exactly. `gvisor-runsc-prod.md` documents
  that the naive `runsc install` + containerd restart does **not** survive on AL2023 because
  nodeadm regenerates `/etc/containerd/config.toml` and wipes the handler — the handler must be
  injected at config-generation time. k3s's `config.toml.tmpl` / `config-v3.toml.tmpl` template
  **is** that durable injection point, so the in-box story is *less* fragile than the current EKS
  nodeadm path, not more.

- **(b) Built-in reuse — decisive.** k3s is the **only** distro of the four that bundles all four
  reusable components gardenio wants:
  - **Traefik** ingress (v3 on k3s ≥1.32) — matches the chart's existing Traefik assumption.
  - **local-path-provisioner** as the default StorageClass `local-path` — direct EBS/EFS CSI
    replacement on a single node.
  - **ServiceLB / Klipper-LB** built-in LoadBalancer — real `type=LoadBalancer` Services on one
    host, no MetalLB.
  - **helm-controller** — drop the gardenio chart as a `HelmChart` manifest under
    `/var/lib/rancher/k3s/server/manifests/` for auto-install on boot.

  k0s, MicroK8s, and kind each force you to hand-wire a default StorageClass **and** a
  LoadBalancer (MetalLB / cloud-provider-kind).

- **(c) Restart durability.** systemd `k3s.service` (`Restart=always`) + SQLite/kine on disk =
  clean unattended reboot recovery, no etcd quorum, no IP/cert breakage (pin `--node-ip` /
  `--tls-san`).

- **(d) Footprint / install.** Single ~70 MB binary, one-line `curl` install or fully air-gapped
  baked-AMI flow — the lightest mainstream option.

### Tradeoffs / when a runner-up wins

- We give up **k0s's nicer `containerd.d` merge-patch drop-in** (k0s special-cases CRI sections
  and merges a drop-in `.toml`, surviving restarts without templating). On k3s you must use a
  `config-v3.toml.tmpl` Go template extending `{{ template "base" . }}` — a documented **one-time**
  setup, not a recurring chore. If the team values the absolute cleanest restart-survivable runsc
  config injection above all, and is willing to hand-wire storage + MetalLB, k0s edges ahead
  purely on the gVisor axis and has a first-class single-command `k0s backup`/`restore`.
- **MicroK8s** only if you specifically want Canonical's add-on ecosystem **and** have validated
  runsc checkpoint/restore past bug canonical/microk8s#5093 (snap containerd does **not** forward
  `[...runtimes.runsc.options]`/`ConfigPath` TOML to custom shims) — otherwise its containerd
  opacity is a real risk to the non-negotiable priority. Also requires holding snapd auto-refresh
  (`snap set system refresh.hold=<future>`).
- **kind**: reserve for ephemeral CI, never this appliance. It fails reboot durability (Docker
  re-IPs the node container → breaks apiserver/kubeconfig), has no in-place upgrade, and runs
  runsc checkpoint/restore two layers deep in nested containerd where KVM is typically unavailable
  and CRIU-style state handling is fragile (gvisor docs show nested KinD-under-gVisor failing).

## 4. gVisor/runsc (critical)

All four distros use containerd, so the runsc handler block + matching RuntimeClass `handler`
name is identical everywhere. **The only real per-distro variation is *where* the containerd
config lives and whether the distro regenerates/overwrites it on restart** — every one of these
distros owns its containerd config and will clobber a hand-edited `config.toml`, so you must use
the distro's supported injection point:

| Distro | Config location | Injection point | Regen behavior |
|---|---|---|---|
| **k3s** | `/var/lib/rancher/k3s/agent/etc/containerd/config.toml` | `config.toml.tmpl` (containerd 1.7) or `config-v3.toml.tmpl` (containerd 2.0), extend `{{ template "base" . }}` | rendered file overwritten every restart → must template |
| k0s | `/run/k0s/containerd-cri.toml` (merged) | drop a `.toml` with a `plugins."io.containerd.grpc.v1.cri"` section into `/etc/k0s/containerd.d/` → **merged as a patch** | cleanest; do not edit the `# k0s_managed=true` file |
| MicroK8s | `/var/snap/microk8s/current/args/containerd.toml` | edit `containerd-template.toml`; `microk8s stop && start` | overwritten; snap PATH confinement + bug #5093 |
| kind | `/etc/containerd/config.toml` inside node container | `containerdConfigPatches` at create time | ephemeral node; lost on cluster delete |

**Freeze/thaw mechanics gardenio must honor (any distro):**
- `runsc checkpoint --image-path=<dir> <cid>` (`--leave-running` to keep the agent live). Each
  image-path **must be unique** → the hostPath thaw-state volume must allocate a fresh subdir per
  snapshot.
- Restore: `runsc create <cid>` then `runsc restore --image-path=<dir> <cid>`. **All** top-level
  runsc flags used at run time (notably `--root` and the platform flag) must be passed
  **identically** to restore.
- In-sandbox FS writes are **not** propagated on restore — durable agent state must live on
  mounted volumes, not the container FS.
- Live exec'd processes present at checkpoint can break restore (gvisor#11439) — drain agent
  shells/REPLs before freezing.

**PodSecurity on a single node — an in-box advantage.** On EKS the managed control plane hides
the apiserver PodSecurity `AdmissionConfiguration`, which is why the prod chart needs a dedicated
PSA-`privileged` namespace (`gvisor-namespace.yaml`). **On the in-box single node we own the
apiserver**, so we can drop in an `AdmissionConfiguration` that exempts `runtimeClasses: [runsc]`
(and/or the agent + probe namespaces) directly — wired via k3s `--kube-apiserver-arg
--admission-control-config-file`. Scope the agent namespace tightly since a runtimeClass exemption
is broad.

## Single-EC2 install sketch (k3s)

1. **Host:** AL2023 (or Ubuntu) AMI. In user-data, pre-stage `/etc/rancher/k3s/config.yaml`
   (token, pinned `node-ip`, pinned `tls-san`; keep traefik / local-storage / servicelb /
   helm-controller — the reuse win). Air-gap: place the `k3s` binary in `/usr/local/bin` and
   `k3s-airgap-images-amd64.tar.zst` in `/var/lib/rancher/k3s/agent/images/`, touch
   `.cache.json` for fast restarts.
2. **Install k3s:** `curl -sfL https://get.k3s.io | sh -` (or `INSTALL_K3S_SKIP_DOWNLOAD=true
   ./install.sh` air-gapped, pinning `INSTALL_K3S_VERSION` to the v1.33 line). Creates + enables
   `k3s.service` (`Restart=always`).
3. **Add runsc RuntimeClass:** install `runsc` + `containerd-shim-runsc-v1` to `/usr/local/bin`.
   Drop `config-v3.toml.tmpl` (or `config.toml.tmpl`) under
   `/var/lib/rancher/k3s/agent/etc/containerd/` that does `{{ template "base" . }}` plus a runsc
   stanza (`runtime_type = "io.containerd.runsc.v1"`). `systemctl restart k3s`. Apply a
   `RuntimeClass` whose `handler` exactly matches the runtime block name — standardize on
   **`runsc`** everywhere (runtime block = RuntimeClass handler = pod `runtimeClassName`).
4. **PodSecurity:** drop an `AdmissionConfiguration` (via `--kube-apiserver-arg
   --admission-control-config-file`) exempting `runtimeClasses: [runsc]` so the privileged hostPID
   checkpoint DaemonSet and agent pods schedule without per-namespace privileged labels.
5. **Deploy gardenio chart:** `helm install`, or drop the chart as a `HelmChart` manifest under
   `/var/lib/rancher/k3s/server/manifests/` so helm-controller installs it on boot. Use the
   bundled `local-path` default StorageClass for postgres/NATS PVCs and ServiceLB for any
   LoadBalancer Service; Traefik is already present.
6. **Freeze/thaw wiring:** the gardenio checkpoint DaemonSet must be `privileged` + `hostPID`,
   mount the runsc/shim binaries and k3s's containerd runsc `--root` state dir, plus a hostPath
   thaw-state volume (unique image-path subdir per snapshot). Confirm gardenio exec's raw
   `runsc checkpoint|restore`, **not** CRI `CheckpointContainer`, and passes identical
   `--root`/platform flags on restore.
7. **Backup/DR:** enable `k3s etcd-snapshot save` on an `etcd-snapshot-schedule-cron` with
   retention + S3 off-site (works with the SQLite datastore). **Separately** back up local-path PV
   host data under `/var/lib/rancher/k3s/storage/` (Velero or filesystem/EBS snapshot) — the
   datastore records PV *objects* but **not** their contents. Always include the `token` with any
   db snapshot (it encrypts datastore secrets).

## Open questions for the implementation phase

1. Confirm gardenio's freeze/thaw exec's raw `runsc checkpoint|restore` against the sandbox
   state/bundle dir and does **not** call the kubelet/CRI `CheckpointContainer` API (unimplemented
   for runsc on all containerd distros). *(Repo evidence: `freezethaw_exec.go` already chroots to
   `/host` and runs raw `runsc` — this looks satisfied, but verify on the k3s containerd root.)*
2. Verify the exact k3s containerd runsc `--root` path on the pinned v1.33 build (under
   `/run/k3s/containerd` or `/var/lib/rancher/k3s/...`) so the DaemonSet `--root` and the
   restore-time `--root` match; flag drift is a top failure mode.
3. Determine which template file the pinned k3s version renders (`config.toml.tmpl` for containerd
   1.7 vs `config-v3.toml.tmpl` for containerd 2.0) and use the correct CRI plugin path in the
   runsc stanza.
4. Validate `runsc checkpoint --leave-running` + restore end-to-end on the target EC2 instance
   type, including the platform flag (KVM vs systrap/ptrace) and `--exclude-committed-zero-pages`
   for large LLM agent memory; ensure live exec/REPL sessions are drained before checkpoint
   (gvisor#11439).
5. Decide the thaw-state hostPath layout to guarantee a unique image-path per snapshot and a
   cleanup policy that only deletes after `runsc wait --restore`.
6. Settle the PV/DR backup mechanism (Velero vs EBS/filesystem snapshot of
   `/var/lib/rancher/k3s/storage`) since datastore snapshots exclude PV data for postgres/NATS.
7. Confirm the `AdmissionConfiguration` runtimeClass exemption is acceptably scoped
   (`runtimeClasses: [runsc]` is broad) and the agent namespace is locked down accordingly.
8. Default to **SQLite/kine** for the single node; if embedded etcd is ever considered, avoid the
   etcd 3.5→3.6 skip hazard on v1.34+.
9. Pin `--node-ip` and `--tls-san` so an EC2 stop/start that changes the private IP does not
   invalidate apiserver cert SANs after reboot.

## Sources

Official docs and issue trackers consulted (2025/2026): k3s.io docs (architecture, air-gap,
advanced containerd config, backup/restore), docs.k0sproject.io (containerd runtime config,
backup/restore), kind.sigs.k8s.io (containerdConfigPatches, known issues), microk8s.io docs
(containerd, hostpath-storage, MetalLB, restore-quorum); gVisor docs (runsc, checkpoint/restore,
Kubernetes runtimeclass); issues gvisor#11810, gvisor#11439, gvisor#11313, containerd#12280,
canonical/microk8s#5093, canonical/microk8s#1022, canonical/microk8s#4691, kind#2262. Repo
cross-references: `gardenio/docs/gvisor-runsc-prod.md`, `gardenio/docs/fast-interruptible-freeze.md`,
`gardenio/controller/internal/podmanager/freezethaw_exec.go`,
`gardenio/helm/gardenio/templates/gvisor-namespace.yaml`.
