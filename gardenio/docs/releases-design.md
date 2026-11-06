# Releases of a project: design

Plan gd-UZKZ9H25UD ("Releases of a project: a release record in kbeads, a release truck on /garden,
a Release page in beads-viewer"), asked in Slack thread gd-vz64elwfXL. Step 1 (gd-hls6BkIEiO)
wrote the research and the design. Step 10 (gd-HHMReWCHUi) made it final: §3 and §4 say what
shipped, with the code that does it, and §7 lists what was designed and is not built.

Diagrams:

| Diagram | File |
|---|---|
| Data flow: who writes a release and who reads it, as shipped | [`releases-data-flow.svg`](releases-data-flow.svg) |
| Entities: project, release, deployment rows (one per Helm revision per target), PR, plan, step | [`releases-entities.svg`](releases-entities.svg) |
| A tenant's first release (designed; not built end to end, §7) | [`releases-tenant-first-release.svg`](releases-tenant-first-release.svg) |

For the people who merged a PR, the short version is
[`docs/user-guide/13-where-did-my-change-ship.md`](../../docs/user-guide/13-where-did-my-change-ship.md).

## 1. Glossary

| Word | Meaning |
|---|---|
| **Project** | A `type=project` bead (kind `config`). It owns plans, agents and releases. gardenio's is gd-Ui9jl00KfA. |
| **Release** | One tag of one project, the commit it names, the merged PRs it carries and where it runs. A `type=release` bead, keyed by `(project, tag)`. |
| **Tag** | The git tag a release is named by. gardenio uses calver `YYYY.DDD.N` (UTC year, day of year, counter). A tenant would use semver `vX.Y.Z`. |
| **Release source** | The three project fields that say which tags are releases and where they can run: `release_tag_pattern`, `release_targets`, `release_branch`. A project without `release_tag_pattern` has no releases. |
| **Cut** | Pushing a tag that matches the release source. For gardenio, `release-cut.sh` (release captain or operator). |
| **Built** | Every artifact the tag promises exists. For gardenio: every image in `.github/images.json` is in ECR as `:<tag>`. Only `gb release built` records it, which `release-cut.sh`'s backfill runs on each cut (§3.4). |
| **Target** | A named place a project runs. One row of `release_targets`, `{name, kind}`. gardenio has one: `prod`, kind `k3s`. |
| **Deployment** | One row on a release: this release ran on this target from `since` at this Helm revision, reported this way by this principal. One row per revision per target. `until` is set when the next row on the target begins. |
| **Contents** | The merged PRs between the previous release's tag and this one on `release_branch`'s first-parent line, each joined to its merge-request bead, task, step and plan. |
| **Previous** | The release of the same project whose tag is the greatest release tag below this one. |
| **Live** | A release with an open deployment row on at least one target. |
| **Superseded** | A release that has deployment rows and runs on no target now. |
| **Skipped** | A reading, not a status: a release never deployed while a newer one is live or superseded. |
| **Window** | The release captain's cut time. The runbook says ~04:00Z and ~16:00Z. |
| **Truck** | The project's release truck on `/garden`, in the loading bay of its home strip's Barn cell. It is loaded while the newest release is cut or built and drives out when it goes live. |
| **Crate** | One plan's share of the newest release, on the truck's bed; one more crate for the unplanned PRs; at most 8. |
| **Release panel** | `/garden?release=<id>`: the changelog by plan and step, opened from the truck. |
| **Release page** | beads-viewer's `/release/<id>`: status, contents by plan and step, the deployments timeline, previous and next. `/releases` lists them. |

## 2. What a release was before this plan

The research of 2026-10-27, kept as the record of what this plan replaced. "Today" in this section
means that date; §3 and §4 say what runs now.

A gardenio release today is a git tag and a set of ECR images. There is no record that joins the
tag to its PRs or to where it runs. Each fact lives somewhere different, and some exist only as
prose.

### 2.1 The cut

| Fact | Where it lives | Notes |
|---|---|---|
| Tag name | git: an annotated tag, message `release <tag>` | `gardenio/scripts/release-cut.sh:161-179` computes `YYYY.DDD.N` from UTC; N is one more than the highest tag of the day. 141 `20*` tags exist, no `v*`. |
| Commit | the tag's target | `release-cut.sh`: `origin/main`'s head, when it is newer than the last calver tag main contains. Since 2026-11-03 the cut asks no CI; the tag's own `ci.yml`, `checks.yml` and `images.yml` runs gate the deploy (`release-deploy.sh` rule 4). |
| Who cut | the tagger name only | Captain (`gardenio-release-captain-standing-daily-*`), operator (`joyousaversion`), and once a crew (2026.298.1, incident gd-CVotEQ3vk9). Every pod pushes as GitHub user `joyousaversion`. |
| When | the tagger date | No other timestamp. |
| Who may cut | `release-role-guard.sh` (sourced by both scripts) | `BOAT_ROLE` must contain `release-captain`, or `GARDENIO_RELEASE_OPERATOR=1`, or empty; else exit 4. `--dry-run` always passes. It cannot stop a raw `git push origin <tag>`. |
| Laptop path | `release.sh` (`make release`) | Bumps `Chart.yaml` `version` and `appVersion`, commits, tags, prints `git push --atomic origin main <tag>`. Last ran 2026-09-23 (`Chart.yaml` still says 2026.266.3). |
| Writes to beads | none | Neither script calls kd, gb or Slack. |

The **release captain** is a daemon bead only: gd-FgKpNn3A02 (`type=trigger`, `action=spawn_agent`,
`role=release-captain`, `cron: "17 5 * * *"`, `America/Los_Angeles`, `max_concurrent: 1`). Each run
reads the last 26 h of `release-deploy` beads, comments on new deployer bugs, files a P1 "tagged but
never rolled out" when the newest tag is over 24 h old with green images and no "deployed and
verified" event, then runs `release-cut.sh --dry-run` and the real cut (exit 1 files a P2 labelled
`release-captain`). Its `terminal_goal` still mentions "escalating to the operator for the atomic
push", which is stale.

**`helm_deploy` escalations** are how a crew asks for a build to ship: `gb escalation create
--operation-type helm_deploy --requesting-task <task>`. `operation_type` is a free string; the
escalation has no `window` or `tag` field, so "the next window" exists only in `--context` prose.
kbeads marks it not approvable by a scoped principal (`kbeads/internal/server/escalation_scope.go:35`)
and its audience is `operator` (`beadsapi/escalation_audience.go:40-55`).

### 2.2 The build

`.github/workflows/images.yml` runs on `push: tags: ['20*']` and builds every image in
`.github/images.json` (29, `agent` included), pushing to
`123456789012.dkr.ecr.us-east-1.amazonaws.com/gardenio/<image>` as `:<tag>`, `:<sha>` and `:latest`
(`images.yml:961-972`). Build args `VERSION=<tag>` and `COMMIT=<sha>` bake the tag into every
binary. **No GitHub Release is made**, and nothing calls the daemon when the images are pushed: the
only record that a tag is built is the workflow run itself.

### 2.3 The rollout

`gardenio/scripts/release-deploy.sh <calver>` rolls one tag; the `release-deployer` CronJob
(`gardenio/helm/gardenio/templates/release-deployer/`, `schedule: "*/15 * * * *"`, enabled in
`deploy-values/gardenio-k3s.yaml`) runs it every 15 minutes on the newest calver tag from `git
ls-remote` (`entrypoint-configmap.yaml:46-55`), using that tag's own copy of the script.

| Fact | Where it lives | Notes |
|---|---|---|
| What runs on prod | `helm get values .global.imageTag` of the live Helm release | The only "current version" record. `release-cut.sh` tags never bump `appVersion`, so the deploy always passes `--set global.imageTag=<tag>` (`release-deploy.sh:455`). |
| Gates | the script | pause ConfigMap, helm busy, not newer (`sort -V` against the live `imageTag`), values drift, images ready (a successful `images.yml` run whose `head_sha` is the tag's commit). |
| Helm revision before/after | the description of a closed `type=event` bead | Title `release-deploy <tag>: <outcome>`, labels `project:gardenio`, `release-deploy`; body `outcome / helm revision before / helm revision after / last 40 lines`. POSTed with curl to `$KD_URL/v1/beads`, not `kd create`. No structured fields. |
| Failure | an open P1 `type=bug`, same title and labels | Deduped on an open bug with the same title. `paused` and `not newer` file nothing. |
| Who read it | nobody in code | Only the release captain's prompt reads these beads. |

Known gap: the CronJob's egress NetworkPolicy allows only Keycloak and DNS, so a real roll is
blocked until gd-VgdO1sx1Ju lands. `release-pipeline.md` still says "deploys are manual", which the
CronJob contradicts.

### 2.4 Version facts on running things

| Surface | What it says | Source |
|---|---|---|
| Controller `/version` (health mux `:8091`) | `{"version","commit","agentImage","namespace"}` | ldflags `main.version` |
| kbeads response headers | `X-Kbeads-Version`, `X-Kbeads-Commit` | `kbeads/internal/buildinfo`; clients warn on 14 days of skew |
| slack-bridge `/unreleased` | per repo: newest tag to `main`, commits ahead; per image: the bridge's own version to `main`, path-filtered; cluster: the controller's `/version` | **GitLab** tags API (`internal/bridge/gitlab.go:978`), so the repo half is stale on GitHub |
| viewer `/health` Build & release tile | the viewer's own ldflag version, its calver date and age, release bugs in 24 h | the bug count filters label `release-bug`; the deployer files `release-deploy`, so its bugs never show here |
| `gb release status` / `gb release smoke` | cut/skip verdict, golden tag | **GitLab only** (`cmd/gb/release.go:40` defaults to `example-org/gardenio`) |

### 2.5 How merged PRs reach beads

github-bridge handles `pull_request`, `check_suite`, `workflow_run`, the review events and `ping`. It
does **not** handle `push`, `create`, `release`, `deployment` or `deployment_status`; nothing in the
repo reads tags from GitHub.

- On a PR event it finds the task bead by `mr_url`, then by the branch name
  `agent/<slug>/<bead-id>`, then by up to five bead ids in the PR body (`pullrequest.go:136-170`). No
  match, no bead.
- It upserts a `type=merge-request` bead keyed on `mr_url` with `forge`, `mr_iid`, `project_path`,
  branches, `mr_state`, `mr_title`, `author`, `mr_outcome`, and closes it on merge.
- It adds `task --produces--> MR` and `MR --triggered-by--> task` (`forgebeads.LinkTaskToMR`) and sets
  `mr_url` and `forge` on the task if unset.
- It decodes the head sha but stores **no merge sha and no merged-at**.
- `gb mr link <bead> <url>` sets `mr_url` (and `forge=github`) on a bead; github-bridge makes the MR
  bead and the edges on the next event.

**Produce rows** (`gardenhub/farm_produce.go`) are read from work beads with `mr_url`, walked up
parent-child edges to their plan. They read `mr_state`/`mr_merged` off the **task**, but github-bridge
writes those only on the MR bead, so a merged GitHub PR shows as "coming" in its bin (live:
gd-tjW8sTFfJH, PR #1245, merged, has no `mr_state`). Filed as gd-9K2MsfmsNr. A release's contents must
therefore join through the MR bead, not the task's fields.

**Bead ids in merge subjects**: of the 38 first-parent commits between 2026.299.2 and 2026.299.3, 33
carry a bead id, all from the branch name in `Merge pull request #N from
mattbakerconsulting/agent/<slug>/gd-…`. Five do not: #1123 (`agent/glassask-code-font`), #1118
(`laptop/glass-63d-relay-only`), and three direct commits (b67cbc85, 79b5f690 and its revert
eeb7b745), two of which name a bead in the body. The PR number is in every merge subject, so
**the PR number joined to the MR bead's `mr_iid` is the primary key**, the subject's bead id the
fallback, and a direct commit with neither is an "unplanned" row.

### 2.6 Where a release is absent today

Evidence: the `type=evidence` child of gd-hls6BkIEiO (phone and desktop PNGs). None of these
surfaces can say which release carries a PR or what runs on prod:

- **The Yard** (`/garden?at=yard`): the barn, mailbox, dog, signposts and villagers. A pickup truck
  exists (`web/art.js:1125-1161`, `web/transitions.js`), but it is the gardener's transport from the
  barn to its plot, not a release.
- **A plan at Plan level** (`/garden?at=gardenio/<plan>`): plots, steps and the bin tooltip; nothing
  after "merged".
- **A plan's bin of MRs**: `merged`, `coming`, `growing`, `done`. A crate never leaves the bin. (The bin was
  retired since, plan gd-PJB3D7glwF; the bead view, the harvest cells and the goats' kin still read `produce[]`.)
- **beads-viewer front page**: `/` redirects to `/garden`.
- **A plan's page** (`/plans/<id>`): narrative and build steps; no "shipped in".
- **`/p/gardenio`** and **`/health`**: the Build & release tile shows the viewer's own version only.

### 2.7 Superseded docs

These describe the GitLab and RWX era. This design replaces them; each gets a banner pointing here.

| Doc | Why it is stale |
|---|---|
| `gardenio/docs/release-signing.md` | `release.sh` signs nothing and takes no lock; `release-cut.sh` makes an unsigned annotated tag; GitLab push rules are gone. |
| `gardenio/docs/release-drought-watchdog.md` | gitlab-bridge's watchdog, keyed on GitLab Release objects; gitlab-bridge is off in the k3s values. A release bead's `cut_at` is the drought signal now. |
| `gardenio/docs/release-owner-noop-probe.md` | the retired `gardener-release-owner` lane, built on `gb release status`. |
| `docs/release-health-badge.md` | GitLab project badges fed by `.rwx/docker.yml` and `.gitlab-ci.yml`; neither exists. |
| `gardenio/docs/release-trains-design.md` | the 2026-10-20 trains design pass (gd-pAwAVi5lac): a different `type:release` lifecycle (`boarding → frozen → cut → published → live`), `boards` / `shipped-in` edges and `type:release-policy`, never built. |
| `docs/archive/rwx/docs/gardenio-docs/deploy-single-source-of-truth.md` (archived) | the old EKS deploy path (`.rwx/deploy.yml`, `deploy-values/gardenio.yaml`); a deployment row is the record now. |
| `gb release status` (`cmd/gb/release.go`) | GitLab-only; retired in step 3 (gd-V9ukTsRAAJ) for `kd release list`, with its last caller `scripts/release-owner-noop-probe.sh` and the `release-status` e2e module. |

Also stale, not rewritten here: the slack-bridge `/unreleased` repo half (GitLab tags API). The
RWX-era `scripts/release-notes.sh`, `release-notify.sh`, `release-lock.sh` and
`check-release-promotion-gate.sh` were deleted in gd-R0jeUZAl3F.


## 3. The record, as shipped

### 3.1 The release bead

`type=release`, kind `data`, parented (parent-child) to its project bead and created closed: a
release is a record, not work. Title `<project> <tag>`, e.g. `gardenio 2026.306.3`. Unique on
`(project, tag)`: the daemon stamps `idempotency_key` `release:<project>:<tag>` and a unique index
backs it, so a second create of the same pair returns the first bead. The script, the bridge and the
backfill can race. The tag must match the project's `release_tag_pattern`
(`kbeads/internal/server/releases.go`).

| Field | Type | Set by | Meaning |
|---|---|---|---|
| `project` | string | cut | the project name; never changes |
| `tag` | string | cut | matches `release_tag_pattern`; never changes |
| `sha` | string | cut | the tag's commit |
| `previous` | bead id | daemon | the release `previous_tag` names, else the newest release of the project cut before this one; empty for the first, and while `previous_tag` has no release yet |
| `previous_tag` | string | cut | the greatest release tag below this one in version order |
| `cut_at` | RFC 3339 | cut | the tagger date; the daemon's clock when the writer sends none |
| `cut_by` | string | cut | the tagger name; the writer's principal when it sends none |
| `source` | enum | cut | `script` (`gb release record`), `webhook` (github-bridge), `backfill` (`gb release backfill`), `manual` (any other create, e.g. `kd release create`) |
| `built_at` | RFC 3339 | build | when every artifact existed; empty = not built |
| `images` | json list | build | `[{"name","ref"}]`, from `gb release built --images-file` (the tag's `.github/images.json`); empty on a release marked before gd-F7kqCQPhLt |
| `contents` | json list | cut | one row per merged PR, below |
| `contents_complete` | bool | cut | false when the GitHub compare was cut short (250 commits); a read from the checkout is always complete |
| `deployments` | json list | daemon | one row per Helm revision per target, below; only the deploy route and the repair write it |
| `status` | enum | daemon | `cut`, `built`, `live`, `superseded`; derived on every write, refused from a client |

The API shape (`GET /v1/releases/{id}`, `kbeads/internal/client/releases.go` `Release`) is these
fields typed, plus `id`, `title`, `created_at` and `updated_at`. `contents` and `images` pass
through as stored.

A **contents** row (`releaserecord.Row`), as 2026.306.3 carries it:

```json
{"pr": 1983, "url": "https://github.com/mattbakerconsulting/gardenio/pull/1983",
 "title": "fix(kbeads): answer a link to another land's bead as missing; …",
 "merge_sha": "9d5c1df7f16ad26ee354346da08cc4510cbdaadc", "merged_at": "2026-11-02T12:13:06Z",
 "task": "gd-Mpy0lOLACZ", "step": "gd-Gx6arHBvzJ", "plan": "gd-X07JyIHEzl"}
```

| Key | Meaning |
|---|---|
| `pr`, `url`, `title` | the PR; `pr` is absent for a direct commit |
| `train` | the merge-train PR that carried this PR onto the branch; absent for a PR merged on its own |
| `merge_sha`, `merged_at` | the merge commit and its time |
| `mr_bead` | the `type=merge-request` bead, when one exists |
| `task` | the bead that produced the PR |
| `step` | that task, or its nearest ancestor that is a plan step |
| `plan` | the nearest ancestor epic with the `plan` label |

A row with no `plan` is **unplanned**. A row with no `task` is a direct commit or a branch with no
bead. Rows are stored flat; every reader groups them by plan, unplanned last.

How a row is joined (`gardenio/controller/internal/releaserecord`): the record reads the tags and
the first-parent history from a checkout (`git log --first-parent prev..tag`) or from the GitHub
API (the compare, walked on first parents). Each commit is joined by the PR number in a merge or
squash subject to the MR bead's `mr_url`, else by its sha to the MR bead's `merge_sha`, then the
MR bead's `triggered-by` task; else by the bead id in the branch name or message; else by the PR's
head branch read from GitHub. A merge-desk train (`merge train …: #A #B`) is one row per PR it
carries, each with `train` set.

A **deployment** row, as 2026.306.3 carries it:

| `target` | `kind` | `since` | `until` | `revision` | `via` | `by` | `image_tag` |
|---|---|---|---|---|---|---|---|
| `prod` | `k3s` | 2026-11-02T15:12:28Z | 2026-11-02T22:23:34Z | 268 | `reconciler` | `observed` | 2026.306.3 |
| `prod` | `k3s` | 2026-11-02T22:23:34Z | 2026-11-02T22:49:11Z | 269 | `reconciler` | `observed` | 2026.306.3 |
| `prod` | `k3s` | 2026-11-02T22:49:11Z | (empty: runs now) | 270 | `reconciler` | `observed` | 2026.306.3 |

| Key | Meaning |
|---|---|
| `target` | a name in the project's `release_targets` (gardenio: `prod`) |
| `kind` | the target's kind: `k3s`, `box`, `land`, `github-environment` |
| `since` | when this release began to run there at this revision (RFC 3339, UTC seconds) |
| `until` | when the next row on that target began; empty while it runs there |
| `revision` | the Helm revision, or the forge's deployment id; empty when unknown |
| `via` | how it was reported: `script`, `contract`, `github`, `reconciler`, `escalation` |
| `by` | the reporting principal, or the word its writer sets: `observed` (reconciler), `rollback` (`release-deploy.sh` after a rollback), `backfill`, `box:<name>`, `github:<login>` |
| `image_tag` | the tag the target runs; every writer today sends the release's own tag |

**One row per Helm revision per target** (gd-yn68eNqnu6, PR #1873). A Helm revision is one roll on
one target, so the deploy route (`POST /v1/releases/{id}/deployments`) treats a report like this:

- A new revision of the same release (a re-roll, a values change) closes the release's own open row
  on that target and opens a new one. That is why 2026.306.3 has three rows.
- A revision the release already records there changes nothing, wherever that row sits in history.
  A late report (an event filed after the roll) cannot reopen an old row or close what runs now.
- A revision another release of the project records on that target is refused with 409: a revision
  cannot carry two tags.
- Without a revision, a repeat of the open row (same target and `image_tag`), or of a closed row
  with the same `since`, changes nothing.
- A row older than the newest on the target is history. It goes in closed, at the next row's
  `since`, and what runs there now is untouched.

The route does this under one lock and in one transaction. It closes the open row of every other
release of the project on that target (a rollback is a deployment of an older release, so the newer
one is closed the same way), appends the row and recomputes every touched status. Exactly one row is
open per (project, target). A failed roll writes no row; its P1 bug stays the record. When the route
writes a row it also closes the project's `awaits_release` work that the release carries
(`releases_awaits.go`, P-R3 gd-ZM7NEBPAqZ; see [`release-pipeline.md`](release-pipeline.md#who-cuts-a-release)).

**The repair.** `POST /v1/releases/repair-deployments` (`kd release repair-deployments --project <p>
[--dry-run]`, `releases_repair.go`) rebuilds a project's history from the rows it has. Rows of one
release with the same (target, revision) collapse into the earliest. Each target's rows are chained
in `since` order, each closed at the next one's `since` and the newest left open, and each changed
status is recomputed. A revision on two releases is listed as a conflict and left alone. A clean
history comes out unchanged, so a second run writes nothing. gardenio's prod rows written before the
rule still need it: 2026.305.2 has two rows for revision 264 and 2026.303.2 two for 259, and
2026.302.2 has no row for 257 and reads `cut`. The repair and a backfill run on prod under
escalation gd-d663jm6gQZ once a release that carries PR #1873 runs there.

Why `deployments` is a field and not child beads: one read answers "where does it run", and a
release has a handful of rows over its life.

### 3.2 Status and the state machine

```text
            tag matches pattern           every artifact exists
  (no record) ───────────────────▶ cut ──────────────────────▶ built
                                    │                            │
                                    │ deployed (no build step)   │ deployed on ≥1 target
                                    ▼                            ▼
                                  live ◀─────────────────────── live
                                    │   ▲
            replaced on every target│   │ deployed again (rollback onto it)
                                    ▼   │
                                superseded
```

| Status | Rule (`releaseStatus`, recomputed on every write) |
|---|---|
| `cut` | no `built_at` and no deployment row |
| `built` | `built_at` set, no deployment row |
| `live` | at least one deployment row with empty `until` |
| `superseded` | has deployment rows, all closed |

`status` is a pure function of the record, so a backfill or a replayed event cannot leave it wrong.
A rollback is a deployment of an older release: it reopens that release (`superseded` → `live`)
and supersedes the newer one on that target. A release that is never deployed (a newer tag went out
first) stays `cut` or `built`. The viewer's `/releases` list reads it as **skipped** ("Never
deployed: a newer release went out first") when a newer release of the project has run anywhere.
The daemon's release view reads it the same way: `GET /v1/releases` and `/v1/releases/{id}` fill
`skipped_by` with the oldest newer release that ran, `kd release show` prints it and `kd release
list` marks the row `built (skipped)`. It is not written to the record. A release gardener retires
on it as on `live` or `superseded` (gd-UbcDWJtcOA).

Nothing writes `built_at` at cut time: the images are not built yet. Each cut's backfill marks the
earlier tags built once their `images.yml` run is green (§3.4), so a tag deployed before the next
cut goes `cut` → `live` and gets its `built_at` and `images` afterwards. A tag the next cut finds
undeployed reads `built`.

### 3.3 Who writes

| Write | Who | The daemon's check (`releaseWriterDenial`) |
|---|---|---|
| record a release; fill its contents, `sha`, `built_at`, `images` | the release captain (an agent whose role names `release-captain`, on its own per-agent token), the operator and `gardenio-admin` (OIDC admin), github-bridge's machine principal (realm role `gardenio-machine-release-recorder`) | `create` / `update` on `type=release` |
| record a deployment; run the repair | the above, and the `gardenio-machine-release-deploy` realm role: the release-deployer CronJob, the controller's reconciler, a box's reporter | `deploy` on `type=release`. It is its own action, so the deploy role cannot PATCH a release's `sha`, `images` or `contents` |
| record a land's deployment | the land's owner, through the Lands contract, for its own project only | the owner tier's `deploy` (`releaseDeployDenial`) |
| read | anyone who can read the project | the GET routes are filtered like any bead read |

The shared static token never qualifies: every pod holds it and its `X-Actor` is a claim. A plain
crew can read releases and cannot write them, the same line `release-role-guard.sh` draws on
`BOAT_ROLE`.

### 3.4 How each fact gets in

| Fact | Before this plan | Written by, as built | Step |
|---|---|---|---|
| tag, sha, cut_at, cut_by, previous_tag, contents | a git tag | `gb release record <tag>`, which `release-cut.sh` runs after the push (`source=script`); `gb release backfill`, which `release-cut.sh` runs next for the newest 8 tags (`source=backfill`); github-bridge's tag recorder (`source=webhook`), built and not running on prod (§7) | 3, 3b, 4e |
| previous | none | the daemon, from `previous_tag` | 2 |
| merge_sha, merged_at on the MR bead | none | github-bridge at merge (`stampMerge`), not running on prod (§7). The contents rows carry both from git either way | 3 |
| built_at, images | the `images.yml` run | `gb release built <tag>`, which `release-cut.sh` runs as `gb release backfill --last 8 --built --images-from-tag` on each cut, for every tag whose `images.yml` push run is green, at that run's finish (gd-F7kqCQPhLt) | 3 |
| deployments | `helm get values .global.imageTag` and event-bead prose | `release-deploy.sh` rule 9; a `helm_deploy` escalation's close; the controller's release reconciler; a box's reporter; `gl release deployed`; github-bridge's `deployment_status` handler (not running, §7); `gb release backfill-deployments` for history | 4, 4b, 4c, 4d, 4f |
| status | none | the daemon, derived | 2 |
| "Shipped in" on a plan | none | `gb release record` comments `Shipped in <project> <tag>` on each plan whose steps the release carries (the backfill does not); the viewer's plan and bead pages read the releases | 3, 8 |
| history | 141 tags | `gb release backfill --since <tag>` or `--last N` (default 30) | 3 |

Of gardenio's 12 newest releases, every record came from the script, the backfill or by hand
(`source` `script`, `backfill`, `manual`); none came from the bridge. The `release-deploy <tag>:
<outcome>` event bead is still written on every run: it is the log of the run and the input of
`gb release backfill-deployments`.

### 3.5 Read paths

| Reader | What it reads | Code; notes |
|---|---|---|
| Farm document, `releases` blob and `FarmPlan.shipped` (step 5) | the daemon's `GET /v1/releases?project=&limit=12` per project with a release source, at most every 60 s | `gardenhub/farm_releases.go`; shape in [`homestead-farm-read-model.md`](homestead-farm-read-model.md#releases) |
| The release truck on `/garden` (steps 7, 9a, 9b) | the blob | `web/truck.js`; build notes in [`homestead-world-design.md`](homestead-world-design.md) 26.16 and 28.10 |
| A plot's shipped stamp and its sign's "shipped in <tag> · <targets>" line | `FarmPlan.shipped` and the blob's `recent` and `deployments` | `web/truck.js` `shippedOf`, `art.js` `drawShippedMark` |
| The release panel, `/garden?release=<id>` (9b) | the viewer's `GET /api/releases/{id}` | `web/release.js`: facts, previous and next, the changelog by plan and step |
| The release face in the `/garden` bead view (gd-j3bQqfZSt7) | the release bead's fields | `bead_face_release.go`, `web/relface.js`: status, where it runs and ran, contents by plan, a link to the panel |
| The Release pages in beads-viewer (step 8) | the viewer's JSON routes | `release_page.go`, `templates/static/releases.js`: `/releases` (each project, newest first) and `/release/{id}` (header, contents by plan and step, the deployments timeline, previous and next) |
| beads-viewer's other release lines | the daemon, per request | "Shipped in <tag>" on a merged-MR bead, a step or a plan page; a "Latest release" tile per project on the index page; the `/health` Build & release tile links the running version to its release |
| Viewer JSON (step 6) | the daemon | `releases_api.go`: `GET /api/releases?project=<p>[&limit=&cursor=]` → `{items, next_cursor}`, `GET /api/releases/{id}[?project=]`. A contents row with no PR number is dropped (the truck's count still has it); each row gains `step_title`, `plan_title` and `plan_status`; the single read gains `next` and `next_tag` |
| Daemon API (step 2) | the release beads | `GET /v1/releases?project=&status=&limit=` (newest cut first), `GET /v1/releases/{id}` (an id, or a tag with `?project=`); `kd release list` and `kd release show` |
| Lands contract and `gl` (step 6) | the daemon's routes, through the land door | `GET /lands/v1/lands/{land}/releases`, `GET …/releases/{release}`, `POST …/releases/{release}/deployments`; `gl release list`, `show`, `deployed` |

Every read runs on the request's context, so the daemon narrows it to what the signed-in user may
read. A link on `/garden` never leaves `/garden` (`TestGardenLinksStayInside`): the truck, the face
and the panel link each other, and the viewer's Release page is a separate page at `/release/<id>`.

## 4. Onboarding a project's releases

A project's releases are switched on by three fields on its project bead, and nothing else. The
daemon validates them (`project_role_validation.go`); the daemon, github-bridge, the farm and the
viewer read them (`beadsapi.ProjectInfo`).

| Field | Type | Default | gardenio | A land (designed) |
|---|---|---|---|---|
| `release_tag_pattern` | Go regexp | unset = no releases | `^20\d{2}\.\d{1,3}\.\d+$` | `^v\d+\.\d+\.\d+$` |
| `release_targets` | json `[{name, kind}]` | `[]` | `[{"name":"prod","kind":"k3s"}]` | `[{"name":"prod","kind":"land"}]` |
| `release_branch` | string | `default_branch` | unset (`main`) | the repo's default branch |

A project with no `release_tag_pattern` sees nothing: no truck, no Release page section, no
bridge work. Not built: `gb lands provision` does not set a land's two defaults yet
(gd-wo6vzs6tjm, §7).

### 4.1 Existing project fields the release source reuses

| Field | Reused? | How |
|---|---|---|
| `git_url` | **yes** | resolves a webhook's `repository.full_name` to the project; the repo whose tags are releases. gardenio has it set (`https://github.com/mattbakerconsulting/gardenio`). |
| `repos` | **yes, when set** | a multi-repo project: only the entry with `role: primary` (or the one equal to `git_url`) is the release repo. gardenio has none set. |
| `default_branch` | **yes** | the default for `release_branch`. |
| `github_app` / `github_installation_id` | **yes** | which App's token reads the compare and the tags: `fleet` for gardenio (unset today = fleet), `lands` plus the installation for a land. Not set on gardenio. |
| `resource_mapping` | **no** | its `{env, cluster, service, db_instance}` rows describe where an agent may reach, not where the project runs. Release targets are their own field. A target name may equal an env name; nothing joins them. |
| `namespace`, `land_kind`, `land_owner_sub` | read | `land_owner_sub` is who may report a land's deployment through the contract. |

**Must add**: `release_tag_pattern`, `release_targets`, `release_branch`, writable by
`gardenio-admin` (the project bead's `write_policy`). They are not in the set
`BEADS_LANDS_OPERATOR_GITHUB_ORGS` freezes, so an admin can change a land's pattern later.


### 4.2 The cut, for any project

A tag on the project's release repo whose name matches `release_tag_pattern` makes a release. Two
ways in, both running `releaserecord.Recorder` and writing the same idempotent `(project, tag)`
record:

1. **Script.** `gb release record <tag>` (`cmd/gb/release_record.go`). `release-cut.sh` runs it
   after the push, then `gb release backfill --last 8` so a cut whose own record failed is filled by
   the next cut (gd-SLPVJe1TRh). A forge with no webhook, or an operator, runs it by hand.
2. **Bridge.** github-bridge's release recorder (`cmd/github-bridge/release.go`, step 3b) handles
   `create` (`ref_type=tag`) and `push` of `refs/tags/*`, and every 5 minutes
   (`RELEASE_POLL_INTERVAL`) lists the tags of each project with a `release_tag_pattern` and a
   github.com `git_url`. It records the tags above the newest recorded release (only the newest tag
   for a project with none; the backfill writes the history). A tag that does not match the pattern
   is ignored. It writes as the bridge's machine principal (`BEADS_OIDC_*`, realm role
   `gardenio-machine-release-recorder`); without it the recorder logs that it is disabled.
   `RELEASE_RECORDER=off` turns it off. **Not running on prod:** github-bridge is not enabled on
   k3s (`githubBridge.enabled` is false by default and `deploy-values/gardenio-k3s.yaml` sets only
   its machine principal), gd-AUdgkFSB1f. For a tenant it would also need tokens from the Lands App,
   gd-5ooLXIbk0e (§7).

**Prerequisite found in the research:** neither App subscribed to `create`, `push` or
`deployment_status`; the Lands App has no webhook at all (`gardenio/docs/lands/github-app.md`). The
owner chose the tag poll for v1 (§6, Q1), so a tenant's cut needs no App change.

A GitLab project (`forge=gitlab`) is out of scope: it writes releases with `kd release create` from
its own CI.

### 4.3 The deployment, for any project

A **target** is a named place a project runs, one row of its `release_targets`. The daemon
refuses a deployment to a target the project does not declare, except a GitHub environment, which it
declares itself as kind `github-environment` on its first `via=github` row.

| Target | Kind | Example | How its deployments are written |
|---|---|---|---|
| gardenio prod | `k3s` | gardenio's `prod` | `release-deploy.sh`, a `helm_deploy` close, the controller's reconciler |
| a gardenio-in-a-box install | `box` | `box-<name>` on the home daemon's `gardenio` | the box's controller reports to the home daemon (below). gardenio declares no box target yet, so no box row exists |
| a tenant land's app | `land` | the land's `prod` | `gl release deployed` from the tenant's CD |
| a GitHub environment | `github-environment` | `staging` | a `deployment_status` webhook (not running, §7) |

**How your project's deploys are recorded:**

| Path | When to use it | What it writes |
|---|---|---|
| GitHub Deployments (automatic) | your CD creates GitHub Deployments and environments | github-bridge (`cmd/github-bridge/deployment.go`, step 4b) turns a `deployment_status` of `success` into a row on the release its ref (a tag) or sha names: target = the environment, `by=github:<login>`, `via=github`, `revision` = the deployment id. `failure`, `error` and `inactive` write nothing. Fleet App only. Built, not running: the bridge is not on prod (gd-AUdgkFSB1f) and the fleet App does not yet read Deployments or subscribe to `deployment_status` (gd-ShFmcrAXy5) |
| `gl release deployed <tag> --target <name> [--revision <r>] [--by <who>]` | a tenant: one command at the end of a deploy | a row through the Lands contract (`DeploymentReport {target, revision, by}`), for the land's owner and its CD credential, `via=contract` |
| `kd release deploy <tag> --project <p> --target <name> [--revision <r>] [--via <v>]` | a script with daemon access and a release writer principal | a row, `via=script` by default |
| observed (gardenio prod and boxes) | always on | the controller's reconciler, below |

**gardenio's own writers** (step 4):

1. `release-deploy.sh` (the `release-deployer` CronJob), rule 9, after `helm upgrade --atomic` and
   `verify-image-tags.sh` pass: `<tag>` on `prod` at the new Helm revision, `via=script`, as the
   deployer's machine principal. When verify fails and `helm rollback` lands, or `--atomic` rolls
   back, it records the tag that ran before at the rollback's revision with `by=rollback`. A failed
   roll writes no row for the failed tag; its P1 bug stays the record. The CronJob was paused by
   the `release-deployer-pause` ConfigMap from 10-21 until P-R4 gd-iK8RnuLG3W, so prod's earlier
   rows come from the reconciler, the backfill and hand reports.
2. A `helm_deploy` escalation closed `success` with the rolled tag (`gb escalation close <id>
   --status success --deployed-tag <tag> --revision <rev> [--target <t>]`, a result of
   `{"tag","revision","target"}`): the daemon writes the row, `via=escalation`, by the executor, if
   the closer may write deployments. The close's response says whether it did.
3. The controller's **release reconciler** (`cmd/controller/release_reconciler.go`, leader only,
   once a minute): it reads the deployed revision of the `gardenio` Helm release from the Helm
   release Secrets' labels (metadata only; the release body is decoded once per revision for
   `global.imageTag` and the deploy time) and compares it with the newest `prod` row. When they
   differ and the new revision has settled for 10 minutes from when this controller first saw it,
   it writes the row with `by=observed`, `via=reconciler`, `since` = Helm's deploy time. A manual
   `helm upgrade` or `helm rollback` is therefore never missed, and a scripted roll writes its own
   row first. Without a readable Helm release it falls back to the controller's own `/version`
   with no revision. Its env: `GARDENIO_RELEASE_RECONCILE_INTERVAL` (0 turns it off),
   `GARDENIO_RELEASE_PROJECT`, `GARDENIO_RELEASE_TARGET`, `GARDENIO_RELEASE_HELM_RELEASE`,
   `GARDENIO_RELEASE_HELM_NAMESPACE`, `GARDENIO_RELEASE_SETTLE`.
4. **History**: `gb release backfill-deployments --project gardenio` replays every closed
   `release-deploy <tag>: deployed and verified` event bead, oldest first, as a row at the event's
   time and the revision its description names (`helm revision after: N`, `became rev N`, or an
   operator's `helm rev N`), `by=backfill`, `via=script`. A roll reported twice is written once. A
   revision another release holds (409) and a tag with no release are skipped. A row older than
   the target's newest goes into the history closed.
5. **A box, on the home daemon** (steps 4c and 4d): a gardenio-in-a-box controller with
   `GARDENIO_BOX_NAME=<name>` and `GARDENIO_RELEASE_HOME_ADDR` set runs a second reconciler with
   the same passes against the home daemon (project `GARDENIO_RELEASE_HOME_PROJECT`, default
   `gardenio`), on target `box-<name>`, `kind=box`, `by=box:<name>`, `via=reconciler`. It
   authenticates as a confidential client of the home Keycloak realm
   (`GARDENIO_RELEASE_HOME_TOKEN_URL`, `GARDENIO_RELEASE_HOME_CLIENT_ID`,
   `GARDENIO_RELEASE_HOME_CLIENT_SECRET_FILE`, optional `GARDENIO_RELEASE_HOME_CA_FILE`) and
   refreshes short-lived machine JWTs by client credentials; a box with any of the three unset is
   not reported (it logs why). The daemon accepts no target by prefix: each box needs a declared
   `{"name": "box-<name>", "kind": "box"}` in gardenio's `release_targets`, and the client's
   service account must hold `gardenio-machine-release-deploy` on the home realm, with the client
   in the home daemon's `BEADS_MACHINE_CLIENTS`. The chart renders the env from `releases.box`
   only when `releases.box.name` is set: `homeAddr` and `homeTokenURL` are required,
   `homeClientID` defaults to `gardenio-box-<name>`, and the client secret is mounted from the
   Secret `releases.box.homeClientSecret` (default `gardenio-release-home-client`, key
   `clientSecret`), with an optional CA Secret `releases.box.homeCA`. `values-inbox.yaml` leaves
   `name` empty.

The CronJob's and the controller's machine principals hold `gardenio-machine-release-deploy`:
`deploy` on `type=release`, which records where a release runs and cannot record a release or PATCH
its `sha`, `images` or `contents`.

### 4.4 A tenant's first release (designed; not built end to end)

See [`releases-tenant-first-release.svg`](releases-tenant-first-release.svg). Steps 5 to 7 work
today for any project that has a release; steps 1 and 3 do not (§7).

1. The land is provisioned with `release_tag_pattern` semver and one `prod` target. **Not built**:
   gd-wo6vzs6tjm.
2. The tenant runs `git tag v1.0.0 && git push --tags`.
3. github-bridge's tag poll sees the tag, resolves the land's project, matches the pattern and
   finds no previous release: the contents are every merged PR on `release_branch` up to the tag.
   **Not built**: the bridge mints tokens from the fleet App only, which no tenant repo installs
   (gd-5ooLXIbk0e); the Phase 0 gate is gd-cHjYGLl0eg.
4. The release bead is written, `status=cut` (a land has no build step).
5. The farm document carries it; the land's truck is loaded with one crate per plan.
6. The tenant runs `gl release deployed v1.0.0 --target prod`: a deployment row, `status=live`.
7. The truck drives out of the farm; the Release page shows v1.0.0 live on prod since that minute.

## 5. Decisions

Within the owner's defaults (the plan's epic, gd-UZKZ9H25UD):

1. A release is a `type=release` bead, kind `data`, child of its project, unique on `(project, tag)`.
2. Contents join on **PR number → MR bead** first, `merge_sha` second, the subject's bead id third;
   a PR with none of them is listed as unplanned, never dropped.
3. Deployments are a field with an `until`, written only through the daemon's deploy route (and its
   repair), which closes the previous holder's row in the same transaction.
4. A Helm revision is one roll on one target: one row per revision per target, a repeat changes
   nothing, and a revision on two releases is refused (gd-yn68eNqnu6).
5. Status words: `cut`, `built`, `live`, `superseded`, derived and never written by a client.
   "Skipped" is a reading on the surfaces, not a status.
6. A failed roll writes no deployment; the existing P1 bug is the record of it.
7. The release source is three project fields; `git_url`, `repos`, `default_branch` and
   `github_app` are reused; `resource_mapping` is not.
8. A plot's "shipped" is one tag per plan (`FarmPlan.shipped`: the release that carries the plan's
   newest merged PR), not a tag on every produce row.
9. The superseded docs in §2.7 carry a banner pointing here.

## 6. Questions answered by the owner

Asked as one decision, gd-zmjoX4dYgN; the owner chose the defaults on 2026-10-27.

- **Q1. How a tenant's tag reaches github-bridge: a 5-minute tag poll.** github-bridge lists the
  tags of each release-enabled project every 5 minutes. No App change and no re-approval by tenants.
  Giving the Lands App a webhook and the `create` event stays possible later, with the poll as the
  fallback. Reading GitHub Deployments for tenants needs `deployments: read`, which every installer
  must approve, so in v1 a tenant reports a deploy with `gl release deployed`.
- **Q2. The truck's load, when a newer tag is cut before the last one went live: the newest `cut`
  or `built` release.** The older one is shown as skipped.
- **Q3. "Skipped" and "rolled back" are readings, not statuses.** The surfaces compute them; the
  stored statuses stay `cut`, `built`, `live` and `superseded`.

The owner's step 9 verdict (gd-w5BTPj4idw) made the truck bigger (9a, gd-r4xwtDmrn2) and made a
click open the changelog (9b, gd-rSz81Me1Pe).

## 7. Designed, not built

| What | Designed in | State | Bead |
|---|---|---|---|
| github-bridge's tag recorder, MR `merge_sha`/`merged_at` stamp and `deployment_status` handler on prod | §4.2, §4.3 | Built (3b, 4b), not running: github-bridge is not enabled on k3s | gd-AUdgkFSB1f; fleet App Deployments read and `deployment_status`: gd-ShFmcrAXy5 |
| A tenant's releases: the land's release source at provision, its tags read with the Lands App token | §4, §4.4 | Not built. `gl release deployed` and the contract routes are built | gd-wo6vzs6tjm, gd-5ooLXIbk0e, gate gd-cHjYGLl0eg; `gl release cut`: gd-x8xCMYPdM9 |
| GitHub Deployments for a tenant | §4.3, Q1 | Not built: the Lands App has no `deployments: read` | none |
| The release-deployer writing prod's rows | §4.3 | Built; the CronJob is paused | P-R4 gd-iK8RnuLG3W (waits on gd-lsNIHCKFGh) |
| gardenio's prod rows repaired to one per revision; 2026.302.2 rev 257 backfilled | §3.1 | The code shipped (#1873); the data waits for a release that carries it on prod | gd-d663jm6gQZ |
| Box rows on the home daemon | §4.3 | Built (4c, 4d); no box target is declared in gardenio's `release_targets` | none |
| Dropping the `release-deploy` success event bead | step 1's §3.4 | Not built; the event stays the run's log and the backfill's input | none |

Built differently from step 1's draft (no bead; the shipped form is above): the farm document has
`FarmPlan.shipped` instead of a `release` on every produce row; the Release page is `/release/<id>`
and `/releases`, not `/p/<project>/releases/<tag>`; the truck stands in its project's home strip
(28.10), not beside the Yard road; it carries a crate per plan, not per PR, its door shows the PR
count and its side plate the tag's tail, not the last live tag; and a click opens the release panel,
not the bead view.
