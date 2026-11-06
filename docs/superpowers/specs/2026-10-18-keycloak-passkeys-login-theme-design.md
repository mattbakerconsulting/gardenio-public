# Passkey sign-in and a custom gardenio login theme — design

Task: gd-spSCaJAumF. Plan: gd-QXq1fjYuRD (epic). Owner request, 2026-10-18:
"could we plan a custom keycloak login page too? also, need device passkeys
working so we dont need authenticator". The owner approved the defaults A–E
below the same day ("sonds good to me yep"), so they are decisions, not
proposals. Sections 8–9 answer F and the open bug.

## 1. Goal

1. People sign in to gardenio with a device passkey (Windows Hello, Touch ID /
   Face ID, an Android or iPhone passkey, a hardware key). No authenticator
   (TOTP) app.
2. `auth.clonk.tv` shows a gardenio-branded login on every screen a user sees:
   username, password, passkey prompt and registration, errors, logout, and
   the `kd login` device-code pages. gardenio is the owner's career demo, so
   it is a first impression and has to look deliberate and match the beads
   viewer.

Non-goals: recovery codes, a password-less realm for every user, SAML,
rebuilding the account console, changing any non-browser client.

## 2. Measured state (laptop, 2026-10-18)

| Thing | Value |
|---|---|
| Runtime | StatefulSet `gardenio-keycloak-runtime-0`, `quay.io/keycloak/keycloak:26.0.7`, args `[start]` |
| Storage | Postgres (`KC_DB_URL jdbc:postgresql://gardenio-postgres.gardenio.svc.cluster.local:5432/keycloak`). Credentials survive restarts. Pod volumes `data` and `tmp` are emptyDir |
| Host | `KC_HOSTNAME=https://auth.clonk.tv/auth`, `KC_HTTP_RELATIVE_PATH=/auth`, `KC_PROXY_HEADERS=xforwarded`, behind the public-edge nginx. Issuer `https://auth.clonk.tv/auth/realms/gardenio`, so the WebAuthn RP ID will be `auth.clonk.tv` |
| Edge exposure | `auth.clonk.tv` proxies only `^~ /auth/realms/gardenio/` and `^~ /auth/resources/` (GET/HEAD) plus the `/__edge_canary`; `/auth/admin` and `realms/master` are 404 (`gardenio/helm/gardenio-ziti/public-edge.yaml`). The account console lives under `realms/gardenio/account`, so it is reachable |
| Config as code | EPAM keycloak-operator **1.32.0** CRs from the chart. `KeycloakRealm gardenio-realm` (`themes.loginTheme: "keycloak"`, `browserFlow: gardenio-mfa-browser`) and three `KeycloakAuthFlow` CRs in `templates/keycloak/auth-flow-mfa.yaml` |
| Live flow | `auth-cookie` ALT, `identity-provider-redirector` ALT, `forms` ALT = `auth-username-password-form` REQUIRED + a CONDITIONAL subflow (`conditional-user-role` = `gardenio-admin`) holding `auth-otp-form` REQUIRED. Admins: password + TOTP |
| Values | `keycloak.mfa.{enabled, adminRole, flowAlias, factor}`. Chart default `factor: webauthn`; **`deploy-values/gardenio-k3s.yaml:1039` sets `mfa: {enabled: true, factor: "totp"}`** (owner decision gd-RTo5vEgg9S) |
| Users | `matthewbaker1110@gmail.com` (owner, `gardenio-admins`), `akashdixit@gmail.com` (teammate, no group; onboarding gd-llFKMS8Pkc) |
| Hardening precedent | Brute-force protection is applied by the first-admin hook with `kcadm` (`realmHardening.bruteForce`, `templates/keycloak-runtime/first-admin-job.yaml`), because the CRD lacks the field |
| Clients | 13 `KeycloakClient`s in `deploy-values/gardenio-k3s.yaml` (section 8) |

## 3. Decisions

### A. Passkey model — passwordless first, password+passkey as the fallback path

Passwordless passkey is the primary sign-in: discoverable credentials, user
verification required, conditional UI (autofill) on the username field. The
user taps the username box, picks the passkey, and is in. Hardware keys, which
cannot do autofill, use the "Sign in with Passkey" button (modal UI).

TOTP is retired for admins once each admin has two passkeys enrolled.

The target browser flow keeps the password step available, but **an admin never
gets in without a passkey**: a passkey used as the primary credential skips the
second factor, and a password sign-in is followed by a passkey as the second
factor. So "passwordless is not workable on some device" degrades to
password+passkey, not to password alone. Non-admins are unchanged (password,
optionally a passkey). Section 4 draws it.

### B. Keycloak version — upgrade first, as its own phase

Passkeys with conditional and modal UI are supported (not preview) from
Keycloak **26.4**; 26.0.7 only has them as a preview feature. Facts gathered
2026-10-18 from the Keycloak 26.4 release post and upgrading guide, and the
EPAM operator's GitHub releases; the preflight step re-reads them on the day:

- 26.4 integrates passkeys in the built-in browser flow and in "all forms
  containing username or password fields"; the Server Administration Guide
  says no browser-flow edit is needed for the built-in flow. It adds a
  "Conditional - credential" authenticator that skips 2FA when a passkey was
  the primary credential.
- The 26.3 "Passkeys Conditional UI authenticator" is deprecated. Do not use it.
- The EPAM operator's generated client targets Keycloak **26.5.2**; the operator
  has no published compatibility matrix. The newest operator release found is
  1.35.0 (adds realm brute-force support); the cluster runs 1.32.0.
- Keycloak 26.8.0 has breaking changes (IdP button redesign in login themes,
  `view-clients` no longer returns secrets, `login-failures` stored in the DB,
  Organizations schema change; EPAM issue #420 reports `KeycloakOrganization`
  is incompatible with 26.8). Our chart renders no `KeycloakOrganization`, but
  the login-theme and brute-force changes touch this plan.

**Target version.** The owner approved "the latest 26.x". The preflight step
(P2-S1) picks the newest 26.x patch whose release notes it has read end to end
and whose operator reconcile it has checked, expected 26.5–26.7, and records
the choice and the reason on the step bead. 26.8.x is excluded from this plan
unless the preflight clears each item above; moving past the chosen line is a
separate small upgrade after sign-in works. This is the one place the plan is
more cautious than the brief, and the reason is that three things (operator
client, theme templates, brute-force storage) all move at 26.8.

**Backup before the roll.** A `pg_dump -Fc` of the `keycloak` database, copied
off the cluster, with a `pg_restore --list` check and a restore into a scratch
database (P2-S3). Keycloak's schema migration is forward-only, so rolling the
image back after it ran needs the restore. Sessions are persistent in 26.x, so
a roll does not sign everybody out; expect one to two minutes of failed
sign-ins (single replica).

### C. Theme technology — FreeMarker theme extending `keycloak.v2`

A login theme `gardenio` with `parent=keycloak.v2`: CSS, a logo, message
overrides, and template overrides only where CSS cannot do it. No Keycloakify
(it adds a JS build and a second toolchain for a handful of screens).

- **Delivery: a ConfigMap, not an image layer.** The assets are text (CSS,
  `.ftl`, `.properties`, an SVG logo) and well under the 1 MiB ConfigMap limit.
  The viewer uses the system font stack, so there are no web fonts to ship. The
  chart renders the ConfigMap from `files/keycloak-theme/gardenio/**` and
  mounts it at `/opt/keycloak/themes/gardenio` (ConfigMap keys cannot contain
  `/`, so the template maps each file to its path with `items`). No image
  build, no calver release needed to change a color. If the theme ever outgrows
  the limit, move it to an image layer; nothing else changes.
- **Selection:** `KeycloakRealm.spec.themes.loginTheme: gardenio` (the chart
  already renders this from `keycloak.realm.themes.loginTheme`); optional
  `accountTheme` and `emailTheme` the same way.
- **Production caches templates.** A theme change needs a pod restart
  (checksum annotation on the ConfigMap triggers it on `helm upgrade`).
- **Rule for template overrides.** Keep the passkey hooks intact: the
  `autocomplete="username webauthn"` token on the username input and the
  passkey script includes the parent template pulls in. A pretty template that
  loses them silently disables autofill, so the harness (below) tests
  conditional UI with the theme on.

**Look.** Match the beads viewer (`gardenio/controller/cmd/beads-viewer/templates/layout.html`):
system sans-serif (`font-family: sans-serif` in the baseline skin, mono only for
ids), hairline borders (`#ccc`), 3–4 px radii, flat surfaces, a single accent.
Light = the `baseline` skin (`--accent #1a73e8`, text `#111827`, page `#fff`,
alt `#f3f4f6`, rule `#d1d5db`). Dark follows `prefers-color-scheme` using the
viewer's dark skin (`--bg #0b0f14`, `--text #d6e0ec`, `--accent #6ee7b7`). The
theme step reads the exact token values from `layout.html` when it starts and
quotes them in its PR, because the viewer's skins change.

**Screens.** Every template a user can hit: `login`, `login-username`,
`login-password`, `login-config-totp` (until TOTP is gone), `webauthn-authenticate`,
`webauthn-register`, `webauthn-error`, `login-update-password`, `login-reset-password`
and `login-verify-email`, `error`, `info`, `logout-confirm`, the OAuth grant and
device pages for `kd login` (`login-oauth-grant`, `login-oauth-user-code`,
`login-page-expired`), and the passkey registration prompt. Mobile (360 px) and
desktop (1280 px), light and dark, are part of acceptance.

**Email and account theme.** Cheap, so included as a last, optional step: an
email theme that reuses the same tokens, and an account theme that is CSS only
(`keycloak.v3` parent) so the page where people add a passkey matches.

**Preview harness.** A CI job starts a Keycloak container (`start-dev`) with
the theme mounted and a realm import using the same flow, drives it with
Playwright, and uploads desktop and mobile screenshots as an artifact. The
Chromium DevTools virtual authenticator (`WebAuthn.addVirtualAuthenticator`,
CTAP2, resident key, user verification) lets the same job register a passkey
and sign in with conditional UI, so the flow and the theme are tested together
without a human. If the ARC runners cannot run Docker, the step escalates to
run the same script on the laptop operator and attaches the screenshots to the
PR. The repo already has Playwright (`gardenio/tests/slack-e2e`).

### D. Enrollment and migration without lockout

Order (a hard rule: **TOTP is removed from the flow only after both admins
have two passkeys**):

1. Deploy the theme. No auth change.
2. Upgrade Keycloak (backup first).
3. Turn on passkeys as an optional credential: the WebAuthn Passwordless
   policy only. The flow still requires TOTP of admins.
4. The owner enrolls two passkeys (laptop and phone). Akash enrolls one or two
   (he is not an admin; see section 9). Enrollment is through the account
   console, not a chart-managed required action.
5. Only then: ship the new flow dormant, then flip `keycloak.mfa.factor` to
   `passkey`, and later delete the TOTP credentials.

Each step has a verification and a rollback (section 6). Steps 3 (live roll),
4 and 5 are owner-gated: they change the live login or need a person at a
device, and each is an escalation with exact click-by-click instructions.

### E. Recovery

Two passkeys per admin (laptop and phone) plus the break-glass reset
(section 7). Losing one device: sign in with the other, remove the dead
passkey in the account console, add a replacement. Losing both: break-glass.

## 4. Target design

### 4.1 Browser flow (`keycloak.mfa.factor: passkey`)

The alias stays `gardenio-mfa-browser`, so `KeycloakRealm.browserFlow` does
not change. `totp` and `webauthn` stay valid factor values (rollback).

```text
gardenio-mfa-browser                         topLevel
  ├─ auth-cookie                               ALTERNATIVE
  ├─ identity-provider-redirector              ALTERNATIVE
  └─ gardenio-mfa-browser-forms                ALTERNATIVE subflow
       ├─ auth-username-password-form          REQUIRED   passkey conditional UI + modal button when the policy enables passkeys
       └─ gardenio-mfa-browser-conditional-mfa CONDITIONAL subflow
            ├─ conditional-user-role           REQUIRED   condUserRole=gardenio-admin
            ├─ conditional-credential          REQUIRED   true unless the primary credential was a passkey
            └─ webauthn-authenticator-passwordless  REQUIRED
```

Reading it: a user who picks a passkey in the username box is authenticated by
`auth-username-password-form`; `conditional-credential` sees a passkey was the
primary credential, the subflow is DISABLED, and they are in. A user who types
a password gets the subflow, and for admins it demands a passkey. Non-admins
skip the subflow either way.

Details the build step must confirm against the target Keycloak, by copying
what Keycloak ships, not from memory:

- The `conditional-credential` config keys (read them from
  `kcadm get authentication/flows/browser/executions` on the upgraded server).
- That `webauthn-authenticator-passwordless` (not `webauthn-authenticator`) is
  the right second factor: passkeys registered through "Webauthn Register
  Passwordless" have credential type `webauthn-passwordless`, and the
  non-passwordless authenticator will not accept them.
- ~~That a REQUIRED authenticator with no credential attaches the
  `webauthn-register-passwordless` required action, as `auth-otp-form` does
  today.~~ **Measured (S4a, harness run on 26.7.5): it does not.** The
  passwordless authenticator's factory does not allow user setup, so the sign-in
  of an admin who has a password and no passkey stops with "Cannot login,
  credential setup required." (HTTP error page, no session). `auth-otp-form` and
  `webauthn-authenticator` do enrol; `webauthn-authenticator-passwordless` does
  not. The flow therefore **fails closed** for an admin with no passkey, which is
  stronger than the "walk them through registering" this section first assumed,
  and it changes two things: (1) enrolment happens *before* the admin role is
  held (the order in 3.D already requires it: the owner enrolls two passkeys
  before the flip), and (2) a new admin, or one who lost every passkey, is
  re-enrolled with the recipe in section 7. The harness proves both halves
  (scenario 4).

### 4.2 WebAuthn Passwordless policy (kcadm, in the first-admin hook)

The CRD cannot express it, so it follows `realmHardening.bruteForce`: a new
`keycloak.runtime.realmHardening.passkeys` block, applied idempotently by
`kcadm update realms/gardenio` in the post-upgrade hook, failure logged and
non-fatal like bruteForce.

| Setting | Value | Why |
|---|---|---|
| Enable Passkeys | on | turns on conditional and modal UI (26.4+) |
| RP ID | `auth.clonk.tv`, set explicitly | pins the identity of every enrolled passkey |
| RP name | `gardenio` | shown in the OS dialog |
| User verification | required | biometric or PIN; this is what makes one passkey a real two factors |
| Require discoverable credential | yes | needed for username-less autofill |
| Attestation | none | no AAGUID allow-list; we do not need to know the device model |
| Authenticator attachment | not specified | platform keys and roaming keys both allowed |
| Signature algorithms | ES256, RS256 | what Windows Hello, Apple, Android and FIDO2 keys use |
| Avoid same authenticator registering twice | yes | stops accidental double-enrollment of one device |

The exact realm-attribute names (they start `webAuthnPolicyPasswordless`) are
read off `kcadm get realms/gardenio` on the upgraded server by the build step.

**Two consequences to carry:**

- **The RP ID is the issuer host.** If `auth.clonk.tv` ever changes (the ziti
  plan gd-P78ygCUgA9 keeps it, but any future move would), every passkey stops
  working and must be re-enrolled. The chart test pins RP ID == the host of
  `keycloak.runtime.hostnameUrl`.
- **U2F-only security keys cannot do discoverable credentials plus user
  verification.** A FIDO2 key with a PIN works; an old U2F key does not.
  Windows Hello, Touch ID, Android and iPhone all work.

Synced passkeys (iCloud Keychain, Google Password Manager, a password manager)
are accepted: attestation is none, so we cannot and do not distinguish them.

### 4.3 Where each piece of config lives

| Piece | Mechanism |
|---|---|
| Theme files, mount | chart `files/keycloak-theme/**`, ConfigMap, StatefulSet volume |
| `loginTheme` / `accountTheme` / `emailTheme` | `KeycloakRealm` CR (existing values keys) |
| Flow | `KeycloakAuthFlow` CRs (existing template, new `factor: passkey` branch) |
| Keycloak image | `keycloak.runtime.image.tag` in `deploy-values/gardenio-k3s.yaml` |
| Passkey policy | `kcadm` in the first-admin hook (`realmHardening.passkeys`) |
| Which factor is live | `deploy-values/gardenio-k3s.yaml` `keycloak.mfa.factor` |

No admin-console click-ops. **One reconcile risk to test, not assume:** the
operator rewrites parts of the realm and users on every reconcile (this is
bug gd-KVIQQ3H1Nr: it drops a user's required actions). The bruteForce
precedent shows realm attributes set by the post-upgrade hook survive a roll,
but nobody has checked they survive the operator's periodic resync with no roll.
P3-S1 measures that (read the attributes, wait one resync, read again). If they
drop, the fix is to re-run the kcadm from a CronJob, not to stop using kcadm.
For the same reason **nothing in this plan depends on a user-level required
action**: enrollment is through the account console.

## 5. Theme build detail

Files (all under `gardenio/helm/gardenio/files/keycloak-theme/gardenio/`):

```text
login/theme.properties         parent=keycloak.v2, styles=css/gardenio.css, kcHtmlClass (dark/light)
login/resources/css/gardenio.css   tokens, layout, buttons, inputs, alerts, passkey affordances
login/resources/img/logo.svg       wordmark + mark, currentColor so it follows light/dark
login/messages/messages_en.properties   copy: "Sign in to gardenio", passkey wording, error text
login/*.ftl                    only where CSS cannot do it (keep the passkey hooks)
email/theme.properties, account/theme.properties   optional last step
```

Copy: short and plain. "Sign in to gardenio", "Use a passkey" (never "WebAuthn"
in user-visible text), "Use your fingerprint, face, PIN or security key". Error
text names the next action the page offers ("Your passkey was not recognized.
Try again with another passkey."). No stock Keycloak wordmark or realm name
visible anywhere.

Accessibility: contrast AA in both schemes, a visible focus ring, 44 px touch
targets, no reliance on color alone for errors, the passkey button reachable
by keyboard, `prefers-reduced-motion` honored.

CSP: the realm's `browserSecurityHeaders` already allow same-origin scripts and
styles; the theme loads only from `/auth/resources/` (which the edge serves) and
inlines nothing, so no CSP change. The step checks the rendered response headers.

## 6. Rollout, verification, rollback

| # | Change | Gate | Verify | Rollback |
|---|---|---|---|---|
| 1 | Theme in the chart; `loginTheme: gardenio` | crew PR, then laptop operator roll | `GET /auth/resources/<ver>/login/gardenio/css/gardenio.css` is 200 through the edge; the login page renders with the theme on desktop and phone; sign in with password+TOTP still works; `kd login` device page themed | `loginTheme: keycloak`, roll. Cosmetic only |
| 2 | Keycloak image tag | **owner-gated** (sign-in is down 1–2 min) | backup restores into a scratch DB; pod Ready; `ClusterKeycloak connected:true`; every Keycloak CR `status` healthy; password+TOTP sign-in; `kd login`; grafana / gardenio / api via the edge | image tag back to 26.0.7 and restore the dump (the migration is forward-only) |
| 3 | `realmHardening.passkeys` | **owner-gated** | policy attributes read back; TOTP sign-in unchanged; the account console shows "Passkey" under Signing in; attributes still present after one operator resync | `realmHardening.passkeys.enabled: false`, roll, then `kcadm update realms/gardenio -s webAuthnPolicyPasswordlessPasskeysEnabled=false` |
| 4 | Owner and Akash enroll | **owner at a device** | owner has ≥2 passkey credentials (count by type with kcadm, no ids pasted); a passkey sign-in works on each device (admins still get a TOTP prompt at this stage) | delete the credential in the account console |
| 5a | Ship the `passkey` flow dormant | crew PR | render tests; the harness e2e passes: passkey primary, password+passkey, non-admin password, admin with no passkey is refused and the re-enrolment recipe (role off, enrol, role on) works | revert the PR |
| 5b | Flip `mfa.factor: passkey` | **owner-gated** (changes the live login) | precondition check first (both admins ≥2 passkeys; ≥1 for Akash if he is to be required); owner signs in with a passkey on laptop and phone; password+passkey path works; `kd login`; edge apps; break-glass drill | `mfa.factor: totp`, roll. TOTP credentials are still there, so this is instant |
| 5c | Delete TOTP credentials, remove TOTP docs | owner-gated, after a ≥7 day soak | no TOTP credential listed; no sign-in failures in the soak window | re-enroll TOTP (needs flow `totp`) — a last resort |

Every live roll goes through the laptop operator as an escalation. CI is the
verdict for code. Nothing here runs a heavy local build.

## 7. Break-glass and recovery

The master-realm bootstrap admin is a different realm with its own browser
flow (the stock `browser`, password only), so a broken gardenio flow does not
lock it out. It is not reachable through the edge; use `kubectl port-forward`.
Its credentials live in the Secret named by `keycloak.runtime.admin.existingSecret`.

**Never print or paste a password, token or credential id** into a bead, Slack
message, PR or log. Run `kcadm` inside the Keycloak pod, which already has the
credentials in its environment (`KC_BOOTSTRAP_ADMIN_USERNAME`,
`KC_BOOTSTRAP_ADMIN_PASSWORD` come from the Secret), so nothing is read out of
the Secret by hand:

```bash
kubectl -n gardenio exec -it gardenio-keycloak-runtime-0 -- bash
# inside the pod:
K=/opt/keycloak/bin/kcadm.sh; C="--config /tmp/kcadm.config"
kc() { $K "$@" $C; }
kc config credentials --server http://localhost:8080/auth --realm master \
   --user "$KC_BOOTSTRAP_ADMIN_USERNAME" --password "$KC_BOOTSTRAP_ADMIN_PASSWORD"
# find the user: -q username= is a SUBSTRING search, so keep the exact row only
# (the runbook's uid() does this); ids go into shell variables, never to a paste
kc get users -r gardenio -q username=<email> --fields id,username --format csv --noquotes
# credential types for that user
kc get users/$U/credentials -r gardenio --fields type --format csv --noquotes
rm -f /tmp/kcadm.config   # holds a session token
```

(`-it` keeps a TTY for the password prompt. The pod shell has no `awk`, `jq`, `python3`,
`unzip` or `curl`; use `sed`, `grep`, `cut`, `tr`.) The full, CI-verified recipes live in
`gardenio/docs/keycloak-passkeys-runbook.md` (S3b); this section keeps the table and the
reasoning. Then, in the same session:

| Situation | Action |
|---|---|
| Lost one of two passkeys | Sign in with the other; account console → Signing in → remove the lost one, add a new one. No admin involved |
| Lost every passkey | For each `webauthn-passwordless` credential, `kc delete users/$U/credentials/$CID -r gardenio` (ids from `--fields id,type --format csv`, kept in shell variables); `kc set-password -r gardenio --username <email> --temporary` (**prompts once**, no confirmation, on a TTY; `KC_CLI_PASSWORD` in the environment skips the prompt; it exits 0 even if the password policy rejects the password, so read the output and check a `password` credential exists); then `kc create users/$U/logout -r gardenio -s realm=gardenio` to end live sessions. Tell the user the temporary password out of band. With the `passkey` flow an admin with a password and no passkey is **refused** ("credential setup required"), not offered enrolment, so they cannot re-enrol while they hold the admin role: take the role away first (`kc remove-roles -r gardenio --uusername <email> --rolename gardenio-admin`, and leave the admin group if the role comes from it), they sign in as a plain user and add a passkey in the account console (Signing in → Set up a Passkey), then give the role back (`kc add-roles ...`). Harness scenario 4 runs exactly this order |
| Flow itself is broken and no admin can get in | `kc update realms/gardenio -s browserFlow=browser` (stock flow, admins on password only), fix the chart, roll. **The operator owns `browserFlow`** (`KeycloakRealm`), so expect it to reset it at its next reconcile or at the latest on the next roll, which is the desired recovery; it also means the stock flow is a window to sign in during, not a place to park |
| Everything else fails | Use `kcadm` in the pod as above. The browser console through `kubectl port-forward` is **not** a reliable fallback: with `KC_HOSTNAME=https://auth.clonk.tv/auth` the master console and its OIDC endpoints are the public URL, which the edge 404s (measured, runbook section 8) |

**Security note on lost-all-passkeys.** With the `passkey` flow the window is
smaller than it was with TOTP: an admin with no passkey cannot sign in at all, so
a stolen password alone gets an attacker an error page, not an enrolment prompt.
The window moves to the recipe above: while the admin role is off, the password
alone signs the user in as a plain user and lets them add a passkey. Keep it
short: reset the password in the same sitting, have the user enrol immediately,
and give the role back as soon as the passkey count by type shows one. Remove the
user's sessions (`kc create users/$U/logout -r gardenio -s realm=gardenio`;
`delete users/<id>/sessions` does not exist) if a compromise is suspected.

The P3 docs step puts this recipe in `gardenio/docs/keycloak-passkeys-runbook.md`
and a drill (list credentials as the bootstrap admin, no changes) proves the
recipe on the upgraded server before it is needed.

## 8. Interplay (F): every client, and the answer

| Client / consumer | Flow type | Browser flow? | Effect of passkeys |
|---|---|---|---|
| `gardenio-cli` (`kd login`, device grant) | public, device code only, `standardFlowEnabled: false` | The verification page (`/realms/gardenio/device`) is a browser page on `auth.clonk.tv` that runs the realm browser flow | Works, and better: the user signs in on a device that has a passkey. Same RP ID. Tokens and refresh unaffected by the flow change. Verified in P4-S3 with a real `kd login`; the device pages are themed in P1 |
| `gardenio-traefik-forwardauth` (oauth2-proxy at the edge: grafana, dashboard, gardenio, mux, api, claimlayer) | confidential, auth-code | Yes | Works. oauth2-proxy checks the `/gardenio-admins` group, issuer and audience, never `acr`/`amr` or an OTP claim (grep of the repo found no consumer of either), so replacing the factor changes nothing it reads |
| `grafana` | confidential, auth-code | Yes; reuses the SSO cookie after the edge sign-in (`auth-cookie` ALTERNATIVE) so no second prompt | Works |
| `claimlayer-demo` | confidential, auth-code | Yes | Works; project members (non-admin) keep password sign-in |
| `gardenio-slack-link` (account link) | confidential, auth-code | Yes | Works |
| `gardenio-controller`, `-coopmux`, `-authz-canary`, `-beads-viewer`, `-release-deployer`, `-grafana-bridge`, `-trigger-engine`, `-slack-bridge` | service-account (client credentials), `standardFlowEnabled: false`, `directAccess: false` | **No** | Unaffected. No browser, no flow |
| Beads daemon (kbeads) | verifies Keycloak JWTs (issuer, `aud=gardenio-traefik-forwardauth`) | No | Unaffected; tokens are unchanged |
| Ziti-to-Keycloak edge plan (gd-P78ygCUgA9) | edge nginx + oauth2-proxy | Yes | It needs the browser flow to keep gating `/gardenio-admins` with "MFA". Passkey with user verification satisfies that, and an admin cannot reach a session without one. The edge probes (`scripts/edge-probe`) test redirects, not factors. Docs that say "Keycloak + TOTP" (`gardenio/helm/gardenio-ziti/README.md`) are updated in P4-S4 |
| Edge nginx paths | — | — | The account console (`/auth/realms/gardenio/account`, REST under the same prefix) and theme assets (`/auth/resources/`) are already inside the two allowed prefixes; the `public_edge_bad_path` filter rejects `..`, `%2e`, `%2f`, `%5c` and `;` in the path, none of which Keycloak's login or WebAuthn URLs use. P1/P3 verify through the edge, not in-cluster |
| Other realms / browser apps | — | — | None found; `realms/master` is not exposed |

Conclusion: nothing breaks. The only consumers that look at the factor are the
chart tests (`keycloak_mfa_test.go`) and docs, which the build steps update.

## 9. Open items closed

- **gd-KVIQQ3H1Nr** (the operator drops `CONFIGURE_TOTP`): superseded. The flow
  only requires TOTP of `gardenio-admin` (which Akash does not hold), and this
  plan removes TOTP. P4-S4 closes it with a pointer here. Meanwhile no step
  relies on a user-level required action.
- **Who is required to have a passkey.** Default: the `gardenio-admin` role,
  exactly today's TOTP scope. Project members (Akash, the `project-*` groups
  the edge admits) are invited to enroll but not forced; widening it to every
  user is one `condUserRole` change. Not a decision bead because the default
  preserves today's policy.
- **Akash's onboarding (gd-llFKMS8Pkc)** stays password-first. A passkey is
  offered as part of it. P3-S5 is the owner's escalation to have him enroll.

## 10. Phases and steps

Four phases, each ending in a verified, reversible state. A step bead carries
its whole task text; a crew reads only that.

**Phase 1 (gd-LtsPHf73WT) — Login theme (no auth change)**
- S1a (gd-S6sixAadTe) Theme plumbing in the chart: ConfigMap, mount, `loginTheme`, render tests
- S1b (gd-aoKg7nuXkw) CI preview harness: Keycloak container, Playwright screenshots, virtual authenticator
- S1c (gd-kfwXlKKZfr) Theme: tokens, login/password/error/logout screens
- S1d (gd-ijqlnnsSJ9) Theme: passkey, update-password, device (`kd login`) and grant pages
- S1e (gd-S3nhltQz1c) Optional email and account themes
- S1f (gd-ktJdfGG7tI) OWNER-GATED roll of the theme and owner visual sign-off

**Phase 2 (gd-Km2WKxR0Ac) — Keycloak upgrade**
- S2a (gd-s47ivaszhW) Preflight: target version, operator compatibility, release-notes audit
- S2b (gd-R44gpWur5R) Chart: image tag bump, mirror, tests
- S2c (gd-BaTFh9QH5R) OWNER-GATED roll with Postgres backup and restore proof

**Phase 3 (gd-rZZhfrpMDv) — Optional passkeys; enrollment**
- S3a (gd-h9M9eTTun8) Chart: `realmHardening.passkeys` + resync-survival test
- S3b (gd-diK82eIdBJ) Runbook: break-glass and recovery; drill
- S3c (gd-UYeWTx8g9w) OWNER-GATED roll of the passkey policy
- S3d (gd-9wXX0iK14f) OWNER-AT-DEVICE: the owner enrolls two passkeys
- S3e (gd-6UHSnwxowp) OWNER-AT-DEVICE: Akash enrolls a passkey

**Phase 4 (gd-pGRjC2k1ls) — Passkey required, TOTP retired**
- S4a (gd-SR4JfUslzG) Chart: the `passkey` factor branch, dormant, with harness e2e
- S4b (gd-wO9MDjpz13) OWNER-GATED flip to `factor: passkey`, with verification and drill
- S4c (gd-GiZfavru5G) Soak, delete TOTP credentials, update docs, close gd-KVIQQ3H1Nr

Dependencies: S1b←S1a; S1c←S1a,S1b; S1d,S1e←S1c; S1f←S1a,S1c,S1d; S2b←S2a; S2c←S2b; S3a,S3b←S2c; S3c←S3a; S3d←S3c,S3b; S3e←S3c; S4a←S1b,S2c; S4b←S4a,S3b,S3c,S3d; S4c←S4b. Phases 1 and 2 are independent and can run in parallel.

## 11. Risks

| Risk | Mitigation |
|---|---|
| Operator 1.32.0 misbehaves against a newer Keycloak | S2a checks; S2c verifies every CR status; image rollback plus restore |
| Theme template override breaks conditional UI | Harness runs conditional UI with the theme on; prefer CSS; keep the parent's passkey hooks |
| kcadm-set policy is reverted by an operator resync | S3a measures; CronJob fallback |
| Passkeys unusable on a device (old U2F key, an old browser) | Password+passkey path needs a passkey too, so admins need at least one capable device; two passkeys per admin; break-glass |
| RP ID change invalidates all passkeys | Pinned explicitly; chart test ties it to the issuer host; documented |
| Locking the owner out at the flip | Hard precondition in S4b; instant rollback via `factor: totp`; TOTP credentials kept until the soak ends |
| A password-only window after the last passkey is removed | Section 7 procedure: reset the password in the same sitting |

## 12. Upgrade preflight (2026-10-18)

Task gd-s47ivaszhW (P2-S1). Research plus measurement, no live step. Everything
below was read from the Keycloak upgrading-guide sources
(`docs/documentation/upgrading/topics/changes/changes-26_*.adoc`, every file from
26.0.6 to 26.8.1), the EPAM operator's releases, issues and source at v1.32.0 and
v1.35.0, and then run in CI against real containers.

### 12.1 Decision

| | |
|---|---|
| **Target** | `quay.io/keycloak/keycloak:26.7.5` (`keycloak.runtime.image.tag: "26.7.5"`) |
| **Rollback tag** | `26.0.7`, but only together with the `pg_dump` restore (12.6) |
| **Operator** | stay on EPAM `keycloak-operator` **1.32.0**; no operator upgrade in this plan |
| **Rejected: 26.8.1** | Newest line, one patch old. Every 26.8 item is read and written down in 12.3, none breaks us, and the harness passes on it. It is rejected anyway for three reasons that are about risk, not breakage: it carries the login-theme IdP-button redesign and the login-failures storage move at the same moment we are changing the theme and the passkey policy; its non-Organization operator-suite failures (12.5) were more frequent, 2 of 4 cells against 1 of 4 on 26.7.5 and 0 of 4 on 26.5.7; and 26.7 is the more settled line (five patches). Moving 26.7.5 to 26.8.x is a separate small upgrade after sign-in works, as section 3.B already says. |
| **Rejected: 26.5.7** | The newest line the operator's own CI covers (it tests 26.4 and 26.5). It is the safe-by-construction choice and is the fallback if 26.7.5 shows a live problem. It is not the target because 26.6 and 26.7 carry the WebAuthn changes this plan builds on (`Discoverable Credential` as `residentKey`, vendor icons for registered passkeys) and the security patch stream; the measurements in 12.5 show the operator is fine on 26.7.5. |
| **Rejected: 26.6.x** | 26.6.0 itself is skipped. Its migration adds an `Organization` sub-flow to custom browser flows bound as the browser flow; 26.6.1 fixed that for anyone upgrading directly from 26.5 or earlier. Going 26.0.7 to 26.7.5 in one step never runs the 26.6.0 behaviour (the harness confirms our custom flow is unchanged). |

Versions 26.5.3, 26.5.6 and 26.6.x exist on quay but have no upgrading-guide entry
beyond what is read here; they are not candidates.

### 12.2 What was read

All 32 `changes-26_*` files from 26.0.6 to 26.8.1: breaking, notable, deprecated
and removed items. The items that touch **this deployment** are in 12.3. The rest
(LDAP, Kerberos, SAML, X.509, Authorization Services, token exchange, FGAP,
SPIs, JDBC drivers for other databases, MySQL/MariaDB/Oracle/SQL Server, clustering,
`stateless`, operator CRDs of Keycloak's own operator, OpenTelemetry, Infinispan,
truststore) is not used here: the chart sets none of those and renders none of
those CRs.

### 12.3 Items that touch this deployment

| Version | Item | Our exposure | Result |
|---|---|---|---|
| 26.2.0 | `X-Forwarded-Host` without a port no longer falls back to the request port | `KC_HOSTNAME` is a full URL, so the browser-facing host is fixed; `hostname-backchannel-dynamic` uses the request `Host` for in-cluster URLs | **Measured**: discovery documents are byte-identical to 26.0.7 in all six cases (proxy headers, in-cluster `Host`, XFH without port) |
| 26.2.0, 26.3.0 | `user-profile-commons.ftl` changed (a `locale` field may appear) | Only if the theme overrides it | S1c/S1d: do not override it; prefer CSS |
| 26.3.0 | Credentials of one user must have **distinct names** | WebAuthn registration | S3d/S3e runbooks: name each passkey differently ("MacBook", "iPhone") or the second one is refused |
| 26.3.0 | Default browser flow gets WebAuthn and recovery codes as 2FA alternatives | New realms only; ours is a custom flow | **Measured**: the custom flow's 12 executions are unchanged after the migration |
| 26.3.0, 26.4.0 | Passkeys Conditional UI Authenticator deprecated, then gated behind a feature | Design already avoids it | `webauthn-authenticator`, `webauthn-authenticator-passwordless` and `conditional-credential` are all present on 26.7.5 |
| 26.4.0 | Passkeys supported; PostgreSQL driver gets `targetServerType=primary` | One Postgres, no replica | None |
| 26.4.0 | Translation bundle file names follow Java `ResourceBundle` | Only custom translations | S1c: `messages_en.properties` is standard; nothing to do |
| 26.4.3, 26.5.2 | Non-normalized paths (`//`, `..`) and `;` in paths answer HTTP 400 | Edge forwards only `^~ /auth/realms/gardenio/` and `/auth/resources/`; all issuer URLs in `deploy-values` have no trailing slash; `kd` and `gb` trim it | **Measured** 400 for `//` and for `;`; no caller of ours produces either |
| 26.5.0 | Client Session Idle/Max may not exceed the realm SSO values; checked when a client or realm is created, updated or imported | `gardenio-cli` sets 28800/604800, equal to `ssoSessionIdleTimeout`/`ssoSessionMaxLifespan` | **Measured**: equal is accepted, a larger idle is rejected. The operator re-PUTs the client every sync, so a future client value above the realm's makes that client's reconcile fail. S2b adds a chart test pinning client `client.session.*` <= `keycloak.realm.sessions` |
| 26.5.0 | PostgreSQL 13 unsupported; DB user needs `SELECT` on `pg_class`/`pg_namespace` | Chart Postgres is `pgvector/pgvector:pg17`; the role is the database owner | None |
| 26.5.0 | Login-theme message keys no longer carry HTML or entities | Only custom messages | S1c: plain UTF-8 text in any message override |
| 26.5.0 | Expired sessions are deleted in small batches by every node; `OFFLINE_USER_SESSION.REMEMBER_ME` column and indexes | `slack-bridge` holds offline tokens, so offline sessions exist; the table is small | Index creation is skipped above 300,000 rows (and logged); the harness migration took 25 to 41 s. Expect the same order live |
| 26.6.0 | Management and HTTP ports open **during** initialisation when health is enabled | The StatefulSet probes use `:9000/auth/health/{started,live,ready}`; the Service routes by the readiness probe | **Measured**: all three return 200 on 26.7.5; the main port still answers 404 for `/auth/health/ready`, as on 26.0.7. A long migration is now no longer aborted by the startup probe (60 x 10 s budget stays) |
| 26.6.0 | New "Secondary Authentication Failures Lockout" | Off unless `Maximum Secondary Authentication Failures` is non-zero; default `0`; counts OTP only | None. `realmHardening.bruteForce` is unaffected (**measured**: the six `kcadm update realms/gardenio` fields read back identical) |
| 26.6.0 | `secure-client-uris` executor requires https for logo, policy and post-logout URIs | We configure no client policies; the grafana client's `http://gardenio.grafana.ziti:3000/*` post-logout URI is not validated | None |
| 26.6.2 | WebAuthn `Acceptable AAGUIDs` strictly requires signed attestation; required actions are one-time by default | Design sets attestation `none`, no AAGUID list | None |
| 26.6.3 | Redirect URI wildcards do not apply to the hostname | The four clients that set `redirectUris` in `deploy-values` use exact hosts (`https://gardenio.clonk.tv/oauth2/callback` and so on); none has a wildcard in the host | None |
| 26.7.0 | WebAuthn `Require Discoverable Credential` deprecated for `Discoverable Credential` (`residentKey`) | S3a sets this attribute | **Measured**: `kcadm update realms/gardenio -s webAuthnPolicyPasswordlessResidentKey=required` is accepted; S3a must use it, not `...RequireResidentKey`. The other attribute names in 4.2 are confirmed (12.4) |
| 26.7.0 | Required actions Configure OTP and Update password move after Verify Email; "Verify Email" before credential setup for **self-registration** | `verifyEmail: true`, but self-registration is off | None |
| 26.7.0 | Login-theme layout: horizontal action buttons, vendor icons for registered passkeys | Theme work | S1c/S1d re-screenshot on 26.7.5, not 26.0.7 |
| 26.7.0 | Async commit for ephemeral tables on PostgreSQL; AES keys default 256-bit; session cookies SHA-384 | Transparent | Sessions stay valid; a stale `KEYCLOAK_SESSION` cookie is replaced silently |
| 26.7.0 | `view-system` role removed | Not used | None |
| 26.7.2 | Legacy client-initiated account linking disabled | Not used | None |
| 26.7.3 | `redirect_uri` and `post_logout_redirect_uri` containing `state`, `code`, `session_state` are rejected | Our post-logout URIs are plain callbacks | None |

### 12.4 26.8 items, cleared (kept so a later 26.8 upgrade starts from here)

| Item | Exposure | Verdict |
|---|---|---|
| Client secrets hidden from `view-clients` callers | The operator authenticates as the master bootstrap admin (`ClusterKeycloak` secret), which holds every admin role (`ClusterKeycloak` uses the same `gardenio-keycloak-runtime-admin` Secret as the bootstrap admin) | Clear |
| IdP buttons redesigned in the login theme (`social-providers.ftl`) | One IdP (`gitlab`); we do not override that template | Re-check in the S1c screenshots |
| Login failures stored in the database (`login-failures:v2`) | Brute force is on (10 failures); slightly more DB load | Clear; `--features=login-failures:v1` restores the old behaviour |
| Redirect URI and web origin host compared case-insensitively | Only relaxes matching | Clear |
| Organizations: many-to-many IdP links, new tables, alias validation | We render no `KeycloakOrganization` (grep of `templates/keycloak/` is empty) | Clear (but see 12.5) |
| Email verified only by real verification flows | `first-admin-job.yaml` sets `emailVerified=true` directly through the admin API | Clear |
| `kcadm` config file forced to 0600 | The hook writes under its own `/tmp` | Clear |
| Disabled clients leave the token audience; IdP mappers cannot grant admin roles | None used | Clear |
| "Full scope allowed" deprecated | Chart clients default it to `true` | Warning only; a later hardening task |
| Offline-session indexes rebuilt (skipped above 300,000 rows) | Small table | Clear |

### 12.5 The operator

The operator publishes no compatibility matrix. Facts:

- Live: `docker.io/epamedp/keycloak-operator:1.32.0` in namespace `operators` (not `gardenio`), 1/1 Ready, **36 restarts**, last exit 255 on 2026-10-16, quiet for the 2 days since (infra read-only check, bead gd-1Vt1pEysdj). Not caused by this plan; S2c should record the restart count before and after the roll.
- 1.32.0 talks to Keycloak through `gocloak v12` and `zmotso/keycloak-go-client`. The "generated client targeting Keycloak 26.5.2" in the earlier notes is 1.33.0 and later (`pkg/client/keycloakapi`, `KEYCLOAK_VERSION ?= 26.5.2`), not what runs here.
- Its own PR CI (`pr.yaml` at v1.32.0) runs the integration suite against Keycloak **26.4 and 26.5 only**.
- Chart check: no `KeycloakOrganization` in `gardenio/helm/gardenio/templates/keycloak/`. The CRs the chart renders are `KeycloakRealm`, `KeycloakRealmGroup`, `KeycloakRealmRole`, `KeycloakRealmUser`, `KeycloakClient`, `KeycloakClientScope`, `KeycloakAuthFlow` and `ClusterKeycloak`.

So the operator's guarantee stops at 26.5. To replace the guess with a measurement,
`.github/workflows/keycloak-operator-compat.yml` runs the operator's **own**
integration suite (`go test ./...` with envtest and a real Keycloak at
`TEST_KEYCLOAK_URL`, the same recipe as its Makefile) from an operator tag against
a Keycloak tag. Six cells, two samples each:

| Operator \ Keycloak | 26.5.7 (control) | 26.7.5 | 26.8.1 |
|---|---|---|---|
| 1.32.0 (live) | 30 ok, 0 fail / 30 ok, 0 fail | 27 ok, 3 fail / 29 ok, 1 fail | 29 ok, 1 fail / 27 ok, 3 fail |
| 1.35.0 (newest) | 35 ok, 0 fail / 35 ok, 0 fail | 34 ok, 1 fail / 34 ok, 1 fail | 34 ok, 1 fail / 33 ok, 2 fail |

(package counts, first sample then second sample; GitHub Actions runs 39149015303 and 39150022698.)

- **`TestKeycloakOrganization` fails on 26.7.5 and 26.8.1 for both operators in every sample.** The failing step is the duplicate-domain check: Keycloak 26.7 changed how organization domains are enforced. It is exactly the `KeycloakOrganization` incompatibility EPAM issue #420 reports, and we render none. It matters only if someone later adds organizations (then it needs an operator that handles the new domain model).
- Every other failure was different between the two samples and landed on a different package each time: a Kubernetes `Operation cannot be fulfilled on secrets "kc-token-keycloak-auth-secret": the object has been modified` conflict inside the test fixture, and `BeforeSuite` 10-second timeouts waiting for the test realm. On the cells that matter, **1.32.0 against 26.7.5, the second sample had the Organization failure alone**: every `KeycloakRealm`, `KeycloakClient`, `KeycloakClientScope`, `KeycloakAuthFlow`, `KeycloakRealmGroup`, `KeycloakRealmRole`, `KeycloakRealmUser` and `ClusterKeycloak` package passed. The control 26.5.7 had no failure in four samples; 26.8.1 had non-Organization failures in two of four. Read that as "the test fixtures are timing-sensitive on a slow runner, a little more on newer Keycloak", not as a product break.

**Verdict: operator 1.32.0 is enough for 26.7.5.** Upgrading it is not required, and
it is not free: 1.33.0 swaps the REST client wholesale (gocloak to a generated
client), and 1.31.0 already made webhooks depend on cert-manager. 1.35.0 would add
realm brute-force support (a chance to retire the `kcadm` brute-force call) and a
same-named-subgroup fix; neither is needed here. If it is wanted later it is a
separate operator-side escalation (the operator is installed from the `epamedp`
chart outside the gardenio chart) and should be proven with the same workflow
first.

The live CR status baseline could not be read (the read-only infra role is
Forbidden on `v1.edp.epam.com`). S2c's laptop step records
`kubectl get clusterkeycloaks,keycloakrealms,keycloakclients,keycloakauthflows,keycloakclientscopes,keycloakrealmgroups,keycloakrealmroles,keycloakrealmusers -A`
status columns before the roll and after it.

### 12.6 The harness run

`.github/workflows/keycloak-upgrade-preflight.yml` with `scripts/keycloak-preflight/run.sh`
starts `keycloak:26.0.7` with the StatefulSet's exact environment
(`KC_HOSTNAME=https://auth.clonk.tv/auth`, `KC_HOSTNAME_BACKCHANNEL_DYNAMIC`,
`KC_HTTP_RELATIVE_PATH=/auth`, `KC_PROXY_HEADERS=xforwarded`, health and metrics on,
`KC_BOOTSTRAP_ADMIN_*`, Postgres 17), seeds a `gardenio` realm shaped like ours (custom
browser flow with 12 executions, the browser client with exact redirect, the
device-grant client, a user, a group, a role), stops it, starts the target on the
**same database**, and checks the results below. It is the "scratch CI job" this
step allows while the S1b theme harness (`keycloak-theme.yml`) is unmerged; S2b should
keep it and point it at the chosen tag. It runs only by dispatch or on
`agent/kc-preflight/**` pushes and is never a required check.

Result for `26.0.7 -> 26.7.5` (GitHub Actions runs 39148250620, 39149015327 and 39150022633, all green;
`26.8.1` was green in all three): **0 hard failures, 18 PASS**.

| Check | 26.7.5 |
|---|---|
| Schema migration of the seeded 26.0.7 database | ready in 25 to 41 s, no ERROR lines; two WARNs: deprecated features `identity-brokering-api:v1`, `twitter-broker:v1` enabled by default (we use neither) |
| Probes `:9000/auth/health/{started,live,ready}`, `:9000/auth/metrics` (ServiceMonitor path) | 200 / 200 / 200 / 200 |
| Issuer and endpoint hosts | issuer `https://auth.clonk.tv/auth/realms/gardenio`; in-cluster `Host` gives `http://gardenio-keycloak-runtime.gardenio.svc.cluster.local:8080/...` token and JWKS URIs, as before |
| The 11 `first-admin-job.yaml` kcadm calls (login, `get realms/..`, brute-force `update`, `get groups -q search=` as CSV, `get users -q username=`, `create users`, `set-password`, credentials list, `update users/..`, `update users/../groups/..-n`, membership readback) | outputs identical to 26.0.7 (`diff` empty), including the CSV shapes the hook parses with `cut`/`grep` |
| Custom browser flow `gardenio-mfa-browser` and its realm binding | unchanged; no Organization sub-flow |
| Client with `client.session.idle.timeout=28800` / `max.lifespan=604800` | re-accepted; idle above the realm's is rejected (26.5 validation) |
| Device authorization endpoint (`kd login`) | returns `user_code`; `verification_uri` host `https://auth.clonk.tv` |
| Login page via proxy headers | 200, `username` field present, `/auth/resources/<hash>/...` assets |
| Passkey groundwork | `webauthn-authenticator`, `webauthn-authenticator-passwordless`, `conditional-credential` present on a stock realm, with no feature flag |
| S3a WebAuthn Passwordless attributes via `kcadm update realms/gardenio` | accepted; read back as `webAuthnPolicyPasswordlessRpId=auth.clonk.tv`, `...RpEntityName=gardenio`, `...UserVerificationRequirement=required`, `...AttestationConveyancePreference=none`, `...SignatureAlgorithms=[ES256,RS256]`, `...AvoidSameAuthenticatorRegister=true`, `...ResidentKey=required`, `...AuthenticatorAttachment="not specified"`. The deprecated `...RequireResidentKey` stays `"not specified"`, so S3a must set `...ResidentKey` and the chart test must assert it |
| Path hygiene | `//` and `;` answer 400 (26.4.3, 26.5.2) |

**What it does not cover**, and who does: the real passkey ceremony and the theme
(S1b); the operator against the live cluster's CRs and secrets (S2c, with the status
baseline above); a database of the live size (the `pg_dump` and scratch restore in S2c).

**Rollback.** In the harness the old 26.0.7 image does start on a 26.7.5-migrated
database (it came up in 23 to 29 s). That is a property of a small synthetic
database, not a supported path: Keycloak's schema migration is forward-only and
rewrites realm data, so S2c keeps the restore as the rollback and does not rely on
re-pinning `26.0.7` alone.

### 12.7 Image availability (input to S2b)

- The runtime image is `gardenio.mirroredImage` of `keycloak.runtime.image`. The chart default already sets `registry: quay.io`, `repository: keycloak/keycloak`, and `deploy-values/gardenio-k3s.yaml` does not override it. `global.mirrorImageRegistry` (`docker.io`) is therefore not used for it, and `global.imageRegistry` (ECR) applies only to gardenio-owned images. The old `.rwx/mirror.yml` is gone and no longer matters for this image.
- The StatefulSet's `imagePullSecrets` are `ecr` and `ghcr`; neither is a quay credential, so pulls are anonymous. A first-admin hook pod pulled `quay.io/keycloak/keycloak:26.0.7` from quay.io in 9 s (235 MB) 16 hours before this check, so the k3s nodes reach quay directly. A node that has not seen the new tag will pull it fresh. The harness runners pulled `26.7.5` and `26.8.1` from quay.io the same way.
- **S2b changes:** the tag in `values.yaml` (`keycloak.runtime.image.tag`) and its comment (which still points at the deleted `.rwx/mirror.yml`); a chart test pinning client session timeouts <= realm session timeouts (12.3); and, since the node has to pull ~235 MB at roll time, optionally a digest pin (`digest:`) so the roll cannot silently change. Whether containerd has a registry mirror for quay.io could not be read without node exec; the successful direct pull says none is in the way.
- Note for S2c: the StatefulSet has no PVC (volumes `data` and `tmp` are emptyDir), so the Keycloak side of the roll is stateless and the backup that matters is the `keycloak` database in `gardenio-postgres`.

### 12.8 What changes in the other steps

- S2b: tag `26.7.5`; fix the stale mirror comment; the client-session chart test; keep or re-point the upgrade-preflight workflow.
- S2c: restore is the rollback; record operator restart count and CR status before and after; expect 25 to 60 s of failed sign-ins on the single replica.
- S1c, S1d: screenshot against 26.7.5; keep `user-profile-commons.ftl` and `social-providers.ftl` un-overridden; message overrides are plain text.
- S3a: use `webAuthnPolicyPasswordlessResidentKey`, not the deprecated attribute; the attribute names in 4.2 are confirmed by 12.6.
- S3d, S3e: passkeys on one account need distinct names.

## 13. S1g: a richer pixel-art nature login (design for the owner's pick)

Step S1g (gd-nWXkV6XRc5). Owner request, 2026-10-18: "a richer pixelated login screen with
nature and colors (can ship after current enhancements but design now)". The flat theme
(section 3.C, shipped by S1c/S1d) stays as it is; this section designs the follow-up look,
and **nothing here changes the live theme**. Three candidates were built as throwaway
variant themes and rendered through the real Keycloak harness (`gardenio/tests/keycloak-theme`):
every screen the harness reaches (26 per variant, listed in 13.6), desktop 1280 and mobile 360,
light and dark. The owner picks one (decision bead in 13.7); the build step is separate.

### 13.1 The three directions

| | A. Dawn Meadow | B. Parallax Forest | C. Hills Through the Day |
|---|---|---|---|
| Light | Sunrise over a garden: peach and rose horizon, a sun behind blue hills, picket fences, sunflowers, tulips, lavender, a scarecrow. The plants grow in when the page opens. | A misty morning wood: green-gold sky, shafts of sun, five layers of trees from pale blue ridge to two big trunks that frame the card, ferns, toadstools, a fallen log. | Patchwork farm hills under snow-capped peaks, a windmill and a cottage, a winding path, oaks and a fence. The sky and the light on the land drift from dawn to noon to golden hour. |
| Dark | The same garden under a moon and stars, hills deepened to blue, fireflies drifting over it. | The same wood at night: moon, stars, teal-black trees, drifting fireflies, glowing mushrooms. | The same land from dusk through midnight to first light: the sky bands, stars, moon and the colour of the fields move together. |
| Mood | Warm, friendly, "a garden"; closest to the product name. | Deep and calm; the most layered; most "game". | Wide and scenic; the only one that changes while you look. |
| Card, button, sign | cream card, **green** button, wooden sign (dark: navy card, **yellow** button) | pale-green card, **green** button, bark sign (dark: black-teal card, **mint** button) | ivory card, **indigo** button, sun-orange sign (dark: navy card, **amber** button) |
| Motion (stepped one cell at a time unless noted) | clouds drift, grass sways one cell, butterflies cross; the bed grows in once (2.4 s); dark: star and firefly twinkle | two mist layers drift opposite ways, beams pulse, motes/fireflies drift and twinkle | clouds and birds drift, the sun/moon arc and the palette ease smoothly (not stepped) over a 150 s timeline that reverses at its ends, fireflies at night |
| Assets added | 157 KB (loads 87 KB light / 88 KB dark) | 265 KB (loads 146 KB / 157 KB) | 193 KB (loads 178 KB / 179 KB) |
| Risk | sun hides behind the card on desktop at some widths (it is a scenery detail, not content) | most pixels to draw; biggest asset set (still under 300 KB) | the CSS timeline is the most complex part (registered `@property` colours and `filter`); a browser without `@property` shows the static noon / midnight frame |

Screenshots: the artifact gallery and the images attached to decision gd-TCaihXOgWd (13.7);
CI artifacts `keycloak-theme-screenshots-{meadow,forest,hills}` on the PR hold all 26 screens
per variant and, for C, `scene-frame-1..5` (the timeline paused at 0, 25, 50, 75 and 100 %).

**Recommendation (not a requirement): A.** It carries the "gardenio" name best, it is the
smallest and simplest to maintain, and it keeps the most calm area around the card. B is the
richest picture; C is the most surprising but the most to keep working across browsers.

### 13.2 How every direction meets the constraints

| Constraint | How, and the check |
|---|---|
| Card stays solid and readable | The card, sign and footer line are opaque plates with a one-cell frame. Measured WCAG contrast (token pairs, AA is 4.5 text / 3 fields): text on card 13.7 to 15.1; muted text 6.9 to 8.9; link 6.2 to 11.2; button label on button 6.5 to 12.7; field border on card 4.4 to 6.0 (min of the six themes); sign text on sign 7.7 to 11.6. `axe` (the hard gate, serious and critical) is green on every screen of every variant. |
| System font for text, pixel font for headings only | Pixelify Sans 600, latin subset, 8.2 KB woff2, self-hosted in `gardenio-scene/login/resources/fonts/` with `OFL-PixelifySans.txt` (SIL OFL 1.1, copyright 2021 The Pixelify Sans Project Authors, upstream https://github.com/eifetx/Pixelify-Sans, taken from the `@fontsource/pixelify-sans` npm package, which ships the same licence). Used for `#kc-page-title` and the wordmark sign only. `scene.spec.js` asserts the title is Pixelify and the username field and labels are not. |
| Original art | All art is generated by `variants/gen/*.js` from seeded noise and sprite maps written for this repo (no sprite pack, no traced or downloaded image, no third-party palette). Deterministic: `node gen/index.js` rewrites identical files. The only third-party file is the font above. **Nothing is taken from play.clonk.tv or the Matt Clanker hub**, and no palette, motif or sprite from there is reused or referred to (the scenes are a garden, a forest and farm hills; the sign is a plain wooden plate). |
| Same-origin, no CSP change, no CDN, no `data:`, no inline script | Pictures are `url("../img/x.svg")` in the theme's own CSS and the font is `url("../fonts/x.woff2")`: same origin as the page. `template.ftl` is **not touched**, so no script or inline style is added. `scene.spec.js` records every request on the login page and fails on any request outside the Keycloak origin or any `data:` URL. |
| Under 300 KB added | Per-variant totals in 13.1, including the shared base (font, base CSS, sprout icon = 14.6 KB). A visitor loads only the colour scheme they use (the table's "loads" figures); `scene.spec.js` fails if one scheme's scene assets (images and font, measured as served) reach 300 KB. SVGs also compress about 4:1 on the wire if Keycloak or the ingress gzips them. |
| Motion optional, gentle, off for reduced motion, no JS | Only CSS `animation` on six background layers, stepped one grid cell at a time (a slow retro drift, not a smooth glide; no drifting layer moves faster than 1.5 cells per second). The one exception is C's time-of-day timeline, which eases smoothly and slowly. `prefers-reduced-motion: reduce` sets `animation: none` on all six layers and on the root element; `scene.spec.js` asserts at least one layer animates normally and none does under reduced motion. |
| Keycloak kc* markup and passkey hooks untouched | Each variant is a child theme of the chart's `gardenio` (`parent=` chain `gardenio-<name>` -> `gardenio-scene` -> `gardenio` -> `keycloak.v2`), so every `.ftl` is the production file. The harness's passkey tests (conditional UI, the `username webauthn` autofill token, the "Use a passkey" button) ran and passed on each variant. |
| Light and dark | A day and a night version of every scene, selected by `prefers-color-scheme`. |
| Device-code, grant, error, logout, expired | They share `template.ftl`, so they inherit the scene. All of them are in the 26 screens per variant (13.6), including `device-code`, `device-code-invalid`, `device-grant`, `device-success`, `oauth-grant`, `error`, `logout-confirm`, `logout-done`, `login-page-expired`, `login-update-password`, `webauthn-*`, `login-config-totp`, `login-verify-email` and the email-driven info pages. |

### 13.3 How a variant layers into the theme

No template change, no markup. The scene is six pseudo-elements that are fixed, full-viewport
and `pointer-events: none`, behind the page, each carrying one or more SVG backgrounds:

| Layer | Element | z | Carries |
|---|---|---|---|
| sky colour | `html` background | | hard-banded `linear-gradient` (a limited palette, not a smooth fade) |
| 1 | `html::before` | -6 | sun, or moon + stars; beams (B) |
| 2 | `html::after` | -5 | clouds (A, C); far ridge + back trees + mist (B) |
| 3 | `body::before` | -4 | hills (A); mountains (C); mid trees (B) |
| 4 | `body::after` | -3 | the garden bed (A, grows in); the farm (C); low mist (B) |
| 5 | `.pf-v5-c-login::before` | -1 | foreground grass (A); the near hill (C); the big trunks and the floor (B) |
| 6 | `.pf-v5-c-login::after` | -1 | butterflies/fireflies (A); light motes/fireflies (B); birds/fireflies (C) |

Every picture is an SVG drawn on an integer grid (rect runs, `shape-rendering="crispEdges"`)
and shown at a whole multiple of its grid: one cell is `--gd-px` CSS pixels, 4 on desktop and
3 at 640 px or narrower, so edges stay sharp at any zoom. Tiles are 320 cells wide and repeat
sideways without a seam. The files per theme:

- `gardenio-scene/` (shared): `css/scene-base.css` (layers, pixel card frame, wordmark sign,
  square controls, heading font, footer plate), `fonts/`, `img/sprout.svg` (the mask behind the wordmark).
- `gardenio-<name>/`: `theme.properties` (`parent=gardenio-scene`, `styles=` the three
  stylesheets in order), `css/scene-<name>.css` (colour tokens for light and dark, the layer
  stack, the keyframes), `img/*.svg`.

The existing colour tokens (`--gd-surface`, `--gd-accent`, ...) are reused, so alerts, inputs
and the passkey hint keep working; each scene overrides them. A scene changes the card
(solid, square, framed), the wordmark (a pixel-font sign instead of the mask logo; the text
stays the accessible name the harness checks) and the buttons (square, one-cell lower edge);
body text, labels and inputs are unchanged.

**What the build step does after the owner picks one** (not part of S1g): copy
`gardenio-scene` and the chosen variant into the chart's theme tree
(`files/keycloak-theme/gardenio/login/resources/{css,img,fonts}`), put the three stylesheets
in `theme.properties` `styles=`, keep the production `template.ftl`, and delete the other
variants, `gen/` stays so the art can be redrawn. Two chart facts matter:

1. **The font is binary.** `templates/keycloak-runtime/theme-configmap.yaml` writes every file
   as ConfigMap `data` with `toString | toJson`, "text files only". A woff2 would corrupt, so the
   build step adds a `binaryData` branch (`b64enc`) for `*.woff2`, and the chart test that maps
   file keys to mount paths must learn it. (Dropping the pixel font is the fallback: headings
   would use the system font and nothing else changes.)
2. **Size.** The chart caps the theme at 900 KiB total; the largest candidate adds 265 KB to
   the current 27 KB, well inside it. A ConfigMap change rolls the Keycloak pod (checksum
   annotation), the same as any theme edit.

### 13.4 Browser support and failure modes

SVG backgrounds, `image-rendering: pixelated` and CSS animation work everywhere the login
page does. `mask` (already used by the logo) and `:has()` (already used by gardenio.css) are
in all current browsers. C additionally needs `@property` (Chrome 85+, Safari 16.4+, Firefox 128+);
without it the sky bands show the static noon (light) or midnight (dark) colours and nothing
breaks. If an image fails to load, the page falls back to the solid sky band colour behind
it and the card is unaffected.

### 13.5 Performance

Six fixed layers repaint only when their stepped animation advances (about 1 to 2.5 Hz, not
every frame); there is no `filter` on the page except in C, where it applies to four layers
and animates on a 150 s timeline. Reduced motion removes all of it.

### 13.6 Evidence

The harness job (`.github/workflows/keycloak-theme.yml`) now has a matrix row per variant
(`chart`, `meadow`, `forest`, `hills`; the `chart` row keeps its old name). Each variant row
runs the full suite against a real Keycloak 26.7.5: 26 screens per variant, in each of the four projects (desktop and mobile, light and dark),
the `axe` hard gate (serious and critical), the passkey tests, and `tests/scene.spec.js`.
Screens: `login`, `login-error`, `forgot-password`, `forgot-password-sent`,
`login-page-expired`, `login-update-password`, `login-config-totp`, `login-verify-email`,
`logout-confirm`, `logout-done`, `error`, `info`, `info-confirm`, `oauth-grant`,
`device-code`, `device-code-invalid`, `device-grant`, `device-success`,
`webauthn-authenticate`, `webauthn-error`, `webauthn-register`, `passkey-register`,
`passkey-signin-button`, `email-*` and `account-signingin` (the account console, which these
login themes do not restyle).

How to reproduce one: `cd gardenio/tests/keycloak-theme && THEME_VARIANT=meadow ./run.sh`
(needs Docker), or open the PR's CI run. How to redraw the art: `node variants/gen/index.js`.

### 13.7 Owner pick

Decision bead **gd-TCaihXOgWd** (options A, B, C) carries the screenshots: the gallery
https://claude.ai/artifact/MuUSNKoFqSQnS3rvCEdxKS (all three directions, desktop and phone,
light and dark, the other pages, and C's time-of-day frames) and six desktop sign-in images
uploaded with `gb image upload`. Whatever is
picked, the follow-up build step is a separate bead: apply 13.3 (copy, `binaryData`,
chart test), re-shoot against the chart's theme, and the owner signs off visually in S1f's way.

**Picked: A, Dawn Meadow** (owner, Slack, 2026-10-18 ~23:12Z: "yeah, dawn meadow, go with it").
Built by S1h (gd-0SYmLWCCM5) as 13.3 describes: `scene-base.css`, `scene-meadow.css`, the
thirteen meadow tiles, `sprout.svg` and the font (with its OFL file) moved into the chart's
`files/keycloak-theme/gardenio/login/resources/`; `login/theme.properties` lists the two scene
stylesheets after `gardenio.css` (the theme name is still `gardenio`, so `loginTheme` and the
realm do not change; dropping those two entries from `styles=` is the visual rollback). The
ConfigMap template gained a `binaryData` branch (`b64enc`) chosen by extension
(`gardenio.keycloakRuntime.themeIsBinary`) and counts binary files at 4/3 against the 900 KiB
cap; the chart tests check that the font round-trips byte-for-byte, that text files stay in
`data`, that every `url()` in the login CSS resolves to a shipped file, and that a font edit
still changes `checksum/theme`. The forest and hills variants, their generators and the
harness's `THEME_VARIANT` matrix were removed (PR #539 in history keeps them); `gen/` moved to
`gardenio/tests/keycloak-theme/gen/` and redraws the meadow straight into the chart tree, and
`scene.spec.js` now runs on every harness run against the shipped theme.

## 14. S1j: the beads viewer wears the same scene

Step S1j (gd-cxU8eyiGyR). Owner request in the plan thread, 2026-10-18 ~23:54Z, right after
picking A Dawn Meadow: "let's ship a similar theme for beads viewer". The viewer
(`gardenio/controller/cmd/beads-viewer`) now renders the login page's scene, tokens and pixel
sign, so the two read as one product. Nothing about auth, kd or gb changes.

### 14.1 What is shared and how

- **One scene, two renderers.** `templates/static/meadow/img/*.svg` (the thirteen meadow tiles
  and `sprout.svg`) and `fonts/` (Pixelify Sans 600 plus its OFL file) are byte-for-byte copies
  of the chart's `files/keycloak-theme/gardenio/login/resources/`; `TestMeadowAssetsMirrorChartTheme`
  fails if they drift, and caps the folder at 300 KB (it is about 165 KB; a visitor loads one
  colour scheme, about 90 KB). The chart cannot read from the Go embed tree and the Go embed
  cannot read from the chart, so a copy with a test is the cheap "shared location".
- **`meadow.css`** is `scene-meadow.css` plus the card/sign rules of `scene-base.css`, with the
  viewer's selectors. Same tokens (`--gd-*`), same layer sizes, same keyframes, stepped motion,
  `prefers-reduced-motion` turns it all off. Every `url()` is relative and same-origin (tested);
  the viewer's CSP (`img-src 'self'`, `font-src 'self'`) needs no change.
- **Switch.** The server stamps `html[data-theme="meadow"]` and links the stylesheet; every rule
  in `meadow.css` is gated on that attribute (tested). `BEADS_VIEWER_THEME=plain` (chart value
  `beadsViewer.theme: plain`) drops the attribute, the stylesheet, the scene markup and the sign,
  which is the pre-theme page byte-for-byte: rollback is a value, like S1f. Unset, `meadow` or
  an unknown value all mean the meadow, so a typo can never strip the product's face.
- **Light and dark follow the viewer's existing control, the skin switcher.** Day is the
  default `baseline` skin; night is the `dark-terminal` skin (`data-skin^="dark"`, the viewer's
  one dark mechanism, so every existing dark override keeps working). The four other candidate
  skins (minimal-refined, dense-admin, docs-notion, tufte) are an explicit opt-out and are not
  touched. The sign-in picker (`/login`, a standalone template) reads the same stored skin.

### 14.2 How much scene a page shows (the rule)

The scene is one fixed, full-viewport, `pointer-events: none` container (`.gd-scene`,
`aria-hidden`) at `z-index: -1` with seven layers back to front: sky, sun (or moon and stars),
clouds, hills, the garden bed, grass, butterflies (or fireflies). `<body>` is a **solid framed
plate** on top of it, with the login card's one-cell frame. A block background paints over
negative-z-index children, so nothing inside the plate ever sits on the picture.

| Page kind | Plate | What shows |
|---|---|---|
| Dense work pages: feed, active, plans, plan detail, maps, deck, charts, agents, agent chat, terminals, bead show, search, settings | the skin's wide plate (96 vw, up to 2400 px) | sky above the header, the gutters at the sides (about 25 px at 1280, 12 px on a phone), the grass and bed below the footer; on a short page, the whole meadow below the plate |
| Reading pages (`.reading-col`: prime, rendered docs) | the 960 px plate | most of the scene |
| Sign-in picker (`/login`) | no plate; the card is framed like the login page's | the full scene |
| The Homestead front door (`/`, `/garden`) | the wide plate; the page keeps its own farm look inside it | the frame only |

So tables, code, xterm and uPlot stay on `--bg` exactly as before; the meadow is a frame around
the work, not a background under it. The garden bed does **not** grow in on the viewer (pages
reload every half-minute); drift, sway, twinkle and flit are kept.

Chrome on the shell only: the pixel wordmark sign in the nav (text is the accessible name; it
links to `/` in vibe mode, where it replaces the plain "beads" link, and to `/feed` in simple
mode, where Feed IS home, decision ka-0000007562), `h1` in the pixel font, square nav controls
with a one-cell lower edge on the primary, square banners, a framed version pill. Body text
stays on the skin's font. The Homestead `/garden` canvas, xterm, uPlot, ECharts and Block Kit
are untouched. Baseline's hard-coded greys (nav rule, `h2` rule, `.meta`, `.empty`, comment
code blocks, search table rules, banners) map onto the meadow tokens so they sit on the cream
plate; the dark skin already reads tokens, so night is the token swap plus a darker
`--accent-dim` (white hover text on it stays 5.4:1). Three deliberate deviations from the login
card keep the viewer's existing contrast margins, each found by running axe before and after
on the live pages: the day plate is white, not cream (a cream surface takes 0.15 off every link
and chip colour the viewer tuned to exactly AA on white; the cream stays in code blocks, table
headers, the frame's inner line and the sign); body text on the plate is pure black (dozens of
labels are text at 55-60% opacity of the inherited colour); and the day plate defines only
`--bg`, `--text`, `--rule`, `--accent` and the warn/err tokens, leaving `--text-muted`,
`--text-subtle` and the `--bg-soft/alt/head` bands undefined exactly as baseline does, because
components read them without a fallback and rely on inheriting black or painting on white.
Links take the accent through a zero-specificity `:where()` rule so component link colours
(`.chip { color: inherit }`) still win. The not-found response is `http.Error`
plain text, not a page, and is unchanged.

### 14.3 Evidence and sign-off

Before/after screenshots (home, feed, plans, plan detail, agent chat, the sign-in picker and
a not-found response; desktop 1280 and phone 360; light and dark) are attached to the step bead
and the PR and posted in the plan thread; axe (serious + critical, the same gate as the viewer's
`a11y.spec.js`) runs on each after-shot. The owner signs off visually in the thread; tweaks
become a follow-on step. Rollback at any time: `beadsViewer.theme: plain`.

### 14.4 The Homestead skin: the look inside the plate (plan gd-JaMxRKsc8z)

Owner request in Slack, 2026-10-19 (thread gd-nfORKZtBU3): "beads viewer needs homestead style
like dense admin and friends and it should be default; also colorful, natureful, and subtly
animated and adorned and beautiful." S1j put the scene *around* the work on a white plate; this
carries the same palette and pixel face *into* the plate, as a skin.

- **Two skins, one file.** `homestead` (day) and `dark-homestead` (night) are entries in the
  skin switcher next to Dense admin and friends, painted by
  `templates/static/homestead/homestead.css`, which is linked on every layout page and on the
  sign-in picker whether the theme is meadow or plain. Every rule in it is gated on one of the
  two `data-skin` values and never on `data-theme` (`TestHomesteadCSS_ScopedToTheHomesteadSkins`),
  so the six other skins are untouched and the skin survives `BEADS_VIEWER_THEME=plain` (then it
  is the look without the scene, with a plain one-cell frame of its own).
- **Default.** `layout.html` and `login.html` stamp `data-skin="homestead"` and the switcher
  falls back to it when nothing is stored (`TestLayout_HomesteadIsTheDefaultSkin`). A stored
  choice, including `baseline`, still wins before first paint.
- **Night is named `dark-*` on purpose.** The viewer's one dark mechanism is
  `html[data-skin^="dark"]` (Dark terminal's structural block plus 300+ component overrides and
  the dark coverage tests). `dark-homestead` matches it and inherits every dark fix; the file
  undoes only the terminal mannerisms (monospace body, the `$ ` home prompt, `▸ ` bullets, the
  glyphs on empties, labels, buttons and banners) — `TestHomesteadCSS_NightUndoesTheTerminal`.
- **The scene frames both.** Every `meadow.css` selector that matched baseline-or-dark now
  matches `homestead` too (`TestMeadowCSS_FramesTheHomesteadSkins`); the meadow's day *tokens*
  stay baseline's, and `homestead.css` owns the Homestead's (the login scene's `--gd-*` set plus
  `--hs-*` leaf, sun, sky, blossom, berry, soil and wood from `/garden`'s `art.js` palette).
- **Specificity contract.** Rules are `html:is([data-skin=…]) X`: they tie Dark terminal's
  `html[data-skin^="dark"] X` (and win, being linked later) and lose to the theme's
  `html[data-theme="meadow"]:is(…) X` (plate, nav, sign, h1, controls), which is the intended
  layering. The day plate keeps S1j's three deviations: `--text-muted`, `--text-subtle` and the
  `--bg-soft/alt/head` bands stay undefined, body text is black, and the plate is `#fffdf6`
  rather than cream (a hair of warmth that costs 0.1 of contrast ratio where cream cost 0.3).
- **What it draws.** Pixel face on `h1` and `h2`, a leaf mask before every `h2` that grows in
  three steps, a picket fence under the nav shelf, paper surfaces with a one-cell lower edge on
  field lists, comments, search forms and stats strips, cream table heads with a wood rule, type
  and state chips at 0.85 opacity (the viewer's own hues, fuller), a blossom before every empty
  state, a breathing dot on working agents, a wiggling sprout on the sign when hovered, and a
  sun-tinted hover on bead rows. All motion is stepped and off under `prefers-reduced-motion`
  (`TestHomesteadCSS_MotionRespectsReducedMotion`). Art is two 16×16 masks (`img/leaf.svg`,
  `img/flower.svg`) plus the meadow's sprout and font by relative path, same-origin
  (`TestHomesteadCSS_SameOriginOnly`), so the CSP needs no change.
- **The open plate (P3 step 3.0, gd-2skiMOyS7f).** Owner, 2026-10-20: "the colour and nature
  barely gets in around the edges because the whole thing is in a big chunk of text". The plate
  becomes paper laid over the meadow. With CSS anchor positioning (Chromium, Safari 26) the paper
  moves to `body::after` (the shelf, down to just under the fence) and `body::before` (the plate,
  from nine cells lower to the foot), both at z -1 with the scene stepped back to z -2, so a
  window of sky opens between shelf and content. The paper is `--hs-plate-alpha` (88% day, 84%
  night) over a `backdrop-filter` wash of the scene (`--hs-plate-wash`: day
  `contrast(0.4) brightness(1.4)`, night `contrast(0.5) brightness(0.45)`); without anchor
  positioning body paints it flat at `--hs-plate-alpha-flat` (93% day, 94% night), no window, and
  never a filter on body (it would capture the fixed overlays). A pixel vine 9-slice
  (`img/vine-frame-{day,night}.svg`, 56×56 cells, slice 12) runs the edge as `border-image`
  (12-cell image on a 3-cell border, one-cell outset so the grown-over corners lie on the frame
  line); `h1` gets a short vine rule (`img/vine-rule-*.svg`). Night is moonlit greens on a
  pewter stem. Cards, `.reading-col` pages, the constellation panel and the plain theme stay
  opaque; the fleet tile's head drops its 0.85 fade. Art is generated by
  `testdata/homestead_art.js` (10 KB for the four files). Contrast evidence: every text run on
  /agents, /feed and a bead page, day and night, 1280 and 390, top and mid-scroll, measured in
  Chromium against the darkest and lightest pixel actually painted under it — the open plate
  adds no failure over main in either path (main's existing failures are step 2.4's).
  `TestHomesteadCSS_OpenPlate` pins the tokens, opaque surfaces, the window gate and where the
  wash may sit.
- **Gates.** `composer_golden_browser.js` measures the empty composer's placeholder contrast in
  both Homestead skins with the others; the PNG goldens still shoot baseline and dark-terminal.
  Later phases (surface pass, ornaments and time of day, `/lands` and Settings) are the plan's
  P2–P4.
