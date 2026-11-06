/* perf_gate.js's self-test (P3.1 gd-7Ns0SDsF7R): its budgets are the rubric's, and it fails what it should and only that.
   node --test gardenio/controller/internal/gardenhub/testdata/perf_gate.test.js (`make test-lands-perf-gate`). */
'use strict';
var test = require('node:test'), assert = require('node:assert'), fs = require('fs'), path = require('path'), os = require('os');
var pg = require('./perf_gate.js');

var KB = 1024, MB = 1048576;
var RUBRIC = path.join(__dirname, '..', '..', '..', '..', 'docs', 'commons-ux-rubric.md');

/* The rubric's section 7 budget table, by id: { text, cls }. */
function rubricRows() {
  var md = fs.readFileSync(RUBRIC, 'utf8'), sec = md.slice(md.indexOf('## 7. Performance budgets'));
  sec = sec.slice(0, sec.indexOf('\n## ', 5) > 0 ? sec.indexOf('\n## ', 5) : undefined);
  var rows = {};
  sec.split('\n').forEach(function (l) {
    var m = /^\| (B\d+) \|(.*)\|$/.exec(l);
    if (!m) return;
    var cells = m[2].split('|').map(function (c) { return c.trim(); });
    if (cells.length !== 5) return;   // the "where each surface stands" table has no Class column
    rows[m[1]] = { text: cells[0].replace(/\*\*/g, ''), cls: cells[4] };
  });
  return rows;
}
function limitOf(text) {
  var m = /([\d,.]+)\s*(KB|MB|ms|s|requests)\b/.exec(text);
  if (!m) return null;
  var n = +m[1].replace(/,/g, '');
  return { KB: n * KB, MB: n * MB, ms: n, s: n * 1000, requests: n }[m[2]];
}

test('the budgets are the rubric section 7 table, id for id', function () {
  var rows = rubricRows(), ids = Object.keys(rows);
  assert.deepStrictEqual(ids, ['B1', 'B2', 'B3', 'B4', 'B5', 'B6', 'B7', 'B8', 'B9', 'B10', 'B11', 'B12', 'B13', 'B14', 'B15', 'B16', 'B17']);
  ids.forEach(function (id) { assert.ok(pg.BUDGETS.some(function (b) { return b.id === id; }), id + ' has no check in perf_gate.js'); });
  pg.BUDGETS.forEach(function (b) {
    var r = rows[b.id];
    assert.ok(r, b.id + ' is not in the rubric');
    assert.strictEqual(b.cls, r.cls, b.id + ': class');
    assert.ok(r.text.indexOf(b.rubric) >= 0, b.id + ': the rubric row says "' + r.text + '", not "' + b.rubric + '"');
    var want = limitOf(b.rubric);
    if (want !== null && b.limit !== 0) assert.strictEqual(b.limit, want, b.id + ' ' + (b.sub || '') + ': limit');
  });
});

/* A results.json with three runs of each named load, each read at bench, the run's (as perf_baseline.js reads one next to
   every cold phone load; a load's own bench, when it has one, wins). */
function load(surface, profile, kind, v) {
  return Object.assign({ surface: surface, profile: profile, kind: kind, bytes: { wire: 100 * KB, requests: 5 }, lcp: 1000, ready: 1000,
    longTasks: { total: 500 }, inp: 40 }, v || {});
}
function results(bench, loads) {
  var rows = [];
  loads.forEach(function (l) {
    if (l.kind === 'idle') { rows.push(l); return; }
    for (var run = 1; run <= 3; run++) rows.push(Object.assign({ run: run, bench: [bench, bench] }, l, l.each ? l.each[run - 1] : {}));
  });
  return { meta: { runs: 3, idle: 60, chromium: 'test', bench: [bench, bench] }, rows: rows };
}
function idle(surface, profile, v) {
  return Object.assign({ surface: surface, profile: profile, kind: 'idle', idleS: 60, idleBytes: 1 * KB, heapLoaded: 4 * MB, heapIdleGC: 3.5 * MB }, v || {});
}
function check(res, id, surface, profile, kind, sub) {
  return res.checks.find(function (c) { return c.id === id && c.surface === surface && c.profile === profile && c.kind === kind && c.sub === (sub || ''); });
}
var REF = pg.REFERENCE_BENCH, FAR = Math.round(REF * 1.6);

test('within budget passes', function () {
  var both = results(REF, [load('world', 'phone', 'cold'), load('world', 'phone', 'warm', { bytes: { wire: 2 * KB, requests: 4 } }),
    load('world', 'desktop', 'cold'), load('world', 'desktop', 'warm'), idle('world', 'phone'), idle('world', 'desktop')]);
  var res = pg.gate(both, both);
  assert.strictEqual(res.failed, 0);
  assert.ok(res.checks.every(function (c) { return c.status === 'ok' || c.id === 'B7'; }), JSON.stringify(res.checks.filter(function (c) { return c.status !== 'ok'; })));
  var b7 = check(res, 'B7', 'world', 'phone', 'pan');
  assert.strictEqual(b7.status, 'ungraded');   // the World ran on the phone, no pan pass on either side
  assert.match(b7.why, /no pan pass/);
  assert.strictEqual(check(res, 'B7', 'world', 'phone', 'pan', 'load'), undefined);
});

test('stable: over budget fails when the base was within it, and is a ratchet when the base was over too', function () {
  var base = results(REF, [load('world', 'phone', 'cold', { bytes: { wire: 456 * KB, requests: 4 } }), load('beads', 'phone', 'cold')]);
  var same = results(REF, [load('world', 'phone', 'cold', { bytes: { wire: 457 * KB, requests: 4 } }), load('beads', 'phone', 'cold', { bytes: { wire: 301 * KB, requests: 5 } })]);
  var res = pg.gate(same, base);
  assert.strictEqual(check(res, 'B1', 'world', 'phone', 'cold').status, 'over');   // +1 KB on 456: within 2 %
  assert.strictEqual(check(res, 'B1', 'beads', 'phone', 'cold').status, 'fail');   // newly over
  var worse = results(REF, [load('world', 'phone', 'cold', { bytes: { wire: 480 * KB, requests: 4 } })]);
  assert.strictEqual(check(pg.gate(worse, base), 'B1', 'world', 'phone', 'cold').status, 'fail');
  var better = results(REF, [load('world', 'phone', 'cold', { bytes: { wire: 350 * KB, requests: 4 } })]);
  assert.strictEqual(check(pg.gate(better, base), 'B1', 'world', 'phone', 'cold').status, 'over');
});

test('stable: one request more over B3 fails, a new surface is held to the budget', function () {
  var base = results(REF, [load('glass', 'phone', 'cold', { bytes: { wire: 100 * KB, requests: 21 } })]);
  var head = results(REF, [load('glass', 'phone', 'cold', { bytes: { wire: 100 * KB, requests: 22 } }), load('newone', 'phone', 'cold', { bytes: { wire: 310 * KB, requests: 3 } })]);
  var res = pg.gate(head, base);
  assert.strictEqual(check(res, 'B3', 'glass', 'phone', 'cold').status, 'fail');
  assert.strictEqual(check(res, 'B1', 'newone', 'phone', 'cold').status, 'fail');
  assert.strictEqual(check(res, 'B1', 'glass', 'phone', 'cold', 'terminal').status, 'ok');
});

test('B1: the Glass watch is held to 300 KB plus its terminal, 367 KB, and only it (gd-T8Lrqgp23V)', function () {
  var head = results(REF, [load('glass', 'phone', 'cold', { bytes: { wire: 360 * KB, requests: 17 } }), load('wall', 'phone', 'cold', { bytes: { wire: 310 * KB, requests: 13 } })]);
  var res = pg.gate(head, results(REF, []));
  assert.strictEqual(check(res, 'B1', 'glass', 'phone', 'cold'), undefined, 'not graded at 300 KB');
  assert.strictEqual(check(res, 'B1', 'glass', 'phone', 'cold', 'terminal').status, 'ok');
  assert.strictEqual(check(res, 'B1', 'wall', 'phone', 'cold').status, 'fail', 'the Wall draws no terminal');
  assert.strictEqual(check(res, 'B1', 'wall', 'phone', 'cold', 'terminal'), undefined);
  var heavy = results(REF, [load('glass', 'phone', 'cold', { bytes: { wire: 370 * KB, requests: 17 } })]);
  assert.strictEqual(check(pg.gate(heavy, results(REF, [])), 'B1', 'glass', 'phone', 'cold', 'terminal').status, 'fail');
});

test('CPU: graded at 1.5x off the reference bench, at the budget near it, never on noise alone', function () {
  var base = results(FAR, [load('beads', 'phone', 'cold', { longTasks: { total: 1400 } }), load('plans', 'phone', 'cold', { longTasks: { total: 2600 } })]);
  var head = results(FAR, [load('beads', 'phone', 'cold', { longTasks: { total: 2000 } }), load('plans', 'phone', 'cold', { longTasks: { total: 2700 } })]);
  var res = pg.gate(head, base);
  assert.strictEqual(res.bench.near, false);
  assert.strictEqual(check(res, 'B5', 'beads', 'phone', 'cold').status, 'noisy');   // 2000: over 1500, under 2250
  assert.strictEqual(check(res, 'B5', 'plans', 'phone', 'cold').status, 'over');    // over 2250, as on the base
  var bad = results(FAR, [load('beads', 'phone', 'cold', { longTasks: { total: 2400 } })]);
  assert.strictEqual(check(pg.gate(bad, base), 'B5', 'beads', 'phone', 'cold').status, 'fail');
  // near the reference the line is the budget, but a 1490 -> 1520 flip is within the noise margin of the base
  var nb = results(REF, [load('wall', 'phone', 'cold', { longTasks: { total: 1490 } })]);
  var nh = results(REF, [load('wall', 'phone', 'cold', { longTasks: { total: 1520 } })]);
  var r2 = pg.gate(nh, nb);
  assert.strictEqual(r2.bench.near, true);
  assert.strictEqual(check(r2, 'B5', 'wall', 'phone', 'cold').status, 'noisy');
  var nf = results(REF, [load('wall', 'phone', 'cold', { longTasks: { total: 1900 } })]);
  assert.strictEqual(check(pg.gate(nf, nb), 'B5', 'wall', 'phone', 'cold').status, 'fail');
  // a slower head node: the base is scaled by the benches before the margin
  // a slower head node: the base is scaled by the benches before the margin (1900 x 1.3 = 2470 on the head's node)
  var sb = results(FAR, [load('beads', 'phone', 'cold', { longTasks: { total: 1900 } })]);
  var slow = results(Math.round(FAR * 1.3), [load('beads', 'phone', 'cold', { longTasks: { total: 2400 } })]);
  assert.strictEqual(check(pg.gate(slow, sb), 'B5', 'beads', 'phone', 'cold').status, 'noisy');
  var same = results(FAR, [load('beads', 'phone', 'cold', { longTasks: { total: 2400 } })]);
  assert.strictEqual(check(pg.gate(same, sb), 'B5', 'beads', 'phone', 'cold').status, 'fail');   // same node: 2400 > 1900 x 1.2

});

test('times: a busy node in some runs is noisy, a regression in every run fails', function () {
  // CI run 40824057236, identical code on both sides: the World's desktop cold INP
  var base = results(REF, [load('world', 'desktop', 'cold', { each: [{ inp: 40 }, { inp: 48 }, { inp: 40 }] })]);
  var busy = results(REF, [load('world', 'desktop', 'cold', { each: [{ inp: 200 }, { inp: 48 }, { inp: 232 }] })]);
  var c = check(pg.gate(busy, base), 'B11', 'world', 'desktop', 'cold');
  assert.strictEqual(c.status, 'noisy');
  assert.strictEqual(c.head, 200);
  assert.strictEqual(c.headBest, 48);
  var slow = results(REF, [load('world', 'desktop', 'cold', { each: [{ inp: 180 }, { inp: 150 }, { inp: 210 }] })]);
  assert.strictEqual(check(pg.gate(slow, base), 'B11', 'world', 'desktop', 'cold').status, 'fail');
  // bytes have no fastest run: the median decides
  var heavy = results(REF, [load('world', 'phone', 'cold', { each: [{ bytes: { wire: 310 * KB, requests: 4 } }, { bytes: { wire: 100 * KB, requests: 4 } }, { bytes: { wire: 320 * KB, requests: 4 } }] })]);
  assert.strictEqual(check(pg.gate(heavy, results(REF, [load('world', 'phone', 'cold')])), 'B1', 'world', 'phone', 'cold').status, 'fail');
});

test('ungraded: a surface the PR broke fails, one neither side measured is reported, the World is held to Ready', function () {
  var base = results(REF, [load('thread', 'phone', 'cold'), load('world', 'phone', 'cold', { ready: 2600 })]);
  var head = results(REF, [load('thread', 'phone', 'cold', { inp: null, actError: 'timeout' }), load('world', 'phone', 'cold', { ready: 4700 })]);
  head.rows.push({ surface: 'glass', profile: 'phone', kind: 'cold', run: 1, error: 'boom' });
  var res = pg.gate(head, base);
  assert.strictEqual(check(res, 'B6', 'thread', 'phone', 'cold').status, 'fail');
  assert.strictEqual(check(res, 'B6', 'glass', 'phone', 'cold').status, 'ungraded');
  assert.strictEqual(check(res, 'B4', 'world', 'phone', 'cold', 'ready').status, 'fail');
  assert.strictEqual(check(res, 'B4', 'thread', 'phone', 'cold', 'ready'), undefined);
});

test('idle: B8 per minute and B9 growth and cap, on both profiles', function () {
  var base = results(REF, [idle('world', 'phone'), idle('world', 'desktop')]);
  var head = results(REF, [idle('world', 'phone', { idleBytes: 12 * KB }), idle('world', 'desktop', { heapLoaded: 4 * MB, heapIdleGC: 11 * MB })]);
  var res = pg.gate(head, base);
  assert.strictEqual(check(res, 'B8', 'world', 'phone', 'idle').status, 'fail');   // 12 KB/min
  assert.strictEqual(check(res, 'B9', 'world', 'desktop', 'idle', 'growth').status, 'fail');
  assert.strictEqual(check(res, 'B9', 'world', 'desktop', 'idle').status, 'fail');
  assert.strictEqual(check(res, 'B9', 'world', 'phone', 'idle').status, 'ok');
});

test('run writes gate.md and gate.json, fails with no head results, and the comment names every failure', function () {
  var dir = fs.mkdtempSync(path.join(os.tmpdir(), 'perf-gate-'));
  var base = results(REF, [load('world', 'phone', 'cold')]), head = results(REF, [load('world', 'phone', 'cold', { bytes: { wire: 320 * KB, requests: 4 } })]);
  fs.writeFileSync(path.join(dir, 'h.json'), JSON.stringify(head));
  fs.writeFileSync(path.join(dir, 'b.json'), JSON.stringify(base));
  var r = pg.run(path.join(dir, 'h.json'), path.join(dir, 'b.json'), path.join(dir, 'out'), 'https://example.invalid/run');
  assert.strictEqual(r.failed, 1);
  assert.match(r.md, /^<!-- lands-perf-gate -->/);
  assert.match(r.md, /\*\*B1\*\* world phone cold: 320 KB against 300, base 100 \(\+220\): over budget, the base was within it\./);
  assert.match(fs.readFileSync(path.join(dir, 'out', 'gate.md'), 'utf8'), /❌ 1 budget check regressed/);
  assert.strictEqual(JSON.parse(fs.readFileSync(path.join(dir, 'out', 'gate.json'), 'utf8')).failed, 1);
  assert.doesNotMatch(r.md, /ran on different nodes/);
  var slow = { meta: { runs: 3, idle: 60, bench: [REF * 2] }, rows: head.rows };
  assert.match(pg.render(pg.gate(slow, base), slow), /ran on different nodes \(CPU bench 122 and 61 ms\)/);
  var none = pg.run(path.join(dir, 'missing.json'), path.join(dir, 'b.json'), null, '');
  assert.strictEqual(none.failed, 1);
  assert.match(none.md, /no results for this PR/);
  var noBase = pg.run(path.join(dir, 'h.json'), null, null, '');
  assert.strictEqual(noBase.failed, 1);
  assert.match(noBase.md, /No base results/);
});

test('verdicts: a UX grade reads each budget at the budget itself, with no base and no 1.5x line', function () {
  // the P0.1 bead view on a far bench: B1 and B5 over, and the desktop's LCP read slow by the busy node; no idle pass
  var head = results(FAR, [load('bead', 'phone', 'cold', { bytes: { wire: 457 * KB, requests: 5 }, longTasks: { total: 2000 } }),
    load('bead', 'phone', 'warm', { bytes: { wire: 2.5 * KB, requests: 5 } }), load('bead', 'desktop', 'cold', { lcp: 2400, inp: 24 }),
    load('bead', 'desktop', 'warm', { inp: 24 })]);
  var v = pg.verdicts(head);
  assert.deepStrictEqual(v.fail, ['B1', 'B5', 'B10']);
  assert.deepStrictEqual(v.pass, ['B2', 'B3', 'B4', 'B6', 'B11']);
  assert.strictEqual(v.checks.find(function (c) { return c.id === 'B10'; }).noisy, true);   // a stable-class time, but the bench is far
  assert.deepStrictEqual(v.ungraded, []);   // no idle rows and not the World: B7, B8 and B9 are not checks at all
  var b5 = v.checks.find(function (c) { return c.id === 'B5'; });
  assert.strictEqual(b5.verdict, 'FAIL');   // 2000 ms is under the gate's 1.5x line (noisy there), over the budget here
  assert.strictEqual(b5.noisy, true);       // and the bench is far from the reference
  assert.strictEqual(v.checks.find(function (c) { return c.id === 'B1'; }).noisy, false);   // bytes are never noisy
  // the World: Ready is a check, B7 is ungraded (no pan pass in a UX grade), an idle pass grades B8 and B9; one ungraded check holds a budget back
  var world = results(REF, [load('world', 'phone', 'cold', { ready: 2600, each: [{ inp: 220 }, { inp: 150 }, { inp: 230 }] }),
    load('world', 'phone', 'warm', { inp: null, actError: 'timeout' }), idle('world', 'phone', { idleBytes: 12 * KB })]);
  var w = pg.verdicts(world);
  assert.strictEqual(w.budgets.B7, 'UNGRADED');
  assert.strictEqual(w.budgets.B8, 'FAIL');
  assert.strictEqual(w.budgets.B9, 'PASS');
  assert.strictEqual(w.budgets.B6, 'FAIL');   // cold median 220 over 200, though the warm load is ungraded
  var inp = w.checks.find(function (c) { return c.id === 'B6' && c.kind === 'cold'; });
  assert.strictEqual(inp.noisy, true);         // its fastest run (150 ms) is within the budget
  assert.ok(w.checks.some(function (c) { return c.id === 'B4' && c.sub === 'ready' && c.verdict === 'PASS'; }));
  var near = pg.verdicts(results(REF, [load('world', 'desktop', 'cold', { lcp: 1700 })]));
  assert.strictEqual(near.checks.find(function (c) { return c.id === 'B10' && !c.sub; }).noisy, false);   // near the reference, every run over
  var ok = pg.verdicts(results(REF, [load('world', 'phone', 'cold'), load('world', 'phone', 'warm', { bytes: { wire: 2 * KB, requests: 4 } })]));
  assert.strictEqual(ok.budgets.B6, 'PASS');
  assert.strictEqual(ok.budgets.B7, 'UNGRADED');
});

/* A Commons cold load's latency pass (perf_baseline.js latencyPass), every number within B12-B15 unless v says otherwise. */
function latency(v) {
  var o = function (c, w) { return { cold: c, warm: w }; };
  return Object.assign({ keyPaint: { n: 7, median: 9, max: 14 }, rows: 0, echo: 30, open: { thread: o(200, 60), plan: o(250, 80), epic: o(220, 70) }, stream: 40, errors: [] }, v || {});
}

test('Commons v2 (B12-B17): graded on the workbench surfaces only, in a UX grade and in the CI gate', function () {
  // the band as the step-1 baseline read it (a round trip per send and per open, a refetch per streamed message), on a
  // workbench surface, beside a workbench that meets every budget and a non-Commons surface
  var band = latency({ keyPaint: { n: 7, median: 12, max: 30 }, rows: 2, echo: 420, open: { thread: { cold: 900, warm: 700 }, plan: { cold: 800, warm: 500 }, epic: { cold: 760, warm: 480 } }, stream: 380 });
  var head = results(REF, [load('commonsplan', 'phone', 'cold', { bytes: { wire: 470 * KB, requests: 12 }, lcp: 3400, latency: band }),
    load('commonsthread', 'phone', 'cold', { bytes: { wire: 140 * KB, requests: 12 }, lcp: 1400, latency: latency() }),
    load('beads', 'phone', 'cold', { bytes: { wire: 140 * KB, requests: 5 }, lcp: 1400 })]);
  var v = pg.verdicts(head);
  ['B12', 'B13', 'B14', 'B15', 'B16', 'B17'].forEach(function (id) { assert.strictEqual(v.budgets[id], 'FAIL', id); });
  var c = function (id, surface, sub) { return v.checks.find(function (x) { return x.id === id && x.surface === surface && x.sub === (sub || ''); }); };
  assert.strictEqual(c('B12', 'commonsplan').verdict, 'PASS');            // the median key paints in 12 ms
  assert.strictEqual(c('B12', 'commonsplan', 'rows').verdict, 'FAIL');    // but repaints two rows
  assert.strictEqual(c('B12', 'commonsplan', 'rows').noisy, false);       // a count is never noisy
  assert.strictEqual(c('B14', 'commonsplan', 'plan warm').head, 500);
  ['B12', 'B13', 'B15', 'B16', 'B17'].forEach(function (id) { assert.strictEqual(c(id, 'commonsthread').verdict, 'PASS', id); });
  assert.strictEqual(c('B14', 'commonsthread', 'epic warm').verdict, 'PASS');   // every root's open is read on every workbench root
  assert.ok(!v.checks.some(function (x) { return x.surface === 'beads' && +x.id.slice(1) >= 12; }), 'B12-B17 apply to the Commons only');
  // the CI gate grades them too: every one is a check, six of them B14's per surface
  var g = pg.gate(head, head);
  assert.strictEqual(g.checks.filter(function (x) { return x.id === 'B14' && x.surface === 'commonsthread'; }).length, 6);
  // a root the surface is already on has no entry in its pass: no check for it, rather than an ungraded one
  var self = pg.gate(results(REF, [load('commonsplan', 'phone', 'cold', { latency: latency({ open: { thread: { cold: 200, warm: 60 }, epic: { cold: 220, warm: 70 } } }) })]), null);
  assert.deepStrictEqual(self.checks.filter(function (x) { return x.id === 'B14'; }).map(function (x) { return x.sub; }), ['thread cold', 'thread warm', 'epic cold', 'epic warm']);
  // a pass whose stream had no test route reads null: ungraded, never passed
  var none = pg.verdicts(results(REF, [load('commonsthread', 'phone', 'cold', { latency: latency({ stream: null }) })]));
  assert.strictEqual(none.checks.find(function (x) { return x.id === 'B15'; }).verdict, 'UNGRADED');
  assert.strictEqual(none.budgets.B15, 'UNGRADED');
});

test('Commons v2 in the CI gate: hard, so an over-budget number fails even where the base was over too', function () {
  var slow = latency({ echo: 420, open: { thread: { cold: 200, warm: 60 }, plan: { cold: 900, warm: 80 }, epic: { cold: 220, warm: 70 } } });
  var base = results(REF, [load('commonsthread', 'phone', 'cold', { bytes: { wire: 200 * KB, requests: 12 }, latency: slow }),
    load('beads', 'phone', 'cold', { bytes: { wire: 400 * KB, requests: 5 } })]);
  var res = pg.gate(base, base);   // the same numbers on both sides: nothing got worse
  var st = function (id, surface, sub) { return res.checks.find(function (x) { return x.id === id && x.surface === surface && x.sub === (sub || ''); }).status; };
  assert.strictEqual(st('B1', 'beads'), 'over');                  // B1 keeps its ratchet
  assert.strictEqual(st('B16', 'commonsthread'), 'fail');         // B16 has none: 200 KB against 150 KB fails as on the base
  assert.strictEqual(st('B13', 'commonsthread'), 'fail');         // 420 ms is over the 50 ms budget in every run
  assert.strictEqual(st('B14', 'commonsthread', 'plan cold'), 'fail');
  assert.strictEqual(st('B14', 'commonsthread', 'thread cold'), 'ok');
  // noise still is noise: over the budget but within the far bench's 1.5x line, or with its fastest run within the line
  var far = results(FAR, [load('commonsthread', 'phone', 'cold', { latency: latency({ echo: 70 }), each: [{}, { latency: latency({ echo: 400 }) }, { latency: latency({ echo: 400 }) }] })]);
  var fr = pg.gate(far, far);
  assert.strictEqual(fr.checks.find(function (x) { return x.id === 'B13'; }).status, 'noisy');
  // the summary's Commons table: one row per workbench surface, its fails marked
  var md = pg.render(res, base, {});
  assert.match(md, /### Commons v2 \(B12–B17, phone, cold\)/);
  assert.match(md, /\| commonsthread \| .*❌ 420/);
  assert.ok(!/\| beads \| [^\n]*B12/.test(md));
  assert.strictEqual(pg.render(pg.gate(results(REF, [load('beads', 'phone', 'cold')]), null), null, {}).indexOf('Commons v2'), -1);
});

/* PR #1449 (gd-WjZhvgyw4c: one CSS property, overflow-wrap on .act .btn.small), as CI read it: four runs of the same
   head-vs-base diff, two red on B5 and B8. Every number below is the run's results.json, head and base. */
function replay(bench, rows) { return { meta: { runs: 3, idle: 60, chromium: '145.0.7632.6', bench: bench }, rows: rows }; }
function cold(surface, totals) { return totals.map(function (t, i) { return load(surface, 'phone', 'cold', { run: i + 1, longTasks: { total: t } }); }); }

test('PR #1449 replay, B5: run 40873635992 read a pod gone busy mid-run, which no bench next to the loads confirms, so it is noisy', function () {
  // the head's node slowed from the fourth surface on (world, channel, thread read like the base): beads 2.6x, wall 4.3x
  var head = replay([46, 54], cold('beads', [2357, 2296, 1614]).concat(cold('wall', [2178, 1507, 2413])));
  var base = replay([41, 34], cold('beads', [878, 1220, 737]).concat(cold('wall', [555, 508, 414])));
  var res = pg.gate(head, base);
  assert.strictEqual(res.bench.near, true);   // the opening benches, 50 and 37.5: the line is the budget, 1500
  ['beads', 'wall'].forEach(function (s) {
    var c = check(res, 'B5', s, 'phone', 'cold');
    assert.strictEqual(c.status, 'noisy', s + ': ' + c.why);   // was fail: fastest 1614 / 1507 over 1500, base within
    assert.match(c.why, /read no bench next to these loads/);
  });
  assert.strictEqual(res.failed, 0);
  // the same numbers with a bench next to each load: the head's node read 2.5x the base's while they loaded, so the
  // base scales to 2.5x and the head is within 20 % of it; on an even node the same head fails, as a page 2-4x slower should
  var busy = function (b) { return function (r) { return Object.assign({}, r, { bench: [b, b] }); }; };
  var hb = replay([46, 54], head.rows.map(busy(150))), bb = replay([41, 34], base.rows.map(busy(REF)));
  var r2 = pg.gate(hb, bb);
  assert.strictEqual(check(r2, 'B5', 'beads', 'phone', 'cold').status, 'noisy');
  assert.strictEqual(check(r2, 'B5', 'wall', 'phone', 'cold').status, 'noisy');
  var even = pg.gate(replay([46, 54], head.rows.map(busy(REF))), bb);
  assert.strictEqual(check(even, 'B5', 'beads', 'phone', 'cold').status, 'fail');
  assert.strictEqual(check(even, 'B5', 'wall', 'phone', 'cold').status, 'fail');
});

test('CPU: the bench next to the loads sets the line and the scale, not the run\'s opening bench', function () {
  var at = function (b, total) { return load('plans', 'phone', 'cold', { bench: [b - 4, b + 4], longTasks: { total: total } }); };
  // an opening bench at the reference, but the loads ran at 1.6x it: graded at the 1.5x line (2250), so 2000 is noisy
  var base = results(REF, [at(REF, 1400)]), head = results(REF, [at(FAR, 2000)]);
  assert.strictEqual(check(pg.gate(head, base), 'B5', 'plans', 'phone', 'cold').status, 'noisy');
  // an opening bench far off, but the loads ran at the reference on both sides: graded at the budget, 1900 fails
  var nb = results(FAR, [at(REF, 1400)]), nh = results(FAR, [at(REF, 1900), load('plans', 'phone', 'warm', { inp: 230 })]);
  var c = check(pg.gate(nh, nb), 'B5', 'plans', 'phone', 'cold');
  assert.strictEqual(c.status, 'fail', c.why);
  assert.match(pg.render(pg.gate(nh, nb), nh), /graded at the budget where the bench read next to a surface's loads is within 20 % of the reference, else at 1.5x the budget \(the head's opening bench is not within it\)/);
  // the warm load is read at its cold load's bench (here the budget, 200, not the 1.5x line); a base with none (an older
  // harness) leaves a time over the line noisy
  assert.strictEqual(check(pg.gate(nh, nb), 'B6', 'plans', 'phone', 'warm').status, 'fail');
  var oh = results(REF, [at(REF, 1900)]), ob = results(REF, [at(REF, 1400)]);
  var old = { meta: ob.meta, rows: ob.rows.map(function (r) { var o = Object.assign({}, r); delete o.bench; return o; }) };
  assert.strictEqual(check(pg.gate(oh, ob), 'B5', 'plans', 'phone', 'cold').status, 'fail');
  var c2 = check(pg.gate(oh, old), 'B5', 'plans', 'phone', 'cold');
  assert.strictEqual(c2.status, 'noisy');
  assert.match(c2.why, /the base read no bench next to these loads/);
  assert.strictEqual(check(pg.gate(nh, null), 'B5', 'plans', 'phone', 'cold').status, 'fail');   // no base at all: held to the line
});

test('PR #1456 replay, B5: run 40978560369 read the head\'s node 3.6x slower at its opening and 1.2x next to the loads, so the base scales by the larger', function () {
  // a harness-only diff: compose's head loads ran 2x the base's; the benches next to them (before/after each) read 114 vs 94
  var at = function (b, total) { return load('compose', 'phone', 'cold', { bench: b, longTasks: { total: total } }); };
  var head = replay([170, 170], [at([109, 235], 2971), at([100, 129], 3133), at([105, 82], 3139)]);
  var base = replay([47, 47], [at([45, 145], 1570), at([115, 60], 1416), at([64, 125], 1606)]);
  var c = check(pg.gate(head, base), 'B5', 'compose', 'phone', 'cold');
  assert.strictEqual(c.status, 'noisy', c.why);   // was fail at the loads' scale alone: 2971 over 1570 x 1.21 x 1.2
  // the same head on a node whose opening matched the base's still fails: the loads' ratio alone sets the scale then
  var even = check(pg.gate(replay([47, 47], head.rows), base), 'B5', 'compose', 'phone', 'cold');
  assert.strictEqual(even.status, 'fail', even.why);
});

test('PR #1449 replay, B8: run 40887048164 counted a minute of idle over 138 s and 200 s, its real windows', function () {
  // every idle request on the Commons is 322 B; the head's pod took 139 s to open the ten surfaces, the base's 77 s, and
  // each page's requests counted from its own open to its read, against idleS = 60
  function idleRows(windows, reqs) {
    return Object.keys(reqs).map(function (s) { return idle(s, 'phone', { idleBytes: 322 * reqs[s], idleRequests: reqs[s], idleMs: windows && windows[s] }); });
  }
  var hReq = { channel: 21, thread: 20, compose: 18 }, bReq = { channel: 18, thread: 17, compose: 16 };
  // as the run read it, over 60 s: 6.6, 6.0 and 5.7 KB/min against the base's 5.7, 5.3 and 5.0, so three fails
  var asRead = pg.gate(replay([49, 64], idleRows(null, hReq)), replay([43, 41], idleRows(null, bReq)));
  assert.strictEqual(asRead.failed, 3);
  assert.strictEqual(pg.fmt(check(asRead, 'B8', 'channel', 'phone', 'idle').head, 'KB/min'), '6.6');
  // over the windows the log's timestamps give (the open phase's start to each row's read; the page opened a few seconds
  // in, so these are upper bounds): about 2 KB/min on the head and 2.5 on the base, the 8 requests a minute every run on
  // main reads (40 runs, 18 commits, 2026-10-29: 7.7-8.3), well within the budget
  var res = pg.gate(replay([49, 64], idleRows({ channel: 200053, thread: 200764, compose: 201574 }, hReq)),
    replay([43, 41], idleRows({ channel: 137796, thread: 138482, compose: 138833 }, bReq)));
  ['channel', 'thread', 'compose'].forEach(function (s) { assert.strictEqual(check(res, 'B8', s, 'phone', 'idle').status, 'ok', s); });
  assert.strictEqual(res.failed, 0);
  assert.strictEqual(pg.fmt(check(res, 'B8', 'channel', 'phone', 'idle').head, 'KB/min'), '2.0');
  assert.strictEqual(pg.fmt(check(res, 'B8', 'channel', 'phone', 'idle').base, 'KB/min'), '2.5');
  // B8 stays a budget: a page that polls three times as often, over a true 60 s window, still fails
  var poll = pg.gate(replay([49, 64], [idle('channel', 'phone', { idleBytes: 322 * 24, idleMs: 60400 })]), replay([43, 41], [idle('channel', 'phone', { idleBytes: 322 * 8, idleMs: 60300 })]));
  assert.strictEqual(check(poll, 'B8', 'channel', 'phone', 'idle').status, 'fail');
});

test('B8 counts the frames a stream opened before the idle window receives in it, and not its load-time bytes', function () {
  // perf_baseline.js's tap on a fake CDP session: the page's EventSource opens during the load, the window starts once every
  // surface is open, then the stream sends a 1 KB frame every 10 s for a minute (6 KB/min) beside one 322 B poll
  var pb = require('./perf_baseline.js'), events = new (require('events').EventEmitter)(), tap = pb.netTap(events);
  var sent = function (id, url, type) { events.emit('Network.requestWillBeSent', { requestId: id, request: { url: url }, type: type, timestamp: 1 }); };
  var got = function (id, mime, type) { events.emit('Network.responseReceived', { requestId: id, type: type, response: { mimeType: mime, status: 200, headers: {} } }); };
  var data = function (id, n) { events.emit('Network.dataReceived', { requestId: id, dataLength: n, encodedDataLength: n }); };
  var done = function (id, n) { events.emit('Network.loadingFinished', { requestId: id, encodedDataLength: n }); };
  sent('doc', 'http://farm/garden', 'Document'); got('doc', 'text/html', 'Document'); data('doc', 40 * KB); done('doc', 40 * KB);
  sent('sse', 'http://farm/garden/events', 'EventSource'); got('sse', 'text/event-stream', 'EventSource'); data('sse', 3 * KB);   // the load's snapshot
  tap.mark();
  for (var i = 0; i < 6; i++) data('sse', KB);
  sent('poll', 'http://farm/garden/typing', 'Fetch'); got('poll', 'application/json', 'Fetch'); data('poll', 322); done('poll', 322);
  var win = tap.since();
  assert.strictEqual(pb.idleRequests(win), 1);   // the poll; the stream was not sent in the window
  assert.strictEqual(pb.idleBytes(win), 6 * KB + 322);
  var row = idle('channel', 'phone', { idleRequests: pb.idleRequests(win), idleBytes: pb.idleBytes(win), idleMs: 60000 });
  var c = check(pg.gate(results(REF, [row]), results(REF, [idle('channel', 'phone', { idleBytes: 322, idleMs: 60000 })])), 'B8', 'channel', 'phone', 'idle');
  assert.strictEqual(c.status, 'fail', c.why);
  assert.strictEqual(pg.fmt(c.head, 'KB/min'), '6.3');
  // with no frames in the window, the stream adds nothing: its 3 KB arrived before the mark
  var quiet = pb.netTap(events = new (require('events').EventEmitter)());
  sent('sse', 'http://farm/garden/events', 'EventSource'); got('sse', 'text/event-stream', 'EventSource'); data('sse', 3 * KB);
  quiet.mark();
  assert.strictEqual(pb.idleBytes(quiet.since()), 0);
});

/* A pan row per run: the pan's longest task, long-task total and frames over panMs (10 s), the calibration's over 2 s. The
   load is the pan's time per frame over the calibration's: (10000 / frames) / (2000 / cframes). */
function pan(runs) {
  return runs.map(function (p, i) {
    return { surface: 'world', profile: 'phone', kind: 'pan', run: i + 1, panMs: p.panMs || 10000, frames: p.frames, longTasks: { n: 40, total: p.total || 5000, max: p.max },
      calibration: { frames: p.cframes, ms: p.cms || 2000, long: 6, total: p.ctotal === undefined ? 1000 : p.ctotal, max: p.cmax }, zoom: { ms: 9000, frames: 60, longTasks: { n: 50, total: 9000, max: 900 } } };
  });
}
function withPan(bench, runs) {
  var r = results(bench, [load('world', 'phone', 'cold')]);
  r.rows = r.rows.concat(pan(runs));
  return r;
}
var POD = { cmax: 300, cframes: 40 };   // P2.1's agent pod: a bare canvas at 4x has 178-469 ms long tasks; 50 ms a frame here
function onPod(runs) { return runs.map(function (p) { return Object.assign({}, POD, p); }); }
var BASE_POD = onPod([{ frames: 100, max: 600 }, { frames: 104, max: 485 }, { frames: 96, max: 572 }]);   // loads 2.00, 1.92, 2.08x

test('B7 on a pod: the floor is over the line, so the pan is graded on its load against the base, never on the raw max', function () {
  var base = withPan(FAR, BASE_POD);
  var same = withPan(FAR, onPod([{ frames: 99, max: 640 }, { frames: 101, max: 900 }, { frames: 97, max: 500 }]));
  var res = pg.gate(same, base);
  var main = check(res, 'B7', 'world', 'phone', 'pan'), ld = check(res, 'B7', 'world', 'phone', 'pan', 'load');
  assert.strictEqual(main.status, 'ungraded');   // 640 ms max, but a bare canvas here is 300 ms
  assert.strictEqual(main.head, 640);
  assert.match(main.why, /cannot show it: a bare full-screen canvas's longest task is 300 ms/);
  assert.strictEqual(ld.status, 'ok');
  assert.ok(Math.abs(ld.head - 200 / 99) < 1e-9 && Math.abs(ld.base - 2) < 1e-9, ld.head + ' / ' + ld.base);
  assert.strictEqual(res.failed, 0);
  assert.deepStrictEqual(res.floor, { head: 300, base: 300 });
  // the node is busier on the head: the pan and the bare canvas both take twice as long a frame, so the load holds
  var busy = withPan(FAR, [{ frames: 50, max: 1400, cframes: 20, cmax: 700 }, { frames: 52, max: 1100, cframes: 20, cmax: 650 }, { frames: 48, max: 1500, cframes: 20, cmax: 720 }]);
  assert.strictEqual(check(pg.gate(busy, base), 'B7', 'world', 'phone', 'pan', 'load').status, 'ok');
  assert.strictEqual(pg.gate(busy, base).failed, 0);
  // the World got slower a frame in every run: over 2.00 x 1.5 = 3.00
  var worse = withPan(FAR, onPod([{ frames: 60, max: 700 }, { frames: 62, max: 650 }, { frames: 58, max: 720 }]));
  var f = check(pg.gate(worse, base), 'B7', 'world', 'phone', 'pan', 'load');
  assert.strictEqual(f.status, 'fail');
  assert.match(f.why, /more than 50 % over the base's 2.00x in every run \(fastest 3.23x\)/);
  // one run within the margin: node load, not the PR
  var once = withPan(FAR, onPod([{ frames: 60, max: 700 }, { frames: 100, max: 650 }, { frames: 58, max: 720 }]));
  assert.strictEqual(check(pg.gate(once, base), 'B7', 'world', 'phone', 'pan', 'load').status, 'noisy');
  // a node so busy the bare canvas is 98 % long tasks (gd-K3KmHhfnhd's run; the share of time in long tasks the load once
  // was could not read past 1.02x there, gd-b9F02iUPb3): time per frame does not saturate, so a pan twice as slow a frame fails
  var satBase = withPan(FAR, [{ frames: 50, total: 9700, max: 491, cframes: 20, ctotal: 1960, cmax: 376 }]);
  var sat = withPan(FAR, [{ frames: 25, total: 9900, max: 855, cframes: 20, ctotal: 1960, cmax: 376 }]);
  var sl = check(pg.gate(sat, satBase), 'B7', 'world', 'phone', 'pan', 'load');
  assert.strictEqual(sl.status, 'fail');
  assert.ok(Math.abs(sl.head - 4) < 1e-9 && Math.abs(sl.base - 2) < 1e-9, sl.head + ' / ' + sl.base);
  assert.strictEqual(check(pg.gate(satBase, satBase), 'B7', 'world', 'phone', 'pan', 'load').status, 'ok');
});

test('B7 floor just under the budget is not a machine that can show it: PR #1466 run 40933736969 (gd-caHXOpfxNC)', function () {
  // a splash-only diff: the head's floor read 98 / 68 / 257 ms (median 98, within 100), so B7 was graded raw at 100 ms and
  // failed on 281 ms against the base's 99, on a floor of 66; the pan's time per frame was no worse than the base's
  var head = withPan(35, [{ panMs: 10528, frames: 227, total: 4209, max: 296, cframes: 68, cms: 2101, ctotal: 638, cmax: 98 },
    { panMs: 10642, frames: 237, total: 3828, max: 149, cframes: 70, cms: 2123, ctotal: 240, cmax: 68 },
    { panMs: 10639, frames: 261, total: 4702, max: 281, cframes: 74, cms: 2150, ctotal: 447, cmax: 257 }]);
  var base = withPan(51, [{ panMs: 10706, frames: 266, total: 2565, max: 190, cframes: 67, cms: 2118, ctotal: 315, cmax: 122 },
    { panMs: 10525, frames: 276, total: 2027, max: 87, cframes: 100, cms: 2113, ctotal: 231, cmax: 66 },
    { panMs: 10482, frames: 285, total: 1264, max: 99, cframes: 94, cms: 2110, ctotal: 164, cmax: 61 }]);
  var res = pg.gate(head, base), main = check(res, 'B7', 'world', 'phone', 'pan'), ld = check(res, 'B7', 'world', 'phone', 'pan', 'load');
  assert.deepStrictEqual(res.floor, { head: 98, base: 66 });
  assert.strictEqual(main.status, 'ungraded');
  assert.strictEqual(main.head, 281);
  assert.match(main.why, /longest task is 98 ms here/);
  assert.strictEqual(ld.status, 'ok');
  assert.ok(Math.abs(ld.head - 1.48) < 0.01 && Math.abs(ld.base - 1.64) < 0.01, ld.head + ' / ' + ld.base);
  assert.strictEqual(res.failed, 0);
  // the line is FLOOR_SHARE of the budget: a floor of 50 ms still grades the raw max, 51 ms grades the load
  assert.strictEqual(pg.FLOOR_SHARE, 0.5);
  var at = withPan(REF, [{ frames: 100, max: 180, cframes: 40, cmax: 50 }]), over = withPan(REF, [{ frames: 100, max: 180, cframes: 40, cmax: 51 }]);
  assert.strictEqual(check(pg.gate(at, at), 'B7', 'world', 'phone', 'pan').status, 'over');   // graded at 100 ms, over as on the base
  assert.strictEqual(check(pg.gate(at, at), 'B7', 'world', 'phone', 'pan', 'load'), undefined);
  assert.strictEqual(check(pg.gate(over, over), 'B7', 'world', 'phone', 'pan').status, 'ungraded');
  assert.strictEqual(check(pg.gate(over, over), 'B7', 'world', 'phone', 'pan', 'load').status, 'ok');
});

test('B7 where the floor is well within the budget: the longest pan task is graded at the budget, the load is not a check', function () {
  var GPU = { cmax: 12, cframes: 110, ctotal: 0 };
  function gpu(runs) { return runs.map(function (p) { return Object.assign({ frames: 550 }, GPU, p); }); }
  var base = withPan(REF, gpu([{ total: 300, max: 80 }, { total: 300, max: 70 }, { total: 300, max: 90 }]));
  var ok = withPan(REF, gpu([{ total: 300, max: 85 }, { total: 300, max: 60 }, { total: 300, max: 95 }]));
  var res = pg.gate(ok, base);
  assert.strictEqual(check(res, 'B7', 'world', 'phone', 'pan').status, 'ok');
  assert.strictEqual(check(res, 'B7', 'world', 'phone', 'pan', 'load'), undefined);
  var slow = withPan(REF, gpu([{ total: 900, max: 180 }, { total: 900, max: 150 }, { total: 900, max: 210 }]));
  var c = check(pg.gate(slow, base), 'B7', 'world', 'phone', 'pan');
  assert.strictEqual(c.status, 'fail');   // graded at 100 ms, not 1.5x, though the bench is the reference's either way
  var spike = withPan(REF, gpu([{ total: 900, max: 180 }, { total: 300, max: 60 }, { total: 900, max: 210 }]));
  assert.strictEqual(check(pg.gate(spike, base), 'B7', 'world', 'phone', 'pan').status, 'noisy');   // its fastest run is within
  // on a far bench too the floor, not the bench, sets the line at 100 ms; the base is scaled by the benches, as for any CPU time
  var far = withPan(FAR, gpu([{ total: 900, max: 140 }, { total: 900, max: 130 }, { total: 900, max: 120 }]));
  assert.strictEqual(check(pg.gate(far, base), 'B7', 'world', 'phone', 'pan').status, 'noisy');   // 120 is within 80 x 1.6 x 1.2
  var farWorse = withPan(FAR, gpu([{ total: 900, max: 170 }, { total: 900, max: 180 }, { total: 900, max: 190 }]));
  assert.strictEqual(check(pg.gate(farWorse, base), 'B7', 'world', 'phone', 'pan').status, 'fail');   // past 154, under the 1.5x line
});

test('B7: a pan the PR broke fails, a pod with no base is ungraded, and the comment carries the floor and the load', function () {
  var base = withPan(FAR, BASE_POD);
  var broke = results(FAR, [load('world', 'phone', 'cold')]);
  broke.rows.push({ surface: 'world', profile: 'phone', kind: 'pan', run: 1, error: 'page.waitForFunction: Timeout 120000ms exceeded' });
  var res = pg.gate(broke, base);
  assert.strictEqual(check(res, 'B7', 'world', 'phone', 'pan').status, 'fail');
  assert.match(check(res, 'B7', 'world', 'phone', 'pan').why, /measured on the base/);
  var lone = pg.gate(base, null);
  assert.strictEqual(check(lone, 'B7', 'world', 'phone', 'pan').status, 'ungraded');
  assert.strictEqual(check(lone, 'B7', 'world', 'phone', 'pan', 'load').status, 'ungraded');
  assert.strictEqual(lone.failed, 0);
  var v = pg.verdicts(base);   // a UX grade on a pod: the max is not B7's, and a load needs a base
  assert.strictEqual(v.budgets.B7, 'UNGRADED');
  var md = pg.render(pg.gate(base, base), base);
  assert.match(md, /B7 floor \(a bare full-screen canvas's longest task, ms\): head 300, base 300; over 50 ms \(no GPU raster\), so B7 is graded on the pan's load against the base \(fails over \+50 % in every run\)/);
  assert.match(md, /\| B5 ms \| B6 ms \| B7 ms \| B8 KB\/min \|/);
  assert.match(md, /\| world \|( [^|]*\|){6} 572 \(±0\) \/ 2.00x \(±0\) \|/);   // the B7 cell: max, then load, unmarked (load ok)
  assert.match(md, /its load: its time per frame over a bare full-screen canvas's/);
  var gpu = withPan(REF, [{ frames: 550, total: 300, max: 60, cframes: 110, ctotal: 0, cmax: 12 }]);
  assert.strictEqual(pg.verdicts(gpu).budgets.B7, 'PASS');
});
