# gardenio on AWS — setup guide

This guide stands up a complete gardenio environment in a **fresh, dedicated
AWS account**, from zero to a reachable `https://<env>.<your-domain>` running
the gardenio control plane.

It is written for an external adopter. Where a value or step is specific to
the gardenio project's own account (e.g. the `gardenio.acme.example` DNS zone), it
is called out as **[gardenio-internal]** with the generic alternative.

> **Maturity note.** This Terraform was built and proven against gardenio's
> own AWS POC account. It is fully functional, but a handful of defaults are
> still gardenio-flavored — see
> [`docs/terraform-oss-readiness.md`](../../docs/terraform-oss-readiness.md) for
> the genericization backlog. None of it blocks a determined adopter; you
> just override more variables today than you will once that work lands.

---

## 0. What you get

One `terraform apply` (across the bootstrap + composition roots) provisions:

- A 3-AZ VPC (per-AZ NAT, S3 gateway endpoint).
- An **EKS** cluster (1.30) with IRSA, an on-demand infra node group, and a
  **Karpenter**-managed Spot agent pool.
- **RDS PostgreSQL 16** (Multi-AZ, pgvector) backing beads + the controller.
- An **S3** sessions bucket and **KMS** CMKs, all SSE-encrypted.
- **Secrets Manager** secret shells (operator loads the values out-of-band).
- In-cluster platform addons: AWS Load Balancer Controller, external-dns,
  external-secrets-operator, cert-manager, VictoriaMetrics.
- A **Route53** public hosted zone + an **ACM** wildcard cert (DNS-01).
- The **gardenio Helm chart** itself, wired to all of the above.
- Optional **AWS Verified Access** edge auth in front of the load balancer.

---

## 1. Prerequisites

| Requirement | Notes |
|---|---|
| A dedicated AWS account | Strongly recommended — gardenio owns the whole account. |
| Admin credentials for that account | An SSO role or an IAM user with admin. The CLI must `aws sts get-caller-identity` cleanly. |
| `terraform >= 1.6` | The version floor. |
| `aws` CLI v2 | Used by the providers' `eks get-token` exec plugin. |
| `kubectl`, `helm` | For post-apply verification. |
| A registrable parent domain you control | You must be able to add an `NS` record to delegate a subdomain to the Route53 zone Terraform creates. |
| Access to the gardenio container images / Helm chart | The chart is published to an OCI registry; see §6. |

---

## 2. Bootstrap the state backend (once per account)

Every composition root stores state in S3 with a DynamoDB lock. Those two
resources are created by the `bootstrap/` root, which itself runs on a
**local** backend the first time (chicken-and-egg).

```sh
cd gardenio/terraform/bootstrap
# Edit variables.tf defaults or pass -var:
#   state_bucket_name = "<org>-gardenio-tfstate"
#   lock_table_name   = "<org>-gardenio-tflock"
#   region            = "us-east-1"
terraform init
terraform apply
```

> **[gardenio-internal]** The committed defaults are
> `gardenio-customer-poc-tfstate` / `gardenio-customer-poc-tflock` and a
> `gardenio:account` tag pinned to gardenio's account ID. Override all three
> for your own account.

After this applies you may optionally migrate the bootstrap state itself
into the new bucket (`terraform init -migrate-state` with an `-backend-config`
pointing at the bucket); for a single-operator setup the local
`terraform.tfstate` is fine to keep.

---

## 3. Configure the environment composition

```sh
cd ../composition/poc
cp terraform.tfvars.example terraform.tfvars
```

Edit `terraform.tfvars`. The greenfield-minimal set:

```hcl
env_name = "poc"                 # short env slug; names get derived from this
region   = "us-east-1"

vpc_cidr              = "10.70.0.0/16"
az_count              = 3
nat_mode              = "per-az"        # "single" is cheaper for a throwaway env
vpc_endpoint_services = ["s3"]

eks_version    = "1.30"
operator_email = "ops@example.com"      # ACME + budget alerts

common_tags = {
  "gardenio:env"         = "poc"
  "gardenio:owner"       = "platform"
  "gardenio:cost-center" = "platform"
}
```

DNS / chart inputs (see `composition/poc/variables.tf` for the full list):

```hcl
# The parent hosted zone Terraform will CREATE in this account.
parent_zone_name = "gardenio.example.com"   # <-- your domain
env_subdomain    = "poc"                    # workload host = poc.gardenio.example.com

gardenio_namespace     = "gardenio-system"
gardenio_image_tag     = "2026.156.23"       # pin a real calver — apply fails loudly if empty
# gardenio_chart_version = ""                 # empty keeps the module-local golden pin
```

> **[gardenio-internal]** `parent_zone_name` defaults to `gardenio.acme.example` and
> `env_subdomain` to `aws-poc`. There is no apex-zone automation — Terraform
> creates the *parent* zone but cannot touch your registrar / apex, so you do
> the NS delegation manually in §5.

### Backend config

`composition/poc/providers.tf` declares `backend "s3" {}` with no inline
config. Supply it at init time pointing at the bucket from §2:

```sh
terraform init \
  -backend-config="bucket=<org>-gardenio-tfstate" \
  -backend-config="key=poc/terraform.tfstate" \
  -backend-config="region=us-east-1" \
  -backend-config="dynamodb_table=<org>-gardenio-tflock"
```

---

## 4. Apply

The graph resolves cross-tier dependencies automatically, so a single
`terraform apply` works. For a first bring-up you can also go tier-by-tier
with `-target` to keep blast radius small and review each layer:

```sh
terraform plan          # review carefully on a fresh account
terraform apply
```

Expect the EKS control plane (~10 min) and RDS (~10–15 min) to dominate
wall-clock. The ACM cert validation will sit `pending` until you complete
the DNS delegation in §5 — that is expected.

---

## 5. Delegate DNS (one manual step)

Terraform creates the parent Route53 hosted zone but cannot edit your
registrar or apex zone. After the apply:

```sh
terraform output -json dns        # read .name_servers (four entries)
```

At your registrar / apex DNS, add an `NS` record for `parent_zone_name`
pointing at those four name servers (TTL 300). Then confirm:

```sh
dig +short NS gardenio.example.com @8.8.8.8          # must return the 4 NS
dig +short NS poc.gardenio.example.com @<one-of-them> # resolves through the zone
```

Once delegation propagates, ACM DNS-01 validation completes on its own and
the cert binds to the load balancer. cert-manager's `letsencrypt-prod`
ClusterIssuer is installed as a cross-check fallback.

> This delegation is a **one-time** step per parent zone. Additional
> environments are just more records inside the same zone — no new
> delegation needed.

---

## 6. Chart pull credentials

The gardenio Helm chart and images are published to a private OCI registry.
Provide a pull token to the `gardenio-chart` module without writing it to
disk:

```sh
export TF_VAR_gitlab_registry_token="$YOUR_REGISTRY_TOKEN"
terraform apply
```

Leaving it unset falls back to your local `helm registry login` credential
cache.

> **[gardenio-internal]** Images live under
> `<registry>/gardenio/`. An OSS consumer will
> point this at their own mirror / public registry once images are published
> publicly (tracked in the OSS-readiness doc).

---

## 7. Operator-loaded secrets

Terraform creates Secrets Manager secret **shells** with
`lifecycle.ignore_changes` so values you load out-of-band survive re-applies.
After the apply, load the real values (Slack/GitLab/OIDC creds, etc.) via the
console or `aws secretsmanager put-secret-value`. external-secrets-operator
then syncs them into the cluster. The shells that need values are listed in
the `stateful` / `edge-auth` outputs.

---

## 8. Verify

```sh
$(terraform output -raw kubeconfig_update_cmd)   # writes kubeconfig via aws eks
kubectl get nodes
kubectl get pods -n gardenio-system
curl -I "$(terraform output -raw gardenio_endpoint_url)"
```

The full acceptance checklist (every box independently verifiable from a
fresh checkout) lives in `gardenio/plans/customer-poc-aws-poc.md` §7.

---

## 9. Day-2 / teardown

- **Chart bumps** are a one-line change to `gardenio_image_tag` (or
  `gardenio_chart_version`) followed by `terraform apply`.
- **Teardown:** `terraform destroy` in the composition root, then the
  bootstrap root. RDS has `deletion_protection` and a final snapshot by
  default — clear those toggles first if you really want it gone.

---

## Edge auth (optional)

`modules/edge-auth` fronts the load balancer with **AWS Verified Access**
trusting an external OIDC IdP (gardenio uses `gitlab.com`). It is wired in
`composition/poc/edge_auth.tf` and toggled by
`edge_auth_create_endpoint` / `edge_auth_target_load_balancer_arn`. Leave it
off for a basic bring-up; enable it once the NLB ARN is known. The OIDC
client id/secret are loaded into a Secrets Manager shell out-of-band.
