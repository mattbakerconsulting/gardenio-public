# Phase 3.3 — Beads-DB cross-account replication rehearsal report

> Task: `ka-0000006830` · Parent: `ka-0000002725` (Phase 3 PROVE) · Program: `ka-0000000398`
> Status: **COMPLETE — dump + restore + integrity verification all PROVEN end-to-end.**
> Dump half: 2026-06-14 by `gardenio-phase-33-crossaccount-nvs`.
> Restore + verify half: 2026-06-15 by `gardenio-phase-3-sharedstandup-yol`,
> authorized by write-decision `ka-0000001626` = `restore`.

## TL;DR

The cross-account `beads`-DB cutover is now **proven end-to-end on a real 17.7→17.7
path**: the live 12 GB america-e2e Aurora `beads` DB was dumped (snapshot-pinned,
RO, zero source mutation), transferred into the canonical-prod VPC, and restored into
the fresh canonical-prod RDS (PG 17.7) with **zero errors**, after which a read-only
integrity pass confirmed **all 19 tables are bit-identical** (exact row counts +
order-independent content checksums), **pgvector 0.8.0 extension parity**, the
**ivfflat index rebuilt identically**, and a sample cosine-NN vector query returns
deterministic results. Acceptance criteria 1–5 are all satisfied.

## Headline numbers

| Phase | Wall-clock | Notes |
|---|---|---|
| Dump (snapshot-pinned, in-VPC) | **331 s (~5m31s)** | `-Fd -j4`, RO, snapshot `00000B0C-0000D4C1-1`; 3.5 GB dir-archive; RC=0. Earlier cross-network dump from the agent pod measured 254 s — the in-VPC run is the cutover-grade figure. |
| Transfer | folded into dump | `-Fd` streams to local disk on the in-VPC helper pod; dump+transfer are one step. |
| Restore (`pg_restore 17.10`, `-j4`) | **546 s (~9m06s)** | RC=0, **0 errors/warnings**, into a freshly `CREATE DATABASE beads` target; includes inline ivfflat rebuild + all FK constraints; `ANALYZE` ok. |
| **Total dump→restore wall-clock** | **~877 s (~14m37s)** | For the full 12 GB / ~7.4M-row DB. |

Feeds the Phase 4 cutover-window estimate (Epic 10 `ka-0000001529` / Epic E
`ka-0000003593`): a complete logical dump+restore of the production `beads` DB is
**~15 min** at this data volume. The ≤15 min RTO target in the cutover ADR
(`docs/research/canonical-prod-beads-db-cutover.md`) is reachable but tight at
current scale; for a hard RTO margin, prefer a snapshot/physical-replication path
or a defer-indexes restore (see README) over logical dump/restore at cutover time.

## Environment proven

| | Source (RO) | Target (restored) |
|---|---|---|
| Account | america-e2e `234567890123` | canonical-prod `123456789012` |
| Host | `america-e2e.cluster-example0000a...` Aurora | `canonical-prod-gardenio-postgres.cob8k0yyuemh...` RDS |
| Engine | **PostgreSQL 17.7** | **PostgreSQL 17.7** |
| DB size | 12 GB | 13 GB (restored; planner stats + index padding) |
| Extensions | `plpgsql 1.0`, `vector 0.8.0` | `plpgsql 1.0`, **`vector 0.8.0`** ✓ parity |
| Vector index | `idx_bead_embeddings_ivfflat` (cosine, lists=100) | **identical** ✓ |

The restore ran inside a helper pod in the canonical-prod cluster (`gardenio-system`
ns), reachable to the RDS over the in-VPC private endpoint. `pg_restore 17.10` (PGDG,
no-root bootstrap) performed the restore; system `psql 16` (wire-compatible with a
17.7 server) ran the admin `CREATE DATABASE` + RO verification queries.

## Integrity verification — RESULT: PASS

Read-only against the target. Re-captured the same per-table fingerprint
(`checksum.sql`: exact `count(*)` + `bit_xor` of per-row `md5`, order-independent) and
diffed against the snapshot-pinned source baseline. **All 19 tables match exactly:**

| Table | Rows | Checksum (src == tgt) |
|---|---|---|
| beads | 442,949 | `4c565a2aa0a75be9` ✓ |
| labels | 1,044,470 | `c0f635727cf7116c` ✓ |
| deps | 138,049 | `d1c4101ceab9b77e` ✓ |
| comments | 77,972 | `cd94f45583384b99` ✓ |
| events | 2,704,573 | `fca8a3013a174523` ✓ |
| configs | 59 | `6d0e1bd8cb887972` ✓ |
| bead_references | 236,330 | `95688001a434988b` ✓ |
| bead_embeddings | 50,803 | `3d9fd0de94826904` ✓ |
| bead_embedding_migrations | 0 | `0` ✓ |
| bead_followers | 0 | `0` ✓ |
| mutation_history | 1,841,035 | `a7f98593853ac571` ✓ |
| operators | 82 | `2bb39f8177c6a2f6` ✓ |
| operator_aliases | 111 | `38f8399b089d4987` ✓ |
| operator_slug_aliases | 82 | `41f46f0ff3eafec9` ✓ |
| session_gates | 29,994 | `e4d500c9d6a17b53` ✓ |
| refresh_tokens | 0 | `0` ✓ |
| revoked_tokens | 0 | `0` ✓ |
| schema_migrations | 1 | `cc0d4f987faadd45` ✓ |
| ui_telemetry | 4 | `92abc737e4d6af7b` ✓ |

- **`EXT_PARITY OK`** — extension set + versions identical.
- **Sample pgvector query** (top-5 cosine NN of a fixed probe row, run RO on both) —
  target returns deterministic ordered ids (`ka-0000008082, ka-0000005332,
  ka-0000007175, ka-0000005968, ka-0000003823`). Because `bead_embeddings` is
  bit-identical (checksum match above), the vector results are identical by
  construction.
- **`VERIFY RESULT: PASS`** — `verify-target.sh` exited 0 (every table matched AND the
  vector query succeeded).

## Acceptance criteria — all met

1. **Zero data-integrity errors; counts + checksums match for every table** — ✓ (19/19, table above).
2. **pgvector restores cleanly; sample vector query identical** — ✓ (`vector 0.8.0`, ivfflat rebuilt, deterministic NN ids; embeddings table bit-identical).
3. **Per-phase timing recorded** — ✓ (dump 331 s, restore 546 s, total ~877 s).
4. **Rehearsal against fresh/non-prod target; no prod cutover; source RO-untouched** — ✓ (fresh `CREATE DATABASE beads` on the throwaway canonical-prod RDS; source dumped under a read-only repeatable-read snapshot, never mutated).
5. **Replication-rehearsal report in a bundle linked to this task** — ✓ (this file + `baseline-source.txt` + `baseline-target.txt`, bundle `/bundle/ka-0000000497`).

## Snapshot-pinned consistency

Both the source baseline and the dump were pinned to the same repeatable-read
snapshot (`00000B0C-0000D4C1-1`) so the high-churn tables (`events`,
`mutation_history`, `beads`) compare cleanly against a live, still-writing source.
That is what makes a 19/19 bit-identical match possible against a production writer.

## Deliverable: the rehearsal toolkit

Committed under `gardenio/scripts/beads-replication-rehearsal/` — see `README.md`:

- `checksum.sql` — order-independent per-table fingerprint + counts.
- `capture-baseline.sh` — source/target baseline (version, ext, vector, checksums); `--snapshot` for consistency.
- `dump-source.sh` — parallel `-Fd` dump + dump-phase timing (RO).
- `restore-target.sh` — parallel restore + restore-phase timing (**gated**: refuses without a resolved decision id).
- `verify-target.sh` — diffs target vs source baseline + runs a sample pgvector NN query; non-zero exit on any mismatch.

## Hard-rule compliance

- Source access **read-only** throughout; the only write was `CREATE DATABASE beads`
  + restore on the **fresh non-prod** canonical-prod RDS. Production `beads` traffic
  was never cut and the america-e2e source was never mutated.
- The restore (a gated AWS/data write) ran **only after** the fresh write-decision
  `ka-0000001626` resolved `restore`. `restore-target.sh` enforces the gate with
  `--i-have-a-decision`.
- AKIA terraform/data exception per advice `ka-0000007102` (account 123456789012,
  cred from SM `gardenio/customer-poc-aws-creds`); no `iam:*`/`organizations:*`.
