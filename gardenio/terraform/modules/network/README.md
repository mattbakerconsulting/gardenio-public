# modules/network

VPC + subnets + NAT + route tables + IGW + VPC endpoints + default SG +
flow logs. Owns AZ slicing math.

## Egress topology (`nat_mode`)

The private-app tier reaches the internet via one of four substrates:

| `nat_mode` | Substrate | EIPs | HA / self-heal |
|---|---|---|---|
| `single` | one managed NAT Gateway, all AZs route to it | 1 | AWS-managed |
| `per-az` | one managed NAT Gateway per AZ | `az_count` | AWS-managed, per-AZ |
| `instance` | **fck-nat / NAT instance** on `t4g.nano`, auto-assigned public IP | **0** | **ASG-of-1 + route-reclaim** |
| `none` | no default route (breaks the ~78% IPv4-only public-HTTPS egress) | 0 | n/a |

`nat_mode=instance` (ADR `docs/research/canonical-prod-nat-elimination.md`) is the
EIP-free substrate: a small ARM box with `source_dest_check=false` running iptables
MASQUERADE, behind an **Auto Scaling Group of size 1** so a lost box (Spot reclaim,
fault) relaunches in ~1-3 min. Because each box gets a *new* auto-assigned public IP
and a *new* ENI, the box **reclaims the `0.0.0.0/0` route itself at boot** via the EC2
API (the canonical fck-nat `update_route_tables` pattern) rather than terraform
pinning a route at a doomed ENI. The private-app route tables therefore
`ignore_changes = [route]` so terraform leaves that runtime-owned route alone.
`nat_instance_per_az=true` runs one box per AZ (each owns only its AZ's route table —
same-AZ wiring); `false` (default) runs one shared box owning all of them. See
`nat_instance.tf`.

With `edge_subnets` set, the box lives in an edge subnet (small, IGW-routed) rather
than `public[i]`: per-AZ, its own AZ's edge subnet (every NAT AZ needs one); shared,
the first edge AZ. That keeps the box out of any subnet it routes once
`nat_routed_public_azs` flips a public subnet onto its private-app route table (an
in-place association change; the subnet also stops handing out public IPs on launch).
Flip one AZ at a time. The box has SSM (`AmazonSSMManagedInstanceCore`), and the ASG
publishes `GroupInServiceInstances` for the unhealthy alarm.

## Inputs

See `variables.tf`. Names track `canonical-prod-module-io-contract.md` §3.2.

- `env_name` — environment short name (kebab)
- `region` — AWS region
- `vpc_cidr` — top-level CIDR (`/16`–`/20`)
- `additional_cidr_blocks` — optional extra CIDRs
- `az_count` — number of AZs to span
- `availability_zones` — explicit AZ override (defaults to first `az_count` from data source)
- `nat_mode` — `single | per-az | instance | none`
- `nat_instance_per_az` — `instance` mode: one box per AZ (true) vs one shared box (false)
- `nat_instance_type` / `nat_instance_arch` — NAT box size + CPU arch (default `t4g.nano` / `arm64`). Size UP (`t4g.small`/`medium`/`large`) for a higher sustained NETWORK baseline: a small burstable box drains its network burst-credit budget under bulk egress and wedges all egress at baseline for hours (the 2026-07-12 outage, ka-0000007968)
- `nat_instance_cpu_credits` — `unlimited` (default) or `standard` CPU-credit mode for a burstable box; emitted only for T-family types. `unlimited` removes the CPU-credit cliff (network burst credits are a separate, non-configurable budget — size up for those)
- `nat_instance_use_spot` — run the NAT box(es) on Spot (safe under ASG-of-1 self-heal)
- `nat_instance_alarm_sns_topic_arns` — SNS topics paged when a NAT-instance ASG has no in-service box
- `extra_public_subnets` — extra public subnets outside the per-AZ set (AZ → `cidrsubnet` index)
- `edge_subnets` — AZ → CIDR of IGW-routed edge subnets (NAT box, public edge, CI pool); default `{}`
- `nat_routed_public_azs` — AZs whose public subnet egresses via the NAT (an extra public AZ uses `private_app[0]`); default `[]`, needs `nat_mode != none`
- `vpc_endpoint_services` — widened default set (S3 gateway + interface
  endpoints for the AWS control-plane data path: STS, Secrets Manager, Logs,
  KMS, EKS, EC2/SSM, ECR, ELB, ASG, SQS). Trim in cost-pressed compositions.
- `enable_private_dns_for_endpoints`
- `common_tags` — caller-supplied tag baseline

## Outputs

- `vpc_id`
- `vpc_cidr`
- `public_subnet_ids` (list)
- `public_extra_subnet_ids` (map, AZ → id)
- `edge_subnet_ids` (map, AZ → id) — empty unless `edge_subnets` is set
- `private_subnet_ids` (list)
- `isolated_subnet_ids` (list)
- `nat_gateway_ids` (list) — managed NAT GW IDs (empty unless `single`/`per-az`)
- `nat_gateway_ips` (list) — managed-NAT EIPs (empty for `instance`: auto-IPs are not terraform-known)
- `nat_instance_asg_names` (list) — NAT-instance ASG names (empty unless `instance`)
- `nat_instance_security_group_id` (string) — NAT-instance SG (null unless `instance`)
- `nat_mode` (string) — resolved topology in effect
- `route_table_ids` (object)
- `availability_zones` (list)
- `security_group_ids` (object)
- `vpc_endpoint_ids` (map)
- `component_tag` (string) — `"network"`

## Out of scope

- Cross-account VPC peering — lives in caller.
- Route53 zone creation — module `data/` or caller owns DNS.
- VPN / Direct Connect — env-specific overlay.
