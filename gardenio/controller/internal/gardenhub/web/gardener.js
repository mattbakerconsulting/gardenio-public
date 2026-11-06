/* web/gardener.js: the gardener panel (Lands workbench-glass spec WB-11, step 5.12, gd-au7ywvWAG8), the Workbench's gardener
   kind, /garden?gardener=<id>: one gardener loop, the trigger bead that spawns its runs. It reads GET /garden/gardener/<id>
   (gardener.go: the viewer's own gardener page sections) and shows it in tabs, one at a time:
   - About: its state in words (paused, waiting for its next fire, a run out, ended and why), its goal, when it wakes and
     when it stops, its counters and spend, and a liveness worry when it should have fired and did not;
   - Runs: the runs it spawned, newest first, each with its outcome, its wrap-up's tl;dr, its cost and turns;
   - Beads: what its runs touched, by state;
   - Config: its prompt, goal, budgets, schedule and stop/progress/conditions specs, and, for an operator, the editor
     (POST /gardener/<id>/config asked for JSON; a refusal says why and keeps what was typed);
   - Successors: the gardeners it hands on to when it ends, and, for an operator, a form that adds one (POST
     /gardener/<id>/successor);
   - Charts, for a viewer who may read the Grafana charts (body data-costs; gd-g7Ms8Ddfmj): its
     runs' bill, context, turns and tokens, GET /garden/charts/gardener?gardener=<id> (charts.go: the server reads the runs,
     the page never names an agent), painted by the Costs panel's cards (COSTS.paintCostCards) under the range chips the
     Costs panel keeps (garden.costs.range). A refresh of the panel keeps the section and every frame in it where it is.
   Above the tabs an operator gets its levers, each the gardener page's own POST asked for JSON: Pause or Resume
   (/disable, /enable), Run now, Cancel the last run, the wake mode (timed or event) and Retire; the last two and Cancel ask
   first. The tab is in the address (tab=), as the project panel's is. Opened from a gardener bead's face, the project
   panel's Gardeners tab and the retired /gardener/<id> page (R9). It refreshes in place when the map's farm document moves
   (at most every REFRESH_S seconds), never over a form being filled in. Every bead it names opens here; text goes in through
   textContent. */
var GARDENER = (function () {
  /* ---------- pure logic (also run under node by the Go tests) ---------- */
  var TABS = [['about', 'About'], ['runs', 'Runs'], ['beads', 'Beads'], ['config', 'Config'], ['successors', 'Successors'], ['charts', 'Charts']];
  var REFRESH_S = 20;
  function isTab(t) { return TABS.some(function (x) { return x[0] === t; }); }
  /* The tabs a viewer is shown: Charts only to one who may read the charts. */
  function tabsFor(charts) { return TABS.filter(function (x) { return charts || x[0] !== 'charts'; }); }
  function url(id) { return '/garden/gardener/' + encodeURIComponent(id); }
  /* The query with tab=<t> set, or dropped for '', every other parameter kept as written. */
  function withTab(search, tab) {
    var keep = String(search || '').replace(/^\?/, '').split('&').filter(function (p) { return p && p.split('=')[0] !== 'tab'; });
    if (tab) keep.push('tab=' + encodeURIComponent(tab));
    return keep.length ? '?' + keep.join('&') : '';
  }
  function pickTab(want, charts) { return isTab(want) && (charts || want !== 'charts') ? want : 'about'; }
  /* A run is out: the life-thread holds a live one; when the runs could not be read, its last run is not known to be done. */
  function running(d) { return (d.live_count || 0) > 0 || (!!d.in_flight && (d.partial || []).indexOf('runs') >= 0); }
  function plural(n, one, many) { return n + ' ' + (n === 1 ? one : many); }
  /* The count a tab carries in its name ('' for none): the runs it holds, the beads its runs touched in every lane the
     pane lists (done ones too, as Runs counts finished runs), its successors. */
  function tabCount(tab, d) {
    if (!d) return '';
    var n = 0;
    if (tab === 'runs') n = (d.runs || []).length + ((d.more || {}).runs || 0);
    else if (tab === 'beads') (d.beads || []).forEach(function (l) { n += l.count || 0; });
    else if (tab === 'successors') n = (d.successors || []).length;
    return n ? String(n) : '';
  }
  /* Its state in words, {text, tone}: ended (and whether its goal was reached), paused, a run out, or waiting for its next
     fire. Tone is '' (fine), 'ask' (a worry: failures, overdue, silent), 'dim' (paused or ended) or 'ok' (goal reached). */
  function state(d) {
    if (d.closed) {
      var why = d.ended ? ': ' + d.ended : '';
      if (d.goal_state === 'achieved') return { text: 'Ended, its goal reached' + why + '.', tone: 'ok' };
      return { text: 'Ended' + why + (d.goal_state === 'unachieved' ? '; its goal was not reached.' : '.'), tone: 'dim' };
    }
    if (!d.enabled) return { text: 'Paused: it does not fire until it is resumed.', tone: 'dim' };
    var t = running(d) ? 'Running: a run is out now.' : 'Waiting' + (d.next_fire ? ' to fire ' + d.next_fire : ' for its next fire') + '.';
    var worry = d.overdue || d.silent || (d.failures ? plural(d.failures, 'run', 'runs') + ' in a row failed.' : '');
    return { text: worry ? t + ' ' + worry : t, tone: worry ? 'ask' : '' };
  }
  var MODE = { timed: 'wakes on a schedule', event: 'wakes on an event' };
  /* The line under the title: how it wakes, its project, its runs, its spend. */
  function summary(d) {
    var out = [];
    if (d.mode && MODE[d.mode]) out.push(MODE[d.mode]);
    if (d.project) out.push('in ' + d.project);
    out.push(plural(d.run_count || 0, 'run', 'runs'));
    if (d.spend) out.push(d.spend + ' spent');
    if (d.standing) out.push('standing');
    return out.join(' · ');
  }
  /* Its counters as rows: the runs the thread holds, the engine's tally, the caps, the failures. */
  function counterRows(d) {
    var rows = [], runs = plural(d.run_count || 0, 'run', 'runs');
    if (d.live_count) runs += ', ' + d.live_count + ' running';
    if (d.did_not_run) runs += ' (+' + d.did_not_run + ' never ran)';
    rows.push(['Runs', runs]);
    if (d.engine_count) rows.push(['Counted', d.engine_count]);
    var caps = [];
    if (d.max_runs) caps.push('stops after ' + plural(d.max_runs, 'run', 'runs'));
    if (d.run_budget) caps.push('budget ' + plural(d.run_budget, 'run', 'runs'));
    if (caps.length) rows.push(['Limits', caps.join(', ') + (d.standing ? ' (counted, not enforced: it is standing)' : '')]);
    if (d.spend) rows.push(['Spent', d.spend + (d.spend_stale ? ' (not refreshed lately)' : '')]);
    if (d.cron) rows.push(['Schedule', d.cron + (d.timezone ? ' (' + d.timezone + ')' : '') + (d.next_fire && !d.closed && d.enabled ? ', fires ' + d.next_fire : '')]);
    if (d.owner) rows.push(['Owner', d.owner]);
    return rows;
  }
  var STATE_WORD = { working: 'working', done: 'done', failed: 'failed', expired: 'expired', idle: 'idle', spawning: 'starting' };
  /* A run's words: how it went, its label, its cost and turns, when. */
  function runLine(r) {
    var out = [r.did_not_run ? 'never ran' : r.live ? (STATE_WORD[r.state] || 'running') : r.outcome === 'fail' ? 'failed' : 'finished'];
    if (r.label) out.push(r.label);
    if (r.cost) out.push(r.cost);
    if (r.turns) out.push(r.turns);
    if (r.ago) out.push(r.ago + ' ago');
    return out;
  }
  /* The levers an operator may pull now, each {act (the POST under /gardener/<id>/), label, params, ask (a confirm naming the
     gardener, or ''), danger}: none on an ended gardener; Pause or Resume; Run now; the mode flip when the mode is known;
     then, apart from those, the two that stop it: Cancel the last run while one is out, and Retire. */
  function levers(d) {
    if (!d || !d.can_operate || d.closed) return [];
    var out = [];
    out.push(d.enabled ? { act: 'disable', label: 'Pause', params: [], ask: '' } : { act: 'enable', label: 'Resume', params: [], ask: '' });
    out.push({ act: 'run-now', label: 'Run now', params: [], ask: '' });
    if (!d.mode_ambiguous && (d.mode === 'timed' || d.mode === 'event')) {
      var to = d.mode === 'timed' ? 'event' : 'timed';
      out.push({ act: 'mode', label: to === 'event' ? 'Wake on an event' : 'Wake on a schedule', params: [['mode', to]], ask: '' });
    }
    var who = '“' + (d.name || d.title || d.id) + '”';
    if (running(d)) out.push({ act: 'cancel-run', label: 'Cancel the last run', params: [], ask: 'Cancel the last run of the gardener ' + who + '? Its agent is stopped.', danger: true });
    out.push({ act: 'retire', label: 'Retire', params: [], ask: 'Retire the gardener ' + who + '? It stops for good and is closed.', danger: true });
    return out;
  }
  /* The config's fields as the tab lists and the editor edits them: [key, label, kind]; kind 'text' is a textarea, 'json'
     a JSON textarea, 'flag' a checkbox, else a one-line input. */
  var FIELDS = [['prompt', 'Run prompt', 'text'], ['terminal_goal', 'Terminal goal', 'text'], ['exit_condition', 'Exit condition'], ['on_exit', 'On exit'],
    ['cron', 'Schedule (cron)'], ['cooldown_minutes', 'Cooldown (minutes)'], ['max_concurrent', 'Max concurrent'],
    ['run_budget', 'Run budget'], ['max_runs', 'Max runs'], ['retry_on_failure', 'Retry a failed run', 'flag'], ['max_retries', 'Max retries'],
    ['run_timeout', 'Run timeout'], ['timeout', 'Loop timeout'], ['one_spawn_at_a_time', 'One run at a time', 'flag'],
    ['stop_json', 'Stop spec', 'json'], ['progress_json', 'Progress spec', 'json'], ['conditions', 'Conditions', 'json']];
  /* The config as label and value rows, the long texts and specs left to their own blocks; a blank says so. */
  function configRows(c) {
    return FIELDS.filter(function (f) { return !f[2]; }).map(function (f) { return [f[1], c[f[0]] || '—']; }).concat(
      FIELDS.filter(function (f) { return f[2] === 'flag'; }).map(function (f) { return [f[1], c[f[0]] ? 'yes' : 'no']; }));
  }
  /* The editor's form as POST /gardener/<id>/config reads it: every field (it writes them all), a checkbox only when on. */
  function configParams(v) {
    var out = [];
    FIELDS.forEach(function (f) {
      if (f[2] === 'flag') { if (v[f[0]]) out.push([f[0], 'on']); return; }
      out.push([f[0], String(v[f[0]] == null ? '' : v[f[0]])]);
    });
    return out;
  }
  /* A successor's form as POST /gardener/<id>/successor reads it; the route wants a phase, a goal or a prompt advice. */
  var HEIR = [['phase', 'Phase'], ['terminal_goal', 'Terminal goal'], ['prompt_advice', 'Prompt advice id'], ['epic', 'Epic'], ['role', 'Role'],
    ['join_parent', 'Join parent'], ['branch_decision_key', 'Branch decision key']];
  function heirParams(v) { return HEIR.map(function (f) { return [f[0], String(v[f[0]] || '').trim()]; }); }
  function heirReady(v) { return ['phase', 'terminal_goal', 'prompt_advice'].some(function (k) { return String(v[k] || '').trim() !== ''; }); }
  /* The Charts tab's route for gardener id over a range (a value outside the chips is 7 days), on the dark theme the panel's
     dark box asks for in both skins; and why the charts did not come, in words, for the route's status (0: no answer). */
  var RANGES = [['today', 'Today'], ['7d', '7 days'], ['30d', '30 days']], RANGE_KEY = 'garden.costs.range';
  function pickRange(r) { return RANGES.some(function (x) { return x[0] === r; }) ? r : '7d'; }
  function chartsUrl(id, range) { return '/garden/charts/gardener?gardener=' + encodeURIComponent(id) + '&range=' + pickRange(range) + '&theme=dark'; }
  function chartsFail(status) {
    if (status === 403) return 'Its charts are for operators for now.';
    if (status === 401 || status === 'redirect') return 'Your session ended. Reload the page to sign in again.';
    return 'Could not load its charts' + (status ? ' (' + status + ')' : ': the garden did not answer') + '.';
  }
  function chartsRetry(status) { return status !== 400 && status !== 401 && status !== 403 && status !== 404 && status !== 'redirect'; }
  var api = { TABS: TABS, isTab: isTab, tabsFor: tabsFor, url: url, RANGES: RANGES, pickRange: pickRange, chartsUrl: chartsUrl, chartsFail: chartsFail, chartsRetry: chartsRetry, withTab: withTab, pickTab: pickTab, running: running, tabCount: tabCount, state: state, summary: summary,
    counterRows: counterRows, runLine: runLine, levers: levers, FIELDS: FIELDS, configRows: configRows, configParams: configParams,
    HEIR: HEIR, heirParams: heirParams, heirReady: heirReady, REFRESH_S: REFRESH_S };
  if (typeof document === 'undefined' || typeof WORKBENCH === 'undefined') return api;

  /* ---------- DOM ---------- */
  var W = WORKBENCH, B = document.body, base = B.dataset.base || '';
  var titleEl = document.getElementById('bv-title'), kindEl = document.getElementById('bv-kind'), bodyEl = document.getElementById('bv-body');
  if (!bodyEl) return api;
  var last = null, lastAt = 0, noteEl = null, sending = false, tab = '', charts = null, mayChart = B.dataset.costs === '1';

  function el(t, cls, text) { var e = document.createElement(t); if (cls) e.className = cls; if (text !== undefined) e.textContent = text; return e; }
  function cur() { var c = W.current(); return c && c.kind === 'gardener' ? c.id : ''; }
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
  function words(p, list) {
    list.forEach(function (w, i) { if (i) p.appendChild(document.createTextNode(' · ')); p.appendChild(document.createTextNode(w)); });
    return p;
  }
  function row(cls) { return el('li', 'qv-row' + (cls ? ' ' + cls : '')); }
  function sec(title, node) { var s = el('section', 'bv-sec pj-sec'); s.appendChild(el('h3', '', title)); s.appendChild(node); return s; }
  function stats(rows) {
    var dl = el('dl', 'bv-fields');
    rows.forEach(function (r) { dl.appendChild(el('dt', '', r[0])); dl.appendChild(el('dd', '', r[1])); });
    return dl;
  }
  function textBlock(t) { return el('p', 'bv-text gd-text', t); }
  function partial(d, what) { return (d.partial || []).indexOf(what) >= 0; }
  function failNote(text) {
    var n = el('p', 'bv-note err', text + ' ');
    n.setAttribute('role', 'status');
    var b = el('button', 'btn small bv-retry', 'Try again'); b.type = 'button';
    b.onclick = function () { b.disabled = true; b.textContent = 'Loading…'; W.refresh('gardener', cur()); };
    n.appendChild(b);
    return n;
  }
  function moreNote(n, what) { return n ? el('p', 'bv-note', '+' + n + ' more ' + what + ' not listed.') : null; }

  /* A POST to the gardener page's own route under /gardener/<id>/, asked for JSON; a refusal carries the route's message. */
  function post(path, params) {
    var body = new URLSearchParams(); params.forEach(function (p) { body.append(p[0], p[1]); });
    return fetch(base + path, { method: 'POST', credentials: 'same-origin', redirect: 'manual',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded', Accept: 'application/json' }, body: body.toString() }).then(function (r) {
      if (r.type === 'opaqueredirect' || r.status === 401) throw new Error('Your session ended. Reload the page to sign in again.');
      return r.json().catch(function () { return null; }).then(function (j) {
        if (j && j.ok) return j;
        throw new Error((j && j.error) || (r.status === 403 ? 'Only an operator may change a gardener.' : 'That did not go through (' + r.status + ').'));
      });
    }, function () { throw new Error('The garden did not answer. Try again.'); });
  }

  /* ---------- the levers ---------- */
  var DONE = { disable: 'Paused.', enable: 'Resumed.', 'run-now': 'A run is starting.', 'cancel-run': 'Cancelled its last run.', mode: 'Its wake mode is changed.', retire: 'Retired.' };
  function leversRow(d) {
    var ls = levers(d);
    if (!ls.length) return null;
    var r = el('div', 'bv-acts gd-levers'); r.setAttribute('role', 'group'); r.setAttribute('aria-label', 'Gardener controls');
    ls.forEach(function (l) {
      var b = el('button', 'btn small' + (l.danger ? ' ghost danger' : ''), l.label); b.type = 'button';
      b.onclick = function () {
        if (l.ask && !window.confirm(l.ask)) return;
        Array.prototype.forEach.call(r.querySelectorAll('button'), function (x) { x.disabled = true; });
        sending = true; say(l.label + '…');
        post('/gardener/' + encodeURIComponent(d.id) + '/' + l.act, l.params).then(function () {
          sending = false; say(DONE[l.act] || 'Done.'); W.refresh('gardener', cur());
        }, function (err) {
          sending = false; say(err.message, true);
          Array.prototype.forEach.call(r.querySelectorAll('button'), function (x) { x.disabled = false; });
        });
      };
      r.appendChild(b);
    });
    return r;
  }

  /* ---------- the tabs ---------- */
  function tabsRow(d) {
    var r = el('div', 'qv-tabs pj-tabs');
    r.setAttribute('role', 'tablist'); r.setAttribute('aria-label', 'Gardener');
    var tabs = tabsFor(mayChart);
    tabs.forEach(function (t) {
      var n = tabCount(t[0], d), b = el('button', 'btn small' + (t[0] === tab ? '' : ' ghost'), t[1] + (n ? ' ' + n : ''));
      b.type = 'button'; b.id = 'gd-tab-' + t[0]; b.setAttribute('role', 'tab'); b.setAttribute('aria-selected', t[0] === tab ? 'true' : 'false');
      b.setAttribute('aria-controls', 'gd-pane'); b.tabIndex = t[0] === tab ? 0 : -1;
      b.onclick = function () { show(t[0]); };
      r.appendChild(b);
    });
    /* left and right walk the tabs, Home and End go to the ends, as a tablist does */
    r.addEventListener('keydown', function (e) {
      var i = tabs.map(function (t) { return t[0]; }).indexOf(tab), n = tabs.length, j = -1;
      if (e.key === 'ArrowRight') j = (i + 1) % n; else if (e.key === 'ArrowLeft') j = (i + n - 1) % n; else if (e.key === 'Home') j = 0; else if (e.key === 'End') j = n - 1;
      if (j < 0) return;
      e.preventDefault(); e.stopPropagation();
      show(tabs[j][0]);
    });
    return r;
  }
  function show(t) {
    if (t === tab || !last) return;
    if (hold() && !window.confirm('Leave the form you are filling in?')) return;
    setTab(t);
    paint(cur(), last, '');
    var nb = document.getElementById('gd-tab-' + t); if (nb) nb.focus();
  }

  function aboutPane(d) {
    var box = el('div');
    if (d.goal) box.appendChild(sec('Goal', textBlock(d.goal)));
    else if (d.chasing) box.appendChild(sec('Chasing', textBlock(d.chasing)));
    if ((d.wakes || []).length) {
      var wu = el('ul', 'gd-list'); wu.setAttribute('aria-label', 'Wakes when');
      d.wakes.forEach(function (w) { wu.appendChild(el('li', '', w)); });
      box.appendChild(sec('Wakes when', wu));
    }
    if ((d.stops || []).length || d.on_loop_end) {
      var su = el('ul', 'gd-list'); su.setAttribute('aria-label', 'Stops when');
      (d.stops || []).forEach(function (s) { su.appendChild(el('li', '', s.charAt(0).toUpperCase() + s.slice(1))); });
      if (d.on_loop_end) su.appendChild(el('li', '', 'When its loop ends: ' + d.on_loop_end));
      box.appendChild(sec('Stops when', su));
    }
    box.appendChild(sec('Counters', stats(counterRows(d))));
    if (d.last_run && running(d)) { var p = el('p', 'qv-origin'); p.appendChild(document.createTextNode('Its last run: ')); p.appendChild(beadA(d.last_run, '', 'bl')); box.appendChild(p); }
    var gp = el('p', 'qv-origin'); gp.appendChild(document.createTextNode('Stored on ')); gp.appendChild(beadA(d.id, 'its trigger bead', 'bl')); box.appendChild(gp);
    return box;
  }
  function runsPane(d) {
    if (partial(d, 'runs')) return failNote('Could not read its runs: the beads daemon did not answer.');
    var box = el('div'), runs = d.runs || [];
    if (!runs.length) { box.appendChild(el('p', 'bv-empty', 'It has not run yet.')); return box; }
    var ul = el('ul', 'qv-list'); ul.setAttribute('aria-label', 'Runs');
    runs.forEach(function (r) {
      var li = row(r.outcome === 'fail' ? 'needs' : r.did_not_run ? 'gd-dim' : '');
      li.appendChild(words(el('p', 'qv-line'), runLine(r)));   // its outcome first, then which run it was
      li.appendChild(beadA(r.id, r.name || r.id));
      if (r.headline) li.appendChild(el('p', 'bv-text qv-text', r.headline));
      if (r.task_id) { var p = el('p', 'qv-origin'), ta = beadA(r.task_id, r.task_title || '', 'bl'); p.appendChild(document.createTextNode('on ')); p.appendChild(ta); if (typeof TITLES !== 'undefined') TITLES.unseen(ta, r.task_id, r.task_title); li.appendChild(p); }
      ul.appendChild(li);
    });
    box.appendChild(BEAD.foldList(ul));
    var m = moreNote((d.more || {}).runs, 'runs'); if (m) box.appendChild(m);
    return box;
  }
  var LANE_WORD = { in_progress: 'growing', blocked: 'blocked', open: 'planted', done: 'done' };
  function beadsPane(d) {
    if (partial(d, 'beads')) return failNote('Could not read what its runs touched: the beads daemon did not answer.');
    var box = el('div'), any = false;
    (d.beads || []).forEach(function (l) {
      if (!l.count) return;
      any = true;
      var ul = el('ul', 'qv-list'); ul.setAttribute('aria-label', l.label);
      (l.beads || []).forEach(function (b) {
        var li = row(l.lane === 'blocked' ? 'needs' : '');
        li.appendChild(beadA(b.id, b.title || b.id));
        var bits = []; if (b.type && b.type !== 'task') bits.push(b.type); bits.push(LANE_WORD[l.lane] || b.status || l.lane);
        li.appendChild(words(el('p', 'qv-line'), bits));
        ul.appendChild(li);
      });
      var s = sec(l.label + ' (' + l.count + ')', BEAD.foldList(ul));
      var m = moreNote((d.more || {})[l.lane], 'beads'); if (m) s.appendChild(m);
      box.appendChild(s);
    });
    if (!any) box.appendChild(el('p', 'bv-empty', 'Its runs have not touched a bead yet.'));
    return box;
  }

  /* ---------- config ---------- */
  function field(label, input) { var f = el('label', 'bw-f'); f.appendChild(el('span', 'bw-l', label)); f.appendChild(input); return f; }
  function configPane(d) {
    var box = el('div'), c = d.config;
    if (d.prompt) box.appendChild(sec('Run prompt', textBlock(d.prompt)));
    if (!c) {
      box.appendChild(el('p', 'bv-note', 'Only an operator may see and edit the rest of its config.'));
      return box;
    }
    var v = el('div');
    v.appendChild(stats(configRows(c)));
    FIELDS.forEach(function (f) { if (f[2] === 'json' && c[f[0]]) { v.appendChild(el('h4', 'bw-l', f[1])); v.appendChild(el('pre', 'gd-json', c[f[0]])); } });
    box.appendChild(sec('Config', v));
    var edit = el('button', 'btn small', 'Edit the config'); edit.type = 'button';
    edit.onclick = function () { edit.hidden = true; v.hidden = true; box.appendChild(editor(d, c, function () { edit.hidden = false; v.hidden = false; })).querySelector('textarea, input').focus(); };
    box.appendChild(edit);
    return box;
  }
  /* The editor: every field as stored; Save posts them all; a refusal keeps what was typed and says why. */
  function editor(d, c, done) {
    var form = el('form', 'bw-edit pj-form gd-form'), ins = {};
    form.setAttribute('aria-label', 'Edit the config of ' + (d.name || d.title || d.id));
    FIELDS.forEach(function (f) {
      var i;
      if (f[2] === 'flag') {
        var l = el('label', 'pj-flag'); i = el('input'); i.type = 'checkbox'; i.name = f[0]; i.checked = !!c[f[0]];
        l.appendChild(i); l.appendChild(document.createTextNode(' ' + f[1])); ins[f[0]] = i; form.appendChild(l); return;
      }
      if (f[2]) { i = el('textarea', 'bw-in' + (f[2] === 'json' ? ' gd-code' : '')); i.rows = f[2] === 'json' ? 4 : 5; i.spellcheck = f[2] !== 'json'; }
      else { i = el('input', 'bw-in'); if (/budget|runs|retries|minutes|concurrent/.test(f[0])) i.inputMode = 'numeric'; }
      i.name = f[0]; i.value = c[f[0]] || ''; ins[f[0]] = i;
      form.appendChild(field(f[1], i));
    });
    var acts = el('div', 'bv-acts'), save = el('button', 'btn small', 'Save'), no = el('button', 'btn small ghost', 'Cancel'), st = el('p', 'bw-h');
    save.type = 'submit'; no.type = 'button'; st.setAttribute('role', 'status');
    no.onclick = function () { form.remove(); done(); };
    acts.appendChild(save); acts.appendChild(no); form.appendChild(acts); form.appendChild(st);
    form.onsubmit = function (e) {
      e.preventDefault();
      var vals = {};
      Object.keys(ins).forEach(function (k) { vals[k] = ins[k].type === 'checkbox' ? ins[k].checked : ins[k].value; });
      save.disabled = true; no.disabled = true; sending = true; st.textContent = 'Saving…'; st.className = 'bw-h';
      post('/gardener/' + encodeURIComponent(d.id) + '/config', configParams(vals)).then(function () {
        sending = false; form.remove(); done(); say('Saved the config.'); W.refresh('gardener', cur());
      }, function (err) {
        sending = false; save.disabled = false; no.disabled = false;
        st.textContent = err.message; st.className = 'bw-h bw-err';
      });
    };
    return form;
  }

  /* ---------- successors ---------- */
  function successorsPane(d) {
    var box = el('div'), hs = d.successors || [];
    if (hs.length) {
      var ul = el('ol', 'gd-list'); ul.setAttribute('aria-label', 'Successors');
      hs.forEach(function (h) { ul.appendChild(el('li', '', h)); });
      box.appendChild(sec('When it ends, it hands on to', ul));
    } else box.appendChild(el('p', 'bv-empty', 'No successor: when it ends, nothing takes over.'));
    if (d.can_operate && !d.closed) {
      var add = el('button', 'btn small', 'Add a successor'); add.type = 'button';
      add.onclick = function () { add.hidden = true; box.appendChild(heirForm(d, function () { add.hidden = false; })).querySelector('input').focus(); };
      box.appendChild(add);
    }
    return box;
  }
  function heirForm(d, done) {
    var form = el('form', 'bw-edit pj-form gd-form'), ins = {};
    form.setAttribute('aria-label', 'Add a successor');
    HEIR.forEach(function (f) { var i = el('input', 'bw-in'); i.name = f[0]; i.maxLength = 2000; ins[f[0]] = i; form.appendChild(field(f[1], i)); });
    form.appendChild(el('p', 'bw-h', 'Give it at least a phase, a terminal goal or a prompt advice id.'));
    var acts = el('div', 'bv-acts'), save = el('button', 'btn small', 'Add'), no = el('button', 'btn small ghost', 'Cancel'), st = el('p', 'bw-h');
    save.type = 'submit'; no.type = 'button'; st.setAttribute('role', 'status');
    no.onclick = function () { form.remove(); done(); };
    acts.appendChild(save); acts.appendChild(no); form.appendChild(acts); form.appendChild(st);
    form.onsubmit = function (e) {
      e.preventDefault();
      var vals = {}; Object.keys(ins).forEach(function (k) { vals[k] = ins[k].value; });
      if (!heirReady(vals)) { st.textContent = 'Give it a phase, a terminal goal or a prompt advice id.'; st.className = 'bw-h bw-err'; ins.phase.focus(); return; }
      save.disabled = true; no.disabled = true; sending = true; st.textContent = 'Adding…'; st.className = 'bw-h';
      post('/gardener/' + encodeURIComponent(d.id) + '/successor', heirParams(vals)).then(function () {
        sending = false; form.remove(); done(); say('Added a successor.'); W.refresh('gardener', cur());
      }, function (err) {
        sending = false; save.disabled = false; no.disabled = false; st.textContent = err.message; st.className = 'bw-h bw-err';
      });
    };
    return form;
  }

  function hold() { return sending || !!bodyEl.querySelector('.gd-form'); }

  /* ---------- the Charts tab ---------- */
  function chartsPane(d) {
    var n = (d.runs || []).filter(function (r) { return !r.did_not_run; }).length;
    return el('p', 'bv-note', n ? 'Its ' + (n === 1 ? 'run' : 'newest ' + n + ' runs') + ' in Grafana: what each cost, how full its context ran, its turns and its tokens.' : 'It has not run yet, so there is nothing to chart.');
  }
  /* The section painted once per showing of the tab, which paint() keeps in place across refreshes; charts is its state:
     the gardener, the range, its parts and card stack, and the fetch in flight (seq). */
  function chartsSec(id) {
    var st = { id: id, seq: 0, range: '7d' };
    try { st.range = pickRange(localStorage.getItem(RANGE_KEY)); } catch (e) { /* a blocked store: 7 days */ }
    var s = el('section', 'bv-sec pj-sec pj-charts gd-charts'); s.setAttribute('aria-label', 'Charts');
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
    Array.prototype.forEach.call(st.chips.children, function (b) { var on = b.dataset.range === st.range; b.setAttribute('aria-checked', on ? 'true' : 'false'); b.tabIndex = on ? 0 : -1; });
    if (!st.cards.children.length) st.cards.appendChild(el('p', 'bv-note', 'Loading the charts…'));
    var fail = function (status) { var e = new Error(chartsFail(status)); e.retry = chartsRetry(status); throw e; };
    fetch(base + chartsUrl(st.id, st.range), { credentials: 'same-origin', redirect: 'manual', cache: 'no-store', headers: { Accept: 'application/json' } })
      .then(function (r) { if (r.ok) return r.json(); fail(r.type === 'opaqueredirect' ? 'redirect' : r.status); }, function () { fail(0); })
      .then(function (res) {
        if (charts !== st || my !== st.seq) return;   // the tab or the range moved on
        paintCharts(st, res);
      }, function (err) {
        if (charts !== st || my !== st.seq) return;
        stopCharts(st); st.cards.textContent = ''; st.foot.textContent = '';
        rangeOff(st, null);
        var n = el('p', 'bv-note', err.message + ' '); n.setAttribute('role', 'status'); st.cards.appendChild(n);
        if (err.retry) {
          var b = el('button', 'btn small bv-retry', 'Try again'); b.type = 'button';
          b.onclick = function () { b.disabled = true; b.textContent = 'Loading…'; loadCharts(st); };
          n.appendChild(b);
        }
      });
  }
  /* The chips only move the cards' srcs: with no answer they go (its note says why), and with no card that has a chart they
     go and one line says why (COSTS.rangeOff). */
  function rangeOff(st, res) {
    var why = res && typeof COSTS !== 'undefined' ? COSTS.rangeOff(res) : '';
    st.chips.hidden = !res || !!why;
    st.off.textContent = why; st.off.hidden = !why;
  }
  function paintCharts(st, res) {
    var cards = (res && res.panels) || [];
    rangeOff(st, res);
    st.foot.textContent = '';
    if (!cards.length || typeof COSTS === 'undefined') {
      stopCharts(st); st.cards.textContent = '';
      st.cards.appendChild(el('p', 'bv-empty', cards.length ? 'The charts did not load on this page.' : 'It has not run yet, so there is nothing to chart.'));
      return;
    }
    if (!(st.stack && st.stack.update(cards))) { stopCharts(st); st.stack = COSTS.paintCostCards(st.cards, cards, { root: bodyEl, idPrefix: 'gd-gc', heading: 'h4' }); }
    if (res.open_dashboard_url) {
      var p = el('p', 'qv-origin'), a = el('a', 'bl', 'Open the dashboard in Grafana ↗'); a.href = res.open_dashboard_url; a.target = '_blank'; a.rel = 'noopener';
      p.appendChild(a); st.foot.appendChild(p);
      var q = el('p', 'bv-note', 'Charts blank? '), g = el('a', 'bl', 'Open Grafana once to sign in ↗'); g.href = res.open_dashboard_url; g.target = '_blank'; g.rel = 'noopener';
      q.appendChild(g); st.foot.appendChild(q);
    }
  }
  /* The painter's lazy-load observer and slow-chart timers go with the cards they watch. */
  function stopCharts(st) { if (st && st.stack) { st.stack.stop(); st.stack = null; } }
  /* Every child of box but the branch that holds node, which stays where it is (an iframe moved in the DOM reloads). */
  function clearBut(box, node) {
    Array.prototype.slice.call(box.childNodes).forEach(function (c) { if (c !== node) { if (c.contains(node)) clearBut(c, node); else box.removeChild(c); } });
  }

  var PANES = { about: aboutPane, runs: runsPane, beads: beadsPane, config: configPane, successors: successorsPane, charts: chartsPane };
  function paint(id, d, note, isErr, retry) {
    kindEl.textContent = d && d.standing ? 'standing gardener' : 'gardener loop';
    var known = (typeof TITLES !== 'undefined' && TITLES.title(id)) || 'This gardener';   // gd-dv5wiNnKuh
    titleEl.textContent = d ? d.name || d.title || known : last && last.id === id ? last.name || last.title || known : known;
    if (d && pickTab(tab, mayChart) !== tab) setTab(pickTab(tab || W.param(location.search, 'tab'), mayChart));   // none yet, or Charts for one who may not
    /* the Charts tab painted again for the same gardener keeps its section, and every frame in it, where it is */
    var keep = d && tab === 'charts' && charts && charts.id === id && bodyEl.contains(charts.node) ? charts.node : null;
    if (keep) clearBut(bodyEl, keep); else { bodyEl.textContent = ''; stopCharts(charts); charts = null; }
    noteEl = el('p', 'bv-note' + (isErr ? ' err' : ''), note || '');
    noteEl.setAttribute('role', 'status');
    if (isErr && retry) { noteEl.appendChild(document.createTextNode(' ')); noteEl.appendChild(failNote('').lastChild); }
    if (!d) { bodyEl.appendChild(noteEl); return; }
    var kept = keep ? keep.parentNode : null;
    var meta = el('div', 'bv-meta'); meta.appendChild(el('span', '', summary(d))); bodyEl.insertBefore(meta, kept);
    var st = state(d), sp = el('p', 'gd-state' + (st.tone ? ' ' + st.tone : ''), st.text);
    sp.setAttribute('role', st.tone === 'ask' ? 'alert' : 'note');
    bodyEl.insertBefore(sp, kept);
    var lv = leversRow(d); if (lv) bodyEl.insertBefore(lv, kept);
    bodyEl.insertBefore(noteEl, kept);
    PROJECT.seeTab(bodyEl.insertBefore(tabsRow(d), kept));
    var pane = kept || el('div', 'pj-pane'); pane.id = 'gd-pane'; pane.setAttribute('role', 'tabpanel'); pane.setAttribute('aria-labelledby', 'gd-tab-' + tab);
    pane.insertBefore(PANES[tab](d), keep);
    if (tab === 'charts' && !keep) pane.appendChild(chartsSec(id));
    if (!kept) bodyEl.appendChild(pane);
    if (typeof TITLES !== 'undefined') TITLES.dress(bodyEl);
  }

  W.register('gardener', {
    noun: 'gardener',
    url: url,
    paint: paint,
    done: function (id, d) { if (d) { last = d; lastAt = Math.floor(Date.now() / 1000); } },
    hold: hold,
    failed: function (id, text, retry) {
      lastAt = Math.floor(Date.now() / 1000);
      var n = el('p', 'bv-note err bv-livenote', W.refreshFailure(text, 'gardener') + ' ');
      n.setAttribute('role', 'status');
      if (retry) n.appendChild(failNote('').lastChild);
      var old = bodyEl.querySelector('.bv-livenote'); if (old) old.remove();
      bodyEl.insertBefore(n, bodyEl.firstChild);
    },
    shown: function () { tab = isTab(W.param(location.search, 'tab')) ? W.param(location.search, 'tab') : ''; },
    closed: function () { last = null; noteEl = null; sending = false; tab = ''; stopCharts(charts); charts = null; if (W.param(location.search, 'tab') !== null) setTab(''); }
  });

  /* The map's farm document moved: its runs may have too. At most every REFRESH_S seconds, never over a form. */
  document.addEventListener('farmdoc', function () { if (cur() && !hold() && Date.now() / 1000 - lastAt >= REFRESH_S) W.refresh('gardener', cur()); });
  return api;
})();
if (typeof module !== 'undefined') module.exports = GARDENER;
