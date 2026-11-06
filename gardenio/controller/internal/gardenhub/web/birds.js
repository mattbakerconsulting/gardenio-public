/* BIRDS: tool calls as birds (gd-pLFocqmYyf, step 4b of plan gd-gAKb8Bf0Nm; spec gd-zE5JlcI54m section 4b). Each call a gardener
   starts (an SSE activity event, which plans.js hands over as `farmactivity`; ACTIVITY.started says how many it folds) lifts one bird
   off its head. The species says what kind of work: a sparrow reads, a finch edits, a crow runs a shell, a test or a build, a bird
   with a twig works git, a pigeon writes beads, a gull deploys, a swift goes to the web and leaves the screen, a robin says (what
   the gardener said, gd-HHGjxt59Mr). It floats up off the head (gd-CykqEHeEpX, the owner: "the birds float up and just leave a
   caption on the calling agent's head"): straight up RUN world px, then on up and away, left or right by turns, gone in 1.5-2 s
   (a flock's later birds each a body behind and a little aside, the flock quicker when it must be so its last is gone in 2 s too).
   A subagent is a hawk that circles over the head until it stops, then lands on it (the owner's default on the Phase 4 decision:
   no second figure). Birds are 10 x 6 and 14 x 8 sprites of four colours (body, wing or back, belly or beak, eye; gd-cFZPuAjqJu)
   of three frames, a flap up, level, down, level (the hawk a fourth, its glide; gd-BuWCnuLYQv), on the dynamic layer, drawn
   before the gardeners so a figure is never under a bird, in device px at a whole number of them per sprite px: PX CSS px at the
   Plan level (gd-7dRp5a6r15: at its 0.49x a world-px bird was two or three screen px), rounded, so 3 device px at DPR 2 (a crow
   21 CSS px wide) and 2 at DPR 1 (28 px), and at the Bead level ZK times the zoom but never under BEAD_PX, so zooming in makes a
   bird bigger (gd-Wucpbrm3Z6). Each is ringed in an outline: by day a dark bird in a pale one and a bright bird in a dark one, so
   it reads on grass, dirt and sand alike; at night a dark bird in a pale one so it shows on dark ground. Plan and Bead level only,
   only in view; at most MAX live, at most FLOCK in flight per gardener (a fold of more flies as a flock of FLOCK), none for QUIET
   ms after the stream (re)opens. Reduced motion: one still bird perched over the figure's head for STILL ms (over it, not beside
   it, so a figure a tile away never covers the bigger bird). World and Project level: no birds; a project's sign says "busy" when
   it had activity in the last BUSY_S seconds.
   Its caption stays on the gardener's head (gd-CykqEHeEpX; until then it flew with the bird, spec 2026-10-28-bird-captions-design.md
   9.1): the call's `cap` off the wire ("Show plan ready frontier", made and scrubbed in the hub), else its tool word, "Read",
   "Edit", "go test", "git push", "shell" (gd-VLVayQzQIB, 4k: ACTIVITY.toolWord, the card's own map, short), on a rounded PLATE
   perched over the head from the call for LINGER ms (spec 9.2), solid at once and fading out over its last TAG_FADE ms (no fade
   under reduced motion): TAG_PX bold ink on the label's halo colour, ringed in its ink, at most PLATE.phone CSS px wide on a phone and
   PLATE.wide otherwise, wrapped at words to two lines, the second ending in "…" when it still overflows (wrap()). One per
   gardener, the newest call's: its next call's plate replaces it at once, bird or no bird (a hawk's, its subagent's description
   else "subagent", stays up while it circles); at most MAX plates on the farm, the oldest going first. A late caption (a
   `farmdoing` with the call's `at`, spec 7.2) rewrites the plate still up. None below the Plan level.
   The words go on top (gd-VHkZ3pCEqK, 4n): draw() places them with the birds, under the figures, and words() draws them after
   the figures, their bubbles and the transitions, so nothing on the canvas covers one. A plate is placed each frame (keepOff,
   which also keeps off the figures a transition walks): over the head, else over it off to the right or the left (its edge on
   the head's middle, so a crowded row keeps more), else its right, else its left, or dropped, never drawn
   over a face, a figure's "?" bubble, the plot's "?" bubble, a sign's tip, the cursor's ring (a DOM outline over the canvas), a
   still bird or a hawk; a flying bird passes under it. Under reduced motion it stands beside the still bird (its right, else its
   left, else over it), level with it, or with its foot when taller, so a two-line plate grows up and away from the figure; a
   still bird on another figure's face is hidden under that figure, so its plate, which would seem to name that one, is dropped.
   One that would overlap a newer one is dropped too, and one is kept EDGE px inside the screen. Each plate is measured, wrapped
   and baked once per caption, theme, DPR and width (a drawImage a plate). Only the caption or the tool word: never the call's
   input, its command or its output.
   A plate is a way to its gardener's Glass (gd-jt6OwmV4Jk, spec 10): a long press or a right-click on it, or the w key (CURNAV
   "watch"), opens /garden?glass=<gardener>; see presser() and wire() below.
   A gardener at home flies its birds from its figure at its field stand (5b, gd-1kFVOmUjiK: a thread agent answering in the
   Commons runs the same hooks; WORLD.homeTile, homestead-world-design.md 28.4, 28.12): up off the figure as from a plot, kept
   inside its strip's Growing field's columns (fieldSky). Each field counts as one plot: at most BARN_MAX of its birds in
   flight. One with no figure drawn (past the field's "+N", or out of the Mine scope) has none; one out on a plot flies them
   from its plot figure. Honest limit: a bird from a back row crosses the figures in front of it.
   route(), at(), admit(), hawkAt(), fieldSky(), speciesOf(), tagOf(), wrap(), pick() and place() are pure and run under node
   (birds_test.go); garden.js calls draw() per render before the figures, words() after them, and sign() per refresh of the
   signs. Units: world px, ms. */
var BIRDS = (function () {
  "use strict";
  var W = typeof WORLD !== "undefined" ? WORLD : require("./world.js");
  var ACT = typeof ACTIVITY !== "undefined" ? ACTIVITY : require("./activity.js");
  var AN = typeof ANIM !== "undefined" ? ANIM : require("./transitions.js");
  var SPEED = 48, DUR = [1500, 2000], RUN = 64, SWIFT_RUN = 160;   // world px per second, a flight's time (ms), the run along the path
  var FLAP = 125, SOAR = 400, LAP = 2400, LAND = 500, FLOCK_GAP = 90, HAWK_UP = 14;   // a wing beat, a hawk's slower one, its lap and landing (ms); how far over the head it circles (world px)
  var BEAT = [0, 1, 2, 1], GLIDE = [3, 3, 0, 1, 2, 1];   // the frames of a flap, a beat each: up, level, down, level; a hawk's lap of SOAR beats, a glide and one flap
  var BARN_MAX = 8, MAX = 24, FLOCK = 3, QUIET = 2000, STILL = 1000, STALE = 1000, BUSY_S = 30, HAWK_FORGET = 30 * 60000;
  var PX = 1.5, BEAD_PX = 2, ZK = 1.25;   // CSS px per sprite px at Plan (a crow 21 px wide at DPR 2); at Bead ZK x the zoom, never under BEAD_PX
  var RIM = "#e6e2cc", DAY_RIM = "#fff8e6", INK = "#2a1a14";   // the outlines: a dark bird's at night and by day, a bright bird's by day
  var TAG_PX = 10, TAG_GAP = 2, TAG_FADE = 300, EDGE = 8, TAG_CAP = 256;   // a label's font and gap (CSS px), its fade (ms), the screen edge kept, the plates baked
  var LINGER = 6000;   // a plate perched by its gardener once its bird is gone (ms, spec 9.2: presentation, not cost, so not a Helm value)
  /* a plate (CSS px): its width at most phone under a viewport of PHONE_W, wide else; its padding (x, y), corner, ring and line
     height (x the font); its fill the halo at fill alpha, its ring the ink at rim alpha (spec 9.1) */
  var PLATE = { phone: 200, wide: 240, PHONE_W: 600, px: 5, py: 3, r: 4, lh: 1.2, lines: 2, fill: 0.92, rim: 0.35 };
  var SPREAD = 3, RING = 5;   // a flock's later birds this far aside (world px); the cursor's ring this far out of its box (CSS px, garden.css .hot.cur)
  var BUBBLE = { dx: 9, dy: -13, w: 14, h: 14 };   // a figure's bubble off its top left (ART.drawBubble at 9, -12, bobbing a px up)
  var HEAD = { dx: -3, dy: 4, w: 22, h: 11 };   // its face: the hat's brim down to a kneeler's chin, as wide as the padded map (its rim and a pose's props)
  var TAG = { day: [INK, DAY_RIM], night: ["#f2f2ee", "#16161e"] };   // a label's ink and its halo
  var T = W.TILE, INKS = "abcdef";   // a sprite's letters, each its colour's place in the palette

  /* The sprites, seen from under the bird and symmetric, so one frame flies every way: rows of the letters a to f, each the
     colour at its place in the palette (a the body, b the wing or back, c the belly or beak, d the eye), and . (none), three
     frames each (wings up, wings level, wings down: a flap is up, level, down, level; gd-BuWCnuLYQv) and the hawk a fourth, its
     glide (wings flat, tail spread), which it soars and lands in. The small birds are 10 x 6 (the sparrow, brown with a pale belly; the finch, yellow
     with olive wing tips; the pigeon, grey with a pale chest and an orange eye; the robin, brown with a red breast), the rest 14 x 8 (the crow, black with fingered
     wings; the twig bird, its green twig (c) in its beak; the gull, white with grey wing tips; the swift, scythe wings and a
     forked tail; the hawk, barred wings and the pale band across its belly and tail it circles with). Day and night palettes,
     a colour per letter the sprite uses; a, the body, is the one held against the ring and the ground. At night a dark bird is
     drawn over a RIM outline, so it still shows on dark ground and keeps its colours inside the ring; a bright one (rim:
     false: the finch, the gull) shows by its own colour. By day a dark bird is drawn over DAY_RIM, dark enough itself to show on
     the lit grass and pale soil where that ring is faint, and a bright one over INK: one of the two always stands 3:1 off the
     grass and the dirt (birds_test.go). */
  var SP = {
    sparrow: {
      f: [["b........b", "ab..aa..ba", ".abdaadba.", "..aaccaa..", "...acca...", "....bb...."],
          ["..........", "....aa....", "bbbdaadbbb", "aaaaccaaaa", "...acca...", "....bb...."],
          ["..........", "....aa....", "..bdaadb..", ".baaccaab.", "ba.acca.ab", "b...bb...b"]],
      day: ["#5e3a1e", "#a8743e", "#d8b98c", "#f4e8c8"], night: ["#8a6440", "#b48c5c", "#dcc296", "#f4e8c8"]
    },
    finch: {
      f: [["b........b", "bb..aa..bb", ".badaadab.", "..aaccaa..", "...aaaa...", "....bb...."],
          ["..........", "....aa....", "bbadaadabb", "bbaaccaabb", "...aaaa...", "....bb...."],
          ["..........", "....aa....", "..adaada..", ".aaaccaaa.", "bb.aaaa.bb", "b...bb...b"]],
      day: ["#e2b51c", "#5f6e24", "#e07a28", "#2a1a14"], night: ["#f6d244", "#a8c460", "#f09a48", "#2a1a14"], rim: false
    },
    pigeon: {
      f: [["b........b", "bb..aa..bb", ".bbdaadbb.", "..aaccaa..", "..acccca..", "...aaaa..."],
          ["..........", "....aa....", "bbbdaadbbb", "bbaaccaabb", "..acccca..", "...aaaa..."],
          ["..........", "....aa....", "..bdaadb..", ".bbaccabb.", "bb.acca.bb", "b..aaaa..b"]],
      day: ["#3c4658", "#7c8698", "#b9c1cc", "#f08a30"], night: ["#6c7a92", "#94a0b4", "#c4ccd8", "#f8a040"]
    },
    robin: {
      f: [["b........b", "ab..aa..ba", ".abdaadba.", "..acccca..", "...acca...", "....bb...."],
          ["..........", "....aa....", "bbbdaadbbb", "aaaccccaaa", "...acca...", "....bb...."],
          ["..........", "....aa....", "..bdaadb..", ".baccccab.", "ba.acca.ab", "b...bb...b"]],
      day: ["#5a3c24", "#8c6a44", "#e45a26", "#f4e8c8"], night: ["#8a6448", "#b08a62", "#f27a46", "#f4e8c8"]
    },
    crow: {
      f: [["a.a........a.a", "aaab......baaa", ".aaab.aa.baaa.", "..aaadaadaaa..", "...aaaccaaa...", "....aaaaaa....", ".....aaaa.....", "....aaaaaa...."],
          ["..............", "..............", "......aa......", ".bbbadaadabbb.", "aaaaaaccaaaaaa", "a.a.aaaaaa.a.a", ".....aaaa.....", "....aaaaaa...."],
          ["..............", "..............", "......aa......", "...badaadab...", "..baaaccaaab..", ".baa.aaaa.aab.", ".aa..aaaa..aa.", "a.a.aaaaaa.a.a"]],
      day: ["#1c1c24", "#3a3a4c", "#5a5a68", "#c8c8d8"], night: ["#26262f", "#44445a", "#a0a0b6", "#d8d8e8"]
    },
    twig: {
      f: [["b............b", "bb..........bb", ".ab...aa...ba.", "..ab.daad.ba..", "....aaaaaa....", ".ccccaaaacccc.", ".c...aaaa...c.", "......aa......"],
          ["..............", "..............", "......aa......", "bbbbbdaadbbbbb", "aaaaaaaaaaaaaa", ".ccccaaaacccc.", ".c...aaaa...c.", "......aa......"],
          ["..............", "..............", "......aa......", "...bbdaadbb...", ".baaaaaaaaaab.", "bccccaaaaccccb", "bc...aaaa...cb", "......aa......"]],
      day: ["#5a3a22", "#8a6038", "#6f8f2e", "#f0e0c0"], night: ["#8e6648", "#a87c58", "#9cc45c", "#f4e8cc"]
    },
    gull: {
      f: [["b............b", "bb..........bb", ".aa...aa...aa.", "..aa.daad.aa..", "...aaaccaaa...", ".....aaaa.....", ".....aaaa.....", "......aa......"],
          ["..............", "..............", "......aa......", ".aaaadaadaaaa.", "bb..aaccaa..bb", ".....aaaa.....", ".....aaaa.....", "......aa......"],
          ["..............", "..............", "......aa......", "....adaada....", "...aaaccaaa...", "..aa.aaaa.aa..", ".aa..aaaa..aa.", "bb....aa....bb"]],
      day: ["#f2f2ee", "#8a8f96", "#e8b830", "#2a2a2a"], night: ["#f2f2ee", "#8a9098", "#e0b040", "#2a2a2a"], rim: false
    },
    swift: {
      f: [["b............b", "bb..........bb", ".bb...aa...bb.", "..bbbdaadbbb..", "....baccab....", ".....aaaa.....", "....aa..aa....", "....a....a...."],
          ["..............", "..............", "......aa......", "...bbdaadbb...", ".bbbbaccabbbb.", "bb...aaaa...bb", "....aa..aa....", "....a....a...."],
          ["..............", "..............", "......aa......", "....bdaadb....", "...bbaccabb...", "..bb.aaaa.bb..", ".bb.aa..aa.bb.", "bb..a....a..bb"]],
      day: ["#23304a", "#34466a", "#e8e8e8", "#c8d0e0"], night: ["#3c5490", "#5068a8", "#e0e4ec", "#e0e4ec"]
    },
    hawk: {
      f: [["a.a........a.a", "abab......baba", ".abab.aa.baba.", "..abadaadaba..", "...aaaccaaa...", "....acccca....", ".....cccc.....", "....aaaaaa...."],
          ["..............", "..............", "......aa......", "ababadaadababa", "bababaccababab", ".aaaaccccaaaa.", ".....cccc.....", "....aaaaaa...."],
          ["..............", "..............", "......aa......", "...abdaadba...", "..abaaccaaba..", "aba.acccca.aba", "ab...cccc...ba", "a...aaaaaa...a"],
          ["..............", "..............", "......aa......", "ababadaadababa", "bababaccababab", "....acccca....", "...acccccca...", "..aaaaaaaaaa.."]],
      day: ["#6e2a10", "#c8925a", "#efdcb0", "#f0c020"], night: ["#b4562c", "#e0a070", "#e6d0a2", "#f4d040"]
    }
  };
  /* each frame as runs of one colour {x, y, w, c, i}, c its letter and i that letter's place in the palette, so a bird is a few
     fillRects */
  Object.keys(SP).forEach(function (k) {
    var s = SP[k];
    s.w = s.f[0][0].length; s.h = s.f[0].length; s.rim = s.rim !== false;
    s.runs = s.f.map(function (rows) {
      var out = [];
      rows.forEach(function (row, y) {
        for (var x = 0; x < row.length; x++) {
          var c = row.charAt(x), i = INKS.indexOf(c);
          if (c === ".") continue;
          if (i < 0) throw new Error("birds: " + k + " has " + JSON.stringify(c) + ", not a to f");
          var r = out[out.length - 1];
          if (r && r.y === y && r.c === c && r.x + r.w === x) r.w++; else out.push({ x: x, y: y, w: 1, c: c, i: i });
        }
      });
      return out;
    });
  });
  var BY = { read: "sparrow", edit: "finch", test: "crow", build: "crow", shell: "crow", git: "twig", beads: "pigeon", deploy: "gull", web: "swift", delegate: "hawk", say: "robin" };
  /* The species of a doing class, or "" for none (a thought, waiting). */
  function speciesOf(doing) { return BY[doing] || ""; }
  /* The word a bird of a call carries: its tool, short (ACTIVITY.toolWord), else its class. */
  function tagOf(doing, tool) { return doing === "delegate" ? "subagent" : ACT.toolWord(tool || "", true) || doing || ""; }
  /* The text on a bird's plate: the event's caption, its whitespace collapsed, else the tool word (tagOf). */
  function plateOf(ev) { return String(ev.cap || "").replace(/\s+/g, " ").trim() || tagOf(ev.doing, ev.tool); }
  /* wrap(text, max, meas) -> its lines on a plate max wide (meas(s): s's width, the same units): at word boundaries, at most
     PLATE.lines; a word wider than max breaks inside it, and the last line, still too wide, is cut back to a word (or a
     character) and ends in "…". Characters are code points, so a cut never splits one. */
  function wrap(text, max, meas) {
    var words = String(text).split(" ").filter(Boolean), out = [];
    var fit = function (s, tail) {   // the longest start of s that fits with tail after it, at a word boundary when one fits
      var cs = Array.from(s), lo = 0, hi = cs.length;   // the width grows with n: a binary search for the largest n that fits
      while (lo < hi) { var mid = (lo + hi + 1) >> 1; if (meas(cs.slice(0, mid).join("") + tail) <= max) lo = mid; else hi = mid - 1; }
      var n = lo;
      var cut = cs.slice(0, n).join(""), sp = cut.lastIndexOf(" ");
      return n < cs.length && sp > 0 && cs[n] !== " " ? cut.slice(0, sp) : cut.replace(/ +$/, "");
    };
    while (words.length && out.length < PLATE.lines) {
      var line = words.shift(), last = out.length === PLATE.lines - 1;
      if (last) { var rest = [line].concat(words).join(" "); words = []; out.push(meas(rest) <= max ? rest : fit(rest, "…") + "…"); break; }
      if (meas(line) > max) { var head = fit(line, ""); words.unshift(line.slice(head.length)); out.push(head); continue; }
      while (words.length && meas(line + " " + words[0]) <= max) line += " " + words.shift();
      out.push(line);
    }
    return out;
  }
  /* pick(cands) -> the labels to try, newest first: of each gardener's candidates {id, t0} only the newest. */
  function pick(cands) {
    var by = {};
    cands.forEach(function (c) { if (!by[c.id] || c.t0 > by[c.id].t0) by[c.id] = c; });
    return Object.keys(by).map(function (k) { return by[k]; }).sort(function (a, b) { return b.t0 - a.t0 || (a.id < b.id ? -1 : 1); });
  }
  function hit(a, b) { return a.x < b.x + b.w && b.x < a.x + a.w && a.y < b.y + b.h && b.y < a.y + a.h; }
  /* place(list, scr, keep) -> the labels drawn: each {x, y, w, h} (and its bird's centre cx, cy) in order, kept e px inside the
     screen scr {w, h, e} (none: anywhere) and dropped when its bird is off it. One that would overlap one placed before it or a
     box of keep tries its next spot (alt, [{x, y}], the other sides of its bird, each maybe its own size w, h and the fields
     it brings: a smaller plate's) and is dropped when none is clear. */
  function place(list, scr, keep) {
    var out = [];
    list.forEach(function (l) {
      if (scr && (l.cx < 0 || l.cy < 0 || l.cx >= scr.w || l.cy >= scr.h)) return;
      var tries = [{ x: l.x, y: l.y }].concat(l.alt || []);
      for (var i = 0; i < tries.length; i++) {
        var tr = tries[i], x = tr.x, y = tr.y, w = tr.w || l.w, h = tr.h || l.h, k;
        if (scr) { x = Math.max(scr.e, Math.min(scr.w - scr.e - w, x)); y = Math.max(scr.e, Math.min(scr.h - scr.e - h, y)); }
        var b = { x: x, y: y, w: w, h: h };
        if (out.some(function (o) { return hit(o, b); }) || (keep || []).some(function (q) { return hit(q, b); })) continue;
        for (k in l) if (!(k in b) && k !== "alt") b[k] = l[k];
        for (k in tr) if (!(k in b) || (k !== "x" && k !== "y" && k !== "w" && k !== "h")) b[k] = tr[k];
        out.push(b);
        return;
      }
    });
    return out;
  }
  var ON_HEAD = ["o", "or", "ol", "r", "l"], BY_BIRD = ["r", "l", "o"];   // the sides a plate tries: over the head centred, over it off to the right or the left (its edge on the head's middle: a crowded row), else beside it right or left; beside a still bird its right, left, then over it
  /* fieldSky(world, biomeId) -> that biome's Growing field as a plot for birds, in world px, or null (28.12): barn, the biome
     id (admit's cap, BARN_MAX per home); at, its top row's middle; x0 and x1, its columns, which keep a bird over the field. */
  function fieldSky(world, biomeId) {
    var b = world && world.index[biomeId], c = b && b.home && b.home.cells.growing;
    return c ? { barn: biomeId, at: c.y * T + T / 2, x0: c.x * T, x1: (c.x + c.w) * T - T } : null;
  }
  /* The head of a gardener standing at spot s (the 16 x 24 sprite's top left). */
  function head(s) { return { x: s.x + 8, y: s.y + 4 }; }
  /* route(s, sp, seed, sky) -> {pts, dur}: a bird's flight up off the head of the gardener at spot s: straight up RUN world px
     (a swift SWIFT_RUN), then on up and away, left or right by seed, half as far again; at home (sky: fieldSky) kept inside
     the field's columns. dur is its length at SPEED, kept to DUR. */
  function route(s, sp, seed, sky) {
    var h = head(s), run = sp === "swift" ? SWIFT_RUN : RUN, dx = seed % 2 ? run / 2 : -run / 2, x2 = h.x + dx;
    if (sky && sky.barn) { if (x2 < sky.x0 || x2 > sky.x1) x2 = h.x - dx; x2 = Math.max(sky.x0, Math.min(sky.x1, x2)); }
    return { pts: [h, { x: h.x, y: h.y - run }, { x: x2, y: h.y - run - run / 2 }], dur: Math.max(DUR[0], Math.min(DUR[1], 2 * run / SPEED * 1000)) };
  }
  /* at(b, t) -> {x, y, f} | {done} | null (not off yet, a flock's later bird): where a bird is at t, its centre, and its frame (a
     beat of its flap, BEAT; a still bird frame 0). */
  function at(b, t) {
    var e = t - b.t0;
    if (e < 0) return null;
    if (e >= b.dur) return { done: true };
    if (b.still) return { x: b.pts[0].x, y: b.pts[0].y, f: 0 };
    var p = AN.along(b.pts, e / b.dur);
    return { x: p.x, y: p.y, f: BEAT[Math.floor(e / FLAP) % 4] };
  }
  /* admit(live, id, k, barn) -> how many of k birds gardener id may launch: at most FLOCK in flight for it, MAX in all, and for
     a figure at home (barn: its biome id) BARN_MAX over its field. */
  function admit(live, id, k, barn) {
    var mine = 0, ours = 0;
    live.forEach(function (b) { if (b.id === id) mine++; if (barn && b.barn === barn) ours++; });
    return Math.max(0, Math.min(k, FLOCK - mine, MAX - live.length, barn ? BARN_MAX - ours : MAX));
  }
  /* A hawk's centre and frame at t: laps over its gardener's head (an ellipse across it, HAWK_UP up), gliding and now and then
     a flap (GLIDE), then, once it lands, down onto the head in its glide. */
  function hawkAt(hk, s, t) {
    var h = head(s), c = { x: h.x, y: h.y - HAWK_UP };
    var lap = function (tt) { var a = 2 * Math.PI * (tt - hk.t0) / LAP; return { x: c.x + 14 * Math.cos(a), y: c.y + 4 * Math.sin(a) }; };
    if (!hk.land) return { x: lap(t).x, y: lap(t).y, f: GLIDE[Math.floor((t - hk.t0) / SOAR) % GLIDE.length] };
    var k = (t - hk.land) / LAND;
    if (k >= 1) return { done: true };
    var q = AN.along([lap(hk.land), h], k);
    return { x: q.x, y: q.y, f: 3 };
  }

  /* ---------- the page's side ---------- */
  var pinned = null, force = false, openAt = -Infinity, pending = [], live = [], hawks = {}, busyAt = {}, projOf = {}, said = "", wakeFn = null, seq = 0, tagged = [], due = false;
  var perch = {}, timer = null;   // the plates perched by gardener id {id, sp, t0, ca, w, tw, from, end, fade}; the wake for the next fade
  var scale = 1, inkNow = INK, held = "", names = {};   // the last draw's CSS px to its frame and its ink; the gardener a press holds (its ring lit); the gardeners' names
  function now() { return pinned === null ? Date.now() : pinned; }
  function wake() { if (wakeFn) wakeFn(); }
  /* One activity event: the project is busy; a call lifts its birds (once the stream has been open QUIET ms); a subagent's start
     calls its gardener's hawk up and its stop lands it. */
  function hear(ev) {
    if (!ev || !ev.id) return;
    var t = now(), key = projOf[ev.id];
    if (key) busyAt[key] = t;
    if (t - openAt < QUIET) return;
    if (ev.doing === "delegate") {
      if (ev.dur === 0 && !hawks[ev.id]) { hawks[ev.id] = { t0: t, land: 0, fresh: true, ca: ev.at, w: plateOf(ev), tw: tagOf(ev.doing, ev.tool) }; wake(); }
      else if (ev.dur > 0 && hawks[ev.id] && !hawks[ev.id].land) {
        hawks[ev.id].land = t; wake();
        var pc = perch[ev.id];
        if (pc && pc.t0 === hawks[ev.id].t0) pc.end = Math.max(pc.end, t + LAND + LINGER);   // its plate lingers on from the landing
      }
      return;
    }
    var sp = speciesOf(ev.doing), k = ACT.started(ev);
    if (!sp || k <= 0) return;
    pending.push({ id: ev.id, sp: sp, k: k, at: t, ca: ev.at, w: plateOf(ev), tw: tagOf(ev.doing, ev.tool) });
    if (pending.length > 4 * MAX) pending.shift();
    wake();
  }
  /* A late caption (a `farmdoing` whose cap the hub's Haiku wrote after the call went out, spec 7.2): the gardener's plate of the
     call that started at its `at`, waiting or perched, takes it. */
  function recap(d) {
    var w = String((d && d.cap) || "").replace(/\s+/g, " ").trim();
    if (!w || !d.id || d.at == null) return;
    var one = function (x) { if (x && x.w && x.ca === d.at) x.w = w; };
    pending.forEach(function (x) { if (x.id === d.id) one(x); });
    one(hawks[d.id]); one(perch[d.id]);
    wake();
  }
  if (typeof document !== "undefined" && document.addEventListener) {
    document.addEventListener("farmactivity", function (e) { hear(e.detail); });
    document.addEventListener("farmdoing", function (e) { recap(e.detail); });
    /* Below the Plan level the stream sends no calls (web/viewcut.js), only the projects busy and how many seconds ago. */
    document.addEventListener("farmbusy", function (e) { var b = (e.detail || {}).busy || {}, t = now(); Object.keys(b).forEach(function (p) { var k = W.projectKey(p); if (k) busyAt[k] = Math.max(busyAt[k] || 0, t - 1000 * (b[p] || 0)); }); });
    document.addEventListener("farmlive", function (e) { if ((e.detail || {}).kind === "open") { openAt = now(); pending = []; live = []; hawks = {}; perch = {}; } });
    document.addEventListener("farmdoc", function (e) {
      projOf = {};
      (((e.detail || {}).gardeners) || []).forEach(function (g) { var k = W.projectKey(g.project); if (k) projOf[g.id] = k; names[g.id] = g.agent || g.id; });
    });
  }
  function inView(pt, v) { return !v || (pt.x >= v.x && pt.x < v.x + v.w && pt.y >= v.y && pt.y < v.y + v.h); }
  /* The gardener id's spot and plot at this level when it stands shown on a plot (and is not on its way there), or at home
     where its figure is drawn at its field stand (homeAt; fieldSky the plot), else null. */
  function homeAt(o, id) { var h = o.gs && o.gs.byId && W.homeTile(o.world, o.gs.byId[id]); return h && h.drawn ? h : null; }
  function standing(o, lv, id) {
    var s = lv.spots[id], p = s && s.shown ? o.world.index[s.plot] : null, h = s && s.home ? homeAt(o, id) : null;
    if (h) { var sky = fieldSky(o.world, h.biome); return o.moving && o.moving(id) || !sky ? null : { s: h, p: sky }; }
    return p && !(o.moving && o.moving(id)) ? { s: s, p: p } : null;
  }
  /* The births of one pending event: its plate on the head at once (bird or no bird), then k birds of its species (a flock, the
     later ones a beat behind and a little aside), or one still bird under reduced motion. The beat is FLOCK_GAP, or the time to
     fly a bird's own drawn height (sc: screen()) once the bird is drawn bigger than its world size, so a flock reads as birds in
     a line and not one blot; the flock flies that much faster when the beats would keep its last bird up past DUR[1]. */
  function launch(q, lv, o, t, sc) {
    var at0 = standing(o, lv, q.id);
    if (!at0 || t - q.at > STALE || !inView(head(at0.s), o.view)) return;
    settle(q.id, t, q, t);
    if (o.reduce) {
      if (live.some(function (b) { return b.id === q.id; }) || !admit(live, q.id, 1, at0.p.barn)) return;
      var h = head(at0.s);
      live.push({ id: q.id, sp: q.sp, still: true, pts: [{ x: h.x, y: h.y - 5 }], t0: t, dur: STILL, barn: at0.p.barn || "" });
      return;
    }
    var n = admit(live, q.id, q.k, at0.p.barn), rs = [], j, i;
    if (n <= 0) return;
    for (j = 0; j < n; j++) rs.push(route(at0.s, q.sp, seq++, at0.p));
    var len = 0;
    for (i = 1; i < rs[0].pts.length; i++) len += Math.abs(rs[0].pts[i].x - rs[0].pts[i - 1].x) + Math.abs(rs[0].pts[i].y - rs[0].pts[i - 1].y);
    /* b: a drawn height as a share of the route. The flock flies faster than one bird when it must, so its last bird is gone
       DUR[1] after the lift too (gd-ZnKiMMTpko); each beat stays a drawn height at the flock's own pace. */
    var b = SP[q.sp].h * sc.n / sc.a / len, dur = Math.min(rs[0].dur, DUR[1] - (n - 1) * FLOCK_GAP, DUR[1] / (1 + (n - 1) * b));
    var beat = Math.max(FLOCK_GAP, Math.ceil(b * dur));
    dur = Math.min(dur, DUR[1] - (n - 1) * beat);
    for (j = 0; j < n; j++) {
      var d = j === 0 ? 0 : j === 1 ? -SPREAD : SPREAD;
      live.push({ id: q.id, sp: q.sp, pts: rs[j].pts.map(function (pt) { return { x: pt.x + d, y: pt.y }; }), t0: t + j * beat, dur: dur, barn: at0.p.barn || "" });
    }
  }
  /* pxOf(z, dpr, level) -> device px per sprite px, whole so the bird stays crisp: PX CSS px at the Plan level (never under the
     zoom's own), ZK x the zoom at the Bead level but never under BEAD_PX, so a Bead bird is bigger than a Plan one and grows as
     the camera comes in. */
  function pxOf(z, dpr, level) { return Math.max(1, Math.round((level > 2 ? Math.max(BEAD_PX, ZK * z) : Math.max(z, PX)) * dpr)); }
  /* The frame birds are drawn in: device px when the context can say its transform (the page), {a, e, f} the world transform's
     scale and offset and n the device px per sprite px, r the outline's width (one CSS px, a quarter of a sprite px once that is
     bigger); else world px, one per sprite px (node). */
  function screen(ctx, z, level) {
    var m = z && ctx.getTransform ? ctx.getTransform() : null;
    if (!m) return { a: 1, e: 0, f: 0, n: 1, r: 1, d: 1 };
    var dpr = Math.round(m.a / z * 1000) / 1000, n = pxOf(z, dpr, level);   // the matrix is single precision: 0.69 / 0.69 is 0.99999...
    return { m: m, a: m.a, e: m.e, f: m.f, n: n, r: Math.max(1, Math.round(dpr), Math.round(n / 4)), d: dpr };
  }
  /* The outline a bird of species s is ringed in, or "" for none. */
  function rimOf(s, night) { return night ? (s.rim ? RIM : "") : s.rim ? DAY_RIM : INK; }
  /* One bird of species sp, frame f, centred on world point (x, y), or a still bird standing on it (its bottom edge at y, a px
     over the top of the figure's padded map, its outline too, so it grows up and away from every pose). Its outline first (rimOf).
     Returns the box it covers, its outline in it. */
  function paint(ctx, sc, sp, x, y, f, night, still) {
    var s = SP[sp], cs = night ? s.night : s.day, n = sc.n, k = sc.r, rim = rimOf(s, night), b = boxOf(sc, sp, x, y, night, still), x0 = b.x0, y0 = b.y0;
    if (rim) { ctx.fillStyle = rim; s.runs[f].forEach(function (r) { ctx.fillRect(x0 + r.x * n - k, y0 + r.y * n - k, r.w * n + 2 * k, n + 2 * k); }); }
    s.runs[f].forEach(function (r) { ctx.fillStyle = cs[r.i]; ctx.fillRect(x0 + r.x * n, y0 + r.y * n, r.w * n, n); });
    return { x: b.x, y: b.y, w: b.w, h: b.h };
  }
  /* The box paint() covers (x0, y0: the sprite's top left in it), drawn or not: a perched plate stands by a still bird's. */
  function boxOf(sc, sp, x, y, night, still) {
    var s = SP[sp], n = sc.n, k = rimOf(s, night) ? sc.r : 0, x0 = Math.round(sc.a * x + sc.e - s.w * n / 2);
    var y0 = still ? Math.round(sc.a * y + sc.f) - s.h * n - k : Math.round(sc.a * y + sc.f - s.h * n / 2);
    return { x0: x0, y0: y0, x: x0 - k, y: y0 - k, w: s.w * n + 2 * k, h: s.h * n + 2 * k };
  }
  /* A bird's plate, baked once per text, theme, DPR and width cap (CSS px) into a small canvas {img, w, h, lines} in device px:
     the text wrapped (wrap()) in TAG_PX bold in the page's display font, its ink on a rounded plate of the halo colour, ringed
     in the ink (PLATE). Off the page (node) only its size and lines, from the monospace advance. */
  var tagCache = {}, tagN = 0, fam = null;
  function family() {
    if (fam === null) {
      fam = "ui-monospace,Menlo,Consolas,monospace";
      try { var v = getComputedStyle(document.documentElement).getPropertyValue("--font-display").trim(); if (v) fam = v; } catch (e) { /* node */ }
    }
    return fam;
  }
  function bake(word, night, dpr, cap) {
    var key = (night ? "n" : "d") + dpr + "|" + cap + "|" + word, c = tagCache[key];
    if (c) return c;
    if (++tagN > TAG_CAP) { tagCache = {}; tagN = 1; }
    var fs = Math.round(TAG_PX * dpr), hw = Math.max(1, Math.round(dpr)), font = "700 " + fs + "px " + family(), cs = night ? TAG.night : TAG.day;
    var px = Math.round(PLATE.px * dpr), py = Math.round(PLATE.py * dpr), lh = Math.ceil(fs * PLATE.lh);
    var cv = typeof document !== "undefined" && document.createElement ? document.createElement("canvas") : null, x = cv && cv.getContext ? cv.getContext("2d") : null;
    if (x && x.measureText) x.font = font;
    var meas = x && x.measureText ? function (s) { return x.measureText(s).width; } : function (s) { return Array.from(s).length * fs * 0.6; };
    var lines = wrap(word, Math.floor(cap * dpr) - 2 * (px + hw), meas);
    var w = Math.ceil(Math.max.apply(null, lines.map(meas).concat(0)));
    c = { w: w + 2 * (px + hw), h: lines.length * lh + 2 * (py + hw), lines: lines };
    c.img = x ? cv : c;
    if (x) {
      cv.width = c.w; cv.height = c.h;   // which resets the context
      var r = Math.round(PLATE.r * dpr), h0 = hw / 2, x1 = c.w - h0, y1 = c.h - h0;   // the ring's line down the middle of its hw px
      x.beginPath(); x.moveTo(h0 + r, h0); x.arcTo(x1, h0, x1, y1, r); x.arcTo(x1, y1, h0, y1, r); x.arcTo(h0, y1, h0, h0, r); x.arcTo(h0, h0, x1, h0, r); x.closePath();
      x.globalAlpha = PLATE.fill; x.fillStyle = cs[1]; x.fill();
      x.globalAlpha = PLATE.rim; x.strokeStyle = cs[0]; x.lineWidth = hw; x.stroke();
      x.globalAlpha = 1; x.font = font; x.textBaseline = "middle"; x.fillStyle = cs[0];
      lines.forEach(function (l, i) { x.fillText(l, hw + px, hw + py + (i + 0.5) * lh); });
    }
    return tagCache[key] = c;
  }
  /* A plate's candidate at box bx (a head's, or a still bird's, paint's), alpha a, at most cap CSS px wide: at the first of its
     sides sd (ON_HEAD, BY_BIRD: r, l, o, and or, ol, over the box with the plate's left or right edge on its middle), the rest its alt. Beside a still bird it is level with it, or with its foot when the plate is
     taller, so it grows up, off its gardener. When no side is clear of the caption's plate, the tool word's (tw), far smaller,
     tries the same sides: a gardener keeps a word where one fits (a phone's crowded plot). */
  function tagAt(id, t0, word, tw, bx, sd, a, night, sc, cap) {
    var g = Math.round(TAG_GAP * sc.d);
    var spots = function (w) {
      var c = bake(w, night, sc.d, cap), mx = Math.round(bx.x + bx.w / 2 - c.w / 2), my = Math.round(bx.y + bx.h / 2 - c.h / 2);
      if (sd[0] === "r") my = Math.min(my, bx.y + bx.h - c.h);
      var cx = Math.round(bx.x + bx.w / 2), oy = bx.y - g - c.h;
      var at = { r: { x: bx.x + bx.w + g, y: my }, l: { x: bx.x - g - c.w, y: my }, o: { x: mx, y: oy }, or: { x: cx, y: oy }, ol: { x: cx - c.w, y: oy } };
      return sd.map(function (k) { return { x: at[k].x, y: at[k].y, w: c.w, h: c.h, word: w, lines: c.lines, img: c.img }; });
    };
    var ss = spots(word).concat(tw && tw !== word ? spots(tw) : []), s0 = ss[0];
    return { id: id, t0: t0, word: word, lines: s0.lines, img: s0.img, x: s0.x, y: s0.y, alt: ss.slice(1), w: s0.w, h: s0.h, a: a, cx: bx.x + bx.w / 2, cy: bx.y + bx.h / 2 };
  }
  /* The beacons on screen (CSS px), each its square and its 12 px point whichever way it faces: a bird's plate keeps off them (gd-TmYAAOBQEH). */
  function bcnBoxes(bs) { return (bs || []).map(function (g) { var h = g.size / 2 + 12; return { x: g.x - h, y: g.y - h, w: 2 * h, h: 2 * h }; }); }
  /* The boxes a word keeps off, in the frame the labels are placed in (sc: world px to it, d: CSS px to it): the still birds and
     hawks drawn (flown, paint's boxes; a flying bird passes under a plate); the faces of the figures on their way (ANIM.faces); o.keep (the signs' tips, CSS px); o.bcns (the beacons on screen, CSS px: bcnBoxes); each figure in view's head and its "?" or "..." bubble, on a plot or at the barn (o.home); at the Bead level a
     plot's "?" bubble (garden.js draws it over the fence's middle); and the cursor's ring, the band round its #hots link that
     the outline and its shadow cover, which is DOM over the canvas, so a word under it would be hidden whatever the order. */
  function keepOff(o, lv, sc, d, flown) {
    var out = flown.concat((o.keep || []).concat(bcnBoxes(o.bcns)).map(function (k) { return { x: k.x * d, y: k.y * d, w: k.w * d, h: k.h * d }; }));
    var put = function (x, y, w, h, id) { if (!o.view || (x < o.view.x + o.view.w && x + w > o.view.x && y < o.view.y + o.view.h && y + h > o.view.y)) out.push({ x: sc.a * x + sc.e, y: sc.a * y + sc.f, w: sc.a * w, h: sc.a * h, id: id }); };
    Object.keys(lv.spots).forEach(function (id) {
      var s = lv.spots[id];
      if (o.moving && o.moving(id)) return;
      if (s.home) { s = homeAt(o, id); if (!s) return; }
      else if (!s.shown) return;
      var lk = o.gs.lookOf ? o.gs.lookOf(id) : o.gs.look && o.gs.look[id];
      put(s.x + HEAD.dx, s.y + HEAD.dy, HEAD.w, HEAD.h, id);   // a face carries its gardener's id
      if (lk && (lk.pose === "ask" || lk.pose === "think")) put(s.x + BUBBLE.dx, s.y + BUBBLE.dy, BUBBLE.w, BUBBLE.h);
    });
    (AN.faces ? AN.faces() : []).forEach(function (f) { put(f.x + HEAD.dx, f.y + HEAD.dy, HEAD.w, HEAD.h); });   // the figures a transition walks or flies, where it draws them now
    if (o.level === 3) (o.world.biomes || []).forEach(function (b) { b.plots.forEach(function (p) { if (p.needs > 0) put(p.x * T + p.w * T / 2 - 7, p.y * T - 15, BUBBLE.w, BUBBLE.h); }); });
    var cur = typeof document !== "undefined" && document.querySelector ? document.querySelector("#hots .hot.cur") : null, st = cur && cur.style;
    if (st && st.width) {
      var x = sc.a * parseFloat(st.left) + sc.e, y = sc.a * parseFloat(st.top) + sc.f, w = sc.a * parseFloat(st.width), h = sc.a * parseFloat(st.height), r = RING * d;
      out.push({ x: x - r, y: y - r, w: w + 2 * r, h: r + d }, { x: x - r, y: y + h - d, w: w + 2 * r, h: r + d }, { x: x - r, y: y - r, w: r + d, h: h + 2 * r }, { x: x + w - d, y: y - r, w: r + d, h: h + 2 * r });
    }
    return out;
  }
  /* Draws the birds in world px (the caller has set the world transform), before the gardeners, places their gardeners' plates
     (words() draws them) and returns true while one flies, so the page asks for the next frame. o = {level, z (the camera's
     zoom), night, view (world px), gs (WORLD.gardenerState), world, reduce, moving, vp, keep, bcns}. Below the Plan
     level nothing flies and nothing waits to. */
  function draw(ctx, o) {
    var t = now();
    tagged = []; due = false;
    if (o.level < 2 || !o.gs || !o.world) { pending = []; live = []; perch = {}; return false; }
    var lv = o.gs[o.level], more = false, sc = screen(ctx, o.z, o.level), cands = [], flown = [];
    var cap = o.vp && o.vp.w < PLATE.PHONE_W ? PLATE.phone : PLATE.wide;   // a plate's width cap, CSS px
    scale = sc.m ? sc.d : 1; inkNow = TAG[o.night ? "night" : "day"][0];
    if (force) o.reduce = false;
    if (sc.m) ctx.setTransform(1, 0, 0, 1, 0, 0);
    pending.forEach(function (q) { launch(q, lv, o, t, sc); });
    pending = [];
    live = live.filter(function (b) { return t - b.t0 < b.dur; });
    Object.keys(hawks).forEach(function (id) {
      var hk = hawks[id], st = standing(o, lv, id);
      if (t - hk.t0 > HAWK_FORGET) { delete hawks[id]; return; }
      if (!st || !inView(head(st.s), o.view)) { if (hk.land) delete hawks[id]; return; }
      if (hk.fresh) { hk.fresh = false; settle(id, hk.t0, hk, t); }   // its plate on the head as it goes up
      if (o.reduce) {   // a still hawk over the figure as the subagent starts; nothing circles
        if (!hk.still && admit(live, id, 1, st.p.barn)) { var h = head(st.s); live.push({ id: id, sp: "hawk", still: true, pts: [{ x: h.x, y: h.y - 5 }], t0: t, dur: STILL, barn: st.p.barn || "" }); }
        hk.still = true;
        if (hk.land) delete hawks[id];
        return;
      }
      var p = hawkAt(hk, st.s, t);
      if (p.done) { delete hawks[id]; return; }
      flown.push(paint(ctx, sc, "hawk", p.x, p.y, p.f, o.night)); more = true;
    });
    live.forEach(function (b) {
      var p = at(b, t);
      more = true;
      if (!p || p.done) return;
      var bx = paint(ctx, sc, b.sp, p.x, p.y, p.f, o.night, b.still);
      if (b.still) flown.push(bx);
    });
    var wakeAt = perched(o, lv, sc, t, cap, cands);
    more = more || wakeAt === t;
    /* the plates placed with the birds, drawn by words() over the figures: the screen (o.vp, CSS px) carried to device px */
    var d = sc.m ? sc.d : 1, scr = sc.m && o.vp ? { w: o.vp.w * d, h: o.vp.h * d, e: EDGE * d } : null;
    var keep = cands.length ? keepOff(o, lv, sc, d, flown) : [];
    /* a still bird on another figure's face is under it (a gardener a row up): its plate would name that one, so it has none */
    cands = cands.filter(function (c) { return !c.on || !keep.some(function (k) { return k.id && k.id !== c.id && hit(k, c.on); }); });
    tagged = cands.length ? place(pick(cands), scr, keep) : [];
    due = tagged.length > 0;
    if (sc.m) ctx.setTransform(sc.m.a, sc.m.b, sc.m.c, sc.m.d, sc.m.e, sc.m.f);
    if (timer !== null) { clearTimeout(timer); timer = null; }
    /* a plate held still asks for no frames: one wake when its fade begins (or, with none, when it goes) */
    if (!more && wakeAt < Infinity && wakeFn && typeof setTimeout === "function") timer = setTimeout(function () { timer = null; wake(); }, Math.max(16, wakeAt - t));
    return more;
  }
  /* settle(id, t0, x, from): the plate x {ca, w, tw} of gardener id's call at t0 perches over its head from `from` for LINGER ms,
     unless a newer call's is up. */
  function settle(id, t0, x, from) {
    var p = perch[id];
    if (p && p.t0 > t0) return;
    perch[id] = { id: id, t0: t0, ca: x.ca, w: x.w, tw: x.tw, from: from, end: from + LINGER };
  }
  /* The perched plates' candidates, pushed to cands, each over its gardener's head (beside its still bird under reduced motion):
     gone once their time is up (a circling hawk's stays up), and of more than MAX plates on the farm the oldest dropped. Each
     is solid at once (so a page whose clock stands still, a test's, shows it) and fades out over its last TAG_FADE ms (not under
     reduced motion). Returns t while one shown fades (a frame wanted), else the time the next one shown starts to fade or goes
     (Infinity: none). */
  function perched(o, lv, sc, t, cap, cands) {
    var wakeAt = Infinity, stills = {};
    live.forEach(function (b) { if (b.still) stills[b.id] = b; });
    var endOf = function (p) { var hk = hawks[p.id]; return hk && !hk.land && hk.t0 === p.t0 ? Infinity : p.end; };
    var ps = Object.keys(perch).map(function (id) { return perch[id]; }).filter(function (p) {
      if (t < endOf(p)) return true;
      delete perch[p.id]; return false;
    }).sort(function (a, b) { return b.t0 - a.t0 || (a.id < b.id ? -1 : 1); });
    ps.slice(MAX).forEach(function (p) { delete perch[p.id]; });
    ps.slice(0, MAX).forEach(function (p) {
      var st = standing(o, lv, p.id);
      if (!st || !inView(head(st.s), o.view)) return;
      var h = head(st.s), b = stills[p.id], hk = hawks[p.id], end = endOf(p), a = 1, bx, sd;
      if (b) { bx = boxOf(sc, b.sp, h.x, h.y - 5, o.night, true); sd = BY_BIRD; }
      else {   // the head's box, up to the top of a circling hawk's laps, so the plate stands over the hawk, not under it
        var hy = sc.a * h.y + sc.f, top = hk && !hk.land && !o.reduce ? sc.a * (h.y - HAWK_UP - 4) + sc.f - SP.hawk.h * sc.n / 2 - sc.r : hy;
        bx = { x: sc.a * (st.s.x + HEAD.dx) + sc.e, y: top, w: sc.a * HEAD.w, h: hy + sc.a * HEAD.h - top }; sd = ON_HEAD;
      }
      if (!o.reduce) a = Math.max(0, Math.min(1, (end - t) / TAG_FADE));
      var c = tagAt(p.id, p.t0, p.w, p.tw, bx, sd, a, o.night, sc, cap);
      if (b) c.on = bx;   // a still bird on another figure's face is hidden under it: no plate (draw)
      cands.push(c);
      if (end === Infinity) return;
      wakeAt = Math.min(wakeAt, o.reduce ? end : a < 1 ? t : end - TAG_FADE);
    });
    return wakeAt;
  }
  /* Draws the words the last draw() placed, after the figures, their bubbles and the transitions, so they sit on top: in the
     frame they were placed in (device px on the page), the caller's transform put back. Once per draw(): a render that drew no
     birds (below the Plan level, no farm) draws no stale words. */
  function words(ctx) {
    if (!due) return;
    due = false;
    var m = ctx.getTransform ? ctx.getTransform() : null;
    if (m) ctx.setTransform(1, 0, 0, 1, 0, 0);
    tagged.forEach(function (l) { if (l.a > 0) { ctx.globalAlpha = l.a; ctx.drawImage(l.img, l.x, l.y); } });
    ctx.globalAlpha = 1;
    tagged.forEach(function (l) {   // a press held on it: its ring brightens, the ink at full strength (spec 10)
      if (l.id !== held || !(l.a > 0) || !ctx.arcTo) return;
      var k = Math.max(1, Math.round(scale)), r = Math.round(PLATE.r * scale), x0 = l.x + k, y0 = l.y + k, x1 = l.x + l.w - k, y1 = l.y + l.h - k;   // bake's ring, twice as wide
      ctx.beginPath(); ctx.moveTo(x0 + r, y0); ctx.arcTo(x1, y0, x1, y1, r); ctx.arcTo(x1, y1, x0, y1, r); ctx.arcTo(x0, y1, x0, y0, r); ctx.arcTo(x0, y0, x1, y0, r); ctx.closePath();
      ctx.strokeStyle = inkNow; ctx.lineWidth = 2 * k; ctx.stroke();
    });
    if (m) ctx.setTransform(m.a, m.b, m.c, m.d, m.e, m.f);
  }
  /* The project keys with activity in the last BUSY_S seconds, sorted, as one string. */
  function busySig() { var t = now(); return Object.keys(busyAt).filter(function (k) { return t - busyAt[k] < BUSY_S * 1000; }).sort().join(","); }
  /* sign(n, status, level) -> status: the World and Project levels' word for it: the status line of cursor Thing n, a project,
     in the title bar (garden.js syncBar) gains "· busy". */
  function sign(n, status, level) {
    var sig = busySig();
    said = sig;
    if (level > 1 || !n || n.kind !== "biome" || !n.key || ("," + sig + ",").indexOf("," + n.key + ",") < 0) return status;
    return status ? status + " · busy" : "busy";
  }

  /* ---------- from a label to the Glass (gd-jt6OwmV4Jk, spec 10) ---------- */
  var PRESS_MS = 500, PRESS_LIT = 250, PRESS_PX = 10, HIT_PAD = 4, TAP_PAD = 13;   // a long press and its slack; ring lit half way; a plate's margin, CSS px, for a mouse and a finger (an 18 px plate grows to 44, the 44 px rule)
  /* hitAt(list, x, y, pad) -> the gardener whose plate (pad round it) or bird is at x, y in tags()'s frame, or "": the first wins. */
  function hitAt(list, x, y, pad) {
    for (var i = 0; i < list.length; i++) { var b = list[i]; if (b.a > 0 && x >= b.x - pad && x < b.x + b.w + pad && y >= b.y - pad && y < b.y + b.h + pad) return b.gid || b.id; }
    return "";
  }
  /* presser(o) -> {down, move, up, cancel, context}, pure: o = {hit(x, y, touch), cam(), lit(id), fire(id, how), set(fn, ms), clear(t)}.
     down(pid, x, y, first, touch) on a target arms it (first: a new touch sequence); at PRESS_LIT its ring lights, at PRESS_MS it
     fires "press". A move of PRESS_PX (Manhattan), a second pointer, a camera move or a cancel drops it; up(pid) is true when it
     fired. context(x, y, touch) fires "menu" at once and is true when the menu is ours: a touch's own long press (while a touch
     is down or for PRESS_MS after) is eaten unfired once its press was dropped, as on Android a pan still ends in one. */
  function presser(o) {
    var p = null, ptrs = {}, late = false, lt = null;
    function drop() { if (!p) return; o.clear(p.t); if (p.lit) o.lit(""); p = null; }
    function go(q, how) { o.clear(q.t); if (q.lit) o.lit(""); q.lit = false; q.done = true; o.fire(q.id, how); }
    function still(q) { if (p !== q) return false; if (o.cam() === q.cam) return true; drop(); return false; }
    function off(pid) {
      var t = ptrs[pid] === 2; delete ptrs[pid];
      if (t) { late = true; o.clear(lt); lt = o.set(function () { late = false; }, PRESS_MS); }
    }
    return {
      down: function (pid, x, y, first, touch) {
        if (first) ptrs = {};
        ptrs[pid] = touch ? 2 : 1; drop();
        var id = Object.keys(ptrs).length === 1 ? o.hit(x, y, touch) : "";
        if (!id) return;
        var q = p = { pid: pid, x: x, y: y, id: id, cam: o.cam(), lit: false, done: false };
        q.t = o.set(function () {
          if (!still(q)) return;
          q.lit = true; o.lit(q.id);
          q.t = o.set(function () { if (still(q)) go(q, "press"); }, PRESS_MS - PRESS_LIT);
        }, PRESS_LIT);
      },
      move: function (pid, x, y) { if (p && !p.done && p.pid === pid && Math.abs(x - p.x) + Math.abs(y - p.y) >= PRESS_PX) drop(); },
      up: function (pid) { off(pid); var f = !!(p && p.pid === pid && p.done); if (!p || p.pid === pid) drop(); return f; },
      cancel: function (pid) { off(pid); drop(); },
      context: function (x, y, touch) {
        if (p && (p.done || still(p))) { if (!p.done) go(p, "menu"); return true; }
        if (touch || late || Object.keys(ptrs).some(function (k) { return ptrs[k] === 2; })) return true;
        var id = o.hit(x, y);
        if (id) o.fire(id, "menu");
        return !!id;
      }
    };
  }
  /* watchOf(st, n, atOf) -> the gardener w watches with the cursor on Thing n: a gardener's own, else of those on its plot or its
     step's cell (st: WORLD.gardenerState's) the newest call's (atOf(id)), else "". */
  function watchOf(st, n, atOf) {
    if (!n || !st) return "";
    if (n.kind === "gardener" || st.spots[n.id]) return n.id;
    var ids = st.plots && st.plots[n.id] ? st.plots[n.id].ids.slice() : Object.keys(st.spots).filter(function (id) { return st.spots[id].cell === n.id; });
    return ids.sort(function (a, b) { return (atOf(b) || 0) - (atOf(a) || 0) || (a < b ? -1 : 1); })[0] || "";
  }
  /* glass(id): /garden?glass=<id> (the Workbench's glass kind) under body[data-glass], else a toast (the route checks the tier
     again, internal/glass); said in #cursay. */
  function glass(id, toast) {
    var B = document.body, say = document.getElementById("cursay");
    if (!B || B.dataset.glass !== "1") { toast("The Glass is not open to you here"); return; }
    if (say) say.textContent = "Watching " + (names[id] || id) + " in the Glass";
    var away = function () { location.href = (B.dataset.base || "") + "/garden?glass=" + encodeURIComponent(id); };
    if (typeof WORKBENCH !== "undefined" && WORKBENCH.has("glass")) WORKBENCH.open("glass", id);
    else if (typeof WORKBENCH === "undefined" && typeof LAZY !== "undefined") LAZY.need("workbench").then(function () { if (WORKBENCH.has("glass")) WORKBENCH.open("glass", id); else away(); }, away);   // the Workbench loads after the World (web/lazy.js)
    else away();
  }
  /* transcript(id): /garden?transcript=<id> (web/transcript.js, spec 11.4), t's act; said in #cursay. */
  function transcript(id) {
    var say = document.getElementById("cursay");
    if (say) say.textContent = "Reading the transcript of " + (names[id] || id);
    var away = function () { location.href = ((document.body && document.body.dataset.base) || "") + "/garden?transcript=" + encodeURIComponent(id); };
    if (typeof WORKBENCH !== "undefined" && WORKBENCH.has("transcript")) WORKBENCH.open("transcript", id);
    else if (typeof WORKBENCH === "undefined" && typeof LAZY !== "undefined") LAZY.need("workbench").then(function () { if (WORKBENCH.has("transcript")) WORKBENCH.open("transcript", id); else away(); }, away);   // the Workbench loads after the World (web/lazy.js)
    else away();
  }
  /* aria(h): a gardener link's " Press W to watch it in the Glass." for a viewer who may watch. */
  function aria(h) { return h && h.kind === "gardener" && typeof document !== "undefined" && document.body && document.body.dataset.glass === "1" ? " Press W to watch it in the Glass." : ""; }
  /* wire(board, o) presses on the board's plates and returns watch(el, tr), w's act (t's with tr, its transcript): a
     focused gardener link's, else watchOf the cursor's. o = {level(), cam(), local(e): CSS px, node(), st(), toast(msg)}. Not over the camera, Jump, tip,
     beacons or a panel, and a press that fired eats its click. It ends on a lift anywhere (the Glass it opened is under it). */
  var HUD = ".cam,#jump,#stip,#bcns,#bead,#tour,#outline";
  function hud(e) { return !!(e.target && e.target.closest && e.target.closest(HUD)); }
  function wire(board, o) {
    var eat = false, at = function (e) { var q = o.local(e); return { x: q[0], y: q[1] }; }, doc = board.ownerDocument || board;
    var pr = presser({
      hit: function (x, y, touch) { return o.level() >= 2 ? hitAt(api.tags(), x * scale, y * scale, (touch ? TAP_PAD : HIT_PAD) * scale) : ""; },
      cam: o.cam, set: function (f, ms) { return setTimeout(f, ms); }, clear: function (t) { clearTimeout(t); },   // not clearTimeout itself: Illegal invocation
      lit: function (id) { held = id; wake(); },
      fire: function (id) { glass(id, o.toast); }
    });
    board.addEventListener("pointerdown", function (e) { if (!hud(e) && (e.pointerType !== "mouse" || e.button === 0)) { var q = at(e); pr.down(e.pointerId, q.x, q.y, e.isPrimary, e.pointerType === "touch"); } });
    board.addEventListener("pointermove", function (e) { var q = at(e); pr.move(e.pointerId, q.x, q.y); });
    doc.addEventListener("pointerup", function (e) { if (pr.up(e.pointerId)) { eat = true; setTimeout(function () { eat = false; }, 0); } }, true);
    doc.addEventListener("pointercancel", function (e) { pr.cancel(e.pointerId); }, true);
    board.addEventListener("contextmenu", function (e) { var q = at(e); if (!hud(e) && pr.context(q.x, q.y, e.pointerType === "touch")) e.preventDefault(); });
    board.addEventListener("click", function (e) { if (eat) { eat = false; e.preventDefault(); e.stopPropagation(); } }, true);
    return function watch(el, tr) {
      var g = el && ((el._h && el._h.kind === "gardener" && el._h.g) || (el._it && el._it.g)), id = g ? g.id : watchOf(o.st(), o.node(), function (x) { var h = ACT.of(x); return h ? h.at : 0; });
      var say = document.getElementById("cursay");
      if (id && tr) transcript(id);
      else if (id) glass(id, o.toast);
      else if (say) say.textContent = "No gardener at the cursor" + (tr ? " to read" : " to watch");
    };
  }

  var api = {
    SPEED: SPEED, DUR: DUR, RUN: RUN, SWIFT_RUN: SWIFT_RUN, MAX: MAX, BARN_MAX: BARN_MAX, HAWK_UP: HAWK_UP, fieldSky: fieldSky, FLOCK: FLOCK, QUIET: QUIET, STILL: STILL, BUSY_S: BUSY_S, SP: SP,
    PX: PX, BEAD_PX: BEAD_PX, ZK: ZK, RIM: RIM, DAY_RIM: DAY_RIM, INK: INK, TAG: TAG, TAG_PX: TAG_PX, TAG_FADE: TAG_FADE, TAG_CAP: TAG_CAP, EDGE: EDGE, PLATE: PLATE, LINGER: LINGER, wrap: wrap, plateOf: plateOf, bcnBoxes: bcnBoxes,
    BEAT: BEAT, GLIDE: GLIDE, FLAP: FLAP, SOAR: SOAR, INKS: INKS, pxOf: pxOf, rimOf: rimOf, speciesOf: speciesOf, tagOf: tagOf, pick: pick, place: place, ON_HEAD: ON_HEAD, BY_BIRD: BY_BIRD, route: route, at: at, admit: admit, hawkAt: hawkAt,
    hear: hear, recap: recap, draw: draw, words: words, sign: sign, busySig: busySig,
    PRESS_MS: PRESS_MS, PRESS_LIT: PRESS_LIT, PRESS_PX: PRESS_PX, HIT_PAD: HIT_PAD, TAP_PAD: TAP_PAD, hitAt: hitAt, presser: presser, watchOf: watchOf, wire: wire, aria: aria,
    /* has a project gone busy or quiet since the signs last said? (garden.js re-reads them every few seconds) */
    stale: function () { return busySig() !== said; },
    onWake: function (fn) { wakeFn = fn; },
    /* test hooks (the goldens and the frame budget): pin the clock, fly under reduced motion, read what flies */
    pin: function (ms) { pinned = ms === undefined ? null : ms; },
    motion: function (on) { force = !!on; },
    list: function () { return live.map(function (b) { return { id: b.id, sp: b.sp, t0: b.t0, dur: b.dur, still: !!b.still, barn: b.barn || "" }; }); },
    /* the plates the last draw placed: {id and gid (its gardener), word (its whole text), lines (as drawn), x, y, w, h, a, cx, cy
       (the centre of what it is on: its gardener's head, or the still bird it stands by)} in the frame it drew in (scale() CSS
       px to it) */
    tags: function () { return tagged.map(function (l) { return { id: l.id, gid: l.id, word: l.word, lines: l.lines.slice(), x: l.x, y: l.y, w: l.w, h: l.h, a: l.a, cx: l.cx, cy: l.cy }; }); },
    scale: function () { return scale; },
    pressed: function () { return held; },
    hawks: function () { return Object.keys(hawks).sort(); },
    /* the plates perched (shown or not), by gardener: {id, word, end} */
    perched: function () { return Object.keys(perch).sort().map(function (id) { var q = perch[id]; return { id: id, word: q.w, end: q.end }; }); },
    opened: function () { return openAt; }
  };
  return api;
})();
if (typeof module !== "undefined") module.exports = BIRDS;
