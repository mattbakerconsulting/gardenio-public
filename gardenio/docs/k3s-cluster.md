# The gardenio cluster: k3s on spot

Three k3s servers with embedded etcd, 100% spot, two AZs, public subnets, no
NAT, no load balancer, no EKS. Defined in `terraform/modules/k3s`, composed in
`terraform/composition/gardenio`. Built 2026-09-16 after EKS was applied and
destroyed the same day for cost.

## Cost (us-east-1 spot prices at build time)

| item | qty | $/month |
| --- | --- | --- |
| t3a.medium / t3.medium servers (1 on-demand, 2 spot; was t3a.small until 2026-10-14) | 3 | ~37+ |
| gp3 root volumes, 30 GiB | 3 | 7.2 |
| Route53 `clonk.tv` zone (pre-existing) | 1 | 0.5 |
| SSM parameters, IMDS, intra-VPC traffic | | ~0 |
| **total** | | **~$45** |

Servers are 4 GiB (`server_instance_types`, `["t3a.medium","t3.medium"]`) since
2026-10-14 (gd-ybTutceJpn). At 2 GiB, k3s-server alone is 0.85-1.2 GiB RSS and the
servers also host everything that tolerates the control-plane taint (CoreDNS,
traefik, metrics-server, local-path, CCM, ziti router-aws-1), so a server roll
ran all three out of memory one after another and took cluster DNS down. Do not
size below 4 GiB. The ARC controller, the ARC listeners and cluster-autoscaler
PREFER the platform pool (`gardenio.io/role=platform`, toleration
`gardenio.agent=true:NoSchedule`) and fall back to the servers only if the pool
has no room. CoreDNS runs 2 replicas with a soft hostname spread, re-asserted by
each server's bootstrap (`node.sh.tftpl`).

Rolling servers: changing the instance type or user_data edits the launch
template only; running servers are never refreshed (no `instance_refresh`,
AZRebalance stays suspended). Replace spot servers one at a time, checking
`free -m` on the survivors first (>= 600 MiB available).

Never stop a server without first taking it out of the ASG's health checks.
Every server is an ASG member, and the ASG treats a stopped instance as
unhealthy and terminates it within a minute, root volume and local-path PVs
included. That is how ip-10-90-5-163 and its PVs (router-aws-1 identity, the
dolt volumes) were lost on 2026-10-14 (gd-ybTutceJpn). To resize in place,
suspend replacement first and resume it after the start:

    aws autoscaling suspend-processes --auto-scaling-group-name gardenio-k3s-servers \
      --scaling-processes HealthCheck ReplaceUnhealthy
    # stop-instances / modify-instance-attribute / start-instances, wait for Ready
    aws autoscaling resume-processes --auto-scaling-group-name gardenio-k3s-servers \
      --scaling-processes HealthCheck ReplaceUnhealthy

Do not resume `AZRebalance`: it stays suspended (see the etcd quorum notes).

## Getting a kubeconfig

The first server publishes it to SSM, already pointed at the API name:

```sh
aws ssm get-parameter --profile gardenio --region us-east-1 \
  --name /gardenio/k3s/kubeconfig --with-decryption \
  --query Parameter.Value --output text > ~/.kube/gardenio.yaml
chmod 600 ~/.kube/gardenio.yaml
KUBECONFIG=~/.kube/gardenio.yaml kubectl get nodes -o wide
```

Port 6443 is open only to the CIDRs in `composition/gardenio/terraform.tfvars`
(`admin_cidrs`). When your public IP changes, update it and `terraform apply`.
Once `admin_cidrs = []` there is no public 6443 at all: use the SSM
port-forward instead (`scripts/k3s/kubectl-ssm.sh start`, then
`kubectl --context gardenio-ssm ...`; see `scripts/k3s/README.md`).
There is no SSH; use SSM Session Manager
(`aws ssm start-session --target <instance-id>`).

## Break-glass: admin access does not depend on OpenZiti

Proven and documented for gd-hJzAV4kTTh, part of the OpenZiti-into-k3s
migration (epic gd-Vn8uWTbQ9S). The controller for the ziti mesh (see
`ziti-routers.md`) runs on a single Vultr box; ziti is now switched off
(gd-xNMLrsgajy) and that box is used only when the opt-in overlay is turned
back on (`../helm/ziti/README.md`, "Where the ziti controller lives").
Whatever happens to it, cluster admin access must not go dark with it.

**All three paths below are already ziti-independent — that is the point of
this section, made explicit.**

0. **PRIMARY — SSM to the HQ host, then kubectl from there.** `aws ssm
   start-session --target <hq-instance-id>` reaches the HQ box with no SSH, no
   bastion, and no ziti; HQ already carries `kubectl` and a kubeconfig pointed
   at `k3s-join.clonk.tv` (the *private*-IP DNS record — see "Two names, one
   cluster" below), and the k3s security group allows port 6443 from HQ's own
   security group. The only dependency is AWS SSM/IAM access to the
   `gardenio` account; there is no source-IP allow-list to fall out of.
   **Caveat:** HQ powers itself off after 30 minutes idle, and SSM
   run-commands do not count as activity. First step is always `aws ec2
   start-instances --instance-ids <hq-instance-id>`; for anything longer than
   a quick check, `touch /var/lib/hq/hold` on HQ and **remove it when done** —
   left in place, HQ never powers off again.
1. **Fast path, not reliable — kubectl via the public API name** (`Getting a
   kubeconfig`, above). `aws ssm get-parameter` reads a plaintext-in-SSM
   (SecureString) value from AWS's own control plane; the resulting
   kubeconfig points straight at `k3s.clonk.tv:6443`, the k3s API server's own
   TLS listener. Nothing in this path — not the credential fetch, not the
   connection — passes through a ziti controller, router, or tunneler. But
   port 6443 is open only to the CIDRs in `admin_cidrs`
   (`composition/gardenio/terraform.tfvars`), and that is currently a single
   `/32`. It works only from that one source IP; a DHCP renewal, ISP
   reassignment, hotspot, or any other network silently closes it, and the
   failure is a plain connection timeout with no hint that a tfvars CIDR is
   the cause. Treat this as the fast path when your IP happens to match, not
   as the dependable one — widening the `/32` is an explicit operator
   trade-off, not proposed here.
2. **SSM Session Manager to a k3s node**, when the API itself is unreachable
   or a node needs a shell: `aws ssm start-session --target <instance-id>`,
   instance IDs from
   `aws ec2 describe-instances --profile gardenio --region us-east-1
   --filters "Name=tag:Name,Values=gardenio-k3s-server"
   "Name=instance-state-name,Values=running" --query
   "Reservations[].Instances[].InstanceId" --output text`.
   This rides the SSM agent and AWS's control plane only — no SSH, no
   bastion, no ziti. (Architecturally ziti-independent; not exercised in the
   2026-09-19 verification below.)

All three paths survive every ziti failure mode in `ziti-routers.md` ("What
breaks if the Vultr controller is down") by construction: they don't call
anything the ziti controller signs, issues, or fronts. The one shared
dependency across all three is AWS IAM/SSM access to the `gardenio` account —
losing *that* is a real single point of failure, but it is an AWS-account
concern, not a ziti one, and is out of scope here.

**Verified 2026-09-19, ~17:00 UTC**, run live against the cluster with no
ziti identity loaded (results from `gd-hJzAV4kTTh` comments, escalation
`gd-ZmlJHMTL9c`):

- **Path 1** (direct kubeconfig -> `k3s.clonk.tv:6443`): confirmed working.
  `KUBECONFIG=~/.kube/gardenio.yaml kubectl get nodes -o wide` returned 5
  nodes (one control-plane, three `Ready` agents, one `NotReady,
  SchedulingDisabled`), matching the pool state at the time. A direct TLS
  probe (`curl -k https://k3s.clonk.tv:6443/ping`) returned 200 with the k3s
  CA cert. Confirmed independent of ziti by construction — `k3s.clonk.tv` is
  not a ziti intercept (the intercept name is the distinct
  `gardenio.k3s.ziti`) — but confirmed fragile in practice: the single-`/32`
  `admin_cidrs` pin is what made it work from the operator's laptop that day,
  not a durable property of the path.
- **Path 0** (SSM -> HQ -> kubectl): confirmed as the sturdier path and
  promoted to PRIMARY above. Used all night by a separate terraform/RBAC
  session with HQ stopped and restarted under it, with no ziti dependency and
  no IP allow-list to fall out of.
- **Path 2** (SSM Session Manager straight to a k3s node) was **not**
  exercised in this verification — it needs AWS credentials the checking
  session didn't have. Its ziti-independence claim still stands on
  architecture (same SSM control-plane mechanism as path 0), just not on a
  live test.

Grafana (public edge, `https://grafana.clonk.tv`, behind the edge sign-in) has its own runbook:
[`grafana-operator-runbook.md`](grafana-operator-runbook.md).

## How it stays up on spot

- **Two names, one cluster.** `k3s.clonk.tv` carries the servers' *public*
  IPs and is what operators use. `k3s-join.clonk.tv` carries their *private*
  IPs and is what nodes join through — a security-group self-reference only
  matches private-IP traffic, so a node dialling the public name is refused.
  Terraform owns both records' existence; the servers own their content
  (`k3s-dns add|remove`, run when a node is Ready and on interruption).
- **Leader election** for `--cluster-init` is an SSM `put-parameter
  --no-overwrite` on `/gardenio/k3s/initialized`: exactly one server wins.
  Every server, the initialiser included, starts with the token from
  `/gardenio/k3s/token` — a joiner presenting a different token is refused
  with "failed to bootstrap cluster data: not authorized".
  Joiners wait up to 20 minutes for the API, then delete the marker and
  re-elect, so a leader reclaimed mid-bootstrap does not wedge the cluster.
- **Interruption**: `k3s-spot-watch` polls IMDS every 5 s for the two-minute
  spot notice or a rebalance recommendation, then drops its IP from DNS,
  drains itself, deletes its Node (k3s removes the etcd member), and stops.
  The ASG (`capacity_rebalance = true`, capacity-optimized across both AZs)
  launches a replacement, which joins through the API name.
- **Quorum**: 3 members survive one reclaim. Two simultaneous reclaims lose
  quorum until replacements join — accepted for a ~$26 cluster.

## API server audit log

The servers write a kube-apiserver audit log (gd-HXRD9yizAE). It is the
server-side record of what an API identity did, chiefly the cluster-admin
ServiceAccount `gardenio-infra-admin` (see `infra-admin-role-design.md`),
whose only other trail is its own task-bead comments.

- **Where.** `/var/log/k3s-audit/audit.log` on each server, JSON lines. The
  policy is `/etc/rancher/k3s/audit-policy.yaml`, written by
  `node.sh.tftpl` before k3s starts. It is read on the node over SSM Session
  Manager, or from VictoriaLogs once the shipper below is on.
- **Rotation.** `audit-log-maxage=7` (days), `maxbackup=5`, `maxsize=100` (MB):
  at most ~500 MB per server. Under heavy traffic size trims before age.
- **What is recorded** (first match wins, order matters):
  1. Dropped entirely: health probes, kubelet traffic (`system:nodes`),
     kube-proxy / scheduler / controller-manager reads.
  2. Secrets, `serviceaccounts/token`, TokenReview and CSR calls at
     **Metadata** only, for every identity. The log never holds a Secret
     body or a minted token. This rule sits above the next one on purpose.
  3. Writes by `gardenio-infra-admin` at **RequestResponse** (full bodies),
     including its event and lease writes.
  4. Events and leases dropped for everyone else (below rule 3 so rule 3 sees
     the admin's).
  5. Everything else, reads included, at **Metadata**.
  The order of 2, 3 and 4 is pinned by
  `scripts/check-k3s-audit-policy-order_test.py` (runs in CI).
- **Not covered.** Secret *reads* by a kubelet are dropped by rule 1; the
  admin identity's Secret reads are recorded (name, user, time, no body).
  An `exec` records the URL with the command, not the session's output.
- **The log dies with the node.** Servers are spot and the root volume is
  deleted on termination, so a reclaimed server takes its audit history with
  it. Decision gd-bNVBdrmIo5 chose to ship the log to the in-chart VictoriaLogs
  (`vlsingle`) and keep it 30 days there (its `retentionPeriod`). The
  audit shipper (gd-ZrPUxj8wyU) does that; see
  [Shipping to VictoriaLogs](#shipping-the-audit-log-to-victorialogs).
- **Size.** The audit block is rendered for the server role only and the server
  launch template's user data is gzipped (`base64gzip`): raw it is ~18.6 KB, over
  the 16 KB EC2 limit. Agent pools are unchanged (~15.5 KB).
- **Rollout.** The change is launch-template user data, so a server picks it
  up only when it is replaced (spot churn, or an instance refresh one server
  at a time to keep etcd quorum). It shares the single rollout with the
  `event-ttl` flag in the same `SERVER_FLAGS`. Verify on a rebuilt server:
  `ls -l /var/log/k3s-audit/` grows, and
  `jq -c 'select(.user.username|endswith("gardenio-infra-admin"))' /var/log/k3s-audit/audit.log`
  shows its calls. After a day, check `du -sh /var/log/k3s-audit` and the
  file count to see whether 100 MB x 5 holds the 7-day window.

### Shipping the audit log to VictoriaLogs

The shipper is a DaemonSet pinned to the control-plane nodes (it tolerates
`node-role.kubernetes.io/control-plane:NoSchedule` and requires the
`node-role.kubernetes.io/control-plane` label). It is **not in the gardenio Helm
chart**: its manifests are operator-applied from `helm/k3s-audit-shipper/` (see its
README for apply steps). Source: `controller/cmd/k3s-audit-shipper`,
`controller/internal/k3sauditshipper`.

- **Reads** `/var/log/k3s-audit/` through a read-only hostPath, follows the
  apiserver's rotation (`audit.log` -> `audit-<timestamp>.log`) and drains the old
  file before moving on. It keeps a checkpoint (inode, offset, a hash of the
  file's head, since inodes get reused) in `/var/lib/k3s-audit-shipper` on the
  node, advanced only after VictoriaLogs accepts a batch: delivery is
  at-least-once, a restart neither skips events nor re-ships them (a lost
  checkpoint costs duplicates; dedupe on `auditID`).
- **Writes** to `vlsingle` `/insert/jsonline` **in its own tenant** (`AccountID: 9`),
  stream fields `source=k3s-audit` and `node`, `_time` = the event's
  `stageTimestamp`, `_msg` = a one-line summary
  (`update configmaps gardenio/cfg by <user> -> 200`). Default-tenant readers
  (`gb logs`, Grafana, vmalert-logs) never see it; `internal/vlogs` pins
  `AccountID: 0`. Read it with an `AccountID: 9` header (README, "Reading the audit
  log"). The tenant is **not access control**: VictoriaLogs has no auth, so a pod
  that can reach :9428 can set the header and read or forge records. That stays
  open until NetworkPolicies come on (README, "Residual risk").
- **Redacts before it ships** (`Transform`). The policy logs the admin identity's
  writes at RequestResponse, so a body can carry operator data:
  - Secrets, ConfigMaps, HelmCharts/HelmChartConfigs, ExternalSecrets, token and
    authn/authz/CSR calls, `pods/exec|attach|portforward` and the webhook
    configurations never ship a request or response body, whatever the policy
    logged. The record says `requestObjectOmitted: "sensitive-kind"`.
  - Other bodies: any key matching password/secret/token/apiKey/credential/
    private key/`valuesContent`/`stringData`/`data`... has its value masked, a
    container env entry is masked by its `name`, `managedFields` and
    `last-applied-configuration` are dropped, and a body over `maxBodyBytes`
    (32 KiB) is replaced by a marker.
  - Every remaining string value is redacted **one value at a time** (never over the
    serialized line: a `token=` match over the whole record can swallow a closing
    quote and yield invalid JSON, which VictoriaLogs drops or rejects, blinding that
    node): the `claudemore` contract (`sk-`/`glpat-`/`token=`) plus JWTs,
    `Authorization:`/`Bearer`, `NAME_KEY|_DSN|_PAT|_SECRET|_TOKEN|_PASSWORD=value`,
    `user:pass@` URLs and PEM private keys. An `exec` URL's query is percent-decoded
    first so those see it. `user` and `impersonatedUser` are cut to
    username/uid/groups.
  - The finished line is re-validated (one JSON object on one line, no credential
    shape left). A line that fails is dropped and logged as an error with a running
    count rather than retried forever.
  - `bodies: none` ships metadata only.
- **Why a separate namespace, and why it is not in the chart.** The release
  namespace enforces Pod Security `baseline`, which forbids hostPath. The shipper
  runs in `gardenio-audit`, labelled PSA `privileged`. Rights over a pod there are
  root on the control plane, so the release deployer (which cannot create
  namespaces and has no rights in `gardenio-audit`) is not given any: the
  namespace and DaemonSet are applied by an operator. The pod runs as uid 0 because
  the apiserver writes the file `0600 root`; all capabilities are dropped, the root
  filesystem is read-only, the audit mount is read-only, and it mounts no
  ServiceAccount token.
- **Enabling it** needs: `vlsingle.enabled=true` (it is `false` in
  `deploy-values/gardenio-k3s.yaml`, which is why `gb logs` fails too,
  gd-q0Sy8rdaLc), the audit policy rolled to the servers (a server without
  `/var/log/k3s-audit` leaves the pod in `ContainerCreating`/`FailedMount` on
  purpose), and `helm/gardenio-ziti/ecr-token-refresh.yaml` re-applied so the `ecr`
  pull secret is kept fresh in `gardenio-audit`. `networkPolicies` are off on this
  cluster; the egress policy in `helm/k3s-audit-shipper/networkpolicy.yaml` is for
  the day they are turned on.
- **Sizing.** vlsingle ships with a 10Gi PVC and `retention.maxDiskSpaceUsageBytes:
  8GiB`. Measured on the three servers 2026-10-15 (gd-ZrPUxj8wyU, SSM read-only):
  ~35, ~85 and ~315 MB/day of raw JSON (the busiest server rotates every ~7.6 h),
  ~435 MB/day in all, so 30 days is ~13 GB raw. VictoriaLogs typically compresses
  such logs 10x or better, ~1.3 GB, comfortably under the 8GiB cap, but that
  ratio is an estimate: after a day of shipping check `vl_data_size_bytes`. The
  shipper drops `user.extra` and, by default, ships every other field, so the
  stored size is a little under the raw size. If it outgrows the PVC, set
  `K3S_AUDIT_DROP_READS=true` in the DaemonSet (ships writes plus reads by
  `K3S_AUDIT_KEEP_READS_USERS`, default the admin SA) before widening the PVC. The
  audit tenant shares the PVC and the 8GiB cap with the default tenant.
- **Not covered.** SELinux: where a server runs enforcing, a container may be
  denied the `var_log_t` file; Amazon Linux 2023 ships permissive.

## Failure modes accepted

- A server reclaimed *without* the 2-minute notice (rare) leaves a stale IP in
  `k3s.clonk.tv` until the next server runs `k3s-dns` (it prunes IPs that are
  not running servers). Clients try every A record, so joins and kubectl work
  through the live ones meanwhile.
- The kubeconfig in SSM is the one the initial server wrote; it stays valid
  because every server has `--tls-san k3s.clonk.tv` and shares the CA.
- No persistent volumes yet. Anything stateful (Postgres) needs an EBS-backed
  StorageClass and a node-affinity story before it lands here.

## Verified 2026-09-16

Built from a clean account in ~7 minutes after apply (ASG launches servers
~2 minutes apart). Reclaim simulation on `ip-10-90-31-53`, exercising the
watcher's exact graceful path via SSM and then terminating the instance:

| event | clock | elapsed |
| --- | --- | --- |
| notice: DNS removed from both records, drained, node deleted | 03:21:55 → 03:22:41 | 46 s |
| instance terminated | 03:22:42 | — |
| API answering from the laptop, 2/2 nginx pods Running on other nodes | 03:22:44 | +2 s |
| ASG replacement launched | 03:23:30 | +48 s |
| replacement Ready, both records back to 3 IPs | 03:24:58 | +2 m 16 s |

The cluster never lost quorum or availability. Three t3.small servers ran
etcd + apiserver + two nginx pods without pressure during the test; watch
memory before adding real workloads (`kubectl top` needs metrics-server to be
scraped first).

Bugs found and fixed on the way (all in the bootstrap template): Ubuntu 24.04
has no `awscli` deb; nodes must join through the private-IP name; the
initialiser must be started with the shared token.
