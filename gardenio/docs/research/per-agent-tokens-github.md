# Per-Agent Tokens — GitHub Options Survey (T2)

**Status:** Research / ADR draft (no implementation).
**Tracks:** Epic [ka-0000004236](https://beads-viewer.gardenio.e2e.example.com/ka-0000004236) — per-agent tokens.
**Bead:** [ka-0000000783](https://beads-viewer.gardenio.e2e.example.com/ka-0000000783).
**Predecessor (T0):** `gardenio/docs/research/per-agent-tokens-codebase-state.md`.
**Predecessor (T1):** `gardenio/docs/research/per-agent-tokens-gitlab.md` (MR !1565).
**Author:** gardenio-t2-survey-github-40f.
**Date:** 2026-04-26.

This ADR surveys the mechanisms GitHub.com offers for issuing per-agent
upstream credentials and recommends one as the **default** path for the
GitHub side of the per-agent-tokens epic. The decision binds the design of
T6 (controller token issuer) and T8 (init-clone wiring) for the GitHub
provider.

## TL;DR

**Recommendation:** Use a single **organization-level GitHub App** as the
issuer, where the controller mints **short-lived (≤1 h) installation
access tokens at agent-spawn time** scoped to the repository or repos the
agent will act on. This is the GitHub-shaped analogue of T1's "PAT issued
at spawn time" pattern: programmatic, revokable, no per-spawn user
sprawl. Fall back to a **fine-grained PAT pool against a small, fixed set
of org-owned machine users** for the rare cases that demand a real
GitHub user as the actor (commit author with verified signature, MR
"Author" column in third-party tooling that hides Apps).

The unavoidable cost — and the largest divergence from T1 — is that the
GitHub UI attributes any action taken via an installation token to
`<app-name>[bot]`, **not** to the agent name. There is no GitHub
mechanism that gives us per-spawn distinct upstream actor identities
without provisioning per-spawn upstream user accounts (which GitHub.com
does not offer a public API for). We close the attribution gap inside
GitHub by:

1. **Encoding the agent identity in the PR/issue body and commit
   trailer** at create time (e.g. `Created by agent: <bead-id> /
   <agent-name>`), so a human reading the PR sees the agent immediately
   even though the author column says `gardenio[bot]`.
2. **Emitting per-spawn audit events** to the controller so the
   `gb prs list` / future `gb tokens list` UI can answer "which agent
   opened PR #N" from our own records, not GitHub's.

Reject as v1 defaults:

- **One real GitHub user account per agent.** GitHub.com has no public
  API to programmatically create user accounts; signup is interactive,
  email-verified, and CAPTCHA-gated. Even if we automate it via
  scripted browser flows, GitHub's ToS prohibits creating fake users
  for impersonation, and on GitHub Enterprise Cloud each user account
  consumes a paid seat. This is a non-starter on SaaS and an admin
  nightmare on Enterprise.
- **Classic PATs against a single shared bot user.** This is the
  status-quo (`GIT_TOKEN`/`GIT_USERNAME` against a static
  `acme-bot`-style account); it is exactly what the epic exists to
  replace. Listed for completeness only.
- **GitHub Actions OIDC / `id-token: write`.** Bound to the GitHub
  Actions runner context; not usable from K8s agent pods running
  outside Actions. Useful only if we ever route agent runs *through*
  Actions, which we do not today.
- **SSH deploy keys.** Read-only or read/write per-repo, but they
  cannot open PRs or comment via the API and produce no actor at all
  on push (commits show by their git author, with no GitHub user
  badge unless the email matches a registered user). They solve a
  different problem.

The recommended GitHub-App path mirrors the existing per-agent
*beads-daemon* token issuer
(`gardenio/controller/cmd/controller/agenttoken.go`): generate → store as
K8s Secret → plumb via `secretKeyRef` → revoke / let-expire on done/kill.
T6 should be re-scoped as "generalise the existing issuer to the GitHub
provider" rather than "build from scratch." Concretely, the GitHub
provider's `provisionToken` calls
`POST /app/installations/{installation_id}/access_tokens` with a
per-spawn `repository_ids` and `permissions` body; the rest of the
plumbing is identical to the GitLab provider.

The biggest open operator question (filed in §6) is **whether the
attribution gap above is acceptable for v1**, or whether we want to
provision a small fixed pool of org-owned machine-user accounts (say 8)
and round-robin them per spawn so that the GitHub Author column at least
shows *a* non-default human name. The recommendation above answers
"acceptable, with PR-body annotation" — but the operator may prefer a
small pool. Both can co-exist behind the same issuer abstraction.

## 1. Constraints carried in from the codebase

From `per-agent-tokens-codebase-state.md` §1–§4 and T1 §1:

1. The init-clone path consumes exactly two GitHub credential shapes
   today (`gardenio/controller/internal/podmanager/initclone.go:109–161`):
   `GIT_USERNAME` and `GIT_TOKEN` together, written as
   `https://${GIT_USERNAME}:${GIT_TOKEN}@github.com` in
   `~/.git-credentials`. Whatever GitHub mechanism we pick must hand
   back a `(username, bearer)` pair the init script can drop into that
   file unchanged. **Installation access tokens satisfy this** —
   GitHub's documented HTTPS form for App-installation auth is
   `https://x-access-token:${INSTALLATION_TOKEN}@github.com/<owner>/<repo>.git`,
   so we set `GIT_USERNAME=x-access-token` and
   `GIT_TOKEN=<installation-token>`. Zero changes to the init shell
   script; only the secret-source changes.
2. The existing per-agent-token machinery for the beads daemon uses
   K8s Secrets named `agent-token-<beadID>`, labelled
   `gardenio.io/type: agent-token`, key `token`. The GitHub issuer
   should follow the same shape T1 proposes for GitLab, with a
   per-provider type label (`gardenio.io/provider: github`) and keys
   `token` (the bearer) plus `username` (always the literal string
   `x-access-token` for the App path; the bot username for the PAT
   pool fallback). One Secret family, one cleanup path.
3. **Two separable attribution problems** (codebase-state §1, T1 §1.3):
   - **Commit-author attribution** (git `user.email`) — `verified` on
     GitHub requires the email belong to a real registered GitHub
     user *and* the commit be signed by a key/GPG-id registered to
     that user. Installation tokens can author commits via the
     [GitHub API contents endpoint](https://docs.github.com/rest/repos/contents)
     (which produces server-side-signed `web-flow` commits attributed
     to the App), but **cannot** sign locally-authored `git push`
     commits as a real user. The PAT-pool fallback can, if we
     register signing keys against the pool's machine users.
   - **PR/issue/API-actor attribution** ("Author" column in the PR
     list, `@author` in mentions) — determined by the auth token on
     the API call. App installation tokens produce `<app-name>[bot]`;
     PATs produce the underlying user. **GitHub does not offer
     per-spawn distinct attribution without per-spawn user accounts**;
     this is the structural divergence from T1.
4. Today's GitHub-side repo footprint inside the org we use
   (`anthropics/...` for the gardenio mirror, plus any external repos
   agents touch) is narrow: in practice the agent population pushes
   to a single primary repo plus occasional reads of public repos.
   Cross-repo writes inside one spawn are rare. Like T1, this drives
   "scope-tightly-by-default" as the right policy.

## 2. The five options, side-by-side

The columns are the dimensions called out in the bead description.

| | (1) GitHub App installation token | (2) Fine-grained PAT, pool of org machine users | (3) Classic PAT against shared bot user (status quo) | (4) Per-agent real user accounts | (5) GHA OIDC → ephemeral token |
|---|---|---|---|---|---|
| **Issued via** | `POST /app/installations/{id}/access_tokens` (App-JWT-authenticated) | Manual user signup → user generates fine-grained PAT in UI; pool is hand-provisioned, then handed to controller | Manual user signup → classic PAT issued in UI; one shared token | **No public programmatic API.** Interactive signup, email verification, CAPTCHA. ToS prohibits fake users. | Issued by GitHub Actions runner via `ACTIONS_ID_TOKEN_REQUEST_*` env; not directly issuable to a K8s pod outside Actions |
| **Backing identity** | App's bot user — appears as `<app-name>[bot]` in UI; one identity for the whole org | One real machine user per pool slot (`gardenio-bot-1`, `…-bot-2`, …); finite, fixed | One real machine user shared across every agent | One real user per agent | Ephemeral; identity is the GHA workflow run, not a user |
| **Expiry / rotation** | Installation tokens expire **after 1 h** (mandatory, not configurable); App's private key rotated on a long cadence (yearly) | Fine-grained PATs require `expires_at`, max 366 d on github.com (1 y) | Classic PAT can be no-expiry (legacy) or expiring; legacy tokens widely used | Same as PATs above | Token TTL = job duration (≤ a few hours) |
| **Scope granularity** | Per-installation: subset of repos selected at install time; per-issuance: caller can narrow to a `repository_ids[]` subset and a `permissions{}` map (contents:write, pull_requests:write, issues:write, etc.). Fine-grained. | Fine-grained PAT scopes (per-repo or all-repos for the user; per-resource permissions: contents, pull_requests, issues, …). Bound to user's repo access. | Classic PAT scopes are coarse: `repo`, `workflow`, `admin:org`, etc. — full repo access if `repo` is granted | Same as PAT (whichever the user creates) | Restricted to GHA permissions; cannot open arbitrary PRs |
| **Attribution in UI** | `<app-name>[bot]` for all PR/issue/comment authorship; commits via the contents API show `<app-name>[bot]` with a server-side `web-flow` signature; locally-pushed commits show their git-author email's matching user (or "no user" if the email is unregistered) | The pool's machine user shows as PR author / commenter; per-pool-slot avatar; commits signed by the user's verified key show "Verified" | Same shape, but always the same single shared user — the problem this epic exists to fix | One real user per agent — full per-agent attribution everywhere | Workflow run / commit SHA; no per-user attribution |
| **Scaling ceiling** | App can have arbitrary installations; per-installation tokens are unbounded in count. Per-installation **secondary rate limit**: ~5,000 req/h, plus per-endpoint short-window limits. Tokens are short-lived so we re-mint constantly (cheap). | Pool size is hand-provisioned; org user limits apply (see §4). 8–16 users covers our peak. | Single shared identity; rate-limit budget shared across **every** agent — already a felt pain. | Effectively unbounded users on Free; on **GitHub Enterprise Cloud each user is a billable seat**. ToS-violating on Free for fake users. | Bounded to GHA runs |
| **API automatability** | **Full** — controller holds the App private key, mints JWT, mints installation token. Zero human steps after one-time App install. | **Partial** — pool members are provisioned by hand, then their PATs are issued by hand; controller only consumes pre-loaded credentials | Trivial — already the status quo | **None on github.com.** No public user-creation API. SCIM (Enterprise) creates users from an IDP, not arbitrary sign-ups. | Full only inside Actions |
| **Cost** | Free on github.com. Apps are first-class. | Free per machine user; Enterprise Cloud charges per seat (each pool slot = 1 seat). On Free/Team plans, no per-seat cost for org members (but org private-repo billing applies). | Free | **Cost-prohibitive on Enterprise** + ToS issue on Free | Free |
| **Revocation** | Installation tokens can be revoked early via `DELETE /installation/token`; otherwise auto-expire ≤1 h. Whole installation can be uninstalled by org admin to nuke all live tokens. | `DELETE /user/personal-access-tokens/{id}` against the user; user account stays unless deleted; admin can also revoke at org level for fine-grained PATs | Same as fine-grained PAT | Same | Auto-revoked at job end |
| **Multi-repo access** | One token per spawn, `repository_ids[]` lists exactly the repos needed (subset of installation's selected repos). Cross-repo: enumerate them in the issuance call. | A pool member's PAT covers whatever repos that user has access to (all-repos or per-repo selection). Cross-repo: trivial if user has access to each. | Single token (whatever the shared user has access to) | Same | N/A |
| **`Verified` signed commits** | API-authored commits are server-side `web-flow` signed and show `Verified` for the App. **Locally-pushed commits cannot be signed *as the App*** — only as the git-author email's user. | Pool user can register a GPG/SSH signing key; locally-pushed commits show `Verified` for that user when `user.email` matches. | Same as (2). Already the status quo. | Same | N/A |

Notes on cells worth elaborating:

- **GitHub Apps are the GitHub-shaped analogue of GitLab's
  Project-Access-Token-with-bot-user**: an org-level identity, no
  per-spawn human user, programmatic mint/revoke, scoped to
  per-issuance repos/permissions. The structural difference is
  *what GitHub displays as the actor*: GitLab's PAT auto-creates
  a *distinct* `project_NNNN_bot_xxxx` user per token, so each
  spawn looks like a different actor in the UI. GitHub's App
  installation collapses every per-spawn token back to the
  single `<app-name>[bot]` identity for display purposes. The
  isolation, lifecycle, and rate-limit benefits are the same; the
  per-spawn-actor-display win is the one thing we lose.

- **Fine-grained PATs vs classic PATs.** Fine-grained PATs (GA in
  2022, generally recommended) carry per-resource permissions
  (contents, pull_requests, issues, secrets, …) and per-repo
  scoping. Classic PATs carry coarse scopes (`repo` is binary).
  Whatever pool we run, it must use **fine-grained** PATs — classic
  PATs concentrate too much authority and GitHub is signalling
  long-term deprecation in their security comms.

- **Per-agent real user accounts is the option a naive read of the
  epic body reaches for, and it is unworkable on GitHub.** GitHub
  has no public API to create user accounts; signup requires
  email + CAPTCHA + ToS-acceptance. Creating thousands of fake
  users to back agents would also violate
  [GitHub Acceptable Use Policy §4](https://docs.github.com/en/site-policy/acceptable-use-policies/github-acceptable-use-policies#4-spam-and-inauthentic-activity-on-github)
  ("inauthentic activity"). Even if we set this aside, on Enterprise
  Cloud each account is a billable seat; on Free/Team a flood of
  fake org members is admin chaos. **Listed and rejected; no
  further design work in this direction.**

- **GitHub Actions OIDC.** GitHub's OIDC issuer hands out a JWT
  inside an Actions runner that the runner can exchange with
  external services (AWS STS, Vault, etc.). It is *not* a way for
  an arbitrary K8s pod outside Actions to obtain a per-job token to
  call back into GitHub. There are tricks to wire it the other way
  via a custom IDP (k8s SA token → cloud STS → external IDP →
  back to GitHub), but the result is still a token that *acts as
  the App or as a PAT user* — the underlying actor identity does
  not change. Useful only if we ever route agent runs *through*
  Actions, which we do not today. Documented for completeness;
  rejected as v1.

- **Deploy keys.** SSH key registered against a single repo,
  read-only or read/write **for git pushes only**. Cannot open
  PRs, cannot comment, cannot read issues. Solves a different
  problem (push-only CI worker) and is not a substitute for an
  API credential. Mentioned because the bead description asks
  about machine users and deploy keys; rejected.

## 3. Recommendation

**Default path: GitHub App, controller mints per-spawn installation
access tokens scoped to the target repo(s) for the spawn.**

Issuance flow at agent-pod creation, by analogy to T1 §3 and the
existing beads-daemon issuer:

1. Controller resolves the **target repo(s)** for the spawn from the
   agent bead (the role/project bead's secrets list and any
   per-spawn override). For most spawns this is exactly one repo
   (e.g. `acme/gardenio`); for cross-repo spawns it's a small
   list (e.g. `[gardenio, acme-helm-chart]`).
2. Controller authenticates as the GitHub App via the App's private
   key (mint App-JWT — RS256 signed, ≤10 min lifetime), then calls
   `POST /app/installations/<installation_id>/access_tokens` with
   body
   ```json
   {
     "repository_ids": [<numeric-ids>],
     "permissions": {
       "contents": "write",
       "pull_requests": "write",
       "issues": "write",
       "metadata": "read"
     }
   }
   ```
   The response carries `token` (one-time-visible bearer, **expires
   in exactly 1 h**) and `expires_at`.
3. Controller writes a K8s Secret `agent-github-token-<beadID>`,
   labels `gardenio.io/type: agent-token`,
   `gardenio.io/provider: github`, keys `token` (the installation
   bearer), `username` (literal string `x-access-token`).
4. Pod-spec's `spec.GitCredentialsSecret` points at this Secret
   instead of the global `gardenio-github-token`. The init-clone
   shell script
   (`gardenio/controller/internal/podmanager/initclone.go:109–123`)
   already writes
   `https://${GIT_USERNAME}:${GIT_TOKEN}@github.com` from those two
   env vars — no init-script change is needed; the Secret reference
   simply points at a per-agent Secret.
5. **1-hour TTL handling.** Installation tokens expire at the wall
   clock time in `expires_at`. For long-running spawns:
   - **Option A (recommended for v1):** controller proactively
     re-mints the token on a 50-min cadence and **rewrites the K8s
     Secret in place**. The agent pod's `glab`/`gh`/`git push`
     calls re-read the credential helper file on each invocation
     (the credentials-helper file is re-read on each git
     operation), so a Secret rewrite propagates. For `gh` API
     calls that hold the token in a process env var, we
     additionally need a small refresh path — see §3.5.
   - **Option B:** controller mints with a per-spawn deadline
     equal to the bead TTL, accepting that spawns >1 h will see
     mid-run token expiry. Simpler; brittle for long sessions.
   - **Option C:** issue an installation token on every API call
     (controller-mediated proxy). Heavy controller dependency for
     the agent's outbound traffic. Rejected for v1.
6. On done/kill, controller calls
   `DELETE /installation/token` against the live token and deletes
   the K8s Secret. Best-effort + janitor-loop sweep, exactly as the
   existing beads-token issuer does.

### 3.1 Why a single org-level App and not multiple Apps

A natural reach is "one App per agent role" so PR-author at least
shows `gardenio-gardener[bot]` vs `gardenio-crew[bot]`. We rejected
this for v1:

- Each App is a separate GitHub App registration with its own
  private key, install on each repo, and rate-limit budget. Adding
  a new role would require a manual GitHub admin step.
- The attribution win is small (a half-dozen App identities, not
  per-agent), and we already need a fallback path (PR body
  annotation) to identify the agent within a role.
- One App keeps the controller plumbing simple: one private key
  Secret, one installation ID, one rate-limit pool to monitor.

Per-role Apps are a *future* refinement that the same issuer
abstraction can grow into; not a v1 requirement.

### 3.2 Machine-user PAT-pool fallback for "needs a real user"

A small subset of agent operations *do* need a real GitHub user as
the actor:

- **Verified-signature commits.** The App can author commits
  server-side via the contents API (`web-flow`-signed by
  `<app-name>[bot]`) and those show `Verified`. But locally-pushed
  commits — which is what `git commit && git push` produces — are
  authored under the agent pod's `user.email`, and that email needs
  to belong to a registered GitHub user with a registered signing
  key for the green Verified badge.
- **Tools that don't render App actors.** Some third-party PR
  dashboards / merge bots / CODEOWNERS UIs treat
  `<app-name>[bot]` differently from a human user (e.g. branch
  protection rules that require human review).
- **Public-repo PRs from forks.** Apps can write to private repos
  inside their installation but can't open PRs against repos they
  aren't installed on; for occasional contributions to public
  upstreams, a real user is needed.

Proposal: the controller maintains a **fixed pool of N=8 org-owned
machine users** (e.g. `acme-ag-{1..8}`), each with a
fine-grained PAT covering the org's repos with `contents:write`,
`pull_requests:write`, `issues:write`. Pool tokens are issued by
hand at pool-bootstrap time (one-time), stored in K8s Secrets, and
the controller leases them per-spawn (one slot in use at a time
per agent that opts in). Agent specs opt in via
`spec.GithubTokenScope = "user"`; default remains the App path.

Sizing rationale: 8 covers our peak concurrent agent count for
"needs-a-real-user" spawns with comfortable headroom; cheap to
run on Free/Team org plans. On Enterprise Cloud, 8 seats × per-seat
cost is the recurring price (call-out in §4).

### 3.3 Commit-author attribution side-effect

Today the agent pod sets git `user.email` to `<role>@gardenio.local`
and `user.name` to the agent name (codebase-state §1.3). With the
App path, the *cleanest* git-author setup is:

- `user.name`: the agent name (e.g. `gardenio-t2-survey-github-40f`)
  — informational; surfaces in commit history but not as a GitHub
  user link.
- `user.email`: `<installation-id>+<app-name>[bot]@users.noreply.github.com`
  — GitHub's documented App noreply email format. This binds
  the local commit's git-author to the App's bot user, so commits
  show with the App's avatar in the PR diff view.

With the PAT-pool fallback path:

- `user.name`: pool member's display name.
- `user.email`: pool member's `@users.noreply.github.com` address
  (or a verified email registered to that user).
- If the pool member has a registered signing key, `git commit -S`
  produces a Verified commit; otherwise commits are author-attributed
  but unsigned.

T9 (the "Phase 0 win" of fixing the synthetic email) folds into
both paths: once the issuer runs, the K8s Secret carries the
correct email and `gb agent start` writes it into git config.

### 3.4 Scope minimisation

The recommended permissions block in §3 step 2 grants the union of
"open PRs, push commits, comment on issues, read repo metadata". A
v1.1 refinement is **per-role permission templates**:

- Research roles (intake, survey): `metadata:read`, `contents:read`,
  `pull_requests:read`, `issues:write` (commenting only).
- Delivery roles: full set above.
- Release roles: add `actions:read` for CI status + `contents:write`
  for tag creation.

Out of scope for v1; recorded for the future issuer.

### 3.5 Token refresh propagation

Installation tokens expire after exactly 1 h; the controller will
re-mint and rewrite the K8s Secret on a 50-min cadence (§3 step 5,
Option A). Two propagation paths to think about:

- **`git` invocations.** Use the credentials-helper file
  (`/tmp/.git-credentials`); each `git push` re-reads the file. If
  the controller has rewritten the Secret, the kubelet will
  eventually project the new content to the pod's mounted Secret
  (default ~60 s sync, configurable). The init-clone script writes
  the credentials-helper file from `$GIT_USERNAME` and `$GIT_TOKEN`
  *once at init time*. To make refresh work for the long-spawn
  case, the **main agent container** needs a small loop (or an
  on-demand `gb token refresh github` command) that re-renders
  `/tmp/.git-credentials` from the env-projected Secret on a
  schedule. This is one ~30-line shell helper, not new
  infrastructure.
- **`gh` / `glab` / app-level API calls.** The `gh` CLI reads
  `GH_TOKEN`/`GITHUB_TOKEN` env vars at command-invocation time.
  As long as the env-vars are sourced from the K8s Secret via
  `secretKeyRef`, the *next* `gh` invocation picks up the new
  value automatically once kubelet rotates the projected secret.
  No agent-side code change needed.

The 50-min mint cadence + ~60-s kubelet sync gives a
worst-case-stale window of ~10 min, well inside the 60-min token
lifetime. Define the controller's mint cadence as
`floor(token_ttl × 0.8) − kubelet_sync_window`.

## 4. Sizing and rate considerations

- **Token count at peak.** Today's agent population spawns at
  roughly the order of dozens-to-low-hundreds per day. Per-spawn
  installation-token mint is one API call per spawn plus one every
  50 min for long spawns; well under the App's documented
  `5,000 req/h` rate limit even at 10× our current spawn rate.
- **Rate-limit attribution.** GitHub's rate limit for App
  installations is **per-installation**, not per-token. All our
  agents share one installation (the org-level install) so they
  share one 5,000-req/h budget. This is **less isolated** than
  T1's GitLab PAT path (where each PAT has its own bucket). Two
  consequences:
  1. Agent-side API code should be polite (cache, paginate
     judiciously).
  2. The bridges and the controller itself should run as their
     own App or a separate installation if traffic justifies — do
     not share rate-limit budget with the agent pool when the
     bridge can spike (e.g. webhook-driven backfills).
  Higher-tier GitHub plans grant elevated installation rate
  limits (Enterprise Cloud: 15,000 req/h per installation per
  plan tier as of 2024 docs; verify current value before sizing).
- **Token-rotation cadence.** Hard 1-h TTL on installation tokens
  is non-negotiable; the controller's mint loop is mandatory for
  spawns >50 min. The pool-PAT fallback uses the same long-cadence
  rotation we'd run for any long-lived PAT (~30 d).
- **Rate-limit isolation per pool member.** Fine-grained PATs
  count against the *user's* primary rate limit (5,000 req/h),
  not the App's. The PAT pool gives us per-pool-slot isolation as
  a side-effect.
- **Storage cost.** One K8s Secret per active agent — additive to
  the existing `agent-token-<beadID>` Secret created by the
  beads-daemon issuer and the GitLab Secret from T1. Per-agent
  Secret count goes from 1 → 3 (beads + GitLab + GitHub) once
  T1+T2 land.

## 5. Migration & co-existence with the existing static token

- Today's `gardenio-github-token` Secret (in pseudocode; the
  current Secret name is whatever the project bead's
  `secrets[]` declares for `GIT_TOKEN`/`GIT_USERNAME`) backs every
  agent. The App path replaces this on a **per-pod** basis: if
  the controller has successfully provisioned a per-agent
  installation token, the pod's `spec.GitCredentialsSecret`
  points at the per-agent Secret; if provisioning fails, the
  controller can choose to (a) refuse to start the pod, or
  (b) fall back to the shared token under a flag. Recommend
  (a) once the rollout is past staging — failing closed is the
  right default for a credential-issuance system.
- The legacy shared GitHub PAT should remain provisioned in the
  controller's namespace for the bridges (`gitlab-bridge`, etc.,
  and any GitHub bridge) and for the controller's own GitHub API
  calls *until the App is healthy*. Once the App-JWT path is in
  prod, the controller's own GitHub identity should switch to
  the App as well (the App can authenticate as itself for org-
  scope reads and as an installation for repo-scope writes). The
  legacy PAT becomes a break-glass-only Secret.
- **In-flight PRs authored by the legacy shared bot user cannot
  be retroactively re-attributed.** GitHub's PR-author field is
  immutable post-creation. Existing PRs will continue to show the
  static account as `Author`; new PRs after cutover show
  `<app-name>[bot]` (or pool member). The cutover plan should
  keep the legacy identity as a valid commenter on pre-existing
  PRs until they merge or close — concretely, do not revoke the
  legacy PAT until the in-flight backlog drains.
- **App installation must be performed by an org admin** (one-
  time, manual click-through). The App is created in the org's
  Developer Settings, then installed on the relevant repos
  (either "all repos" or a curated list). This is operator toil
  but it happens *once*; the per-spawn path is fully automated
  thereafter.

## 6. Open questions for operator review

These are decisions the human operator must make before T6
implementation begins. Each is small enough to handle as a single
`gb decision` once T1–T5 are all in. Per the bead AC, surface
these here for batching with T1's seven decisions plus T3/T4 once
those land.

1. **Is the `<app-name>[bot]` actor display acceptable for v1?**
   Recommendation: yes, paired with a PR-body annotation
   identifying the agent. Alternative: provision the
   8-machine-user PAT pool *as the default* and accept the
   per-seat cost on Enterprise / the manual provisioning toil
   on Free. The trade is "GitHub UI shows agent role" vs
   "GitHub UI shows the App + PR-body shows the agent."
2. **GitHub plan tier.** What plan is our GitHub org on? This
   affects (a) per-seat cost of any PAT pool, (b) whether we
   have access to fine-grained PAT enforcement at org level,
   (c) installation rate-limit ceiling, (d) availability of
   org-level Apps + secret manager. Recommendation does not
   require Enterprise; if we are on Enterprise, the rate-limit
   budget is 3× larger and the PAT-pool seat cost is the
   binding constraint.
3. **PAT-pool size.** If we ship the pool fallback (§3.2),
   what's the right N? Recommendation: 8. The pool is sized for
   concurrent "needs-a-real-user" spawns, not per-day count.
4. **Per-spawn installation-token TTL handling.** Recommend
   Option A (controller re-mints every 50 min, rewrites Secret).
   Operator may prefer Option B (one-shot per-spawn-deadline
   token, accept >1 h spawn breakage) for simplicity at the
   cost of some long-running spawns.
5. **Failure-mode policy.** If GitHub API rate-limits the issuer
   or the App-installation-tokens endpoint is down at spawn time,
   do we (a) fail the pod, (b) start the pod with no GitHub
   token (any `gh` / `git push` will fail), or (c) fall back to
   the shared PAT for that spawn under a metric? Recommend (a)
   for prod, (c) for staging-only with the metric loud enough to
   alarm. Mirrors T1 §6.4.
6. **Installation-token signing-commits.** Do we want to invest
   in either (a) routing all commits through the API contents
   endpoint (server-side `web-flow` signed by the App) or (b)
   pool-member commits only (locally signed via registered key)
   for the Verified-badge UX? Recommendation: defer; ship
   unsigned commits in v1 and add signing as a v1.1 follow-on
   once the issuer is in prod.
7. **Per-role Apps.** Should we register one App per agent role
   (so the actor display is `gardenio-gardener[bot]` vs
   `gardenio-crew[bot]`) instead of a single org App? Recommend
   no for v1 (one-time admin toil per role; small attribution
   win over the PR-body annotation). Easy to add later under
   the same issuer abstraction.
8. **Public-upstream-PR support.** Do we need agents to open PRs
   against repos *outside* the org's installation (e.g. forking
   a public OSS upstream and PR-ing back)? If yes, only the
   PAT-pool path works (Apps can't impersonate users to open
   cross-org PRs from forks). Today this is rare; recommend
   defer until a real spawn needs it.

## 7. Pointers (for the T6/T8 implementer)

| Topic | Where to look |
|---|---|
| Existing per-agent token issuer (the pattern to extend) | `gardenio/controller/cmd/controller/agenttoken.go` |
| Issuer call sites (will need a parallel set for the GitHub provider) | `gardenio/controller/cmd/controller/main.go:804,835,865,870` |
| Where `GIT_TOKEN`/`GIT_USERNAME` enter the pod spec today | `gardenio/controller/cmd/controller/podspec.go:585–624` (project), `:638–691` (role) |
| Init-clone consumer of `GIT_TOKEN`/`GIT_USERNAME` | `gardenio/controller/internal/podmanager/initclone.go:109–161` |
| `PER_AGENT_TOKENS` flag and config plumbing | `gardenio/controller/internal/config/config.go:189,552` |
| Helm value the parent enablement plan flips today | `gardenio/helm/gardenio/values.yaml` (`agents.perAgentTokens`) |
| GitHub docs: GitHub App auth (App-JWT) | `https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/about-authentication-with-a-github-app` |
| GitHub API: create installation access token | `https://docs.github.com/en/rest/apps/apps#create-an-installation-access-token-for-an-app` |
| GitHub API: revoke installation token | `https://docs.github.com/en/rest/apps/installations#revoke-an-installation-access-token` |
| GitHub API: fine-grained PATs | `https://docs.github.com/en/rest/users/personal-access-tokens` |
| GitHub HTTPS clone form for App tokens (`x-access-token`) | `https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/installing-a-github-app-using-the-rest-api#http-based-git-access-by-an-installation` |
| GitHub Acceptable Use Policy (re: fake users) | `https://docs.github.com/en/site-policy/acceptable-use-policies/github-acceptable-use-policies` |
| Predecessor (codebase state) | `gardenio/docs/research/per-agent-tokens-codebase-state.md` |
| Predecessor (T1 GitLab survey) | `gardenio/docs/research/per-agent-tokens-gitlab.md` |
| Parent enablement plan (beads-daemon side) | `gardenio/docs/per-agent-tokens-enablement-plan.md` |

## 8. What this ADR does NOT decide

- The **token-lifecycle design** (when issued, where stored, how
  revoked at scale, refresh strategy in production code) — that's
  T4. This doc only sketches the per-spawn case + the 1-h-TTL
  refresh to make the survey concrete.
- **Provider-inventory v1 scope.** T3 covers Jira and any other
  shared-token surfaces. The recommendations here will likely
  generalise (issuer abstraction over App / OAuth / PAT-pool
  shapes), but the per-provider details are out of scope.
- **Attribution-verification tests.** T5 defines the automated
  test that proves end-to-end attribution works (spawn agent →
  open PR → assert author from API). The shape of that assertion
  on the GitHub side is "PR `user.login` matches
  `<app-name>[bot]` *or* the leased pool member, and PR body
  contains `Created by agent: <bead-id>`."
- **Per-role Apps.** Open question §6.7. Not v1.
- **Whether to ship the PAT pool in v1 or defer to v1.1.** Open
  question §6.1. Recommend ship the App path in v1 with a stub
  for the pool path; light it up only when a spawn opts in or a
  signed-commit need lands.
