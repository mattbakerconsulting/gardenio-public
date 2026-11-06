# Release trains: what will ship, and how the release is going

> **Superseded (2026-10-28)** by [`releases-design.md`](releases-design.md) (gd-hls6BkIEiO), implemented by #1310. That design is the live `type=release` (keyed by `(project, tag)`, derived statuses `cut / built / live / superseded`); the `boarding → frozen → cut → published → live` lifecycle, `boards` / `shipped-in` edges and `type:release-policy` proposed here were never built and are not planned. Kept as the record of the 2026-10-20 trains design pass.

> Bead: **gd-pAwAVi5lac** · Thread: gd-ywSTciHLkb · Status: **superseded, never implemented**
> Produced 2026-10-20 by a three-phase multi-agent design pass (survey → mechanism judge panel + adversarial review → UX critique). Nothing in this document was built; the slices in Part 2 §7 and Part 3 §8 were not taken up.

## TL;DR

The ask: gardenio Lands and beads generally should be able to say **what is going to be in a release of a project** and **when/how the releasing is going**, and the answer must be **generic across projects that do not share gardenio's release infra**.

What exists today (Part 1): a gardenio release is only a calver git tag. Beads know bead→PR (`produces` edges, `type:merge-request` beads) but never persist PR→merge sha or sha→tag, so "what shipped in X" is not answerable in bead terms. Every release status tool (`gb release status`, `/unreleased`, the drought watchdog) is still wired to the retired GitLab lane. The Lands contract has no release noun.

The mechanism (Part 2), chosen by a judge panel over four independent designs and revised against 43 confirmed refutations:

- **A release is a bead**: builtin `type:release` ("train") per project with a daemon-validated lifecycle `boarding → frozen → cut → published → live` (or `failed` / `abandoned`), a `channel` axis, and a provider-native `ref` (calver, semver, npm version, or a human string).
- **Planned vs actual are edges**: `boards` (work bead → train, planned) and `shipped-in` (work or MR bead → train, actual). The daemon reconciles them into shipped / missed / unplanned / **unmatched** (never a silent empty set) and rolls missed work to the derived next train.
- **Per-project policy bead** (`type:release-policy`): provider, trains-vs-continuous mode, terminal state (`live` or `published`), channels, who may attest "live", SLOs and quiet hours. Tracking is opt-in; with no policy there is no sweep and no alarm.
- **Health is pure and shared**: `ok | in_flight | stale | failed | unknown`, computed from evidence timestamps, a channel heartbeat and SLOs, so absence-of-success (the 16-silent-failures post-mortem) is caught by a deadman in the daemon sweep, not by a bridge that may be off.
- **Generality**: the core knows nothing about git, CI, images or helm. gardenio's `release-cut.sh` / `images.yml` / `release-deployer` are one adapter writing the same verbs; a tenant Land with Lands-App permissions only, or a project with no infra at all, runs the whole lifecycle with `kd release` by hand.

The UX (Part 3) is one vocabulary across the browser (Releases index, train page, bead-page "did this ship?"), Slack (a `/release` family and one in-place train card per release), the CLI (`kd release` verbs, `gb release` wrapper) and the Lands API (beta.2 `Release` object and verbs).

Open questions for the operator are collected at the end of Part 2 and Part 3.


---

## Part 1. What exists today (survey)

### 1. The release lifecycle as it exists today

A gardenio release is a calver git tag `YYYY.DDD.N` (UTC year, UTC day-of-year without leading zeros, per-day counter), computed identically from `date -u` by `release.sh` and `release-cut.sh` (gardenio/docs/release-pipeline.md:51-56; gardenio/scripts/release-cut.sh:155-173). There is no release object anywhere else: zero GitHub Releases exist (`gh api repos/mattbakerconsulting/gardenio/releases` returns 0), no changelog is committed, no cosign step exists (gardenio/docs/release-signing.md:1-31). Each state below is re-derived at call time; none is stored.

| State | Evidence / record | Where observable |
|---|---|---|
| **Unreleased on main** | No record. Set = first-parent commits on `origin/main` above the newest calver tag that is an ancestor of main (release-cut.sh:79-107); 11 commits above 2026.293.10 today. | `release-cut.sh --dry-run`; `git rev-list --first-parent <tag>..origin/main`; `/unreleased` + `/api/unreleased` (bridge/unreleased.go:13-75) but only via the GitLab client, unconfigured on k3s. |
| **Green** | Since 2026-11-03 (owner: test the release tag, not main): `ci.yml` and `checks.yml` run on the calver tag push and no longer on main pushes, and the tag is green when its own `images.yml`, `ci.yml` and `checks.yml` push runs succeeded at its commit (`release-deploy.sh` rule 4; a `ci.yml` run must really have built). `release-cut.sh` no longer reads CI. | `gh api .../workflows/<wf>/runs?branch=<tag>&event=push` only. |
| **Tagged** | Annotated tag `refs/tags/<calver>`, message `release <calver>`, pushed alone; main untouched, Chart.yaml unbumped; exit 3 = nothing to release (release-cut.sh:4-17,184-216). Tagger = whoever ran it (operator account, `gardenio-release-captain-standing-daily-c-*`, `gardenio-checkin-deploy-*`); 5-12 tags/day since 2026-10-11. Writes no bead. | `git cat-file -p <tag>` (tagger, timestamp). |
| **Images published** | `images.yml` triggers on `tags: ['20*']`, builds the full matrix, pushes `:latest`, `:<sha>`, `:<calver>` to ghcr with `VERSION=<ref_name> COMMIT=<sha>` baked via ldflags (images.yml:3-23,71-80,910-938). ~20 min tag-to-published for 2026.293.10. No per-image manifest/digest list. | `gh api .../workflows/images.yml/runs?branch=<calver>&event=push`; registry tags. |
| **Deployed** | No deploy workflow (release-pipeline.md:58-67). `release-deployer` CronJob (k3s, `*/15`, Forbid) resolves newest tag via `git ls-remote` + `sort -V`, shallow-clones it, runs that tag's `release-deploy.sh` (templates/release-deployer/cronjob.yaml:11-31; entrypoint-configmap.yaml:40-77). Gates in order: pause ConfigMap, helm busy, not-newer vs `helm get values .global.imageTag`, values drift, images.yml success at the tag sha (release-deploy.sh:283-406). Deploy = pack chart `--version/--app-version <calver>`, `helm upgrade --atomic --set global.imageTag=<calver>`, then `verify-image-tags.sh` with `helm rollback` on failure (release-deploy.sh:440-502). | `helm history` / release Secret `sh.helm.release.v1.gardenio.vN` (chart version = calver); `helm get values .global.imageTag`; pod image tags; controller `GET /version` `{version,commit,agentImage,namespace}` (cmd/controller/main.go:830-848). |
| **Recorded in beads** | Only `release-deploy.sh`, only when `KD_URL` is set: closed `type=event` bead titled `release-deploy <calver>: <outcome>`, labels `project:gardenio`,`release-deploy`, description with helm revision before/after and 40 log lines; open P1 `type=bug` on exit 1/2 or values drift, deduped by exact title. Outcomes `paused` and `not newer` file nothing (release-deploy.sh:177-265). Hand-run `helm upgrade` leaves no bead. | `GET /v1/beads?labels=release-deploy`. |

Legacy flow: `release.sh` (`make release`) bumps `Chart.yaml`, commits `chore: release X`, prints `git push --atomic origin main <TAG>`; last used for 2026.266.3. `Chart.yaml` is still `2026.266.3` while the newest tag is 2026.293.10 (gardenio/helm/gardenio/Chart.yaml:4-5), so a plain `helm upgrade` without `--set global.imageTag` is a rollback.

Status tooling that is GitLab/RWX-bound and inert on GitHub: `gb release status` (states `green-golden|red-rc|pending`, "golden" = a GitLab Release exists, default project `example-org/gardenio`; cmd/gb/release.go:38-40,250-260), `gb release smoke` drift probe (release_smoke.go:111-113), the release-drought watchdog in gitlab-bridge (disabled by default, gitlabBridge absent from deploy-values; release_drought.go:23-30; values.yaml:15316-15317), `release-notes.sh` (unwired, still references `.rwx`; ci.yml:227), `release-notify.sh`, `release-lock.sh`, `check-release-promotion-gate.sh` (still in `make lint`), `release-owner-noop-probe.sh` (git leg works, condition-4 leg needs `gb release status`).

### 2. The identity chain today

```
bead ──(1)──> PR/MR bead ──(2)──> merge sha ──(3)──> calver tag ──(4)──> images ──(5)──> deployed version
```

**(1) bead -> PR: exists, in beads.** github-bridge locates the task for a PR by (a) a work bead already carrying `mr_url` (`gb mr link`), (b) branch `^agent/[^/]+/([a-z]{2}-[A-Za-z0-9]{6,12})$`, (c) up to 5 bead ids in the PR body; no match = PR ignored (cmd/github-bridge/pullrequest.go:79-161). It upserts a `type:merge-request` bead keyed by unique `mr_url` (migration 042) with `forge, mr_iid, project_path, source_branch, target_branch, mr_state, mr_outcome, author` and links `produces` (task->MR) / `triggered-by` (MR->task) via `forgebeads.LinkTaskToMR` (pullrequest.go:195-240; forgebeads.go:18-50). CI reaches the MR bead as `mr_pipeline_status`/`mr_pipeline_url` only for runs listing `pull_requests[]` (pipeline.go:154-212). The daemon auto-closes leaf task/bug/feature/chore beads when every produced MR is merged (kbeads/internal/server/auto_close.go:14-49). Wrap-up JSON `pull_requests` are auto-linked at `gb stop` (stop_mrlink.go:3-20).

**(2) PR -> merge sha: missing in beads, present in git.** `head.sha` and `merged_by` are decoded but never written (pullrequest.go:34-37,279-285); the MR bead field set has no sha, no `merged_at`; `closed_at` is the only merge timestamp (beadsapi/client_mr.go:437-478; client.go:1428). GitLab's MR struct carries `SHA`/`MergedAt` but the sync maps neither (bridge/gitlab.go:93,108; gitlab_sync.go:233-248). The only sha near a bead is a transient dispatch-cooldown HTML comment (mr_dispatch_cooldown.go:28-45). In git the link is deterministic: merge commits read `Merge pull request #N from .../agent/<slug>/<bead-id>` and subjects end `(<bead-id>)` (`git log --first-parent 2026.293.9..2026.293.10`).

**(3) merge sha -> calver tag: git only.** Tag containment is `git rev-list --first-parent <prev>..<tag>`; `release-notes.sh` computes this range and buckets by conventional-commit type/scope but never looks at beads (scripts/release-notes.sh:62-130). No bead field, label or dependency says "shipped in <calver>"; no `version`/`calver`/`released_in` field exists on any issue-kind type (the only `release_status` is the SCIF Gate-2 enum; kbeads/internal/server/config.go:1975). `release-cut.sh` has git and gh in hand but writes nothing.

**(4) tag -> images: GitHub Actions only.** The predicate is an `images.yml` push run with `head_sha == gh api commits/<calver>` sha (release-deploy.sh:386-406). github-bridge subscribes to no push/create/release/deployment events (docs/runbooks/github-bridge.md:99-101) and drops workflow_run deliveries with empty `pull_requests[]` (pipeline.go:127-131), so the tag's images.yml run never reaches a bead.

**(5) images -> deployed: helm + /version.** `global.imageTag` in `helm get values`, chart version stamped = calver, pod image tags checked by `verify-image-tags.sh` (release-deploy.sh:29-38; verify-image-tags.sh:60-120); binaries self-report via `/version` (images.yml:936-937). Chart resolution is per-component tag > `global.imageTag` > `Chart.AppVersion` (release-pipeline.md:71-75). Rollbacks are indistinguishable from forward deploys except in `helm history`. Beads see this only through the `release-deploy` event ledger (§1).

Net: links (1) and (5) are recorded; (2), (3), (4) are derivable from git/gh at query time but have no persisted record, and nothing joins them end to end.

### 3. Lands contract constraints a release resource must obey

The contract (`lands/contract/openapi.yaml`, `1.0.0-beta.1`) is a closed projection with ten nouns: land, plot, crew, ticket, decision, escalation, plan, thread, budget, event (openapi.yaml:45-55). No release noun, field, event or problem code exists; no server implements `/lands/v1/` and no `gl` CLI exists (grep: only `lands/contract` imports `landsv1`).

Rules stated in `info.description` and enforced by `spec_test.go`:

- Paths under `/lands/v1/`; `X-Lands-Contract` on every response; additive-only; new required request fields only under `/v2/`; enums closed per version (openapi.yaml:10-34).
- Every object `additionalProperties: false`, no `nullable`/type lists, absent-means-none, opaque ids <= 64 (spec_test.go:279-316).
- Field cap of 8 properties per object; "Raising this is a design change, not a test fix" (spec_test.go:19-22). Plot `{id,name,default_branch}` has room for 5 more; Ticket is already at 8.
- Forbidden names: `operated_by, token, daemon_url, project, labels, fields, relevance_score, created_by, owner, notes` (spec_test.go:24-30). Hygiene denylist additionally bans `bead, coopmux, daemon, plan_state, phase_order, step_order, idempotency_key, scoped_principal`, fleet ids `(gd|ka)-[A-Za-z0-9]{10}`, and backing routes, across spec, fixtures and both bindings (hygiene_denylist.txt:87-100).
- Lists are `{items,next_cursor}` with limit 1-200, only `list*`/`search*` return `*Page`; every POST takes `Idempotency-Key`; no PUT/DELETE, state moves through named POST verbs; errors are problem+json with closed `ProblemCode` (`unauthenticated, not_found, forbidden, invalid_request, conflict, budget_exhausted, land_paused, rate_limited, upstream_unavailable`) (spec_test.go:150-208; openapi.yaml:1040-1051).
- `EventType` is pinned to seven values (`ticket.updated, decision.opened, decision.resolved, escalation.opened, message.posted, plan.updated, budget.updated`); any new value must be added to `wantEventTypes`, `eventObjects`, `fixtures/Event/` (spec_test.go:227-237; fixtures_test.go:34-69; openapi.yaml:1008-1037).
- Procedure: fixtures per touched schema, `goTypes` entry, bump `info.version` + `landsv1.ContractVersion`, regenerate bindings; CI job `lands-contract` fails on diff (README.md:33-41; checks.yml:1435). Module is outside `go.work`, must not import kbeads/controller.

Backing facts: a land is a `type:project` bead (`land_kind=tenant`, `git_url`, `default_branch`, `repos[{url,branch,role}]`, `github_app=lands`, `github_installation_id`, `max_live_agents`, plus `namespace`/`runtime_class` in PR #736) with no release/version/tag field (config.go:975-1040). Plot maps to that one repo. The subject must be named "plot" or "land", never "project". Lands App permissions are contents/pull_requests/issues write, checks/metadata read only: tenant agents cannot tag, release, run workflows or read packages, and there is no webhook (docs/lands/github-app.md:104-117,155). Plan `state` ends at `complete`; no state after "done". Phases 10-11 of Track B have no release step (track-b-design.md:709-735).

### 4. Surface catalog and idioms

| Surface | Idiom | Release info today |
|---|---|---|
| **beads-viewer /plans, /plans/{id}** | Plan = `type=epic` + `plan` label, `plan_state` owned by daemon, reactor nudges, viewer renders over SSE `/plans/{id}/events`; write-through bead writes (plan-authoring-runbook.md:13-29). Index shows open-MR pills via `produces` edges and a "last ship" facet (plans_mrs.go:3-24; plans.go:130-150). | None per calver. |
| **beads-viewer /health** | 12 htmx tiles, 30 s refresh. "Build & release" tile = running viewer's calver, date parsed from `YYYY.DDD.N`, 24 h `release-bug` count (health.go:705-729; http_health_snapshot.go:533-562). | Self-version only. |
| **beads-viewer /, /feed, /p/{project}** | Static front page with GitLab MR rail (index_mr_status.go:40-66); `/feed` event-sourced SSE recognising releases only by squawk-title regex `cut YYYY.DDD`/deployed/shipped (feed.go:1602-1618); project hub lazy sections `/b/project/{name}/<Endpoint>` (project_hub_sections.go:106-452). | Heuristic text only. |
| **/garden (gardenhub)** | In-memory farm read model, versioned SSE patches, 400 ms coalesce, 30 s reconcile (hub.go:1-50). FarmPlan/FarmTask with MR `{url,iid,state}`, task state `review`, seeds, harvested plans, piles (farm_model.go:13-120). Commons cards = `type=message kind=card` beads re-rendered in place; `kind=notice` faces (commons/notices.go:11-40). Workbench DROP list retires /health tiles, charts, Grafana proxy (workbench-glass-design.md:80-118); fleet notices incl. release drought stay Slack-only in v1 (commons-design.md:229-232). | None. |
| **Slack (bridge)** | Block Kit per Theme (Garden/Business via `card_theme`); `SemRelease` glyph = tractor / `[RELEASE]` exists (theme.go:107,183,271). Slash router bot_commands.go:42-67; `/unreleased` ungated, ephemeral, per-repo commits ahead of latest tag + per-image file counts + controller `/version` (bot_unreleased.go:24-197). Pinned Dashboard message with content hashing (dashboard.go:1-8). Default alert slug `release-notifications` (coverage_alert.go:34). | `/unreleased` only, GitLab-backed, dead on k3s. |
| **gb CLI** | `gb release status` stable JSON `{latest_tag, state, latest_pipeline_status, golden_tag_age_min, main_commits_ahead, consecutive_rc_count, blocking_decisions[], quiet_hours_active}`; pure `computeReleaseStatus` is backend-agnostic (release.go:139-192,351-419); blocking shapes `release-stuck|preflight-broken|release-pipeline-broken` (release.go:47-51). `gb release smoke` six probes (release_smoke.go:10-35). `gb mr link/status/list`. | GitLab reads; verdict unreachable on GitHub. |
| **Grafana / Prometheus** | Dashboards are cost/fleet/perf/security; only release metrics are `gardenio_release_*` gauges + PrometheusRule under gitlab-bridge (release_drought_metrics.go:22-65; release-drought-alert.yaml:1-40). | Not live on k3s. |
| **Beads primitives** | Extensible `type:<name>` configs with typed FieldDef/WritePolicy (model/type_config.go:3-91); free-form dependency types <= 50 chars (model/dependency.go:7-51); `type:event` ledger schema (config.go:1413-1428); formula/molecule with `vapor` phase "for releases/health checks" (docs/formula-reference.md:30-36); idempotency keys; event bus `beads.bead.*`, `beads.dependency.added` (events.go:10-75). | Used only by release-deploy.sh. |

### 5. Ranked gaps

1. **No release entity.** No bead type, field, label schema or dependency kind represents a calver release; no GitHub Release; no record links a calver to the beads/PRs it ships (config.go:1975; client_mr.go:437-478).
2. **No persisted merge sha / merged_at on MR beads**, so beads cannot be bucketed into a tag range without going back to git (pullrequest.go:34-37; client_mr.go:437-478).
3. **No "planned for release" concept**: nothing lets an operator or tenant declare intended contents ahead of a cut; the unreleased set is recomputed from git on demand.
4. **Lifecycle states are not stored anywhere**; green/tagged/images/deployed are re-derived via gh/helm/kubectl from inside the cluster, and the tag cut and image publish write no bead or event (release-cut.sh; pipeline.go:127-131).
5. **Every status/watchdog/notification tool is GitLab/RWX-bound** and non-functional on the live GHA lane: `gb release status`, release-drought watchdog, `/unreleased`, `release-notes.sh`, `release-notify.sh`, promotion gate (release.go:38-40; release_drought.go; helpers.go:63).
6. **github-bridge is blind to tags, pushes, releases and deployments**; workflow_run deliveries without `pull_requests[]` are dropped (runbooks/github-bridge.md:99-101; pipeline.go:127-131).
7. **Deploy history is a side effect**: event beads only for non-steady outcomes and only with `KD_URL`; manual `helm upgrade` leaves nothing; rollbacks indistinguishable; success is silent in Slack (release-deploy.sh:177-265).
8. **Lands contract has no release noun, event or field**, no server, and tenant agents cannot tag or read packages (openapi.yaml:45-55; github-app.md:104-117).
9. **`Chart.AppVersion` (2026.266.3) has diverged from the released calver (2026.293.10)**, so the chart's own version is not a reliable signal (Chart.yaml:4-5).
10. **No UI home**: /health tile shows self-version; /feed uses title regex; /garden farm has no "shipped" beyond a closed plan; Workbench DROP list orphans health tiles; no Block Kit release card beyond a glyph constant.
11. **No time-series or dashboard** of tag->images->deploy latency or lead time; only drought gauges that depend on the retired GitLab Release object.
12. **Everything hard-codes the gardenio monorepo** (`project:gardenio` labels, single repo); nothing parametrises "release of project X" for other beads projects or Lands plots.
13. **Decision-bead release blockers are label/title substring matches** with no structured tie to a specific tag (release.go:425-438).

---

## Part 2. Mechanism: release trains

### Decision record

| Rank | Design | Generality | Fit / cost | Truthfulness | Σ score | Σ rank |
|---|---|---|---|---|---|---|
| 1 | Release trains (boarding / observed shipping / daemon reconciliation) | 8.5 (1st) | 6.5 (2nd) | 7.5 (3rd) | 22.5 | 6 |
| 2 | Release Bead (`type:release`, `targets`/`ships`, validated `release_state`) | 8.0 (2nd) | 8.0 (1st) | 6.0 (4th) | 22.0 | 7 |
| 3 | Release Resolver (derived state over join keys + declaration) | 5.5 (4th) | 6.0 (3rd) | 8.5 (1st) | 20.0 | 8 |
| 4 | Release Ledger (evidence ledger + projection) | 7.0 (3rd) | 4.0 (4th) | 8.0 (2nd) | 19.0 | 9 |

**Winner: Release trains.** The kbeads daemon owns validation, reconciliation, minting, health and the alarm sweep, so a kd-only project with no controller, webhooks or CI gets the full planned-vs-actual model (the hard constraint), while gardenio's calver chain is two scripts that already hold `KD_URL`. Grafts: the Release Bead's `channel` axis and derived `release_key`; the Ledger's immutable evidence rows with evidence time distinct from posting time; the Resolver's probed "live" with a first-class `unknown`.

---

### 1. Core model (project-agnostic)

#### Entities

**Train — builtin `type:release`, kind `issue`**, declared with `issueFields(extras)` beside `type:epic` (config.go:297); inherits status, priority, labels, `due_at` (target ship date), `project:<x>`, `auditFields`. `write_policy: {"escalatable": true}` only — **not** `project_scoped` (see Authorization).

| field | type | stored/derived | meaning |
|---|---|---|---|
| `release_state`, `failed_reason` | enum `boarding, frozen, cut, published, live, failed, abandoned`; string | STORED | lifecycle, daemon-validated; reason required to enter `failed` |
| `channel` | string, default `default` | STORED | release track; validated against the policy's channel list |
| `ref` | string | STORED | provider-native identity (calver, semver, npm version, human string). Absent until `cut`; immutable after, except via the moved-ref rule |
| `commit` | string | STORED | sha the ref pointed at when cut; compared on every later verb |
| `release_key` | string | DERIVED, daemon-stamped | `<project>/<channel>/<bead id>` while uncut; `<project>/<channel>/<ref>` after cut (`…/<ref>@<sha7>` after a moved ref). Unique among **cut** trains per (project, channel) |
| `provider`; `ref_url`, `previous_ref`, `live_target` | string | STORED | free provider name (`manual`, `gardenio-calver`, `git-tag`, `npm`, …); opaque evidence pointers |
| `cut_at`, `published_at`, `live_at` | timestamp | STORED, write-once | evidence time: request `observed_at` if present (must lie in `[previous cut_at − 30d or release_epoch, now]`, else 400), else `now`; absent on a post-hoc mint with unknown time. Posting time/actor are in `mutation_history` |
| `observed_at`, `observed_ref` | timestamp, string | STORED, from `observe` | channel heartbeat as last seen by this train |
| `reconcile_summary` | json | DERIVED cache | `{boarded, shipped, missed, unplanned, unmatched:[{pr, sha, subject}], commits, basis, final, computed_at}`; edges are truth |
| `release_health` | — | DERIVED, never stored | `ok \| in_flight \| stale \| failed \| unknown` |

Labels: `release-stale`, `release-observer-silent` (sweeper, add+remove), `release-unplanned-train`, `release-observer-unparseable`, `no-auto-roll`. `release-stuck` stays the gardener **decision** shape (gb/release.go:42-51, 425-438); the sweeper never writes it.

**Evidence row — existing `type:event`** (config.go:1413-1428), created closed, `parent-child` under the train, immutable, written **only by the daemon** in the same transaction as the transition it records. Fields: `action` (`cut|published|live|failed|observed|superseded|rolled-back-to|rolled|boarded|shipped|rejected`), `outcome`, `match_key` (train id), `match_title` (`release_key`), `project`, `reason`, `timestamp` (evidence time); additive: `ref`, `evidence_url`, `idempotency_key` (absent from `dataFields` today), `source` stamped from the verified subject (`machine:<id>`, `land:<land>`, `oidc:<email>`), `source_claim` (the client's `--source`). Provenance is the server-stamped `Bead.CreatedBy` column (model/bead.go:238); the caller-written `type:event` `created_by` field (config.go:1422) is not written for release rows. Key `release-event:<train id>:<action>:<ref>` (train id, not `release_key`, which changes at cut). Heartbeats are field writes; an `observed` row is written only when `observed_ref` changes, so a 15-min CronJob yields no rows in steady state.

**Project policy — new builtin `type:release-policy`**, one bead per project (key `release-policy:<project>`), so a tenant can declare its shape without `type:project`'s `gardenio-admin` floor (config.go:977); writable by the project's land principal, a token scoped to it, or an OIDC user with it in `authorized_projects`. Tracking is **opt-in**: no policy bead, no sweep, alarm or forecast. Fields: `release_provider` (default `manual`), `release_train_mode` `trains|continuous`, `release_terminal` `live|published` (default `live`; `npm`/`git-tag` default `published`), `release_epoch` (MRs merged before it are invisible to forecast, window and drought), `release_roll_missed` (default true), `release_observer_subjects` (verified subjects whose `observe` may move state), `release_observed` (`{<channel>: {at, ref}}`, daemon-written heartbeat), `release_slo` `{boarding_grace_days, cut_to_published_min, published_to_live_min, observer_silent_min, quiet_hours: {tz, window, days}}` (absent key = no alarm/no quiet hours), and `release_channels`: objects `{name, repo?, branch?, paths?, ref_pattern?, terminal?, observer_subjects?}` (default `[{name: default}]`; `branch` defaults to the project's `default_branch`; channel keys override project keys). The gardenio policy bead carries today's quiet-hours constants (`leaf_time_window.go:100-106`).

**MR join keys — additive on `mrFields`** (config.go:40-57): `merge_commit_sha`, `head_sha`, `merged_at`, `merged_by`, `cherry_picked_from` (sha[], from cherry-pick trailers in a manifest); plus `close_reason` on issue-kind `auditFields` (`done|wontfix|duplicate|superseded`; today only agent/lands types have one, config.go:646). github-bridge decodes `head.sha` and `merged_by` but persists neither (pullrequest.go:27-37, 197-208); `merge_commit_sha`/`merged_at` are not decoded, so `pullRequestEvent` and `MergeRequestBeadFields`/`mrFieldsMap` (client_mr.go:13-41, 434-478) gain all four.

#### Dependency kinds

Free-form types (dependency.go:28-31); constants beside `DepChainsTo` (dependency.go:16-24), mirrored in `forgebeads.go`: `boards` (work bead → train, PLANNED), `shipped-in` (work or MR bead → train, ACTUAL), `follows` (train → previous cut train on the channel). None is in `blockingDepTypes` (queries.go:219); `blocks` and `parent-child` are reused. At most one `boards` edge and one `rolled` row per (leaf, train).

#### Event vocabulary

S1 rides existing topics. S6 adds `beads.release.updated` `{release_id, project, channel, from_state, to_state, ref, actor, observed_at}` beside `TopicPlanStepAdded` (events.go:56).

#### Identifier scheme

Identity is the bead id; display name is `ref` else `title`. **"Next" is derived, never stored**: `kd release next` (and the Lands list) returns the oldest open `boarding`/`frozen` train by `due_at` then `created_at`, so `1.4`, `1.5`, `2.0` may board at once. Channel order is the `follows` chain, then the provider's ref ordering (calver/semver), only then `cut_at`, so a post-hoc mint never sorts newest by accident.

#### Lifecycle and evidence

`kbeads/internal/server/release_state_validation.go` (sibling of `plan_state_validation.go`) has **three entry points**, pinned by one test like `machine_gates_test.go`: `createBead` (prior = empty; `→boarding`, or for provider/daemon principals `→cut|published|live` with full ref checks; `release_key` minted here), `updateBead` (beside `validatePlanStateTransition`, beads.go:1852; covers gRPC, HTTP PATCH, `kd update -f`, undo via undo.go:102), and `CloseBead`/`DeleteBead` (beads.go:2300-2375, http_beads.go:910-941, which never call `updateBead`): close refused unless terminal; delete refused with evidence children or `shipped-in` edges. Reconcile runs **synchronously inside the transition** (failure fails the request); a new post-persist `afterBeadUpdated` in `updateBead` (beads.go:2038-2050) only emits the bus event; the daemon's own close reuses `afterBeadClosed`.

| from → to | mover | daemon requires / does |
|---|---|---|
| (create) → `boarding` | human, agent, Lands, daemon | `provider`/`channel` defaulted; `release_key=…/<id>` |
| `boarding` ↔ `frozen` | human/agent | after freeze, `boards` refused (409) unless `--force` |
| `boarding`/`frozen` → `cut` | provider/human | `ref` matches the channel's `ref_pattern`, unique per (project, channel); stamps `cut_at`; row |
| any non-terminal → `published` / `live` | provider (human for `live`) | folds missing earlier legs (`cut_at = published_at`); stamps the leg's `*_at`; row |
| any non-terminal (with `ref`) → `failed` | provider/human | `failed_reason`; mints post-hoc if no train holds the ref |
| `failed` → `cut`/`published`/`live` | retry | same evidence as target |
| `cut`/`published` → `abandoned` | daemon only | reason `superseded`; row |
| `boarding`/`frozen`/`failed` → `abandoned` | human | reason; roll-forward |
| `abandoned`/closed `live` → `live` | daemon drift fix / human | `rolled-back-to` re-entry; reopens; row |
| X → X; earlier leg after a later leg | anyone / provider | no-op, never re-stamps `*_at`; late leg is evidence-only (stamp absent `*_at`, write row) |

The declared terminal state (`release_terminal`, per channel) closes the bead; `abandoned` always does. **Moved ref**: every verb carrying `--commit` compares it with the stored `commit`; mismatch → `failed: ref moved <old>→<new>` plus a new train `…/<ref>@<sha7>`; a ref reported gone → `failed: ref deleted`. **Supersession**: a later train (channel order) reaching terminal moves older open `cut`/`published`/`failed` trains to `abandoned/superseded` (release_drought.go:23-31 lesson).

**Authorization.** `project_scoped` is not used: it denies land principals outright (write_authz.go:378-389; checkProjectScope :861-895 needs a user bead that `land:<land>` lacks) and never runs for static/fleet tokens (`checkTokenScopes`, write_authz.go:318-322). The validator enforces a release-specific binding instead: a land principal may touch only trains, policy and evidence whose `project:` label equals its bound land (`landPrincipalFromContext`, the channel_binding_authz.go pattern); an agent-pod token is bound by its scopes; a machine principal by its grant; an OIDC user by `authorized_projects`. A static `KD_TOKEN` write (release-deploy.sh today, `machinePrincipal.enabled` off) is stored `attested=false`, counts for nothing in health and drives no drift fix — its `X-Actor` subject (interceptors.go:411-425) is self-declared. `type:release` joins `serviceFieldGatedTypes` (service_field_policy.go:22-33): no `*`-owned field (ka-0000006237).

The daemon never calls git, gh, helm or npm.

---

### 2. The two questions

#### "What will be in release X?"

*Planned* = reverse `boards` edges (epics expanded to leaves with the auto_close.go subtree CTE); `kd release show` lists them with status and MR via `produces`.

*Forecast* is computed, never stored: planned ∪ MR beads with `merged_at` after the channel's previous cut (or `release_epoch`), `project_path` = channel `repo`, `target_branch` = channel `branch`, no `shipped-in`. **Honesty bound**: MR beads exist only for PRs github-bridge could tie to a bead (pullrequest.go:186-193); human PRs and direct commits have none, so forecast, `window` and `sha-range` are a lower bound ("agent work that shipped"), and readers say so via `basis`. Opt-in bridge fix: upsert a thin `release-only` MR bead for every merged PR of a release-tracked repo.

*Actual* = reverse `shipped-in` edges with `reconcile_summary.basis`:
1. `manifest` — the provider passes `{commits, prs[], bead_ids[], cherry_picks[]}`; kd resolves PRs to MR beads by unique `mr_url` (migration 042), MRs to tasks via `triggered-by`. PRs with no MR bead go to `reconcile_summary.unmatched` (placement `unmatched`, never folded into `unplanned`). Append-only.
2. `sha-range` — set join on `merge_commit_sha` and `cherry_picked_from`. Re-evaluable.
3. `window` — `prev.cut_at < merged_at ≤ this.cut_at` filtered by channel repo/branch. Re-evaluable. Channels split by `paths` get `window` only from a manifest provider (MR beads carry no changed paths); otherwise `boarded`.
4. `boarded` — at the first of `cut`/terminal, ship each boarded leaf closed with `close_reason=done` (or whose produced MRs are all merged); in manual projects a human-attested assumption, correctable by `kd release ship/unship`.
5. `epoch` — a channel's first train (bootstrap or first post-hoc mint) has no predecessor and no window; nothing before `release_epoch` is ever "unreleased".
When no join works the daemon falls back to `boarded` and records it; it never emits a silent empty set.

**Reconciliation** (`release_reconcile.go`) is idempotent (edges by set difference, mints by key) and runs (a) synchronously in every transition at or past `cut`, (b) in `AddDependency` when a `boards`/`shipped-in` edge touches such a train, (c) on an MR-bead update that sets `merged_at`/`merge_commit_sha`, matched to the newest train whose window/range covers it (bridge downtime, github-bridge.md:99-101), (d) in the daemon sweep over trains with `reconcile_summary.computed_at < updated_at`, (e) via `kd release reconcile`. B = boarded leaves, S = shipped leaves (MR beads replaced by `triggered-by` tasks): shipped = B∩S, missed = B\S (`pending` until terminal; **final "missed" = frozen-and-not-shipped only**), unplanned = S\B. Roll-forward on terminal/`abandoned`: each missed leaf without `no-auto-roll` gets a `boards` edge to the derived next train (minted with key `release-next:<project>:<channel>:<T.id>` only when none exists) and a `rolled` row.

**Train modes.** `trains`: a human `cut` consumes the derived next; an **automated** cut consumes a boarding train only when `frozen`, otherwise it mints a post-hoc train from the forecast and the boarding train survives, so a channel cutting 5-12 times a day never churns boarders through "missed". `continuous`: one standing `boarding` holder per channel (never consumed or closed) answers the planned question and is the `board`/roll-forward target; each per-merge cut train reconciles its members. `kd release check` rejects `continuous`+`manual` unless an observer subject is declared.

**Binding a ref to a train**: `--train <id>` when the provider knows; else the derived next; several candidates with no `due_at` → mint post-hoc rather than guess. A `cut` whose ref already exists mints post-hoc instead of failing (two concurrent `release-cut.sh` runs), so cut evidence is never dropped. Reverse lookup is Y's forward `shipped-in` edge.

#### "How is release X going?"

One GET gives state, `*_at`, `observed_*`, `reconcile_summary`, evidence children and blockers (open `blocks`→train ∪ the `release-stuck|preflight-broken|release-pipeline-broken` decision shapes, release.go:47-51). `release_health` is one pure function in **`kbeads/pkg/release/health.go`** (new public package beside `gen/`; the controller imports kbeads, never the reverse), shared by kd, gb and the viewer:

- `failed` — state `failed`.
- `stale` — anchor = max(latest `*_at`, channel `release_observed.at`); `now − anchor > release_slo[<next leg>]` outside the policy's `quiet_hours`; for `boarding`, `now > due_at + boarding_grace_days` (else `created_at`-based).
- `unknown` — `observer_silent_min` is set and the channel heartbeat is older, whatever the provider name.
- `in_flight` — a later leg up to `release_terminal` is not reached.
- `ok` — terminal reached, or boarding on schedule.

**Absence-of-success** is caught by the deadman (cut, no published; published, no live), the heartbeat, and the drought net (oldest post-epoch merged MR without `shipped-in` older than `boarding_grace_days` while nothing is in flight), all in the daemon sweep, so a kd-only install has every net.

**Forward vs rollback.** `observe` is a **channel-level** write: the daemon stamps `release_observed[channel]` and `observed_at/ref` on every open train of the channel, so a 90-minute image build keeps the deadman fed. Only a declared `release_observer_subjects` entry moves state; any other `observe` is a `rejected` row. An `observed_ref` not matching the channel's `ref_pattern` (helm's `null`) is stored, drives nothing, and labels `release-observer-unparseable`. If the ref matches a non-live train, the daemon moves it to `live` (`rolled-back-to` re-entry if closed) with a row naming the observer; a later train (channel order) that was `live` goes `failed: rolled back to <ref>`.

---

### 3. The adapter boundary

**Minimum declaration: one policy bead** (`kd release init --project P`): `provider=manual`, `trains`, channel `default`, no ref pattern, `release_terminal=live`, SLO `{boarding_grace_days: 14}`, `release_epoch=now`. That is the tenant-Land and human-flips-a-flag case; channels, release branches and `v`-prefixed refs are tenant-writable on the same bead.

The provider interface is a **write contract on the bead API**. `kd release <verb>` (new `kbeads/cmd/kd/release*.go`) is the reference client; `gb release` wraps it; curl works. Each request carries its evidence (`--source`, `--observed-at`, `--url`, `--commit`); the daemon writes patch + row in one transaction. A provider MUST use the channel's ref pattern, pass `--observed-at`/`--commit` when known, retry freely (self-transitions are no-ops), and never let reporting change its exit code (release-deploy.sh rule 8). `kd release check` validates the declaration against credentials (github-app.md:23-38) and prints a dry reconcile.

Provider roles: membership resolver (`--manifest -`, or nothing for window/boarded), evidence provider (`cut --ref [--commit --previous-ref --observed-at --train]`, `publish [--url]`, `live [--target]`, `fail --reason`), observer (`observe --channel --ref <probed or unknown> [--commit]` on **every** tick).

#### (a) gardenio

Policy bead: `provider=gardenio-calver`, `train_mode=continuous` (5-12 tags/day is continuous deploy), channel `default` `{ref_pattern: ^\d{4}\.\d{1,3}\.\d+$, observer_subjects: ["machine:release-deployer"]}`, `release_epoch` via `kd release bootstrap --ref 2026.293.10 --cut-at <tag date>`, SLO `{cut_to_published_min:45, published_to_live_min:45, observer_silent_min:60, boarding_grace_days:7, quiet_hours: today's constants}`. No workflow changes.

- **Cut** — `release-cut.sh` after `push_tag` (:184-216) has `FINAL_TAG` (:194) and `RELEASE_SHA` (:138-141); Step 2's loop (:84-93) must also retain the matched tag name as `PREV_TAG` (one line). New Step 6, guarded by `KD_URL`, exit-code neutral: `scripts/release-manifest.sh <PREV> <TAG>`, written fresh on the first-parent idiom of release-cut.sh:79 (`git log --first-parent --merges` for `Merge pull request #N`, a non-merge pass for `(<bead-id>)` suffixes and cherry-pick trailers; repo-root release-notes.sh:71-79 is `--no-merges`, only its refs/tags guard is reused), then `kd release cut --project gardenio --ref $FINAL_TAG --commit $RELEASE_SHA --previous-ref $PREV_TAG --observed-at "$(git log -1 --format=%cI $FINAL_TAG)" --manifest - --source script:release-cut.sh`. Optional `--require-boarded` exits 3 before tagging if frozen boarders are unmerged.
- **Published** — when the images.yml gate passes (release-deploy.sh:386-406): `publish --ref $CALVER --commit <run head_sha> --url <run html_url> --observed-at <run updated_at>`. No kd token in Actions.
- **Live / failed** — after `helm upgrade --atomic` + `verify-image-tags.sh` (:440-502): `live --target k3s/gardenio`; rollback → `fail --reason`. The P1 bug on exit 1/2 stays; the untyped ledger row becomes typed.
- **Observe** — `report_and_exit` calls `observe` on **every** exit path (all exit-3 outcomes incl. `images not ready` and `values drift`, plus 1/2; release-deploy.sh:217-221, 391-406), reporting the effective tag per component as `verify-image-tags.sh` computes it (release-pipeline.md:71-75) or `unknown`, with `--observed-at` from the tag's commit date it already fetches (:396-401). Needs `machinePrincipal.enabled` so the subject is `machine:release-deployer`.
- **Status** — `gb release status --from-beads` (release.go:139-192); the GitLab drought watchdog retires.

#### (b) Tenant Land, bare GitHub repo, Lands-App permissions only

No actions/packages, no webhook (github-app.md:16, :76). Lands are provisioned `triggers_enabled:false, schedules_enabled:false` (provision.go:496) and nothing ticks a tenant agent unprompted, so **tenants are `manual` until the Lands server exists**: the forecast works from day one via github-bridge MR beads, the agent boards tickets as it opens PRs, cut/live are Lands verbs. What makes `git-tag` real is operator-side: a fleet-level `release-tag-observer` in the controller polling `git ls-remote --tags` with the Lands App installation token (contents:read) for policies declaring `git-tag`, calling `cut --ref <tag> --commit <sha>` as `machine:release-tag-observer` (`ls-remote` yields no dates). Basis `sha-range` with a manifest, else `window` (lower bound), else `boarded`.

#### (c) Manual / no infra

`kd release init`, `kd release create "Newsletter #12" --due 2026-11-01` (`#13`, `#14` likewise; `next` is derived), `board`, optional `freeze`, `live --ref 12 --target newsletter` (cut folded). `close_reason=done` boarders ship, the rest roll, the bead closes. Health is `ok`/`stale` on `due_at`; never `unknown`. An npm library sets `release_terminal=published`, so `release-observe-npm.sh`'s `publish` closes the train.

---

### 4. Writers table

| writer | writes |
|---|---|
| kbeads daemon — `release_state_validation.go` (3 entry points), `release_reconcile.go`, `release_sweep.go` (periodic, like `decision_aging.go:119`) | rejects illegal transitions, missing `ref`/`failed_reason`, pattern/uniqueness/commit mismatch, out-of-range `observed_at`, `boards` after freeze, close on non-terminal, delete with evidence, unbound principals; stamps `*_at`, `release_key`; **writes every evidence row**; closes on terminal, reopens on `failed`/rollback; supersession; moved-ref re-mint; drift fix from declared observers; `reconcile_summary`; roll-forward; mints; sweep: `release-stale`/`release-observer-silent` labels, one P1 per breach keyed `release-alarm:<train>:<kind>` (restart/replica safe), auto-closed on recovery; `beads.release.updated` (S6) |
| humans / agents via `kd release` (`gb release` wraps) | `init`, `create`, `board/unboard`, `freeze/unfreeze`, `cut`, `publish`, `live`, `fail`, `abandon`, `ship/unship`, `observe`, `reconcile`, `bootstrap`, `check`, `next/show/list`; one request per verb, carrying evidence |
| `release-cut.sh` Step 6 + `release-manifest.sh`; `release-deploy.sh` | `cut` with manifest; `publish`, `live`, `fail`, `observe` on every exit (P1 bug kept) |
| github-bridge `pullrequest.go` (gitlab `gitlab_sync.go:233-248`) | MR join keys; opt-in `release-only` MR beads; optional `releaseSink` for tag `workflow_run`s (pipeline.go:127-131) |
| controller `release-tag-observer` (new); `release-observe-git.sh`, `release-observe-npm.sh` | `cut`/`publish`/`observe`; inputs ref + sha, `observed_at` optional |
| tenant / operator | the policy bead |
| beads-viewer, `/garden`, Slack | read-only; the viewer renders health, never sweeps |

---

### 5. Lands contract exposure

Additive bump `1.0.0-beta.1 → beta.2`; tag `release`; "train" stays internal; no `project`, `labels`, `fields`, backing ids or `/v1/` routes.

- `ReleaseState` (closed): the seven states. `Release` (8 of 8): `id`, `title`, `state`, `ref`, `channel` (absent = default), `due_at`, `live_at`, `updated_at`; required `[id, title, state, updated_at]`; `ref_url` waits.
- `ReleaseTicket` (4): `id`, `title`, `status`, `placement` (`planned, shipped, missed, unplanned, unmatched`).
- Requests: `ReleaseCreate {title, due_at, channel?}`, `ReleaseBoard {ticket_id}`, `ReleaseCut {ref, note}`, `ReleaseMark {note, target}`, `ReleaseFail {reason}`, `ReleaseClose {reason}`.
- Paths under `/lands/v1/lands/{land}/releases`: `GET` (`?state=&channel=`), `GET /{release}`, `GET /{release}/tickets`, named POSTs with `Idempotency-Key`: create, `/board`, `/unboard`, `/freeze`, `/unfreeze`, `/cut`, `/publish`, `/live`, `/fail`, `/abandon`. Illegal transition → `conflict`; pattern violation → `invalid_request`. `EventType += release.updated`. Policy stays out of the contract.

---

### 6. Failure modes

| failure | surfaced by |
|---|---|
| Stale leg | deadman on evidence-time anchor → `stale`, `release-stale`, one idempotent P1 |
| Observer dead / forged | heartbeat ages → `unknown`; undeclared subject → `rejected` row |
| Rollback (helm rollback, old `imageTag`, superseded train) | next tick: `rolled-back-to` re-entry, later train `failed` |
| Moved / deleted tag | commit mismatch → `failed: ref moved`, re-mint `@sha7`; gone → `failed: ref deleted` |
| Out-of-order evidence | legs fold in; late legs are evidence-only |
| Wrong human attestation | admissible; provenance in `CreatedBy` + `source`; corrected by `fail`/`live` |
| Non-bead PRs | `unmatched` placement; `window` reported as lower bound |

---

### 7. Implementation slices

1. **MR join keys + `close_reason`** — `config.go`, `client_mr.go:13-41,434-478`, `pullrequest.go:17-48,195-214`, `gitlab_sync.go:233-248`.
2. **Policy + train type + validator + manual verbs** — `config.go` (`type:release-policy`, `type:release`, `type:event` fields), `dependency.go`, `forgebeads.go`, `release_state_validation.go` (three entry points, release binding; prerequisite: verify the land-principal path, since release-deploy.sh writes `type:event` today), `service_field_policy.go`, `kd release`. Every project runs the lifecycle with kd alone.
3. **Reconcile + sweep + health** — `release_reconcile.go`, `release_sweep.go`, `kbeads/pkg/release/health.go`, `AddDependency` and MR-update hooks; optional partial unique index on cut `release_key` cloning migration 042.
4. **gardenio cut adapter** — `release-manifest.sh`, `release-cut.sh` Steps 2 and 6, checks.yml self-test.
5. **gardenio deploy adapter** — `release-deploy.sh` (`report_and_exit`), `machinePrincipal.enabled`, policy bead + bootstrap.
6. **gb + events + viewer read side** — `gb release --from-beads`, `TopicReleaseUpdated`, drought watchdog retired, `feed.go:1602-1618` off the title regex.
7. **Observers** — `release-observe-git.sh`, `release-observe-npm.sh`, controller `release-tag-observer`, optional bridge `releaseSink` and `release-only` MR beads.
8. **Lands contract beta.2** — `openapi.yaml`, `spec_test.go`, `fixtures_test.go`, bindings.
9. UX phase (out of scope).

---

### 8. Open questions for the operator

1. Unique `release_key` index now (migration 042 recorded a ~30h freeze) or dedupe-by-query first?
2. Should `--require-boarded` gate the gardenio cut? Default stays "main ships regardless".
3. Verb home: kd (chosen) with gb wrapper, or gb only?
4. Per-component channels for gardenio (the deployer has one `global.imageTag`), and opt-in `release-only` MR beads for its repo?
5. Keep untyped `release-deploy` ledger beads for one cycle alongside typed rows?
6. Lands beta.2: `ReleaseTicket.placement` now, or `Release` + verbs first?

---

### Changed after review

- **Authorization (three `project_scoped` refutations):** `project_scoped` dropped; release binding enforced in the validator (land principal → own project, token scopes, machine grants, OIDC `authorized_projects`); static-token writes `attested=false`; `source` stamped from the verified subject; `created_by` column only.
- **Observer forgery:** `release_observer_subjects`; `type:release` in `serviceFieldGatedTypes`; undeclared `observe` → `rejected` row.
- **Membership honesty:** `window`/`sha-range` are a lower bound; `reconcile_summary.unmatched` + placement `unmatched`; opt-in `release-only` MR beads.
- **Ref uniqueness, channels, release branches:** uniqueness per (project, channel); channel objects with repo/branch/paths/ref_pattern; cherry-pick join; fallback to `boarded`.
- **`release_terminal`:** `live|published`, per channel.
- **Derived `next`, ref binding, ls-remote:** uncut key `…/<id>`; `next` by `due_at` then `created_at`; `--train`; ambiguity or ref conflict mints post-hoc; observer inputs are ref + sha.
- **Opt-in tracking, `due_at` SLO, policy home:** tenant-writable `type:release-policy` required; `boarding_grace_days` anchors on `due_at`.
- **Continuous mode + gardenio churn:** standing holder; `continuous`+`manual` rejected without observer; `unknown` keyed on `observer_silent_min`; gardenio declares `continuous`; `trains`-mode automated cuts consume only frozen trains; "missed" = frozen-and-not-shipped.
- **Tenant git-tag:** tenants are `manual` until the Lands server; the controller's fleet-level tag observer is the operator-side path.
- **Health location + quiet hours:** `kbeads/pkg/release/health.go`; `quiet_hours` is policy.
- **Validator entry points, hooks, reconcile durability:** create/update/close+delete; `afterBeadUpdated` is new; reconcile synchronous + daemon sweep + `kd release reconcile`; MR-update trigger.
- **Supersession/rollback edges, leg folding, moved tag, epoch, single-writer idempotency, heartbeat volume, `null` ref, `observed_at` bounds, channel-level observe:** adopted as written in §1-§2.
- **Sweeper:** in the daemon with idempotent alarm creates; `release-stale` replaces its use of `release-stuck`; own period (`lands_apply_reactor.go` is 30 s).
- **Citations:** `PREV_TAG` retention; release-manifest.sh written fresh (release-cut.sh:79); github-bridge persists neither decoded field; github-app.md:16/:76.
- **Partial disagreement:** the health fix suggested re-export "through `gen/`"; `gen/` is generated protobuf, so a new public `kbeads/pkg/release` package is used instead, same import direction.

---

## Part 3. UX: one vocabulary across browser, Slack, CLI and Lands

Scope: every human and agent surface over the release-train mechanism (`mechanism-spec-revised.md`). All surfaces are views over `type:release`, `type:release-policy`, `boards` / `shipped-in` / `follows` edges and `type:event` evidence children; nothing computes membership or health outside the daemon and `kbeads/pkg/release`. Every write is one daemon-validated bead write; there are no drafts.

### 1. Vocabulary and states

One table, exported from `kbeads/pkg/release` (`Word()`, `Mark()`, `Token()`) and consumed by beads-viewer, `/garden`, the Slack bridge, kd, gb and gl. The noun is **release** on every human surface; *train* appears only in Go identifiers and the kd `--help` glossary. "Next" is a derived tag, never stored. Display name is `ref`, else `title`; an uncut, untitled release is "next release (not yet cut)" — never an invented number.

| Kind | Word (spelled exactly, all surfaces) | Text mark | Colour token | Plain-English label |
|---|---|---|---|---|
| state | boarding | `○` | `--box` | collecting work; open for boarding |
| state | frozen | `◇` | `--box` | contents locked; boarding refused |
| state | cut | `▸` | `--box` | a version exists |
| state | published | `▸▸` | `--box` | artifacts built |
| state | live | `✓` | `--ok` | running where it is meant to run |
| state | failed | `✕` | `--alert` | rolled back, moved or declared failed (reason always shown) |
| state | abandoned | `⊘` | `--muted` | dropped or superseded (reason always shown) |
| health | ok | none | `--ok` | terminal reached, or boarding on schedule |
| health | in flight | none | `--box` | a later leg is still expected; "waiting 9m / 45m since published" |
| health | stale | none | `--warn` | a leg is over its SLO: "stale · waiting 1h 12m / 45m since published" |
| health | failed | none | `--alert` | state is failed |
| health | unknown | none | `--warn` | observer silent: "unknown · observer silent 2h 10m (limit 60m)" |
| placement | planned | `[ ]` | — | boarded, not yet shipped (also on cut releases) |
| placement | shipped | `[x]` | — | has a shipped-in edge |
| placement | missed | `[!]` | `--warn` | boarded, release reached its final state without it; "→ rolled to …" |
| placement | unplanned | `[+]` | — | shipped without being boarded |
| placement | unmatched | `[?]` | — | a change with no bead ("PR with no bead") |
| placement | forecast | `[~]` | — | merged since the previous cut, not boarded (operator surfaces only) |
| basis | manifest · sha-range · window · boarded · epoch | — | — | always printed with its honesty suffix: `window · lower bound`, `sha-range · lower bound`, `boarded · human attestation`, `epoch · first release on this channel` |

Rules the table carries:

- **Health is a word, never a mark.** It is coloured by token and always followed by its cause and anchor. The only marks on a page are the state mark (header chip) and placement marks (rows); no mark means two things on one surface.
- **missed is final.** Before a release reaches its terminal or abandoned state, a boarded-not-shipped leaf is `planned` everywhere (on cut releases the count line reads "1 planned (not yet shipped)"). This replaces "pending", "missed pending", `0*` and Lands' premature `missed`.
- **Planned = boarded \ shipped.** The daemon removes a continuous holder's `boards` edge when a leaf gains `shipped-in`; every read model subtracts anyway. A bead with both edges renders shipped.
- **in flight** has a space outside JSON; JSON keeps `in_flight`.
- **Times** are evidence times in UTC (`14:02Z`; RFC3339 in JSON), "recorded 3 min later" when posting time differs, `time unknown` for a post-hoc mint, `folded into live` for a folded leg. Ages use `fmtTimeAgo`.
- **Actors** render as display names (machine grant name, user name, land name); the verified subject sits in `title`/muted suffix; a static-token write reads "not attested".
- **ship** is reserved for attestation (`shipped-in`). Boarding verbs say *board*; the viewer row action is "Mark shipped / Unmark shipped"; Lands has no ship verb.

Health anchors (fix to mechanism §2, lands in `kbeads/pkg/release/health.go` and the sweep): `stale` for a cut/published release measures from **that leg's own `*_at`** (`cut_at` for cut→published, `published_at` for published→live); the observer heartbeat feeds only `unknown` and the drift fix, never the deadman. A continuous-mode holder is never stale and has no due date: its health is `ok` and its state chip reads "boarding · standing". Precedence: failed > unknown > stale > in flight > ok. The sweeper also files `release-alarm:<id>:failed` so a rollback reaches Slack.

### 2. The seven scenarios (acceptance script)

**S1 — "What is going to be in the next release?" (gardenio, continuous).** Browser `/releases?project=gardenio`: NEXT card "next cut · boarding · standing · 4 planned · forecast 7 · lower bound", health ok, no due chip, no stale path. Slack `/release` in #release-notifications resolves gardenio via the alert ChannelMap and prints the same four planned (bead-type glyph per row, "merged"/"open" as words) and the forecast group with its lower-bound footer. `kd release next --forecast` lists `[ ]` planned then `[~]` forecast. The holder's planned list contains only unshipped leaves; nothing fabricates a calver.

**S2 — "When will my PR be live?"** Agent wraps up; its card line reads "Boarded for the next cut — PR #731 merged 12m ago → next cut (gardenio cuts 5-12×/day), then ≤45m to published, ≤45m to live", or "— PR #731 open, not merged: nothing ships until it merges". `@gardenio release` or `status` in the thread (new exact phrases) replies with the same line instead of respawning a pod; the bead page sidebar and `kd show` print the identical ETA sentence from `release_slo`.

**S3 — Cut at 12:04, images never built, nobody watching.** 12:49: health flips to stale from `cut_at` (heartbeats from `report_and_exit 3 "images not ready"` do not move the anchor); sweeper files `release-alarm:<id>:stale`; Slack alarm "Release stale — 2026.294.2: cut 12:04Z, no published after 45m"; `/health` tile sorts gardenio first; `/garden` wagon stops with ◷, biome sign ◷, beacon and Tend stop; `gb release status` says `state: pending, latest_pipeline_status: cut→published, release_health: stale`. Every card reads "stale · waiting 45m / 45m since cut".

**S4 — Tenant declares "v1.4 is live" with three tickets closed by hand.** `gl release live <id> --ref v1.4 --tickets or-T1 or-T2 or-T3` → one PATCH; daemon boards, folds cut, ships the three (basis declared). Without `--tickets`, a manual live with zero boarded tickets counts tickets closed done since the previous release and gl prints "3 tickets closed since 1.3 were counted; unboard any that did not go out". `gl ticket releases or-T1` → "shipped in v1.4 (live since 2026-11-01T09:00:00Z, as marked by the land)". The default tenant policy has no version pattern; if one exists, `gl release show` prints it and the Problem detail quotes it.

**S5 — helm reports the older calver.** Deployer `observe --ref 2026.293.10` → 2026.293.10 re-enters live (rolled-back-to), 2026.294.2 → failed "rolled back to 2026.293.10"; sweeper files `release-alarm:…:failed` → Slack alarm posts, card header "✕ 2026.294.2 — failed", thread reply "rolled back to 2026.293.10 by release-deployer at 16:02Z — the deployer redeploys at its next tick unless paused". Bead chip: "shipped in 2026.294.2 — rolled back, not live (live: 2026.293.10)" in warn; `kd show` and `gl ticket releases` carry the same state. When 2026.294.2 goes live again, the reopened 2026.293.10 is superseded → abandoned, so LIVE never shows two.

**S6 — Human merged PR #740 with no bead.** Manifest basis puts it under Unmatched everywhere (count on the Slack card, row on Show, `[?]` row in kd). Train page row offers "Create a bead for this PR" (chore bead from the subject, `mr_url` set, LinkTaskToMR, re-reconcile); `kd release link --release 2026.294.2 --pr 740 --create` does the same; reconcile re-resolves `unmatched[]` by `mr_url` on every run, so the PR moves to shipped. "Notes" on the card/train page and `kd release notes 2026.294.2` render the changelog.

**S7 — New project, no policy.** `/p/newproj` → "Releases →" → `/releases?project=newproj` renders newproj's own empty state: "newproj does not track releases yet", the three adapter stories, and an operator-only "Start tracking (manual releases)" button (write-proxy for the `kd release init` composition). Hub line, Slack reply ("Release tracking is not set up for newproj — <link>") and the garden Plans drawer link there; the kd command appears only on kd surfaces.

### 3. Browser (beads-viewer + /garden)

#### 3.1 Releases index — `GET /releases?project=&channel=&state=`

Header: h1 "Releases"; right end the single primary action ("+ New release" for manual/trains, "Board on next →" for continuous). Filters reuse the `/plans` chip idiom (`plans.html:1726-1762`, `planProjectOptions`); on phone they collapse into `<details summary="Filters">` so the NEXT card's top is < 667 px (floor test asserts it). An untracked `?project=` renders that project's empty state (S7), never another project's data.

Body per project, row 1 **Now** (`.release-now`, three columns ≥ 60rem, stacked below):

- **NEXT** — the derived next as a card: name, state chip (`○ boarding` / `◇ frozen` / `○ boarding · standing` for continuous), due chip only when `due_at` exists (loud past due), health word + cause, counts "4 planned · 7 forecast · lower bound" or "4 planned · forecast unavailable (no repo declared for this channel)". Empty: "Nothing is boarding." + the primary action; continuous with no holder: "The standing release is missing — run a check" (loud).
- **IN FLIGHT** — one card per open cut/published/failed release: name, state chip, leg line "cut 12:04Z → published 12:22Z → live —", health "in flight · waiting 9m / 45m since published" | "stale · waiting 1h 12m / 45m since published" | "failed: ref moved abc1234→def5678" | "unknown · observer silent 2h", counts "12 shipped · 1 planned (not yet shipped) · 3 unplanned · 2 unmatched · manifest". Manual projects say "no observer declared".
- **LIVE** — newest terminal: "live 2h ago (recorded 3 min later)", observer line, "live again (rolled back to) · 14m ago" on re-entry. Empty: "No release recorded since Oct 19 (epoch)".

Row 2 **History**: table ≥ 60rem / cards below; columns name · state · cut → published → live durations · shipped / missed / unplanned / unmatched pills · basis printed inline with its suffix ("window · lower bound") · actor display name. Abandoned rows muted with reason. 20 per page, cursor.

Data: `ListBeadsFiltered type=release`, policy cache 60 s (`plans_project_cache.go` shape), forecast from the daemon's `GET /v1/release/next?forecast=1` (never computed in the viewer), health via `release.Health(train, policy, s.now())`. Live: viewer hub (`bead_feed_hub.go`) on `beads.bead.>` + `beads.release.updated`, SSE `release-card {id}` → OOB card swap (`plan_card.html:19`); the 60 s health timer swaps only the health `<span>`, never a fragment holding links or levers. Write: `POST /releases` → daemon create (boarding). A11y: `/releases` and `/releases/{id}` join `routes.spec.js` and `FIXTURE_KEYS` with a `release` fixture; every form field has a `<label>`.

#### 3.2 Train page — `GET /releases/{id}`

**Header** (`#release-header`): breadcrumb Releases › project › channel (when not default); h1 = name with title subline; chips: state (mark + word), health (word · cause · anchor), due (boarding only), provider, "next" tag, "bead →", "in the garden →". On phone the Controls panel's primary lever repeats at the header's right.

**Lifecycle rail**: five legs boarding → frozen → cut → published → live, each reached leg with evidence time and actor display name ("cut · Oct 21 12:04Z · release-cut.sh"), `time unknown` / `folded into live` / "skipped" for frozen; the live leg dashed "not tracked for this channel" when `release_terminal=published`. **Phone: the legs wrap to two rows; never an inner horizontal scroll** (same treatment in the garden face and the Lands web page). failed and abandoned render as a full-width band under the rail with reason, actor and the retry/redeploy hint ("the deployer redeploys at its next tick unless paused").

**Grid** (`.release-grid`, mirrors `.plan-grid`):

- *Manifest* (left): head "What's in it" + basis badge with its sentence inline ("window · lower bound — only bead-linked PRs are counted; human PRs and direct commits are not listed"); summary pill row (not sticky on phone); groups in order Planned, Forecast (uncut) / Shipped, Planned (not yet shipped), Missed (terminal only), Unplanned, Unmatched; rows = placement mark + word · bead link with hover preview · title · status chip · MR pill · assignee · operator row actions (Unboard, Mark shipped / Unmark shipped). Unmatched rows: PR link · sha7 · subject · "no bead" · [Link to a bead…] [Create a bead for this PR]. Each group has its own empty line (e.g. Shipped under window: "Nothing counted — the window found no bead-linked PRs; lower bound").
- *Evidence* (right): Observer line ("release-deployer · last seen 7m ago · reports 2026.293.10" | "No observer declared — state is what people mark" | warn "Observer silent 2h 14m — health unknown"); rows newest first: action word, evidence time, "recorded Δ later", actor display name, "not attested" for static tokens, loud "rejected" rows, rolled rows naming leaves; "older →" cursor.
- *Blockers*: `blocks` edges, release-stuck/preflight-broken/release-pipeline-broken decisions (shared helper lifted from `gb/release.go:425-438`), open `release-alarm:<id>:*` P1s; "waiting on you" wording from FarmWait.
- *Controls* (operator): one primary per state — boarding → **Freeze**, Board a bead…, Cut… (hidden for continuous), Abandon (danger); frozen → **Cut…**, Unfreeze, Board anyway (force); cut → **Mark published…**, Mark live…, Mark failed… (danger); published → **Mark live…**; failed → **Retry as…**, Abandon (danger); terminal → "Finished — reopened only by a rollback or drift fix"; **Reconcile now** always. Each lever confirms naming the target and its effect, shows pending within 1 s, disables while pending, and surfaces the daemon's refusal verbatim. For non-manual providers: "Normally written by release-cut.sh / release-deploy.sh; a mark here is recorded as a human attestation under your name".

Writes: thin proxies (`plans_lifecycle.go` carve-out) `POST /releases/{id}/{board|unboard|freeze|unfreeze|cut|publish|live|fail|abandon|ship|unship|reconcile}`, `/unmatched/{pr}/{link|create}`, each forwarding `X-Operated-By`. Live: `/releases/{id}/events` (`handlePlanEvents` pattern) re-renders named fragments; on reconnect every fragment is refetched and "reconnected" announced once.

#### 3.3 "Did my bead ship?" — bead page, MR page, plan step, garden panel

Header chip (one, word first): "shipped in 2026.294.2" (ok) · "shipped in 2026.294.2 — rolled back, not live (live: 2026.293.10)" (warn) · "planned for Newsletter #13" · "boarded for the next cut" (continuous) · "missed 2026.294.1 → rolled to 2026.294.3" (warn) · "shipped in 2026.294.2 · window · lower bound". A closed bead with no edge says **"not shipped"** only when the reason is known (close_reason wontfix/duplicate, closed before epoch, or manifest basis examined the PR); otherwise "no shipping record · window · lower bound". No chip in an untracked project. Sidebar "Release" row carries the full sentence, including the S2 ETA ("merged 12m ago → next cut …"), and "Not tracked — start tracking →" with the S7 link. `type_mr.html` adds the chip plus Merged / Shipped in rows. Plan step rows get the chip after the MR pill (batched forward-edge enrich, `plans_mrs.go` idiom); the `/plans` card facet is renamed "last step closed". Garden bead panel adds one "Release" line with the same wording. Lever: "Board on next release" / "Unboard" (`POST /b/{id}/board|unboard`), daemon resolves next; frozen 409 shown.

#### 3.4 /garden World

Dock band cell per tracked project (absent for untracked, present with an empty gate for tracked-but-empty). Wagon at the gate = derived next (open; `◇` tailgate closed when frozen; pips ≥ 3 px, capped 9+); wagons on the road per open cut/published release; failed = tipped `✕`; stale = still with `◷`; **unknown = upright with `?` in `--warn`, no fog tint**; newest live parks with ✓ for 24 h; cap 6 then a "+N" crate; business theme draws `[RELEASE]` crates with the same words. Biome sign carries one health mark only for failed/stale/unknown. Every wagon, sign mark and pip has an `aria-label` equal to its cursor label ("release 2026.294.2 · published · stale · waiting 52m / 45m since published"); a boarded task cell's accessible name appends "· boarded for the next release" (asserted in `a11y_test.go`). Descend on a wagon opens the typed "release" face: state + health line, the rail as two wrapped rows, pill row, groups as `<details>`, observer line, last five evidence rows, blockers, and "Open in Workbench (leaves the garden)". Beacons: failed/stale/unknown or a blocker waiting on a human. Tend tour: a stuck release or a boarding release past due. Plans drawer: "Next release: … →" per tracked project. Farm doc adds `releases[]`, `FarmTask.boarded`, `meta.releases_as_of` (schema 1 additive, golden fixture updated); health computed at refresh with `release.Health`; stale roster hatch when `releases_as_of` > 2 min.

#### 3.5 Front page, /feed, /health, project hub

`/feed`: one line per `beads.release.updated` from the payload ("🚜 gardenio · 2026.294.2 · cut → published · 18m · by release-deployer"), the title regex in `feed.go:1602-1618` retired. `/health` "Build & release" tile: "This viewer runs 2026.293.9 · behind live 2026.293.10" then one row per tracked project ("gardenio · live 2026.293.10 2h · next boarding (4) · ok"), failed/stale/unknown first; links to `/releases`. `/p/{project}`: eager rail line "Release: live 2026.293.10 · 2h ago · next boarding (4) · ok" (or "Not tracked — start tracking →"); tab "Releases →"; lazy section (first in order) = the Now strip partial.

### 4. Slack (slack-bridge)

**Command family** `/release` (replaces `/unreleased`, kept one release as an alias with a pointer line): `next` (default), `status [<ref>]`, `show <ref|id> [--placement] [--bead]`, `list`, and gated writes `board`, `unboard`, `freeze`, `unfreeze`, `create`, `cut`, `live`, `fail` (cut/live only for `provider=manual`). Project resolution: `projectFromChannel`, then the alert ChannelMap slug mapped back to its project (so #release-notifications answers), then `--project`; with nothing resolved the reply lists tracked projects as buttons. No policy → "Release tracking is not set up for *p* — <viewer link>". Every view and reply has a non-empty `text` fallback ("next release gardenio: boarding, 4 planned"). Strings never contain train, bead or daemon (golden-test grep); the error line is "Couldn't read releases (service error) — nothing below is current".

**Glyph discipline** (U6): one glyph in the header — Garden: the state glyph from a new `Theme.ReleaseState` (🪣 boarding, ❄️ frozen, 🌽 cut, 🛻 published, 🏡 live, 🪨 failed, 🍂 abandoned), Business: `[BOARDING]`…`[ABANDONED]` tags (no marks, so nothing collides with Lifecycle ○◐●). Health, legs and placements are words (Business bracket tags where a tag exists); body rows carry at most the bead-type glyph. `BeadTypeBadge` gains `release` (🚜 / `[RELEASE]`). theme.go and `gardenio/plans/slack-greenhouse-restyle.md` change together.

**Release Next** (ephemeral, ≤ 14 blocks): header "🪣 *Next release — gardenio*"; context "next cut · boarding · standing · cuts on every tag" (continuous) or "Newsletter #12 · boarding · due Fri 24 Oct · 3 days left"; Planned (n) rows "ka-… title · closed · merged"; Forecast (n) rows with the fixed footer "_Forecast is a lower bound: only PRs tied to a bead are visible (basis window)._" or "_Forecast unavailable: this channel has no repository; planned is the whole answer (basis boarded)._"; Blockers; Previous; footer with the write verbs.

**Release Status** (ephemeral): roll-up "ok · 1 in flight · 0 stale · 0 unknown · 0 failed"; per channel the leg line "*2026.294.3* · cut 14:02Z → published 14:21Z (19m / 45m) → live — waiting 31m / 45m since published" (spaces before arrows so phones wrap there); observer line ("observer release-deployer saw 2026.294.2 at 14:15Z (7m ago)"); counts "planned 7 · shipped 7 · unplanned 2 · unmatched 1 · basis manifest"; blockers and alarms; footer "health as of 14:33Z (computed here from release evidence) · quiet hours: off". Unknown: bold observer line, durations suffixed `?`; stale: bold waiting figure; failed: "*Reason:* …".

**Release Card** (`release_cards.go`, one `PostMessage` per cut release, `chat.update`d in place; card ref = `type=message kind=card subject_id=<release>` bead via `agent_cards.go` generalised): header "🛻 2026.294.3 — published" / "[PUBLISHED] 2026.294.3"; context project · commit · run link · "cut by release-cut.sh"; legs line (shared `releaseLegsLine`); counts; top-5 members; observer + health line; actions: **Show**, and for manual non-terminal releases **Mark live** (primary) and **Mark failed** (danger, reason modal titled "Mark 2026.294.3 failed?" naming the effect); footer "updated 14:33Z · /release status 2026.294.3". Transitions: failed → header "🪨 … — failed", reason section, thread reply that is state-specific (S5 wording; retry hint only for a genuine failure); abandoned → collapsed header + "superseded by …"; live → "closed 14:40Z"; rollback re-entry → "(rolled back to)". Hash excludes `observed_at`; 3 s min interval; 5-min force refresh keeps "waiting" honest. Agent-card refreshes are coalesced per transition and skipped when the Ships-in text is unchanged; policy `release_slo.cards: per_cut|daily_rollup` chooses cadence.

**Release Alarm** (`label_alert.go`, generalised from `coverage_alert.go`): kinds stale, observer-silent, drought, **failed**; "🌵 *Release stale — 2026.294.3 (gardenio)* · published 14:21Z, no live after 1h 12m (SLO 45m)"; drought appends "(bead-linked PRs only — lower bound)"; bullets Release / Last evidence / Observer / Blockers / Alarm bead; edited to "🌼 *Recovered — …*" on auto-close, "*Dismissed —*" on wontfix; routed by `alert-channel:<slug>` → ChannelMap → default → project channel → drop (never a DM); deduped by alarm bead id, restart-safe via the card bead.

**Wrap-up "Ships in" line** (`buildWrapUpAgentCardBlocks` after the restart-condition section; `formatWrapUpSlack`): "🚜 Boarded for the next cut — PR #731 merged 12m ago → next cut (5-12×/day), then ≤45m to published, ≤45m to live" · "— PR #731 open, not merged" · "Shipping in *2026.294.3* — published 14:21Z, waiting 9m / 45m" · "Shipped in *2026.293.10* — live 14:19Z" · "In *2026.294.2* — failed: rolled back to 2026.293.10, not live" · "Missed *12* → rolled to *Newsletter #13*" · "Not boarded — reply "@gardenio board this"". Omitted for untracked projects or failed lookups; shipped-in wins over boards.

**Thread boarding**: exact phrases `board`, `board this`, `put this on the next release`, `board <ref>`, `unboard`, plus `release` / `status` which reply with the Ships-in line (no respawn when the agent has wrapped up). Reply "○ Boarded *ka-…* on *Newsletter #12* — boarding, due Fri 24 Oct"; frozen 409 relayed in a code span; no boarding release in trains mode → "No release is boarding on default — /release create …" (the bridge never mints). Writes run under `withSlackActor` so evidence rows say the user's name, attested.

**Dashboard row**: "🚜 2026.293.10 live · next: 7 planned" / "🚜 2026.294.3 published · waiting 9m / 45m" / "🚜 2026.294.3 stale 1h 12m"; omitted with no policy; "🚜 releases: /release status" when the channel maps to several projects.

### 5. CLI (kd, gb, gl)

Shared conventions: `--json/--jq` through `emitJSON` / `printJSON`; exit 0 success or idempotent no-op, 1 transport/flags, 2 refused by the validator (one-line stderr `{"error":"release_refused","code":…}`), 3 nothing to act on (no policy, no next, `--require-boarded` unmet), 4 `--fail-on` health match, 64 operator refusal. gl adopts the same codes (3 for "no release is boarding"). `--watch` is honest polling of `GET /v1/events` (trailer "-- watching (poll 5s) --"). Marks are the §1 placement marks; legs not reached print `-`; arrows are `->`.

| Verb | Key flags | Idempotency | `--json` |
|---|---|---|---|
| `kd release init --project P` | `--provider --mode --terminal --channel … --grace-days --epoch --observer` | key `release-policy:<P>`; re-run prints `(exists)`, never overwrites (`--set k=v` to change) | policy bead |
| `check [--project]` | — | read-only; `[ok]/[warn]/[FAIL]` probes + dry reconcile; exit 2 on FAIL | probes[] |
| `show <id\|ref\|next>` | `--contents --evidence --watch --fail-on` | read-only | the stable show object (header, legs, contents{basis, final, lower_bound, counts, items, unmatched}, blockers, alarms, evidence, next) |
| `list` | `--project/--all-projects --channel --state --health --open/--all --short --watch` | read-only; exit 3 when untracked (never a silent empty table) | bare array or `--json-envelope` |
| `next` | `--forecast --mint` | `--mint` key `release-next:<P>:<C>:<prev>` | show object + forecast block (`lower_bound: true`) |
| `create <title>` | `--due --channel --idempotency-key` | by key | `{action, train}` |
| `board / unboard <bead>…` | `--train <id\|next> --force / --if-exists` | no-op lines, 409 frozen → exit 2 | `{action, train, beads[{id,result,reason}]}` |
| `freeze / unfreeze [id\|next]` | — | self-transition no-op | train header |
| `cut --ref R` | `--train --commit --previous-ref --observed-at --url --manifest - --source --require-boarded --dry-run` | repeat = no-op; duplicate ref mints post-hoc and reports it | `{action, noop, train, evidence, reconcile, minted[]}` |
| `publish / live / fail / abandon` | `--ref\|<id> --url --target --reason --observed-at --source` | late leg = evidence only; `--reason` required for fail/abandon | same |
| `ship / unship <bead>… --train` | `--reason --if-exists` | edge set difference | same |
| `link --release R --pr N [<bead>\|--create]` | — | by `mr_url` | same |
| `reconcile <id>` | `--dry-run` | idempotent | reconcile summary |
| `observe --channel C --ref <ref\|unknown>` | `--commit --observed-at --source --newest-ref` | never exits 2 (heartbeats are exit-neutral); `--newest-ref` lets the daemon mint a post-hoc cut for a tag it never saw | observe result |
| `bootstrap --ref R --cut-at T` | `--state` | epoch train | train |
| `notes <ref>` | `--format md` | read-only changelog (shipped by type, unplanned, unmatched with PR links, basis sentence) | sections |

`kd show <bead>` gains a "Release:" line and JSON `release{train_id, ref, placement, state, health, basis}` ("shipped in 2026.294.2 (failed: rolled back to 2026.293.10)" when the release is failed). `kd release show` prints `live (not tracked: terminal is published)` for such channels; `kd project` gains a release line.

**gb wrapper**: `gb release status --from-beads` keeps the stable JSON and adds `source, train_id, release_state, release_health, health_reason, contents_basis, contents_lower_bound, next_train_id, next_planned_count, observer_age_min, unmatched_count`. Projection table documented in `release.go`'s contract comment: health ok at terminal → `green-golden`; failed → `red-rc`; in flight/stale → `pending`; **unknown → `pending` with `latest_pipeline_status: "observer-silent"`** (never golden on silence). `gb release next|mine|stuck|board` print kd's lines. At `gb stop`, `stop_releaseboard.go` runs after `autoLinkWrapupMRs`: warn-and-auto, never blocks, `--no-release-board` skips; it boards only when the policy's `release_auto_board` is true (default true for continuous, false for trains, where it prints "release: not auto-boarded (policy) — kd release board gd-x1 to plan it"); continuous line reads "boarded on the standing release — the next cut reconciles it".

**Manual golden path** (printed as the `init` trailer): `kd release init --project newsletter` → `kd release create "Newsletter #12" --due 2026-11-01` → `kd release board gd-a1 gd-a2 gd-e3` → optional `kd release freeze` → `kd release live --ref 12 --target newsletter` (cut and published folded; boarded leaves closed done ship, the rest miss and roll to the minted next) → `kd release show 12` ("ATTESTED: shipped = boarded leaves closed done; correct with kd release ship/unship"). Health for a manual project is only ok, stale (past `due_at` + grace) or failed; `check` prints "[ok] no observer declared: health can never be unknown".

### 6. Lands API beta.2

Additive bump; tag `release`; no backing names in descriptions (train, bead, daemon, project, channel, label, sha, commit, PR are denylisted).

Objects (≤ 8 fields, `additionalProperties: false`): **Release** {id, title, state, since, health, ref, due_at, basis}, `basis` ∈ {declared, observed} with the fixed sentences ("what this land marked done; the repository was not consulted" / "matched against the repository; a lower bound for changes without a ticket"); **ReleaseItem** {id, title, status (absent for unmatched), placement, release_id}; **ReleaseEntry** {action, at, note, ref, target, author}; pages `{items, next_cursor}`. Enums closed: ReleaseState (seven), ReleaseHealth (five), ReleasePlacement (planned, shipped, missed, unplanned, unmatched — `missed` emitted only on terminal/abandoned releases; a boarded, unshipped ticket stays `planned`), ReleaseAction (cut, published, live, failed, abandoned). Requests: ReleaseCreate {title, due_at}, ReleaseUpdate {title, due_at} (boarding/frozen only), ReleaseBoard {ticket_id}, ReleaseCut {ref, note, observed_at, ticket_ids}, ReleaseMark {ref, target, note, observed_at, ticket_ids} (`ref` required only when uncut; `ticket_ids` boards and attests in one write), ReleaseFail {reason, observed_at}, ReleaseAbandon {reason}.

Routes under `/lands/v1/lands/{land}`: `GET/POST /releases` (`?state=&health=&ref=`; open releases first with the derived next at items[0]), `GET/PATCH /releases/{release}`, `GET …/items?placement=`, `GET …/history`, `POST …/{board|unboard|freeze|unfreeze|cut|publish|live|fail|abandon}` with `Idempotency-Key` forwarded as the bead/evidence key, `GET /tickets/{ticket}/releases`. Event `release.updated` (object Release) on state, health and item-set changes. Problems: illegal transition → conflict; pattern/`observed_at`/missing field → invalid_request with the pattern quoted; other land's ticket → not_found; guest write → forbidden; no policy → conflict ("releases are not set up for this land; ask in #support"); kbeads down → upstream_unavailable.

gl: `gl release list|next|show|items|history|create|edit|board|unboard|freeze|unfreeze|cut|publish|live|fail|abandon`, `gl ticket releases`, `gl watch --type release.updated`. List marker column is `>` for next only; health is a word. Contents line before cut: "contents: planned (boarded tickets; work merged outside a ticket is not counted)". Ticket lines: "shipped in 12 (live since …, as marked by the land)", "in 1.4 (failed — not live)", "planned for 1.5 (boarding, due …)", "missed 12, rolled to Newsletter #13", "No release recorded for this ticket." The land home strip shows next and any release with health in (stale, failed, unknown), omitted when empty. The provisioner writes `release-policy:<land>` with manual defaults and no version pattern. Tenant pods get the `## Releases` prime block (also enabled for any project with a policy); cut/publish/live/fail/abandon from a pod are refused by the restricted-ops check.

Withheld in v1, by design: the policy itself, `channel`, `observe`, commit/sha/ref_url/release_key, provenance beyond `author`, reconcile internals, per-ticket evidence rows, ship/unship/reconcile/bootstrap/check/init, supersession, alarms (surfaced only as health).

### 7. Cross-surface consistency rules

Every surface that names a release must show, in this order: name (ref else title; "next release (not yet cut)" when neither), state word, health word with its cause and anchor, and the basis with its honesty suffix wherever contents or counts appear. Every surface that names a bead's placement must carry the release's current state and, for a failed release, the channel's live ref.

Every surface must never: invent a ref; show `stale` for a continuous holder; show `missed` before a terminal/abandoned state; say "not shipped" without a known reason under window or sha-range; hide "lower bound" or "human attestation" behind hover-only UI; let health upgrade on observer silence; use one mark for two meanings on one page; say train, bead or daemon to a Slack or Lands user; let the heartbeat move a stale anchor; post a Slack alarm without a recovery edit; write anything outside the daemon-validated bead API.

Shared code: `kbeads/pkg/release` exports `Health`, `Word`, `Mark`, `Token`, `LegsLine`, `WaitingPhrase` ("waiting <elapsed> / <SLO> since <leg>") and `ETASentence(policy)`; beads-viewer, gardenhub, the bridge, kd, gb and gl import it rather than restating any table.

### 8. Ordered UX implementation slices

| Slice | Needs mechanism | Files |
|---|---|---|
| U0 Shared vocabulary + health anchors | M3 | `kbeads/pkg/release/{health,words,marks,phrases}.go` + table tests (per-leg anchor, continuous holder ok, precedence, holder planned subtraction in `release_reconcile.go`, `release-alarm:*:failed` in `release_sweep.go`) |
| U1 kd read + planning verbs, `kd show` line | M2, M3 | `kbeads/cmd/kd/release*.go`, `show.go`, `project.go`, `output.go` |
| U2 kd provider verbs + adapters | M4, M5 | `kd release cut/publish/live/fail/observe/bootstrap/link/notes`, `gardenio/scripts/release-cut.sh`, `release-deploy.sh` (`--newest-ref`, pending report), `checks.yml` self-test |
| U3 Viewer index + train page (read) | M3, M6 | `beads-viewer/releases*.go`, `templates/releases.html`, `release_card.html`, `release_now_strip.html`, `release_detail.html`, `type_release.html`, `routes.spec.js`, `seed/fixtures.mjs` |
| U4 Viewer writes + bead affordance | M2, M6 | `releases_lifecycle.go`, `bead_show.html`, `bead_actions.html`, `type_mr.html`, `plan_steps.html`, `plans_mrs.go`, `plans.html` (facet rename), untracked empty state + "Start tracking" proxy |
| U5 Viewer health everywhere | M6 | `health.go`, `health_tile_build.html`, `feed.go`, `project_hub.go`, `project_hub_sections.go`, `bead_feed_hub.go` |
| U6 Slack theme + read views | M3, M6 | `bridge/theme.go`, `plans/slack-greenhouse-restyle.md`, `release_view.go`, `bot_release.go` (replaces `bot_unreleased.go`), `bot_commands.go`, golden tests incl. vocabulary grep |
| U7 Slack card, alarms, wrap-up, thread | M3, M6 | `release_cards.go`, `label_alert.go` (from `coverage_alert.go`), `agent_cards.go`, `bot_agents.go`, `bot_agents_wrapup.go`, `bot_mentions.go`, `bot_decisions_modal.go`, `dashboard.go`, `helm/gardenio/values.yaml` |
| U8 gb wrapper + stop auto-board | M6 | `gb/release.go`, `release_test.go`, `stop_releaseboard.go`, `stop.go`, `prime.go` (`## Releases` block), `gb mr status` |
| U9 /garden Dock + face | M3, M6 | `gardenhub/farm_model.go`, `farm.go`, `bead.go`, `web/{world,bead,beacons,tour,plans}.js`, `garden.css`, `garden.html`, `commons/cards.go`, `a11y_test.go`, golden fixture + commons-ux-acceptance captures |
| U10 Lands beta.2 + gl | M8 | `lands/contract/openapi.yaml`, `spec_test.go`, `fixtures_test.go`, fixtures, `landsv1/version.go`, bindings; Lands server projectors and writes; `provision.go`; gl verbs |
| U11 Acceptance + docs | all | Playwright floor tests (375×667, rail wrap, SSE reconnect, lever POSTs, a11y after swap), slack-ux-acceptance on both themes, `gardenio/docs/release-trains-design.md` Part 3, runbook section, `kd release --help` |

### 9. Open UX questions for the operator

1. **Missed before terminal**: this spec shows `planned (not yet shipped)` until the release is final. Alternative: a sixth placement `pending`. Both are consistent; the first keeps Lands' enum at five.
2. **gardenio card cadence**: one Slack card per cut (5-12/day) or a daily roll-up card with per-cut thread replies (`release_slo.cards`)? The spec defaults to per-cut.
3. **Auto-boarding in trains mode**: default off (planned means "someone planned it") as specified, or on so that every agent deliverable is visible on the next release?
4. **Start tracking from the browser**: operator-only button that writes the policy bead, or keep policy creation CLI-only and show the command?
5. **Release notes**: `kd release notes` plus a Notes link is enough, or should the cut adapter post the notes as the card's thread reply?
6. **Tenant `ticket_ids` on live/cut**: accept in beta.2 (spec), or defer and rely on the "tickets closed since previous release" fallback only?
