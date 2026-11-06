# In-box Keycloak MFA hardening

**Beads:** task ka-0000000050 · MFA epic ka-0000001366 · gating spike ka-0000006878
· research ka-0000004895 §5 (`docs/research/sso-identity-gardenio-in-a-box.md`).

This chart work closes the "MFA completely absent" gap for the public-internet
gardenio-in-a-box: it makes a second factor **mandatory for the `gardenio-admin`
role**, and adds a per-IP rate limit in front of the public Keycloak login
endpoint.

**Live state.** The k3s box runs `keycloak.mfa.factor: passkey` since 2026-10-18
(S4b gd-wO9MDjpz13 of plan gd-QXq1fjYuRD): an admin signs in with a passkey, or
with a password followed by a passkey. TOTP is retired: it was the live factor
before that, and S4c (gd-GiZfavru5G) deleted the remaining TOTP credentials after
a 13-day soak. `totp` and `webauthn` stay available as factor values (rollback).

## Why not `required-actions.yaml`?

The original plan-of-record assumed a `KeycloakRealmRequiredAction` CRD
(`v1.edp.epam.com/v1`) to make `CONFIGURE_TOTP` / `WEBAUTHN_REGISTER` mandatory
for the admin role. The gating spike **ka-0000006878 disproved that**: the CRD
does not exist in the deployed EDP keycloak-operator (v1.32.0) — nor upstream.
The implementation pivoted to the spike's validated, fully-CRD-native
alternative.

## How it works

### 1. Mandatory MFA — custom browser flow (`KeycloakAuthFlow`)

`templates/keycloak/auth-flow-mfa.yaml` renders three `KeycloakAuthFlow` CRs
that compose a clone of Keycloak's built-in `browser` flow, with the
conditional-OTP subflow re-gated on a **role** instead of "user has OTP
configured":

```text
gardenio-mfa-browser                  topLevel browser flow
  ├─ auth-cookie                            ALTERNATIVE   (existing SSO session)
  ├─ identity-provider-redirector           ALTERNATIVE   (corporate IdP)
  └─ gardenio-mfa-browser-forms              ALTERNATIVE subflow
       ├─ auth-username-password-form       REQUIRED
       └─ gardenio-mfa-browser-conditional-mfa  CONDITIONAL subflow
            ├─ conditional-user-role        condUserRole=gardenio-admin
            └─ webauthn-authenticator       REQUIRED   (factor webauthn; auth-otp-form for totp)
```

With `factor: passkey` (the live k3s value) the conditional subflow gains a
middle step and a different last one (design doc
`docs/superpowers/specs/2026-10-18-keycloak-passkeys-login-theme-design.md` 4.1):

```text
       └─ gardenio-mfa-browser-conditional-mfa  CONDITIONAL subflow
            ├─ conditional-user-role                condUserRole=gardenio-admin
            ├─ conditional-credential               skip when a passkey was the primary credential
            └─ webauthn-authenticator-passwordless  REQUIRED
```

A passkey picked in the username box (autofill or **Sign in with Passkey**)
authenticates in `auth-username-password-form` and the subflow is skipped; a
typed password is followed by a passkey prompt, so an admin never gets in
without one.

The `CONDITIONAL` subflow evaluates to **REQUIRED for users holding the
`gardenio-admin` realm role** and to **DISABLED for everyone else** — so only
admins are forced through the second factor; all other local users authenticate
with username+password exactly as before.

Each subflow is its own CR referencing its parent's alias via `spec.parentName`
(the EDP operator model — it calls `AddChildFlowToFlow` and reconciles each CR
independently with retry, so apply ordering is eventually consistent).

The custom flow is bound to the realm by `templates/keycloak/realm.yaml` setting
`spec.browserFlow: gardenio-mfa-browser` when `keycloak.mfa.enabled`.

**Enrollment:** with `totp` or `webauthn` the second-factor execution enrols:
an admin with no credential is prompted to set one up inline on next login (the
authenticator auto-triggers its `webauthn-register` / `CONFIGURE_TOTP` required
action). With `passkey` it does **not**: Keycloak's passwordless authenticator
does not offer user setup, so an admin with no passkey is refused ("credential
setup required"). Admins enrol a passkey in the account console first
([`keycloak-passkeys-runbook.md`](keycloak-passkeys-runbook.md) section 1). No
realm-wide required action is needed — and none is CRD-expressible.

**The factors.** `keycloak.mfa.factor` picks the second factor:

| `factor` | Authenticator | Enrols inline | Notes |
|---|---|---|---|
| `passkey` | `webauthn-authenticator-passwordless` behind `conditional-credential` | no | **Live on k3s.** Phishing-resistant, origin-bound. A passkey sign-in needs nothing more. Needs Keycloak ≥ 26.4 and the WebAuthn Passwordless policy (`keycloak.runtime.realmHardening.passkeys`). |
| `webauthn` | `webauthn-authenticator` | yes | Chart default. FIDO2 security key or passkey as a second factor after the password. |
| `totp` | `auth-otp-form` | yes | Authenticator-app codes. Retired on k3s (S4c); kept only as a last-resort rollback, after which every admin enrols again. |

The in-box runtime image (`keycloak.runtime.image`, Keycloak 26.x) supports all
three.

### 2. Auth-host rate limit (Traefik)

`templates/keycloak-runtime/ingressroute.yaml` emits a per-source-IP `rateLimit`
Middleware and attaches it to the runtime IngressRoute. This is the front-line
guard against credential-stuffing on `/realms/<realm>/protocol/openid-connect/auth`.

It is the *implementable* mitigation because **the EDP `KeycloakRealm` CRD
exposes no brute-force / `failureFactor` field** (verified live against the
v1.32.0 operator schema — its spec is `browserFlow, browserSecurityHeaders,
displayHtmlName, displayName, frontendUrl, id, keycloakRef, login,
organizationsEnabled, passwordPolicy, realmEventConfig, realmName, sessions,
smtp, themes, tokenSettings, userProfileConfig, users`). Realm-level Keycloak
brute-force protection therefore cannot be configured from this chart today; it
needs either a future CRD field or a Keycloak admin-API step, tracked as a
follow-up.

Defaults (`keycloak.runtime.ingress.rateLimit`) are tuned so real interactive
logins (which fan out to several sub-requests) are never throttled, while a
single stuffing source is bounded: `average: 50/min`, `burst: 100`,
`ipStrategyDepth: 1` (read the client IP the trusted edge LB appended to XFF).

## Enabling

Both features are **off by default** so customer-poc / no-operator installs
render zero `KeycloakAuthFlow` CRs and no rate-limit Middleware. Flip on per-env
(e.g. the in-box overlay) once the in-cluster Keycloak runtime is live:

```yaml
keycloak:
  mfa:
    enabled: true
    adminRole: gardenio-admin    # role forced through the second factor
    factor: webauthn            # or "passkey" (live on k3s) or "totp"
  runtime:
    enabled: true
    ingress:
      rateLimit:
        enabled: true           # on by default once runtime.enabled
```

## Tests

`tests/keycloak_mfa_test.go` pins: default-off renders zero flows / no
`browserFlow`; enabled renders the 3-CR conditional chain gated on the admin
role; the WebAuthn default, the `totp` swap and the `passkey` flow (its
`conditional-credential` step); role configurability; and the
auth-host rate-limit Middleware presence/attachment and its disable path.
`tests/traefik_keycloak_gate_test.go` already counts `KeycloakAuthFlow` in its
both-off / independent-toggle gates.

## Enrollment, recovery and break-glass

Click-by-click passkey enrollment, the lost-one-device and lost-every-passkey recipes,
what to do when the flow itself is broken, and a read-only drill that proves the recipe
live are in [`keycloak-passkeys-runbook.md`](keycloak-passkeys-runbook.md). Every
command in it is run against a real Keycloak by CI.

## Residual / follow-ups

- **Realm brute-force protection** is not CRD-expressible (above) — Traefik
  rate-limit is the front line; native Keycloak brute-force is a follow-up.
- **Recovery** from a lost factor is not a CRD field either: an admin who loses
  one passkey removes it and enrols another; one who loses every passkey is
  re-enrolled by the laptop operator with `kcadm`
  ([`keycloak-passkeys-runbook.md`](keycloak-passkeys-runbook.md) sections 3
  and 4).
- **XSS audit of beads-viewer's mutating POST endpoints** (research §4) is a
  separate, optional pre-cutover item and is not part of this chart change.
