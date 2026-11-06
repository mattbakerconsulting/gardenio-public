/* web/project.js: the project panel (workbench-glass spec WB-10, gd-fT15vJniWQ), the Workbench's project
   kind, /garden?project=<name>. It reads GET /garden/project/<name> (project.go: the viewer's own project hub sections, the
   project's open decisions and escalations as the Queues panel's rows, the land's purse, the plot's spend from the farm)
   and shows it in tabs, one at a time:
   - Agents: who is working here, in what state, on what;
   - Gardeners: its triggers and schedules, each opening its gardener panel (web/gardener.js), whether the project's config
     has them paused, and, for an operator, New gardener (POST /create-gardener asked for JSON);
   - Accounts: the LLM accounts it spends through, each one's spend today, this week and in all;
   - Cost: a tenant land's purse (credits granted, spent, in flight and left; at the wall or unknown, which stops new spawns),
     the plot's spend and budget as the map's header reads them, the accounts' spend summed (a shared key once), and the
     project's Grafana charts (GET /garden/charts/costs-project, gd-vzbd6hMBSh) under the Costs panel's range chips and
     painter (COSTS.paintCostCards, else their words and Open links). A refresh keeps the frames; a range change gives them
     new srcs. With no chart for a chip to move, or a failed load, the chips go and one line says why (gd-TGYiliW9OR);
   - Health (gd-SKVLOuGToB): GET /garden/charts/health-land with the Cost tab's chips and painter: the Lands SLOs
     fleet-wide, and on a tenant land its own API use, Commons event lag and spawn to working. Admin-only, as Cost's charts;
   - Activity: what is growing (the project's /active) and what moved last (its /feed);
   - Decisions: the open decisions and escalations, each with the Mailbox's answer box, and what is blocked on them;
   - Config: the project's config as stored, its validity findings, and, for a viewer who may, the editor (POST
     /p/<name>/config asked for JSON; a refused field says why beside it);
   - Share, for the land's owner or an operator (can_share): windows on the whole land, from the viewer's
     /p/<name>/share (land_share.go): what a link grants and withholds, the preview, Create link (shown once), Revoke. Kept
     across refreshes, so a link just shown stays.
   On an operator land (not a tenant's) an admin's header line also opens the fleet's credential pool (credential_pool;
   web/creds.js, GL-15). A tenant land's header names its owner, with an admin's Become (web/become.js).
   The tab is in the address (tab=), so a reload, a shared link and the retired /active and /feed pages (tab=activity)
   land on it; it goes when the panel closes. It opens on Decisions when something there waits, else on Agents. Opened from
   the project's sign on the map (garden.js openOf) and the retired /p/<name> hub. It refreshes in place when a farm patch
   names its rows (below) and when an answer is sent from it, never over a form being filled in. Every bead it names opens
   here. Text goes in through textContent. */
var PROJECT = (function () {
  /* ---------- pure logic (also run under node by the Go tests) ---------- */
  var TABS = [['agents', 'Agents'], ['gardeners', 'Gardeners'], ['accounts', 'Accounts'], ['cost', 'Cost'], ['health', 'Health'], ['activity', 'Activity'], ['decisions', 'Decisions'], ['config', 'Config']];
  var REFRESH_S = 20;
  var SHARE = ['share', 'Share'];
  function tabsOf(d) { return d && d.can_share ? TABS.concat([SHARE]) : TABS; }
  function isTab(t) { return t === SHARE[0] || TABS.some(function (x) { return x[0] === t; }); }
  function tabWord(t) { var x = TABS.concat([SHARE]).filter(function (x) { return x[0] === t; })[0]; return x ? x[1] : t; }
  function url(name) { return '/garden/project/' + encodeURIComponent(name); }
  /* The query with tab=<t> set, or dropped for '', every other parameter kept as written. */
  function withTab(search, tab) {
    var keep = String(search || '').replace(/^\?/, '').split('&').filter(function (p) { return p && p.split('=')[0] !== 'tab'; });
    if (tab) keep.push('tab=' + encodeURIComponent(tab));
    return keep.length ? '?' + keep.join('&') : '';
  }
  function asks(d) { return d ? (d.decisions || []).length + (d.escalations || []).length : 0; }
  /* The tab shown: the one the address names, else Decisions when something there waits, else Agents. */
  function pickTab(want, d) {
    if (isTab(want) && (want !== SHARE[0] || (d && d.can_share))) return want;
    return asks(d) ? 'decisions' : 'agents';
  }
  /* The count a tab carries in its name ('' for none). */
  function tabCount(tab, d) {
    if (!d) return '';
    var n = tab === 'agents' ? (d.agents || []).length : tab === 'gardeners' ? (d.gardeners || []).length : tab === 'accounts' ? (d.accounts || []).length :
      tab === 'activity' ? (d.growing || []).length : tab === 'decisions' ? asks(d) : 0;
    var more = d.more || {};
    if (tab === 'agents') n += more.agents || 0;
    if (tab === 'decisions') n += (more.decisions || 0) + (more.escalations || 0);
    return n ? String(n) : '';
  }
  function plural(n, one, many) { return n + ' ' + (n === 1 ? one : many); }
  /* Dollars, to the cent; a figure not known is a dash, never $0. */
  function usd(v) { return typeof v === 'number' && isFinite(v) ? '$' + v.toFixed(2) : '—'; }
  function credits(v) { return typeof v === 'number' && isFinite(v) ? (Math.round(v * 100) / 100) + ' credits' : '—'; }
  /* The line under the title: who works here, what waits, what it spends today. Its agents are the Agents tab's count, not
     the purse's running_agents. */
  function summary(d) {
    var out = [], a = (d.agents || []).length + ((d.more || {}).agents || 0), g = (d.gardeners || []).length, n = asks(d);
    out.push(plural(a, 'agent', 'agents'));
    if (g) out.push(plural(g, 'gardener', 'gardeners'));
    if (n) out.push(plural(n, 'ask waits', 'asks wait'));
    var today = d.plot && d.plot.has ? d.plot.today_usd : d.spend && d.spend.has ? d.spend.today_usd : null;
    if (typeof today === 'number') out.push(usd(today) + ' today');
    if (d.purse) out.push(purseWords(d.purse).state);
    return out.join(' · ');
  }
  /* A purse in words: {state, line}. Unknown and the wall both stop new spawns, and say so (never colour alone). */
  function purseWords(p) {
    if (!p) return { state: '', line: '' };
    if (p.state === 'wall') return { state: 'at the wall', line: 'At the wall: ' + credits(p.remaining_credits) + ' left, under its margin of ' + credits(p.margin_credits) + '. New spawns are stopped.' };
    if (p.state === 'unknown') return { state: 'purse unknown', line: 'The purse is unknown' + (p.reason ? ' (' + p.reason + ')' : '') + ', which counts as the wall: new spawns are stopped.' };
    return { state: credits(p.remaining_credits) + ' left', line: credits(p.remaining_credits) + ' left of ' + credits(p.granted_credits) + (typeof p.used_pct === 'number' ? ' (' + Math.round(p.used_pct) + '% used)' : '') + '.' };
  }
  /* A row's words. */
  function agentLine(a) {
    var out = [];
    if (a.state) out.push(String(a.state).replace(/_/g, ' '));
    if (a.role) out.push(a.role);
    if (a.ago) out.push(a.ago + ' ago');
    return out;
  }
  /* An agent that failed, set apart in the Agents tab (gd-icriMpe3wR). */
  function agentFailed(a) { return /^(failed|stuck|crashed|error|dead)$/.test(String(a && a.state || '')); }
  /* The Agents tab's order: the failed ones first, the rest as the route lists them. */
  function agentsByNeed(list) {
    var bad = [], rest = [];
    (list || []).forEach(function (a) { (agentFailed(a) ? bad : rest).push(a); });
    return bad.concat(rest);
  }
  function gardenerLine(g) {
    var out = [g.kind || 'gardener', g.enabled ? 'on' : 'off'];
    if (g.status && g.status !== 'active' && g.status !== 'disabled') out.push(g.status);
    out.push(plural(g.runs || 0, 'run', 'runs'));
    return out;
  }
  function accountLine(a) {
    var out = [];
    if (a.provider) out.push(a.provider);
    if (a.model) out.push(a.model);
    if (a.status) out.push(a.status);
    if (typeof a.today_usd === 'number') out.push(usd(a.today_usd) + ' today', usd(a.week_usd) + ' this week', usd(a.total_usd) + ' in all');
    else out.push('no spend read');
    if (a.shared) out.push('shares a key: counted once, on another account');
    return out;
  }
  var STATE_WORD = { in_progress: 'growing', open: 'planted', blocked: 'blocked', deferred: 'dormant', closed: 'closed', review: 'in review' };
  function beadLine(b) {
    var out = [];
    if (b.type && b.type !== 'task') out.push(b.type);
    if (b.status) out.push(STATE_WORD[b.status] || b.status);
    if (b.ago) out.push(b.ago + ' ago');
    return out;
  }
  /* The config's fields as the tab lists and edits them: [key, label, kind, choices]; flags are checkboxes. */
  var FIELDS = [['git_url', 'Repository'], ['default_branch', 'Default branch'], ['image', 'Agent image'], ['account', 'LLM account'], ['model', 'Model'],
    ['cpu_request', 'CPU request'], ['cpu_limit', 'CPU limit'], ['memory_request', 'Memory request'], ['memory_limit', 'Memory limit'],
    ['storage_class', 'Storage class'], ['card_theme', 'Slack card theme', 'select', ['garden', 'business']], ['budget_usd', 'Budget (US$)'],
    ['budget_period', 'Budget period', 'select', ['month', '7d']]];
  var FLAGS = [['docker', 'Docker'], ['postgres', 'Postgres'], ['needs_k8s_api', 'Kubernetes API'], ['schedules_enabled', 'Schedules'], ['triggers_enabled', 'Triggers']];
  /* The config as label and value rows; a blank value says what it falls back to. */
  function configRows(c) {
    var rows = FIELDS.map(function (f) {
      var v = c[f[0]] || '';
      if (!v) v = f[0] === 'card_theme' ? 'garden' : f[0] === 'budget_usd' ? 'no budget' : f[0] === 'budget_period' ? (c.budget_usd ? 'month' : '') : 'inherited';
      return [f[1], v];
    }).filter(function (r) { return r[1] !== ''; });
    rows.push(['Turned on', FLAGS.filter(function (f) { return c[f[0]]; }).map(function (f) { return f[1]; }).join(', ') || 'none']);
    return rows;
  }
  /* The editor's form as POST /p/<name>/config reads it: every field, a checkbox only when on, the add-secret trio only
     when one of it is typed. */
  function formParams(v) {
    var out = [];
    FIELDS.forEach(function (f) { out.push([f[0], String(v[f[0]] == null ? '' : v[f[0]]).trim()]); });
    FLAGS.forEach(function (f) { if (v[f[0]]) out.push([f[0], 'on']); });
    var s = ['add_secret_env', 'add_secret_secret', 'add_secret_key'];
    if (s.some(function (k) { return String(v[k] || '').trim(); })) s.forEach(function (k) { out.push([k, String(v[k] || '').trim()]); });
    return out;
  }
  /* A new gardener's form as POST /create-gardener reads it (WB-11): the project, the epic it drives, its
     prompt and preset, and the goal and schedule when typed (blank, the preset seeds them). createProblem says what is
     missing first, '' when it may go. */
  var CREATE = [['epic_id', 'Epic it drives'], ['prompt', 'Prompt'], ['terminal_goal', 'Terminal goal (blank: the preset’s)'], ['cron', 'Schedule, cron (blank: the preset’s)']];
  function createParams(project, v) {
    var out = [['project', project], ['preset', String(v.preset || '')]];
    CREATE.forEach(function (f) { var x = String(v[f[0]] || '').trim(); if (x || f[0] === 'epic_id' || f[0] === 'prompt') out.push([f[0], x]); });
    return out;
  }
  function createProblem(v) {
    if (!String(v.epic_id || '').trim()) return 'Name the epic it drives.';
    if (!String(v.prompt || '').trim()) return 'Say what it should do: the prompt every run is sent.';
    return '';
  }
  /* The charts under the Cost and Health tabs (gardenio/docs/garden-grafana-panels.md): each tab's set on the dark theme, as
     the panel's dark box asks in both skins, over one of the ranges; the range the viewer chose last is the Costs panel's too. */
  var RANGES = [['today', 'Today'], ['7d', '7 days'], ['30d', '30 days']], RANGE_KEY = 'garden.costs.range';
  var CHARTS = { cost: { set: 'costs-project', word: 'costs' }, health: { set: 'health-land', word: 'health' } };
  function isRange(r) { return RANGES.some(function (x) { return x[0] === r; }); }
  function pickRange(stored) { return isRange(stored) ? stored : '7d'; }
  function chartsOf(tab) { return CHARTS[tab] || CHARTS.cost; }
  function chartsUrl(name, range, tab) {
    return '/garden/charts/' + chartsOf(tab).set + '?project=' + encodeURIComponent(name) + '&range=' + encodeURIComponent(pickRange(range)) + '&theme=dark';
  }
  /* The words for a tab with no chart for this project. */
  function chartsNone(tab) { return 'The ' + chartsOf(tab).word + ' dashboard has no charts for this project.'; }
  /* Why the charts did not come, in words, for the route's status (0: no answer). */
  function chartsFail(status, tab) {
    var w = chartsOf(tab).word;
    if (status === 403) return w.charAt(0).toUpperCase() + w.slice(1) + ' charts are for operators for now.';
    if (status === 400) return chartsNone(tab);
    if (status === 401 || status === 'redirect') return 'Your session ended. Reload the page to sign in again.';
    return 'Could not load the charts' + (status ? ' (' + status + ')' : ': the garden did not answer') + '.';
  }
  /* Whether trying again could help: not for a refusal, a project the route does not chart, or an ended session. */
  function chartsRetry(status) { return status !== 400 && status !== 401 && status !== 403 && status !== 'redirect'; }
  /* Grafana's own front door (its base, a sub-path kept), from the dashboard's Open link <base>/d/<uid>?...: where "Open
     Grafana once to sign in" goes. */
  function windowWords(w) {
    return [w.state, plural(w.view_count || 0, 'view', 'views'), w.last_viewed_at ? 'last viewed ' + String(w.last_viewed_at).slice(0, 10) : 'never viewed',
      'expires ' + String(w.expires_at || '').slice(0, 10), w.created_by || ''].filter(Boolean);
  }
  function withheldWords(p) {
    var w = p.withheld || [];
    return w.length ? plural(w.length, 'field is', 'fields are') + ' withheld for an internal reference, and the page reads “' + p.withheld_text + '” in its place: ' +
      w.map(function (x) { return x.path; }).join(', ') + '.' : 'Nothing is withheld: every field passes the window’s checks.';
  }
  function ownerWords(o) { var n = o ? String(o.name || String(o.principal || '').replace(/^oidc:/, '')).trim() : ''; return n ? 'Owner: ' + n : ''; }
  function grafanaHome(openURL) {
    var m = /^(https?:\/\/[^?#]*?)\/d\/[^\/?#]+(\?|#|$)/.exec(String(openURL || ''));
    return m ? m[1] + '/' : '';
  }
  /* Did a farm patch move this project (gd-HprlA40a7z)? plans.js swaps only the rows a patch names, so a plan, seed or
     gardener row of its own that is not the one held moved (a gardener parked files an ask; its cost moves the spend), as
     does its meadow, closed plans or cost row (the header's spend, the Cost tab, a purse). */
  function rowsOf(doc, name) {
    var o = {}, of = function (r) { return r.project === name; };
    ['plans', 'seeds', 'gardeners'].forEach(function (k) { (doc[k] || []).filter(of).forEach(function (r) { o[k + ' ' + r.id] = r; }); });
    o[''] = JSON.stringify([doc.meadow, doc.closed_plans].map(function (b) { return ((b || {}).shown || []).filter(of); }).concat([((doc.cost || {}).projects || []).filter(of)]));
    return o;
  }
  function moved(was, doc, name) {
    var a = was ? rowsOf(was, name) : {}, b = rowsOf(doc, name);
    return Object.keys(a).concat(Object.keys(b)).some(function (k) { return a[k] !== b[k]; });
  }
  var api = { TABS: TABS, tabsOf: tabsOf, windowWords: windowWords, withheldWords: withheldWords, isTab: isTab, tabWord: tabWord, url: url, withTab: withTab, pickTab: pickTab, tabCount: tabCount, usd: usd, credits: credits, summary: summary,
    purseWords: purseWords, agentLine: agentLine, agentFailed: agentFailed, agentsByNeed: agentsByNeed, gardenerLine: gardenerLine, accountLine: accountLine, beadLine: beadLine, configRows: configRows, formParams: formParams,
    FIELDS: FIELDS, FLAGS: FLAGS, REFRESH_S: REFRESH_S, CREATE: CREATE, createParams: createParams, createProblem: createProblem,
    RANGES: RANGES, CHARTS: CHARTS, pickRange: pickRange, chartsUrl: chartsUrl, chartsNone: chartsNone, chartsFail: chartsFail, chartsRetry: chartsRetry, grafanaHome: grafanaHome, ownerWords: ownerWords, moved: moved };
  if (typeof document === 'undefined' || typeof WORKBENCH === 'undefined') return api;

  /* ---------- DOM ---------- */
  var W = WORKBENCH, B = document.body, base = B.dataset.base || '';
  var titleEl = document.getElementById('bv-title'), kindEl = document.getElementById('bv-kind'), bodyEl = document.getElementById('bv-body');
  if (!bodyEl) return api;
  var last = null, lastAt = 0, noteEl = null, sending = false, tab = '', charts = null, share = null, shareDays = 30;

  function el(t, cls, text) { var e = document.createElement(t); if (cls) e.className = cls; if (text !== undefined) e.textContent = text; return e; }
  function cur() { var c = W.current(); return c && c.kind === 'project' ? c.id : ''; }
  function href(id) { return base + '/garden?bead=' + encodeURIComponent(id); }
  function say(msg, err) { if (!noteEl) return; noteEl.textContent = msg; noteEl.className = 'bv-note' + (err ? ' err' : ''); }
  function setTab(t) {
    tab = t;
    try { if (history.replaceState) history.replaceState(history.state, '', location.pathname + withTab(location.search, t) + location.hash); } catch (e) { /* a sandboxed frame */ }
  }
  function beadA(id, title, cls) {
    var a = el('a', cls || 'bl qv-t', title || id); a.href = href(id); a.title = title && title !== id ? title + ' (' + id + ')' : id;
    if (title && typeof TITLES !== 'undefined') TITLES.learn(id, title);
    return a;
  }
  function words(p, list, gold) {
    list.forEach(function (w, i) { if (i) p.appendChild(document.createTextNode(' · ')); p.appendChild(i === 0 && gold ? el('b', '', w) : document.createTextNode(w)); });
    return p;
  }
  function row(cls) { var li = el('li', 'qv-row' + (cls ? ' ' + cls : '')); return li; }
  function list(label, items, make, emptyText) {
    var box = el('div');
    if (!items.length) { box.appendChild(el('p', 'bv-empty', emptyText)); return box; }
    var ul = el('ul', 'qv-list'); ul.setAttribute('aria-label', label);
    items.forEach(function (x) { ul.appendChild(make(x)); });
    box.appendChild(BEAD.foldList(ul));
    return box;
  }
  function sec(title, node) { var s = el('section', 'bv-sec pj-sec'); s.appendChild(el('h3', '', title)); s.appendChild(node); return s; }
  function moreNote(n, what) { return n ? el('p', 'bv-note', '+' + n + ' more ' + what + ' not listed.') : null; }
  function partial(d, what) { return (d.partial || []).indexOf(what) >= 0; }
  function failNote(text) {
    var n = el('p', 'bv-note err', text + ' ');
    n.setAttribute('role', 'status');
    var b = el('button', 'btn small bv-retry', 'Try again'); b.type = 'button';
    b.onclick = function () { b.disabled = true; b.textContent = 'Loading…'; W.refresh('project', cur()); };
    n.appendChild(b);
    return n;
  }
  function stats(rows) {
    var dl = el('dl', 'bv-fields');
    rows.forEach(function (r) { dl.appendChild(el('dt', '', r[0])); dl.appendChild(el('dd', '', r[1])); });
    return dl;
  }

  /* ---------- the tabs ---------- */
  function tabsRow(d) {
    var r = el('div', 'qv-tabs pj-tabs');
    r.setAttribute('role', 'tablist'); r.setAttribute('aria-label', 'Project');
    var tabs = tabsOf(d);
    tabs.forEach(function (t) {
      var n = tabCount(t[0], d), b = el('button', 'btn small' + (t[0] === tab ? '' : ' ghost'), t[1] + (n ? ' ' + n : ''));
      b.type = 'button'; b.id = 'pj-tab-' + t[0]; b.setAttribute('role', 'tab'); b.setAttribute('aria-selected', t[0] === tab ? 'true' : 'false');
      b.setAttribute('aria-controls', 'pj-pane'); b.tabIndex = t[0] === tab ? 0 : -1;
      b.onclick = function () { show(t[0]); };
      r.appendChild(b);
    });
    /* left and right walk the tabs, Home and End go to the ends, as a tablist does; the map's cursor keys (garden.js) never
       see them, so Home here is not the map's home cell */
    r.addEventListener('keydown', function (e) {
      var i = tabs.map(function (t) { return t[0]; }).indexOf(tab), n = tabs.length, j = -1;
      if (e.key === 'ArrowRight') j = (i + 1) % n; else if (e.key === 'ArrowLeft') j = (i + n - 1) % n; else if (e.key === 'Home') j = 0; else if (e.key === 'End') j = n - 1;
      if (j < 0) return;
      e.preventDefault(); e.stopPropagation();
      show(tabs[j][0]);
    });
    return r;
  }
  /* the chosen tab in view in a sideways row (gd-SXTYMHfxW6; scrollLeft clamps); gardener.js's too */
  function seeTab(r) { var on = r.querySelector('[aria-selected=true]'); if (on) r.scrollLeft = on.getBoundingClientRect().right - r.getBoundingClientRect().right + 8; }
  api.seeTab = seeTab;
  function show(t) {
    if (t === tab || !last) return;
    if (hold() && !window.confirm('Leave the form you are filling in?')) return;
    if (t === SHARE[0]) share = null;   // showing the tab reads it afresh
    setTab(t);
    paint(cur(), last, '');
    var nb = document.getElementById('pj-tab-' + t); if (nb) nb.focus();   // the repaint took the old button, and focus with it
  }

  function agentsPane(d) {
    if (partial(d, 'agents')) return failNote('Could not read its agents: the beads daemon did not answer.');
    var box = list('Agents', agentsByNeed(d.agents), function (a) {
      var li = row(agentFailed(a) ? 'pj-failed' : '');
      li.appendChild(beadA(a.id, a.name || a.id));
      /* its state a chip coloured as the Porch's (failed red, working green), in words as well as colour */
      var ln = el('p', 'qv-line'), w = agentLine(a);
      if (a.state) {
        ln.appendChild(el('span', 'chip pj-st st-' + String(a.state).replace(/[^a-z_-]/gi, ''), w.shift()));
        if (w.length) ln.appendChild(document.createTextNode(' · '));
      }
      li.appendChild(words(ln, w));
      if (a.task_id) { var p = el('p', 'qv-origin'), ta = beadA(a.task_id, a.task_title || a.task_id, 'bl'); p.appendChild(document.createTextNode('on ')); p.appendChild(ta); if (typeof TITLES !== 'undefined') TITLES.unseen(ta, a.task_id, a.task_title); li.appendChild(p); }
      return li;
    }, 'No agent is working in this project.');
    var m = moreNote((d.more || {}).agents, 'agents'); if (m) box.appendChild(m);
    if (W.has('transcript') && (d.agents || []).length) box.appendChild(el('p', 'qv-origin')).appendChild(el('a', 'bl', 'Read every call they made, newest first')).href = W.href('transcript', 'project:' + cur());   // its gardeners' calls, merged
    return box;
  }
  function gardenersPane(d) {
    var box = el('div');
    if (d.schedules_paused || d.triggers_paused) {
      var off = [d.schedules_paused ? 'schedules' : '', d.triggers_paused ? 'triggers' : ''].filter(Boolean).join(' and ');
      var n = el('p', 'bv-note pj-paused', 'Paused: the project’s config turns its ' + off + ' off, so they do not run.');
      n.setAttribute('role', 'note'); box.appendChild(n);
    }
    if (partial(d, 'gardeners')) { box.appendChild(failNote('Could not read its gardeners: the beads daemon did not answer.')); return box; }
    box.appendChild(list('Gardeners', d.gardeners || [], function (g) {
      var li = row();
      if (W.has('gardener')) {   // its gardener panel (web/gardener.js): runs, levers, config
        var a = el('a', 'bl qv-t', g.name || g.id); a.href = base + '/garden' + W.withPanel('', 'gardener', g.id); a.title = g.id; li.appendChild(a);
      } else li.appendChild(beadA(g.id, g.name || g.id));
      li.appendChild(words(el('p', 'qv-line'), gardenerLine(g)));
      if (g.goal) li.appendChild(el('p', 'bv-text qv-text', g.goal));
      return li;
    }, 'No gardener tends this project.'));
    if (d.can_create_gardener && (d.gardener_presets || []).length) {
      var add = el('button', 'btn small', 'New gardener'); add.type = 'button';
      add.onclick = function () { add.hidden = true; box.appendChild(creator(d, function () { add.hidden = false; })).querySelector('select').focus(); };
      box.appendChild(add);
    }
    return box;
  }
  /* "New gardener": a preset (what it is for), the epic it drives and its prompt; Create posts POST /create-gardener asked for
     JSON and opens the new gardener's panel. A refusal says why and keeps what was typed. */
  function creator(d, done) {
    var form = el('form', 'bw-edit pj-form pj-create'), ins = {}, ps = d.gardener_presets;
    form.setAttribute('aria-label', 'New gardener in ' + d.name);
    var sel = el('select', 'bw-in'); sel.name = 'preset';
    ps.forEach(function (p) { var o = el('option', '', p.label); o.value = p.key; sel.appendChild(o); });
    var hint = el('span', 'bw-h', ps[0].description || '');
    sel.onchange = function () { var p = ps.filter(function (x) { return x.key === sel.value; })[0]; hint.textContent = p ? p.description || '' : ''; };
    var pf = field('What it is for', sel); pf.appendChild(hint); form.appendChild(pf); ins.preset = sel;
    CREATE.forEach(function (f) {
      var i = f[0] === 'prompt' || f[0] === 'terminal_goal' ? el('textarea', 'bw-in') : el('input', 'bw-in');
      if (i.tagName === 'TEXTAREA') i.rows = 3; else { i.autocomplete = 'off'; i.spellcheck = false; }
      i.name = f[0]; i.maxLength = 8192; ins[f[0]] = i;
      form.appendChild(field(f[1], i));
    });
    var acts = el('div', 'bv-acts'), save = el('button', 'btn small', 'Create'), no = el('button', 'btn small ghost', 'Cancel'), st = el('p', 'bw-h');
    save.type = 'submit'; no.type = 'button'; st.setAttribute('role', 'status');
    no.onclick = function () { form.remove(); done(); };
    acts.appendChild(save); acts.appendChild(no); form.appendChild(acts); form.appendChild(st);
    form.onsubmit = function (e) {
      e.preventDefault();
      var vals = {}; Object.keys(ins).forEach(function (k) { vals[k] = ins[k].value; });
      var bad = createProblem(vals);
      if (bad) { st.textContent = bad; st.className = 'bw-h bw-err'; (vals.epic_id.trim() ? ins.prompt : ins.epic_id).focus(); return; }
      save.disabled = true; no.disabled = true; sending = true; st.textContent = 'Creating…'; st.className = 'bw-h';
      postForm('/create-gardener', createParams(d.name, vals)).then(function (j) {
        sending = false; form.remove(); done();
        if (j.bead && W.has('gardener')) W.open('gardener', j.bead); else { say('Created the gardener.'); W.refresh('project', cur()); }
      }, function (err) {
        sending = false; save.disabled = false; no.disabled = false; st.textContent = err.message; st.className = 'bw-h bw-err';
      });
    };
    return form;
  }
  /* A POST to one of the viewer's routes asked for JSON; a refusal carries the route's message. */
  function postForm(path, params) {
    var body = new URLSearchParams(); params.forEach(function (p) { body.append(p[0], p[1]); });
    return fetch(base + path, { method: 'POST', credentials: 'same-origin', redirect: 'manual',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded', Accept: 'application/json' }, body: body.toString() }).then(function (r) {
      if (r.type === 'opaqueredirect' || r.status === 401) throw new Error('Your session ended. Reload the page to sign in again.');
      return r.json().catch(function () { return null; }).then(function (j) {
        if (j && j.ok) return j;
        throw new Error((j && j.error) || (r.status === 403 ? 'Only an operator may create a gardener.' : 'That did not go through (' + r.status + ').'));
      });
    }, function () { throw new Error('The garden did not answer. Try again.'); });
  }
  function accountsPane(d) {
    if (partial(d, 'accounts')) return failNote('Could not read its accounts: the beads daemon did not answer.');
    return list('Accounts', d.accounts || [], function (a) {
      var li = row();
      li.appendChild(el('span', 'qv-t', a.name));
      li.appendChild(words(el('p', 'qv-line'), accountLine(a)));
      return li;
    }, 'No LLM account is authorised for this project.');
  }
  function costPane(d) {
    var box = el('div');
    if (d.purse) {
      var p = d.purse, pw = purseWords(p), s = el('div', 'pj-purse' + (p.state === 'ok' ? '' : ' wall'));
      var head = el('p', 'pj-big', pw.line); head.setAttribute('role', p.state === 'ok' ? 'note' : 'alert'); s.appendChild(head);
      if (p.state !== 'unknown') {
        var rows = [['Granted', credits(p.granted_credits)], ['Spent', credits(p.spent_credits)], ['In flight', credits(p.in_flight_credits) + (p.running_agents ? ' held for ' + plural(p.running_agents, 'running agent', 'running agents') : '')],
          ['Left', credits(p.remaining_credits)], ['Margin', credits(p.margin_credits)]];
        if (typeof p.remaining_usd === 'number') rows.push(['At cost', usd(p.spent_usd) + ' spent, ' + usd(p.remaining_usd) + ' left']);
        if (p.rate_version) rows.push(['Rate', 'v' + p.rate_version + (p.llm_multiplier ? ', ×' + p.llm_multiplier + ' on LLM spend' : '')]);
        s.appendChild(stats(rows));
      }
      box.appendChild(sec('Purse', s));
    }
    var pl = d.plot;
    if (pl) {
      var r2 = [['Today', usd(pl.today_usd)], ['This week', usd(pl.week_usd)]];
      if (pl.has_budget) r2.push(['Budget', usd(pl.budget_usd) + (pl.period === '7d' ? ' a rolling week' : ' a month')], ['Spent of it', usd(pl.spent_usd)], ['Left of it', usd(pl.remaining_usd)]);
      else r2.push(['Budget', 'none set']);
      var pb = el('div'); pb.appendChild(stats(r2));
      if (!pl.has) pb.appendChild(el('p', 'bv-note', 'The costs rollup did not answer' + (pl.as_of ? '; these are the figures from ' + new Date(pl.as_of * 1000).toISOString().slice(11, 16) + ' UTC' : '') + '.'));
      box.appendChild(sec('Spend', pb));
    }
    if (partial(d, 'accounts')) box.appendChild(failNote('Could not read its accounts’ spend: the beads daemon did not answer.'));
    else if (d.spend) {
      var sp = d.spend, ab = el('div');
      ab.appendChild(stats([['Today', usd(sp.today_usd)], ['This week', usd(sp.week_usd)], ['In all', usd(sp.total_usd)]]));
      if (!sp.has) ab.appendChild(el('p', 'bv-note', 'Its accounts have no spend read yet.'));
      box.appendChild(sec('Its accounts', ab));
    }
    if (!box.children.length) box.appendChild(el('p', 'bv-empty', 'No spend is read for this project.'));
    return box;
  }
  /* The Health tab: its charts in words, over them. The SLOs are the fleet's; the land's own cards only a tenant land's. */
  function healthPane(d) {
    var box = el('div');
    box.appendChild(el('p', 'qv-line', 'The Lands SLOs, for the whole fleet: how fast each spends its error budget, what is left of it this month, and the off-cluster uptime probes.'));
    box.appendChild(el('p', 'qv-line', d.purse ? 'Under them, this land’s own use of the Lands API, its Commons event lag and how long its agents take to start working.'
      : 'This is not a tenant land, so its own Lands API charts are not shown.'));
    return box;
  }
  /* A tab's charts: a section painted once per showing of the tab, which paint() keeps in place across refreshes.
     charts is its state: the tab, the project, the range, the section and its card stack, and the fetch in flight (seq). */
  function storedRange() { try { return pickRange(localStorage.getItem(RANGE_KEY)); } catch (e) { return '7d'; } }
  function chartsSec(name, t) {
    var st = { tab: t, project: name, range: storedRange(), seq: 0 };
    var s = el('section', 'bv-sec pj-sec pj-charts'); s.setAttribute('aria-label', 'Charts');
    s.appendChild(el('h3', '', 'Charts'));
    var chips = el('div', 'qv-scope pj-chips'); chips.setAttribute('role', 'radiogroup'); chips.setAttribute('aria-label', 'Chart range');
    RANGES.forEach(function (r) {
      var b = el('button', 'btn small', r[1]); b.type = 'button'; b.setAttribute('role', 'radio'); b.dataset.range = r[0];
      b.onclick = function () { pick(st, r[0]); };
      chips.appendChild(b);
    });
    /* left and right walk the ranges, as a radio group does; the map's cursor keys (garden.js) never see them */
    chips.addEventListener('keydown', function (e) {
      var i = RANGES.map(function (r) { return r[0]; }).indexOf(st.range), n = RANGES.length, j = -1;
      if (e.key === 'ArrowRight' || e.key === 'ArrowDown') j = (i + 1) % n; else if (e.key === 'ArrowLeft' || e.key === 'ArrowUp') j = (i + n - 1) % n;
      if (j < 0) return;
      e.preventDefault(); e.stopPropagation();
      pick(st, RANGES[j][0]);
      chips.children[j].focus();
    });
    s.appendChild(chips);
    st.off = el('p', 'bv-note pj-off'); st.off.hidden = true; s.appendChild(st.off);
    st.cards = el('div', 'pj-cards'); s.appendChild(st.cards);
    st.foot = el('div', 'pj-chartfoot'); s.appendChild(st.foot);
    st.node = s; st.chips = chips;
    charts = st;
    loadCharts(st);
    return s;
  }
  function pick(st, range) {
    if (range === st.range) return;
    st.range = range;
    try { localStorage.setItem(RANGE_KEY, range); } catch (e) { /* a blocked store keeps it for this showing only */ }
    loadCharts(st);
  }
  function loadCharts(st) {
    var my = ++st.seq;
    Array.prototype.forEach.call(st.chips.children, function (b) {
      var on = b.dataset.range === st.range;
      b.setAttribute('aria-checked', on ? 'true' : 'false'); b.tabIndex = on ? 0 : -1;
    });
    if (!st.cards.children.length) st.cards.appendChild(el('p', 'bv-note', 'Loading the charts…'));
    var fail = function (status) { var e = new Error(chartsFail(status, st.tab)); e.retry = chartsRetry(status); throw e; };
    fetch(base + chartsUrl(st.project, st.range, st.tab), { credentials: 'same-origin', redirect: 'manual', cache: 'no-store', headers: { Accept: 'application/json' } })
      .then(function (r) {
        if (r.ok) return r.json();
        fail(r.type === 'opaqueredirect' ? 'redirect' : r.status);
      }, function () { fail(0); })
      .then(function (res) {
        if (charts !== st || my !== st.seq) return;   // the tab or the range moved on
        paintCharts(st, res);
      }, function (err) {
        if (charts !== st || my !== st.seq) return;
        stopCharts(st); st.cards.textContent = ''; st.foot.textContent = '';
        rangeOff(st, null);
        var n = el('p', 'bv-note', err.message); n.setAttribute('role', 'status'); st.cards.appendChild(n);
        if (err.retry) {
          var b = el('button', 'btn small bv-retry', 'Try again'); b.type = 'button';
          b.onclick = function () { b.disabled = true; b.textContent = 'Loading…'; loadCharts(st); };
          n.appendChild(b);
        }
      });
  }
  /* The chips only move the cards' srcs: with no answer they go (its note says why), and with no card that has a chart
     they go and one line says why (COSTS.rangeOff, gd-TGYiliW9OR). */
  function rangeOff(st, res) {
    var why = res && typeof COSTS !== 'undefined' && COSTS.rangeOff ? COSTS.rangeOff(res) : '';
    st.chips.hidden = !res || !!why;
    st.off.textContent = why; st.off.hidden = !why;
  }
  function paintCharts(st, res) {
    var cards = (res && res.panels) || [];
    rangeOff(st, res);
    if (!cards.length) { stopCharts(st); st.cards.textContent = ''; st.cards.appendChild(el('p', 'bv-empty', chartsNone(st.tab))); }
    else if (typeof COSTS !== 'undefined' && COSTS.paintCostCards) {   // the same cards take the new range's srcs
      if (!(st.stack && st.stack.update(cards))) { stopCharts(st); st.stack = COSTS.paintCostCards(st.cards, cards, { root: bodyEl, idPrefix: 'pj-gc', heading: 'h4' }); }
    } else { st.cards.textContent = ''; cardWords(st.cards, cards); }
    st.foot.textContent = '';
    var dash = res && res.open_dashboard_url, home = grafanaHome(dash);
    if (dash) {
      var p = el('p', 'qv-origin'), a = el('a', 'bl ext', 'Open the dashboard in Grafana'); a.href = dash; a.target = '_blank'; a.rel = 'noopener';
      p.appendChild(a); st.foot.appendChild(p);
    }
    if (home) {
      var q = el('p', 'bv-note', 'Charts blank? '), b = el('a', 'bl ext', 'Open Grafana once to sign in'); b.href = home; b.target = '_blank'; b.rel = 'noopener';
      q.appendChild(b); st.foot.appendChild(q);
    }
  }
  /* The painter's lazy-load observer and slow-chart timers go with the cards they watch. */
  function stopCharts(st) { if (st && st.stack) { st.stack.stop(); st.stack = null; } }
  /* A card as its words and its Open link, no frame: what the tab shows until the Costs panel's painter is on the page. */
  function cardWords(box, cards) {
    var unset = false;
    cards.forEach(function (c) {
      var s = el('section', 'pj-card'), h = el('h4', 'pj-ct', c.title || 'Chart');
      h.id = 'pj-card-' + c.id; s.setAttribute('aria-labelledby', h.id); s.appendChild(h);
      if (c.scope) s.appendChild(el('p', 'qv-line', c.scope));
      if (c.note) s.appendChild(el('p', 'bv-text qv-text', c.note));
      if (c.open_url) {
        var a = el('a', 'bl ext', 'Open'); a.href = c.open_url; a.target = '_blank'; a.rel = 'noopener'; a.setAttribute('aria-label', 'Open ' + (c.title || 'the chart') + ' in Grafana');
        var p = el('p', 'qv-origin'); p.appendChild(a); s.appendChild(p);
      } else unset = true;
      box.appendChild(s);
    });
    if (unset) box.appendChild(el('p', 'bv-note', 'Grafana isn’t set up in this environment.'));
  }
  /* box's descendants all go but the branch that holds node, so node stays in the document (a frame moved is reloaded). */
  function clearBut(box, node) {
    Array.prototype.slice.call(box.childNodes).forEach(function (c) {
      if (c === node) return;
      if (c.contains(node)) clearBut(c, node); else box.removeChild(c);
    });
  }
  function beadRows(label, items, emptyText) {
    return list(label, items, function (b) {
      var li = row(b.status === 'blocked' ? 'needs' : '');
      li.appendChild(beadA(b.id, b.title || b.id));
      li.appendChild(words(el('p', 'qv-line'), beadLine(b)));
      return li;
    }, emptyText);
  }
  function activityPane(d) {
    var box = el('div');
    box.appendChild(sec('Growing now', partial(d, 'growing') ? failNote('Could not read what is growing: the beads daemon did not answer.') : beadRows('Growing now', d.growing || [], 'Nothing is in progress here.')));
    box.appendChild(sec('Moved last', partial(d, 'recent') ? failNote('Could not read what moved: the beads daemon did not answer.') : beadRows('Moved last', d.recent || [], 'Nothing has moved here yet.')));
    return box;
  }
  function askRows(label, rows, d, emptyText) {
    if (!rows.length) return el('p', 'bv-empty', emptyText);
    var ul = el('ul', 'qv-list'), now = Math.floor(Date.now() / 1000); ul.setAttribute('aria-label', label);
    rows.forEach(function (r) {
      if (typeof QUEUE !== 'undefined' && QUEUE.row) { ul.appendChild(QUEUE.row(r, { approve: d.approve }, now)); return; }   // the Queues panel's row and its answer box
      var li = row(); li.appendChild(beadA(r.id, r.title || r.id)); ul.appendChild(li);
    });
    return ul;
  }
  function decisionsPane(d) {
    var box = el('div'), more = d.more || {};
    box.appendChild(sec('Decisions', partial(d, 'decisions') ? failNote('Could not read its decisions: the beads daemon did not answer.') : BEAD.foldList(askRows('Decisions', d.decisions || [], d, 'No decision is open in this project.'))));
    var m = moreNote(more.decisions, 'decisions'); if (m) box.appendChild(m);
    box.appendChild(sec('Escalations', partial(d, 'escalations') ? failNote('Could not read its escalations: the beads daemon did not answer.') : BEAD.foldList(askRows('Escalations', d.escalations || [], d, 'No escalation is open in this project.'))));
    m = moreNote(more.escalations, 'escalations'); if (m) box.appendChild(m);
    if (partial(d, 'blocked')) box.appendChild(sec('Blocked', failNote('Could not read what is blocked: the beads daemon did not answer.')));
    else if ((d.blocked || []).length) box.appendChild(sec('Blocked', beadRows('Blocked', d.blocked, '')));
    return box;
  }

  /* ---------- config ---------- */
  function field(label, input, err) {
    var f = el('label', 'bw-f'); f.appendChild(el('span', 'bw-l', label)); f.appendChild(input);
    if (err) { var e = el('span', 'bw-err', err); e.setAttribute('role', 'alert'); f.appendChild(e); input.setAttribute('aria-invalid', 'true'); }
    return f;
  }
  function configPane(d) {
    var box = el('div'), c = d.config;
    if (partial(d, 'config')) return failNote('Could not read its config: the beads daemon did not answer.');
    if (!c) { box.appendChild(el('p', 'bv-empty', 'This project has no config bead: it is a label only. One is made with kd create --type=project.')); return box; }
    var fs = c.findings || [];
    if (fs.length) {
      var ul = el('ul', 'qv-list pj-findings'); ul.setAttribute('aria-label', 'Config findings');
      fs.forEach(function (f) { var li = row(f.severity === 'error' ? 'needs' : ''); words(li.appendChild(el('p', 'qv-line')), [f.severity === 'error' ? 'error' : 'warning', f.field || 'config']); li.appendChild(el('p', 'bv-text', f.message)); ul.appendChild(li); });
      box.appendChild(sec('Findings', ul));
    }
    var v = el('div');
    v.appendChild(stats(configRows(c)));
    if ((c.secrets || []).length) v.appendChild(stats(c.secrets.map(function (s) { return [s.env, s.secret + ' / ' + s.key]; })));
    var p = el('p', 'qv-origin'); p.appendChild(document.createTextNode('Stored on ')); p.appendChild(beadA(c.bead_id, 'its config bead', 'bl')); v.appendChild(p);
    var cs = box.appendChild(sec('Config', v));
    if (d.can_edit) {
      var edit = el('button', 'btn small', 'Edit the config'); edit.type = 'button';
      edit.onclick = function () { edit.hidden = true; v.hidden = true; box.appendChild(editor(d, c, {}, function () { edit.hidden = false; v.hidden = false; })).querySelector('input').focus(); };
      var hd = el('div', 'pj-sh');   // beside its heading, not under the rows: a phone reaches it unscrolled (gd-kUzzLSjtUc)
      cs.insertBefore(hd, cs.firstChild); hd.appendChild(hd.nextSibling); hd.appendChild(edit);
    }
    return box;
  }
  /* The editor: every field as stored, Save posts them all (the route PATCHes only what it manages), a refusal keeps what
     was typed and marks the fields it names. */
  function editor(d, c, errs, done) {
    var form = el('form', 'bw-edit pj-form'), ins = {};
    form.setAttribute('aria-label', 'Edit the config of ' + d.name);
    FIELDS.forEach(function (f) {
      var i;
      if (f[2] === 'select') {
        i = el('select', 'bw-in');
        f[3].forEach(function (o) { var op = el('option', '', o); op.value = o; i.appendChild(op); });
        i.value = c[f[0]] || f[3][0];
      } else { i = el('input', 'bw-in'); i.value = c[f[0]] || ''; i.maxLength = 2000; if (f[0] === 'budget_usd') i.inputMode = 'decimal'; }
      i.name = f[0]; ins[f[0]] = i;
      form.appendChild(field(f[1], i, errs[f[0]]));
    });
    var flags = el('fieldset', 'pj-flags'); flags.appendChild(el('legend', 'bw-l', 'Turned on'));
    FLAGS.forEach(function (f) {
      var l = el('label', 'pj-flag'), i = el('input'); i.type = 'checkbox'; i.name = f[0]; i.checked = !!c[f[0]]; i.defaultChecked = i.checked; ins[f[0]] = i;
      l.appendChild(i); l.appendChild(document.createTextNode(' ' + f[1])); flags.appendChild(l);
    });
    form.appendChild(flags);
    var sr = el('div', 'bw-row'); form.appendChild(el('p', 'bw-l', 'Add a secret'));
    [['add_secret_env', 'Env var'], ['add_secret_secret', 'Kubernetes Secret'], ['add_secret_key', 'Key']].forEach(function (f) {
      var i = el('input', 'bw-in'); i.name = f[0]; i.value = c[f[0]] || ''; i.maxLength = 253; i.autocomplete = 'off'; ins[f[0]] = i; sr.appendChild(field(f[1], i, errs[f[0]]));
    });
    form.appendChild(sr);
    if (errs.secrets) form.appendChild(el('p', 'bw-err', errs.secrets));
    var acts = el('div', 'bv-acts'), save = el('button', 'btn small', 'Save'), no = el('button', 'btn small ghost', 'Cancel'), st = el('p', 'bw-h');
    save.type = 'submit'; no.type = 'button'; st.setAttribute('role', 'status');
    no.onclick = function () { form.remove(); done(); };
    acts.appendChild(save); acts.appendChild(no); form.appendChild(acts); form.appendChild(st);
    form.onsubmit = function (e) {
      e.preventDefault();
      var vals = {};
      Object.keys(ins).forEach(function (k) { vals[k] = ins[k].type === 'checkbox' ? ins[k].checked : ins[k].value; });
      save.disabled = true; no.disabled = true; sending = true; st.textContent = 'Saving…';
      postConfig(d.name, formParams(vals)).then(function () {
        sending = false; form.remove(); say('Saved the config.'); W.refresh('project', cur());
      }, function (err) {
        sending = false; save.disabled = false; no.disabled = false;
        var f = err.fields || {}, again = editor(d, vals, f, done);
        form.replaceWith(again);
        var st2 = again.querySelector('.bw-h'); st2.textContent = err.message; st2.classList.add('bw-err');
        var bad = again.querySelector('[aria-invalid=true]'); if (bad) bad.focus();
      });
    };
    return form;
  }
  /* POST /p/<name>/config asked for JSON; a refusal carries the route's message and its fields. */
  function postConfig(name, params) {
    var body = new URLSearchParams(); params.forEach(function (p) { body.append(p[0], p[1]); });
    return fetch(base + '/p/' + encodeURIComponent(name) + '/config', { method: 'POST', credentials: 'same-origin', redirect: 'manual',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded', Accept: 'application/json' }, body: body.toString() }).then(function (r) {
      if (r.type === 'opaqueredirect' || r.status === 401) throw new Error('Your session ended. Reload the page to sign in again.');
      return r.json().catch(function () { return null; }).then(function (j) {
        if (j && j.ok) return j;
        var e = new Error((j && j.error) || (r.status === 403 ? 'Only a gardenio admin may edit a project’s config.' : 'That did not go through (' + r.status + ').'));
        e.fields = (j && j.fields) || {};
        throw e;
      });
    }, function () { throw new Error('The garden did not answer. Try again.'); });
  }

  /* ---------- share ---------- */
  function sharePane() {
    var box = el('div', 'pj-share');
    if (share && share.name === cur()) shareDraw(box, share.p);
    else { box.appendChild(el('p', 'bv-note', 'Loading the windows…')); shareGo(box, 'GET', '/share?days=' + shareDays); }
    return box;
  }
  /* every answer is the tab again, a refusal saying why in it */
  function shareGo(box, method, path, body) {
    var name = cur();
    sending = method === 'POST';
    fetch(base + '/p/' + encodeURIComponent(name) + path, { method: method, credentials: 'same-origin', redirect: 'manual', cache: 'no-store',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded', Accept: 'application/json' }, body: body }).then(function (r) { return r.json(); })
      .then(function (p) { sending = false; if (cur() === name) shareDraw(box, p); }, function () {
        sending = false; share = null; box.textContent = '';
        box.appendChild(failNote('Could not read its windows: the garden did not answer.'));
      });
  }
  function shareDraw(box, p) {
    share = p.error ? null : { name: cur(), p: p };
    box.textContent = '';
    if (p.error) { box.appendChild(el('p', 'bv-note err', p.error)); return; }
    var post = function (path) { shareGo(box, 'POST', path, 'days=' + shareDays); };
    if (p.minted) {
      var m = box.appendChild(el('div', 'bv-sec pj-minted')), u = el('input', 'bw-in'), c = el('button', 'btn small', 'Copy');
      m.setAttribute('role', 'status');
      m.appendChild(el('p', 'pj-big', 'This link is shown once: copy it now.'));
      u.readOnly = true; u.value = p.minted.url; u.setAttribute('aria-label', 'Window link'); u.onfocus = function () { u.select(); };
      c.type = 'button'; c.onclick = function () { u.select(); if (navigator.clipboard) navigator.clipboard.writeText(u.value); c.textContent = 'Copied'; };
      m.appendChild(u); m.appendChild(c);
      m.appendChild(el('p', 'bv-note', 'It expires on ' + String(p.minted.expires_at).slice(0, 10) + '. Revoke it below at any time.'));
    }
    if (p.action_error) box.appendChild(el('p', 'bv-note err', p.action_error)).setAttribute('role', 'alert');
    if (p.revoked) box.appendChild(el('p', 'bv-note', 'Window ' + p.revoked + ' is revoked: its link no longer opens.')).setAttribute('role', 'status');
    box.appendChild(el('p', 'qv-line', 'A window is a read-only public link to the whole land. ' + p.grants));
    var f = el('form', 'bv-acts'), sel = el('select', 'bw-in'), mk = el('button', 'btn small', 'Create link');
    sel.setAttribute('aria-label', 'Expires in');
    (p.expiries || []).forEach(function (n) { var o = el('option', '', 'Expires in ' + n + ' days'); o.value = n; o.selected = n === p.days; sel.appendChild(o); });
    sel.onchange = function () { shareDays = +sel.value; shareGo(box, 'GET', '/share?days=' + shareDays); };   // the preview names the expiry
    mk.type = 'submit';
    f.onsubmit = function (e) { e.preventDefault(); mk.disabled = true; post('/share'); };
    f.appendChild(sel); f.appendChild(mk); box.appendChild(f);
    var v = el('div'), a = el('a', 'btn small', 'Open the preview');
    a.href = p.preview; a.target = '_blank'; a.rel = 'noopener';
    v.appendChild(el('p', 'qv-line', 'The exact page a window minted now would show, ' + plural(p.plans || 0, 'plan', 'plans') + ' on it, in a new tab:'));
    v.appendChild(a);
    v.appendChild(el('p', 'bv-note', withheldWords(p)));
    box.appendChild(sec('What a visitor sees', v));
    box.appendChild(sec('Windows on this land', list('Windows', p.windows || [], function (w) {
      var li = row();
      li.appendChild(el('span', 'qv-t', w.id));
      li.appendChild(words(el('p', 'qv-line'), windowWords(w)));
      if (w.state === 'live') {
        var b = li.appendChild(el('button', 'btn small ghost', 'Revoke')); b.type = 'button';
        b.onclick = function () { if (window.confirm('Revoke this window? Its link stops opening for everyone who has it.')) { b.disabled = true; post('/share/' + encodeURIComponent(w.id) + '/revoke'); } };
      }
      return li;
    }, 'No windows on this land yet.')));
  }

  function hold() {
    if (sending) return true;
    if (bodyEl.querySelector('.pj-form, .armed, .act [disabled]')) return true;
    return Array.prototype.some.call(bodyEl.querySelectorAll('.act textarea'), function (x) { return !!x.value; });
  }

  var PANES = { agents: agentsPane, gardeners: gardenersPane, accounts: accountsPane, cost: costPane, health: healthPane, activity: activityPane, decisions: decisionsPane, config: configPane, share: sharePane };
  function paint(name, d, note, isErr, retry) {
    kindEl.textContent = 'project';
    titleEl.textContent = name;
    if (d && (!tab || (tab === SHARE[0] && !d.can_share))) setTab(pickTab(W.param(location.search, 'tab'), d));
    /* the Cost or Health tab painted again for the same project keeps its charts section, and every frame in it, where it is */
    var keep = d && CHARTS[tab] && charts && charts.tab === tab && charts.project === name && bodyEl.contains(charts.node) ? charts.node : null;
    if (keep) clearBut(bodyEl, keep); else { bodyEl.textContent = ''; stopCharts(charts); charts = null; }
    noteEl = el('p', 'bv-note' + (isErr ? ' err' : ''), note || '');
    noteEl.setAttribute('role', 'status');
    if (isErr && retry) { noteEl.appendChild(document.createTextNode(' ')); noteEl.appendChild(failNote('').lastChild); }
    if (!d) { bodyEl.appendChild(noteEl); return; }
    var kept = keep ? keep.parentNode : null;
    var meta = el('div', 'bv-meta'); meta.appendChild(el('span', '', summary(d))); bodyEl.insertBefore(meta, kept);
    if (d.can_edit) {
      // the project's bulk edits are LATER (spec 2.3): its Work list on the old viewer keeps them (Lands 5.13)
      var old = el('a', 'bl', 'Bulk edits: old viewer'); old.href = base + '/p/' + encodeURIComponent(name) + '?view=list';
      meta.appendChild(el('p', 'qv-line')).appendChild(old);   // its own line: a wrapped ' · ' would dangle
    }
    if (d.credential_pool) {
      // the operator land's header opens the fleet's credential pool for an admin (GL-15, web/creds.js; project.go)
      var pool = el('a', 'bl', 'Credential pool'); pool.href = base + '/garden' + W.withPanel('', 'creds', 'pool');
      meta.appendChild(el('p', 'qv-line')).appendChild(pool);
    }
    if (ownerWords(d.owner)) {
      var row = meta.appendChild(el('p', 'qv-line pj-owner', ownerWords(d.owner)));
      var owner = function () { var bec = BECOME.button(0, 0, d.owner.principal, d.owner.name); if (bec && row.isConnected) row.append(' ', bec); };
      if (typeof BECOME !== 'undefined') owner();
      else if (typeof LAZY !== 'undefined' && document.body.dataset.admin === '1') LAZY.need('land').then(owner, function () {});
    }
    seeTab(bodyEl.insertBefore(tabsRow(d), kept));
    bodyEl.insertBefore(noteEl, kept);
    var pane = kept || el('div', 'pj-pane'); pane.id = 'pj-pane'; pane.setAttribute('role', 'tabpanel'); pane.setAttribute('aria-labelledby', 'pj-tab-' + tab);
    pane.insertBefore(PANES[tab](d), keep);
    if (CHARTS[tab] && !keep) pane.appendChild(chartsSec(name, tab));
    if (!kept) bodyEl.appendChild(pane);
    if (typeof TITLES !== 'undefined') TITLES.dress(bodyEl);
  }

  W.register('project', {
    noun: 'project',
    url: url,
    paint: paint,
    done: function (name, d) { if (d) { last = d; lastAt = Math.floor(Date.now() / 1000); } },
    hold: hold,
    failed: function (name, text, retry) {
      lastAt = Math.floor(Date.now() / 1000);   // the map's next moves wait REFRESH_S before trying again
      var n = el('p', 'bv-note err bv-livenote', W.refreshFailure(text, 'project') + ' ');
      n.setAttribute('role', 'status');
      if (retry) n.appendChild(failNote('').lastChild);
      var old = bodyEl.querySelector('.bv-livenote'); if (old) old.remove();
      bodyEl.insertBefore(n, bodyEl.firstChild);
    },
    shown: function () { tab = isTab(W.param(location.search, 'tab')) ? W.param(location.search, 'tab') : ''; },
    closed: function () { last = null; noteEl = null; sending = false; tab = ''; share = null; stopCharts(charts); charts = null; if (W.param(location.search, 'tab') !== null) setTab(''); }   // before the host writes the next address
  });

  /* A farm patch that moved this project's rows refetches it, at most every REFRESH_S seconds, never over a form. */
  var farm = null, dirty = false;
  document.addEventListener('farmdoc', function (e) {
    var was = farm, name = cur();
    farm = e.detail;
    if (!name || !farm) return;
    if (moved(was, farm, name)) dirty = true;
    if (dirty && !hold() && Date.now() / 1000 - lastAt >= REFRESH_S) { dirty = false; W.refresh('project', name); }
  });
  /* An answer sent from a decision or escalation here (web/answer.js) moves the project: show it. */
  document.addEventListener('garden:answered', function () { if (cur()) W.refresh('project', cur()); });
  return api;
})();
if (typeof module !== 'undefined') module.exports = PROJECT;
