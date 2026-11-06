# Bootstrap outputs.

output "state_bucket_name" {
  description = "S3 bucket name for the Terraform state backend."
  value       = aws_s3_bucket.state.bucket
}

output "state_bucket_arn" {
  description = "S3 bucket ARN for the Terraform state backend."
  value       = aws_s3_bucket.state.arn
}

output "lock_table_name" {
  description = "DynamoDB lock table name."
  value       = aws_dynamodb_table.lock.name
}

output "lock_table_arn" {
  description = "DynamoDB lock table ARN."
  value       = aws_dynamodb_table.lock.arn
}
