# modules/ecr-pull-through-cache — mirror upstream public registries (GitHub
# Container Registry, Docker Hub) into this account's ECR via pull-through
# cache rules, so node image pulls land on the FREE S3-gateway / ECR-interface
# endpoint path instead of traversing the NAT instance.
#
# Why (ADR docs/research/canonical-prod-nat-elimination.md §6, edges P3/P6/P14):
#   - GHCR + Docker Hub image pulls are the single highest-leverage NAT-byte
#     reduction. A pull-through cache rule makes the first pull of an upstream
#     image fetch-and-store it under `<prefix>/...` in this account's ECR; every
#     subsequent pull (by any node) is served from ECR over the VPC endpoint —
#     $0 NAT bytes, and no Docker Hub rate-limit exposure.
#
# What this module provisions:
#   - One Secrets Manager credential secret per credentialed upstream, named with
#     the AWS-mandated `ecr-pullthroughcache/` prefix. VALUES ARE POPULATED
#     OUT-OF-BAND (same paths-only convention as modules/secrets-manager): the
#     operator writes `{"username":"...","accessToken":"..."}` after apply.
#     AWS requires a credential for both GHCR and Docker Hub upstreams.
#   - One `aws_ecr_pull_through_cache_rule` per upstream.
#   - One IAM policy granting the EKS node role the pull + first-pull-create
#     permissions the AmazonEC2ContainerRegistryReadOnly managed policy lacks
#     (CreateRepository / BatchImportUpstreamImage / TagResource), scoped to the
#     cache repo prefixes. Output as a policy ARN; the composition attaches it to
#     the Karpenter node role (keeps this module decoupled from modules/karpenter).
#
# Toggle: `enabled=false` (default) provisions nothing — opt in per composition
# once the credential secrets can be populated. Apply order: create the secrets
# (this module), populate their values out-of-band, THEN the cache rules validate
# against live upstream creds. validate-only runs need no values.

locals {
  module_tags = merge(
    var.common_tags,
    {
      "gardenio:module" = "ecr-pull-through-cache"
      "gardenio:env"    = var.env_name
    },
  )

  enabled = var.enabled

  # The node grant is separable from the cache itself: a cluster module may
  # already carry these verbs inline on its node role (modules/k3s does), and
  # two overlapping grants is worse than one.
  node_policy = local.enabled && var.create_node_policy

  # Registered per upstream so EVERY repo auto-created by a first pull inherits
  # the policy. Without it only repos that already exist can be fixed, and each
  # new cached image starts the problem again.
  untagged_expiry = local.enabled && var.untagged_expiry_days > 0

  cache_lifecycle_policy = jsonencode({
    rules = [{
      rulePriority = 1
      description  = "Pull-through cache: expire untagged images after ${var.untagged_expiry_days} days. In a cache an untagged image is a superseded copy (the tag moved upstream), so it is pure storage. ${var.untagged_expiry_days} days leaves a rollback window."
      selection = {
        tagStatus   = "untagged"
        countType   = "sinceImagePushed"
        countUnit   = "days"
        countNumber = var.untagged_expiry_days
      }
      action = { type = "expire" }
    }]
  })

  # Upstreams keyed by logical name. Every gardenio upstream (GHCR, Docker Hub)
  # requires a credential, so each gets a paths-only SM secret.
  upstreams = local.enabled ? var.upstreams : {}

  credentialed = { for k, u in local.upstreams : k => u if u.requires_credential }

  account_id = data.aws_caller_identity.current.account_id
  region     = data.aws_region.current.name
  partition  = data.aws_partition.current.partition

  # ECR ARNs for the cache repos auto-created under each prefix (first pull).
  cache_repo_arns = [
    for k, u in local.upstreams :
    "arn:${local.partition}:ecr:${local.region}:${local.account_id}:repository/${u.ecr_repository_prefix}/*"
  ]
}

data "aws_caller_identity" "current" {}
data "aws_region" "current" {}
data "aws_partition" "current" {}

# Paths-only credential secrets. AWS mandates the `ecr-pullthroughcache/`
# name prefix for any secret referenced by a pull-through cache rule's
# credential_arn. Values populated out-of-band (see README).
resource "aws_secretsmanager_secret" "upstream" {
  for_each = local.credentialed

  name                    = "ecr-pullthroughcache/${coalesce(each.value.secret_name_suffix, "${var.env_name}-${each.key}")}"
  description             = "${var.env_name} ECR pull-through cache upstream creds: ${each.key} (${each.value.upstream_registry_url}) — value populated out-of-band"
  kms_key_id              = var.kms_key_arn
  recovery_window_in_days = var.recovery_window_days

  tags = merge(local.module_tags, {
    Name               = "ecr-pullthroughcache-${var.env_name}-${each.key}"
    "gardenio:purpose" = "ecr-pull-through-cache-credential"
  })
}

resource "aws_ecr_pull_through_cache_rule" "this" {
  for_each = local.upstreams

  ecr_repository_prefix = each.value.ecr_repository_prefix
  upstream_registry_url = each.value.upstream_registry_url
  credential_arn        = each.value.requires_credential ? aws_secretsmanager_secret.upstream[each.key].arn : null
}

# Repository creation template: ECR applies this to each repo it auto-creates
# under the prefix on a first pull, which is the only way a cache repo can be
# born with a lifecycle policy. Fixing existing repos one by one does not stop
# the next cached image from starting untagged-copy accumulation again.
resource "aws_ecr_repository_creation_template" "this" {
  for_each = local.untagged_expiry ? local.upstreams : {}

  prefix      = each.value.ecr_repository_prefix
  description = "Pull-through cache repos under ${each.value.ecr_repository_prefix}/: inherit untagged-expiry so a large image does not accumulate a copy per build."
  applied_for = ["PULL_THROUGH_CACHE"]

  image_tag_mutability = "MUTABLE"
  lifecycle_policy     = local.cache_lifecycle_policy

  encryption_configuration {
    encryption_type = "AES256"
  }
}

# Node-role policy: pull-through cache populates a repo on FIRST pull, which
# requires CreateRepository + BatchImportUpstreamImage on top of the standard
# read verbs (the AmazonEC2ContainerRegistryReadOnly managed policy grants
# neither). Scoped to the cache repo prefixes; GetAuthorizationToken is
# account-wide (no resource scoping available for that action).
data "aws_iam_policy_document" "node_pull_through" {
  count = local.node_policy ? 1 : 0

  statement {
    sid       = "EcrAuth"
    effect    = "Allow"
    actions   = ["ecr:GetAuthorizationToken"]
    resources = ["*"]
  }

  statement {
    sid    = "EcrPullThroughCache"
    effect = "Allow"
    actions = [
      "ecr:GetDownloadUrlForLayer",
      "ecr:BatchGetImage",
      "ecr:BatchCheckLayerAvailability",
      "ecr:BatchImportUpstreamImage",
      "ecr:CreateRepository",
      "ecr:TagResource",
    ]
    resources = local.cache_repo_arns
  }
}

resource "aws_iam_policy" "node_pull_through" {
  count       = local.node_policy ? 1 : 0
  name        = "${var.env_name}-ecr-pull-through-cache-node"
  description = "Lets EKS nodes populate ECR pull-through cache repos on first pull (CreateRepository + BatchImportUpstreamImage)."
  policy      = data.aws_iam_policy_document.node_pull_through[0].json
  tags        = local.module_tags
}
