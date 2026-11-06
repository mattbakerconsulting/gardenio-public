# Live-namespace e2e lane on the k3s ARC runners — design

Task: gd-WBz2qt9ADZ. Census: gd-xad8rBcLmH. Predecessor plan: gd-ZYb9OTNwfe
(decision O4, 2026-09-22: "proceed, census first, dedicated in-cluster role,
not the CI runners' credentials").

## 1. What exists and what is missing

`.github/workflows/e2e.yml` already has six suites (wiring, coop, viewer,
shell-hermetic, go-e2e, go-integration), three doors (PR label `e2e`,
`workflow_dispatch`, nightly cron 09:17 UTC) and a `push: agent/e2e-ci/**`
proving trigger. None of them touches a cluster. `run-shard.sh` wires 209
shell modules; 18 are `HERMETIC_MODULES` and run in CI; the other 191 need a
namespace with the chart installed and have been run by nothing since
8a3eb29e (2026-09-16). The census (gd-xad8rBcLmH) splits those 191 into 138
NAMESPACE (the lane target), 34 EXTERNAL (Slack/GitLab/Jira/LiteLLM/Grafana/
Keycloak/cert-manager/VictoriaLogs; mostly self-skip), 18 PARKED (gVisor /
microVM substrate that does not exist on k3s; all self-skip) and 1 DEAD.

## 2. Shape of the lane

Two new suite names in `select`'s list, so every existing door selects them
and a dispatch can narrow to either:

```text
live-setup ──┬── live-smoke ─────────────┐
             └── live-shard [0,1,2,3] ───┴── live-report ── live-teardown (always)
```

| job | runner | what | wall |
|---|---|---|---|
| `live-setup` | `gardenio-k3s-e2e` | acquire a pool namespace (lease), sweep stale leases, wipe leftovers, mint run secrets, `helm upgrade --install`, `wait-for-stack-ready.sh`, `seed-types-and-users.sh` | ~8 min |
| `live-smoke` | `gardenio-k3s-e2e` | `run-shard.sh` with `ONLY=stack-health,agent-crew-lifecycle,beads-crud,mail-ready,trigger-engine-smoke` (SMOKE_MODULES minus `tls-certs`, which needs cert-manager) | ~5 min |
| `live-shard` ×4 | `gardenio-k3s-e2e` | `E2E_SHARD=n TOTAL_SHARDS=4 SKIP=<HERMETIC_MODULES> run-shard.sh`; the partition is the one `shard_for()` already defines and `test-shard-isolation.sh` guards | ≤25 min each (SHARD_BUDGET_SECONDS=1500) |
| `live-report` | `gardenio-k3s-light` | merge every shard's `e2e-results.jsonl` into one pass/fail/skip table in the step summary; red if any module failed | <1 min |
| `live-teardown` | `gardenio-k3s-e2e` | `helm uninstall`, delete non-Helm leftovers (agent pods, PVCs, Jobs, run secrets), release the lease. `if: always()` | ~3 min |

`live-smoke` is the "lane first" deliverable: it proves setup, install,
port-forward, seed, one shard invocation and teardown on five cheap modules.
`live-shards` is the second PR, stacked on the first.

## 3. Runner shape and tier

A **fourth scale set, `gardenio-k3s-e2e`**, with the **medium tier's shape**
(750m / 1Gi request, 4Gi limit, no dind) and `serviceAccountName:
gardenio-e2e-runner`. `maxRunners: 6`, `minRunners: 0`.

Why a new scale set rather than reusing medium: the ServiceAccount is a
property of the scale set (`template.spec.serviceAccountName`, honoured by
the gha-runner-scale-set chart in dind and plain mode alike). Granting the
medium set a cluster credential would hand every medium job (terraform fmt,
dictation corpus, quench) namespace-admin over the pool. A dedicated set
keeps the credential on exactly the jobs that need it, which is what O4
asked for ("not on the CI runners' credentials").

Why the medium shape: the shard jobs do the same work as `shell-hermetic`
(two Go builds + shell modules), which measured 2039Mi anonmax on this shape
(gardenio/helm/ci/README.md, Task 12). Setup and teardown are kubectl/helm
only and would fit light, but a fifth scale set for two 3-minute jobs a night
is not worth the listener. Per the tier rules the tier is set by the heaviest
step, and the e2e set is sized for its heaviest job.

Slot and node math (gardenio/helm/ci/README.md "Runner tiers"): one run
peaks at 5 concurrent e2e runners (smoke + 4 shards); `maxRunners: 6` lets a
second run start its setup while the first drains. 6 × 750m = 4.5 CPU of
requests, which the README's measurement says co-schedules onto nodes already
hosting a heavy runner, so **node demand is +0 and `ci_max_nodes` is
untouched**. The heavy tier gains nothing and loses nothing: the lane claims
**zero heavy slots** (the existing four e2e jobs on heavy are unchanged).

Queue cost per nightly: ≈ 8 (setup) + 5 (smoke) + 4 × 18 (shards) + 3
(teardown) ≈ **90 e2e-slot-minutes**, plus one 1-minute light job. Nothing
is added to every-push pressure: the lane runs only on the nightly, a
dispatch, or an `e2e`-labelled PR.

## 4. Namespace model: a fixed pool, leased per run

Two operator-created namespaces, `gardenio-e2e-0` and `gardenio-e2e-1`,
labelled `gardenio.io/e2e-pool: "true"`. A run acquires one by creating a
`coordination.k8s.io/Lease` named `e2e-run-lock` in it (create fails on
conflict, so acquisition is atomic), holding `holderIdentity=<run_id>-<attempt>`
and `leaseDurationSeconds=7200`. A lease whose `acquireTime + duration` has
passed is stale (a reclaimed runner never ran its teardown) and is swept:
namespace wiped, lease deleted. If both namespaces are held, setup polls for
up to 20 minutes, then fails loudly.

Why a pool and not `kubectl create ns` per run:

- **No cluster-scoped RBAC.** Namespaces are cluster-scoped; a runner that
  can create them needs a ClusterRole, and RBAC cannot restrict `create` by
  name. With a pool, the runner holds one namespaced Role per pool namespace
  and nothing else. This is the O4 boundary.
- **Image pulls are namespaced.** ghcr packages are private; production
  namespaces receive the `ecr` pull secret from the `ecr-token-refresh`
  CronJob, which writes to a fixed list. Fixed names go on that list once.
- **Quota is pre-set by the operator**, so a runaway controller in e2e is
  bounded by a ResourceQuota the runner cannot change: the tenant Role
  enumerates core resources and holds no verb on `resourcequotas` or
  `limitranges`.
- **Pod Security `baseline` is enforced** on both pool namespaces (warn/audit
  `restricted`). With every verb on pods the runner could otherwise create a
  privileged or hostPath pod, which is root on the node. It holds no verb on
  `namespaces`, so it cannot remove the label.
- **Sweeping is namespaced.** A stale-lease sweeper needs only the two
  Roles; a create-per-run design needs `list namespaces`.

"Disposable" is preserved by wiping: `helm uninstall --wait`, then delete
pods, jobs, PVCs, configmaps, leases and every Secret except `ecr`/`ghcr`.
Both setup (before install) and teardown (after) wipe, so a namespace is
clean whichever end of the previous run died.

Release name = namespace name. `run-shard.sh` derives `BEADS_SVC`,
`${NAMESPACE}-agents`, `${NAMESPACE}-coopmux` from `NAMESPACE`, and the
chart's `gardenio.fullname` is the release name, so the two must coincide.

## 5. Chart overlay

`gardenio/tests/e2e/lane/values-e2e.yaml` (kept under tests/, not helm/, so
the overlay is owned by the lane). It renders to **namespaced core kinds
only**: Deployments, StatefulSets, Services, Jobs, PDBs, Roles/RoleBindings,
ServiceAccounts, one chart-generated Secret (postgres). No CRDs, no
cluster-scoped objects, and no ResourceQuota or LimitRange: the operator's
copies in the pool manifest are the only ones, since the runner holds no verb
on either kind (`resourceQuota.enabled=false`, `limitRange.enabled=false`).
Verified by `gardenio/helm/gardenio/tests/e2e_lane_namespace_pool_test.go`,
which also checks every rendered pod template against Pod Security
`baseline`.

On: beads + in-chart postgres + nats (the readiness gate's three critical
services), the controller (`agents.enabled`), coopmux, beads-viewer (dev auth
bypass, loopback-acked), trigger-engine. Off: TLS/cert-manager, Keycloak and
forwardauth, every bridge, landing, PSA job, platform PDBs, external secrets,
PV garbage-collector RBAC and admission policy. `global.imageRegistry` and
`imagePullSecrets: [ecr]` as in `deploy-values/gardenio-k3s.yaml`;
`global.storageClass: local-path`.

Image tag: `images.yml` publishes only on calver tags and `Chart.yaml`'s
`appVersion` is not bumped by `release-cut.sh`, so setup resolves the newest
calver tag (`git tag --sort=-creatordate | grep '^20'`) and passes
`--set global.imageTag=<tag>`, as `release-deploy.sh` does. A dispatch input
`image_tag` overrides it. Test scripts, `kd` and `gb` are HEAD; the deployed
daemon is the last release. `run-shard.sh`'s context banner already prints
this skew on every shard.

One chart change is required and is in the lane PR (chart is the operator's
lane; the PR is DRAFT + `merge-desk:hold` and the escalation names it):

1. `agents.pvGc.rbac.enabled` (default `true`) gating
   `templates/controller/pv-gc-rbac.yaml`. Today a ClusterRole +
   ClusterRoleBinding render whenever `agents.enabled`, which a namespaced
   installer cannot create.

It defaults to today's rendering byte-for-byte.

The viewer needs no chart change: `beadsViewer.devAuthBypass: true` alone is
enough. The chart's viewer pod sets `automountServiceAccountToken: false`, so
`dev_auth.go`'s SA-token prod guard never trips. An earlier draft also set
`BEADS_VIEWER_DEV_AUTH_BYPASS_ACK_AGENT_POD=1`; review gd-h3F6SjVL53 found that
`cmd/beads-viewer/main.go` forces `LISTEN_ADDR` to loopback whenever that env
is present, so the pod-IP httpGet probes fail and `helm --wait` times out. The
chart test `TestE2ELane_ViewerIsRoutableWithDevAuthBypass` pins the overlay to
the working shape.

## 6. RBAC and Secrets the runner needs (operator applies)

File: `gardenio/tests/e2e/lane/rbac/namespace-pool.yaml`.

- `ServiceAccount gardenio-e2e-runner` in `arc-runners`.
- Per pool namespace: `Role gardenio-e2e-tenant` with every verb on an
  ENUMERATED list of namespaced resources (never `*` in a resource list):
  core `pods` (+ `log`, `exec`, `attach`, `portforward`), `secrets`,
  `configmaps`, `services`, `endpoints`, `events`, `serviceaccounts`
  (+ `token`), `persistentvolumeclaims`; apps, batch, policy, autoscaling,
  coordination, networking, rbac `roles`/`rolebindings`, external-secrets.io
  `externalsecrets`, metrics.k8s.io `pods` (get/list); and a `RoleBinding` to
  the SA. `roles`/`rolebindings` is what lets `helm install` create the
  controller's Role: RBAC forbids granting what you do not hold, so the tenant
  Role must be a superset of the controller's and the adopt-PVCs Job's. The
  chart test renders the overlay and proves the superset.
- The rbac rule names its verbs (`get list watch create update patch delete`),
  never `*`: `*` includes `escalate` and `bind`, with which the runner could
  write a Role granting update/delete on resourcequotas and limitranges in its
  own namespace, bind it to itself, and lift the ceiling while every direct
  `can-i` still printed `no` (operator hold on gd-LvbhX557UD). Without them
  the API's grant-time escalation check is the fence, and the overlay must
  bind only Roles the runner fully holds (never a ClusterRole; chart test).
- Deliberately NOT granted, for any verb: `resourcequotas`, `limitranges`,
  `namespaces`, `nodes`, `persistentvolumes`, `pods/eviction`; and `escalate`
  / `bind` on anything. The chart test is the offline `kubectl auth can-i`
  (direct verbs and escalate/bind); `lane.sh assert-tenant` is the live one,
  run from the runner's token before every install, followed by a positive
  control that creates a Role granting `update resourcequotas` as the runner
  (server-side dry run) and demands Forbidden; the operator runs the `--as=`
  loop (incl. `escalate roles`, `bind roles`, `bind clusterroles`) and the
  same positive control from the manifest header once after applying.
- Per pool namespace: a `ResourceQuota` (pods 40, requests.cpu 6,
  requests.memory 12Gi, PVCs 10, services 20, secrets 40) and a `LimitRange`
  (defaultRequest 50m/64Mi, default limit 2/2Gi, max 4/8Gi, so `kubectl run`
  helper pods and agent pods without resources still admit under the quota).
  The overlay turns the chart's own quota and limit range off.
- Both Namespaces: `pod-security.kubernetes.io/enforce: baseline`, `warn` and
  `audit: restricted`. Every pod template the overlay renders passes baseline
  (chart test) and so does the controller's plain agent pod
  (`podmanager/pss_baseline_test.go`); DinD/Ziti sidecars are refused at the
  controller's preflight, which the modules needing them already treat as a
  skip.
- **No ClusterRole, no ClusterRoleBinding.**
- `ecr-token-refresh` CronJob: add `gardenio-e2e-0 gardenio-e2e-1` to the
  `for NS in …` list and the matching Role/RoleBinding pairs. Without it every
  pod in the pool is `ImagePullBackOff`.
- No operator-minted Secrets. Each run mints a random beads token and a
  random coop credential key with `kubectl create secret generic` (never
  `apply`, per `lint-kubectl-apply-secret.py`); the chart generates the
  postgres password. Shards read the token back from the namespace Secret
  rather than receiving it through job outputs.

## 7. Teardown that always runs

- `live-teardown` has `if: always()` and `needs` every lane job.
- Setup wipes before installing (covers a previous run whose teardown never
  ran) and sweeps any pool namespace with an expired lease.
- A spot reclaim mid-shard leaves a live lease; the next run's sweep (or the
  nightly, at the latest) wipes it after 2h.
- `retry-lost-runners.yml` already re-runs reclaimed jobs; a re-run of
  `live-setup` finds the stale lease and recovers.

## 8. Reporting and "done"

Each shard uploads `junit-results-<n>/` (JUnit XML + jsonl); `live-report`
merges the jsonl into one table (status, duration, module, skip reason) in
the run's step summary and fails if any module failed. A known-failing module
is **not** skipped: it stays red in the table and gets its own bug bead
(census class and reason recorded there). Done = a nightly executes the four
shards against a pool namespace and the table lists every NAMESPACE module
with pass/fail.

## 9. Operator steps (verbatim in the merge_window escalation)

1. `kubectl apply -f gardenio/tests/e2e/lane/rbac/namespace-pool.yaml`
   (namespaces with PSS labels, SA, Roles, RoleBindings, ResourceQuotas,
   LimitRanges; no Secrets in it), then the `kubectl auth can-i … --as=` loop
   in its header (direct ceiling verbs plus `escalate roles`, `bind roles`,
   `bind clusterroles`): every line prints `no`; then the positive control
   from the header (create a Role granting `update resourcequotas` as the
   runner, `--dry-run=server`): Forbidden.
2. Apply the `ecr-token-refresh` change
   (`gardenio/helm/gardenio-ziti/ecr-token-refresh.yaml`) and run the
   CronJob once: `kubectl -n gardenio create job --from=cronjob/ecr-token-refresh ecr-e2e-seed`;
   confirm `kubectl -n gardenio-e2e-0 get secret ecr`.
3. `helm upgrade --install gardenio-k3s-e2e oci://ghcr.io/actions/actions-runner-controller-charts/gha-runner-scale-set --version $ARC -n arc-runners -f gardenio/tests/e2e/lane/arc-runners-e2e-values.yaml`
   and confirm the listener registers (`kubectl get autoscalingrunnersets -n arc-runners`).
4. Merge the lane PR, then `gh workflow run e2e.yml -f suites=live-smoke`.
5. Merge the shards PR, then `gh workflow run e2e.yml -f suites=live-shards`
   and read `live-report`.

## 10. Known limits and follow-ups (separate beads)

- 5 slack-bridge-backed modules stay EXTERNAL until the chart exposes
  `SLACK_API_URL` and `images.yml` publishes `slackmock`.
- `test-concurrency-isolation.sh` is still red on main (gd-xg0uQhRE0G); the
  lane is safe regardless (one namespace and one runner pod per run).
- The nightly also runs `coop`, `viewer`, `go-integration` on heavy as
  before; the lane adds nothing there.
- `push: agent/e2e-ci/**` would now also run the lane; this work uses
  `agent/e2e-live/**` to avoid burning a namespace per push while the RBAC
  is not applied.
