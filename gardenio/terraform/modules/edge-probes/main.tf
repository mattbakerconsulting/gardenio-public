# Standing edge prober (gd-xuxyIMbfCU, plan gd-P78ygCUgA9).
#
# A Lambda OUTSIDE any VPC runs scripts/edge-probe/edge_probe.py every 15
# minutes over the public internet: the same code the GitHub workflow runs,
# plus Route53/EC2 discovery and the security-group audit (E7). It depends on
# AWS APIs, not on the edge it checks, and alerts by SNS email, not Slack.
# A dead prober is caught by the heartbeat alarm (missing data = breaching).

locals {
  name = "gardenio-edge-probe"
}

data "aws_caller_identity" "current" {}

data "archive_file" "edge_probe" {
  type        = "zip"
  source_dir  = "${path.module}/../../../../scripts/edge-probe"
  output_path = "${path.module}/.build/edge-probe.zip"
  excludes    = ["__pycache__", "edge_probe_test.py", "README.md"]
}

resource "aws_cloudwatch_log_group" "edge_probe" {
  name              = "/aws/lambda/${local.name}"
  retention_in_days = var.log_retention_days
  tags              = var.tags
}

# ── alerts ────────────────────────────────────────────────────────────
resource "aws_sns_topic" "edge_probe" {
  name = local.name
  tags = var.tags
}

# The owner confirms the subscription from the email AWS sends.
resource "aws_sns_topic_subscription" "email" {
  topic_arn = aws_sns_topic.edge_probe.arn
  protocol  = "email"
  endpoint  = var.alert_email
}

# CloudWatch alarms publish with the cloudwatch.amazonaws.com principal.
locals {
  topic_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "AlarmsPublish"
      Effect    = "Allow"
      Action    = "sns:Publish"
      Resource  = aws_sns_topic.edge_probe.arn
      Principal = { Service = "cloudwatch.amazonaws.com" }
      Condition = { StringEquals = { "aws:SourceAccount" = data.aws_caller_identity.current.account_id } }
    }]
  })
}

resource "aws_sns_topic_policy" "edge_probe" {
  arn    = aws_sns_topic.edge_probe.arn
  policy = local.topic_policy
}

# ── role: least privilege ─────────────────────────────────────────────
resource "aws_iam_role" "edge_probe" {
  name = local.name
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

# jsonencode (not aws_iam_policy_document) so tests/edge_probes.tftest.hcl can
# assert the exact action list at plan time.
locals {
  policy_statements = [
    {
      Sid      = "OwnLogs"
      Effect   = "Allow"
      Action   = ["logs:CreateLogStream", "logs:PutLogEvents"]
      Resource = ["${aws_cloudwatch_log_group.edge_probe.arn}:*"]
    },
    {
      Sid    = "Metrics"
      Effect = "Allow"
      Action = ["cloudwatch:PutMetricData"]
      # PutMetricData has no resource-level scoping; the namespace condition is the scope.
      Resource  = ["*"]
      Condition = { StringEquals = { "cloudwatch:namespace" = "GardenioEdgeProbe" } }
    },
    {
      Sid      = "DescribeEc2" # these Describe calls do not support resource scoping
      Effect   = "Allow"
      Action   = ["ec2:DescribeSecurityGroups", "ec2:DescribeInstances", "ec2:DescribeAddresses"]
      Resource = ["*"]
    },
    {
      Sid      = "ListZoneRecords"
      Effect   = "Allow"
      Action   = ["route53:ListResourceRecordSets"]
      Resource = ["arn:aws:route53:::hostedzone/${var.zone_id}"]
    },
    {
      Sid      = "PublishAlert"
      Effect   = "Allow"
      Action   = ["sns:Publish"]
      Resource = [aws_sns_topic.edge_probe.arn]
    },
  ]
}

resource "aws_iam_role_policy" "edge_probe" {
  name = local.name
  role = aws_iam_role.edge_probe.id
  policy = jsonencode({
    Version   = "2012-10-17"
    Statement = local.policy_statements
  })
}

# ── function: deliberately NO vpc_config ──────────────────────────────
# Inside the VPC it would reach the edge over the private path and would not
# see what an outsider sees. Do not add one.
resource "aws_lambda_function" "edge_probe" {
  function_name    = local.name
  role             = aws_iam_role.edge_probe.arn
  runtime          = "python3.13"
  handler          = "edge_probe.handler"
  timeout          = 120
  memory_size      = 256
  filename         = data.archive_file.edge_probe.output_path
  source_code_hash = data.archive_file.edge_probe.output_base64sha256

  environment {
    variables = {
      EDGE_PROBE_VANTAGE   = "lambda"
      EDGE_PROBE_ZONE_ID   = var.zone_id
      EDGE_PROBE_VPC_ID    = var.vpc_id
      EDGE_PROBE_TOPIC_ARN = aws_sns_topic.edge_probe.arn
    }
  }

  depends_on = [aws_cloudwatch_log_group.edge_probe, aws_iam_role_policy.edge_probe]
  tags       = var.tags
}

# ── schedule ──────────────────────────────────────────────────────────
resource "aws_cloudwatch_event_rule" "edge_probe" {
  name                = local.name
  description         = "Run the edge prober"
  schedule_expression = var.schedule_expression
  tags                = var.tags
}

resource "aws_cloudwatch_event_target" "edge_probe" {
  rule = aws_cloudwatch_event_rule.edge_probe.name
  arn  = aws_lambda_function.edge_probe.arn
}

resource "aws_lambda_permission" "events" {
  statement_id  = "AllowEventBridge"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.edge_probe.function_name
  principal     = "events.amazonaws.com"
  source_arn    = aws_cloudwatch_event_rule.edge_probe.arn
}

# ── alarms (all notify the one topic) ─────────────────────────────────
resource "aws_cloudwatch_metric_alarm" "failures" {
  alarm_name          = "${local.name}-failures"
  alarm_description   = "The edge probe found something open that should not be (a FAIL)."
  namespace           = "GardenioEdgeProbe"
  metric_name         = "Failures"
  statistic           = "Maximum"
  period              = 900
  evaluation_periods  = 1
  datapoints_to_alarm = 1
  threshold           = 1
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching" # a silent prober is the heartbeat alarm's job
  alarm_actions       = [aws_sns_topic.edge_probe.arn]
  tags                = var.tags
}

resource "aws_cloudwatch_metric_alarm" "broken" {
  alarm_name          = "${local.name}-broken"
  alarm_description   = "A positive control failed: the probe cannot be trusted (BROKEN)."
  namespace           = "GardenioEdgeProbe"
  metric_name         = "Broken"
  statistic           = "Maximum"
  period              = 900
  evaluation_periods  = 1
  datapoints_to_alarm = 1
  threshold           = 1
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.edge_probe.arn]
  tags                = var.tags
}

# Dead-man: a prober that stops running emits nothing, so missing data MUST breach.
resource "aws_cloudwatch_metric_alarm" "heartbeat" {
  alarm_name          = "${local.name}-heartbeat-missing"
  alarm_description   = "No edge-probe heartbeat for an hour: the prober is dead or disabled."
  namespace           = "GardenioEdgeProbe"
  metric_name         = "Heartbeat"
  statistic           = "Sum"
  period              = 1800
  evaluation_periods  = 2
  datapoints_to_alarm = 2
  threshold           = 1
  comparison_operator = "LessThanThreshold"
  treat_missing_data  = "breaching"
  alarm_actions       = [aws_sns_topic.edge_probe.arn]
  tags                = var.tags
}

resource "aws_cloudwatch_metric_alarm" "lambda_errors" {
  alarm_name          = "${local.name}-lambda-errors"
  alarm_description   = "The prober Lambda itself raised an error (crash, timeout, failed alert publish)."
  namespace           = "AWS/Lambda"
  metric_name         = "Errors"
  dimensions          = { FunctionName = aws_lambda_function.edge_probe.function_name }
  statistic           = "Sum"
  period              = 900
  evaluation_periods  = 1
  datapoints_to_alarm = 1
  threshold           = 1
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [aws_sns_topic.edge_probe.arn]
  tags                = var.tags
}
