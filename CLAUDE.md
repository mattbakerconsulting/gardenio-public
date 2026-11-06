# Gardenio Monorepo

Unified repository for the gardenio platform — K8s agent controller, terminal mux, and beads daemon.

> **New machine?** Operator (kd + gb) setup is in [`docs/onboarding-mac-operator.md`](docs/onboarding-mac-operator.md)
> — or use the `gardenio` skill in Claude Code (`/gardenio onboard`).
>
> **New project + role?** Adding a project + role to an *already-running* gardenio:
> [`gardenio/docs/onboarding-new-project-and-role.md`](gardenio/docs/onboarding-new-project-and-role.md).

## Architecture

| Directory | Language | Description |
|---|---|---|
| `gardenio/` | Go | K8s agent controller, Slack bridge, Helm chart, agent images |
| `coop/` | Rust | Agent terminal multiplexer + credential manager |
| `kbeads/` | Go | Beads daemon (gRPC/HTTP server) + kd CLI |

> The bead UI is served by `beads-viewer` (`gardenio/controller/cmd/beads-viewer/`).
> The legacy `beads3d` 3D force-graph viewer was removed (ka-0000006444).

Each component keeps its own `CLAUDE.md` with component-specific instructions —
see `gardenio/CLAUDE.md`, `coop/CLAUDE.md`, `kbeads/CLAUDE.md`.

## Directory Structure

```
gardenio/   # controller/ (Go: controller + bridge + CLI binaries), helm/gardenio/, images/
coop/      # Rust workspace — crates/cli (coop), crates/mux (coopmux), tests/, proto/
kbeads/    # Go: cmd/kd/ (CLI), gen/ (generated protobuf), beads daemon
.github/   # GitHub Actions workflows (the only CI)
scripts/   # Release and utility scripts
```

## Workspaces

- **Go**: `go.work` at repo root links `gardenio/controller` and `kbeads`.
- **Rust**: `coop/Cargo.toml` is a self-contained workspace (`coop/crates/*`).

## Build

`make build` / `make test` / `make lint` operate on all components. There is no
`make preflight` any more (it ran RWX, deleted 2026-09-16 with `scripts/preflight-targets.sh`);
the [Landing the Plane](#landing-the-plane) checklist is the local pre-push gate, and CI
runs on the PR (see [CI](#ci)). Root `make test` does not reach `test-helm`: the
chart/dashboard suite in `gardenio/helm/gardenio/tests` is a separate Go module excluded
from `go.work`, which CI runs in the `checks.yml` `helm` job. Chart-only change?
`make test-helm` runs the whole suite locally (needs helm, yq, promtool).
For chart work on an agent pod, run only the tests you touched
(`cd gardenio/helm/gardenio/tests && GOWORK=off go test -count=1 -run <TestName> .`) and
leave the full suite (~24 min on a pod) to CI's `checks.yml` `helm` job.

**A repo-root `go test ./...` does NOT run every Go test.** `go.work` links only
`gardenio/clicreds`, `gardenio/controller`, `kbeads` and the two `scripts/` modules; in
workspace mode `./...` resolves against workspace members only, so six modules are
skipped **silently, with a green exit** (bug ka-0000008045):

| Module | Runs via |
|---|---|
| `gardenio/helm/gardenio/tests` (chart + Grafana dashboard assertions, ~5 min on CI, ~24 min on a pod) | `make test-helm` |
| `gardenio/tests/slackmock` | `make test-slackmock` |
| `gardenio/grafana-plugins/gardenio-assistant-app` | `make test-grafana-plugin` |
| `lands/contract` (Lands contract v1: OpenAPI spec, Go + TS bindings, golden fixtures) | `make test-lands-contract` |
| `lands/gl` (gl, the tenant's CLI for a land) | `make test-lands-gl` |
| `scripts/heraldry-proto` (Phase 0 heraldry renderer prototype, throwaway) | `make test-heraldry-proto` |

`make test-out-of-workspace` runs all six. From inside one of those directories the
working invocation is `GOWORK=off go test ./...` — plain `go build ./...` fails with
"directory prefix . does not contain modules listed in go.work", and `GOFLAGS=-mod=mod`
errors rather than helping. `make lint` runs
`scripts/check-go-module-coverage.sh`, which reds the build if a new module ever lands in
this blind spot without a target.

## Release

`gardenio/scripts/release-cut.sh` is the current way to cut a release: it tags `origin/main`'s
head (when it is newer than the last calver tag) and pushes only the tag (`--dry-run`
previews). It asks no CI: `ci.yml` and `checks.yml` run on the tag push, not on `main`
pushes, and `release-deploy.sh` rolls a tag only once its own `images.yml`, `ci.yml` and
`checks.yml` runs are green; a red tag is never rolled: the deployer files one escalation per red
workflow, and the fix is a quarantine row on `main` plus a re-run of the tag's failed jobs, or a
revert and a new cut (owner 2026-11-03). `make release` is the laptop flow that also bumps
`Chart.yaml`: it runs the Landing-the-Plane checks (there is no CI preflight to call), then
`release.sh` bumps `Chart.yaml`, commits and tags. `make release-dry-run` previews it.

**Who cuts: only the release captain or the operator.** The standing release captain
(trigger gd-FgKpNn3A02, `BOAT_ROLE=release-captain`) and the operator cut, in the two daily
windows (~04:00Z and ~16:00Z, one cut each; every cut builds every image). A task crew never
pushes a release tag, not even for a check-in or deploy step. When its task is done only
once its merged PR runs, it sets `kd update <task> -f awaits_release=true` and stops: the
merge leaves the task open, and the deployment record of the first release that carries
the PR closes it, with a comment naming the release and target (P-R3, gd-ZM7NEBPAqZ;
[`gardenio/docs/release-pipeline.md`](gardenio/docs/release-pipeline.md#who-cuts-a-release)).
It files no "roll PR #N" `helm_deploy` escalation. `release-cut.sh` and `release.sh` refuse
(exit 4) any other `BOAT_ROLE`; `--dry-run` is always allowed (gd-CVotEQ3vk9).

One tag releases every component together: a calver tag push makes `images.yml` publish
every image as `:<calver>`. Nothing deploys automatically — there is no deploy workflow;
the k3s cluster is upgraded by hand with `helm upgrade`.

Non-obvious constraints (full runbook: [`gardenio/docs/release-pipeline.md`](gardenio/docs/release-pipeline.md)):

- **Atomic push.** `release.sh` never pushes; it prints `git push --atomic origin main
  <TAG>`. Publish with exactly that — never a bare `git push` — so a rejected `main` update
  can never orphan a tag off-`main` (ka-0000001299).
- **A `release-cut.sh` tag never bumps `Chart.yaml`'s `appVersion`**, so rolling one out
  needs `--set global.imageTag=<tag>` (`release-deploy.sh` always passes it); dropping the
  override is a rollback to `Chart.AppVersion`.

## CI

GitHub Actions is the only CI (RWX and `.gitlab-ci.yml` were deleted 2026-09-16, commit
8a3eb29). Workflows live in `.github/workflows/` and run on the k3s ARC runner pool:

- `ci.yml` — Go (controller, kbeads) and coop build + test, path-filtered per component.
  A PR runs only the light tier; the full suite runs on a release tag push, nightly and on
  dispatch (not on pushes to `main`).
- `checks.yml` — golangci-lint, shell guards + script self-tests, terraform, helm lint +
  chart tests, gitleaks, security scans, cargo audit/deny, quench.
- `images.yml` — never runs on a PR (gd-bjj3robkjs); a calver tag push publishes
  every image to ECR `123456789012.dkr.ecr.us-east-1.amazonaws.com/gardenio/<image>:<calver>`
  (plus `:latest` and `:<sha>`); nothing goes to ghcr. `workflow_dispatch` takes an `images`
  list to rebuild only some.
- `e2e.yml` — optional, never a required check: a nightly cron, or dispatch with a
  `suites` list (on a branch too); there is no PR label door any more. No cluster is
  deployed; the shell suite runs only its cluster-free (`HERMETIC_ONLY`) subset.
- `retry-lost-runners.yml` — re-runs jobs whose spot runner was reclaimed mid-job.

### CI on pull requests (owner rule, 2026-10-31)

Big CI runs on pull requests are not allowed. In the owner's words: "doing big ci runs on
MRs isn't allowed anymore and we're trying to get away from it; it's exploding our CI bill
for little benefit." On 10-27, PRs were about 75% of ~23,400 runner-minutes a day
(`docs/superpowers/plans/2026-10-28-ci-minutes-slimming.md`).

- Never start a heavy PR run on purpose. That means no empty commits to retrigger, no
  merging `main` into a PR branch to refresh it, no "Update branch", no `gh run rerun`
  sweeps, and no draft-to-ready flip just to get CI.
- A PR that is red only because `main` was red is not re-proved on the PR. Fix `main`;
  `main`'s own run is the proof.
- Open PRs as drafts (`gb pr open`). Every PR push, draft or ready, runs only the light
  tier: the gate jobs plus ci.yml's `light: build + vet + changed-package tests`. No
  heavy job runs on a `pull_request` event, and `ready_for_review` starts no run (Task 7.3
  part B, gd-19ECZxvFQw; `scripts/check-ci-draft-gate.py` guards it). The heavy jobs run
  on a release tag push, the nightly and `workflow_dispatch`. A PR that changes a heavy-only
  job proves it with one `workflow_dispatch` run on its branch, named in the PR body.
- The one sanctioned re-run or dispatch is `gb ci rerun <run>` / `gb ci dispatch <workflow>
  --ref <agent-branch>` (P9.1, gd-FsqKPpFgaf): ci.yml, checks.yml, images.yml or lands-perf.yml
  on an `agent/` branch, a run's failed jobs once at the branch head, at most 2 per PR per
  hour. Use it for a job that never reached a verdict, never to re-prove a red. A PR that
  touches `internal/commons` or `web/commons2.js` proves the Commons perf budgets (B12–B17)
  with one `gb ci dispatch lands-perf.yml -f surfaces=commonsthread,commonsplan`
  (`gardenio/docs/commons-ux-rubric.md` §7).
- A queue of red or stale PRs is not a reason to spend CI, or to raise `ci_max_nodes`.
- Where `docs/merge-desk.md`, a gardener prompt or a `gb` default says to retrigger or
  refresh a PR, this rule wins until they are rewritten.

### Flaky tests (owner rule, 2026-11-05)

"Flaky tests should be quarantined, we should add a rule." A test that goes red on a change
that could not have caused it gets a row, its own file in `.github/quarantine.d/`, the first
time anyone sees it: on a PR, a release tag or the nightly. Never re-run a job to see if a red test goes
green. A test that went green on a re-run is a flake, so give it a row. A red tag's job is
re-run only after its row is on main. Rules: `.github/known-failures.md`, "Quarantine". The
lane that fixes quarantined tests and takes them off the list: `docs/quarantine-warden.md`.

## No bake-in periods (owner rule, 2026-11-02)

The owner's words: "continuous development cannot survive mandatory waits". Plans, specs,
rollouts and handoffs never say "bake", "soak" or "wait N hours/days, then".

- Gate on an event or evidence: the first occurrence, N successes, one measurement, a test that
  trips. Go on as soon as it holds; if it goes wrong, revert or fix forward.
- Measuring after a change is fine, as long as nothing waits on it.
- A compatibility shim goes on an event ("no caller uses the old name"), not on a date.
- A deadline or time box ("tag within 3h or it ships nothing") is not a wait and is allowed.
- This holds unless the owner explicitly asks for a waiting period.

## New binaries and guard tests

These rules stop the code growing back after the audit (gd-eRFLdnAEeB, rules from
gd-kCs3VDmMWR). The first two are checked in CI by the `quench` job.

1. **A new `gardenio/controller/cmd/<x>` must ship or run.** Either an image builds it
   (add it to the bin list in `gardenio/images/_controller-base/Dockerfile` and COPY
   `/out/<x>` in a leaf Dockerfile listed in `.github/images.json`), or a workflow in
   `.github/workflows/` or an e2e module under `gardenio/tests/e2e/` runs it. A Makefile
   target does not count. If neither holds, it does not merge.
   `scripts/check-cmd-shipped.py` enforces this. Its `UNSHIPPED` list holds the binaries
   that predate the rule, each with the bead that wires or deletes it. Do not add to it.
2. **A new guard, lint, drift or invariant test must cite the incident or bead it
   protects against**, and must say why a normal unit test would not do. Prefer the
   unit test. `scripts/check-guard-provenance.py` checks the citation for
   `scripts/**/check-*`, `scripts/**/lint-*` and any `*_test.go` named for a guard,
   drift or invariant. Review checks the why.
3. **Comments say the contract and the why; history goes in beads.** Keep what the code
   cannot say: what a function promises, a non-obvious reason, and a bead or doc id.
   Do not retell incident history, bead trails or design debate in a comment; put it in
   the bead or a `gardenio/docs` file and cite the id (gd-EOIlesG6x0).

A new `scripts/check-*` guard follows the existing ones: a `_test` file next to it that
builds its own fixtures, including one that must fail, run from `checks.yml`.

## Commits

Short imperative subject; scope in parentheses with component prefix, e.g.
`fix(gardenio/bridge): handle nil bead metadata`, `feat(kbeads): add search command`,
`fix(coop): credential refresh race`.

## Landing the Plane

1. `make build` — all components must build.
2. `make test` — all tests must pass. **This is not every Go test** — see Build above.
3. `make test-out-of-workspace` — the modules outside `go.work` (required when touching
   `gardenio/helm/**`; `make test-helm` alone covers the chart/dashboard suite).
4. `make lint` — all linters must pass.
5. `helm lint gardenio/helm/gardenio/` — Helm chart must lint.
