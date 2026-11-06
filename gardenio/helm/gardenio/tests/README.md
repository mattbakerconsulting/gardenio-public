# Deploy-values overlays: dead, and the tests that pointed at them are gone

15 files in this package used to call `helm template` with `-f
../../../deploy-values/<overlay>.yaml` files that don't exist in this repo.
Only `deploy-values/gardenio-k3s.yaml` survived the export; the other five
overlays did not, and decision `gd-KGcBBRQUdw` (filed from task
`gd-Il0jwnzCMT`) confirmed why: `gardenio-inbox*.yaml` is dead (Inbox
decommissioned) and `gardenio-rwx-prod.yaml` / `gardenio-rwx-poc.yaml` /
`gardenio-claudemore-isolation.yaml` were dead by the 2026-09-16 decommission
of the RWX CI. None of the five is coming back on its own — reviving any of
them is a deliberate future decision, not a rebuild-from-triage exercise.

Per that disposition, the individual test functions (and file-local helpers
used only by them) that asserted against those five dead overlays were
**deleted**, not skipped — see the single revertible commit that made this
change for the full list. What's gone: 28 refs to `gardenio-inbox.yaml`, 6 to
`gardenio-rwx-prod.yaml`, 3 to `gardenio-rwx-poc.yaml`, 1 to
`gardenio-inbox-import-only.yaml`, 1 to `gardenio-claudemore-isolation.yaml`.
Tests in the same files that didn't depend on a dead overlay were left alone.
Four files (`forwardauth_coverage_test.go`, `inbox_import_only_overlay_test.go`,
`inbox_overlay_layering_test.go`, `keycloak_audit_client_role_test.go`) had
every test depend on a dead overlay and were removed entirely.

If Inbox or an RWX-era environment is ever revived, `git revert` that commit
to get the coverage back — the overlay data just needs to exist again at
`deploy-values/<name>.yaml`.

The `helm` job in `.github/workflows/checks.yml` no longer carries
`continue-on-error: true` for this reason: every deploy-values overlay a
remaining test asserts against actually exists.

## canonical-prod Terraform composition: dead

17 tests across 4 files (`agent_image_prewarm_test.go`,
`eks_api_intrusion_infra_deletion_test.go`,
`eks_api_intrusion_sensitive_verb_test.go`,
`eks_api_intrusion_unauthorized_test.go`) used to read live Terraform state
from `gardenio/terraform/composition/canonical-prod/`, which is missing from
this repo — same "export left files behind" shape as the deploy-values
overlays above, but for the canonical-prod Terraform composition instead.
This was tracked separately from decision `gd-KGcBBRQUdw` (task
`gd-wzxRDryYsR`) because, unlike the dead deploy-values overlays,
canonical-prod backs real prod (see `docs/release-pipeline.md`) — so
"reconstruct it" looked like the likelier disposition going in. Decision
`gd-mRJ0F3rIVe` confirmed otherwise: canonical-prod is out of scope for this
repo's test suite.

Per that disposition, the 17 test functions (and file-local helpers used only
by them, e.g. `terraformDir`/`terraformFilterPattern`) were **deleted**, not
skipped — see the single revertible commit that made this change for the
full list:
- `agent_image_prewarm_test.go`: `TestPrewarm_ProdOverlayEnablesBothDaemonSets`
- `eks_api_intrusion_infra_deletion_test.go`: every test — the whole file was
  removed (all 5 of its tests, and the helpers/fixtures used only by them,
  depended on canonical-prod).
- `eks_api_intrusion_sensitive_verb_test.go`: every test — the whole file
  (and its `eks_api_intrusion_sensitive_verb_events.jsonl` fixture) was
  removed for the same reason.
- `eks_api_intrusion_unauthorized_test.go`: `TestUnauthorized_FilterSplitsAnonymousFromAuthenticated`,
  `TestUnauthorized_EmptyExclusionListCountsEverything`,
  `TestUnauthorized_401BranchCarriesNoPrincipalGuard`,
  `TestUnauthorized_ExclusionsAreExactMatchNoWildcard`,
  `TestUnauthorized_ExcludedPrincipalsMatchProdTfvars`. The file's remaining
  test (`TestUnauthorized_AnonymousStillCoveredBySameAlert`) doesn't depend on
  canonical-prod and was left alone.

Every remaining `eks_api_intrusion_*` file was deleted later with the
EKS-only chart features themselves (gd-R4Ao2mKEwZ), so the revert below no
longer restores a runnable test.

A wider set of files merely *mention* canonical-prod in comments without
actually depending on it at runtime (confirmed by running every candidate
test before this cleanup); those pass today and were left alone.

If canonical-prod is ever exported into this repo, `git revert` that commit
to get the coverage back.

The `-skip` list this section used to describe is gone from the `helm` job
in `.github/workflows/checks.yml` — every test remaining in this package
actually exercises something that exists in this repo.

## `helm` here is a caching shim

`TestMain` (`helm_render_cache_test.go`) puts the test binary on `PATH` as
`helm` for the whole run (gd-iQ99AoxcdR). It renders each distinct set of
values once and answers `--show-only` itself, the way helm's `template.go`
does, which cuts ~1,950 `helm template` calls a run to ~1,250 renders. Call
sites need no change: `exec.Command("helm", ...)` and `helm` inside a
`bash -c` pipeline both go through it. A command it does not fully understand
(another subcommand, `--debug`, `--output-dir`, a chart directory other than
`..`) runs the real helm.

Two things follow. A `randAlphaNum` secret repeats across calls that share a
render, so no test may rely on it changing between calls. And after a helm
upgrade, run the suite once with `GARDENIO_TEST_HELM_VERIFY=1`, which checks
every call against the real helm and fails on any difference.
`GARDENIO_TEST_HELM_CACHE=off` turns the shim off.

## Tests run in parallel

Every test here starts with `t.Parallel()` (gd-1SaC2j9iYB). The suite was fully
serial: on a 4-vCPU CI runner the `helm` job averaged 0.72 of a core and
`go test` took 499s, almost all of it waiting on one `helm template` at a time.
The render cache above is safe under concurrency (each render is written to a
temp file and renamed into place), so tests only need to keep their own state
private: write files under `t.TempDir()`, never into the chart directory, and
do not change package-level variables. A test that calls `t.Setenv` cannot be
parallel (the testing package panics), so leave `t.Parallel()` out of that one
test, as `nats_server_conf_parse_test.go` does. New tests should start with
`t.Parallel()` too.
