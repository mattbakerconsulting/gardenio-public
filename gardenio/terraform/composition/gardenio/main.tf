# Minimal gardenio: network + remote state + budget alert. Public subnets,
# no NAT, gateway endpoints only. The cluster will be k3s on spot instances
# (EKS was applied and destroyed 2026-09-16 -- see plans/self-hosted-ci-runner.md).

locals {
  common_tags = {
    "gardenio:env" = var.env_name
    "managed-by"   = "terraform"
    "repo"         = "joyousaversion/gardenio"
  }
}

module "network" {
  source   = "../../modules/network"
  env_name = var.env_name
  region   = var.region
  vpc_cidr = "10.90.0.0/16"
  az_count = 2
  # A third zone for the k3s servers alone, so the three etcd members split
  # 1/1/1 and one zone's spot event cannot take quorum (2026-10-01).
  extra_public_subnets = { "us-east-1c" = 6 } # 10.90.96.0/20
  # Cost plan 2026-10-21 C6: per-AZ NAT instances in new edge subnets. Sized
  # from C1 flow logs (gd-hUa6jYrfk1): H = 19.1 GB/h non-CI -> t4g.small;
  # B = 8.0 GB/h from 1b/1c >> 0.7 -> one box per AZ. Nothing routes through
  # them until C9-C11 move nodes off public IPs.
  # Rollback: nat_mode = "none", edge_subnets = {} (the subnets are empty).
  nat_mode          = "instance"
  nat_instance_type = "t4g.small"
  # Shape A (owner 2026-10-23): ONE NAT box for the VPC; 1b/1c egress crosses
  # into 1a (cheaper than a second box at our volumes).
  nat_instance_per_az = false
  # C10 (gd-QBmUxolO0f): old public subnets egress via the NAT one AZ at a
  # time, 1c first (one etcd member; quorum survives on 1a + 1b). Rollback:
  # drop the AZ from this list and apply; the IGW route returns in seconds.
  nat_routed_public_azs             = ["us-east-1c", "us-east-1a", "us-east-1b"]
  edge_subnets                      = { "us-east-1a" = "10.90.160.0/24", "us-east-1b" = "10.90.161.0/24" }
  nat_instance_alarm_sns_topic_arns = [module.edge_probes.topic_arn]
  vpc_endpoint_services             = ["s3", "dynamodb"] # gateway endpoints only (free)
  common_tags                       = local.common_tags
}

module "finops" {
  source                    = "../../modules/finops"
  env_name                  = var.env_name
  common_tags               = local.common_tags
  account_budget_usd        = var.monthly_budget_usd
  account_budget_thresholds = [50, 80, 100]
  component_budgets         = {}
  agent_spot_budget_usd     = 0
  data_transfer_budget_usd  = 0
  # AWS Budgets emails these addresses DIRECTLY, with no confirmation step —
  # unlike an SNS email subscription, which sits in PendingConfirmation until
  # the recipient clicks a link and delivers nothing until they do. That is
  # why budget alerts have never arrived: both SNS email subscriptions on the
  # finops topic are still unconfirmed, so the only confirmed subscriber is an
  # SQS queue nothing drains. This list is the reliable path.
  budget_emails               = ["fimbaz@gmail.com"]
  email_subscriptions_enabled = false
  # The account already has AWS's Default-Services-Monitor (one dimensional
  # SERVICE monitor is the per-account limit); it covers everything, so a
  # terraform-managed duplicate cannot be created and is not needed.
  anomaly_detection_enabled = false
}

data "aws_route53_zone" "clonk" {
  name = "clonk.tv."
}

# ── k3s on spot ───────────────────────────────────────────────────────
data "aws_ssm_parameter" "ubuntu_ami" {
  name = "/aws/service/canonical/ubuntu/server/24.04/stable/current/amd64/hvm/ebs-gp3/ami-id"
}

module "k3s" {
  source = "../../modules/k3s"
  # C11 (gd-v4eUJvNMLV): no public IPs on new servers or on-demand agents; the
  # old public subnets all route through the NAT since C10 (10-23).
  server_public_ip  = false
  agent_public_ip   = false
  env_name          = var.env_name
  vpc_id            = module.network.vpc_id
  vpc_cidr          = module.network.vpc_cidr
  subnet_ids        = module.network.public_subnet_ids
  server_subnet_ids = concat(module.network.public_subnet_ids, values(module.network.public_extra_subnet_ids))
  # Cost plan 2026-10-21 C8: the public-edge node lives in edge subnet 1a (its EIP
  # re-associates; rollback = "" puts it back in public 1a).
  edge_subnet_id = module.network.edge_subnet_ids["us-east-1a"]
  # C7: the CI pool launches in the edge subnets (public IP, edge route table).
  ci_subnet_ids = values(module.network.edge_subnet_ids)
  # C9: the spot agent pool launches in private-app (no public IP) behind the
  # per-AZ NAT boxes of C6.
  agent_spot_subnet_ids = module.network.private_subnet_ids
  zone_id               = data.aws_route53_zone.clonk.zone_id
  api_fqdn              = "k3s.clonk.tv"
  join_fqdn             = "k3s-join.clonk.tv"
  admin_cidrs           = var.admin_cidrs

  # Dedicated on-demand edge node + EIP + own SG (gd-dCvIWuT6t6, operator ruling
  # 2026-09-23; ~$19/mo, t3a.small + EIP). It hosts the public-edge nginx below
  # and, while ziti_public_router_enabled, router-public-1 at ziti.clonk.tv.
  # The two ziti flags switch ziti off WITHOUT touching the node, EIP, 443 rule
  # or the public_edge records. Ziti is OFF (gd-xNMLrsgajy): false destroys
  # world 3022 on the edge SG, the ziti.clonk.tv record and VPC 3022, nothing
  # else. To turn ziti back on, set both true, apply targeted, and see
  # gardenio/helm/ziti/README.md "Turning ziti back on".
  edge_node_enabled          = true
  ziti_public_router_enabled = false
  ziti_router_vpc_enabled    = false

  # Public HTTPS for the ClaimLayer demo and the Keycloak sign-in it needs
  # (decision gd-GbIgemW3dt "public", escalation gd-NoOR0riqwi). Served from
  # the edge node's own SG + EIP so web_ingress_cidrs below stays the GitHub
  # hooks allowlist. See modules/k3s ziti_edge_web.
  # api.clonk.tv: the daemon API for kd and gb behind a bearer-JWT check (plan
  # ziti-to-keycloak S6, gd-FQJHYGAnjB).
  # dashboard.clonk.tv: Headlamp (plan ziti-to-keycloak S5, gd-duCuVLz5cU).
  # grafana.clonk.tv: the first app behind the edge sign-in (plan
  # ziti-to-keycloak S3, gd-iS4Uxv9tfN); the owner approves each host.
  # gardenio.clonk.tv (viewer): S4, gd-UqhR5d6qPt. mux.clonk.tv (coopmux
  # terminal): S4b, gd-0e6aSFhxM5, published on the owner's answer to
  # gd-jfXRKAn6Ez. Enrolled ziti devices intercept both names.
  # api.clonk.tv (daemon API for kd/gb): S6, gd-FQJHYGAnjB.
  # hooks.clonk.tv (GitHub App webhooks -> trigger-engine): moved off the
  # servers' public IPs on the cost plan (C4, gd-040vrRf2II). The edge nginx
  # allowlists GitHub's hooks ranges and POST; trigger-engine checks the HMAC.
  # runners.clonk.tv (public runner listener, /runner invites): plan
  # gd-fwFefkDP4H Phase 3, PR #1390, gd-T4eL6P5OtC.
  # clonk.tv (the splash, the landing Service) and www.clonk.tv (301 to it):
  # the public front door, gd-gQ8hZLd4AQ. The apex had no A record before
  # (checked 2026-10-29), so this creates it rather than colliding.
  public_edge_fqdns = ["claimlayer.clonk.tv", "auth.clonk.tv", "grafana.clonk.tv", "dashboard.clonk.tv", "gardenio.clonk.tv", "mux.clonk.tv", "api.clonk.tv", "hooks.clonk.tv", "runners.clonk.tv", "clonk.tv", "www.clonk.tv"]

  # Closed to the internet 2026-09-19. Nothing needed it: the only IngressRoute
  # is an http->https redirect, ACME is DNS-01 (no HTTP-01 challenge to answer),
  # and every real path is the ziti overlay. Left open it is one IngressRoute
  # away from publishing viewer-authproxy -- which has no authentication of its
  # own and stamps the operator's identity on every request. Reopen in the same
  # change that lands an authenticated public route, not before.
  # VPC only. The operator's /32 is deliberately NOT here: the laptop reaches
  # these services over ziti, so an allow-listed public IP would buy nothing
  # except a hole that also defeats any attempt to verify the port is shut --
  # you cannot test "closed to the internet" from an address you exempted.
  #
  # REOPENED 2026-10-11 for GitHub webhooks only (decision gd-Bz9rkZg5Q0,
  # escalation gd-fyCexuAP9d): GitHub's IPv4 `hooks` ranges from
  # `gh api meta --jq .hooks` (2026-10-11; GitHub changes these, re-check on
  # failed deliveries). The allowlist lives HERE, not in Traefik: ServiceLB
  # SNATs (externalTrafficPolicy: Cluster), so Traefik sees 10.42.x and an
  # ipWhiteList could never match. The rule is shared across 80 and 443, so 80
  # opens to these ranges too; it only answers the http->https redirect. The
  # one public route is trigger-engine Host(hooks.clonk.tv) &&
  # PathPrefix(/webhooks/), HMAC-verified, TLS-only. IPv6 ranges are omitted:
  # the nodes have no IPv6.
  #
  # CLOSED AGAIN on the cost plan (2026-10-21, C4 gd-040vrRf2II): hooks.clonk.tv
  # moved to the public edge (public_edge_fqdns above), whose nginx sees real
  # client IPs and carries the GitHub ranges itself (public-edge.yaml). The
  # CNAME to the servers is gone (hooks_fqdn = ""), so nothing public needs
  # the servers' 80/443. Rollback: restore the 7 ranges and hooks_fqdn.
  web_ingress_cidrs = [module.network.vpc_cidr]
  hooks_fqdn        = ""
  k3s_version       = var.k3s_version
  ami_id            = data.aws_ssm_parameter.ubuntu_ami.value
  common_tags       = local.common_tags
  # beads Dolt backups (helm/beads) land next to the HQ host's, under k3s-dolt/.
  backup_bucket_arn = module.hq.backup_bucket_arn
  # One runner pod per node (3 vCPU / 12 Gi on 4-vCPU spot), so this is the real
  # ceiling on CI concurrency -- not the per-scale-set maxRunners. At the module
  # default of 2, gates across four mirrors serialised and a second job on the
  # same mirror queued ~40 min behind the first. The pool is spot and scales to
  # zero, so unused headroom costs nothing.
  # 6 -> 8 on 2026-09-20 (operator-approved envelope, number delegated to
  # gd-uGKVgBc63f). One runner pod per node -- it requests 3 of a 4 vCPU node's
  # CPU, so a second cannot fit beside it -- which makes total maxRunners the
  # node demand: 12 after gardenio goes 2 -> 4 (gardenio 4, gascity-hq 4,
  # beads-hq 2, packs-hq 2). 8 lets the two ACTIVE gate sets, gardenio and
  # gascity-hq, run concurrently; 12 would let all four burst at once, which
  # has not been observed and costs reliability, since these are spot nodes and
  # a reclaim mid-job fails the job. Free at rest: min_size 0, scales to zero.
  #
  # 8 -> 11 on 2026-09-22. This is the binding constraint on CI throughput, and
  # it was measured rather than argued: over a 4h40m window the gardenio heavy
  # scale set ran pinned at its cap 65% of the time with 56 jobs queued on
  # GitHub's side, while gardenio-k3s-medium averaged 1.53 of 6 and
  # gardenio-k3s-light 0.64 of 10. Because one heavy runner needs a whole node,
  # the heavy tier physically cannot exceed this number, so every cheaper fix
  # (tiering jobs down, node-local build caches) is a workaround for it.
  #
  # PAIRED with gardenio/helm/ci/arc-runners-values.yaml maxRunners 7 -> 10, and
  # neither half works alone -- that mistake has now been made in both
  # directions. Raising maxRunners without nodes produced 5 runner pods with 3
  # PENDING within seconds on 2026-09-20; raising nodes without maxRunners buys
  # literally nothing, because ARC never asks for the 9th runner. 10 and not 11
  # preserves the invariant that file chose 7 for: one node stays completely
  # free at all times, so a gascity-hq/packs/beads-hq job -- heavy-shaped, so it
  # needs a whole node rather than a gap -- still starts immediately instead of
  # waiting out a ~40 minute image build.
  #
  # THE FURTHEST CEILING, per the lesson recorded in that same values file
  # (constraints stack maxRunners -> ci_max_nodes -> ACCOUNT SPOT QUOTA, and a
  # limit check is only as good as its furthest layer): L-34B43A08 is 64 spot
  # vCPU. SUPERSEDED 2026-09-23 -- the quota is 96 now, the increase requested at
  # the end of this block was GRANTED, and the "22 vCPU spare" arithmetic in the
  # next two paragraphs is stale. See the UPDATE block at ci_max_nodes below.
  # Current draw measured live from EC2, not from notes, and independently
  # confirmed by a second session:
  #   ci      8 x xlarge spot     = 32 vCPU
  #   agents  3 x t3.large spot   =  6   (+2 t3a.large on-demand, off-quota)
  #   servers 2 x t3.small spot   =  4   (+1 on-demand)
  #                               = 42 of 64, so 22 vCPU ≈ 5 xlarge spare.
  #
  # 11 and not 13 is about the AGENTS pool, which is the non-obvious half of
  # this and the reason the number is not just "spend the headroom". That pool
  # is ALSO pinned at its cap right now (min 0 / desired 5 / max 5, five
  # instances up, eight crew pods) and draws from this same quota. Taking ci to
  # 13 would consume all 22 spare vCPU and hard-wedge crew capacity at 5 with no
  # path to grow -- and CI is support while the crews are the work, the same
  # trade that forced gardenio's maxRunners back from 6 to 4 on 2026-09-20 when
  # an agents bump to 5 failed outright with MaxSpotInstanceCountExceeded and
  # the CI group sat in OutOfResource backoff. 11 takes 12 of the 22 and leaves
  # ~10 vCPU (≈5 more t3.large) for crews. Going past 11 needs a quota increase
  # requested FIRST, not discovered afterwards.
  #
  # CONFIRMED and then EXCEEDED, 2026-09-22. Replaying every launch/terminate
  # activity for all three pools (417 events, calibrated so the reconstruction
  # lands exactly on the live instance count) puts the true peak at 54 of 64 --
  # precisely the number above, reached 09-22 02:59 with all three pools at
  # their caps at once. The arithmetic holds. What it does NOT model is that a
  # draining instance keeps its spot allocation until it is fully terminated,
  # so a pool that sheds and rebuilds needs old + new simultaneously:
  #
  #   09-22 04:17:39-04:19:01  CA sheds 8 ci nodes, desired 11 -> 3
  #   09-22 04:19:13           2 launches issued, desired 3 -> 4
  #   09-22 04:19:21           third launch REFUSED, MaxSpotInstanceCountExceeded
  #   09-22 04:20:17-04:25:33  rebuilds to 10
  #
  # Settled draw at that instant was ~30 vCPU, less than half the quota, so
  # this is not the ceiling being spent -- it is the overlap being charged
  # twice. Five such shed-and-rebuild episodes happened that day (10->3->11,
  # 8->1->10, 9->4->11, 7->1->9, 10->5->11) and none in the preceding 4.5 days;
  # 09-22 also carried ~10x the usual run density, which is enough to explain
  # the burst shape on its own -- do not read a code cause into it without
  # re-measuring on a second busy day.
  #
  # The failure is SILENT. A refused launch fails no job and shows up in no
  # runner metric; the queued pod simply stays Pending until the next CA loop.
  # Nothing here alerts on it, which is why it took an activity-log replay to
  # find. L-34B43A08 64 -> 96 requested 2026-09-22 (8a78b754ad0b4512b3f0ca9f);
  # that removes this collision class for $0, because spend stays bounded by
  # ASG max_size, not by the quota. Raising the quota is NOT permission to
  # raise ci_max_nodes -- that decision still belongs to the paragraph above.
  #
  # Cost: roughly node-minute neutral for a fixed backlog rather than additive
  # -- the same jobs run either way, just more at once. Measured on this repo:
  # ten PRs take ~129 min at 4 runners and ~65 at 8, which is 516 vs 520
  # node-minutes. That neutrality holds ONLY while min_size/minRunners stay 0;
  # if anything ever pins warm runners, this cap becomes a standing bill instead
  # of a burst ceiling. Flagging plainly because the account's run rate is
  # already $212-238/mo against a $200 ceiling: this buys wall-clock at
  # approximately flat spend, it does not reduce spend, and the ceiling itself
  # is the operator's call.
  #
  # Sizing note for the next revision: six optional e2e jobs on
  # runs-on: gardenio-k3s are landing under plan gd-ZYb9OTNwfe (viewer-e2e and
  # coop-e2e both Playwright, coop-e2e a 25-minute long pole, plus
  # go-integration with a postgres service). None of them existed during the
  # window measured above, and a labelled PR or the nightly can fan out six at
  # once, so the heavy-tier peak this is sized against is about to rise.
  # UPDATE 2026-09-23: 11 -> 13. The quota premise above is stale, and the
  # agents-starvation argument that actually set 11 does not survive
  # re-measurement.
  #
  # L-34B43A08 is 96, not 64 -- the increase requested at the end of the block
  # above was granted 2026-09-22 (CASE_CLOSED). Re-measured from the ASGs
  # themselves rather than from a live instance count, because what competes for
  # quota is each pool's CEILING, not its current draw:
  #
  #   pool      max  onDemandBase   max SPOT draw
  #   agents      6             2   4 x t3.large  =  8 vCPU
  #   ci         11             0  11 x xlarge    = 44
  #   servers     3             1   2 x t3.small  =  4
  #                                               = 56 of 96
  #
  # All three pools were pinned at their caps simultaneously when this was
  # measured, so 56 is not a busy moment -- it is the most these ASGs can ever
  # ask for. 40 vCPU of quota is unreachable by any scaling policy.
  #
  # The agents row is LIVE state (max 6). This composition still says 5, so an
  # untargeted apply would shrink that pool -- drift someone else owns, left
  # alone here. 6 is the larger of the two, so 8 vCPU is the upper bound and the
  # conclusion below holds either way; at 5 it would be 6 vCPU and hold harder.
  #
  # That retires the specific objection recorded above ("taking ci to 13 would
  # hard-wedge crew capacity at 5"). Crew capacity is bounded by the agents
  # ASG's own max_size, and its spot half is 8 vCPU whatever ci does; the pools
  # only compete once the SUM of ceilings crosses the quota, which at ci 13 is
  # 64 of 96. That leaves 32 vCPU -- MORE crew growth room than the ~10 the
  # paragraph above settled for, not less.
  #
  # Attribution, because ci_max_nodes has been a measured dead end before and
  # must not be reopened on a queue number alone. Named pods, not aggregates:
  # gardenio-k3s-medium-9mcpr-runner-{qwglp,lnbvr,krmbs,gt5gt,wzsqn} each failed
  # FailedScheduling with "Insufficient cpu" against EVERY schedulable ci node
  # while desired == max_size == 11. Live free CPU across the pool read 15940m,
  # but 11970m of that sat on three nodes the spot-interruption handler had
  # cordoned, leaving 3970m actually schedulable against a 1900m heavy request.
  # Reading the unqualified 15940m would have said "capacity is fine" and been
  # exactly wrong -- subtract cordoned nodes before calling a pool idle.
  #
  # PAIRED, per the rule this block already states in both directions: medium
  # maxRunners went 6 -> 8 earlier today (1e7c703c) and filled instantly, so the
  # runner half is already spent and this is the node half it needs. Heavy
  # maxRunners stays 10 -- it is not the half that moved, and changing both at
  # once would make the next measurement uninterpretable.
  #
  # 13 and not higher: the shed-and-rebuild overlap documented above charges a
  # draining instance and its replacement at the same time. At 13 the settled
  # ceiling is 64, so even the worst episode on record (8 ci nodes shed and
  # rebuilt at once, ~32 vCPU of overlap) lands at 96 exactly rather than over
  # it -- tight, and stated plainly rather than rounded away. 16 would only be
  # safe against a 5-node overlap, and nothing has measured that.
  #
  # 2026-10-21 COST CAP, owner ("stop the bleeding"): 13 -> 6. October ran to a
  # ~$1,100 forecast against a $200 ceiling; CI spot was ~$16/day of it at 11-13
  # busy nodes. This is a budget cap, not a capacity ruling: CI queues longer by
  # design until the runners move off AWS (Hetzner/OVH dedicated, under
  # consideration). Raise only with the owner.
  #
  # 2026-10-28 02:1xZ TEMPORARY, owner ("can we enlarge for 24h?"): 6 -> 12 for a
  # merge burst (~23 desk merges approved at once). REVERT to 6 at or after
  # 2026-10-29 02:15Z. 12 stays under the 13 ceiling argued above.
  # REVERTED 2026-10-29 ~04:30Z: back to the 6 budget cap.
  #
  # 2026-10-31 ~19:45Z TEMPORARY, owner ("Do it" on decision gd-rAhfwPGC1T,
  # option a): 6 -> 12 for 24 h. PRs were piling up behind CI, not reviewers:
  # 127 runs queued/running, all 6 nodes full, 0 green PRs waiting at the desk.
  # REVERT to 6 at or after 2026-11-01 19:45Z.
  # REVERTED 2026-11-01 ~18:10Z, early on the owner's call ("should we scale
  # down ci runners now?"), with ASG desired already at 6.
  #
  # 2026-11-03 ~22:00Z TEMPORARY, owner ("Do all 3 yep"): 6 -> 10 for 24 h.
  # Merge desk blocked on CI, not reviewers: 29 runs queued, all 6 ci nodes
  # full, 7 runner pods Pending, 22 merge escalations waiting on ci/checks.
  # REVERT to 6 at or after 2026-11-04 22:00Z.
  # REVERTED 2026-11-04 ~15:45Z, early on the owner's call ("our AWS bill is
  # like way high right now, can we fix that again plz"); ASG desired was 5.
  ci_max_nodes = 6
  # The HQ host drives session pods through the k8s runtime (client-go), so it
  # needs the API. Referenced by SG, not CIDR: its private IP moves when the
  # host self-stops and restarts.
  api_source_security_group_ids = [module.hq.security_group_id]

  # The in-k3s ziti controller keeps no PVC: an initContainer restores its
  # state from S3 on every start and a sidecar backs it up. So this prefix is
  # not just where backups land, it is what the controller BOOTS FROM.
  # Inherited by every pod that can reach IMDS, same as the k3s-dolt grant.
  #
  # k3s-postgres (gd-vekJWxnNGG): off-node backup for the beads-store
  # Postgres, whose PVC is node-local and does not survive node loss. Unlike
  # ziti-ctrl this is write-only from the cluster's side (pg_dumpall ->
  # S3, hourly) — nothing in-cluster boots from it.
  extra_backup_prefixes = ["ziti-ctrl", "k3s-postgres"]

  # The gardenio platform pool (taint gardenio.agent=true:NoSchedule, label
  # gardenio.io/role=platform). Two nodes: on_demand_base_capacity=1 in the
  # module pins the first to on-demand for Postgres' node-local PV and for the
  # controller's required on-demand affinity; the second is spot. t3a.large
  # (2 vCPU / 8 GiB) rather than the module's t3a.medium default because the
  # chart's four control-plane pods request ~3.1 GiB between them and an
  # agent pod defaults to 2 CPU / 8 GiB before trimming.
  # NOTE: the module pins desired_capacity behind lifecycle { ignore_changes },
  # so agent_count only seeds a NEW asg and sets max_size = max(agent_count, 3).
  # On a live asg the capacity is operator-set:
  #   aws autoscaling set-desired-capacity --auto-scaling-group-name \
  #     gardenio-k3s-agents --desired-capacity N
  # 2 -> 4 on 2026-09-19 (user-approved). CPU, not memory, is the binding
  # constraint: five gardeners at max_concurrent=1 plus the crews they spawn is
  # ~6500m worst case against 6000m allocatable on three t3.large, and the
  # scheduler was reporting "Insufficient cpu" with pods Pending while the
  # autoscaler declined ("pod didn't trigger scale-up") because the pool was at
  # its ceiling.
  #
  # max_size is max(agent_count, 3), so this raises the CEILING to exactly the
  # four nodes that were approved. That ceiling is deliberately not set higher:
  # it is what forces a human conversation before a pool that autonomously
  # spawns its own workload grows again, and it has now done that twice.
  # desired_capacity stays operator-set (see the ignore_changes note below).
  # 4 -> 5 raises the CEILING only (max_size = max(agent_count, 3)); desired is
  # behind ignore_changes and stays operator-set.
  # 5 -> 7 on 2026-10-10 (owner: "might need to scale up a bit to handle peak
  # load"). At the old ceiling, four crew pods (500m each) sat Pending with
  # "Insufficient cpu" on every platform node, and the autoscaler declined them.
  # Live ASG set to max 7 / desired 7 by CLI first; this keeps an apply from
  # dropping max_size back to 5.
  # 7 -> 8 the same day, for the third on-demand node below (decision
  # gd-7GdfTU7oPa).
  #
  # 2026-10-11 (gd-AEm6o8aWkF): the pool is now TWO ASGs. agent_count is the
  # total ceiling; the on-demand ASG is fixed at agent_on_demand_base and the
  # spot ASG (cluster-autoscaler managed, min agent_spot_min) has max
  # agent_count - agent_on_demand_base = 5. Nobody hand-sets desired on the
  # spot group any more; a Pending crew scales it up, idle nodes scale down
  # after 10 min below 30% utilization. Measured before the split: 8 pinned
  # nodes, 6 of them at 6-17% CPU, ~$4.6/day.
  # 8 -> 10 on 2026-10-21 (gd-2qjxTVMBkC): spot max 5 -> 7. On 10-20 a crew
  # burst filled all five spot nodes and starved infra pods (viewer-authproxy,
  # ziti-host, the backup), so the ceiling was raised by CLI at 21:17Z; this
  # records it. It is a ceiling only: the autoscaler adds a node (~$0.75/day
  # for a spot t3a.large) only while a crew is Pending, and removes it after
  # 10 idle minutes. The tiered PriorityClasses (#750) are the real fix for
  # infra starvation; this keeps crews from queueing behind it.
  agent_count = 10

  # 1 -> 2 on 2026-09-19 (operator-approved, ~+$42/mo). Gardeners require an
  # on-demand node via podmanager capacityAffinity, so ONE on-demand node was
  # the real cap on concurrent gardeners (~2 slots), regardless of how many spot
  # nodes the pool had. The second on-demand node carries no platform baseline,
  # so it should add ~3 gardener slots rather than 2.
  # 2 -> 3 on 2026-10-10 (owner decision gd-7GdfTU7oPa, ~+$55/mo). A plan
  # gardener sat Pending on "Insufficient cpu" after two spot nodes were added:
  # spot cannot take on-demand-only roles, and one of the two on-demand nodes
  # is a t3a.medium that platform services keep 89% CPU-requested. Raised
  # together with desired 7 -> 8, so the ASG launched an on-demand node
  # rather than replacing a spot node with live crews on it.
  # 3 -> 2 on 2026-10-23 (owner, shape A): 17-224 drained and retired by hand;
  # the two left carry postgres+kbeads (30-213) and MinIO+Grafana+slack-bridge
  # (2-170). Gardeners still need an on-demand node: fewer concurrent slots.
  agent_on_demand_base = 2
  # One warm spot node (~$22/mo) so the first crew of the day does not wait
  # for a launch. Set to 0 if the account needs the last dollar.
  # Shape A (owner 2026-10-23): no warm spot node; the first crew waits ~3 min.
  agent_spot_min       = 0
  agent_instance_types = ["t3a.large", "t3.large"]

  # Pull-through cache for ghcr (rule + credential secret live outside
  # terraform, created alongside the gardenio platform work). Lets the kubelet
  # pull gardenio images through ECR instead of ghcr, so the ghcr credential
  # leaves the cluster entirely.
  ecr_cache_repo_arns = ["arn:aws:ecr:us-east-1:360190250083:repository/ghcr/*"]

  # CI pushes gardenio/* to ECR directly (plan 2026-10-21-aws-cost-cuts);
  # the node role pulls it, pull-only.
  ecr_read_repo_arns = ["arn:aws:ecr:us-east-1:360190250083:repository/gardenio/*"]

  # cert-manager issues the gardenio.clonk.tv cert by ACME DNS-01 against this
  # zone, using ambient (node-role) credentials so no long-lived key sits in a
  # cluster Secret. No public A record is created; the name resolves only
  # inside the ziti tunnel while the certificate stays publicly trusted.
  enable_acme_dns01 = true
}

# ── Standing edge prober (gd-xuxyIMbfCU) ─────────────────────────────────
# A Lambda outside the VPC probes the public edge every 15 minutes; alerts go
# to SNS email, a missing heartbeat catches a dead prober. Nothing goes public
# until this has been green for 48 h. Apply with
# `./scripts/tf-gardenio.sh apply -target=module.edge_probes`.
module "edge_probes" {
  source      = "../../modules/edge-probes"
  alert_email = var.edge_probe_alert_email
  zone_id     = data.aws_route53_zone.clonk.zone_id
  vpc_id      = module.network.vpc_id
  tags        = local.common_tags
}

# ── Gas City HQ host (supervisor + dogfood city), phase 1 ────────────────
# Placement A: public subnet, zero-ingress SG. Powers itself off when idle;
# see docs/hq-host.md and scripts/hq for start/stop/resize.
module "hq" {
  source        = "../../modules/hq"
  env_name      = var.env_name
  vpc_id        = module.network.vpc_id
  subnet_id     = module.network.public_subnet_ids[0] # us-east-1a
  ami_id        = data.aws_ssm_parameter.ubuntu_ami.value
  instance_type = var.hq_instance_type
  common_tags   = local.common_tags
}

# ── Read-only AWS for the infra tier (gd-83VraF4SeC, decision D3) ────────────
# ReadOnlyAccess plus an explicit Deny on every secret-returning read, as one scoped IAM
# user per subject. A static key, not IRSA (impossible on k3s) and not the node role (PR
# #270 removes that path); the full reasoning is in the module header. The key is minted
# out of band so it never enters state: see modules/infra-aws-readonly/README.md.
# Apply with `./scripts/tf-gardenio.sh apply -target=module.infra_aws_readonly`.
module "infra_aws_readonly" {
  source   = "../../modules/infra-aws-readonly"
  env_name = var.env_name
  # P6 appends "gardenio/gardenio-infra-admin" here.
  subjects = ["gardenio/gardenio-infra-reader"]
  tags     = local.common_tags
}

# ── SES sending domain lands.clonk.tv + Keycloak SMTP user (gd-uBGCzwjlIh) ──
# DKIM, MAIL FROM bounce.lands.clonk.tv and DMARC in the clonk.tv zone, plus the IAM
# user Keycloak's realm SMTP signs in with: ses:SendRawEmail on this identity, From
# lands@lands.clonk.tv only. No access key in state; it is minted by hand and the SMTP
# password derived from it (modules/ses-identity/README.md). SES production access is
# an owner support request, not terraform. Apply with
# `./scripts/tf-gardenio.sh apply -target=module.ses_lands`.
module "ses_lands" {
  source            = "../../modules/ses-identity"
  domain            = "lands.clonk.tv"
  zone_id           = data.aws_route53_zone.clonk.zone_id
  smtp_user_name    = "gardenio-keycloak-smtp"
  smtp_from_address = "lands@lands.clonk.tv"
  # The DMARC report mailbox is an owner pick (gd-o4fbw9qUJc item b); until it is set
  # in the untracked terraform.tfvars, DMARC is published without rua.
  dmarc_rua = var.ses_lands_dmarc_rua
  tags      = local.common_tags
}

# ── support@lands.clonk.tv forward (Lands §6.1, gd-UBeJ3kVicQ) ─────────────
# MX lands.clonk.tv -> SES inbound; one receipt rule (support@ only, spam/virus
# scanned) stores the raw mail in a private expiring bucket and invokes a Lambda that
# re-sends it to the operator with Reply-To the sender. The forwarder sends only as
# support@lands.clonk.tv on module.ses_lands' identity; its errors alarm to the edge
# probe topic. The mailbox defaults to the edge-probe alert address (already the
# operator's, in the untracked terraform.tfvars). Apply with
# `./scripts/tf-gardenio.sh apply -target=module.ses_support_inbound`
# (modules/ses-support-inbound/README.md).
module "ses_support_inbound" {
  source          = "../../modules/ses-support-inbound"
  domain          = "lands.clonk.tv"
  zone_id         = data.aws_route53_zone.clonk.zone_id
  identity_arn    = module.ses_lands.identity_arn
  forward_to      = var.lands_support_forward_to != "" ? var.lands_support_forward_to : var.edge_probe_alert_email
  alarm_topic_arn = module.edge_probes.topic_arn
  tags            = local.common_tags
}

# ── Lands mailer sender (Lands gate 5, gd-j0YpZFfoK7) ─────────────────────
# The controller's mailer (internal/mailer, SES v2 SendEmail) sends the Lands
# applicant and land mail as lands@lands.clonk.tv. k3s has no IRSA, so it reads
# a static key from the mounted Secret gardenio-mailer-ses (agents.mailer
# credentialsSecret, task gd-kSLbp23ekV). This user may only SendEmail as that one
# address on module.ses_lands' identity. Like the Keycloak SMTP user, no
# aws_iam_access_key: the key is minted out of band so its secret never sits in
# state. Apply with
# `./scripts/tf-gardenio.sh apply -target=aws_iam_user.lands_mailer -target=aws_iam_user_policy.lands_mailer_send`.
resource "aws_iam_user" "lands_mailer" {
  name = "gardenio-lands-mailer"
  path = "/gardenio/"
  tags = merge(local.common_tags, {
    "gardenio:purpose" = "ses-sendemail lands@lands.clonk.tv controller mailer"
  })
}

resource "aws_iam_user_policy" "lands_mailer_send" {
  name = "ses-sendemail-lands.clonk.tv"
  user = aws_iam_user.lands_mailer.name

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid    = "SendAsLandsOnTheLandsIdentity"
      Effect = "Allow"
      Action = "ses:SendEmail"
      # Every identity, not just lands.clonk.tv: while the account is in the SES
      # sandbox, SES also authorizes against each RECIPIENT's verified identity,
      # so the identity-only grant was AccessDenied for every invite (gd-j0YpZFfoK7,
      # invites gd-AP0yL0rSB8, gd-c2w9fxaWmF). The From condition still pins it.
      Resource = "arn:aws:ses:us-east-1:${data.aws_caller_identity.current.account_id}:identity/*"
      Condition = {
        StringEquals = { "ses:FromAddress" = "lands@lands.clonk.tv" }
      }
    }]
  })
}

# ── Session-pod image registry ───────────────────────────────────────────
# The gc k8s session runtime hardcodes ImagePullPolicy: Always on the agent
# container (internal/runtime/k8s/pod.go builds it that way, with no env
# override), so an image imported straight into a node's containerd is never
# used -- session pods can only start from a registry the kubelet can reach.
# Private, with untagged images expired; pods authenticate through an
# imagePullSecret attached to their ServiceAccount, so the node role
# deliberately gets no ECR grant of its own.
resource "aws_ecr_repository" "session" {
  name                 = "gc-session"
  image_tag_mutability = "MUTABLE"
  # Dogfood registry: a destroy should not strand the repository on images.
  force_delete = true

  image_scanning_configuration {
    scan_on_push = true
  }

  tags = local.common_tags
}

resource "aws_ecr_lifecycle_policy" "session" {
  repository = aws_ecr_repository.session.name
  policy = jsonencode({
    rules = [{
      rulePriority = 1
      description  = "expire untagged images after 7 days"
      selection = {
        tagStatus   = "untagged"
        countType   = "sinceImagePushed"
        countUnit   = "days"
        countNumber = 7
      }
      action = { type = "expire" }
    }]
  })
}

# ── gardenio platform images ─────────────────────────────────────────────
# One repository per component (see var.gardenio_image_components). CI pushes
# here through GitHub OIDC (ci_ecr.tf); the k3s node role may pull gardenio/*
# and nothing more (ecr_read_repo_arns above), since every pod that reaches
# IMDS inherits it.
resource "aws_ecr_repository" "gardenio" {
  for_each = toset(var.gardenio_image_components)

  name                 = "gardenio/${each.value}"
  image_tag_mutability = "MUTABLE"
  force_delete         = true

  image_scanning_configuration {
    scan_on_push = true
  }

  tags = merge(local.common_tags, { component = each.value })
}

resource "aws_ecr_lifecycle_policy" "gardenio" {
  for_each = aws_ecr_repository.gardenio

  repository = each.value.name
  # ECR evaluates rules by priority, and an image matched by rule 1's tag
  # selection is never expired by rule 2, so the 15 newest calver releases
  # survive however old they are.
  policy = jsonencode({
    rules = [
      {
        rulePriority = 1
        description  = "keep the 15 newest calver releases"
        selection = {
          tagStatus     = "tagged"
          tagPrefixList = ["20"]
          countType     = "imageCountMoreThan"
          countNumber   = 15
        }
        action = { type = "expire" }
      },
      {
        rulePriority = 2
        description  = "expire everything else (sha/latest-only, untagged) after 14 days"
        selection = {
          tagStatus   = "any"
          countType   = "sinceImagePushed"
          countUnit   = "days"
          countNumber = 14
        }
        action = { type = "expire" }
      },
    ]
  })
}

# ── ECR pull-through cache for ghcr ──────────────────────────────────────
# Adopts resources created by hand on 2026-09-18 while bringing the gardenio
# platform up (see the import blocks below): the cache rule, its credential
# secret, and the repository creation template. The node grant lives inline on
# the k3s node role (ecr_cache_repo_arns above), so create_node_policy stays
# false rather than adding a second overlapping policy.
#
# untagged_expiry_days closes the hole rather than cleaning up after it: a
# creation template applies only at repo creation, so it governs every cache
# repo a FIRST pull auto-creates from here on. The four repos that already
# existed were backfiled with put-lifecycle-policy out-of-band and are not
# terraform-managed -- ECR creates them, not us.
module "ecr_pull_through_cache" {
  source   = "../../modules/ecr-pull-through-cache"
  env_name = var.env_name
  enabled  = true

  create_node_policy   = false
  untagged_expiry_days = 2

  upstreams = {
    ghcr = {
      ecr_repository_prefix = "ghcr"
      upstream_registry_url = "ghcr.io"
      requires_credential   = true
      # Adopts the existing secret; renaming would force replacement and a
      # cache rule's credential_arn cannot be repointed in place.
      secret_name_suffix = "ghcr-gardenio"
    }
  }

  common_tags = local.common_tags
}

