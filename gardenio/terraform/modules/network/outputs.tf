# Outputs for modules/network.
#
# Shape tracks canonical-prod-module-io-contract.md §5.2 `networking`.

output "vpc_id" {
  description = "VPC ID."
  value       = aws_vpc.this.id
}

output "vpc_cidr" {
  description = "VPC primary CIDR block."
  value       = aws_vpc.this.cidr_block
}

output "public_subnet_ids" {
  description = "Public subnet IDs (one per AZ)."
  value       = aws_subnet.public[*].id
}

output "public_extra_subnet_ids" {
  description = "Extra public subnet IDs, keyed by availability zone."
  value       = { for az, s in aws_subnet.public_extra : az => s.id }
}

output "private_subnet_ids" {
  description = "Private-app subnet IDs (one per AZ). EKS node tier."
  value       = aws_subnet.private_app[*].id
}

output "isolated_subnet_ids" {
  description = "Private-data subnet IDs (no NAT egress). RDS / stateful tier."
  value       = aws_subnet.private_data[*].id
}

output "nat_gateway_ids" {
  description = "Managed NAT gateway IDs (empty unless nat_mode in {single, per-az})."
  value       = aws_nat_gateway.this[*].id
}

# NAT-instance public IPs are auto-assigned per launch and not known to terraform
# (the boxes are ASG-managed, not aws_instance resources), so this output carries
# only the managed-NAT EIPs. For nat_mode=instance it is an empty list — query the
# live boxes by the gardenio:nat-role=instance tag, or read nat_instance_asg_names.
output "nat_gateway_ips" {
  description = "Public IPv4 EIPs of the managed-NAT egress path (single/per-az). Empty for nat_mode=instance (auto-assigned IPs are not terraform-known)."
  value       = aws_eip.nat[*].public_ip
}

output "nat_instance_asg_names" {
  description = "NAT-instance Auto Scaling Group names (empty unless nat_mode=instance). Each ASG holds exactly one self-healing NAT box."
  value       = aws_autoscaling_group.nat_instance[*].name
}

output "nat_instance_security_group_id" {
  description = "Security group ID shared by the NAT instance(s) (null unless nat_mode=instance)."
  value       = length(aws_security_group.nat_instance) > 0 ? aws_security_group.nat_instance[0].id : null
}

output "nat_mode" {
  description = "Resolved NAT topology in effect (single | per-az | instance | none)."
  value       = var.nat_mode
}

output "route_table_ids" {
  description = "Route table IDs grouped by tier."
  value = {
    public   = [aws_route_table.public.id]
    private  = aws_route_table.private_app[*].id
    isolated = aws_route_table.private_data[*].id
  }
}

output "availability_zones" {
  description = "Availability zones in use."
  value       = local.azs
}

output "security_group_ids" {
  description = "Module-managed security group IDs."
  value = {
    default = length(aws_security_group.interface_endpoints) > 0 ? aws_security_group.interface_endpoints[0].id : null
  }
}

output "vpc_endpoint_ids" {
  description = "VPC endpoint IDs keyed by service short name."
  value = merge(
    { for k, ep in aws_vpc_endpoint.gateway : k => ep.id },
    { for k, ep in aws_vpc_endpoint.interface : k => ep.id },
  )
}

output "component_tag" {
  description = "Module identifier appended to common_tags by callers."
  value       = "network"
}

output "edge_subnet_ids" {
  description = "Edge subnet IDs (IGW route; NAT box, public edge, CI pool), keyed by availability zone. Empty unless edge_subnets is set."
  value       = { for az, s in aws_subnet.edge : az => s.id }
}
