/* A DOM just wide enough to boot web/garden.js under node, so the frame budget can be measured without a browser
   (homestead-world-design.md section 6.6). Canvas contexts record their draw calls instead of painting; the DOM records
   nothing but structure (children, classes, styles), so what is timed is the page's own JavaScript per frame: input
   handling, culling, the hot-link and label reconciliation, and the draw calls it issues. Raster cost is not modelled.
   stats.canvases counts the bitmaps baked (canvases of 256 px or more: the chunks); stats.sprites the small
   ones (the tile and plant sprite caches, the legend), which are painted once per session and are not a per-frame cost.
   Timed timers never fire; requestAnimationFrame and a zero-delay setTimeout (a task boundary: the page splits work into a task of
   its own with one) are queues the test flushes, so a run is synchronous and deterministic and split work is still measured. */
"use strict";
var path = require("path");

function boot(webDir, opts) {
  opts = opts || {};
  var stats = { main: fresh(), off: fresh(), canvases: 0, sprites: 0 };
  function fresh() { return { fillRect: 0, drawImage: 0, setTransform: 0 }; }

  function Ctx(s) { this._s = s; this.fillStyle = "#000000"; this.imageSmoothingEnabled = true; }
  Ctx.prototype.fillRect = function () { this._s.fillRect++; };
  Ctx.prototype.drawImage = function () { this._s.drawImage++; };
  Ctx.prototype.setTransform = function (a, b, c, d, e, f) { this._s.setTransform++; this._m = { a: a, b: b, c: c, d: d, e: e, f: f }; };
  Ctx.prototype.getTransform = function () { return this._m || { a: 1, b: 0, c: 0, d: 1, e: 0, f: 0 }; };
  Ctx.prototype.clearRect = Ctx.prototype.scale = Ctx.prototype.translate = Ctx.prototype.save = Ctx.prototype.restore = Ctx.prototype.fillText = Ctx.prototype.strokeText =
    Ctx.prototype.beginPath = Ctx.prototype.moveTo = Ctx.prototype.lineTo = Ctx.prototype.arcTo = Ctx.prototype.closePath = Ctx.prototype.fill = Ctx.prototype.stroke = function () {};   // paths: a bird's caption plate
  Ctx.prototype.measureText = function (t) { var m = /(\d+)px/.exec(this.font || ""); return { width: String(t).length * 0.6 * (m ? +m[1] : 10) }; };   // a monospace advance

  function ClassList(el) { this.el = el; this.set = {}; }
  ClassList.prototype.add = function (c) { this.set[c] = 1; };
  ClassList.prototype.remove = function (c) { delete this.set[c]; };
  ClassList.prototype.contains = function (c) { return !!this.set[c]; };
  ClassList.prototype.toggle = function (c, on) { if (on === undefined) on = !this.set[c]; if (on) this.set[c] = 1; else delete this.set[c]; return !!on; };

  var byId = {};
  function El(tag) {
    this.tagName = String(tag).toUpperCase(); this.id = ""; this.dataset = {}; this.children = []; this.parentNode = null;
    this.classList = new ClassList(this); this.attrs = {}; this.style = { setProperty: function (k, v) { this[k] = v; } };
    this.hidden = false; this.tabIndex = 0; this._text = ""; this._on = {}; this.width = 0; this.height = 0;
  }
  Object.defineProperty(El.prototype, "className", { get: function () { return Object.keys(this.classList.set).join(" "); }, set: function (v) { this.classList.set = {}; var self = this; String(v).split(/\s+/).forEach(function (c) { if (c) self.classList.set[c] = 1; }); } });
  Object.defineProperty(El.prototype, "textContent", { get: function () { return this._text; }, set: function (v) { this._text = String(v); this.children.forEach(function (c) { c.parentNode = null; }); this.children = []; } });
  Object.defineProperty(El.prototype, "innerHTML", { get: function () { return ""; }, set: function () { this.children = []; } });
  Object.defineProperty(El.prototype, "firstChild", { get: function () { return this.children[0] || null; } });
  Object.defineProperty(El.prototype, "firstElementChild", { get: function () { return this.children[0] || null; } });
  Object.defineProperty(El.prototype, "nextSibling", { get: function () { if (!this.parentNode) return null; var k = this.parentNode.children.indexOf(this); return this.parentNode.children[k + 1] || null; } });
  Object.defineProperty(El.prototype, "content", { get: function () { return this; } });
  Object.defineProperty(El.prototype, "clientWidth", { get: function () { return this._w || 0; } });
  Object.defineProperty(El.prototype, "clientHeight", { get: function () { return this._h || 0; } });
  Object.defineProperty(El.prototype, "offsetWidth", { get: function () { return this._w || 0; } });
  Object.defineProperty(El.prototype, "offsetHeight", { get: function () { return this._h || 0; } });
  El.prototype.appendChild = function (c) { if (c.parentNode) c.parentNode.removeChild(c); c.parentNode = this; this.children.push(c); return c; };
  El.prototype.insertBefore = function (c, ref) { if (c.parentNode) c.parentNode.removeChild(c); c.parentNode = this; var k = ref ? this.children.indexOf(ref) : -1; if (k < 0) this.children.push(c); else this.children.splice(k, 0, c); return c; };
  El.prototype.removeChild = function (c) { var k = this.children.indexOf(c); if (k >= 0) this.children.splice(k, 1); c.parentNode = null; return c; };
  El.prototype.remove = function () { if (this.parentNode) this.parentNode.removeChild(this); };
  El.prototype.replaceChild = function (c, old) { var k = this.children.indexOf(old); if (k < 0) throw new TypeError("replaceChild: node is not a child"); if (c.parentNode) c.parentNode.removeChild(c); k = this.children.indexOf(old); c.parentNode = this; this.children[k] = c; old.parentNode = null; return old; };
  El.prototype.cloneNode = function () { var e = new El(this.tagName); e.dataset = Object.assign({}, this.dataset); e.className = this.className; e._text = this._text; return e; };
  El.prototype.setAttribute = function (k, v) { this.attrs[k] = String(v); };
  El.prototype.getAttribute = function (k) { return this.attrs[k] === undefined ? null : this.attrs[k]; };
  El.prototype.removeAttribute = function (k) { delete this.attrs[k]; };
  El.prototype.addEventListener = function (t, f) { (this._on[t] = this._on[t] || []).push(f); };
  El.prototype.removeEventListener = function (t, f) { var l = this._on[t] || []; var k = l.indexOf(f); if (k >= 0) l.splice(k, 1); };
  El.prototype.dispatchEvent = function (e) { e.target = e.target || this; var l = (this._on[e.type] || []).slice(); for (var i = 0; i < l.length; i++) l[i].call(this, e); var h = this["on" + e.type]; if (typeof h === "function") h.call(this, e); return true; };
  El.prototype.querySelector = function () { return null; };
  El.prototype.querySelectorAll = function () { return []; };
  El.prototype.getElementsByTagName = function () { return []; };
  El.prototype.closest = function () { return null; };
  El.prototype.contains = function (o) { for (var e = o; e; e = e.parentNode) if (e === this) return true; return false; };
  El.prototype.getBoundingClientRect = function () { return { left: 0, top: 0, width: this._w || 0, height: this._h || 0, right: this._w || 0, bottom: this._h || 0 }; };
  El.prototype.focus = El.prototype.blur = function () {};
  El.prototype.getContext = function () {
    if (!this._ctx) { this._ctx = new Ctx(this.id === "cv" ? stats.main : stats.off); if (this.id !== "cv") { if (Math.max(this.width, this.height) >= 256) stats.canvases++; else stats.sprites++; } }
    return this._ctx;
  };

  var body = new El("body"); body.dataset = { me: opts.me || "", v: "1", base: "" };   // opts.me: the viewer, for the "Mine" scope
  var doc = new El("#document");
  var board = get("board"); board._w = opts.w || 1280; board._h = opts.h || 720;
  function get(id) { if (!byId[id]) { byId[id] = new El(id === "cv" ? "canvas" : "div"); byId[id].id = id; } return byId[id]; }
  get("bead").hidden = get("tour").hidden = get("jump").hidden = true;   // the bead view, the tour card and the jump palette start closed, as their hidden attributes say
  doc.body = body; doc.documentElement = new El("html"); doc.activeElement = body; doc.hidden = false;
  var bSky = get("cam-sky"), skyG = new El("span"), skyT = new El("span");   // the Menu's Sky item (3b R10) with its glyph (drawn in CSS) and word spans (SKY.label says the word)
  skyG.className = "sky-g"; skyT.className = "sky-t"; bSky.appendChild(skyG); bSky.appendChild(skyT);
  bSky.querySelector = function (s) { return s === ".sky-g" ? skyG : s === ".sky-t" ? skyT : null; };
  doc.getElementById = get;
  doc.createElement = function (t) { return new El(t); };
  doc.querySelector = function () { return null; };
  doc.querySelectorAll = function () { return []; };
  doc.getElementsByTagName = function () { return [];  };

  var rafQ = [], taskQ = [], store = {};
  var win = {
    innerWidth: board._w, innerHeight: board._h, devicePixelRatio: opts.dpr || 1, console: console,
    matchMedia: function (q) { return { matches: /reduced-motion/.test(q) ? opts.reduce !== false : !!opts.night, addEventListener: function () {}, addListener: function () {} }; },
    requestAnimationFrame: function (f) { rafQ.push(f); return rafQ.length; },
    addEventListener: function () {}, scrollTo: function () {}
  };
  function EventSource() { this.readyState = 1; }
  EventSource.prototype.addEventListener = function () {}; EventSource.prototype.close = function () {};

  var g = global;
  g.ESCKIND = require(path.join(webDir, "esckind.js")); g.REASON = require(path.join(webDir, "reason.js")); g.ART = require(path.join(webDir, "art.js")); g.WORLD = require(path.join(webDir, "world.js")); g.WEEDS = require(path.join(webDir, "weeds.js")); g.NEIGHBOURS = require(path.join(webDir, "neighbours.js")); g.EDGES = require(path.join(webDir, "edges.js")); g.PLANS = require(path.join(webDir, "plans.js")); g.BEACONS = require(path.join(webDir, "beacons.js")); g.SNAP = require(path.join(webDir, "snap.js")); g.POSTCARD = require(path.join(webDir, "postcard.js")); g.TOUR = require(path.join(webDir, "tour.js")); g.CURSOR = require(path.join(webDir, "cursor.js")); g.CURNAV = require(path.join(webDir, "curnav.js")); g.SIGNFIT = require(path.join(webDir, "signfit.js")); g.HERALDRY = require(path.join(webDir, "heraldry.js")); g.ARMS = require(path.join(webDir, "arms.js"));
  g.window = win; g.document = doc; g.location = { hash: "", pathname: "/garden", search: "" }; g.history = { replaceState: function () {} };
  g.localStorage = { getItem: function (k) { return store[k] === undefined ? null : store[k]; }, setItem: function (k, v) { store[k] = String(v); }, removeItem: function (k) { delete store[k]; } };
  g.EventSource = EventSource; g.CustomEvent = function (t, init) { this.type = t; this.detail = init && init.detail; };
  g.setInterval = function () { return 0; }; g.clearInterval = function () {};
  var taskN = 0;
  g.setTimeout = function (f, ms) { if (ms > 0) return 0; taskQ.push({ id: ++taskN, f: f }); return taskN; };
  g.clearTimeout = function (id) { taskQ = taskQ.filter(function (t) { return t.id !== id; }); };
  g.fetch = function () { return Promise.reject(new Error("no network under the mock")); };
  g.requestAnimationFrame = win.requestAnimationFrame;
  g.ANIM = require(path.join(webDir, "transitions.js"));   // after document: it listens for plans.js's farmlive events
  g.BIRDS = require(path.join(webDir, "birds.js"));   // after document: it listens for the farmactivity, farmlive and farmdoc events
  g.GOATS = require(path.join(webDir, "goats.js"));   // after document: it listens for the farmdoc events
  g.TRUCK = require(path.join(webDir, "truck.js"));   // after document: it listens for the farmdoc events
  g.LIVE = require(path.join(webDir, "live.js"));   // after document: no data-live, so it hands out the mock's EventSource
  g.VIEWCUT = require(path.join(webDir, "viewcut.js"));   // after document: it listens for plans.js's farmhello events
  g.SCENE = require(path.join(webDir, "scene.js"));   // the World's drawing, which garden.js calls each frame
  g.PORCH = require(path.join(webDir, "porch.js"));   // after document: it binds the Porch pane when it loads
  store["garden.sky"] = opts.night ? "night" : "day";   // pin the sky (web/sky.js) to the mock's scheme, whatever the clock says
  g.SKY = require(path.join(webDir, "sky.js"));   // after document and localStorage: it sets html[data-sky] as it loads
  g.ANSWER = require(path.join(webDir, "answer.js"));   // before garden.js: it installs the in-place answer as it boots
  g.STIP = require(path.join(webDir, "stip.js"));
  require(path.join(webDir, "garden.js"));

  /* Run the queued tasks, then one generation of queued animation frames (what one browser frame would run), and return how
     many frames ran; what they queue waits for the next flush. flushAll runs generations until both queues are empty. */
  function tasks() { var n = 0; while (taskQ.length && n < 500) { var q = taskQ; taskQ = []; q.forEach(function (t) { n++; t.f(); }); } return n; }
  function flush() { tasks(); var q = rafQ; rafQ = []; q.forEach(function (f) { f(); }); return q.length; }
  function flushAll() { var n = 0, g = 0; while ((rafQ.length || taskQ.length) && g++ < 500) n += flush(); return n; }
  function pending() { return rafQ.length + taskQ.length; }
  function key(k, shift) { doc.dispatchEvent({ type: "keydown", key: k, shiftKey: !!shift, ctrlKey: false, metaKey: false, altKey: false, preventDefault: function () {} }); }
  function wheel(dy, x, y) { board.dispatchEvent({ type: "wheel", deltaY: dy, deltaMode: 0, ctrlKey: false, clientX: x, clientY: y, target: board, preventDefault: function () {} }); }
  function pointer(type, id, x, y) { board.dispatchEvent({ type: type, pointerId: id, pointerType: "mouse", button: 0, clientX: x, clientY: y, target: board, preventDefault: function () {} }); }
  function farmdoc(d) { doc.dispatchEvent({ type: "farmdoc", detail: d }); }
  function farmlive(kind, exits) { doc.dispatchEvent({ type: "farmlive", detail: { kind: kind, exits: exits || [] } }); }
  function activity(ev) { doc.dispatchEvent({ type: "farmactivity", detail: ev }); }
  function level() { return win.GARDEN.cam().level; }
  function zoom() { return win.GARDEN.cam().z; }
  /* Zero the counters in place: the contexts hold references to these objects. */
  function reset() { [stats.main, stats.off].forEach(function (s) { Object.keys(s).forEach(function (k) { s[k] = 0; }); }); stats.canvases = 0; stats.sprites = 0; }
  return { stats: stats, reset: reset, tasks: tasks, flush: flush, flushAll: flushAll, pending: pending, key: key, wheel: wheel, pointer: pointer, farmdoc: farmdoc, farmlive: farmlive, activity: activity, level: level, zoom: zoom,
    hots: get("hots"), ghots: get("ghots"), lbls: get("lbls"), bar: get("cursor-bar"), board: board, store: store, W: g.WORLD };
}

module.exports = { boot: boot };
