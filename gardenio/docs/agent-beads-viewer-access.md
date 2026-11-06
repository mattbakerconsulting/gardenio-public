# What an agent can reach: beads and beads-viewer

Task gd-4YkfPTZH4W (plan gd-xiV5hyhPrG, step P4). Measured 2026-10-13 from an ordinary
crew pod on the k3s cluster (`deploy-values/gardenio-k3s.yaml`), then checked against the
code. Statuses and shapes only; no token or body is recorded here.

**Scope.** This is the k3s deployment. The EKS deployment differs (viewer behind AWS
Verified Access, `networkPolicies` on, `K8S_SA_*` branch possible); where a claim is
k3s-only it says so.

## Summary

| Question | Answer |
|---|---|
| Can an agent read beads with no escalation? | **Yes.** `kd` and `gb` work out of the box. |
| Can an agent look at the viewer with no escalation? | **Yes, on k3s**, over plain HTTP to the in-cluster Service, with no credential at all. This is a gap, not a design (see Gaps). |
| Can it use `gb viewer-login`? | **No.** The pod has no ServiceAccount token. By design for ordinary crew pods. |
| Can it read pod logs / `kubectl`? | **No** without an infra agent. `gb logs` reads the log store for six components only. |

## Measured results (crew pod, no SA token mounted)

| # | Command | Result |
|---|---|---|
| 1 | `kd list --limit 1` | exit 0; a table with one bead row and a `N beads (… total; project=gardenio …)` footer |
| 2 | `gb ready` | exit 0; a list of ready beads |
| 3 | `gb news` | exit 0 |
| 4 | `curl $BEADS_HTTP_ADDR/healthz` with no token | **401** |
| 5 | `curl $BEADS_HTTP_ADDR/v1/beads?limit=1` with no token | **401** |
| 6 | same, with `Authorization: Bearer $BEADS_AUTH_TOKEN` | **200** |
| 7 | `curl $BEADS_VIEWER_INTERNAL_URL/` (viewer Service, no credential) | **200** `text/html` (the activity feed) |
| 8 | same host: `/feed`, `/plans`, `/active`, `/search?q=x` | **200** each |
| 9 | same host: `/healthz` | **200** |
| 10 | same host: `/api/beads`, `/api/v1/beads` | **404** (no such route; the viewer has no JSON beads API of that shape) |
| 11 | same host: `/me`, `/u/me`, `/login` | **302/308** to `/login?next=…` or `/oauth2/start` (no resolved operator) |
| 12 | same host: `/me` with `X-Auth-Request-Email: <the pod's own operator email>` | **308** to `/u/<slug>`: the viewer accepted a self-asserted identity |
| 13 | `curl http://$VIEWER_AUTHPROXY_SERVICE_HOST:8095/` (the nginx authproxy) | **200** |
| 14 | `curl $BEADS_VIEWER_URL/` (`https://gardenio.clonk.tv`, the public name) | **000**: not reachable from inside the pod (ziti-only, no public route) |
| 15 | `gb viewer-login` | exit 1, `could not auto-detect pod namespace; pass --namespace ($POD_NAMESPACE unset and no ServiceAccount namespace file mounted)` |
| 16 | `gb viewer-login --status` | `No viewer bearer session. Run 'gb viewer-login' from inside a pod.` |
| 17 | `ls /var/run/secrets/kubernetes.io` | `No such file or directory`: no SA token |
| 18 | `kubectl auth can-i get pods` | error: no usable kubeconfig or in-cluster config |
| 19 | `gb logs beads-viewer` | exit 1: `component "beads-viewer" is not queryable; allowed: controller, gitlab-bridge, grafana-bridge, jira-bridge, slack-bridge, trigger-engine` |

Row 15 is refused for a different reason than the task text expected: it never reaches the
TokenRequest call. With no SA token mounted there is no namespace file, so the command stops
at namespace auto-detection. Passing `--namespace` would still fail, because there is no
token to authenticate the TokenRequest with.

## Beads (`kd`, `gb`)

Works with no escalation. Credentials come from the pod environment: `BEADS_AUTH_TOKEN`,
`BEADS_DAEMON_TOKEN`, `BEADS_ACTOR`, `BEADS_OPERATED_BY`, `BEADS_HTTP_ADDR`,
`BEADS_GRPC_ADDR` (rows 1-6). The daemon rejects an unauthenticated request with 401 on
every route, including `/healthz` (rows 4-5); a request with the pod's token succeeds (row 6).

`kd list` is scoped to the pod's project by default; `--all-projects` widens it (row 1
footer). Reads of other projects' beads are therefore a flag away, not an escalation.
Whether the daemon should let a crew token read across projects is a separate authz
question and is not decided here.

**Minimal path for "look at beads":** `kd show <id>`, `kd list`, `kd search`, `gb ready`,
`gb news`. Nothing to request.

## Viewer: which routes need what

The auth chain is built in `gardenio/controller/cmd/beads-viewer/main.go`:
`ipWhitelist -> authMiddleware -> operatorMiddleware -> viewTelemetry -> appMux`
(`main.go:441-444`).

`authMiddleware` is one of two things:

- **k3s (this cluster): `devBypassMiddleware`.** `deploy-values/gardenio-k3s.yaml:1211`
  sets `beadsViewer.devAuthBypass: true`; OIDC, AVA and ingress are all off (`:1213-1216`).
  The middleware passes every request through and, if `X-Auth-Request-Email` is present,
  promotes that raw header into the verified identity (`dev_auth.go:144-176`).
- **EKS: `oidcauth`**, which verifies an AVA JWT, a Keycloak token, or (when `k8sSA.enabled`)
  an audience-pinned EKS ServiceAccount token whose `sub` is on `K8S_SA_ALLOWED_SUBJECTS`
  (`main.go:628`, `oidcauth/k8ssa.go:207`). Unauthenticated requests do not pass.

Route classes on k3s, from `server.go` and rows 7-12:

| Route class | Needs | Evidence |
|---|---|---|
| `/healthz`, `/readyz` | nothing (registered outside the auth chain) | `main.go:383-397`, row 9 |
| `/login` | nothing (anonymous by design) | `main.go:399-407`, row 11 |
| Read pages: `/`, `/feed`, `/plans`, `/active`, `/search`, `/captains`, `/threads`, `/fleet`, `/b/{id}` … | nothing on k3s | `server.go:1630-1738`, rows 7-8 |
| `/me`, `/u/{slug}` redirect | a resolved operator; absent one, a 302 to `/login` | `user_home.go:280-310`, row 11 |
| Writes that carry an identity: `POST /b/{id}/comment`, `/decision/resolve`, `/escalation/approve`, `/escalation/reject`, `/thread-reply`, … | an identity in context; the handler 401s without one | `server.go:3910-3917`, `server.go:2288-2312` |

On k3s the only identity source is the `X-Auth-Request-Email` header. On the normal path
(public edge, then the authproxy's `listen 8097` block) nginx overwrites it with the email
the edge's Keycloak sign-in verified (`gardenio/helm/gardenio-ziti/viewer-authproxy.yaml`;
the fixed-email ziti listeners went with ziti, gd-xNMLrsgajy). Direct to the viewer Service
nothing overwrites it, so the caller picks the identity (row 12).

## Viewer: what an agent can do

### Works with no escalation (k3s)

- **Read any viewer page** over `http://gardenio-beads-viewer.gardenio.svc.cluster.local:8095`
  (`$BEADS_VIEWER_INTERNAL_URL`), no credential (rows 7-9). This is the minimal path for
  "look at the viewer": `curl -s "$BEADS_VIEWER_INTERNAL_URL/b/<id>"`. The pages are HTML; for
  data prefer `kd`.

### Needs an infra agent

- **`gb viewer-login`** (`cmd/gb/viewer_login.go:92`) needs a pod ServiceAccount that can call
  TokenRequest and whose subject is on `K8S_SA_ALLOWED_SUBJECTS`. Crew pods mount no SA token
  because `automountServiceAccountToken = spec.NeedsK8sAPI` (`podmanager/manager.go:1644`).
  It is also unnecessary on k3s, where the k8sSA branch is not configured and the viewer
  does not check tokens. It matters on EKS only.
- **Pod logs, events, `kubectl`**: the default agent SA has no `pods/log`
  (`cmd/gb/logs.go:65`); `gb logs` reads VictoriaLogs for six components, not the viewer
  (row 19). Anything else is the read-only `infra` role's job (P5, P8).

### Not possible

- Reaching the viewer by its public name `gardenio.clonk.tv` from a pod (row 14): it is
  ziti-only. Use the Service DNS name.
- A viewer API token for an agent. The viewer has no per-agent credential; on k3s it has
  none at all, and on EKS an agent would need an SA token (above).

## Gaps

1. **The k3s viewer trusts a client-supplied `X-Auth-Request-Email` from any pod** (row 12).
   Any agent pod can reach the viewer Service directly (the k3s overlay sets
   `networkPolicies: {enabled: false}`, `gardenio-k3s.yaml:1317`) and assert any operator's
   email, which makes the viewer treat the request as that operator for the identity-carrying writes
   above (escalation approve/reject, decision resolve, comments). Row 12 shows the identity is
   accepted on a read; whether each write then succeeds depends on the daemon's own authz
   (gd-WJ09iJNvFM notes the viewer's relayed token is currently rejected for human approvals),
   which was not exercised here. This breaks the
   assumption written into `dev_auth.go:128-168` and into the `viewer-authproxy.yaml` header
   ("a caller cannot forge a different operator"): that holds only for callers who go
   through nginx. Filed as a bug under this plan; related to gd-epq9jTtIxV (replace the
   authproxy's fixed email) and to the open kbeads header-trust fixes (PR #291, #294), which
   cover the daemon, not the viewer. **Not by design.**
2. **`gb viewer-login`'s failure message is misleading for a crew pod** (row 15): it says
   to pass `--namespace`, but no namespace will help without an SA token. Minor; recorded
   as a bug under this plan.
3. **No agent-facing JSON API on the viewer** (row 10). **By design:** agents use `kd`.

## References

- `gardenio/controller/cmd/beads-viewer/main.go` (auth chain), `dev_auth.go`,
  `server.go` (routes), `user_home.go`
- `gardenio/controller/internal/oidcauth/k8ssa.go`
- `gardenio/controller/cmd/gb/viewer_login.go`, `cmd/gb/logs.go`
- `gardenio/controller/internal/podmanager/manager.go:1644`
- `gardenio/helm/gardenio-ziti/viewer-authproxy.yaml`
- `deploy-values/gardenio-k3s.yaml:1191-1216, 1317`
