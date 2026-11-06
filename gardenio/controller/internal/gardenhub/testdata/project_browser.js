/* Browser check for the project panel (Lands workbench-glass spec WB-10, step 5.11), run from TestProjectUnderChromium
   against /garden on cardWorld, whose project reader is fixtureProjects (project_test.go). Usage:
   node project_browser.js <base url> <node_modules dir>.

   Desktop: with the cursor on the gardenio project (?at=gardenio), Space opens its panel here: the address says project=
   and tab=decisions (an ask waits there), the header names the project, the line under it says who works there and what
   waits. The Decisions tab carries the Queues panel's rows with the Mailbox's answer box, and what is blocked. The tabs
   move the address (tab=) and walk with the arrow keys; Agents names each agent and its task as links here, Gardeners a
   trigger and a schedule that is off, Accounts the account and its spend, Cost the plot's spend and budget and its
   accounts' spend and, under them, the project's charts (step 2.2: the route's refusal in words, then each card's words
   and Open link from a set with Grafana set up, range chips that walk with the arrows and are remembered, the section kept
   in place across a refresh), Health its words and the health-land set's cards (step 4.1), Activity what grows and what moved, Config the config as stored with its finding. The config editor
   saves (POST /p/gardenio/config asked for JSON) and says so; a refused field says why beside it and keeps what was typed.
   A tab named in the address opens on it (the retired /active and /feed land on tab=activity). acme, a tenant land, says
   in words that its purse is at the wall and that its gardeners could not be read, and its header has no Credential pool;
   gardenio's (an operator land, the viewer an admin) opens the pool here (GL-15, web/creds.js), read only. Closing the panel drops tab= from the
   address, and an unknown project says it is not there. orchard, a land the viewer may share (LW.5), has a Share tab that
   says what the link grants and withholds, links the preview, mints a link shown once and revokes its live window; acme
   has no Share tab. Phone: the panel and its tabs fit the width. No console errors
   but the unknown project's 404 and the refused field's 422. */
'use strict';
var url = process.argv[2], mods = process.argv[3], assert = require('assert');
var pw = require(require.resolve('playwright-core', { paths: [mods] }));

function search(page) { return page.evaluate(function () { return location.search; }); }
/* the address's query as its sorted key=value pairs: the map's own address write (garden.js writeURL) may move at= to the end */
function params(page) { return page.evaluate(function () { return Array.from(new URLSearchParams(location.search)).map(function (p) { return p.join('='); }).sort().join('&'); }); }
async function shown(page, name, tab) {
  await page.waitForFunction(function (a) {
    var p = document.getElementById('bead'), sel = document.querySelector('#bv-body .pj-tabs [aria-selected=true]');
    return !p.hidden && p.dataset.kind === 'project' && document.getElementById('bv-title').textContent === a[0] && sel && sel.id === 'pj-tab-' + a[1];
  }, [name, tab]);
}
function pane(page) { return page.locator('#pj-pane').textContent(); }
function watch(page) {
  var errors = [];
  page.on('console', function (m) { if (m.type() === 'error') errors.push(m.text()); });
  page.on('pageerror', function (e) { errors.push(String(e)); });
  return errors;
}

async function desktop(browser) {
  var ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } }), page = await ctx.newPage(), errors = watch(page);
  await page.goto(url + '/garden?at=gardenio');
  await page.waitForFunction(function () { return window.WORKBENCH && WORKBENCH.has('project') && !!document.querySelector('#hots .hot.cur'); });
  await page.focus('#hots .hot.cur');
  await page.keyboard.press(' ');
  await shown(page, 'gardenio', 'decisions');
  var q = await search(page);
  assert.ok(/(^|[?&])project=gardenio($|&)/.test(q) && /(^|[?&])tab=decisions($|&)/.test(q), 'the address names the panel and its tab: ' + q);
  assert.strictEqual(await page.locator('#bead .bv-kind').textContent(), 'project');
  var meta = await page.locator('#bv-body .bv-meta').textContent();
  assert.strictEqual(meta, '2 agents · 2 gardeners · 3 asks wait · $4.20 todayBulk edits: old viewerCredential pool', 'the line under the title');
  // a viewer who may edit is offered the project's bulk edits, LATER (spec 2.3), on its Work list in the old viewer (Lands 5.13)
  assert.ok(/\/p\/gardenio\?view=list$/.test(await page.locator('#bv-body .bv-meta a').first().getAttribute('href')), 'Bulk edits links the Work list');
  // gardenio is an operator land and the viewer an admin, so its header opens the credential pool (GL-15, web/creds.js)
  assert.strictEqual(await page.locator('#bv-body .bv-meta a').nth(1).getAttribute('href'), '/garden?creds=pool', 'Credential pool opens the pool here');
  assert.strictEqual(await page.locator('#pj-tab-decisions').textContent(), 'Decisions 3');

  // Decisions: the Queues panel's rows, each decision with its answer box; then what is blocked
  assert.strictEqual(await page.locator('#pj-pane .qv-row').count(), 4, 'two decisions, an escalation and a blocked bead');
  assert.ok(await page.locator('#pj-pane .qv-row[data-id="gd-FaceDecide1"] .act').count() >= 1, 'a decision carries the answer box');
  assert.ok(/Turn the old IdP off/.test(await pane(page)), 'what is blocked');

  // the tabs walk with the arrows and move the address
  await page.focus('#pj-tab-decisions');
  await page.keyboard.press('ArrowRight');
  await shown(page, 'gardenio', 'config');
  assert.ok(/(^|[?&])tab=config($|&)/.test(await search(page)), 'tab=config');
  assert.ok(await page.evaluate(function () { return document.activeElement && document.activeElement.id === 'pj-tab-config'; }), 'focus moves to the tab');
  await page.keyboard.press('Home');
  await shown(page, 'gardenio', 'agents');
  var t = await pane(page);
  assert.ok(/gardenio-73-github-idp/.test(t) && /working · crew · 4m ago/.test(t) && /on Roll the edge IdP out/.test(t), 'Agents: ' + t);
  assert.strictEqual(await page.locator('#pj-pane a[href="/garden?bead=gd-EdgeIdP002"]').count(), 1, 'its task is a link here');

  await page.click('#pj-tab-gardeners');
  await shown(page, 'gardenio', 'gardeners');
  t = await pane(page);
  assert.ok(/guard the plan/.test(t) && /trigger · on · 12 runs/.test(t) && /schedule · off · 3 runs/.test(t), 'Gardeners: ' + t);
  await page.click('#pj-tab-accounts');
  await shown(page, 'gardenio', 'accounts');
  assert.ok(/anthropic · claude-opus · active · \$4\.20 today · \$31\.50 this week · \$412\.00 in all/.test(await pane(page)), 'Accounts: ' + await pane(page));
  await page.click('#pj-tab-cost');
  await shown(page, 'gardenio', 'cost');
  t = await pane(page);
  assert.ok(/Spend/.test(t) && /\$300\.00 a month/.test(t) && /\$180\.00/.test(t) && /Its accounts/.test(t), 'Cost: ' + t);
  assert.ok(!/Purse/.test(t), 'an operator land has no purse');
  // under the figures, the project's charts (step 2.2). The card world's farm has no gardenio project bead, so no cost row,
  // but it draws gardenio's plans, so the route charts it (gd-vViKmjtrTj) from the stand-in Grafana, with the chips, each
  // card titled as a card under the Charts heading (an h4 under its h3, 2.3b gd-mrcuxcJyyj)
  await page.waitForFunction(function () { return document.querySelectorAll('#pj-pane .pj-charts .pj-cards > section.gp-card').length > 0; });
  assert.ok(await page.locator('#pj-pane .pj-chips').isVisible(), 'charts: the chips are there');
  assert.strictEqual(await page.locator('#pj-pane .pj-off').isVisible(), false, 'and no line saying they are off');
  assert.strictEqual(await page.locator('#pj-pane .pj-charts .pj-cards > section.gp-card > .gp-head > h4.gp-t').first().textContent(), 'Spend (window)', 'a card title is an h4');
  assert.strictEqual(await page.locator('#pj-pane .pj-charts .pj-cards h3').count(), 0, 'no card heading at the section\'s level');
  assert.strictEqual(await page.locator('#pj-pane .pj-chips [aria-checked=true]').textContent(), '7 days', 'a week by default');
  // with Grafana set up the set answers its cards: each card's words and Open link, the dashboard's and the sign-in line
  var asked = [];
  await page.route('**/garden/charts/costs-project?*', function (route) {
    asked.push(route.request().url());
    var g = 'https://grafana.example/d/gardenio-garden-costs', r = /range=([^&]*)/.exec(route.request().url())[1];
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ set: 'costs-project', project: 'gardenio', range: r, theme: 'dark',
      open_dashboard_url: g + '?var-project=gardenio', panels: [
        { id: 1, title: 'Spend (window)', scope: 'USD · the chip’s window', note: '', src: 'about:blank', open_url: g + '?viewPanel=1' },
        { id: 6, title: 'Spend by agent', scope: 'USD · top 10 agents', note: 'The range chips do not move this figure.', src: 'about:blank', open_url: g + '?viewPanel=6' }] }) });
  });
  // a range chip re-reads the set and is remembered; the address does not move
  q = await params(page);
  await page.focus('#pj-pane .pj-chips [aria-checked=true]');
  await page.keyboard.press('ArrowLeft');
  await page.waitForFunction(function () { var b = document.querySelector('#pj-pane .pj-chips [aria-checked=true]'); return b && b.textContent === 'Today' && document.activeElement === b; });
  // a card is a section labelled by its title, whichever painted it: the Costs panel's (web/costs.js) or the words-only one
  await page.waitForFunction(function () { return document.querySelectorAll('#pj-pane .pj-charts .pj-cards > section[aria-labelledby]').length === 2; });
  assert.strictEqual(await page.locator('#pj-pane .pj-charts .pj-cards > section.gp-card').count(), 2, 'the Costs panel painted them (COSTS.paintCostCards)');
  assert.ok(/[?&]project=gardenio&range=today&theme=dark$/.test(asked[asked.length - 1]), 'the chip asks for its range, dark: ' + asked);
  assert.strictEqual(await page.evaluate(function () { return localStorage.getItem('garden.costs.range'); }), 'today');
  assert.strictEqual(await params(page), q, 'the address keeps its params');
  assert.ok(!/(^|[?&])range=/.test(await search(page)), 'the range is not in the address');
  t = await page.locator('#pj-pane .pj-charts').textContent();
  assert.ok(/^Charts/.test(t) && /Spend by agent/.test(t) && /The range chips do not move this figure\./.test(t) && /Charts blank\? Open Grafana once to sign in/.test(t), 'Charts: ' + t);
  assert.strictEqual(await page.locator('#pj-pane .pj-cards a[aria-label="Open Spend by agent in Grafana"]').getAttribute('href'), 'https://grafana.example/d/gardenio-garden-costs?viewPanel=6');
  assert.strictEqual(await page.locator('#pj-pane .pj-charts a:has-text("Open Grafana once to sign in")').getAttribute('href'), 'https://grafana.example/');
  assert.strictEqual(await page.locator('#pj-pane .pj-charts a:has-text("Open the dashboard in Grafana")').count(), 1);
  // a refresh of the tab keeps the charts where they are, so no frame is reloaded each time the farm moves
  await page.evaluate(function () { window.__charts = document.querySelector('#pj-pane .pj-charts'); window.__meta = document.querySelector('#bv-body .bv-meta'); WORKBENCH.refresh('project', 'gardenio'); });
  await page.waitForFunction(function () { var m = document.querySelector('#bv-body .bv-meta'); return m && m !== window.__meta; });
  assert.ok(await page.evaluate(function () {
    var c = document.querySelector('#pj-pane .pj-charts'), kids = document.getElementById('bv-body').children;
    return c === window.__charts && c.parentNode.lastElementChild === c && kids[kids.length - 1].id === 'pj-pane' && document.querySelectorAll('#pj-pane').length === 1;
  }), 'the same charts section, last in the one pane');
  assert.ok(/Its accounts/.test(await pane(page)), 'the figures are painted again above it');
  // cards with no chart (Grafana not set up): the chips go and one line says why (gd-TGYiliW9OR)
  await page.unroute('**/garden/charts/costs-project?*');
  await page.route('**/garden/charts/costs-project?*', function (route) {
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ set: 'costs-project', project: 'gardenio', range: '7d', theme: 'dark',
      panels: [{ id: 1, title: 'Spend (window)', scope: 'USD', note: '', src: '', open_url: '' }] }) });
  });
  await page.click('#pj-pane .pj-chips [data-range="7d"]');
  await page.waitForFunction(function () { var o = document.querySelector('#pj-pane .pj-off'); return o && !o.hidden; });
  assert.strictEqual(await page.locator('#pj-pane .pj-off').textContent(), 'No range to choose: Grafana isn’t set up here.');
  assert.strictEqual(await page.locator('#pj-pane .pj-chips').isVisible(), false, 'no chip to press that changes nothing');
  await page.unroute('**/garden/charts/costs-project?*');
  // Health (step 4.1, gd-SKVLOuGToB): its words, then the health-land set's cards in the Cost tab's section, on the range
  // the Cost tab's chips left; an operator land is not a tenant one, so it says its own land charts are not shown
  var askedHealth = [];
  await page.route('**/garden/charts/health-land?*', function (route) {
    askedHealth.push(route.request().url());
    var g = 'https://grafana.example/d/gardenio-garden-health';
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ set: 'health-land', project: 'gardenio', range: '7d', theme: 'dark',
      open_dashboard_url: g + '?var-project=gardenio', panels: [
        { id: 1, title: 'SLO burn rate', scope: '× budget · every Lands SLO', note: '', src: 'about:blank', open_url: g + '?viewPanel=1' },
        { id: 2, title: 'Error budget left', scope: 'the last 30 days', note: 'The range chips do not move this figure.', src: 'about:blank', open_url: g + '?viewPanel=2' }] }) });
  });
  await page.click('#pj-tab-health');
  await shown(page, 'gardenio', 'health');
  assert.ok(/[?&]tab=health(&|$)/.test(await search(page)), 'the address says tab=health');
  await page.waitForFunction(function () { return document.querySelectorAll('#pj-pane .pj-charts .pj-cards > section[aria-labelledby]').length === 2; });
  t = await pane(page);
  assert.ok(/Lands SLOs/.test(t) && /not a tenant land/.test(t) && /SLO burn rate/.test(t) && /Error budget left/.test(t), 'Health: ' + t);
  assert.ok(/\/garden\/charts\/health-land\?project=gardenio&range=7d&theme=dark$/.test(askedHealth[askedHealth.length - 1]), 'the Health tab asks for health-land: ' + askedHealth);
  assert.strictEqual(await page.locator('#pj-pane a[aria-label="Open SLO burn rate in Grafana"]').getAttribute('href'), 'https://grafana.example/d/gardenio-garden-health?viewPanel=1');
  await page.unroute('**/garden/charts/health-land?*');
  await page.click('#pj-tab-activity');
  await shown(page, 'gardenio', 'activity');
  t = await pane(page);
  assert.ok(/Growing now/.test(t) && /Moved last/.test(t) && /epic · closed · 2h ago/.test(t), 'Activity: ' + t);

  // Config: as stored, its finding; the editor saves, and a refused field says why
  await page.click('#pj-tab-config');
  await shown(page, 'gardenio', 'config');
  t = await pane(page);
  assert.ok(/https:\/\/github\.com\/acme\/gardenio/.test(t) && /Docker, Schedules, Triggers/.test(t) && /GITHUB_TOKEN/.test(t), 'Config: ' + t);
  assert.ok(/memory_limit is set without memory_request/.test(t), 'its finding');
  await page.click('#pj-pane button:has-text("Edit the config")');
  var form = page.locator('#pj-pane form.pj-form');
  await form.waitFor();
  await form.locator('input[name=cpu_request]').fill('lots');
  await form.locator('button[type=submit]').click();
  await page.waitForFunction(function () { var e = document.querySelector('#pj-pane form.pj-form input[name=cpu_request][aria-invalid=true]'); return !!e; });
  assert.strictEqual(await page.locator('#pj-pane form.pj-form input[name=cpu_request]').inputValue(), 'lots', 'what was typed stays');
  assert.ok(/not a valid Kubernetes quantity/.test(await page.locator('#pj-pane form.pj-form').textContent()), 'the field says why');
  await page.locator('#pj-pane form.pj-form input[name=cpu_request]').fill('750m');
  await page.locator('#pj-pane form.pj-form button[type=submit]').click();
  await page.waitForFunction(function () { return /Saved the config\./.test(document.getElementById('bv-body').textContent) || !document.querySelector('#pj-pane form.pj-form'); });

  // a tab in the address opens on it; acme's purse is at the wall, its gardeners unread
  await page.goto(url + '/garden?project=acme&tab=cost');
  await shown(page, 'acme', 'cost');
  t = await pane(page);
  assert.ok(/^Purse/.test(t) && /At the wall: 1\.5 credits left/.test(t) && /New spawns are stopped/.test(t), 'acme Cost: ' + t);
  assert.ok(/at the wall/.test(await page.locator('#bv-body .bv-meta').textContent()), 'the line under the title says so');
  assert.ok(/^1 agent · /.test(await page.locator('#bv-body .bv-meta').textContent()) && /In flight2 credits held for 1 running agent/.test(t), 'the summary and the purse count the same agent: ' + t);
  assert.ok(!/Credential pool/.test(await page.locator('#bv-body .bv-meta').textContent()), 'a tenant land\'s header does not open the fleet\'s pool');
  assert.strictEqual(await page.locator('#pj-tab-share').count(), 0, 'acme is not the viewer\'s to share: no Share tab');
  await page.click('#pj-tab-gardeners');
  await shown(page, 'acme', 'gardeners');
  assert.ok(/Could not read its gardeners: the beads daemon did not answer\./.test(await pane(page)), 'a section the daemon did not answer says so');
  assert.strictEqual(await page.locator('#pj-pane .bv-retry').count(), 1, 'with Try again');

  // the operator land's header opens the credential pool in place (GL-15): read only, each account's health in words, its
  // two windows as meters, its line; the header's link is the way back
  await page.goto(url + '/garden?project=gardenio&tab=agents');
  await shown(page, 'gardenio', 'agents');
  await page.click('#bv-body .bv-meta a:text("Credential pool")');
  await page.waitForFunction(function () { return document.getElementById('bead').dataset.kind === 'creds' && document.querySelectorAll('#bv-body .qv-row').length === 4; });
  assert.ok(/(^|[?&])creds=pool($|&)/.test(await search(page)) && !/(^|[?&])project=/.test(await search(page)), 'the address names the pool: ' + await search(page));
  assert.strictEqual(await page.locator('#bv-title').textContent(), 'Credential pool');
  t = await page.locator('#bv-body').textContent();
  assert.ok(/^4 accounts, 1 healthy · 6 sessions/.test(t) && /New sessions go to the least loaded healthy account\./.test(t), 'the line under the title: ' + t);
  assert.deepStrictEqual(await page.locator('#bv-body .qv-row .gl-state').allTextContents(), ['Healthy', 'Warming', 'Throttled', 'No key'], 'each account\'s health');
  assert.deepStrictEqual(await page.locator('#bv-body .qv-row:nth-child(3) [role=meter]').evaluateAll(function (m) { return m.map(function (x) { return x.getAttribute('aria-valuenow'); }); }), ['100', '91'], 'a throttled account\'s windows');
  assert.ok(/claude · 0 sessions now, 52 in all · resets at \d\d:\d\d UTC/.test(await page.locator('#bv-body .qv-row:nth-child(3) .qv-line').textContent()), 'its line says when it resets');
  assert.ok(/Not probed yet\./.test(await page.locator('#bv-body .qv-row:nth-child(4)').textContent()) && /openai · 0 sessions now, 0 in all · no key set/.test(await page.locator('#bv-body .qv-row:nth-child(4)').textContent()), 'an account with no key');
  assert.ok(/^Read only\. Read at \d\d:\d\d UTC/.test(await page.locator('#bv-body .gc-foot').textContent()), 'read only, and when it was read');
  assert.strictEqual(await page.locator('#bv-body button, #bv-body input, #bv-body form').count(), 0, 'nothing on it changes the pool');

  // Share (LW.5): orchard's owner may share the whole land. The tab says what the link grants and what it withholds, links
  // the preview, lists the land's windows; Create link shows its link once; the live window revokes, and the link goes
  await page.goto(url + '/garden?project=orchard&tab=share');
  await shown(page, 'orchard', 'share');
  await page.waitForSelector('#pj-pane .pj-share form');
  t = await pane(page);
  assert.ok(/A window is a read-only public link to the whole land\. Anyone who has this link can watch the whole land\./.test(t), 'orchard Share: ' + t);
  assert.ok(/1 field is withheld for an internal reference/.test(t) && /plans\[1\]\.title\./.test(t), 'what it withholds: ' + t);
  assert.ok(/live · 12 views · never viewed · expires 2026-11-28 · owner@orchard\.example/.test(t), 'the live window: ' + t);
  assert.strictEqual(await page.locator('#pj-pane a:text("Open the preview")').getAttribute('href'), '/p/orchard/share/preview?days=30', 'the preview at the lifetime chosen');
  assert.strictEqual(await page.locator('#pj-pane .qv-row button:text("Revoke")').count(), 1, 'only the live window revokes');
  await page.click('#pj-pane button:text("Create link")');
  await page.waitForSelector('#pj-pane .pj-minted');
  assert.strictEqual(await page.locator('#pj-pane .pj-minted input').inputValue(), 'https://garden.example/w/gw_AAAAAAAAAAAAAAAAAAAAAA', 'the new link');
  assert.ok(/This link is shown once/.test(await page.locator('#pj-pane .pj-minted').textContent()));
  page.once('dialog', function (d) { d.accept(); });
  await page.click('#pj-pane .qv-row button:text("Revoke")');
  await page.waitForFunction(function () { return /Window gd-LandWin001 is revoked/.test(document.getElementById('pj-pane').textContent); });
  assert.strictEqual(await page.locator('#pj-pane .pj-minted').count(), 0, 'the link is not shown again');
  assert.strictEqual(await page.locator('#pj-pane .qv-row button:text("Revoke")').count(), 0, 'nothing left to revoke');

  // closing drops tab=; an unknown project is not there
  await page.click('#bv-x');
  await page.waitForFunction(function () { return document.getElementById('bead').hidden; });
  q = await search(page);
  assert.ok(!/(^|[?&])(tab|project)=/.test(q), 'closed, the address names neither: ' + q);
  await page.goto(url + '/garden?project=nothere');
  await page.waitForFunction(function () { return /There is no project nothere, or you cannot see it\./.test(document.getElementById('bv-body').textContent); });

  // the unknown project's 404, the refused field's 422 and acme's charts' 400 are the only errors: the farm has no acme project
  // bead and draws nothing of acme's, so the route refuses it; gardenio charts though no project bead gives it a cost row
  // (gd-vViKmjtrTj)
  assert.deepStrictEqual(errors.filter(function (e) { return !/\b(400|404|422)\b/.test(e); }), [], 'no console errors');
  await ctx.close();
}

async function phone(browser) {
  var ctx = await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true }), page = await ctx.newPage(), errors = watch(page);
  await page.goto(url + '/garden?project=gardenio&tab=agents');
  await shown(page, 'gardenio', 'agents');
  var wide = await page.evaluate(function () { var b = document.getElementById('bv-body'); return b.scrollWidth - b.clientWidth; });
  assert.ok(wide <= 1, 'nothing runs off the side of the panel: ' + wide);
  await page.tap('#pj-tab-config');
  await shown(page, 'gardenio', 'config');
  wide = await page.evaluate(function () { var b = document.getElementById('bv-body'); return b.scrollWidth - b.clientWidth; });
  assert.ok(wide <= 1, 'nor in the config: ' + wide);
  await page.tap('#pj-tab-cost');
  await shown(page, 'gardenio', 'cost');
  await page.waitForFunction(function () { var f = document.querySelectorAll('#pj-pane .gp-frame[src]').length; return f > 0 && document.querySelectorAll('#pj-pane .gp-box.loaded').length === f; });
  assert.ok(await page.locator('#pj-pane .pj-chips').isVisible(), 'its range chips');
  wide = await page.evaluate(function () { var b = document.getElementById('bv-body'); return b.scrollWidth - b.clientWidth; });
  assert.ok(wide <= 1, 'nor in the cost tab, its range chips and its charts: ' + wide);
  await page.goto(url + '/garden?project=orchard&tab=share');
  await shown(page, 'orchard', 'share');
  await page.waitForSelector('#pj-pane .pj-share form');
  await page.tap('#pj-pane button:text("Create link")');
  await page.waitForSelector('#pj-pane .pj-minted');
  wide = await page.evaluate(function () { var b = document.getElementById('bv-body'); return b.scrollWidth - b.clientWidth; });
  assert.ok(wide <= 1, 'nor in the Share tab with a link shown: ' + wide);
  assert.deepStrictEqual(errors, [], 'no console errors');
  await ctx.close();
}

(async function () {
  var browser;
  try { browser = await pw.chromium.launch(); } catch (e) { console.log('no chromium: ' + e.message); process.exit(3); }
  try { await desktop(browser); await phone(browser); } finally { await browser.close(); }
  console.log('project panel: ok');
})().catch(function (e) { console.error(e); process.exit(1); });
