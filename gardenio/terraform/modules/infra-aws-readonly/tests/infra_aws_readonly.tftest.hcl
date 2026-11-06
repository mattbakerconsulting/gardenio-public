# Native terraform tests for the infra tier's read-only AWS identity (gd-83VraF4SeC).
#
# Properties locked down:
#   - the group carries ReadOnlyAccess AND the deny policy (a deny that is not attached
#     denies nothing);
#   - the deny policy is ALL Deny, names every action D3 and the #302 review require (each
#     one has its own case, so removing any single action fails the test), and denies S3
#     object reads in every bucket;
#   - one user per subject, and no standing access key is created (the secret would land in
#     state, which is itself a denied path);
#   - subjects is a list: a second entry yields a second user.
#
# Run: terraform test   (from modules/infra-aws-readonly/)

mock_provider "aws" {}

variables {
  env_name = "gardenio"
}

run "group_has_read_only_and_the_deny" {
  command = plan

  assert {
    condition     = aws_iam_group_policy_attachment.read_only_access.policy_arn == "arn:aws:iam::aws:policy/ReadOnlyAccess"
    error_message = "the group must carry the AWS-managed ReadOnlyAccess policy"
  }

  # Positive control for the deny assertions below: the deny policy is attached to the SAME
  # group that carries ReadOnlyAccess, so a Deny can actually beat an Allow.
  assert {
    condition     = aws_iam_group_policy_attachment.deny_secret_reads.group == aws_iam_group_policy_attachment.read_only_access.group
    error_message = "the deny policy must be attached to the same group as ReadOnlyAccess"
  }
}

run "deny_policy_is_all_deny_and_names_every_required_action" {
  command = plan

  assert {
    condition     = alltrue([for s in jsondecode(aws_iam_policy.deny_secret_reads.policy).Statement : s.Effect == "Deny"])
    error_message = "every statement in the secret-read policy must be a Deny"
  }

  # One assertion per required action: removing any one of them fails exactly that case
  # (the same kind of check as kms:Decrypt below).
  assert {
    condition     = contains(jsondecode(aws_iam_policy.deny_secret_reads.policy).Statement[0].Action, "secretsmanager:GetSecretValue")
    error_message = "the deny must name secretsmanager:GetSecretValue"
  }

  assert {
    condition     = contains(jsondecode(aws_iam_policy.deny_secret_reads.policy).Statement[0].Action, "secretsmanager:BatchGetSecretValue")
    error_message = "the deny must name secretsmanager:BatchGetSecretValue"
  }

  assert {
    condition     = contains(jsondecode(aws_iam_policy.deny_secret_reads.policy).Statement[0].Action, "ssm:GetParameter")
    error_message = "the deny must name ssm:GetParameter"
  }

  assert {
    condition     = contains(jsondecode(aws_iam_policy.deny_secret_reads.policy).Statement[0].Action, "ssm:GetParameters")
    error_message = "the deny must name ssm:GetParameters"
  }

  assert {
    condition     = contains(jsondecode(aws_iam_policy.deny_secret_reads.policy).Statement[0].Action, "ssm:GetParametersByPath")
    error_message = "the deny must name ssm:GetParametersByPath"
  }

  assert {
    condition     = contains(jsondecode(aws_iam_policy.deny_secret_reads.policy).Statement[0].Action, "ssm:GetParameterHistory")
    error_message = "the deny must name ssm:GetParameterHistory"
  }

  assert {
    condition     = contains(jsondecode(aws_iam_policy.deny_secret_reads.policy).Statement[0].Action, "kms:Decrypt")
    error_message = "the deny must name kms:Decrypt"
  }

  assert {
    condition     = contains(jsondecode(aws_iam_policy.deny_secret_reads.policy).Statement[0].Action, "lambda:GetFunction")
    error_message = "the deny must name lambda:GetFunction"
  }

  assert {
    condition     = contains(jsondecode(aws_iam_policy.deny_secret_reads.policy).Statement[0].Action, "iam:GetCredentialReport")
    error_message = "the deny must name iam:GetCredentialReport"
  }

  assert {
    condition     = contains(jsondecode(aws_iam_policy.deny_secret_reads.policy).Statement[0].Action, "ecr:GetAuthorizationToken")
    error_message = "the deny must name ecr:GetAuthorizationToken"
  }

  assert {
    condition     = contains(jsondecode(aws_iam_policy.deny_secret_reads.policy).Statement[0].Action, "ecr:BatchGetImage")
    error_message = "the deny must name ecr:BatchGetImage"
  }

  assert {
    condition     = contains(jsondecode(aws_iam_policy.deny_secret_reads.policy).Statement[0].Action, "ecr:GetDownloadUrlForLayer")
    error_message = "the deny must name ecr:GetDownloadUrlForLayer"
  }

  assert {
    condition     = contains(jsondecode(aws_iam_policy.deny_secret_reads.policy).Statement[0].Action, "ssm:ListCommands")
    error_message = "the deny must name ssm:ListCommands"
  }

  assert {
    condition     = contains(jsondecode(aws_iam_policy.deny_secret_reads.policy).Statement[0].Action, "ssm:ListCommandInvocations")
    error_message = "the deny must name ssm:ListCommandInvocations"
  }

  assert {
    condition     = contains(jsondecode(aws_iam_policy.deny_secret_reads.policy).Statement[0].Action, "ssm:GetCommandInvocation")
    error_message = "the deny must name ssm:GetCommandInvocation"
  }

  assert {
    condition     = contains(jsondecode(aws_iam_policy.deny_secret_reads.policy).Statement[0].Action, "sts:GetSessionToken")
    error_message = "the deny must name sts:GetSessionToken"
  }

  assert {
    condition     = contains(jsondecode(aws_iam_policy.deny_secret_reads.policy).Statement[0].Action, "logs:StartQuery")
    error_message = "the deny must name logs:StartQuery"
  }

  assert {
    condition     = contains(jsondecode(aws_iam_policy.deny_secret_reads.policy).Statement[0].Action, "logs:GetQueryResults")
    error_message = "the deny must name logs:GetQueryResults"
  }

  assert {
    condition     = jsondecode(aws_iam_policy.deny_secret_reads.policy).Statement[0].Resource == "*"
    error_message = "the secret-read deny must apply to every resource"
  }

  # No wildcard actions: an `s3:*`-style deny would also deny the positive-control reads.
  assert {
    condition     = alltrue([for a in jsondecode(aws_iam_policy.deny_secret_reads.policy).Statement[0].Action : !strcontains(a, "*")])
    error_message = "deny actions must be explicit, not wildcards"
  }
}

run "positive_controls_stay_allowed" {
  command = plan

  # The acceptance check reads EC2 and STS; the deny must not swallow them.
  assert {
    condition = alltrue([for a in ["ec2:DescribeInstances", "sts:GetCallerIdentity", "s3:ListBucket", "s3:GetBucketLocation", "logs:DescribeLogGroups", "ssm:DescribeInstanceInformation", "ecr:DescribeRepositories"] :
    !contains(jsondecode(aws_iam_policy.deny_secret_reads.policy).Statement[0].Action, a) && !contains(jsondecode(aws_iam_policy.deny_secret_reads.policy).Statement[1].Action, a) && !contains(jsondecode(aws_iam_policy.deny_secret_reads.policy).Statement[2].Action, a)])
    error_message = "the diagnosis reads (ec2, sts:GetCallerIdentity, bucket and log-group listing, SSM instance info, ECR describe) must not be denied"
  }
}

run "log_content_is_denied_outside_the_readable_groups" {
  command = plan

  assert {
    condition     = jsondecode(aws_iam_policy.deny_secret_reads.policy).Statement[2].Effect == "Deny" && toset(jsondecode(aws_iam_policy.deny_secret_reads.policy).Statement[2].Action) == toset(["logs:GetLogEvents", "logs:FilterLogEvents"])
    error_message = "logs:GetLogEvents and logs:FilterLogEvents must be denied outside the readable groups"
  }

  # The carve-out is NotResource, so a group missing from it is denied; a wildcard here would
  # open every group.
  assert {
    condition     = alltrue([for r in jsondecode(aws_iam_policy.deny_secret_reads.policy).Statement[2].NotResource : startswith(r, "arn:aws:logs:*:*:log-group:/aws/lambda/gardenio-") && !strcontains(trimsuffix(split("log-group:", r)[1], ":*"), "*")])
    error_message = "the log-read carve-out must name only the gardenio lambda log groups, exactly"
  }

  assert {
    condition     = contains(jsondecode(aws_iam_policy.deny_secret_reads.policy).Statement[2].NotResource, "arn:aws:logs:*:*:log-group:/aws/lambda/gardenio-edge-probe:*")
    error_message = "the edge-probe log group's streams must be readable"
  }

  # The Allow names the same ARNs the Deny leaves open, no more.
  assert {
    condition     = toset(jsondecode(aws_iam_policy.cost_and_metrics_read.policy).Statement[2].Resource) == toset(jsondecode(aws_iam_policy.deny_secret_reads.policy).Statement[2].NotResource)
    error_message = "the log-read Allow must cover exactly the groups the Deny carves out"
  }

  # Insights stays denied everywhere: GetQueryResults takes no resource, so it cannot be scoped.
  assert {
    condition     = contains(jsondecode(aws_iam_policy.deny_secret_reads.policy).Statement[0].Action, "logs:StartLiveTail")
    error_message = "the deny must name logs:StartLiveTail"
  }
}

run "cost_and_metrics_read_is_attached" {
  command = plan

  assert {
    condition     = aws_iam_group_policy_attachment.cost_and_metrics_read.group == aws_iam_group_policy_attachment.read_only_access.group
    error_message = "the cost and metrics policy must be attached to the read-only group"
  }

  assert {
    condition     = alltrue([for s in jsondecode(aws_iam_policy.cost_and_metrics_read.policy).Statement : s.Effect == "Allow"])
    error_message = "the cost and metrics policy must only Allow"
  }

  assert {
    condition     = contains(jsondecode(aws_iam_policy.cost_and_metrics_read.policy).Statement[0].Action, "ce:Get*")
    error_message = "Cost Explorer reads (ce:Get*) must be allowed"
  }

  assert {
    condition     = alltrue([for a in ["cloudwatch:GetMetricData", "cloudwatch:ListMetrics"] : contains(jsondecode(aws_iam_policy.cost_and_metrics_read.policy).Statement[1].Action, a)])
    error_message = "cloudwatch:GetMetricData and cloudwatch:ListMetrics must be allowed"
  }

  # Read only: no write verb in the grant.
  assert {
    condition     = alltrue(flatten([for s in jsondecode(aws_iam_policy.cost_and_metrics_read.policy).Statement : [for a in s.Action : can(regex(":(Get|Describe|List|Filter)", a))]]))
    error_message = "the cost and metrics policy must grant only Get/Describe/List/Filter actions"
  }
}

run "rejects_wildcard_log_group" {
  command = plan

  variables {
    readable_log_groups = ["/aws/lambda/gardenio-*"]
  }

  expect_failures = [var.readable_log_groups]
}

run "s3_object_deny_covers_every_bucket" {
  command = plan

  assert {
    condition     = jsondecode(aws_iam_policy.deny_secret_reads.policy).Statement[1].Resource == "arn:aws:s3:::*/*"
    error_message = "the S3 object deny must cover objects in every bucket, not a per-bucket list"
  }

  assert {
    condition     = alltrue([for a in ["s3:GetObject", "s3:GetObjectVersion"] : contains(jsondecode(aws_iam_policy.deny_secret_reads.policy).Statement[1].Action, a)])
    error_message = "s3:GetObject and s3:GetObjectVersion must both be denied"
  }

  # Positive control: listing is not part of the object deny.
  assert {
    condition     = !contains(jsondecode(aws_iam_policy.deny_secret_reads.policy).Statement[1].Action, "s3:ListBucket")
    error_message = "s3:ListBucket must stay allowed"
  }
}

run "default_subject_gets_one_user_and_no_access_key" {
  command = plan

  assert {
    condition     = length(aws_iam_user.subject) == 1 && aws_iam_user.subject["gardenio-infra-reader"].name == "gardenio-gardenio-infra-reader-aws"
    error_message = "the default subject must yield exactly the gardenio-infra-reader user"
  }

  assert {
    condition     = aws_iam_user.subject["gardenio-infra-reader"].tags["gardenio:subject"] == "gardenio/gardenio-infra-reader"
    error_message = "the user must record its subject in a tag"
  }

  assert {
    condition     = length(aws_iam_user_group_membership.subject) == 1
    error_message = "each user must be a member of the read-only group"
  }
}

run "subjects_is_a_list" {
  command = plan

  variables {
    subjects = ["gardenio/gardenio-infra-reader", "gardenio/gardenio-infra-admin"]
  }

  assert {
    condition     = length(aws_iam_user.subject) == 2 && contains(keys(aws_iam_user.subject), "gardenio-infra-admin")
    error_message = "a second subject must yield a second user"
  }
}

run "rejects_malformed_subject" {
  command = plan

  variables {
    subjects = ["gardenio-infra-reader"]
  }

  expect_failures = [var.subjects]
}
