# Research: Replace RDS with In-Box Postgres

**Bead:** ka-0000001956 · **Parent epic:** ka-0000001295 (Gardenio in a Box)
**Author:** gardenio-research-replace-rds-j79 (crew) · **Date:** 2026-06-08

## TL;DR / Recommendation

**Use the chart's existing in-box `templates/postgres` StatefulSet (single replica,
`pgvector/pgvector:pg17`, RWO PVC). Do NOT adopt Bitnami or CloudNativePG for the
single-EC2 "Gardenio in a Box" target.** The StatefulSet path already exists, already
wires every DSN consumer (beads daemon, migrate-Job, litellm), and matches the
single-node constraint. It needs **three small fixes** to be self-sufficient with
zero external cloud deps:

1. **Chart-managed password Secret** for the in-box path (today the in-box password
   Secret is created *only* by ExternalSecrets → AWS Secrets Manager — a hard cloud
   dependency). Add a `lookup`-stable `randAlphaNum 32` Secret exactly like the
   existing `external-rds-credentials.yaml` pattern.
2. **A local backup/restore story** — a `pg_dump` CronJob to a PVC (or hostPath),
   plus a documented restore. There is **no** in-box backup today; the only backup
   mechanism in the chart is `VolumeSnapshotClass` with `driver: ebs.csi.aws.com`
   (cloud-only).
3. **litellm database creation** (only if litellm is enabled in-box) — the StatefulSet
   `initdb` creates a single `POSTGRES_DB=beads`; the litellm DSN points at a separate
   `litellm` database that nothing creates in the in-box path.

CloudNativePG is the better answer **if/when** HA, automated PITR, or in-cluster
object-store backups become requirements — but that is explicitly out of scope for a
single-EC2, single-node box (epic ka-0000001295: "single node, no autoscaling").

---

## 1. Current state — what's actually in the repo

### 1.1 AWS RDS (today's production path)
`gardenio/terraform/modules/rds/` provisions **Amazon RDS for PostgreSQL 16**, Multi-AZ
instance (not Aurora), gp3 encrypted storage, RDS-managed master password in Secrets
Manager, 7-day automated backups, deletion-protection, enhanced monitoring + Performance
Insights. pgvector is enabled at runtime via `CREATE EXTENSION vector` (migration
`005_bead_embeddings.up.sql`), not in `shared_preload_libraries`.

This is the cloud surface the epic wants to eliminate: RDS itself + Secrets Manager
(master password) + KMS CMK + CloudWatch log export.

### 1.2 The chart ALREADY has in-box Postgres
`gardenio/helm/gardenio/templates/postgres/`:

| File | Role |
|---|---|
| `statefulset.yaml` | 1-replica StatefulSet, `pgvector/pgvector:pg17`, RWO `volumeClaimTemplate`, hardened securityContext (runAsNonRoot 999, readOnlyRootFS, drop ALL caps, `pg_isready` probes), `PGDATA=/var/lib/postgresql/data/pgdata` |
| `service.yaml` | ClusterIP on 5432 |
| `external-rds-credentials.yaml` | per-instance role password Secret (only when `beads.externalRDS.enabled`) |
| `external-rds-provision-job.yaml` | pre-install/upgrade hook that creates DB+role+`vector` on a *shared* external RDS |

Gated by `postgres.enabled` (default **false** — prod uses external RDS). Values block
at `values.yaml:2066`: `pgvector/pgvector:pg17`, `database: beads`, `persistence.size:
10Gi`, modest resources (250m/256Mi → 1cpu/1Gi).

> **Image note (load-bearing):** the comment at `values.yaml:2069-2073` records the
> 2026-04-22 silent-deploy outage (ka-0000002109 / ka-0000002010): vanilla
> `postgres:17-alpine` ships **no** pgvector and broke migration 005. Must stay on
> `pgvector/pgvector` (or Bitnami). Any swap must preserve pgvector.

### 1.3 DSN wiring — four prioritized branches (`_helpers.tpl:362`)
`gardenio.beads.databaseURL` resolves in this order:
1. `beads.databaseURL` (explicit override)
2. `beads.externalRDS.enabled` → shared-RDS, per-instance scoped role (ephemeral E2E)
3. `postgresExternal.enabled` → generic external master creds
4. `postgres.enabled` → **in-box**: `postgres://postgres:$(BEADS_DATABASE_PASSWORD)@<release>-postgres:5432/beads?sslmode=disable`

The beads `deployment.yaml` and `migrate-job.yaml` both select the matching
`BEADS_DATABASE_PASSWORD` secretKeyRef per branch. litellm has a parallel helper
(`gardenio.litellm.databaseURL`, `_helpers.tpl:705`) pointing at a separate `litellm`
database on the same in-box service.

### 1.4 Migration path is already decoupled and in-box-friendly
`templates/beads/migrate-job.yaml` runs `kd migrate` as a Helm hook **before** the
daemon rolls (pre-upgrade always; pre-install added only for the external paths).
Rationale baked in: a 313K-row backfill once killed the daemon's 60s startup probe
(ka-0000006380). The daemon still self-migrates on boot (`postgres.go:161 m.Up()`),
idempotent → sub-second `ErrNoChange` after the Job. `BEADS_MIGRATE_AUTO_RECOVER`
defaults to `last-clean`. **This works as-is for in-box** — no changes needed for the
migrate story itself.

---

## 2. Options assessed

### Option A — Existing StatefulSet on local PV ✅ RECOMMENDED
- **Single-node persistence:** RWO PVC via `volumeClaimTemplate`. On the single-EC2
  box this binds to whatever the in-box StorageClass is (local-path-provisioner /
  hostPath — owned by sibling research ka-0000005525 "replace EBS/EFS CSI"). `PGDATA`
  is a subdir of the mount, correct for `lost+found` avoidance.
- **Conn-string wiring:** already complete (branch 4 above). Zero new helper work.
- **migrate-Job:** already wired and in-box-correct.
- **Effort:** lowest. It's the path the chart was built to support. Three gaps (§3).
- **Fit to epic:** perfect — single replica, no operator, no CRDs, no extra cloud.

### Option B — Bitnami postgresql subchart ❌
- Pros: ships pgvector (the outage workaround historically pointed here), mature,
  configurable backups via sidecar.
- Cons: **adds a subchart dependency** (Chart.yaml `dependencies:` + `helm dep build`
  → needs registry access at package time, friction for air-gapped box). Re-implements
  what `templates/postgres` already does. Bitnami's 2024 image-catalog changes
  (Bitnami Secure Images / docker.io rate limits) make the image pull a *new* external
  dep — directly against the epic's "no external registry pull as a hard dep" goal
  (sibling ka-0000005069). Net: more moving parts, no benefit over Option A on a single
  node. **Reject.**

### Option C — CloudNativePG operator ⚖️ (right tool, wrong scale here)
- Pros: best-in-class — declarative `Cluster` CR, automatic failover, **continuous
  backup to object store + PITR**, `Backup`/`ScheduledBackup` CRs, in-place minor
  upgrades, connection pooling. pgvector via a custom image.
- Cons: requires installing the **CNPG operator** (cluster-wide CRDs + controller
  Deployment) — another component to bundle, image, and run on a 1-node box. Its
  headline features (HA failover, multi-instance) are **moot at replicas=1**. Backups
  want an object store (MinIO) → couples to the S3-replacement research (ka-0000004744),
  adding a dependency chain for a box whose backup need is "a nightly dump I can scp
  off the instance."
- **Verdict:** Over-engineered for "Gardenio in a Box." **Recommend as the documented
  upgrade path** for any future multi-node / HA gardenio install, not for the single-EC2
  target. Worth a one-line note in the install doc so the choice is intentional.

---

## 3. Gaps to close for a true zero-cloud in-box Postgres

### Gap 1 — Password Secret has a hidden cloud dependency (BLOCKER)
The in-box `statefulset.yaml` reads its password from
`postgres.passwordSecret | default <release>-postgres-password`. That Secret is created
**only** by `external-secrets.yaml` (ExternalSecret → AWS Secrets Manager). With
`externalSecrets.enabled=false` and `postgres.enabled=true`, **no Secret exists** and
the StatefulSet + daemon fail to start. There is no chart-managed plaintext fallback
for the in-box path.

**Fix:** add a `templates/postgres/credentials.yaml` Secret rendered when
`postgres.enabled && not externalSecrets.enabled`, using the proven idempotent pattern
from `external-rds-credentials.yaml`: `helm.sh/resource-policy: keep` +
`lookup`-then-`randAlphaNum 32` (helper already exists in spirit at `_helpers.tpl:445`).
Must emit both `password` and `password_url` keys (the deployment/migrate-Job choose
`password_url` only under `externalSecrets.enabled`, so plain `password` is correct for
the in-box branch — verify the keyref in `migrate-job.yaml:75-77`). Small, well-trodden.

### Gap 2 — No in-box backup/restore (BLOCKER for acceptance)
Only backup mechanism in the chart is `volumesnapshotclass.yaml` with
`driver: ebs.csi.aws.com` — cloud-only. The epic's acceptance ("no AWS API at runtime")
forbids it.

**Fix (minimal, fits single EC2):** add an opt-in `pg_dump` CronJob
(`templates/postgres/backup-cronjob.yaml`, gated `postgres.backup.enabled`) writing
timestamped `pg_dump -Fc` archives to a dedicated PVC (or a hostPath the operator can
rsync/scp off-box), with retention pruning. Document `pg_restore` for recovery. Reuse
the `pgvector/pgvector:pg17` image (psql/pg_dump already present) so no new image to
bundle. This is the right altitude for a single-node box; PITR/WAL archiving is a CNPG
concern deferred to the HA upgrade path.

### Gap 3 — litellm database not created in-box (conditional)
Only relevant if litellm runs in-box (`litellm.enabled` default false). The StatefulSet
`initdb` makes one DB (`POSTGRES_DB=beads`); `gardenio.litellm.databaseURL` points at a
separate `litellm` DB nothing creates. **Fix:** if litellm is enabled with in-box
postgres, add a tiny init step (initContainer or one-shot Job) running
`CREATE DATABASE litellm` — mirror the provision-job's idempotent `CREATE DATABASE IF
NOT EXISTS` logic. Track as a follow-up only when in-box litellm is in scope.

---

## 4. Recommendation summary

| Concern | Decision |
|---|---|
| Engine | **Existing StatefulSet**, `pgvector/pgvector:pg17`, replicas=1 |
| Bitnami subchart | Reject — extra dep + image-pull cloud coupling, no single-node benefit |
| CloudNativePG | Defer — document as the HA/multi-node upgrade path only |
| Persistence | RWO PVC via in-box StorageClass (depends on ka-0000005525) |
| Conn-string | Already wired (branch 4) — no work |
| migrate-Job | Already wired & decoupled — no work |
| **Gap 1 password Secret** | **Add chart-managed `lookup`+`randAlphaNum` Secret** |
| **Gap 2 backup/restore** | **Add `pg_dump` CronJob → PVC/hostPath + restore doc** |
| Gap 3 litellm DB | Add `CREATE DATABASE litellm` init *iff* in-box litellm enabled |

### Suggested follow-up implementation beads (children of ka-0000001295)
1. `feat(gardenio/chart): chart-managed in-box postgres password Secret` (Gap 1) — small, unblocks the whole in-box path.
2. `feat(gardenio/chart): in-box postgres pg_dump backup CronJob + restore runbook` (Gap 2).
3. `feat(gardenio/chart): create litellm DB on in-box postgres when litellm enabled` (Gap 3, conditional).
4. `docs(gardenio): single-EC2 postgres choice + CNPG upgrade path note`.

### Cross-references to sibling research
- **ka-0000005525** (EBS/EFS CSI → local-path) — owns the StorageClass the PVC binds to.
- **ka-0000005069** (image distribution w/o external registry) — must bundle `pgvector/pgvector:pg17`.
- **ka-0000004744** (S3 → object store) — *only* relevant if a future CNPG backup-to-MinIO path is chosen; the recommended `pg_dump`-to-PVC backup avoids that coupling.
