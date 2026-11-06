package main

// skinClockJS is the viewer's time of day (gd-yV1HX7zGNE, plan step 3.3):
// night on the viewer is the dark-homestead skin, so with no stored skin the
// sky follows the local clock the way /garden's does. The rule is
// internal/gardenhub/web/sky.js's verbatim: night from DUSK to DAWN local
// time, or whenever the OS prefers dark; else day. A stored skin
// (beads-viewer:skin) is the explicit override and wins outright.
//
// layout.html, login.html and lands.html inline it in <head> through the
// skinClockJS template func and call SKIN_CLOCK.start(day, night) before the
// first paint: the viewer, the sign-in page and the Lands front door all pass
// homestead/dark-homestead (the front door since plan step 4.1), so all three
// agree on when it is night and on how it looks. A timer to the next
// dusk or dawn, a flip of the OS preference and a tab coming back to the front
// re-resolve, so an open tab crosses dusk without a reload; the attribute is
// on <html>, which htmx swaps never touch. The switcher's Auto entry is
// SKIN_CLOCK.set('auto'), which forgets the stored skin.
//
// The season rides along (gd-WP5vygf7Ly, plan step 3.4): every apply() stamps
// html[data-season] from the local month, which homestead.css keys the vine's
// tint and the meadow's accent on. Northern hemisphere, no setting yet. It is
// set whether or not a skin is stored, and an open tab picks up a new month at
// the next dusk, dawn or return to the front.
//
// The stored preference (skin_pref.go, gd-Ah9Zr8CxYB, plan step 4.2): the
// viewer's layout passes the user bead's skin as start()'s third argument.
// When there is one it wins over the cache and is written into it, so a new
// device paints the chosen skin from the first frame and the next page with no
// viewer (/login) paints it too. With none ("" for never chosen, or no
// argument on /login and /lands) the cache decides, as before.
//
// The PWA chrome follows the sky: every paint retints <meta name="theme-color">
// to the top band of the Homestead sky, day or night (SKY, the same colours
// as skin_pref.go's homesteadSky* and meadow.css's .gd-sky), and any other
// skin gets the slate the viewer wore before (CHROME). Pages without the meta
// skip it.
//
// Under node (TestSkinClockUnderNode) only the pure part is built: resolve(),
// msToFlip() and seasonOf(). The server-rendered data-skin stays the no-JS
// default, and with no script there is no data-season (the plain meadow).
const skinClockJS = `var SKIN_CLOCK = (function () {
  var DUSK = 19, DAWN = 7, KEY = 'beads-viewer:skin';
  var SKY = { 'homestead': '#5f98d6', 'dark-homestead': '#0b1030' }, CHROME = '#1e2f42';
  var SEASONS = ['winter', 'winter', 'spring', 'spring', 'spring', 'summer', 'summer', 'summer', 'autumn', 'autumn', 'autumn', 'winter'];
  function isNightHour(h) { return h >= DUSK || h < DAWN; }
  /* seasonOf(month) -> the season for a Date.getMonth() month (0 = January): Mar-May spring, Jun-Aug summer, Sep-Nov autumn, Dec-Feb winter. Pure. */
  function seasonOf(m) { return SEASONS[m]; }
  /* resolve(hour, osDark, day, night) -> the skin for that hour: night (default dark-homestead) or day (default homestead). Pure. */
  function resolve(hour, osDark, day, night) {
    return (isNightHour(hour) || osDark) ? (night || 'dark-homestead') : (day || 'homestead');
  }
  /* msToFlip(d) -> milliseconds from Date d to the next dusk or dawn in local wall-clock time (sky.js's, so DST is honoured). */
  function msToFlip(d) {
    var h = d.getHours(), to = (h >= DAWN && h < DUSK) ? DUSK : DAWN, next = new Date(d.getTime());
    next.setHours(to, 0, 0, 0);
    if (next.getTime() <= d.getTime()) next.setDate(next.getDate() + 1);
    return next.getTime() - d.getTime();
  }
  /* chromeOf(skin) -> the theme-color for a skin: its sky's top band, or the old slate. Pure. */
  function chromeOf(s) { return Object.prototype.hasOwnProperty.call(SKY, s) ? SKY[s] : CHROME; }
  var api = { DUSK: DUSK, DAWN: DAWN, KEY: KEY, isNightHour: isNightHour, resolve: resolve, msToFlip: msToFlip, seasonOf: seasonOf, chromeOf: chromeOf };
  if (typeof document === 'undefined') return api;

  var root = document.documentElement, meta = document.querySelector('meta[name="theme-color"]'), pick = '', day = '', night = '', timer = 0, started = false;
  var mq = window.matchMedia ? window.matchMedia('(prefers-color-scheme: dark)') : null;
  try { pick = localStorage.getItem(KEY) || ''; } catch (e) { /* private mode / disabled storage: auto */ }
  /* paint(skin): the skin on <html>, and the PWA chrome to match. */
  function paint(s) {
    root.setAttribute('data-skin', s);
    if (meta) meta.setAttribute('content', chromeOf(s));
  }
  /* apply(): the season from the month; then the stored skin when there is one (the override), else the clock's, with the timer armed for the next boundary. */
  function apply() {
    if (timer) { clearTimeout(timer); timer = 0; }
    root.setAttribute('data-season', seasonOf(new Date().getMonth()));
    if (pick) { paint(pick); return; }
    paint(resolve(new Date().getHours(), !!(mq && mq.matches), day, night));
    timer = setTimeout(function () { timer = 0; apply(); }, msToFlip(new Date()) + 50);
  }
  /* store(skin): a named skin is cached and wins; 'auto' (or nothing) removes the cached key and hands the sky back to the clock. */
  function store(s) {
    pick = (s && s !== 'auto') ? s : '';
    try { if (pick) localStorage.setItem(KEY, pick); else localStorage.removeItem(KEY); } catch (e) { /* a per-viewer convenience only */ }
  }
  api.auto = function () { return !pick; };
  api.set = function (s) { store(s); apply(); };
  /* start(day, night, saved): saved is the viewer's stored preference, when the page knows one; it wins over the cache and refreshes it. */
  api.start = function (d, n, saved) {
    day = d || ''; night = n || '';
    if (saved) store(saved);
    apply();
    if (started) return;
    started = true;
    if (mq) { if (mq.addEventListener) mq.addEventListener('change', apply); else if (mq.addListener) mq.addListener(apply); }
    document.addEventListener('visibilitychange', function () { if (!document.hidden) apply(); });   // background tabs throttle timers; catch up on return
  };
  return api;
})();
if (typeof module !== 'undefined') module.exports = SKIN_CLOCK;`
