# CI runners on the k3s cluster

Status: proposed 2026-09-17. Owner: laptop-claude. Supersedes the laptop runner
plan (`self-hosted-ci-runner.md`).

## Why

Both machines we own were used as GitHub Actions runners and both fell over:
the laptop's WSL VM is torn down by Windows Modern Standby mid-job, and the
Vultr box (4 vCPU, 7 GB) is Gas City HQ, where a `cargo build` or a docker
image build starves the city's supervisor and dolt. The user's rule is now:
**no CI on the laptop or on Vultr; compute-heavy jobs run on k3s runners.**
Cost still matters, so idle cost must be ~$0.

## Which repos need this

| Repo | Visibility | CI plan |
|---|---|---|
| `mattbakerconsulting/gardenio` | private | self-hosted on k3s (this plan) |
| `joyousaversion/gascity` | public fork | GitHub-hosted, free for public repos; upstream workflows run as-is |
| `mattbakerconsulting/gascity-hq` | private mirror | Actions disabled; scratch only. Anything needing CI goes through a branch on the public fork |

So this is a gardenio concern (and any future private repo).

## Design

1. **Runner scale set via ARC.** Install actions-runner-controller in
   `gha-runner-scale-set` mode on the existing cluster (`k3s.clonk.tv`):
   `gha-runner-scale-set-controller` in namespace `arc-systems`, one
   `gha-runner-scale-set` per repo in `arc-runners`, `minRunners: 0`,
   `maxRunners: 2`. Workflows switch from
   `runs-on: [self-hosted, linux, x64, gardenio]` to `runs-on: gardenio-k3s`.
   Auth with a GitHub App scoped to the repo (preferred) or, until one exists,
   the PAT already in SSM `/gardenio/ci/github-token`.
2. **A dedicated spot node pool for CI.** The three `t3.small` etcd nodes
   (2 GB each) cannot build coop. Add a `ci` autoscaling group to
   `terraform/modules/k3s`: spot, instance types `m6a.xlarge, m5a.xlarge,
   m6i.xlarge, m5.xlarge` (4 vCPU / 16 GiB; every type must fit the pod
   request — an 8 GiB c6a.xlarge could not), 100 GB gp3 root, `min 0`,
   `max 2`, joined as agents (not servers), taint `ci=true:NoSchedule`, label
   `gardenio.io/role=ci`. Runner pods request `3 CPU / 12 Gi` and tolerate the
   taint, so nothing else lands there and no runner lands on an etcd node.
3. **Scale-from-zero with cluster-autoscaler.** Deploy cluster-autoscaler with
   the AWS provider and ASG auto-discovery
   (`k8s.io/cluster-autoscaler/enabled`, `.../gardenio` tags), IAM allowing
   `autoscaling:Describe*`, `SetDesiredCapacity`, `TerminateInstanceInAutoScalingGroup`,
   `ec2:Describe*`. A pending runner pod scales the ASG 0→1; the autoscaler
   scales back to 0 after ~10 idle minutes. This works on k3s: the autoscaler
   only needs the ASG and the node object, not a managed control plane.
4. **Docker builds.** Scale set in `dind` container mode so `images.yml` keeps
   `driver: docker`. Layer cache does not survive a pod, so push
   `--cache-to type=registry,ref=ghcr.io/mattbakerconsulting/gardenio/cache/<image>`
   and `--cache-from` the same. No `type=gha` (that is the paid cache).
5. **Ephemeral tmp.** Go links through `TMPDIR`; runner pods get an
   `emptyDir` on the node's 100 GB disk rather than a tmpfs.

## Cost

| Item | Estimate |
|---|---|
| `m6a.xlarge` spot, us-east-1 | ~$0.06/h → ~$2.50/mo at 10 job-hours/week |
| 100 GB gp3 while a node exists | ~$0.01/h |
| ARC + autoscaler pods on existing nodes | $0 (fits in the t3.small headroom, ~200 Mi total) |
| Idle | $0 |

## Steps

1. Terraform: `ci` ASG + launch template (agent join via the existing
   `k3s-join.clonk.tv` + SSM token path), IAM policy for the autoscaler on the
   server instance role, tags for auto-discovery. `terraform apply` in
   `composition/gardenio`.
2. Helm: cluster-autoscaler (`kubernetes/autoscaler` chart), ARC controller,
   ARC scale set `gardenio-k3s`. Values checked in under `gardenio/helm/ci/`.
3. Workflows: `runs-on: gardenio-k3s` in `ci.yml`, `images.yml`, `checks.yml`;
   drop the `CI_RUNNER_READY` gate once a run is green; add registry cache to
   the image build.
4. Retire the machine runners: delete `laptop-fimbaz`, `laptop-fimbaz-2` and
   `vultr-claude` from the repo's runner list; remove the units.
5. Re-run `ci`, `checks`, `images` on `main` and watch the ASG go 0→1→0.

## Open questions

- Spot reclaim mid-job: the runner pod dies and the job fails. Acceptable at
  first (re-run); if it bites, add `capacity-optimized` allocation and
  a second instance family.
- GitHub App vs PAT: the PAT is broader than needed. Create the App when the
  user is at a keyboard; it needs an org/user-level install click.
