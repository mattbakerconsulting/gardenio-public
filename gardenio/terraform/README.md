# gardenio-terraform

Terraform for the live gardenio AWS account: the VPC, the k3s cluster on EC2
spot, budgets, ECR, the edge probes and SES. The cluster is k3s; the EKS
stack (composition/poc, canonical-prod and their modules) was applied,
destroyed on 2026-09-16 and deleted from the tree. Its from-zero guide is
archived at [`docs/archive/terraform-eks-poc-setup.md`](../../docs/archive/terraform-eks-poc-setup.md).

## Layout

- `composition/gardenio/` — the one composition root, the live account.
- `modules/` — pure-TF submodules, each called by `composition/gardenio`.
  No `backend` block, no `provider` block.
  - `network/` — VPC, subnets, NAT instances, route tables, VPC endpoints
  - `k3s/` — k3s servers and agent pools on EC2 spot
  - `finops/` — budgets and cost alerts
  - `hq/` — the HQ supervisor host
  - `edge-probes/` — external uptime probes (Lambda)
  - `infra-aws-readonly/` — read-only AWS for the infra agent tier
  - `ses-identity/`, `ses-support-inbound/` — mail sending and the support inbox
  - `ecr-pull-through-cache/` — ECR pull-through cache for public registries
- `bootstrap/` — account-level bootstrap: the S3 state bucket and
  DynamoDB lock table that `composition/gardenio`'s `backend "s3"`
  needs. Applied once per account with a **local** backend, then its
  state is migrated into the bucket it just created.

## Conventions

- `terraform >= 1.6` and `hashicorp/aws ~> 5.0` (see `versions.tf` in each
  root for the full provider-pin set).
- **No `backend` blocks inside `modules/`.** Backends live only in
  `composition/<env>/` and `bootstrap/`.
- **One state file per composition root**, keyed under the shared bucket,
  so the blast radius of an `apply` is bounded.
- Common tags flow via `var.common_tags`, merged with a per-module
  `gardenio:module` tag inside each submodule, per the tagging policy in
  `docs/research/canonical-prod-aws-account.md` §1.

## Quick start

```sh
# 0. bootstrap state backend (once per account, local backend)
cd gardenio/terraform/bootstrap
terraform init && terraform apply

# 1. init the environment composition (once per checkout)
cd ../composition/gardenio
terraform init                                  # uses the bootstrap bucket

# 2. plan and apply it -- through the wrapper, from the repo root
cd ../../../..
./scripts/tf-gardenio.sh plan
./scripts/tf-gardenio.sh apply -target=module.k3s
```

### Use `scripts/tf-gardenio.sh`, not bare `terraform`

`composition/gardenio` is the live account. The wrapper runs
[`scripts/check-tf-tree-freshness.sh`](../../scripts/check-tf-tree-freshness.sh)
first and **refuses, non-zero, before terraform is invoked at all** unless the
checkout is exactly `origin/main`. It fetches as part of that check, so it
cannot be satisfied by a stale remote-tracking ref.

This is not hygiene. Measured 2026-09-23: a `terraform plan` from a checkout
about two hours old reported `1 to add, 3 to change, 6 to destroy`, where the
six destroys were the entire ziti public edge node and the one add was a
superseded world-open `0.0.0.0/0` ingress rule returning to the shared nodes
security group. Neither change existed in that tree, so a `prevent_destroy`
lifecycle block could not have helped — there was no resource to attach it to.
The same tree at `origin/main`: `0 to add, 3 to change, 0 to destroy`.

The guard covers `plan` as well as `apply`, because the stale plan above was
nearly read as evidence about `main`. It also refuses on unpushed local commits;
pass `--allow-unpushed` **first** if that is deliberate:

```sh
./scripts/tf-gardenio.sh --allow-unpushed plan
```

A dirty working tree is deliberately *not* blocked — editing and re-planning is
normal work, and blocking it would get the wrapper routed around.

**Separately, and not enforced by the wrapper:** `apply` on this composition is
expected to be `-target`ed. The residual untargeted plan currently touches the
agents ASG `max_size` and the AMI on the `agent` and `server` launch templates,
and the servers are the etcd voters. That is an operational judgement about
today's drift rather than an invariant, which is why it is written here instead
of encoded as a check.

## Validate locally (no AWS calls)

```sh
cd gardenio/terraform/composition/gardenio && terraform init -backend=false && terraform validate
cd gardenio/terraform/bootstrap            && terraform init -backend=false && terraform validate
```
