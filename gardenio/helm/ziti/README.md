# gardenio-ziti-overlay — OpenZiti access overlay (off by default)

Every OpenZiti manifest gardenio ever ran lives here as a template, still
renders, and is tested — but **nothing deploys it unless `ziti.enabled=true`**.

## Why it is off

The owner decided on 2026-10-13 to keep ziti "as a possibility for
configuration in the future, but not use it in the stack today" (plan
`ziti-to-keycloak`, gd-P78ygCUgA9). Access moves to Keycloak-authenticated
public hostnames; the in-cluster ziti controller, routers and host pods are
removed from the live cluster by gd-xNMLrsgajy. This chart is how the removal
stays reversible: the manifests did not get deleted, they got a switch.

These manifests used to be applied by hand with `kubectl apply -f`; no Helm
release ever owned them. After this change they are not applied at all.

## What is in it

| Template | Switch (all under `ziti.`) | Objects |
|---|---|---|
| `namespace.yaml` | `enabled` | the `ziti` Namespace, declared once |
| `controller.yaml`, `controller-config.yaml` | `controller.enabled` | in-cluster ziti controller (Deployment, PDB, Service, Certificate, config) |
| `router-aws-1.yaml` | `routerAws.enabled` | private edge router inside the VPC |
| `router-public.yaml` | `routerPublic.enabled` | public edge router |
| `coredns-split-horizon.yaml` | `corednsSplitHorizon.enabled` | `kube-system/coredns-custom` rewrite for `ziti-ctrl.clonk.tv` |
| `ziti-host-gardenio.yaml` | `hostGardenio.enabled` | `gardenio/ziti-host`, binds the gardenio ziti services |
| `ziti-host-beads.yaml` | `hostBeads.enabled` (default **false**) | `beads/ziti-host`, binds the Dolt service |

Every component also needs `ziti.enabled=true`; with the default values the
chart renders zero documents.

## Turning ziti back on

1. **Terraform.** Set `ziti_public_router_enabled` and `ziti_router_vpc_enabled`
   (the security-group ingress and the public router's static address live in
   `gardenio/terraform/modules/k3s`) and apply.
2. **Controller and identities.** The Vultr controller stays running by owner
   decision 2026-10-13 and still holds the enrolled identities, services and
   policies (`k3s-gardenio-host`, `router-aws-1`, laptop identities;
   `router-public-1` was enrolled against the in-cluster controller). See
   "Where the ziti controller lives" below. To use the in-cluster controller
   instead, restore it from backup (`docs/ziti-controller-backup.md`) and enrol
   the routers (`docs/ziti-routers.md`). The host pods need their `ziti-identity` Secret
   ("Rebuilding the ziti side from scratch" below).
3. **Render, then install.**

   ```sh
   helm template gardenio-ziti gardenio/helm/ziti --set ziti.enabled=true | less   # review first
   helm upgrade --install gardenio-ziti gardenio/helm/ziti -n default --set ziti.enabled=true
   # pick components: --set ziti.routerPublic.enabled=false, --set ziti.hostBeads.enabled=true
   ```

   Install the release into a namespace other than `ziti`: the chart declares
   the `ziti` Namespace itself, so `-n ziti --create-namespace` would clash with
   it. Objects that were once applied by hand carry no Helm ownership, so
   `helm install` refuses to adopt them; delete them first or annotate them for
   adoption.
4. **Gardenio chart.** The gardenio chart's own ziti switches stay off
   (`zitiBrowser.enabled`, `agents.zitiAccess.enabled`). `kd` and `gb` still
   dial in process: `--ziti-identity`, `--ziti-service`, `--ziti-tls-server-name`
   or `GARDENIO_ZITI_IDENTITY`, `GARDENIO_ZITI_SERVICE`,
   `GARDENIO_ZITI_TLS_SERVER_NAME`.
5. **The backends the ziti services land on.** The switch-off (gd-xNMLrsgajy)
   also removed what the host pods dialled, so the viewer, mux and Headlamp
   services have nothing to reach until these return: revert that change to
   `gardenio/helm/gardenio-ziti/viewer-authproxy.yaml` (listeners 8095, 8080,
   8443, 8446, 8447, their Service ports and TLS volumes, and the `ziti-host`
   rule of `viewer-authproxy-ingress`) and to `headlamp-networkpolicy.yaml`
   (the `ziti-host` peer), and `kubectl apply` both. In deploy-values set
   `edgeBackends.zitiHostSelector: {app: ziti-host}` and add `app: ziti-host`
   back to `coopmux.networkPolicy.extraFrom`, then roll. The Grafana ziti name
   also needs its redirect URI and web origin back on the Keycloak `grafana`
   client. In `scripts/edge-probe/expectations.json` set `ziti_public_router`
   true and put `ziti.clonk.tv` back in `non_http_hosts`.

## Where the ziti controller lives

On the Vultr box (`45-77-68-134.nip.io`, client and management API on `:8440`,
with its own `router-instance-1` on `:8442`), not in AWS. The owner decided on
2026-10-13 to keep that box running ("it has some other stuff on it"; decision
gd-xQZlM5hEx8): it also holds Gas City's frozen rollback city and
gc-dashboard. gardenio stopped depending on it (gd-K8d2mKf6Ey) but did not
change it: the k3s identities, services and policies there (`k3s-gardenio-host`,
`k3s-beads-host`, `router-aws-1`, the operator's device identities, the nine
services below) were left in place.

**It is used only when `ziti.enabled=true`.** Nothing on gardenio's default
path dials it: not the public edge, not the edge probes
(`scripts/edge-probe/`, `gardenio/terraform/modules/edge-probes/`), not
`deploy-values/gardenio-k3s.yaml`, not CI. Turning this overlay on reconnects
to it: `router-aws-1` enrols against `45-77-68-134.nip.io`
(`ZITI_CTRL_ADVERTISED_ADDRESS` in `templates/router-aws-1.yaml`), and the host
pods' `ziti-identity` Secrets are identities that controller signed.

The alternative is the in-k3s controller, `templates/controller.yaml`
(`ziti.controller.enabled`, gd-Vn8uWTbQ9S): a fresh, independent network with
its own PKI, so every identity and router must be enrolled against it
(`docs/ziti-controller-k3s.md`, `docs/ziti-routers.md`; restore from
`docs/ziti-controller-backup.md`). `templates/router-public.yaml` already points
at it.

Gas City's own Vultr paths (the HQ host's tunneler in `docs/hq-host.md`, the
frozen city, mail to it) are Gas City's, not this chart's.

## The nine ziti services

> **Used only when `ziti.enabled=true`.** Moved here from
> `../gardenio-ziti/README.md` (gd-K8d2mKf6Ey). Ziti is switched off
> (gd-xNMLrsgajy, plan `ziti-to-keycloak` gd-P78ygCUgA9): no ziti controller,
> router or host pod runs in the cluster, `ziti.clonk.tv` and the router's 3022
> rules are gone, and the backends below that only ziti reached
> (viewer-authproxy 8095/8443/8080/8446/8447) were removed. Every name here is
> now a public-edge host behind the Keycloak sign-in
> (`../gardenio-ziti/public-edge.yaml`). The Vultr controller still holds these
> services and the gardenio identities, so ziti can come back as it was ("Turning
> ziti back on" above). The table is what that would restore.

Created on the Vultr controller ("Where the ziti controller lives" above; CLI at
`/home/claude/.local/bin/ziti` as user `claude` — it is NOT on the default
non-interactive PATH, which is why `command -v ziti` over ssh comes back empty).

| Service | Intercept | Backed by | Dialable by |
|---|---|---|---|
| **`gardenio-frontdoor`** | **`gardenio.clonk.tv:443`** | `viewer-authproxy:8443` (TLS) | `#gc-admins` |
| `gardenio-frontdoor-http` | `gardenio.clonk.tv:80` | `viewer-authproxy:8080` (301 → https) | `#gc-admins` |
| `gardenio-viewer` | `gardenio.viewer.ziti:8095` | **`viewer-authproxy:8095`** (not the viewer directly) | `#gc-admins` |
| `gardenio-beads` | `gardenio.beads.ziti:8080` | `gardenio-beads:8080` | `#gc-clients` |
| `gardenio-beads-grpc` | `gardenio.beads-grpc.ziti:9090` | `gardenio-beads:9090` | `#gc-clients` |
| `gardenio-grafana` | `gardenio.grafana.ziti:3000` | `gardenio-grafana:3000` | `#gc-admins` |
| `gardenio-k3s-api` | `gardenio.k3s.ziti:6443` | `kubernetes.default.svc:443` | `#gc-admins` |
| `gardenio-controller` | `gardenio.controller.ziti:8091` | `gardenio-agents:8091` | `#gc-admins` |
| `gardenio-pg` | `gardenio.pg.ziti:5432` | `gardenio-postgres:5432` | `#gc-admins` |

Keycloak is deliberately **not** a ziti service — its browser-facing hostname
is `https://auth.clonk.tv/auth`, served by `../gardenio-ziti/public-edge.yaml`
(realm `gardenio` paths only).

Two tiers. `#gc-admins` is `matthew-laptop`, `matthew-laptop-win` and
`matthew-phone` — all three of the operator's own devices, so an admin-tier
service is reachable from the phone as well.

Note `gardenio-frontdoor-http` is on the SAME tier as the https service on
purpose. A redirect nobody is authorised to reach is not a fix: the point is
that typing the bare hostname works, so the identity must hold both.

`#gc-clients` additionally contains a leftover **`test-client`** identity, which
is why the Kubernetes API, the controller's mutating endpoints, the database and
the front door are on the admin tier rather than granted to everything. That
identity should probably be deleted; it is the only credential here that is not
one of the operator's devices.

### kubectl over ziti (history)

> **History.** `~/.kube/gardenio-ziti.yaml` pointed at `gardenio.k3s.ziti:6443`
> through the `gardenio-k3s-api` service; it was retired with ziti
> (gd-xNMLrsgajy). kubectl now reaches the cluster at `k3s.clonk.tv:6443`
> (`admin_cidrs`) or over SSM.

It pinned `tls-server-name: k3s.clonk.tv`, so certificate verification matched
a real SAN instead of being skipped (the k3s serving cert carries
`k3s.clonk.tv`). It ran through the same `ziti-host` pod as everything else, so
restarting that pod cut the operator's own kubectl; the public kubeconfig
(`~/.kube/gardenio.yaml` via `k3s.clonk.tv:6443`) was always kept as the way
back in, which is why `admin_cidrs` was never emptied.

### Rebuilding the ziti side from scratch

The ziti manifests (controller, routers, CoreDNS split horizon, the host pods)
are this chart; "Turning ziti back on" above has the Terraform flags. The steps
below are the identity and policy half on the controller. They run on the Vultr
box, so they are the operator's (crew sessions have no `ssh vultr`), and only
when ziti is being turned back on.

```sh
# ---- on the controller (ssh vultr, user claude) ----
export PATH=$HOME/.local/bin:$PATH

ziti edge create identity k3s-gardenio-host -a gardenio-hosts \
  -o /home/claude/ziti/enrollments/k3s-gardenio-host.jwt   # NOTE: ziti/, not $HOME

# one host.v1 + one intercept.v1 per service, e.g.
ziti edge create config gardenio-viewer-host host.v1 \
  '{"protocol":"tcp","address":"viewer-authproxy.gardenio.svc.cluster.local","port":8095}'
ziti edge create config gardenio-viewer-intercept intercept.v1 \
  '{"protocols":["tcp"],"addresses":["gardenio.viewer.ziti"],"portRanges":[{"low":8095,"high":8095}]}'
ziti edge create service gardenio-viewer \
  --configs gardenio-viewer-host,gardenio-viewer-intercept
ziti edge create service-policy gardenio-viewer-bind Bind \
  --service-roles '@gardenio-viewer' --identity-roles '#gardenio-hosts'
ziti edge create service-policy gardenio-viewer-dial Dial \
  --service-roles '@gardenio-viewer' --identity-roles '#gc-admins'

# gardenio-grafana (Grafana epic gd-cz4Jm2DebY): same shape, admin tier.
# Grafana is published at https://grafana.clonk.tv by the public edge; this
# ziti service is only for a cluster that turns ziti back on. No second Bind identity and no Deployment change — the existing
# k3s-gardenio-host session picks the new service up on its own.
ziti edge create config gardenio-grafana-host host.v1 \
  '{"protocol":"tcp","address":"gardenio-grafana.gardenio.svc.cluster.local","port":3000}'
ziti edge create config gardenio-grafana-intercept intercept.v1 \
  '{"protocols":["tcp"],"addresses":["gardenio.grafana.ziti"],"portRanges":[{"low":3000,"high":3000}]}'
ziti edge create service gardenio-grafana \
  --configs gardenio-grafana-host,gardenio-grafana-intercept
ziti edge create service-policy gardenio-grafana-bind Bind \
  --service-roles '@gardenio-grafana' --identity-roles '#gardenio-hosts'
ziti edge create service-policy gardenio-grafana-dial Dial \
  --service-roles '@gardenio-grafana' --identity-roles '#gc-admins'

# REQUIRED, easily missed: the in-AWS router admits NAMED identities only.
# Without this the host still works but hairpins out to Vultr and back.
ziti edge update edge-router-policy hq-k3s-private-aws-routers \
  --identity-roles '@k3s-beads-host,@gardenio-hq,@k3s-gardenio-host'
# This rule is per IDENTITY, not per service: gardenio-grafana is bound by the
# same k3s-gardenio-host, so an existing install needs no change here. It is
# in the recipe so a rebuild from scratch does not forget it.

# ---- on the laptop ----
ziti-edge-tunnel enroll -j k3s-gardenio-host.jwt -i k3s-gardenio-host.json
kubectl -n gardenio create secret generic ziti-identity \
  --from-file=k3s-gardenio-host.json
shred -u k3s-gardenio-host.json k3s-gardenio-host.jwt   # enrolled cert lives in the cluster now

# the ziti-host Deployment is this chart's ziti-host-gardenio.yaml:
helm upgrade --install gardenio-ziti gardenio/helm/ziti -n default --set ziti.enabled=true
```

`ziti edge list` **paginates at 10 rows by default** — pass `'limit 500'` as a
positional filter (not `--limit`, which silently returns nothing) or an audit
will quietly miss half the policies.

### Alternative server certs for BrowZer (controller-side)

Task gd-wTCD9MhoT0 (epic gd-akt1qKz1Si, decision gd-ipJJAwZ8EY). BrowZer's
bootstrapper is a plain browser client with no ziti CA trust bundle, so it
cannot validate the controller's normal ziti-CA-signed
`identity.server_cert`. The fix is `identity.alt_server_certs`: a second
`server_cert`/`server_key` pair the controller presents via SNI, **added
alongside** the existing identity block, never replacing it — every
already-enrolled identity (laptop, phone, k3s hosts) keeps validating the
original cert unchanged.

```yaml
# controller config.yml — ADD under the existing identity: block.
identity:
  cert:        /path/to/existing/ctrl-client.chain.pem   # unchanged
  server_cert: /path/to/existing/ctrl-server.chain.pem   # unchanged (ziti CA)
  server_key:  /path/to/existing/ctrl-server.key         # unchanged
  ca:          /path/to/existing/ctrl.cas.pem             # unchanged
  key:         /path/to/existing/ctrl-client.key          # unchanged
  alt_server_certs:
    - server_cert: /home/claude/ziti/alt-certs/45-77-68-134.nip.io.chain.pem
      server_key:  /home/claude/ziti/alt-certs/45-77-68-134.nip.io.key
```
Sources: OpenZiti controller config reference, `identity` section
("alternate server certificates and keys to support SNI on hosted
services"); github.com/openziti/helm-charts#159; openziti.discourse.group
`/t/alt-server-certs-not-working-properly/4744` and
`/t/right-place-for-the-alt-server-cert-in-router-config/2815`. Full trail:
spike gd-NzK41IpRne, decision gd-ipJJAwZ8EY.

**Hostname is `45-77-68-134.nip.io`, not a new DNS name.** nip.io is a public
wildcard resolver that already maps `<ip-with-dashes>.nip.io` to the embedded
IP — this doc already names the controller's edge API that way (see "The
nine ziti services" above) and it already resolves publicly. No new
A/AAAA record is needed, and this is deliberately **not** the same public
name as the viewer front door (`gardenio.clonk.tv`, gd-xZOjaRfA9P) — the
controller's public identity is independent of the viewer's.

**Getting a publicly-trusted cert for a nip.io name.** Nobody owns the
`nip.io` zone, so DNS-01 is not available; issue via HTTP-01
(`certbot certonly --standalone -d 45-77-68-134.nip.io`, needs port 80 free
during issuance) and add a renewal timer on the box — there is no
cert-manager here, that's the in-cluster `../gardenio-ziti/tls-clusterissuer.yaml` pattern,
which does not run on a bare Vultr VM.

**`alt_server_certs` changes which cert TLS presents, not which APIs a port
serves — do not let it leak the management API.** The controller's existing
bind point (`45-77-68-134.nip.io:8440`) serves all three API surfaces
(`edge-client`, `edge-management`, `fabric`). If the alt cert is presented on
that same bind point, BrowZer's public reachability drags the management API
along with it — exactly what the epic's trust boundary forbids
("`gardenio-controller`... keep ziti-only"). The safe shape is a **second**
`web` bind point, restricted to `edge-client` only:

```yaml
web:
  - name: internal-all-apis              # existing bind point, UNCHANGED
    bindPoints:
      - interface: 0.0.0.0:8440
        address: 45-77-68-134.nip.io:8440
    apis:
      - binding: edge-client
      - binding: edge-management
      - binding: fabric
  - name: browzer-client-only            # NEW
    bindPoints:
      - interface: 0.0.0.0:8441
        address: 45-77-68-134.nip.io:8441
    apis:
      - binding: edge-client             # nothing else
```

`ZITI_CONTROLLER_HOST`/`ZITI_CONTROLLER_PORT` on the bootstrapper then point
at `45-77-68-134.nip.io:8441` — **that is the one port this task makes newly
reachable from the bootstrapper's network: 8441, edge-client API only.**
Port 8440 (all three APIs, including management) is unchanged and still only
reachable through the ziti overlay path it uses today.

The exact bind-point layout above must be read off the live `config.yml`
before applying — no crew session has `ssh vultr` (working-rules
gd-oI2AIaSzjF), so this is the documented mechanism, not a diff taken against
the running file. See escalation gd-MgYXsp6B3b's sibling for this task for
the apply + verify request.

**Verifying after the change, from off-box with no ziti client installed:**
```sh
openssl s_client -connect 45-77-68-134.nip.io:8441 \
  -servername 45-77-68-134.nip.io </dev/null 2>/dev/null | openssl x509 -noout -issuer
#   expect Let's Encrypt, NOT the ziti CA

curl -sk https://45-77-68-134.nip.io:8441/edge/management/v1/ -o /dev/null -w '%{http_code}\n'
#   expect connection refused, not a management API response

# then re-enroll/reconnect one already-enrolled tunneler (laptop, phone, or a
# k3s host identity) against :8440 unchanged, to confirm nothing regressed.
```

## Tests

```sh
bash gardenio/helm/ziti/tests/render_test.sh   # needs helm and yq
```

It asserts: default values render nothing; `ziti.enabled=true` renders exactly
the expected `kind/namespace/name` set; `hostBeads` adds the beads host; the
rendered documents equal the template files' own content (the `{{ if }}`
wrappers changed nothing); and a per-component switch drops only that
component. CI runs it in the `helm` job of `checks.yml`
(`gardenio/helm/ziti/**` is in that job's path filter). The gardenio chart's
side — the live overlay renders no ziti access wiring — is
`gardenio/helm/gardenio/tests/ziti_off_by_default_test.go`.
