# Archived RWX pipelines

Reference copies of the ACME-era RWX (rwx.com) pipeline definitions, kept as
the design spec for what the old CI did. **Nothing here runs.** RWX is not
used, no account exists, and the live `.rwx/` directories are deleted so the
RWX CLI can never treat these as pipelines again.

Every load-bearing check was proven and ported on 2026-09-16; the proof
table lives in the commit message of the port and in
`.github/known-failures.md`. Mapping:

| archived file | what it did | now |
| --- | --- | --- |
| `root/ci.yml` | build/lint/test all components, security + lint lanes, ~40 policy tests | `ci.yml` (build+test), `checks.yml` (lint, guards, terraform, helm, security, quench) |
| `root/docker.yml` | crane-built images incl. `-cover` variants and `agent-base` | `images.yml` (24 images). `-cover` variants: not ported (coverage ratchets are a D row) |
| `root/release.yml`, `root/promote.yml` | calver release DAG: ephemeral deploy → e2e → gate → promote | not ported; needs a running gardenio |
| `root/e2e.yml` | full E2E incl. gVisor substrate | not ported; needs a running gardenio |
| `root/coverage*.yml`, `coverage-baselines.yml` | coverage lanes and ratchets | not ported (D); Makefile `coverage-go` / `lcov-go` / `test-coop-coverage` remain |
| `root/helm.yml` | chart lint + publish on `v*` tags | lint in `checks.yml`; publish not ported |
| `coop/*.yml` | coop fmt/clippy/audit/deny/tests, images, GitLab push, tag releases | `ci.yml` rust job, `checks.yml` cargo-security, `images.yml` coop-claude/coopmux; releases not ported |
| `kbeads/*.yml` | kbeads lint/test, image, releases | `ci.yml` go job, `checks.yml` lint-go, `images.yml` kbeads; releases not ported |
| `*/README-LOCKS.md` | documented the lock files | locks now live in `gardenio/locks/` |
| `root/release.sh`, `root/release_test.sh` | the `make release` cut: calver tag + RWX preflight/mirror dispatches | not ported; `make release` prints a pointer here and exits 1 |
| `rwx-agent-guide.md` | how agents used the RWX CLI | obsolete; kept for context |

The ~30 root pipelines not archived (deploy/teardown/drift alarms/mirror/
fix-* one-offs) targeted ACME's cluster, GitLab and RWX vaults and have no
successor.

## Archived docs (`docs/`)

Docs that were wholly about RWX, moved here on 2026-11-01 (gd-rLdZ1MGGbo) under
their old path. Links inside them that pointed at live docs were repointed.

| archived file | was at | what it was |
| --- | --- | --- |
| `docs/docs/ci-toolchain-checksums.md` | `docs/` | byte pins for the `rwx` CLI downloads in `.gitlab-ci.yml` and `.rwx/*.yml` |
| `docs/docs/design/full-release-dag.md` | `docs/design/` | the RWX release-DAG redesign (ka-0000006850) |
| `docs/gardenio-docs/coverage.md` | `gardenio/docs/` | the RWX coverage pipeline (`.rwx/coverage*.yml`); the coverage tools stay for gd-d29n0m678l |
| `docs/gardenio-docs/deploy-single-source-of-truth.md` | `gardenio/docs/` | the `.rwx/deploy.yml` EKS deploy path, superseded by `releases-design.md` |
| `docs/gardenio-docs/e2e-flake-report.md` | `gardenio/docs/` | the last flake table from the RWX Captain API (2026-07-29) |
| `docs/gardenio-docs/lol-*.md` (4) | `gardenio/docs/` | the Live Oak Labs RWX-org port: billing decision, dry-run audit, release port, provisioning |
| `docs/gardenio-docs/stale-namespace-reaper-scope-decision.md` | `gardenio/docs/` | scope of the `rwxReaper` CronJob (removed by #1707) |
| `docs/gardenio-docs/research/ephemeral-rwx-e1-literal-audit.md` | `gardenio/docs/research/` | `gardenio-rwx` literal audit for per-instance namespaces |
| `docs/gardenio-plans/ephemeral-rwx-smoketests.md` | `gardenio/plans/` | plan of record for per-instance `gardenio-rwx-<id>` smoke namespaces |
