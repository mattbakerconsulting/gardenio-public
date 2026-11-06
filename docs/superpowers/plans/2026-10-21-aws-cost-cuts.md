# AWS cost cuts: image pushes to ECR, beads read traffic, node public IPv4 — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Cut three AWS line items the owner approved on 2026-10-21. (A) Internet egress: CI stops pushing images to ghcr and pushes to ECR in the same region, which is free. (B) Cross-AZ traffic from Postgres and the beads daemon: stop the waste found by measurement. (C) Public IPv4: remove it from every k3s node except the public edge and the CI pool. Each phase ends with a measured before/after.

**Owner direction (chat, 2026-10-21):** the account is self-funded. October cost to date is $446, Cost Explorer forecasts about $1,100, and the ceiling is $200/month. The owner approved all three workstreams. Already done the same day: `ci_max_nodes` 13 -> 6 (commit 442df2aa1), and releases batched to at most about one a day.

**Architecture:**
- **A.** `images.yml` gets AWS credentials from GitHub OIDC (no IMDS, no static key) and pushes images and the BuildKit cache to ECR repositories `gardenio/<image>` and `gardenio/cache/<image>`. Before that, Task A1 stops the PR-time push of `controller-base` when no image in the PR uses it. That one push is most of today's egress. After one release is pushed to both registries, the cluster pulls from ECR directly. Then the ghcr push is removed.
- **B.** The measured top reader is beads-viewer's garden hub and farm (`internal/gardenhub`). It re-reads every open work bead on every bead event in each of its two replicas, and none of those events are work beads. Filter events by bead type. Pin the hourly `pg_dumpall` to the Postgres node. Add per-caller byte metrics to kbeads. Prefer same-zone placement for the daemon and its heavy clients.
- **C.** Add two small edge subnets (IGW route) for the NAT instance, the public-edge node and the CI pool. Turn on the network module's existing NAT instance. Move the spot agent pool into the private-app subnets. Then flip the old public subnets' route tables to the NAT one AZ at a time, and strip the public IPs in place, pool by pool (on-demand agents, then servers last). kubectl over SSM gets a relay through the edge instance so it never depends on the NAT.

**Tech stack:** GitHub Actions (`docker/build-push-action@v6`, `aws-actions/configure-aws-credentials@v4`, `aws-actions/amazon-ecr-login@v2`), Terraform 1.16 with AWS provider ~> 5.0 (`gardenio/terraform/composition/gardenio`, modules `k3s` and `network`), Helm chart `gardenio/helm/gardenio` and `deploy-values/gardenio-k3s.yaml`, raw manifest `gardenio/helm/gardenio-ziti/public-edge.yaml`, Go 1.25 (`gardenio/controller/internal/gardenhub`, `kbeads/internal/server`), Python 3 stdlib guards under `scripts/`, bash.

**Spec:** none separate. The owner-approved brief (2026-10-21) and the measurements in "Measured baselines" below are the spec. Every number a task needs is copied into that task.

**Anchors:** verified against `main` at `442df2aa1` (2026-10-21). Lines drift. If a cited line has moved, find it by the quoted text and say so in the PR.

---

## Measured baselines (2026-10-21, profile `gardenio`, region `us-east-1`)

Re-measure with the same commands after each phase. The commands are inside the tasks.

### Cost Explorer

October 1-21 by usage type. These are the three lines this plan targets, plus context:

| Usage type | Usage | Cost |
|---|---|---|
| DataTransfer-Out-Bytes (internet egress) | 726.3 GB | $56.37 |
| USE1-PublicIPv4:InUseAddress | 5,203.6 h | $26.02 |
| DataTransfer-Regional-Bytes (cross-AZ, counted on both sides) | 2,369.2 GB | $23.68 |
| *Context:* EC2 compute (all instance types) | | $341.68 |
| *Context:* EC2-Other (EBS gp3 $39.63) | | $70.70 |
| *Context:* S3 | | $2.20 |

The three targeted lines are $106 of $446 (24%). Compute is 77% and is not in this plan (see "Not in scope").

Daily (10-21 is partial):

| Day | Egress GB / $ | Cross-AZ GB / $ | IPv4 hours / $ |
|---|---|---|---|
| 10-12 | 16.6 / 1.49 | 77.0 / 0.77 | 236.6 / 1.18 |
| 10-13 | 49.4 / 4.45 | 98.1 / 0.98 | 287.2 / 1.44 |
| 10-14 | 52.5 / 4.72 | 119.2 / 1.19 | 351.7 / 1.76 |
| 10-15 | 67.8 / 6.11 | 133.4 / 1.33 | 359.5 / 1.80 |
| 10-16 | 36.0 / 3.24 | 132.2 / 1.32 | 288.1 / 1.44 |
| 10-17 | 36.4 / 3.28 | 193.8 / 1.94 | 264.0 / 1.32 |
| 10-18 | 20.8 / 1.88 | 210.6 / 2.11 | 281.0 / 1.40 |
| 10-19 | 95.4 / 8.59 | 300.2 / 3.00 | 451.4 / 2.26 |
| 10-20 | 142.8 / 12.86 | 425.9 / 4.26 | 516.6 / 2.58 |
| 10-21 (partial) | 23.6 / 2.12 | 104.5 / 1.05 | 86.7 / 0.43 |

### A: what the egress is

- VPC flow logs (20 minutes, taken earlier on 10-21 and then deleted) showed the paid egress is CI nodes pushing to GitHub and ghcr.
- **Most of it is `controller-base`, pushed on every PR.** `images.yml` pushes `cache/controller-base:<sha>` with `push: true` on every triggering event, PRs included (`.github/workflows/images.yml:619-647`). Tagged versions per day, from `gh api /orgs/mattbakerconsulting/packages/container/gardenio%2Fcache%2Fcontroller-base/versions`:

  | Day | 10-13 | 10-14 | 10-15 | 10-16 | 10-17 | 10-18 | 10-19 | 10-20 |
  |---|---|---|---|---|---|---|---|---|
  | controller-base pushes | 9 | 48 | 86 | 39 | 20 | 26 | 222 | 356 |
  | Egress GB | 49.4 | 52.5 | 67.8 | 36.0 | 36.4 | 20.8 | 95.4 | 142.8 |

  A linear fit of egress on pushes over these 8 days gives **0.308 GB per push**, intercept 31.6 GB/day, **r = 0.963**. The pushed image is 316 MB compressed (two manifests, one platform plus an attestation). Its compile layer is unique per commit by construction (`images.yml:648-660`), so every push uploads it again.
- On most PRs nothing pulls the pushed base. The PR leaf skip (`images.yml:430-530`) drops every `controllerBase` image whose own `gardenio/images/<name>/` directory is untouched.
- Release tags: 5-12 calver tags a day over 10-12..10-21. This is before the 1-a-day batching. A tag build pushes all 11 enabled images plus `cache-to ... mode=max` for each. The brief puts that at about 25-35 GB per release.
- Compressed sizes at 2026.294.10: agent 2,733 MB; controller-base 316 MB; coop-claude 303 MB; coopmux 187 MB; ziti-ctrl-backup 145 MB; kbeads 63 MB; beads-viewer 56 MB; controller 26 MB; slack-bridge 23 MB; grafana-bridge 22 MB; trigger-engine 20 MB; k3s-audit-shipper 4 MB.
- CloudWatch NetworkIn/Out by ASG on 10-20:

  | ASG | In | Out |
  |---|---|---|
  | ci | 3,098.6 GB | 146.8 GB |
  | agents | 535.5 GB | 569.9 GB |
  | agents-spot | 286.7 GB | 103.3 GB |
  | servers | 23.5 GB | 94.1 GB |

  These totals include in-VPC traffic.
- Average in-service CI nodes: 8.2 on 10-19 and 10.1 on 10-20. That was before the cap of 6.

### B: what the cross-AZ traffic is

- **The hourly backup is the largest single item.**
  - `gardenio-postgres-s3-backup` runs `pg_dumpall | gzip` at `17 * * * *`. It runs in a pod on any on-demand platform node (nodeSelector only, no affinity), and pulls the dump over the network from `gardenio-postgres-0`.
  - Postgres is on `ip-10-90-30-213`, us-east-1b, pinned by a local-path PV. The other on-demand nodes are `ip-10-90-17-224` (1b) and `ip-10-90-2-170` (1a). Two of the four runs on 10-21 landed on the 1a node (18:17Z and 21:17Z).
  - Hourly NetworkIn of the node that ran the backup: 19.2-21.0 GB. In hours without a backup the same node took 1.7-4.2 GB, so one run moves about 17-19 GB.
  - The Postgres node's NetworkOut is a flat 23.9-26.1 GB/h.
  - The S3 objects are 2.63-2.75 GB gzipped, growing about 25 MB per hour.
  - `s3://gardenio-hq-backups-123456789012/k3s-postgres/` holds 183.4 GB in 275 objects, with 30-day expiry.
- **Database:**
  - `beads` is 5,277 MB. `events` is 2,490 MB and `mutation_history` is 2,477 MB. Both are mostly TOAST, and neither has been pruned since 2026-09-18.
  - `litellm` is 678 MB and `keycloak` 14 MB.
- **Postgres clients** (`pg_stat_activity`):
  - 13 connections are from `gardenio-beads` (10.42.21.16).
  - 7 are LiteLLM (2 pods), 2 are Keycloak, and 1-2 are the backup pod.
  - In 30 samples 2 s apart, every active non-backup query came from the beads daemon.
- **`pg_stat_user_tables`** cumulative since the postmaster started (2026-10-15 22:27Z, 5.95 days), and the rate over 2 minutes at 21:38-21:40Z:

  | Table | seq_scan | seq_tup_read | idx_scan | idx_tup_fetch | Rate (2 min) |
  |---|---|---|---|---|---|
  | beads | 324,432 | 6.67B | 307.1M | 5.64B | idx_tup_fetch 9,213/s, idx_scan 1,570/s |
  | deps | 730,993 | 11.86B | 231.8M | 321.9M | seq_scan 0.6/s (16,229 rows/s), idx_scan 1,216/s |
  | operators | 346.7M | 12.05B | 61.4M | 61.4M | seq_scan 1,652/s (one per bead row: the planner seq-scans the 91-row table inside a nested loop for `LEFT JOIN operators`) |
  | labels | 542 | 16.9M | 368.4M | 1.07B | idx_scan 1,226/s |

  `pg_stat_statements` is not installed.
- **Bytes each client receives:** `process_network_receive_bytes_total` from the in-cluster VictoriaMetrics (`svc/gardenio-vmsingle:8428`), last 24 h, summed by workload:
  - `gardenio-beads-viewer`: 102.4 GB
  - `gardenio-trigger-engine`: 58.6 GB
  - `gardenio-agents` (the controller): 35.4 GB
  - `gardenio-slack-bridge`: 2.6 GB
  - The viewer sends 1.0 GB a day to its own clients. Its receive rate never fell below about 1 MB/s at night, so a background loop is reading, not the browser.
- **kbeads request rates** (`gardenio_beads_requests_by_auth_type_total`, 60 s delta):
  - machine read: 27.8 req/s
  - fleet read: 0.83 req/s
  - oidc read: 0.05 req/s
  - other read: 0.13 req/s
- **Slow-request log:** 181 of the 208 requests over 1 s in 40 minutes were the owner's OIDC reads through the viewer. They came in bursts (page loads).
- **The garden farm read** is `Farm.readRows`: open, in_progress, blocked, deferred and partial beads of the types task, feature, bug, chore, decision, escalation, epic and molecule, with ExcludeScheduled and WithParent. Live, that is **876 rows and 3.44 MB of JSON per refresh**.
  - The query shape `status IN (5) AND type IN (8) ... schedule_id ... multi_fire_key` was among the most frequent active queries in sampling.
  - `Hub.watch` (`gardenhub/hub.go:262-291`) marks the hub dirty and calls `Farm.Invalidate` for every `beads.bead.>` event.
  - Mutation log lines in 10 minutes (21:23-21:33Z): 284. By type they were agent 114, trigger 78, message 43, session 28, account 13, audit 6, event 2, and **farm work types 0**.
  - NATS published 229 MutationUpdate events in those 10 minutes, averaging 31.9 KB (7.3 MB).
- **Placement now:** daemon `gardenio-beads` on 17-224 (1b); viewers on 31-46 and 30-213 (1b); trigger-engine on 30-213 (1b); controller `gardenio-agents` on 2-170 (1a); coopmux on 2-170 (1a). The daemon is rescheduled on most rolls; there were 9 ReplicaSets in 32 hours.
- **Nodes carry no `topology.kubernetes.io/zone` label** (checked on ip-10-90-30-213). The AWS CCM runs node-lifecycle only, so zone-keyed affinity does nothing today.

### C: public IPv4

- At 21:20Z there were 17 running instances, each with a public IPv4: 3 servers, 3 on-demand agents, 3 spot agents, 6 CI, and the edge (EIP 32.199.110.32). The HQ host was stopped.
- The 10-20 average was 21.5 addresses (516.6 h).
- At $0.005/h each, one address is $0.12/day.
- **Network as it is:**
  - VPC `vpc-0123456789abcdef0` 10.90.0.0/16. Public subnets: 10.90.0.0/20 (1a, `subnet-0123456789abcdef0`), 10.90.16.0/20 (1b, `subnet-0123456789abcdef1`) and 10.90.96.0/20 (1c, `subnet-0123456789abcdef2`, servers only).
  - Unused private-app subnets: 10.90.32.0/20 (1a, `subnet-0123456789abcdef3`, `rtb-0cfd8a153ef58230e`) and 10.90.48.0/20 (1b, `subnet-0123456789abcdef4`, `rtb-0405867b24d95fabe`).
  - Public route table: `rtb-0b4cd4409e0cc60ee`, which also carries three empty leftover `uh2-spot-public` subnets at 10.90.112.0/20, 10.90.128.0/20 and 10.90.144.0/20 (1d/1e/1f).
  - The S3 and DynamoDB gateway endpoints are already attached to the public, private-app and private-data route tables (`modules/network/main.tf:245-260`).
  - No interface endpoints and no NAT exist. `nat_mode = "none"` (`composition/gardenio/main.tf:22`).
- **IPv6 reachability (AAAA records, 2026-10-21):**
  - No AAAA: github.com, api.github.com, codeload.github.com, objects.githubusercontent.com, ghcr.io, slack.com, wss-primary.slack.com, the account's ECR registry host, api.ecr.us-east-1.amazonaws.com, ssm/ssmmessages/ec2messages.us-east-1.amazonaws.com, sts, autoscaling and route53.
  - Have AAAA: api.anthropic.com, pkg-containers.githubusercontent.com, proxy.golang.org, registry-1.docker.io, registry.k8s.io, get.k3s.io, awscli.amazonaws.com, archive.ubuntu.com, and the dual-stack `*.api.aws` names for ECR, SSM and EC2.

---

## Decisions (with the numbers that drove them)

**A1 first.** Most of the egress is the PR-time `controller-base` push, and on most PRs nothing uses it. That is a code-only fix with no infra. At the 10-19/10-20 rate (222-356 pushes a day) it removes 68-110 GB/day, which is $6.1-9.9/day.

**Move to ECR, and drop ghcr after one dual-pushed release.**
- A push from EC2 to ECR in the same region is free.
- Keeping ghcr as a mirror would keep paying for the push we are removing: about $0.09/GB, roughly $0.3 per release for images and more for the cache.
- ghcr stays reachable as the rollback. The ECR pull-through rule `ghcr` and the `ghcr` pull secret are left in place.
- After Task A5 a rollback also needs ghcr pushes turned back on and one release cut (written into A5).

**The build cache lives in ECR.**
- Leaf images use `cache/<image>:buildcache` with `image-manifest=true,oci-mediatypes=true`, which ECR requires for registry caches.
- `controller-base` stays cache-less, as it is today (`images.yml:648-681`, measured net loss).

**Runners authenticate with GitHub OIDC.**
- `aws-actions/configure-aws-credentials` gets temporary credentials from STS over the internet, with no IMDS involved.
- `imdsDeny` (`deploy-values/gardenio-k3s.yaml:2257-2260`, enforced for `arc-runners`) and the CI probe "IMDS is unreachable from a runner pod" (`checks.yml:690-691`) stay as they are.
- There are two roles. `gardenio-ci-ecr-pr` (sub `repo:mattbakerconsulting/gardenio:pull_request`) may push only `gardenio/cache/controller-base` and pull `gardenio/cache/*`. `gardenio-ci-ecr-release` (tags `20*` and `main`) may push `gardenio/*`.
- Fork PRs get no OIDC token. The repo takes no fork PRs (`images.yml:619-626`).

**B's culprit is beads-viewer's `gardenhub` Hub and Farm, not polling by an agent or `kd list`.**
- It is the largest receiver by measured bytes (102 GB/day). Its refresh is triggered by events that cannot change what it reads (0 of 284).
- The slack-bridge dashboard looked like the top suspect from reading the code. Measured, it is only 2.6 GB/day, so it is ruled out.
- The hourly backup is the larger dollar item because it crosses AZs about a third of the time.
- **Indexes:** none added. The table counters say the reads are many small, already-indexed lookups driven by callers: `operators` seq scans are one per bead row in a nested loop over a 91-row table, and `labels` and `deps` show per-row probes. An index would not cut bytes. Fewer and narrower reads will.

**C uses a NAT instance, not a NAT gateway, and not IPv6.**
- A NAT gateway costs $0.045/h ($1.08/day, $32.85/month) plus $0.045 per GB processed.
- The IPv4 it would save is about $1.20/day (10 addresses), so a NAT gateway loses money as soon as more than about 3 GB/day goes through it.
- With CI behind it, 3.1 TB/day of CI inbound would cost about $139/day.
- IPv6-only egress fails, because GitHub, ghcr, Slack, the ECR registry, SSM and STS have no AAAA records (measured above).
- The module's NAT instance (`modules/network/nat_instance.tf`) is used: AL2023 arm64, ASG of 1, and it takes over the route at boot.

**The CI pool keeps its public IPs.**
- CI took in 3.1 TB on 10-20. Through a t4g.nano NAT (32 Mbps baseline, about 345 GB/day sustained) that would be impossible.
- Through a NAT sized for it (for example c7gn.medium, about $1.50/day) it would cost more than the roughly $0.60/day of CI IPv4 it saves.
- CI moves to the new edge subnets, which keep the IGW route.

**Order: flip AZ by AZ, strip pool by pool.**
- A VPC route is per subnet. The on-demand agents and the servers share the old public subnets, and the on-demand agents carry node-pinned local-path PVs: postgres, nats and victoria-logs on 30-213; minio, vmsingle and grafana on 2-170; slack-bridge state on 17-224.
- So those nodes are not replaced. Their subnets are re-associated to the NAT route table (instant, reversible), and their public IPs are removed in place with `aws ec2 modify-network-interface-attribute --no-associate-public-ip-address`. This is supported for the primary ENI in the installed CLI 2.36.45.
- The stateless spot pool moves first by replacement into the private-app subnets.
- So the order is: CI moved (public) -> spot agents moved (private) -> subnets flipped 1c, 1a, 1b -> on-demand agents stripped -> servers stripped last.

**kubectl and the edge never depend on the NAT.**
- The edge node moves into an edge subnet with the IGW route and keeps its EIP.
- `kubectl-ssm.sh` gets an `edge` relay: an SSM session on the edge instance (which reaches SSM over its own public address) with `AWS-StartPortForwardingSessionToRemoteHost` to a server's private IP on 6443.
- No SSM interface endpoints: 3 endpoints x 2 AZ x $0.01/h is $1.44/day, more than the whole saving.

---

## Global Constraints

1. **Lanes.**
   - CREW: code and PRs only. No kubectl or helm against the live cluster, no Terraform plan or apply, no AWS mutations.
   - OPERATOR: laptop-claude. Runs every live step marked OPERATOR. Terraform runs only through `./scripts/tf-gardenio.sh` from a `~/ws/gardenio` tree equal to `origin/main` (`git fetch && git merge --ff-only` first), always with `-target`. AWS always uses `--profile gardenio --region us-east-1`. kubectl always uses `export KUBECONFIG=$HOME/ws/scratch/ssm.kubeconfig` (kept live by `scripts/k3s/kubectl-ssm.sh start`).
   - OWNER: only GitHub App webhook redelivery in the GitHub UI, if Task C4 drops a delivery.
   - A crew finishes its PR, then posts one comment on its task listing the OPERATOR steps verbatim with the PR link.
2. **Merge desk.**
   - Any PR touching `deploy-values/**`, `gardenio/helm/**`, `gardenio/terraform/**` or `.github/workflows/**` is a merge-desk PR; say so in the description.
   - PRs touching `deploy-values/**` go up as DRAFT with label `merge-desk:hold`. The OPERATOR merges them in a watched window.
   - Holds go on the PR as a "Verdict: send back" comment, not only in mail.
   - Before merging: check `baseRefName == main`, read the head SHA, and re-read the desk's latest "Verdict:" comment.
3. **Releases.** Releases are batched to at most about one a day (owner, 2026-10-21). A task that needs a release uses the next one, or the OPERATOR cuts one with the laptop's `gardenio/scripts/release-cut.sh`, never from an agent pod. Every helm roll un-suspends the release-deployer; plan for it.
4. **Secrets.** Never print a Secret value, a token, a password or a key, in logs, test output, PR text or comments. No `kubectl get secret -o yaml`. No go-templates or jsonpath across Secrets (use `-o json` and print names only). No `helm get manifest` without dropping `kind: Secret` first. Create Secrets with `kubectl create`, never `apply`.
5. **Checks that cannot fail are worthless.** Every guard gets a negative control (a fixture it must reject) and a positive control (the real tree, which must pass, after it was shown to fail before the change). Gate each commit on the test command's exit code: `<test> && git commit ...`, never on piped output. Go: `gofumpt -l`, `go vet ./...` and `staticcheck ./...` clean on touched packages. Bash: `set -euo pipefail` and `shellcheck -x`. Python: stdlib only (the runners have no PyYAML).
6. **No new recurring cost** beyond what a task states. No NAT gateway, no interface endpoints, no flow logs left running, no new EIPs.
7. **Known hazards.**
   - Never cordon or drain a node with node-pinned PVs (30-213, 2-170, 17-224) before a helm upgrade.
   - Never stop or terminate an ASG member of `servers` or `agents` without first suspending `HealthCheck` and `ReplaceUnhealthy`: the ASG replaces it and its local PVs are lost.
   - Never `kubectl set image`.
   - Never hand-set `desired_capacity` on `gardenio-k3s-agents-spot`.
   - The agent and CI launch templates are within about 800 bytes of the 16 KB EC2 user_data limit while they are plain base64 (Task C3 gzips them).
8. **Rollback first.** Every OPERATOR step writes down its rollback command before running the change. The report says what was changed, with ids.
9. **Branches.** `agent/aws-cost-cuts/<task-id>` against `main`.

## Review Focus

1. **A release where one of the 11 images is missing from ECR when the registry flips.** Expected: no pod is ever pointed at a tag that ECR lacks. Task A4 checks all 11 `gardenio/<image>:<calver>` before merging, and its rollback is one revert.
2. **A PR job that gets no OIDC token,** for example a missing `id-token: write` or a role trust that does not match the `sub`. Expected: the images check fails loudly at "AWS credentials". The guard in Task A3 asserts `id-token: write` and the credentials step in every job that pushes or pulls ECR.
3. **An event payload gardenhub cannot read,** for example an unknown topic shape or a missing bead. Expected: it refreshes as it does today (fail open), and the garden never freezes. Task B2 has a test for it.
4. **A node that boots with no public IP.** Expected: k3s starts without a bare `--node-external-ip`, and `k3s.clonk.tv` holds private IPs, never stale public ones that AWS may hand to someone else. Task C3 tests the rendered user_data, and Task C9 is the first live boot.
5. **The NAT box dies or is replaced.** Expected: nodes behind it lose egress for about 1-3 minutes and recover with no action; kubectl via the edge relay and the public edge keep working throughout. Task C6 terminates the box once on purpose and times the gap.

---

## Task map and dependencies

| Task | Phase | Who | Depends on |
|---|---|---|---|
| A1 Push controller-base on a PR only when a leaf uses it | A | CREW | none |
| A2 ECR repos, lifecycle, GitHub OIDC roles, node pull grant | A | CREW + OPERATOR apply | none |
| A3 images.yml pushes to ECR (ghcr mirror on tags for one release) | A | CREW | A1, A2 applied |
| A4 Cluster pulls from ECR directly | A | CREW + OPERATOR window | A3 merged plus one release |
| A5 Drop the ghcr push | A | CREW + OPERATOR verify | A4 live for 48 h |
| B1 Backup pod runs on the Postgres node | B | CREW + OPERATOR verify | none |
| B2 gardenhub refreshes only for events that can change it | B | CREW + OPERATOR verify | none |
| B3 kbeads per-caller response bytes | B | CREW | none |
| B4 Zone labels and same-zone preference for daemon and clients | B | CREW + OPERATOR labels and window | none |
| B5 Measure after, report the residual top callers | B | OPERATOR | B1-B4 live for 24 h |
| C1 Measure non-CI internet bytes, pick NAT size and topology | C | OPERATOR | none |
| C2 Network module: edge subnets, NAT placement, route flip toggle | C | CREW | C1 |
| C3 k3s module: pool subnets, public-IP toggles, no-public-IP boot | C | CREW | none |
| C4 hooks.clonk.tv moves to the public edge | C | CREW + OPERATOR cutover | none |
| C5 kubectl-ssm.sh edge relay | C | CREW | none |
| C6 Apply edge subnets and NAT; canary test; NAT failover test | C | OPERATOR | C2 |
| C7 CI pool to the edge subnets | C | OPERATOR | C3, C6 |
| C8 Edge node to the edge subnet | C | OPERATOR | C3, C4 done, C5, C6 |
| C9 Spot agents to private-app behind the NAT | C | OPERATOR | C7, C8 |
| C10 Flip old public subnets to the NAT, 1c then 1a then 1b | C | OPERATOR | C9 |
| C11 Strip public IPs: on-demand agents, then servers | C | OPERATOR | C10 |
| C12 Measure after | C | OPERATOR | C11 plus 48 h |

Do A1, B1 and B2 first: they are the largest and cheapest. Phases are otherwise independent.

---

## Phase A: release and CI images go to ECR

### Task A1: Push `controller-base` on a pull request only when a leaf image in that PR's matrix pulls it

**Who:** CREW (one merge-desk PR). OPERATOR verifies after merge (read-only).

**Why:** `images.yml:619-647` pushes `cache/controller-base:<sha>` with `push: true` on every event, so the leaves can `COPY --from` it. But the PR leaf skip (`images.yml:430-530`) drops every `controllerBase` leaf whose `gardenio/images/<name>/` directory is untouched. So on most PRs the 316 MB base is uploaded and nothing reads it. This is measured at 0.308 GB of internet egress per push, r = 0.963 over 8 days.

**Savings:** 0.308 GB x (PR base pushes per day with no consumer) x $0.09/GB. At 10-20 (356 pushes) that is about $9.6/day; at 10-14..10-18 rates (20-86) $0.6-2.4/day.

**Files:**
- Modify: `.github/workflows/images.yml`
  - the `changes` job `outputs:` block, after `needs_base:` (`:94`)
  - the `select images` step, final output block (`:532-536`, the lines `echo "matrix=$sel" >> "$GITHUB_OUTPUT"` .. `echo "controller-base needed: $needs_base"`)
  - the `controller-base` job's step `build + push controller-base`, line `push: true` (`:638`)
- Create: `scripts/check-image-registry.py`
- Create: `scripts/check-image-registry_test.py`
- Modify: `.github/workflows/checks.yml`
  - after the step `images.yml path filters exclude Go test sources` (`:494`)
  - after `python3 scripts/check-images-filters_test.py` (`:604`)

**Interfaces:**
- Produces: workflow output `needs.changes.outputs.base_consumers` (`"true"` or `"false"`).
- Produces: the guard `scripts/check-image-registry.py` (exit 0 pass, 1 fail; `GARDENIO_ROOT` overrides the repo root) with function `check_base_push(text: str) -> list[str]`. Tasks A3 and A5 add checks to this file.

- [ ] **Step 1: Write the failing guard test**

Create `scripts/check-image-registry_test.py`:

```python
#!/usr/bin/env python3
"""check-image-registry_test.py -- unit tests for check-image-registry.py.

Each case writes a synthetic .github/workflows/images.yml into a tmpdir and
runs the guard against it with GARDENIO_ROOT. The bad cases are the drift the
guard exists for: a controller-base push that fires on every PR again.

Usage:  python3 scripts/check-image-registry_test.py
"""
import os
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
GUARD = os.path.join(HERE, "check-image-registry.py")
PASS = FAIL = 0

GOOD = """name: images
jobs:
  changes:
    runs-on: x
    outputs:
      needs_base: ${{ steps.select.outputs.needs_base }}
      base_consumers: ${{ steps.select.outputs.base_consumers }}
    steps:
      - id: select
        run: |
          base_consumers=$(echo "$sel" | jq 'any(.[]; .controllerBase == true)')
          echo "base_consumers=$base_consumers" >> "$GITHUB_OUTPUT"
  controller-base:
    runs-on: x
    steps:
      - name: build + push controller-base
        uses: docker/build-push-action@v6
        with:
          push: ${{ github.event_name != 'pull_request' || needs.changes.outputs.base_consumers == 'true' }}
  build:
    runs-on: x
    steps:
      - run: true
"""


def run(text):
    with tempfile.TemporaryDirectory() as d:
        os.makedirs(os.path.join(d, ".github", "workflows"))
        with open(os.path.join(d, ".github", "workflows", "images.yml"), "w") as f:
            f.write(text)
        env = dict(os.environ, GARDENIO_ROOT=d, CHECK_IMAGE_REGISTRY_ONLY="base_push")
        p = subprocess.run([sys.executable, GUARD], env=env, capture_output=True, text=True)
        return p.returncode, p.stdout + p.stderr


def case(name, text, want_rc, want_in=""):
    global PASS, FAIL
    rc, out = run(text)
    ok = rc == want_rc and want_in in out
    print(("PASS" if ok else "FAIL") + f": {name} (rc={rc})")
    if not ok:
        print(out)
    PASS, FAIL = PASS + ok, FAIL + (not ok)


case("good workflow passes", GOOD, 0)
case("push: true on every event is rejected",
     GOOD.replace("push: ${{ github.event_name != 'pull_request' || needs.changes.outputs.base_consumers == 'true' }}", "push: true"),
     1, "controller-base push:")
case("missing job output is rejected",
     GOOD.replace("      base_consumers: ${{ steps.select.outputs.base_consumers }}\n", ""),
     1, "outputs.base_consumers")
case("output never written is rejected",
     GOOD.replace('          echo "base_consumers=$base_consumers" >> "$GITHUB_OUTPUT"\n', ""),
     1, "does not write base_consumers")
case("missing controller-base step is rejected",
     GOOD.replace("build + push controller-base", "build controller-base"),
     1, "no 'build + push controller-base' step")
print(f"{PASS} passed, {FAIL} failed")
sys.exit(1 if FAIL else 0)
```

- [ ] **Step 2: Run it and see it fail**

Run: `python3 scripts/check-image-registry_test.py`
Expected: every case FAILs, because the guard file does not exist (`can't open file`), and the script exits 1.

- [ ] **Step 3: Write the guard**

Create `scripts/check-image-registry.py`:

```python
#!/usr/bin/env python3
"""check-image-registry.py -- guard where .github/workflows/images.yml pushes.

WHY: internet egress from the CI nodes is paid ($0.09/GB). Measured
2026-10-13..20: 0.308 GB of egress per controller-base push, r=0.963, and on
most PRs no leaf image pulls the pushed base (the PR leaf skip drops them).
This guard keeps the push gated on a consumer. Plan:
docs/superpowers/plans/2026-10-21-aws-cost-cuts.md.

Checks (each a function returning a list of error strings):
  base_push  controller-base pushes on a PR only when the post-skip matrix
             still holds a controllerBase image (outputs.base_consumers).

Text matching on purpose: the runners have no PyYAML.
Usage:  python3 scripts/check-image-registry.py
        GARDENIO_ROOT=/path/to/tree python3 scripts/check-image-registry.py
        CHECK_IMAGE_REGISTRY_ONLY=base_push (run one check; used by the tests)
"""
import os
import re
import sys

ROOT = os.environ.get(
    "GARDENIO_ROOT", os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
)
WF = os.path.join(ROOT, ".github", "workflows", "images.yml")
BASE_PUSH = (
    "${{ github.event_name != 'pull_request' || "
    "needs.changes.outputs.base_consumers == 'true' }}"
)


def job_block(text, job):
    """Body of one job under `jobs:` (2-space job keys)."""
    m = re.search(
        rf"^  {re.escape(job)}:\n(.*?)(?=^  [A-Za-z0-9_-]+:\n|\Z)", text, re.S | re.M
    )
    return m.group(1) if m else ""


def step_block(job_text, name):
    m = re.search(
        rf"^      - name: {re.escape(name)}\n(.*?)(?=^      - |\Z)", job_text, re.S | re.M
    )
    return m.group(1) if m else None


def check_base_push(text):
    errs = []
    changes = job_block(text, "changes")
    if not re.search(
        r"^\s+base_consumers: \$\{\{ steps\.select\.outputs\.base_consumers \}\}\s*$",
        changes,
        re.M,
    ):
        errs.append("changes job does not export outputs.base_consumers")
    if 'echo "base_consumers=$base_consumers" >> "$GITHUB_OUTPUT"' not in changes:
        errs.append("select step does not write base_consumers to $GITHUB_OUTPUT")
    step = step_block(job_block(text, "controller-base"), "build + push controller-base")
    if step is None:
        errs.append("controller-base job has no 'build + push controller-base' step")
    else:
        pm = re.search(r"^\s+push: (.+?)\s*$", step, re.M)
        if not pm or pm.group(1) != BASE_PUSH:
            errs.append(f"controller-base push: must be exactly {BASE_PUSH}")
    return errs


CHECKS = {"base_push": check_base_push}


def main():
    with open(WF) as f:
        text = f.read()
    only = os.environ.get("CHECK_IMAGE_REGISTRY_ONLY")
    errs = []
    for name, fn in CHECKS.items():
        if only and name != only:
            continue
        errs += [f"{name}: {e}" for e in fn(text)]
    for e in errs:
        print(f"check-image-registry: FAIL {e}")
    if not errs:
        print("check-image-registry: ok")
    return 1 if errs else 0


if __name__ == "__main__":
    sys.exit(main())
```

- [ ] **Step 4: Run the tests, then run the guard on the real tree as the positive control**

Run: `python3 scripts/check-image-registry_test.py`
Expected: `5 passed, 0 failed`, exit 0.

Run: `python3 scripts/check-image-registry.py`
Expected: exit 1 with `base_push: changes job does not export outputs.base_consumers`. The real tree has not been changed yet, so this proves the guard reads the live file.

- [ ] **Step 5: Change `images.yml`**

In the `changes` job `outputs:`, directly after the `needs_base:` line (`:94`), add:

```yaml
      # Whether any image LEFT in the matrix after the PR leaf skip pulls
      # controller-base. needs_base (above) decides whether the base is BUILT
      # (it must compile on any PR that touches the controller tree);
      # base_consumers decides whether it is PUSHED on a pull_request. A push
      # nothing pulls costs 0.308 GB of paid egress (measured 2026-10-20, plan
      # 2026-10-21-aws-cost-cuts Task A1). Guarded by
      # scripts/check-image-registry.py.
      base_consumers: ${{ steps.select.outputs.base_consumers }}
```

In the `select images` step, replace the final block:

```bash
          echo "matrix=$sel" >> "$GITHUB_OUTPUT"
          echo "count=$(echo "$sel" | jq 'length')" >> "$GITHUB_OUTPUT"
          echo "needs_base=$needs_base" >> "$GITHUB_OUTPUT"
          echo "selected: $(echo "$sel" | jq -r '[.[].image]|join(" ")')"
          echo "controller-base needed: $needs_base"
```

with:

```bash
          # Computed AFTER the PR leaf skip, unlike needs_base: the question
          # is whether any job in THIS run will pull the pushed base.
          base_consumers=$(echo "$sel" | jq 'any(.[]; .controllerBase == true)')
          echo "matrix=$sel" >> "$GITHUB_OUTPUT"
          echo "count=$(echo "$sel" | jq 'length')" >> "$GITHUB_OUTPUT"
          echo "needs_base=$needs_base" >> "$GITHUB_OUTPUT"
          echo "base_consumers=$base_consumers" >> "$GITHUB_OUTPUT"
          echo "selected: $(echo "$sel" | jq -r '[.[].image]|join(" ")')"
          echo "controller-base needed: $needs_base (pushed on a PR only if consumed: $base_consumers)"
```

In the `controller-base` job's `build + push controller-base` step, replace `push: true` with:

```yaml
          # Built whenever needs_base; PUSHED on a pull_request only when a
          # leaf in this run pulls it (base_consumers). Tags and dispatches
          # always push. A build with push false stays in the builder and
          # still proves all binaries compile.
          push: ${{ github.event_name != 'pull_request' || needs.changes.outputs.base_consumers == 'true' }}
```

Also replace the comment block above `log in to ghcr` that begins `# Pushed on every triggering event, PRs included:` so it says the push now happens on PRs only when `base_consumers` is true (keep the fork-safety sentence).

- [ ] **Step 6: Wire the guard into CI**

In `.github/workflows/checks.yml`, after the step `- name: images.yml path filters exclude Go test sources` / `run: python3 scripts/check-images-filters.py`, add:

```yaml
      # controller-base costs 0.308 GB of paid egress per push (measured
      # 2026-10-20); a PR may push it only when a leaf in that run pulls it.
      - name: images.yml pushes only what is consumed
        run: python3 scripts/check-image-registry.py
```

After the line `python3 scripts/check-images-filters_test.py`, add:

```bash
          python3 scripts/check-image-registry_test.py
```

- [ ] **Step 7: Run guard and tests, then commit**

```bash
python3 scripts/check-image-registry_test.py && python3 scripts/check-image-registry.py && \
git add .github/workflows/images.yml .github/workflows/checks.yml scripts/check-image-registry.py scripts/check-image-registry_test.py && \
git commit -m "ci(images): push controller-base on a PR only when a leaf pulls it

0.308 GB of paid egress per push, measured 2026-10-13..20 (r=0.963).
Plan docs/superpowers/plans/2026-10-21-aws-cost-cuts.md Task A1."
```

Expected: the guard prints `check-image-registry: ok`.

- [ ] **Step 8: Prove it in the PR's own run**
  - In this PR's images run, the `changes` log line reads `controller-base needed: true (pushed on a PR only if consumed: false)`. It is true for this PR only if it touched `gardenio/images/<leaf>/`; this PR touches neither, so expect `needs_base` false and no controller-base job at all.
  - Then, as a positive control, push a throwaway commit to a scratch branch PR that touches only one Go file under `gardenio/controller/cmd/trigger-engine/` (not `_test.go`). Its run must show `controller-base` built with push false (the build-push log shows no `pushing layers`).
  - Then push a commit touching `gardenio/images/trigger-engine/Dockerfile` (a comment line). That run must show `pushed on a PR only if consumed: true` and a push.
  - Close the scratch PR without merging, and paste the three run URLs into the PR description.

**OPERATOR verification after merge (read-only):**

```bash
export GH_CONFIG_DIR=~/.config/gh-ja
gh api --paginate '/orgs/mattbakerconsulting/packages/container/gardenio%2Fcache%2Fcontroller-base/versions?per_page=100' \
  --jq '.[] | select((.metadata.container.tags|length)>0) | .created_at[0:10]' | sort | uniq -c | tail -5
aws ce get-cost-and-usage --profile gardenio --region us-east-1 --time-period Start=$(date -u -d '-7 days' +%F),End=$(date -u +%F) \
  --granularity DAILY --metrics UsageQuantity UnblendedCost --filter '{"Dimensions":{"Key":"USAGE_TYPE","Values":["DataTransfer-Out-Bytes"]}}' \
  --query 'ResultsByTime[].[TimePeriod.Start,Total.UsageQuantity.Amount,Total.UnblendedCost.Amount]' --output text
```

Pass: tagged controller-base versions per day fall to about the number of release tags plus PRs that touched a leaf directory (expect under 20), and DataTransfer-Out GB/day falls by about 0.3 x the drop in pushes.

**Rollback:** revert the PR. Nothing outside the workflow changes.

**Risk:** a later job that silently relies on the PR-time base existing. Today only the leaves in the same run pull it (`images.yml:927-938`). The positive-control run in Step 8 proves a consuming PR still pushes.

---

### Task A2: ECR repositories and lifecycle for every release image and cache, GitHub OIDC push roles, and the node pull grant

**Who:** CREW (one merge-desk PR). OPERATOR applies.

**Why:** ECR has only 4 of the 11 enabled images (`gardenio/controller`, `kbeads`, `beads-viewer`, `coopmux`, from `var.gardenio_image_components`, `composition/gardenio/variables.tf:41-45`). It has no cache repos. No principal can push (`composition/gardenio/main.tf:491-494`), and the node role can pull only `ghcr/*` (`main.tf:384`). There is no GitHub OIDC provider (the only one is the EKS one).

**Savings:** none by itself; it enables A3. New cost: ECR storage at $0.10/GB-month. With the lifecycle below, expect 20-60 GB stored, so $2-6/month ($0.07-0.20/day).

**Files:**
- Modify: `gardenio/terraform/composition/gardenio/variables.tf:41-45` (`gardenio_image_components` default)
- Modify: `gardenio/terraform/composition/gardenio/main.tf:509-526` (`aws_ecr_lifecycle_policy.gardenio`)
- Modify: `gardenio/terraform/composition/gardenio/main.tf` (`module "k3s"`: add `ecr_read_repo_arns` next to `ecr_cache_repo_arns` at `:384`)
- Create: `gardenio/terraform/composition/gardenio/ci_ecr.tf`
- Modify: `gardenio/terraform/composition/gardenio/outputs.tf`
- Modify: `gardenio/terraform/modules/k3s/variables.tf` (after `ecr_cache_repo_arns`, `:41-45`)
- Modify: `gardenio/terraform/modules/k3s/main.tf:278-310` (ECR statements in `data.aws_iam_policy_document.node`)
- Create: `gardenio/terraform/modules/k3s/tests/ecr_read.tftest.hcl`

**Interfaces:**
- Produces IAM roles `arn:aws:iam::123456789012:role/gardenio-ci-ecr-release` and `arn:aws:iam::123456789012:role/gardenio-ci-ecr-pr`.
- Produces ECR repositories `gardenio/<image>` for the 11 images (kbeads, controller, agent, coop-claude, coopmux, beads-viewer, slack-bridge, trigger-engine, k3s-audit-shipper, grafana-bridge, ziti-ctrl-backup), plus `gardenio/cache/<image>` for each and `gardenio/cache/controller-base`.
- Produces k3s module variable `ecr_read_repo_arns` (list(string), default `[]`).

- [ ] **Step 1: Write the failing k3s module test**

Create `gardenio/terraform/modules/k3s/tests/ecr_read.tftest.hcl`:

```hcl
# The node role may PULL gardenio/* (images CI pushes to ECR, plan
# 2026-10-21-aws-cost-cuts Task A2) but never push, create or import there.
# Run: terraform test   (from modules/k3s/)

mock_provider "aws" {
  mock_data "aws_iam_policy_document" {
    defaults = {
      json = "{\"Version\":\"2012-10-17\",\"Statement\":[]}"
    }
  }
}
mock_provider "random" {}

variables {
  env_name    = "test"
  vpc_id      = "vpc-0123456789abcdef0"
  vpc_cidr    = "10.0.0.0/16"
  admin_cidrs = []
  subnet_ids  = ["subnet-0a", "subnet-0b"]
  zone_id     = "Z0123456789ABCDEFGHIJ"
  api_fqdn    = "k3s.example.invalid"
  join_fqdn   = "k3s-join.example.invalid"
  k3s_version = "v1.31.0+k3s1"
  ami_id      = "ami-0123456789abcdef0"
}

run "read_grant_is_pull_only" {
  command = plan
  variables {
    ecr_read_repo_arns = ["arn:aws:ecr:us-east-1:111111111111:repository/gardenio/*"]
  }
  assert {
    condition = anytrue([
      for s in data.aws_iam_policy_document.node.statement :
      s.sid == "EcrReadGardenio" &&
      toset(s.actions) == toset(["ecr:GetDownloadUrlForLayer", "ecr:BatchGetImage", "ecr:BatchCheckLayerAvailability"])
    ])
    error_message = "EcrReadGardenio must exist and carry exactly the three pull actions"
  }
  assert {
    condition = anytrue([
      for s in data.aws_iam_policy_document.node.statement : s.sid == "EcrAuth"
    ])
    error_message = "EcrAuth must exist when only ecr_read_repo_arns is set"
  }
}

run "no_read_grant_by_default" {
  command = plan
  assert {
    condition = !anytrue([
      for s in data.aws_iam_policy_document.node.statement : s.sid == "EcrReadGardenio"
    ])
    error_message = "EcrReadGardenio must not exist when ecr_read_repo_arns is empty"
  }
}
```

- [ ] **Step 2: Run it and see it fail (in CI)**

Crews do not run Terraform locally on the laptop. Push the branch and read the checks job `terraform fmt + validate + module tests`, step `k3s module tests`.
Expected: FAIL with `An input variable with the name "ecr_read_repo_arns" has not been declared`.

- [ ] **Step 3: Add the module variable and statement**

In `gardenio/terraform/modules/k3s/variables.tf`, after `variable "ecr_cache_repo_arns"`:

```hcl
variable "ecr_read_repo_arns" {
  type        = list(string)
  default     = []
  description = "ECR repository ARNs the node role may PULL from (no create, no import, no push), e.g. arn:aws:ecr:<region>:<acct>:repository/gardenio/*. CI pushes there through GitHub OIDC (plan 2026-10-21-aws-cost-cuts). Inherited by every pod that can reach IMDS, which is why it is pull-only."
}
```

In `gardenio/terraform/modules/k3s/main.tf`, change the `EcrAuth` statement's `for_each` from `length(var.ecr_cache_repo_arns) == 0 ? [] : [1]` to:

```hcl
    for_each = length(var.ecr_cache_repo_arns) + length(var.ecr_read_repo_arns) == 0 ? [] : [1]
```

and add after the `EcrPullThroughCache` dynamic statement:

```hcl
  dynamic "statement" {
    for_each = length(var.ecr_read_repo_arns) == 0 ? [] : [1]
    content {
      sid = "EcrReadGardenio"
      actions = [
        "ecr:GetDownloadUrlForLayer",
        "ecr:BatchGetImage",
        "ecr:BatchCheckLayerAvailability",
      ]
      resources = var.ecr_read_repo_arns
    }
  }
```

- [ ] **Step 4: Repositories, lifecycle, OIDC and roles in the composition**

In `composition/gardenio/variables.tf`, set the `gardenio_image_components` default to the 11 enabled images in `.github/images.json`:

```hcl
  default     = ["kbeads", "controller", "agent", "coop-claude", "coopmux", "beads-viewer", "slack-bridge", "trigger-engine", "k3s-audit-shipper", "grafana-bridge", "ziti-ctrl-backup"]
```

Replace the `policy` of `aws_ecr_lifecycle_policy.gardenio` (`main.tf:509-526`) with:

```hcl
  policy = jsonencode({
    rules = [
      {
        rulePriority = 1
        description  = "keep the 15 newest calver releases"
        selection = {
          tagStatus     = "tagged"
          tagPrefixList = ["20"]
          countType     = "imageCountMoreThan"
          countNumber   = 15
        }
        action = { type = "expire" }
      },
      {
        rulePriority = 2
        description  = "expire everything else (sha/latest-only, untagged) after 14 days"
        selection = {
          tagStatus   = "any"
          countType   = "sinceImagePushed"
          countUnit   = "days"
          countNumber = 14
        }
        action = { type = "expire" }
      },
    ]
  })
```

(ECR evaluates rules by priority, and an image matched by rule 1's tag selection is never expired by rule 2. So the 15 newest calver images survive.)

Create `composition/gardenio/ci_ecr.tf`:

```hcl
# ── CI pushes images to ECR (plan 2026-10-21-aws-cost-cuts, Phase A) ─────
# Pushing from the CI nodes to ghcr was paid internet egress ($0.09/GB,
# 95-143 GB/day on 10-19/20). EC2 -> ECR in-region is free. Runner pods get
# AWS credentials from GitHub OIDC through STS; IMDS stays denied to them
# (imdsDeny, deploy-values/gardenio-k3s.yaml).

locals {
  github_repository = "mattbakerconsulting/gardenio"
  gardenio_cache_repos = toset(concat(
    [for c in var.gardenio_image_components : "cache/${c}"],
    ["cache/controller-base"],
  ))
}

resource "aws_ecr_repository" "gardenio_cache" {
  for_each             = local.gardenio_cache_repos
  name                 = "gardenio/${each.value}"
  image_tag_mutability = "MUTABLE"
  force_delete         = true
  tags                 = merge(local.common_tags, { component = each.value })
}

resource "aws_ecr_lifecycle_policy" "gardenio_cache" {
  for_each   = aws_ecr_repository.gardenio_cache
  repository = each.value.name
  policy = jsonencode({
    rules = [
      each.key == "cache/controller-base" ? {
        rulePriority = 1
        description  = "controller-base is per-commit and only read in the same run: expire after 2 days"
        selection    = { tagStatus = "any", countType = "sinceImagePushed", countUnit = "days", countNumber = 2 }
        action       = { type = "expire" }
        } : {
        rulePriority = 1
        description  = "a replaced :buildcache manifest goes untagged: expire after 1 day"
        selection    = { tagStatus = "untagged", countType = "sinceImagePushed", countUnit = "days", countNumber = 1 }
        action       = { type = "expire" }
      }
    ]
  })
}

resource "aws_iam_openid_connect_provider" "github_actions" {
  url            = "https://token.actions.githubusercontent.com"
  client_id_list = ["sts.amazonaws.com"]
  # IAM no longer validates these for GitHub (it trusts the IdP's CA), but the
  # provider still expects a list.
  thumbprint_list = ["6938fd4d98bab03faadb97b34396831e3780aea1", "1c58a3a8518e8759bf075b76b750d4f2df264fcd"]
  tags            = local.common_tags
}

data "aws_iam_policy_document" "ci_ecr_trust_release" {
  statement {
    actions = ["sts:AssumeRoleWithWebIdentity"]
    principals {
      type        = "Federated"
      identifiers = [aws_iam_openid_connect_provider.github_actions.arn]
    }
    condition {
      test     = "StringEquals"
      variable = "token.actions.githubusercontent.com:aud"
      values   = ["sts.amazonaws.com"]
    }
    condition {
      test     = "StringLike"
      variable = "token.actions.githubusercontent.com:sub"
      values = [
        "repo:${local.github_repository}:ref:refs/tags/20*",
        "repo:${local.github_repository}:ref:refs/heads/main",
      ]
    }
  }
}

data "aws_iam_policy_document" "ci_ecr_trust_pr" {
  statement {
    actions = ["sts:AssumeRoleWithWebIdentity"]
    principals {
      type        = "Federated"
      identifiers = [aws_iam_openid_connect_provider.github_actions.arn]
    }
    condition {
      test     = "StringEquals"
      variable = "token.actions.githubusercontent.com:aud"
      values   = ["sts.amazonaws.com"]
    }
    condition {
      test     = "StringEquals"
      variable = "token.actions.githubusercontent.com:sub"
      values   = ["repo:${local.github_repository}:pull_request"]
    }
  }
}

locals {
  ecr_pull_actions = ["ecr:BatchCheckLayerAvailability", "ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer"]
  ecr_push_actions = ["ecr:InitiateLayerUpload", "ecr:UploadLayerPart", "ecr:CompleteLayerUpload", "ecr:PutImage"]
  ecr_repo_arn     = "arn:aws:ecr:${var.region}:${data.aws_caller_identity.current.account_id}:repository"
}

data "aws_iam_policy_document" "ci_ecr_release" {
  statement {
    sid       = "Auth"
    actions   = ["ecr:GetAuthorizationToken"]
    resources = ["*"]
  }
  statement {
    sid       = "PushPullGardenio"
    actions   = concat(local.ecr_pull_actions, local.ecr_push_actions)
    resources = ["${local.ecr_repo_arn}/gardenio/*"]
  }
}

data "aws_iam_policy_document" "ci_ecr_pr" {
  statement {
    sid       = "Auth"
    actions   = ["ecr:GetAuthorizationToken"]
    resources = ["*"]
  }
  statement {
    sid       = "PullCaches"
    actions   = local.ecr_pull_actions
    resources = ["${local.ecr_repo_arn}/gardenio/cache/*"]
  }
  statement {
    sid       = "PushControllerBaseOnly"
    actions   = local.ecr_push_actions
    resources = ["${local.ecr_repo_arn}/gardenio/cache/controller-base"]
  }
}

resource "aws_iam_role" "ci_ecr_release" {
  name                 = "gardenio-ci-ecr-release"
  assume_role_policy   = data.aws_iam_policy_document.ci_ecr_trust_release.json
  max_session_duration = 3600
  tags                 = local.common_tags
}

resource "aws_iam_role_policy" "ci_ecr_release" {
  name   = "ecr-push"
  role   = aws_iam_role.ci_ecr_release.id
  policy = data.aws_iam_policy_document.ci_ecr_release.json
}

resource "aws_iam_role" "ci_ecr_pr" {
  name                 = "gardenio-ci-ecr-pr"
  assume_role_policy   = data.aws_iam_policy_document.ci_ecr_trust_pr.json
  max_session_duration = 3600
  tags                 = local.common_tags
}

resource "aws_iam_role_policy" "ci_ecr_pr" {
  name   = "ecr-pr"
  role   = aws_iam_role.ci_ecr_pr.id
  policy = data.aws_iam_policy_document.ci_ecr_pr.json
}
```

If `data "aws_caller_identity" "current"` or `var.region` is not already declared in the composition, declare the data source in this file and use the literal `us-east-1`. Check with `grep -n 'aws_caller_identity\|variable "region"' gardenio/terraform/composition/gardenio/*.tf`.

In `module "k3s"` (`main.tf`), next to `ecr_cache_repo_arns`, add:

```hcl
  # CI pushes gardenio/* to ECR directly (plan 2026-10-21-aws-cost-cuts);
  # the node role pulls it, pull-only.
  ecr_read_repo_arns = ["arn:aws:ecr:us-east-1:123456789012:repository/gardenio/*"]
```

Append to `outputs.tf`:

```hcl
output "ci_ecr_role_arns" {
  value = { release = aws_iam_role.ci_ecr_release.arn, pr = aws_iam_role.ci_ecr_pr.arn }
}
```

- [ ] **Step 5: CI green, then commit**

Run `terraform fmt -recursive gardenio/terraform` in the crew pod if Terraform is installed there; otherwise rely on CI's `terraform fmt -check`. Push and wait for `terraform fmt + validate + module tests` to pass, including `k3s module tests` (both runs of `ecr_read.tftest.hcl`). Commit message: `terraform(ecr): repos, lifecycle and GitHub OIDC push roles for CI images (cost plan A2)`.

**OPERATOR apply:**

```bash
cd ~/ws/gardenio && git fetch && git merge --ff-only origin/main
# The org must use the default OIDC subject (repo:<owner>/<repo>:...):
GH_CONFIG_DIR=~/.config/gh-ja gh api /repos/mattbakerconsulting/gardenio/actions/oidc/customization/sub
# Expect {"use_default":true} (or an error saying no customization). If it is customized, STOP and adapt the trust policy's sub values.
./scripts/tf-gardenio.sh plan -target=aws_iam_openid_connect_provider.github_actions -target=aws_iam_role.ci_ecr_release -target=aws_iam_role_policy.ci_ecr_release -target=aws_iam_role.ci_ecr_pr -target=aws_iam_role_policy.ci_ecr_pr -target=aws_ecr_repository.gardenio -target=aws_ecr_lifecycle_policy.gardenio -target=aws_ecr_repository.gardenio_cache -target=aws_ecr_lifecycle_policy.gardenio_cache -target=module.k3s.aws_iam_role_policy.node
```

Before applying, check the plan:
- Creates: 1 OIDC provider, 2 roles, 2 role policies, 7 new `aws_ecr_repository.gardenio`, 12 `gardenio_cache` repos and their lifecycle policies.
- Updates in place: the 4 existing lifecycle policies, and `module.k3s.aws_iam_role_policy.node`.
- **Zero destroys.** If anything destroys, stop.

Then run `apply` with the same targets.

Verify:

```bash
aws ecr describe-repositories --profile gardenio --region us-east-1 --query 'repositories[?starts_with(repositoryName,`gardenio/`)].repositoryName' --output text | tr '\t' '\n' | sort | wc -l   # expect 23
aws iam get-role --profile gardenio --role-name gardenio-ci-ecr-release --query Role.Arn --output text
aws iam simulate-principal-policy --profile gardenio --policy-source-arn arn:aws:iam::123456789012:role/gardenio-k3s \
  --action-names ecr:BatchGetImage ecr:PutImage --resource-arns arn:aws:ecr:us-east-1:123456789012:repository/gardenio/kbeads \
  --query 'EvaluationResults[].[EvalActionName,EvalDecision]' --output text   # expect BatchGetImage allowed, PutImage implicitDeny
```

Report what was created. **Rollback:** `terraform destroy` with the same `-target`s for the new resources, and revert the PR for the lifecycle and policy changes. The repos are empty at this point, so no data is lost.

---

### Task A3: `images.yml` pushes images and the build cache to ECR through GitHub OIDC; tag builds also mirror to ghcr for one transition release

**Who:** CREW (one merge-desk PR). OPERATOR watches the first release.

**Why:** after A2, the ECR repos and roles exist. The cluster still pulls via the ghcr pull-through until A4. So for one release, tag builds push to both registries, and the cache moves to ECR immediately.

**Savings:**
- All PR-time pushes (controller-base with consumers) and all cache exports leave the internet from the first merge.
- During the mirror window, only the tag images go to ghcr (about 3.9 GB per release, no cache), down from about 25-35 GB per release.
- At about 1 release a day that is $1.9-2.8/day saved now, and the rest in A5.

**Files:**
- Modify: `.github/workflows/images.yml`
  - `env:` (`:66-67`, `REGISTRY: ghcr.io`)
  - `controller-base` job: permissions (`:583-585`), `log in to ghcr` step (`:627-632`), tags (`:644`)
  - `build` job: permissions (`:717-719`), `log in to ghcr` (`:903-908`), `compute tags` (`:914-925`), `compute build args` (`:931-942`), `build + push` cache lines (`:954-955`)
- Modify: `scripts/check-image-registry.py`, `scripts/check-image-registry_test.py`

**Interfaces:**
- Consumes: roles `gardenio-ci-ecr-release` and `gardenio-ci-ecr-pr`, and repos `gardenio/<image>` and `gardenio/cache/<image>` (Task A2).
- Consumes: output `base_consumers` (Task A1).
- Produces: images at `123456789012.dkr.ecr.us-east-1.amazonaws.com/gardenio/<image>:<calver|sha|latest>`, the cache at `.../gardenio/cache/<image>:buildcache`, and the base at `.../gardenio/cache/controller-base:<sha>`.
- Produces: the env switch `GHCR_MIRROR` (`"true"` here; Task A5 removes it).

- [ ] **Step 1: Write the failing guard tests**

Add to `scripts/check-image-registry_test.py` (before the summary line) a second fixture and cases:

```python
GOOD_ECR = GOOD.replace("name: images\n", """name: images
env:
  IMAGE_PREFIX: 123456789012.dkr.ecr.us-east-1.amazonaws.com/gardenio
""").replace("""  controller-base:
    runs-on: x
    steps:
""", """  controller-base:
    runs-on: x
    permissions:
      contents: read
      id-token: write
    steps:
      - name: AWS credentials (GitHub OIDC, no IMDS)
        uses: aws-actions/configure-aws-credentials@v4
""").replace("""  build:
    runs-on: x
    steps:
      - run: true
""", """  build:
    runs-on: x
    permissions:
      contents: read
      id-token: write
    steps:
      - name: AWS credentials (GitHub OIDC, no IMDS)
        uses: aws-actions/configure-aws-credentials@v4
      - name: build + push
        uses: docker/build-push-action@v6
        with:
          cache-from: type=registry,ref=${{ env.IMAGE_PREFIX }}/cache/${{ matrix.image }}:buildcache
          cache-to: x,image-manifest=true,oci-mediatypes=true
""")


def run_ecr(text):
    with tempfile.TemporaryDirectory() as d:
        os.makedirs(os.path.join(d, ".github", "workflows"))
        with open(os.path.join(d, ".github", "workflows", "images.yml"), "w") as f:
            f.write(text)
        env = dict(os.environ, GARDENIO_ROOT=d, CHECK_IMAGE_REGISTRY_ONLY="ecr_push")
        p = subprocess.run([sys.executable, GUARD], env=env, capture_output=True, text=True)
        return p.returncode, p.stdout + p.stderr


def case_ecr(name, text, want_rc, want_in=""):
    global PASS, FAIL
    rc, out = run_ecr(text)
    ok = rc == want_rc and want_in in out
    print(("PASS" if ok else "FAIL") + f": {name} (rc={rc})")
    if not ok:
        print(out)
    PASS, FAIL = PASS + ok, FAIL + (not ok)


case_ecr("ECR workflow passes", GOOD_ECR, 0)
case_ecr("ghcr image prefix is rejected",
         GOOD_ECR.replace("123456789012.dkr.ecr.us-east-1.amazonaws.com/gardenio", "ghcr.io/mattbakerconsulting/gardenio"),
         1, "IMAGE_PREFIX")
case_ecr("a job without id-token: write is rejected",
         GOOD_ECR.replace("      id-token: write\n", "", 1),
         1, "id-token: write")
case_ecr("cache on ghcr is rejected",
         GOOD_ECR.replace("ref=${{ env.IMAGE_PREFIX }}/cache/", "ref=ghcr.io/x/cache/"),
         1, "cache")
case_ecr("ECR cache without image-manifest is rejected",
         GOOD_ECR.replace(",image-manifest=true,oci-mediatypes=true", ""),
         1, "image-manifest=true")
```

Run: `python3 scripts/check-image-registry_test.py`
Expected: the five new cases FAIL (`ecr_push` is not a known check yet, so the guard runs no check and exits 0 where 1 is wanted, and the pass case cannot see the check run). Exit 1.

- [ ] **Step 2: Add the `ecr_push` check**

In `scripts/check-image-registry.py`, add to the docstring's check list:
`ecr_push   images and caches go to ECR; every job that builds has id-token: write and an OIDC credentials step; ECR caches carry image-manifest=true,oci-mediatypes=true.`

Then add:

```python
ECR_PREFIX = "123456789012.dkr.ecr.us-east-1.amazonaws.com/gardenio"


def check_ecr_push(text):
    errs = []
    m = re.search(r"^  IMAGE_PREFIX: (\S+)\s*$", text, re.M)
    if not m or m.group(1) != ECR_PREFIX:
        errs.append(f"env IMAGE_PREFIX must be {ECR_PREFIX}")
    for job in ("controller-base", "build"):
        body = job_block(text, job)
        if not body:
            errs.append(f"job {job} not found")
            continue
        if not re.search(r"^      id-token: write\s*$", body, re.M):
            errs.append(f"job {job} lacks permissions id-token: write")
        if "aws-actions/configure-aws-credentials@" not in body:
            errs.append(f"job {job} has no aws-actions/configure-aws-credentials step")
    for line in re.findall(r"^\s+cache-(?:from|to): (.+)$", text, re.M):
        if "ghcr.io" in line or "env.REGISTRY" in line:
            errs.append(f"cache must not live on ghcr: {line.strip()}")
    for line in re.findall(r"^\s+cache-to: (.+)$", text, re.M):
        if line.strip() not in ("", "''") and "image-manifest=true,oci-mediatypes=true" not in line:
            errs.append(f"ECR registry cache needs image-manifest=true,oci-mediatypes=true: {line.strip()}")
    return errs
```

and register it: `CHECKS = {"base_push": check_base_push, "ecr_push": check_ecr_push}`.

Run: `python3 scripts/check-image-registry_test.py`
Expected: `10 passed, 0 failed`.

Run: `python3 scripts/check-image-registry.py`
Expected: exit 1 with `ecr_push: env IMAGE_PREFIX must be ...` (the real file is not changed yet).

- [ ] **Step 3: Change `images.yml`**

Replace the `env:` block (`REGISTRY: ghcr.io`) with:

```yaml
env:
  # Images and caches live in ECR, same region as the CI nodes: a push there
  # is free, a push to ghcr is paid internet egress (plan
  # 2026-10-21-aws-cost-cuts). Guarded by scripts/check-image-registry.py.
  ECR_REGISTRY: 123456789012.dkr.ecr.us-east-1.amazonaws.com
  IMAGE_PREFIX: 123456789012.dkr.ecr.us-east-1.amazonaws.com/gardenio
  # TRANSITION (Task A3 -> A5): a calver tag build ALSO pushes its image tags
  # (not caches) to ghcr, so the ghcr pull-through the cluster uses today
  # keeps working until deploy-values points at ECR. Task A5 deletes this.
  GHCR_MIRROR: "true"
  GHCR_PREFIX: ghcr.io/mattbakerconsulting/gardenio
  ECR_ROLE_RELEASE: arn:aws:iam::123456789012:role/gardenio-ci-ecr-release
  ECR_ROLE_PR: arn:aws:iam::123456789012:role/gardenio-ci-ecr-pr
```

In both the `controller-base` and `build` jobs, set `permissions:` to:

```yaml
    permissions:
      contents: read
      packages: write # ghcr mirror on calver tags only, removed in Task A5
      id-token: write # GitHub OIDC -> STS -> ECR; runners never touch IMDS
```

In both jobs, replace the `log in to ghcr` step with these three steps:

```yaml
      - name: AWS credentials (GitHub OIDC, no IMDS)
        uses: aws-actions/configure-aws-credentials@v4
        with:
          aws-region: us-east-1
          role-to-assume: ${{ github.event_name == 'pull_request' && env.ECR_ROLE_PR || env.ECR_ROLE_RELEASE }}
          role-session-name: images-${{ github.run_id }}-${{ github.run_attempt }}
      - name: log in to ECR
        uses: aws-actions/amazon-ecr-login@v2
      - name: log in to ghcr (calver mirror, transition only)
        if: github.ref_type == 'tag' && env.GHCR_MIRROR == 'true'
        uses: docker/login-action@v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}
```

(The `controller-base` job does not need the ghcr step, because the base is never mirrored. Omit the third step there.)

In `controller-base`, change `tags:` to `${{ env.IMAGE_PREFIX }}/cache/controller-base:${{ github.sha }}`.

In the `build` job's `compute tags` step, replace the body with:

```bash
          {
            echo "list<<TAGS_EOF"
            echo "${{ env.IMAGE_PREFIX }}/${{ matrix.image }}:latest"
            echo "${{ env.IMAGE_PREFIX }}/${{ matrix.image }}:${{ github.sha }}"
            if [ "${{ github.ref_type }}" = "tag" ]; then
              echo "${{ env.IMAGE_PREFIX }}/${{ matrix.image }}:${{ github.ref_name }}"
              if [ "${{ env.GHCR_MIRROR }}" = "true" ]; then
                echo "${{ env.GHCR_PREFIX }}/${{ matrix.image }}:latest"
                echo "${{ env.GHCR_PREFIX }}/${{ matrix.image }}:${{ github.sha }}"
                echo "${{ env.GHCR_PREFIX }}/${{ matrix.image }}:${{ github.ref_name }}"
              fi
            fi
            echo "TAGS_EOF"
          } >> "$GITHUB_OUTPUT"
```

In `compute build args`, change the `CONTROLLER_BASE=` line to `echo "CONTROLLER_BASE=${{ env.IMAGE_PREFIX }}/cache/controller-base:${{ github.sha }}"`.

In `build + push`, replace the two cache lines with:

```yaml
          # Cache in ECR (free in-region). ECR needs the cache written as an
          # OCI image manifest. One mutable tag per image; the lifecycle
          # policy expires the replaced manifest after a day.
          cache-from: type=registry,ref=${{ env.IMAGE_PREFIX }}/cache/${{ matrix.image }}:buildcache
          cache-to: ${{ github.event_name != 'pull_request' && format('type=registry,ref={0}/cache/{1}:buildcache,mode=max,image-manifest=true,oci-mediatypes=true', env.IMAGE_PREFIX, matrix.image) || '' }}
```

Search the file for any remaining `env.REGISTRY` and replace it with `env.IMAGE_PREFIX`, minding the different path shape (`IMAGE_PREFIX` already ends in `/gardenio`, so `${{ env.REGISTRY }}/${{ github.repository }}` becomes `${{ env.IMAGE_PREFIX }}`).

Run: `grep -n 'env.REGISTRY\|github.repository }}/' .github/workflows/images.yml`
Expected: no output.

- [ ] **Step 4: Guard and tests pass on the real tree; commit**

```bash
python3 scripts/check-image-registry_test.py && python3 scripts/check-image-registry.py && \
git add .github/workflows/images.yml scripts/check-image-registry.py scripts/check-image-registry_test.py && \
git commit -m "ci(images): push images and caches to ECR via GitHub OIDC; ghcr mirror on tags for one release (cost plan A3)"
```

- [ ] **Step 5: Prove it on the PR**

The PR's own images run (pull_request) must show:
- `AWS credentials` succeeding with role `gardenio-ci-ecr-pr`
- leaf builds reading `cache-from` from ECR (a first run reports `importing cache manifest ... not found`, which is fine)
- if a leaf directory is touched, `controller-base` pushed to `.../gardenio/cache/controller-base:<sha>`

Paste the run URL. The `IMDS is unreachable from a runner pod` check must still pass in `checks.yml`.

**OPERATOR, after merge, on the next release** (or cut one with `gardenio/scripts/release-cut.sh`):

```bash
TAG=$(git -C ~/ws/gardenio ls-remote --tags origin '20*' | awk -F/ '{print $3}' | grep -v '\^{}' | sort -V | tail -1); echo "$TAG"
for i in kbeads controller agent coop-claude coopmux beads-viewer slack-bridge trigger-engine k3s-audit-shipper grafana-bridge ziti-ctrl-backup; do
  printf '%-18s ecr=' "$i"
  aws ecr describe-images --profile gardenio --region us-east-1 --repository-name "gardenio/$i" --image-ids imageTag="$TAG" --query 'imageDetails[0].imagePushedAt' --output text 2>/dev/null || printf 'MISSING'
  echo
done
```

Pass: all 11 have a push time. The release-deployer still rolls from ghcr (unchanged), and `kubectl -n gardenio get pods -o wide` shows the new tag Running.

**Rollback:** revert the PR. Pushes go back to ghcr, and nothing in the cluster changed.

---

### Task A4: The cluster pulls gardenio images from ECR directly

**Who:** CREW (draft PR, `merge-desk:hold`). OPERATOR merges and rolls in a watched window.

**Why:** after A3, every calver image exists in ECR at `gardenio/<image>`. Pointing `global.imageRegistry` there makes the ghcr mirror (and its egress) unnecessary.

**Savings:** none until A5. This is the gate for A5.

**Files:**
- Modify: `deploy-values/gardenio-k3s.yaml:46-50`. The comment, and the line `imageRegistry: "123456789012.dkr.ecr.us-east-1.amazonaws.com/ghcr/mattbakerconsulting/gardenio"`.
- Modify: `gardenio/tests/e2e/lane/values-e2e.yaml:25`, same line.
- Modify: `gardenio/docs/release-pipeline.md:195-203`, the pull path paragraph.
- Not changed, on purpose: the fixed-tag refs to old pull-through tags in `gardenio/helm/ziti/templates/controller.yaml:188,430,688`, `gardenio/helm/k3s-audit-shipper/shipper.yaml:69` and `gardenio/helm/manifests/machine-principal-canary-job.yaml:102`. Those tags stay in the pull-through cache.

- [ ] **Step 1: Change the registry**

In `deploy-values/gardenio-k3s.yaml`, replace the four comment lines above `imageRegistry:` and the line itself with:

```yaml
  # CI pushes every gardenio image to this account's ECR (images.yml, plan
  # 2026-10-21-aws-cost-cuts): <acct>.dkr.ecr.<region>.amazonaws.com/gardenio/<c>.
  # The node role pulls it (EcrReadGardenio); the `ecr` pull secret comes from
  # the ecr-token-refresh CronJob. The ghcr pull-through rule is kept for the
  # pinned old tags and as the rollback path.
  imageRegistry: "123456789012.dkr.ecr.us-east-1.amazonaws.com/gardenio"
```

Make the same `imageRegistry:` change in `gardenio/tests/e2e/lane/values-e2e.yaml:25`. In `release-pipeline.md`, replace the pull-through paragraph with two sentences saying images are pushed to and pulled from ECR `gardenio/<c>`, and that ghcr is not used for gardenio images after the cost plan.

- [ ] **Step 2: Render check**

Run, in the crew pod: `helm template gardenio gardenio/helm/gardenio -f deploy-values/gardenio-k3s.yaml | grep -o '123456789012.dkr.ecr.us-east-1.amazonaws.com/[a-z/-]*gardenio/[a-z-]*' | sort | uniq -c`
Expected: only `.../gardenio/<c>` paths, and no `ghcr/mattbakerconsulting` paths, for the chart's gardenio components.

Run the chart tests: `cd gardenio/helm/gardenio/tests && go test ./...`
Expected: PASS. If a test pins the pull-through path for deploy-values, update its expectation.

Commit: `deploy-values: pull gardenio images from ECR directly (cost plan A4)`. Open the PR as DRAFT with label `merge-desk:hold`, and post the operator steps below on the task.

**OPERATOR window:**

```bash
cd ~/ws/gardenio && git fetch && git merge --ff-only origin/main
TAG=$(git ls-remote --tags origin '20*' | awk -F/ '{print $3}' | grep -v '\^{}' | sort -V | tail -1)
# 1. Every image of the release the deployer will roll exists in ECR (copy of the loop in Task A3).
for i in kbeads controller agent coop-claude coopmux beads-viewer slack-bridge trigger-engine k3s-audit-shipper grafana-bridge ziti-ctrl-backup; do
  aws ecr describe-images --profile gardenio --region us-east-1 --repository-name "gardenio/$i" --image-ids imageTag="$TAG" >/dev/null 2>&1 || echo "MISSING $i:$TAG"
done
# Any MISSING: do not merge.
# 2. The node role can pull: pull one small image through the cluster.
export KUBECONFIG=$HOME/ws/scratch/ssm.kubeconfig
kubectl -n gardenio run ecr-pull-probe --restart=Never --image=123456789012.dkr.ecr.us-east-1.amazonaws.com/gardenio/trigger-engine:$TAG \
  --overrides='{"spec":{"imagePullSecrets":[{"name":"ecr"}],"tolerations":[{"key":"gardenio.agent","operator":"Equal","value":"true","effect":"NoSchedule"}]}}' --command -- /trigger-engine --help
kubectl -n gardenio wait --for=jsonpath='{.status.phase}'=Succeeded pod/ecr-pull-probe --timeout=120s || kubectl -n gardenio describe pod ecr-pull-probe | tail -20
kubectl -n gardenio delete pod ecr-pull-probe
```

A Succeeded or Failed phase after an image pull is a pass (the binary's `--help` exit code does not matter). `ErrImagePull` is a fail: stop. Then:
- Mark the PR ready and merge it.
- Let the release-deployer roll (or do a values-only admin roll per the calver-release recipe).
- Watch the pods:

```bash
kubectl -n gardenio get pods -o json | python3 -c 'import json,sys; [print(c["image"]) for p in json.load(sys.stdin)["items"] for c in p["spec"]["containers"] if "/gardenio/" in c["image"]]' | sort | uniq -c
kubectl -n gardenio get pods | grep -E 'ImagePull|ErrImage|CrashLoop' || echo "no pull errors"
```

Pass:
- Every gardenio container image is `...amazonaws.com/gardenio/<c>:<calver>`.
- There are no pull errors.
- A newly spawned crew pod (`COOP_IMAGE`) shows `.../gardenio/agent:<calver>`.

**Rollback:** revert the deploy-values PR and roll. The ghcr pull-through still serves every calver pushed while `GHCR_MIRROR` was true.

---

### Task A5: Stop pushing gardenio images to ghcr

**Who:** CREW (merge-desk PR). OPERATOR verifies.

**When:** A4 has been live through at least one deployer roll and 48 h with no pull errors.

**Savings:** the remaining release egress, about 3.9 GB per release of tag images. Combined with A3, the release egress goes from about 25-35 GB to about 0 per release: **$2.3-3.2/day** at one release a day.

**Files:**
- Modify: `.github/workflows/images.yml` (env `GHCR_MIRROR`, `GHCR_PREFIX`; `packages: write`; the `log in to ghcr` step; the ghcr lines in `compute tags`)
- Modify: `scripts/check-image-registry.py`, `scripts/check-image-registry_test.py`

- [ ] **Step 1: Failing test**

Add to the test file:

```python
def run_only(text, only):
    with tempfile.TemporaryDirectory() as d:
        os.makedirs(os.path.join(d, ".github", "workflows"))
        with open(os.path.join(d, ".github", "workflows", "images.yml"), "w") as f:
            f.write(text)
        env = dict(os.environ, GARDENIO_ROOT=d, CHECK_IMAGE_REGISTRY_ONLY=only)
        p = subprocess.run([sys.executable, GUARD], env=env, capture_output=True, text=True)
        return p.returncode, p.stdout + p.stderr


for name, text, want in [
    ("no ghcr anywhere passes", GOOD_ECR, 0),
    ("a ghcr login is rejected", GOOD_ECR + "      - uses: docker/login-action@v3\n        with:\n          registry: ghcr.io\n", 1),
    ("a ghcr tag is rejected", GOOD_ECR + '            echo "ghcr.io/mattbakerconsulting/gardenio/x:latest"\n', 1),
]:
    rc, out = run_only(text, "no_ghcr")
    ok = rc == want
    print(("PASS" if ok else "FAIL") + f": {name} (rc={rc})")
    if not ok:
        print(out)
    PASS, FAIL = PASS + ok, FAIL + (not ok)
```

Run: `python3 scripts/check-image-registry_test.py`
Expected: the two reject cases FAIL (the check does not exist yet).

- [ ] **Step 2: Add the check**

```python
def check_no_ghcr(text):
    """After the transition, nothing in images.yml may push to or log in to ghcr."""
    errs = []
    for i, line in enumerate(text.splitlines(), 1):
        if line.lstrip().startswith("#"):
            continue
        if "ghcr.io" in line:
            errs.append(f"images.yml:{i} still names ghcr.io: {line.strip()}")
    return errs
```

and register it: `CHECKS = {"base_push": check_base_push, "ecr_push": check_ecr_push, "no_ghcr": check_no_ghcr}`. Update the docstring.

Run: `python3 scripts/check-image-registry.py`
Expected: exit 1 listing the ghcr lines (the positive control).

- [ ] **Step 3: Remove the mirror**

- Delete `GHCR_MIRROR` and `GHCR_PREFIX` from `env:`.
- Delete `packages: write` from both jobs.
- Delete the step `log in to ghcr (calver mirror, transition only)`.
- Delete the inner `if [ "${{ env.GHCR_MIRROR }}" = "true" ]; then ... fi` block in `compute tags`.
- Delete the `TRANSITION` comment.

Then run:

```bash
python3 scripts/check-image-registry_test.py && python3 scripts/check-image-registry.py && \
git add .github/workflows/images.yml scripts/check-image-registry.py scripts/check-image-registry_test.py && \
git commit -m "ci(images): stop pushing gardenio images to ghcr (cost plan A5)"
```

**OPERATOR verification, after the next release:**

```bash
export GH_CONFIG_DIR=~/.config/gh-ja
gh api '/orgs/mattbakerconsulting/packages/container/gardenio%2Fagent/versions?per_page=5' --jq '.[] | [.created_at, (.metadata.container.tags|join(","))] | @tsv'
# The new calver must NOT appear on ghcr; it must appear in ECR (loop from Task A3).
```

Then compare DataTransfer-Out per day (the Cost Explorer command in Task A1) for 7 days against the baseline table.

Expected: under 10 GB/day. The residual is non-push traffic, which Task C1 measures.

**Rollback:** revert this PR and the A4 PR together, then cut a release so ghcr gets the current calver again.

---

## Phase B: Postgres and beads daemon traffic

### Task B1: The hourly backup pod runs on the Postgres node

**Who:** CREW (merge-desk PR, chart). OPERATOR verifies three runs.

**Why:**
- `gardenio-postgres-s3-backup` (`gardenio/helm/gardenio/templates/postgres/s3-backup-cronjob.yaml`, schedule `17 * * * *`) pulls a `pg_dumpall` of about 17-19 GB per run over the network from `gardenio-postgres-0`.
- It lands on any on-demand platform node, and Postgres is pinned to `ip-10-90-30-213` (us-east-1b). 2 of 4 runs on 10-21 landed on `ip-10-90-2-170` (1a).
- A cross-AZ run costs about 18 GB x $0.02 = $0.36.

**Savings:** about 8 cross-AZ runs a day x $0.36 = **about $2.9/day** at today's dump size, growing with the database.

**Files:**
- Modify: `gardenio/helm/gardenio/templates/postgres/s3-backup-cronjob.yaml` (pod spec, next to the `gardenio.nodeSelector` / `gardenio.tolerations` includes at `:60-61`)
- Modify: the chart test that renders this CronJob. Find it with `grep -rln 'postgres-s3-backup' gardenio/helm/gardenio/tests`. If there is none, create `gardenio/helm/gardenio/tests/postgres_backup_affinity_test.go` in the same package and style as the neighbouring `*_test.go` files there.

- [ ] **Step 1: Failing render test**

Add a test that renders the chart with `deploy-values/gardenio-k3s.yaml` and asserts that the CronJob `gardenio-postgres-s3-backup` pod spec has:

```yaml
affinity:
  podAffinity:
    preferredDuringSchedulingIgnoredDuringExecution:
      - weight: 100
        podAffinityTerm:
          topologyKey: kubernetes.io/hostname
          labelSelector:
            matchLabels:
              app.kubernetes.io/component: postgres
              app.kubernetes.io/instance: gardenio
```

Use the render helper the other chart tests use. Find it with `grep -rn 'func render' gardenio/helm/gardenio/tests/*.go | head`.

Run: `cd gardenio/helm/gardenio/tests && go test -run PostgresBackupAffinity ./...`
Expected: FAIL (no affinity rendered).

- [ ] **Step 2: Add the affinity**

In the CronJob pod spec, directly after the tolerations include, add:

```yaml
          # Run on the postgres node so the ~18 GB dump never crosses an AZ
          # (cross-AZ is $0.01/GB each way; 2 of 4 runs on 2026-10-21 landed
          # in 1a while postgres is pinned to a 1b node by its local PV; plan
          # 2026-10-21-aws-cost-cuts Task B1). Preferred, not required: a
          # full node must delay nothing, it only costs what it costs today.
          affinity:
            podAffinity:
              preferredDuringSchedulingIgnoredDuringExecution:
                - weight: 100
                  podAffinityTerm:
                    topologyKey: kubernetes.io/hostname
                    labelSelector:
                      matchLabels:
                        app.kubernetes.io/component: postgres
                        app.kubernetes.io/instance: {{ .Release.Name }}
```

- [ ] **Step 3: Test passes, commit**

```bash
cd gardenio/helm/gardenio/tests && go test ./... && cd - && \
git add gardenio/helm/gardenio/templates/postgres/s3-backup-cronjob.yaml gardenio/helm/gardenio/tests && \
git commit -m "chart(postgres-backup): run the dump on the postgres node (cost plan B1)"
```

**OPERATOR verification, after the next roll** (read-only):

```bash
export KUBECONFIG=$HOME/ws/scratch/ssm.kubeconfig
# Each hourly job's pod node; expect ip-10-90-30-213 (or whatever node gardenio-postgres-0 is on) for 3 runs in a row.
kubectl -n gardenio get pods -l app.kubernetes.io/component=postgres-s3-backup -o wide --sort-by=.metadata.creationTimestamp | tail -3
kubectl -n gardenio get pod gardenio-postgres-0 -o wide
aws s3 ls s3://gardenio-hq-backups-123456789012/k3s-postgres/ --profile gardenio --region us-east-1 | tail -3   # a new object every hour
```

Also check the node has room for the 2.7 GB gzip in the pod's emptyDir: `kubectl get --raw /api/v1/nodes/ip-10-90-30-213/proxy/stats/summary | python3 -c 'import json,sys; d=json.load(sys.stdin)["node"]["fs"]; print(round(d["availableBytes"]/1e9,1),"GB free")'`. Expect more than 10 GB free.

**Rollback:** revert the PR.

**Risk:** gzip (limit 500m CPU) now runs next to Postgres, on a node at 92% of CPU requests, for about 35 minutes an hour. If the daemon's p95 latency rises during :17-:52, switch the term to the zone (topologyKey `topology.kubernetes.io/zone`) once Task B4's zone labels exist, so the backup goes to the other 1b node.

---

### Task B2: beads-viewer's garden hub and farm refresh only for events that can change them

**Who:** CREW (Go PR, not a merge-desk path). OPERATOR measures after the release.

**Why (measured 2026-10-21):**
- beads-viewer receives 102.4 GB/day from the daemon and sends 1.0 GB/day. It never drops below about 1 MB/s, even overnight.
- `gardenhub.Hub.watch` (`gardenio/controller/internal/gardenhub/hub.go:262-291`) marks the hub dirty and calls `Farm.Invalidate` for every `beads.bead.>` event.
- `Farm.readRows` (`farm.go:283-317`) then re-reads every open work bead: 876 rows, 3.44 MB, at most once per settle (250 ms) plus a gap of `farmMinGap` (1 s), in each of the 2 replicas.
- In 10 minutes there were 284 mutation lines, and **none** were for the farm's types. They were agent 114, trigger 78, message 43, session 28, account 13, audit 6 and event 2. Most of the agent ones are the controller's 60 s usage writes.
- `Hub.fetch` (`hub.go:425-450`) re-lists all in_progress beads with the `has_report` and `blocked_on_decision` projections, plus open decisions and escalations, on every event too. Those projections are the per-row `deps` and `labels` probes that show up as about 1,200 index scans per second each.

**Savings:**
- Viewer reads: about 102 GB/day down to about 5 GB/day (a full farm read every 5 minutes per replica, plus roster-only refreshes).
- The dollar effect depends on placement. At 50% cross-AZ it is about $1/day.
- It also removes most of the daemon's and Postgres's CPU on these reads.

**Files:**
- Create: `gardenio/controller/internal/gardenhub/relevance.go`
- Create: `gardenio/controller/internal/gardenhub/relevance_test.go`
- Modify: `gardenio/controller/internal/gardenhub/hub.go` (`onEvent` field `:186-188`, `OnEvent` `:196`, `watch` `:262-291`, `fireEvent` `:293-297`)
- Modify: `gardenio/controller/internal/gardenhub/farm.go` (constants `:42-77`, the `Farm` struct, `NewFarm` `:186`, `loop` `:243-280`, `refresh` and `refreshOnce` `:533-560`)
- Modify: `gardenio/controller/internal/gardenhub/hub_test.go`, `farm_test.go` (fake gains a work-row counter)
- Modify: `gardenio/controller/cmd/beads-viewer/main.go:414` (`gardenHub.OnEvent(gardenFarm.Invalidate)`)

**Interfaces:**
- Produces: `type Relevance struct{ Hub, Farm, Roster bool }`.
- Produces: `func classify(ev beadsapi.SSEEvent) Relevance`.
- Produces: `func (h *Hub) OnEvent(fn func(Relevance))`.
- Produces: `func (f *Farm) InvalidateFor(r Relevance)`.
- Produces: `func (f *Farm) refreshRoster(ctx context.Context) error`.
- `Farm.Invalidate()` keeps its meaning (a full refresh) for `wake.go` and `adopt.go`.

- [ ] **Step 1: Failing classifier tests**

Create `relevance_test.go`:

```go
package gardenhub

import (
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func ev(event, data string) beadsapi.SSEEvent {
	return beadsapi.SSEEvent{Event: event, Data: []byte(data)}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		in   beadsapi.SSEEvent
		want Relevance
	}{
		{"agent usage write touches only the roster",
			ev("bead.updated", `{"bead":{"type":"agent","status":"open"},"changes":{"fields":{}}}`), Relevance{Roster: true}},
		{"trigger write touches nothing",
			ev("bead.updated", `{"bead":{"type":"trigger","status":"open"},"changes":{"fields":{}}}`), Relevance{}},
		{"message create touches nothing",
			ev("bead.created", `{"bead":{"type":"message","status":"open"}}`), Relevance{}},
		{"task update touches the farm",
			ev("bead.updated", `{"bead":{"type":"task","status":"open"},"changes":{"title":"x"}}`), Relevance{Farm: true}},
		{"in_progress task touches hub and farm",
			ev("bead.updated", `{"bead":{"type":"task","status":"in_progress"},"changes":{"notes":"x"}}`), Relevance{Hub: true, Farm: true}},
		{"a status change of any type touches the hub",
			ev("bead.updated", `{"bead":{"type":"session","status":"closed"},"changes":{"status":"closed"}}`), Relevance{Hub: true}},
		{"decision touches hub and farm",
			ev("bead.created", `{"bead":{"type":"decision","status":"open"}}`), Relevance{Hub: true, Farm: true}},
		{"report touches the hub (has_report)",
			ev("bead.created", `{"bead":{"type":"report","status":"open"}}`), Relevance{Hub: true}},
		{"close of any type touches the hub",
			ev("bead.closed", `{"bead":{"type":"message","status":"closed"}}`), Relevance{Hub: true}},
		{"unreadable payload fails open", ev("bead.updated", `not json`), relevantAll},
		{"payload without a bead fails open", ev("bead.deleted", `{"id":"gd-1"}`), relevantAll},
		{"empty payload fails open", ev("bead.updated", ``), relevantAll},
	}
	for _, c := range cases {
		if got := classify(c.in); got != c.want {
			t.Errorf("%s: classify = %+v, want %+v", c.name, got, c.want)
		}
	}
}
```

Run: `cd gardenio/controller && go test ./internal/gardenhub/ -run TestClassify`
Expected: FAIL to compile (`undefined: classify`, `undefined: Relevance`).

- [ ] **Step 2: Write the classifier**

Create `relevance.go`:

```go
package gardenhub

import (
	"encoding/json"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// Relevance says which snapshot an upstream bead event can change. The daemon
// already puts the whole bead on beads.bead.* events (kbeads/internal/events:
// BeadCreated, BeadUpdated, BeadClosed), so the type and status are free to
// read. Measured 2026-10-21: of 284 mutations in ten minutes none was a farm
// work type (114 agent, 78 trigger, 43 message, ...), yet each one re-read
// 876 open work beads (3.44 MB) in each viewer replica. Plan
// docs/superpowers/plans/2026-10-21-aws-cost-cuts.md Task B2.
type Relevance struct {
	Hub    bool // Hub.fetch: in_progress beads of any type, open decisions and escalations
	Farm   bool // Farm.readRows: the farmWorkTypes rows
	Roster bool // the farm's live-agent roster and gardeners (type agent)
}

// relevantAll is a (re)connect, or an event we cannot read: anything may have
// changed. It is the old behaviour, so failing open costs bytes, never
// freshness.
var relevantAll = Relevance{Hub: true, Farm: true, Roster: true}

// hubTypes change Hub.fetch whatever their status: it lists open decisions and
// escalations, and a report feeds the in_progress rows' has_report.
var hubTypes = map[string]bool{"decision": true, "escalation": true, "report": true}

var farmTypeSet = func() map[string]bool {
	m := make(map[string]bool, len(farmWorkTypes))
	for _, t := range farmWorkTypes {
		m[t] = true
	}
	return m
}()

type eventPayload struct {
	Bead *struct {
		Type   string `json:"type"`
		Status string `json:"status"`
	} `json:"bead"`
	Changes map[string]json.RawMessage `json:"changes"`
}

func classify(ev beadsapi.SSEEvent) Relevance {
	var p eventPayload
	if len(ev.Data) == 0 || json.Unmarshal(ev.Data, &p) != nil || p.Bead == nil || p.Bead.Type == "" {
		return relevantAll
	}
	t, s := p.Bead.Type, p.Bead.Status
	_, statusChanged := p.Changes["status"]
	left := strings.HasSuffix(ev.Event, ".closed") || strings.HasSuffix(ev.Event, ".deleted") ||
		strings.HasSuffix(ev.Event, ".transferred")
	return Relevance{
		Hub:    hubTypes[t] || s == "in_progress" || statusChanged || left,
		Farm:   farmTypeSet[t],
		Roster: t == "agent",
	}
}
```

Run: `go test ./internal/gardenhub/ -run TestClassify`
Expected: PASS.

- [ ] **Step 3: Failing hub and farm wiring tests**

In `farm_test.go`, add a field `workLists int` to `fakeFarmSrc`. In its `ListBeadsFiltered`, right after `f.lists++`, add `if in(q.Types, "task") { f.workLists++ }`.

Add to `hub_test.go`:

```go
func TestHubIgnoresIrrelevantEvents(t *testing.T) {
	f := newFake()
	h := New(f, "", nil)
	var got []Relevance
	var mu sync.Mutex
	h.OnEvent(func(r Relevance) { mu.Lock(); got = append(got, r); mu.Unlock() })
	h.Start()
	defer h.Close()
	time.Sleep(2 * refreshCoalesce) // let the connect refresh settle
	f.mu.Lock()
	before := f.lists
	f.mu.Unlock()
	f.events <- beadsapi.SSEEvent{Event: "bead.updated", Data: []byte(`{"bead":{"type":"agent","status":"open"},"changes":{"fields":{}}}`)}
	time.Sleep(3 * refreshCoalesce)
	f.mu.Lock()
	after := f.lists
	f.mu.Unlock()
	if after != before {
		t.Fatalf("agent usage event re-read the hub: lists %d -> %d", before, after)
	}
	f.events <- beadsapi.SSEEvent{Event: "bead.created", Data: []byte(`{"bead":{"type":"decision","status":"open"}}`)}
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.mu.Lock()
		n := f.lists
		f.mu.Unlock()
		if n > after {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("decision event did not refresh the hub (positive control)")
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) < 2 || got[len(got)-1] != (Relevance{Hub: true, Farm: true}) {
		t.Fatalf("onEvent relevances = %+v, want the last to be {Hub Farm}", got)
	}
}
```

Add to `farm_test.go`:

```go
func TestFarmRosterEventDoesNotReadRows(t *testing.T) {
	f := newFakeFarm()
	fm := NewFarm(f, nil)
	fm.Start()
	defer fm.Close()
	f.mu.Lock()
	before := f.workLists
	f.mu.Unlock()
	if before == 0 {
		t.Fatal("warm-up did not read the work rows")
	}
	fm.InvalidateFor(Relevance{Roster: true})
	time.Sleep(farmRosterGap + 500*time.Millisecond)
	f.mu.Lock()
	afterRoster := f.workLists
	f.mu.Unlock()
	if afterRoster != before {
		t.Fatalf("roster-only event re-read the work rows: %d -> %d", before, afterRoster)
	}
	fm.InvalidateFor(Relevance{Farm: true})
	deadline := time.Now().Add(3 * time.Second)
	for {
		f.mu.Lock()
		n := f.workLists
		f.mu.Unlock()
		if n > afterRoster {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("farm event did not re-read the work rows (positive control)")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
```

To keep this test fast, declare `farmRosterGap` as a `var` (not a `const`) so a test can shorten it. If the real 15 s makes the test slow, set `farmRosterGap = 200 * time.Millisecond` at the start of the test and restore it with `defer`.

Run: `go test ./internal/gardenhub/`
Expected: FAIL to compile (`OnEvent` takes `func()`, and `InvalidateFor` and `farmRosterGap` are undefined).

- [ ] **Step 4: Wire the hub**

In `hub.go`:
- Change the field to `onEvent func(Relevance)`.
- Change the method to `func (h *Hub) OnEvent(fn func(Relevance)) { h.onEvent = fn }`. Update its comment: called with what each event can change, and `relevantAll` on every (re)connect.
- In `watch`, replace the connect lines and the pump with:

```go
		h.setUpstream(true)
		h.markDirty() // events may have been missed while disconnected
		h.fireEvent(relevantAll)
	pump:
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-events:
				if !ok {
					break pump
				}
				r := classify(ev)
				if r.Hub {
					h.markDirty()
				}
				h.fireEvent(r)
			}
		}
```

Replace `fireEvent` with:

```go
func (h *Hub) fireEvent(r Relevance) {
	if h.onEvent != nil && (r.Farm || r.Roster) {
		h.onEvent(r)
	}
}
```

Update the package doc's sentence that every event marks the snapshot dirty, so it says only relevant events do.

- [ ] **Step 5: Wire the farm**

In `farm.go`, add next to the other constants:

```go
	// farmFullEvery bounds how stale the work rows get when no work-type event
	// arrives; a roster-only refresh reuses the last rows (cost plan B2).
	farmFullEvery = 5 * time.Minute
```

and, as a package `var` (tests shorten it):

```go
// farmRosterGap is the least time between two roster-only refreshes.
var farmRosterGap = 15 * time.Second
```

Add to the `Farm` struct:

```go
	rosterDirty chan struct{}
	// The last readRows result, reused by roster-only refreshes. Touched only
	// by refresh, which is serialised (Start's warm-up, then loop).
	rows          []*beadsapi.BeadDetail
	rowsTruncated bool
	rowsAt        time.Time
```

and initialise `rosterDirty: make(chan struct{}, 1),` in `NewFarm`.

Add:

```go
// InvalidateFor is the Hub's OnEvent hook. A work-type event re-reads the rows;
// an agent event only re-reads the roster and recomposes from the last rows.
func (f *Farm) InvalidateFor(r Relevance) {
	if r.Farm {
		f.Invalidate()
		return
	}
	if r.Roster {
		select {
		case f.rosterDirty <- struct{}{}:
		default:
		}
	}
}
```

In `loop`, replace the `select` and the refresh call with:

```go
		full := true
		select {
		case <-ctx.Done():
			return
		case <-reconcile.C:
			full = f.now().Sub(f.rowsAt) >= farmFullEvery
		case <-f.rosterDirty:
			full = false
			if gap := farmRosterGap - time.Since(lastEnd); gap > 0 {
				if !sleep(ctx, gap) {
					return
				}
			}
		case <-f.dirty:
			wait := farmSettle
			if gap := farmMinGap - time.Since(lastEnd); gap > wait {
				wait = gap
			}
			if !sleep(ctx, wait) {
				return
			}
			select {
			case <-f.dirty:
			default:
			}
		}
		rctx, cancel := context.WithTimeout(ctx, refreshTimeout)
		var err error
		if full {
			err = f.refresh(rctx)
		} else {
			err = f.refreshRoster(rctx)
		}
		cancel()
```

Keep the rest of the loop body (the `lastEnd` update and the error retry) unchanged.

Split `refresh`:

```go
func (f *Farm) refresh(ctx context.Context) error       { return f.refreshTimed(ctx, true) }
func (f *Farm) refreshRoster(ctx context.Context) error { return f.refreshTimed(ctx, false) }

func (f *Farm) refreshTimed(ctx context.Context, readRows bool) error {
	start := time.Now()
	err := f.refreshOnce(ctx, readRows)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastRefresh = time.Since(start)
	if err != nil {
		f.lastErr = err.Error()
	} else {
		f.lastErr = ""
	}
	return err
}
```

Change `refreshOnce(ctx)` to `refreshOnce(ctx context.Context, readRows bool)`, and replace its first lines (`rows, truncated, err := f.readRows(ctx)` / `if err != nil { return err }`) with:

```go
	rows, truncated := f.rows, f.rowsTruncated
	if readRows || f.rows == nil {
		var err error
		if rows, truncated, err = f.readRows(ctx); err != nil {
			return err
		}
		f.rows, f.rowsTruncated, f.rowsAt = rows, truncated, f.now()
	}
```

Rows are reused, so check that `refreshOnce` never writes into a `*beadsapi.BeadDetail` from `rows`:

`grep -nE '\bb\.[A-Z][A-Za-z]* *=[^=]|byID\[[^]]*\]\.[A-Z][A-Za-z]* *=[^=]' internal/gardenhub/farm*.go`

Expected: no output. If there is output, copy each row (`c := *b; rows[i] = &c`) before reuse, and say so in the PR.

Update every other caller of `refreshOnce(` to pass `true`; find them with `grep -n 'refreshOnce(' internal/gardenhub/*.go`.

In `cmd/beads-viewer/main.go:414`, change the call to `gardenHub.OnEvent(gardenFarm.InvalidateFor)`.

- [ ] **Step 6: Tests, lint, commit**

```bash
cd gardenio/controller && go test ./internal/gardenhub/ ./cmd/beads-viewer/ && gofumpt -l internal/gardenhub cmd/beads-viewer && go vet ./internal/gardenhub/ ./cmd/beads-viewer/ && staticcheck ./internal/gardenhub/ ./cmd/beads-viewer/ && cd - && \
git add gardenio/controller/internal/gardenhub gardenio/controller/cmd/beads-viewer/main.go && \
git commit -m "gardenhub: refresh hub and farm only for events that can change them (cost plan B2)

Measured 2026-10-21: 102 GB/day daemon->viewer; each of 284 mutations in
10 min (none a farm type) re-read 876 open work beads (3.44 MB) per replica."
```

`gofumpt -l` must print nothing. Do not run the browser tests (`*_browser_test.go`) on the laptop.

**OPERATOR, after the release carrying this:**

```bash
export KUBECONFIG=$HOME/ws/scratch/ssm.kubeconfig
(timeout 60 kubectl -n gardenio port-forward svc/gardenio-vmsingle 18428:8428 >/dev/null 2>&1 &); sleep 5
for m in receive transmit; do
  curl -s --get localhost:18428/api/v1/query --data-urlencode "query=sum by (wl) (label_replace(increase(process_network_${m}_bytes_total{job=\"kubernetes-pods\"}[24h]), \"wl\", \"\$1\", \"pod\", \"(.*)-[a-z0-9]{8,10}-[a-z0-9]{5}\"))" \
  | python3 -c 'import json,sys; [print("%8.1f GB  %s"%(float(r["value"][1])/1e9, r["metric"].get("wl"))) for r in sorted(json.load(sys.stdin)["data"]["result"], key=lambda r:-float(r["value"][1]))[:6]]'
done
```

Run it 24 h after the roll. Pass: `gardenio-beads-viewer` receive is under 10 GB per 24 h (baseline 102.4). Also open `https://gardenio.clonk.tv/garden/farm` and check that a task status change shows within about 2 s (the work-type path) and that gardeners still show as live (the roster path, at most 60 s old).

**Rollback:** revert the PR and cut a release.

---

### Task B3: kbeads counts response bytes per caller and route

**Who:** CREW (Go PR). No operator step beyond reading the metric (Task B5).

**Why:**
- The daemon logs only requests over 1 s (`kbeads/internal/server/http.go:770-776`).
- Its only per-caller metric counts requests by auth type (`gardenio_beads_requests_by_auth_type_total`), not bytes.
- Machine reads run at 27.8 req/s. The receive totals put trigger-engine at 58.6 GB/day and the controller at 35.4 GB/day, but which routes carry that is unknown.

**Savings:** none directly. It names the next cut, with numbers.

**Files:**
- Create: `kbeads/internal/server/http_bytes_metrics.go`
- Create: `kbeads/internal/server/http_bytes_metrics_test.go`
- Modify: `kbeads/internal/server/http.go`
  - `statusRecorder` `:649-664`
  - `loggingMiddleware` `:737-776`
  - the chain root `var downstream http.Handler = mux` at `:385`
- Modify: `kbeads/internal/server/http_metrics.go:334-346` (register the new counter next to `requestsByAuthTypeTotal`)

**Interfaces:**
- Produces: Prometheus counter `gardenio_beads_http_response_bytes_total{caller,route}`.
  - `caller` is the `machine:<name>` subject for machine principals; otherwise one of `static`, `fleet`, `oidc`, `edge`, `other`.
  - `route` is the ServeMux pattern, for example `GET /v1/beads`, or `unmatched`.
- Produces: an access-log attribute `bytes`.

- [ ] **Step 1: Failing test**

Create `http_bytes_metrics_test.go`:

```go
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestResponseBytesByCallerAndRoute(t *testing.T) {
	fc := NewFleetConfig("gardenio", nil)
	fc.RegisterTokenFull("agent-tok", "agent-gd-a1", "gardenio", nil, nil, time.Time{}, "crew-a", false)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/beads", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 1234)))
	})
	h := AuthMiddleware("secret", fc, nil, loggingMiddleware(nil, requestTimeoutMiddleware(captureRoute(mux))))
	responseBytesTotal.Reset()

	for _, path := range []string{"/v1/beads", "/v1/nope"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer agent-tok")
		req.Header.Set("X-Operated-By", "slack:U0OPERATOR1")
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	if got := testutil.ToFloat64(responseBytesTotal.WithLabelValues("fleet", "GET /v1/beads")); got != 1234 {
		t.Errorf("fleet GET /v1/beads bytes = %v, want 1234", got)
	}
	if got := testutil.ToFloat64(responseBytesTotal.WithLabelValues("fleet", "unmatched")); got == 0 {
		t.Errorf("unmatched route recorded no bytes (the 404 body)")
	}
}

func TestResponseCaller(t *testing.T) {
	for _, c := range []struct {
		id   AuthIdentity
		want string
	}{
		{AuthIdentity{Type: "machine", Subject: "machine:gardenio-trigger-engine"}, "machine:gardenio-trigger-engine"},
		{AuthIdentity{Type: "machine", Subject: "weird"}, "machine"},
		{AuthIdentity{Type: "oidc", Subject: "someone@example.com"}, "oidc"},
		{AuthIdentity{Type: "fleet", Subject: "agent-x"}, "fleet"},
		{AuthIdentity{}, "other"},
	} {
		if got := responseCaller(c.id); got != c.want {
			t.Errorf("responseCaller(%+v) = %q, want %q", c.id, got, c.want)
		}
	}
}
```

If the `AuthIdentity` field names differ, adapt the literals. Check with `grep -n 'type AuthIdentity' -A8 kbeads/internal/server/*.go`.

Run: `cd kbeads && go test ./internal/server/ -run 'ResponseBytes|ResponseCaller'`
Expected: FAIL to compile (`undefined: responseBytesTotal`, `captureRoute`, `responseCaller`).

- [ ] **Step 2: Implement**

Create `http_bytes_metrics.go`:

```go
package server

import (
	"context"
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// responseBytesTotal sizes the read traffic per caller (cost plan
// 2026-10-21-aws-cost-cuts Task B3). caller is bounded: the configured
// machine principals plus the five auth types. route is the ServeMux pattern.
// SSE streams add their bytes when the stream ends.
var responseBytesTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "gardenio_beads_http_response_bytes_total",
		Help: "HTTP response body bytes by caller (machine:<name> for machine principals, else static|fleet|oidc|edge|other) and route (ServeMux pattern, or unmatched).",
	},
	[]string{"caller", "route"},
)

func responseCaller(id AuthIdentity) string {
	t := normalizeRequestAuthType(id.Type)
	if t == "machine" && strings.HasPrefix(id.Subject, "machine:") {
		return id.Subject
	}
	return t
}

type routeCaptureKey struct{}

type routeCapture struct{ pattern string }

// captureRoute wraps the ServeMux. ServeMux sets r.Pattern on the request it is
// handed, but loggingMiddleware sits above handlers that copy the request
// (WithContext), so the pattern is passed back up through the context.
func captureRoute(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
		if rc, ok := r.Context().Value(routeCaptureKey{}).(*routeCapture); ok {
			rc.pattern = r.Pattern
		}
	})
}

func withRouteCapture(ctx context.Context) (context.Context, *routeCapture) {
	rc := &routeCapture{}
	return context.WithValue(ctx, routeCaptureKey{}, rc), rc
}
```

In `http.go`:
- Give `statusRecorder` a `bytes int64` field and this method:

```go
func (r *statusRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}
```

- In `loggingMiddleware`, replace `ctx := context.WithValue(r.Context(), errorCaptureKey, cap)` with:

```go
		ctx := context.WithValue(r.Context(), errorCaptureKey, cap)
		ctx, rc := withRouteCapture(ctx)
```

- After `duration := time.Since(start)`, add:

```go
		route := rc.pattern
		if route == "" {
			route = "unmatched"
		}
		responseBytesTotal.WithLabelValues(responseCaller(AuthIdentityFromContext(r.Context())), route).Add(float64(rec.bytes))
```

- Append `slog.Int64("bytes", rec.bytes)` to `attrs`.
- Change `var downstream http.Handler = mux` to `var downstream http.Handler = captureRoute(mux)`.

In `http_metrics.go`, add `responseBytesTotal,` to the `reg.MustRegister(` list after `requestsByAuthTypeTotal,`.

- [ ] **Step 3: Tests, lint, commit**

```bash
cd kbeads && go test ./internal/server/ && gofumpt -l internal/server && go vet ./internal/server/ && staticcheck ./internal/server/ && cd - && \
git add kbeads/internal/server && git commit -m "kbeads: count HTTP response bytes per caller and route (cost plan B3)"
```

`go test ./internal/server/` covers the existing `TestRequestsByAuthType_HTTP` and the SSE tests; they must stay green with the new `Write` method. `http.Flusher` is still delegated by `statusRecorder.Flush`.

**Rollback:** revert the PR.

---

### Task B4: Nodes carry their zone; the daemon prefers Postgres's zone and its heavy clients prefer the daemon's

**Who:** CREW (two PRs: the k3s boot-script line, then deploy-values as DRAFT with `merge-desk:hold`). OPERATOR labels existing nodes and merges in a window.

**Why:**
- The daemon is rescheduled on most rolls (9 ReplicaSets in 32 h).
- Its clients spread over both AZs: the controller and coopmux are in 1a now, the viewers and trigger-engine in 1b.
- Every byte between them that crosses an AZ costs $0.02/GB.
- Nodes have no `topology.kubernetes.io/zone` label (the AWS CCM runs node-lifecycle only), so zone affinity does nothing today.

**Savings:** the remaining daemon-client traffic (about 100 GB/day after B2) stays in one AZ: about $1/day at 50% cross-AZ.

**Files:**
- PR 1:
  - Modify `gardenio/terraform/modules/k3s/templates/node.sh.tftpl:123` (the `COMMON=` line).
  - Modify `gardenio/terraform/modules/k3s/tests/edge_flags.tftest.hcl` (add an assertion; see Step 1).
- PR 2:
  - Modify `deploy-values/gardenio-k3s.yaml`, sections `beads:` (`:2021`), `beadsViewer:` (`:2049`, keep the existing `nodeAffinity` at `:2174-2181` and the replica spread), `triggerEngine:` (`:2208`) and `agents:` (`:1842`, the controller).

- [ ] **Step 1: Boot script adds the zone label (PR 1)**

Add a `run` to `edge_flags.tftest.hcl` asserting that the rendered agent launch template user data carries the label:

```hcl
run "nodes_label_their_zone" {
  command = plan
  assert {
    condition     = strcontains(base64decode(aws_launch_template.agent.user_data), "--node-label topology.kubernetes.io/zone=$AZ")
    error_message = "every node must register with its zone label (cost plan B4)"
  }
}
```

If Task C3 has already switched the agent template to `base64gzip`, assert on the rendered template instead: `strcontains(templatefile("${path.module}/templates/node.sh.tftpl", merge(local.userdata_vars, { role = "agent", extra_args = "" })), "...")`.

CI must show it failing first. Then, in `node.sh.tftpl`, append to the `COMMON=` string, right after `--kubelet-arg=provider-id=aws:///$AZ/$INSTANCE_ID`:

```bash
 --node-label topology.kubernetes.io/zone=$AZ
```

(kubelet's NodeRestriction allows a node to set its own `topology.kubernetes.io/zone`; `$AZ` is already set in the script.) This adds 45 bytes to plain-base64 user_data, inside the roughly 800 bytes left. CI's `terraform validate` plus the test must pass. Commit: `k3s: nodes register their zone label (cost plan B4)`.

**OPERATOR, after PR 1 merges** (new nodes get the label at boot; existing nodes need it now):

```bash
export KUBECONFIG=$HOME/ws/scratch/ssm.kubeconfig
kubectl get nodes -o json | python3 -c '
import json,sys
for n in json.load(sys.stdin)["items"]:
    pid=n["spec"].get("providerID","")   # aws:///us-east-1b/i-...
    parts=pid.split("/")
    if len(parts)>=5 and parts[3].startswith("us-east-1"): print(n["metadata"]["name"], parts[3])
' | while read -r node az; do kubectl label node "$node" topology.kubernetes.io/zone="$az" --overwrite; done
kubectl get nodes -L topology.kubernetes.io/zone
```

Pass: every node shows a zone. There is no Terraform apply here: the launch template change reaches nodes as they are replaced. If the k3s module is applied for another reason, `-target=module.k3s.aws_launch_template.agent -target=module.k3s.aws_launch_template.ci -target=module.k3s.aws_launch_template.server` picks it up with no instance replacement.

- [ ] **Step 2: Same-zone preferences (PR 2, after the labels exist)**

In `deploy-values/gardenio-k3s.yaml`, under `beads:` add:

```yaml
  # Same zone as postgres (pinned to its local-PV node): every daemon read
  # crossed an AZ whenever a roll put the daemon in the other zone (cost plan
  # 2026-10-21 B4). Preferred, never required.
  affinity:
    podAffinity:
      preferredDuringSchedulingIgnoredDuringExecution:
        - weight: 100
          podAffinityTerm:
            topologyKey: topology.kubernetes.io/zone
            labelSelector:
              matchLabels: {app.kubernetes.io/component: postgres, app.kubernetes.io/instance: gardenio}
```

Under `beadsViewer:`, an explicit `affinity` replaces the chart's computed one (`templates/beads-viewer/deployment.yaml:664-686`). So carry the existing spot preference and the replica spread into it, and delete the now-unused `nodeAffinity:` key from this section:

```yaml
  affinity:
    nodeAffinity:
      preferredDuringSchedulingIgnoredDuringExecution:
        - weight: 100
          preference:
            matchExpressions:
              - key: gardenio.io/lifecycle
                operator: In
                values: [spot]
    podAffinity:
      preferredDuringSchedulingIgnoredDuringExecution:
        - weight: 80
          podAffinityTerm:
            topologyKey: topology.kubernetes.io/zone
            labelSelector:
              matchLabels: {app.kubernetes.io/component: beads, app.kubernetes.io/instance: gardenio}
    podAntiAffinity:
      preferredDuringSchedulingIgnoredDuringExecution:
        - weight: 100
          podAffinityTerm:
            topologyKey: kubernetes.io/hostname
            labelSelector:
              matchLabels: {app.kubernetes.io/name: gardenio, app.kubernetes.io/instance: gardenio, app.kubernetes.io/component: beads-viewer}
```

Under `triggerEngine:` and under `agents:` (the controller), add the same `podAffinity` block as the viewer's (weight 80, zone, component `beads`). If either section already sets `affinity`, merge into it rather than replacing it. Check with `grep -n -A3 '^triggerEngine:\|^agents:' deploy-values/gardenio-k3s.yaml` and the template's default (`templates/controller/deployment.yaml:1573-1586` renders a hostname anti-affinity only when `agents.replicaCount > 1`).

Render check:

```bash
helm template gardenio gardenio/helm/gardenio -f deploy-values/gardenio-k3s.yaml | python3 -c '
import sys
docs=sys.stdin.read().split("\n---")
for name in ("gardenio-beads","gardenio-beads-viewer","gardenio-trigger-engine","gardenio-agents"):
    d=[x for x in docs if "kind: Deployment" in x and f"name: {name}\n" in x]
    print(name, "zone-affinity" if d and "topology.kubernetes.io/zone" in d[0] else "MISSING")'
```

Expected: four lines ending `zone-affinity`. Chart tests (`cd gardenio/helm/gardenio/tests && go test ./...`) pass. Commit, then open the PR as DRAFT with `merge-desk:hold`.

**OPERATOR window:** merge, roll, then:

```bash
export KUBECONFIG=$HOME/ws/scratch/ssm.kubeconfig
for c in postgres beads beads-viewer trigger-engine; do kubectl -n gardenio get pods -l app.kubernetes.io/component=$c -o wide --no-headers | awk '{print $1,$7}'; done
kubectl -n gardenio get pods -l app.kubernetes.io/name=gardenio -o wide --no-headers | grep 'gardenio-agents-' | awk '{print $1,$7}'
kubectl get nodes -L topology.kubernetes.io/zone --no-headers | awk '{print $1,$NF}'
```

Pass: postgres, beads, both viewers, trigger-engine and the controller are on nodes in the same zone. If capacity forced one elsewhere, report it; preferred affinity does not evict.

**Rollback:** revert either PR (labels are harmless to leave).

---

### Task B5: Measure Phase B, and report the next cut

**Who:** OPERATOR. **When:** 24 h after B1-B4 are all live.

- [ ] **Step 1: Table counters and rates.** Run twice, 120 s apart, and diff (same query as the baseline):

```bash
export KUBECONFIG=$HOME/ws/scratch/ssm.kubeconfig
Q="select relname, seq_scan, seq_tup_read, coalesce(idx_scan,0), coalesce(idx_tup_fetch,0) from pg_stat_user_tables where relname in ('beads','deps','labels','operators') order by 1"
kubectl -n gardenio exec gardenio-postgres-0 -c postgres -- psql -U postgres -d beads -AtF' ' -c "$Q" > /tmp/b5-1.txt; sleep 120
kubectl -n gardenio exec gardenio-postgres-0 -c postgres -- psql -U postgres -d beads -AtF' ' -c "$Q" > /tmp/b5-2.txt
paste -d' ' /tmp/b5-1.txt /tmp/b5-2.txt | awk '{printf "%-10s seq_scan/s=%7.1f seq_tup_read/s=%8.0f idx_scan/s=%7.1f idx_tup_fetch/s=%7.0f\n",$1,($7-$2)/120,($8-$3)/120,($9-$4)/120,($10-$5)/120}'
```

Compare with the baseline rates: beads 9,213 idx_tup_fetch/s; deps 1,216 idx_scan/s; operators 1,652 seq_scan/s; labels 1,226 idx_scan/s.

- [ ] **Step 2: Bytes per client.** Run the VictoriaMetrics 24 h receive query from Task B2. Compare with 102.4 / 58.6 / 35.4 / 2.6 GB.
- [ ] **Step 3: Postgres node egress.**

```bash
aws cloudwatch get-metric-statistics --profile gardenio --region us-east-1 --namespace AWS/EC2 --metric-name NetworkOut \
  --dimensions Name=InstanceId,Value=$(aws ec2 describe-instances --profile gardenio --region us-east-1 --filters Name=private-ip-address,Values=10.90.30.213 --query 'Reservations[0].Instances[0].InstanceId' --output text) \
  --start-time $(date -u -d '-6 hours' +%FT%TZ) --end-time $(date -u +%FT%TZ) --period 3600 --statistics Sum --query 'sort_by(Datapoints,&Timestamp)[].Sum' --output text
```

Baseline: 23.9-26.1 GB/h. Expect most hours under 8 GB. The backup no longer leaves the node.

- [ ] **Step 4: Cross-AZ dollars.** Run Cost Explorer for `DataTransfer-Regional-Bytes` daily over the 7 days after (the command from Task A1 with that usage type). Baseline 10-20: 425.9 GB / $4.26.
- [ ] **Step 5: Next cut.** Read `gardenio_beads_http_response_bytes_total` (Task B3):

```bash
(timeout 60 kubectl -n gardenio port-forward svc/gardenio-vmsingle 18428:8428 >/dev/null 2>&1 &); sleep 5
curl -s --get localhost:18428/api/v1/query --data-urlencode 'query=topk(10, sum by (caller,route) (increase(gardenio_beads_http_response_bytes_total[24h])))' \
 | python3 -c 'import json,sys; [print("%8.2f GB  %s  %s"%(float(r["value"][1])/1e9, r["metric"]["caller"], r["metric"]["route"])) for r in sorted(json.load(sys.stdin)["data"]["result"], key=lambda r:-float(r["value"][1]))]'
```

Post the results of Steps 1-5 as one comment on the plan's B phase. Name the top three (caller, route) pairs with GB/day as the input for a follow-up plan. Expected candidates from reading the code:
- trigger-engine `Evaluate` (every 30 s) listing all open trigger beads several times per cycle; trigger beads average 10.3 KB, mostly `fields`.
- the controller's pool manager, metrics collector and reconciler re-listing agents every 30-60 s.
- the per-actor N+1 in `GET /v1/agents/roster` (`kbeads/internal/server/http_roster.go:62`).

---

## Phase C: public IPv4 only on the edge, the NAT and the CI pool

### Task C1: Measure non-CI internet bytes and choose the NAT size and topology

**Who:** OPERATOR. **Cost:** about $0.05 of flow-log ingestion, deleted afterwards.

- [ ] **Step 1: One hour of flow logs on the three old public subnets**

```bash
P="--profile gardenio --region us-east-1"
aws logs create-log-group $P --log-group-name /gardenio/costplan-c1-flowlogs
aws logs put-retention-policy $P --log-group-name /gardenio/costplan-c1-flowlogs --retention-in-days 1
ROLE=$(aws iam get-role $P --role-name gardenio-costplan-flowlogs --query Role.Arn --output text 2>/dev/null) || {
  aws iam create-role $P --role-name gardenio-costplan-flowlogs --assume-role-policy-document '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"vpc-flow-logs.amazonaws.com"},"Action":"sts:AssumeRole"}]}' >/dev/null
  aws iam put-role-policy $P --role-name gardenio-costplan-flowlogs --policy-name logs --policy-document '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["logs:CreateLogStream","logs:PutLogEvents","logs:DescribeLogStreams"],"Resource":"*"}]}'
  sleep 10; ROLE=$(aws iam get-role $P --role-name gardenio-costplan-flowlogs --query Role.Arn --output text); }
aws ec2 create-flow-logs $P --resource-type Subnet --resource-ids subnet-0123456789abcdef0 subnet-0123456789abcdef1 subnet-0123456789abcdef2 \
  --traffic-type ALL --log-destination-type cloud-watch-logs --log-group-name /gardenio/costplan-c1-flowlogs --deliver-logs-permission-arn "$ROLE" \
  --max-aggregation-interval 60 --log-format '${interface-id} ${srcaddr} ${dstaddr} ${bytes} ${pkt-srcaddr} ${pkt-dstaddr}' --query FlowLogIds --output text
```

Write down the flow-log id. **Rollback / cleanup** (run after Step 2, even if Step 2 fails): `aws ec2 delete-flow-logs $P --flow-log-ids <id>; aws logs delete-log-group $P --log-group-name /gardenio/costplan-c1-flowlogs; aws iam delete-role-policy $P --role-name gardenio-costplan-flowlogs --policy-name logs; aws iam delete-role $P --role-name gardenio-costplan-flowlogs`.

- [ ] **Step 2: After 60 minutes, sum internet bytes per node, excluding CI nodes**

```bash
CI_IPS=$(aws ec2 describe-instances $P --filters Name=tag:aws:autoscaling:groupName,Values=gardenio-k3s-ci Name=instance-state-name,Values=running --query 'Reservations[].Instances[].PrivateIpAddress' --output text | tr '\t' '|')
QID=$(aws logs start-query $P --log-group-name /gardenio/costplan-c1-flowlogs --start-time $(date -u -d '-65 min' +%s) --end-time $(date -u +%s) \
  --query-string 'parse @message "* * * * * *" as eni, src, dst, bytes, psrc, pdst
| filter not (isIpv4InSubnet(src,"10.90.0.0/16") and isIpv4InSubnet(dst,"10.90.0.0/16"))
| fields if(isIpv4InSubnet(src,"10.90.0.0/16"), src, dst) as node
| stats sum(bytes)/1e9 as gb by node | sort gb desc' --query queryId --output text)
sleep 30; aws logs get-query-results $P --query-id "$QID" --query 'results[*][*].value' --output text | grep -vE "^(${CI_IPS:-none})\b"
```

Map each private IP to its pool with `aws ec2 describe-instances $P --filters Name=private-ip-address,Values=<ip> --query 'Reservations[].Instances[].Tags[?Key==`Name`].Value' --output text`. Each node's subnet AZ: 10.90.0-15.x is 1a, 10.90.16-31.x is 1b, 10.90.96-111.x is 1c.

- [ ] **Step 3: Decide, and record the decision on the plan's C phase**

Let `H` be the non-CI internet GB in that hour (servers, agents, spot and edge, excluding the edge's EIP traffic, which stays on the IGW), and `B` the share from 1b and 1c nodes.

- Size: if `H` < 10, use `nat_instance_type = "t4g.micro"` ($0.0084/h, 64 Mbps baseline, about 690 GB/day sustained, burst to 5 Gbps; unlimited CPU credits are already the module default). If `H` is 10-25, use `t4g.small`. If `H` > 25, STOP: NAT throughput would cost more than the IPv4 it saves. Report back.
- Topology: single box (`nat_instance_per_az = false`) unless `B x 24 x $0.02` > $0.32/day, the cost of a second box with its IPv4. That is, use per-AZ boxes only if 1b/1c internet traffic is over about 0.7 GB/h.
- Record `H`, `B` and the choice. Tasks C2 and C6 use them.

---

### Task C2: Network module: edge subnets, NAT box placement, and a per-AZ route flip toggle

**Who:** CREW (merge-desk PR). Defaults change nothing live.

**Why:**
- The NAT box is placed in `aws_subnet.public[count.index]` (`modules/network/nat_instance.tf:313`). Once the public subnets route through the NAT, the box would route through itself.
- The public subnets are associated only with the IGW table (`main.tf:160-164`, `:183-187`).
- The NAT IAM has no SSM, and the NAT ASG enables no group metrics, so the "no in-service box" alarm (`nat_instance.tf:348-369`, `treat_missing_data = "breaching"`) would sit in ALARM permanently.
- The module tests (`modules/network/tests/nat_mode.tftest.hcl`) are not run in CI.

**Files:**
- Modify: `gardenio/terraform/modules/network/variables.tf`
- Modify: `gardenio/terraform/modules/network/main.tf:76-88` (`aws_subnet.public`), `:160-187` (associations), plus a new `aws_subnet.edge`
- Modify: `gardenio/terraform/modules/network/nat_instance.tf:306-343` (ASG subnet, `enabled_metrics`), plus an SSM policy attachment
- Modify: `gardenio/terraform/modules/network/outputs.tf`
- Modify: `gardenio/terraform/modules/network/tests/nat_mode.tftest.hcl`
- Modify: `.github/workflows/checks.yml` (terraform job, after `k3s module tests`)

**Interfaces:**
- Produces variables:
  - `edge_subnets` (map(string), AZ to CIDR, default `{}`)
  - `nat_routed_public_azs` (set(string), default `[]`)
- Produces output `edge_subnet_ids` (map AZ to subnet id).
- When `edge_subnets` is non-empty, the NAT box goes in `edge_subnets`' first AZ (per-AZ mode: the edge subnet of that AZ).

- [ ] **Step 1: Failing module tests.** Append to `nat_mode.tftest.hcl`:

```hcl
run "edge_subnets_host_the_nat_box" {
  command = plan
  variables {
    nat_mode     = "instance"
    edge_subnets = { "us-east-1a" = "10.0.160.0/24", "us-east-1b" = "10.0.161.0/24" }
  }
  assert {
    condition     = length(aws_subnet.edge) == 2 && alltrue([for s in aws_subnet.edge : s.map_public_ip_on_launch])
    error_message = "two public edge subnets"
  }
  assert {
    condition     = contains(aws_autoscaling_group.nat_instance[0].vpc_zone_identifier, aws_subnet.edge["us-east-1a"].id)
    error_message = "the NAT box must live in an edge subnet, never a subnet it routes"
  }
  assert {
    condition     = contains(aws_autoscaling_group.nat_instance[0].enabled_metrics, "GroupInServiceInstances")
    error_message = "the unhealthy alarm needs GroupInServiceInstances"
  }
}

run "flipping_one_az_routes_its_public_subnet_through_the_nat" {
  command = plan
  variables {
    nat_mode              = "instance"
    edge_subnets          = { "us-east-1a" = "10.0.160.0/24" }
    nat_routed_public_azs = ["us-east-1b"]
  }
  assert {
    condition     = aws_route_table_association.public[1].route_table_id == aws_route_table.private_app[1].id
    error_message = "the flipped AZ's public subnet must use its private-app route table"
  }
  assert {
    condition     = aws_route_table_association.public[0].route_table_id == aws_route_table.public.id
    error_message = "an unflipped AZ must keep the IGW route table"
  }
  assert {
    condition     = !aws_subnet.public[1].map_public_ip_on_launch && aws_subnet.public[0].map_public_ip_on_launch
    error_message = "a flipped subnet stops handing out public IPs"
  }
}

run "defaults_change_nothing" {
  command = plan
  variables {
    nat_mode = "none"
  }
  assert {
    condition     = length(aws_subnet.edge) == 0 && alltrue([for a in aws_route_table_association.public : a.route_table_id == aws_route_table.public.id])
    error_message = "no edge subnets and no flipped associations by default"
  }
}
```

In `checks.yml`, after the `k3s module tests (edge / ziti flag split)` step, add:

```yaml
      - name: network module tests (NAT placement, route flip)
        run: cd gardenio/terraform/modules/network && terraform init -backend=false -input=false >/dev/null && terraform test
```

Push. Expected in CI: the new runs fail (`edge_subnets` is not declared).

- [ ] **Step 2: Implement.** In `variables.tf`:

```hcl
variable "edge_subnets" {
  type        = map(string)
  default     = {}
  description = "AZ -> CIDR of small public subnets (IGW route) for what must keep a public IP: the NAT box, the public-edge node, the CI pool (cost plan 2026-10-21 C2). The NAT box is placed here when set."
}

variable "nat_routed_public_azs" {
  type        = set(string)
  default     = []
  description = "AZs whose PUBLIC subnet (public or public_extra) is re-associated to a private-app route table, i.e. egresses through the NAT box. Flip one AZ at a time. An AZ with no private-app table (us-east-1c here) uses private_app[0]."
}
```

In `main.tf`:
- In `aws_subnet.public`, set `map_public_ip_on_launch = !contains(var.nat_routed_public_azs, local.azs[count.index])`.
- In `aws_subnet.public_extra`, set `map_public_ip_on_launch = !contains(var.nat_routed_public_azs, each.key)`.
- Change the two association resources and add the edge subnets:

```hcl
resource "aws_route_table_association" "public" {
  count     = local.az_count
  subnet_id = aws_subnet.public[count.index].id
  route_table_id = (
    contains(var.nat_routed_public_azs, local.azs[count.index])
    ? aws_route_table.private_app[count.index].id
    : aws_route_table.public.id
  )
}

resource "aws_route_table_association" "public_extra" {
  for_each  = aws_subnet.public_extra
  subnet_id = each.value.id
  route_table_id = (
    contains(var.nat_routed_public_azs, each.key)
    ? aws_route_table.private_app[0].id
    : aws_route_table.public.id
  )
}

resource "aws_subnet" "edge" {
  for_each                = var.edge_subnets
  vpc_id                  = aws_vpc.this.id
  cidr_block              = each.value
  availability_zone       = each.key
  map_public_ip_on_launch = true
  tags = merge(local.module_tags, {
    Name                   = "${var.env_name}-edge-${each.key}"
    "gardenio:subnet-tier" = "edge"
  })
}

resource "aws_route_table_association" "edge" {
  for_each       = aws_subnet.edge
  subnet_id      = each.value.id
  route_table_id = aws_route_table.public.id
}
```

(`aws_route_table_association.route_table_id` updates in place through ReplaceRouteTableAssociation. The plan must show `~ update in-place`, never a replace.)

In `nat_instance.tf`, in `aws_autoscaling_group.nat_instance`:
- Change `vpc_zone_identifier` to:

```hcl
  vpc_zone_identifier = [
    length(var.edge_subnets) == 0 ? aws_subnet.public[count.index].id
    : (var.nat_instance_per_az ? aws_subnet.edge[local.azs[count.index]].id : aws_subnet.edge[sort(keys(var.edge_subnets))[0]].id)
  ]
```

- Add `metrics_granularity = "1Minute"` and `enabled_metrics = ["GroupInServiceInstances"]`.
- Add, after the instance profile:

```hcl
resource "aws_iam_role_policy_attachment" "nat_instance_ssm" {
  count      = local.nat_instance_count > 0 ? 1 : 0
  role       = aws_iam_role.nat_instance[0].name
  policy_arn = "arn:${data.aws_partition.current.partition}:iam::aws:policy/AmazonSSMManagedInstanceCore"
}
```

Use the actual role resource name from the file: `grep -n 'resource "aws_iam_role"' nat_instance.tf`.

In `outputs.tf`: `output "edge_subnet_ids" { value = { for k, s in aws_subnet.edge : k => s.id } }`.

- [ ] **Step 3: CI green** (`terraform fmt -check`, validate, and the new network module tests, with the three new runs passing and the old ones unchanged). Commit: `terraform(network): edge subnets, NAT box placement, per-AZ route flip (cost plan C2)`.

**Rollback:** revert. With defaults, nothing live changes.

---

### Task C3: k3s module: per-pool subnets and public-IP toggles, a boot script that works without a public IP, gzipped agent and CI user data

**Who:** CREW (merge-desk PR). Defaults change nothing live.

**Why:** with no public IP:
- `PUBLIC_IP=$(imds meta-data/public-ipv4)` is empty (`node.sh.tftpl:16`).
- Then `COMMON="--node-external-ip $PUBLIC_IP ..."` (`:123`) passes a bare flag.
- `SERVER_FLAGS` (`:225`) gets `--tls-san` with no value.
- `k3s-dns add "$PUBLIC_IP"` (`:244`) and the API-record upsert (`:41`, which keeps the old list when the new one is empty, `:37-38`) leave stale public IPs in `k3s.clonk.tv`.

Also:
- The spot pool, the CI pool and the edge are hard-wired to `var.subnet_ids` (`main.tf:548`, `:653-735`, `:810`).
- The agent and CI templates are plain base64 within about 800 bytes of the 16 KB limit (`:389`, `:621`).

**Files:**
- Modify: `gardenio/terraform/modules/k3s/templates/node.sh.tftpl` (`:16`, `:37-48`, `:58`, `:123`, `:225`, `:244`, `:345`)
- Modify: `gardenio/terraform/modules/k3s/variables.tf`
- Modify: `gardenio/terraform/modules/k3s/main.tf`
  - LT `server` `:342-375`
  - LT `agent` `:386-415`
  - new LT `agent_private`
  - ASG `agents_spot` `:545-606` (subnets `:548`, launch template `:578`)
  - LT `ci` `:618-651`
  - ASG `ci` (subnets)
  - edge ENI `:808-814`
- Create: `gardenio/terraform/modules/k3s/tests/no_public_ip.tftest.hcl`

**Interfaces:** variables, all defaulting to today's behaviour:
- `ci_subnet_ids` (list, default `[]`, meaning `subnet_ids`)
- `agent_spot_subnet_ids` (list, default `[]`, meaning `subnet_ids` and the public LT)
- `edge_subnet_id` (string, default `""`, meaning `subnet_ids[0]`)
- `server_public_ip` (bool, default `true`)
- `agent_public_ip` (bool, default `true`)

Consumed by Tasks C7-C11 through `composition/gardenio/main.tf` (`module "k3s"`).

- [ ] **Step 1: Failing tests.** Create `tests/no_public_ip.tftest.hcl`, with the same `mock_provider` and `variables` header as `tests/ecr_read.tftest.hcl` (Task A2; copy it verbatim) and these runs:

```hcl
run "defaults_keep_todays_wiring" {
  command = plan
  assert {
    condition = (
      aws_launch_template.server.network_interfaces[0].associate_public_ip_address == "true" &&
      aws_launch_template.agent.network_interfaces[0].associate_public_ip_address == "true" &&
      tolist(aws_autoscaling_group.ci.vpc_zone_identifier) == tolist(["subnet-0a", "subnet-0b"]) &&
      tolist(aws_autoscaling_group.agents_spot.vpc_zone_identifier) == tolist(["subnet-0a", "subnet-0b"]) &&
      length(aws_launch_template.agent_private) == 0
    )
    error_message = "defaults must not change any pool"
  }
}

run "pools_move_and_lose_public_ips" {
  command = plan
  variables {
    ci_subnet_ids         = ["subnet-edge-a", "subnet-edge-b"]
    agent_spot_subnet_ids = ["subnet-priv-a", "subnet-priv-b"]
    edge_subnet_id        = "subnet-edge-a"
    server_public_ip      = false
    agent_public_ip       = false
  }
  assert {
    condition = (
      aws_launch_template.server.network_interfaces[0].associate_public_ip_address == "false" &&
      aws_launch_template.agent.network_interfaces[0].associate_public_ip_address == "false" &&
      aws_launch_template.agent_private[0].network_interfaces[0].associate_public_ip_address == "false" &&
      aws_launch_template.ci.network_interfaces[0].associate_public_ip_address == "true"
    )
    error_message = "servers and agents lose public IPs; CI keeps them"
  }
  assert {
    condition = (
      tolist(aws_autoscaling_group.ci.vpc_zone_identifier) == tolist(["subnet-edge-a", "subnet-edge-b"]) &&
      tolist(aws_autoscaling_group.agents_spot.vpc_zone_identifier) == tolist(["subnet-priv-a", "subnet-priv-b"]) &&
      aws_network_interface.ziti_edge[0].subnet_id == "subnet-edge-a"
    )
    error_message = "CI, spot and edge go to their own subnets"
  }
}

run "boot_script_handles_no_public_ip" {
  command = plan
  assert {
    condition = alltrue([
      for s in ["$${PUBLIC_IP:+--node-external-ip $PUBLIC_IP}", "$${PUBLIC_IP:+--tls-san $PUBLIC_IP}", "k3s-dns add \"$${PUBLIC_IP:-$PRIVATE_IP}\""] :
      strcontains(templatefile("${path.module}/templates/node.sh.tftpl", merge(local.userdata_vars, { role = "server", extra_args = "" })), s)
    ])
    error_message = "the boot script must not pass empty public-IP flags or publish an empty public IP"
  }
  assert {
    condition     = length(aws_launch_template.agent.user_data) < 8000 && length(aws_launch_template.ci.user_data) < 8000
    error_message = "agent/ci user_data must be gzipped (16 KB EC2 limit)"
  }
  assert {
    condition     = length(templatefile("${path.module}/templates/node.sh.tftpl", merge(local.userdata_vars, { role = "agent", extra_args = local.ziti_edge_extra_args }))) < 16384
    error_message = "the edge instance's raw user_data must stay under 16 KB"
  }
}
```

Push. Expected in CI: fail (undeclared variables, missing `agent_private`, strings not found).

- [ ] **Step 2: Boot script.** In `node.sh.tftpl`:
  - `:123`: replace `--node-external-ip $PUBLIC_IP` with `$${PUBLIC_IP:+--node-external-ip $PUBLIC_IP}`. In a `.tftpl`, `$${` renders a literal `${`. Check how the file escapes other shell `${...}` uses (`grep -n '\$\${' node.sh.tftpl | head`) and match it.
  - `:225`: replace `--tls-san $PUBLIC_IP` with `$${PUBLIC_IP:+--tls-san $PUBLIC_IP}`.
  - `:244`: change `k3s-dns add "$PUBLIC_IP" "$PRIVATE_IP"` to `k3s-dns add "$${PUBLIC_IP:-$PRIVATE_IP}" "$PRIVATE_IP"`.
  - `:58` area (`echo "$PUBLIC_IP" > /run/k3s-public-ip`): change to `echo "$${PUBLIC_IP:-$PRIVATE_IP}" > /run/k3s-public-ip`.
  - In the `k3s-dns` heredoc (`:42-44`), make the API record's live list fall back to the private IP when a server has no public IP. Replace `upsert "$API"  "$(echo "$inst" | awk '{print $1}' | grep -v None || true)" "$PUB"` with:

```bash
upsert "$API"  "$(echo "$inst" | awk '{print ($1=="None")?$2:$1}' || true)" "$PUB"
```

  - Add a comment above it: `# A server with no public IP (cost plan C3) publishes its private IP: the API name is then reachable only in the VPC and via the SSM relay, and never names an address AWS may give to someone else.`
  - `:345` (`echo "bootstrap complete: role=$ROLE ip=$PUBLIC_IP"`): use `$${PUBLIC_IP:-$PRIVATE_IP}`.

- [ ] **Step 3: Variables and resources.** In `variables.tf`:

```hcl
variable "ci_subnet_ids" {
  type        = list(string)
  default     = []
  description = "Subnets for the CI pool; empty uses subnet_ids. CI keeps public IPs (3.1 TB/day inbound on 2026-10-20 would swamp a NAT box), so these must route to the IGW (cost plan C)."
}
variable "agent_spot_subnet_ids" {
  type        = list(string)
  default     = []
  description = "Subnets for the spot agent pool; when set, the pool launches from agent_private (no public IP) and these must route through the NAT. Empty keeps today's wiring."
}
variable "edge_subnet_id" {
  type        = string
  default     = ""
  description = "Subnet for the public-edge node's ENI; empty uses subnet_ids[0]. Changing it REPLACES the edge instance (the EIP re-associates)."
}
variable "server_public_ip" {
  type    = bool
  default = true
}
variable "agent_public_ip" {
  type    = bool
  default = true
}
```

In `main.tf`:
- LT `server`: `associate_public_ip_address = var.server_public_ip`.
- LT `agent`: set `associate_public_ip_address = var.agent_public_ip`, and change `user_data = base64encode(templatefile(...))` to `user_data = local.agent_user_data`, defined as:

```hcl
locals {
  # gzipped like the server's (cloud-init unzips): plain base64 was within
  # ~800 bytes of the 16 KB EC2 limit (cost plan C3).
  agent_user_data = base64gzip(templatefile("${path.module}/templates/node.sh.tftpl", merge(local.userdata_vars, { role = "agent", extra_args = local.gardenio_extra_args })))
}
```

- Add `aws_launch_template.agent_private`. It has `count = length(var.agent_spot_subnet_ids) > 0 ? 1 : 0` and a body identical to `aws_launch_template.agent`, except:
  - `name_prefix = "${local.name}-agent-private-"`
  - `user_data = local.agent_user_data`
  - `associate_public_ip_address = false`
  - the instance tag `Name = "${local.name}-agent-spot"`
- ASG `agents_spot`: `vpc_zone_identifier = length(var.agent_spot_subnet_ids) > 0 ? var.agent_spot_subnet_ids : var.subnet_ids`. Its launch template id becomes `length(var.agent_spot_subnet_ids) > 0 ? aws_launch_template.agent_private[0].id : aws_launch_template.agent.id`.
- LT `ci`: `user_data = base64gzip(templatefile(...same args...))`.
- ASG `ci`: `vpc_zone_identifier = length(var.ci_subnet_ids) > 0 ? var.ci_subnet_ids : var.subnet_ids`.
- Edge ENI: `subnet_id = var.edge_subnet_id != "" ? var.edge_subnet_id : var.subnet_ids[0]`.
- Leave the edge `aws_instance` on `user_data` (raw) with `ignore_changes = [ami, user_data]` unchanged. Switching it to `user_data_base64` would force a replacement on the next apply.

- [ ] **Step 4: CI green**, including all three `no_public_ip.tftest.hcl` runs and the existing `edge_flags.tftest.hcl`. Commit: `terraform(k3s): pool subnets, public-IP toggles, no-public-IP boot, gzipped agent/ci user_data (cost plan C3)`.

**OPERATOR, right after merge** (a no-op apply that only rolls launch template versions):

```bash
./scripts/tf-gardenio.sh plan -target=module.k3s.aws_launch_template.agent -target=module.k3s.aws_launch_template.ci -target=module.k3s.aws_launch_template.server
```

Expect only `latest_version` and `user_data` updates on three launch templates, with zero ASG or instance changes. Apply. Then make the next new node prove the gzipped boot:

```bash
kubectl get nodes --sort-by=.metadata.creationTimestamp | tail -2
```

When a new CI or spot node appears (they churn daily), check that it is Ready and that its `/var/log/k3s-bootstrap.log` ends `bootstrap complete` (via SSM: `aws ssm send-command --profile gardenio --region us-east-1 --instance-ids <id> --document-name AWS-RunShellScript --parameters 'commands=["tail -2 /var/log/k3s-bootstrap.log"]'`, then `get-command-invocation`).

**Rollback:** revert, then re-apply the three launch templates.

---

### Task C4: `hooks.clonk.tv` (GitHub App webhooks) moves from the servers' public IPs to the public edge

**Who:** CREW (one PR: manifest, Terraform, probe expectations). OPERATOR cuts over in a window. OWNER only if a delivery must be redelivered.

**Why:**
- `hooks.clonk.tv` is a CNAME to `k3s.clonk.tv`, which holds the servers' public IPs (`modules/k3s/main.tf:57-68`).
- GitHub delivers to Traefik through ServiceLB on those IPs, allowlisted by `web_ingress_cidrs` (`composition/gardenio/main.tf:109-128`). Traefik routes `Host(hooks.clonk.tv) && PathPrefix(/webhooks/)` to `gardenio-trigger-engine:8094`, which checks HMAC.
- Removing the servers' public IPs, or flipping their subnet to the NAT, breaks this.
- The edge already terminates TLS for seven names on its EIP and proxies by in-cluster DNS.

**Files:**
- Modify: `gardenio/helm/gardenio-ziti/public-edge.yaml`
  - the `Certificate` `dnsNames` (`:111`)
  - the 8080 redirect `server_name` list (`:202`)
  - a new 8443 `server` block after the `claimlayer.clonk.tv` block (`:216-232`)
- Modify: `gardenio/terraform/composition/gardenio/main.tf`
  - `public_edge_fqdns` (`:96`, add `"hooks.clonk.tv"`)
  - `hooks_fqdn` (`:129`, set `""`)
  - `web_ingress_cidrs` (`:119-128`, keep only `module.network.vpc_cidr`)
- Modify: `scripts/edge-probe/expectations.json` (`edge_hosts`, `exceptions`)
- Modify: `scripts/edge-probe/edge_probe_test.py`, if it pins the host list

- [ ] **Step 1: Failing probe expectation test.** In `scripts/edge-probe/edge_probe_test.py`, add a test that loads `expectations.json` and asserts that `hooks.clonk.tv` is in `edge_hosts` and that `exceptions["hooks.clonk.tv"] == {"/": 404, "/webhooks/edge-probe": 403}`. Run `python3 scripts/edge-probe/edge_probe_test.py`; it fails.
- [ ] **Step 2: Expectations.** Add `"hooks.clonk.tv"` to `edge_hosts`, and add to `exceptions`:

```json
    "hooks.clonk.tv": {
      "/": 404,
      "/webhooks/edge-probe": 403
    }
```

(The probe runs from outside GitHub's hooks ranges, so `/webhooks/` must refuse it with 403. `/` is not served.) Run the test again; it passes.

- [ ] **Step 3: Edge nginx.** In `public-edge.yaml`:
  - Add `hooks.clonk.tv` to the Certificate's `dnsNames` and to the port-8080 redirect `server_name` list.
  - Add this block after the `claimlayer.clonk.tv` 8443 block:

```nginx
    # hooks.clonk.tv: GitHub App webhook deliveries (cost plan 2026-10-21 C4;
    # was Traefik on the servers' public IPs). No sign-in: trigger-engine
    # verifies each delivery's HMAC. Defence in depth: only GitHub's `hooks`
    # IPv4 ranges (`gh api meta --jq .hooks`, 2026-10-11; re-check on failed
    # deliveries) and only POST. The edge pod sees the real client address
    # (hostPort, no SNAT), as the per-IP limits above rely on.
    server {
      listen 8443 ssl;
      http2 on;
      server_name hooks.clonk.tv;
      location /webhooks/ {
        allow 192.30.252.0/22;
        allow 185.199.108.0/22;
        allow 140.82.112.0/20;
        allow 143.55.64.0/20;
        allow 9.234.98.160/28;
        allow 9.234.98.176/28;
        allow 9.234.106.48/28;
        deny all;
        limit_except POST { deny all; }
        client_max_body_size 25m;
        set $up_hooks gardenio-trigger-engine.gardenio.svc.cluster.local:8094;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $remote_addr;
        proxy_set_header X-Forwarded-Proto https;
        proxy_pass http://$up_hooks;
      }
      location / { return 404; }
    }
```

  - Match the other blocks' `listen`/`http2` directive style exactly (copy from the `claimlayer.clonk.tv` block).
  - GitHub caps webhook payloads at 25 MB, hence `client_max_body_size 25m`; the file default is 1m.
  - trigger-engine has no ingress NetworkPolicy today (`kubectl -n gardenio get netpol` lists none selecting it), so no policy change is needed.

- [ ] **Step 4: Terraform.**
  - Add `"hooks.clonk.tv"` to `public_edge_fqdns`.
  - Set `hooks_fqdn = ""`. That removes the CNAME (`count = var.hooks_fqdn == "" ? 0 : 1`).
  - Reduce `web_ingress_cidrs` to `[module.network.vpc_cidr]`, and update its comment to say hooks moved to the edge on the cost plan.
  - The two record changes must not be applied in one step: Route53 refuses an A record while a CNAME of the same name exists. The OPERATOR applies them in two targeted steps (below). Say so in the PR.
- [ ] **Step 5: Validate and commit.** `nginx -t` cannot run on the laptop or in CI against this ConfigMap. Instead:
  - Run `python3 scripts/edge-probe/edge_probe_test.py`.
  - Run `kubectl apply --dry-run=client -f gardenio/helm/gardenio-ziti/public-edge.yaml` in the crew pod (client-side only, no cluster).
  - Rely on CI's Terraform validate.
  - Commit: `edge: serve hooks.clonk.tv (GitHub webhooks) on the public edge (cost plan C4)`.

**OPERATOR cutover (window; deliveries in the minute between steps 4 and 5 can fail):**

```bash
export KUBECONFIG=$HOME/ws/scratch/ssm.kubeconfig; cd ~/ws/gardenio && git fetch && git merge --ff-only origin/main
# 1. Cert and nginx first; DNS still points at the servers.
kubectl apply -f gardenio/helm/gardenio-ziti/public-edge.yaml
kubectl -n public-edge wait certificate/public-edge --for=condition=Ready --timeout=300s
kubectl -n public-edge rollout restart deploy -l app.kubernetes.io/name=public-edge 2>/dev/null || kubectl -n public-edge get pods
# 2. Test through the EIP before any DNS change: from the laptop (not a GitHub address) expect 403 on /webhooks/ and 404 on /.
curl -s -o /dev/null -w '%{http_code}\n' --resolve hooks.clonk.tv:443:32.199.110.32 -X POST https://hooks.clonk.tv/webhooks/edge-probe   # 403
curl -s -o /dev/null -w '%{http_code}\n' --resolve hooks.clonk.tv:443:32.199.110.32 https://hooks.clonk.tv/                             # 404
# 3. Positive control from inside the VPC (allowed by nothing but HMAC at trigger-engine): POST via the edge pod to the upstream directly returns trigger-engine's own 4xx, proving the upstream resolves.
kubectl -n public-edge exec deploy/public-edge -- wget -qO- --post-data='{}' http://gardenio-trigger-engine.gardenio.svc.cluster.local:8094/webhooks/edge-probe 2>&1 | head -2
# 4. Remove the CNAME (hooks down from here).
./scripts/tf-gardenio.sh apply -target=module.k3s.aws_route53_record.hooks
# 5. Create the A record on the EIP.
./scripts/tf-gardenio.sh apply -target='module.k3s.aws_route53_record.public_edge["hooks.clonk.tv"]'
dig +short hooks.clonk.tv @1.1.1.1    # expect 32.199.110.32
# 6. Watch a real delivery arrive through the edge.
kubectl -n public-edge logs deploy/public-edge --since=30m | grep -c 'hooks.clonk.tv\|/webhooks/' ; kubectl -n gardenio logs deploy/gardenio-trigger-engine --since=30m | grep -i webhook | tail -3
# 7. Only then close the servers' 80/443 to GitHub.
./scripts/tf-gardenio.sh plan -target=module.k3s   # expect only the web_ingress rules for the 7 GitHub CIDRs to be destroyed
./scripts/tf-gardenio.sh apply -target=module.k3s
# 8. Re-deploy the edge-probe Lambda (merges do not redeploy it) and invoke it.
./scripts/tf-gardenio.sh apply -target=module.edge_probes
```

- If step 1 fails to produce a Ready certificate, or step 2 returns anything else, stop before step 4.
- If no delivery is seen within 30 minutes in step 6, the OWNER opens the GitHub App's Advanced > Recent Deliveries and redelivers the newest one. It must show 2xx.
- Report which records and SG rules changed.

**Rollback:** set `hooks_fqdn = "hooks.clonk.tv"` and remove `hooks.clonk.tv` from `public_edge_fqdns`. Apply the public_edge record destroy first, then the CNAME. Then restore the 7 CIDRs and apply `module.k3s`. The Traefik IngressRoute is untouched by this task.

---

### Task C5: `kubectl-ssm.sh` can relay through the edge instance

**Who:** CREW (script PR).

**Why:**
- Today the forward targets a server's own SSM agent (`scripts/k3s/kubectl-ssm.sh:71-81`, `:144-147`).
- Once the servers sit behind the NAT, their SSM agent needs the NAT, and kubectl would die with it.
- The edge instance keeps a public address and the IGW route, and it can reach any server's 6443 (both are in the `nodes` SG, which allows all traffic from itself).

**Files:**
- Modify: `scripts/k3s/kubectl-ssm.sh`
  - header comment and env list (`:1-30`)
  - `find_server` (`:71-81`)
  - `cmd_start` (`:131-160`)
- Modify: `scripts/k3s/kubectl-ssm_test.sh`

**Interfaces:**
- New env `GARDENIO_SSM_VIA`: `server` (today's behaviour), `edge`, or `auto` (default: server first, edge if the port does not answer within `WAIT_SECS`).
- New functions `find_edge` (prints the instance id of the running instance tagged `Name=gardenio-k3s-ziti-edge`) and `server_private_ip` (prints the private IP of the chosen server).

- [ ] **Step 1: Failing tests.** Look at how `kubectl-ssm_test.sh` stubs `aws`: `sed -n 1,60p scripts/k3s/kubectl-ssm_test.sh`. Using the same stub mechanism, add two cases:
  - **edge relay:** with `GARDENIO_SSM_VIA=edge`, the stubbed `aws` must be called with `ssm start-session --target <edge-id> --document-name AWS-StartPortForwardingSessionToRemoteHost --parameters host=<server-private-ip>,portNumber=6443,localPortNumber=<port>`. The stub's `describe-instances` must answer the edge filter `Name=tag:Name,Values=gardenio-k3s-ziti-edge` with an edge id, and the server filter with an id and private IP.
  - **auto falls back:** with `GARDENIO_SSM_VIA=auto` and a stub whose first (server) session never opens the port, a second call to the edge document must follow.

  Run `bash scripts/k3s/kubectl-ssm_test.sh`; the new cases fail.
- [ ] **Step 2: Implement.**
  - Add `VIA="${GARDENIO_SSM_VIA:-auto}"` to the env block, and document it in the header (`server`, `edge` or `auto`; the edge path survives a NAT outage).
  - Add:

```bash
find_edge() {
	local id
	id="$(aws_ ec2 describe-instances \
		--filters 'Name=tag:Name,Values=gardenio-k3s-ziti-edge' 'Name=instance-state-name,Values=running' \
		--query 'Reservations[].Instances[].InstanceId' --output text)"
	id="${id%%[[:space:]]*}"
	[ -n "$id" ] && [ "$id" != None ] || die "no running edge instance (tag Name=gardenio-k3s-ziti-edge)"
	printf '%s\n' "$id"
}

server_private_ip() {
	aws_ ec2 describe-instances --instance-ids "$1" \
		--query 'Reservations[0].Instances[0].PrivateIpAddress' --output text
}

# open_session <via> <server-id>: start one detached forward, record its pid.
open_session() {
	local via=$1 sid=$2 target doc params
	if [ "$via" = edge ]; then
		target="$(find_edge)"
		doc=AWS-StartPortForwardingSessionToRemoteHost
		params="host=$(server_private_ip "$sid"),portNumber=6443,localPortNumber=$PORT"
	else
		target="$sid"
		doc=AWS-StartPortForwardingSession
		params="portNumber=6443,localPortNumber=$PORT"
	fi
	printf 'kubectl-ssm: forwarding via %s (%s)\n' "$via" "$target"
	setsid aws --profile "$PROFILE" --region "$REGION" ssm start-session --target "$target" \
		--document-name "$doc" --parameters "$params" </dev/null >>"$LOGFILE" 2>&1 &
	printf '%s\n' "$!" >"$PIDFILE"
}
```

  - In `cmd_start`, replace the single `setsid aws ... start-session` block and the wait loop with a loop over `vias` (`server edge` for `auto`, else the one given). For each: call `open_session`, then run the existing wait loop. On success, break. On failure, `cmd_stop` that session and try the next.
  - Keep the final `die` message, extended with `tried: <vias>`.
  - The written context and `--tls-server-name` are unchanged: the server certificate's SANs include `k3s.clonk.tv`.
- [ ] **Step 3:** `shellcheck -x scripts/k3s/kubectl-ssm.sh && bash scripts/k3s/kubectl-ssm_test.sh && git commit -am "kubectl-ssm: relay through the edge instance when the server path fails (cost plan C5)"`.

**OPERATOR check (before Task C8 and again after C11):**

```bash
GARDENIO_SSM_VIA=edge scripts/k3s/kubectl-ssm.sh stop; GARDENIO_SSM_VIA=edge scripts/k3s/kubectl-ssm.sh start && kubectl --context gardenio-ssm get nodes | head -3
```

The operator's IAM needs `ssm:StartSession` on the `AWS-StartPortForwardingSessionToRemoteHost` document. If that is denied, add it to the operator's policy and report the change.

---

### Task C6: Apply the edge subnets and the NAT box; prove egress with a canary; prove NAT failover

**Who:** OPERATOR. **Depends on:** C1 decision, C2 merged.

- [ ] **Step 1: Free CIDRs.**

```bash
aws ec2 describe-subnets --profile gardenio --region us-east-1 --filters Name=vpc-id,Values=vpc-0123456789abcdef0 --query 'Subnets[].CidrBlock' --output text | tr '\t' '\n' | grep -E '^10\.90\.16[01]\.' || echo "10.90.160.0/24 and 10.90.161.0/24 are free"
```

(10.90.112.0/20, 10.90.128.0/20 and 10.90.144.0/20 are taken by the empty leftover `uh2-spot-public` subnets in 1d, 1e and 1f. Leave them alone.)

- [ ] **Step 2: Composition change** (an OPERATOR commit straight to main is allowed for this repo; or a one-line PR). In `composition/gardenio/main.tf`, `module "network"`:

```hcl
  nat_mode          = "instance"  # cost plan 2026-10-21 C: was "none"
  nat_instance_type = "t4g.micro" # size chosen in Task C1 Step 3; write the measured H (GB/h) in this comment
  # nat_instance_per_az = true    # only if Task C1 said so
  edge_subnets = {
    "us-east-1a" = "10.90.160.0/24"
    "us-east-1b" = "10.90.161.0/24"
  }
  nat_instance_alarm_sns_topic_arns = [module.edge_probes.sns_topic_arn]
```

Use the edge-probes module's actual SNS output name: `grep -n 'output' gardenio/terraform/modules/edge-probes/outputs.tf`.

- [ ] **Step 3: Plan and apply.**

```bash
./scripts/tf-gardenio.sh plan -target=module.network
```

Expected:
- Creates: 2 subnets and 2 associations; the NAT SG, IAM role, profile, policies and the SSM attachment; 1 launch template; 1 ASG; 1 alarm.
- Updates: none to existing subnets or associations.
- **Zero destroys.**

Apply. Then confirm the box took over the private-app routes:

```bash
aws ec2 describe-route-tables --profile gardenio --region us-east-1 --route-table-ids rtb-0cfd8a153ef58230e rtb-0405867b24d95fabe \
  --query 'RouteTables[].[RouteTableId,Routes[?DestinationCidrBlock==`0.0.0.0/0`]|[0].NetworkInterfaceId]' --output text
```

Both tables must show an `eni-...`, the NAT box's.

- [ ] **Step 4: Canary with no public IP in private-app 1b** (the far AZ, so the cross-AZ path is tested too):

```bash
P="--profile gardenio --region us-east-1"
AMI=$(aws ssm get-parameter $P --name /aws/service/canonical/ubuntu/server/24.04/stable/current/amd64/hvm/ebs-gp3/ami-id --query Parameter.Value --output text)
SG=$(aws ec2 describe-security-groups $P --filters Name=vpc-id,Values=vpc-0123456789abcdef0 Name=group-name,Values='gardenio-k3s-nodes*' --query 'SecurityGroups[0].GroupId' --output text)
IID=$(aws ec2 run-instances $P --image-id "$AMI" --instance-type t3.micro --subnet-id subnet-0123456789abcdef4 --no-associate-public-ip-address \
  --security-group-ids "$SG" --iam-instance-profile Name=gardenio-k3s --metadata-options HttpTokens=required \
  --tag-specifications 'ResourceType=instance,Tags=[{Key=Name,Value=costplan-c6-canary}]' --query 'Instances[0].InstanceId' --output text); echo "$IID"
# Rollback/cleanup, always: aws ec2 terminate-instances $P --instance-ids "$IID"
sleep 120
CID=$(aws ssm send-command $P --instance-ids "$IID" --document-name AWS-RunShellScript --parameters 'commands=["curl -s -m 10 https://checkip.amazonaws.com","curl -s -o /dev/null -m 10 -w %{http_code} https://api.github.com","curl -s -o /dev/null -m 10 -w %{http_code} https://api.anthropic.com","curl -s -o /dev/null -m 10 -w %{http_code} https://123456789012.dkr.ecr.us-east-1.amazonaws.com/v2/","curl -s -o /dev/null -m 10 -w %{http_code} https://slack.com"]' --query Command.CommandId --output text)
sleep 15; aws ssm get-command-invocation $P --command-id "$CID" --instance-id "$IID" --query StandardOutputContent --output text
aws ec2 terminate-instances $P --instance-ids "$IID"
```

Pass:
- SSM answered at all, which proves the SSM agent reaches SSM through the NAT.
- `checkip` prints the NAT box's public IP.
- The four HTTP codes are non-zero (GitHub 200, Anthropic 404/401, ECR 401, Slack 200 or 3xx).

- [ ] **Step 5: NAT failover, timed.**

```bash
NAT=$(aws ec2 describe-instances $P --filters Name=tag:gardenio:nat-role,Values=instance Name=instance-state-name,Values=running --query 'Reservations[0].Instances[0].InstanceId' --output text)
date -u +%T; aws ec2 terminate-instances $P --instance-ids "$NAT" >/dev/null
until aws ec2 describe-route-tables $P --route-table-ids rtb-0405867b24d95fabe --query 'RouteTables[0].Routes[?DestinationCidrBlock==`0.0.0.0/0`]|[0].State' --output text | grep -q active && \
      [ "$(aws ec2 describe-instances $P --filters Name=tag:gardenio:nat-role,Values=instance Name=instance-state-name,Values=running --query 'length(Reservations)' --output text)" != 0 ]; do sleep 10; done; date -u +%T
```

Expect a gap of about 1-3 minutes, until the route is active again on the new ENI. Nothing depends on the NAT yet. Record the gap; Review Focus item 5 depends on it.

**Rollback:** set `nat_mode = "none"` and `edge_subnets = {}`, then apply `-target=module.network`. That destroys the box and the edge subnets, which are still empty at this point.

---

### Task C7: The CI pool moves to the edge subnets (pool 1, keeps public IPs)

**Who:** OPERATOR. **Depends on:** C3 merged and applied, C6.

- [ ] **Step 1.** In `module "k3s"` in the composition, add:

```hcl
  ci_subnet_ids = values(module.network.edge_subnet_ids)
```

- [ ] **Step 2.** `./scripts/tf-gardenio.sh plan -target=module.k3s.aws_autoscaling_group.ci` must show only `vpc_zone_identifier` changing, in place. Apply.
- [ ] **Step 3.** Existing CI nodes stay until the autoscaler scales them in; do not terminate them by hand. Within a day:

```bash
aws ec2 describe-instances --profile gardenio --region us-east-1 --filters Name=tag:aws:autoscaling:groupName,Values=gardenio-k3s-ci Name=instance-state-name,Values=running --query 'Reservations[].Instances[].[InstanceId,PrivateIpAddress,PublicIpAddress]' --output text
```

Pass:
- New CI nodes have 10.90.160.x or 10.90.161.x addresses and a public IP.
- CI jobs are green: the next `checks` and `images` runs on main.
- The NTH DaemonSet runs on them: `kubectl -n kube-system get ds | grep -i termination`.

**Rollback:** remove the line and apply the ASG target.

---

### Task C8: The public-edge node moves into edge subnet 1a, keeping its EIP

**Who:** OPERATOR, in a window: the public edge is down while the replacement node boots, about 5-10 minutes. **Depends on:** C3, C4 cut over, C5, C6.

- [ ] **Step 1: Announce and back up.** Back up the edge's manifests as the 813 edge backup did (`~/ws/scratch/edge-backup-pre-813/` is the pattern). This is not Secrets: they are re-created from cert-manager. Check the relay first: `GARDENIO_SSM_VIA=server scripts/k3s/kubectl-ssm.sh start`. kubectl must not depend on the edge during its own replacement.
- [ ] **Step 2.** In `module "k3s"`: `edge_subnet_id = module.network.edge_subnet_ids["us-east-1a"]`.

```bash
./scripts/tf-gardenio.sh plan -target=module.k3s.aws_network_interface.ziti_edge -target=module.k3s.aws_instance.ziti_edge -target=module.k3s.aws_eip.ziti_edge
```

The plan **must** show:
- `aws_network_interface.ziti_edge[0]` replaced
- `aws_instance.ziti_edge[0]` replaced
- `aws_eip.ziti_edge[0]` updated **in place**, with `network_interface` changing and `public_ip` staying `32.199.110.32`

If the EIP is to be destroyed, STOP. Releasing it is irreversible: the address would be gone.

- [ ] **Step 3.** Apply. Then delete the old node object once its instance is terminated:

```bash
kubectl get nodes -L gardenio.io/role | grep ziti-edge
```

When the old node is NotReady, run `kubectl delete node <old-name>`. Wait for the new edge node to be Ready and the public-edge pod Running on it:

```bash
kubectl -n public-edge get pods -o wide
```

- [ ] **Step 4: Verify.**

```bash
for h in auth.clonk.tv gardenio.clonk.tv grafana.clonk.tv api.clonk.tv mux.clonk.tv dashboard.clonk.tv claimlayer.clonk.tv hooks.clonk.tv; do printf '%-22s ' $h; curl -s -o /dev/null -m 10 -w '%{http_code}\n' https://$h/; done
aws lambda invoke --profile gardenio --region us-east-1 --function-name "$(aws lambda list-functions --profile gardenio --region us-east-1 --query 'Functions[?contains(FunctionName,`edge-probe`)].FunctionName|[0]' --output text)" --invocation-type Event /dev/null
GARDENIO_SSM_VIA=edge scripts/k3s/kubectl-ssm.sh stop; GARDENIO_SSM_VIA=edge scripts/k3s/kubectl-ssm.sh start && kubectl --context gardenio-ssm get nodes | head -2
```

Pass:
- The codes match the edge-probe expectations: auth `/` 404, hooks `/` 404, the others a sign-in redirect.
- The probe Lambda's next result is green.
- The edge relay works from the new instance.

**Rollback:** set `edge_subnet_id = ""` and apply the ENI, instance and association targets:

```bash
./scripts/tf-gardenio.sh plan -target=module.k3s.aws_network_interface.ziti_edge -target=module.k3s.aws_instance.ziti_edge -target=module.k3s.aws_eip_association.ziti_edge
```

The plan must show the ENI, the instance and `aws_eip_association.ziti_edge[0]` replaced, and `aws_eip.ziti_edge[0]` not in it at all. That puts the edge back into public 1a with `32.199.110.32` re-associated to the new ENI.

This needs the `aws_eip_association` split (gd-nTBAOEoxqj) applied first, with the live association imported. The C8 apply itself hit the bug it fixes: the in-place EIP update called AssociateAddress with the terminated instance's id, and the EIP stayed unattached until `aws ec2 associate-address --allocation-id eipalloc-02008a15dd5212b81 --network-interface-id <new eni>` by hand. Before that split is live, a rollback with the old targets must expect that same manual step.

---

### Task C9: The spot agent pool moves into private-app behind the NAT (pool 2)

**Who:** OPERATOR. **Depends on:** C7, C8.

- [ ] **Step 1.** In `module "k3s"`: `agent_spot_subnet_ids = module.network.private_subnet_ids`. Confirm with `grep -n 'output "private_subnet_ids"' -A3 gardenio/terraform/modules/network/outputs.tf` that this output is the private-app subnets.

```bash
./scripts/tf-gardenio.sh plan -target=module.k3s.aws_launch_template.agent_private -target=module.k3s.aws_autoscaling_group.agents_spot
```

Expect: create `agent_private`; update `agents_spot` in place (subnets and launch template id). Apply.

- [ ] **Step 2: Roll the pool one node at a time.** Do not hand-set desired capacity (Global Constraint 7). Use an instance refresh that keeps the pool's minimum healthy:

```bash
aws autoscaling start-instance-refresh --profile gardenio --region us-east-1 --auto-scaling-group-name gardenio-k3s-agents-spot \
  --preferences '{"MinHealthyPercentage":100,"MaxHealthyPercentage":200,"InstanceWarmup":300,"SkipMatching":true}'
```

Spot nodes carry crews, and a crew's session dies with its node. Run this when `kubectl -n gardenio get pods -l gardenio.io/agent -o wide` shows no crew on spot nodes, or accept and report the restarts. Watch progress:

```bash
aws autoscaling describe-instance-refreshes --profile gardenio --region us-east-1 --auto-scaling-group-name gardenio-k3s-agents-spot --query 'InstanceRefreshes[0].[Status,PercentageComplete]'
```

- [ ] **Step 3: Verify.** This is the first live boot without a public IP (Review Focus 4):
  - New spot nodes have 10.90.32-63.x addresses and no public IP.
  - They are Ready.
  - Their bootstrap log ends `bootstrap complete` (via SSM, which now goes through the NAT).
  - The node object has no ExternalIP (`kubectl get node <n> -o wide`).
  - Pods on them pull images and reach Anthropic: a crew spawned there completes a turn.
  - Watch NAT health: `aws cloudwatch get-metric-statistics --namespace AWS/EC2 --metric-name NetworkIn` for the NAT instance, and `CPUCreditBalance`, hourly for a day.

**Rollback:** remove `agent_spot_subnet_ids`, apply both targets, and start an instance refresh again.

---

### Task C10: The old public subnets route through the NAT, one AZ at a time: 1c, then 1a, then 1b

**Who:** OPERATOR. **Depends on:** C9 healthy for 24 h. Nodes in these subnets keep their public IPs for now. Only their default route changes.

- **What is in each subnet:** 1c holds server `ip-10-90-96-100` only. 1a holds a server, on-demand agent 2-170 (minio, vmsingle, grafana PVs) and HQ (stopped). 1b holds a server, on-demand agents 17-224 and 30-213 (postgres).
- **Why 1c first:** it holds one etcd member of three. If its egress fails, quorum survives on 1a and 1b.
- **Why 1b last:** it holds Postgres.

For each AZ in order `us-east-1c`, `us-east-1a`, `us-east-1b`:

- [ ] **Step 1.** Add the AZ to `nat_routed_public_azs` in `module "network"`, for example `nat_routed_public_azs = ["us-east-1c"]`. Plan with `-target=module.network`. Expect exactly one `aws_route_table_association` (`public_extra` for 1c, `public[i]` for 1a/1b) updated in place, and that subnet's `map_public_ip_on_launch` true -> false. Nothing else. Write down the rollback (remove the AZ, apply). Apply.
- [ ] **Step 2: Verify within 5 minutes**, for each node in that subnet:

```bash
P="--profile gardenio --region us-east-1"
# The subnet being flipped: subnet-0123456789abcdef2 (1c), subnet-0123456789abcdef0 (1a), subnet-0123456789abcdef1 (1b).
SUBNET=subnet-0123456789abcdef2
for ip in $(aws ec2 describe-instances $P --filters Name=subnet-id,Values=$SUBNET Name=instance-state-name,Values=running --query 'Reservations[].Instances[].PrivateIpAddress' --output text); do
  IID=$(aws ec2 describe-instances $P --filters Name=private-ip-address,Values=$ip --query 'Reservations[0].Instances[0].InstanceId' --output text)
  CID=$(aws ssm send-command $P --instance-ids "$IID" --document-name AWS-RunShellScript --parameters 'commands=["curl -s -m 10 https://checkip.amazonaws.com","curl -s -o /dev/null -m 10 -w %{http_code} https://api.github.com"]' --query Command.CommandId --output text)
  sleep 10; echo "$ip: $(aws ssm get-command-invocation $P --command-id "$CID" --instance-id "$IID" --query StandardOutputContent --output text | tr '\n' ' ')"
done
export KUBECONFIG=$HOME/ws/scratch/ssm.kubeconfig; kubectl get nodes; kubectl get --raw '/readyz/etcd'; kubectl -n gardenio get pods | grep -vE 'Running|Completed' | head
```

Pass:
- `checkip` shows the NAT IP.
- GitHub answers 200.
- All nodes are Ready, `/readyz/etcd` says `ok`, and there are no new failing pods.
- For 1b: also `kubectl -n gardenio logs deploy/gardenio-beads --since=5m | grep -c ERROR` shows no rise, and `gb`/`kd` work (`kd list --limit 1`).

If any check fails: remove the AZ from the list and apply. That restores the IGW route in seconds.

- [ ] **Step 3.** Wait 24 h before the next AZ, watching NAT `NetworkIn`, `CPUCreditBalance` and the alarm.

---

### Task C11: Remove public IPs in place: on-demand agents (pool 3), then servers (pool 4, last)

**Who:** OPERATOR. **Depends on:** C10 complete (all three AZs routed through the NAT) for 24 h.

- [ ] **Step 1: Launch templates first,** so a replacement node never gets a public IP again. In `module "k3s"`: `agent_public_ip = false`, `server_public_ip = false`. Plan `-target=module.k3s.aws_launch_template.agent -target=module.k3s.aws_launch_template.server`: only those two templates change. Apply.
- [ ] **Step 2: On-demand agents, one at a time,** in the order 17-224, 2-170, 30-213 (Postgres last). No stop, no drain, no reboot. The instance keeps running.

```bash
P="--profile gardenio --region us-east-1"
strip() { local ip=$1 iid eni
  iid=$(aws ec2 describe-instances $P --filters Name=private-ip-address,Values=$ip --query 'Reservations[0].Instances[0].InstanceId' --output text)
  eni=$(aws ec2 describe-instances $P --instance-ids "$iid" --query 'Reservations[0].Instances[0].NetworkInterfaces[?Attachment.DeviceIndex==`0`].NetworkInterfaceId' --output text)
  echo "strip $ip $iid $eni (rollback: aws ec2 modify-network-interface-attribute $P --network-interface-id $eni --associate-public-ip-address)"
  aws ec2 modify-network-interface-attribute $P --network-interface-id "$eni" --no-associate-public-ip-address
  aws ec2 describe-instances $P --instance-ids "$iid" --query 'Reservations[0].Instances[0].PublicIpAddress' --output text   # expect None
}
strip 10.90.17.224
```

After each strip, wait 10 minutes and repeat the C10 Step 2 checks for that node, plus `kubectl -n gardenio get pods -o wide | grep <node>` (all Running). Then do the next node.

- [ ] **Step 3: Servers, one at a time,** in the order 1c (96-100), 1a (8-183), 1b (28-242). Before each: `kubectl get --raw '/readyz/etcd'` must be `ok`. Then:

```bash
strip 10.90.96.100
```

After each, check etcd, `kubectl get nodes`, and both kubectl paths:

```bash
GARDENIO_SSM_VIA=server scripts/k3s/kubectl-ssm.sh stop; GARDENIO_SSM_VIA=server scripts/k3s/kubectl-ssm.sh start && kubectl --context gardenio-ssm get nodes | head -2
GARDENIO_SSM_VIA=edge   scripts/k3s/kubectl-ssm.sh stop; GARDENIO_SSM_VIA=edge   scripts/k3s/kubectl-ssm.sh start && kubectl --context gardenio-ssm get nodes | head -2
```

- [ ] **Step 4: The API name.** Running servers do not re-run `k3s-dns`, so point `k3s.clonk.tv` at the servers' private IPs now. Old public IPs return to AWS's pool and must not stay published.

```bash
ZONE=$(aws route53 list-hosted-zones-by-name $P --dns-name clonk.tv --query 'HostedZones[0].Id' --output text)
IPS=$(aws ec2 describe-instances $P --filters Name=tag:k3s:role,Values=server Name=instance-state-name,Values=running --query 'Reservations[].Instances[].PrivateIpAddress' --output text)
aws route53 change-resource-record-sets $P --hosted-zone-id "$ZONE" --change-batch "$(python3 -c '
import json,sys; ips=sys.argv[1].split()
print(json.dumps({"Changes":[{"Action":"UPSERT","ResourceRecordSet":{"Name":"k3s.clonk.tv.","Type":"A","TTL":30,"ResourceRecords":[{"Value":i} for i in ips]}}]}))' "$IPS")"
dig +short k3s.clonk.tv @1.1.1.1
```

Expect the three 10.90.x addresses. Then re-deploy and invoke the edge-probe Lambda (`./scripts/tf-gardenio.sh apply -target=module.edge_probes`, then an async invoke as in Task C8): its sweep now covers only the edge EIP, the NAT box and the CI nodes.

- [ ] **Step 5: Report** every ENI changed (instance id, ENI id, the released address) and the new `k3s.clonk.tv` value. **Irreversible part:** the released public addresses cannot be got back. A rollback gets new addresses (`--associate-public-ip-address`), which is harmless here because nothing allowlists them (checked: no outside service pins node IPs).

---

### Task C12: Measure Phase C

**Who:** OPERATOR. **When:** 48 h after C11.

```bash
P="--profile gardenio --region us-east-1"
aws ec2 describe-instances $P --filters Name=instance-state-name,Values=running --query 'Reservations[].Instances[?PublicIpAddress!=null].[Tags[?Key==`Name`]|[0].Value]' --output text | sort | uniq -c
aws ce get-cost-and-usage $P --time-period Start=$(date -u -d '-7 days' +%F),End=$(date -u +%F) --granularity DAILY --metrics UsageQuantity UnblendedCost \
  --filter '{"Dimensions":{"Key":"USAGE_TYPE","Values":["USE1-PublicIPv4:InUseAddress","DataTransfer-Regional-Bytes"]}}' --group-by Type=DIMENSION,Key=USAGE_TYPE \
  --query 'ResultsByTime[].[TimePeriod.Start,Groups[].[Keys[0],Metrics.UsageQuantity.Amount,Metrics.UnblendedCost.Amount]]' --output json
```

Pass:
- Public IPs only on: the edge (1), the NAT box (1, or 2 if per-AZ) and the CI nodes (0-6).
- IPv4 hours per day near 24 x (2 + average CI nodes), compared with 516.6 on 10-20.
- Report the NAT cost (instance hours x price) and any rise in `DataTransfer-Regional` from cross-AZ NAT traffic.

---

## Expected savings

| Phase | What changes | Basis (measured 2026-10-21) | $/day |
|---|---|---|---|
| A1 | No PR push of an unused controller-base | 0.308 GB/push; 20-356 pushes/day | 0.6-9.9 |
| A2-A5 | Release images and cache to ECR, ghcr dropped | 25-35 GB/release, about 1 release/day; ECR storage costs back about $0.1-0.2 | 2.1-3.0 |
| B1 | Backup stays on the Postgres node | about 8 cross-AZ runs/day x 18 GB x $0.02 | about 2.9 |
| B2 + B4 | Viewer stops re-reading; daemon and clients in one zone | 102 -> about 5 GB/day viewer reads; about 100 GB/day of remaining daemon traffic kept in one AZ | 1-2 |
| C | 10 public IPv4 removed, NAT t4g.micro added | 10 x $0.12 - ($0.20 instance + $0.12 its IPv4) | about 0.9 (0.6 per-AZ) |
| **Total** | | at 10-19/20 rates | **about 7.5-18.7** |

Against October's average for these three lines ($106 over 21 days, about $5/day), expect them to fall to about $1/day. That is about $120/month saved at average rates, and more on busy days. It does not by itself reach the $200/month ceiling: compute was $342 of the $446.

## Not in scope (flagged for the owner, with numbers)

- **Compute** is 77% of October. `ci_max_nodes` was cut to 6 today; measure a week before anything else.
- **The backup's size and frequency.**
  - The hourly `pg_dumpall` reads about 18 GB from Postgres for about 35 minutes of every hour.
  - S3 holds 183 GB under `k3s-postgres/` and grows about 65 GB a day toward a 30-day steady state of about 2 TB, about $45/month at S3 Standard.
  - `events` and `mutation_history` (5 GB together, unpruned since 09-18) drive it.
  - Retention, frequency and pruning are owner calls (a decision bead), not this plan's.
- **The HQ host** (`modules/hq`, public 1a) gets egress through the NAT after Task C10. Its own public IPv4 ($0.12/day while running) can be stripped in place the same way when convenient.
- **Leftover UH2 subnets** 10.90.112.0/20, 10.90.128.0/20 and 10.90.144.0/20 (1d/1e/1f) are empty and attached to the public route table. They cost nothing and are left alone.
- **`controller/internal/reconciler/imagedigest.go:179-255`** polls manifests with a ghcr token. It already fails quietly against ECR hosts, at Debug level. The digest pin it feeds has not worked since the pull-through cache went live. That is a separate fix.
- **Next beads cut:** trigger-engine (58.6 GB/day) and the controller (35.4 GB/day). Task B5 names the routes; a follow-up plan does the work.
