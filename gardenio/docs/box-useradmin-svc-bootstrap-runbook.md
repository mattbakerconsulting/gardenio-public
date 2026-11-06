# Box user-admin service-client bootstrap — Operator Runbook

> **Task:** [ka-0000001926](https://gardenio.e2e.example.com/beads/ka-0000001926)
> (operator, one-time SSH) ·
> **Epic:** [ka-0000001297](https://gardenio.e2e.example.com/beads/ka-0000001297)
> — self-serve box-admin access path for agents (Keycloak user-mgmt) ·
> **Design + validation (the WHY behind every value here):**
> [ka-0000005452](https://gardenio.e2e.example.com/beads/ka-0000005452) ·
> **Operator:** Matt Baker (slack:U0OPERATOR1)

This is the **one-time operator action** that unblocks agent self-serve user
management on a gardenio-in-a-box (GiB) instance. You SSH to the box once, create a
least-privilege Keycloak **service-account client** on the `gardenio-inbox` realm,
prove its token carries the right roles, and smoke-test the create-user →
set-password flow. After this, agents add/manage users entirely over the public
Keycloak REST — **no SSH, no master admin password, no per-user operator touch**.

> **Scope boundary — what this runbook does NOT do.** It creates the client and
> reads out its secret. **Provisioning that secret to agent pods** (AWS Secrets
> Manager → ExternalSecrets → k8s Secret) + the agent-side helper is the sibling
> task [ka-0000001845](https://gardenio.e2e.example.com/beads/ka-0000001845),
> §7 below hands off to it. This runbook ends when you have a working client
> secret in hand and the §5 smoke test is green.

> **What box is this?** Gardenio-in-a-Box (GiB) is a *portable product* — the whole
> platform on **one EC2 instance** with in-box k3s and **zero external cloud
> deps**. It is **NOT** prod and **NOT** a ACME cluster (see
> [`gasbox-vs-aws-stacks.md`](gasbox-vs-aws-stacks.md)). Everything here runs
> against the box's **in-box** Keycloak, never against `prod-gardenio-eks`.

---

## 0. The target box

The GiB instance lives in the **AWS-poc account (`123456789012`)** and is driven
by an operator **over SSH** — the in-cluster fleet's AWS role is in the mgmt
account and cannot reach it, by design. Known live box from prior work:

| Box | EIP | Realm | Auth host | State |
|---|---|---|---|---|
| **gardenio-in-a-box** | `192.0.2.91` | `gardenio-inbox` | `https://auth.192-0-2-91.sslip.io/auth` | live, 443 reachable |

> Confirm the live box is still the one you want before wiring — if you spun up a
> fresh box, substitute its EIP (and therefore its `<dashed-ip>.sslip.io` auth
> host) below. Set these once and reuse them:
>
> ```sh
> EIP=192.0.2.91
> NS=gardenio                                  # k8s namespace / helm release name
> REALM=gardenio-inbox
> AUTH=https://auth.192-0-2-91.sslip.io/auth   # public auth host (dashed EIP)
> CLIENT=gardenio-box-useradmin-svc
> ```

---

## 1. One-time SSH access to the box

The box exposes **only 80/443/22** on its public NIC; the k3s apiserver (`:6443`)
is **loopback-only** (firewall-dropped on the public interface). So all admin work
is done *on the box* over SSH, or through an SSH tunnel to loopback.

```sh
# SSH in. The box has kubectl shimmed to `k3s kubectl` and the gardenio repo
# checked out (typically /opt/gardenio/src).
ssh gardenio-op@$EIP
```

> **Access prerequisites.** You need the `gardenio-op` SSH key for this instance
> (held by the operator who provisioned the box — it is not in the agent fleet's
> AWS account, and there is no agent path to it; that is the whole reason this is
> an operator-gated one-time action). If you do not have it, you cannot proceed —
> this step is exactly why the epic exists.

**On the box**, k3s ships its own kubectl; the kubeconfig is root-owned, so most
commands need `sudo`:

```sh
# on the box — sanity-check the stack is up and Keycloak is running:
sudo k3s kubectl -n $NS get pods | grep -i keycloak     # keycloak-runtime pod Running
sudo k3s kubectl -n $NS get svc  | grep -i keycloak     # ClusterIP svc present
```

> If the box's shim already exports `KUBECONFIG=/etc/rancher/k3s/k3s.yaml` for
> your shell you can drop `sudo k3s` and just use `kubectl`. The commands below
> use `sudo k3s kubectl` to be safe on a bare login.

**Operator kubectl from a laptop (optional)** — only over an SSH tunnel to
loopback; never copy `k3s.yaml` and repoint it at the public IP (`:6443` is
firewall-dropped):

```sh
ssh -L 6443:127.0.0.1:6443 gardenio-op@$EIP
# then locally, with a kubeconfig whose server is https://127.0.0.1:6443:
kubectl get nodes
```

---

## 2. Create the service-account client (the core action)

We create **one confidential service-account client** on `gardenio-inbox`, granted
the **minimal** realm-management roles. The full authorization trace (read from
the box's exact Keycloak 26.0.7 source) is in the design doc
[ka-0000005452](https://gardenio.e2e.example.com/beads/ka-0000005452) §3; the
values below are its validated output.

| Setting | Value | Why |
|---|---|---|
| `clientId` | `gardenio-box-useradmin-svc` | the agent user-admin identity |
| `publicClient` | `false` | confidential — has a secret |
| `serviceAccountsEnabled` | `true` | enables `client_credentials` grant |
| `standardFlowEnabled` | `false` | no browser login |
| `directAccessGrantsEnabled` | `false` | no password grant |
| `implicitFlowEnabled` | `false` | — |
| `fullScopeAllowed` | **`false`** | least-priv; **see §3 footgun** |
| SA realm-management roles | **`manage-users` + `view-users` ONLY** | everything user-mgmt needs, nothing more |

**Do NOT grant** `manage-realm`, `realm-admin`, `impersonation`,
`manage-clients`, or `admin`. `manage-users` alone authorizes the entire
create-user → set-password → join-group workflow; `view-users` is added for
explicit read intent + headroom (it composites `query-users` + `query-groups` for
free). Anything above `{manage-users, view-users}` is over-grant.

### 2a. Get the master-realm admin credentials (on the box)

The client is created *as the master-realm runtime admin*, whose credentials live
in an in-box k8s Secret:

```sh
ADMIN_USER=$(sudo k3s kubectl -n $NS get secret gardenio-keycloak-runtime-admin \
  -o jsonpath='{.data.username}' | base64 -d)
ADMIN_PW=$(sudo k3s kubectl -n $NS get secret gardenio-keycloak-runtime-admin \
  -o jsonpath='{.data.password}' | base64 -d)
```

> This master password stays on the box — it is **never** handed to agents. That
> is the entire point of minting a scoped service client instead.

### 2b. Create the client + assign roles (kcadm inside the Keycloak pod)

Run `kcadm` inside the running keycloak-runtime pod, which already ships it and can
reach the admin API on loopback `:8080/auth`:

```sh
POD=$(sudo k3s kubectl -n $NS get pod \
  -l app.kubernetes.io/component=keycloak-runtime \
  -o jsonpath='{.items[0].metadata.name}')

sudo k3s kubectl -n $NS exec -i "$POD" -- bash -ec '
  KCADM=/opt/keycloak/bin/kcadm.sh
  REALM='"$REALM"'
  CLIENT='"$CLIENT"'

  "$KCADM" config credentials --server http://localhost:8080/auth \
    --realm master --user "'"$ADMIN_USER"'" --password "'"$ADMIN_PW"'"

  # 1) create the confidential service-account client (least-priv flags).
  #    -i makes create print the new internal client id (CID).
  CID=$("$KCADM" create clients -r "$REALM" -i \
    -s clientId="$CLIENT" \
    -s protocol=openid-connect \
    -s publicClient=false \
    -s serviceAccountsEnabled=true \
    -s standardFlowEnabled=false \
    -s directAccessGrantsEnabled=false \
    -s implicitFlowEnabled=false \
    -s fullScopeAllowed=false)
  echo "created client internal id: $CID"

  # 2) grant ONLY manage-users + view-users from the realm-management client
  #    to the service account.
  "$KCADM" add-roles -r "$REALM" \
    --uusername "service-account-'"$CLIENT"'" \
    --cclientid realm-management \
    --rolename manage-users --rolename view-users

  # 3) read the client secret (hand to secret-provisioning, ka-0000001845).
  echo -n "CLIENT_SECRET="
  "$KCADM" get "clients/$CID/client-secret" -r "$REALM" --fields value
'
```

> **Idempotency / re-runs.** If the client already exists, step 1's `create`
> fails with a 409. That is fine — skip to `add-roles` (it is idempotent) and read
> the existing secret with `kcadm get clients -r $REALM -q clientId=$CLIENT` to
> find the CID, then `kcadm get clients/$CID/client-secret`. To rotate the secret
> instead, `kcadm create clients/$CID/client-secret` mints a fresh one.

**Capture the printed `CLIENT_SECRET=` value** — that string is the only output of
this runbook that leaves the box, and it goes straight into §7 (secret
provisioning). Treat it as a credential.

### 2c. Verify realm group state (do not assume)

The chart's `first-admin-job` defaults to creating a `gardenio-admins` group and
joining the first admin, so groups likely **do** exist. This does not change the
role set (`manage-users` covers group-join regardless), but confirm which
group(s) new users should join and record the group id(s) for ka-0000001845:

```sh
sudo k3s kubectl -n $NS exec -i "$POD" -- \
  /opt/keycloak/bin/kcadm.sh get groups -r $REALM --fields id,name
```

---

## 3. The `fullScopeAllowed` footgun — the token-scope smoke gate (MUST run)

**This is the #1 thing that silently breaks this pattern.** A `client_credentials`
token only carries the service account's realm-management roles in its
`resource_access["realm-management"].roles` claim **if those roles are in the
client's token scope**. With `fullScopeAllowed=false` (which we set, for
least-privilege), you must confirm the roles actually reach the token — otherwise
every REST call 401/403s even though the role assignment *looks* correct in the
admin console.

Mint a token via `client_credentials` and decode its claims:

```sh
CLIENT_SECRET=<paste the secret from §2b>

TOKEN=$(curl -sS --fail-with-body \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  --data-urlencode grant_type=client_credentials \
  --data-urlencode "client_id=$CLIENT" \
  --data-urlencode "client_secret=$CLIENT_SECRET" \
  "$AUTH/realms/$REALM/protocol/openid-connect/token" | jq -r .access_token)

# decode the JWT payload and print the realm-management roles it carries:
echo "$TOKEN" | jq -R 'split(".")[1] | @base64d | fromjson
  | .resource_access["realm-management"].roles'
```

**PASS gate:** the output MUST list `manage-users` (and `view-users`):

```json
[ "view-users", "manage-users" ]
```

If it is `null` / empty / missing, the token scope is wrong. **Fix:** ensure the
default **`roles`** client scope is on the client (its "client roles" mapper is
what populates `resource_access.*.roles`) — it is a default client scope, so it is
normally present; only a client that had it removed needs it re-added, or the two
roles added explicitly on the client **Scope** tab. Re-run this gate until it is
green **before** proceeding — a green §5 depends entirely on a green §3.

> You can run this from the box, from a laptop, or from a prod agent pod — the
> auth host is public. It needs only `curl` + `jq`.

---

## 4. (Reference) how the box Keycloak surface was validated

For confidence that the mechanism is sound before you create anything, these
probes (from the design doc, re-runnable against the live box) prove the realm
will actually issue a service-account token and that the admin surface is live and
correctly gated:

```sh
# realm supports the client_credentials grant the whole design rests on:
curl -sS "$AUTH/realms/$REALM/.well-known/openid-configuration" \
  | jq -r '.grant_types_supported | index("client_credentials")'   # -> a number (present)

# admin users REST correctly rejects an unauthenticated caller:
curl -sS -o /dev/null -w '%{http_code}\n' "$AUTH/admin/realms/$REALM/users"  # -> 401
```

---

## 5. Smoke-test agent self-serve add-user (the acceptance gate)

This is the **exact flow an agent will run** once the secret is provisioned — run
it here, by hand, to prove end-to-end that the client works. Reuse `$TOKEN` from
§3 (re-mint if it expired).

```sh
# --- create a throwaway test user ---
TESTUSER=smoketest-$(date +%s)@acme.example
curl -sS -o /dev/null -w 'create user -> %{http_code}\n' \
  -X POST "$AUTH/admin/realms/$REALM/users" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d "{\"username\":\"$TESTUSER\",\"enabled\":true,\"email\":\"$TESTUSER\",\"emailVerified\":true}"
#   expect: 201

# --- look up its id ---
UID=$(curl -sS "$AUTH/admin/realms/$REALM/users?username=$TESTUSER&exact=true" \
  -H "Authorization: Bearer $TOKEN" | jq -r '.[0].id')
echo "user id: $UID"

# --- set a temporary password ---
curl -sS -o /dev/null -w 'set password -> %{http_code}\n' \
  -X PUT "$AUTH/admin/realms/$REALM/users/$UID/reset-password" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"type":"password","value":"Temp-'"$(date +%s)"'!","temporary":true}'
#   expect: 204

# --- (optional) join a group, e.g. gardenio-admins ---
GID=$(curl -sS "$AUTH/admin/realms/$REALM/groups?search=gardenio-admins" \
  -H "Authorization: Bearer $TOKEN" | jq -r '.[0].id')
curl -sS -o /dev/null -w 'join group -> %{http_code}\n' \
  -X PUT "$AUTH/admin/realms/$REALM/users/$UID/groups/$GID" \
  -H "Authorization: Bearer $TOKEN"
#   expect: 204

# --- clean up the throwaway test user ---
curl -sS -o /dev/null -w 'delete user -> %{http_code}\n' \
  -X DELETE "$AUTH/admin/realms/$REALM/users/$UID" \
  -H "Authorization: Bearer $TOKEN"
#   expect: 204
```

**PASS gate:** `create user -> 201`, `set password -> 204`, `join group -> 204`,
`delete user -> 204`. All green means an agent holding this client's secret can
self-serve the full create-user → set-password → join-group workflow with **zero**
further operator action. (Delete the test user so the realm is left clean.)

> **Testing via the box UI instead of curl.** You can also drive the same checks
> through the browser: open `https://beads-viewer.<dashed-ip>.sslip.io` (or the
> Keycloak admin console at `$AUTH/admin/`) and confirm the smoke-created user
> appears under **Users** in the `gardenio-inbox` realm, then delete it. The curl
> path above is the authoritative gate because it exercises the *service-account
> token* an agent uses — the UI uses your interactive admin session, which proves
> less.

---

## 6. Rotation, revocation, audit

- **Rotate:** `kcadm create clients/$CID/client-secret` (or console → *Regenerate
  Secret*) mints a new secret; then re-run ka-0000001845's SM write + ESO refresh.
  No master cred, no SSH beyond this one-time creation.
- **Revoke:** `kcadm update clients/$CID -r $REALM -s enabled=false` (or delete the
  client) — every agent token stops working immediately (tokens are short-lived;
  the SA can mint no more).
- **Audit:** enable realm admin events on `gardenio-inbox`; every REST write is
  attributed to `service-account-gardenio-box-useradmin-svc`, so "who added this
  user" resolves to the svc client (and, via the agent that held the secret, to
  the agent bead / operator).
- **Blast radius:** `{manage-users, view-users}` + `fullScopeAllowed=false` confine
  it to user CRUD + group membership on one realm. No master, no manage-realm, no
  impersonation.

---

## 7. Hand-off — provisioning the secret to agents (ka-0000001845)

You now hold a working `CLIENT_SECRET`. The **sibling operator task**
[ka-0000001845](https://gardenio.e2e.example.com/beads/ka-0000001845) provisions
it to agent pods and lands the agent-side helper. The canonical wiring (per that
task's implementation note, MR !7629):

1. **Write the secret to AWS Secrets Manager** under **`gardenio/prod/box-useradmin-svc-client-secret`**.
   > **Prefix matters:** the canonical-prod ESO `ClusterSecretStore` IRSA is
   > `GetSecretValue`-scoped to **`gardenio/prod/*`** — a `gardenio/box/*` key
   > `AccessDenied`s and ESO sits in `SecretSyncedError`. The prefix is dictated
   > by *which IRSA reads it*, not by what the credential is for.
2. **ExternalSecret** syncs it into the agent namespace as a k8s Secret
   (`gardenio-box-useradmin-svc`, key `clientSecret`).
3. **Agent-side helper** is a thin wrapper over the proven
   `gardenio/tests/e2e/scripts/lib-keycloak-client-creds.sh` — override
   `KC_BASE_URL=$AUTH`, `KC_REALM=gardenio-inbox`, `KC_CLIENT_ID=$CLIENT`, and read
   the secret from the mounted k8s Secret. The agent then mints a token and drives
   the §5 REST flow — the same calls you just smoke-tested, now self-serve.

Once ka-0000001845 lands, the epic
[ka-0000001297](https://gardenio.e2e.example.com/beads/ka-0000001297) goal is met:
agents add/manage box users self-serve, least-privilege, rotatable, SSH-free.
