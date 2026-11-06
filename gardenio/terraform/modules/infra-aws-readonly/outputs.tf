output "user_names" {
  description = "IAM user per subject (ServiceAccount name => user name). Mint a key for each out of band (README)."
  value       = { for k, u in aws_iam_user.subject : k => u.name }
}

output "group_name" {
  description = "Group carrying ReadOnlyAccess plus the secret-read deny."
  value       = aws_iam_group.this.name
}

output "deny_policy_arn" {
  description = "ARN of the explicit-Deny managed policy."
  value       = aws_iam_policy.deny_secret_reads.arn
}

output "cost_and_metrics_policy_arn" {
  description = "ARN of the Cost Explorer / CloudWatch metrics / gardenio log-groups read policy."
  value       = aws_iam_policy.cost_and_metrics_read.arn
}
