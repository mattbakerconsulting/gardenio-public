# Force-logout redirect on forwardauth 403

**Status:** design proposal — DOES NOT implement
**Bead:** ka-0000006780 (parent epic ka-0000005759)
**Author:** gardenio-forcelogout-redirect-on-n99
**Date:** 2026-05-03

## Problem

When a user hits a gardenio-fronted host with a *present-but-rejected* `_gardenio_oauth2`
cookie (stale session, missing required group, refresh-token expired beyond grace,
email-domain mismatch), oauth2-proxy returns **HTTP 403** to the Traefik
`forwardAuth` middleware. Traefik then forwards that 403 — body and all — to the
browser. The user sees a bare "403 Forbidden" page (Slack screenshot file_id
`F0B1RMW94TB`) with no recovery path. On mobile (especially iOS Safari) clearing
cookies for a single subdomain is not obvious; users get stuck and ping support.

The 401-equivalent path (no cookie at all) already works: oauth2-proxy issues a
**302 to Keycloak** and the user lands in a fresh login flow. The gap is purely
the 403 branch — auth check ran, request was understood, the answer was "no",
and the response is currently a dead-end page.

## Goal

When forwardauth rejects with 403, the user's browser must be steered to
`/oauth2/sign_out?rd=<original-url>`. That endpoint:

1. clears the local `_gardenio_oauth2` cookie,
2. redirects to Keycloak's `end_session_endpoint` (because the issuer host is
   already in `--whitelist-domain`, see
   `gardenio/helm/gardenio/templates/forwardauth/deployment.yaml:71-74`),
3. Keycloak ends the SSO session and bounces back to `<original-url>`,
4. forwardauth re-runs, oauth2-proxy has no cookie, and the standard 302→login
   flow takes over.

End state: the user lands in a working session without ever knowing what a
cookie is.

Out of scope: distinguishing "cookie expired" (recoverable by re-login) from
"user is not in the required group" (re-login won't help — they will just hit
403 again from Keycloak's claim mapping). The recovery loop will eventually
terminate at the Keycloak login page if the underlying authorization decision
is permanent. We accept this; the user experience is still strictly better
than the dead-end 403, and a future enhancement can detect the loop with a
counter cookie.

## Current state (verified)

- Forwardauth middleware: `gardenio/helm/gardenio/templates/forwardauth/middleware.yaml`.
- oauth2-proxy deployment: `gardenio/helm/gardenio/templates/forwardauth/deployment.yaml`.
  - `--upstream=static://202` — 2xx ⇒ pass; non-2xx ⇒ Traefik forwards to client.
  - `--reverse-proxy=true`, `--whitelist-domain=.<cookieDomain>` and the
    issuer host, `--cookie-name=_gardenio_oauth2` (note: the bead description
    says `_oauth2_proxy`; the deployed name is `_gardenio_oauth2`).
- `/oauth2/*` is exposed on a single host (`keycloak.forwardAuth.ingress.host`,
  default `gardenio.e2e.example.com`) by
  `gardenio/helm/gardenio/templates/forwardauth/ingressroute.yaml`.
- Every protected IngressRoute (beads-viewer, beads3d, grafana, coopmux,
  roles-server, slack-bridge UI, beads API) attaches the
  `gardenio-forwardauth` middleware via the standard `middlewares:` list.

## Options considered

The task framed three:

- **(a)** Traefik `errors` middleware that catches 403 from forwardauth and
  rewrites to a redirect to `/oauth2/sign_out?rd=<original-url>`.
- **(b)** Configure oauth2-proxy itself to send a redirect on auth failure
  instead of 403.
- **(c)** A small static error page that auto-redirects via
  `<meta http-equiv="refresh">` to `/oauth2/sign_out?rd=...`.

### (b) "Make oauth2-proxy redirect on 403" — rejected

oauth2-proxy 7.x has no flag to convert a 403 (forbidden by allowed-groups,
email-domain, or skip\_jwt-bearer-token rules) into a 302. The flags floated
in the task description are not what they sound like:

- `--skip-auth-strip-headers` — controls header propagation, not status.
- `--reverse-proxy` — already on; affects `X-Forwarded-*` trust, not status.
- `--allow-error-page` — does not exist in 7.x.
- `--signout-redirect-url` — controls the *post*-sign\_out redirect, not the
  auth-failure response.

The only path to make oauth2-proxy itself redirect on 403 is a **patched
upstream binary**. That moves auth-path code into our maintenance burden
forever, on the load-bearing path that gates every protected ingress. Reject.

There is a related-but-different idea: change the `forwardauth` middleware to
treat oauth2-proxy responses as `auth-uri redirect`-style (i.e. 401 → 302 to
login URL). Traefik's `forwardAuth` does not have a mode for this; the
behavior is hard-coded — non-2xx is forwarded to the client. So there is no
oauth2-proxy- or Traefik-config-only fix at this layer.

### (c) Static page with `<meta http-equiv="refresh">` — partial fit

A 200-OK HTML page that meta-refreshes is the simplest mechanism that works
on every browser (no JS, no `Refresh:` header dependency). But it cannot stand
alone — Traefik `forwardAuth` does not let you replace a non-2xx auth-server
response with arbitrary HTML. Some component must intercept the 403 *after*
forwardauth and substitute the page. That component is the Traefik `errors`
middleware (option a). So (c) is not really an alternative to (a); it is a
choice about *what (a) serves*.

### (a) Traefik `errors` middleware — selected

Traefik's `errors` middleware
(<https://doc.traefik.io/traefik/middlewares/http/errorpages/>) matches a
status-code range on the response and transparently replaces the body by
proxying to a configured backend service. Crucially for our case, the
matched response is the one Traefik is about to return to the client —
including the response from a `forwardAuth` reject — so a chain of
`[forwardauth, errors-403]` on the IngressRoute will trigger the errors
middleware on the 403 leak.

The errors middleware does not support emitting a 302 directly. What we can
do is point it at a small static service whose `/403` returns either:

1. **HTML with `<meta http-equiv="refresh" content="0; url=/oauth2/sign_out?rd=...">`**
   plus a visible "If you are not redirected, click here" link, **or**
2. **A 302 response with `Location: /oauth2/sign_out?rd=...`** (Traefik
   forwards the backend's status + Location to the client unchanged).

Both work. Option 1 is more robust: the Traefik errors middleware was
designed to serve HTML error pages, the response-rewriting interactions with
caching proxies / browsers are well-understood, and a meta-refresh degrades
visibly (the user sees a "redirecting…" page with a link) instead of dying
silently if something downstream eats the redirect. Option 2 is one fewer
network round-trip but exposes us to subtle interactions (e.g. some browser
cache layers refusing to cache a 302 that came back through an errors
middleware). **Pick option 1.**

Where to host the static page: the existing **landing** deployment
(`gardenio/helm/gardenio/templates/landing/`) is already an nginx pod serving
static HTML on the same release; adding a `/403` location to its ConfigMap
costs zero extra infra. The errors middleware then references
`gardenio-landing` as its `service`.

## Recommendation

Implement **option (a)**, served by the existing **landing** pod, returning a
**meta-refresh HTML page** (option-1 sub-variant above).

```yaml
# Sketch only — DO NOT apply.
# gardenio/helm/gardenio/templates/forwardauth/middleware-errors.yaml
apiVersion: traefik.io/v1alpha1
kind: Middleware
metadata:
  name: gardenio-forwardauth-errors
spec:
  errors:
    status:
      - "403"
    service:
      name: gardenio-landing
      port: 80
    query: "/403"
```

Each protected IngressRoute then chains the new middleware **after**
`gardenio-forwardauth` in its `middlewares:` list:

```yaml
middlewares:
  - name: gardenio-security-headers
  - name: gardenio-forwardauth
  - name: gardenio-forwardauth-errors   # NEW — only fires on 403 leak
```

The landing nginx ConfigMap gains a `/403` location whose body is roughly:

```html
<!DOCTYPE html>
<html><head>
<meta charset="utf-8">
<meta http-equiv="refresh" content="0; url=/oauth2/sign_out?rd=https://HOST/PATH">
<title>Refreshing session…</title>
</head><body>
<p>Your session has expired. Refreshing…</p>
<p><a href="/oauth2/sign_out?rd=...">Click here if you are not redirected.</a></p>
</body></html>
```

The `rd=` value comes from `X-Forwarded-Host` + `X-Forwarded-Uri`, which
Traefik exposes to the errors backend via the `X-Content` family of error-page
headers. Implementation will need to confirm exact header names (Traefik
`errors` sets `X-Content-Type` for the *response* content type; the upstream
URI surfaces via `X-Forwarded-Uri` on the request to the errors service —
verify against the deployed Traefik version before shipping).

## Trade-offs

| Concern | Status |
|---|---|
| Does not patch oauth2-proxy → no upstream maintenance burden | ✓ |
| Does not change the auth decision itself → no security regression | ✓ |
| Meta-refresh is visible to the user (sees "Refreshing…"), not silent | ✓ |
| Permanent-403 cases (user not in group) loop back to Keycloak login → eventual dead-end | ⚠ accepted, see Goal §; future-work counter cookie can break the loop |
| Errors middleware adds one extra middleware per protected route | trivial; matches existing chain pattern |
| Landing pod becomes load-bearing for the 403 path | mitigate with replicaCount ≥ 2 + PDB on landing (already common pattern; verify) |
| /oauth2/sign_out is exposed on the forwardAuth host, but protected hosts may be different subdomains | the redirect target stays absolute (`https://gardenio.e2e.example.com/oauth2/sign_out?rd=...`); cookie domain `.e2e.dev.acme.example` makes the cookie clearance global |
| Possible loop if `/oauth2/sign_out` itself ever returns 403 (it shouldn't — it's a public path) | hard-exclude `/oauth2/` from the errors middleware via route ordering; the `/oauth2/` IngressRoute does not attach forwardauth, so 403 cannot originate there |
| Landing page caching: meta-refresh HTML must `Cache-Control: no-store` to avoid a stale `rd=` being reused on a different host | implement explicit `add_header Cache-Control "no-store"` on `/403` |

## Open questions for implementation

These are not blockers for this design — they are flagged for the
implementation MR, not for the operator decision on the approach.

1. **Header name for the original URI** in Traefik errors middleware. Recent
   Traefik (v3) sets `X-Forwarded-Uri` and `X-Forwarded-Host` on the request
   to the errors backend. Verify in the cluster Traefik version, document
   the exact header in the implementation MR.
2. **Per-host vs single-host redirect.** The `rd=` target should send the
   user back to the *original* protected host, not always to the
   forwardAuth host. The sign\_out path itself lives on the forwardAuth host
   (single `/oauth2/*` IngressRoute). Implementation needs an absolute URL
   for sign\_out and a relative-or-absolute `rd=` decision. Recommend:
   absolute sign\_out URL, absolute `rd=` reconstructed from the
   X-Forwarded-* headers.
3. **403s from upstream services (not from forwardauth).** A genuine
   application 403 (e.g. beads daemon ACL denies a request) would currently
   leak through the same errors middleware and force a logout. That would
   be a regression. Mitigation: scope the errors middleware to **only the
   forwardauth response** by applying it only when the upstream is the
   forwardauth service. In Traefik this is most cleanly done by using
   `forwardAuth.errors` (NO — does not exist). The robust path is to
   distinguish "auth 403" from "app 403" via a marker header that the
   errors-page nginx checks (e.g. oauth2-proxy already sets
   `X-Auth-Request-Redirect` or similar; if not, add a static
   `--set-xauthrequest=true` plus an error-page-side check). Implementation
   must confirm a reliable distinguisher before broad rollout. **This is
   the single most important risk to validate during implementation.**
4. **Mobile behavior.** Verify on iOS Safari (the platform that motivated
   this work) that meta-refresh fires before the user can see the page —
   acceptable lag is <1s.
5. **Tests.** The implementation MR must add an E2E test that:
   sets a deliberately-bad `_gardenio_oauth2` cookie → hits a protected
   host → confirms the response chain ends at a working session, not a
   bare 403. The existing forwardauth audit fixtures (see
   `gardenio/docs/audits/forwardauth-authorization-collision.md`) are a
   reasonable starting point.

## Why not just (c) by itself?

We considered "skip the errors middleware, just point oauth2-proxy at a
custom upstream that serves the meta-refresh page on 403." That requires
either a sidecar or an oauth2-proxy fork — both are worse than reusing the
landing pod via Traefik's purpose-built error mechanism.

## Decision needed from operator

Before implementation begins, the operator must sign off on:

1. **Approach (a) with the landing-pod static page** as described above.
2. **Risk #3** (auth-403 vs app-403 disambiguation) — the implementation MR
   will spike the disambiguation strategy and surface it for review before
   any rollout to `gardenio` (prod).

If the operator picks an alternative approach, this document should be
amended (not replaced) and the rationale recorded inline.

## References

- Slack thread: <https://acme.slack.com/archives/C0AHE7C0E1G/p1777831245785019>
- Screenshot: Slack file id `F0B1RMW94TB` (iOS Safari 403 page).
- Traefik errors middleware: <https://doc.traefik.io/traefik/middlewares/http/errorpages/>
- oauth2-proxy 7.x flags: <https://oauth2-proxy.github.io/oauth2-proxy/configuration/overview>
- Existing forwardauth audit:
  `gardenio/docs/audits/forwardauth-authorization-collision.md`
  (ka-0000000839, follow-up ka-0000002928).
- Forwardauth deployment + middleware:
  - `gardenio/helm/gardenio/templates/forwardauth/deployment.yaml`
  - `gardenio/helm/gardenio/templates/forwardauth/middleware.yaml`
  - `gardenio/helm/gardenio/templates/forwardauth/ingressroute.yaml`
- Landing pod (proposed host of `/403`):
  - `gardenio/helm/gardenio/templates/landing/configmap.yaml`
  - `gardenio/helm/gardenio/templates/landing/deployment.yaml`
