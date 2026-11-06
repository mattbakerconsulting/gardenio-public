# Known-failing tests: THE list

Owner rule 2026-11-01: a test that has blocked one PR, or failed on main, never blocks a second PR.
Each `*.tsv` file in this directory is one row of the list, named `<kind>-<name>.tsv`
(`go-TestControlLeaseLapses.tsv`, `golden-land-phone-dark.png.tsv`). One file per row, so two
PRs that quarantine or unquarantine different tests never touch the same file and never conflict
(gd-3lWbWYIFoT). Add a row: create its file. Remove a row: delete its file. Renew a row: rewrite
its file. This README keeps the directory in git when the list is empty.

CI reads the files on MAIN (`scripts/quarantine.sh fetch`, which concatenates them into one TSV),
not the PR head's, so a file pushed to main unblocks every open PR at once, with no push to any of
them. Policy (`.github/known-failures.md`, "Quarantine"):

- low bar: red on code that did not touch it, once, is enough; no reruns-to-green first.
- a quarantined test still runs and still reports; it never fails the job.
- every row names a bug bead; fixing it means deleting the row's file in the same PR.
- expires is a nag, not a gate: CI warns on a row past it (fix the test or renew the row).

A row is one line, tab-separated, with these columns:

| column | value |
|---|---|
| kind | `go` (a top-level Go test name, no subtest) or `golden` (a screenshot file name) |
| name | the test or shot name |
| bead | the bug bead |
| added | YYYY-MM-DD |
| expires | added + 14 days |
| evidence | a failing run URL or main commit |

Lines starting with `#` are comments. `scripts/quarantine.sh rows <go test log>` prints the rows
for a failing log; `scripts/quarantine.sh fetch` drops a malformed row rather than obey it.
