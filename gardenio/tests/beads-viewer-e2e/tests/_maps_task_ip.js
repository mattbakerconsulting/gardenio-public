// Reusable E2E fixture module for the "maps in-progress task" (gd-k5YNKNMI7J).
//
// The underlying fixture bead is seeded by seed/fixtures.mjs under the stable
// key `maps_task_ip` (label `fixture:e2e:maps_task_ip`): an in_progress P0
// task parented under the `maps_epic` epic and forming part of the gardener
// map fixture graph (gd-EBQ0PytygF). Because it is really status=in_progress
// with priority=0, it is the fixture other suites reach for when they need a
// live, high-priority task node — the HEAT lens on the gardener constellation
// emphasizes it, the status chip strips render an `in_progress` facet, and the
// bead-show page renders the in_progress status chip.
//
// WHY A MODULE (not another spec): several concerns want to drive this same
// fixture — the gardener connected-beads graph, the status/filter chips, the
// bead-show status pill. Rather than each of them re-deriving "which key is the
// in-progress maps task, what status/priority/parent should it have, and how do
// I locate its status chip", this module centralizes:
//
//   • the stable fixture KEY + a thin id resolver,
//   • the EXPECTED invariants the seed guarantees (status, priority, type,
//     parent epic key, project label) as data other tests assert against,
//   • navigation helpers (open the bead-show page for the task),
//   • DOM LOCATORS for the surfaces the task appears on (bead-show status/
//     priority chips, the gardener dense-lane row, the deps → parent-epic link),
//   • a single `assertInProgressBadge()` that pins the in_progress contract in
//     one place, so a status-pill CSS-class regression is caught by every
//     consumer instead of being duplicated N times.
//
// Import it from any spec:
//
//   import * as mapsTaskIP from './_maps_task_ip.js';
//   const id = mapsTaskIP.id();
//   await mapsTaskIP.gotoBeadShow(page);
//   await mapsTaskIP.assertInProgressBadge(page);
//
// The module is import-only (no top-level `test(...)`), so importing it never
// registers phantom tests. A companion spec `maps_task_ip.spec.js` exercises
// the module against the live viewer as the CI gate.

import { expect } from '@playwright/test';

import { fixtureId } from './_helpers.js';

// The stable seed key. Kept as a named export so consumers reference the key
// symbolically (mapsTaskIP.KEY) rather than hard-coding the string.
export const KEY = 'maps_task_ip';

// The parent epic's seed key — the maps in-progress task is a parent-child
// child of `maps_epic` (seed/fixtures.mjs). Exported so graph/deps consumers
// can resolve the edge target without re-reading the seed.
export const PARENT_EPIC_KEY = 'maps_epic';

// Invariants the seed guarantees for this fixture. Consumers assert against
// these instead of restating literals, so a change to the seeded shape is a
// one-line edit here rather than a scavenger hunt across specs.
//
// NOTE: priority is the NUMERIC P0 (0). The bead-show inline-header chip is
// gated on `{{if .Bead.Priority}}`, and Go templates treat the int 0 as falsy
// — so the P-priority chip is deliberately ABSENT for this P0 fixture. That is
// a real rendering contract, not an oversight; `assertPriorityChipAbsent()`
// pins it so a future "always render a priority chip" change is caught.
export const EXPECTED = Object.freeze({
  type: 'task',
  status: 'in_progress',
  priority: 0,
  projectLabel: 'project:gardenio',
});

// Resolve the fixture's daemon-assigned `kd-...` id from the seeded manifest.
// Throws a precise error (via fixtureId) if the seed did not run or the key is
// missing from the manifest.
export function id() {
  return fixtureId(KEY);
}

// Resolve the parent epic's id.
export function parentEpicId() {
  return fixtureId(PARENT_EPIC_KEY);
}

// ── Navigation ────────────────────────────────────────────────────────────

// Open the bead-show page for the maps in-progress task and assert the
// response is non-error. Returns the Playwright response for callers that want
// to assert further on it.
export async function gotoBeadShow(page) {
  const res = await page.goto(`/${id()}`);
  expect(res, 'navigation response').not.toBeNull();
  expect(res.status(), `${KEY} (${id()}) bead-show HTTP status`).toBeLessThan(400);
  return res;
}

// ── Locators ────────────────────────────────────────────────────────────────
// Every locator is a pure function of `page` (or a scoped root) so consumers
// compose them without this module owning any page state.

// The bead-show inline-header status chip. Class hooks come from
// templates/partials/bead_show_inline_header.html:
//   <span class="bsh-chip bsh-chip-status bsh-chip-status-in_progress">in_progress</span>
export function statusChip(page) {
  return page.locator('.bead-show-inline-header .bsh-chip-status');
}

// The bead-show inline-header priority chip (see EXPECTED.priority note — this
// is ABSENT for the P0 fixture; the locator exists so consumers can assert its
// absence explicitly).
export function priorityChip(page) {
  return page.locator('.bead-show-inline-header .bsh-chip-priority');
}

// The sidebar Status field value on the bead-show page (the
// `Status` dt/dd row in dl.field-list). Scoped to the side aside so it does
// not collide with the inline-header chip.
export function statusFieldValue(page) {
  return page
    .locator('.bead-show-side dt', { hasText: /^Status$/ })
    .locator('xpath=following-sibling::dd[1]');
}

// The Dependencies section link to the parent epic on the bead-show page
// (templates/partials/deps.html — <a href=".../{epicId}"><code>{epicId}</code></a>).
export function parentEpicDepLink(page) {
  return page.locator(`.deps-section .dep-list a[href$="/${parentEpicId()}"]`);
}

// The gardener connected-beads DENSE-view "in_progress" lane (opt-in via
// ?view=dense on /gardener/{id}/beads — templates/partials/gardener_beads.html).
// `root` is typically the #cap-beads-host container after its htmx swap.
export function denseInProgressLane(root) {
  return root.locator('.cap-beads-lane[data-lane="in_progress"]');
}

// The dense-lane row for this task inside a given lane/root: the one-line node
// whose link points at the task's bead-show page.
export function denseNodeRow(root) {
  return root.locator(`.cap-beads-dnode:has(.cap-beads-dnode-link[href$="/${id()}"])`);
}

// ── Shared assertions ───────────────────────────────────────────────────────

// Pin the in_progress status contract on the bead-show page in ONE place:
// the inline-header status chip is visible, reads "in_progress", and carries
// the `bsh-chip-status-in_progress` CSS hook. Call after gotoBeadShow(page).
export async function assertInProgressBadge(page) {
  const chip = statusChip(page);
  await expect(chip, 'status chip visible').toBeVisible();
  await expect(chip).toHaveText(EXPECTED.status);
  await expect(chip).toHaveClass(new RegExp(`bsh-chip-status-${EXPECTED.status}`));
}

// Pin the deliberate ABSENCE of a priority chip for the P0 fixture (see the
// EXPECTED.priority note above). Call after gotoBeadShow(page).
export async function assertPriorityChipAbsent(page) {
  await expect(
    priorityChip(page),
    'P0 fixture renders no inline-header priority chip (Go template treats 0 as falsy)',
  ).toHaveCount(0);
}

// Pin the parent-epic parent-child edge on the bead-show deps section. Call
// after gotoBeadShow(page).
export async function assertParentEpicLink(page) {
  await expect(
    parentEpicDepLink(page),
    'bead-show deps links to the parent maps epic',
  ).toBeVisible();
}
