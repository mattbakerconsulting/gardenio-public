/* Browser check for the settings panel (Lands workbench-glass spec WB-13, step 5.12), run from TestSettingsUnderChromium
   against /garden on cardWorld, whose settings reader is fixtureSettings (gardener_test.go). Usage:
   node settings_browser.js <base url> <node_modules dir>.

   Desktop: the Menu's Settings opens the panel here: the address says settings=me, the header says Your settings, the
   line under it who is signed in; Spawning shows the default project and role, that a prompt template is set and the
   template itself, and one agent for a selection; You shows the name, pronouns and team, a blank as a dash. Edit opens one
   form with the projects and roles to choose from; a Slack channel that is no channel id is refused in place and nothing is
   posted; a save the route refuses says why and keeps the form; a good save posts POST /settings asked for JSON with
   scope=garden and only the panel's fields (the fake refuses anything else) and closes the form. Closing the panel drops
   settings= from the address, and /garden?settings=me opens it straight. Phone: the panel and its form fit the width.
   No console errors but the refused save's 502. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));

function search(page) { return page.evaluate(function () { return location.search; }); }
async function shown(page) {
  await page.waitForFunction(function () {
    var p = document.getElementById('bead');
    return !p.hidden && p.dataset.kind === 'settings' && document.getElementById('bv-title').textContent === 'Your settings' && !!document.querySelector('#bv-body .pj-sec');
  });
}
function body(page) { return page.locator('#bv-body').textContent(); }
function watch(page) {
  var errors = [];
  page.on('console', function (m) { if (m.type() === 'error') errors.push(m.text()); });
  page.on('pageerror', function (e) { errors.push(String(e)); });
  return errors;
}

async function desktop(browser) {
  var ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } }), page = await ctx.newPage(), errors = watch(page);
  await page.goto(url + '/garden');
  await page.waitForFunction(function () { return window.WORKBENCH && WORKBENCH.has('settings'); });
  await page.click('#cam-menu');
  await page.click('#cam-settings');
  await shown(page);
  assert.ok(/(^|[?&])settings=me($|&)/.test(await search(page)), 'the address names the panel');
  assert.strictEqual(await page.locator('#bead .bv-kind').textContent(), 'settings');
  assert.strictEqual(await page.locator('#bv-body .bv-meta').textContent(), 'Signed in as operator@example.com');
  assert.ok(await page.evaluate(function () { return document.getElementById('menu-list').hidden; }), 'the Menu closes');
  var t = await body(page);
  assert.ok(/Default projectgardenio/.test(t) && /Default rolecrew/.test(t) && /Prompt templateset \(below\)/.test(t) && /Work \{\{id\}\}: \{\{title\}\}\./.test(t), 'Spawning: ' + t);
  assert.ok(/one agent for all of them/.test(t), 'a selection gets one agent');
  assert.ok(/Display nameOperator/.test(t) && /Pronounsthey\/them/.test(t) && /Teamcore/.test(t) && /Slack DM channel—/.test(t), 'You: ' + t);

  // the editor: choices, a refused channel id in place, a refused save, a good save
  await page.click('#bv-body button:has-text("Edit your settings")');
  var form = page.locator('#bv-body form.st-form');
  await form.waitFor();
  assert.strictEqual(await form.locator('select[name=default_spawn_project]').inputValue(), 'gardenio');
  assert.deepStrictEqual(await form.locator('select[name=default_spawn_project] option').evaluateAll(function (os) { return os.map(function (o) { return o.value; }); }), ['', 'acme', 'gardenio', 'music']);
  assert.deepStrictEqual(await form.locator('select[name=default_spawn_role] option').evaluateAll(function (os) { return os.map(function (o) { return o.value; }); }), ['', 'crew', 'gardener', 'thread']);
  assert.strictEqual(await form.locator('textarea[name=default_spawn_prompt_template]').inputValue(), 'Work {{id}}: {{title}}.');
  var sent = 0, onReq = function (r) { if (r.method() === 'POST' && new URL(r.url()).pathname === '/settings') sent++; };
  page.on('request', onReq);
  await form.locator('input[name=slack_channel_id]').fill('#general');
  await form.locator('button[type=submit]').click();
  await page.waitForFunction(function () { var s = document.querySelector('#bv-body form.st-form .bw-err'); return s && /looks like C0123ABC/.test(s.textContent); });
  assert.strictEqual(sent, 0, 'a channel that is no channel id is refused before it is posted');
  page.off('request', onReq);
  await form.locator('input[name=slack_channel_id]').fill('D0123ABC');
  await form.locator('input[name=team]').fill('refuse');
  await form.locator('button[type=submit]').click();
  await page.waitForFunction(function () { var s = document.querySelector('#bv-body form.st-form .bw-err'); return s && /Failed to save preferences\./.test(s.textContent); });
  assert.strictEqual(await form.locator('input[name=team]').inputValue(), 'refuse', 'what was typed stays');
  await form.locator('input[name=team]').fill('core');
  await form.locator('select[name=spawn_per_selection]').selectOption('per-bead');
  var p = page.waitForRequest(function (r) { return r.method() === 'POST' && new URL(r.url()).pathname === '/settings'; });
  await form.locator('button[type=submit]').click();
  var req = await p, f = new URLSearchParams(req.postData() || '');
  assert.ok(/application\/json/.test(req.headers().accept || ''), 'asked for JSON');
  assert.ok(f.get('scope') === 'garden' && f.get('spawn_per_selection') === 'per-bead' && f.get('slack_channel_id') === 'D0123ABC' && !f.has('vibe_mode'), 'scope=garden and only its fields');
  await page.waitForFunction(function () { return !document.querySelector('#bv-body form.st-form'); });

  // closing drops settings=; the address opens it straight
  await page.click('#bv-x');
  await page.waitForFunction(function () { return document.getElementById('bead').hidden; });
  assert.ok(!/(^|[?&])settings=/.test(await search(page)), 'closed, the address does not name it');
  await page.goto(url + '/garden?settings=me');
  await shown(page);

  assert.deepStrictEqual(errors.filter(function (e) { return !/\b502\b/.test(e); }), [], 'no console errors');
  await ctx.close();
}

async function phone(browser) {
  var ctx = await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true }), page = await ctx.newPage(), errors = watch(page);
  function wide() { return page.evaluate(function () { var b = document.getElementById('bv-body'); return b.scrollWidth - b.clientWidth; }); }
  await page.goto(url + '/garden?settings=me');
  await shown(page);
  assert.ok(await wide() <= 1, 'nothing runs off the side of the panel');
  await page.tap('#bv-body button:has-text("Edit your settings")');
  await page.locator('#bv-body form.st-form').waitFor();
  assert.ok(await wide() <= 1, 'nor in the form');
  assert.deepStrictEqual(errors, [], 'no console errors');
  await ctx.close();
}

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); } catch (e) { console.log('no chromium: ' + e.message); process.exit(3); }
  try { await desktop(browser); await phone(browser); } finally { await browser.close(); }
  console.log('settings panel: ok');
})().catch(function (e) { console.error(e); process.exit(1); });
