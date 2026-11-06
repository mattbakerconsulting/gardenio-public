# bootstrap

Account-level bootstrap that creates the resources every other
composition needs *before* it can run:

1. **State bucket** — S3 bucket holding `<state-key>.tfstate` for the
   `composition/<env>/` roots.
2. **Lock table** — DynamoDB table backing the S3 backend's lock
   handle.

> **Scope (POC tier (a), ka-0000002293):** S3 bucket + DynamoDB lock table only.
> The GitLab OIDC provider + IAM trust roles are deferred to a follow-up MR
> once the GitLab-CI assume-role pattern is wired (see plan-of-record §5,
> `gardenio/plans/canonical-prod-aws-poc.md`).

## Chicken-and-egg

This composition is special because the state bucket it creates is the
state bucket *its own state* will eventually live in. Bootstrapping
sequence:

1. First apply: backend is `local` (no `backend` block). Creates the
   bucket + lock table.
2. Migrate state into the new bucket: `terraform init -migrate-state`
   pointing at `backend.s3` with the bucket from step 1.
3. Subsequent applies are remote-stated alongside every other
   composition.

This is the same pattern site-deployment uses in
`live/common/operations/`. See
`docs/research/canonical-prod-tf-module-architecture.md` §2 for the
prior-art trace.

## Run procedure (POC, account 123456789012, us-east-1)

Per operator Matt Baker (Slack, 2026-05-15), the POC uses long-lived
IAM keys delivered at AWS Secrets Manager
`gardenio/customer-poc-aws-creds` (the SM secret itself is hosted in
the fleet account `234567890123`, but the keys it contains
authenticate as IAM user `gardenio` in the customer-poc POC account
`123456789012` — see retarget decision ka-0000001393). The
state-backend bootstrap is otherwise operator-only per plan §8 — this
is a one-time deviation for POC scope.

```sh
# 1. Fetch creds from Secrets Manager (operator workstation; do NOT
#    paste output anywhere).
aws secretsmanager get-secret-value \
  --secret-id gardenio/customer-poc-aws-creds \
  --region us-east-1 \
  --query SecretString --output text > /tmp/poc-creds.json

export AWS_ACCESS_KEY_ID="$(jq -r .AWS_ACCESS_KEY_ID /tmp/poc-creds.json)"
export AWS_SECRET_ACCESS_KEY="$(jq -r .AWS_SECRET_ACCESS_KEY /tmp/poc-creds.json)"
export AWS_DEFAULT_REGION=us-east-1
rm /tmp/poc-creds.json

# Sanity-check the account.
aws sts get-caller-identity   # → Account: 123456789012 (user/gardenio)

# 2. Init with local backend (no backend block in providers.tf).
cd gardenio/terraform/bootstrap
terraform init

# 3. Plan + apply.
terraform plan -out=bootstrap.tfplan
terraform apply bootstrap.tfplan

# 4. (One-time) migrate state into the bucket we just created.
#    Drop a backend.tf with:
#
#      terraform {
#        backend "s3" {
#          bucket         = "gardenio-customer-poc-tfstate"
#          key            = "bootstrap/terraform.tfstate"
#          region         = "us-east-1"
#          dynamodb_table = "gardenio-customer-poc-tflock"
#          encrypt        = true
#        }
#      }
#
#    Then:
terraform init -migrate-state
```

Step 4 is **out of scope for this MR** — it ships as a separate task
after the first apply succeeds.

## Inputs

- `region` — AWS region (default `us-east-1`).
- `state_bucket_name` — default `gardenio-customer-poc-tfstate`.
- `lock_table_name` — default `gardenio-customer-poc-tflock`.
- `common_tags` — default includes `gardenio:env`, `gardenio:account`,
  `gardenio:managed` tags.

## Outputs

- `state_bucket_name`, `state_bucket_arn`
- `lock_table_name`, `lock_table_arn`

## Out of scope (deferred to follow-up MRs)

- `aws_iam_openid_connect_provider` for GitLab CI assume-role.
- Bucket lifecycle rules (noncurrent-version expiration, multipart-upload
  cleanup) — adds in a hardening pass.
- KMS-CMK SSE — POC uses SSE-S3 for simplicity; CMK upgrade is a
  follow-up.
- State migration into the S3 backend (see step 4 above).
