# modules/finops

Cost guardrails for a fresh canonical-prod standup: AWS Budgets + AWS Cost
Anomaly Detection, fanned out through a single SNS topic. All three are
effectively **$0/mo** at our scale — the point is to catch a cost regression
before finance does, not to add cost.

Bakes the FinOps plan (`docs/research/canonical-prod-finops-plan.md`,
ka-0000008238) into Terraform, sized off the line items in
`docs/research/canonical-prod-cost-delta.md`. The DR (RTO/RPO) half of the
companion task lives in `docs/research/canonical-prod-dr-cost-posture.md`.

## What it provisions

| Resource | Purpose |
|---|---|
| `aws_sns_topic.finops` | The one notification hook. Email subscriptions are the paper trail; subscribe AWS Chatbot / slack-bridge here for the `#gardenio-ops` fan-out. Skipped if `external_sns_topic_arn` is set. |
| `aws_budgets_budget.account` | Account-wide monthly COST budget. Default $1,500 (~30% over the ~$1,150/mo model). Fires at 50/80/100% actual + 100% forecast. |
| `aws_budgets_budget.component` | One per `component_budgets` entry, scoped by the `gardenio:component` tag. Fires at 80/100% actual. |
| `aws_budgets_budget.agent_spot` | Ceiling on agent-node Spot spend (`gardenio:component=agent`). Catches a stuck Karpenter/autoscaler loop. |
| `aws_budgets_budget.data_transfer` | Ceiling on DataTransfer-Out + NAT byte processing — the cheapest way to 10× a bill is an egress loop. |
| `aws_ce_anomaly_monitor.services` | ML-detected per-service step changes. |
| `aws_ce_anomaly_monitor.component` | Tag-scoped per-component step changes. Opt-in (`custom_anomaly_monitor_enabled`, default off) — CUSTOM monitors require the Organizations management account. |
| `aws_ce_anomaly_subscription.this` | Anomalies ≥ the absolute threshold, to the SNS topic. Frequency is fixed `IMMEDIATE` (AWS only allows DAILY/WEEKLY for Email subscribers). |
| `aws_iam_role.audit` + `aws_iam_role_policy.audit_read` | Opt-in (`audit_role_enabled`, default off). The read-only `FinOpsAudit` role a FinOps agent assumes for RESOURCE enumeration — Describe/List on EC2/ELB/ASG/EKS/RDS, CloudWatch metric reads, Resource Groups Tagging, plus an explicit deny on data-plane/secret reads. Budgets tell you the bill went up; this tells you which unattached volume, idle NAT or orphaned EIP did it. |

## Operator action required (cannot be Terraformed)

Tag-scoped budgets (and the per-component anomaly monitor, when enabled via
`custom_anomaly_monitor_enabled` on an Organizations management account) only
resolve once the
`gardenio:component` tag is **activated as a cost-allocation tag** in the
Billing console (Billing → Cost allocation tags → activate `gardenio:component`).
This is an account-root billing action with no Terraform resource. The
composition surfaces the tag key needing activation via the `finops` output
(`cost_allocation_tag_activation_required`). Until activated, the account-level
budget and the data-transfer budget still work (they filter on built-in AWS
dimensions, not a user tag); the per-component budgets **and the agent-Spot
budget** stay empty, since the latter is also tag-scoped
(`user:gardenio:component$agent`).

**Activation alone is not sufficient — the resources must also carry the tag.**
This module only *consumes* `gardenio:component`; it is the **caller's** job to
assign the tag to the resources it provisions (the module stays portable and
bakes in no cost taxonomy). The canonical-prod composition does this via
`local.component_tags` in `locals.tf`. A consumer that activates the tag but
never tags any resource will see $0 component budgets. See the operator
checklist: `gardenio/docs/runbooks/finops-cost-allocation-tag.md`.

Email subscriptions also require each recipient to **confirm** the SNS
subscription via the email AWS sends — until confirmed they receive nothing.
Worse for long-lived state: an AWS email-protocol SNS subscription that is
never confirmed is auto-deleted after ~72h, so it never reaches a stable state
and `terraform plan` re-plans it as a phantom "1 to add" on every run
(ka-0000006061). Two independent email delivery paths exist:

- **Budget-native** — `budget_emails` are wired directly into every AWS Budgets
  notification via `subscriber_email_addresses`. No confirmation, no expiry.
  This is the paper trail that actually works out of the box.
- **SNS topic email hop** — `aws_sns_topic_subscription.email`, gated by
  `email_subscriptions_enabled` (default `true`). This is the one that needs
  confirmation and churns when left unconfirmed. Its only unique value is
  covering **anomaly** notifications routed through the topic; budget
  notifications are already covered by the native path. If anomaly detection is
  off (or the topic is consumed only by Chatbot/slack-bridge subscribed
  out-of-band), set `email_subscriptions_enabled = false` to drop the redundant
  hop and stop the plan churn without touching the paper trail.

## Design choices

- **Portable.** No acme-specific value in any default. `env_name`, emails,
  thresholds, and budget amounts are caller inputs.
- **No budget Actions.** We deliberately do NOT wire auto-scale-to-zero / IAM
  throttling Actions. The failure mode of throttling during a legitimate agent
  burst (e.g. a demo) is worse than paying an extra $200 while a human looks.
- **Notify, don't remediate.** Anomaly subscriptions notify only; no
  auto-remediation. Revisit after 6 months of real data.
- **One topic.** Every budget + anomaly notification routes through the single
  SNS topic so Slack routing is configured in exactly one place.

## Usage

```hcl
module "finops" {
  source = "../../modules/finops"

  env_name      = "canonical-prod"
  common_tags   = var.common_tags
  budget_emails = ["gardenio-ops@example.com", "finance@example.com"]

  account_budget_usd = 1500
  # component_budgets / agent_spot / data_transfer / anomaly all default sanely
}
```

To reuse an account-wide Slack-bridged topic instead of creating one:

```hcl
  external_sns_topic_arn = "arn:aws:sns:us-east-1:123456789012:account-alerts"
```

To create the read-only audit role for a FinOps agent's resource sweeps:

```hcl
  audit_role_enabled                = true
  audit_role_trusted_principal_arns = ["arn:aws:iam::123456789012:user/finops-bot"]
```

Keep `audit_role_name` at its `FinOpsAudit` default: the caller side is
conventionally granted `sts:AssumeRole` on `arn:aws:iam::*:role/FinOpsAudit`, so
a renamed role is unreachable without a matching change on the caller. Assuming
a role that does not exist returns **AccessDenied, not NoSuchEntity** — which is
how a missing role once got escalated as a missing permission (ka-0000006541).

## Inputs

See `variables.tf` — every input is documented inline with its default and the
cost-model line it derives from. Key knobs: `account_budget_usd`,
`component_budgets`, `agent_spot_budget_usd`, `data_transfer_budget_usd`,
`anomaly_absolute_threshold_usd`, `custom_anomaly_monitor_enabled`,
`audit_role_enabled`.

## Outputs

| Output | Description |
|---|---|
| `sns_topic_arn` | Topic to subscribe Chatbot / slack-bridge to. |
| `account_budget_name` | Account-level budget name (empty if disabled). |
| `component_budget_names` | Map of component → budget name. |
| `anomaly_monitor_arns` | Anomaly monitor ARNs (empty if disabled). |
| `cost_allocation_tag_activation_required` | Tag key to activate in Billing. |
| `audit_role_arn` | ARN of the read-only audit role (empty if disabled). |
