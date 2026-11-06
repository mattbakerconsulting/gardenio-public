# Outputs for modules/ecr-pull-through-cache.

output "enabled" {
  description = "Whether the pull-through cache is provisioned."
  value       = local.enabled
}

output "node_policy_arn" {
  description = "IAM policy ARN granting nodes first-pull cache-populate permissions. null when disabled or when create_node_policy=false (the cluster module carries the grant inline). Attach to the node role in the composition."
  value       = local.node_policy ? aws_iam_policy.node_pull_through[0].arn : null
}

output "credential_secret_arns" {
  description = "Map of upstream logical name -> Secrets Manager ARN for its (out-of-band-populated) credential. Empty when disabled."
  value       = { for k, s in aws_secretsmanager_secret.upstream : k => s.arn }
}

output "cache_repository_prefixes" {
  description = "Map of upstream logical name -> ECR repository prefix the cache populates."
  value       = { for k, u in local.upstreams : k => u.ecr_repository_prefix }
}

output "pull_through_cache_rules" {
  description = "Map of upstream logical name -> { prefix, upstream } for the provisioned cache rules."
  value = {
    for k, r in aws_ecr_pull_through_cache_rule.this : k => {
      ecr_repository_prefix = r.ecr_repository_prefix
      upstream_registry_url = r.upstream_registry_url
    }
  }
}

output "repository_creation_template_prefixes" {
  description = "Map of upstream logical name -> prefix for which a repository creation template is registered (so auto-created cache repos inherit the lifecycle policy). Empty when untagged_expiry_days = 0."
  value       = { for k, t in aws_ecr_repository_creation_template.this : k => t.prefix }
}
