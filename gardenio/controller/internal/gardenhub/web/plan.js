/* web/plan.js: the plan cockpit (Lands workbench-glass WB-7, WB-8, step 5.9), the Workbench's plan kind, /garden?plan=<id>.
   It reads GET /garden/plan/<id> (plan.go) and shows where the plan stands and its levers, what it waits on, its steps by
   phase, its asks, crew, narrative and the pour of a formula as a new phase. Every write is the viewer's own /plans/<id>/*
   route asked for JSON, offered when d.edit; asks are answered through the Mailbox's answer box (web/answer.js). Live
   (WB-14): /garden/plan/<id>/events says when it moves; a repaint waits while the viewer holds something here. Text goes
   in through textContent; the narrative's markdown element by element (BEAD.mdInto). */
var PLAN = (function () {
  /* ---------- pure logic (also run under node by the Go tests) ---------- */
  function url(id) { return '/garden/plan/' + encodeURIComponent(id); }
  function path(id, rest) { return '/plans/' + encodeURIComponent(id) + (rest || ''); }
  function span(sec) {
    if (sec < 3600) return Math.max(1, Math.round(sec / 60)) + 'm';
    if (sec < 86400) return Math.round(sec / 3600) + 'h';
    return Math.round(sec / 86400) + 'd';
  }
  var STATE = { draft: 'Draft: still being worked out.', ready: 'Ready: waiting on the owner to approve the build.', building: 'Building.', complete: 'Complete.', abandoned: 'Abandoned.' };
  var STATUS = { open: 'open', in_progress: 'in progress', blocked: 'blocked', deferred: 'deferred', closed: 'closed', review: 'in review' };
  function words(s) { return String(s || '').replace(/_/g, ' '); }
  /* Where the plan stands, in a sentence: its lifecycle, or its status when it has none. */
  function stateLine(d) {
    if (!d.plan) return 'An epic, not yet a plan.';
    if (STATE[d.state]) return STATE[d.state];
    return d.status === 'closed' ? 'Closed.' : 'Not in the plan lifecycle.';
  }
  /* The line under it: progress, then when it last moved. */
  function progress(d, now) {
    var out = [d.total ? d.closed + ' of ' + d.total + ' steps closed (' + d.pct + '%)' : 'No steps yet'];
    if (d.project) out.push(d.project);
    if (d.last_change > 0) out.push('changed ' + span(Math.max(60, now - d.last_change)) + ' ago');
    return out.join(' · ');
  }
  function finished(d) { return d.status === 'closed' || d.state === 'complete' || d.state === 'abandoned'; }
  /* What an armed verb names (rubric C8): a bead's short title quoted, cut at 40, or its id. */
  function named(x) {
    var t = Array.from(String(x.short_title || x.title || ''));
    return t.length ? '“' + (t.length > 40 ? t.slice(0, 39).join('') + '…' : t.join('')) + '”' : x.id;
  }
  /* The lifecycle levers by where the plan stands, as the daemon takes them (plans_lifecycle.go planStateCanComplete). A
     terminal one (complete, abandon) arms first; Abandon is the danger verb, last. None without edit. */
  function levers(d) {
    if (!d || !d.edit || !d.id) return [];
    var p = function (rest) { return path(d.id, rest); };
    if (!d.plan) return [{ act: 'mark', path: p('/mark'), word: 'Mark as a plan', done: 'Marked ' + d.id + ' as a plan.' }];
    if (finished(d)) return [{ act: 'reopen', path: p('/reopen'), word: 'Reopen', done: 'Reopened.' }];
    var out = [];
    if (d.state === 'ready') {
      out.push({ act: 'build', path: p('/build'), word: 'Approve and build', done: 'Building: the gardener is on.' });
      out.push({ act: 'draft', path: p('/state'), params: { state: 'draft' }, word: 'Keep editing', done: 'Back to draft.' });
    } else if (d.state === 'draft') out.push({ act: 'ready', path: p('/state'), params: { state: 'ready' }, word: 'Mark ready', done: 'Marked ready.' });
    if (d.state === 'building' || !d.state) out.push({ act: 'complete', path: p('/complete'), word: 'Mark complete', arm: 'Tap again to complete', done: 'Completed.' });
    out.push({ act: 'abandon', path: p('/abandon'), word: 'Abandon', arm: 'Tap again to abandon ' + named(d), danger: true, done: 'Abandoned.' });
    return out;
  }
  /* The step groups: each phase in order, then the steps under none ("Other steps" when there are phases). */
  function groups(d) {
    var out = (d.phases || []).map(function (ph) { return { phase: ph, title: ph.title || ph.id, steps: ph.steps || [] }; });
    if ((d.steps || []).length || !out.length) out.push({ phase: null, title: out.length ? 'Other steps' : 'Steps', steps: d.steps || [] });
    return out;
  }
  /* The words under a step's title: state, owner, MR, waits, age; every state a word (never colour alone). */
  function stepLine(s, now) {
    var out = [STATUS[s.status] || words(s.status) || (s.closed ? 'closed' : 'open')];
    if (s.assignee) out.push(s.assignee);
    if (s.mr && s.mr.state) out.push('MR ' + words(s.mr.state));
    var n = (s.blocked_by || []).length;
    if (n && !s.closed) out.push('waits on ' + (n === 1 ? (s.blocked_by[0].escalation ? 'an escalation' : '1 bead') : n + ' beads'));
    if (s.updated > 0) out.push(span(Math.max(60, now - s.updated)) + ' ago');
    return out;
  }
  /* A step's writes: Dispatch, Rename, Up and Down within its group, and apart, Close (armed) or Reopen. None without
     edit, or on a finished plan but Reopen. */
  function stepVerbs(d, s, first, last) {
    if (!d || !d.edit || !s || !s.id) return [];
    var p = function (rest) { return path(d.id, '/steps/' + encodeURIComponent(s.id) + rest); }, out = [];
    if (s.closed) return [{ act: 'reopen', path: p('/reopen'), word: 'Reopen', done: 'Reopened ' + s.id + '.' }];
    if (!finished(d)) out.push({ act: 'dispatch', path: p('/dispatch'), word: 'Dispatch', done: 'Handed ' + s.id + ' to the plan’s gardener' });
    out.push({ act: 'rename', word: 'Rename' });
    if (!first) out.push({ act: 'up', path: p('/reorder?dir=up'), word: 'Up', aria: 'Move ' + s.id + ' up', done: 'Moved ' + s.id + ' up.' });
    if (!last) out.push({ act: 'down', path: p('/reorder?dir=down'), word: 'Down', aria: 'Move ' + s.id + ' down', done: 'Moved ' + s.id + ' down.' });
    out.push({ act: 'close', path: p('/close'), word: 'Close', arm: 'Tap again to close ' + named(s), danger: true, done: 'Closed ' + s.id + '.' });
    return out;
  }
  /* A phase's writes: Up and Down in the plan's phase order. */
  function phaseVerbs(d, ph, first, last) {
    if (!d || !d.edit || !ph || !ph.id) return [];
    var p = function (dir) { return path(d.id, '/phases/' + encodeURIComponent(ph.id) + '/reorder?dir=' + dir); }, out = [];
    if (!first) out.push({ act: 'up', path: p('up'), word: 'Up', aria: 'Move phase ' + (ph.title || ph.id) + ' up', done: 'Moved the phase up.' });
    if (!last) out.push({ act: 'down', path: p('down'), word: 'Down', aria: 'Move phase ' + (ph.title || ph.id) + ' down', done: 'Moved the phase down.' });
    return out;
  }
  /* Whom an ask waits on, as the Queues panel says it (web/queue.js); "" when nothing says. */
  var APPROVER = { owner: 'the owner', operator: 'an operator', desk: 'the merge desk' };
  function askedOf(x) { return x.mine ? 'asked of you' : x.asked_of ? 'asked of ' + x.asked_of : ''; }
  function approver(x) { return APPROVER[x.audience] || ''; }
  /* What the plan waits on, a line each: the human asks first, then each bead open steps wait on, once. */
  function waits(d) {
    var out = [];
    (d.decisions || []).forEach(function (x) { if (!x.resolved) out.push({ id: x.id, title: x.title, text: 'decision ' + (askedOf(x) || 'waiting on an answer'), human: true, since: x.since }); });
    (d.escalations || []).forEach(function (x) { out.push({ id: x.id, title: x.title, text: (x.operation ? words(x.operation) : 'escalation') + (approver(x) ? ' for ' + approver(x) + ' to approve' : ' waiting on approval'), human: true, since: x.since }); });
    var by = {};
    groups(d).forEach(function (g) {
      g.steps.forEach(function (s) {
        if (s.closed) return;
        (s.blocked_by || []).forEach(function (b) {
          if (b.escalation) return;   // an escalation is listed above
          var w = by[b.id];
          if (!w) out.push(by[b.id] = w = { id: b.id, title: b.title, step: s.short_title || s.title || s.id, steps: {}, n: 0, human: false });
          if (!w.steps[s.id]) w.steps[s.id] = ++w.n;
          w.text = w.step + (w.n > 1 ? ' and ' + (w.n - 1) + ' more step' + (w.n > 2 ? 's wait' : ' wait') : ' waits') + ' on';
        });
      });
    });
    return out;
  }
  /* A crew member in words: how it is going, its role, the step it is on, how long since it moved. */
  function crewLine(m, now) {
    var out = [m.bucket || words(m.state) || 'unknown'];
    if (m.role) out.push(m.role);
    if (m.step) out.push('on ' + (m.step_n ? '#' + m.step_n + ' ' : '') + (m.step.title || m.step.id));
    if (m.outcome) out.push(m.outcome);
    if (m.since > 0) out.push(span(Math.max(60, now - m.since)) + ' ago');
    return out;
  }
  function crewSum(c) {
    if (!c || !c.total) return 'No crew yet.';
    var bits = [];
    [['working', 'working'], ['starting', 'starting'], ['idle', 'idle'], ['done', 'done'], ['failed', 'failed']].forEach(function (k) { if (c[k[0]]) bits.push(c[k[0]] + ' ' + k[1]); });
    return c.total + (c.total === 1 ? ' agent' : ' agents') + (bits.length ? ': ' + bits.join(', ') : '') + '.';
  }
  /* The pour's JSON body (POST /plans/<id>/pours, plan_pour.go); blank variables are left to the formula's defaults. */
  function pourBody(formula, vars, title, preview) {
    var v = {};
    Object.keys(vars || {}).forEach(function (k) { var x = String(vars[k] == null ? '' : vars[k]).trim(); if (x) v[k] = x; });
    var b = { formula_id: formula, vars: v, dry_run: !!preview };
    if (title && String(title).trim()) b.phase_title = String(title).trim();
    return b;
  }
  /* The variables a pour still needs: each required one with no value and no default. */
  function missing(f, vars) {
    return ((f && f.vars) || []).filter(function (v) { return v.required && !v.default && !String((vars || {})[v.name] || '').trim(); }).map(function (v) { return v.name; });
  }
  /* A preview's steps as lines, nested ones indented: [{depth, text}]. */
  function previewLines(steps, depth, out) {
    out = out || []; depth = depth || 0;
    (steps || []).forEach(function (s) { out.push({ depth: depth, text: (s.title || s.id) + (s.type && s.type !== 'task' ? ' (' + s.type + ')' : '') }); previewLines(s.children, depth + 1, out); });
    return out;
  }
  var api = { url: url, path: path, stateLine: stateLine, progress: progress, levers: levers, groups: groups, stepLine: stepLine, stepVerbs: stepVerbs, phaseVerbs: phaseVerbs, askedOf: askedOf, approver: approver, waits: waits, crewLine: crewLine, crewSum: crewSum, pourBody: pourBody, missing: missing, previewLines: previewLines };
  if (typeof document === 'undefined' || typeof WORKBENCH === 'undefined') return api;

  /* ---------- DOM ---------- */
  var W = WORKBENCH, B = document.body, base = B.dataset.base || '';
  var titleEl = document.getElementById('bv-title'), kindEl = document.getElementById('bv-kind'), bodyEl = document.getElementById('bv-body');
  if (!bodyEl) return api;
  var noteEl = null, last = null, lastAt = 0, sending = false, flash = '', opened = {}, folds = 0;   // flash: the last write's words, kept over its refetch; opened: step id -> its phone verbs shown

  function el(t, cls, text) { var e = document.createElement(t); if (cls) e.className = cls; if (text !== undefined) e.textContent = text; return e; }
  function href(id) { return base + '/garden?bead=' + encodeURIComponent(id); }
  function cur() { var c = W.current(); return c && c.kind === 'plan' ? c.id : ''; }
  function known(id) { return window.GARDEN && GARDEN.known ? GARDEN.known(id) : { id: id }; }
  function say(msg, err) { if (!noteEl) return; noteEl.textContent = msg; noteEl.className = 'bv-note' + (err ? ' err' : ''); }
  function sec(title, node) { var s = el('section', 'bv-sec pv-sec'); s.appendChild(el('h3', '', title)); if (node) s.appendChild(node); return s; }
  function link(id, text, cls) {
    var a = el('a', cls || 'bl', text || id); a.href = href(id); a.title = text && text !== id ? text + ' (' + id + ')' : id;
    if (text && text !== id && typeof TITLES !== 'undefined') TITLES.learn(id, text);
    return a;
  }
  function btn(text, cls) { var b = el('button', 'btn small' + (cls ? ' ' + cls : ''), text); b.type = 'button'; return b; }
  function refresh() { var id = cur(); if (id) W.refresh('plan', id); }
  /* One write as the viewer, JSON asked for (web/answer.js post): the panel says how it went and refetches. An arm verb
     answers on its second tap. */
  function send(v, b, onDone) {
    if (v.arm && !b.classList.contains('armed')) {
      b.textContent = v.arm; b.classList.add('armed');
      setTimeout(function () { if (b.classList.contains('armed')) { b.classList.remove('armed'); b.textContent = v.word; } }, 4000);
      return;
    }
    b.classList.remove('armed');
    var row = b.parentNode, bs = row ? row.querySelectorAll('button') : [b];
    Array.prototype.forEach.call(bs, function (x) { x.disabled = true; });
    sending = true;
    say('Sending…');
    ANSWER.post(v.path, v.params || {}).then(function (j) {
      sending = false;
      say(flash = v.done + (v.act === 'dispatch' ? (j && j.agent ? ' (' + j.agent + ').' : '.') : ''));
      if (onDone) onDone();
      refresh();
    }, function (err) {
      sending = false;
      Array.prototype.forEach.call(bs, function (x) { x.disabled = false; });
      b.textContent = v.word;
      say(err.message || 'That did not go through.', true);
    });
  }
  function verbRow(vs, cls, extra) {
    var row = el('div', 'bv-acts ' + (cls || ''));
    vs.forEach(function (v) {
      var b = btn(v.word, v.act === 'build' ? '' : v.danger ? 'ghost danger' : 'ghost');   // Approve and build is the one call to action
      b.dataset.pv = v.act;
      if (v.aria) b.setAttribute('aria-label', v.aria);
      b.onclick = function () { if (extra && extra[v.act]) extra[v.act](b); else send(v, b); };
      row.appendChild(b);
    });
    return row;
  }
  /* A one-field form in place (Rename, Add, Edit): Save posts it, Cancel puts back what it replaced. */
  function fieldForm(o) {
    var f = el('form', 'bw-edit pv-form'), lab = el('label', 'bw-l', o.label), x = el(o.multi ? 'textarea' : 'input', o.multi ? 'bw-text' : 'bw-in');
    if (!o.multi) x.type = 'text';
    x.name = o.name; x.value = o.value || ''; x.defaultValue = o.keep ? '' : (o.value || ''); x.maxLength = o.max || 500;
    if (o.multi) x.rows = o.rows || 8;
    if (o.placeholder) x.placeholder = o.placeholder;
    lab.appendChild(x); f.appendChild(lab);
    if (o.more) f.appendChild(o.more);
    var row = el('div', 'bv-acts'), save = btn(o.save || 'Save'), cancel = btn('Cancel', 'ghost');
    save.type = 'submit'; row.appendChild(save);
    if (o.cancel) { cancel.onclick = o.cancel; row.appendChild(cancel); }
    f.appendChild(row);
    x.addEventListener('keydown', function (e) { if (e.key !== 'Escape') e.stopPropagation(); else if (o.cancel) { e.preventDefault(); e.stopPropagation(); o.cancel(); } });
    f.addEventListener('submit', function (e) {
      e.preventDefault();
      var val = x.value.trim();
      if (!val && !o.allowEmpty) { x.focus(); return; }
      var params = {}; params[o.name] = val;
      if (o.params) Object.keys(o.params()).forEach(function (k) { params[k] = o.params()[k]; });
      save.disabled = true; cancel.disabled = true; sending = true;
      say('Sending…');
      ANSWER.post(o.path, params).then(function () {
        sending = false;
        say(flash = o.done);
        x.value = ''; x.defaultValue = '';
        if (o.cancel) o.cancel();
        refresh();
      }, function (err) {
        sending = false; save.disabled = false; cancel.disabled = false;
        say(err.message || 'That did not go through.', true);
      });
    });
    f.field = x;
    return f;
  }

  function stepItem(d, s, first, last, now) {
    var li = el('li', 'qv-row pv-step' + (s.closed ? ' done' : ''));
    li.dataset.id = s.id;
    var h = el('div', 'pv-sh');
    if (s.n) h.appendChild(el('span', 'pv-n', '#' + s.n));
    h.appendChild(link(s.id, s.short_title || s.title || s.id, 'bl qv-t'));
    li.appendChild(h);
    li.appendChild(el('p', 'qv-line', stepLine(s, now).join(' · ')));
    if (s.mr && s.mr.url && /^https?:\/\//.test(s.mr.url)) {
      var mr = el('a', 'bl pv-mr', 'Merge request'); mr.href = s.mr.url; mr.target = '_blank'; mr.rel = 'noopener noreferrer';
      li.appendChild(mr);
    }
    var vs = stepVerbs(d, s, first, last);
    if (vs.length) {
      var row = verbRow(vs, 'qv-acts' + (vs.length > 1 ? ' pv-sacts' : ''), {
        rename: function () {
          row.hidden = true;
          var f = fieldForm({ label: 'Step title', name: 'title', value: s.title, max: 500, path: path(d.id, '/steps/' + encodeURIComponent(s.id) + '/title'), done: 'Renamed ' + s.id + '.',
            cancel: function () { f.remove(); row.hidden = false; } });
          li.appendChild(f); f.field.focus(); f.field.select(); f.scrollIntoView({ block: 'nearest' });   // Save and Cancel too (gd-9r9KyiHJZx)
        }
      });
      li.appendChild(row);
    }
    if (vs.length > 1) {
      /* On a phone the verbs fold under one Actions button a row (rubric C3, C11, gd-kUzzLSjtUc); a desktop shows them
         inline (garden.css .pv-more); a lone Reopen stays inline. */
      var more = btn('Actions', 'ghost pv-more');
      row.id = 'pv-sacts-' + (++folds);
      more.setAttribute('aria-controls', row.id);
      more.setAttribute('aria-label', 'Actions for ' + (s.short_title || s.title || s.id));
      var fold = function (on) { li.classList.toggle('open', on); more.setAttribute('aria-expanded', on ? 'true' : 'false'); if (on) opened[s.id] = true; else delete opened[s.id]; };
      more.onclick = function () { fold(!li.classList.contains('open')); if (li.classList.contains('open')) row.querySelector('button').focus(); };
      fold(!!opened[s.id]);   // a repaint keeps an opened row open
      h.appendChild(more);
    }
    return li;
  }
  function stepsSec(d, now) {
    var s = sec('Steps'), gs = groups(d), phased = (d.phases || []).length;
    gs.forEach(function (g, gi) {
      var box = el('div', 'pv-group');
      if (phased) {
        var gh = el('div', 'pv-gh'), t = el('h4', 'pv-gt');
        t.appendChild(g.phase ? link(g.phase.id, g.title, 'bl') : document.createTextNode(g.title));
        gh.appendChild(t);
        if (g.phase) {
          gh.appendChild(el('span', 'pv-gl', [g.phase.done + ' of ' + g.phase.total + ' closed'].concat(g.phase.closed ? ['phase closed'] : [], g.phase.formula ? ['from ' + g.phase.formula] : []).join(' · ')));
          var pvs = phaseVerbs(d, g.phase, gi === 0, gi === phased - 1);
          if (pvs.length) gh.appendChild(verbRow(pvs, 'pv-gacts'));
        }
        box.appendChild(gh);
      }
      if (!g.steps.length) box.appendChild(el('p', 'bv-empty', g.phase ? 'No steps in this phase yet.' : 'No steps yet.'));
      else {
        var ol = el('ol', 'qv-list pv-steps');
        ol.setAttribute('aria-label', g.title);
        g.steps.forEach(function (st, i) { ol.appendChild(stepItem(d, st, i === 0, i === g.steps.length - 1, now)); });
        box.appendChild(ol);
      }
      s.appendChild(box);
    });
    if (d.more) s.appendChild(el('p', 'bv-note', 'More steps than the garden lists here.'));
    if (d.edit && !finished(d)) {
      var sel = null;
      if (phased) {
        var pl = el('label', 'bw-l', 'In phase');
        sel = el('select', 'bw-in');
        sel.name = 'phase';
        var none = el('option', '', '(no phase)'); none.value = ''; sel.appendChild(none);
        (d.phases || []).forEach(function (ph) { var o = el('option', '', ph.title || ph.id); o.value = ph.id; sel.appendChild(o); });
        var cur1 = (d.phases || []).filter(function (ph) { return !ph.closed; })[0];   // the current phase, the first not closed
        if (cur1) sel.value = cur1.id;
        pl.appendChild(sel);
      }
      s.appendChild(fieldForm({ label: 'Add a step', name: 'title', keep: true, max: 500, placeholder: 'What needs doing', save: 'Add step', path: path(d.id, '/steps'), done: 'Added the step.', more: pl || null,
        params: function () { return sel ? { phase: sel.value } : {}; } }));
      s.appendChild(fieldForm({ label: 'Add a phase', name: 'title', keep: true, max: 500, placeholder: 'Phase title', save: 'Add phase', path: path(d.id, '/phases'), done: 'Added the phase.' }));
    }
    return s;
  }
  function waitsSec(d, now) {
    var ws = waits(d);
    if (!ws.length) return null;
    var ul = el('ul', 'pm-waits pv-waits');
    ws.forEach(function (w) {
      var li = el('li', w.human ? 'ask' : 'wtg');
      li.appendChild(el('span', 'wt', w.text)); var wa = li.appendChild(link(w.id, w.title || w.id));
      if (typeof TITLES !== 'undefined') TITLES.unseen(wa, w.id, w.title);   // gd-im6yFl7Nk1
      if (w.since > 0) li.appendChild(el('span', 'sm', span(Math.max(60, now - w.since)) + ' ago'));
      ul.appendChild(li);
    });
    return sec('Waiting on', BEAD.foldList(ul));
  }
  function asksSec(d, now) {
    var open = (d.decisions || []).filter(function (x) { return !x.resolved; }), done = (d.decisions || []).filter(function (x) { return x.resolved; });
    var esc = d.escalations || [];
    if (!open.length && !done.length && !esc.length) return null;
    var s = sec('Decisions and escalations'), ul = el('ul', 'qv-list');
    open.forEach(function (x) {
      var li = el('li', 'qv-row needs');
      li.dataset.id = x.id;
      li.appendChild(link(x.id, x.title || x.id, 'bl qv-t'));
      li.appendChild(el('p', 'qv-line', ['decision', askedOf(x), x.requested_by ? 'asked by ' + x.requested_by : '', x.since > 0 ? 'waiting ' + span(Math.max(60, now - x.since)) : ''].filter(Boolean).join(' · ')));
      if (x.prompt) li.appendChild(el('p', 'bv-text qv-text', x.prompt));
      if (typeof BEAD !== 'undefined' && BEAD.answerBox) li.appendChild(BEAD.answerBox({ kind: 'decision', id: x.id, title: x.title, options: x.options || [] }));
      ul.appendChild(li);
    });
    esc.forEach(function (x) {
      // one this viewer may not approve waits on, and names, its approver
      var li = el('li', 'qv-row' + (d.approve ? ' needs' : '')), who = approver(x) || 'the owner';
      li.dataset.id = x.id;
      li.appendChild(link(x.id, x.title || x.id, 'bl qv-t'));
      li.appendChild(el('p', 'qv-line', ['escalation', x.operation ? words(x.operation) : '', 'for ' + who, x.sensitivity || '', x.since > 0 ? 'waiting ' + span(Math.max(60, now - x.since)) : ''].filter(Boolean).join(' · ')));
      if (!d.approve) li.appendChild(el('p', 'qv-who', who.replace(/^./, function (c) { return c.toUpperCase(); }) + ' approves or rejects this; your account cannot.'));
      if (d.approve && typeof BEAD !== 'undefined' && BEAD.answerBox) li.appendChild(BEAD.answerBox({ kind: 'escalation', id: x.id, title: x.title }));
      ul.appendChild(li);
    });
    if (ul.children.length) s.appendChild(ul);
    if (done.length) {
      var det = el('details', 'pv-done'), sum = el('summary', '', 'Answered (' + done.length + ')'), dl = el('ul', 'qv-list');
      det.appendChild(sum);
      done.forEach(function (x) {
        var li = el('li', 'qv-row');
        li.appendChild(link(x.id, x.title || x.id, 'bl qv-t'));
        li.appendChild(el('p', 'qv-line', x.chosen ? 'answered: ' + x.chosen : 'answered'));
        dl.appendChild(li);
      });
      det.appendChild(dl); s.appendChild(det);
    }
    return s;
  }
  /* The plan's spend from the farm document (web/plans.js costOf), the figure its tile shows; null when it holds none. */
  function spendLine(id) {
    var d = window.GARDEN && GARDEN.farm(), c = typeof PLANS !== 'undefined' && PLANS.spendShown(d) && PLANS.costOf(d, id);
    return c ? el('p', 'bv-note pv-spend' + (c.stale ? ' stale' : ''), 'Spend: ' + PLANS.spendText(c)) : null;
  }
  function crewSec(d, now) {
    var c = d.crew, s = sec('Crew');
    s.classList.add('pv-crew');
    s.appendChild(el('p', 'bv-note', crewSum(c)));
    var sp = spendLine(d.id); if (sp) s.appendChild(sp);
    if (!c || !(c.members || []).length) return s;
    var ul = el('ul', 'qv-list');
    c.members.forEach(function (m) {
      var li = el('li', 'qv-row' + (m.bucket === 'failed' ? ' needs' : ''));
      li.appendChild(m.id ? link(m.id, m.name || m.id, 'bl qv-t') : el('span', 'qv-t', m.name));
      li.appendChild(el('p', 'qv-line', crewLine(m, now).join(' · ')));
      ul.appendChild(li);
    });
    s.appendChild(ul);
    return s;
  }
  /* A block of markdown with Edit beside it (when the viewer may write and it was not cut for the panel). */
  function prose(text, empty, edit) {
    var box = el('div', 'pv-prose'), view = el('div', 'bv-md');
    if (text && String(text).trim()) { if (BEAD.mdInto) BEAD.mdInto(view, BEAD.markdown(text)); else view.textContent = text; }
    else view.appendChild(el('p', 'bv-empty', empty));
    box.appendChild(view);
    if (edit) {
      var row = el('div', 'bv-acts'), b = btn('Edit', 'ghost');
      b.setAttribute('aria-label', edit.aria);
      b.onclick = function () {
        view.hidden = true; row.hidden = true;
        var f = fieldForm({ label: edit.label, name: edit.name, value: text || '', multi: true, rows: 12, max: 200000, allowEmpty: edit.allowEmpty, path: edit.path, done: edit.done,
          cancel: function () { f.remove(); view.hidden = false; row.hidden = false; } });
        box.appendChild(f); f.field.focus();
      };
      row.appendChild(b); box.appendChild(row);
    }
    return box;
  }
  function narrativeSec(d) {
    var n = d.narrative || {}, s = sec('Narrative'), can = d.edit;
    s.appendChild(prose(n.summary, 'No summary yet.', can && !n.summary_clipped ? { aria: 'Edit the plan’s summary', label: 'Summary (markdown)', name: 'narrative', allowEmpty: true, path: path(d.id, '/narrative'), done: 'Saved the summary.' } : null));
    if (n.summary_clipped) s.appendChild(el('p', 'bv-note', 'The summary is longer than the garden shows.'));
    (n.docs || []).forEach(function (x) {
      var h = el('h4', 'pv-dt');
      h.appendChild(link(x.id, x.headline || x.id, 'bl'));
      if (x.format) h.appendChild(el('span', 'pv-gl', ' ' + x.format));
      s.appendChild(h);
      if (x.url && !x.content) {
        if (/^https?:\/\//.test(x.url)) { var a = el('a', 'bl', 'Read it'); a.href = x.url; a.target = '_blank'; a.rel = 'noopener noreferrer'; s.appendChild(a); }
        return;
      }
      var md = !x.format || /^(markdown|md|text|plain)$/i.test(x.format);
      s.appendChild(md ? prose(x.content, 'This document is empty.', can && !x.clipped ? { aria: 'Edit ' + (x.headline || x.id), label: (x.headline || 'Document') + ' (markdown)', name: 'content', path: path(d.id, '/docs/' + encodeURIComponent(x.id)), done: 'Saved ' + (x.headline || x.id) + '.' } : null)
        : el('p', 'bv-note', 'A ' + x.format + ' document: open it to read it.'));
      if (x.clipped) s.appendChild(el('p', 'bv-note', 'This document is longer than the garden shows.'));
    });
    return s;
  }
  /* Pour a formula as a new phase (WB-8): the fold fetches the project's formulas, then the chosen one's variables;
     Preview shows the steps it would add, Pour adds them. */
  function pourSec(d) {
    var det = el('details', 'pv-pour'), sum = el('summary', '', 'Pour a formula'), body = el('div', 'pv-pourbody');
    det.appendChild(sum); det.appendChild(body);
    var loaded = false;
    det.addEventListener('toggle', function () {
      if (!det.open || loaded) return;
      loaded = true;
      body.textContent = '';
      body.appendChild(el('p', 'bv-note', 'Loading the formulas…'));
      fetch(base + url(d.id) + '/pour', { credentials: 'same-origin', redirect: 'manual', cache: 'no-store', headers: { Accept: 'application/json' } })
        .then(function (r) { if (!r.ok) throw new Error(W.failure(r.type === 'opaqueredirect' ? 'redirect' : r.status, 'formula list', 'the formulas').text); return r.json(); }, function () { throw new Error(W.failure(0, 'formula list', 'the formulas').text); })
        .then(function (p) { pourForm(d, body, p.formulas || []); }, function (err) { loaded = false; body.textContent = ''; body.appendChild(el('p', 'bv-note err', err.message)); });
    });
    return sec('Pour', det);
  }
  function pourForm(d, body, fs) {
    body.textContent = '';
    if (!fs.length) { body.appendChild(el('p', 'bv-empty', 'No formula in ' + (d.project || 'this land') + ' to pour.')); return; }
    var f = el('form', 'bw-edit pv-form'), lab = el('label', 'bw-l', 'Formula'), sel = el('select', 'bw-in'), vars = el('div', 'pv-vars');
    sel.name = 'formula';
    var pick = el('option', '', 'Choose a formula'); pick.value = ''; sel.appendChild(pick);
    fs.forEach(function (x) { var o = el('option', '', (x.title || x.id) + (x.steps ? ' (' + x.steps + ' steps)' : '')); o.value = x.id; sel.appendChild(o); });
    lab.appendChild(sel); f.appendChild(lab); f.appendChild(vars);
    var tl = el('label', 'bw-l', 'Phase title (optional)'), ti = el('input', 'bw-in');
    ti.type = 'text'; ti.maxLength = 300; ti.placeholder = 'The formula’s title'; tl.appendChild(ti); f.appendChild(tl);
    var row = el('div', 'bv-acts'), pre = btn('Preview', 'ghost'), go = btn('Pour');
    go.type = 'submit'; row.appendChild(pre); row.appendChild(go); f.appendChild(row);
    var out = el('div', 'pv-preview'); f.appendChild(out);
    var chosen = function () { return fs.filter(function (x) { return x.id === sel.value; })[0] || null; };
    var values = function () { var v = {}; Array.prototype.forEach.call(vars.querySelectorAll('[data-var]'), function (x) { v[x.dataset.var] = x.value; }); return v; };
    sel.onchange = function () {
      vars.textContent = ''; out.textContent = '';
      var x = chosen();
      ((x && x.vars) || []).forEach(function (v) {
        var l = el('label', 'bw-l', v.name + (v.required && !v.default ? ' (required)' : '')), i;
        if ((v.enum || []).length) { i = el('select', 'bw-in'); v.enum.forEach(function (e) { var o = el('option', '', e); o.value = e; i.appendChild(o); }); if (v.default) i.value = v.default; }
        else { i = el('input', 'bw-in'); i.type = 'text'; i.maxLength = 2000; i.placeholder = v.default || ''; }
        i.dataset.var = v.name;
        l.appendChild(i);
        if (v.note) l.appendChild(el('span', 'bw-h', v.note));
        vars.appendChild(l);
      });
    };
    var run = function (preview) {
      var x = chosen();
      if (!x) { say('Choose a formula to pour.', true); sel.focus(); return; }
      var miss = missing(x, values());
      if (miss.length) { say('Fill in ' + miss.join(', ') + ' first.', true); return; }
      pre.disabled = true; go.disabled = true; sending = true;
      say(preview ? 'Asking for a preview…' : 'Pouring…');
      fetch(base + path(d.id, '/pours'), { method: 'POST', credentials: 'same-origin', redirect: 'manual', headers: { 'Content-Type': 'application/json', Accept: 'application/json' }, body: JSON.stringify(pourBody(x.id, values(), ti.value, preview)) })
        .then(function (r) {
          if (r.type === 'opaqueredirect') throw new Error('Your session ended. Reload the page to sign in again.');
          return r.json().catch(function () { return null; }).then(function (j) {
            if (!r.ok || (j && j.ok === false)) throw new Error((j && j.error) || 'The pour did not go through (' + r.status + ').');
            return j || {};
          });
        })
        .then(function (j) {
          sending = false; pre.disabled = false; go.disabled = false;
          out.textContent = '';
          if (j.warning) out.appendChild(el('p', 'bv-note', j.warning));
          if (preview) {
            var ls = previewLines(j.step_plan);
            say('Pouring ' + (x.title || x.id) + ' would add ' + (j.total_steps || ls.length) + ' steps.');
            var ul = el('ul', 'pv-plan');
            ls.forEach(function (l) { var li = el('li', '', l.text); li.style.marginLeft = (l.depth * 14) + 'px'; ul.appendChild(li); });
            out.appendChild(ul);
            return;
          }
          say(flash = 'Poured ' + (x.title || x.id) + ': ' + (j.total_steps || 0) + ' steps added' + (j.molecule ? ' as ' + j.molecule : '') + '.');
          sel.value = ''; sel.onchange(); ti.value = '';
          refresh();
        }, function (err) { sending = false; pre.disabled = false; go.disabled = false; say(err.message || 'The pour did not go through.', true); });
    };
    pre.onclick = function () { run(true); };
    f.addEventListener('submit', function (e) { e.preventDefault(); run(false); });
    f.addEventListener('keydown', function (e) { if (e.key !== 'Escape') e.stopPropagation(); });
    body.appendChild(f);
  }

  function paint(id, d, note, isErr, retry) {
    var k = known(id);
    kindEl.textContent = 'plan';
    titleEl.textContent = (d && (d.short_title || d.title)) || k.title || id;
    bodyEl.textContent = '';
    noteEl = el('p', 'bv-note' + (isErr ? ' err' : ''), note || (d ? flash : ''));
    if (d) flash = '';
    noteEl.setAttribute('role', 'status');
    if (isErr && retry) {
      var rb = btn('Try again', 'bv-retry');
      rb.onclick = function () { rb.disabled = true; rb.textContent = 'Loading…'; W.reload('plan', id); };
      noteEl.appendChild(document.createTextNode(' ')); noteEl.appendChild(rb);
    }
    bodyEl.appendChild(noteEl);
    if (!d) {
      // what the map already knows (WB-15): the plan's bead view, where its plot's progress and waits are drawn
      if (isErr && W.has('bead')) { var a = el('a', 'bl', 'Open its bead view'); a.href = href(id); bodyEl.appendChild(el('p', 'bv-meta')).appendChild(a); }
      return;
    }
    var now = Math.floor(Date.now() / 1000), meta = el('div', 'bv-meta pv-meta');
    meta.appendChild(el('p', 'pv-state' + (d.state === 'ready' ? ' ask' : ''), stateLine(d)));
    meta.appendChild(el('p', 'qv-line', progress(d, now)));
    var bead = el('a', 'bl', 'Its bead view'), line = meta.appendChild(el('p', 'qv-line')), wb = el('a', 'bl', 'Open in workbench');
    bead.href = href(d.id); bead.title = d.id; wb.href = base + '/garden/c/n/' + encodeURIComponent(d.id);   // web/commons2.js
    line.append(bead, ' · ', wb);
    if (d.edit) {
      var old = el('a', 'bl', 'Critique, assistant and gardener: old viewer'); old.href = base + '/plans/' + encodeURIComponent(d.id) + '?old=1';
      meta.appendChild(el('p', 'qv-line')).appendChild(old);
    }
    bodyEl.appendChild(meta);
    var lv = levers(d);
    if (lv.length) bodyEl.appendChild(verbRow(lv, 'pv-levers'));
    var w = waitsSec(d, now); if (w) bodyEl.appendChild(w);
    bodyEl.appendChild(stepsSec(d, now));
    var a2 = asksSec(d, now); if (a2) bodyEl.appendChild(a2);
    bodyEl.appendChild(crewSec(d, now));
    bodyEl.appendChild(narrativeSec(d));
    if (d.edit && !finished(d)) bodyEl.appendChild(pourSec(d));
    if (typeof TITLES !== 'undefined') TITLES.dress(bodyEl);
  }
  /* A refresh now would lose the viewer's work: a form open, a button armed or sending, a typed draft, the pour fold open. */
  function hold() {
    if (sending) return true;
    if (bodyEl.querySelector('.armed, .pv-prose form, .pv-step form, .pv-pour[open] [data-var], .act [disabled]')) return true;
    if (bodyEl.querySelector('.pv-pour[open] select') && bodyEl.querySelector('.pv-pour[open] select').value) return true;
    return Array.prototype.some.call(bodyEl.querySelectorAll('textarea, input'), function (x) { return !!x.value && x.value !== x.defaultValue; });
  }
  function liveNote(text, act, fn) {
    var n = bodyEl.querySelector('.bv-livenote');
    if (!n) { n = el('div', 'bv-note bv-livenote'); n.setAttribute('role', 'status'); bodyEl.insertBefore(n, noteEl ? noteEl.nextSibling : bodyEl.firstChild); }
    n.textContent = '';
    n.appendChild(el('span', '', text + ' '));
    if (act) { var b = btn(act); b.onclick = fn; n.appendChild(b); }
    return n;
  }

  W.register('plan', {
    noun: 'plan',
    url: url,
    paint: paint,
    done: function (id, d) { last = d; if (d) lastAt = Math.floor(Date.now() / 1000); },
    live: function (id) { return url(id) + '/events'; },   // plan.go: "moved" when anything the cockpit draws moves (WB-14)
    hold: hold,
    stale: function () { liveNote('This plan changed while you were working here.', 'Show the latest', refresh); },
    failed: function (id, text, retry) {
      var n = liveNote(W.refreshFailure(text, 'plan') + ' It shows the plan as it was ' + (Date.now() / 1000 - lastAt >= 60 ? span(Math.floor(Date.now() / 1000) - lastAt) + ' ago' : 'a moment ago') + '.');
      n.classList.add('err');
      if (retry) { var b = btn('Try again', 'bv-retry'); b.onclick = function () { b.disabled = true; b.textContent = 'Loading…'; refresh(); }; n.appendChild(b); }
    },
    closed: function () { last = null; noteEl = null; sending = false; flash = ''; }
  });

  /* The farm document moved (web/plans.js announce): redraw the spend line in place. */
  document.addEventListener('farmdoc', function () {
    var id = cur(), crew = id && bodyEl.querySelector('.pv-crew'), sp;
    if (!crew) return;
    if ((sp = crew.querySelector('.pv-spend'))) sp.remove();
    if ((sp = spendLine(id))) crew.querySelector('.bv-note').after(sp);
  });
  /* An answer sent from a decision or escalation here (web/answer.js) moves the plan: show it. */
  document.addEventListener('garden:answered', function () { if (cur()) refresh(); });
  return api;
})();
if (typeof module !== 'undefined') module.exports = PLAN;
