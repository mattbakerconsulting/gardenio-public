# modules/finops/audit_role.tf — the read-only role a FinOps agent assumes to
# ENUMERATE account resources (escalation ka-0000006541, sweep ka-0000006722).
#
# Why a role rather than a policy on the caller: the FinOps identity is expected
# to reach `arn:aws:iam::*:role/FinOpsAudit` in EVERY account it audits — that
# org-wide convention is already hard-coded in the finops bot user's own inline
# policy (`assume-finops-audit-only`, Resource `arn:aws:iam::*:role/FinOpsAudit`)
# and is how the payer account is audited today. A member account that wants a
# resource-level sweep therefore needs its OWN same-named role; this creates it.
# Attaching describe permissions directly to the caller instead would work once
# and leave that inline policy pointing at a role that exists nowhere — the
# exact confusion ka-0000006541 was filed over (an AssumeRole against a missing
# role returns AccessDenied, which was read as a permissions problem).
#
# Scope: Describe/List only — no mutations, and an explicit deny on data-plane
# reads so the role stays an inventory role even if the allow list is later
# widened. The consumer is the idle/orphaned-resource sweep: unattached EBS
# volumes, unassociated EIPs, idle ELBs/NAT, stale snapshots, and node groups
# whose capacity the fleet is not using. Cost Explorer is deliberately NOT
# granted here — aggregate spend is already reachable (payer FinOpsAudit, plus
# the in-account gardenio-prod-agent-cost-explorer-ro policy); what was missing
# is only resource enumeration.

locals {
  create_audit_role = var.audit_role_enabled
}

data "aws_iam_policy_document" "audit_assume_role" {
  count = local.create_audit_role ? 1 : 0

  statement {
    sid     = "TrustFinOpsPrincipals"
    effect  = "Allow"
    actions = ["sts:AssumeRole"]

    principals {
      type        = "AWS"
      identifiers = var.audit_role_trusted_principal_arns
    }
  }
}

resource "aws_iam_role" "audit" {
  count = local.create_audit_role ? 1 : 0

  name               = var.audit_role_name
  description        = "Read-only resource enumeration for FinOps cost sweeps (ka-0000006541). Describe/List only; no mutations."
  assume_role_policy = data.aws_iam_policy_document.audit_assume_role[0].json
  tags               = local.module_tags

  lifecycle {
    precondition {
      condition     = length(var.audit_role_trusted_principal_arns) > 0
      error_message = "audit_role_enabled = true requires at least one entry in audit_role_trusted_principal_arns — a role nobody can assume is dead weight."
    }
  }
}

data "aws_iam_policy_document" "audit_read" {
  count = local.create_audit_role ? 1 : 0

  # Inventory of the resources that actually drive the bill. Describe/List APIs
  # do not support resource-level ARNs, hence Resource = "*".
  statement {
    sid    = "ComputeStorageNetworkInventory"
    effect = "Allow"
    actions = [
      # EC2 covers instances, EBS volumes/snapshots, EIPs, NAT gateways, VPC
      # endpoints, and every other orphan class in the sweep.
      "ec2:Describe*",
      "elasticloadbalancing:Describe*",
      "autoscaling:Describe*",
      "eks:List*",
      "eks:Describe*",
      "rds:Describe*",
      "rds:ListTagsForResource",
    ]
    resources = ["*"]
  }

  # Utilization evidence: an idle-resource claim needs the metric behind it, so
  # a teardown recommendation can carry "0 bytes/0 connections for N days".
  statement {
    sid    = "MetricsRead"
    effect = "Allow"
    actions = [
      "cloudwatch:GetMetricData",
      "cloudwatch:GetMetricStatistics",
      "cloudwatch:ListMetrics",
    ]
    resources = ["*"]
  }

  # Resource Groups Tagging API — the cheapest way to enumerate everything
  # carrying (or MISSING) the cost-allocation tag the budgets are scoped on.
  statement {
    sid    = "TagInventory"
    effect = "Allow"
    actions = [
      "tag:GetResources",
      "tag:GetTagKeys",
      "tag:GetTagValues",
    ]
    resources = ["*"]
  }

  # Belt-and-braces: this role never needs to read data or secret material, and
  # an explicit deny survives any later widening of the allow list above.
  # ec2:DescribeInstanceAttribute is denied because it exposes instance user
  # data; nothing in a cost sweep needs it.
  statement {
    sid    = "DenySensitiveReads"
    effect = "Deny"
    actions = [
      "secretsmanager:GetSecretValue",
      "ssm:GetParameter",
      "ssm:GetParameters",
      "ssm:GetParametersByPath",
      "kms:Decrypt",
      "s3:GetObject",
      "dynamodb:GetItem",
      "dynamodb:Query",
      "dynamodb:Scan",
      "logs:GetLogEvents",
      "logs:FilterLogEvents",
      "ec2:DescribeInstanceAttribute",
      "ec2:GetPasswordData",
    ]
    resources = ["*"]
  }
}

resource "aws_iam_role_policy" "audit_read" {
  count = local.create_audit_role ? 1 : 0

  name   = "${var.audit_role_name}-read-only"
  role   = aws_iam_role.audit[0].id
  policy = data.aws_iam_policy_document.audit_read[0].json
}
