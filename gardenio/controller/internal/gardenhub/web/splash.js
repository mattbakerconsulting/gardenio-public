/* SPLASH: the splash page's scene (splash.go, plan gd-EJGA3zDGCk): three.js (vendored, web/vendor/VENDOR.md, loaded with
   import()) on a full-viewport canvas. LAND (web/splash/land.js, step 3) the hill and valley, ALMANAC (web/almanac.js) the
   sky at the page's clock, STONE (step 4) the marked stone on the brow, FLORA (step 5) the meadow, trees and rooks, FARM
   (step 6) the farmstead and pond, LIGHT (step 7) the sun or moon in two shadow cascades, sky, stars, fog and fireflies.
   TIME (step 8): the scene follows the clock, the shadow map redrawn only when the light moves a quarter degree; a scrubber
   drags it through the day (arrows 10 min, Shift an hour), Play runs a day a minute, ?t= pins and is written back with
   replaceState, ?play=1 plays; the camera breathes except under reduced motion and ?t= (the goldens). QUALITY (step 9)
   picks and steps down the tier (?q=low|mid|high); ?perf=1 shows the frame budget readout. Reduced motion redraws once a
   minute; hidden or off screen pauses; pagehide frees the GPU. VIEW (web/splash/view.js, plan gd-I5QHVfGLbD) the zoom and
   the look: the camera's fov narrowed by the zoom and its aim turned by the yaw and pitch, from ?z=&look=; the canvas's
   gestures (wheel, drag, pinch, double tap) and the view's buttons and keys (#splash-view) move it and write it back. Nothing is fetched. No WebGL or no three.js: a still and a
   note, never a blank canvas. The pure parts take THREE, the canvas and the window as arguments (testdata/splash_test.js). */
var SPLASH = (function () {
  "use strict";
  var A = typeof ALMANAC !== "undefined" ? ALMANAC : require("./almanac.js");
  var L = typeof LAND !== "undefined" ? LAND : require("./splash/land.js");
  var ST = typeof STONE !== "undefined" ? STONE : require("./splash/stone.js");
  var FL = typeof FLORA !== "undefined" ? FLORA : require("./splash/flora.js");
  var FM = typeof FARM !== "undefined" ? FARM : require("./splash/farm.js");
  var LT = typeof LIGHT !== "undefined" ? LIGHT : require("./splash/light.js");
  var Q = typeof QUALITY !== "undefined" ? QUALITY : require("./splash/quality.js");
  var V = typeof VIEW !== "undefined" ? VIEW : require("./splash/view.js");
  // what the pond's mirror never shows: the near things, behind and below its view of the far slope
  var UNREFLECTED = ["grass", "flowers", "hedge leaves", "stone", "fireflies"];
  var FOV = 50, MAX_DPR = 2, NEAR = 0.3, FAR = 3000;
  // the frame on a narrow screen: its field of view widens from FOV until the width shows WIDE degrees, up to FOV_MAX, so a
  // portrait phone keeps the stone whole right of centre and the oak's crown at the left rather than cropping them
  var WIDE = 60, FOV_MAX = 80;
  var HOURS_EVERY = 10000; // how often (ms of the scene's clock) the farm's hour is read again as the clock runs
  // the frame log: the first after WARM_LOG ms of drawing, then one every LOG_EVERY ms
  var WARM_LOG = 5000, LOG_EVERY = 60000;
  // the ?perf=1 readout: the frames of the last ROLL ms, written again every METER_EVERY ms
  var ROLL = 10000, METER_EVERY = 2000;
  // under reduced motion nothing moves, and the scene is drawn again once a minute as the light moves on (else every second)
  var STILL_EVERY = 60000;
  // the time: a day of unix ms, a day played in PLAY_MS, the scrubber's MINUTES; the shadow map is drawn again when the light
  // has moved SHADOW_STEP degrees since it was last drawn
  var DAY = 86400000, PLAY_MS = 60000, PLAY = DAY / PLAY_MS, MINUTES = 1440, SHADOW_STEP = 0.25, SHADOW_COS = Math.cos(SHADOW_STEP * Math.PI / 180);
  // the scrubber's steps by key, in minutes: an arrow, and an arrow with Shift
  var STEP = 10, STEP_SHIFT = 60;
  // the camera's breath: how far the eye drifts (m) and how far its aim wanders (radians), over periods of half a minute or so
  var BREATH_EYE = 0.03, BREATH_AIM = 0.003;
  // the gestures: a view's change eases over EASE ms; a ctrl wheel (a trackpad's pinch) zooms CTRL_WHEEL times a wheel's;
  // a wheel burst ends QUIET ms after its last turn; a double tap is two within TAP_MS and TAP_PX
  var EASE = 150, CTRL_WHEEL = 5, QUIET = 200, TAP_MS = 300, TAP_PX = 24;
  // the view's buttons: Zoom in and Zoom out step the zoom ZOOM_STEP times; an arrow turns the view a tenth of the frame (TURN),
  // a third with Shift (TURN_SHIFT)
  var ZOOM_STEP = 1.5, TURN = 0.1, TURN_SHIFT = 1 / 3;
  var TWILIGHT = { day: "day", golden: "golden hour", civil: "civil twilight", nautical: "nautical twilight", astronomical: "astronomical twilight", night: "night" };
  var MONTHS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];

  /* clock reads ?t= from a location.search: unix ms, or anything Date.parse takes ("2026-10-26T12:00:00Z"); without one, or
     with one it cannot read, now. The scene will be lit at that instant; the placeholder draws the same at every t. */
  function clock(search, now) {
    var m = /[?&]t=([^&#]*)/.exec(search || ""), v = m ? decodeURIComponent(m[1]) : "";
    var t = /^\d+$/.test(v) ? +v : Date.parse(v);
    return isFinite(t) ? t : now;
  }

  /* rfc3339 writes unix ms t as ?t= takes it, to the whole second in UTC: "2026-10-26T12:00:00Z". */
  function rfc3339(t) { return new Date(Math.floor(t / 1000) * 1000).toISOString().replace(/\.\d{3}Z$/, "Z"); }

  /* query returns location.search with the keys of set set to their values, a null removing the key, the others kept in
     their order: query("?lat=52&t=x", {t: "y", play: null}) is "?lat=52&t=y". */
  function query(search, set) {
    var seen = {}, parts = (search || "").replace(/^\?/, "").split("&").filter(Boolean).map(function (kv) {
      var k = kv.split("=")[0];
      if (!Object.prototype.hasOwnProperty.call(set, k)) return kv;
      seen[k] = true;
      return set[k] == null ? null : k + "=" + set[k];
    }).filter(function (kv) { return kv != null; });
    Object.keys(set).forEach(function (k) { if (!seen[k] && set[k] != null) parts.push(k + "=" + set[k]); });
    return parts.length ? "?" + parts.join("&") : "";
  }

  /* dayStart is the unix ms of the local midnight that begins t's day: the day the scrubber spans and Play goes round. */
  function dayStart(t) { var d = new Date(t); return new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime(); }

  /* minute is t's minute of the day that begins at day, 0 to MINUTES - 1: the scrubber's value. */
  function minute(t, day) { return Math.max(0, Math.min(MINUTES - 1, Math.floor((t - day) / 60000))); }

  /* step is the scrubber's value after key (a KeyboardEvent's key, with shift) from value v: ArrowLeft and ArrowDown back
     STEP minutes, ArrowRight and ArrowUp on, STEP_SHIFT with Shift, held to the day; null for any other key. */
  function step(v, key, shift) {
    var d = { ArrowLeft: -1, ArrowDown: -1, ArrowRight: 1, ArrowUp: 1 }[key];
    return d ? Math.max(0, Math.min(MINUTES - 1, v + d * (shift ? STEP_SHIFT : STEP))) : null;
  }

  /* reading is what the readout says at unix ms t under palette p: the local time and date, the twilight's name, the sun's
     height to the degree and the moon's phase and lit fraction. */
  function reading(t, p) {
    var d = new Date(t), el = Math.round(p.sun.elevation);
    function two(n) { return (n < 10 ? "0" : "") + n; }
    return {
      time: two(d.getHours()) + ":" + two(d.getMinutes()), date: d.getDate() + " " + MONTHS[d.getMonth()],
      twilight: TWILIGHT[p.twilight] || p.twilight, sun: "sun " + (el < 0 ? "\u2212" : "") + Math.abs(el) + "\u00b0",
      moon: p.moon.phase + " moon " + Math.round(p.moon.fraction * 100) + "%"
    };
  }

  /* breath is the camera's drift s seconds in, for an eye dist m from what it looks at: {eye, aim}, offsets (m) to add to
     each, a few centimetres and about BREATH_AIM radians, on slow periods that do not share a beat. */
  function breath(s, dist) {
    function w(period, phase) { return Math.sin(2 * Math.PI * s / period + phase); }
    var a = BREATH_AIM * dist;
    return {
      eye: [BREATH_EYE * w(29, 0), BREATH_EYE * 0.5 * w(37, 1.3), BREATH_EYE * 0.8 * w(43, 2.1)],
      aim: [a * w(47, 0.7), a * 0.6 * w(53, 2.9), 0]
    };
  }

  /* fov is the camera's vertical field of view in degrees for a frame of aspect (width / height): FOV wherever the width
     shows WIDE degrees at it (a desktop, a phone on its side), else wider, up to FOV_MAX (a phone upright). LAND.camera
     pitches the camera for it, so the crest stays a third of the way up whatever the screen. */
  function fov(aspect) {
    var v = 2 * Math.atan(Math.tan(WIDE * Math.PI / 360) / (aspect || 1)) * 180 / Math.PI;
    return Math.round(Math.max(FOV, Math.min(FOV_MAX, v)) * 10) / 10;
  }

  /* label is what the canvas says to a screen reader at palette p: the hour, where the sun and the moon stand
     (ALMANAC.words) and what the picture always shows, "Dusk over the farm: the sun low in the west; a full moon, 100
     percent, rising in the east; the gardenio stone on its hill". */
  function label(p) {
    var w = A.words(p);
    return w.when + " over the farm: " + [w.sun, w.moon, "the gardenio stone on its hill"].filter(Boolean).join("; ");
  }

  /* pixelRatio is the renderer's device px per CSS px: the screen's, capped at cap (a tier's ratio) or MAX_DPR without one
     (a 3x phone draws at 2x). */
  function pixelRatio(dpr, cap) { return Math.min(cap || MAX_DPR, dpr || 1); }

  /* webgl says whether doc can make a WebGL context at all, so boot falls back before it fetches three.js. */
  function webgl(doc) {
    try { var c = doc.createElement("canvas"); return !!(c.getContext("webgl2") || c.getContext("webgl")); } catch (e) { return false; }
  }

  /* world(THREE, opts) builds the scene and its camera with the THREE it is given: the sky at opts.t (unix ms) seen from
     opts.lat, opts.lon (ALMANAC's defaults without them), the land (its field map painted when opts.doc is a document),
     the stone on the brow (its mark cut when opts.doc is a document), what grows, the farm at its hour and the light (LIGHT's,
     lit at t), all at the quality tier opts.tier (a name of QUALITY.TIERS): its blades, its trees, its pond and its shadow
     cascades. Without a tier, opts.grass blades (FLORA's default without it), the pond a mirror when opts.reflect, both
     cascades. */
  function world(THREE, opts) {
    opts = opts || {};
    var q = opts.tier ? Q.TIERS[opts.tier] : null;
    var t = opts.t == null ? Date.now() : opts.t, p = A.skyPalette(t, opts.lat, opts.lon);
    var scene = new THREE.Scene();
    scene.fog = new THREE.FogExp2(p.fog, L.FOG_DENSITY);
    var view = L.camera(FOV), camera = new THREE.PerspectiveCamera(FOV, 1, NEAR, FAR);
    camera.position.set(view.eye[0], view.eye[1], view.eye[2]);
    camera.lookAt(view.at[0], view.at[1], view.at[2]);

    var land = L.build(THREE, opts.doc);
    land.getObjectByName("land").receiveShadow = true;
    var stone = ST.build(THREE, opts.doc, p);
    var flora = FL.build(THREE, opts.doc, { grass: q ? q.grass : opts.grass, impostors: !!(q && q.impostors), splat: land.getObjectByName("land").userData.splat, palette: p });
    var farm = FM.build(THREE, opts.doc, { t: t, lat: opts.lat, lon: opts.lon, palette: p, pond: q && q.pond, reflect: !!opts.reflect });

    var light = LT.build(THREE, { t: t, lat: opts.lat, lon: opts.lon, stone: stone, eye: camera.position, fog: scene.fog, quality: q });
    scene.add(land, stone, flora, farm, light);
    FM.reflected(scene, UNREFLECTED);
    var w = { scene: scene, camera: camera, palette: p, view: view, flora: flora, farm: farm, light: light, lat: opts.lat, lon: opts.lon, t: null, hoursAt: t, tier: q };
    relight(w, t);
    return w;
  }

  /* relight lights world w at unix ms t (LIGHT.update) and hands the palette on: the leaves' glow, the sky the gems' facets
     reflect and a tinted pond shows, and, every HOURS_EVERY ms of the clock, the farm's hour (its windows, smoke and birds). */
  function relight(w, t) {
    if (w.t === t) return w.palette;
    var p = LT.update(w.light, t), sky = w.scene.getObjectByName("stone").userData.sky;
    FL.light(p);
    if (sky) { sky.zenith.set(p.zenith); sky.horizon.set(p.horizon); sky.ground.set(p.ground); }
    FM.sky(w.farm, p);
    if (Math.abs(t - w.hoursAt) >= HOURS_EVERY) { FM.set(w.farm, FM.hours(t, w.lat, w.lon)); w.hoursAt = t; }
    w.palette = p;
    w.t = t;
    return p;
  }

  /* start draws the world (opts as world's) on canvas cv in win: ACES tone mapping, the shadow map redrawn only after the
     light moves SHADOW_STEP degrees, the pixel ratio capped by the tier. It draws on each ResizeObserver report and, with
     opts.motion, every animation frame (opts.breathe moves the camera), counting frames on cv.dataset.frames, timing in perf.
     The clock starts at opts.t, pinned or (opts.run) following the real clock, the light moved once a second.
     set(t, rate): rate 0 pins, 1 follows, PLAY a day a minute; at(t) relights now; now() the clock's unix ms; listen(fn)
     calls fn(t, palette) as the light moves; animate(motion, breathe); every(ms) the timer's redraw.
     The view (VIEW's {zoom, yaw, pitch}) starts at opts.view, the default without it: look(state) holds it to its bounds,
     turns the camera and draws a frame; state() reads it back; glide(state) eases there over EASE ms on the animation
     frames, which stop again when it lands unless something else moves; goal() is where the view is going.
     Quality (step 9): with opts.adapt, QUALITY.warm then QUALITY.watch against opts.budget ms step the tier down;
     quality(name) sets it; perf.steps logs changes, perf.roll the last opts.roll ms of frames. pause(on); dispose() frees
     the GPU and the context. Throws with no WebGL context. */
  function start(THREE, cv, win, opts) {
    opts = opts || {};
    var renderer = new THREE.WebGLRenderer({ canvas: cv, antialias: true });
    renderer.outputColorSpace = THREE.SRGBColorSpace;
    renderer.toneMapping = THREE.ACESFilmicToneMapping;
    renderer.shadowMap.enabled = true;
    renderer.shadowMap.type = THREE.PCFShadowMap;
    renderer.shadowMap.autoUpdate = false;
    var w = world(THREE, opts), frames = 0, clock = win.performance || Date;
    var perf = { frames: 0, frame: [], draw: [], logged: null, tier: w.tier ? w.tier.name : null, budget: opts.budget || null,
      held: !!(w.tier && !opts.adapt), began: null, steps: [], roll: opts.roll ? [] : null };
    // the scene's clock: base (unix ms) at from (the page's ms), running at rate scene ms a page ms, round the day from day
    var clk = { base: w.t, from: null, rate: opts.run ? 1 : 0, day: null }, stride = opts.every || 1000, drawn = null;
    var motion = !!opts.motion, breathe = !!opts.breathe, looping = false, queued = false, listeners = [];
    var last = null, began = null, shadowed = null, clear = new THREE.Color(), breathed = 0;
    // the tier's judges: the warm-up's frames, then the watch; skip is the frames not judged after a change (they compile)
    var adapt = !!(opts.adapt && w.tier), budget = opts.budget || Q.BUDGET.laptop, warmup = adapt ? [] : null, watch = adapt ? Q.watch(budget) : null, skip = 0;
    var paused = false, gone = false, took = 0, ease = null;
    // the viewer's zoom and look, and the designed frame's fov (draw's, for the screen) that the zoom narrows
    var state = V.clamp(opts.view), base = w.camera.fov;
    if (w.tier) cv.dataset.tier = w.tier.name;
    function draw() {
      if (gone) return;
      var t0 = clock.now(), width = Math.max(1, cv.clientWidth), height = Math.max(1, cv.clientHeight), ratio = pixelRatio(win.devicePixelRatio, w.tier && w.tier.ratio);
      renderer.setPixelRatio(ratio);
      renderer.setSize(width, height, false);
      w.camera.aspect = width / height;
      // the view is framed for the designed fov, so the crest's pitch is the frame's; the zoom narrows the camera's past it
      var f = fov(w.camera.aspect);
      if (f !== base) { base = f; w.view = L.camera(f); sway(breathed); }
      w.camera.fov = V.fov(base, state.zoom);
      w.camera.updateProjectionMatrix();
      renderer.toneMappingExposure = w.light.userData.exposure;
      renderer.setClearColor(clear.set(w.palette.horizon), 1);
      LT.resize(w.light, height, ratio);
      var d = w.palette.direction;
      if (!shadowed || d[0] * shadowed[0] + d[1] * shadowed[1] + d[2] * shadowed[2] < SHADOW_COS) { renderer.shadowMap.needsUpdate = true; shadowed = d; }
      renderer.render(w.scene, w.camera);
      cv.dataset.frames = String(++frames);
      drawn = clock.now();
      perf.draw.push(took = drawn - t0);
    }
    var observer = new win.ResizeObserver(function () { if (!paused) draw(); });
    observer.observe(cv);
    // time is the scene's unix ms at the page's ms now: pinned, to the whole second following the clock, or played round the day
    function time(now) {
      if (clk.from == null) clk.from = now;
      var dt = Math.max(0, now - clk.from) * clk.rate;
      if (clk.rate === 1) return clk.base + Math.floor(dt / 1000) * 1000;
      if (clk.day != null) return clk.day + ((clk.base - clk.day + dt) % DAY + DAY) % DAY;
      return clk.base + dt;
    }
    function light(t) {
      if (w.t === t) return;
      relight(w, t);
      listeners.forEach(function (fn) { fn(w.t, w.palette); });
    }
    // sway puts the camera where the view (framed for the screen by draw) turned by the viewer's look has it, breathing s
    // seconds in: the aim's wander over the zoom, so a zoomed-in frame does not shake; the eye's drift is centimetres, kept
    function sway(s) {
      var a = V.aim(w.view, state), d0 = a.eye, a0 = a.at, reach = Math.sqrt(Math.pow(a0[0] - d0[0], 2) + Math.pow(a0[1] - d0[1], 2) + Math.pow(a0[2] - d0[2], 2));
      var b = breathe ? breath(s, reach / state.zoom) : { eye: [0, 0, 0], aim: [0, 0, 0] };
      breathed = s;
      w.camera.position.set(d0[0] + b.eye[0], d0[1] + b.eye[1], d0[2] + b.eye[2]);
      w.camera.lookAt(a0[0] + b.aim[0], a0[1] + b.aim[1], a0[2] + b.aim[2]);
    }
    if (state.zoom !== 1 || state.yaw || state.pitch) sway(0);
    // judge hands a frame of dt ms that ended at now to the warm-up, then to the watch, while the wind blows
    function judge(now, dt) {
      if (!adapt || !motion) return;
      if (skip > 0) { skip--; return; }
      if (warmup) {
        warmup.push(dt);
        var k = Q.warm(warmup, budget, w.tier.name);
        if (k == null) return;
        warmup = null;
        if (k !== w.tier.name) quality(k, "warm-up");
        return;
      }
      if (watch.frame(now, dt)) {
        var b = Q.below(w.tier.name);
        if (b) quality(b, Q.RUN + " s over budget");
        else adapt = false; // nothing below low: stop judging
      }
    }
    function tick(now) {
      if (paused || gone) { looping = false; last = null; return; }
      var dt = last == null ? null : now - last;
      if (dt != null) perf.frame.push(dt);
      if (began == null) began = perf.began = now;
      last = now;
      if (ease) {
        var k = Math.min(1, (now - ease.at) / EASE), e = 1 - Math.pow(1 - k, 3), a = ease.from, b = ease.to;
        state = k < 1 ? { zoom: a.zoom * Math.pow(b.zoom / a.zoom, e), yaw: a.yaw + (b.yaw - a.yaw) * e, pitch: a.pitch + (b.pitch - a.pitch) * e } : b;
        if (k >= 1) ease = null;
        if (!motion || !breathe) sway(breathed);
      }
      if (motion) {
        var s = (now - began) / 1000;
        FL.update(w.flora, s);
        FM.update(w.farm, s);
        LT.tick(w.light, s);
        if (breathe) sway(s);
      }
      light(time(now));
      draw();
      perf.frames++;
      if (perf.roll && dt != null) {
        perf.roll.push([now, dt, took]);
        while (perf.roll[0][0] <= now - opts.roll) perf.roll.shift();
      }
      if (dt != null) judge(now, dt);
      if (now - (perf.logged == null ? began - LOG_EVERY + WARM_LOG : perf.logged) >= LOG_EVERY) { log(win, perf, w.flora); perf.logged = now; }
      if (motion || clk.rate > 1 || ease) win.requestAnimationFrame(tick);
      else { looping = false; last = null; }
    }
    // wake starts the animation frames when something moves (the wind, a played day, the view easing) and they are not
    // running; it says whether they are
    function wake() {
      if (!looping && !paused && !gone && win.requestAnimationFrame && (motion || clk.rate > 1 || ease)) { looping = true; win.requestAnimationFrame(tick); }
      return looping;
    }
    wake();
    // following the clock with nothing moving, the light moves on a one-second timer instead, drawn every stride ms
    var stale = false;
    var timer = win.setInterval ? win.setInterval(function () {
      if (looping || paused || gone || clk.rate !== 1) return;
      var t = w.t, now = clock.now();
      light(time(now));
      stale = stale || w.t !== t;
      if (stale && (drawn == null || now - drawn >= stride - 1)) { stale = false; draw(); }
    }, 1000) : null;
    function at(t) {
      clk.base = t;
      clk.from = clock.now();
      if (clk.day != null) clk.day = dayStart(t);
      light(t);
      draw();
    }
    // redraw has the next animation frame draw the scene: the running frames, or one asked for however often it is called
    function redraw() {
      if (wake()) return;
      if (!win.requestAnimationFrame) { draw(); return; }
      if (!queued) { queued = true; win.requestAnimationFrame(function () { queued = false; if (!paused) draw(); }); }
    }
    function set(t, rate) {
      clk = { base: t, from: clock.now(), rate: rate || 0, day: rate > 1 ? dayStart(t) : null };
      light(t);
      redraw();
    }
    // look moves the view to s, held to its bounds, and draws it, even with nothing moving (reduced motion, a pinned ?t=)
    function look(s) {
      if (gone) return V.clamp(state);
      ease = null;
      state = V.clamp(s);
      sway(breathed);
      redraw();
      return V.clamp(state);
    }
    // glide eases the view from where it stands to s, held to its bounds; with no animation frames to run on it looks at once
    function glide(s) {
      if (gone) return V.clamp(state);
      ease = { from: V.clamp(state), to: V.clamp(s), at: clock.now() };
      return wake() ? V.clamp(ease.to) : look(s);
    }
    function animate(m, b) {
      motion = !!m;
      breathe = !!b;
      if (!breathe) sway(0);
      wake();
    }
    // every sets how often (ms) the timer draws the scene as the light moves
    function every(ms) { stride = ms; }
    // quality moves the scene to tier name now: what grows, the shadows (drawn again), the pond, the pixel ratio
    function quality(name, why) {
      var q = Q.TIERS[name];
      if (!q || q === w.tier || gone) return;
      FL.quality(w.flora, q);
      LT.quality(w.light, q);
      shadowed = null;
      FM.pond(THREE, w.farm, q.pond, w.palette);
      perf.steps.push({ from: w.tier ? w.tier.name : null, to: name, at: began == null ? 0 : clock.now() - began });
      w.tier = q;
      perf.tier = cv.dataset.tier = name;
      skip = Q.SKIP;
      if (watch) watch.reset();
      if (win.console) win.console.info("splash: " + name + " tier" + (why ? " (" + why + ")" : ""));
      if (!looping && !paused) draw();
    }
    function pause(p) {
      p = !!p;
      if (p === paused || gone) return;
      paused = p;
      if (paused) return;
      last = null;
      if (watch) watch.reset();
      if (!wake()) { light(time(clock.now())); draw(); }
    }
    // dispose lets go of the scene's geometry, materials, textures, render targets and shadow maps, then the renderer's
    // context, so a page the viewer has left holds nothing of the GPU's
    function dispose() {
      if (gone) return;
      gone = paused = true;
      if (timer != null && win.clearInterval) win.clearInterval(timer);
      if (observer.disconnect) observer.disconnect();
      w.scene.traverse(function (o) {
        if (o.geometry) o.geometry.dispose();
        [].concat(o.material || [], o.customDepthMaterial || []).forEach(function (m) {
          Object.keys(m).forEach(function (k) { if (m[k] && m[k].isTexture) m[k].dispose(); });
          Object.keys(m.uniforms || {}).forEach(function (k) { var u = m.uniforms[k].value; if (u && u.isTexture) u.dispose(); });
          m.dispose();
        });
        if (o.isInstancedMesh) o.dispose();
        Object.keys(o.userData).forEach(function (k) { var u = o.userData[k]; if (u && (u.isTexture || u.isRenderTarget)) u.dispose(); });
        if (o.shadow && o.shadow.map) { o.shadow.map.dispose(); o.shadow.map = null; }
      });
      if (renderer.dispose) renderer.dispose();
      if (renderer.forceContextLoss) renderer.forceContextLoss();
      cv.dataset.gone = "1";
    }
    var v = { renderer: renderer, scene: w.scene, camera: w.camera, flora: w.flora, farm: w.farm, light: w.light, draw: draw, at: at, set: set, animate: animate, every: every, perf: perf,
      look: look, glide: glide, state: function () { return V.clamp(state); }, goal: function () { return V.clamp(ease ? ease.to : state); },
      quality: quality, pause: pause, dispose: dispose, listen: function (fn) { listeners.push(fn); }, now: function () { return time(clock.now()); },
      // log writes the frames since the last log now, the next a minute on
      log: function () { var s = log(win, perf, w.flora); perf.logged = clock.now(); return s; } };
    Object.defineProperty(v, "palette", { get: function () { return w.palette; }, enumerable: true });
    Object.defineProperty(v, "view", { get: function () { return w.view; }, enumerable: true });
    Object.defineProperty(v, "t", { get: function () { return w.t; }, enumerable: true });
    Object.defineProperty(v, "rate", { get: function () { return clk.rate; }, enumerable: true });
    Object.defineProperty(v, "tier", { get: function () { return w.tier; }, enumerable: true });
    Object.defineProperty(v, "paused", { get: function () { return paused; }, enumerable: true });
    Object.defineProperty(v, "judging", { get: function () { return adapt ? warmup ? "warm-up" : "watch" : null; }, enumerable: true });
    return v;
  }

  /* log writes the frames since the last log to the console and returns their summary: the mean and the 95th percentile of
     the time between frames (what the eye sees; a 60 Hz screen caps it at 16.7 ms) and of the time spent drawing one, the
     blades drawn, and the tier and its budget when there is one. */
  function log(win, perf, flora) {
    var s = summary(perf);
    s.tier = perf.tier;
    s.budget = perf.budget;
    perf.last = s;
    perf.frame = []; perf.draw = [];
    if (win.console) win.console.info(line(s, flora.getObjectByName("grass").count));
    return s;
  }
  /* line is the log's words for summary s (with its tier and budget) and blades drawn: "splash: frame 16.7 ms (p95 18.2),
     draw 4.1 ms (p95 6) over 598 frames, 8000 blades, low tier, budget 33 ms". */
  function line(s, blades) {
    return "splash: frame " + s.frame + " ms (p95 " + s.frame95 + "), draw " + s.draw + " ms (p95 " + s.draw95 + ") over " + s.n + " frames, " + blades + " blades" +
      (s.tier ? ", " + s.tier + " tier" + (s.budget ? ", budget " + s.budget + " ms" : "") : "");
  }
  /* summary is the mean and 95th percentile of perf's frame and draw times, in ms to a tenth. */
  function summary(perf) {
    function stat(a) {
      if (!a.length) return [0, 0];
      var b = a.slice().sort(function (x, y) { return x - y; }), sum = 0;
      b.forEach(function (v) { sum += v; });
      return [Math.round(sum / b.length * 10) / 10, Math.round(b[Math.min(b.length - 1, Math.floor(b.length * 0.95))] * 10) / 10];
    }
    var f = stat(perf.frame), d = stat(perf.draw);
    return { n: perf.frame.length, frame: f[0], frame95: f[1], draw: d[0], draw95: d[1] };
  }

  /* perfOn says whether the address asks for the frame readout: ?perf=1. */
  function perfOn(search) { return /[?&]perf=1(&|$)/.test(search || ""); }

  /* gauge is what the readout says of perf (start's, with a roll) at the page's ms now with blades drawn: the log's line over
     the frames of the last ROLL ms, then the time since the first frame and the tier's step-downs so far ("stepped high ->
     mid at 12 s"), or that there are none, or that ?q= holds the tier. */
  function gauge(perf, blades, now) {
    var r = perf.roll || [], s = summary({ frame: r.map(function (f) { return f[1]; }), draw: r.map(function (f) { return f[2]; }) });
    s.tier = perf.tier;
    s.budget = perf.budget;
    function secs(ms) { var n = Math.max(0, Math.floor(ms / 1000)); return n < 60 ? n + " s" : Math.floor(n / 60) + " m " + n % 60 + " s"; }
    var steps = perf.steps.map(function (k) { return k.from + " -> " + k.to + " at " + secs(k.at); }).join(", ");
    if (steps) steps = "stepped " + steps;
    else steps = perf.held ? "held at " + perf.tier + " by ?q=" : perf.tier ? "no step-downs" : "";
    return line(s, blades) + "\nrunning " + secs(perf.began == null ? 0 : now - perf.began) + ", the last " + ROLL / 1000 + " s" + (steps ? "; " + steps : "");
  }

  /* meter adds the ?perf=1 readout to the page (#splash-perf, top left, over the scene): gauge's words for view, written again
     every METER_EVERY ms until pagehide. It is text to select and copy, not announced (aria-live off); it takes no room
     from the canvas or the time controls along the bottom. */
  function meter(doc, win, view) {
    var out = doc.createElement("output"), clock = win.performance || Date;
    out.id = "splash-perf";
    out.setAttribute("aria-live", "off");
    out.setAttribute("aria-label", "Frame times");
    function show() {
      var text = gauge(view.perf, view.flora.getObjectByName("grass").count, clock.now());
      if (out.textContent !== text) out.textContent = text;
    }
    show();
    doc.body.appendChild(out);
    var timer = win.setInterval ? win.setInterval(show, METER_EVERY) : null;
    win.addEventListener("pagehide", function () { if (timer != null && win.clearInterval) win.clearInterval(timer); });
    return { el: out, show: show };
  }

  /* flat shows the still and the note in place of the canvas, and says why on body.dataset.gl. */
  function flat(doc, why) {
    doc.body.dataset.gl = "off";
    doc.body.dataset.why = why;
    doc.getElementById("splash-flat").hidden = false;
  }

  /* motion says whether the wind blows: when the clock runs free, or ?wind=1 asks for it under a pinned one, and never under
     prefers-reduced-motion. A pinned ?t= alone draws one still, so a golden is the same every time. */
  function motion(search, win) {
    return !reduced(win) && (/[?&]wind=1(&|$)/.test(search || "") || !/[?&]t=/.test(search || ""));
  }

  /* reduced says whether the viewer asked for reduced motion: no wind, and no breath. */
  function reduced(win) {
    try { return !!(win.matchMedia && win.matchMedia("(prefers-reduced-motion: reduce)").matches); } catch (e) { return false; }
  }

  /* breathes says whether the camera breathes: never under reduced motion, nor under a pinned ?t=, which a golden takes. */
  function breathes(search, win) { return !reduced(win) && !/[?&]t=/.test(search || ""); }

  /* controls binds the page's time controls (#splash-time) to view and shows them: the readout and the scrubber follow the
     light; dragging the scrubber, or its arrow keys, pins the scene in the scrubber's day and writes ?t= back with
     replaceState (on the drag's release), so the address is a link to the frame; Play plays the day from where it stands
     (?play=1) and pauses it (?t= again); Now follows the clock again, the wind and the breath back unless reduced motion
     says otherwise (no ?t=, no ?play). */
  function controls(doc, win, view) {
    var ui = doc.getElementById("splash-time");
    if (!ui) return null;
    var scrub = doc.getElementById("splash-scrub"), play = doc.getElementById("splash-play"), now = doc.getElementById("splash-now"), out = doc.getElementById("splash-readout");
    var when = out.querySelector(".when"), sky = out.querySelector(".sky"), day = dayStart(view.t);
    function write(set) { replace(win, set); }
    function show(t, p) {
      var r = reading(t, p), playing = view.rate > 1, text = r.time + " \u00b7 " + r.date;
      day = dayStart(t);
      scrub.value = String(minute(t, day));
      scrub.setAttribute("aria-valuetext", r.time);
      if (when.textContent !== text) when.textContent = text;
      text = [r.twilight, r.sun, r.moon].join(" \u00b7 ");
      if (sky.textContent !== text) sky.textContent = text;
      out.dataset.t = String(t);
      play.setAttribute("aria-pressed", String(playing));
      play.textContent = playing ? "Pause" : "Play";
    }
    function pin(v) { view.set(day + v * 60000, 0); show(view.t, view.palette); }
    scrub.addEventListener("input", function () { pin(+scrub.value); });
    scrub.addEventListener("change", function () { write({ t: rfc3339(view.t), play: null }); });
    scrub.addEventListener("keydown", function (e) {
      var v = step(+scrub.value, e.key, e.shiftKey);
      if (v == null) return;
      e.preventDefault();
      pin(v);
      write({ t: rfc3339(view.t), play: null });
    });
    play.addEventListener("click", function () {
      var playing = view.rate > 1;
      // paused, the scene stands where the day has got to (not where the last frame drew it), on the whole second its link names
      view.set(playing ? Math.floor(view.now() / 1000) * 1000 : view.t, playing ? 0 : PLAY);
      write(playing ? { t: rfc3339(view.t), play: null } : { play: "1" });
      show(view.t, view.palette);
    });
    now.addEventListener("click", function () {
      var still = reduced(win);
      view.set(Date.now(), 1);
      view.animate(!still, !still);
      write({ t: null, play: null });
      show(view.t, view.palette);
    });
    view.listen(show);
    show(view.t, view.palette);
    ui.hidden = false;
    return { show: show };
  }

  /* replace writes set (as query takes it) into win's address with replaceState, so the address is a link to the frame. */
  function replace(win, set) {
    var l = win.location;
    try { win.history.replaceState(win.history.state, "", l.pathname + query(l.search, set) + l.hash); } catch (e) { /* a browser that throttles replaceState keeps the last link */ }
  }

  /* gestures binds the canvas cv's pointer to view's zoom and look, with Pointer Events only (the canvas is touch-action
     none, splash.css, so the page never scrolls or zooms from it). The wheel zooms by VIEW.wheel toward the pointer, a
     ctrl wheel (a trackpad's pinch) CTRL_WHEEL times as hard; one pointer drags the view (VIEW.drag); two zoom by their
     distance (VIEW.pinch) toward their midpoint and drag by its travel. A second touch within TAP_MS and TAP_PX of the last,
     or a mouse's double click, zooms 2x toward the point, or back to the default view at ZOOM_MAX. Each change eases (glide),
     or lands at once under reduced motion; the view is written to the address (?z=&look=) as a pointer lifts and QUIET ms
     after a wheel burst. It returns that path for the view's buttons (buttons): move(fn) eases the view to fn(goal), zoom(f, x, y)
     zooms f times toward client px (x, y), the frame's middle without them, write() writes the address, and listen(fn) calls
     fn(goal) after each change. */
  function gestures(cv, win, view) {
    var down = {}, last = null, tap = null, kind = null, quiet = null, heard = [];
    function move(fn) {
      var s = fn(view.goal());
      if (reduced(win)) view.look(s); else view.glide(s);
      heard.forEach(function (h) { h(view.goal()); });
    }
    function zoom(f, x, y) {
      var r = cv.getBoundingClientRect(), w = Math.max(1, r.width), h = Math.max(1, r.height), b = fov(w / h);
      if (x == null) { x = r.left + w / 2; y = r.top + h / 2; }
      move(function (s) { return V.zoomAt(s, f, x - r.left, y - r.top, w, h, b, w / h, view.view.pitch); });
    }
    function write() { replace(win, V.write(view.goal())); }
    // pair is where the pointers down stand: the first two's midpoint and their distance (0 for one), null for none
    function pair() {
      var ids = Object.keys(down), a = down[ids[0]], b = down[ids[1]] || a;
      return a ? { x: (a.x + b.x) / 2, y: (a.y + b.y) / 2, d: Math.hypot(a.x - b.x, a.y - b.y), n: ids.length } : null;
    }
    function twice(x, y) {
      if (view.goal().zoom >= V.ZOOM_MAX) move(V.view); else zoom(2, x, y);
      write();
    }
    cv.addEventListener("wheel", function (e) {
      e.preventDefault();
      zoom(V.wheel(e.deltaY * (e.ctrlKey ? CTRL_WHEEL : 1), e.deltaMode), e.clientX, e.clientY);
      win.clearTimeout(quiet);
      quiet = win.setTimeout(write, QUIET);
    }, { passive: false });
    cv.addEventListener("pointerdown", function (e) {
      kind = e.pointerType;
      if (kind === "mouse" && e.button !== 0) return;
      try { cv.setPointerCapture(e.pointerId); } catch (x) { /* a pointer already gone */ }
      down[e.pointerId] = { x: e.clientX, y: e.clientY };
      last = pair();
      if (kind === "mouse" || last.n > 1) return;
      if (tap && e.timeStamp - tap.t < TAP_MS && Math.hypot(e.clientX - tap.x, e.clientY - tap.y) < TAP_PX) { tap = null; twice(e.clientX, e.clientY); }
      else tap = { t: e.timeStamp, x: e.clientX, y: e.clientY };
    });
    cv.addEventListener("pointermove", function (e) {
      var p = down[e.pointerId];
      if (!p) return;
      p.x = e.clientX; p.y = e.clientY;
      var now = pair();
      if (last) {
        if (now.n > 1) zoom(V.pinch(last.d, now.d), now.x, now.y);
        var h = Math.max(1, cv.clientHeight), dx = now.x - last.x, dy = now.y - last.y;
        move(function (s) { return V.drag(s, dx, dy, fov(cv.clientWidth / h), h); });
      }
      last = now;
    });
    function up(e) {
      if (!down[e.pointerId]) return;
      delete down[e.pointerId];
      last = pair();
      write();
    }
    cv.addEventListener("pointerup", up);
    cv.addEventListener("pointercancel", up);
    cv.addEventListener("dblclick", function (e) { if (kind === "mouse") twice(e.clientX, e.clientY); });
    return { move: move, zoom: zoom, write: write, listen: function (fn) { heard.push(fn); } };
  }

  /* buttons binds the view's buttons (#splash-view) to g, the gestures' path, and shows them: Zoom in and Zoom out step the
     zoom ZOOM_STEP times toward the frame's middle, Reset view (shown only off the default view) goes back to it; on any of
     them an arrow turns the view as a drag of TURN of the frame would (TURN_SHIFT with Shift), ArrowLeft looking left, and
     Escape resets. Each writes ?z=&look= at once. The readout's zoom line says the zoom while zoomed, as the address has it. */
  function buttons(doc, view, g) {
    var ui = doc.getElementById("splash-view");
    if (!ui) return null;
    var zin = doc.getElementById("splash-in"), zout = doc.getElementById("splash-out"), reset = doc.getElementById("splash-reset");
    var line = doc.querySelector("#splash-readout .zoom"), cv = doc.getElementById("splash");
    function show(s) {
      var w = V.write(s);
      if (!w.z && !w.look && doc.activeElement === reset) zin.focus();
      reset.hidden = !w.z && !w.look;
      line.hidden = !w.z;
      line.textContent = w.z ? "zoom " + w.z + "\u00d7" : "";
    }
    function then(fn) { return function () { fn(); g.write(); }; }
    zin.addEventListener("click", then(function () { g.zoom(ZOOM_STEP); }));
    zout.addEventListener("click", then(function () { g.zoom(1 / ZOOM_STEP); }));
    reset.addEventListener("click", then(function () { g.move(V.view); }));
    ui.addEventListener("keydown", function (e) {
      var d = { ArrowLeft: [1, 0], ArrowRight: [-1, 0], ArrowUp: [0, 1], ArrowDown: [0, -1] }[e.key], k = e.shiftKey ? TURN_SHIFT : TURN;
      if (!d && e.key !== "Escape") return;
      e.preventDefault();
      var w = Math.max(1, cv.clientWidth), h = Math.max(1, cv.clientHeight);
      g.move(d ? function (s) { return V.drag(s, d[0] * w * k, d[1] * h * k, fov(w / h), h); } : V.view);
      g.write();
    });
    g.listen(show);
    show(view.goal());
    ui.hidden = false;
    return { show: show };
  }

  /* phone says whether the page is on a phone: a coarse pointer, or a screen under 600 px on its short side. A phone starts
     at the low tier and has a phone's frame budget (QUALITY). */
  function phone(win) {
    var coarse = false;
    try { coarse = !!(win.matchMedia && win.matchMedia("(pointer: coarse)").matches); } catch (e) { coarse = false; }
    var sc = win.screen || {};
    return coarse || Math.min(sc.width || Infinity, sc.height || Infinity) < 600;
  }

  /* probe is window.__splash: what a test reads from the drawn scene. shadow() is where the stone's shadow ends
     (LIGHT.tip), at(t) relights the page at unix ms t, and light() the light now: its body, strength, direction, the
     exposure and the clock. look(state) moves the view as start's look does and state() reads it back. */
  function probe(THREE, view) {
    return {
      shadow: function () { return LT.tip(THREE, view.light, view.scene.getObjectByName("stone"), view.scene.getObjectByName("lands")); },
      at: function (t) { view.at(t); return view.t; },
      look: function (s) { return view.look(s); },
      state: function () { return view.state(); },
      light: function () {
        var p = view.palette;
        return { t: view.t, source: p.source, strength: p.strength, direction: p.direction, twilight: p.twilight, exposure: view.light.userData.exposure, sun: p.sun.elevation, moon: p.moon.elevation, fraction: p.moon.fraction };
      },
      quality: function () {
        var o = view.light.userData, fl = view.flora;
        return { tier: view.tier && view.tier.name, judging: view.judging, paused: view.paused, ratio: view.renderer.getPixelRatio(), blades: fl.getObjectByName("grass").count,
          impostors: !!fl.userData.impostors, cascades: o.cascades, maps: [o.near.visible ? o.near.shadow.mapSize.x : 0, o.far.shadow.mapSize.x], pond: view.farm.getObjectByName("pond").userData.mode, frames: view.perf.frames };
      },
      log: function () { return view.log(); }
    };
  }

  /* lifecycle ties the drawing to the page: it pauses while the page is hidden (visibilitychange) or the canvas is off
     screen (an IntersectionObserver) and draws on when both come back; a change of the reduced-motion preference applies at
     once (calm); on pagehide everything the GPU holds is let go (dispose), and a page the back-forward cache brings back
     (pageshow, persisted) loads afresh. */
  function lifecycle(doc, win, view, cv) {
    var hidden = doc.visibilityState === "hidden", off = false;
    function sync() { view.pause(hidden || off); }
    doc.addEventListener("visibilitychange", function () { hidden = doc.visibilityState === "hidden"; sync(); });
    if (win.IntersectionObserver) new win.IntersectionObserver(function (es) { off = !es[es.length - 1].isIntersecting; sync(); }).observe(cv);
    win.addEventListener("pagehide", function () { view.dispose(); });
    win.addEventListener("pageshow", function (e) { if (e.persisted && cv.dataset.gone) win.location.reload(); });
    var mq = null;
    try { mq = win.matchMedia ? win.matchMedia("(prefers-reduced-motion: reduce)") : null; } catch (e) { mq = null; }
    if (mq && mq.addEventListener) mq.addEventListener("change", function () { calm(win, view); });
    sync();
  }

  /* calm sets view's motion by the viewer's preference and the address: the wind and the breath as motion and breathes say,
     the scene drawn again once a minute under reduced motion, else every second. */
  function calm(win, view) {
    var q = win.location.search || "";
    view.every(reduced(win) ? STILL_EVERY : 1000);
    view.animate(motion(q, win), breathes(q, win));
  }

  /* boot runs the page: the clock and the place (?lat=&lon=, else the timezone's longitude), then three.js from the body's
     data-three by load (import() on the page), then start, the wind blowing as motion says and the camera breathing as
     breathes says, the light running with the clock unless ?t= pins it or playing the day under ?play=1 (under reduced
     motion drawn once a minute), at the quality tier QUALITY.pick chooses, free to step down against the device's
     budget unless ?q= holds it, the view as ?z=&look= give it (VIEW.parse) from the first frame, then the canvas's name (label, kept as the light moves), the time controls, the gestures and the view's buttons, the frame
     readout under ?perf=1, and the page's lifecycle; any step that fails shows the still. It
     resolves when the page is drawn one way or the other, with window.__splash the probe. */
  function boot(doc, win, load) {
    SPLASH.t = clock(win.location.search, Date.now());
    SPLASH.place = A.place(win.location.search, new Date(SPLASH.t).getTimezoneOffset());
    var cv = doc.getElementById("splash");
    if (!webgl(doc)) { flat(doc, "no webgl"); return Promise.resolve(); }
    return load(doc.body.dataset.three).then(function (THREE) {
      var q = win.location.search || "", ph = phone(win), nav = win.navigator || {}, pf = perfOn(q);
      var pk = Q.pick(q, { phone: ph, cores: nav.hardwareConcurrency, dpr: win.devicePixelRatio });
      SPLASH.view = start(THREE, cv, win, { t: SPLASH.t, lat: SPLASH.place.lat, lon: SPLASH.place.lon, doc: doc, motion: motion(q, win), breathe: breathes(q, win), run: !/[?&]t=/.test(q),
        tier: pk.tier, adapt: !pk.held, budget: Q.budget(ph), every: reduced(win) ? STILL_EVERY : 1000, roll: pf ? ROLL : 0, view: V.parse(q) });
      if (/[?&]play=1(&|$)/.test(q)) SPLASH.view.set(SPLASH.t, PLAY);
      win.__splash = probe(THREE, SPLASH.view);
      doc.body.dataset.gl = "on";
      // the canvas is an image whose name says the picture at the scene's clock, kept as the light moves
      var named = function (t, p) { var s = label(p); if (cv.getAttribute("aria-label") !== s) cv.setAttribute("aria-label", s); };
      SPLASH.view.listen(named);
      named(SPLASH.view.t, SPLASH.view.palette);
      controls(doc, win, SPLASH.view);
      buttons(doc, SPLASH.view, gestures(cv, win, SPLASH.view));
      if (pf) meter(doc, win, SPLASH.view);
      lifecycle(doc, win, SPLASH.view, cv);
    }).catch(function (e) { flat(doc, String(e && e.message || e)); });
  }

  return {
    FOV: FOV, WIDE: WIDE, FOV_MAX: FOV_MAX, MAX_DPR: MAX_DPR, STILL_EVERY: STILL_EVERY, ROLL: ROLL, METER_EVERY: METER_EVERY, DAY: DAY, PLAY: PLAY, MINUTES: MINUTES, SHADOW_STEP: SHADOW_STEP, STEP: STEP, STEP_SHIFT: STEP_SHIFT, ZOOM_STEP: ZOOM_STEP,
    clock: clock, rfc3339: rfc3339, query: query, dayStart: dayStart, minute: minute, step: step, reading: reading, breath: breath,
    fov: fov, label: label, pixelRatio: pixelRatio, webgl: webgl, motion: motion, breathes: breathes, phone: phone, summary: summary, line: line, perfOn: perfOn, gauge: gauge, meter: meter, world: world, start: start, controls: controls, gestures: gestures, buttons: buttons, lifecycle: lifecycle, calm: calm, boot: boot
  };
})();
if (typeof module !== "undefined") module.exports = SPLASH;

if (typeof document !== "undefined") SPLASH.boot(document, window, function (url) { return import(url); });
