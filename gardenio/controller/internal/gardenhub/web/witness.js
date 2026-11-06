/* WITNESS: what the browser did to /garden, said on the page (gd-TNC80dPgv3). A phone "refresh" on a full zoom-out that no
   desktop and no Chromium emulation shows has four different possible shapes, and only the phone can tell them apart: a real
   load again (reload, or a back/forward restore), a tab the engine killed and loaded afresh, a system gesture that hid the
   page (the stream reconnects and the farm is laid out again on return), or a camera cut from a viewport resize or a cancelled
   touch. Off by default. /garden?witness=1 turns it on for this browser (localStorage), ?witness=0 turns it off. On, it keeps
   the last LOG_N events and the camera once a second in sessionStorage (which survives a reload and a kill, never a new tab),
   says each event in the page's toast as it happens, and on a boot says how the page came back and what it was doing before.
   flag(search, stored) and boot(prev, nav, now) are pure and run under node (witness_test.go). */
var WITNESS = (function () {
  'use strict';
  var KEY = 'garden.witness', LOG = 'garden.witness.log', LOG_N = 12, EVERY = 1000;
  /* flag(search, stored) -> 'on' | 'off' | '': what the query says (witness=1 / witness=0), else what was stored. */
  function flag(search, stored) {
    var m = /[?&]witness=([^&#]*)/.exec(search || '');
    if (m) return m[1] === '1' || m[1] === 'on' ? 'on' : 'off';
    return stored === 'on' ? 'on' : '';
  }
  /* boot(prev, nav, now) -> the words for a boot: prev is the last run's record ({boots, hid, at, cam, log}), nav the
     navigation entry type ('navigate' | 'reload' | 'back_forward' | 'prerender'), now the time. */
  function boot(prev, nav, now) {
    var n = (prev && prev.boots || 0) + 1;
    if (n === 1) return { boots: n, say: 'witness on: this is the first load in this tab' };
    var how = nav === 'reload' ? 'reloaded' : nav === 'back_forward' ? 'came back through history' : 'loaded again (' + (nav || 'navigate') + ')';
    var end = prev.hid ? 'the page was closed first (pagehide seen)' : 'the page was NOT closed first: the engine killed it';
    var ago = prev.at ? Math.round((now - prev.at) / 1000) + 's ago' : 'time unknown';
    return { boots: n, say: 'witness: load #' + n + ', ' + how + ', ' + end + '; last seen ' + ago + (prev.cam ? ' at level ' + prev.cam.level + ' z ' + prev.cam.z : '') + (prev.log && prev.log.length ? '; last events: ' + prev.log.slice(-3).map(function (l) { return l.replace(/^\d+ /, ''); }).join(' / ') : '') };
  }
  var api = { flag: flag, boot: boot, KEY: KEY, LOG: LOG };
  if (typeof document === 'undefined' || !document.addEventListener) return api;

  /* ---------- the page's side ---------- */
  var on = '';
  try { on = flag(location.search, localStorage.getItem(KEY)); if (/[?&]witness=/.test(location.search)) localStorage.setItem(KEY, on || 'off'); } catch (e) { on = ''; }
  if (on !== 'on') return api;
  var rec = null, toastEl = null, timer = 0, said = [];
  try { rec = JSON.parse(sessionStorage.getItem(LOG) || 'null'); } catch (e) { rec = null; }
  var navs = (window.performance && performance.getEntriesByType) ? performance.getEntriesByType('navigation') : [], nav = navs.length ? navs[navs.length - 1].type : '';
  var b = boot(rec, nav, Date.now());
  rec = { boots: b.boots, hid: false, at: Date.now(), cam: null, last: '', log: (rec && rec.log) || [] };
  function save() { try { sessionStorage.setItem(LOG, JSON.stringify(rec)); } catch (e) { /* storage full or off */ } }
  function say(msg) {
    said.push(msg); if (said.length > 3) said.shift();
    toastEl = toastEl || document.getElementById('toast');
    if (!toastEl) return;
    toastEl.textContent = said.join(' · '); toastEl.hidden = false;
    clearTimeout(timer); timer = setTimeout(function () { toastEl.hidden = true; said = []; }, 8000);
  }
  function note(what) {
    var c = window.GARDEN && GARDEN.cam ? GARDEN.cam() : null, line = what + (c ? ' @L' + c.level + ' z' + (Math.round(c.z * 1000) / 1000) : '');
    rec.last = line; rec.at = Date.now(); rec.log.push(Math.round(Date.now() / 1000) % 100000 + ' ' + line); if (rec.log.length > LOG_N) rec.log.shift(); save();
    say(line);
  }
  document.addEventListener('DOMContentLoaded', function () { say(b.say); });
  if (document.readyState !== 'loading') say(b.say);
  save();
  document.addEventListener('visibilitychange', function () { note(document.hidden ? 'hidden' : 'visible again'); });
  window.addEventListener('pagehide', function (e) { rec.hid = true; rec.kept = !!e.persisted; rec.at = Date.now(); save(); });   // the last event stays what it was: the pagehide is the verdict's 'closed first'
  window.addEventListener('pageshow', function (e) { if (e.persisted) note('restored from back/forward cache'); });
  window.addEventListener('popstate', function () { note('history popped to ' + location.pathname + location.search + location.hash); });
  window.addEventListener('resize', function () { note('window ' + window.innerWidth + 'x' + window.innerHeight); });
  if (window.visualViewport) {
    var vv = window.visualViewport, sc = vv.scale;
    vv.addEventListener('resize', function () { if (Math.abs(vv.scale - sc) > 0.01) { sc = vv.scale; note('page pinch scale ' + Math.round(sc * 100) / 100); } });
  }
  document.addEventListener('farmlive', function (e) { var k = e.detail && e.detail.kind; if (k === 'reset' || k === 'open') note('farm stream ' + k); });
  window.addEventListener('error', function (e) { note('error: ' + String(e.message || '').slice(0, 60)); });
  document.addEventListener('DOMContentLoaded', function () {
    var board = document.getElementById('board');
    if (board) board.addEventListener('pointercancel', function (e) { note('touch cancelled by the browser (' + e.pointerType + ')'); });
  });
  setInterval(function () { var c = window.GARDEN && GARDEN.cam ? GARDEN.cam() : null; if (c) { rec.cam = { level: c.level, z: Math.round(c.z * 1000) / 1000 }; rec.at = Date.now(); save(); } }, EVERY);
  return api;
})();
if (typeof module !== 'undefined') module.exports = WITNESS;
