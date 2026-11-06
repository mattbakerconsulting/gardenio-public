# Removing Keycloak from the Ingress: Direct GitLab IdP Assessment

**Tracking bead:** ka-0000007266
**Question (Matt, 2026-05-04):** Can we drop Keycloak from our ingress and just
use GitLab IdP directly? How hard / how tricky? Make a plan if it's smart.

## TL;DR

**Smart, but not cheap.** Pointing ForwardAuth (`oauth2-proxy`) at GitLab
instead of Keycloak is a one-day change. The painful part is what Keycloak
*also* does today that GitLab doesn't: realm-role-based authorization in
`kbeads` write policies, service-account clients for two confidential APIs,
and federated identity for the iOS / corporate-IdP flows. Removing Keycloak
without breaking those requires a role-translation shim in `kbeads`, a new
identity strategy for two service accounts, and a session-revocation
replacement for offboarding.

Estimated effort: **2–3 weeks of focused work**, mostly in `kbeads` and the
helm chart, plus an outage-window cutover. Recommend doing it in **two
phases** (see §6) with a deliberate hold-point between them.

---

## 1. What Keycloak gives us today

Sourced from `gardenio/helm/gardenio/values.yaml` (`keycloak.*`), the
`keycloak-operator` CRDs in `templates/keycloak/`, `oidcauth.go`, and
`kbeads/internal/server/{interceptors,oidc,write_authz}.go`.

| Capability | Where it lives today | Used by |
|---|---|---|
| OIDC IdP at the ingress | `oauth2-proxy` v7.8.1, issuer `https://auth.e2e.dev.acme.example/auth/realms/gardenio` | All protected ingresses (beads-viewer, beads3d, coopmux, grafana, roles-server, slack-bridge UI, beads API, landing) |
| 6 public OIDC clients (PKCE SPAs) | `keycloak.clients[]` — `gardenio-beads3d`, `gardenio-roles-server`, `gardenio-coopmux`, `gardenio-landing`, `gardenio-ios`, `gardenio-escalation` | SPA login flows that don't go through ForwardAuth (e.g. iOS, dev-mode beads3d) |
| 2 confidential clients | `gardenio-beads-api` (service account, `gardenio-admin` role), `gardenio-admin-api` (client-credentials grant for session revocation) | Internal service-to-service auth + offboarding |
| 1 ForwardAuth confidential client | `gardenio-traefik-forwardauth` | oauth2-proxy → Keycloak code exchange |
| Realm roles | `gardenio-admin`, `gardenio-operator`, `gardenio-viewer`, `escalation-approver` | `kbeads` `write_policy.min_role` (config.go: ~14 type configs are admin-only); `oidcauth.RequiredRole` on viewers; escalation API approval check |
| Realm groups | `gardenio-admins`, `gardenio-operators`, `gardenio-viewers`, `gardenio-escalation-approvers` | Group-claim mapper drops `groups` into the JWT; consumed where role resolution wants finer-grained mapping |
| Identity providers | `gitlab` (already federated), `corporate` (Google/Azure, configurable) | First-broker login; today most users come in via the `gitlab` IdP |
| Local user accounts | Keycloak realm DB | Anyone who isn't in the `gitlab` IdP (small set; mostly carry-overs) |
| Session revocation | `keycloak.admin.*` + `keycloak_admin.go` (`kbeads`) | Offboarding agent / user account |
| Centralized logout | `/protocol/openid-connect/logout` | force-logout-on-403 path, `gb logout`, `donk-clear` |
| Per-environment realm reuse | `keycloak.realm.create=false` | `gardenio-rwx` reuses the prod realm |

**Important fact:** `kbeads` already accepts JWTs from *multiple* OIDC issuers
via `BEADS_OIDC_ISSUER_URLS` (kbeads/internal/server/oidc.go: `multiOIDCVerifier`
— "enables accepting JWTs from both Keycloak (web SSO via ForwardAuth) and
GitLab (CLI SSO via gb login)"). The CLI auth path is **already** GitLab-direct.
The web/ingress path is what's still routed through Keycloak.

## 2. What "use GitLab IdP directly" actually means

oauth2-proxy ships a first-class `--provider=gitlab` mode with native
group-restriction (`--gitlab-group=<g1,g2>`), wildcard cookie-domain support,
and the same `X-Auth-Request-*` header injection ForwardAuth already relies
on. The mechanical swap is small:

```yaml
# Today
oidcIssuerUrl: https://auth.e2e.dev.acme.example/auth/realms/gardenio
clientId:      gardenio-traefik-forwardauth
scope:         openid email profile groups

# After
provider:      gitlab
oidcIssuerUrl: https://gitlab.com
clientId:      <new-gitlab-application-id>
clientSecret:  <new-gitlab-application-secret>
gitlabGroup:   acme/platform/gardenio
scope:         openid email profile read_user
```

But "the rest" doesn't follow for free. Three structural problems show up
the moment you do this on a green deployment:

### 2.1. The realm-role authorization model has no GitLab equivalent

`kbeads` write policies are gated on `min_role: gardenio-admin` at ~14 type
configs (`type:audit`, `type:rig`, `type:convoy`, `type:config`, escalation
beads, etc.). Today the role list is sourced from
`token.realm_access.roles` — a Keycloak-shaped claim that GitLab does not
emit. Without translation, every admin operation 403s.

**Replacement options:**
- **(a) Group → role mapping in the JWT verifier.** Add a config knob in
  `kbeads/internal/server/oidc.go` (or a new `roleresolver` package): when
  the token issuer is GitLab, extract `groups_direct` and translate
  configured GitLab paths (`acme/platform/gardenio-admins` etc.) into
  internal `gardenio-admin`/`gardenio-operator`/`gardenio-viewer`. Keep the
  `RoleViewer/Operator/Admin` constants stable. Lowest blast radius.
- **(b) User-bead capability check.** Already partially built — the
  `UserDirectory` resolves token email/preferred_username to a user bead.
  Add `capabilities` (or reuse an existing labels field) to user beads, and
  switch `write_authz.go` to consult that instead of token claims. Cleaner
  long-term, but introduces a per-request DB lookup on the write path and
  needs a real migration plan for existing users.

Option (a) is the right v1 — it preserves the JWT-claims-are-source-of-truth
model and is purely a verifier change. Option (b) is the right v3 if we ever
want to break with JWT-encoded authorization (kept out of scope here).

### 2.2. Service-to-service auth loses its substrate

Two confidential clients today are *not* user-driven:

- `gardenio-beads-api` — service account with `gardenio-admin` realm role,
  used by internal services to write admin-gated bead types.
- `gardenio-admin-api` — client-credentials grant; the only consumer of
  Keycloak's admin REST API for session revocation on offboarding.

GitLab does not issue OIDC tokens via client-credentials. We need
replacements:

- **For `gardenio-beads-api`:** use a per-service GitLab Project/Group
  Access Token, or — simpler — switch internal callers to the existing
  `BEADS_HMAC_SECRET` static-token path (`internal/server/interceptors.go`
  authMethod=static). The static path is already used in the cluster for
  bridge → beads calls; expanding it for the few additional callers is
  cheap and avoids putting an OAuth flow in the middle of a controller
  reconcile loop.
- **For `gardenio-admin-api`:** the only thing it does is *revoke Keycloak
  sessions*. If Keycloak is gone, there is nothing to revoke. The
  offboarding flow becomes "remove user from GitLab group → next refresh
  fails → cookie expires within 5 min." Forced immediate revocation goes
  away unless we add a deny-list cookie at oauth2-proxy. Acceptable
  trade-off for most threat models, but worth flagging explicitly.

### 2.3. The federation point disappears

Today, Keycloak federates *both* a `gitlab` IdP and an optional
`corporate` IdP (Google Workspace / Azure / generic OIDC). All apps see
one issuer. If a user signs in with Google, they show up at gardenio
through Keycloak's broker-login flow.

If we delete Keycloak, every gardenio user must have a GitLab account.
For Acme this is plausible — engineers all have GitLab — but it
forecloses on:
- Stakeholders who only have Google/Azure accounts
- The currently-disabled `corporateIdp` knob in values.yaml (lines
  2837–2900)
- Any future "sign in with Slack" via Keycloak identity brokering (the
  Phase-2 enhancement called out in `escalation-auth-recommendation.md`)

If those are real requirements, the answer is **don't delete Keycloak —
keep it but make GitLab the *only* IdP within the realm.** That gets you
~80% of the simplification (one user database to administer in practice,
one IdP to configure) without losing the broker layer.

## 3. Difficulty rating per workstream

| Area | Effort | Risk | Notes |
|---|---|---|---|
| oauth2-proxy → GitLab provider switch | 0.5d | Low | Config change in `helm/gardenio/values.yaml` + GitLab OAuth app registration. Test against `gardenio-rwx`. |
| Helm chart cleanup (delete `templates/keycloak/*`) | 1d | Low | Mechanical. ~9 templates, ~100 lines values.yaml. |
| `kbeads` role-resolver shim (GitLab `groups_direct` → internal role) | 2–3d | **Medium** | Net-new code in `internal/server/oidc.go` + `interceptors.go` + tests. Needs feature flag + config for the GitLab group→role table. |
| Service-account replacement (beads-api, admin-api) | 2–3d | **Medium** | Migrate internal callers off Keycloak service accounts. Static HMAC token path exists; needs audit. |
| iOS OAuth client — needs GitLab app | 1–2d | Medium | New GitLab application + iOS code path change. Out of scope here if the iOS app isn't actively used. |
| `oidcauth.RequiredRole` callers (beads-viewer, slack-bridge, advice-viewer) | 1d | Low | Same role-translation; reuses §3.3. |
| Session-revocation rework (or accept refresh-window) | 0.5d (accept) / 3–5d (deny-list cookie) | Low–Medium | Recommend "accept refresh window" in v1; revisit if a real offboarding incident demands sub-5-min revocation. |
| `gardenio-rwx` realm reuse retirement | 0.5d | Low | One less thing to keep in sync; net negative complexity. |
| Cutover (rotate cookies, rebuild sessions, run E2E) | 1d + outage window | **Medium** | All authenticated users get bounced once. Plan a low-traffic window; pre-dispatch the GitLab consent screen for known users via email. |
| Documentation + runbook updates | 1d | Low | `secret-rotation.md`, `escalation-auth-recommendation.md`, ingress runbooks all reference Keycloak. |

**Total: ~12–18 dev-days, single owner.** Lower bound assumes happy path
on the rwx environment; upper bound assumes one nontrivial production
incident during cutover.

## 4. What's load-bearing and would *break loudly* if missed

1. **`kbeads` `write_policy.min_role` enforcement** — silent breakage of
   admin bead types until the role-resolver is in place.
2. **`gardenio-admin-api` session-revocation** — only consumer of
   `keycloak.admin.*`. Drop without replacement and offboarding becomes a
   "wait 5 min" operation. **Not silent**, but the contract change must
   be communicated.
3. **`escalation-approver` realm role** — escalation web UI gates
   approvals on this role. Needs to map to a GitLab subgroup (e.g.
   `acme/platform/gardenio-escalation-approvers`).
4. **The `traefik-forwardauth` client secret rotation pipeline** — today
   the secret comes from RWX vault and is referenced by both Keycloak (as
   the imported client secret) and oauth2-proxy. Removing Keycloak means
   only one consumer, which simplifies things — but the rotation runbook
   in `docs/secret-rotation.md` needs updating.
5. **`gardenio-rwx` shared-realm assumption** — `keycloak.realm.create=false`
   in the rwx overlay. Removing Keycloak means rwx and prod no longer
   share a realm; their GitLab OAuth app registrations are independent.

## 5. What's *not* a problem

- **kbeads CLI auth.** Already GitLab-direct via `BEADS_OIDC_ISSUER_URLS`
  and `gb login`. No CLI changes needed.
- **Cookie domain / SSO across subdomains.** oauth2-proxy with
  `cookieDomain: .e2e.dev.acme.example` works identically against any OIDC
  provider.
- **PKCE for SPAs.** If we keep ForwardAuth as the single auth boundary,
  the SPAs do not need their own GitLab OAuth applications — they read
  identity from the `X-Auth-Request-*` headers oauth2-proxy injects, exactly
  as they do today. (The current per-SPA Keycloak public clients are only
  used in dev-mode bypasses; verify before deletion.)
- **`donk-clear` / 403 force-logout.** The middleware-errors machinery
  bounces through `/oauth2/sign_out`, which is oauth2-proxy's endpoint, not
  Keycloak's. It stays as-is.

## 6. Recommended plan (if we proceed)

**Phase 1 — De-risk and shrink the dependency surface (no user-visible change).**

1. Land the role-resolver shim in `kbeads`. Make it issuer-aware: when
   the token issuer matches a configured GitLab issuer, translate
   `groups_direct` → `realm_access.roles`-shaped output. Default-off
   behind `BEADS_GITLAB_GROUP_ROLE_MAP=`. Add tests covering the
   admin/operator/viewer translations and the no-match fallback.
2. Migrate `gardenio-beads-api` callers to the static HMAC token path.
   Remove the `serviceAccount` block from the Keycloak client def.
3. Enumerate iOS client status — is it actively used? If not, delete
   `gardenio-ios` outright. If yes, register a GitLab OAuth app and stage
   the change in the iOS repo.
4. Stand up a parallel `gardenio-rwx` deployment with
   `keycloak.enabled=false` and the GitLab-direct ForwardAuth config.
   Run the existing E2E suite against it. **Hold-point: human review +
   `gb decision` before flipping prod.**

**Phase 2 — Flip prod and clean up.**

5. Cutover: switch `keycloak.enabled=false` in the prod values file
   atomically with the GitLab OAuth app registration; users get bounced
   to GitLab once; sessions re-establish.
6. Wait one cookie-lifetime + one refresh-window for the long tail.
7. Delete `templates/keycloak/*` from the chart, drop the
   `keycloak-operator` dependency, decommission the realm CR. Update
   `docs/secret-rotation.md` and `docs/runbooks/ingress-trusted-ips.md`.
8. Strip the now-dead Keycloak admin-API code from `kbeads` and update
   `escalation-auth-recommendation.md` (whose Phase-2 federation
   recommendation no longer applies).

A `gb decision` checkpoint between Phase 1 and Phase 2 is non-negotiable —
the cutover is single-direction (revert means re-creating the Keycloak
realm + re-importing users).

## 7. Open questions for the operator

1. Is the iOS app in active use? (Determines whether we delete the
   client or invest in a GitLab OAuth app for it.)
2. Is the corporate-IdP federation expected to come back? If yes,
   deleting Keycloak forecloses on it without re-introducing a broker
   later.
3. Is sub-5-minute session revocation a real requirement, or is the
   "wait one refresh window" model acceptable for offboarding?
4. Are there gardenio users who don't have a GitLab account today?

If the answers are *(no, no, acceptable, no)*: this is straightforwardly
worth doing.

If any answer is *yes*: keep Keycloak as the federation point, but lock
its IdP list down to GitLab-only. Gets ~70% of the simplification with
none of the migration risk.

## 8. Pointers

- Current Keycloak config: `gardenio/helm/gardenio/values.yaml` lines
  ~2400–3015 (`keycloak:` block) and `templates/keycloak/*.yaml`.
- ForwardAuth middleware: `templates/forwardauth/middleware.yaml`,
  `templates/forwardauth/deployment.yaml`.
- OIDC verifier (multi-issuer already present):
  `kbeads/internal/server/oidc.go`.
- Role-source-of-truth in beads: `kbeads/internal/server/interceptors.go`
  (`RoleViewer/Operator/Admin`), `write_authz.go` (`min_role`),
  `config.go` (per-type policies).
- Prior auth recommendation (Keycloak as federation point):
  `gardenio/plans/archive/escalation-auth-recommendation.md` — that doc's
  Phase-2 "use Keycloak identity brokering" recommendation is the
  counterargument to this proposal; the current question (Matt, 2026-05-04)
  is asking us to revisit it.
