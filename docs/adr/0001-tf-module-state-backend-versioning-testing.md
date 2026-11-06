# ADR 0001 — gardenio Terraform module: state backend, versioning, and test tooling

> Status: **Proposed** (awaiting human ratification — `gb decision`)
> Bead: ka-0000003471
> Parent epic: ka-0000005315 (Epic 2 — TF module architecture)
> Umbrella epic: ka-0000005972 (canonical-prod, research-only)
> Prior art: `docs/research/canonical-prod-tf-module-architecture.md` (not in this repo — the canonical-prod research tree was excluded from the 2026-09-16 export)
> Sibling task: ka-0000002543 (input/output contract — separate ADR)

## Context

The gardenio Terraform module will live in this repo (`gardenio/`, path TBD by Epic 2 layout task) and be invoked from `site-deployment/terraform/live/`. The prior-art survey (ka-0000003215, MR !1308) flagged three forks that are unresolved by the org's existing TF setup:

1. **State backend** — what does the caller configure in the new gardenio-prod AWS account?
2. **Cross-repo module versioning** — how do callers in `site-deployment` pin a version of code that lives in `gardenio/`? Nothing else in the org does cross-repo TF.
3. **Test tooling** — org default is `validate` + plan-review on the MR. Do we hold that bar, or raise it?

This ADR records the recommendation per fork, the alternatives considered, and what each choice locks in for downstream Epic 2 work (skeleton authoring) and Epic 9 (site-deployment invocation).

This ADR does **not** specify the module's input/output contract — that is sibling task ka-0000002543.

---

## Decision 1 — State backend: **S3 + DynamoDB, per-account, in-account**

### Recommendation

Use **S3 + DynamoDB** for remote state and locking, with the bucket and lock table living **inside the new gardenio-prod AWS account** (not the existing america-e2e/site-deployment state account). The module **does not declare a backend**; the caller's Terragrunt leaf in `site-deployment/terraform/live/<env>/.../gardenio/terragrunt.hcl` declares it via the existing `generate "backend_opt_in"` pattern, exactly like `foundation` and `eks` do today.

Concrete configuration (caller-side, e.g. `site-deployment/terraform/live/<env>/env_cfg.hcl` for the new env):

```hcl
remote_state {
  backend = "s3"
  config = {
    bucket         = "gardenio-prod-terraform-state"
    key            = "${local.state_name}.tfstate"
    region         = "us-east-1"
    encrypt        = true
    dynamodb_table = "gardenio-prod-terraform-state-lock"
  }
}
```

Bootstrap follows the existing site-deployment chicken-and-egg pattern: a `live/common/operations/` Terragrunt leaf in the new account, manually `terraform init -backend-config=...` once at account creation, with `aws_s3_bucket` + `aws_dynamodb_table` Terraform resources self-managing thereafter. This is unchanged from how `prod2-new-deployment-terraform-state` was bootstrapped — not a new pattern.

### Alternatives considered

| Option | Verdict | Reason |
|---|---|---|
| **Terraform Cloud / HCP Terraform** | Rejected | Net-new SaaS dependency, billing, SSO setup. Org has zero precedent. Adds a new outage surface to a critical-path apply. No site-deployment job currently authenticates to TFC. |
| **Reuse `prod2-new-deployment-terraform-state` (the existing prod state bucket)** | Rejected | Violates the "no state reuse from america-e2e" core rule. Couples the new account's lifecycle to the old account's S3 bucket and IAM. Cross-account state read requires a bucket policy change in prod-v2, owned by a different IAM admin. Failure modes leak across accounts. |
| **Per-env (gardenio-prod, gardenio-stage) buckets in the same new account** | **Subsumed** | This is the recommendation. `env_cfg.hcl` overrides the bucket per-env exactly as `prod` does today; the bootstrap leaf creates each one. |
| **Cross-region replication for the state bucket** | Deferred | The existing site-deployment buckets are not replicated. Recovery story is "restore from versioning". Same posture for gardenio. Revisit only if disaster-recovery RTO requires it. |
| **OpenTofu state backend** | Out of scope | Tooling choice (opentofu vs terraform binary) is orthogonal; backend protocol is identical. Defer to Epic 9. |

### Consequences

- The gardenio module's `versions.tf` declares `terraform { required_version = ">= 1.5.0" }` and `required_providers { aws = "~> 5.0" }` (per Decision 3 / prior-art §4). It does **not** contain a `backend` block. Adding one would conflict with Terragrunt's `generate "backend_opt_in"`.
- Bootstrap leaf must be authored as part of Epic 1 (ka-0000001315 — AWS account & Org baseline). It is a precondition for the first plan in the new account.
- State migration is not in scope for this module. Epic 10 (cutover) handles any data movement; for net-new resources there is no state to migrate.

---

## Decision 2 — Versioning & caller pinning: **git tag on `gardenio/`, callers pin by tag**

### Recommendation

Adopt **semver git tags** on the `gardenio/` repo for the Terraform module. Callers pin by tag using Terragrunt's `git::` source syntax:

```hcl
# site-deployment/terraform/live/<env>/.../gardenio/terragrunt.hcl
terraform {
  source = "git::ssh://git@github.com/mattbakerconsulting/gardenio.git//gardenio/terraform/composition?ref=tfmod-v1.2.0"
}
```

**Tag convention:** `tfmod-vMAJOR.MINOR.PATCH`. The `tfmod-` prefix namespaces these tags away from the existing gardenio **app** calver tags (`YYYY.DDD.N`) so the release pipeline does not pick up TF-module tag pushes and try to ship containers. Existing release tooling matches `[0-9]{4}\.[0-9]+\.[0-9]+` and will not match `tfmod-*`.

**Semver policy** (the contract callers rely on):

| Bump | Triggered by |
|---|---|
| MAJOR | Any breaking change to the module's input variable contract (renamed/removed input, changed type, changed default with behavior change), output contract (renamed/removed output, changed type), or a forced-replacement of a stateful resource (`terraform plan` shows `-/+` on RDS, EKS cluster, S3 buckets, IAM roles consumed by IRSA). |
| MINOR | New optional input with a safe default; new output; new submodule; new resource that is purely additive in `terraform plan`. |
| PATCH | Fixes that produce **no** plan change for an existing caller pinned at the prior MINOR — provider pin tightening, comment/docs/lint, refactoring with identical resource graph. |

**Breaking-change protocol:**

1. Author the change behind a new optional input with `default = <legacy behavior>`. Ship as MINOR.
2. Migrate callers to set the new input explicitly.
3. After all known callers are migrated, flip the default and bump MAJOR. Document in `CHANGELOG.md` at the module root.
4. Never delete an output without a MAJOR bump and a deprecation notice that shipped at least one MINOR before.

**Tag mechanics:** tags are pushed manually by the maintainer on `main` via a `task tfmod-tag VERSION=v1.2.0` shortcut (to be added in Epic 2 skeleton work). The CI plan job in `gardenio/` runs on the tagged commit and on every MR; the tag itself is the publish.

### Alternatives considered

| Option | Verdict | Reason |
|---|---|---|
| **Commit-SHA pinning only** | Rejected | Loses the breaking-vs-additive signal. Callers can't tell if a bump is safe. Reviewers reading a site-deployment MR see `?ref=a1b2c3d` and have to git-log against `gardenio/` to learn the diff scope. |
| **Internal Terraform registry (Artifactory / GitLab TF registry)** | Rejected for now | GitLab TF registry exists per project and can be revisited later. Standing it up requires CI to publish module tarballs on every tag, plus caller-side `source = "git::..."` → `source = "<registry>/..."` migration. Net-new infrastructure for marginal benefit while we have one consumer. **Add to Epic 2 backlog as a follow-up; not blocking.** |
| **Branch-based pinning (`?ref=main`)** | Rejected | Unstable. Any merge to `main` would change every caller's plan immediately. Defeats the point of having a versioned module and breaks change-isolation between the gardenio repo and site-deployment. |
| **Publish to public Terraform Registry** | Rejected | Module is org-internal (consumes org-internal pull secrets, IAM policies, SSO config). No reason to publish externally; would invite version-bump-by-stranger expectations. |
| **Git submodule of `gardenio/` inside `site-deployment`** | Rejected | Submodules are widely disliked and would couple the two repos at the working-tree level. Pinning by `?ref=` is the same outcome with none of the submodule UX cost. |
| **Calver tags (`tfmod-2026.116.0`)** | Rejected | The whole point of versioning here is to signal breaking changes to callers; calver doesn't. The app-level calver works because the app's release surface is "the running cluster", not a typed input contract. |

### Consequences

- The `tfmod-v*` tag namespace is reserved on `gardenio/`; release scripts must not consume it. **Verify before first tag** that the existing release pipeline regex won't accidentally catch `tfmod-v*` (current `release/release.sh` matches calver `YYYY.DDD.N`, so we are safe — but Epic 2 skeleton work should add a CI guard / explicit allowlist).
- A `CHANGELOG.md` at the module root is part of the skeleton (Epic 2 deliverable). The PR template for the module directory must require a changelog entry.
- Initial tag is **`tfmod-v0.1.0`** — pre-1.0 to signal "experimental, breaking changes expected". Callers may pin `?ref=main` only inside the gardenio repo's own ephemeral test invocations, never from `site-deployment`.

---

## Decision 3 — Test tooling: **`fmt -check` + `validate` + `tflint` on PR; defer terratest**

### Recommendation

Wire three checks into the gardenio-repo CI for the module directory, gating MR merges:

1. **`terraform fmt -check -recursive`** — fails on any unformatted HCL. Org currently has the task but doesn't gate; we will. Catches formatting drift before review.
2. **`terraform validate`** — per submodule. Catches type errors and missing required-providers / required-version constraints.
3. **`tflint`** with the `terraform-linter/tflint-ruleset-aws` plugin — catches AWS-specific anti-patterns (deprecated resources, invalid instance types, missing required tags). Use a committed `.tflint.hcl` at the module root.

Plan-review remains the gate that catches resource-shape regressions (org standard). Plans run in `gardenio/` CI as a smoke test against a mock-providers fixture (no AWS auth in this repo); real plans run in `site-deployment` CI per the existing pattern.

**`tfsec` / `checkov` / `trivy config`:** add as a **non-blocking** report in CI for the first quarter post-cutover. Promote to blocking only after the rule list is curated and false-positive baselines are committed. Tracked as a follow-up in Epic 2 backlog.

**`terratest` / `kitchen-terraform` / ephemeral-account smoke:** **defer.** Reasons:

- Requires a sandbox AWS account budgeted by FinOps (Epic 11), which doesn't exist yet.
- Real-cluster smoke takes 20-40 min per run and would block PR throughput.
- Module is initially consumed by exactly one caller (gardenio-prod via site-deployment). Plan-review on the caller's MR exercises the module against real production-shaped inputs, which is a stronger signal than terratest against synthetic inputs.
- Revisit when (a) a sandbox account is funded, or (b) a second consumer adopts the module — at that point the caller-side plan-review no longer covers all input shapes.

### Alternatives considered

| Option | Verdict | Reason |
|---|---|---|
| **Hold the org line: validate-only, no extra gates** | Rejected | The org default is local-only; nothing is gated. Any new module deserves at least the gates the org already has tooling for (`fmt`, `validate`) actually wired into CI. The marginal cost is low, the catch rate on basic regressions is high. |
| **Adopt terratest from day 1** | Rejected | See "defer" above. Without a sandbox account, terratest either runs against america-e2e (violates "no state reuse" rule and is slow) or runs against mocks (which is what `validate` already does). |
| **`tfsec` blocking from day 1** | Rejected for v0 | First-pass tfsec runs against this kind of module produce ~30-50 findings, mostly tag-policy or KMS-scope nits requiring per-rule judgment. Blocking on day 1 turns the first MR into a tfsec-tuning marathon. Promote to blocking after baseline curation. |
| **`checkov` instead of `tflint` + `tfsec`** | Rejected | Overlapping coverage with tflint+tfsec, larger rule set, slower. Nothing wrong with checkov but no reason to pick it over the more focused two-tool combo. Re-evaluate if a rule we want exists only in checkov. |
| **`OpenTofu test` (1.6+) for unit tests** | Deferred | Promising; could replace terratest for fixture-based assertions. Org currently runs Terraform binary, not OpenTofu. Pin this to a follow-up after tooling-binary decision (out of scope for this ADR). |
| **Pre-commit hook for fmt/validate** | Complementary | Add as a developer convenience but not as the gate of record. CI is authoritative because pre-commit is bypassable. |

### Consequences

- The gardenio-repo CI gains a `tf-module-check` job running `fmt -check`, `validate`, `tflint` per submodule. RWX or GitLab CI — implementation TBD by Epic 2 skeleton task; the prior-art survey suggests reusing `cicd-templates/terraform` for shape but stripping out the apply half (we never apply from this repo).
- The module ships a `.tflint.hcl` and a CONTRIBUTING-style README section explaining what each gate catches. New contributors can run all three locally via a `task tf-check` shortcut.
- We accept that v0 of the module ships without terratest. Callers in site-deployment carry the shape-validation burden via plan review. Document this expectation in the module README so reviewers know what to look for.

---

## Decision 4 — Terraform version floor: **`required_version = ">= 1.5.0"`**

### Recommendation

Pin a Terraform version floor in the module's `versions.tf`:

```hcl
terraform {
  required_version = ">= 1.5.0, < 2.0.0"
  required_providers {
    aws = { source = "hashicorp/aws", version = "~> 5.0" }
  }
}
```

`>= 1.5.0` because: (a) `import { ... }` blocks (1.5+) are useful for the cutover plan in Epic 10, (b) the org's `terraform-tools` image ships ≥ 1.5 today (the live root pins `aws 5.81.0` which requires terraform ≥ 1.4), (c) the upper bound `< 2.0.0` reserves a future MAJOR break.

The provider pin **`aws = "~> 5.0"`** is widened from the live root's exact `5.81.0` so this module remains callable from any `live/` root that pins 5.x — including future minor bumps. Tighten only if a specific resource requires a newer minor.

### Alternatives considered

| Option | Verdict | Reason |
|---|---|---|
| Leave `required_version` unset (org default) | Rejected | Implicit floor "whatever the CI image ships" — fragile across CI image bumps and impossible for a caller to verify locally. A new module is a clean slate; pin it. |
| Pin exactly to `5.81.0` (match live root) | Rejected | Couples the module to a specific live-root pin, defeating the cross-repo version independence we're setting up. `~> 5.0` is the safest and matches the org's looser-pin convention inside `new-modules/base/foundation`. |
| Pin to a 1.7+ floor for `terraform test` | Rejected for v0 | We're deferring terratest; pinning for a feature we don't use is premature. Bump to 1.6+ when adopting `terraform test` (Decision 3 follow-up). |

### Consequences

- `versions.tf` is part of every submodule + the top-level composition. Skeleton work in Epic 2 must enforce this in CI (a tflint custom rule or a shell check).
- If `site-deployment` ever bumps below 1.5.0, the module call breaks fast at `terraform init` time — desired behavior.

---

## Open follow-ups (not in this ADR)

- **Module directory layout** under `gardenio/` — sibling task in Epic 2 (not yet filed).
- **Input/output contract** — sibling task ka-0000002543.
- **Bootstrap leaf in site-deployment for the new state bucket** — depends on Epic 1 (ka-0000001315) account creation.
- **GitLab TF registry adoption** — backlog item; revisit when a second module consumer appears.
- **`tfsec`/`checkov` baseline curation** — backlog; promote to blocking gate post-cutover.
- **Terratest re-evaluation** — gated on FinOps sandbox account (Epic 11 / ka-0000007484).

## Ratification

This ADR is **Proposed**. It moves to **Accepted** when the human operator ratifies via `gb decision` against bead ka-0000003471 (or comments approval on the merging MR). After acceptance, sibling Epic 2 tasks (skeleton, layout, CI wiring) consume these decisions as fixed input.
