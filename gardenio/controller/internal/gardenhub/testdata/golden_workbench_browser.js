/* The Commons v2 workbench's golden rows (plan gd-lRaCxsiLvC step 4, gd-sXB0CyaqTA), spread into golden_browser.js's
   SHOTS (kept here so that file stays under its line cap; the band's rows went with the band, gd-ZeiSZV7j2Y). At /garden/c/n/{id}: the
   tree, the node's document and the Jobs, Plan and Open rails, on an epic, on #general's gd-wishPorch as a thread and on a
   plan with its building step selected (the crew's jobs, the step marked among its phase's, the step's ask and PR); and
   (steward view S2, gd-LisW87zrnf) gardenio's steward as a root on a desktop; the fixture Slack wish as a root, its document
   the bead view's (gd-0GnBi7SRfK); its head, notes, the plans it tends, its
   #general threads with the composer, and its Tending, Asks for you and Jobs rails; on a phone (S3, gd-qV1nCdpbVA) the
   steward screen, its conversation under the Thread / Jobs / Tending / Asks strip.
   The phone's push stack is step 7 (gd-DhGFLKK0tj). */
'use strict';

/* The workbench open with its tree, sel's row selected (clicked; the root when none) and its document, transcript and rails
   filled. On a phone (step 7, gd-DhGFLKK0tj) the rows taps names are tapped in turn (sel's by default), each pushing a
   screen, and then tab's tab: with no taps it is the tree screen. */
function workbenchShown(sel, phone) {
  return async function (page, layout) {
    var taps = layout === 'phone' ? (phone && phone.taps) || (sel ? [sel] : []) : sel ? [sel] : [];
    await page.waitForSelector('#wb2 .wb2-row[aria-selected=true]', { state: 'visible' });
    for (var i = 0; i < taps.length; i++) {
      await page.locator('#wb2 .wb2-tree:not(.wb2-tree--kept) .wb2-row[data-id=' + taps[i] + '] > .wb2-line .wb2-t').click();
      if (layout === 'phone' && i < taps.length - 1) await page.waitForFunction(function (id) { return COMMONS2.state().root === id; }, taps[i]);
    }
    if (layout === 'phone' && phone && phone.tab) await page.locator('#wb2 .wb2-stab[data-tab=' + phone.tab + ']').click();
    await page.waitForFunction(function (id) {
      var r = document.querySelector('#wb2 .wb2-row[aria-selected=true]'), d = document.querySelector('#wb2 .wb2-doc');
      return r && (!id || r.dataset.id === id) && d && !d.hasAttribute('aria-busy') && d.querySelector('.wb2-head') &&
        !d.querySelector('.bv-cv-foot:empty') &&   // the bead view's Conversation has its composer (its place is read after)
        !/Loading/.test(d.textContent) && !/Loading/.test(document.querySelector('#wb2 .wb2-rails').textContent);
    }, taps[taps.length - 1] || '');
    await page.evaluate(function () { var a = document.activeElement; if (a && a.blur) a.blur(); });   // no focus ring in a golden
  };
}

/* The steward root shown: the workbench's (a phone's steward screen, which has no tree, once its document settles), then
   its conversation, its run's jobs and the notes read that offers Edit on Holds (the golden rig has no notes, so it offers
   none; the walk checks the editor, workbench_steward_browser.js). */
async function stewardShown(page, layout) {
  var phone = layout === 'phone';
  if (!phone) await workbenchShown()(page);
  else await page.waitForFunction(function () {
    var s = COMMONS2.state(), d = document.querySelector('#wb2 .wb2-doc');
    return s.steward && s.screen === 'node' && s.doc && s.doc === s.sel && d && !d.hasAttribute('aria-busy') && !/Loading/.test(d.textContent);
  });
  await page.waitForSelector('#wb2 .wb2-swtalk .cm-sw-thread', { state: 'visible' });
  await page.waitForSelector('#wb2 [data-rail=jobs] .cm-job', { state: phone ? 'attached' : 'visible' });   // a phone's is its Jobs tab
  await page.waitForSelector('#wb2 .wb2-face[data-holds]', { state: 'attached' });
  await page.evaluate(function () { var a = document.activeElement; if (a && a.blur) a.blur(); });
}

/* The rows. */
function rows() {
  return [
    // on a phone: the epic's task pushed (its Thread tab), the thread's tree screen, the plan's tree screen (its phases), and
    // the building step pushed from its phase on its Jobs tab
    { name: 'commons', path: '/garden/c/n/gd-wbEpic', show: workbenchShown('gd-wbEt3') },
    { name: 'commonsthread', path: '/garden/c/n/gd-wishPorch', show: workbenchShown() },
    // the fixture Slack wish (commonstest.WishFamily) as a root: its document is the bead view's, its face and Conversation
    // the panel's own (gd-0GnBi7SRfK, TestOneBeadViewUnderChromium), its tree the task it gave rise to with its crew
    { name: 'commonswish', path: '/garden/c/n/gd-wishSlack', show: workbenchShown() },
    // and on a phone with its document pushed (its Thread tab), the conversation opened on its newest line (gd-1jjjx7CMU3)
    { name: 'commonswishdoc', path: '/garden/c/n/gd-wishSlack', devices: ['phone'], show: workbenchShown('gd-wishSlack', { taps: ['gd-wishSlack'] }) },
    { name: 'commonsplan', path: '/garden/c/n/gd-wbPlan', show: workbenchShown('gd-wbS3', { taps: [] }) },
    { name: 'commonsjobs', path: '/garden/c/n/gd-wbPhase1', devices: ['phone'], show: workbenchShown('gd-wbS3', { tab: 'jobs' }) },
    { name: 'commonssteward', path: '/garden/c/s/gardenio', perfOn: ['phone'], show: stewardShown },
  ];
}

module.exports = { rows: rows, workbenchShown: workbenchShown };
