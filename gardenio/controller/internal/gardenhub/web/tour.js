/* CHORE TOUR: "Tend the garden" (gd-S2Cn2dTsP0; homestead-world-design.md section 24). Tend, in the camera's Menu, walks the five most
   neglected things on the farm, one stop at a time: the weeds (web/weeds.js, in WEEDS.rank order) and what waits on a human
   (the beacons' targets, web/beacons.js) in one list, heaviest first. At each stop the camera flies there and a card says what
   it is and why it needs tending, and offers what can be done from /garden: ping its owner, adopt it, defer it or close it
   (answer, for a decision or escalation waiting on you). Each action posts to the viewer's own endpoint, the one the bead page
   uses, so permission and audit are unchanged. Under the cursor (lands-cursor-grid-design.md section 8) the tour is a cursor
   walk: each stop and each Next is a cursor jump to the stop's Thing, so the cursor's ring marks it, and the card is an inset
   the cursor's camera frames around. stops(), actions(), pingText(), free() and inset() are pure (plain data in, plain data
   out) and run under node (tour_test.go); mount() and measure() are the only DOM code.

   WELCOME (step 9.14b): the same card walks a land's person round once on their first /garden (gardenhub welcome.go). */
var TOUR = (function () {
  "use strict";
  var STOPS = 5;
  var ASK_WEIGHT = 3;      // waiting on a human weighs like an orphaned PR, the heaviest weed; age decides between them
  var HOUR = 3600, DAY = 86400;
  /* What a weed's stop offers, best first; actions() drops what does not apply (a ping needs someone, a closed bead only
     takes a ping). The first is the suggested one. */
  var SUGGEST = {
    orphan: ["ping"],
    wilt: ["ping", "adopt", "defer", "close"],
    frosted: ["ping", "close", "defer"],
    phaseless: ["ping", "adopt", "defer", "close"],
    unlabelled: ["adopt", "ping", "defer", "close"]
  };
  var PATH = { close: "close", defer: "defer", adopt: "claim" };   // POST {base}/b/{id}/<path>; a ping posts a comment

  function plural(n, one, many) { return n + " " + (n === 1 ? one : many); }
  function age(s) { s = Math.max(0, s); return s < 2 * DAY ? plural(Math.floor(s / HOUR), "hour", "hours") : plural(Math.floor(s / DAY), "day", "days"); }   // as WEEDS.age
  function weight(w, since, now) { return w * (1 + Math.log2(1 + Math.max(0, now - since) / DAY)); }   // as a weed's score
  function ruleName(r) { return r === "ask" ? "Waiting on you" : WEEDS.RULES[r].name; }

  /* Who each bead is for, and its state, from the farm document: a step's or loose bead's assignee ("Gardener"), a plan's
     owner or operator and a seed's operator ("Owner", without an oidc: scheme), an orphaned PR's closed bead's last assignee. */
  function people(doc) {
    var who = {}, st = {}, handle = function (n) { return String(n || "").replace(/^[a-z][a-z0-9+.-]*:(?=.)/i, ""); };   // oidc:ann@x -> ann@x
    var bead = function (t) { who[t.id] = { name: t.assignee || "", word: "Gardener" }; st[t.id] = t.state; };
    (doc.plans || []).forEach(function (p) {
      who[p.id] = { name: handle(p.owner || p.operator), word: "Owner" }; st[p.id] = p.status;
      (p.phases || []).concat(p.unphased ? [p.unphased] : []).forEach(function (ph) { (ph.tasks || []).forEach(bead); });
    });
    (doc.seeds || []).forEach(function (s) { who[s.id] = { name: handle(s.operator), word: "Owner" }; st[s.id] = s.status; });
    ((doc.meadow || {}).shown || []).forEach(bead);
    ((doc.piles || {}).orphan_mrs || []).forEach(function (m) { if (m.bead) { who[m.bead] = { name: m.assignee || "", word: "Gardener" }; st[m.bead] = "closed"; } });
    return { who: who, state: st };
  }

  /* stops(o) -> the tour, at most STOPS, heaviest first. o = { doc (the /garden/farm document), ix (WEEDS.index of its lint),
     targets (BEACONS.targets), world (WORLD.layout), asks (the Ledger's decisions and escalations: {id, title, type, u}), now
     (unix s), me (the viewer), at(id) (WORLD.atFor: where an id is on the map, or null), ro (the viewer sees the garden read only:
     a weed's stop offers nothing to do, but is still a stop) }.
     Stop: { key, kind (weed|ask), rule, bead (what the actions act on), title, whys: [[name, why]], at, who, whoWord, state,
     score, since, actions, answer (the ask to open; "" with mail, a project's Mailbox zone, for its loose asks), cur (the
     cursor's Thing: at) }. A bead with several weeds is one stop
     naming each; an unlinked PR (no bead to act on) and anything off the map are left out. */
  function stops(o) {
    var out = [], seen = {}, doc = o.doc || {}, now = o.now, P = people(doc);
    var at = function (id) { return id ? o.at(id) : null; };
    if (o.ix && o.ix.lint) WEEDS.rank(o.ix.lint.weeds).forEach(function (w) {
      var id = w.kind === "pr" ? w.bead : w.id;
      if (!id) return;
      var s = seen[id];
      if (s) { s.whys.push([ruleName(w.rule), w.why]); return; }
      var where = at(id) || at(w.plan) || at("project:" + w.key);
      if (!where) return;
      var p = P.who[id] || { name: "", word: w.kind === "plan" || w.kind === "seed" ? "Owner" : "Gardener" };
      s = seen[id] = { key: "weed:" + id, kind: "weed", rule: w.rule, bead: id, title: w.title, whys: [[ruleName(w.rule), w.why]], at: where, cur: where,
        who: p.name, whoWord: p.word, state: P.state[id] || (w.kind === "pr" ? "closed" : ""), score: w.score, since: w.since };
      s.actions = actions(s, o.me, o.ro);
      if (s.actions.length || o.ro) out.push(s);
    });
    var inPlan = {};
    ((o.world && o.world.biomes) || []).forEach(function (b) { b.plots.forEach(function (p) { ((p.plan && p.plan.waiting) || []).forEach(function (x) { inPlan[x.id] = 1; }); }); });
    (o.targets || []).forEach(function (t) {
      var first = null, since = now, title = t.name, answer = "", where, n = t.count;
      var box = t.kind === "mailbox";
      if (box) {
        (o.asks || []).forEach(function (a) { if (!inPlan[a.id] && (t.asks || []).indexOf(a.id) >= 0 && (!first || a.u < first.since)) first = { id: a.id, title: a.title, since: a.u }; });
        title = t.name.charAt(0).toUpperCase() + t.name.slice(1); where = at(t.id);
      } else {
        var plot = o.world && o.world.index[t.id], pl = plot && plot.plan;
        if (!pl) return;
        ((pl.waiting || []).filter(function (x) { return x.human; })).forEach(function (x) { if (!first || x.since < first.since) first = x; });
        since = pl.activity || pl.updated || now; answer = t.id; where = at(t.id);
      }
      if (!where) return;
      if (first) { since = first.since || since; answer = box ? "" : first.id; }
      var why = (n === 1 ? "A decision or escalation" + (box ? " that no plan carries" : "") + " waits" : n + " decisions or escalations" + (box ? " that no plan carries" : "") + " wait") + " on you" +
        (first ? "; the oldest, “" + first.title + "”, for " + age(now - since) : "") + ".";
      var s = { key: "ask:" + t.id, kind: "ask", rule: "ask", bead: box ? "" : t.id, title: title, whys: [[ruleName("ask"), why]], at: where, cur: where,
        who: "", whoWord: "", state: "", score: weight(ASK_WEIGHT, since, now), since: since, answer: answer, mail: box ? t.id : "" };
      s.actions = actions(s, o.me, o.ro);
      out.push(s);
    });
    out.sort(function (a, b) { return b.score - a.score || a.since - b.since || (a.key < b.key ? -1 : a.key > b.key ? 1 : 0); });
    return out.slice(0, STOPS);
  }
  /* welcome(o) -> the welcome tour's stops, each framing the land with nothing to do. o = { land, role, gate (the
     gatehouse), at (the land on the map, or null) }. */
  function welcome(o) {
    var land = o.land, at = o.at || null, where;
    if (o.gate) where = "Everyone who signs in without a land of their own starts here, as a guest of " + land + ".";
    else if (o.role === "owner") where = land + " is your land.";
    else if (o.role === "member") where = "You are a member of " + land + ".";
    else where = "You are a guest of " + land + ": you can look round and talk in its channels.";
    var stop = function (key, title, whys) { return { key: "welcome:" + key, kind: "welcome", rule: "", bead: "", title: title, whys: whys, at: at, cur: at, land: land, who: "", whoWord: "", state: "", actions: [] }; };
    var ask = [["Ask in words", "Say what you need in the Commons, the land's conversations. Its steward reads #general: it answers, and it fetches whoever can help."]];
    if (o.gate) ask.push(["A land of your own", "Ask the steward how to apply for one, or how to try the demo land."]);
    return [
      stop("land", o.gate ? "Welcome to the gatehouse" : "Welcome to " + land, [["Where you are", where], ["The map", "The plots are the land's plans, and the gardeners on them are its agents at work. Drag or use the arrows to look round."]]),
      stop("ask", "How to ask", ask),
      stop("general", "Your steward said hello", [["In #general", "When you were let in, " + land + "'s steward greeted you by name in #general. Answer it there, or ask it anything."]])
    ];
  }
  /* What a stop offers, suggested first: an ask is answered; a weed is pinged (when someone else holds it), adopted (when it
     is open and not yours already), deferred or closed (when open). A read-only viewer (ro, gd-GuZ3tbnTFI) may only go and
     read: an ask is looked at, a weed offers nothing, since every write it would post is closed to them. */
  function actions(s, me, ro) {
    if (s.kind === "ask") return [ro ? "look" : "answer"];
    if (ro) return [];
    var mine = !!me && s.who === me, open = s.state !== "closed";
    return (SUGGEST[s.rule] || []).filter(function (a) {
      if (a === "ping") return !!s.who && !mine;
      if (a === "adopt") return open && !mine;
      return open;
    });
  }
  /* The comment a ping leaves on the bead: addressed to its owner, with every reason the tour stopped there. */
  function pingText(s) {
    return "@" + s.who + " the chore tour in /garden stopped at " + s.bead + ". " + s.whys.map(function (w) { return w[0] + ": " + w[1]; }).join(" ") +
      (s.state === "closed" ? " Could you see to it?" : " Could you tend it: post an update, hand it on, or say if it should be deferred or closed?");
  }
  /* The board rect the cursor's camera frames into while the card is up: the largest strip above, below, left or right of the
     card (board px), under the header (top) and above a bar along the bottom (bottom: the phone tab bar's top, else vp.h). */
  function free(vp, top, card, bottom) {
    var y0 = Math.max(0, top || 0), y1 = Math.min(vp.h, bottom || vp.h), c = card, all = { x: 0, y: y0, w: vp.w, h: y1 - y0 };
    if (!c || !c.w || !c.h) return all;
    var cand = [
      { x: 0, y: y0, w: vp.w, h: Math.min(c.y, y1) - y0 },
      { x: 0, y: c.y + c.h, w: vp.w, h: y1 - c.y - c.h },
      { x: 0, y: y0, w: c.x, h: y1 - y0 },
      { x: c.x + c.w, y: y0, w: vp.w - c.x - c.w, h: y1 - y0 }
    ].filter(function (r) { return r.w > 0 && r.h > 0; });
    cand.sort(function (a, b) { return b.w * b.h - a.w * a.h; });
    return cand[0] || all;
  }
  /* The free rect as the HUD insets CURSOR.frame takes ({top, right, bottom, left} board px), so the cursor's camera fits its
     grid in the map the card leaves free. */
  function inset(vp, f) { return { top: f.y, right: vp.w - f.x - f.w, bottom: vp.h - f.y - f.h, left: f.x }; }

  /* ---------- DOM ---------- */
  /* measure(board, vp, top): the insets the open card (#tour) leaves the cursor's camera, under the header (top) and above the
     phone's tab bar while it is a row (held sideways it is a rail beside the board, gd-p0DsdxVxJU). */
  function measure(board, vp, top) {
    var o = board.getBoundingClientRect(), c = document.getElementById("tour").getBoundingClientRect(), tb = document.getElementById("tabbar").getBoundingClientRect();
    return inset(vp, free(vp, top, { x: c.left - o.left, y: c.top - o.top, w: c.width, h: c.height }, tb.height && tb.width > tb.height ? tb.top - o.top : vp.h));
  }
  /* The in-garden bead view (web/workbench.js takes the garden:open-bead event), else the bead page. */
  function openBead(id, href) {
    var ev;
    try { ev = new CustomEvent("garden:open-bead", { detail: { id: id }, cancelable: true }); } catch (e) { ev = null; }
    if (!ev || document.dispatchEvent(ev)) location.href = href(id);
  }
  var VERB = { ping: "Ping", adopt: "Adopt", defer: "Defer", close: "Close", answer: "Answer", look: "Look at it" };
  function verb(a, s) { return a === "ping" ? "Ping " + s.who : (a === "answer" || a === "look") && s.mail ? "Open the Mailbox" : VERB[a]; }
  function did(a, s) {
    return { ping: "Pinged " + s.who + " on " + s.bead + ".", adopt: "Adopted " + s.bead + ": it is yours, in progress.", defer: "Deferred " + s.bead + ".", close: "Closed " + s.bead + "." }[a];
  }
  /* mount(el, btn, h): the tour card in el, started and ended by btn. h = { stops() -> [Stop], show(stop|null) (a cursor jump
     to the stop; null, on the summary and when the tour ends, leaves the cursor where it is and re-frames it for the card),
     post(path, params) -> Promise, mail(zone) (the Mailbox at that zone's project), href(beadId), toast(msg), back (where focus goes when the tour ends: btn
     when it is not given; the Menu's button, since btn is an item in the closed Menu) }; an answer opens its bead in the bead view
     (openBead). Close and Defer ask for a second tap, like the Mailbox's answers.
     start(run) runs another tour on the card: { name, stops, last (the last button: it ends the run, then done()), done(),
     ended() (on any end) }. */
  function mount(el, btn, h) {
    if (!el || !btn) return null;
    var list = [], i = 0, tended = 0, note = "", armed = null, armTimer = 0, gen = 0, run = null;
    var CHORE = { name: "Tend the garden", stops: function () { return h.stops(); } };
    function mk(tag, cls, text) { var e = document.createElement(tag); if (cls) e.className = cls; if (text !== undefined) e.textContent = text; return e; }
    function button(cls, text, fn) { var b = mk("button", "btn small" + (cls ? " " + cls : ""), text); b.type = "button"; b.addEventListener("click", fn); return b; }
    function disarm() { if (armed) { armed.textContent = armed.dataset.was; armed.classList.remove("armed"); armed = null; } clearTimeout(armTimer); }
    function head(n) {
      el.textContent = "";
      var hd = el.appendChild(mk("div", "tour-head"));
      hd.appendChild(mk("span", "tour-n", n)).id = "tour-n";
      hd.appendChild(button("tour-x", "×", end)).setAttribute("aria-label", "End the tour");
    }
    function title(text) { var t = el.appendChild(mk("h2", "", text)); t.id = "tour-h"; t.tabIndex = -1; return t; }
    function status(text, err) {
      var p = el.appendChild(mk("p", "tour-note" + (err ? " err" : ""), text || "")); p.setAttribute("role", "status");
      if (typeof TITLES !== "undefined") TITLES.dress(p);   // "Pinged crew-a on <title>"
      return p;
    }
    function focus(n) { try { n.focus({ preventScroll: true }); } catch (e) { n.focus(); } }
    function show(k) {
      disarm(); i = k; gen++;
      if (i >= list.length) { finish(); return; }
      var s = list[i];
      head(run.name + " · stop " + (i + 1) + " of " + list.length);
      var t = title(s.title);
      s.whys.forEach(function (w) { var p = el.appendChild(mk("p", "tour-why")); p.appendChild(mk("b", "", w[0] + ":")); p.appendChild(document.createTextNode(" " + w[1])); });
      // the bead is the stop's title and its Open link, the id that link's tooltip (titles, not ids: gd-9yOATV2qCs)
      if (s.kind !== "welcome") {
        var meta = el.appendChild(mk("p", "tour-meta", s.kind === "weed" ? (s.who ? s.whoWord + ": " + s.who : "Nobody holds it") : ""));
        if (s.bead) { var a = meta.appendChild(mk("a", "tour-open", "Open")); a.href = h.href(s.bead); a.title = s.bead; }
      }
      var acts = el.appendChild(mk("div", "tour-acts"));
      s.actions.forEach(function (x, j) { var b = acts.appendChild(button(j === 0 ? "tour-do first" : "tour-do", verb(x, s), function () { act(s, x, b); })); b.dataset.act = x; });
      var nav = el.appendChild(mk("div", "tour-nav"));
      if (i > 0) nav.appendChild(button("", "Back", function () { note = ""; show(i - 1); }));
      nav.appendChild(button("tour-next", i + 1 < list.length ? "Next" : run.last || "Finish", function () { note = ""; show(i + 1); }));
      status(note); note = "";
      el.hidden = false; if (run === CHORE) btn.setAttribute("aria-expanded", "true");
      h.show(s);
      focus(t);
    }
    function act(s, a, b) {
      if (a === "answer" || a === "look") { if (s.mail) h.mail(s.mail); else openBead(s.answer, h.href); return; }
      if ((a === "close" || a === "defer") && armed !== b) {
        disarm(); armed = b; b.dataset.was = b.textContent; b.textContent = "Confirm: " + b.textContent; b.classList.add("armed");
        armTimer = setTimeout(disarm, 4000); return;
      }
      disarm();
      var id = encodeURIComponent(s.bead), my = gen, btns = el.querySelectorAll(".tour-acts button"), st = el.querySelector(".tour-note");
      Array.prototype.forEach.call(btns, function (x) { x.disabled = true; });
      st.className = "tour-note"; st.textContent = "Sending…";
      h.post(a === "ping" ? "/b/" + id + "/comments" : "/b/" + id + "/" + PATH[a], a === "ping" ? { text: pingText(s) } : {}).then(function () {
        tended++; s.done = did(a, s);
        if (my === gen) { note = s.done; show(i + 1); } else h.toast(s.done);   // the card moved on while it was sending
      }, function (e) {
        if (my !== gen) { h.toast(e.message || "That did not go through."); return; }
        Array.prototype.forEach.call(btns, function (x) { x.disabled = false; });
        st.className = "tour-note err"; st.textContent = e.message || "That did not go through.";
      });
    }
    function finish() {
      if (run.last) { var r = run; end(); if (r.done) r.done(); return; }
      h.show(null);
      head("Tend the garden · done");
      var t = title(tended ? "You tended " + plural(tended, "thing", "things") + "." : "Nothing tended this time.");
      el.appendChild(mk("p", "tour-why", "The tour takes the " + plural(list.length, "most neglected thing", "most neglected things") + " on the farm, heaviest first. The thistles and the arrows at the map's edge show the rest."));
      var nav = el.appendChild(mk("div", "tour-nav"));
      nav.appendChild(button("", "Start again", start));
      nav.appendChild(button("tour-next", "Done", end));
      status(note); note = "";
      focus(t);
    }
    function start(r) {
      if (run) end();
      run = r && r.stops ? r : CHORE;
      list = typeof run.stops === "function" ? run.stops() : run.stops.slice(); tended = 0; note = "";
      if (!list.length) { var chore = run === CHORE; end(); if (chore) h.toast("Nothing to tend: no weeds on the map, and nothing waits on you."); return; }
      show(0);
    }
    function end() {
      var was = !el.hidden, r = run;
      disarm(); gen++; list = []; run = null; el.hidden = true; btn.setAttribute("aria-expanded", "false"); h.show(null);
      if (r && r.ended) r.ended();
      if (was) focus(h.back || btn);
    }
    btn.addEventListener("click", function () { if (el.hidden) start(); else end(); });
    el.addEventListener("keydown", function (e) { if (e.key === "Escape") { e.stopPropagation(); e.preventDefault(); end(); } });
    return { start: start, end: end, show: show };
  }

  return { STOPS: STOPS, ASK_WEIGHT: ASK_WEIGHT, SUGGEST: SUGGEST, stops: stops, welcome: welcome, actions: actions, pingText: pingText, free: free, inset: inset, measure: measure, mount: mount };
})();
if (typeof module !== "undefined") module.exports = TOUR;
