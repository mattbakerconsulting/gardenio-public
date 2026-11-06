/* live.js: the page's one EventSource (P2.4 gd-ymB1Pgg0fQ; server livemux.go). LIVE.source(url) is a stand-in EventSource;
   all ride one GET /garden/live/events (hello carries its sid), later opens and closes are POST /garden/live/sub. A message
   is "<key> <event>[ <id>]" then data. Full-jitter backoff (<1 s doubling to 30 s, reset after STEADY_MS): a dropped,
   refused or silent (WATCHDOG_MS) connection errors every stream and reopens; a stream ended or refused 5xx/408/429 is asked
   again, any other 4xx closes for good; always from its last id. The first connection waits for the page's load, so the
   streams its scripts open ride the GET, not a POST each. Without data-live: native EventSources.
   Woken (M3.2 gd-Xo3Stftjv6): no backoff's wait, a connection quiet STALE_MS replaced; #netbar: offline, or down GRACE_MS. */
var LIVE = (function () {
  'use strict';
  var BASE_MS = 1000, CAP_MS = 30000, STEADY_MS = 10000, WATCHDOG_MS = 45000, STALE_MS = 20000, GRACE_MS = 2000;
  /* pure, run under node by the Go tests. backoff: a wait in [0, min(CAP_MS, BASE_MS x 2^n)) */
  function backoff(n, rnd) { return Math.floor((rnd === undefined ? Math.random() : rnd) * Math.min(CAP_MS, BASE_MS * Math.pow(2, Math.min(n, 16)))); }
  /* one message: {key, type, id (null if none), data}, or null */
  function frame(d) {
    d = String(d === undefined || d === null ? '' : d);
    var nl = d.indexOf('\n'), head = nl < 0 ? d : d.slice(0, nl), a = head.indexOf(' ');
    if (a <= 0) return null;
    var b = head.indexOf(' ', a + 1);
    return { key: head.slice(0, a), type: b < 0 ? head.slice(a + 1) : head.slice(a + 1, b), id: b < 0 ? null : head.slice(b + 1), data: nl < 0 ? '' : d.slice(nl + 1) };
  }
  /* a url below the viewer root (a Glass stream's carries the root) */
  function rel(url, base) { url = String(url || ''); return base && url.indexOf(base + '/garden/') === 0 ? url.slice(base.length) : url; }
  function spec(key, last, url) { return key + ',' + encodeURIComponent(last || '') + ',' + url; }
  function retryable(status) { return !(status >= 400 && status < 500) || status === 408 || status === 429; }
  /* doc with part p laid on (gd-HprlA40a7z): a list grows by p's, an object takes p's keys, the rest is p's */
  function patch(doc, p) {
    var cp = function (o, t) { for (var k in o) t[k] = o[k]; return t; }, d = cp(doc, {});
    for (var k in p) { var v = p[k]; d[k] = Array.isArray(v) ? (d[k] || []).concat(v) : v && typeof v === 'object' ? cp(v, cp(d[k], {})) : v; }
    return d;
  }

  var api = { backoff: backoff, frame: frame, rel: rel, spec: spec, retryable: retryable, patch: patch };
  if (typeof document === 'undefined' || !document.body) return api;

  var B = document.body, ds = B.dataset || {}, base = ds.base || '', shared = ds.live === '1' && typeof EventSource !== 'undefined';
  var subs = {}, next = 1, es = null, sid = '', have = {}, openedAt = 0, heard = 0, tries = 0, timer = 0, queued = false, posting = false, again = false;
  var lostAt = 0, since = 0;   // when it went (0: up), when last heard
  function report(x) { setTimeout(function () { throw x; }, 0); }
  function offline() { return typeof navigator !== 'undefined' && navigator.onLine === false; }
  function paint() {
    var bar = document.getElementById && document.getElementById('netbar');
    if (!bar) return;
    var off = offline(), show = !!lostAt && (off || shared && Object.keys(subs).length > 0 && Date.now() - lostAt >= GRACE_MS);
    if (show) {
      var d = new Date(since || lostAt), at = ('0' + d.getHours()).slice(-2) + ':' + ('0' + d.getMinutes()).slice(-2);
      bar.textContent = (off ? 'Offline' : 'Reconnecting') + ' · as of ' + at;
    }
    bar.hidden = !show;
  }
  function lose() { if (!lostAt) { lostAt = Date.now(); since = heard || lostAt; setTimeout(paint, GRACE_MS + 50); } paint(); }

  /* one stream as its surface sees it; key is never reused */
  function Source(url) {
    this.url = url; this.readyState = 0;
    this.key = (next++).toString(36); this.last = ''; this.fails = 0; this.since = 0; this.waiting = false; this.ls = {};
  }
  Source.prototype.addEventListener = function (type, fn) { if (fn) (this.ls[type] = this.ls[type] || []).push(fn); };
  Source.prototype.close = function () { if (this.readyState === 2) return; this.readyState = 2; if (subs[this.key] === this) delete subs[this.key]; sync(); };
  function fire(s, type, data) {
    var e = { type: type, data: data === undefined ? '' : data, lastEventId: s.last, target: s };
    [s['on' + type]].concat(s.ls[type] || []).forEach(function (fn) { if (fn) try { fn.call(s, e); } catch (x) { report(x); } });
  }

  function source(url) {
    if (!shared) return new EventSource(url);
    var s = new Source(url);
    subs[s.key] = s;
    sync();
    return s;
  }
  function wanted() { return Object.keys(subs).filter(function (k) { return !subs[k].waiting; }); }
  function sync() {   // one request per turn; the first once the page has loaded
    if (queued) return;
    queued = true;
    if (es || !/loading|interactive/.test(document.readyState)) setTimeout(run, 0);
    else window.addEventListener('load', function () { setTimeout(run, 0); });
  }
  function run() {
    queued = false;
    var want = wanted();
    if (!want.length) { if (!Object.keys(subs).length) shut(); return; }
    if (!es) { if (!timer) open(want); return; }
    post();
  }
  function open(want) {
    shut();
    heard = openedAt = Date.now();
    var q = want.map(function (k) { have[k] = 1; return 's=' + encodeURIComponent(spec(k, subs[k].last, rel(subs[k].url, base))); }).join('&');
    var my = es = new EventSource(base + '/garden/live/events?' + q);
    my.onmessage = function (e) { if (es === my) take(e.data); };
    my.onerror = function () { if (es === my) drop(); };
  }
  function shut() { if (es) { es.close(); es = null; } sid = ''; have = {}; }
  function drop() {
    if (sid && Date.now() - openedAt >= STEADY_MS) tries = 0;
    shut();
    lose();
    Object.keys(subs).forEach(function (k) { var s = subs[k]; if (s && !s.waiting) { s.readyState = 0; s.since = 0; fire(s, 'error'); } });
    if (!timer) timer = setTimeout(function () { timer = 0; run(); }, backoff(tries++));
  }
  function take(d) {
    heard = Date.now();
    var f = frame(d);
    if (!f) return;
    if (f.key === '*') {
      if (f.type === ':hello') { try { sid = JSON.parse(f.data).sid || ''; } catch (x) { sid = ''; } lostAt = 0; paint(); post(); }
      return;
    }
    var s = subs[f.key];
    if (!s || !have[f.key]) return;
    if (f.type === ':open') { s.readyState = 1; s.since = Date.now(); fire(s, 'open'); return; }
    if (f.type === ':err' || f.type === ':end') { lost(s, f.type === ':err' ? +f.id || 502 : 0); return; }
    if (f.id !== null) s.last = f.id;
    fire(s, f.type, f.data);
  }
  /* one stream ended (status 0) or was refused */
  function lost(s, status) {
    delete have[s.key];
    if (status && !retryable(status)) { s.readyState = 2; delete subs[s.key]; fire(s, 'error'); sync(); return; }
    if (s.since && Date.now() - s.since >= STEADY_MS) s.fails = 0;
    s.readyState = 0; s.since = 0;
    fire(s, 'error');
    if (s.readyState === 2) return;
    s.waiting = true;
    setTimeout(function () { s.waiting = false; if (subs[s.key] === s) sync(); }, backoff(s.fails++));
  }
  /* what changed, one request at a time; a refused one (sid gone, or another replica's) reopens */
  function post() {
    if (!es || !sid) return;
    if (posting) { again = true; return; }
    var add = [], rm = [];
    wanted().forEach(function (k) { if (!have[k]) add.push({ k: k, l: subs[k].last || undefined, u: rel(subs[k].url, base) }); });
    Object.keys(have).forEach(function (k) { if (!subs[k]) rm.push(k); });
    if (!add.length && !rm.length) return;
    add.forEach(function (a) { have[a.k] = 1; });
    rm.forEach(function (k) { delete have[k]; });
    posting = true;
    var my = es;
    fetch(base + '/garden/live/sub', { method: 'POST', credentials: 'same-origin', cache: 'no-store', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ sid: sid, add: add, rm: rm }) })
      .then(function (r) { if (!r.ok && es === my) drop(); }, function () { if (es === my) drop(); })
      .then(function () { posting = false; if (again) { again = false; run(); } });
  }
  /* dead without an error: nothing, heartbeats included, for WATCHDOG_MS */
  setInterval(function () { if (es && Object.keys(have).length && Date.now() - heard > WATCHDOG_MS) drop(); }, 5000);
  function wake(e) {
    if (document.hidden) return;
    if (!shared) { if (e.type === 'online') lostAt = 0; paint(); return; }
    if (es && (e.type === 'online' || Date.now() - heard > STALE_MS)) drop();
    if (timer) { clearTimeout(timer); timer = 0; tries = 0; run(); }
    paint();
  }
  if (typeof window !== 'undefined' && window.addEventListener && document.addEventListener) {
    window.addEventListener('online', wake);
    window.addEventListener('offline', function () { if (es) drop(); else lose(); });
    window.addEventListener('pageshow', function (e) { if (e.persisted) wake(e); });
    document.addEventListener('visibilitychange', wake);
  }

  api.source = source;
  api.shared = function () { return shared; };
  return api;
})();
if (typeof module !== 'undefined') module.exports = LIVE;
