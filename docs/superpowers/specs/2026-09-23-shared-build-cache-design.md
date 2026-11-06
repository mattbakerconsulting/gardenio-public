# Shared persistent build cache: findings and design

Status: drafted 2026-09-23 against `main` (125d0f5d) and the live `gardenio-k3s`
cluster. Author: laptop-claude for the operator.
Companion plan: `docs/superpowers/plans/2026-09-23-shared-build-cache.md`
(written after this spec is approved).

This document is the evidence and the decisions. The plan is the work. **§0 is
binding.** Anything marked **UNCONFIRMED** is a Phase 0 spike, not a fact.

## Why now: the reopen condition was met

Decision gd-vKoFE1xqOr (2026-09-22) declined a central cache server because the
free node-local hostPath cache (`7cac2cca` and follow-ups) was capturing most of
the win. That ruling named its own reopen condition: *"a measured change in
node lifetime"*. On 2026-09-23 that condition holds:

- Of 13 live spot nodes, most were **3 minutes to 2 hours old**; even an
  on-demand node was 3 minutes old. The per-node cache assumed nodes live
  "hours to days".
- The operator's direction is **big, cheap spot instances that scale up and
  down with bursty load** (gd-bPVTlMeWV6). More churn means a node-keyed cache
  hits less, by construction.
- Live `du` on four heavy runners (2026-09-23): `go-build` ranged **171 MB to
  3973 MB** across nodes aged 7 to 135 minutes. The cache is being rebuilt on
  every node, and a fresh node starts with nothing.

The operator's thesis, to be measured rather than assumed: shorter jobs
free the spot capacity sooner, so **a persistent cache lowers node-hours per
PR** by more than it costs to run.

## 0. Decisions (binding)

| # | Decision | Why |
|---|---|---|
| C1 | **One central cache service, backed by a gp3 EBS volume through the AWS EBS CSI driver.** It is not node-local and it is not S3. | EBS is the only option where the cache survives node loss without putting AWS credentials in runner or agent pods. `local-path` volumes die with their node, and on-demand nodes are replaced too. With S3, every client would need IAM credentials. Agent pods can't get object-store credentials today (gd-W8OwRoHLLn), and runners would lean on the node-role hole (gd-9ip3Fsen5c) instead of closing it. |
| C2 | **The server is SeaweedFS in single-process `weed server -s3` mode.** It is Apache-2.0 and actively maintained. | It is the one candidate that serves three consumers over one protocol (S3): Go through a GOCACHEPROG client, Rust through sccache's S3 backend, and Docker through buildx `type=s3`. It also enforces per-bucket write permissions *server-side*. MinIO is archived (2026-04-25, AGPL). bazel-remote only serves sccache and has no per-path auth. Garage is AGPL and has fewer features. |
| C3 | **Split namespaces: two buckets, `cache-main` and `cache-pr`.** Main-branch CI reads and writes `cache-main`. PR CI and crew agents read `cache-main` and read and write `cache-pr`. | This is the operator's choice (2026-09-23). SeaweedFS enforces it with per-identity `Read:`/`Write:` bucket actions. Client read-only flags are hygiene, not enforcement. |
| C4 | **The main-writer key reaches only main-branch pushes, through a GitHub Environment (`build-cache-main`) with a deployment-branch rule of `main`.** It is not a repository secret. | Crew agents push branches to *this* repo, so a PR workflow receives every repo-level secret. An environment secret is only released to a job that targets that environment and passes its branch rule. The pr-writer key can be an ordinary repo secret because PRs are meant to hold it. |
| C5 | **The EBS CSI controller gets its own IAM credentials: a dedicated IAM user whose policy is scoped by tag. The node role gets no EBS permissions.** | There is one node role shared by all three pools, and IMDS hop limit 2 lets every pod use it (`modules/k3s/main.tf:324-325` et al., gd-adirNxx3Qo). Adding `ec2:CreateVolume`/`AttachVolume`/`DeleteVolume` there would give every pod volume control. The static key pair is a **credential**, so its creation is an **operator decision** (O1), not a crew's call. |
| C6 | **CI keeps a node-local L1 cache (the existing hostPath) in front of the remote store. Agents use a per-pod L1.** | Every remote read that L1 serves saves latency and cross-AZ bytes (§3). Keeping L1 does **not** weaken C3 in any way that isn't already true: every CI job gets a privileged `dind` sidecar (`containerMode: dind`), so a hostile CI job can already own the node, including anything a later main job does there. The README already records this as "not a new exposure class". What C3 buys is that **a PR job cannot write `cache-main` from its own credentials**, and that **agents**, which are not privileged, cannot write it at all. |
| C7 | **Rust moves to sccache with a per-pod `CARGO_TARGET_DIR`.** Keep `CARGO_HOME`/`RUSTUP_HOME` on the hostPath and set `CARGO_INCREMENTAL=0`. | Heavy runners now pack **two per node** (CPU request 1900m, `arc-runners-values.yaml:649`; two observed live on ip-10-90-29-31), which breaks the "one heavy runner per node" argument for a shared target dir (gd-kuQP7w43HC). sccache cannot cache incremental crates or linker-invoking crates (bin, dylib, cdylib, proc-macro), so link time remains. |
| C8 | **The module proxy (Athens) is gated on a Phase 0 measurement, not built by default.** | CI runs inside AWS with a fast uplink. `go-mod` is ~1.2 GB per node. The proxy is worth building only if module download shows up as a measurable share of cold-node time. |
| C9 | **The hostPath Go cache stays until the remote path is measured at least as good.** It is L1 under C6 anyway. Nothing is removed before the numbers say so. | The standing rule in this repo is that cache claims are checked against the per-node distribution, never a mean or a comment (see `helm/ci/README.md`, "Node-local Go build cache"). |

## 1. What exists today (verified 2026-09-23)

- **Node-local cache.**
  - **Heavy scale set:** hostPath `/var/lib/gardenio-ci-cache` is mounted at `/cache` (`gardenio/helm/ci/arc-runners-values.yaml:516-520, 586-588`), with `GOCACHE`, `GOMODCACHE`, `CARGO_HOME`, `RUSTUP_HOME`, `CARGO_TARGET_DIR`, `GOTOOLS_BIN`, `GOLANGCI_LINT_CACHE` and `NODE_NAME` set at `:534-585`. The init container `prepare-build-cache` (`:474-515`) runs `rm -rf /cache/cargo-target` when free space is under 20 GB.
  - **Medium:** Go directories only (`arc-runners-medium-values.yaml:382-479`).
  - **Light:** no cache and no dind.
- **Rust cache is effectively unused.** cargo, rustup and cargo-target were about 1 MB on all four heavy nodes sampled. No sccache anywhere. `coop/.cargo/config.toml:1-3` anticipates `RUSTC_WRAPPER`, but nothing sets it.
- **Docker layer cache** goes to ghcr over the internet: `images.yml:897-898` uses `type=registry,ref=ghcr.io/<repo>/cache/<image>` with `mode=max` on non-PR runs, and the buildx driver is `docker-container` (`:558-560, :834-840`). `controller-base` deliberately has no cache (`50a8aeec`, a measured net loss because of the per-commit `COMMIT` ldflag).
- **Go** is pinned to `1.25.14` (`ci.yml:95`, `checks.yml:47`) and `setup-go` runs with `cache: false`. **Rust** is `1.93.0` (`coop/rust-toolchain.toml`).
- **Storage:** there is no CSI driver. The StorageClasses are `local-path` (default) and `local-path-retain`. The node role has no EBS permissions.
- **AZs:** all three ASGs span `us-east-1a` (`subnet-0123456789abcdef0`) and `us-east-1b` (`subnet-0123456789abcdef1`). CI nodes split 4/4 across them, and the two on-demand platform nodes sit one in each AZ. **Nodes carry no `topology.kubernetes.io/zone` label**; the zone appears only in `providerID`.
- **Network:** there are no NetworkPolicies, and runners already depend on cluster DNS and Services (the `dns-preflight` init container, `arc-runners-values.yaml:446-466`).
- **Crew agent pods** are built in Go, not by a chart: `controller/cmd/controller/podspec.go`, `internal/podmanager/manager.go:2028` for env and `volumes.go` for volumes. They run in namespace `gardenio` with SA `gardenio-agent`. `/workspace` is an emptyDir, and no Go or Cargo cache env is set. Project and role `EnvVars` come from beads (`internal/config/config.go:2196, 2252`); these are plain values, not secret references.
- **Triggers:** `ci.yml` and `checks.yml` run on `push: [main]`, `pull_request` and `workflow_dispatch`. Neither references any secret today.

## 2. Architecture

```
            us-east-1a (EBS volume lives here)          us-east-1b
  ┌──────────────────────────────────────────┐   ┌────────────────────────┐
  │ build-cache pod (SeaweedFS, S3 :8333)    │   │                        │
  │   PVC 200Gi gp3, Retain                  │◄──┤ runners / agents       │
  │   buckets: cache-main, cache-pr          │   │ (cross-AZ reads, §3)   │
  └──────────────▲───────────────────────────┘   └────────────────────────┘
                 │ svc s3.build-cache.svc.cluster.local:8333
  ┌──────────────┴────────────┐
  │ CI runner pod             │   L1: hostPath /cache (Go, cargo registry)
  │  GOCACHEPROG client ──────┤   L2: S3 cache-main | cache-pr
  │  sccache (RUSTC_WRAPPER)──┤
  │  dind: buildx type=s3 ────┘
  └───────────────────────────┘
  crew agent pod: same clients, L1 = per-pod dir, key = pr-writer
```

### 2.1 Storage (EBS CSI)
- The `aws-ebs-csi-driver` Helm chart. **Only the controller** needs AWS credentials, supplied from a Secret created out-of-band per C5. The node plugin needs none.
- The IAM user's policy is AWS's `AmazonEBSCSIDriverPolicy` shape, with create and delete conditioned on the `ebs.csi.aws.com/cluster=true` tag, so the key can only touch volumes the driver itself created.
- A new StorageClass `gp3-retain`: `ebs.csi.aws.com`, `type: gp3`, `reclaimPolicy: Retain`, `volumeBindingMode: WaitForFirstConsumer`, and `allowVolumeExpansion: true` (so we start at 200Gi and grow rather than guess).
- **Zone scheduling:** the driver labels nodes with its own topology key (`topology.ebs.csi.aws.com/zone`), and the PV's nodeAffinity uses that key. This means we don't need the missing `topology.kubernetes.io/zone` label. **UNCONFIRMED on k3s without a cloud provider; Phase 0 checks it.**

### 2.2 The cache service
- SeaweedFS, installed from its upstream chart with a values file at `gardenio/helm/ci/seaweedfs-values.yaml`. This matches how `arc-runners-*-values.yaml` sit next to the ARC charts.
- One replica, run as a StatefulSet with the `gp3-retain` PVC. Service `s3` in a new namespace `build-cache`, addressed by FQDN `s3.build-cache.svc.cluster.local` everywhere: nested containers in dind may not inherit the pod's DNS search domains.
- **Scheduling:** prefer the on-demand platform node in the volume's AZ, and tolerate spot in that same AZ as a fallback. When the pod moves, the volume detaches and reattaches in roughly 1–2 minutes, and clients treat the gap as a miss (§4).
- **Identities** (SeaweedFS S3 config, stored in a Secret):

  | Identity | Actions | Where the key lives |
  |---|---|---|
  | `main-writer` | `Read:cache-main`, `Write:cache-main`, `List:cache-main` | GitHub Environment `build-cache-main` secret (C4) |
  | `pr-writer` | `Read:cache-main`, `List:cache-main`, `Read:cache-pr`, `Write:cache-pr`, `List:cache-pr` | repo Actions secret, plus a k8s Secret in `gardenio` for agent pods |
  | `admin` | all | k8s Secret only; used for bucket lifecycle and ops |

- **Eviction:** S3 lifecycle expiry of **14 days** on `cache-pr` and **30 days** on `cache-main`. We also alert when volume usage passes 80%. Lifecycle on SeaweedFS is supported by upstream, but **our retention numbers are a starting guess**; Phase 5 re-derives them from the measured growth rate.

### 2.3 Go (CI and agents)
- `GOCACHEPROG` points at an S3-capable client whose local directory is L1: under `/cache` on CI, per-pod on agents.
- **Read fall-through (L1, then `cache-pr`, then `cache-main`) is UNCONFIRMED** for every off-the-shelf client; each one found takes one bucket. The Phase 0 spike decides between:
  - **(a)** an off-the-shelf client, if one supports two remotes;
  - **(b)** a small in-repo client, `gardenio/cmd/gocacheprog`, built on the `bradfitz/go-tool-cache` library (BSD-3). It composes a disk cacher with a read-write S3 cacher and a read-only S3 cacher. The GOCACHEPROG protocol is a small JSON stream over stdin and stdout, so this is a few hundred lines with tests;
  - **(c)** as a floor: PR builds and agents read `cache-main` only and write nothing remote.
- **Whether concurrent client processes can safely share one L1 directory** (two heavy runners per node) is **UNCONFIRMED** and is part of the spike. If it is not safe, L1 becomes per-pod on CI too.
- The client binary follows house style: a pinned release with a `_SHA256` in `gardenio/locks/security-tools.lock` or, if option (b) below is chosen, built from this repo into the runner and agent images. It is **not** `go install`ed at job time.
- `GOTOOLS_BIN` and `GOLANGCI_LINT_CACHE` stay on hostPath and are unchanged by this design.

### 2.4 Rust (coop)
- `RUSTC_WRAPPER=sccache` with its S3 backend: `SCCACHE_BUCKET`, `SCCACHE_ENDPOINT`, `SCCACHE_S3_USE_SSL=false` and `SCCACHE_REGION=us-east-1` pointed at the service. `SCCACHE_S3_RW_MODE=READ_ONLY` is set for any identity that shouldn't write.
- **Reading both buckets** would use `SCCACHE_MULTILEVEL_CHAIN`. **Whether that chain can point at two S3 buckets is UNCONFIRMED**; the spike decides. If it can't, PR builds read and write `cache-pr` only, and `cache-main` is seeded by main pushes.
- `CARGO_INCREMENTAL=0`, and `CARGO_TARGET_DIR` becomes a per-pod path. This resolves gd-kuQP7w43HC permanently (that bead has its own interim one-line fix and does not wait for this plan).
- The `prepare-build-cache` `rm -rf /cache/cargo-target` branch is deleted once nothing uses that directory.

### 2.5 Docker layers (images.yml)
- Replace `type=registry,ref=ghcr.io/.../cache/<image>` with `type=s3,endpoint_url=http://s3.build-cache.svc.cluster.local:8333,bucket=cache-main|cache-pr,region=us-east-1,use_path_style=true,name=<image>,mode=max`, plus `ignore-error=true` on the export.
- **UNCONFIRMED:**
  - BuildKit's `type=s3` against SeaweedFS (the docs name only MinIO);
  - resolving the cluster Service name from inside the `docker-container` buildkit container running in dind.

  The Phase 0 spike checks both. If either fails, this consumer stays on ghcr and is dropped from scope without blocking the rest.
- `controller-base` stays uncached (the `50a8aeec` reasoning still holds).

### 2.6 Crew agents
- The agent image (`images/agent`) gains the GOCACHEPROG client and sccache.
- The controller's podmanager gains the ability to inject the pr-writer credentials from a k8s Secret by `secretKeyRef`. Project `EnvVars` in beads are plain text and must never carry the key. The non-secret settings (`GOCACHEPROG`, endpoint, bucket) can come from project `EnvVars`.
- Agents never receive `main-writer`.
- This closes gd-7jXVTFHM5B: its acceptance, "second run reaches the first test line in under 5 minutes", holds even on a fresh pod, which is stronger than the PVC it proposed.

### 2.7 Go modules (gated, C8)
- Only if Phase 0 shows module download is at least 10% of a cold-node Go job: add Athens with SeaweedFS as its S3 storage, and set `GOPROXY=http://athens.build-cache.svc.cluster.local:3000,https://proxy.golang.org,direct`.
- Otherwise, record the measurement and close the task as not needed.

## 3. Cost model (to be measured in Phase 0, not assumed)

| Item | Estimate | Basis |
|---|---|---|
| gp3 200 GiB | ~$16/mo | $0.08/GB-month, default 3000 IOPS / 125 MB/s |
| SeaweedFS pod | ~0 marginal | fits on the existing on-demand platform node; **resource needs UNCONFIRMED** |
| Cross-AZ reads | **unknown; this is the risk** | ~half of CI nodes sit in the other AZ. In-region cross-AZ traffic is billed on both sides, ~$0.02/GB round trip. At 2 GB of remote reads per job and 300 jobs/day that is ~$180/mo, which would erase the win. L1 (C6) is the main lever. Phase 0 measures **bytes read remote per job type** before anything is built. |
| EBS CSI controller | ~0 | two small pods |

**Kill criterion:** if the measured cross-AZ bytes project to more than the measured node-hour savings, we stop and consider one of:
- pinning the CI pool to one AZ, which cuts spot diversity (trades against gd-bPVTlMeWV6 item 3);
- a cache pod per AZ, each with its own volume, which doubles storage and needs no replication (each AZ warms on its own).

Neither is part of this design until the numbers ask for it.

## 4. Failure behaviour

- **Cache service down, moving, or full: builds must not fail, only slow down.**
  - Every client runs in fail-open mode. sccache falls back to local compile on storage errors. The GOCACHEPROG client treats remote errors as misses and drops failed PUTs. buildx uses `ignore-error=true`.
  - The Phase 0 spike checks each one by pointing the client at a dead endpoint (a **positive control**, per the "CI guards that cannot fail" rule: a build that "passes" with the cache down proves nothing unless the same setup, pointed at the live cache, shows hits).
- **A poisoned `cache-pr` entry** affects only PR and agent builds, and expiry clears it within 14 days. The admin identity can also empty the bucket.
- **A poisoned `cache-main` entry** requires either the main-writer key (C4) or a node compromise, which is already possible via dind (C6).
- **Volume lost** (AZ event, accidental delete): `Retain` protects against PVC deletion. Losing the volume costs one pool-wide cold start, not an outage. There are no backups, deliberately; it is a cache.
- **The Go patch bump cliff** (`2e8f6768`) now happens once for the whole fleet instead of once per node. That is an improvement, and the design keeps the pin.

## 5. Measurement and success criteria

Baselines come from `gh api .../jobs` step durations, taken as the distribution over 12 or more runs, split by `node=` warm and cold per the README method:

1. **A cold node matches today's warm node:**
   - controller build+vet+test ≤ 200 s on a node that has never run it (today's warm best is 194 s, cold median 472 s);
   - kbeads ≤ 60 s;
   - coop build+test ≤ 190 s.
2. **Crew agents:** the second gascity run on a *fresh pod* reaches its first test line in under 5 minutes (from 20–40 minutes).
3. **The operator's thesis:** CI node-hours per merged PR, measured over one week before and one week after, fall by more than the service's measured monthly cost (EBS plus cross-AZ).
4. **Fail-open:** one run of each consumer with the service scaled to 0 still passes.

## 6. Phases (the plan expands these)

0. **Spikes and measurement (throwaway).** SeaweedFS on a `local-path` PVC in a scratch namespace (no EBS needed yet). Resolve every UNCONFIRMED item, measure remote bytes per job and the module-download share, and write a findings doc. This phase gates the rest.
1. **Storage.** Operator decision O1 (IAM user and keys). Terraform for the IAM user and policy (`-target`ed, from a tree containing `5412373e`), the EBS CSI chart, and the `gp3-retain` StorageClass.
2. **Service.** SeaweedFS values, buckets, identities, lifecycle, Service. Operator decision O2 (the Environment secret and repo secret).
3. **CI Go**, then **4. CI Rust**, then **5. buildx.** Each lands behind measurement against §5, and each can be reverted on its own.
6. **Crew agents.**
7. **Athens,** only if gated in.
8. **Retire what the numbers say to retire.** Update `helm/ci/README.md`, and add a correction banner to `gardenio/docs/ci-go-cache-prototype.md` and its doc bead gd-A0f2QqgYpM, which still carry the refuted "~0% node-local" headline.

## 7. Operator decisions this plan will raise

- **O1:** create an IAM user with long-lived keys for the EBS CSI controller (C5). Rotation: manual, recorded next to the 2026-10-19 credential cliff.
- **O2:** create the GitHub Environment `build-cache-main` with a `main`-only deployment rule and its secret (C4), plus the repo-level pr-writer secret.

## 8. Non-goals

- Changing runner tiers, requests, or pool shape; that is gd-bPVTlMeWV6's work, and this design only has to survive it.
- Replacing the ECR pull-through cache for base images.
- Closing the IMDS or node-role exposure (gd-adirNxx3Qo, gd-9ip3Fsen5c). This design only avoids widening it.
- Bazel or a remote-execution setup.
- A cache for agents' `node_modules` or other ecosystems.
