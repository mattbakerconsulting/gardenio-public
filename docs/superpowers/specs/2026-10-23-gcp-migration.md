# gardenio on GCP: migration spec (draft for the owner's decision)

Date: 2026-10-23. Author: laptop operator. Status: DRAFT, not approved.

## 1. The decision

Whether to move the gardenio platform (k3s cluster, 29 images, Postgres,
MinIO, Keycloak, metrics, bridges, CI runners) from AWS us-east-1 to GCP, and
if so to which shape. The owner's stated motives are cost, possible credits,
GCP fluency for interviews (ClaimLayer is already a GCP org exercise), and the
product claim "gardenio runs on GKE". This document gives a cost model for
three GCP shapes against the AWS shapes we already have, the full list of what
has to move, a cutover plan with its double-run cost, and a recommendation.
Every price below is a list price from memory and MUST be re-checked on the
pricing pages before anything is approved.

## 2. Where the AWS bill stands today

Measured 2026-10-23 (Cost Explorer, Oct 16-22 average, then today's cuts):

| AWS shape | $/day | $/month | State |
|---|---|---|---|
| Oct 16-22 average (before the cost plan) | 33 | ~1,000 | past |
| After C10/C11/A5/B5 and shape A (one NAT, spot floor 0, 2 on-demand agents) | 13-15 | 400-450 | live since 17:24Z |
| Planned AWS floor: CPU requests right-sized so the spot pool empties, CI image builds gated on PRs | 9-10 | 280-300 | tasks filed |
| Single node (one m6a.xlarge runs control plane + everything, no NAT, crews on spot only while running) | 5-6 | 165-180 | not started; loses HA |

At rest the platform uses about 1.5 vCPU and 17 GiB across ~35 pods. Crews
and gardeners are bursty (default request 2 vCPU / 8 GiB each). CI used
120-190 runner-hours/day before the two-releases-a-day rule.

## 3. Why move (owner's motives, in their words and ours)

- "GCP i think will be cheaper; and we may have more credits available there."
- GCP fluency for interviews; ClaimLayer's org-level Terraform already lives on
  GCP (org 936422992904, repo homehealthtpa).
- "gardenio runs on GKE" is a product claim for Lands.
- Not a motive: the current AWS stack is broken. It is not; it is oversized.

## 4. Credits: what could be verified

- `gcloud auth list` shows matthew.baker@homehealthtpa.com active, but the
  token has expired: every billing/projects call fails with "Reauthentication
  failed. cannot prompt during non-interactive execution". The operator could
  NOT read billing accounts or credits.
- Owner to check, in the console: Billing > the homehealthtpa billing account
  > Credits. Things to look for:
  - Free Trial: $300 for 90 days, granted once per NEW billing account when
    billing is first activated. If the ClaimLayer account already activated
    billing on 10-19 the clock is already running.
  - Google for Startups Cloud Program: $2,000 (self-serve "Start" tier) up to
    $200,000+ for backed startups; needs an application with a company site.
  - Workspace-linked promotions, if any, show in the same Credits panel.
- Run on the laptop once signed in: `gcloud billing accounts list`, then
  `gcloud billing accounts describe <id>`; credits themselves are only in the
  console or the BigQuery billing export.

How long each shape runs on a $300 trial (section 5 numbers): Autopilot about
4-6 weeks, GKE Standard about 6-8 weeks, k3s-on-GCE about 7-9 weeks. None of
these credits change the long-run answer; they defer it by one to two months.

## 5. Target shapes and cost model

Price assumptions (us-central1, list, re-check at
https://cloud.google.com/kubernetes-engine/pricing,
https://cloud.google.com/compute/vm-instance-pricing,
https://cloud.google.com/nat/pricing, https://cloud.google.com/vpc/network-pricing,
https://cloud.google.com/sql/pricing):

| Item | Assumed list price |
|---|---|
| GKE cluster fee | $0.10/h ($73/mo); one zonal or Autopilot cluster per billing account is covered by a $74.40/mo credit |
| Autopilot general-purpose pod | $0.0445/vCPU-h, $0.0049/GiB-h; minimum 0.25 vCPU and 0.5 GiB per pod |
| Autopilot Spot pod | about 60-70% off the above (~$0.0133/vCPU-h, ~$0.0015/GiB-h) |
| e2-standard-2 (2 vCPU, 8 GiB) | $0.067/h ($49/mo); Spot ~$0.020/h ($15/mo) |
| e2-standard-4 (4 vCPU, 16 GiB) | $0.134/h ($98/mo); Spot ~$0.040/h ($29/mo) |
| e2-medium (2 shared vCPU, 4 GiB) | $0.0335/h ($24/mo) |
| Cloud NAT | $0.044/h per gateway-VM slot (~$32/mo for a small VPC) + $0.045/GB |
| VM external IP (instead of Cloud NAT) | $0.004/h ($3/mo) |
| Internet egress (premium tier, N. America) | $0.12/GB first TB (AWS is $0.09/GB); standard tier $0.085/GB |
| pd-balanced / pd-standard | $0.10 / $0.04 per GB-month |
| External HTTP(S) load balancer | $0.025/h ($18/mo) + rules; avoidable with nginx on a VM external IP as today |
| Cloud SQL Postgres (if used) | db-f1-micro ~$9/mo, db-g1-small ~$27/mo, Enterprise HA 2 vCPU ~$150+/mo |
| Artifact Registry | $0.10/GB-mo |
| GCS standard | $0.02-0.026/GB-mo |
| Secret Manager | $0.06 per active secret version per month |
| Cloud DNS | $0.20/zone/mo + $0.40 per million queries |

### 5a. GKE Autopilot

Billing is per pod request, so the ~35 platform pods are priced at their
requests with a 0.25 vCPU / 0.5 GiB floor each. With requests right-sized to
usage (about 4 vCPU and 20 GiB total, after floors) at-rest compute is about
$130 (vCPU) + $72 (memory) = ~$200/mo. Crews run as Spot pods at about
$0.04/h each; CI runners (3 vCPU / 12 GiB Spot) at about $0.06/h, so 120
runner-hours/day is ~$7/day.

| Autopilot | $/month |
|---|---|
| Platform pods at rest (requests right-sized) | ~200 |
| Cluster fee | 0 (free-tier credit) |
| Disks 45 GiB pd-balanced + GCS 50 GB | ~8 |
| NAT/egress (Spot pods need Cloud NAT; ~30 GB/day egress at $0.12) | ~140 |
| Crews (bursty, Spot) | 20-40 |
| CI at 2 releases/day plus PR CI (~60 runner-h/day Spot) | ~110 |
| Total | ~480-500 |

Gives up: hostPath (log shippers, the slack-bridge state move we did today),
privileged pods, most DaemonSets, local-path volumes, node-level control,
kyverno runtimeClass tricks. gVisor is native (good for crews). Autopilot does
not get cheaper than AWS at rest because of the per-pod floors on a chart with
many small pods.

### 5b. GKE Standard: one e2-standard-4 plus a Spot node pool

One on-demand e2-standard-4 (4 vCPU, 16 GiB) runs the platform; a Spot
e2-standard-2 pool, min 0, takes crews and CI runners. The node's external IP
replaces Cloud NAT. Nginx edge on the node's external IP as today, no load
balancer.

| GKE Standard | $/month |
|---|---|
| e2-standard-4 on-demand | 98 |
| Cluster fee | 0 (one zonal cluster is free-tier) |
| Node external IP | 3 |
| Disks 100 GiB pd-balanced + GCS 50 GB | ~12 |
| Egress ~30 GB/day at $0.12 (standard tier $0.085 lowers it) | 75-110 |
| Spot pool for crews (e2-standard-2, ~8 h/day) | ~5 |
| CI runners (Spot e2-standard-4, ~60 runner-h/day) | ~75 |
| Total | ~270-300 |

Gives up: control-plane HA (zonal cluster; GKE manages the control plane, so
it is a cluster-availability risk, not an etcd-loss risk), the 3-AZ story.
Memory is the limit: 16 GiB for 17 GiB used today means a second node or
trimming (the kbeads 2 GiB request uses 31 MiB; litellm 2 GiB x2).

### 5c. k3s on one or two GCE VMs (closest to today)

One e2-standard-4 running k3s server + workloads; cluster-autoscaler with the
GCE provider over a Spot managed instance group for crews. Everything stays as
it is (local-path PVs, hostPath shippers, kyverno, gVisor needs the node
runtime installed by us).

| k3s on GCE | $/month |
|---|---|
| e2-standard-4 | 98 |
| External IP, disks, GCS | ~15 |
| Egress | 75-110 |
| Spot MIG for crews + CI (same as 5b) | ~80 |
| Total | ~270-300 |

Gives up: "runs on GKE" (it runs on GCE), and we keep operating the control
plane ourselves. It is the AWS single-node option with a different logo.

### 5d. Side by side

| Shape | $/month at rest | $/month with crews + CI | HA | Effort to get there |
|---|---|---|---|---|
| AWS today (shape A) | ~230 | 400-450 | yes (3 servers, 3 AZ) | done |
| AWS floor (requests + CI gating) | ~180 | 280-300 | yes | 2 crew tasks |
| AWS single node | ~120 | 165-180 | no | 1 maintenance window, ~2 h outage |
| GCP Autopilot | ~350 | 480-500 | GKE-managed | L (chart rework for Autopilot limits) |
| GCP GKE Standard | ~190 | 270-300 | GKE-managed control plane, 1 node | M-L |
| GCP k3s on GCE | ~190 | 270-300 | no | M |

Reading: on cost alone, GCP lands where the AWS floor lands, and the AWS
single-node shape is cheaper than any GCP shape because GCP egress is a third
more expensive and the cheapest GCP compute is not below AWS's. The only
numbers that favour GCP are credits, and they run out in one to two months.

## 6. What moves and how

Effort: S = a day or less for a crew, M = a few days, L = a week or more.
"Owner" = a click, login or account action only the owner can do.

| Component | AWS today | GCP equivalent | Effort | Owner needed |
|---|---|---|---|---|
| Network, NAT, egress | VPC, 2 edge subnets, private-app subnets, one NAT box (fck-nat style, runtime-owned routes) | VPC, one subnet per region; node external IP or Cloud NAT; firewall rules | M | no |
| Nodes | 3 k3s servers (spot+on-demand), 2 on-demand agents, Spot ASG via cluster-autoscaler, CI ASG | GKE Standard node pools (one on-demand, one Spot min 0) or k3s on GCE MIGs | M | enable APIs, quota check |
| Ingress + TLS | public-edge node (nginx) with an EIP, Traefik in-cluster, cert-manager + ACM | same nginx on a node external IP; cert-manager with Cloud DNS solver; no ACM | S-M | no |
| DNS | Route53 zone clonk.tv (13 records, incl. k3s.clonk.tv, auth, api, lands.*) | keep Route53 (simplest, $0.50/mo) or Cloud DNS + NS change at the registrar | S (keep) / M (move) | NS change at registrar if moved |
| Images + CI trust | ECR (3 repos, pull-through cache), GitHub OIDC role with the immutable sub format | Artifact Registry; Workload Identity Federation pool for GitHub Actions | M | no (Terraform) |
| Object storage | S3: gardenio-sessions (session resume), postgres backups, status bucket (public, uptime Lambda), support-mail bucket | GCS buckets; the Go code imports aws-sdk s3 in 59 places: either S3-compatible mode against GCS (interoperability HMAC keys) or a storage interface | M-L | HMAC keys or SA |
| In-cluster object store | MinIO 20 GiB PV (pgsty) | keep MinIO, or point it at GCS | S | no |
| Secrets | Secrets Manager (12 import sites), SSM parameters | Secret Manager; the controller's secretsmanager client needs a second backend | M | no |
| Mail | SES (sesv2 client, support@lands.clonk.tv inbound to S3, identity in sandbox) | keep SES (works from anywhere with keys) or SendGrid/Mailgun | S (keep) / M | account creation if moved; SES sandbox exit is still owner-pending |
| Operator access | SSM port-forward to a server (tunnel.sh), kubectl-ssm.sh via server or edge | `gcloud container clusters get-credentials` with a private endpoint + IAP, or `gcloud compute ssh --tunnel-through-iap` | S | gcloud login |
| Metrics, cost panels | CloudWatch exporter (chart key cloudwatchExporter), Cost Explorer panels in Grafana, cloudwatchlogs client in 6 sites | Billing export to BigQuery + a Grafana BigQuery datasource; Cloud Monitoring exporter | M | enable billing export |
| Edge probes | 2 Lambdas (edge probe, uptime probe) on EventBridge schedules, status bucket | Cloud Functions on Cloud Scheduler, GCS status bucket | S-M | no |
| Keycloak | keycloak-runtime StatefulSet on Postgres, auth.clonk.tv behind the edge | unchanged (portable) | S | re-login |
| Postgres | self-hosted StatefulSet, 10 GiB local-path PV, hourly S3 dumps | self-hosted on pd-balanced (keep), or Cloud SQL db-g1-small ~$27/mo + storage (adds cost, removes the backup chore) | S (keep) | no |
| Agent runtime | controller, coopmux, crews, gVisor runtimeClass, lambdamicrovms client (15 sites) | GKE Sandbox (gVisor) native on Standard/Autopilot; the lambdamicrovms path has no equivalent and must be fenced off | M | no |
| Log shippers | hostPath DaemonSets (idp, ava, traefik, bridge, core shippers) | fine on Standard/GCE; not on Autopilot | S / L | no |
| Backups, KMS, budgets, SNS alarms | backup-vault modules, 3 KMS keys, 4 budgets, SNS | Cloud KMS, Budgets with Pub/Sub, Cloud Monitoring alerts | M | billing-budget permission |
| Terraform | 25 modules, composition, state in S3 | new composition against the google provider; keep the AWS one until teardown | L | no |

The portable core (chart, images, Keycloak, Postgres, MinIO, Traefik,
cert-manager, Grafana/VictoriaMetrics, bridges) is most of the value and moves
with a values file. The AWS-specific layer is the Terraform composition and
five SDK integrations in the controller (s3, secretsmanager, sesv2,
cloudwatchlogs, sts) plus the lambdamicrovms client.

## 7. Migration order and cutover

1. Foundation (owner clicks): billing account and credits confirmed; a
   `gardenio-prod` project under the existing org; APIs enabled; Workload
   Identity Federation for GitHub; Artifact Registry. One day.
2. Images: images.yml pushes calver tags to Artifact Registry as well as ECR
   (A5 just removed a second registry; this re-adds one for the window). One
   crew day.
3. Cluster: GKE Standard zonal cluster, one e2-standard-4 node pool, one Spot
   pool min 0, GKE Sandbox enabled, cert-manager with a Cloud DNS or HTTP
   solver, nginx edge on the node's external IP. Two to three crew days.
4. Storage and secrets: GCS buckets, HMAC keys or the storage interface,
   Secret Manager backend, Cloud Function probes. Three to five crew days.
5. Shadow install: the chart with a GCP values file, pointed at a fresh
   Postgres restored from the latest S3 dump, MinIO synced, Keycloak realm
   exported/imported. Smoke-test on a parallel hostname (gcp.clonk.tv) with
   the edge nginx in front. Two crew days.
6. Cutover: freeze writes (pause the trigger-engine and bridges), final
   Postgres dump and restore (minutes), final MinIO sync, flip DNS (TTL is
   already 30 s on k3s.clonk.tv; lower the others to 60 s a day before),
   unpause. Expected outage 15-30 minutes. Rollback = flip DNS back; the AWS
   stack is untouched until step 7.
7. Teardown AWS after 48 h of clean operation: terraform destroy the
   composition except Route53, SES, the S3 backup bucket (keep for 30 days).

Double-run window: steps 3-6 keep both stacks up for about 7-10 days. At
shape A on AWS ($13-15/day) plus the GCP cluster (~$7-10/day), that is
$140-250 of overlap, before credits.

Total calendar: two to three weeks with crews doing the work and the owner
clicking through steps 1 and 6. The ops knowledge in the operator notes,
runbooks and memory (SSM paths, ASG recipes, NAT routes) is retired and
rewritten.

## 8. Risks and unknowns

- Egress is dearer on GCP ($0.12/GB vs $0.09/GB); our egress fell sharply
  after A4/A5 but is not yet measured on a quiet day. Standard network tier
  helps but removes the global anycast front.
- Autopilot restrictions (hostPath, privileged, DaemonSet limits, no
  local-path, its own PodSecurity) make option 5a a chart rewrite; it is also
  the most expensive at rest. Only Standard or GCE are realistic.
- Quotas: a new project starts with small CPU quotas (often 24-32 vCPU per
  region) and Spot quotas; CI runners at 4 vCPU each can hit them. Request
  increases early.
- The lambdamicrovms client (15 import sites) has no GCP counterpart; the
  code paths need a feature flag or removal.
- S3 compatibility: GCS's XML API covers most of the SDK calls but not all
  (multipart quirks, presigned URL formats, SSE headers). The session-resume
  path and the postgres dump path need a real test.
- SES stays in sandbox until the owner's case is answered; moving clouds does
  not change that.
- GKE zonal cluster = single control plane; a maintenance event is a short
  API outage, not data loss. Regional clusters cost the $73/mo fee.
- Credits may not exist or may already be ticking.
- Two stacks for a week doubles the attack surface and the operator's
  attention; the owner is the only reviewer.

## 9. Recommendation

Do not migrate for cost. The cheapest honest GCP shape (GKE Standard, one
e2-standard-4 plus a Spot pool) lands at $270-300/month, the same as the AWS
floor we can reach in two crew tasks, and above the AWS single-node shape at
$165-180/month, because GCP egress and small VMs are not cheaper than AWS's.
Credits only defer the bill by one to two months.

Migrate if the owner wants GCP fluency and the "runs on GKE" claim badly
enough to spend two to three weeks of crew time and $140-250 of overlap on it,
and in that case choose GKE Standard (5b), not Autopilot. Before that, finish
the AWS floor (right-size requests, gate CI image builds), because that work
is needed on either cloud and halves the bill now.

A middle path that serves the career goal without the migration: run the
ClaimLayer demo environment on GKE Standard (one small node, $50-70/month,
possibly inside the free trial) and keep gardenio on AWS. That yields the
GKE experience and a portable-chart proof without moving production.

## 10. Open questions for the owner

1. Is there a credit balance on the homehealthtpa billing account, and when
   did its 90-day trial start? (Console: Billing > Credits.)
2. Is "runs on GKE" a product claim you want to make this quarter, or is GCP
   fluency the real goal (then the ClaimLayer-on-GKE path is cheaper)?
3. Is a 15-30 minute cutover outage and a week of double running acceptable?
4. Keep Route53 and SES on AWS after a move, or move everything?
5. Single zonal GKE cluster (free fee, one control plane) or regional
   ($73/month)?
6. Should the AWS single-node shape be done first regardless, since it is the
   cheapest number on either cloud?
