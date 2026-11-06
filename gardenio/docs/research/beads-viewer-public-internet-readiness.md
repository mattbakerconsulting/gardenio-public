# beads-viewer — Public Internet Readiness Assessment

**Status:** Research / readiness assessment.
**Tracking bead:** ka-0000004314
**Source thread:** Slack 2026-05-02 (Matt) — "assess readiness for public internet exposure"
**Plan-of-record:** `gardenio/docs/beads-viewer-public-internet.md` (epic ka-0000002083)

## TL;DR

**Not yet — but close.** All hardening pieces (T1, T3-prereq, T4, T5) are
either landed or ready; the cutover (T2 chart fail-closed + T6 prod
IP-whitelist drop) is still gated by **T3 verification** (GitLab SSO E2E
walkthrough on `gardenio-rwx`). One pre-existing posture gap deserves
explicit Matt sign-off before cutover: **the viewer is not read-only** —
32 mutating POST endpoints are exposed to any authenticated
`@acme.{ai,com}` user, including agent control (input, keys, nudge,
respond) and bead bulk mutations. That has been the policy since the
office-VPN era; it just becomes a wider blast radius the moment the IP
whitelist comes off.

Recommend **gate cutover (T6) on T3 walkthrough + explicit Matt
acknowledgment of the mutation surface posture** (operator-role gate
already covers the agent-control endpoints; everything else is
authenticated-user-only).

## Status of plan-of-record children (epic ka-0000002083)

| Task | Subject | Status |
|---|---|---|
| T1 (ka-0000005956) | Login picker page (Option B) | **closed** — implemented in `cmd/beads-viewer/login.go`, `loginPicker.*` values wired in chart |
| T3 (ka-0000002649) | GitLab SSO E2E on gardenio-rwx | **open** — escalation cleared, walkthrough not captured |
| T4 (ka-0000001205) | Auth source decision | **closed** — email-only gate (ka-0000002658, 2026-04-27) |
| T5 (ka-0000001362) | Rate-limit + abuse logging | **closed** — per-IP `sourceCriterion` shipped |
| T2 (ka-0000003476) | Chart fail-closed; remove IP whitelist | **open** |
| T6 (ka-0000002062) | Prod cutover | **open** — last step |
| Escalation (ka-0000003031) | GitLab OAuth + AWS SM secret | **closed** — Keycloak IdP CR healthy, GitLab button rendering |

## Defense-in-depth review (current code/chart, May 2026)

### 1. Auth chain — solid

Browser → ALB (TLS, ACM cert) → Traefik websecure → IngressRoute (priority
100, public-host route) → middlewares: security-headers → strip-prefix →
ip-whitelist (currently on, scheduled to drop) → **forwardAuth** →
rate-limit → `beads-viewer` Service (port 8095) → `oidcauth` middleware
(verifies `id_token` from `Authorization: Bearer` or
`X-Auth-Request-Access-Token`) → email-domain check → handler.

Higher-priority routes (priority 200) cleanly bypass `forwardAuth` only
where intentional: `/oauth2/*` (the auth provider itself) and `/login`
(unauthenticated picker page); both still pass through ip-whitelist,
security-headers, and rate-limit. Confirmed by reading
`gardenio/helm/gardenio/templates/beads-viewer/ingressroute.yaml`.

### 2. OIDC — fail-closed

`oidcauth.New` uses bounded exponential backoff (5 attempts, 1-30 s) for
provider discovery. If discovery permanently fails, every request 503s
and `/readyz` reports `not_ready`, so K8s removes the pod from the
Service. **No passthrough on failure.** Confirmed at
`controller/internal/oidcauth/oidcauth.go:96-117` and `cmd/beads-viewer/main.go:88-97`.

### 3. Email-domain gate — defense in depth

oauth2-proxy is configured with `--email-domain=*` (any) at the ForwardAuth
layer; the `acme.example,acme.example` enforcement happens **inside
beads-viewer** via `oidcauth.AllowedEmailDomains` (`deploy-values/gardenio.yaml:880`,
verified at `oidcauth.go:159-163`). This means any authenticated user
clears ForwardAuth, but only `@acme.{ai,com}` reaches a real handler.
Two-layer check is fine; it just means oauth2-proxy logs will show
"authenticated" for users we then 403, which is the abuse-logging
signal T5 wanted in Mezmo.

### 4. CSRF — adequate-by-default, no app-layer token

beads-viewer has **32 POST endpoints** (server.go:413-498) and **no CSRF
token middleware**. Mitigations are environmental:

- The session cookie (`_gardenio_oauth2`) is `SameSite=lax` (pinned
  explicitly per audit F12 at
  `helm/gardenio/templates/forwardauth/deployment.yaml:59`). Cross-origin
  POSTs from third-party sites do not carry the cookie; ForwardAuth
  rejects them at the gate.
- All state-changing endpoints sit behind ForwardAuth and OIDC; they
  cannot be reached anonymously.
- The cookie is `Secure=true`.

Residual risk: an XSS or open-redirect on a same-origin page would
defeat SameSite=Lax. The viewer's templating (Go `html/template`) is
context-aware by default; no `template.HTML` injection trickery noted in
a quick scan. Worth a focused XSS pass on user-supplied bead content
rendering (descriptions, comments, titles, advice bodies) before public
exposure — see "Recommended pre-cutover work" below.

### 5. RBAC — partial coverage of mutation surface

Operator-role gate is enforced on agent control endpoints only:

- `POST /agents/{id}/input` (send keystrokes to a running agent)
- `POST /agents/{id}/control/claim`
- `POST /agents/{id}/keys` and `/keys/cancel`
- `POST /agents/{id}/nudge`
- `POST /agents/{id}/respond`

Everything else (close/reopen/claim/release beads, edit description,
title, owner, labels, priority, assignee, add comments, submit ratings,
resolve/dismiss decisions, project bulk mutations, settings, advice
create, view create/delete, quick-spawn) is **authenticated-only**. Any
`@acme.{ai,com}` user can run them. This is the same posture as the
office-VPN era, but the public-internet move widens the population from
"on-VPN acme staff" to "any acme.{ai,com} mailbox owner".

The beads-viewer description in `deploy-values/gardenio.yaml:858` says
"Deep-linkable read-only HTML renderer". That's stale documentation —
the viewer is not read-only.

### 6. Rate limiting — per-IP, post-T5 fix

`beadsViewer.ingress.rateLimit`: 50 avg / 100 burst, with
`sourceCriterion.ipStrategy.depth: 1` (X-Forwarded-For rightmost). Same
limit applied to the public-host route, the `/oauth2/*` route, and the
`/login` route. Confirmed at
`helm/gardenio/templates/beads-viewer/ingressroute.yaml:132-153`. Per-IP
fix landed via the T5 review.

Not addressed (and probably fine for now): no separate, tighter limit on
`/login` or on Keycloak's `/realms/gardenio/protocol/openid-connect/auth`
endpoint. T5 marked "recommend if cheap"; nothing implemented. Login
bombing is bounded by Keycloak's own brute-force protection (verify on
the realm).

### 7. TLS

`tls.enabled: false` in prod (`deploy-values/gardenio.yaml:802`). ALB
terminates TLS with an ACM cert and forwards HTTP to Traefik internally.
HSTS is still applied at the security-headers middleware
(`global.securityHeaders` defaults: `stsSeconds=63072000`,
`includeSubdomains`, `preload`). Min TLS version is enforced at the ALB
listener side, not in this chart.

The chart's optional `tls.optionName` (Mozilla-intermediate TLSOption)
is **not** wired up because `tls.enabled=false`. That's only relevant
for clusters terminating TLS at Traefik (gardenio-rwx, future deploys);
not a prod concern today.

### 8. Pod hardening

`beads-viewer` deployment:
- `runAsNonRoot: true`, `runAsUser: 65534`, `fsGroup: 65534`
- `seccompProfile: RuntimeDefault`
- `allowPrivilegeEscalation: false`, `readOnlyRootFilesystem: true`,
  `capabilities.drop: [ALL]`
- `automountServiceAccountToken: false`

All good — confirmed at `helm/gardenio/templates/beads-viewer/deployment.yaml:22-42`.

### 9. Dev bypass — refuses to start in cluster

`BEADS_VIEWER_DEV_AUTH_BYPASS=1` refuses to start when the K8s SA token
is mounted (`cmd/beads-viewer/dev_auth.go:41-47`). Cannot accidentally
ship a bypassed binary to prod.

### 10. Logging

Auth-failure paths log at debug level (`oidcauth.go:137-162`). Neutral
for abuse forensics. T5 closed assuming Traefik+oauth2-proxy logs land
in Mezmo with status/source-IP/path/UA fidelity; not independently
verified in this assessment. Worth a 2-minute Mezmo query post-cutover
to confirm.

## Recommended pre-cutover work

**Must do (gating cutover):**

1. **Close T3 (ka-0000002649)** — actually walk a `@acme.example` GitLab
   account through SSO end-to-end on `gardenio-rwx` (no IP whitelist
   there) and capture screenshots/logs. Test both picker buttons + each
   picker disable flag.

2. **Land T2 (ka-0000003476)** — make ForwardAuth required (chart-side
   lint or template guard) for any public-host beads-viewer route. The
   chart today only adds the `forwardauth` middleware when
   `keycloak.forwardAuth.enabled` is true; an operator misconfig that
   sets `beadsViewer.ingress.enabled: true` with
   `keycloak.forwardAuth.enabled: false` would publish the viewer
   unauthenticated. Recommended fix: in
   `helm/gardenio/templates/beads-viewer/ingressroute.yaml`, refuse to
   render the public IngressRoute when `forwardAuth.enabled: false`
   (`{{ fail "..." }}`), or always include the forwardauth middleware
   reference (rendering will fail on missing CR if forwardAuth is
   disabled, which is the desired fail-closed mode).

**Should do (cheap, high-value before cutover):**

3. **Update the docstring** at `deploy-values/gardenio.yaml:858` to
   reflect reality — the viewer is not read-only.

4. **Pass the templates through an XSS lens** — focus on bead title,
   description, comments, advice body rendering. Go `html/template`
   defaults are safe; the risk is anywhere a handler emits
   `template.HTML` or builds HTML by string concat. Quick `grep -r
   "template.HTML\|RawHTML\|unsafe" gardenio/controller/cmd/beads-viewer`
   pass.

5. **Confirm Mezmo has Traefik + oauth2-proxy auth-failure events** —
   T5's recommendation. 2-minute check, not a blocker.

**Nice to have (post-cutover):**

6. Tighter rate limit on `/login` and `/oauth2/start` (login bombing
   defense). T5 left this open as "recommend if cheap".

7. **Decide policy on the mutation surface.** Two reasonable answers,
   both acceptable; pick one explicitly:
   - **Stay as-is** — `@acme.{ai,com}` SSO is the trust boundary,
     mutations stay on for any signed-in user. Cheapest. Aligns with
     iOS app posture.
   - **Layer in `gardenio-operator` role** — gate the broader mutation
     surface (close/reopen/edit/comment/etc.) behind operator role,
     making the viewer truly read-only for default users. Requires the
     T4-deferred role backfill. Tighter but more work.

## Cutover go/no-go checklist (T6)

Copying from ka-0000002062 with current state inline:

- [x] T1 picker page in prod (with both buttons enabled)
- [ ] T2 ForwardAuth fail-closed in chart
- [ ] T3 GitLab SSO verified end-to-end on gardenio-rwx
- [x] T4 authorization source decision implemented (email-only)
- [x] T5 rate-limit + abuse logging confirmed (per-IP fix shipped)
- [x] ka-0000003031 closed (GitLab OAuth + AWS SM secret confirmed)
- [ ] Matt explicit approval to flip the whitelist off
- [ ] Pre-cutover snapshot of who's accessing the viewer (last 7d)
- [ ] Post-cutover monitoring window (24h)

**Three boxes left** before cutover is honest: T2, T3, and Matt's
approval (with mutation-surface posture clarified at the same time).

## Out of scope for this assessment

- Keycloak's own public exposure (`auth.e2e.dev.acme.example`) — already
  public, separate posture review.
- Coopmux / agent terminal exposure — beads-viewer proxies to coopmux
  for agent terminal screen/events; coopmux is its own hardening story.
- iOS app — separate auth flow, not affected by this cutover.
