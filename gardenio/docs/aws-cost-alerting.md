# AWS budget + Cost Anomaly Detection alerting — gardenio account

> **Scope.** The **gardenio account** (`prod-gardenio-eks`, AWS `123456789012`,
> `us-east-1`) — *our own agent fleet*, **not** customer prod (`acme-prod-v2`)
> and **not** the payer (`mgmt-542`). This is the cost-alerting half of the
> "alerting on fleet size and utilization" deliverable in the gardenio
> cost-control plan (bead `ka-0000006909`), and the account-level alerts
> line-item of the FinOps program epic (`ka-0000003540`).
>
> **What this doc is.** The runbook + as-code config for two guardrails:
> 1. a monthly **AWS Budget** on the gardenio linked account, with actual +
>    forecast threshold alerts, and
> 2. an **AWS Cost Anomaly Detection** monitor scoped to the account, both
>    sinking to Slack **#aws-cost-management** (`C0BGK3N4LNN`).
>
> **Why it's a runbook and not a merged terraform apply.** Budgets, Cost
> Anomaly Detection, SNS, and AWS Chatbot are all **privileged writes**
> (`budgets:Create*`, `ce:CreateAnomalyMonitor`, `ce:CreateAnomalySubscription`,
> `sns:CreateTopic`, `chatbot:*`). The read-only agent role in acct 133 cannot
> perform them — confirmed live 2026-08-13 (every call below returned
> `AccessDenied`). The concrete, copy-pasteable config lives here and is
> attached to escalation **ka-0000005498** for an operator/admin principal to
> apply. Nothing here mutates until an operator runs it.

---

## TL;DR

| Guardrail | What it catches | Threshold | Sink |
|---|---|---|---|
| Monthly **AWS Budget** | Slow drift / sustained overspend against a planned monthly ceiling | actual 80% / forecast 100% / actual 100% / actual 120% | Slack #aws-cost-management |
| **Cost Anomaly Detection** | Sudden spikes a static budget misses (a new service, a runaway loop, a stuck instance) | ML baseline; alert on impact ≥ **$100** absolute **and** ≥ **40%** above expected | Slack #aws-cost-management |

**Seed budget: `$4,500 / month`.** Grounded in the plan's run-rate: net-billed
~**$138/day ≈ $4.1k/mo** today (gross usage is higher — Savings Plans absorb
part of it). $4.5k leaves ~10% headroom above current net so the **80% actual**
alert (~$3.6k) fires on drift rather than on steady state. **Tighten this as
waste is burned down** (plan children P2a/P2b/P2c) — the budget is a moving
ceiling, not a fixed allowance.

Two independent signals by design (cf. advice ka-0000005924): the **budget** is
a static monthly ceiling that catches slow drift; **anomaly detection** is an ML
baseline that catches sudden spikes the budget's monthly horizon would miss.
Neither alone is sufficient.

---

## 1. Prerequisites (one-time)

### 1a. Cost Explorer must be enabled
Budgets and Cost Anomaly Detection both depend on Cost Explorer being enabled on
the account. It already is (CE analysis has been driving the plan numbers). No
action needed; noted for completeness.

### 1b. Slack delivery path — pick ONE

Budgets and Cost Anomaly Detection deliver to Slack via one of two paths. Both
are documented; **AWS Chatbot (path A) is preferred** because it renders native
Slack cards and needs no Lambda.

- **Path A — AWS Chatbot (recommended).** SNS topic → AWS Chatbot Slack channel
  configuration → posts into #aws-cost-management. One-time: authorize the AWS
  Chatbot Slack app in the acme workspace (operator/Slack-admin action).
- **Path B — SNS + email/Lambda.** SNS topic with an email subscription, or a
  small Lambda that reposts to a Slack incoming webhook. Use only if the Chatbot
  app cannot be installed.

Both paths share the **same SNS topic**, created in §2.

---

## 2. SNS topic + Slack wiring (shared sink)

```bash
set -euo pipefail
ACCOUNT=123456789012
REGION=us-east-1            # Budgets is a global (us-east-1) service; keep the topic here.
TOPIC_NAME=gardenio-cost-alerts

# 2.1 Create the topic.
TOPIC_ARN=$(aws sns create-topic \
  --name "$TOPIC_NAME" \
  --region "$REGION" \
  --tags Key=purpose,Value=cost-alerting Key=account,Value=gardenio \
  --query TopicArn --output text)
echo "TOPIC_ARN=$TOPIC_ARN"

# 2.2 Allow the AWS Budgets + Cost Anomaly Detection service principals to publish.
cat > /tmp/sns-policy.json <<JSON
{
  "Version": "2012-10-17",
  "Id": "gardenio-cost-alerts-policy",
  "Statement": [
    {
      "Sid": "AllowBudgets",
      "Effect": "Allow",
      "Principal": { "Service": "budgets.amazonaws.com" },
      "Action": "SNS:Publish",
      "Resource": "$TOPIC_ARN"
    },
    {
      "Sid": "AllowCostAnomalyDetection",
      "Effect": "Allow",
      "Principal": { "Service": "costalerts.amazonaws.com" },
      "Action": "SNS:Publish",
      "Resource": "$TOPIC_ARN"
    }
  ]
}
JSON
aws sns set-topic-attributes \
  --topic-arn "$TOPIC_ARN" \
  --attribute-name Policy \
  --attribute-value "$(cat /tmp/sns-policy.json)" \
  --region "$REGION"
```

### 2.3 Slack — Path A (AWS Chatbot)

```bash
# One-time Slack-workspace authorization is done in the AWS Chatbot console
# (Configure a Slack client) — it cannot be scripted headlessly the first time.
# After the workspace is authorized, capture its Slack workspace/team id and the
# target channel id (#aws-cost-management = C0BGK3N4LNN), then:
aws chatbot create-slack-channel-configuration \
  --configuration-name gardenio-cost-alerts \
  --iam-role-arn arn:aws:iam::123456789012:role/gardenio-chatbot-cost-alerts \
  --slack-channel-id C0BGK3N4LNN \
  --slack-workspace-id <SLACK_WORKSPACE_ID> \
  --sns-topic-arns "$TOPIC_ARN" \
  --logging-level INFO
# The IAM role needs the AWS-managed policy AWSResourceExplorerReadOnlyAccess or
# a minimal read policy; Chatbot uses it only to enrich notifications, not to
# read cost data. A CloudWatch-notifications-only role is sufficient.
```

### 2.3 (alt) Slack — Path B (email fallback)

```bash
aws sns subscribe \
  --topic-arn "$TOPIC_ARN" \
  --protocol email \
  --notification-endpoint aws-cost-management-aaaa@acme.slack.com \
  --region "$REGION"   # confirm the subscription from the email AWS sends
```

---

## 3. Monthly AWS Budget (actual + forecast)

Budgets is a **global** service — always target `us-east-1`. The budget below is
scoped to the gardenio **linked account** so it measures exactly this account's
spend even though billing rolls up to the payer.

Save as `budget.json`:

```json
{
  "BudgetName": "gardenio-account-monthly",
  "BudgetLimit": { "Amount": "4500", "Unit": "USD" },
  "TimeUnit": "MONTHLY",
  "BudgetType": "COST",
  "CostFilters": { "LinkedAccount": ["123456789012"] },
  "CostTypes": {
    "IncludeCredit": false,
    "IncludeDiscount": true,
    "IncludeOtherSubscription": true,
    "IncludeRecurring": true,
    "IncludeRefund": false,
    "IncludeSubscription": true,
    "IncludeSupport": true,
    "IncludeTax": true,
    "IncludeUpfront": true,
    "UseAmortized": false,
    "UseBlended": false
  }
}
```

Save as `notifications.json` (four thresholds → the same SNS topic):

```json
[
  {
    "Notification": { "NotificationType": "ACTUAL", "ComparisonOperator": "GREATER_THAN", "Threshold": 80, "ThresholdType": "PERCENTAGE" },
    "Subscribers": [ { "SubscriptionType": "SNS", "Address": "REPLACE_WITH_TOPIC_ARN" } ]
  },
  {
    "Notification": { "NotificationType": "FORECASTED", "ComparisonOperator": "GREATER_THAN", "Threshold": 100, "ThresholdType": "PERCENTAGE" },
    "Subscribers": [ { "SubscriptionType": "SNS", "Address": "REPLACE_WITH_TOPIC_ARN" } ]
  },
  {
    "Notification": { "NotificationType": "ACTUAL", "ComparisonOperator": "GREATER_THAN", "Threshold": 100, "ThresholdType": "PERCENTAGE" },
    "Subscribers": [ { "SubscriptionType": "SNS", "Address": "REPLACE_WITH_TOPIC_ARN" } ]
  },
  {
    "Notification": { "NotificationType": "ACTUAL", "ComparisonOperator": "GREATER_THAN", "Threshold": 120, "ThresholdType": "PERCENTAGE" },
    "Subscribers": [ { "SubscriptionType": "SNS", "Address": "REPLACE_WITH_TOPIC_ARN" } ]
  }
]
```

Create it:

```bash
sed -i "s#REPLACE_WITH_TOPIC_ARN#$TOPIC_ARN#g" notifications.json
aws budgets create-budget \
  --account-id 123456789012 \
  --budget file://budget.json \
  --notifications-with-subscribers file://notifications.json \
  --region us-east-1
```

**Threshold rationale.**
- **ACTUAL 80%** (~$3.6k) — early-warning that the month is trending hot before
  the ceiling is hit.
- **FORECASTED 100%** — Budgets' own projection says end-of-month will exceed the
  ceiling; fires days ahead of the actual breach.
- **ACTUAL 100%** (~$4.5k) — the ceiling is reached; treat as a page.
- **ACTUAL 120%** (~$5.4k) — hard overshoot; something is materially wrong.

---

## 4. Cost Anomaly Detection monitor + subscription

Anomaly Detection is a **global** service — target `us-east-1`. Two objects: a
**monitor** (what to watch) and a **subscription** (when + where to alert).

```bash
# 4.1 Monitor — dimension = LINKED_ACCOUNT gives one ML baseline for the whole
# gardenio account. (Switch to a SERVICE monitor later if you want per-service
# baselines; LINKED_ACCOUNT is the right first cut for a single account.)
MONITOR_ARN=$(aws ce create-anomaly-monitor \
  --anomaly-monitor '{
    "MonitorName": "gardenio-account-monitor",
    "MonitorType": "DIMENSIONAL",
    "MonitorDimension": "SERVICE"
  }' \
  --region us-east-1 \
  --query MonitorArn --output text)
echo "MONITOR_ARN=$MONITOR_ARN"
```

> **Note on `MonitorDimension`.** The CE API only accepts `SERVICE` for a
> `DIMENSIONAL` monitor (there is no `LINKED_ACCOUNT` dimension value). Because
> this account is already a single linked account, a `SERVICE` monitor **is**
> effectively "this account, broken down by service" — which is what we want: it
> tells you *which service* spiked, not just that the account did. (A true
> account-scoping filter, if this monitor ever ran at the payer, would be
> expressed on the **subscription's** `MonitorArnList` + a cost filter, not on
> the monitor dimension.)

```bash
# 4.2 Subscription — alert when anomaly impact is material.
# THRESHOLDEXPRESSION requires BOTH an absolute-dollar AND a percentage gate so
# we don't page on a $3 blip that happens to be 300% of a tiny baseline.
aws ce create-anomaly-subscription \
  --anomaly-subscription '{
    "SubscriptionName": "gardenio-cost-alerts",
    "MonitorArnList": ["'"$MONITOR_ARN"'"],
    "Frequency": "IMMEDIATE",
    "Subscribers": [
      { "Type": "SNS", "Address": "'"$TOPIC_ARN"'" }
    ],
    "ThresholdExpression": {
      "And": [
        { "Dimensions": { "Key": "ANOMALY_TOTAL_IMPACT_ABSOLUTE", "Values": ["100"], "MatchOptions": ["GREATER_THAN_OR_EQUAL"] } },
        { "Dimensions": { "Key": "ANOMALY_TOTAL_IMPACT_PERCENTAGE", "Values": ["40"], "MatchOptions": ["GREATER_THAN_OR_EQUAL"] } }
      ]
    }
  }' \
  --region us-east-1
```

**Threshold rationale.** `IMMEDIATE` frequency delivers each anomaly as it's
detected (vs. a `DAILY`/`WEEKLY` digest). The `AND` of **≥ $100 absolute** and
**≥ 40% over expected** suppresses noise: a spike must be both dollar-material
*and* proportionally significant to page. Given ~$138/day net, $100 is a
meaningful single-day movement. Loosen the percentage or drop the absolute floor
if too few anomalies surface; tighten if it's noisy.

> **SNS-subscriber note.** A CE anomaly subscription with an **SNS** subscriber
> requires `ThresholdExpression` (not the legacy scalar `Threshold`) and the
> topic policy from §2.2 allowing `costalerts.amazonaws.com` to publish.

---

## 5. Verification (run after the operator applies)

```bash
# Budget exists with the four notifications:
aws budgets describe-budget --account-id 123456789012 \
  --budget-name gardenio-account-monthly --region us-east-1
aws budgets describe-notifications-for-budget --account-id 123456789012 \
  --budget-name gardenio-account-monthly --region us-east-1

# Anomaly monitor + subscription exist:
aws ce get-anomaly-monitors --region us-east-1 \
  --query 'AnomalyMonitors[?MonitorName==`gardenio-account-monitor`]'
aws ce get-anomaly-subscriptions --region us-east-1 \
  --query 'AnomalySubscriptions[?SubscriptionName==`gardenio-cost-alerts`]'

# SNS topic + subscription/Chatbot config exist:
aws sns get-topic-attributes --topic-arn "$TOPIC_ARN" --region us-east-1
aws sns list-subscriptions-by-topic --topic-arn "$TOPIC_ARN" --region us-east-1
aws chatbot describe-slack-channel-configurations   # if Path A

# End-to-end smoke: publish a synthetic message and confirm it lands in Slack.
aws sns publish --topic-arn "$TOPIC_ARN" --region us-east-1 \
  --subject "gardenio cost alerting — smoke test" \
  --message "If you can read this in #aws-cost-management, the SNS→Slack path works."
```

**Acceptance is met when:** the budget + its four notifications exist, the
anomaly monitor + subscription exist, and the §5 SNS smoke message appears in
**#aws-cost-management**.

---

## 6. Operations

- **Tighten the ceiling as waste burns down.** The $4.5k seed reflects *today's*
  wasteful run-rate. As P2a (idle sweep), P2b (right-size EC2 — the fleet is 100%
  On-Demand, zero Spot), and P2c (Bedrock review) land, lower `BudgetLimit` so the
  80% alert keeps biting. A budget that never fires is a budget set too high.
- **Where alerts land.** #aws-cost-management (`C0BGK3N4LNN`). Budget threshold
  breaches and anomaly detections both post there.
- **Teardown / edit.** `aws budgets update-budget`, `aws ce update-anomaly-*`,
  `aws sns …`. Every change is an operator action (privileged writes).
- **Cross-refs.** Plan-of-record `ka-0000006909`; FinOps program `ka-0000003540`;
  finops read-access escalation `ka-0000006541` (unblocks the resource-level P2a
  sweep — independent of this alerting work). Two-independent-signals rationale:
  advice `ka-0000005924`.

---

## 7. Why this is an escalation, not a merged apply

The gardenio-account IaC (`composition/canonical-prod`, applied by the release
pipeline as a targeted `terraform apply -target=module.gardenio`) is **not** in
this monorepo, and the agent role in acct 133 is **read-only for cost/billing**.
Confirmed live 2026-08-13 from a gardenio agent pod — every write path is denied:

```
aws budgets describe-budgets           → AccessDenied (budgets:ViewBudget)
aws ce get-anomaly-monitors            → AccessDenied (ce:GetAnomalyMonitors)
aws sns list-topics                    → AuthorizationError (SNS:ListTopics)
aws ce get-cost-and-usage              → AccessDenied (ce:GetCostAndUsage)
```

So the deliverable is this runbook (merged to `gardenio/docs`) plus the executable
config carried on escalation **ka-0000005498** for an operator to apply. Once the
operator wires the same config into the terraform composition, this doc becomes
the human-readable companion to that code.
