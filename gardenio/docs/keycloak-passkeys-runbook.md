# Passkeys: enrollment, recovery and break-glass runbook

**Beads:** step gd-diK82eIdBJ (S3b) of plan gd-QXq1fjYuRD, phase 3 gd-rZZhfrpMDv.
**Design:** `docs/superpowers/specs/2026-10-18-keycloak-passkeys-login-theme-design.md`
(sections 3.D, 3.E, 7). **Related:** [`keycloak-mfa-hardening.md`](keycloak-mfa-hardening.md)
(how the second factor is wired), [`first-admin-bootstrap-runbook.md`](first-admin-bootstrap-runbook.md)
(the first admin, and the console-less fallback).

Every way of getting locked out of `https://auth.clonk.tv` has a recipe below, and
every command in a recipe was run against a real Keycloak by CI
(`.github/workflows/keycloak-runbook-verify.yml`, [section 8](#8-what-was-verified)).
A recipe that was never run is a guess.

**Where things stand.** Since S4b (gd-wO9MDjpz13, live 2026-10-18 21:50Z) the live admin
flow is `keycloak.mfa.factor: "passkey"` (`deploy-values/gardenio-k3s.yaml`): an admin
signs in with a passkey, or with a password followed by a passkey. An admin without a
working passkey cannot sign in, and sections 3 to 6 are the way back in.

**TOTP is retired** (S4c gd-GiZfavru5G, 2026-10-31). After a 13-day soak with no sign-in
problem reported, the remaining TOTP (`otp`) credentials of realm users were deleted and
the docs stopped telling anyone to use an authenticator app. `totp` stays a valid
`keycloak.mfa.factor` value, but only as a last-resort rollback (section 5 B).

## Rules for every command in this document

- **Never print or paste a password, token or credential id** into a bead, Slack
  message, PR or log. Reporting a count or list of credential *types* (`password`,
  `webauthn-passwordless`) is fine; an id is not, and neither is the label text of a
  device you do not want named.
- Run `kcadm` **inside the Keycloak pod**. The master-realm admin credentials are already
  in that pod's environment (`KC_BOOTSTRAP_ADMIN_USERNAME`, `KC_BOOTSTRAP_ADMIN_PASSWORD`,
  both from the Secret `gardenio-keycloak-runtime-admin`), so nobody reads the Secret by
  hand and the password never appears in a command you type. Credential ids stay in shell
  variables.
- The pod shell has `sed`, `grep`, `cut`, `tr`, `head`, `tail`, `sort`, `uniq`, `wc` and
  `bash`. It has **no** `awk`, `jq`, `python3`, `unzip` or `curl`.
- Only the laptop operator has `kubectl`. Crew pods do not; they file an escalation with
  the exact commands (`--operation-type kcadm_read_only` for the drill in
  [section 7](#7-the-read-only-drill)).

## 0. One shell in the pod

All the recipes below assume this. Open a shell, paste the prelude, and leave the shell
open for the whole sitting.

```bash
kubectl -n gardenio exec -it gardenio-keycloak-runtime-0 -- bash
```

Then, inside the pod:

```bash prelude
K=/opt/keycloak/bin/kcadm.sh; C="--config /tmp/kcadm.config"
kc() { $K "$@" $C; }
login() { kc config credentials --server http://localhost:8080/auth --realm master --user "$KC_BOOTSTRAP_ADMIN_USERNAME" --password "$KC_BOOTSTRAP_ADMIN_PASSWORD"; }
uid() { local i n; while IFS=, read -r i n; do [ "$n" = "$1" ] && { echo "$i"; return 0; }; done < <(kc get users -r gardenio -q "username=$1" --fields id,username --format csv --noquotes); return 1; }
```

- `kc` is `kcadm.sh` with a config file under `/tmp` (the image's own default location is
  read-only).
- `login` authenticates as the master-realm bootstrap admin from the pod environment. If it
  fails with `invalid_grant` the password in the pod environment is no longer the master
  admin's current one (someone changed it in the master realm); use the Secret
  `gardenio-keycloak-runtime-admin` only as a last resort, and see [section 6](#6-the-master-realm-admin-and-the-console).
- `uid <email>` prints that user's id. It filters to the **exact** username because
  `kcadm get users -q username=<x>` is a *substring* search (it also returns
  `xowner@example.com` for `owner@example.com`); taking the first row is how you reset the
  wrong person.
- When you are done, run `rm -f /tmp/kcadm.config` (it holds a session token) and `exit`.

## 1. Enrollment (the owner, click by click)

Do this on the laptop first, then repeat on the phone. Each device needs a screen lock
(Windows Hello PIN or face, Touch ID, Face ID, Android or iPhone lock screen) because the
passkey policy requires **user verification**.

1. In a browser on the device, open
   `https://auth.clonk.tv/auth/realms/gardenio/account/`.
2. Sign in the usual way: pick a passkey you already have, or type your email and password
   (an admin is then asked for a passkey). A new admin enrols here *before* holding the
   `gardenio-admin` role (section 4, last paragraph).
3. In the left menu choose **Signing in**.
4. Find the **Passkey** entry (Keycloak 26.7's account console files it under a
   **Passwordless** heading) and press its **Set up Passkey** button. If it says
   "Passkey is not set up", that is the same entry before you have added one.
5. The page asks the browser to create a passkey and your device shows its own prompt
   ("Use your fingerprint / face / PIN", or "Choose where to save this passkey"). Approve it
   with the biometric or PIN. This is the device prompt, not a Keycloak page; if nothing
   appears, see the table below.
6. Keycloak asks you to name it (the box is pre-filled with "Passkey (Default Label)").
   Type something you will recognise later: **`laptop`**, **`phone`**. Passkeys on one
   account need **distinct** labels.
7. Back on **Signing in** the new passkey is listed with its label and a **Created** date.
   Repeat steps 1 to 7 on the phone, then check **both** labels are listed (reload the page
   once).
8. Test it. Sign out (top right), open
   `https://auth.clonk.tv/auth/realms/gardenio/account/` again and click into the email
   box: the browser offers **Select your passkey** (autofill). Pick it, approve with the
   biometric or PIN, and you are signed in without typing a password. A hardware key cannot
   autofill: use the **Sign in with Passkey** button on the sign-in page.

| What you see | What it means |
|---|---|
| No device prompt at step 5 | The browser blocks passkeys (private window, an extension, an old browser) or the screen lock is not set. Use a normal window of Chrome, Edge, Safari or Firefox, set a screen lock, retry. |
| "Failed to register your Passkey" | The device refused or the prompt was dismissed. Retry. If it names a duplicate, this device already has a passkey on the account: you do not need a second one for the same device. |
| An old USB security key is refused | U2F-only keys cannot do discoverable credentials plus user verification. A FIDO2 key with a PIN works; Windows Hello, Touch ID, Android and iPhone all work. |
| After sign-in you land on the account page, not where you started | Normal: you opened the account console directly. |

## 2. Day to day: add or remove a passkey

Account console, **Signing in** (same URL as above):

- **Add** one: the same **Set up Passkey** button; give it a new label.
- **Remove** one: press **Remove** (some versions say **Delete**) next to its label; the
  dialog asks "Stop using Passkey?": press **Confirm**.
- Never remove your last working passkey without another one already enrolled and tested.
  After S4b an admin with no passkey cannot sign in (see section 4).
- A new device means a new passkey. Do it while the old device still works.
- Synced passkeys (iCloud Keychain, Google Password Manager, a password manager) are
  accepted; Keycloak cannot and does not tell them from device-bound ones. A synced passkey
  follows you to your other devices of that ecosystem, which is convenient and is not the
  same as two independent devices.

## 3. Lost one device

You still have one working passkey, so no admin is needed.

1. Sign in with the surviving passkey.
2. Account console → **Signing in** → remove the passkey of the lost device.
3. Enroll a replacement on the new device (section 1) with a new label.

If the lost device was stolen rather than mislaid, also change the account password and ask
an admin to run the sign-out step in [section 4](#4-lost-every-passkey) for your user.

## 4. Lost every passkey

Needs the laptop operator and the shell from [section 0](#0-one-shell-in-the-pod). Replace
the email.

```bash lost-all
lostall() {
  : "${OWNER:?set OWNER=<email> first}"
  login >/dev/null || { echo "STOP: could not authenticate as the master admin"; return 1; }
  local U CID TYPE
  U=$(uid "$OWNER"); [ -n "$U" ] || { echo "STOP: no such user"; return 1; }
  # 1. Remove every passkey. The ids stay in these variables; nothing prints them.
  kc get users/$U/credentials -r gardenio --fields id,type --format csv --noquotes |
    while IFS=, read -r CID TYPE; do
      [ "$TYPE" = webauthn-passwordless ] && kc delete users/$U/credentials/$CID -r gardenio && echo "removed one $TYPE"
    done
  # 2. A temporary password. It prompts for it ("Enter password:", once, no confirmation:
  #    type it carefully). The user must change it at their next sign-in.
  kc set-password -r gardenio --username "$OWNER" --temporary
  # 3. Sign out every live session of that user.
  kc create users/$U/logout -r gardenio -s realm=gardenio
  echo "credential types now: $(kc get users/$U/credentials -r gardenio --fields type --format csv --noquotes | tr '\n' ',' | sed 's/,$//')"
  echo "required actions now: $(kc get users/$U -r gardenio --fields requiredActions --format csv --noquotes)"
}
```

Then run it for the user who lost their passkeys:

```bash
OWNER=<email>; lostall
```

Check the last two lines: the credential types should list no `webauthn-passwordless`, and
the required actions should include `UPDATE_PASSWORD`. **`set-password` exits 0 even when
the realm's password policy rejects the password** (it prints `Invalid password: ...`), so
read its output; a user whose list of credential types has no `password` has no usable
password at all. Then tell the user the temporary password **out of band** (in person, a
call), never in Slack or a bead. At their next sign-in they enter the temporary password,
choose a new one, and are walked through enrolling a passkey (section 1 from step 4).

**When `keycloak.mfa.factor` is `passkey` (design 4.1, measured by the S4a harness).** The
last paragraph changes: an admin with a password and no passkey is **refused** at sign-in
("credential setup required"), not walked through enrolling, because Keycloak's passwordless
authenticator does not offer enrolment. So after `lostall`, take the admin role away before the
user signs in, let them enroll as a plain user, then give the role back:

```bash
# before the user signs in (and again, with add-roles, once `credential types` shows webauthn-passwordless)
kc remove-roles -r gardenio --uusername "$OWNER" --rolename gardenio-admin
# if the role comes from a group, remove the user from that group for the same window
```

With `totp` or `webauthn` as the factor nothing changes.

**Security note (design 7).** Removing a user's last passkey opens a window: until they
enroll a new one, the password alone is enough to get in: on `totp` or `webauthn` because a
REQUIRED second factor with no credential enrolls instead of denying, and on `passkey`
because the admin role is off for that window (above), so the password opens a plain-user
session. Close the window by doing the password reset in the same sitting (step 2 above) and
having the user enroll immediately, with you waiting. Step 3 (sign-out) matters when a
compromise is suspected: without it an attacker's existing session survives the reset.

## 5. The flow itself is broken

Symptom: every admin is refused or loops on the sign-in page, e.g. after a bad flow roll.
The master realm has its own stock browser flow (password only), so you can still authenticate
to *Keycloak's admin API* with [section 0](#0-one-shell-in-the-pod). Two moves:

**A. Fast, temporary: put the gardenio realm on Keycloak's stock browser flow.**

```bash flow-stock
flowstock() {
  login >/dev/null || { echo "STOP: could not authenticate"; return 1; }
  kc update realms/gardenio -s browserFlow=browser &&
    echo "browserFlow now: $(kc get realms/gardenio --fields browserFlow --format csv --noquotes)"
}
flowstock
```

Admins can now sign in with a password alone (no second factor), so do this only to get in
and fix the cause, then undo it. **The operator owns `browserFlow`** (the `KeycloakRealm`
CR sets it to `gardenio-mfa-browser`), so expect it to be put back the next time it
reconciles, at the latest on the next roll. Treat the stock flow as a window of minutes
to sign in during, not a place to park.

**B. The real fix is in the chart.** Revert the bad change and roll. As a last resort set
`keycloak.mfa.factor: totp` (or `enabled: false`) in `deploy-values/gardenio-k3s.yaml` and
roll. TOTP is retired and its credentials were deleted (S4c), so on `totp` every admin is
walked through enrolling an authenticator app at the next sign-in (`auth-otp-form` enrols,
the passkey authenticator does not): that is a weaker factor and the reason it is the last
resort (design 6, step 5c).

To undo A without waiting for the operator:

```bash flow-restore
kc update realms/gardenio -s browserFlow=gardenio-mfa-browser &&
  echo "browserFlow now: $(kc get realms/gardenio --fields browserFlow --format csv --noquotes)"
```

## 6. The master-realm admin and the console

The master realm is separate from `gardenio`: its own users, its own browser flow. A broken
gardenio flow cannot lock it out. It is **not** reachable through the public edge (only
`/auth/realms/gardenio/` and `/auth/resources/` are), so you reach it from inside the
cluster.

- **Preferred: `kcadm` in the pod** ([section 0](#0-one-shell-in-the-pod)). It talks to
  `http://localhost:8080/auth` inside the pod, needs no browser and no password on your
  screen. Everything in sections 3 to 5 is this.
- **Browser console through a port-forward: do not count on it.** The harness measured
  the master console page with the StatefulSet's hostname settings
  (`KC_HOSTNAME=https://auth.clonk.tv/auth`): it tells the browser its `authUrl`,
  `authServerUrl`, `adminBaseUrl` and `serverBaseUrl` are all
  `https://auth.clonk.tv/auth`, and the master realm's OIDC `authorization_endpoint` is
  `https://auth.clonk.tv/auth/realms/master/...`. The public edge answers 404 for
  `realms/master` and `/auth/admin`, so a console opened at `http://localhost:8080/auth/`
  redirects to a URL the edge refuses. Setting `KC_HOSTNAME_ADMIN` moves `authUrl` and
  `adminBaseUrl` to localhost but leaves `authServerUrl` and the authorization endpoint
  public ([section 8](#8-what-was-verified)). Nobody drove a real browser through it, but
  the console does not need to be the plan: every recovery above is `kcadm`.
  If you do need the console, `kubectl -n gardenio port-forward gardenio-keycloak-runtime-0
  8080:8080`, and the master `admin` password is in the Secret
  `gardenio-keycloak-runtime-admin`; copy it without showing it, for example on macOS
  `kubectl -n gardenio get secret gardenio-keycloak-runtime-admin -o jsonpath='{.data.password}' | base64 -d | pbcopy`
  (and clear the clipboard afterwards). Stop the port-forward (`Ctrl-C`) when done.
- **No admin of any kind can get in** (the pod will not start, the Secret is gone): that is
  not a passkey problem; use [`first-admin-bootstrap-runbook.md`](first-admin-bootstrap-runbook.md) §3.

## 7. The read-only drill

Run this before anyone depends on passkeys, and again after the S4b flip. It changes
nothing: it authenticates as the master admin, finds the owner, and lists credential
**types**. The laptop operator runs it; crew files the escalation. Set the owner's email,
paste [the prelude](#0-one-shell-in-the-pod), then:

```bash drill
drill() {
  : "${OWNER:?set OWNER=<email> first}"
  if login >/dev/null 2>&1; then echo "authenticated: yes"; else echo "authenticated: no"; return 1; fi
  local U; U=$(uid "$OWNER")
  [ -n "$U" ] || { echo "owner credential types: (owner not found)"; return 1; }
  echo "owner credential types: $(kc get users/$U/credentials -r gardenio --fields type --format csv --noquotes | tr '\n' ',' | sed 's/,$//')"
  echo "browserFlow: $(kc get realms/gardenio --fields browserFlow --format csv --noquotes)"
  echo "labels (your terminal only, do not report): $(kc get users/$U/credentials -r gardenio --fields type,userLabel --format csv --noquotes | tr '\n' ';')"
}
```

Then run it for the owner:

```bash
OWNER=<email>; drill
```

Report back **only** these three lines, and nothing else (no ids, no labels):

```text
authenticated: yes|no
owner credential types: <comma list, e.g. password,webauthn-passwordless,webauthn-passwordless>
browserFlow: <alias>
```

Then `rm -f /tmp/kcadm.config` and `exit`. The expected answer today is `authenticated: yes`,
types containing `password` and `webauthn-passwordless` and no `otp` (S4c deleted it), and
`browserFlow: gardenio-mfa-browser`.

### 7.1 Live drill record

Append a row each time the drill is run against the live pod. Report lines only: no ids, no
labels.

| Date (UTC) | Keycloak | Run by | authenticated | owner credential types | browserFlow | Context |
|---|---|---|---|---|---|---|
| 2026-10-18 ~20:40 | 26.7.5 | laptop operator (escalation gd-kWkTyHgfaG) | yes | password,otp | gardenio-mfa-browser | post-S3c, before the owner enrolled a passkey (S3b gd-diK82eIdBJ) |

## 8. What was verified

`.github/workflows/keycloak-runbook-verify.yml` (script
`scripts/keycloak-runbook-verify/run.sh`) starts a throwaway Keycloak with the live image
tag, the live `KC_HTTP_RELATIVE_PATH=/auth` and a bootstrap admin supplied **only through the
container environment**, then runs the `prelude`, `drill`, `lost-all`, `flow-stock` and
`flow-restore` blocks of this file exactly as written (it extracts them from the markdown, so
the doc cannot drift from what CI ran) against a seeded realm: an owner with a password and
two `webauthn-passwordless` credentials, a look-alike user, and the `gardenio-mfa-browser`
flow. Result for Keycloak 26.7.5, run 39156645359 (the doc text as of this commit, apart from this run number): **0 failures**.

| Claim | Evidence |
|---|---|
| `kcadm.sh` documents every flag used: `config credentials --server --realm --user --password`, `get --fields --format --noquotes -q`, `update -s`, `create -s`, `delete -r`, `set-password --username --new-password --temporary` | `kcadm.sh <cmd> --help` captured per subcommand; each flag grepped |
| The pod shell has `sed grep cut tr head tail sort uniq wc tee` and lacks `awk jq python3 unzip curl` | `command -v` in the image |
| `get users -q username=<x>` is a substring search; `uid` returns the exact row only | seeded `owner@...`, `xowner@...` and `owner@....au`: the drill read the owner and `lost-all` left `owner@....au` untouched |
| `get users/<id>/credentials --fields id,type --format csv --noquotes` prints `<id>,<type>` rows; `--fields type` one type per line | session output |
| Deleting each `webauthn-passwordless` credential by id from shell variables works and prints no id | `lost-all`: "removed one webauthn-passwordless" twice; output pattern-checked for UUIDs |
| `set-password --temporary` leaves `UPDATE_PASSWORD` pending | `required actions now: UPDATE_PASSWORD` |
| Without `--new-password` and without `KC_CLI_PASSWORD`, `set-password` prompts once (`Enter password:`, no confirmation) on a TTY; with `KC_CLI_PASSWORD` set it does not prompt | TTY run and non-TTY run |
| `delete users/<id>/sessions` does **not** work (exit 1); `create users/<id>/logout -r gardenio -s realm=gardenio` does | the design doc's sign-out line was wrong and is fixed |
| `update realms/gardenio -s browserFlow=browser` and back read back correctly | `flow-stock`, `flow-restore` |
| The drill prints exactly the three report lines (plus a terminal-only labels line) and no id | `drill-before.txt`, `drill-after.txt` |
| The account console says **Signing in**, **Set up Passkey**, **Passkey is not set up**, **Stop using Passkey?** and files passkeys under **Passwordless**; the sign-in page says **Sign in with Passkey**, **Select your passkey**, **Passkey Registration**, and asks for the label ("Passkey (Default Label)" is the pre-filled value) | message bundles of `keycloak-account-ui` and `keycloak-themes` in the 26.7.5 image |
| With the live hostname env the master console's `authUrl`, `authServerUrl`, `adminBaseUrl`, `serverBaseUrl` and the master OIDC `authorization_endpoint` are all `https://auth.clonk.tv/auth`; `KC_HOSTNAME_ADMIN` fixes only `authUrl` and `adminBaseUrl` | `console probe` lines of the run summary |

**Not covered here, on purpose.** The harness seeds credential *rows*; it does not run a real
WebAuthn ceremony (that is the theme harness, S1b, and the owner at a device, S3d). It does
not prove the live pod environment holds the master admin's current password: that is the
first line of the drill, and the [live drill record](#71-live-drill-record) is where that
proof lives (first run 2026-10-18: `authenticated: yes`). And a real browser was not driven
through a port-forwarded console.
