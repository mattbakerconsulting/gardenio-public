output "sns_topic_arn" {
  description = "ARN of the FinOps SNS topic notifications route through (created or external). Subscribe AWS Chatbot / slack-bridge here for the #gardenio-ops fan-out."
  value       = local.sns_topic_arn
}

output "account_budget_name" {
  description = "Name of the account-level monthly budget (empty if disabled)."
  value       = var.account_budget_usd > 0 ? aws_budgets_budget.account[0].name : ""
}

output "component_budget_names" {
  description = "Names of the per-component budgets keyed by component."
  value       = { for k, b in aws_budgets_budget.component : k => b.name }
}

output "anomaly_monitor_arns" {
  description = "ARNs of the Cost Anomaly Detection monitors (empty list if disabled). The per-component monitor is present only when component_budgets is non-empty."
  value = var.anomaly_detection_enabled ? concat(
    [aws_ce_anomaly_monitor.services[0].arn],
    length(aws_ce_anomaly_monitor.component) > 0 ? [aws_ce_anomaly_monitor.component[0].arn] : [],
  ) : []
}

output "cost_allocation_tag_activation_required" {
  description = "Tag key that MUST be activated as a cost-allocation tag in Billing (account-root action Terraform cannot perform) for tag-scoped budgets/monitors to resolve."
  value       = var.component_tag_key
}

output "audit_role_arn" {
  description = "ARN of the read-only FinOps audit role (empty when audit_role_enabled = false). This is what a FinOps agent assumes for resource enumeration."
  value       = local.create_audit_role ? aws_iam_role.audit[0].arn : ""
}
