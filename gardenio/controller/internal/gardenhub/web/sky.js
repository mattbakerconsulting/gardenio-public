/* /garden sky (gd-SwRqFcjWc6): day or night for the whole page, from one setting. The farm has a real night (art.js paints a
   night palette, garden.css a night theme), but until this it was reached only through the OS dark preference. The setting,
   garden.sky in localStorage, is one of
     auto   (the default) night by the viewer's clock from DUSK to DAWN local time, or whenever the OS prefers dark; else day
     day    always day, even on a dark OS
     night  always night
   The resolved sky is html[data-sky="day"|"night"], set here before the first paint (this script is in <head>, after the
   stylesheet); garden.css keys its night tokens on it and a page with no script falls back to the OS preference. Scripts
   read SKY.night() and subscribe with SKY.on(fn): the scene, the Porch sprites and the plans slot
   repaint on a flip. A timer set for the next dusk or dawn, a flip of the OS preference, a tab coming back to the front and
   the Menu's Sky item all re-resolve. Under node (the Go tests) only the pure part is built. */
var SKY = (function () {
  var DUSK = 19, DAWN = 7, KEY = 'garden.sky', MODES = ['auto', 'day', 'night'];
  function isNightHour(h) { return h >= DUSK || h < DAWN; }
  /* resolve(mode, hour, osDark) -> 'day' | 'night'. Pure; TestSkyLogicUnderNode walks it. */
  function resolve(mode, hour, osDark) {
    if (mode === 'day' || mode === 'night') return mode;
    return (isNightHour(hour) || osDark) ? 'night' : 'day';
  }
  /* msToFlip(d) -> milliseconds from Date d to the next dusk or dawn in local time, so the auto timer fires on the boundary
     (through a DST change too: the boundary is set as a wall-clock hour, not added as a duration). */
  function msToFlip(d) {
    var h = d.getHours(), to = (h >= DAWN && h < DUSK) ? DUSK : DAWN, next = new Date(d.getTime());
    next.setHours(to, 0, 0, 0);
    if (next.getTime() <= d.getTime()) next.setDate(next.getDate() + 1);
    return next.getTime() - d.getTime();
  }
  var api = { DUSK: DUSK, DAWN: DAWN, KEY: KEY, MODES: MODES, isNightHour: isNightHour, resolve: resolve, msToFlip: msToFlip };
  if (typeof document === 'undefined') return api;

  var root = document.documentElement, mode = 'auto', cur = '', subs = [], timer = 0;
  try { var saved = localStorage.getItem(KEY); if (saved === 'day' || saved === 'night') mode = saved; } catch (e) { /* private mode: auto */ }
  var mq = window.matchMedia ? window.matchMedia('(prefers-color-scheme: dark)') : null;
  function tell() { for (var i = 0; i < subs.length; i++) { try { subs[i](cur, mode); } catch (e) { /* one listener must not stop the rest */ } } }
  /* apply(force): resolve now, arm the timer for the next boundary when auto, and tell the subscribers when the sky changed
     (or when force says the mode did, so a label can follow). */
  function apply(force) {
    var s = resolve(mode, new Date().getHours(), !!(mq && mq.matches));
    if (timer) { clearTimeout(timer); timer = 0; }
    if (mode === 'auto') timer = setTimeout(function () { timer = 0; apply(false); }, msToFlip(new Date()) + 50);
    if (s === cur && !force) return;
    cur = s; root.setAttribute('data-sky', s);
    tell();
  }
  api.mode = function () { return mode; };
  api.sky = function () { return cur; };
  api.night = function () { return cur === 'night'; };
  api.set = function (m) {
    if (MODES.indexOf(m) < 0) m = 'auto';
    mode = m;
    try { if (m === 'auto') localStorage.removeItem(KEY); else localStorage.setItem(KEY, m); } catch (e) { /* a per-viewer convenience only */ }
    apply(true);
  };
  api.cycle = function () { api.set(MODES[(MODES.indexOf(mode) + 1) % MODES.length]); return mode; };
  api.on = function (f) { subs.push(f); };
  /* label(el): the Menu's Sky item (3b R10) says the mode in its .sky-t word and in its accessible name and title, and follows
     every change, the clock's flips included. garden.js wires the click to SKY.cycle(). */
  api.label = function (el) {
    var word = el.querySelector('.sky-t');
    function say() {
      var n = cur === 'night', next = { auto: 'always day', day: 'always night', night: 'automatic' }[mode];
      word.textContent = mode === 'auto' ? 'Auto' : mode === 'day' ? 'Day' : 'Night';
      el.setAttribute('aria-label', 'Sky: ' + (mode === 'auto' ? 'automatic, ' + (n ? 'night' : 'day') + ' now' : 'always ' + mode) + '. Press for ' + next);
      el.title = mode === 'auto' ? 'Night by your clock, ' + DUSK + ':00 to ' + (DAWN < 10 ? '0' : '') + DAWN + ':00, or when your device prefers dark' : 'Always ' + mode;
    }
    say(); subs.push(say);
  };
  if (mq) { var os = function () { apply(false); }; if (mq.addEventListener) mq.addEventListener('change', os); else if (mq.addListener) mq.addListener(os); }
  document.addEventListener('visibilitychange', function () { if (!document.hidden) apply(false); });   // background tabs throttle timers; catch up on return
  apply(false);
  return api;
})();
if (typeof module !== 'undefined') module.exports = SKY;
