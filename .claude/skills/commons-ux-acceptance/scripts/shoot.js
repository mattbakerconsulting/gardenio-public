/* Shoot a /garden surface for a Commons UX grade (gardenio/docs/commons-ux-rubric.md): every capture-matrix cell, the §2 DOM
   probes in each, and a draft COMMONS-UX-GRADE.md per surface for the grader to finish from the PNGs. Run by
   TestCommonsUXShoot (gardenio/controller/internal/gardenhub/accept_browser_test.go), which serves /garden on an httptest
   server fed the golden harness's fixture farm with a frozen clock; `make commons-ux-shoot OUT=... SURFACES=...` runs that.
     node shoot.js <base url> <node_modules dir> <out dir> <golden_browser.js> <fonts dir> <now, unix ms>
   COMMONS_UX_SURFACES and COMMONS_UX_CELLS (comma lists) narrow the run; the default is every surface in every cell.
   COMMONS_UX_PERF=1 also measures each surface against the rubric's §7 budgets (perfOf, below), into
   <surface>-{phone,desktop}-light.perf.json and the draft's perf rows; it is on by default when COMMONS_UX_SURFACES names
   surfaces and off for a full-matrix run (=0 turns it off). COMMONS_UX_PERF_RUNS (default 3) and COMMONS_UX_PERF_IDLE
   (seconds, default 60; 0 skips the idle pass and B8/B9) shorten it. The perf pass measures on COMMONS_UX_PERF_URL when set
   (TestCommonsUXShoot's perfWorld: the same farm with its Commons live and the latency pass's test routes, B12-B15), else on
   the shots' own farm, where the stream budget (B15) reads ungraded.

   The surfaces are the golden harness's own SHOTS rows (required from golden_browser.js, so a graded surface is exactly the
   one its golden guards) plus EXTRA rows for surfaces that have no golden yet. Pinned fonts, frozen clock, reduced motion
   and the settle step also come from there, so an accepted capture can be promoted to a golden as is (rubric §5). */
'use strict';
var url = process.argv[2], mods = process.argv[3], out = process.argv[4], goldenJs = process.argv[5], fonts = process.argv[6], now = +process.argv[7];
var fs = require('fs'), path = require('path'), crypto = require('crypto'), assert = require('assert');
var probes = require('./probes.js'), golden = require(path.resolve(goldenJs));
/* The perf rows' measuring and grading are P0.1's and P3.1's own, from the golden harness's testdata next to golden_browser.js. */
var perf = require(path.join(path.dirname(path.resolve(goldenJs)), 'perf_baseline.js')), perfGate = require(path.join(path.dirname(path.resolve(goldenJs)), 'perf_gate.js'));
var env = process.env, PERF = env.COMMONS_UX_PERF ? env.COMMONS_UX_PERF !== '0' : !!env.COMMONS_UX_SURFACES;
var PERF_RUNS = +(env.COMMONS_UX_PERF_RUNS || 3), PERF_IDLE = +(env.COMMONS_UX_PERF_IDLE === undefined ? 60 : env.COMMONS_UX_PERF_IDLE);

/* A viewer who may write (cardWorldAs's uxWriter): the bead view's write bar, the cockpit's and the queues' writes. */
var WRITER = { extraHTTPHeaders: { 'X-UX-Writer': '1' } };
/* The Workbench readers' other worlds (X-UX-Fixture, ux_fixture_test.go): stress (F1 long text, F2 unbroken tokens, F3 many
   rows, F4 emoji and non-Latin, F6 references the daemon does not have), empty, and down (a daemon that does not answer:
   the panel's failure line, or for the bead view its comments and links). writer: as WRITER too. */
function world(v, writer) { var h = { 'X-UX-Fixture': v }; if (writer) h['X-UX-Writer'] = '1'; return { extraHTTPHeaders: h }; }
var STRESS = world('stress'), EMPTY = world('empty'), DOWN = world('down');
// a failed load's line (WB-15)
var FAILED = '#bv-body .bv-note.err';

// the Costs panel's rows are ready on a card, a failure's note or its no-charts line (.gp-cards is there before the load ends)
var COSTS_READY = '#bv-body .gp-card, #bv-body .bv-note.err, #bv-body .bv-empty';
var LONG_LAND = 'a-tenant-land-named-at-the-longest-a-land-name-may-be-58ch';
/* The Escalations zone at gardenio's farmstead (plan gd-PJB3D7glwF step 8, gd-AmIpCiVTvY), its escalations added to the farm
   document on its way to the page (the fixture farm has one), the cursor on the first: one of each state, so the grid's name
   counts "2 waiting on you, 2 running, ..." and a phone's bar over the pad reads the cursor's tip; stress adds F1 200-char
   titles, F2 a URL and a SHA, F3 30 of them past ESCKIND.CAP (the +N crate) and F4 emoji and Arabic. escFarm adds n of them
   to the farm document from the next load on. */
function escFarm(page, n, stress) {
  var FAM = ['merge', 'deploy', 'config', 'prompt', 'beads', 'access', 'infra', 'read', 'other'], ST = ['pending', 'approved', 'pending', 'rejected', 'failed'];
  var long = 'Merge https://github.com/mattbakerconsulting/gardenio/pull/2244/checks?sha=0123456789abcdef0123456789abcdef01234567 🧑\u200d🌾 مريم reviewed it, café ';
  return page.route('**/garden/farm', async function (route) {
    var res = await route.fetch(), doc = await res.json(), t = Math.floor(now / 1000), es = doc.escalations || (doc.escalations = { open: [], total: 0, by_project: {} });
    for (var i = 0; i < n; i++) {
      var st = ST[i % ST.length], human = st === 'pending' && i % 2 === 0;
      es.open.push({ id: 'gd-EscUX' + (i < 9 ? '0' : '') + (i + 1), kind: 'escalation', state: st, human: human, waits_on: st === 'pending' && !human ? 'the merge desk' : undefined,
        title: stress ? (long.repeat(2)).slice(0, 200) : ['Merge the fix', 'Roll the chart', 'Grant read on the bucket', 'Install the steward prompt', 'Close the stale beads'][i], since: t - 600 * (i + 1), project: 'gardenio', family: FAM[i % FAM.length] });
    }
    es.total = es.open.length; es.by_project = es.by_project || {}; es.by_project.gardenio = es.open.filter(function (w) { return w.project === 'gardenio'; }).length;
    await route.fulfill({ response: res, json: doc });
  });
}
function escShot(name, stress) {
  return { name: name, path: '/garden?at=gardenio/esc%3Agd-EscUX01', show: async function (page) {
    await escFarm(page, stress ? 30 : 5, stress);
    await page.reload();
    await page.waitForSelector('#hots a.w-escalation.cur', { state: 'attached' });
    await page.waitForSelector('#cursor-bar .cb-me');
    await page.mouse.move(1, 1);
    await page.waitForTimeout(250);
  } };
}
function barnEsc(name, n) {
  var barn = golden.SHOTS.filter(function (r) { return r.name === 'barn'; })[0];
  return { name: name, path: barn.path, show: async function (page, as) {
    await escFarm(page, n, n > 5);
    await page.reload();
    await barn.show(page, as);
    await page.mouse.move(1, 1);
    await page.waitForTimeout(250);
  } };
}
/* Surfaces with no golden yet. Same shape as a SHOTS row; show(page, as) gets 'phone' (touch, phone layout) or 'desktop'. */
var EXTRA = [
  escShot('escalations', false), escShot('escalationsstress', true),
  // the barn golden's framing with 12 and 30 escalations: on a phone the Escalations cell is at the right edge, where the
  // off-screen arrows keep off its items (gd-WzYgtbwMT7)
  barnEsc('barnesc12', 12), barnEsc('barnesc30', 30),
  // The release panel (gd-rSz81Me1Pe, web/release.js; golden.releaseShot) under stress: F1 200-char titles, F2 an unbroken
  // URL, F3 72 PRs over 9 plans, F4 emoji and Arabic, F6 plans and steps the daemon does not have ("Untitled plan").
  golden.releaseShot('releasestress', true),
  // the land header under stress (Lands 8.8, golden_land_browser.js): F1 a land name at the longest a land may have (58), the
  // purse unknown (its words and the alert fill) beside the steward resting at a three-digit run cap (gd-OvKMtyWtrp)
  { name: 'landstress', path: '/garden?land=' + LONG_LAND, show: require(path.join(path.dirname(path.resolve(goldenJs)), 'golden_land_browser.js')).show(
    { state: 'unknown', blocked: true, reason: 'spend source unreachable', granted_credits: 100000, remaining_credits: null, margin_credits: 5000, used_pct: null }, LONG_LAND,
    { max_runs_per_day: 120, runs_today: 120, capped: true, resumes_at: 1792800000 }) },
  // the impersonation banner (plan gd-0SUIdKXWV3 step 4) under F1: a 90-char unbroken admin name beside the tenant, which the
  // banner wraps and scrolls inside its strip with Stop kept whole (the impersonated golden rows are the plain look)
  { name: 'impersonatedstress', path: '/garden', root: '#imp',
    ctx: { extraHTTPHeaders: { 'X-UX-Impersonator': 'matthew.an-admin-username-long-enough-to-run-past-any-banner-at-all@clonk.example.tv' } },
    show: async function (page) { await page.waitForSelector('#imp .imp-stop', { state: 'visible' }); await page.waitForSelector('#cursor-bar .cb-me'); } },
  // the Commons v2 workbench (plan gd-lRaCxsiLvC step 4) on a phase root, and on its stress epic (workbench_world_test.go:
  // F1 a 200-char title, F2 a URL, a SHA and ids, F3 40 tasks, F4 emoji, CJK and Arabic, F6 a child the daemon lacks)
  { name: 'commonsphase', path: '/garden/c/n/gd-wbPhase1', show: golden.workbenchShown() },
  { name: 'commonsplanstress', path: '/garden/c/n/gd-wbStress', show: golden.workbenchShown('gd-wbX7') },
  // the steward root on the stress land (gd-r7JiBQdZNS; steward_stress_test.go): F1 a 200-char plan title and a 90-char run, F2
  // tokens in its reason, notes, job and threads, F3 13 plans and a thread past its replies, F4 emoji, CJK and Arabic, F6 a plan,
  // a step and a decision the daemon lacks. Shown as the commonssteward golden row is: a phone opens on the steward screen (S3,
  // gd-qV1nCdpbVA), which has no tree
  { name: 'commonsstewardstress', path: '/garden/c/s/stewards', root: '#wb2', show: function (page, as) {
    return golden.SHOTS.filter(function (s) { return s.name === 'commonssteward'; })[0].show(page, as);
  } },
  // an agent's node with its live head's Nudge box opened in place (gd-1iJEc1nYqm; node.html's page-nudge.js)
  { name: 'commonsagentnudge', path: '/garden/c/n/gd-wbCrew3', devices: ['desktop'], root: '#wb2', show: async function (page) {
    await golden.workbenchShown()(page);
    await page.click('#wb2 .wb2-head .cm-node-live .cm-nudge');
    await page.waitForSelector('#wb2 .wb2-head form.gl-nudge textarea', { state: 'visible' });
  } },
  // the steward screen's other tabs on a phone (steward view S3, gd-qV1nCdpbVA): Jobs, Tending (the plans it tends, the
  // stalled and the parked) and Asks, each tapped from the commonssteward golden row's screen
  ...[['jobs', 'jobs'], ['tending', 'plan'], ['asks', 'open']].map(function (t) {
    return { name: 'commonssteward' + t[0], path: '/garden/c/s/gardenio', devices: ['phone'], root: '#wb2', show: async function (page, as) {
      await golden.SHOTS.filter(function (s) { return s.name === 'commonssteward'; })[0].show(page, as);
      await page.tap('#wb2 .wb2-stab[data-tab=' + t[1] + ']');
      await page.waitForFunction(function (k) { return COMMONS2.state().tab === k && !!document.querySelector('#wb2 [data-rail=' + k + ']').offsetParent; }, t[1]);
    } };
  }),
  // the typed faces under stress (WB-1, gd-vFUZG6FnTT; faceStressBeads in bead_face_test.go): a decision with F1 a long title and
  // prompt, F2 URLs and a branch in its options, F3 fourteen options, F4 emoji and RTL, F6 an asker and task not in the daemon;
  // an escalation with an unbroken URL and SHA and a 90-char asker; an agent with a 90-char name and a URL in its wrap-up
  golden.faceShot('facedecisionstress', 'gd-FaceStressD1'), golden.faceShot('faceescalationstress', 'gd-FaceStressE1', true),
  golden.faceShot('faceagentstress', 'gd-FaceStressA1'),
  // the cursor on gardenio's home cell at the Project level, its ring on the lane (the cursorproject golden row's ring on a
  // home cell; by day once 1.3:1 on the lane, gd-y7dymfevBP)
  { name: 'cursorhome', path: '/garden?at=gardenio/home', root: '#lbls', show: async function (page) {
    await page.waitForSelector('#hots .hot.cur', { state: 'attached' });
    await page.waitForSelector('#cursor-bar .cb-me');
    await page.evaluate(function () { var a = document.activeElement; if (a && a.blur) a.blur(); });   // the ring as .cur draws it, no focus
  } },
  // tool calls as birds at the Bead level (gd-Wucpbrm3Z6): the birds golden row's flock and hawk with the camera at 1x, where a
  // bird is drawn bigger than at Plan (its still birds are the birdsbeadstill golden row, 4n)
  { name: 'birdsbead', path: '/garden?at=gardenio/gd-planA', clip: golden.plotsClip(['gd-planA'], 24, 30), show: golden.birdsShown(true, 3) },
  // the birds row's plates lingering (gd-2mYRxayl9b, captions spec 9.2): the clock 5 s past the lift, the birds gone 3 s since
  // and each plate perched by its gardener's head (the hawk still circles, its subagent running)
  { name: 'birdslinger', path: '/garden?at=gardenio/gd-planA', clip: golden.plotsClip(['gd-planA', 'gd-planB'], 40, 30), show: golden.birdsShown(true, 2, 5000) },
  // A screenshot report as an agent files one (task 3.7, gd-GJ3xhSQsdh; reportBeads in bead_face_test.go): markdown with
  // headings, captioned laptop and phone screenshots through the /image/ proxy, a list, a table, a quote, code, an outside
  // picture and F2 a long URL, shot once every picture has loaded (they load lazily, so the shot asks for them now)
  { name: 'report', path: '/garden?bead=gd-Report3p6', show: async function (page, as) {
    await golden.faceShot('report', 'gd-Report3p6').show(page, as);
    await page.evaluate(function () { document.querySelectorAll('#bv-body img').forEach(function (i) { i.loading = 'eager'; }); });
    await page.waitForFunction(function () { return Array.prototype.every.call(document.querySelectorAll('#bv-body img'), function (i) { return i.complete; }); });
  } },
  // The steward's tip (Lands 9.8) under stress: the golden row with music's steward rewritten in the farm document on its way to
  // the page: F1 a 90-char agent name and a reason at the server's 140-rune clip, F2 an unbroken URL and bead id, F4 emoji and RTL
  { name: 'stewardstress', path: '/garden?at=music', show: async function (page, as) {
    await page.route('**/garden/farm', async function (route) {
      var res = await route.fetch(), doc = await res.json();
      (doc.cost && doc.cost.projects || []).forEach(function (r) {
        if (r.project !== 'music' || !r.steward) return;
        r.steward.agent = 'music-steward-' + 'with-a-very-long-land-name-'.repeat(3).slice(0, 76);
        r.steward.reason = { kind: 'in_flight', text: ('Waiting on CI for https://github.com/acme/music/pull/4242/checks?check_run_id=0123456789abcdef 🧑\u200d🌾 مريم reviewed gd-AbCdEfGhIj and ').slice(0, 140) };
        r.steward.capped = true; r.steward.max_runs_per_day = 24; r.steward.runs_today = 24;
      });
      await route.fulfill({ response: res, json: doc });
    });
    await page.reload();
    return golden.SHOTS.filter(function (s) { return s.name === 'steward'; })[0].show(page, as);
  } },
  // A Nursery pot's card under stress (9.11b, gd-m38yKRX0hE; golden_browser.js potShot, whose plain rows are goldens): F1 a
  // 200-char title, F2 a URL and bead id in the prompt, F3 eight long options, F4 emoji and RTL, and a decision for someone
  // else ("Waiting on mo's answer"); the tip and the bead view.
  golden.potShot('potstress', true, false), golden.potShot('potbeadstress', true, true),
  // The ready releases plan staked out in the fields (staked plots step 4, gd-zcUua6XeEJ; golden_browser.js stakedShot): its
  // plot's tip and its bead view, plain and under F1 200-char titles, F2 URLs in the bodies, F4 emoji, a flag and Arabic, and
  // F6 a step the daemon does not have.
  // (the plain rows, staked and stakedbead, are goldens)
  golden.stakedShot('stakedstress', true, false), golden.stakedShot('stakedbeadstress', true, true),
  // A goat's tip with its kin's line arms (heraldry 5.10, gd-WUqr6IkgkM; golden_browser.js goatShot, whose goatarms, goatgate
  // and goatpen rows are goldens): the goat grazing by plan A untended, a ghost wearing its kin's arms
  golden.goatShot('goatghost', 'out', true),
  // The World level fully zoomed out (4b.2, gd-SU76HOEOeJ): depth 0 on gardenio, the other projects headed, nothing over the
  // map (no plan board, gd-OOt8Mx2Mps). A phone frames depth 0 at the World level; a desktop window only when it is small
  // enough, so it is made smaller until the camera is at level 0
  { name: 'toplod', path: '/garden?at=gardenio', show: async function (page, as) {
    var top = function () { return page.evaluate(function () { return window.GARDEN.cam().level === 0; }); };
    await page.waitForSelector('#cursor-bar .cb-me');
    for (var i = 0; i < 4 && !(await top()); i++) {
      var vs = page.viewportSize();
      await page.setViewportSize({ width: Math.round(vs.width * 0.75), height: Math.round(vs.height * 0.75) });
      await page.waitForTimeout(300);
    }
    await page.waitForFunction(function () { return window.GARDEN.cam().level === 0; });
    await page.waitForSelector('#lbls .sign.head');
    await page.waitForTimeout(250);
  } },
  // The bead view under the rubric's stress fixtures (F1 long text, F2 unbroken tokens, F4 emoji and non-Latin, F6 a missing
  // child): gd-StressF1F2 is on no plot, so only this view reaches it (stressBead in card_browser_test.go).
  { name: 'beadstress', path: '/garden?bead=gd-StressF1F2', show: async function (page) {
    await page.locator('#bead').waitFor({ state: 'visible' });
    await page.waitForFunction(function () { var b = document.getElementById('bv-body'); return b.textContent.trim() && !/Loading/.test(b.textContent); });
  } },
  // A Slack wish with its whole family (plan gd-gO1fTT7oGC step 1; commonstest.WishFamily, wishSlack in commons_world_test.go):
  // replies by the person, squawks, a card, a session, an agent, its task and MR, and comments, in its three views: the bead
  // view, its workbench node and gardenio's steward (whose node does not list the land's wishes yet). On no plot.
  { name: 'wishbead', path: '/garden?bead=gd-wishSlack', show: async function (page) {
    await page.locator('#bead').waitFor({ state: 'visible' });
    await page.waitForFunction(function () { var b = document.getElementById('bv-body'); return b.textContent.trim() && !/Loading/.test(b.textContent); });
  } },
  { name: 'wishnode', path: '/garden/c/n/gd-wishSlack', root: '#wb2', show: golden.workbenchShown() },
  { name: 'wishsteward', path: '/garden/c/s/gardenio', root: '#wb2', show: function (page, as) {
    return golden.SHOTS.filter(function (s) { return s.name === 'commonssteward'; })[0].show(page, as);
  } },
  // A closed step's Harvest section (harvest spec 6.2, step 4b.6.3): gd-HarvestSt1 is on no plot (harvestStep in
  // bead_harvest_test.go); shot once its document and pictures have loaded.
  { name: 'beadharvest', path: '/garden?bead=gd-HarvestSt1', show: async function (page) {
    await page.locator('#bead').waitFor({ state: 'visible' });
    await page.waitForFunction(function () {
      var b = document.getElementById('bv-body');
      return b.textContent.trim() && !/Loading/.test(b.textContent) && Array.prototype.every.call(b.querySelectorAll('.bv-pics img'), function (i) { return i.complete; });
    });
  } },
  { name: 'picpreviewharvest', path: '/garden?bead=gd-HarvestSt1&at=gardenio', show: async function (page, as) {
    await harvestPics(page);
    await previewOn(page, as, '#bv-body .bv-pics a >> nth=0');
  } },
  { name: 'picviewerharvest', path: '/garden?bead=gd-HarvestSt1&at=gardenio', root: '#bead', show: async function (page) {
    await harvestPics(page);
    await page.click('#bv-body .bv-pics a >> nth=1');
    await viewerShown(page);
  } },
  // The camera's Menu open on its Snap item (gd-GbF2JS04h5; Snap moved into the Menu in 3b R8). The fixture page has no
  // signed-in viewer, for whom Snap is hidden, so the shot unhides it: what is graded is its look and place in the Menu,
  // not who is offered it.
  { name: 'snapbar', path: '/garden', show: async function (page) {
    await page.waitForSelector('#cam-menu', { state: 'visible' });
    await page.evaluate(function () { document.getElementById('cam-snap').hidden = false; });
    await page.click('#cam-menu');
    await page.locator('#cam-snap').waitFor({ state: 'visible' });
  } },
  // The Postcard (gd-WxRuyfdUtR): the Menu open on plan A's plot with Snap and Postcard unhidden (as snapbar does: the
  // fixture page has no signed-in viewer); then Postcard pressed, the plot's picture and caption waiting in the composer of
  // plan A's thread (the Porch wish, commons_world_test.go)
  { name: 'postcardmenu', path: '/garden?at=gardenio/gd-planA', show: async function (page) {
    await page.waitForSelector('#cursor-bar .cb-me');
    await page.evaluate(function () { ['cam-snap', 'cam-postcard'].forEach(function (id) { var b = document.getElementById(id); if (b) b.hidden = false; }); });
    await page.click('#cam-menu');
    await page.locator('#cam-snap').waitFor({ state: 'visible' });
  } },
  /* the page without script is the Ledger (3b R4, gd-ynRqThFqwE): the noscript style shows its three lists under the plain bar */
  { name: 'noscript', path: '/garden', ctx: { javaScriptEnabled: false }, show: async function () {} },
  // The Workbench check-in (Lands 5.14, gd-ye6hpIWoDf): every panel tab the golden rows (queues, project, projectcost,
  // gardener, settings) do not already shoot, the Costs panel kind, and the bead view, cockpit and queues as a writer sees
  // them (X-UX-Writer, cardWorldAs's uxWriter): the write bar, the inline editor, Put an agent on this
  wbShot('queueswriter', '/garden?queue=inbox', 'queue', '#bv-body .qv-list', WRITER),
  wbShot('projectconfig', '/garden?project=gardenio&tab=config', 'project', '#pj-pane'),
  wbShot('gardenerbeads', '/garden?gardener=gd-GardTrig01&tab=beads', 'gardener', '#gd-pane'),
  wbShot('gardenerconfig', '/garden?gardener=gd-GardTrig01&tab=config', 'gardener', '#gd-pane'),
  wbShot('gardenersuccessors', '/garden?gardener=gd-GardTrig01&tab=successors', 'gardener', '#gd-pane'),
  wbShot('costsfleet', '/garden?costs=fleet', 'costs', COSTS_READY),
  wbShot('cockpitwriter', '/garden?plan=gd-planA', 'plan', '#bv-body .pv-step', WRITER),
  wbShot('beadwriter', '/garden?bead=gd-planA', 'bead', '#bv-body .bw-bar', WRITER),
  wbShot('beadedit', '/garden?bead=gd-planB1a', 'bead', '#bv-body .bw-bar', WRITER, async function (page) {
    await page.click('#bv-body .bw-bar button:text-is("Edit")');
    await page.waitForSelector('#bv-body .bw-slot form', { state: 'visible' });
  }),
  wbShot('beadspawn', '/garden?bead=gd-planB1a', 'bead', '#bv-body .bw-bar', WRITER, async function (page) {
    await page.click('#bv-body .bw-bar button:text-is("Put an agent on this")');
    await page.waitForSelector('#bv-body .bw-spawn .bw-text', { state: 'visible' });
  }),
  // The Workbench panels under stress, empty and down (gd-wSb1jRJnJm): every panel kind the 5.14 check-in graded on short ASCII
  // data only, once with the rubric's stress fixtures, once with nothing in it and once with the daemon not answering. Cmd-K's
  // suggestions are the farm's own (WORLD.search, eight at most) and the Plans drawer's the farm's plans, so neither has a
  // reader to stress; the farm's long-titled plans and phases (cardWorld) are their F1.
  wbShot('queuesstress', '/garden?queue=inbox', 'queue', '#bv-body .qv-list .qv-row', STRESS),
  wbShot('queuedecisionsstress', '/garden?queue=decisions', 'queue', '#bv-body .qv-list .qv-row', STRESS),
  wbShot('queuesempty', '/garden?queue=inbox', 'queue', '#bv-body .bv-empty', EMPTY),
  wbShot('queueserr', '/garden?queue=inbox', 'queue', FAILED, DOWN),
  wbShot('searchstress', '/garden?q=edge', 'q', '#bv-body .sv-row', STRESS),
  wbShot('searcherr', '/garden?q=edge', 'q', FAILED, DOWN),
  wbShot('cockpitstress', '/garden?plan=gd-planA', 'plan', '#bv-body .pv-step', STRESS),
  wbShot('cockpitempty', '/garden?plan=gd-planA', 'plan', '#bv-body .bv-acts, #bv-body .bv-empty, #bv-body .bv-note', EMPTY),
  wbShot('cockpiterr', '/garden?plan=gd-planA', 'plan', FAILED, DOWN),
  wbShot('bundlestress', '/garden?bundle=gd-FaceBundle1', 'bundle', '#bv-body .bd-doc', STRESS),
  wbShot('bundleempty', '/garden?bundle=gd-FaceBundle1', 'bundle', '#bv-body .bv-empty, #bv-body .bv-note', EMPTY),
  wbShot('bundleerr', '/garden?bundle=gd-FaceBundle1', 'bundle', FAILED, DOWN),
  wbShot('projectstress', '/garden?project=gardenio', 'project', '#pj-pane .qv-row', STRESS),
  wbShot('projectagentsstress', '/garden?project=gardenio&tab=agents', 'project', '#pj-pane .qv-row', STRESS),
  wbShot('projectconfigstress', '/garden?project=gardenio&tab=config', 'project', '#pj-pane', STRESS),
  wbShot('projectempty', '/garden?project=gardenio', 'project', '#pj-pane', EMPTY),
  wbShot('projecterr', '/garden?project=gardenio', 'project', FAILED, DOWN),
  wbShot('gardenerstress', '/garden?gardener=gd-GardTrig01&tab=runs', 'gardener', '#gd-pane .qv-row', STRESS),
  wbShot('gardenerbeadsstress', '/garden?gardener=gd-GardTrig01&tab=beads', 'gardener', '#gd-pane .qv-row', STRESS),
  wbShot('gardenerempty', '/garden?gardener=gd-GardTrig01&tab=runs', 'gardener', '#gd-pane', EMPTY),
  wbShot('gardenererr', '/garden?gardener=gd-GardTrig01&tab=runs', 'gardener', FAILED, DOWN),
  // One gardener's transcript (web/transcript.js, bird-captions spec 11.4): its golden row 'transcript' (golden_browser.js)
  // shoots gd-agentBeadW's 60 calls; an id the farm has no gardener for is its 404 line.
  wbShot('transcripterr', '/garden?transcript=gd-NoSuchGard', 'transcript', FAILED),
  wbShot('settingsstress', '/garden?settings=me', 'settings', '#bv-body .pj-sec', STRESS),
  wbShot('settingsempty', '/garden?settings=me', 'settings', '#bv-body .pj-sec, #bv-body .bv-note', EMPTY),
  wbShot('settingserr', '/garden?settings=me', 'settings', FAILED, DOWN),
  wbShot('costserr', '/garden?costs=nosuchland', 'costs', COSTS_READY),
  // the bead view with the daemon answering the bead but not its comments or links (WB-15's soft degradation)
  wbShot('beaderr', '/garden?bead=gd-planA', 'bead', '#bv-body .bv-note', DOWN),
  // The forms the check-in did not open: the comment composer with a draft (F2 tokens, F4) and its Preview, a step's Rename in
  // the cockpit, Pour a formula with its formula chosen, the settings editor (and under stress), the Plans drawer's Forest.
  wbShot('commentpreview', '/garden?bead=gd-planA', 'bead', '#bv-body form.bw-comment textarea', WRITER, async function (page) {
    var ta = page.locator('#bv-body form.bw-comment textarea');
    await ta.scrollIntoViewIfNeeded();
    await ta.fill('Merged in https://gitlab.example.com/gardenio/controller/-/merge_requests/4242/diffs?commit_id=0123456789abcdef0123456789abcdef01 at 0123456789abcdef0123456789abcdef01234567.\n\n🧑‍🌾 مريم and 田中花子 reviewed it; café.');
    await page.click('#bv-body form.bw-comment .bw-tog');
    await page.waitForFunction(function () { var p = document.querySelector('#bv-body form.bw-comment .bw-pv'); return p && !p.hidden && !/Rendering/.test(p.textContent); });
    await page.locator('#bv-body form.bw-comment').scrollIntoViewIfNeeded();
  }),
  wbShot('commentdraft', '/garden?bead=gd-planA', 'bead', '#bv-body form.bw-comment textarea', WRITER, async function (page) {
    var ta = page.locator('#bv-body form.bw-comment textarea');
    await ta.scrollIntoViewIfNeeded();
    await ta.fill('A draft with a long URL https://gitlab.example.com/gardenio/controller/-/merge_requests/4242/diffs?commit_id=0123456789abcdef0123456789abcdef01 in it.');
  }),
  wbShot('cockpitrename', '/garden?plan=gd-planA', 'plan', '#bv-body .pv-step', WRITER, async function (page) {
    var more = page.locator('#bv-body .pv-step .pv-more').first();   // a phone folds a step's verbs under Actions
    if (await more.isVisible()) await more.click();
    var b = page.locator('#bv-body .pv-step button:text-is("Rename")').first();
    await b.scrollIntoViewIfNeeded();
    await b.click();
    await page.waitForSelector('#bv-body .pv-step form input', { state: 'visible' });
  }),
  wbShot('cockpitpour', '/garden?plan=gd-planA', 'plan', '#bv-body .pv-pour', WRITER, async function (page) {
    var s = page.locator('#bv-body .pv-pour > summary');
    await s.scrollIntoViewIfNeeded();
    await s.click();
    await page.waitForSelector('#bv-body .pv-pour select[name=formula]', { state: 'visible' });
    await page.selectOption('#bv-body .pv-pour select[name=formula]', 'gd-fRel');
    await page.locator('#bv-body .pv-pour form').scrollIntoViewIfNeeded();
  }),
  wbShot('settingseditstress', '/garden?settings=me', 'settings', '#bv-body .pj-sec', STRESS, settingsEditOpened),
  { name: 'plansforest', path: '/garden', root: '#dw-plans', show: async function (page) {
    await page.waitForSelector('#cursor-bar .cb-me');
    if (await page.locator('#cam-plans').count()) { await page.click('#cam-menu'); await page.click('#cam-plans'); }
    else await page.click('#dw-plans > summary');
    await page.locator('#plans-slot').waitFor({ state: 'visible' });
    await page.click('#plans-slot button:text-is("Forest")');
    await page.locator('#plans-slot .forest').waitFor({ state: 'visible' });
  } },
  // the Plans drawer open: from the camera's Menu (3b R9), or its button on the map before that, so a before/after pair shoots
  { name: 'plansdrawer', path: '/garden', show: async function (page) {
    await page.waitForSelector('#cursor-bar .cb-me');
    if (await page.locator('#cam-plans').count()) { await page.click('#cam-menu'); await page.click('#cam-plans'); }
    else await page.click('#dw-plans > summary');
    await page.locator('#plans-slot').waitFor({ state: 'visible' });
  } },
];

/* A Workbench panel row (Lands 5.14): the golden harness's panelShot (path opened, the panel up as kind, ready inside it and
   no Loading line, then(page, as) to open a form after) with ctx (WRITER, STRESS, EMPTY, DOWN). A row promoted to a golden
   is the same panelShot without ctx. */
function wbShot(name, path, kind, ready, ctx, then) {
  return Object.assign(golden.panelShot(name, path, kind, ready, then), { ctx: ctx, root: '#bead' });
}
/* The settings panel's Edit your settings pressed, its form up. */
async function settingsEditOpened(page) {
  await page.click('#bv-body button:text-is("Edit your settings")');
  await page.waitForSelector('#bv-body form[aria-label="Edit your settings"]', { state: 'visible' });
}

/* What the probes look inside, per surface, and where the keyboard pass looks for focus: shared with the golden harness's C3
   pass (golden_roots_browser.js). */
var ROOTS = Object.assign({}, golden.ROOTS), FOCUS_ROOTS = golden.FOCUS_ROOTS;
EXTRA.forEach(function (x) { if (x.root) ROOTS[x.name] = x.root; });


/* The capture matrix (rubric §1). as is the layout show() should drive; desktop cells are 1440x900 as the rubric says (the
   goldens shoot 1280x800). docked is shot only for a surface with a dock(page) step, live not at all yet (see SKILL.md). */
function cells(pw) {
  var phone = Object.assign({}, pw.devices['iPhone 12'], { viewport: { width: 390, height: 844 }, deviceScaleFactor: 1 });
  var desktop = { viewport: { width: 1440, height: 900 }, deviceScaleFactor: 1 };
  return {
    'phone-light': { as: 'phone', ctx: phone, theme: 'light' },
    'phone-dark': { as: 'phone', ctx: phone, theme: 'dark' },
    'desktop-light': { as: 'desktop', ctx: desktop, theme: 'light' },
    'desktop-dark': { as: 'desktop', ctx: desktop, theme: 'dark' },
    mid: { as: 'phone', ctx: { viewport: { width: 820, height: 1180 }, deviceScaleFactor: 1, isMobile: true, hasTouch: true }, theme: 'light' },
    docked: { as: 'desktop', ctx: desktop, theme: 'light', needs: 'dock' },
    'focus-light': { as: 'desktop', ctx: desktop, theme: 'light', focus: true },
    'focus-dark': { as: 'desktop', ctx: desktop, theme: 'dark', focus: true },
  };
}

/* The phone sweep (Lands mobile M0.1, gd-0B2drDTgFw): Playwright's iPhone 12 (Safari's user agent, 390x664 portrait and 750x340
   landscape, the viewport left once Safari's bars are drawn) and Galaxy S8 (Android Chrome's, 360x740 and 740x360), each light
   and dark. Under 900 px wide even landscape is the phone layout. Shot only when COMMONS_UX_CELLS names them (or says sweep),
   never in the default matrix. The engine is always Chromium: user agent, size, touch and isMobile are emulated; WebKit's
   rendering is not. */
function sweepCells(pw) {
  var out = {};
  [['ios390', 'iPhone 12'], ['and360', 'Galaxy S8']].forEach(function (d) {
    var port = Object.assign({}, pw.devices[d[1]], { deviceScaleFactor: 1 }), land = Object.assign({}, pw.devices[d[1] + ' landscape'], { deviceScaleFactor: 1 });
    delete port.defaultBrowserType; delete land.defaultBrowserType;
    ['light', 'dark'].forEach(function (t) {
      out[d[0] + '-' + t] = { as: 'phone', ctx: port, theme: t, sweep: true };
      out[d[0] + 'land-' + t] = { as: 'phone', ctx: land, theme: t, sweep: true };
    });
  });
  return out;
}

/* The picture rows' steps: the grade's pictures loaded, a preview raised on sel, the viewer's picture and
   filmstrip loaded. */
function loaded(sel) {
  return function (page) {
    return page.waitForFunction(function (q) {
      var a = document.querySelectorAll(q);
      return a.length > 0 && Array.prototype.every.call(a, function (i) { return i.complete && i.naturalWidth > 0; });
    }, sel);
  };
}
async function harvestPics(page) {
  await page.locator('#bead').waitFor({ state: 'visible' });
  await loaded('#bv-body .bv-pics img')(page);
}
/* The page settled first (a panel lays out as its fonts land), the picture scrolled into view, then the preview
   raised; any scroll takes the preview down, so a keyboard focus that scrolled is tried again from where it now stands. */
async function previewOn(page, as, sel) {
  var a = page.locator(sel), up = function () { var t = document.getElementById('pic-preview'); return !!t && !t.hidden && t.querySelector('img').complete; };
  await golden.settle(page);
  await a.evaluate(function (e) { e.scrollIntoView({ block: 'center' }); });
  await golden.settle(page);
  if (as === 'desktop') await a.hover();
  else {
    await a.focus();
    for (var i = 0; i < 4 && !(await page.evaluate(up)); i++) {   // a keyboard focus: focus-visible
      await page.keyboard.press('Shift+Tab'); await page.keyboard.press('Tab'); await page.waitForTimeout(300);
    }
  }
  // a picture with no room for a bigger preview raises none (5.18, place() returns null): shoot the cell with none
  await page.waitForFunction(up, null, { timeout: 3000 }).catch(function () {});
}
async function viewerShown(page) {
  await page.waitForSelector('#bead[data-kind=pic] .pic-stage img');
  await loaded('#bead[data-kind=pic] .pic-stage img, #bead[data-kind=pic] .pic-thumb img')(page);
}

/* Which cells each gate is graded in (rubric §3), for the draft grade. */
var GATES = [
  ['C1', 'Truncation', ['phone-light', 'desktop-light', 'docked']],
  ['C2', 'Wrapping and overflow', ['phone-light', 'mid', 'docked']],
  ['C3', 'Phone (390 px)', ['phone-light', 'phone-dark']],
  ['C4', 'Desktop and docked', ['desktop-light', 'docked']],
  ['C5', 'Light and dark', ['desktop-light', 'desktop-dark', 'phone-dark']],
  ['C6', 'Garden and Business', ['desktop-light', 'phone-light']],
  ['C7', 'Emoji and glyphs', ['desktop-light', 'desktop-dark', 'phone-light']],
  ['C8', 'Controls', ['desktop-light', 'desktop-dark', 'phone-light', 'live']],
  ['C9', 'State at a glance', ['desktop-light', 'desktop-dark', 'phone-light']],
  ['C10', 'Live in place', ['live']],
  ['C11', 'Hierarchy and density', ['desktop-light', 'phone-light', 'docked']],
  ['C12', 'Links, ids and accessibility', ['desktop-light', 'phone-light', 'focus-light', 'focus-dark']],
];

/* Tab from the top of the page until focus lands inside the surface (at most 40 presses), and say where it is. */
async function tabInto(page, root) {
  for (var i = 1; i <= 40; i++) {
    await page.keyboard.press('Tab');
    var at = await page.evaluate(function (r) {
      var a = document.activeElement, box = document.querySelector(r);
      if (!a || a === document.body || !box || !box.contains(a)) return null;
      var cs = getComputedStyle(a);
      return { el: a.tagName.toLowerCase() + (a.id ? '#' + a.id : ''), name: (a.getAttribute('aria-label') || a.innerText || '').trim().slice(0, 60),
        focusVisible: a.matches(':focus-visible'), outline: cs.outlineStyle + ' ' + cs.outlineWidth + ' ' + cs.outlineColor, boxShadow: cs.boxShadow };
    }, root);
    if (at) return Object.assign({ presses: i }, at);
  }
  return { presses: 40, el: null, note: 'focus never reached ' + root };
}

/* With script off a page runs no promise or frame callback, so golden.settle (and any evaluate that awaits one) never
   returns; plain synchronous evaluates still work. Poll the font set until it has finished loading instead. */
function noScript(shot) { return !!shot.ctx && shot.ctx.javaScriptEnabled === false; }
async function settleNoScript(page) {
  for (var i = 0; i < 50 && await page.evaluate(function () { return document.fonts.status; }) !== 'loaded'; i++) await page.waitForTimeout(100);
  await page.waitForTimeout(250);
}

var MAX_SCROLLS = 8;   // scroll frames per cell, after the as-opened shot

async function shootCell(browser, golden, shot, cellName, cell, style) {
  var file = shot.name + '-' + cellName, rec = { cell: cellName, png: file + '.png' };
  var ctx = await browser.newContext(Object.assign({}, cell.ctx, shot.ctx, { colorScheme: cell.theme, reducedMotion: 'reduce', locale: 'en-US', timezoneId: 'UTC' }));
  // the sky (web/sky.js) is auto by default, night by the clock, and the pinned clock is 20:33 UTC: pin it to the cell's theme
  await ctx.addInitScript(function (s) { try { localStorage.setItem('garden.sky', s); } catch (e) { /* no storage: the emulated scheme still decides by day */ } }, cell.theme === 'dark' ? 'night' : 'day');
  var page = await ctx.newPage(), errors = [];
  page.on('pageerror', function (e) { errors.push(String(e)); });
  try {
    if (shot.init) await page.addInitScript(shot.init);   // as the golden harness does: before the page's own scripts (splashflat)
    await golden.pin(page, url, now, style);
    var res = await page.goto(url + shot.path);
    rec.status = res ? res.status() : 0;
    await shot.show(page, cell.as);
    if (cell.needs) await shot[cell.needs](page);
    if (noScript(shot)) await settleNoScript(page); else await golden.settle(page);
    var root = ROOTS[shot.name] || 'body';
    if (cell.focus) rec.focus = await tabInto(page, FOCUS_ROOTS[shot.name] || root);
    await page.screenshot({ path: path.join(out, rec.png), animations: 'disabled', caret: 'hide' });
    if (shot.clip && !cell.focus) {
      // the golden's frame of the same cell: a SHOTS row's clip(page) resolves to a viewport rect (paneClip, planClip)
      rec.clip = file + '-clip.png';
      await page.screenshot({ path: path.join(out, rec.clip), clip: await shot.clip(page), animations: 'disabled', caret: 'hide' });
    }
    if (!cell.focus) {
      // A pane that scrolls inside itself (the bead view's body) hides most of the surface below the first viewport, and a
      // full-page shot does not reach it: step each scroller in the root, largest first (a workbench transcript before its
      // tree), from its top down a page at a time, up to MAX_SCROLLS frames each, the last one at its bottom. The
      // as-opened shot stays frame 0, and a Commons timeline or thread opens scrolled to the bottom, so the top of the pane
      // (gd-k4DwTGdELq) gets a frame of its own. A step that would repeat the as-opened position is skipped, and each
      // scroller is put back where it opened before the next one and the probes run.
      var opened = await page.evaluate(function (r) {
        var box = document.querySelector(r), els = box ? [box].concat(Array.prototype.slice.call(box.querySelectorAll('*'))) : [];
        var scs = els.filter(function (e) { return /auto|scroll/.test(getComputedStyle(e).overflowY) && e.scrollHeight > e.clientHeight + 1 && e.clientHeight > 100; });
        scs.sort(function (x, y) { return y.clientWidth * y.clientHeight - x.clientWidth * x.clientHeight; });
        return scs.map(function (sc, k) {
          sc.setAttribute('data-cux-scroller', k);
          sc.style.scrollBehavior = 'auto';   // a smooth scroller would still be moving when the frame is taken
          return sc.scrollTop;
        });
      }, root);
      rec.scrolls = [];
      for (var k = 0; k < opened.length; k++) {
        for (var i = 0, n = 0, top = 0; n < MAX_SCROLLS && i < 2 * MAX_SCROLLS; i++) {
          // re-read the scroller each step: a timeline that loads older history at the top grows as it is stepped
          var at = await page.evaluate(function (a) {
            var sc = document.querySelector('[data-cux-scroller="' + a.k + '"]'), max = sc.scrollHeight - sc.clientHeight, step = sc.clientHeight * 0.9;
            var want = a.last ? max : Math.min(a.top, max);
            var at = { next: want + step, done: want >= max - 1, jumped: a.last && a.top < max - 1 };
            if (Math.abs(want - a.opened) <= 1) at.skip = true; else sc.scrollTop = want;
            return at;
          }, { k: k, top: top, opened: opened[k], last: n === MAX_SCROLLS - 1 });
          if (!at.skip) {
            if (noScript(shot)) await page.waitForTimeout(100);
            else await page.evaluate(function () { return new Promise(function (r) { requestAnimationFrame(function () { requestAnimationFrame(r); }); }); });
            var sf = file + '-scroll' + (rec.scrolls.length + 1) + '.png';
            await page.screenshot({ path: path.join(out, sf), animations: 'disabled', caret: 'hide' });
            rec.scrolls.push(sf);
            n++;
          }
          if (at.jumped) rec.scrollsCapped = true;   // the draft says the middle of the pane was not shot
          if (at.done) break;
          top = at.next;
        }
        await page.evaluate(function (a) { document.querySelector('[data-cux-scroller="' + a.k + '"]').scrollTop = a.o; }, { k: k, o: opened[k] });
      }
      rec.probes = await page.evaluate(probes.run, { root: root, coarse: !!cell.ctx.hasTouch });
      fs.writeFileSync(path.join(out, file + '.probes.json'), JSON.stringify(rec.probes, null, 2));
    }
    // last: a full-page shot drops the touch emulation, so after it (pointer: coarse) no longer matches and the scroll
    // frames and probes would grade a fine pointer (gd-v2VcASjBfl: false 23 px small targets on noscript landscape)
    var tall = await page.evaluate(function () { return document.scrollingElement.scrollHeight > innerHeight + 1; });
    if (tall) {
      rec.full = file + '-full.png';
      await page.screenshot({ path: path.join(out, rec.full), fullPage: true, animations: 'disabled', caret: 'hide' });
    }
  } catch (e) {
    rec.error = String(e.message || e).split('\n')[0];
    // where it stood when it gave up: a step that times out on a phone is often a control the layout hid
    rec.errorPng = file + '-error.png';
    await page.screenshot({ path: path.join(out, rec.errorPng), animations: 'disabled', caret: 'hide', timeout: 5000 }).catch(function () { delete rec.errorPng; });
  }
  try { if (shot.after) await shot.after(page); }   // a row that changed the farm (a transition's patch) puts it back for the next cell
  catch (e) { rec.error = rec.error || 'after: ' + String(e.message || e).split('\n')[0]; }
  rec.pageErrors = errors;
  await ctx.close();
  console.log((rec.error ? 'FAILED ' : 'shot ') + rec.png + (rec.error ? ': ' + rec.error : ''));
  return rec;
}

/* Perf (rubric §4's perf rows, §7's budgets; P3.2 gd-qeaDwFSDqq). A surface is measured on §7's profiles, not the shot cells
   (phone: 390x844, 4x CPU, 9 Mbps / 170 ms RTT; desktop: 1280x800), with perf_baseline.js's measure() and idle() and
   without the shots' pinned clock and fonts, and its numbers are filed under the cell of that profile: */
var PERF_CELLS = { phone: 'phone-light', desktop: 'desktop-light' };
/* panes opened by a query parameter, which perf_baseline.js's bead view act (a click on #bv-body) fits */
var PANES = ['bead', 'q', 'bundle', 'project', 'gardener', 'settings', 'costs', 'creds', 'transcript'];

/* A shot as a perf_baseline.js surface row: its own path, context and init, ready once show() is done, and the first
   interaction INP is read from. That act is the P0.1 row's for the same path, else that of the row of the same kind (a
   bead pane, the cockpit, the queues, the Glass, the World), else a j in the workbench's tree; an act that does
   not fit the surface leaves B6/B11 ungraded (actError in the perf file), never passed. gradedAs is the name perf_gate.js
   grades it under: 'world' for a World surface, so it is held to Ready and B7 as P0.1's world row is. */
function perfRow(shot) {
  var u = new URL(shot.path, 'http://farm'), q = u.searchParams, kind;
  var row = function (n) { return perf.SURFACES.filter(function (s) { return s.name === n; })[0]; };
  var same = perf.SURFACES.filter(function (s) { return s.path === shot.path; })[0], act;
  if (u.pathname.indexOf('/garden/c/n/') === 0 || /^\/garden\/c\/s(\/|$)/.test(u.pathname)) kind = 'workbench';
  else if (q.has('glass')) kind = q.get('glass') === 'wall' ? 'wall' : 'glass';
  else if (q.has('plan')) kind = 'plans';
  else if (q.has('queue')) kind = 'inbox';
  else if (PANES.some(function (k) { return q.has(k); })) kind = 'beads';
  else kind = 'world';
  if (same) act = same.act;
  else if (kind === 'workbench') act = async function (page) { await page.focus('#wb2 .wb2-tree'); await page.keyboard.press('j'); };
  else act = row(kind).act;
  var gradedAs = kind === 'world' ? 'world' : shot.name;
  // the Commons v2 latency pass (B12-B15) of the P0.1 row for the same path: its selectors name that row's channel
  var latency = same ? same.latency : undefined;
  // the Commons v2 workbench's (web/commons2.js): the one the CI gate's workbench rows run
  if (kind === 'workbench') latency = perf.WB_LATENCY;
  // the steward root (steward view S2): its conversation's threads and its composer, a reply streamed into the thread that asks it
  if (kind === 'workbench' && u.pathname.indexOf('/garden/c/s') === 0) latency = { section: '#wb2 .wb2-transcript', channel: 'gd-chGeneral', thread: 'gd-msgSw1', open: true, roots: perf.WB_ROOTS,
    ready: function () { return !!document.querySelector('#wb2 .wb2-swform .cm-input') && /channel=/.test(COMMONS2.state().stream); } };
  return { kind: kind, gradedAs: gradedAs, s: { name: gradedAs, label: shot.name, path: shot.path, ctx: shot.ctx, init: shot.init, act: act, latency: latency,
    ready: async function (page, profile) {
      await shot.show(page, profile);
      if (shot.after) await shot.after(page);   // a row that changed the farm puts it back before the next load
    } } };
}

/* Measure one shot on each profile in want (median of PERF_RUNS cold and warm loads, then an idle pass of PERF_IDLE s)
   and grade it at the budgets (perf_gate.js verdicts()). The bench is read before and after, as perf_baseline.js does. */
async function perfOf(browser, shot, want) {
  var r = perfRow(shot), rows = [], b0 = await perf.bench(browser);
  for (var profile of want) {
    for (var run = 1; run <= PERF_RUNS; run++) rows = rows.concat(await perf.measure(browser, r.s, profile, run));
    if (PERF_IDLE > 0) rows = rows.concat(await perf.idle(browser, [r.s], profile));
  }
  var head = { meta: { target: 'fixture', chromium: browser.version(), at: new Date().toISOString(), runs: PERF_RUNS, idle: PERF_IDLE, bench: [b0, await perf.bench(browser)] }, rows: rows };
  return { kind: r.kind, gradedAs: r.gradedAs, head: head, v: perfGate.verdicts(head), profiles: want };
}

/* The perf file of one cell: its profile's checks with their verdicts and the runs they were read from. */
function perfFile(shot, cell, profile, p) {
  return { surface: shot.name, cell: cell, profile: profile, gradedAs: p.gradedAs, kind: p.kind, meta: p.head.meta, bench: p.v.bench,
    budgets: p.v.budgets, checks: p.v.checks.filter(function (c) { return c.profile === profile; }),
    rows: p.head.rows.filter(function (x) { return x.profile === profile; }) };
}

/* The draft's perf rows: one per budget check (id, sub-check and load), its budget, and its value and verdict in the phone
   and desktop cells ("—" where the budget does not apply to that profile). */
function perfDraft(shot, p) {
  var lines = ['## Performance budgets (rubric §7)', ''];
  if (!p) {
    lines.push('Not measured on this run (' + (PERF ? 'no phone-light or desktop-light cell was shot' : 'COMMONS_UX_PERF off: a full-matrix run, or PERF=0') +
      '): every budget is ungraded. `make commons-ux-shoot ... SURFACES=' + shot.name + '` measures it.', '');
    return { lines: lines, line: 'Perf: not measured (every budget ungraded)' };
  }
  var fmt = perfGate.fmt, b = p.v.bench, rows = [], key = function (c) { return c.id + '|' + c.sub + '|' + c.kind; };
  p.v.checks.forEach(function (c) { if (rows.indexOf(key(c)) < 0) rows.push(key(c)); });
  lines.push('Beside the C1–C12 score, never in it. Measured on §7\'s profiles, not the shot cells (phone: 390x844, 4x CPU, ' +
    '9 Mbps / 170 ms RTT; desktop: 1280x800), medians of ' + p.head.meta.runs + ' run(s), idle pass ' + p.head.meta.idle + ' s' +
    (p.gradedAs === 'world' ? ', graded as the World (Ready and B7 apply)' : '') + '. Verdicts are at the budget itself. ' +
    'CPU bench ' + fmt(b.head, 'ms') + ' ms against the reference ' + b.reference + ' ms: ' +
    (b.near ? 'times read as measured.' : '**not within 20 %**, so a time FAIL (B4–B6, B10–B15, B17) is marked noisy: re-measure on a quiet node before filing it.'), '');
  lines.push('| Budget | Limit | Load | ' + p.profiles.map(function (pr) { return PERF_CELLS[pr]; }).join(' | ') + ' |', '|---|---|---|' + p.profiles.map(function () { return '---'; }).join('|') + '|');
  rows.forEach(function (k) {
    var all = p.v.checks.filter(function (c) { return key(c) === k; }), c0 = all[0];
    lines.push('| ' + c0.id + ' ' + c0.label + ' | ≤ ' + fmt(c0.limit, c0.unit) + ' ' + c0.unit + ' | ' + c0.kind + ' | ' +
      p.profiles.map(function (pr) {
        var c = all.filter(function (x) { return x.profile === pr; })[0];
        if (!c) return '—';
        if (c.verdict === 'UNGRADED') return 'UNGRADED: ' + c.why;
        return fmt(c.head, c.unit) + ' ' + c.unit + ' ' + (c.verdict === 'FAIL' ? '**FAIL**' : 'PASS') +
          (c.noisy ? ' (noisy' + (c.headBest !== null && c.headBest !== c.head ? ': fastest ' + fmt(c.headBest, c.unit) : '') + ')' : '');
      }).join(' | ') + ' |');
  });
  var probs = p.head.rows.filter(function (r) { return r.error || r.actError; });
  if (probs.length) lines.push('', 'Perf problems: ' + probs.map(function (r) { return r.profile + ' ' + r.kind + ' run ' + r.run + ': ' + (r.error || 'first interaction: ' + r.actError); }).join('; '));
  lines.push('', 'Files: ' + p.profiles.map(function (pr) { return '`' + shot.name + '-' + PERF_CELLS[pr] + '.perf.json`'; }).join(', '), '');
  var of = p.v.pass.length + p.v.fail.length;
  return { lines: lines, line: 'Perf: ' + p.v.pass.length + '/' + of + ' budgets PASS' + (p.v.fail.length ? ' · FAIL: ' + p.v.fail.join(' ') : '') +
    (p.v.ungraded.length ? ' · ungraded: ' + p.v.ungraded.join(' ') : '') + '   (rubric §4: beside the score, not in it)' };
}

function sha(file) { try { return crypto.createHash('sha256').update(fs.readFileSync(path.join(out, file))).digest('hex'); } catch (e) { return ''; } }

/* The draft grade for one surface (rubric §4 shape): every gate with the cells it needs, what was shot, and the probe hits
   that are candidate FAILs. Verdicts are left as "?" on purpose: they come from reading the PNGs, not from this file. */
function draft(shot, recs, perfRes) {
  var by = {};
  recs.forEach(function (r) { by[r.cell] = r; });
  var lines = ['# Commons UX grade — ' + shot.name + ' — ' + new Date().toISOString().slice(0, 10), '',
    'SUT: `' + shot.path + '` on the golden fixture farm (cardWorld + goldenPorch, clock frozen at ' + new Date(now).toISOString() + ')' +
    '   probe root: `' + (ROOTS[shot.name] || 'body') + '`   card theme: none', '',
    'Captured cells: ' + recs.filter(function (r) { return !r.error && !r.skipped; }).map(function (r) { return r.cell; }).join(', '),
    'Not captured: ' + (recs.filter(function (r) { return r.error || r.skipped; }).map(function (r) { return r.cell + ' (' + (r.error || r.skipped) + ')'; }).join(', ') || 'none') + '; live (no SSE driver yet)', ''];
  var light = by['desktop-light'], dark = by['desktop-dark'];
  if (light && dark && !light.error && !dark.error && sha(light.png) === sha(dark.png)) lines.push('**Dark emulation did not apply** (desktop-dark is byte-identical to desktop-light): C5 is ungraded.', '');
  lines.push('| Gate | Verdict | Cell(s) | Evidence | Probe candidates |', '|---|---|---|---|---|');
  GATES.forEach(function (g) {
    var shotCells = g[2].filter(function (c) { return by[c] && !by[c].error && !by[c].skipped; });
    var cand = [];
    shotCells.forEach(function (c) {
      var p = by[c].probes;
      if (!p) return;
      Object.keys(p.hits).forEach(function (k) { if (p.hits[k].n && p.hits[k].gates.indexOf(g[0]) >= 0) cand.push(k + ' ×' + p.hits[k].n + ' @' + c); });
    });
    lines.push('| ' + g[0] + ' ' + g[1] + ' | ? | ' + (shotCells.join(', ') || '—') + ' | ' +
      shotCells.map(function (c) { return [by[c].png].concat(by[c].clip ? [by[c].clip] : [], by[c].full ? [by[c].full] : [], by[c].scrolls || []).join(' '); }).join(' ') + ' | ' + (cand.join('; ') || 'clean') + ' |');
  });
  var pd = perfDraft(shot, perfRes);
  lines.push('', 'Score = PASS ÷ (12 − N/A) = ?   Band: ?', pd.line, '');
  lines.push.apply(lines, pd.lines);
  lines.push('## Probe hits', '');
  var PAGE = ['untokenedColour', 'hoverOnly', 'duplicateH1'], seenPage = false;   // stylesheet and page scans: same in every cell
  recs.forEach(function (r) {
    if (r.focus) lines.push('- `' + r.cell + '`: focus after ' + r.focus.presses + ' Tab(s) on `' + r.focus.el + '` "' + (r.focus.name || '') + '" — :focus-visible ' + r.focus.focusVisible + ', outline `' + r.focus.outline + '`');
    if (r.scrollsCapped) lines.push('- `' + r.cell + '`: a pane is taller than ' + MAX_SCROLLS + ' scroll frames: its last frame jumps to its bottom, the rows above it are not shot');
    if (r.pageErrors && r.pageErrors.length) lines.push('- `' + r.cell + '`: page errors: ' + r.pageErrors.join(' | '));
    if (!r.probes) return;
    if (r.probes.error) { lines.push('- `' + r.cell + '`: ' + r.probes.error); return; }
    Object.keys(r.probes.hits).forEach(function (k) {
      var h = r.probes.hits[k];
      if (!h.n || (seenPage && PAGE.indexOf(k) >= 0)) return;
      lines.push('- `' + r.cell + '` **' + k + '** (' + h.gates.join(', ') + ') ×' + h.n + ': ' +
        h.items.slice(0, 5).map(function (i) { return '`' + (i.sel || i.text) + '` ' + (i.sel && i.text ? '"' + i.text + '" ' : '') + i.value; }).join('; ') + (h.n > 5 ? '; …' : ''));
    });
    seenPage = true;
    if (Object.keys(r.probes.notRun).length) lines.push('- `' + r.cell + '` probes not run: ' + JSON.stringify(r.probes.notRun));
  });
  lines.push('', '## FAILs', '', '(one paragraph each: mechanism → where in the code → the fix; link the PNG)', '', '## N/A', '', '(each with its reason)', '');
  return lines.join('\n');
}

(async function () {
  assert.ok(url && mods && out && goldenJs && fonts && now > 0, 'usage: node shoot.js <base url> <node_modules> <out dir> <golden_browser.js> <fonts dir> <now ms>');
  var pw = require(require.resolve('playwright-core', { paths: [mods] }));
  var all = golden.SHOTS.concat(EXTRA), matrix = Object.assign(cells(pw), sweepCells(pw));
  var want = (process.env.COMMONS_UX_SURFACES || '').split(',').filter(Boolean);
  var wantCells = [].concat.apply([], (process.env.COMMONS_UX_CELLS || '').split(',').filter(Boolean).map(function (c) {
    return c === 'sweep' ? Object.keys(matrix).filter(function (k) { return matrix[k].sweep; }) : [c];
  }));
  var unknown = want.filter(function (w) { return !all.some(function (s) { return s.name === w; }); }).concat(wantCells.filter(function (c) { return !matrix[c]; }));
  assert.deepStrictEqual(unknown, [], 'unknown surface or cell; surfaces: ' + all.map(function (s) { return s.name; }).join(',') + '; cells: ' + Object.keys(matrix).join(','));
  var shots = all.filter(function (s) { return !want.length || want.indexOf(s.name) >= 0; });
  fs.mkdirSync(out, { recursive: true });
  if (PERF) perf.use({ base: env.COMMONS_UX_PERF_URL || url, pw: pw, runs: PERF_RUNS, idle: PERF_IDLE, settle: 0, latency: true });
  var browser;
  try { browser = await pw.chromium.launch(); }
  catch (e) { console.log('SKIP: ' + String(e.message || e).split('\n')[0]); process.exit(3); }
  var manifest = { url: url, now: now, surfaces: {} };
  try {
    var style = golden.fontStyle(fonts);
    for (var shot of shots) {
      var recs = [];
      for (var name of Object.keys(matrix)) {
        if (wantCells.length ? wantCells.indexOf(name) < 0 : matrix[name].sweep) continue;
        if (shot.devices && shot.devices.indexOf(matrix[name].as) < 0) continue;   // a surface that exists at one width only
        var cell = matrix[name];
        if (cell.as === 'desktop' && shot.devices && shot.devices.indexOf('desktop') < 0) { recs.push({ cell: name, skipped: 'surface does not exist at desktop width' }); continue; }
        if (cell.needs && !shot[cell.needs]) { recs.push({ cell: name, skipped: 'surface has no ' + cell.needs + ' step (no docked pane)' }); continue; }
        recs.push(await shootCell(browser, golden, shot, name, cell, style));
      }
      manifest.surfaces[shot.name] = recs;
      var perfRes = null, want = Object.keys(PERF_CELLS).filter(function (pr) {
        return recs.some(function (r) { return r.cell === PERF_CELLS[pr] && !r.skipped; }) || (shot.perfOn || []).indexOf(pr) >= 0;
      });   // perfOn: profiles a surface is measured on though it is not shot there (the workbench's phone, before step 7)
      if (PERF && want.length && !noScript(shot)) {
        // a no-script page runs none of the observers perf_baseline.js reads, so it is not measured (every budget ungraded)
        perfRes = await perfOf(browser, shot, want);
        want.forEach(function (pr) {
          var rec = recs.filter(function (r) { return r.cell === PERF_CELLS[pr]; })[0];
          if (!rec) recs.push(rec = { cell: PERF_CELLS[pr], skipped: 'measured only: not shot at this width' });
          rec.perf = shot.name + '-' + PERF_CELLS[pr] + '.perf.json';
          fs.writeFileSync(path.join(out, rec.perf), JSON.stringify(perfFile(shot, PERF_CELLS[pr], pr, perfRes), null, 1));
        });
        (manifest.perf = manifest.perf || {})[shot.name] = { gradedAs: perfRes.gradedAs, budgets: perfRes.v.budgets, bench: perfRes.v.bench };
        console.log('perf ' + shot.name + ': PASS ' + perfRes.v.pass.join(' ') + ' | FAIL ' + perfRes.v.fail.join(' ') + ' | ungraded ' + perfRes.v.ungraded.join(' '));
      }
      fs.writeFileSync(path.join(out, 'COMMONS-UX-GRADE.' + shot.name + '.draft.md'), draft(shot, recs, perfRes));
    }
  } finally { await browser.close(); }
  fs.writeFileSync(path.join(out, 'manifest.json'), JSON.stringify(manifest, null, 2));
  var failed = [].concat.apply([], Object.keys(manifest.surfaces).map(function (k) { return manifest.surfaces[k].filter(function (r) { return r.error; }); }));
  console.log('commons-ux shots in ' + out + (failed.length ? ' (' + failed.length + ' cell(s) failed to shoot)' : ''));
  process.exit(failed.length ? 1 : 0);
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });
