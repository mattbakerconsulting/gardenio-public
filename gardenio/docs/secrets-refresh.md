# ExternalSecret refreshInterval policy

The gardenio Helm chart provisions a handful of `ExternalSecret` resources
that fetch credentials from the cluster's configured secret store
(AWS Secrets Manager via ClusterSecretStore in production). Each
`refreshInterval` is a tradeoff: short intervals propagate rotations
quickly but increase API churn against the secret backend; long intervals
minimize churn but widen the window where pods hold a stale value after a
rotation.

Rather than tune each ExternalSecret independently, the chart groups
secrets into three rotation-cadence buckets and exposes one knob per
bucket under `externalSecrets.refreshIntervals`. Templates declare which
bucket each secret belongs to via the
`gardenio.externalSecrets.refreshInterval` helper.

## Categories

| Category     | Default | When to use | Rationale |
| ------------ | ------- | ----------- | --------- |
| `rotated`    | `15m`   | Secrets actively rotated by automation: HMAC signing keys, NATS subject-isolation tokens, daemon tokens managed by the `token-rotator` CronJob. | The rotator publishes a new value with a short overlap window. A 15m refresh keeps the in-cluster K8s Secret converged inside that window so pods do not authenticate with the about-to-be-revoked value. |
| `rotated-db` | `5m`    | The RDS-managed master **DB password**, which AWS RDS / Secrets Manager rotates **automatically** on its own schedule (not a manual op). | The rotation is invisible to K8s and has **no** overlap window the consumer can observe: the instant RDS cuts over, the old password is dead. The daemon substitutes the password into `BEADS_DATABASE_URL` at **container start** (a kubelet env-var expansion that never live-updates), so a stale in-cluster Secret means the pod CrashLoops on `pq 28P01` until the next refresh AND a restart. A 5m refresh caps that stale window at minutes instead of the `static` bucket's hours. This is the fix for the P0 outage ka-0000006811 (2026-07-25, ~30min fleet-wide). |
| `static`     | `6h`    | Long-lived infrastructure credentials: registry pull credentials, LiteLLM master keys, OIDC client credentials. | These rotate only as a manual operator step (with downtime tolerance). A 6h refresh is well inside any reasonable operator rotation SLA while keeping secret-store request volume low. **Do NOT use `static` for the auto-rotating RDS master password — use `rotated-db`.** |
| `oauth`      | `1h`    | OAuth client secrets and external-provider API tokens: oauth2-proxy client/cookie secrets, JIRA API token + webhook HMAC, Mezmo MCP service key. | These usually only change when an operator rotates them in the upstream system. A 1h refresh balances propagation latency against rate-limit pressure on the upstream provider. |

## Mapping (current chart)

| ExternalSecret                                  | Category | File                                                  |
| ----------------------------------------------- | -------- | ----------------------------------------------------- |
| beads daemon token                              | rotated  | `templates/external-secrets.yaml`                     |
| edge-auth HMAC signing key                      | rotated  | `templates/edge-auth/externalsecret.yaml`             |
| NATS agent (subject-isolation) token            | rotated  | `templates/nats/agent-creds-secret.yaml`              |
| NATS infra (full pub/sub) token                 | rotated  | `templates/nats/infra-creds-secret.yaml`              |
| PostgreSQL password (RDS-managed master)        | rotated-db | `templates/external-secrets.yaml`                   |
| LiteLLM master key                              | static   | `templates/external-secrets.yaml`                     |
| GitLab IdP client-id / client-secret            | static   | `templates/external-secrets.yaml`                     |
| Corporate IdP client-id / client-secret         | static   | `templates/external-secrets.yaml`                     |
| Registry pull credentials                       | static   | `templates/registry-external-secrets.yaml`            |
| oauth2-proxy / Keycloak forwardAuth secrets     | oauth    | `templates/external-secrets.yaml`                     |
| JIRA API token                                  | oauth    | `templates/external-secrets.yaml`                     |
| JIRA webhook HMAC                               | oauth    | `templates/external-secrets.yaml`                     |
| Mezmo MCP service key                           | oauth    | `templates/mezmo-credentials-external-secret.yaml`    |

## Tuning

The category defaults live in [`helm/gardenio/values.yaml`](../helm/gardenio/values.yaml)
under `externalSecrets.refreshIntervals`:

```yaml
externalSecrets:
  refreshInterval: "1h"       # fallback when refreshIntervals is unset
  refreshIntervals:
    rotated:    "15m"
    rotated-db: "5m"
    static:     "6h"
    oauth:      "1h"
```

> **Note — the retired terraform sibling.** On the old EKS canonical-prod / POC
> deployments the beads daemon mounted `<release>-postgres-external-password`,
> an ExternalSecret defined **outside** this chart by the `gardenio-chart`
> terraform module (`kubectl_manifest.rds_password_external_secret`, pinned to
> `5m`). That module and the RDS it served were deleted with the EKS stack
> (gd-R0jeUZAl3F); the k3s deployment has no such sibling, so `rotated-db` here
> is the only knob.

To shorten or extend the cadence for a whole bucket, override one of
these values in your per-env values file. For example, to refresh
rotated secrets every 5 minutes in a staging environment:

```yaml
externalSecrets:
  refreshIntervals:
    rotated: "5m"
```

Per-secret overrides still take precedence where the chart exposes them
(for example `agents.mezmoServiceKeyExternalSecret.refreshInterval`). Leave those
empty to inherit from the centralized category.

## Adding a new ExternalSecret

When adding a new `ExternalSecret` template, pick a category and call
the helper in the manifest:

```yaml
spec:
  refreshInterval: {{ include "gardenio.externalSecrets.refreshInterval" (dict "category" "rotated" "root" .) }}
```

Do not hardcode a duration string. Add the new secret to the mapping
table above so reviewers can see its rotation cadence at a glance.
