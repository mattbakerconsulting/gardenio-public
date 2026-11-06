# Grafana on k3s: operator runbook

How to reach, recover and reason about the `gardenio-grafana` Deployment on the
k3s cluster. Grafana is an **operator surface**: it is published at
`https://grafana.clonk.tv` behind the public-edge sign-in (plan
`gd-P78ygCUgA9`, pilot `gd-iS4Uxv9tfN`). It used to be ziti-only; that path is
superseded.

- Spec: [`docs/superpowers/specs/2026-09-21-grafana-enablement-design.md`](../../docs/superpowers/specs/2026-09-21-grafana-enablement-design.md)
  (decisions G1-G10 in §0 are cited below as `G<n>`; **G1 and G4 are superseded**
  by `gd-P78ygCUgA9`, the rest still hold).
- Plan: [`docs/superpowers/plans/2026-09-21-grafana-enablement.md`](../../docs/superpowers/plans/2026-09-21-grafana-enablement.md)
- Values: the `grafana:` and `grafanaBridge:` blocks of
  [`deploy-values/gardenio-k3s.yaml`](../../deploy-values/gardenio-k3s.yaml) —
  read its comments first, they carry the reasoning.
- Public edge (nginx server block, sign-in check): `grafana.clonk.tv` in
  [`gardenio/helm/gardenio-ziti/public-edge.yaml`](../helm/gardenio-ziti/public-edge.yaml).
- Legacy ziti service definitions (retired by `gd-xNMLrsgajy`):
  [`gardenio/helm/ziti/README.md`](../helm/ziti/README.md) (opt-in ziti overlay).
- Cluster access in general (kubeconfig, SSM, break-glass):
  [`k3s-cluster.md`](k3s-cluster.md).

> **Crews do not run `helm`, `kubectl` or `ziti` commands.** Everything below that
> mutates the cluster is an operator action; a crew requests it with an
> escalation. Read-only checks (`curl` against the public discovery URL or
> `https://grafana.clonk.tv/api/health` — the latter is behind the edge sign-in, so
> an unauthenticated `curl` gets a 302 to `/oauth2/start`, which itself proves the
> edge is up) are fine.

## 1. Reaching Grafana

```
https://grafana.clonk.tv
```

There are **two sign-ins in series**, and it matters which one is failing:

1. **The edge sign-in.** The public-edge nginx runs `auth_request` against the
   in-cluster oauth2-proxy (`gardenio-forwardauth`, Keycloak client
   `gardenio-traefik-forwardauth`) for every request to the host. No session →
   302 to `/oauth2/start` on the same host → Keycloak. A signed-in user who is not
   in the Keycloak group **`/gardenio-admins`** gets a **403 from the edge**; the
   request never reaches Grafana. Nothing in Grafana (including its local admin
   account) can fix that.
2. **Grafana's own Keycloak login** (`generic_oauth`, client `grafana`), done on top
   once the edge lets you through (§3).

Grafana has no Ingress, IngressRoute or LoadBalancer of its own
(`grafana.ingress.enabled: false`). The only public route is the edge's
`grafana.clonk.tv` server block, which proxies to `gardenio-grafana:3000`. The
Service is fenced by a NetworkPolicy so that only the edge, the viewer's
authproxy and the ziti host (until `gd-xNMLrsgajy`) can reach it.

**You need** a Keycloak user in `/gardenio-admins` (with MFA) and a browser. No
tunneler or device enrolment is involved.

Quick health check:

```sh
curl -s -o /dev/null -w '%{http_code}\n' https://grafana.clonk.tv/api/health
# 302 -> the edge is up and wants a sign-in (expected unauthenticated)
# signed-in browser session: {"database":"ok",...}
```

- **Connection error / TLS error / 5xx from the edge** — the edge, not Grafana.
  Check the edge node and `public-edge.yaml`'s nginx, and that the cert SAN list
  still includes `grafana.clonk.tv`.
- **Redirect loop, or 5xx on `/oauth2/*`** — the edge sign-in (oauth2-proxy /
  Keycloak) is broken. This also blocks the break-glass admin form below, because
  the edge answers before Grafana does. Use the port-forward path.
- **403 from the edge** — you are signed in but not in `/gardenio-admins`. Fix the
  group membership in Keycloak (operator).
- **Reaches Grafana, but `/api/health` is not `ok`** — it is Grafana. Go to §2
  (get in anyway) and then check the pod (§4, §5) from a kubeconfig.
- **Edge sign-in or edge down entirely** — you can still reach Grafana with
  `kubectl -n gardenio port-forward svc/gardenio-grafana 3000:3000` from any of the
  admin paths in [`k3s-cluster.md`](k3s-cluster.md#break-glass-admin-access-does-not-depend-on-openziti)
  (SSM port-forward to the API; none depend on ziti), then browse
  `http://localhost:3000`. OIDC login will not complete through a port-forward
  (`grafana.rootURL` and the redirect URI are `https://grafana.clonk.tv`) — use the
  local admin (§2).

> **Legacy ziti path.** `http://gardenio.grafana.ziti:3000` still answers for
> enrolled `#gc-admins` devices until the ziti path is retired (`gd-xNMLrsgajy`),
> but Grafana's root URL and OIDC redirect are now `https://grafana.clonk.tv`, so
> the OIDC login does not complete on that name. Do not rely on it.

## 2. Break-glass: the local admin account

Grafana keeps a local `admin` account as the steady-state fallback (G2, G6). It is
the way in when OIDC is broken, Keycloak is down, or you are on a port-forward.

- **Username:** `admin` (`grafana.adminUser`).
- **Password:** in the Kubernetes Secret **`gardenio-grafana-admin`**, namespace
  `gardenio`, key **`admin-password`** (G6). The operator creates it by hand before
  the first rollout; the chart never creates or rotates it (G7: no ExternalSecret
  on this cluster).

```sh
kubectl -n gardenio get secret gardenio-grafana-admin \
  -o jsonpath='{.data.admin-password}' | base64 -d; echo
```

Sign in at `https://grafana.clonk.tv` (after passing the edge sign-in, §1) or on
the port-forward, using the **username/password form**.
The form is on unless `grafana.auth.disableLoginForm` is `true`; it is `false` in
the k3s overlay on purpose, so that this path always works. Do not flip it to `true`
until OIDC has been exercised for a few days, and if you do, remember the local
admin still answers the HTTP API with basic auth.

Notes:

- If the Secret is missing, the Grafana pod sits in `CreateContainerConfigError`
  (the env is a `secretKeyRef`). It does **not** fall back to `admin`/`admin` —
  that fallback only applies when `adminPasswordSecret` is unset in values.
- Grafana reads the password at **first boot** to create the admin user in
  `grafana.db`. Changing the Secret afterwards does not change the password of an
  existing database. Reset it with
  `kubectl -n gardenio exec deploy/gardenio-grafana -- grafana cli admin reset-admin-password '<new>'`
  and update the Secret to match so the two do not drift.
- Never paste the password into a bead, comment or Slack thread.

## 3. When OIDC login fails

OIDC is Keycloak realm `gardenio`, client `grafana` (G2), reached through the edge
sign-in of §1 first — if you never see Grafana's own "Sign in with Keycloak"
button, you are in §1, not here. Grafana's
`auth_url` is followed **by the browser**, so it must be an address the browser can
reach; Grafana's token and userinfo calls are made server-side and can stay on the
in-cluster Service.

**First check, always — what does Keycloak's discovery document advertise?**

```sh
curl -s https://auth.clonk.tv/auth/realms/gardenio/.well-known/openid-configuration \
  | jq -r '.issuer, .authorization_endpoint'
```

| `authorization_endpoint` | Meaning |
|---|---|
| `https://auth.clonk.tv/auth/realms/gardenio/protocol/openid-connect/auth` | Healthy. Keycloak's browser-facing hostname is correct. Go to the checklist below. |
| `http://gardenio-keycloak-runtime.gardenio.svc.cluster.local:8081/auth/...` | **`KC_HOSTNAME` regressed.** Keycloak is advertising its in-cluster URL, which no browser can reach, so login is a loop/connection error, not a login. See below. |
| Connection error / 404 | The public edge (`public-edge.yaml`, nginx) is down or no longer serves `/auth/realms/gardenio/`. That is a Keycloak-edge outage, not a Grafana one. |

Fixing a `KC_HOSTNAME` regression: it is rendered from
`keycloak.runtime.hostnameUrl` (`https://auth.clonk.tv/auth`) plus
`keycloak.runtime.hostnameBackchannelDynamic: true` in
`deploy-values/gardenio-k3s.yaml`. If a values edit or a fresh overlay dropped
those lines, restore them and `helm upgrade` (operator). Restarting Keycloak alone
does not help. Note this changes the issuer for every client in the realm, not
just Grafana.

> The spec's §3 sketch (G4) put Keycloak on a second ziti service
> (`gardenio.keycloak.ziti:8080`). That was never deployed; Keycloak's browser-facing
> hostname is the public-edge name `auth.clonk.tv`. The invariant is unchanged:
> discovery must advertise a browser-reachable URL, never `*.svc.cluster.local`.

If discovery is healthy and login still fails, work down this list:

1. **Redirect URI mismatch** (Keycloak error page "Invalid parameter:
   redirect_uri"): the `grafana` client must allow
   `https://grafana.clonk.tv/login/generic_oauth`, and `grafana.rootURL` must be
   `https://grafana.clonk.tv` (declared under `keycloak.clients` in
   `deploy-values/gardenio-k3s.yaml`). If you reached Grafana through a
   port-forward, the legacy ziti name or any other hostname, this is expected —
   use `https://grafana.clonk.tv`.
2. **`invalid_client` / 401 on the token call** — the client secret in Secret
   `gardenio-grafana-oidc` (key exactly `client-secret`) does not match the
   Keycloak client's credential. Re-read it from Keycloak and recreate the Secret,
   then restart Grafana (env is read at start).
3. **Login succeeds but you land as Viewer, or are refused** — role mapping.
   `grafana.auth.oidc.roleAttributePath` maps the realm role `gardenio-admin`
   (carried by the `gardenio-admins` group) to `Admin`, everyone else to `Viewer`.
   `allowSignUp` is `false`: a Keycloak user with no matching Grafana user is not
   auto-created, so the first login of a new person needs an existing Grafana user
   (create it from the break-glass admin, matching on email).
4. **Grafana logs** — the real reason is usually there:
   `kubectl -n gardenio logs deploy/gardenio-grafana -c grafana | grep -i oauth`.
5. **Still stuck** — sign in with the break-glass account (§2); nothing about the
   dashboards or data depends on OIDC. (If the failure is at the edge, before
   Grafana renders anything, that account cannot help — see §1.)

## 4. What the PVC holds, and what is lost with the node

`grafana.persistence.enabled=true` is mandatory (G5): the root filesystem is
read-only, and `/var/lib/grafana` is the only writable path. It is a **1Gi
`local-path` PVC** — node-local, no replication — and Grafana is pinned to the
on-demand platform node (`gardenio.io/lifecycle=on-demand` **and**
`gardenio.io/role=platform`) so a spot reclaim does not take it.

If that node dies and its disk is gone, Grafana comes back with an empty
`grafana.db`. **Lost:**

- Grafana users and their preferences (including OIDC-created accounts),
- teams / org settings changed in the UI,
- ad-hoc dashboards, folders and annotations created by hand.

The `admin` account is re-created from the `gardenio-grafana-admin` Secret on the
next first boot, so keep that Secret (it lives in etcd, not on the Grafana PVC).

**Not lost:**

- **The 26 provisioned dashboards.** They are not in `grafana.db`; they arrive
  from the bridge as ConfigMaps (§5) and reappear on their own.
- **The metrics.** They live in `vmsingle` (30d, its own PVC), not in Grafana. The
  datasource is `http://gardenio-vmsingle:8428`, provisioned from config.

So: do not author boards you care about in the Grafana UI. Put them through the
bridge. Anything you must keep from the UI, export as JSON and bead it.

## 5. Dashboards come from the bridge, not from Grafana

(G8) The chart renders **no** dashboard ConfigMaps (they overflowed the 1 MiB
Helm release-Secret limit). The 26 JSONs under
`gardenio/helm/gardenio/dashboards/` are baked into the **`grafana-bridge`** image.
On start, with `grafanaBridge.backfill.enabled`, the bridge imports them as
`dashboard` beads (idempotent on `uid`) and writes each as a
`grafana_dashboard="1"` ConfigMap. The Grafana **sidecar** container watches those
ConfigMaps and provisions them into Grafana.

```
grafana-bridge image ─► dashboard beads ─► ConfigMaps (grafana_dashboard=1) ─► sidecar ─► Grafana
```

**Therefore a missing or stale dashboard is a bridge problem first, a Grafana problem
second.** Triage in this order:

1. Are the ConfigMaps there?
   `kubectl -n gardenio get cm -l grafana_dashboard=1` — expect 26.
   - **None / too few** → the bridge. `kubectl -n gardenio logs deploy/gardenio-grafana-bridge`,
     and check the pod is Running (an `ImagePullBackOff` means the
     `grafana-bridge` image tag was never published for the release in use; the
     tag follows `global.imageTag`). Confirm `grafanaBridge.enabled` and
     `grafanaBridge.backfill.enabled` are both true in the deployed values.
   - **All 26 present** → step 2.
2. Is the sidecar healthy?
   `kubectl -n gardenio logs deploy/gardenio-grafana -c grafana-sc-dashboard`.
   The sidecar needs the ServiceAccount/Role/RoleBinding the chart renders when
   `grafana.sidecar.enabled` is true; an RBAC `forbidden` in its log means that
   binding is missing.
3. Only then look at Grafana itself.

Editing a dashboard in the Grafana UI does not change what the bridge ships; to
change one, change the JSON (or the dashboard bead) and let the bridge re-deliver.
The bridge runs **write-only** here (`grafanaBridge.grafana.url` is empty), so UI
edits are not read back.

## 6. `grafana.enabled` rolls the controller

The controller Deployment injects `GRAFANA_URL` whenever `grafana.enabled` is true
(`templates/controller/deployment.yaml`, spec §2 item 4). Any `helm upgrade` that
**flips** `grafana.enabled` therefore **restarts the controller** (`gardenio-agents`),
not just Grafana. Schedule the flip like any controller roll:

- not while a migration, a drain or a mass respawn is mid-flight;
- expect agent pods to be re-reconciled and live sessions to blip;
- re-run `gb news` right before the upgrade.

Changes to other `grafana.*` keys (persistence, OIDC, sidecar) roll only the
Grafana pod. Rolling back by setting `grafana.enabled: false` rolls the controller
again, and removes the PVC template from the release — check what happened to the
claim (`kubectl -n gardenio get pvc`) before assuming `grafana.db` survives.

## 7. Operator-only actions (each is an escalation from a crew)

| Action | Who | Notes |
|---|---|---|
| Create Secret `gardenio-grafana-admin` (key `admin-password`) | operator | before the first rollout (G6) |
| Create Secret `gardenio-grafana-oidc` (key `client-secret`) | operator | secret handed over out of band |
| Create / edit the Keycloak `grafana` client | operator | redirect URI in §3 |
| Add or remove users in the Keycloak group `/gardenio-admins` | operator | gates the edge sign-in (§1) |
| Edit / apply `public-edge.yaml` (nginx, cert SAN) | operator | the `grafana.clonk.tv` server block |
| `helm upgrade` | operator | via the usual release script; pass `--set global.imageTag=<calver>` |
| Retire the legacy `gardenio-grafana` ziti service | operator | `gd-xNMLrsgajy`; ziti is off by default and being deprecated (`gd-P78ygCUgA9`) |

Guard rails that apply to every change here: never enable any
`*.externalSecret.enabled` (G7 — no ESO, it fails the whole install), never render
an Ingress or LoadBalancer for Grafana (the only public route is the edge nginx
block; G1's "ziti only" is superseded but "no chart-rendered public route" stands), and note that `values.schema.json`
does not validate the `grafana:` block, so a misspelled key is silently ignored —
chart tests are the only guard.

## 8. Out of scope

vmalert, alertmanager, VictoriaLogs (and its datasource), traces, `grafana.llm.*`,
`grafana.assistant.*`, the image renderer, mcp-grafana, plugins, and the external
ACME-estate `grafanaApiKey` path (G10). `gb logs` failing for crews is
`gd-q0Sy8rdaLc` (caused by `vlsingle` being off) and is deliberately separate.
