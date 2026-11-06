# Onboarding a New Project + Role to an Existing Gardenio

**Status:** Runbook (authored from a verified friction audit, epic ka-0000003121). This is the doc the audit proved did not exist. Every step below is grounded in live code (file:line cited) and a real worked example (the `pooltest` project bead ka-0000001683).

**Goal:** stand up a brand-new project + role in an *already-running* gardenio, point an agent at a fresh repo, and have it open a PR. This is a different concern from standing up gardenio itself or setting up an operator laptop — see [See also](#see-also) for those.

> **Glossary — read this first (the #1 confusion point).**
> - **project** = a `type:project` *config bead* describing how agents run for a body of work (git repo, image, account, secrets). This is what you create. There is **no `gb project` command** — you author the bead with `kd create --type=project`.
> - **role** = a `type:role` *config bead* (pod overrides: model, image, resources) **+ optionally** a `config:claude-instructions:role:<name>` bead (behavioral instructions). Authored with `kd create --type=role`. No controller release needed.
> - **fleet** = a k8s *namespace + RBAC + token* (`gb fleet create`). This is **NOT** a project and is **not** part of this runbook. Most projects live in the existing `gardenio` namespace and do not need a fleet. Do not run `gb fleet create` thinking it onboards a project.
> - **account** = a `type:account` bead naming the LLM credential a project's agents use. There is **no `gb account create`** — author the bead by hand.
>
> A second axis of confusion: **project ≠ fleet ≠ AWS account.** A *project* is a body of work (a bead). A *fleet* is a k8s namespace boundary. An *AWS account* (e.g. `234567890123` prod, `123456789012` POC) is where the cluster and Secrets Manager live. One AWS account hosts many fleets; one fleet hosts many projects.

---

## Prerequisites

- You (the operator) are a `gardenio-admin` in OIDC, **or** you are acting through an agent with a static daemon token. `type:project`/`type:role`/`type:account` are `write_policy.min_role: gardenio-admin` (kbeads `internal/server/config.go:451`), but that gate is **only enforced for OIDC/UI callers** — static-token agent writes bypass it (`internal/server/write_authz.go:48-57`). So agents can author these freely; a human in the UI needs admin.
- The target repo exists on **gitlab.com** (a private repo on any other GitLab host currently cannot complete its first init-clone — `controller/internal/podmanager/initclone.go:109,112` hardcodes `gitlab.com`; this is a known major bug tracked in ka-0000000552).
- A bot GitLab account that will be a **Developer or Maintainer** on the repo (Developer can push `agent/*` branches and open MRs; Maintainer is needed if branch protection requires it).

> **Heads-up — the only project example previously in this repo was wrong.** Older research notes (`docs/research/coverage-per-project-w4-onboarding.md`) show `kd create "Project: <Name>" --type=project --slug=<name>`. **There is no `--slug` flag** — `kd create` accepts only `-f/--field key=value` typed fields (plus `--type`, `--role`, `--kind`, `--format`). Running the old example errors out with `unknown flag: --slug`. The project's slug/name is simply its **title** (stripped of any `Project: ` prefix). Use the `-f` field form shown below.

---

## Step 1 — Create the project config bead

The **minimal PR-capable field set** (anything less and agents spawn into an empty/broken workspace, silently):

```sh
kd create '<project-name>' --type=project \
  -f prefix=kd \
  -f git_url=https://gitlab.com/<group>/<repo>.git \
  -f default_branch=main \
  -f account=<account-name> \
  -f repos='[{"url":"https://gitlab.com/<group>/<repo>.git","role":"primary","branch":"main"}]' \
  -f release_tag_pattern='^v\d+\.\d+\.\d+$' \
  -f release_targets='[{"name":"prod","kind":"k8s"}]' \
  -f release_branch=main
```

The last three flags are optional: they switch on the project's releases (see
[Releases](#releases) below). Leave them out and the project has none.

Field notes (from `controller/internal/beadsapi/projects.go` parser + the config schema):
- **`git_url`** — REQUIRED in practice. Empty ⇒ init-clone returns early (`initclone.go`) ⇒ empty workspace ⇒ no PRs, **no error**. The daemon will *accept* a project bead with no git_url — do not rely on it to stop you.
- **`prefix`** — the bead-ID prefix for issues in this project (e.g. `kd`).
- **`default_branch`** — defaults to `main` if unset; set it explicitly if your repo's default is not `main` (otherwise init-clone clones a nonexistent branch).
- **`account`** — must match a `type:account` bead title (Step 2). An unresolved account ⇒ pods spawn but produce only "synthetic turns" (look alive, do nothing).
- **`repos`** — JSON array of `{url, role, branch}`. `role:"primary"` is the repo cloned to `<workspace>/<project>/work`; `role:"reference"` repos are read-only clones (`projects.go:27-33`). **Shape matters**: a string array or wrong-keyed object is *accepted then silently dropped* by the controller (`projects.go` `unmarshalProjectField`). Validate with the viewer's project hub badge after creating.

> **Gotcha — the project's label is NOT its name.** `kd create --type=project` auto-applies a `project:<your-current-project>` label, not `project:<new-name>` (papercut, confirmed). The project's *name* is its **title**, stripped of any `Project: ` prefix (`projects.go:325`). Don't be surprised that a new project bead carries `project:gardenio`.

> **Gotcha — don't double-create.** Two project beads with the same name are both accepted; on read, the lexicographically-lowest bead ID wins and the other's fields are *silently dropped* (`projects.go:317`, "a sparse duplicate could blank git_url/account"). If a create fails halfway, `kd close` the stray bead before retrying.

> **The empty-project contract.** A pod whose env carries neither `BOAT_PROJECT`
> nor `KD_PROJECT` subscribes to `global` + `role:<r>` layers only — it never
> subscribes to `project:<name>`, so it receives **no** project-scoped config
> bead, not this project's and not any other's. This is deliberate, not a bug
> (a pod without a project has no project config to apply). It is now also
> **loud**: `gb setup claude` emits `[setup] warning: no project in env …` at
> startup instead of silently degrading.

---

## Step 2 — Create the LLM account bead

There is no `gb account create`. Author it by hand (shape from the live account beads + the `projects.go` account parser):

```sh
kd create '<account-name>' --type=account -l project:<project-name> \
  -f provider=anthropic \
  -f credential=secret:<project-name>-claude-oauth \
  -f model=claude-opus-4-6-oauth
```

- `credential` schemes: `secret:<k8s-secret-name>` (ESO/k8s Secret), `coopmux:<name>` (coop credential manager), or `env:<VAR>`.
- **Bedrock caveat (advice ka-0000005094):** a Bedrock-routed account must NOT point `credential` at an `sk-ant-` OAuth token — LiteLLM forwards it as a malformed SigV4 and Bedrock 403s. Use the matching credential scheme for your provider.

---

## Step 3 — Wire git/GitLab credentials (the part that silently fails)

Agents clone, push, and run `glab` using credentials injected from a **K8s Secret referenced by the project's `secrets[]` array**. The controller infers what each secret is for **purely from the env name** (`controller/cmd/controller/podspec.go:885-888`) — there is no explicit "this is the git token" field:

| Env name (must match exactly) | What it wires |
|---|---|
| `GIT_USERNAME` + `GIT_TOKEN` | clone + push credential helper (`~/.git-credentials`) |
| `GITLAB_TOKEN` | init-clone private clone (`/etc/git-creds/gitlab/token`) **and** `glab` auth + push |
| `GLAB_TOKEN` | `glab` CLI (some paths; same secret/key as `GITLAB_TOKEN`) |

### 3a. Create the K8s Secret value in AWS Secrets Manager

The project `secrets[]` entry becomes an ExternalSecret whose remoteRef is `gardenio/<k8s-secret-name>`. **You must pre-create the AWS SM entry by hand** — nothing in gardenio writes the secret *value*:

```sh
aws secretsmanager create-secret \
  --name 'gardenio/<project-name>-gitlab-token' \
  --secret-string '{"token":"<bot-PUSH-capable-PAT>"}'
```

- The JSON **property name** (`token` here) must match the `key` you put in the `secrets[]` entry below.
- The PAT scope **must allow push + MR creation** (`write_repository` + `api`). A `read_repository`-only token clones but **silently cannot push or open MRs** — the provable goal fails with no obvious cause (major finding).

### 3a-bis. Grant external-secrets-operator READ on the new SM prefix (REQUIRED — easy to miss)

Creating the SM secret *value* is not enough: external-secrets-operator reads it
through an IRSA policy whose resource list is an **explicit per-project prefix
allow-list**, NOT an open `gardenio/*` wildcard. A brand-new `gardenio/<project>-*`
prefix is **denied by default**, so ESO gets `AccessDenied`, the K8s Secret never
materializes, and every agent spawn defers forever at `verifySecretsExist` with no
operator-visible signal (this is exactly what bit glm-lab — bug ka-0000006981).

Add the prefix to `local.p44_oauth_bootstrap_secret_arns` in
`gardenio/terraform/composition/canonical-prod/platform.tf`:

```hcl
"arn:${data.aws_partition.current.partition}:secretsmanager:${var.region}:${data.aws_caller_identity.current.account_id}:secret:gardenio/<project-name>-*",
```

then apply it (targeted, prod account 123456789012):

```sh
cd gardenio/terraform/composition/canonical-prod
terraform apply -target=module.platform -var-file=prod-real.tfvars
```

Verify ESO can now read it (ESO's IRSA role, not your agent role):
`aws secretsmanager list-secrets --region us-east-1 --filters Key=name,Values=gardenio/<project-name>` should list all the secrets you created in 3a.

> This is the **first** of two per-project cluster-side grants that a new project is not on
> by default and that fail silently when missed. The second — **namespace RBAC** — is
> [Step 3e](#step-3e--if-the-projects-agents-run-workloads-in-their-own-namespace-namespace-rbac).
> Both were the source of real silent-failure incidents; add them together.

### 3b. Reference the secret on the project bead

The K8s Secret name **MUST be prefixed `<project-name>-`** or the entry is *silently dropped* (podspec secret-prefix guard; only a controller log warns). Append to the project's `secrets` array (read-modify-write the whole array — there is no `gb secret add`):

```sh
kd update <project-bead-id> -f secrets='[
  {"env":"GIT_USERNAME","secret":"<project-name>-git-credentials","key":"username"},
  {"env":"GIT_TOKEN","secret":"<project-name>-git-credentials","key":"token"},
  {"env":"GITLAB_TOKEN","secret":"<project-name>-gitlab-token","key":"token"},
  {"env":"GLAB_TOKEN","secret":"<project-name>-gitlab-token","key":"token"}
]'
```

The `pooltest` project (ka-0000001683) is a complete real example of exactly this shape:

```json
[
  {"env":"GIT_USERNAME","key":"username","secret":"pooltest-git-credentials"},
  {"env":"GIT_TOKEN","key":"token","secret":"pooltest-git-credentials"},
  {"env":"GITLAB_TOKEN","key":"token","secret":"pooltest-gitlab-token"},
  {"env":"GLAB_TOKEN","key":"token","secret":"pooltest-gitlab-token"}
]
```

### 3c. Wait for ESO to sync

ExternalSecrets materialize on a refresh bucket (15m / 6h / 1h depending on tier). The **first spawn before sync silently defers** — the pod waits with no operator-visible signal. Give it a few minutes; verify with `kubectl get secret <project-name>-gitlab-token -n gardenio`. If it never appears, the #1 cause is a missing ESO IRSA read-grant (step 3a-bis) — check the external-secrets pod logs for `AccessDenied` on `gardenio/<project-name>-*`.

### 3d. Make the bot a repo member with push rights

On the GitLab repo: add the bot account as **Developer** (or Maintainer if protected branches require it), and ensure branch protection **allows the `agent/*` branch prefix** (advice ka-0000007064). gardenio does NOT create branch-protection rules for you — a brand-new repo either rejects the bot push or has no protection at all.

---

## Step 3e — (If the project's agents run workloads in their OWN namespace) Namespace RBAC

> **The two per-project cluster-side grants live here — read both boxes even if you skip the step.**
> Onboarding has **two** grants that are (a) per-project, (b) scoped by an explicit
> allow-list that a new project is *not* on by default, and (c) invisible until an agent
> silently fails. Historically each lived as a lone comment in a different repo, and each
> got missed on a real onboarding:
> 1. **ESO Secrets-Manager read-grant** → `platform.tf` (Step&nbsp;3a-bis). Missed for
>    glm-lab (ka-0000006981) and reverted for cs (v13 apply, escalation ka-0000002335) —
>    both left ExternalSecrets in `SecretSyncedError` with nothing paging.
> 2. **Namespace RBAC** → the gardenio Helm chart (this step). Hand-applied by `kubectl
>    apply` for both glm-lab (2026-07-11) and cs (2026-08-24), with **no record in any
>    repo** until epic ka-0000005677 / MR !7704 codified it. This step exists so the third
>    project does not repeat that.

**When you need it.** Most projects run agents in the shared `gardenio` namespace and need
**no** namespace RBAC — skip this step. You need it only when a project's agent pods must
operate on Kubernetes objects in a **different, project-owned namespace** — e.g. the cs
agent `helm install`s the crab-science chart into the `crab-science` namespace, which
requires create/get/delete on jobs, secrets, configmaps, serviceaccounts, and (for the
chart's own per-release RBAC) roles/rolebindings **in that namespace**.

**Who the grant is for.** A spawned agent pod runs as ServiceAccount
**`gardenio-system/gardenio-agent`** (the fleet default). It reaches *across* into the
project namespace through a RoleBinding whose subject is that SA. So the RoleBinding, not a
ClusterRole, is the boundary — and the grant **must be scoped to the project's own
namespace only**. A cs agent must not be able to create a Role in any other namespace, and
must hold nothing cluster-wide.

**How to add it.** The chart values key is `projectNamespaceRbac` (chart default:
`enabled: false`, empty `grants` — renders nothing). The **live prod posture** is the
`projectNamespaceRbac` block of
[`deploy-values/gardenio-k3s.yaml`](../../deploy-values/gardenio-k3s.yaml),
applied by the prod deploy path; template
[`gardenio/helm/gardenio/templates/project-namespace-rbac.yaml`](../helm/gardenio/templates/project-namespace-rbac.yaml).
Add one `grants` entry for the project:

```yaml
projectNamespaceRbac:
  enabled: true          # master gate — must be on for ANY grant to render
  grants:
    <grant-id>:          # map key = the default Role/RoleBinding object name
      enabled: true
      namespace: <project-namespace>   # REQUIRED; must already exist (this step does NOT create it)
      project: <project-name>          # stamped as label gardenio.io/project — the inventory sweep finds it by this
      rules:             # Role rules, verbatim — explicit resources + verbs, NEVER wildcard-on-wildcard
        - apiGroups: ["batch"]
          resources: ["jobs"]
          verbs: ["create", "get", "list", "watch", "update", "patch", "delete"]
        # …only the resources/verbs the project's workload actually needs…
      subjects:
        - kind: ServiceAccount
          name: gardenio-agent
          namespace: gardenio-system    # the fleet's spawned-agent SA — reaches across into <project-namespace>
```

Copy the **shape** of the live `crab-science-runner` grant in the overlay above; it is the
worked example. Keep rules least-privilege and explicit — a `resources: ["*"], verbs:
["*"]` grant is exactly the un-reviewed namespace-admin posture that got glm-lab revoked
(operator ruling, 2026-08-25).

> **Adoption vs. new grant.** If an object was already hand-applied to the cluster, it is
> live *without* Helm ownership metadata and a naive upgrade fails with `invalid ownership
> metadata`. Stamp `app.kubernetes.io/managed-by=Helm` and `meta.helm.sh/release-{name,namespace}`
> on it by hand before the upgrade. For a genuinely new grant there is nothing to adopt.

> **`kubectl auth can-i` is NOT sufficient proof.** RBAC escalation-prevention admission
> can reject a `create` on roles/rolebindings even when the verb is granted. If the grant
> includes `rbac.authorization.k8s.io` roles/rolebindings, verify by actually
> creating/getting/deleting an SA + Role + RoleBinding as the subject SA (how ka-0000000617
> was proven), not with `can-i`.

**Verify against the cluster** (this is the checkable step — a project that skipped it
returns nothing):

```sh
# The inventory sweep — every codified per-project grant carries this label:
kubectl get roles,rolebindings -A -l gardenio.io/project

# The project's objects exist AND are Helm-owned (not a stray hand-apply):
kubectl get role,rolebinding -n <project-namespace> -l gardenio.io/project \
  -o jsonpath='{range .items[*]}{.kind}{" "}{.metadata.name}{" managed-by="}{.metadata.labels.app\.kubernetes\.io/managed-by}{"\n"}{end}'
#   → each line must end `managed-by=Helm`. Blank output = grant not applied; `managed-by`
#     absent = still a hand-apply, re-run the deploy path so the chart adopts it.
```

Live posture at time of writing (2026-08-28): **cs** — `crab-science/crab-science-runner`
Role + RoleBinding, `managed-by=Helm`, codified & applied. **glm-lab** — deliberately
**empty**; its grant was revoked 2026-08-25 (operator ruling) and is parked `enabled:
false` in the overlay as a one-flag re-enable, not a live object.

---

## Step 4 — (Optional) Create a custom role

A new role needs **no controller release** (the role cache refreshes every reconcile tick; runtime config beads are merged by `gb prime`). A *complete* role is up to **three beads** — pod overrides, behavioral instructions, and the per-agent `type:agent` bead the spawn creates for you:

```sh
# (a) Pod overrides — model, image, resources, account, secrets
kd create '<role-name>' --type=role -l project:<project-name> \
  -f model=claude-opus-4-6-oauth

# (b) Behavioral instructions (OPTIONAL but usually wanted).
#     Without this, the role silently runs on the GLOBAL default instructions.
kd create 'config:claude-instructions:role:<role-name>' --type=config \
  -l claude-instructions -l role:<role-name> \
  -f value='{"...section overrides...":"..."}'
```

Caveats (all confirmed):
- Role **secrets** must be prefixed `<role-name>-` (same silent-drop rule as projects).
- The instructions bead can only override a **fixed set of section keys**; content outside them is dropped.
- **Per-role skill / sub-agent toggles (optional).** A role can restrict which Claude Code skills its agents get via the `enabled_skills` (allow-list) / `disabled_skills` (deny-list) fields — e.g. `gb role scaffold <name> --enabled-skills a,b --disabled-skills c` — and likewise which repo-provided sub-agents link via `enabled_agents` / `disabled_agents` (`--enabled-agents` / `--disabled-agents`). Empty = everything discovered on. See [`agent-skill-toggle.md`](agent-skill-toggle.md) for the full semantics (deny-wins, sibling-repo `{repo}-` namespacing, multi-role union).
- **Harness (optional; default `claude`).** The `harness` field selects the agent CLI a pod runs: `claude` (Claude Code) or `codex` (OpenAI Codex CLI). It can be set on the project, role, or agent bead and resolves **agent > role > project > controller default (`AGENT_DEFAULT_HARNESS`, `claude`)**; the first role with a value wins for a multi-role agent. kbeads rejects any value other than `claude`/`codex` at write time. For a one-off, `gb spawn --harness codex` stamps it on the agent bead. A `codex` pod gets `GARDENIO_HARNESS=codex`, mounts its state at `~/.codex` (instead of `~/.claude`), and gets `CODEX_MODEL` from the resolved model (account, then project/role/agent `model`); the Claude-only `CLAUDE_MODEL`, `ANTHROPIC_MODEL` and `ANTHROPIC_DEFAULT_*_MODEL` pins are dropped. The harness must match the account: `codex` needs a `provider=openai` account **pinned on the role** (`account=<openai-account>`; a project-level pin or the spread pool never reaches an openai account, and a codex spawn with no resolved account is refused) and `claude` refuses one, otherwise the spawn is refused up front and the agent bead is failed with the reason (no escalation is filed; fix the bead). Leave a role or project that mixes providers in its account pool alone for now, since account spread is not harness-aware. Example: `kd create '<role-name>' --type=role -l project:<project-name> -f harness=codex -f account=<openai-account> -f model=<codex-model>`. To let a Slack thread opt in to Codex on a role that is otherwise Claude, set `allowed_harnesses=codex` on the role (comma list; a role always allows its own resolved harness) and mention the bot with `--harness codex` or `--model codex`; the bot refuses with the allowed set when the role does not list it. Per-harness latency series carry a `harness` label and beads-viewer badges Codex agents. Full guide, env vars and debugging steps: [`harness-codex.md`](harness-codex.md).
- The **initial spawn nudge** is keyed by the agent's detected *type* (thread/crew/gardener), not your role name — a brand-new role gets the generic nudge for its type, then your `config:claude-instructions:role:<name>` overrides apply once `gb prime` runs.
- **Do not name your role the same as a future built-in.** `EnsureConfigs` upserts code-seeded instruction beads on every controller startup, which would clobber a runtime instructions bead that collides with a code-defined role name.

---

## Step 4b — (Optional) Wire a Slack channel to a role as its default handler

Prod channel→role routing lives on the **project bead** (the daemon `type:project`
TypeConfig is the single source of truth — projects are edited CLI-only; there is **no**
git-tracked source to reconcile). Three fields drive it:

- **`slack_channel`** — a CSV of the project's Slack channel IDs. A channel must be listed
  here for the project to own it (channel→project resolution, primary-channel fallback).
- **`channel_roles`** — a JSON map `{channelID: role}`. A channel with a role here becomes
  that role's **default handler**: an @mention in the channel spawns that role
  (`roleFromChannel`, `bot_commands.go`), and that role's agent cards route **back** to the
  channel (`resolveChannel` → `projectRoleChannel`, `bot_agents.go`).
- **`channel_modes`** — a JSON map `{channelID: "mention"|"concierge"}` setting how the bot
  answers in the channel; unset means `mention`.

The **first** channel in `slack_channel` is the project's primary channel.

For the full order in which these fields, spawn pins and the bridge default decide where
an agent's card, decisions, escalations and notices go, and how to check one agent's
route with `gb slack route`, see [Where does my agent's card go?](slack-channel-routing.md).

Wire all three with `gb project channel`. It resolves a channel name to its ID, refuses a
channel another project already lists, warns when the role has no `type:role` bead, and
writes `slack_channel`, `channel_roles` and `channel_modes` together in one bead update, keeping
every other channel on the project:

```sh
# Make <role> the channel's default handler (name or C0… ID; add --project if not KD_PROJECT):
gb project channel add my-channel --role <role>

# Optional: concierge mode, or make it the project's primary channel:
gb project channel add C0XXXXXXXXX --mode concierge --primary

# See what is wired (role, mode, primary):
gb project channel list

# Unwire it (drops all three entries):
gb project channel rm C0XXXXXXXXX
```

Re-running `add` on a wired channel changes only the flags you pass. `--role` takes a
comma-separated list when several roles' cards should route to the same channel.

> ⚠️ Do not hand-edit these fields with a bare `kd update -f slack_channel=…` or
> `-f channel_roles=…`: both are shared aggregates, and a bare `-f` **replaces the whole
> value**, silently dropping every other channel/role already mapped. If you must use `kd`,
> use the per-entry flags (`--field-append slack_channel=C0…`,
> `--field-set channel_roles.C0…=<role>`, `--field-remove`, `--field-unset-key`).

Prereqs: the channel must exist and the gardenio app must be a member
(`gb slack channel-create` / `gb slack invite`). Worked example — wiring `#bill-navarro`
(`C0B8Q04M4KZ`) as the gardenio **crew** default handler:
`gb project channel add C0B8Q04M4KZ --project gardenio --role crew` (task ka-0000000709).

**Verify** (the project→channel cache has a ~30s TTL, so allow a moment):

```sh
gb spawn --role <role> --prompt '…'
gb slack route <spawned-agent>
#   Rule:  role_channel — channel_roles maps role <role> in project <project>  ← success
```

> **Gotcha:** spawning from a pod of the **same role** that already has its own channel
> binding (`slack_spawn_channel`/`schedule_slack_channel`/`slack_thread_channel`, or
> `SLACK_SPAWN_CHANNEL` in env) propagates THAT channel onto the child, and it wins over
> `channel_roles`. A channel inherited from a spawner of a different role (a gardener
> spawning crew) loses to the child's role channel (gd-3JHijlgZrE). `--slack-spawn-channel`
> always wins. `gb slack route` names the rule that won.

---

## Step 4c — (Optional) Pick the project's Slack card theme

Every Slack card gardenio posts for a project's channels (agent cards, wrap-up cards,
compact cards, bead unfurls, the assistant thread status) renders through one of two
themes, chosen **per project** by the `card_theme` scalar on the `type:project` bead:

- **`garden`** (default; also used when the field is unset or the channel has no
  resolvable project) — the Greenhouse glyph set (🌱 task, …) and palette.
- **`business`** — a restrained, conventional glyph set and palette for channels shared
  with people who don't want the garden metaphor.

Either theme keeps every glyph glued to its verbal label, so cards stay readable by screen
readers and in notifications. Full glyph tables: `gardenio/plans/slack-greenhouse-restyle.md`.

Set it from the **beads-viewer** project editor (`/p/<project>/edit`, admin-gated, a
`<select>` next to `model`/`image`) or the CLI:

```sh
kd update <project-bead-id> -f card_theme=business    # or garden
kd update <project-bead-id> --unset-field card_theme   # back to the default
```

No redeploy, no env var, no helm value. The bridge caches the channel→theme lookup for
~30s (`themeCacheTTL`, `theme.go`), so the next card posted after that window uses the new
theme; cards already posted keep their old look until they next update in place.

## Step 4d — (Optional) Project budget

A project can carry a budget in **US dollars** on its `type:project` bead (decision
`gd-PtNuTyjQoN`). People scoped to the project see an honest "$ remaining" against it;
accounts are shared across projects, so an account's spend limit cannot stand in for it.

- **`budget_usd`** — the amount, a number greater than 0 (e.g. `250` or `99.50`). Blank
  means **no budget**, and the scoped header hides "$ remaining". A literal `0` is
  rejected rather than stored, so the UI never shows a fake $0 remaining.
- **`budget_period`** — the window the amount covers: `month` (calendar month, the default
  when unset) or `7d` (rolling seven days).

The budget is **display-only**: nothing pauses a gardener or blocks a spawn when it runs
out. It applies to operator projects. Tenant lands keep their purse in credits (Lands 8.1)
and do not use `budget_usd` as their limit.

Set it from the **beads-viewer** project editor (`/p/<project>/edit`, admin-gated, the
"Budget" section; it rejects 0, negatives and non-numbers inline, and blanking the amount
clears both fields) or the CLI:

```sh
kd update <project-bead-id> -f budget_usd=250 -f budget_period=month
kd update <project-bead-id> --unset-field budget_usd --unset-field budget_period  # no budget
```

The daemon only checks that `budget_usd` is a number, so a `0` or negative written from the
CLI is stored but turns the project's config-validity badge red until it is fixed.

## Releases

A project's releases (what each tag carries and where it runs:
[`releases-design.md`](releases-design.md)) are switched on by three fields on its
`type:project` bead, and nothing else. Like every project field, only `gardenio-admin` writes
them.

| Field | Type | Meaning |
|---|---|---|
| `release_tag_pattern` | string, a Go regexp | A tag must match it to be a release. Unset: the project has no releases, and the daemon refuses a release bead for it. gardenio uses `^20\d{2}\.\d{1,3}\.\d+$` (calver); a semver project `^v\d+\.\d+\.\d+$`. |
| `release_targets` | JSON list of `{name, kind}` | The named places the project runs, e.g. `[{"name":"prod","kind":"k3s"}]`. A deployment must name one of them. |
| `release_branch` | string, optional | The branch whose first-parent line a release's contents are counted on. Default: `default_branch`. |

The daemon refuses a pattern that does not compile and a target list that is not a list of
named, unique targets. To switch releases on for an existing project:

```sh
kd update <project-bead-id> \
  -f release_tag_pattern='^v\d+\.\d+\.\d+$' \
  -f release_targets='[{"name":"prod","kind":"k8s"}]'
```

A release is a `type=release` bead parented to the project bead, one per tag. Anyone who can
read the project reads them; the release captain, the operator and `gardenio-admin` write them:

```sh
kd release list --project <project>                        # newest first: tag, status, where it runs
kd release show <tag> --project <project>                  # or by the release bead's id
kd release create --project <project> --tag v1.0.0 --sha <sha>
kd release deploy v1.0.0 --project <project> --target prod --revision <n>
```

A release's status is derived by the daemon: `cut` when recorded, `built` once every artifact
exists, `live` while it runs on a target, `superseded` once replaced on every target.

---

## Step 5 — Spawn an agent and verify the PR loop

```sh
# Create an issue bead in the new project, then spawn an agent on it:
kd create '<first task>' --type=task -l project:<project-name>
gb spawn --task <task-bead-id> --role <role-name>
```

- **Spawn does no validation** — a typo'd project or role, a sparse project bead, an unresolved account all spawn a pod that *looks* alive. So verify deliberately.
- Without `--task` and with `auto_assign` unset (default OFF), a crew agent **idles forever** (boots, primes, finds no work). Always pass `--task` for a fresh project, or set `auto_assign=true` on the project (then it claims issue-kind beads labeled `project:<name>`).
- **ProjectCache lag (~60s):** a just-created project bead is invisible to pod spawning for up to a minute, with no "not ready yet" signal. If the first spawn lands in an empty namespace state, wait and retry.

### Verify the agent can actually make a PR

Inside the agent (or by inspecting its session): it should be able to run

```sh
git remote -v                 # origin = your repo, push URL present
glab auth status              # logged in to gitlab.com as the bot
git push origin agent/<x>     # succeeds (not rejected by protection)
glab mr create ...            # opens the MR
gb mr link <bead-id> <url>    # REQUIRED — the bridge is forward-only; forgetting drops the MR from all tracking
```

If the push is rejected → bot lacks push rights or branch protection blocks `agent/*` (Step 3d). If `glab auth status` is empty → `GITLAB_TOKEN` not synced or wrong env name (Step 3). If clone never happened → `git_url` empty or non-gitlab.com host (Step 1 / known init-clone bug ka-0000000552).

---

## The 60-second checklist

```
[ ] type:project bead created, title = project name
[ ] git_url set (gitlab.com), default_branch correct, prefix set
[ ] repos[] JSON well-shaped (verify green badge on the project hub page)
[ ] type:account bead created, account field on project matches its title
[ ] AWS SM secret 'gardenio/<project>-*' created with PUSH-capable PAT (write_repository + api)
[ ] ESO IRSA read-grant: 'gardenio/<project>-*' added to platform.tf p44_oauth_bootstrap_secret_arns + terraform apply -target=module.platform (Step 3a-bis — SKIP THIS AND SPAWNS DEFER FOREVER)
[ ] project secrets[] references it with env GITLAB_TOKEN/GIT_TOKEN, secret name prefixed '<project>-'
[ ] kubectl get secret <project>-gitlab-token -n gardenio  → exists (ESO synced)
[ ] (only if the project runs workloads in its OWN namespace) namespace RBAC added under projectNamespaceRbac in project-namespace-rbac-values.yaml, subject SA gardenio-system/gardenio-agent, scoped to the project namespace only (Step 3e)
[ ] (same) kubectl get role,rolebinding -n <project-ns> -l gardenio.io/project  → objects exist AND managed-by=Helm (blank = not applied; missing managed-by = still a hand-apply)
[ ] bot is Developer/Maintainer on the repo; agent/* branch prefix allowed by protection
[ ] (optional) type:role + config:claude-instructions:role:<name> beads created
[ ] (optional) releases: release_tag_pattern + release_targets set on the project bead; kd release list --project <name> answers
[ ] (optional) channel default-handler: gb project channel add <channel> --project <proj> --role <role>  (NEVER bare kd update -f — it clobbers the shared map)
[ ] gb spawn --task <issue> --role <role>  → agent claims work
[ ] agent: git push + glab mr create + gb mr link  → first PR lands
```

---

## See also

These cover *adjacent* concerns and are commonly confused with this runbook:

- [`onboarding-mac-operator.md`](../../docs/onboarding-mac-operator.md) — set up the operator laptop (the `kd` + `gb` CLIs against the remote daemon). Per-person, not per-project.
- [`byo-cluster.md`](byo-cluster.md) — stand up an entire gardenio on your own Kubernetes cluster. Whole-platform, not a single project.
- [`first-admin-bootstrap-runbook.md`](first-admin-bootstrap-runbook.md) — bootstrap the first Keycloak admin for a fresh gardenio. Identity bootstrap, not project onboarding.

Source friction audit (50 verified findings) and remediation backlog: epic ka-0000003121, bundle ka-0000004912.
