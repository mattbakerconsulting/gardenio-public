/* The Porch by plot (gd-ikrXlqYTL5, homestead-agents-as-people.md sections 2 and 6), run by TestPorchUnderNode:
     node porch_dom_test.js <web dir>
   garden.js boots under world_dom_mock.js as the viewer oidc:matt@example.com and is handed the fixture farm document
   (farm_fixture.json), with two of its gardeners operated by someone else. Asserted: "Needs you" first, with the gardeners
   down or asking (gd-J3l6W4X6DJ), one heading per plot and "In the field" last, holding exactly the gardeners the map leaves
   at home, every gardener once; the "Mine" scope and the viewer's own gardeners first; each
   row's link, role, state word (red when down), spend chip (gd-iSYItBAyA5), reason, since, step and place words; the pip and its "N at work" sub-count,
   with the rows' spend summed under the scope; "Go to plot" flies to the plot; a gardener's sprite on its plot goes home to
   its field figure; and one key walks the three stops, field figure, Glass, plot figure and home (H-D21). Prints a one-line
   JSON report. */
"use strict";
var path = require("path"), assert = require("assert"), fs = require("fs");
var M = require(path.join(__dirname, "world_dom_mock.js"));
var webDir = process.argv[2], ME = "oidc:matt@example.com";

var doc = JSON.parse(fs.readFileSync(path.join(__dirname, "farm_fixture.json"), "utf8"));
var by = {}; doc.gardeners.forEach(function (g) { by[g.id] = g; });
by["gd-gWork2"].operator = by["gd-gIdle"].operator = "oidc:flo@example.com";   // someone else's, one on the plot and one at home

var h = M.boot(webDir, { w: 1280, h: 720, reduce: true, me: ME });
h.flushAll();
var get = function (id) { return document.getElementById(id); }, ul = get("d-porch");
/* The Porch as [heading, [row ids]] pairs, and the rows' DOM by id. */
function shape() {
  var out = [], rows = {};
  ul.children.forEach(function (li) {
    if (li.classList.contains("pgrp")) { out.push([li.children[0].textContent, li.children[0].children[0].textContent, []]); return; }
    out[out.length - 1][2].push(li.dataset.gid); rows[li.dataset.gid] = li;
  });
  return { groups: out, rows: rows };
}

// Before a farm document the Porch mirrors the Ledger (empty here): no headings.
assert.strictEqual(ul.children.length, 0, "no farm document, no gardeners rows");
/* Two rows as web/garden.js mirrors them off the Ledger first (data-src, no data-k): the farm document's rows replace them all,
   none left at the end of the list (gd-aVUvUN8gQy). */
["i-gd-gWork", "i-gd-gIdle"].forEach(function (src) { var li = document.createElement("li"); li.className = "item"; li.dataset.src = src; li.dataset.u = src; ul.appendChild(li); });

h.farmdoc(doc); h.flushAll();
assert.ok(ul.children.every(function (li) { return li.dataset.k; }), "no Ledger mirror row outlives the farm document");
var placed = h.W.gardenerSpots(h.W.layout(doc, null, { now: 1 }), doc.gardeners, 3), home = placed.home.slice().sort();
var needy = doc.gardeners.filter(function (g) { return g.reason && (g.reason.kind === "down" || g.reason.kind === "parked"); });

// 1. "Mine" (the default scope): only the viewer's gardeners, by plot, the barn last.
var s = shape(), mine = doc.gardeners.filter(function (g) { return g.operator === ME; }).length;
assert.deepStrictEqual(s.groups.map(function (g) { return g[0]; }), ["Needs you ", "Edge IdP rollout ", "In the Meadow: Slack thread: not totally convinced the nudge fires ", "In the field "], "Needs you, one heading per plot or band item (gd-gAway by its wildflower, gd-6FCoWelnx2), those at home last");
assert.ok(ul.children[0].classList.contains("needs"), "the Needs you heading is marked");
assert.strictEqual(s.groups.reduce(function (n, g) { return n + g[2].length; }, 0), mine, "only the viewer's gardeners under Mine");
assert.ok(s.groups[1][2].indexOf("gd-gWork2") < 0 && s.groups[s.groups.length - 1][2].indexOf("gd-gIdle") < 0, "someone else's gardeners are left out");
assert.strictEqual(get("dpip-porch").textContent, String(mine), "the pip counts the Porch's gardeners");
/* One gardener count (gd-rC46vJomwy): the title plaque and the barn read the Porch's set, never the Ledger's agents. */
function plaque() { return (/ · (\d+) gardeners?/.exec(get("sub").dataset.line) || [])[1]; }
/* a Barn: the north-west zone of a biome's home cell (HW 29.6), a cell of the cursor's grid once the cursor is inside it, Up and
   Left from any other zone. presses Homes reach it: one the home cell of the project under the cursor, a second the Commons's
   (H-D11). -> {id, tip} */
function toBarn(presses) {
  for (var i = 0; i < presses; i++) { h.key("Home"); h.flushAll(); }
  h.key("Enter"); h.flushAll();
  for (i = 0; i < 4 && !(cur() && cur()._h.kind === "zone" && cur()._h.node.zone === "barn"); i++) { h.key(i % 2 ? "ArrowLeft" : "ArrowUp"); h.flushAll(); }
  assert.ok(cur() && cur()._h.kind === "zone" && cur()._h.node.zone === "barn", "the cursor on a Barn: " + (cur() ? cur()._h.kind + " " + cur()._h.id : "none") + " screen " + document.body.dataset.screen);
  return cur();
}
function barn(presses) { var c = toBarn(presses); return { id: c._h.id, tip: c._h.tipText || "" }; }
/* the biome whose Barn a gardener stands in, as WORLD.homeFill puts it (28.2), and a Barn's account in words (5a, gd-GL76uVzdAQ):
   everyone it holds, those down apart from those running, and they and the one asking as needing you */
var lay = h.W.layout(doc, null, { now: 1 }), homeOf = function (g) { var id = h.W.homeFor(g); return lay.index[id] ? id : "project:"; };
function account(id) {
  var gs = doc.gardeners.filter(function (g) { return homeOf(g) === id; }), n = gs.length, at = gs.filter(function (g) { return home.indexOf(g.id) >= 0; }).length;
  var down = gs.filter(function (g) { return g.reason && g.reason.kind === "down"; }).length, need = down + gs.filter(function (g) { return g.reason && g.reason.kind === "parked"; }).length;
  return n + " gardeners, " + need + (need === 1 ? " needs you, " : " need you, ") + (n - down) + " running, " + down + " down: " + at + " resting, " + (n - at) + " out in the plots.";
}
assert.strictEqual(plaque(), String(mine), "the plaque counts the Porch's gardeners: " + get("sub").dataset.line);
/* Mine sums only the viewer's own spend: g-down $4.10, g-foreman $0.67 and g-work-1 $3.20, never Flo's g-work-2. */
assert.ok(/ · spent at least \$7\.97\. $/.test(get("porch-sub").textContent), "Mine sums the viewer's spend: " + get("porch-sub").textContent);

// 2. Everyone: every gardener; the barn holds exactly those the map leaves at home; the viewer's own come first.
get("b-all").onclick(); h.flushAll();
s = shape();
assert.deepStrictEqual(s.groups[0][2], ["gd-gDown", "gd-gThreadDown", "gd-gAsk"], "Needs you: the two down, then the one asking");
assert.strictEqual(needy.length, 3, "the fixture's needy gardeners");
assert.strictEqual(s.groups[1][1], String(8 - needy.filter(function (g) { return g.plot; }).length), "the plot's heading counts its other gardeners: " + JSON.stringify(s.groups));
var barnGrp = s.groups[s.groups.length - 1];   // the barn is always last; the Meadow group (gd-gAway by its wildflower) sits before it
assert.deepStrictEqual(barnGrp[2].slice().sort(), home.filter(function (id) { return s.groups[0][2].indexOf(id) < 0; }), "In the field is the unplaced bucket, less those that need you: " + JSON.stringify(barnGrp[2]));
assert.deepStrictEqual(s.groups.reduce(function (a, g) { return a.concat(g[2]); }, []).sort(), Object.keys(by).sort(), "every gardener once");
assert.strictEqual(s.groups[1][2][s.groups[1][2].length - 1], "gd-gWork2", "someone else's gardener after the viewer's own");
assert.strictEqual(barnGrp[2][barnGrp[2].length - 1], "gd-gIdle", "at the barn too");
var work = doc.gardeners.filter(function (g) { return g.state === "working"; }).length;
assert.strictEqual(get("dpip-porch").textContent, String(doc.gardeners.length), "the pip counts every gardener under Everyone");
assert.strictEqual(get("porch-sub").textContent, doc.gardeners.length + " gardeners · " + work + " at work · spent at least $11.07. ", "the pane's count and everyone's spend");
assert.strictEqual(plaque(), String(doc.gardeners.length), "the plaque agrees with the Porch under Everyone");
/* Each Barn says its own project's account whole (28.6): gardenio's those of gardenio at home and those out on its
   plots, the Commons's the thread agents; every gardener stands in one Barn, so the Barns together count the Porch's everyone. */
var bt = barn(1); assert.ok(bt.tip, "the barn has a tip in view");
assert.strictEqual(bt.id, "home:gardenio/barn", "one Home: the Barn of the project under the cursor");
assert.strictEqual(account("project:gardenio"), "11 gardeners, 2 need you, 10 running, 1 down: 4 resting, 7 out in the plots.", "the fixture's gardenio account");
assert.strictEqual(bt.tip.indexOf(account("project:gardenio")), 0, "gardenio's Barn counts gardenio's gardeners, the one down apart from those running, and it and the one asking as needing you: " + bt.tip);
var ct = barn(2); assert.strictEqual(ct.id, "home:/barn", "a second Home: the Commons's Barn");
assert.strictEqual(ct.tip.indexOf("2 gardeners, 1 needs you, 1 running, 1 down: 2 resting, 0 out in the plots."), 0, "the Commons's Barn counts the thread agents: " + ct.tip);
var barnsOf = {}; doc.gardeners.forEach(function (g) { barnsOf[homeOf(g)] = (barnsOf[homeOf(g)] || 0) + 1; });
assert.strictEqual(Object.keys(barnsOf).reduce(function (a, id) { return a + barnsOf[id]; }, 0), doc.gardeners.length, "every gardener in one Barn");
assert.strictEqual(get("porch-sub").textContent.indexOf(doc.gardeners.length + " gardeners"), 0, "the Porch's every-gardener count is the Barns' together");

// 3. A row: data-src for chat.js, the link into the garden, role chip, state word, since and step; one tab stop.
var r = s.rows["gd-gAsk"], kids = r.children, meta = kids[2].children;
assert.strictEqual(r.dataset.src, "i-gd-gAsk", "chat.js reads the agent from data-src");
assert.strictEqual(kids[0].tagName, "CANVAS", "a sprite thumbnail");
assert.strictEqual(kids[1].textContent, "g-ask"); assert.strictEqual(kids[1].href, by["gd-gAsk"].href, "the name links the bead in the garden");
assert.deepStrictEqual([meta[0].textContent, meta[1].textContent], [by["gd-gAsk"].role || "agent", "Waiting on a human"], "role and state in words");
assert.ok(/ ago$/.test(meta[2].textContent) && meta[2].dataset.t === String(by["gd-gAsk"].since), "since");
assert.deepStrictEqual([kids[3].className, kids[3].textContent], ["gd-why rsn-parked", "Waiting on Where does the chat live?"], "its reason, in the card's words");
assert.strictEqual(kids[4].textContent, "Tending: Where does the chat live?", "the step it tends");
assert.deepStrictEqual([kids[5].className, kids[5].textContent], ["gd-where", "In the field of gardenio and on Edge IdP rollout"], "where it stands: its field, and the plot it is out on (28.12)");
assert.strictEqual(kids[6].textContent, "Go to plot");
var dn = s.rows["gd-gDown"].children;
assert.deepStrictEqual([dn[2].children[1].className, dn[2].children[1].textContent, dn[3].className, dn[3].textContent], ["st st-failed", "Down (failed)", "gd-why rsn-down", "Failed"], "down: a red state word, then its cause");
var dc = dn[2].children[2];   // its spend, after the state word: the figure, said in words, the farm's sentence on hover
assert.deepStrictEqual([dc.className, dc.textContent, dc.getAttribute("role"), dc.getAttribute("aria-label"), dc.title],
  ["chip gd-cost", "$4.10+", "img", "spent $4.10, at least", "Spent at least $4.10; this session is counted in full when it ends."], "the spend chip");
assert.ok(!meta.some(function (c) { return /gd-cost/.test(c.className); }), "g-ask has no figure, so no chip: never a made-up $0");
assert.deepStrictEqual([s.rows["gd-gAway"].children[3].textContent, s.rows["gd-gAway"].children[4].textContent, s.rows["gd-gAway"].children[5].textContent, s.rows["gd-gAway"].children[5].getAttribute("aria-label")], ["Tending: Slack thread: not totally convinced the nudge fires in the Meadow", "In the field of zeta and in the Meadow: Slack thread: not totally convinced the nudge fires", "Go there", "Go to the Meadow: Slack thread: not totally convinced the nudge fires: g-away"], "one whose task is a loose bead stands by its wildflower (gd-6FCoWelnx2)");
assert.strictEqual(s.rows["gd-gThread"].children[5].textContent, "Go to the field", "a gardener at home goes to its field figure");
// a thread agent at home (5b): in the Commons's field, its row says the thread it answers, with no task; one down says why
var th = s.rows["gd-gThread"].children, td = s.rows["gd-gThreadDown"].children;
assert.ok(s.groups[s.groups.length - 1][2].indexOf("gd-gThread") >= 0, "a thread agent is at home");
assert.deepStrictEqual([th[2].children[0].textContent, th[3].className, th[3].textContent, th[4].textContent, th[5].textContent], ["thread", "gd-task", "In the Commons: a pen for the goats", "In the field of the Commons", "Go to the field"], "its thread, in words, and the field it stands in");
assert.deepStrictEqual([td[3].textContent, td[4].textContent], ["Failed: stale: unschedulable, cluster capacity", "In the Commons: can the exchange run behind the edge?"], "down, and the thread it was answering");
// a gardener's task arms (heraldry 5.9): the fixture's Rollout phase bears 🚀🔑📣 and Foundations 🧭🔥📦, so a row under
// either carries the run's picture for each sky before "Tending:" / "Assigned:", named in words; a row whose line bears none
// (g-down's unphased step) has no picture and no gap
function armsOn(id) {
  var t = s.rows[id].children.filter(function (c) { return c.classList.contains("gd-task"); })[0], p = t && t.children.filter(function (c) { return c.classList.contains("ar-pic"); })[0];
  return p ? [p.className, p.children.length, p.children[0].alt, p.children[0].src.indexOf("/garden/arms.png?arms=") >= 0, p.children[0].loading, p.children[1].className, t.textContent] : null;
}
assert.deepStrictEqual(armsOn("gd-gNested"), ["ar-pic ar-line ar-gd", 2, "arms: rocket, key, megaphone", true, "lazy", "ar-night", "Tending: Remove the old login page"], "a gardener under an armed phase wears its run before its step");
assert.deepStrictEqual(armsOn("gd-gLate"), ["ar-pic ar-line ar-gd", 2, "arms: compass, fire, package", true, "lazy", "ar-night", "Assigned: Title gd-A1y (not on a plot)"], "at the barn too, from the farm's own read of its closed task");
assert.strictEqual(armsOn("gd-gDown"), null, "no picture under an unarmed line");
var stops = Object.keys(s.rows).filter(function (id) { return s.rows[id].children[1].tabIndex === 0; });
assert.deepStrictEqual(stops, [s.groups[0][2][0]], "one tab stop: the first row");

// 4. Go to plot: the cursor goes into the gardener's plot and the camera follows, at the Plan level or closer.
var lv0 = h.level(), btn = s.rows["gd-gAsk"]._go;
ul.dispatchEvent({ type: "click", target: { closest: function () { return btn; } }, preventDefault: function () {} });
h.flushAll();
assert.ok(h.level() >= 2, "Go to plot lands at Plan or Bead, from " + lv0 + " to " + h.level());
assert.ok(s.rows["gd-gAsk"].classList.contains("here"), "the row it was sent from is marked");

// 5. A gardener's sprite on its plot goes home to its field figure (H-D21): the cursor on field:<gid>, the farm kept; its link
// stays a link (href kept) and says where it goes.
/* at depth 2 the camera frames the plan's beds in use (cursor-grid spec 4.1), and a gardener stands at the plot's gate below
   them: the wheel (the free camera, until step 4) widens the view onto it */
for (var wz = 0; wz < 8 && !h.ghots.children.some(function (n) { return n._h && n._h.id === "gd-gForeman"; }); wz++) { h.wheel(60, 640, 360); h.flushAll(); }
var node = null; h.ghots.children.forEach(function (n) { if (n._h && n._h.kind === "gardener" && n._h.id === "gd-gForeman") node = n; });   // beside the grid (cursor-grid spec section 8)
assert.ok(node, "gd-gForeman's sprite has a link in view");
assert.strictEqual(node.href, by["gd-gForeman"].href, "the sprite's link is its bead");
assert.strictEqual(node.getAttribute("aria-label").indexOf("g-foreman, tending Edge IdP rollout on Edge IdP rollout, in the field of gardenio: go home"), 0, "its name says it goes home: " + node.getAttribute("aria-label"));
var ev = { type: "click", button: 0, defaultPrevented: false, preventDefault: function () { this.defaultPrevented = true; } };
node.dispatchEvent(ev); h.flushAll();
assert.ok(ev.defaultPrevented, "the click is the map's, not the bead view's");
assert.strictEqual(document.body.dataset.screen, "farm", "the farm stays: the thread is on its card and its Porch row");
assert.strictEqual(cur() && cur()._h.id, h.W.FIELD + "gd-gForeman", "the cursor on its field figure");

// 6. Every running gardener of a project stands in its Growing field, solid (28.12, plan gd-ryynhxINtp), at home or out on a
// plot (each keeps the slot it was first given, so the Mine scope's stands stay put under Everyone); one out names its plot and
// keeps its own speech bubble; nothing has a ghost.
/* the link cells in view, and the cursor's */
function cells() { var out = []; h.hots.children.forEach(function (row) { (row.children || []).forEach(function (gc) { var n = gc.children[0]; if (n && n._h) out.push(n); }); }); return out; }
function cur() { return cells().filter(function (n) { return n.classList.contains("cur"); })[0] || null; }
/* the Growing field of the project presses Homes reach (one: the project under the cursor, two: the Commons's), the cursor inside it */
function toField(presses) {
  toBarn(presses);
  for (var i = 0; i < 4 && !(cur() && cur()._h.kind === "zone" && cur()._h.node.zone === "growing"); i++) { h.key(i % 2 ? "ArrowDown" : "ArrowRight"); h.flushAll(); }
  assert.ok(cur() && cur()._h.node.zone === "growing", "the cursor on a Growing field: " + (cur() ? cur()._h.id : "none"));
  h.key("Enter"); h.flushAll();
}
function toStand(presses, id) {
  toField(presses);
  var to = cells().filter(function (n) { return n._h.id === h.W.FIELD + id; })[0]; assert.ok(to, id + " has a stand in the field's grid");
  for (var i = 0; i < 40 && cur() && cur() !== to; i++) { var at = cur()._n; h.key(at.r < to._n.r ? "ArrowDown" : at.r > to._n.r ? "ArrowUp" : at.c < to._n.c ? "ArrowRight" : "ArrowLeft"); h.flushAll(); }
  var c = cur(); assert.ok(c && c._h.id === h.W.FIELD + id, "the cursor on " + id + "'s stand: " + (c && c._h.id)); return c;
}
var back = shape().rows["gd-gIdle"]._go;   // Go to the field (gardenio's): back on the farm from the Porch section 5 opened
ul.dispatchEvent({ type: "click", target: { closest: function () { return back; } }, preventDefault: function () {} }); h.flushAll();
var gn = toStand(1, "gd-gAsk"), standIds = cells().filter(function (n) { return n._it && n._it.kind === "stand"; }).map(function (n) { return n._it.g.id; });
var ours = doc.gardeners.filter(function (g) { return homeOf(g) === "project:gardenio"; }).map(function (g) { return g.id; });
var onPlot = ours.filter(function (id) { return home.indexOf(id) < 0; });
assert.deepStrictEqual([standIds.length, standIds.slice().sort()], [ours.length, ours.slice().sort()], "every gardenio gardener stands in its field once: " + JSON.stringify(standIds));
assert.ok(!cells().some(function (n) { return n._it && n._it.ghost; }) && h.W.GHOST === undefined, "nothing has a ghost");
assert.strictEqual(gn.getAttribute("aria-label").indexOf("g-ask"), 0, "the stand's link names its gardener: " + gn.getAttribute("aria-label"));
assert.ok(/Edge IdP rollout/.test(gn.getAttribute("aria-label")), "and the plot it is out on: " + gn.getAttribute("aria-label"));
assert.ok(!gn._it.ghost && gn._it.letter && gn._it.plot === "gd-planA", "solid, its speech bubble its own, its plot named");

// 7. A thread agent in the field (5b, 28.12): its link and the title bar lead with the thread it answers, so it shows on a
// phone's strip too, not cut off after the state word. It stands in the Commons's field: the second Home from a project's.
var tf = toStand(2, "gd-gThread"), txt = function (e) { return (e.textContent || "") + (e.children || []).map(txt).join(""); };
assert.ok(tf && tf._h.id === h.W.FIELD + "gd-gThread", "the cursor on g-thread's stand");
assert.strictEqual(tf.getAttribute("aria-label").indexOf("g-thread, at work in the Commons on a pen for the goats: watch in the Glass"), 0, "its link: " + tf.getAttribute("aria-label"));
var full = txt(h.bar);
assert.ok(/g-thread.*In the Commons: a pen/.test(full), "the title bar leads with the Commons: " + full);

// 8. One key, three stops (H-D21): Enter on a field figure opens its Glass, the figure its opener; GARDEN.plot (the Glass's
// Enter and its "Go to its plot", web/glass.js) closes it and flies to the plot figure at the map's full zoom, the cursor on its
// plot and focus on the figure's link; that link's Enter (its click) comes home to the same field figure. A resting gardener
// has no plot: nothing moves.
var wb = { opened: [], closed: 0 };
global.WORKBENCH = { open: function (k, id, how) { wb.opened.push([k, id, how && how.opener]); }, close: function () { wb.closed++; }, has: function () { return true; } };
document.body.dataset.glass = "1";
ul.dispatchEvent({ type: "click", target: { closest: function () { return back; } }, preventDefault: function () {} }); h.flushAll();   // back in gardenio, from the Commons
var fa = toStand(1, "gd-gAsk");
assert.strictEqual(fa.getAttribute("aria-label").indexOf("g-ask, working Where does the chat live? on Edge IdP rollout: watch in the Glass"), 0, "the field figure says it opens the Glass: " + fa.getAttribute("aria-label"));
h.key("Enter"); h.flushAll();
assert.deepStrictEqual(wb.opened.map(function (o) { return [o[0], o[1], o[2] === fa]; }), [["glass", "gd-gAsk", true]], "Enter opens its Glass, the figure the opener");
assert.strictEqual(get("cursay").textContent, "Watching g-ask in the Glass; Enter again goes to its plot");
var proto = Object.getPrototypeOf(h.board), focus0 = proto.focus;
proto.focus = function () { document.activeElement = this; };   // the mock's focus does nothing: here it moves
assert.strictEqual(window.GARDEN.plot("gd-gAsk"), true, "a gardener on a plot goes to it"); h.flushAll();
var fig = document.activeElement;
assert.strictEqual(wb.closed, 1, "the Glass closes");
assert.strictEqual(h.zoom(), h.W.Z_MAX, "at the map's full zoom");
assert.strictEqual(cur() && cur()._h.id, "gd-planA", "the cursor on its plot");
assert.ok(fig && fig._h && fig._h.kind === "gardener" && fig._h.id === "gd-gAsk" && h.ghots.contains(fig), "focus on its plot figure's link");
var cv = window.GARDEN.cam(), fx = fig._h.x + fig._h.w / 2;
assert.ok(Math.abs((fx - cv.cam.cx) * cv.z) < cv.vp.w / 2, "the figure is on the screen");
assert.strictEqual(get("cursay").textContent, "On Edge IdP rollout, g-ask: Enter goes home to the yard");
var ev2 = { type: "click", button: 0, defaultPrevented: false, preventDefault: function () { this.defaultPrevented = true; } };
fig.dispatchEvent(ev2); h.flushAll();
assert.strictEqual(cur() && cur()._h.id, h.W.FIELD + "gd-gAsk", "Enter again: home, the cursor on the same field figure");
assert.strictEqual(document.activeElement, cur(), "and focus on it");
assert.strictEqual(window.GARDEN.plot("gd-gIdle"), false, "a resting gardener has no plot to go to");
assert.strictEqual(wb.closed, 1, "and its Glass stays open");
delete document.body.dataset.glass;   // below the operator tier the Workbench opens the transcript alone, whose Enter goes nowhere new (gd-6LpSDBI9cK)
cur().dispatchEvent({ type: "click", button: 0, defaultPrevented: false, preventDefault: function () { this.defaultPrevented = true; } }); h.flushAll();   // the focused link's Enter
assert.strictEqual(wb.opened.length, 2, "the figure opens its panel again");
assert.strictEqual(get("cursay").textContent, "Reading the transcript of g-ask", "the transcript's sign promises no plot");
proto.focus = focus0; document.activeElement = document.body; delete global.WORKBENCH;

console.log(JSON.stringify({ ok: true, groups: s.groups.length, rows: Object.keys(s.rows).length, home: home.length, barn: !!bt, out: onPlot.length }));
