# CI minutes slimming: full CI once a PR is ready, batched main runs, scoped repo-wide checks, one gate, a lighter browser job — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Cut CI runner-minutes by 45-60%, from about 23,400 a day (measured 2026-10-27) to about 10,000-13,000. That would let the CI pool average 2.5-3 nodes instead of 5.4, stay under `ci_max_nodes = 6` without queueing, and save roughly $120-170 a month of spot compute. Every phase ends with a measured before/after, using the same method as Phase 0.

**Owner direction (chat, 2026-10-28):** "do you think maybe it's time to do a new plan to slim down our CI runner minute use? efficiency and all". The owner was shown the 10-27 numbers and the five levers below and answered "agree", then asked how to enforce "heavy jobs only once marked ready": "that's a great idea but we need to institute it somehow". Phase 1 is that enforcement. The context is a self-funded account with a $100-200/month elastic budget. The CI pool alone runs about $10/day (~$300/month) at the current spot price of about $0.08/h per xlarge.

**Architecture:**
- **Phase 1, the ready gate.** GitHub's draft flag is the switch. Draft PRs cannot be merged, so a draft can never get past full CI.
  - `ci.yml`, `checks.yml` and `images.yml` add `ready_for_review` to their `pull_request` types and gate every heavy job on `github.event.pull_request.draft != true`.
  - A draft gets a light tier instead: build and unit tests of the changed packages, aiming for 10 minutes or less.
  - Crews open PRs with `gb pr open`, which always makes a draft. They call `gb pr ready` once, which flips the PR and files the merge escalation.
  - `gb escalation create` refuses a merge escalation for a draft PR.
  - The github-bridge converts any non-draft PR opened from an `agent/` branch to a draft, as a backstop.
- **Phase 2, batched main runs.** `checks.yml` adopts the supersession guard that `ci.yml` already has (`.github/actions/superseded`). Then both workflows run at most one full main run at a time. The newest waiting run replaces older waiting ones, so the tip of main is always tested but the commits in between are not re-tested one by one. Before this lands, a task proves that release cutting and the promotion gate select "the newest commit with a green run", not "HEAD must be green".
- **Phase 3, scoped repo-wide checks.**
  - On PRs, gitleaks scans only the PR's own commits.
  - govulncheck and osv run when `go.mod`, `go.sum` or `Cargo.lock` change.
  - Terraform and the shell guards run only when their paths change.
  - A new nightly `schedule` run covers everything these gates skip.
  - Tag builds stay unchanged.
- **Phase 4, one gate per workflow.**
  - Measure what a gate job costs: pod start, checkout and script time.
  - Merge `ci.yml`'s two gate jobs (`supersession` and `changes`) into one, and give every gate a sparse checkout.
  - A decision bead then asks whether to fold the PR side of `checks.yml` and `images.yml` under one `ci.yml` gate, with `workflow_call`.
- **Phase 5, the browser job and keycloak-theme.**
  - Bake playwright-core and Chromium into the CI runner image.
  - Run `make test-garden-goldens` only when visual inputs change.
  - Make `keycloak-theme` (the only workflow on GitHub-hosted, billed `ubuntu-latest` minutes) exit in its first step when the diff does not touch Keycloak.
- **Phase 6:** measure the whole plan and report.

**Tech stack:** GitHub Actions (`dorny/paths-filter`, the repo's `.github/actions/superseded` composite, `workflow_call`, `schedule`), Go 1.25 (`gardenio/controller/cmd/gb`, `gardenio/controller/cmd/github-bridge`), Python 3 stdlib guards under `scripts/`, bash, AWS CLI (CloudWatch `AWS/AutoScaling`), `gh api`.

**Spec:** none separate. The owner's 2026-10-28 brief and the measurements below are the spec. Every number a task needs is copied into that task.

**Anchors:** verified against `main` at `b37058e69` (2026-10-27). Lines drift. If a cited line has moved, find it by the quoted text and say so in the PR.

---

## Measured baseline (2026-10-27, repo `mattbakerconsulting/gardenio`)

**Method (the Phase 0 script makes this repeatable):**
- All workflow runs created on 10-27 were listed in eight 3-hour windows, because the runs search API caps each query at 1,000 results: 1,917 runs. They were then grouped by workflow, event and conclusion.
- Jobs were fetched for a seeded random sample of 96 non-skipped runs: 25 each of `ci`, `images` and `checks`, plus all 21 `keycloak-theme` runs.
- Each job's runner-minutes is `completed_at - started_at`. Skipped jobs and jobs with no start time are excluded.
- Daily runner-minutes = the runs per day for each workflow and event × the sampled minutes per run.

**Runs on 10-27:**

| Workflow | Event | Runs | Notes |
|---|---|---|---|
| ci | pull_request | 218 | 99 distinct PR branches, so 2.2 pushes per PR; up to 7 on one PR |
| ci | push (main) | 73 | |
| checks | pull_request / push | 218 / 73 | |
| images | pull_request | 218 | `images.yml` does not run on main pushes, only on PRs and calver tags |
| keycloak-theme | pull_request | 21 | `runs-on: ubuntu-latest`, so these are GitHub-hosted, billed minutes |
| retry-lost-runners | workflow_run | 955 | 744 skipped; about 210 runner-minutes on `gardenio-k3s-light` |
| e2e | pull_request (labeled) | 137 | 136 skipped |

Conclusions across all runs: 845 success, 880 skipped, 100 cancelled, 92 failure. ci failed on 77 of 291 runs (26%).

**Runner-minutes per run (sampled):**

| Workflow / event | Min/run | Biggest jobs (min/run, and how many sampled runs had the job) |
|---|---|---|
| ci / pull_request | 37.5 | gardenhub browser 17.5 (ran on 15 of 19); build + test 12.9 (matrix); lands conformance 4.1; changed-path gate 2.5 |
| ci / push | 29.6 | gardenhub browser 17.1 (4 of 6); build + test 9.6; changed-path gate 2.3 |
| checks / pull_request | 31.4 | shell guards 6.5; golangci-lint 6.0; gitleaks history 4.4; govulncheck + osv 4.3; terraform 4.3; changed-path gate 2.2; lands/contract 1.1 |
| checks / push | 46.6 | govulncheck 8.6; helm lint + chart tests 7.7; shell guards 7.4; gitleaks 7.0; terraform 6.6; golangci 3.5; gate 2.5 |
| images / pull_request | 12.2 | controller-base 4.9; controller 4.5; changes 1.8; kbeads 1.0 |
| keycloak-theme / pull_request | 8.4 | theme screenshots + passkeys 8.4 (GitHub-hosted) |

**Daily runner-minutes, about 23,400 in total:**

| Source | Runs/day | Min/run | Min/day |
|---|---|---|---|
| ci on PRs | 218 | 37.5 | 8,200 |
| checks on PRs | 218 | 31.4 | 6,800 |
| images on PRs | 218 | 12.2 | 2,700 |
| checks on main | 73 | 46.6 | 3,400 |
| ci on main | 73 | 29.6 | 2,200 |
| keycloak-theme (GitHub-hosted) | 21 | 8.4 | 175 |

**The CI pool** (`gardenio-k3s-ci` ASG, CloudWatch `GroupInServiceInstances`, daily average and maximum):

| Day | 10-21 | 10-22 | 10-23 | 10-24 | 10-25 | 10-26 | 10-27 |
|---|---|---|---|---|---|---|---|
| Avg nodes | 5.92 | 5.25 | 4.04 | 2.34 | 5.09 | 5.95 | 5.39 |
| Max nodes | 13 | 6 | 6 | 6 | 6 | 6 | 6 |

The cap of 6 binds all day. On 2026-10-28 at about 02:15Z the owner raised `ci_max_nodes` 6 → 12 for 24 hours ("sez no room in cluster; can we enlarge for 24h?"); the revert is due at or after 2026-10-29 02:15Z. Instance types on 10-28: m6a, m6i, m7a, m7i, r5 and r5a, all xlarge. Spot price is $0.073-0.087/h for m5a and m6a xlarge in us-east-1.

**Findings that change the obvious plan:**
- `gardenhub browser` is already path-gated (`ci.yml:1062-1070`, `needs.changes.outputs.gardenhub_browser`). It runs on most PRs because most current work touches gardenhub. Gating it again saves nothing. Phase 5 makes it cheaper instead.
- `ci.yml` already has a main-push supersession guard (job `supersession`, `ci.yml:235`, composite `.github/actions/superseded`). `checks.yml` has none on its heavy jobs, and checks on main is the larger cost (46.6 min/run against ci's 29.6).
- Every PR, including the laptop's own, is opened by one GitHub login (`joyousaversion`). 188 of the last 200 PRs came from `agent/` branches. "Is this a crew PR?" must therefore be decided by head branch prefix `agent/`, not by author.
- `main` has no branch protection (the only ruleset, "protet", is disabled), so no required-check list needs editing.

## Expected savings (each lever applied after the ones above it, so savings are not double-counted)

| # | Lever | Saves/day | Basis |
|---|---|---|---|
| 1 | Full CI only once a PR is ready; a light tier of about 10 minutes on drafts | 4,000-6,500 | 2.2 pushes per PR today → about 1.3 full runs per PR, which is about 89 fewer full runs at about 73 minutes each, minus the light tier |
| 2 | Main: supersession on checks, then batched tip-only full runs | 3,000-4,500 | 73 main pushes at 76 minutes → about 30 full runs |
| 3 | Repo-wide checks run on PRs only when relevant files change, with a full nightly run | 1,500-2,500 | About 15 of checks' 31 minutes on each remaining full PR run |
| 4 | One gate job per workflow with a sparse checkout | 1,200-1,600 | About 7 minutes of gate overhead per push; light runners, so fewer node-hours than minutes |
| 5 | Browser job: Chromium in the runner image; goldens only on visual changes | 500-900 | Its tests are about 11 of its 17-23 minutes; the rest is setup |
| | **Total** | **10,000-14,000 (45-60%)** | Leaves about 10,000-13,000/day |

Separately, keycloak-theme's 175 GitHub-hosted minutes a day drop to about 25 (one billed minute for each run that exits early).

**Caveats:**
- This is one busy day (99 PRs) and a 96-run sample, and Phase 0 replaces it with a 3-day baseline.
- Lever 1 depends on crews actually pushing work in progress as drafts, which is why Phase 1 enforces it in code (gb and the bridge) rather than only in a prompt.

## Decisions

- **D1 (owner, 2026-10-28):** slim CI; use the draft flag as the "ready" signal. Settled in chat.
- **D2 (operator, this plan):** a crew PR is one whose head branch starts with `agent/`. Owner and laptop PRs are never converted to draft.
- **D3 (operator, this plan):** the full suite still runs on every calver tag, and nightly at a fixed odd minute. Nothing this plan removes from PRs or main is removed from tags.
- **D4 (owner, 2026-10-30, filed by Task 4.3):** fold the `pull_request` side of `checks.yml` and `images.yml` under the one `ci.yml` gate with `workflow_call`. Chosen: **fold**, which adds Task 4.4. After 4.2 the three gates cost about 300-400 runner-min/day, so the fold saves at most about 150-200 a day.

## Global Constraints

- **Workflow changes go through PRs, never a push straight to main.** CI must run the new workflow on its own PR. A workflow that is wrong on main stops every merge.
- **Every new gate gets a positive control.** A test or guard must show the gate letting a heavy job through when it should, not only skipping it. A zero-match filter looks like success; see `scripts/check-*` guards and their `_test.py` files for the house pattern.
- **Gates fail open.** If a gate step errors, its output must make the heavy job RUN. This matches `checks.yml`'s existing `changes` / `gate` design ("These come from the `gate` step, not `filter`", `checks.yml:138-140`).
- **No local `go test`, `go build` or Terraform on the laptop** (the laptop's WSL dies under that load). Go changes are verified by CI.
- **Never patch a CI scale set by hand.** `ci_max_nodes` changes only through `gardenio/terraform/composition/gardenio/main.tf` and `scripts/tf-gardenio.sh`.
- **Tag builds are untouched.** Any `if:` this plan adds must keep the job running when `github.ref_type == 'tag'` and on `schedule` and `workflow_dispatch` events.
- **Measure with the Phase 0 script**, over 3 full UTC days, after each phase has been live for those days. Report runner-minutes per day, per PR and per main push, plus the ASG average.
- Crews never cut tags. The operator rolls gb and the github-bridge in the normal ~04:00Z or ~16:00Z release windows.

## Task map and dependencies

| Task | Who | Blocked by |
|---|---|---|
| 0.1 `scripts/ci-minutes.py` + test | CREW | — |
| 0.2 3-day baseline report | OPERATOR | 0.1 |
| 1.1 Workflows: `ready_for_review`, draft gate, light tier | CREW | 0.2 |
| 1.2 `gb pr open` / `gb pr ready` | CREW | — |
| 1.3 Merge escalation refuses drafts; `claudemd_default.md` | CREW | 1.2 |
| 1.4 github-bridge converts crew PRs to draft (flagged off) | CREW | — |
| 1.5 Roll out (1.1 merged; release with 1.2-1.4; flag on); desk skips drafts | OPERATOR | 1.1, 1.3, 1.4 |
| 1.6 Measure Phase 1 | OPERATOR | 1.5 |
| 2.1 Release cut + promotion gate pick the newest green commit (prove or fix) | CREW | — |
| 2.2 `checks.yml` adopts the supersession guard | CREW | 0.2 |
| 2.3 Batched tip-only main runs | CREW | 2.1, 2.2 |
| 2.4 Measure Phase 2 | OPERATOR | 2.3 |
| 3.1 Nightly full run (`schedule`) for checks and ci | CREW | 0.2 |
| 3.2 gitleaks diff-only on PRs | CREW | 3.1 |
| 3.3 govulncheck/osv on dependency changes | CREW | 3.1 |
| 3.4 Narrow the terraform and shell-guard path gates | CREW | 3.1 |
| 3.5 Measure Phase 3 | OPERATOR | 3.2, 3.3, 3.4 |
| 4.1 Measure what a gate job costs | CREW | 0.1 |
| 4.2 One gate job in `ci.yml`; sparse checkout in every gate | CREW | 4.1 |
| 4.3 Decision D4 (fold PR workflows under one gate) | OPERATOR | 4.2 |
| 4.4 Fold the PR side of checks and images under the ci gate (`workflow_call`) | CREW | 4.3; merges after 1.6's and 3.5's windows |
| 5.1 playwright-core + Chromium in the CI runner image | CREW | 0.2 |
| 5.2 Goldens only when visual inputs change | CREW | 0.2 |
| 5.3 keycloak-theme exits early when Keycloak is untouched | CREW | 0.2 |
| 6.1 Final measurement, report, close | OPERATOR | 1.6, 2.4, 3.5, 4.3, 4.4, 5.1, 5.2, 5.3 |

---

## Phase 0: a repeatable baseline

### Task 0.1: `scripts/ci-minutes.py` measures runner-minutes per day, workflow, event and job

**Who:** CREW (one PR). It runs wherever `gh` and `aws` are signed in; the operator runs it from the laptop.

**Why:** every later phase claims a saving in runner-minutes. The 10-27 numbers came from ad-hoc commands, and the runs search API silently caps a query at 1,000 results, so a naive one-day query undercounts. A committed script makes each phase's before/after comparable.

**Files:**
- Create: `scripts/ci-minutes.py`
- Create: `scripts/ci-minutes_test.py`
- Modify: `.github/workflows/checks.yml`, the `script-selftests` job (`checks.yml:451`): add `python3 scripts/ci-minutes_test.py` next to the other `_test.py` lines.

**Behaviour:**
- `scripts/ci-minutes.py --repo mattbakerconsulting/gardenio --from 2026-10-25 --to 2026-10-27 [--sample N] [--seed 7] [--asg gardenio-k3s-ci --region us-east-1 --profile gardenio] [--json]`.
- **Run listing:** list runs with `gh api -X GET repos/<repo>/actions/runs -f created=<start>..<end> --paginate`, in windows of 3 hours or less. If a window reports `total_count >= 1000`, split it in half and retry. Deduplicate by run id.
- **Job timings:** for each `(workflow, event)` pair, take a seeded random sample of up to N non-skipped runs (default 30; `--sample 0` means every run). Fetch `runs/<id>/jobs?filter=all&per_page=100`. Sum `completed_at - started_at` per job, skipping `conclusion == skipped` and jobs with a null or inverted start.
- **Output:**
  - Table 1: runs per day by workflow, event and conclusion.
  - Table 2: minutes per run by workflow and event, with a per-job breakdown giving minutes per run and how many sampled runs had the job.
  - Table 3: estimated runner-minutes per day (runs × minutes per run), with a total.
  - Table 4: distinct PR branches per day, and full ci runs per PR.
  - Table 5 (when `--asg` is given): the ASG's daily average and maximum of `GroupInServiceInstances` from `aws cloudwatch get-metric-statistics --namespace AWS/AutoScaling --period 86400`.
  - Runner label per job, so that GitHub-hosted (`ubuntu-*`) minutes are reported separately.
- `--json` prints the same data as one JSON object, which Task 6.1 diffs.
- **Pure functions for the test:** `split_window(start, end, total) -> list`, `job_minutes(job) -> float|None`, `estimate_daily(runs, per_run) -> dict`. The test feeds them fixture JSON (no network) and covers:
  - a skipped job
  - a null `started_at`
  - an inverted start and end
  - a window at 1,000 results that splits
  - a GitHub-hosted label being kept apart.
- The test also includes a **positive control**: a fixture day with a known total that must come out exactly.

- [ ] **Step 1:** write `scripts/ci-minutes_test.py` with the fixtures above; run it; it fails because the script does not exist.
- [ ] **Step 2:** write `scripts/ci-minutes.py` (stdlib only: `subprocess`, `json`, `datetime`, `random`, `argparse`); run the test until it passes.
- [ ] **Step 3:** add the test to `checks.yml`'s `script-selftests`; open the PR.

**Acceptance:**
- The test passes in CI.
- Run against 2026-10-27 with `--sample 25`, the script reproduces the baseline above within ±10% per row: 1,917 runs; ci PR 37.5 and checks PR 31.4 minutes per run.

### Task 0.2: 3-day baseline report

**Who:** OPERATOR.

- [ ] Run `scripts/ci-minutes.py --from <D-3> --to <D-1> --sample 40 --asg gardenio-k3s-ci --json` for the three full UTC days before Phase 1 starts.
- [ ] Post the tables as a comment on the plan epic, prefixed `laptop-claude:`.
- [ ] Commit the JSON as `gardenio/docs/ci-minutes/baseline-<date>.json`.
- [ ] If any lever's basis moved by more than 25% from the 10-27 numbers, say so in the comment and restate that lever's expected saving.

**Acceptance:** the comment and the JSON exist, and the JSON's total runner-minutes per day is stated in the comment.

---

## Phase 1: full CI only once a PR is ready (owner: "we need to institute it somehow")

### Task 1.1: ci, checks and images run their heavy jobs only on ready PRs; drafts get a light tier

**Who:** CREW (one PR touching three workflows plus a guard). This PR must itself be opened ready (non-draft), so its own CI runs in full.

**Why:** 99 PRs took 218 pushes on 10-27, and every push ran about 81 runner-minutes of ci, checks and images. Crews iterate against CI. A draft should get fast feedback (build and unit tests); the full suite should run once, when the crew says the work is done.

**Files:**
- Modify: `.github/workflows/ci.yml`: the `on.pull_request` block (`ci.yml:55`), and the `if:` of the heavy jobs `go` (`:534`), `rust` (`:850`), `gardenhub-browser` (`:1055`) and `lands-conformance` (`:1208`).
- Modify: `.github/workflows/checks.yml`: `on.pull_request` (`checks.yml:15`), and the `if:` of `lint-go` (`:324`), `script-selftests` (`:451`), `terraform` (`:726`), `helm` (`:762`), `gitleaks` (`:975`), `security` (`:1020`) and `cargo-security` (`:1234`). Re-check the job list against the file; every job except `changes` is heavy.
- Modify: `.github/workflows/images.yml`: `on.pull_request` (`images.yml:21`) and the `if:` of `controller-base` (`:607`) and `build` (`:730`).
- Create: `scripts/check-ci-draft-gate.py` and `scripts/check-ci-draft-gate_test.py`. Wire both into `checks.yml` `script-selftests`.
- Modify: `.github/workflows/ci.yml`: a new job `draft-light`.

**The change:**
1. In all three workflows:
   ```yaml
   pull_request:
     types: [opened, synchronize, reopened, ready_for_review]
   ```
   `converted_to_draft` is deliberately absent: turning a PR back into a draft needs no run.
2. Every heavy job's `if:` gains one conjunct, written exactly like this so the guard can find it:
   ```yaml
   (github.event_name != 'pull_request' || github.event.pull_request.draft != true)
   ```
   - It is ANDed with the job's existing condition, so path gates and supersession still apply.
   - Push, tag, schedule and dispatch events are unaffected by construction.
3. A new `ci.yml` job `draft-light`:
   - `runs-on: gardenio-k3s-medium`
   - `needs: [supersession, changes]`
   - `if: github.event_name == 'pull_request' && github.event.pull_request.draft == true && needs.supersession.outputs.superseded != 'true'`
   - Steps:
     - Check out the repo.
     - For each Go module the PR touches (`kbeads`, `gardenio/controller`): `go build ./... && go vet ./...`, then `go test` of only the packages whose files changed, from `git diff --name-only origin/${{ github.base_ref }}...HEAD` mapped to package directories.
     - For coop, `cargo check` only.
   - Budget: `timeout-minutes: 15`, target ≤ 10.
   - Its name starts with `draft:` so a reader of the checks list can tell it is not the full suite.
4. Concurrency needs no change: the `ready_for_review` run lands in the same per-PR group and supersedes any draft run in flight.

**Guard `scripts/check-ci-draft-gate.py`:**
- Parses each of the three workflow files with a stdlib line scanner; no PyYAML exists on the runners.
- Fails if `pull_request.types` lacks `ready_for_review`, if any job other than the gate jobs (`supersession`, `changes`, `draft-light`) lacks the exact conjunct, or if `draft-light` is missing.
- **Its test must include a positive control:** a good synthetic workflow passes, plus one bad case per failure mode.

**Steps:**
- [ ] **Step 1:** write the guard test; run it; it fails.
- [ ] **Step 2:** write the guard; the test passes; the guard fails on today's workflows.
- [ ] **Step 3:** edit the workflows until the guard passes.
- [ ] **Step 4:** open the PR (non-draft). In its body, record a run of each kind:
  - Convert a throwaway draft test PR (branch `ci-draft-probe/<date>`, a one-line comment change) to draft and push, then show only `changes`, `supersession`, `draft-light` and the images `changes` job ran.
  - Mark it ready, and show the full suite started on `ready_for_review`.
  - Close the probe PR without merging.

**Acceptance:**
- Guard and test green.
- The probe PR's draft push ran 15 runner-minutes or less in total across the three workflows (read with `scripts/ci-minutes.py`-style job sums).
- Its `ready_for_review` run executed every heavy job that the path gates select.

### Task 1.2: `gb pr open` and `gb pr ready`

**Who:** CREW (one PR in `gardenio/controller/cmd/gb`).

**Why:** a line in a prompt drifts; a command is harder to forget. Crews already open PRs with raw `gh pr create` (`gardenio/controller/cmd/gb/claudemd_default.md:72`) and file merge escalations with `gb escalation create`, so one place should own both.

**Files:**
- Create: `gardenio/controller/cmd/gb/pr.go`, `gardenio/controller/cmd/gb/pr_test.go`.
- Modify: the gb root command registration (where the other top-level commands are added; find it with `grep -n 'AddCommand(' gardenio/controller/cmd/gb/*.go`).

**Behaviour:**
- `gb pr open --title T [--body-file F] [--base main]`:
  - Runs `gh pr create --draft` with the body shape `claudemd_default.md` already prescribes (the `## Summary` block, `Operator: $OP_NAME ($BEADS_OPERATED_BY)`, `Bead: $KD_AGENT_ID`). If `--body-file` lacks the trailers, they are appended.
  - Records the PR URL on the agent's task bead in the `mr_url` field, the same field the merge guard's `fillMergeSpecURL` reads (`escalation_merge_dedupe.go`).
  - Prints the URL.
  - Refuses to run on `main`.
- `gb pr ready [<pr-url-or-number>]`; with no argument it uses the current branch's PR:
  1. Fails if the PR is not a draft (it prints "already ready") or if the local HEAD is not pushed.
  2. Runs `gh pr ready`.
  3. Files the merge escalation through the existing code path that `gb escalation create` uses for merge types. That path deduplicates per PR, so running it twice files nothing new.
  4. Comments on the task bead: `PR #<n> marked ready; full CI started; merge escalation <id>`.
- `gb pr draft [<pr>]` runs `gh pr ready --undo`, for a crew starting a big rework.

**Tests (`pr_test.go`, with fakes for the gh runner and the beads client):**
- `open` always passes `--draft`.
- `open` appends missing trailers.
- `ready` on a non-draft returns an error.
- `ready` files exactly one escalation when called twice.
- `ready` with an unpushed HEAD returns an error.

- [ ] Steps: write the tests, see them fail; implement; open the PR (ready, non-draft); CI must be green.

**Acceptance:** `pr_test.go` is green in CI, and `gb pr --help` lists `open`, `ready` and `draft`.

### Task 1.3: a merge escalation for a draft PR is refused; the crew instructions use `gb pr`

**Who:** CREW (one PR).

**Files:**
- Modify: `gardenio/controller/cmd/gb/escalation_create.go`, in the merge-type path (the types are in `mergeOperationTypes`, `escalation_merge_dedupe.go`): before creating, look up the PR named by `operation_spec.mr_url`. If it is a draft, return an error: `PR #<n> is a draft; run 'gb pr ready' (it files this escalation for you)`. If the lookup fails, create anyway (fail open) and print a warning.
- Modify: `gardenio/controller/cmd/gb/escalation_create_test.go` (or a new `escalation_draft_test.go`): draft refused, ready allowed, lookup error allowed with a warning.
- Modify: `gardenio/controller/cmd/gb/claudemd_default.md`, the Delivery Protocol (`:58-82`):
  - Step 4 becomes `gb pr open --title "..." --body-file <file>`, with the same body content, and says the PR opens as a draft.
  - A new step after it: "Push as often as you like while it is a draft; only build + unit tests run. When the work is done and your own checks pass, run `gb pr ready` once. That starts the full CI and files the merge escalation; do not file one yourself."
  - The raw `gh pr create` stays only as the fallback for a non-GitHub remote.
  - This file is `go:embed`-ed into gb (see the comment at `ci.yml:22-27`), so the change ships with gb.

**Acceptance:** tests green in CI. `grep -n 'gh pr create' gardenio/controller/cmd/gb/claudemd_default.md` shows only the fallback line.

### Task 1.4: the github-bridge converts a crew PR opened as ready back to a draft (behind a flag)

**Who:** CREW (one PR).

**Why:** a backstop that does not depend on any prompt. The bridge already receives `opened`, `ready_for_review` and `converted_to_draft` (`gardenio/controller/cmd/github-bridge/pullrequest.go:63-66`), and it holds a GitHub App installation token source (`tokens.go:37`) plus a GraphQL client (`resolver.go`).

**Files:**
- Modify: `gardenio/controller/cmd/github-bridge/pullrequest.go`. On `action == "opened"` with `!pr.Draft`, `pr.Base.Ref == "main"`, `strings.HasPrefix(pr.Head.Ref, "agent/")` and the flag on:
  - Call the GraphQL mutation `convertPullRequestToDraft(input:{pullRequestId: <node_id>})` with the installation token.
  - Then post one PR comment: `Opened as ready from an agent branch, so moved to draft: drafts run only build + unit tests. Run \`gb pr ready\` when the work is done.`
  - Log both calls.
  - On error, log and continue. Never block event handling.
- Modify: the bridge's config/env parsing: `GITHUB_BRIDGE_DRAFT_AGENT_PRS` (bool, default false).
- Modify: `gardenio/helm/gardenio` github-bridge template and `values.yaml`: `githubBridge.draftAgentPRs: false`, rendered into that env var. Default false, so the chart renders unchanged.
- Create/modify tests in `pullrequest_test.go`:
  - Flag off: no call.
  - Flag on with an agent branch, non-draft and base main: one mutation and one comment.
  - Draft: no call.
  - Non-`agent/` branch: no call.
  - Mutation error: event still handled.
- Check the GitHub App's permissions. Converting needs `pull_requests: write`. Read the installation's permissions with `gh api /repos/mattbakerconsulting/gardenio/installation --jq .permissions` (as the operator) and put the result in the PR body. If `pull_requests` is not `write`, say so in the PR; Task 1.5 then needs an owner click to grant it.

**Acceptance:** tests green in CI; the chart renders identically with the default values.

### Task 1.5: roll out the ready gate

**Who:** OPERATOR.

**Order:**
1. Merge 1.1; it is inert while no PR is a draft.
2. Release gb with 1.2 and 1.3 in the next window; crew pods pick up the new gb and `claudemd_default.md` on their next spawn.
3. Release the bridge with 1.4.
4. After one day of crews using `gb pr open`, set `githubBridge.draftAgentPRs: true` in `deploy-values/gardenio-k3s.yaml` (a values-only change) and roll it.

**Merge desk:**
- Amend the merge-desk prompt (trigger `gd-WksO1FSmvV`) so it skips draft PRs (`gh pr view --json isDraft`). A draft is never a merge candidate, and "not mergeable" on a draft is not a failure to report.
- Reseal the trigger after editing.

**Watch for one day after each step:**
- PRs stuck as drafts with a merge escalation (there should be none, because 1.3 refuses that).
- Crews calling `gh pr create` directly: count `opened` events from `agent/` branches with `draft == false`; the bridge log shows each conversion.

**Acceptance:** after the flag is on, at least 90% of new `agent/` PRs in a day reach ready through `gb pr ready`. Count `ready_for_review` events against `opened` events from `agent/` branches.

### Task 1.6: measure Phase 1

**Who:** OPERATOR.

- [ ] Run `scripts/ci-minutes.py` over the 3 full days after Task 1.5's flag went on.

**Acceptance:**
- Full ci runs per PR are 1.5 or fewer (baseline 2.2).
- Runner-minutes per day on PRs (ci + checks + images) fall by at least 4,000 against the Phase 0 baseline, normalised to the same PR count.
- Post the comparison on the plan epic. If the saving is under 4,000, the comment says why (for example, the light tier costing more than its 10-minute budget, or crews marking ready too early) and what changes.

---

## Phase 2: batched main runs

### Task 2.1: release cutting and the promotion gate select the newest commit with a green run

**Who:** CREW (one PR, possibly docs-only).

**Why:** Task 2.3 stops testing every main commit separately, so some commits will have no run of their own. Anything that requires "this exact commit is green" must instead select "the newest commit that has a green run".

**Files to read and, if needed, fix:**
- `gardenio/scripts/release-cut.sh` and `gardenio/scripts/release-cut_test.sh`.
- `scripts/check-release-promotion-gate.sh` and its `_test.sh`.
- `gardenio/scripts/release-deploy.sh`.
- Anything else found with `git grep -n -E 'check-runs|conclusion|/actions/runs' -- '*.sh' '*.py'`.

**Acceptance:**
- The PR body lists every consumer of run results and says, for each, whether it already picks the newest green commit.
- Each one that doesn't is fixed, and its test gains a case where HEAD has no run and HEAD~1 has a green one: the tool must select HEAD~1, not fail and not select HEAD.

### Task 2.2: `checks.yml` heavy jobs skip a superseded main push

**Who:** CREW (one PR).

**Why:** checks on main cost 46.6 minutes per run against ci's 29.6, and `ci.yml` already skips a main push that a newer push supersedes (job `supersession`, `ci.yml:235`; composite `.github/actions/superseded`). `checks.yml` does not.

**Files:**
- Modify: `.github/workflows/checks.yml`:
  - Add a `supersession` job copied from `ci.yml:235-262`: same `runs-on: gardenio-k3s-light`, same sparse checkout, same composite.
  - Add `needs.supersession.outputs.superseded != 'true'` to every heavy job's `if:`.
  - Add the in-job re-check step that `ci.yml` uses, so a supersession that arrives after the gate is caught too. Read `ci.yml:120-230` (the sentinel comment) and copy the pattern, including the `heavy work starting` sentinel step.
- Modify: `scripts/check-ci-draft-gate.py` from Task 1.1, so it also requires the supersession conjunct on `checks.yml`'s heavy jobs. If Task 1.1 has not landed yet, add a separate small guard instead.

**Acceptance:** guard green. The PR body shows one main push in a burst whose checks heavy jobs were skipped as superseded, with the sentinel `heavy work starting` concluding `skipped`.

### Task 2.3: at most one full main run per workflow at a time; the newest waiting run replaces older ones

**Who:** CREW (one PR). Blocked by 2.1 and 2.2.

**Why:** 73 main pushes on 10-27 each ran about 76 minutes. A run of the tip covers every commit before it, because the tests are cumulative. GitHub concurrency groups hold one running and one pending run, and a newer pending run evicts the older one. `checks.yml:18-38` explains why that eviction was deliberately removed on 2026-09-20 (gd-jrNLZaVGhb), when it silently left main untested. With Task 2.1 done, it becomes the intended batching, as long as the newest run always completes.

**Files:**
- Modify: `.github/workflows/ci.yml` and `.github/workflows/checks.yml`, the `concurrency:` blocks. For `push` events on `refs/heads/main` use `group: ${{ github.workflow }}-main`, with `cancel-in-progress: false`. Keep PR, dispatch, schedule and tag keys as they are.
- Rewrite the comment above each block: say what changed, why the 2026-09-20 hazard no longer applies (Task 2.1), and that the run left pending is always the newest.
- Create: `scripts/check-main-concurrency.py` and its test. They assert the main-push group key is shared per workflow, that `cancel-in-progress` is false for it, and that the PR group still cancels in progress. Include a positive control.

**Acceptance:**
- Guard green.
- Over one day after merge, every main commit that was the tip when its predecessor's run finished has a completed run.
- The newest main commit always gets a completed run within 90 minutes of its push: list the day's main commits and their runs.
- No main tip goes without a run.

### Task 2.4: measure Phase 2

**Who:** OPERATOR.

**Acceptance:**
- Full main runs per day (ci + checks) are 35 or fewer against 73 pushes.
- Main runner-minutes per day fall by at least 3,000 against Phase 0.
- Post the result on the epic.

---

## Phase 3: repo-wide checks on PRs only when they can find something new

### Task 3.1: a nightly full run of `checks.yml` and `ci.yml`

**Who:** CREW (one PR). This must land before 3.2-3.4: it is where everything they skip still runs.

**Files:**
- Modify: `.github/workflows/checks.yml` and `ci.yml`, `on:`. Add `schedule: - cron: '43 10 * * *'`: an odd minute, outside the 09:17Z e2e nightly (`e2e.yml:54-56`) and away from the 04:00Z and 16:00Z release windows.
- Make sure every path gate and `changes` output treats `schedule` like `workflow_dispatch`, so everything runs. The existing gates already OR `github.event_name == 'workflow_dispatch'` (for example `ci.yml:1068`); extend each to `schedule`.
- Extend the `check-ci-draft-gate.py` guard: every heavy job must run on `schedule`.

**Acceptance:**
- Guard green.
- The first scheduled run executes every job in both workflows; the PR's follow-up comment lists them with conclusions.

### Task 3.2: gitleaks scans only the PR's commits on a PR

**Who:** CREW (one PR).

**Files:**
- Modify: `.github/workflows/checks.yml`, job `gitleaks` (`checks.yml:975-1019`).
  - On `pull_request`, pass `--log-opts="origin/${{ github.base_ref }}..HEAD"`, with a fetch depth that includes the base.
  - On push, schedule, dispatch and tag, keep the full-history scan.
  - Keep the job's existing comments accurate.
- Add a positive control: a test fixture or a documented one-off probe commit on a throwaway draft PR, carrying a planted fake key. It must still fail the PR scan. Use the word-boundary-safe fake format the repo's secret guards already use, and remove it before closing.

**Acceptance:** the gitleaks job on PRs drops from 4.4 to under 1.5 minutes per run (sampled); the planted fake is caught.

### Task 3.3: govulncheck and osv run on a PR only when dependencies change

**Who:** CREW (one PR).

**Files:**
- Modify: `.github/workflows/checks.yml`, the `changes` job (`:129`): a new output `deps`, true when any `go.mod`, `go.sum`, `Cargo.toml` or `Cargo.lock` changed. It is fail-open like the other outputs: on error, true.
- Modify: job `security` (`:1020`) and `cargo-security` (`:1234`): on `pull_request`, require `needs.changes.outputs.deps == 'true'`. Every other event runs as today. New vulnerability-database entries are caught by the nightly run (3.1) and on main.

**Acceptance:** guard green; security jobs run on fewer than 20% of PRs over a day; the nightly run executes them.

### Task 3.4: narrow the terraform and shell-guard path gates

**Who:** CREW (one PR).

**Why:** `terraform` (4.3 min) and `script-selftests` (6.5 min) ran on 22 of 21 sampled PR runs (some runs had more than one job), although few PRs touch Terraform or scripts. The gates match too much, or fail open too often.

**Steps:**
- [ ] Over the last 100 PR runs, for each of the two jobs, read the `changes` job's log line that decided it. The `gate` step ORs the path verdict with a per-job-block verdict (`checks.yml:138-140`), so a run is in one of three states: matched by path, matched by the block, or failed open.
- [ ] Put the counts in the PR body.
- [ ] Narrow the filters to the paths each job actually reads.
- [ ] Fix any fail-open that fires on ordinary PRs.
- [ ] Keep "fails open on error".

**Acceptance:** each job runs on fewer than 40% of PR runs over a day after merge. The PR body shows the before and after counts.

### Task 3.5: measure Phase 3

**Who:** OPERATOR.

**Acceptance:** checks runner-minutes per PR run fall by at least 7 (from 31.4) against Phase 0; post the result on the epic.

---

## Phase 4: one gate per workflow

### Task 4.1: measure what a gate job costs

**Who:** CREW (report only, posted on the task bead).

- [ ] For 50 recent runs, split each gate job into three parts:
  - queue to pod start (`started_at` minus the run's `created_at`);
  - checkout;
  - the gate script.
- [ ] Do this for the gate jobs `changes` and `supersession` in ci, `changes` in checks and `changes` in images. On 10-27 they took 2.5, 0.5, 2.2 and 1.8 minutes per run.

**Acceptance:** the posted table gives, for each gate job, the median and p90 of each part, and the total runner-minutes per day.

### Task 4.2: one gate job in `ci.yml`; every gate uses a sparse checkout or the API

**Who:** CREW (one PR).

**Files:**
- Modify: `.github/workflows/ci.yml`:
  - Merge `supersession` (`:235`) and `changes` (`:379`) into one job `gate`, which outputs both `superseded` and the path outputs.
  - Update every `needs:` and `if:` that names either job.
- Modify: the `changes` jobs in all three workflows. Wherever Task 4.1 shows checkout as a large part of the cost, replace the full checkout with `sparse-checkout` of only the action or script the gate runs. `dorny/paths-filter` reads PR files through the API, so it needs no tree.
- Update the `check-ci-draft-gate.py` gate-job list.

**Acceptance:** guard green; gate runner-minutes per push across the three workflows fall by at least 40% from Task 4.1's total.

### Task 4.3: decision D4, one PR gate for all three workflows

**Who:** OPERATOR.

- [ ] File a decision bead for the owner:
  - **Prompt:** whether to fold the `pull_request` side of `checks.yml` and `images.yml` under one `ci.yml` gate with `workflow_call`. Give the measured gate cost left after 4.2, and the risk: one workflow file owns PR CI, so a mistake there stops all PR CI.
  - **Options:** a = fold (a new task is added to this plan); b = keep three workflows (Phase 4 ends here).
  - Do not name other beads' ids in the body; that parks them.

**Acceptance:** the decision is filed; its answer is read back from the `chosen` field before acting.

**Outcome (2026-10-30):** D4 chose **fold**. The gate cost measured after 4.2, over about 7h of runs, is posted on the task bead. Task 4.4 does the fold.

### Task 4.4: fold the PR side of `checks.yml` and `images.yml` under the `ci.yml` gate

**Who:** CREW (two PRs, in order).

**Why:** a PR now runs three gate jobs (ci `gate`, checks `changes`, images `changes`). Each costs 0.26-0.34 runner-min, and most of that is pod start and teardown. Folding drops the PR side to one gate and saves about 150-200 runner-min/day. It also cuts two pod starts and their queue wait from every PR push. The risk D4 accepted is that `ci.yml` then owns all PR CI, so a mistake in that one file stops every PR check. The rules below are there to keep that from failing open or in silence.

**Shape:**
- `checks.yml` and `images.yml` gain `on: workflow_call` and lose `on: pull_request`. Their `push`, `schedule`, `workflow_dispatch` and calver-tag triggers stay exactly as they are, so `release-cut.sh` (push runs of ci + checks) and the image publish (tag runs of images) are unchanged.
- `ci.yml` gets two caller jobs: `uses: ./.github/workflows/checks.yml` and `uses: ./.github/workflows/images.yml`. Each has `if: github.event_name == 'pull_request'`, `secrets: inherit`, and the permissions its callee needs. `images` needs `id-token: write` for `ECR_ROLE_PR`. A callee cannot get more than its caller grants.
- The ci `gate` job computes the checks and images path outputs too (checks' `gate`-step outputs, images' `select` matrix/count/controller-base). It passes them in as `workflow_call` inputs. When called, each callee's own `changes` job is skipped. Detect "called" with an input, such as `from_ci_gate: true`, not with `github.event_name`, which in a callee is the caller's event (`pull_request`).
- The gate still fails open: if a step errors, the ci gate's outputs for checks and images must make their heavy jobs RUN. That is the same rule as today (Global Constraints).

**Things that break if you don't handle them:**
- **Concurrency.** `checks.yml`'s top-level group starts with `${{ github.workflow }}`. In a callee that evaluates to the caller's name (`ci`), so the called run lands in ci's own PR group and cancels or deadlocks it. Key the callee's group on a literal, or drop it when called, since the caller's group already covers the PR. Extend `scripts/check-main-concurrency.py` to evaluate the called case.
- **Required checks.** The ruleset requires the check names `ci` and `checks`. A callee's jobs report as `<caller job name> / <callee job name>`, so on a PR the checks aggregator would report `… / checks`, and the required `checks` would never arrive. Keep a top-level job in `ci.yml` whose check name is `checks` (`checks (draft)` on drafts) that judges the called run. Alternatively, change the ruleset through an operator escalation, but keeping the names is preferred. Make `ci` need every caller job too. Update `scripts/check-ci-required-checks.py` (aggregators, the "no other job reports `ci`/`checks`" rule, and its fixtures) so the folded shape passes and a missing caller job in `needs:` reds.
- **The merge desk.** `scripts/check-pr-ready-run.py` looks for a checks.yml run on the PR's sha and reads `quench quality gate` in it. After the fold, PRs have no checks.yml run, and quench reports inside the ci run as `<caller> / quench quality gate`. Teach it the folded names, and keep the old shape accepted (as `CI_LEGACY_GATES` does) for branches cut before the fold. **This lands first, in its own PR**, because the desk runs from main: if the fold merged first, the desk would fail closed on every PR (as it did with #1509).
- **Guards that name these files or jobs.** Bring along `scripts/check-ci-draft-gate.py` (gate-job list, draft conjunct on called jobs), `scripts/check-images-filters.py` and `scripts/check-ci-path-gates.py` (the filters move into ci.yml's gate), `scripts/workflow-changed-jobs.py` (a checks- or images-scope filter that self-references its workflow file must now also see the ci.yml caller), and `.github/workflows/retry-lost-runners.yml` (it matches `run.path` for images.yml, but images PR jobs now run in ci.yml runs). Read each guard's header before changing it. Each one exists because a gate once failed open.
- `last-run-base` and the `superseded` action are push-only; check that the fold leaves them untouched.

**Steps:**
- [ ] PR 1: `check-pr-ready-run.py` accepts both the folded and the unfolded shapes. Add test fixtures for both, plus a folded run whose quench was skipped (fails closed). Merge it.
- [ ] PR 2: the fold and every guard above. Open it as a draft (`gb pr open`) and mark it ready (`gb pr ready`). Its own PR runs the new ci.yml, so the PR proves itself:
  - its draft run shows one gate job and the light tier;
  - its ready run runs the checks and images jobs its paths select, with the required `ci` and `checks` checks green;
  - `check-pr-ready-run.py` PASSes on it.
- [ ] Do not merge PR 2 before 1.6's and 3.5's measurement windows close (3.5 measures 10-31..11-02). Those windows read the per-workflow PR runs this PR moves.
- [ ] Positive controls on PR 2, or on a throwaway PR cut from it: a PR that touches only a checks-scope path (for example, a file under `scripts/` that a shell guard reads) runs that checks job. A PR that touches one image's path builds that image. A docs-only PR skips both and the required checks still go green.

**Acceptance:**
- On ready PR runs after the merge, exactly one gate job runs per PR push across the three workflows.
- Over 3 full UTC days, PR-side gate runner-min/day is at most the ci gate's alone plus 20% (Task 4.3 measured ≈102/day for it).
- Every guard is green, and the merge desk PASSes post-fold PRs.
- Push, schedule, dispatch and tag runs of checks.yml and images.yml are unchanged: same job names and same triggers, and `release-cut.sh --dry-run` still selects a commit.

---

## Phase 5: the browser job and keycloak-theme

### Task 5.1: playwright-core and Chromium in the CI runner image

**Who:** CREW (one PR).

**Files:**
- Modify: `gardenio/images/ci-runner/` (its Dockerfile): install Node 22, `playwright-core` pinned to the version `ci.yml`'s browser job installs, and its Chromium. Read the version from the step `playwright-core + Chromium (timed)` in `ci.yml:1100-1115`.
- Modify: `.github/workflows/ci.yml`, job `gardenhub-browser`: when the image already has the pinned version, the install step is a no-op. The comment at `ci.yml:1108-1112` says it already is "by then"; make that hold on a fresh node too.
- Measure the image size change and state it in the PR. The memory note that node-local caches don't hit on this pool applies: a baked binary beats a cache.

**Acceptance:** the browser job's setup (everything before `make test-gardenhub-browser`) drops below 2 minutes on a fresh node. Sample 10 runs.

### Task 5.2: `make test-garden-goldens` only when visual inputs change

**Who:** CREW (one PR).

**Files:**
- Modify: `.github/workflows/ci.yml`:
  - Add a `changes` output `garden_goldens`, true when the PR changes the inputs the goldens render. Derive them from the Makefile target `test-garden-goldens` and the golden test files: the web assets, templates, sprites and the golden files themselves.
  - Gate the step `make test-garden-goldens` (`ci.yml:1151`) on it.
  - Fail open: on error, true. Schedule, push and tag always run it.

**Acceptance:** goldens run on fewer than 60% of browser-job runs over a day, against nearly all today. The job saves about 3.6 minutes on the rest.

### Task 5.3: keycloak-theme exits in its first step when nothing Keycloak changed

**Who:** CREW (one PR).

**Why:** this is the only workflow on GitHub-hosted runners (`runs-on: ubuntu-latest`, `keycloak-theme.yml:54`), which GitHub bills by the minute. It ran 21 times on 10-27 at 8.4 minutes each. Its path filter includes `gardenio/helm/gardenio/values.yaml` and `deploy-values/gardenio-k3s.yaml` (`keycloak-theme.yml:26-28`), which change on nearly every chart PR, though only a Keycloak image change matters.

**Files:**
- Modify: `.github/workflows/keycloak-theme.yml`:
  - Add a first step. For PRs whose only matched paths are those two values files, diff them and continue only if a Keycloak key changed. Write the key path the step looks for (for example `keycloak.image`) in the step and its comment.
  - Otherwise, every later step is skipped through a step output.
  - The job then bills one minute.

**Acceptance:** over a day, keycloak-theme's billed minutes fall below 50, against about 175 on 10-27. A positive control (a PR that bumps the Keycloak tag) still runs the full harness.

---

## Phase 6: measure the whole plan

### Task 6.1: final measurement and report

**Who:** OPERATOR.

- [ ] Run `scripts/ci-minutes.py --json` over 3 full days after the last merged task has been live for those days. Diff it against the Phase 0 JSON.
- [ ] Post on the plan epic:
  - runner-minutes per day, against the 23,400 baseline and the Phase 0 baseline;
  - the share saved by each lever;
  - the CI ASG daily average, against 5.4;
  - GitHub-hosted minutes per day;
  - whether `ci_max_nodes = 6` now queues. Queue p90: the PR runs' `started_at - created_at` for heavy jobs.
- [ ] Close the plan, or name the lever that missed and why.

**Acceptance:**
- The report exists.
- Total runner-minutes per day are at least 45% below the Phase 0 baseline, or the report says which lever fell short and by how much.

---

## Not in scope

- The CI pool's instance shape and `ci_max_nodes` (bead gd-bPVTlMeWV6 tracks instance shape). The temporary 6 → 12 raise on 2026-10-28 is reverted on its own schedule, not by this plan.
- Faster builds (Go and cargo caches, gd-A0f2QqgYpM). This plan cuts what runs, not how fast it runs.
- The e2e workflow (label-triggered; 136 of 137 runs skipped on 10-27).
- `retry-lost-runners` (about 210 light runner-minutes a day).

**Supersedes:** the CI-minutes phase beads under the 2026-09-20 CI-speed epic gd-irs5C9BEBi (phase-1 measurement gd-fEHhy9cUbw, phase-2 ranking gd-503hW3MMJN, phase-3 re-measurement gd-jbpL0XAuQH). Their measurements predate today's pool. The still-open bugs under that epic stay where they are.
