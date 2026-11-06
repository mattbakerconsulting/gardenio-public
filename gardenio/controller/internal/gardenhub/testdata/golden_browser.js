/* Golden screenshots of /garden (gd-D953GccBTY), run from TestGardenGoldens against a live /garden on an httptest server fed
   the card walk's fixture farm (cardWorld) with a frozen clock. Usage:
     node golden_browser.js <base url>[,<base url>...] <node_modules dir> <out dir> <fonts dir> <now, unix ms>
   Writes <out dir>/<surface>-<device>-<theme>.png for every row of SHOTS and prints one line per file; the Go test compares
   them with testdata/golden/*.png. This script only captures: what counts as a difference lives in golden_browser_test.go.
   More than one base URL shards the sweep (gd-EwwEfGfe6H): each URL is its own card world, and its shard shoots every
   N-th row in one Chromium beside the others (sweep).

   What makes a shot repeatable: the pinned Noto subsets in <fonts dir> replace the page's system font stacks before first
   paint (the page draws no canvas text, so CSS covers every glyph: the signs' glyph tier is a pixel font); Date.now() is fixed at <now> while timers still run;
   reduced motion is on, so flights are jump cuts and nothing pulses; CSS animations are disabled and the caret hidden at
   capture; a tap leaves no tap highlight (pin); device scale is 1 but on a row that sets dpr. The pinning is golden_pin_browser.js's. */
'use strict';
var url = process.argv[2], mods = process.argv[3], out = process.argv[4], fonts = process.argv[5], now = +process.argv[6];
var path = require('path'), assert = require('assert');
// the fonts, clock, tap highlight and sky pinned per page, and the drawn-text probe (golden_pin_browser.js)
var { fontStyle, pin, drawnText, skyCtx } = require(path.join(__dirname, 'golden_pin_browser.js'));
// what a shot waits for in the page: the lazy bundles, Plan P's arms, the Commons' attachments (golden_ready_browser.js)
var { lazyLoaded, ARMS_P, armsLoaded, attachmentsLoaded } = require(path.join(__dirname, 'golden_ready_browser.js'));
// the Commons UX rubric's DOM probes and the box each surface is probed in, for the C3 pass on every phone shot (shoot)
var probes = require(path.join(__dirname, '..', '..', '..', '..', '..', '.claude', 'skills', 'commons-ux-acceptance', 'scripts', 'probes.js'));
var { ROOTS, FOCUS_ROOTS } = require(path.join(__dirname, 'golden_roots_browser.js'));
var pw, DEVICES;
var ARMS = require('./golden_browser_arms.js')({ into: into, look: look, undo: undo, plotsClip: plotsClip, plan: function () { return PLAN_P; } });   // the heraldry shots' helpers
var gdArmsDrawn = ARMS.gdArmsDrawn, armsDrawn = ARMS.armsDrawn, armsClip = ARMS.armsClip, FACE_ARMS = ARMS.FACE_ARMS;
var TRUCK_SHOTS = require('./golden_truck_browser.js')({ boxClip: boxClip, look: look, paneClip: paneClip });   // the release truck's shots
var truckShot = TRUCK_SHOTS.truckShot, releaseShot = TRUCK_SHOTS.releaseShot;

/* Required as a module (the commons-ux-acceptance skill's shoot.js), nothing runs: the caller gets SHOTS, the font splice and
   the settle step, so it shoots the very surfaces the goldens guard, set up the same way. */
function devices(pwc) {
  return {
    phone: Object.assign({}, pwc.devices['iPhone 12'], { viewport: { width: 390, height: 844 }, deviceScaleFactor: 1 }),
    // the phone turned sideways (M4.1, gd-T3JPWCD9xM): the iPhone 12 landscape's 750x340, what Safari leaves of it once its
    // bars are drawn. Under 900 px wide it is still the phone layout, so a row's show() is told 'phone' (AS).
    phoneland: Object.assign({}, pwc.devices['iPhone 12 landscape'], { deviceScaleFactor: 1 }),
    desktop: { viewport: { width: 1280, height: 800 }, deviceScaleFactor: 1 },
  };
}
var THEMES = ['light', 'dark'];
// the golden rows of the known-failing list CI names in GARDENIO_QUARANTINE (.github/quarantine.d); none on a laptop
var QUARANTINED = new Set(!process.env.GARDENIO_QUARANTINE ? [] : require('fs').readFileSync(process.env.GARDENIO_QUARANTINE, 'utf8')
  .split('\n').map(function (l) { return l.split('\t'); }).filter(function (f) { return f[0] === 'golden' && f.length >= 3; })
  .map(function (f) { return f[1]; }));
var AS = { phoneland: 'phone' };   // the layout a device's show() drives, when it is not the device's own name

/* The cells a row is shot in, as [device, theme]: each of its devices (phone and desktop unless it names some) in each of its
   themes (both unless it names some), and wherever it shoots the phone, phoneland in its first theme only: light, but for a
   dark-only row. One theme sideways is what keeps the sweep inside its -timeout (gardenio/docs/lands-mobile.md §4).
   upright: '<bead>' keeps a row off phoneland until that bug is fixed (or says why it never needs to be there). GARDENHUB_GOLDEN_CELLS (device-theme, comma
   separated) narrows the run to some cells, as GARDENHUB_GOLDEN_ONLY does to some rows (GOLDENS.md). */
var CELLS = (process.env.GARDENHUB_GOLDEN_CELLS || '').split(',').map(function (s) { return s.trim(); }).filter(Boolean);
function cellsOf(row) {
  var out = [], themes = row.themes || THEMES;
  (row.devices || ['phone', 'desktop']).forEach(function (d) {
    themes.forEach(function (t) { out.push([d, t]); });
    if (d === 'phone' && !row.upright) out.push(['phoneland', themes[0]]);
  });
  return out.filter(function (c) { return !CELLS.length || CELLS.indexOf(c[0] + '-' + c[1]) >= 0; });
}
var PLOT_A = '#hots a.w-plot[href="/garden?bead=gd-planA"]';

/* One row per surface. path is where the page opens; show(page, device) puts the surface on screen and resolves once it is
   there; devices limits a surface to the widths it exists at; clip(page), when given, resolves to the part of the viewport
   shot: paneClip(selector) for a pane whose backdrop other rows already guard (the farm behind the bead view at plot zoom is
   ~500 KB of grass), planClip for a plot and its neighbours. dpr, when given, is the device scale (the PNG is that many
   device px per CSS px). init, when given, runs in the page before its own scripts. gl marks a WebGL scene, shot in its own
   Chromium, one at a time (glChromium). A new surface is a new row here plus its tolerance row in
   golden_browser_test.go, then `make update-garden-goldens` (gardenio/tests/gardenhub-browser/GOLDENS.md). */
var SHOTS = [
  // the opening cursor (cursor-grid spec 3.5): depth 0 on the first project, the lattice framed
  { name: 'opening', path: '/garden', show: async function (page) {
    await page.waitForFunction(function () { return document.querySelector('#cursor-bar .cb-me') && document.querySelector('#hots .hot.cur'); });
  } },
  // the cursor's signs at each depth (cursor-grid spec 6, build step 5): the title bar names the cursor's Thing, the container as
  // its prefix, child labels where they fit, the rest glyphs. World: the cursor on the gardenio project at depth 0; project:
  // on plan A; plan: inside plan A on a phase bed; phase: inside that bed on a step.
  { name: 'cursorworld', path: '/garden?at=gardenio', show: async function (page) { await page.waitForSelector('#hots .w-biome.cur', { state: 'attached' }); await page.waitForSelector('#cursor-bar .cb-me'); } },
  // the World level (z under the first edge, gd-GgKjBgsc0U): the Project stamps under the boards (owner decision gd-H6ZPz5V262),
  // gardenio's plan A with its "!1" and heads and no pulsing ring around it or its biome
  { name: 'world', path: '/garden?at=gardenio', show: async function (page) {
    await page.waitForSelector('#hots .w-biome.cur', { state: 'attached' });
    await page.evaluate(function () { window.GARDEN.look({ z: 0.1 }); });
    await page.waitForFunction(function () { return window.GARDEN.cam().level === 0; });
    await page.waitForTimeout(100);
  } },
  { name: 'cursorproject', path: '/garden?at=gardenio/gd-planA', show: signShown(0) },
  { name: 'cursorplan', path: '/garden?at=gardenio/gd-planA', show: signShown(1) },
  { name: 'cursorphase', path: '/garden?at=gardenio/gd-planA', show: signShown(2) },
  // a harvest cell under the cursor (harvest spec 5): the title bar's status line for gd-A1x, its lead words and every badge's icon
  { name: 'cursorharvest', path: '/garden?at=gardenio/gd-A1x', show: async function (page) {
    await page.waitForSelector('#hots a.hot.cur[href="/garden?bead=gd-A1x"]', { state: 'attached' });
    await page.waitForSelector('#cursor-bar .cb-st .ic');
    await page.evaluate(function () { var a = document.activeElement; if (a && a.blur) a.blur(); });   // no focus ring in a golden
  } },
  { name: 'porch', path: '/garden', show: async function (page, device) {
    await page.waitForSelector('#d-porch > li', { state: 'attached' });
    if (device === 'phone') await page.tap('#tabbar [data-go=porch]'); else await page.click('#dbtn-porch');
    await page.locator('#d-porch').waitFor({ state: 'visible' });
    await gdArmsDrawn(page);
  } },
  ...require(path.join(__dirname, 'golden_cost_browser.js')).rows(boxClip, paneClip, function () { return now; }),   // the purse, a stale plan's cockpit
  // the in-garden bead view of plan A; promoted after its Commons UX grade reached accept (gd-vzsNYKoGNX). Its plan sections
  // (cursor-grid spec 6.4, build step 6) fill once the farm document is in.
  { name: 'bead', path: '/garden?bead=gd-planA', clip: paneClip('#bead'), show: beadShown },
  // plan P's bead view scrolled to its progress and what the plot's card carried (build step 6): its gardeners, operator and
  // nine phases, the current one gold and named so
  { name: 'beadplan', path: '/garden?bead=gd-planP', clip: paneClip('#bead'), show: async function (page) {
    await beadShown(page);
    await page.evaluate(function () { var s = document.querySelector('#bv-body .bv-prog'), b = document.getElementById('bv-body'); b.scrollTop = s.offsetTop - b.offsetTop - 4; });
  } },
  // the fixture Slack wish's Conversation section (plan gd-gO1fTT7oGC step gd-tx2wMH48MV, commonstest.WishFamily): the person's
  // replies, the crew's card as one state line and its squawks, and the head's two kd comments in the comment voice, one
  // timeline oldest first, the thread's reply composer at its foot (the card world's Commons lets the viewer reply); scrolled
  // to the section
  { name: 'beadconv', path: '/garden?bead=gd-wishSlack', clip: paneClip('#bead'), show: async function (page) {
    await page.locator('#bead').waitFor({ state: 'visible' });
    await page.waitForFunction(function () { var b = document.getElementById('bv-body'); return !/Loading/.test(b.textContent) && b.querySelector('.bv-cv-list > li') && b.querySelector('.bv-cv-say'); });
    var n = await page.evaluate(function () {
      var s = document.querySelector('#bv-body .bv-cv-list').closest('.bv-sec'), b = document.getElementById('bv-body'), a = document.activeElement;
      if (a && a.blur) a.blur();   // no focus ring in a golden
      b.scrollTop = s.offsetTop - b.offsetTop - 4;
      return [].map.call(s.querySelectorAll('.bv-cv-list > li'), function (li) { return li.className.replace('bv-cv bv-cv--', ''); }).join(' ');
    });
    assert.strictEqual(n, 'person comment comment card agent agent person agent', 'gd-wishSlack: replies, comments, the card and squawks in one timeline');
    assert.strictEqual(await page.locator('#bv-body .bv-cv-list > li[data-via=squawk]').count(), 3, 'gd-wishSlack: its three squawks carry the squawk mark');
  } },
  // the same wish's panel as it opens, unscrolled (gd-1jjjx7CMU3, graded accept): the ask, one counts line, who asked, then the
  // conversation, which on a phone is no taller than the room under it so its newest line is on screen
  { name: 'beadwish', path: '/garden?bead=gd-wishSlack', clip: paneClip('#bead'), show: async function (page) {
    await page.locator('#bead').waitFor({ state: 'visible' });
    await page.waitForFunction(function () { var b = document.getElementById('bv-body'); return !/Loading/.test(b.textContent) && b.querySelector('.bv-cv-say'); });
    await page.evaluate(function () { var a = document.activeElement; if (a && a.blur) a.blur(); });
  } },
  // step gd-A2a as a writer (X-UX-Writer) scrolled to its relations: what it waits on and what it holds up, each row with its
  // ghost Remove (WB-4). At night a panel-wide .btn rule once gave Remove the cream fill, --box-bad on it 1.9:1 (gd-r5dlqvrIau)
  { name: 'beadrels', path: '/garden?bead=gd-A2a', clip: paneClip('#bead'), show: async function (page) {
    await page.setExtraHTTPHeaders({ 'X-UX-Writer': '1' });
    await page.reload();
    await page.locator('#bead').waitFor({ state: 'visible' });
    await page.waitForFunction(function () { var b = document.getElementById('bv-body'); return !/Loading/.test(b.textContent) && b.querySelectorAll('.bv-rels .bw-rm').length >= 2; });
    await page.evaluate(function () { var s = document.querySelector('#bv-body .bv-rels .bw-rm').closest('.bv-sec'), b = document.getElementById('bv-body'); b.scrollTop = s.offsetTop - b.offsetTop - 4; });
  } },
  // the typed faces (Lands workbench-glass spec WB-1, step 5.3): a face fixture's bead view (faceBeads, bead_face_test.go) from
  // the panel's top to the end of its face; an open decision and a pending escalation with their answer boxes (the
  // escalation as an account that may approve sees it), and each answered; the generic fields table of a type with no face
  faceShot('facework', 'gd-FaceWork01'), faceShot('faceepic', 'gd-FaceEpic01'), faceShot('faceplan', 'gd-FacePlan01'),
  faceShot('facedecision', 'gd-FaceDecide1'), faceShot('facedecisionanswered', 'gd-FaceDecide2'),
  // the acceptance decision (decisiontest, gd-DiU8SB6TAM): the Gate's plot application, its context folded, the --evidence
  // task's two screenshots under it, five options and Other
  faceShot('facedecisiongate', 'gd-FaceGateDec'),
  // the same scrolled to its evidence: the context's last lines, the two screenshots, then the five options and Other
  { name: 'facedecisiongateev', path: '/garden?bead=gd-FaceGateDec', clip: faceClip, show: async function (page) {
    await faceShot('facedecisiongate', 'gd-FaceGateDec').show(page);
    await page.evaluate(function () { var s = document.querySelector('#bv-body .dec-ev'), b = document.getElementById('bv-body'); b.scrollTop = s.offsetTop - b.offsetTop - 120; });
    await page.waitForFunction(function () {   // the thumbnails load lazily once scrolled into view
      var t = document.querySelectorAll('#bv-body .dec-ev-thumb img');
      return t.length === 2 && Array.prototype.every.call(t, function (i) { return i.complete && i.naturalWidth > 0; });
    });
  } },
  faceShot('faceescalation', 'gd-FaceEscal01', true), faceShot('faceescalationdone', 'gd-FaceEscal02'),
  faceShot('faceagent', 'gd-FaceAgent01'), faceShot('facemessage', 'gd-FaceMsg0001'), faceShot('facebundle', 'gd-FaceBundle1'),
  faceShot('facereport', 'gd-FaceReport1'), faceShot('facegardener', 'gd-FaceLoop001'), faceShot('facemr', 'gd-FaceMR00001'),
  faceShot('facegeneric', 'gd-FaceGeneric'),
  // the release face (gd-j3bQqfZSt7, web/relface.js; bead_face_release_test.go releaseBeads): live on prod after staging, its PRs by plan
  faceShot('facerelease', 'gd-FaceRelease1'),
  // an agent whose task is a step of the armed epic: the epic's run before its title and its task (5.8, gd-ASLjQPkHJz)
  faceShot('faceagentline', 'gd-FaceAgentL1'),
  // Change arms on the epic (heraldry step 3.2, graded and promoted in 3.4, one row in 5.5b) as a writer (X-UX-Writer,
  // cardWorldAs's uxWriter): the row of three emoji in order, each over its Remove, the count in words, the register naming
  // plan F's look-alike (the same emoji in another order), the library's search and grid, and Save, Remove and Cancel
  { name: 'facearms', path: '/garden?bead=gd-FaceEpic01', clip: armsClip, show: async function (page) {
    await page.setExtraHTTPHeaders({ 'X-UX-Writer': '1' });
    await page.reload();
    await page.locator('#bv-body .bv-arms button', { hasText: 'Change arms' }).click();
    await page.locator('#bv-body .ar-col .bv-note', { hasText: 'Looks alike' }).waitFor();
    await page.evaluate(function () { var s = document.querySelector('#bv-body .bv-arms'), b = document.getElementById('bv-body'); b.scrollTop = s.offsetTop - b.offsetTop - 4; });
    await armsDrawn(page);
    await page.evaluate(function () { var a = document.activeElement; if (a && a.blur) a.blur(); });   // no focus ring in a golden
  } },
  // the Queues panel (WB-6, step 5.5): its Decisions tab (fixtureQueues, queue_test.go), the tabs, Mine and Everyone's, the
  // longest-waiting row first, each saying why it needs the viewer in words and carrying the Mailbox's answer box
  { name: 'queues', path: '/garden?queue=decisions', clip: paneClip('#bead'), show: async function (page) {
    await page.locator('#bead').waitFor({ state: 'visible' });
    await page.waitForFunction(function () { return !!document.querySelector('#bv-body .qv-list .qv-row'); });
  } },
  // the search panel (WB-5, step 5.7): "edge" (fixtureSearch, search_test.go), its field over three hits closest in meaning
  // first, each saying what it is, its state, land, priority and age in words
  { name: 'search', path: '/garden?q=edge', clip: paneClip('#bead'), show: async function (page) {
    await page.locator('#bead').waitFor({ state: 'visible' });
    await page.waitForFunction(function () { return document.querySelectorAll('#bv-body .sv-row').length === 3; });
  } },
  // the bundle panel (WB-9, step 5.10): the face fixture's bundle (fixtureBundles, bundle_test.go), its documents read as
  // one, each by its format: markdown, a diff, a table, a screenshot, a link and text; on a desktop the contents rail beside
  // them
  { name: 'bundle', path: '/garden?bundle=gd-FaceBundle1', clip: paneClip('#bead'), show: async function (page) {
    await page.locator('#bead').waitFor({ state: 'visible' });
    await page.waitForFunction(function () {
      var i = document.querySelector('#bv-body .bd-doc[data-format=image] img');
      return document.querySelectorAll('#bv-body .bd-doc').length === 7 && i && i.complete && i.naturalWidth > 0;
    });
  } },
  // a picture's preview and the picture viewer (WB-18, step 5.15, gd-uEEeNMlk1a) on the closed step gd-HarvestSt1, whose newest
  // grade has four pictures: picpreview, a mouse resting on the first raises the preview beside it (desktop: a phone has no
  // hover; clipped to the Harvest section and the preview, clear of the header's Open PR arrow, a glyph the pinned fonts
  // lack); picviewer, a click on the second opens the viewer on it, 2 of 4, its filmstrip under the bar
  { name: 'picpreview', path: '/garden?bead=gd-HarvestSt1&at=gardenio', devices: ['desktop'], clip: unionClip('#bv-body .bv-harvest', '#pic-preview'), show: async function (page) {
    await picsShown(page);
    await page.hover('#bv-body .bv-pics a >> nth=0');
    await page.waitForFunction(function () { var t = document.getElementById('pic-preview'); return !!t && !t.hidden && t.querySelector('img').complete; });
  } },
  { name: 'picviewer', path: '/garden?bead=gd-HarvestSt1&at=gardenio', clip: paneClip('#bead'), show: async function (page) {
    await picsShown(page);
    await page.click('#bv-body .bv-pics a >> nth=1');
    await page.waitForFunction(function () {
      return Array.prototype.every.call(document.querySelectorAll('#bead[data-kind=pic] .pic-stage img, #bead[data-kind=pic] .pic-thumb img'), function (i) { return i.complete && i.naturalWidth > 0; }) &&
        !!document.querySelector('#bead[data-kind=pic] .pic-stage img');
    });
  } },
  // the plan cockpit (WB-7, step 5.9): plan A (fixturePlans, plan_test.go) from the top, where it stands and its progress in
  // words, then what it waits on, the human asks first, and its first phase's steps
  { name: 'cockpit', path: '/garden?plan=gd-planA', clip: paneClip('#bead'), show: async function (page) {
    await page.locator('#bead').waitFor({ state: 'visible' });
    await page.waitForFunction(function () { return document.getElementById('bead').dataset.kind === 'plan' && document.querySelectorAll('#bv-body .pv-step').length === 9 && !!document.querySelector('#bv-body .pv-spend'); });
  } },
  // the project panel (WB-10, step 5.11): gardenio (fixtureProjects, project_test.go) on its Decisions tab, where its asks
  // wait, each with the Mailbox's answer box; and acme's Cost tab, its purse at the wall, and under it its charts (step 2.2),
  // which the card world's farm neither lists nor draws, so they say so in words (gardenio's, which it draws, are projectcostcharts)
  { name: 'project', path: '/garden?project=gardenio', clip: paneClip('#bead'), show: async function (page) {
    await page.locator('#bead').waitFor({ state: 'visible' });
    await page.waitForFunction(function () { return document.querySelectorAll('#pj-pane .qv-row').length === 4; });
  } },
  { name: 'projectcost', path: '/garden?project=acme&tab=cost', clip: paneClip('#bead'), show: async function (page) {
    await page.locator('#bead').waitFor({ state: 'visible' });
    await page.waitForFunction(function () { var c = document.querySelector('#pj-pane .pj-cards'); return !!document.querySelector('#pj-pane .pj-purse.wall') && !!c && !/^Loading/.test(c.textContent); });
  } },
  // Workbench panel tabs the 5.14 check-in graded accept and gd-wSb1jRJnJm promoted from shoot.js's EXTRA rows (panelShot):
  // acme's Gardeners tab, whose list the daemon did not answer (WB-15: says so, with Try again)
  panelShot('projectpartial', '/garden?project=acme&tab=gardeners', 'project', '#pj-pane'),
  // guard the plan's About tab: its goal, wakes and stops, prompt, owner and spend (WB-11)
  panelShot('gardenerabout', '/garden?gardener=gd-GardTrig01&tab=about', 'gardener', '#gd-pane'),
  // the Queues panel's Inbox (a pinned row, why each row is there, its state in words) and Escalations (whom each waits on)
  panelShot('queueinbox', '/garden?queue=inbox', 'queue', '#bv-body .qv-list'),
  panelShot('queueescalations', '/garden?queue=escalations', 'queue', '#bv-body .qv-list'),
  // gardenio's Agents, Gardeners, Accounts and Activity tabs (WB-10)
  panelShot('projectagents', '/garden?project=gardenio&tab=agents', 'project', '#pj-pane'),
  panelShot('projectgardeners', '/garden?project=gardenio&tab=gardeners', 'project', '#pj-pane'),
  panelShot('projectaccounts', '/garden?project=gardenio&tab=accounts', 'project', '#pj-pane'),
  panelShot('projectactivity', '/garden?project=gardenio&tab=activity', 'project', '#pj-pane'),
  // the Mailbox tab of the dock with its open decision (the dock only)
  { name: 'mailbox', path: '/garden', clip: paneClip('#dock'), show: async function (page, as) {
    await page.waitForSelector('#d-mail > li', { state: 'attached' });
    if (as === 'phone') await page.tap('#tabbar [data-go=mail]');
    else await page.click('#dbtn-mail');
    await page.locator('#d-mail').waitFor({ state: 'visible' });
  } },
  // Cmd-K over the map (WB-5): the '/' jump, its field holding "edge" and its suggestions up (the jump only)
  { name: 'cmdk', path: '/garden', clip: boxClip(['#jump'], 0), show: async function (page) {
    await page.waitForSelector('#cursor-bar .cb-me');
    await page.keyboard.press('Control+k');
    await page.locator('#jump-q').waitFor({ state: 'visible' });
    await page.keyboard.type('edge');
    await page.waitForTimeout(400);
  } },
  // a search that finds nothing: the field, and what to try instead
  panelShot('searchempty', '/garden?q=nothing+matches+this', 'q', '#bv-body .bv-empty'),
  // the settings editor open (Edit your settings): the spawn defaults and who the viewer is, as one form
  panelShot('settingsedit', '/garden?settings=me', 'settings', '#bv-body .pj-sec', async function (page) {
    await page.click('#bv-body button:text-is("Edit your settings")');
    await page.waitForSelector('#bv-body form[aria-label="Edit your settings"]', { state: 'visible' });
  }),
  // gardenio's Cost tab (step 2.3b, gd-mrcuxcJyyj), a project the card world's farm draws, scrolled to its Charts: the range
  // chips and the cards in view, titled as cards under the section, their frames loaded from the stand-in Grafana
  { name: 'projectcostcharts', path: '/garden?project=gardenio&tab=cost', clip: paneClip('#bead'), show: async function (page) {
    await page.locator('#bead').waitFor({ state: 'visible' });
    await page.waitForFunction(function () { return !!document.querySelector('#pj-pane .pj-cards .gp-card'); });
    await page.evaluate(function () { var s = document.querySelector('#pj-pane .pj-charts'), b = document.getElementById('bv-body'); b.scrollTop = s.offsetTop - b.offsetTop - 4; });
    await page.waitForFunction(function () {
      var started = document.querySelectorAll('#pj-pane .gp-frame[src]').length;
      return started > 0 && document.querySelectorAll('#pj-pane .gp-box.loaded').length === started;
    });
  } },
  // the gardener panel (WB-11, step 5.12): guard the plan (fixtureGardenerPanels, gardener_test.go) on its Runs tab: its
  // state, its levers, the runs newest first with the failed one marked; and the settings panel (WB-13), the viewer's own
  { name: 'gardener', path: '/garden?gardener=gd-GardTrig01&tab=runs', clip: paneClip('#bead'), show: async function (page) {
    await page.locator('#bead').waitFor({ state: 'visible' });
    await page.waitForFunction(function () { return document.querySelectorAll('#gd-pane .qv-row').length === 3 && !!document.querySelector('#bv-body .gd-levers'); });
  } },
  { name: 'settings', path: '/garden?settings=me', clip: paneClip('#bead'), show: async function (page) {
    await page.locator('#bead').waitFor({ state: 'visible' });
    await page.waitForFunction(function () { return document.getElementById('bead').dataset.kind === 'settings' && document.querySelectorAll('#bv-body .pj-sec').length === 2; });
  } },
  // the Costs panel (plan gd-gh5J0vVbJP steps 2.1 and 2.3) on the fleet: the farm's figures, the range chips and the
  // cards, the frames in view loaded from the card world's stand-in Grafana (fixtureGrafanaPanel)
  { name: 'costs', path: '/garden?costs=fleet', clip: paneClip('#bead'), show: async function (page) {
    await page.locator('#bead').waitFor({ state: 'visible' });
    await page.waitForFunction(function () {
      var started = document.querySelectorAll('#bv-body .gp-frame[src]').length;
      return document.getElementById('bead').dataset.kind === 'costs' && started > 0 && document.querySelectorAll('#bv-body .gp-box.loaded').length === started;
    });
  } },
  // the same on one project, gardenio (?costs=gardenio): its own figures and cards (gd-vViKmjtrTj), promoted from the
  // commons-ux-acceptance EXTRA row once graded accept (gd-wSb1jRJnJm)
  { name: 'costsproject', path: '/garden?costs=gardenio', clip: paneClip('#bead'), show: async function (page) {
    await page.locator('#bead').waitFor({ state: 'visible' });
    await page.waitForFunction(function () {
      var started = document.querySelectorAll('#bv-body .gp-frame[src]').length;
      return document.getElementById('bead').dataset.kind === 'costs' && started > 0 && document.querySelectorAll('#bv-body .gp-box.loaded').length === started;
    });
  } },
  // the agent panels (plan gd-gh5J0vVbJP step 4.2): guard the plan's Charts tab, its runs' cards with the frames in view
  // loaded from the stand-in Grafana; and one gardener's charts in the Costs panel, opened from its face (no farm figures)
  { name: 'gardenercharts', path: '/garden?gardener=gd-GardTrig01&tab=charts', clip: paneClip('#bead'), show: async function (page) {
    await page.locator('#bead').waitFor({ state: 'visible' });
    await page.waitForFunction(function () {
      var started = document.querySelectorAll('#gd-pane .gp-frame[src]').length;
      return document.querySelectorAll('#gd-pane .gp-card').length === 5 && started > 0 && document.querySelectorAll('#gd-pane .gp-box.loaded').length === started;
    });
  } },
  { name: 'agentcharts', path: '/garden?costs=agent:gardenio-53-typed-faces', clip: paneClip('#bead'), show: async function (page) {
    await page.locator('#bead').waitFor({ state: 'visible' });
    await page.waitForFunction(function () {
      var started = document.querySelectorAll('#bv-body .gp-frame[src]').length;
      return document.getElementById('bead').dataset.kind === 'costs' && started > 0 && document.querySelectorAll('#bv-body .gp-box.loaded').length === started;
    });
  } },
  require(path.join(__dirname, 'golden_costsland_browser.js')).row(paneClip, function () { return now; }),   // a tenant land's credits (3.3)
  // the Glass, watch (GL-1, GL-2, GL-3; step 6.4, gd-9adIfFKLls): gd-GlassLive01 (fixtureGlass, glass_panel_test.go) live, its
  // stream the recorded session testdata/glass_session.jsonl through the real relay: the state chip, look only, the header's
  // rows (the stream's cost and context over the bead's), and xterm.js at the PTY's 100 x 26 scaled to the panel; and
  // gd-GlassOff001, whose stream ends unavailable (no relay key, 6.3b): its cached screen and why, in words
  { name: 'glass', path: '/garden?glass=gd-GlassLive01', clip: paneClip('#bead'), show: async function (page) {
    await page.locator('#bead').waitFor({ state: 'visible' });
    await page.waitForFunction(function () {
      var rows = document.querySelector('#bv-body .gl-x .xterm-rows'), note = document.querySelector('#bv-body .gl-note');
      return !!rows && rows.textContent.indexOf('token exchange for agent pods') >= 0 && !!note && note.textContent.indexOf('Live.') === 0 &&
        document.querySelector('#bv-body .gl-rows').textContent.indexOf('$3.18') >= 0;
    });
  } },
  { name: 'glassoff', path: '/garden?glass=gd-GlassOff001', clip: paneClip('#bead'), show: async function (page) {
    await page.locator('#bead').waitFor({ state: 'visible' });
    await page.waitForFunction(function () {
      var note = document.querySelector('#bv-body .gl-note');
      var rows = document.querySelector('#bv-body .gl-x .xterm-rows');
      return !!rows && rows.textContent.indexOf('token exchange for agent pods') >= 0 && !!note && note.textContent.indexOf('not available here yet') >= 0;
    });
  } },
  // one gardener's transcript (bird-captions spec 11.4, gd-kl2CL3X9jQ): the card world's gd-agentBeadW, its 60 calls newest
  // first with a failure and an unfinished one (transcript_fixture_test.go); its box is dark in both themes, so dark only: one
  // load per layout
  Object.assign(panelShot('transcript', '/garden?transcript=gd-agentBeadW', 'transcript', '#bv-body .tx-row'), { themes: ['dark'] }),
  // plot B's transcript (spec 11.5, gd-X5dM2VRgDd): g-ask-b's and g-down-b's calls merged by time, each run headed by its name
  Object.assign(panelShot('transcriptplot', '/garden?transcript=plot:gd-planB', 'transcript', '#bv-body .tx-who'), { themes: ['dark'] }),
  // the credential pool (GL-15; step 6.9, gd-zM1oXXxlYp): the card world's four accounts (withFixturePool, glass_panel_test.go),
  // one for each health, with the line under the title, the chips, the 5 h and 7 d meters and each account's line
  { name: 'creds', path: '/garden?creds=pool', clip: paneClip('#bead'), show: async function (page) {
    await page.locator('#bead').waitFor({ state: 'visible' });
    await page.waitForFunction(function () { return document.getElementById('bead').dataset.kind === 'creds' && document.querySelectorAll('#bv-body .qv-row').length === 4 && !!document.querySelector('#bv-body .gc-foot'); });
  } },
  // the Glass, nudge and respond (GL-7, GL-8; step 6.5, gd-m1aiBXtDak): gd-GlassAsk001 is parked at a Bash permission prompt
  // (glassFixturePrompts, glass_panel_test.go), so under its terminal are the prompt, its three options by label from the
  // respond component, and the nudge box; its chip says Prompt on coop's word, though its bead says working and its stream
  // is unavailable, and the line under it who it waits on (gd-kUryJKRt7U)
  { name: 'glassask', path: '/garden?glass=gd-GlassAsk001', clip: paneClip('#bead'), show: async function (page) {
    await page.locator('#bead').waitFor({ state: 'visible' });
    await page.waitForFunction(function () {
      var note = document.querySelector('#bv-body .gl-note'), rows = document.querySelector('#bv-body .gl-x .xterm-rows');
      return !!rows && rows.textContent.indexOf('token exchange for agent pods') >= 0 && !!note && note.textContent.indexOf('not available here yet') >= 0 &&
        document.querySelectorAll('#bv-body .gl-prompt .rsp button').length === 3 && !!document.querySelector('#bv-body .gl-nudge textarea') &&
        document.querySelector('#bv-body .gl-state').dataset.state === 'prompt' && document.querySelector('#bv-body .gl-detail').textContent.indexOf('Waiting on you') === 0;
    });
    await page.locator('#bv-body .gl-respond').scrollIntoViewIfNeeded();
  } },
  // the Glass, attach (GL-9, GL-10; step 6.6, gd-mWooPMTNwA; the terminal as the input, gd-FpaO6Pb9bR): gd-GlassKey001 (the
  // recorded session again, attach offered on it alone) after a click on the terminal: the banner "Attached as you", the
  // Attached chip, the terminal in the accent frame, Fit, Compact and Let go pinned under it, and Ctrl-C typed into the
  // terminal, so the server's "confirm" asks in their place before it is sent
  { name: 'glassattach', path: '/garden?glass=gd-GlassKey001', clip: paneClip('#bead'), show: async function (page) {
    await page.locator('#bead').waitFor({ state: 'visible' });
    await page.waitForFunction(function () {
      var rows = document.querySelector('#bv-body .gl-x .xterm-rows');
      return !!rows && rows.textContent.indexOf('token exchange for agent pods') >= 0 && !!document.querySelector('#bv-body .gl-term.can-type');
    });
    await page.locator('#bv-body .gl-term .gl-x').click();
    await page.waitForFunction(function () { return !!document.querySelector('#bv-body .gl-attached.mine') && !!document.querySelector('#bv-body .gl-term.typing') && !!document.querySelector('#bv-body .gl-letgo'); });
    await page.keyboard.press('Control+c');
    await page.waitForFunction(function () { var a = document.querySelector('#bv-body .gl-ask'); return !!a && !a.hidden && a.textContent.indexOf('Ctrl-C') >= 0; });
    await page.locator('#bv-body .gl-dock').scrollIntoViewIfNeeded();
  } },
  // the Wall (GL-5; step 6.7, gd-Pjha29mbgN): ?glass=wall, the card world's four live sessions (fixtureWall, glass_wall_test.go)
  // as text-grid tiles in attention order, through the real Wall: g-ask-b at a permission prompt first, its tile marked, then
  // the two working (the glass panel's crew on the recorded screen) and the relay's crew idle; each screen in its colours,
  // scaled to its tile
  { name: 'wall', path: '/garden?glass=wall', clip: paneClip('#bead'), show: async function (page) {
    await page.locator('#bead').waitFor({ state: 'visible' });
    await page.waitForFunction(function () {
      var tiles = document.querySelectorAll('#bv-body .wl-grid .wl-cell'), note = document.querySelector('#bv-body .wl-note');
      return tiles.length === 4 && tiles[0].textContent.indexOf('g-ask-b') >= 0 && tiles[3].textContent.indexOf('glass-relay') >= 0 &&
        Array.prototype.every.call(tiles, function (t) { return !!t.querySelector('.wl-pre .wl-ln'); }) && !!note && note.textContent.indexOf('Live.') === 0;
    });
  } },
  // a gardener figure's card (spec 3.3): g-work-1 on plot A at the Bead level, focused, shows its tile at small size, its
  // state chip and its screen's last six lines, live from the Wall's feed narrowed to it (on a phone its sprite is no link)
  { name: 'wallcard', path: '/garden?at=gardenio/gd-planA', devices: ['desktop'], clip: boxClip(['.wl-card', '#ghots a.hot.w-gardener[href$="gd-gWork1"]'], 12), show: async function (page) {
    await gardenersShown(3)(page);
    await page.locator('#ghots a.hot.w-gardener[href$="gd-gWork1"]').focus();
    await page.waitForFunction(function () { var c = document.querySelector('.wl-card'); return !!c && !c.hidden && c.querySelectorAll('.wl-ln').length === 6 && c.textContent.indexOf('Working') >= 0; });
  } },
  // a finger's tap on a gardener drawn with no link (GL-12, gd-Tw4heR9fYO): its card, pinned, with Watch. The tap comes before
  // page-workbench.js (then page-glass.js, web/wall.js) has run, held back here: web/lazy.js holds it and the card opens once
  // they arrive
  { name: 'wallcardtap', path: '/garden?at=gardenio/gd-planA', devices: ['phone'], clip: boxClip(['.wl-card'], 12), show: async function (page) {
    var go, held = new Promise(function (ok) { go = ok; });
    await page.route('**/garden/assets/page-workbench.*', async function (r) { await held; await r.continue(); });
    await page.reload();
    await gardenersShown(3, null, true)(page);
    var at = await page.evaluate(function () {
      // between the header and the title bar on the tab bar, clear of the card's room above it; sideways, the whole short map (gd-e83LE907oK)
      var c = window.GARDEN.cam(), b = document.getElementById('board').getBoundingClientRect(), land = innerHeight < 480;
      var lo = land ? document.querySelector('header.crumb').getBoundingClientRect().bottom : 120, hi = land ? document.querySelector('.curbar').getBoundingClientRect().top : innerHeight - 260;
      var on = c.ghots.filter(function (x) { return x.kind === 'gardener'; }).map(function (h) {
        var p = window.WORLD.toScreen(c.cam, c.vp, h.x + h.w / 2, h.y + h.h / 2);
        return { id: h.id, x: b.left + p.x, y: b.top + p.y };
      }).filter(function (p) { return p.x > 8 && p.y > lo && p.x < innerWidth - 8 && p.y < hi && !document.elementFromPoint(p.x, p.y).closest('.cam,.curbar,.dock,header'); });
      return on[0] || null;
    });
    if (!at) throw new Error('no gardener on screen to tap');
    assert.strictEqual(await page.evaluate(lazyLoaded, 'workbench'), false, 'page-workbench.js ran before the tap');
    await page.touchscreen.tap(at.x, at.y);
    go();
    await page.waitForSelector('.wl-card:not([hidden]) .gwatch', { state: 'visible' });
  } },
  // plan P's nine phases (gd-kIUS2y1z7I): the cursor inside plan P (Enter from it), its phases framed, the cursor's labelled
  { name: 'plan', path: '/garden?at=gardenio/gd-planP', clip: planClip, show: async function (page) { await into(page, PLAN_P); await planShown(page); } },
  // and at the bottom of the Plan level (0.47x): no rest of the cursor's, so the camera is set there about the same middle;
  // the label rides the world, its text fixed. page-workbench.js (the purse) is held back until the look is set: sideways the
  // purse wraps the header to a second row, and arriving first it re-framed the cursor under it, plot P 53 px lower
  // (gd-ZAPcrzyYXg); arriving after, the look's camera holds
  { name: 'planout', path: '/garden?at=gardenio/gd-planP', clip: planClip, show: async function (page) {
    var go, held = new Promise(function (ok) { go = ok; });
    await page.route('**/garden/assets/page-workbench.*', async function (r) { await held; await r.continue(); });
    await page.reload();
    await into(page, PLAN_P); await planShown(page);
    await look(page, { z: 0.47 });
    go();
    await planShown(page);
  } },
  // plan P's arms at its plot gate (heraldry step 4.4, spec 6.3 and 6.6) at device pixel ratio 2, where the bitmap is 48 device
  // px wide and must still land 1:1 on whole device px; plan and planout shoot it at 1. The frame is the gate and the first
  // phase's bed: its stake and its step wear arms too and draw none (Q16, R6). The arms add no DOM node, only words in the
  // plot's, the stake's and nothing else's label.
  { name: 'armsgate', path: '/garden?at=gardenio/gd-planP', dpr: 2, clip: gateClip, show: async function (page) {
    var said = function (sel) { return page.evaluate(function (sel) { var n = document.querySelector(sel); return n && n.getAttribute('aria-label'); }, sel); };
    var armsSaid = function () {
      return page.evaluate(function () { return Array.prototype.filter.call(document.querySelectorAll('#hots [aria-label]'), function (n) { return /Arms:/.test(n.getAttribute('aria-label')); }).map(function (n) { return n.className + ' ' + n.getAttribute('aria-label'); }); });
    };
    // the Project depth, the cursor on plan P: its plot says the arms at its gate, and nothing else does
    await page.waitForSelector(PLAN_P + '.cur', { state: 'attached' });
    assert.ok(/\. Arms: compass, fire, package\.( |$)/.test(await said(PLAN_P)), 'plan P says its arms, a sentence after a full stop: ' + await said(PLAN_P));
    assert.ok(!/\.,/.test(await said(PLAN_P)), 'plan P\'s place is not run on with ".," (gd-jhE8MpXvJm): ' + await said(PLAN_P));
    assert.deepStrictEqual((await armsSaid()).length, 1, 'only plan P says arms at the Project depth');
    // the Phase depth, the cursor on the first phase's step: the step bears arms and says none, and no hot does
    await into(page, PLAN_P); await page.keyboard.press('Enter');
    await page.waitForSelector('#hots a.w-task.cur[href="/garden?bead=gd-planPa1"]', { state: 'attached' });
    await framed(page);
    assert.deepStrictEqual(await armsSaid(), [], 'a step bearing arms says none, nor does anything around it');
    // the page again, into plan P as the plan row goes: the first phase's stake alone says its arms, in words, and draws nothing
    await page.goto(new URL(page.url()).origin + '/garden?at=gardenio/gd-planP');
    await into(page, PLAN_P); await planShown(page);
    var beds = await armsSaid();
    assert.ok(beds.length === 1 && /^hot w-bed\b.* 0\. Ground rules: \d+\/\d+ steps\. Arms: compass\. Row \d+ of \d+(, column \d+ of \d+)?\.$/.test(beds[0]), 'the first stake alone says its arms: ' + beds.join(' | '));
    // the farm's arms add no DOM node but in a label's leading slot (spec 6.3): the picture is canvas pixels, the words ride the
    // hots' labels; the cursor's label keeps off the picture (CURSOR.keepOff). The gardeners' runs in the Porch (#d-porch) are
    // list rows, not the farm, and picture their task's arms as an img by design (5.9); the plot's roster in the bead view does not
    // (decision gd-C5WGUm5SLu)
    assert.strictEqual(await page.evaluate(function () {
      return Array.prototype.filter.call(document.querySelectorAll('[src*="/garden/arms"], [href*="/garden/arms"], #hots img, #hots svg'), function (n) { return !n.closest('#lbls .sign, #d-porch'); }).length;
    }), 0, 'no node for the farm\'s arms outside a label');
    await page.evaluate(function () { var a = document.activeElement; if (a && a.blur) a.blur(); });   // no focus ring in a golden
  } },
  // gardeners at their plots (gd-e4uXZs8coZ, homestead-agents-as-people.md section 8): the cursor inside plan A, the wheel to
  // the Plan level, three on A and its "+6"
  { name: 'gardeners', path: '/garden?at=gardenio/gd-planA', clip: plotsClip(['gd-planA', 'gd-planB'], 40, 30), show: gardenersShown(2, ['gd-planA', 'gd-planB']) },
  // plot A at the Bead level: six under their task columns (down and asking first, the foreman at the gate) and "+3"
  // tool calls as birds (gd-pLFocqmYyf): the same framing, each gardener there lifting a bird of another class 0.7 s into its
  // flight (one a fold of five: a flock of three), and a hawk circling for a subagent; under reduced motion, one still bird each
  { name: 'birds', path: '/garden?at=gardenio/gd-planA', clip: plotsClip(['gd-planA', 'gd-planB'], 40, 30), show: birdsShown(true) },
  { name: 'birdsstill', path: '/garden?at=gardenio/gd-planA', clip: plotsClip(['gd-planA', 'gd-planB'], 40, 30), show: birdsShown(false) },
  // the still birds at the Bead level (4n, gd-VHkZ3pCEqK): each word beside its bird over the figures, clear of every face, the
  // "?" bubbles and the cursor's ring round the phase bed, or none
  { name: 'birdsbeadstill', path: '/garden?at=gardenio/gd-planA', clip: plotsClip(['gd-planA'], 24, 30), show: birdsShown(false, 3) },
  { name: 'gardenersbead', path: '/garden?at=gardenio/gd-planA', clip: plotsClip(['gd-planA'], 24, 30), show: gardenersShown(3) },
  ...ARMS.shots,   // a gardener's task arms (5.9): gardenerarms, porcharms
  // beads keep their cell (gd-xhpccozIcC, cursor-grid spec 5.4): the gardenersbead framing after a second farm document that
  // files a step older than every other in plan A's first phase and no longer lists one of its second phase's: nothing moved,
  // the new step in the lowest free cell, the unlisted one a dim marker (cellsKept)
  { name: 'cellskeep', path: '/garden?at=gardenio/gd-planA', clip: plotsClip(['gd-planA'], 24, 30), show: cellsKept },
  // the cursor on gardenio's Barn (homestead-world-design.md 28.6): its home cell's zones, and the Barn's label counting its
  // gardeners at home and those out on plots; the strip draws its figures from step 6 (gd-2vAMq1HHI9)
  { name: 'barn', path: '/garden?at=gardenio/barn', show: async function (page, device) {
    await page.waitForFunction(function () { return Array.prototype.some.call(document.querySelectorAll('#hots .hot'), function (n) { return n._h && n._h.kind === 'zone' && n._h.node.zone === 'barn'; }); });
    var barn = await page.evaluateHandle(function () { return Array.prototype.filter.call(document.querySelectorAll('#hots .hot'), function (n) { return n._h && n._h.kind === 'zone' && n._h.node.zone === 'barn'; })[0]; });
    await barn.focus();   // focus moves the cursor to the Barn, and the title bar follows
    await page.waitForFunction(function () { var e = document.querySelector('#cursor-bar .cb-me'); return e && /Barn/.test(e.textContent); });
    await page.evaluate(function () { var a = document.activeElement; if (a && a.blur) a.blur(); });   // no focus ring in a golden
  } },
  // thread agents at the barn (5b, gd-1kFVOmUjiK): the cursor on g-thread's figure in the Commons's Barn (a thread agent's home,
  // H-D10), the title bar saying the thread it answers ("In the Commons: a pen for the
  // goats"); the strip draws its figures from step 6 (gd-2vAMq1HHI9)
  { name: 'barnthread', path: '/garden?at=~/barn', devices: ['phone'], show: async function (page) {   // the farm comes after the at= is read, so the cursor jumps there
    await page.waitForFunction(function () { return window.BIRDS.opened() > 0 && window.GARDEN.fly('gd-gThread'); });   // the cursor on its figure once the farm has put it in the Barn
    await page.waitForFunction(function () { var e = document.querySelector('#cursor-bar'); return e && /g-thread.*In the Commons/.test(e.textContent); });   // the bar leads with the thread it answers
    await page.evaluate(function () { var a = document.activeElement; if (a && a.blur) a.blur(); });   // no focus ring in a golden
    await page.evaluate(async function () {   // its read's bird; birds over its own barn are step 7's (gd-9bRQLfvrZQ, 28.4)
      var B = window.BIRDS, t = B.opened() + B.QUIET + 1000;
      B.motion(true); B.pin(t);
      document.dispatchEvent(new CustomEvent('farmactivity', { detail: { id: 'gd-gThread', doing: 'read', tool: 'Read', at: Math.floor(Date.now() / 1000), dur: 0 } }));
      await new Promise(function (r) { requestAnimationFrame(function () { r(); }); });   // the frame that launches it
      B.pin(t + 700);
      await new Promise(function (r) { requestAnimationFrame(function () { requestAnimationFrame(function () { r(); }); }); });
    });
  } },
  // the steward at music's homestead (Lands 9.8, homestead-agents-as-people.md section 9.2): its sprite right of the land's sign at
  // the Bead level and its tip as a hover opens it, led by its memory's In flight line
  { name: 'steward', path: '/garden?at=music', clip: boxClip(['#stip', '#ghots a.w-steward'], 40), show: async function (page) {
    await page.waitForSelector('#hots .w-biome.cur', { state: 'attached' });
    var at = await page.evaluate(async function () {
      var doc = await (await fetch('/garden/farm', { headers: { Accept: 'application/json' } })).json();
      var b = WORLD.layout(doc, null, { readonly: true }).index['project:music'], s = WORLD.stewardSpot(b), T = WORLD.TILE;
      return { cx: s.col * T, cy: (s.row - 1) * T };
    });
    await look(page, { cx: at.cx, cy: at.cy, z: 1.12 }); await look(page, { z: 1 });
    await page.waitForSelector('#ghots a.w-steward', { state: 'attached' });
    await page.evaluate(function () { document.querySelector('#ghots a.w-steward').dispatchEvent(new PointerEvent('pointerenter', { pointerType: 'mouse' })); });
    await page.waitForFunction(function () { var t = document.getElementById('stip'), b = t && t.querySelector('b'); return t && !t.hidden && b && /steward/.test(b.textContent) && t.children.length > 3; });
  } },
  // a Nursery pot's card (9.11b): its tip beside the pot, and the bead view a tap or a click opens, its Nursery section first
  potShot('pot', false, false), potShot('potbead', false, true),
  // the ready releases plan staked out from the fixture farm with ten steps (staked plots step 4, graded and promoted in
  // gd-zcUua6XeEJ): its plot's tip, and its bead view
  stakedShot('staked', false, false), stakedShot('stakedbead', false, true),
  // the graveyard (3.2, gd-s9KzRk6boh): the cursor on the done wish's headstone, its tip open as a hover (a desktop) or focus (a
  // phone) opens it: how it ended and why, what its kin merged, its thread; the plain stones of the abandoned and expired wishes beside it
  { name: 'graveyard', path: '/garden?at=gardenio/wish%3Agd-wishx1', clip: boxClip(['#stip', '#hots a.w-stone.cur'], 40), show: async function (page, as) {
    var stone = '#hots a.w-stone.cur';
    await page.waitForSelector(stone, { state: 'attached' });
    if (as === 'phone') await page.evaluate(function (sel) { document.querySelector(sel).focus(); }, stone);
    else await page.evaluate(function (sel) { document.querySelector(sel).dispatchEvent(new PointerEvent('pointerenter', { pointerType: 'mouse' })); }, stone);
    await page.waitForFunction(function () { var t = document.getElementById('stip'); return t && !t.hidden && /merged 1 merge request/.test(t.textContent); });
  } },
  // a goat's tip with its kin's line arms before the wish's title (heraldry 5.10, gd-WUqr6IkgkM; goatShot): the goat grazing by
  // plan A, its open gate in the pen, and a kinless pen goat, a ghost with none
  goatShot('goatarms', 'out'), goatShot('goatgate', 'gate'), goatShot('goatpen', 'pen'),
  // the release truck at gardenio's loading bay (gd-yOOVahJMCh; truckShot): loading 2026.299.2 with a crate per plan and its
  // unplanned PRs, the tag's tail on its side, at the Plan level; the same release live, standing still under reduced motion
  // with SHIPPED over its cab; and the loading truck's tip at the Bead level, the release, its counts and where it runs
  truckShot('truckload', 'shipping', false), truckShot('truckgone', 'live', false), truckShot('truck', 'shipping', true),
  // Enter on the focused truck: the release panel, its changelog by plan and step (gd-rSz81Me1Pe; releaseShot)
  releaseShot('release', false),
  // a plot with the shipped stamp (web/truck.js shipped, art.js drawShippedMark): plan A, whose newest merged PR shipped in
  // 2026.300.1 (FarmPlan.shipped, farm_releases_test.go), the cursor on it at the Plan level, the stamp on its top-left
  // fence and its sign's last row "shipped in 2026.300.1 · prod" (clip: the plot's corner to the sign)
  { name: 'shippedplot', path: '/garden?at=gardenio/gd-planA', clip: shippedClip, show: async function (page) {
    await page.waitForSelector('#hots .w-home', { state: 'attached' });
    await signShown(0)(page);
    await page.evaluate(function () { window.GARDEN.look({ z: 0.82, on: 'cursor' }); });   // the Plan level, the cursor still on plot A
    await page.waitForTimeout(300);
    await page.waitForFunction(function () { return /shipped in 2026\.300\.1/.test((document.querySelector('#cursor-bar .cb-st') || {}).textContent || ''); });
  } },
  // the pen and the graveyard from the camera column (3.3, gd-NbWjIdLTnd): a phone has no Down from the Yard to hand, so Go is
  // its way there. jumpwish: Go, then "wish": the pen (its open wishes), gardenio's Graveyard (its three closed wishes), the
  // goats and the stones. pen: Go, then "pen" and its row tapped: the cursor on gardenio's Pen zone, framed at 390 px,
  // with the title bar (clipped to them). graveyardjump: Go, then "graveyard" and its row: the cursor on gardenio's Graveyard
  // band, its project framed round it on the whole phone screen, A then goes in among the stones (the graveyard row's framing)
  { name: 'jumpwish', path: '/garden', devices: ['phone'], clip: boxClip(['.cam', '#jump'], 8), show: jumpTo('wish', ['pen', 'graveyard', 'open wish', 'closed wish']) },
  { name: 'pen', path: '/garden', devices: ['phone'], clip: boxClip(['#hots .hot.cur', '#cursor-bar'], 24), show: jumpTo('pen', ['pen'], 'home:gardenio/pen') },   // gardenio's Pen zone (homestead-world-design.md 28.7)
  { name: 'graveyardjump', path: '/garden', devices: ['phone'], show: jumpTo('graveyard', ['graveyard'], 'band:gardenio/graveyard') },
  // a project's home from the camera column (28.6, 29.6, gd-cTKXpm8JH7): gohome, the cursor on plan A and Go tapped, the empty
  // list led by gardenio's home row; homein, that row tapped and the camera's In: the cursor on a zone, the whole farmstead on the
  // phone's width; commonsin, the same on the Commons, whose strip holds only the project-less letters and wishes
  { name: 'gohome', path: '/garden?at=gardenio/gd-planA', devices: ['phone'], clip: boxClip(['.cam', '#jump'], 8), show: jumpTo('', ['home']) },
  { name: 'homein', path: '/garden?at=gardenio/gd-planA', devices: ['phone'], show: homeIn('gardenio home', 'home:gardenio') },
  { name: 'commonsin', path: '/garden', devices: ['phone'], show: homeIn('Commons home', 'home:') },
  // and one more In from gardenio's Mailbox and its Pen: the cursor on a letter and on a goat, their links 44 px under a finger (C3)
  { name: 'homeletter', path: '/garden?at=gardenio/mailbox', devices: ['phone'], show: camIn('item') },
  { name: 'homegoat', path: '/garden?at=gardenio/pen', devices: ['phone'], show: camIn('goat') },
  // the field stands from a phone (28.12, gd-a628FpJRO5): gostands, the cursor on plan A and Go tapped, the empty list led by
  // gardenio's home row and its field stands under it; fieldcrowd, coop crowded (cardWorld's crowd scene) and the camera's In
  // from its Growing field: more stands than it holds, its last slot the "+N", the loose beads at their half share; fieldglass,
  // a field figure tapped twice: its Glass as the sheet over the map, the transcript folded under the terminal; fieldplot, that
  // Glass's Go to its plot: the plot figure at the map's full zoom, focused, a tap from home
  { name: 'gostands', path: '/garden?at=gardenio/gd-planA', devices: ['phone'], clip: boxClip(['.cam', '#jump'], 8), show: jumpTo('', ['home', 'gardener']) },
  { name: 'fieldcrowd', path: '/garden', show: crowdIn, after: undo(['crowd']) },
  { name: 'fieldglass', path: '/garden?at=gardenio/growing', devices: ['phone'], show: fieldGlassShot(false) },
  { name: 'fieldplot', path: '/garden?at=gardenio/growing', devices: ['phone'], show: fieldGlassShot(true) },
  // the legend's gardener rows draw the real sprites
  { name: 'legend', path: '/garden', clip: paneClip('#legend'), show: async function (page) {
    await page.waitForSelector('#cursor-bar .cb-me');
    await page.click('#cam-menu');
    await page.click('#cam-help');   // Legend & keys is in the camera's Menu (3b R9)
    await page.locator('#legend').waitFor({ state: 'visible' });
    await page.evaluate(function () { var a = document.activeElement; if (a && a.blur) a.blur(); });   // no focus ring in a golden
  } },
  // the legend's doing poses (4c, gd-kxCviRUVA2): scrolled so the first one, the magnifier, is the top row
  { name: 'legenddo', path: '/garden', clip: paneClip('#legend'), show: async function (page) {
    await page.waitForSelector('#cursor-bar .cb-me');
    await page.click('#cam-menu');
    await page.click('#cam-help');
    await page.locator('#legend').waitFor({ state: 'visible' });
    await page.evaluate(function () {
      var li = Array.prototype.filter.call(document.querySelectorAll('#legend li'), function (l) { return /^Kneeling with a magnifier/.test(l.textContent); })[0];
      li.scrollIntoView({ block: 'start' });
      var a = document.activeElement; if (a && a.blur) a.blur();
    });
  } },
  // the Plans panel (gd-BL2JQbsfyy): opened from the camera's Menu, sorted by spend, so the crop tiles lead with plan A's
  // figure, plan C's stale one and plan B's settled one, then the plans never rolled up, each a dash
  { name: 'plans', path: '/garden', clip: paneClip('#dw-plans'), show: plansShown('crops') },
  // the same as the Forest at its Grove zoom: each tree's caption carries the spend line
  { name: 'plansforest', path: '/garden', clip: paneClip('#dw-plans'), show: plansShown('forest') },
  // the camera's Menu open (3b R8, R9, R10): Home, All, Plans, Outline, Tend, Costs, Snap, Sky, Settings and Legend & keys under its button (on a phone, left of the Go · Menu
  // column), framed with the bar
  { name: 'menu', path: '/garden', clip: boxClip(['.cam', '#menu-list'], 8), show: async function (page) {
    await page.waitForSelector('#cursor-bar .cb-me');
    await page.click('#cam-menu');
    await page.locator('#menu-list').waitFor({ state: 'visible' });
    await page.evaluate(function () { var a = document.activeElement; if (a && a.blur) a.blur(); });   // no focus ring in a golden
  } },
  // the gardeners' transitions (gd-elQWxzYhdm, homestead-agents-as-people.md section 8): a live farm patch pushed to the open
  // page (cardWorld's /test/farm/scene), one keyframe each with ANIM's clock pinned, at the Plan level on its path between plot and
  // its field stand (homestead-world-design.md 28.4, 28.12). g-ride-c starts on plot C: halfway along its walk out of its stand
  // (the truck when plot C is another biome's); g-ask-b goes down on plot B: the particles at 15%, then its figure 60% along the
  // flight back to its stand with the scarecrow up; g-foreman finishes: walking home with its scroll at 50%, and its tldr over
  // its stand on arrival
  { name: 'ride', path: '/garden?at=~/home', show: transition([['ride', 'gd-gRideC', function (t) { return t.t0 + t.dur / 2; }, 0.5]]), after: undo(['ride']) },
  { name: 'burst', path: '/garden?at=~/home', show: transition([['burst', 'gd-gAskB', function (t) { return t.t0 + 0.15 * t.burst; }, 0]]), after: undo(['burst']) },
  { name: 'flyback', path: '/garden?at=~/home', show: transition([['burst', 'gd-gAskB', function (t) { return t.t0 + t.burst + 0.6 * t.arc; }, 0.6]]), after: undo(['burst']) },
  { name: 'walkhome', path: '/garden?at=~/home', show: transition([['walkhome', 'gd-gForeman', function (t) { return t.t0 + t.dur / 2; }, 0.5]]), after: undo(['walkhome']) },
  { name: 'arrive', path: '/garden?at=~/home', show: transition([['walkhome', 'gd-gForeman', function (t) { return t.t0 + t.dur; }, 1]], '.lbl.w-tldr'), after: undo(['walkhome']) },
  // the same three patches under reduced motion: the end state only, and no animation frame is asked for afterwards
  { name: 'reduced', path: '/garden?at=~/home', show: transition([['ride', 'gd-gRideC'], ['burst', 'gd-gAskB'], ['walkhome', 'gd-gForeman']], '', ['gd-planB', 'gd-planC']), after: undo(['ride', 'burst', 'walkhome']) },
  // beacons (gd-gtr7qBCn9z): the cursor inside plan P, so what needs you is off-screen and arrows sit at the map's edge
  { name: 'beacons', path: '/garden?at=gardenio/gd-planP', show: async function (page) {
    await into(page, PLAN_P);
    await page.waitForSelector('#bcns .bcn', { state: 'visible' });
  } },
  // the Commons v2 workbench at /garden/c/n/{id} (step 4) and /garden/c/s (steward view S2)
  ...require(path.join(__dirname, 'golden_workbench_browser.js')).rows(),
  ...require(path.join(__dirname, 'golden_impersonated_browser.js')).rows(),   // Become's banner (gd-0SUIdKXWV3 step 4)
  // where a tenant lands (Lands step 8.8): /garden?land=, the camera and the land header with its purse
  ...require(path.join(__dirname, 'golden_land_browser.js')).rows(paneClip),
  // the splash (plan gd-EJGA3zDGCk, steps 1 and 10): the WebGL canvas drawn through headless Chromium's SwiftShader at four
  // hours of the full moon of 26 Oct 2026, each pinned by ?t=: noon, dusk (the sun low in the west), midnight (the full moon
  // high in the south) and dawn (the sun low in the east, the moon setting), so every later step that lights by the hour is
  // guarded in the day, the golden hour and the night; light only: the scene is the almanac's sky at that hour whatever the
  // colour scheme, so a dark shot is the same picture at most of a minute of SwiftShader a frame. Each is held at the mid
  // quality tier by ?q=mid (step 9's ?q= pins it, unjudged), phone and desktop alike, so no runner's cores or frame times
  // change the picture; each asserts the canvas's name, the almanac's words for that hour. Then the same page where the
  // browser gives no WebGL context, the still and its note, whose band the scheme does colour
  splashShot('splash', '2026-10-26T12:00:00Z', 'Midday over the farm: the sun high in the south; the gardenio stone on its hill'),
  // sideways, noon stands for the scene: the other three light the same layout, and each costs a minute or more of SwiftShader
  Object.assign(splashShot('splashdusk', '2026-10-26T16:45:00Z', 'Dusk over the farm: the sun low in the west; the gardenio stone on its hill'), { upright: 'noon stands for it' }),
  Object.assign(splashShot('splashnight', '2026-10-26T00:00:00Z', 'Night over the farm: a full moon, 100 percent, high in the south; the gardenio stone on its hill'), { upright: 'noon stands for it' }),
  Object.assign(splashShot('splashdawn', '2026-10-26T06:45:00Z', 'Dawn over the farm: the sun low in the east; a full moon, 100 percent, setting in the west; the gardenio stone on its hill'), { upright: 'noon stands for it' }),
  // zoomed (plan gd-I5QHVfGLbD step 5): noon at ?z=4&look=5,-8.2, the stone's mark in the middle of the frame, so the
  // camera's narrowed field of view, the turned aim and the zoom line in the readout are guarded; phone and desktop only, as
  // noon's sideways shot stands for the layout
  Object.assign(splashShot('splashzoom', '2026-10-26T12:00:00Z', 'Midday over the farm: the sun high in the south; the gardenio stone on its hill', { zoom: 4, yaw: 5, pitch: -8.2 }), { upright: 'noon stands for it' }),
  { name: 'splashflat', path: '/garden/splash?t=2026-10-26T12:00:00Z', init: function () {
    var get = HTMLCanvasElement.prototype.getContext;
    HTMLCanvasElement.prototype.getContext = function (k) { return /webgl/.test(k) ? null : get.apply(this, arguments); };
  }, show: async function (page) {
    await page.waitForFunction(function () { return document.body.dataset.gl === 'off'; });
    await page.locator('#splash-flat img').evaluate(function (img) { return img.decode(); });
    // Chromium gives text subpixel (LCD) anti-aliasing only on a layer it judges opaque, so how the page's layers fell decided
    // the title's and the note's colour fringes: a slow runner shot them fringed (gd-owQJjry58h). Each on a layer of its own,
    // grey anti-aliasing, as the golden has.
    // The still on a layer of its own too. The menu's buttons (backdrop-filter) are composited and the fixed still overlaps
    // them, so whether Chromium gave the img a layer was its call: on a slow runner it did, and then drew the img as a
    // directly composited image, rastered at its own 800 x 500 and scaled up by the compositor, its 256-texel tiles
    // filtered at their seams (columns 407, 814 and 1221 on a desktop: 22 px over, gd-RnP4RSVFUv). Always on a layer, so
    // the desktop golden is always that picture.
    await page.addStyleTag({ content: '#splash-menu .title, #splash-flat figcaption, #splash-flat img { will-change: transform; }' });
  } },
];

/* A splash golden (plan gd-EJGA3zDGCk step 10): the scene at rfc3339 instant at, the mid tier, light only; it waits out
   SwiftShader's first frame and asserts the page drew in WebGL at that instant, at that tier, its canvas named label. view,
   when given, is the {zoom, yaw, pitch} the page opens at by ?z=&look= (plan gd-I5QHVfGLbD), asserted as the page's own. */
function splashShot(name, at, label, view) {
  var q = view ? '&z=' + view.zoom + '&look=' + view.yaw + ',' + view.pitch : '';
  return { name: name, path: '/garden/splash?t=' + at + '&q=mid' + q, themes: ['light'], gl: true, show: async function (page) {
    // SwiftShader compiles the stone's shaders (its displacement, the shadow's depth pass, the gems' transmission) slowly:
    // the page's thread sits in that first render for 77 s at 390 px and 144 s at 1280 px on a 2-CPU agent pod, the scene
    // alone in its GPU process (gd-iXUhuhF1wO), so 90 s timed out on any runner slower than CI's usual
    await page.waitForFunction(function () { return document.body.dataset.gl && +document.getElementById('splash').dataset.frames >= 1 || document.body.dataset.gl === 'off'; }, null, { timeout: 180000 });
    // and the flora's frame drawn through: render() returns once the GL calls are queued, and SwiftShader takes most of a
    // minute over them; a pixel read waits for the GPU here, untimed, so the screenshot's 30 s is not spent on it
    await page.evaluate(function () { var gl = window.SPLASH.view && window.SPLASH.view.renderer.getContext(); if (gl) gl.readPixels(0, 0, 1, 1, gl.RGBA, gl.UNSIGNED_BYTE, new Uint8Array(4)); });
    assert.strictEqual(await page.evaluate(function () {
      return [document.body.dataset.gl, document.body.dataset.why || '', window.SPLASH.t, document.getElementById('splash').dataset.tier, document.getElementById('splash').getAttribute('aria-label')].join(' | ');
    }), ['on', '', Date.parse(at), 'mid', label].join(' | '), name + ': the splash draws in WebGL at ' + at + ', mid, named for the hour');
    if (view) assert.deepStrictEqual(await page.evaluate(function () { return window.__splash.state(); }), view, name + ': the splash opens at the view its address gives');
  } };
}

/* A transition keyframe (homestead-agents-as-people.md section 8). The page opens with the cursor on the Commons's strip; the
   camera is set at the Plan level (0.55x) on gardenio's Barn zone, where its gardeners' trucks and walks start and end (28.4),
   so each transition starts in a phone's view too, and, once its keyframe is pinned, on the point a fraction k (a scene's
   fourth item) along its way from `from` to `to` (its lane route, homestead-world-design.md 29.3, when it has one); without motion, on the world.index ids in `on`. Once the farm stream is
   open, ANIM's clock is pinned past the 2 s catch-up window and each scene [name, gardener id, keyframe(t), k] is pushed as a
   live patch; the clock is then pinned at keyframe(t) of that gardener's transition (t: {t0, dur, arc, burst}). Without a keyframe
   (the reduced row) motion stays reduced (the goldens' context): the patches cut to the end state, and a probe asserts that
   no transition runs and no frame asks for another afterwards. wait, when given, is an element to wait for. */
function transition(scenes, wait, on) {
  return async function (page) {
    await page.waitForFunction(function () { return document.querySelector('#hots .hot.cur') && window.GARDEN.known('gd-gDownB').title === 'g-down-b' && window.ANIM.opened() > 0; });
    await look(page, { z: 0.55, on: ['home:gardenio/barn'] });
    var motion = scenes.every(function (s) { return s[2]; });
    await page.evaluate(function (motion) { window.ANIM.motion(motion); window.ANIM.pin(window.ANIM.opened() + window.ANIM.CATCH_UP + 1000); }, motion);
    for (var s of scenes) {
      var res = await page.request.post(new URL(page.url()).origin + '/test/farm/scene?name=' + s[0]);
      assert.strictEqual(res.status(), 204, s[0] + ': the farm published the patch');
      if (motion) {
        await page.waitForFunction(function (id) { return window.ANIM.list().some(function (t) { return t.id === id; }); }, s[1]);
        var t = await page.evaluate(function (a) { var t = window.ANIM.list().filter(function (t) { return t.id === a[0]; })[0]; t.at = window.ANIM.along(t.pts || [t.from, t.to], a[1]); return t; }, [s[1], s[3]]);
        await page.evaluate(function (at) { window.ANIM.pin(at); }, s[2](t));
        await look(page, { cx: t.at.x + 8, cy: t.at.y + 12, z: 0.55 });
      } else {
        // the page has the patch: g-ride-c on the map starting, g-ask-b down, g-foreman gone
        await page.waitForFunction(function (sc) { var k = window.GARDEN.known(sc[1]); return { ride: k.status === 'spawning', burst: k.status === 'failed', walkhome: !k.title }[sc[0]]; }, s);
      }
    }
    if (!motion && on) await look(page, { z: 0.55, on: on });
    if (wait) await page.waitForSelector(wait, { state: 'visible' });
    if (!motion) {
      await page.waitForTimeout(400);   // the last patch's document and render
      // an animation is a frame that asks for the next one: count only those. A lone frame asked for by a timer is a still
      // repaint (garden.js's 5 s tick at the Plan level), which landed in the window about one run in three (gd-3X6pZPMkze).
      var probe = await page.evaluate(function () {
        var n = 0, inFrame = false, real = window.requestAnimationFrame;
        window.requestAnimationFrame = function (f) {
          if (inFrame) n++;
          return real.call(window, function (t) { inFrame = true; try { f(t); } finally { inFrame = false; } });
        };
        return new Promise(function (r) { setTimeout(function () { window.requestAnimationFrame = real; r({ frames: n, running: window.ANIM.list().length }); }, 500); });
      });
      assert.deepStrictEqual(probe, { frames: 0, running: 0 }, 'reduced motion: the end state, and no frame asks for another');
    }
  };
}
/* Puts the fixture back for the next shot (each opens a fresh page on the same server). The server is the page's origin, so
   the rows also work required as a module (the commons-ux-acceptance skill's shoot.js). */
function undo(names) {
  return async function (page) { for (var n of names) assert.strictEqual((await page.request.post(new URL(page.url()).origin + '/test/farm/scene?name=' + n + '&undo=1')).status(), 204, n + ' undone'); };
}

/* The box of one element, widened to whole pixels: the frame a locator screenshot of it takes. */
/* The bead view open, its document loaded and, for a plan, its phase list filled from the farm document. */
async function beadShown(page) {
  await page.locator('#bead').waitFor({ state: 'visible' });
  await page.waitForFunction(function () { var b = document.getElementById('bv-body'); return b.textContent.trim() && !/Loading/.test(b.textContent) && b.querySelector('.bv-phases'); });
}
/* A typed face's row: the bead view of id, shown once its face section (or, with none, its fields table, opened) is in.
   esc: as an account that may approve escalations (body[data-esc]), so an open one shows Approve and Reject. */
function faceShot(name, id, esc) {
  return { name: name, path: '/garden?bead=' + id, clip: faceClip, show: async function (page) {
    await page.locator('#bead').waitFor({ state: 'visible' });
    await page.waitForFunction(function () { var b = document.getElementById('bv-body'); return !/Loading/.test(b.textContent) && b.querySelector('.bv-face, .bv-fold'); });
    // heraldry step 3.4: an epic bearing arms opens on its Heraldry section; the task bears arms too and shows none (R6)
    assert.strictEqual(await page.locator('#bv-body .bv-arms').count(), FACE_ARMS[id] ? 1 : 0, id + ': a Heraldry section only for an epic bearing arms');
    await armsDrawn(page);
    await page.evaluate(function (esc) {
      if (esc) document.body.dataset.esc = '1';
      var f = document.querySelector('#bv-body .bv-face') ? null : document.querySelector('#bv-body .bv-fold');
      if (f) f.open = true;
    }, !!esc);
  } };
}
/* The bead view from its top down to the end of its face (or its opened fields table), inside the viewport. */
async function faceClip(page) {
  return page.evaluate(function () {
    var p = document.getElementById('bead').getBoundingClientRect(), f = document.querySelector('#bv-body .bv-face') || document.querySelector('#bv-body .bv-fold');
    var x = Math.floor(p.left), y = Math.floor(p.top), end = Math.min(Math.ceil(f.getBoundingClientRect().bottom) + 10, Math.floor(p.bottom), window.innerHeight);
    return { x: x, y: y, width: Math.ceil(p.right) - x, height: end - y };
  });
}
/* potShot: a Nursery pot's card (9.11b, gd-m38yKRX0hE). The ready seed gd-seed2 is given the steward's "run it?" decision in
   the farm document on its way to the page, the cursor deep-linked onto it. stress (the commons-ux-acceptance skill's EXTRA
   rows, never a golden): F1, F2, F3, F4 and a decision for someone else. The ready seed is staked out on a plot
   (homestead-world-design.md section 3), whose link wears the pot's card; the farm document gives it a short title, three
   steps and its narrative, and its bead document the steps' bodies (26.14). bead false: that tip, the card then the step
   list, opened by a hover on a desktop and by focus on a phone; bead true: the plan's bead view (?bead=), its Staked out
   section first and its Steps after it. */
var POT_STEPS = [
  ['gd-seed2s1', 'Package the gardener for a laptop', 'A brew formula and a signed binary, so the owner installs it in one line.'],
  ['gd-seed2s2', 'Reach the cluster through the edge', 'The local gardener reaches the beads daemon and the mux through the edge proxy, never a tunnel of its own. ' +
    'Its token is the owner\'s, exchanged at the edge for a short-lived one, and it is refreshed before it lapses so a long run never stops halfway. ' +
    'Nothing is written to disk: the keychain holds what must outlive a restart.'],
  ['gd-seed2s3', 'Keep the credentials in the keychain', '']];
function potShot(name, stress, bead) {
  return { name: name, clip: stress ? undefined : bead ? paneClip('#bead') : boxClip(['#stip', '#hots a.w-staked.cur'], 40), path: '/garden?at=gd-seed2', show: async function (page, as) {
    await page.route('**/garden/farm', async function (route) {
      var res = await route.fetch(), doc = await res.json();
      (doc.seeds || []).forEach(function (s) {
        if (s.id !== 'gd-seed2') return;
        s.state = 'asked'; s.short_title = 'local agents'; s.summary = 'Run the gardener next to the owner, on their own laptop, so a plan can start before the cluster has room.';
        s.steps = POT_STEPS.map(function (st, i) { return { id: st[0], title: st[1], n: i + 1, status: 'open' }; });
        s.decision = { id: 'gd-seed2ask', prompt: 'Run it?', for: '', since: s.updated, options: [
          { id: 'approve', text: 'Run the plan', label: 'Run the plan: the steward starts its first step' },
          { id: 'change', text: 'Change it first', label: 'Change the plan first: say what to change, and the steward writes it again' },
          { id: 'drop', text: 'Drop it', label: 'Drop the plan: nothing runs' }] };
        if (!stress) return;
        s.title = ('Local agents on every laptop 🧑‍🌾 مريم: run the gardener next to the owner, reach the cluster through the edge, keep the credentials in the keychain and never on disk, and ').repeat(2).slice(0, 200);
        s.decision.for = 'oidc:mo@example.com';
        s.decision.prompt = 'Run it? It replaces gd-AbCdEfGhIj and https://github.com/acme/gardenio/pull/4242/files#diff-0123456789abcdef0123456789abcdef first';
        for (var i = 1; i <= 5; i++) s.decision.options.push({ id: 'opt' + i, text: 'Option ' + i + ': split step ' + i + ' into its own plan with a gardener per laptop 🚜', label: '' });
      });
      await route.fulfill({ response: res, json: doc });
    });
    await page.route('**/garden/bead/gd-seed2', async function (route) {   // the bead route's step bodies on a ready or asked plan
      var res = await route.fetch(), d = await res.json();
      d.children = POT_STEPS.map(function (st) { return { id: st[0], title: st[1], type: 'task', status: 'open', body: st[2] }; });
      await route.fulfill({ response: res, json: d });
    });
    await page.reload();
    await stakedShown(page, as, 'gd-seed2', bead);   // the ready seed is staked out on a plot, which wears the pot's card
  } };
}
/* goatShot (heraldry 5.10, gd-WUqr6IkgkM): a goat's tip, its kin's line arms before the wish's title (GOATS.card, STIP.show).
   The fixture farm's plans bear no arms, so the farm document is edited on its way to the page: gd-wish1 is given two kin runs
   (FarmWish.arms; the server's side is TestFarmWishArms) and, ghost, is untended. which: 'out', the goat grazing by plan A;
   'gate', its open gate in the pen; 'pen', gd-wish2 in its cell, kinless, a ghost with no arms. A desktop's tip is a hover's,
   a phone's a tap's (STIP.goat on the goat GOATS.at hits, as the board's pointer does). */
var GOAT_ARMS = [{ canonical: '🧭🔥📦', n: 3, name: 'arms: compass, fire, package' }, { canonical: '🛡️🗣️📢', n: 3, name: 'arms: shield, speaking head, loudspeaker' }];
function goatShot(name, which, ghost) {
  var id = which === 'pen' ? 'gd-wish2' : 'gd-wish1', at = 'gardenio/gd-planA';   // the cursor inside plan A; a pen goat is framed in its project's Pen cell below
  return { name: name, path: '/garden?at=' + at, clip: boxClip(['#stip'], 48), show: async function (page, as) {
    await page.route('**/garden/farm', async function (route) {
      var res = await route.fetch(), doc = await res.json();
      ((doc.wishes || {}).open || []).forEach(function (w) { if (w.id === 'gd-wish1') { w.arms = GOAT_ARMS; if (ghost) w.tending = false; } });
      await route.fulfill({ response: res, json: doc });
    });
    await page.reload();
    if (which === 'out') await gardenersShown(2)(page);   // plan A at the Plan level, where goats are drawn (its plot alone frames the Project level)
    else { await page.waitForFunction(function () { return window.GARDEN.here() === 'gardenio'; }); await page.evaluate(function (k) { window.GARDEN.look({ z: 1.5, on: ['wish:' + k] }); }, id); }   // its pen cell (homestead-world-design.md 28.2) at the Bead level
    var q = { id: id, which: which, pin: as === 'phone', arms: id === 'gd-wish1' ? GOAT_ARMS.length * 2 : 0 };
    await page.waitForFunction(function (q) {
      var d = GOATS.list().filter(function (d) { return d.id === q.id && (q.which === 'gate' ? d.out : q.which === 'pen' ? d.pen && !d.out : !d.pen); })[0];
      if (!d) return false;
      var g = GOATS.at(d.css.x + d.css.w / 2, d.css.y + d.css.h / 2, 0);
      if (!g || g.id !== q.id) return false;
      STIP.goat(g, q.pin);
      var t = document.getElementById('stip');
      return t && !t.hidden && t.querySelectorAll('b .ar-line img').length === q.arms;
    }, q);
    await page.evaluate(function () { return Promise.all(Array.prototype.map.call(document.querySelectorAll('#stip img'), function (i) { return i.decode().catch(function () {}); })); });
  } };
}
/* shippedplot's frame: plot A's top-left corner (its stamp) and the title bar that says it shipped, inside the viewport. */
async function shippedClip(page) {
  var p = await page.locator(PLOT_A).boundingBox(), s = await page.locator('#cursor-bar').boundingBox(), vs = page.viewportSize();
  var x0 = Math.max(0, Math.floor(Math.min(p.x, s.x) - 24)), y0 = Math.max(0, Math.floor(Math.min(p.y, s.y) - 24));
  return { x: x0, y: y0, width: Math.min(vs.width, Math.ceil(s.x + s.width + 24)) - x0, height: Math.min(vs.height, Math.ceil(s.y + s.height + 24)) - y0 };
}
/* stakedShot: the ready releases plan gd-planR staked out in gardenio's fields (staked plots step 4, gd-zcUua6XeEJ;
   card_browser_test.go releasesPlan): ten steps and the steward's "run it?" decision, straight from the fixture farm, the
   cursor deep-linked onto its plot. bead false: the plot's tip (its card, then the step list), opened by a hover on a desktop
   and by focus on a phone; bead true: the plan's bead view (?bead=), Staked out then Steps (10) then the narrative. stress
   (the commons-ux-acceptance skill's EXTRA rows, never a golden) edits the documents on their way to the page: F1 200-char
   titles on the plan and two steps, F2 URLs, a SHA and a bead id in the bodies and the question, F4 emoji, a flag and Arabic,
   and F6 an eleventh step the daemon does not have (no body, its link opens a bead that is not there). */
var STAKED_LONG = ('Releases of every project 🏷️🚚 إصدار: a release record in kbeads keyed by tag, a truck in the Yard that drives out when it ships, and ').repeat(2).slice(0, 200);
function stakedShot(name, stress, bead) {
  return { name: name, clip: stress ? undefined : bead ? paneClip('#bead') : boxClip(['#stip', '#hots a.w-staked.cur'], 40), path: '/garden?at=gardenio/gd-planR', show: async function (page, as) {
    if (stress) {
      await page.route('**/garden/farm', async function (route) {
        var res = await route.fetch(), doc = await res.json();
        (doc.seeds || []).forEach(function (s) {
          if (s.id !== 'gd-planR') return;
          s.title = STAKED_LONG; s.short_title = 'releases 🏷️🇳🇱 إصدار';
          s.summary += ' See https://github.com/acme/gardenio/compare/2026.299.1...2026.299.2#diff-0123456789abcdef0123456789abcdef and gd-UZKZ9H25UD.';
          s.steps[1].title = '2. ' + STAKED_LONG; s.steps[6].title = '7. Frontend: the release truck 🚚👩🏽‍🌾 on /garden, الشاحنة';
          s.steps.push({ id: 'gd-planRgone', title: '11. A step the daemon no longer has', n: 11, status: 'open' });
          s.decision.prompt = 'Run it? It replaces gd-AbCdEfGhIj and https://github.com/acme/gardenio/pull/4242/files#diff-0123456789abcdef0123456789abcdef first';
        });
        await route.fulfill({ response: res, json: doc });
      });
      await page.route('**/garden/bead/gd-planR', async function (route) {
        var res = await route.fetch(), d = await res.json();
        d.title = STAKED_LONG;
        d.description += ' See https://github.com/acme/gardenio/compare/2026.299.1...2026.299.2#diff-0123456789abcdef0123456789abcdef and gd-UZKZ9H25UD.';   // what the view shows in place of the summary
        (d.children || []).forEach(function (c) {
          if (c.id === 'gd-planR02') c.body = 'Lands on branch agent/releases/gd-Dvour96HjO at 9ba77bd9c0ffee1234567890abcdef1234567890, in gardenio/controller/internal/releases/record.go: ' +
            'https://github.com/mattbakerconsulting/gardenio/pull/1234/files#diff-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef and gd-UZKZ9H25UD 🏷️.';
          if (c.id === 'gd-planR07') c.body = 'الشاحنة في الفناء 🚚 drives out when a release goes live; the crates come from the produce bins 📦🇯🇵.';
        });
        await route.fulfill({ response: res, json: d });
      });
      await page.reload();
    }
    await stakedShown(page, as, 'gd-planR', bead);
  } };
}
/* The staked plot under the cursor (potShot, stakedShot) shown with its card's answer and its step list: bead, in its plan's
   bead view (a press on a staked plot goes in, as on any plot); else in its tip, a hover's on a desktop, a focus's on a phone. */
async function stakedShown(page, as, id, bead) {
  var plot = '#hots a.w-staked.cur';
  await page.waitForSelector(plot, { state: 'attached' });
  if (bead) await page.goto(page.url() + '&bead=' + id);
  else if (as === 'phone') await page.evaluate(function (sel) { document.querySelector(sel).focus(); }, plot);
  else await page.evaluate(function (sel) { document.querySelector(sel).dispatchEvent(new PointerEvent('pointerenter', { pointerType: 'mouse' })); }, plot);
  await page.waitForFunction(function (bead) { var t = document.getElementById(bead ? 'bead' : 'stip'); return t && !t.hidden && t.querySelector('.pot-card .act [data-act=resolve]') && t.querySelector(bead ? '.pot-steps .pot-body[role=button]' : '.pot-steps'); }, bead);
}
/* The bead view of gd-HarvestSt1 with its grade's four pictures loaded. */
async function picsShown(page) {
  await page.locator('#bead').waitFor({ state: 'visible' });
  await page.waitForFunction(function () {
    var a = document.querySelectorAll('#bv-body .bv-pics img');
    return a.length === 4 && Array.prototype.every.call(a, function (i) { return i.complete && i.naturalWidth > 0; });
  });
}
/* The box round the elements a and b, inside the viewport. */
function unionClip(a, b) {
  return async function (page) {
    var r = await page.locator(a).boundingBox(), q = await page.locator(b).boundingBox(), vp = page.viewportSize();
    var x = Math.max(0, Math.floor(Math.min(r.x, q.x))), y = Math.max(0, Math.floor(Math.min(r.y, q.y)));
    return { x: x, y: y, width: Math.min(vp.width, Math.ceil(Math.max(r.x + r.width, q.x + q.width))) - x, height: Math.min(vp.height, Math.ceil(Math.max(r.y + r.height, q.y + q.height))) - y };
  };
}
/* A Workbench panel row: path opened, the panel up as kind, and ready (a selector) inside it with something in it that is not
   a Loading line, then then(page, as) when given (a form opened); the panel only (paneClip). The commons-ux-acceptance skill's
   shoot.js builds its EXTRA panel rows the same way, so a row promoted from there is the one it graded. */
function panelShot(name, path, kind, ready, then) {
  return { name: name, path: path, clip: paneClip('#bead'), show: async function (page, as) {
    await page.locator('#bead').waitFor({ state: 'visible' });
    await page.waitForFunction(function (a) {
      var b = document.getElementById('bead'), r = document.querySelector(a[1]);
      return b.dataset.kind === a[0] && !!r && !/Loading…/.test(document.getElementById('bv-body').textContent);
    }, [kind, ready]);
    if (then) await then(page, as);
  } };
}

/* The Plans panel open from the camera's Menu in mode (crops or forest), sorted by spend. */
function plansShown(mode) {
  return async function (page) {
    await page.waitForSelector('#cursor-bar .cb-me');
    await page.click('#cam-menu');
    await page.click('#cam-plans');
    await page.locator('#dw-plans').waitFor({ state: 'visible' });
    await page.waitForSelector('#plans-slot .plans-sort');
    if (mode === 'forest') {
      await page.click('#plans-slot .plans-views button >> text=Forest');
      await page.waitForSelector('#plans-slot .tree .tree-spend');
    }
    await page.click('#plans-slot .plans-sort button >> text=Spend');
    await page.waitForSelector('#plans-slot .plans-sort button[aria-pressed=true] >> text=Spend');
    await page.evaluate(function () { var a = document.activeElement; if (a && a.blur) a.blur(); var b = document.querySelector('#dw-plans .dbody'); if (b) b.scrollTop = 0; });   // no focus ring in a golden
  };
}

function paneClip(sel) {
  return async function (page) {
    var b = await page.locator(sel).boundingBox(), x = Math.floor(b.x), y = Math.floor(b.y);
    return { x: x, y: y, width: Math.ceil(b.x + b.width) - x, height: Math.ceil(b.y + b.height) - y };
  };
}

/* The union of sels' boxes, pad px around it, kept inside the viewport: a control and the popup it opens. */
function boxClip(sels, pad) {
  return async function (page) {
    var x0 = Infinity, y0 = Infinity, x1 = -Infinity, y1 = -Infinity, vs = page.viewportSize();
    for (var i = 0; i < sels.length; i++) {
      var b = await page.locator(sels[i]).boundingBox();
      x0 = Math.min(x0, b.x); y0 = Math.min(y0, b.y); x1 = Math.max(x1, b.x + b.width); y1 = Math.max(y1, b.y + b.height);
    }
    x0 = Math.max(0, Math.floor(x0 - pad)); y0 = Math.max(0, Math.floor(y0 - pad));
    x1 = Math.min(vs.width, Math.ceil(x1 + pad)); y1 = Math.min(vs.height, Math.ceil(y1 + pad));
    return { x: x0, y: y0, width: x1 - x0, height: y1 - y0 };
  };
}

/* Go in the camera column and q typed into the jump list, until it lists a row of each of kinds (their words); then, given an
   id, its first row tapped and the cursor on that Thing, the title bar on it and no focus ring. */
function jumpTo(q, kinds, id) {
  return async function (page) {
    await page.waitForSelector('#cursor-bar .cb-me');
    await page.locator('#cam-go').tap();
    await page.locator('#jump-q').fill(q);
    await page.waitForFunction(function (kinds) { var k = Array.prototype.map.call(document.querySelectorAll('#jump-list .jk'), function (e) { return e.textContent; }); return kinds.every(function (w) { return k.indexOf(w) >= 0; }); }, kinds);
    if (!id) return;
    await page.locator('#jump-list .jump-it').first().tap();
    await page.waitForFunction(function (id) { var c = document.querySelector('#hots .hot.cur'), f = document.querySelector('#cursor-bar .cb-me'); return document.getElementById('jump').hidden && c && c._n && c._n.id === id && f; }, id);
    await page.evaluate(function () { var a = document.activeElement; if (a && a.blur) a.blur(); });   // no focus ring in a golden
    await page.waitForTimeout(300);   // the flight there (a cut under reduced motion) and the chunks' bake
  };
}
/* The camera column's In from the cursor's zone (its ?at=): the cursor on its first item, of kind kind. */
function camIn(kind) {
  return async function (page) {
    await page.waitForSelector('#cursor-bar .cb-me');
    await page.locator('#cam-in').tap();
    await page.waitForFunction(function (kind) { var c = document.querySelector('#hots .hot.cur'); return c && c._n && c._n.kind === kind && document.querySelector('#cursor-bar .cb-me'); }, kind);
    await page.evaluate(function () { var a = document.activeElement; if (a && a.blur) a.blur(); });
    await page.waitForTimeout(400);
  };
}
/* coop crowded (cardWorld's crowd scene: POST /test/farm/scene?name=crowd), then its Growing field opened by its ?at= (coop's
   farmstead has no field until the scene fills it) and the camera's In: the cursor on the field's first item, its "+N" in the
   last slot. */
async function crowdIn(page, as) {
  var origin = new URL(page.url()).origin;
  assert.strictEqual((await page.request.post(origin + '/test/farm/scene?name=crowd')).status(), 204, 'crowd: the farm published the patch');
  await page.goto(origin + '/garden?at=coop/growing');
  await page.waitForSelector('#cursor-bar .cb-me');
  await page.waitForFunction(function () { var c = document.querySelector('#hots .hot.cur'); return window.GARDEN.known('gd-gCrowd00').title === 'coop-crew-01' && c && /^Growing field: 30 working/.test(c.getAttribute('aria-label') || ''); });
  if (as === 'phone') await page.locator('#cam-in').tap(); else await page.locator('#cam-in').click();   // a click leaves a hover, and a gardener's card, on a phone
  await page.waitForFunction(function () { var c = document.querySelector('#hots .hot.cur'); return c && c._n && c._n.kind !== 'zone' && /^Home\s*›\s*Growing field\s*›/.test((document.getElementById('cursor-bar') || {}).textContent || ''); });
  await page.evaluate(function () { var a = document.activeElement; if (a && a.blur) a.blur(); });
  await page.waitForTimeout(400);   // the zoom step (a cut under reduced motion) and the chunks' bake
  assert.ok(await page.evaluate(function () { var c = document.querySelector('.wl-card'); return !c || c.hidden; }), 'In pins no card of a gardener drawn under it');
}
/* Into gardenio's Growing field from its ?at= (a tap on the field, the cursor's cell, goes in), then g-ask-b's field figure
   tapped twice (the first puts the cursor on it): its Glass, the transcript folded on a narrow phone. plot: then the Glass's Go to its
   plot, the panel closed and the camera on g-ask-b's plot figure at full zoom, focus on it. */
function fieldGlassShot(plot) {
  return async function (page) {
    var stand = '#hots .hot[aria-label^="g-ask-b, "]';
    await page.waitForSelector('#cursor-bar .cb-me');
    await page.waitForFunction(function () { var c = document.querySelector('#hots .hot.cur'); return c && c._n && c._n.zone === 'growing' && c._n.kids.indexOf('field:gd-gAskB') >= 0; });
    await page.locator('#hots .hot.cur').tap();
    await page.locator(stand).tap();
    await page.waitForFunction(function (sel) { return document.querySelector(sel).classList.contains('cur'); }, stand);
    await page.locator(stand).tap();
    await page.waitForFunction(function () { var p = document.getElementById('bead'), t = document.querySelector('#bv-body .gl-tx'); return !p.hidden && p.dataset.kind === 'glass' && t && !/Loading/.test(t.textContent) && !document.querySelector('#bv-body .gl-plot').hidden; });
    if (plot) {
      await page.locator('#bv-body .gl-plot').tap();
      await page.waitForFunction(function () { var a = document.activeElement; return document.getElementById('bead').hidden && a && a.matches('#ghots a.w-gardener[href$="gd-gAskB"]') && window.GARDEN.cam().z === 4; });
    } else await page.evaluate(function () { var a = document.activeElement; if (a && a.blur) a.blur(); });   // no focus ring in a golden
    await page.waitForTimeout(400);
  };
}
/* Go's empty list, its row titled title (home's) tapped, then the camera column's In: the cursor on one of home's zones, zoomed a step
   about the farmstead (cursor-grid spec revision 3, 4.3). */
function homeIn(title, home) {
  return async function (page) {
    await jumpTo('', ['home'])(page);
    await page.locator('#jump-list .jump-it').filter({ has: page.locator('.jt', { hasText: new RegExp('^' + title + '$') }) }).first().tap();
    await page.waitForFunction(function (id) { var c = document.querySelector('#hots .hot.cur'); return document.getElementById('jump').hidden && c && c._n && c._n.id === id; }, home);
    await page.locator('#cam-in').tap();
    await page.waitForFunction(function (id) { var c = document.querySelector('#hots .hot.cur'); return c && c._n && c._n.kind === 'zone' && c._n.id.indexOf(id + '/') === 0 && document.querySelector('#cursor-bar .cb-me'); }, home);
    await page.evaluate(function () { var a = document.activeElement; if (a && a.blur) a.blur(); });
    await page.waitForTimeout(400);   // the zoom step (a cut under reduced motion) and the chunks' bake
  };
}
var PLAN_P = '#hots a.w-plot[href="/garden?bead=gd-planP"]';
/* The cursor on sel's plan (from its ?at=), then Enter: into its phases (cursor-grid spec 3.2), framed. */
async function into(page, sel) {
  await page.waitForSelector(sel + '.cur', { state: 'attached' });
  await page.keyboard.press('Enter');
  await page.waitForFunction(function () { return /w-bed/.test((document.querySelector('#hots .hot.cur') || {}).className || ''); });
  await framed(page);
}
/* Enter only zooms a step about the cursor's Thing (cursor-grid spec revision 3); a cursor jump to the Thing the cursor is
   on frames it, as an ?at= opens it, so a golden reached by Enter keeps the framing it guards. Until revision 3.2 the title
   bar's last part was that jump and framed() pressed it; since gd-zDdiA3HBh5 a press there opens the bead view instead (spec 7
   Start), which hid the map's arrows under it (gd-n6J8iiN3TQ), so the jump is made through GARDEN.fly. */
async function framed(page) {
  await page.evaluate(function () {
    var a = document.querySelector('#cursor-bar a[aria-current=location]');
    window.GARDEN.fly(a.dataset.hot);
  });
}
/* The camera at c ({cx, cy, z} in world px, a missing one kept) as a cut, the cursor where it is (GARDEN.look): a mouse drag
   pans nothing and the wheel is one depth step (build step 4), so a framing the cursor never rests at is set, not steered to. */
async function look(page, c) {
  await page.evaluate(function (c) { window.GARDEN.look(c); }, c);
  await page.waitForTimeout(c.on ? 300 : 120);   // on: the signs are placed again after FADE_MS (120)
}

/* The gardeners are drawn once the farm document is in and the camera has reached the level: the cursor inside plan A, then,
   where its framing is at another level (a phone frames it at Plan, a desktop at Bead), the camera at 0.82x (Plan) about the
   same middle, or at 1.12x, past the Bead edge, then 1x, the snap the wheel settled on before step 4 (Bead), on the cursor's
   bed. plots, at the Plan level: the plots the shot is of, centred below the header when they fit (GARDEN.look's on). About
   the middle, a phone had the cursor's bed off its left edge and plot B under its header (gd-ISIukGDl4w). held: page-workbench.js
   is held back (no purse yet). */
function gardenersShown(lv, plots, held) {
  return async function (page) {
    await into(page, '#hots a.w-plot[href="/garden?bead=gd-planA"]');
    // the purse (page-workbench.js) loads late and grows the header (a second row sideways), which a look() keeps clear of: look
    // under its final height (gd-e83LE907oK). held: the row holds page-workbench.js back, so the header stays as it is
    if (!held) await page.waitForSelector('#purse', { state: 'visible' });
    var at = await page.evaluate(function () { return window.GARDEN.cam().level; });
    if (at !== lv && lv === 3) { await look(page, { z: 1.12 }); await look(page, { z: 1, on: 'cursor' }); }
    else if (at !== lv) await look(page, { z: 0.82, on: plots });
    else if (plots) await look(page, { on: plots });
    // on a phone a sprite is no target under 44 px, so the gardener links may be absent: ask the page whether it knows them
    await page.waitForFunction(function (lv) { return window.GARDEN.cam().level === lv && window.GARDEN.known('gd-gDownB').title === 'g-down-b'; }, lv);
  };
}

/* A new step filed while the page is open (gd-xhpccozIcC), at the Bead level of gardenersShown(3): the page's own document
   again, normalized as web/plans.js hands it over, with gd-A1new planted first by created in plan A's first phase and the
   last plant no gardener tends dropped from its second, sent as the next `farmdoc`. A cold layout would move every step of
   the first phase on by a cell; the page's memo (sessionStorage garden.cells.v1) keeps every bead's cell, gives gd-A1new the
   lowest free one and leaves the dropped one as a marker, which the row checks before it shoots. */
async function cellsKept(page, device) {
  await gardenersShown(3)(page, device);
  var r = await page.evaluate(async function () {
    var memo = function () { return JSON.parse(sessionStorage.getItem('garden.cells.v1') || '{}')['gd-planA'] || {}; };
    var was = memo(), doc = PLANS.normalize(await (await fetch('/garden/farm', { headers: { Accept: 'application/json' } })).json());
    var plan = doc.plans.filter(function (p) { return p.id === 'gd-planA'; })[0], ph1 = plan.phases[0], ph2 = plan.phases[1];
    var tended = {}; (doc.gardeners || []).forEach(function (g) { tended[g.task] = 1; });
    var first = ph1.tasks.reduce(function (m, t) { return Math.min(m, +t.created || m); }, Infinity);
    ph1.tasks = [{ id: 'gd-A1new', title: 'A step filed while the page is open', type: 'task', state: 'open', priority: 2, project: 'gardenio', updated: first - 60, created: first - 60 }].concat(ph1.tasks);
    var drop = ph2.tasks.filter(function (t) { return !tended[t.id]; }).pop();
    ph2.tasks = ph2.tasks.filter(function (t) { return t !== drop; });
    doc.v = (doc.v || 0) + 1;
    document.dispatchEvent(new CustomEvent('farmdoc', { detail: doc }));
    return { was: was, now: memo(), bed1: ph1.id, bed2: ph2.id, dropped: drop && drop.id };
  });
  assert.ok(r.dropped, 'plan A\'s second phase has a step no gardener tends');
  Object.keys(r.was).forEach(function (bed) {
    Object.keys(r.was[bed]).forEach(function (id) { assert.strictEqual(r.now[bed][id], r.was[bed][id], id + ' kept its cell in ' + bed); });
  });
  var held = Object.keys(r.was[r.bed1]).map(function (id) { return r.was[r.bed1][id]; });
  assert.ok(r.now[r.bed1]['gd-A1new'] !== undefined && held.indexOf(r.now[r.bed1]['gd-A1new']) < 0, 'the new step took a free cell');
  await page.waitForTimeout(120);
}

/* Birds over plots A and B at the Plan level (gardenersShown(2)): once the farm stream is open, BIRDS's clock is pinned past its
   2 s quiet window and every gardener on plan A or B is sent a call of the next class in turn as a `farmactivity` event (the
   first a fold of five, the last a subagent's start: the hawk), each with its class's tool word ("go test", "git push"...) and,
   but for one in four, a caption (gd-TmYAAOBQEH: short, two lines, one cut with "…"; the hawk its subagent's description), which
   flies with its bird on its plate, the caption else the tool word; the clock then moves on 0.7 s (motion: forced on) or stays (the
   goldens' reduced motion: one still bird each), and the shot waits for the frames that draw it. lv 3: the same at the Bead level
   (the birdsbeadstill golden; commons-ux-acceptance's birdsbead row in motion). after: the clock moves on that many ms instead,
   past the flights, so the plates linger by their gardeners (gd-2mYRxayl9b; commons-ux-acceptance's birdslinger row). */
function birdsShown(motion, lv, after) {
  var show = gardenersShown(lv || 2, lv === 3 ? null : ['gd-planA', 'gd-planB']);
  return async function (page) {
    await show(page);
    await page.waitForFunction(function () { return window.BIRDS.opened() > 0; });
    var n = await page.evaluate(async function (a) {
      var motion = a.motion, after = a.after;
      var doc = await (await fetch('/garden/farm', { headers: { Accept: 'application/json' } })).json(), B = window.BIRDS, t = B.opened() + B.QUIET + 1000;
      var ids = (doc.gardeners || []).filter(function (g) { return g.plot && (g.plot.plan === 'gd-planA' || g.plot.plan === 'gd-planB'); }).map(function (g) { return g.id; });
      var cls = ['read', 'edit', 'test', 'git', 'beads', 'web', 'deploy', 'shell'], send = function (ev) { document.dispatchEvent(new CustomEvent('farmactivity', { detail: ev })); };
      var tool = { read: 'Read', edit: 'Edit', test: 'go_test', git: 'git_push', beads: 'kd_show', web: 'WebFetch', deploy: 'helm_upgrade', shell: 'other' };   // the word each carries (4k)
      var caps = ['Read web/birds.js', 'Trial-merge main into #1282 in a detached worktree', 'Run the gardenhub bird tests under node', '', 'Show task, claim it, and read team news',
        'Rebase the caption plate branch onto main and rerun every bird golden under node', 'Roll the chart to the next window', ''];   // the plate's caption, else its tool word
      B.motion(motion); B.pin(t);
      ids.forEach(function (id, i) {
        if (i === ids.length - 1) send({ id: id, doing: 'delegate', tool: 'subagent', at: 1, dur: 0, cap: 'Explore gardenhub transcript endpoints' });
        else send({ id: id, doing: cls[i % cls.length], tool: tool[cls[i % cls.length]], at: 1, dur: 0, n: i === 0 ? 5 : 1, cap: caps[i % caps.length] });
      });
      await new Promise(function (r) { requestAnimationFrame(function () { r(); }); });   // the frame that launches them
      if (motion || after) B.pin(t + (after || 700));
      await new Promise(function (r) { requestAnimationFrame(function () { requestAnimationFrame(function () { r(); }); }); });
      return B.list().length + B.hawks().length + (after ? B.perched().length : 0);
    }, { motion: motion, after: after });
    assert.ok(n > 0, after ? 'plates lingering over plots A and B' : 'birds in the air over plots A and B');
  };
}

/* Some plots and a margin around them (world boxes from WORLD.layout carried to the screen by the #hots transform), kept inside
   the viewport. */
function plotsClip(ids, mx, my) {
  return async function (page) {
    var g = await page.evaluate(async function (ids) {
      var doc = await (await fetch('/garden/farm', { headers: { Accept: 'application/json' } })).json();
      var w = WORLD.layout(doc, null, { readonly: true }), r = null;
      ids.forEach(function (id) { var p = w.index[id]; r = WORLD.union(r, { x: p.x, y: p.y, w: p.w, h: p.h }); });
      var m = document.getElementById('hots').style.transform.match(/translate\(([-\d.e]+)px, ([-\d.e]+)px\) scale\(([\d.e-]+)\)/);
      var bd = document.getElementById('board').getBoundingClientRect(), T = WORLD.TILE, z = +m[3];
      return { x: bd.left + +m[1] + r.x * T * z, y: bd.top + +m[2] + r.y * T * z, width: r.w * T * z, height: r.h * T * z };
    }, ids);
    var vs = page.viewportSize(), x = Math.max(0, Math.round(g.x - mx)), y = Math.max(0, Math.round(g.y - my));
    return { x: x, y: y, width: Math.min(vs.width, Math.round(g.x + g.width + mx)) - x, height: Math.min(vs.height, Math.round(g.y + g.height + my)) - y };
  };
}

/* The cursor's signs at a depth: plan A's link, then Enter `depth` times (into a phase bed, into a step), the title bar on it. */
function signShown(depth) {
  return async function (page) {
    await page.waitForSelector(PLOT_A + '.cur', { state: 'attached' });
    if (depth) { await page.focus(PLOT_A); for (var i = 0; i < depth; i++) await page.keyboard.press('Enter'); }
    await page.waitForFunction(function (d) { var c = document.querySelector('#hots .hot.cur'); return c && (d === 0 || (d === 1 ? /w-bed/ : /w-task/).test(c.className)) && document.querySelector('#cursor-bar .cb-me'); }, depth);
    if (depth) await framed(page);
    await page.evaluate(function () { var a = document.activeElement; if (a && a.blur) a.blur(); });   // no focus ring in a golden
  };
}
/* Plan P's phases framed: the Plan depth, or the Bead depth where the framing rests at a snap (a desktop frames plan P at 1x,
   the Bead depth's detailed beds now that the depth is a pure function of zoom, gd-bwI1PZ6jnM), with the Homestead label. */
async function planShown(page) {
  await page.waitForFunction(function () { return window.GARDEN && window.GARDEN.cam().level >= 2 && /^Homestead/.test((document.querySelector('#cursor-bar .cb-p') || {}).textContent || ''); });
}

/* Plan P's plot and a margin of its neighbours, the frame TestCardUnderChromium's phaseRows shoots: the plot's world box
   from WORLD.layout, carried to the screen by the #hots transform. */
async function planClip(page) {
  var g = await page.evaluate(async function () {
    var doc = await (await fetch('/garden/farm', { headers: { Accept: 'application/json' } })).json();
    var plot = WORLD.layout(doc, null, { readonly: true }).index['gd-planP'];
    var m = document.getElementById('hots').style.transform.match(/translate\(([-\d.e]+)px, ([-\d.e]+)px\) scale\(([\d.e-]+)\)/);
    var bd = document.getElementById('board').getBoundingClientRect(), T = WORLD.TILE, z = +m[3];
    return { x: bd.left + +m[1] + plot.x * T * z, y: bd.top + +m[2] + plot.y * T * z, width: plot.w * T * z, height: plot.h * T * z };
  });
  var vs = page.viewportSize(), x = Math.max(0, Math.round(g.x - 140)), y = Math.max(0, Math.round(g.y - 70));
  return { x: x, y: y, width: Math.min(vs.width, Math.round(g.x + g.width + 140)) - x, height: Math.min(vs.height, Math.round(g.y + g.height + 50)) - y };
}

/* Plan P's plot gate and its first phase's bed (heraldry step 4.4): the gate picture's box (HERALDRY.gate, a fixed screen size)
   down to the bed's first rows, carried to the screen by the #hots transform as planClip is. */
async function gateClip(page) {
  var r = await page.evaluate(async function () {
    var doc = await (await fetch('/garden/farm', { headers: { Accept: 'application/json' } })).json();
    var plot = WORLD.layout(doc, null, { readonly: true }).index['gd-planP'];
    var m = document.getElementById('hots').style.transform.match(/translate\(([-\d.e]+)px, ([-\d.e]+)px\) scale\(([\d.e-]+)\)/);
    var bd = document.getElementById('board').getBoundingClientRect(), T = WORLD.TILE, z = +m[3], off = WORLD.BED.off[0];
    var gate = HERALDRY.gate((plot.x + off) * T, (plot.y + off) * T, z, devicePixelRatio);
    var sx = function (wx) { return bd.left + +m[1] + wx * z; }, sy = function (wy) { return bd.top + +m[2] + wy * z; };
    return { x: sx(plot.x * T), y: sy(gate.y), right: sx((plot.x + off + WORLD.BED.size) * T), bottom: sy((plot.y + off + WORLD.BED.size) * T) };
  });
  var vs = page.viewportSize(), x = Math.max(0, Math.floor(r.x - 8)), y = Math.max(0, Math.floor(r.y - 8));
  return { x: x, y: y, width: Math.min(vs.width, Math.ceil(r.right + 8)) - x, height: Math.min(vs.height, Math.ceil(r.bottom + 8)) - y };
}

// settle: every pinned face loaded, the Commons' unread counts and read marks answered, then two frames for the label pass
// and any queued render
async function settle(page) {
  await page.waitForFunction(lazyLoaded, '');   // the unread pips, the Menu's Costs and Wall and the Porch's purse are the lazy bundles'
  await page.waitForFunction(armsLoaded, ARMS_P, { timeout: 10000 });
  await page.waitForFunction(attachmentsLoaded, null, { timeout: 10000 });
  await page.evaluate(function () { return Promise.all(Array.prototype.map.call(document.querySelectorAll('.cm-img img[src]'), function (i) { return i.complete && i.naturalWidth ? i.decode().catch(function () {}) : null; })); });
  await page.evaluate(function () { return Promise.all([document.fonts.load('400 15px "Golden Sans"'), document.fonts.load('700 12px "Golden Mono"'), document.fonts.ready]); });
  assert.strictEqual(await page.evaluate(function () { return document.fonts.check('700 12px "Golden Mono"') && document.fonts.check('400 15px "Golden Sans"'); }), true, 'pinned fonts loaded');
  await page.evaluate(function () { return new Promise(function (r) { requestAnimationFrame(function () { requestAnimationFrame(r); }); }); });
  await page.waitForTimeout(250);
}

async function shoot(browser, base, shot, device, theme, style) {
  var ctx = await skyCtx(browser, Object.assign({}, DEVICES[device], shot.dpr ? { deviceScaleFactor: shot.dpr } : {}, { colorScheme: theme, reducedMotion: 'reduce', locale: 'en-US', timezoneId: 'UTC' }));
  try {
    var page = await ctx.newPage(), errors = [], reads = [];
    page.on('pageerror', function (e) { errors.push(String(e)); });
    page.on('request', function (q) { var ch = readMark(q); if (ch) reads.push(ch); });
    await pin(page, base, now, style);
    if (shot.init) await page.addInitScript(shot.init);   // before the page's own scripts
    await page.goto(base + shot.path);
    var file = path.join(out, shot.name + '-' + device + '-' + theme + '.png');
    try {
      await shot.show(page, AS[device] || device);
      await settle(page);
      // No surface pushes the page sideways (Commons UX rubric C2, M1.1 gd-H8gSsz2h85): the document is never wider than the
      // viewport, on the 390 px phone above all. A box built to scroll sideways inside itself (a code block, a picture strip)
      // is the rubric's own probe's to judge; here only the page as a whole counts, with a pixel of rounding allowed.
      var wide = await page.evaluate(function () { return document.documentElement.scrollWidth - window.innerWidth; });
      assert.ok(wide <= 1, shot.name + '-' + device + '-' + theme + ' scrolls sideways: document ' + wide + 'px wider than the viewport (rubric C2)');
      // The rubric's C3 probes under a touch pointer (M4.1, gd-T3JPWCD9xM): every control in the surface's probe box at least
      // 44x44, no box in it scrolling sideways that is not built to, and no field under 16 px, which iOS zooms into (M1.3,
      // gd-xWWqgcmzXM). Reported, not judged: TestGardenGoldens holds the counts against goldenC3Known.
      if (DEVICES[device].hasTouch) console.log('golden c3 ' + path.basename(file) + ' ' + JSON.stringify(c3(await page.evaluate(probes.run, { root: ROOTS[shot.name] || 'body', coarse: true }))));
      var clip = shot.clip ? await shot.clip(page) : undefined, text = await page.evaluate(drawnText, clip || null);
      if (text.glyphs || text.families.length) console.log('golden text ' + path.basename(file) + ' ' + JSON.stringify(text));
      await page.screenshot({ path: file, animations: 'disabled', caret: 'hide', clip: clip });
    } finally { if (shot.after) await shot.after(page); }   // a row that changed the farm puts it back, pass or fail
    assert.deepStrictEqual(errors, [], shot.name + '-' + device + '-' + theme + ' page errors');
    console.log('shot ' + path.basename(file));
    return reads;
  } finally { await ctx.close().catch(function () {}); }   // a timed-out shot's page goes too, before its retry
}

/* The C3 half of a probes.run result: its small targets, sideways boxes and fields iOS zooms into, each { n, items }, or the
   error that kept the probes from running (a probe root the surface does not have). */
function c3(r) {
  if (r.error) return { error: r.error };
  var o = {};
  ['smallTarget', 'sideways', 'zoomField'].forEach(function (k) { o[k] = { n: r.hits[k].n, items: r.hits[k].items }; });
  return o;
}

/* The WebGL rows' own Chromium, one scene at a time (gd-iXUhuhF1wO). All of a browser's pages share its one GPU process,
   where SwiftShader draws in software, and a splash scene keeps it busy for a minute or two: in the shared browser that
   held up the frames of every other shard's page, whose clicks and screenshots then timed out on surfaces with nothing
   wrong (the rows just before the splash's in SHOTS). And the splash rows sit
   side by side, so the shards reached them together and queued four scenes at once, each waiting out the others' work in
   its first-frame budget. Launched on the first gl row; glWait gives each gl shot its turn while the other shards go on. */
var glBrowser, glTurn = Promise.resolve();
function glChromium() { return glBrowser || (glBrowser = pw.chromium.launch()); }
function glWait() {
  var free, mine = new Promise(function (r) { free = r; }), before = glTurn;
  glTurn = before.then(function () { return mine; });
  return before.then(function () { return free; });
}

/* A shot is taken once more when a wait in it timed out (gd-iXUhuhF1wO): on a loaded ARC runner a goto, a click or the
   splash's first frame passes its timeout on a different surface each run, with nothing wrong in the page. A second
   timeout fails the sweep; so does any other error at once (an assertion, a page error). The "golden retry" line shows in
   TestGardenGoldens's log. */
async function shootRetried(browser, base, shot, device, theme, style) {
  for (var tries = 1; ; tries++) {
    var free = shot.gl ? await glWait() : null;
    try { return await shoot(shot.gl ? await glChromium() : browser, base, shot, device, theme, style); }
    catch (e) {
      if (tries > 1 || !e || e.name !== 'TimeoutError') throw e;
      console.log('golden retry ' + shot.name + '-' + device + '-' + theme + ': ' + String(e.message).split('\n')[0]);
    } finally { if (free) free(); }
  }
}

/* What one shot leaves behind for the next is the Commons read marks (POST /garden/c/{land}/{channel}/read): the badges
   of a channel list depend on which channels every row before it opened. GARDENHUB_GOLDEN_ONLY skips those rows, so it
   replays their marks: it opens each skipped row's channel by its path, which marks it read up to its newest entry, as
   the row's own shots would (a mark only moves forward, so the order does not matter; gd-VJWqCUihlQ). With the band gone
   (gd-ZeiSZV7j2Y) no row opens a channel by its path, so nothing is replayed; the check still fails a row that marks one.
   Everything else a row changes is its
   own bead's (the Glass claim) or put back by its after. A full sweep checks that a row marks no channel but its path's
   and those a row before it marked, so the replay stays whole as rows are added. */
function channelOf(p) { var m = /^\/garden\/c\/([^/?#]+)\/([^/?#]+)/.exec(p); return m ? m[1] + '/' + m[2] : ''; }
function readMark(q) { return q.method() === 'POST' && /\/read$/.test(new URL(q.url()).pathname) ? channelOf(new URL(q.url()).pathname) : ''; }

async function replayReads(browser, base, shot, style) {
  var ctx = await skyCtx(browser, Object.assign({}, DEVICES.desktop, { reducedMotion: 'reduce', locale: 'en-US', timezoneId: 'UTC' }));
  try {
    var page = await ctx.newPage(), marks = [];
    page.on('request', function (q) { var ch = readMark(q); if (ch) marks.push(q.response().then(function (r) { assert.ok(r && r.ok(), 'GARDENHUB_GOLDEN_ONLY: replaying ' + shot.name + "'s read mark on " + ch); return ch; })); });
    await pin(page, base, now, style);
    await page.goto(base + shot.path);
    await settle(page);   // a channel the viewer is not in is shown without a mark
    console.log('replayed ' + shot.name + ': ' + ((await Promise.all(marks)).join(', ') || 'no read mark'));
  } finally { await ctx.close(); }
}

/* One shard of the sweep: the rows mine (indexes into SHOTS, ascending) against the card world at base, which no other shard
   touches, so a row's farm scene or Glass claim is its shard's alone. Before each row, every channel a row before it in SHOTS
   opens and this shard has not read is replayed, so the row sees the read marks a whole sweep would leave it, whichever
   shard shot the rows before it (as GARDENHUB_GOLDEN_ONLY does). check: the sweep is whole, so a row that marks a channel no
   row before it opens goes in unreplayable. A shot that fails goes in failed and the sweep goes on, so one run names every
   failing shot. Resolves to the shard's wall time in ms. */
async function sweep(browser, base, mine, style, check, took, unreplayable, failed) {
  var read = {}, at = 0, t0 = Date.now();
  for (var i of mine) {
    for (; at < i; at++) {
      var skipped = channelOf(SHOTS[at].path);
      if (skipped && !read[skipped]) { await replayReads(browser, base, SHOTS[at], style); read[skipped] = true; }   // a later surface sees it read
    }
    at = i + 1;
    var ch = channelOf(SHOTS[i].path), ts = Date.now();
    for (var [device, theme] of cellsOf(SHOTS[i])) {
      var reads;
      try { reads = await shootRetried(browser, base, SHOTS[i], device, theme, style); }
      catch (e) {
        var shot = SHOTS[i].name + '-' + device + '-' + theme, why = shot + ': ' + String(e && e.message || e).split('\n')[0];
        // a quarantined shot that cannot even be taken fails no sweep: golden.Check reports its golden as QUARANTINED
        if (QUARANTINED.has(shot + '.png')) { console.log('QUARANTINED capture ' + why); continue; }
        console.log('golden failed ' + why + '\n' + (e && e.stack || e));
        failed.push(why);
        continue;
      }
      reads.forEach(function (r) {
        // a whole sweep checks the replay is whole: a row marks only its path's channel, or one a row before it opens
        if (check && r !== ch && !read[r]) unreplayable.push(SHOTS[i].name + ' marks ' + r + ' read, not its path\'s channel');
        read[r] = true;
      });
    }
    took.push([SHOTS[i].name, Date.now() - ts]);
  }
  return Date.now() - t0;
}

if (require.main !== module) {
  module.exports = { SHOTS: SHOTS, ROOTS: ROOTS, FOCUS_ROOTS: FOCUS_ROOTS, devices: devices, fontStyle: fontStyle, pin: pin, settle: settle, faceShot: faceShot, birdsShown: birdsShown, plotsClip: plotsClip, potShot: potShot, stakedShot: stakedShot, goatShot: goatShot, truckShot: truckShot, releaseShot: releaseShot, paneClip: paneClip, workbenchShown: require(path.join(__dirname, 'golden_workbench_browser.js')).workbenchShown, panelShot: panelShot, drawnText: drawnText };
} else (async function () {
  assert.ok(url && mods && out && fonts && now > 0, 'usage: node golden_browser.js <base url> <node_modules> <out dir> <fonts dir> <now ms>');
  pw = require(require.resolve('playwright-core', { paths: [mods] }));
  DEVICES = devices(pw);
  var browser;
  try { browser = await pw.chromium.launch(); }
  catch (e) { console.log('SKIP: ' + String(e.message || e).split('\n')[0]); process.exit(3); }
  console.log('golden browser ' + browser.version() + ' ' + require(require.resolve('playwright-core/package.json', { paths: [mods] })).version);   // golden.Capture
  try {
    var style = fontStyle(fonts);
    var only = (process.env.GARDENHUB_GOLDEN_ONLY || '').split(',').map(function (s) { return s.trim(); }).filter(Boolean);   // some surfaces only (GOLDENS.md)
    // a row GARDENHUB_GOLDEN_CELLS leaves no cell of is skipped, so the rows after it get its read marks replayed
    var rows = SHOTS.map(function (sh, i) { return i; }).filter(function (i) { return (!only.length || only.indexOf(SHOTS[i].name) >= 0) && cellsOf(SHOTS[i]).length; });
    var bases = url.split(','), took = [], unreplayable = [], failed = [], t0 = Date.now();   // took: per surface, every device and theme, for the "golden time" line
    var shards = bases.map(function (base, k) { return { base: base, rows: rows.filter(function (i, j) { return j % bases.length === k; }) }; });
    var spent = await Promise.all(shards.map(function (sh) { return sweep(browser, sh.base, sh.rows, style, !only.length && !CELLS.length, took, unreplayable, failed); }));
    took.sort(function (a, b) { return b[1] - a[1]; });   // what to shard on when the sweep outgrows its -timeout (gd-JWgIQDt4Yr)
    console.log('golden time ' + Math.round((Date.now() - t0) / 1000) + 's on ' + shards.length + ' shard(s) (' +
      spent.map(function (ms) { return Math.round(ms / 1000) + 's'; }).join(', ') + '), slowest: ' +
      took.slice(0, 10).map(function (r) { return r[0] + ' ' + (r[1] / 1000).toFixed(1) + 's'; }).join(', '));
    assert.deepStrictEqual(failed.sort(), [], 'shots that failed (each "golden failed" line above has its stack)');
    assert.deepStrictEqual(unreplayable.filter(function (v, k, a) { return a.indexOf(v) === k; }), [],
      'GARDENHUB_GOLDEN_ONLY and the shards replay a row they skip by opening its path; give these rows a path that opens the channel they read');
    console.log('golden shots taken');
  }
  finally { await browser.close(); if (glBrowser) await (await glBrowser).close(); }
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
