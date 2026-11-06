# Tarboat test-in-box scripts

Scripts that validate the **boxed** gardenio artifact (the air-gapped tarball),
as opposed to prod. Home epic: **ka-0000003645** (test-in-box e2e regression
suite). Operator: Matt Baker (oidc:matthew.baker@acme.example).

## The provision → validate → file-regressions → teardown chain

The standing regression suite (driven by the test-in-box gardener or a CI cron)
runs against a **fresh ephemeral box** that the suite stands up itself:

0. **`tarboat-provision-box.sh provision`** (E4, ka-0000002700) — launches a
   fresh EC2 tarboat box (reusing `render-user-data.sh` + the `run-instances`
   contract in `gardenio/docs/gardenio-in-a-box-install.md` §A.3), tags it for the
   reaper, waits for its edge to answer, and emits a machine-readable
   **`tarboat-box/v1`** descriptor: `{instance_id, ip, dashed_ip, apex, hosts,
   namespace, calver, allocation_id, eip_allocated_by_us, expiry_epoch, ready,
   …}`. Feed `ip`+`namespace` straight into the validate step. This is the step
   that turns Phase A (validate-an-already-provisioned-box) into a *standing*
   Phase-B sweep — provisioning is no longer an operator action.

1. **`tarboat-validate.sh`** (E1, ka-0000001107) — runs the sequenced gates
   (air-gap staging + checksum → bootstrap in true-air-gap → acceptance smoke →
   in-box inference round-trip → **login completes** → **beads data present** →
   **DB persistence** → **cache state** → **snapshot integrity** →
   **chart lint** → **helm upgrade** → **helm rollback** →
   **routing** → **load balancing** → **DNS**)
   against the box and, with `--results-json PATH`, emits a machine-readable
   **`tarboat-validate/v1`** document: one object per gate `{key, status:
   pass|fail|skip, duration_seconds, message}` plus `calver`/`ip`/timestamps and
   a top-level `release_grade` flag.

   **The functional "it actually works" gates** (operator directive, decision
   ka-0000006524 — *"prove a newly provisioned gasbox actually works; it seems
   like right now it is not working"*). Posture-green + Services-exist is NOT
   proof a fresh box works. Three gates assert the bars the live-box reconcile
   (ka-0000005410) had to verify by hand and that the posture-only checks miss:
   - `inference` — with **`--inference-live`**, the gate POSTs a REAL
     `/v1/messages` through the in-cluster LiteLLM and requires an assistant
     message back (catches the coopmux↔agent netpol breakage, bug ka-0000004409,
     that a Service-exists + unauthenticated `/health` probe stays green through).
     Off in true air-gap (no LLM-provider egress); turn it on for a live box.
   - `login` — follows the full oauth2 chain to the rendered Keycloak Sign-in
     page and requires a usable form with **no `invalid_scope`** (the headline
     gasbox breakage, epic ka-0000006506), not merely a route-gating 302.
     Skip with `--skip-login`.
   - `beads_data` — the `beads` table must have **> 0 rows** (daemon↔postgres
     path healthy), not just pods Running. Skip with `--skip-beads`.

   **The persistence & state gates** (ka-0000004199). A posture check (pods
   Running, `/health` 200) proves the state layer is *up*; it does not prove the
   state layer actually *persists a write* or *survives*. Three gates close that:
   - `db_persistence` — writes a unique sentinel row into the beads postgres,
     reads it back (proves the RW path end-to-end), **and** classifies the data
     dir backing: a `persistentVolumeClaim`-backed StatefulSet is durable across
     a pod restart (release-grade); an `emptyDir` data dir
     (`postgres.persistence.enabled=false`, the ephemeral test-namespace shape,
     ka-0000000711) round-trips now but is **lost on restart** and FAILS unless
     `POSTGRES_EPHEMERAL_OK=1`. It derives durability from the live pod spec
     rather than restart-bouncing postgres (which would blow the run's DB out
     from under the other gates). The sentinel lives in its own table, dropped
     after read, so it never pollutes the `beads_data` count. Skip with
     `--skip-persistence`.
   - `cache_state` — SET/GET round-trip through the **optional** in-chart LiteLLM
     Redis (`component=litellm-redis`, renders only when
     `litellm.redis.deploy.enabled`). An **absent** in-chart Redis PASSES
     (external/none → cache-state N/A); only a present-but-not-serving cache
     fails. Skip with `--skip-cache`.
   - `snapshot_integrity` — a live `pg_dump -Fc` piped into `pg_restore -l` must
     enumerate a non-empty archive table-of-contents. This is the exact
     custom-format mechanism the `postgres.backup` CronJob uses, so a green gate
     proves the on-box backup path yields a **restorable** archive — not merely
     that a dump file exists. Read-only (nothing is restored). Skip with
     `--skip-snapshot`.

   All three factor their pass/fail decision into pure, unit-tested classifiers
   (`classify_persistence` / `classify_cache` / `classify_snapshot`) locked in
   `tarboat-validate_test.sh` without a live box.

   **The helm deployment gates** (ka-0000003227). The box installs the platform
   as a **Helm release**, so the release machinery — chart validity, in-place
   upgradeability, and a rollback-safe revision ledger — is its own regression
   surface, distinct from the state gates and invisible to a pods-Running /
   `/health`-200 posture check. Three gates cover it, all **read-only /
   non-mutating** so they never disturb the running box the other gates depend on:
   - `chart_lint` — `helm lint` on the **shipped** gardenio chart
     (`gardenio/helm/gardenio` under the staged repo, or `--chart-dir DIR`). Fully
     offline (no cluster): proves the chart the box would (re)deploy from is
     structurally valid. Skip with `--skip-chart-lint`.
   - `helm_upgrade` — the live release must be `deployed` **and** a
     `helm upgrade --dry-run --reuse-values` of it must render against the live
     cluster. `--dry-run` applies nothing; a green gate proves the running release
     can still be re-templated — catching a chart that installed once but drifted
     out of upgradeability (values-schema break, removed template dep) that
     posture never sees. Skip with `--skip-helm-upgrade`.
   - `helm_rollback` — `helm history` must be a coherent revision ledger whose
     latest revision is `deployed`. A single deployed revision (freshly installed)
     **passes** — rollback machinery present, no prior target yet; an empty
     history or a stuck/mid-operation latest revision fails. Read-only: nothing is
     rolled back (a real rollback would disrupt the box). Skip with
     `--skip-helm-rollback`.

   These three likewise factor into pure, unit-tested classifiers
   (`classify_chart_lint` / `classify_helm_upgrade` / `classify_helm_rollback`)
   locked in `tarboat-validate_test.sh` without a live box or a helm binary.

   **The networking gates** (ka-0000006092). Posture-green (routes, Services and
   pods *exist*) does not prove a request actually reaches a backend. Three gates
   assert the data path itself, catching the black-holes a Service/route-exists
   check sails straight through:
   - `routing` — every Traefik **IngressRoute** service reference on the box must
     name a **Service that exists** in the namespace. A dangling ref means Traefik
     loaded the route but it 404s that Host silently (the `ka-0000007182` class of
     "route loaded, goes nowhere" bug). A box whose routing is edge-owned (AVA/ALB,
     no Traefik IngressRoute CRD) has nothing in-cluster to validate and PASSES
     (`no-routes`). Read-only. Skip with `--skip-routing`.
   - `load_balancing` — each critical Service (`beads`, `beads-viewer`,
     `forwardauth`, `litellm`) must have **≥1 READY endpoint**. A Service with a
     ClusterIP but zero ready backends exists + resolves + accepts a connection,
     then 503s because kube-proxy has nothing to forward to. **Absent** Services
     are skipped (optional component / box shape); only a present-but-endpointless
     one fails. Override the list with `LB_SERVICES`. Read-only. Skip with
     `--skip-lb`.
   - `dns` — in-cluster **CoreDNS** must resolve a Service's cluster FQDN
     (`<svc>.<ns>.svc.cluster.local`) to its ClusterIP, probed from a throwaway
     in-cluster pod. Everything in the box talks by DNS name, so a broken CoreDNS
     is a whole-box outage that leaves every pod Running but unable to reach
     anything. Headless Services still validate resolution (any answer counts).
     Override the target with `DNS_SERVICE`. Read-only. Skip with `--skip-dns`.

   All three factor their pass/fail decision into pure, unit-tested classifiers
   (`classify_routing` / `classify_lb` / `classify_dns`) locked in
   `tarboat-validate_test.sh` without a live box.

   The functional gates carry pure, unit-tested response classifiers
   (`classify_login` / `classify_inference` / `classify_beads_count`) so the
   pass/fail logic is locked in `tarboat-validate_test.sh` without a box.

2. **`tarboat-file-regressions.sh`** (E2, ka-0000001077) — consumes that results
   doc and, for every gate with `status == "fail"`, auto-files a typed
   regression **bug** parented to the suite epic, capturing the gate key, the box
   calver, and the failing assertion message. It de-dupes on `(gate, calver)`:
   re-running the suite on the same box never files a duplicate while an open bug
   for that gate+calver already exists.

3. **`tarboat-provision-box.sh teardown`** (E4) — terminates the instance and
   releases the EIP **the provisioner allocated** (it never releases an EIP it
   did not own). A `tarboat-provision-box.sh reap` safety net terminates any
   ephemeral-tagged box past its expiry — the box-level mirror of the
   namespace reaper the RWX-era release pipeline ran. Both honor `lifecycle=keep` (refuse / skip).

### The two ephemeral lifecycles (don't confuse them)

The RWX-era release pipeline (deleted 2026-09-16) had a **namespace**-level
ephemeral lifecycle: it provisioned a per-instance namespace per run, tore it
down, and ran a reaper CronJob as the periodic safety net. E4 is the
**EC2-box**-level analogue of that triad for
the *boxed* artifact: `provision` / `teardown` / `reap`, same `lifecycle=keep`
exemption contract. A tarboat box is a whole EC2 instance (its own k3s), not a
namespace in a shared cluster — so it needs its own provisioner.

### Canonical invocation (gardener / CI cron)

```sh
# 0. Stand up a fresh box; capture its descriptor (instance id, IP, apex, …).
scripts/inbox/tarboat-provision-box.sh provision \
    --calver "$CALVER" --namespace gardenio \
    --ami "$TARBOAT_AMI" --subnet-id "$TARBOAT_SUBNET_ID" \
    --security-group-ids "$TARBOAT_SECURITY_GROUP_IDS" --key-name "$TARBOAT_KEY_NAME" \
    --ttl-minutes 120 --descriptor /tmp/tarboat-box.json
BOX_IP="$(sed -n 's/.*"ip": "\([^"]*\)".*/\1/p' /tmp/tarboat-box.json)"

# 1. Run the gates, capture a machine-readable verdict.
scripts/inbox/tarboat-validate.sh \
    --tarboat /srv/tarboat --namespace gardenio --ip "$BOX_IP" \
    --calver "$CALVER" \
    --results-json /tmp/tarboat-results.json || true   # don't abort the cron on NOSHIP

# 2. Turn every failing gate into a tracked regression bug under the suite epic.
scripts/inbox/tarboat-file-regressions.sh /tmp/tarboat-results.json

# 3. Always tear the box down (the reaper is the backstop if this is skipped).
scripts/inbox/tarboat-provision-box.sh teardown /tmp/tarboat-box.json
```

`tarboat-validate.sh` exits non-zero on any failing/skipped gate, so the `|| true`
keeps the cron alive long enough to run the filer — the results doc is emitted
from an `EXIT` trap regardless of exit code. The two can also be piped directly:

```sh
scripts/inbox/tarboat-validate.sh ... --results-json - \
  | scripts/inbox/tarboat-file-regressions.sh -
```

The filer exits 0 whenever every failing gate was handled (filed or already
open) — filing a regression is its success path. It exits non-zero only on
usage / parse / `kd` errors.

### Filer flags

```
tarboat-file-regressions.sh RESULTS_JSON [flags]
  RESULTS_JSON       path to a tarboat-validate/v1 doc, or "-" for stdin
  --epic ID          parent epic for filed bugs (default: ka-0000003645)
  --label L          dedupe/marker label on filed bugs (default: tarboat-regression)
  -l L               extra label to add to filed bugs (repeatable)
  --priority N       bug priority (default: 1)
  --operated-by ID   operator id stamped on filed bugs
  --dry-run          print what WOULD be filed; do not call `kd create`
```

Filed bugs carry typed fields `tarboat_gate=<key>` and `tarboat_calver=<calver>`
(the de-dupe keys) plus the `tarboat-regression` label.

### Provisioner flags (E4)

```
tarboat-provision-box.sh provision --calver VER [flags]
  --calver VER        REQUIRED — box image/calver (stamped into descriptor + tags)
  --region R          AWS region (default $AWS_REGION / $AWS_DEFAULT_REGION)
  --ami ID            AMI id (AL2023/Ubuntu)        default $TARBOAT_AMI
  --subnet-id ID      launch subnet                 default $TARBOAT_SUBNET_ID
  --security-group-ids "sg-a sg-b"   22/80/443-only SG(s)  default $TARBOAT_SECURITY_GROUP_IDS
  --key-name NAME     EC2 keypair for operator SSH  default $TARBOAT_KEY_NAME
  --instance-type T   default r7i.2xlarge (§A.1 RAM-bound default)
  --namespace NS      gardenio namespace on the box  default gardenio
  --ttl-minutes N     reaper expiry tag (default 120); --keep exempts from reap
  --no-eip            use the box's auto-assigned public IP, don't allocate an EIP
  --wait-timeout S    wait for the box edge to answer (default 1800s; 0 = no wait)
  --descriptor PATH   write the tarboat-box/v1 descriptor ("-"=stdout, default)
  --offline-assets D  true air-gap: pass through to render-user-data --offline-assets
  --dry-run           print the AWS calls; launch nothing

tarboat-provision-box.sh teardown (DESCRIPTOR|--instance-id ID) [--force] [--dry-run]
tarboat-provision-box.sh reap [--ttl-minutes N] [--dry-run]
```

The descriptor's `eip_allocated_by_us` flag is the EIP-ownership ledger:
`teardown` only releases an EIP the provisioner itself allocated. Both
`teardown` and `reap` honor `lifecycle=keep` (refuse without `--force` / skip).

### Dependencies

- `tarboat-validate.sh` runs **on the air-gapped box** — no jq/python, no network.
- `tarboat-file-regressions.sh` runs **on the orchestration side** (the gardener /
  CI host where `kd` lives) and uses `jq` to parse the results doc.
- `tarboat-provision-box.sh` runs **on the orchestration side** (where the AWS CLI
  + credentials live). It needs `aws` (and `curl` for the edge-readiness wait); it
  is jq-free — the descriptor is emitted/parsed by hand the way `tarboat-validate.sh`
  builds its results doc.

All three have sibling unit tests (`*_test.sh`) wired into `make lint` and the
checks.yml `script-selftests` job; the filer's test drives a stub `kd` and the provisioner's
test drives a stub `aws`, so neither needs a beads server, AWS, or network.
