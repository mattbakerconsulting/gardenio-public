# Gardenio Release Pipeline

## How to Cut a Release (tag-only, GHA-based)

`gardenio/scripts/release-cut.sh` is the current way to cut a release tag.
It replaced the standing release gardener (which had run three times and cut
nothing, waiting for a provably green `main`). The script tags `main`'s head
and pushes the tag, so it never touches `main` and never bumps `Chart.yaml`.

```bash
gardenio/scripts/release-cut.sh --dry-run   # preview: what would be tagged
gardenio/scripts/release-cut.sh             # tag + push refs/tags/<TAG>
```

**The release tag is what gets tested, not `main`** (owner, 2026-11-03: "we
don't care for ci on main, rather on the release branch"). `main` moves every
few minutes, so a `main` push run was nearly always replaced by the next
merge before it finished: of the last 30 `ci.yml` push runs on `main`, 19 were
cancelled and 9 succeeded (`checks.yml`: 10 of 15 cancelled). The old cut
tagged only a commit with green `main` runs, so it kept landing on commits
hours old. Now:

1. **Cut = tag `main`'s head.** `git fetch origin main --tags`; if
   `origin/main` has first-parent commits newer than the last calver tag it
   contains, its head gets the next calver tag (`YYYY.DDD.N`, same scheme as
   `release.sh`), created as an annotated tag and pushed as `refs/tags/<TAG>`.
   No CI is consulted and no branch is ever pushed.
2. **The tag's own runs gate the deploy.** The tag push (`push: tags:
   ['20*']`) runs `images.yml`, `ci.yml` and `checks.yml`, each as a full
   run: every path gate open and its own concurrency group, never cancelled.
   `release-deploy.sh` rule 4 rolls the tag only once all three have a
   successful push run of the tag at its commit, and a `ci.yml` run counts
   only if it really built (a `heavy work starting` step succeeded and none
   was skipped). Until then each deployer tick skips (exit 3) and names the
   workflow that is not green, e.g. `ci.yml not green: ... (newest there:
   in_progress/-)`.
3. **A red tag is never rolled, and never goes quiet** (owner, 2026-11-03).
   While the tag's runs are pending or in progress, every deployer tick skips
   quietly. Once one of the three has a **completed** push run at the tag's
   commit that did not succeed (`failure`, `timed_out`, `cancelled`,
   `startup_failure`, `action_required`) and no run of it succeeded,
   `release-deploy.sh` (rule 10) files **one** escalation per red workflow:
   title `Release <tag> red: <workflow> failed`, `idempotency_key`
   `release-red:<tag>:<workflow>`, `operation_type` `github_actions_rerun`
   (the escalation policy holds it in the `coordinator:laptop` lane). Its
   context names the tag, the commit, the run URLs and the failed jobs, and
   the recovery rule below. Filing is best effort (a failure is logged, the
   exit stays 3) and never happens under `--dry-run`.

   **Recovery**, one of two paths:
   - **Narrow failure** (one or a few tests): add a row to the shared
     quarantine list on `main` (its own file in `.github/quarantine.d/`; the failing job's
     "quarantine rows" step prints it, and `scripts/quarantine.sh` is how CI
     reads `main`'s copy), then re-run **only** the failed jobs of the tag's
     run: `gh run rerun <id> --failed`. The tagged code is unchanged; only
     `main`'s quarantine list changes. No new cut.
   - **Obvious large regression attributable to one PR** (build or compile
     broken, failures across many packages, a whole job down rather than a
     test): revert that PR's merge on `main` and cut a new tag. The deployer
     always targets the newest tag, so it moves on by itself.

   The escalation's result records the path chosen and the culprit PR.
4. **`main` pushes run neither `ci.yml` nor `checks.yml`.** The nightly
   `schedule` run (10:43Z) is the one full `main`-head run a day; PRs still
   run both, and `workflow_dispatch` runs them on demand.

**CI tiers.** A pull request runs only the light tier (the gate jobs and
`ci.yml`'s `light: build + vet + changed-package tests`). A calver tag push
runs everything in `ci.yml` and `checks.yml`, with one exception (owner
2026-11-01, gd-rmHsVjmKse): the `gardenhub browser (chromium)` suite runs only
when a `gardenhub_browser` path changed since the newest earlier calver tag
whose suite **passed** (a skipped suite never counts, so skips cannot chain).
`.github/actions/last-tag-base` finds that tag and the `gate` job's log
step `browser suite on this tag?` names it and the verdict; no such tag, an
API or diff error, or a non-calver ref all run the suite. A skipped suite is
`skipped`, which rule 4 accepts. The 10:43Z nightly runs everything on
`main`'s tip, the browser suite included, and is the backstop:
`.github/workflows/daily-full-run.yml` keeps one pinned issue, **"Daily full
CI run: red"** (label `ci-daily`), open with the failed jobs and run links
while either nightly run is red and closes it once both are green; the
README badges show the same two runs. `scripts/check-main-concurrency.py`
and `scripts/check-ci-draft-gate.py` pin the exception and its fail-open arms.

Exit codes:
- `0` — tag pushed (or, under `--dry-run`, would have been).
- `3` — nothing to release. This is a **normal** outcome, not an error: it
  means `origin/main`'s head is the last release.
- `1` — a `git` command failed; the failing command is named in the error.
- `4` — refused: an agent session that is not the release captain (see
  below).

This only creates the tag; it does not build images, publish the Helm chart,
or deploy anything. `images.yml`, `ci.yml` and `checks.yml` run off the tag
push itself. See the next section for how the running cluster picks up a new
tag.

> **A `release-cut.sh` tag never bumps `Chart.yaml`'s `appVersion`.** A plain
> `helm upgrade` with no override deploys `Chart.AppVersion`, which stays at
> whatever `release.sh` last pinned. So every rollout of a release-cut tag must
> pass `--set global.imageTag=<tag>` (`release-deploy.sh` always does), and
> dropping the override is a **rollback** to `Chart.AppVersion`, not a
> roll-forward.

### Who cuts a release

Only the standing **release captain** (trigger gd-FgKpNn3A02, role
`release-captain`, plan gd-adrgCGsfkP) and the **operator** cut a release
tag. Cuts happen in two daily windows, ~04:00Z and ~16:00Z, one cut per
window: every tag makes `images.yml` build every image, so an extra cut is
real ECR and runner cost.

A task crew never pushes a release tag, whatever its step says. A task that is
done only once its change runs:

1. lands its change on `main` as usual (`gb pr open`, `gb pr ready`);
2. sets `kd update <task> -f awaits_release=true` (P-R3, gd-ZM7NEBPAqZ);
3. `gb stop`s. The stop gate lets an awaits-release task through.

The PR's merge, and the merge escalation's close-on-execute, leave such a task
open with a comment. When a deployment is recorded (`POST
/v1/releases/<tag>/deployments`, from any row in the table under
[The deployment](#the-deployment-where-a-release-runs)), kbeads closes every
open task and escalation of the project with `awaits_release=true` whose work
the deployed release carries, with a comment naming the release and target
(`kbeads/internal/server/releases_awaits.go`). "Carries" is read from the
contents rows of the deployed release and of every release on its `previous`
chain, which together are the merges that are ancestors of its tag:

- a task is carried when each merged merge-request bead it produces is in a
  row (by `mr_bead`, `merge_sha` or `url`) and none of its PRs is still open;
  a task with no merged merge-request bead is carried when a row names it as
  `task` or `step`;
- an escalation is carried when the PR its `operation_spec.mr_url` names is in
  a row, else when its `requesting_task` is carried.

So a rollback to an older release closes nothing the older release does not
carry, and a release whose record misses a PR (no contents, or truncated
contents, `contents_complete=false`) closes nothing for it. Work to do after the
deploy goes in a step the task `blocks`; the task's close unblocks it. File no
"roll PR #N" `helm_deploy` escalation for a change that rides the next window's
tag.

Enforcement, and its limit:

- `release-cut.sh` and `release.sh` source `release-role-guard.sh`, which
  refuses (exit 4) when `BOAT_ROLE` is set and does not contain
  `release-captain`. The operator's laptop has no `BOAT_ROLE` and passes;
  `GARDENIO_RELEASE_OPERATOR=1` is the operator's override for a shell that
  exports one. `--dry-run` is never refused.
- That does not stop a raw `git push origin <tag>`. A GitHub tag ruleset
  would, but only once the captain pushes as a different identity from the
  crews: today every agent pod and the operator push as the same account
  (the images.yml runs for 2026.297.1 operator, 2026.297.2 captain and
  2026.298.1 crew all show actor `joyousaversion`), so a ruleset cannot tell
  them apart (gd-CVotEQ3vk9).

### Who rolls a release

The **`release-deployer` CronJob** rolls every release (owner ruling 4 on
gd-7qUpu15BBK, P-R4 gd-iK8RnuLG3W). Every 15 minutes it resolves the newest
calver tag and runs that tag's `gardenio/scripts/release-deploy.sh`, whose
gates (pause, helm busy, not newer, values drift, tag not green) decide
whether it rolls. Each run leaves a `release-deploy <tag>: <outcome>` event
bead.

The **operator's laptop** rolls only two kinds of release:

- a **floor-ordered** one: a sequence the operator orders step by step, such
  as a security change that must roll, be checked, then be followed by its
  next step (#1686, then gd-gdf3L8kjT5);
- a **failed** one: a tag the deployer could not roll, i.e. a values-drift park
  (rule 3b), a failed roll or rollback, or a gate the deployer cannot pass by
  itself.

After a laptop roll the laptop does **not** re-create the pause ConfigMap or
set the CronJob's `suspend`. The deployer's next run finds the tag already
deployed (rule 3, not newer) and skips; the tag after it rolls on its own.

The pause switch is an **emergency lever only**:

```bash
kubectl -n gardenio create configmap release-deployer-pause   # stop all rolls
kubectl -n gardenio delete configmap release-deployer-pause   # resume
```

Create it when no tag may roll (an incident, or a floor-ordered sequence a
newer tag must not overtake), say so on the incident or escalation bead, and
delete it as the last step of that same bead. While it exists every run exits
3 at rule 1.

### The release bead: what a cut carries

Every cut also writes a `type=release` bead
([`releases-design.md`](releases-design.md), plan gd-UZKZ9H25UD step 3).
`release-cut.sh` runs `gb release record <tag> --project gardenio` after the
tag push. `release.sh` never pushes: run that command yourself after
the push. The record carries the tag's commit, the previous release tag and
the merged PRs between them on main's first-parent line, each joined to its
merge-request bead, task, plan step and plan (a merge train counts as the PRs
it carries). Each plan whose steps shipped gets a `Shipped in gardenio <tag>`
comment. A failed record is a warning, never a failed cut. After the record,
`release-cut.sh` also runs `gb release backfill --last 8`
(`RELEASE_BACKFILL_LAST`), so a cut whose own record failed is filled by the
next cut. Tags already recorded are skipped. The backfill runs with `--built`:
each of those tags whose `images.yml` push run is green is marked built at that
run's finish, with one image ref per entry of the tag's own `.github/images.json`
(`IMAGE_PREFIX` from the tag's `images.yml`, or `RELEASE_IMAGE_REGISTRY`). The tag just cut has no green run yet, so the next
cut marks it (gd-F7kqCQPhLt). A gb without `--images-from-tag` gets a warning
and a records-only backfill. The operator laptop's 2026.303.1,
304.1, 304.3 and 305.1 went unrecorded this way (gd-SLPVJe1TRh): a gb older than
2026.304.1 has no `release record`. Keep the laptop's gb current: a gb older
than 2026.308.5 records a cut as a fresh release beside the open one and binds
no gardener (2026.308.5 to 308.12, gd-6Z9rCUcfqS), so `release-cut.sh` now
refuses before it tags (exit 5, "Update gb") when `gb release record` has no
`--no-gardener` flag.

The `gb release` verbs (`gardenio/controller/cmd/gb/release.go`, `release_record.go`,
`release_smoke.go`):

| Verb | What it does | When it runs | Who runs it |
|---|---|---|---|
| `gb release record <tag>` | writes the release with its contents (`source=script`) and comments `Shipped in <project> <tag>` on each plan it carries; on a release written by hand it fills only the missing `contents`, `previous_tag` and `sha`. When the project has an open release and the tag is above every recorded one, the tag is cut onto that bead in one write (its contents reconciled against git: git's rows win, and a row a merge added that git does not show is dropped and listed); either way it then opens the next release (plan gd-lgN4U7X5Tb). github-bridge's tag webhook and poll do the same | after every cut: `release-cut.sh` runs it after the tag push; after a `release.sh` push, run it yourself | the release captain or the operator (the script runs as them) |
| `gb release built <tag>` | sets `built_at` (status cut → built) from the finish of the tag's successful `images.yml` push run, or `--at`; `--images-file .github/images.json --registry <ecr>/gardenio` adds the image refs. No green run yet: exit 3, nothing written | `release-cut.sh`'s backfill marks the previous cuts (below); by hand, once `images.yml`'s tag run is green, to mark the newest cut before the next one. `--images-from-tag` reads the images file at the tag | the captain or the operator |
| `gb release backfill [--since <tag> \| --last 30] [--built]` | the records of older tags, oldest first (`source=backfill`), with no plan comments; tags already recorded with contents are skipped. `--built` (with `gb release built`'s image flags) marks each built whose `images.yml` run is green and leaves the rest cut | after every cut (`release-cut.sh` runs `--last 8 --built`), and by hand to fill history | the captain or the operator |
| `gb release backfill-deployments [--since <time>] [--target prod]` | replays each `release-deploy <tag>: deployed and verified` event bead as a deployment row (`by=backfill`) at the revision it names; a revision already recorded, or held by another release, is skipped | by hand, after `gb release backfill`, when event beads exist that have no row (`--dry-run` first) | the captain or the operator |
| `gb release open [project]` | opens the project's next release (a release bead with no tag, status `open`); a no-op that prints it when one is open. Either way it binds the release's gardener (below; `--no-gardener` skips it) | `gb release record` opens the next after every cut; by hand for a project's first | the captain or the operator |
| `gb release gardener [project]` | binds the open release's gardener (`--release <id>` for another release) when it has none, and makes the release name it in `gardener_id`; `--paused` binds it disabled | `gb release open` and `gb release record` run it on the release they open; by hand for a release opened without one (`kd release open`, github-bridge) | the captain or the operator |
| `gb release next [project]` | prints the open release and what it carries so far, by plan | any time | anyone who can read the project |
| `gb release smoke [--namespace gardenio]` | six read-only prod probes (pods, controller, slack-bridge, a daemon round trip, a `gb news`-shaped query, prod drift) as one pass/fail JSON; non-zero exit on any failure | by hand, after a roll, to check prod | anyone with read access to the namespace. Its `prod-drift` probe still reads the golden tag from GitLab (`--project example-org/gardenio`, `GITLAB_BASE_URL`), so on GitHub it cannot pass; read probes 1 to 5 |

And on the daemon side, from `kd`:

| Verb | What it does |
|---|---|
| `kd release list --project gardenio` / `kd release show <tag> --project gardenio` | read the releases, newest cut first, and one release with its deployment rows (they replace the retired `gb release status`) |
| `kd release create --project <p> --tag <t> --sha <sha>` | record a release by hand (`source=manual`); `gb release record` fills its contents |
| `kd release open <project>` | open the project's next release, as `gb release open` does |
| `kd release deploy <tag> --project <p> --target prod --revision <n>` | record a deployment row by hand, `via=script` |
| `kd release repair-deployments --project <p> [--dry-run]` | rebuild a project's deployment history to one row per Helm revision per target ([`releases-design.md`](releases-design.md#31-the-release-bead)) |

All of these are idempotent: a second run writes nothing. Only the release captain, the operator and
`gardenio-admin` may write releases (the deploy role may also record deployments and run the
repair); `--dry-run` prints what would be written and is allowed for anyone. The `gb` verbs read
history from the checkout, so fetch the tags first (`git fetch --tags`).

### Each open release's gardener

An open release has its own gardener (plan gd-lgN4U7X5Tb, step gd-BS9fpWa1bx;
`gardenio/controller/internal/releasegardener`), much as a plan does. It is a `type=trigger`, pinned by
the `gardener-release:<release>` label, that spawns the `release-captain` role, so `release-cut.sh`
and `release.sh` admit its runs. It wakes on the release bead changing (a PR boarding, the cut,
`built`, a deployment row), throttled to once an hour, and in the two cut windows (`0 4,16 * * *`
UTC). A run that cut a tag waits on the tag's runs with `gb wait ci` and is resumed when they
conclude. Its steps are the advice labelled `release-gardener-playbook`, which the prompt points
at: cut in a window when the release carries PRs (one cut per window), `gb release built` once
`images.yml` is green, one escalation per red workflow (never a re-run), check the `Shipped in`
comments, and retire when the release is `live` or `superseded`, or skipped (never deployed
while a newer release went out: `kd release show <id> --json` names it in `skipped_by`). The run that retires stops with
`goal_satisfied: yes`, which closes the trigger. Its backstops are 16 runs and a week of wall clock.

`release-captain` is a privileged spawn role, so the trigger fires only with a spawn-lineage seal.
An operator's OIDC `gb release open` or `gb release gardener` seals it; that is the root of a
chain. After that, each gardener seals its successor (decision gd-cDG0zTiNSk, "succession";
kbeads `spawn_lineage_release_succession.go`). When a run of release N's open, sealed gardener binds
release N+1's gardener on its own token (`release-cut.sh`'s `gb release record`, or `gb release
gardener`), the daemon seals N+1's gardener to the same admin with the same `iat`. Succession never
extends a seal, so the chain lapses one seal TTL (30 days) after an admin last sealed a link, and an
admin's `gb trigger reseal` or OIDC bind restarts it. A release gets one sealed gardener. A gardener
that github-bridge or any other caller binds is not sealed, and its first fire raises the usual
reseal escalation.

### The deployment: where a release runs

Each release bead also lists where it runs: one deployment row per Helm
revision per target (`prod` for gardenio), written when something rolls
([`releases-design.md`](releases-design.md#43-the-deployment-for-any-project),
plan gd-UZKZ9H25UD steps 4 and 4f). `kd release show <tag> --project gardenio`
prints them; the open row on a target is what runs there. A re-roll of the same
tag at a new revision closes its own row and opens one. A revision already
recorded changes nothing, so reporting a roll twice is harmless; a revision
another release holds is refused (409).

| Who rolls | What records the deployment |
|---|---|
| the `release-deployer` CronJob | `release-deploy.sh` (rule 9), after `verify-image-tags.sh` passes: the tag at the new Helm revision. After a rollback it records the tag that ran before at the rollback's revision, `by=rollback`. The `release-deploy <tag>: <outcome>` event bead stays the log of the run. The CronJob runs every 15 minutes; see [Who rolls a release](#who-rolls-a-release). |
| the operator, for a `helm_deploy` escalation | the close: `gb escalation close <id> --status success --deployed-tag <tag> --revision <helm rev>` |
| anyone, by hand (`helm upgrade`, `helm rollback`) | the controller's release reconciler, within about ten minutes: `by=observed`, `via=reconciler` |
| history before step 4 | `gb release backfill-deployments --project gardenio` replays the `deployed and verified` event beads (`--dry-run` first) |

The CronJob and the controller write as their machine principals, which hold
the `gardenio-machine-release-deploy` realm role (`deploy` on `type=release`:
record a deployment, never record or edit a release). A tag with no release record cannot be deployed: record it first
(`gb release record <tag>`), and the reconciler fills the row on its next pass.

### What the captain sees

After a cut:

- `release-cut.sh` prints the record it wrote (`gb release record`), then
  `TAG=<tag>`. `kd release show <tag> --project gardenio` reads it back:
  status `cut`, its contents, `Deployments: none`.
- On `/garden`, within about a minute (the farm reads releases every 60 s),
  gardenio's truck stands loaded in the bay at the foot of its barn, in the
  project's home strip: a crate per plan, the tag's tail on its gold side
  plate, the PR count on the cab's door, and the board `CUT 1M` over the cab.
  At the Project level the sign says "N releases, 1 shipping". Hover or focus
  the truck for its tip; click it, or Enter on it, for the changelog by plan
  and step.
- Every plot whose newest merged PR the release carries gets the shipped
  stamp (a crate on a plate at its top-left fence), and its sign says
  "shipped in <tag>", with no target yet.
- In beads-viewer, `/releases` lists the release first as `cut` (`built`
  once `gb release built` has run), and `/release/<id>` shows its contents by
  plan and step and "No deployments yet".

After the deploy:

- A deployment row is written: by `release-deploy.sh` as soon as the roll
  verifies, by a `helm_deploy` close, or by
  the controller's reconciler about ten minutes after a hand roll. The
  release reads `live`; its Release page's Deployments timeline shows `prod`,
  the Helm revision and since when, and the release before it turns
  `superseded`.
- On the next farm read the loading truck drives out east and comes back
  empty, and an open `/garden` toasts "Released <tag> to prod (gardenio)". It
  parks with a gold board, `PROD 1M`.
- The shipped plots' signs gain the target: "shipped in <tag> · prod".
- Open work with `awaits_release=true` that the release carries closes, with
  a comment naming the release and the target.

A re-roll of the same tag (a values change, a restart through Helm) adds a
row at the new revision; the truck stays parked.

### Calver convention

`YYYY.DDD.N`: the UTC year, the UTC day-of-year with no leading zeros, and a
per-day counter starting at 1 (e.g. `2026.265.2`). `release.sh` and
`release-cut.sh` compute it the same way, from `date -u`, so a laptop and an
in-cluster job agree on the day.

## Current k3s Deploy + Rollback (2026-09-20, GHA-based)

> The retired GitLab/RWX/ACME pipeline (`registry.gitlab.com`,
> `acme-helm-chart`, RWX deploy tasks, the single-flight release lock, the
> old "Release Steps"/Overview docs) has been removed from this file — none
> of it runs anymore. The live cluster today is the self-managed k3s cluster
> (`gd-ljoVmxrW8O`). There is no GitHub Actions deploy workflow; everything in
> `.github/workflows/` is CI-only. The in-cluster `release-deployer` CronJob
> rolls each new tag (see [Who rolls a release](#who-rolls-a-release)); the
> commands below are the hand roll the laptop uses for floor-ordered and
> failed releases.

Chart: `gardenio/helm/gardenio`. Values: `deploy-values/gardenio-k3s.yaml`.
Release name: `gardenio`. Namespace: `gardenio`. With `global.latestMode`
retired (commit 96a2ad8), the chart resolves each image as
per-component tag > `global.imageTag` > `Chart.AppVersion` — so a plain
upgrade with no override always deploys whatever calver `Chart.AppVersion` is
pinned to, and `--set global.imageTag=<calver>` pins to any other calver on
demand (rollback *or* forward).

### Deploy / rollback / roll-forward, one invocation family

```bash
# Normal deploy: whatever calver Chart.yaml's appVersion is pinned to.
helm upgrade gardenio gardenio/helm/gardenio \
  -f deploy-values/gardenio-k3s.yaml \
  -n gardenio

# Rollback (or pin forward to any specific calver): override the tag.
helm upgrade gardenio gardenio/helm/gardenio \
  -f deploy-values/gardenio-k3s.yaml \
  -n gardenio \
  --set global.imageTag=<calver>

# Drop the override: falls back to Chart.AppVersion. That is only a
# roll-forward when Chart.AppVersion is newer than the pinned tag; after a
# release-cut.sh release it is a rollback (see the note above).
helm upgrade gardenio gardenio/helm/gardenio \
  -f deploy-values/gardenio-k3s.yaml \
  -n gardenio
```

### Rollback caveats: the image registry

Release images live only in ECR (`123456789012.dkr.ecr.us-east-1.amazonaws.com/gardenio/<c>`):
since AWS cost plan Task A5 (2026-10), `images.yml` no longer mirrors calver tags to ghcr,
and `scripts/check-image-registry.py` (`no_ghcr`) fails CI if a ghcr login or tag comes back.
On a calver tag, `images.yml` also cosign-signs each image it pushes, keyless and by digest;
see [`image-signing.md`](image-signing.md).

- A calver rollback (`--set global.imageTag=<calver>`) is unaffected for any calver cut
  after Task A3 merged; those are all in ECR. A calver cut before A3 exists only on ghcr.
- **Registry rollback** (pulling from ghcr again): revert the A5 PR and the A4 PR together,
  then cut a release, so ghcr gets the current calver again. The ghcr pull-through rule
  and its pull secret are kept on purpose; they are this rollback path. Do not delete them.

### Rollback caveats: state a newer bridge wrote

- **Thread bindings keyed by head ID (from PR #784, slice 4d).** From that
  release the slack-bridge keys its thread→agent bindings in its state file by
  the wish head's bead ID (`bindingKeyLocked`, `BindThreadHead` in
  `controller/internal/bridge/state.go`), not by the `channel:thread_ts`
  tuple. An older bridge image reads those keys as malformed tuples. After a
  rollback past #784, replies in those Slack threads stop reaching their
  agents until someone @-mentions the bot in the thread again, which rebinds
  it. Tell the people in active threads, or roll forward instead.

### The release record is capped at 1 MiB: pack, and apply the theme first

Helm stores the whole chart (every file, base64) plus the rendered manifest in
one Secret, `sh.helm.release.v1.gardenio.vN`, and Kubernetes refuses a Secret
over 1,048,576 bytes. Release 2026.292.1 failed exactly there
(`data: Too long`), with `--atomic` rolling back and every later release
blocked (gd-qiUiXvTX0k). Three things keep it under:

- **Upgrade from the packed chart, never the source dir**, as
  `gardenio/scripts/release-deploy.sh` does: `scripts/pack-gardenio-chart.sh`
  strips Go-template comments and comment-only YAML lines (450 KB of the
  templates, rendered into the manifest too) and self-verifies the render.
  The commands above with `gardenio/helm/gardenio` as the chart are the
  shape; by hand, pack first:

  ```bash
  tgz="$(scripts/pack-gardenio-chart.sh gardenio/helm/gardenio /tmp/pack --version <calver> --app-version <calver> | tail -1)"
  helm upgrade gardenio "$tgz" -f deploy-values/gardenio-k3s.yaml -n gardenio --set global.imageTag=<calver>
  ```

- **The Keycloak login theme is not in the chart.** It lives in
  `gardenio/keycloak-theme` and the deployer applies it as the ConfigMap
  `gardenio-keycloak-runtime-theme-files` right before the upgrade
  (`gardenio/scripts/keycloak-theme-configmap.py | kubectl apply --server-side`);
  the chart's StatefulSet mounts it and reads its checksum annotation
  (`lookup`) to restart Keycloak only on a theme edit. Rolling by hand, run
  that apply first (see `gardenio/keycloak-theme/README.md`), or the Keycloak
  pod fails in its `theme-unpack` initContainer.

- **CI guard:** `scripts/check-helm-release-size.sh` (the `checks.yml` helm
  job, and `make lint`) packs the chart, renders it with the deploy values,
  rebuilds the release record and fails over 900 KiB. Run it alone for the
  per-part breakdown; it was within 2% of the live record when calibrated.

After the first roll with this layout, verify:

```bash
# the record: well under 1,048,576 (the estimate at the change was ~650 KB)
kubectl get secret -n gardenio -l owner=helm,name=gardenio --sort-by=.metadata.creationTimestamp -o json \
  | jq -r '.items[-1] | "\(.metadata.name) \(.data.release | length)"'
helm get manifest gardenio -n gardenio | wc -c
# the theme: ConfigMap present, Keycloak unpacked it, the CSS is served
kubectl get configmap gardenio-keycloak-runtime-theme-files -n gardenio -o jsonpath='{.metadata.annotations.gardenio\.dev/theme-checksum}'; echo
kubectl logs -n gardenio statefulset/gardenio-keycloak-runtime -c theme-unpack
curl -sI https://auth.clonk.tv/auth/resources/<kc-version>/login/gardenio/css/gardenio.css | head -1   # 200
```

### The edge manifests ride the release

`gardenio/helm/gardenio-ziti/public-edge.yaml` and `viewer-authproxy.yaml` are
plain manifests, not chart templates. Since P-R2 (gd-WHBtmG7K3Z),
`release-deploy.sh` applies both from the tag right after the theme (rule 4c),
for `--namespace gardenio` only. A merged edge change therefore goes live with
the next roll, and an "apply public-edge.yaml after #N" escalation is no longer
needed. For each edge the script:

1. server-side applies the manifest's ConfigMaps (field manager `release-deploy`);
2. when the nginx ConfigMap changed, waits up to `EDGE_SYNC_TIMEOUT` (300s) for every
   Running pod to mount the new file, then runs `nginx -t` in each pod. viewer-authproxy
   first re-renders its envsubst template into `conf.d`, as its entrypoint does at start;
3. when a test fails, puts back the live ConfigMap as it was before the apply (and
   viewer-authproxy's `conf.d`) and exits 1, before the upgrade. It exits 2 if that
   rollback apply fails, because a pod restart would then load the untested config;
4. when every test passes, runs `nginx -s reload` in each pod, then applies the rest
   of the manifest (Deployment, Service, ...), so a pod never starts on an untested
   config.

`--dry-run` makes it a server-side dry run of each whole manifest. An edge upstream
that names a chart Service must use the `resolver` + variable form, as
`public-edge.yaml` does. A static name that does not resolve yet fails `nginx -t`
before rule 5 creates the Service.

**Precondition — check the registry before applying, not just the render.**
A `helm template` can look completely correct while an image for the target
calver hasn't finished building yet; applying anyway puts those workloads
into `ImagePullBackOff`. Confirm all required images exist at the tag before
running `helm upgrade`.

### Mandatory last step: verify the running digest, not the rollout status

`kubectl rollout status` / `rollout restart` reports success based on pod
readiness — it says nothing about *which image digest* the running container
actually has. On 2026-09-19 a stale `:latest` served through the ECR
pull-through cache caused exactly this: `kubectl rollout restart` reported
success, and the pod came back running the same 5-hour-stale binary, still
carrying a bug that had already been fixed and merged. Nothing in the
rollout output said so — only reading `kd version` out of the running pod
did. `global.latestMode` is now retired for this reason (immutable
per-calver tags), but the gate below is what actually *proves* a deploy
took, and is required after every `helm upgrade` above:

```bash
scripts/verify-image-tags.sh <calver> --namespace=gardenio
```

This reads every pod's live container image out of `kubectl get pods` in the
target namespace and fails (non-zero exit) if any gardenio-managed image
doesn't carry the expected calver tag. Treat a non-zero exit as the deploy
having **not** happened, regardless of what `helm upgrade` or `rollout
status` reported — do not consider a deploy, rollback, or roll-forward done
until this exits 0.

> The gate checks **release-managed pods only** (everything except
> `crew-*`). Crew pods aren't part of the helm release — their image is
> fixed at spawn time — so a long-lived crew will always straddle an
> upgrade; counting them would block every real deploy on a busy cluster.
> Stale crew pods are still printed, as an informational "expected to
> drain" list, not a failure.

> This run also fixed a latent bug in the script: it matched images only by
> a hardcoded `<registry>/gardenio/*` prefix, left
> over from the retired pipeline. Release images are pushed to and pulled
> from this account's ECR at `123456789012.dkr.ecr.us-east-1.amazonaws.com/
> gardenio/<c>` (see `deploy-values/gardenio-k3s.yaml`); since the AWS cost
> plan (2026-10-21) ghcr is not used for gardenio images. Neither that path
> nor the earlier ghcr pull-through path ever matched the old prefix — so the
> script would report `CHECKED=0` and
> hard-fail on every real invocation against this cluster, regardless of
> whether the deploy was correct. It now matches on the `*/gardenio/*` repo
> path instead of a specific registry host, so it isn't tied to whichever
> registry happens to be in front of the images today.

### Proven end to end (commit 96a2ad8, 2026-09-20)

The full deploy → rollback → roll-forward cycle above has actually been run
against this cluster, verified each time by reading `kd version` out of the
running pods (this was before the `verify-image-tags.sh` fix above, so the
script itself wasn't the gate that day — it is now):

| Step | Command | Result |
|---|---|---|
| before | *(prior state)* | chart on `main`/`:latest`, commit `bf952031` |
| deploy | `helm upgrade` (no override) | chart `2026.263.2`, commit `42412aeb`, all five components on `:2026.263.2` |
| rollback | `helm upgrade --set global.imageTag=2026.263.1` | `2026.263.1`, commit `17a38275` |
| roll forward | `helm upgrade` (override removed) | back to `2026.263.2`, commit `42412aeb` |

Final state: 0 gardenio images left on `:latest`; all 9 workloads `1/1`.
Full incident detail and the coredns/control-plane-taint issues hit along
the way: `git show 96a2ad8 -s`.
