# Force-logout on forwardauth 403 — implementation-MR spike notes

**Bead:** ka-0000000378 (impl); design ka-0000006780; epic ka-0000005759.
**Design doc:** [`forwardauth-403-force-logout.md`](forwardauth-403-force-logout.md).
**Author:** gardenio-implement-forcelogout-redirect-x5r.
**Date:** 2026-05-03.

This document records the two pre-implementation spikes the design doc
required (Risks #2 and #3 in
[`forwardauth-403-force-logout.md`](forwardauth-403-force-logout.md)) so that
the operator can sign off before flipping
`keycloak.forwardAuth.forceLogoutOn403.enabled` on in any environment.

The implementation MR (this same branch) ships the chart changes
**toggled OFF**. The toggle is the gate; nothing renders the new middleware,
the new nginx `/403` block, or the chained middleware reference in protected
IngressRoutes until the operator opts in.

## Spike 1 — auth-403 vs app-403 disambiguation (Risk #3)

### The risk, restated

The Traefik `errors` middleware fires on **any** 403 status leaving the
IngressRoute. It cannot natively distinguish:

- **auth 403** — `gardenio-forwardauth` ForwardAuth middleware rejected the
  request because oauth2-proxy returned 403 (stale cookie, wrong group,
  expired refresh-token, email-domain mismatch). Traefik never called the
  upstream; this is the case we want to rewrite into a force-logout page.
- **app 403** — forwardauth passed (200/202), Traefik forwarded the
  request to the upstream service, and the upstream itself returned 403
  (e.g. beads daemon ACL denies a kd RPC, slack-bridge rejects an
  agent endpoint, grafana viewer permissions). We must NOT rewrite this
  into a force-logout — the user's session is fine; logging them out is
  user-hostile and obscures a legitimate authorization decision.

### Investigation

- **Path A — distinguish via response header markers from oauth2-proxy.**
  Rejected. Traefik's `errors` middleware proxies the *original request*
  to the errors backend; it does not pass the *original response*'s headers
  along. The errors-page nginx therefore cannot read whatever marker
  oauth2-proxy may have set. (Verified by reading
  [Traefik errors middleware docs](https://doc.traefik.io/traefik/middlewares/http/errorpages/)
  and the v3 source for the `errors` middleware request mutation.)

- **Path B — distinguish via custom status code.** Rejected. oauth2-proxy
  7.x has no flag to remap its auth-failure status; the design doc already
  enumerates the (non-)flags. Patching upstream is the explicit
  no-fly-zone in the parent epic.

- **Path C — Traefik response-header-based routing on the errors backend.**
  Rejected. Traefik's CRD does not support per-status, per-source-middleware
  scoping for the `errors` middleware.

- **Path D — restrict the routes that chain the errors middleware.**
  Selected, with caveats below.

### Selected approach (Path D + scoped rollout)

For the protected routes we ship with chained-on-toggle:

| Route | Risk of upstream-emitted 403 | Handling |
|---|---|---|
| beads-viewer (`gardenio-beads-viewer`) | **Low.** Reads bead state via beads HTTP; auth check is forwardauth + viewer's own OIDC verifier. Upstream returns 401/200, not 403. | OK to chain. |
| beads3d (`gardenio-beads3d`) | **None.** Static SPA bundle from nginx; no per-bead authorization. | OK to chain. |
| coopmux dashboard (`/mux`, `/ws/`, `/api/v1/sessions`) | **Low.** Mux's OIDC verifier is itself fed by oauth2-proxy's Authorization header. Mux rejects with 401, not 403, on bad/missing token. | OK to chain. |
| grafana | **Possible.** Grafana itself returns 403 for "viewer cannot edit dashboard X" or org-mismatch. | **Acceptable in the short term** — grafana behind forwardauth is staff-only; a force-logout on a permissions-denied dashboard click is annoying but not data-loss. Operator should re-evaluate post-rollout. |
| roles-server | **Low.** Single-tenant role-assignment UI; not a public, per-user authorization surface. | OK to chain. |
| slack-bridge UI route | **Low.** Decisions/escalations UI; bearer- or session-gated above the bridge. The bridge's own 403 paths are webhooks (different IngressRoute, no forwardauth, not chained). | OK to chain. |
| beads API (`gardenio-beads`) | **Possible — watch closely.** The beads daemon emits 403 for project-label scoping, RBAC denies, and bead-write authorization. These would currently get rewritten into force-logout pages. | **Watch.** First rollout should leave `forceLogoutOn403.enabled=false` for the prod beads host until the daemon's app-403 pathways are audited. The chart toggle is global, so a per-route opt-out (future work) is required to scope safely. |

### Recommended rollout sequence (operator decision)

1. **Dev / e2e cluster:** flip `forceLogoutOn403.enabled=true` first.
   Hit each protected host with a known-good and known-bad cookie; observe
   that a known-good 200 path is unaffected and a known-bad cookie hits
   the meta-refresh page → `/oauth2/sign_out` → Keycloak → fresh login.
   Run the existing forwardauth-audit fixtures
   (`gardenio/docs/audits/forwardauth-authorization-collision.md`).
2. **gardenio-rwx (staging) E2E:** add an E2E test (see Acceptance below)
   that sets a deliberately-bad `_gardenio_oauth2` cookie, hits a protected
   host, and asserts the response chain ends at a working session — not
   a bare 403.
3. **prod (`gardenio`)** flipped only after operator sign-off.
4. **Open follow-up bead** (not in this MR) to add per-route opt-out for
   the beads-API route once daemon app-403 pathways are mapped.

### Why this is acceptable to land toggled-off

The toggle defaults to `false`. With the toggle off, **no protected
IngressRoute references `gardenio-forwardauth-errors`** and the middleware
itself does not render. The risk surface for app-403 hijack is therefore
zero on installation; flipping the toggle is an explicit operator action
that incorporates this spike's caveats by reference.

## Spike 2 — request-header name for the original URI

### The question

Traefik's `errors` middleware proxies the **request** (not the response) to
the errors backend. The original requested URL needs to land at the
errors backend so the `/403` page can build the `rd=<original-url>` query
parameter.

### What Traefik v3 actually sets

When the errors middleware proxies to the errors backend, Traefik sets the
following on the request (verified against the
[Traefik v3 errors middleware source](https://github.com/traefik/traefik/blob/v3.x/pkg/middlewares/customerrors/custom_errors.go)
and the
[errorpages doc](https://doc.traefik.io/traefik/middlewares/http/errorpages/)):

| Header | Value | Notes |
|---|---|---|
| `X-Forwarded-Host` | original Host header | Always set by Traefik when proxying. |
| `X-Forwarded-Uri` | original request URI (path + query) | Set by Traefik for the errors backend specifically. |
| `X-Forwarded-Proto` | `http` / `https` | Always set by Traefik when proxying. |
| `X-Forwarded-Method` | `GET` / `POST` / etc. | Set by Traefik for the errors backend; not used by us. |

The request URL **path** at the errors backend is whatever the `query`
field of the errors middleware specifies (we use `/403`). Variable
substitution (`{url}`) is supported in `query` for Traefik v3, but rather
than rely on the substitution edge cases (`{url}` URL-encodes the original
URL), we read `X-Forwarded-Host` + `X-Forwarded-Uri` server-side in nginx
and build the absolute `rd=` ourselves. This is more robust against
upstream substitution-syntax churn.

### Implementation choice

The nginx `/403` location reads `$http_x_forwarded_host` +
`$http_x_forwarded_uri`, with sane defaults for the in-cluster probe case
(`$host` and `/` respectively). The meta-refresh URL is constructed
server-side as:

```
{signOutURL}?rd=https://$forcelogout_host$forcelogout_uri
```

where `signOutURL` is rendered at chart-install time from
`keycloak.forwardAuth.forceLogoutOn403.signOutURL` (override) or, by
default, `https://{keycloak.forwardAuth.ingress.host}/oauth2/sign_out`.

### Verification on the deployed Traefik

The chart does not pin a Traefik version (Traefik is installed at the
cluster level, outside this chart). Before the operator flips the toggle
in any environment, run the following sanity check against the deployed
Traefik to confirm the header names match this spike:

```bash
# Hit a protected host with a deliberately-bad cookie and check Traefik's
# access log for the errors-backend leg. The relevant headers are visible
# in Traefik's accessLog when fields[X-Forwarded-Host]=keep is set.
kubectl -n gardenio logs deploy/traefik --tail=200 | grep "/403"
```

Or curl directly with `Cookie: _gardenio_oauth2=garbage` and observe the
nginx access log on the landing pod:

```bash
kubectl -n gardenio logs deploy/gardenio-landing --tail=20 | grep "/403"
# Expect: GET /403 with X-Forwarded-Host=<original-host>, X-Forwarded-Uri=<path>
```

If the headers are absent on the deployed Traefik build (theoretically
possible in a heavily-customized Traefik install), the page will still
render — but the `rd=` will fall back to the in-cluster `$host` / `/`,
which is **not** what we want for a real user. The toggle must stay off
until headers are confirmed.

## Acceptance checklist (carried from the impl bead)

- [x] Spike outputs documented (this file).
- [x] Helm-chart changes land behind a feature toggle (default off).
- [ ] E2E test: set bad `_gardenio_oauth2` cookie, hit protected host, confirm working session (not bare 403). **Pending — to be added in a follow-up MR after operator sign-off on Spike 1.**
- [ ] iOS Safari smoke test. **Pending operator/QA validation in dev cluster.**
- [ ] Operator sign-off before enabling in `gardenio` (prod). **Pending.**

## What this MR does and does not change

| Component | Change |
|---|---|
| `keycloak.forwardAuth.forceLogoutOn403.enabled` | New value, default `false`. |
| `keycloak.forwardAuth.forceLogoutOn403.signOutURL` | New value, defaults to `https://<forwardAuth.ingress.host>/oauth2/sign_out`. |
| `templates/forwardauth/middleware-errors.yaml` | New, renders only when toggle on. |
| `templates/landing/configmap.yaml` | Adds `/403` location + X-Forwarded-* maps. Conditional on `keycloak.forwardAuth.enabled` (independent of the toggle, so the page is reachable for ad-hoc testing in `kubectl port-forward` mode without flipping the chained-route toggle on). |
| `templates/{beads-viewer,beads,beads3d,coopmux,grafana,roles-server,slack-bridge}/*.yaml` | Add chained `gardenio-forwardauth-errors` middleware reference, gated on the toggle. |
| `templates/_helpers.tpl` | Update `gardenio.coopmux.ingressMiddlewares` helper to include the chained middleware when toggle is on. |
| oauth2-proxy itself | **No change.** No flags added; the auth decision is unchanged. |
| Traefik install / cluster CRD | **No change.** All changes live inside this chart. |

## References

- Design doc: [`forwardauth-403-force-logout.md`](forwardauth-403-force-logout.md).
- Traefik errors middleware: <https://doc.traefik.io/traefik/middlewares/http/errorpages/>.
- oauth2-proxy 7.x configuration: <https://oauth2-proxy.github.io/oauth2-proxy/configuration/overview>.
- Forwardauth audit: `docs/audits/forwardauth-authorization-collision.md` (not in this repo — the audits tree was excluded from the 2026-09-16 export).
