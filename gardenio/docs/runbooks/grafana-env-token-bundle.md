# Env-side Grafana Editor token bundle — operator runbook

**Task:** ka-0000002337 (DEVOPS-722 chunk 3)
**Epic:** ka-0000002808 — gardenio observability access gaps (FSM-311 fallout)
**Design:** `docs/research/fsm-311-observability-gaps-design.md` §1
**Operator:** Matt Baker (slack:U0OPERATOR1)

## What this is

Wires per-environment Grafana **Editor** service-account tokens into gardenio
agent pods so agents can reach the **ACME environment Grafana** HTTP API
(`lens.acme.net` for prod, the equivalent ingress for devel/perf) — where
FSM-class incidents actually happen.

This is **distinct** from the in-cluster gardenio Grafana that agents already
reach via `GRAFANA_URL` + the auth-proxy (`gb grafana render`). That path is
done; this bundle is the missing env-side hole.

The chart + controller plumbing is **landed and OFF by default** (zero blast
radius). It lights up only after the operator does the two one-time,
privileged steps below — which an agent cannot do (they require Grafana
admin in each env and AWS Secrets Manager writes; prod `acme.net` is
off-limits to agents).

## How it flows (already in code)

```text
AWS Secrets Manager                 External Secret              agent pod env
gardenio/grafana-api-key/devel  ─┐
gardenio/grafana-api-key/perf   ─┼─▶ Secret grafana-api-key  ─┬─▶ GRAFANA_API_KEY_DEVEL
gardenio/grafana-api-key/prod   ─┘     keys: token-{env}      ├─▶ GRAFANA_API_KEY_PERF
                                                             └─▶ GRAFANA_API_KEY_PROD
deploy-values grafanaBaseURLs  ───────────────────────────────▶ GRAFANA_BASE_URL_{ENV}
```

- ExternalSecret template: `gardenio/helm/gardenio/templates/grafana-api-key-external-secret.yaml`
- Controller wiring: `gardenio/controller/cmd/controller/podspec.go` (`applyCommonConfig`)
  + `gardenio/helm/gardenio/templates/controller/deployment.yaml`
- Config knobs: `agents.grafanaApiKeySecret`, `agents.grafanaApiKeyExternalSecret.*`,
  `agents.grafanaBaseURLs.{devel,perf,prod}` in `deploy-values/gardenio.yaml`.
- The per-env `GRAFANA_API_KEY_<ENV>` `SecretKeyRef` is marked **optional**, so
  provisioning one env at a time (devel first, prod last) never wedges pod
  creation on a not-yet-synced key.

## Access level: Editor, not Admin

Per design §1: **Editor** (read + write dashboards/folders), never **Admin**
(user management, datasource edits, more token minting). Editor lets agents
iterate dashboards mid-incident; the durability mechanism is the
dashboards-as-code round-trip MR back to
`terraform/new-modules/ecs-services/grafana/dashboards/*.json` in
`site-deployment`. See the design doc's "Risks of Grafana write access" table.

Verify on each env that the Editor role is **not** granted `alert.rules:write`
(default unified-alerting RBAC) and that `editors_can_admin = false`.

## Operator checklist (per environment: devel → perf → prod)

> Do devel first, smoke-test, then perf, then prod. The optional-key wiring
> means a half-provisioned bundle is safe.

- [ ] **1. Cut the Grafana Editor service-account token.** Design recommends
      **Option A** (native Grafana service account, no Keycloak change):
  - In each env's Grafana (`https://<env-grafana-host>`), as an admin:
    `Administration → Service accounts → Add` →
    name `gardenio-agent-editor-<env>`, role **Editor**.
  - Add a token on that SA (no expiry, or long-lived per
    `gardenio/docs/secret-rotation.md`). Copy the token once.
  - (Option B — Keycloak SA in the observability realm — is documented in
    design §1 if single-source-of-truth identity is preferred later.)

- [ ] **2. Store the token in AWS Secrets Manager** (gardenio account):
  ```sh
  aws secretsmanager create-secret \
    --name gardenio/grafana-api-key/<env> \
    --secret-string '{"token":"<the-SA-token>"}'
  # or put-secret-value if it already exists
  ```
  The property key must be `token` (matches `tokenProperty` default).

- [ ] **3. Confirm the per-env Grafana base URL.** From
      `terraform/new-modules/ecs-services/grafana/grafana.tf`, the ingress is
      derived from `var.base_info.public_dns_name` (e.g.
      `https://lens.acme.net/grafana` for prod). Set it in
      `deploy-values/gardenio.yaml` under `agents.grafanaBaseURLs.<env>`.

- [ ] **4. Enable the bundle** in `deploy-values/gardenio.yaml`:
  ```yaml
  agents:
    grafanaApiKeySecret: "grafana-api-key"
    grafanaApiKeyExternalSecret:
      enabled: true            # ← flip on
    grafanaBaseURLs:
      devel: "https://<devel-grafana-host>/grafana"
      perf:  "https://<perf-grafana-host>/grafana"
      prod:  "https://lens.acme.net/grafana"
  ```
  Deploy the chart (the standard gardenio release path).

- [ ] **5. (gVisor agents only)** If runsc freeze/thaw agents are enabled,
      mirror the Secret into the gVisor namespace by adding to
      `gvisor.namespace.secretMirrors` in `deploy-values/gardenio.yaml`:
  ```yaml
      - name: grafana-api-key
        remoteRef: gardenio/grafana-api-key/prod   # JSON object → dataFrom.extract
        refreshInterval: 15m
  ```
  > Only add the mirror once the SM entry exists, or the canary defers forever
  > on "referenced secrets not yet synced" (see the secretMirrors note in
  > `deploy-values/gardenio.yaml`). One mirror per env Secret as needed.

- [ ] **6. Acceptance smoke** from inside a gardenio agent pod:
  ```sh
  # Read
  curl -fsS -H "Authorization: Bearer $GRAFANA_API_KEY_PROD" \
       "$GRAFANA_BASE_URL_PROD/api/search?type=dash-db" | jq '.[0]'

  # Write — create then delete a throwaway dashboard
  curl -fsS -X POST -H "Authorization: Bearer $GRAFANA_API_KEY_PROD" \
       -H "Content-Type: application/json" \
       -d '{"dashboard":{"title":"agent-write-smoke","panels":[]},"overwrite":false}' \
       "$GRAFANA_BASE_URL_PROD/api/dashboards/db" | jq '.uid'
  ```
  Both return 200. (Repeat with `_DEVEL` / `_PERF`.)

## Followups (design §1)

- Advice bead: "How to use Grafana API from agent pods" — env-var names +
  cookbook (list dashboards, fetch a panel's PromQL, create/update via
  `/api/dashboards/db`, export to JSON for an MR).
- Workflow bead: round-tripping a dashboard improvement to `site-deployment`
  (`terraform/new-modules/ecs-services/grafana/dashboards/<name>.json`).
- Rotation entry in `gardenio/docs/secret-rotation.md`.
- `agent-edits/` folder convention + 30-day cleanup (design §1 risks table).
