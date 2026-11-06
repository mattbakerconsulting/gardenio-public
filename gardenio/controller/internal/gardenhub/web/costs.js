/* web/costs.js: the Costs panel (gardenio/docs/garden-grafana-panels.md), the Workbench's costs kind,
   /garden?costs=fleet|<project>|agent:<name>: an operator's Grafana cost charts for the fleet or one project as panel
   cards, or one gardener's bill (no farm figures in its header). It reads /garden/charts/<set> (charts.go builds every
   src and open_url) and shows a header of the farm's own figures for the scope (farm_cost.go; "as of" when the rollup did
   not answer, a no-spend-yet line for a project with no row), the range chips (default 7 days, localStorage
   garden.costs.range; gone with one line why when there is no chart to move), the cards (paintCostCards, also the
   Project panel's: each frame lazy, its src set within 200 px of view, a slow line after 15 s) and "Open the dashboard in
   Grafana" with the sign-in footer. Degraded: no Grafana base, a 403 for
   non-operators, a failed load with Try again; the header still paints. theme=dark: the panel is the --box surface.
   Text goes in through textContent.

   The openers: the Menu's Costs item (body data-costs) opens the cursor's project, else the fleet's; the header purse
   (paintPurse) says $ today and $ remaining. A tenant's item and purse open its land's credits, land:<name> (credits.js,
   page-land.js's: the purse's credits row waits for it). */
var COSTS = (function () {
  /* ---------- pure logic (also run under node by the Go tests) ---------- */
  var RANGES = [['today', 'Today'], ['7d', '7 days'], ['30d', '30 days']];
  var DEFAULT_RANGE = '7d', KEY = 'garden.costs.range', THEME = 'dark', FLEET = 'fleet', AGENT = 'agent:', LAND = 'land:';
  var SLOW_MS = 15000, MARGIN = '0px 0px 200px 0px';
  var UNSET = 'Grafana isn’t set up in this environment.', WAIT = 'Loading chart…', SLOW = 'This chart is taking a while.';
  function isRange(r) { return RANGES.some(function (x) { return x[0] === r; }); }
  function rangeWord(r) { var x = RANGES.filter(function (x) { return x[0] === r; })[0]; return x ? x[1] : r; }
  /* The agent an id names (agent:<name>), else ''. */
  function agentOf(id) { return String(id).indexOf(AGENT) === 0 ? String(id).slice(AGENT.length) : ''; }
  /* The tenant land an id names (land:<name>, step 3.3: its credits, drawn by credits.js), else ''. */
  function landOf(id) { return String(id).indexOf(LAND) === 0 ? String(id).slice(LAND.length) : ''; }
  /* The chart set an id names: the fleet's, one agent's, one land's credits, else the one project's. */
  function setOf(id) { return id === FLEET ? 'costs-fleet' : agentOf(id) ? 'agent' : landOf(id) ? 'costs-land' : 'costs-project'; }
  function scopeName(id) { return id === FLEET ? 'Fleet' : agentOf(id) || landOf(id) || id; }
  /* The set's route under base for id in range: the server answers the cards built for it. */
  function chartsURL(id, range) {
    var a = agentOf(id), l = landOf(id), q = id === FLEET ? '' : (a ? 'agent=' + encodeURIComponent(a) : l ? 'land=' + encodeURIComponent(l) : 'project=' + encodeURIComponent(id)) + '&';
    return '/garden/charts/' + setOf(id) + '?' + q + 'range=' + encodeURIComponent(isRange(range) ? range : DEFAULT_RANGE) + (l ? '' : '&theme=' + THEME);
  }
  /* The range the viewer last chose (store: localStorage, or null when it is blocked), else 7 days. */
  function readRange(store) {
    try { var r = store && store.getItem(KEY); return isRange(r) ? r : DEFAULT_RANGE; } catch (e) { return DEFAULT_RANGE; }
  }
  function keepRange(store, r) { try { if (store && isRange(r)) store.setItem(KEY, r); } catch (e) { /* a blocked store: the chip still works */ } }
  /* Dollars, to the cent; a figure not known is a dash, never $0. */
  function usd(v) { return typeof v === 'number' && isFinite(v) ? '$' + v.toFixed(2) : '—'; }
  function hhmm(t) { return new Date(t * 1000).toISOString().slice(11, 16) + ' UTC'; }
  /* The header's figures from cost.projects[] for id: {rows: [label, value] pairs, stale: '' or the line that says the
     rollup did not answer}, or null when the farm has nothing for it. The fleet sums the projects' known figures. */
  function figures(cost, id) {
    var rows = ((cost && cost.projects) || []).filter(function (p) { return p && (id === FLEET || p.project === id); });
    if (!rows.length || agentOf(id) || landOf(id)) return null;
    var down = rows.filter(function (p) { return !p.has; }), asOf = down.reduce(function (m, p) { return p.as_of && (!m || p.as_of < m) ? p.as_of : m; }, 0);
    var stale = !down.length ? '' : (id === FLEET ? 'The costs rollup did not answer for ' + down.length + ' of ' + rows.length + ' projects' : 'The costs rollup did not answer') +
      (asOf ? '; these are the figures from ' + hhmm(asOf) : '') + '.';
    if (id !== FLEET) {
      var p = rows[0], out = [['Today', usd(p.today_usd)], ['This week', usd(p.week_usd)]];
      if (p.has_budget) out.push(['Budget', usd(p.budget_usd) + (p.period === '7d' ? ' a rolling week' : ' a month') + ', ' + usd(p.remaining_usd) + ' left']);
      return { rows: out, stale: stale };
    }
    function sum(k) {
      var known = rows.filter(function (p) { return typeof p[k] === 'number' && isFinite(p[k]); });
      return known.length ? known.reduce(function (s, p) { return s + p[k]; }, 0) : null;
    }
    return { rows: [['Today', usd(sum('today_usd'))], ['This week', usd(sum('week_usd'))], ['Projects', String(rows.length)]], stale: stale };
  }
  /* The header's line for a project with no row in the farm's cost blob: no spend recorded, which is not $0. */
  function unrecorded(cost, id) {
    if (id === FLEET || agentOf(id) || landOf(id) || !cost || !cost.projects || figures(cost, id)) return '';
    return 'The garden has recorded no spend for ' + id + ' yet.';
  }
  /* Why the range chips are off for a loaded answer d, in one line, or '' when some card has a chart they would move: a
     chip only changes a frame's src, so with no card or no Grafana there is nothing for it to change. */
  function rangeOff(d) {
    var panels = (d && d.panels) || [];
    if ((d && d.land) || panels.some(function (p) { return !!p.src; })) return '';
    return panels.length ? 'No range to choose: Grafana isn’t set up here.' : 'No range to choose: there are no charts here.';
  }
  /* A failed load in the panel's own words, {text, retry}, or null for the host's: the route is for operators only, and a
     project name it cannot chart is not one to try again. */
  function failure(status, id) {
    if (status === 403) return { text: 'Costs charts are for operators for now.', retry: false };
    if (landOf(id) && (status === 404 || status === 400)) return { text: 'There is no land ' + landOf(id) + ' to chart.', retry: false };
    if (status === 400 && id !== FLEET) return { text: 'There is no ' + (agentOf(id) ? 'gardener ' : 'project ') + scopeName(id) + ' to chart.', retry: false };
    return null;
  }
  /* What a card's frame box says: no src is Grafana not set up here; else the skeleton until it loads, and the slow line
     once SLOW_MS pass without a load. */
  function boxWord(card, slow) { return !card.src ? UNSET : slow ? SLOW : WAIT; }
  /* The id the Menu's Costs item opens for the cursor's project (GARDEN.here; '' off every project): the fleet's off one. */
  function scopeOf(project) { return project || FLEET; }
  /* The header purse's rows: [{name, today, left, level, stale}], the fleet's for an admin, else one per project, never
     a sum. left: null for a project with no budget (hidden), a dash for a fleet with none. A tenant land has no row. */
  function purseRows(cost) {
    function stale(x) { return x.has ? '' : x.as_of ? 'as of ' + hhmm(x.as_of) : 'not read yet'; }
    function row(name, x, left) { return { name: name, today: x.has ? usd(x.today_usd) : '—', left: left, level: x.level || '', stale: stale(x) }; }
    if (!cost) return [];
    if (cost.scope === FLEET) return [row('Fleet', cost, cost.has_budget ? usd(cost.remaining_usd) : '—')];
    if (cost.scope !== 'project') return [];
    return (cost.projects || []).filter(function (p) { return p && !p.purse; }).map(function (p) { return row(p.project, p, p.has_budget ? usd(p.remaining_usd) : null); });
  }
  /* A level in words, so it is not said by colour alone. */
  function levelWord(level) { return level === 'warn' ? 'low' : level === 'over' ? 'over budget' : ''; }
  /* A row in a sentence, for the purse's accessible name: "Fleet: $13.50 today, $86.50 left", "acme: 38.0 credits left". */
  function purseSay(r) {
    if (r.credits !== undefined) return r.name + ': ' + r.credits + ' credits left';
    var s = r.name + ': ' + r.today + ' today' + (r.stale ? ' (' + r.stale + ')' : '');
    if (r.left !== null) s += ', ' + r.left + ' left' + (levelWord(r.level) ? ', ' + levelWord(r.level) : '');
    return s;
  }
  /* The land a tenant's openers chart: the cursor's project when it is a land the farm gives a purse, else the first. */
  function landHere(cost, project) {
    var ls = ((cost && cost.projects) || []).filter(function (p) { return p && p.purse && p.purse.state !== 'unmetered'; });
    var at = ls.filter(function (p) { return p.project === project; })[0] || ls[0];
    return at ? { land: at.project, left: at.purse.remaining_credits } : null;
  }
  /* The cards' ids in order: a range change keeps them, so the cards stay and only their srcs move. */
  function sameCards(a, b) { return (a || []).map(function (c) { return c.id; }).join(',') === (b || []).map(function (c) { return c.id; }).join(','); }

  /* The panel-card stack (garden-grafana-panels.md, "The panel card") for the costs kind and the Project panel's cost
     tab: paints panels into host and returns {update(panels), stop()}. update moves the cards to a new range's srcs and
     answers false when the cards differ (paint again). opts.root: the scroll container the lazy load watches (default
     #bv-body); opts.heading: the cards' heading element (default h3; the Project panel passes h4). */
  function paintCostCards(host, panels, opts) {
    opts = opts || {};
    var doc = host.ownerDocument || document, root = opts.root || doc.getElementById('bv-body') || null;
    var cards = [], timers = [], io = null;
    function el(t, cls, text) { var e = doc.createElement(t); if (cls) e.className = cls; if (text !== undefined) e.textContent = text; return e; }
    function openLink(c, text) {
      var a = el('a', 'gp-open ext', text); a.href = c.data.open_url; a.target = '_blank'; a.rel = 'noopener';
      a.setAttribute('aria-label', 'Open ' + c.data.title + ' in Grafana');
      return a;
    }
    function say(c) {
      c.word.textContent = boxWord(c.data, c.slow);
      if (c.slow && c.data.open_url) { c.word.appendChild(doc.createTextNode(' ')); c.word.appendChild(openLink(c, 'Open')); }
      c.word.hidden = !!c.loaded;
    }
    function clock(c) {
      if (c.timer) clearTimeout(c.timer);
      c.timer = setTimeout(function () { c.timer = 0; if (!c.loaded) { c.slow = true; say(c); } }, SLOW_MS);
      timers.push(c.timer);
    }
    function start(c) {
      if (c.started || !c.frame) return;
      c.started = true; c.loaded = false; c.slow = false;
      c.frame.setAttribute('src', c.data.src);
      clock(c); say(c);
    }
    function card(p, i) {
      var c = { data: p, started: false, loaded: false, slow: false, timer: 0, frame: null };
      var s = el('section', 'gp-card'), head = el('div', 'gp-head'), h = el(opts.heading || 'h3', 'gp-t', p.title), hid = (opts.idPrefix || 'gp-c') + i + '-' + p.id;
      h.id = hid; s.setAttribute('aria-labelledby', hid);
      head.appendChild(h);
      c.open = p.open_url ? openLink(c, 'Open') : null;
      if (c.open) head.appendChild(c.open);
      s.appendChild(head);
      if (p.scope) s.appendChild(el('p', 'gp-scope', p.scope));
      var box = el('div', 'gp-box' + (p.src ? '' : ' unset'));
      c.word = el('p', 'gp-word'); c.word.setAttribute('role', 'status');
      box.appendChild(c.word);
      if (p.src) {
        var f = el('iframe', 'gp-frame');
        f.setAttribute('title', p.title + ' Grafana panel');
        f.setAttribute('height', '360'); f.setAttribute('width', '100%');
        f.setAttribute('scrolling', 'no'); f.setAttribute('loading', 'lazy');
        f.setAttribute('sandbox', 'allow-scripts allow-same-origin allow-popups');
        f.setAttribute('referrerpolicy', 'same-origin');
        f.addEventListener('load', function () {
          if (!c.started) return;   // about:blank before any src
          c.loaded = true; c.slow = false;
          if (c.timer) { clearTimeout(c.timer); c.timer = 0; }
          box.classList.add('loaded'); say(c);
        });
        box.appendChild(f); c.frame = f; c.box = box;
      }
      s.appendChild(box);
      if (p.note) s.appendChild(el('p', 'gp-note', p.note));
      c.el = s; say(c);
      return c;
    }
    host.textContent = '';
    (panels || []).forEach(function (p, i) { var c = card(p, i); cards.push(c); host.appendChild(c.el); });
    var lazy = cards.filter(function (c) { return !!c.frame; });
    if (typeof IntersectionObserver === 'undefined') lazy.forEach(start);
    else if (lazy.length) {
      io = new IntersectionObserver(function (entries) {
        entries.forEach(function (e) {
          if (!e.isIntersecting) return;
          var c = cards.filter(function (x) { return x.el === e.target; })[0];
          if (c) { start(c); io.unobserve(c.el); }
        });
      }, { root: root, rootMargin: MARGIN });
      lazy.forEach(function (c) { io.observe(c.el); });
    }
    return {
      update: function (next) {
        // other cards, or a Grafana base that came or went between two answers: paint again
        if (!sameCards(panels, next) || cards.some(function (c, i) { return !!c.data.src !== !!next[i].src; })) return false;
        panels = next;
        cards.forEach(function (c, i) {
          var p = next[i], was = c.data.src;
          c.data = p;
          if (c.open) c.open.href = p.open_url;
          if (!c.frame || p.src === was) return;
          if (c.started) { c.started = false; c.box.classList.remove('loaded'); start(c); }
        });
        return true;
      },
      stop: function () { if (io) io.disconnect(); io = null; timers.forEach(clearTimeout); timers = []; }
    };
  }

  var api = { RANGES: RANGES, DEFAULT_RANGE: DEFAULT_RANGE, KEY: KEY, SLOW_MS: SLOW_MS, MARGIN: MARGIN, AGENT: AGENT, LAND: LAND, landOf: landOf, landHere: landHere, isRange: isRange, rangeWord: rangeWord, agentOf: agentOf, setOf: setOf,
    scopeName: scopeName, chartsURL: chartsURL, readRange: readRange, keepRange: keepRange, usd: usd, figures: figures, unrecorded: unrecorded, rangeOff: rangeOff,
    failure: failure, boxWord: boxWord,
    sameCards: sameCards, paintCostCards: paintCostCards, scopeOf: scopeOf, purseRows: purseRows, levelWord: levelWord, purseSay: purseSay };
  if (typeof document === 'undefined' || typeof WORKBENCH === 'undefined') return api;

  /* ---------- DOM ---------- */
  var W = WORKBENCH;
  var titleEl = document.getElementById('bv-title'), kindEl = document.getElementById('bv-kind'), bodyEl = document.getElementById('bv-body');
  if (!bodyEl) return api;
  function store() { try { return window.localStorage; } catch (e) { return null; } }
  var range = readRange(store());
  var view = null;   // the panel painted: {id, d, stack, head, chips, dash, note}

  function el(t, cls, text) { var e = document.createElement(t); if (cls) e.className = cls; if (text !== undefined) e.textContent = text; return e; }
  function cur() { var c = W.current(); return c && c.kind === 'costs' ? c.id : ''; }
  function retryButton(id) {
    var b = el('button', 'btn small bv-retry', 'Try again'); b.type = 'button';
    b.onclick = function () { b.disabled = true; b.textContent = 'Loading…'; W.refresh('costs', id); };
    return b;
  }
  function noteLine(text, isErr, retry, id) {
    var n = el('p', 'bv-note' + (isErr ? ' err' : ''), text || '');
    n.setAttribute('role', 'status');
    if (isErr && retry) { n.appendChild(document.createTextNode(' ')); n.appendChild(retryButton(id)); }
    return n;
  }
  /* The header: the farm's figures for id, painted again whenever the farm document moves; with none, the no-spend line,
     but not over a failed load (gd-weOKYDBRGQ). */
  function header(id, doc, failed) {
    var g = window.GARDEN, box = el('div', 'gp-figs'), cost = doc ? doc.cost : g && g.cost ? g.cost() : null, f = figures(cost, id);
    if (!f) { var u = !failed && unrecorded(cost, id); if (u) box.appendChild(el('p', 'bv-note', u)); return box; }
    var dl = el('dl', 'bv-fields');
    f.rows.forEach(function (r) { dl.appendChild(el('dt', '', r[0])); dl.appendChild(el('dd', '', r[1])); });
    box.appendChild(dl);
    if (f.stale) box.appendChild(el('p', 'bv-note', f.stale));
    return box;
  }
  /* The range chips: a radiogroup of buttons, the arrows moving the choice as in any radiogroup. */
  function chips(id) {
    var g = el('div', 'qv-scope gp-chips'); g.setAttribute('role', 'radiogroup'); g.setAttribute('aria-label', 'Time range');
    RANGES.forEach(function (r, i) {
      var b = el('button', 'btn small', r[1]); b.type = 'button'; b.dataset.range = r[0];
      b.setAttribute('role', 'radio'); b.setAttribute('aria-checked', String(r[0] === range)); b.tabIndex = r[0] === range ? 0 : -1;
      b.onclick = function () { pick(id, r[0]); };
      b.onkeydown = function (e) {
        var step = e.key === 'ArrowRight' || e.key === 'ArrowDown' ? 1 : e.key === 'ArrowLeft' || e.key === 'ArrowUp' ? -1 : 0;
        if (!step) return;
        e.preventDefault();
        var next = RANGES[(i + step + RANGES.length) % RANGES.length][0];
        pick(id, next);
        var to = g.querySelector('[data-range="' + next + '"]'); if (to) to.focus();
      };
      g.appendChild(b);
    });
    return g;
  }
  function syncChips() {
    if (!view) return;
    Array.prototype.forEach.call(view.chips.children, function (b) { var on = b.dataset.range === range; b.setAttribute('aria-checked', String(on)); b.tabIndex = on ? 0 : -1; });
  }
  function pick(id, r) {
    if (r === range) return;
    range = r; keepRange(store(), r); syncChips();
    W.refresh('costs', id);
  }
  function links(d) {
    var box = el('div', 'gp-links');
    if (!d || !d.open_dashboard_url) return box;
    var a = el('a', 'gp-dash ext', 'Open the dashboard in Grafana'); a.href = d.open_dashboard_url; a.target = '_blank'; a.rel = 'noopener';
    box.appendChild(el('p', 'gp-line')).appendChild(a);
    var foot = el('p', 'gp-foot', 'Charts blank? '), s = el('a', 'ext', 'Open Grafana once to sign in'); s.href = d.open_dashboard_url; s.target = '_blank'; s.rel = 'noopener';
    foot.appendChild(s); box.appendChild(foot);
    return box;
  }
  function stop() { if (view && view.stack) view.stack.stop(); }

  function paint(id, d, note, isErr, retry) {
    kindEl.textContent = agentOf(id) ? 'gardener charts' : landOf(id) ? 'credits' : 'costs';
    titleEl.textContent = (agentOf(id) ? 'Charts: ' : landOf(id) ? 'Credits: ' : 'Costs: ') + scopeName(id);
    /* A new range's answer for the cards shown: the cards stay, their srcs move (a loaded frame reloads in place). */
    if (d && view && view.id === id && view.d && view.stack && sameCards(view.d.panels, d.panels) && view.stack.update(d.panels || [])) {
      view.d = d;
      var n = noteLine('', false, false, id); view.note.replaceWith(n); view.note = n;
      var l = links(d); view.dash.replaceWith(l); view.dash = l;
      return;
    }
    stop();
    bodyEl.textContent = '';
    view = { id: id, d: d, stack: null, failed: !d && !!isErr };
    view.head = header(id, null, view.failed); bodyEl.appendChild(view.head);
    view.chips = chips(id); bodyEl.appendChild(view.chips);
    /* A failed load: the note says why and the chips go. An answer with no chart: the chips go and one line says why. The
       loading paint keeps them, so nothing moves when the answer lands. */
    var off = d ? rangeOff(d) : '';
    view.chips.hidden = (!d && !!isErr) || !!off;
    if (off) bodyEl.appendChild(el('p', 'bv-note gp-off', off));
    view.note = noteLine(note, isErr, retry, id); bodyEl.appendChild(view.note);
    var stackEl = el('div', 'gp-cards'); bodyEl.appendChild(stackEl);
    view.dash = links(d); bodyEl.appendChild(view.dash);
    if (!d) return;
    if (landOf(id)) {   // credits.js is page-land.js's
      if (typeof CREDITS !== 'undefined') CREDITS.paint(stackEl, d);
      else if (typeof LAZY !== 'undefined') LAZY.need('land').then(function () { if (stackEl.isConnected) CREDITS.paint(stackEl, d); }, function () {});
      return;
    }
    if (!(d.panels || []).length) { stackEl.appendChild(el('p', 'bv-empty', 'There are no charts for ' + scopeName(id) + '.')); return; }
    view.stack = paintCostCards(stackEl, d.panels, { root: bodyEl });
  }

  W.register('costs', {
    noun: 'costs',
    url: function (id) { return chartsURL(id, range); },
    paint: paint,
    failure: failure,
    /* A chip chosen while the first answer was on its way: that answer is for the old range, so fetch the new one. */
    landed: function (id, d) { if (d && d.range && d.range !== range) setTimeout(function () { if (cur() === id) W.refresh('costs', id); }, 0); },
    /* A refresh (a chip) that failed keeps the charts shown and says so over them. */
    failed: function (id, text, retry) {
      if (!view) return;
      var n = noteLine(W.refreshFailure(text, '', 'Could not change the range'), true, retry, id);
      view.note.replaceWith(n); view.note = n;
    },
    closed: function () { stop(); view = null; }
  });

  /* The openers (step 2.3): the Menu's Costs item, its word the scope it will open, read again each time the Menu opens;
     the header purse, for an admin or a tenant a link the Workbench host opens in place. */
  var item = document.getElementById('cam-costs'), purse = document.getElementById('purse'), menuBtn = document.getElementById('cam-menu');
  var may = document.body.dataset.costs === '1', tenant = !may && document.body.dataset.cut === '1', lh = null;
  function here() { var g = window.GARDEN; return scopeOf(g && g.here ? g.here() : ''); }
  function itemWord() { var t = item.querySelector('.costs-t'); if (t) t.textContent = scopeName(here()); }
  if (may && item) {
    item.hidden = false; itemWord();
    if (menuBtn) menuBtn.addEventListener('click', itemWord);
    item.addEventListener('click', function () { W.open('costs', here()); });
  }
  /* A tenant's land (step 3.3), the one its item and purse open. */
  function tenantSay(cost) {
    var g = window.GARDEN;
    lh = tenant ? landHere(cost, g && g.here ? g.here() : '') : null;
    if (tenant && item) { item.hidden = !lh; var t = item.querySelector('.costs-t'); if (t && lh) t.textContent = lh.land; }
  }
  /* The header purse, from each farm document: a link to the fleet's panel for an admin, a tenant's to its land's. */
  function paintPurse(cost) {
    tenantSay(cost);
    if (!purse) return;
    var rows = purseRows(cost).concat(lh && typeof CREDITS !== 'undefined' ? [{ name: lh.land, credits: CREDITS.num(lh.left), left: null }] : []);
    purse.hidden = !rows.length;
    purse.textContent = '';
    if (!rows.length) return;
    var href = may && cost.scope === FLEET ? purse.dataset.href : lh ? document.body.dataset.base + '/garden?costs=' + LAND + encodeURIComponent(lh.land) : '';
    if (href) purse.setAttribute('href', href); else purse.removeAttribute('href');
    if (href) purse.setAttribute('aria-label', (lh ? 'Credits: open ' + lh.land + '’s credits.' : 'Costs: open the fleet’s panel.') + ' ' + rows.map(purseSay).join('; ')); else purse.removeAttribute('aria-label');
    rows.forEach(function (r) {
      var p = el('span', 'pu-row' + (r.stale ? ' stale' : ''));
      if (r.level) p.dataset.level = r.level;
      p.appendChild(el('span', 'pu-n', r.name));
      var f = el('span', 'pu-f'), cr = r.credits !== undefined;
      f.appendChild(el('b', cr ? 'pu-v pu-left' : 'pu-v', cr ? r.credits : r.today)); f.appendChild(document.createTextNode(cr ? ' credits left' : ' today'));
      if (r.stale) f.appendChild(el('span', 'pu-s', ' ' + r.stale));
      if (r.left !== null) {
        f.appendChild(document.createTextNode(' · '));
        f.appendChild(el('b', 'pu-v pu-left', r.left)); f.appendChild(document.createTextNode(' left'));
        if (levelWord(r.level)) f.appendChild(el('span', 'pu-lv', levelWord(r.level)));
      }
      p.appendChild(f); purse.appendChild(p);
    });
  }
  api.paintPurse = paintPurse;   // the goldens paint each state from a fixture cost
  function farmCost() { var g = window.GARDEN; return g && g.cost ? g.cost() : null; }
  paintPurse(farmCost());
  if (tenant) {
    if (typeof CREDITS === 'undefined' && typeof LAZY !== 'undefined') LAZY.need('land').then(function () { paintPurse(farmCost()); }, function () {});
    if (menuBtn) menuBtn.addEventListener('click', function () { paintPurse(farmCost()); });
    if (item) item.addEventListener('click', function () { if (lh) W.open('costs', LAND + lh.land); });
  }

  /* The farm document moved: the header's figures may have too, and the purse's. */
  document.addEventListener('farmdoc', function (e) {
    paintPurse(e.detail ? e.detail.cost : null);
    if (!view || !cur()) return;
    var h = header(view.id, e.detail, view.failed); view.head.replaceWith(h); view.head = h;
  });
  return api;
})();
if (typeof module !== 'undefined') module.exports = COSTS;
