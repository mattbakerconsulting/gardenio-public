/* The Window's page (window.go, templates/window.html; Lands: the Window PW.3). Served as /w/static/window.js after the
   skin clock, the page's only script (its CSP allows no inline script). Every 20 s it asks plan.json whether the plan
   changed, sending the last ETag so an unchanged plan is a 304 that costs the daemon nothing. When it changed, it
   fetches this page again (?refresh=1, which the viewer does not count as a view), swaps the body of <main> for the
   fresh render and marks the Recent rows that are new. In between it keeps the "3 min ago" words current. The page is
   complete without it.
   Live (PW.7, window_live.go): once it knows the ETag it opens /w/<token>/live, an event stream whose "changed" frames
   carry plan.json's new ETag about a second after the plan changed, and whose "heartbeat" frames (every 15 s) say the
   stream is flowing. The 20 s poll stands down only while a frame arrived within STALE, so a stream that is open but
   silent (an edge buffering it, a daemon gone quiet) leaves the page polling; the stream is closed while the tab is
   hidden. "updated" is stamped only on evidence: a 304, a render, or a frame. When the stream is refused (a cap, a
   land window), lost for good, or the browser has no EventSource, the poll carries on as before; after LIVE_TRIES
   such failures the page only polls. A "gone" frame (the window ended) closes the stream for good. */
(function () {
  'use strict';
  var EVERY = 20000;
  var LIVE_TRIES = 3;
  var STALE = 40000; // a little over two heartbeats
  var page = location.pathname.replace(/\/+$/, '');
  var etag = '';
  var live = null;
  var liveFailures = 0;
  var heard = 0; // when the stream last sent a frame

  // ago says how long before now t was, as window.go's windowAgo does.
  function ago(t, now) {
    var d = now - t, min = 60000, hour = 60 * min, day = 24 * hour;
    if (d < min) return 'just now';
    if (d < hour) return Math.floor(d / min) + ' min ago';
    if (d < day) return Math.floor(d / hour) + ' h ago';
    if (d < 30 * day) return Math.floor(d / day) + ' d ago';
    var dt = new Date(t);
    var months = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
    return dt.getUTCDate() + ' ' + months[dt.getUTCMonth()] + ' ' + dt.getUTCFullYear();
  }

  function tick() {
    var now = Date.now();
    document.querySelectorAll('time[data-ago]').forEach(function (el) {
      var t = Date.parse(el.getAttribute('datetime'));
      if (!isNaN(t)) el.textContent = ago(t, now);
    });
  }

  function keys(root) {
    var seen = {};
    root.querySelectorAll('#w-recent li[data-key]').forEach(function (li) { seen[li.getAttribute('data-key')] = true; });
    return seen;
  }

  function swap(html) {
    var main = document.getElementById('w-main');
    var fresh = new DOMParser().parseFromString(html, 'text/html').getElementById('w-main');
    if (!main || !fresh) return;
    var before = keys(main);
    main.replaceChildren.apply(main, Array.prototype.map.call(fresh.childNodes, function (n) { return document.importNode(n, true); }));
    main.querySelectorAll('#w-recent li[data-key]').forEach(function (li) {
      if (!before[li.getAttribute('data-key')]) li.classList.add('w-new');
    });
  }

  // stamp says the page is current as of now.
  function stamp() {
    var updated = document.getElementById('w-updated');
    if (updated) updated.setAttribute('datetime', new Date().toISOString());
    tick();
  }

  // render fetches the page again and swaps it in; it resolves true when it did.
  function render() {
    return fetch(page + '?refresh=1', { cache: 'no-store', credentials: 'omit' }).then(function (r) {
      return r.ok ? r.text() : null;
    }).then(function (html) {
      if (!html) return false;
      swap(html);
      stamp();
      return true;
    });
  }

  // streaming is true while the live stream is open and has sent a frame within STALE; the poll stands down.
  function streaming() {
    return live !== null && live.readyState === 1 && Date.now() - heard < STALE;
  }

  // hear notes a frame from the stream: evidence it is flowing and the page is current.
  function hear() {
    heard = Date.now();
    stamp();
  }

  function closeLive() {
    if (live) live.close();
    live = null;
  }

  function openLive() {
    if (live || !etag || document.hidden || !window.EventSource || liveFailures >= LIVE_TRIES) return;
    var es = new EventSource(page + '/live');
    live = es;
    heard = 0;
    // A (re)opened stream counts only once it sends something.
    es.addEventListener('open', function () { heard = 0; });
    es.addEventListener('heartbeat', hear);
    es.addEventListener('changed', function (e) {
      var tag = e.data;
      if (!tag || tag === etag) { hear(); return; }
      heard = Date.now();
      render().then(function (ok) { if (ok) etag = tag; }).catch(function () { /* the next change tries again */ });
    });
    es.addEventListener('gone', function () {
      if (live !== es) return;
      closeLive();
      liveFailures = LIVE_TRIES;
    });
    es.addEventListener('error', function () {
      // While CONNECTING the browser retries on its own (the stream ended after its lifetime); CLOSED is a refusal.
      if (es.readyState !== 2 || live !== es) return;
      closeLive();
      liveFailures++;
    });
  }

  function refresh() {
    if (document.hidden || streaming()) return;
    var headers = etag ? { 'If-None-Match': etag } : {};
    fetch(page + '/plan.json', { headers: headers, cache: 'no-store', credentials: 'omit' }).then(function (res) {
      if (res.status === 304) { stamp(); return; }
      if (!res.ok) return; // not evidence: the stamp stays as it was
      var first = !etag;
      etag = res.headers.get('ETag') || '';
      if (first) { stamp(); return; } // the page already shows this ETag
      return render();
    }).then(function () {
      openLive();
    }).catch(function () { /* the next round tries again */ });
  }

  document.addEventListener('DOMContentLoaded', function () {
    if (!document.getElementById('w-main')) return;
    // The first plan.json read only learns the ETag of what the page already shows.
    refresh();
    // While streaming, frames stamp the page; the interval only keeps the "ago" words current.
    setInterval(function () { if (streaming()) tick(); else refresh(); }, EVERY);
    document.addEventListener('visibilitychange', function () {
      if (document.hidden) closeLive(); else refresh();
    });
  });
})();
