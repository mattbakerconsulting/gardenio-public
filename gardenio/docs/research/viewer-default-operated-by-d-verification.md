# D — Verification & rollout: beads-viewer default `operated_by=me`

Bead: ka-0000005487 · Parent epic: ka-0000005389 · Authorizing decision: ka-0000007227

## Goal

Smoke-verify the seven items in ka-0000005487 after P1–P7 landed on `main`
(commits `36583808`, `0a9390ab`, `7a05fd19`, `d685cfcb`, `12d70882`,
`a571d970`, `72556fc0`). Per Matt's epic rule the verification is smoke-only;
unit/e2e work stays out of scope.

## Method

The deployed `beads-viewer` requires real OIDC tokens to reach `/active`,
`/search`, and the dismiss POST handler. The dev-auth bypass refuses to run
in pods that have a K8s SA token mounted (see `dev_auth.go::checkDevAuthBypassAllowed`),
so an agent operating from inside the cluster cannot impersonate Matt's OIDC
session to drive the UI checks. Direct cluster-internal probes
(`http://gardenio-beads-viewer.gardenio.svc.cluster.local:8095/...`) are
rejected with `missing authorization token` for everything except `/livez`.

Verification therefore proceeds in two layers:

1. **Code-level confirmation** — read each handler and template wire-point on
   `main` and confirm the documented behavior is correctly implemented.
2. **Operator-driven UI smoke** — the seven items that require an authenticated
   OIDC session as Matt or as a non-Matt user are escalated to the operator
   via `gb decision`.

## Code-level findings (per scope item)

### 1. Default-me on `/active` and `/search` — wired ✓; chip render — wired ✓ (post-fix)

- Server-side default is wired in both surfaces.
  - `active.go:78–85` → `resolveOperatedByParam` then `resolveDefaultOperatedBy`,
    composed into `effectiveOperatedBy` and passed as `q.OperatedBy` (or
    `OperatedByIsNull` on `null` / `(none)`).
  - `search.go:51–60` → `resolveDefaultOperatedBy` (which echoes any URL
    value verbatim) then `resolveOperatedByParam`. The order reversal is
    benign because `resolveDefaultOperatedBy` is a pass-through whenever the
    URL key is present, and search has no `MyWorkSummaryMode` toggle dependent
    on `isMe`.
- Header chip — initial main snapshot (commits up to `72556fc0`) registered
  the `operatorHeaderChip` template helper but no template invoked it,
  leaving the cold-load default case with no visible chip. Filed as bug
  `ka-0000001394`; fix MR !2025 merged on main as commit `a1c87dd6` and
  wires the chip in both surfaces:
  - `templates/active.html:43` invokes
    `{{operatorHeaderChip .OperatedByURLHasKey .EffectiveOperatedBy
    .OperatedByIsUnowned .OperatedByDisplay .ViewerEmail .ChipBasePath
    .ChipExtraQuery}}`.
  - `templates/search.html:9` invokes the same call.
  - Handlers thread the new template fields (URL-has-key, effective value,
    is-unowned, display, viewer email, base path, extra query) through
    `active.go` / `search.go`.
  - Legacy filter-summary paragraph removed (chip subsumes it across all
    five states).
  - Render-shape table-driven test added covering all five chip states.

### 2. Override semantics — wired ✓ (resolver-level)

`resolveDefaultOperatedBy` at `operator_chip.go:155–164` uses
`r.URL.Query().Has("operated_by")` (not the value) as the trigger, so:

- `?operated_by=` (empty) — URL key present → echoed back as `""` → daemon
  query has no `OperatedBy` filter → "show all".
- `?operated_by=null` — echoed back; `handleActive` and `handleSearch` both
  detect this in their own branch and set `OperatedByIsNull=true` on the
  daemon query. Resolves to "unowned".
- `?operated_by=me` — URL key present → echoed back as `"me"` →
  `resolveOperatedByParam` expands to viewer email → daemon query filters to
  the viewer's beads. Identical to the default for an authenticated viewer.
- Any other literal — passes through verbatim.

R3 §1's "URL is the source of truth" precedence holds.

### 3. No auto-claim — wired ✓ by absence

The two handlers compute `q.OperatedBy` for the LIST query but neither calls
any mutation on the bead being viewed. There is no code path in
`active.go` / `search.go` / `operator_chip.go` / `migration_banner.go` that
issues a write against the daemon for the default-me filter. Bead-show
(`bead_show.go`) is unchanged by this epic.

### 4. Unowned exclusion — wired ✓

In `active.go:112–116`, the daemon query's `OperatedByIsNull` is only set
when `operatedByRaw == "null"` (or `(none)`). The default-me path only sets
`q.OperatedBy = effectiveOperatedBy` (a non-empty oidc id), never
`OperatedByIsNull=true`. Same logic in `search.go:60–91`. So unowned beads
(`operated_by IS NULL`) are NOT in a default-me result set; they are only
reachable via `?operated_by=null`.

### 5. Migration banner render — wired ✓

- `active.go:98` and `search.go:74` set `ShowMigrationBanner` from
  `s.shouldRenderMigrationBanner(r)`.
- `templates/active.html:6` and `templates/search.html:6` gate the
  `migration_banner` template on `.ShowMigrationBanner`.
- `migration_banner.go:72–94` enforces the V-R2 §5.3 visibility rules:
  OIDC-authenticated AND email != Matt's (env `GARDENIO_VIEWER_MATT_EMAIL`,
  fail-closed when unset) AND no dismiss cookie AND
  `viewer.show_migration_banner` flag true AND
  `time.Now().Before(migrationBannerBackstop)` where
  `migrationBannerBackstop = 2026-08-01T00:00:00Z`.
- Banner copy in `templates/partials/migration_banner.html` matches V-R2
  §5.2 (header "Migration notice", explanation about historic-Matt
  attribution, `[Show all]` / `?operated_by=` link, sunset note).

### 6. Migration banner dismiss — wired ✓

- Route registered in `server.go:472` as `POST /banner/migration/dismiss`.
- `migration_banner.go:102–112` writes the cookie:
  - Name `gb_migration_banner_dismissed`, value `1`, Path `/`, MaxAge ~1 year,
    HttpOnly, SameSite=Lax. (No `Secure` flag — would be a hardening nit;
    not in scope unless the operator flags it.)
  - Returns `204 No Content`.
- htmx button in the partial: `hx-post`, `hx-target=#migration-banner`,
  `hx-swap=outerHTML`, plus `hx-on::after-request` fallback that removes the
  banner element on success.
- Subsequent renders are gated by the cookie via
  `shouldRenderMigrationBanner` step 3 (cookie absent → render; cookie
  present → suppress).

### 7. Sunset wiring — wired ✓ (read-only verification)

- `migration_banner.go:87` reads `viewer.show_migration_banner` from the
  config-bead tunables (`s.tunables.GetBool`) with env override
  `GARDENIO_VIEWER_SHOW_MIGRATION_BANNER` and compiled default `true`.
- `migration_banner.go:90` evaluates the `2026-08-01T00:00:00Z` calendar
  backstop independently — both gates are AND-ed (V-R4 §3 / P7 spec).
- The verification task ("verify by reading the config bead, not by setting
  it") is a read-only check. The flag is read, not the bead, so the test
  gap noted in the scope ("use existing override or note the test gap")
  applies: changing the flag at runtime requires either flipping the env on
  the viewer Deployment and rolling, or setting the field on the tunables
  config bead (no override path exposes the flag from the UI).

## Deployed-state confirmation (2026-05-04, jxc re-spawn)

Both target environments are running viewer code that contains the full
P1–P7 stack and the chip fix:

- `gardenio-rwx`: `beads-viewer:main-0c1e87f` — HEAD of `main` (includes
  the new banner unit + e2e tests landed in commit `12bf90a1`).
- `gardenio` (prod): `beads-viewer:2026.124.1` — `git tag --contains
  a1c87dd6` and `--contains 72556fc0` both resolve to `2026.124.1`, so the
  chip fix and P7 sunset wiring are present.

Cluster-internal probes (`/livez`, `/active`, `POST /banner/migration/
dismiss`) on both services return `401 missing authorization token` for
unauthenticated callers, confirming the OIDC middleware is enforcing on
the same routes the verification scope targets.

## Runtime config gap: `GARDENIO_VIEWER_MATT_EMAIL` unset in both envs

A direct `kubectl get deploy ... -o jsonpath='...env'` on both viewer
Deployments shows the env var is not set:

- `gardenio-rwx/gardenio-rwx-beads-viewer` — no `GARDENIO_VIEWER_MATT_EMAIL`.
- `gardenio/gardenio-beads-viewer` — no `GARDENIO_VIEWER_MATT_EMAIL`.

`migration_banner.go::shouldRenderMigrationBanner` is fail-closed when
`GARDENIO_VIEWER_MATT_EMAIL` is empty, so today the banner is suppressed
for **every** viewer in both environments regardless of identity, cookie,
config flag, or date. Concretely:

- §5 ("non-Matt OIDC viewer sees banner") cannot fire today; the banner
  template is gated off in production. Code-read still confirms the
  visibility logic is correct; runtime UI-smoke for §5/§6 in either
  cluster will only become exercisable once the env var is set.
- §6 (dismiss POST returns 204 + cookie + htmx swap) is independently
  validated by the new unit tests below — handler behavior does not
  depend on `GARDENIO_VIEWER_MATT_EMAIL`.
- §7 (config-flag sunset) is moot at runtime today; the env-gate
  short-circuits before the flag is consulted.

This is a config gap, not a code defect — ka-0000000489's MR description
already flagged it as optional follow-up. No new bug bead filed; if the
operator wants to actually exercise the banner in a deployed environment
they must wire the env var into the viewer Deployment first.

## Test-coverage layer (post-D)

After this D task was first drafted, MR !2042 (ka-0000000489, commit
`12bf90a1`) added:

- `gardenio/controller/cmd/beads-viewer/migration_banner_test.go` — unit
  coverage for all five gates of `shouldRenderMigrationBanner` plus the
  `204 + cookie` contract of `handleMigrationBannerDismiss` (covers §5
  visibility logic and §6 dismiss handler in CI).
- `gardenio/tests/e2e/scripts/test-viewer-migration-banner.sh` — e2e
  module covering banner-render scenarios 1, 2, 3, 4, 6 from the
  coverage plan (1: non-Matt sees banner; 2: Matt does not; 3: dismiss
  POST + cookie + re-GET suppression; 4: tunable flag flip suppresses
  banner; 6: cross-surface render on `/search`). Wired into
  `run-shard.sh:458` as the `viewer-migration-banner` shard. Render
  scenarios skip cleanly when `GARDENIO_VIEWER_MATT_EMAIL` is unset.
- Scenario 5 (calendar backstop, V-R4 §3) is intentionally e2e-skipped —
  no clock-injection seam exists; it is unit-tested only.

This means §5, §6, §7 are now covered by repeatable automated tests in
addition to the code-read posture below. The remaining UI-only gaps
(Matt vs non-Matt URL state, real browser dismiss-then-reload across
sessions) still need an operator with an OIDC session.

## Defects filed

- **P4 header chip never rendered** — `ka-0000001394` (CLOSED). Fix MR !2025
  merged on main as commit `a1c87dd6`: chip helper invoked in
  `templates/active.html` and `templates/search.html`, legacy filter-summary
  paragraph removed, table-driven render-shape test covers all five chip
  states. P4 deliverable now satisfied in code; verification scope §1's
  "Header chip says 'Showing your work' with [all]/[unowned] switch links"
  is satisfiable by code-read.

## Operator-driven UI smoke (escalated)

The following items can only be confirmed end-to-end by an operator with a
real OIDC session — an agent inside the cluster cannot impersonate Matt or a
non-Matt OIDC user against the deployed `beads-viewer`:

- §1 (URL has no `operated_by` after a Matt-authenticated GET; result set
  filtered to `oidc:matt@…`; chip text rendered as expected — code now
  wires it, runtime visibility is the remaining UI-only confirmation).
- §2 (URL override matrix as Matt and a non-Matt OIDC user).
- §5 (non-Matt sees banner; Matt does not).
- §6 (POST `/banner/migration/dismiss` returns 204, sets cookie, htmx swap
  removes the banner; cookie persists across reloads).
- §7 (verifying the flag's runtime effect — distinct from confirming it is
  read).

These are escalated via `gb decision` on the parent epic. The chip-render
defect is independent of these and proceeds as a fix MR under the impl epic.

## Closeout note

Per the task description, "After smoke verification passes (or any defects
are filed as separate bugs), close this task and the parent epic." The
chip-render defect (`ka-0000001394`) is now FIXED on main (MR !2025, commit
`a1c87dd6`). Closing this D task and parent epic `ka-0000005389` is gated
on operator decision `ka-0000005237`:

- `[accept-code-only]` — accept code-read + automated-test verification;
  close D and parent epic now. Recommended by all three preceding agents
  and reaffirmed by this re-spawn. All seven scope items are now
  satisfiable by code-read; §5–§7 additionally have repeatable unit + e2e
  coverage post-MR !2042. Runtime banner UI-smoke is moot today because
  `GARDENIO_VIEWER_MATT_EMAIL` is not set on either viewer Deployment
  (banner fail-closed everywhere).
- `[operator-smokes]` — operator runs the OIDC-required UI smokes against
  gardenio-rwx and prod and pastes results; agent updates this doc and
  closes out afterwards. Note: operator must first set
  `GARDENIO_VIEWER_MATT_EMAIL` on the target viewer Deployment, otherwise
  banner scenarios skip and §5/§6/§7 cannot be exercised at runtime.
- `[fix-chip-first]` — already done (MR !2025 merged), so this option is
  effectively a no-op.
