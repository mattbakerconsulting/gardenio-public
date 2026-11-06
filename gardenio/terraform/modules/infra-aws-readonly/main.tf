# infra-aws-readonly — read-only AWS for the infra tier (gd-83VraF4SeC, decision D3
# `irsa-readonly`: full read of the account, secret paths denied).
#
# WHY A SCOPED STATIC KEY, NOT IRSA / ROLES ANYWHERE (written down per the task):
#   - IRSA is impossible here. The k3s API server's service-account issuer is
#     https://kubernetes.default.svc.cluster.local, which AWS STS cannot fetch, and no IAM
#     OIDC provider exists (see deploy-values/gardenio-k3s.yaml, "IRSA is impossible").
#     Making it possible means changing the k3s server flags and re-rolling every server
#     (node-image change), which is far outside a read-only-diagnostics task.
#   - IAM Roles Anywhere needs a private CA, a trust anchor, a profile and a client
#     certificate delivered to the pod as FILES plus aws_signing_helper in the image. The
#     controller injects role secrets as environment variables only (applyRoleSecrets), so
#     that path needs a controller change in runtime-authz code. Not warranted here.
#   - The node role over IMDS is NOT an option: every pod can assume it today, and PR #270
#     removes that path. A pod must never depend on it for its own identity.
#   - A static key reaches the pod through the mechanism that already exists: a ROLE bead
#     `secrets` entry (env AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY from a K8s Secret named
#     "<role>-…"). The key is minted OUT OF BAND (README) so it never enters terraform state.
#
# Trade-off accepted: the key is long-lived (rotate on a schedule, README) and anyone who
# can run code in an infra pod can read it. That is why the policy below is the real
# boundary: read-only, with an explicit Deny on every action that returns secret material.

locals {
  # name => { namespace, serviceaccount }
  subjects = {
    for s in var.subjects : split("/", s)[1] => {
      namespace      = split("/", s)[0]
      serviceaccount = split("/", s)[1]
    }
  }

  name = "${var.env_name}-infra-aws-readonly"

  # Every action here returns secret material, or credentials, or code with embedded
  # secrets. A Deny beats any Allow, including everything ReadOnlyAccess grants.
  secret_read_actions = [
    # Secrets Manager values.
    "secretsmanager:GetSecretValue",
    "secretsmanager:BatchGetSecretValue",
    # SSM Parameter Store. IAM cannot condition on WithDecryption, so the whole read family
    # is denied (SecureString values, incl. /<env>/k3s/token and /<env>/k3s/kubeconfig).
    "ssm:GetParameter",
    "ssm:GetParameters",
    "ssm:GetParametersByPath",
    "ssm:GetParameterHistory",
    # Decryption of anything KMS-encrypted.
    "kms:Decrypt",
    "kms:ReEncryptFrom",
    # Lambda: GetFunction returns a pre-signed URL to the code package, and the function
    # configuration (also returned by ListFunctions / ListVersionsByFunction) carries the
    # environment variables in clear text.
    "lambda:GetFunction",
    "lambda:GetFunctionConfiguration",
    "lambda:ListFunctions",
    "lambda:ListVersionsByFunction",
    # IAM credential report: every user's key and password state.
    "iam:GetCredentialReport",
    # ECR: a 12 h registry credential, and the image layers themselves (which carry
    # whatever was baked into an image), which ReadOnlyAccess otherwise grants.
    "ecr:GetAuthorizationToken",
    "ecr:BatchGetImage",
    "ecr:GetDownloadUrlForLayer",
    # SSM Run Command history: retained command output (kubeconfig reads, `get secret`).
    "ssm:ListCommands",
    "ssm:ListCommandInvocations",
    "ssm:GetCommandInvocation",
    # Minting a fresh temporary credential from the static key would outlive a revoke of
    # the key's group membership, so it is denied.
    "sts:GetSessionToken",
    # CloudWatch Logs content (EKS control-plane and lambda groups). Log GROUP and STREAM
    # listings stay readable. GetLogEvents and FilterLogEvents are denied outside
    # var.readable_log_groups by the DenyLogReadsOutsideReadableGroups statement instead.
    # The Insights query pair stays denied everywhere: GetQueryResults takes no resource.
    "logs:StartQuery",
    "logs:GetQueryResults",
    "logs:StartLiveTail",
    # EC2 Windows administrator password blob.
    "ec2:GetPasswordData",
  ]

  # Log-content reads that are allowed on var.readable_log_groups and denied on every
  # other group. A group ARN is matched both bare and with ":*" (its streams).
  log_read_actions = ["logs:GetLogEvents", "logs:FilterLogEvents"]
  readable_log_group_arns = flatten([
    for g in var.readable_log_groups : [
      "arn:aws:logs:*:*:log-group:${g}",
      "arn:aws:logs:*:*:log-group:${g}:*",
    ]
  ])
}

resource "aws_iam_group" "this" {
  name = local.name
  path = "/gardenio/"
}

resource "aws_iam_group_policy_attachment" "read_only_access" {
  group      = aws_iam_group.this.name
  policy_arn = "arn:aws:iam::aws:policy/ReadOnlyAccess"
}

resource "aws_iam_policy" "deny_secret_reads" {
  name        = "${local.name}-deny-secret-reads"
  path        = "/gardenio/"
  description = "Explicit Deny on every secret-returning read, attached beside ReadOnlyAccess (gd-83VraF4SeC)."
  tags        = var.tags

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid      = "DenySecretReads"
        Effect   = "Deny"
        Action   = local.secret_read_actions
        Resource = "*"
      },
      {
        # Object bytes in EVERY bucket: state and backups live in buckets that a per-bucket
        # list misses (review of #302 found four). Listing and bucket config stay allowed.
        Sid      = "DenyAllObjectReads"
        Effect   = "Deny"
        Action   = ["s3:GetObject", "s3:GetObjectVersion", "s3:GetObjectTorrent"]
        Resource = "arn:aws:s3:::*/*"
      },
      {
        Sid         = "DenyLogReadsOutsideReadableGroups"
        Effect      = "Deny"
        Action      = local.log_read_actions
        NotResource = local.readable_log_group_arns
      },
    ]
  })
}

resource "aws_iam_group_policy_attachment" "deny_secret_reads" {
  group      = aws_iam_group.this.name
  policy_arn = aws_iam_policy.deny_secret_reads.arn
}

# Cost and metrics reads (P9.4, gd-brIH1y5J9S). ReadOnlyAccess already covers most of
# these; naming them keeps the grant true if AWS narrows the managed policy, and the logs
# Allow is the only Allow the DenyLogReadsOutsideReadableGroups carve-out leaves open.
resource "aws_iam_policy" "cost_and_metrics_read" {
  name        = "${local.name}-cost-and-metrics-read"
  path        = "/gardenio/"
  description = "Cost Explorer read, CloudWatch metric read, and log-content read on the gardenio log groups (gd-brIH1y5J9S)."
  tags        = var.tags

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid      = "CostExplorerRead"
        Effect   = "Allow"
        Action   = ["ce:Get*", "ce:Describe*", "ce:List*"]
        Resource = "*"
      },
      {
        Sid      = "CloudWatchMetricsRead"
        Effect   = "Allow"
        Action   = ["cloudwatch:GetMetricData", "cloudwatch:GetMetricStatistics", "cloudwatch:ListMetrics", "cloudwatch:DescribeAlarms"]
        Resource = "*"
      },
      {
        Sid      = "GardenioLogGroupsRead"
        Effect   = "Allow"
        Action   = local.log_read_actions
        Resource = local.readable_log_group_arns
      },
    ]
  })
}

resource "aws_iam_group_policy_attachment" "cost_and_metrics_read" {
  group      = aws_iam_group.this.name
  policy_arn = aws_iam_policy.cost_and_metrics_read.arn
}

# One user per subject. No aws_iam_access_key here: the key is minted out of band so its
# secret half never lands in terraform state (the state bucket is itself a denied path).
resource "aws_iam_user" "subject" {
  for_each = local.subjects

  name = "${var.env_name}-${each.value.serviceaccount}-aws"
  path = "/gardenio/"
  tags = merge(var.tags, {
    "gardenio:subject" = "${each.value.namespace}/${each.value.serviceaccount}"
  })
}

resource "aws_iam_user_group_membership" "subject" {
  for_each = local.subjects

  user   = aws_iam_user.subject[each.key].name
  groups = [aws_iam_group.this.name]
}
