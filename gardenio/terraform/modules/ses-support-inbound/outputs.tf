output "recipient" {
  description = "The one address received."
  value       = local.recipient
}

output "mail_bucket" {
  description = "Bucket holding the raw received mail (expires after retention_days)."
  value       = aws_s3_bucket.mail.bucket
}

output "function_name" {
  description = "The forwarder Lambda."
  value       = aws_lambda_function.forward.function_name
}

output "rule_set_name" {
  description = "The active receipt rule set."
  value       = aws_ses_receipt_rule_set.this.rule_set_name
}
