/* The Lands performance gate (plan gd-MEGB3dEaHu, step P3.1 gd-7Ns0SDsF7R): grades a head commit's perf_baseline.js
   results against the B1-B17 budgets of gardenio/docs/commons-ux-rubric.md section 7, next to the same script's results
   for its base, and fails when a budget regresses (B12-B17, the Commons v2 budgets, are hard: commonsV2, below).
   .github/workflows/lands-perf.yml measures both sides and runs it, nightly on main and on dispatch, never on a pull
   request; "the PR" below is the head and its changes.
   Usage:
     node perf_gate.js <head results.json> [<base results.json>] <out dir>
   Writes <out dir>/gate.md (the run summary) and <out dir>/gate.json (every check), prints gate.md, and exits 1 when a
   budget regressed, 0 otherwise. Environment: LANDS_PERF_RUN_URL, a link to the run whose artifact holds the traces.

   Each budget is graded per surface, profile and load kind on the median of the runs, as the rubric says. What fails:
     stable class (bytes, requests, heap, desktop times): the head is over the budget, and the base was within it, or
                there is no base value for it (a new surface is held to the budget), or the head is worse than the base
                by more than the budget's tolerance. Every surface is over B1 today (1.5x, P0.1), so the gate is a
                ratchet: an over-budget number that does not get worse is reported, not failed. A hard budget (B12-B17)
                has no ratchet: over it fails, whatever the base read.
     CPU class (phone times under 4x throttling, which move up to 2x with node load): graded at the budget when the head's
                CPU bench is within 20 % of REFERENCE_BENCH, else at 1.5x the budget, as on any shared CI pod. Over the
                budget but within that line is "over budget, noisy". Over the line fails only when the base was within
                it and the head is more than CPU_MARGIN worse than the base, with the base scaled by the two runs'
                benches (base and head run on different nodes); a hard budget fails over the line even when the base
                was over it too. The bench is the one perf_baseline.js reads next to each
                cold phone load (rows' bench: just before and just after it), median over the surface's runs: a pod's
                speed moves 2-4x within a run, which the run's opening bench never sees. PR #1449 (one CSS property,
                run 40873635992) read B5 on the head like the base for the three surfaces measured first, then 2.5-4.3x
                the base from the fourth on, at an opening bench of 46 ms against the base's 41; on main against
                itself, 40 runs on 2026-10-29 read the wall's B5 from 333 to 3,324 ms. So where either side's rows have
                no bench of their own (results from before gd-CLQT35hQnk), a phone time over the line is noisy, not a
                failure: the run cannot tell the node from the page. The base is scaled by the larger of the two
                ratios, the loads' benches and the openings': each is a few hundred ms of one workload, and either can
                miss a slow node. PR #1456's own run 40978560369 (harness-only diff) read the head's opening bench 170
                ms against the base's 47 and its compose loads 2x the base's, at load benches of 114 against 94.
     times:     a time (B4-B6, B10, B11) fails only when the head's fastest run is over as well: node load only ever adds
                time, so a regression shows in every run, while a busy node shows in one or two. The first CI run on
                identical code (40824057236) read the World's desktop INP 200, 48 and 232 ms on the head against 40, 48
                and 40 on the base. A median over the line with its fastest run within it is "noisy".
     B8:        idle bytes over the idle window each row measured (idleMs: from the moment every surface is open to the
                row's read), per minute; rows without it (before gd-CLQT35hQnk) over idleS, which counted the requests
                a page sent while the surfaces after it opened, 40-140 s on a CI pod, as idle.
     B7 (pan):  the pan pass's rows (kind pan, the World on the phone; perf_baseline.js) carry a calibration: a bare
                full-screen canvas at the same throttle, the machine's floor. Where the head's floor (the median of its
                runs' longest calibration task) is within FLOOR_SHARE of the 100 ms budget, the machine can show B7, and
                the pan's longest task is graded as a CPU time at the budget. Where it is not (a shared pod has no GPU,
                so any full-screen canvas rasterizes on the main thread at Commit: 56-258 ms on the CI pool, 178-469 ms
                on an agent pod, P2.1), a raw maximum says nothing about the PR, so B7 is ungraded and its sub-check
                "load" is graded instead. A floor just under the budget is not a machine that can show it: PR #1466's
                head read the floor 98 ms (its runs 98, 68 and 257), the pan 281 ms against the base's 99, on a splash-
                only diff (gd-caHXOpfxNC), and on 21 base runs of main the floor's median was 94 ms. The load is the
                pan's time per frame over the calibration's, both read in the same run, so node load moves both and
                neither saturates: on those 21 runs it read 1.40-2.72x (the share of time in long tasks it replaced
                read 0.22-6.25x, and could not read past 1 / the bare canvas's share on a busy node, gd-b9F02iUPb3).
                It fails only when the head's fastest run is more than PAN_MARGIN worse than the base's median; with
                no base it is ungraded. It catches work added to every frame of the pan, not a lone long task, which
                only a machine whose floor allows it can show.
     ungraded:  a value the head could not measure fails when the base measured it (the PR broke the surface), else it
                is reported. B7 with no pan pass on either side (a narrowed run, LANDS_PERF_PAN=0) is ungraded.
   Run with `require` (the workflow's github-script does), it exports run() and the pieces perf_gate.test.js checks, and
   verdicts(), which grades one surface at the budgets for a Commons UX grade (the commons-ux-acceptance skill's shoot.js). */
'use strict';
var fs = require('fs'), path = require('path');

var KB = 1024, MB = 1048576;

/* The reference bench (perf_baseline.js's bench(): a fixed workload, unthrottled, median ms of five tries; lower is
   faster) for the CPU class, recorded by P3.1: the machine the budgets were set on, as P0.1 read it on a gardenio agent
   pod (2 CPUs on a shared node, Chromium 145.0.7632.6, 2026-10-28; gardenio/docs/lands-perf-baseline.md, Caveats): 69 ms
   before a phone run and 53 ms after it, so 61. The same bench on an agent pod while its node ran at load 10 (2026-10-29)
   read 282-546 ms over 15 tries, which is why a CPU budget is graded at itself only near the reference. */
var REFERENCE_BENCH = 61;
var BENCH_NEAR = 0.2, CPU_LINE = 1.5, CPU_MARGIN = 0.2;
/* How much worse than the base's B7 load the head's fastest run may be. The load is a ratio of two reads in the same run,
   so it is not scaled by the benches, but the two reads are 2 s and 10 s apart on a shared CPU, and some nodes read the pan
   at 2.5-3.0x every run where most read 1.4-2.1x. On every pairing of 24 CI results of the same code (gd-caHXOpfxNC) the
   fastest-run rule fails 2.5 % of them at 50 % (6.7 % at 25 %); a pan made 2x slower a frame fails 76 % of them. */
var PAN_MARGIN = 0.5;
/* The share of B7's budget the calibration floor must be within for the pan's longest task to be graded at the budget. */
var FLOOR_SHARE = 0.5;

function at(row, field) { return field.split('.').reduce(function (o, k) { return o === null || o === undefined ? o : o[k]; }, row); }
function fieldOf(field) { return function (r) { return at(r, field); }; }
/* A pan row's load: the pan's time per frame over the calibration's (a bare full-screen canvas, same run). */
function panLoad(r) {
  var c = r.calibration;
  return c && c.frames > 0 && c.ms > 0 && r.frames > 0 && r.panMs > 0 ? (r.panMs / r.frames) / (c.ms / c.frames) : null;
}

/* One entry per graded check, in rubric order. rubric: text the rubric's row for the id must carry (perf_gate.test.js
   holds the two together). tol: how much worse than the base an already-over stable number may get, the larger of rel
   (of the base) and abs. only: the surfaces it applies to (default every one). time: graded on the fastest run too. hard:
   no ratchet, an over-budget number fails even where the base was over too. has: a check only where a side has a row it
   holds for (B14's per-root opens). floor: graded at the budget only where the calibration floor is within FLOOR_SHARE
   of it (B7). load: graded against the base's median, PAN_MARGIN apart, only where the floor is not (B7 load). always:
   reported as ungraded when the surface ran on the profile but neither side has a row of this kind. */
var BUDGETS = [
  { id: 'B1', label: 'cold weight', rubric: '≤ 300 KB', cls: 'stable', profile: 'phone', kinds: ['cold'], only: function (s) { return s !== 'glass'; }, value: fieldOf('bytes.wire'), limit: 300 * KB, unit: 'KB', tol: { rel: 0.02, abs: 2 * KB } },
  /* the Glass watch: B1 plus its terminal, xterm.js (gd-T8Lrqgp23V) */
  { id: 'B1', sub: 'terminal', label: 'cold weight with a terminal', rubric: '≤ 367 KB', cls: 'stable', profile: 'phone', kinds: ['cold'], only: ['glass'], value: fieldOf('bytes.wire'), limit: 367 * KB, unit: 'KB', tol: { rel: 0.02, abs: 2 * KB } },
  { id: 'B2', label: 'warm weight', rubric: '≤ 50 KB', cls: 'stable', profile: 'phone', kinds: ['warm'], value: fieldOf('bytes.wire'), limit: 50 * KB, unit: 'KB', tol: { rel: 0.05, abs: 2 * KB } },
  { id: 'B3', label: 'requests', rubric: '≤ 20 requests', cls: 'stable', profile: 'phone', kinds: ['cold'], value: fieldOf('bytes.requests'), limit: 20, unit: 'req', tol: { rel: 0, abs: 0 } },
  { id: 'B4', time: true, label: 'LCP', rubric: 'LCP ≤ 3.0 s', cls: 'CPU', profile: 'phone', kinds: ['cold'], value: fieldOf('lcp'), limit: 3000, unit: 'ms' },
  { id: 'B4', sub: 'ready', time: true, label: 'Ready', rubric: 'Ready ≤ 3.0 s', cls: 'CPU', profile: 'phone', kinds: ['cold'], only: ['world'], value: fieldOf('ready'), limit: 3000, unit: 'ms' },
  { id: 'B5', time: true, label: 'long tasks', rubric: '≤ 1,500 ms in total', cls: 'CPU', profile: 'phone', kinds: ['cold'], value: fieldOf('longTasks.total'), limit: 1500, unit: 'ms' },
  { id: 'B6', time: true, label: 'INP', rubric: 'INP ≤ 200 ms', cls: 'CPU', profile: 'phone', kinds: ['cold', 'warm'], value: fieldOf('inp'), limit: 200, unit: 'ms' },
  { id: 'B7', time: true, floor: true, always: true, label: 'pan long task', rubric: 'No long task over 100 ms', cls: 'CPU', profile: 'phone', kinds: ['pan'], only: ['world'],
    value: fieldOf('longTasks.max'), limit: 100, unit: 'ms' },
  { id: 'B7', sub: 'load', time: true, load: true, label: 'pan load', rubric: 'No long task over 100 ms', cls: 'CPU', profile: 'phone', kinds: ['pan'], only: ['world'],
    value: panLoad, limit: 0, unit: 'x' },
  { id: 'B8', label: 'idle network', rubric: '≤ 5 KB per minute', cls: 'stable', profile: 'phone', kinds: ['idle'], limit: 5 * KB, unit: 'KB/min', tol: { rel: 0.1, abs: 0.5 * KB },
    value: function (r) { var ms = r.idleMs > 0 ? r.idleMs : r.idleS * 1000; return ms > 0 && r.idleBytes !== undefined ? r.idleBytes / (ms / 60000) : null; } },
  { id: 'B9', sub: 'growth', label: 'idle heap growth', rubric: 'heap after GC ≤ heap loaded', cls: 'stable', profile: ['phone', 'desktop'], kinds: ['idle'], limit: 0, unit: 'MB', tol: { rel: 0, abs: 0.5 * MB },
    value: function (r) { return r.heapIdleGC !== undefined && r.heapLoaded !== undefined ? r.heapIdleGC - r.heapLoaded : null; } },
  { id: 'B9', label: 'idle heap', rubric: '≤ 10 MB', cls: 'stable', profile: ['phone', 'desktop'], kinds: ['idle'], value: fieldOf('heapIdleGC'), limit: 10 * MB, unit: 'MB', tol: { rel: 0.1, abs: 0.5 * MB } },
  { id: 'B10', time: true, label: 'desktop LCP', rubric: 'LCP ≤ 1.5 s', cls: 'stable', profile: 'desktop', kinds: ['cold'], value: fieldOf('lcp'), limit: 1500, unit: 'ms', tol: { rel: 0.2, abs: 50 } },
  { id: 'B10', time: true, sub: 'ready', label: 'desktop Ready', rubric: 'Ready ≤ 1.5 s', cls: 'stable', profile: 'desktop', kinds: ['cold'], only: ['world'], value: fieldOf('ready'), limit: 1500, unit: 'ms', tol: { rel: 0.2, abs: 50 } },
  { id: 'B11', time: true, label: 'desktop INP', rubric: 'INP ≤ 100 ms', cls: 'stable', profile: 'desktop', kinds: ['cold', 'warm'], value: fieldOf('inp'), limit: 100, unit: 'ms', tol: { rel: 0.2, abs: 16 } },
].concat(commonsV2());

/* The Commons v2 interaction budgets (plan gd-lRaCxsiLvC, step 1 gd-J3LZYVcuBK): B12-B15 read perf_baseline.js's latency
   pass, B16 and B17 tighten B1 and B4 for the Commons. They apply to the workbench's surfaces only (commons(): perf_baseline.js's
   commonsthread and commonsplan, a UX grade's commons* rows) and are hard (no ratchet), so the budgets the plan met hold
   after it (step 9, gd-4s2Rw9hgBd). A surface whose rows have no latency pass reads them ungraded. */
function commons(s) { return /^commons/.test(s); }
function commonsV2() {
  var b = function (o) { return Object.assign({ cls: 'CPU', time: true, hard: true, profile: 'phone', kinds: ['cold'], only: commons, unit: 'ms' }, o); };
  var open = [];
  ['thread', 'plan', 'epic'].forEach(function (root) {
    // a root the surface is already on is not opened (perf_baseline.js latencyPass): no check, rather than an ungraded one
    var has = function (r) { return !!(r.latency && r.latency.open && r.latency.open[root]); };
    open.push(b({ id: 'B14', sub: root + ' cold', label: 'open ' + root + ', cold', rubric: '≤ 300 ms cold', has: has, value: fieldOf('latency.open.' + root + '.cold'), limit: 300 }));
    open.push(b({ id: 'B14', sub: root + ' warm', label: 'open ' + root + ', warm', rubric: '≤ 100 ms warm', has: has, value: fieldOf('latency.open.' + root + '.warm'), limit: 100 }));
  });
  return [
    b({ id: 'B12', label: 'key to paint', rubric: '≤ 16 ms', value: fieldOf('latency.keyPaint.median'), limit: 16 }),
    b({ id: 'B12', sub: 'rows', time: false, label: 'rows repainted by a key', rubric: 'no row repainted', value: fieldOf('latency.rows'), limit: 0, unit: 'rows' }),
    b({ id: 'B13', label: 'send to echo', rubric: '≤ 50 ms', value: fieldOf('latency.echo'), limit: 50 }),
  ].concat(open, [
    b({ id: 'B15', label: 'stream event to paint', rubric: '≤ 100 ms', value: fieldOf('latency.stream'), limit: 100 }),
    b({ id: 'B16', label: 'Commons cold weight', rubric: '≤ 150 KB', cls: 'stable', time: false, value: fieldOf('bytes.wire'), limit: 150 * KB, unit: 'KB', tol: { rel: 0.02, abs: 2 * KB } }),
    b({ id: 'B17', label: 'Commons LCP', rubric: 'LCP ≤ 1.5 s', value: fieldOf('lcp'), limit: 1500 }),
  ]);
}
function applies(b, surface) { return !b.only || (typeof b.only === 'function' ? b.only(surface) : b.only.indexOf(surface) >= 0); }

function median(xs) {
  xs = xs.filter(function (x) { return x !== null && x !== undefined && !isNaN(x); }).sort(function (a, b) { return a - b; });
  if (!xs.length) return null;
  var m = xs.length >> 1; return xs.length % 2 ? xs[m] : (xs[m - 1] + xs[m]) / 2;
}
function bench(res) { return res && res.meta && res.meta.bench && res.meta.bench.length ? median(res.meta.bench) : null; }
/* A side's bench next to a surface's loads on a profile: the median over its cold runs of each run's mean of the bench just
   before and just after the load (perf_baseline.js's row bench), null when no row carries one. A warm load follows its
   cold one in the same run, so it is read at the same bench. */
function loadBench(res, surface, profile) {
  if (!res) return null;
  return median(res.rows.filter(function (r) { return r.surface === surface && r.profile === profile && r.kind === 'cold' && !r.error && r.bench && r.bench.length; })
    .map(function (r) { return r.bench.reduce(function (a, x) { return a + x; }, 0) / r.bench.length; }));
}
/* The calibration floor of a side's pan pass: the median over its runs of the longest bare-canvas task (null with no pan
   rows). */
function calRows(res) { return res ? res.rows.filter(function (r) { return r.kind === 'pan' && !r.error && r.calibration; }) : []; }
function floorOf(res) { return median(calRows(res).map(function (r) { return r.calibration.max; })); }
function surfaces(res) {
  var seen = [];
  (res ? res.rows : []).forEach(function (r) { if (seen.indexOf(r.surface) < 0) seen.push(r.surface); });
  return seen;
}

/* A side's value for one check: undefined when the side has no rows for it at all (not run, or a surface it does not
   have), null when it has rows but no value (the load errored, the act threw), else the median. */
function valueOf(res, b, surface, profile, kind) {
  if (!res) return undefined;
  var rows = res.rows.filter(function (r) { return r.surface === surface && r.profile === profile && r.kind === kind; });
  if (!rows.length) return undefined;
  return median(rows.filter(function (r) { return !r.error; }).map(b.value));
}
/* The fastest run's value, for a time budget. */
function bestOf(res, b, surface, profile, kind) {
  if (!res || !b.time) return null;
  var xs = res.rows.filter(function (r) { return r.surface === surface && r.profile === profile && r.kind === kind && !r.error; }).map(b.value)
    .filter(function (x) { return x !== null && x !== undefined && !isNaN(x); });
  return xs.length ? Math.min.apply(null, xs) : null;
}

/* h and base are medians; best is the head's fastest run for a time budget (null otherwise), and it, not the median, is
   what must be over a line or worse than the base for a time to fail. */
function grade(b, h, base, ctx, best) {
  if (b.ungraded) return { status: 'ungraded', why: b.ungraded };
  if (h === null || h === undefined) {
    if (base !== null && base !== undefined) return { status: 'fail', why: 'not measured on this PR, measured on the base' };
    return { status: 'ungraded', why: h === null ? 'not measured (the load or its interaction failed)' : 'not run' };
  }
  var noBase = base === null || base === undefined, d = best === null || best === undefined ? h : best;
  if (b.floor) {
    if (ctx.floor !== null && ctx.floor > FLOOR_SHARE * b.limit) {
      return { status: 'ungraded', floored: true, why: 'this machine cannot show it: a bare full-screen canvas\'s longest task is ' + fmt(ctx.floor, 'ms') +
        ' ms here (no GPU raster), so the pan is graded on its load against the base (B7 load)' };
    }
    ctx = Object.assign({}, ctx, { near: true });   // the floor is well within the budget: grade at the budget itself
  }
  if (b.load) {
    if (noBase) return { status: 'ungraded', why: 'nothing on the base to compare: the pan\'s load is graded only against the base' };
    var worse = base * (1 + PAN_MARGIN);
    if (h <= worse) return { status: 'ok' };
    if (d <= worse) return { status: 'noisy', why: 'more than ' + PAN_MARGIN * 100 + ' % over the base in the median, but its fastest run (' + fmt(d, b.unit) + 'x) is within it: node load' };
    return { status: 'fail', why: 'more than ' + PAN_MARGIN * 100 + ' % over the base\'s ' + fmt(base, b.unit) + 'x in every run (fastest ' + fmt(d, b.unit) + 'x)' };
  }
  var fastest = 'over budget in the median, but its fastest run (' + fmt(d, b.unit) + ' ' + b.unit + ') is within ';
  if (b.cls === 'stable') {
    if (h <= b.limit) return { status: 'ok' };
    if (d <= b.limit) return { status: 'noisy', why: fastest + 'it: node load' };
    if (noBase) return { status: 'fail', why: 'over budget, nothing on the base to compare' };
    if (base <= b.limit) return { status: 'fail', why: 'over budget, the base was within it' };
    if (b.hard) return { status: 'fail', why: 'over budget, as on the base: a hard budget has no ratchet' };
    var slack = Math.max(b.tol.abs, b.tol.rel * base);
    if (d - base > slack) return { status: 'fail', why: 'over budget and worse than the base by more than ' + fmt(slack, b.unit) + ' ' + b.unit };
    return { status: 'over', why: 'over budget, as on the base' };
  }
  var line = ctx.near ? b.limit : CPU_LINE * b.limit;
  if (h <= b.limit) return { status: 'ok' };
  if (h <= line) return { status: 'noisy', why: 'over budget, noisy (fails over ' + fmt(line, b.unit) + ' ' + b.unit + ' on this bench)' };
  if (d <= line) return { status: 'noisy', why: fastest + 'the ' + fmt(line, b.unit) + ' ' + b.unit + ' line: node load' };
  if (noBase) return { status: 'fail', why: 'over ' + (ctx.near ? 'budget' : CPU_LINE + 'x the budget') + ', nothing on the base to compare' };
  var scaled = base * ctx.scale;
  if (base > line) return { status: b.hard ? 'fail' : 'over', why: 'over ' + (ctx.near ? 'budget' : CPU_LINE + 'x the budget') + ', as on the base' + (b.hard ? ': a hard budget has no ratchet' : '') };
  if (ctx.unread) return { status: 'noisy', why: 'over the line, but ' + ctx.unread + ' read no bench next to these loads, so node load cannot be told from the page' };
  if (d <= scaled * (1 + CPU_MARGIN)) return { status: 'noisy', why: 'over the line, but within ' + CPU_MARGIN * 100 + ' % of the base at this bench' };
  return { status: 'fail', why: 'over ' + (ctx.near ? 'budget' : CPU_LINE + 'x the budget') + ', the base was within it' };
}

/* A phone time's context: near from the bench read next to the surface's loads on the head, scale the larger of the loads'
   ratio and the run's, else (a side has none) the run's, marked unread. Kinds other than a cold or warm load (B7's pan)
   keep the run's. */
function loadCtx(ctx, head, base, surface, profile, kind) {
  if (kind !== 'cold' && kind !== 'warm') return ctx;
  var hb = loadBench(head, surface, profile), bb = loadBench(base, surface, profile);
  if (hb === null || (base && bb === null)) return Object.assign({}, ctx, { unread: hb === null ? 'the head' : 'the base' });
  return Object.assign({}, ctx, { near: Math.abs(hb / REFERENCE_BENCH - 1) <= BENCH_NEAR, scale: bb ? Math.max(hb / bb, ctx.scale) : ctx.scale });
}

/* Every check for head against base (either may be null). */
function gate(head, base) {
  var hb = bench(head), bb = bench(base), hf = floorOf(head), bf = floorOf(base), floorMax = FLOOR_SHARE * BUDGETS.find(function (x) { return x.floor; }).limit;
  var ctx = { near: hb !== null && Math.abs(hb / REFERENCE_BENCH - 1) <= BENCH_NEAR, scale: hb && bb ? hb / bb : 1, floor: hf };
  var names = surfaces(head);
  surfaces(base).forEach(function (s) { if (names.indexOf(s) < 0) names.push(s); });
  var checks = [];
  BUDGETS.forEach(function (b) {
    [].concat(b.profile).forEach(function (profile) {
      b.kinds.forEach(function (kind) {
        names.forEach(function (surface) {
          if (!applies(b, surface)) return;
          if (b.has && !hasRow(head, b, surface, profile, kind) && !hasRow(base, b, surface, profile, kind)) return;   // not a check here
          var h = b.ungraded ? undefined : valueOf(head, b, surface, profile, kind), v = b.ungraded ? undefined : valueOf(base, b, surface, profile, kind);
          if (b.load && (hf === null || hf <= floorMax)) return;   // B7 itself is graded (or not run): the load adds nothing
          if (!b.ungraded && h === undefined && v === undefined) {   // neither side ran it (a narrowed run)
            if (!b.always || !ranOn(head, surface, profile)) return;
          }
          var best = bestOf(head, b, surface, profile, kind), g = grade(b, h, v, b.cls === 'CPU' ? loadCtx(ctx, head, base, surface, profile, kind) : ctx, best);
          if (b.always && h === undefined && v === undefined) g = { status: 'ungraded', why: 'not run: no ' + kind + ' pass on either side' };
          checks.push({ id: b.id, sub: b.sub || '', label: b.label, cls: b.cls, surface: surface, profile: profile, kind: kind, unit: b.unit,
            limit: b.limit, head: h === undefined ? null : h, headBest: best, base: v === undefined ? null : v, status: g.status, why: g.why || '', floored: !!g.floored });
        });
      });
    });
  });
  return { checks: checks, failed: checks.filter(function (c) { return c.status === 'fail'; }).length, bench: { head: hb, base: bb, reference: REFERENCE_BENCH, near: ctx.near },
    floor: { head: hf, base: bf } };
}
/* Whether a side has a row of the load that b.has() holds for. */
function hasRow(res, b, surface, profile, kind) {
  return !!res && res.rows.some(function (r) { return r.surface === surface && r.profile === profile && r.kind === kind && b.has(r); });
}
/* Whether a side has any row for surface on profile. */
function ranOn(res, surface, profile) {
  return !!res && res.rows.some(function (r) { return r.surface === surface && r.profile === profile; });
}

function fmt(v, unit) {
  if (v === null || v === undefined) return '–';
  if (unit === 'KB' || unit === 'KB/min') { var k = v / KB; return Math.abs(k) < 10 ? k.toFixed(1) : Math.round(k) + ''; }
  if (unit === 'MB') return (v / MB).toFixed(1);
  if (unit === 'x') return v.toFixed(2);
  return Math.round(v) + '';
}
function delta(c) {
  if (c.head === null || c.base === null) return '';
  var d = c.head - c.base, s = fmt(Math.abs(d), c.unit);
  return /^0(\.0+)?$/.test(s) ? '±0' : (d > 0 ? '+' : '−') + s;
}
var MARK = { fail: '❌ ', over: '⚠️ ', noisy: '〰️ ', ungraded: '· ', ok: '' };
var RANK = { fail: 4, over: 3, noisy: 2, ungraded: 1, ok: 0 };
/* A check's line: its budget, or for B7's load (a ratio with no budget of its own) the base's plus PAN_MARGIN. */
function limitOf(c) { return c.unit === 'x' ? 'the base +' + PAN_MARGIN * 100 + ' %' : fmt(c.limit, c.unit); }
function where(c) { return c.surface + ' ' + c.profile + ' ' + c.kind; }
function name(c) { return c.id + (c.sub ? ' ' + c.sub : ''); }

/* The by-surface table's columns: each budget's main check on the phone (B9: the phone's heap after GC; B7: the pan's
   longest task and its load), a cell's mark the worst of every check under it but a B7 left to its load. */
var COLUMNS = ['B1', 'B2', 'B3', 'B4', 'B5', 'B6', 'B7', 'B8', 'B9', 'B10', 'B11'];

function render(res, head, opts) {
  opts = opts || {};
  var meta = (head && head.meta) || {}, n = res.failed, lines = ['<!-- lands-perf-gate -->'];
  lines.push('## Lands perf gate: ' + (n ? '❌ ' + n + ' budget check' + (n > 1 ? 's' : '') + ' regressed' : '✅ no budget regressed'), '');
  var b = res.bench, cpu = 'graded at the budget where the bench read next to a surface\'s loads is within ' + BENCH_NEAR * 100 + ' % of the reference, else at ' +
    CPU_LINE + 'x the budget (the head\'s opening bench is ' + (b.head === null ? 'missing' : (b.near ? '' : 'not ') + 'within it') + ')';
  lines.push('Fixture farm, Chromium ' + (meta.chromium || '?') + ', medians of ' + (meta.runs || '?') + ' run(s), idle pass ' + (meta.idle || 0) + ' s. ' +
    'CPU bench (ms, lower is faster): head ' + fmt(b.head, 'ms') + ', base ' + fmt(b.base, 'ms') + ', reference ' + b.reference + '. ' +
    'Phone CPU budgets (B4–B6) ' + cpu + '. ' + floorLine(res) + 'Budgets and classes: `gardenio/docs/commons-ux-rubric.md` §7.' +
    (opts.noBase ? ' **No base results**: every over-budget number fails.' : ''), '');
  var bad = res.checks.filter(function (c) { return c.status === 'fail'; });
  if (bad.length) {
    lines.push('### Regressions', '');
    bad.forEach(function (c) {
      lines.push('- **' + name(c) + '** ' + where(c) + ': ' + fmt(c.head, c.unit) + ' ' + c.unit + ' against ' + limitOf(c) +
        (c.base !== null ? ', base ' + fmt(c.base, c.unit) + ' (' + delta(c) + ')' : '') + ': ' + c.why + '.');
    });
    lines.push('');
  }
  var names = [];
  res.checks.forEach(function (c) { if (names.indexOf(c.surface) < 0) names.push(c.surface); });
  lines.push('### By surface', '', 'Head value (change from the base). ❌ regressed, ⚠️ over budget as on the base, 〰️ over budget but noisy, · ungraded.', '');
  lines.push('| Surface | ' + COLUMNS.map(function (id) { return id + ' ' + BUDGETS.find(function (x) { return x.id === id; }).unit; }).join(' | ') + ' |');
  lines.push('|---|' + COLUMNS.map(function () { return '--:'; }).join('|') + '|');
  names.forEach(function (s) {
    var cells = COLUMNS.map(function (id) {
      var all = res.checks.filter(function (c) { return c.surface === s && c.id === id; });
      if (!all.length) return '';
      var marked = all.filter(function (c) { return !c.floored; });
      if (!marked.length) marked = all;
      var worst = marked.reduce(function (a, c) { return RANK[c.status] > RANK[a.status] ? c : a; }, marked[0]);
      var shown = all.filter(function (c) { return (!c.sub || c.sub === 'load') && (c.profile === 'phone' || id === 'B10' || id === 'B11'); });
      var parts = (shown.length ? shown : [all[0]]).map(function (c) { return fmt(c.head, c.unit) + (c.unit === 'x' && c.head !== null ? 'x' : '') + (delta(c) ? ' (' + delta(c) + ')' : ''); });
      return MARK[worst.status] + parts.join(' / ');
    });
    lines.push('| ' + s + ' | ' + cells.join(' | ') + ' |');
  });
  commonsTable(res, lines);
  if (b.head && b.base && Math.abs(b.head / b.base - 1) > BENCH_NEAR) {
    lines.push('', 'Head and base ran on different nodes (CPU bench ' + fmt(b.head, 'ms') + ' and ' + fmt(b.base, 'ms') + ' ms), so the phone CPU columns (B4–B6) move with the node, not the PR. The gate compares them with the base scaled by the two benches.');
  }
  lines.push('', 'B6 and B11 show cold / warm. B4 and B10 on the World are graded on Ready as well; B9 on the desktop and on growth. ' +
    'B7 shows the longest task in the World\'s 10 s pan on the 10x farm, then its load: its time per frame over a bare full-screen canvas\'s on the same machine.', '');
  lines.push('<details><summary>Every check (' + res.checks.length + ')</summary>', '');
  lines.push('| Check | Class | Surface | Profile | Load | Head | Base | Change | Budget | Status |');
  lines.push('|---|---|---|---|---|--:|--:|--:|--:|---|');
  res.checks.slice().sort(function (a, c) { return RANK[c.status] - RANK[a.status]; }).forEach(function (c) {
    lines.push('| ' + [name(c), c.cls, c.surface, c.profile, c.kind, fmt(c.head, c.unit), fmt(c.base, c.unit), delta(c), limitOf(c) + (c.unit === 'x' ? '' : ' ' + c.unit),
      MARK[c.status] + c.status + (c.why ? ': ' + c.why : '')].join(' | ') + ' |');
  });
  lines.push('', '</details>', '');
  lines.push('Playwright traces, one per surface on the phone (a cold load, not measured): the `lands-perf-head` artifact' +
    (opts.runUrl ? ' of [this run](' + opts.runUrl + ')' : '') + ', `traces/phone-<surface>.zip`; open one at https://trace.playwright.dev.');
  return lines.join('\n') + '\n';
}

/* The Commons v2 table (B12-B17, phone, cold): one row per workbench surface, one column per check, each cell its head
   value (change from the base) marked as in the by-surface table. Nothing when no Commons surface was graded. */
var COMMONS_COLUMNS = [['B12', ''], ['B12', 'rows'], ['B13', ''], ['B14', 'thread cold'], ['B14', 'thread warm'], ['B14', 'plan cold'], ['B14', 'plan warm'],
  ['B14', 'epic cold'], ['B14', 'epic warm'], ['B15', ''], ['B16', ''], ['B17', '']];
function commonsTable(res, lines) {
  var cs = res.checks.filter(function (c) { return +c.id.slice(1) >= 12; }), names = [];
  cs.forEach(function (c) { if (names.indexOf(c.surface) < 0) names.push(c.surface); });
  if (!names.length) return;
  lines.push('', '### Commons v2 (B12–B17, phone, cold)', '', 'Hard budgets: over the budget (or the line on a far bench) fails, whatever the base read.', '');
  lines.push('| Surface | ' + COMMONS_COLUMNS.map(function (k) {
    var b = BUDGETS.find(function (x) { return x.id === k[0] && (x.sub || '') === k[1]; });
    return k[0] + (k[1] ? ' ' + k[1] : '') + ' ≤ ' + fmt(b.limit, b.unit) + ' ' + b.unit;
  }).join(' | ') + ' |');
  lines.push('|---|' + COMMONS_COLUMNS.map(function () { return '--:'; }).join('|') + '|');
  names.forEach(function (s) {
    lines.push('| ' + s + ' | ' + COMMONS_COLUMNS.map(function (k) {
      var c = cs.find(function (x) { return x.surface === s && x.id === k[0] && x.sub === k[1]; });
      return c ? MARK[c.status] + fmt(c.head, c.unit) + (delta(c) ? ' (' + delta(c) + ')' : '') : '';
    }).join(' | ') + ' |');
  });
}

/* The header's sentence on B7's calibration floor, when a side ran the pan pass. */
function floorLine(res) {
  var f = res.floor || {};
  if ((f.head === null || f.head === undefined) && (f.base === null || f.base === undefined)) return '';
  var lim = FLOOR_SHARE * BUDGETS.find(function (x) { return x.floor; }).limit;
  return 'B7 floor (a bare full-screen canvas\'s longest task, ms): head ' + fmt(f.head, 'ms') + ', base ' + fmt(f.base, 'ms') + '; ' +
    (f.head !== null && f.head !== undefined && f.head <= lim ? 'within ' + lim + ' ms, so B7 is graded at the budget. ' :
      'over ' + lim + ' ms (no GPU raster), so B7 is graded on the pan\'s load against the base (fails over +' + PAN_MARGIN * 100 + ' % in every run). ');
}

function read(p) {
  if (!p) return null;
  try { return JSON.parse(fs.readFileSync(p, 'utf8')); } catch (e) { return null; }
}

/* Grade the files and write gate.md and gate.json into out. Returns { failed, md, gate }. No head results is a failure:
   the PR's measurement never finished. */
function run(headPath, basePath, out, runUrl) {
  var head = read(headPath), base = read(basePath), res, md;
  if (!head || !head.rows || !head.rows.length) {
    res = { checks: [], failed: 1, bench: {} };
    md = '<!-- lands-perf-gate -->\n## Lands perf gate: ❌ no results for this PR\n\nperf_baseline.js wrote no results for the PR head' +
      (runUrl ? ' ([run](' + runUrl + '))' : '') + ', so no budget is graded. The measure (head) job\'s log says why.\n';
  } else {
    res = gate(head, base && base.rows ? base : null);
    md = render(res, head, { runUrl: runUrl, noBase: !(base && base.rows) });
  }
  if (out) {
    fs.mkdirSync(out, { recursive: true });
    fs.writeFileSync(path.join(out, 'gate.md'), md);
    fs.writeFileSync(path.join(out, 'gate.json'), JSON.stringify(res, null, 1));
  }
  return { failed: res.failed, md: md, gate: res };
}

/* A Commons UX grade's perf rows (rubric §4; P3.2 gd-qeaDwFSDqq): one surface's results, no base, every check graded at
   the budget itself, as §7 says a rubric grade is (no 1.5x line, no ratchet): PASS within it, FAIL over it, UNGRADED
   with no value. A FAIL is marked noisy when it may be the node's and not the page's: a time (ms) on a bench not within
   BENCH_NEAR of the reference (an agent pod at load reads the desktop's times 3x P0.1's as well as the phone's), or a
   time whose fastest run is within the budget. Bytes, requests and heap are never noisy. Per budget id: FAIL when any of
   its checks fails, PASS when every one passed, else UNGRADED. */
function verdicts(head) {
  var res = gate(head, null), ids = [];
  var checks = res.checks.map(function (c) {
    var v = c.status === 'ungraded' ? 'UNGRADED' : c.head <= c.limit ? 'PASS' : 'FAIL';
    if (ids.indexOf(c.id) < 0) ids.push(c.id);
    return Object.assign({}, c, { verdict: v, noisy: v === 'FAIL' && c.unit === 'ms' && (!res.bench.near || (c.headBest !== null && c.headBest <= c.limit)) });
  });
  var budgets = {}, out = { checks: checks, budgets: budgets, pass: [], fail: [], ungraded: [], bench: res.bench };
  ids.forEach(function (id) {
    var vs = checks.filter(function (c) { return c.id === id; }).map(function (c) { return c.verdict; });
    budgets[id] = vs.indexOf('FAIL') >= 0 ? 'FAIL' : vs.every(function (v) { return v === 'PASS'; }) ? 'PASS' : 'UNGRADED';
    out[budgets[id] === 'FAIL' ? 'fail' : budgets[id] === 'PASS' ? 'pass' : 'ungraded'].push(id);
  });
  return out;
}

module.exports = { BUDGETS: BUDGETS, REFERENCE_BENCH: REFERENCE_BENCH, PAN_MARGIN: PAN_MARGIN, FLOOR_SHARE: FLOOR_SHARE, gate: gate, grade: grade, render: render, run: run, verdicts: verdicts, fmt: fmt };

if (require.main === module) {
  var args = process.argv.slice(2);
  if (args.length < 2 || args.length > 3) { console.error('usage: node perf_gate.js <head results.json> [<base results.json>] <out dir>'); process.exit(2); }
  var r = run(args[0], args.length === 3 ? args[1] : null, args[args.length - 1], process.env.LANDS_PERF_RUN_URL || '');
  console.log(r.md);
  process.exit(r.failed ? 1 : 0);
}
