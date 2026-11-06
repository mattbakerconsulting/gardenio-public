# beads-viewer accessibility (a11y) baseline

Phase T16 of the beads-viewer Playwright behavioral suite — see
`gardenio/plans/archive/beads-viewer-playwright-tests.md`. Bead: `ka-0000005762`.
Spec: `gardenio/tests/beads-viewer-e2e/tests/a11y.spec.js`.

## What this guards against

- A regression that lands a serious/critical [axe-core] rule violation
  on any canonical viewer page (top-level routes, per-type bead-show).
- Specifically the high-impact failure modes that screen-reader and
  keyboard users hit hardest: missing form labels, color-only state
  cues, broken landmark structure, low-contrast text, click handlers
  on non-interactive elements without keyboard equivalents.

## Threshold (initial baseline)

**Fail on `serious` + `critical` violations only.**
Skip `moderate` and `minor` for now.

Rationale (matches the recommendation in
`plans/archive/beads-viewer-playwright-tests.md`):

1. Critical and serious are axe's "must-fix" tier — they correspond to
   real barriers (a screen-reader user *cannot* use the page, a
   keyboard user *cannot* reach an interactive control, contrast fails
   WCAG AA). Anything less is a quality bar we want to ratchet *up to*,
   not gate the initial green CI run on.
2. Initial broad coverage > narrow strictness. Better to assert "no
   critical regression on every canonical page" than "no moderate
   issue on the homepage and nothing else."
3. Ratcheting is the explicit follow-up. Once the baseline is stable
   we can flip rules in one at a time (see "Ratcheting" below).

If a new threshold choice is needed (e.g. "skip critical on a specific
admin page that is operator-only"), raise it via `gb decision create`
with concrete options and an `artifact_type` — do not silently relax
the threshold in the spec.

## Coverage

### Top-level routes

Same set as the T3 route smoke (`tests/routes.spec.js`):

- `/`
- `/search`
- `/active`
- `/decisions`
- `/agents`
- `/advice`
- `/schedules`
- `/costs`
- `/p/{project}` (uses `gardenio`)

### Per-type bead-show pages

One test per canonical fixture from `seed/fixtures.mjs`. Each exercises
the matching `templates/partials/type_*.html` partial:

- `schedule` → `type_schedule.html`
- `agent` → `type_agent.html`
- `advice` → `type_advice.html`
- `decision` → `type_decision.html` (open)
- `decision_resolved` → `type_decision.html` (resolved branch)
- `formula` → `type_formula.html`
- `mr` → `type_mr.html`
- `generic` → `type_generic.html`

Adding a new canonical fixture? Update the `FIXTURE_KEYS` array in
`tests/a11y.spec.js` so the new partial gets a11y coverage too.

## What we explicitly do NOT cover (yet)

- Full WCAG 2.2 AAA — the suite runs the default axe ruleset (WCAG
  2.0/2.1 A + AA + best practice).
- Authenticated-only flows — the dev-auth-bypass mode used by the
  rest of the suite is what we test against; if the OIDC path
  introduces a11y issues, those are caught upstream by the OIDC
  provider's own UI tests.
- Dynamic content after htmx swaps — initial render only. T8
  (`htmx_swaps.spec.js`) asserts the swap mechanics; a11y after a
  swap is a follow-up. File a child of the epic if a regression
  surfaces here.
- Color-contrast at non-default themes — viewer ships one theme.

## Ratcheting (follow-ups)

Plan-of-record direction: once the baseline holds for several weeks
without flakiness, broaden the threshold to include `moderate`, one
violation class at a time:

1. Pick a `moderate` rule that currently has zero or one offender
   (run the suite without the threshold filter to inventory).
2. Fix the offender(s) in the same MR that ratchets the rule.
3. Update the `FAILING_IMPACTS` set in `tests/a11y.spec.js` OR add an
   explicit `withRules([...])` that includes the newly-required rule.
4. Update this doc with the new floor.

Do NOT ratchet by adding `disableRules([...])` to suppress an
inconvenient finding. If a rule is not applicable (e.g. a known
intentional pattern), document the rationale here AND in a comment
on the suppression call.

## Suppressions (none yet)

If/when a rule must be disabled for a specific page, record it here:

| Page / scope | Rule id | Reason | Bead |
|---|---|---|---|
| _none_ | | | |

## Running locally

```sh
# from the repo root, with a beads daemon reachable at $BEADS_HTTP_ADDR
make seed-beads-viewer-e2e   # one-time fixture seed
cd gardenio/tests/beads-viewer-e2e
npx playwright test tests/a11y.spec.js
```

The suite logs a one-line summary per page even on green runs:

```
[a11y] GET /: 0 blocking (serious+critical), 3 advisory (moderate+minor)
[a11y] bead-show:schedule: 0 blocking (serious+critical), 1 advisory (moderate+minor)
…
```

These advisory counts are the inventory you'll consult when ratcheting.

[axe-core]: https://www.deque.com/axe/
