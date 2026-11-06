# Keycloak login-theme preview harness

CI harness for the gardenio login theme and the passkey sign-in flow
(plan gd-QXq1fjYuRD, step S1b; design doc section 3.C "Preview harness").
It starts a real Keycloak with the chart's theme tree mounted, drives the login
pages with Playwright, and uploads screenshots. Later steps (S1c, S1d, S4a) add
screens and scenarios to it.

```
run.sh                       start Keycloak, wait for /health/ready, run the passkey hook block, run Playwright, stop it
passkey-policy.sh            render the chart's first-admin Job, run its passkey block with kcadm, assert the realm
become-client.sh             run the first-admin Job's scope-mappings block for gardenio-become; impersonate a probe user and check the impersonator claim
realm/gardenio-test-realm.json   realm `gardenio` import (TEST-ONLY throwaway users)
playwright.config.js         4 projects: desktop 1280x800 / mobile 360x740 x light / dark; 6 phone-sweep ones (screens.spec.js only): ios390, ios390land, and360land
tests/screens.spec.js        one screenshot + axe run per reachable screen per project
tests/s1e-email.spec.js      the emails Keycloak sends (password reset, admin account actions): MIME, render, screenshot
tests/passkey.spec.js        register + sign in with a passkey (CDP virtual authenticator)
tests/scene.spec.js          the shipped pixel-art scene: same-origin, 300 KB, pixel font on headings only, reduced motion, markup untouched
build-flow.sh                helm template -> the chart's passkey KeycloakAuthFlow CRs -> kcadm flows in the realm, bound as browser flow (S4a)
tests/passkey-flow.spec.js   the seven sign-in paths through that flow (S4a)
tests/lib/kc.js              shared helpers (credentials, version check, naming)
gen/                         node gen/index.js: redraws the shipped login scene's SVGs into the chart's theme tree (deterministic)
../../../.github/workflows/keycloak-theme.yml   the CI job
```

## Run it

Needs Docker, Node 20+, `helm`, `yq` and `jq` (all on GitHub's `ubuntu-latest`).

```bash
cd gardenio/tests/keycloak-theme
./run.sh                                  # image = chart value keycloak.runtime.image
KC_IMAGE=quay.io/keycloak/keycloak KC_TAG=26.5.2 ./run.sh   # override the chart tag
./run.sh -- tests/screens.spec.js --project=mobile-dark     # extra Playwright args
AXE_ENFORCE=0 ./run.sh                    # report axe violations instead of failing on them
```

Outputs, all in this directory and git-ignored: `screenshots/`, `axe-report/`,
`playwright-report/`, `kc.log`, `realm-webauthn.json`, `realm-webauthn-after-hook.json`.

**The image follows the chart.** `run.sh` reads `keycloak.runtime.image`
(registry, repository, tag, digest) from `deploy-values/gardenio-k3s.yaml` first
and `gardenio/helm/gardenio/values.yaml` second, the way `helm -f` layers them,
so the harness follows a Keycloak upgrade. The workflow's `kc_image`/`kc_tag`
dispatch inputs (or `KC_IMAGE`/`KC_TAG`) override it, and drop the chart digest.

**The mounted theme is the shipped bytes.** The container mounts
`gardenio/keycloak-theme/gardenio` read-only at
`/opt/keycloak/themes/gardenio`, the same tree the release deployer ships as a
ConfigMap (gardenio/scripts/keycloak-theme-configmap.py). The ConfigMap path-encoding (keys cannot contain `/`) is covered by
its unit test and the chart test, not here. The realm sets `loginTheme: gardenio`.

## Screens

File names are `<screen>-<form>-<scheme>.png` (`form` = desktop|mobile,
`scheme` = light|dark), full-page, in `screenshots/`. A matching
`axe-report/<same name>.json` lists that page's axe violations.

| screen | how it is reached |
|---|---|
| `login` | `/realms/gardenio/account` while signed out |
| `login-error` | bad password on `login` |
| `forgot-password` | the "Forgot Password?" link (realm `resetPasswordAllowed: true`) |
| `login-page-expired` | load `login`, drop the cookies, submit the stale form |
| `error` | authorization request with an unregistered `redirect_uri` |
| `login-update-password` | sign in as `test-pwchange` (required action `UPDATE_PASSWORD`) |
| `account-signingin` | account console Account security > Signing in, signed in; wears the S1e account theme (checked in `tests/lib/account.js`: tab title, wordmark loaded, page/card colours are the gardenio tokens in light and dark, the hamburger in the bar's light ink, the drawer closed again after navigating on a phone, axe `color-contrast` and `button-name` clean) |
| `email-password-reset`, `email-execute-actions` | the emailed HTML read back from the SMTP sink and rendered in a clean page under the project's colour scheme (`tests/s1e-email.spec.js`); also asserts the text/plain part, inline styles, no remote resources |
| `logout-confirm` | `/protocol/openid-connect/logout` with no `id_token_hint`, signed in |
| `logout-done` | confirm the logout; whichever page Keycloak shows next (recorded in the `page:logout-done` annotation) |
| `device-code` | `/realms/gardenio/device` after a device authorization for the public `gardenio-cli`-shaped client |
| `device-grant` | enter the user code, sign in, `gardenio-cli` has `consentRequired` |
| `device-success` | accept the device grant |
| `oauth-grant` | authorization-code request for `gardenio-consent-test` (`consentRequired`) |
| `device-code-invalid` | a wrong code on `device-code` |
| `webauthn-authenticate` | client `gardenio-passkey-test`, bound (`authenticationFlowBindingOverrides`) to a flow whose only step is the passwordless passkey authenticator; made by `global-setup.js` through the admin API |
| `webauthn-error` | the same page after a click with an empty virtual authenticator (nothing to offer, so the browser answers "not allowed") |
| `webauthn-register` | sign in as `test-passkey` (required action `webauthn-register-passwordless`) |
| `login-config-totp` | sign in as `test-totp` (required action `CONFIGURE_TOTP`) |
| `forgot-password-sent` | submit the forgot-password form; the "check your email" step |
| `login-verify-email` | sign in as `test-verify-<form>-<scheme>` (required action `VERIFY_EMAIL`; one user per project) |
| `info-confirm`, `info` | follow the emailed verification link in a fresh browser context: "Confirm your email address", then (after Continue) "Email address verified" |
| `passkey-register`, `passkey-signin-button` | taken by the passkey spec (Keycloak >= 26.4 only) |

**Mail.** The realm's `smtpServer` is `host.docker.internal:2525`; `run.sh` starts the
container with `--add-host=host.docker.internal:host-gateway`, and `global-setup.js`
runs a tiny SMTP sink (`tests/lib/mail.js`) that writes each message to a temp dir.
Specs read the emailed link from there; nothing leaves the machine.

Axe is a **hard gate** since S1c: a serious or critical violation (colour contrast
at AA included) on any screen, in any of the four projects, fails the run.
`AXE_ENFORCE=0` makes it a report again. `account-signingin` is exempt from the
general gate (the account console is a separate app) but, since S1e, is held to its
own checks including colour contrast and button names. Every other screen must also be themed: no "Keycloak" in the
title or visible text, no sideways scroll at 360px, the wordmark in the header and
a `lang` on `<html>`. The `login` test also checks 44px touch targets and a visible
keyboard focus ring.

### Email and account themes (S1e)

The realm sets `accountTheme` and `emailTheme` to `gardenio`, like the chart's
`keycloak.realm.themes.*` values. `account/` is `parent=keycloak.v3` plus
`css/gardenio-account.css` (PatternFly 5 custom properties re-pointed at the login
tokens; no template overrides), `js/gardenio-account.js` (closes the phone drawer
after a link in it is chosen and names the icon-only menu buttons) and the wordmark
in `img/logo.svg`. `email/` is
`parent=base`: the layout `html/template.ftl` plus the five HTML bodies the realm
sends (verify email, reset password, admin account actions, email change, SMTP test),
all inline-styled with a text wordmark; the plain-text alternatives are the base
templates with the gardenio wording from `messages/messages_en.properties`.

### Not reached

- `info` after a *logout* may land on the login page instead, depending on the
  version; `info` itself is reached through the verification link (above).
- The real realm's `gardenio-mfa-browser` flow and TOTP: the test realm uses
  Keycloak's built-in browser flow.
- `login-reset-password`'s emailed link (the page that follows it is the
  update-password screen, already captured), `login-otp` and the recovery-code
  pages: no flow in the test realm reaches them and the chart does not use them.

### Template inventory (Keycloak 26.7.5, `keycloak-themes` jar)

The login templates the pinned version ships, read from the Maven `keycloak-themes`
jar (`theme/base/login`, `theme/keycloak/login`, `theme/keycloak.v2/login`). How each
is handled by the gardenio theme (parent `keycloak.v2`):

| template | handling |
|---|---|
| `login`, `login-username`, `login-password`, `select-authenticator`, `register`, `login-reset-password`, `login-update-password` | CSS + messages (S1c/S1d) |
| `webauthn-authenticate`, `webauthn-register`, `webauthn-error`, `login-passkeys-conditional-authenticate` | CSS + messages only; **not overridden**, so the parent's script includes and `autocomplete="username webauthn"` stay as shipped |
| `login-config-totp` | override: fixes two `<label for>` ids that point nowhere (axe `label`, critical) and the English barcode alt text |
| `login-oauth-grant` | override: "Allow" / "Do not allow" instead of "Yes" / "No"; same ids, names and hidden code |
| `login-oauth2-device-verify-user-code` | CSS + messages (large monospaced code field, "Enter the code shown in your terminal") |
| `login-verify-email`, `info`, `error`, `login-idp-link-*` | CSS + messages |
| `login-page-expired`, `logout-confirm` | overrides from S1c |
| `login-otp`, `login-reset-otp`, `login-recovery-authn-code-*`, `login-update-profile`, `update-email`, `idp-review-user-profile`, `terms`, `delete-credential`, `delete-account-confirm`, `select-organization`, `login-x509-info`, `link-idp-action`, `code`, `cli_splash`, `frontchannel-logout`, `saml-post-form`, `oid4vc-credential-offer` | not used by gardenio's realm today; they inherit the theme CSS and the stock wording |

## One-click Become

`become-client.sh` (plan gd-0SUIdKXWV3, step gd-rbML2txvGG) runs after the passkey
hook block, on every Keycloak version. It takes the `gardenio-become` client from
`deploy-values/gardenio-k3s.yaml`, renders the chart's `first-admin-job.yaml` with it,
cuts out the text between the `# --- scope-mappings:begin` / `# --- scope-mappings:end`
markers, creates the client as the operator would, runs the block **twice** and reads
the client's `realm-management` scope mappings back (`impersonation`, `query-users`,
`view-users`). It then creates a probe client with the overlay forwardauth client's two
session-note mappers, impersonates a probe user through the admin API, walks the code
flow on that session with curl, and asserts both tokens carry `impersonator` (the
master admin's username) and `impersonator_id`. The same user's own password sign-in
must carry neither.

## Passkeys

**The shipped hook block (S3a).** `passkey-policy.sh` runs between the realm import
and Playwright (Keycloak >= 26.4 only). It renders the chart's `first-admin-job.yaml`
with `realmHardening.passkeys.enabled=true` and `hostnameUrl=http://localhost:8080/auth`,
cuts out the text between the `# --- passkeys:begin` / `# --- passkeys:end` marker
comments, runs it with `kcadm` inside the container **twice**, reads the realm back
and asserts every `webAuthnPolicyPasswordless*` attribute (`PasskeysEnabled`, `RpId`,
`RpEntityName`, `UserVerificationRequirement`, `ResidentKey`,
`AttestationConveyancePreference`, `AuthenticatorAttachment`, `SignatureAlgorithms`,
`AvoidSameAuthenticatorRegister`). That proves the commands the cluster runs, not a
copy. The RP ID is `localhost` here, which is also what the passkey spec below needs.
The deprecated `...RequireResidentKey` is not set; the chart uses `...ResidentKey`.

`tests/passkey.spec.js` opens a CDP session, runs `WebAuthn.enable` and adds a
virtual authenticator (`ctap2`, `internal`, resident key, user verification,
`isUserVerified`, `automaticPresenceSimulation`). It signs in with a password,
registers a passkey from the account console, signs out, and signs back in:
test 1 with conditional UI (focus the username field), test 2 with the "Sign in
with Passkey" button (modal UI). Only the credential *count* is asserted; ids
are never logged. It runs in one project (`desktop-light`).

Passkeys are supported from Keycloak **26.4**. The chart pins 26.7.5 (S2b), so
the spec runs by default. On an older server (a dispatch with an old `kc_tag`) the tests **skip** with `Passkeys ... need Keycloak >= 26.4; this
server is <version>`. The version comes from the admin `serverinfo` endpoint
(bootstrap admin from `run.sh`), falling back to `KC_TAG`.

**Realm attributes.** The passkey policy is `webAuthnPolicyPasswordless*` in the
realm JSON. The names were read from the server, not guessed: `run.sh` runs
`kcadm.sh get realms/gardenio` against the container after import and keeps the
`webAuthnPolicy*` keys in `realm-webauthn.json` (uploaded as a CI artifact).
`webAuthnPolicyPasswordlessPasskeysEnabled` only exists from 26.4 and an older
server rejects the unknown field on import, so `run.sh` adds it to a rendered
copy of the realm only when the tag is >= 26.4 (non-numeric tags are assumed new).

## Findings

Proven on `quay.io/keycloak/keycloak:26.5.2` (PR #517 run, dispatched through
the temporary `KC_TAG` default) and on the chart's then-current `26.0.7` (passkey tests
skipped; the chart is on 26.7.5 now and they run):

- **Conditional UI works under the virtual authenticator, with no click.** The
  login page starts a conditional-mediation request on load, and Chromium
  answers it from the resident credential in the virtual authenticator, so a
  fresh visit lands signed in. Consequence for later steps: a test that needs to
  *see* the login page while a credential exists (a screenshot, the modal
  button) must keep that request from the authenticator. Holding presence back
  with `WebAuthn.setAutomaticPresenceSimulation {enabled: false}` is not enough:
  Chromium reads presence only when it gets to the request, which can be after
  the test re-enables it, and then the request signs in before the button is
  clicked (gd-PrwqK1y4zd). The modal tests call `holdConditionalUI` (`tests/lib/kc.js`)
  instead: an init script keeps every conditional request in the page until the
  button's handler aborts it, so presence stays on. The autofill token is asserted separately, on a page with no
  authenticator (`autocomplete="username webauthn"`), so the S1c template work
  is checked even though the conditional sign-in itself is not click-driven.
- **Keycloak 26.5 differences the harness absorbs:** the realm import file must
  be named `<realm>-realm.json` (`run.sh` stages it that way); the account
  console uses path routes, not hash routes (the harness clicks through the
  navigation to Account security > Signing in); "Sign in with Passkey" is an
  `<a href="#">` rather than a button.
- **Realm gotchas:** an imported realm needs a `defaultRole` with the `account`
  client roles and users need `default-roles-gardenio`, or the account console
  token has no audience and every account API call answers 401 ("Something went
  wrong"). Without `webAuthnPolicyPasswordlessPasskeysEnabled` on >= 26.4 the
  login page shows no passkey control.
- **Passkey spec does not retry:** a failed attempt can leave a passkey on the
  server, and a retry then fails with "Device already exists with the same name".
- **Not-yet-themed finding:** the stock theme's axe report on every screen has
  `html-has-lang` (serious); the grant pages add `heading-order` (moderate).

## The pixel-art scene (Dawn Meadow) and its generator

The chart's login theme carries a pixel-art nature scene since S1h (gd-0SYmLWCCM5): the owner
picked direction A, Dawn Meadow, from the three S1g candidates (plan gd-QXq1fjYuRD, design doc
section 13; decision gd-TCaihXOgWd). It lives inside the one `gardenio` theme as two extra
stylesheets (`css/scene-base.css`, `css/scene-meadow.css`), thirteen SVG tiles under `img/`, and
the pixel heading font under `fonts/` (Pixelify Sans, SIL OFL, licence file beside it). The
`.ftl` templates are untouched. The losing candidates (forest, hills) and their generators were
removed with this step; PR #539 in git history has them.

```
gen/index.js      node gen/index.js: redraws every scene SVG into the chart's login/resources/img/ from seeded code (deterministic: a re-run leaves `git diff` empty)
gen/meadow.js     the meadow's sprites, layers and night recolour
gen/lib.js        the grid, noise and SVG writer
```

`tests/scene.spec.js` runs on every harness run and holds the scene to its constraints: every
request stays on the Keycloak origin (no CDN, no `data:` URI), one colour scheme's images and
font stay under 300 KB, the pixel font is on the page title only, the layers animate normally
and stop under `prefers-reduced-motion`, and the `#username` passkey autofill hook and the
kc* markup are the production ones.

## The passkey browser flow (S4a)

After the screenshot/axe/passkey pass, `run.sh` does a second pass on the same container
(default runs only; `RUN_FLOW=1` forces it with Playwright args, `SKIP_FLOW=1` skips it):

1. `build-flow.sh` renders `templates/keycloak/auth-flow-mfa.yaml` with
   `keycloak.mfa.factor=passkey`, takes the `KeycloakAuthFlow` CRs and creates the same
   flows, executions, requirements and authenticator configs with `kcadm` (parents first,
   executions in priority order), binds the top flow as the realm `browserFlow`, then reads
   everything back and fails if the realm differs from the CRs. It also dumps
   `browser-builtin-executions.json` (the stock flow) and `conditional-credential-config.json`
   (the config schema Keycloak ships), uploaded with the diagnostics.
2. `tests/passkey-flow.spec.js` (`KC_FLOW=passkey`, `desktop-light` only) runs: (1) admin with a
   passkey, conditional UI; (2) the "Use a passkey" button (`#authenticateWebAuthnButton`); (3) username + password, then a
   passkey; (4) admin with a password and no passkey; (5) non-admin, password only; (6) wrong
   password; (7) the `kd login` shaped device flow approved with a passkey. Each scenario makes its
   own user. "No second prompt" is measured as the virtual authenticator's `signCount` delta.

Findings, on Keycloak 26.7.5:

- `conditional-credential` takes `credentials` (a list, `##`-delimited) and `included`
  (boolean). The flow uses `credentials=webauthn-passwordless`, `included=false`: true unless a
  passkey was the primary credential. A passkey picked from the username box or the modal button
  skips the second factor (scenarios 1, 2, 7: exactly one assertion); a typed password gets
  the passkey prompt (scenario 3: exactly one assertion).
- **`webauthn-authenticator-passwordless` does not enrol.** Its factory does not allow user setup,
  so an admin with a password and no passkey ends on "Cannot login, credential setup required."
  (scenario 4). The design assumed a REQUIRED step would attach the register action, as
  `auth-otp-form` does; it does not. The flow fails closed instead, so a passkey is enrolled
  *before* the admin role is held (design doc 3.D, 4.1 and 7 are corrected in S4a).
- The second-factor page is "Use your passkey" (S1d copy; stock "Passkey login") with a "Use a passkey" button; with presence
  held back it waits, and the button answers it once presence is on.
- The themed bad-password message is "That username or password is not right."
