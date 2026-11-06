variable "env_name" {
  description = "Short environment identifier (e.g. \"canonical-prod\"). Drives budget/monitor names."
  type        = string
}

variable "common_tags" {
  description = "Tags applied to every taggable resource (SNS topic, budgets)."
  type        = map(string)
  default     = {}
}

# --- Notification routing -----------------------------------------------

variable "budget_emails" {
  description = <<-EOT
    Email addresses subscribed to the FinOps SNS topic (the paper trail).
    Budget + anomaly notifications fan out here. Empty creates the topic
    with no email subscriptions (Slack/Chatbot can subscribe out-of-band).

    Note: these addresses are ALSO wired directly into every AWS Budgets
    notification via `subscriber_email_addresses` — that native path needs no
    SNS confirmation and never expires. The SNS topic email subscription
    (gated by `email_subscriptions_enabled`) is a second, redundant delivery
    hop that additionally covers anomaly notifications routed through the topic.
  EOT
  type        = list(string)
  default     = []
}

variable "email_subscriptions_enabled" {
  description = <<-EOT
    Subscribe `budget_emails` to the FinOps SNS topic as email subscriptions.
    Default true (backward compatible). Set false to skip the SNS topic email
    subscription entirely.

    Why you may want false: AWS email-protocol SNS subscriptions stay in
    `PendingConfirmation` until the recipient clicks the confirm link, and AWS
    auto-deletes an unconfirmed subscription after ~72h. An unconfirmed sub
    therefore never reaches a stable state, so `terraform plan` re-plans it as
    a phantom "1 to add" on every run — polluting apply-gate plan reviews on a
    long-lived composition (ka-0000006061). When the paper trail is already
    delivered by the budget-native `subscriber_email_addresses` path (and
    anomaly detection is off, or the topic is otherwise consumed only by
    Chatbot/slack-bridge subscribed out-of-band), the SNS email subscription
    adds no delivery guarantee — only plan churn. Setting this false removes
    the churn without touching the paper trail or the topic itself.
  EOT
  type        = bool
  default     = true
}

variable "external_sns_topic_arn" {
  description = <<-EOT
    Optional pre-existing SNS topic ARN to publish notifications to instead
    of creating one. Empty = this module creates `<env_name>-gardenio-finops`
    and wires email subscriptions to it. Set this to reuse an account-wide
    alerting topic (e.g. one already bridged to Slack via AWS Chatbot).
  EOT
  type        = string
  default     = ""
}

# --- Account-level budget -----------------------------------------------

variable "account_budget_usd" {
  description = <<-EOT
    Account-wide monthly cost budget in USD. Per the canonical-prod cost
    model (~$1,150/mo AWS) the recommended ceiling is ~$1,500 (≈30% headroom).
    0 disables the account budget.
  EOT
  type        = number
  default     = 1500

  validation {
    condition     = var.account_budget_usd >= 0
    error_message = "account_budget_usd must be >= 0."
  }
}

variable "account_budget_thresholds" {
  description = <<-EOT
    Percentage thresholds (of actual spend) that trigger an account-budget
    notification. The 100%-forecast notification is added automatically on
    top of these actual-spend thresholds.
  EOT
  type        = list(number)
  default     = [50, 80, 100]
}

# --- Per-component budgets ----------------------------------------------

variable "component_budgets" {
  description = <<-EOT
    Per-component monthly budgets, scoped by the `gardenio:component` cost-
    allocation tag. Map of component -> monthly USD ceiling. Default sizes
    each component at ~120% of its line in canonical-prod-cost-delta.md.
    Empty disables per-component budgets. Each fires at 80% + 100% actual.

    NOTE: the `gardenio:component` tag must be ACTIVATED as a cost-allocation
    tag in the Billing console for tag-scoped budgets to resolve — Terraform
    cannot activate cost-allocation tags (account-root billing action).
  EOT
  type        = map(number)
  default = {
    rds           = 350 # ~120% of the ~$290 RDS line
    networking    = 270 # ~120% of the ~$220 networking line
    security      = 200 # ~120% of the ~$170 security/ops line
    observability = 120 # ~120% of the ~$100 AWS observability line
    platform      = 400 # EKS control plane + infra nodes + addons
  }
}

variable "component_tag_key" {
  description = "Cost-allocation tag key used to scope per-component budgets."
  type        = string
  default     = "gardenio:component"
}

# --- Spot agent-node ceiling --------------------------------------------

variable "agent_spot_budget_usd" {
  description = <<-EOT
    Monthly ceiling for agent-node Spot spend, scoped by `gardenio:component
    = agent`. Default $500 covers the full-duty-cycle upper bound (~$440)
    with headroom and catches a stuck Karpenter/autoscaler loop. 0 disables.
  EOT
  type        = number
  default     = 500
}

# --- Data-transfer / NAT ceiling ----------------------------------------

variable "data_transfer_budget_usd" {
  description = <<-EOT
    Monthly ceiling on data-transfer-out + NAT byte processing — the cheapest
    way to 10× a bill is an egress loop through NAT. Default $100 (≈2× the
    ~$54 baseline). 0 disables. Implemented as a usage-type-filtered COST
    budget on DataTransfer-Out + NatGateway-Bytes.
  EOT
  type        = number
  default     = 100
}

# --- Cost anomaly detection ---------------------------------------------

variable "anomaly_detection_enabled" {
  description = "Provision AWS Cost Anomaly Detection monitors + subscription. Detection itself is free; only the underlying spend is billed."
  type        = bool
  default     = true
}

variable "anomaly_absolute_threshold_usd" {
  description = "Absolute dollar impact above which an anomaly fires a notification. Noise floor for a ~$1,150/mo account."
  type        = number
  default     = 50
}

variable "anomaly_frequency" {
  description = <<-EOT
    Anomaly subscription frequency. The subscription always uses an SNS
    subscriber, and AWS Cost Anomaly Detection only supports IMMEDIATE for SNS
    subscribers (DAILY/WEEKLY digests are EMAIL-only). Must be IMMEDIATE.
  EOT
  type        = string
  default     = "IMMEDIATE"

  validation {
    # SNS subscribers REQUIRE frequency=IMMEDIATE; AWS rejects DAILY/WEEKLY with
    # "Daily or weekly frequencies only support Email subscriptions." Since this
    # module always provisions an SNS subscriber, IMMEDIATE is the only valid value.
    condition     = var.anomaly_frequency == "IMMEDIATE"
    error_message = "anomaly_frequency must be IMMEDIATE: the anomaly subscription uses an SNS subscriber, which AWS only supports with IMMEDIATE frequency (DAILY/WEEKLY are EMAIL-only)."
  }
}

variable "custom_anomaly_monitor_enabled" {
  description = <<-EOT
    Provision the tag-scoped CUSTOM (per-component) cost-anomaly monitor.
    AWS only allows CUSTOM monitors on the Organizations MANAGEMENT account;
    a LINKED/member account apply fails ("Linked accounts can only create AWS
    Services monitor", ka-0000000010). Default false so member-account standups
    succeed with the DIMENSIONAL services monitor alone — set true only when
    applying against the Organizations management account.
  EOT
  type        = bool
  default     = false
}

# --- Read-only audit role (resource enumeration) ------------------------

variable "audit_role_enabled" {
  description = <<-EOT
    Create the read-only `FinOpsAudit` role in this account (see audit_role.tf).
    Default false: budgets/anomaly monitors are useful without it, and a role
    nobody assumes is just an IAM object to explain. Set true in accounts where
    a FinOps agent needs RESOURCE enumeration (idle/orphaned-resource sweeps),
    not just Cost Explorer aggregates. Requires
    `audit_role_trusted_principal_arns`.
  EOT
  type        = bool
  default     = false
}

variable "audit_role_name" {
  description = <<-EOT
    Name of the read-only audit role. Keep the default unless you have a reason
    not to: the FinOps caller's own policy grants sts:AssumeRole on
    `arn:aws:iam::*:role/FinOpsAudit`, so a renamed role is unreachable without
    a matching change on the caller side.
  EOT
  type        = string
  default     = "FinOpsAudit"
}

variable "audit_role_trusted_principal_arns" {
  description = <<-EOT
    Principals allowed to assume the audit role — the FinOps bot user/role ARN,
    or an account root ARN to mirror the payer-account pattern. Empty with
    `audit_role_enabled = true` fails at plan time.
  EOT
  type        = list(string)
  default     = []
}
