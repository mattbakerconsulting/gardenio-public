/* A land window's page (window.go, templates/window.html "window_land"; Lands: the land window LW.4 gd-OJOsIH52NE). Served
   as /w/static/land.js after the skin clock and gardenhub's page-scene.js bundle (SCENE and WORLD: the World drawn as /garden
   draws it, LW.3), the page's only script. It fetches land.json, the land view shaped as the farm document the scene reads,
   draws it on the canvas and then shows the canvas. Drag, pinch, the wheel and the arrow and +/- keys move the camera; a tap
   on a plan, or its name in the list, opens its read-only card, which links nowhere. Every 20 s it asks land.json whether the
   land changed, sending the last ETag so an unchanged land is a 304 that costs the daemon nothing; when it changed it redraws
   and swaps the list for a fresh render (?refresh=1, which the viewer does not count as a view). The page is complete
   without it. The owner's preview (land_share.go, LW.5) has no land.json: it carries the document inline (#w-seed), which
   this draws once and never polls. */
(function () {
  'use strict';
  var EVERY = 20000, TAP = 10, LABELS = { draft: 'Draft', frosted: 'Draft', ready: 'Ready', building: 'Building', complete: 'Complete', abandoned: 'Abandoned' };
  var page = location.pathname.replace(/\/+$/, ''), etag = '', doc = null, st = null, cam = null, vp = { w: 0, h: 0, dpr: 1 };
  var cv, sc, box, card, raf = 0, opened = '', from = null, ptrs = {}, drag = null;
  var reduce = !!(window.matchMedia && matchMedia('(prefers-reduced-motion: reduce)').matches);

  // ago says how long before now t was, as window.go's windowAgo does.
  function ago(t, now) {
    var d = now - t, min = 60000, hour = 60 * min, day = 24 * hour;
    if (d < min) return 'just now';
    if (d < hour) return Math.floor(d / min) + ' min ago';
    if (d < day) return Math.floor(d / hour) + ' h ago';
    if (d < 30 * day) return Math.floor(d / day) + ' d ago';
    var dt = new Date(t);
    return dt.getUTCDate() + ' ' + ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'][dt.getUTCMonth()] + ' ' + dt.getUTCFullYear();
  }
  function tick() {
    var now = Date.now();
    document.querySelectorAll('time[data-ago]').forEach(function (el) {
      var t = Date.parse(el.getAttribute('datetime'));
      if (!isNaN(t)) el.textContent = ago(t, now);
    });
  }

  function night() { return /^dark/.test(document.documentElement.getAttribute('data-skin') || ''); }
  function bbox() { return st && st.world.bbox; }
  function paint() {
    raf = 0;
    if (!st || !cam) return;
    var peek = opened && st.world.index[opened];
    if (sc.draw({ cam: cam, vp: vp, night: night(), reduce: reduce, world: st.world, gs: st.gs, sst: st.sst, edgeIx: st.edgeIx, weedIx: st.weedIx, nb: st.nb, peek: peek || null })) later();
  }
  function later() { if (!raf) raf = requestAnimationFrame(paint); }
  function move(c) { cam = c; later(); }

  // The land's regions in world px: what the first frame fits.
  function landRect() {
    var r = null;
    st.world.biomes.forEach(function (b) {
      var f = WORLD.tiles(b.foot);
      r = r ? { x: Math.min(r.x, f.x), y: Math.min(r.y, f.y), w: Math.max(r.x + r.w, f.x + f.w) - Math.min(r.x, f.x), h: Math.max(r.y + r.h, f.y + f.h) - Math.min(r.y, f.y) } : f;
    });
    return r || st.world.bbox;
  }
  function resize() {
    var r = cv.getBoundingClientRect(), dpr = Math.min(window.devicePixelRatio || 1, 2);
    if (!r.width || !r.height) return;
    vp = { w: r.width, h: r.height, dpr: dpr };
    cv.width = Math.round(r.width * dpr);
    cv.height = Math.round(r.height * dpr);
    if (!cam && st) cam = WORLD.fit(landRect(), vp, { pad: 0.04 });
    later();
  }
  function setDoc(d) {
    doc = d;
    st = SCENE.lay(d);
    if (opened) show(opened);
    later();
  }

  /* The card: what the land view says of one plan, a seed or a finished plan, in words. */
  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  }
  function find(id) {
    var lists = [['plan', doc.plans], ['seed', doc.seeds], ['closed', doc.closed_plans.shown]];
    for (var i = 0; i < lists.length; i++) for (var j = 0; j < lists[i][1].length; j++) if (lists[i][1][j].id === id) return { kind: lists[i][0], p: lists[i][1][j] };
    return null;
  }
  function list(items) {
    var ul = el('ul', 'w-list');
    items.forEach(function (it) {
      var li = el('li', it[2] || '');
      if (it[2]) li.append(el('span', 'w-dot'));
      li.append(el('span', 'w-what', it[0]), ' ', el('span', 'w-meta', it[1]));
      ul.append(li);
    });
    return ul;
  }
  function total(c) { return c.in_progress + c.blocked + c.open + c.deferred + c.closed; }
  function show(id, focus) {
    var t = doc && find(id);
    if (!t) { hide(); return; }
    var p = t.p, state = t.kind === 'seed' ? p.state : p.state || '', x = el('button', 'w-x', '×'), chip = el('p', 'w-status');
    opened = id;
    card.replaceChildren();
    x.type = 'button';
    x.setAttribute('aria-label', 'Close the card');
    x.addEventListener('click', function () { hide(true); });
    chip.append(el('span', 'w-chip w-state-' + state, (t.kind === 'closed' ? 'Finished · ' : '') + (LABELS[state] || 'Active')));
    var h = el('h2', '', p.title || p.short_title || 'A plan');
    h.id = 'w-card-h';
    card.append(x, h, chip);
    if (t.kind === 'plan') {
      var n = total(p.counts), done = p.counts.closed;
      if (n) {
        var bar = el('div', 'w-bar'), fill = el('span');
        fill.style.width = Math.round(100 * done / n) + '%';
        bar.append(fill);
        bar.setAttribute('aria-hidden', 'true');
        card.append(bar, el('p', 'w-progress', done + ' of ' + n + ' steps done'));
      }
      var phases = p.phases.map(function (ph) { return [ph.title || ph.short_title, total(ph.counts) ? ph.counts.closed + ' of ' + total(ph.counts) + ' done' : 'No steps yet', 'w-st-' + (total(ph.counts) && ph.counts.closed === total(ph.counts) ? 'done' : ph.counts.closed || ph.counts.in_progress ? 'working' : 'todo')]; });
      if (p.unphased.tasks.length || (p.unphased.closed || []).length) {
        phases.push([phases.length ? 'Other steps' : 'Steps', p.unphased.counts.closed + ' of ' + total(p.unphased.counts) + ' done', 'w-st-' + (p.unphased.counts.closed === total(p.unphased.counts) ? 'done' : 'working')]);
      }
      if (phases.length) card.append(el('h3', '', 'Phases'), list(phases));
      var waits = (p.waiting || []).map(function (w) { return [w.title, 'Waiting for an answer']; });
      if (p.open_escalations) waits.push([p.open_escalations === 1 ? '1 request' : p.open_escalations + ' requests', 'Waiting on the operator']);
      card.append(el('h3', '', 'Waiting on the operator'), waits.length ? list(waits) : el('p', 'w-meta', 'Nothing is waiting on the operator.'));
    } else if (t.kind === 'seed') {
      card.append(el('p', '', p.state === 'ready' ? 'Staked out and waiting to be planted.' : 'A draft in the nursery, not yet approved.'));
      if (p.steps && p.steps.length) card.append(el('h3', '', 'Steps'), list(p.steps.map(function (s) { return [s.title, 'Not started']; })));
      else if (p.tasks) card.append(el('p', 'w-meta', p.tasks === 1 ? '1 step' : p.tasks + ' steps'));
    }
    var crew = doc.gardeners.filter(function (g) { return g.plot && g.plot.plan === id; });
    if (crew.length) {
      card.append(el('h3', '', 'Crew'), list(crew.map(function (g) { return [g.agent, g.bucket === 'alive' ? 'Working now' : 'Idle', g.bucket === 'alive' ? 'w-live' : '']; })));
    }
    card.hidden = false;
    if (focus) { card.tabIndex = -1; card.focus({ preventScroll: true }); }
    later();
  }
  function hide(back) {
    var was = opened;
    opened = '';
    card.hidden = true;
    card.replaceChildren();
    if (back && from && from.isConnected) from.focus();
    else if (back) cv.focus();
    from = null;
    if (was) later();
  }
  // The thing under a tap: a plot (a plan, or a staked seed), else a tree (a finished plan) or a pot (a draft).
  function tap(x, y) {
    var w = WORLD.toWorld(cam, vp, x, y), r = TAP / cam.z;
    var vis = WORLD.visible(st.world, { x: w.x - r, y: w.y - r, w: 2 * r, h: 2 * r }, 3);
    var hit = vis.plots[0] || vis.items.filter(function (it) { return it.plan || it.seed; })[0];
    if (hit) show((hit.plan || hit.seed).id);
    else hide();
  }
  // From the list: open the card and bring the plan into view.
  function pick(id, btn) {
    var at = st && st.world.index[id];
    from = btn;
    if (at && cam) move(WORLD.clampCam(WORLD.fit(WORLD.tiles(at), vp, { pad: 1.2, max: Math.max(cam.z, WORLD.levelRange(1)[0]) }), bbox()));
    box.scrollIntoView({ block: 'nearest', behavior: reduce ? 'auto' : 'smooth' });
    show(id, true);
  }
  function enhanceList() {
    if (!doc) return;
    document.querySelectorAll('#w-plans li[data-plan]').forEach(function (li) {
      var what = li.querySelector('.w-what'), id = li.getAttribute('data-plan');
      if (!what || !find(id)) return;
      var b = el('button', 'w-pick w-what', what.textContent);
      b.type = 'button';
      b.addEventListener('click', function () { pick(id, b); });
      what.replaceWith(b);
    });
  }

  /* Pointers: one drags, two pinch about their midpoint; a press that moved less than TAP px is a tap. */
  function local(e) { var r = cv.getBoundingClientRect(); return { x: e.clientX - r.left, y: e.clientY - r.top }; }
  function pair() {
    var ps = Object.keys(ptrs).map(function (k) { return ptrs[k]; }).slice(0, 2);
    return { x: (ps[0].x + ps[1].x) / 2, y: (ps[0].y + ps[1].y) / 2, d: Math.hypot(ps[0].x - ps[1].x, ps[0].y - ps[1].y) };
  }
  function input() {
    cv.addEventListener('pointerdown', function (e) {
      if (!cam) return;
      try { cv.setPointerCapture(e.pointerId); } catch (err) { /* a pointer already gone */ }
      ptrs[e.pointerId] = local(e);
      if (Object.keys(ptrs).length === 1) drag = { x: ptrs[e.pointerId].x, y: ptrs[e.pointerId].y, moved: false };
      else if (drag) drag.moved = true;
    });
    cv.addEventListener('pointermove', function (e) {
      var p = ptrs[e.pointerId];
      if (!p || !cam) return;
      var q = local(e);
      if (Object.keys(ptrs).length >= 2) {
        var a = pair();
        ptrs[e.pointerId] = q;
        var b = pair(), c = a.d > 0 ? WORLD.zoomAt(cam, b.d / a.d, b.x, b.y, vp, bbox()) : cam;
        move(WORLD.panBy(c, b.x - a.x, b.y - a.y, bbox()));
        return;
      }
      ptrs[e.pointerId] = q;
      if (drag && !drag.moved && Math.hypot(q.x - drag.x, q.y - drag.y) > TAP) drag.moved = true;
      if (drag && drag.moved) move(WORLD.panBy(cam, q.x - p.x, q.y - p.y, bbox()));
    });
    function up(e) {
      if (!ptrs[e.pointerId]) return;
      var q = local(e), n = Object.keys(ptrs).length;
      delete ptrs[e.pointerId];
      if (e.type === 'pointerup' && n === 1 && drag && !drag.moved) tap(q.x, q.y);
      if (n === 1) drag = null;
    }
    cv.addEventListener('pointerup', up);
    cv.addEventListener('pointercancel', up);
    cv.addEventListener('wheel', function (e) {
      if (!cam) return;
      e.preventDefault();
      var l = local(e);
      move(WORLD.zoomAt(cam, Math.exp(-e.deltaY * (e.deltaMode ? 0.05 : 0.002)), l.x, l.y, vp, bbox()));
    }, { passive: false });
    cv.addEventListener('keydown', function (e) {
      if (!cam) return;
      var k = e.key, step = Math.min(vp.w, vp.h) * 0.2, pan = { ArrowLeft: [step, 0], ArrowRight: [-step, 0], ArrowUp: [0, step], ArrowDown: [0, -step] }[k];
      if (pan) move(WORLD.panBy(cam, pan[0], pan[1], bbox()));
      else if (k === '+' || k === '=') move(WORLD.zoomAt(cam, 1.25, vp.w / 2, vp.h / 2, vp, bbox()));
      else if (k === '-' || k === '_') move(WORLD.zoomAt(cam, 0.8, vp.w / 2, vp.h / 2, vp, bbox()));
      else if (k === 'Escape' && opened) hide(true);
      else return;
      e.preventDefault();
    });
    card.addEventListener('keydown', function (e) { if (e.key === 'Escape') { e.preventDefault(); hide(true); } });
    window.addEventListener('resize', resize);
    new MutationObserver(later).observe(document.documentElement, { attributes: true, attributeFilter: ['data-skin'] });
  }

  function swapList(html) {
    var fresh = new DOMParser().parseFromString(html, 'text/html'), list = document.getElementById('w-plans'), sum = document.querySelector('.w-sum');
    var fl = fresh.getElementById('w-plans'), fs = fresh.querySelector('.w-sum');
    if (list && fl) list.replaceChildren.apply(list, Array.prototype.map.call(fl.childNodes, function (n) { return document.importNode(n, true); }));
    if (sum && fs) sum.textContent = fs.textContent;
    enhanceList();
  }
  function first() {
    box.hidden = false;
    resize();
    enhanceList();
    return null;
  }
  function refresh(start) {
    if (document.hidden && !start) return;
    fetch(page + '/land.json', { headers: etag ? { 'If-None-Match': etag } : {}, cache: 'no-store', credentials: 'omit' }).then(function (res) {
      if (res.status === 304 || !res.ok) return null;
      etag = res.headers.get('ETag') || '';
      return res.json();
    }).then(function (d) {
      if (!d) return null;
      var had = !!doc;
      setDoc(d);
      if (!had) return first();
      return fetch(page + '?refresh=1', { cache: 'no-store', credentials: 'omit' }).then(function (r) { return r.ok ? r.text() : null; }).then(function (html) { if (html) swapList(html); });
    }).then(function () {
      var updated = document.getElementById('w-updated');
      if (updated && doc) updated.setAttribute('datetime', new Date().toISOString());
      tick();
    }).catch(function () { /* the next round tries again */ });
  }

  document.addEventListener('DOMContentLoaded', function () {
    box = document.getElementById('w-world');
    cv = document.getElementById('w-cv');
    card = document.getElementById('w-card');
    if (!box || !cv || !card || typeof SCENE === 'undefined') return;
    sc = SCENE.scene(cv);
    input();
    var seed = document.getElementById('w-seed');
    if (seed) {
      try { setDoc(JSON.parse(seed.textContent)); first(); } catch (e) { /* the list stands alone */ }
      return;
    }
    refresh(true);
    setInterval(refresh, EVERY);
    document.addEventListener('visibilitychange', function () { if (!document.hidden) refresh(); });
  });
})();
