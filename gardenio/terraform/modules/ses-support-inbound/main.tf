# ses-support-inbound — mail to support@<domain> reaches the operator's mailbox (Lands
# Track B §6.1, gd-UBeJ3kVicQ; item 3 of gd-o4fbw9qUJc, which the laptop deferred to
# terraform). People who cannot sign in reach the operator this way; everyone else uses
# their land's #support.
#
# WHAT IT CREATES, and why each piece:
#   - An MX for <domain> to the region's SES inbound endpoint. Only the one recipient
#     below is accepted; SES rejects mail to any other address on the domain.
#   - A receipt rule set, made the ACTIVE one, with one rule: recipient
#     support@<domain>, spam and virus scanning on, then (1) store the raw message in
#     a private bucket that expires it after retention_days and (2) invoke the
#     forwarder Lambda.
#   - The forwarder (scripts/support-forward/support_forward.py): it re-sends the
#     message to forward_to as "<sender> via support" <support@domain> with Reply-To
#     the sender, so the operator's reply reaches them; a message that fails the virus
#     scan, or that SES will not send, becomes a short notice naming its S3 key.
#   - The forwarder's role: read the stored mail, and ses:SendRawEmail as this one
#     From address on this one domain identity, plus the forward_to identity: while
#     the account is in the SES sandbox, SES also authorizes the RECIPIENT's verified
#     identity as a resource (10-25: AccessDenied on identity/<forward_to> with only
#     the domain listed). Harmless once production access is granted. Nothing else.
#   - An alarm on the forwarder's errors to alarm_topic_arn, so a support mail that
#     cannot be forwarded is not lost silently.
#
# WHAT IT DELIBERATELY DOES NOT DO:
#   - Create a support bead. Mail is from people who cannot sign in, so it may name a
#     land that is not theirs; the operator files the bead by hand
#     (gardenio/docs/lands/support-runbook.md, "Mail to support@").
#   - Use SNS for delivery. An SNS email carries the message as escaped JSON with no
#     way to reply; the Lambda keeps the message readable and answerable.

data "aws_region" "current" {}
data "aws_caller_identity" "current" {}
data "aws_partition" "current" {}

locals {
  region       = data.aws_region.current.name
  account_id   = data.aws_caller_identity.current.account_id
  partition    = data.aws_partition.current.partition
  name         = "gardenio-support-forward"
  recipient    = "${var.recipient_local_part}@${var.domain}"
  mail_prefix  = "${var.recipient_local_part}/"
  bucket_name  = "gardenio-support-mail-${local.account_id}"
  rule_name    = var.recipient_local_part
  rule_arn     = "arn:${local.partition}:ses:${local.region}:${local.account_id}:receipt-rule-set/${var.rule_set_name}:receipt-rule/${local.rule_name}"
  forward_from = local.recipient
  # The forward_to mailbox is a verified identity in this account (sandbox requirement);
  # SES checks it as a resource of SendRawEmail while the sandbox lasts.
  forward_to_identity_arn = "arn:${local.partition}:ses:${local.region}:${local.account_id}:identity/${var.forward_to}"

  # Built rather than read from the resources so both policies are known at plan time
  # and can be reviewed (and tested) before apply, as in ses-identity.
  bucket_arn    = "arn:${local.partition}:s3:::${local.bucket_name}"
  log_group_arn = "arn:${local.partition}:logs:${local.region}:${local.account_id}:log-group:/aws/lambda/${local.name}"
}

# ── the raw mail ──────────────────────────────────────────────────────────
resource "aws_s3_bucket" "mail" {
  bucket = local.bucket_name
  tags   = var.tags
}

resource "aws_s3_bucket_public_access_block" "mail" {
  bucket                  = aws_s3_bucket.mail.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_ownership_controls" "mail" {
  bucket = aws_s3_bucket.mail.id
  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "mail" {
  bucket = aws_s3_bucket.mail.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "mail" {
  bucket = aws_s3_bucket.mail.id
  rule {
    id     = "expire-received-mail"
    status = "Enabled"
    filter {}
    expiration {
      days = var.retention_days
    }
  }
}

locals {
  bucket_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid       = "SESStoresSupportMail"
        Effect    = "Allow"
        Principal = { Service = "ses.amazonaws.com" }
        Action    = "s3:PutObject"
        Resource  = "${local.bucket_arn}/${local.mail_prefix}*"
        Condition = {
          StringEquals = { "aws:SourceAccount" = local.account_id }
          ArnLike      = { "aws:SourceArn" = local.rule_arn }
        }
      },
      {
        Sid       = "TLSOnly"
        Effect    = "Deny"
        Principal = "*"
        Action    = "s3:*"
        Resource  = [local.bucket_arn, "${local.bucket_arn}/*"]
        Condition = { Bool = { "aws:SecureTransport" = "false" } }
      },
    ]
  })
}

resource "aws_s3_bucket_policy" "mail" {
  bucket = aws_s3_bucket.mail.id
  policy = local.bucket_policy

  depends_on = [aws_s3_bucket_public_access_block.mail]
}

# ── the forwarder ─────────────────────────────────────────────────────────
data "archive_file" "forward" {
  type        = "zip"
  source_dir  = "${path.module}/../../../../scripts/support-forward"
  output_path = "${path.module}/.build/support-forward.zip"
  excludes    = ["__pycache__", "support_forward_test.py"]
}

resource "aws_cloudwatch_log_group" "forward" {
  name              = "/aws/lambda/${local.name}"
  retention_in_days = var.log_retention_days
  tags              = var.tags
}

resource "aws_iam_role" "forward" {
  name = local.name
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "lambda.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
  tags = var.tags
}

locals {
  forward_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid      = "Logs"
        Effect   = "Allow"
        Action   = ["logs:CreateLogStream", "logs:PutLogEvents"]
        Resource = ["${local.log_group_arn}:log-stream:*"]
      },
      {
        Sid      = "ReadStoredMail"
        Effect   = "Allow"
        Action   = ["s3:GetObject"]
        Resource = ["${local.bucket_arn}/${local.mail_prefix}*"]
      },
      {
        Sid       = "SendAsSupportOnly"
        Effect    = "Allow"
        Action    = ["ses:SendRawEmail"]
        Resource  = [var.identity_arn, local.forward_to_identity_arn]
        Condition = { StringEquals = { "ses:FromAddress" = local.forward_from } }
      },
    ]
  })
}

resource "aws_iam_role_policy" "forward" {
  name   = local.name
  role   = aws_iam_role.forward.id
  policy = local.forward_policy
}

resource "aws_lambda_function" "forward" {
  function_name    = local.name
  role             = aws_iam_role.forward.arn
  runtime          = "python3.13"
  handler          = "support_forward.handler"
  timeout          = 30
  memory_size      = 256
  filename         = data.archive_file.forward.output_path
  source_code_hash = data.archive_file.forward.output_base64sha256

  environment {
    variables = {
      FORWARD_TO   = var.forward_to
      FORWARD_FROM = local.forward_from
      MAIL_BUCKET  = aws_s3_bucket.mail.bucket
      MAIL_PREFIX  = local.mail_prefix
    }
  }

  depends_on = [aws_cloudwatch_log_group.forward, aws_iam_role_policy.forward]
  tags       = var.tags
}

resource "aws_lambda_permission" "ses" {
  statement_id   = "AllowSESReceiptRule"
  action         = "lambda:InvokeFunction"
  function_name  = aws_lambda_function.forward.function_name
  principal      = "ses.amazonaws.com"
  source_account = local.account_id
  source_arn     = local.rule_arn
}

# ── receiving ─────────────────────────────────────────────────────────────
resource "aws_route53_record" "mx" {
  zone_id = var.zone_id
  name    = var.domain
  type    = "MX"
  ttl     = 1800
  records = ["10 inbound-smtp.${local.region}.amazonaws.com"]
}

resource "aws_ses_receipt_rule_set" "this" {
  rule_set_name = var.rule_set_name
}

resource "aws_ses_active_receipt_rule_set" "this" {
  rule_set_name = aws_ses_receipt_rule_set.this.rule_set_name
}

resource "aws_ses_receipt_rule" "support" {
  name          = local.rule_name
  rule_set_name = aws_ses_receipt_rule_set.this.rule_set_name
  recipients    = [local.recipient]
  enabled       = true
  scan_enabled  = true
  tls_policy    = "Optional"

  s3_action {
    bucket_name       = aws_s3_bucket.mail.bucket
    object_key_prefix = local.mail_prefix
    position          = 1
  }

  lambda_action {
    function_arn    = aws_lambda_function.forward.arn
    invocation_type = "Event"
    position        = 2
  }

  depends_on = [aws_s3_bucket_policy.mail, aws_lambda_permission.ses]
}

# ── alarm ─────────────────────────────────────────────────────────────────
resource "aws_cloudwatch_metric_alarm" "errors" {
  count = var.alarm_topic_arn == "" ? 0 : 1

  alarm_name          = "${local.name}-errors"
  alarm_description   = "A mail to ${local.recipient} could not be forwarded or noticed. Read the raw mail in s3://${local.bucket_name}/${local.mail_prefix} (support-runbook.md)."
  namespace           = "AWS/Lambda"
  metric_name         = "Errors"
  dimensions          = { FunctionName = aws_lambda_function.forward.function_name }
  statistic           = "Sum"
  period              = 300
  evaluation_periods  = 1
  threshold           = 1
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [var.alarm_topic_arn]
  tags                = var.tags
}
