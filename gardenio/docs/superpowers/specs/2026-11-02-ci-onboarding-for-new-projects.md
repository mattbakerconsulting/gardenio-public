# CI onboarding for new projects: the gardenio CI lessons as product defaults (design spec)

**Status:** DRAFT for the owner, 2026-11-02. Docs only: nothing here is built, installed or changed in any tenant
repo. The decision is a `type=decision` bead under the CI slimming plan gd-WFMvg9sDKE.
**Ask (owner, 2026-11-02 ~02:10Z):** "Further, I would like to propagate these lessons to all users of gardenio, not
just this project; a ci onboarding plan for new project should probably be researched".
**Amended by the owner's later ruling on releases:** "2 releases a day isn't much… we should prioritize making
releases cheap so we can do more on the margin". For tenants this spec therefore proposes cheap, on-demand releases
and no fixed per-day cadence (§2.7).
**DECIDED (owner, 2026-11-02 ~02:37Z, decision gd-oQAIXhuIm9 = `adopt-hosted-only`):** "Agreed; keep it on the
target repo's dime but keep cheap that's on us". Tenant CI runs on GitHub-hosted runners and is billed to the
tenant's repo or account (§5). Keeping it cheap is gardenio's job, not the tenant's (§5.1). Phase 5 is out.

Every claim was read on `main` at `b089b0b91` (paths relative to the repo root), with `kd` and read-only `gh` on
2026-11-02. The live test tenant is `fimbazland/crab-shack-3.5` (gd-bmsQ7FT3oj, comment of 11-01 19:23Z).

---

## 0. Summary

1. **Today a new project gets no CI from gardenio.** The provisioner wires a project bead, purse, Keycloak group,
   channels, namespace, steward and mail. It writes no workflow, sets up no runner, and sets no release pattern. The
   Lands App can read checks but has no webhook, so nothing in gardenio ever sees a tenant's CI.
2. **The seven lessons become a starter kit plus steward duties.** The kit is a thin caller workflow, a
   `.gardenio/ci.toml` that names the repo's units, and two vendored scripts (`changed-units.sh`, `quarantine.sh`).
   The steward proposes it, keeps the quarantine list, reports CI in the digest, and cuts releases on demand.
3. **One tier map for every stack.** PRs run only the changed units: Go packages, npm workspaces, Python projects,
   Cargo crates. Main pushes and a daily run run everything. A release runs only what changed since the last tag.
   A repo with no tests gets a smoke tier: secrets scan plus syntax checks.
4. **Tenant CI runs on GitHub-hosted runners**, billed to the tenant's GitHub account and free on public repos.
   Gardenio's ARC pool is shared, has privileged dind on its heavy sets, and is capped by `ci_max_nodes = 6`; it is
   not a place for a stranger's PR code.
5. **The person commits the caller workflow once.** The Lands App has no `workflows` permission, and this spec keeps
   it that way (Q2). After that one commit, the steward maintains everything else through normal PRs.
6. **The rollout has six phases, each step one crew PR.** Every gate is an event or evidence (a fixture goes red, the
   first tenant run lands), never elapsed time.

## 1. What a new project gets today

| Piece | Today | Evidence |
|---|---|---|
| Provisioning | 7 steps: project bead, purse, keycloak, channels, namespace, steward, mail. It creates no repo; the land points at the applicant's existing repo | `gardenio/controller/internal/lands/provision/provision.go:6-25`, `:597-657` |
| Agents on a land | Steward only. `triggers_enabled` and `schedules_enabled` are false at birth, and `lands.tenantSpawns` is off, so lands-gardeners are refused | `provision.go:630`, `helm/gardenio/values.yaml:3824`, `gardenio/docs/lands/provisioner.md:26-32` |
| CI workflow in the tenant repo | None installed. Nothing in the controller, kbeads or `lands/` writes `.github/workflows` | search over those trees |
| GitHub App | `gardenio-lands`: contents, pull_requests, issues write; checks read; metadata. **No** actions, workflows or statuses. `default_events: []`: "No webhook, no events" | `gardenio/docs/lands/github-app.md:27-38,63,76`, `internal/githubapp/identity.go:41-55` |
| Wake on CI | `gb wait ci` plus the github-bridge ledger exist for the **fleet** App only. The bridge is still off on k3s (`githubBridge.enabled: false`, escalation gd-AUdgkFSB1f). For a tenant it cannot work even once enabled: there is no Lands webhook, the bridge mints only fleet tokens, and the Lands token lacks `actions:read` | `cmd/github-bridge/main.go:277-292`, `tokens.go:59-67`, `ciwait.go:15-35`, `cmd/gb/wait_ci.go:210` |
| Merge policy | The playbook says "you do not merge it: the owner does", and the Never list includes "Merge a PR, approve anything". At the same time kbeads sets `land_steward_approves=["github_pr_merge"]` on new tenant lands, and the greeting says the steward approves merges. `steward_authority` (`ask`/`merge-on-green`/`operate`) is read by the greeting only. No grant checks CI | `internal/steward/steward.go:~638,~841,570-572`, `kbeads/internal/server/land_steward_delegation.go:36`, `gardenio/docs/lands/steward.md:351-357` |
| Releases | Recording only. The provisioner sets no `release_tag_pattern`, so "a new land has no releases". This contradicts `gardenio/docs/releases-design.md:338`, which says the provisioner sets it. The bridge's tag poll uses the fleet token source, so a tenant tag would likely not be read either (inferred, not run) | `gardenio/docs/lands/provisioner.md:44-60`, `cmd/github-bridge/release.go:298-306` |
| Steward digest | Steps done, plans, answers, gardeners, in flight, credits. No CI line | `internal/steward/digest.go:62-99` |
| Cost / budget | The purse is LLM spend only (`purse/purse.go:5-25`). `credit_rate` compute is 0. Nothing meters CI | gd-bmsQ7FT3oj comment 10-28 01:03Z |
| Runners | Every ARC scale set is registered to one `mattbakerconsulting/*` repo by `githubConfigUrl`: gardenio ×4, beads-hq, gascity-hq, gascity-packs-hq. No org-level set and no runner group | `gardenio/helm/ci/arc-runners-*-values.yaml`, `gardenio/helm/ci/README.md:73-130` |

**The live tenant, `fimbazland/crab-shack-3.5`,** is a public fork of `groblegark/crab-shack-3.5`: JavaScript
(3.7 MB), Python, C and shell, with no `package.json`, `pyproject.toml`, `go.mod`, `Cargo.toml`, Makefile or test
files. Its one workflow, `image.yml`, runs on main pushes on `ubuntu-latest`. It has **0 workflow runs** (GitHub
keeps Actions off on a fork until the owner enables them) and no PRs. This is the case §3.3 must handle.

## 2. The lessons as product defaults

| # | Lesson | Status today | For a tenant project |
|---|---|---|---|
| 1 | No big CI on PRs | gardenio-repo-only (`ci.yml` `draft-light`; #1876 open) | missing |
| 2 | Liberal quarantine, one list read from main | gardenio-repo-only; `scripts/quarantine.sh` knows `go` and `golden` rows only | missing |
| 3 | Breaking main is not taboo: attribution, then quarantine or fix by release | gardenio-repo-only (batched main runs, desk Step T0, the daily-run issue in draft #1896) | missing |
| 4 | No bake-in periods | a working rule in gardenio plans, written nowhere as product policy | missing |
| 5 | Event-driven agents, not timed crons | product default for new plan gardeners (#1907). The steward's quiet-gardener watch (#1918) was **reverted** on main by #1923 for red lint. Tenants are event-driven by construction: steward bead events plus one daily digest cron (`steward/bind.go:164-212`) | default, keep it so |
| 6 | Truck driver per release | draft spec #1921, gardenio only ("other lands' releases" out of scope, its §11) | missing |
| 7 | Release cadence (superseded): cheap releases, on demand | gardenio cuts by `release-cut.sh` newest-green walk, `REPO` hardcoded (`:24`) | missing |

### 2.1 The starter kit (lessons 1, 3)

The kit lives in gardenio at `lands/ci/` and is vendored into the tenant repo: `.github/workflows/gardenio-ci.yml`
(about 40 lines the person commits once), `.gardenio/ci/` (the scripts, which the steward updates by PR:
`.github/workflows` is the only path the App cannot write) and `.gardenio/ci.toml` (the repo's units and commands).

| Event | Tier | What runs |
|---|---|---|
| `pull_request` (draft or ready) | **light** | `changed-units.sh` maps the diff to units; each changed unit runs its `build`, `lint` and `test` commands. Budget 10 min; `timeout-minutes: 15` |
| `push` to the default branch | **full** | every unit's commands. One concurrency group per branch; a newer push replaces the pending run (no cancel of the running one) |
| `schedule`, daily at a minute staggered per land | **full + slow** | everything, plus units marked `slow` (browser, e2e). A red run opens or updates the pinned issue "Daily full CI run: red" (the workflow's own `GITHUB_TOKEN` with `issues: write`) |
| `push` of a tag, or `workflow_dispatch` `release` | **release** | units changed since the previous tag, plus their `release` command (§2.7) |

- PR concurrency cancels in progress, per ref; no `ready_for_review` trigger and no matrix, so a PR costs one
  light job however it is flipped. In a one-unit repo the light tier is the whole suite, which is correct: lesson 1
  is about cost, and the changed-unit rule finds the cheap set by itself.
- **Never retrigger.** Tenant agents have no `actions` permission, so they cannot rerun jobs. The rule against empty
  commits and update-branch goes into the lands-gardener and steward prompts, in the same words as `CLAUDE.md:120-137`.
- **Attribution (lesson 3).** The full run on each push names the batch that broke the default branch. The steward
  (§4) writes "main red since `<sha>`, merges `<range>`, test `<name>`" to `#machines` and to the land's digest. It
  then quarantines (§2.2) or opens a fix step. There are no PR-time guards against breaking main.

### 2.2 Quarantine for every stack (lesson 2)

The list is `.github/quarantine.tsv`, with gardenio's columns (`kind name bead added expires evidence`) and the
same policy: quarantine on first sighting, a quarantined test still runs as advisory, and an expiry is a nag, not a
gate. It adds `kind` values per ecosystem, each validated by a strict pattern before it reaches a runner flag:

| kind | name | how it is skipped |
|---|---|---|
| `go` | top-level `TestX` | `go test -skip` (as today) |
| `pytest` | node id `tests/test_a.py::test_b` | `--deselect` |
| `jest` / `vitest` | `file::test name` | `--testNamePattern` negative, or a generated skip config |
| `cargo` | test path `mod::test` | `cargo test -- --skip` |
| `golden` | screenshot file name | the project's golden helper reads `GARDENIO_QUARANTINE` |

**Read from where the steward can write without a merge.** In gardenio, CI reads main's copy, and the desk pushes
rows straight to main. A tenant steward under the default `ask` grant cannot push to the default branch. So the
helper reads the union of the default branch's list and a steward-owned branch `gardenio/quarantine`. That branch
holds only this file, and the App can push it with `contents: write` today.

A row takes effect on every open PR's next run (the "never block a second PR" rule). The person sees every row in
the branch history and the digest, and removes one by PR or by asking; a fix PR deletes its row and the steward
prunes the branch copy. This is Q3.

### 2.3 No bake-in periods (lesson 4)

This is a rule for the steward prompt and for this rollout. A CI step, a grant raise, or a quarantine removal is
gated on an event or evidence: a green run, a merged fix, a positive control going red. It is never gated on
"after N days". The daily run is a schedule for *coverage*, not a waiting period before anything may happen.

### 2.4 Event-driven agents (lesson 5)

Tenant lands already satisfy this, and the kit must not undo it: the steward wakes on bead events, and its one
daily digest cron is the watchdog. **CI completion becomes a bead event, not a poll:** the Lands App subscribes to
`check_suite` (covered by the existing `checks: read`) and the github-bridge learns the Lands identity (Phase 3), so
a completed suite on the default branch or a step's PR writes onto the step bead and wakes the steward or the
step's lands-gardener. Until then the digest wake reads check runs once a day; nothing polls on a cron. The
quiet-gardener watch (#1918, reverted by #1923) is home-garden only; tenants keep the 45-minute `QUIET` flag
(steward.md:384-388).

### 2.5 Merge on green, made real

The kit gives "green" a meaning on a tenant repo: the `gardenio-ci` light check on the PR head, red only on
quarantined rows.

Under `ask` the steward brings the merge to the person with the check's state; under `merge-on-green` it merges a
step's PR whose check is green and whose diff it has read, and writes "merged #N under merge-on-green" to
`#machines`. Step 4.1 makes the Never list, the greeting and `land_steward_approves` say the same thing (§1).

### 2.6 Truck driver per land (lesson 6): opt-in, not default

A small project does not need a release branch. The truck driver (#1921) is offered to a land as an opt-in
`release_flow=branch` once it has more than one unit and a deploy target. The default is `release_flow=tag` (§2.7).
A land that opts in gets the same driver role, run as a lands-gardener inside its purse, with its 3-crew cap
counting against `max_live_agents`.

### 2.7 Releases: cheap and on demand (lesson 7, as amended)

There is no fixed cadence. A release costs only what changed, so a land can release whenever a person, or the
steward under `operate`, asks.

- **Cut.** `gl release cut`, or the steward, tags the newest default-branch commit whose full run is green or red
  only on quarantined rows. This is `release-cut.sh`'s newest-green walk, with the repo and the required check
  name as inputs.
- **Release tier.** The tag runs only the units changed since the previous tag, plus their `release` command:
  publish a package, or push an image. `slow` units (browser) run only if their paths changed.
- **Promote, don't rebuild.** Where the full run on main already built an artifact, such as an image at
  `:sha-<sha>`, the release command retags it and does not rebuild it.
- **Record.** The bridge's tag poll records the release once it uses the Lands token (Phase 0). The tenant's own CD
  reports deploys with `gl release deployed`, as `gardenio/docs/releases-design.md:390-398` already designs.

## 3. Language and stack variety

### 3.1 Units, detected once and written down

At onboarding the steward detects the repo's units and writes them to `.gardenio/ci.toml`, which the person
reviews. CI does not guess at run time, so a wrong guess is visible and fixable in one line.

| Ecosystem | Unit = nearest dir with | Light commands (defaults) | Widen to all units when |
|---|---|---|---|
| Go | `go.mod` (package = nearest dir with `*.go`, as in `ci.yml` `draft-light`) | `go build ./...`, `go vet <pkgs>`, `go test <pkgs>` | `go.work`, `go.sum` |
| Node | `package.json` (npm/pnpm workspace member) | `npm ci`, `npm run --if-present lint`, `npm test --if-present` | root lockfile or root `package.json` |
| Python | `pyproject.toml` / `setup.cfg` / `setup.py` | `pip install -e .[test]` (or `uv sync`), `ruff check`, `pytest` | root lockfile, `requirements*.txt` |
| Rust | `Cargo.toml` with `[package]` | `cargo clippy -p <crate>`, `cargo test -p <crate>` | `Cargo.lock`, workspace `Cargo.toml` |
| none | — | smoke tier (§3.3) | — |

`changed-units.sh` is one script for all of them. For each changed file it walks up to the nearest unit root in
`ci.toml`. If a file changes in `.gardenio/` or in the caller workflow, every unit is selected. A file under no unit
selects nothing beyond the smoke checks. The light-tier guards that #1921 §2.3 found missing in gardenio (lint and
the line budget on the changed packages) are part of each unit's `lint` command from the start.

### 3.2 Per-ecosystem starter set

`lands/ci/starters/{go,node,python,rust,none}.toml` hold the defaults above, pinned tool versions, and cache keys
for `actions/setup-*`. A polyglot repo concatenates them. Gardenio ships fixture repos for each starter under
`lands/ci/testdata/`. Each fixture has a passing unit, a failing unit, and a quarantined failing test, and the
fixtures run in gardenio's own CI as positive and negative controls.

### 3.3 Minimum viable onboarding

For a repo with no manifests and no tests, like crab-shack-3.5, onboarding is:

1. The caller workflow with the smoke tier on PRs and pushes: gitleaks over the PR's own commits, `node --check`
   on changed `.js`/`.mjs`, `python -m py_compile` on changed `.py`, `sh -n` plus shellcheck on changed `.sh`.
   Under a minute.
2. An empty `.github/quarantine.tsv` with the header.
3. The person enables Actions, which a fork needs.
4. The digest's CI line.

When a test runner appears, the steward proposes the matching starter as an ordinary PR to `ci.toml`.

## 4. The steward's role

The steward onboards CI for its land, and the person approves. Its duties, added to `StewardPlaybookPrompt`:

1. **Propose** (first wake after provisioning, or on "set up CI"): detect the units, render the kit, open a PR
   with `.gardenio/` and `.github/quarantine.tsv` (the App can write both), and file one decision for the person
   holding the caller workflow's text and `gl ci init`, plus "enable Actions" on a fork. It never pushes to
   `.github/workflows`.
2. **Watch.** On a CI event (the digest wake until Phase 3), classify reds as in #1921 §4: caused by a merge in the
   batch gets a fix step; flaky or unrelated gets a row on `gardenio/quarantine` with a bug bead. No reruns.
3. **Keep the list.** Prune rows whose fix merged, and nag on expired rows in the digest.
4. **Report.** One digest line, only when something changed: default-branch state, reds and their rows, and
   runner-minutes since the last digest, summed from check-run start/end times (`checks: read`; wall time, not
   billed minutes).
5. **Release** on request, or under `operate`, by §2.7.

## 5. Cost guardrails and where tenant CI runs

**Who pays.** Under the recommended option, the tenant's GitHub account pays. Standard GitHub-hosted runners are
free on public repos. A private repo draws on the account's included minutes, and with GitHub's default spending
limit it stops there. Gardenio pays nothing and adds no node to its pool. The template's own caps: light
`timeout-minutes: 15`, full 45; PR cancel-in-progress and one pending run per branch; path gates for `slow` units;
no matrix by default; one daily run per land.

The digest line (§4) is the meter. A land field `ci_minutes_soft_cap` (default unset) makes the steward say so when
the period's minutes pass it. It informs; it never blocks a run.

### 5.1 Cheap is on us

The tenant pays for the minutes, but gardenio owns how many minutes its defaults burn. A tenant who takes the kit
as shipped should get the cheapest CI that still catches breakage. They should never have to tune it to stop
paying for waste. In practice:

- **The kit's defaults are the cost policy.** PRs run changed units only. Full runs happen on push and once a day.
  `cancel-in-progress` is on, there's no matrix, path gates cover `slow` units, and timeouts are tight. If a
  tenant's runs are expensive, the kit is fixed upstream, and every land picks up the fix on its next kit PR.
- **Gardenio measures it.** The steward digest's CI-minutes line (§4) is read fleet-wide as well as per land. The
  steward flags the most expensive unit and job, and any re-run storm.
- **Gardenio fixes it.** A cost regression in the kit is a gardenio bug, filed under the CI slimming plan,
  carrying the same weight as one in gardenio's own CI. The tenant isn't asked to fix it.
- **Releases follow the same rule** (§2.7): promote, don't rebuild; build only the units that changed.

**Why not gardenio's runners, today.**

- **Untrusted code on shared nodes.** A public tenant repo runs any PR author's code. The heavy gardenio set and
  beads-hq run privileged dind sidecars (`arc-runners-values.yaml:~399`), and a privileged container is a way out
  to the node.
- **Shared node identity and caches.** CI nodes share the cluster's node instance profile and security group
  (`gardenio/terraform/modules/k3s/main.tf:~697-713`). `imdsDeny` covers `arc-runners`
  (`deploy-values/gardenio-k3s.yaml:2950-2961`), so one NetworkPolicy is all that stands between a job and the
  node role. Docker layer
  and build caches on the node disk are shared across jobs, so a tenant job could poison gardenio's.
- **Shared capacity.** `ci_max_nodes = 6` (`composition/gardenio/main.tf:340`) is the gardenio pool's ceiling,
  sized to gardenio's own load. A tenant's runs would queue gardenio's and raise the owner's AWS bill. On 10-27 the
  pool cost about $10/day, against a $100-200/month elastic budget (gd-WFMvg9sDKE).
- **Auth reach.** Scale sets register with the `gardenio-ci` App, which is installed on the `mattbakerconsulting`
  org. A tenant repo lives in the tenant's account, so serving it means a runner-registration App installed on a
  stranger's repo.

**If the owner later wants gardenio runners for a land** (option a, Phase 5), the minimum is a separate tainted
node pool (or per-job VMs), no dind (or rootless BuildKit), IMDS hop limit 1 on that pool, no shared cache,
`maxRunners` per land, fork PRs never on those runners, and minutes metered into the purse through a non-zero
`credit_rate` compute rate.

## 6. Rollout plan

One PR per step, sized for a crew. A phase opens when the previous phase's gate event has happened.

**Phase 0: fix what the kit depends on** (gate: a tenant tag on crab-shack-3.5 is recorded as a release bead).
- 0.1 The provisioner's `project` step sets `release_tag_pattern` (`v*` or calver) and `release_flow=tag`. Fix
  provisioner.md or releases-design.md so they agree.
- 0.2 The github-bridge picks its token source by the project's `github_app` (`lands` → `LandsTokenPermissions`)
  for the tag poll and for check reads.
- 0.3 Depends on gd-AUdgkFSB1f (bridge enabled on k3s). This is an existing escalation, not a new step.

**Phase 1: the kit in gardenio** (gate: the fixtures' negative controls go red and the positive ones go green in
gardenio CI).
- 1.1 `lands/ci/changed-units.sh` with its tests: ci.toml parsing, nearest-unit walk, widen rules.
- 1.2 Generalise `scripts/quarantine.sh`: the repo and branch as inputs, the `gardenio/quarantine` union, and the
  new kinds with strict patterns. Gardenio's own CI keeps calling it unchanged.
- 1.3 The caller workflow template, plus the go/node/python/rust/none starters and fixture repos.
- 1.4 Guards: a test that renders the template for each starter and runs actionlint plus a concurrency check modelled on
  `scripts/check-main-concurrency.py` over it.

**Phase 2: delivery** (gate: crab-shack-3.5's first PR run and first default-branch run both show in the digest).
- 2.1 `gl ci init`, which renders the kit locally from `ci.toml`, and `gl ci status`.
- 2.2 The steward playbook's "CI onboarding" duty (§4.1) and the onboarding decision template.
- 2.3 The first tenant. The person, on crab-shack-3.5 as fimbazland, enables Actions, commits the caller, and merges
  the steward's `.gardenio/` PR.

**Phase 3: CI as events** (gate: a red default-branch run on a tenant wakes its steward with no cron involved).
- 3.1 The Lands App gets a webhook URL and secret and `default_events: ["check_suite"]`, kept equal across its
  three places by `github-app-register.py` tests; the operator updates the App (first check whether existing
  installations must re-accept).
- 3.2 The github-bridge handles Lands-App webhooks and writes check results onto step and project beads.
- 3.3 The steward's watch and quarantine duties (§4.2-4.3), plus the digest CI line (§4.4).

**Phase 4: merges and releases** (gate: one tenant release cut on demand and recorded, and one merge under
`merge-on-green` audited in `#machines`).
- 4.1 Make the playbook's Never list, the greeting and `land_steward_approves` agree. Then wire `merge-on-green` to
  the light check.
- 4.2 `gl release cut`, with the newest-green walk taking the repo and check as inputs and the release tier
  running only changed units.
- 4.3 `release_flow=branch` opt-in, reusing #1921's driver once that spec is adopted.

**Phase 5 (NOT ADOPTED: the owner chose option b, 11-02): gardenio runners for a land, opt-in** (gate: a security review of §5's list passes, and
the owner approves a named land).

## 7. Open questions for the owner (each has a recommendation)

- **Q1. Where does tenant CI run?** Recommended: **GitHub-hosted only** (decision option b). It is free on public
  repos, billed to the tenant otherwise, and keeps strangers' code off the shared pool. Gardenio runners stay a
  later opt-in only if a paying land needs them.
- **Q2. Does the Lands App gain `workflows: write` so that the steward can open the workflow PR itself?**
  Recommended: **no**. The person commits about 40 lines once (`gl ci init`), and everything after that is
  `contents: write`. Adding the permission makes every tenant re-approve the App and lets tenant agents edit CI.
- **Q3. Where does the quarantine list live so that a row needs no merge?** Recommended: **the union of the default
  branch's copy and a steward-owned `gardenio/quarantine` branch**. The alternative is a narrow grant for the
  steward to merge list-only PRs.
- **Q4. Vendored scripts or a public reusable-workflow repo?** Recommended: **vendored** (`.gardenio/ci/`, updated
  by steward PRs). A public `gardenio-ci` repo would be easier to upgrade, but it is a new public footprint and an
  unpinned supply-chain edge into every tenant.
- **Q5. Default grant for new lands?** Recommended: **keep `ask`**. Offer `merge-on-green` in the onboarding
  decision once the light check exists, so that "green" means something first.
- **Q6. Truck driver for tenants?** Recommended: **opt-in `release_flow=branch`**, with tag-on-demand as the
  default. Most lands have one unit and no deploy target.
- **Q7. Daily run for tenants?** Recommended: **yes, one per land at a staggered minute**. It is the only place
  `slow` units run on a schedule. A land can turn it off in `ci.toml`.

## 8. Not in scope

Gardenio's own CI (gd-WFMvg9sDKE phases 7-8, #1876, #1896); the truck driver itself (#1921); unpausing the
deployer; GitLab tenants; any change to a tenant repo, the Lands App or a live trigger before the decision.
