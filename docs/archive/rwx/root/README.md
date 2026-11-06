# RWX workflows

RWX is the heavy-lifting executor (build, deploy, e2e); GitLab CI is the
single control plane for release-time orchestration. Most gated stages on
main / tag pushes are native GitLab jobs that dispatch an RWX workflow
and block on `rwx results --wait`, so commit statuses are produced
natively by GitLab.

**Exception — R2 fire-and-forget (ka-0000005957):** some dispatch jobs do NOT
block on `rwx results --wait`. They POST a `pending` commit status and exit;
the dispatched run's terminal `report-status` task POSTs the success/failed
status via `/repository/commits/:sha/statuses` (`GITLAB_CLONE_TOKEN`). The
scheduled `rwx-status-watchdog` GitLab job fails any such status left stuck
pending past a hard timeout (crashed-run safety net).

**Ephemeral lifecycle (2026-07-09):** automatic per-MR/per-main-push staging
environments are GONE. `rwx-main` / `gardenio-main` (`.rwx/main.yml`) was
removed after the reaper deadlock incident: every merged MR spawned a
~27-pod `gardenio-rwx-<sha>` namespace with the rwx-reaper CronJob as the only
cleanup; when the reaper wedged, 94 environments packed prod-gardenio-eks and
stalled production agent spawns. Ephemerals are now created ONLY by
`rwx-release`, `rwx-scheduled`, or manual dispatch — and each DAG deletes its
own namespace via an embedded `teardown-ephemeral` task (green path; failed
runs are left 45m for triage, then reaped).

## Dispatch chain (tag + scheduled)

```diagram
GitLab CI (.gitlab-ci.yml)
  ├─ (main push: NO staging deploy — ci.yml tests + docker.yml image builds only)
  ├─ rwx-scheduled    →  dispatches gardenio-scheduled   (.rwx/scheduled.yml)   [scheduled]
  │                      └─ ONE embedded-run DAG: deploy-rwx (ephemeral) →
  │                         { deploy-rwx (singleton) ∥ e2e (full suite) } →
  │                         teardown-rwx (ephemeral, green path)
  └─ (tag pipelines)
     ├─ rwx-release             →  dispatches gardenio-release     (.rwx/release.yml)
     │                             └─ ONE embedded-run DAG: deploy-rwx (ephemeral) → e2e (full)
     │                                → teardown-rwx (ephemeral, green path)
     │                                (reporter applies MAX_MODULE_FAILURES stopgap)
     │  (no tag-pipeline singleton deploy: rwx-deploy-rwx-singleton is
     │   manual-only — GARDENIO_DEPLOY=1, ka-0000005327 — and since the
     │   rwx-main removal it is the ONLY singleton refresh path besides the
     │   scheduled DAG's deploy-singleton task.)
     ├─ promote                 →  dispatches gardenio-promote     (.rwx/promote.yml)  (needs: rwx-release)
     └─ rwx-deploy-prod          →  dispatches gardenio-deploy --target=prod  (.rwx/deploy.yml)  (needs: promote)

GitLab push / tag-push (event-triggered, image builds only)
  └─ docker.yml  →  build + push images  →  promote-images on tag

```

Image builds for `main` and `<calver>` tags are event-triggered on the
GitLab side (`docker.yml`'s `on.gitlab.push` / `on.gitlab.tag-push`).
docker.yml runs in parallel with the GitLab CI dispatch chain — it
produces image artifacts only. Both triggers set `status-checks: false`
so RWX posts **no** commit status for those refs (RWX posts a *default*
status check unless explicitly disabled — omitting it lets a transient
image-push flake falsely red the whole main/tag pipeline, ka-0000006462).
It therefore cannot gate the GitLab pipeline. The dispatch chain above
remains the single source of truth for gated stages (deploy-rwx →
smoke/e2e → deploy-prod).

## Files

| File | Purpose | Triggered by |
|---|---|---|
| `docker.yml` | Build + push all images, promote `-rc` → calver on tag | GitLab push (main) / tag-push, PR/MR fanout |
| `microvm-image.yml` | Lambda MicroVM ARM64 agent image (`microvm-agent` + `microvm-agent-context`) — native arm64 DAG, Docker build + smoke test + push; sources in `gardenio/images/microvm-agent/` (ka-0000001041) | GitLab push (main) / tag-push, PR/MR status check `MicroVM Image` |
| `scheduled.yml`  | Scheduled DAG — embeds `deploy-rwx.yml` (ephemeral + singleton) + `e2e.yml` (full suite) + `teardown-rwx.yml` (ephemeral, green path) as one `after:`-ordered run (P2 collapse, ka-0000007436) | GitLab CI dispatch (`gardenio-scheduled`) on scheduled pipeline |
| `release.yml`    | Tag/release staging+test DAG — embeds `deploy-rwx.yml` (ephemeral) + `e2e.yml` (full suite) + `teardown-rwx.yml` (ephemeral, green path) as one `after:`-ordered run (P3 collapse, ka-0000007165). Promote/prod stay separate jobs so the reporter's MAX_MODULE_FAILURES stopgap still gates them. | GitLab CI dispatch (`gardenio-release`) on tag push |
| `deploy-rwx.yml` | Deploy a known image tag to gardenio-rwx (staging). Singleton deploys serialize via the `deploy-rwx-singleton` concurrency pool (target=singleton only). | GitLab CI dispatch (`gardenio-deploy-rwx`), embedded by `scheduled.yml`/`release.yml` |
| `teardown-rwx.yml` | Delete one `gardenio-rwx-<instance-id>` ephemeral (helm uninstall + ns delete; honors `lifecycle=keep`). Embedded by `scheduled.yml`/`release.yml` on the green path; the rwx-reaper CronJob (ttl 45m / cap 10) is the backstop for failed/ad-hoc namespaces. | GitLab CI dispatch (`gardenio-teardown-rwx`, manual MR/main button), embedded, or CLI |
| `deploy.yml`     | Deploy to gardenio-rwx and/or gardenio (prod) via Helm | GitLab CI dispatch (`gardenio-deploy`, `target=prod` for prod) |
<!-- dashboards-sync.yml removed (ka-0000002799): dashboards no longer ship as
     chart-rendered grafana_dashboard ConfigMaps (that double-stored ~200KB per
     revision in the release Secret and overflowed the 1 MiB limit, ka-0000003724).
     They now ride in the grafana-bridge image and the bridge's SSE/catch-up sync
     live-provisions them within ~30s of any dashboard-bead edit — which closes the
     ka-0000003544 accuracy gap the removed workflow used to. See
     gardenio/docs/dashboard-sync.md. -->
| `e2e.yml`        | Full E2E module suite against gardenio-rwx | GitLab CI dispatch (`gardenio-e2e`) on tag / scheduled |
| `smoke-rwx.yml`  | 5-module SMOKE_ONLY subset (ka-0000004434) | Manual dispatch (`gardenio-smoke-rwx`) — no longer auto-dispatched since the 2026-07-09 rwx-main removal |
| `migration-acceptance.yml` | P4.4 cutover-acceptance gate — READ-ONLY `MIGRATION_ACCEPTANCE` subset (post-auth viewer 200, coopmux reachable, opt-in agent-opens-MR) against the fresh prod cluster (epic ka-0000003481) | **Manual** dispatch (`gardenio-migration-acceptance`) during a cutover window |
| `helm.yml`       | Lint + package + push Helm chart on tag | GitHub push (tag), PR/MR lint |
| `bundle.yml`     | `build-bundle`: the Gardenio-in-a-Box air-gap image bundle (one OCI-layout tarball of all runtime images) from the promoted `<calver>` tags. `build-tarboat`: the single signed Tarboat tarball `gardenio-tarboat-<calver>.tar.zst` (reuses the image-archive + adds bin/ manifests/ chart/ repo tree/ LICENSES + MANIFEST/SHA256SUMS — Tarboat T1/G1.3, ka-0000000192) | GitLab CI dispatch (`gardenio-bundle`), **manual** on tag (P4.1, ka-0000003363) |
| `ci.yml`         | Per-component build / lint / test fanout | GitHub / GitLab MR + push |

The dispatch-only files (`deploy-rwx.yml`, `deploy.yml`, `e2e.yml`,
`smoke-rwx.yml`) do **not** post custom GitLab commit statuses — the
dispatching GitLab job's exit code is the commit status.

## Published-package pinning & trust (ka-0000005520)

Every third-party building block these workflows `call:` is an **RWX
published package** — `git/clone`, `golang/install`, `nodejs/install`,
`rwx/install-captain`. The supply-chain posture for them is:

- **Exact-version pinning is the only pin RWX offers.** A package `call:`
  takes `<org>/<name> <exact-version>` — no ranges, no semver notation, and
  **no digest/SHA reference** (`rwx packages update`, [call-packages
  docs](https://www.rwx.com/docs/call-packages)). So the fully-qualified
  version string *is* the pin; there is no `@sha256:…` form to add. RWX
  enforces semver server-side: a package may not add a required parameter
  or remove a parameter without a major-version bump, so a same-major pin is
  parameter-stable by construction.
- **Trust root = the RWX package registry.** Today "only RWX can develop
  published packages" — there are no third-party publishers, so each package
  above resolves to an RWX-authored, RWX-hosted definition. We inherit RWX's
  integrity guarantees for package resolution; there is no per-call
  content-address we can additionally verify. (Base *images* — `image:
  ubuntu:24.04` in each `base:` block — are a separate axis and can be
  digest-resolved with `rwx resolve`; that is out of scope for this note.)
- **Convergence policy: one version per package, fleet-wide.** Keep a single
  pinned version of each package across all `.rwx/*.yml`. Version drift (e.g.
  `golang/install` on both `1.2.0` and `1.2.1`) defeats reproducibility and
  cache reuse for no benefit. Bump in lockstep with `rwx packages update`
  (add `--allow-major-version-change` for a major bump), then re-check for
  drift with:

  ```sh
  grep -rhoE 'call:\s+[a-z0-9-]+/[a-z0-9-]+ [0-9.]+' .rwx/*.yml | sort | uniq -c
  ```

  Each package name should appear on exactly one version line.

> Note: most workflows clone via a hand-rolled retry-wrapped `git clone`
> shell task rather than `call: git/clone` (the stock package has no fetch
> retry — ka-0000001787); `e2e.yml` is the one remaining `call: git/clone`
> site. Those shell clones are not published-package calls and are not
> subject to the pinning above.

## Where to look when stuck

- **GitLab pipeline failed?** Open the failed job in GitLab CI. The job
  log prints the RWX `RunURL` early, before `rwx results --wait` blocks.
  Click through to the RWX run for full task logs.
- **`rwx-deploy-rwx` failed at image-tag preflight?** `verify-images`
  polls the registry for up to 15 minutes (every 30 s) waiting for
  `docker.yml` (event-triggered on the GitHub mirror, runs in parallel
  with the GitLab pipeline) to push the controller image for the
  commit. If it still fails after that, the docker.yml run probably
  errored — open the matching docker.yml run from the RWX dashboard
  and check its `push-*` tasks. Override the wait window via
  `WAIT_TIMEOUT_SEC` / `WAIT_INTERVAL_SEC` env on `verify-images` if
  needed.
- **`rwx-e2e` failed?** The job auto-files a bug bead on tag failure and
  attaches a JUnit report + dotenv (`PASSED_MODULES`, `FAILED_MODULES`,
  `RWX_RUN_URL`) for downstream Slack/release-body jobs.
- **Need to run a workflow manually?** Each YAML's leading comment block
  has a `rwx run` invocation example.

## Manual dispatch (CLI)

```sh
# Deploy a specific commit to gardenio-rwx
rwx run .rwx/deploy-rwx.yml \
  --init commit-sha=$(git rev-parse HEAD) \
  --init ref-name=refs/heads/main --wait

# Run E2E against gardenio-rwx
rwx run .rwx/e2e.yml --init commit-sha=$(git rev-parse HEAD) --wait

# Run the 5-module smoke subset
rwx run .rwx/smoke-rwx.yml \
  --init commit-sha=$(git rev-parse HEAD) \
  --init ref-name=refs/heads/main --wait

# Run the P4.4 migration-acceptance gate against the fresh prod cluster
# (READ-ONLY; eks-cluster is REQUIRED — no default target). Add
# --param agent-mr=1 to also run the slow real-agent MR-loop check.
rwx dispatch gardenio-migration-acceptance \
  --param eks-cluster=prod-gardenio-eks \
  --param namespace=gardenio \
  --param host-domain=prod.gardenio.acme.example \
  --param commit-sha=$(git rev-parse HEAD) \
  --param ref-name=refs/heads/main

# Tear down one ephemeral namespace (fire-and-forget delete; reaper backstops)
rwx run .rwx/teardown-rwx.yml \
  --init instance-id=<short-sha-or-slug> \
  --init host-domain=prod.gardenio.acme.example --wait

# One-off: re-register the in-account MicroVM image with run+resume hooks ENABLED
# (ka-0000004153). Dry-run first, then apply. Break-glass creds from gardenio-main.
rwx dispatch gardenio-fix-microvm-run-hook-enabled --ref <branch> \
  --param dry-run=--dry-run --wait
rwx dispatch gardenio-fix-microvm-run-hook-enabled --ref <branch> --wait
```

NOTE: any manually-dispatched ephemeral (deploy-rwx/e2e/smoke with an
instance-id) is YOURS to tear down — use the teardown above, or let the
rwx-reaper CronJob sweep it at ttlMinutes=45. There is no automatic
per-MR/per-push environment anymore (2026-07-09).

See `docs/rwx-agent-guide.md` for `rwx dispatch` / `rwx results` / `rwx logs`
gotchas (CLI v3.13+ JSON keys, dispatch progress filtering, etc.).
