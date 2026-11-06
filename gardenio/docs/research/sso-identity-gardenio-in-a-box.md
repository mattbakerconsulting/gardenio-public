# Gardenio-in-a-Box: SSO / Identity Architecture — Research Deliverable

*Epic: ka-0000001295 · Research bead: ka-0000004895 · Single-EC2, no-cloud, public-internet box*

---

> ## ⚠️ CORRECTION (2026-06-08) — Read §0 first
>
> The operator challenged the premise — *"I thought we were Keycloak-free already in prod"* — and **they are right.**
> Live-cluster verification (gardenio prod namespace) shows the original framing below **overstates Keycloak as the
> operative identity substrate**. **Prod authenticates humans against GitLab.com OIDC directly, runs zero in-cluster
> Keycloak pods, and the only Keycloak in play is the *shared external corporate ACME Keycloak* — not a gardenio-hosted
> one.** This materially changes the recommendation: there is **no "Keycloak already works, just toggle it" default**
> for the box. See **§0 — Prod Reality Check & Corrected Framing** immediately below. The original §§1–8 are retained
> for their per-option analysis, which remains valid, but the §0 corrections take precedence wherever they conflict.

---

## 0. Prod Reality Check & Corrected Framing (authoritative)

**What I verified against the live `gardenio` prod namespace and deployed Helm release (not just chart capabilities):**

| Claim | Original framing | **Verified prod reality** | Evidence |
|---|---|---|---|
| Human browser edge | "ForwardAuth against the in-cluster Keycloak issuer" | **oauth2-proxy `--provider=gitlab`, issuer `https://gitlab.com`**, gated on GitLab group `example-org`; email domains `acme.example/.com` | live `deploy/gardenio-forwardauth` args; commit `95585987a` "gate front door on GitLab group membership" (2026-06-07) |
| In-cluster Keycloak | Default substrate; "Keycloak always present" | **ZERO** Keycloak StatefulSets/pods in `gardenio` ns; `keycloak.runtime.enabled=false` in every overlay | `kubectl get statefulset,pod -n gardenio` → none; `values/gardenio.yaml` |
| The Keycloak that *does* exist | (implied gardenio-owned) | **Shared external corporate ACME Keycloak** at `auth.e2e.dev.acme.example` (hosted on ECS, org-wide), referenced as `ClusterKeycloak/acme-keycloak` — and it reads `CONNECTED=false` | `realm.clusterKeycloakRef: acme-keycloak`; values comment "externally-managed ClusterKeycloak (today: ECS)" |
| kbeads realm-role authz | "Keycloak realm roles gate 12 admin types" | **True, but the issuer is the external ACME Keycloak**, not a box-local one. `BEADS_OIDC_ISSUER_URL=https://auth.e2e.dev.acme.example/auth/realms/gardenio`; multi-issuer GitLab acceptance (`BEADS_OIDC_ISSUER_URLS`) is **unset** in prod | live `deploy/gardenio-beads` env; `kbeads/internal/server/oidc.go:120-132`, `write_authz.go:64-80` |
| AVA | "AVA replacement ships via cookieAuth" | AVA is **still live in prod** (`edgeAuth.ava.trustSigner` = a real `vai-…` ARN; `gardenio-edge-auth` deploy running) — it is the AWS path that genuinely disappears in-a-box | live values; `deploy/gardenio-edge-auth` |

**The split prod actually runs today:** GitLab-direct OIDC at the *browser edge* (oauth2-proxy) **+** external-ACME-Keycloak realm-role JWT validation on the *beads API* (kbeads/beads-viewer). These are two different IdPs serving two different layers. Keycloak is **not** what logs humans in; it is what mints the `realm_access.roles` claim the beads API trusts for admin authz. The box has **neither** of these for free: no GitLab.com to federate to in an air-gapped box, and no corporate ACME Keycloak to validate roles against.

**Corrected implication for Gardenio-in-a-Box.** The original doc's "(a)+(b) is the default because the box already runs Keycloak" is **wrong** — the box has *never* run in-cluster Keycloak; `keycloak.runtime.enabled` has never been flipped on in any environment, so that StatefulSet path is **untested in practice**, not a turnkey default. The honest decision is therefore between two real builds, neither free:

- **Path A — Stand up the in-cluster Keycloak the chart ships but prod never used.** Flip `keycloak.runtime.enabled=true`, wire its Postgres/TLS/admin-secret, point `realm.clusterKeycloakRef` at the new in-box `ClusterKeycloak`, and **migrate the human edge off GitLab-direct onto this Keycloak** (today's oauth2-proxy `--provider=gitlab` must become `--provider=oidc` against the box Keycloak, or Keycloak must broker an external IdP). This preserves the realm-role authz model **but is a genuine first-time bring-up**, not a toggle — plus the §3 first-admin bootstrap and §5 MFA gaps still apply.
- **Path B — BYO-OIDC (original option c).** Point the edge + beads API at the operator's own OIDC/GitLab and do the kbeads role-resolver rework (8–13 dev-days) so `gardenio-{admin,operator,viewer}` derive from a non-Keycloak claim. This is closer to **what prod's *edge* already does** (GitLab-direct), so it is less of a leap than the original doc implied — the rework is real but it is not introducing a foreign architecture, it is finishing the migration the edge already started.

**Net:** the original recommendation leaned on a false "Keycloak is already the running default" premise. With that removed, **Path A (in-box Keycloak) and Path B (BYO-OIDC) are genuine peers**, and the choice turns on whether the box operator values the realm-role authz granularity (→ A) or minimal moving parts that mirror the current edge (→ B). The MFA-absent and first-admin-bootstrap blockers (§§3, 5) are real under **both** paths. This correction is why the decision should **not** be auto-greenlit as "(a)+(b)"; it needs a fresh operator pick between A and B with the true costs on the table.

> ### ✅ RESOLVED (2026-06-08) — Operator chose **Path A: in-box Keycloak** (decision `ka-0000001876`)
>
> The operator picked **Path A** — stand up the never-yet-run in-cluster Keycloak as the box's identity
> substrate, preserving the kbeads realm-role authz model. Implementation is tracked under epic
> **`ka-0000005376`** (*"Path A: stand up in-box Keycloak"*, child of mega epic `ka-0000001295` and this
> research bead). Children, mapped to the §8 phased plan:
>
> | Bead | Work | §ref |
> |---|---|---|
> | `ka-0000004234` | in-box Keycloak bring-up (flip `runtime.enabled`, Postgres/TLS/admin-secret, `ClusterKeycloak`) | §1, §7 |
> | `ka-0000006087` | migrate human edge off GitLab-direct OIDC onto box Keycloak (AVA off, GitLab-PAT off, cookieAuth on) — *blocked by bring-up* | §6 |
> | `ka-0000007117` | first-admin bootstrap Job (solve the OIDC chicken-and-egg lock-out) — *blocked by bring-up* | §3 |
> | `ka-0000001366` | **MFA epic** — mandatory TOTP/WebAuthn for `gardenio-admin` (BLOCKING for public cutover) | §5 |
> | ↳ `ka-0000006878` | gating spike: validate EDP `KeycloakRealmRequiredAction` CRD (role-conditional binding + backup codes) | §5 |
> | ↳ `ka-0000000050` | MFA impl: `required-actions.yaml` + brute-force protection + Traefik rate-limit — *blocked by spike* | §5 |
> | `ka-0000005523` | docs: single-EC2 first-admin bootstrap runbook + `port-forward` lock-out fallback — *blocked by bootstrap Job* | §3, §8 |
>
> Sequencing within the epic is deferred to the in-a-box synthesis (`ka-0000000791`). Path B (BYO-OIDC)
> is **not** pursued; it remains documented above as the escape hatch.

---

## TL;DR — Recommendation *(original — superseded by §0 where they conflict)*

**Default the box to self-hosted, in-cluster Keycloak (option a) with `login.userRegistration: false`, and ship federation (option b) as a config flip on the *same* deployment.** The decisive fact is that options (a) and (b) are not separate architectures — they are the identical Keycloak runtime with `keycloak.corporateIdp.enabled` toggled, and the chart already ships both the in-cluster Keycloak StatefulSet (`keycloak.runtime.enabled`, default OFF) and the declarative `KeycloakRealmIdentityProvider` federation CRD. Keycloak-always-present is what preserves the realm-role authorization model (`gardenio-admin`/`operator`/`viewer`) that kbeads enforces on 12 admin-gated bead types via `write_authz.go`; dropping it (option c, BYO-OIDC) is the only path that *breaks* that model and forces an 8–13 dev-day kbeads role-resolver rework. For orgs that want **no passwords on the box**, federation (b) is the recommended posture — Keycloak brokers Google Workspace / Azure AD / generic OIDC and delegates MFA upstream. **BYO-OIDC behind ForwardAuth (c) is the escape hatch**, not the default: it has the lowest infra cost but the heaviest application rework and loses the local realm-role authz substrate.

**Two hard caveats on "secure default":** (1) MFA is **completely absent** from the chart today — for a public-internet box this is a blocking gap, and mandatory TOTP (WebAuthn preferred) for `gardenio-admin` must be *built* before cutover. (2) Several no-cloud install ergonomics (Postgres auto-wiring, TLS issuer, admin-secret seeding, first-admin bootstrap) are **manual today** and need a small amount of chart work.

---

## 1. Current State — What the Chart Ships Today

| Capability | Status | Evidence |
|---|---|---|
| In-cluster Keycloak (Bitnami 26.0.7) StatefulSet + Service + IngressRoute + `ClusterKeycloak` CR | **Ships**, default OFF (`keycloak.runtime.enabled`) | `values.yaml:4017`; `templates/keycloak-runtime/{statefulset,service,ingressroute,cluster-keycloak}.yaml` |
| Declarative realm / clients / roles / groups via EDP keycloak-operator (`v1.edp.epam.com/v1`) | **Ships** | `templates/keycloak/{realm,clients,roles,groups}.yaml`; 5 roles, 5 groups, **8 clients** (`values.yaml:4273–4514`) |
| Corporate IdP federation (Google / Azure / generic OIDC) | **Ships**, default OFF (`keycloak.corporateIdp.enabled`) | `values.yaml:4534–4610`; `templates/keycloak/identity-providers.yaml:59–185` |
| GitLab OIDC IdP (generic `providerId: "oidc"`) | **Ships** | `identity-providers.yaml:1–57`; `values.yaml:4619–4681` |
| oauth2-proxy ForwardAuth (v7.15.2), pluggable `--provider=oidc` *or* `--provider=gitlab` | **Ships** | `templates/forwardauth/deployment.yaml:56–85`; `values.yaml:4705,4727,4730,4768` |
| edge-auth CLI gate: GitLab-PAT / AVA / oauth2-proxy-cookie, HMAC envelope | **Ships** | `templates/edge-auth/deployment.yaml:145–162`; `cmd/edge-auth/config.go:88–126` |
| kbeads multi-issuer JWT verifier; auto-provision `type=user` bead on first OIDC request | **Ships** | `kbeads/internal/server/oidc.go:157–243`; `user_provision.go:64–129`; `write_authz.go:66–82` |
| Realm hardening: `userRegistration:false`, email-as-username, email verification, password policy, session/token settings | **Ships** | `values.yaml:4164–4219` |
| **MFA (TOTP / WebAuthn / OTP)** | **ABSENT — not in any template or value** | (no `KeycloakRealmRequiredAction` CRD anywhere in chart) |

Important correction folded in: the chart defines **8** Keycloak clients (`gardenio-roles-server`, `gardenio-coopmux`, `gardenio-landing`, `gardenio-ios`, `gardenio-escalation`, `gardenio-beads-api`, `gardenio-traefik-forwardauth`, `gardenio-admin-api`), not 9. kbeads gates **12** bead types on `min_role: gardenio-admin` (jira-poller, advice, report, project, fleet, user, account, role, audit, rig, convoy, config), not ~14.

---

## 2. The Three Options Compared

| Dimension | (a) Self-hosted Keycloak + local users | (b) Keycloak federating external IdP | (c) BYO-OIDC behind ForwardAuth |
|---|---|---|---|
| **Infra cost on box** | Keycloak pod + Postgres (in-cluster) | Same as (a) — federation is config | Lowest: no Keycloak, no Keycloak DB, no operator |
| **kbeads rework** | None — realm roles work as-is | None — same realm-role path | **8–13 dev-days** (role-resolver shim, service-account migration, session-revocation change) |
| **Public-internet security** | Good *if* MFA added; passwords live on box | **Best**: no passwords on box; MFA delegated to corp IdP | Depends entirely on the BYO IdP; loses realm-role authz granularity |
| **No-cloud fit** | Needs Postgres/TLS/secret wiring (small) | Same wiring; needs egress to public IdP endpoints (always available) | Cleanest no-cloud infra, but breaks service-account clients + offboarding model |
| **Federation flexibility** | Flip `corporateIdp.enabled` → becomes (b) | Native — this *is* the flexibility | **Forecloses** corp-IdP + future Slack brokering without re-introducing a broker |

**Key insight:** (a) and (b) are the **same deployment**. The box always runs Keycloak; whether users get local passwords or are brokered to Google/Azure is `keycloak.corporateIdp.enabled`. This is the chart's existing `corporateIdp` design, not new work. (c) is a genuinely different architecture and a one-way door — reverting requires re-creating the Keycloak realm and re-importing users.

**Why (c) is the escape hatch, not the default:** dropping Keycloak silently breaks all 12 admin-gated bead types until a GitLab-`groups_direct` → `gardenio-{admin,operator,viewer}` role-resolver shim is built in `kbeads/internal/server/oidc.go` (`ExtractRoles` reads Keycloak `realm_access.roles` today — GitLab's userinfo does not emit that claim). It also strands the two service-account clients (`gardenio-beads-api`, `gardenio-admin-api`), which must move to the existing static-token path. **Correction:** that path's env var is **`DAEMON_HMAC_KEY`** (read by `edgeauth.ConfigFromEnv()` in `kbeads/internal/edgeauth/verify.go:83`), *not* `BEADS_HMAC_SECRET`.

---

## 3. First-Admin Bootstrap — The Secure Install-Time Flow & the Chicken-and-Egg

**The chicken-and-egg (real, must be solved before public cutover):**

1. The Keycloak **master-realm** bootstrap user (`KEYCLOAK_ADMIN` / `KEYCLOAK_ADMIN_PASSWORD`, sourced from Secret `gardenio-keycloak-runtime-admin`, `statefulset.yaml:75–84`) is a superuser of the *master realm only* — it can manage the gardenio realm but is **not** a gardenio-realm OIDC user.
2. The first human who logs in via OIDC gets auto-provisioned a `type=user` bead by `UserProvisioner.Middleware` (`user_provision.go:64–129`) — with `email`/`auth_methods` fields only. **Correction:** the bead stores *no role*; the `gardenio-viewer` role comes from Keycloak's `realm.defaultGroups: [gardenio-viewers]` (`values.yaml:4164–4165`) at token-generation time, not from kbeads.
3. So the first human lands as a **read-only viewer**. `write_authz.go` blocks every admin write (12 types). With `userRegistration:false`, there is no self-service escape. **The system locks.**

**Secure install-time flow (needs building — small):**

- Install script generates a strong master-realm admin password and seeds Secret `gardenio-keycloak-runtime-admin`. The chart does **not** auto-generate this today.
- A post-install one-shot Job (after the StatefulSet startup probe passes, before external clients connect) uses master-realm creds against the Keycloak REST API to create a gardenio-realm user `admin@<box-host>` with `emailVerified=true`, assigned to the `gardenio-admins` group (which carries the `gardenio-admin` role, `groups.yaml:239–242`), and emits the installer credential for one-time retrieval.
- Realm stays `userRegistration:false` + `defaultGroups:[gardenio-viewers]`; all subsequent users start as viewers and are promoted by the admin (or by corp-IdP group mapping under federation).
- Fallback if the hook is omitted/fails: `kubectl port-forward` to the Keycloak console and create the user manually. **This must be documented** — a missing hook is a hard lock-out.

---

## 4. Session / Cookie Security

The session/cookie posture **ships in good shape** for a public HTTPS box (`forwardauth/deployment.yaml:86–129`):

- `cookie-secure: true`, `cookie-samesite: lax` (intentionally pinned to prevent oauth2-proxy default drift; `strict` would break cross-subdomain), PKCE `code-challenge-method: S256`.
- `cookie-csrf-per-request: true` + `cookie-csrf-expire: 15m` (widened for mobile 2FA / password-reset flows).
- `cookie-refresh: 3m` (`values.yaml:4752`) — correctly **shorter** than the 5m access-token lifetime, leaving a 2m safety margin. **Correction:** the configured refresh is `3m`, not ~5m; 5m is the Keycloak *access-token* lifespan.
- Helm **fails the install** if `oidcIssuerUrl` is not `https://` (`deployment.yaml:2–4`) — fail-closed against plaintext token leakage.

Keycloak realm sessions (`values.yaml:4184–4205`): 8h SSO idle / 7d max; access token 5m; RS256.

**Open session risks:**
- `revokeRefreshToken: false` — offboarded users keep a usable refresh token for up to 7 days unless an admin revokes it via the Keycloak API. **Correction (folded across dimensions):** federated offboarding is **not immediate** — access ends when the Keycloak session expires (8h idle / 7d max). The ~5-minute revocation window people cite is the oauth2-proxy refresh cycle (cookie-refresh `3m`), not an instantaneous logout; sub-5-minute forced revocation needs a deny-list cookie layer (out of scope).
- `SameSite=lax` leaves residual same-site CSRF exposure: beads-viewer has 32 mutating POST endpoints with no CSRF tokens — mitigated by the OIDC gate but warrants a pre-cutover XSS audit of bead title/description/comment rendering.
- oauth2-proxy pinned at **v7.15.2** — no auto-update; security bumps are manual.

---

## 5. MFA — Current GAP and Recommended Default

**MFA is completely absent from the chart.** There is no TOTP, WebAuthn, or OTP configuration in `realm.yaml`, `clients.yaml`, `identity-providers.yaml`, or `values.yaml`, and **no `KeycloakRealmRequiredAction` CRD is instantiated anywhere**. (The EDP operator *supports* `KeycloakRealmRequiredAction` at `v1.edp.epam.com/v1`, but the chart never uses it — for MFA or anything else.) For a box that authenticates real users over the public internet, this is a **blocking gap** for the local-user default: one local password with no second factor owns the realm.

**Recommended MFA default:**
- **Mandatory TOTP for the `gardenio-admin` role; WebAuthn (FIDO2/passkey) preferred** where the user base supports it (phishing-resistant, origin-bound). Optional TOTP opt-in for all other local users.
- **Under federation (b), delegate MFA to the corporate IdP** — set `keycloak.corporateIdp.enabled: true` and rely on Google/Azure MFA. This is the cleanest path and avoids managing OTP secrets on the box.
- **Build work required:** add a `templates/keycloak/required-actions.yaml` instantiating `CONFIGURE_TOTP` (and/or `WEBAUTHN_REGISTER`), gated to the admin role via a conditional auth flow, with backup codes. **Open validation risk:** it is unconfirmed whether the EDP `KeycloakRealmRequiredAction` CRD exposes role-conditional flow binding and backup-code config — this must be validated against the operator's CRD schema before committing to the approach.

There is also **no per-IP rate-limit on the Keycloak login endpoint** (`/realms/gardenio/protocol/openid-connect/auth`); only Keycloak's built-in brute-force protection (which the chart does not explicitly enable) stands between a public attacker and password guessing. Enable realm brute-force protection and/or front the auth host with Traefik rate-limiting.

---

## 6. Replacing the GitLab-OIDC-via-AVA Path

AWS Verified Access (AVA) does not exist on a standalone EC2 box — the `edgeAuth.ava.trustSigner` path is impossible off-cloud. The replacement **ships today**: the **oauth2-proxy-cookie auth path** in edge-auth.

- Leave `edgeAuth.ava.trustSigner: ""` (default → AVA disabled).
- Set `edgeAuth.cookieAuth.enabled: true` with `cookieAuth.authURL` pointing at the in-cluster ForwardAuth subrequest endpoint (`http://<release>-forwardauth:4180/oauth2/auth`) and `cookieName: _gardenio_oauth2` (`edge-auth/deployment.yaml:145–162`; `cmd/edge-auth/config.go:88–126`).
- Flow: a CLI/agent request carrying the oauth2-proxy session cookie (from a prior browser `gb login`) is sub-requested to oauth2-proxy; on `200 OK` edge-auth stamps the validated `X-Auth-Request-Email` into the **same HMAC envelope** (`X-Operator` / `X-Edge-Auth-Sig` / `X-Edge-Auth-Ts`) that PAT and AVA modes produce. The daemon validator (`DAEMON_HMAC_KEY`) is unchanged.
- **Disable the GitLab-PAT fallback on the box:** set `edgeAuth.gitlabGroup: ""`. It defaults to `example-org` (`values.yaml:401`); if the box has internet egress, a PAT from a stale membership could still authenticate. Either empty the group or rely solely on ForwardAuth (web) + cookie-auth (CLI).

ForwardAuth itself is AWS-free and works as-is against the in-cluster Keycloak issuer.

---

## 7. No-Cloud Install Gaps to Close (small, chart-side)

These are the deltas between "ships today" and "installs clean on a fresh single EC2," none large:

1. **In-cluster Postgres wiring** for Keycloak — not auto-wired; operator pre-creates the `KC_DB_*` Secret pointing at the postgres Service FQDN, or chart auto-wires when `postgres.enabled=true` (~1 day).
2. **TLS without Route53** — cert-manager + Route53 DNS-01 is the documented default, but the chart already supports `tls.existingSecretName` (`values.yaml:3478–3483`); the no-cloud path is operator-pre-created TLS Secret or a self-signed/HTTP-01 issuer (~1 day if templated).
3. **Admin & DB Secret seeding** — chart does not generate passwords; add Helm-hook generation or document pre-creation (~0.5–1 day).
4. **Issuer-URL auto-derivation** from `runtime.host` + realm name instead of manual overlay (~2h).
5. **First-admin bootstrap Job** (§3).
6. **Encrypted-at-rest PVC** for Keycloak — operator must choose an encrypted StorageClass; not enforced.

Estimated chart effort for full no-cloud support: **5–10 dev-days** (0–3 if the operator accepts manual Secret pre-creation). MFA is additional.

---

## 8. Phased Recommendation & Next-Bead Suggestions

**Phase 0 — Decision checkpoint (`gb decision`):** Adopt **Keycloak-always-present** as the box's identity substrate (default (a), federation (b) as a flip). Explicitly reject BYO-OIDC (c) as default; keep it documented as the escape hatch. *Rationale: preserves the realm-role authz model, avoids the 8–13-day kbeads rework, and makes federation a one-line toggle.*

**Phase 1 — No-cloud install ergonomics (chart only, ~5–10 dev-days):** Postgres auto-wiring, TLS existing-secret/self-signed path, admin/DB secret seeding, issuer-URL auto-derivation, first-admin bootstrap Job + documented `port-forward` fallback. Disable AVA + GitLab-PAT, enable `cookieAuth`.

**Phase 2 — MFA hardening (blocking for public cutover):** Build `templates/keycloak/required-actions.yaml` (mandatory TOTP for `gardenio-admin`, WebAuthn preferred, backup codes), gated by conditional auth flow. **First** validate the EDP `KeycloakRealmRequiredAction` CRD supports role-conditional binding. Enable Keycloak realm brute-force protection and/or Traefik rate-limit on the auth host. Pre-cutover XSS audit of beads-viewer's 32 POST endpoints.

**Phase 3 — Federation enablement (optional per org):** Document the `corporateIdp.enabled: true` flip for Google/Azure/generic OIDC; pin the email-domain allowlist across all three layers (corp IdP `hd`/tenant, `forwardAuth.emailDomains`, `oidcAllowedEmailDomains`). Note GitHub is **OAuth2, not OIDC** — GitHub-direct SSO is out of scope without a broker (Zitadel/Authentik) or custom connector.

**Phase 4 — Offboarding posture:** Decide whether 8h-idle/7d-max natural expiry is acceptable, or enable `revokeRefreshToken: true` / wire admin-API session revocation for faster lockout. Sub-5-minute forced revocation = deny-list cookie layer (separate epic if required).

**Suggested next beads:**
- `feat(gardenio/helm): no-cloud install profile` — Postgres/TLS/secret/issuer wiring + first-admin bootstrap Job.
- `feat(gardenio/helm): mandatory TOTP/WebAuthn for gardenio-admin` — gated on EDP `KeycloakRealmRequiredAction` schema spike.
- `spike: validate EDP KeycloakRealmRequiredAction conditional-flow + backup-code support`.
- `feat(gardenio/helm): box auth profile` — AVA off, GitLab-PAT off, cookieAuth on, brute-force protection + auth-host rate-limit.
- `docs(gardenio): single-EC2 first-admin bootstrap runbook` including the `port-forward` lock-out fallback.
---

## Provenance & Methodology

Produced for research bead **ka-0000004895** (child of epic ka-0000001295 "Gardenio in a Box").
Methodology: a 5-dimension fan-out research workflow over the live chart and kbeads source —
(a) self-hosted Keycloak, (b) external-IdP federation, (c) BYO-OIDC, first-admin bootstrap +
AVA replacement, and session/cookie/MFA — with each dimension's factual claims independently
**adversarially grounding-verified** against the repo before synthesis. Verifier corrections
(client count 8, admin-gated bead types ~12, HMAC env `DAEMON_HMAC_KEY`, viewer-role-via-
defaultGroups not via the user bead, cookie-refresh 3m, non-instant federated offboarding) are
folded into the body. Citations are `file:line` against the chart state at the time of writing
(`gardenio/helm/gardenio/...`, `kbeads/internal/server/...`); re-verify line numbers before acting.

Operator: matthew.baker@acme.example. This is a research deliverable — no code changed.
