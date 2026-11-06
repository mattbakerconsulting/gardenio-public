# ADR 0002 — gardenio Terraform module: multi-cloud portability seam

> Status: **Accepted** (ratified via `gb decision` ka-0000007781 on 2026-05-14 — option (a))
> Bead: ka-0000004922
> Parent epic: ka-0000006189 (research+design: add Vultr to canonical-prod TF module)
> Lands inside: ka-0000005315 (Epic 2 — TF module architecture) per umbrella rule
> Umbrella epic: ka-0000005972 (canonical-prod, research-only)
> Prior art:
> - [`docs/adr/0001-tf-module-state-backend-versioning-testing.md`](0001-tf-module-state-backend-versioning-testing.md) — state/versioning/test gates
> - `docs/research/canonical-prod-tf-module-architecture.md` — prior-art survey (not in this repo — the canonical-prod research tree was excluded from the 2026-09-16 export)
> - `docs/research/canonical-prod-tf-module-skeleton.md` — proposed directory tree (AWS-only as written) (not in this repo, see note above)
> Siblings (open):
> - ka-0000002019 — AWS↔Vultr resource map + gap analysis (in flight under ka-0000006189)
> - ka-0000006197 — Vultr Terraform provider survey (in flight under ka-0000006189)
> - ka-0000002100 — per-canonical-prod-epic Vultr impact analysis (in flight)
> - ka-0000000366 — Vultr POC scope + cutline

## 1. Context

The gardenio canonical-prod Terraform module is being designed in this repo as **AWS-only** today (EKS, RDS Postgres, S3+KMS, ALB, Route53, IRSA, Secrets Manager, CloudWatch). Umbrella epic ka-0000005972 and parent epic ka-0000006189 extend the design to **Vultr** as a second cloud target (VKE, Vultr Managed Postgres, Vultr Object Storage, Vultr VPC 2.0, Vultr LBs, Vultr DNS). This ADR resolves the **architecture-level fork** that every other Epic-2 deliverable depends on:

> What is the seam in the module's source tree along which AWS-specific and Vultr-specific code are separated?

Three candidate shapes were named in the epic body and the umbrella plan:

- **(a) Per-cloud composition root, shared submodules where genuinely portable.** Two top-level compositions: `gardenio/terraform/aws/` and `gardenio/terraform/vultr/`. Each invokes cloud-specific submodules (`modules/aws-network/`, `modules/vke/`, ...) **and** shared submodules where the implementation is cloud-agnostic (`modules/platform/` — the k8s-native bootstrap layer consuming a kubeconfig + IAM-equivalents).
- **(b) Single composition root with provider abstraction inside each submodule.** One top-level module taking `var.cloud_target = "aws" | "vultr"`. Each submodule (`modules/network/`, `modules/eks/`, `modules/data/`, `modules/platform/`) contains both AWS and Vultr resource bodies gated on the target variable (or via `count` / `for_each` blocks tied to the target).
- **(c) Two parallel top-level modules with a shared variable/output contract.** `gardenio/terraform/aws/` and `gardenio/terraform/vultr/` are entirely separate, share **no Terraform code** at all, and only agree on the input variable shape and the `gardenio_info` output shape. Each one is internally free to organise as it wishes; portability is enforced only at the I/O boundary.

The decision affects: the directory skeleton (ka-0000007733), the input/output contract (ka-0000002543), how callers in `site-deployment` invoke the module (Epic 9), the CI gates this repo wires (this ADR's sibling ka-0000003471), the per-canonical-prod-epic impact analysis (ka-0000002100), and the Vultr POC plan (ka-0000000366). It cannot defer to a later epic without re-doing work — hence its place inside Epic 2.

This ADR does **not** specify per-resource AWS↔Vultr mappings (sibling ka-0000002019), Vultr provider version pinning (sibling ka-0000006197), or the POC scope (ka-0000000366). It decides only the seam.

---

## 2. Decision — Option (a): per-cloud composition root, shared submodules where genuinely portable

### 2.1 Recommendation

Adopt **Option (a)**. Concrete shape, layered on top of the existing AWS-only skeleton in ka-0000007733:

```text
gardenio/terraform/
├── README.md                            # entry point; explains the seam
├── CHANGELOG.md                         # single changelog for both compositions
├── aws/                                 # AWS composition root
│   ├── README.md
│   ├── versions.tf                      # required_providers: aws, kubernetes, helm, tls
│   ├── variables.tf                     # caller contract (AWS-shaped inputs)
│   ├── outputs.tf                       # gardenio_info (portable shape — ka-0000002543)
│   ├── main.tf                          # wires aws-network, eks, aws-data, platform
│   └── locals.tf
├── vultr/                               # Vultr composition root
│   ├── README.md
│   ├── versions.tf                      # required_providers: vultr, kubernetes, helm, tls
│   ├── variables.tf                     # caller contract (Vultr-shaped inputs)
│   ├── outputs.tf                       # gardenio_info (same portable shape)
│   ├── main.tf                          # wires vultr-network, vke, vultr-data, platform
│   └── locals.tf
├── modules/
│   ├── aws-network/                     # VPC, subnets, NAT, IGW, route tables, endpoints, flow logs
│   ├── eks/                             # EKS control plane + OIDC + node groups + addons + IRSA glue
│   ├── aws-data/                        # RDS Postgres, S3+KMS, Secrets Manager
│   ├── vultr-network/                   # VPC 2.0, subnets, reserved IPs, firewall groups
│   ├── vke/                             # VKE cluster + node pools + OIDC + addons
│   ├── vultr-data/                      # Vultr Managed Postgres, Object Storage buckets, secrets store
│   └── platform/                        # SHARED — in-cluster bootstrap addons (external-dns,
│                                        #   cert-manager, external-secrets, victoria-metrics, etc.)
│                                        #   takes kubeconfig + a small set of cloud-neutral inputs
└── examples/
    ├── aws-minimal/                     # validate-only stub for AWS composition
    └── vultr-minimal/                   # validate-only stub for Vultr composition
```

The portable seam **is the `gardenio_info` output contract plus the `platform/` shared submodule**. Everything cloud-specific lives in a `aws-*` or `vultr-*` submodule and is wired by the matching composition root. Callers in `site-deployment` pick a composition root by source path:

```hcl
# AWS env
terraform { source = "git::ssh://.../gardenio.git//gardenio/terraform/aws?ref=tfmod-v1.2.0" }

# Vultr env
terraform { source = "git::ssh://.../gardenio.git//gardenio/terraform/vultr?ref=tfmod-v1.2.0" }
```

Both compositions accept a `cloud`-neutral subset of inputs (env name, domain, slack channel refs, image pull creds, observability backend URLs) and a cloud-specific subset (subnet shape, instance/plan IDs, IAM/IRSA principals vs Vultr roles, KMS/SSE knobs vs Vultr SSE knobs). Both produce the same `gardenio_info` output map. The contract for that map is defined in ka-0000002543 and **must** be extended (not replaced) to add a `cloud = "aws" | "vultr"` discriminator plus normalised fields (`oidc_provider_url`, `cluster_endpoint`, `node_security_group_id_or_equivalent`, `object_store_bucket`, `secret_store_namespace`, `dns_zone_id`, ...).

### 2.2 What "shared submodule" really means

A submodule is shared **only if** it consumes no cloud provider directly. The litmus test:

> Does this submodule's `required_providers` need `aws` or `vultr`?

If yes, it is cloud-specific and lives in `modules/{aws,vultr}-<name>/`. If no — it speaks only to `kubernetes`, `helm`, `tls`, `kubectl`, `random`, etc. — it lives in `modules/<name>/` and is consumed by both compositions.

Today that is exactly **one** module: `modules/platform/`. Future portable layers (a future `modules/cluster-observability/` that takes a Prometheus remote-write URL, a future `modules/gardenio-controller-bootstrap/`) follow the same rule. **We do not pre-share modules speculatively.** A module joins `modules/<name>/` only when a second consumer exists and the implementation is trivially identical across clouds.

### 2.3 What the seam buys us

1. **Each composition root is readable on its own.** Reviewing the Vultr composition does not require reading the AWS composition. There is no `if cloud_target == "aws"` ladder to trace.
2. **Provider blocks are clean.** AWS composition declares `provider "aws"` and never imports `vultr`. Vultr composition declares `provider "vultr"` and never imports `aws`. `terraform init` for a Vultr env does not download the (large) AWS provider, and AWS-only callers in site-deployment never need a Vultr API token in CI.
3. **Resource graphs are explicit.** No `count = var.cloud_target == "aws" ? 1 : 0` patterns. `terraform plan` output reads as a flat resource list, not a conditional ladder.
4. **Drift between clouds is detectable structurally.** If `aws/main.tf` wires `module.aws-data` with a new input and `vultr/main.tf` doesn't add a matching input on `module.vultr-data`, the contract divergence shows up as an `outputs.tf` mismatch — caught by the planned `gardenio_info` contract test (see §6).
5. **Net-new clouds are cheap to add and contained.** A Hetzner target is a new `gardenio/terraform/hetzner/` composition root + `modules/hetzner-*/` submodules. Nothing in `aws/` or `vultr/` changes.

### 2.4 Costs we are accepting

- **More directories.** The repo grows by one composition root, three cloud-specific submodule directories, and one example directory per cloud target added. Manageable; mirrors `site-deployment`'s `new-modules/` layout where modules are split by lifecycle, not deduplicated.
- **`platform/` is the only true code dedup.** Per-cloud network/cluster/data submodules cannot share files. Anyone hoping for "write once, ship to all clouds" will be disappointed — that is option (b) (rejected, see §3.1).
- **CHANGELOG and versioning span both compositions.** A single `tfmod-v*` tag covers both `aws/` and `vultr/` (per ADR 0001 §2). A breaking change to either bumps MAJOR. Acceptable: the alternative (per-composition tags `tfmod-aws-v*`/`tfmod-vultr-v*`) couples release cadence asymmetrically and complicates the `?ref=` discipline.
- **Two stub examples instead of one.** `examples/aws-minimal/` and `examples/vultr-minimal/` both exist to give `terraform validate` something to point at per cloud. CI gains a parallel matrix dimension.

---

## 3. Alternatives considered

### 3.1 Option (b) — single composition root with in-module provider abstraction

**Rejected.** Looks clean as a diff, but every prior incarnation of this pattern in the Terraform ecosystem (e.g. early multi-cloud `terraform-aws-modules` forks, internal multi-cloud platform modules at large orgs) collapses into the same anti-patterns:

| Anti-pattern | Concrete shape inside a `modules/eks/main.tf` ladder |
|---|---|
| **Resource-count gating** | `resource "aws_eks_cluster" "this" { count = var.cloud == "aws" ? 1 : 0 ... }` plus the mirror `resource "vultr_kubernetes" "this" { count = var.cloud == "vultr" ? 1 : 0 ... }`. Every downstream `data` and dependency reference becomes `module.eks.aws_eks_cluster_id[0]` or a `try(...)` wrapper. |
| **Required-providers bloat** | The single module declares both `aws` and `vultr` in `required_providers`. `terraform init` for a Vultr-only env still downloads the AWS provider (≈400 MB) and vice versa. CI cache invalidation gets larger. |
| **Provider configuration crosstalk** | `provider "aws"` is required to exist even when unused, because the module's `aws_*` resources are *parsed* even when their `count = 0`. Callers in site-deployment must declare a no-op AWS provider block in Vultr envs (and supply a region, even though no aws resources will exist), or vice versa. This has been the most frequent footgun in multi-provider modules in the wild. |
| **Type-system damage to outputs** | `output "cluster_endpoint"` becomes `coalesce(try(aws_eks_cluster.this[0].endpoint, null), try(vultr_kubernetes.this[0].endpoint, null))`. Every output devolves into a `try()` chain. Static type guarantees inside a single output value evaporate. |
| **Review surface area explodes** | Reviewers reading a 1-line input change to `modules/eks/variables.tf` must now think about both AWS and Vultr resource graphs. The "what does plan show?" question doubles. |
| **Plan output is noisy** | `terraform plan` against a Vultr env shows `aws_eks_cluster.this[0]: no changes` and dozens of sibling `aws_*` no-op lines (because the module evaluates all resource blocks regardless of `count`). Operators learn to skim past them — which means real AWS resource drift in a Vultr env (e.g. someone misconfigured the unused AWS provider block) hides in the noise. |

The "diff is small" claim is also wrong in practice. Adding a Vultr resource to a `modules/eks/main.tf` that already contains the AWS resource graph means **every reviewer pass on either resource touches the other file**. The blast radius for a Vultr-only PR is the entire EKS module.

The community consensus pattern (look at `cloudposse/terraform-aws-*` vs `cloudposse/terraform-vultr-*`, look at `terraform-google-modules` and `terraform-aws-modules` deliberately not merging, look at HashiCorp's own EKS/AKS/GKE modules being kept separate) is per-provider. That is option (a), exactly because of the costs above.

### 3.2 Option (c) — two parallel modules with shared I/O contract only

**Rejected.** This is option (a) minus `modules/platform/` minus shared discipline. It maximises **drift risk** for the layers that are genuinely portable:

- `external-dns`, `cert-manager`, `external-secrets`, `victoria-metrics`, the gardenio controller helm release itself — all of these take a kubeconfig and a small set of cloud-neutral knobs (DNS zone reference, IAM-equivalent role ARN/principal, observability backend URL). Duplicating them across `aws/` and `vultr/` means:
  - One PR fixing an `external-dns` rbac edge case must be applied twice.
  - Add-on version bumps drift independently. `aws/` is on `external-dns 1.14.1`; `vultr/` is still on `1.12.0`. Six months in, the two compositions are subtly different platforms.
  - CVE response in a shared addon doubles in latency.
- The umbrella epic's core rule "module portability trumps minimising diff" is best honoured by structurally requiring a shared module for portable layers, not by hoping two parallel modules stay aligned through review discipline.
- Option (c)'s "only the I/O contract is shared" framing is also subtly under-specified. If the contract is the gate, **what tests the contract**? Without a shared examples/ matrix exercising both compositions against the same input shape, contract drift only surfaces in production when the site-deployment caller fails to compile against the new shape. Option (a) puts the contract test in this repo (a shared `examples/contract-shape/` validate harness can target both compositions); option (c) cannot.
- Option (c) is strictly less constrained than option (a): anything we forbid in option (a) (e.g. accidentally importing a cloud-specific provider in `platform/`) is silently legal in option (c). Less structure here is not a feature — it is the absence of a backstop.

**When option (c) would win:** if the AWS and Vultr designs diverged so deeply that there were **zero** genuinely portable submodules (e.g. radically different addon ecosystems, no shared k8s control-plane assumptions). Today's design has at least one true-shared layer (`platform/`), and the Epic-13 "batteries included" goals (ka-0000007371) push toward more (a shared controller-bootstrap submodule, a shared observability submodule). Option (c) bets against where the design is going.

### 3.3 Option (d, considered and dropped before this ADR) — Terraform workspaces keyed on cloud

`terraform workspace new vultr`, then `var.cloud_target = terraform.workspace`. Rejected without inclusion in the main alternatives table because: (a) Terragrunt-native deployments do not use Terraform workspaces (state is keyed by `state_name` from leaf path, not by workspace) and the site-deployment convention has no workspace machinery; (b) workspaces hide the cloud target inside a CLI-state variable, which is exactly the kind of implicit configuration we are trying to eliminate; (c) it is a strictly worse variant of option (b) with all the same provider-block crosstalk problems.

### 3.4 Option (e, considered) — generator/codegen layer

Author a single source file, codegen `aws/` and `vultr/` from it. Rejected on cost-benefit: net-new tooling, net-new lint surface, no prior art in the org, and the gain over option (a) is small once `platform/` already absorbs the genuinely-shared logic. Reserve as a far-future revisit if a third or fourth cloud target arrives and dedup pressure dominates.

---

## 4. Tradeoff matrix

| Criterion | (a) per-cloud composition, shared portable submodules | (b) in-module provider abstraction | (c) parallel modules, contract-only sharing |
|---|---|---|---|
| **Diff size when adding Vultr** | +1 composition root, +3 cloud-specific submodules, +1 example | +1 input var, ~2x source size in every submodule, +1 provider in `required_providers` | +1 full top-level module (≈ size of the AWS module) |
| **Cognitive load per submodule** | Low — one cloud per file | High — every file holds two resource graphs | Low — but doubled total |
| **Drift risk on shared layers** | Low — `platform/` is a single source | N/A — everything is one source | High — duplicated logic drifts |
| **Provider isolation** | Clean — AWS provider never appears in Vultr planning, vice versa | Bad — both providers always present | Clean |
| **Plan output readability** | Flat resource list per cloud | Noisy with no-op `count=0` resources | Flat |
| **Cost to add a 3rd cloud (e.g. Hetzner)** | Linear — +1 composition + cloud-specific modules | Quadratic — every `modules/*/main.tf` grows by another branch | Linear — but full redundancy on portable layers |
| **Caller ergonomics (site-deployment)** | `source = ".../gardenio/terraform/{aws,vultr}?ref=..."` — explicit and self-documenting | `source = ".../gardenio/terraform?ref=..."`, then `cloud_target = "vultr"` input — single source but indirection | Same as (a) |
| **CI matrix in this repo** | 2 validate jobs (one per composition) + 1 contract test | 1 validate job, but must exercise both targets to be meaningful | 2 validate jobs, no contract test possible in-repo |
| **Compatibility with ADR 0001 versioning** | Single `tfmod-v*` tag covers both compositions; MAJOR on either bumps both — fine | Single tag, one composition — natural | Single tag covers both — fine |
| **Type guarantees on outputs** | Per-cloud outputs are concrete; cross-cloud contract is enforced at `gardenio_info` boundary | Outputs degrade to `try()`/`coalesce()` chains | Per-cloud outputs are concrete; contract enforced by convention only |
| **Reviewer surface for a cloud-specific PR** | Just that cloud's tree | Both cloud bodies in the same file | Just that cloud's module |
| **Honors umbrella core rule "module portability trumps minimising diff"** | Yes — `platform/` is structurally shared | Indirectly — abstraction is in code but not in structure | No — encourages drift |

Option (a) wins on every axis except "minimum diff size at the moment Vultr is added", where (b) appears to win but in practice loses to (a) once the count/try cost is paid back.

---

## 5. Consequences

### 5.1 Immediate (Epic 2 follow-ups)

- The skeleton task **ka-0000007733** must be amended to introduce `gardenio/terraform/aws/` as the AWS composition root (renamed from the current root) and add a parallel `gardenio/terraform/vultr/` skeleton. The shared `modules/platform/` survives in place; today's `modules/{network,eks,data}/` are renamed to `modules/aws-{network,eks,data}/`. New stubs for `modules/vultr-{network,vke,vultr-data}/` are added with placeholder `versions.tf` / `variables.tf` / `outputs.tf` only.
- The I/O contract task **ka-0000002543** must extend `gardenio_info` with: a `cloud = "aws" | "vultr"` discriminator (`type = string`, validated to one of the two), a normalised `cluster.oidc_issuer_url` field (set to the EKS OIDC URL on AWS, the VKE OIDC URL on Vultr), and a `secrets.backend = "aws-secrets-manager" | "vultr-managed-secrets" | "external-secrets-only"` discriminator. Per-cloud sub-objects (`aws = { secrets_manager_arn_prefix = ..., kms_key_id = ... }`, `vultr = { object_storage_endpoint = ..., dns_zone_id = ... }`) are nullable and only populated for the matching cloud.
- The state-backend ADR (this ADR's sibling **ka-0000003471**) does not need to change. The state backend is caller-side (Terragrunt-generated). Vultr envs in `site-deployment` use the same S3+DynamoDB pattern in the new gardenio-prod AWS account — Vultr does **not** require its own state backend just because workloads run there. (Open follow-up: revisit if a Vultr-only consumer wants to keep their state on Vultr Object Storage.)
- CI in this repo grows a parallel `tf-module-check` matrix: `{ composition: [aws, vultr] }`. `fmt -check` is repo-wide; `validate` runs per composition; `tflint` runs per composition with the appropriate provider plugin (`tflint-ruleset-aws` for `aws/`, none-or-vultr-ruleset-if-it-exists for `vultr/`). Shared `modules/platform/` validates under both compositions via their example fixtures.
- The Vultr provider survey (sibling ka-0000006197) is unaffected as a *task*, but its output goes into `gardenio/terraform/vultr/versions.tf` (the Vultr composition's `required_providers`), not into a shared `versions.tf`.

### 5.2 Affects every other epic that touches the module shape

This ADR is what ka-0000006189 §2 anchors to. Downstream effects:

- **Per-canonical-prod-epic impact analysis (ka-0000002100).** The impact table now has a structural answer for the question "where does Vultr design live?": either (i) extending `modules/platform/` (additive), (ii) authoring a new `modules/vultr-<name>/` sibling alongside an existing `modules/aws-<name>/` (replaces AWS-specific design but only in the new tree), or (iii) extending the `gardenio_info` contract (extends contract). No epic produces a Vultr-only patch *inside* an AWS-named module.
- **Epic 4 (EKS) → VKE handling.** A new `modules/vke/` becomes the home for VKE control plane + node pools + OIDC + addon pinning. `modules/eks/` is unchanged. The Karpenter-vs-Cluster-Autoscaler ADR (`docs/research/canonical-prod-karpenter-vs-ca.md`) splits: AWS keeps the existing recommendation, Vultr's compute auto-scaling story is a sibling research item.
- **Epic 5 (Stateful).** Object-storage parity (Vultr Object Storage is S3-API but lacks KMS-SSE and bucket-policy parity) is contained inside `modules/vultr-data/`. The Object Storage CORS/lifecycle gap surfaces as a documented unsupported-feature flag on the Vultr composition's `gardenio_info.object_store.features`.
- **Epic 7 (Identity).** Unchanged. IdP integration lives in `modules/platform/` (Keycloak/OIDC inputs are cloud-agnostic), per the impact analysis.
- **Epic 9 (site-deployment invocation).** Two Terragrunt leaf shapes documented: `live/<env>/.../gardenio-aws/terragrunt.hcl` and `live/<env>/.../gardenio-vultr/terragrunt.hcl`. Each `include`s a `tg-templates/services/gardenio-{aws,vultr}/terragrunt.hcl`. Caller pin discipline (ADR 0001 §2) is unchanged.
- **Epic 11 (FinOps).** Cost-estimate doc grows a Vultr column; module-shape change has no cost impact.
- **Epic 13 (batteries-included).** The "kube-first platform" claim is structurally guaranteed: the kube-first surface area lives in shared `modules/platform/`, so by construction the same gardenio controller, slack-bridge, beads, and platform addons stand up on either cloud.

### 5.3 Out of scope (explicitly)

- The full per-resource AWS↔Vultr mapping. That is sibling ka-0000002019.
- The Vultr provider version pin. That is sibling ka-0000006197.
- The Vultr POC scope and cutline. That is sibling ka-0000000366.
- A third cloud target. The seam supports it cheaply but no design work happens in this epic.
- An apply path inside this repo for Vultr. Per ADR 0001 §1, this repo never applies. Vultr applies happen in site-deployment, same as AWS.

---

## 6. How we will know option (a) was the right call

These are the falsification criteria for this decision. If any of them fail within the first two consumers of the module, we revisit:

1. **Adding the Vultr composition takes ≤ the diff size of duplicating the AWS module.** If `vultr/` plus `modules/vultr-*/` ends up larger than a straight copy of `aws/` plus `modules/aws-*/`, option (c) wasn't actually worse on diff size and we missed a code-share opportunity.
2. **`modules/platform/` validates unchanged from both compositions.** If we are forced to fork `platform/` into per-cloud variants within the first 3 months of Vultr work, option (a) collapses into option (c) in practice and we should formalise that.
3. **No `count = var.cloud == "..." ? 1 : 0` patterns appear anywhere in `modules/*/main.tf`.** If they do, we leaked option (b) into the design and should refactor.
4. **The `gardenio_info` output shape is identical (modulo nullable per-cloud sub-objects) on both compositions.** A contract test in CI enforces this (see §5.1 CI changes).
5. **Reviewers on a cloud-specific PR do not need to read the other cloud's tree.** Test by sampling MRs after Vultr lands: if reviewers report "had to read the AWS module to understand the Vultr change", the seam isn't holding.

---

## 7. Open follow-ups (not in this ADR)

- **Naming.** `aws-network` vs `network-aws` vs `aws/network`. The convention above is `<cloud>-<purpose>` to keep tab-completion alphabetised by cloud. Final naming locked in the skeleton task (ka-0000007733).
- **`modules/platform/` provider declarations.** It needs to NOT declare any cloud provider. Whether it declares `kubernetes`, `helm`, `kubectl`, `tls`, etc., is finalised in the skeleton task. Either way, it must remain cloud-agnostic.
- **Contract test harness.** The shape of `examples/contract-shape/` (a tiny TF root that consumes both compositions with mocks and asserts on `gardenio_info` keys) is a separate skeleton-task follow-up.
- **`tflint-ruleset-vultr`.** No widely-adopted Vultr-specific tflint plugin exists today. Decide in sibling ka-0000006197 whether to run tflint with rules disabled on the Vultr composition or skip tflint there entirely (with a CI comment explaining why).
- **Per-cloud `CHANGELOG.md` sections.** ADR 0001 §2 mandates a `CHANGELOG.md` at the module root. Decide whether changelog entries are tagged with `[aws]`/`[vultr]`/`[platform]` prefixes (recommended) or kept in separate per-cloud changelog files (rejected — fragmenting the changelog defeats its single-source-of-truth role).
- **Cross-cloud migration.** Out of scope for Epic 2 (Epic 10 owns migration). The seam permits a future site-deployment caller to stand up both `gardenio-aws` and `gardenio-vultr` against the same upstream beads DB during a migration window, but the migration plan is its own design.

---

## 8. Ratification

This ADR is **Accepted** as of 2026-05-14, ratified by the operator via `gb decision` ka-0000007781 (choice: `approve-a`). Consequences:

- Skeleton task **ka-0000007733** consumes §2.1 as fixed input.
- I/O contract task **ka-0000002543** consumes §5.1 (the `gardenio_info` extensions) as fixed input.
- Impact-analysis task **ka-0000002100** classifies each child epic using the seam from §5.2 as the structural answer.
- Vultr POC scope task **ka-0000000366** assumes the Vultr composition root exists at `gardenio/terraform/vultr/`.
