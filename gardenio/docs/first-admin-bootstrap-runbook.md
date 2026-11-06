# Single-EC2 first-admin bootstrap runbook

How the **first usable gardenio-realm admin** gets created when you stand up an
in-box Keycloak on a fresh single-EC2 box — and the **port-forward fallback** to
run by hand if the bootstrap hook is disabled or fails. **A missing bootstrap is
a hard lock-out**, so the fallback is mandatory reading, not an appendix.

This is the Path A install-time flow from the SSO research
(`docs/research/sso-identity-gardenio-in-a-box.md` §3 + §8), epic
[`ka-0000005376`](#5-cross-links), task `ka-0000005523`.

- **Before this**: stand up the in-box Keycloak itself — see
  [`inbox-keycloak.md`](inbox-keycloak.md) (task `ka-0000004234`). That doc
  covers Postgres wiring, TLS, the admin Secret, issuer-URL derivation, and the
  `ClusterKeycloak` ref. **Do that first**; this runbook picks up at the point
  where the StatefulSet is healthy and the realm is reconciled.
- **The bootstrap Job** that automates the happy path is task `ka-0000007117`
  (merged; MR 3910).

---

## 1. Why a fresh box locks itself out (the chicken-and-egg)

The lock-out is real and **must be solved before any public cutover**:

1. The Keycloak **master-realm** bootstrap user
   (`KEYCLOAK_ADMIN` / `KEYCLOAK_ADMIN_PASSWORD`, from Secret
   `gardenio-keycloak-runtime-admin`, `keycloak-runtime/statefulset.yaml`) is a
   superuser of the **master realm only**. It can administer the gardenio realm
   but is **not** a gardenio-realm OIDC user — it cannot log into beads.
2. The first human who logs in via OIDC is auto-provisioned a roleless
   `type=user` bead by `UserProvisioner.Middleware`
   (`controller/internal/server/user_provision.go`). The `gardenio-viewer` role
   does **not** come from that bead — it comes from the realm's
   `defaultGroups: [gardenio-viewers]` at token-generation time. So the first
   human lands **read-only**.
3. `write_authz.go` blocks every admin write (~12 types) and the realm ships
   `userRegistration: false` — there is **no self-service escape**. The box
   locks with no usable admin.

The fix is to mint a **gardenio-realm** user, joined to the `gardenio-admins`
group (which carries the `gardenio-admin` role, `keycloak/groups.yaml`), using the
master-realm superuser to bootstrap it — either automatically (§2) or by hand
(§3).

---

## 2. Happy path — the first-admin bootstrap Job

When the in-box Keycloak is on and `firstAdmin.enabled` is true (default for the
in-box profile), the chart renders two objects:

| Object | Template | Purpose |
|---|---|---|
| `<release>-keycloak-first-admin` Secret | `keycloak-runtime/first-admin-secret.yaml` | One-time installer username + 32-char random password, `helm.sh/resource-policy: keep` + `lookup`-stable across upgrades. |
| `<svc>-first-admin` Job | `keycloak-runtime/first-admin-job.yaml` | `post-install,post-upgrade` hook that creates the realm admin and joins the admins group. |

### Enable it

The bootstrap is gated by **three** switches, all of which must be true:

```yaml
keycloak:
  enabled: true
  runtime:
    enabled: true          # the in-box Keycloak itself (inbox-keycloak.md)
    firstAdmin:
      enabled: true        # default true for the in-box profile
      # username: ""       # empty -> derived as admin@<runtime.host>
      group: "gardenio-admins"
      passwordSecret:
        generate: true     # chart generates the one-time password Secret
```

With the in-box overlay layered on, a normal install/upgrade is all it takes:

```sh
helm upgrade --install gardenio gardenio/helm/gardenio \
  --namespace <ns> --create-namespace \
  --values deploy-values/<base-env>.yaml \
  --values deploy-values/gardenio-inbox.yaml
```

### What the Job does (idempotent)

The Job runs the Keycloak runtime image and drives `kcadm.sh` against the
in-cluster admin REST API
(`http://<svc>.<ns>.svc.cluster.local:8080/auth`). Because it is a Helm
`post-*` hook it is **not** gated on StatefulSet readiness, and the realm and its
groups are reconciled **asynchronously** by the EDP keycloak-operator, so the
Job polls before acting:

1. Authenticate to the **master** realm as the bootstrap superuser (retries to
   cover cold StatefulSet starts — up to 60 × 5s).
2. Wait for the target realm to exist.
3. Wait for the target group (`gardenio-admins`) and capture its id.
4. Create the realm user `admin@<runtime.host>` (`emailVerified=true`,
   `enabled=true`) **only if absent**, and set the one-time password **only on
   fresh creation**. On re-run it leaves the password untouched and just
   re-ensures `enabled`/`emailVerified`.
5. Join the user to `gardenio-admins` (a no-op if already a member).

Re-running on every `helm upgrade` is safe — it never clobbers a password the
admin may have rotated.

### Retrieve the one-time password

The Job log prints the exact command; it is:

```sh
kubectl -n <ns> get secret <release>-keycloak-first-admin \
  -o jsonpath='{.data.password}' | base64 -d; echo
```

The username is `admin@<runtime.host>` (or your `firstAdmin.username` override).
Log into beads over OIDC with those credentials, then **rotate the password**
and **promote real teammates** (move them from `gardenio-viewers` to
`gardenio-admins`/`gardenio-operators`) from the admin console or beads UI.

### Verify the Job succeeded

```sh
# Job completion
kubectl -n <ns> get job -l app.kubernetes.io/component=keycloak-runtime-first-admin
# Logs (look for: "[first-admin] DONE.")
kubectl -n <ns> logs job/<release>-keycloak-runtime-first-admin
```

> **Note on `helm template`.** The password helper uses `lookup`, which returns
> nil under `helm template` — so an **offline** `helm template | kubectl apply`
> would rotate the password on every apply. Use `helm install/upgrade` against a
> live cluster (the normal path) for a stable password.

---

## 3. Fallback — port-forward lock-out recovery (MANDATORY)

**If the bootstrap Job is disabled (`firstAdmin.enabled=false`), removed, or
exhausts its `backoffLimit`/`activeDeadlineSeconds`, the box is locked out with
no admin.** Recover by talking to the Keycloak admin console directly with the
master-realm superuser and creating the realm admin by hand. This is the same
work the Job automates — there is nothing special about it beyond having the
console open.

### When to use this

- The Job never ran (`firstAdmin.enabled: false`, or the in-box overlay omitted).
- The Job failed: `kubectl -n <ns> get job ... -o jsonpath='{.status.failed}'`
  is non-zero, or its logs end on a `FATAL` line (admin API never came up, realm
  never appeared, or group never appeared).
- You need to recover an admin after the one-time password was lost and no other
  admin exists to reset it.

### Step 1 — get the master-realm admin password

```sh
kubectl -n <ns> get secret gardenio-keycloak-runtime-admin \
  -o jsonpath='{.data.password}' | base64 -d; echo
# username is in the same Secret (default "admin"):
kubectl -n <ns> get secret gardenio-keycloak-runtime-admin \
  -o jsonpath='{.data.username}' | base64 -d; echo
```

> Locked out of the *gardenio realm* rather than the box (lost a passkey or authenticator,
> broken sign-in flow)? Use [`keycloak-passkeys-runbook.md`](keycloak-passkeys-runbook.md):
> it is `kcadm` in the pod, and its section 6 records why the browser console through a
> port-forward may not complete a login when `KC_HOSTNAME` is the public URL.

### Step 2 — port-forward the Keycloak console

```sh
kubectl -n <ns> port-forward svc/<release>-keycloak-runtime 8080:8080
# leave this running; open a second terminal / your browser
```

Then open `http://localhost:8080/auth/` and log in with the **master-realm**
admin from Step 1. (The console serves under the `/auth` relative path —
`httpRelativePath` — so the trailing `/auth/` matters.)

### Step 3 — create the realm admin by hand

In the console:

1. Switch the realm dropdown from **master** to the **gardenio realm**
   (`keycloak.realm.name`, defaults to the release name; prod owns `gardenio`).
2. **Users → Add user**:
   - Username/email: `admin@<runtime.host>` (the realm has `emailAsUsername=true`,
     so username == email).
   - **Email verified: ON**.
   - Enabled: ON.
3. On the new user's **Credentials** tab, set a password (turn **Temporary** off
   unless you want a forced reset on first login).
4. On the **Groups** tab, **Join** the `gardenio-admins` group (it carries the
   `gardenio-admin` realm role via `keycloak/groups.yaml`).

### Step 4 — verify and lock down

- Log into beads over OIDC with the new admin; confirm admin writes succeed.
- Stop the port-forward (`Ctrl-C`) — the console must not stay reachable.
- Promote teammates out of `gardenio-viewers` as needed.

### Optional — same thing without the browser (kcadm in-cluster)

If you prefer no UI, exec the runtime pod and drive `kcadm.sh` — this is exactly
what the Job does:

```sh
POD=$(kubectl -n <ns> get pod -l app.kubernetes.io/component=keycloak-runtime \
  -o jsonpath='{.items[0].metadata.name}')
ADMIN_PW=$(kubectl -n <ns> get secret gardenio-keycloak-runtime-admin \
  -o jsonpath='{.data.password}' | base64 -d)

kubectl -n <ns> exec -it "$POD" -- bash -ec '
  KCADM=/opt/keycloak/bin/kcadm.sh
  REALM=<realm>; USER=admin@<runtime.host>
  "$KCADM" config credentials --server http://localhost:8080/auth \
    --realm master --user admin --password "'"$ADMIN_PW"'"
  "$KCADM" create users -r "$REALM" -s username="$USER" -s email="$USER" \
    -s emailVerified=true -s enabled=true
  "$KCADM" set-password -r "$REALM" --username "$USER" --new-password "<choose>"
  GID=$("$KCADM" get groups -r "$REALM" -q search=gardenio-admins \
    --fields id,name --format csv --noquotes | awk -F, "\$2==\"gardenio-admins\"{print \$1;exit}")
  UID=$("$KCADM" get users -r "$REALM" -q username="$USER" \
    --fields id --format csv --noquotes | head -1)
  "$KCADM" update "users/$UID/groups/$GID" -r "$REALM" -n
'
```

---

## 4. Troubleshooting

| Symptom | Likely cause | Action |
|---|---|---|
| Job log: `FATAL: admin API never came up` | StatefulSet not ready / Service name wrong | Check `kubectl -n <ns> get pods -l app.kubernetes.io/component=keycloak-runtime`; confirm the `/auth/health/ready` probe is green; re-run `helm upgrade` (hook re-runs). |
| Job log: `FATAL: realm ... never appeared` | EDP operator hasn't reconciled the `KeycloakRealm` CR yet | Check the `KeycloakRealm`/`ClusterKeycloak` CR status; the operator may be down or the `clusterKeycloakRef` mismatched (see inbox-keycloak.md §7.5). |
| Job log: `FATAL: group gardenio-admins never appeared` | `KeycloakRealmGroup` not reconciled | Confirm `keycloak.groups[]` includes `gardenio-admins` and the operator reconciled it. |
| First login still read-only after Job success | Logged in as the master admin (not an OIDC user), or as a different OIDC user | Log in as `admin@<runtime.host>` with the one-time password; that user is the one joined to `gardenio-admins`. |
| One-time password rotates on every apply | Used `helm template \| kubectl apply` (offline) | Use `helm install/upgrade` against the live cluster so the `lookup` helper preserves the Secret. |
| Lost the one-time password, no other admin | — | Use the §3 fallback (master-realm console) to set a new password or create another admin. |

---

## 5. Cross-links

- **SSO research** (authoritative): `docs/research/sso-identity-gardenio-in-a-box.md`
  §3 (first-admin chicken-and-egg) + §8 (phased plan).
- **In-box Keycloak bring-up**: [`inbox-keycloak.md`](inbox-keycloak.md) — do
  this first. Task `ka-0000004234`.
- **Bootstrap Job implementation**: task `ka-0000007117` (MR 3910).
- **Path A epic**: `ka-0000005376`.
- **Box auth profile** (AVA off, GitLab-PAT off, cookieAuth on): research §6.
- **MFA hardening** (a mandatory second factor for `gardenio-admin`: a passkey on
  the live box since 2026-10-18; TOTP is retired):
  [`keycloak-mfa-hardening.md`](keycloak-mfa-hardening.md), research §5, epic
  `ka-0000001366`.
- **Passkey enrollment, recovery and break-glass**:
  [`keycloak-passkeys-runbook.md`](keycloak-passkeys-runbook.md).

Operator: matthew.baker@acme.example.
