/* QUALITY: the splash's quality tiers (plan gd-EJGA3zDGCk, step 9), so a laptop draws the whole scene in its 16 ms and a phone
   a lighter one in its 33. Three tiers, each a set of knobs the scene's parts read (TIERS): high, 60,000 grass blades, two
   shadow cascades (2048 px round the stone, 1024 over the valley), the pond a planar mirror, up to 2 device px a CSS px;
   mid, 25,000 blades, one 2048 px cascade over it all, the pond a mirror colour that follows the sky, 1.5 px; low, 8,000
   blades, one 1024 px cascade, the pond its own water with no reflection, 1 px, and the poplars and the orchard drawn as
   impostor cards like the far woods.

   The page picks a tier at start (pick): ?q=low|mid|high if the address says, else a guess from the device (guess: a phone
   low, a laptop by its cores and its screen's pixel ratio), then while the scene animates the first WARM frames say whether
   the guess was too rich (warm: their median frame time against the budget, a tier down or two). The budget is BUDGET.laptop
   ms a frame, BUDGET.phone on a phone; a frame is over it past SLACK times it, so a 60 Hz screen's 16.7 ms is in budget.
   After the warm-up a watch() keeps a mean of the frame times over each second and steps a tier down when RUN seconds
   running are over budget; it never steps up, so the page does not flicker between two. Under ?q= the tier is the viewer's
   choice and stays. All of it is pure: it takes the numbers it judges, so testdata/splash_test.js runs it under node. */
var QUALITY = (function () {
  "use strict";
  var TIERS = {
    high: { name: "high", grass: 60000, cascades: 2, near: 2048, far: 1024, pond: "mirror", ratio: 2, impostors: false },
    mid: { name: "mid", grass: 25000, cascades: 1, near: 0, far: 2048, pond: "tint", ratio: 1.5, impostors: false },
    low: { name: "low", grass: 8000, cascades: 1, near: 0, far: 1024, pond: "water", ratio: 1, impostors: true }
  };
  var ORDER = ["low", "mid", "high"]; // poorest first
  var BUDGET = { laptop: 16, phone: 33 }, SLACK = 1.25;
  // the warm-up: WARM frame times, the first SKIP not counted (the shaders compile on them); a slow device is judged sooner,
  // once the counted frames have taken WARM budgets twice over
  var WARM = 20, SKIP = 2;
  var RUN = 3, WINDOW = 1000; // RUN windows of WINDOW ms running over budget step a tier down

  function clamp(x, lo, hi) { return x < lo ? lo : x > hi ? hi : x; }

  /* override reads ?q= from a location.search: "low", "mid" or "high"; null without one or with any other word. */
  function override(search) {
    var m = /[?&]q=([^&#]*)/.exec(search || ""), v = m ? decodeURIComponent(m[1]).toLowerCase() : "";
    return Object.prototype.hasOwnProperty.call(TIERS, v) ? v : null;
  }

  /* guess is the tier a device starts at from what it says of itself ({phone, cores, dpr}): a phone low; a laptop high with
     8 cores or more, mid with 4 or more, low under that (a browser that does not say has 4); a screen past 2 device px a CSS
     px costs a tier, since even capped it draws the most pixels. */
  function guess(env) {
    env = env || {};
    if (env.phone) return "low";
    var cores = env.cores || 4, i = cores >= 8 ? 2 : cores >= 4 ? 1 : 0;
    if ((env.dpr || 1) > 2) i = Math.max(0, i - 1);
    return ORDER[i];
  }

  /* pick is the tier to start at, and whether the page may change it: ?q='s, held, else guess's, free to step down. */
  function pick(search, env) {
    var q = override(search);
    return q ? { tier: q, held: true } : { tier: guess(env), held: false };
  }

  /* budget is the ms a frame may take: a phone's, else a laptop's. limit is past what a frame is over it. */
  function budget(phone) { return phone ? BUDGET.phone : BUDGET.laptop; }
  function limit(ms) { return ms * SLACK; }

  /* below is the tier under name, or null under low. */
  function below(name) { var i = ORDER.indexOf(name); return i > 0 ? ORDER[i - 1] : null; }

  /* median of a list of numbers; 0 for none. */
  function median(a) {
    if (!a.length) return 0;
    var b = a.slice().sort(function (x, y) { return x - y; }), h = b.length >> 1;
    return b.length % 2 ? b[h] : (b[h - 1] + b[h]) / 2;
  }

  /* warm judges the warm-up: frames is the frame times so far (ms, the first SKIP not counted), ms the budget, name the tier
     drawn. It returns null while it wants more frames, else the tier to keep: name when the median is in budget, a tier down
     within twice the limit, two past that. It has enough at WARM frames, or sooner once the counted ones have taken
     2 x WARM budgets (a frame of seconds is judged on a few). */
  function warm(frames, ms, name) {
    var f = frames.slice(SKIP), sum = 0;
    f.forEach(function (x) { sum += x; });
    if (!f.length || f.length + SKIP < WARM && sum < 2 * WARM * ms) return null;
    var m = median(f), over = m <= limit(ms) ? 0 : m <= 2 * limit(ms) ? 1 : 2, i = ORDER.indexOf(name);
    return ORDER[clamp(i - over, 0, ORDER.length - 1)];
  }

  /* watch(ms) watches the frames after the warm-up against budget ms. frame(now, dt) takes each frame's time (dt ms, ended at
     page ms now) and returns true when it is time to step a tier down: RUN windows of WINDOW ms running whose mean is over the
     limit; then it starts afresh. reset() forgets the frames, for after a pause or a tier change. */
  function watch(ms) {
    var w0 = null, sum = 0, n = 0, over = 0;
    function reset() { w0 = null; sum = 0; n = 0; over = 0; }
    function frame(now, dt) {
      if (w0 == null) w0 = now - dt;
      sum += dt; n++;
      if (now - w0 < WINDOW) return false;
      over = sum / n > limit(ms) ? over + 1 : 0;
      w0 = now; sum = 0; n = 0;
      if (over < RUN) return false;
      reset();
      return true;
    }
    return { frame: frame, reset: reset, get over() { return over; } };
  }

  return { TIERS: TIERS, ORDER: ORDER, BUDGET: BUDGET, SLACK: SLACK, WARM: WARM, SKIP: SKIP, RUN: RUN, WINDOW: WINDOW,
    override: override, guess: guess, pick: pick, budget: budget, limit: limit, below: below, median: median, warm: warm, watch: watch };
})();
if (typeof module !== "undefined") module.exports = QUALITY;
