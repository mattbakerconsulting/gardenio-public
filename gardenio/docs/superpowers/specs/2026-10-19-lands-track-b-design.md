# Lands, Track B: Gate, Deed and purse, Steward, Fences, Open day (design spec)

**Status:** design spec, phase 0e of plan gd-vmw5s07A1F (Gardenio: Lands). Task gd-W8kHIgcYhz.
**Scope:** the five phases that open Lands to the public: 7 Gate (gd-pqJennUltH), 8 Deed and
purse (gd-KiiqMwbZLu), 9 Steward (gd-G8lmByYEoM), 10 Fences (gd-Gx6arHBvzJ), 11 Open day
(gd-xHvLXLLdXf). For each: the design, then an ordered step list the plan-gardener mints.
**Inputs:** bundle gd-uj7BxrMQsH (narrative gd-NcaFgzREzt including its draft 3 "What is net
new" list, product definition gd-jXfrmBe6Yy cited `PD`); the 0d doc
`docs/superpowers/specs/2026-10-19-lands-open-questions-and-track-b-dependencies.md` (cited
`0d`), whose prerequisite pins and three gaps this spec closes; the owner's twelve answers.
**Siblings:** Commons spec `2026-10-19-lands-commons-design.md` (0a, cited `CS`), Workbench +
Glass spec `2026-10-19-lands-workbench-glass-design.md` (0b, cited `WG`), C1–C12 rubric
`gardenio/docs/commons-ux-rubric.md` (0c).

Every code claim below was re-read on `main` at 79e66ff0. Paths are relative to
`gardenio/controller/` unless they start with `kbeads/`, `gardenio/` or `docs/`.

---

## 0. Summary

A stranger signs in with GitHub, accepts the terms, installs the Gardenio Lands GitHub App on
one repo and applies for a **plot**. The operator approves it from a decision card. A
**provisioner** turns the approved application into a land: a project bead, a Keycloak group
with the applicant as owner, a prepaid **purse** of credits, the land's channels, a namespace
with a quota and a NetworkPolicy, and a **steward** agent that greets the tenant in `#general`
and spawns gardeners against the repo while the purse lasts. When the purse is empty, spawns
are refused and running agents park. Fences make that safe for a stranger's code, product
hygiene (terms, SLA, support) makes it a product, and Open day runs the first real land.

The owner's answers fix most of the shape:

| Answer | What it fixes in Track B |
|---|---|
| Q1 credits (gd-l13fjWA0iW) | The purse is a prepaid balance the operator grants by hand. No payment processor, no invoices in the product. Stripe is a post-Open-day decision (step 11.6). |
| Q2 GitHub IdP only (gd-2Yb3mGI6QM) | One door: Keycloak brokering GitHub. No password or email sign-up. Repo control is proven by the GitHub App installation, not by a form field. |
| Q3 manual approval (gd-N7jCRnzwtz) | Every application becomes an operator decision. No auto-approve path is built. |
| Q4 namespace per plot (gd-prDX5Xhr8n) | Each land's agent pods run in `land-<name>` with a NetworkPolicy, a ResourceQuota and an RBAC-less ServiceAccount. |
| Q5 product (gd-p1yI8kaI4q) | Terms, privacy and AUP before registration (7), SLOs and a status page before Open day (10), support before the first external land (11). |
| Q6 github.com only, App tokens (gd-PojrrBbCo4) | Tenant agents get GitHub App installation tokens for their one repo, never a PAT or a deploy key. |
| Q8c land-role spawn rights (gd-fPXoeHDQS3) | Owner and members spawn within the land and the purse; guests do not; the steward spawns too. |
| Q8d Glass operator-only (gd-kAgWFTwF3U) | Tenants never see a PTY. The Glass is out of Track B's scope except as an operator tool for the land. |

## 1. Decisions taken in this spec

Say so on the plan thread if any is wrong; each is load-bearing for the step lists in §7.

| # | Decision | Choice | Why |
|---|---|---|---|
| B-D1 | What an applicant is | A signed-in realm user with **no gardenio role and no group**. kbeads already refuses such a caller on every bead route (`requireRole(RoleViewer, …)`, `kbeads/internal/server/http.go:107-125`; `hasMinimumRole`, `interceptors.go:105`). The Gate serves applicants from a small set of beads-viewer handlers under a new route class `routeApplicant`, which read and write with the viewer's own service identity and only ever touch the caller's own application. | The safe default already exists: a self-registered user can read nothing. The Gate adds the narrowest possible door instead of a new role. |
| B-D2 | Tenants' roles | A tenant holds exactly `gardenio-project-member` through `/project-<land>` and the static group `/lands-tenants`; never `gardenio-viewer`, `gardenio-operator` or `gardenio-mention`. A test asserts it on every provisioned land. | An OIDC caller who is not a scoped principal and has no `allowed_projects` reads unrestricted (`kbeads/internal/server/read_authz.go:59-65`). Holding `gardenio-viewer` without `gardenio-project-member` would make a tenant a global reader. |
| B-D3 | Repo control | Proven by the applicant installing a **separate Gardenio Lands GitHub App** on the repo, with "request user authorization during installation" on. The setup callback gets `installation_id` plus a user-to-server code; the server checks that the authorizing GitHub user is the applicant's linked GitHub account and has admin on the repo. | Q2/Q6. A separate App keeps strangers from installing the fleet's App, which carries the fleet's broad permission set (`internal/githubapp/githubapp.go:227-237`) and webhook. |
| B-D4 | The application | A new bead type `plot_application`, labelled `project:lands-intake` (an operator-only project), created by the Gate handler, never by the applicant's own bearer. | A bead with no `project:` label is visible to every scoped principal (`CS §6.2` rule 1). The intake project keeps applications invisible to tenants of other lands. |
| B-D5 | Approval | The Gate files a **decision** (options `approve`, `deny`, `ask`) addressed to the operator, with the application as its subject. Resolving it from Slack, the Commons, a phone or `/decisions` drives the application's state. | Q3. Decisions already reach the operator everywhere, with DM-first delivery and phone-answerable cards. A second approval queue would duplicate that. |
| B-D6 | Runtime Keycloak groups (0d gap 1) | The **provisioner calls the Keycloak admin API** with a dedicated confidential client `gardenio-lands-provisioner`. It creates `project-<land>` (role `gardenio-project-member` only) and manages membership. It never creates groups outside the `project-` prefix and never touches roles. | Membership has to be the admin API either way: brokered GitHub users are created by Keycloak, not by `KeycloakRealmUser` CRs, so a CR cannot add them to a group. One credential and one code path beats a CR for the group plus the API for membership. The chart's `projectGroups` list (`templates/keycloak/groups.yaml:36-60`) stays for the operator's own projects. |
| B-D7 | The purse (Q1, 0d gap 3) | **A credit is a marked-up unit** (OQ1 gd-7sTt20714N): 1 credit = US$1 billed to the tenant, and a tenant land is charged its LLM spend × 1.5 (rate decision gd-XQmEoHAqmp, `single`). The multiplier lives in a versioned **rate table** (`credit_rate` beads), not in code: none ships, and grants are refused until the operator sets one. The operator grants credits with `gb lands credit add` and each grant is a `credit_grant` bead in credits, carrying the rate version in force. Every land has **its own LiteLLM virtual key and account bead**; the balance is the sum of grants minus the charged spend (key spend × multiplier) minus the live estimate of the land's running agents. The key's LiteLLM `max_budget` (US dollars, at cost) is granted credits ÷ multiplier, rounded down, as a second wall. The **hard wall** (refuse spawns, park agents) is Phase 8's and lives in the controller's spawn admission plus a purse watcher. The optional `budget_usd`/`budget_period` from gd-SAPwFE4FyW stays as a rate cap and its `remaining_usd` reads the same figure for tenant lands. | The owner chose prepaid credits, and a ledger of grants is the audit trail a product needs. A dedicated key makes the spend exact (the account spend sync already reads `GET /key/info` every 5 min, `cmd/controller/spend_sync.go:103-107`) instead of a share of a pooled account, and LiteLLM refuses calls past `max_budget` even if every gardenio check failed. |
| B-D8 | The steward | A **gardener bound to a land** (a project), not to a plan, running a new `steward` role under the restricted tier. It is created by the provisioner and woken by land events and a daily schedule. | The plan-gardener machinery already gives a long-lived, wakeable, operator-steerable agent. Binding it to a land is the only new thing. |
| B-D9 | Tenant namespaces | `land-<name>`, created by the provisioner from one template: Namespace, ResourceQuota, LimitRange, default-deny NetworkPolicy plus explicit egress, a ServiceAccount with no RBAC and `automountServiceAccountToken: false`. Each land namespace gets a RoleBinding that gives the controller one fixed ClusterRole (pods, secrets, configmaps, events) there and nowhere else (§5.2). | Q4. Kubernetes RBAC cannot select namespaces by label, so a per-namespace binding to one reviewed ClusterRole is the narrowest grant that lets one controller run pods in many tenant namespaces. |
| B-D10 | Email | One transactional mailer (`internal/mailer`, SES) built in Phase 7. The Gate, the purse and support use it, and Commons step 4.7d's email digest reuses it. | Nothing in the code, chart or terraform sends email today. The Gate cannot tell a stranger "approved" without it. |
| B-D11 | Tenant land conversation never reaches the fleet Slack | Tenant lands are created with `land_slack_mirror=off`. The bridge refuses to mirror a channel whose land is a tenant land, and only owner-audience escalations (#756's `audience`) held by `operator` reach the operator's Slack. A tenant land's merge escalations route to `steward:<land>` (when the land delegates, `desk` audience) or `land-owner:<land>` (`owner` audience, that land's owners) by the escalation policy (escalation-routing plan, D7 rule 2 and D11), so they never do. | Q8a keeps Slack as a permanent per-land mirror, but the operator's workspace is not a place for a tenant's conversation. |

## 2. Phase 7, Gate (gd-pqJennUltH)

### 2.1 What exists

- **The realm** is an EDP keycloak-operator `KeycloakRealm` CR (`gardenio/helm/gardenio/templates/keycloak/realm.yaml:7`).
  Registration is `login.userRegistration` (`realm.yaml:43`), default `false`
  (`values.yaml:14190`), and the live stack keeps it off. `verifyEmail: true` is set, but no
  SMTP server is configured anywhere.
- **Identity providers** are `KeycloakRealmIdentityProvider` CRs
  (`templates/keycloak/identity-providers.yaml`). The chart default is a GitLab OIDC broker
  (`values.yaml:14781`); the live stack sets `identityProviders: []`. There is no GitHub IdP.
- **No default role.** `gardenio-viewer` is no longer a default role (gd-zuV3EfwrzB,
  `values.yaml:14302-14306`). A new realm user holds no gardenio role.
- **The user bead is auto-created** on a user's first OIDC request, keyed by the `email` claim
  (`kbeads/internal/server/user_provision.go:64-100`), and project groups sync into
  `allowed_projects`. Identity fields on a user bead are admin-only
  (`gardenio/docs/security/user-identity-fields.md`), so the Gate must not try to write
  `github_username` with a machine token.
- **The edge** admits `gardenio.clonk.tv` only for hand-typed groups:
  `allowed_groups=/gardenio-admins,/project-claimlayer`
  (`gardenio/helm/gardenio-ziti/public-edge.yaml:417`, and `viewer-authproxy.yaml:223`), checked
  by `scripts/check-viewer-edge-groups.py`. Rate limiting exists only for `api.clonk.tv /v1/`
  (`public-edge.yaml:131`). There is no captcha; CrowdSec is available but off
  (`templates/edge-crowdsec.yaml:22-25`).
- **The fleet GitHub App** mints per-agent installation tokens
  (`cmd/controller/github_app_credentials.go`, behind `GITHUB_APP_ENABLED`), finding the
  installation with `GET /repos/{o}/{r}/installation` (`internal/githubapp/githubapp.go:194-216`).
  github-bridge handles PR, review, comment and pipeline events but not `installation` or
  `installation_repositories`.
- **Approval prior art:** decisions (DM-first delivery, phone cards, `/decisions`), escalations
  (`POST /v1/escalations/{id}/approve`), and the "born inert" request types
  `investigation-request` and `scrub-request` (`kbeads/internal/model/bead.go:51-70`).

### 2.2 Registration

1. **GitHub IdP.** Add a `github` entry to `keycloak.identityProviders` using Keycloak's
   built-in `github` provider, `syncMode: IMPORT`, the stock `first broker login` flow, and
   mappers that copy the GitHub numeric id and login into user attributes `github_id` and
   `github_login`. A protocol mapper puts both in the access token. The OAuth app is created by
   the operator on github.com (an escalation: client id and secret into a secret).
2. **Verified email only.** gd-rnklckVFKS lands first: kbeads and the viewer reject a token
   whose `email_verified` is not `true`. Brokered GitHub users get `emailVerified` only from
   GitHub's verified primary email (`trustEmail: true` on the GitHub IdP and nowhere else). A
   GitHub account with no verified email cannot register, and the login page says why.
3. **Who may register.** Anyone with a GitHub account. The `first broker login` flow creates the
   realm user with no group and no role (B-D1). Local username/password registration stays off
   (`userRegistration: false`): GitHub is the only way in (Q2).
4. **What a new user sees.** The edge sends a signed-in user with no admitted group to
   `/lands/`, the front of house (§2.3), instead of a 403. The edge change: `gardenio.clonk.tv`
   admits `/gardenio-admins`, `/lands-tenants` and the operator's project groups; a new
   location `^~ /lands/` admits any signed-in realm user. Adding `/lands-tenants` once removes
   the need to edit the edge for every new land, which hand-typed `allowed_groups` would
   otherwise require.
   **Amended 2026-10-28 (owner ruling gd-hCMlZ5jnvB = b, gd-ycE484kLyk):** GitHub sign-in is
   invite-only, and invites go out by email (`gb lands invite`, `type=invite`). `^~ /lands/`
   admits the viewer's groups plus `/invited`, which a person joins when their invite's email
   matches their verified, GitHub-linked user; anyone else signed in gets a "You need an invite"
   page. Item 3's "anyone with a GitHub account" now applies only to who may hold a realm user.
   See [provisioner.md](../../lands/provisioner.md#sign-in-invites-gd-yce484klyk).
5. **Abuse at the door.** GitHub is the bot filter in v1; no captcha. Per-IP `limit_req` on
   `auth.clonk.tv` and `/lands/`. The application form shows the operator the GitHub account's
   age, public repo count and followers, so a throwaway account is visible at approval time.
   CrowdSec stays the operator's switch.

### 2.3 Front of house: `/lands/`

Served by beads-viewer. The landing page and the three documents are `routePublic`; every other
route is a new class `routeApplicant` (any signed-in OIDC caller, including one with no role). Every page is server-rendered, phone-first, in the Dawn Meadow look the login page
already wears.

| Route | What it does |
|---|---|
| `GET /lands/` | Landing: what a land is, the tagline, prices in words ("prepaid credits granted by the operator"), links to terms. Signed-out visitors see it too: the edge lets `/lands/` and the documents through without a session. |
| `GET /lands/terms`, `/lands/privacy`, `/lands/aup` | The current versions of the three documents (gd-fyboiKszlg), each with its version id. |
| `POST /lands/accept` | Records acceptance of the current versions on the user bead (`terms_version`, `terms_accepted_at`). Written by the viewer's machine principal through a new kbeads endpoint that only writes those two fields for the caller's own user bead (`user-identity-fields.md` keeps identity fields admin-only; these are not identity fields). |
| `GET /lands/apply` | The application form (§2.4). Requires accepted current terms. |
| `GET /lands/github/setup` | The GitHub App setup URL (B-D3). Verifies the installation and the authorizing user, stores the result in a short-lived signed cookie, returns to the form. |
| `POST /lands/apply` | Validates and creates the `plot_application` and its decision. |
| `GET /lands/applications` | The caller's own applications and their state. Read server-side with a filter on `applicant_sub`, never a general list. |
| `POST /lands/applications/{id}/withdraw` | Withdraw a submitted application. |

When a user already belongs to one or more lands, `/lands/` shows them and links to
`/garden?land=<name>` (§3.6).

### 2.4 The plot application

`type=plot_application`, registered in kbeads' type config, label `project:lands-intake` (B-D4).

| Field | Meaning |
|---|---|
| `applicant_sub` | Keycloak subject of the applicant (stable; the email is a label) |
| `applicant_email`, `github_login`, `github_id` | Copied from the verified token at submit time |
| `land_name` | Requested name: DNS label, 3–30 chars, not a reserved or existing project name |
| `repo` | `owner/name` on github.com |
| `repo_private` | From GitHub at verification time |
| `github_installation_id` | From the setup callback, verified (B-D3) |
| `purpose` | Free text, 1000 chars max |
| `credits_requested_usd` | What the applicant asks for; the operator grants the real amount |
| `terms_version`, `aup_version` | Versions accepted at submit |
| `github_account_age_days`, `github_public_repos` | Signals for the operator (§2.2 item 5) |
| `state` | `submitted` → `approved` / `denied` / `withdrawn` → `provisioning` → `provisioned` / `failed` → `closed` |
| `decision_id` | The approval decision (B-D5) |
| `decided_by`, `decided_at`, `deny_reason`, `credits_granted_usd` | From the decision |
| `project` | The land's project name, once provisioned |

Limits: one open application per applicant; at most N submitted applications across all
applicants per day (a config value, default 20); the repo must not already back a land or an
open application.

### 2.5 Approval

`POST /lands/apply` files a decision with `--requesting-task` set to the application. Its
context renders the application (applicant, GitHub signals, repo, privacy, purpose, credits
asked). Options:

- `approve`: the operator's free text, if any, may carry the credit grant (`credits=50`);
  without it the default grant from config applies. The state becomes `approved` and Phase 8's
  provisioner picks it up. Until Phase 8 ships, `approved` is a resting state.
- `deny`: the free text is the reason the applicant sees.
- `ask`: the free text is mailed to the applicant, and the application stays `submitted`.

A small reactor (in the viewer, beside the plan-step reactor) watches resolved decisions whose
subject is a `plot_application` and applies the state change. The applicant is emailed on every
state change (B-D10), and `/lands/applications` shows the same words.

### 2.6 Terms, privacy and AUP (gd-fyboiKszlg, scope refined)

The minted step keeps its title, with its scope narrowed to the documents:

- Drafts of the three documents as Markdown under `gardenio/docs/lands/legal/`, each with a
  `version` front-matter field. The privacy policy names what is stored: the GitHub identity
  (id, login, verified email), repo access through the App, agent transcripts and terminal
  recordings (operator-visible, Q8d), conversation beads, usage and credit records, and how long
  each is kept after a land closes.
- The AUP names what agents may not be pointed at (other people's systems, credential stuffing,
  mining, spam, anything illegal) and what happens on abuse (freeze, close, report).
- One owner decision for what only the owner can set: the legal entity, jurisdiction, liability
  cap, retention period after a land closes, and whether private repos are allowed in v1 (§8).
- The serving and acceptance plumbing is not in this step; it is step 7.5.

### 2.7 Mailer

`internal/mailer`: an interface with an SES implementation and a recorder fake; templates for
application received, approved, denied, question asked, land ready, purse low, purse empty, land
frozen and land closed. The sender domain and an IRSA role with `ses:SendEmail` limited to that
identity are operator work (an escalation). Keycloak itself does not send mail in v1: there are no
passwords to reset and no emails to verify, because GitHub verifies them.

## 3. Phase 8, Deed and purse (gd-KiiqMwbZLu)

### 3.1 What exists

- **A project** is a `type:project` config bead (`kbeads/internal/model/bead.go:77`, schema
  `kbeads/internal/server/config.go:941-990`: `prefix`, `git_url`, `default_branch`,
  `account`, `secrets`, `repos`, `slack_channel`, `intake_taints`, `triggers_enabled`,
  `schedules_enabled`, …), admin-written. Creating one is manual (`kd create … --type=project`,
  `gardenio/docs/onboarding-new-project-and-role.md:32`), and so are its LLM account bead and
  git secrets (AWS Secrets Manager + ESO). There is no `gb project` command.
- **Accounts and spend.** An agent's LLM account is role-pinned or spread over the project's
  healthy pool (`applyAccountConfig`, `cmd/controller/podspec.go:1871`; `account_spread.go:77`).
  Account beads carry `litellm_key` and `max_budget`; the controller syncs each key's spend onto
  its account bead every 5 min (`spend_sync.go:103-107`, `:204-211`). Sessions settle exact
  spend from LiteLLM tags at close (`reconcile_session_spend.go`), and pods report
  `live_cost_usd` while running (`internal/statusreporter/reporter.go:931`). `gb` can mint
  budget-capped keys (`cmd/gb/litellm_key.go:17-53`).
- **No project budget exists yet.** gd-SAPwFE4FyW adds `budget_usd`/`budget_period` and a
  display-only `remaining_usd`; its own text rules out enforcement. The only enforced dollar cap
  is the dark-gardener per-day guard (`internal/bridge/dark_agent_budget.go`), which stamps
  `agent_state=budget_exceeded` and skips the gardener's next fire.
- **Spawn admission** in the controller's `AgentSpawn` case
  (`cmd/controller/main.go:2624`): pod caps `COOP_MAX_PODS`/`COOP_MAX_PODS_PER_PROJECT`
  (`:2626-2656`), the credential guard (`:2751`), then `SpawnBlocked` with `agent_state=queued`
  when the account pool is exhausted (`:2765-2775`). The reconciler repeats the caps
  (`internal/reconciler/reconciler.go:1149-1198`). Gardeners pass `gardenerPreFireCheck`
  (`internal/bridge/gardener_evaluator.go:142`).
- **GitHub tokens** come from one App (`GITHUB_APP_ID`, `GITHUB_APP_INSTALLATION_ID`), scoped to
  the project's repo plus same-owner reference repos
  (`cmd/controller/github_app_credentials.go:148-168`).
- **`/garden`** has one region per project (`internal/gardenhub/web/world.js:130-132`); there is
  no purse display and no `gardeners[]` yet (gd-DKMSc1Y91I adds it).

### 3.2 Land fields on the project bead

A land is a project (CS C2). Track B adds, all admin- or provisioner-written:

| Field | Meaning |
|---|---|
| `land_kind` | `operator` (default, today's projects) or `tenant` |
| `land_owner_sub` | Keycloak subject of the owner |
| `land_application_id` | The `plot_application` it came from |
| `land_opened_at` | When provisioning finished; the purse counts spend from here |
| `land_frozen` | Kill switch (§5.5) |
| `land_slack_mirror` | From CS §2.1; `off` and locked for tenant lands (B-D11) |
| `github_app` | `fleet` (default) or `lands`: which App mints this project's tokens |
| `github_installation_id` | Pinned installation for the land's repo |
| `max_live_agents` | Per-land override of `COOP_MAX_PODS_PER_PROJECT`; default 3 for tenant lands |
| `namespace`, `runtime_class` | Phase 10 (§5.2) |

Step 8.1 registers all of them on `type:project` (kbeads builtin and the controller's copy in
`internal/bridge/init.go`); kbeads refuses `land_slack_mirror=on` on a project with
`land_kind=tenant` (`kbeads/internal/server/lands_ledger.go`).

### 3.3 Provisioning a plot as code

A provisioner in the controller (`internal/lands/provision`) reconciles every
`plot_application` in state `approved`. Each step is idempotent, records itself in
`provision_step` on the application, and is safe to re-run after a crash:

1. **Project bead** `<land>`: `git_url` from the repo, `default_branch` from GitHub, the land
   fields above, `prefix` derived from the name.
2. **Purse:** a LiteLLM virtual key with `max_budget` equal to the grant (via the same
   `/key/generate` call `gb` uses), stored as an account bead `lands-<land>` with the key in a
   per-land secret; the project's `account` pins it. The initial `credit_grant` bead records
   the operator, the amount and the decision id.
3. **Keycloak** (B-D6): group `project-<land>` with realm role `gardenio-project-member` only;
   the applicant joins it as owner (Commons 4.2's owner sub-role) and joins `/lands-tenants`.
4. **Channels:** the Commons seeding function from Commons step 2.3 (gd-FFpayTSTK0), called with
   `mirror=off`, plus `#support` (§6.1).
5. **Namespace and runtime** (Phase 10, §5.2). Until Phase 10 ships, provisioning stops after
   step 4, and tenant spawns stay off (`lands.tenantSpawns`, a controller setting that defaults
   to off, §3.5).
6. **Steward** (Phase 9): create the land-bound gardener and wake it once.
7. **Mail** "your land is ready" with the `/garden?land=<land>` link; state → `provisioned`.

A failed step sets `failed` with the error, files one operator escalation carrying the
application id and the step, and stops; `gb lands provision --retry <application>` resumes from
`provision_step`.

**Deprovision** (`gb lands close <land>`, operator only) runs in reverse: freeze (§5.5), stop
agents, remove members from the group, revoke the LiteLLM key, delete the namespace, close the
project bead with `land_closed_at`. Beads are kept for the retention period the terms set
(gd-fyboiKszlg), then purged by a sweep that is its own later step. The tenant is mailed, and
told to uninstall the App.

### 3.4 The purse

`internal/lands/purse` computes, per tenant land:

All figures are credits; `m` is the `llm_multiplier` of the rate in force.

```
granted   = sum(credit_grant.amount_credits)
spent     = m × account(lands-<land>).total_cost_usd      (exact, ≤ 5 min old)
in_flight = m × sum(live_cost_usd − baseline) over the land's running agents
remaining = granted − spent − in_flight
wall      = remaining ≤ margin,  margin = max(2 credits, 5% of granted)
```

The baseline is each agent's `live_cost_usd` when the spend sync last read the land's key: the
sync stamps `spend_synced_at` and `live_cost_baseline` (agent id → live cost, read just before
the key) on the `lands-<land>` account bead, so in-flight counts only spend the synced figure
does not hold yet (step 8.2).

The margin covers the 5-minute spend-sync lag plus one gardener's next few calls; LiteLLM's
`max_budget` on the key stops the rest. If the spend source is unreachable for more than 15
minutes, the purse is **unknown**, and unknown counts as the wall for new spawns (running agents
continue). Fail closed is the rule for a tenant's money; the operator's own lands keep today's
behaviour.

The figure is exposed three ways: in the gardenhub per-project cost entry that gd-T6zwDjWLBI
adds (one number on `/garden` and `/costs`), as `GET /api/lands/{land}/purse` for the steward
and `gb lands purse`, and as notices: a line in `#general` at 50, 80 and 100 percent, and mail
to the owner at 80 and 100.

**Credits.** A credit is a marked-up unit, not a dollar of LLM spend at cost (OQ1
gd-7sTt20714N): 1 credit = US$1 billed to the tenant, and tenant LLM spend is charged at
× 1.5 (gd-XQmEoHAqmp, `single`; no per-agent-hour compute charge). The rate is data, not code:
a **rate table** of versioned `credit_rate` beads (`version`, `effective_at`, `llm_multiplier`
= credits per US$1 of LiteLLM spend, `compute_credits_per_agent_hour`, default 0), written by
`gb lands rates set` and read by `gb lands rates show` (admin only). The highest version is in
force. None ships: until the operator sets the first version (step gd-lN2ENPcFyE), every grant
is refused, so no grant is priced on a guessed number.

`gb lands credit add <land> <credits> --reason "<text>"` (admin only) writes a `credit_grant`
bead (`amount_credits`, `reason`, `granted_by`, `decision_id`, `land`, `rate_version`, labelled
`project:<land>`) and sets the land key's LiteLLM `max_budget`, which is US dollars, to the
whole ledger at cost: granted credits ÷ `llm_multiplier`, rounded down to the cent (it is the
second wall). A grant that fails to reach LiteLLM is rolled back. A rate change re-derives
`max_budget` on every tenant land key by the same rule (`gb lands rates set`, and
`gb lands rates reapply` to retry a key that failed). `gb lands credit list <land>` prints the
ledger in credits and the at-cost US dollar equivalent, the operator's second figure. A negative
grant (a refund or a correction) needs a reason; SLA credits (step 10.9) are ordinary grants
whose reason names the incident. Grants and rate versions are never edited.

### 3.5 The hard wall

- **Spawn admission.** In the controller's `AgentSpawn` case, next to the per-project cap
  (`main.go:2626-2656`) and in the reconciler's twin (`reconciler.go:1172-1198`): a spawn for a
  tenant land is refused when the land is frozen, when `lands.tenantSpawns` is off, or when
  the purse is at the wall or unknown. The agent bead gets `agent_state=budget_exceeded` (already
  in the enum, `kbeads/internal/server/config.go:630`) and a comment saying which, and the
  spawner sees it: `gb spawn` prints it, and the Commons shows it on the compose box.
  One decision serves every door (`internal/lands/wall`, step 8.3). `lands.tenantSpawns` is the
  controller env `LANDS_TENANT_SPAWNS`. The refusal is recorded in `spawn_wall` (the code:
  `land_frozen`, `tenant_spawns_off`, `purse_wall`, `purse_unknown`, and since 10.4-10.5 `spawn_rate`,
  `lands_paused`, `land_gone`) and `spawn_wall_reason`;
  the bead stays open and is re-decided on each reconcile pass. The reconciler asks only for a
  new spawn: recreating a running agent's pod (spot loss, image drift) is not one. A project
  the controller's cache knows as an operator land costs no daemon read; any other project
  whose land cannot be read is refused as unknown.
- **Gardeners.** `gardenerPreFireCheck` refuses to fire a gardener whose project is at the wall
  or frozen, as the dark-budget guard does for dark gardeners. It refuses the same four cases
  as admission; a failed land read fails open there, because admission still fails closed.
- **The purse watcher.** A controller loop (1 minute) parks running agents of a land at the wall:
  a nudge ("the purse is empty, commit and push what you have, then stop"), a 5-minute grace,
  then the suspend path (`agent_state=stopping`, `cmd/gb/stop.go:507`), so the session resumes
  if credits arrive. Each parked agent gets reason `purse_empty` for the reason model
  (gd-TvJosTKfAt). A credit grant wakes the steward (§4.3), which decides what to resume; nothing
  resumes on its own. An unknown purse parks nothing. The watcher (`internal/lands/watcher`,
  step 8.4) keeps its state on beads: `purse_notice_level` and `purse_granted_seen` on the
  `lands-<land>` account bead (a rise in granted credits is a refill), `purse_nudged_at` and
  `park_reason` on the agent bead. Notices at 50/80/100% are said once each and re-armed by a
  refill; 100% is the wall.

### 3.6 The tenant lands on their plot

After provisioning, a tenant who opens `/` or `/lands/` is sent to `/garden?land=<land>`. With
several lands, `/lands/` lists them. `?land=` opens the camera on that land's region and the
Commons pane on its default channel. The per-viewer cut (gd-MnQlEmbUq3) makes that region the
only one they see. The land header shows the purse (remaining, granted, level) next to the
land's name.

### 3.7 Members

The owner invites people by GitHub login from the land header (a lightbox, CS D12). The viewer
resolves the login to a GitHub id when the invite is made, and writes a `land_invite` bead
(project label of the land: `github_id`, role `member` or `guest`, inviter, expiry 14 days).
When someone signs in whose token carries that `github_id`, the provisioner adds them to
`project-<land>` with the role and to `/lands-tenants`, and closes the invite. Owners can revoke a
membership; it takes effect within the 5-minute token lifespan (`values.yaml:14369-14373`). Roles
and what each may do are Commons step 4.2's (gd-9aCcG1zCCY); this step is only the door.

## 4. Phase 9, Steward (gd-G8lmByYEoM)

### 4.1 What exists

- **The plan-gardener** (`internal/plangardener/plangardener.go`): role `plan-gardener` (`:59`),
  a playbook prompt (`PlaybookPrompt`, `:117`, rendered by `RenderPrompt`, `:260`), bound to a
  plan by the `gardener-epic:<plan>` label (`Bind`, `:689`), with run caps (`DefaultMaxRuns=8`)
  and a ~30-minute cadence. A gardener is a `type:trigger` bead with the `role:gardener` label
  (`IsGardenerBead`, `:521`; spec fields in `GardenerSpec`, `internal/bridge/triggers.go:564-700`),
  woken by trigger-engine on a cron or an event (`internal/gardenermode/mode.go:16-24`), with
  run-now and pause controls (`cmd/gb/gardener_firenow.go:49`; viewer
  `POST /gardener/{id}/run-now|pause-toggle`).
- **Roles** are `type:role` beads, admin-written (`kbeads/internal/server/config.go:1150`).
- No `capability_tiers` code exists yet (gd-Bcqj2r4m7E adds it).

### 4.2 What the steward is

One per tenant land, created by the provisioner (§3.3 step 6). It is a gardener bound to a land
(B-D8) running the `steward` role under the `lands-tenant` tier (§5.1). Its work in the land:

1. **Greets.** On the first run it posts in `#general`: what it is, what it can do, the purse,
   how to ask, and where `#support` is.
2. **Turns conversation into plans.** When a member asks for work, the steward writes a plan in
   the land's project (an epic with the `plan` label and steps, the shape plans have today) and
   files a decision to the land's owner: approve, change, or drop. The owner answers in the
   Commons card. Scoped principals may already answer decisions
   (`kbeads/internal/server/scoped_principal.go:67-81`).
3. **Spawns gardeners within the purse.** For an approved plan, it spawns `lands-gardener`
   crews one step at a time, up to the land's `max_live_agents`. Before each spawn it reads
   `/api/lands/{land}/purse` and an estimate for the step: the mean settled cost of the land's
   last 10 gardener sessions, or a configured default before there are 10. If the estimate is
   more than what remains above the margin, it asks the owner instead of spawning. The spawn
   gate (§3.5) refuses anyway when it is wrong.
4. **Reports.** A daily digest in `#general`: what merged, what is in flight, what is waiting on
   whom, spend today and remaining. A quiet day posts nothing.
5. **Parks.** At the wall it posts once ("the purse is empty; work is parked; ask the operator
   for credits in #support"), carries reason `purse_empty`, and sleeps until a credit grant.

It never merges a PR itself. A `lands-gardener` merges its own PR only after the PR's merge
escalation (`github_pr_merge` or `gitlab_mr_merge`) is approved, either by the steward or by a
land owner.

The steward may approve one only when all of these hold:
- the land owner has delegated that operation type to it (`land_steward_approves` on the land's
  project bead, written through `steward_delegate`);
- the escalation is not `operator_only`;
- the requester is a different agent bead from the steward.

Without delegation the escalation waits on the land owner (`held_by=land-owner:<land>`), who
approves or rejects it in Commons. A land owner who merges on GitHub themselves rejects the
escalation with the reason `merged on GitHub`.

**Approval stays inside the land (P10.C gd-wi44L8Qy4I).** A land's merges and its own people
never need the gardenio owner. The steward approves what `land_steward_approves` delegates, as
built: merges by default, plus `create`/`update`/`close` if the owner adds them, never a floor
type. Everything else routed to the land waits on `land-owner:<land>`. That includes a
`keycloak_group_membership` escalation that touches only the land's own groups
(`/project-<land>` and its subgroups, Commons spec §6.4). A land owner approves it on the
escalation's page (P10.B presence). `gl escalation approve` and `kd escalation approve` hand
over the link. A group outside the land, a realm or client role, or any spec the daemon cannot
read as the land's own stays on the owner floor.

The steward does not talk outside its land and never sees another land. It does not file
escalations to the operator; its own needs go through `#support`.

### 4.3 Binding a gardener to a land

`internal/steward` reuses the plan-gardener's builder with three differences:

- **Binding** is the label `gardener-land:<land>` and the trigger bead carries the land's
  `project:` label, so the steward's own beads are inside the land's scope.
- **Wake events** (event mode): a human message in `#general`; an `@steward` mention in any
  channel of the land; a decision of the steward's resolved; a credit grant; a purse threshold;
  a gardener of the land finished. Plus a daily cron for the digest. Runs per day are capped
  (default 24), and the cap is visible on the land header when hit.
- **Prompt** is a land playbook (`StewardPlaybookPrompt`) rendered with the land's name, repo,
  purse, members and open plans, instead of a plan's state.

### 4.4 Where the steward lives

On the map it stands at the land's homestead, using the gardeners layer from gd-DKMSc1Y91I/
gd-e4uXZs8coZ with its own sprite; its reason (gd-TvJosTKfAt) shows on hover and tap
("Parked: the purse is empty", "Waiting on you: approve plan X").

## 5. Phase 10, Fences (gd-Gx6arHBvzJ)

### 5.1 The restricted tier for tenant agents

Built on P9 (gd-aR6UzS0HgM: `capability_tiers` on roles, the spawn gate reading tiers, one
shared restricted-ops check, the untrusted-input taint, the grants ledger). Lands adds one tier
set, `lands-tenant`, held by the `steward` and `lands-gardener` roles. It is modelled on
contributor-local (gd-5UVkKGj9jJ).

| May | May not |
|---|---|
| Read and write beads in its own project (per-agent token bound to the project) | Read or write any other project, any config bead, any user bead |
| `gb say` in its own land's channels | Post to Slack, send email, call any external-send path |
| Push branches and open PRs on the land's one repo with a Lands App token | Use any other repo, a PAT, a deploy key, or the fleet App |
| Spawn `lands-gardener` in its own project (steward only), within the purse | Spawn any other role; privileged spawn; spawn in another project |
| Run tools inside its own pod; file a merge escalation (`github_pr_merge`, `gitlab_mr_merge`) for a PR in its own land's repo | File any other escalation; write grants |
| (steward only) approve a merge escalation in its own land whose operation type the land owner delegated (`land_steward_approves`), requested by another agent bead | Approve anything `operator_only`, anything undelegated, anything in another land, or its own request |

The approve check is the daemon's coordinator path (escalation-routing D4/D8);
`POST /v1/escalations/{id}/can-approve` answers it dry-run for any surface.

Every bead in a tenant land carries the untrusted-input taint (gd-hGfgjuviQy), so text a
stranger wrote can never lift an agent's restrictions, and the shared check (gd-kLkxfyARXz)
returns a structured denial an agent can explain in the Commons.

As built (step 10.1, gd-g77WcV22SK; P9 was dropped, so 10.1 built the parts it needs): the
shared check is `kbeads/internal/server/restricted_ops.go`, and the "May" column above is its
`landsTenantRules` table. A tenant agent spawns as before; files only a merge escalation whose
one project is its land (the X-Escalate retry, which files replays and grant requests, stays
closed); approves only as the steward, as before; and creates, updates or closes no bead of a
type that configures the fleet (config, user, role, advice, trigger, gate, project and the
like), even in its own project. Refusals are 403 `denied_reason: lands_tenant`, not
escalatable, pointing at the land's owner or `#support`. The rows the daemon does not rule on
are enforced where the resource is: other projects by the per-agent token's project binding,
external send by the bridge's land fence (`fenceTarget`), repos by the pod allowlist (10.3).
The taint is `untrusted-input` with effect `restrict`, applied to every bead created in a tenant
land once the operator has registered it (taints ADR §15.7); an agent carrying it has approved
escalations refused at replay, and a fleet agent carrying it is also refused spawn, approval and
fleet-type and config writes (`denied_reason: taint_restrict`, gd-gaGdksntAG). A merge made with
an agent's own repo token is beyond it. There is no grants ledger:
taint grants stay in `taint_grants`.

### 5.2 A namespace per plot (Q4)

The provisioner applies one template per land:

- `Namespace land-<name>`, labelled `gardenio.io/land=<name>`.
- `ResourceQuota`: pods = `max_live_agents` + 1, CPU, memory, ephemeral storage and one workspace claim (count and `requests.storage`) per pod; no Services; capped Secrets and ConfigMaps (gd-pXik57wZA9).
- `LimitRange` with the agent pod's defaults.
- `NetworkPolicy`: default deny ingress and egress. Egress allowed to cluster DNS; the LiteLLM
  proxy; the kbeads daemon; coopmux and the NATS relay (the agent registers its session); the
  bridge's land listener (port 8091: only the per-agent-token routes `gb say` and `gb thread`
  call, never the bridge's full API; gd-pXik57wZA9); and `0.0.0.0/0` **except** the cluster pod
  and service CIDRs, RFC 1918 ranges and `169.254.169.254`, so github.com and package
  registries work and nothing internal does. No ingress except from coopmux to the coop port.
- `ServiceAccount land-agent`, no RBAC bindings, `automountServiceAccountToken: false`
  (already the agent default, `internal/podmanager/manager.go:1712-1722`).
- A `RoleBinding` that gives the controller's ServiceAccount a fixed ClusterRole
  `gardenio-land-agent-manager` (pods, secrets, configmaps, events) in this namespace only.

The controller places a tenant land's pods in the project's `namespace` (the pod spec already
honours a bead-level namespace, `cmd/controller/podspec.go:1645-1650`) with
`runtime_class=runsc`: tenant code runs under gVisor (superseded: tenant pods run on runc by
the owner's ruling gd-iyZom5u11A, see As built below; the runtime-class order is
project < role < pilot < agent, `podspec.go:327-339`). The controller needs new RBAC to create
namespaces, quotas, limit ranges, network policies, service accounts, and RoleBindings to
that one ClusterRole (`bind` on it, nothing else). That is an operator escalation, as is
gVisor on the k3s nodes and confirming the CNI enforces NetworkPolicy (k3s ships kube-router's
policy controller; `networkPolicies.enabled` is false for the release namespace today,
`values.yaml:16347`).

As built (step 10.2, gd-NH44mohWlL): `internal/lands/landns` renders and applies the
template (namespace PodSecurity `restricted`; quota and LimitRange sized from the agent pod's
2/4 CPU, 8/16Gi; the policy `land-fence`, whose egress peers are kbeads, LiteLLM, coopmux, NATS
and the slack-bridge by component label, overridable with `LANDS_NAMESPACE_EGRESS_PEERS`, and
whose internet rule cuts RFC 1918, 100.64.0.0/10 and 169.254.0.0/16). It refuses to adopt or
delete a namespace not labelled `gardenio.io/land=<name>` by the controller. It is the
provisioner's `namespace` step (between channels and mail), which also writes the project's
`namespace` and `runtime_class` (the land RuntimeClass setting, empty by default), and the `namespace` close step (between key and
project). With `LANDS_NAMESPACES_ENABLED` off the step fails and escalates: no land opens
unfenced. Both pod spec builders end with `applyLandFence`: a tenant land's pod goes to
`land-<land>`, derived from the land's name (the bead's `namespace` field is never trusted for
placement), under the land RuntimeClass (`landNamespaces.runtimeClassName`, default `""`: runc,
the owner's ruling gd-iyZom5u11A; `runsc` stays selectable) as `land-agent`, on Kubernetes,
with no API token and no thaw-state hostPath (so no freeze; it resumes cold), whatever a role
or agent pin says.

Because runc shares the node kernel, placement makes up for it (gd-NDJIOutLsv,
`podmanager.LandTenantAffinity`). A tenant pod is required off the k3s on-demand platform pair
(`gardenio.io/lifecycle=on-demand`), control-plane nodes and per-MR nodes, and off any node that
runs a fleet agent pod (`gardenio.io/agent` in a namespace without `gardenio.io/land`). That
anti-affinity is required, so the scheduler also keeps fleet agents off a node that runs a
tenant pod. A tenant pod always takes the lowest agent PriorityClass (preemption `Never`), so a
tenant thread, dm or gardener role cannot preempt fleet crew. Land pods count toward
`CoopMaxPods` and the land's per-project cap at both the event door and the reconciler. Tenant
pods from different lands may share a node. There is no dedicated tenant node pool (a
`nodeSelector` plus a taint only land pods tolerate): that needs nodes the owner has not
budgeted, and is OQ4's question. Admission refuses
a tenant spawn with `land_unfenced` while the project cache does not know the land as a tenant
whose bead names its own `land-<land>`. The chart (`landNamespaces`, off) renders the two
ClusterRoles, `bind` on `gardenio-land-agent-manager` only, and three ValidatingAdmissionPolicies
with no switch of their own: the controller's writes stay in `land-*` namespaces; nobody binds
`gardenio-land-agent-manager` by a ClusterRoleBinding or outside a `land-*` namespace (the
`bind` is cluster-wide, and the controller holds unscoped ClusterRoleBinding `create` under
`agents.clusterAdmin`); and every pod created in a `gardenio.io/land` namespace runs under
the land RuntimeClass as `land-agent` with no token and no hostPath.

### 5.3 What a tenant pod holds

Only: a per-agent daemon token bound to its project (no shared static token, gd-ke6PC7bRto); the
land's LiteLLM key; a Lands App installation token for the one repo (no same-owner reference
repos for tenant lands); its coop credentials. No AWS role, no fleet secrets, no Slack or
GitHub bridge tokens. A test renders a tenant agent's pod spec and fails on any env var, volume
or projected secret not on that allowlist.

As built (step 10.3, gd-R51lnWgPc8): `applyLandFence` ends with `fenceTenantPodContents`
(`cmd/controller/podspec_land_allowlist.go`), so both spec builders strip a tenant pod to the
list. Secret env keeps only `COOP_BROKER_TOKEN`/`COOP_MUX_TOKEN` from the coopmux Secret and
`CLAUDE_CODE_OAUTH_TOKEN` from the `lands-<land>` account's own credential; plain env keeps
only `tenantPodEnv` (daemon and coop addresses, the land account's LiteLLM base URL, key and
model, `SLACK_BRIDGE_URL` for `gb say`, public links, the agent's prompt and thread). The shared
daemon token, every git credential and reference repo, the fleet Claude credentials file, the
object-store keys, the Slack viewer session, ConfigMaps, SSH keys, image pull secrets, Ziti,
the DinD and Postgres sidecars and the coverage PVC all go; the doors then mount the per-agent
daemon token and the Lands App token. NATS keeps only the subject-isolated agent user's token
(with isolation off the pod gets none). A fleet role's pinned account is never read for a
tenant land (`roleAccountPin`), and a pod that did not resolve the land's own account loses
the account env too. The wall refuses a tenant spawn with `land_unfenced` while
`PER_AGENT_TOKENS` is off. What it found and removed: the Slack and Jira bridge tokens, the
Grafana key bundle, the shared daemon token, project and role secrets (git PATs included),
the MinIO agent keys, `BOAT_PROJECTS` (every fleet repo), internal URLs (Grafana, logs, vmagent,
the viewer's in-cluster address, the Jira bridge), and a role-pinned fleet LiteLLM key.
`TestTenantPodAllowlist` renders the Pod through both builders and `podmanager.BuildPod`, with
every scalar controller setting filled in, and fails on anything else.

A tenant pod holds no fleet token but the Claude credential (gd-gokoYMl5px, gd-ApgYe740ps,
owner rulings gd-E07QvX3Vum and gd-NXTrUXD1Sq: "Claude credential will have to stay or we
have no way to function"). Besides the doors' per-agent daemon token and Lands App token it
reads two Secrets:

- **Its land's coopmux token.** The controller mints
  `coopmux-land.v1.<land>.<base64url HMAC-SHA256>` under the key in
  `COOP_MUX_LAND_TOKEN_KEY_FILE` (`internal/lands/landtoken`, the contract coopmux verifies,
  gd-zVTBvaSIdG) into `coopmux-land-token` in `land-<land>`, and the pod reads it as
  `COOP_MUX_TOKEN`/`COOP_BROKER_TOKEN`. coopmux scopes it to the land's own sessions and keeps
  it off the credential endpoints. The fleet's static token (`cfg.CoopmuxTokenSecret`, which
  the chart default falls back to the shared daemon token) never reaches a tenant pod. With no
  key configured the pod gets no coopmux token and no coopmux URL, and does not register.
- **Its account's Claude credential**: the release-namespace Secret its `lands-<land>` account
  names (`landCredential`), copied into `land-<land>` with the one key it is read by, if it is
  one of exactly two: the `LANDS_ACCOUNT_TEMPLATE` account's credential (today the fleet
  `claude-oauth` the provisioner copies to every land account) or `secret:lands-<land>`
  (`landCredentialSecret`), the land's own, which an operator may repoint one land at. A
  `lands-<land>` account whose `credential` names any other Secret (the fleet coopmux or daemon
  token, another land's), a forbidden name or no Secret blocks the spawn with a reason that says
  what to set, so the fence fails closed; an account with no credential bills through its LiteLLM
  key alone. `refuseBeadSecret` lets a `lands-<land>` Secret be read
  only as the credential of the account of that name, and refuses it to every project, role and
  other account.

No fleet NATS token reaches a tenant pod. The agent user is one fleet-wide user that may
subscribe to `coop.mux.session.*.input` and publish `coop.mux.session.>` (10.7 review,
gd-MiaGhgmZ4L), and the legacy token is wider. With `landCallout` on, a tenant pod gets its
land's own NATS user instead (gd-sS7bSXOdGs, `internal/lands/natsland`): user `land-<land>`,
password `landtoken.MintNATS` in the `nats-land-token` Secret, publish only under
`coop.events.land.<land>.>`, subscribe to nothing.

- **Revoking one land's NATS password** (gd-zo4W7Q8Yno): bump the `nats_epoch` field on the
  land's project bead (`kd update <project-bead> -f nats_epoch=<n+1>`; unset is 0). The
  password is a MAC over the land and its epoch, so the callout refuses the old one at the next
  connect, once the project cache refreshes. A live connection ends when its user expires
  (`natsland.DefaultTTL`, 15m), and the land secret sync writes the new password for the land's
  pods. A `nats_epoch` that is not a decimal integer refuses the land outright. Rotating
  `COOP_MUX_LAND_TOKEN_KEY_FILE` is still the way to revoke every land at once.
- **Consuming land events**: no fleet code subscribes `coop.events.land.>` yet. The first
  consumer moves `parseLandMessage` from `natsland_test.go` into `natsland` and reads every event
  through it: it takes the land from the subject and drops an event whose actor is not that
  land's agent. The payload stays untrusted tenant content.

The fence lists the credential with the one key it is read by (`tenantSecretCopies`,
`AgentPodSpec.LandSecretCopies`) and sets `AgentPodSpec.LandCoopmuxToken`.
`podmanager.CreateAgentPod` writes both just before its `verifySecretsExist` gate, so every
door (event spawn, stuck restart, reconciler) reads the current value. A copy is labelled
`gardenio.io/type=land-secret-copy`, the token `land-coopmux-token`, and a Secret of either
name the controller did not write is never overwritten. The spawn credential guard probes the
credential in the release namespace, where it is copied from. `runLandSecretSync`
(`cmd/controller/lands_sweep.go`) brings each land in line with `landSecretsFor` every five
minutes, from the controller's config and the land's account, never from a copy's
`copied-from` annotation: a rotated credential or key reaches the land, and a copy the land's
account no longer names, or whose source is gone, is deleted, as is the token once no key is
set. What the sweep cannot tell it leaves alone: with no account cache entry for the land the copies stay, and a failed mint keeps the token while the copies are still reconciled. Env-sourced tokens are read at container start, so a running pod keeps the old value
until its next pod. Everything goes with the namespace when `gb lands close` deletes it. No
new RBAC: `gardenio-land-agent-manager` already grants Secrets in a land namespace.

### 5.4 Abuse controls

- Caps per land: `max_live_agents` (default 3); spawns per hour (the controller gains the
  per-key `spawnguard`, `internal/spawnguard/spawnguard.go`, keyed by land); Commons messages
  per member per minute; repo size at application (refuse above a configured size).
- Alerts: spend velocity per land (dollars per hour above N times the land's 7-day mean), purse
  wall hits, egress bytes per land namespace, spawn refusals per land. They route to the
  operator like today's alerts.
- Abuse handling follows the AUP (gd-fyboiKszlg): freeze first, then ask.
- As built (step 10.4, gd-l8ltROhREx): the spawn rate is `spawnguard.Keyed`
  keyed by land and agent bead, decided after the spawn wall at both pod doors
  (`LANDS_SPAWNS_PER_HOUR`, default 12, 0 off; refusal code `spawn_rate`, so the bead
  waits at `budget_exceeded` and starts when the window frees). It is in memory, so a
  controller restart resets it. Commons says and replies are capped per member per land
  (`COMMONS_MESSAGES_PER_MINUTE`, default 20) with a 429. A new `@gardenio` mention that
  would mint a wish head is capped per member per tenant land in the slack-bridge
  (`COMMONS_SPAWNS_PER_HOUR_PER_MEMBER`, default 4, 0 off; gd-qBvHX9jigO), before the head
  is minted, with a 429 that says when a slot frees; operator lands are exempt. The repo size check runs in the
  Lands App setup leg (§2.3), the only place the application gets its repository
  (`LANDS_MAX_REPO_SIZE_MB`, default 1024, refusal `repo_size`). The alerts are one
  PrometheusRule, `agents.metrics.prometheusRule.landsAbuse` (off by default), over the
  land's account gauge, the controller's `gardenio_lands_spawn_refusals_total{land,code}`
  and cadvisor's `container_network_transmit_bytes_total` for `land-*` namespaces.

### 5.5 Kill switches

- **Per land:** `gb lands freeze <land> --reason` sets `land_frozen`: spawns refused, the purse
  watcher's park path runs for every agent, the steward's gardener is paused, Commons writes for
  the land's members are refused with a banner. `gb lands thaw` reverses it. The operator also
  gets a Freeze button on the land header.
- **All lands:** a `type:gate` bead `lands-pause`, read like `gardeners-pause`
  (`internal/pausegate/pausegate.go:47-50`) with one difference: the existing gates **fail
  open** (`gardener_bounds_guard.go:260-264`), and this one fails closed for tenant spawns. If
  the gate cannot be read, no tenant agent starts.
- As built (step 10.5, gd-GCzHo1CDNF): `internal/lands/freeze` is the one write path, shared by
  `gb lands freeze|thaw` and the Freeze button. It writes `land_frozen` first and on its own,
  then `land_frozen_by`, `land_frozen_at` and `land_frozen_reason` (operator-only; a thaw
  clears them), and leaves a comment on the project bead. Only tenant lands can be frozen, and
  a freeze needs a reason. The spawn wall refuses with `land_frozen`. The purse watcher parks
  every running agent through the usual nudge, 5-minute grace and suspend, with
  `park_reason=land_frozen`, whatever the purse says. A thaw inside the grace calls the suspend
  off; an agent already suspended stays parked. The steward's gardener is paused by the same
  wall in `gardenerPreFireCheck`. Commons writes are refused twice: in kbeads, a scoped
  principal (member, owner, guest or land principal) creates nothing in a frozen land, and a
  failed project lookup also refuses; in beads-viewer, which reads the frozen set every 15 s
  and right after a button press, members get a banner in place of the composer and are
  refused say, reply, join and leave. The operator still posts. One exception, for Terms §9.1
  (gd-mJw9pxK2wu): a person's plain message in the land's own `#support` is taken at both,
  joined or not, and the composer stays there under its own banner; the land principal and
  every other write are still refused. The land header (8.8) does not
  exist yet, so Freeze and Thaw sit on a tenant land's channel header in the Commons. Freeze is
  a disclosure: a reason, then a red confirm. Both are goldens (`commonsfreeze`,
  `commonsfrozen`), graded C1-C12 at 100 %. `gb lands pause|resume` open and close the
  `lands-pause` gate; `wall.Paused` refuses with `lands_paused` while the gate is open and
  while it cannot be read. Operator lands never read it. Running agents keep running under a
  pause: freeze a land to park them.
- Who may throw them (PR #669 review, gd-QVP0Q6tzYB): kbeads lets only an OIDC
  `gardenio-admin` (or an approved escalation replay) set, change or clear `land_kind` and
  `land_frozen*` on a project bead, change a project bead's status or title, or close, rename
  or delete the `lands-pause` gate (`kbeads/internal/server/land_switch_authz.go`, HTTP and
  gRPC, the field endpoints and a close body included). `{update, project}` stays on the
  machine allow-list for the other project fields, so the static and fleet tokens are refused
  only these. The Freeze and Thaw POSTs also refuse a cross-origin browser request. A tenant
  land whose project bead is closed, deleted or renamed stays walled: before letting a name no
  live project carries through, the wall looks for a closed tenant project bead or a
  `lands-<land>` account and refuses with `land_gone`. The purse watcher parks frozen lands
  even when the purse or account reads fail.

### 5.6 Proving the fence

A negative matrix with two test lands A and B, run in CI where it can be and live at the
check-in where it cannot:

- every kbeads route and every viewer route class as A's member, against B's beads (404, never
  403, never data);
- the Commons SSE stream, the farm document, push alerts and mail: nothing of B's reaches A;
- A's GitHub token against B's repo; A's LiteLLM key spend never counted in B's purse;
- from a pod in `land-a`: B's pods, the kbeads admin routes, the Kubernetes API, the metadata IP,
  the release namespace's services other than the allowed ones.

As built (step 10.6, gd-USqiG2sdPU): the daemon half is `TestCrossLandMatrix`
(`kbeads/internal/server/cross_land_matrix_test.go`). It seeds two tenant lands, A (claimlayer)
and B (bravo), puts a marker in every one of B's beads, and calls every route `registerRoutes`
registers as A's member, owner, guest, land principal and agent, on the bare daemon and through
A's land door, with each path parameter filled from B. Every call must carry none of B's data
and change none of B's beads, labels, comments or dependencies, and a route that names B may not
answer 2xx. On such a route the answer for B's bead must equal the answer for a bead that does
not exist; that is the "404, never 403" rule, since a 403 beside a 404 tells A that B's bead
exists. The 39 routes that first differed (the shared read and write gates answered 403) were
drained by gd-7APTAIhdC5: every by-id gate now answers a bead outside the caller's projects as a
missing one (`beadHiddenFromCaller`), and a write gate does so only when it refuses the write, so a
write the gates allow is untouched. A read-scoped caller also gets 404 for a missing parent's
sub-resources, where an unscoped one keeps its empty 200. `crossLandExistenceOracles` is empty and
stays the ratchet for any new route. A positive control reads B's marker back as B's own member. The walk found one write hole, now
closed: requireRole admits every non-OIDC token on `/v1/agents/{id}/gates`, so an agent on one
tenant land could satisfy or reset another land's agent's stop gate. A tenant agent now reaches
only its own gates, and any other id answers 404 (`gateTargetAllowed`, `http_gates.go`).

The live half is `gardenio/scripts/lands-fence-matrix.sh`, which the 10.10 check-in runs against
two test lands that each have a running tenant pod. From A's pod it checks that B's pods, the
Kubernetes API, the metadata IP and every release-namespace service that A's env does not
address all stay silent. It checks that A's own daemon token is refused on the admin routes and
gets a 404 for B's land. Over the daemon's gRPC port, which the land fence also opens, it checks
that A's token lists A's own land but is refused config and lists nothing of B's (D3, added by
10.10). It checks that A's Lands App token gets a 404 for B's repository, that
A and B have different LiteLLM keys, and that a one-token call on A's key moves A's spend and
not B's. On Keycloak it checks that `default-roles-<realm>` has no `gardenio-*` composite and
that a fresh tenant user holds no viewer, operator or admin role. That is the 10-25 drift,
gd-llFKMS8Pkc: the user is made, read and deleted. It prints PASS, FAIL or SKIP per check, and
`lands-fence-matrix_test.sh` proves its verdicts against a fake fenced cluster and a fake leaky
one in the `script-selftests` job. The viewer half (viewer routes, the farm document, the
Commons SSE fan-out, push alerts and mail) is gd-cIux5tj6WI.

### 5.7 Security review

A threat model, `gardenio/docs/security/lands-threat-model.md`, in the shape of
`gardenio-in-a-box-threat-model-signoff.md`: assets, tenants as attackers (of each other, of the
fleet, of the operator's Slack and GitHub), stranger repos as attackers (prompt injection
through repo content, supply chain through `npm install`), and the operator as a single point of
failure. A review crew runs `/security-review` over the Track B diff and the matrix results, and
findings become bugs under Phase 10. The owner signs off with a decision. The public security
notes (gd-mu3cEUKoAV) publish what tenants should know.

### 5.8 Uptime and SLA (gd-rqtbyqCMZJ, scope refined; one new step)

What exists: about 45 vmalert alerts and 21 VMRule files, Alertmanager routing to Slack; no SLO,
no burn-rate rule, no blackbox probe and no status page of our own. The edge-probes terraform
module (`gardenio/terraform/modules/edge-probes/`) probes from AWS and mails via SNS.

The minted step keeps the measurable part:

- SLIs and SLOs: availability of `/garden`, `/lands/` and the Commons API (edge probe success),
  Commons event latency, spawn-to-working latency for tenant lands, steward response time to a
  human message in `#general`.
- Multi-window burn-rate VMRules for each SLO, routed like existing pages.
- External probes for `/lands/` and `/garden` from the edge-probes module.
- A status page hosted **outside the cluster** (a static page the edge-probes module updates),
  so it is up when gardenio is down. Terraform is operator work: an escalation with the exact
  module change.

The new sibling step 10.9 carries the words: the SLA document (targets, exclusions, the remedy
paid in credits through a `credit_grant`), the incident process and the
status-page update runbook. The targets are an owner decision filed by that step.

## 6. Phase 11, Open day (gd-xHvLXLLdXf)

### 6.1 Support (gd-UBeJ3kVicQ, scope refined)

The minted step says "a support channel in the operator's land". A tenant cannot see the
operator's land; that is the isolation wall. So support lives **in each tenant land**:

- `#support` is seeded in every tenant land (§3.3 step 4). Its members are the land's members plus
  the operator (an admin sees every land). A message there creates a `support` labelled bead in
  the land's project and notifies the operator the way decisions do (Slack DM first today, the
  Commons after Commons step 4.11).
- An email address for people who cannot sign in: mail to `support@` forwards to the operator's
  mailbox (an SES receipt rule, part of the 7.2 escalation).
- A runbook, `gardenio/docs/lands/support-runbook.md`: triage, how a tenant issue becomes a bead
  (in the tenant's land, or a gardenio bug when it is ours), when to freeze, how to refund
  credits, response targets taken from the SLA (step 10.9).

### 6.2 The day itself

1. A full **dress rehearsal** with a test GitHub account and a test repo, from the landing page
   to a merged PR to a drained purse to `gb lands close`.
2. The owner picks the first tenant, repo and grant (a decision).
3. The tenant registers, applies and is approved; their teammates join by invite (§3.7). The
   land has Slack mirroring off from birth (B-D11), and the rehearsal's check that nothing reaches the
   fleet Slack is repeated live.
4. One week of use. The plan-gardener checks the land daily: purse, steward runs, support
   beads, alerts, C1–C12 grades of what the tenant saw.
5. A **retrospective** doc in the bundle: spend against credits, what the steward did well and
   badly, support load, incidents, what the tenant asked for that Lands cannot do.
6. A **payments decision** for the owner, with the retrospective's numbers: stay on operator-set
   credits, add Stripe, or invoice outside the product.

## 7. Steps

The plan-gardener mints these as child tasks of the phase epics. Each step is one PR sized for
one crew agent unless it says otherwise. **Blocked by** names the edges to add
(`kd dep add <step> <blocker> --type blocks`); steps with none can start as soon as they are
minted. **⚑ operator** marks a step whose PR is code or chart only and whose rollout needs an
escalation the crew files with the exact change (secrets, deploy-values, RBAC, terraform,
third-party accounts); the crew does not wait on it to open the PR. UI steps ship goldens under
the looking glass (gd-D953GccBTY) and a C1–C12 grade, as in CS §7.

Three already-minted steps are refined here rather than re-minted: 7.1 is gd-fyboiKszlg, 10.8
is gd-rqtbyqCMZJ and 11.1 is gd-UBeJ3kVicQ. The gardener should update their descriptions to
the scope below.

### 7.1 Phase 7, Gate (gd-pqJennUltH)

The phase epic's existing edges stay: S9c gd-xNMLrsgajy, realm groups gd-myIsTYHWX7,
`email_verified` gd-rnklckVFKS, the live scope check gd-llFKMS8Pkc.

| Step | What | Blocked by |
|---|---|---|
| **7.1** (gd-fyboiKszlg, refined) | Terms, privacy policy and AUP drafts under `gardenio/docs/lands/legal/` with versions (§2.6); one owner decision for entity, jurisdiction, liability cap, retention after close and private repos (§8 OQ2). Docs only. | — |
| **7.2** | Mailer: `internal/mailer` interface, SES implementation, recorder fake, the templates in §2.7; config off by default. ⚑ operator: SES sender domain, IRSA role limited to `ses:SendEmail` on that identity, `support@` receipt rule forwarding to the operator. | — |
| **7.3** | GitHub IdP: `github` entry in `keycloak.identityProviders` with `trustEmail` only there, IdP mappers to `github_id`/`github_login` user attributes, a protocol mapper putting both in the access token; kbeads and the viewer read the two claims into the auth identity. Tests: the chart renders the IdP; a token without `email_verified=true` is refused (relies on gd-rnklckVFKS). ⚑ operator: GitHub OAuth app, client secret, deploy-values enabling the IdP. | gd-rnklckVFKS, gd-myIsTYHWX7 |
| **7.4** | Intake model in kbeads: `type=plot_application` with the §2.4 fields and state enum; the `lands-intake` project; `POST /v1/users/me/terms` writing only `terms_version`/`terms_accepted_at` on the caller's own user bead (not identity fields, so `user-identity-fields.md` holds). Tests in kbeads for the type, the field gate and the self-only rule. | — |
| **7.5** | Front of house, read side: `/lands/`, `/lands/terms|privacy|aup`, `POST /lands/accept`; the `routeApplicant` route class with `TestEveryViewerRouteHasClass` updated; a test that an applicant (no role, no group) reaches exactly these routes and nothing else in the viewer, and gets 403 from every kbeads bead route. Goldens at phone and desktop. | 7.1, 7.4 |
| **7.6** | Lands GitHub App: `internal/githubapp` holds two App identities (fleet and lands) chosen by name; `GET /lands/github/setup` verifies installation id, authorizing user = the applicant's `github_id`, admin on the repo, repo visibility; App manifest and permission list documented (contents, pull requests, issues write; checks read; metadata). ⚑ operator: create the App on github.com, private key secret, setup URL. | 7.5 |
| **7.7** | Apply: the form, `POST /lands/apply` with the §2.4 limits, the approval decision (§2.5), the reactor that applies resolved decisions to the application, `/lands/applications` and withdraw, mail on every state change. Tests: approve/deny/ask from a resolved decision; one open application per applicant; an applicant never sees another's application. | 7.2, 7.4, 7.6 |
| **7.8** | Edge: static group `/lands-tenants` in the chart; `gardenio.clonk.tv` admits it; a `^~ /lands/` location admitting any signed-in user and `/lands/` itself without a session; `limit_req` on `auth.clonk.tv` and `/lands/`; `scripts/check-viewer-edge-groups.py` taught the new group. ⚑ operator: rollout. Written against the edge as it stands after S9c. | gd-xNMLrsgajy |
| **7.9** | Gate check-in: a test GitHub account registers, accepts the terms, installs the App, applies; the operator approves, denies and asks from a phone; mails arrive; screenshots of every page at both widths in #gardenio-homestead; the applicant's 403s recorded; owner go/no-go decision. | 7.3, 7.5, 7.7, 7.8, gd-llFKMS8Pkc |

### 7.2 Phase 8, Deed and purse (gd-KiiqMwbZLu)

Existing edges stay: per-viewer `/garden` gd-MnQlEmbUq3, per-project budget gd-SAPwFE4FyW, realm
groups gd-myIsTYHWX7.

| Step | What | Blocked by |
|---|---|---|
| **8.1** | Land model: the §3.2 fields on `type:project`; `type=credit_grant` (amount in credits, reason, granted_by, decision id, rate version, land label); the versioned rate table (`type=credit_rate`, `gb lands rates show|set|reapply`, shipped unset, grants fail closed); `gb lands credit add|list` (admin only) that writes the grant and sets the key's `max_budget` to granted ÷ multiplier atomically, rolling back on a LiteLLM error. | 7.4 |
| **8.2** | Purse: `internal/lands/purse` (§3.4 arithmetic, margin, unknown-after-15-min); `GET /api/lands/{land}/purse`; `gb lands purse`; the gardenhub cost entry reads it for tenant lands. Tests: each term, the margin, unknown → wall, operator lands unchanged. | 8.1, gd-T6zwDjWLBI |
| **8.3** | Spawn wall: refusal in the controller's `AgentSpawn` admission and the reconciler twin for frozen, `tenantSpawns` off, wall or unknown; `agent_state=budget_exceeded` plus a comment; `gb spawn` prints the reason; `gardenerPreFireCheck` refuses the same cases. Tests for each refusal and for operator lands being untouched. | 8.2 |
| **8.4** | Purse watcher: the 1-minute loop that nudges, waits the grace and suspends a land's agents at the wall with reason `purse_empty`; the threshold notices in `#general` and mail at 80/100%; a credit grant posts "purse refilled" and wakes the steward hook (no-op until 9.2). | 8.3, 7.2 |
| **8.5** | Keycloak provisioner client: `internal/keycloakadmin` with create-group (prefix `project-` enforced, role `gardenio-project-member` only), add/remove member, find user by `github_id`; idempotent; tests against a fake Keycloak. A chart-upgrade test (or documented manual check) that the EDP operator leaves runtime groups alone. ⚑ operator: confidential client `gardenio-lands-provisioner` with fine-grained admin permissions limited to group creation and membership, and its secret. | gd-myIsTYHWX7 |
| **8.6** | Provisioner: `internal/lands/provision` reconciling `approved` applications through §3.3 steps 1–4 and 7, `provision_step` resume, failure escalation, `gb lands provision --retry`, `gb lands close` (reverse order, minus the namespace until 10.2). Tests with fakes for every step and for a crash between each pair of steps. | 7.7, 8.1, 8.5, gd-FFpayTSTK0 (Commons 2.3 seeding), gd-9aCcG1zCCY (Commons 4.2 owner role) |
| **8.7** | Tenant GitHub tokens: `github_app_credentials.go` mints with the App the project names (`github_app=lands`) and the pinned `github_installation_id`; tenant lands get the one repo and never same-owner reference repos. Tests: token scope per App; a tenant project can never select the fleet App. | 7.6, 8.6 |
| **8.8** | Tenant lands on their plot: redirect from `/` and `/lands/` to `/garden?land=`; `?land=` camera and Commons focus; land header with the purse; goldens at both widths. | 8.2, gd-MnQlEmbUq3, gd-b4IDsCKWbB (2.9 pane) |
| **8.9** | Member invites: invite lightbox on the land header, `land_invite` bead, the provisioner's join-on-sign-in by `github_id`, revoke; mail to the invitee when 7.2 is live. Tests: wrong `github_id` never joins; expiry; revoke takes effect on next token. | 8.5, 8.6, gd-9aCcG1zCCY |
| **8.10** | Deed check-in: provision a test land end to end from a real application; the operator spawns into it with the admin override and drains a $1 purse to the wall; refill; close. Screenshots; owner go/no-go. | 8.1–8.9 |

### 7.3 Phase 9, Steward (gd-G8lmByYEoM)

Existing edges stay: gd-DKMSc1Y91I, gd-e4uXZs8coZ, gd-TvJosTKfAt, gd-T6zwDjWLBI, gd-Pcyz0ftcTg.
**Add the edge 0d gap 2 asked for: gd-G8lmByYEoM → Commons I gd-KoOR5SPlNE** (it is still
missing on 2026-10-19).

| Step | What | Blocked by |
|---|---|---|
| **9.1** | Roles: `steward` and `lands-gardener` role beads with `capability_tiers: [lands-tenant]`; the steward playbook prompt (§4.2) as a template beside the plan-gardener's. Content only; the tier's meaning is 10.1. | gd-Bcqj2r4m7E |
| **9.2** | Land-bound gardener: `internal/steward` on the plan-gardener builder; `gardener-land:<land>` binding; the §4.3 wake events in trigger-engine; daily cron; runs-per-day cap. Tests: each wake event fires once; a message in another land never wakes it. | 9.1, gd-RFynVHDLXW (2.7 `gb say`) |
| **9.3** | Provisioner hook and greeting: §3.3 step 6; the first run posts the greeting in `#general`. | 9.2, 8.6 |
| **9.4** | Conversation to plan: the steward writes a plan in the land's project and files an owner decision; on approval it spawns step crews. A `gb` helper for "plan from this thread" if the playbook needs one. | 9.3, gd-jAcBpiVyPN (3.3 decisions in the Commons) |
| **9.5** | Spawn within the purse: step cost estimate (mean of the land's last 10 settled gardener sessions, default before that); ask-instead-of-spawn; `max_live_agents`; the spawn gate allows the steward to spawn only `lands-gardener` in its own project. Tests for each refusal. | 9.4, 8.3, gd-Pcyz0ftcTg |
| **9.6** | Daily digest in `#general`; quiet days post nothing. | 9.3 |
| **9.7** | Parking: the steward's empty-purse post, reason `purse_empty`, sleep until a credit-grant wake; resume on refill by the steward's choice. | 8.4, 9.2, gd-TvJosTKfAt |
| **9.8** | The steward on the map: sprite at the land's homestead, reason on hover and tap; goldens. | gd-DKMSc1Y91I, gd-e4uXZs8coZ, 9.3 |
| **9.9** | Steward check-in on the 8.10 test land (operator acting as tenant): greet, ask, plan, approve, gardener PR on the test repo, digest, drain, park, refill, resume. Transcript excerpts and screenshots; owner go/no-go. | 9.1–9.8 |

### 7.4 Phase 10, Fences (gd-Gx6arHBvzJ)

Existing edges stay (the nine from 0d §3).

| Step | What | Blocked by |
|---|---|---|
| **10.1** | `lands-tenant` tier: the §5.1 table as tier data and checks on the shared enforcement point; untrusted-input taint on every bead of a tenant land; escalations from tenant agents refused with a structured denial pointing at `#support`, **except** a merge escalation (`github_pr_merge`, `gitlab_mr_merge`) about the agent's own land, which the escalation policy routes to the steward or land owner (escalation-routing plan Task 11). | gd-Bcqj2r4m7E, gd-Pcyz0ftcTg, gd-kLkxfyARXz, gd-hGfgjuviQy, gd-vvdOkF0bDL, 9.1 |
| **10.2** | Land namespace: the §5.2 template applied by the provisioner (and removed by `gb lands close`); tenant pods placed in `namespace` with `runtime_class=runsc`. Tests render the template and the tenant pod spec. ⚑ operator: controller RBAC (namespaces, quotas, limit ranges, network policies, service accounts, `bind` on `gardenio-land-agent-manager` only), gVisor on the k3s nodes, a NetworkPolicy enforcement check. | 8.6 |
| **10.3** | Tenant pod allowlist (§5.3): a test that fails on any env var, volume or secret outside the list; remove whatever it finds. | 10.2, 8.7, gd-ke6PC7bRto |
| **10.4** | Abuse controls (§5.4): per-land spawn rate in the controller via `spawnguard`, Commons message rate per member, repo size check at apply, the four alerts as VMRules. | 8.3 |
| **10.5** | Kill switches (§5.5): `gb lands freeze|thaw`, the Freeze button, the fail-closed `lands-pause` gate. Tests: freeze parks everything and refuses writes; an unreadable gate refuses tenant spawns. | 8.4 |
| **10.6** | Cross-land negative matrix (§5.6), in CI where possible; the live part is a script the 10.10 check-in runs. | 10.2, 10.3, 8.6, gd-9aCcG1zCCY |
| **10.7** | Security review (§5.7): threat model doc, review run, findings filed as bugs under Phase 10, owner sign-off decision. Publishing is gd-mu3cEUKoAV. | 10.1–10.6 |
| **10.8** (gd-rqtbyqCMZJ, refined) | SLIs, SLOs, burn-rate VMRules, external probes for `/lands/` and `/garden`, the off-cluster status page. ⚑ operator: the edge-probes terraform change. | 7.8 |
| **10.9** | SLA document, incident process and status-page runbook under `gardenio/docs/lands/`; one owner decision for the targets and the remedy. | 10.8, 7.1 |
| **10.10** | Fences check-in: a second test land; the live matrix; review findings closed or accepted by the owner; owner go/no-go. `lands.tenantSpawns` stays off; turning it on moved to 11.7 (gd-3Vx9UFk7Ad). As run: [threat model §10.10](../../security/lands-threat-model.md#1010-the-fences-check-in). | 10.2–10.9 |

### 7.5 Phase 11, Open day (gd-xHvLXLLdXf)

| Step | What | Blocked by |
|---|---|---|
| **11.1** (gd-UBeJ3kVicQ, refined) | Support (§6.1): `#support` seeded per tenant land, the support bead and operator notice, the `support@` forward, the support runbook with response targets from 10.9. | 8.6, 7.2, 10.9 |
| **11.2** | Dress rehearsal (§6.2 item 1) on a fresh test GitHub account, recorded step by step with screenshots; every break filed as a bug blocking 11.4. | 7.9, 8.10, 9.9, 10.10, 11.1 |
| **11.3** | First tenant: the gardener files the owner decision (who, which repo, initial grant) and, on the answer, the tenant gets the `/lands/` link. No code. | 11.2 |
| **11.4** | Open day: the tenant's land for one week; `lands.tenantSpawns` on for that land; daily plan-gardener checks; the live no-Slack-leak check; incidents logged. | 11.3, Commons III gd-lDmTW3GkPq |
| **11.5** | Retrospective doc in bundle gd-uj7BxrMQsH (§6.2 item 5). | 11.4 |
| **11.6** | Payments decision for the owner with the retrospective's numbers. | 11.5 |

Open day waits on Commons III because the tenant has no Slack: land roles (Commons 4.2), scoped writes
(4.1), notifications (4.7) and the first multi-human land (4.9) are what make the Commons usable
by people who are not the operator. It does not wait on the Workbench or the Glass: the bead
panel already in `/garden` and the Commons cards carry what a tenant needs, and the Glass is
operator-only (Q8d).

### 7.6 Parallelism

Track B's first steps have no Track B blockers: 7.1, 7.2 and 7.4 can start now, and 8.5 waits only on the realm-groups rollout. 8.1
follows 7.4. The long poles are outside Track B: P9's tier work (gates 9.1 and 10.1), the
per-viewer `/garden` (gates 8.8), Commons I (gates Phase 9) and Commons III (gates Open day).

## 8. Owner questions

Genuine forks only: each option leads to a different build. The plan-gardener files each as a
decision with `--requesting-task` set to the step it gates. The recommended option comes first.

| # | Question | Options | What changes | Gates |
|---|---|---|---|---|
| OQ1 | What is a credit? | $1 of LLM spend at the LiteLLM price · **a marked-up unit that also covers compute and the operator's time** (chosen, gd-7sTt20714N) | Resolved: markup plus a rate table. 1 credit = US$1 billed; tenant LLM spend × 1.5, one meter, no compute charge (gd-XQmEoHAqmp, `single`); key `max_budget` = granted ÷ 1.5, rounded down. The rate is a `credit_rate` version the operator sets after deploy; `gb lands credit list` gives the at-cost dollar figure as the operator's second figure (§3.4). | 8.1 |
| OQ2 | Private repos in v1? | **allowed** · public only | With private repos, the privacy policy covers confidential code in transcripts the operator can see (Q8d), the App needs no extra permission, and the review (10.7) treats leaks of repo content as a top threat. Public-only drops that and drops the paying tenants who want it. Filed inside 7.1's owner decision, not separately. | 7.1, 7.6 |
| OQ3 | Which models do tenant agents run? | **a cheaper default for tenant lands (Sonnet 5.5), Opus per land at the operator's choice at approval** · the fleet default (Opus 5.5) everywhere | Credits buy several times more work on Sonnet, so the first tenant's purse lasts. The steward's role bead and the approval free text (`model=`) carry the choice. | 9.1, 7.7 |
| OQ4 | If gVisor cannot run on the k3s nodes, does Open day wait? | **wait** (tenant pods only under `runsc`) · open with `runc` plus namespace, NetworkPolicy and quota | Asked only if the 10.2 escalation finds gVisor infeasible. A stranger's `npm install` sharing the node kernel with the fleet is the risk being accepted. Answered: runc (gd-iyZom5u11A), with tenant pods kept off fleet nodes (gd-NDJIOutLsv, §5.2 As built). A dedicated tenant node pool stays open. | 10.2, 10.10 |

Not new questions, but owner calls that steps already file: entity, jurisdiction, liability
and retention (7.1); SLA targets and remedy (10.9); the first tenant (11.3); payments after
Open day (11.6); go/no-go at each check-in.

## 9. Risks

- **A stranger's code in our cluster.** Gardeners run whatever a tenant's repo makes them run.
  Mitigation: gVisor, namespace, default-deny egress to anything internal, an allowlisted pod
  spec (10.3), no tenant spawns before the Fences check-in (`lands.tenantSpawns` off), freeze in
  one command.
- **Prompt injection through repo content and messages.** Mitigation: the untrusted-input taint
  on every tenant bead, the `lands-tenant` tier, which has nothing worth stealing (no external send, no
  escalation, one repo), and the threat model naming it first.
- **Spend runs past the wall.** Accounting lags up to 5 minutes, and a long tool call can spend
  after the check. Mitigation: the margin, the live estimate, unknown counting as the wall, and
  LiteLLM's `max_budget` on the land's key as a backstop that does not depend on gardenio.
- **Runtime Keycloak writes.** A bug in the provisioner could put a user in the wrong group.
  Mitigation: the client can only create `project-*` groups and manage membership; the
  provisioner never assigns roles; the matrix (10.6) runs against every provisioned land; an
  audit line per membership change.
- **The EDP operator prunes runtime groups.** If a chart upgrade deleted groups it did not
  render, every tenant would lose access. Mitigation: 8.5 checks it before any land exists.
- **One operator is the support desk, the approver and the incident responder.** Mitigation:
  manual approval (Q3) throttles growth, the SLA targets are set by the owner knowing that, the
  status page and support runbook make an outage visible instead of silent.
- **Product obligations before product revenue.** Terms, SLA and support are promises made with
  credits granted by hand. Mitigation: 11.6 revisits payments with real numbers before a
  second tenant.
- **Long poles outside Track B** (P9 tiers, per-viewer `/garden`, Commons I and III). Mitigation:
  §7.6 starts every Track B step that does not wait on them now, and the check-ins expose any
  slip.
