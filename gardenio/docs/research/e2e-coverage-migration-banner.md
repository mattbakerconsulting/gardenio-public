# E2E coverage gap: viewer migration banner (P5+P6+P7)

Bead: ka-0000002673 (coverage-gap pass 2026-05-04). Source MRs:

- !2021 — P5 banner render + visibility (ka-0000007452)
- !2022 — P6 cookie-based dismiss (ka-0000004510)
- !2023 — P7 config-flag sunset wiring (ka-0000006508)

## Gap

`gardenio/controller/cmd/beads-viewer/migration_banner.go` (and the partial
`templates/partials/migration_banner.html`, embedded by `active.html` and
`search.html`) has **zero** automated coverage today:

- No unit test alongside `operator_chip_test.go` etc. exercises
  `shouldRenderMigrationBanner` or `handleMigrationBannerDismiss`.
- No e2e module under `gardenio/tests/e2e/scripts/` hits `/active`,
  `/search`, or `POST /banner/migration/dismiss` and asserts banner
  presence/absence.
- The forwardauth e2e (`test-forwardauth-403-force-logout.sh`) only
  validates the 403/redirect path, not banner HTML.

Five gates are evaluated by `shouldRenderMigrationBanner`; none are
covered:

1. OIDC email present.
2. `GARDENIO_VIEWER_MATT_EMAIL` set and viewer email != Matt's.
3. Dismiss cookie absent.
4. `viewer.show_migration_banner` tunable still true.
5. `time.Now().Before(2026-08-01 UTC)` (calendar backstop).

`gardenio-rwx` does **not** set `GARDENIO_VIEWER_MATT_EMAIL` today, so the
banner is suppressed entirely in the e2e cluster — meaning even an
authenticated probe would not exercise the render path without env
changes.

## Why this isn't a one-cycle e2e write

Hitting `/active` or `/banner/migration/dismiss` against a deployed
viewer pod requires either:

- A real Keycloak-issued bearer token (token mint helper does not exist
  in `gardenio/tests/e2e/scripts/`; viewer pod's OIDC middleware verifies
  the token, so a forged `X-Auth-Request-Email` is not enough); or
- A purpose-built test viewer pod with `BEADS_VIEWER_DEV_AUTH_BYPASS=1`,
  which is fail-closed in any pod that has a K8s service-account token
  mounted (i.e. every pod in the cluster).

Either path is real architectural work — a token-mint helper script
(see advice ka-0000000349 for the kcadmin pattern) plus optionally
plumbing `GARDENIO_VIEWER_MATT_EMAIL` into the rwx Helm values.

## Recommended scenarios

When the architectural prerequisite lands, the e2e module
`test-viewer-migration-banner.sh` should cover:

1. **Banner renders for non-Matt OIDC user.** Mint a token for a test
   user (e.g. `e2e-not-matt@example.com`), set
   `GARDENIO_VIEWER_MATT_EMAIL=matt@example.com` in the deployment (or a
   sidecar test viewer), GET `/active`, assert response contains
   `id="migration-banner"` and the dismiss button's
   `hx-post="/banner/migration/dismiss"`.

2. **Banner suppressed for Matt.** Same setup, but mint a token whose
   email matches `GARDENIO_VIEWER_MATT_EMAIL`. GET `/active`, assert the
   banner partial does NOT appear.

3. **Cookie dismiss path (P6).** With a non-Matt token, POST
   `/banner/migration/dismiss`. Assert HTTP 204 and a
   `Set-Cookie: gb_migration_banner_dismissed=1` header with `HttpOnly`,
   `SameSite=Lax`, `Path=/`, `Max-Age=31536000` (~1 year). Re-GET
   `/active` carrying that cookie; assert banner absent.

4. **Tunable sunset (P7 — flag side).** Flip
   `viewer.show_migration_banner=false` on the viewer's tunables config
   bead (or set `GARDENIO_VIEWER_SHOW_MIGRATION_BANNER=false` in env).
   Wait one ConfigWatcher poll (ref existing
   `test-config-bead-hotreload.sh`). GET `/active` with non-Matt token;
   assert banner absent. Restore tunable in cleanup.

5. **Calendar backstop (P7 — date side).** Out of e2e scope unless we
   add a clock-injection seam to `migration_banner.go`. Mark as a
   parity gap and rely on the unit test that should be added in the
   same impl task.

6. **Cross-surface render.** Repeat scenario 1 against `/search` to
   confirm the same partial renders on both default-on surfaces (the
   shared regression `!2025` was specifically about
   `operatorHeaderChip` template helper drift; the banner partial sits
   in the same template chain and benefits from identical scrutiny).

## Lower-cost intermediate step

While the architectural work is pending, drop a unit test
(`migration_banner_test.go`) that exercises gates 1–5 against a
hand-built `Server`+`*http.Request` with the `tunables` interface
faked. This is a cheap parity backstop: one file, no new harness, no
test pod, follows the `operator_chip_test.go` pattern. Do this first
in the impl task so that even if the e2e plumbing slips, banner gate
logic is locked.

## Plan-of-record

- This doc + a follow-up impl task (filed by the same MR) capture the
  scope. The impl task lives under the same area as the original P5/
  P6/P7 epic chain so the work is rediscoverable from beads-viewer.
- No new e2e module is added in this MR.
- Banner is suppressed in `gardenio-rwx` today (env var unset), so
  shipping the impl task also requires deciding whether to set
  `GARDENIO_VIEWER_MATT_EMAIL` in the rwx values. That decision belongs
  on the impl task, not here.
