/* The golden script's heraldry shots and helpers: the bead view's arms (steps 3.2-3.4) and a gardener's task arms (step 5.9,
   gd-QboM9sPYCg), kept out of golden_browser.js so that file stays under quench's line cap. h is the golden script's own into,
   look, undo and plotsClip, and plan, the plan P plot's selector, read when a shot runs. */
'use strict';
var assert = require('assert');

module.exports = function (h) {
  /* The face fixtures bearing arms the bead view draws (faceBeads): the epic and plan F. gd-FaceWork01, a task, bears arms too. */
  var FACE_ARMS = { 'gd-FaceEpic01': true, 'gd-FacePlan01': true };
  /* Every arms picture and charge in the bead view's viewport has loaded (an <img> the settle step does not wait for). */
  async function armsDrawn(page) {
    await page.waitForFunction(function () {
      return Array.prototype.every.call(document.querySelectorAll('#bv-body .bv-arms img'), function (i) {
        var r = i.getBoundingClientRect();
        return (r.bottom <= 0 || r.top >= innerHeight || !r.width) && !i.closest('.ar-pic') ? true : i.complete && i.naturalWidth > 0;
      });
    });
  }
  /* The Heraldry section with Change arms open, from its heading to its end or the bead view's bottom, inside the viewport.
     The section's edges are rounded, as Chromium paints a box at its rounded position: its top's fraction is set by the
     layout above it, and a floor moved the clip a row off the paint, and a row short of the pane's fixed bottom, when that
     fraction crossed a whole pixel (gd-g2bDuHkJYp). 9 px over the rounded top is the row the goldens start on. */
  async function armsClip(page) {
    return page.evaluate(function () {
      var p = document.getElementById('bead').getBoundingClientRect(), s = document.querySelector('#bv-body .bv-arms').getBoundingClientRect();
      var x = Math.floor(p.left), y = Math.max(Math.floor(p.top), Math.round(s.top) - 9), end = Math.min(Math.round(s.bottom) + 8, Math.floor(p.bottom), window.innerHeight);
      return { x: x, y: y, width: Math.ceil(p.right) - x, height: end - y };
    });
  }
  /* Every gardener's task arms in view (.ar-gd: a Porch row) has loaded. They are lazy
     (web/arms.js pic, 5.9), so only a shown picture is fetched: the other sky's, a hidden Porch's and a row scrolled away are
     not waited for. In view is inside the viewport and every clipping box round the picture. */
  async function gdArmsDrawn(page) {
    await page.waitForFunction(function () {
      return Array.prototype.every.call(document.querySelectorAll('.ar-gd img'), function (i) {
        var r = i.getBoundingClientRect(), top = 0, bot = innerHeight;
        for (var a = i.parentElement; a; a = a.parentElement) if (getComputedStyle(a).overflowY !== 'visible') { var q = a.getBoundingClientRect(); top = Math.max(top, q.top); bot = Math.min(bot, q.bottom); }
        return !r.width || r.bottom <= top || r.top >= bot || (i.complete && i.naturalWidth > 0);
      });
    });
  }
  /* The arms scenes posted (transitions_test.go mountFarmScenes): the farm publishes g-arms-p and g-arms-pa, and the page has
     them. */
  async function armsScene(page) {
    await page.waitForFunction(function () { return window.GARDEN.known('gd-gDownB').title === 'g-down-b'; });
    for (var n of ['arms', 'armspa']) assert.strictEqual((await page.request.post(new URL(page.url()).origin + '/test/farm/scene?name=' + n)).status(), 204, n + ': the farm published the patch');
    await page.waitForFunction(function () { return window.GARDEN.known('gd-gArmsP').title === 'g-arms-p' && window.GARDEN.known('gd-gArmsPa').title === 'g-arms-pa'; });
  }
  /* The two armed gardeners on plan P at level lv, their badges' pictures loaded (HERALDRY.picture answers once a bitmap is in,
     and asks for it when it is not), then a frame to draw them. On a phone the camera then drops the plot 40 px: the title
     bar's strip wraps to three lines clear of the camera column (gd-hX9BRyBq1G), so plan P sits up under the Mine/Everyone toggle,
     which would cover the badge over g-arms-p's head that the shot is for. */
  function armsShown(lv) {
    return async function (page, device) {
      await armsScene(page);
      await h.into(page, h.plan());
      var at = await page.evaluate(function () { return window.GARDEN.cam().level; });
      if (at !== lv && lv === 3) { await h.look(page, { z: 1.12 }); await h.look(page, { z: 1 }); }
      else if (at !== lv) await h.look(page, { z: 0.82 });
      await page.waitForFunction(function (lv) { return window.GARDEN.cam().level === lv; }, lv);
      if (device === 'phone') {
        var c = await page.evaluate(function () { return window.GARDEN.cam(); });
        await h.look(page, { cy: c.cam.cy - 40 / c.z });
        await page.waitForFunction(function (lv) { return window.GARDEN.cam().level === lv; }, lv);
      }
      await page.waitForFunction(function () {
        var doc = window.GARDEN.farm(), night = document.documentElement.getAttribute('data-sky') === 'night', z = window.GARDEN.cam().z;
        return (doc.gardeners || []).filter(function (g) { return g.task_arms && (g.id === 'gd-gArmsP' || g.id === 'gd-gArmsPa'); }).length === 2 && (doc.gardeners || []).every(function (g) {
          return !g.task_arms || !!window.HERALDRY.picture(g.task_arms.canonical, window.HERALDRY.BADGE * z, window.devicePixelRatio, night);
        });
      });
      await page.evaluate(function () { return new Promise(function (r) { requestAnimationFrame(function () { requestAnimationFrame(r); }); }); });
    };
  }
  /* a gardener's task arms (heraldry step 5.9): the arms scenes put two gardeners on plan P at the Bead level, each with its
     line's arms as a badge over its head, the plan's for g-arms-p and the first phase's for g-arms-pa (the plan's picture stays
     at the gate); and the Porch's rows for them, the run before "Tending:", light and dark */
  var shots = [
    { name: 'gardenerarms', path: '/garden?at=gardenio/gd-planP', clip: h.plotsClip(['gd-planP'], 24, 30), show: armsShown(3), after: h.undo(['arms', 'armspa']) },
    { name: 'porcharms', path: '/garden', show: async function (page, device) {
      await armsScene(page);
      if (device === 'phone') await page.tap('#tabbar [data-go=porch]'); else await page.click('#dbtn-porch');
      await page.locator('#d-porch').waitFor({ state: 'visible' });
      await page.waitForSelector('#d-porch [data-gid=gd-gArmsPa] .gd-task', { state: 'attached' });
      var mid = function () { return page.evaluate(function () { document.querySelector('#d-porch [data-gid=gd-gArmsP]').scrollIntoView({ block: 'center', behavior: 'instant' }); }); };
      await mid();   // the scene's two rows, under the Porch's first screen
      await page.waitForFunction(function () { return document.querySelectorAll('#d-porch [data-gid=gd-gArmsP] .gd-task .ar-day, #d-porch [data-gid=gd-gArmsPa] .gd-task .ar-day').length === 2; });
      await gdArmsDrawn(page); await mid(); await gdArmsDrawn(page);   // again once the rows round them wear theirs
    }, after: h.undo(['arms', 'armspa']) },
  ];
  return { gdArmsDrawn: gdArmsDrawn, armsDrawn: armsDrawn, armsClip: armsClip, FACE_ARMS: FACE_ARMS, shots: shots };
};
