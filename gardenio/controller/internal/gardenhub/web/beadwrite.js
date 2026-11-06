/* The bead view's writes (Lands workbench-glass spec WB-2, WB-3, WB-4, WB-12; Phase 5 step 5.4, gd-PXHQecYeDU): the in-garden
   bead panel (web/workbench.js) edits a bead in place, takes a comment, closes, reopens, claims and releases it, sets its
   parent, adds and removes what it waits on and holds up, and puts an agent on it. Every write is the viewer's own POST
   route, asked for JSON (W-D2: /b/<id>/{title,description,priority,assignee,owner,labels,status,comments,close,reopen,claim,
   release,parent,deps,deps/remove} and /quick-spawn, action_json.go), so permission, audit and attribution stay those
   routes'. The markdown previews are the viewer's own renders (/b/<id>/{description,comments}/preview), server-sanitized,
   their bead links pointed back into the garden. After a write the panel fetches the bead again (WORKBENCH.reload) and says
   what happened in a status line, kept across that repaint.

   The page offers the edits only to a viewer the server says may write (body data-edit), and "Put an agent on this" only on
   a bead whose land lets the viewer spawn (the document's spawn: owner and members, not guests, Q8c); the routes check both
   again. Every empty composer shows the house placeholder (body data-empty-prompt, voice.go). Text goes in through
   textContent only. */
var BEADWRITE = (function () {
  /* ---------- pure logic (also run under node by the Go tests) ---------- */
  /* The inline status editor's states (bead_actions.go statusOptions) and the priorities it takes (0 highest). */
  var STATUSES = ['open', 'in_progress', 'closed'];
  var PRIORITIES = [0, 1, 2, 3, 4, 5];
  var STATE_WORDS = { open: 'planted (open)', in_progress: 'growing (in progress)', closed: 'closed' };
  /* The editable fields, in the form's order: the key in the bead document, its label, the route under /b/<id>/ and the form
     field it posts. */
  var FIELDS = [
    { k: 'title', label: 'Title', path: 'title', name: 'title' },
    { k: 'status', label: 'State', path: 'status', name: 'status' },
    { k: 'priority', label: 'Priority', path: 'priority', name: 'priority' },
    { k: 'assignee', label: 'Assignee', path: 'assignee', name: 'assignee' },
    { k: 'owner', label: 'Owner', path: 'owner', name: 'owner' },
    { k: 'labels', label: 'Labels', path: 'labels', name: 'labels' },
    { k: 'description', label: 'Description', path: 'description', name: 'description' }
  ];
  /* A field's value as the form shows it: labels comma-separated, the priority as its digit, the rest as written. */
  function value(d, k) {
    var v = (d || {})[k];
    if (k === 'labels') return (v || []).join(', ');
    if (k === 'priority') return v === undefined || v === null ? '' : String(v);
    return v == null ? '' : String(v);
  }
  /* The fields the form changed, each {k, label, path, name, value}, in FIELDS order. Values compare trimmed (a description
     is sent as typed, its line breaks kept); labels as a set of names, so reordering or spacing them is no change. A clipped
     description (bead.go description_clipped) is never sent. */
  function setOf(s) { return String(s || '').split(/[,\n]/).map(function (x) { return x.trim(); }).filter(Boolean).sort().join(','); }
  function changes(d, form) {
    var out = [];
    FIELDS.forEach(function (f) {
      if (!(f.k in (form || {}))) return;
      if (f.k === 'description' && (d || {}).description_clipped) return;
      var was = value(d, f.k), now = String(form[f.k] == null ? '' : form[f.k]);
      var same = f.k === 'labels' ? setOf(was) === setOf(now) : was.trim() === now.trim();
      if (!same) out.push({ k: f.k, label: f.label, path: f.path, name: f.name, value: f.k === 'description' ? now : now.trim() });
    });
    return out;
  }
  /* Whether the bead is the viewer's: me is the page's data-me ("oidc:<email>", lowercase); a claim writes the bare email. */
  function mine(assignee, me) {
    var a = String(assignee || '').toLowerCase().trim();
    return !!a && !!me && (a === me || 'oidc:' + a === me || a === 'oidc:' + me);
  }
  /* The lifecycle verbs the panel offers, as bead_actions.go buildBeadActions does: a closed bead reopens; an open one closes
     (not an escalation, which its own Approve and Reject settle) and is claimed, or released by the one holding it. */
  function verbs(d, me) {
    d = d || {};
    if (!d.id || d.type === 'agent') return [];
    if (d.status === 'closed') return ['reopen'];
    var out = [];
    if (d.type !== 'escalation') out.push('close');
    out.push(mine(d.assignee, me) ? 'release' : 'claim');
    return out;
  }
  var VERB = { close: 'Close…', reopen: 'Reopen', claim: 'Claim', release: 'Release' };
  var DONE = { close: 'Closed', reopen: 'Reopened', claim: 'Claimed', release: 'Released' };
  /* A bead id as a person pastes it: two letters, a dash and its letters and digits (kd's ids). */
  function isId(s) { return /^[a-z]{2}-[A-Za-z0-9]{3,40}$/.test(String(s || '').trim()); }
  /* The relations a link form writes: what the select says, where it posts and its form. */
  var LINKS = [
    { k: 'waits', label: 'Waits on', path: 'deps', form: function (o) { return { other: o, dir: 'waits' }; } },
    { k: 'holds', label: 'Holds up', path: 'deps', form: function (o) { return { other: o, dir: 'holds' }; } },
    { k: 'parent', label: 'Part of (parent)', path: 'parent', form: function (o) { return { parent_id: o }; } }
  ];
  function linkOf(k) { return LINKS.filter(function (l) { return l.k === k; })[0] || null; }
  /* The remove a relation row offers: a waits-on row ("blocked_by") and a holds-up row ("blocks"), with the form that
     removes that edge; none for parents, children or the rest. */
  function unlink(key, other) {
    if (key === 'blocked_by') return { path: 'deps/remove', form: { other: other, dir: 'waits' }, word: 'Stop waiting on' };
    if (key === 'blocks') return { path: 'deps/remove', form: { other: other, dir: 'holds' }, word: 'Stop holding up' };
    return null;
  }
  /* How many comments to ask for to show older ones: forty more, up to bead.go's beadCommentsAll; 0 when every one is shown. */
  var STEP = 40, ALL = 400;
  function older(shown, total) { return total > shown && shown < ALL ? Math.min(shown + STEP, ALL, total) : 0; }
  /* What a write's answer says went wrong, in words, or '' when it went through (answer.js's reading of action_json.go). */
  var WHY = { 401: 'Your session ended. Reload the page to sign in again.', 403: 'You may not do that here.', 404: 'That bead is gone, or you cannot see it.', 409: 'Someone changed it first. Close and reopen to see it as it is.', 413: 'That is too long.', 502: 'The beads daemon did not answer. Try again.', 503: 'Writing is not enabled here.' };
  function failure(status, j) {
    if (j && j.ok === true) return '';
    if (status >= 200 && status < 300 && !j) return '';
    var m = j && typeof j.error === 'string' ? j.error.replace(/\s+/g, ' ').trim() : '', code = (j && j.status) || status;
    return m || WHY[code] || 'That did not go through (' + code + ').';
  }
  /* The quick-spawn form for bead d: its land, the bead as the task, and what the viewer picked (an empty role or model keeps
     the viewer's default role and the role's own model). */
  function spawnForm(d, pick) {
    pick = pick || {};
    var f = { project: d.project || '', task_id: d.id, prompt: String(pick.prompt || '').trim() };
    if (pick.role) f.role = pick.role;
    if (pick.model) f.model = pick.model;
    return f;
  }
  /* Who may be offered "Put an agent on this": a bead whose document says the viewer may spawn in its land, not a gardener. */
  function spawnable(d) { return !!(d && d.id && d.spawn && d.project && d.type !== 'agent'); }
  /* A preview's links, pointed back into the garden: a link to the viewer's bead page (<base>/<id>) opens the bead here. */
  function repoint(base, href, origin) {
    var u;
    try { u = new URL(href, origin + '/'); } catch (e) { return ''; }
    if (u.origin !== origin) return '';
    var m = new RegExp('^' + (base || '').replace(/[.*+?^${}()|[\]\\]/g, '\\$&') + '/([a-z]{2}-[A-Za-z0-9]{3,40})$').exec(u.pathname);
    return m ? (base || '') + '/garden?bead=' + encodeURIComponent(m[1]) : '';
  }

  var api = { STATUSES: STATUSES, PRIORITIES: PRIORITIES, FIELDS: FIELDS, LINKS: LINKS, value: value, changes: changes, mine: mine, verbs: verbs, isId: isId, linkOf: linkOf, unlink: unlink, older: older, failure: failure, spawnForm: spawnForm, spawnable: spawnable, repoint: repoint,
    mount: function () {}, links: function () { return null; }, rel: function () {}, composer: function () { return null; } };
  if (typeof document === 'undefined') return api;

  /* ---------- DOM ---------- */
  var B = document.body, base = B.dataset.base || '', me = (B.dataset.me || '').toLowerCase(), told = {};
  function canEdit() { return B.dataset.edit === '1'; }
  function placeholder() { return B.dataset.emptyPrompt || ''; }
  function el(t, cls, text) { var e = document.createElement(t); if (cls) e.className = cls; if (text !== undefined) e.textContent = text; return e; }
  function button(text, cls, fn) { var b = el('button', cls || 'btn small', text); b.type = 'button'; if (fn) b.onclick = fn; return b; }
  function W() { return typeof WORKBENCH !== 'undefined' ? WORKBENCH : null; }
  function reload(id) { var w = W(); if (w) w.reload('bead', id); }
  function post(path, form) {
    return fetch(base + path, { method: 'POST', credentials: 'same-origin', redirect: 'manual', cache: 'no-store', headers: { 'Content-Type': 'application/x-www-form-urlencoded', Accept: 'application/json' }, body: new URLSearchParams(form || {}).toString() })
      .then(function (r) {
        if (r.type === 'opaqueredirect') throw new Error(WHY[401]);
        return r.json().catch(function () { return null; }).then(function (j) { var e = failure(r.status, j); if (e) throw new Error(e); return j || { ok: true }; });
      }, function () { throw new Error('The garden did not answer. Try again.'); });
  }
  /* Say what happened, and keep it across the repaint the reload brings (the next mount shows it once). */
  function say(note, text, err) { note.textContent = text; note.className = 'bv-note' + (err ? ' err' : ''); if (typeof TITLES !== 'undefined') TITLES.dress(note); }
  function after(id, text) { told[id] = text; reload(id); }
  function busy(root, on) { Array.prototype.forEach.call(root.querySelectorAll('button,input,select,textarea'), function (x) { x.disabled = on; }); }
  function status() { var n = el('p', 'bv-note'); n.setAttribute('role', 'status'); return n; }
  function field(label, input, id) { var w = el('div', 'bw-f'), l = el('label', 'bw-l', label); input.id = id; l.htmlFor = id; w.appendChild(l); w.appendChild(input); return w; }

  /* A markdown draft with Write and Preview: the textarea, and the viewer's own render of it (path, posted as name);
     the toggle is a ghost that says where it goes (gd-IoX1pPN7kI). */
  function draft(id, name, path, text, label) {
    var box = el('div', 'bw-md'), ta = el('textarea', 'bw-text'), pv = el('div', 'bw-pv bv-text'), tog = button('Preview', 'btn small ghost bw-tog');
    ta.value = text || ''; ta.rows = name === 'description' ? 8 : 3; ta.maxLength = name === 'description' ? 65536 : 10000;
    ta.placeholder = placeholder(); ta.setAttribute('aria-label', label);
    pv.hidden = true; pv.setAttribute('aria-live', 'polite'); pv.setAttribute('aria-label', label + ' preview');
    tog.onclick = function () {
      var showing = !pv.hidden;
      if (showing) { pv.hidden = true; ta.hidden = false; tog.textContent = 'Preview'; ta.focus(); return; }
      pv.textContent = 'Rendering…'; pv.hidden = false; ta.hidden = true; tog.textContent = 'Back to writing';
      var body = {}; body[name] = ta.value;
      fetch(base + '/b/' + encodeURIComponent(id) + '/' + path, { method: 'POST', credentials: 'same-origin', redirect: 'manual', cache: 'no-store', headers: { 'Content-Type': 'application/x-www-form-urlencoded' }, body: new URLSearchParams(body).toString() })
        .then(function (r) { if (!r.ok) throw new Error(r.status === 413 ? WHY[413] : 'No preview (' + (r.status || 'no answer') + ').'); return r.text(); })
        .then(function (html) {
          var t = document.createElement('template'); t.innerHTML = html;   // the viewer's own sanitized render (goldmark, no raw HTML)
          var inner = t.content.querySelector('.description, .comment-body') || t.content.querySelector('.empty') || t.content;
          Array.prototype.forEach.call(inner.querySelectorAll('script, style, iframe, object, embed, form'), function (x) { x.remove(); });
          Array.prototype.forEach.call(inner.querySelectorAll('a[href]'), function (a) {
            var to = repoint(base, a.getAttribute('href'), location.origin);
            if (to) { a.href = to; a.removeAttribute('target'); } else { a.target = '_blank'; a.rel = 'noopener noreferrer'; }
          });
          pv.textContent = ''; pv.appendChild(inner);
          if (typeof TITLES !== 'undefined') TITLES.dress(pv);
        }, function (e) { pv.textContent = e.message; });
    };
    box.appendChild(ta); box.appendChild(pv); box.appendChild(tog);
    box.text = ta;
    return box;
  }

  /* The edit form (WB-2): every field, Save sends only what changed, one route at a time, and stops at the first refusal. */
  function editor(host, d, note, done) {
    var form = el('form', 'bw-edit'), ins = {};
    form.setAttribute('aria-label', 'Edit ' + d.id);
    var title = el('input', 'bw-in'); title.value = value(d, 'title'); title.maxLength = 500; ins.title = title;
    form.appendChild(field('Title', title, 'bw-title'));
    var row = el('div', 'bw-row');
    var st = el('select', 'bw-in');
    STATUSES.forEach(function (s) { var o = el('option', '', STATE_WORDS[s]); o.value = s; st.appendChild(o); });
    if (STATUSES.indexOf(d.status) < 0) { var o0 = el('option', '', d.status); o0.value = d.status; st.insertBefore(o0, st.firstChild); }
    st.value = d.status; ins.status = st;
    var pr = el('select', 'bw-in');
    PRIORITIES.forEach(function (p) { var o = el('option', '', 'P' + p + (p === 0 ? ' (highest)' : '')); o.value = String(p); pr.appendChild(o); });
    pr.value = value(d, 'priority'); ins.priority = pr;
    row.appendChild(field('State', st, 'bw-status')); row.appendChild(field('Priority', pr, 'bw-priority'));
    form.appendChild(row);
    ['assignee', 'owner', 'labels'].forEach(function (k) {
      var i = el('input', 'bw-in'); i.value = value(d, k); i.maxLength = 2000; ins[k] = i;
      if (k === 'labels') i.setAttribute('aria-describedby', 'bw-labels-h');
      form.appendChild(field(k.charAt(0).toUpperCase() + k.slice(1), i, 'bw-' + k));
      if (k === 'labels') form.appendChild(el('p', 'bw-h', 'Comma-separated, e.g. project:gardenio, phase:5')).id = 'bw-labels-h';
    });
    var desc = null;
    if (d.description_clipped) form.appendChild(el('p', 'bv-note', 'The description is too long to edit here.'));
    else { desc = draft(d.id, 'description', 'description/preview', d.description || '', 'Description'); form.appendChild(el('p', 'bw-l', 'Description')); form.appendChild(desc); }
    var save = el('button', 'btn small', 'Save'), no = button('Cancel', 'btn small ghost', function () { form.remove(); done(); });
    save.type = 'submit';
    var acts = el('div', 'bv-acts'); acts.appendChild(save); acts.appendChild(no); form.appendChild(acts);
    form.onsubmit = function (e) {
      e.preventDefault();
      var now = {};
      Object.keys(ins).forEach(function (k) { now[k] = ins[k].value; });
      if (desc) now.description = desc.text.value;
      if (!now.title.trim()) { say(note, 'A bead needs a title.', true); title.focus(); return; }
      var todo = changes(d, now);
      if (!todo.length) { say(note, 'Nothing changed.', false); return; }
      busy(form, true); say(note, 'Saving…', false);
      var i = 0;
      (function next() {
        if (i >= todo.length) { after(d.id, 'Saved ' + todo.map(function (c) { return c.label.toLowerCase(); }).join(', ') + '.'); return; }
        var c = todo[i++], body = {}; body[c.name] = c.value;
        post('/b/' + encodeURIComponent(d.id) + '/' + c.path, body).then(next, function (err) {
          busy(form, false);
          var saved = todo.slice(0, i - 1).map(function (x) { return x.label.toLowerCase(); });
          say(note, c.label + ': ' + err.message + (saved.length ? ' (Saved ' + saved.join(', ') + '.)' : ''), true);
          (ins[c.k] || (desc && desc.text) || save).focus();
        });
      })();
    };
    host.appendChild(form);
    title.focus();
  }

  /* Close with a reason (WB-4): the reason, if any, is the viewer's comment before the close (kd close --reason). */
  function closer(host, d, note, done) {
    var form = el('form', 'bw-close'), ta = el('textarea', 'bw-text');
    ta.rows = 2; ta.maxLength = 10000; ta.placeholder = 'Why (optional): done, superseded by …, won’t do';
    var go = el('button', 'btn small danger', 'Close ' + d.id), no = button('Cancel', 'btn small ghost', function () { form.remove(); done(); });
    go.type = 'submit';
    form.appendChild(field('Reason', ta, 'bw-reason'));
    var acts = el('div', 'bv-acts'); acts.appendChild(go); acts.appendChild(no); form.appendChild(acts);
    form.onsubmit = function (e) {
      e.preventDefault(); busy(form, true); say(note, 'Closing…', false);
      post('/b/' + encodeURIComponent(d.id) + '/close', { reason: ta.value.trim() }).then(function () { after(d.id, DONE.close + ' ' + d.id + '.'); },
        function (err) { busy(form, false); say(note, err.message, true); });
    };
    host.appendChild(form);
    ta.focus();
  }

  /* Put an agent on this (WB-12): the viewer's spawn defaults (GET /quick-spawn-inline as JSON: role, prompt rendered against
     the bead), a role, a model alias and the brief; Start posts /quick-spawn and links the new agent here. */
  function spawner(host, d, note, done) {
    var form = el('form', 'bw-spawn');
    form.appendChild(el('p', 'bv-note', 'Loading your spawn settings…'));
    host.appendChild(form);
    fetch(base + '/quick-spawn-inline?format=json&task=' + encodeURIComponent(d.id) + '&project=' + encodeURIComponent(d.project), { credentials: 'same-origin', redirect: 'manual', cache: 'no-store', headers: { Accept: 'application/json' } })
      .then(function (r) { if (r.type === 'opaqueredirect') throw new Error(WHY[401]); if (!r.ok) throw new Error(WHY[r.status] || 'Could not load your spawn settings (' + r.status + ').'); return r.json(); })
      .then(function (cfg) {
        form.textContent = '';
        var row = el('div', 'bw-row'), role = el('select', 'bw-in'), model = el('select', 'bw-in');
        (cfg.roles || ['crew']).forEach(function (x) { var o = el('option', '', x); o.value = x; role.appendChild(o); });
        role.value = cfg.role || 'crew';
        var o0 = el('option', '', 'the role’s own'); o0.value = ''; model.appendChild(o0);
        (cfg.models || []).forEach(function (x) { var o = el('option', '', x); o.value = x; model.appendChild(o); });
        row.appendChild(field('Role', role, 'bw-role')); row.appendChild(field('Model', model, 'bw-model'));
        form.appendChild(row);
        var ta = el('textarea', 'bw-text'); ta.rows = 3; ta.maxLength = 10000; ta.value = cfg.prompt || ''; ta.placeholder = placeholder();
        form.appendChild(field('What should it do?', ta, 'bw-prompt'));
        var go = el('button', 'btn small', 'Start the agent'), no = button('Cancel', 'btn small ghost', function () { form.remove(); done(); });
        go.type = 'submit';
        var acts = el('div', 'bv-acts'); acts.appendChild(go); acts.appendChild(no); form.appendChild(acts);
        form.onsubmit = function (e) {
          e.preventDefault();
          if (!ta.value.trim()) { say(note, 'Say what the agent should do.', true); ta.focus(); return; }
          busy(form, true); say(note, 'Starting an agent on ' + d.id + '…', false);
          post('/quick-spawn', spawnForm(d, { role: role.value, model: model.value, prompt: ta.value })).then(function (j) {
            form.remove();
            note.textContent = 'Started ' + (j.agent || 'an agent') + ' on this bead. ';
            note.className = 'bv-note';
            if (j.bead) { var a = el('a', 'bl', 'Open it'); a.href = base + '/garden?bead=' + encodeURIComponent(j.bead); note.appendChild(a); }
            done(true);
          }, function (err) { busy(form, false); say(note, err.message, true); });
        };
        ta.focus();
      }, function (err) { form.remove(); say(note, err.message, true); done(); });
  }

  /* Into host, for bead document d: the write bar (Edit, the lifecycle verbs, Put an agent on this) and the form one of them
     opens below it. */
  function mount(host, d) {
    if (!host || !d || !d.id) return;
    var edit = canEdit(), vs = edit ? verbs(d, me) : [], spawn = spawnable(d);
    if (!edit && !spawn) return;
    var box = el('div', 'bw'), bar = el('div', 'bv-acts bw-bar'), slot = el('div', 'bw-slot'), note = status();
    box.dataset.bead = d.id;
    if (told[d.id]) { say(note, told[d.id], false); delete told[d.id]; }
    function open(fn) { bar.hidden = true; slot.textContent = ''; fn(slot, d, note, function (keep) { slot.textContent = ''; bar.hidden = false; if (!keep) bar.firstChild.focus(); }); }
    if (edit && d.type !== 'agent') bar.appendChild(button('Edit', 'btn small', function () { open(editor); }));
    var shut = null;   // Close… goes last, apart from the safe verbs and styled as the one that ends the bead (rubric C8)
    vs.forEach(function (v) {
      var b = button(VERB[v], 'btn small' + (v === 'close' ? ' ghost danger' : ''), function () {
        if (v === 'close') { open(closer); return; }
        busy(bar, true); say(note, VERB[v] + 'ing…', false);
        post('/b/' + encodeURIComponent(d.id) + '/' + v, {}).then(function () { after(d.id, DONE[v] + ' ' + d.id + '.'); }, function (err) { busy(bar, false); say(note, err.message, true); });
      });
      b.dataset.w = v;
      if (v === 'close') shut = b; else bar.appendChild(b);
    });
    if (spawn) bar.appendChild(button('Put an agent on this', 'btn small', function () { open(spawner); }));
    if (shut) bar.appendChild(shut);
    if (!bar.children.length) return;
    box.appendChild(bar); box.appendChild(slot); box.appendChild(note);
    host.appendChild(box);
  }

  /* The "Add a link" form under the relation lists (WB-4): waits on, holds up, or part of (its parent), by bead id. */
  function links(d) {
    if (!canEdit() || !d || !d.id || d.type === 'agent') return null;
    var form = el('form', 'bw-links'), kind = el('select', 'bw-in'), other = el('input', 'bw-in'), note = status();
    LINKS.forEach(function (l) { var o = el('option', '', l.label); o.value = l.k; kind.appendChild(o); });
    other.placeholder = 'gd-…'; other.maxLength = 60; other.autocomplete = 'off'; other.spellcheck = false;
    var row = el('div', 'bw-row');
    row.appendChild(field('This bead', kind, 'bw-link-kind')); row.appendChild(field('Bead id', other, 'bw-link-other'));
    var go = el('button', 'btn small', 'Add'); go.type = 'submit';
    form.appendChild(row); form.appendChild(go); form.appendChild(note);
    form.onsubmit = function (e) {
      e.preventDefault();
      var id = other.value.trim(), l = linkOf(kind.value);
      if (!isId(id)) { say(note, 'That is not a bead id. Paste one like gd-AbC123xyz0.', true); other.focus(); return; }
      if (id === d.id) { say(note, 'A bead cannot link to itself.', true); other.focus(); return; }
      busy(form, true); say(note, 'Linking…', false);
      post('/b/' + encodeURIComponent(d.id) + '/' + l.path, l.form(id)).then(function () { after(d.id, l.label + ' ' + id + ': linked.'); },
        function (err) { busy(form, false); say(note, err.message, true); });
    };
    var s = el('section', 'bv-sec bw-linksec'); s.appendChild(el('h3', '', 'Add a link')); s.appendChild(form);
    return s;
  }

  /* On a relation row: Remove for a waits-on or holds-up edge, on its second tap (it names what it removes, rubric C8). */
  function rel(li, key, r, d) {
    var u = canEdit() && d && d.id ? unlink(key, r.id) : null;
    if (!u) return;
    var b = button('Remove', 'btn small ghost danger bw-rm'), name = r.title || r.id;
    b.setAttribute('aria-label', u.word + ' ' + name);
    b.onclick = function () {
      if (!b.dataset.armed) { b.dataset.armed = '1'; b.textContent = 'Tap again to remove'; setTimeout(function () { if (b.isConnected) { delete b.dataset.armed; b.textContent = 'Remove'; } }, 4000); return; }
      b.disabled = true; b.textContent = 'Removing…';
      post('/b/' + encodeURIComponent(d.id) + '/' + u.path, u.form).then(function () { after(d.id, u.word + ' ' + name + '.'); },
        function (err) {
          b.disabled = false; delete b.dataset.armed; b.textContent = 'Remove';
          var n = li.querySelector('.bw-err') || li.appendChild(el('span', 'sm bw-err'));
          n.setAttribute('role', 'status'); n.textContent = err.message;
        });
    };
    li.appendChild(b);
  }

  /* The comment composer (WB-3), atop the comments: a markdown draft with its preview, posted as the viewer. */
  function composer(d) {
    if (!canEdit() || !d || !d.id) return null;
    var form = el('form', 'bw-comment'), md = draft(d.id, 'text', 'comments/preview', '', 'Your comment'), note = status();
    var go = el('button', 'btn small', 'Comment'); go.type = 'submit';
    form.appendChild(md); form.appendChild(go); form.appendChild(note);
    form.onsubmit = function (e) {
      e.preventDefault();
      var text = md.text.value.trim();
      if (!text) { say(note, 'Write something first.', true); md.text.hidden = false; md.text.focus(); return; }
      busy(form, true); say(note, 'Posting…', false);
      post('/b/' + encodeURIComponent(d.id) + '/comments', { text: text }).then(function () { after(d.id, 'Comment posted.'); },
        function (err) { busy(form, false); say(note, err.message, true); });
    };
    return form;
  }

  api.mount = mount; api.links = links; api.rel = rel; api.composer = composer;
  return api;
})();
if (typeof module !== 'undefined') module.exports = BEADWRITE;
