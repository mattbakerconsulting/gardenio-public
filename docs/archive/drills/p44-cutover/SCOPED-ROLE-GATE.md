# P4.4 Gate-2 — dest scoped DB role (advice ka-0000004647)

> Operator (Matt Baker, 2026-06-30) elevated "daemon connects as a scoped
> `gardenio_*` role, never the RDS master" to a **must-clear gate BEFORE the
> step-D data write**. This doc is the decision-ready artifact for that gate.
> It is grounded in live RO recon of dest `prod-gardenio-postgres` (acct 133)
> and in the adversarial review NO-GO finding B5 (ka-0000008220).

## TL;DR — the safe shape

**Pre-cut (this gate):** create the LOGIN ROLE + seed its password Secret ONLY.
The daemon KEEPS connecting as master `gardenio` through step D and step E.
**Post-cut (separate, reversible gate):** transfer object ownership to the
scoped role and re-point the daemon. Do NOT re-point the daemon to the scoped
role during the frozen cutover window.

Re-pointing the daemon to a scoped role *before* a verified-good restore is
**net-risk-ADDING**, not risk-reducing — see "Why not before the cut".

## Live recon (RO, 2026-06-30, prod-gardenio-postgres db=gardenio)

| Fact | Value |
|---|---|
| daemon connection identity | `gardenio` = **RDS master** (member of `rds_superuser`, `createrole`/`createdb`) via `BEADS_DATABASE_URL=postgres://gardenio:***@.../gardenio` |
| login roles present | ONLY `gardenio` (+ `rdsadmin`, RDS-internal). **No scoped role exists.** |
| db `gardenio` owner | `gardenio` (master) |
| public schema owner | `pg_database_owner` (PG15+ — the DB owner implicitly owns schema public) |
| `vector` extension | installed 0.8.1, owner `rdsadmin`, already present |
| can master grant `rds_superuser`? | **NO** — `gardenio` lacks `admin_option` on `rds_superuser` |
| daemon password secret | K8s `gardenio-postgres-external-password` (`password` + `password_url`), ESO-synced from acct-133 SM |

## Why not before the cut (review finding B5 — VERIFIED in code)

The restore is `pg_dump -Fd --no-owner --no-acl`, restored by the master. With
`--no-owner`, **every** restored object (all 19 tables, 6 sequences, the
`beads_search_tsv` trigger function, indexes) is owned by **master `gardenio`**.
A scoped *non-owner* role grafted on afterwards:

1. **CrashLoops the daemon on boot.** `postgres.New() → runMigrations() →
   postgres.WithInstance()` runs on EVERY boot (kbeads `postgres.go:60`) and
   issues `CREATE TABLE IF NOT EXISTS schema_migrations` — which requires
   `CREATE` on schema `public`. On PG15+ `public` no longer grants `CREATE` to
   `PUBLIC`, so a scoped role without an explicit `GRANT ... ON SCHEMA public`
   dies at WithInstance, *before* the no-op `m.Up()`. Presents as
   CrashLoopBackOff — observed blind (kd/gb frozen) via Slack only.
2. **Breaks the next schema-040 migration.** `ALTER TABLE` / `CREATE INDEX` /
   `DROP` require **ownership** (or superuser); `GRANT ALL ON TABLE` confers
   only DML (SELECT/INSERT/UPDATE/DELETE/…), NOT DDL. And the migrate-job uses
   the **same** `BEADS_DATABASE_URL` as the daemon, so swapping the daemon to a
   non-owner also swaps the migrator → 040 dies in the pre-upgrade hook.
3. **Gives a false-green verify.** `verify-target.sh` runs as **master**, so a
   PASS attests data-under-master, not the scoped role's ability to operate. A
   PASS would trigger `scale dest → 1`, then the daemon CrashLoops after source
   is already frozen.

Doing the split before the cut converts a boot that today is a **proven no-op**
into an untested config change executed inside the irreversible window. The
source is the rollback net regardless, so pre-cut re-pointing buys nothing.

## Gate-2 (pre-cut) — what to actually do

Run as **master**, out-of-band SQL (NOT a `beads.externalRDS` chart flip — its
`roleName` default == databaseName == `gardenio` == the master, a footgun). Keep
`beads.externalRDS.enabled=false`; keep `postgresExternal` shape unchanged.

```sql
-- Idempotent. Run as master 'gardenio' against db postgres (or gardenio).
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='gardenio_app') THEN
    CREATE ROLE gardenio_app WITH LOGIN PASSWORD :'scoped_pw';
  ELSE
    ALTER ROLE gardenio_app WITH LOGIN PASSWORD :'scoped_pw';
  END IF;
END $$;
```

Then seed the scoped password in acct-133 SM (e.g.
`gardenio/prod/beads-scoped-db-password`) + a dest ExternalSecret, so the
post-cut re-point is a one-line `postgresExternal.username`/passwordSecret flip.
**The daemon stays on master through step D and step E.**

## Post-cut (SEPARATE reversible gate) — transfer ownership + re-point

Only AFTER a master-owned restore has verified PASS and the daemon is confirmed
healthy on master:

Run `scoped-role-transfer.sh` (the codified, idempotent version of the SQL
below). The SQL, as executed:

```sql
-- Run as master. Transfer ownership of the application objects so the scoped
-- role can run future migrations (ALTER/CREATE INDEX) and WithInstance boot.
-- lock_timeout (set via PGOPTIONS in the script) makes each ALTER fail fast
-- rather than block the LIVE daemon's DML while it waits for the lock.
ALTER DATABASE gardenio OWNER TO gardenio_app;   -- + owns schema public via pg_database_owner
-- Per-object owner transfer (prefer explicit over blanket REASSIGN OWNED, which
-- would also try to move the master's catalog grants):
DO $$DECLARE r record; BEGIN
  FOR r IN SELECT tablename FROM pg_tables WHERE schemaname='public' LOOP
    EXECUTE format('ALTER TABLE public.%I OWNER TO gardenio_app', r.tablename); END LOOP;
  FOR r IN SELECT sequence_name FROM information_schema.sequences WHERE sequence_schema='public' LOOP
    EXECUTE format('ALTER SEQUENCE public.%I OWNER TO gardenio_app', r.sequence_name); END LOOP;
  FOR r IN SELECT table_name FROM information_schema.views WHERE table_schema='public' LOOP
    EXECUTE format('ALTER VIEW public.%I OWNER TO gardenio_app', r.table_name); END LOOP;
  -- CRITICAL: APP functions only. Schema public ALSO holds the `vector`
  -- extension's functions (118 on prod, owned by rdsadmin). Re-owning those to
  -- gardenio_app is wrong (extension objects belong to the extension owner) — so
  -- exclude extension members via pg_depend deptype='e'. An unfiltered loop
  -- silently drags all 118 vector functions onto gardenio_app.
  FOR r IN SELECT p.proname, pg_get_function_identity_arguments(p.oid) a
           FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
           WHERE n.nspname='public'
             AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.objid=p.oid AND d.deptype='e') LOOP
    EXECUTE format('ALTER FUNCTION public.%I(%s) OWNER TO gardenio_app', r.proname, r.a); END LOOP;
END$$;
GRANT ALL ON SCHEMA public TO gardenio_app;
```

`CREATE EXTENSION IF NOT EXISTS vector` is a **no-op** for any role because the
extension is already installed (owner `rdsadmin`) — so the inability to grant
`rds_superuser` does NOT block boot. A FUTURE migration that adds a *new*
extension would need rds_superuser; if/when that lands, either keep the
migrate-job on master (split migrate-role from runtime-role) or grant the role
the needed RDS capability. Note in the gate decision.

**Verify the scoped role BEFORE re-pointing the live daemon.** The script's
STEP 2 does this two ways (`PROBE_MODE`):
- `template` (default) — on a throwaway `CREATE DATABASE … TEMPLATE gardenio`
  copy; requires the daemon at 0 replicas (no FORCE on TEMPLATE copy).
- `live` — connect AS `gardenio_app` **directly to the live DB** and run the
  exact WithInstance preflight **non-destructively** (create+drop a throwaway
  probe table; `CREATE TABLE IF NOT EXISTS schema_migrations` no-op; `SELECT
  count(*) FROM beads`). Use this when the DB is a live control plane (kd/gb's
  own store) that cannot be scaled to 0 — it verifies the exact target DB while
  the daemon stays UP.

Only after a GREEN probe, flip `postgresExternal.username` to `gardenio_app` +
its password secret and roll the daemon (see the re-point overlay
`scripts/phase32-load-harness/day0-wiring/scoped-role-repoint-values.yaml`).

### Executed on prod (ka-0000001170, 2026-08-06)

STEP 1 + STEP 2 ran live against `prod-gardenio-postgres` db=`gardenio`:
- DB + **19 tables / 6 sequences / 1 view / 12 app functions** → `gardenio_app`;
  0 objects left master-owned. The **118 `vector` functions stayed on
  `rdsadmin`** (deptype='e' filter). `GRANT ALL ON SCHEMA public` → `gardenio_app`
  has CREATE on public.
- Boot probe AS `gardenio_app` (PROBE_MODE=live) **PASS**: CREATE/DROP throwaway
  table, schema_migrations no-op, `SELECT count(*) FROM beads` = 603,544.
- **NON-DISRUPTIVE**: the live `gardenio-beads` daemon kept serving on **master**
  with 0 restarts throughout — master `gardenio` is a member of `rds_superuser`,
  so it retains full DML on every object even after ownership moves. This is the
  reversibility net: the daemon runs healthy on master regardless of who OWNS
  the objects.

The remaining step — re-pointing the daemon to `gardenio_app` — is a prod
`terraform apply -target=module.gardenio` (operator/gardener-release-owner gated);
the overlay is wired **commented-out** in `prod-real.tfvars`, uncommented only
after this GREEN.

### Reversibility (the flip-back)

Fully reversible at two independent layers:
1. **Daemon identity** (the thing that matters for health): re-comment the
   re-point overlay in `prod-real.tfvars` (or override `username` back to
   `gardenio` + master secret) and re-`apply`; the daemon rolls back onto master.
   Healthy **regardless** of object ownership (master is rds_superuser).
2. **Object ownership**: `scoped-role-revert.sh` (APPLY=1) moves the DB +
   schema-public objects back to master `gardenio`. Optional — only needed to
   return to the exact pre-transfer ownership state; the daemon does not require
   it to run on master. Both ownership directions were verified reversible in
   rolled-back transactions (ka-0000001170).
