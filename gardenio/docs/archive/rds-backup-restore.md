# RDS backup & restore (canonical-prod)

How the managed **Amazon RDS for PostgreSQL** instance behind canonical-prod is
backed up, and the **tested** procedure to restore it — both point-in-time
recovery (PITR) and from a specific snapshot.

> Scope: the cloud-managed RDS instance provisioned by
> `gardenio/terraform/modules/rds` and wired in
> `gardenio/terraform/composition/canonical-prod/stateful.tf`.
> For the **in-box** Postgres StatefulSet (the no-AWS chart deployment), see
> [`postgres-backup-restore.md`](../postgres-backup-restore.md) instead — that is a
> different, in-cluster `pg_dump` CronJob.

## What is backed up, and how

The RDS module turns on AWS-native automated backups + PITR. There is no
external job — RDS itself takes the snapshots and streams the logs.

| Control | Module var | canonical-prod default | Effect |
|---|---|---|---|
| Automated backups + PITR window | `backup_retention_days` | `30` | Daily snapshot **and** continuous transaction-log archiving. Restore to any second in the last 30 days. |
| Backup window | `backup_window` | `07:00-08:00` UTC | When the daily snapshot is taken. |
| Maintenance window | `maintenance_window` | `sun:08:00-sun:09:00` UTC | Patching window; must not overlap the backup window. |
| Survive teardown | `delete_automated_backups` | `false` | Retained automated backups + PITR history survive an instance destroy. |
| Final snapshot | `skip_final_snapshot=false`, `final_snapshot_identifier` | `<env>-gardenio-final` | A manual snapshot is taken on destroy. |
| Tag propagation | `copy_tags_to_snapshot` | `true` | Snapshots inherit instance tags. |
| Encryption | `storage_encrypted`, `kms_key_id` | CMK | Snapshots are encrypted with the same customer-managed key. |

**PITR window:** because `backup_retention_days > 0`, RDS keeps the daily
snapshot as the base and streams write-ahead logs continuously. The restorable
range is `earliest_restorable_time … latest_restorable_time`, roughly the last
`backup_retention_days` days up to ~5 minutes ago.

### Verify backups are configured (read-only)

```sh
ID=canonical-prod-gardenio-postgres   # <env>-gardenio-postgres

aws rds describe-db-instances --db-instance-identifier "$ID" \
  --query 'DBInstances[0].{retention:BackupRetentionPeriod,window:PreferredBackupWindow,maint:PreferredMaintenanceWindow,latest:LatestRestorableTime,multiaz:MultiAZ,deletionprot:DeletionProtection}' \
  --output table

# Earliest/latest PITR bounds:
aws rds describe-db-instances --db-instance-identifier "$ID" \
  --query 'DBInstances[0].[InstanceCreateTime,LatestRestorableTime]' --output text

# List automated + manual snapshots:
aws rds describe-db-snapshots --db-instance-identifier "$ID" \
  --query 'DBSnapshots[].{id:DBSnapshotIdentifier,type:SnapshotType,time:SnapshotCreateTime,status:Status}' \
  --output table
```

Terraform also exposes these via the composition output:
`terraform output -json data_plane | jq .rds` →
`backup_retention_days`, `backup_window`, `maintenance_window`,
`latest_restorable_time`.

## RTO / RPO

- **RPO** (data loss): ≤ ~5 minutes for PITR (transaction-log flush lag). From a
  daily snapshot: up to 24 h.
- **RTO** (time to restore): RDS provisions a **new** instance from the
  snapshot/PITR; expect ~15–40 min for the instance to reach `available`, plus
  the cutover steps below. RDS cannot restore in place — every restore creates a
  new instance you then cut over to.

> **Observed (POC drill, 2026-06-10):** a `--use-latest-restorable-time` PITR of
> `poc-gardenio-postgres` (db.t3.medium, PG 16.4) took **~43 min** to reach
> `available` (restore API 04:59:23Z → available 05:42:36Z). Sentinel row counts
> matched the source at the restore point. See epic [[ka-0000006754]] / task
> [[ka-0000003875]] for the full drill record.

## Restore — point in time (PITR)

Use when you need the database as of a specific moment (e.g. just before a bad
migration or deletion). **This creates a new instance**; the original is
untouched, which is what you want during an incident.

```sh
SRC=canonical-prod-gardenio-postgres
TGT=canonical-prod-gardenio-postgres-restore   # NEW identifier
WHEN=2026-06-10T14:30:00Z                      # UTC, within the PITR window

aws rds restore-db-instance-to-point-in-time \
  --source-db-instance-identifier "$SRC" \
  --target-db-instance-identifier "$TGT" \
  --restore-time "$WHEN" \
  --db-subnet-group-name "$(aws rds describe-db-instances --db-instance-identifier "$SRC" --query 'DBInstances[0].DBSubnetGroup.DBSubnetGroupName' --output text)" \
  --vpc-security-group-ids "$(aws rds describe-db-instances --db-instance-identifier "$SRC" --query 'DBInstances[0].VpcSecurityGroups[0].VpcSecurityGroupId' --output text)" \
  --no-multi-az \
  --no-publicly-accessible

aws rds wait db-instance-available --db-instance-identifier "$TGT"
```

> Restore the new instance into the **same subnet group + security group** so
> it lands in the private-data subnets and is reachable from the pods. Use
> `--use-latest-restorable-time` instead of `--restore-time` to restore to the
> most recent possible point.
>
> **Master password on restore:** the restored instance carries over the
> **source's master password** as of the restore point — a PITR/snapshot restore
> does **not** mint a new credential by default. (Confirmed in the 2026-06-10 POC
> drill: the restored instance came up with `MasterUserSecret=None` and the source
> master password still worked.) A *fresh RDS-managed master secret* is created
> only when the source already uses RDS-managed master password, or when you pass
> `--manage-master-user-password` on the restore call. If you want that, add the
> flag and point the app at the new secret ARN (see Cut over).

## Restore — from a snapshot

Use when you want a specific known-good daily/manual snapshot rather than a
point in time.

```sh
SNAP=$(aws rds describe-db-snapshots --db-instance-identifier canonical-prod-gardenio-postgres \
  --query 'reverse(sort_by(DBSnapshots[?Status==`available`],&SnapshotCreateTime))[0].DBSnapshotIdentifier' --output text)
TGT=canonical-prod-gardenio-postgres-restore

aws rds restore-db-instance-from-db-snapshot \
  --db-instance-identifier "$TGT" \
  --db-snapshot-identifier "$SNAP" \
  --db-subnet-group-name "$(aws rds describe-db-instances --db-instance-identifier canonical-prod-gardenio-postgres --query 'DBInstances[0].DBSubnetGroup.DBSubnetGroupName' --output text)" \
  --vpc-security-group-ids "$(aws rds describe-db-instances --db-instance-identifier canonical-prod-gardenio-postgres --query 'DBInstances[0].VpcSecurityGroups[0].VpcSecurityGroupId' --output text)" \
  --no-multi-az --no-publicly-accessible

aws rds wait db-instance-available --db-instance-identifier "$TGT"
```

## Cut over to the restored instance

1. **Find the new endpoint** (and master secret ARN, if one exists — it will be
   `None` for a default PITR/snapshot restore; see the master-password note above):
   ```sh
   aws rds describe-db-instances --db-instance-identifier "$TGT" \
     --query 'DBInstances[0].{ep:Endpoint.Address,secret:MasterUserSecret.SecretArn}' --output table
   ```
2. **Smoke-test the data** before cutting over (verify the restore actually has
   what you expect — see drill below).
3. **Repoint the app.** The chart reads the RDS host + master secret from the
   data-plane wiring. Two clean options:
   - **Promote in Terraform (preferred, durable):** rename the restored instance
     to the canonical identifier (delete/rename the broken one first, behind a
     fresh `gb decision` gate per advice [[ka-0000007102]]), then `terraform plan`
     should show no diff. Re-enable `--multi-az` via the module var.
   - **Fast manual cutover (incident):** update the chart's `postgresExternal.host`
     and the master-secret reference to the restored instance, redeploy, then
     reconcile Terraform afterward.
4. **Re-enable Multi-AZ** on the restored instance (restores come up
   single-AZ): `aws rds modify-db-instance --db-instance-identifier "$TGT" --multi-az --apply-immediately`.
5. **Re-enable deletion protection** on the promoted instance.

## Tested restore drill (run this to prove the procedure)

Do this in a non-prod env (or the POC) to validate the runbook end to end
**without touching prod**. No AWS writes against prod without a fresh
`gb decision` gate (advice [[ka-0000007102]]).

```sh
SRC=<env>-gardenio-postgres
TGT=${SRC}-drill

# 1. Note a known row count / sentinel BEFORE the restore point.
#    (psql via the bastion tunnel — see advice ka-0000001600 — or a throwaway
#     psql pod in the EKS cluster, which shares the RDS VPC/SG.)
psql "$DSN" -c "select count(*) from beads;"

# 2. PITR-restore to ~10 min ago into a throwaway instance.
aws rds restore-db-instance-to-point-in-time \
  --source-db-instance-identifier "$SRC" --target-db-instance-identifier "$TGT" \
  --use-latest-restorable-time \
  --db-subnet-group-name "$(aws rds describe-db-instances --db-instance-identifier "$SRC" --query 'DBInstances[0].DBSubnetGroup.DBSubnetGroupName' --output text)" \
  --vpc-security-group-ids "$(aws rds describe-db-instances --db-instance-identifier "$SRC" --query 'DBInstances[0].VpcSecurityGroups[0].VpcSecurityGroupId' --output text)" \
  --no-multi-az --no-publicly-accessible
aws rds wait db-instance-available --db-instance-identifier "$TGT"

# 3. Get the restored instance endpoint. The master password carries over from
#    the source (no new secret by default — MasterUserSecret will be None unless
#    the source uses managed master password). Reuse the SOURCE master password:
EP=$(aws rds describe-db-instances --db-instance-identifier "$TGT" --query 'DBInstances[0].Endpoint.Address' --output text)
SRC_SECRET=$(aws rds describe-db-instances --db-instance-identifier "$SRC" --query 'DBInstances[0].MasterUserSecret.SecretArn' --output text)
PW=$(aws secretsmanager get-secret-value --secret-id "$SRC_SECRET" --query SecretString --output text | jq -r .password)

# 4. Connect to the restored instance and verify the sentinel data matches.
#    (Run from inside the VPC — e.g. a throwaway psql pod in the EKS cluster, or
#    the bastion tunnel. The restored instance lands in the same private subnets/SG.)
psql "host=$EP user=gardenio dbname=gardenio password=$PW sslmode=require" -c "select count(*) from beads;"

# 5. CLEAN UP — delete the drill instance (no final snapshot needed for a drill).
aws rds delete-db-instance --db-instance-identifier "$TGT" --skip-final-snapshot --delete-automated-backups
```

**Drill pass criteria:**
- [ ] Restored instance reaches `available`.
- [ ] Restored instance lands in the private-data subnets + RDS security group
      (reachable from pods / bastion tunnel).
- [ ] A sentinel query against the restored instance returns the expected data.
- [ ] Master credential confirmed: source master password works against the
      restored instance (or, if `--manage-master-user-password` was passed, a new
      distinct secret ARN exists).
- [ ] Drill instance deleted; no orphaned instances/snapshots left behind.

Record the drill result (date, env, RTO observed) as a comment on the backup
epic so the "tested restore" claim stays current.

## Gotchas

- **Restores are never in place.** Every restore is a new instance you cut over
  to. Plan the identifier/cutover, not an in-place rewind.
- **Master password carries over.** A restored instance keeps the **source's**
  master password as of the restore point — it does **not** get a fresh secret by
  default (`MasterUserSecret` is `None` unless the source uses RDS-managed master
  password). Pass `--manage-master-user-password` on the restore if you want a new
  RDS-managed secret; otherwise connect with the source master password and rotate
  after cutover.
- **Single-AZ on restore.** Restored instances come up single-AZ regardless of
  the source; re-enable Multi-AZ after promotion.
- **`prevent_destroy` + `deletion_protection`.** The canonical instance has both
  on. Tearing it down to promote a restore requires lifting `prevent_destroy` in
  Terraform and disabling deletion protection — gate that behind a fresh
  `gb decision` (advice [[ka-0000007102]]).
- **KMS key access.** Cross-account snapshot sharing requires the CMK grant;
  for same-account restores the default CMK is already usable.
