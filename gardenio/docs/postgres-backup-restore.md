# In-box Postgres backup & restore

The chart ships an **opt-in, fully in-cluster** logical-backup CronJob for the
in-box Postgres StatefulSet (`templates/postgres/backup-cronjob.yaml`). It is
the in-box alternative to the EBS-CSI `VolumeSnapshotClass`
(`templates/volumesnapshotclass.yaml`), which is cloud-only (driver
`ebs.csi.aws.com`) and unusable when the chart runs with no AWS API at runtime.

The CronJob reuses the postgres image (`pgvector/pgvector:pg17` — `pg_dump`/
`pg_restore` already present), connects over the in-cluster service DNS, and
writes timestamped `pg_dump -Fc` (custom-format, restorable with `pg_restore`)
archives to a dedicated PVC, pruning to the N most-recent files each run.

## Enabling

Backups are **off by default** and require the in-box StatefulSet
(`postgres.enabled: true`). Enable in your values:

```yaml
postgres:
  enabled: true
  backup:
    enabled: true
    schedule: "30 2 * * *"   # nightly 02:30, cluster timezone
    retention: 7             # keep 7 most-recent archives
    storage:
      size: 20Gi
      storageClass: ""       # falls back to global.storageClass
```

| Key | Default | Purpose |
|---|---|---|
| `postgres.backup.enabled` | `false` | Master switch (also needs `postgres.enabled`). |
| `postgres.backup.schedule` | `30 2 * * *` | Cron schedule (cluster timezone). |
| `postgres.backup.retention` | `7` | Keep this many most-recent `*.dump` archives. |
| `postgres.backup.activeDeadlineSeconds` | `3600` | Kill a dump that hangs past this. |
| `postgres.backup.storage.size` | `20Gi` | Backup PVC size. |
| `postgres.backup.storage.storageClass` | `""` | PVC storage class (falls back to `global.storageClass`). |
| `postgres.backup.storage.accessModes` | `[ReadWriteOnce]` | Backup PVC access modes. |

This renders two objects: a `PersistentVolumeClaim`
(`<release>-postgres-backup`) and a `CronJob` (`<release>-postgres-backup`).
Archives are named `<database>-<UTC-timestamp>.dump`, e.g.
`beads-20260609T023000Z.dump`.

## Verifying

```sh
# CronJob and PVC exist
kubectl -n <ns> get cronjob,pvc -l app.kubernetes.io/component=postgres-backup

# Trigger an out-of-schedule run to test
kubectl -n <ns> create job --from=cronjob/<release>-postgres-backup pg-backup-manual

# Watch it and read the log
kubectl -n <ns> logs -l app.kubernetes.io/component=postgres-backup --tail=-1

# List the archives on the PVC (mount it from a throwaway pod)
kubectl -n <ns> get pods   # find a postgres-backup pod, or:
kubectl -n <ns> run pg-ls --rm -it --restart=Never \
  --image=pgvector/pgvector:pg17 \
  --overrides='{"spec":{"containers":[{"name":"ls","image":"pgvector/pgvector:pg17","command":["ls","-lh","/backups"],"volumeMounts":[{"name":"b","mountPath":"/backups"}]}],"volumes":[{"name":"b","persistentVolumeClaim":{"claimName":"<release>-postgres-backup"}}]}}'
```

## Restore

`pg_restore` reads the custom-format `*.dump` archives. The archive PVC is
`ReadWriteOnce`, so run the restore from a pod that mounts it (the backup PVC)
and reaches the DB.

### 1. Pick the archive

```sh
kubectl -n <ns> debug ...   # or a throwaway pod mounting the backup PVC
ls -lh /backups              # archives are newest-last by name; pick one
```

### 2. Restore into the live database

`pg_dump -Fc` produces a custom-format archive; restore it with `pg_restore`.
Run from a pod that mounts the backup PVC and has the DB connection env. The
simplest path is to mount the backup PVC into a one-off pod using the postgres
image and run `pg_restore` against the service:

```sh
PGPASSWORD=$(kubectl -n <ns> get secret <release>-postgres-password \
  -o jsonpath='{.data.password}' | base64 -d)

kubectl -n <ns> run pg-restore --rm -it --restart=Never \
  --image=pgvector/pgvector:pg17 \
  --env="PGPASSWORD=${PGPASSWORD}" \
  --overrides='{
    "spec": {
      "containers": [{
        "name": "restore",
        "image": "pgvector/pgvector:pg17",
        "stdin": true, "tty": true,
        "env": [{"name":"PGPASSWORD","value":"'"${PGPASSWORD}"'"}],
        "command": ["bash"],
        "volumeMounts": [{"name":"b","mountPath":"/backups"}]
      }],
      "volumes": [{"name":"b","persistentVolumeClaim":{"claimName":"<release>-postgres-backup"}}]
    }
  }'
```

Then inside the pod:

```sh
# --clean --if-exists drops objects before recreating (in-place restore).
# Drop them and restore in one pass. Use --create only when restoring into a
# fresh server without the target DB yet.
pg_restore \
  --host=<release>-postgres --port=5432 --username=postgres \
  --dbname=beads \
  --clean --if-exists --no-owner --no-privileges \
  --exit-on-error \
  /backups/beads-<timestamp>.dump
```

### 3. Restore into a fresh empty database

If the target DB does not exist yet (e.g. disaster recovery onto a new volume),
let `pg_restore` create it. Connect to the maintenance `postgres` database and
pass `--create`:

```sh
pg_restore \
  --host=<release>-postgres --port=5432 --username=postgres \
  --dbname=postgres \
  --create --no-owner --no-privileges \
  --exit-on-error \
  /backups/beads-<timestamp>.dump
```

### Notes

- **pgvector extension**: the archive includes `CREATE EXTENSION` statements, so
  a restore re-creates the `vector` extension — but the target image must ship
  pgvector. The chart's `pgvector/pgvector:pg17` image does; vanilla
  `postgres:17` does not.
- **`--no-owner --no-privileges`**: the in-box setup uses the single `postgres`
  superuser, so role-grant restoration is unnecessary and noisy. Drop these
  flags if you restore into a multi-role server.
- **Stopping writers first**: for a clean point-in-time restore, scale the
  beads daemon / controller to 0 so nothing writes mid-restore, then scale back
  up once `pg_restore` exits 0.
- **External Postgres**: this CronJob targets only the in-box StatefulSet
  (`postgres.enabled`). For `postgresExternal` / RDS, use the provider's
  managed snapshot/backup tooling instead.

## Off-node S3 backup (gd-vekJWxnNGG)

`postgres.backup` above and the EBS-CSI `VolumeSnapshotClass` are the only
other backup primitives in the chart, and **neither survives node loss** on a
single-node cluster: `postgres.backup` writes to a second PVC that is
node-affine local storage bound to the *same* node as the live data, and
EBS-CSI is cloud-only. `postgres.s3Backup` (`templates/postgres/s3-backup-cronjob.yaml`)
closes that gap: it runs `pg_dumpall` to an `emptyDir` (no PVC at all) and
ships the zstd-compressed dump to S3 with an `aws-cli` sidecar reading the k3s node's
IMDS instance role — modelled on `helm/beads/dolt-backup-cronjob.yaml`.

```yaml
postgres:
  enabled: true
  s3Backup:
    enabled: true
    schedule: "17 * * * *"                      # hourly
    bucket: gardenio-hq-backups-123456789012
    prefix: k3s-postgres
```

| Key | Default | Purpose |
|---|---|---|
| `postgres.s3Backup.enabled` | `false` | Master switch (also needs `postgres.enabled`). |
| `postgres.s3Backup.schedule` | `17 * * * *` | Cron schedule (cluster timezone). |
| `postgres.s3Backup.bucket` | `""` | Destination S3 bucket. |
| `postgres.s3Backup.prefix` | `k3s-postgres` | Destination S3 prefix — must match a terraform `extra_backup_prefixes` entry granting the node role access, or the sidecar fails `AccessDenied`. |
| `postgres.s3Backup.region` | `us-east-1` | AWS region for the `aws` CLI. |
| `postgres.s3Backup.activeDeadlineSeconds` | `900` | Kill a dump+upload that hangs past this. |

This is write-only from the cluster's side: the node role needs
`PutObject`/`GetObject`/`GetObjectVersion`/`DeleteObject`/`AbortMultipartUpload`
on `s3://<bucket>/<prefix>/*`, granted in
`terraform/composition/gardenio/main.tf`'s `extra_backup_prefixes` (there is no
IRSA on this cluster, so every pod that can reach IMDS inherits the node
role — same tradeoff as the existing `k3s-dolt` and `ziti-ctrl` grants).
Retention is an S3 lifecycle rule on the prefix (`terraform/modules/hq`), not
job-side pruning — each run uploads a new timestamped object with `aws s3 cp`,
never `sync --delete`.

Dumps land at `s3://<bucket>/<prefix>/pgdumpall-<UTC-timestamp>.sql.zst`,
compressed with `zstd -T2 -3` (gd-w24jkWkith). Objects written before that
change are `.sql.gz` (gzip); restore those with `gunzip` in place of
`zstd -d` below.

### Restoring from an S3 dump

`pg_dumpall` produces a single plaintext (zstd-compressed) stream covering every
database plus global roles — restore with `psql`, not `pg_restore`.

**Do NOT stream a large dump through a long-lived exec pipe** (e.g. piping it
over an OpenZiti tunnel exec session). A ~22MB dump has been observed to time
out *silently* partway through with `psql` still exiting 0, leaving a
partially-populated database that looks like a successful restore until you
check row counts against the source. Copy the artifact **into** the pod first,
then restore locally:

```sh
# 1. Download the dump locally, verify its sha256 against the S3 object's
#    ETag/version metadata if you have it.
aws s3 cp s3://<bucket>/<prefix>/pgdumpall-<timestamp>.sql.zst ./pgdumpall.sql.zst

# 2. Copy it INTO a throwaway pod (chunked kubectl cp handles this fine at
#    this size) rather than piping through a live exec session.
kubectl -n <ns> cp ./pgdumpall.sql.zst <restore-pod>:/tmp/pgdumpall.sql.zst

# 3. Restore locally inside the pod — decompress to disk first, then psql
#    against the target (a scratch DB for a drill, or the live server for a
#    real DR restore after stopping writers). The pgvector/pgvector:pg17
#    image ships zstd. With no disk for the raw dump (~33 GB in 2026-10),
#    stream it instead: zstd -dc /tmp/pgdumpall.sql.zst | psql ... -f -
kubectl -n <ns> exec -it <restore-pod> -- bash -c '
  zstd -d /tmp/pgdumpall.sql.zst -o /tmp/pgdumpall.sql
  PGPASSWORD=$PGPASSWORD psql --host=<host> --port=5432 --username=postgres \
    -f /tmp/pgdumpall.sql
'

# 4. Verify: compare row counts of a few key tables (e.g. beads, deps,
#    comments, events) between the restored target and production. Equal
#    counts mean you compared the dump to itself — the restored counts
#    should be slightly LOWER than live, by however long production kept
#    writing after the dump's snapshot time.
```

A backup that has not been restored is a file, not a backup — re-run this
drill periodically against the CronJob's own latest output, not just the
one-off manual dump that first proved the pattern.
