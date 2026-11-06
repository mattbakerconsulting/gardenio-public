/* TRUCK: the release truck, one per project, in a bay by its sign (gd-yOOVahJMCh; the goats' pattern). Loaded while the
   newest release ships, it drives out when it goes live. Its door says its PR count, its board where it runs or its status and
   since when, and a click opens its changelog (gd-rSz81Me1Pe). Pure but for draw() and the page hooks (truck_test.go). Build notes:
   homestead-world-design.md 26.16. Units: world px, ms. */
var TRUCK = (function () {
  "use strict";
  var W = typeof WORLD !== "undefined" ? WORLD : require("./world.js");
  var A = typeof ART !== "undefined" ? ART : require("./art.js");
  var LN = typeof LANES !== "undefined" ? LANES : require("./lanes.js");
  var T = W.TILE, S = 3, K = S / 2, TW = 24 * S, TH = 16 * S;   // world px per sprite px, per art px; the box (72 x 48, inside the 6 x 4 tile bay)
  var MAX_CRATES = 8, CRATE = 5, PITCH = 6, BED = { x: 4, y: 14, w: 24 }, PLATE = { x: 4, y: 13, w: 22, h: 9 };   // the bed's crates; the tag's side plate (facing right), a gold art px round its letters
  var DRIVE = 3500, AWAY = 1500, BACK = 3500, SIGN = { h: 9, dy: 12 };   // drive out, away, back; the board over the cab
  var DOOR = { x: 27, y: 15, w: 12 }, BOARD = 44;   // the PR count on the cab's door; the widest board over the cab (art px)
  var FONT = {};   // 3 x 5 letters, a row an octal digit (4 the left column); "." and " " one column wide
  "0255521262272612473612164557115746166346527712228757579253161A25755B65656C34443D65556E74647F74644G34553H55755I72227J11152K55655L44447M57755N65555O25552P65644Q25563R65655S34216T72222U55557V55552W55775X55255Y55222Z71247-00700+02720v05552.00004 00000"
    .replace(/(.)(\d{5})/g, function (m, c, b) { FONT[c] = b; });
  /* secs(v) -> unix seconds from RFC 3339 or a number; 0 when neither. */
  function secs(v) { if (typeof v === "number") return v; var t = Date.parse(v || ""); return isFinite(t) ? t / 1000 : 0; }
  /* plansOf(n) -> [{id, count}], most PRs first. */
  function plansOf(n) {
    var p = n.plans || n.plan_counts || n.plan_ids || [], out;
    if (Array.isArray(p)) out = p.map(function (x) { return typeof x === "string" ? { id: x, count: 1 } : { id: x && (x.id || x.plan) || "", count: x && x.count || 0 }; });
    else out = Object.keys(p).map(function (id) { return { id: id, count: p[id] || 0 }; });
    return out.filter(function (x) { return x.id; }).sort(function (a, b) { return b.count - a.count || (a.id < b.id ? -1 : 1); });
  }
  /* norm(blob) -> {<project key>: release} from the releases blob in either shape (step 5's latest/deployments, or 3.5's
     newest/targets): {key, project, tag, id, status, shipping, cut, built, live, count, plans, unplanned, targets [{name, tag, since,
     revision}], recent [{tag, status}], total, shipped {plan: tag}}. */
  function norm(blob) {
    var src = blob && (blob.projects || blob), list = [], out = {};
    if (!src || typeof src !== "object") return out;
    if (Array.isArray(src)) list = src.filter(Boolean).map(function (e) { return [e.project, e]; });
    else list = Object.keys(src).sort().map(function (k) { return [src[k] && src[k].project || k, src[k]]; });
    list.forEach(function (pe) {
      var e = pe[1] || {}, n = e.latest || e.newest || {}, key = W.projectKey(pe[0] || "");
      if (!n.tag || !key || out[key]) return;
      var status = n.status || "cut", plans = plansOf(n), sum = plans.reduce(function (a, p) { return a + p.count; }, 0);
      var count = +(n.count != null ? n.count : n.contents_count != null ? n.contents_count : typeof n.contents === "number" ? n.contents : Array.isArray(n.contents) ? n.contents.length : sum) || 0;
      var recent = (e.recent || []).map(function (r) { return typeof r === "string" ? { tag: r, status: "" } : { tag: r && r.tag || "", status: r && r.status || "" }; }).filter(function (r) { return r.tag; });
      if (!recent.length || recent[0].tag !== n.tag) recent.unshift({ tag: n.tag, status: status });
      var live = status === "live" ? n.tag : (recent.filter(function (r) { return r.status === "live"; })[0] || {}).tag || "";
      var targets = (e.deployments || e.targets || []).filter(function (d) { return d && (d.target || d.name) && !d.until; }).map(function (d) {
        return { name: d.target || d.name, tag: d.tag || d.image_tag || live, since: secs(d.since), revision: d.revision != null ? String(d.revision) : "" };
      }).sort(function (a, b) { return a.name < b.name ? -1 : 1; });
      out[key] = { key: key, project: pe[0], tag: n.tag, id: n.id || "", status: status, shipping: typeof e.shipping === "boolean" ? e.shipping : status === "cut" || status === "built",
        cut: secs(n.cut_at), built: secs(n.built_at), live: secs(n.live_since) || targets.reduce(function (m, d) { return d.tag === n.tag && d.since && !(m && m < d.since) ? d.since : m; }, 0), count: count, plans: plans, unplanned: Math.max(0, count - sum), targets: targets, recent: recent.slice(0, 12), total: +(e.total || e.count) || recent.length,
        shipped: e.shipped && typeof e.shipped === "object" ? e.shipped : {} };
    });
    return out;
  }
  /* bayOf(b) -> biome b's truck bay, the fixed rect under its Barn's keep (homestead-world-design.md 28.10), in tiles {x, y, w, h}. */
  function bayOf(b) { var y = b.home.bay; return { x: y.x, y: y.y, w: y.w, h: y.h }; }
  /* load(rel) -> its crates: one per plan, then one (id "") for the unplanned, at most MAX_CRATES. */
  function load(rel) {
    var out = rel.plans.slice();
    if (rel.unplanned > 0) out.push({ id: "", count: rel.unplanned });
    return out.slice(0, MAX_CRATES);
  }
  /* drive(b, bay, at) -> the truck's way out from its rest box at (world px), its box's top left at each corner: due west along
     its own row onto the west lane, north to the crossroads and east along the north lane, past the sign, until it is off its
     biome's east edge (29.3, X-D11); along its row to that edge when the biome has no lanes. */
  function drive(b, bay, at) {
    var L = LN.laneLines(b), end = b.x + b.w + TW / T / 2 + 1, box = function (p) { return { x: Math.round(p.x * T - TW / 2), y: Math.round(p.y * T - TH / 2) }; };
    if (!L) return [at, { x: (b.x + b.w) * T + T, y: at.y }];
    var o = LN.bayOut(b, L), way = LN.laneWay(L, o[1], { x: end, y: L.north, on: "north" }).map(box);
    way[0] = { x: way[0].x, y: at.y };   // the turn north is on the truck's own row, so it pulls straight west out of the bay
    return [at].concat(way);
  }
  /* place(rels, world) -> a truck per biome with a release: {id (release:<tag>, or release:<key>/<tag> when taken), key, tag,
     rel, bay, x, y, w, h, dir, path (drive's), road (the x its drive ends at: off its biome's edge, past the Pen, 28.10), crates}.
     The box stands centred on the bay's floor, its wheels on the ruts the strip paints there (28.10). */
  function place(rels, world) {
    var out = [], ids = {};
    if (!world || !world.biomes) return out;
    world.biomes.forEach(function (b) {
      var rel = rels[b.key]; if (!rel) return;
      var bay = bayOf(b), id = "release:" + rel.tag, at = { x: bay.x * T + Math.floor((bay.w * T - TW) / 2), y: (bay.y + bay.h) * T - TH }, path = drive(b, bay, at);
      if (ids[id]) id = "release:" + b.key + "/" + rel.tag;
      ids[id] = 1;
      out.push({ id: id, key: b.key, tag: rel.tag, rel: rel, bay: bay, x: at.x, y: at.y, w: TW, h: TH, dir: 1,
        path: path, road: path[path.length - 1].x, crates: load(rel) });
    });
    return out;
  }
  /* depart(prev, rels, t) -> {state, gone}: state[key] = {tag, shipping, crates, at (drive out began; -Infinity), cargo}. A
     loading truck whose release is now live drives out at t and is in gone {key, project, tag, targets}. No prev, none. */
  function depart(prev, rels, t) {
    var state = {}, gone = [];
    Object.keys(rels).sort().forEach(function (k) {
      var n = rels[k], p = prev && prev[k], s = { tag: n.tag, shipping: n.shipping, crates: load(n).length, at: p ? p.at : -Infinity, cargo: p ? p.cargo : 0 };
      if (p && p.shipping && !n.shipping && n.status === "live") {
        s.at = t; s.cargo = p.crates;
        gone.push({ key: k, project: n.project, tag: n.tag, targets: n.targets.filter(function (d) { return d.tag === n.tag; }).map(function (d) { return d.name; }) });
      }
      state[k] = s;
    });
    return { state: state, gone: gone };
  }
  function ease(k) { return k * k * (3 - 2 * k); }
  /* on(pts, k) -> {x, y, dir}: the point a fraction k of the way along pts by distance, whole px, and the way it faces: its
     run across x, else the last run across x before it, else 0. */
  function on(pts, k) {
    var len = 0, i, d = function (a, b) { return Math.abs(b.x - a.x) + Math.abs(b.y - a.y); };
    for (i = 1; i < pts.length; i++) len += d(pts[i - 1], pts[i]);
    var left = k * len, dir = 0;
    for (i = 1; i < pts.length; i++) {
      var a = pts[i - 1], b = pts[i], n = d(a, b);
      if (b.x !== a.x) dir = Math.sign(b.x - a.x);
      if (left <= n && n > 0) return { x: Math.round(a.x + (b.x - a.x) * left / n), y: Math.round(a.y + (b.y - a.y) * left / n), dir: dir };
      left -= n;
    }
    var z = pts[pts.length - 1];
    return { x: z.x, y: z.y, dir: dir };
  }
  /* pose(tr, st, t, still) -> {phase (load, park, out, away, back), x, y, dir, crates, sign}: along its path out, off the map
     away, and back along the same path; still (reduced motion): no drive. The board over the cab (sign) stands while it loads
     or parks, never on the road. */
  function pose(tr, st, t, still) {
    var e = st && isFinite(st.at) ? t - st.at : Infinity, rest = tr.rel.shipping ? tr.crates.length : 0, p;
    if (still || !(e >= 0) || e >= DRIVE + AWAY + BACK) return { phase: tr.rel.shipping ? "load" : "park", x: tr.x, y: tr.y, dir: tr.dir, crates: rest, sign: true };
    if (e < DRIVE) { p = on(tr.path, ease(e / DRIVE)); return { phase: "out", x: p.x, y: p.y, dir: p.dir || tr.dir, crates: st.cargo || 0, sign: false }; }
    if (e < DRIVE + AWAY) { p = tr.path[tr.path.length - 1]; return { phase: "away", x: p.x, y: p.y, dir: tr.dir, crates: 0, sign: false }; }
    p = on(tr.path.slice().reverse(), ease((e - DRIVE - AWAY) / BACK));
    return { phase: "back", x: p.x, y: p.y, dir: p.dir || -tr.dir, crates: 0, sign: false };
  }
  /* textW(s): s's width in glyphs; tagFit(tag, w): the tag, or the tail of dot-parts ("299.2") or characters that fits w. */
  function glyphW(c) { return c === "." || c === " " ? 1 : 3; }
  function textW(s) { var n = 0; for (var i = 0; i < s.length; i++) n += glyphW(s.charAt(i)) + 1; return Math.max(0, n - 1); }
  function tagFit(tag, w) {
    var s = String(tag || ""), parts = s.split(".");
    while (parts.length > 1 && textW(parts.join(".")) > w) parts.shift();
    s = parts.join(".");
    while (s.length > 1 && textW(s) > w) s = s.slice(1);
    return s;
  }
  /* Paints s in glyphs, top left at (x, y). */
  function paintText(ctx, s, x, y, fill) {
    ctx.fillStyle = fill;
    for (var i = 0; i < s.length; i++) {
      var c = s.charAt(i), g = FONT[c] || FONT[c.toUpperCase()] || "";
      for (var j = 0; j < g.length; j++) for (var q = 0; q < 3; q++) if (+g.charAt(j) & 4 >> q) ctx.fillRect(x + q * K, y + j * K, K, K);
      x += (glyphW(c) + 1) * K;
    }
  }
  /* A plate w x h art px in a dark rim, gold unless fill says, top left at (x, y). */
  function plate(ctx, pal, x, y, w, h, fill) { ctx.fillStyle = pal.k; ctx.fillRect(x, y, w * K, h * K); ctx.fillStyle = fill || pal.G; ctx.fillRect(x + K, y + K, (w - 2) * K, (h - 2) * K); }
  /* board(rel, t) -> over the cab: a live release's first target (+N more), else its status, and its age ("PROD 2H"). */
  function age(s) { s = Math.max(0, s); return s < 3600 ? Math.max(1, Math.floor(s / 60)) + "M" : s < 172800 ? Math.floor(s / 3600) + "H" : Math.floor(s / 86400) + "D"; }
  function board(rel, t) {
    var on = rel.targets.filter(function (d) { return d.tag === rel.tag; }), live = rel.status === "live", at = live ? rel.live : rel.status === "built" ? rel.built || rel.cut : rel.status === "cut" ? rel.cut : 0;
    var w = String(live && on.length ? on[0].name : rel.status).toUpperCase(), tail = (live && on.length > 1 ? "+" + (on.length - 1) : "") + (at ? " " + age(t / 1000 - at) : "");
    while (w.length > 1 && textW(w + tail) > BOARD) w = w.slice(0, -1);
    return w + tail;
  }
  /* shippedOf(plan, rels) -> {tag, targets}: the release its newest merged PR shipped in (FarmPlan.shipped, the blob's map,
     else produce[].release) and the targets running it or newer; {tag: "", open: true} when it is on the open release
     (FarmPlan.boarded); null for none. */
  function shippedOf(plan, rels) {
    if (!plan) return null;
    var rel = rels && rels[W.projectKey(plan.project || "")], tag = plan.shipped || rel && rel.shipped[plan.id] || "";
    if (!tag) (plan.produce || []).filter(function (r) { return r && r.state === "merged" && r.release; }).sort(function (a, b) { return (b.updated || 0) - (a.updated || 0); }).slice(0, 1).forEach(function (r) { tag = r.release; });
    if (!tag) return plan.boarded ? { tag: "", open: true, targets: [] } : null;
    var out = { tag: tag, targets: [] };
    if (!rel) return out;
    var order = rel.recent.map(function (r) { return r.tag; }), at = order.indexOf(tag);
    out.targets = rel.targets.filter(function (d) { var i = order.indexOf(d.tag); return d.tag === tag || (at >= 0 && i >= 0 && i < at); }).map(function (d) { return d.name; });
    return out;
  }
  function plural(n, one, many) { return n + " " + (n === 1 ? one : many); }
  /* signWords(rel) -> "3 releases, 1 shipping". */
  function signWords(rel) { return plural(rel.total, "release", "releases") + (rel.shipping ? ", 1 shipping" : ""); }
  var STATUS = { cut: "Cut, not built or deployed yet", built: "Built, not deployed yet", live: "Live", superseded: "Superseded" };
  /* href(tr, base) -> the release's changelog (web/release.js), else the truck on the map; never out of /garden (TestGardenLinksStayInside). */
  function href(tr, base) { var id = tr.rel.id; return (base || "") + (id ? "/garden?release=" + encodeURIComponent(id) : "/garden?at=" + encodeURIComponent(tr.key) + "/" + encodeURIComponent(tr.id)); }
  /* card(tr, base, ago) -> the tip, as WORLD.stewardCard's {title, lines, href, tip}. ago(unix s) -> "3 days". */
  function card(tr, base, ago) {
    var r = tr.rel, lines = [], np = r.plans.length, h = href(tr, base);
    var st = (r.shipping ? "Loading: " : "") + (STATUS[r.status] || r.status) + (r.cut && ago ? " · cut " + ago(r.cut) + " ago" : "");
    var what = plural(r.count, "PR", "PRs") + (np ? " from " + plural(np, "plan", "plans") : "") + (r.unplanned && np ? ", " + r.unplanned + " unplanned" : "");
    var where = r.targets.length ? r.targets.map(function (d) { return d.name + " " + (d.tag === r.tag ? "runs it" : "runs " + d.tag) + (d.since && ago ? " since " + ago(d.since) + " ago" : ""); }).join(" · ") : "No deployments yet";
    lines.push(["rsn" + (r.shipping ? " rsn-sleeping" : ""), st]);
    lines.push(["", what]);
    lines.push(["who", where]);
    lines.push(["why", [{ text: "Open the changelog", href: h, cls: "grel" }]]);
    return { title: "Release " + r.tag, lines: lines, href: h, tip: "Release " + r.tag + " of " + r.project + ". " + st + ". " + what + ". " + where + "." };
  }

  /* ---------- the page's side ---------- */
  var pinned = null, force = false, rels = {}, fed = null, docBlob = null, state = null, docN = 0, placed = [], placedFor = null, placedDoc = -1, said = "", drawn = [], toastFn = null;
  function now() { return pinned === null ? Date.now() : pinned; }
  /* A document's releases (or the fed ones): state moves on; a release gone live toasts. */
  function take(blob) {
    rels = norm(blob); docN++;
    var d = depart(state, rels, now());
    state = d.state;
    if (toastFn) d.gone.forEach(function (g) { toastFn("Released " + g.tag + (g.targets.length ? " to " + g.targets.join(", ") : "") + (g.project ? " (" + g.project + ")" : "")); });
  }
  if (typeof document !== "undefined" && document.addEventListener) document.addEventListener("farmdoc", function (e) { docBlob = (e.detail || {}).releases || null; if (!fed) take(docBlob); });
  /* placedIn(world) -> place(), kept until either changes, each in world.index. */
  function placedIn(world) {
    if (placedFor !== world || placedDoc !== docN) {
      placed = place(rels, world); placedFor = world; placedDoc = docN;
      if (world && world.index) placed.forEach(function (tr) { var b = tr.bay; world.index[tr.id] = { id: tr.id, kind: "truck", x: b.x, y: b.y, w: b.w, h: b.h, project: tr.key, truck: tr }; });
    }
    return placed;
  }
  function meets(a, v) { return a.x < v.x + v.w && v.x < a.x + a.w && a.y < v.y + v.h && v.y < a.y + a.h; }
  /* Draws the trucks in world px, o = {level, night, view, world, reduce}; none below Plan. True while one drives. */
  function draw(ctx, o) {
    drawn = [];
    if (o.level < 2 || !o.world) return false;
    var t = now(), moving = false, pal = A.palette(o.night);
    placedIn(o.world).forEach(function (tr) {
      var p = pose(tr, state && state[tr.key], t, o.reduce && !force), x = p.x, y = p.y, d = p.dir < 0 ? "l" : "r";
      if (p.phase === "out" || p.phase === "away" || p.phase === "back") moving = true;
      if (p.phase === "away" || (o.view && !meets({ x: x, y: y - SIGN.dy * K, w: TW, h: TH + SIGN.dy * K }, o.view))) return;
      A.drawReleaseTruck(ctx, x, y, d, o.night, S);
      var at = function (o) { return x + K * (d === "l" ? 48 - o.x - o.w : o.x); }, bx = at(BED), px = at(PLATE);
      var tag = tagFit(tr.tag, PLATE.w - 4);
      plate(ctx, pal, px, y + PLATE.y * K, PLATE.w, PLATE.h);   // under the crates: its top rim is their bottom edge
      paintText(ctx, tag, px + Math.floor((PLATE.w - textW(tag)) / 2) * K, y + (PLATE.y + 2) * K, pal.k);
      for (var i = 0; i < p.crates; i++) A.drawCrate(ctx, pal, bx + (i % 4) * PITCH * K, y + (BED.y - CRATE - Math.floor(i / 4) * CRATE) * K, K);
      var n = String(Math.min(tr.rel.count, 999)), dx = at(DOOR), b = p.sign ? board(tr.rel, t) : "", bw = textW(b) + 4;
      if (tr.rel.count) paintText(ctx, n, dx + Math.floor((DOOR.w - textW(n)) / 2) * K, y + DOOR.y * K, pal.j);
      if (b) {
        var sx = x + Math.floor((48 - bw) / 2) * K, sy = y - SIGN.dy * K, st = tr.rel.status;
        plate(ctx, pal, sx, sy, bw, SIGN.h, st === "live" ? pal.G : st === "superseded" ? pal.J : pal.j);
        ctx.fillStyle = pal.k; ctx.fillRect(sx + Math.floor(bw / 2) * K, sy + SIGN.h * K, K, (SIGN.dy - SIGN.h) * K);
        paintText(ctx, b, sx + 2 * K, sy + 2 * K, pal.k);
      }
      drawn.push({ id: tr.id, key: tr.key, tag: tr.tag, phase: p.phase, x: x, y: y, dir: p.dir, crates: p.crates, sign: p.sign, board: b, door: tr.rel.count ? n : "" });
    });
    return moving;
  }
  /* hots(world) -> the trucks' links for garden.js's #ghots, at Plan and Bead. */
  function hots(world) { return placedIn(world).map(function (tr) { return { kind: "truck", id: tr.id, rect: tr.bay, lv: 4 | 8, tr: tr }; }); }
  /* line(n, level, cap): the release words of cursor Thing n's status line in the title bar (garden.js syncBar): a biome's
     signWords at Project, a plot's "shipped in <tag> · <targets>" at Plan and Bead; past cap (0: none) the targets go, then
     the tag's head. */
  function line(n, level, cap) {
    var rel, sh;
    said = sig();
    if (!n) return "";
    if (level === 1 && n.kind === "biome" && (rel = rels[n.key])) return signWords(rel);
    if (level >= 2 && n.kind === "plot" && n.ref && (sh = shippedOf(n.ref.plan || n.ref, rels))) return shipWords(sh, cap);
    return "";
  }
  function shipWords(sh, cap) {
    if (sh.open) return !cap || cap >= 26 ? "on the next release (open)" : "next release";
    var head = "shipped in ", w = head + sh.tag + (sh.targets.length ? " · " + sh.targets.join(", ") : ""), parts = String(sh.tag).split(".");
    if (!cap || w.length <= cap) return w;
    while (parts.length > 1 && (head + parts.join(".")).length > cap) parts.shift();
    return head + parts.join(".");
  }
  function sig() { return Object.keys(rels).sort().map(function (k) { var r = rels[k]; return k + ":" + r.tag + ":" + r.status + ":" + r.total + ":" + r.targets.length; }).join(","); }
  /* shipped(plan): the plot's crate stamp? */
  function shipped(plan) { var sh = shippedOf(plan, rels); return !!sh && !sh.open; }

  return {
    S: S, K: K, TW: TW, TH: TH, MAX_CRATES: MAX_CRATES, CRATE: CRATE, PITCH: PITCH, BED: BED, PLATE: PLATE, DRIVE: DRIVE, AWAY: AWAY, BACK: BACK, SIGN: SIGN, DOOR: DOOR, BOARD: BOARD, FONT: FONT,
    norm: norm, board: board, age: age, bayOf: bayOf, drive: drive, on: on, place: place, load: load, depart: depart, pose: pose, tagFit: tagFit, textW: textW, shippedOf: shippedOf, signWords: signWords, card: card, href: href,
    draw: draw, hots: hots, line: line, shipWords: shipWords, shipped: shipped,
    /* has the project of key any release? (its strip's bay ruts, homestead-world-design.md 28.10) */
    has: function (key) { return !!rels[key]; },
    /* have the signs' release words changed? */
    stale: function () { return sig() !== said; },
    /* the page's toast */
    wire: function (env) { toastFn = env && env.toast || null; },
    /* golden hooks: feed a blob (null: the document's), pin the clock, force motion, read the last draw */
    feed: function (blob) { fed = blob || null; take(fed || docBlob); if (typeof document !== "undefined" && document.dispatchEvent && typeof CustomEvent !== "undefined") document.dispatchEvent(new CustomEvent("trucks")); },
    pin: function (ms) { pinned = ms === undefined ? null : ms; },
    motion: function (on) { force = !!on; },
    list: function () { return drawn.slice(); },
    /* the newest release of a project by its bead id (the release panel's title and count, web/release.js), or null */
    release: function (id) { var k = Object.keys(rels).filter(function (k) { return rels[k].id === id; })[0]; return k ? rels[k] : null; }
  };
})();
if (typeof module !== "undefined") module.exports = TRUCK;
