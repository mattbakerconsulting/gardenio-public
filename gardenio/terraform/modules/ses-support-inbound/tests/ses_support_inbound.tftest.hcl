# Native terraform tests for the support@ forward (gd-UBeJ3kVicQ).
#
# Properties locked down:
#   - the domain's MX points at the region's SES inbound endpoint;
#   - the one receipt rule takes only support@<domain>, scans for spam and viruses, and
#     stores the message BEFORE it invokes the forwarder;
#   - SES may write the bucket only for that rule in this account, and the bucket is
#     private and TLS-only with an expiry;
#   - the forwarder may read only the stored mail and send only as support@<domain> on
#     the one identity: exactly the documented actions, no wildcard action;
#   - the error alarm exists only when a topic is given.
#
# Run: terraform test   (from modules/ses-support-inbound/)

mock_provider "aws" {
  override_data {
    target = data.aws_caller_identity.current
    values = { account_id = "123456789012" }
  }
  override_data {
    target = data.aws_region.current
    values = { name = "us-east-1" }
  }
  override_data {
    target = data.aws_partition.current
    values = { partition = "aws" }
  }
}
mock_provider "archive" {}

variables {
  domain          = "lands.clonk.tv"
  zone_id         = "Z0000000000000000000"
  identity_arn    = "arn:aws:ses:us-east-1:123456789012:identity/lands.clonk.tv"
  forward_to      = "owner@example.invalid"
  alarm_topic_arn = "arn:aws:sns:us-east-1:123456789012:gardenio-edge-probe"
}

run "mx_and_rule" {
  command = plan

  assert {
    condition     = aws_route53_record.mx.name == "lands.clonk.tv" && aws_route53_record.mx.type == "MX" && length(aws_route53_record.mx.records) == 1 && contains(aws_route53_record.mx.records, "10 inbound-smtp.us-east-1.amazonaws.com")
    error_message = "lands.clonk.tv needs one MX to inbound-smtp.us-east-1.amazonaws.com"
  }

  assert {
    condition     = length(aws_ses_receipt_rule.support.recipients) == 1 && contains(aws_ses_receipt_rule.support.recipients, "support@lands.clonk.tv")
    error_message = "the rule must take support@lands.clonk.tv and nothing else"
  }

  assert {
    condition     = aws_ses_receipt_rule.support.scan_enabled && aws_ses_receipt_rule.support.enabled
    error_message = "the rule must be enabled with spam and virus scanning"
  }

  assert {
    condition     = one(aws_ses_receipt_rule.support.s3_action).position == 1 && one(aws_ses_receipt_rule.support.lambda_action).position == 2
    error_message = "the message must be stored before the forwarder runs (it reads it from S3)"
  }

  assert {
    condition     = one(aws_ses_receipt_rule.support.s3_action).object_key_prefix == "support/" && aws_lambda_function.forward.environment[0].variables["MAIL_PREFIX"] == "support/"
    error_message = "the rule and the forwarder must agree on the key prefix"
  }

  assert {
    condition     = aws_ses_active_receipt_rule_set.this.rule_set_name == "gardenio-inbound"
    error_message = "the rule set must be the active one"
  }
}

run "bucket_is_private_and_written_only_by_this_rule" {
  command = plan

  assert {
    condition     = aws_s3_bucket_public_access_block.mail.block_public_acls && aws_s3_bucket_public_access_block.mail.block_public_policy && aws_s3_bucket_public_access_block.mail.restrict_public_buckets && aws_s3_bucket_public_access_block.mail.ignore_public_acls
    error_message = "the mail bucket must block all public access"
  }

  assert {
    condition     = one(one(aws_s3_bucket_lifecycle_configuration.mail.rule).expiration).days == 30
    error_message = "received mail must expire after 30 days by default"
  }

  assert {
    condition     = jsondecode(aws_s3_bucket_policy.mail.policy).Statement[0].Condition == { StringEquals = { "aws:SourceAccount" = "123456789012" }, ArnLike = { "aws:SourceArn" = "arn:aws:ses:us-east-1:123456789012:receipt-rule-set/gardenio-inbound:receipt-rule/support" } }
    error_message = "SES may store mail only for this account's support rule"
  }

  assert {
    condition     = jsondecode(aws_s3_bucket_policy.mail.policy).Statement[0].Action == "s3:PutObject" && jsondecode(aws_s3_bucket_policy.mail.policy).Statement[0].Resource == "arn:aws:s3:::gardenio-support-mail-123456789012/support/*"
    error_message = "SES may only put objects under support/"
  }

  assert {
    condition     = jsondecode(aws_s3_bucket_policy.mail.policy).Statement[1].Effect == "Deny" && jsondecode(aws_s3_bucket_policy.mail.policy).Statement[1].Condition == { Bool = { "aws:SecureTransport" = "false" } }
    error_message = "the bucket must refuse non-TLS access"
  }
}

run "forwarder_role_is_narrow" {
  command = plan

  assert {
    condition     = toset(flatten([for s in jsondecode(aws_iam_role_policy.forward.policy).Statement : s.Action])) == toset(["logs:CreateLogStream", "logs:PutLogEvents", "s3:GetObject", "ses:SendRawEmail"])
    error_message = "the forwarder role grants exactly logs, s3:GetObject and ses:SendRawEmail"
  }

  assert {
    condition     = alltrue([for s in jsondecode(aws_iam_role_policy.forward.policy).Statement : alltrue([for a in s.Action : !strcontains(a, "*")])])
    error_message = "no wildcard action"
  }

  assert {
    condition     = [for s in jsondecode(aws_iam_role_policy.forward.policy).Statement : s if s.Sid == "SendAsSupportOnly"][0].Resource == ["arn:aws:ses:us-east-1:123456789012:identity/lands.clonk.tv", "arn:aws:ses:us-east-1:123456789012:identity/owner@example.invalid"]
    error_message = "the forwarder may send on the lands.clonk.tv identity and the forward_to identity (sandbox) only"
  }

  assert {
    condition     = [for s in jsondecode(aws_iam_role_policy.forward.policy).Statement : s if s.Sid == "SendAsSupportOnly"][0].Condition == { StringEquals = { "ses:FromAddress" = "support@lands.clonk.tv" } }
    error_message = "the forwarder may send as support@lands.clonk.tv only"
  }

  assert {
    condition     = [for s in jsondecode(aws_iam_role_policy.forward.policy).Statement : s if s.Sid == "ReadStoredMail"][0].Resource == ["arn:aws:s3:::gardenio-support-mail-123456789012/support/*"]
    error_message = "the forwarder may read the stored support mail only"
  }

  assert {
    condition     = aws_lambda_permission.ses.principal == "ses.amazonaws.com" && aws_lambda_permission.ses.source_account == "123456789012" && aws_lambda_permission.ses.source_arn == "arn:aws:ses:us-east-1:123456789012:receipt-rule-set/gardenio-inbound:receipt-rule/support"
    error_message = "only this account's support rule may invoke the forwarder"
  }

  assert {
    condition     = aws_lambda_function.forward.environment[0].variables["FORWARD_FROM"] == "support@lands.clonk.tv" && aws_lambda_function.forward.environment[0].variables["FORWARD_TO"] == "owner@example.invalid"
    error_message = "the forwarder sends from support@ to forward_to"
  }

  assert {
    condition     = length(aws_lambda_function.forward.vpc_config) == 0 && aws_lambda_function.forward.handler == "support_forward.handler"
    error_message = "handler drifted, or the forwarder gained a VPC"
  }
}

run "alarm_on_errors" {
  command = plan

  assert {
    condition     = length(aws_cloudwatch_metric_alarm.errors) == 1 && aws_cloudwatch_metric_alarm.errors[0].metric_name == "Errors" && length(aws_cloudwatch_metric_alarm.errors[0].alarm_actions) == 1 && contains(aws_cloudwatch_metric_alarm.errors[0].alarm_actions, "arn:aws:sns:us-east-1:123456789012:gardenio-edge-probe")
    error_message = "forwarder errors must alarm to the given topic"
  }
}

run "no_topic_no_alarm" {
  command = plan

  variables {
    alarm_topic_arn = ""
  }

  assert {
    condition     = length(aws_cloudwatch_metric_alarm.errors) == 0
    error_message = "no alarm without a topic"
  }
}

run "bad_forward_to_is_rejected" {
  command = plan

  variables {
    forward_to = "not an address"
  }

  expect_failures = [var.forward_to]
}
