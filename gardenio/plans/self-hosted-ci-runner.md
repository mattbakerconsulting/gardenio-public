# Self-hosted CI runner on the gardenio cluster

**Status:** planned 2026-09-16, blocked on the cluster existing. **Owner:** infra step.

## Why

GitHub-hosted runners cost us two ways. The free tier is 2,000 minutes/month
and one full image push is ~140 billable minutes (agent 29m, coopmux 12m,
coop-claude 12m, ~20 small jobs); 25 pushes exhausted September's quota in a
day. And the hosted runner has ~14 GB of usable disk, which the agent image
(peaks ~22 GB) only fits after deleting the runner's preinstalled toolchains.

A runner on our own cluster has no minute quota, whatever disk we give it, and
a **persistent BuildKit cache** — so the agent image rebuilds incrementally
instead of from scratch every time. Path filtering (`images.yml` `changes`
job) cuts the typical push to ~15 minutes on hosted runners; this plan removes
the ceiling entirely for the pushes that do touch the heavy images.

## Shape: on the cluster, not beside it

Run **actions-runner-controller (ARC)** with a `gha-runner-scale-set` on the
same EKS cluster gardenio runs on. Same control plane (already paid for), same
Karpenter, same spot capacity, and ARC scales the runner set to **zero**
between jobs, so idle cost is nil. A standalone EC2 runner would be a second
thing to patch, back up and forget about.

### Node pool: reuse `ephemeral_pool`

`terraform/modules/karpenter/nodepool_ephemeral.tf` already defines an optional
dedicated NodePool + EC2NodeClass with its own taint/label, configurable root
volume, arch / capacity-type / instance-family / size requirements,
`expireAfter`, and `consolidationPolicy: WhenEmptyOrUnderutilized`. That is
exactly a CI pool. Compose it as:

| requirement | value | why |
| --- | --- | --- |
| `kubernetes.io/arch` | **`amd64` only** | builds hardcode x86: `GOARCH=amd64` in `controller/Dockerfile`, `x86_64`/`amd64` tarballs in the agent image. Graviton needs a multi-arch pass first (later, separately). |
| `karpenter.sh/capacity-type` | `spot` | CI is interruptible; ARC re-queues a job whose runner dies |
| instance family / size | `m6i` `c6i` `m7i` / `xlarge`+ | 4 vCPU/16 GB is the floor for the agent build; spot across three families keeps availability up |
| root volume | **100 Gi gp3** | agent peaks ~22 GB with no cache; a persistent cache needs room to be useful |
| taint | `ci-runner=true:NoSchedule` | nothing but runner pods lands here |
| `expireAfter` | `24h` | fresh AMI daily, bounded blast radius |

### ARC

Add `arc.tf` to `terraform/modules/platform/` (next to `traefik.tf`,
`cert_manager.tf`): Helm-install `gha-runner-scale-set-controller` and one
`gha-runner-scale-set` named **`gardenio-ci`** with `minRunners: 0`,
`maxRunners: 4`, `containerMode.type: dind`, the ephemeral pool's nodeSelector
+ toleration, and an ephemeral volume claim for the runner's work dir.

`dind` first, because it needs **zero workflow changes** — `docker/setup-buildx`
and `docker/build-push-action` work as-is. Kubernetes-mode (`kubernetes`
container mode + BuildKit rootless) is leaner and avoids privileged pods;
switch to it once the pipeline is proven.

**Auth:** a GitHub App (repo-scoped: Actions R/W, Administration R/W,
Metadata R) rather than a PAT — rotatable, no user identity, no expiry
surprises. Store app id / installation id / private key in **SSM Parameter
Store** and pull them with the chart's existing `external_secrets.tf`
plumbing (SSM is free; this is the same swap the minimal build makes for the
rest of Secrets Manager).

### Workflow changes

1. Make the runner a matrix field in `images.yml`:
   `runs-on: ${{ matrix.runner || 'ubuntu-latest' }}`, with
   `runner: gardenio-ci` on **agent, coop-claude, coopmux, kbeads** (the
   compile-heavy four). The ~20 thin wrappers finish in 2–5 minutes each and
   can stay hosted, or move later with one edit.
2. Drop the agent job's "free disk space" step and post-build `df` **only when
   `matrix.runner` is set** — on our node they are meaningless.
3. Cache: `type=gha` still works from self-hosted runners but round-trips
   GitHub's cache service over the network. Prefer
   `type=registry,ref=ghcr.io/mattbakerconsulting/gardenio/cache:<image>` (a
   throwaway package) or simply rely on the node's local BuildKit cache, which
   persists while the node lives (`expireAfter: 24h`).
4. `ci.yml` (Go ×2 + Rust, ~14 min/push) stays on `ubuntu-latest` — cheap,
   and it keeps a hosted-runner control group so we notice if the self-hosted
   path drifts.

### Cost

m6i.xlarge spot in us-east-1 ≈ $0.05–0.07/hr. At ~3 h of builds/day that is
**~$5–7/month**, zero while idle, and the node also absorbs the disk problem.
Hosted overage is $0.008/min ≈ **$1.12 per full push**. Break-even is about
six heavy pushes a month.

### Security notes

- Private repo, so the usual "never self-host for public repos" warning does
  not apply (fork PRs cannot reach it).
- Runner pods get **no** cluster credentials. They only need GHCR push, which
  `GITHUB_TOKEN` already provides. If we ever push to ECR, add an IRSA role to
  the runner service account then — not before.
- `dind` means a privileged pod. Acceptable on a tainted, ephemeral, CI-only
  node pool; still the reason to move to Kubernetes-mode later.

## Sequencing

1. Minimal gardenio cluster exists (EKS + Karpenter + `ephemeral_pool`).
2. `arc.tf` + GitHub App + SSM params → `gardenio-ci` scale set healthy,
   `minRunners: 0` observed scaling from zero on a `workflow_dispatch`.
3. Flip the four heavy images to `runner: gardenio-ci`; compare agent build
   time hosted vs self-hosted (expect the second build to be a fraction of the
   first thanks to cache).
4. Decide whether to move the wrappers too.

Until step 1, the only way to run CI is GitHub's billing (add a payment
method with a small spending limit); the path filter keeps that cheap.

## Open questions

- GitHub App vs fine-grained PAT (plan says App; a PAT is faster to bootstrap).
- Move `ci.yml` too, or keep it hosted as a control group (plan says keep).
- Multi-arch images (would unlock Graviton, ~20% cheaper spot) — separate plan.

## Update 2026-09-16 — EKS abandoned before use

The EKS composition above was applied on 2026-09-16 (control plane, VPC,
IAM, budget) and **destroyed the same hour**, before any node ran. The
$73/month control-plane fee was the reason: it is the single largest line
in a personal-billing budget and buys nothing until there are workloads.
The cluster will be **k3s on spot instances** instead. Consequences for
this plan: no ARC — the CI runner will be a classic `actions/runner`
service, with the user's laptop as the primary runner and a spot instance
as overflow; the network wiring (public subnets, no NAT, gateway endpoints
only), the remote-state bootstrap and the finops budget alert are kept in
`terraform/composition/gardenio/` and carry over unchanged.

## Update 2026-09-16 — cluster is k3s, no cloud runner in phase 1

The cluster is k3s on spot (`terraform/modules/k3s`, see `docs/k3s-cluster.md`),
~$26/month. There is no ARC and no cloud CI runner: the laptop is the sole
runner (label set `[self-hosted, linux, x64, gardenio]`). If overnight queueing
bites, a classic `actions/runner` on a k3s node carrying the same labels is a
one-node ASG away.
