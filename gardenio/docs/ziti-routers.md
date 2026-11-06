# OpenZiti routers for gardenio

> **Opt-in only.** This applies only when the ziti overlay is on
> (`ziti.enabled=true`, `gardenio/helm/ziti`, off by default since
> gd-xNMLrsgajy). The Vultr controller it describes is kept running, unchanged,
> by owner decision 2026-10-13, and nothing on gardenio's default path dials
> it: see [`../helm/ziti/README.md`](../helm/ziti/README.md) "Where the ziti
> controller lives".

Written 2026-09-17, when `router-aws-1` was added. Companion to
`beads-on-k3s.md` (which uses the fabric) and `hq-host.md`.

## Topology

```diagram
                 controller (Vultr, 45-77-68-134.nip.io:8440)
                   |                         |                      |
   laptop/phone ---+                         +--- router-aws-1      +--- router-public-1 (k3s ns ziti)
        |                                          (k3s ns ziti)         attr public
        v                                          attr private-aws      edge :3022 on ziti.clonk.tv
   router-instance-1 (Vultr, :8442) <--fabric link-- (dialled outbound)  (public internet, link listener)
        attr public                                    ^            ^          ^
                                                       |            |          |
                                        gardenio-hq (HQ host)   k3s-beads-host  laptop/phone/anything
                                        i-0123456789abcdef0     (ns beads pod)  off-VPC (eventual Vultr cutover)
```

`router-public-1` is being stood up to replace `router-instance-1` (gd-dCvIWuT6t6): once
proven, identities cut over (Phase 2) and the Vultr box is decommissioned (Phase 3). Until
then both public routers exist side by side — `router-instance-1` stays the one laptop/phone
actually use.

| | `router-instance-1` | `router-aws-1` | `router-public-1` |
| --- | --- | --- | --- |
| Where | Vultr, alongside the controller | k3s pod, ns `ziti`, on-demand server `ip-10-90-5-163` | k3s pod, ns `ziti`, node labelled `gardenio.io/ziti-public-router=true` (static-EIP node, part b) |
| Role attribute | `public` | `private-aws` | `public` |
| Edge listener | `:8442`, public internet | `:3022` on `10.90.5.163`, VPC only | `:3022` on `ziti.clonk.tv`, public internet |
| Link listener | yes (public) | **no** — config built with `--private`, dials out | yes (public) — default bootstrap, no `--private` |
| Tunneler mode | `host` | `none` (not tunneler-enabled on the controller) | `host` (default, matches `router-instance-1`) |
| Who uses it | matthew-laptop, matthew-laptop-win, matthew-phone, and anything not in the VPC | `gardenio-hq`, `k3s-beads-host` | eventually the same as `router-instance-1`, once proven |
| Managed by | by hand on the Vultr box | `helm/ziti/templates/router-aws-1.yaml` | `helm/ziti/templates/router-public.yaml` |

**Why `router-aws-1` exists.** The HQ host and the k3s cluster are both in
us-east-1, in the same VPC, but until this router existed every dial between
them went HQ → Vultr → k3s and back. That is ~130 ms per round trip, and
`bd`/`gc` open a fresh connection and do tens of sequential round trips per
command. See the latency section of `beads-on-k3s.md` for the numbers.

**Why it is not `public`.** The `public` attribute is what
`all-endpoints-public-routers` (edge-router-policy, `#public` → `#all`) keys
on. Giving this router that attribute would offer a private, unreachable
address to the laptop and the phone, which would then waste connect attempts
on it. `private-aws` is admitted only by `hq-k3s-private-aws-routers`
(`#private-aws` → `@gardenio-hq`, `@k3s-beads-host`).

## Policies

```diagram
edge-router-policies
  all-endpoints-public-routers   #public        -> #all
  hq-k3s-private-aws-routers     #private-aws   -> @k3s-beads-host @gardenio-hq
  edge-router-8yyKLyyNa-system   @router-instance-1 -> @router-instance-1   (auto)

service-edge-router-policies
  all-routers-all-services       #all -> #all     (already admits #private-aws)
```

Only the edge-router-policy had to be added; the service-edge-router-policy is
`#all`/`#all` and needed nothing.

## Adding another router

1. On the controller (Vultr, `ssh vultr`, `export PATH=$HOME/.local/bin:$PATH`):

   ```sh
   ziti edge create edge-router <name> --role-attributes private-aws \
     -o /tmp/<name>.jwt
   ```

   The JWT is one-shot and expires in ~3 h. If you burn it (a failed start
   counts), mint a new one with `ziti edge re-enroll edge-router <name> -o …`.

2. Copy the manifest, change `ZITI_ROUTER_NAME`, `ZITI_ROUTER_ADVERTISED_ADDRESS`
   and `ZITI_ROUTER_IP_OVERRIDE`, then:

   ```sh
   kubectl -n ziti create secret generic <name>-jwt --from-file=router.jwt=/path/to.jwt
   # opt-in chart, off by default (gardenio/helm/ziti/README.md); copy templates/<name>.yaml for a new router
   helm upgrade --install gardenio-ziti gardenio/helm/ziti -n default --set ziti.enabled=true
   ```

3. `ziti edge list edge-routers` → ONLINE true. `ziti edge list terminators` →
   the hosting identities' terminators should move onto it (the tunnelers pick
   up a new router within a minute, but an already-established **bind** session
   stays where it is; delete the hosting pod to re-bind).

4. If the router is meant for a new set of identities, add an
   edge-router-policy for them; `#private-aws` already covers HQ and the
   beads host.

## Gotchas learned the hard way

- **`ZITI_ROUTER_IP_OVERRIDE` is mandatory when the advertised address is an
  IP.** `ziti create config router edge` logs *"DNS provided (10.90.5.163)
  appears to be an IP, ignoring for DNS entry"* and puts only `localhost` and
  the pod hostname in the CSR. The router then refuses to start:
  `identity is not valid for provided host: [10.90.5.163]`. Setting
  `ZITI_ROUTER_IP_OVERRIDE` to the same address adds the IP SAN.
- **`ZITI_ROUTER_MODE` defaults to `host`** and the router panics if the
  controller did not create it with `--tunneler-enabled`. We create plain
  routers, so the manifest sets `ZITI_ROUTER_MODE=none`.
- **Bootstrap is once-only by design.** `ZITI_BOOTSTRAP_CONFIG=true` /
  `ZITI_BOOTSTRAP_ENROLLMENT=true` act only when the config/cert are missing,
  which is what makes the PVC worth having: a pod restart re-uses the
  enrolment. To redo it, delete the PVC *and* re-enrol on the controller.
- **A `tcpSocket` probe on 3022 logs an error per poll**
  (`handshake failed: EOF`), because a bare TCP open is not a TLS handshake.
  The manifest uses an `exec` probe instead.
- **`hostNetwork: true`** is what makes the listener reachable at the node's
  private IP, and it pins the advertised address to that node. There is no
  private Route 53 zone in `modules/network`, so the address is the node IP in
  the manifest. **If the on-demand server is replaced, edit
  `ZITI_ROUTER_ADVERTISED_ADDRESS`/`ZITI_ROUTER_IP_OVERRIDE`, delete the PVC
  and re-enrol** — the old server certificate is bound to the old IP.
- Keep the router image at the controller's version. Controller is `v2.0.4`;
  the image is `openziti/ziti-router:2.0.4`. There is no floating `2.0` tag.
- **`router-public-1`'s node is not yet labelled.** Its nodeSelector
  (`gardenio.io/ziti-public-router: "true"`) is applied by the static-EIP
  node build (part b, gd-oBrWeIVcsd), owned separately from the manifest and
  SG rule in this doc. Until that node exists and carries the label, the
  Deployment sits Pending — expected, not a stranding bug like the
  `router-aws-1` toleration gotcha above.
- **`router-public-1` has no `ZITI_ROUTER_IP_OVERRIDE`.** Its advertised
  address is the DNS name `ziti.clonk.tv`, not an IP, and the override is
  only needed (per the gotcha above) when the advertised address *is* an IP.
  The DNS record tracking the router's current node is owned by part (b),
  not this manifest.
- Enrolment for `router-public-1` is blocked on gd-1hMSYnoW1d (the real
  in-cluster controller) — do not enrol it against the Vultr quickstart
  controller being replaced.

## Security group

`terraform/modules/k3s` →
`aws_vpc_security_group_ingress_rule.ziti_router`: TCP 3022 from
`var.vpc_cidr` (`10.90.0.0/16`) only, on the k3s node SG. No public ingress —
`router-aws-1` has no link listener, so nothing outside the VPC ever needs to
dial it. `vpc_cidr` is wired in from `module.network.vpc_cidr` in
`composition/gardenio`.

A second, additive rule —
`aws_vpc_security_group_ingress_rule.ziti_router_public`: TCP 3022 from
`0.0.0.0/0`, same SG — covers `router-public-1`, which unlike `router-aws-1`
*does* have a link listener and is meant to be dialled from the public
internet. It does not touch or narrow the VPC-only `ziti_router` rule above;
the two routers hostNetwork on the same port on two different nodes, so both
rules apply to the same SG without conflicting.

## What breaks if the Vultr controller is down

- **Existing sessions keep working.** The fabric is a data plane; routers and
  tunnelers hold their certificates and their established links, so
  `beads.ziti` and `gc.dashboard.ziti` keep answering for already-authorised
  identities.
- **New API sessions cannot be created.** A tunneler that restarts (or whose
  API session expires, ~30 min) cannot re-authenticate, so it stops dialling
  and hosting. In practice a controller outage longer than the session
  lifetime takes the whole fabric down.
- **No administration.** No new identities, routers, services or policies, and
  no enrolment or re-enrolment — the JWT is signed by the controller.
- **Certificate renewal stops.** Router and identity certs auto-renew through
  the controller; the router logs its next renewal (~1 year out), so this is
  a slow failure, not an immediate one.
- `router-aws-1` does not help here: it is enrolled with and controlled by the
  same single controller. The controller is the single point of failure for
  the whole mesh, and it is a single Vultr box. Making it HA (or at least
  restorable) is the obvious next piece of work.

**Restorable, now.** `ziti-controller-backup.md` covers the other half of that
sentence: `gardenio/scripts/ziti-ctrl-backup` pulls the raft DB and the PKI off
the box into S3, encrypted, and the restore has been exercised end to end
against a controller of the same shape. HA is still open; losing the box is no
longer unrecoverable.
