# Gardenio

K8s agent controller and Slack bridge for beads automation — extracted from Gastown.

## Architecture

- **controller/** — Go module. K8s agent controller that translates bead lifecycle events into pod operations.
- **controller/internal/bridge/** — Slack notification bridge (decisions/mail watchers, Slack interactions). Zero K8s dependencies.
- **controller/cmd/** — entry points: `controller/` (single binary), `slack-bridge/` (standalone), `gb/`, `beads-viewer/`.
- **helm/gardenio/** — Helm chart for all components (controller, coopmux, slack-bridge, PostgreSQL, NATS).
- **images/** — Dockerfiles for agent pods and bridges.

## Directory Structure

```
controller/
├── cmd/                # entry points (controller, slack-bridge, gb, beads-viewer)
└── internal/
    ├── beadsapi/       # HTTP client to beads daemon (used by controller + bridge)
    ├── bridge/         # Slack notifications + trigger engine (trigger_source_*.go)
    ├── podmanager/     # Pod spec construction & CRUD against K8s API
    ├── reconciler/     # Periodic desired-vs-actual sync loop
    ├── statusreporter/ # Pod phase → bead state updates
    └── subscriber/     # SSE/NATS event listener for bead lifecycle events
helm/gardenio/           # Helm chart (controller, coopmux, slack-bridge, postgres, nats)
images/                 # agent/ + bridge Dockerfiles
```

The **trigger engine** (`internal/bridge/trigger_source_*.go`) fires controller actions
from JIRA/GitLab/webhook/bead/account/schedule/Slack events. References:

- [`docs/gardener-authoring-guide.md`](docs/gardener-authoring-guide.md) — **canonical front-door for authoring a gardener** — the one command, the five non-negotiable rules (self-block, cadence, blob-not-flat-fields, repo-less, prompts-as-advice), wake recipes, and an authoring checklist. Read this first; the runbook is the deep reference it points to.
- [`docs/triggers.md`](docs/triggers.md) — operator-facing schema, sources, actions, engine semantics.
- [`docs/agent-triggers-guide.md`](docs/agent-triggers-guide.md) — agent tutorial for condition-driven gardeners + the schedule/trigger field reference.
- [`docs/gardener-loop-runbook.md`](docs/gardener-loop-runbook.md) — scheduled-gardener primitive (`triggers:`/`stop:`/`progress:` on a `type=trigger` bead; `type=schedule` firing was removed in Phase 3b/D10).
- [`docs/plan-authoring-runbook.md`](docs/plan-authoring-runbook.md) — `/plans` authoring lifecycle (draft → ready → build). Plan is a three-layer split (epic ka-0000000187): daemon owns plan STATE + invariants, one controller reactor (`internal/planagent/`) owns REACTIVE collaboration, beads-viewer RENDERS + thin write-proxies. Every `gb plan` write verb is daemon-direct under the caller's own bearer (`BEADS_AUTH_TOKEN` is the client-side env var name; the daemon no longer accepts a shared static token, gd-ke6PC7bRto), so a headless gardener drives a plan end-to-end (ka-0000006389).
- [`docs/beads-viewer-dev.md`](docs/beads-viewer-dev.md) — local dev loop (`make run-beads-viewer`).
- [`docs/bridge-token-runbook.md`](docs/bridge-token-runbook.md) — bridge token lifecycle (issue/rotate/revoke/audit).

## Build

```sh
cd controller && go build ./cmd/controller/    # controller binary
cd controller && go build ./cmd/slack-bridge/   # slack bridge binary
make test                                       # run all tests
quench check                                    # quality checks
```

## E2E test suites (Playwright)

Three Playwright suites live under `tests/`. The first two reuse the same chromium base
image + `gardenio/locks/e2e-system-deps.lock`.

- **`tests/beads-viewer-e2e/`** — behavioral suite for the beads-viewer. Auto-boots
  the viewer (`webServer:`) + seeds a local daemon. Run: `make test-beads-viewer-e2e`.
- **`tests/gardenhub-browser/`** — hermetic Chromium checks for the `/garden` Homestead page,
  driven from Go (`internal/gardenhub/card_browser_test.go` boots the page on an httptest server
  with the fixture farm; no daemon, no viewer binary). Only `playwright-core` lives here. Run:
  `make test-gardenhub-browser`; without its `node_modules` the Go test skips, unless
  `GARDENHUB_BROWSER_REQUIRED=1`, which `ci.yml`'s `gardenhub-browser` job sets so it fails instead.
  The same job runs the **golden-screenshot gate** for `/garden` and for the viewer's empty composer
  (`make test-garden-goldens`, on a release tag, the nightly and dispatch, never a PR). **Never commit `testdata/golden`
  in a feature PR**: the light tier fails it (`scripts/check-no-pr-goldens.py`, gd-fedEoEGC9i), and
  `.github/workflows/bless-goldens.yml` re-shoots the goldens on main before each release window. Render-check locally
  with `make update-garden-goldens` if you like, then drop the PNGs. How-to and how to add a surface:
  [`tests/gardenhub-browser/GOLDENS.md`](tests/gardenhub-browser/GOLDENS.md).
- **`tests/slack-e2e/`** — **real-user Slack E2E** (epic `ka-0000006991`). Drives the
  real `app.slack.com` web client as a dedicated test human user (injected
  `storageState`) against a dedicated test workspace/app wired to the ephemeral
  `slack-bridge`, and asserts the controller/bridge react over Socket Mode.

### Running slack-e2e

This suite is the `slack-ui` E2E module — **you do not run it directly**. Its
runner [`tests/e2e/scripts/test-slack-ui.sh`](tests/e2e/scripts/test-slack-ui.sh)
gates the whole rig and **skips cleanly** (exit 0) when any piece is absent —
the Playwright scaffold, `node`/`npx`, a live `slack-bridge` (`BRIDGE_HTTP_URL`),
or the materialized test-user session Secret — mirroring the `SLACK_BOT_TOKEN`
guard on the sibling bash Slack scripts. So local dev, fork CI, and un-opted-in
ephemeral namespaces stay green. No CI workflow runs it today: the RWX E2E pipeline that
dispatched it was deleted 2026-09-16, and `.github/workflows/e2e.yml`'s `shell-hermetic`
job runs only the cluster-free subset. Against a live namespace, run it through the shard
runner (the rest of the required env is in that script's header):

```sh
# from the repo root; slack-ui is pinned to shard 2 (Socket Mode serialized)
ONLY=slack-ui E2E_SHARD=2 ./gardenio/tests/e2e/scripts/run-shard.sh
```

The runner extracts the captured session out of the ESO-synced K8s Secret
`slack-e2e-session` into a private temp file, exports the scaffold's env
contract (`SLACK_E2E_STORAGE_STATE` / `SLACK_E2E_BRIDGE_URL` / `SLACK_E2E_CHANNEL`
/ `BEADS_HTTP_URL`), then runs `npm ci` + `npx playwright install --with-deps
chromium` + `npm test`. **Fail-loud override:** set `SLACK_UI_REQUIRE=true`
(scheduled readiness dispatches only) to turn an absent rig into a hard failure
instead of a skip.

- **Provisioning + session-refresh runbook** (the `d`/`d-s` cookies expire —
  this is the most likely ongoing failure): [`docs/slack-e2e-provisioning.md`](docs/slack-e2e-provisioning.md).
- **Suite details** (env contract, slack-bridge fixtures, layout):
  [`tests/slack-e2e/README.md`](tests/slack-e2e/README.md).
- **Plan of record:** [`plans/slack-e2e-playwright.md`](plans/slack-e2e-playwright.md).

## Credential Architecture

The gardenio controller is the single source of truth for agent credentials:

1. **Account beads** define credential accounts (provider, env_key, API key).
2. **Controller** watches account bead SSE events and syncs to coopmux via
   `POST /api/v1/credentials/set` (startup + periodic + real-time).
3. **Coopmux credential broker** stores API keys and distributes them to sessions as
   profiles with pool load balancing.
4. **Per-project secrets** (K8s Secrets) are declared on project beads via the `secrets`
   JSON field and injected as env vars at pod creation.

Account identity is established at session registration (`account_name` field); the health
poller reconciles drift from credential profile switches.

> **Gotcha:** the `--credential-config` static JSON file is **deprecated** — credentials
> sync from the controller via the API, not from a config file.

## Environment variables

| Variable | Default | Purpose |
|---|---|---|
| `BEADS_HTTP_ADDR` | `http://localhost:8080` | Beads daemon HTTP address |
| `NATS_URL` | `nats://localhost:4222` | NATS server URL |
| `SLACK_BOAT_TOKEN` | *(optional)* | Slack bot OAuth token |
| `SLACK_CHANNEL` | *(optional)* | Slack channel for notifications |

## Release (Golden Release Path)

One canonical path, driven from repo root — `make release` (see the root `CLAUDE.md` and
[`docs/release-pipeline.md`](docs/release-pipeline.md) for the full runbook). Formula
template: `kd formula apply ka-0000000359 --var version=YYYY.DDD.N --http-url https://gardenio.example.com/kd`

Images build and publish in `.github/workflows/images.yml` (to ECR
`123456789012.dkr.ecr.us-east-1.amazonaws.com/gardenio/<image>`); re-run a single failed image with its
`workflow_dispatch` `images` input. There is no deploy workflow: the k3s cluster is upgraded
by hand, per [Current k3s Deploy + Rollback](docs/release-pipeline.md#current-k3s-deploy--rollback-2026-09-20-gha-based).

**Deployment repo**: a wrapper chart in your deployment repo that depends
on the upstream OCI chart.

## Agent Image

The agent pod image has one build, `images/agent/Dockerfile`, used both locally and by
`.github/workflows/images.yml`; it sources `images/agent/install/versions.env` and calls the
shared install scripts. CI builds it only for calver tags and `workflow_dispatch`, never on
PRs. Full build reference — shared install scripts, the Dockerfile stages, how to
add/update/verify a tool, and the monorepo-built image list — is in
[`docs/agent-image.md`](docs/agent-image.md).

Sharpest gotchas:
- **Dockerfile stages are isolated.** Each builder stage starts from a clean image — they
  cannot share installed packages. If a stage needs cmake, install it in that stage.
- **Helm `default` with empty strings**: Helm's `default` does NOT treat `""` as falsy. Use
  `{{ if .Values.x }}…{{ else }}…{{ end }}`, not `{{ .Values.x | default … }}`.

## Before Picking Up Any Task (Agents)

> **Gotcha:** `gb ready` is scoped to your project (`$BOAT_PROJECT`/`$KD_PROJECT`) by
> default — pass `--all-projects` **only** if you deliberately want the cross-project
> queue, and even then work only tasks in your assigned project unless told otherwise.
> (The `gb news` → `kd claim` boot workflow is already in your session prompt; not
> repeated here.)

## New binaries, guard tests and comments

The repo-root `CLAUDE.md` ("New binaries and guard tests") has the full rules; in short:

- A new `controller/cmd/<x>` ships in an image (`images/_controller-base/Dockerfile` bin
  list plus a COPY in a leaf Dockerfile) or is run by a workflow or an e2e module under
  `tests/e2e/`. Otherwise it does not merge; `scripts/check-cmd-shipped.py` reds the
  `quench` job.
- A new guard, lint, drift or invariant test cites the incident or bead it protects
  against and says why a normal unit test would not do (`scripts/check-guard-provenance.py`
  checks the citation).
- Comments carry the contract and the non-obvious why, plus a bead or doc id. History
  goes in the bead.

## Commits

Short imperative subject; scope in parentheses: `fix(bridge): handle nil bead metadata`.

## Landing the Plane

1. **Build** — `make build` and `make build-bridge` must succeed.
2. **Test** — `make test` must pass.
3. **Chart + dashboard tests** — `helm/gardenio/tests` is its own Go module and is NOT in
   the repo-root `go.work`, so a root `go test ./...` reports success having never
   compiled it (bug ka-0000008045 — a green root run shipped and CI came back red on
   `test-helm` with 6 failures). Run `make test-helm` from the repo root, or
   `GOWORK=off go test -count=1 .` from inside `helm/gardenio/tests` (~5 min). Mandatory
   when touching `helm/**` or any Grafana dashboard. The agent image ships `promtool` at the
   pin `checks.yml`'s `helm` job uses (`PROMTOOL_VERSION`, gd-hZl1cKiELM); on an image older
   than that, a local run `t.Skip()`s the three promtool firing-proof tests (advice-budget,
   fsm-composite, eks-api-intrusion), which CI still runs.
4. **Quench** — `quench check` must pass. CI's `checks.yml` `quench` job runs
   `quench check --ci` from the repo root (as does `make lint`). **Gotcha:** any
   escape-suppression pragma (`# pragma: no cover`, a `panic(`, etc.) needs a justification
   comment on the **preceding line**, or quench's `suppress_missing_comment` rule reds every
   PR branched off that main until it is fixed (ka-0000004491). Run `quench check --ci` from
   the repo root before pushing when touching Python/litellm.
5. **Helm lint** — `helm lint helm/gardenio/` must pass.
6. **Follow existing patterns** — bridge code in `internal/bridge/`, K8s logic in
   `internal/podmanager/` and `internal/reconciler/`.

## Advice Hygiene

`gb prime` injects every matching advice bead into every session, so bloat there steals
context from the code the agent is reasoning about. When creating/editing/reviewing advice
beads (`kd list --type advice`), follow the hygiene rules and the label-targeting contract
in [`docs/advice-hygiene.md`](docs/advice-hygiene.md). The runtime ceiling is enforced in CI
by `TestPrimeOutputSizeBudget` and the real-catalog budget tests — those are the enforcement
mechanism, not a hand-rolled char limit. Check with `gb advice lint` / `gb advice audit`
before adding or relabelling.
