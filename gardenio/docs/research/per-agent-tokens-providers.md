# Per-Agent Tokens — Provider Inventory & v1 Scope (T3)

**Status:** Research / ADR draft (no implementation).
**Tracks:** Epic [ka-0000004236](https://beads-viewer.gardenio.e2e.example.com/ka-0000004236) — per-agent tokens.
**Bead:** [ka-0000007455](https://beads-viewer.gardenio.e2e.example.com/ka-0000007455).
**Predecessors:**
- `gardenio/docs/research/per-agent-tokens-codebase-state.md` (T0 — codebase walk).
- `gardenio/docs/research/per-agent-tokens-gitlab.md` (T1 — GitLab options survey).
- T2 (GitHub options survey) — in progress under `gardenio-t2-survey-github-40f`.

**Author:** gardenio-t3-survey-jira-l3p.
**Date:** 2026-04-26.

This survey inventories every upstream provider where a Claude-Code agent pod
currently authenticates with a **shared service-account token** rather than a
per-agent identity, and proposes which of those providers should land in the
v1 cutover and which can stay shared until later. The output binds the scope
of the T6 controller token issuer and the T7/T8 wiring tasks: anything in the
"v1" column needs a per-agent token path; anything "deferred" can keep the
shared secret until a follow-up.

## TL;DR

**v1 must include the providers where the agent's writes are visible to
humans and confused attribution causes real harm:**

1. **GitLab.com** — agents open MRs, push branches, comment on issues.
   Recommendation already drafted in T1 (Project Access Tokens at spawn
   time).
2. **GitHub.com** — agents push branches and open PRs in `groblegark/*`
   and other GH-hosted repos. Recommendation is T2's job; this doc just
   confirms it as a v1 must.
3. **Jira (Atlassian Cloud)** — agents post comments and transition
   tickets via `JIRA_API_TOKEN`. Same human-facing problem as GitLab:
   every comment lands as the shared bot user. **This is the focus of
   T4** and the only "human-facing" provider beyond Git hosts.

**Defer (v1.x or later, keep shared tokens for now):**

- **Slack.** Agent → bridge auth uses a shared `SLACK_BRIDGE_TOKEN`, but the
  bridge re-attributes posts to a per-agent display name in-thread. The
  externally-visible identity is already per-agent — no new upstream cred
  needed.
- **Beads daemon.** The per-agent token issuer is already implemented and
  gated behind `PER_AGENT_TOKENS=true`. Out of scope for this epic — it's
  the existence proof.
- **Anthropic / LiteLLM (`ANTHROPIC_*`, `CLAUDE_CODE_OAUTH_TOKEN`).** Per
  agent identity here is a billing-and-budget concern, not a human-facing
  attribution concern. Already partly handled by per-account beads and
  LiteLLM virtual keys; out of scope for the per-agent-tokens epic.
- **Coopmux / NATS bus tokens (`COOP_BROKER_TOKEN`, `COOP_NATS_TOKEN`).**
  Internal cluster-to-cluster auth. No external blast radius.
- **AWS keys (`AWS_*`, `GARDENIO_AWS_*`).** Currently shared IAM user creds.
  Per-agent IAM is a separate, larger scoping problem (STS / role assumption)
  and should be its own epic.
- **RWX (`RWX_ACCESS_TOKEN`), Mezmo (`MEZMO_SERVICE_KEY`), Victoria-
  Logs/Metrics/Traces creds.** Read-only or write-with-no-human-audience
  observability/CI reads. No human attribution surface to corrupt.
- **K6 / Keycloak load-testing creds.** Test-fixture credentials, not
  human-visible writes.

**Net:** v1 = three providers (GitLab, GitHub, Jira). Every other env var the
controller injects today is either internal infra, observability/CI reads, or
already-attributed-elsewhere. Don't broaden v1 unless a new write surface
emerges.

## Methodology

Inventory was assembled from three sources:

1. **Controller-injected env vars** — every `spec.SecretEnv = append(...)`
   site in `gardenio/controller/cmd/controller/podspec.go` `applyCommonConfig`
   and friends. These are install-wide secrets baked into every agent pod.
2. **Special-cased init-clone env vars** — env vars consumed by
   `gardenio/controller/internal/podmanager/initclone.go` to write
   `~/.git-credentials`. Currently only `GIT_TOKEN`, `GIT_USERNAME`, and
   `GITLAB_TOKEN`.
3. **Project-bead `secrets[]` declarations** — every `{Env, Secret, Key}`
   triple on currently-open `type=project` beads. In practice today only
   `monorepo` declares secrets (47 env vars across the providers below);
   other projects either have empty `secrets:[]` or no secrets field at all.
   The v1 scope question is about which of *these* env names the controller
   should rewrite to per-agent values.

Bridge-side tokens (`slack-bridge`, `jira-bridge`, `gitlab-bridge`,
`push-bridge`, `wl-bridge`, `trigger-engine`, `roles-server`) are out of scope
for this inventory: those Helm-chart `secretName` references live on bridge
Deployments, never on agent pods. They are mentioned only when an agent
*talks to* a bridge that holds a shared token (Slack), since that affects the
"is the externally-visible action attributed?" answer.

## Inventory

Columns:

- **Env var(s)** — the env name(s) injected into the agent pod.
- **Source** — where the value comes from. `controller-cfg` = controller-level
  Helm values applied to every agent in the install; `project-bead` = declared
  on a `type=project` bead's `secrets[]`; `account-bead` = resolved from a
  `type=account` bead's `Credential`/`LiteLLMKey`.
- **Code path** — file:line where the controller appends the SecretEnv.
- **Attribution scope today** — what identity the upstream sees when an
  agent uses this credential.
- **v1 / deferred** — whether T6/T7/T8 must replace this with a per-agent
  token in v1.
- **Notes** — special-casing or known constraints.

| Env var(s) | Source | Code path | Attribution scope today | v1 / deferred | Notes |
|---|---|---|---|---|---|
| `GIT_TOKEN`, `GIT_USERNAME` | project-bead | `podspec.go` ~601 → `initclone.go` ~110 | Shared GitHub bot user | **v1** (T2's job) | Special-cased: also sets `spec.GitCredentialsSecret` for init-clone `~/.git-credentials` write. Recommendation: T2. |
| `GITLAB_TOKEN` | project-bead | `podspec.go` ~604 → `initclone.go` ~115 | Shared GitLab bot user (`@gardenio-bot` or similar) | **v1** (T1's job) | Special-cased: also sets `spec.GitlabTokenSecret`. Recommendation: T1's PAT-at-spawn path. |
| `GLAB_TOKEN` | project-bead | `podspec.go` ~595 (generic SecretEnv) | Same shared GitLab bot — duplicate of `GITLAB_TOKEN` for the `glab` CLI | **v1** (T1's job) | Same upstream identity as `GITLAB_TOKEN`; rotate together. The CLI just looks for a different env name. |
| `JIRA_API_TOKEN` | project-bead | `podspec.go` ~595 | Shared Atlassian service account (one human-licensed user, one API token, one comment author for every agent) | **v1** (this epic, T4) | **Highest-priority human-facing attribution gap after Git hosts.** Atlassian Cloud bills per human; per-agent users are not free. T4 must survey Atlassian's options (scoped API tokens? OAuth 2.0 with rotating client creds? bot-account multiplexing?) before committing. |
| `JIRA_BASE_URL`, `JIRA_EMAIL` | project-bead | `podspec.go` ~595 | n/a — non-secret config | n/a | Plain config; carried alongside `JIRA_API_TOKEN` for convenience. |
| `SLACK_BRIDGE_TOKEN` | controller-cfg (`slackBridge.agentTokenSecret`) | `podspec.go` 572-577 | Shared bearer for `/api/slack/*` and `/api/permission/*` on the bridge. The **bridge** then posts as the configured Slack bot, but stamps the agent name in-thread. | **deferred** | The externally-visible Slack identity is already per-agent (bridge inserts agent name in the message). Replacing the bridge bearer with per-agent values would harden cluster-internal auth but add no human-facing benefit. |
| `COOP_NATS_TOKEN` | controller-cfg (`cfg.NatsAgentUserSecret` ∥ `cfg.NatsTokenSecret`) | `podspec.go` 480-491 | Cluster-internal NATS auth. With subject isolation enabled the token is publish-restricted to the agent's own subject namespace. | **deferred** | NATS subject isolation already provides per-agent scoping when enabled. No external blast radius. |
| `COOP_BROKER_TOKEN`, `COOP_MUX_TOKEN` | controller-cfg (`cfg.CoopmuxTokenSecret`) | `podspec.go` 550-562 | Shared bearer to coopmux for credential broker registration. | **deferred** | Same — internal-only. |
| `CLAUDE_CODE_OAUTH_TOKEN` | account-bead (`acct.Credential` → `secret:<name>`) | `podspec.go` 814-823 | The Claude OAuth account itself; every agent on the same account beads shares the same token. | **deferred** | Per-agent attribution here is a budget/billing problem, not human-facing. Already segmentable by `account` bead per project. |
| `ANTHROPIC_BASE_URL`, `ANTHROPIC_MODEL`, `ANTHROPIC_CUSTOM_HEADERS` | account-bead | `podspec.go` 783-808 | LiteLLM virtual key embedded in `ANTHROPIC_CUSTOM_HEADERS` is per-account, not per-agent. | **deferred** | Same — LiteLLM virtual keys give per-account budget scoping; per-agent virtual keys would be a follow-up. |
| `BEADS_TOKEN` (per-agent today, gated) | controller — `agenttoken.go` issuer | (existing, separate path) | Per-agent when `PER_AGENT_TOKENS=true`; shared static `kd-beads-token` otherwise. | **out of scope** | This is the existence-proof the rest of the epic models on. T0 covers it; nothing for this epic to do. |
| `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` | project-bead | `podspec.go` ~595 | Shared IAM user. | **deferred** (separate epic) | Per-agent AWS auth is properly STS / IAM-role-per-agent, which is a much larger scoping problem. Don't fold it into the per-agent-tokens epic. File a follow-up if/when it matters. |
| `GARDENIO_AWS_ACCESS_KEY_ID`, `GARDENIO_AWS_SECRET_ACCESS_KEY` | project-bead | `podspec.go` ~595 | Same as above, gardenio-scoped IAM user. | **deferred** | Same. |
| `RWX_ACCESS_TOKEN` | project-bead | `podspec.go` ~595 | Shared RWX CI account. | **deferred** | Used for `rwx dispatch` / `rwx results` — RWX UI shows the shared account. Follow-up only if RWX adds per-user attribution surface that an operator cares about. |
| `MEZMO_SERVICE_KEY` | project-bead | `podspec.go` ~595 | Shared log-ingestion key — write-only, not attributed in any UI. | **deferred** | Write-only ingestion key; no human-facing identity to corrupt. |
| `VICTORIA_LOGS_*`, `VICTORIA_METRICS_*`, `VICTORIA_TRACES_*` (24 vars across `_DEV`, `_PERF`, `_PROD` tiers) | project-bead | `podspec.go` ~595 | Shared basic-auth creds for read-only Victoria observability stack. | **deferred** | Read-only metrics/logs queries; no write surface. |
| `K6_PERF_*` (10 vars: `KC_ADMIN_*`, `KC_CLIENT_*`, `PIADMIN_PASSWORD`, `PMM_*`, etc.) | project-bead | `podspec.go` ~595 | Test-fixture creds for ACME load testing. | **deferred** | Synthetic test users; not real human identities. |

### Per-role secrets

`applyRoleSecrets` (`podspec.go` 642-691) repeats the same loop with a
`{role}-` prefix instead of `{project}-`. Same special-casing rules apply
for `GIT_TOKEN`/`GIT_USERNAME`/`GITLAB_TOKEN`. No new providers appear
that aren't already in the project-secrets table — role secrets are an
override mechanism, not a new injection surface.

### Currently-not-injected providers

For completeness, env vars that bridges hold but **never** reach an agent
pod (so they are not v1 candidates):

- `slack.botToken`, `slack.appToken`, `slack.signingSecret`
  (`slack-bridge` only).
- `gitlab.{token,webhookSecret}` on `gitlab-bridge` (used to validate
  inbound GitLab webhooks; agents never see it).
- `jira.{user,apiToken}` on `jira-bridge` (used by the bridge to fetch
  ticket metadata; separate from the agent-side `JIRA_API_TOKEN` above —
  both currently point at the same Atlassian account, but are distinct
  K8s Secrets and could be split).
- `dolthub.{token,org}` on `wl-bridge`.
- `apns.{keySecret,keyID,...}` on `push-bridge`.
- `triggerEngine.slack.botToken` (separate from the main slack-bridge bot).
- `litellm.masterKey` (admin key for LiteLLM proxy, not an agent cred).
- `keycloak.admin.clientSecret`, `keycloak.forwardAuth.{client,cookie}Secret`
  (used by the cluster auth proxy, not by agents).

## Recommendations

### v1 scope: GitLab + GitHub + Jira

These three are the only providers where the agent's writes land in a
human-visible attribution surface (MR/PR/issue authorship, ticket
comments). The cost of getting them wrong is real — operators cannot tell
which agent did what during an incident review, and "who closed FSM-1234"
points at the shared bot regardless of who actually drove the action.

The shape of the v1 cutover should mirror the existing per-agent beads
token issuer (`agenttoken.go` ~80-120 LoC):

1. Controller observes a spawn.
2. Controller calls a provider-specific issuer to mint a token scoped to
   the project being acted on.
3. Controller materialises a K8s Secret named `agent-<provider>-<beadID>`.
4. Controller adds a `SecretEnv` injection (or, for git-hosts, also
   updates `spec.GitCredentialsSecret` / `spec.GitlabTokenSecret`).
5. On pod despawn, the existing secret-cleanup path (`gardenio.io/type=
   agent-token` label pattern) revokes the token and deletes the Secret.

Each of T1/T2/T4 owns the issuer-specific details (PAT lifecycle, GitHub
App vs. token, Atlassian's options).

### Deferred providers: keep shared tokens, no controller changes

For everything in the "deferred" column, no v1 work is required. The
project-bead `secrets[]` declarations and the controller's generic
`SecretEnv` loop already do the right thing — they just inject a shared
secret. T6's per-agent token rewriter should treat the env-name list as
an explicit allow-list (`{GIT_TOKEN, GIT_USERNAME, GITLAB_TOKEN, GLAB_TOKEN,
JIRA_API_TOKEN}`) and pass everything else through unchanged.

### Out-of-scope, file separately

- **Per-agent AWS** — properly handled with STS or IRSA-style role
  assumption per pod, not by minting a long-lived access key per agent.
  Worth its own epic; do not fold into this one.
- **Per-agent Anthropic / LiteLLM virtual keys** — would give per-agent
  budget tracking. Possible but distinct from the
  attribution-corruption-on-human-surfaces story this epic targets.
- **Bridge-side token hardening** (`SLACK_BRIDGE_TOKEN`,
  `COOP_BROKER_TOKEN`) — moving these from shared to per-agent would
  improve cluster-internal auth posture but doesn't move any
  human-facing identity. Worth doing eventually; not now.

## Open questions for the consolidated decision

The post-T2/T4 `gb decision` should resolve these. Listed in priority
order:

1. **Is Atlassian Cloud's billing the gating constraint for v1 Jira?** If
   per-bot users on Atlassian count as billable seats, the v1 path almost
   certainly cannot be "one bot user per spawn" the way T1's GitLab
   recommendation works. T4 needs to land before this question can be
   answered authoritatively, but operators should preview their licence
   posture (number of seats remaining, willingness to pay for more,
   willingness to use a service-account-token-scoped solution if Atlassian
   offers one).
2. **Should `GLAB_TOKEN` and `GITLAB_TOKEN` be unified at the spawn
   site?** Today they are two SecretEnv entries pointing at the same
   value. The per-agent issuer should mint once and write both env names —
   trivial wiring, but worth being explicit.
3. **Bot-user multiplexing as a `JIRA_API_TOKEN` v1 fallback if
   Atlassian's options are bad.** I.e. one Atlassian user per *project*
   (not per agent), with the project ID baked into the comment body or
   prefix. This is strictly worse than per-agent attribution but strictly
   better than today's shared identity, and may be the only feasible v1
   if Atlassian's per-user pricing is prohibitive. T4 should explicitly
   surface this option.
4. **Failure-mode policy for issuer outages.** When the Jira/GitLab/GitHub
   token issuer is down at spawn time, does the agent (a) refuse to
   spawn, (b) spawn with no upstream cred (and fail when it tries to use
   one), (c) fall back to a shared "break-glass" token that re-introduces
   the attribution problem? T1 punted this question; T3 also punts; the
   consolidated decision must answer it.
5. **Audit-trail integration.** When a per-agent token mints, does the
   controller log the `(bead-id, agent-name, provider, token-id,
   created_at)` tuple to a queryable place? Operators investigating an
   incident need this — "I see PR #1234 was opened by `@gardenio-bot-x9k2`,
   which agent was that?". Not strictly part of v1, but the controller
   should at least emit the structured log.
6. **Revocation race.** Existing beads-token issuer revokes on pod
   despawn. For Git-host tokens that are also used by `git push --tags`
   inside long-lived MR pipelines triggered by the agent, what happens
   when the agent despawns mid-pipeline? T1 acknowledged this; T4 likely
   doesn't have it (Jira API calls are synchronous). Worth confirming
   per-provider.

## What this ADR does NOT decide

- It does not pick a Jira mechanism (Atlassian PATs vs. OAuth 2.0
  client-creds vs. bot-account multiplexing). That is **T4's** output.
- It does not pick a GitHub mechanism. That is **T2's** output (in
  progress).
- It does not change any code, secret reference, project-bead, role-bead,
  or Helm value. T6/T7/T8 are still gated on the consolidated `gb
  decision` after T1-T4 land.
- It does not file the AWS-per-agent or LiteLLM-per-agent follow-up
  epics. Those should be filed separately by an operator after this
  epic ships.

## Pointers

- T0 codebase walk: `gardenio/docs/research/per-agent-tokens-codebase-state.md`
- T1 GitLab survey: `gardenio/docs/research/per-agent-tokens-gitlab.md`
- Existing per-agent beads-token issuer:
  `gardenio/controller/cmd/controller/agenttoken.go`
- Project-bead secret injection:
  `gardenio/controller/cmd/controller/podspec.go` 585-617 (project),
  642-691 (role).
- Init-clone special-casing:
  `gardenio/controller/internal/podmanager/initclone.go` 100-180.
- Helm chart bridge-side secret references (out of scope, listed for
  context): `gardenio/helm/gardenio/values.yaml` — `slackBridge`,
  `jiraBridge`, `gitlabBridge`, `pushBridge`, `wlBridge`,
  `triggerEngine`, `litellm`, `keycloak` sections.
