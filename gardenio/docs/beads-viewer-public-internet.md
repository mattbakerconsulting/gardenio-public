# beads-viewer → Public Internet — Research & Plan

> **⚠️ SUPERSEDED in part (T6 Keycloak permadelete, ka-0000006953).** The
> "Keycloak (local) vs GitLab" picker and every `keycloak.*` realm / identity-provider
> / `kc_idp_hint` mechanism described below assumed the in-chart Keycloak realm as
> the fleet front door. That realm has been removed (`templates/keycloak/*`); the
> fleet browser edge is now oauth2-proxy directly against the environment OIDC issuer
> (GitLab on `gardenio-rwx`/CI/staging, Entra/GitLab in prod), so there is no local
> Keycloak login page or provider-picker to toggle. Treat the auth-mechanism sections
> as **historical design context only**; the public-exposure / IP-whitelist / rate-limit
> analysis remains relevant. (The in-box Gasbox IdP still uses Keycloak — separate work.)

**Status:** Research / plan-of-record. **No impl until human approval.**
**Tracking bead:** ka-0000006029
**Source thread:** Slack 2026-04-25 (Matt)
**Parent epic candidate:** reopen ka-0000005905 (deferred 2026-04-17) or new epic.

## Goal

Drop the office-VPN IP whitelist on `beads-viewer.gardenio.e2e.example.com` and
serve the viewer on the public internet, gated only by SSO. Add a login landing
page that lets users pick **Keycloak (local)** or **GitLab**, with a feature
flag to disable either provider for cutover.

## Today (current state)

| Concern | Prod (`gardenio`) | Staging (`gardenio-rwx`) |
|---|---|---|
| Host | `beads-viewer.gardenio.e2e.example.com` | `beads-viewer.gardenio-rwx.e2e.example.com` |
| Auth | Keycloak ForwardAuth (oauth2-proxy → realm `gardenio`) | Same |
| Email gate | `acme.example` only (`oidcAllowedEmailDomains`) | Same |
| Realm role | None (any authenticated `@acme.example` user) | Same |
| IP whitelist | **enabled, `203.0.113.5/32`** | disabled |
| Rate limit | 50 avg / 100 burst | 50 avg / 100 burst |
| TLS | `gardenio-wildcard-tls` | Same |

**Auth chain:** browser → Traefik → IP whitelist → ForwardAuth (oauth2-proxy)
→ Keycloak `gardenio` realm → email-domain check at oauth2-proxy → service.

## What's already in place for GitLab SSO

The Helm chart has the GitLab IdP wiring complete:

- `gardenio/helm/gardenio/templates/keycloak/identity-providers.yaml` ranges
  over `keycloak.identityProviders[]` and emits a `KeycloakRealmIdentityProvider`
  CR for each entry. The `gitlab` alias is the special-case that pulls
  `clientSecret` from `keycloak.gitlabIdpClientSecret` (a scalar value injected
  by deploy, not in the array — this avoids array-merge issues).
- `gardenio/helm/gardenio/values.yaml` (default `keycloak.identityProviders`)
  defines the `gitlab` IdP with `displayName: "GitLab"`, mappers for
  `username`/`email`/`firstName`/`lastName`/`groups`, `syncMode: IMPORT`,
  `trustEmail: true`.
- The `clientId` is hardcoded in chart defaults:
  `57ab1f8bf23244ada4ccbc9dbafadbdf03931be30e7d1f85b4fb7b3fe32383e6`.
- `externalSecrets.gitlabIdp.remoteRef: "gardenio/gitlab-idp-client-secret"`
  (set in `deploy-values/gardenio.yaml`) — pulls `client-id` and
  `client-secret` from AWS Secrets Manager into the K8s Secret
  `gardenio-gitlab-idp-credentials`.
- `.rwx/deploy.yml` injects `keycloak.gitlabIdpClientSecret` from the RWX
  secret `GARDENIO_PROD_KEYCLOAK_IDP_SECRET` on prod deploys.

**Implication:** the chart and deploy plumbing are complete. We just need
confirmation that the upstream pieces (GitLab OAuth Application + AWS SM
secret + RWX secret) actually exist and are correct, and that the IdP is
visibly working on the Keycloak login page.

The corporate IdP federation is also chart-ready and currently disabled
(`keycloak.corporateIdp.enabled: false`).

## What "rock solid for the public internet" means

Removing the IP whitelist removes a (thin) layer of defense. To stay safe we
need the rest of the stack to be airtight:

1. **No bypass of ForwardAuth.** Audit `beadsViewer/ingressroute.yaml` and
   confirm there is no path that skips the `forwardauth` middleware. Today
   the chart conditionally adds it iff `keycloak.forwardAuth.enabled`. We
   should make ForwardAuth **required** for any public host (fail closed if
   it's somehow not wired).
2. **Email-domain claim verified end-to-end.** oauth2-proxy enforces
   `emailDomains` (today: `acme.example`). Once GitLab is live, we need to
   decide: does a `@acme.example` GitLab account count? (Probably yes — but
   `trustEmail: true` on the GitLab IdP means we trust whatever email GitLab
   sends.) If we'd rather rely on GitLab group membership, switch to a group
   claim check instead of email-domain.
3. **Keycloak realm role.** Today `gardenio-viewer` is `isDefault: true` but
   not enforced (because pre-existing users never got it). Decide whether
   to backfill the role on existing users and re-enable enforcement, or
   keep relying on the email-domain gate.
4. **Rate limit + abuse handling.** 50/100 is fine for office-VPN traffic;
   the public internet will get crawler/scanner noise. The Traefik
   rate-limit Middleware in the gardenio chart is **not** per-IP today —
   it has no `sourceCriterion`, so it buckets by the LB connection peer.
   See `docs/research/beads-viewer-rate-limit-review.md` for the fix
   (add `sourceCriterion.ipStrategy.depth: 1` to all rate-limit
   Middlewares) and the full rate-limit + abuse-logging review.
5. **Logging.** Auth failures and rate-limit rejections need to land in
   Mezmo with enough fidelity to investigate abuse.
6. **Keycloak's own exposure.** `auth.e2e.dev.acme.example` is already public;
   confirm it has not regressed (admin endpoints not exposed, brute-force
   protection on, etc.). Out of scope of beads-viewer per se but on the
   blast radius.

## Login landing page

**Matt's ask:** a landing page that lets users pick Keycloak or GitLab,
with a flag to disable either side.

Two implementation paths:

### Option A — use Keycloak's stock login page

Today (once GitLab IdP is live), Keycloak's login page already shows a
"GitLab" button alongside the local username/password form. To approximate
Matt's spec:
- "Disable Keycloak local login" → set the realm's `loginWithEmailAllowed`
  / `registrationAllowed` flags or a custom login flow that hides the
  password form. Keycloak doesn't have a one-click toggle for this; it
  requires either a theme override or a custom auth flow.
- "Disable GitLab" → flip `keycloak.identityProviders[gitlab].enabled: false`
  (or remove the IdP CR via Helm).
- "Disable Corporate" → flip `keycloak.corporateIdp.enabled: false`.

Cheap. Ugly-ish login page. No code change in beads-viewer.

### Option B — beads-viewer-side picker (RECOMMENDED for Matt's spec)

Add a small `/login` page to beads-viewer (or a dedicated `auth-picker`
service) that renders two buttons:

- **Keycloak (local):** redirect to Keycloak login with no `kc_idp_hint`
  → user gets the local username/password form.
- **GitLab:** redirect to Keycloak login with `kc_idp_hint=gitlab`
  → Keycloak skips its picker and goes straight to GitLab.

Two values flags drive visibility:

```yaml
beadsViewer:
  loginPicker:
    enabled: true                 # render picker; false → straight-to-keycloak
    keycloakLocal:
      enabled: true
      label: "Keycloak (local)"
    gitlab:
      enabled: true
      label: "GitLab"
```

This is the cleanest match for Matt's "feature flag to disable either" — an
operator can toggle either button in deploy values without touching Keycloak.

The actual auth still runs through Keycloak; `kc_idp_hint=gitlab` is a
documented Keycloak query parameter. Both paths land in the same oauth2-proxy
session, which means downstream services don't change.

**Where it lives:** the picker is a static page; `oauth2-proxy` is what
actually unauthenticates the user. The unauthenticated landing page can be
served by oauth2-proxy's `--sign-in-page` template, or as a
`PathPrefix(/login)` IngressRoute that bypasses ForwardAuth and points at
a tiny handler in beads-viewer.

## Asks of GitLab admin / Keycloak admin

Filed as **escalation bead** (see linked bead). Concrete questions:

1. Confirm GitLab OAuth Application exists at the Acme GitLab tier (or
   gitlab.com) with:
   - Client ID `57ab1f8bf23244ada4ccbc9dbafadbdf03931be30e7d1f85b4fb7b3fe32383e6`
   - Redirect URI `https://auth.e2e.dev.acme.example/auth/realms/gardenio/broker/gitlab/endpoint`
   - Scopes: `openid profile email`
   - Active (not expired/revoked).
2. Confirm AWS Secrets Manager entry `gardenio/gitlab-idp-client-secret` has
   `client-id` and `client-secret` keys, and that the RWX secret
   `GARDENIO_PROD_KEYCLOAK_IDP_SECRET` matches.
3. (Verification step) Visit `https://auth.e2e.dev.acme.example/auth/realms/gardenio/account/`
   and confirm a "GitLab" button appears on the login page.
4. If any of (1)–(3) fail: rotate / recreate as needed.

If the OAuth app does not exist, the admin needs to create one with the
fields above. The chart will pick up the new secret on next deploy with no
code change.

## Proposed child tasks (not yet filed)

- T1 — Login picker page (beads-viewer-side, `kc_idp_hint`).
- T2 — IP-whitelist removal in prod values + harden ForwardAuth fail-closed.
- T3 — Verification dry-run on `gardenio-rwx` (already has IP whitelist off).
  Walk through: GitLab SSO works end-to-end; picker disable flags work.
- T4 — Realm role / email-domain decision: keep email-only gate, or layer
  in `gardenio-viewer` realm role with backfill.
  **RESOLVED 2026-04-27 (decision ka-0000002658 → `t4-email`):** keep
  email-only gate. See [T4 resolution](#t4-resolution-email-only-authorization-gate-2026-04-27)
  below.
- T5 — Rate-limit and abuse-logging review for public exposure.
- T6 — Cutover: prod IP-whitelist off after T1–T5 are green.

## Open questions for Matt

- **Whose GitLab?** Acme's self-hosted GitLab, or gitlab.com Acme
  group? The redirect URI tells GitLab admins which instance.
- **What does "disable either one" govern at the picker level?** Hide the
  button only, or also reject the upstream auth? (Both, probably — defense
  in depth.)
- **GitLab group as authorization source?** Today we trust the email
  domain. We could instead trust GitLab group membership
  (`@example-org` etc) — more accurate, but couples access to GitLab
  org graph.
- **Cutover order.** OK to land picker page first (still IP-whitelisted)
  and remove whitelist in a later release?

## T4 resolution: email-only authorization gate (2026-04-27)

**Decision:** ka-0000002658 (operator, 2026-04-27) → option `t4-email`.
**Bead:** ka-0000001205.

### What we keep

`oauth2-proxy` continues to enforce `allowedEmailDomains: "acme.example,acme.example"`
as the sole authorization gate (see `deploy-values/gardenio.yaml:712-716`). The
`gardenio-viewer` realm role remains `isDefault: true` for new Keycloak users
but is **not** enforced at the proxy. No backfill of pre-existing users is
required.

### Why email-only is sufficient under the new threat model

1. Both IdPs feed the same email-domain check.
   - Keycloak local realm: users provisioned by ops have a `acme.example`/
     `acme.example` email by policy.
   - GitLab IdP: `trustEmail: true` in the IdP config means we trust the
     email GitLab returns, but only `@acme.{ai,com}` accounts pass the
     oauth2-proxy gate.
2. Email-domain matches the existing access policy for sibling viewers
   (gardenio landing, beads3d) — same posture, no new asymmetry.
3. Defense in depth at the kbeads API layer: write actions are still gated
   by the API's own RBAC; the viewer is read-only.
4. Layering in the realm role would require a one-time backfill across all
   pre-existing acme Keycloak users (the original reason `requiredRole`
   was removed — see comment at `deploy-values/gardenio.yaml:786-794`). The
   added security delta vs. email-domain is small for this audience.

### What this unblocks

- **T2 (ka-0000003476) — chart fail-closed + drop IP whitelist.**
  - Make ForwardAuth required for any public host (today the chart
    conditionally adds it iff `keycloak.forwardAuth.enabled`; the chart
    should refuse to render the public IngressRoute if ForwardAuth isn't
    wired). No new authorization config needed — email-only is already
    live.
  - Remove the `203.0.113.5/32` entry at
    `deploy-values/gardenio.yaml:809-812` (`beadsViewer.ingress.ipWhitelist`)
    by setting `enabled: false` (matches the pattern used by `landing` and
    `beads3d`).
- **T6 (ka-0000002062) — prod cutover.**
  - Once T2 ships and T3 (GitLab SSO E2E on gardenio-rwx) is green, the
    cutover is the same single-line change above plus a release.

### Risk register (unchanged from plan)

- Public exposure with email-domain only is the same posture we run for
  other gardenio viewers today; the IP whitelist on `beads-viewer` was the
  only outlier.
- Rate-limit + abuse-logging review (T5, ka-0000001362) closed with
  R1 sourceCriterion fix already merged. Re-verify that the fix is in the
  release that ships T2.

### Non-goals

- We are **not** revisiting the GitLab group claim path. If a future
  audit demands tighter authorization, file a follow-up with a backfill
  plan; do not gate this cutover on it.
- We are **not** adding the realm-role enforcement now. The `isDefault`
  bit on `gardenio-viewer` stays as-is for the case where we change our
  mind later.
