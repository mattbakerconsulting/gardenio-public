# Availability prober and off-cluster status page (Lands Track B §5.8,
# gd-rqtbyqCMZJ).
#
# A second Lambda, also OUTSIDE any VPC, runs scripts/uptime-probe/uptime_probe.py
# every minute against /lands/, /garden and the Commons API (the targets in
# scripts/uptime-probe/targets.json). Each run rewrites the status page in an
# S3 bucket that nothing in the cluster serves, so the page is up when gardenio
# is down. The same bucket carries a Prometheus `metrics` file the in-cluster
# vmagent scrapes for the Lands SLO burn-rate rules; these alarms are the
# off-cluster backstop for when the cluster cannot alert at all.

locals {
  uptime_name    = "gardenio-uptime-probe"
  uptime_targets = [for t in jsondecode(file("${path.module}/../../../../scripts/uptime-probe/targets.json")).targets : t.name]
  # The only objects anyone may read without credentials. state.json stays private.
  uptime_public_keys = ["index.html", "status.json", "metrics"]
}

data "archive_file" "uptime_probe" {
  type        = "zip"
  source_dir  = "${path.module}/../../../../scripts/uptime-probe"
  output_path = "${path.module}/.build/uptime-probe.zip"
  excludes    = ["__pycache__", "uptime_probe_test.py", "README.md"]
}

# ── the status bucket ─────────────────────────────────────────────────
resource "aws_s3_bucket" "status" {
  bucket = "gardenio-status-${data.aws_caller_identity.current.account_id}"
  tags   = var.tags
}

resource "aws_s3_bucket_ownership_controls" "status" {
  bucket = aws_s3_bucket.status.id
  rule {
    object_ownership = "BucketOwnerEnforced" # no ACLs; the bucket policy is the only grant
  }
}

# Public by bucket policy only, and only for the three public keys. ACLs stay
# blocked. Needs the ACCOUNT-level block_public_policy off as well, or the
# policy below is refused at apply.
resource "aws_s3_bucket_public_access_block" "status" {
  bucket                  = aws_s3_bucket.status.id
  block_public_acls       = true
  ignore_public_acls      = true
  block_public_policy     = false
  restrict_public_buckets = false
}

resource "aws_s3_bucket_server_side_encryption_configuration" "status" {
  bucket = aws_s3_bucket.status.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

locals {
  status_bucket_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "PublicStatusPage"
      Effect    = "Allow"
      Principal = "*"
      Action    = "s3:GetObject"
      Resource  = [for k in local.uptime_public_keys : "${aws_s3_bucket.status.arn}/${k}"]
    }]
  })
}

resource "aws_s3_bucket_policy" "status" {
  bucket     = aws_s3_bucket.status.id
  policy     = local.status_bucket_policy
  depends_on = [aws_s3_bucket_public_access_block.status]
}

# ── role: least privilege ─────────────────────────────────────────────
resource "aws_cloudwatch_log_group" "uptime_probe" {
  name              = "/aws/lambda/${local.uptime_name}"
  retention_in_days = var.log_retention_days
  tags              = var.tags
}

resource "aws_iam_role" "uptime_probe" {
  name = local.uptime_name
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Action    = "sts:AssumeRole"
      Principal = { Service = "lambda.amazonaws.com" }
    }]
  })
  tags = var.tags
}

locals {
  uptime_policy_statements = [
    {
      Sid      = "OwnLogs"
      Effect   = "Allow"
      Action   = ["logs:CreateLogStream", "logs:PutLogEvents"]
      Resource = ["${aws_cloudwatch_log_group.uptime_probe.arn}:*"]
    },
    {
      Sid       = "Metrics"
      Effect    = "Allow"
      Action    = ["cloudwatch:PutMetricData"]
      Resource  = ["*"]
      Condition = { StringEquals = { "cloudwatch:namespace" = "GardenioUptime" } }
    },
    {
      Sid      = "StatusObjects"
      Effect   = "Allow"
      Action   = ["s3:GetObject", "s3:PutObject"]
      Resource = ["${aws_s3_bucket.status.arn}/*"]
    },
    {
      # Without ListBucket a missing state.json reads as 403, not NoSuchKey,
      # and the first run fails instead of starting fresh.
      Sid      = "StatusBucket"
      Effect   = "Allow"
      Action   = ["s3:ListBucket"]
      Resource = [aws_s3_bucket.status.arn]
    },
  ]
}

resource "aws_iam_role_policy" "uptime_probe" {
  name = local.uptime_name
  role = aws_iam_role.uptime_probe.id
  policy = jsonencode({
    Version   = "2012-10-17"
    Statement = local.uptime_policy_statements
  })
}

# ── function: deliberately NO vpc_config (see the edge prober) ────────
resource "aws_lambda_function" "uptime_probe" {
  function_name    = local.uptime_name
  role             = aws_iam_role.uptime_probe.arn
  runtime          = "python3.13"
  handler          = "uptime_probe.handler"
  timeout          = 50 # three 10 s probes and four small writes, inside the minute
  memory_size      = 128
  filename         = data.archive_file.uptime_probe.output_path
  source_code_hash = data.archive_file.uptime_probe.output_base64sha256
  # One run at a time (each run reads and rewrites state.json) comes from the 50 s timeout inside the 1-minute
  # schedule plus no async retries (aws_lambda_function_event_invoke_config below), NOT reserved concurrency: the
  # account's concurrency limit is 10, the unreserved minimum, so any reservation is refused at apply (2026-10-21).

  environment {
    variables = {
      UPTIME_PROBE_BUCKET = aws_s3_bucket.status.id
    }
  }

  depends_on = [aws_cloudwatch_log_group.uptime_probe, aws_iam_role_policy.uptime_probe]
  tags       = var.tags
}

# A failed run is not retried: a retry could overlap the next minute's run, and the next tick probes again anyway.
resource "aws_lambda_function_event_invoke_config" "uptime_probe" {
  function_name                = aws_lambda_function.uptime_probe.function_name
  maximum_retry_attempts       = 0
  maximum_event_age_in_seconds = 60
}

resource "aws_cloudwatch_event_rule" "uptime_probe" {
  name                = local.uptime_name
  description         = "Run the availability prober"
  schedule_expression = var.uptime_schedule_expression
  tags                = var.tags
}

resource "aws_cloudwatch_event_target" "uptime_probe" {
  rule = aws_cloudwatch_event_rule.uptime_probe.name
  arn  = aws_lambda_function.uptime_probe.arn
}

resource "aws_lambda_permission" "uptime_events" {
  statement_id  = "AllowEventBridge"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.uptime_probe.function_name
  principal     = "events.amazonaws.com"
  source_arn    = aws_cloudwatch_event_rule.uptime_probe.arn
}

# ── alarms: the backstop for when the cluster cannot alert ────────────
# One per target: down for 3 of the last 5 minutes. OK actions too, so the
# email thread says when it came back.
resource "aws_cloudwatch_metric_alarm" "uptime_down" {
  for_each            = toset(local.uptime_targets)
  alarm_name          = "${local.uptime_name}-${each.key}-down"
  alarm_description   = "${each.key} failed its availability probe in 3 of the last 5 minutes. Status page: https://${aws_s3_bucket.status.bucket_regional_domain_name}/index.html"
  namespace           = "GardenioUptime"
  metric_name         = "Success"
  dimensions          = { Target = each.key }
  statistic           = "Average"
  period              = 60
  evaluation_periods  = 5
  datapoints_to_alarm = 3
  threshold           = 1
  comparison_operator = "LessThanThreshold"
  treat_missing_data  = "notBreaching" # a silent prober is the heartbeat alarm's job
  alarm_actions       = [aws_sns_topic.edge_probe.arn]
  ok_actions          = [aws_sns_topic.edge_probe.arn]
  tags                = var.tags
}

# Dead-man: no run for 15 minutes means the status page is frozen.
resource "aws_cloudwatch_metric_alarm" "uptime_heartbeat" {
  alarm_name          = "${local.uptime_name}-heartbeat-missing"
  alarm_description   = "No availability-probe heartbeat for 15 minutes: the prober is dead or disabled, and the status page is stale."
  namespace           = "GardenioUptime"
  metric_name         = "Heartbeat"
  statistic           = "Sum"
  period              = 300
  evaluation_periods  = 3
  datapoints_to_alarm = 3
  threshold           = 1
  comparison_operator = "LessThanThreshold"
  treat_missing_data  = "breaching"
  alarm_actions       = [aws_sns_topic.edge_probe.arn]
  ok_actions          = [aws_sns_topic.edge_probe.arn]
  tags                = var.tags
}

resource "aws_cloudwatch_metric_alarm" "uptime_lambda_errors" {
  alarm_name          = "${local.uptime_name}-lambda-errors"
  alarm_description   = "The availability prober raised errors in 3 or more runs in 15 minutes (S3 write, CloudWatch publish, crash)."
  namespace           = "AWS/Lambda"
  metric_name         = "Errors"
  dimensions          = { FunctionName = aws_lambda_function.uptime_probe.function_name }
  statistic           = "Sum"
  period              = 900
  evaluation_periods  = 1
  datapoints_to_alarm = 1
  threshold           = 3
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.edge_probe.arn]
  tags                = var.tags
}
