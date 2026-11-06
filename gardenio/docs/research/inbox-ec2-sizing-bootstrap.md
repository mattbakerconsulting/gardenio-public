# Gardenio in a Box — EC2 instance sizing + user-data bootstrap (RESEARCH/SPIKE)

**Bead:** ka-0000003889 · **Parent epic:** ka-0000001295 (Gardenio in a Box: single-EC2 full
install, in-box kube, zero external cloud deps) · **Operator:** Matt Baker
(oidc:matthew.baker@acme.example)

> **Status: research/spike.** This designs a *new* single-box topology. There is **no**
> single-EC2 / k3s / single-node profile in the repo today — the canonical deploy
> (`gardenio/terraform/composition/poc` + EKS + Karpenter + external Aurora RDS + ACM/NLB)
> is multi-node. The closest existing artifact is the **BYO-cluster** path
> (`deploy-values/gardenio-byo.yaml` + `scripts/deploy-byo.sh`), which this spike reuses as
> the helm-install half of the bootstrap. Numbers below are computed from the committed
> Helm chart (`gardenio/helm/gardenio/values.yaml`, appVersion `2026.159.13`); EC2/distro
> facts are externally sourced and adversarially fact-checked.

---

## TL;DR

| Question | Answer |
|---|---|
| **Distro** | **k3s** — single binary, embedded containerd, batteries included (Traefik v3, ServiceLB/Klipper, local-path-provisioner, CoreDNS), turnkey gVisor + air-gap import. |
| **Sizing driver** | **Agent-pod memory.** Every agent reserves a hard **16 GiB** (limitRange request==max). Infra is only ~6 GiB. Box size ≈ `infra + (N_agents × 16 GiB)`, RAM-bound. |
| **Instance shape** | Memory-optimized **r7i** (8 GiB/vCPU exactly matches the 2 vCPU / 16 GiB agent ratio). m7i over-buys CPU; c-family starves RAM. |
| **Demo (1–2 agents)** | `r7i.2xlarge` (8 vCPU / 64 GiB) + 150 GiB gp3 root. |
| **Small team (4–6 agents)** | `r7i.4xlarge` (16 vCPU / 128 GiB) + 250 GiB gp3 — comfortable at 4–5, 6 at burst. |
| **Heavier (8–12 agents)** | `r7i.8xlarge` (32 vCPU / 256 GiB) + 400 GiB gp3. |
| **Bootstrap** | EC2 user-data → k3s install → (optional) runsc shim → image load → seed secrets → `helm install` w/ in-box overlay → rollout + smoke. Reuse `scripts/deploy-byo.sh` for the last three stages. |

---

## 1. Resource budget (computed from `values.yaml`)

These are **measured from the committed chart**, not estimated. Per-component memory
*requests* (the scheduling-binding number):

| Component | CPU req | Mem req | CPU lim | Mem lim | PVC | In-box default |
|---|---|---|---|---|---|---|
| controller | 100m | 128Mi | 500m | 256Mi | — | on |
| coopmux | 100m | 256Mi | 500m | 512Mi | — | on (agents) |
| postgres (pgvector) | 250m | 256Mi | 1 | 1Gi | 10Gi | on (in-box) |
| beads (kbeads) | 100m | **2Gi** | 500m | 2Gi | — | on |
| nats (jetstream) | 100m | 128Mi | 500m | 512Mi | 2Gi | on |
| keycloak | 250m | **1Gi** | 2 | 2Gi | 8Gi | on (public-safe) |
| litellm | 200m | **1Gi** | 2 | 2Gi | — | on (if LLM via proxy) |
| edgeAuth/forwardauth | 50m | 64Mi | 200m | 256Mi | — | on (public-safe) |
| vmsingle | 100m | 256Mi | 500m | 512Mi | 10Gi | optional |
| vmagent / vmalert | 50m | 64Mi | 200m | 128Mi | — | optional |
| grafana | 100m | 128Mi | 500m | 256Mi | 1Gi | optional |
| beads-viewer | 50m | 128Mi | 200m | 512Mi | — | optional |
| bridges (slack/jira/gitlab/wl/push) | ~50m ea | 48–64Mi ea | — | — | ~0.1–1Gi ea | per-integration |
| landing / triggerEngine / rolesServer | 10–50m | 16–64Mi | — | — | — | optional |

**Aggregate (computed):**

- **CORE infra** (controller, coopmux, postgres, beads, nats, keycloak, litellm, forwardauth):
  **1.15 vCPU / 4.8 GiB requests**; limits 7.2 vCPU / 8.5 GiB.
- **Full infra** (every infra pod enabled): **~1.8 vCPU / 5.9 GiB requests**; limits ~10.2 vCPU / 11.0 GiB.
- **In-box PVCs** (local-path, on the root/EBS volume): postgres 10Gi + nats 2Gi + keycloak 8Gi
  + vmsingle 10Gi + grafana 1Gi + small state PVCs ≈ **~32 GiB** when fully featured;
  **~14 GiB** for a minimal core (postgres + nats + small state). Note: if a profile points at
  external RDS instead of in-box postgres, drop 10 GiB; if keycloak is off (BYO), drop 8 GiB.

> **The binding fact:** `limitRange.defaultRequest = 2 vCPU / 16 GiB`, `max = 4 vCPU / 16 GiB`
> (`values.yaml:64-77`). Request == max == 16 GiB, so **every concurrent agent reserves a
> full 16 GiB regardless of actual use**. Infra (~6 GiB) is noise next to one agent.
> `resourceQuota` hard cap (200 pods / 200 vCPU / 1Ti, `values.yaml:5015`) never binds before
> instance memory does. `claudeTeams` agent pods are bigger — **4 vCPU / 24 GiB** request
> (`values.yaml:1643`) — and would be *rejected by the namespace LimitRange max (16 GiB)*, so
> teams-mode agents need their own fleet namespace/quota.

**Sizing formula:** `RAM_needed = 6 GiB (infra) + N × 16 GiB (agents)`, then divide by **0.82**
for OS + kubelet + containerd + gVisor-sentry headroom.

---

## 2. Instance-type recommendation

Workload is **RAM-bound at 8 GiB/vCPU** (2 vCPU per 16 GiB agent). That is exactly the
**r-family** ratio. `m7i` (4 GiB/vCPU) forces buying 2× the CPU to hit the RAM target;
`c-family` runs out of RAM long before CPU. Use **r7i** (current-gen, Intel Sapphire Rapids).

| Scenario | Agents | RAM needed | ÷0.82 floor | **Instance** | vCPU/RAM | gp3 root | ~$/hr* |
|---|---|---|---|---|---|---|---|
| **A — Demo/eval** | 1–2 | 38 GiB | ~46 GiB | **r7i.2xlarge** | 8 / 64 GiB | 150 GiB | ~$0.53 |
| **B — Small team** | 4–6 | 102 GiB | ~124 GiB | **r7i.4xlarge** | 16 / 128 GiB | 250 GiB | ~$1.06 |
| **C — Heavier** | 8–12 | 198 GiB | ~242 GiB | **r7i.8xlarge** | 32 / 256 GiB | 400 GiB | ~$2.12 |

\* On-demand us-east-1, approximate, drifts by region/date — confirm with the pricing API.

- **CPU is never the limit:** Scenario C needs only ~26 vCPU of requests against 32 available.
- **Scenario B caveat:** 6 agents (102 GiB) on a 128 GiB box leaves ~3% headroom → kubelet
  eviction risk. Treat r7i.4xlarge as **4–5 steady / 6 burst**, or step to `r7i.6xlarge`
  (24 vCPU / 192 GiB) if 6 concurrent must be guaranteed.
- **claudeTeams:** at 24 GiB/pod these drop the per-scenario counts ~1.5× (C holds ~10, not 12).
- **gVisor overhead is negligible to sizing:** ~15 MB runsc sentry/pod + an optional declared
  `gvisor.runtimeClass.overhead.podFixed` (~120Mi, currently a commented example). Syscall
  interception costs CPU *latency*, not standing RAM.

**EBS gp3 root** must cover OS+containerd image cache (~20–30 GiB; agent images are large),
the ~14–32 GiB of local-path PVCs, plus per-agent scratch. gp3 baseline (3000 IOPS / 125 MB/s)
is adequate; bump IOPS only if postgres+vmsingle I/O contend. **Snapshot or split state onto a
dedicated data volume** — all PVCs are unreplicated local-path; EBS loss = data loss.

```sh
aws ec2 run-instances --instance-type r7i.2xlarge \
  --block-device-mappings 'DeviceName=/dev/xvda,Ebs={VolumeSize=150,VolumeType=gp3}'   # A
aws ec2 modify-volume --volume-id <vol-id> --size <newGiB>                             # online-grow
```

---

## 3. Distro: **k3s**

Compared k3s / k0s / kind / k3d / microk8s. Two camps:

- **kind, k3d** — run k8s *inside Docker containers*, nodes as root, designed for dev/CI &
  ephemeral use. Rule out for a public box.
- **microk8s** — production-capable but delivered **only as a snap** → snapd is a hard runtime
  dependency that fights the no-external-deps / air-gap / single-`curl|sh` goal. Rule out.
- **k3s vs k0s** — architecturally near-identical (single binary, embedded etcd, ~650–750 MB
  idle incl. OS). **k3s wins for this target** because it ships the *entire serving stack* out
  of the box: containerd, CoreDNS, **Traefik v3** ingress (80/443), **ServiceLB/Klipper**
  (satisfies `type=LoadBalancer` with no cloud LB), **local-path-provisioner** (dynamic local
  PVs for postgres/nats/beads), metrics-server, embedded Helm controller. k0s ships leaner and
  expects you to add ingress/LB/storage yourself.

**Sizing floor:** do **not** attempt below 2 vCPU / 4 GiB — all self-hosting distros became
unusable at exactly 1 GiB RAM in 2025 benchmarks (API server stops once a pod starts).

**Install one-liner** (disable bundled Traefik/ServiceLB only if you front with your own):
```sh
curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC='--write-kubeconfig-mode 644' sh -
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
```

**Air-gapped image import** (two ways): (1) **preferred** — drop tarballs into
`/var/lib/rancher/k3s/agent/images/` and k3s auto-loads on start (survives restarts);
(2) `k3s ctr images import <file>.tar`. Manually-imported images can be GC'd under
DiskPressure — pin with labels `io.cri.containerd.pinned=pinned` /
`io.cattle.k3s.pinned=pinned`.

---

## 4. gVisor (runsc) on a single k3s node — **RuntimeClass-only mode**

The chart decouples the concerns (verified in `templates/gvisor.yaml`):
- `gvisor.runtimeClass.create` (default **true**) renders the RuntimeClass.
- `gvisor.nodePool.create` (default **false**) renders the **Karpenter** EC2NodeClass/NodePool
  — that path requires Karpenter CRDs + subnet/SG/role and **hard-fails in-box**.
- The controller's only routing is `pod.spec.runtimeClassName` from the `runtime_class`
  metadata (`podspec.go:175`). **No Karpenter dependency in the scheduling path** — a pod runs
  on runsc purely by carrying `runtimeClassName: runsc`.

**In-box recipe:** `gvisor.enabled=true`, `gvisor.runtimeClass.create=true`,
`gvisor.nodePool.create=false`, `gvisor.freezeThaw.enabled=false`, `gvisor.namespace.name=""`
(sandbox-only; no PSA-privileged namespace / probe DaemonSet needed).

> **⚠ Two corrections the adversarial verifier caught — get these right:**
>
> 1. **containerd config schema/filename depends on the k3s/containerd version.** Current k3s
>    (Feb-2025 releases, `v1.31.6+k3s1` / `v1.32.2+k3s1` and later) ships **containerd 2.x**,
>    which renders **`config-v3.toml.tmpl`** with schema
>    `[plugins.'io.containerd.cri.v1.runtime'...]`. Older k3s (containerd 1.7) uses
>    **`config.toml.tmpl`** with schema `io.containerd.grpc.v1.cri`. **Pairing the v3 schema
>    with the v2 filename silently registers nothing.** Check `k3s --version` first. Use the
>    k3s base-template merge (`{{ template "base" . }}`) — do **not** `cat` the rendered
>    `config.toml` into the template (k3s explicitly warns against it, and it has a first-boot
>    ordering bug).
> 2. **Single-node scheduling.** The chart's RuntimeClass carries
>    `scheduling.nodeSelector {gvisor: enabled}` + a `NoSchedule` toleration (assumes a
>    dedicated pool). On one node you must **either** label the node
>    (`kubectl label node <n> gvisor=enabled`) **or** set `gvisor.runtimeClass.scheduling={}`
>    to drop the selector entirely. **Do not** hand-`kubectl apply` a second `runsc`
>    RuntimeClass — it collides with the chart's (which carries
>    `helm.sh/resource-policy: keep`). Pick one owner.
>
> **Platform:** use `--platform=systrap` on EC2 **Nitro guest** instances (no nested virt; no
> `/dev/kvm`). `--platform=kvm` only works on `.metal` bare-metal types. `systrap` is the
> default, so the explicit pin is optional. Pin the **runsc release** (e.g. a tested
> `release-YYYYMMDD.0`) rather than fetching `latest`.

```sh
# install runsc + shim (pin the release; verify checksums separately, not doubled -c)
ARCH=$(uname -m); URL=https://storage.googleapis.com/gvisor/releases/release/<PINNED>/${ARCH}
wget ${URL}/runsc ${URL}/containerd-shim-runsc-v1
sudo install -m 0755 runsc containerd-shim-runsc-v1 /usr/local/bin/

# containerd 2.x (current k3s): write config-v3.toml.tmpl, merging the base
sudo tee /var/lib/rancher/k3s/agent/etc/containerd/config-v3.toml.tmpl >/dev/null <<'EOF'
{{ template "base" . }}

[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runsc]
  runtime_type = 'io.containerd.runsc.v1'
EOF
sudo systemctl restart k3s
kubectl label node "$(kubectl get nodes -o name | head -1 | cut -d/ -f2)" gvisor=enabled --overwrite
```

---

## 5. Bootstrap path (EC2 user-data)

Ordered stages. The repo already ships the load-bearing pieces this reuses:
`deploy-values/gardenio-byo.yaml` (self-contained, ACME-free values profile) and
`scripts/deploy-byo.sh` (seeds the 3 core secrets via `openssl rand`, `helm lint`,
`helm upgrade --install --wait --timeout 10m`, then runs `scripts/smoke-byo.sh`).
`deploy-byo.sh` targets whatever kubeconfig is current and **never** calls
`aws eks update-kubeconfig` — pointing `KUBECONFIG=/etc/rancher/k3s/k3s.yaml` is sufficient.

```sh
#!/usr/bin/env bash
# EC2 user-data → full gardenio in-a-box. Run as #cloud-config runcmd or raw user-data.
set -euxo pipefail   # cloud-init failures are silent; tail /var/log/cloud-init-output.log

# 1. OS prep
swapoff -a && sed -i.bak '/ swap / s/^/#/' /etc/fstab
modprobe overlay br_netfilter
printf 'overlay\nbr_netfilter\n' >/etc/modules-load.d/k8s.conf
printf 'net.bridge.bridge-nf-call-iptables=1\nnet.ipv4.ip_forward=1\n' >/etc/sysctl.d/99-k8s.conf
sysctl --system
# Ubuntu: apt-get update && apt-get install -y curl ca-certificates openssl jq git
# AL2023: dnf install -y curl openssl jq tar git

# 2. in-box k8s (k3s, embedded containerd)
curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC='--write-kubeconfig-mode 644' sh -
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
until kubectl get nodes 2>/dev/null | grep -q ' Ready'; do sleep 3; done

# 3. (OPTIONAL) gVisor — only if running sandboxed agents. See §4; mind containerd v2/v3.

# 4. images — air-gapped tarball baked into AMI, OR rely on public-registry pull (chart default)
for t in /opt/gardenio/images/*.tar; do k3s ctr images import "$t" 2>/dev/null || true; done

# 5+6+7 — delegate to the repo's existing BYO deploy path (seeds secrets, helm install, smoke)
cd /opt/gardenio/repo
NAMESPACE=gardenio RELEASE=gardenio RUN_SMOKE=1 \
  ./scripts/deploy-byo.sh --namespace gardenio --values deploy-values/gardenio-byo.yaml
```

**Manual equivalents** (if not delegating to `deploy-byo.sh`):
```sh
kubectl -n gardenio create secret generic gardenio-postgres-password --from-literal=password="$(openssl rand -hex 24)"
kubectl -n gardenio create secret generic kd-beads-token           --from-literal=token="$(openssl rand -hex 24)"
kubectl -n gardenio create secret generic coop-credential-key      --from-literal=key="$(openssl rand -hex 32)"
helm upgrade --install gardenio /opt/gardenio/repo/gardenio/helm/gardenio -n gardenio --create-namespace \
  --values /opt/gardenio/repo/deploy-values/gardenio-byo.yaml --wait --timeout 10m
```

### Bootstrap risks / gotchas
- **`deploy-values/gardenio-byo.yaml` turns keycloak/forwardauth/edgeAuth/TLS OFF and exposes
  ClusterIP only.** That contradicts the epic's HARD public-internet-safety requirement
  (ka-0000003100 / ka-0000002581 / ka-0000004895). A real public in-box profile needs a **new
  overlay** that re-enables keycloak SSO + traefik ingress + TLS + forwardauth-on-all-routes —
  that is sibling research, not this bead, but the bootstrap must point at *that* overlay, not
  byo. (The 6 GiB infra budget above already assumes keycloak/litellm/forwardauth ON.)
- **Secret seeding is idempotent (`ensure_secret` leaves existing alone)** — good for re-runs,
  but a fresh helm install against a half-seeded namespace can mismatch the postgres password
  vs an already-initialized PVC. Seed before first postgres start.
- **Air-gap tarball must cover every image the enabled subcharts reference** (kbeads, pgvector,
  nats, keycloak, litellm, controller/coopmux, viewer if on) — a partial bundle = ImagePullBackOff
  with no registry fallback.
- **`--wait --timeout 10m`** can expire on a small box while pgvector initializes its PVC on
  slow disk — bump the timeout or pre-warm.
- **k3s regenerates containerd config on every restart** — runsc handler must live in the
  `*.tmpl`, never a raw `config.toml` edit.
- **Single box = no HA.** Instance/AZ failure takes the whole platform down. Acceptable for
  demo/small-team POC; snapshot EBS + keep an AMI/user-data rebuild path for anything beyond.

---

## 6. Hand-off to the epic

- **Sizing & distro are settled** (this bead). Implementation children should:
  1. Author the **public-safe in-box values overlay** (keycloak SSO + traefik + TLS +
     forwardauth, in-box postgres/nats, local-path storage) — depends on the security siblings.
  2. Turn §5 into a committed `scripts/bootstrap-ec2.sh` + a tested `#cloud-config` user-data
     template, parameterized by scenario (instance type / gp3 size).
  3. Decide gVisor default for in-box (recommend **on** for agent sandboxing on a public box,
     RuntimeClass-only per §4) and pin the runsc release + containerd-schema branch.
- **Open dependency:** the in-box bootstrap should target the new public-safe overlay, not
  `gardenio-byo.yaml` (which is deliberately auth-off / ClusterIP-only).
