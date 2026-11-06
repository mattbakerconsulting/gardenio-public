# gardenio on k3s — access, auth edge and registry plumbing

The manifests here are what makes the `gardenio` release reachable and able to
pull images. The chart itself is installed from `deploy-values/gardenio-k3s.yaml`;
read that file's comments first, they carry the reasoning for every non-default
value.

**Front door: https://gardenio.clonk.tv**

**gardenio has two public endpoints, both narrow:** hooks.clonk.tv (GitHub
webhooks only, SG-allowlisted to GitHub's hook ranges) and the public edge
(`public-edge.yaml`): Keycloak's realm `gardenio` sign-in at auth.clonk.tv, and
every human-facing host (the viewer at gardenio.clonk.tv, mux, Grafana,
Headlamp, the daemon API, the ClaimLayer demo) behind the Keycloak sign-in
check. Everything else: no Ingress, no LoadBalancer Service, no public DNS
record. OpenZiti, which used to be the only way in, is switched off
(gd-xNMLrsgajy); the opt-in chart `../ziti` keeps it configurable.

Manifests are plain YAML applied with `kubectl`, matching the house style set by
`gardenio/helm/beads/` rather than a second Helm chart.

| File | What it is |
|---|---|
| `../ziti/templates/ziti-host-gardenio.yaml` | binds the gardenio ziti services, host mode only (opt-in `helm/ziti` chart, off by default and not running since gd-xNMLrsgajy) |
| `viewer-authproxy.yaml` | turns the identity the public edge verified into the viewer's and coopmux's trust headers |
| `ecr-token-refresh.yaml` | keeps the ECR pull secret alive from the node role |
| `tls-clusterissuer.yaml` | Let's Encrypt DNS-01 issuer + the certificates (the two ziti front-door ones are unused since gd-xNMLrsgajy) |
| `public-edge.yaml` | the ONE public human-facing HTTPS edge: claimlayer.clonk.tv + auth.clonk.tv (realm gardenio paths only) + grafana.clonk.tv + dashboard.clonk.tv (Headlamp) + gardenio.clonk.tv (viewer) (behind the edge sign-in) + api.clonk.tv (the daemon API for kd/gb, bearer-JWT check, and the per-land door /land/<name>/v1/*), hostPort 443 on the ziti edge node (gd-NoOR0riqwi) |
| `headlamp-values.yaml` | helm values for the `headlamp` release (namespace `headlamp`) behind dashboard.clonk.tv, see "Headlamp" below |
| `headlamp-rbac.yaml` | binds Headlamp's ServiceAccount to the builtin `view` ClusterRole (no Secrets) |
| `headlamp-networkpolicy.yaml` | only `public-edge` may reach Headlamp |
| `gc-k8s-live-serviceaccount.yaml` | the dedicated SA for the privileged k8s-live role — see "The k8s-live role" below |

---

## Ziti (switched off)

OpenZiti is switched off (gd-xNMLrsgajy, plan `ziti-to-keycloak`
gd-P78ygCUgA9): no ziti controller, router or host pod runs in the cluster, and
nothing on this default path (the public edge, the probes, `deploy-values/`)
dials a ziti controller. Every name the ziti services used to carry is now a
public-edge host behind the Keycloak sign-in (`public-edge.yaml`); kd and gb use
api.clonk.tv, kubectl uses `k3s.clonk.tv` (admin CIDRs) or SSM.

Grafana is published at `https://grafana.clonk.tv` behind the edge sign-in
(`public-edge.yaml`, plan `gd-P78ygCUgA9`; spec G1 is superseded). Day-2
operation (break-glass, OIDC failures, what the PVC loses):
[`../../docs/grafana-operator-runbook.md`](../../docs/grafana-operator-runbook.md).

The nine ziti services, the controller-side rebuild recipe and the BrowZer
alt-server-cert notes moved to [`../ziti/README.md`](../ziti/README.md) with the
opt-in chart: they apply only when `ziti.enabled=true`, and that file says
where the controller lives.

---

## Why `viewer-authproxy` exists

beads-viewer fails closed. With neither OIDC nor an AVA trust signer it refuses
to start at all:

```
OIDC_ISSUER_URL and OIDC_CLIENT_ID are required when AVA_TRUST_SIGNER is unset
(no basic-auth fallback); set BEADS_VIEWER_DEV_AUTH_BYPASS=1 for local dev
```

Under `devAuthBypass` it renders read-only pages but derives operator identity
from exactly one source — the `X-Auth-Request-Email` header — which
`cmd/beads-viewer/dev_auth.go` documents as trustworthy *only* behind a
forwardauth edge that overwrites it from a verified auth response. With no edge
there is no identity, so every mutating endpoint returns **401** and the
"+ New Agent" button silently does nothing.

`viewer-authproxy` sits between the public edge and the viewer. The edge
(`public-edge.yaml`) runs the oauth2-proxy sign-in check and overwrites
`X-Auth-Request-Email` and the access token from the verified reply; the proxy
passes that identity on, adds the edge secret below and builds the bearer.
`proxy_set_header` overwrites rather than appends, so a client cannot forge an
identity by sending its own header.

Until gd-xNMLrsgajy it also had ziti listeners (8095 break-glass, 8080/8443 for
gardenio.clonk.tv, 8446/8447 for mux.clonk.tv) that stamped a FIXED operator
email, because there the ziti identity was the credential. They were removed
with ziti. There is no break-glass door now: with Keycloak or forwardauth down,
`kubectl -n gardenio port-forward svc/gardenio-beads-viewer 8095` still reads
(no identity, so no writes).

**The viewer Service is also reachable in-cluster, so the edge proves itself
(gd-kRyMsWiDNZ).** The edge is not the only way to the viewer: any pod can
dial `gardenio-beads-viewer:8095` directly (the k3s overlay has no NetworkPolicy)
and forge `X-Auth-Request-Email`. So the proxy also stamps
`X-Authproxy-Secret` from the `viewer-authproxy-secret` Secret, and the viewer —
with `beadsViewer.devAuthProxySecret.secretName` set — promotes the email header
only when that secret matches. A direct caller gets no identity, as if it had sent
no header.

**The proxy itself is fenced too.** Its two listeners trust the email they are
sent and add the valid secret (`:8097`) or the admin coopmux token (`:8448`), so a
pod that could reach them would be whoever it named. `viewer-authproxy.yaml`
therefore ends with a `NetworkPolicy` (`viewer-authproxy-ingress`) admitting only
the `public-edge` namespace on 8097/8448. k3s enforces it (embedded kube-router
netpol, not disabled in `node.sh.tftpl`) even though the chart's
`networkPolicies` are off; the file is applied by hand, not by helm.

**The listeners (S4, gd-UqhR5d6qPt).** `:8097` (viewer) and `:8448` (mux) serve
the public-edge nginx (`gardenio.clonk.tv` and `mux.clonk.tv` from the internet, after the
oauth2-proxy `/gardenio-admins` check; mux was held ziti-only, gd-1bizVpXrfk, until
the owner answered publish, gd-jfXRKAn6Ez, S4b gd-0e6aSFhxM5). They stamp no fixed email: they
pass on the `X-Auth-Request-Email` the edge sets (401 when empty), add the edge secret
and the user's own access token (viewer) or the coopmux bearer (mux). The
`viewer-authproxy-ingress` policy admits only the `public-edge` namespace on those two
ports, and that policy is the whole trust. `:8448` also blanks `X-Operator`, `X-Auth-Audit-Id`
and every `X-Edge-Auth-*` header (coopmux has no `DAEMON_HMAC_KEY` and would trust them), and
the `mux.clonk.tv` edge block refuses any request whose `Origin` is not `https://mux.clonk.tv`
(403; the lax session cookie rides along from sibling `*.clonk.tv` pages, so without it one
could open `wss://mux.clonk.tv/ws/mux`) and sends `X-Frame-Options: DENY` plus
`frame-ancestors 'none'`. Operator
steps: `kubectl apply -f viewer-authproxy.yaml`, `kubectl -n gardenio rollout restart
deploy/viewer-authproxy`, then the Terraform records and `public-edge.yaml` (see the
task's escalation).

**Rollout** (order matters: the reverse strips operator identity until the proxy
catches up). Run as a script file so a failure stops it, and never put the secret
on argv or in the output:

```bash
set -euo pipefail
AWSP=(--profile gardenio --region us-east-1); NS=gardenio; NAME=viewer-authproxy-secret
# 0. neither copy may exist yet (rotation: put-secret-value + delete/create, never apply)
kubectl -n $NS get secret $NAME -o name 2>/dev/null && { echo "k8s Secret exists: stop"; exit 1; }
aws "${AWSP[@]}" secretsmanager describe-secret --secret-id gardenio/$NAME >/dev/null 2>&1 \
  && { echo "SM secret exists: stop"; exit 1; }
# 1. mint into the cluster; the value is never echoed
S=$(openssl rand -hex 32); [[ $S =~ ^[0-9a-f]{64}$ ]]
kubectl -n $NS create secret generic $NAME --from-literal=secret="$S" >/dev/null; unset S
# 2. durable copy in Secrets Manager, same JSON key
kubectl -n $NS get secret $NAME -o json | jq -c '.data|map_values(@base64d)' \
  | aws "${AWSP[@]}" secretsmanager create-secret --name gardenio/$NAME \
      --description "viewer-authproxy edge secret (X-Authproxy-Secret), gd-kRyMsWiDNZ" \
      --secret-string file:///dev/stdin --query Name --output text
# 3. sha256 match (prints only match/MISMATCH)
a=$(kubectl -n $NS get secret $NAME -o json | jq -j '.data.secret|@base64d' | sha256sum | cut -d' ' -f1)
b=$(aws "${AWSP[@]}" secretsmanager get-secret-value --secret-id gardenio/$NAME \
      --query SecretString --output text | jq -j .secret | sha256sum | cut -d' ' -f1)
[ "$a" = "$b" ] && echo match || { echo MISMATCH; exit 1; }; unset a b
# 4. roll the proxy (stamps the header and applies the NetworkPolicy)
kubectl apply -f viewer-authproxy.yaml
kubectl -n $NS rollout status deploy/viewer-authproxy --timeout=180s
# header rendered with a 64-hex value in the viewer block (count only; expect 1)
kubectl -n $NS exec deploy/viewer-authproxy -- \
  grep -cE 'X-Authproxy-Secret "[0-9a-f]{64}"' /etc/nginx/conf.d/default.conf
```

Then `helm upgrade` the viewer with `beadsViewer.devAuthProxySecret.secretName:
viewer-authproxy-secret`. Verify:

```bash
# positive: the front door. An unauthenticated curl gets 302 -> /oauth2/start
# (expected, sign-in is required); the identity check is a SIGNED-IN BROWSER load of
# https://gardenio.clonk.tv/me, which must redirect to /u/<your-slug>. If it
# shows /login instead, the 8097 block is not stamping X-Authproxy-Secret.
curl -s -o /dev/null -w '%{http_code} %{redirect_url}\n' https://gardenio.clonk.tv/me
# negative, from an ordinary crew pod:
#  a) forged header straight to the viewer -> 302 /login?next=/me
curl -s -o /dev/null -w '%{http_code} %{redirect_url}\n' -H 'X-Auth-Request-Email: matthewbaker1110@gmail.com' \
  http://gardenio-beads-viewer.gardenio.svc.cluster.local:8095/me
#  b) through the proxy -> must time out (000), not 308
curl -s -m 5 -o /dev/null -w '%{http_code}\n' -H 'X-Auth-Request-Email: matthewbaker1110@gmail.com' \
  http://viewer-authproxy.gardenio.svc.cluster.local:8097/me
#  c) reads still work without identity -> 200
curl -s -o /dev/null -w '%{http_code}\n' http://gardenio-beads-viewer.gardenio.svc.cluster.local:8095/feed
```

Rollback: unset `devAuthProxySecret` and `helm upgrade`; leave the Secret and the
proxy env in place (harmless to the old viewer). Rotation: delete + create the
k8s Secret, `put-secret-value` the Secrets Manager copy, repeat the sha256 check,
then restart the proxy and the viewer together (both read it only at start).

**What actually authenticates a request here is the public edge's Keycloak
sign-in, not this proxy.** Each request carries the signed-in person's own
email and access token, so the audit trail names the person and kbeads
authorizes their writes as them (`requireUserBearerForWrites`).

---

## Why `ecr-token-refresh` exists

Images come from an in-region ECR pull-through cache in front of ghcr
(`global.imageRegistry` in the values file). **k3s ships no AWS credential
provider** — verified on the node: no `ecr-credential-provider` binary, no
`--image-credential-provider-config` on the kubelet. So granting the node role
ECR permissions is *not* on its own enough to pull from ECR; containerd never
consults IMDS for registry auth and a docker-registry Secret is still required.

What the node-role grant buys is that the token can be minted **inside** the
cluster, so no long-lived registry credential is stored anywhere. This CronJob
calls `aws ecr get-login-password` against IMDS every 8 hours (ECR tokens last
12) and rewrites the `ecr` Secret in place.

`imagePullSecrets` lists `ecr` first and `ghcr` second on purpose: if the cache
rule, its credential or this CronJob break, the kubelet falls through to the
ghcr paths and pulls still work.

If the ECR credential provider is ever installed on the nodes (a user-data
change, terraform's lane), delete this CronJob and drop `ecr` from
`imagePullSecrets`.

---

## Headlamp

Headlamp (the Kubernetes dashboard) runs as its own helm release, `headlamp`
in namespace `headlamp`, outside the gardenio chart. It has no login
(`config.unsafeUseServiceAccountToken: true`): whoever reaches it acts as
ServiceAccount `headlamp:headlamp`. So the controls are around it:

- **Edge:** `dashboard.clonk.tv` (`public-edge.yaml`) runs the sign-in check
  (Keycloak + passkey, `/gardenio-admins`) before proxying to
  `headlamp.headlamp.svc.cluster.local:80`, WebSocket upgrade included. There
  is no Ingress or HTTPRoute. The old ziti name `gardenio.dashboard.ziti`
  went with ziti (gd-xNMLrsgajy).
- **RBAC:** `headlamp-rbac.yaml` binds the SA to the builtin `view` role, which
  cannot read Secrets. It replaces the hand-made `headlamp-readonly`
  ClusterRole (get/list/watch on `*`, Secrets included). The UI loses nodes,
  PersistentVolumes, CRDs and custom resources. Apply the new binding first,
  then delete the old one, so Headlamp never has less than it needs:

  ```sh
  kubectl apply -f headlamp-rbac.yaml
  kubectl delete clusterrolebinding headlamp-readonly
  kubectl delete clusterrole headlamp-readonly
  kubectl auth can-i get secrets -A --as=system:serviceaccount:headlamp:headlamp   # no
  kubectl auth can-i list pods -A --as=system:serviceaccount:headlamp:headlamp     # yes
  ```

- **Network:** `headlamp-networkpolicy.yaml` admits ingress to the namespace
  only from namespace `public-edge`. From a crew pod `curl -m 5 http://headlamp.headlamp/` must time
  out; from `public-edge` it returns 200.

Install or upgrade (read the installed chart version first; do not float it):

```sh
helm list -n headlamp                                  # CHART column
kubectl apply -f headlamp-networkpolicy.yaml
helm upgrade headlamp headlamp/headlamp -n headlamp \
  -f headlamp-values.yaml --version <installed chart version>
```

Rollback: remove the `dashboard.clonk.tv` server block, SAN and DNS record;
delete the NetworkPolicy to restore open in-cluster reach. The RBAC swap is the
safe direction: to go back, recreate `headlamp-readonly` as a ClusterRole with
get/list/watch on `*` and bind it to the SA.

---

## api.clonk.tv (daemon API for kd and gb)

Plan ziti-to-keycloak S6 (gd-FQJHYGAnjB). `api.clonk.tv` (`public-edge.yaml`)
proxies `/v1/*` and `/health` to `gardenio-beads.gardenio.svc.cluster.local:8080`
after `auth_request` to oauth2-proxy (`allowed_groups=/gardenio-admins`,
`--skip-jwt-bearer-tokens`, so it verifies the bearer JWT's signature, issuer
and audience). API clients never get a redirect: no or bad token is 401, a valid
token outside `/gardenio-admins` is 403. The static fleet, bridge and agent
tokens are 32-byte hex (`bridgetokenrotator`, `agenttoken.go`), not JWTs, and
are refused here, so the shared token is useless from the internet.

- **Not proxied (404):** `/v1/bridge-tokens`, `/v1/fleet-tokens`, `/v1/admin/`,
  `/v1/embeddings/`, `/v1/events/stream` (SSE is out of scope) and everything
  else, `/metrics` included.
- **Rate limit:** `limit_req_zone api_per_ip` 10 r/s, burst 20, 429 beyond it.
  It runs before the sign-in check, so garbage `Bearer eyJ...` tokens stop at
  the edge instead of costing a Keycloak JWKS fetch.
- **Headers:** `X-Operated-By`, `X-Edge-Auth-*` and `X-Auth-Request-*` are
  blanked on the way to the daemon. `client_max_body_size` is 8m on this host.
- **Daemon side:** the daemon verifies the same JWT again (go-oidc against the
  public issuer) and stamps `operated_by_source=oidc`. Its issuer discovery
  goes out through `auth.clonk.tv`, so API sign-in cannot start while
  public-edge is down.
- **Clients:** `kd login` against `https://api.clonk.tv` (the session binds to
  that host); gb reads the same session file. Ziti stays an opt-in:
  `GARDENIO_ZITI_IDENTITY` and `GARDENIO_ZITI_SERVICE`.

Roll order (operator): targeted Terraform apply for the record; `kubectl apply
-f public-edge.yaml`; wait for `kubectl -n public-edge get certificate
public-edge` to be Ready with the new SAN; then `kubectl -n public-edge rollout
restart deploy/public-edge` (nginx reads the cert at start; a few seconds of 443
downtime on every host).

Rollback: remove the `api.clonk.tv` server block, the `limit_req_zone`, the SAN
and the DNS record, then restart public-edge. gb's session lookup is inert
without a session file.

---

## gardenio.clonk.tv /lands/ and the edge rate limits (Lands Track B 7.8)

Spec `gardenio/docs/superpowers/specs/2026-10-19-lands-track-b-design.md` §2.2
and §2.3, step 7.8 (gd-ylb7gW9sb1). What changed on `public-edge.yaml`:

- **`/lands-tenants`** joins the viewer's `allowed_groups`. It is a static chart
  group (`keycloak.groups`, carrying `gardenio-project-member` only) that the
  Lands provisioner puts every tenant in, so a land created at runtime needs no
  edge edit. `scripts/check-viewer-edge-groups.py` derives it.
- **`location ^~ /lands/`** signs in against `/oauth2/auth-signed-in`, a check
  with no `allowed_groups`: any signed-in realm user, a GitHub registrant with
  no group included, reaches `/lands/accept`, `/lands/apply` and the rest. The
  check script fails the build if any other location uses that check, or if
  `mux.clonk.tv` grows one.
- **No session:** `= /lands/`, `= /lands/terms`, `= /lands/privacy`,
  `= /lands/aup`, `^~ /lands/static/meadow/` and `^~ /lands/static/homestead/`
  (the Homestead skin the pages wear, plan gd-JaMxRKsc8z step 4.1), and the
  splash `= /garden/splash` with its two files `= /garden/splash/three.module.min.js`
  and `= /garden/splash/still.png` (plan gd-EJGA3zDGCk, public since
  gd-hzRHm1KB5r), go to the viewer GET-only with the cookie, bearer and
  identity headers blanked. They are the viewer's `noSessionPatterns`, and
  viewer-authproxy's `$bv_public` map lets exactly these through with no
  identity; `TestPublicEdge_LandsPublicPathsMatchViewer` keeps the three lists
  equal.
- **Unadmitted users:** in `location /`, an `auth_request` 403 (signed in, in
  no admitted group) sends a navigation to `/lands/` with a 302; a script
  (htmx/fetch) still gets the 403.
- **Rate limits** (per client IP, 429 beyond the burst, before any sign-in
  check): `front_door_per_ip` 5 r/s, burst 20, on all of `/lands/`. Named for
  the class of page so later sessionless pages on this host share it.
  `auth_per_ip` 10 r/s, burst 40, on every `auth.clonk.tv` location, except
  the back-channel endpoints (`protocol/openid-connect/token`, `certs`,
  `userinfo`, `token/introspect`, `.well-known/`), which the cluster itself
  calls through this host from a few node addresses: forwardauth's redeem and
  refresh, every verifier's discovery and JWKS fetch.

Roll order (operator; both files are applied by hand, not by helm):
`kubectl apply -f viewer-authproxy.yaml` and `kubectl -n gardenio rollout
restart deploy/viewer-authproxy` (its config is a template rendered at start);
then `kubectl apply -f public-edge.yaml` and `kubectl -n public-edge rollout
restart deploy/public-edge` (a few seconds of 443 downtime on every host). The
`lands-tenants` Keycloak group arrives with the next `helm upgrade` of the
chart; admitting it at the edge before it exists is harmless.

Rollback: revert both files and restart both deployments. The group can stay.

### gardenio.clonk.tv /w/ (the Window, plan gd-pz87vf3xsB PW.5)

A window is a read-only public page on one plan, `https://gardenio.clonk.tv/w/gw_<token>`.
The link is the credential: the viewer hands the token to kbeads, which opens
exactly one route for it (`GET /v1/windows/self/view`). What changed:

- **`public-edge.yaml`:** `location ^~ /w/` has no `auth_request`. It is GET and
  HEAD only, with the cookie, bearer and identity headers blanked, and sits
  behind `front_door_per_ip` (burst 10) plus a new per-IP `limit_conn`
  (`front_door_conn_per_ip`, 10 in flight). The edge itself enforces
  `Referrer-Policy: no-referrer` and `X-Robots-Tag: noindex, nofollow`.
- **`viewer-authproxy.yaml`:** its own `location ^~ /w/` in the 8097 server
  skips `location /`'s no-identity 401. It still sends `X-Authproxy-Secret` and
  forwards no email, access token, bearer or cookie. `/w/` is not in
  `$bv_public`, because the viewer serves it from its own pattern list and not
  from `noSessionPatterns`.
- **The live stream (PW.7, gd-nBkInNpy3r):** `/w/<token>/live` is a
  long-lived event stream that tells the page when the plan changed.
  `public-edge.yaml`'s `/w/` location sets `proxy_buffering off`, since
  viewer-authproxy honors the viewer's `X-Accel-Buffering: no` but hides that
  header from the edge. A stream holds one of the ten `front_door_conn_per_ip`
  connections; the viewer caps one address at three streams and kbeads one
  window at 32, and a refused page keeps its 20 s poll. A 15 s keepalive keeps
  it inside both hops' default 60 s `proxy_read_timeout`.
- **No token in the logs**, on both proxies. `map $uri $log_uri` writes `/w/<token>/rest` as
  `/w/-/rest`, and every server logs the `redacted` format (the image's
  `nginx.conf` would log the full request line). In `/w/`, the error log is
  crit-only, because nginx's error lines quote the request line.
- **api.clonk.tv** already carries `/v1/windows` (mint, list, revoke) for
  operators through `^~ /v1/`. Land owners minting through the land door need
  `land_door_routes.txt` lines once kbeads registers the routes (PW.1).

The roll order is the same as above: viewer-authproxy first, then the edge.

### gardenio.clonk.tv /lands/demo (the demo land's door, gd-T37nCeWMhq)

`^~ /lands/` now signs in against `/oauth2/auth-lands` (the viewer's list plus
`/invited`; GitHub sign-in is invite-only, gd-ycE484kLyk), not the
`/oauth2/auth-signed-in` check described above. The owner opened one door
through it (decision gd-T37nCeWMhq = open): `= /lands/demo`, where a stranger
asks for a demo pass, and `= /lands/accept`, the terms page the door wants
accepted first, sign in against `/oauth2/auth-lands-demo`, a check with no
`allowed_groups`. They proxy like `^~ /lands/` (same rate limit, GET and POST
only). `scripts/check-viewer-edge-groups.py` allows that check on exactly those
two locations and refuses a group list on it. Runbook:
`gardenio/docs/lands/demo-land.md`.

Rolling only this change out (operator; only `public-edge.yaml`, no
viewer-authproxy change). The ConfigMap is a directory mount, so nginx can
reload in place with no 443 downtime:

```sh
kubectl -n public-edge apply -f public-edge.yaml   # or patch just these hunks onto the live ConfigMap
# wait for the kubelet to sync the mount (up to ~1 min), until the new check is there:
kubectl -n public-edge exec deploy/public-edge -- grep -c auth-lands-demo /etc/nginx/conf.d/default.conf  # 4
kubectl -n public-edge exec deploy/public-edge -- nginx -t
kubectl -n public-edge exec deploy/public-edge -- nginx -s reload
curl -s -o /dev/null -w '%{http_code}\n' https://gardenio.clonk.tv/lands/demo    # 302 to sign-in
curl -s -o /dev/null -w '%{http_code}\n' https://gardenio.clonk.tv/lands/apply   # 302 to sign-in, as before
```

Rollback: restore the previous ConfigMap and reload the same way.

---

## api.clonk.tv /land/<name>/v1/* (the land door, Door D.2)

Task gd-h8nQExxbkD, owner decision gd-BTHHmA2rYc (edge-route), boundary
recommendation gd-uYNbyW4a9Z §6 step 2. A tenant's `kd`, `gb` or `gl` talks to
one land at `https://api.clonk.tv/land/<name>`: it never holds a daemon token
or learns a daemon address. Both clients build URLs as base + `/v1/...`, so
setting `BEADS_HTTP_URL` to that base is all they need, and `kd login` against
`api.clonk.tv` is the same session for `/v1/*` and every land.

- **Path, not host.** v1 puts a land in the path on the existing API host,
  which is already bearer-only (no cookie, no redirect). A host per land
  (`<name>.lands.<host>`, wildcard DNS and TLS) is a later upgrade.
- **Which land.** `map $request_uri $land_door_name` reads the name off the raw
  request line with the contract's `LandName` pattern
  (`^[a-z][a-z0-9-]{0,62}$`; `TestPublicEdge_LandDoorNameIsContractLandName`
  keeps them equal). The sign-in subrequest sees its parent's `$request_uri`,
  so the land's groups are built per request: a land created at runtime has a
  door with no edge edit. A name that is percent-encoded, doubled or otherwise
  not in that pattern maps to `""`, and the door 404s it before any check.
- **Who.** `/oauth2/auth-land` is the same bearer-JWT check as `/v1/*`, with
  `allowed_groups=/project-<name>,/project-<name>/owners,/project-<name>/guests`
  (land roles are exclusive, gd-A9avO8cXBr). Not `/gardenio-admins` and not
  `/lands-tenants`: operators use `/v1/*`. A bearer for another land, or for a
  land whose name is a prefix of this one, is 403; no bearer or a bad JWT is
  401. The cookie and any forwarded access token are dropped before the
  check, as on `/v1/*`. `scripts/check-viewer-edge-groups.py` fails the build
  if the list changes or any location but the door uses it.
- **Or a minted land token (Door D.18a, gd-WTyoLCSMUp).** A tenant's laptop
  agent runs `kd`, `gb` and `gl` under a `land-<name>` token, the 10.11 land
  principal that D.17 mints through `/v1/bridge-tokens`. It is opaque, not a
  JWT, so oauth2-proxy cannot judge it. `map $http_authorization
  $land_door_land_token` marks an `Authorization` of exactly `Bearer ` and one
  base64url segment (no dot, so never a JWT), and `/oauth2/auth-land` sends
  that, after its empty-land guard, to the internal
  `/oauth2/auth-land-token`: `GET /v1/me` on the daemon itself (not
  `$land_door_upstream`, so the edge's check does not depend on the land's
  landgate), carrying the caller's bearer, `X-Land-Door: <name>`, no body, and
  none of the identity headers the door blanks. kbeads' `landDoorDenial`
  answers 200 only for this land's land principal, 403 for another land's or
  for a fleet or bridge token, and 401 for a token it does not know (expired
  or revoked included). Nothing is cached, so a revoke bites on the next
  request. Every other `Authorization` (a JWT, a malformed bearer, none) goes
  to forwardauth exactly as before. landgate classifies a bearer with the
  same pattern (`TestEdgeParity_LandTokenBearer`), and the check script fails
  the build if any location `auth_request`s `/oauth2/auth-land-token`
  directly. `TestPublicEdgeNginx_*` runs the api.clonk.tv block in a real
  nginx against stubs (nginx from `$EDGE_NGINX` or `PATH`; the checks.yml
  helm job installs it, and under GitHub Actions a missing nginx fails the
  test rather than skipping it).
- **What.** `/land/<name>/v1/X` is forwarded as `/v1/X` with the caller's
  `Authorization` untouched; kbeads verifies the JWT again and its
  scoped-principal engine cuts what it may read and write. The identity and
  static-path headers are blanked exactly as on `/v1/*`.
- **Only the allowlist (Door D.18, gd-nvvTcZeBZD).** `map "$request_method
  $uri" $land_door_route` names every route the door forwards: the Lands API
  (`gl`) and the daemon routes a tenant's agent loop (`kd`, `gb`) calls. Both
  forwarding locations 404 anything else before the sign-in check. The one
  list is `kbeads/internal/server/land_door_routes.txt`; kbeads and landgate
  enforce it too, and `TestPublicEdge_LandDoorRouteMapIsTheList` fails if the
  map and the file differ. To open a route, add it to the file (and landgate's
  copy) and its map line here, then roll the edge.
- **Not proxied (404) by name as well,** a second wall under the allowlist,
  matched on the decoded path the daemon routes on:
  `/v1/events/stream` and `/v1/bus/events` (the daemon's SSE; the land's own
  feed, `/land/<name>/v1/lands/v1/lands/<name>/events`, has its own
  unbuffered location, D.9),
  `/v1/bridge-tokens`, `/v1/fleet-tokens`, `/v1/auth/` (the refresh exchange
  mints fleet access tokens), `/v1/admin/`, `/v1/hooks/` and
  `/v1/embeddings/`; also `/land/<name>/health`, anything not under `/v1/`, and
  `/land/` itself.
- **Rate limit:** the door shares `api_per_ip` (10 r/s, burst 20) with
  `/v1/*`, before the sign-in check. A per-land limit would have to run after
  the check, or a stranger could spend a land's budget with garbage tokens;
  the land feed's per-land limits (new streams a second, streams open at
  once) are kbeads', after the check, for that reason. A land token's
  `GET /v1/me` check (the edge's, and landgate's) is not counted by kbeads'
  per-identity limiter (`isLandDoorAuthCheck`): every agent of a land shares
  the one `land:<name>` bucket, and two uncounted checks a call would cut its
  read budget to a third. The request the check admits is counted.

One JWT names all of a user's lands (boundary §8 question 6, 5-minute access
tokens bound replay). The door checks membership of the land in the path; it
does not narrow the bearer to that land. kbeads still scopes the request to
every land the token names, so a member of two lands can read the second
through the first's door. Narrowing to the path's land is daemon work (the map,
D.6, or a land principal).

Roll order (operator; applied by hand, not by helm): `kubectl apply -f
public-edge.yaml`, then `kubectl -n public-edge rollout restart
deploy/public-edge` (a few seconds of 443 downtime on every host). No DNS,
certificate, Keycloak or viewer-authproxy change. Check: `curl -s -o /dev/null
-w '%{http_code}\n' https://api.clonk.tv/land/scopetest/v1/beads` is 401;
with a `/project-scopetest` member's `kd login` bearer, `GET
/land/scopetest/v1/me` is 200 and `/land/claimlayer/v1/beads` is 403;
`/land/scopetest/v1/events/stream` is 404. With a minted `land-scopetest`
token (D.18a), `/land/scopetest/v1/me` is 200 and `/land/claimlayer/v1/me` is
403; an unknown opaque token is 401.

Rollback: revert the file and restart public-edge.

### landgate: the land door's second wall (Phase 10, gd-ParwLpmgID)

Boundary gd-uYNbyW4a9Z §6 step 11 (Design B v0). `map $land_door_name
$land_door_upstream` picks where the door forwards a land: the daemon by
default, or that land's `gardenio-landgate-<name>` once a line names it.
landgate (`gardenio/controller/cmd/landgate`, chart `landgate.lands`) re-checks
everything the door checks (bearer JWT, the land's three groups, the same 404
routes, the same blanked headers; its tests read this file to keep the lists
equal), refuses a bearer kbeads would not scope (no `gardenio-project-member`,
or `gardenio-admin`), and forwards the caller's bearer untouched. A minted land
token (D.18a) it asks kbeads about, as the edge does: `GET /v1/me` with
`X-Land-Door`, 5s timeout, 401 and 403 passed back, anything else 502; on 200
it checks the body itself (`operator_id` `land:<name>`, not unrestricted, this
land only) before forwarding. It holds no
fleet, coopmux or daemon token. Its ingress NetworkPolicy admits only this
namespace, and with `landgate.enabled` every policy that selects agent pods
stops admitting this namespace and cuts `landgate.crewDeny.edgeCIDRs` (this
node's EIP, private IP and podCIDR), so a crew with a stolen bearer cannot use
the door.

Roll order per land: `helm upgrade` with `landgate.enabled=true`,
`landgate.lands={<name>}` and the edge CIDRs; wait for
`deploy/gardenio-landgate-<name>` Ready; add
`<name> gardenio-landgate-<name>.gardenio.svc.cluster.local:8080;` to the map;
`kubectl apply -f public-edge.yaml` and restart public-edge. Check as above (a
member's bearer is 200 through the door, another land's is 403), and the
landgate log shows the request. Rollback: drop the map line and re-apply; the
door goes straight to the daemon again.

---

## The k8s-live role

Role bead `gd-gi5v71wMMo` (`privileged=true`, `needs_k8s_api=true`), epic
gd-qbBJGHiGHo, design doc gd-sGW5Y1B8sg. It is the one agent role that gets a
ServiceAccount token, and it exists for **one job**: running Gas City's live
k8s provider suite against the `gc-dogfood` namespace. The suite creates,
execs into and deletes real pods, so it cannot run from an ordinary
(token-less) agent pod. Nothing else should run under this role.

### The gc-dogfood RBAC it relies on

Live state, read 2026-10-11 (`get role,rolebinding,sa -n gc-dogfood` and
`auth can-i --as=system:serviceaccount:gardenio:<sa>`):

| Object | What it holds |
|---|---|
| Role `gc-dogfood/gc-dogfood-session-driver` | `pods` get/list/watch/create/delete; `pods/exec` create; `pods/log`, `pods/status` get/list |
| RoleBinding `gc-dogfood/gc-dogfood-session-driver` | subjects `gardenio/gardenio-agent` **and** `gardenio/gc-k8s-live` |
| SA `gardenio/gc-k8s-live` | `gc-k8s-live-serviceaccount.yaml`, `automountServiceAccountToken: false`; the pod opts in via `needs_k8s_api` |
| SA `gc-dogfood/gc-agent` | what the suite's session pods run as (`GC_K8S_SERVICE_ACCOUNT`); bound to nothing |

`gardenio-agent` and `gc-k8s-live` both answer **yes** for create pods, create
pods/exec and delete pods in `gc-dogfood`, and **no** for create pods in
`gardenio`. The only other binding
on `gardenio-agent` is `gardenio/gardenio-agent`, a Role with no rules.

The Role and RoleBinding are **not in this repo**: they were applied by hand
(Role created 2026-09-19) and live only in the cluster. Only the SA manifest is
here.

The role bead has no `service_account` field yet, so k8s-live pods still run
as the shared `gardenio-agent` SA (`podspec.go` sets `serviceAccountName` from
`service_account`). **The move to the dedicated `gc-k8s-live` ServiceAccount
is pending under gd-KRHGbBJqRN:** phase A (second subject on the binding) is
live; still to do are setting `service_account=gc-k8s-live` on the role bead
and then removing `gardenio-agent` from the binding.

### The two provenance cases

A non-human caller (any static/fleet token, i.e. every agent) that spawns a
privileged role is denied as escalatable
(`kbeads/internal/server/spawn_authz.go`: `authorizeAgentSpawn` →
`authorizeNonHumanSpawn`). The code has no header override (gd-HWWEVbCy8N).
`gb spawn --escalate` turns the denial into a `type:escalation` bead carrying
`spawn_role`. Once
the escalation is approved, `replayEscalation`
(`kbeads/internal/server/http_escalations.go`) replays the create and
stamps three fields server-side (`stampSpawnProvenance`). The controller
copies them onto the pod as annotations
(`gardenio/controller/internal/podmanager/manager.go`).

| | Human-approved | Policy auto-approved |
|---|---|---|
| Who approves | an OIDC human via `POST /v1/escalations/{id}/approve`; `approved_by` is derived from the session, never the body (`escalation_approval_authz.go`) | the evaluator in `kbeads/internal/server/spawn_policy.go`, from an `auto` rule on a `type:spawn_policy` bead (matches project/namespace/requester_role, capped by `max_per_day`/`max_concurrent`) |
| `spawn_approved_by` / `gardenio.io/spawn-approved-by` | the approver's OIDC subject (bare email) | `system:spawn-policy` |
| `spawn_approval_mode` / `gardenio.io/spawn-approval-mode` | `human` | `auto` (`spawnApprovalMode`: any `system:` prefix) |
| `spawned_by` / `gardenio.io/spawned-by` | the escalation id | the escalation id |

The policy path **ships dark**. It runs only with `BEADS_SPAWN_POLICY_ENABLED`
set on the daemon. As of 2026-10-11 that variable is not set on
`gardenio-beads` and no `type:spawn_policy` bead exists, so today every
k8s-live escalation waits for a human. A human spawning k8s-live directly
(OIDC, no escalation) carries none of the three stamps.

## Escalating to an infra agent

A crew pod has no ServiceAccount token and cannot run `kubectl`. To read the cluster
(nodes, pods, events, pod logs), ask for a read-only `infra` agent; the request is
auto-approved within caps (6 per day, 2 concurrent, for `gardenio` crew and plan-captain
requesters) and the agent reports on your task bead:

```bash
gb spawn --role infra --escalate --prompt "Read-only check for <bead>: <commands>; comment a summary on <bead> (no secrets); then gb stop."
```

An over-cap request stays open (the sweep re-checks every 30 s) until a slot frees or a
human approves. The agent can read cluster-wide and read pod logs; it cannot read Secrets,
`exec`, or write. `infra-admin` (full cluster control) exists for platform changes and is
approved by a human every time. Full runbook, prompt pattern and measured results:
[`gardenio/docs/agent-infra-escalation-runbook.md`](../../docs/agent-infra-escalation-runbook.md).

### Running the suite

The target is `test-k8s-native-live` in the Gas City Makefile. It is on the HQ
patch branch `hq/patches/hq-k8s-live-harness` (fixed target at `2ed681fb4`),
not on Gas City `main`. Copied exactly from the Makefile:

```sh
make test-k8s-native-live EXTRA_TEST_ENV='\
  KUBECONFIG=$$HOME/.kube/config \
  GC_K8S_NAMESPACE=gc-dogfood \
  GC_K8S_IMAGE=<registry>/gc-session:v1 \
  GC_K8S_SERVICE_ACCOUNT=gc-agent'
```

`TEST_ENV` is `env -i`, so a `GC_K8S_*` exported in your shell never reaches
the test. Only `EXTRA_TEST_ENV` passes through, and the target sets
`GC_K8S_NATIVE_LIVE=1` itself. With `GC_K8S_IMAGE` unset the run fails
instead of skipping.

**Inside a k8s-live pod there is no kubeconfig.** The provider tries
`rest.InClusterConfig()` first (`internal/runtime/k8s/provider.go`,
`buildRESTConfig`). That call needs `KUBERNETES_SERVICE_HOST` and
`KUBERNETES_SERVICE_PORT`, which `env -i` strips, so pass them in
`EXTRA_TEST_ENV` in place of `KUBECONFIG`. This comes from reading the code;
it has not been run in a pod.

---

## Install order

```sh
export KUBECONFIG=~/.kube/gardenio.yaml

kubectl create namespace gardenio

# Self-generated; nothing external is required for a minimal bring-up.
kubectl -n gardenio create secret generic gardenio-postgres-password \
  --from-literal=password="$(openssl rand -hex 24)"
kubectl -n gardenio create secret generic kd-beads-token \
  --from-literal=token="$(openssl rand -hex 24)"
kubectl -n gardenio create secret generic gardenio-controller-api \
  --from-literal=token="$(openssl rand -hex 32)"
# viewer-authproxy.yaml injects this as the coopmux bearer (the agent-facing
# coopmux token, split from kd-beads-token). It must exist before that manifest
# is applied.
kubectl -n gardenio create secret generic coopmux-agent-token \
  --from-literal=token="$(openssl rand -hex 32)"

# Helm adopts pre-created Secrets only if they carry ownership metadata.
for s in gardenio-postgres-password kd-beads-token gardenio-controller-api; do
  kubectl -n gardenio label  secret $s app.kubernetes.io/managed-by=Helm --overwrite
  kubectl -n gardenio annotate secret $s \
    meta.helm.sh/release-name=gardenio meta.helm.sh/release-namespace=gardenio --overwrite
done

kubectl apply -f ecr-token-refresh.yaml
kubectl -n gardenio create job ecr-refresh-now --from=cronjob/ecr-token-refresh

kubectl apply -f viewer-authproxy.yaml
# Ziti is off (gd-xNMLrsgajy); ../ziti/README.md "Turning ziti back on" if wanted.

helm upgrade --install gardenio ../gardenio -n gardenio \
  -f ../../../deploy-values/gardenio-k3s.yaml
```

Claude credentials for agent pods are a separate step — see
`agents.claudeOAuthSecret` in the values file, which explains why the Secrets
Manager entry cannot be used verbatim.

## Verifying

```sh
curl -s -o /dev/null -w '%{http_code}\n' https://gardenio.clonk.tv/          # 302 (edge sign-in)
curl -s -o /dev/null -w '%{http_code}\n' https://grafana.clonk.tv/api/health  # 302 (edge sign-in)
curl -s -o /dev/null -w '%{http_code}\n' https://api.clonk.tv/health          # 401 (no bearer)
kd list --limit 1                                                            # via api.clonk.tv
kubectl get pods -A -o name | grep -i ziti                                   # nothing
```

## Known gaps

- **No off-node postgres backup.** `postgres.backup` is a pg_dump CronJob
  writing to another `local-path` PVC, which binds on the same node class and
  dies in the same event — it protects against logical corruption, not node
  loss. Off-node backup needs an S3 sync CronJob mirroring
  `gardenio/helm/beads/dolt-backup-cronjob.yaml` plus an S3 grant on the node
  role (inherited by every pod via IMDS, since there is no IRSA). Bucket and
  prefix chosen: `gardenio-hq-backups-123456789012` under `gardenio-pg/`.
- **A crew agent's session log dies with its pod.** The `.claude` PVC is
  garbage-collected on reap, so only token counts survive. A completed run is
  also recorded as `status=crashed, exit_code=143` because the controller's own
  reap sends SIGTERM — cosmetic, but it will misreport on any dashboard
  counting failures.
- ~~The ECR cache rule, its credential secret and the repository creation
  template are CLI-created and absent from terraform.~~ **Closed 2026-09-18.**
  `modules/ecr-pull-through-cache` now also models the repository creation
  template (`untagged_expiry_days`), the composition instantiates it for the
  `ghcr` upstream, and all three live resources were imported, so the rule,
  the secret and the template survive an account rebuild. One piece remains
  deliberately unmanaged: the lifecycle policies on the four cache repos that
  already existed. ECR auto-creates cache repos on first pull, so terraform
  does not own them; they were backfilled with `put-lifecycle-policy`, and
  every repo created from here on inherits the policy from the template.
