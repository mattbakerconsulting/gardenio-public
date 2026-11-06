# Box user-management service-account client secret — operator runbook

**Task:** ka-0000001845 — provision the box user-mgmt client secret to the prod
agent namespace + land the agent-side REST helper.
**Epic:** ka-0000001297 — self-serve box-admin access path for agents.
**Design (authoritative):** ka-0000005452 — least-priv Keycloak service-account
client for box user-mgmt.
**Sibling (creates the client on the box):** ka-0000001926.

## What this is

The confidential Keycloak **service-account client**
`gardenio-box-useradmin-svc` on the gardenio-in-a-box (GiB) `gardenio-inbox` realm
lets an agent pod self-serve user-management on the box
(https://auth.192-0-2-91.sslip.io/auth) — create users, set temporary
passwords, join groups — via the admin REST API, using a
`client_credentials` grant. No master admin password, no SSH.

This runbook covers **provisioning the client's secret** into the prod agent
namespace so an agent can read it, and **verifying** an agent can mint a token
with it. Creating the client itself (and assigning `{manage-users, view-users}`,
`fullScopeAllowed=false`) is the sibling task ka-0000001926 — do that first.

## Why AWS SM → ESO, not a KeycloakClient CRD

The in-cluster `gardenio-info-density-audit` client is reconciled by the EDP
keycloak-operator from a `KeycloakClient` CRD, which auto-generates its secret
into a K8s Secret (see [`agent-pod-keycloak-auth.md`](../agent-pod-keycloak-auth.md)).

The **box** Keycloak is a *separate server* on the box that the in-cluster
operator cannot reconcile into. So `gardenio-box-useradmin-svc` has **no CRD
path**: its secret must flow

```text
box Keycloak (secret generated on the box)
   │  operator copies value once
   ▼
AWS Secrets Manager   gardenio/prod/box-useradmin-svc-client-secret   (plain string)
   │  ExternalSecret (externalSecrets.boxUserAdminSvcSecret.*)
   ▼
K8s Secret   gardenio-box-useradmin-svc   key: clientSecret   (release namespace)
   │  agent SA `get` (agents.agentServiceAccount.extraRules)
   ▼
agent pod   →  lib-keycloak-box-useradmin.sh  →  client_credentials token  →  box admin REST
```

## ⚠️ SM key prefix: use `gardenio/prod/*`, NOT `gardenio/box/*`

Design ka-0000005452 §9 suggested `gardenio/box/useradmin-svc-client-secret`.
**Do not use that.** The canonical-prod ESO ClusterSecretStore IRSA is
`secretsmanager:GetSecretValue`-scoped to the **`gardenio/prod/*`** prefix (see
[`canonical-prod-secrets-iam-leastpriv-audit.md`](../canonical-prod-secrets-iam-leastpriv-audit.md)).
A `gardenio/box/*` key is `AccessDenied` and the ExternalSecret would sit in
`SecretSyncedError` forever.

Verified live from a prod agent pod (2026-08-21):

```
$ aws secretsmanager describe-secret --secret-id gardenio/box/useradmin-svc-client-secret
AccessDeniedException … not authorized to perform: secretsmanager:DescribeSecret
$ aws secretsmanager list-secrets --query 'SecretList[?starts_with(Name,`gardenio/prod/`)]'
… (readable) …
```

So the canonical SM key is **`gardenio/prod/box-useradmin-svc-client-secret`**.
The prefix is dictated by which IRSA reads it, not by what the credential is for.

## Components

| Piece | Where |
|---|---|
| SM secret (plain-string value) | `gardenio/prod/box-useradmin-svc-client-secret` (operator-seeded) |
| ExternalSecret template | [`templates/external-secrets.yaml`](../../helm/gardenio/templates/external-secrets.yaml) (`boxUserAdminSvcSecret` block) |
| Values | `externalSecrets.boxUserAdminSvcSecret.*` in [`values.yaml`](../../helm/gardenio/values.yaml) |
| Target K8s Secret | `gardenio-box-useradmin-svc` (key `clientSecret`), release namespace |
| Agent SA RBAC (`get` on that Secret) | `agents.agentServiceAccount.extraRules` in the deploy overlay |
| Agent-side helper | [`gardenio/tests/e2e/scripts/lib-keycloak-box-useradmin.sh`](../../tests/e2e/scripts/lib-keycloak-box-useradmin.sh) |

## Provisioning steps

Test in **gardenio-rwx** first, then repeat for prod.

### 1. Seed the SM value (operator, once)

Take the client secret produced by the sibling task ka-0000001926
(`kcadm get clients/$CID/client-secret -r gardenio-inbox --fields value`), then:

```bash
aws secretsmanager create-secret \
  --name gardenio/prod/box-useradmin-svc-client-secret \
  --secret-string '<the-client-secret-value>'
# (or `put-secret-value --secret-id … --secret-string …` to rotate)
```

The ESO provider role can **read** but not create SM entries, so this is an
operator step. Store the raw secret as a **plain string** (not JSON) — the
ExternalSecret maps the whole value to the `clientSecret` key.

### 2. Enable the ExternalSecret in the deploy overlay

In the deploy-values overlay (`deploy-values/gardenio-rwx-prod.yaml`, then the
prod overlay), under `externalSecrets:`:

```yaml
externalSecrets:
  enabled: true
  secretStoreName: aws-secrets-manager   # canonical-prod store name
  boxUserAdminSvcSecret:
    remoteRef: "gardenio/prod/box-useradmin-svc-client-secret"
    # secretName defaults to gardenio-box-useradmin-svc; key is always clientSecret
```

### 3. Grant the agent SA `get` on the target Secret

Add to `agents.agentServiceAccount.extraRules` in the same overlay (mirrors the
info-density precedent):

```yaml
agents:
  agentServiceAccount:
    extraRules:
      - apiGroups: [""]
        resources: ["secrets"]
        resourceNames:
          - "gardenio-box-useradmin-svc"
        verbs: ["get", "list"]
```

Deploy the chart.

## Verifying after a deploy

Run in **gardenio-rwx** first (substitute your namespace/release):

```bash
NS=gardenio-rwx

# 1. ExternalSecret synced cleanly:
kubectl -n "$NS" get externalsecret gardenio-box-useradmin-svc \
  -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}{"\n"}'
# Expect: True  (SecretSyncedError → wrong prefix/missing SM value/IRSA scope)

# 2. Target Secret materialized with the clientSecret key:
kubectl -n "$NS" get secret gardenio-box-useradmin-svc \
  -o jsonpath='{.data.clientSecret}' | wc -c
# Expect: non-zero

# 3. Agent SA can read it:
kubectl -n "$NS" auth can-i get secret/gardenio-box-useradmin-svc \
  --as=system:serviceaccount:"$NS":"$NS"-agent
# Expect: yes

# 4. From inside an agent pod, mint a token and prove the roles are in scope:
source gardenio/tests/e2e/scripts/lib-keycloak-box-useradmin.sh
token=$(mint_box_useradmin_token) && echo "token len: ${#token}"
# §4 token-scope gate (design ka-0000005452): manage-users MUST appear.
printf '%s' "$token" | cut -d. -f2 | tr '_-' '/+' | base64 -d 2>/dev/null \
  | jq '.resource_access["realm-management"].roles'
# Expect: ["manage-users","view-users"]  — empty ⇒ fullScopeAllowed footgun (§4)

# 5. End-to-end user-mgmt smoke (creates a throwaway user):
box_create_user smoke-$RANDOM smoke@acme.example
```

## Rotation

1. Regenerate the secret on the box (sibling task path):
   `kcadm create clients/<id>/client-secret -r gardenio-inbox`, then read it back.
2. Rewrite the SM value:
   `aws secretsmanager put-secret-value --secret-id gardenio/prod/box-useradmin-svc-client-secret --secret-string '<new>'`.
3. ESO re-syncs on its `oauth`-category refreshInterval (1h) — no redeploy. Force
   immediate: `kubectl -n <ns> annotate externalsecret gardenio-box-useradmin-svc force-sync=$(date +%s) --overwrite`.

## Revocation

Disable or delete the client on the box
(`kcadm update clients/<id> -r gardenio-inbox -s enabled=false`). Agent tokens are
short-lived; the service account can mint no more. Optionally also delete the SM
value so nothing re-syncs.

## See also

- Design: ka-0000005452 (role model, `fullScopeAllowed` footgun, kcadm bootstrap).
- Companion pattern (CRD-backed, in-cluster): [`agent-pod-keycloak-auth.md`](../agent-pod-keycloak-auth.md).
- Base token primitive: [`lib-keycloak-client-creds.sh`](../../tests/e2e/scripts/lib-keycloak-client-creds.sh).
