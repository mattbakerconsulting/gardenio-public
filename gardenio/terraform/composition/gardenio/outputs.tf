output "vpc_id" { value = module.network.vpc_id }
output "public_subnet_ids" { value = module.network.public_subnet_ids }
output "finops_sns_topic" { value = try(module.finops.sns_topic_arn, null) }
output "clonk_zone_id" { value = data.aws_route53_zone.clonk.zone_id }
output "k3s_api" { value = module.k3s.api_endpoint }
output "k3s_kubeconfig_ssm" { value = module.k3s.kubeconfig_ssm_parameter }
output "k3s_server_asg" { value = module.k3s.server_asg_name }
output "hq_instance_id" { value = module.hq.instance_id }
output "hq_public_ip" { value = module.hq.public_ip }
output "hq_security_group_id" { value = module.hq.security_group_id }
output "session_image_repo" { value = aws_ecr_repository.session.repository_url }
output "gardenio_image_repos" { value = { for k, r in aws_ecr_repository.gardenio : k => r.repository_url } }
output "ci_ecr_role_arns" {
  value = { release = aws_iam_role.ci_ecr_release.arn }
}
output "infra_aws_readonly_users" { value = module.infra_aws_readonly.user_names }
output "ses_lands_identity_arn" { value = module.ses_lands.identity_arn }
output "ses_lands_smtp_user" { value = module.ses_lands.smtp_user_name }
output "ses_lands_smtp_endpoint" { value = module.ses_lands.smtp_endpoint }
