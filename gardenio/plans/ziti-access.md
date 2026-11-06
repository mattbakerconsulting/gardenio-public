# OpenZiti as gardenio's access layer

**Status:** plan, 2026-09-16. **Decision context:** the user has named OpenZiti
"our preferred and soon to be exclusive tunnel mechanism and access mechanism",
and cost is the top priority (~$26/month cluster). This plan makes ziti the
way operators, agents and CI reach the cluster, and says plainly where it
cannot be the *only* way.

## What already exists in this tree

The codebase is the **consumer half** of a ziti deployment; the fabric half
(controller, routers, the access broker) lived in ACME's separate
`network-fabric-deployment` repo and is not here.

| piece | where | what it does |
| --- | --- | --- |
| SDK dialer for the beads daemon | `kbeads/internal/client/ziti.go:14-40`, `gardenio/controller/internal/beadsapi/client.go:126-137,323-330` | `ZitiIdentityPath` + `ZitiService` replace TCP dialing with an overlay dial; auth is untouched ("Ziti only gates reachability") |
| Per-pod overlay access for agents | `gardenio/controller/internal/podmanager/ziti_access.go:25-50`, `manager.go:415-417,1078-1084` | init container obtains a task-scoped lease from a broker with the pod's own SA token; a `ziti-tunnel` sidecar enrolls the JWT and runs `ziti-edge-tunnel` in TUN mode |
| Broker contract + runbook | `gardenio/docs/ziti-access-broker-integration.md` (status: "prototype, default-OFF", l.13) | lease minted in-pod, controller only revokes (l.19-30); constraints l.200-215 |
| Chart switches | `values.yaml:4788-4812` (`agents.zitiAccess.*`, default `enabled: false`), `values.yaml:584-612` (`zitiBrowser`: a Traefik `ziti` entrypoint fronted by oauth2-proxy/Entra), `templates/ziti-browser/*` | the "private browser" shape: UIs reachable only on an entrypoint bound to the overlay |
| MicroVM agents | `gardenio/images/microvm-agent/Dockerfile:3-53` (ziti CLI 2.0.0), `helm/gardenio/tests/microvm_ziti_broker_test.go` | per-run identities via the broker |
| Retired AWS alternative | `gardenio/terraform/modules/edge-auth/main.tf:1-12` | AWS Verified Access with gitlab.com OIDC — ~$250/mo, deleted in the teardown |
| ACME-era fabric state | EBS snapshot `snap-0123456789abcdef0` ("ziti controller state and PKI"), 5 GiB | the old controller's PVC; see "fresh vs restore" |

Two things the tree assumes that we no longer have: the broker (`brokerURL`)
and the Traefik `ziti` entrypoint's binding — neither is defined anywhere
here (`grep -rn 'entrypoints.ziti'` is empty); both were fabric-owned.

## 1. Topology: self-host on k3s

Run the fabric on the cluster we have. Charts (from `helm repo add openziti
https://openziti.io/helm-charts/`, 2026-09-16): `openziti/ziti-controller`
3.3.2 (app 2.0.3), `openziti/ziti-router` 3.0.3 (app 2.0.3),
`openziti/ziti-edge-tunnel` 1.5.0 for the laptop. Namespace `ziti`.

- **Controller**: 1 replica, data on a PVC. k3s ships `local-path` only — a
  reclaim would strand the volume on the dead node — so the controller needs
  an **EBS-backed StorageClass** (the same "node-affinity story" the k3s doc
  already flags as the first stateful-workload cost, `docs/k3s-cluster.md:71`).
  Accepted failure mode: on a controller-node reclaim the overlay's *control
  plane* is down for the ~2–3 min the ASG takes to replace the node and the
  PVC re-attaches (EBS is AZ-bound: pin the controller to one AZ via
  `WaitForFirstConsumer` + the ASG spanning both — or accept a rebuild from
  the daily snapshot). Existing dial sessions on routers survive a controller
  outage; new dials do not.
- **Edge routers**: 2 (one per AZ), `hostNetwork` on a public-IP node, each
  listening on a **single public TCP port (443 or 3022)**. They are the only
  thing ziti needs open to the internet. Routers are re-enrollable, so a
  reclaimed router node is replaced by re-running enrollment from an SSM-held
  JWT in the node's user-data hook (`modules/k3s/templates/node.sh.tftpl`
  already runs post-Ready hooks; add one).
- **Stable name without an LB**: reuse the mechanism the k3s join name uses —
  `node.sh.tftpl:16-42` UPSERTs the node's IP into a Route53 A record and
  prunes dead ones (`k3s-dns add|remove`). Add `ziti.clonk.tv` (router public
  IPs, TTL 30) driven by the same script for router nodes. Clients (the
  tunneler, the SDK) retry across A records exactly as kubectl does today.
  No ELB (~$18/mo), no EIP.
- **Fresh PKI, not the snapshot.** The snapshot holds ACME's controller
  identity, CA, and every enrolled identity/policy from the gardenio fleet —
  restoring it re-imports trust for identities that no longer exist and a CA
  we do not want to keep. Mint a new PKI; keep the snapshot only until the new
  fabric is proven, then delete it (~$0.25/mo otherwise).

## 2. Phase 1 — laptop → cluster, then close 6443

Today: `modules/k3s/main.tf:72-77` allows 6443 from `var.admin_cidrs`, set to
the operator's current public IP in `composition/gardenio/terraform.tfvars`
("update when it changes"). That is the brittle bit.

1. Install the fabric (above) and enroll one operator identity on the laptop.
   `ziti-edge-tunnel` on WSL2: the WSL kernel has `/dev/net/tun`, and systemd
   is PID 1 here, so `ziti-edge-tunnel run` as a system service works.
   Gotchas: the tunneler's DNS resolver wants to own `/etc/resolv.conf`, which
   WSL regenerates — set `[network] generateResolvConf=false` in
   `/etc/wsl.conf` (needs the same one-time sudo the runner setup needs), or
   run in proxy mode (`ziti-edge-tunnel run-host`/`tproxy` off) and use an
   explicit `--dns-upstream`. Docker rootless containers on the laptop do
   **not** see the host TUN by default; CI jobs that need the cluster use
   `ziti-edge-tunnel` in the job container or an SDK-based `kubectl` — see
   Phase 3.
2. Ziti service `k3s-api` → `k3s-join.clonk.tv:6443` (the private name;
   routers sit inside the SG so intra-VPC works — the same reason joins use
   it, `docs/k3s-cluster.md:43-47`). Bind policy: routers; dial policy: the
   operator identity. Intercept `k3s.ziti:6443` on the tunneler.
3. kubeconfig: `server: https://k3s.ziti:6443`, `tls-server-name:
   k3s.clonk.tv` (the servers already have that SAN, `docs/k3s-cluster.md:68`).
   `kubeztl` is not needed; plain kubectl over the tunneler suffices.
4. **Then remove the `api` ingress rule** (`main.tf:72-77`) and drop
   `admin_cidrs`. `k3s.clonk.tv` stays for the servers' own use and as the
   TLS SAN; nothing on the internet can reach 6443 any more.

SSM Session Manager (`modules/k3s` attaches the SSM core policy) needs no
inbound and stays as the **break-glass** path — it is how you fix the fabric
when the fabric is down. "Exclusive" should mean ziti for all *routine*
access; SSM remains, gated by IAM, for recovery.

## 3. Phase 2 — services over ziti instead of public ingress

The chart already has the shape: `zitiBrowser` (`values.yaml:584`) puts the
UIs on a Traefik entrypoint that exists only on the overlay, fronted by
oauth2-proxy. With our own fabric, the entrypoint binds to a ziti service
hosted by a `ziti-host` pod (or the router's tunneler) that forwards to
Traefik's ClusterIP — Traefik needs **no** public port.

| component | reachability | ziti-only? |
| --- | --- | --- |
| beads-viewer, grafana, coop mux, the kbeads API | operator/browser → cluster | **yes** — `zitiBrowser` paths `/beads`, `/mux`, `/kd` (`values.yaml:589-593`) |
| controller ↔ kbeads daemon (in-cluster) | pod → pod | in-cluster ClusterIP is fine; optionally the SDK dialer (`client.go:126`) if the daemon ever leaves the cluster |
| slack-bridge | Slack → us | **yes, via Socket Mode** — outbound WebSocket, no inbound (`internal/bridge/bot.go` imports `slack-go/slack/socketmode`; webhook mode is optional, `cmd/slack-bridge/helpers.go:735`) |
| gitlab-bridge `/webhook`, jira-bridge `/webhook`, push-bridge | third party → us | **no** — these accept **inbound webhooks from the public internet** (`cmd/*-bridge`, `grep Handle(`). GitLab is dead; a GitHub replacement would be the same shape. |

**Open question (asking, not forcing):** for the webhook receivers, which of:
(a) keep one small public entrypoint (the chart's existing `webhooks`
entrypoint idea, `values.yaml:1587`, with the "containment wall" that returns
404 for anything off-apex) — one port, ACM-free if Traefik terminates TLS
with Let's Encrypt via cert-manager, cost $0; (b) poll instead of receive
(GitHub/Jira both support polling APIs at the cost of latency) — then the
cluster has **zero** public ports besides the ziti routers; (c) a hosted relay
(e.g. a tiny Lambda URL that re-publishes webhooks into the overlay) — extra
moving part, ~free. My default is (a) for GitHub and (b) for Jira/Alertmanager
until traffic justifies otherwise, but this is the one place ziti does not fit
and it needs your call.

What Phase 2 removes: public 80/443 on nodes (`main.tf:82-86`), Traefik's
public entrypoint, ACM (never re-created), any LB, `edge-auth`/Verified Access
(already gone), and the oauth2-proxy's Entra dependency can become any OIDC
(or be dropped — the overlay identity is already per-person).

## 4. Phase 3 — agents and CI

- **Agent pods.** The design in `ziti_access.go` is sound but the TUN shape
  needs PSA `privileged` (NET_ADMIN + `/dev/net/tun` hostPath,
  `ziti_access.go:38-46`; the runtime warning at `manager.go:1084`). k3s has
  no PSA labels by default, so it *works* — but the doc's own recommendation
  is better: the **SDK/proxy enrollment shape** (workload links the SDK, no
  tun, no NET_ADMIN, `ziti-access-broker-integration.md:203-208`) keeps
  agents unprivileged. It "drops in behind the same lease/JWT handoff".
- **The broker.** We do not have one. Two options: (i) run the ziti
  controller's own identity enrollment directly — the controller mints a
  per-task identity (`ziti edge create identity … --jwt-output-file`) from an
  init container with a scoped admin credential, and revokes on exit
  (`cmd/controller/ziti_revoke.go` already exists for the revoke half); this
  is the broker's job done by the controller SA and loses the confused-deputy
  property the doc cares about (`:19-30`). (ii) Write a minimal broker
  (~200 lines: validate projected SA token, call the controller API, return a
  JWT). Recommend (i) for phase 3a — it is honest about who mints — and (ii)
  only if multi-tenant trust ever matters.
- **CI (laptop) → cluster.** Deploy steps run `helm upgrade` from the laptop
  runner. Give the runner its own ziti identity (`ci-laptop`), dial policy
  `k3s-api` only, and run `ziti-edge-tunnel` on the host so `kubectl`/`helm`
  in jobs see `k3s.ziti`. Rootless-Docker job containers do not inherit the
  host TUN; run deploy steps on the host (`runs-on` the laptop, no container)
  or use `ziti-edge-tunnel run-host` proxy mode with `HTTPS_PROXY`.
- **coop / coopmux.** The mux is reached via `/mux` on the private browser
  (`values.yaml:590`); agents reach it in-cluster. No change.

## 5. Identity and enrollment ops

- Controller admin credentials, the PKI root passphrase, and router enrollment
  JWTs live in **SSM Parameter Store** (SecureString, free) under
  `/gardenio/ziti/*`, next to `/gardenio/k3s/*`. Nodes already have the SSM
  policy; scope a read-only path grant for the router hook.
- Identities: `ziti edge create identity user <name> -a operators -o <name>.jwt`;
  enroll on the device with `ziti-edge-tunnel enroll`. One identity per person
  and per machine role (`op-<person>`, `ci-laptop`); attributes drive policies
  (`#operators` dial `k3s-api`+`ui`, `#ci` dial `k3s-api`).
- Rotation/revocation: `ziti edge delete identity`; router re-enrollment on
  replacement is automatic via the hook. Controller cert renewal: the chart
  manages it with cert-manager (chart dependency) — install cert-manager
  (already in `modules/platform/cert_manager.tf` shape) first.
- End state of "exclusive": SG has **two** inbound rules — intra-SG self and
  the router port(s) — plus whatever Phase 2 decides for webhooks. No 6443,
  no 80/443 unless (a) above. SSM stays for break-glass.

## 6. Cost

Marginal **~$0**: controller + 2 routers run on the three existing t3.small
servers (they are small — the controller ~200 MB RSS; watch the t3.small
headroom caveat in `docs/k3s-cluster.md`). Deltas: one 10 GiB EBS volume for
the controller PVC (~$0.80/mo), one extra Route53 record (free), snapshots of
that volume (~$0.05/GiB-mo). Removes: the SG allowlist churn, any future LB
(~$18/mo), ACM/ingress plumbing, and the ~$250/mo Verified Access this
replaced. Egress is unchanged (traffic already leaves via public IPs).

## 7. Risks and open questions

1. **Controller on spot** — single replica with an AZ-pinned PVC; ~2–3 min
   overlay control-plane outage per reclaim of that node. Acceptable at this
   cost; a second controller (HA) is a v2 item and needs the enterprise-ish
   raft setup.
2. **Router public port** — the one internet-facing surface. mTLS-only, no
   plaintext listener; keep it on 443 so egress-filtered networks can reach it.
3. **WSL2 TUN + DNS** — `generateResolvConf=false` is a one-time sudo edit;
   until then use proxy mode. Windows sleep drops the tunnel; it reconnects.
4. **Webhook receivers** — the open question in §3; ziti cannot make Slack
   or GitHub speak ziti.
5. **Broker vs direct minting** — §4; direct minting is simpler and
   sufficient for a solo operator.
6. **The ACME snapshot** — delete after the new fabric is proven (do not
   restore it).
7. **Docker-in-CI visibility of the TUN** — deploy steps must run on the host
   or in proxy mode; build steps never need the cluster.

## Touchpoints

Terraform (`modules/k3s`): drop `aws_vpc_security_group_ingress_rule.api`
(`main.tf:72-77`) and later `.web` (`:82-86`); add the router port rule; add
`ziti.clonk.tv` to the records `node.sh.tftpl` maintains; add
`/gardenio/ziti/*` to the SSM policy; an EBS StorageClass (aws-ebs-csi via
helm, or the k3s `local-path` + AZ pin as the cheap first cut).
Helm (`gardenio/helm/gardenio`): `zitiBrowser.enabled`, `.host`, the
oauth2-proxy provider; `agents.zitiAccess.enabled`/`tunnelerImage` (or the
SDK shape when it lands); a new `ziti/` values block for the two openziti
charts as subcharts or a sibling release.
