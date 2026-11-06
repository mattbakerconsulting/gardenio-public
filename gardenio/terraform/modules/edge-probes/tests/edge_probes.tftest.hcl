# Native terraform tests for the standing edge prober (gd-xuxyIMbfCU).
#
# The properties locked down:
#   - the function has NO vpc_config (a VPC vantage would not see what an
#     outsider sees);
#   - the dead-man alarm treats missing data as breaching (a dead prober emits
#     nothing, so anything else would stay green forever);
#   - the schedule is rate(15 minutes);
#   - the role grants exactly the documented actions, no "*" action, and each
#     resource-scopable action is scoped.
#
# Run: terraform test   (from modules/edge-probes/)

mock_provider "aws" {}
mock_provider "archive" {}

variables {
  alert_email = "alerts@example.invalid"
  zone_id     = "Z0123456789ABCDEFGHIJ"
  vpc_id      = "vpc-0123456789abcdef0"
}

run "function_is_outside_any_vpc" {
  command = plan

  assert {
    condition     = length(aws_lambda_function.edge_probe.vpc_config) == 0
    error_message = "the prober Lambda must not have a vpc_config"
  }

  assert {
    condition     = aws_lambda_function.edge_probe.handler == "edge_probe.handler" && aws_lambda_function.edge_probe.runtime == "python3.13"
    error_message = "handler/runtime drifted from edge_probe.handler on python3.13"
  }

  assert {
    condition     = aws_lambda_function.edge_probe.timeout == 120 && aws_lambda_function.edge_probe.memory_size == 256
    error_message = "timeout/memory drifted from 120s/256MB"
  }

  assert {
    condition     = aws_lambda_function.edge_probe.environment[0].variables["EDGE_PROBE_VANTAGE"] == "lambda"
    error_message = "EDGE_PROBE_VANTAGE must be lambda"
  }

  assert {
    condition = alltrue([for k in ["EDGE_PROBE_ZONE_ID", "EDGE_PROBE_VPC_ID", "EDGE_PROBE_TOPIC_ARN"] :
    contains(keys(aws_lambda_function.edge_probe.environment[0].variables), k)])
    error_message = "zone, vpc and topic env vars must be set"
  }
}

run "schedule_is_every_15_minutes" {
  command = plan

  assert {
    condition     = aws_cloudwatch_event_rule.edge_probe.schedule_expression == "rate(15 minutes)"
    error_message = "schedule must be rate(15 minutes)"
  }
}

run "heartbeat_alarm_breaches_on_missing_data" {
  command = plan

  assert {
    condition     = aws_cloudwatch_metric_alarm.heartbeat.treat_missing_data == "breaching"
    error_message = "the dead-man alarm must treat missing data as breaching"
  }

  assert {
    condition = (aws_cloudwatch_metric_alarm.heartbeat.metric_name == "Heartbeat" &&
      aws_cloudwatch_metric_alarm.heartbeat.statistic == "Sum" &&
      aws_cloudwatch_metric_alarm.heartbeat.period == 1800 &&
      aws_cloudwatch_metric_alarm.heartbeat.evaluation_periods == 2 &&
      aws_cloudwatch_metric_alarm.heartbeat.threshold == 1 &&
    aws_cloudwatch_metric_alarm.heartbeat.comparison_operator == "LessThanThreshold")
    error_message = "heartbeat alarm must be Sum < 1 over 2 periods of 30 minutes"
  }
}

run "every_alarm_notifies_the_topic" {
  command = plan

  assert {
    condition = alltrue([for a in [
      aws_cloudwatch_metric_alarm.failures,
      aws_cloudwatch_metric_alarm.broken,
      aws_cloudwatch_metric_alarm.heartbeat,
      aws_cloudwatch_metric_alarm.lambda_errors,
    ] : length(a.alarm_actions) == 1])
    error_message = "every alarm needs exactly the SNS topic as its alarm action"
  }

  assert {
    condition = (aws_cloudwatch_metric_alarm.failures.threshold == 1 &&
      aws_cloudwatch_metric_alarm.broken.threshold == 1 &&
      aws_cloudwatch_metric_alarm.lambda_errors.threshold == 1 &&
      aws_cloudwatch_metric_alarm.failures.period == 900 &&
    aws_cloudwatch_metric_alarm.broken.period == 900)
    error_message = "Failures/Broken/Errors alarms must be >= 1 over one 15 minute period"
  }

  assert {
    condition     = aws_sns_topic_subscription.email.protocol == "email" && aws_sns_topic_subscription.email.endpoint == "alerts@example.invalid"
    error_message = "the topic must have the email subscription"
  }
}

run "iam_is_least_privilege" {
  command = plan

  # The policy embeds these ARNs; without a plan-time value the whole policy
  # string is unknown and cannot be decoded.
  override_resource {
    target          = aws_cloudwatch_log_group.edge_probe
    override_during = plan
    values = {
      arn = "arn:aws:logs:us-east-1:111111111111:log-group:/aws/lambda/gardenio-edge-probe"
    }
  }

  override_resource {
    target          = aws_sns_topic.edge_probe
    override_during = plan
    values = {
      arn = "arn:aws:sns:us-east-1:111111111111:gardenio-edge-probe"
    }
  }

  # Every action the role may use, flattened.
  assert {
    condition = toset(flatten([for s in jsondecode(aws_iam_role_policy.edge_probe.policy).Statement : s.Action])) == toset([
      "logs:CreateLogStream", "logs:PutLogEvents",
      "cloudwatch:PutMetricData",
      "ec2:DescribeSecurityGroups", "ec2:DescribeInstances", "ec2:DescribeAddresses",
      "route53:ListResourceRecordSets",
      "sns:Publish",
    ])
    error_message = "the role must grant exactly the documented actions and nothing else"
  }

  assert {
    condition     = !anytrue([for s in jsondecode(aws_iam_role_policy.edge_probe.policy).Statement : anytrue([for a in s.Action : strcontains(a, "*")])])
    error_message = "no IAM action may contain a wildcard"
  }

  assert {
    condition = alltrue([for s in jsondecode(aws_iam_role_policy.edge_probe.policy).Statement :
    s.Effect == "Allow"])
    error_message = "the role policy must be allow-only"
  }

  # Only the unscopable actions may use Resource "*": PutMetricData (conditioned
  # on the namespace) and the three ec2 Describe calls.
  assert {
    condition = alltrue([for s in jsondecode(aws_iam_role_policy.edge_probe.policy).Statement :
      contains(s.Resource, "*") ? contains(["Metrics", "DescribeEc2"], s.Sid) : true
    ])
    error_message = "only the metrics and ec2 Describe statements may use Resource *"
  }

  assert {
    condition     = [for s in jsondecode(aws_iam_role_policy.edge_probe.policy).Statement : s.Condition.StringEquals["cloudwatch:namespace"] if s.Sid == "Metrics"][0] == "GardenioEdgeProbe"
    error_message = "PutMetricData must be conditioned on cloudwatch:namespace = GardenioEdgeProbe"
  }

  assert {
    condition     = [for s in jsondecode(aws_iam_role_policy.edge_probe.policy).Statement : s.Resource[0] if s.Sid == "ListZoneRecords"][0] == "arn:aws:route53:::hostedzone/Z0123456789ABCDEFGHIJ"
    error_message = "route53:ListResourceRecordSets must be scoped to the one zone"
  }
}
