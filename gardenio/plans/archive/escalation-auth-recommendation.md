# Authentication for Escalation Web UI: Recommendation

**Task:** ka-0000003291
**Epic:** ka-0000001184 (Privilege Escalation Pipeline)
**Date:** 2026-04-05
**Phase:** Phase 1 — Foundation

## Executive Summary

**Recommendation: Use the existing Keycloak OIDC infrastructure with oauth2-proxy
ForwardAuth, adding a new `gardenio-escalation` client and an `escalation-approver`
realm role.**

This is the lowest-effort, highest-security option. The entire auth stack already
exists and is battle-tested on reports-viewer, advice-viewer, beads3d, coopmux, and
the landing page. The escalation UI needs only a new Keycloak client entry and a
role-based authorization check — no new OAuth provider integration, no new session
management, no new dependencies.

---

## Current Auth Landscape (What Already Exists)

### Infrastructure (Helm chart)

| Component | Status | Details |
|---|---|---|
| **Keycloak realm** | Deployed | `gardenio` realm via EDP keycloak-operator CRD |
| **oauth2-proxy** | Deployed | ForwardAuth deployment (`quay.io/oauth2-proxy/oauth2-proxy:v7.8.1`) |
| **Traefik ForwardAuth middleware** | Deployed | Routes auth to oauth2-proxy, passes `X-Auth-Request-*` headers |
| **OIDC clients** | 6 public + 2 confidential | beads3d, advice-viewer, reports-viewer, roles-server, coopmux, landing, beads-api, traefik-forwardauth |
| **Realm roles** | 3 defined | `gardenio-admin`, `gardenio-operator`, `gardenio-viewer` |
| **Realm groups** | 3 defined | `gardenio-admins`, `gardenio-operators`, `gardenio-viewers` |

### Go Libraries (already in go.mod)

| Library | Version | Purpose |
|---|---|---|
| `github.com/coreos/go-oidc/v3` | v3.17.0 | OIDC token verification |
| `golang.org/x/oauth2` | v0.34.0 | OAuth2 flows |
| `github.com/go-jose/go-jose/v4` | v4.1.3 | JOSE/JWT handling |

### Existing Auth Code

| File | What it does |
|---|---|
| `internal/oidcauth/oidcauth.go` | HTTP middleware — validates OIDC Bearer tokens, checks `realm_access.roles` |
| `cmd/reports-viewer/main.go` | Uses `oidcauth.Middleware` with `OIDC_ISSUER_URL`, `OIDC_CLIENT_ID`, `OIDC_REQUIRED_ROLE` env vars |
| `cmd/advice-viewer/main.go` | Same pattern as reports-viewer |
| Both viewers | Fallback chain: OIDC → basicAuth → IP whitelist |

### User Identity Model (user beads)

Fields on `type:user` beads (indexed in `UserDirectory`):

| Field | Maps to OAuth provider |
|---|---|
| `slack_id` | Slack OAuth (via Slack user ID) |
| `github_username` | GitHub OAuth (via login claim) |
| `email` | Google OAuth / any OIDC (via email claim) |
| `gitlab_username` | GitLab (via preferred_username) |
| `display_name` | Any provider (via name claim) |

### Current Decisions Web UI (the baseline)

- Served at `/ui/` on the slack-bridge HTTP server (`bridge/web/index.html`)
- API at `/api/decisions/{id}/resolve` — **no authentication**, `responded_by` is free-text
- SSE proxy at `/api/decisions/events`
- IngressRoute uses ForwardAuth middleware when `keycloak.enabled=true`

**Gap:** Even with ForwardAuth at the ingress level, the application-level API
does not verify the authenticated identity. The `responded_by` field accepts any
string. For decisions this is acceptable; for escalations it is not.

---

## Options Evaluated

### Option A: Keycloak OIDC via ForwardAuth (RECOMMENDED)

**How it works:** Same pattern as every other gardenio web UI.

1. Traefik ForwardAuth middleware intercepts requests to the escalation UI
2. oauth2-proxy handles the OIDC Authorization Code flow with Keycloak
3. On success, Traefik injects `X-Auth-Request-Email`, `X-Auth-Request-User`,
   and `X-Auth-Request-Access-Token` headers
4. The escalation API validates the access token using `oidcauth.Middleware`
5. Role check: token must contain `escalation-approver` in `realm_access.roles`
6. Identity resolution: email from token → `UserDirectory.LookupByEmail()` → user bead ID

**What needs to be added:**

| Change | Where | Effort |
|---|---|---|
| New Keycloak client `gardenio-escalation` | `values.yaml` keycloak.clients[] | ~20 lines YAML |
| New realm role `escalation-approver` | `values.yaml` keycloak.roles[] | ~3 lines YAML |
| New realm group `gardenio-escalation-approvers` | `values.yaml` keycloak.groups[] | ~5 lines YAML |
| IngressRoute for escalation UI | New template or extend slack-bridge route | ~30 lines YAML |
| Role extraction from `X-Auth-Request-Access-Token` | Extend `oidcauth` or new middleware | ~50 lines Go |
| Identity resolution (email → user bead) | Use existing `UserDirectory.LookupByEmail` | ~10 lines Go |
| `approved_by` set to user bead ID (not free text) | Escalation resolve handler | ~5 lines Go |

**Pros:**
- Zero new infrastructure — Keycloak, oauth2-proxy, ForwardAuth already deployed
- Zero new Go dependencies — `go-oidc/v3`, `oauth2` already in go.mod
- Proven pattern — identical to reports-viewer and advice-viewer
- Single sign-on — users already have Keycloak accounts
- Role-based access — Keycloak realm roles map directly to escalation capabilities
- Audit trail — Keycloak logs all authentication events
- Session management handled by oauth2-proxy (cookie-based, configurable TTL)

**Cons:**
- Requires Keycloak to be enabled (`keycloak.enabled: true` in values.yaml)
- Users must exist in Keycloak (currently not auto-provisioned from Slack)

**Fallback for non-Keycloak deployments:** The existing `oidcauth.Middleware`
already handles this — when `OIDC_ISSUER_URL` is empty, it falls through to
basicAuth. For escalations, basicAuth is insufficient (no identity), so the
escalation UI should require OIDC and return a clear error if unconfigured.

---

### Option B: Slack OAuth (Direct)

**How it works:** Users authenticate via Slack's OAuth 2.0 flow. The bridge
already has a Slack app with bot/user scopes.

**What would be needed:**
- New OAuth 2.0 flow endpoints (`/auth/slack/login`, `/auth/slack/callback`)
- Slack OAuth app configuration (user token scopes: `identity.basic`, `identity.email`)
- Session management (JWT or server-side sessions)
- Identity resolution: Slack user ID → `UserDirectory.LookupBySlackID()`

**Pros:**
- Natural fit — all gardenio users have Slack accounts
- Direct mapping — `slack_id` on user beads is the most populated identity field
- No Keycloak dependency

**Cons:**
- **New OAuth flow to implement** — ~200-300 lines of Go for the auth flow, session management, CSRF protection
- **New dependency** — need to manage Slack OAuth app credentials separately
- **Slack-only** — doesn't support users who aren't in the Slack workspace
- **Session management** — must implement JWT or cookie-based sessions from scratch
- **Scope creep** — the Slack app currently uses bot scopes only; adding user OAuth scopes changes the app's permission model
- **Duplicates existing infrastructure** — Keycloak already handles this

**Verdict:** Viable but wasteful. Builds a parallel auth system when one already exists.

---

### Option C: GitHub OAuth (Direct)

**How it works:** Users authenticate via GitHub OAuth Apps or GitHub Apps.

**What would be needed:**
- GitHub OAuth App registration
- OAuth flow endpoints
- Session management
- Identity resolution: GitHub username → `UserDirectory.LookupByGitHubUsername()`

**Pros:**
- GitHub accounts are universal among developers
- `github_username` is populated on user beads

**Cons:**
- **New infrastructure** — GitHub OAuth App, client secret management
- **New code** — full OAuth flow + session management (~300 lines)
- **Coverage gap** — not all gardenio users necessarily have GitHub accounts mapped
- **Org restriction needed** — must restrict to specific GitHub org to prevent random GitHub users from authenticating
- **Duplicates existing infrastructure**

**Verdict:** Even more redundant than Slack OAuth. GitHub adds nothing Keycloak doesn't already provide.

---

### Option D: Google OAuth (Direct)

**How it works:** Users authenticate via Google OAuth using their work email.

**Pros:**
- Email-based — maps to `email` field on user beads
- Google Workspace restriction — can limit to `@company.com` domain

**Cons:**
- Same "new OAuth flow" overhead as B and C
- Not all users may have Google Workspace accounts
- **Duplicates existing infrastructure**

**Verdict:** Same story. Keycloak can federate Google as an identity provider if needed.

---

### Option E: API Key Auth (for programmatic access)

**How it works:** Users generate API keys (stored as user bead metadata) and
pass them via `Authorization: Bearer <key>` or `X-API-Key` header.

**What would be needed:**
- API key generation and storage (new fields on user beads or separate key beads)
- Key validation middleware
- Key → user bead ID resolution

**Pros:**
- Essential for CLI/programmatic escalation approval (`gb escalation approve`)
- Simple to implement (~100 lines)
- No browser required

**Cons:**
- Not suitable as the primary auth mechanism for a web UI
- Key management (rotation, revocation) adds operational burden
- No SSO experience

**Verdict:** Useful as a **supplementary** mechanism alongside OIDC, specifically
for CLI workflows. Should be Phase 2 scope — not needed for the initial web UI.

---

### Option F: Multi-Provider (Keycloak-federated)

**How it works:** Keycloak acts as the single OIDC provider but federates
upstream identity providers (Slack, GitHub, Google) via its Identity Brokering
feature.

**Pros:**
- Users can "Sign in with Slack" or "Sign in with GitHub" while the application
  only talks to Keycloak
- Single auth integration in code; provider flexibility in Keycloak admin
- Automatic account linking by email

**Cons:**
- Requires Keycloak admin configuration for each federated provider
- Each provider needs OAuth app registration on their platform
- More operational complexity

**Verdict:** Excellent long-term direction. Keycloak's identity brokering is the
right way to add Slack/GitHub/Google sign-in without building separate OAuth
flows. This can be added incrementally without changing the application code —
it's purely Keycloak configuration. **Recommend as a Phase 2 enhancement.**

---

## Recommendation

### Phase 1: Keycloak OIDC (Option A)

Use the existing Keycloak + oauth2-proxy + ForwardAuth stack. Specific changes:

**1. New Keycloak realm role: `escalation-approver`**

```yaml
# values.yaml keycloak.roles[]
- name: escalation-approver
  description: "Can approve privilege escalation requests"
```

This is deliberately separate from `gardenio-admin` and `gardenio-operator` to
support least-privilege assignment. An operator can manage beads without being
able to approve infrastructure escalations.

**2. New Keycloak group: `gardenio-escalation-approvers`**

```yaml
# values.yaml keycloak.groups[]
- name: gardenio-escalation-approvers
  description: "Users authorized to approve escalation requests"
  realmRoles:
    - escalation-approver
```

**3. New Keycloak client: `gardenio-escalation`**

```yaml
# values.yaml keycloak.clients[]
- clientId: gardenio-escalation
  name: "Escalation Approval UI"
  public: true
  redirectUris:
    - "https://escalation.*/\052"
  webOrigins:
    - "+"
  attributes:
    post.logout.redirect.uris: "+"
    pkce.code.challenge.method: "S256"
  protocolMappers:
    - name: realm-roles
      protocol: openid-connect
      protocolMapper: oidc-usermodel-realm-role-mapper
      config:
        claim.name: realm_roles
        jsonType.label: String
        multivalued: "true"
        id.token.claim: "true"
        access.token.claim: "true"
        userinfo.token.claim: "true"
```

**4. Application-level auth middleware**

Extend `oidcauth.Middleware` (or wrap it) to:
- Extract email from the verified token claims
- Resolve email → user bead via `UserDirectory.LookupByEmail()`
- Inject the user bead ID into the request context
- Check `escalation-approver` role for approval endpoints

```go
// Pseudocode for the resolve handler
func (a *EscalationAPI) handleApprove(w http.ResponseWriter, r *http.Request, id string) {
    userBeadID := auth.UserBeadIDFromContext(r.Context()) // set by middleware
    if userBeadID == "" {
        writeJSONError(w, http.StatusUnauthorized, "authenticated user has no user bead")
        return
    }
    // ... approve with cryptographic identity
    err := a.client.ApproveEscalation(r.Context(), id, ApproveRequest{
        ApprovedBy:     userBeadID,      // user bead ID, not free text
        ApprovalMethod: "web_ui",
    })
}
```

**5. Session management: oauth2-proxy cookies**

No custom session management needed. oauth2-proxy handles:
- Cookie-based sessions (`_gardenio_oauth2` cookie)
- Token refresh via Keycloak refresh tokens
- Session TTL controlled by Keycloak token settings (currently 5min access, 30min idle SSO)

For escalation approvals, the 5-minute access token lifetime is appropriate —
approvals are point-in-time actions, not long-lived sessions.

### Phase 2: Enhancements (future)

1. **Keycloak identity brokering** (Option F) — Add Slack and/or GitHub as
   federated identity providers in Keycloak. Users get "Sign in with Slack"
   without any application code changes.

2. **API key auth** (Option E) — For `gb escalation approve` CLI workflow.
   Generate API keys stored as user bead metadata, validated by a separate
   middleware path.

3. **User bead auto-provisioning** — When a user authenticates via Keycloak
   but has no user bead, auto-create one from token claims (email, name).

---

## Session Management: JWT vs Server Sessions

**Not applicable.** oauth2-proxy handles session management via encrypted
cookies. The application is stateless — it validates the access token on each
request using the `go-oidc` verifier against Keycloak's JWKS endpoint.

If API key auth is added in Phase 2, it is also stateless (key → user bead
lookup on each request). No server-side session store is needed.

---

## Compliance Considerations

| Requirement | How Keycloak OIDC addresses it |
|---|---|
| **Authenticated identity** | OIDC token contains verified email + user ID |
| **Audit trail** | Keycloak audit logs + escalation bead `approved_by` (user bead ID) + `approved_at` (RFC3339) + `approval_method` ("web_ui") |
| **Role-based access** | `escalation-approver` realm role checked in middleware |
| **Token security** | RS256 signed JWTs, 5min lifetime, refresh rotation |
| **Session security** | Encrypted cookies, HTTPS-only, SameSite, configurable domain |
| **PKCE** | S256 code challenge method on all public clients |
| **Account linking** | Email-based resolution to user beads (extensible to multi-field) |

---

## Identity Resolution: Mapping OAuth to User Beads

The critical link between "who authenticated" and "who approved" in the bead
system:

```
OIDC Token (email claim)
    ↓
UserDirectory.LookupByEmail(ctx, email)
    ↓
UserIdentity.BeadID  →  escalation.approved_by
```

**Edge case: authenticated user has no user bead.** Two options:
1. **Reject** (recommended for Phase 1) — return 403 with "user not found in
   gardenio directory; contact an admin to create your user bead"
2. **Auto-create** (Phase 2) — create a user bead from token claims

Rejecting is safer for Phase 1 because the user bead is the source of truth
for capabilities/authorization. Auto-creating a user bead without capabilities
would authenticate but not authorize — the user still couldn't approve.

---

## Summary: Why Not Direct OAuth Providers?

Building direct Slack/GitHub/Google OAuth flows would:

1. **Duplicate infrastructure** that Keycloak already provides
2. **Add ~300 lines of auth flow code per provider** (CSRF, callback, token exchange, session management)
3. **Create multiple session management paths** instead of one
4. **Require separate OAuth app registrations** and secret management per provider
5. **Not compose** — each provider is a silo; Keycloak composes them via federation

The only scenario where direct provider OAuth makes sense is if Keycloak is
not deployed. But Keycloak is a prerequisite for the reports-viewer,
advice-viewer, beads3d, coopmux, and landing page — if those work, Keycloak
is available.
