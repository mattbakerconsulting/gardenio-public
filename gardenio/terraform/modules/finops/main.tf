# modules/finops — cost guardrails for a fresh canonical-prod standup.
#
# Three knobs, all effectively free at our scale (AWS Budgets, Cost Anomaly
# Detection, and an SNS topic cost ~$0/mo):
#
#   1. AWS Budgets — one account-level COST budget, one per-component budget
#      (tag-scoped), one agent-Spot ceiling, one data-transfer/NAT ceiling.
#   2. AWS Cost Anomaly Detection — an AWS_SERVICES monitor + a tag-scoped
#      CUSTOM monitor, both feeding a single subscription.
#   3. SNS topic (`<env>-gardenio-finops`) — the one integration hook. Email
#      subscriptions are the paper trail; AWS Chatbot / slack-bridge subscribe
#      out-of-band for the `#gardenio-ops` fan-out.
#
# Design notes (canonical-prod-finops-plan.md, ka-0000008238):
#   - Module defaults work for any greenfield AWS consumer; nothing acme-
#     specific is baked in. env_name + emails + thresholds are caller inputs.
#   - No budget Actions (auto-scale-to-zero) — the failure mode of throttling
#     during a real agent burst is worse than paying while a human looks.
#   - Tag-scoped budgets/monitors require the `gardenio:component` tag to be
#     ACTIVATED as a cost-allocation tag in Billing (an account-root action
#     Terraform cannot perform — flagged in the README + composition output).
#
# LIVE BUT NOT MANAGED HERE (gardenio account, set by hand 2026-09-18). These
# are account-level rather than per-environment, so they do not belong to this
# composition; recorded here because this is where a reader looks:
#   - `gardenio:component` cost-allocation tag: ACTIVATED. That is the
#     account-root action referred to above — it is done, not pending.
#   - Cost anomaly threshold lowered $100 -> $10 on the
#     Default-Services-Subscription.
#   - SQS queue arn:aws:sqs:us-east-1:360190250083:gardenio-finops-alerts,
#     subscribed to the finops SNS topic (auto-confirms).
#
# READ THIS BEFORE TRUSTING BUDGET ALERTS: the email subscriptions never
# landed. SNS reports 3 delivered / 0 failed, so AWS is sending and the mail
# provider is dropping them. Budget and anomaly alerts are therefore captured
# ONLY in that SQS queue, and NOTHING DRAINS IT. Until either the email
# subscriptions are fixed or something consumes the queue, an alert firing is
# equivalent to no alert at all.

locals {
  module_tags = merge(
    var.common_tags,
    {
      "gardenio:module" = "finops"
      "gardenio:env"    = var.env_name
    },
  )

  name_prefix = "${var.env_name}-gardenio"

  # The SNS topic every notification routes through: created here unless the
  # caller points at a pre-existing account-wide topic.
  create_topic   = var.external_sns_topic_arn == ""
  sns_topic_arn  = local.create_topic ? aws_sns_topic.finops[0].arn : var.external_sns_topic_arn
  sns_topic_arns = [local.sns_topic_arn]

  enable_component_budgets = length(var.component_budgets) > 0
}

# --- SNS topic + email subscriptions ------------------------------------

resource "aws_sns_topic" "finops" {
  count = local.create_topic ? 1 : 0

  name = "${local.name_prefix}-finops"
  tags = local.module_tags
}

# Allow the AWS Budgets + Cost Anomaly Detection services to publish.
data "aws_iam_policy_document" "finops_topic" {
  count = local.create_topic ? 1 : 0

  statement {
    sid     = "AllowBudgetsAndCostAnomalyPublish"
    effect  = "Allow"
    actions = ["SNS:Publish"]

    principals {
      type = "Service"
      identifiers = [
        "budgets.amazonaws.com",
        "costalerts.amazonaws.com",
      ]
    }

    resources = [aws_sns_topic.finops[0].arn]
  }
}

resource "aws_sns_topic_policy" "finops" {
  count = local.create_topic ? 1 : 0

  arn    = aws_sns_topic.finops[0].arn
  policy = data.aws_iam_policy_document.finops_topic[0].json
}

# Email subscriptions on the topic. Gated by email_subscriptions_enabled
# (default true) because an AWS email-protocol SNS subscription sits in
# PendingConfirmation until the recipient clicks the confirm link and is
# auto-deleted after ~72h unconfirmed — so an unconfirmed sub never stabilizes
# and terraform re-plans it as a phantom "1 to add" forever (ka-0000006061).
# The budget-native subscriber_email_addresses path (below) delivers the same
# paper trail with no confirmation and no expiry, so a consumer that does not
# need the topic-level email hop can set this false to kill the plan churn.
resource "aws_sns_topic_subscription" "email" {
  for_each = local.create_topic && var.email_subscriptions_enabled ? toset(var.budget_emails) : toset([])

  topic_arn = aws_sns_topic.finops[0].arn
  protocol  = "email"
  endpoint  = each.value
}

# --- Budget 1: account-level monthly ------------------------------------

resource "aws_budgets_budget" "account" {
  count = var.account_budget_usd > 0 ? 1 : 0

  name         = "${local.name_prefix}-account-monthly"
  budget_type  = "COST"
  limit_amount = format("%d", var.account_budget_usd)
  limit_unit   = "USD"
  time_unit    = "MONTHLY"

  # Actual-spend thresholds.
  dynamic "notification" {
    for_each = toset(var.account_budget_thresholds)
    content {
      comparison_operator        = "GREATER_THAN"
      threshold                  = notification.value
      threshold_type             = "PERCENTAGE"
      notification_type          = "ACTUAL"
      subscriber_email_addresses = local.create_topic ? var.budget_emails : []
      subscriber_sns_topic_arns  = local.sns_topic_arns
    }
  }

  # End-of-month forecast overage — catches a trajectory before it lands.
  notification {
    comparison_operator        = "GREATER_THAN"
    threshold                  = 100
    threshold_type             = "PERCENTAGE"
    notification_type          = "FORECASTED"
    subscriber_email_addresses = local.create_topic ? var.budget_emails : []
    subscriber_sns_topic_arns  = local.sns_topic_arns
  }

  tags = local.module_tags
}

# --- Budget 2: per-component (tag-scoped) -------------------------------

resource "aws_budgets_budget" "component" {
  for_each = local.enable_component_budgets ? var.component_budgets : {}

  name         = "${local.name_prefix}-component-${each.key}"
  budget_type  = "COST"
  limit_amount = format("%d", each.value)
  limit_unit   = "USD"
  time_unit    = "MONTHLY"

  # Budget TagKeyValue format is `user:<TagKey>$<TagValue>`. A `$` immediately
  # followed by an interpolation (`$${each.key}`) is the HCL escape for a
  # literal `${`, which renders `user:gardenio:component${each.key}` verbatim and
  # AWS rejects (InvalidParameterException, ka-0000000670). Use format() so the
  # `$` is an unambiguous literal separator -> `user:gardenio:component$rds`.
  cost_filter {
    name   = "TagKeyValue"
    values = [format("user:%s$%s", var.component_tag_key, each.key)]
  }

  dynamic "notification" {
    for_each = toset([80, 100])
    content {
      comparison_operator        = "GREATER_THAN"
      threshold                  = notification.value
      threshold_type             = "PERCENTAGE"
      notification_type          = "ACTUAL"
      subscriber_email_addresses = local.create_topic ? var.budget_emails : []
      subscriber_sns_topic_arns  = local.sns_topic_arns
    }
  }

  tags = local.module_tags
}

# --- Budget 3: agent-node Spot ceiling ----------------------------------

resource "aws_budgets_budget" "agent_spot" {
  count = var.agent_spot_budget_usd > 0 ? 1 : 0

  name         = "${local.name_prefix}-agent-spot"
  budget_type  = "COST"
  limit_amount = format("%d", var.agent_spot_budget_usd)
  limit_unit   = "USD"
  time_unit    = "MONTHLY"

  cost_filter {
    name   = "TagKeyValue"
    values = ["user:${var.component_tag_key}$agent"]
  }

  notification {
    comparison_operator        = "GREATER_THAN"
    threshold                  = 100
    threshold_type             = "PERCENTAGE"
    notification_type          = "ACTUAL"
    subscriber_email_addresses = local.create_topic ? var.budget_emails : []
    subscriber_sns_topic_arns  = local.sns_topic_arns
  }

  tags = local.module_tags
}

# --- Budget 4: data-transfer + NAT processing ---------------------------

resource "aws_budgets_budget" "data_transfer" {
  count = var.data_transfer_budget_usd > 0 ? 1 : 0

  name         = "${local.name_prefix}-data-transfer"
  budget_type  = "COST"
  limit_amount = format("%d", var.data_transfer_budget_usd)
  limit_unit   = "USD"
  time_unit    = "MONTHLY"

  # UsageTypeGroup dimension catches both internet DataTransfer-Out and the
  # per-GB NAT byte-processing charge — the two egress-loop blowup vectors.
  # Multiple values in one cost_filter are OR'd.
  cost_filter {
    name = "UsageTypeGroup"
    values = [
      "EC2: Data Transfer - Internet (Out)",
      "EC2: NAT Gateway - Data Processed",
    ]
  }

  notification {
    comparison_operator        = "GREATER_THAN"
    threshold                  = 100
    threshold_type             = "PERCENTAGE"
    notification_type          = "ACTUAL"
    subscriber_email_addresses = local.create_topic ? var.budget_emails : []
    subscriber_sns_topic_arns  = local.sns_topic_arns
  }

  tags = local.module_tags
}

# --- Cost anomaly detection ---------------------------------------------

# Monitor 1: per-service step changes (ML-detected expected spend).
resource "aws_ce_anomaly_monitor" "services" {
  count = var.anomaly_detection_enabled ? 1 : 0

  name              = "${local.name_prefix}-services"
  monitor_type      = "DIMENSIONAL"
  monitor_dimension = "SERVICE"

  tags = local.module_tags
}

# Monitor 2: per-component step changes (tag-scoped CUSTOM monitor).
# A CUSTOM monitor tracks specific tag VALUES in aggregate — the AWS API
# rejects an empty Values list, so we enumerate the components we budget for.
#
# CUSTOM monitors can only be created by an AWS Organizations MANAGEMENT
# account; a LINKED/member account apply fails with "Linked accounts can only
# create AWS Services monitor" (ka-0000000010). Gated behind a var that defaults
# false so member-account standups succeed with just the DIMENSIONAL services
# monitor; set true only on the Organizations management account.
resource "aws_ce_anomaly_monitor" "component" {
  count = var.anomaly_detection_enabled && var.custom_anomaly_monitor_enabled && length(var.component_budgets) > 0 ? 1 : 0

  name         = "${local.name_prefix}-component"
  monitor_type = "CUSTOM"

  monitor_specification = jsonencode({
    Tags = {
      Key          = var.component_tag_key
      MatchOptions = ["EQUALS"]
      Values       = sort(keys(var.component_budgets))
    }
  })

  tags = local.module_tags
}

resource "aws_ce_anomaly_subscription" "this" {
  count = var.anomaly_detection_enabled ? 1 : 0

  name = "${local.name_prefix}-anomaly-sub"
  # Must be IMMEDIATE: this subscription uses an SNS subscriber (below), and AWS
  # only supports IMMEDIATE for SNS — DAILY/WEEKLY digests are EMAIL-only. The
  # var validation enforces IMMEDIATE so an invalid combo fails at plan time.
  frequency = var.anomaly_frequency

  # The per-component monitor is conditional on having components to watch.
  monitor_arn_list = concat(
    [aws_ce_anomaly_monitor.services[0].arn],
    length(aws_ce_anomaly_monitor.component) > 0 ? [aws_ce_anomaly_monitor.component[0].arn] : [],
  )

  subscriber {
    type    = "SNS"
    address = local.sns_topic_arn
  }

  # Cost Anomaly Detection validates SNS publish permission at subscription-
  # create time, so the topic policy granting costalerts.amazonaws.com must
  # exist first. Harmless when the topic is external (policy count = 0).
  depends_on = [aws_sns_topic_policy.finops]

  # Only fire when the total impact crosses the absolute noise floor.
  threshold_expression {
    dimension {
      key           = "ANOMALY_TOTAL_IMPACT_ABSOLUTE"
      match_options = ["GREATER_THAN_OR_EQUAL"]
      values        = [tostring(var.anomaly_absolute_threshold_usd)]
    }
  }

  tags = local.module_tags
}
