# gardenio-next Config Mirror — Per-Env Deny-List

Source of truth for what the gardenio → gardenio-next config mirror MUST NOT carry.

The gardenio-next environment is a low-risk twin of gardenio that runs the latest
code against a faithful copy of gardenio's *config* (custom types, allowed prefixes,
role contexts, decision templates, project:gardenio config keys). It explicitly
does NOT carry live work (issues/comments/audit/gate state) and it MUST filter
out per-environment keys whose values are only valid in the source env — copying
them into gardenio-next would break the new env or, worse, cross-wire it to the
production env's Slack channel / webhook / Keycloak realm / billing key.

This file is the concrete deny-list that the mirror mechanism (Epic 4 task
[`ka-0000006436`](#)) consumes. Update this file and the consumer together;
do not let one drift ahead of the other.

> **Cross-reference**: advice [`ka-0000007027`](#) — *gardenio-next mirror:
> config-only, with per-env deny-list*. When that advice changes (categories
> added or removed), this document is the authoritative checklist that must
> change with it.

---

## 1. Mirror scope (recap)

| In scope (mirror copies) | Out of scope (never mirrored) |
|---|---|
| Custom type definitions (`type:<name>`, `kind=config`) | Live issues, in-progress work, assignee state |
| Allowed prefixes / project prefix list | Comments, audit events, gate state |
| Role contexts (`context:<role>`) | Per-env keys (this document) |
| Decision option metadata / decision templates | K8s Secrets in either namespace |
| Generic `project:gardenio` config beads — UNLESS they appear in §3 below | Helm values (those live in acme-helm-chart, not in beads) |

The mirror operates on **kbeads config records** (config-typed beads + the
underlying KV store). It does not touch K8s Secrets, ConfigMaps, or the Helm
chart at all — those are deployed independently per env. K8s Secrets are listed
in this doc only as **source-of-truth pointers** so the deny-list categories
are concrete and verifiable.

---

## 2. Hard deny-list — per-env keys

Anything in this section MUST be filtered by the mirror on every sync. Each
entry has:

- **Pattern / key** — what the mirror matches against.
- **Source of truth** — where the env-specific value actually lives (so we can
  verify a violation by reading the source).
- **Rationale** — why mirroring would be destructive.

### 2.1 Slack credentials & channels

| Pattern | Source of truth | Rationale |
|---|---|---|
| Helm `controller.slack.botToken` / `appToken` / `signingSecret` / `secretName` | K8s Secret named by `controller.slack.secretName` (per env) | Bot tokens belong to a single Slack workspace install. Sharing them either fails authentication or, worse, causes one env's bot to post into the other env's channel. |
| Helm `controller.slack.channel` | Helm value per env | Channel ID is workspace-specific; gardenio-next must post into its own channel (or none at all). |
| Helm `controller.slack.threadingMode` | Helm value per env | Threading-mode policy is an env preference, not a global config. |
| Helm `beads3d.slack.botToken` / `channel` / `secretName` | K8s Secret per env | Same reason as controller Slack creds. |
| Helm `slackBridge.slack.botToken` / `channel` / `secretName` | K8s Secret per env | Standalone bridge has the same constraint. |
| Helm `slackBridge.agentTokenSecret` | K8s Secret per env | Agent ↔ bridge auth token is namespace-local. |
| Config bead label `role:thread` carrying any `thread_ts` / `slack_channel_id` payload | bead `description` JSON | Thread/channel IDs only resolve in the env that produced them. |

**Verification:** any string matching `xoxb-`, `xoxa-`, `xoxp-`, `xapp-` in a
bead description is a Slack token and is a hard mirror failure.

### 2.2 Slack bridge / mention authorization

| Pattern | Source of truth | Rationale |
|---|---|---|
| Helm `slackBridge.keycloak.url` / `realm` / `clientID` / `secretName` / `mentionRole` | Helm + K8s Secret per env | Mention auth is gated against the env's own Keycloak realm. |

### 2.3 Ingress hostnames & DNS

| Pattern | Source of truth | Rationale |
|---|---|---|
| Helm `*.ingress.host` (controller, beads, beads3d, coopmux, slack-bridge, jira-bridge, gitlab-bridge, jira-bridge.webhook, etc.) | Helm value per env | Hostnames are per-env (`gardenio.e2e.example.com` vs `gardenio-next.e2e.example.com`). |
| Helm `keycloak.realm.issuerURL` and any embedded URL referencing a hostname | Helm value per env | The OIDC issuer URL contains the env's Keycloak hostname. |
| Any `*.e2e.example.com`-shaped string in a bead description | bead `description` | Hostname leak through config; same reason. |

### 2.4 Keycloak (OIDC) configuration

| Pattern | Source of truth | Rationale |
|---|---|---|
| Helm `keycloak.realm.name` (`"gardenio"`) | Helm value per env | gardenio-next runs in its own realm (or shares the gardenio realm via `realm.create: false` with `clusterKeycloakRef`); this is an env decision, not a beads-mirrored fact. |
| Helm `keycloak.realm.clusterKeycloakRef` | Helm value per env | Names a CR in the env's own cluster. |
| Helm `keycloak.realm.clients[*].redirectUris` and `webOrigins` | Helm value per env | Redirects encode env hostnames. |
| Helm `keycloak.oidcAudience` | Helm value per env | Audience matches the env's client registration. |
| Helm `keycloak.oidcAllowedEmailDomains` | Helm value per env | Domain allow-list is an env policy. |
| Helm `keycloak.admin.url` / `clientId` / `clientSecretRef` | Helm + K8s Secret per env | Admin client is bound to the env's realm. |
| Helm `externalSecrets.gitlabIdpCredentials.secretName` (`"gardenio-gitlab-idp-credentials"`) | K8s Secret per env | Per-env IdP creds. |
| Helm `externalSecrets.corporateIdpCredentials.secretName` (`"gardenio-corporate-idp-credentials"`) | K8s Secret per env | Per-env IdP creds. |
| Helm `externalSecrets.forwardAuth.clientSecret` / `cookieSecret` (K8s Secret `gardenio-forwardauth-secrets`) | K8s Secret per env | ForwardAuth (oauth2-proxy) cookies must not be shared. |

### 2.5 Webhook URLs (GitLab / JIRA)

| Pattern | Source of truth | Rationale |
|---|---|---|
| Helm `gitlabBridge.gitlab.baseURL` / `secretName` / `webhookSecret` / `webhookSecretName` / `groupID` / `botUsername` | Helm + K8s Secret per env | Webhook secret is shared between gardenio and the GitLab webhook *for that env*. Mirroring it would either silently break HMAC verification on gardenio-next or cause it to start accepting webhooks meant for gardenio. |
| Helm `controller.gitlab.baseURL` / `secretName` / `projectPath` | Helm + K8s Secret per env | The `/unreleased` integration is per-env. |
| Helm `controller.github.token` / `secretName` / `repos` (deprecated) | K8s Secret per env | Deprecated GitHub integration — still per-env until removed. |
| Helm `jiraBridge.jira.host` / `secretName` / `botAccountID` | Helm + K8s Secret per env | JIRA webhook URL + bot account are per-env. |
| Helm `jiraBridge.webhook.host` / `secret` / `secretName` | Helm + K8s Secret per env | HMAC secret is shared with the registered Atlassian webhook. |
| Helm `jiraBridge.webhook.ipWhitelist.sourceRange` | Helm value per env | Atlassian source CIDRs apply to the registered tenant. |

### 2.6 Bot account names & JIRA self-assignment

| Pattern | Source of truth | Rationale |
|---|---|---|
| Helm `gitlabBridge.gitlab.botUsername` | Helm value per env | The bot user is a real GitLab user owned by *one* env. Posting under it from a second env would confuse audit + trigger feedback loops. |
| Helm `jiraBridge.jira.botAccountID` | Helm value per env | JIRA assignee is the env's bot account. |
| Helm `controller.litellm.account` / any "default account name" | Helm value per env | Account fallbacks reference accounts that are scoped to the env's credentials store. |
| Config bead `auth` (`type:config`, `project:gardenio`, e.g. `ka-0000005433`) carrying `account` / `primary_account` / `failed_over_at` / `failover_reason` | bead `description` JSON | Failover state is per-env runtime state. The mirror must skip the *value* of this bead — it should not propagate primary-account selection or rate-limit failover state across envs. (The bead's *existence* is fine to mirror as long as the env-specific runtime fields are stripped.) |

### 2.7 LiteLLM keys & URLs

| Pattern | Source of truth | Rationale |
|---|---|---|
| Helm `controller.litellmURL` | Helm value per env | URL points at the env's LiteLLM proxy. |
| Helm `litellm.masterKeySecret` | K8s Secret per env (default `<release>-litellm-master-key`) | Master key authorizes admin against the env's LiteLLM. |
| Helm `externalSecrets.litellmMasterKey` | K8s Secret per env | Same. |
| Helm `litellm.databaseURL` | Helm value per env | DSN points at the env's Postgres. |
| Account bead fields (`type:account`): `base_url`, `litellm_key`, `credential` (e.g. on `ka-0000006686`) | bead `fields` JSON | `base_url` is the env's LiteLLM hostname; `litellm_key` is a virtual key issued against that LiteLLM; `credential` names a per-env K8s Secret (`secret:gardenio-claude-oauth-...`). All three are per-env. |
| Account bead runtime/usage fields: `status`, `exhausted_at`, `session_count`, `daily_cost_usd`, `total_*`, `rotation_count`, `last_rotated_at`, `throttle_*`, `weekly_cost_usd`, `probe_*`, `utilization_*`, `representative_claim`, `overage_*` | bead `fields` JSON | Per-env runtime/billing state. Mirroring would corrupt rotation and billing tracking on gardenio-next. The account bead's *identity* (id, type, title) and *static config* (provider, model, probe_model, fallback) MAY be mirrored; the runtime fields above MUST be stripped. |

### 2.8 External service endpoints & K8s service refs

| Pattern | Source of truth | Rationale |
|---|---|---|
| Helm `controller.natsURL`, `slackBridgeURL` | Helm value per env | In-cluster service DNS includes the namespace (`gardenio-beads.gardenio.svc.cluster.local`); gardenio-next must self-resolve. |
| Helm `coopmux.coopmuxTokenSecret`, `controller.natsTokenSecret`, `controller.claudeOAuthSecret`, `beads3d.daemonTokenSecret`, `beads.tokenSecret`, `slackBridge.agentTokenSecret`, `controller.autodestruct.tokenSecret` | K8s Secret names per env | All token secrets are namespace-scoped and rotated per env. |
| Helm `nats.tokens.infraToken` / `agentToken` and the `externalSecrets.natsAgentToken` / `natsInfraToken` ExternalSecret refs | K8s Secret per env | NATS auth tokens. |
| Helm `controller.externalSecretStore.name` / `kind` (`secretstore`/`ClusterSecretStore`) | Helm value per env | Names the env's ESO store; values are not interchangeable. |
| Helm `imagePullSecrets` and `registrySecrets.*` | K8s Secret per env | Pull secrets are per-cluster. |
| Helm `previousTokenSecret` (token rotator) | K8s Secret per env | Rotation history. |
| Bead description references to `*.svc.cluster.local`, `localhost:`, `127.0.0.1:`, or any `http(s)://` URL whose host is in §2.3 | bead `description` JSON | Endpoint leak through config. |

### 2.9 Per-project secret references

| Pattern | Source of truth | Rationale |
|---|---|---|
| Project bead `secrets` JSON field (any value matching `secret:`) | project bead `fields.secrets` | The secretreconciler creates ExternalSecret CRDs for these; the names are env-local. The project bead itself MAY be mirrored, but the `secrets` field MUST be stripped (or mapped to the gardenio-next equivalents in a follow-up). |

### 2.10 Static identifiers that must not collide

| Pattern | Source of truth | Rationale |
|---|---|---|
| Any bead whose description contains `gardenio.svc.cluster.local`, `gardenio-postgres-password`, `gardenio-claude-oauth-`, or any other string with the literal namespace prefix `gardenio-` followed by a name that resolves to a K8s object | bead `description` JSON | These are namespace-scoped pointers that gardenio-next will not be able to resolve. |

---

## 3. Conditional / partial-mirror rules

Some bead types are mirrored but with fields stripped. The mirror is responsible
for applying these per-type filters.

| Bead type | Mirror behavior | Stripped fields |
|---|---|---|
| `type:account` | Mirror identity + static config | All runtime/usage fields in §2.7 (account bead runtime row). |
| `type:config` title=`auth` | Mirror existence + schema | `account`, `primary_account`, `failed_over_at`, `failover_reason` (§2.6). |
| `type:config` title=`claude-mcp` | Mirror with care | Strip any field that names a per-env service hostname or token (e.g. MCP server URLs that point at in-cluster services). When in doubt: do not mirror. |
| `type:config` title=`claude-hooks` | Mirror as-is | The hook commands (`/hooks/wiki-sync.sh`, etc.) are baked into the agent image and are env-agnostic. Re-verify if any future hook starts referencing env-specific paths. |
| `type:user` | Out of scope for first cut | User beads carry capabilities tied to the env's GitLab/JIRA accounts (§2.6). Defer mirroring until the consumer task explicitly handles them. |
| `type:schedule` | Out of scope for first cut | Schedules reference role/project that exist in the env. Defer until both sides have the prerequisite roles/projects. |

---

## 4. Allow-list (positive — what the mirror DOES carry)

For belt-and-braces clarity, the mirror's allow-list is shorter than the deny-list.
A bead is mirrored iff:

1. It is `kind=config` AND
2. Its type is one of: `type:custom`, `type:context`, `type:prefix`, `type:decision-template`, `type:formula`, `type:advice` (with `project:gardenio` or `global` label) AND
3. None of its fields/description/labels match any pattern in §2 AND
4. Per-type partial-mirror rules in §3 are applied.

`type:account`, `type:config` (KV) carrying §2.6 / §2.7 state, `type:user`,
`type:schedule`, and any open `issue`-kind bead are **not** in the allow-list
for the first cut of the mirror.

---

## 5. Enforcement contract for the mirror

When task `ka-0000006436` lands, it MUST:

1. Apply the allow-list in §4 first; reject anything not allowed.
2. For each allowed bead, run §2 deny-list patterns over the bead's labels,
   description, and `fields` map. Any match → drop the bead (and emit a
   structured warning so we can audit drift).
3. For beads in §3, apply the per-type field-stripper before writing to
   gardenio-next.
4. Fail closed: an unrecognized type or an unrecognized bead schema is treated
   as deny, not allow.
5. Re-read §2 on every run (do not bake the deny-list into the mirror binary —
   load it as config or include it in code that is reviewed alongside this doc).

---

## 6. Maintenance

When the chart adds a new env-specific value, the chart change MUST update §2
in the same MR. The release preflight should fail any MR that adds a new
`*.host`, `*.secretName`, `*.botToken`, `*.signingSecret`, `*.appToken`,
`*.webhookSecret`, `*.token`, `*.url`, `*.URL`, `*.account`, `*.channel`,
`*.realm`, `*.clientID`, or `*.clientId` value to `helm/gardenio/values.yaml`
without a corresponding entry here.

When advice `ka-0000007027` is updated (categories added/removed), update §2
to match. The advice bead is the durable cross-session reminder; this doc is
the verifiable checklist.
