# Coverage path mapping — canonical scheme

This document defines the canonical path scheme used by the
cross-component coverage merge pipeline (Coverage 2a/W3,
`scripts/coverage/`). It is the contract that every per-language
coverage emitter is normalized against before merging.

If you add a new emitter (new component, new test runner, new CI
runner), this is the doc to update — and the merge will fail loudly
(via the allowlist verifier in `normalize-paths.py`) if your new
inputs don't conform.

## Goal

Every `SF:` line in every merged LCOV tracefile MUST be:

1. **Repo-root-relative.** No leading `/`, no module prefix, no
   build-tree prefix.
2. **Forward-slash-delimited.** No backslashes, even on Windows.
3. **Aligned to the layout in `CLAUDE.md`.** The first segment of
   every path is one of the component roots below.

Without this, `lcov -a` (the merge tool) cannot deduplicate file
records across `unit` and `e2e` inputs, and downstream consumers
(GitLab MR widget, Cobertura derivation, beads-viewer) double-count
or split coverage on what should be the same file.

## Canonical layout

```text
gardenio/controller/...      — Go module "gardenio/controller"
kbeads/...                  — Go module "gardenio/kbeads"
coop/crates/{cli,mux,web}/...  — Rust workspace under coop/
scripts/...                 — repo-rooted shell + python utilities
.rwx/...                    — RWX CI definitions (rare in coverage)
```

These are the only top-level prefixes that may appear in a merged
tracefile. The verifier in
`scripts/coverage/normalize-paths.py:KNOWN_PREFIXES` is the single
source of truth — keep it aligned with `CLAUDE.md`.

beads3d is intentionally **not** in this list. Per the §8.6 decision
on `ka-0000007391` (`ka-0000004712` resolved exempt), beads3d does
not emit coverage under Phase 2a. If beads3d is later de-deprecated
(see `CLAUDE.md`), file a fresh §8.6-style decision before adding it
back to merge inputs and to this doc.

## Emitter → canonical rewrites

The normalizer in `scripts/coverage/normalize-paths.py` applies these
rewrites in order. The first match wins per record. New emitters
should match an existing rewrite or get a new one added.

| Emitter | Raw `SF:` shape | Rewrite | Canonical form |
|---|---|---|---|
| `go test -coverprofile` (controller) | `gardenio/controller/...` | passthrough | `gardenio/controller/...` |
| `go test -coverprofile` (kbeads) | `gardenio/kbeads/...` | strip module prefix | `kbeads/...` |
| `go tool covdata textfmt` + `gcov2lcov` (E2E controller) | `/<workspace>/gardenio/controller/...` | `--workspace-root` strip | `gardenio/controller/...` |
| `cargo llvm-cov --lcov` (coop unit) | `/<host>/coop/crates/cli/src/main.rs` | `--workspace-root` strip | `coop/crates/cli/src/main.rs` |
| `grcov -t lcov` (coop E2E) | repo-relative when `--prefix-dir` is set, absolute otherwise | `--workspace-root` strip if needed | `coop/crates/cli/src/main.rs` |

Workspace-strip patterns currently recognised by the normalizer:

- `/workspace/` — RWX
- `/builds/<group>/<project>/` — GitLab CI
- `/home/<user>/(bot/)?gardenio/work/` — agent pods + local

Pass `--workspace-root <path>` to the normalizer for any other CI root
(it's a single path-prefix strip applied before the regex table).

## Exclusion list

The normalizer drops records whose canonical path matches any of:

```text
**/gen/**             — generated code (e.g. kbeads internal/gen, sqlc)
**/*.pb.go            — generated protobuf
**/proto/**           — proto sources (definitions, not implementation)
**/*_sqlc.go          — sqlc-generated Go
**/sqlc/**            — sqlc directories
**/dist/**            — built JS bundles
**/node_modules/**    — third-party JS
**/vendor/**          — vendored Go (defensive; gardenio does not vendor today)
**/target/**          — Rust build dir
**/*_test.go          — Go test helpers (not the system under test)
**/tests/**           — integration test directories
**/fixtures/**        — test fixtures
**/testdata/**        — Go testdata convention
**/*.test.{ts,js}     — JS unit tests
**/*.spec.{ts,js}     — JS spec tests
```

These shrink **both numerator and denominator** — generated and test
files are removed from the calculation entirely (W3 §6 / W5 §4.3-4.4).
This is intentional: covering generated code with a test suite is not
a meaningful signal, and counting tests as their own coverage targets
is double-counting.

If you need a different exclusion set for a specific consumer (e.g.
"include _test.go to debug a coverage gap"), pass `--no-default-excludes`
plus an explicit `--exclude` set to `normalize-paths.py`.

## Workflow when adding a new emitter

1. Run the new emitter and save its raw LCOV.
2. Pipe it through the normalizer with `--strict --workspace-root <ci-root>`
   and inspect stderr for `bad_prefix>0`.
3. If any records have bad prefixes, either:
   - add a rewrite rule to `PATH_REWRITES` in `normalize-paths.py`, or
   - add a workspace-root pattern to the same table, then
   - add a unit test in `normalize-paths_test.py:TestPathRewrites`.
4. If the emitter introduces a new top-level component, add it to
   `KNOWN_PREFIXES` in the normalizer **and** to this doc's
   "Canonical layout" table.
5. If the emitter introduces a new exclusion (e.g. a tooling-specific
   build dir), add the glob to `DEFAULT_EXCLUDES` and to the
   exclusion list above. Add a `TestExclusion` case.
6. Re-run `python3 -m unittest scripts/coverage/normalize-paths_test.py`
   and `bash scripts/coverage/merge_test.sh`.

## Why not absolute paths?

Two reasons:

1. **Reproducibility.** Absolute paths bake the CI runner's
   filesystem into the artifact. A merged LCOV with `/builds/...` in
   it is unusable for offline analysis on a developer laptop.
2. **Deduplication.** Two emitters writing the same file from two
   different CI machines produce different absolute paths. The merge
   tool sees them as distinct files and can't sum their counters.

Repo-root-relative is the lowest-cost canonical form that solves both.

## Why not module-prefixed?

Three reasons:

1. **Cross-language consistency.** Rust crates and JS packages have
   different module-path conventions; using module prefixes for Go
   only would split the merged file into a heterogeneous mix.
2. **Greppability.** Repo-root-relative paths are what `git grep`,
   `rg`, and IDE jump-to-file expect. Coverage tooling that links
   from a coverage line to the source file works out of the box.
3. **Stability under module renames.** Renaming a Go module
   (`gardenio/kbeads` → `gitlab.com/example-org/gardenio/kbeads`,
   for instance) wouldn't move any source files; the canonical paths
   should stay the same regardless.

## References

- Research: `docs/research/coverage-merge-format.md` (W3, locked-in)
- Implementation: `scripts/coverage/{normalize-paths.py,merge.sh,derive-cobertura.sh}`
- Tests: `scripts/coverage/normalize-paths_test.py`, `scripts/coverage/merge_test.sh`
- Decisions: `ka-0000004712` (beads3d exempt), Epic `ka-0000000117` (Phase 2a)
