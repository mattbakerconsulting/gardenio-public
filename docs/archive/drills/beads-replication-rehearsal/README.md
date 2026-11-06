# Beads-DB cross-account replication rehearsal (Phase 3.3)

Toolkit for the Phase 3.3 PROVE rehearsal: `pg_dump` the production `beads`
database from the shared **america-e2e** Aurora cluster and `pg_restore` it into
a **fresh** RDS instance in the new canonical-prod account (123456789012),
measuring timing and verifying integrity. This de-risks the Phase 4 beads-DB
cutover (Epic 10 `ka-0000001529`).

> **Task:** `ka-0000006830` · **Parent:** `ka-0000002725` (Phase 3 PROVE) ·
> **Blocked on:** `ka-0000006206` (Phase 3.1 — the fresh PG17 RDS target).

## HARD RULES (carried from the task)

- **RO source only.** Nothing here mutates america-e2e. `capture-baseline.sh`
  and `dump-source.sh` are read-only; `verify-target.sh` is read-only.
- **Restore is a gated data write.** `restore-target.sh` refuses to run without
  `--i-have-a-decision <resolved-gb-decision-id>`. File a **fresh** `gb decision`
  and let it resolve before every apply/restore.
- **Fresh/non-prod target only.** The restore target must be the Phase 3.1
  stand-up RDS, never production.
- **Short-lived STS in agent context** (≤12h). The cross-account terraform path
  uses the AKIA exception per advice `ka-0000007102` (account 123456789012 only).

## Source of truth (measured 2026-06-14, RO)

| Property | Value |
|---|---|
| Engine | Aurora PostgreSQL **17.7** |
| Host | `america-e2e.cluster-example0000a.us-east-1.rds.amazonaws.com` |
| Account | `234567890123` (us-east-1) |
| Database | `beads` |
| Size | **12 GB** |
| Encoding | UTF8 / en_US.UTF-8 |
| Extensions | `plpgsql 1.0`, **`vector` (pgvector) 0.8.0** |
| Vector column | `bead_embeddings.embedding` |
| Vector index | `idx_bead_embeddings_ivfflat` (ivfflat, `vector_cosine_ops`, lists=100) |

**Target parity requirement:** the Phase 3.1 RDS MUST be **PostgreSQL 17.x**
(matching 17.7 for a true 17→17 cutover) with **pgvector 0.8.0** available. A
PG16 target degrades the rehearsal to a cross-version test — a `pg_dump`/restore
will not move data to an *older* major version.

## Tooling constraint: you need a pg17 client

The source is 17.7. `pg_dump`/`pg_restore` refuse to operate on a server newer
than the client (`aborting because of server version mismatch`). The default
agent image ships pg16. Obtain a pg17 client **without root** like this (proven
on Ubuntu 24.04 "noble"):

```sh
mkdir -p /tmp/pg17 && cd /tmp/pg17
curl -fsSL -o pgc17.deb \
  "https://apt.postgresql.org/pub/repos/apt/pool/main/p/postgresql-17/postgresql-client-17_17.10-1.pgdg24.04%2B1_amd64.deb"
dpkg-deb -x pgc17.deb root
# pg17 binaries (they link the system libpq16, which is wire-compatible):
PG17=/tmp/pg17/root/usr/lib/postgresql/17/bin
"$PG17/pg_dump" --version    # -> pg_dump (PostgreSQL) 17.10
```

(The real operator runs this in-VPC where a pg17 client is installable normally.)

## Run order

```sh
export PGPASSWORD=...        # source master pw (k8s secret gardenio-postgres-password .password)
PG17=/tmp/pg17/root/usr/lib/postgresql/17/bin

# 1. Source baseline (RO). Use --snapshot for a checksum that a parallel dump
#    can match exactly on the live, mutating source.
./capture-baseline.sh --host <aurora-host> --out baseline-source.txt --snapshot

# 2. Dump (RO). Pass --snapshot <id> printed by step 1 to pin the same view.
./dump-source.sh --pg-dump "$PG17/pg_dump" --host <aurora-host> \
                 --out /data/beadsdump --jobs 4 [--snapshot <id>]

# 3. Restore into the fresh target (GATED WRITE — needs a resolved decision).
export PGPASSWORD=...        # TARGET master pw
./restore-target.sh --pg-restore "$PG17/pg_restore" --host <target-rds-host> \
                    --in /data/beadsdump --jobs 4 --create-db \
                    --i-have-a-decision ka-XXXX

# 4. Verify integrity (RO against target) — diffs counts+checksums, runs a
#    sample pgvector NN query, exits non-zero on any mismatch.
./verify-target.sh --host <target-rds-host> \
                   --source-baseline baseline-source.txt --out baseline-target.txt
```

## Integrity model

`checksum.sql` computes, per table, an **order-independent** content fingerprint:
`bit_xor` of a per-row `md5(row::text)` prefix. Because XOR is commutative the
fingerprint is invariant under the row re-ordering a dump/restore inevitably
causes, while still catching any cell-level change. Counts are exact.

**Consistency caveat:** the source is a *live, continuously-mutating* writer
(observed `beads`/`events` row counts advancing second-to-second). A baseline and
a dump taken at different instants will disagree on high-churn tables. For a
zero-RPO cutover proof, pin both to one repeatable-read snapshot
(`--snapshot`). For the Phase 4 *real* cutover, the writer fleet is drained
first, which removes the churn entirely.

## Files

| File | Role | Side-effects |
|---|---|---|
| `checksum.sql` | per-table order-independent checksum + counts | none (RO) |
| `capture-baseline.sh` | source/target baseline (version, ext, vector, checksums) | none (RO) |
| `dump-source.sh` | parallel `-Fd` dump + dump-phase timing | reads source, writes local archive |
| `restore-target.sh` | parallel restore into target + restore-phase timing | **gated write to target** |
| `verify-target.sh` | diff target vs source baseline + sample vector query | RO against target |
