# Known-excluded tests and images

## Quarantine: the known-failing tests list (owner rule 2026-11-01)

The owner, 2026-11-01: "the goal should be to have a list of failing tests;
no reason for multiple MRs to be blocked from merging on the same failing
test, ever." The list is `.github/quarantine.d/`: one file per row, named
`<kind>-<name>.tsv` (`go-TestFoo.tsv`, `golden-land-phone-dark.png.tsv`), so two
PRs that add or remove different rows never conflict (gd-3lWbWYIFoT). A test
that has failed one PR, or main, gets a row, and from then on it fails no other
PR.

**CI reads main's copy, not the PR's.** `scripts/quarantine.sh fetch` fetches
`origin/main` and concatenates the files there into one TSV. It falls back to
the checkout's copy, with a warning, only when main cannot be fetched. It
still reads a `.github/quarantine.tsv` left on main by a writer on an older
prompt, and warns until its rows move into files. So a row landed on
main takes effect in every PR's next run, and no open PR has to be touched.
A tag (or a main-head run) also keeps the rows of its own tree that main has
since removed: a fix and its row's removal land together, so a tag cut before
the fix still needs the row. A PR's own rows never count.
A `workflow_dispatch` run of a branch drops the rows that branch deleted (in its
merge-base with main, not in its HEAD), so a fix's proof run runs the test under
repair; a row main added after the branch was cut still applies, and a PR run
always reads main's list whole.
A change to the list alone lands as its own ready PR, never a direct push:
main's ruleset requires `ci` and `checks` on every update, no push to main
runs either workflow since 2026-11-03, and the row matches no path filter,
so the PR's run is light and green in about three minutes (gd-iSYkWs402D).

**The bar is low, on purpose.** A Go test or golden shot needs a row when it
fails once on a change that could not have caused the failure: it fails on
main, on a PR that does not touch its code, or on two PRs. Don't re-run the
job to see if it goes green first. A flaky test costs every PR that hits it,
and one row costs nothing.

**Flaky tests are quarantined, never re-run (owner rule 2026-11-05).** The owner:
"Flaky tests should be quarantined, we should add a rule." This holds on a release tag and
the nightly as well as on a PR. A red test that the change could not have caused gets its row on main
first. For a tag, the job is re-run only after that (`ci.yml` reads main's list
when the job starts, so the re-run is green because of the row, not by luck). A
red that turns green on a re-run is a flake, and it still gets its row. The one
re-run without a row is for a job that never reached a verdict (lost runner,
cancelled). The quarantine warden (`docs/quarantine-warden.md`) catches what a
reader misses, and runs the fix and dequarantine lane.

**What a row does:**
- **`go` rows:** the test is dropped from the job's `-skip` run. It runs
  again in the next step, `quarantined tests (advisory)`, which is
  `continue-on-error`. Its log still shows whether it passes, and it never
  fails the job.
- **`golden` rows:** one row per screenshot file, never a whole golden test.
  The shot still renders and is diffed. The log says `QUARANTINED <shot>: N
  of M pixels differ (<bead>, ...)`, and its actual, expected and diff PNGs
  go into the `garden-goldens` artifact, which is now uploaded on a pass too.
  It does not fail the test. A row also covers a golden whose surface no
  longer shoots it, or a new shot with no golden yet. A quarantined shot that passes logs `quarantined
  <shot> passed; delete .github/quarantine.d/golden-<shot>.tsv once <bead> is fixed`.
  `golden.Check` does this for every golden test (garden, composer, lands),
  reading the file named in `GARDENIO_QUARANTINE`. When that is unset, as on
  a laptop, nothing is quarantined.

**Adding a row.** When a Go test or golden shot fails, the job's `quarantine
rows for the failures` step prints a ready-to-paste row for each failure.
1. File a bug bead for the cause. One bead can cover several rows.
2. Put its id in the row, save each row as its own file,
   `.github/quarantine.d/<kind>-<name>.tsv`, and land them as their own PR: a
   branch from origin/main whose one commit only adds files there,
   opened ready (not draft), merged with a plain `gh pr merge` once `ci`
   and `checks` are green. Any agent may open one. A direct push to main
   is refused (GH013); see `docs/merge-desk.md`, "Known-failing tests".
3. Don't send the PR back. The merge desk merges it over that red as a
   MAIN-DRIFT MERGE (`docs/merge-desk.md`, "Known-failing tests").

A failure the PR's own diff could have caused is not quarantined: that is
the PR's to fix. Lint, compile, guard and script steps are never
quarantined, and neither is a job that names no failing test.

**Rows.** One row per file, one line; `#` lines are comments
(`.github/quarantine.d/README.md`). The columns are tab-separated: kind (`go` or `golden`), name (a
top-level Go test, or a shot file name), bug bead, date added, expiry, and
evidence (a failing run URL or main commit). `fetch` drops a malformed row
with a warning: a bad name, kind or bead, or a name that would widen
`-skip` past one test.

**Expiry is 14 days.** After that, CI warns on every run until someone fixes
the test or renews the row with fresh evidence. Expiry warns. It never
un-quarantines the test, because a test that comes back red blocks every PR
again.

**Leaving.** The fix deletes the row's file in the same PR. Close the bead once
every row it covers is gone. A row whose bead was closed some other way is
taken off by the quarantine warden once a run after the close shows the test
passing, and a closed bead whose test still fails is reopened
(`docs/quarantine-warden.md`).

## Tests skipped in `ci.yml`

One test is excluded by name via `-skip`, apart from the quarantined ones
above. It is not flaky-by-accident; it has a specific cause and a specific
fix.

(The five `internal/podmanager` freeze/thaw tests previously listed here are
fixed and no longer skipped — see `freezethaw_runner_uid_test.go`. They executed
prod script fragments that hardcode the in-pod agent uid/gid 1000, which a
runner at any other uid cannot satisfy; the harnesses now retarget those
literals onto the running uid/gid, a no-op in the uid-1000 lane.)

### `TestRealCatalogSnapshotFreshness`
Pre-existing, unrelated to the gardenio migration — it fails identically on the
pre-migration tree. The advice-catalog snapshot in `testdata/` was captured
2026-09-01 and the test enforces a 336h hard ceiling, so it aged out on its own.

**Fix:** regenerate the snapshot against a live beads instance:
`cd gardenio/controller && go test ./cmd/gb -run TestRealCatalog -update-advice-snapshot`
(needs `BEADS_HTTP_ADDR` + `BEADS_AUTH_TOKEN`). Blocked until a gardenio beads
daemon exists.

## Advisory jobs

### `lands perf gate` (`lands-perf.yml`, advisory since 2026-10-29)
Operator decision `gd-meUNukv4qC` (bug `gd-caHXOpfxNC`). Since 2026-11-01
`lands-perf.yml` never runs on a pull request (owner rule 2026-10-31, task
`gd-yqSVQsjtyW`), so it gates no merge. It runs nightly on main (main's tip
against main 25 h earlier) and on dispatch. Until the fixes below land, a red
`lands perf gate` job is read, not acted on alone. It goes red for reasons
unrelated to the change:
- **B7 world phone pan:** graded raw at 100 ms once the calibration floor
  reads under 100, so a floor of 98 ms and a floor of 30 ms are treated the
  same (PR #1466: 281 vs 100, floor 98). About 11% of same-code head/base
  pairings fail. **Fix:** PR #1484.
- **B8 idle bytes (Commons channel, thread, compose):** `idle()` divides a
  page's bytes by 60 s, but the tap runs from when the page opens, so it
  reads about 2x the real rate and sits at the budget (PR #1466: 5.0 vs 5.0).
  **Fix:** PR #1456 (`gd-CLQT35hQnk`).
- **B10 glass desktop cold LCP:** graded as stable, but it is CPU-bound and
  scales with the node's bench (PR #1484: 1844 vs 612 ms, bench 110 vs 32).
  **Fix:** `gd-mhCqAqbT0Y`, no PR yet.

Only the `lands perf gate` job is excused. A red `lands perf (head)` or
`lands perf (base)` means something broke: they run the perf gate self-test
and the harness. The gate is still read. When the nightly's gate goes red on
a surface a day's merge touched, or on any budget outside the three listed
above, file a bug against that merge.

**Remove this entry** once #1484 and #1456 are merged and `gd-mhCqAqbT0Y` is
fixed. Then a red nightly `lands perf gate` is a regression to fix.

## Resolved

### The two FNV hash-collision tests (was: skipped)

`TestStaggeredCron_NoCollisionOnProdFleet` and
`TestGardenerWallClockCohortDesynchronized` each pinned a handful of production
bead ids and asserted those particular ids did not collide in a hashed slot
space. That asserted a property of the SAMPLE: ~29% of arbitrary id sets
collide by the birthday effect, so their green was luck, not evidence, and
remapping the ids to the `ka-`/`gd-` namespace drew an unlucky set.

Replaced by `TestStaggeredCron_SpreadsFleetAcrossMinutes` and
`TestGardenerWallClock_SpreadsCohortAcrossDays`, which assert the invariant the
stagger actually exists for -- that the hash SPREADS gardeners so a shared cron
default or a shared epoch baseline cannot produce a herd. Each asserts
uniformity over a 3000-id corpus, robustness to character reordering, and a
realistic fleet/cohort spread, all from a fixed seed so they are deterministic.
Every threshold is derived from the binomial arithmetic in a comment beside it.

Both were mutation-tested: a constant slot, an order-blind byte-sum hash, a
first-byte-only hash, and (for the cohort) reinstating the original always-zero
offset all turn them red.

**A real property of the production hash, documented in the test:** FNV-1a's
low bit is the XOR of every input byte's low bit, which is permutation
invariant, and multiplying by the odd FNV prime preserves it. Against an even
modulus that parity-locks the result, so permutations of one digit multiset
reach only 15 of the 30 minute slots. Harmless for close-DAYS (each day still
holds 12 reachable even hours) but it halves the effective minute space for the
`ka-` sequential-digit namespace. Not changed here: re-seeding the hash would
reschedule every gardener, and the contract is "same seed, same cron, forever".

### coop `pty_backend` integration tests — ROOT CAUSE FOUND (not a skip any more)
`coop/crates/cli/tests/pty_backend.rs` hung on the laptop runner. Reproduced
locally: all 7 tests pass in 3 s with `--test-threads=1` and in 1 s with 2
threads; at the default thread count (12 on this box) the binary hangs until
the job timeout. Signals, TTY, stdin, env and systemd context were all ruled out
one by one.

Mechanism: `crates/cli/src/backend/spawn.rs:68` uses `nix::pty::forkpty`,
which forks the whole test process. A child spawned by one test inherits the
pty master/slave fds of every other test running at that moment and keeps
their slave ends open, so those tests never see EOF from their own child.
Fewer cores on the old RWX runner made the overlap rare; here it is certain.

CI now runs the workspace in parallel with those four excluded, then runs the
`pty_backend` binary alone with `--test-threads=1` — nothing is skipped.

**Code fix (todo, coop):** mark the pty fds `O_CLOEXEC` (or close all
inherited fds in the forkpty child before `exec`), then drop the serialized
step. This is a real bug: any coop process that spawns multiple ptys
concurrently can leak fds between children the same way.

### `go-integration` (was: `continue-on-error`)
The 25 `//go:build integration` files had never executed in any CI — not in
GitHub Actions, and not in the deleted RWX pipeline either. Twenty-four are
`kbeads/internal/store/postgres` real-Postgres tests gated on
`BEADS_TEST_DATABASE_URL`; one is `gardenio/controller/cmd/edge-auth/pat_integration_test.go`,
which gates on `GITLAB_TEST_URL`/`GITLAB_TEST_GROUP` and skips permanently (it
needs a live GitLab and always will).

The job landed `continue-on-error: true` so red-tolerant coverage shipped
immediately instead of letting unknown breakage stall the phase. T10 (e2e-ci
plan) triaged the resulting census —
[35684956919](https://github.com/mattbakerconsulting/gardenio/actions/runs/35684956919),
27 of 704 top-level kbeads tests failing — across three CI round-trips and
landed on a clean, deterministic run
([35770139486](https://github.com/mattbakerconsulting/gardenio/actions/runs/35770139486)):
0 failures, 1 legitimate self-skip. `continue-on-error` is dropped from the
job; it is blocking now.

**Bucket 1 — test was wrong, fixed (`ad38998`, `1b196dc`, `a24a129`):**
- 19 files inserted beads (raw SQL or `store.CreateBead`) without
  `operated_by_id`, NOT NULL since migration 022 — every insert 500'd with
  `pq: null value in column "operated_by_id" ... (23502)`. Fixed by resolving
  a real operator FK via the existing `anyOperatorID` helper.
- `TestEventsKeysetPagination_NoDropOrDup_RealPostgres`'s raw `INSERT INTO
  events` fixture was missing `operated_by_id` too (`events`'s own NOT NULL
  CHECK from migration 026). Added it — then a second failure surfaced on the
  next CI run: the test asserted strict *id* ordering, but the query sorts by
  the composite `(created_at, id)` (queries.go:2215/2290, matching unit test
  already pins the ORDER BY), and the test's own `permuted` fixture
  deliberately decorrelates id from created_at to exercise the keyset bug the
  query comment describes. Asserting id monotonicity checked a property the
  sort key never promised. Fixed the assertion to check `(created_at, id)`
  ordering instead.
- Nine `TestMigrationNNN_*` tests called `m.Up()` expecting to land on their
  own migration as HEAD; HEAD had moved past all of them, so `Steps(-1)/
  Steps(1)` exercised a later migration's down/up instead. Pinned each to
  `m.Migrate(N)` for its own migration number.
- `TestHasReportPredicate_RealPostgres/nil_filter_returns_all_with_correct_per_row`:
  not a product bug — `queries.go:1234` deliberately projects a cheap `FALSE
  AS has_report` placeholder unless the filter sets `WithHasReport`
  (documented on `model.BeadFilter.HasReport`). The subtest passed a nil
  `HasReport` without `WithHasReport`, so it got the intentional stub. Fixed
  the subtest to set `WithHasReport: true`.
- `TestMigration038_FieldsSlackCardTSIndex`: the EXPLAIN query never included
  `fields ? 'slack_card_ts'` as an explicit conjunct, so Postgres had no
  provable way to use the partial index even with `enable_seqscan=off`. Added
  the same explicit clause migration 041's equivalent query already carries.

**Bucket 3 — needs something CI cannot give it:**
- `TestMigration041_FieldsMRURLIndex` / `TestMigration042_FieldsMRURLUniqueIndex`:
  the final EXPLAIN assertion in each is genuinely cost-based and not
  reproducible in CI. Both migrations size their win against production
  population (~8400 / ~7800 merge-request rows); the CI fixture has 2–5 rows,
  so the planner correctly picks the cheaper generic `idx_beads_type` scan
  over the new partial index — both are valid index scans, `enable_seqscan=off`
  isn't even in play. This is logged (`t.Logf`), not skipped: in
  `TestMigration042_FieldsMRURLUniqueIndex` the EXPLAIN check sits mid-test,
  ahead of the down-042, refuse-over-duplicate (23505) and Force+re-apply
  assertions, so a `t.Skipf` there would have silently dropped that coverage
  every CI run instead of just the one ambiguous planner check (merge desk
  review on PR #108, gd-Ai6ekNov28). `TestMigration041_FieldsMRURLIndex`'s
  EXPLAIN check is its last statement, so nothing downstream was ever at
  risk, but it's logged too for consistency and so the test reports as a
  clean PASS instead of a misleading SKIP.
- `TestPATValidation_Integration` (the one controller file) is the sole
  remaining self-skip: it skips permanently for lack of a live GitLab —
  expected, not a regression.

No bucket-2 (real product bug) failures were found in this census.

## Images not yet in the `images.yml` matrix

| image | why |
| --- | --- |
| `ci-runner` | builds the CI image itself; pinned by `gardenio/locks/ci-runner-tools.lock` |
| `geo-table` | needs `scripts/build-geo-table.py` — different context |
| `grafana-assistant-plugin` | npm build, needs `.npmrc` credentials |
| `grafana-llm-app-plugin` | pulls a pinned upstream plugin via `pin.env` |
| `grafana-vl-datasource-plugin` | as above |
| `microvm-agent` | needs prebuilt `artifacts/rootfs/` not present in the tree |
| `slackmock` | test fixture, not a deployable |

## RWX decommission (2026-09-16)

The ACME-era RWX pipelines are deleted; specs are archived under
`docs/archive/rwx/`. Every task was classified (proof table in the commit
`Port the load-bearing RWX checks to a checks.yml workflow` and its
successors): covered by `ci.yml`/`images.yml`, ported to `checks.yml`,
ACME-obsolete, or parked as a D row in `checks.yml`'s `deferred` job.

Known debt carried by `checks.yml` (jobs are `continue-on-error`, verified
red on the pristine snapshot too):
- `lint-go` is no longer known debt: the SA4023 finding is gone and the job has
  no `continue-on-error`. A red `golangci-lint (...)` job is never tolerated by
  the merge desk, whatever this file says.
- `helm`: was "64 chart tests read `canonical-prod` / `deploy-values`
  overlays the redaction stripped" (RWX decommission triage). The
  `deploy-values` half is resolved (task `gd-Il0jwnzCMT`): decision
  `gd-KGcBBRQUdw` confirmed all five missing overlays are dead (Inbox
  decommissioned; `rwx-prod`/`rwx-poc`/`claudemore-isolation` dead by the
  2026-09-16 RWX decommission), so the test functions asserting against them
  were deleted (not skipped) in one revertible commit — see
  `gardenio/helm/gardenio/tests/README.md`. `continue-on-error` is dropped
  from the `helm` job for exactly this reason: it is real signal again.
  The `canonical-prod` half is also resolved (task `gd-wzxRDryYsR`): decision
  `gd-mRJ0F3rIVe` confirmed canonical-prod is out of scope for this repo's
  test suite, so the 17 tests across 4 files (`agent_image_prewarm_test.go`,
  `eks_api_intrusion_infra_deletion_test.go`,
  `eks_api_intrusion_sensitive_verb_test.go`,
  `eks_api_intrusion_unauthorized_test.go`) that read live Terraform state
  from `gardenio/terraform/composition/canonical-prod/` (missing from this
  repo) were deleted (not skipped) in one revertible commit — see
  `gardenio/helm/gardenio/tests/README.md`. The `-skip` list is gone from the
  `helm` job in `checks.yml` for the same reason as the deploy-values half:
  every remaining test is real signal again.
- `security` (advisory, as in RWX): 50 gitleaks findings, all test fixtures; 59 OSV ids on HEAD.
- `cargo-security` (advisory): `cargo audit` reports **2 vulnerabilities** —
  h2 (RUSTSEC-2026-0258, fix >=0.4.16) and rustls (RUSTSEC-2026-0285, medium,
  fix >=0.23.45) — plus 4 allowed warnings (anyhow, lru, scc unsound; spin yanked);
  `cargo deny` fails `bans` on 10 duplicate-version crates (licenses and sources ok).
  The two vulnerabilities are real and fixable with `cargo update -p h2 -p rustls`.

Not ported (D rows): coverage ratchets, beads-viewer e2e, advice-snapshot
freshness, terraform composition tests (roots stripped), coop rendering
with claudeless, the calver release DAG (`make release` exits 1 until then).
