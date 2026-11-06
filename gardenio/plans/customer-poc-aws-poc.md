# customer-poc AWS POC — runbook (as-built) + acceptance checklist

> **Runbook status (2026-05-24):** this document is now a **live runbook** for the
> customer-poc AWS POC cluster in account `123456789012` (us-east-1). The §7
> acceptance checklist is the **operator validation flow for any re-apply** — every
> box is independently verifiable from a fresh checkout of `gardenio/terraform/composition/poc/`.
> Implementation work is tracked under the AWS implementation epic **ka-0000002270**
> (sibling of the research epic ka-0000005972). Per §7.9 hand-off item 2, the doc
> below reflects the **as-built** POC (hostname, provider pins, node-group sizing,
> deviations from §3) — not the original plan-of-record.

**Status:** runbook (operational — re-apply path for the customer-poc POC).
**Tracking bead:** ka-0000005971.
**Implementation epic:** [[ka-0000002270]] — "gardenio customer-poc AWS implementation" (sibling of ka-0000005972).
**Research epic:** ka-0000005972 — "gardenio customer-poc cluster + reusable Terraform module".
**Sibling plan:** `gardenio/plans/canonical-prod-vultr-poc.md` (ka-0000006197) — the portability proof; this doc is its AWS twin.

**Naming note (renamed 2026-05-15 per ka-0000007122):** the program previously called "canonical-prod" has been renamed to "customer-poc". The dedicated AWS account is `gardenio-ai` (not `gardenio-prod-poc`). Federated/role credentials for the new account are stored in AWS Secrets Manager as `gardenio/customer-poc-aws-creds` (the SM secret itself is hosted in the fleet account 234567890123 us-east-1, per recipe-1 of `docs/research/customer-poc-aws-cred-federation.md`). Predecessor research-doc filenames under `docs/research/canonical-prod-*.md` retain their original names for git-history continuity; only this plan-of-record and bead titles/descriptions were renamed.

**Edge-auth decree (2026-05-21 per decision ka-0000006859):** the operator decreed that the **GitLab AWS VPN SSO validated connection** (the production approach) is the **canonical edge-auth solution** for gardenio clusters; Keycloak is **NOT** part of the canonical stack going forward. Verbatim quote: *"If we don't need Keycloak for the Gitlab AWS vpn sso validated connection thingy we use in prod, which shall be the canonical solution (I here decree, update all plans), then we don't want in our cluster"*. The new reference doc is `docs/research/customer-poc-edge-auth-canonical.md`. **D-6** below is updated accordingly; the predecessor doc `docs/research/canonical-prod-keycloak-placement.md` is marked deprecated. Chart cleanup (suppressing Keycloak CR emission) is owned by sibling task **ka-0000004958** — not by this plan.

**Apply-gating decree (2026-05-29 per operator directive ka-0000008263):** **`aws-poc` applies are NOT operator-gated.** The customer-poc POC account `123456789012` is **gardenio's own AWS account** — agents own it end-to-end, per the standing rule *"no operator is coming to save you"* (advice ka-0000003508). Verbatim: *"we have to make aws-poc applies NOT be operator gated; agents keep thinking it is but aws-poc is for gardenio"*. Therefore: **routine `terraform apply` against `123456789012` (re-applying a chart bump, reconciling drift, applying an already-merged tier change) is agent-driven — do NOT file a `gb decision` and do NOT wait for an operator to approve it.** The per-tier `gb decision` language elsewhere in this doc (§5, §6, the superseded Runbook-gate note) is **historical** and does not bind routine re-applies. The **only** remaining operator-firewall items are the §8 list: account / Identity-Center bootstrap, Secrets-Manager seed *values*, and state-backend bootstrap — never routine applies. Guardrail that *replaces* the decision gate: before any apply, run `terraform plan` and review it; if the plan shows **unexpected infra churn** (anything beyond the intended change), STOP and file a bug under the currency epic [[ka-0000005495]] instead of applying.

**Account-target retarget (2026-05-18 per decision ka-0000001393):** earlier revisions of this plan assumed the customer-poc POC target AWS account was `234567890123`. When D-7 crew (ka-0000006801) fetched the SM-delivered keys and ran `aws sts get-caller-identity`, the keys authenticated as IAM user `gardenio` in account **`123456789012`**, not `234567890123`. Operator Matt Baker resolved decision ka-0000001393 → **RETARGET**: the customer-poc POC target account is `123456789012`. The fleet account `234567890123` continues to host the SM secret and the `gardenio-agents-s3` IRSA reader role; all bootstrap-tier creates land in `123456789012`. Any remaining `234567890123` references in this doc refer to the fleet/SM-hosting account, not the POC target.
**Predecessor docs (customer-poc research, all under `docs/research/`):**
- Foundation: `canonical-prod-aws-account.md`, `canonical-prod-networking.md`, `canonical-prod-tf-module-architecture.md`, `canonical-prod-tf-module-skeleton.md`, `canonical-prod-module-io-contract.md`.
- Cluster core: `canonical-prod-eks.md`, `canonical-prod-eks-addon-pinning.md`, `canonical-prod-karpenter-vs-ca.md`, `canonical-prod-irsa-vs-pod-identity.md`, `canonical-prod-irsa-policies.md`, `canonical-prod-stateful-services.md`, `canonical-prod-secrets-manager.md`.
- Platform + identity + observability: `canonical-prod-platform.md`, `canonical-prod-batteries-included.md`, `canonical-prod-operator-placement-matrix.md`, `canonical-prod-identity.md` (Keycloak placement section superseded — see edge-auth decree below), `canonical-prod-keycloak-placement.md` (**deprecated** — superseded by `docs/research/customer-poc-edge-auth-canonical.md`), `customer-poc-edge-auth-canonical.md` (canonical edge-auth = GitLab AWS VPN SSO), `canonical-prod-observability.md`, `canonical-prod-metrics-dashboards.md`, `canonical-prod-alerting-catalog.md`.
- Invocation + cutover + cost: `canonical-prod-site-deployment-invocation.md`, `canonical-prod-per-env-skeleton.md`, `canonical-prod-apply-pipeline.md`, `canonical-prod-tf-secret-injection.md`, `canonical-prod-migration-runbook.md`, `canonical-prod-live-dependency-inventory.md`, `canonical-prod-dns-tls-cutover.md`, `canonical-prod-beads-db-cutover.md`, `canonical-prod-stateful-migration.md`, `canonical-prod-cost-estimate.md`, `canonical-prod-cost-delta.md`, `canonical-prod-finops-plan.md`, `canonical-prod-tenant-system-chart.md`.
- Round-2 plan-of-record: `gardenio/plans/canonical-prod-round2-refinement.md`.

> **Runbook gate (2026-05-24, superseded 2026-05-29):** the historical "no-implementation guardrail" has been satisfied. The Terraform composition under `gardenio/terraform/composition/poc/` is the as-built scaffold; tiers (a)–(e) have been applied at least once against account `123456789012` per epic [[ka-0000002270]]. ~~Per-tier `gb decision` gates **still apply** to any re-apply that would touch live state~~ — **superseded by the Apply-gating decree above (ka-0000008263): routine re-applies are agent-driven and NOT operator-gated.** For an out-of-band full rebuild from scratch, still follow §5 in tier order, but the agent owns the applies; the gate is now `terraform plan` review (stop on unexpected churn), not a `gb decision`. The §7 checklist is the post-apply validation flow.

---

## 1. TL;DR

The AWS POC is the **first concrete implementation slice** of the customer-poc gardenio module in a fresh, gardenio-dedicated AWS account. One operator, one workstation, one region — `terraform apply` (of the root module that lands as the implementation answer to ka-0000007733) stands up an EKS cluster with managed Postgres, S3, a real Route53 hostname, a valid ACM cert, IRSA wired through every addon and workload, and the gardenio chart running end-to-end. A human ticks every box in §7 to call it a success.

**In scope.** A single-region (`us-east-1`), 3-AZ AWS environment running:

- **EKS 1.30** managed cluster — public + private API endpoint, IRSA enabled, control-plane logs to CloudWatch.
- **Two node groups**:
  - **Infra pool** — 3 × `m5.large` on-demand managed node group, one node per AZ, runs cluster addons + gardenio control-plane pods.
  - **Karpenter-managed agent pool** — `m5.2xlarge` (and family fallbacks) on Spot, taints + tolerations so only gardenio agent pods land here.
- **Multi-AZ Amazon RDS for PostgreSQL 16** with the **pgvector** extension enabled — backs both the beads daemon and the gardenio controller (no in-cluster Postgres in this POC, contra the Vultr POC).
- **In-cluster NATS** (gardenio chart's bundled NATS — same code path as Vultr POC and america-e2e-eks today).
- **S3 bucket `gardenio-sessions-aws-poc`** — KMS-SSE with an account-scoped CMK, server-side encryption only (no PII allowance in the POC).
- **Platform addons** (Helm-installed per `canonical-prod-platform.md` order):
  - AWS Load Balancer Controller (IRSA-bound, manages an internet-facing NLB for the gardenio `Service type=LoadBalancer`).
  - external-dns (IRSA-bound, scoped to one Route53 hosted zone).
  - external-secrets-operator with an `aws-secrets-manager` `ClusterSecretStore` (real backing store, unlike the Vultr POC where the store is unwired).
  - cert-manager with an ACM-bound `ClusterIssuer` (DNS-01 against Route53 — wildcards allowed) plus a Let's-Encrypt fallback `ClusterIssuer` for cross-validation per §7.
  - VictoriaMetrics stack (vm-operator + vmsingle + vmagent) — in-cluster only; CloudWatch is observed but not depended on.
- **Route53 public hosted zone** — as-built: 2-label parent zone `gardenio.acme.example` (per deviation §3.7 #2), with NS delegation from the existing `acme.example` apex; per-env workload domains (`aws-poc.gardenio.acme.example`, future `prod`, etc.) are records inside the same zone.
- **ACM certificate** issued via **DNS-01 validation** through the same Route53 zone, attached to the NLB.
- **IRSA** (NOT EKS Pod Identity) for every addon and every gardenio workload that touches AWS APIs — chosen by the existing research (see §3 and **D-3** in §6).
- **gardenio controller + coopmux + slack-bridge** — the standard gardenio Helm chart, deployed with an AWS-flavored values overlay.

**Out of scope (cutline — §4).** No multi-region. No production cutover from `america-e2e-eks`. No Slack/GitLab/Keycloak cutover. No beads DB data migration. No CI-dispatched `terraform apply` (apply runs from an operator workstation only). No Mezmo cross-account log shipping. No second-cloud target. No GitOps (Argo / Flux).

**Done when.** Every box in §7 is ticked by a human reviewer working from a fresh checkout, and one `gb decision` ("AWS POC ready to leave research and enter implementation") resolves `approve` per the umbrella epic's done-when.

---

## 2. POC architecture (one diagram, no surprises)

```
                  acme.example (apex, Route53 — existing acme zone)
                         │
                         │ NS delegation: gardenio.acme.example (2-label parent zone)
                         ▼
   ┌──────────────────────────────────────────────────────────────────────────┐
   │ AWS account: gardenio-ai (acct 123456789012, us-east-1)                   │
   │                                                                           │
   │   Route53 public zone: gardenio.acme.example (parent, per-env records inside)  │
   │     └── A/AAAA: aws-poc.gardenio.acme.example → NLB                            │
   │   ACM cert (DNS-01 validation) — *.gardenio.acme.example + apex SAN            │
   │                                                                           │
   │   Identity Center (account-scoped) — operator login, no IAM users        │
   │                                                                           │
   │   ┌─ VPC 10.70.0.0/16 ──────────────────────────────────────────────┐   │
   │   │                                                                   │   │
   │   │   AZ us-east-1a       AZ us-east-1b       AZ us-east-1c          │   │
   │   │   ┌──────────────┐    ┌──────────────┐    ┌──────────────┐      │   │
   │   │   │ public sn    │    │ public sn    │    │ public sn    │ NLB  │   │
   │   │   │ /20          │    │ /20          │    │ /20          │ tgts │   │
   │   │   ├──────────────┤    ├──────────────┤    ├──────────────┤      │   │
   │   │   │ private sn   │    │ private sn   │    │ private sn   │ EKS  │   │
   │   │   │ /19          │    │ /19          │    │ /19          │ pods │   │
   │   │   ├──────────────┤    ├──────────────┤    ├──────────────┤      │   │
   │   │   │ db sn /24    │    │ db sn /24    │    │ db sn /24    │ RDS  │   │
   │   │   └──────────────┘    └──────────────┘    └──────────────┘      │   │
   │   │                                                                   │   │
   │   │   1× NAT gateway per AZ (3 total — HA per AZ, per §3.2)          │   │
   │   │                                                                   │   │
   │   │   ┌─────────────────────────────────────────────────────────┐   │   │
   │   │   │ EKS 1.30 cluster (managed control plane, IRSA enabled)  │   │   │
   │   │   │                                                          │   │   │
   │   │   │  Infra MNG: 3× m5.large on-demand (one per AZ)          │   │   │
   │   │   │    runs: AWS-LB-Controller, external-dns,               │   │   │
   │   │   │          external-secrets, cert-manager,                │   │   │
   │   │   │          VictoriaMetrics, Karpenter,                    │   │   │
   │   │   │          gardenio-controller, coopmux, slack-bridge,     │   │   │
   │   │   │          NATS (in-cluster)                              │   │   │
   │   │   │                                                          │   │   │
   │   │   │  Karpenter NodePool (agent-pool):                       │   │   │
   │   │   │    m5.2xlarge Spot, taint=gardenio.agent=true:NoSchedule │   │   │
   │   │   │    scales 0→N on agent-pod demand                       │   │   │
   │   │   │                                                          │   │   │
   │   │   │  Service type=LoadBalancer → NLB (internet-facing,      │   │   │
   │   │   │    AWS-LB-Controller annotation,                        │   │   │
   │   │   │    ACM cert attached, target type=ip)                   │   │   │
   │   │   └─────────────────────────────────────────────────────────┘   │   │
   │   │                                                                   │   │
   │   │   RDS for PostgreSQL 16 (Multi-AZ) — db subnet group              │   │
   │   │     extension: pgvector — backs beads + gardenio-controller        │   │
   │   │                                                                   │   │
   │   └───────────────────────────────────────────────────────────────────┘   │
   │                                                                           │
   │   S3 bucket: gardenio-sessions-aws-poc (KMS-SSE, CMK)                     │
   │   Secrets Manager: /gardenio/aws-poc/* (Slack, GitLab, LiteLLM tokens)    │
   │   KMS CMKs: one per bounded data class (RDS, S3, Secrets Manager)       │
   │   ECR (consumer-only in POC) — pulls from registry.gitlab.com            │
   │                                                                           │
   └──────────────────────────────────────────────────────────────────────────┘
```

**Mirror to the Vultr POC.** Inside the cluster, the deployed shape is identical: gardenio chart, beads daemon, agent pods, coopmux, slack-bridge. The seam is at the provider layer: EKS replaces VKE, RDS replaces in-cluster Postgres, S3 replaces Vultr Object Storage, NLB (via AWS-LB-Controller) replaces the in-tree Vultr CCM LB, Route53 + ACM replaces Vultr DNS + Let's-Encrypt HTTP-01.

---

## 3. What gets provisioned

The POC is one Terraform root module (`gardenio/terraform/composition/poc/`), one workspace, one state file. State backend lives in this account: `s3://gardenio-customer-poc-tfstate/composition/poc/terraform.tfstate` with DynamoDB lock table `gardenio-customer-poc-tflock` (bootstrapped via `gardenio/terraform/bootstrap/`). No cross-account assumed-role for the POC; the operator runs apply against the AKIA stored in Secrets Manager `gardenio/customer-poc-aws-creds` (customer-poc POC account `123456789012` cannot use STS — see advice **ka-0000007102**).

### 3.0 Provider pins (as-built)

Defined in `gardenio/terraform/composition/poc/versions.tf`:

| Provider | Source | Version constraint |
|---|---|---|
| terraform core | — | `>= 1.6` |
| aws | `hashicorp/aws` | `~> 5.0` |
| tls | `hashicorp/tls` | `~> 4.0` |
| helm | `hashicorp/helm` | `~> 2.13` |
| kubernetes | `hashicorp/kubernetes` | `~> 2.27` |
| kubectl | `gavinbunney/kubectl` | `~> 1.14` |

Karpenter chart pin: **v1.3.3** (`oci://public.ecr.aws/karpenter`, defined in `modules/karpenter/variables.tf`).
gardenio Helm chart pin: source of truth is `locals.chart_version_default` in `modules/gardenio-chart/main.tf` — read that local for the current value (it is intentionally not restated here, since a literal copy drifts on every pin bump). Overridable at apply time via `gardenio_chart_version` tfvar. The pin tracks the **latest golden (E2E-verified, non-`-rc`) chart release** and is kept current by the AWS-POC currency gardener / formula (tracking bead ka-0000002475); any pin bump is a code-only change and re-apply remains gated per §5 tier (e).

### 3.1 AWS resources (Terraform-managed)

Citations point at the research doc that committed the concrete choice; cells without a citation are decisions still open in §6.

| Resource | AWS type | Count | Source |
|---|---|---|---|
| VPC | `aws_vpc` | 1 | `canonical-prod-networking.md` §2 |
| Subnets (public / private / db) | `aws_subnet` | 9 (3 per AZ × 3 AZs) | `canonical-prod-networking.md` §3 |
| NAT gateways | `aws_nat_gateway` | 3 (one per AZ, see §3.2) | `canonical-prod-networking.md` §4 / **D-4** |
| Internet gateway | `aws_internet_gateway` | 1 | `canonical-prod-networking.md` |
| VPC endpoints (S3 gateway + interface set) | `aws_vpc_endpoint` | per `canonical-prod-networking.md` §5 | networking doc |
| EKS cluster | `aws_eks_cluster` | 1 (Kubernetes **1.30**, `authentication_mode = API_AND_CONFIG_MAP`, `endpoint_public_access = true` w/ CIDR `0.0.0.0/0` per decision **ka-0000003006** — POC posture is "public-open" since account `123456789012` is isolated and has no prod data; control-plane logs all 5 types) | `canonical-prod-eks.md` §2 + as-built `composition/poc/main.tf` |
| Infra managed node group | `aws_eks_node_group` | 1 (`m5.large` on-demand, desired=3 / min=3 / **max=6**, AL2023_x86_64_STANDARD, 80 GiB root EBS) — see deviations §3.7 | `canonical-prod-eks.md` §3 + as-built `modules/eks/variables.tf` |
| Karpenter controller install | `helm_release` (`karpenter`) | 1 | `canonical-prod-karpenter-vs-ca.md` |
| Karpenter `NodePool` (agent-pool, Spot, families=`[m5]`, sizes=`[2xlarge, xlarge, 4xlarge]`, taint `gardenio.agent=true:NoSchedule`, AL2023, 80 GiB root) | `kubectl_manifest` | 1 | `canonical-prod-karpenter-vs-ca.md` §4 + as-built `modules/karpenter/variables.tf` |
| EKS addons (kube-proxy, coredns, vpc-cni, aws-ebs-csi-driver) | `aws_eks_addon` | 4 | `canonical-prod-eks-addon-pinning.md` |
| RDS Postgres **16.4** (Multi-AZ instance, `db.t3.medium`, 100 GiB → 400 GiB autoscale, 7-day backups, master-password managed by Secrets Manager, **not** Aurora — see **D-5**) | `aws_db_instance` + `aws_db_subnet_group` + `aws_db_parameter_group` | 1 | as-built `composition/poc/variables.tf` / `stateful.tf` |
| S3 bucket | `aws_s3_bucket` (+ versioning, lifecycle, public-access block, SSE) | 1 | `canonical-prod-stateful-services.md` §3 |
| KMS CMKs (RDS, S3, Secrets Manager) | `aws_kms_key` + `aws_kms_alias` | 3 | `canonical-prod-stateful-services.md` §4 / `canonical-prod-secrets-manager.md` |
| Secrets Manager path | `aws_secretsmanager_secret` | per ESO inventory (see `canonical-prod-secrets-manager.md` §3) | `canonical-prod-secrets-manager.md` |
| Route53 hosted zone | `aws_route53_zone` | 1 — **2-label parent zone `gardenio.acme.example`** (per 2026-05-21 operator pivot — see deviations §3.7); per-env workload subdomains (`aws-poc`, future `prod`, future tenants) are **records inside** this zone, not their own delegated sub-zones | `canonical-prod-dns-tls-cutover.md` §2 + as-built `modules/route53/zone.tf` |
| ACM cert (DNS-01 validation) | `aws_acm_certificate` + `aws_acm_certificate_validation` | 1 wildcard + apex SAN for workload domain **`aws-poc.gardenio.acme.example`** | `canonical-prod-dns-tls-cutover.md` §3 + as-built `composition/poc/dns.tf` |
| OIDC provider (cluster) | `aws_iam_openid_connect_provider` | 1 | `canonical-prod-irsa-vs-pod-identity.md` |
| IRSA roles per workload | `aws_iam_role` + `aws_iam_role_policy(_attachment)` | per `canonical-prod-irsa-policies.md` | `canonical-prod-irsa-policies.md` |
| ECR pull-through cache (optional) | `aws_ecr_pull_through_cache_rule` | 0–1 (out by default, see §4) | `canonical-prod-aws-account.md` |

**Not** provisioned by Terraform in the POC (operator pre-creates or out-of-band):

- The AWS account itself, AWS Organizations placement, and Identity Center bootstrap — see §8 and **D-1**.
- The Route53 NS-delegation record in the existing `acme.example` apex zone — out-of-band Route53 change, **D-2**.
- IAM users — there are none; all operator access goes through Identity Center permission sets.
- The state backend bucket + DynamoDB table — bootstrapped once via the procedure documented in `canonical-prod-tf-module-architecture.md` §5 (a `terraform/bootstrap/` mini-root that runs with `local` backend, then a one-time `terraform state mv` migration). Captured as **D-7** in §6.

### 3.2 Networking notes

- **CIDR:** `10.70.0.0/16` for the POC VPC. `america-e2e-eks` and the Vultr POC do not overlap (`10.50.0.0/16` and `10.60.0.0/16` respectively).
- **NAT HA:** one NAT gateway per AZ (3 total). The cost-vs-availability fork in `canonical-prod-networking.md` §4 picks per-AZ NAT for the POC; if the FinOps plan (ka-0000008238) later says "single NAT for non-prod," it becomes a values knob in the implementation epic, not a POC change. See **D-4**.
- **VPC endpoints:** S3 gateway endpoint (free) is in; the interface-endpoint set (STS, EC2, ECR API/dkr, Logs, Secrets Manager) is gated on whether the POC's egress-volume math justifies the per-endpoint hourly cost — see **D-4**.
- **Pod CIDR / VPC CNI:** standard VPC-CNI with prefix delegation (per `canonical-prod-eks.md` §4).

### 3.3 Cluster-internal install order (Helm)

Mirrors `canonical-prod-platform.md` §3. All releases run from the Terraform-managed Helm provider in the POC — not Argo, not Flux, no GitOps loop in the POC.

1. **CRDs** — cert-manager, external-secrets, AWS-LB-Controller, victoria-metrics-operator, Karpenter.
2. **AWS Load Balancer Controller** (IRSA-bound), then verify it can reconcile a test `Service type=LoadBalancer`.
3. **Karpenter** (IRSA-bound; the EKS managed node group already exists, Karpenter is for the agent pool only).
4. **cert-manager** + Route53-DNS-01 `ClusterIssuer` (IRSA-bound).
5. **external-dns** (IRSA-bound, scoped to the POC Route53 zone).
6. **external-secrets** + `ClusterSecretStore` of type `aws-secrets-manager` (IRSA-bound to the path `/gardenio/aws-poc/*`).
7. **VictoriaMetrics stack** — vm-operator + one VMSingle + a VMAgent that scrapes the gardenio namespace. No Grafana in POC; access via `kubectl port-forward` or VM UI.
8. **gardenio chart** (`helm/gardenio/`) with the AWS overlay (see §3.5).

### 3.4 RDS + pgvector

The POC uses **Amazon RDS for PostgreSQL 16.4, Multi-AZ instance** (not Aurora — see **D-5**) with the pgvector extension enabled via a `aws_db_parameter_group` that includes `pgvector` in `shared_preload_libraries`, then a one-time `CREATE EXTENSION vector;` against the gardenio database (run by the controller's init job, not Terraform). Citation: `canonical-prod-stateful-services.md` §2.

- **As-built instance:** `db.t3.medium`, 100 GiB initial allocated storage, 400 GiB autoscale ceiling, master-password managed by Secrets Manager (`master_user_secret_arn` wired through ESO IRSA via `additional_secret_arns` in `composition/poc/platform.tf` — resolves **ka-0000003494**).
- **Why RDS-managed (contra the Vultr POC's in-cluster Postgres):** AWS RDS lists pgvector as a supported extension on PG ≥ 15.5. The gap that forced in-cluster Postgres on Vultr (no managed pgvector on Vultr Managed Postgres) does not exist here.
- **Why Multi-AZ instance, not Aurora:** the Aurora-vs-RDS fork is filed as **D-5**; until that resolves, the POC tracks the lower-cost, simpler-failover RDS Multi-AZ instance shape.
- **Backups:** automated backups, 7-day retention, no PITR-extended retention. Cross-region backups are explicitly out (§4).

### 3.5 Gardenio chart AWS overlay

A new `gardenio/helm/gardenio/values-aws-poc.yaml` (or, more likely, a `site-deployment/<env>/aws-poc/values.yaml` once the per-env skeleton from `canonical-prod-per-env-skeleton.md` is real) overrides the defaults:

| Key | POC value | Why |
|---|---|---|
| `postgres.enabled` | `false` | RDS provides the DB. |
| `nats.enabled` | `true` | In-cluster NATS (same as Vultr POC and america-e2e-eks). |
| `agents.s3.bucket` | `gardenio-sessions-aws-poc` | S3 bucket from §3.1. |
| `agents.s3.endpoint` | unset (use AWS default) | The chart's `COOP_S3_ENDPOINT` is for MinIO/Vultr; AWS S3 is the default. |
| `agents.s3.region` | `us-east-1` | Single-region POC. |
| `ingress.tls.issuer` | `acm` (NLB attaches the ACM cert directly) | DNS-01 wildcard via Route53. |
| `ingress.host` | **`aws-poc.gardenio.acme.example`** (as-built workload domain, from `local.workload_domain` in `composition/poc/dns.tf` = `${var.env_subdomain}.${var.parent_zone_name}`) | Single hostname for POC. See deviations §3.7. |
| Service annotations | AWS-LB-Controller NLB annotations (`service.beta.kubernetes.io/aws-load-balancer-type: external`, `…/aws-load-balancer-nlb-target-type: ip`, ACM cert ARN) | Standard NLB-with-ACM shape. |
| Service-account annotations | `eks.amazonaws.com/role-arn: <controller-irsa-role>` and similar for every workload that talks to AWS APIs | IRSA, not Pod Identity (see **D-3**). |
| `karpenterAgentPool.enabled` | `true` | Toggles the Karpenter `NodePool` for agents. |

The overlay file's eventual home is the implementation epic's call (Epic 9 / `canonical-prod-site-deployment-invocation.md`); for the POC, the working path is `site-deployment/<env>/aws-poc/`.

### 3.6 IRSA, not Pod Identity

Per `canonical-prod-irsa-vs-pod-identity.md`, IRSA is the chosen identity-binding for the POC. Pod Identity is filed as a future migration (**D-3**) once a sibling task confirms feature parity for the gardenio addon set. The OIDC provider is created by Terraform; every addon + workload SA gets an `eks.amazonaws.com/role-arn` annotation pointing at its IRSA role.

**As-built OIDC issuer:** `https://oidc.eks.us-east-1.amazonaws.com/id/03460CBE015CE7B8799AD163F3107B89` (cluster `poc-gardenio-eks`).

**As-built IRSA roles** (composition `gardenio-poc-*`):
- `gardenio-poc-agent` — `gardenio-system:gardenio-agent` SA. S3 read/write on `gardenio-sessions-aws-poc` + KMS GenerateDataKey/Decrypt/DescribeKey on the s3 CMK. Defined in `composition/poc/agent_irsa.tf` — landed via MR !2966 / **ka-0000003915**. See §3.7 deviations.
- Platform addons (AWS LB Controller, external-dns, ESO, cert-manager, Karpenter) — IRSA roles defined in `modules/platform/` and `modules/karpenter/`, all bound to `oidc_issuer_host` from `modules/eks`.
- `aws-ebs-csi-driver` — IRSA role provisioned by `modules/eks` (only core addon with built-in IRSA SA).

### 3.7 As-built deviations from §3 (recorded 2026-05-24)

The following deviations from the original §3 plan-of-record surfaced during tier (a)–(e) apply and are now the **as-built shape**. Any re-apply must reproduce these; the next round of `terraform plan` against the current `composition/poc/` produces zero unexpected drift (verified twice — see **ka-0000001980** triage report).

1. **Target account retarget.** Original plan assumed `234567890123` (customer-poc fleet account). Operator decision **ka-0000001393** (2026-05-18) retargeted to `123456789012` after the SM-delivered AKIA authenticated as IAM user `gardenio` in that account. `234567890123` remains the fleet/SM-hosting account; all POC infra lives in `123456789012`. Credential surface: AKIA from `gardenio/customer-poc-aws-creds` (STS is **not viable** in this account — advice **ka-0000007102**).
2. **Workload hostname.** Original plan: `gardenio.aws-poc.gardenio.app.acme.example` (4-label, dedicated per-env delegated zone). As-built: **`aws-poc.gardenio.acme.example`** (3-label, record inside the 2-label parent zone `gardenio.acme.example` owned by the POC account). Driven by the 2026-05-21 operator pivot — see `composition/poc/README.md` and `modules/route53/zone.tf` header comments. Per-env workload subdomains (`prod`, future tenants) are additional records inside the same parent zone — no new NS delegation per env.
3. **Infra MNG max-size = 6 (not pinned at 3).** Original plan: "3 × `m5.large`, one per AZ". As-built: `desired_size = 3`, `min_size = 3`, `max_size = 6` (`modules/eks/variables.tf`) with `lifecycle.ignore_changes = [scaling_config[0].desired_size]` to let in-cluster autoscalers grow the pool without TF drift. Disk: 80 GiB root EBS (`gp3` via AL2023 default). AMI: `AL2023_x86_64_STANDARD`.
4. **Karpenter NodePool — multi-size, not single-size.** Original plan: "`m5.2xlarge` (and family fallbacks)". As-built: families `[m5]`, sizes `[2xlarge, xlarge, 4xlarge]`, capacity `[spot]`, architecture `amd64`, AL2023 AMI family, 80 GiB root EBS. Taint `gardenio.agent=true:NoSchedule`. CPU limit 1000, memory limit 1000Gi. NodePool name `agent-pool`. Karpenter chart pinned to `v1.3.3` (`modules/karpenter/variables.tf`).
5. **EKS posture is public-open.** Per decision **ka-0000003006**, the POC cluster runs with `endpoint_public_access = true` and `public_access_cidrs = ["0.0.0.0/0"]`. The operator workflow runs from agent pods outside the VPC, and the account has no production data. Production environments **must** override these to a private endpoint + narrow CIDR list. `authentication_mode = API_AND_CONFIG_MAP` is set at create time and cannot be narrowed in place (would force replacement); the cluster creator IAM principal (`gardenio` user) gets admin via `bootstrap_cluster_creator_admin_permissions`.
6. **EKS addons — `bootstrap_self_managed_addons = false` + split network/workload addon resources.** `aws_eks_addon.network` (`vpc-cni`, `kube-proxy`) depends only on the cluster — these are pre-node networking that the MNG needs before nodes can reach Ready. `aws_eks_addon.this` (`coredns`, `aws-ebs-csi-driver`) depends on the node group — they need a node to schedule on. Resolves the deadlock that surfaced in **ka-0000000480**. State-migration `moved {}` blocks let existing clusters re-key without destroy/recreate.
7. **gardenio-agent IRSA shape (MR !2966 / [[ka-0000003915]]).** Original plan said "every gardenio workload that touches AWS APIs" gets IRSA but didn't enumerate the agent SA. As-built `composition/poc/agent_irsa.tf` provisions `gardenio-poc-agent` IAM role with: (a) `s3:PutObject,GetObject,DeleteObject,AbortMultipartUpload` on `arn:aws:s3:::gardenio-sessions-aws-poc/*`; (b) `s3:ListBucket` on the bucket arn; (c) `kms:GenerateDataKey,Decrypt,DescribeKey` on the s3 CMK from `modules/kms-cmks` (the bucket is SSE-KMS). Trust policy is OIDC `system:serviceaccount:gardenio-system:gardenio-agent` with aud `sts.amazonaws.com`. The role arn is threaded via `agent_service_account_role_arn` into `modules/gardenio-chart` which annotates the SA. Discovered when the §7.5 in-cluster smoke (**ka-0000007584**) had to inject static AKIA creds into a K8s Secret; the as-built shape removes that workaround.
8. **ESO IRSA widened for RDS-managed master password.** `modules/platform` ESO IRSA includes `additional_secret_arns = [module.rds.master_user_secret_arn]` and `additional_kms_key_arns = [module.kms_cmks.rds_key_arn]` so the `gardenio-postgres-external-password` ExternalSecret can resolve the AWS-managed `rds!*` secret (outside `/gardenio/*`). Resolves **ka-0000003494** (option `wire-explicit-arn`). ESO secrets prefix is `/gardenio/aws-poc` (runbook literal) with `/gardenio/poc` retained as transitional fallback per decision **ka-0000005331**.
9. **Drift triage (ka-0000001980) — clear.** The originally-reported drifts on `module.gardenio.helm_release` and `module.platform.kubectl_manifest.cert_manager_cluster_issuer[0]` were transient (helm release settling + kubectl-provider server-side re-read) and no longer reproduce on two consecutive plans against current main. **Recommendation: no `lifecycle.ignore_changes` pinning** — adding it would over-fit to transient signal. The only pending TF change at triage time was `module.platform.kubectl_manifest.vm_single[0]` adding `storageClassName: gp2` — intentional, owned by **ka-0000006803** (merged, awaits operator apply).
10. **VictoriaMetrics single-node** (no clustered VM). VMSingle + VMAgent only; vm-operator manages both. No Grafana in POC.

---

## 4. Cutline — what is explicitly NOT in the POC

Each line below is a deliberate "no" so the POC can ship on a single operator's apply session, not on the production timeline. Anything here that we eventually want is a follow-up epic, **not** a stretch goal of the POC.

| Out of scope | Why deferred | Re-opens under |
|---|---|---|
| Multi-region | Not the POC's goal; multi-region requires a separate networking + RDS-replica design. | Future "AWS multi-region" epic; not committed. |
| Full beads-DB data migration from america-e2e-eks | Owned by Epic 10 (ka-0000001529) tasks ka-0000007634 + ka-0000002179; POC is **greenfield only** (`kd create` from an empty schema). | Epic 10 implementation epic. |
| Slack app cutover | Owned by Epic 7 (ka-0000007440) task ka-0000006653. POC uses an isolated Slack app + channel, not the production app. | Epic 7. |
| GitLab OAuth + webhook cutover | Owned by Epic 7 task ka-0000006074. POC reads from GitLab via a scoped PAT, not a cutover OAuth app. | Epic 7. |
| Keycloak in the canonical stack | **Removed by operator decree on ka-0000006859** (2026-05-21). Canonical edge-auth = GitLab AWS VPN SSO validated connection. See `docs/research/customer-poc-edge-auth-canonical.md` and **D-6** below. POC remains Slack-only. | Chart cleanup owned by ka-0000004958; Terraform overlay updates by sibling task. |
| In-cluster vs managed observability backends | POC uses VictoriaMetrics in-cluster only; CloudWatch logs are enabled at the EKS-control-plane level but not depended on. Mezmo cross-account ingest is out. | Epic 8 (ka-0000007067). |
| CI dispatch of `terraform apply` | Apply runs from an operator workstation only. No GitLab-CI / RWX wiring for the POC. | Epic 9 task ka-0000003917. |
| GitOps (Argo / Flux) | POC uses `helm_release` from Terraform; GitOps is a follow-on. | Future operational-tooling task. |
| Aurora vs RDS final pick | Filed as **D-5**; POC tracks RDS Multi-AZ instance. | Epic 5 task ka-0000001656. |
| Wildcard cert split per service | One wildcard ACM cert is enough for the POC; per-service cert split is production hardening. | Future hardening task. |
| Mezmo / centralized log shipping | POC stays at CloudWatch + in-cluster VictoriaMetrics. | Epic 8 task ka-0000005391. |
| Cross-account ECR consumer rules | POC pulls public images from `<registry>/gardenio/*`. ECR pull-through is filed but unused. | Future hardening task. |
| Tenant-system / multi-tenant chart | Epic 12 child ka-0000003395 — not the POC's job. | Epic 12. |
| acme-specific bleed (Slack channel IDs, GitLab project IDs, account-ID literals) | Module defaults stay portable per umbrella core rule 3. | Lives in the (future) `site-deployment/<env>/aws-poc/` overlay tfvars, never in the module. |
| Disaster-recovery testing / restore drill | Operational hardening, not POC scope. | Epic 11 / FinOps + operations follow-on. |

---

## 5. Implementation tiers

> **Gating note (2026-05-29, ka-0000008263):** the "each tier gated on a fresh `gb decision`" model below is **historical** and applied during the original greenfield bring-up. Per the Apply-gating decree at the top of this doc, **routine re-applies against gardenio's own account `123456789012` are agent-driven and NOT operator-gated.** The `**Gate:**` lines per tier below are retained for historical context; for any re-apply today the effective gate is **`terraform plan` review — stop and file a bug on unexpected churn**, not a `gb decision`. Tier (a) (account/Identity-Center/state-backend bootstrap) remains operator-only per §8.

The POC is **six tiers** of `terraform apply`. (Tiers (a)–(e) have already been applied against account `123456789012` per epic [[ka-0000002270]].) The tier breakdown below documents the as-built target and the original bring-up order; an agent re-applying follows the same order but owns the applies directly.

### Tier (a) — Account bootstrap

Manual, operator-only. **No agent context** sees AWS root credentials or the initial Identity Center bootstrap (see §8 for the firewall rules).

- Provision the dedicated AWS account (Organizations sibling — placement per `canonical-prod-aws-account.md`).
- Bootstrap Identity Center: directory, permission sets (`gardenio-admin`, `gardenio-readonly`), assignments.
- Create the cluster OIDC trust target — handled by Terraform later, **but** the account-level OIDC provider in IAM is created here.
- Create the state-backend bootstrap: S3 bucket + DynamoDB lock table for `terraform/customer-poc-aws-poc/` per `canonical-prod-tf-module-architecture.md` §5.

**Gate:** operator opens **D-1** (account procurement) + **D-7** (state-backend bootstrap procedure) `gb decision`s; both resolve `approve` before tier (b).

### Tier (b) — Networking + EKS

- Apply the networking module: VPC, 9 subnets, IGW, 3 NAT gateways, route tables, VPC endpoints (per **D-4** decision).
- Apply the EKS cluster module: control plane, addons (vpc-cni, kube-proxy, coredns, ebs-csi), infra managed node group, OIDC provider, IRSA trust policies.
- **Karpenter installs here** (Helm release from Terraform) so that the agent pool can come online before the gardenio chart.

**Gate:** `gb decision` "proceed to tier b apply" + **D-3** (IRSA vs Pod Identity) confirmed `irsa`.

### Tier (c) — RDS + S3 + Secrets Manager

- Apply the stateful-services module: RDS Multi-AZ Postgres 16, parameter group with pgvector, db subnet group, security groups locked to the EKS pod CIDR.
- S3 bucket for `gardenio-sessions-aws-poc` with KMS-SSE.
- Secrets Manager paths populated from operator-supplied tfvars (or a one-time bootstrap script — see §3.5 of `canonical-prod-tf-secret-injection.md`).
- KMS CMKs created here, not in tier (b), so the cluster IRSA roles can grant on them without a chicken-and-egg dependency.

**Gate:** `gb decision` "proceed to tier c apply" + **D-5** (Aurora vs RDS) confirmed `rds-multi-az-instance`.

### Tier (d) — Platform addons via Helm

Helm releases through the Terraform Helm provider, in the order listed in §3.3 (AWS-LB-Controller → cert-manager → external-dns → external-secrets → VictoriaMetrics).

- Each addon gets its IRSA role + `ClusterRoleBinding` from Terraform.
- ACM certificate request lands in this tier (validated via DNS-01 against the Route53 zone created in tier (e) — Terraform graph resolves the dependency).

**Gate:** `gb decision` "proceed to tier d apply".

### Tier (e) — Route53 hosted zone + ACM validation + gardenio chart deploy

- Apply the Route53 parent hosted zone `gardenio.acme.example` (as-built per deviation §3.7 #2).
- Apply the ACM certificate validation records (wildcard `*.gardenio.acme.example` + apex SAN).
- Apply the gardenio Helm release with the AWS overlay; workload host = `aws-poc.gardenio.acme.example`.
- external-dns reconciles the Service-LB hostname into the zone.

**Operator step (out of Terraform):** add the NS-delegation record for `gardenio.acme.example` to the existing `acme.example` apex zone (**D-2**). Wait for `dig +trace NS gardenio.acme.example` to show AWS nameservers.

**Gate:** `gb decision` "proceed to tier e apply" + **D-2** resolved.

### Tier (f) — Real DNS + TLS reachability check

No new Terraform resources. The operator walks the §7 checklist:

- `dig` resolves the gardenio hostname to the NLB.
- `curl` returns 200 with a valid certificate.
- A test Slack message reaches the gardenio-controller and a smoke agent runs end-to-end against the new RDS.

**Gate:** `gb decision` on ka-0000005971 (the tracking bead) — "AWS POC ready to leave research and enter implementation". This gate has been resolved for the initial apply (epic [[ka-0000002270]] is the implementation sibling of ka-0000005972); any re-apply opens a fresh decision.

### Tier (g) — Edge-auth port (beads-viewer + GitLab AWS VPN SSO)

Port the **canonical edge-auth model** (per **D-6** decree — see `docs/research/customer-poc-edge-auth-canonical.md`) into the customer-poc POC account `123456789012`: stand up a dedicated AWS Verified Access (AVA) instance + endpoint that gates `beads-viewer` on a POC-scoped hostname, configured to trust `gitlab.com` as the OIDC IdP — the same shape proven in production today (303 → `gitlab.com/oauth/authorize`, direct OIDC, **no Keycloak in the path**, verified working under task [[ka-0000000904]] on 2026-05-24).

This tier is the canonical edge-auth proof on the new account **before** prod cutover. It exists to surface any account-shape, IAM, or trust-provider differences while there is still no production data behind the cluster.

Decision provenance: this tier was authorized by operator response on decision [[ka-0000000470]] (resolved 2026-05-24 22:28Z, artifact_type=plan); see the task description on [[ka-0000003897]] for the verbatim choice. Parent epic: [[ka-0000002270]].

#### Pre-conditions (must be in place from tiers a–f)

- Tiers (a)–(f) all green on the §7 checklist for the POC cluster (current state per epic [[ka-0000002270]]).
- Route53 parent zone `gardenio.acme.example` is live in account `123456789012` and resolves end-to-end (§7.4 boxes ticked).
- ACM DNS-01 issuance is working in the POC zone (already proven by tier (e) — the wildcard `*.gardenio.acme.example` cert can cover the new edge host without re-validation).
- `beads-viewer` is deployed by the gardenio Helm release in `gardenio-system` (already wired in §3.3 / §3.5; this tier toggles its AVA-ingress overlay on).
- Operator has admin access to the `gloas-*` GitLab OAuth application — same app used in prod — and can append a new redirect URI per `docs/research/canonical-prod-identity-gitlab.md` §G1.
- A `gb decision` for tier (g) is open and resolves `approve` (see gate language below). **No AWS writes happen before that resolution.**

#### What gets provisioned

Terraform additions land under `gardenio/terraform/composition/poc/edge_auth.tf` (new file) and `modules/edge-auth/` (new module — mirror of whatever the prod fleet account ships, scoped down to one endpoint and one trust provider). The implementation MR — filed as a sibling task under [[ka-0000002270]] once this plan is merged — owns the module factoring; this doc commits the resource list.

| Resource | AWS type | Count | Notes |
|---|---|---|---|
| AVA instance | `aws_verifiedaccess_instance` | 1 | POC-scoped; lives in `123456789012`, **not** the fleet account `234567890123`. |
| AVA OIDC trust provider | `aws_verifiedaccess_trust_provider` | 1 | `type = "user"`, `device_trust_provider_type = null`, `user_trust_provider_type = "oidc"`. `oidc_options.issuer = "https://gitlab.com"`, `authorization_endpoint = "https://gitlab.com/oauth/authorize"`, `token_endpoint = "https://gitlab.com/oauth/token"`, `user_info_endpoint = "https://gitlab.com/oauth/userinfo"`, `scope = "openid profile email"`. `client_id` + `client_secret` resolve from Secrets Manager (`/gardenio/aws-poc/gitlab-ava-oidc`) — wired via ESO at apply time, **not** committed in tfvars. |
| AVA trust-provider attachment | `aws_verifiedaccess_instance_trust_provider_attachment` | 1 | Binds the OIDC trust provider to the AVA instance. |
| AVA group | `aws_verifiedaccess_group` | 1 | Default-allow `policy_document` for the POC — any `gitlab.com`-authenticated principal in the `gardenio-*` GitLab groups is allowed. Policy uses the same Cedar-style shape as the prod AVA group (carry-over from the fleet account); the POC group sits empty of explicit user pins. |
| AVA endpoint | `aws_verifiedaccess_endpoint` | 1 | `endpoint_type = "load-balancer"` targeting the NLB created in tier (d). `application_domain = "beads-viewer.aws-poc.gardenio.acme.example"`, `endpoint_domain_prefix = <auto-generated>`, `domain_certificate_arn = <ACM cert from tier (e), wildcard `*.gardenio.acme.example`>`. `security_group_ids` reference the same SG that fronts the NLB. |
| Route53 A/AAAA records | `aws_route53_record` | 1 | `beads-viewer.aws-poc.gardenio.acme.example` → AVA endpoint domain (CNAME-style ALIAS to the AVA-managed FQDN). |
| Secrets Manager secret | `aws_secretsmanager_secret` (+ `_version`) | 1 | Path `/gardenio/aws-poc/gitlab-ava-oidc`, holds `client_id` + `client_secret` for the GitLab OAuth app (POC-scoped redirect URI). **Operator-loaded** per §8 firewall — Terraform creates the secret shell with `prevent_destroy = true`, operator runs `aws secretsmanager put-secret-value` from the workstation. |
| IRSA grant extension | edit to `modules/platform` ESO IRSA | — | Add `/gardenio/aws-poc/gitlab-ava-oidc` to the ESO secret-arn list so the AVA trust provider can resolve client creds at apply time. Same pattern as **ka-0000003494**'s RDS-master-password fix (deviation §3.7 #8). |
| beads-viewer chart values overlay | `helm/gardenio/values-aws-poc.yaml` (or per-env `site-deployment/<env>/aws-poc/values.yaml`) | — | Flip `beadsViewer.avaIngress.enabled: true`, set `host: beads-viewer.aws-poc.gardenio.acme.example`, `trustSigner: <AVA instance ARN from the new instance in 123456789012>`, `region: us-east-1`, `defaultRole: gardenio-viewer`. Mirrors the prod chart values exactly except for the AVA ARN, host, and (eventual) account-ID. |

**GitLab-side change** (operator, out-of-band — does not flow through Terraform):

- Append a second redirect URI to the existing `gloas-*` OAuth application:
  ```
  https://<ava-endpoint-domain>/oauth2/idpresponse
  ```
  Per `canonical-prod-identity-gitlab.md` §G1, GitLab OAuth apps accept multiple redirect URIs; adding one is zero-downtime for the prod URI. Captured as **D-11** in §6.

**Not** provisioned by this tier:

- A new GitLab OAuth application — the existing `gloas-*` app is shared between prod and POC. Reusing it is explicitly the cheap, low-blast-radius path (see `canonical-prod-identity-gitlab.md` §G1).
- Any change to the Keycloak surface — Keycloak is removed from the canonical stack per **D-6**. The POC's `beads-viewer` overlay does **not** enable the Keycloak forwardauth route.
- Prod cutover of any DNS / SSO record — this tier is the **POC port**, not the prod cutover. Prod cutover is owned by a follow-on epic to be filed under [[ka-0000002270]] once tier (g) is green.

#### Apply / verification steps

Same `terraform plan` / `gb decision` / `terraform apply` cadence as tiers (b)–(e). The operator runs apply from the workstation against the AKIA in Secrets Manager `gardenio/customer-poc-aws-creds` (account `123456789012`, no STS — advice **ka-0000007102**).

1. **Pre-flight** — operator confirms tiers (a)–(f) are green, the new bead acceptance items in §7.10 are understood, and decision [[ka-0000000470]]'s follow-on tier-(g) `gb decision` is resolved `approve`.
2. **Secrets seed** — operator pre-creates the `/gardenio/aws-poc/gitlab-ava-oidc` secret value (client ID + secret) from the workstation. Verifies via `aws secretsmanager get-secret-value --secret-id /gardenio/aws-poc/gitlab-ava-oidc --query Name`.
3. **`terraform plan`** — operator reviews the plan output from a fresh checkout of `gardenio/terraform/composition/poc/` (with the implementation MR's edge-auth changes applied). Plan must show: 1 AVA instance, 1 trust provider, 1 group, 1 endpoint, 1 attachment, 1 Route53 record, 1 Secrets Manager secret shell, ESO IRSA policy update — **and nothing else** outside the existing tier (a)–(f) graph.
4. **`gb decision` resolves `approve`** for tier (g) apply.
5. **`terraform apply`** — operator runs apply; expects ~5 minutes (AVA endpoint provision is the long tail).
6. **GitLab-side append** — operator appends the second redirect URI to the `gloas-*` OAuth app (see GitLab-side note above). Recorded in the bead audit log as a `gb squawk` from the operator (per §8 firewall, this step is operator-only).
7. **Helm reconcile** — Terraform's `helm_release` for the gardenio chart picks up the `values-aws-poc.yaml` flip (`beadsViewer.avaIngress.enabled: true`). The `beads-viewer` pod restarts; the `ava-ingressroute.yaml` template emits the parallel IngressRoute on the new host.
8. **Smoke** — operator walks §7.10 acceptance items below.

#### Gate

**`gb decision`** "proceed to tier g apply (edge-auth port: AVA + GitLab OIDC for beads-viewer in account 123456789012)". Resolves `approve` before any AWS write. Mirrors tier (e)'s gate language: the runbook documents the gate, the decision authorizes the apply. **D-11** (GitLab redirect-URI append) must also be acknowledged in the gate — operator owns the change, it does not flow through Terraform.

For any re-apply of tier (g) (e.g. AVA endpoint replacement, trust-provider re-config), the gate re-opens; the runbook does **not** pre-authorize re-apply.



Each decision must resolve before the corresponding tier in §5 starts (whether for the initial apply already executed under epic [[ka-0000002270]], or for any re-apply against `123456789012`).

| ID | Question | Who decides | Blocks |
|---|---|---|---|
| **D-0** | Scaffold the real `terraform/` directory tree (root module + per-tier child modules), since `canonical-prod-tf-module-skeleton.md` (ka-0000007733) only landed the doc skeleton. | Sibling implementation task under Epic 2 — must land before tier (b). | All tiers from (b) onward. |
| **D-1** | AWS account procurement, billing approval, Organizations placement, root-credential custody, Identity Center bootstrap ownership. | Matt / acme ops. | Tier (a) — cannot start without an account. Also captured by the Epic 1 account-provisioning-runbook task ka-0000001214. |
| **D-2** | NS-delegation in the existing `acme.example` apex zone for the 2-label parent zone `gardenio.acme.example` (as-built shape per deviation §3.7 #2). Who owns the change, what zone holds the apex today, change-window approval. | Matt / DevOps. | Tier (e); one-time manual step. |
| **D-3** | IRSA vs EKS Pod Identity for the addon + workload role inventory. POC tracks **IRSA**; flag as decision so the implementation epic re-confirms before commit. | Epic 4 task ka-0000000573. | Tier (b) onward. |
| **D-4** | NAT-gateway HA shape (one per AZ vs single NAT for non-prod) and the interface-VPC-endpoint set. POC tracks **per-AZ NAT** + **S3 gateway endpoint only**. | Epic 3 tasks ka-0000000283 + ka-0000002147. | Tier (b). |
| **D-5** | RDS Multi-AZ instance vs Aurora Postgres (cost + HA + pgvector availability). POC tracks **RDS Multi-AZ instance**. | Epic 5 task ka-0000001656. | Tier (c). |
| **D-6** | Keycloak placement — **RESOLVED (2026-05-21) by operator decree on ka-0000006859**: Keycloak is **NOT** in the canonical stack. The canonical edge-auth model is the **GitLab AWS VPN SSO validated connection** (production approach). See `docs/research/customer-poc-edge-auth-canonical.md`. Predecessor doc `docs/research/canonical-prod-keycloak-placement.md` is deprecated. Chart cleanup is owned by **ka-0000004958** (suppress Keycloak CR emission when disabled); Terraform overlay updates by a sibling follow-up task to be filed by the implementation epic. | Decided. | None — chart cleanup decoupled; canonical POC ships without Keycloak. |
| **D-7** | State-backend bootstrap mechanism (one-time `local`-backend mini-root, then `terraform state mv` into the S3+DynamoDB backend) per `canonical-prod-tf-module-architecture.md` §5. | Epic 2 task ka-0000003471 (state backend + versioning + test tooling). | Tier (a). |
| **D-8** | The 6 operator questions surfaced by `canonical-prod-live-dependency-inventory.md` (ka-0000002168): Slack app reuse vs fork; GitLab webhook dual-subscribe window; `203.0.113.5/32` allow-list source; `gardenio-sessions` S3 size; ~~Keycloak issuer URL strategy~~ (moot — Keycloak removed from canonical stack per **D-6** decree on ka-0000006859); LiteLLM egress allow-list. POC ducks the remaining items (greenfield, isolated Slack app, etc.), but the cutover epic needs answers. | Matt / DevOps. | Epic 10 cutover work; **not blocking POC**. |
| **D-9** | CI dispatch of `terraform apply` (GitLab CI reusing `cicd-templates` TF component vs RWX vs neither). POC explicitly stays operator-workstation-only. | Epic 9 task ka-0000003917. | Future implementation epic, not POC. |
| **D-10** | Observability backend (Mezmo cross-account ingest vs in-cluster only vs CloudWatch only). POC tracks **in-cluster VictoriaMetrics only**. | Epic 8. | Future implementation epic, not POC. |
| **D-11** | GitLab OAuth-app redirect-URI append for the POC AVA endpoint (`https://<ava-endpoint-domain>/oauth2/idpresponse`). Operator-only change to the existing `gloas-*` app; zero-downtime per `canonical-prod-identity-gitlab.md` §G1. POC tracks **append second URI, keep prod URI**. | Matt / DevOps. | Tier (g). |

---

## 7. Acceptance checklist (human ticks each after a real apply)

The POC is "done" when every box below is ticked by a reviewer working from a fresh checkout of the (future) `site-deployment/<env>/aws-poc/` (or whatever path the per-env skeleton ADR settles on). Each item is independently verifiable — no "looks good overall" rows.

### 7.1 Pre-apply preconditions

- [ ] **D-1** resolved: dedicated AWS account exists, billing is approved, Identity Center is bootstrapped, and the operator has a `gardenio-admin` permission set assigned. **No long-lived IAM user credentials exist in the account.**
- [ ] **D-2** resolved: parent zone `gardenio.acme.example` is NS-delegated from the existing `acme.example` apex; `dig +trace NS gardenio.acme.example` shows AWS nameservers (per-env records like `aws-poc.gardenio.acme.example` resolve via this zone).
- [ ] **D-7** resolved: state-backend bucket + DynamoDB lock table exist in the new account, and the operator has migrated the bootstrap state in via the documented procedure.
- [ ] Operator can `aws sts get-caller-identity` using short-lived Identity Center creds and the result shows the new account ID.
- [ ] `terraform/` directory tree scaffolded (**D-0** done) — root module + per-tier children compile with `terraform init`/`terraform validate`.

### 7.2 Apply succeeds

- [ ] Each of tiers (b)–(e) in §5 has its own `terraform plan` reviewed by a human, and the apply outputs zero unexpected drift on a second plan.
- [ ] `terraform apply` (final tier (e)) completes with no orphan resources in the AWS console.
- [ ] `kubectl get nodes` shows 3 Ready infra nodes, one per AZ.
- [ ] `kubectl get nodes -l karpenter.sh/nodepool=agent-pool` returns zero nodes initially (scale-to-zero) — and after deploying a smoke agent pod (§7.5), shows one provisioned node.

### 7.3 Cluster is gardenio-ready

- [ ] `kubectl -n gardenio-system get pods` shows `gardenio-controller`, `coopmux`, `slack-bridge`, `nats-0` all `Running 1/1`.
- [ ] **No `postgres-0` pod exists in the cluster.** (`postgres.enabled: false` — proves the chart is using RDS.)
- [ ] `kubectl -n gardenio-system run psql --image=postgres:16 --rm -it -- psql -h <rds-endpoint> -U gardenio -d gardenio -c "SELECT extversion FROM pg_extension WHERE extname='vector';"` returns a non-empty result.
- [ ] `kubectl -n kube-system get pods -l app.kubernetes.io/name=aws-load-balancer-controller` shows the controller `Running`.
- [ ] `kubectl -n karpenter get pods` shows Karpenter `Running`.
- [ ] `kubectl -n external-dns logs deploy/external-dns` shows a successful sync to the Route53 zone (no auth errors, no zone-not-found errors).
- [ ] `kubectl -n cert-manager get clusterissuer letsencrypt-prod-dns01 -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}'` returns `True` (sanity-check fallback issuer). The primary ACM-attached cert lives on the NLB — verified by `aws elbv2 describe-listeners`.
- [ ] `kubectl -n external-secrets get clustersecretstore aws-secrets-manager -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}'` returns `True`.
- [ ] At least one `ExternalSecret` in `gardenio-system` is `SecretSynced=True`, pulling a real value from `/gardenio/aws-poc/*`.
- [ ] `kubectl -n victoria-metrics get pods` shows vm-operator + VMSingle + VMAgent `Running`.
- [ ] Every Pod with an AWS-API-touching `ServiceAccount` has `eks.amazonaws.com/role-arn` annotated (IRSA, not Pod Identity — confirms **D-3**).

### 7.4 Public reachability

- [ ] `dig +short aws-poc.gardenio.acme.example` returns the NLB's IP(s).
- [ ] `curl -sS https://aws-poc.gardenio.acme.example/healthz` returns HTTP 200 with a valid ACM cert (no `--insecure`, no cert warnings in the browser, Common Name matches).
- [ ] `openssl s_client -connect aws-poc.gardenio.acme.example:443 -servername aws-poc.gardenio.acme.example </dev/null 2>&1 | openssl x509 -noout -issuer` shows an Amazon-issued cert.
- [ ] The Slack bridge can post to the configured (isolated) POC channel: a `gb squawk "aws-poc reachable"` from inside a test agent pod surfaces in Slack within 30 seconds.

### 7.5 Beads + agent loop

- [ ] `kd create "aws-poc smoke" --type=task -l project:gardenio-aws-poc` succeeds against the POC beads endpoint, and the row lands in RDS (operator confirms via `psql ... SELECT id, title FROM beads ORDER BY created_at DESC LIMIT 1;`).
- [ ] A test agent pod spawned by the gardenio controller starts, registers with coopmux, reads its session config, and writes session output to S3 (`coop peek --archived <session-id>` returns the transcript via the standard AWS SDK — no `COOP_S3_ENDPOINT` override needed).
- [ ] Karpenter has scaled the agent pool from 0 to 1 to host the test agent pod; the node is on **Spot** (taint + Spot-instance label visible on `kubectl describe node`).
- [ ] After the agent pod terminates, Karpenter scales the agent pool back to 0 within the consolidation window.

### 7.6 Observability sanity

- [ ] VMSingle has ingested at least one metric from the gardenio namespace (`kubectl port-forward svc/vmsingle 8429` + `curl 'localhost:8429/api/v1/query?query=up{namespace="gardenio-system"}'` returns at least one series).
- [ ] EKS control-plane log groups exist in CloudWatch and are receiving entries (`aws logs describe-log-groups --log-group-name-prefix /aws/eks/`). No assertion on Mezmo — cutline.
- [ ] No CrashLoopBackOff pods anywhere in the cluster for 10 consecutive minutes.

### 7.7 Cutline confirmed (negative assertions)

- [ ] `terraform state list | grep aws_region` reports only `us-east-1` resources — **no multi-region resources exist.**
- [ ] No Aurora resources (`terraform state list | grep aws_rds_cluster` is empty).
- [ ] No Pod Identity associations exist (`aws eks list-pod-identity-associations --cluster-name <c>` returns an empty list).
- [ ] No `vultr_*` resources in the state, no second-cloud resources.
- [ ] No production Slack app / production GitLab webhook is registered against this cluster.
- [ ] No Mezmo log-shipper Helm release is installed.
- [ ] No GitLab-CI / RWX job has dispatched `terraform apply` — every apply line in the bead audit log shows an operator hostname, not a CI runner.

### 7.8 Teardown

- [ ] `terraform destroy` (in reverse tier order) removes the gardenio release, addons, EKS cluster, RDS instance, S3 bucket (after empty), KMS CMKs (with scheduled deletion), Route53 zone (or — if zone destroy is risky — explicit `lifecycle.prevent_destroy` on the zone and the operator runs a documented manual NS-undelegation), and VPC. No orphaned resources visible in the AWS console after 1 hour.
- [ ] Estimated cost during the POC lifetime is logged in the bead (ka-0000005971) for the eventual cost-delta doc (Epic 11 / `canonical-prod-cost-delta.md`).

### 7.9 Hand-off

- [x] Operator opened a `gb decision` on ka-0000005971: "AWS POC ready to leave research and enter implementation." A human reviewer ticked `approve` → the AWS implementation epic [[ka-0000002270]] was filed as a sibling to ka-0000005972.
- [x] This document is updated with the actual hostname (§3.5 `aws-poc.gardenio.acme.example`), provider version pins (§3.0), node-group sizing (§3.1 + deviation §3.7 #3), and deviations from §3 (§3.7) — turning it from a plan-of-record into a runbook. (Conversion: [[ka-0000003908]].)

### 7.10 Edge-auth port (tier (g) — beads-viewer + GitLab AWS VPN SSO)

Each box below must be ticked from a fresh terminal by a reviewer who is **not** already logged in (no cached cookies, fresh browser profile). The whole point of this tier is to prove the canonical edge-auth flow works in account `123456789012` against the production GitLab OAuth app, with **no Keycloak in the path** — mirroring the verification recorded under task [[ka-0000000904]] on 2026-05-24 for prod.

- [ ] **D-11** resolved: the `gloas-*` GitLab OAuth application lists **both** the prod redirect URI **and** the POC AVA endpoint redirect URI (`https://<ava-endpoint-domain>/oauth2/idpresponse`). `gb squawk` from the operator confirms the append happened, with timestamp.
- [ ] `aws verifiedaccess describe-instances` (or `aws ec2 describe-verified-access-instances` per the current AWS CLI) in account `123456789012` returns exactly **one** instance whose ARN matches the value pinned in `beadsViewer.avaIngress.trustSigner` in `values-aws-poc.yaml`.
- [ ] `aws ec2 describe-verified-access-trust-providers --filters Name=verified-access-instance-id,Values=<id>` returns exactly **one** OIDC trust provider with `issuer = "https://gitlab.com"`.
- [ ] `aws ec2 describe-verified-access-endpoints --filters Name=verified-access-instance-id,Values=<id>` returns exactly **one** endpoint with `application_domain = "beads-viewer.aws-poc.gardenio.acme.example"` and status `active`.
- [ ] `dig +short beads-viewer.aws-poc.gardenio.acme.example` resolves to the AVA-managed endpoint FQDN (not the raw NLB).
- [ ] `curl -sSI https://beads-viewer.aws-poc.gardenio.acme.example/` from an unauthenticated session returns HTTP **303** with a `Location:` header pointing at `https://gitlab.com/oauth/authorize?...` (direct OIDC redirect — **no Keycloak host** in the redirect chain). Matches the prod verification under [[ka-0000000904]].
- [ ] Following the redirect through a browser with a `gardenio-*` GitLab group member account lands back on `beads-viewer.aws-poc.gardenio.acme.example` with a valid ACM cert and the beads-viewer index page renders. **No 401, no Keycloak login page, no double prompt.**
- [ ] `kubectl -n gardenio-system logs deploy/<release>-beads-viewer | grep -i "ava"` shows the `oidcauth/ava` middleware accepting the `X-Amzn-Ava-User-Context` JWT (signed by the AVA instance ARN listed in `trustSigner`). No fallback to Keycloak in the log.
- [ ] `kubectl -n gardenio-system get ingressroute -l app.kubernetes.io/component=beads-viewer,app.kubernetes.io/gate=ava` returns exactly **one** IngressRoute, on host `beads-viewer.aws-poc.gardenio.acme.example`, with no Keycloak forwardauth middleware attached.
- [ ] **Negative assertion — Keycloak is not in the path.** `kubectl -n gardenio-system get pods -l app.kubernetes.io/name=keycloak` returns **no resources**, and the AVA IngressRoute template does not reference any Keycloak forwardauth middleware (verified by `kubectl get ingressroute <name> -o yaml | grep -i keycloak` returning empty). Consistent with **D-6** decree.
- [ ] **Negative assertion — no prod cutover.** Prod beads-viewer still resolves and authenticates via the prod AVA endpoint; the POC AVA endpoint is additive only. `dig +short beads-viewer.e2e.example.com` still resolves to the prod AVA endpoint (unchanged), proving the POC apply did not touch prod state.
- [ ] Operator records the tier (g) outcome with a `gb squawk` on this bead and updates the tracking bead [[ka-0000003897]] (or the implementation MR's bead) with the actual AVA instance ARN + endpoint domain for future reference.

---

## 8. Operator handoff — keep root creds away from agent context

**The very first manual step** (tier (a) — account bootstrap) is operator-only and must **never** flow through an agent. The firewall rules:

1. **AWS root credentials** for the new gardenio-ai account are created and used **only** to enable Identity Center, register the MFA device, and create the initial `gardenio-admin` permission set. Then the root credentials are rotated, locked, and stored per acme's secret-management policy. They are **never** typed into a Claude / `gb` / `kd` session, never pasted into a bead, never sent over Slack DM to a bot, never committed to any repo.
2. **Identity Center bootstrap** (directory source, permission sets, account assignments) happens in the AWS console + AWS CLI **with operator-owned credentials only**. No agent runs `aws sso-admin *` or `aws organizations *` against this account. The audit-event for "Identity Center provisioned" is a `gb squawk` from the operator, not from an agent.
3. **The state-backend bootstrap** (S3 bucket + DynamoDB lock table) per `canonical-prod-tf-module-architecture.md` §5 is also operator-only. The bootstrap mini-root uses `local` backend on the operator workstation; the migration into the remote backend is operator-driven. Agents do not see the bootstrap state file.
4. **Identity Center login + short-lived creds** for any subsequent agent-assisted work happens only **after** (1)–(3) are complete. The credential surface that agents see is therefore: short-lived (≤ 12h) Identity-Center-issued role credentials, scoped to permission sets that do **not** include `iam:*` against the org root or `organizations:*`.
5. **Secrets Manager seed values** (Slack bot token for the isolated POC channel, GitLab PAT for the read-only POC user, LiteLLM API key for the POC) are also operator-loaded via `aws secretsmanager put-secret-value` from the operator workstation. The agent reads them through external-secrets only — never directly through the AWS SDK.

This section is the only part of this plan that an operator is expected to do **before** any agent picks up an implementation task. After §8 lands, the rest of the tiers (§5 b–f) are agent-eligible work — and per the Apply-gating decree (ka-0000008263) the applies themselves are **agent-driven, not operator-gated**; the gate is `terraform plan` review, not a `gb decision`.

---

## 9. What this doc does NOT cover (and where to look)

- **The AWS account baseline.** Lives in `docs/research/canonical-prod-aws-account.md`. This POC consumes its conclusions; it does not re-derive them.
- **The TF module architecture and skeleton.** Lives in `docs/research/canonical-prod-tf-module-architecture.md` + `docs/research/canonical-prod-tf-module-skeleton.md` (ka-0000007733). The as-built layout under `gardenio/terraform/composition/poc/` follows that skeleton (D-0 scaffold landed as part of the tier-(b) bootstrap under epic [[ka-0000002270]]).
- **The per-env caller layout.** Lives in `docs/research/canonical-prod-site-deployment-invocation.md` + `docs/research/canonical-prod-per-env-skeleton.md`. The POC's `site-deployment/<env>/aws-poc/` directory will follow those.
- **The cutover from america-e2e-eks.** Lives in `docs/research/canonical-prod-migration-runbook.md`, `canonical-prod-stateful-migration.md`, `canonical-prod-beads-db-cutover.md`, `canonical-prod-dns-tls-cutover.md`, `canonical-prod-live-dependency-inventory.md`. None of this is in scope for the POC (cutline §4).
- **The cost story.** Lives in `docs/research/canonical-prod-cost-estimate.md` + `canonical-prod-cost-delta.md` + `canonical-prod-finops-plan.md`. The POC informs the cost-delta with real teardown numbers (§7.8).
- **The umbrella roadmap.** `gardenio/plans/canonical-prod-round2-refinement.md` is the canonical 12-epic plan; this runbook is the as-built record of the first concrete AWS implementation slice (epic [[ka-0000002270]]).
- **The Vultr POC twin.** `gardenio/plans/canonical-prod-vultr-poc.md` (ka-0000006197) — the portability proof. This AWS POC is the customer-poc home-court reference; together they define the portability seam.

---

## 10. Sign-off

This document is the **runbook** for the customer-poc AWS POC. The authoritative
implementation work is tracked under epic **[[ka-0000002270]]** — "gardenio
customer-poc AWS implementation" (sibling of the research epic
[[ka-0000005972]]). The as-built shape captured throughout §3 (provider pins,
node-group sizing, hostnames, IRSA roster, RDS specifics) and the deviations
recorded in §3.7 are the source of truth for any re-apply; the §7 acceptance
checklist is the operator validation flow.

**Re-applies are agent-driven (2026-05-29, ka-0000008263).** Account
`123456789012` is gardenio's own account; agents own its applies. A rebuild
or re-apply from a fresh checkout of `gardenio/terraform/composition/poc/`
does **not** require a `gb decision` — the effective gate is `terraform plan`
review: if the plan shows unexpected infra churn, stop and file a bug under
the currency epic [[ka-0000005495]]. The §8 operator-firewall (account /
Identity-Center / state-backend bootstrap, SM secret seed values) is the
only operator-only surface that remains.

> Per umbrella core rule 4: the migration from `america-e2e-eks` to the new
> gardenio-prod cluster is owned by Epic 10 ([[ka-0000001529]]) and is **not**
> part of this POC. The POC is greenfield only.
