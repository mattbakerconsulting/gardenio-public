# Phase R — CI/CD-minutes metrics: data sources, schema & design

**Epic:** ka-0000005891 · **Task:** ka-0000003156 · **Status:** research complete,
awaiting operator sign-off (decision bead gates Phases G + X).

This doc pins down the *exact* data sources, API shapes, metric schema,
cardinality bounds, collector placement, auth path, dashboard, and backfill
scope for getting **CI/CD compute minutes** (GitLab CI + RWX) into gardenio's
VictoriaMetrics + Grafana stack. Every API claim below was verified live from
this agent pod on 2026-07-20 against the real tokens the fleet already carries.

The pattern to copy is the **jira-metrics bridge**
(`controller/internal/bridge/jira_metrics.go` + `dashboards/jira-metrics.json`)
and the even-closer **R5 trust-metrics collector**
(`controller/internal/trustmetrics/`, wired in `cmd/gitlab-bridge/main.go`):
a periodic goroutine polls an external API, `Set()`s `promauto` gauges on the
default registry, and vmagent scrapes the bridge's internal `/metrics`.

---

## TL;DR / recommendations (what the decision bead approves)

1. **RWX authoritative source** = the RWX REST API `GET https://cloud.rwx.com/mint/api/runs`
   (Bearer `RWX_ACCESS_TOKEN`). It returns cursor-paginated recent runs, each
   with `completed_runtime_seconds` (≈ billed execution time), `definition_path`,
   `branch`, `trigger`, `result_status`. **The CLI cannot list runs** — `rwx runs
   list` does not exist in *any* installed version (it is an alias for `rwx
   results <run-id>`, which is single-run and returns only status). Poll the REST
   API directly.
2. **GitLab authoritative source** = sum of per-**job** `duration` (+ track
   `queued_duration`) from `GET /projects/:id/pipelines/:pid/jobs`. This instance
   is **gitlab.com SaaS** on shared `saas-linux-small-amd64` runners, so job
   duration *is* billable compute. There is **no first-party CI-minutes/quota
   number reachable with our token** (the usage surfaces are admin/owner-scoped
   or 404 for this PAT) — reconstruct from job durations.
3. **Both collectors live in the existing `gitlab-bridge` binary.** Its egress
   NetworkPolicy already allows `0.0.0.0/0:443`, so it reaches **both**
   `gitlab.com` and `cloud.rwx.com` with **zero** NetworkPolicy change, and
   vmagent **already scrapes its `:9092/metrics`**. No new pod, no new scrape
   target, no new egress rule for the common case.
4. **Auth:** GitLab uses the token gitlab-bridge already holds
   (`GITLAB_API_TOKEN`). RWX needs `RWX_ACCESS_TOKEN` mounted onto the
   gitlab-bridge pod from the existing `gardenio-rwx-access-token` K8s Secret
   (already present in-cluster; the agent pods and controller consume it). This
   is the one small chart change Phase X needs.
5. **Metrics** are **counters** for cumulative minutes
   (`gardenio_cicd_minutes_total{provider,…}`) plus scrape-health gauges/counters,
   mirroring jira_metrics.go. Cardinality is bounded by design: label by
   `provider`, `project`, a **bounded** work-unit key (`definition`/`job_name`),
   `status`, `runner_type` — **never** by raw pipeline id / RWX run id.
6. **Backfill:** forward-only. Both APIs page backwards, but metrics are a
   forward-looking rate signal; a bounded one-time backfill is a Phase X stretch,
   not a requirement.

---

## 1. Data sources — what we can actually reach, and the timing they give

### 1a. RWX

**Installed CLI:** `rwx version v3.13.0` (pod verified). `RWX_ACCESS_TOKEN` is
present in agent pods and is an **organization access token**
(`rwx whoami` → `{"organization_slug":"acme","token_kind":"organization_access_token"}`).

**The CLI cannot enumerate runs.** The task brief guessed a CLI bump would add
`rwx runs list`; it will not. `rwx runs` is a **hard alias for `rwx results`**
(verified: `rwx runs list` → `Error: unable to get run status … 404 Not Found`,
because it parses `list` as a run-id). `rwx results <run-id> [--task <key>]`
returns only run/task *status* — not timing:

```
$ rwx --output json results b038e48b4f9a4818af09c51c4cb2c1ac
{"RunID":"b038…","ResultStatus":"succeeded","Completed":true}
```

So the CLI is a dead end for a minutes collector. **Use the REST API the CLI
itself calls** (base `https://cloud.rwx.com/mint/api`, extracted from the CLI
binary and confirmed live).

**Authoritative endpoint — `GET /mint/api/runs`** (Bearer token). Returns a
cursor-paginated list of recent runs, newest first. Verified live shape:

```jsonc
{
  "runs": [
    {
      "id": "b038e48b4f9a4818af09c51c4cb2c1ac",
      "status": { "result": "succeeded", "execution": "finished", ... },
      "started_at":   "2026-07-20T19:20:03.708Z",
      "completed_at": "2026-07-20T19:20:43.890Z",
      "completed_runtime_seconds": 28,          // ← RWX "minutes" raw material
      "repository_name": "gardenio",
      "branch": "fix/ka-0000006601",
      "tag": null,
      "commit_sha": "ff8bb528…",
      "definition_path": ".rwx/ci.yml",          // ← bounded work-unit key
      "trigger": "gitlab.merge-request",          // cli|dispatch|gitlab.push|gitlab.merge-request
      "title": "chore: retrigger CI …",
      "result_status": "succeeded",               // succeeded|failed|no_result
      "execution_status": "in_progress|finished",
      "run_url": "https://cloud.rwx.com/mint/acme/runs/b038…",
      "created_at": "2026-07-20T19:20:21.273Z"
    }
  ],
  "pagination": { "next_cursor": "<opaque base64>", "limit": 50 }
}
```

**What `completed_runtime_seconds` means (measured, not assumed).** Across 40
finished runs it tracked wall-clock (`completed_at − started_at`) at **ratio
0.85–0.99** — i.e. it is a **run-level billed-execution figure that excludes
queue/wait time**, not a sum of parallel per-task compute. Two consequences:
- It is the right, first-party "RWX minutes" number — use it directly, do not
  reconstruct from tasks.
- It is **not** parallelism-weighted. A run that fans out 8 parallel tasks for
  60s bills ~60 runtime-seconds here, not 480. If per-task compute-seconds are
  ever needed, that is a separate, heavier drill-down (see "task detail" below);
  for a spend/usage dashboard the run-level figure is what RWX itself bills on.

**Pagination — cursor, not page number.** `?page=2` is **ignored** (returns the
same first page). The real mechanism is `?cursor=<next_cursor>&limit=N`
(verified: passing `next_cursor` walks to the next 50, oldest `created_at`
strictly decreasing). Default/`max` page size observed = 50; `?limit=5` honored.

**Server-side filters — only two work; the rest are silently ignored.**
Verified by checking the *distinct values actually returned*:

| Param | Effect | Verdict |
|---|---|---|
| `?branch=main` | filters (only `main` returned) | ✅ works |
| `?definition_path=.rwx/ci.yml` | filters (only that def) | ✅ works |
| `?limit=N`, `?cursor=…` | pagination | ✅ works |
| `?trigger=…`, `?status=…` | returned mixed values | ❌ ignored |
| `?created_after=…`, `?since=…`, `?after=…` | future date still returned 50 | ❌ ignored |

**⇒ The collector must window client-side.** Page from newest until
`created_at` falls before `now − window`, dedup by run `id`, and only count runs
whose `execution_status == "finished"` (skip `in_progress`; its
`completed_runtime_seconds` is a partial tick — 30 in the sample). This is the
single most important RWX implementation constraint.

**Per-task timing (drill-down, optional).** `rwx results <id> --task <key>`
exists and `/mint/api/results/status?id=<id>` returns run+polling status, but
neither cheaply yields per-task execution seconds in a documented, stable shape.
The MCP endpoint `/mint/api/mcp/get_run_test_failures` and artifact/log endpoints
exist but are failure-triage surfaces, not billing. **Recommendation:** ship the
run-level `completed_runtime_seconds` metric first; treat per-task breakdown as a
future enhancement only if the dashboard demands it.

**No org "daily usage"/billing API is reachable with this token.** The CLI
exposes `vaults`, `runs/results`, `artifacts`, `logs`, `image`, `dispatch` — no
usage/billing verb. The RWX org "Daily usage" email is a notification, not an
API. `completed_runtime_seconds` summed over `/runs` is our usage number.

**Polling model (RWX):** every `RWX_METRICS_INTERVAL` (default 120s), page
`/runs?limit=50` from newest, dedup by id against a rolling seen-set, window to
`created_at ≥ now − lookback` (default 60m — comfortably longer than one
interval so nothing is missed across a restart), and for each newly-*finished*
run add its `completed_runtime_seconds` to the `provider="rwx"` counter series
keyed by `definition_path` + `result_status` + `trigger`.

### 1b. GitLab

**Instance:** `gitlab.com` SaaS (verified: `glab auth status` → gitlab.com; jobs
run on `saas-linux-small-amd64.runners-manager.gitlab.com`,
`runner_type=instance_type`). Shared-runner seconds are billable compute, so job
duration is a true minutes signal.

**Token:** the gitlab-bridge already holds a Group Access Token
(`GITLAB_API_TOKEN`, scopes include `api`/`read_api`) — the same client used by
`GetPipeline`/`ListPipelineJobs`. No new token.

**Per-job duration — the authoritative source (verified live).** Both pipeline
and job objects carry the timing fields:

```
GET /projects/:id/pipelines/:pid           → { duration, queued_duration, status, ref, source, … }
GET /projects/:id/pipelines/:pid/jobs       → [ { name, stage, status, duration, queued_duration,
                                                  runner:{description,runner_type}, started_at,
                                                  finished_at, … } ]
GET /projects/:id/jobs?scope=success        → same job shape, project-wide (no pipeline hop)
```

Verified: for a recent gardenio pipeline every one of 20 success jobs had a
non-null `duration` (e.g. `coverage` 33.1s / queued 0.44s) and a resolved
`runner`. **Sum job `duration` per pipeline** for pipeline compute-minutes; keep
`queued_duration` as a separate "wait" series.

**⚠️ Existing struct gap (Phase G work item).** `bridge.GitLabPipeline` and
`bridge.GitLabJob` (`controller/internal/bridge/gitlab.go`) do **not** currently
unmarshal `duration`/`queued_duration` — the fields exist on the wire but are
dropped. Phase G must add:
```go
// GitLabPipeline
Duration        *float64 `json:"duration"`
QueuedDuration  *float64 `json:"queued_duration"`
FinishedAt      *string  `json:"finished_at"`
// GitLabJob
Duration        *float64        `json:"duration"`
QueuedDuration  *float64        `json:"queued_duration"`
StartedAt       *string         `json:"started_at"`
Runner          *GitLabRunner   `json:"runner"`   // {description, runner_type}
```
(Pointers because `duration` is `null` for created/pending jobs.) These are
additive, back-compatible field adds.

**First-party CI-minutes / quota — NOT reachable with our token (verified).**
- `GET /projects/:id?statistics=true` → returns storage sizes
  (`job_artifacts_size`, `packages_size`, …) but **no compute-minutes** field.
- `GET /groups/:id` → exposes only `shared_runners_minutes_limit` /
  `extra_shared_runners_minutes_limit` (the *quota ceiling*, not usage).
- `GET /namespaces/:id` → membership/seats, no minutes.
- `GET /usage_data`, `/groups/:id/usage_export` → **404** for this token
  (admin/owner-scoped).

**⇒ Reconstruct minutes by summing job durations.** There is no first-party
usage number to prefer, so the "prefer first-party else sum" decision resolves
to *sum job durations*.

**Which pipelines to poll.** The bridge already knows the group
(`GITLAB_GROUP_ID`) and lists pipelines via `ListPipelines(projectPath, ref,
status, updatedAfter)`. Poll model: every `CICD_METRICS_INTERVAL` (default 120s),
`ListPipelines(updatedAfter = now − window)` for the tracked project(s), and for
each **newly-finished** pipeline not yet counted (dedup by pipeline id in a
rolling seen-set), fetch its jobs once and add `Σ job.duration` to the
`provider="gitlab"` counter, plus per-job `duration`/`queued_duration`
observations. `updated_after` **is** honored by the GitLab pipelines endpoint
(unlike RWX), so the GitLab window query is server-side-cheap.

---

## 2. Metric schema

New file **`gardenio/docs/metric-schema.md`** (currently absent — this is its
first section; the broader inventory lives in
`docs/research/timeseries-metrics-catalog.md`). Proposed families, all prefixed
`gardenio_cicd_`:

### Compute-minutes (the headline)

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `gardenio_cicd_minutes_total` | **counter** | `provider, project, definition, status` | Cumulative **compute minutes** billed. GitLab: `Σ job.duration/60` per counted pipeline. RWX: `completed_runtime_seconds/60` per counted run. Rate-able for minutes/day. |
| `gardenio_cicd_runs_total` | **counter** | `provider, project, definition, status` | Count of counted pipelines/runs — denominator for "avg minutes per run" and failure-rate. |
| `gardenio_cicd_queued_seconds_total` | **counter** | `provider, project, definition` | Cumulative queue/wait seconds (GitLab `queued_duration`; RWX derives `started−created`). "Wait vs run" split. |

`status` ∈ `{success, failed, canceled, skipped}` (GitLab) / `{succeeded,
failed, no_result}` (RWX) — **bounded** (≤6 values). Failure-weighted "wasted
CI" = `sum(rate(gardenio_cicd_minutes_total{status="failed"}[$__rate_interval]))`.

**Why counters, not gauges.** The timeseries-metrics-catalog explicitly flags
"gauges with `_total` suffix that should be counters" as a quality bug to fix.
CI minutes are a monotonically-accumulating event total, so a real
`promauto.NewCounterVec` incremented once per newly-counted pipeline/run is
correct and rate()-able. (Contrast jira_metrics.go, whose *point-in-time issue
count* is legitimately a gauge.) The collector's dedup seen-set is what keeps
increments idempotent across polls/restarts.

### Per-work-unit distribution (optional, bounded)

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `gardenio_cicd_job_duration_seconds` | **histogram** | `provider, project, job_name, runner_type` | Per-job (GitLab) duration distribution → p50/p95 per job. **Guard cardinality**: only when the distinct `job_name` set is known-bounded (gardenio CI has ~dozens of stable stage/job names). If a project has unbounded generated job names, omit this family and rely on the counters. |

RWX has no cheap per-task duration (see §1a), so this histogram is
**GitLab-only** at launch.

### Scrape-health (per collector — copied from jira_metrics.go)

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `gardenio_cicd_scrape_duration_seconds` | gauge | `provider` | Wall-clock of the most recent poll cycle per provider. |
| `gardenio_cicd_scrape_errors_total` | counter | `provider` | Failed API calls (HTTP/transport/ctx), so a stalled collector is visible while its last-good counters linger. |
| `gardenio_cicd_last_success_timestamp_seconds` | gauge | `provider` | Unix time of last fully-successful cycle — drives a "collector stale" alert. |

### Cardinality discipline (hard rules — mirror the operator-label PII allow-list)

- **NEVER label by raw pipeline id or RWX run id** — unbounded, and blows up
  vmsingle's 1y retention. The dedup seen-set holds ids in memory only; they
  never become label values.
- `project` — bounded (gardenio today; a small enumerable set).
- `definition` — GitLab: the pipeline `source`/`ref`-class or a coarse pipeline
  name; RWX: `definition_path` (5 distinct observed: `ci.yml`, `docker.yml`,
  `coverage.yml`, `helm.yml`, `microvm-image.yml`). Bounded.
- `job_name` — bounded only within the histogram family and only for projects
  with stable job names; drop the family otherwise.
- `runner_type` — GitLab `instance_type`/`group_type`/`project_type` (≤3).
- Do **not** add `branch`, `commit_sha`, `title`, `trigger` as labels on the
  counters (branch and title are effectively unbounded). `trigger` *may* be an
  extra bounded label if per-trigger spend proves interesting (4 distinct
  values), but launch without it and add only if a panel needs it.
- `env`/`cluster` are stamped by vmagent `external_labels` at scrape — do **not**
  emit them from the collector (avoids the cross-env aliasing bug the catalog
  calls out).

---

## 3. Where the collectors live

**Recommendation: both collectors as goroutines inside the existing
`gitlab-bridge` binary** (`controller/cmd/gitlab-bridge/`), each a
`New*MetricsCollector(...)` mirroring `NewJiraMetricsCollector` /
`trustmetrics.NewPeriodicCollector`. Rationale:

1. **Egress already permits both hosts.** The `gitlab-bridge-egress`
   NetworkPolicy allows `to: 0.0.0.0/0 port 443` (excluding RFC1918/IMDS) — so
   the pod already reaches `gitlab.com` **and** `cloud.rwx.com`. A separate RWX
   sidecar would need its own egress policy re-derived; putting RWX here needs
   **zero** NetworkPolicy change.
2. **Scrape is already wired.** gitlab-bridge's pod annotations are
   `prometheus.io/scrape:"true"`, `port:"9092"`, `path:"/metrics"`, and
   `buildMetricsServer(:9092)` already serves the default gatherer that
   `promauto` registers into. New `gardenio_cicd_*` series appear on the **same**
   `/metrics` with no new target, no ServiceMonitor, no vmagent config edit.
   (`:9092` is in the vmagent-egress allow-list `9091/9092/9093/3000/8429`.)
3. **The GitLab client is already here.** The GitLab collector reuses the same
   `*GitLabClient` (`ListPipelines`, `ListPipelineJobs`) — literally the closest
   possible code reuse, exactly like the R5 trust-metrics collector already does
   in this binary.
4. **Opt-in, dark-launch-safe.** Gate each collector on an env flag
   (`CICD_GITLAB_METRICS_ENABLED` / `CICD_RWX_METRICS_ENABLED`, default off),
   exactly like `TRUST_METRICS_ENABLED` / the JQL collector. The binary is inert
   until the operator flips the flag(s) — so Phases G and X can merge dark and
   turn on independently.

**Why not a new binary/sidecar for RWX?** Only justified if RWX polling needed a
different egress posture or credentials the bridge shouldn't hold. Neither is
true: the egress rule is host-blind (any :443) and the RWX token is a read-only
org token mountable as one env var. A new deployment adds a pod, a Service, an
ingress-less egress policy, a scrape annotation, and release surface for zero
benefit. **Keep it in gitlab-bridge.**

**vmagent wiring — confirmed, no change needed.** The `kubernetes-pods` scrape
job discovers any pod with `prometheus.io/scrape=true` in the release namespace
and scrapes its annotated port/path. gitlab-bridge already qualifies. The
PII-allow-list `metric_relabel_configs` `labeldrop` list
(`oidc_email|slack_user_id|display|…`) does not touch any `gardenio_cicd_*` label,
so nothing is stripped.

---

## 4. Auth + config path

| Provider | Credential | Where today | Phase change |
|---|---|---|---|
| GitLab | `GITLAB_API_TOKEN` (Group Access Token, `read_api`) | Already env on gitlab-bridge (from `gitlabBridge.gitlab.secretName`) | **None** — reuse. |
| RWX | `RWX_ACCESS_TOKEN` (org access token, read-only) | K8s Secret **`gardenio-rwx-access-token`** exists in-cluster (agent pods + controller consume it; provisioned via RWX vault → external-secret, see bead ka-0000004592) | **Small chart add**: mount it as env on the gitlab-bridge Deployment via a new `gitlabBridge.rwx.secretName`/`secretKey` block (mirror the `gitlabBridge.gitlab.secretName → GITLAB_API_TOKEN` pattern). |

The RWX secret already exists — Phase X does **not** need a new secret minted;
it only references the existing one on a new pod. If, on the target cluster, the
secret is not yet synced into the release namespace, that (and only that) is an
operator escalation — noted for Phase X, not Phase R.

Config knobs (all env, defaulting to inert; mirror jira-bridge's
`JIRA_METRICS_*`):
```text
CICD_GITLAB_METRICS_ENABLED   (default false)
CICD_RWX_METRICS_ENABLED      (default false)
CICD_METRICS_INTERVAL         (default 120s)
CICD_METRICS_WINDOW           (default 60m — lookback per cycle; > interval)
CICD_METRICS_PROJECTS         (GitLab project paths; default = the bridge's group)
RWX_API_BASE_URL              (default https://cloud.rwx.com/mint/api)
```

---

## 5. Dashboard plan

One dashboard **`dashboards/cicd-minutes.json`** (uid `cicd-minutes`, tags
`[gardenio, cicd, metrics, minutes]`), added to
`templates/controller/grafana-dashboard-configmap.yaml` as one more
`gardenio-cicd-minutes.json: {{ .Files.Get "dashboards/cicd-minutes.json" }}`
entry (Grafana sidecar live-reloads on the `grafana_dashboard:"1"` label). A
`$provider` template var (`gitlab|rwx|All`) and `$project` var, matching
jira-metrics.json's templating style.

Panels (GitLab + RWX unified):
1. **Minutes/day by provider** — `sum by (provider) (increase(gardenio_cicd_minutes_total[1d]))` (bar/timeseries).
2. **Top pipelines/definitions by minutes** — `topk(10, sum by (definition,provider) (increase(gardenio_cicd_minutes_total[$__range])))` (table).
3. **Queued vs run time** — two series: `rate(gardenio_cicd_queued_seconds_total[$__rate_interval])` vs `rate(gardenio_cicd_minutes_total[$__rate_interval])*60`.
4. **Failure-rate-weighted minutes (wasted CI)** — `sum by (provider)(increase(gardenio_cicd_minutes_total{status=~"failed|canceled"}[$__range]))` and the % of total.
5. **Runs/day + avg minutes-per-run** — `increase(gardenio_cicd_runs_total)` and `minutes_total / runs_total`.
6. **Per-job p50/p95 (GitLab)** — `histogram_quantile(0.95, sum by (le,job_name)(rate(gardenio_cicd_job_duration_seconds_bucket[$__rate_interval])))`.
7. **Collector health** — `gardenio_cicd_last_success_timestamp_seconds` freshness + `rate(gardenio_cicd_scrape_errors_total[5m])` (stat panels, red when stale).

A `dashboards-sync.yml` RWX flow (`gardenio-dashboards-sync`) can push the JSON to
live Grafana without a full release once merged.

---

## 6. Backfill & alerting (scope decisions)

**Backfill — forward-only (recommended).** Both `/mint/api/runs` (cursor) and
GitLab pipelines (`updated_after`) page backwards, so a bounded historical
backfill is *possible*, but:
- Counters are a forward rate signal; a one-time backfill produces a single
  retroactive jump, not a smooth history, and risks double-counting on restart.
- vmsingle retains 1y going forward from launch, which satisfies the "spend/usage
  trends" goal within weeks.
**⇒ Launch forward-only.** A bounded (e.g. last-14-day) one-shot backfill is a
Phase X *stretch* only if the operator wants immediate history; if so, it must
seed the same dedup seen-set to stay idempotent.

**Alerting — stretch, via the existing path.** A budget/threshold alert fits the
deployed `vmalert → Alertmanager → alertmanager-bridge → beads` chain (same
shape as `release-drought-alert.yaml` and the Karpenter GAP-2 alert). Example
rule: `sum(increase(gardenio_cicd_minutes_total[1d])) > $BUDGET` for 1h → P2 bug
bead + Slack. Defer to a follow-up bead; not required for the dashboard to land.

---

## Exit criteria & what the decision bead unblocks

- ✅ Design doc landed (this file).
- A `type=decision` bead is filed for the operator to approve this design and
  unblock **Phase G** (ka-0000002378) and **Phase X** (ka-0000008168), which the
  epic depends on. The three real options the operator chooses among:
  1. **Approve as-is** — both collectors in gitlab-bridge, counters + GitLab-only
     histogram, forward-only, alerting deferred. Phases G & X proceed.
  2. **Approve with per-task RWX drill-down** — additionally invest in RWX
     per-task compute-seconds (heavier; needs a results-detail spike). Larger
     Phase X.
  3. **Split collectors** — put the RWX collector in a new sidecar instead of
     gitlab-bridge (only if the operator wants credential/egress isolation;
     costs a new deployment + egress policy + scrape target).

**Recommendation: option 1.**

### CI-burn note (advice ka-0000001560)

This is a docs-only change under `gardenio/docs/research/` — the repo already
holds 144 such research docs, and the coverage/heavy RWX CI jobs are
`changes:`-gated to code paths (`gardenio/controller/**`, `kbeads/**`, `coop/**`,
`.rwx/**`, `go.work`), so a docs-only MR does **not** dispatch a coverage/E2E
run. The one required check is `lint-mr-proof` (a PROOF block in the MR
description). Committing the doc is consistent with repo convention and does not
burn heavy CI minutes.
