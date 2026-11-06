# mcp-grafana scoped Grafana token — operator runbook

**Task:** ka-0000002328 (Track A A2 — mint scoped Grafana SA token + Secret)
**Epic:** ka-0000005390 — mcp-grafana → gardenio agents (Cloud-free Assistant powers)
**Decision:** ka-0000007279 → go-read-only (operator: Matt Baker)
**RBAC spike:** ka-0000002166 (NB2) — empirical per-tool scope, summarized below.

## What this is

The scoped **Grafana service-account token** that the standalone
[`mcp-grafana`](https://github.com/grafana/mcp-grafana) server (A1, ka-0000004361)
uses to authenticate against the **in-cluster gardenio Grafana** HTTP API.

The in-cluster Grafana runs `GF_AUTH_PROXY` with the basic login form disabled
(`disableLoginForm: true`) and anonymous access off, so any unauthenticated API
call 302-loops to `/login`. mcp-grafana therefore needs a Bearer
service-account token, injected into its Deployment as
`GRAFANA_SERVICE_ACCOUNT_TOKEN`.

This is **distinct** from the env-side `grafana-api-key` bundle
(`docs/runbooks/grafana-env-token-bundle.md`), which carries **Editor** tokens
for the **ACME environment Grafana** (`lens.acme.net`) and is injected into
*agent pods*. This token is **Viewer-scoped**, targets the **in-cluster**
gardenio Grafana, and lives only in the one mcp-grafana pod — so it never
reaches an agent's `.mcp.json`.

## Access level: Viewer, not Editor or Admin

Per decision ka-0000007279 the first slice is **read-only**, and per the
"no long-lived admin creds" rule the token must be minimally scoped. **Viewer**
is the correct floor:

- OSS Grafana 11.4 ships only the basic org roles (Viewer / Editor / Admin);
  fine-grained RBAC is Grafana Enterprise, so Viewer is the tightest role
  available.
- Empirically (NB2 ka-0000002166, Grafana 11.4.0), a Viewer token returns
  **200** for every read tool mcp-grafana enables in read-only mode and **403**
  on writes — defense-in-depth on top of the server's own `--disable-write`:

  | mcp-grafana tool        | Grafana API endpoint                              | Viewer |
  |-------------------------|---------------------------------------------------|:------:|
  | `list_datasources`      | `GET /api/datasources`                            | 200    |
  | `get_datasource_by_uid` | `GET /api/datasources/uid/<uid>`                  | 200    |
  | datasource health       | `GET /api/datasources/uid/<uid>/health`           | 200    |
  | `search_dashboards`     | `GET /api/search?type=dash-db`                    | 200    |
  | `get_dashboard_by_uid`  | `GET /api/dashboards/uid/<uid>`                   | 200    |
  | `query_prometheus`      | ds-proxy `/api/v1/query` + `POST /api/ds/query`   | 200    |
  | traces (jaeger)         | ds-proxy `/api/services`                          | 200    |
  | `get_current_org`       | `GET /api/org`                                    | 200    |
  | (write) create folder   | `POST /api/folders`                               | **403**|
  | org/team mgmt           | `GET /api/org/users`, `/api/teams/search`         | 403\*  |

  \* not in the A3 read-only allowlist, so the 403 is irrelevant.

Do **not** mint Editor/Admin for this token. Writes are a separate,
operator-gated decision (A4/A5 are deferred; enabling writes needs a new go).

## How it flows

```text
Grafana SA  "gardenio-mcp-grafana" (Viewer)
   └─ token (glsa_…)
        │  provisioned one of two ways:
        │
   (1) break-glass mint ──────────────▶ K8s Secret  grafana-mcp-token  ──▶ mcp-grafana pod
        kubectl create secret              key: token                       env GRAFANA_SERVICE_ACCOUNT_TOKEN
                                                  ▲
   (2) AWS Secrets Manager ── ExternalSecret ─────┘
        gardenio/mcp-grafana/grafana-sa-token   (mcpGrafana.token.externalSecret.enabled=true)
        property: token
```

- Token Secret + ExternalSecret config: `mcpGrafana.token.*` in `values.yaml`.
- ExternalSecret template: `gardenio/helm/gardenio/templates/mcp-grafana-token-external-secret.yaml`.
- The A1 Deployment (ka-0000004361) consumes the Secret as
  `GRAFANA_SERVICE_ACCOUNT_TOKEN` via `secretKeyRef` (same shape as the
  grafana-bridge `GRAFANA_TOKEN`).

## Provisioning path 1 — break-glass mint (read-only first slice)

The admin login form is disabled at the edge, but the in-cluster Grafana still
accepts the break-glass admin basic-auth on its API directly via the ClusterIP
Service (Traefik/AVA are bypassed pod-to-pod). Use that to mint the SA + token
once.

> **Service name** is `<release>-grafana`, not always `gardenio-grafana`. In
> prod the Helm release IS `gardenio`, but staging/ephemeral releases are named
> e.g. `gardenio-rwx-<sha>`, so substitute your release name (or discover it via
> the label selector below) or the host will NXDOMAIN.

```sh
# Discover the in-cluster Grafana Service for your namespace (release-agnostic).
NS=<namespace>   # e.g. gardenio
GSVC=$(kubectl get svc -n "$NS" -l app.kubernetes.io/name=grafana -o jsonpath='{.items[0].metadata.name}')
GHOST="$GSVC.$NS.svc.cluster.local:3000"
ADMIN="-u admin:admin"   # in-cluster break-glass; no admin password set in prod

# 1. Create a Viewer service account (idempotent — check first).
curl -s $ADMIN "http://$GHOST/api/serviceaccounts/search?query=gardenio-mcp-grafana"
SA_ID=$(curl -s $ADMIN -X POST -H 'Content-Type: application/json' \
  -d '{"name":"gardenio-mcp-grafana","role":"Viewer","isDisabled":false}' \
  "http://$GHOST/api/serviceaccounts" | jq -r .id)

# 2. Mint a token on that SA (copy the key once — it is shown only here).
TOKEN=$(curl -s $ADMIN -X POST -H 'Content-Type: application/json' \
  -d '{"name":"mcp-grafana-readonly"}' \
  "http://$GHOST/api/serviceaccounts/$SA_ID/tokens" | jq -r .key)

# 3. Seed the K8s Secret (key "token") — never put the token in argv.
printf '%s' "$TOKEN" > /tmp/grafana-mcp-token
kubectl create secret generic grafana-mcp-token -n "$NS" \
  --from-file=token=/tmp/grafana-mcp-token \
  --dry-run=client -o yaml | kubectl apply -f -
shred -u /tmp/grafana-mcp-token
unset TOKEN   # drop the token from the interactive shell session

# 4. Point the chart at it (deploy-values for the env):
#    mcpGrafana:
#      enabled: true
#      token:
#        secretName: "grafana-mcp-token"
#        externalSecret:
#          enabled: false        # break-glass: Secret seeded out-of-band
```

Acceptance smoke (from inside any pod in the namespace; `$GHOST`/`$NS` as above):

```sh
T=$(kubectl get secret grafana-mcp-token -n "$NS" -o jsonpath='{.data.token}' | base64 -d)
curl -fsS -H "Authorization: Bearer $T" \
  "http://$GHOST/api/datasources" | jq '.[].name'
# → ["VictoriaMetrics","VictoriaTraces"]  (200, read-only)
unset T
```

## Provisioning path 2 — durable AWS Secrets Manager rotation (production)

For a rotation-friendly source of truth, store the token in AWS Secrets Manager
and let ExternalSecret sync it. The ESO provider role can **read** SM but
**cannot `CreateSecret`**, so the operator seeds the entry once:

```sh
aws secretsmanager create-secret \
  --name gardenio/mcp-grafana/grafana-sa-token \
  --secret-string '{"token":"<the-glsa-token>"}'
# or put-secret-value if it already exists
```

Then enable the ExternalSecret in deploy-values:

```yaml
mcpGrafana:
  enabled: true
  token:
    secretName: "grafana-mcp-token"
    externalSecret:
      enabled: true
      secretStoreName: "aws-secretsmanager"
      secretStoreKind: ClusterSecretStore
      remoteRef: "gardenio/mcp-grafana/grafana-sa-token"
      tokenProperty: "token"
```

## Rotation

1. Mint a fresh token on the **same** `gardenio-mcp-grafana` SA (step 2 above),
   or via the Grafana UI (Administration → Service accounts → gardenio-mcp-grafana
   → Add token).
2. Update the source of truth:
   - break-glass: re-apply the K8s Secret (step 3);
   - AWS SM: `aws secretsmanager put-secret-value --secret-id
     gardenio/mcp-grafana/grafana-sa-token --secret-string '{"token":"<new>"}'`
     (ExternalSecret picks it up within the refresh interval).
3. Roll the mcp-grafana Deployment so it re-reads the env (`kubectl rollout
   restart deploy/<release>-mcp-grafana -n <namespace>`).
4. Delete the old token from the SA (Grafana UI or
   `DELETE /api/serviceaccounts/$SA_ID/tokens/<tokenId>`).

Add an entry to `gardenio/docs/secret-rotation.md` referencing this runbook.

## Sibling: the grafana-bridge read-back token (ka-0000003616)

The **grafana-bridge** Grafana→beads read-back poll (W2-impl-b, ka-0000001236)
needs the *same class* of token — a **Viewer** SA token — because it only calls
`GET /api/search` + `GET /api/dashboards/uid/<uid>` (the beads→Grafana write
direction stays on the least-privileged ConfigMap provisioning path and needs no
Grafana auth). So the two tokens share this runbook's scope, mint, and rotation
procedure. Provision it the same two ways:

- **Break-glass mint** (how the `gardenio-rwx` staging singleton is seeded today):
  mint a Viewer token on an SA named `gardenio-grafana-bridge` via the steps above
  (substitute that SA name), then seed a Secret named `grafana-bridge-token`
  (key `token`). Wire it in deploy-values:

  ```yaml
  grafanaBridge:
    grafana:
      url: "http://<release>-grafana:3000"   # in-cluster Grafana Service
      tokenSecret: "grafana-bridge-token"
      conflictPolicy: "grafana-wins"
  ```

  The bridge activates read-back only when `grafana.url` is set; a
  `grafana_token_set:true` + `poll complete … reconciled:0 conflicts:0` log line
  is the healthy steady state (the hash loop-breaker agrees on every dashboard).

- **AWS Secrets Manager** (rotation-friendly): flip
  `grafanaBridge.grafana.externalSecret.enabled=true`. Its `remoteRef` defaults
  to the SAME SM entry as mcp-grafana (`gardenio/mcp-grafana/grafana-sa-token`) —
  one Viewer token, one rotation — rendered by
  `templates/grafana-bridge/token-external-secret.yaml`.

## Notes

- The token is Viewer-scoped, so a leak is read-only blast radius on
  observability data already visible to every AVA user in the gardenio Grafana.
- Keep the SA named `gardenio-mcp-grafana` so rotation and audits are
  unambiguous; there is exactly one such SA per Grafana org.
- Datasources reachable today: `VictoriaMetrics` (Prometheus, uid
  `victoriametrics`, default) and `VictoriaTraces` (Jaeger, uid
  `victoriatraces`). There is deliberately **no Loki datasource**: mcp-grafana's
  `query_loki_logs` needs a `loki`-typed datasource, which the in-cluster
  VictoriaLogs (vlsingle) store cannot back — it speaks LogsQL, not the Loki
  query API. The A4 logs gap is instead closed by a **separate** standalone
  server, `mcp-victorialogs` (the official VictoriaLogs MCP server pointed at
  vlsingle), NOT by this Grafana SA token. See
  [`mcp-victorialogs.md`](mcp-victorialogs.md).
