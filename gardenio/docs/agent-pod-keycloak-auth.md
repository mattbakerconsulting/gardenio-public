# Agent pod auth: minting Keycloak tokens from inside a pod

Some agent-driven flows need to call protected gardenio services
(beads-viewer, etc.) from inside a pod that has no human session — for
example the info-density audit, or any agent that wants to dogfood the
viewer's HTTP API. Those services accept Keycloak-issued OIDC bearer
tokens; the question is how a non-interactive pod gets one.

This doc describes the supported path: a confidential Keycloak client
with a service account, and a small shell helper that exchanges the
client's K8s-Secret-resident `clientSecret` for an access token via the
OAuth2 `client_credentials` grant.

## Components

| Piece | Where |
|---|---|
| Confidential KeycloakClient `gardenio-info-density-audit` | `deploy-values/gardenio-rwx.yaml` and `deploy-values/gardenio.yaml` (under `keycloak.clients`) |
| Chart template that renders it as a `KeycloakClient` CRD | [`gardenio/helm/gardenio/templates/keycloak/clients.yaml`](../helm/gardenio/templates/keycloak/clients.yaml) |
| Operator-managed K8s Secret (`clientSecret` key) | `keycloak-client-<release>-client-gardenio-info-density-audit-secret` in the release namespace |
| Agent SA RBAC granting `get` on that Secret | `agents.agentServiceAccount.extraRules` in `deploy-values/gardenio-rwx.yaml` |
| Token helper | [`gardenio/tests/e2e/scripts/lib-keycloak-client-creds.sh`](../tests/e2e/scripts/lib-keycloak-client-creds.sh) |

## How it works

1. Helm renders the `gardenio-info-density-audit` entry under
   `keycloak.clients` into a `KeycloakClient` CRD via
   `templates/keycloak/clients.yaml`. The CRD name is
   `<release>-client-gardenio-info-density-audit`.
2. The EDP keycloak-operator reconciles the CRD into a real Keycloak
   client on the `gardenio` realm and writes an `Opaque` K8s Secret
   named `keycloak-client-<KeycloakClient name>-secret` with a single
   `clientSecret` data key holding the generated client secret.
3. The agent ServiceAccount has a narrow `get` rule on that specific
   secret name (no wildcard, no other secrets).
4. From an agent pod the helper reads the secret and POSTs a
   `grant_type=client_credentials` request to the realm's token
   endpoint. Keycloak returns a JWT whose `aud` includes
   `gardenio-beads-viewer` (via the `beads-viewer-audience` protocol
   mapper on the client) and whose `realm_roles` includes
   `gardenio-viewer` (via the client's `serviceAccount.realmRoles`).
5. The token is presented as `Authorization: Bearer <jwt>` to any
   service whose OIDC middleware accepts `aud=gardenio-beads-viewer` —
   primarily beads-viewer itself.

## Usage from a script

```bash
# shellcheck disable=SC1090
source "$(dirname "$0")/lib-keycloak-client-creds.sh"

token=$(mint_client_credentials_token) || exit 1

curl -sS -H "Authorization: Bearer ${token}" \
  https://beads-viewer.gardenio-rwx.e2e.example.com/api/whoami
```

Override env before sourcing if you need a different realm / client /
secret reference:

| Env | Default |
|---|---|
| `KC_BASE_URL` | `https://auth.e2e.dev.acme.example` |
| `KC_REALM` | `gardenio` |
| `KC_CLIENT_ID` | `gardenio-info-density-audit` |
| `KC_CLIENT_SECRET` | *(unset — read from K8s Secret)* |
| `KC_CLIENT_SECRET_NS` | `$POD_NAMESPACE`, else current kubectl ctx |
| `KC_CLIENT_SECRET_NAME` | derived: `keycloak-client-<release>-client-<KC_CLIENT_ID>-secret` |
| `KC_CLIENT_SECRET_KEY` | `clientSecret` |
| `KC_RELEASE_NAME` | `$RELEASE_NAME`, else `$POD_NAMESPACE` |

For local development outside the cluster, set `KC_CLIENT_SECRET=...`
directly (e.g. from a Vault entry) so the helper skips kubectl.

## Verifying after a deploy

```bash
# 1. The KeycloakClient CRD reconciled cleanly:
kubectl -n gardenio-rwx get keycloakclient \
  gardenio-rwx-client-gardenio-info-density-audit -o yaml \
  | yq '.status'
# Expect failureCount: 0, value: "OK".

# 2. The operator wrote the secret:
kubectl -n gardenio-rwx get secret \
  keycloak-client-gardenio-rwx-client-gardenio-info-density-audit-secret

# 3. The agent SA can read it:
kubectl -n gardenio-rwx auth can-i get \
  secret/keycloak-client-gardenio-rwx-client-gardenio-info-density-audit-secret \
  --as=system:serviceaccount:gardenio-rwx:gardenio-rwx-agent
# Expect: yes

# 4. From inside an agent pod (gardenio-rwx namespace), the helper mints a token:
source gardenio/tests/e2e/scripts/lib-keycloak-client-creds.sh
mint_client_credentials_token | head -c 40 && echo …

# 5. The token is accepted by beads-viewer:
token=$(mint_client_credentials_token)
curl -sS -H "Authorization: Bearer ${token}" \
  https://beads-viewer.gardenio-rwx.e2e.example.com/api/whoami
```

## Why this is separate from `gardenio-beads-viewer-test`

`gardenio-beads-viewer-test` (also in `deploy-values/gardenio-rwx.yaml`)
was the **password-grant** sibling of the locked-down
`gardenio-beads-viewer` client, historically used by E2E scripts to
impersonate a specific user via `mint_user_token`. As of T6b
(ka-0000001463) the E2E `mint_user_token` moved to
[`lib-gitlab-token.sh`](../tests/e2e/scripts/lib-gitlab-token.sh) and
resolves a **GitLab-direct** transport bearer instead of a Keycloak
password grant — the human identity now travels out-of-band as a
GitLab-verified email header, not baked into the token.

`gardenio-info-density-audit` is the **client_credentials** equivalent:
confidential, no user impersonation, no password. It is the right
choice for any pod-driven flow that just needs "a token that
beads-viewer will accept" without representing a particular human.

## See also

- [`machine-principal-design.md` §12](machine-principal-design.md#12-as-built) — the service-to-service counterpart: platform components (controller, bridges, viewer, trigger-engine, coopmux) authenticate as `machine:<clientId>` Keycloak service accounts with enumerated grants. Agent pods are not machine principals; they stay on the shared static token until P7 per-agent tokens, which is why most `machineAllowedWrites` entries remain.
- [`inpod-beads-viewer-fetch.md`](inpod-beads-viewer-fetch.md) — one-command self-service screenshot/DOM of the **prod** beads-viewer using this helper (`fetch-beads-viewer-prod.sh`, task ka-0000002064).
- [`lib-gitlab-token.sh`](../tests/e2e/scripts/lib-gitlab-token.sh) — E2E transport-bearer helper (`mint_user_token` / `mint_client_credentials_token`, GitLab-direct); replaced the retired `lib-keycloak-token.sh` password-grant helper in T6b (ka-0000001463).
- [`docs/research/keycloak-in-cluster-from-ecs.md`](research/keycloak-in-cluster-from-ecs.md) — earlier design discussion of client-credentials grants from non-human workloads.
- ka-0000003037 — plan-of-record for the info-density audit auth track.
- ka-0000008133 — Track B epic.
- ka-0000000125 — agent SA RBAC pattern for KeycloakClient CRDs.
