/* Browser check for the gardener panel (Lands workbench-glass spec WB-11, step 5.12), run from TestGardenerUnderChromium
   against /garden on cardWorld, whose gardener reader is fixtureGardenerPanels (gardener_test.go). Usage:
   node gardener_browser.js <base url> <node_modules dir>.

   Desktop: the gardenio project's Gardeners tab links "guard the plan" to its gardener panel, which opens here on About:
   the address says gardener= and tab=about and no longer project=, the header names it, the line under it says how it
   wakes, where, its runs and spend, its state says a run is out, and its levers are Pause, Run now, Cancel the last run,
   Wake on an event and Retire. A lever posts the gardener page's route asked for JSON; Retire asks first and, refused,
   posts nothing. About shows its goal, when it wakes and stops and its counters; Runs (the arrows walk the tabs) its three
   runs, newest first, the failed one marked; Beads what its runs touched by state, the empty state left out; Config its
   prompt, schedule and stop spec, and the editor, whose refusal says why and keeps what was typed, then saves; Successors
   its successor, and a form that wants a phase, a goal or a prompt advice before it posts. A paused gardener offers
   Resume; an ended one says its goal was reached and offers nothing. The project panel's New gardener wants an epic and a
   prompt, then posts /create-gardener with the project and preset and opens the new gardener. An unknown gardener says it
   is not there. Charts (step 4.2): its runs' five cards, each frame scoped to its three runs, kept in place across a
   refresh. Phone: the panel, its runs and its config editor fit the width. No console errors but the unknown
   gardener's 404 and the refused writes' 400. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));

function search(page) { return page.evaluate(function () { return location.search; }); }
async function shown(page, title, tab) {
  await page.waitForFunction(function (a) {
    var p = document.getElementById('bead'), sel = document.querySelector('#bv-body .pj-tabs [aria-selected=true]');
    return !p.hidden && p.dataset.kind === 'gardener' && document.getElementById('bv-title').textContent === a[0] && sel && sel.id === 'gd-tab-' + a[1];
  }, [title, tab]);
}
function pane(page) { return page.locator('#gd-pane').textContent(); }
function body(page) { return page.locator('#bv-body').textContent(); }
function levers(page) { return page.locator('#bv-body .gd-levers button').allTextContents(); }
function watch(page) {
  var errors = [];
  page.on('console', function (m) { if (m.type() === 'error') errors.push(m.text()); });
  page.on('pageerror', function (e) { errors.push(String(e)); });
  return errors;
}
/* The next POST to path, its form as a URLSearchParams and whether it asked for JSON. */
function posted(page, path) {
  return page.waitForRequest(function (r) { return r.method() === 'POST' && new URL(r.url()).pathname === path; }).then(function (r) {
    return { form: new URLSearchParams(r.postData() || ''), json: /application\/json/.test(r.headers().accept || '') };
  });
}

async function desktop(browser) {
  var ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } }), page = await ctx.newPage(), errors = watch(page);
  await page.goto(url + '/garden?project=gardenio&tab=gardeners');
  await page.waitForFunction(function () { return window.WORKBENCH && WORKBENCH.has('gardener') && !!document.querySelector('#pj-pane .qv-row'); });
  assert.strictEqual(await page.locator('#pj-pane a[href="/garden?gardener=gd-GardTrig01"]').count(), 1, 'a gardener row opens its panel');
  await page.click('#pj-pane a[href="/garden?gardener=gd-GardTrig01"]');
  await shown(page, 'guard the plan', 'about');
  var q = await search(page);
  assert.ok(/(^|[?&])gardener=gd-GardTrig01($|&)/.test(q) && /(^|[?&])tab=about($|&)/.test(q) && !/(^|[?&])project=/.test(q), 'the address names the gardener and its tab: ' + q);
  assert.strictEqual(await page.locator('#bead .bv-kind').textContent(), 'gardener loop');
  assert.strictEqual(await page.locator('#bv-body .bv-meta').textContent(), 'wakes on a schedule · in gardenio · 3 runs · $14.20 spent', 'the line under the title');
  assert.strictEqual(await page.locator('#bv-body .gd-state').textContent(), 'Running: a run is out now.');
  assert.deepStrictEqual(await levers(page), ['Pause', 'Run now', 'Wake on an event', 'Cancel the last run', 'Retire']);

  // About: goal, wakes and stops, counters
  var t = await pane(page);
  assert.ok(/Every crew PR is merged\./.test(t) && /Every 20 minutes/.test(t) && /Ends the loop: after 40 runs/.test(t), 'About: ' + t);
  assert.ok(/3 runs, 1 running/.test(t) && /12 spawned, 11 finished/.test(t) && /\*\/20 \* \* \* \* \(UTC\), fires in 7m/.test(t), 'its counters: ' + t);

  // a lever posts the page's route asked for JSON; Retire asks first, and refused posts nothing
  var p = posted(page, '/gardener/gd-GardTrig01/disable');
  await page.click('#bv-body .gd-levers button:has-text("Pause")');
  assert.ok((await p).json, 'Pause posts /disable asked for JSON');
  await page.waitForFunction(function () { return !document.querySelector('#bv-body .gd-levers button[disabled]'); });
  var asked = '', sent = false;
  page.once('dialog', function (d) { asked = d.message(); d.dismiss(); });
  var onReq = function (r) { if (r.method() === 'POST' && /\/retire$/.test(r.url())) sent = true; };
  page.on('request', onReq);
  await page.click('#bv-body .gd-levers button:has-text("Retire")');
  await page.waitForTimeout(300);
  page.off('request', onReq);
  assert.ok(/Retire the gardener “/.test(asked) && !sent, 'Retire asks first, naming the gardener; refused, it posts nothing: ' + asked);
  p = posted(page, '/gardener/gd-GardTrig01/mode');
  await page.click('#bv-body .gd-levers button:has-text("Wake on an event")');
  assert.strictEqual((await p).form.get('mode'), 'event', 'the mode flip names the mode');

  // Runs: the arrows walk the tabs; newest first, the failed one marked
  await page.waitForFunction(function () { return !document.querySelector('#bv-body .gd-levers button[disabled]'); });
  await page.focus('#gd-tab-about');
  await page.keyboard.press('ArrowRight');
  await shown(page, 'guard the plan', 'runs');
  assert.ok(/(^|[?&])tab=runs($|&)/.test(await search(page)), 'tab=runs');
  assert.strictEqual(await page.locator('#gd-pane .qv-row').count(), 3);
  t = await pane(page);
  assert.ok(/working · run 12 · \$0\.82 · 31 turns · 4m ago/.test(t) && /Walking PR #1043 to green/.test(t), 'Runs: ' + t);
  assert.ok(/^working · /.test(await page.locator('#gd-pane .qv-row').first().innerText()), 'a run row leads with its outcome (gd-tPd6ljOAsH)');
  assert.ok(t.indexOf('run 12') < t.indexOf('run 11') && t.indexOf('run 11') < t.indexOf('run 10'), 'newest first');
  assert.strictEqual(await page.locator('#gd-pane .qv-row.needs').count(), 1, 'the failed run is marked');
  assert.strictEqual(await page.locator('#gd-pane a[href="/garden?bead=gd-agentBeadW"]').count(), 1, 'a run opens its agent here');
  assert.ok(/on Roll the edge IdP out/.test(t), 'a run names its task by its title');

  // Beads: by state, the empty one left out
  await page.click('#gd-tab-beads');
  await shown(page, 'guard the plan', 'beads');
  t = await pane(page);
  assert.ok(/In progress \(1\)/.test(t) && /Blocked \(1\)/.test(t) && /Done \(1\)/.test(t) && !/Open \(/.test(t), 'Beads: ' + t);
  assert.strictEqual(await page.locator('#gd-tab-beads').textContent(), 'Beads 3', 'the tab counts every lane the pane lists (gd-TCstotDcIL)');

  // Config: as stored; the editor's refusal says why and keeps what was typed, then it saves
  await page.click('#gd-tab-config');
  await shown(page, 'guard the plan', 'config');
  t = await pane(page);
  assert.ok(/Walk every crew PR to green/.test(t) && /\*\/20 \* \* \* \*/.test(t) && /"max_runs": 40/.test(t), 'Config: ' + t);
  await page.click('#gd-pane button:has-text("Edit the config")');
  var form = page.locator('#gd-pane form.gd-form');
  await form.waitFor();
  assert.strictEqual(await form.locator('textarea[name=prompt]').inputValue(), 'Walk every crew PR to green; merge what is ready.');
  assert.ok(await form.locator('input[name=one_spawn_at_a_time]').isChecked(), 'a flag is a checkbox, as stored');
  await form.locator('input[name=run_budget]').fill('lots');
  await form.locator('button[type=submit]').click();
  await page.waitForFunction(function () { var s = document.querySelector('#gd-pane form.gd-form .bw-err'); return s && /Run budget must be a non-negative whole number/.test(s.textContent); });
  assert.strictEqual(await form.locator('input[name=run_budget]').inputValue(), 'lots', 'what was typed stays');
  await form.locator('input[name=run_budget]').fill('50');
  p = posted(page, '/gardener/gd-GardTrig01/config');
  await form.locator('button[type=submit]').click();
  var cf = (await p).form;
  assert.ok(cf.get('run_budget') === '50' && cf.get('cron') === '*/20 * * * *' && cf.get('one_spawn_at_a_time') === 'on' && !cf.has('retry_on_failure'), 'every field, a box only when ticked');
  await page.waitForFunction(function () { return !document.querySelector('#gd-pane form.gd-form'); });

  // Successors: listed; the form wants a phase, a goal or a prompt advice
  await page.click('#gd-tab-successors');
  await shown(page, 'guard the plan', 'successors');
  assert.ok(/phase=review/.test(await pane(page)), 'its successor');
  await page.click('#gd-pane button:has-text("Add a successor")');
  form = page.locator('#gd-pane form.gd-form');
  await form.waitFor();
  await form.locator('input[name=epic]').fill('gd-EdgeIdP001');
  await form.locator('button[type=submit]').click();
  await page.waitForFunction(function () { var s = document.querySelector('#gd-pane form.gd-form .bw-err'); return s && /phase, a terminal goal or a prompt advice/.test(s.textContent); });
  await form.locator('input[name=phase]').fill('deploy');
  p = posted(page, '/gardener/gd-GardTrig01/successor');
  await form.locator('button[type=submit]').click();
  var sf = (await p).form;
  assert.ok(sf.get('phase') === 'deploy' && sf.get('epic') === 'gd-EdgeIdP001', 'the successor as typed');
  await page.waitForFunction(function () { return !document.querySelector('#gd-pane form.gd-form'); });

  // paused offers Resume; ended says its goal was reached and offers nothing
  await page.goto(url + '/garden?gardener=gd-GardSched1');
  await shown(page, 'nightly sweep', 'about');
  assert.strictEqual(await page.locator('#bv-body .gd-state').textContent(), 'Paused: it does not fire until it is resumed.');
  assert.deepStrictEqual((await levers(page)).slice(0, 1), ['Resume']);
  await page.goto(url + '/garden?gardener=gd-GardDone01');
  await shown(page, 'drive the epic', 'about');
  assert.strictEqual(await page.locator('#bv-body .gd-state').textContent(), 'Ended, its goal reached: every child of its epic closed.');
  assert.strictEqual(await page.locator('#bv-body .gd-levers').count(), 0, 'an ended gardener has no levers');

  // New gardener from the project panel: an epic and a prompt, then the new gardener opens
  await page.goto(url + '/garden?project=gardenio&tab=gardeners');
  await page.waitForFunction(function () { return !!document.querySelector('#pj-pane .qv-row'); });
  await page.click('#pj-pane button:has-text("New gardener")');
  form = page.locator('#pj-pane form.pj-create');
  await form.waitFor();
  assert.strictEqual(await form.locator('select[name=preset]').inputValue(), 'epic-driver', 'the first preset is chosen');
  assert.ok(/Wakes every 30m/.test(await form.textContent()), 'with what it is for');
  await form.locator('button[type=submit]').click();
  await page.waitForFunction(function () { var s = document.querySelector('#pj-pane form.pj-create .bw-err'); return s && /Name the epic it drives/.test(s.textContent); });
  await form.locator('input[name=epic_id]').fill('gd-planA');
  await form.locator('textarea[name=prompt]').fill('Drive plan A to done.');
  await form.locator('select[name=preset]').selectOption('mr-green-watcher');
  p = posted(page, '/create-gardener');
  await form.locator('button[type=submit]').click();
  var nf = (await p).form;
  assert.ok(nf.get('project') === 'gardenio' && nf.get('preset') === 'mr-green-watcher' && nf.get('epic_id') === 'gd-planA' && !nf.has('cron'), 'the project, preset, epic, and no blank schedule');
  await shown(page, 'nightly sweep', 'about');

  // an unknown gardener is not there
  await page.goto(url + '/garden?gardener=nothere');
  await page.waitForFunction(function () { return /There is no gardener nothere, or you cannot see it\./.test(document.getElementById('bv-body').textContent); });

  assert.deepStrictEqual(errors.filter(function (e) { return !/\b(404|400)\b/.test(e); }), [], 'no console errors');
  await ctx.close();
}

/* The Charts tab (plan gd-gh5J0vVbJP step 4.2): its runs' cards from the stand-in Grafana, each frame scoped to the three
   runs the server read (never a name the page put together), and a refresh of the panel keeping the section and every
   frame in it where it is (a frame moved in the DOM would reload). */
async function charts(browser) {
  var ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } }), page = await ctx.newPage(), errors = watch(page);
  await page.goto(url + '/garden?gardener=gd-GardTrig01&tab=charts');
  await shown(page, 'guard the plan', 'charts');
  await page.waitForFunction(function () { return document.querySelectorAll('#gd-pane .gp-card').length === 5; });
  var titles = await page.locator('#gd-pane .gp-card .gp-t').allTextContents();
  assert.deepStrictEqual(titles, ['Spend by run', 'Cost over time', 'Context size', 'Turns', 'Tokens'], 'the gardener set, Spend by run first');
  await page.waitForFunction(function () { return !!document.querySelector('#gd-pane .gp-frame[src]'); });
  var agents = await page.evaluate(function () {
    var f = document.querySelector('#gd-pane .gp-frame[src]'); f.dataset.kept = '1';
    document.querySelector('#bv-body .bv-meta').dataset.old = '1';
    return new URL(f.getAttribute('src')).searchParams.get('var-agent');
  });
  assert.strictEqual(agents, 'gardenio-guard-run-12|gardenio-guard-run-11|gardenio-guard-run-10', 'its runs, newest first');
  await page.evaluate(function () { WORKBENCH.refresh('gardener', 'gd-GardTrig01'); });
  await page.waitForFunction(function () { var m = document.querySelector('#bv-body .bv-meta'); return !!m && !m.dataset.old; });
  assert.ok(await page.evaluate(function () { return !!document.querySelector('#gd-pane .gp-frame[data-kept="1"]') && document.querySelectorAll('#gd-pane .gp-card').length === 5; }),
    'a refresh keeps the cards and their frames in place');
  await page.click('#gd-tab-about');
  await shown(page, 'guard the plan', 'about');
  assert.strictEqual(await page.locator('#gd-pane .gp-card').count(), 0, 'another tab lets the charts go');
  assert.deepStrictEqual(errors, [], 'no console errors');
  await ctx.close();
}

async function phone(browser) {
  var ctx = await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true }), page = await ctx.newPage(), errors = watch(page);
  function wide() { return page.evaluate(function () { var b = document.getElementById('bv-body'); return b.scrollWidth - b.clientWidth; }); }
  await page.goto(url + '/garden?gardener=gd-GardTrig01&tab=runs');
  await shown(page, 'guard the plan', 'runs');
  assert.ok(await wide() <= 1, 'nothing runs off the side of the panel');
  await page.tap('#gd-tab-config');
  await shown(page, 'guard the plan', 'config');
  await page.tap('#gd-pane button:has-text("Edit the config")');
  await page.locator('#gd-pane form.gd-form').waitFor();
  assert.ok(await wide() <= 1, 'nor in the config editor');
  assert.deepStrictEqual(errors, [], 'no console errors');
  await ctx.close();
}

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); } catch (e) { console.log('no chromium: ' + e.message); process.exit(3); }
  try { await desktop(browser); await charts(browser); await phone(browser); } finally { await browser.close(); }
  console.log('gardener panel: ok');
})().catch(function (e) { console.error(e); process.exit(1); });
