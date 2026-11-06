# Native terraform tests for the availability prober and the off-cluster
# status page (uptime.tf, Lands Track B §5.8, gd-rqtbyqCMZJ).
#
# The properties locked down:
#   - the function has NO vpc_config, runs every minute, one run at a time;
#   - the bucket is readable without credentials for exactly the three public
#     keys (state.json stays private) and never through ACLs;
#   - one down alarm per target in scripts/uptime-probe/targets.json, and a
#     dead-man alarm that treats missing data as breaching;
#   - the role grants exactly the documented actions, scoped where it can be.
#
# Run: terraform test   (from modules/edge-probes/)

mock_provider "aws" {}
mock_provider "archive" {}

variables {
  alert_email = "alerts@example.invalid"
  zone_id     = "Z0123456789ABCDEFGHIJ"
  vpc_id      = "vpc-0123456789abcdef0"
}

run "function_runs_every_minute_outside_any_vpc" {
  command = plan

  assert {
    condition     = length(aws_lambda_function.uptime_probe.vpc_config) == 0
    error_message = "the availability prober must not have a vpc_config"
  }

  assert {
    condition     = aws_lambda_function.uptime_probe.handler == "uptime_probe.handler" && aws_lambda_function.uptime_probe.runtime == "python3.13"
    error_message = "handler/runtime drifted from uptime_probe.handler on python3.13"
  }

  assert {
    condition     = aws_lambda_function.uptime_probe.timeout < 60 && aws_lambda_function_event_invoke_config.uptime_probe.maximum_retry_attempts == 0
    error_message = "a run must finish inside its minute, one run at a time (state.json is read-modify-write)"
  }

  assert {
    condition     = aws_cloudwatch_event_rule.uptime_probe.schedule_expression == "rate(1 minute)"
    error_message = "schedule must be rate(1 minute)"
  }
}

run "status_page_is_public_for_three_keys_only" {
  command = plan

  override_resource {
    target          = aws_s3_bucket.status
    override_during = plan
    values = {
      arn = "arn:aws:s3:::gardenio-status-111111111111"
      id  = "gardenio-status-111111111111"
    }
  }

  assert {
    condition = toset(jsondecode(aws_s3_bucket_policy.status.policy).Statement[0].Resource) == toset([
      "arn:aws:s3:::gardenio-status-111111111111/index.html",
      "arn:aws:s3:::gardenio-status-111111111111/status.json",
      "arn:aws:s3:::gardenio-status-111111111111/metrics",
    ])
    error_message = "only index.html, status.json and metrics may be public; state.json stays private"
  }

  assert {
    condition = (length(jsondecode(aws_s3_bucket_policy.status.policy).Statement) == 1 &&
    jsondecode(aws_s3_bucket_policy.status.policy).Statement[0].Action == "s3:GetObject")
    error_message = "the public grant is s3:GetObject and nothing else"
  }

  assert {
    condition = (aws_s3_bucket_public_access_block.status.block_public_acls &&
      aws_s3_bucket_public_access_block.status.ignore_public_acls &&
    aws_s3_bucket_ownership_controls.status.rule[0].object_ownership == "BucketOwnerEnforced")
    error_message = "ACLs stay blocked: the bucket policy is the only public grant"
  }
}

run "one_down_alarm_per_target" {
  command = plan

  assert {
    condition     = toset(keys(aws_cloudwatch_metric_alarm.uptime_down)) == toset(["lands", "garden", "commons_api"])
    error_message = "one down alarm per target in scripts/uptime-probe/targets.json"
  }

  assert {
    condition = alltrue([for a in aws_cloudwatch_metric_alarm.uptime_down :
      a.namespace == "GardenioUptime" && a.metric_name == "Success" && a.period == 60 &&
      a.evaluation_periods == 5 && a.datapoints_to_alarm == 3 && a.threshold == 1 &&
      a.comparison_operator == "LessThanThreshold" && length(a.alarm_actions) == 1 && length(a.ok_actions) == 1
    ])
    error_message = "a down alarm is Success below 1 in 3 of 5 one-minute periods, notifying on alarm and on recovery"
  }

  assert {
    condition = (aws_cloudwatch_metric_alarm.uptime_heartbeat.treat_missing_data == "breaching" &&
      aws_cloudwatch_metric_alarm.uptime_heartbeat.metric_name == "Heartbeat" &&
    aws_cloudwatch_metric_alarm.uptime_heartbeat.comparison_operator == "LessThanThreshold")
    error_message = "the uptime dead-man alarm must treat missing data as breaching"
  }
}

run "uptime_iam_is_least_privilege" {
  command = plan

  override_resource {
    target          = aws_cloudwatch_log_group.uptime_probe
    override_during = plan
    values = {
      arn = "arn:aws:logs:us-east-1:111111111111:log-group:/aws/lambda/gardenio-uptime-probe"
    }
  }

  override_resource {
    target          = aws_s3_bucket.status
    override_during = plan
    values = {
      arn = "arn:aws:s3:::gardenio-status-111111111111"
    }
  }

  assert {
    condition = toset(flatten([for s in jsondecode(aws_iam_role_policy.uptime_probe.policy).Statement : s.Action])) == toset([
      "logs:CreateLogStream", "logs:PutLogEvents",
      "cloudwatch:PutMetricData",
      "s3:GetObject", "s3:PutObject", "s3:ListBucket",
    ])
    error_message = "the role must grant exactly the documented actions and nothing else"
  }

  assert {
    condition = alltrue([for s in jsondecode(aws_iam_role_policy.uptime_probe.policy).Statement :
      contains(s.Resource, "*") ? s.Sid == "Metrics" : true
    ])
    error_message = "only the metrics statement may use Resource *"
  }

  assert {
    condition     = [for s in jsondecode(aws_iam_role_policy.uptime_probe.policy).Statement : s.Condition.StringEquals["cloudwatch:namespace"] if s.Sid == "Metrics"][0] == "GardenioUptime"
    error_message = "PutMetricData must be conditioned on cloudwatch:namespace = GardenioUptime"
  }

  assert {
    condition     = [for s in jsondecode(aws_iam_role_policy.uptime_probe.policy).Statement : s.Resource[0] if s.Sid == "StatusObjects"][0] == "arn:aws:s3:::gardenio-status-111111111111/*"
    error_message = "object access must be scoped to the status bucket"
  }
}
