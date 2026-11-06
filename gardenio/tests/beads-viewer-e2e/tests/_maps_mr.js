// Reusable E2E fixture module for the "maps merge request" (gd-YDwA1wpnnH).
//
// The underlying fixture bead is seeded by seed/fixtures.mjs under the stable
// key `maps_mr` (label `fixture:e2e:maps_mr`): a type=merge-request bead that
// hangs off the in-progress maps task (`maps_task_ip`) as a parent-child child
// and forms part of the gardener map fixture graph (gd-EBQ0PytygF). It carries a
// single `mr_url` field and, deliberately, NOTHING else — no mr_state, no
// pipeline status, no branch names. That thin shape is the point: it is the
// canonical fixture other suites reach for when they need an MR *node* rather
// than a fully-populated MR *card*. Because it is really type=merge-request:
//
//   • the bead-show page dispatches the `type_mr` card (Merge Request heading
//     + the clickable mr_url link),
//   • the gardener connected-beads constellation renders it as a DIAMOND
//     (`constellation-node-mr`, DiamondPath) instead of a circle — the "has-MR
//     facet + diamond glyph" the seed comment calls out (ka-0000004604),
//   • the gardener dense-lane view renders it as a one-line node tagged with the
//     `merge-request` type.
//
// WHY A MODULE (not another spec): several concerns want to drive this same
// fixture — the type_mr card, the gardener constellation diamond, the dense
// lane row, the deps → parent-task edge. Rather than each of them re-deriving
// "which key is the maps MR, what type/parent should it have, where does the
// mr_url link live, and how do I locate its diamond", this module centralizes:
//
//   • the stable fixture KEY + a thin id resolver,
//   • the EXPECTED invariants the seed guarantees (type, mr_url, parent task
//     key, project label) as data other tests assert against,
//   • navigation helpers (open the bead-show page for the MR),
//   • DOM LOCATORS for the surfaces the MR appears on (bead-show type_mr card
//     + mr_url link, inline-header type chip, the deps → parent-task link, the
//     gardener constellation MR-diamond node, the dense-lane MR row),
//   • a single `assertMRCard()` that pins the type_mr contract in one place, so
//     a type_mr regression is caught by every consumer instead of being
//     duplicated N times.
//
// Import it from any spec:
//
//   import * as mapsMR from './_maps_mr.js';
//   const id = mapsMR.id();
//   await mapsMR.gotoBeadShow(page);
//   await mapsMR.assertMRCard(page);
//
// The module is import-only (no top-level `test(...)`), so importing it never
// registers phantom tests. A companion spec `maps_mr.spec.js` exercises the
// module against the live viewer as the CI gate.

import { expect } from '@playwright/test';

import { fixtureId } from './_helpers.js';

// The stable seed key. Kept as a named export so consumers reference the key
// symbolically (mapsMR.KEY) rather than hard-coding the string.
export const KEY = 'maps_mr';

// The parent task's seed key — the maps MR is a parent-child child of the
// in-progress maps task (`maps_task_ip`) per seed/fixtures.mjs. Exported so
// graph/deps consumers can resolve the edge target without re-reading the seed.
export const PARENT_TASK_KEY = 'maps_task_ip';

// Invariants the seed guarantees for this fixture. Consumers assert against
// these instead of restating literals, so a change to the seeded shape is a
// one-line edit here rather than a scavenger hunt across specs.
//
// NOTE: the fixture carries ONLY `mr_url` — no `mr_state`, `mr_pipeline_status`
// or branch fields. So on the bead-show type_mr card the state badge, pipeline
// <dd>, and branch rows are deliberately ABSENT; `assertMRCard()` asserts the
// heading + URL link only, and `assertNoStateBadge()` pins the absence so a
// future "always render a state badge" change is caught against this fixture.
export const EXPECTED = Object.freeze({
  type: 'merge-request',
  mrURL: 'https://gitlab.com/example/-/merge_requests/4242',
  projectLabel: 'project:gardenio',
});

// Resolve the fixture's daemon-assigned `kd-...` id from the seeded manifest.
// Throws a precise error (via fixtureId) if the seed did not run or the key is
// missing from the manifest.
export function id() {
  return fixtureId(KEY);
}

// Resolve the parent task's id.
export function parentTaskId() {
  return fixtureId(PARENT_TASK_KEY);
}

// ── Navigation ────────────────────────────────────────────────────────────

// Open the bead-show page for the maps MR and assert the response is
// non-error. Returns the Playwright response for callers that want to assert
// further on it.
export async function gotoBeadShow(page) {
  const res = await page.goto(`/${id()}`);
  expect(res, 'navigation response').not.toBeNull();
  expect(res.status(), `${KEY} (${id()}) bead-show HTTP status`).toBeLessThan(400);
  return res;
}

// ── Locators ────────────────────────────────────────────────────────────────
// Every locator is a pure function of `page` (or a scoped root) so consumers
// compose them without this module owning any page state.

// The bead-show type_mr card (templates/partials/type_mr.html —
// <section class="mr-card"> with an <h3>Merge Request</h3>).
export function mrCard(page) {
  return page.locator('section.mr-card');
}

// The mr_url link inside the type_mr card. The partial renders it as
// <a href="{mr_url}" target="_blank" rel="noopener">{mr_url}</a>.
export function mrURLLink(page) {
  return mrCard(page).locator(`a[href="${EXPECTED.mrURL}"]`);
}

// The type_mr MR-state badge (<p class="mr-state mr-state-{state}">). ABSENT
// for this fixture (no mr_state field) — the locator exists so consumers can
// assert its absence explicitly.
export function mrStateBadge(page) {
  return mrCard(page).locator('p.mr-state');
}

// The bead-show inline-header type chip. Class hooks come from
// templates/partials/bead_show_inline_header.html:
//   <span class="bsh-chip bsh-chip-type">merge-request</span>
export function typeChip(page) {
  return page.locator('.bead-show-inline-header .bsh-chip-type');
}

// The Dependencies section link to the parent maps task on the bead-show page
// (templates/partials/deps.html — <a href=".../{taskId}"><code>{taskId}</code></a>).
export function parentTaskDepLink(page) {
  return page.locator(`.deps-section .dep-list a[href$="/${parentTaskId()}"]`);
}

// The gardener constellation MR-diamond node for this MR. type=merge-request
// nodes get the `constellation-node-mr` class and a DiamondPath instead of a
// circle (constellation.html / applyMRState — ka-0000004604). Scoped to the
// node whose anchor points at this bead's show page. `root` is typically the
// #cap-beads-host container (or the page) after the constellation renders.
export function constellationDiamond(root) {
  return root.locator(`.constellation-node-mr:has(a[href$="/${id()}"])`);
}

// The gardener dense-lane row for this MR: the one-line node whose link points
// at the MR's bead-show page (templates/partials/gardener_beads.html dense view).
export function denseNodeRow(root) {
  return root.locator(`.cap-beads-dnode:has(.cap-beads-dnode-link[href$="/${id()}"])`);
}

// The `merge-request` type tag inside a dense-lane row (the
// <span class="cap-beads-dnode-type"> sibling of the link).
export function denseNodeType(root) {
  return denseNodeRow(root).locator('.cap-beads-dnode-type');
}

// ── Shared assertions ───────────────────────────────────────────────────────

// Pin the type_mr card contract on the bead-show page in ONE place: the card
// is visible, the heading reads "Merge Request", and the mr_url link is present
// with the correct href and safe external-link attributes (target=_blank +
// rel=noopener). Call after gotoBeadShow(page).
export async function assertMRCard(page) {
  const card = mrCard(page);
  await expect(card, 'type_mr card visible').toBeVisible();
  await expect(card.locator('h3')).toHaveText('Merge Request');

  const link = mrURLLink(page);
  await expect(link, 'mr_url link visible').toBeVisible();
  await expect(link).toHaveAttribute('href', EXPECTED.mrURL);
  await expect(link).toHaveAttribute('target', '_blank');
  await expect(link).toHaveAttribute('rel', /\bnoopener\b/);
}

// Pin the deliberate ABSENCE of an MR-state badge for this fixture (it carries
// only mr_url — no mr_state; see the EXPECTED note above). Call after
// gotoBeadShow(page).
export async function assertNoStateBadge(page) {
  await expect(
    mrStateBadge(page),
    'maps MR fixture (no mr_state field) renders no state badge',
  ).toHaveCount(0);
}

// Pin the merge-request type chip on the inline header. Call after
// gotoBeadShow(page).
export async function assertTypeChip(page) {
  const chip = typeChip(page);
  await expect(chip, 'type chip visible').toBeVisible();
  await expect(chip).toHaveText(EXPECTED.type);
}

// Pin the parent-task parent-child edge on the bead-show deps section. Call
// after gotoBeadShow(page).
export async function assertParentTaskLink(page) {
  await expect(
    parentTaskDepLink(page),
    'bead-show deps links to the parent maps task',
  ).toBeVisible();
}
