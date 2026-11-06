# GitHub App onboarding for gardenio — plan of record

**Status:** D1–D7 ruled 2026-09-21 (§7). **P0 done 2026-09-21; P1 done 2026-09-22.** P2–P5 are not
authorised and no epic is minted. Org name chosen:
**`gardenio-io`**. P0's unblocked half is **done** — CMK, SSM write path, registration tooling,
rotation runbook and the `type:credential` inventory all landed 2026-09-21. The remaining half is
blocked on a single operator click: **GitHub has no API for creating a github.com org**, so
`gardenio-io` must be created by hand at `https://github.com/organizations/plan`. Everything after
that is one command.
**Authored:** 2026-09-21, laptop-claude (`fimba-7c`). Research drafted by a subagent; every
load-bearing claim below was re-verified against the tree, the live GitHub API and the bead
store before this file was committed. §10 lists what remains unverified.
**Goal:** replace hand-minted PAT-in-a-Secret GitHub wiring with a GitHub App whose onboarding
is "click Install, pick repos, done."

**Forcing function:** the fine-grained PAT every agent mounts today, `gardenio-crew-contents`,
**expires 2026-10-19 — 28 days from this writing** (`gd-QoziM1L954`). Either P1/P2 land before
then, or the operator hand-mints another PAT and the clock resets for a year. That is the
concrete cost of deferring this.

---

## 0. The headline finding, first

**The design already exists and is already ADR'd. It was never built, and its tracking beads are dead.**

`gardenio/docs/research/per-agent-tokens-github.md` (561 lines, authored 2026-04-26 in the ACME
era, last touched only by the gasboat→gardenio rename `d4e2d122`) recommends, verbatim at `:19-27`:

> **Recommendation:** Use a single **organization-level GitHub App** as the issuer, where the
> controller mints **short-lived (≤1 h) installation access tokens at agent-spawn time** scoped
> to the repository or repos the agent will act on.

That is the correct answer and this plan **inherits it rather than re-deriving it**. What the ADR
does *not* cover — and what was actually asked for — is the **onboarding flow**. The ADR's only
word on installation is `:456-461`: "App installation must be performed by an org admin (one-time,
manual click-through)… This is operator toil but it happens *once*." That sentence is where the
question begins, not ends.

Three corrections to that ADR before anything is built (all verified in code, details in §2 and §9):

1. Its §3.5 refresh story is **wrong**: it says rotated Secret values reach the pod through
   `secretKeyRef` env vars. They do not — env vars are frozen at pod start. It is also moot,
   because the code moved to file projection after the ADR was written.
2. The controller RBAC gate for per-agent token Secrets grants `create, delete` but **not
   `update`** (`gardenio/helm/gardenio/templates/controller/rbac.yaml:22-27` — verified). "Rewrite
   the Secret in place every 50 min" cannot work as written.
3. It assumes a GitHub **organization**. `joyousaversion` is a **User account on the Free plan**
   (verified live: `gh api users/joyousaversion` → `{"login":"joyousaversion","plan":"free","type":"User"}`).
   Several of the ADR's §6 open questions (per-seat cost, org-level fine-grained PAT enforcement,
   Enterprise rate-limit tiers) are therefore moot or differently-shaped.

Every bead ID in that ADR and its five sibling docs is a **dead `ka-` reference** pointing at
`beads-viewer.gardenio.e2e.example.com`, a host that no longer exists. Specifically dead:
`ka-0000004236` (epic), `ka-0000000783` (T2/this ADR), `ka-0000003589` (T1 GitLab),
`ka-0000007455` (T3), `ka-0000005384` (T4), `ka-0000008147` (T5). Treat the docs as design
records; treat the IDs as litter.

---

## 1. What we are trying to beat: the GitLab integration

GitLab/RWX CI was deleted in `8a3eb29e` (2026-09-16). The *application* integration was not
deleted and is still in the tree, dormant (`gardenio/helm/gardenio/templates/gitlab-bridge/`,
`gitlabBridge.enabled: false` at `gardenio/helm/gardenio/values.yaml:14013`).

**What GitLab onboarding actually looked like, end to end. Not one step of it was self-serve:**

| Leg | How it was done | Citation |
|---|---|---|
| CI credentials | Human types `RWX_ACCESS_TOKEN`, `RWX_MR_TOKEN`, `GITLAB_CLONE_TOKEN` into GitLab project CI/CD variable settings | `8a3eb29e^:.gitlab-ci.yml:196, :481-489, :269` |
| Webhooks | Registered **by hand in the GitLab project UI**. No code anywhere in the tree or history calls `POST /projects/:id/hooks` | `gardenio/docs/runbooks/devops-798-staging-env-setup.md:115-117` |
| Webhook auth | Shared static secret in an `X-Gitlab-Token` header, constant-time compared, fail-closed on empty. **No body HMAC** — GitLab offers none | `gardenio/controller/internal/bridge/gitlab_sync.go:848-862` |
| Bot identity | Human creates a bot user, mints a PAT with `api` scope, adds it as Developer/Maintainer, and adjusts branch protection by hand ("gardenio does NOT create branch-protection rules for you") | `gardenio/docs/onboarding-new-project-and-role.md:153` |
| Per-project agent creds | Hand-create an AWS Secrets Manager entry → hand-edit Terraform `local.p44_oauth_bootstrap_secret_arns` to grant ESO read → read-modify-write the project bead's `secrets[]` array | `gardenio/docs/onboarding-new-project-and-role.md:84-143` |
| Human operator login | `scripts/onboard-mac.sh:81-91` literally prompts "Paste your Acme gitlab.com PAT" | same |
| OAuth redirect URIs | Manual two-pass append by a human with admin on a **shared** `gloas-*` OAuth app, used by both prod and the customer POC | `gardenio/terraform/modules/edge-auth/README.md:22`; decision D-11 in `gardenio/plans/customer-poc-aws-poc.md:393` |

The runbook's own section heading is **"Step 3 — Wire git/GitLab credentials (the part that
silently fails)"** (`gardenio/docs/onboarding-new-project-and-role.md:69`), with at least four
documented silent-failure modes: unprefixed secret names are dropped; a missing ESO IAM grant
makes spawns "defer forever with no operator-visible signal"; a first spawn before ESO sync
silently defers; and a read-only token "clones but **silently cannot push or open MRs**."

**What GitLab did well, and we should keep:**

- Group membership was the *single* authorization primitive — no separate user directory
  (`values.yaml:13891-13945`, oauth2-proxy `--gitlab-group=`).
- First login auto-provisioned a `type:user` bead with `auth_methods: gitlab-oauth`
  (`kbeads/internal/server/user_provision.go:146-148` — verified). This is the one genuinely
  automatic piece of onboarding gardenio has ever had, and the GitHub flow should reproduce it.
- Edge auth failed **closed** on provider outage with an explicit audit reason
  (`deny:gitlab-unavailable`, `gardenio/controller/internal/edgeauth/audit.go:45`).
- The chart **refuses** plaintext secrets in values and forces a pre-existing K8s Secret reference
  (`templates/gitlab-bridge/deployment.yaml:86-88, :96-98`).

**The bar to beat, stated plainly:** GitLab required a human to visit four systems (GitLab
settings, AWS Secrets Manager, Terraform, a bead field) and hand-register a webhook. A GitHub App
reduces the webhook leg to **zero** (the App has one webhook URL; installing it starts delivery
with no per-repo registration) and the credential leg to **one click**. That is not a marginal
improvement; it removes two entire classes of silent failure.

---

## 2. How GitHub auth works in gardenio TODAY

### 2.1 Agent pods

Credentials are **file-projected, not env vars**, since the W6 hardening:

- The controller resolves `spec.GitCredentialsSecret` from the project bead's `secrets[]` array,
  keyed purely off the **env var name string** `GIT_TOKEN` / `GIT_USERNAME` — there is no explicit
  "this is the git credential" field (`gardenio/controller/cmd/controller/podspec.go:1460-1465`
  for project, `:1597-1602` for role). Secret names must be prefixed `{project}-` or `{role}-` or
  they are silently skipped (`:1437-1443`).
- That Secret is mounted as a **projected volume** at `/etc/git-creds/github/{username,token}`
  (`internal/podmanager/volumes.go:114-147`, mount at `:304-310`).
- The init container reads those files and materializes `/tmp/.git-credentials` **once**
  (`internal/podmanager/initclone.go:167-185`). The comments at `:129-135` and `:321-326` are
  explicit that env vars were deliberately removed because they leak via `/proc/<pid>/environ`,
  `ps eww` and `kubectl describe pod`.
- The main agent container authenticates the `gh` CLI by **writing `$GH_CONFIG_DIR/hosts.yml`
  directly**, reading the same projected file, deliberately avoiding a network round-trip to
  validate (`cmd/gb/agent_k8s_workspace.go:690-743`).

**A gift for the App design, already in the code:** `agent_k8s_workspace.go:709-712` already
defaults the username to the literal string `x-access-token` when the Secret carries no `username`
key (verified) — exactly GitHub's documented HTTPS form for installation-token auth. The live
Secrets already carry `username=x-access-token` explicitly (`gd-QoziM1L954`). **The agent side
needs zero changes to consume an installation token.**

**Both consumers are one-shot materializations.** The projected volume *does* re-project on Secret
change (kubelet, ~60s), but `/tmp/.git-credentials` is written once in init and `hosts.yml` is
written once at agent start. A rotated token will therefore **not** reach a running agent without
a new refresh helper. This is the real refresh gap, and it is not what the ADR described.

Agents open PRs by shelling out to `gh pr create` inside the pod. There is no Go-level GitHub PR
client; `cmd/gb/mr.go` is GitLab.

### 2.2 What the shared token actually is

The tree cannot answer this — nothing in the repo creates these Secrets, and gardenio-k3s has no
ESO. **The bead store can**, and it does. From `gd-QoziM1L954` (closed P0 decision, resolved
2026-09-19) and `gd-NnaRPh0AY0`:

| Fact | Value | Source |
|---|---|---|
| Token name | `gardenio-crew-contents`, fine-grained | `gd-QoziM1L954` comment |
| Expiry | **2026-10-19** | same; tracked-as-prose was itself decision `gd-NnaRPh0AY0` |
| Grant | Contents read-write on `mattbakerconsulting/gardenio` and `mattbakerconsulting/gascity-hq`; Metadata read-only. **Nothing else** | same |
| Which Secrets | `gardenio-git-credentials` **and `gascity-git-credentials`**, both in ns `gardenio`, username `x-access-token` | same |
| Predecessor | A **full-scope classic PAT** (`admin:org`, `workflow`, `repo`, `write:packages`, …) that every crew mounted until 2026-09-19 | same |
| Is the old classic PAT revoked? | **No.** "still valid and unused by the cluster… also on disk at `~/.gh-token-ja` for gh CLI use, so revoking it will break `gh` until replaced" | same |
| ghcr / ECR pull-through | Rotated onto a **separate `read:packages`-only token** at the same time | same |

Two corrections to the research draft this plan is built from: the second GitHub Secret is
`gascity-git-credentials`, **not** `gardenio-github-token`; and the checks-API 403 is a live,
open decision (`gd-4j5m4jtW34`), not a guess.

**The verification lesson from that bead, which P1 and P2 acceptance criteria must use:** GitHub's
repo `permissions` object reports the **authenticated user's** role on the repo, not the token's
grant, and reported `push=true admin=true` on all three repos including ones the token could not
write. Probe `git-receive-pack` instead. Reading the grant form is not verification.

**Consequence of sharing:** one project-level Secret backs every agent pod for that project
(`podspec.go:1462`); there is no per-agent issuance for GitHub at all
(`cmd/controller/tokenmetrics.go:30` still says GitHub "**will** reuse the same metrics" — future
tense). Every commit, push and PR from every crew, every gardener and both laptop sessions
collapses to one GitHub actor. Revocation is all-or-nothing: revoking it stops every agent and
both laptops at once.

### 2.3 Every other place GitHub identity is needed

| # | Consumer | Credential today | Location | Minimum scope truly needed |
|---|---|---|---|---|
| 1 | Agent pods: clone, push, `gh pr create` | shared fine-grained PAT (§2.2) | `gardenio-git-credentials`, `gascity-git-credentials` → `/etc/git-creds/github/*` | `contents:write`, `pull_requests:write`, `metadata:read` |
| 2 | ARC runner scale sets ×6 (4 repos; the three gardenio tiers share one `githubConfigUrl`) | **one shared classic PAT with near-total ACCOUNT scope** (see correction below) | `gardenio-github`/`github_token` in ns `arc-runners`, from SSM `/gardenio/ci/github-token`; `gardenio/helm/ci/README.md:18-20`, `arc-runners-values.yaml:5-9` | `administration:write` + `metadata:read` per repo |
| 3 | Workflow jobs (`ci.yml`, `checks.yml`) | ephemeral `GITHUB_TOKEN`, **no `permissions:` block declared** | `.github/workflows/ci.yml`, `checks.yml` | `contents:read` only — currently inheriting the repo default |
| 4 | `images.yml` ghcr push | ephemeral `GITHUB_TOKEN` | `.github/workflows/images.yml:291-293, :357-359` | `packages:write` + `contents:read` (already correct) |
| 5 | `retry-lost-runners.yml` | ephemeral `GITHUB_TOKEN` | `.github/workflows/retry-lost-runners.yml:100-103` | `actions:write`, `checks:read`, `contents:read` (already correct) |
| 6 | Cluster image pulls (live path) | **AWS ECR token via IMDS — no GitHub credential** | `gardenio/helm/gardenio-ziti/ecr-token-refresh.yaml:153-186`; registry at `deploy-values/gardenio-k3s.yaml:50` | n/a |
| 7 | ECR pull-through cache → ghcr.io | GitHub user + `read:packages`-only token in AWS SM `ecr-pullthroughcache/ghcr-gardenio` | `gardenio/terraform/modules/ecr-pull-through-cache/main.tf:87-100` | `read:packages` |
| 8 | `ghcr` fallback imagePullSecret | same `read:packages` token, **hand-created, nothing in the repo creates it** | `deploy-values/gardenio-k3s.yaml:27-28` (comment), consumed `:41-43` | `read:packages` |
| 9 | Agent PR merge | **nothing — advisory-only, run by hand** | `github_pr_merge` hits the `default` branch with no executor: `docs/research/escalation-review-loop-as-is-2026-09-20.md:98-104`; auto-close list `kbeads/internal/server/auto_close.go:134` | would be `pull_requests:write` + `contents:write` |

Nothing in Terraform creates or rotates any GitHub credential: there is no `provider "github"` and
no `github_*` resource anywhere under `gardenio/terraform/`.

**Correction, 2026-09-21 — all three long-lived tokens were probed directly.** The table above was
written from repo text; the live tokens say something worse. Each row below is a
`GET https://api.github.com/user` response header, now recorded as a `type:credential` bead (§8 P0):

| Token | Live finding | Bead |
|---|---|---|
| Row 2, ARC | Not "admin on 4 repos". `x-oauth-scopes` = `admin:enterprise, admin:gpg_key, admin:org, admin:org_hook, admin:public_key, admin:repo_hook, admin:ssh_signing_key, audit_log, codespace, copilot, delete:packages, gist, notifications, project, repo, user, workflow, write:discussion, write:network_configurations, write:packages` — near-total account authority held by six scale sets that need two per-repo grants. Same class as `gd-QoziM1L954`. Expires **2026-12-14 20:03 UTC**. | `gd-OX3Px0lTm6` |
| Rows 7+8, ghcr | `read:packages` only, and **classic** — which settles §10 item 6, since fine-grained tokens emit no `x-oauth-scopes` header at all. Expires **2026-10-19 06:25 UTC**. | `gd-swfgrU5atw` |
| Row 1, crew PAT | Expiry confirmed **2026-10-19 09:29 UTC**; no `x-oauth-scopes`, so fine-grained as recorded. Both Secrets hold the byte-identical token, username `x-access-token`. | `gd-XCUtYCxe6N` |

**2026-10-19 is a two-credential cliff, not one.** The crew PAT and the ghcr token lapse the same
day about three hours apart, so agent pushes and image pulls through the ECR cache would fail
together. The ghcr token is also the credential behind the *fallback* pull secret, so the fallback
shares its expiry with the path it backs up. Nothing in the repo or the memory record said this.

### 2.4 What already exists that this plan must NOT re-propose

All verified in code today:

- **A per-run credential mint → deliver → revoke pipeline is already implemented**, for GitLab, on
  the Lambda MicroVM path: `cmd/controller/microvm_git_credentials.go`. It mints a project-scoped
  token at launch (`Mint`, `:152`), delivers it only via a per-run task-scoped document, and
  **revokes on all four exit paths** — normal termination, timeout, failed launch, leak GC
  (`Revoke` at `:197`, `microvmCredentialRevoker` at `:386-404`). The GitHub version of this file
  is the same shape with `POST /app/installations/{id}/access_tokens` in place of
  `POST /api/v4/projects/{p}/access_tokens`. **This file is the template; do not design a new one.**
- **A per-agent token issuer already exists** for the beads daemon:
  `cmd/controller/agenttoken.go` — generate → register → K8s Secret → revoke on done/kill, with
  TTL and refresh constants. Its RBAC gate is already in the chart behind `agents.perAgentTokens`
  (`rbac.yaml:22-27`), currently `false` (`values.yaml:3951` — verified).
- **Short-lived-credential machinery with refresh is shipped in kbeads** — `POST /v1/auth/refresh`,
  token expiry enforced in both the gRPC and HTTP interceptors, subject claims, persistent
  hash-based revocation. (The design doc `docs/research/c1-token-ttl-refresh-design.md:3` still
  says "No implementation"; that line is **false today**.)
- **A complete OAuth2 PKCE + device-grant implementation already exists** at
  `controller/internal/oauth/oauth.go` (`NewPKCE`, `RequestDeviceCode` at `:198-262`) — and it is
  **dead code**: the package and its own test file are the only files in the repo that touch it,
  with **zero importers anywhere** (verified). It is GitLab-shaped, but the PKCE/state/callback
  scaffolding is reusable for the GitHub user-authorization leg.
- **The viewer already has a mature operator-gated write surface**:
  `oidcauth.RequireRole(oidcauth.RoleOperator)` guarding ~30 POST routes
  (`cmd/beads-viewer/server.go:1693-1889`), plus `GET /login` (`main.go:396`) and
  `GET /auth/cli-exchange` (`:404`). A setup-callback route is an addition to an existing pattern,
  not new infrastructure.
- Both plan docs **already name the GitHub App as acknowledged debt**:
  `gardenio/helm/ci/arc-runners-values.yaml:7` ("Debt: replace the PAT with a GitHub App (needs an
  interactive install click)") and `gardenio/plans/self-hosted-ci-runner.md:57-62`, which already
  specifies the exact permission set — "repo-scoped: Actions R/W, Administration R/W, Metadata R…
  store app id / installation id / private key in **SSM Parameter Store**."

### 2.5 Two operator rulings this plan inherits

Found in the bead store, both closed, **both unimplemented**. They are binding, not advisory.

- **`gd-OICMBdBfIo` — chosen: `shared-ledger`.** "Build one shared, append-only `capability_grants`
  ledger now… that every credential-issuing path writes to, rather than letting each in-flight
  design (taints, escalation policy, account assignment, ziti leases, git-token rotation) build its
  own observability mechanism separately." The rejected option was explicitly `per-domain`.
  **The GitHub App minter is a new credential-issuing path.** P4 therefore writes to that ledger;
  it does not invent a GitHub-specific audit mechanism. `grep -r capability_grants` returns nothing
  — the table does not exist yet (latest migration on main is `047_taint_registry`; `048` is
  claimed by PR #67 and `049` by `gd-J7Wqmqfnpq`, so this is **050 or later**).
- **`gd-NnaRPh0AY0` — chosen: `structured-field`.** Record `token_name, scopes, minted_at,
  expires_at, secret_ref` in a queryable bead field so expiries are alertable. There is no
  `credential` bead type (`kd list -t credential` → "unknown bead type"), so this was never built.
  P0's credential inventory should *be* that record, not a doc section.

---

## 3. The gap

Everything above says the same thing: **gardenio has all the parts and none of the assembly.**
The gap is not "we lack a design" or "we lack primitives." It is three concrete things:

1. **No App exists.** No `app_id`, `installation_id` or private key anywhere in the repo, cluster
   config or Terraform. Every GitHub credential in the system is a human-minted, human-pasted
   PAT with no rotation automation and no revocation story. The worst of them has admin on four
   repos, three of which are private mirrors. The best of them expires in 28 days.
2. **No onboarding surface.** There is no route, no CLI verb and no bead type that turns "a GitHub
   user wants gardenio to work on their repo" into a working configuration. There is a 388-line
   runbook (`docs/onboarding-new-project-and-role.md`) with a 14-item checklist and four documented
   silent-failure modes, two of whose central claims are now stale in code (§9).
3. **The onboarding substrate the docs assume is switched off on the live cluster.**
   `deploy-values/gardenio-k3s.yaml:120` — `externalSecrets: {enabled: false}` (verified), and
   `:16-18` — "No External Secrets Operator, and IRSA is impossible." The entire
   `secretreconciler` → ExternalSecret → AWS Secrets Manager path that every onboarding doc
   describes **does not run on gardenio-k3s**. Every agent Secret there is hand-created with
   `kubectl create secret`.

Point 3 is unexpectedly good news: a controller that mints installation tokens and writes K8s
Secrets directly needs no ESO, no IRSA and no Terraform IAM edit. **The App design fits the live
cluster better than the design it replaces.**

---

## 4. Proposed design

### 4.1 Why a GitHub App, and not the alternatives

| | GitHub App | OAuth App | PAT (status quo) |
|---|---|---|---|
| Install UX | **One click**, repo picker built in, revocable by the user from their settings page | One click, but grants **user-wide** scopes — no repo picker | Human mints in UI, pastes into a shell |
| Token lifetime | 1 h installation token, minted on demand | Long-lived user token | Up to 366 d (fine-grained) or never (classic) |
| Scope | Per-installation repo set, **narrowable per issuance** to `repository_ids[]` + a `permissions{}` map | Whatever scopes the user consented to, across everything they can see | Whole `repo` scope for classic |
| Acts as | the App (`<app>[bot]`) — not a human | **the human**, indistinguishably | the human who minted it |
| Revocation | User uninstalls → every live token dies. Or `DELETE /installation/token` | Revoke the grant | Revoke the PAT, if you can find it |
| Webhooks | **Included.** One URL on the App; installing starts delivery | None | None |
| Blast radius if leaked | 1 h, those repos, those permissions | everything that user can reach, until noticed | everything in scope, for up to a year |

An OAuth App is disqualified on a point that matters more here than usual: **it acts as the
human.** The entire reason this work exists is that agent actions are currently indistinguishable
from the user's. An OAuth App reproduces that exactly, permanently. A GitHub App is the only
primitive that gives a one-click install *and* a non-human actor *and* a repo picker.

Already rejected in the ADR — **do not relitigate**: per-agent real GitHub user accounts (no
creation API, GitHub AUP §4 inauthentic-activity violation, billable seats;
`per-agent-tokens-github.md:174-183`), classic PATs on a shared bot (the status quo being
replaced, `:53-57`), Actions OIDC (not obtainable outside an Actions runner, `:185-195`), SSH
deploy keys (cannot open PRs or comment, `:196-202`), and per-role Apps (deferred, not rejected,
`:267-283`).

### 4.2 Mechanics

> **Superseded in part by §7.** This subsection was written before D1 and D4 were ruled. D1 puts
> the App under an org (`gardenio-io`) and D4 splits it in two, so "one App owned by
> `joyousaversion`" and the single merged permission list below are no longer the design. The
> authoritative permission sets are `MANIFESTS` in `gardenio/scripts/github-app-register.py`,
> which `verify` diffs the live Apps against. Two corrections that apply to both:
> **`workflows: write` is required** on the agents App — GitHub rejects an App-token write under
> `.github/workflows` without it, and crews already do exactly that (PRs #73, #65, #54, #19; the
> e2e plan `gd-ZYb9OTNwfe` has 33 further references to `.github/workflows/e2e.yml` in crew steps
> still to run). And **`actions: read` does not cover `gh run rerun --failed` or
> `gh workflow run`**, both of which crew plans instruct crews to run; those need Actions *write*.

1. ~~**Register one App** owned by `joyousaversion`. Not an org~~ — verified live, `type: "User"`,
   `plan: "free"`.
   - Permissions, as the union of §2.3 rows 1, 2 and 9: `contents:write`, `workflows:write`,
     `pull_requests:write`,
     `issues:write`, `metadata:read`, `administration:write` (ARC runner registration),
     `actions:read`, `checks:read`, `packages:read`. Note `checks:read` closes `gd-4j5m4jtW34`
     as a side effect — it gates `GET /commits/{ref}/check-runs`, which is exactly what 403s
     today, while `actions:read` separately gates the `gh run list` fallback.
   - **Turn on "Request user authorization (OAuth) during installation."** This is what makes
     onboarding one click instead of two: the install redirect then carries both `installation_id`
     *and* an OAuth `code` identifying the human, so gardenio learns *who* installed it without a
     second sign-in.
   - Set a **Setup URL** and a **Webhook URL** pointing at the viewer (§5).
   - Decide public vs private (§7, D2).
2. **Private key → AWS SSM Parameter Store** as a `SecureString`, at
   `/gardenio/github-app/private-key`, alongside `/gardenio/github-app/app-id` and
   `/gardenio/github-app/webhook-secret`. SSM (not Secrets Manager) because the ARC runbook already
   reads SSM (`gardenio/helm/ci/README.md:19`) and SSM is free. The chart materializes it into a
   K8s Secret named, per the lifecycle doc's own suggestion at
   `per-agent-tokens-lifecycle.md:587`, `gardenio-github-app-private-key`.
3. **Mint an App JWT** — RS256 over `{iat, exp ≤ 10 min, iss: app_id}` — then
   `POST /app/installations/{installation_id}/access_tokens` with a body narrowing
   `repository_ids[]` and `permissions{}` to what this spawn actually needs. The response carries
   `token` (visible once) and `expires_at` (exactly 1 h).
4. **Write a per-agent K8s Secret** `agent-github-token-<beadID>`, labels
   `gardenio.io/type: agent-token` + `gardenio.io/provider: github`, keys `token` and `username`
   (literal `x-access-token`). Point `spec.GitCredentialsSecret` at it instead of the shared
   Secret. **The agent side needs no change** — `agent_k8s_workspace.go:712` already defaults to
   `x-access-token` and `initclone.go:174-178` already reads the same two files.
5. **Refresh.** Re-mint at ~50 min and rewrite the Secret. Two things must change first, neither of
   which the ADR knew about:
   - RBAC needs `update` (or delete+create) on Secrets — today it is `create, delete` only
     (`rbac.yaml:22-27`).
   - A small in-pod refresh helper must re-render `/tmp/.git-credentials` and `hosts.yml` from the
     projected mount, because both are written once and never re-read (§2.1). Roughly 30 lines, or
     a `gb token refresh github` subcommand. **Do not skip this**; without it the rotation is
     invisible to the running agent and long spawns break at the 60-minute mark.
6. **Revoke** on done/kill via `DELETE /installation/token`, plus a janitor sweep — exactly the
   four-exit-path pattern `microvm_git_credentials.go` already implements for GitLab.
7. **Webhooks** arrive at one URL with an `X-Hub-Signature-256` HMAC-SHA256 over the body. This is
   **strictly better than the GitLab bridge**, which had no body signature at all and relied on a
   bare shared-secret header (`gitlab_sync.go:856-861`).

### 4.3 Where the minter lives

**In the controller, as a provider inside the existing issuer.** Justification from the code, not
from taste:

- The controller is the only component that already (a) holds cluster credentials, (b) creates and
  deletes per-agent Secrets (`agenttoken.go`), (c) knows the spawn lifecycle well enough to revoke
  on all four exit paths (`microvm_git_credentials.go:386-404`), and (d) has an RBAC gate for
  exactly this, already written and merely switched off (`rbac.yaml:22-27`).
- **Not a sidecar**, and not because sidecars are bad: the ADR already rejected a token sidecar for
  the janitor (`per-agent-tokens-lifecycle.md:454-456`), and a sidecar would need the App private
  key in the agent namespace — the single worst place to put it given §6.
- **Not a controller endpoint the agent calls.** Explicitly rejected twice:
  `per-agent-tokens-github.md:259-261` ("Option C… Heavy controller dependency for the agent's
  outbound traffic. Rejected for v1") and `per-agent-tokens-lifecycle.md:193` ("opens a new
  cross-pod auth surface"). Both rejections still hold.
- **Not a kbeads credential service.** No such component is named anywhere in the research corpus,
  and `per-agent-tokens-lifecycle.md:506` is explicit: "**No new database.**"

Concretely: a new `gardenio/controller/internal/githubapp` package (JWT minting,
installation-token mint/revoke, a clock-aware cache) plus a `github_app_credentials.go` in
`cmd/controller/` modelled line-for-line on `microvm_git_credentials.go`, behind a new
`agents.githubApp.enabled` chart value. The ARC cutover (§8, P1) uses the same package via a tiny
`mint-runner-token` path or, more simply, ARC's own native App support.

### 4.4 Per-agent attribution: what the App does and does not fix

Say this plainly so nobody is surprised later.

**It does not fix per-agent attribution in the GitHub UI.** Every PR, comment and API action from
every agent will show as `<app-name>[bot]`. There is no GitHub mechanism for per-spawn distinct
actors short of per-spawn user accounts, which are unavailable and against the AUP
(`per-agent-tokens-github.md:29-35`). GitLab was genuinely better here: its project access tokens
auto-create a distinct `project_NNNN_bot_xxxx` user per token, so each spawn looked like a
different actor (`:154-164`). **This is the one dimension on which the GitHub integration will be
worse than the GitLab one, and it is a GitHub platform limitation, not a design choice.**

**It does fix everything else that matters:** per-spawn *isolation* (a leaked token is one agent's,
one repo's, one hour's), per-spawn *revocation*, per-spawn *scope*, and — critically — it separates
the agent fleet's identity from **the user's own GitHub account**, which is the actual security
property being bought.

Mitigations, as already chosen in the ADR (`:36-44`) — a `Created by agent: <bead-id> /
<agent-name>` trailer in every PR body and commit, plus per-spawn audit events. Those events go in
the `capability_grants` ledger (§2.5), not in a bespoke GitHub table.

**Composition with the ziti per-agent identity spike (`gd-npzICz0rKS`) is the right long-term shape
and should be scoped as a separate question.** Ziti gives each agent a distinguishable identity *on
the network path*; the App gives the fleet one identity *at GitHub*. Composing them means the mint
request itself is authenticated per-agent — the controller can attribute "agent X asked for a token
scoped to repo Y" with cryptographic certainty, even though GitHub will still render the result as
one bot. That is the honest ceiling: **verifiable attribution in gardenio's records, collapsed
attribution in GitHub's UI.** The ziti spike's design has not been read; this is architectural
reasoning, not a review of that work.

### 4.5 What breaks or needs migrating

| Dependency | Works with an installation token? | Action |
|---|---|---|
| Agent clone / push / `gh pr create` | **Yes**, with zero code change — `x-access-token` is already the username (`agent_k8s_workspace.go:712`, and the live Secrets already set it) | Cut over |
| ARC self-hosted runner registration | **Yes.** ARC supports GitHub App auth natively, and the repo already specifies the needed permissions (`self-hosted-ci-runner.md:57-58`). ⚠ The exact `githubConfigSecret` key names for App auth are unverified — confirm against the ARC chart before P1 | Cut over in P1 |
| `images.yml` ghcr push | **Untouched.** Uses the workflow's own ephemeral `GITHUB_TOKEN` | No change |
| **ECR pull-through cache → ghcr.io** | **NO. This is the hard blocker.** An ECR pull-through rule stores a *static* credential in Secrets Manager and has no hourly-refresh mechanism. A 1-hour installation token cannot be used here | **Keep a long-lived credential on this path**, or make the ghcr packages public and drop it. Do not attempt to migrate |
| `ghcr` fallback imagePullSecret | Same problem: a kubelet pull secret is static | Keep, or make packages public |
| Agent PR merge | Not currently exercised — `github_pr_merge` is advisory-only and run by hand | Out of scope until the review loop grows an executor (`gd-3UQKfj0nA8`) |
| GitHub as a login IdP for the viewer | Different problem. GitHub is not an OIDC provider for third parties the way GitLab was; the App's user-authorization flow is OAuth2, not OIDC | Do not conflate with the Keycloak work |

---

## 5. The onboarding flow

This is the part that was actually asked for. Written from the installing person's point of view.

> **Precondition, stated honestly:** this flow needs a publicly reachable HTTPS URL for the App's
> Setup, Callback and Webhook URLs. gardenio-k3s today has **no public DNS record, no Ingress and
> no LoadBalancer** by design (`deploy-values/gardenio-k3s.yaml`, Keycloak scope note at
> `:174-190`), and forwardAuth is deliberately off (`:189`). Exposing the viewer is decisions
> `gd-4bu0CHgWQr` (BrowZer front door) and `gd-xZOjaRfA9P` (first public DNS record), which exist
> to be made carefully. **This is the real gating constraint on the whole plan** and it belongs in
> P3, not P0 — which is why the phases are ordered as they are.

1. The person lands on `https://<gardenio>/connect/github` — a one-screen page that says what
   gardenio will do with their repos and shows one button, **Install GitHub App**.
2. They click it and go to GitHub's own install page. They pick **which account** and **which
   repositories** (all, or a chosen list). GitHub renders the permission list; nothing gardenio
   controls.
3. They click Install. Because the App has "Request user authorization during installation"
   enabled, GitHub bounces them to gardenio's callback with `installation_id`,
   `setup_action=install`, and an OAuth `code`.
4. **gardenio does all of this automatically, with no further input:**
   - exchanges the `code` for a user token, reads `GET /user`, and auto-provisions a `type:user`
     bead with `auth_methods: github-app` — the same reflex the GitLab path already has at
     `kbeads/internal/server/user_provision.go:146-148`;
   - mints an App JWT and calls `GET /installation/repositories` to enumerate exactly what was
     granted;
   - creates one **project bead** per selected repo, populating `git_url`, `default_branch` and
     `repos[]` — the fields the runbook currently makes a human hand-author
     (`onboarding-new-project-and-role.md:27-38`);
   - records the installation (installation id, account login, repo ids, granted permissions) as a
     bead or controller-owned record — **not** as a hand-made Secret;
   - verifies the whole loop by minting one throwaway installation token and probing
     `git-receive-pack` on each repo — **not** by reading GitHub's `permissions` object, which
     reports the user's role rather than the token's grant (§2.2).
5. They land on `/connect/github/done`, which shows the repos now connected, a green or red result
   for that live verification, and one button — **Spawn an agent here**. This is the first-run
   moment; it should end in something running, not in a checklist.
6. **The minimum they must still type.** Be honest about this, because it is what separates a demo
   from a product:
   - **Nothing** for repo access. That is the whole win.
   - **An LLM account.** gardenio cannot invent one. Today this is a hand-authored account bead
     (`onboarding-new-project-and-role.md:53-62`), and an unresolved account produces the worst
     failure mode in the system: pods spawn, look alive, and emit only synthetic turns (`:44`).
     The connect flow must either ask for this on screen 1 or refuse to finish, and it must
     **never** leave a project connected but unrunnable.
   - **Branch protection**, if their repo has rules that block `agent/*` pushes. gardenio does not
     and should not edit branch protection. Detect it and say so on the done screen; do not fail
     silently the way the GitLab path did (`:153`).

**The natural landing page is the beads-viewer** (`cmd/beads-viewer/`). It already has session auth
(`GET /login`, `main.go:396`), an operator-role gate (`server.go:1693`), htmx forms, and a project
hub. The webhook receiver is the one piece that must **not** sit behind the operator gate — GitHub
cannot authenticate to it. It needs its own route, verified solely by `X-Hub-Signature-256`,
failing closed on an empty secret, exactly as the GitLab bridge does at `gitlab_sync.go:848-862`.

---

## 6. Security review

**The private key is a new crown-jewel secret, and it is more dangerous than any PAT currently in
the system.** An installation token is 1 hour and scoped; the private key mints unlimited
installation tokens across every repo the App is installed on, for as long as it is valid. Blast
radius is strictly larger than the shared PAT it replaces. Treating it as "just another secret"
would be the single biggest mistake this plan could make.

**Where it must live:**

- AWS SSM Parameter Store, `SecureString`, under a customer-managed KMS key, readable by **one**
  IAM principal.
- Materialized into **one** K8s Secret in the **controller's** namespace. **Never** in the agent
  namespace, never in `arc-runners`, never in the gVisor namespace
  (`gvisor-namespace.yaml:258-267` mirrors agent Secrets across namespaces — the App key must be
  excluded from that list explicitly, and a test should assert it).
- Never in a pod env var. Project it as a file, following the W6 precedent the codebase already set
  (`initclone.go:129-135`).

**Who can read it, and the IMDS problem.** Every pod on this cluster can currently reach IMDS and
assume the node role, and the node role can read the admin kubeconfig from SSM (known open P1,
decision `gd-adirNxx3Qo`, netpol task `gd-fTCYDqMYeH`). The implication is blunt: **so long as that
hole is open, any agent pod can reach the App private key regardless of where in the cluster it is
placed.** Kubernetes RBAC does not help when the attacker can obtain cluster-admin. Therefore:

- Either the IMDS netpol lands **before** the key does, or
- the key does not live in the cluster at all in P0/P1 — it stays in SSM and is read at mint time
  by the controller through a narrowly scoped IAM path, which at least means a compromised pod
  must also win an IAM race rather than just reading a Secret.
- Flipping the IMDS hop limit to 1 as a fix is **barred** — it breaks the autoscaler and
  `ecr-token-refresh`. Hard constraint, not a suggestion.

**Do not put the key in `arc-runners`.** Runner pods execute arbitrary workflow code from any
branch of four repos. Giving that namespace a credential that mints tokens for all of them
recreates, with a sharper edge, the exact hazard the old `.gitlab-ci.yml` recorded at `:481-489` —
an unprotected master key handed to every MR branch. It is also the specific escalation path
`gd-QoziM1L954` closed by dropping `workflow` scope; an App key with `administration:write` in that
namespace would reopen it. ARC should hold **only** its own App credentials for runner
registration, ideally a **second, separate App** with `administration:write` + `metadata:read` and
nothing else (D4).

**Revocation, which is the part that genuinely improves:**

- The user uninstalls the App from their own settings page. Every live installation token dies. No
  operator involvement, no ticket, no "find the PAT."
- Per-token: `DELETE /installation/token`.
- Blast-radius comparison: leak the shared PAT today → Contents R/W on two repos, until
  2026-10-19, and revocation stops every agent and both laptop sessions at once. Leak an
  installation token → one repo set, one hour, revoke one agent. Leak the private key →
  catastrophic, which is why the whole of this section exists.

**Rotation:** GitHub allows multiple private keys per App, so rotation is generate-new → deploy →
delete-old with no downtime window. Nothing in gardenio does this today for any credential; build
it into P0 rather than discovering its absence in a year.

**Two hygiene fixes worth taking while in the area, neither of which depends on the App:** add
`permissions: {contents: read}` to `ci.yml` and `checks.yml`, which declare none and therefore
inherit whatever the repo default is; and drop
`gardenio/grafana-plugins/gardenio-assistant-app/.github/workflows/cp-update.yml`, vendored
scaffold boilerplate referencing a `secrets.GH_PAT_TOKEN` with contents+PR+workflows read/write.
It is inert where it sits, but it is a loaded reference waiting for someone to move the directory.

---

## 7. Operator decisions — ruled 2026-09-21

All seven were put to the operator and answered. The ADR's §6.1–§6.8 questions had been raised in
the ACME era against dead `ka-` beads and were never re-filed; these supersede them.

| # | Question | Ruling |
|---|---|---|
| D1 | App owner | **Create an org; org-owned App.** Not user-owned. |
| D2 | Public or private App | **Public** — the distribution surface for a proprietary harness. Scoped by the operator: *the App is public, the source and infra stay private.* |
| D3 | Is `gardenio[bot]` attribution acceptable | **Accept**, with the `Created by agent:` trailer and real per-agent attribution in gardenio's own records (§4.4). The machine-user PAT pool is rejected. |
| D4 | One App or two | **Two.** One for agents (`contents`, `pull_requests`), one for ARC (`administration:write` + `metadata:read` only). |
| D5 | Public callback exposure | **Conventional public Ingress + the first public DNS record** (`gd-xZOjaRfA9P`). Not the BrowZer/ziti front door. |
| D6 | Name | ~~`gardenio`~~ → **`gardenio-io`**, re-ruled 2026-09-21. The original rested on an App-name namespace that does not exist; `gardenio` is an unrelated Organization and GitHub would reject it. PRs show as **`gardenio-io[bot]`**. CI App stays `gardenio-ci`. See §7.1. |
| D8 | App owner and visibility | **Register both Apps private, owned by the user `joyousaversion`** (2026-09-21). A private App installs only on its owner's own account and every CI repo is under that user, so D1's org ownership cannot hold yet. **At P3: flip the agents App public *first*, then transfer it to `gardenio-io`** — transferring while private would uninstall it. D1 is deferred, not overturned. |
| D9 | `actions` level on the agents App | **`write`** (2026-09-21). Crew plans call `gh run rerun --failed` and `gh workflow run`, both of which require Actions write. Ruled knowing it also grants cancel and deletion of workflow runs and logs — a crew can erase CI evidence. |
| D7 | ghcr strategy | **Keep packages private**; carry the pull-through-cache credential as a documented exception with a named owner and rotation date. |
| — | Sequencing | **P0 + P1 now.** P2 — which would make the 2026-10-19 expiry a non-event — is not yet authorised. |

### 7.1 What the rulings changed, and three things they surfaced

**The shape D2 chooses: a public front door onto a closed system.** The App is the only public
artifact; the harness, the repos and the infra stay private. That is a coherent distribution model
— GitHub's install flow becomes the product's front door without exposing what is behind it — and
it is the first thing in this system that is deliberately public. It also means the App's public
page (name, description, owner, permission list) is discoverable, and the permission list is the
one thing a prospective installer will judge the product by. Keep it minimal for that reason as
well as the security one.

**The org name `gardenio` is taken** — it already exists as an Organization on GitHub (verified,
`gh api users/gardenio` → `type: Organization`, created 2020-05-17, unrelated to us). `gardenio-dev`
is taken as a User. The org created for D1 is **`gardenio-io`**.

> **CORRECTION (2026-09-21).** This section previously claimed GitHub App names live in a
> *separate global namespace* from org/user names, and D6 ruled the name `gardenio` on that
> basis. **That is wrong.** GitHub's registration doc states: "The name must be unique across
> GitHub. You cannot use the same name as an existing GitHub account, unless it is your own user
> or organization name." We own `gardenio-io`, not `gardenio`, so the exception does not apply and
> the first submit would have been rejected at the confirmation page. Free at the time of writing:
> `gardenio-agents`, `gardenio-bot`, `gardenio-crew`, `gardenio-ci`, plus `gardenio-io` itself
> under the own-organization exception. Screen a candidate with `gh api users/<name>` (want 404).
> An App name **can** be changed after registration, so this is recoverable — but a name edited at
> GitHub's confirmation page silently drifts from `MANIFESTS`, which is why `verify` now diffs it.

> **CORRECTION (2026-09-21).** This section previously claimed "an org-owned App does not require
> moving any repository … the App installs *onto* that user account." **That is wrong for a
> private App.** GitHub: "Private GitHub Apps can only be installed on the user or organization
> account of the app owner." Every CI repo is owned by the **user** `joyousaversion`
> (`joyousaversion/gardenio`, `/beads-hq`, `/gascity-hq`, `/gascity-packs-hq`, verified from the
> live `AutoscalingRunnerSet` specs), so a **private** App owned by org `gardenio-io` cannot be
> installed on any of them and P1 is impossible as designed. The claim *is* true once the App is
> public — which D2 already rules it will be, just at P3. Three ways out, in cost order:
> **(a)** register public at P0 (an App with no webhook, no events and no user-auth leg grants an
> installer nothing, so §7.1's abuse argument below does not yet bite — that argument is about §5
> step 4's auto-provisioning, which does not exist until P3); **(b)** own the App from the
> `joyousaversion` account instead of the org, keeping it private (`--owner-type user`; contradicts
> D1); **(c)** move the four repos into `gardenio-io` (D1 explicitly rules this out).
> `--public` and `--owner-type` exist in `github-app-register.py` for (a) and (b).
> **Ruled (D8): (b).** Both Apps are registered private under `joyousaversion`. The org is not
> abandoned — an App can be transferred between accounts — but the order at P3 is load-bearing:
> **flip public first, then transfer to `gardenio-io`.** GitHub warns that a transfer which would
> uninstall the App from the source account is allowed to proceed, and transferring a *private*
> App away from the account its four installs live on is exactly that case. Public first means the
> installs survive the move.

**D2 creates an abuse surface this plan did not previously have to solve.** A public App can be
installed by **any GitHub user**, and GitHub provides no install-time allowlist or approval queue —
the integrator receives an `installation` webhook and must decide for itself. Left unhandled, §5
step 4 means a stranger's install auto-provisions a user bead and one project bead per repo they
selected, and the done screen offers them **Spawn an agent here**, spending the operator's LLM
budget. Two consequences:

- **P3 must gate installation.** An allowlist, or an approval queue where an install lands as a
  pending record and creates nothing until an operator admits it. Without it, "one click installs
  me" is an open door to bead creation and spawn attempts. This is a new P3 requirement and the
  direct cost of D2.
- **Register the App private in P0 and flip it to public at P3**, when the gating exists. Public is
  the chosen end state; it buys nothing during P0/P1, which have no callback and no onboarding
  surface, and a public App with neither is a liability with no upside. Sequencing, not a reversal
  of D2.

**D5 raises the stakes on the webhook receiver.** With a public App on a public Ingress, the
receiver takes unauthenticated POSTs from the whole internet, not just from GitHub. The
`X-Hub-Signature-256` verification and its fail-closed-on-empty-secret behaviour (§5) are
load-bearing security controls, not hygiene, and need tests asserting both the reject path and the
empty-secret path.

**D7 stands on its own merits, not on footprint.** The earlier recommendation to make packages
public was conditioned on the App being private. Public App / private packages is coherent — App
visibility and package visibility are independent. P5's acceptance therefore reads "zero classic
PATs, one documented long-lived exception," not "zero long-lived credentials."

---

## 8. Phased rollout

Each phase ships alone and is verifiable without the next.

### P0 — Register the Apps; change nothing else — **DONE 2026-09-21**

- ✅ Create the org, name `gardenio-io` (D1, name chosen 2026-09-21). GitHub has no API for creating
  a github.com org, so this one click could not be automated; the operator created it 2026-09-21.
  The org now exists and holds nothing — under D8 both Apps were registered against the **user**
  account instead, because a private App can only be installed on its owner's own account and every
  CI repo is under `joyousaversion`. The org is where they move at P3, *after* they go public.
- ✅ Register **two** Apps (D4), named per D6 as re-ruled, **private for now**; they flip to public
  at P3 once install gating exists (§7.1). Both registered via the manifest flow with a private key
  minted straight into SSM:

  | app | name | id | url |
  |---|---|---|---|
  | agents | `gardenio-io` | 5029022 | <https://github.com/apps/gardenio-io> |
  | arc | `gardenio-ci` | 5029021 | <https://github.com/apps/gardenio-ci> |

  Both are **inert**: no `hook_attributes`, no `events`, no user-authorization leg, 0 installations.
  `verify` exits 0 on both — name, visibility and the full permission set match `MANIFESTS` exactly
  (8 grants / 2 grants).
- ✅ Store `app-id`, `private-key`, `webhook-secret` in SSM as `SecureString` under a CMK, with a
  single-principal read policy. CMK `alias/gardenio-github-app`
  (`9b311963-9a58-4d0a-9586-b5ef20107f04`) created 2026-09-21, annual rotation on; policy grants
  `gardenio-administrator` encrypt/decrypt **only** via `kms:ViaService = ssm.us-east-1…`. The
  write path is proven end-to-end: a multi-line PEM round-tripped byte-exact as a `SecureString`
  under that CMK. **Not in Terraform** — that lane is another session's, so this is flagged drift
  for its owner to codify.
- ✅ Write a throwaway local script that mints a JWT and calls `GET /app`. It is the `verify`
  subcommand of the same script; it also diffs the App's live permissions against the manifest and
  lists installations. Nothing in the cluster consumes anything yet.
- ✅ Document the rotation procedure (§6) at the same time, not later —
  `gardenio/docs/runbooks/github-app-key-rotation.md`, covering routine key rotation, webhook
  secret rotation, key compromise, leaked installation token, and an explicit list of what is *not*
  automated.
- ✅ Land the `structured-field` credential record the operator already chose in `gd-NnaRPh0AY0`
  (`token_name, scopes, minted_at, expires_at, secret_ref`) and seed it with every credential in
  §2.3. Shipped as a real bead type: `type:credential`, added to `builtinConfigs`
  (`kbeads/internal/server/config.go`) with `expires_at` as a genuine `timestamp` and admin-only
  writes, guarded by `TestBuiltinCredentialType`. Registered on the live daemon's config KV so it
  works before the next deploy. Seeded with the three long-lived credentials, each **probed against
  the live token** rather than transcribed — which is how the §2.3 corrections above were found.
  The ephemeral `GITHUB_TOKEN` rows (§2.3 3–5), the IMDS/ECR row (6) and the not-implemented row
  (9) are deliberately not inventoried: they are per-job or nonexistent, and padding the inventory
  with them would dilute the expiry query that is the point of the type.

**Tooling built for this phase:** `gardenio/scripts/github-app-register.py` (+ `_test.py`, 30
hermetic tests, no network and no AWS). It drives the GitHub App Manifest flow so the App's
permission set is code under review instead of a web form, and so the PEM travels from GitHub's
API response into SSM **without ever being written to a Downloads folder**. Subcommands:
`manifest`, `register` (one click, local callback), `convert` (paste-back path for when the click
happens on a phone), `verify`. Zero Python dependencies — JWT signing shells out to `openssl`.
Wired into `checks.yml`'s script self-tests, so the D4 two-App split is enforced by CI: if the
agents App ever acquires `administration`, the build goes red instead of shipping a credential
every agent pod mounts that can also delete runners and manage collaborators.

**Acceptance — met.** `GET /app` returns 200 for a JWT signed by the stored key, for both Apps.
`GET /app/installations` returns `0` — correct at P0, which registers only; the install is P1's
first step. Each key exists in exactly one place (`/gardenio/github-app/<app>/private-key`,
`SecureString` under the CMK). No gardenio component references either App. No cluster change.
Every row of §2.3 has a queryable expiry.

One acceptance criterion had to be repaired before it meant anything: `verify` reported visibility
by printing the `public` field of `GET /app`, and **that response has no `public` field**, so the
line read `public: None` for any App in any state. It looked like a measurement and was not one —
the same failure mode as the §2.3 permission-object trap, one layer up. The positive probe is the
**unauthenticated** `GET /apps/{slug}`: 200 for a public App, 404 for a private one (control:
`dependabot` → 200; both of ours → 404). `MANIFESTS` now declares the expected visibility and
`verify` diffs the measured value against it (`6e61d0a2`).

### P1 — Cut ARC runners over to the App — **DONE 2026-09-22**

**Outcome.** All six scale sets run on the `gardenio-ci` App (app id 5029021, installation
163683294). All six `*-listener-config` Secrets are PAT-free; a cluster-wide scan of every Secret
value for PAT-shaped strings finds the ARC PAT nowhere. SSM `/gardenio/ci/github-token` is deleted.
Jobs completed green on all four repos after the cutover.

| what | evidence |
| --- | --- |
| 6/6 listeners on App auth | each logs `getting access token for GitHub App auth` → `/app/installations/163683294/access_tokens` → `getting runner registration token` for **its own** repo URL |
| 6/6 listener configs PAT-free | `github_token` empty, `github_app_id: 5029021` on all six |
| gardenio (3 tiers) | completed `success` jobs post-cutover on `gardenio-k3s` / `-light` / `-medium` |
| gascity-packs-hq | run `35692674944` `workflow_dispatch` → completed **success** (2.4 min) |
| beads-hq | run `35692671968` → completed **success** (28 min) — its first CI since 2026-09-17 |
| gascity-hq | run `35692673579` → completed **success** (14 min) |
| still flowing 10h later | queue empty, 6/6 listeners Running, pools correctly at zero; last run 16:03Z. Listeners restarted 44–74 min earlier during unrelated node churn and came back on App auth — re-checked, not assumed |
| no runners lost | 3 in-flight EphemeralRunners survived the heavy-tier listener delete; 3 more were created 74s after it |
| deregistration works | light tier logged `deleting: 1 → 0` on the App credential |
| controller | 0 `ERROR` lines, 0 auth failures across 200 lines |

**The listener-config trap is real and was hit.** A `helm upgrade` with unchanged values does *not*
regenerate `*-listener-config`; deleting the `AutoscalingListener` CR does (the ARS controller
recreates it from the current `githubConfigSecret`). That delete is also the *lighter* operation —
in-flight runners talk to GitHub directly and are unaffected; only new dispatch pauses ~10s.

**Free fix found on the way.** `beads-hq-k3s` was live at revision 2 (2026-09-17), predating the
2026-09-20 `listenerTemplate` toleration fix that was already in its values file — its listener had
been `Pending` and unschedulable for 42 minutes, so that repo had *no* CI at all. `helm upgrade` to
revision 3 fixed it in ~8s. The other five were already current.

> **Revocation is blocked, and the reason is the finding.** The ARC PAT is byte-identical
> (`sha256[:16] = 3adc3e76e60da0e2`) to the token stored in `~/.config/gh-ja` — the `gh` CLI
> credential for the `joyousaversion` account. It was never a dedicated CI token; it is a human's
> personal working credential that was *also* pasted into a k8s Secret. That is precisely why its
> scopes are `admin:enterprise, admin:org, repo, workflow, delete:packages, gist, user, …` rather
> than the `administration:write + metadata:read` ARC actually needs.
> Revoking it now logs `gh-ja` out. **Order: re-authenticate `gh-ja` first**
> (`GH_CONFIG_DIR=$HOME/.config/gh-ja gh auth login` — interactive, the operator runs it), *then*
> revoke at <https://github.com/settings/tokens>.
> Verified safe to revoke once that is done: it is **not** the `ghcr` pull credential (a different
> classic PAT, `sha eec906e7fff3c0c9`), **not** the crew git token (fine-grained `github_pat_…`, 93
> chars, in `gardenio-git-credentials` + `gascity-git-credentials`), and no repo-level Actions
> secret exists on any of the four repos.
> The CI-side exposure P1 existed to remove is **already gone** — the credential is out of the
> cluster and out of SSM. What remains is killing the string itself.

**Third credential for the cliff list.** The `ghcr` Secret in ns `gardenio` holds its own classic
PAT (`ghp_…`, `sha eec906e7fff3c0c9`, user `joyousaversion`). Not in P1's scope, but it belongs
next to the two tracked in `gd-QoziM1L954`.

<details><summary>Original P1 plan as written</summary>

- Install the ARC App on the four repos; **delete and recreate** the `gardenio-github` Secret with
  App credentials (`kubectl create secret generic`, never `kubectl apply` — the current Secret's
  `last-applied-configuration` annotation holds the PAT in cleartext and `apply` would recreate
  it); `helm upgrade` the **six** scale sets, driven from `helm list -n arc-runners -o json`
  rather than a written list.
- **Force the listeners to re-read it.** The PAT is materialised into six
  `*-listener-config` Secrets in `arc-systems`; a `helm upgrade` with unchanged values will not
  refresh them, so an apparently successful swap can leave every listener still on the PAT.
  Confirm all six are PAT-free before going further.
- Only once all six listeners are healthy and a real job has run on each of the four repos: delete
  the SSM PAT, then **revoke it at GitHub** last. Keep the PAT value in hand (not only in SSM)
  until that point — the two halves of this phase are irreversible in opposite directions, and a
  wrong permission set is cheap to fix right up until revocation.

**Acceptance:** all six scale sets report healthy with `minRunners: 0`; all six
`*-listener-config` Secrets are free of the PAT; a `workflow_dispatch` on each of the four repos
scales from zero and completes; the classic PAT is revoked and a `gh api` call with it returns 401. **This alone removes a human-owned, admin-on-four-private-repos,
never-rotated credential from the system** — worth doing even if every later phase is abandoned.

</details>

**Acceptance — met except the revocation clause, which the operator has closed as out of scope.**
Six scale sets healthy at `minRunners: 0`; six listener configs PAT-free; `workflow_dispatch`
scaled from zero and completed **green on all three mirror repos**, and gardenio ran its own
push-triggered jobs — four of four repos completed successfully on App auth.

The old PAT is **not** revoked and is not being tracked as open work (operator's call,
2026-09-22). It is out of the cluster and out of SSM, so the exposure this phase existed to remove
is gone; what remains is only the string itself. The don't-revoke-before-re-authenticating-`gh-ja`
hazard is recorded above for whoever eventually touches it.

### P2 — Per-agent installation tokens (the minter)

> **Status 2026-10-12 (gd-BHLDnjX1Hw):** minter + controller wiring landed behind
> `agents.githubApp.enabled` (default false) in task gd-fzNBBfmdEO:
> `controller/internal/githubapp`, `cmd/controller/github_app_credentials.go`, RBAC
> `update` (+`list` when enabled), key Secret mounted as a file in the controller only,
> excluded from the gVisor mirror (chart `fail` guard + test) and on the agent-pod mount
> deny-list. In-pod refresh is gd-uPJ6AEsfyn; the prod cutover is gd-Rq4qekWWgb. Where the
> code differed from this plan: (1) the token is scoped with the `repositories` (names)
> param, not `repository_ids`; (2) the chart runs the controller and agent pods in the SAME
> namespace, so "key never in the agent namespace" (§6) cannot hold literally — the
> compensating controls are file-only mount in the controller pod, the
> `isForbiddenPodSecret` deny-list, and the mirror guard; (3) the controller Role granted
> secrets `get` only (plus `create`/`delete` behind `perAgentTokens`), so the existing
> beads token janitor's `List` has never been authorised — the GitHub sweep adds `list`
> only when `githubApp.enabled`, and the beads janitor gap is left for its own fix;
> (4) the freeze/thaw path returns before the spawn door, so a thawed agent is not
> re-provisioned a token (same as the beads per-agent token today).

- New `internal/githubapp` package; `cmd/controller/github_app_credentials.go` modelled on
  `microvm_git_credentials.go`.
- Fix the RBAC gap: add `update` to the per-agent-token Secret rule (`rbac.yaml:22-27`).
- Add the in-pod refresh helper for `/tmp/.git-credentials` and `hosts.yml` (§4.2 step 5).
- Behind `agents.githubApp.enabled`, default `false`. Fail **closed** in prod — no silent fallback
  to the shared token.
- Exclude the private-key Secret from the gVisor namespace mirror, with a test asserting it.

**Acceptance:** a spawned agent clones, pushes a branch and opens a PR using a token that (a) did
not exist before the spawn, (b) is scoped to that repo only — **proved by a `git-receive-pack`
probe against a repo it should not reach, not by reading the `permissions` object** (§2.2) — and
(c) is gone from GitHub within minutes of the agent finishing. A spawn lasting >70 minutes pushes
successfully after the refresh. The shared PAT is removed from that project bead and the agent
still works.

**If P2 lands before 2026-10-19, the shared PAT expiry becomes a non-event.**

### P3 — The one-click onboarding surface

D5 chose a conventional public Ingress plus the first public DNS record (`gd-xZOjaRfA9P`).

- **Install gating (new, and the precondition for flipping the Apps public — §7.1).** A public App
  is installable by any GitHub user with no GitHub-side approval queue. An install must land as a
  pending record that creates nothing until an operator admits it, or an allowlist must reject it
  outright. Nothing auto-provisions, and nothing becomes spawnable, before that check.
- `GET /connect/github`, the setup/OAuth callback, and `GET /connect/github/done` in beads-viewer.
- Unauthenticated webhook receiver with `X-Hub-Signature-256` verification, failing closed on an
  empty secret, with tests asserting both the bad-signature reject and the empty-secret reject —
  this endpoint faces the whole internet under D5, not just GitHub.
- Auto-provision the user bead; auto-create project beads from `GET /installation/repositories`.
- The LLM-account prompt, and branch-protection detection surfaced on the done screen.

**Acceptance:** a person with no gardenio knowledge, given one URL, gets from zero to a spawned
agent on their own repo. Time it. Count what they had to type — the target is the LLM account and
nothing else. Nothing in the flow requires `kubectl`, `kd`, Terraform or a runbook.

### P4 — Attribution and audit

- `Created by agent: <bead-id> / <agent-name>` trailer on every PR body and commit.
- Mint/revoke events written to the shared `capability_grants` ledger the operator chose in
  `gd-OICMBdBfIo` — **not** a GitHub-specific audit mechanism, which is the option that ruling
  explicitly rejected. The ledger does not exist yet; its migration is **050 or later** (047 is
  latest on main, 048 is PR #67, 049 is `gd-J7Wqmqfnpq`). Whoever builds it first owns the table;
  this plan writes to it.
- Scope, separately, whether the ziti per-agent identity spike (`gd-npzICz0rKS`) should
  authenticate the mint request itself (§4.4).

**Acceptance:** for any PR opened by an agent in the last 30 days, gardenio's own records name the
agent, the bead and the token — even though GitHub shows `<app>[bot]`.

### P5 — Decommission

- Revoke and delete every shared GitHub PAT still live: `gardenio-git-credentials`,
  `gascity-git-credentials`, the `read:packages` token, and **the full-scope classic PAT from
  `gd-QoziM1L954` that is still unrevoked** — noting it also sits at `~/.gh-token-ja`, so revoking
  it breaks the laptop's `gh` until replaced.
- Resolve the ECR pull-through cache credential per D7 — the one that legitimately cannot migrate.
- Excavate `onboarding-new-project-and-role.md`: delete the GitHub steps the App replaces, and fix
  the two claims now false in code (§9).

**Acceptance:** the P0 credential record shows every GitHub credential gardenio holds, each one
either short-lived-and-minted or explicitly justified as an exception with a named owner and a
rotation date. Zero classic PATs.

---

## 9. Errors found in the existing design record

Worth fixing in-tree whether or not this plan proceeds:

1. `per-agent-tokens-github.md:378-384` asserts that a rotated Secret reaches the pod through
   `secretKeyRef` env vars. **False** — env vars are fixed at pod creation. Also moot: the code
   moved to projected files after the ADR was written (`initclone.go:129-135`).
2. The same §3.5 assumes `/tmp/.git-credentials` is the only materialization. `hosts.yml`
   (`agent_k8s_workspace.go:738`) is a second one-shot copy the ADR predates entirely.
3. `rbac.yaml:22-27` grants `create, delete` but not `update`, so the ADR's "rewrite the Secret in
   place" is not merely untested — it is denied by RBAC.
4. `c1-token-ttl-refresh-design.md:3` says "No implementation." All four Shape C pieces have
   shipped (`kbeads/internal/server/fleet.go`, `refresh_token.go`, `http_auth.go`,
   `token_revocation.go`). The status line is stale and misleading.
5. `onboarding-new-project-and-role.md` carries two claims now false in code: the `gitlab.com`
   hardcode (fixed — `initclone.go:39-60,144-151` derives hosts per repo URL) and env-var
   credential injection (replaced by file mounts).
6. `gardenio/docs/research/canonical-prod-identity-gitlab.md` is cited four times as the authority
   on the OAuth app design and **is not in the tree or in any commit** (verified with
   `git log --all --diff-filter=A`). That design record is lost.
7. `controller/internal/oauth/` is dead code — a complete PKCE + device-grant implementation with
   **zero importers anywhere in the repo**, including tests outside its own package. Either P3
   adopts it or it should be deleted; leaving unimported auth code in the tree is a trap.

---

## 10. Claims not verified

Listed so nobody builds on them. The draft this plan came from listed eleven; four were closed by
the bead store (§2.2) and are removed.

1. ~~**ARC's exact `githubConfigSecret` key names for GitHub App auth.**~~ **Resolved 2026-09-21**
   against the published chart (`helm show values
   oci://ghcr.io/actions/actions-runner-controller-charts/gha-runner-scale-set`). Variation B is
   `github_app_id` / `github_app_installation_id` / `github_app_private_key`, and for the
   pre-defined-secret form we use, all three are `--from-literal` keys on the same Secret. Two
   consequences for P1: the IDs **must be quoted strings**, and the swap **replaces the Secret's
   keys**, not just the value of `github_token` — so the old key has to be dropped, not overwritten.
   The `github_app_id` field accepts either the App ID or the client ID.
2. **Whether ghcr.io accepts a GitHub App installation token for pulls.** Believed yes with
   `packages:read`, not verified. Irrelevant to the live path (ECR/IMDS) but relevant to D7.
3. **Installation rate limits for an App installed on a Free personal account.** The ADR quotes
   5,000 req/h from 2024 docs (`:397, :410-412`) and says to re-verify. Not done.
4. **The design of the ziti per-agent-identity spike `gd-npzICz0rKS`.** Not read. §4.4's
   composition argument is architectural reasoning, not a review of that work.
5. **`docs/research/per-agent-tokens-verification.md` (T5).** Not read directly; its conclusions
   reached this plan secondhand. If it specifies the attribution test, P4's acceptance criteria
   should be reconciled with it.
6. ~~**Whether the `read:packages` token from `gd-QoziM1L954` is classic or fine-grained.**~~
   **Resolved 2026-09-21: classic.** Probed directly — it returns `x-oauth-scopes: read:packages`,
   and a fine-grained token emits no `x-oauth-scopes` header at all, so the header's presence is
   itself the answer. It counts against P5's "zero classic PATs".

**A note for anyone re-running this research:** `gd-` bead IDs are alphanumeric (`gd-adirNxx3Qo`,
`gd-4bu0CHgWQr`), not numeric like the dead `ka-0000004236` form. A `gd-[0-9]+` grep returns zero
hits and will wrongly suggest no live beads are referenced in the tree. They are referenced widely
— `gardenio/helm/ci/nth-values.yaml:68`, `deploy-values/gardenio-k3s.yaml:57`, and many more.
Likewise, `kd search` is lexical and requires **all** terms; `--semantic` is disabled on this
daemon (`BEADS_EMBED_BACKEND=noop`), so a two-word query returning zero beads means nothing.
