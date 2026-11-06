/* web/settings.js: the settings panel (Lands workbench-glass spec WB-13, step 5.12, gd-au7ywvWAG8), the Workbench's settings
   kind, /garden?settings=me: the viewer's own settings, opened from the Menu's Settings item and the retired /settings page
   (R10). It reads GET /garden/settings (settings.go: the viewer's user bead as /settings reads it) and shows two sections:
   - Spawning: the default project (where a bead names none), role and prompt that "Put an agent on this" starts from, and
     whether a selection of beads gets one agent or one each;
   - You: display name, pronouns, team and the Slack channel the viewer's DMs go to.
   Edit opens both as one form; Save posts POST /settings asked for JSON with scope=garden and only these fields, so the
   page's other settings (the homepage layout, the simple or full viewer) keep what is stored; a refusal says why and keeps
   what was typed. A viewer with no user bead is told there is nowhere to keep settings yet. Text goes in through
   textContent. */
var SETTINGS = (function () {
  /* ---------- pure logic (also run under node by the Go tests) ---------- */
  var SPAWN = [['default_spawn_project', 'Default project', 'project'], ['default_spawn_role', 'Default role', 'role'],
    ['default_spawn_prompt_template', 'Prompt template', 'text'], ['spawn_per_selection', 'For a selection', 'per']];
  var IDENTITY = [['display_name', 'Display name'], ['pronouns', 'Pronouns'], ['team', 'Team'], ['slack_channel_id', 'Slack DM channel']];
  var PER = [['single', 'one agent for all of them'], ['per-bead', 'one agent for each bead']];
  function url() { return '/garden/settings'; }
  function perWord(v) { return v === 'per-bead' ? PER[1][1] : PER[0][1]; }
  /* The spawn preferences as label and value rows; a blank says what it falls back to. */
  function spawnRows(s) {
    s = s || {};
    return [['Default project', s.default_spawn_project || 'none (gardenio)'], ['Default role', s.default_spawn_role || 'none (crew)'],
      ['Prompt template', s.default_spawn_prompt_template ? 'set (below)' : 'none'], ['For a selection', perWord(s.spawn_per_selection)]];
  }
  function identityRows(i) {
    i = i || {};
    return IDENTITY.map(function (f) { return [f[1], i[f[0]] || '—']; });
  }
  /* A Slack channel id the route takes ('' is none): C, D or G and capitals and digits. '' when it is fine, else why not. */
  function channelProblem(v) { v = String(v || '').trim(); return !v || /^[CDG][A-Z0-9]+$/.test(v) ? '' : 'A Slack channel id looks like C0123ABC or D0123ABC.'; }
  /* The choices for a select: the list, with the stored value kept when it is not in it, and '' (none) first. */
  function choices(list, stored) {
    var out = [''].concat(list || []);
    if (stored && out.indexOf(stored) < 0) out.push(stored);
    return out;
  }
  /* The save's form as POST /settings reads it: scope=garden, then every field this panel edits (trimmed, all but the
     prompt template) and no other, so the rest keep what is stored. */
  function formParams(v) {
    var out = [['scope', 'garden']];
    SPAWN.concat(IDENTITY).forEach(function (f) {
      var x = String(v[f[0]] == null ? '' : v[f[0]]);
      out.push([f[0], f[0] === 'default_spawn_prompt_template' ? x : x.trim()]);
    });
    return out;
  }
  var api = { SPAWN: SPAWN, IDENTITY: IDENTITY, url: url, perWord: perWord, spawnRows: spawnRows, identityRows: identityRows, channelProblem: channelProblem,
    choices: choices, formParams: formParams };
  if (typeof document === 'undefined' || typeof WORKBENCH === 'undefined') return api;

  /* ---------- DOM ---------- */
  var W = WORKBENCH, B = document.body, base = B.dataset.base || '';
  var titleEl = document.getElementById('bv-title'), kindEl = document.getElementById('bv-kind'), bodyEl = document.getElementById('bv-body');
  if (!bodyEl) return api;
  var noteEl = null, sending = false;

  function el(t, cls, text) { var e = document.createElement(t); if (cls) e.className = cls; if (text !== undefined) e.textContent = text; return e; }
  function say(msg, err) { if (!noteEl) return; noteEl.textContent = msg; noteEl.className = 'bv-note' + (err ? ' err' : ''); }
  function sec(title, node) { var s = el('section', 'bv-sec pj-sec'); s.appendChild(el('h3', '', title)); s.appendChild(node); return s; }
  function stats(rows) {
    var dl = el('dl', 'bv-fields');
    rows.forEach(function (r) { dl.appendChild(el('dt', '', r[0])); dl.appendChild(el('dd', '', r[1])); });
    return dl;
  }
  function field(label, input, hint) {
    var f = el('label', 'bw-f'); f.appendChild(el('span', 'bw-l', label)); f.appendChild(input);
    if (hint) f.appendChild(el('span', 'bw-h', hint));
    return f;
  }
  function select(name, list, value, word) {
    var s = el('select', 'bw-in'); s.name = name;
    list.forEach(function (o) { var op = el('option', '', word ? word(o) : o); op.value = o; s.appendChild(op); });
    s.value = value || '';
    return s;
  }

  function view(d) {
    var box = el('div');
    if (!d.has_bead) {
      box.appendChild(el('p', 'bv-empty', 'You have no user bead yet, so there is nowhere to keep your settings. An operator adds you as a user.'));
      return box;
    }
    var sp = el('div'); sp.appendChild(stats(spawnRows(d.spawn)));
    if (d.spawn && d.spawn.default_spawn_prompt_template) sp.appendChild(el('pre', 'gd-json', d.spawn.default_spawn_prompt_template));
    box.appendChild(sec('Spawning', sp));
    box.appendChild(sec('You', stats(identityRows(d.identity))));
    var edit = el('button', 'btn small', 'Edit your settings'); edit.type = 'button';
    edit.onclick = function () {
      edit.hidden = true; box.hidden = true;
      var f = editor(d, function () { edit.hidden = false; box.hidden = false; });
      bodyEl.appendChild(f); f.querySelector('select, input').focus();
    };
    box.appendChild(edit);
    return box;
  }
  function editor(d, done) {
    var form = el('form', 'bw-edit pj-form st-form'), ins = {}, s = d.spawn || {}, id = d.identity || {};
    form.setAttribute('aria-label', 'Edit your settings');
    form.appendChild(el('h3', '', 'Spawning'));
    ins.default_spawn_project = select('default_spawn_project', choices(d.projects, s.default_spawn_project), s.default_spawn_project, function (o) { return o || 'none (gardenio)'; });
    form.appendChild(field('Default project', ins.default_spawn_project));
    ins.default_spawn_role = select('default_spawn_role', choices(d.roles, s.default_spawn_role), s.default_spawn_role, function (o) { return o || 'none (crew)'; });
    form.appendChild(field('Default role', ins.default_spawn_role));
    var ta = el('textarea', 'bw-in'); ta.name = 'default_spawn_prompt_template'; ta.rows = 4; ta.maxLength = 8192; ta.value = s.default_spawn_prompt_template || '';
    ins.default_spawn_prompt_template = ta;
    form.appendChild(field('Prompt template', ta, '{{title}} and {{id}} become the bead’s.'));
    ins.spawn_per_selection = select('spawn_per_selection', PER.map(function (p) { return p[0]; }), s.spawn_per_selection === 'per-bead' ? 'per-bead' : 'single', perWord);
    form.appendChild(field('For a selection', ins.spawn_per_selection));
    form.appendChild(el('h3', '', 'You'));
    IDENTITY.forEach(function (f) {
      var i = el('input', 'bw-in'); i.name = f[0]; i.value = id[f[0]] || ''; i.maxLength = 256; ins[f[0]] = i;
      if (f[0] === 'slack_channel_id') { i.autocapitalize = 'characters'; i.spellcheck = false; i.placeholder = 'D0123ABC'; }
      form.appendChild(field(f[1], i));
    });
    var acts = el('div', 'bv-acts'), save = el('button', 'btn small', 'Save'), no = el('button', 'btn small ghost', 'Cancel'), st = el('p', 'bw-h');
    save.type = 'submit'; no.type = 'button'; st.setAttribute('role', 'status');
    no.onclick = function () { form.remove(); done(); };
    acts.appendChild(save); acts.appendChild(no); form.appendChild(acts); form.appendChild(st);
    form.onsubmit = function (e) {
      e.preventDefault();
      var vals = {}; Object.keys(ins).forEach(function (k) { vals[k] = ins[k].value; });
      var bad = channelProblem(vals.slack_channel_id);
      if (bad) { st.textContent = bad; st.className = 'bw-h bw-err'; ins.slack_channel_id.setAttribute('aria-invalid', 'true'); ins.slack_channel_id.focus(); return; }
      save.disabled = true; no.disabled = true; sending = true; st.textContent = 'Saving…'; st.className = 'bw-h';
      post(formParams(vals)).then(function () {
        sending = false; form.remove(); done(); say('Saved your settings.'); W.refresh('settings', 'me');
      }, function (err) {
        sending = false; save.disabled = false; no.disabled = false; st.textContent = err.message; st.className = 'bw-h bw-err';
      });
    };
    return form;
  }
  /* POST /settings asked for JSON; a refusal carries the route's message. */
  function post(params) {
    var body = new URLSearchParams(); params.forEach(function (p) { body.append(p[0], p[1]); });
    return fetch(base + '/settings', { method: 'POST', credentials: 'same-origin', redirect: 'manual',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded', Accept: 'application/json' }, body: body.toString() }).then(function (r) {
      if (r.type === 'opaqueredirect' || r.status === 401) throw new Error('Your session ended. Reload the page to sign in again.');
      return r.json().catch(function () { return null; }).then(function (j) {
        if (j && j.ok) return j;
        throw new Error((j && j.error) || 'That did not go through (' + r.status + ').');
      });
    }, function () { throw new Error('The garden did not answer. Try again.'); });
  }

  function hold() { return sending || !!bodyEl.querySelector('.st-form'); }
  /* A failed load names what failed as the viewer knows it, "your settings", not the panel's id (me); the host's words
     otherwise, a session that ended among them. */
  function failure(status) { return status === 401 || status === 'redirect' ? null : W.failure(status, 'settings', 'your settings'); }
  function paint(id, d, note, isErr, retry) {
    kindEl.textContent = 'settings';
    titleEl.textContent = 'Your settings';
    bodyEl.textContent = '';
    noteEl = el('p', 'bv-note' + (isErr ? ' err' : ''), note || '');
    noteEl.setAttribute('role', 'status');
    if (isErr && retry) {   // a failure a second try may pass: Try again, as every Workbench panel offers (WB-15)
      var b = el('button', 'btn small bv-retry', 'Try again'); b.type = 'button';
      b.onclick = function () { b.disabled = true; b.textContent = 'Loading…'; W.refresh('settings', id); };
      noteEl.appendChild(document.createTextNode(' ')); noteEl.appendChild(b);
    }
    if (d && d.email) { var meta = el('div', 'bv-meta'); meta.appendChild(el('span', '', 'Signed in as ' + d.email)); bodyEl.appendChild(meta); }
    bodyEl.appendChild(noteEl);
    if (d) bodyEl.appendChild(view(d));
  }

  W.register('settings', { noun: 'settings', url: url, paint: paint, failure: failure, hold: hold, closed: function () { noteEl = null; sending = false; } });

  /* The Menu's Settings item opens the panel (garden.html #cam-settings); the Menu closes itself after the item's handler. */
  var item = document.getElementById('cam-settings');
  if (item) item.addEventListener('click', function () { W.open('settings', 'me'); });
  return api;
})();
if (typeof module !== 'undefined') module.exports = SETTINGS;
