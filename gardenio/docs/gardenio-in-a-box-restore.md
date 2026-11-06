# Gardenio-in-a-Box — backup & restore runbook

Disaster-recovery procedure for the **Gardenio-in-a-Box** single-node profile
(k3s + `local-path` provisioner, no AWS CSI at runtime). On the box the
local-path PVs are the **only** copy of state, so the backup story is
mandatory, not optional. This runbook covers the two layers shipped by the
chart plus the off-cluster DR floor.

Source research: [`research/inbox-storage-localpv.md`](research/inbox-storage-localpv.md) §5/§8.
Task: ka-0000008198.

> **Not for cloud / external RDS.** This is the in-box path. Cloud deployments
> use the EBS `VolumeSnapshotClass` and the prod per-DB `postgres.backup.*`
> `pg_dump -Fc` job ([`postgres-backup-restore.md`](postgres-backup-restore.md)),
> and managed RDS snapshots for external Postgres.

---

## The three layers

| Layer | What it protects | Mechanism | Chart object |
|---|---|---|---|
| **1. reclaimPolicy: Retain** | data survives `helm uninstall` / PVC delete | local-path StorageClass with `reclaimPolicy: Retain` | `templates/backup/storageclass-retain.yaml` |
| **2. pg_dumpall (the MUST)** | portable, app-consistent DB restore | `pg_dumpall \| gzip` CronJob → backup PVC | `templates/backup/postgres-cronjob.yaml` |
| **3. EBS host snapshot (DR floor)** | crash-consistent whole-box | off-cluster `aws ec2 create-snapshot` of the k3s data volume | *ops, not in-chart* |

NATS JetStream is an **optional** nice-to-have layer (`backup.nats`,
`templates/backup/nats-cronjob.yaml`) — see the bottom of this doc.

Enable layers 1–2 with the in-box overlays — `storageClassRetain.create: true`,
postgres/keycloak `persistence.storageClass` at `local-path-retain`, and
`backup.postgres.enabled` are set by the **storage half**
([`gardenio/helm/gardenio/values-inbox.yaml`](../helm/gardenio/values-inbox.yaml))
and pinned to the same values by the **edge half**
(`deploy-values/gardenio-inbox.yaml`, decommissioned — excluded from this export).
The full box layers both; see
[`inbox-overlay-layering.md`](inbox-overlay-layering.md) for the order.

---

## Layer 1 — reclaimPolicy: Retain (data survives uninstall)

k3s's built-in `local-path` StorageClass is `reclaimPolicy: Delete`: deleting a
PVC (or `helm uninstall`, which deletes the StatefulSet PVCs) **erases the
on-disk directory**. For the critical stores (postgres, nats, keycloak) that is
unrecoverable. The chart ships a sibling StorageClass `local-path-retain`
(`reclaimPolicy: Retain`); the overlay points those stores' data PVCs at it so a
deleted PVC **orphans** (rather than deletes) its PV and the data survives.

```sh
kubectl get storageclass local-path-retain          # RECLAIMPOLICY: Retain
kubectl get pvc -n <ns> -l app.kubernetes.io/component=postgres \
  -o custom-columns=NAME:.metadata.name,SC:.spec.storageClassName
```

### Caveat: reclaimPolicy is set at provision time

A StorageClass's `reclaimPolicy` is **immutable** and applies only to PVs
provisioned **after** a store binds to it. Switching `persistence.storageClass`
on an already-bound StatefulSet does **not** retroactively change the existing
PV's policy (and StatefulSet `volumeClaimTemplates` are themselves immutable).

To migrate an already-bound PVC to Retain without data loss, patch the PV in
place (no reprovision needed):

```sh
PV=$(kubectl get pvc -n <ns> data-<release>-postgres-0 -o jsonpath='{.spec.volumeName}')
kubectl patch pv "$PV" -p '{"spec":{"persistentVolumeReclaimPolicy":"Retain"}}'
kubectl get pv "$PV" -o custom-columns=NAME:.metadata.name,RECLAIM:.spec.persistentVolumeReclaimPolicy
```

After an `uninstall` that orphaned a Retain PV, the PV lands in `Released`. To
re-bind a fresh PVC to it, clear its stale `claimRef`:

```sh
kubectl patch pv "$PV" -p '{"spec":{"claimRef":null}}'   # -> Available
```

---

## Layer 2 — pg_dumpall (the MUST-backup)

The `templates/backup/postgres-cronjob.yaml` CronJob runs `pg_dumpall` against
the in-cluster postgres Service and writes a timestamped, gzipped **plaintext**
dump to a dedicated PVC (`<release>-postgres-dumpall`), pruning dumps older than
`backup.postgres.retentionDays`. `pg_dumpall` (not `pg_dump`) captures **every**
database — beads, controller, **and keycloak** — **plus the global roles** in
one stream. That is the artifact a fresh box is rehydrated from. The dump PVC
lives on the data volume, so a host EBS snapshot (layer 3) captures the dumps
too.

Image: `pgvector/pgvector:pg17` (the chart's postgres image — `pg_dumpall`
already present). Dumps are named `pg_dumpall-<UTC-timestamp>.sql.gz`.

### Verify backups are running

```sh
kubectl -n <ns> get cronjob,pvc -l app.kubernetes.io/component=postgres-dumpall-backup

# Trigger an out-of-schedule run
kubectl -n <ns> create job --from=cronjob/<release>-postgres-dumpall pg-dumpall-manual
kubectl -n <ns> logs -l app.kubernetes.io/component=postgres-dumpall-backup --tail=-1 -f

# List dumps on the PVC
kubectl -n <ns> run pg-ls --rm -it --restart=Never --image=pgvector/pgvector:pg17 \
  --overrides='{"spec":{"containers":[{"name":"ls","image":"pgvector/pgvector:pg17","command":["ls","-lh","/backups"],"volumeMounts":[{"name":"b","mountPath":"/backups"}]}],"volumes":[{"name":"b","persistentVolumeClaim":{"claimName":"<release>-postgres-dumpall"}}]}}'
```

### Restore from a pg_dumpall dump

`pg_dumpall` produces a plaintext SQL script (`CREATE ROLE`, `CREATE DATABASE`,
`\connect`, then per-DB data). Restore it with **`psql`** (not `pg_restore` —
that is only for `pg_dump -Fc` custom-format archives), connected to the
maintenance `postgres` database so the embedded `CREATE DATABASE` / `\connect`
statements can run.

**1. Stop the writers** so nothing mutates mid-restore:

```sh
kubectl -n <ns> scale deploy <release>-controller <release>-beads --replicas=0
# also any bridges / coopmux that write to the DB
```

**2. Run the restore** from a pod that mounts the dump PVC and reaches the DB:

```sh
PGPASSWORD=$(kubectl -n <ns> get secret <release>-postgres-password \
  -o jsonpath='{.data.password}' | base64 -d)

kubectl -n <ns> run pg-restore --rm -it --restart=Never \
  --image=pgvector/pgvector:pg17 \
  --env="PGPASSWORD=${PGPASSWORD}" \
  --overrides='{
    "spec": {
      "containers": [{
        "name":"restore","image":"pgvector/pgvector:pg17",
        "stdin":true,"tty":true,
        "env":[{"name":"PGPASSWORD","value":"'"${PGPASSWORD}"'"}],
        "command":["bash"],
        "volumeMounts":[{"name":"b","mountPath":"/backups"}]
      }],
      "volumes":[{"name":"b","persistentVolumeClaim":{"claimName":"<release>-postgres-dumpall"}}]
    }
  }'
```

Then inside the pod — gunzip and pipe into `psql` against the `postgres` DB:

```sh
# ON_ERROR_STOP=1 aborts on the first error instead of plowing through.
gunzip -c /backups/pg_dumpall-<timestamp>.sql.gz \
  | psql --host=<release>-postgres --port=5432 --username=postgres \
         --dbname=postgres --set ON_ERROR_STOP=1
```

> **Onto a fresh empty server** (e.g. a brand-new box / recreated data PV):
> identical command. `pg_dumpall` embeds the role and `CREATE DATABASE`
> statements, so it rebuilds the whole cluster — no `--create`/`--clean`
> flags needed. The target image must ship **pgvector** (the dump re-runs
> `CREATE EXTENSION vector`); `pgvector/pgvector:pg17` does, vanilla
> `postgres:17` does not.

**3. Restart the writers:**

```sh
kubectl -n <ns> scale deploy <release>-controller <release>-beads --replicas=1
```

After restore, the controller re-syncs coopmux credentials from the account
beads, the Keycloak operator reconciles the realm from its CRDs, and the
bridges rebuild their SSE cursors — no manual step for those.

---

## Layer 3 — EBS host snapshot (the DR floor)

The box runs on EC2, so the EBS volume under the k3s data dir
(`/var/lib/rancher/k3s/storage`) is snapshottable at the **AWS layer**,
independent of the cluster's storage class. This captures **every** PVC at once,
crash-consistent — including the dynamic agent PVCs the logical backups skip.

This is an **ops / control-plane action**, not a runtime call from the workload,
so it does **not** reintroduce an AWS runtime dependency.

```sh
# Find the data volume (run on the EC2 host or via the AWS API)
aws ec2 describe-volumes --filters Name=attachment.instance-id,Values=<instance-id> \
  --query 'Volumes[].{ID:VolumeId,Dev:Attachments[0].Device,GiB:Size}' --output table

# Snapshot it (cadence: hourly/daily). An AWS Backup plan automates this.
aws ec2 create-snapshot --volume-id <vol-id> \
  --description "gardenio-in-a-box $(date -u +%FT%TZ)" \
  --tag-specifications 'ResourceType=snapshot,Tags=[{Key=app,Value=gardenio-in-a-box}]'
```

### Whole-box restore from an EBS snapshot

1. `aws ec2 create-volume --snapshot-id <snap-id> --availability-zone <az>`
   (and `--volume-type`/`--size` to match).
2. Stop k3s, detach the current data volume, attach the restored one at the
   same device path, restart k3s. All PVs come back as they were at snapshot
   time (crash-consistent).
3. For an **app-consistent** DB state, layer the latest `pg_dumpall` restore
   (layer 2) on top — the EBS snapshot is crash-consistent, the dump is
   transactionally clean.

> **No volume expansion / recreate-to-resize.** local-path cannot honor
> `allowVolumeExpansion`; editing `requests.storage` silently no-ops. To grow a
> store, recreate the PV (restore into a larger volume).

---

## Optional layer — NATS JetStream backup

Enable `backup.nats.enabled` (requires `nats.enabled`) for zero JetStream
event-loss across a restore. The `templates/backup/nats-cronjob.yaml` CronJob
runs **`nats account backup`** (from the nats-box image) into a timestamped dir
on `<release>-nats-backup`, pruning dirs older than `backup.nats.retentionDays`.

> We use `nats account backup`, **never** a `cp -r` of the live JetStream dir —
> copying a live store corrupts it mid-write.

Most state does **not** need this: postgres (caught by layer 2) is the durable
record; NATS carries in-flight event history that the daemons rebuild. Turn it
on only if event continuity across a restore matters.

### Restore caveat — delete the stream first

`nats account restore` (or `nats stream restore`) **fails over an existing
same-named stream**. You must delete the stream, then restore:

```sh
# From a nats-box pod reaching the server (-s nats://<release>-nats:4222):
nats stream ls                                   # see what exists
nats stream rm MUTATION_EVENTS -f               # delete the conflicting stream
nats account restore /backups/nats-<timestamp>  # then restore
```

After restore, confirm the controller's `nats-stream-init` initContainer still
finds the stream (it is idempotent — creates `MUTATION_EVENTS` only if absent).

---

## Quick reference — values flags

| Key | Default | Purpose |
|---|---|---|
| `storageClassRetain.create` | `false` | Render the `local-path-retain` SC. |
| `storageClassRetain.reclaimPolicy` | `Retain` | Orphan (not delete) PVs on uninstall. |
| `backup.postgres.enabled` | `false` | pg_dumpall CronJob (also needs `postgres.enabled`). |
| `backup.postgres.schedule` | `17 */6 * * *` | Dump cadence (cluster TZ). |
| `backup.postgres.retentionDays` | `7` | Delete dumps older than N days. |
| `backup.nats.enabled` | `false` | Optional `nats account backup` (also needs `nats.enabled`). |
| `backup.nats.retentionDays` | `7` | Delete NATS backup dirs older than N days. |

The in-box overlays turn on layers 1–2 and leave the NATS layer off. The
storage half (`gardenio/helm/gardenio/values-inbox.yaml`) sets these; the edge
half (`deploy-values/gardenio-inbox.yaml`) pins the same values. Layer both —
see [`inbox-overlay-layering.md`](inbox-overlay-layering.md).
