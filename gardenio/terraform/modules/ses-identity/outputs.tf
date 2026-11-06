output "identity_arn" {
  description = "ARN of the SES domain identity (the Resource of the SMTP user's policy)."
  value       = aws_sesv2_email_identity.this.arn
}

output "mail_from_domain" {
  description = "Custom MAIL FROM domain."
  value       = aws_sesv2_email_identity_mail_from_attributes.this.mail_from_domain
}

output "smtp_user_name" {
  description = "IAM user to mint the SMTP access key for, out of band (README)."
  value       = aws_iam_user.smtp.name
}

output "smtp_endpoint" {
  description = "SES SMTP host for this region (port 587, STARTTLS)."
  value       = "email-smtp.${local.region}.amazonaws.com"
}
