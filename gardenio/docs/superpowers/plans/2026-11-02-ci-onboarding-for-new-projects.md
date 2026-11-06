# CI onboarding for new projects: gardenio's CI lessons as product defaults for every land — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every new land gets cheap CI from gardenio on GitHub-hosted runners. The parts:
- a thin caller workflow that the person commits once;
- a `.gardenio/` kit that the steward keeps current by pull request;
- one tier map for Go, Node, Python, Rust and repos with no tests;
- a quarantine list that never blocks a second PR;
- a CI line in the steward's digest;
- CI completion as a bead event;
- a merge-on-green that means something;
- releases cut on demand, building only what changed.

Cheap is gardenio's job, not the tenant's.

**Owner direction:**
- 2026-11-02 ~02:10Z (the ask): "Further, I would like to propagate these lessons to all users of gardenio, not just this project; a ci onboarding plan for new project should probably be researched".
- The owner's release ruling amends it: "2 releases a day isn't much… we should prioritize making releases cheap so we can do more on the margin". So tenants get cheap releases on demand and no fixed cadence.
- 2026-11-02 ~02:37Z: decision gd-oQAIXhuIm9 chose `adopt-hosted-only`. The owner wrote: "Agreed; keep it on the target repo's dime but keep cheap that's on us". Tenant CI runs on GitHub-hosted runners, billed to the tenant's repo or account. Keeping it cheap is gardenio's job (spec §5.1, binding). Phase 5, gardenio runners for tenants, is NOT adopted and is not in this plan.

**Spec:** `gardenio/docs/superpowers/specs/2026-11-02-ci-onboarding-for-new-projects.md` (PR #1929). It explains the why. Every fact a task needs is copied into that task, because a crew reads only its bead.

**Architecture:**
- **Phase 0** fixes what the kit depends on:
  - the provisioner sets a land's release source and `release_flow=tag`;
  - the github-bridge reads a tenant's tags with the Lands App token instead of the fleet token.
- **Phase 1** builds the kit in gardenio at `lands/ci/`:
  - a ci.toml reader and `changed-units.sh`;
  - a generalised `quarantine.sh` with per-ecosystem kinds and a steward-owned `gardenio/quarantine` branch;
  - the caller workflow template and its runner scripts;
  - a composite setup action;
  - five starters;
  - one Go renderer (module `gardenio/lands/ci`);
  - fixture repos that act as positive and negative controls;
  - guards over the rendered workflow.
- **Phase 2** delivers the kit:
  - `gl ci init` and `gl ci status` for the person;
  - `gb lands ci render` for the steward;
  - the steward's CI onboarding duty and its decision;
  - the digest's CI line, read once a day;
  - the first tenant, crab-shack-3.5.
- **Phase 3** turns CI into events:
  - the Lands App subscribes to `check_suite`;
  - the github-bridge writes check results onto the step and project beads, and posts a notice that wakes the steward;
  - the steward's watch and quarantine duties;
  - a fleet-wide CI cost read.
- **Phase 4** handles merges and releases:
  - a land's grant, its `land_steward_approves` and the playbook say the same thing;
  - merge-on-green is gated on a green `gardenio-ci/light` run at the PR head;
  - the kit's newest-green `release-cut.sh`;
  - the steward's release duty and `gl release cut`.

**Tech stack:** Go 1.25 (`gardenio/controller` cmd/gb, cmd/github-bridge, internal/steward, internal/lands/provision; `kbeads/internal/server`; `lands/gl`; new module `lands/ci`), GitHub Actions on `ubuntu-latest` (tenant side), bash and Python 3.11+ stdlib (`tomllib`) for the vendored kit scripts, `gh` CLI, the Helm chart `gardenio/helm/gardenio`.

**Anchors:** verified against `origin/main` at `8af6bbd6d` (2026-11-02, "Merge pull request #1930"). Lines drift. If a cited line has moved, find it by the quoted text and say so in the PR.

**Live facts (read-only, 2026-11-02):**
- The live test tenant is `fimbazland/crab-shack-3.5`, a public fork of `groblegark/crab-shack-3.5`. It is JavaScript (3.7 MB), Python, C and shell, with no `package.json`, `pyproject.toml`, `go.mod`, `Cargo.toml`, Makefile or test files. Its one workflow, `image.yml`, runs on main pushes on `ubuntu-latest`. It has 0 workflow runs (GitHub keeps Actions off on a fork) and no PRs.
- Its land is not provisioned yet. That waits on the first-tenant gates task gd-bmsQ7FT3oj (blocked; IdP, credit rate and namespaces done, tenant spawns and mailer in progress). The person's GitHub identity is `fimbazland`. The owner's clone is `~crab/src/crab-shack-3.5` on the Vultr host.
- The github-bridge is off on k3s (`githubBridge.enabled: false`). Turning it on is escalation gd-AUdgkFSB1f (in_progress, operator-held). That is an existing escalation, not a step of this plan. Phase 0's gate needs it.
- Every ARC scale set is registered to one `mattbakerconsulting/*` repo. `ci_max_nodes = 6` is the gardenio pool's ceiling.

**Plan beads** (minted 2026-11-02, `plan_state=ready`, no captain bound): plan epic `gd-QM4BoQ9gzO` (`plan:ci-onboarding`).

| Phase | Phase epic | Steps |
|---|---|---|
| 0 | `gd-oZ1VvmrrnB` | 0.1 `gd-wo6vzs6tjm`, 0.2 `gd-5ooLXIbk0e`, 0.3 `gd-cHjYGLl0eg` |
| 1 | `gd-YqnUB50BAs` | 1.1 `gd-xS8k8Qqpay`, 1.2 `gd-615J2iOMHz`, 1.3 `gd-5VfuNMaT0B`, 1.4 `gd-w6DI4WJAOa`, 1.5 `gd-NZYd7LO6uH`, 1.6 `gd-Rvl1lkDhGz` |
| 2 | `gd-h6oS8rp6Wp` | 2.1 `gd-ZCk8ooF1Hf`, 2.2 `gd-l5JkZu9iKw`, 2.3 `gd-5NjUboUxfb`, 2.4 `gd-wfnTmJnMyT`, 2.5 `gd-AzXbvgbil4` |
| 3 | `gd-iAuGg0vZKg` | 3.1 `gd-EqLUculUla`, 3.2 `gd-2N521f7z2R`, 3.3 `gd-EkMee4XB6e`, 3.4 `gd-9PMqtKzt5r`, 3.5 `gd-lfrDes3uRO` |
| 4 | `gd-Fs78vhiCEJ` | 4.1 `gd-At3RyyzAio`, 4.2 `gd-VvvxOHtm5V`, 4.3 `gd-ZozPWIibRf`, 4.4 `gd-x8xCMYPdM9`, 4.5 `gd-iqK5OR1JpM` |

## Decisions

- **D1 (owner, gd-oQAIXhuIm9, 2026-11-02):**
  - Tenant CI runs only on GitHub-hosted runners, billed to the tenant's GitHub account. Standard runners are free on public repos; a private repo uses the account's included minutes.
  - Gardenio pays nothing and adds no node.
  - Phase 5 is out.
- **D2 (owner, the same decision: "Agreed" to the spec's recommendations Q2-Q7):**
  - Q2: the Lands App gains no `workflows` permission. The person commits the caller once.
  - Q3: the quarantine list is the union of the default branch's `.github/quarantine.tsv` and the same file on a steward-owned `gardenio/quarantine` branch.
  - Q4: the kit's scripts are vendored into `.gardenio/ci/`. There is no public reusable-workflow repo.
  - Q5: a new land keeps the `ask` grant.
  - Q6: the truck driver is an opt-in `release_flow=branch`, and tag-on-demand is the default.
  - Q7: one daily run per land, at a staggered minute.
- **D3 (operator, this plan): one renderer.**
  - The kit is a Go module `gardenio/lands/ci` (package `kit`) that embeds every kit file and owns `Parse`, `Detect` and `Render`.
  - `gl ci init` (the person's machine) and `gb lands ci render` (the steward's pod) both call it, so the two can never render different kits.
  - `lands/gl` and `gardenio/controller` reach it with a `replace` directive, as kbeads reaches `lands/contract`.
- **D4 (operator): check names and "green".**
  - The caller workflow is named `gardenio-ci`. Its jobs are named `gardenio-ci/light`, `gardenio-ci/full`, `gardenio-ci/daily`, `gardenio-ci/release` and `gardenio-ci/daily-issue`, and those are the check-run names.
  - "Green" means that check run concluded `success`.
  - A quarantined test still runs as advisory but never fails its job. So "green, or red only on quarantined rows" (spec §2.5, §2.7) is exactly `success`.
  - Failing tests are reported as check-run annotations titled `gardenio-ci failing test`, which the Lands token can read with `checks: read`.
- **D5 (operator): the digest's CI line is Task 2.4, not Phase 3.**
  - The spec lists the digest CI line under step 3.3. But Phase 2's gate is "the first PR run and the first default-branch run both show in the digest", and spec §2.4 says the digest wake reads check runs once a day until Phase 3.
  - So the daily read lands in Phase 2. Phase 3 adds the event wake.
- **D6 (operator): the caller is static.**
  - Toolchains come from a composite action at `.gardenio/ci/setup/action.yml`, which the App can write. The tier logic lives in `.gardenio/ci/run.sh`.
  - So the person commits the caller once, and every later change is an ordinary steward PR outside `.github/workflows`.
  - The caller is about 75 lines, not the spec's "about 40", because it also carries the daily run, its issue job and the release tier.
- **D7 (operator): under `ask` the steward approves no merge.**
  - A land's `land_steward_approves` takes effect for merges only when its `steward_authority` is `merge-on-green` or `operate` (Task 4.1).
  - This makes the Never list ("Merge a PR, approve anything"), the greeting and the daemon's delegation agree. Today they contradict each other (spec §1, "Merge policy").
- **D8 (operator): `gl release cut` asks the steward.**
  - `gl` holds no GitHub credential (`lands/gl/README.md`). So `gl release cut` opens a ticket titled `Cut a release`, which wakes the steward.
  - The steward cuts with the kit's `release-cut.sh` and its Lands installation token, which carries `contents: write`.
- **D9 (operator): spec step 4.3 is not in this plan.**
  - Step 4.3 is the `release_flow=branch` opt-in that reuses the truck driver. It reuses #1921's driver "once that spec is adopted", and #1921 is not adopted, so no concrete task can be written.
  - `release_flow` is still declared (enum `tag|branch`, Task 0.1), so the later step needs no schema change.
- **D10 (operator): §5.1's fleet-wide cost read is Task 3.4.** Spec §6 lists no step for it, but §5.1 is binding.
- **D11 (operator): phase gates are blocks edges.**
  - "A phase opens when the previous phase's gate event has happened" (spec §6). Each phase therefore ends in a gate task, owned by the operator or the person, and the next phase's first tasks are blocked by it.
  - Two gates wait on things outside this plan, which are named in their bodies and not linked as edges:
    - Phase 0's gate needs gd-AUdgkFSB1f and a provisioned crab-shack land (gd-bmsQ7FT3oj).
    - Phase 4's merge gate needs tenant spawns on.

## Global Constraints

1. **No bake-in periods.** Every gate in this plan is an event or evidence: a run goes green, a fixture's negative control goes red, a release bead exists, a wake fires. Nothing waits on elapsed time. Measuring afterwards is fine, as long as nothing waits on it.
2. **Breaking main is not taboo.** A red main is not an emergency. What matters is attribution (which merge broke what), then a quarantine row or a fix by release time. No task adds a PR-time guard to stop main going red. The same holds for tenants: the full run on each push names the batch that broke the default branch, and the steward quarantines it or opens a fix step.
3. **No big CI on PRs: the light tier only.**
   - In gardenio, crews open PRs as drafts with `gb pr open`.
   - Never start a heavy PR run on purpose: no empty commits to retrigger, no merging main in, no "Update branch", no `gh run rerun` sweeps, no draft/ready flip just to get CI.
   - A PR that is red only because main is red is not re-proved on the PR.
   - For tenants, a PR runs `gardenio-ci/light` and nothing else. The kit has no `ready_for_review` trigger and no matrix, so a PR costs one light job however it is flipped.
4. **Tenant CI runs on GitHub-hosted runners only, never on gardenio's ARC scale sets.**
   - Every job the kit renders says `runs-on: ubuntu-latest`. No task routes a tenant job to a `gardenio-k3s*` label.
   - No task adds a runner registration, runner group, scale set or self-hosted label for a tenant.
   - Phase 5 is not adopted.
5. **Cheap is on us.**
   - The tenant pays for minutes, but gardenio owns how many minutes its defaults burn. The kit's defaults are the cost policy:
     - PRs run changed units only;
     - full runs happen on a push and once a day;
     - `cancel-in-progress` is on for PRs;
     - there is no matrix;
     - path gates cover `slow` units;
     - timeouts are tight (light 15 min, full and release 45).
   - A cost regression in the kit is a gardenio bug, filed under the CI slimming plan gd-WFMvg9sDKE with the same weight as one in gardenio's own CI. The tenant is never asked to fix it.
   - Releases promote and do not rebuild, and build only the units that changed.
6. **Never modify a tenant repo without the person's approval, in the flow the spec describes.**
   - The steward changes a tenant repo only by a pull request the person merges (`.gardenio/` and `.github/quarantine.tsv`), and by the steward-owned `gardenio/quarantine` branch the onboarding decision tells them about.
   - It pushes a tag only for a release a person asked for, or under the `operate` grant.
   - The person commits `.github/workflows/gardenio-ci.yml` themselves.
   - Operators and crews never push to a tenant repo. On the test tenant, the owner acts as the person `fimbazland`.
7. **The Lands App gains no `workflows` permission**, and no other permission either. `githubapp.LandsTokenPermissions` (`gardenio/controller/internal/githubapp/identity.go:46-54`) stays `contents: write`, `pull_requests: write`, `issues: write`, `checks: read`, `metadata: read`. Phase 3 adds only a webhook and the `check_suite` event subscription, which needs no new permission.
8. **Test narrowly.**
   - Crew pods have a 2Gi limit, so run package-scoped tests (`go test ./internal/<pkg>/ -run <Name>`) and never `go test ./...` at the `gardenio/controller` or `kbeads` root.
   - The small out-of-workspace modules (`lands/gl`, `lands/ci`) run `GOWORK=off go test -count=1 ./...` from the module directory, as CI does.
   - The laptop runs no Go tests, builds or Terraform.
9. **Releases.** Crews never cut gardenio tags. The operator rolls gb, the controller, kbeads and the github-bridge in the normal ~04:00Z or ~16:00Z release windows. A deploy-values flip that adds objects goes through CI like any change.
10. **Every new guard gets a positive control.** A test must show the check letting a good input through and catching each bad input. A zero-match filter looks like success.

## Task map and dependencies

Each CREW task is one PR. OPERATOR and PERSON tasks are not PRs; they are the gate events.

| Task | Spec step | Who | Blocked by |
|---|---|---|---|
| 0.1 Provisioner sets the release source and `release_flow` | 0.1 | CREW | — |
| 0.2 github-bridge reads tenant tags with the Lands token | 0.2 | CREW | — |
| 0.3 Phase 0 gate: a crab-shack tag becomes a release bead | 0.3 + gate | OPERATOR | 0.1, 0.2 |
| 1.1 `changed-units.sh`, the ci.toml reader, the `lands-ci-kit` job | 1.1 | CREW | 0.3 |
| 1.2 Generalise `quarantine.sh` | 1.2 | CREW | 0.3 |
| 1.3 Caller template, runner scripts, starters, Go renderer | 1.3 | CREW | 1.1, 1.2 |
| 1.4 Fixture repos: positive and negative controls | 1.3 | CREW | 1.3 |
| 1.5 Guards over the rendered workflow | 1.4 | CREW | 1.3 |
| 1.6 Phase 1 gate: the controls behave in gardenio CI | gate | OPERATOR | 1.4, 1.5 |
| 2.1 `gl ci init` and `gl ci status` | 2.1 | CREW | 1.6 |
| 2.2 `gb lands ci render` | 2.2 (tool) | CREW | 1.6 |
| 2.3 The steward's CI onboarding duty and decision | 2.2 | CREW | 2.2 |
| 2.4 The digest's CI line (daily read) | 3.3, moved (D5) | CREW | 1.6 |
| 2.5 Phase 2 gate: crab-shack's first runs show in the digest | 2.3 + gate | PERSON + OPERATOR | 2.1, 2.3, 2.4 |
| 3.1 The Lands App: webhook and `check_suite`, in three places | 3.1 | CREW | 2.5 |
| 3.2 github-bridge: Lands webhooks onto step and project beads | 3.2 | CREW | 2.5 |
| 3.3 The steward's CI watch and quarantine duties, and the CI wake | 3.3 | CREW | 3.2 |
| 3.4 Fleet-wide CI cost read and the soft cap | §5, §5.1 (D10) | CREW | 2.5 |
| 3.5 Phase 3 gate: a red default-branch run wakes the steward | 3.1 (operator half) + gate | OPERATOR + PERSON | 3.1, 3.2, 3.3 |
| 4.1 The grant, the delegation and the playbook agree | 4.1 (first half) | CREW | 3.5 |
| 4.2 Merge-on-green is gated on the light check | 4.1 (second half) | CREW | 4.1 |
| 4.3 The kit's `release-cut.sh` | 4.2 (cut) | CREW | 3.5 |
| 4.4 The steward's release duty and `gl release cut` | 4.2 (ask) | CREW | 4.3 |
| 4.5 Phase 4 gate: one release on demand, one audited merge | gate | PERSON + OPERATOR | 4.2, 4.4 |

---

## Phase 0: fix what the kit depends on (gate: a tenant tag on crab-shack-3.5 is recorded as a release bead)

### Task 0.1: the provisioner's `project` step sets the land's release source and `release_flow=tag`

**Who:** CREW (one PR across `gardenio/controller`, `kbeads` and docs).

**Why:** A new land has no releases today. The provisioner sets no `release_tag_pattern`, and `gardenio/docs/lands/provisioner.md` ("### Releases", :44-60) says so. `gardenio/docs/releases-design.md:338` says the opposite: "`gb lands provision` sets the land's two defaults in its `project` step". The CI kit's release tier and the release recorder need the pattern. The CI onboarding spec also adds `release_flow` (`tag`, the default, or `branch`, a later opt-in for the truck driver). Plan: CI onboarding (`plan:ci-onboarding`), spec `gardenio/docs/superpowers/specs/2026-11-02-ci-onboarding-for-new-projects.md` §2.6, §2.7, §6 step 0.1.

**Files:**
- Modify `gardenio/controller/internal/lands/provision/provision.go`:
  - Next to `DefaultMaxLiveAgents` (:99), add:
    ```go
    // The release source a new land is born with (releases-design.md §4):
    // semver tags, one prod target, tags cut on demand (CI onboarding §2.7).
    DefaultReleaseTagPattern = `^v\d+\.\d+\.\d+$`
    DefaultReleaseFlow       = "tag"
    ```
  - In `stepProject` (:597-659), add three entries to the `fields` map:
    - `"release_tag_pattern": DefaultReleaseTagPattern`
    - `"release_targets": []map[string]string{{"name": "prod", "kind": "land"}}`
    - `"release_flow": DefaultReleaseFlow`
- Modify `gardenio/controller/internal/lands/provision/provision_test.go`. Add to the `want` map in `TestProvision_ApprovedApplicationBecomesALand` (:546-553):
  ```go
  "release_tag_pattern": `^v\d+\.\d+\.\d+$`,
  "release_targets":     `[{"kind":"land","name":"prod"}]`,
  "release_flow":        "tag",
  ```
  `json.Marshal` sorts map keys, so the targets string is exactly that.
- Modify `kbeads/internal/server/config.go`, the `type:project` builtin:
  - Add `{"name": "release_flow", "type": "enum", "values": ["tag", "branch"]}` after `release_branch` (:1261).
  - Extend the comment above it (:1181-1186) with one sentence: "release_flow is how a land's releases are cut: tag (on demand, the default) or branch (the truck driver's release branch, an opt-in not yet wired)."
- Modify `gardenio/controller/internal/bridge/init_configs.yaml`, the `type:project` list (:439-445): add `- {name: release_flow, type: enum, values: [tag, branch]}` after `release_branch`, and the same sentence in its comment.
- Modify `gardenio/controller/internal/beadsapi/projects.go`:
  - `ReleaseSource` (:280-284) gains `Flow string`.
  - `parseReleaseSource` (:292-304) sets `Flow` from `fields["release_flow"]`. Unset or any value other than `branch` reads as `tag`, so a value the reader does not know never turns on the branch flow.
  - Extend the `ReleaseSource` doc comment.
- Modify `gardenio/controller/internal/beadsapi/releases_test.go`. In `TestParseReleaseSource` (:63), add three cases: `release_flow` unset gives `Flow == "tag"`, `"branch"` gives `"branch"`, and `"bogus"` gives `"tag"`.
- Modify `kbeads/internal/server/config_driftguard_test.go`: add `"release_flow"` to `projectInfoReadFields` (:41-53). `TestProjectSchema_ParserDrift` fails until both the list and the builtin carry it.
- Modify the docs so they agree:
  - `gardenio/docs/lands/provisioner.md`, "### Releases" (:44-60): the `project` step now sets `release_tag_pattern` `^v\d+\.\d+\.\d+$`, `release_targets` `[{"name":"prod","kind":"land"}]` and `release_flow` `tag`. Keep the `kd update` example as how an admin changes them later, and add `-f release_flow=tag` to it.
  - `gardenio/docs/releases-design.md` §4 table (:332-336): add a `release_flow` row (enum `tag|branch`, default `tag`, gardenio: unset, a land: `tag`). The sentence at :338 is now true; leave it.
  - `gardenio/docs/onboarding-new-project-and-role.md` field table (:424): add a `release_flow` row.

**Steps:**
- [ ] Step 1: add the test expectations (provisioner want map, parse cases, drift list). Run the narrow tests below; they fail.
- [ ] Step 2: add the schema entries, the parser field and the provisioner fields. The tests pass.
- [ ] Step 3: edit the three docs. Then `grep -n 'does not\s*set them yet' gardenio/docs/lands/provisioner.md` prints nothing.
- [ ] Step 4: open the PR with `gb pr open`.

**Narrow test:**
```bash
cd gardenio/controller && go test ./internal/lands/provision/ -run 'TestProvision_ApprovedApplicationBecomesALand' -count=1
cd gardenio/controller && go test ./internal/beadsapi/ -run 'TestParseReleaseSource' -count=1
cd gardenio/controller && go test ./internal/bridge/ -run 'TestEnsureConfigs' -count=1
cd kbeads && go test ./internal/server/ -run 'TestProjectSchema_ParserDrift|Release' -count=1
```

**Acceptance:**
- The four commands above pass, and the PR's CI is green.
- `provisioner.md` and `releases-design.md` both say the `project` step sets the release defaults.
- If the daemon refuses the provisioner's create because of the new fields, the refusal names the rule. Fix it there; do not drop the fields.

**Rules for this task (from the plan's Global Constraints):**
- One PR, opened as a draft with `gb pr open`; run `gb pr ready` once when done. PRs get the light tier only.
- Never retrigger CI: no empty commits, no merging main in, no "Update branch", no `gh run rerun`, no draft/ready flips. A PR red only because main is red is not re-proved.
- Breaking main is not taboo. If main goes red after your merge, fix forward or add a row to `.github/quarantine.tsv` on main. Add no PR-time guard to prevent it.
- No bake-in periods: every check here is an event or evidence, never elapsed time.
- Tenant CI is GitHub-hosted only. Nothing here may route a tenant job to a `gardenio-k3s*` scale set or add a runner for a tenant.
- Cheap is on us: a kit default that burns minutes it does not need is a gardenio bug.
- Never modify a tenant repo. The Lands App gains no permission (no `workflows`, no `actions`).
- Test narrowly with the commands above. Never run `go test ./...` at the `gardenio/controller` or `kbeads` root.

### Task 0.2: the github-bridge reads a tenant's tags and checks with the Lands App token

**Who:** CREW (one PR: `gardenio/controller/cmd/github-bridge` plus the Helm chart).

**Why:**
- The bridge's tag poll is the only way a tenant release gets recorded, because a tenant repo sends gardenio no webhook (`cmd/github-bridge/release.go:1-19`). Today it mints every token from the one fleet App it is configured with:
  - `buildTokenSource` (`main.go:277-293`);
  - `tokenSource.Token` with `githubapp.BridgeTokenPermissions()` (`tokens.go:45-78`).
- The fleet App ("gardenio-io") is never installed on a tenant repo, so a tenant tag cannot be read.
- A tenant land's project bead carries `github_app=lands` and a pinned `github_installation_id`; the provisioner sets both. Its tokens must come from the Lands App with exactly `githubapp.LandsTokenPermissions()` (`internal/githubapp/identity.go:46-54`), and never from the fleet App (`identity.go:36-38`: "Callers must treat it as a refusal, never fall back to the other App").
- Phase 3 (Task 3.2) reuses the same per-project routing to read check runs.

Plan: CI onboarding, spec §6 step 0.2.

**Files:**
- Modify `gardenio/controller/cmd/github-bridge/tokens.go`:
  - `tokenSource` gains a `permissions map[string]string` field, set by `newTokenSource`. Existing callers pass `githubapp.BridgeTokenPermissions()`, so fleet behaviour is unchanged.
  - Add `type projectTokens struct { fleet, lands *tokenSourceFactory; operatorOrgs map[string]bool }`, or an equivalent you name, with `For(p releaseProject) (repoTokener, error)`:
    - `github_app` empty or `fleet`, on a project whose `land_kind` is not `tenant`: the fleet source.
    - `github_app=lands`: a Lands source pinned to the project's `github_installation_id`. Cache it per installation id; never look the installation up.
    - A tenant land (`land_kind=tenant`) with any `github_app` other than `lands`: refused.
    - A repo whose owner is in `LANDS_OPERATOR_GITHUB_ORGS`: refused (gd-72m3STiqya).
    - The Lands App not configured: refused with `githubapp.ErrNotConfigured`.
    - A refusal is a `Warn` once per poll per project. It never falls back to the other App.
- Modify `gardenio/controller/cmd/github-bridge/release.go`:
  - `releaseProject` (:140-144) gains `app string`, `installationID int64` and `landKind string`, copied from `beadsapi.ProjectInfo` (`GithubApp`, `GithubInstallationID`, `LandKind`) in `releaseProjectsList` (:149-175).
  - `recorderFor` (:207-210) builds its `releaserecord.GitHub` on a `githubCommenter` whose token source is `projectTokens.For(p)`. A refusal skips the project for this poll or delivery.
- Modify `gardenio/controller/cmd/github-bridge/main.go`:
  - Read the Lands App with `githubapp.ConfigFromEnv(githubapp.Lands, "LANDS_GITHUB_APP", false, nil, nil)`. When ok, build its client with `githubapp.New(cfg.Config)`; when nothing is set, it is nil (Lands off). A half-set config is a startup error, as for the fleet App.
  - Read `LANDS_OPERATOR_GITHUB_ORGS` (comma-separated).
  - Pass both to `newReleaseRecorderFromEnv` (:307-333).
  - Log `lands_app_configured` in the start line (:342-346).
- Create `gardenio/controller/cmd/github-bridge/projecttokens_test.go`, with fakes for `installationMinter`. It covers:
  - a fleet project gets fleet permissions;
  - a `lands` project gets exactly `LandsTokenPermissions` and the pinned installation id, and `InstallationFor` is never called;
  - a tenant land naming `fleet` is refused;
  - an operator-org repo is refused;
  - Lands unconfigured is refused with `ErrNotConfigured` and never served by the fleet minter;
  - the cache is per installation.
- Modify `gardenio/controller/cmd/github-bridge/release_test.go`: a tenant project's tag poll reads tags through the Lands source (assert which fake minted).
- Modify `gardenio/helm/gardenio/values.yaml`. Under `githubBridge` (after `githubApp`, about :15567), add:
  ```yaml
  # The Lands App (gardenio-lands), for reading a tenant land's tags and
  # checks with that land's pinned installation (plan ci-onboarding 0.2).
  # Empty appId = off: tenant lands are skipped, never served by the fleet App.
  landsApp:
    appId: ""
    apiUrl: ""
    privateKeySecret: gardenio-lands-github-app
    mountPath: /etc/gardenio/lands-github-app
  ```
  The Secret name and mount path are the ones the controller's `agents.lands.provisioner.githubApp` uses (values.yaml:3892-3893).
- Modify `gardenio/helm/gardenio/templates/github-bridge/deployment.yaml`. When `landsApp.appId` is set:
  - env `LANDS_GITHUB_APP_ID`, `LANDS_GITHUB_APP_PRIVATE_KEY_PATH` (`<mountPath>/private-key`) and an optional `LANDS_GITHUB_APP_API_URL`;
  - `LANDS_OPERATOR_GITHUB_ORGS` from `.Values.agents.lands.operatorGithubOrgs`, joined with commas, exactly as `templates/controller/deployment.yaml:384-398` does;
  - a read-only volume from the Secret.
- Modify `gardenio/helm/gardenio/tests/github_bridge_test.go`. Add `TestGithubBridge_LandsAppBlockOmittedWithoutAppID` and `TestGithubBridge_LandsAppMountsKeyAsFile`, modelled on `TestGithubBridge_AppBlockOmittedWithoutAppID` (:91): the key is a file, never an env value.

**Steps:**
- [ ] Step 1: write `projecttokens_test.go` and the release_test case; they fail.
- [ ] Step 2: implement the router and the wiring; the tests pass.
- [ ] Step 3: chart values, template and chart tests.
- [ ] Step 4: `gb pr open`.

**Narrow test:**
```bash
cd gardenio/controller && go test ./cmd/github-bridge/ -run 'TestProjectTokens|TestRelease|TestTokenSource' -count=1
cd gardenio/helm/gardenio/tests && GOWORK=off go test -count=1 -run 'TestGithubBridge' .
```

**Acceptance:**
- Tests are green in CI.
- With `landsApp.appId` empty, the rendered github-bridge Deployment is byte-identical to before (the chart test asserts it).
- No log line, error string or bead field contains a token or key.

**Rules for this task (from the plan's Global Constraints):**
- One PR, opened as a draft with `gb pr open`; run `gb pr ready` once when done. PRs get the light tier only.
- Never retrigger CI: no empty commits, no merging main in, no "Update branch", no `gh run rerun`, no draft/ready flips.
- Breaking main is not taboo: fix forward or quarantine. Add no PR-time guard.
- No bake-in periods.
- Tenant CI is GitHub-hosted only; nothing here adds a runner for a tenant.
- Cheap is on us.
- Never modify a tenant repo.
- The Lands App gains no permission. Its tokens request exactly `LandsTokenPermissions`.
- Test narrowly with the commands above. Never run `go test ./...` at the `gardenio/controller` root.

### Task 0.3: Phase 0 gate — a tag on crab-shack-3.5 is recorded as a release bead

**Who:** OPERATOR (no PR). The person, the owner acting as `fimbazland`, pushes the tag.

**Why:** Phase 1 opens only once a tenant tag is recorded (CI onboarding spec §6, Phase 0 gate). This proves the release source (Task 0.1) and the Lands-token tag poll (Task 0.2) end to end.

**Preconditions (outside this plan; not bead edges):**
- Escalation gd-AUdgkFSB1f is done: the github-bridge is enabled on k3s. This was spec step 0.3, an existing escalation.
- The crab-shack-3.5 land is provisioned (first-tenant gates task gd-bmsQ7FT3oj).
- Tasks 0.1 and 0.2 are merged and rolled in a release window.

**Steps:**
- [ ] 1. Set `githubBridge.landsApp.appId` in `deploy-values/gardenio-k3s.yaml` to the value of `agents.lands.provisioner.githubApp.appId` in the same file, through a normal PR (a deploy-values flip goes through CI). Roll it in the next release window.
- [ ] 2. Find the land's project bead: `kd list --type project --all-projects --json | jq -r '.[] | select(.fields.git_url | test("crab-shack-3.5")) | .id'`. If its `release_tag_pattern` is empty (the land was provisioned before Task 0.1 rolled), run:
  `kd update <id> -f release_tag_pattern='^v\d+\.\d+\.\d+$' -f release_targets='[{"name":"prod","kind":"land"}]' -f release_flow=tag`
- [ ] 3. Ask the owner, as the person `fimbazland`, to push tag `v0.0.1` on the default branch's tip of `fimbazland/crab-shack-3.5`, for example from `~crab/src/crab-shack-3.5`: `git tag v0.0.1 && git push origin v0.0.1`. The operator does not push to the tenant repo.
- [ ] 4. On the bridge's next tag poll (it runs every 5 minutes), check `kd release list --project <land>`. It lists `v0.0.1`. If it does not, read the bridge's `release:` log lines and fix the cause. Do not wait and re-check on a clock.
- [ ] 5. Comment on the plan epic: `laptop-claude: Phase 0 gate: <land> release v0.0.1 recorded as <release bead id> at <time>.` Close this task.

**Acceptance:** the release bead for `v0.0.1` exists on the crab-shack land, and the plan epic carries the evidence comment.

**Rules:**
- No bake-in: proceed on the evidence.
- Never modify the tenant repo yourself; the person pushes the tag.
- No local Go tests, builds or Terraform on the laptop.
- Name the AWS profile and region of any AWS command, and say what it changed.

---

## Phase 1: the kit in gardenio (gate: the fixtures' negative controls go red and the positive ones go green in gardenio CI)

### Task 1.1: `changed-units.sh`, the ci.toml reader, and the `lands-ci-kit` job

**Who:** CREW (one PR).

**Why:** The CI kit (CI onboarding spec §2.1, §3.1) runs only the units a change touches.
- A unit is a Go module, an npm workspace member, a Python project or a Cargo crate, named in the tenant repo's `.gardenio/ci.toml`, which the steward writes and the person reviews.
- CI does not guess at run time. A wrong guess shows up in ci.toml and is fixed in one line.

This task creates the reader and the selector, plus the gardenio CI job that tests the kit. Everything under `lands/ci/kit/` is later copied verbatim into a tenant repo's `.gardenio/ci/`. So the scripts must work on GitHub's `ubuntu-latest` image (bash, git, Python 3.12) with nothing else installed.

**Files:**
- Create `lands/ci/README.md`. In two short paragraphs, say what the kit is, that `lands/ci/kit/` is vendored to `.gardenio/ci/`, and that tenant CI runs on GitHub-hosted runners only.
- Create `lands/ci/kit/citoml.py`: Python 3.11+ stdlib only (`tomllib`, `argparse`, `fnmatch`, `json`, `sys`). Run it as `python3 -I`.
- Create `lands/ci/kit/changed-units.sh` (bash, `set -euo pipefail`).
- Create `lands/ci/changed-units_test.sh` (bash; temp git repos; no network).
- Create `lands/ci/testdata/citoml/*.toml`: one valid file and one invalid file per rule below. Task 1.3's Go parser reads the same files, so the two parsers cannot drift.
- Modify `.github/workflows/checks.yml` to add the job `lands-ci-kit`, wired as below.

**The ci.toml format** (`version = 1`):
```toml
version = 1
default_branch = "main"
daily = true                 # false: the daily run exits 0 at once
widen = []                   # extra repo-root globs that select every unit

[toolchains]                 # optional; empty = the runner's preinstalled tool
go = ""                      # empty with a go unit = the unit's go.mod
node = "22"
python = "3.12"
rust = "1.93.0"

[[unit]]
name = "api"                 # ^[a-z0-9][a-z0-9-]{0,39}$, unique
path = "services/api"        # repo-relative; "." allowed; no "..", no leading "/"
kind = "go"                  # go | node | python | rust
build = "go build ./..."
lint = "go vet $GARDENIO_CI_PKGS"
test = 'go test $GARDENIO_CI_PKGS "$@"'
release = ""                 # empty = no release step
slow = false                 # true: daily run, and a release when its paths changed
```
Validation:
- `version` must be 1. Unit names must be unique and match the pattern above. `path` is relative, without `..`.
- `kind` must be one of the four.
- An unknown key at any level is an error naming the key.
- Any violation exits 2 with `ci.toml: <what>`.

**`citoml.py` subcommands:**
- `citoml.py units [--toml F]` prints one TSV line per unit: `name<TAB>path<TAB>kind<TAB>slow`.
- `citoml.py get <unit> <build|lint|test|release>` prints the command, or nothing when it is empty.
- `citoml.py widen` prints the root-level globs that select every unit:
  - the `widen` list;
  - for each kind present: go `go.work`, `go.work.sum`, `go.sum`; node `package.json`, `package-lock.json`, `npm-shrinkwrap.json`, `pnpm-lock.yaml`, `yarn.lock`; python `uv.lock`, `poetry.lock`, `requirements*.txt`; rust `Cargo.lock`, `Cargo.toml`.

  These are repo-root paths only. A unit's own lockfile selects just that unit, through the nearest-unit walk.
- `citoml.py toolchains` prints `KEY=VALUE` lines for `$GITHUB_OUTPUT`. Each key is printed only when a unit of that kind exists:
  - `go`: the pin;
  - `go_version_file`: the first go unit's `go.mod`, printed when the go pin is empty;
  - `node`;
  - `node_cache=npm`: when a `package-lock.json` exists at any node unit root;
  - `python`;
  - `rust`.
- `citoml.py daily` prints `true` or `false`.

**`changed-units.sh --base <rev> --head <rev> [--toml F] [--go-pkgs <unit>]`:**
1. List the changed files: `git diff --name-only <base>...<head>`. If `--base` is empty, all zeros, or the diff fails, print every unit and emit `::notice::gardenio-ci: no usable base (<why>); selecting every unit`. The gate fails open.
2. A changed file under `.gardenio/`, or `.github/workflows/gardenio-ci.yml`, selects every unit.
3. A changed file matching a `citoml.py widen` glob selects every unit.
4. Otherwise each file selects the unit whose `path` is the longest directory prefix of it. A unit at `.` takes whatever no longer path does. A file under no unit selects nothing; the smoke checks still run (Task 1.3).
5. Print the selected names, one per line, in ci.toml order.
6. With `--go-pkgs <unit>`, print instead the changed Go package directories of that unit, relative to its root, as `./a ./b`:
   - a package is the nearest directory holding a `*.go` file at `<head>`;
   - a changed `go.mod` or `go.sum` in the unit prints `./...`;
   - deleted packages are skipped;
   - print nothing when no package changed.

**The `lands-ci-kit` job in `checks.yml`:**
- Model it on `script-selftests` (`checks.yml:771`). It is path-gated, so it carries no run-wide supersession conjunct (see that job's header comment).
- In the `changes` job (:267):
  - add an output `lands_ci: ${{ steps.gate.outputs.lands_ci }}`;
  - add a dorny filter key `lands_ci` with `lands/ci/**`, `scripts/quarantine.sh` and `scripts/check-main-concurrency.py`;
  - in the `gate` step, add the `F_LANDS_CI` variable, the fail-open `ALL` arm, a case `lands-ci-kit) q=true ;;` in the job-block map, and `printf 'lands_ci=%s\n' "$q" >> "$GITHUB_OUTPUT"`.
- The job's `if:` is the `script-selftests` one, with `needs.changes.outputs.lands_ci`.
- The job runs on `runs-on: gardenio-k3s-light` with `timeout-minutes: 15`. Its steps are checkout, then `bash lands/ci/changed-units_test.sh`.
- Add `lands-ci-kit` to the `needs:` of the required `checks` job (:2513).
- Add `lands/ci/kit/changed-units.sh lands/ci/changed-units_test.sh` to the shellcheck line in `script-selftests` (:837).
- The repo's own guards must pass: `scripts/check-ci-draft-gate.py`, `scripts/check-ci-required-checks.py`, `scripts/check-ci-path-gates.py` and `scripts/check-main-concurrency.py`.

**`changed-units_test.sh` cases.** Each case builds a temp repo with `git init`:
- Every invalid `testdata/citoml/*.toml` exits 2; the valid one exits 0.
- Nested units `.` and `services/api`: a change in `services/api/x.go` selects only `api`, and a change in `README.md` selects only the root unit. **This is the positive control: the gate can say "not this unit".**
- A root `go.sum`, `Cargo.lock` or `package-lock.json` change selects every unit of a repo with that kind.
- A `.gardenio/ci/run.sh` change selects every unit.
- An empty base selects every unit and prints the notice.
- A file under no unit selects nothing.
- `--go-pkgs`: two changed packages print both; a `go.mod` change prints `./...`; a docs-only change prints nothing.

**Steps:**
- [ ] Step 1: write the testdata and `changed-units_test.sh`; run it; it fails.
- [ ] Step 2: write `citoml.py` and `changed-units.sh`; the test passes.
- [ ] Step 3: wire the job; run the four guards in Narrow test.
- [ ] Step 4: `gb pr open`.

**Narrow test:**
```bash
bash lands/ci/changed-units_test.sh
shellcheck -x lands/ci/kit/changed-units.sh lands/ci/changed-units_test.sh
python3 scripts/check-ci-draft-gate.py && python3 scripts/check-ci-required-checks.py
python3 scripts/check-ci-path-gates.py && python3 -S -E scripts/check-main-concurrency.py
```

**Acceptance:**
- The test and the four guards pass.
- The PR's CI is green, and on main `lands-ci-kit` runs and passes.

**Rules for this task (from the plan's Global Constraints):**
- One PR, opened as a draft with `gb pr open`; run `gb pr ready` once when done. PRs get the light tier only.
- Never retrigger CI: no empty commits, no merging main in, no "Update branch", no `gh run rerun`, no draft/ready flips.
- Breaking main is not taboo: fix forward or quarantine. Add no PR-time guard.
- No bake-in periods.
- Tenant CI runs on GitHub-hosted runners only. The kit you write must run on `ubuntu-latest`. The `gardenio-k3s-light` runner above runs gardenio's own test of the kit, never a tenant's job.
- Cheap is on us: the selector is the cost policy. When unsure, select fewer units, except where a rule above says to fail open.
- Never modify a tenant repo. The Lands App gains no permission.
- Test narrowly with the commands above.

### Task 1.2: generalise `quarantine.sh`: repo and branch as inputs, the `gardenio/quarantine` union, per-ecosystem kinds

**Who:** CREW (one PR).

**Why:** gardenio's quarantine list (`.github/quarantine.tsv`, policy in `.github/known-failures.md`, "Quarantine") is the owner's rule of 2026-11-01: a test that has blocked one PR, or failed on main, never blocks a second PR.
- Today `scripts/quarantine.sh` reads main's copy and knows only `go` and `golden` rows.
- The CI kit vendors the same script into every tenant repo. There it must also read a steward-owned branch `gardenio/quarantine` holding only that file (CI onboarding spec §2.2, decision Q3). A tenant steward under the default `ask` grant cannot push to the default branch, but the Lands App can push that branch with `contents: write` today.
- It must also learn the `pytest`, `jest`, `vitest` and `cargo` kinds.
- gardenio's own CI keeps calling `scripts/quarantine.sh` unchanged.

**Files:**
- Create `lands/ci/kit/quarantine.sh`, the generalised script. Start from today's `scripts/quarantine.sh` (70 lines) and keep its `fetch`, `regex` and `rows` behaviour exactly.
- Replace `scripts/quarantine.sh` with a wrapper:
  ```bash
  #!/usr/bin/env bash
  # quarantine.sh -- gardenio's entry point to the CI kit's quarantine script
  # (lands/ci/kit/quarantine.sh). Defaults read main's copy only, as before.
  set -euo pipefail
  exec "$(cd "$(dirname "$0")" && pwd)/../lands/ci/kit/quarantine.sh" "$@"
  ```
- Modify `scripts/quarantine_test.sh`. Every existing case must still pass through the wrapper; add the cases below.
- Modify `.github/known-failures.md`, "Quarantine": one paragraph on the new kinds and on the union branch tenant repos use. gardenio itself still reads main only.
- Modify `.github/workflows/checks.yml`:
  - add `lands/ci/kit/quarantine.sh` to the shellcheck line in `script-selftests` (:837);
  - add `'lands/ci/kit/quarantine.sh'` to the `scripts` path filter (:414-417), so a change to it reruns `scripts/quarantine_test.sh`.
- `.github/workflows/ci.yml` is NOT modified. Its calls `scripts/quarantine.sh fetch`, `regex go` and `rows` (:1146-1168, :1510-1548) keep working through the wrapper.

**Inputs** (environment, so gardenio's call sites need no flags):
- `QUARANTINE_REMOTE`, default `origin`;
- `QUARANTINE_BRANCH`, default `main`;
- `QUARANTINE_UNION`, default empty.

`fetch OUT`:
- Read `<remote>/<branch>:.github/quarantine.tsv`, falling back to the checkout's copy with today's warning.
- When `QUARANTINE_UNION` is set, also run `git fetch -q --depth=1 <remote> <union>` and append that copy's non-comment rows.
- A missing union branch is a `::notice::` (it is absent until the first row), never an error.
- Rows are de-duplicated by (kind, name).

**Row validation.** A malformed row is dropped with a warning, never obeyed, because a name reaches a test runner's flags:

| kind | name pattern | skip args (`skip-args`) | only-quarantined args (`only-args`) |
|---|---|---|---|
| `go` | `^Test[A-Za-z0-9_]+$` | `-skip` `^(A\|B)$` | `-run` `^(A\|B)$` |
| `golden` | `^[a-z0-9]+(-[a-z0-9]+)*\.png$` | (unchanged: `GARDENIO_QUARANTINE`) | — |
| `pytest` | `^[A-Za-z0-9_./-]+\.py::[A-Za-z_][A-Za-z0-9_]*(::[A-Za-z_][A-Za-z0-9_]*)?$` | `--deselect` `<id>` per row | `<id>` per row |
| `jest`, `vitest` | `^[A-Za-z0-9_./-]+\.(js\|jsx\|ts\|tsx\|mjs\|cjs)::[A-Za-z0-9 _.,:'-]{1,200}$` | `--testNamePattern` `^(?!(?:A\|B)$).*` | `--testNamePattern` `^(?:A\|B)$` |
| `cargo` | `^[a-z_][a-z0-9_]*(::[a-z_][a-z0-9_]*)*$` | `--exact` then `--skip` `<name>` per row | `--exact` then `<name>` per row |

Rules for the table:
- The bead column keeps today's pattern, `^[a-z]+-[A-Za-z0-9]+$`. Expiry stays a warning, never a gate.
- pytest, jest and vitest names are repo-relative paths. `skip-args KIND F UNIT_PATH` and `only-args KIND F UNIT_PATH` keep only the rows under `UNIT_PATH/` (all rows when it is `.`) and strip that prefix.
- For jest and vitest, the test-name part (after `::`) is what the pattern matches. The file part is for people.
- jest and vitest names allow no regex metacharacters, so no escaping is needed.
- go and cargo names apply to every unit of that kind.
- Both subcommands print one argument per line and nothing when no row applies. `run.sh` (Task 1.3) reads them with `mapfile -t`.

**New cases in `scripts/quarantine_test.sh`:**
- The origin has a `gardenio/quarantine` branch with one `pytest` row. With `QUARANTINE_UNION=gardenio/quarantine`, `fetch` keeps both main's and the branch's rows.
- A missing union branch gives main's rows only, plus the notice.
- One good row per new kind survives, **and appears in `skip-args`. This is the positive control: the validator does not drop everything.**
- One bad row per kind is dropped: `tests/a.py::test_[x]`, a jest name with `(`, an uppercase cargo path `Foo::bar`, a `../` path.
- Unit scoping: `services/api/tests/test_a.py::test_b` gives `--deselect tests/test_a.py::test_b` for unit `services/api`, and nothing for unit `web`.
- `only-args` for each kind.

**Steps:**
- [ ] Step 1: add the new test cases; run the test; they fail.
- [ ] Step 2: write `lands/ci/kit/quarantine.sh` and the wrapper; every case passes.
- [ ] Step 3: shellcheck, the docs paragraph, the checks.yml lines.
- [ ] Step 4: `gb pr open`.

**Narrow test:**
```bash
bash scripts/quarantine_test.sh
shellcheck -x lands/ci/kit/quarantine.sh scripts/quarantine.sh scripts/quarantine_test.sh
python3 scripts/check-ci-path-gates.py
```

**Acceptance:**
- The test passes in CI, and `git diff origin/main -- .github/workflows/ci.yml` is empty.
- On the first main run after merge, ci.yml's quarantine steps print `quarantine: main's .github/quarantine.tsv`, as before.

**Rules for this task (from the plan's Global Constraints):**
- One PR, opened as a draft with `gb pr open`; run `gb pr ready` once when done. PRs get the light tier only.
- Never retrigger CI: no empty commits, no merging main in, no "Update branch", no `gh run rerun`, no draft/ready flips.
- Breaking main is not taboo. If gardenio's quarantine reading breaks main, fix forward. Add no PR-time guard.
- No bake-in periods: an expired row is a nag, never a gate.
- Tenant CI is GitHub-hosted only.
- Cheap is on us.
- Never modify a tenant repo. The `gardenio/quarantine` branch is written only by a land's steward (Task 3.3). The Lands App gains no permission.
- Test narrowly with the commands above.

### Task 1.3: the caller workflow template, the runner scripts, the setup action, the starters and the Go renderer

**Who:** CREW (one PR; creates the `gardenio/lands/ci` Go module).

**Why:** This is the kit itself (CI onboarding spec §2.1, §3.2, §3.3). It has two halves:
- **The caller,** `.github/workflows/gardenio-ci.yml`, which the person commits once. The Lands App has no `workflows` permission and gets none, so gardenio can never update this file. It therefore has to be static and complete from day one.
- **Everything else** lives in `.gardenio/`, which the steward updates by PR:
  - the tier runner;
  - the selector;
  - the quarantine script;
  - the smoke checks;
  - the toolchain setup, as a composite action at `.gardenio/ci/setup` (any repo path works with `uses: ./path`);
  - the daily-issue script;
  - `ci.toml`.

One Go renderer embeds every file, so `gl ci init` (Task 2.1) and `gb lands ci render` (Task 2.2) cannot render different kits. Tenant CI runs on GitHub-hosted `ubuntu-latest` runners only (owner decision gd-oQAIXhuIm9).

**Tiers** (spec §2.1):
- **light:** `pull_request` opened, synchronize or reopened. Smoke, then the changed units' build, lint and test. Budget 10 minutes, `timeout-minutes: 15`.
- **full:** a push to the default branch, or dispatch `tier=full`. Smoke over the pushed range, then every non-slow unit. `timeout-minutes: 45`.
- **daily:** a schedule at a per-land staggered minute. Everything, including `slow` units. `timeout-minutes: 45`. A red run opens or updates the issue "Daily full CI run: red".
- **release:** a tag push matching the land's glob, or dispatch `tier=release`. The units changed since the previous tag, plus their `release` command. `timeout-minutes: 45`.

**Files (`lands/ci/`):**
- `go.mod`: `module gardenio/lands/ci`, `go 1.25.12`, `require github.com/BurntSushi/toml v1.6.0` (the version `gardenio/controller/go.mod` uses); plus `go.sum`.
- `kit.go`, package `kit`:
  - `//go:embed kit template starters`.
  - `type Config` and `type Unit`, mirroring ci.toml (Task 1.1's format) with toml tags.
  - `Parse([]byte) (*Config, error)`, with exactly `citoml.py`'s validation. `kit_test.go` runs both over `testdata/citoml/*.toml` and requires the same verdict for each file.
  - `Detect(fsys fs.FS, defaultBranch string) (*Config, error)`:
    - It skips `.git`, `node_modules`, `vendor`, `target`, `testdata` and `.gardenio`.
    - A unit is a directory with `go.mod`; with `package.json`, unless it only declares `workspaces` and has no `scripts.test`; with `pyproject.toml`, `setup.cfg` or `setup.py`; or with a `Cargo.toml` holding `[package]`. A `[workspace]`-only root is not a unit.
    - Commands and toolchain pins come from the kind's starter. `{crate}` and `{name}` are replaced with the package name.
    - No manifest gives zero units, which is smoke only (spec §3.3).
  - `type Params struct { Land, DefaultBranch, TagGlob string }`.
  - `Render(cfg *Config, p Params) (map[string][]byte, error)` returns every kit file by repo path:
    - `.github/workflows/gardenio-ci.yml`;
    - `.gardenio/ci.toml`;
    - `.gardenio/ci/{run.sh,changed-units.sh,citoml.py,quarantine.sh,smoke.sh,daily-issue.sh,VERSION}`;
    - `.gardenio/ci/setup/action.yml`;
    - `.github/quarantine.tsv`, with the header only.
  - `AppWritable(path string) bool` is false only under `.github/workflows/`.
  - `DailyCron(land string) string`: with `h := fnv32a(land)`, the minute is `h % 60` and the hour is `(h / 60) % 24`.
  - `TagGlob(pattern string) (string, error)`: `^v\d+\.\d+\.\d+$` gives `v*.*.*` and `^20\d{2}\.\d{1,3}\.\d+$` gives `20*.*.*`. Anything else is an error asking for an explicit glob.
  - `Version`: the first 12 hex characters of the sha256 over every embedded file, sorted by path. It is written to `.gardenio/ci/VERSION` and into the caller's header comment.
- `kit/`: the files vendored to `.gardenio/ci/`. `changed-units.sh` and `citoml.py` come from Task 1.1 and `quarantine.sh` from Task 1.2. Add `run.sh`, `smoke.sh`, `daily-issue.sh` and `setup/action.yml`, below.
- `template/gardenio-ci.yml.tmpl`: a Go `text/template` with delimiters `[[` `]]`, so GitHub's `${{ }}` passes through untouched.
- `starters/{go,node,python,rust,none}.toml`, below.
- `cmd/gardenio-ci-render/main.go`: `gardenio-ci-render --dir D --land L --default-branch B --tag-glob G`. It writes `Render`'s output into D; an existing `.gardenio/ci.toml` is parsed, otherwise it runs `Detect` on D. Tasks 1.4 and 1.5 use it.
- `kit_test.go`:
  - the Parse parity check;
  - Detect over `testdata/detect/{gomod,npmws,pyproject,cargows,none}/`;
  - Render's exact file set;
  - `AppWritable`;
  - a `TagGlob` table;
  - `DailyCron` is stable for a fixed land;
  - a golden of the rendered caller for land `fixture`, branch `main` and glob `v*.*.*` at `testdata/golden/gardenio-ci.yml`, refreshed with `-update`.
- Register the module:
  - add `lands/ci` to `OUT_OF_WORKSPACE` in `scripts/check-go-module-coverage.sh` (:73) with target `test-lands-ci`;
  - add a root `Makefile` target `test-lands-ci:` whose recipe is `cd lands/ci && GOWORK=off go test -count=1 ./...`, listed in `test-out-of-workspace` (:77) and `.PHONY`;
  - add a step `GOWORK=off go test -count=1 ./...` (working directory `lands/ci`, after `actions/setup-go@v5` with `go-version: "${{ env.GO_VERSION }}"` and `cache: false`) to the `lands-ci-kit` job in `.github/workflows/checks.yml`.

**The caller template** (render it exactly; `[[.Version]]`, `[[.DefaultBranch]]`, `[[.TagGlob]]` and `[[.Cron]]` are filled in):
```yaml
# gardenio-ci, from gardenio's CI kit [[.Version]]. Commit it once: everything it
# runs lives in .gardenio/, which your land's steward keeps current by pull
# request. GitHub-hosted runners only; your repo's account pays its minutes.
name: gardenio-ci
on:
  pull_request:
    types: [opened, synchronize, reopened]
  push:
    branches: ['[[.DefaultBranch]]']
    tags: ['[[.TagGlob]]']
  schedule:
    - cron: '[[.Cron]]'
  workflow_dispatch:
    inputs:
      tier: {type: choice, options: [full, release], default: full}
permissions:
  contents: read
concurrency:
  group: ${{ github.event_name == 'pull_request' && format('gardenio-ci-pr-{0}', github.ref) || (github.event_name == 'push' && github.ref_type == 'branch') && format('gardenio-ci-{0}', github.ref) || format('gardenio-ci-run-{0}', github.run_id) }}
  cancel-in-progress: ${{ github.event_name == 'pull_request' }}
jobs:
  light:
    name: gardenio-ci/light
    if: github.event_name == 'pull_request'
    runs-on: ubuntu-latest
    timeout-minutes: 15
    steps:
      - uses: actions/checkout@v4
        with: {fetch-depth: 0, filter: 'blob:none'}
      - uses: ./.gardenio/ci/setup
      - run: .gardenio/ci/run.sh light
  full:
    name: gardenio-ci/full
    if: (github.event_name == 'push' && github.ref_type == 'branch') || (github.event_name == 'workflow_dispatch' && inputs.tier == 'full')
    runs-on: ubuntu-latest
    timeout-minutes: 45
    steps:
      - uses: actions/checkout@v4
        with: {fetch-depth: 0, filter: 'blob:none'}
      - uses: ./.gardenio/ci/setup
      - run: .gardenio/ci/run.sh full
        env: {GARDENIO_CI_BEFORE: '${{ github.event.before }}'}
  daily:
    name: gardenio-ci/daily
    if: github.event_name == 'schedule'
    runs-on: ubuntu-latest
    timeout-minutes: 45
    steps:
      - uses: actions/checkout@v4
      - uses: ./.gardenio/ci/setup
      - run: .gardenio/ci/run.sh daily
  daily-issue:
    name: gardenio-ci/daily-issue
    needs: daily
    if: always() && github.event_name == 'schedule' && needs.daily.result != 'skipped'
    runs-on: ubuntu-latest
    timeout-minutes: 5
    permissions: {contents: read, issues: write}
    steps:
      - uses: actions/checkout@v4
        with: {sparse-checkout: .gardenio/ci}
      - run: .gardenio/ci/daily-issue.sh '${{ needs.daily.result }}'
        env: {GH_TOKEN: '${{ github.token }}'}
  release:
    name: gardenio-ci/release
    if: (github.event_name == 'push' && github.ref_type == 'tag') || (github.event_name == 'workflow_dispatch' && inputs.tier == 'release')
    runs-on: ubuntu-latest
    timeout-minutes: 45
    steps:
      - uses: actions/checkout@v4
        with: {fetch-depth: 0, fetch-tags: true}
      - uses: ./.gardenio/ci/setup
      - run: .gardenio/ci/run.sh release
```

**`kit/setup/action.yml`** (composite, `name: gardenio-ci setup`):
- Step `id: tc`, `shell: bash`: `python3 -I .gardenio/ci/citoml.py toolchains >> "$GITHUB_OUTPUT"`.
- `actions/setup-go@v5`, only when `steps.tc.outputs.go != '' || steps.tc.outputs.go_version_file != ''`, with `go-version`, `go-version-file` and `cache-dependency-path: '**/go.sum'`.
- `actions/setup-node@v4`, only when `steps.tc.outputs.node != ''`, with `node-version`, plus `cache: npm` and `cache-dependency-path: '**/package-lock.json'` when `node_cache == 'npm'`.
- `actions/setup-python@v5`, only when `steps.tc.outputs.python != ''`, with `python-version` and `cache: pip`.
- Rust, only when `steps.tc.outputs.rust != ''`: `rustup toolchain install "$V" --profile minimal -c clippy && rustup default "$V"`, then `actions/cache@v4` on `~/.cargo/registry`, `~/.cargo/git` and `target`, keyed by `rust-$V-${{ hashFiles('**/Cargo.lock') }}`.

**`kit/run.sh <light|full|daily|release>`** (bash, `set -euo pipefail`; it reads ci.toml with `citoml.py` and never guesses):
- **The range.** For tests, `GARDENIO_CI_BASE` and `GARDENIO_CI_HEAD` override it. Otherwise:
  - light: `git merge-base origin/$GITHUB_BASE_REF HEAD` .. `HEAD`;
  - full: `$GARDENIO_CI_BEFORE` .. `HEAD` (empty or zeros means the selector fails open);
  - daily: every unit, including slow ones;
  - release: the previous tag that matches the caller's glob (`git describe --tags --abbrev=0 --match '<glob>' HEAD^`) .. `HEAD`. With no previous tag, every unit.
- **Smoke:** `smoke.sh <base> <head>` runs first in light and full. In daily it runs with `--all`. Release has no smoke.
- **Units:**
  - light and full take `changed-units.sh` output minus `slow` units; daily takes all units; release takes the changed units, so a slow unit runs only when its paths changed.
  - For a go unit, `GARDENIO_CI_PKGS` is `changed-units.sh --go-pkgs` in light and `./...` otherwise. In light, an empty package list skips that unit's lint and test with a `::notice::`.
- **Each unit, with its directory as the working directory:**
  - Run `build`, then `lint` (no lint in release).
  - Then `test` as `bash -c "<test>" gardenio-ci "${skip[@]}"`, where `skip` is `quarantine.sh skip-args <kind> <list> <path>` and `<list>` is `quarantine.sh fetch` with `QUARANTINE_BRANCH=<default_branch>` and `QUARANTINE_UNION=gardenio/quarantine`.
  - If the unit has quarantine rows, run the advisory pass `bash -c "<test>" gardenio-ci "${only[@]}" || true`. When it fails, emit `::warning title=gardenio-ci quarantined::<unit>: quarantined tests still fail`.
  - In release only, run `release` when it is non-empty, with `GARDENIO_RELEASE_TAG` and `GARDENIO_RELEASE_SHA` set.
  - Tee every test output to `$RUNNER_TEMP/<unit>-test.log`.
- **Failure names.** For each failing test found in a log (go `^--- FAIL: (Test\w+)`, pytest `^FAILED (\S+::\S+)`, cargo `^test (\S+) \.\.\. FAILED$`; jest and vitest report "see log"), emit `::error title=gardenio-ci failing test::<kind> <repo-relative name>`, at most 20 per job. Every failed step also emits `::error title=gardenio-ci <unit> <step>::exit <rc>`.
- **Exit code:** non-zero when any build, lint, test or release step failed. The advisory pass never changes it.
- **Daily off:** when `citoml.py daily` is `false`, `run.sh daily` prints a notice and exits 0.
- **Summary:** one table row per unit and step goes to `$GITHUB_STEP_SUMMARY`.

**`kit/smoke.sh <base> <head> | --all`** (under a minute on crab-shack-sized repos):
- **gitleaks 8.30.1:**
  - Download `https://github.com/gitleaks/gitleaks/releases/download/v8.30.1/gitleaks_8.30.1_linux_x64.tar.gz` and verify sha256 `551f6fc83ea457d62a0d98237cbad105af8d557003051f41f3e7ca7b3f2470eb`. A mismatch fails the job. Cache it under `$RUNNER_TOOL_CACHE/gardenio-ci/`.
  - Run `gitleaks git --no-banner --redact --log-opts="<base>..<head>" .`, or `--log-opts="-1 <head>"` when base is empty or zeros. With `--all`, gitleaks is skipped (history scans are not cheap).
- **Syntax checks** over changed files that still exist, or every tracked file with `--all`:
  - `node --check` for `.js` and `.mjs`;
  - `python3 -m py_compile` for `.py`;
  - `bash -n` for `.sh` files with a bash shebang, `sh -n` for the rest;
  - `shellcheck -S warning` for `.sh`.

  A missing `node` or `shellcheck` is a `::notice::`, not a failure.

**`kit/daily-issue.sh <result>`:**
- It uses `gh` with `GH_TOKEN` and finds the one open issue whose title is exactly `Daily full CI run: red`.
- `failure`: comment `red at <sha7>: <run url>` on it, or create it with that body and try `gh issue pin` (a pin failure is a warning).
- `success`: if the issue exists, comment `green again at <sha7>` and close it.
- Anything else: do nothing.

**Starters** (pins and default commands; `"$@"` receives the quarantine args):
- `go.toml`:
  - `[toolchains] go = ""` (use the unit's go.mod);
  - build `go build ./...`, lint `go vet $GARDENIO_CI_PKGS`, test `go test $GARDENIO_CI_PKGS "$@"`.
- `node.toml`:
  - `node = "22"`;
  - build `npm ci`, lint `npm run --if-present lint`, test `npm test --if-present -- "$@"`.
- `python.toml`:
  - `python = "3.12"`;
  - build `python -m pip install --quiet -e '.[test]' ruff==0.8.4 pytest==8.3.4`, lint `ruff check .`, test `pytest "$@"`.
- `rust.toml`:
  - `rust = "1.93.0"`;
  - build `cargo build -p {crate}`, lint `cargo clippy -p {crate} -- -D warnings`, test `cargo test -p {crate} -- "$@"`.
- `none.toml`: no units; a comment saying smoke only.

A polyglot repo concatenates the units.

**Steps:**
- [ ] Step 1: write `kit_test.go` and the golden. Run `GOWORK=off go test ./...` in `lands/ci`; it fails.
- [ ] Step 2: write `kit.go`, the template, the scripts, the action and the starters; refresh the golden with `-update`; read it line by line against the template above.
- [ ] Step 3: register the module (coverage guard, Makefile, checks.yml step).
- [ ] Step 4: `gb pr open`.

**Narrow test:**
```bash
cd lands/ci && GOWORK=off go test -count=1 ./...
bash scripts/check-go-module-coverage.sh
shellcheck -x lands/ci/kit/run.sh lands/ci/kit/smoke.sh lands/ci/kit/daily-issue.sh
python3 scripts/check-ci-draft-gate.py && python3 scripts/check-ci-required-checks.py
```

**Acceptance:**
- The kit's Go tests pass in `lands-ci-kit`.
- The rendered caller equals the golden.
- Every job in the template has `runs-on: ubuntu-latest`.
- `Render`'s file list has exactly one path under `.github/workflows/`.

**Rules for this task (from the plan's Global Constraints):**
- One PR, opened as a draft with `gb pr open`; run `gb pr ready` once when done. PRs get the light tier only.
- Never retrigger CI: no empty commits, no merging main in, no "Update branch", no `gh run rerun`, no draft/ready flips.
- Breaking main is not taboo: fix forward or quarantine. Add no PR-time guard.
- No bake-in periods.
- Tenant CI runs on GitHub-hosted runners only: every rendered job is `ubuntu-latest`, and no `gardenio-k3s*` or `self-hosted` label may appear in anything under `lands/ci/kit` or `lands/ci/template`.
- Cheap is on us: the light tier stays changed-units-only, PR runs cancel in progress, there is no matrix, and the timeouts are as given.
- Never modify a tenant repo. The Lands App gains no permission; the caller is the one file it can never write.
- Test narrowly with the commands above.

### Task 1.4: fixture repos for every starter, run as positive and negative controls

**Who:** CREW (one PR).

**Why:** The Phase 1 gate (CI onboarding spec §6) is "the fixtures' negative controls go red and the positive ones go green in gardenio CI". Each fixture is a tiny tenant repo with:
- a passing unit;
- a failing unit;
- a quarantined failing test.

Each is run through the real kit (`run.sh`, `changed-units.sh`, `quarantine.sh`, all from Tasks 1.1-1.3) in the `lands-ci-kit` job. So "green" and "red" are proven to mean what the steward and the digest will say they mean. The kit renderer is `go run ./cmd/gardenio-ci-render` in `lands/ci`.

**Files:**
- Create `lands/ci/testdata/fixtures/{go,node,python,rust,none}/`. Each is a tree with its own `.gardenio/ci.toml` and `.github/quarantine.tsv`:
  - **go:**
    - units `ok` (`units/ok/go.mod` module `ok`; `TestOK` passes, `TestQuarantined` fails) and `broken` (`TestBroken` fails);
    - quarantine row `go<TAB>TestQuarantined<TAB>fx-fixture1<TAB>2026-11-02<TAB>2099-01-01<TAB>fixture`.
  - **python:**
    - units `ok` (`tests/test_ok.py::test_ok` passes, `tests/test_ok.py::test_quarantined` fails) and `broken`;
    - row `pytest  units/ok/tests/test_ok.py::test_quarantined …`;
    - its ci.toml sets `build = "true"`, because the job installs pytest and ruff once (the fixture tests tiering and quarantine, not pip).
  - **node:**
    - units `ok` and `broken`, each with `package.json` devDependency `vitest` `2.1.8`, an exact pin with a committed `package-lock.json`, and `"test": "vitest run"`;
    - row `vitest  units/ok/ok.test.mjs::quarantined fails`.
  - **rust:**
    - crates `ok` (`tests::ok` passes, `tests::quarantined` fails) and `broken`, in one Cargo workspace;
    - row `cargo  tests::quarantined …`.
  - **none:** a tree with `a.js`, `b.py` and `c.sh`, with no units and an empty quarantine list.
- Create `lands/ci/fixtures_test.sh`. For each fixture, in a temp directory:
  1. Copy the fixture, `git init -b main`, commit `base`.
  2. Render the kit: `(cd lands/ci && go run ./cmd/gardenio-ci-render --dir "$tmp" --land fixture --default-branch main --tag-glob 'v*.*.*')`. Commit.
  3. **NEGATIVE:** change one file in each unit, commit, and run `GARDENIO_CI_BASE=<base> GARDENIO_CI_HEAD=HEAD .gardenio/ci/run.sh light`. Assert:
     - the exit code is non-zero;
     - the output has `::error title=gardenio-ci failing test::<kind> <broken test>` (jest/vitest: the `gardenio-ci broken test` step error);
     - there is no error line for the quarantined test;
     - there is a `::warning title=gardenio-ci quarantined::` line.
  4. **POSITIVE:** reset to the render commit, change a file only in `ok`, and run light. The exit code is 0, while the quarantined failure is still present.
  5. **CONTROL OF THE CONTROL:** delete the quarantine row, commit, and run step 4 again. The exit code is non-zero, which proves the row is what made it green.
  6. **none:**
     - a valid change exits 0;
     - a change that adds a `.sh` syntax error exits non-zero;
     - a change that plants an AWS-style key exits non-zero. Build the key at runtime from two halves, for example `printf 'AKIA%s%s' Z3MSJV4WWNKW W5FV`, so that gardenio's own gitleaks job never sees a literal key in this repo.

  The script prints `CONTROL <fixture> <negative|positive|control> ok` per assertion and fails on the first wrong one.
- Modify the `lands-ci-kit` job in `.github/workflows/checks.yml`:
  - raise `timeout-minutes` to 30;
  - add `actions/setup-node@v4` (`node-version: '22'`);
  - add a venv with the pins: `python3 -m venv "$RUNNER_TEMP/venv" && "$RUNNER_TEMP/venv/bin/pip" install --quiet pytest==8.3.4 ruff==0.8.4 && echo "$RUNNER_TEMP/venv/bin" >> "$GITHUB_PATH"`;
  - add the `rustup` step copied from the `cargo-security` job (it pins 1.93.0; read its comments before copying);
  - add shellcheck, copied from `script-selftests`' install step;
  - then `bash lands/ci/fixtures_test.sh`.

  Path gates are unchanged: `lands/ci/**` already selects the job.
- Modify `.gitleaks.toml` only if the planted key still trips gardenio's scan. With the runtime split it should not.

**Steps:**
- [ ] Step 1: write `fixtures_test.sh` against the go fixture alone; see each assertion fail before the fixture exists, then pass.
- [ ] Step 2: add the other four fixtures one at a time.
- [ ] Step 3: wire the job; `gb pr open`.

**Narrow test:**
```bash
bash lands/ci/fixtures_test.sh   # needs go, node 22, python3 + pytest/ruff, cargo 1.93.0, shellcheck
shellcheck -x lands/ci/fixtures_test.sh
```
If your pod lacks a toolchain, say so in the PR and rely on the job's run. The test must FAIL, not skip, when a toolchain is missing, so a green job always means every fixture ran.

**Acceptance:** `lands-ci-kit` is green on the main run that includes this merge. Its log has `CONTROL <fixture> negative ok`, `positive ok` and `control ok` for go, python, node and rust, and the three `none` lines.

**Rules for this task (from the plan's Global Constraints):**
- One PR, opened as a draft with `gb pr open`; run `gb pr ready` once when done. PRs get the light tier only.
- Never retrigger CI: no empty commits, no merging main in, no "Update branch", no `gh run rerun`, no draft/ready flips. Your PR's own run of this job is wherever checks.yml runs path-gated jobs; do not force it.
- Breaking main is not taboo: fix forward or quarantine. Add no PR-time guard.
- No bake-in periods: the gate is the job's log, read once.
- Tenant CI is GitHub-hosted only. The `gardenio-k3s-light` runner here tests the kit, never a tenant.
- Cheap is on us.
- Never modify a tenant repo. The Lands App gains no permission.
- Test narrowly with the commands above.

### Task 1.5: guards over the rendered workflow: actionlint, concurrency, hosted-only, no matrix

**Who:** CREW (one PR).

**Why:** The caller workflow is committed once into every tenant repo and can never be fixed by gardenio afterwards; the Lands App has no `workflows` permission. So its shape has to be proven before any tenant commits it. CI onboarding spec §6 step 1.4: "a test that renders the template for each starter and runs actionlint plus a concurrency check modelled on `scripts/check-main-concurrency.py` over it". The binding cost and hosting rules are checked here too:
- GitHub-hosted only;
- no matrix;
- no `ready_for_review`;
- the timeouts;
- least-privilege permissions.

**Files:**
- Create `lands/ci/check-kit-workflow.py` (stdlib only). For each starter in `lands/ci/starters/`:
  1. Render into a temp repo with `go run ./cmd/gardenio-ci-render --dir <tmp> --land fixture --default-branch main --tag-glob 'v*.*.*'`, run from `lands/ci`. The starter's units come from the matching fixture in `lands/ci/testdata/fixtures/<starter>/`.
  2. Run `actionlint -no-color .github/workflows/gardenio-ci.yml` with the temp repo as the working directory, so the local action `./.gardenio/ci/setup` resolves. A missing actionlint binary is a FAILURE, never a skip.
  3. Parse the workflow with the stdlib loader and evaluator that `scripts/check-main-concurrency.py` already has. Load it with `importlib.util.spec_from_file_location`; there is no PyYAML on the runners. Check:
     - every job has `runs-on: ubuntu-latest`, and no `gardenio-k3s`, `self-hosted` or runner-group key appears anywhere;
     - no `strategy` or `matrix`;
     - `pull_request.types` is exactly `[opened, synchronize, reopened]`;
     - timeouts: light 15, full 45, daily 45, release 45, daily-issue 5;
     - the top-level `permissions` is exactly `contents: read`, and only `daily-issue` adds `issues: write`. No job requests `actions`, `workflows`, `packages` or `pull-requests: write`.
     - each job's `run:` calls `.gardenio/ci/run.sh <its tier>` (daily-issue calls `.gardenio/ci/daily-issue.sh`).
     - The concurrency block, evaluated:
       - `pull_request`: two pushes to one PR share a group, another PR gets a different one, and cancel-in-progress is true;
       - `push` to `refs/heads/main`: two shas share the group `gardenio-ci-refs/heads/main`, and cancel-in-progress is false;
       - `schedule`, `workflow_dispatch` and a tag push: every run gets its own group, and cancel-in-progress is false.
  4. Check the composite action `.gardenio/ci/setup/action.yml`: `using: composite`, and every `uses:` is one of `actions/setup-go@v5`, `actions/setup-node@v4`, `actions/setup-python@v5` or `actions/cache@v4`.
- Create `lands/ci/check-kit-workflow_test.py`. **Positive control:** the rendered go starter passes. Then one synthetic bad workflow per rule fails with a message naming the rule:
  - a `gardenio-k3s-light` runs-on;
  - a matrix;
  - `ready_for_review`;
  - `timeout-minutes: 60` on light;
  - `actions: write`;
  - a PR concurrency key without the ref;
  - cancel-in-progress true on a push;
  - an unlisted action in setup.
- Modify the `lands-ci-kit` job in `.github/workflows/checks.yml`:
  - fetch actionlint with `scripts/ci-fetch-verified.sh https://github.com/rhysd/actionlint/releases/download/v1.7.12/actionlint_1.7.12_linux_amd64.tar.gz 8aca8db96f1b94770f1b0d72b6dddcb1ebb8123cb3712530b08cc387b349a3d8 "$RUNNER_TEMP/actionlint.tgz"`. This is the version and sha the agent image pins (`gardenio/images/agent/install/versions.env:39`, `checksums.env:54`);
  - untar it to `$HOME/.local/bin`;
  - then run `python3 lands/ci/check-kit-workflow_test.py && python3 lands/ci/check-kit-workflow.py`.

**Steps:**
- [ ] Step 1: write the test with the synthetic cases; it fails.
- [ ] Step 2: write the guard; the test passes; the guard passes on the real render.
- [ ] Step 3: wire the job; `gb pr open`.

**Narrow test:**
```bash
python3 lands/ci/check-kit-workflow_test.py
python3 lands/ci/check-kit-workflow.py   # needs actionlint on PATH and go
```

**Acceptance:**
- Both pass in `lands-ci-kit`.
- Reverting any one line of the caller template that a rule covers makes the guard fail. Show one such run in the PR body, from your pod.

**Rules for this task (from the plan's Global Constraints):**
- One PR, opened as a draft with `gb pr open`; run `gb pr ready` once when done. PRs get the light tier only.
- Never retrigger CI: no empty commits, no merging main in, no "Update branch", no `gh run rerun`, no draft/ready flips.
- Breaking main is not taboo. Add no PR-time guard to gardenio's own workflows; this guard checks the tenant template only.
- No bake-in periods.
- Tenant CI is GitHub-hosted only, and this guard is where that rule becomes code.
- Cheap is on us: the timeout, matrix and cancel rules are the cost policy.
- Never modify a tenant repo. The Lands App gains no permission.
- Test narrowly with the commands above.

### Task 1.6: Phase 1 gate — the fixtures' controls behave in gardenio CI

**Who:** OPERATOR (no PR).

**Why:** Phase 2 delivers the kit to a real tenant, and the caller can never be changed by gardenio after the person commits it. So Phase 2 opens only once gardenio's own CI has shown the kit's controls working (CI onboarding spec §6, Phase 1 gate).

**Steps:**
- [ ] 1. Find the first `checks.yml` push run on `main` after Tasks 1.4 and 1.5 both merged. With batched main runs, a later run covers both: `gh run list -R mattbakerconsulting/gardenio -w checks.yml -b main -e push -L 20`.
- [ ] 2. In its `lands-ci-kit` job, check that the job concluded `success` and that its log has:
  - `CONTROL <fixture> negative ok`, `positive ok` and `control ok` for go, python, node and rust;
  - the three `none` lines;
  - the guard's pass line.

  Use `gh run view <id> -R mattbakerconsulting/gardenio --log --job <job id> | grep -E '^.*CONTROL|check-kit-workflow'`.
- [ ] 3. If a line is missing, the job did not prove the gate. File a bug under this plan and do not close this task.
- [ ] 4. Comment on the plan epic: `laptop-claude: Phase 1 gate: checks run <url>, lands-ci-kit <job url>: <n> CONTROL lines, guard passed.` Close this task.

**Acceptance:** the evidence comment exists, and it names the run.

**Rules:**
- No bake-in: proceed on the evidence.
- Never retrigger CI to get the evidence; read the run that exists, or the next main run.
- No local Go tests, builds or Terraform on the laptop.

---

## Phase 2: delivery (gate: crab-shack-3.5's first PR run and first default-branch run both show in the digest)

### Task 2.1: `gl ci init` and `gl ci status`

**Who:** CREW (one PR in `lands/gl`).

**Why:** The Lands App has no `workflows` permission (owner decision gd-oQAIXhuIm9, CI onboarding spec Q2). So the person commits `.github/workflows/gardenio-ci.yml` themselves, once. `gl ci init` renders it from the repo's `.gardenio/ci.toml`, which the steward's onboarding PR brought, using the same renderer the steward uses: Go module `gardenio/lands/ci`, package `kit`, with `Parse`, `Detect`, `Render`, `AppWritable`, `Version` and `TagGlob`. `gl` holds no credential but its land sign-in and talks only to the land door (`lands/gl/README.md`). Both commands work on the local checkout and send no request.

**Files:**
- Create `lands/gl/ci.go`, with `newCICmd(a *app) *cobra.Command` and subcommands `init` and `status`.
- Create `lands/gl/ci_test.go` (temp git repos, no network).
- Modify `lands/gl/main.go`: add `newCICmd(a)` to `root.AddCommand(...)` (:98).
- Modify `lands/gl/go.mod`:
  - `require gardenio/lands/ci v0.0.0` and `replace gardenio/lands/ci => ../ci`;
  - `github.com/BurntSushi/toml` arrives as an indirect requirement;
  - update `go.sum`.
- Modify `lands/gl/deps_test.go`:
  - add `"gardenio/lands/ci": true` to `allowedRepoPackages` (:14-19);
  - add `"gardenio/lands/ci": "../ci"` to the `want` map of `TestGoModReplacesOnlyInRepoModules` (:47-50);
  - update the go.mod header comment (it says the two replace directives point at two in-repo modules; now three).
- Modify `lands/gl/README.md`: add `gl ci init` and `gl ci status` to the Commands block, with one paragraph on who commits what.

**`gl ci init [--dir .] [--tag-glob G] [--default-branch B] [--detect] [--force]`:**
- `--dir` must be a git work tree.
- The default branch is `--default-branch`, else `git symbolic-ref --short refs/remotes/origin/HEAD` minus `origin/`.
- The land name, used only for `kit.DailyCron`, is the one every gl command resolves (`--land`, `GL_LAND`, `gl use`). No request is sent.
- The tag glob is `--tag-glob`, else `v*.*.*` (the land default, `^v\d+\.\d+\.\d+$`).
- **When `.gardenio/ci.toml` exists:**
  - parse it;
  - render;
  - write ONLY `.github/workflows/gardenio-ci.yml`;
  - if `.gardenio/ci/VERSION` differs from `kit.Version`, print `your steward keeps .gardenio/ current by pull request; this gl was built with kit <v>`.
- **When it does not exist:**
  - without `--detect`, fail with `no .gardenio/ci.toml: merge your steward's CI pull request first, or run gl ci init --detect to write the whole kit yourself`;
  - with `--detect`, run `kit.Detect` and write every file `Render` returns.
- An existing caller that differs from the render is refused without `--force`, with a line count of the difference.
- It never runs `git add`, `commit` or `push`. It prints:
  ```
  git add .github/workflows/gardenio-ci.yml && git commit -m "ci: gardenio-ci" && git push
  ```
  For a repo whose `origin` is a fork, it adds: `Actions are off on a fork until you enable them: https://github.com/<owner>/<repo>/actions`. Fork detection here is only a hint: say so when `git config remote.upstream.url` exists.

**`gl ci status [--dir .] [--json]`** reads locally only:
- the kit version in `.gardenio/ci/VERSION` against gl's `kit.Version`;
- whether the committed caller equals what `Render` gives now (`caller out of date: run gl ci init and commit it`);
- the units from ci.toml;
- the quarantine rows from the checkout's `.github/quarantine.tsv` and from `refs/remotes/origin/gardenio/quarantine:.github/quarantine.tsv` when that ref exists, each marked `expired` past its expiry date.

`--json` prints `{kit_version, vendored_version, caller_current, units[], quarantine[]}`.

**Tests (`ci_test.go`):**
- init with ci.toml writes only the caller;
- init without ci.toml fails with the message above;
- `--detect` writes the whole kit;
- a differing caller is refused, and `--force` overwrites it;
- status reports a stale caller, a version mismatch, and rows from both copies (create the `refs/remotes/origin/gardenio/quarantine` ref in the test), with expired rows flagged;
- the `--json` shape;
- the deps guards pass with the new module.

**Steps:**
- [ ] Step 1: write the tests; they fail. Step 2: implement. Step 3: README. Step 4: `gb pr open`.

**Narrow test:**
```bash
cd lands/gl && GOWORK=off go test -count=1 -run 'TestCI|TestNoBackingStoreDependencies|TestGoModReplacesOnlyInRepoModules' ./...
cd lands/gl && GOWORK=off go test -count=1 ./...
```

**Acceptance:** the `lands-gl` job in checks.yml is green, and `gl ci --help` lists `init` and `status`.

**Rules for this task (from the plan's Global Constraints):**
- One PR, opened as a draft with `gb pr open`; run `gb pr ready` once when done. PRs get the light tier only.
- Never retrigger CI: no empty commits, no merging main in, no "Update branch", no `gh run rerun`, no draft/ready flips.
- Breaking main is not taboo: fix forward or quarantine. Add no PR-time guard.
- No bake-in periods.
- Tenant CI is GitHub-hosted only.
- Cheap is on us.
- Never modify a tenant repo. gl writes files in the person's own checkout, at their command, and never commits or pushes.
- The Lands App gains no permission.
- Test narrowly with the commands above.

### Task 2.2: `gb lands ci render`, the steward's renderer

**Who:** CREW (one PR: `gardenio/controller` plus image wiring).

**Why:** A tenant land's steward onboards CI by opening a PR that adds `.gardenio/` and `.github/quarantine.tsv`. The Lands App can write both; it cannot write `.github/workflows/`. The steward then shows the person the caller text in a decision (Task 2.3). The steward runs in an agent pod with `gb`, so gb needs the same renderer `gl ci init` uses: Go module `gardenio/lands/ci`, package `kit` (CI onboarding spec §4.1, plan decision D3).

**Files:**
- Create `gardenio/controller/cmd/gb/lands_ci.go`. It registers `landsCiCmd` with `landsCmd.AddCommand(...)`, as `lands_digest.go:143` does, with the subcommand `render`. Task 2.3 adds `decision`.
- Create `gardenio/controller/cmd/gb/lands_ci_test.go`.
- Modify `gardenio/controller/go.mod`: `require gardenio/lands/ci v0.0.0` and `replace gardenio/lands/ci => ../../lands/ci`, after the existing `replace gardenio/clicreds => ../clicreds` (:230). Update `go.sum` and the root `go.work.sum` as `go mod tidy` leaves them.
- Modify every image that builds the controller module, so `lands/ci` is in the build context. Mirror how `gardenio/images/agent/Dockerfile:140-147` copies `lands/contract`:
  - copy `lands/ci/go.mod` and `go.sum` before the module download;
  - copy `lands/ci/` before the build.

  Find the images with `grep -rln 'gardenio/controller' gardenio/images/*/Dockerfile`. images.yml has no path filters (gd-whdmfznixO): a release tag builds every enabled image, so there is no filter to extend.
- Modify `gardenio/docs/lands/steward.md`: a short "CI kit renderer" paragraph under the steward's tools.

**`gb lands ci render <land> --dir <checkout> [--print-caller] [--json]`:**
- Read the land's project bead through the daemon.
- Refuse a land whose `land_kind` is not `tenant`: `gardenio's own CI is not kit-managed`.
- The tag glob is `kit.TagGlob(release_tag_pattern)`. An unset pattern or an unknown glob is an error naming the field.
- The default branch is the bead's `default_branch`.
- `--dir` must be a git work tree whose `git remote get-url origin`, normalised to `owner/repo`, equals the land's `git_url`. Otherwise refuse.
- With `.gardenio/ci.toml` present, `kit.Parse` it; otherwise `kit.Detect` the checkout.
- Write every path for which `kit.AppWritable` is true. `.github/quarantine.tsv` is written only when absent. **Never** write anything under `.github/workflows/`.
- Print the written paths, or JSON `{written[], kit_version, caller_path}`.
- `--print-caller` prints only the caller's text to stdout and writes nothing.
- No git add, commit or push. The steward's duty does that (Task 2.3).

**Tests:**
- A fake daemon with a tenant land; render into a temp checkout. Assert the written set and that no path under `.github/workflows/` exists afterwards.
- An existing `.github/quarantine.tsv` is kept.
- A mismatched origin is refused.
- An operator land is refused.
- `--print-caller` equals `kit.Render(...)[".github/workflows/gardenio-ci.yml"]`.
- An unset `release_tag_pattern` is refused.

**Steps:**
- [ ] Step 1: tests, which fail. Step 2: the command. Step 3: the go.mod and image wiring; run the module-coverage guard. Step 4: `gb pr open`.

**Narrow test:**
```bash
cd gardenio/controller && go test ./cmd/gb/ -run 'TestLandsCI' -count=1
bash scripts/check-go-module-coverage.sh
```

**Acceptance:** tests and guards are green in CI, and the images built on the merge include `lands/ci` (the build succeeds).

**Rules for this task (from the plan's Global Constraints):**
- One PR, opened as a draft with `gb pr open`; run `gb pr ready` once when done. PRs get the light tier only.
- Never retrigger CI: no empty commits, no merging main in, no "Update branch", no `gh run rerun`, no draft/ready flips.
- Breaking main is not taboo: fix forward or quarantine. Add no PR-time guard.
- No bake-in periods.
- Tenant CI is GitHub-hosted only.
- Cheap is on us.
- Never modify a tenant repo: the renderer writes into the steward's checkout only, and the steward's PR is the person's to merge.
- The Lands App gains no permission; the renderer never writes the one path it cannot.
- Test narrowly with the commands above. Never run `go test ./...` at the `gardenio/controller` root.

### Task 2.3: the steward's CI onboarding duty and the onboarding decision

**Who:** CREW (one PR: `gardenio/controller/internal/steward` and `cmd/gb`).

**Why:** CI onboarding spec §4.1:
- The steward onboards CI for its land, and the person approves.
- On its first wake after provisioning (the `land-opened` wake), or when a person asks it to "set up CI", it:
  - detects the units;
  - renders the kit with `gb lands ci render` (Task 2.2);
  - opens a PR with `.gardenio/` and `.github/quarantine.tsv`;
  - files ONE decision for the person, holding the caller workflow's text and `gl ci init`, plus "enable Actions" on a fork.
- It never pushes to `.github/workflows` or to the default branch.
- It also keeps the kit current: a kit fix upstream reaches every land on its next kit PR (§5.1, "cheap is on us").
- The never-retrigger rule goes into the steward's prompt in the words of the repo's `CLAUDE.md` ("CI on pull requests").

The tenant playbook is `StewardPlaybookPrompt` in `gardenio/controller/internal/steward/steward.go`: duties 1-5, then "## Never" (:823). The steward pod's GitHub token is the Lands installation token at `/etc/git-creds/github/token`.

**Files:**
- Create `gardenio/controller/internal/steward/cionboard.go`, with `CIOnboardingContext(repo, prURL, caller string, fork bool) string`. It returns the decision context, in plain words for a person:
  - **What it is:** gardenio CI runs on GitHub-hosted runners and is billed to this repo's GitHub account; it is free on public repos, and a private repo uses the account's included minutes.
  - **What it runs:**
    - a light check on each PR, for only what changed;
    - a full run on each push to the default branch;
    - one daily run;
    - a release run on a tag.
  - **What they do:**
    - merge the steward's PR `<prURL>`;
    - then commit the workflow, either the text below in a fenced `yaml` block (`caller`), or `gl ci init` after pulling, then `git add .github/workflows/gardenio-ci.yml && git commit -m "ci: gardenio-ci" && git push`.
  - **What the steward keeps:**
    - `.gardenio/`, updated by pull request;
    - the branch `gardenio/quarantine`, a list of known-failing tests that never blocks a second PR, which they can read and change;
    - nothing in `.github/workflows` is ever pushed by gardenio.
  - **When `fork` is true:** "Actions are off on a fork until you enable them: https://github.com/<repo>/actions."
- Create `gardenio/controller/internal/steward/cionboard_test.go`:
  - the caller is in a fenced block;
  - the fork line appears only for forks;
  - "GitHub-hosted" and who pays are stated;
  - the PR URL is present.
- Modify `gardenio/controller/cmd/gb/lands_ci.go` to add `gb lands ci decision <land> --pr <url> --dir <checkout>`:
  - it reads the caller with the same code as `render --print-caller`;
  - it asks GitHub whether the repo is a fork, with `GET /repos/{repo}` (`.fork`) using the token file `/etc/git-creds/github/token`, never printing it;
  - it prints `CIOnboardingContext(...)`.

  Add a test to `lands_ci_test.go` with an httptest GitHub.
- Modify `gardenio/controller/internal/steward/steward.go`, in the tenant playbook only (not `.Home`, not `.Gatehouse`):
  - **New duty "6. SET UP CI",** inserted before `## Never`:
    ```
    6. SET UP CI. On the {{wakeOpened}} wake, or when a person asks you to set up
       CI, and when your notes' In flight has no "CI onboarding" line:
         git fetch origin && git show origin/<default branch>:.gardenio/ci.toml
       If that prints the file, CI is set up: say so if asked, and stop here.
       Otherwise:
         git switch -c gardenio/ci-onboarding origin/<default branch>
         gb lands ci render {{.Name}} --dir .
         git add .gardenio .github/quarantine.tsv
         git commit -m "ci: gardenio CI kit (.gardenio/)"
         git push -u origin gardenio/ci-onboarding
         GH_TOKEN=$(cat /etc/git-creds/github/token) gh pr create --base <default branch> \
           --title "ci: gardenio CI kit" --body "The gardenio CI kit. See the decision in #decisions."
         gb lands ci decision {{.Name}} --pr <pr url> --dir . > ci-onboarding.md
         gb decision create --no-wait --for <owner> --prompt "Set up CI on {{.Repo}}?" \
           --context "$(cat ci-onboarding.md)" \
           --options '[{"id":"done","short":"I committed the workflow"},{"id":"later","short":"Not now"}]'
       Note "CI onboarding: PR <url>, decision <id>" under In flight. On the
       decision's answer, done: check the workflow is on the default branch
       (git show origin/<default branch>:.github/workflows/gardenio-ci.yml) and
       move the line to Answers; later: move it to Answers and do not ask
       again until a person asks.
       KEEP THE KIT CURRENT on the {{wakeDigest}} wake, once CI is set up: in a
       clean checkout of the default branch run gb lands ci render {{.Name}} --dir .
       If git status shows changes under .gardenio/, open a PR from branch
       gardenio/ci-kit-<.gardenio/ci/VERSION> with them, unless one is open.
       If gb lands ci render {{.Name}} --dir . --print-caller differs from the
       committed .github/workflows/gardenio-ci.yml, tell the people once in
       #general that a new workflow is ready, with gl ci init to apply it.
    ```
  - **The tenant `## Never` list gains:**
    - `- Push to .github/workflows, to the default branch, or a tag.` (Task 4.4 will relax the tag part for releases.)
    - `- Re-run CI: no empty commits to retrigger, no merging the default branch into a PR to refresh it, no "Update branch", no draft-to-ready flips just to get CI. A PR that is red only because the default branch was red is not re-proved; fix the default branch.`
- Modify `gardenio/controller/internal/steward/steward_test.go`:
  - add `TestRenderPrompt_CIOnboardingDuty`: the tenant prompt has duty 6 with `gb lands ci render <land>` and `gb lands ci decision`, and the two Never lines; the Home and Gatehouse prompts have neither;
  - update any test that pins the Never list.
- Modify `gardenio/docs/lands/steward.md`: a section "CI onboarding (plan ci-onboarding 2.3)" with the duty, the decision, and what the steward never pushes.
- Check, and say in the PR: lands-gardeners read `gardenio/controller/cmd/gb/claudemd_default.md`. Its Delivery Protocol already says "PR CI is never re-run on purpose" (:90-93), so `grep -n 'never re-run on purpose' gardenio/controller/cmd/gb/claudemd_default.md` prints a line. If it does not, add the same sentence there.

**Steps:**
- [ ] Step 1: tests (cionboard, prompt, gb decision); they fail. Step 2: implement. Step 3: docs. Step 4: `gb pr open`.

**Narrow test:**
```bash
cd gardenio/controller && go test ./internal/steward/ -run 'TestRenderPrompt|TestCIOnboarding' -count=1
cd gardenio/controller && go test ./cmd/gb/ -run 'TestLandsCI' -count=1
```

**Acceptance:** tests are green in CI, and the rendered tenant prompt for a fixture land contains duty 6 verbatim with the land's name substituted.

**Rules for this task (from the plan's Global Constraints):**
- One PR, opened as a draft with `gb pr open`; run `gb pr ready` once when done. PRs get the light tier only.
- Never retrigger CI: no empty commits, no merging main in, no "Update branch", no `gh run rerun`, no draft/ready flips.
- Breaking main is not taboo: fix forward or quarantine. Add no PR-time guard.
- No bake-in periods: the duty runs on a wake or a person's ask, never on a clock.
- Tenant CI is GitHub-hosted only, and the decision says so to the person.
- Cheap is on us: KEEP THE KIT CURRENT is how a cost fix reaches every land.
- Never modify a tenant repo without the person's approval: the steward opens a PR and a decision, and the person merges and commits.
- The Lands App gains no permission; the steward never pushes `.github/workflows`.
- Test narrowly with the commands above. Never run `go test ./...` at the `gardenio/controller` root.

### Task 2.4: the digest's CI line, read once a day from check runs

**Who:** CREW (one PR in `gardenio/controller`).

**Why:** CI onboarding spec §4.4 asks for one digest line, only when something changed, with:
- the default branch's state;
- the reds and their rows;
- runner-minutes since the last digest, summed from check-run start and end times (`checks: read`; wall time, not billed minutes).

Until Phase 3, the daily digest wake reads check runs once a day, and nothing polls (§2.4). The Phase 2 gate needs both of crab-shack-3.5's first runs to show in the digest. That is why this lands in Phase 2 (plan decision D5), although the spec lists it under step 3.3.

The digest is `gb lands digest <land>` (`gardenio/controller/cmd/gb/lands_digest.go`) over `steward.GatherDigest` and `steward.FormatDigest` (`internal/steward/digest.go:117`, :256). The steward pod's Lands installation token is at `/etc/git-creds/github/token`. The kit's check names are `gardenio-ci/light`, `gardenio-ci/full`, `gardenio-ci/daily` and `gardenio-ci/release`. Failing tests are annotations titled `gardenio-ci failing test` (plan decision D4).

**Files:**
- Create `gardenio/controller/internal/steward/ci.go`:
  - `type CheckRun struct { ID int64; Name, SHA, Conclusion string; StartedAt, CompletedAt time.Time; PR int }`.
  - `type CIReader interface { DefaultBranchRuns(ctx, repo, branch string, since time.Time) ([]CheckRun, error); PRRuns(ctx, repo string, since time.Time) ([]CheckRun, error); FailingTests(ctx, repo string, checkRunID int64) ([]string, error) }`.
  - `GitHubCIReader{APIBase, TokenFile string, HC *http.Client}`. It re-reads the token file on each call, and no error string or log contains the token. It uses:
    - `GET /repos/{repo}/commits?sha={branch}&since={RFC3339}&per_page=30`, then `GET /repos/{repo}/commits/{sha}/check-runs?per_page=100` for each commit;
    - `GET /repos/{repo}/pulls?state=all&sort=updated&direction=desc&per_page=30`, keeping PRs with `updated_at >= since`, then their head sha's check runs;
    - `GET /repos/{repo}/check-runs/{id}/annotations`, keeping messages whose title is `gardenio-ci failing test`, at most 10 calls per digest.

    It keeps only check runs whose name starts with `gardenio-ci/`. That is at most 30 + 30 + 10 + 2 calls per digest.
- Modify `gardenio/controller/internal/steward/digest.go`:
  - Add `type DigestCI struct { Branch, State, SHA, RedSinceSHA string; Failing []string; PRRuns, PRRed int; Minutes float64; Top string; Err string }` and `Digest.CI *DigestCI`.
  - `GatherDigest` takes an extra `ci CIReader`, which is nil for a land without the kit, and the land's repo and default branch, through an options struct you add. From the reader:
    - **State:** `green` or `red`, from the newest completed `gardenio-ci/full` or `gardenio-ci/daily` run on the default branch since `Since`. If none ran, `State` is `none`.
    - **RedSinceSHA:** the oldest sha of the consecutive red streak in the window.
    - **Failing:** the failing tests of the newest red run.
    - **PRRuns and PRRed:** PR `gardenio-ci/light` runs since `Since`.
    - **Minutes:** the sum of `CompletedAt - StartedAt` over every `gardenio-ci/*` run completed since `Since`.
    - **Top:** the check name with the most minutes.
  - A reader error sets `Err` and never fails the digest.
  - `Quiet` is false when any `gardenio-ci/*` run completed since `Since`.
  - `FormatDigest` adds one `CI:` line when `CI != nil` and (any run since, or `Err != ""`). Examples:
    - `CI: main green at abc1234; 3 PR runs (1 red); 12.5 runner-minutes since the last digest (wall time; most: gardenio-ci/full).`
    - `CI: main red since abc1234 (gardenio-ci/full): go api TestFoo; ...`
    - `CI: could not read (<Err>).`
- Modify `gardenio/controller/cmd/gb/lands_digest.go`: build a `GitHubCIReader` (token file `/etc/git-creds/github/token`, API `https://api.github.com`) only for a tenant land whose project bead has `github_app=lands`; pass nil otherwise. `--json` carries `ci`.
- Modify `gardenio/controller/internal/steward/steward.go`, duty 4 REPORT. Add to the message order, after "what happened": `- CI: the digest's CI line as written, if it has one;`.
- Modify `gardenio/controller/internal/steward/digest_test.go`, with a fake `CIReader`:
  - green;
  - red with failing tests and a red-since sha;
  - no runs (no line, and `Quiet` unaffected);
  - a reader error (the "could not read" line, and the digest still succeeds);
  - minutes summed exactly;
  - a PR updated before `Since` excluded.
- Create `gardenio/controller/internal/steward/ci_test.go`:
  - an httptest GitHub; request paths and the `Authorization: Bearer <file token>` header;
  - non-`gardenio-ci/` runs are dropped;
  - an error message never contains the token (assert on a 401 response).

**Steps:**
- [ ] Step 1: tests; they fail. Step 2: implement. Step 3: the playbook line. Step 4: `gb pr open`.

**Narrow test:**
```bash
cd gardenio/controller && go test ./internal/steward/ -run 'TestDigest|TestGatherDigest|TestFormatDigest|TestGitHubCIReader|TestRenderPrompt' -count=1
cd gardenio/controller && go test ./cmd/gb/ -run 'TestLandsDigest' -count=1
```

**Acceptance:** tests are green in CI, and `gb lands digest <land> --json` on a land without the kit has `"ci": null` and is otherwise unchanged.

**Rules for this task (from the plan's Global Constraints):**
- One PR, opened as a draft with `gb pr open`; run `gb pr ready` once when done. PRs get the light tier only.
- Never retrigger CI: no empty commits, no merging main in, no "Update branch", no `gh run rerun`, no draft/ready flips.
- Breaking main is not taboo: fix forward or quarantine. Add no PR-time guard.
- No bake-in periods, and no new cron: the read happens on the existing digest wake.
- Tenant CI is GitHub-hosted only.
- Cheap is on us: the minutes line is the meter.
- Never modify a tenant repo; this task only reads.
- The Lands App gains no permission; `checks: read` is enough.
- Test narrowly with the commands above. Never run `go test ./...` at the `gardenio/controller` root.

### Task 2.5: Phase 2 gate — crab-shack-3.5's first PR run and first default-branch run show in the digest

**Who:** PERSON (the owner, acting as `fimbazland`) with the OPERATOR (no PR).

**Why:** This is the first tenant (CI onboarding spec §6 step 2.3 and Phase 2 gate). Every change to the tenant repo is the person's, made in the flow the spec describes:
- they enable Actions on the fork;
- they merge the steward's `.gardenio/` PR;
- they commit the caller themselves.

Phase 3 opens when both runs show in the digest.

**Preconditions:**
- Tasks 2.1-2.4 are merged, and gb is rolled in a release window (gb ships in the agent image).
- `gl` built from main is on the Vultr host (`~crab/.local/bin`, as of 11-01 a build from main, not a release asset). Rebuild it from main after Task 2.1 merges.

**Steps:**
- [ ] 1. The steward's onboarding PR and decision exist. The land-opened wake ran them, or a land member writes "please set up CI" in the land's `#general`. Check with `kd list --project <land> --type decision` and the PR list on `fimbazland/crab-shack-3.5`.
- [ ] 2. The PERSON, as `fimbazland`:
  - enables Actions on the fork: the repo's Actions tab, then "I understand my workflows, go ahead and enable them";
  - reviews and merges the steward's PR;
  - in `~crab/src/crab-shack-3.5`, runs `git pull && gl ci init`, then the printed `git add … && git commit … && git push`;
  - answers the decision `done`.

  That push is the first default-branch run (`gardenio-ci/full`, the smoke tier only: crab-shack has no units).
- [ ] 3. The PERSON opens one small PR, for example a README fix. That is the first `gardenio-ci/light` run.
- [ ] 4. The OPERATOR runs `gb lands digest <land>` (read-only; it writes nothing). The output has a `CI:` line naming the default-branch state and the PR run. Note each run's wall minutes; the smoke tier should be under a minute. If it is not, file a kit cost bug under gd-WFMvg9sDKE (cheap is on us).
- [ ] 5. Comment on the plan epic: `laptop-claude: Phase 2 gate: <land> full run <url> (<state>), light run <url>; digest CI line: "<line>".` Close this task.

**Acceptance:** the digest's CI line shows both runs, and the evidence comment exists.

**Rules:**
- No bake-in: read the digest as soon as both runs completed; do not wait for the daily cron.
- Never modify the tenant repo as the operator: every change above is the person's.
- No local Go tests, builds or Terraform on the laptop.

---

## Phase 3: CI as events (gate: a red default-branch run on a tenant wakes its steward with no cron involved)

### Task 3.1: the Lands App gets a webhook and `check_suite`, kept equal in three places

**Who:** CREW (one PR: the registration script, `internal/githubapp`, docs). The live App is updated by the operator in Task 3.5.

**Why:** "CI completion becomes a bead event, not a poll" (CI onboarding spec §2.4). The Lands App subscribes to `check_suite`. GitHub delivers `check_suite` `completed` to every App with `checks: read` on the repo, and the Lands App already has `checks: read`. So no permission changes, and the Lands App gains no `workflows` permission (owner decision gd-oQAIXhuIm9, Q2).

Today the App has `default_events: []` ("No webhook, no events"; `gardenio/docs/lands/github-app.md:63,76`). Its permission set is stated in three places that tests keep equal:
- `MANIFESTS["lands"]` in `gardenio/scripts/github-app-register.py` (:217-240);
- `githubapp.LandsTokenPermissions` (`internal/githubapp/identity.go:46-54`);
- the doc's table.

The event list gets the same treatment (spec §6 step 3.1).

**Files:**
- Modify `gardenio/scripts/github-app-register.py`:
  - `MANIFESTS["lands"]` gains `"events": ["check_suite"]`;
  - its header comment (:205-216) replaces "No webhook and no events in v1" with "One event, check_suite (plan ci-onboarding 3.1), delivered to the github-bridge's /webhook/lands; no new permission";
  - `build_manifest` (:249-302) is unchanged: it emits `default_events` only with `--webhook-url`.
- Modify `gardenio/controller/internal/githubapp/identity.go`: add
  ```go
  // LandsWebhookEvents is the Lands App's whole event subscription: a
  // completed check suite, which the github-bridge turns into bead writes
  // (plan ci-onboarding 3.2). check_suite needs only checks: read, which the
  // App already has. A fresh slice per call.
  func LandsWebhookEvents() []string { return []string{"check_suite"} }
  ```
  Add `TestLandsWebhookEvents` to `identity_test.go`.
- Modify `gardenio/scripts/github-app-register_test.py`:
  - add `test_lands_events_match_the_go_set`, which parses `identity.go` the way `test_lands_permissions_match_the_go_set` (:110) does;
  - add `test_lands_events_match_the_doc`, which parses the new "## Events" table in `github-app.md`;
  - update `test_lands_manifest_has_the_setup_leg_and_no_webhook` (:129): without `--webhook-url` there is still no `hook_attributes`; with it, `default_events == ["check_suite"]`;
  - the permissions test still asserts the exact set.
- Modify `gardenio/docs/lands/github-app.md`:
  - a new "## Events" section with a one-row table (`check_suite`, `checks: read`, why);
  - the webhook URL form, `https://<githubBridge.ingress.host>/webhook/lands`;
  - the secret's SSM name, `/gardenio/github-app/lands/webhook-secret`;
  - the operator runbook:
    1. Generate a secret with `openssl rand -hex 32` and store it with `aws ssm put-parameter --profile gardenio --region us-east-1 --type SecureString --name /gardenio/github-app/lands/webhook-secret --value <secret>`, never echoing it.
    2. In the App's settings, set Webhook active, the URL and the secret, and tick "Check suite" under Subscribe to events. GitHub offers no API to change an App's event subscription.
    3. Check that installations do not need to re-accept. GitHub asks installers to re-approve only when permissions change. Confirm on the App's install page for crab-shack that no permission request is pending.
  - In "The manifest", the bullet "No webhook, no events." becomes the new text.

**Steps:**
- [ ] Step 1: tests; they fail. Step 2: implement. Step 3: docs. Step 4: `gb pr open`.

**Narrow test:**
```bash
python3 gardenio/scripts/github-app-register_test.py
cd gardenio/controller && go test ./internal/githubapp/ -run 'TestLands' -count=1
```

**Acceptance:**
- Tests are green in CI.
- `./gardenio/scripts/github-app-register.py manifest --app lands --org mattbakerconsulting --webhook-url https://example.invalid/webhook/lands` prints `"default_events": ["check_suite"]` and the unchanged `default_permissions`.

**Rules for this task (from the plan's Global Constraints):**
- One PR, opened as a draft with `gb pr open`; run `gb pr ready` once when done. PRs get the light tier only.
- Never retrigger CI: no empty commits, no merging main in, no "Update branch", no `gh run rerun`, no draft/ready flips.
- Breaking main is not taboo: fix forward or quarantine. Add no PR-time guard.
- No bake-in periods.
- Tenant CI is GitHub-hosted only.
- Cheap is on us.
- Never modify a tenant repo.
- The Lands App gains no permission at all: no `workflows`, no `actions`. The permission tests must still assert the exact five.
- Do not change the live App in this PR; the operator does that in Task 3.5.
- Test narrowly with the commands above.

### Task 3.2: the github-bridge turns Lands-App `check_suite` webhooks into step and project bead writes

**Who:** CREW (one PR: `cmd/github-bridge`, the kbeads and controller schemas, the chart).

**Why:**
- A completed suite on a tenant's default branch, or on a step's PR, must write onto the step or project bead and wake the steward, with no poll (CI onboarding spec §2.4, §6 step 3.2).
- The bridge's `/webhook` route verifies only the fleet App's secret (`cmd/github-bridge/main.go:413-419`). Its `pipelineHandler` stamps `mr_pipeline_status` and `mr_pipeline_url` on fleet MR beads (`pipeline.go:57-175`).
- Tenant PRs have no merge-request bead. A land's step (task) bead carries the PR in `mr_url`, a field every issue type declares through `mrFields` (`kbeads/internal/server/config.go:40-57`).
- Tasks 4.1 and 4.2 gate a steward's merge approval on what this task records, so the head sha must be recorded too.
- Task 0.2 gave the bridge a per-project token router (`projectTokens`): a tenant land uses the Lands App, pinned to its installation, with `LandsTokenPermissions`.

**Files:**
- Modify `gardenio/controller/cmd/github-bridge/main.go`:
  - config `landsWebhookSecret` from `GITHUB_LANDS_WEBHOOK_SECRET_FILE`;
  - when set, `mux.Handle("/webhook/lands", newWebhookHandler(webhookConfig{Secret: landsSecret, Sink: landsSink, Replay: replay, Logger: logger, Observe: ...}))`, with outcomes counted under `gardenio_github_webhook_deliveries_total` with label values prefixed `lands_`;
  - unset: no route, and the start log says `lands_webhook=off`.
- Create `gardenio/controller/cmd/github-bridge/landsci.go`, `landsCIHandler`, which handles only `check_suite` with action `completed`. Everything else is ignored at debug level.
  1. Resolve the project by `repository.full_name`. It must be a project with `github_app=lands`, `land_kind=tenant` and a matching `git_url`, and its owner must not be an operator org. Otherwise ignore the delivery.
  2. Map the conclusion with `pipelineStatusFor` (`pipeline.go:31`). No usable conclusion means ignore.
  3. **For each PR in `check_suite.pull_requests`:** find the land's task bead with `mr_url == https://github.com/<repo>/pull/<n>`. Use `ListBeadsFiltered` with type `task`, label `project:<land>` and fields `{mr_url: <url>}`. Set:
     - `mr_pipeline_status`;
     - `mr_pipeline_url` (`https://github.com/<repo>/commit/<sha>/checks`);
     - `mr_pipeline_sha` (the suite's `head_sha`).

     An unchanged state is not rewritten. No such bead: log at info and skip.
  4. **When `check_suite.head_branch` equals the project's `default_branch`:** read the suite's check runs with the project's Lands token: `GET /repos/{repo}/check-suites/{id}/check-runs`, through `projectTokens.For`. Keep `gardenio-ci/full` and `gardenio-ci/daily`; none means ignore (another workflow's suite). Write the project fields:
     - `ci_default_state` (`green` for success, `red` otherwise);
     - `ci_default_sha`;
     - `ci_default_at` (RFC 3339);
     - `ci_red_since_sha`: set to the head sha on a green-to-red change, kept while red, cleared on green.
  5. **On a change of `ci_default_state`:** create ONE message bead in the land's `#machines`, the channel whose `channel_kind` is `machines` under label `project:<land>`, with:
     - author `github-bridge`;
     - origin `ci_default_red` or `ci_default_green`;
     - text `<branch> red at <sha7> (<check name>): https://github.com/<repo>/commit/<sha>/checks`, or `<branch> green again at <sha7>`.

     Build it the way the purse watcher builds its notice (`grep -rn 'OriginNotice' gardenio/controller/internal` for the create). The same state delivered again posts nothing.
- Modify `kbeads/internal/server/config.go`:
  - `type:project` gains `{"name": "ci_default_state", "type": "enum", "values": ["green", "red"]}`, `ci_default_sha` (string), `ci_default_at` (timestamp) and `ci_red_since_sha` (string), with a comment: written by the github-bridge, plan ci-onboarding 3.2;
  - `mrFields` (:40-57) gains `{"name": "mr_pipeline_sha", "type": "string"}`.
- Modify `gardenio/controller/internal/bridge/init_configs.yaml`: the same four project fields in its `type:project` list.
- Modify `gardenio/helm/gardenio/values.yaml`, under `githubBridge`, next to `webhookSecret` (:15525-15533): a `landsWebhookSecret` block of the same shape, with `enabled: false`, `secretName: gardenio-lands-github-webhook-secret`, `secretKey: webhook-secret` and `externalSecret.remoteRef: /gardenio/github-app/lands/webhook-secret`.
- Modify the chart templates:
  - `templates/github-bridge/deployment.yaml`: env `GITHUB_LANDS_WEBHOOK_SECRET_FILE` plus a volume, when enabled;
  - `templates/github-bridge/webhook-secret-external-secret.yaml`: a second ExternalSecret when enabled;
  - `templates/github-bridge/ingressroute.yaml`: `Path(`/webhook`) || Path(`/webhook/lands`)` when enabled.
- Tests:
  - Create `landsci_test.go`:
    - a PR suite stamps the task's three `mr_pipeline_*` fields;
    - a red default-branch suite sets the project fields and posts one `#machines` message, and a second identical delivery posts nothing;
    - green clears `ci_red_since_sha` and posts the green message;
    - a non-tenant repo, an operator-org repo, and a suite without `gardenio-ci/` runs are each ignored;
    - Lands unconfigured: logged, no write.
  - Modify `webhook_test.go`: `/webhook/lands` accepts the Lands secret and rejects the fleet secret, and `/webhook` the reverse. Both directions are tested: that is the positive and the negative control.
  - Modify `gardenio/helm/gardenio/tests/github_bridge_test.go`: the lands secret, env and route render only when enabled.
  - kbeads: the builtin declares the new fields. Extend `TestBuiltinHarvestFieldsDeclared` or the nearest builtin-fields test for `mr_pipeline_sha`.

**Steps:**
- [ ] Step 1: tests; they fail. Step 2: the handler and the route. Step 3: the schemas. Step 4: the chart. Step 5: `gb pr open`.

**Narrow test:**
```bash
cd gardenio/controller && go test ./cmd/github-bridge/ -run 'TestLandsCI|TestWebhook|TestPipeline' -count=1
cd kbeads && go test ./internal/server/ -run 'TestProjectSchema|TestBuiltin' -count=1
cd gardenio/helm/gardenio/tests && GOWORK=off go test -count=1 -run 'TestGithubBridge' .
```

**Acceptance:**
- Tests are green in CI.
- With `landsWebhookSecret.enabled: false`, the rendered chart is unchanged (asserted).
- No token or secret appears in a log, an error or a bead field.

**Rules for this task (from the plan's Global Constraints):**
- One PR, opened as a draft with `gb pr open`; run `gb pr ready` once when done. PRs get the light tier only.
- Never retrigger CI: no empty commits, no merging main in, no "Update branch", no `gh run rerun`, no draft/ready flips.
- Breaking main is not taboo: fix forward or quarantine. Add no PR-time guard.
- No bake-in periods.
- No new poll: this is the event path.
- Tenant CI is GitHub-hosted only.
- Cheap is on us.
- Never modify a tenant repo; the bridge only reads it.
- The Lands App gains no permission; check-run reads use `checks: read`.
- Test narrowly with the commands above. Never run `go test ./...` at the `gardenio/controller` or `kbeads` root.

### Task 3.3: the steward's CI watch and quarantine duties, and the CI wake

**Who:** CREW (one PR: `internal/steward`, `cmd/gb`, `lands/ci`).

**Why:** CI onboarding spec §4.2-4.3 and §2.1 ("Attribution"):
- **The CI wake.** When a tenant's default branch turns red or green, the github-bridge (Task 3.2):
  - writes `ci_default_state`, `ci_default_sha`, `ci_default_at` and `ci_red_since_sha` on the land's project bead;
  - posts a message in the land's `#machines` with author `github-bridge` and origin `ci_default_red` or `ci_default_green`.

  That message must wake the steward. The CI wake replaces a cron: tenants stay event-driven (§2.4).
- **On a red, the steward:**
  - attributes it: "main red since `<sha>`, merges `<range>`, test `<name>`", written to `#machines` and the digest;
  - classifies it: caused by a merge in the batch gets a fix step; flaky or unrelated gets a row on the `gardenio/quarantine` branch with a bug bead;
  - never reruns.
- **It keeps the list:** it prunes rows whose fix merged, and nags on expired rows in the digest.

The quarantine row format is gardenio's: `kind<TAB>name<TAB>bead<TAB>added<TAB>expires<TAB>evidence`. The kinds are `go`, `golden`, `pytest`, `jest`, `vitest` and `cargo`, validated by `lands/ci/kit/quarantine.sh` (Task 1.2). The steward's playbook is `StewardPlaybookPrompt` (`internal/steward/steward.go`). Its wake entries are in `internal/steward/bind.go` (`wakeTriggers`, :163-212; `UpgradeWakes`, :232).

**Files:**
- Modify `gardenio/controller/internal/steward/bind.go`:
  - add `WakeCI = "ci-default-branch"`, a tenant-only bead-event entry on `created` messages in the land with `author == "github-bridge"` and `origin contains "ci_default_"`, placed before the tail;
  - the `machines-delta` entry gains `not(field("origin", "contains", "ci_default_"))` so it does not wake twice;
  - `UpgradeWakes` adds `WakeCI` to an already-bound steward, as it added person-joined (see `gardenio/docs/lands/steward.md`, "A steward bound before step 9.14").
- Modify `gardenio/controller/internal/steward/steward.go`:
  - `Land` gains `CIState`, `CISHA`, `CIRedSince` and `CIAt`, read from the project bead's fields where `Land` is built (`bind.go:363` area). The prompt shows a "CI on <default branch>" line when they are set.
  - New tenant duty "7. WATCH CI", after duty 6 (Task 2.3):
    ```
    7. WATCH CI. On the {{wakeCI}} wake, and on the {{wakeDigest}} wake while CI
       on the default branch is red:
       - Read why. List the red sha's gardenio-ci runs and their failing tests:
           GH_TOKEN=$(cat /etc/git-creds/github/token) gh api repos/{{.Repo}}/commits/<sha>/check-runs \
             --jq '.check_runs[] | select(.name|startswith("gardenio-ci/")) | [.id,.name,.conclusion] | @tsv'
           GH_TOKEN=$(cat /etc/git-creds/github/token) gh api repos/{{.Repo}}/check-runs/<id>/annotations \
             --jq '.[] | select(.title=="gardenio-ci failing test") | .message'
       - Attribute it. The batch is git log --first-parent --oneline <last green sha>..<red sha>.
           gb say --channel machines "<branch> red since <sha7>, merges <first>..<last>, test <name>"
         and note it under In flight.
       - Classify each failing test. If its file or package changed in the batch, the
         batch broke it: write a one-step plan "Fix <test>" with duty 2's gb lands plan
         write, naming the merge. Otherwise it is flaky or unrelated: file a bug,
           kd create --type bug --project {{.Name}} --title "<kind> <name> fails on <branch>" --short-title "fix flaky <name>"
         and quarantine it:
           gb lands ci quarantine {{.Name}} --dir . --add "<kind> <name>" --bead <bug id> --evidence <run url>
       - Never re-run anything: you cannot, and a rerun is not evidence.
       On the {{wakeCI}} wake for green, move the In flight line to Answers.
       KEEP THE LIST on the {{wakeDigest}} wake: for each row on gardenio/quarantine
       whose bug bead is closed,
           gb lands ci quarantine {{.Name}} --dir . --remove "<kind> <name>"
       and list rows past their expiry in the digest as "expired: <kind> <name> (<bug>)".
       An expired row is a nag, never a gate.
    ```
- Modify `gardenio/controller/cmd/gb/lands_ci.go`. Add `gb lands ci quarantine <land> --dir <checkout> (--add "<kind> <name>" --bead <id> --evidence <url> | --remove "<kind> <name>" | --list) [--json]`:
  - it works in a separate worktree: `git worktree add <tmp> origin/gardenio/quarantine`, or an orphan branch when absent, seeded with the default branch's `.github/quarantine.tsv` header only;
  - it validates the row with `kit.ValidRow(kind, name, bead)` (below), rejecting it with the pattern;
  - `--add` appends `kind name bead <today> <today+14d> evidence`, with dates in UTC;
  - `--remove` deletes the row;
  - it commits `quarantine: add|remove <kind> <name> (<bead>)` and runs `git push origin gardenio/quarantine`. It never touches the default branch;
  - a duplicate add is a no-op;
  - `--list` prints the rows with `expired` flags.
- Modify `lands/ci/kit.go`: add `ValidRow(kind, name, bead string) error` with exactly the patterns of `lands/ci/kit/quarantine.sh`. Create `lands/ci/quarantine_parity_test.go`, which runs `kit/quarantine.sh fetch` over `testdata/quarantine/rows.tsv` (good and bad rows of every kind) and requires the kept set to equal the rows `ValidRow` accepts.
- Tests:
  - `bind_test.go`: the tenant has `WakeCI`; Home does not; `UpgradeWakes` adds it once; `machines-delta` excludes the CI origins.
  - `steward_test.go`: duty 7 is in the tenant prompt only.
  - `lands_ci_test.go`: quarantine add, remove, duplicate and invalid against a temp origin repo; the branch is created when absent; the default branch is untouched (assert its sha).
  - `quarantine_parity_test.go`.
- Modify `gardenio/docs/lands/steward.md`: the CI wake, the watch duty, and the `gardenio/quarantine` branch.

**Steps:**
- [ ] Step 1: tests; they fail. Step 2: implement. Step 3: docs. Step 4: `gb pr open`.

**Narrow test:**
```bash
cd gardenio/controller && go test ./internal/steward/ -run 'TestWakeTriggers|TestUpgradeWakes|TestRenderPrompt' -count=1
cd gardenio/controller && go test ./cmd/gb/ -run 'TestLandsCI' -count=1
cd lands/ci && GOWORK=off go test -count=1 -run 'TestQuarantineParity|TestValidRow' ./...
```

**Acceptance:** tests are green in CI, and a bound steward on a provisioned land gains the `ci-default-branch` entry on the provisioner's next pass, with no rebind.

**Rules for this task (from the plan's Global Constraints):**
- One PR, opened as a draft with `gb pr open`; run `gb pr ready` once when done. PRs get the light tier only.
- Never retrigger CI: no empty commits, no merging main in, no "Update branch", no `gh run rerun`, no draft/ready flips.
- Breaking main is not taboo: a red default branch is attributed, then quarantined or fixed. It is not an emergency, and no PR-time guard is added.
- No bake-in periods: expiry is a nag, never a gate.
- Tenant CI is GitHub-hosted only.
- Cheap is on us: quarantining a flaky test stops it costing every PR.
- Never modify a tenant repo without the person's approval: the steward writes only the `gardenio/quarantine` branch the onboarding decision named, and writes fixes only as approved plan steps.
- The Lands App gains no permission.
- Test narrowly with the commands above. Never run `go test ./...` at the `gardenio/controller` root.

### Task 3.4: the fleet-wide CI cost read and the soft cap

**Who:** CREW (one PR: `internal/steward`, `cmd/gb`, kbeads schema, `lands/ci/README.md`).

**Why:** CI onboarding spec §5.1, "Cheap is on us", is binding (owner: "keep it on the target repo's dime but keep cheap that's on us"):
- The tenant pays for minutes, but gardenio owns how many minutes its defaults burn.
- Gardenio measures them: the digest's CI-minutes line is read fleet-wide as well as per land, and the steward flags the most expensive unit or job and any re-run storm.
- Gardenio fixes them: a kit cost regression is a gardenio bug under the CI slimming plan gd-WFMvg9sDKE, and the tenant is not asked to fix it.

Spec §5 adds a land field `ci_minutes_soft_cap`, default unset. Past it, the steward says so; it informs and never blocks a run. The digest's CI line already sums wall minutes since the last digest (`steward.DigestCI`, Task 2.4). `gb lands digest --post` records the digest's mark on the steward's notes bead (`RecordDigest`, `internal/steward/digest.go`, fields `digest_at` and `digest_used_credits` on `type:doc`, declared in `kbeads/internal/server/config.go:2117-2130`).

**Files:**
- Modify `gardenio/controller/internal/steward/digest.go`:
  - `DigestCI` gains `Reruns int`, the number of (check name, sha) pairs with more than one completed `gardenio-ci/*` run in the window;
  - `RecordDigest` also records:
    - `digest_ci_minutes` (this digest's `Minutes`);
    - `digest_ci_month` (UTC `YYYY-MM` of as-of);
    - `digest_ci_minutes_month`: the previous total when the month is the same, otherwise 0, plus `Minutes`;
    - `digest_ci_top` (`Top`);
    - `digest_ci_reruns`;
  - `GatherDigest` reads the previous month total and the land's `ci_minutes_soft_cap`. When `month total + Minutes > cap`, the CI line ends with `; CI used <n> of the <cap> runner-minutes you set for <month>`. Reruns > 0 adds `; <n> re-runs`.
- Modify `kbeads/internal/server/config.go`:
  - `type:doc` gains `digest_ci_minutes`, `digest_ci_month`, `digest_ci_minutes_month`, `digest_ci_top` and `digest_ci_reruns`, all strings, as the existing digest fields are; extend its comment;
  - `type:project` gains `{"name": "ci_minutes_soft_cap", "type": "integer", "min": 0}`.
- Modify `gardenio/controller/internal/bridge/init_configs.yaml`: `ci_minutes_soft_cap` in `type:project`.
- Modify `gardenio/controller/cmd/gb/lands_ci.go`. Add `gb lands ci cost [--json]`: one row per tenant land, read from each land's steward notes bead, with:
  - the month;
  - the month minutes;
  - the last digest's minutes;
  - the cap;
  - the top check;
  - the reruns.

  Rows are sorted by month minutes, descending. The output ends with `A kit cost regression is a gardenio bug: file it under gd-WFMvg9sDKE.` It reads beads only, with no GitHub call.
- Modify `lands/ci/README.md`: a "Cost policy" section with the defaults (changed units on PRs; full on push and daily; cancel-in-progress; no matrix; slow path gates; timeouts 15/45), the soft cap, and the rule that a regression is a gardenio bug.
- Tests:
  - `digest_test.go`: month roll-over; same-month accumulation; cap exceeded and not exceeded; reruns counted; `RecordDigest` writes the five fields.
  - `lands_ci_test.go`: `cost` ordering and output with a fake daemon.
  - kbeads: the builtin declares the new fields.

**Steps:**
- [ ] Step 1: tests; they fail. Step 2: implement. Step 3: README. Step 4: `gb pr open`.

**Narrow test:**
```bash
cd gardenio/controller && go test ./internal/steward/ -run 'TestDigest|TestRecordDigest|TestGatherDigest' -count=1
cd gardenio/controller && go test ./cmd/gb/ -run 'TestLandsCI' -count=1
cd kbeads && go test ./internal/server/ -run 'TestProjectSchema|TestBuiltin|TestEnsureConfigs' -count=1
```

**Acceptance:** tests are green in CI, and `gb lands ci cost` lists every provisioned tenant land, with crab-shack-3.5 among them once it has posted a digest after this rolls.

**Rules for this task (from the plan's Global Constraints):**
- One PR, opened as a draft with `gb pr open`; run `gb pr ready` once when done. PRs get the light tier only.
- Never retrigger CI: no empty commits, no merging main in, no "Update branch", no `gh run rerun`, no draft/ready flips.
- Breaking main is not taboo: fix forward or quarantine. Add no PR-time guard.
- No bake-in periods: the soft cap informs and never blocks.
- Tenant CI is GitHub-hosted only.
- Cheap is on us, and this task is the measuring half of that.
- Never modify a tenant repo.
- The Lands App gains no permission.
- Test narrowly with the commands above. Never run `go test ./...` at the `gardenio/controller` or `kbeads` root.

### Task 3.5: Phase 3 gate — update the live Lands App, roll, and see a red default-branch run wake the steward

**Who:** OPERATOR, with the PERSON (the owner as `fimbazland`) for the tenant-repo change. No PR, except the deploy-values flip.

**Why:** "A red default-branch run on a tenant wakes its steward with no cron involved" (CI onboarding spec §6, Phase 3 gate). This is also the operator half of spec step 3.1: update the App, after first checking whether existing installations must re-accept.

**Preconditions:** Tasks 3.1, 3.2 and 3.3 are merged and rolled in a release window.

**Steps:**
- [ ] 1. Generate the Lands webhook secret and store it in SSM (profile `gardenio`, region `us-east-1`; never echo it):
  `aws ssm put-parameter --profile gardenio --region us-east-1 --type SecureString --name /gardenio/github-app/lands/webhook-secret --value "$(openssl rand -hex 32)"`
  Afterwards, say that this created the parameter.
- [ ] 2. In `deploy-values/gardenio-k3s.yaml`, through a normal PR (a deploy-values flip that adds objects goes through CI), set `githubBridge.landsWebhookSecret.enabled: true` with `externalSecret.enabled: true` and the cluster's secret store, as the fleet `webhookSecret` uses. Roll it in the next window.
- [ ] 3. Before saving anything in the Lands App's settings, check that no permission changes. The diff is only the webhook and the "Check suite" event. GitHub asks installers to re-approve only for permission changes. Then:
  - set Webhook active;
  - set the URL `https://<githubBridge.ingress.host>/webhook/lands`;
  - set the secret: read it with `aws ssm get-parameter --with-decryption` and paste it without printing it to a log;
  - tick "Check suite";
  - save.

  Confirm that the crab-shack installation shows no pending permission request.
- [ ] 4. Use the App settings' "Recent Deliveries" (or the bridge's `lands_` delivery counter) to check the next `check_suite` delivery returns 2xx.
- [ ] 5. The PERSON, as `fimbazland`, pushes to crab-shack's default branch one commit that adds a shell file with a syntax error, for example `printf 'if then\n' > smoke-red.sh`. The smoke tier turns `gardenio-ci/full` red.
- [ ] 6. Evidence, read right away, with no waiting on a clock:
  - the land's project bead has `ci_default_state=red` and `ci_red_since_sha=<sha>`;
  - the land's `#machines` has the bridge's `ci_default_red` message;
  - the steward ran with wake entry `ci-default-branch`: the trigger bead's `last_outcome` and the new agent bead show it, at a time that is not the digest's cron minute;
  - its `#machines` attribution line names the commit.
- [ ] 7. The PERSON reverts the commit. Check that a `ci_default_green` message and a green wake followed.
- [ ] 8. Comment on the plan epic: `laptop-claude: Phase 3 gate: red <sha7> → steward woke via ci-default-branch at <time> (agent <id>); green again at <sha7>.` Close this task.

**Acceptance:** the evidence comment exists, and it names the wake entry and the agent bead.

**Rules:**
- No bake-in: proceed on the evidence.
- Never modify the tenant repo as the operator; the red commit and its revert are the person's.
- The Lands App's permissions stay exactly as they are.
- Name the AWS profile and region of every AWS command, and say what was changed.
- No local Go tests, builds or Terraform on the laptop.

---

## Phase 4: merges and releases (gate: one tenant release cut on demand and recorded, and one merge under `merge-on-green` audited in `#machines`)

### Task 4.1: the land's grant, its `land_steward_approves` and the steward playbook say the same thing

**Who:** CREW (one PR: kbeads, `internal/steward`, docs).

**Why:** The three disagree today (CI onboarding spec §1, "Merge policy"):
- The tenant playbook says "You do not merge it: the land's people review and merge on GitHub", and its Never list has "Merge a PR, approve anything, or write a grant" (`gardenio/controller/internal/steward/steward.go`, duty 3 and `## Never`).
- kbeads stamps every new tenant land with `land_steward_approves=["github_pr_merge"]` (`kbeads/internal/server/land_steward_delegation.go:36`, owner decision gd-fBSlZfEvmo). `landDelegatesToSteward` (`escalation_lane_members.go:66-81`) staffs the steward lane from that list alone.
- The greeting says the steward approves merges whenever that list has `github_pr_merge` (`StewardApprovesMerges`, set at `bind.go:363`).
- `steward_authority` (`ask`, `merge-on-green`, `operate`; unset reads as `ask`, `ParseAuthority`) is read only by the greeting.

The owner kept `ask` as the default grant (spec Q5). Plan decision D7: the merge delegation takes effect only at `merge-on-green` or `operate`. Under `ask` a gardener's merge goes to the land's people.

Spec §6 step 4.1 is: "Make the playbook's Never list, the greeting and `land_steward_approves` agree. Then wire `merge-on-green` to the light check." This task is the first half; Task 4.2 is the second.

**Files:**
- Modify `kbeads/internal/server/escalation_lane_members.go`, `landDelegatesToSteward` (:66-81). For a merge operation type (`stewardDelegableMerges`, `land_steward_delegation.go:40`), also require the land's `steward_authority` to be `merge-on-green` or `operate`. Unset or anything else reads as `ask`, mirroring the controller's `ParseAuthority`. Non-merge operation types are unchanged. Update its doc comment.
- Modify `kbeads/internal/server/land_steward_delegation.go`: the comment on `defaultLandStewardApproves` (:31-36) says the list is the owner's delegation and the grant is the gate (D7). The value stays `["github_pr_merge"]`.
- Modify `gardenio/controller/internal/steward/bind.go:363`: `StewardApprovesMerges: ParseStewardApprovesMerges(...) && ParseAuthority(proj.Fields[AuthorityField]) != AuthorityAsk`.
- Modify `gardenio/controller/internal/steward/steward.go`, tenant prompt:
  - duty 3's sentence "You do not merge it: the land's people review and merge on GitHub." stays under `ask`. When `.StewardApprovesMerges`, it becomes: "When its gardener asks to merge, the ask comes to you: duty 3b says how you answer it."
  - Add duty "3b. MERGE ON GREEN" inside `{{if .StewardApprovesMerges}}`, with this text: "A step's merge escalation is held by you. Read the diff (`GH_TOKEN=$(cat /etc/git-creds/github/token) gh pr diff <n> -R {{.Repo}}`). If it does what the step says and nothing else, approve it with `gb escalation approve <id>`. Otherwise answer it with what is wrong and do not approve. Then say `gb say --channel machines "merged #<n> under merge-on-green"`." Task 4.2 adds the green check.
  - The tenant Never line "Merge a PR, approve anything, or write a grant." becomes, when `.StewardApprovesMerges`: "Merge a PR yourself, approve anything but a step's merge as duty 3b says, or write a grant." Under `ask` it is unchanged.
- Tests:
  - kbeads: add `TestLandDelegatesToSteward_MergeNeedsGrant`. With `land_steward_approves=["github_pr_merge"]`: `ask` and unset give false; `merge-on-green` and `operate` give true; a non-merge delegable type is unaffected by the grant. Update the existing lane tests that assumed the old behaviour, and say which in the PR.
  - controller `steward_test.go`: extend `TestRenderPrompt_GreetingUnderEachGrant` (:223) and `TestRenderPrompt_GreetingSaysStewardApprovesMerges` (:197). Under `ask` with the delegation: no merge line in the greeting, no duty 3b, the old Never line. Under `merge-on-green`: all three changed.
- Modify `gardenio/docs/lands/steward.md`, "### The grant" (:351-357): replace "Duty 3 and the "Never" list still state the `ask` level whatever the grant…" with the D7 rule.

**Steps:**
- [ ] Step 1: tests; they fail. Step 2: implement. Step 3: docs. Step 4: `gb pr open`.

**Narrow test:**
```bash
cd kbeads && go test ./internal/server/ -run 'TestLandDelegatesToSteward|TestEscalationLane|TestLaneStaffed|TestStewardDelegation' -count=1
cd gardenio/controller && go test ./internal/steward/ -run 'TestRenderPrompt|TestBind' -count=1
```

**Acceptance:**
- Tests are green in CI.
- On a land at `ask` with the default delegation, a gardener's `github_pr_merge` escalation is no longer staffed by the steward lane; it goes to the land owner lane. A kbeads test asserts it.

**Rules for this task (from the plan's Global Constraints):**
- One PR, opened as a draft with `gb pr open`; run `gb pr ready` once when done. PRs get the light tier only.
- Never retrigger CI: no empty commits, no merging main in, no "Update branch", no `gh run rerun`, no draft/ready flips.
- Breaking main is not taboo: fix forward or quarantine. Add no PR-time guard.
- No bake-in periods.
- Tenant CI is GitHub-hosted only.
- Cheap is on us.
- Never modify a tenant repo without the person's approval. Under `ask`, every merge is the person's.
- The Lands App gains no permission.
- Test narrowly with the commands above. Never run `go test ./...` at the `kbeads` or `gardenio/controller` root.

### Task 4.2: merge-on-green is gated on a green `gardenio-ci/light` run at the PR head

**Who:** CREW (one PR: kbeads approval path, the merge executor, `internal/steward`).

**Why:** "Green" has to mean something before a steward merges on it (CI onboarding spec §2.5).
- A tenant PR's check is `gardenio-ci/light`. It concludes `success` unless a non-quarantined test failed, because quarantined failures never fail the job (plan decision D4).
- Task 3.2 makes the github-bridge stamp, on the land's step (task) bead whose `mr_url` is the PR, the fields `mr_pipeline_status` (`success`, `failed`, …), `mr_pipeline_url` and `mr_pipeline_sha` (the suite's head sha).
- Task 4.1 lets a steward approve a step's `github_pr_merge` escalation only at `merge-on-green` or `operate`.

This task adds the green gate in the daemon, which a prompt cannot skip. It also pins the merge to the sha that went green, so a push after the green run makes the merge fail instead of merging untested code. Spec §6 step 4.1, second half: "wire `merge-on-green` to the light check".

**Files:**
- Modify kbeads, the escalation approve path. Find where an approval through the steward lane (`laneSteward`, `kbeads/internal/server/escalation_lane_members.go`) is authorised: `grep -n 'laneSteward' kbeads/internal/server/*.go`. When the lane is the steward's and the operation type is `github_pr_merge` on a tenant land:
  - find the land's task bead whose `mr_url` equals the escalation's `operation_spec.mr_url` (the merge spec carries `mr_url`; see `fillMergeSpecURL` in `gardenio/controller/cmd/gb/escalation_merge_dedupe.go`);
  - require `mr_pipeline_status == "success"` and a non-empty `mr_pipeline_sha`;
  - otherwise refuse with 409 and error code `merge_not_green`: `<mr_url> has no green gardenio-ci run recorded on its head`;
  - on success, record `operation_spec.expected_head_sha = mr_pipeline_sha` with the approval, in the same write.

  A land owner's own approval is NOT gated: the person decides.
- Modify the `github_pr_merge` executor. Find it with `grep -rln 'github_pr_merge' gardenio/controller/internal kbeads/internal --include=*.go`, and look for the code that calls GitHub's merge. When `operation_spec.expected_head_sha` is set, pass it as the REST merge's `sha` parameter (`PUT /repos/{o}/{r}/pulls/{n}/merge`, "SHA that pull request head must match to allow merge"), or as `gh pr merge --match-head-commit`. A mismatch fails the execution with that reason. If the executor has no head-sha input yet, add it.
- Modify `gardenio/controller/internal/steward/steward.go`, duty 3b (from Task 4.1). Before approving, check `GH_TOKEN=$(cat /etc/git-creds/github/token) gh pr checks <n> -R {{.Repo}}` shows `gardenio-ci/light` pass on the PR's current head (`gh pr view <n> --json headRefOid`). If the daemon answers `merge_not_green`, say in `#machines` that the PR is not green, and do not retry until the step's gardener pushes again.
- Tests:
  - kbeads (`TestStewardMergeApproval_RequiresGreen`):
    - no task with that `mr_url`: refused;
    - status `failed`: refused;
    - status `success` with no sha: refused;
    - `success` with a sha: approved, and `expected_head_sha` recorded;
    - a land owner's approval with status `failed`: allowed.

    The last case is the positive control that the gate is the steward lane's only.
  - executor test: `expected_head_sha` reaches the merge call, and a mismatch fails.
  - `steward_test.go`: duty 3b has the `gh pr checks` line.

**Steps:**
- [ ] Step 1: tests; they fail. Step 2: implement. Step 3: `gb pr open`.

**Narrow test:**
```bash
cd kbeads && go test ./internal/server/ -run 'TestStewardMergeApproval|TestEscalationApprove' -count=1
cd gardenio/controller && go test ./internal/steward/ -run 'TestRenderPrompt' -count=1
```
For the executor, run its package's test with `-run` on the new test name; name the package in the PR.

**Acceptance:** tests are green in CI. The PR body names the executor package and shows the merge call carrying the sha.

**Rules for this task (from the plan's Global Constraints):**
- One PR, opened as a draft with `gb pr open`; run `gb pr ready` once when done. PRs get the light tier only.
- Never retrigger CI: no empty commits, no merging main in, no "Update branch", no `gh run rerun`, no draft/ready flips. The steward is told the same.
- Breaking main is not taboo. This gate is about a steward merging on its own say-so, not a PR-time guard on main: the person's own merges are not gated.
- No bake-in periods.
- Tenant CI is GitHub-hosted only.
- Cheap is on us.
- Never modify a tenant repo without the person's approval: the merge happens only under the grant the person set.
- The Lands App gains no permission. The merge executor's App rights are unchanged.
- Test narrowly with the commands above. Never run `go test ./...` at the `kbeads` or `gardenio/controller` root.

### Task 4.3: the kit's `release-cut.sh`, a newest-green walk that takes the repo and the check as inputs

**Who:** CREW (one PR in `lands/ci`).

**Why:** Releases are cheap and on demand, with no cadence (CI onboarding spec §2.7, the owner's release ruling). "`gl release cut`, or the steward, tags the newest default-branch commit whose full run is green or red only on quarantined rows". That is gardenio's `release-cut.sh` newest-green walk (`gardenio/scripts/release-cut.sh`), whose `REPO` is hardcoded (:24) and which reads the Actions API.

A tenant's token has `checks: read` but no `actions` permission, so the kit's walk reads check runs instead. The kit's full-run check is `gardenio-ci/full`, and a quarantined failure never fails it, so "green or red only on quarantined rows" is exactly a `success` conclusion. The tag push then runs the caller's release tier (`.gardenio/ci/run.sh release`, Task 1.3):
- the units changed since the previous tag, plus their `release` command;
- slow units only if their paths changed;
- promote, don't rebuild.

**Files:**
- Create `lands/ci/kit/release-cut.sh --repo <owner/repo> --check <name> --glob <tag glob> [--branch <b>] [--max-scan N] [--dry-run]`. Bash, with `gh` and `git`, run in a checkout of the repo:
  1. `git fetch origin <branch> --tags`.
  2. Find the previous tag: `git describe --tags --abbrev=0 --match '<glob>' origin/<branch>`, or none.
  3. List the candidates: `git rev-list --first-parent --max-count=<N, default 30> <prev>..origin/<branch>` (all of `origin/<branch>` when there is no previous tag), newest first. None means exit 3, "nothing to release".
  4. For each candidate: `gh api "repos/<repo>/commits/<sha>/check-runs?check_name=<check>&per_page=10"`. Take the newest completed run by `completed_at`; it is green when its conclusion is `success`. A commit with no run is passed over for an older green one. The first green candidate wins. None means exit 3.
  5. Compute the next tag:
     - `v*.*.*`: `vX.Y.Z` becomes `vX.Y.(Z+1)`; with no previous tag, `v0.1.0`.
     - `20*.*.*`: `YYYY.DDD.N`, today's UTC year and day-of-year, with N one more than the highest tag of that day, starting at 1.
     - Any other glob exits 1 and names it.
  6. `git tag -a <tag> <sha> -m "release <tag> (gardenio-ci: <check> green at <sha7>)"` and `git push origin <tag>`. `--dry-run` prints the tag and sha and pushes nothing.

  Exit codes: 0 tagged (or would tag), 3 nothing to release (normal), 1 error naming the failed command. It never pushes a branch.
- Create `lands/ci/release-cut_test.sh`. It puts a fake `gh` on `PATH` that answers check-runs per sha from fixture files, and uses a temp origin repo. Cases:
  - **positive control:** a green commit behind a newer red one is chosen;
  - a commit with no run is skipped;
  - nothing newer than the tag exits 3;
  - red only exits 3;
  - no previous tag gives `v0.1.0`;
  - `v1.2.3` gives `v1.2.4`;
  - calver numbering within a day;
  - `--dry-run` pushes nothing (the origin has no new tag);
  - an unknown glob exits 1.
- Modify `lands/ci/kit.go`: add `release-cut.sh` to the files `Render` writes under `.gardenio/ci/`. `Version` changes, so the steward's KEEP THE KIT CURRENT duty (Task 2.3) brings it to every land.
- Modify `lands/ci/kit_test.go` for the file set and the golden's `Version` line, and refresh the golden.
- Modify `lands/ci/README.md`: a "Releases" section on cutting, the release tier, and promote-not-rebuild. A unit's `release` command should retag an artifact the full run built (for example `docker buildx imagetools create -t <image>:<tag> <image>:sha-<sha>`) rather than rebuild it.
- Modify the `lands-ci-kit` job in `.github/workflows/checks.yml`: add `bash lands/ci/release-cut_test.sh`, and add both scripts to the job's shellcheck call.

**Steps:**
- [ ] Step 1: the test; it fails. Step 2: the script. Step 3: Render and README. Step 4: `gb pr open`.

**Narrow test:**
```bash
bash lands/ci/release-cut_test.sh
shellcheck -x lands/ci/kit/release-cut.sh lands/ci/release-cut_test.sh
cd lands/ci && GOWORK=off go test -count=1 ./...
```

**Acceptance:** `lands-ci-kit` is green on main with the release-cut cases.

**Rules for this task (from the plan's Global Constraints):**
- One PR, opened as a draft with `gb pr open`; run `gb pr ready` once when done. PRs get the light tier only.
- Never retrigger CI: no empty commits, no merging main in, no "Update branch", no `gh run rerun`, no draft/ready flips.
- Breaking main is not taboo: the walk simply passes over red commits. Add no PR-time guard.
- No bake-in periods: a release is cut on demand, never after a waiting period.
- Tenant CI is GitHub-hosted only.
- Cheap is on us: the walk reads check runs and never triggers a run, and the release tier builds only changed units.
- Never modify a tenant repo. The script runs only where a person or the steward runs it (Task 4.4).
- The Lands App gains no permission; `checks: read` and `contents: write` are enough.
- Test narrowly with the commands above.

### Task 4.4: the steward's release duty, and `gl release cut`

**Who:** CREW (one PR: `internal/steward`, `lands/gl`).

**Why:** A land releases whenever a person, or the steward under `operate`, asks (CI onboarding spec §2.7, §4.5; owner: releases cheap and on demand).
- `gl` holds no GitHub credential and talks only to the land door (`lands/gl/README.md`). So `gl release cut` asks the steward by opening a ticket titled exactly `Cut a release` (plan decision D8; the contract's ticket kinds are `task`, `bug` and `feature`).
- The steward cuts with the vendored `.gardenio/ci/release-cut.sh --repo <repo> --check gardenio-ci/full --glob <glob>`, from Task 4.3, using its Lands installation token at `/etc/git-creds/github/token`. The token has `contents: write`, which can push a tag.
- The tag push runs the release tier, and the github-bridge's tag poll records the release (Phase 0).
- The steward's Never list (Task 2.3) forbids pushing a tag. This task makes the one exception.

**Files:**
- Modify `gardenio/controller/internal/steward/bind.go`:
  - add `WakeReleaseAsk = "release-ask"`, a tenant-only entry on `created` beads in the land with `type == task` and `title == "Cut a release"`;
  - `UpgradeWakes` adds it to bound stewards.
- Modify `gardenio/controller/internal/steward/steward.go`:
  - Add tenant duty "8. RELEASE" (the `<glob>` is `kit.TagGlob` of the land's `release_tag_pattern`; render it into the prompt from the project bead):
    ```
    8. RELEASE. On the {{wakeReleaseAsk}} wake, or when a person asks in a thread to
       release, cut one; under the operate grant you may also cut when a plan you
       ran has finished and the default branch is green. Never on your own
       initiative under ask or merge-on-green.
         git fetch origin --tags
         GH_TOKEN=$(cat /etc/git-creds/github/token) .gardenio/ci/release-cut.sh \
           --repo {{.Repo}} --check gardenio-ci/full --glob '{{.TagGlob}}'
       Exit 0: say "released <tag> from <sha7>" in #general, and close the ticket
       with that line. Exit 3: say there is nothing new and green to release, and
       close the ticket with why. Anything else: say what failed and leave the
       ticket open.
    ```
  - The Never line from Task 2.3 changes to `- Push to .github/workflows or to the default branch, or push a tag except by duty 8.`
- Modify `lands/gl/releases.go`. Add `releaseCut(a *app)`, `Use: "cut [--note text]"`, to `newReleaseCmd` (:18-26):
  - it opens a ticket through the contract (`kind: task`, `title: "Cut a release"`, `body`: the note, or `Asked from gl release cut.`);
  - it prints `asked your steward to cut a release: ticket <id>; it tags the newest commit whose gardenio-ci/full run is green`;
  - `--json` prints the ticket.
- Modify `lands/gl/README.md`: add `gl release cut` to the Commands block.
- Tests:
  - `bind_test.go`: the tenant has `release-ask`; Home does not; the upgrade adds it.
  - `steward_test.go`: duty 8 has the land's repo and glob; the Never line names duty 8.
  - `lands/gl/releases_test.go`: `cut` posts exactly that ticket to the fake land and prints its id.

**Steps:**
- [ ] Step 1: tests; they fail. Step 2: implement. Step 3: README. Step 4: `gb pr open`.

**Narrow test:**
```bash
cd gardenio/controller && go test ./internal/steward/ -run 'TestWakeTriggers|TestUpgradeWakes|TestRenderPrompt' -count=1
cd lands/gl && GOWORK=off go test -count=1 -run 'TestRelease' ./...
```

**Acceptance:** tests are green in CI, and `gl release --help` lists `cut`.

**Rules for this task (from the plan's Global Constraints):**
- One PR, opened as a draft with `gb pr open`; run `gb pr ready` once when done. PRs get the light tier only.
- Never retrigger CI: no empty commits, no merging main in, no "Update branch", no `gh run rerun`, no draft/ready flips.
- Breaking main is not taboo: the cut takes the newest green commit, so a red tip does not block a release.
- No bake-in periods.
- Tenant CI is GitHub-hosted only.
- Cheap is on us.
- Never modify a tenant repo without the person's approval: a tag is pushed only on a person's ask, or under the `operate` grant they set.
- The Lands App gains no permission.
- Test narrowly with the commands above. Never run `go test ./...` at the `gardenio/controller` root.

### Task 4.5: Phase 4 gate — one tenant release cut on demand and recorded, and one merge under `merge-on-green` audited in `#machines`

**Who:** PERSON (the owner as `fimbazland`) with the OPERATOR. No PR.

**Why:** This is the CI onboarding spec's Phase 4 gate, and the end of this plan.

**Preconditions:**
- Tasks 4.1-4.4 are merged and rolled.
- For the merge half, the crab-shack land must be able to run a lands-gardener: tenant spawns on (first-tenant gates gd-bmsQ7FT3oj, gate 4), and a purse with credits.

**Steps:**
- [ ] 1. **Release.** The PERSON runs `gl release cut --land <land>` on the Vultr host, or writes "please cut a release" in the land's `#general`. Evidence:
  - the steward's `released <tag> from <sha7>` line in `#general`;
  - a `gardenio-ci/release` run on the tag;
  - `kd release list --project <land>` lists the tag.
- [ ] 2. **Grant.** The PERSON raises the land's grant to `merge-on-green`, through the flow the greeting names for changing it (`changeAuthority` in the steward prompt). The operator does not set it for them.
- [ ] 3. **Merge.** The PERSON asks the steward in `#general` for a small change, then approves the plan's decision. The step's gardener opens a PR, and `gardenio-ci/light` runs on it. The gardener files its merge escalation, and the steward approves it under duty 3b. Evidence:
  - the steward's `merged #<n> under merge-on-green` line in `#machines`;
  - the escalation's `operation_spec.expected_head_sha` equals the PR's merged head.
- [ ] 4. Comment on the plan epic: `laptop-claude: Phase 4 gate: release <tag> (<release bead>, run <url>); merge #<n> under merge-on-green (<escalation id>, #machines <message id>).` Close this task.

**Acceptance:** both pieces of evidence are in the comment.

**Rules:**
- No bake-in: proceed on the evidence.
- Never modify the tenant repo or its grant as the operator; the person asks, approves and sets the grant.
- No local Go tests, builds or Terraform on the laptop.
