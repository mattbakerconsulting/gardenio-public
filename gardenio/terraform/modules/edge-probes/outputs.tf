output "function_name" {
  value       = aws_lambda_function.edge_probe.function_name
  description = "Lambda function name (aws lambda invoke --function-name ...)."
}

output "schedule_rule_name" {
  value       = aws_cloudwatch_event_rule.edge_probe.name
  description = "EventBridge rule name (disable it for the dead-man drill)."
}

output "topic_arn" {
  value       = aws_sns_topic.edge_probe.arn
  description = "SNS topic every alarm and the prober publish to."
}

output "alarm_names" {
  value = [
    aws_cloudwatch_metric_alarm.failures.alarm_name,
    aws_cloudwatch_metric_alarm.broken.alarm_name,
    aws_cloudwatch_metric_alarm.heartbeat.alarm_name,
    aws_cloudwatch_metric_alarm.lambda_errors.alarm_name,
  ]
  description = "CloudWatch alarm names (the 48 h gate reads their history)."
}

output "status_page_url" {
  value       = "https://${aws_s3_bucket.status.bucket_regional_domain_name}/index.html"
  description = "The off-cluster status page (uptime.tf)."
}

output "uptime_metrics_url" {
  value       = "https://${aws_s3_bucket.status.bucket_regional_domain_name}/metrics"
  description = "Prometheus text the in-cluster vmagent scrapes: set vmagent.uptimeProbeScrape.url to this."
}

output "uptime_function_name" {
  value       = aws_lambda_function.uptime_probe.function_name
  description = "Availability prober Lambda (aws lambda invoke --function-name ...)."
}

output "uptime_alarm_names" {
  value = concat(
    [for a in aws_cloudwatch_metric_alarm.uptime_down : a.alarm_name],
    [aws_cloudwatch_metric_alarm.uptime_heartbeat.alarm_name, aws_cloudwatch_metric_alarm.uptime_lambda_errors.alarm_name],
  )
  description = "The availability prober's alarms: one down-alarm per target, the dead-man and Lambda errors."
}
