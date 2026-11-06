# Per-Agent Tokens — GitLab Options Survey (T1)

**Status:** Research / ADR draft (no implementation).
**Tracks:** Epic [ka-0000004236](https://beads-viewer.gardenio.e2e.example.com/ka-0000004236) — per-agent tokens.
**Bead:** [ka-0000003589](https://beads-viewer.gardenio.e2e.example.com/ka-0000003589).
**Predecessor:** `gardenio/docs/research/per-agent-tokens-codebase-state.md` (T0).
**Author:** gardenio-t1-survey-gitlab-8l5.
**Date:** 2026-04-26.

This ADR surveys the mechanisms GitLab.com offers for issuing per-agent
upstream credentials and recommends one as the **default** path for the
GitLab side of the per-agent-tokens epic. The decision binds the design of
T6 (controller token issuer) and T8 (init-clone wiring) for the GitLab
provider.

## TL;DR

**Recommendation:** Use **Project Access Tokens (PATs) issued at agent-spawn
time, scoped to the project being acted on, with one bot user per
spawn**, falling back to a small **pre-warmed pool of group-scoped Group
Access Tokens** for cases where the agent must touch more than one project
inside the `example-org` group (e.g. cross-repo MRs). Both mechanisms
produce real per-bot-user attribution (`@project_NNNN_bot_xxxx` /
`@group_NNNN_bot_xxxx`) in the GitLab UI, are fully API-automatable, and
neither requires a paid SaaS feature.

Reject:
- **PATs against per-agent human bot users** — unbounded user creation on
  GitLab.com SaaS counts as billable seats and triggers admin sprawl.
- **GitLab service-account users** — Premium-only feature; their attribution
  shape is identical to PATs/GATs but at extra cost with no benefit.
- **OIDC / short-lived flows** for the agent-pod path — GitLab's CI_JOB_JWT
  / OIDC issuance is bound to GitLab CI runners, not to arbitrary K8s pods
  outside the runner. Useful only for the *CI side* of the system, not for
  Claude-Code agent pods. Documented for completeness; not the v1 mechanism.

The recommended Project-Access-Token path mirrors the existing per-agent
*beads-daemon* token issuer
(`gardenio/controller/cmd/controller/agenttoken.go`): generate → register
upstream → store as K8s Secret → plumb via `secretKeyRef` → revoke on
done/kill. T6 should be re-scoped as "generalise the existing issuer to
the GitLab provider" rather than "build from scratch."

The biggest open operator question (filed in §6) is **which GitLab tier
the `example-org` top-level group is on today**. Service-account
support, the issuance API for service accounts, and the upper bound on
Project/Group Access Tokens all depend on the answer. The recommendation
above does not require Premium; if we *are* on Premium, the
service-account option re-enters the conversation as a quality-of-life
improvement (cleaner attribution display) without changing the security
model.

## 1. Constraints carried in from the codebase

From `per-agent-tokens-codebase-state.md` §1–§4:

1. The init-clone path consumes exactly two GitLab credential shapes:
   `GITLAB_TOKEN` (preferred — `https://oauth2:${GITLAB_TOKEN}@gitlab.com`)
   and the legacy `GIT_USERNAME` / `GIT_TOKEN` fallback. Whichever
   mechanism we pick must hand back a plain bearer string the init script
   can drop into `~/.git-credentials` unchanged.
2. The existing per-agent-token machinery for the beads daemon uses
   K8s Secrets named `agent-token-<beadID>`, labelled
   `gardenio.io/type: agent-token`, key `token`. The GitLab issuer should
   follow this exact shape with a per-provider type label
   (`gardenio.io/type: agent-token-gitlab` or `gardenio.io/provider: gitlab`)
   so audit/cleanup tools see one family of resources.
3. There are **two separable attribution problems** (codebase-state §1):
   - **Commit-author attribution** (git `user.email`) — purely cosmetic
     unless the email belongs to a registered GitLab user with a verified
     signing key. The recommended PAT path makes this work for free
     because Project/Group Access Tokens automatically create a backing
     bot user with a known synthetic email
     (`project_NNNN_bot_xxxx@noreply.gitlab.com`) — git commits authored
     under that email show as **made by the bot user** in the MR diff
     view.
   - **MR/PR/API-actor attribution** ("Created By" in the MR list,
     "@author" in mentions and notifications) — determined entirely by
     the Authorization-bearer token on the API call. PAT/GAT both
     produce per-bot-user attribution here; this is the load-bearing win
     for the epic's stated success criteria.
4. Today's repo footprint inside `example-org`:
   `gardenio`, `site-deployment`, `monorepo`, `acme-helm-chart`,
   `cicd-templates` (and a handful of small subgroups). An agent's MR
   typically targets exactly one of these; cross-repo MRs are rare but
   do exist (gardenio ↔ acme-helm-chart on chart-version bumps, for
   example). This drives the project-scope-vs-group-scope choice.

## 2. The five options, side-by-side

The columns are the dimensions called out in the bead description.

| | (1) Project Access Token | (2) Group Access Token | (3) PAT against per-agent bot user | (4) Service Account user (Premium) | (5) OIDC / CI_JOB_JWT |
|---|---|---|---|---|---|
| **Issued via** | `POST /projects/:id/access_tokens` (REST) | `POST /groups/:id/access_tokens` (REST) | Manual user signup (or admin API on self-managed) → user logs in → `POST /personal_access_tokens` | `POST /service_accounts` (Premium+, group-level on SaaS) → `POST /service_accounts/:id/personal_access_tokens` | Issued by GitLab CI runner; not directly issuable to a K8s pod |
| **Backing identity** | Auto-created bot user `project_NNNN_bot_xxxx`; one per token | Auto-created bot user `group_NNNN_bot_xxxx`; one per token | One real user account per agent | Service-account user (no email, no login UI); one per call | Ephemeral; no backing user |
| **Expiry / rotation** | `expires_at` mandatory, max ~365 d (SaaS); `rotate` API supported | Same as PAT | Same as PAT | Same as PAT, but service-account user has no signup expiry of its own | Token TTL = job duration (minutes) |
| **Scope granularity** | `api`, `read_api`, `read_repository`, `write_repository`, role: Reporter…Owner. Project-scoped. | Same scopes; Group-scoped (covers all projects under the group) | Full PAT scope set; user-scoped (whatever projects user has access to) | Same scopes as PAT; Group-scoped via membership | `read_repository`, plus pre-set CI scopes |
| **Attribution in UI** | Bot user shows as MR author; "@project_NNNN_bot_xxxx" in mentions; bot-user avatar visible | Same shape, group-scoped bot | Real-looking user account; full attribution | Service-account user shows as MR author; UI badge marks it `Service account` | Pipeline / job link; no per-user attribution |
| **Scaling ceiling** | Documented limit: per-resource access-token count is enforced by GitLab; SaaS soft-cap order-of-100s per project (no published hard cap). One token per agent spawn ≈ a few thousand/day at peak — measured against 1 token/spawn × spawn rate. | Same ceiling, group-scoped. | **Hard problem on SaaS**: each bot user counts as a billable seat once given any project membership ≥ Reporter; user creation is throttled and admin-visible. Self-managed has no seat cost but admin sprawl is severe. | Service-account users do **not** consume billable seats (this is the main reason Premium customers buy this feature). Per-group ceiling is documented per Premium plan, but in practice >> agent count. | N/A |
| **API automatability** | Full — single REST call returns the bearer string | Full — single REST call | Partial — user signup is interactive; SCIM/admin API automation requires self-managed admin or paid SCIM | Full — single REST call (when the feature is enabled) | Full *only inside CI* |
| **Cost** | Free (Free / Premium / Ultimate) | Free | "Free" but seat-billed if the bot user has any project membership at Reporter+ | **Premium minimum**; service-account count tied to plan (Premium / Ultimate quotas differ) | Free |
| **Revocation** | `DELETE /projects/:id/access_tokens/:tid` or token-rotate; effective immediately | `DELETE /groups/:id/access_tokens/:tid`; effective immediately | `DELETE /personal_access_tokens/:tid` against the user; user account itself stays unless deleted | `DELETE /service_accounts/:id` (deletes the SA user) or revoke its PAT | Auto-revoked at job end |
| **Multi-project access** | One token per project; cross-repo MRs need either multiple tokens (one per project being touched) or a Group Access Token instead | Single token covers all projects in the group | Single token (per user's project membership) | Single token (per SA user's group membership) | Single token (per pipeline scope) |

Notes on cells worth elaborating:

- **Project Access Token vs Group Access Token feature equivalence.**
  GitLab.com treats GAT as a strict superset of PAT in scope: anything a
  PAT can do, a GAT can do across every project under the group. The
  trade-off is blast radius — a leaked GAT compromises every project; a
  leaked PAT compromises one. For the agent use-case where most spawns
  touch a single repo, project-scoped is preferable.

- **PAT-against-bot-user is the option a naive read of the epic body
  reaches for**, and it is the wrong default. On GitLab.com, creating
  one upstream user per agent (a) consumes a paid seat once that user is
  invited to any private project, (b) requires SCIM or admin
  intervention because user signup is interactive, and (c) leaves an
  unbounded fleet of zombie accounts visible in `Admin → Users` that
  must be GC'd manually. Project Access Tokens give us the *same*
  attribution (a bot user appears as MR author) without any of these
  costs because the bot user is auto-created, scoped to the project,
  and disappears with the token.

- **Service accounts** are "Project Access Tokens with nicer UI"
  semantically. They differ from PAT/GAT only in (a) the user object
  has a `Service account` badge in the UI, (b) the user is *not*
  auto-created — you call a separate endpoint, (c) Premium minimum.
  For our attribution goals (MR author shows the agent's identity),
  PAT/GAT bot users already work. The premium-tier SA endpoint is a
  nice-to-have if and only if we are already on Premium.

- **OIDC / CI_JOB_JWT.** GitLab's OIDC issuer is wired to runners — it
  hands out a short-lived JWT to a job that the job can exchange with
  external services (AWS STS, Vault, etc.). It is *not* a way for an
  arbitrary K8s pod outside a CI runner to obtain a per-job token to
  call back into GitLab. Some people have wired it the other way for
  external clusters (using ID-token exchange via a custom IDP), but
  that is a build-it-yourself path and outside the scope of "give the
  agent a GitLab credential at spawn time." It is documented here for
  completeness and rejected as v1.

## 3. Recommendation

**Default path: Project Access Token, one per agent spawn, project-scoped.**

Issuance flow at agent-pod creation, by analogy to the existing
beads-daemon issuer:

1. Controller resolves the **target project** for the spawn from the
   agent bead (today: the `BOAT_PROJECTS` env var derives this from
   `gardenio`/`monorepo`/etc.; T6 will need a small extension to map
   "this is the GitLab repo this agent will push to" → numeric project
   ID).
2. Controller calls
   `POST /api/v4/projects/<id>/access_tokens` with body
   `{ name: "agent-<beadID>", scopes: ["read_repository","write_repository","api"], access_level: 30 (Developer), expires_at: <now+ttl> }`.
   The `name` is the only operator-visible string in the bot user list,
   so we encode the agent bead ID for traceability.
3. Response carries `token` (one-time-visible bearer) and the
   auto-created bot user's username (`project_<n>_bot_<hash>`).
4. Controller writes a K8s Secret
   `agent-gitlab-token-<beadID>`, label
   `gardenio.io/type: agent-token`, `gardenio.io/provider: gitlab`,
   keys `token` (the bearer) and `username` (the bot username — used
   to set git `user.name` for commit attribution; see §3.3).
5. Pod-spec's `spec.GitlabTokenSecret` points at this Secret instead
   of the global `gardenio-gitlab-token`. (`podspec.go` already supports
   this field; no init-clone code changes are required.)
6. On done/kill, controller calls
   `DELETE /api/v4/projects/<id>/access_tokens/<tid>` and deletes the
   K8s Secret. Best-effort + janitor-loop sweep, exactly as the
   existing beads-token issuer does.

### 3.1 Why PAT and not GAT as the default

For the gardenio agent population today, the overwhelming majority of
MRs target a single project (`gardenio` or `site-deployment` or
`acme-helm-chart`, never two at once in the same agent spawn). A PAT
satisfies these without expanding blast radius. Cross-repo work is
rare enough that a small GAT-backed pool (§3.2) is the right escape
hatch.

### 3.2 Group Access Token fallback for cross-repo work

A small subset of agents cut MRs across multiple repos in a single
spawn (typical: a release gardener bumping `gardenio` Chart.yaml in
`gardenio`, then bumping the chart version in `acme-helm-chart`).
Issuing N PATs per spawn for the rare cross-repo case adds complexity
the controller code path doesn't need.

Proposal: the controller maintains a **pre-warmed pool of GATs at the
`example-org` group level**, lifecycle-managed the same way
project tokens are (rotated periodically, revoked on demand), and
agent specs can opt into a GAT instead of a PAT via a per-role or
per-spawn flag (`spec.GitlabTokenScope = "group"`). Default remains
project-scope; group-scope is opt-in. Pool size starts at 4–8 and
rotates on a long cadence; this keeps the active count bounded and
revocation simple.

### 3.3 Commit-author attribution side-effect

Today the agent pod sets git `user.email` to `<role>@gardenio.local`
and `user.name` to the agent name (codebase-state §1.3). With the PAT
path, the controller can additionally write the auto-issued bot
username/email into the per-agent K8s Secret, and `gb agent start`
can pick those up for `git config user.email` /
`user.name`. Then commits show as authored by the bot user that
opened the MR — **the same identity the GitLab MR author column
shows** — and the "Author"/"Created By" finally line up.

This collapses T9 (the codebase-state §4.5 "Phase 0 win") into the
PAT rollout: once the issuer runs, the synthetic email becomes the
real bot email automatically. T9 can still ship independently as a
small win, but doing so on top of PAT issuance gives the cleanest
end-state.

### 3.4 Why not service accounts even if we are on Premium

The only ergonomic difference between an SA-PAT and a project PAT is
the UI badge ("Service account" vs the auto-created bot user). The
attribution semantics are identical: an MR author column, a mention
handle, an audit-event actor. The cost of switching mid-rollout is
small (the issuer has the same shape; the API path differs by one
URL prefix). The *value* of switching is essentially cosmetic.

If, after we are operating PATs at scale, the operator decides the
bot-user sprawl in the project's Members list is noisy enough to be
worth the Premium cost, the issuer can grow a `Provider: gitlab-sa`
mode that targets the Premium endpoint. This is a future option, not
a v1 requirement. **Do not block the v1 rollout on a Premium-tier
purchase decision.**

## 4. Sizing and rate considerations

- **Token count at peak.** Today's agent population spawns at
  roughly the order of dozens-to-low-hundreds per day across all
  projects. Even with per-spawn token issuance, that's well under
  any documented GitLab cap. Storage cost is one K8s Secret per
  active agent — additive to the existing `agent-token-<beadID>`
  Secret created by the beads-daemon issuer, so per-agent Secret
  count goes from 1 → 2 (or 1 → 3 once GitHub T2 lands).
- **Rate-limit attribution.** Each PAT counts as a separate identity
  for GitLab.com's per-user/per-IP rate limiters. This is good for
  isolation (one runaway agent doesn't burn the shared token's
  rate-limit budget) but means we should monitor per-agent rate-limit
  hits during the staging rollout. The bridges and the controller
  itself stay on a long-lived service token — they should not share
  identity with the agent pool.
- **Token-rotation cadence.** Maximum `expires_at` is approximately
  365 d on SaaS (changed from "no expiry" to "max 1 year" on
  GitLab.com sometime in 2023–2024). Per-spawn tokens have lifetime
  ≪ 1 day, so this never bites. The GAT pool's TTL needs to be set
  short (e.g. 30 d) and rotated by a reconciler.

## 5. Migration & co-existence with the existing static token

- Today's `gardenio-gitlab-token` Secret backs every agent. The PAT
  path replaces this on a **per-pod** basis: if the controller has
  successfully provisioned a per-agent token, the pod's
  `spec.GitlabTokenSecret` points at the per-agent Secret; if
  provisioning fails, the controller can choose to (a) refuse to
  start the pod, or (b) fall back to the shared token under a flag.
  Recommend (a) once the rollout is past staging — failing closed is
  the right default for a credential-issuance system.
- The legacy `gardenio-gitlab-token` should remain provisioned in the
  controller's namespace for the bridges (`gitlab-bridge`, etc.) and
  for the controller's own GitLab API calls (issuing per-agent
  tokens uses *this* token as the API caller). It's not a per-agent
  thing; it's the controller's identity. It must be a token with
  Maintainer-or-Owner on the target projects so it can issue PATs
  there.
- In-flight MRs/PRs authored by the legacy shared token cannot be
  retroactively re-attributed (codebase-state §5). The cutover plan
  should keep these editable by the legacy identity for at least one
  release cycle past the flip — i.e. we can issue per-agent tokens
  going forward but the legacy identity remains a valid actor on
  pre-existing MRs until they merge or close.

## 6. Open questions for operator review

These are decisions the human operator must make before T6
implementation begins. Each is small enough to handle as a single
`gb decision` once T1–T5 are all in.

1. **Which GitLab tier is `example-org` on?** The recommendation
   above stands on Free; Service Accounts and SCIM-driven user
   provisioning need Premium. If we are on Premium, do we want to
   adopt SA's Service-account badge for cosmetic clarity (a follow-on
   ticket) or stay on PAT-with-bot-user?
2. **Group Access Token pool: in-scope for v1?** The proposed default
   (project-scope by default; opt-in group-scope) covers the
   common case but adds a second issuance path. Alternative: ship
   PAT-only in v1, defer the GAT pool to a v1.1 once cross-repo
   demand is measured.
3. **Per-spawn token TTL.** Recommend `agent-spawn-time + 24h` so a
   long-running spawn finishes under its own credential and a
   crashed/orphaned token still expires on its own if revoke fails.
   Operator may prefer shorter (e.g. 4h) for tighter blast radius
   at the cost of more rollover noise.
4. **Failure-mode policy.** If GitLab API rate-limits the issuer or
   the project-tokens endpoint is down at spawn time, do we (a) fail
   the pod, (b) start the pod with no GitLab token (any `glab` /
   `git push` will fail), or (c) fall back to the shared token for
   that spawn under a metric? Recommend (a) for prod, (c) for
   staging-only with the metric loud enough to alarm.
5. **Bot-user GC.** Project access tokens auto-delete their bot user
   on token revoke; group access tokens behave the same. We don't
   need a separate GC pass for bot users so long as we always go
   through the documented delete endpoint. Confirm this is still true
   on the current GitLab.com release before we commit.
6. **Scope minimisation.** Recommendation lists `api`,
   `read_repository`, `write_repository` as the default scope set.
   Some agents only read (intake research tasks): a future
   refinement is per-role scopes (`read_*` for research roles,
   `api+write_*` for delivery). Out-of-scope for v1 but worth
   recording.
7. **GAT-pool churn metric.** If we ship the GAT pool, what's the
   right rotation cadence (30 d? 7 d?) and what visibility do we
   want into it? The pool size and rotation should be a `kd fleet
   list`-style command in `gb tokens` (T10).

## 7. Pointers (for the T6/T8 implementer)

| Topic | Where to look |
|---|---|
| Existing per-agent token issuer (the pattern to extend) | `gardenio/controller/cmd/controller/agenttoken.go` |
| Issuer call sites (will need a parallel set for the GitLab provider) | `gardenio/controller/cmd/controller/main.go:804,835,865,870` |
| Where `GITLAB_TOKEN` enters the pod spec today | `gardenio/controller/cmd/controller/podspec.go:585–624` (project), `:638–691` (role) |
| Init-clone consumer of `GITLAB_TOKEN` | `gardenio/controller/internal/podmanager/initclone.go:109–161` |
| `PER_AGENT_TOKENS` flag and config plumbing | `gardenio/controller/internal/config/config.go:189,552` |
| Helm value the parent enablement plan flips today | `gardenio/helm/gardenio/values.yaml` (`agents.perAgentTokens`) |
| GitLab.com API: project access tokens | `https://docs.gitlab.com/api/project_access_tokens/` |
| GitLab.com API: group access tokens | `https://docs.gitlab.com/api/group_access_tokens/` |
| GitLab.com API: service accounts (Premium) | `https://docs.gitlab.com/api/group_service_accounts/` |
| GitLab.com docs: token rotation API | `https://docs.gitlab.com/api/project_access_tokens/#rotate-a-project-access-token` |
| Predecessor research (read this first) | `gardenio/docs/research/per-agent-tokens-codebase-state.md` |
| Parent enablement plan (beads-daemon side) | `gardenio/docs/per-agent-tokens-enablement-plan.md` |

## 8. What this ADR does NOT decide

- The **token-lifecycle design** (when issued, where stored, how
  revoked at scale) — that's T4. This doc only sketches the per-spawn
  case to make the survey concrete.
- The **GitHub equivalent**. T2 covers GitHub; the recommendations
  there will likely differ (GitHub Apps + installation tokens are the
  GitHub-shaped equivalent of GitLab's PAT-with-bot-user pattern, but
  attribution semantics are different — see T2).
- The **scheduler/bridge `created_by = static` problem** documented
  in codebase-state §7.7. That is independent of upstream tokens —
  fixing it is one `SetActor` call per bridge binary against the
  existing static beads token. The epic should file that as a
  separate `gb decision`.
- Whether to ship the GAT pool in v1 or defer to v1.1 — see open
  question §6.2.
