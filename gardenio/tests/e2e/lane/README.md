# Live-namespace e2e lane

The lane that runs `run-shard.sh` against a real namespace in GitHub Actions
(task gd-WBz2qt9ADZ; design
[`docs/superpowers/specs/2026-10-13-e2e-live-namespace-lane-design.md`](../../../../docs/superpowers/specs/2026-10-13-e2e-live-namespace-lane-design.md);
census gd-xad8rBcLmH). Workflow jobs: `live-setup → live-smoke | live-shard ×4
→ live-report → live-teardown` in [`.github/workflows/e2e.yml`](../../../../.github/workflows/e2e.yml).
Driver: [`../scripts/lane.sh`](../scripts/lane.sh); its hermetic test is
[`../scripts/test-lane-lib.sh`](../scripts/test-lane-lib.sh) (runs in
`checks.yml` script-selftests).

| file | owner | applied by |
|---|---|---|
| `values-e2e.yaml` | the lane | `lane.sh install` (packs the chart with `scripts/pack-gardenio-chart.sh`, then `helm upgrade --install <ns> <packed .tgz> -f values-e2e.yaml --set global.imageTag=<newest calver>`; the unpacked dir overflows the 1 MiB release Secret, gd-mS5EZeWhZF) |
| `rbac/namespace-pool.yaml` | operator | `kubectl apply -f` once: the two pool namespaces (`pod-security.kubernetes.io/enforce=baseline`, warn/audit `restricted`), the runner SA, one Role + RoleBinding + ResourceQuota + LimitRange per namespace. The Role enumerates resources (never `*` in the core group) and holds **no verb on resourcequotas, limitranges or namespaces**, and its rbac rule names its verbs so it holds **neither `escalate` nor `bind`**. **Nothing cluster-scoped.** |
| `arc-runners-e2e-values.yaml` | operator | `helm upgrade --install gardenio-k3s-e2e … -n arc-runners -f` — the fourth scale set: medium shape + `serviceAccountName: gardenio-e2e-runner`, `maxRunners: 6` |

Plus one edit outside this directory the operator applies:
`gardenio/helm/gardenio-ziti/ecr-token-refresh.yaml` now lists the pool
namespaces, so they receive the `ecr` pull Secret (ghcr packages are private).

## Invariants

- **The overlay renders namespaced core kinds only.** Check after any chart
  change (CI's chart tests do not, yet):

  ```sh
  helm template gardenio-e2e-0 gardenio/helm/gardenio -n gardenio-e2e-0 \
    -f gardenio/tests/e2e/lane/values-e2e.yaml \
    | grep -E '^kind: (Cluster|Namespace|PriorityClass|StorageClass|RuntimeClass|ValidatingAdmission)' \
    && echo "FAIL: cluster-scoped object" || echo ok
  ```

- **Release name == namespace name.** `run-shard.sh` derives `<ns>-beads`,
  `<ns>-agents`, `<ns>-coopmux` from `NAMESPACE`; the chart's `fullname` is the
  release name.
- **A run owns a namespace through a Lease** (`e2e-run-lock`, 2h). A lease
  older than its duration is a run whose teardown never ran; `lane.sh` sweeps
  it. Never hand-delete a live lease to "free" a namespace; wait or widen the
  pool.
- **Wipe never touches operator objects.** Deletes are by kind for workloads
  and label-scoped (`app.kubernetes.io/managed-by=Helm`, `gardenio.io/e2e-run`)
  for Secrets/SAs/RBAC. The `ecr` Secret, the tenant Role/RoleBinding, the
  ResourceQuota and the LimitRange survive every wipe.
- **The runner cannot touch the ceiling.** The tenant Role enumerates core
  resources, so it holds no verb on `resourcequotas`, `limitranges` or
  `namespaces` (it cannot raise the quota or drop the Pod Security label).
  **Nor can it grant itself one:** the rbac rule on `roles`/`rolebindings`
  names its verbs (`get list watch create update patch delete`), never `*`,
  so the runner holds neither `escalate` (write a Role with rules you do not
  hold) nor `bind` (reference one), and the API's grant-time escalation check
  refuses any Role it writes that names the ceiling kinds. Proven three ways:
  the chart test
  `gardenio/helm/gardenio/tests/e2e_lane_namespace_pool_test.go` evaluates
  `can-i` over the manifest offline (direct verbs and escalate/bind) and
  checks the tenant Role is a superset of every Role the overlay renders and
  that every rendered RoleBinding references a Role, not a ClusterRole;
  `lane.sh assert-tenant` runs the real `kubectl auth can-i` from the runner's
  token before every install, then the positive control (create a Role
  granting `update resourcequotas` as the runner, server-side dry run, must
  be Forbidden) (in `test-lane-lib.sh` against a fake API); and the operator
  runs the `--as=system:serviceaccount:arc-runners:gardenio-e2e-runner` loop
  and the same positive control in the manifest header after applying it.
  Because the runner holds no verb on
  those two kinds, `values-e2e.yaml` turns the chart's own ResourceQuota and
  LimitRange off; the operator's LimitRange supplies default requests so
  `kubectl run` helper pods admit under a quota on `requests.cpu/memory`.
- **Pool namespaces enforce Pod Security `baseline`.** Every pod template the
  overlay renders passes (same chart test), and so does the plain agent pod the
  controller spawns (`podmanager/pss_baseline_test.go`). The DinD and Ziti
  sidecars do not: the controller's admission preflight refuses them, and the
  modules that need them skip (`test-sidecar-injection.sh`) or are parked on
  k3s (gVisor).
- **Secrets are `kubectl create`d, never `apply`d**
  (`scripts/security/lint-kubectl-apply-secret.py`).

## Running by hand

With a kubeconfig that holds `gardenio-e2e-tenant` in a pool namespace:

```sh
ns=$(./gardenio/tests/e2e/scripts/lane.sh acquire)
./gardenio/tests/e2e/scripts/lane.sh install "$ns"        # newest calver tag; or pass one
./gardenio/tests/e2e/scripts/lane.sh seed "$ns"
BEADS_TOKEN=$(./gardenio/tests/e2e/scripts/lane.sh token "$ns") \
  NAMESPACE=$ns E2E_SHARD=0 TOTAL_SHARDS=4 SKIP=$(./gardenio/tests/e2e/scripts/lane.sh skip-list) \
  ./gardenio/tests/e2e/scripts/run-shard.sh
./gardenio/tests/e2e/scripts/lane.sh report junit-results-0/e2e-results.jsonl
./gardenio/tests/e2e/scripts/lane.sh teardown "$ns"
```

## Reading a run

`live-report`'s step summary is one table: status, duration, shard, module,
skip/fail reason, over every shard. A failing module is red there and gets its
own bug bead (class and reason from the census); it is not skipped. The
`live-namespace-logs` artifact (beads, controller, coopmux, viewer,
trigger-engine logs + pods + events, captured before the wipe) is where a
red module's cause lives.

## Adding a pool namespace

Copy the `gardenio-e2e-1` block in `rbac/namespace-pool.yaml` (Namespace with
its PSS labels, Role, RoleBinding, ResourceQuota, LimitRange), rename, add the
name to `LANE_POOL` in `lane.sh` and to the `for NS in` list and Role pairs in
`ecr-token-refresh.yaml`. Three namespaces lets two full runs overlap.
