# ── CI pushes images to ECR (plan 2026-10-21-aws-cost-cuts, Phase A) ─────
# Pushing from the CI nodes to ghcr was paid internet egress ($0.09/GB,
# 95-143 GB/day on 10-19/20). EC2 -> ECR in-region is free. Runner pods get
# AWS credentials from GitHub OIDC through STS; IMDS stays denied to them
# (imdsDeny, deploy-values/gardenio-k3s.yaml).

data "aws_caller_identity" "current" {}

locals {
  # The OIDC subject GitHub actually issues for this repo. The repo has
  # use_immutable_subject on, so the sub starts with the owner and repo IDs, not
  # the names, and a "repo:mattbakerconsulting/gardenio:..." condition never
  # matches (STS: "The web identity token provided could not be validated",
  # A3 PR run 39694891467). From
  # `gh api repos/mattbakerconsulting/gardenio/actions/oidc/customization/sub`
  # (2026-10-21, gd-VAyugfDaI5):
  #   use_default: true, use_immutable_subject: true,
  #   sub_claim_prefix: "repo:mattbakerconsulting@334111061/gardenio@1360966138"
  # Renaming the owner or the repo does not change it; recreating either does.
  # Re-check with .github/workflows/oidc-sub-probe.yml, which prints the
  # decoded sub (never the token). tests/ci_ecr_oidc_sub.tftest.hcl pins it.
  github_oidc_sub_prefix = "repo:mattbakerconsulting@334111061/gardenio@1360966138"
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

# Release role: calver tag pushes (publish), pushes to main (refresh the
# build caches), and workflow_dispatch runs on agent/* branches: the
# sanctioned `gb ci dispatch images.yml --ref agent/<branch>` that proves a
# Dockerfile change before the release tag (escalation gd-pp0AEoYGwp; run
# 41759454364 on PR #2196 failed at AssumeRoleWithWebIdentity because only
# main and calver tags were trusted). Such a run pushes only :<sha> tags,
# which nothing consumes (global.latestMode is false). Assumes the immutable
# subject above and no GitHub environments on these jobs; an environment
# would change the sub.
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
        "${local.github_oidc_sub_prefix}:ref:refs/tags/20*",
        "${local.github_oidc_sub_prefix}:ref:refs/heads/main",
        "${local.github_oidc_sub_prefix}:ref:refs/heads/agent/*",
      ]
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
