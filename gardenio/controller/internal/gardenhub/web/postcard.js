/* POSTCARD (gd-WxRuyfdUtR): the plot at the cursor as a picture posted into the Commons at its plan's thread, with a
   one-line caption, so the bridge uploads it into the plan's Slack thread
   (bridge/thread_message_forward.go). The picture is #cv as drawn (its device-pixel backing store) cropped to the plot and a
   margin, never the labels over it; where the view cuts the plot off, the card shows what the view shows. Its context is a
   farm snapshot's (SNAP.snapContext, fields.snap) framing the plan, so the Commons and Slack link the view at the plot.
   The place is the plan's thread as the Commons locates it (GET /garden/c/bead/{id}, read on the viewer's own scope), else
   the #general of the plan's project; once posted, the workbench opens on the thread. Signed-in viewers only, since
   posting needs one; the Menu's Postcard item. With the cursor in a project but on no plan, the card is the project's: its
   farmstead and its first ring of plots (farm, homestead-world-design.md 29.6), to the project's #general.
   garden.js passes its state in at the press (wire's frame()): the canvas, viewport, the cursor's plot (WORLD's plot, its
   rect on screen and its at) or else its project (WORLD's biome, the farm's rect on screen, its at), the level, the page
   base and path, scope and night. crop, farm, phase, caption, farmCaption and target are pure and run under node
   (postcard_test.go); the rest is browser only. */
var POSTCARD = (function () {
  "use strict";
  var PAD = 12, MIN = 24, TITLE_MAX = 120;
  /* The device-pixel box of the canvas a postcard of r (a screen rect in CSS px) shows: r and PAD around it, cut to the
     viewport and rounded out to whole pixels; null when less than MIN CSS px of it is in view either way. */
  function crop(r, vp, dpr) {
    if (!r || !vp || !vp.w || !vp.h) return null;
    var x0 = Math.max(0, r.x - PAD), y0 = Math.max(0, r.y - PAD), x1 = Math.min(vp.w, r.x + r.w + PAD), y1 = Math.min(vp.h, r.y + r.h + PAD);
    if (x1 - x0 < MIN || y1 - y0 < MIN) return null;
    var d = dpr || 1, x = Math.floor(x0 * d), y = Math.floor(y0 * d);
    return { x: x, y: y, w: Math.ceil(x1 * d) - x, h: Math.ceil(y1 * d) - y, css: { w: Math.round(x1 - x0), h: Math.round(y1 - y0) } };
  }
  /* The rect (tiles) a project's postcard shows: its farmstead (slot 0) and the plots of the spiral's first ring round it,
     slots 1 to 8 (29.1). */
  function farm(b) {
    var h = b.home, x0 = h.x, y0 = h.y, x1 = h.x + h.w, y1 = h.y + h.h;
    (b.plots || []).forEach(function (p) { if (p.slot >= 1 && p.slot <= 8) { x0 = Math.min(x0, p.x); y0 = Math.min(y0, p.y); x1 = Math.max(x1, p.x + p.w); y1 = Math.max(y1, p.y + p.h); } });
    return { x: x0, y: y0, w: x1 - x0, h: y1 - y0 };
  }
  /* A project's line: "Postcard: gardenio · 9 plans · 2 need you". */
  function farmCaption(b) {
    var n = (b.plots || []).length + (b.overflow ? b.overflow.n : 0), parts = ["Postcard: " + String(b.name || b.key || "a project").replace(/\s+/g, " ").trim()];
    if (n) parts.push(n + (n === 1 ? " plan" : " plans"));
    if (b.needs) parts.push(b.needs + (b.needs === 1 ? " needs you" : " need you"));
    return parts.join(" · ");
  }
  /* The plan's current phase, as the plot's status says it (cursor.js buildPlot): the first phase not closed, {n, of, title};
     null for a plan without phases or with all of them closed. */
  function phase(plan) {
    var ps = (plan && plan.phases) || [];
    for (var i = 0; i < ps.length; i++) if (ps[i].status !== "closed") return { n: i + 1, of: ps.length, title: String(ps[i].title || "") };
    return null;
  }
  function steps(c) { c = c || {}; return (c.in_progress || 0) + (c.review || 0) + (c.blocked || 0) + (c.open || 0) + (c.deferred || 0) + (c.closed || 0); }
  /* One line: "Postcard: <title> · phase 2 of 4: <phase> · building · 3/9 steps". The status is the plan's plan_state, else
     its bead status; the parts it has no words for are left out. The view link goes with the picture (the snap context). */
  function caption(plan) {
    plan = plan || {};
    var title = String(plan.title || plan.id || "a plan").replace(/\s+/g, " ").trim(), ph = phase(plan), n = steps(plan.counts), parts = [];
    if (title.length > TITLE_MAX) title = title.slice(0, TITLE_MAX - 1).trim() + "…";
    if (ph) parts.push("phase " + ph.n + " of " + ph.of + (ph.title ? ": " + ph.title.replace(/\s+/g, " ").trim() : ""));
    var st = String(plan.state || plan.status || "").replace(/_/g, " ");
    if (st) parts.push(st);
    if (n) parts.push(((plan.counts && plan.counts.closed) || 0) + "/" + n + " steps");
    return ["Postcard: " + title].concat(parts).join(" · ");
  }
  /* Where the card goes: the plan's thread when the Commons placed it ({land, channel, thread}), else its project's land
     ({land}, whose #general takes it); null with neither. */
  function target(place, project) {
    if (place && place.land && place.channel && place.thread) return { land: String(place.land), channel: String(place.channel), thread: String(place.thread) };
    project = String(project || "").trim();
    return project ? { land: project } : null;
  }

  function canvasPNG(c) { return new Promise(function (ok, no) { try { c.toBlob(function (b) { if (b) ok(b); else no(new Error("The plot could not be drawn as a picture.")); }, "image/png"); } catch (e) { no(e); } }); }
  /* box of cv as a PNG, at DPR 1 (unsmoothed, the pixel art stays crisp) when it is over max */
  function png(cv, box, max) {
    var draw = function (w, h) {
      var c = document.createElement("canvas"), x;
      c.width = Math.max(1, w); c.height = Math.max(1, h); x = c.getContext("2d");
      x.imageSmoothingEnabled = false; x.drawImage(cv, box.x, box.y, box.w, box.h, 0, 0, c.width, c.height);
      return canvasPNG(c);
    };
    return draw(box.w, box.h).then(function (b) { return b.size <= max || box.w <= box.css.w ? b : draw(box.css.w, box.css.h); });
  }
  /* The Commons' place for the plan, or null when it has none the viewer may see (or the read fails). */
  function place(base, id) {
    if (typeof fetch !== "function") return Promise.resolve(null);
    return fetch(base + "/garden/c/bead/" + encodeURIComponent(id), { headers: { Accept: "application/json" }, credentials: "same-origin" })
      .then(function (r) { return r.ok ? r.json() : null; }).catch(function () { return null; });
  }
  /* wire shows btn when shown and returns send(max), which the button and GARDEN.postcard call. frame() is garden.js's state
     at the press, or null when the farm is not the view; toast(msg) tells the viewer why a postcard did not go. */
  function wire(btn, shown, frame, toast) {
    var busy = false;
    btn.hidden = !shown;
    function send(max) {
      var f = busy ? null : frame();
      if (!f) return Promise.resolve(null);
      var p = f.plot, plan = p && p.plan, b = !plan && f.biome && f.biome.key ? f.biome : null;
      if (!plan && !b) { toast("Put the cursor on a plan or a project to send its postcard."); return Promise.resolve(null); }
      var box = crop(f.rect, f.vp, f.vp.dpr);
      if (!box) { toast((plan ? "The plot" : "The farm") + " is out of view. Bring it into view to send its postcard."); return Promise.resolve(null); }
      busy = true; btn.disabled = true;
      var now = new Date(), text = plan ? caption(plan) : farmCaption(b), ctx = SNAP.snapContext({ path: f.path, search: location.search, at: f.at || "", level: f.at ? f.level : null, who: f.who, night: f.night,
        items: [plan ? { kind: "plot", id: plan.id, title: plan.title } : { kind: "biome", id: b.key, title: b.name }], vp: box.css, now: now });
      return Promise.all([png(f.cv, box, max), plan ? place(f.base, plan.id) : null]).then(function (got) {
        var to = plan ? target(got[1], plan.project || p.project) : target(null, b.key);
        if (!to) { toast("This plan has no thread or project to send its postcard to."); return null; }
        var name = SNAP.snapName(to.land, plan ? plan.id : b.key, new Date(ctx.taken_at)).replace(/^farm-/, "postcard-");   // postcard-<land>-<plan id or project>-<when>.png
        return SNAP.post(f.base, to, got[0], ctx, text, name).then(function (id) {
          location.assign(f.base + "/garden/c/n/" + encodeURIComponent(to.thread || id));
          return { to: to, context: ctx, text: text };
        });
      })
        .catch(function (e) { toast(e && e.message || "The plot could not be drawn as a picture."); if (e && e.gone) btn.hidden = true; return null; })
        .then(function (r) { busy = false; btn.disabled = false; return r; });
    }
    btn.onclick = function () { send(SNAP_MAX); };
    return send;
  }
  var SNAP_MAX = 10 << 20;   // the Commons' file cap (internal/commons files.go), as snap.js's MAX
  return { PAD: PAD, MIN: MIN, MAX: SNAP_MAX, crop: crop, farm: farm, phase: phase, caption: caption, farmCaption: farmCaption, target: target, wire: wire };
})();
if (typeof module !== "undefined") module.exports = POSTCARD;
