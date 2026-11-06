/* The network inventory of every /garden surface (P0.4 of the Lands UX performance pass, gd-hsekIVjOWI), run from
   TestNetworkInventory (netinv_browser_test.go) against cardWorld's farm with the Commons live, on a real clock. Usage:
     node netinv_browser.js <base url> <node_modules dir> <out dir> <config json>
   config: {idleS, activeS, tickMs, blipS, outageS, parallel, surfaces: "a,b" or ""}.

   Each surface opens in its own Chromium context, every request it makes tagged X-Netinv-Surface so the server counts its
   bytes. Surfaces run `parallel` at a time; each batch goes through the same phases, which the server is told of so a
   long stream's bytes land in the phase they were sent in:
     load    navigate and wait for the surface (the golden row's show step)
     idle    idleS seconds, nothing moves
     active  activeS seconds, POST /test/netinv/tick every tickMs (the Go side says what a tick changes); the World pans
     blip    every connection cut once (POST /test/netinv/blip), then blipS seconds with the ticks going on
     outage  every connection cut and every request answered 502 for outageS seconds, then 60 s to recover, ticks going on
   Every request, response, SSE message and failure is read over CDP (the Network domain), so each is timed to the ms.
   Writes netinv.json (everything, raw events included) and netinv.md (the tables) to the out dir. */
'use strict';
var base = process.argv[2], mods = process.argv[3], out = process.argv[4], cfg = JSON.parse(process.argv[5] || '{}');
var fs = require('fs'), path = require('path');
var G = require('./golden_browser.js');   // the surfaces, opened and waited for the way the goldens do it
var pw;

var IDLE_S = cfg.idleS, ACTIVE_S = cfg.activeS, TICK_MS = cfg.tickMs || 5000, BLIP_S = cfg.blipS, OUTAGE_S = cfg.outageS;
var RECOVER_S = 60, PAN_MS = 15000, SNAP_MS = 10000;   // the outage's tail; the World's pan in the active phase; a re-snapshot's window

/* One row per surface: its golden row (path and show step), and what the viewer does in the active phase. */
var SURFACES = [
  { name: 'world', golden: 'opening', note: '/garden at the opening zoom: the World', active: pan },
  { name: 'commons', golden: 'commons', note: 'Commons: the workbench on an epic' },
  { name: 'commonsthread', golden: 'commonsthread', note: 'Commons: the workbench on a thread' },
  { name: 'bead', golden: 'bead', note: 'Workbench: the bead view of plan A' },
  { name: 'cockpit', golden: 'cockpit', note: 'Workbench: plan A\'s cockpit' },
  { name: 'queues', golden: 'queues', note: 'Workbench: the decisions queue (inbox)' },
  { name: 'project', golden: 'project', note: 'Workbench: the gardenio project panel' },
  { name: 'glass', golden: 'glass', note: 'Glass: a live session watched' },
  { name: 'wall', golden: 'wall', note: 'Glass: the Wall' },
];

async function pan(page, n) {
  await page.evaluate(function (n) { var c = window.GARDEN.look(); window.GARDEN.look({ cx: c.cx + (n % 2 ? -1 : 1) * 600 }, 600); }, n);
}

function rel(t0) { return function (ts) { return Math.round(ts * 1000 - t0); }; }
async function tell(what) {
  var r = await fetch(base + '/test/netinv/' + what, { method: 'POST' });
  if (!r.ok) throw new Error(what + ': ' + r.status);
}
function sleep(ms) { return new Promise(function (r) { setTimeout(r, ms); }); }
function log(s) { console.log(new Date().toISOString().slice(11, 19) + ' ' + s); }

/* route: the path with its ids folded, as the Go side folds them (netinv_browser_test.go route). */
function route(u) {
  var p = new URL(u).pathname.replace(/\/(gd|lb|ka)-[A-Za-z0-9]+/g, '/{id}');
  if (p.indexOf('/garden/c/') === 0) {
    var parts = p.slice('/garden/c/'.length).split('/');
    if (parts.length >= 2 && ['events', 'unread', 'bead', 'm'].indexOf(parts[0]) < 0) { parts[0] = '{land}'; parts[1] = '{channel}'; }
    p = '/garden/c/' + parts.join('/');
  }
  return p;
}

/* open: a context and page for s, recording over CDP into rec. */
async function open(browser, s, t0) {
  var ctx = await browser.newContext(Object.assign({}, G.devices(pw).desktop, { extraHTTPHeaders: { 'X-Netinv-Surface': s.name }, locale: 'en-US', timezoneId: 'UTC' }));
  var page = await ctx.newPage(), cdp = await ctx.newCDPSession(page), at = rel(t0);
  var rec = s.rec = { reqs: {}, order: [], errors: [] };
  // CDP timestamps are monotonic seconds; wallTime on requestWillBeSent anchors them to the run's clock
  var skew = null;
  function ms(ts) { return skew === null ? null : Math.round(ts * 1000 + skew - t0); }
  cdp.on('Network.requestWillBeSent', function (e) {
    if (skew === null) skew = e.wallTime * 1000 - e.timestamp * 1000;
    var u = e.request.url;
    if (u.indexOf(base) !== 0) return;   // data: URLs and the like
    var q = rec.reqs[e.requestId];
    if (q && e.redirectResponse) q.redirected = true;
    q = rec.reqs[e.requestId] = { id: e.requestId, url: u.slice(base.length), route: route(u), method: e.request.method, type: e.type, t: ms(e.timestamp), phase: s.phase, msgs: [], bytes: 0 };
    rec.order.push(q);
  });
  cdp.on('Network.responseReceived', function (e) { var q = rec.reqs[e.requestId]; if (q) { q.status = e.response.status; q.tResp = ms(e.timestamp); } });
  cdp.on('Network.dataReceived', function (e) { var q = rec.reqs[e.requestId]; if (q) q.bytes += e.encodedDataLength || 0; });
  cdp.on('Network.loadingFinished', function (e) { var q = rec.reqs[e.requestId]; if (q) { q.tEnd = ms(e.timestamp); q.wire = e.encodedDataLength; } });
  cdp.on('Network.loadingFailed', function (e) { var q = rec.reqs[e.requestId]; if (q) { q.tEnd = ms(e.timestamp); q.failed = e.canceled ? 'canceled' : e.errorText; } });
  cdp.on('Network.eventSourceMessageReceived', function (e) {
    var q = rec.reqs[e.requestId];
    if (q) q.msgs.push({ t: ms(e.timestamp), ev: e.eventName, id: e.eventId, n: (e.data || '').length, phase: s.phase });
  });
  await cdp.send('Network.enable');
  page.on('pageerror', function (e) { rec.errors.push(String(e)); });
  s.ctx = ctx; s.page = page; s.cdp = cdp; s.phase = 'load';
  var shot = G.SHOTS.filter(function (x) { return x.name === s.golden; })[0];
  s.path = shot.path;
  await page.goto(base + shot.path);
  await shot.show(page, 'desktop');
  return s;
}

async function batch(browser, list, t0, phases) {
  function mark(name) { list.forEach(function (s) { s.phase = name; }); phases.push({ name: name, t: Date.now() - t0, surfaces: list.map(function (s) { return s.name; }) }); }
  function end() { phases[phases.length - 1].end = Date.now() - t0; }
  await tell('phase?name=load');
  mark('load');
  await Promise.all(list.map(function (s) { return open(browser, s, t0); }));
  await sleep(5000);   // the opening fetches settle
  end();

  log('idle ' + IDLE_S + 's: ' + list.map(function (s) { return s.name; }).join(', '));
  await tell('phase?name=idle'); mark('idle');
  await sleep(IDLE_S * 1000); end();

  log('active ' + ACTIVE_S + 's');
  await tell('phase?name=active'); mark('active');
  var stop = Date.now() + ACTIVE_S * 1000, nextPan = Date.now() + PAN_MS, n = 0;
  while (Date.now() < stop) {
    await tell('tick');
    if (Date.now() >= nextPan) {
      nextPan += PAN_MS; n++;
      await Promise.all(list.map(function (s) { return s.active ? s.active(s.page, n).catch(function (e) { s.rec.errors.push('active: ' + e); }) : null; }));
    }
    await sleep(Math.max(0, Math.min(TICK_MS, stop - Date.now())));
  }
  end();

  async function ticking(secs) {
    var until = Date.now() + secs * 1000;
    while (Date.now() < until) { await tell('tick'); await sleep(Math.max(0, Math.min(TICK_MS, until - Date.now()))); }
  }
  log('blip, then ' + BLIP_S + 's');
  await tell('phase?name=blip'); mark('blip');
  phases[phases.length - 1].cut = Date.now() - t0;
  await tell('blip');
  await ticking(BLIP_S); end();

  log('outage ' + OUTAGE_S + 's, then ' + RECOVER_S + 's');
  await tell('phase?name=outage'); mark('outage');
  phases[phases.length - 1].cut = Date.now() - t0;
  phases[phases.length - 1].up = Date.now() - t0 + OUTAGE_S * 1000;
  await tell('outage?s=' + OUTAGE_S);
  await ticking(OUTAGE_S + RECOVER_S); end();

  await tell('phase?name=done');
  await Promise.all(list.map(function (s) { return s.ctx.close(); }));
}

/* ---------- analysis ---------- */
function inPhase(ph, t) { return t !== null && t >= ph.t && t < ph.end; }
function median(xs) { if (!xs.length) return null; var s = xs.slice().sort(function (a, b) { return a - b; }); return s[Math.floor(s.length / 2)]; }

/* per route in phase ph: requests started, their gaps, SSE messages by event name, and a stream's lifetime */
function routes(s, ph) {
  var by = {};
  s.rec.order.forEach(function (q) {
    var k = q.method + ' ' + q.route + (q.type === 'EventSource' ? ' (SSE)' : '');
    var r = by[k] || (by[k] = { route: k, sse: q.type === 'EventSource', starts: [], msgs: {}, msgBytes: 0, status: {} });
    if (inPhase(ph, q.t)) { r.starts.push(q.t); r.status[q.status || q.failed || 'pending'] = (r.status[q.status || q.failed || 'pending'] || 0) + 1; }
    q.msgs.forEach(function (m) { if (inPhase(ph, m.t)) { r.msgs[m.ev] = (r.msgs[m.ev] || 0) + 1; r.msgBytes += m.n; } });
  });
  return Object.keys(by).map(function (k) {
    var r = by[k], gaps = [];
    for (var i = 1; i < r.starts.length; i++) gaps.push(r.starts[i] - r.starts[i - 1]);
    r.count = r.starts.length; r.medianGapMs = median(gaps); delete r.starts;
    return r;
  }).filter(function (r) { return r.count || Object.keys(r.msgs).length; }).sort(function (a, b) { return a.route < b.route ? -1 : 1; });
}

/* What a cut did to each stream open at ph.cut: when it reconnected, what it asked for, what came first, which events it
   saw twice, and what the page fetched again within SNAP_MS. An outage adds the attempts made while the server was down
   and how long after it came back the stream was live again. */
function drop(s, ph) {
  var open = s.rec.order.filter(function (q) { return q.type === 'EventSource' && q.t !== null && q.t < ph.cut && (!q.tEnd || q.tEnd >= ph.cut - 100); });
  return open.map(function (old) {
    var after = s.rec.order.filter(function (q) { return q.type === 'EventSource' && q.route === old.route && q.t > ph.cut - 100 && q.t < ph.end; });
    var live = after.filter(function (q) { return q.status === 200 && (!ph.up || q.t >= ph.up - 100); })[0];
    var seen = {}, dup = [];
    old.msgs.forEach(function (m) { if (m.id) seen[m.ev + '#' + m.id] = true; });
    if (live) live.msgs.forEach(function (m) { if (m.id && seen[m.ev + '#' + m.id] && m.ev !== 'hb' && m.ev !== 'ping') dup.push(m.ev + '#' + m.id); });
    var tLive = live ? live.tResp || live.t : null;
    var first = live ? live.msgs.slice(0, 4).map(function (m) { return m.ev; }) : [];
    return {
      route: old.route, cutAt: old.tEnd, failed: old.failed || null,
      attempts: after.length, attemptStatus: after.map(function (q) { return q.status || q.failed || 'pending'; }),
      reconnectMs: live ? live.t - (ph.up || old.tEnd || ph.cut) : null, liveUrl: live ? live.url : null,
      tLive: tLive, firstEvents: first, reset: first.indexOf('reset') >= 0, duplicates: dup,
    };
  });
}

/* What the page fetched (no streams) from the cut to SNAP_MS after its last stream came back: its re-snapshot. */
function refetched(s, ph, streams) {
  var back = streams.reduce(function (a, d) { return d.tLive !== null && d.tLive > a ? d.tLive : a; }, ph.up || ph.cut), out = {};
  s.rec.order.forEach(function (q) {
    if (q.type !== 'EventSource' && q.t >= ph.cut && q.t < back + SNAP_MS) { var k = q.method + ' ' + q.route + (q.status ? ' ' + q.status : ''); out[k] = (out[k] || 0) + 1; }
  });
  return out;
}

(async function main() {
  pw = require(require.resolve('playwright-core', { paths: [mods] }));
  var browser;
  try { browser = await pw.chromium.launch(); }
  catch (e) { console.log('SKIP: ' + String(e.message || e).split('\n')[0]); process.exit(3); }
  var only = (cfg.surfaces || '').split(',').map(function (x) { return x.trim(); }).filter(Boolean);
  var list = SURFACES.filter(function (s) { return !only.length || only.indexOf(s.name) >= 0; });
  var t0 = Date.now(), phases = [], par = Math.max(1, cfg.parallel || 3);
  log('chromium ' + browser.version() + ', ' + list.length + ' surfaces, ' + par + ' at a time; idle ' + IDLE_S + 's, active ' + ACTIVE_S + 's, tick ' + TICK_MS + 'ms');
  try {
    for (var i = 0; i < list.length; i += par) await batch(browser, list.slice(i, i + par), t0, phases);
  } finally { await browser.close(); }
  var server = await (await fetch(base + '/test/netinv/bytes')).json();

  var result = { config: cfg, chromium: browser.version(), started: new Date(t0).toISOString(), phases: phases, actors: server.actors, ticks: server.ticks, surfaces: [] };
  list.forEach(function (s) {
    var mine = phases.filter(function (p) { return p.surfaces.indexOf(s.name) >= 0; }), r = { name: s.name, note: s.note, path: s.path, errors: s.rec.errors, phases: {} };
    mine.forEach(function (ph) {
      var rows = server.rows.filter(function (x) { return x.Surface === s.name && x.Phase === ph.name; });
      var bytes = rows.reduce(function (a, x) { return a + x.bytes; }, 0), mins = (ph.end - ph.t) / 60000;
      r.phases[ph.name] = {
        seconds: Math.round((ph.end - ph.t) / 1000), serverBytes: bytes, kbPerMin: +(bytes / 1024 / mins).toFixed(2),
        requests: s.rec.order.filter(function (q) { return inPhase(ph, q.t); }).length,
        server: rows.map(function (x) { return { method: x.Method, route: x.Route, requests: x.requests, bytes: x.bytes, status: x.Status }; }),
        routes: routes(s, ph),
      };
      if (ph.cut !== undefined) { r.phases[ph.name].drop = drop(s, ph); r.phases[ph.name].refetched = refetched(s, ph, r.phases[ph.name].drop); }
    });
    r.raw = s.rec.order.map(function (q) { return { url: q.url, method: q.method, type: q.type, t: q.t, phase: q.phase, status: q.status, tEnd: q.tEnd, failed: q.failed, wire: q.wire, msgs: q.msgs }; });
    result.surfaces.push(r);
  });
  fs.writeFileSync(path.join(out, 'netinv.json'), JSON.stringify(result, null, 1));
  fs.writeFileSync(path.join(out, 'netinv.md'), report(result));
  log('done: ' + path.join(out, 'netinv.md'));
})().catch(function (e) { console.error(e && e.stack || e); process.exit(1); });

/* ---------- the tables ---------- */
function report(R) {
  var L = [], kb = function (b) { return (b / 1024).toFixed(1); };
  L.push('# /garden network inventory', '');
  L.push('Chromium ' + R.chromium + ', run ' + R.started + '. Idle ' + R.config.idleS + ' s, active ' + R.config.activeS + ' s (a tick every ' + R.config.tickMs + ' ms: ' + R.ticks + ' ticks in all), blip then ' + R.config.blipS + ' s, outage ' + R.config.outageS + ' s then ' + RECOVER_S + ' s. Bytes are response bodies as the server wrote them (after the page\'s gzip, before TLS and headers).', '');
  L.push('## Per surface', '', '| Surface | Load: requests / KB | Idle: requests / KB/min | Active: requests / KB/min | Streams open |', '|---|---|---|---|---|');
  R.surfaces.forEach(function (s) {
    var p = s.phases, ld = p.load || {}, id = p.idle || {}, ac = p.active || {};
    var streams = (ac.routes || []).filter(function (r) { return r.sse; }).map(function (r) { return r.route.replace(' (SSE)', '').replace('GET ', ''); });
    L.push('| ' + s.name + ' | ' + (ld.requests || 0) + ' / ' + kb(ld.serverBytes || 0) + ' | ' + (id.requests || 0) + ' / ' + (id.kbPerMin || 0) + ' | ' + (ac.requests || 0) + ' / ' + (ac.kbPerMin || 0) + ' | ' + streams.join(', ') + ' |');
  });
  R.surfaces.forEach(function (s) {
    L.push('', '## ' + s.name + ' (' + s.note + ', ' + s.path + ')', '');
    if (s.errors.length) L.push('Page errors: ' + s.errors.join('; '), '');
    ['idle', 'active'].forEach(function (n) {
      var ph = s.phases[n];
      if (!ph) return;
      L.push('**' + n + '** (' + ph.seconds + ' s, ' + ph.requests + ' requests, ' + ph.kbPerMin + ' KB/min)', '', '| Route | Requests | Median gap | SSE events | Server KB |', '|---|---|---|---|---|');
      ph.routes.forEach(function (r) {
        var key = r.route.replace(' (SSE)', ''), sb = ph.server.filter(function (x) { return x.method + ' ' + x.route === key; }).reduce(function (a, x) { return a + x.bytes; }, 0);
        var ev = Object.keys(r.msgs).sort().map(function (e) { return e + ' ' + r.msgs[e]; }).join(', ');
        L.push('| ' + r.route + ' | ' + r.count + ' | ' + (r.medianGapMs === null ? '' : (r.medianGapMs / 1000).toFixed(1) + ' s') + ' | ' + ev + ' | ' + kb(sb) + ' |');
      });
      L.push('');
    });
    ['blip', 'outage'].forEach(function (n) {
      var ph = s.phases[n];
      if (!ph || !ph.drop) return;
      L.push('**' + n + '**: ' + ph.requests + ' requests in ' + ph.seconds + ' s. Reconnect is from the cut (blip) or from the server coming back (outage).', '', '| Stream | Attempts | Reconnect | First events | Reset | Duplicates |', '|---|---|---|---|---|---|');
      ph.drop.forEach(function (d) {
        L.push('| ' + d.route + ' | ' + d.attempts + ' (' + d.attemptStatus.join(' ') + ') | ' + (d.reconnectMs === null ? 'never' : (d.reconnectMs / 1000).toFixed(1) + ' s') + ' | ' + d.firstEvents.join(' ') + ' | ' + (d.reset ? 'yes' : 'no') + ' | ' + (d.duplicates.length ? d.duplicates.length + ' (' + d.duplicates.slice(0, 3).join(' ') + ')' : '0') + ' |');
      });
      var rf = Object.keys(ph.refetched).sort().map(function (k) { return k + ' ×' + ph.refetched[k]; }).join(', ');
      L.push('', 'Fetched again, from the cut to 10 s after the last stream came back: ' + (rf || 'nothing') + '.', '');
    });
  });
  return L.join('\n') + '\n';
}
