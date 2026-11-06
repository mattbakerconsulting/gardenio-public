# Postgres + pgvector for the e2e CI jobs

`viewer-e2e` and `go-integration` both need a PostgreSQL 16 with the `vector`
extension available. `kbeads/internal/store/postgres/migrate_preflight.go:44-69`
queries `pg_available_extensions` and hard-fails the migration without it;
migration `005_bead_embeddings` is the consumer.

Two mechanisms were tried on `gardenio-k3s` (ARC, `containerMode: dind`) on
2026-09-22 under gd-gKwtyIafHc.

## DECIDED: RECIPE-SERVICES

Actions service containers work fine under this ARC scale set's dind mode.
The Docker daemon available to the job pulls and starts the service like any
other GitHub-hosted runner would; there is no special dind port-mapping
problem to route around.

```yaml
    services:
      postgres:
        image: pgvector/pgvector:pg16
        env:
          POSTGRES_USER: beads
          POSTGRES_PASSWORD: beads
          POSTGRES_DB: beads
        ports:
          - 5432:5432
        options: >-
          --health-cmd "pg_isready -U beads"
          --health-interval 5s
          --health-timeout 5s
          --health-retries 20
    steps:
      - name: psql client
        run: sudo apt-get update -q >/dev/null && sudo DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=a apt-get install -y -q --no-install-recommends postgresql-client
      - name: create extension and prove it
        env:
          PGPASSWORD: beads
        run: |
          set -euo pipefail
          psql -h localhost -U beads -d beads -v ON_ERROR_STOP=1 \
            -c "CREATE EXTENSION IF NOT EXISTS vector;"
          psql -h localhost -U beads -d beads -tAc \
            "SELECT default_version FROM pg_available_extensions WHERE name='vector';"
          psql -h localhost -U beads -d beads -tAc \
            "SELECT extversion FROM pg_extension WHERE extname='vector';"
```

Both `SELECT` queries printed `0.8.6` (the version `pgvector/pgvector:pg16`
ships as of this spike) — [run 35676706800](https://github.com/mattbakerconsulting/gardenio/actions/runs/35676706800).

Setup cost: ~20s (image pull + health check ~13s, `postgresql-client` apt
install ~6.5s, `CREATE EXTENSION` + both proof queries <1s). Whole job
completed in 23s wall-clock.

## REJECTED: RECIPE-APT

Not run — the services path worked on the first attempt, and an image pull
beats an apt install on every run (no PGDG source juggling, no
`pg_ctlcluster` bring-up, fewer packages). Kept here as the documented
fallback in case the services path ever regresses (e.g. a future ARC/dind
change that breaks service-container networking):

```bash
sudo apt-get update -qq
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
  postgresql-16 postgresql-16-pgvector >/dev/null
sudo pg_ctlcluster 16 main start
for i in $(seq 1 30); do
  if sudo -u postgres pg_isready -q; then break; fi
  sleep 1
done
sudo -u postgres psql -v ON_ERROR_STOP=1 <<'SQL' >/dev/null
CREATE USER beads WITH SUPERUSER PASSWORD 'beads';
CREATE DATABASE beads OWNER beads;
\connect beads
CREATE EXTENSION IF NOT EXISTS vector;
SQL
```

If `postgresql-16`/`postgresql-16-pgvector` are ever missing from the
mirrored Ubuntu 24.04 archive, add the PGDG apt source first — the repo
already pins PGDG client debs by sha (`PGDG_PG17_CLIENT_DEB_SHA256_AMD64` in
`gardenio/images/agent/install/checksums.env`), so extend that pin rather
than improvising apt sources in the job.

Setup cost: not measured (path not exercised).

## The DSN both jobs use

    postgres://beads:beads@localhost:5432/beads?sslmode=disable

`viewer-e2e` passes it as `BEADS_DATABASE_URL`; `go-integration` passes it as
`BEADS_TEST_DATABASE_URL`. Changing it means changing both jobs.
