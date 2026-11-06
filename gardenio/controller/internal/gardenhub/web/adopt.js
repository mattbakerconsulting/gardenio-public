/* Meadow adoption (gd-6HQyB0jgmO, Homestead World phase 3; gd-BXbYNWyOcF): the in-garden bead view (web/workbench.js) offers an
   operator "Adopt into a plan" on a loose bead. One press hands the bead to a gardener: it says an @gardenio mention in the
   bead's land's general channel (POST /garden/c/<land>/general/say, the Commons compose path of internal/commons mention.go,
   which starts a thread agent as a typed mention would), asking it to read the bead, choose the plan and phase that fit, and
   write the one parent-child edge; the note then links the thread to follow. The owner asked for this over a picker: "it
   should most definitely just call an agent to do its best on that, not make the user pick" (gd-GokiIZXH6Q). The picker
   stays behind "Pick the plan yourself…", and opens by itself when no gardener can be asked (no Commons in this land, a role
   that may not post): it posts the choice to POST /garden/bead/<id>/adopt (adopt.go), the plans being the farm document's (the
   'farmdoc' event plans.js sends), cut to the bead's project. Keys and clicks alike, and the keyboard and screen reader path.
   The page shows it only to a viewer the server says may adopt (body data-adopt); the routes check again. Text goes in
   through textContent only. */
var ADOPT = (function () {
  /* ---------- pure logic (also run under node by the Go tests) ---------- */
  var LOOSE = { task: 1, feature: 1, bug: 1, chore: 1 };
  /* A bead document (GET /garden/bead/<id>) the picker may be offered for: open loose work under no epic. A parent the
     viewer cannot see (no type) counts against it, as on the server. */
  function adoptable(d) {
    if (!d || !d.id || !LOOSE[d.type] || d.status === 'closed') return false;
    return !(d.parents || []).some(function (p) { return !p.type || p.type === 'epic'; });
  }
  function sameProject(a, b) { a = String(a || '').trim(); b = String(b || '').trim(); return !a || !b || a === b; }
  function byTitle(a, b) { var x = String(a.title || a.id).toLowerCase(), y = String(b.title || b.id).toLowerCase(); return x < y ? -1 : x > y ? 1 : a.id < b.id ? -1 : a.id > b.id ? 1 : 0; }
  /* The choices for a bead in project, from a farm document: each open plan (then its open phases, in phase order), and
     each draft plan, by title. A choice is { plan, phase, label, group }; group is the plan's title. */
  function choices(doc, project) {
    var out = [];
    var plans = ((doc || {}).plans || []).filter(function (p) { return p && p.id && p.status !== 'closed' && sameProject(p.project, project); }).slice().sort(byTitle);
    plans.forEach(function (p) {
      var t = p.title || p.id;
      out.push({ plan: p.id, phase: '', label: 'No phase', group: t });
      (p.phases || []).forEach(function (ph) {
        if (ph && ph.id && ph.status !== 'closed') out.push({ plan: p.id, phase: ph.id, label: ph.title || ph.id, group: t });
      });
    });
    var seeds = ((doc || {}).seeds || []).filter(function (s) { return s && s.id && s.status !== 'closed' && sameProject(s.project, project); }).slice().sort(byTitle);
    seeds.forEach(function (s) { var t = (s.title || s.id) + ' (draft)'; out.push({ plan: s.id, phase: '', label: t, group: 'Draft plans' }); });
    return out;
  }
  /* Where a choice puts the bead, in full: the plan, then the phase after a chevron. */
  function where(c) { return c.phase ? c.group + ' \u203a ' + c.label : c.group === 'Draft plans' ? c.label : c.group; }
  /* The form the route takes for a choice. */
  function params(c) { var p = { plan: c.plan }; if (c.phase) p.phase = c.phase; return p; }
  /* What to tell the viewer after the route answers with status and its JSON body (null when it sent none). */
  var WHY = { 401: 'Your session ended. Reload the page to sign in again.', 403: 'You are not allowed to adopt beads.', 404: 'That bead or plan is gone.', 503: 'Adoption is not enabled here.' };
  function outcome(status, body, label) {
    body = body || {};
    if (status === 200 && body.ok) return { ok: true, text: (body.already ? 'Already in ' : 'Adopted into ') + label + '.' };
    return { ok: false, text: body.error || WHY[status] || 'That did not go through (' + status + ').' };
  }

  /* ---------- handing the bead to a gardener (gd-BXbYNWyOcF) ---------- */
  var CHANNEL = 'general';   // a land's general channel: a mention of @gardenio there starts a thread agent (internal/commons mention.go)
  /* ask(d) -> the mention that hands bead d to a gardener: what to read, how to choose, the one edge to write, what to say back. */
  function ask(d) {
    var id = d.id, pj = d.project ? ' in ' + d.project : '', title = d.title ? ' ("' + String(d.title).replace(/\s+/g, ' ').replace(/"/g, "'").slice(0, 120) + '")' : '';
    return '@gardenio adopt ' + id + ' into a plan. It is loose work' + pj + title + '. ' +
      'Read it (kd show ' + id + '), look at the open plans' + pj + ' and their phases, and pick the plan whose goal it serves and the phase whose work it is; a draft plan only if nothing open fits. ' +
      'File it there with `kd dep add ' + id + ' <plan or phase id> --type parent-child` (the one edge the garden\'s Adopt writes). ' +
      'Then say here, in a line or two, which plan and phase you chose and why. If no plan fits, say so and leave it in the meadow.';
  }
  /* askPath(base, d) -> where the mention is said, the land's general channel; '' when the bead has no project, so no land. */
  function askPath(base, d) { return d && d.project ? base + '/garden/c/' + encodeURIComponent(d.project) + '/' + CHANNEL + '/say' : ''; }
  /* threadPath(base, d, mid) -> the thread the mention roots, to follow the gardener in: its node in the workbench. */
  function threadPath(base, d, mid) { return base + '/garden/c/n/' + encodeURIComponent(mid); }
  /* asked(status, note, id, mid) -> what the viewer is told once the mention is answered (note is the X-Commons-Note, the
     bridge's word on the agent; mid the id of the message the Commons wrote, from its fragment). It is asked only when a
     message came back: a 2xx with none is some other page answering a path that has no Commons behind it (the viewer's
     /garden prefix, say), not a gardener. pick says the picker should open instead: nobody can be asked here. */
  var ASK_WHY = { 401: 'Your session ended. Reload the page to sign in again.', 403: 'You may not post in this land, so no gardener can be asked.', 404: 'This land has no Commons, so no gardener can be asked.', 429: 'The Commons is busy. Try again in a moment.', 501: 'The Commons cannot start gardeners here.', 503: 'The Commons is not enabled here.' };
  function asked(status, note, id, mid) {
    if ((status === 201 || status === 200) && mid) return { ok: true, text: 'Asked a gardener to adopt ' + id + ' into a plan.' + (note ? ' ' + note : ''), pick: false };
    if (status === 201 || status === 200) status = 404;
    return { ok: false, text: ASK_WHY[status] || 'The ask did not go through (' + status + ').', pick: status === 403 || status === 404 || status === 501 || status === 503 };
  }

  var api = { adoptable: adoptable, choices: choices, where: where, params: params, outcome: outcome, ask: ask, askPath: askPath, threadPath: threadPath, asked: asked, CHANNEL: CHANNEL, mount: function () {} };
  if (typeof document === 'undefined') return api;

  /* ---------- DOM ---------- */
  var B = document.body, base = B.dataset.base || '', farm = typeof PLANS !== 'undefined' && PLANS.doc ? PLANS.doc() : null, said = {}, handed = {};
  document.addEventListener('farmdoc', function (e) { farm = e.detail || farm; });
  function el(t, cls, text) { var e = document.createElement(t); if (cls) e.className = cls; if (text !== undefined) e.textContent = text; return e; }
  function button(text, cls, fn) { var b = el('button', cls, text); b.type = 'button'; b.onclick = fn; return b; }

  /* Adopt bead id into choice c: a promise of the outcome() the viewer is told. */
  function post(id, c) {
    var label = where(c);
    return fetch(base + '/garden/bead/' + encodeURIComponent(id) + '/adopt', { method: 'POST', credentials: 'same-origin', redirect: 'manual', headers: { 'Content-Type': 'application/x-www-form-urlencoded', Accept: 'application/json' }, body: new URLSearchParams(params(c)).toString() })
      .then(function (r) {
        if (r.type === 'opaqueredirect') return outcome(401, null, label);
        return r.json().then(function (b) { return outcome(r.status, b, label); }, function () { return outcome(r.status, null, label); });
      }, function () { return { ok: false, text: 'The garden did not answer. Try again.' }; });
  }
  /* Hand bead d to a gardener: say the mention in its land's general channel. A promise of asked() plus href, the thread to
     follow (the message roots it; the fragment the Commons answers with carries its id). */
  function hand(d) {
    var p = askPath(base, d);
    if (!p) return Promise.resolve({ ok: false, text: d.id + ' has no project, so there is no land to ask in.', pick: true });
    return fetch(p, { method: 'POST', credentials: 'same-origin', redirect: 'manual', cache: 'no-store', headers: { 'Content-Type': 'application/x-www-form-urlencoded', 'X-Commons': 'fragment' }, body: new URLSearchParams({ text: ask(d) }).toString() })
      .then(function (r) {
        if (r.type === 'opaqueredirect') return asked(401, '', d.id);
        var note = ''; try { note = decodeURIComponent(r.headers.get('X-Commons-Note') || ''); } catch (e) { note = ''; }
        return r.text().then(function (t) {
          var m = /<li\b[^>]*\bclass="cm-msg\b[^>]*\bdata-id="([^"]+)"/.exec(t), o = asked(r.status, note, d.id, m ? m[1] : '');
          if (o.ok) { o.thread = m[1]; o.href = threadPath(base, d, m[1]); }
          return o;
        }, function () { return asked(r.status, note, d.id, ''); });
      }, function () { return { ok: false, text: 'The garden did not answer. Try again.', pick: false }; });
  }
  /* Into note: what the viewer is told, and the thread to follow when there is one (it opens in the workbench). */
  function tell(note, o, d) {
    note.textContent = o.text; note.className = 'bv-note' + (o.ok ? '' : ' err');
    if (!o.href) return;
    var a = el('a', 'ad-follow', 'Follow it in the Commons'); a.href = o.href;
    note.appendChild(document.createTextNode(' ')); note.appendChild(a); note.appendChild(document.createTextNode('.'));
  }
  /* Into host, for bead document d: what the last adoption of this bead said, then, while the bead is loose, "Adopt into a plan"
     (a gardener is asked) and "Pick the plan yourself…" (the picker); once asked, the note and its thread instead, so one press
     is one gardener. done() reloads the bead, whose new parent then shows under "Part of". */
  function mount(host, d, done) {
    if (B.dataset.adopt !== '1' || !host || !d || !d.id) return;
    var box = el('div', 'ad'), note = el('p', 'bv-note');
    note.setAttribute('role', 'status');
    if (said[d.id]) { note.textContent = said[d.id].text; if (!said[d.id].ok) note.className = 'bv-note err'; delete said[d.id]; }
    if (adoptable(d)) {
      if (handed[d.id]) tell(note, handed[d.id], d);
      else {
        var acts = el('div', 'bv-acts'), own;
        var go = button('Adopt into a plan', 'btn small ad-ask', function () {
          go.disabled = own.disabled = true; note.textContent = 'Asking a gardener to adopt ' + d.id + '\u2026'; note.className = 'bv-note';
          if (!note.parentNode) box.appendChild(note);
          hand(d).then(function (o) {
            go.disabled = own.disabled = false;
            if (o.ok) { handed[d.id] = o; acts.remove(); tell(note, o, d); return; }
            tell(note, o, d);
            if (o.pick) pick(box, d, note, acts, done); else go.focus();
          });
        });
        own = button('Pick the plan yourself\u2026', 'btn small ghost ad-own', function () { note.textContent = ''; note.className = 'bv-note'; pick(box, d, note, acts, done); });
        acts.appendChild(go); acts.appendChild(own); box.appendChild(acts);
      }
    }
    if (note.textContent) box.appendChild(note);
    if (box.children.length) host.appendChild(box);
  }
  /* The picker: the bead's project's open plans and their phases, from the farm document. acts (the two buttons) hides while it
     is open and comes back on Cancel. A note already in place (why the gardener could not be asked) stays above the form. */
  function pick(box, d, note, acts, done) {
    var cs = choices(farm, d.project);
    acts.hidden = true;
    if (!note.parentNode) box.appendChild(note);
    if (!cs.length) { note.textContent = 'There is no open plan' + (d.project ? ' in ' + d.project : '') + ' to adopt ' + d.id + ' into.'; note.className = 'bv-note'; acts.hidden = false; return; }
    var form = el('form', 'ad-form'), sel = el('select', 'ad-sel'), lab = el('label', 'ad-lab', 'Plan or phase');
    sel.id = 'ad-sel'; lab.htmlFor = 'ad-sel';
    var groups = {};
    cs.forEach(function (c, i) {
      var g = groups[c.group];
      if (!g) { g = groups[c.group] = el('optgroup'); g.label = c.group; sel.appendChild(g); }
      var o = el('option', '', c.label); o.value = String(i); g.appendChild(o);
    });
    var into = el('p', 'bv-note ad-into');
    sel.onchange = function () { into.textContent = 'Into ' + where(cs[+sel.value]) + '.'; };
    sel.onchange();
    var go = el('button', 'btn small', 'Adopt'), no = button('Cancel', 'btn small', function () { form.remove(); note.textContent = ''; note.className = 'bv-note'; acts.hidden = false; acts.firstChild.focus(); });
    go.type = 'submit';
    var row = el('div', 'bv-acts'); row.appendChild(go); row.appendChild(no);
    form.appendChild(lab); form.appendChild(sel); form.appendChild(into); form.appendChild(row);
    box.appendChild(form);
    var why = note.textContent; if (why && note.className.indexOf('err') >= 0) { note.className = 'bv-note'; box.insertBefore(note, form); } else note.textContent = '';   // why the picker opened stays as a plain note above it
    form.onsubmit = function (e) {
      e.preventDefault();
      var c = cs[+sel.value];
      go.disabled = sel.disabled = true; note.textContent = 'Adopting ' + d.id + '\u2026'; note.className = 'bv-note';
      box.appendChild(note);   // under the form while it works
      post(d.id, c).then(function (o) {
        if (o.ok) { said[d.id] = o; done(); return; }
        go.disabled = sel.disabled = false; note.textContent = o.text; note.className = 'bv-note err'; sel.focus();
      });
    };
    sel.focus();
  }
  api.mount = mount;
  return api;
})();
if (typeof module !== 'undefined') module.exports = ADOPT;
