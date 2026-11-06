/* web/glassact.js: the Glass, nudge and respond (Lands workbench-glass spec GL-7, GL-8, GL-11; step 6.5, gd-m1aiBXtDak).
   The Glass panel itself (web/glass.js) is watch and only watch: it posts nothing. This module adds the two writes that are
   not the keyboard, under the terminal of a Glass whose document (GET /garden/glass/<id>, glass.go) says the viewer may:
   - Nudge (can_nudge): a sentence for the agent's next turn, POST /garden/glass/<id>/send type=nudge (glass_send.go). The
     garden nudges the bridge's way (the agent's MCP channel first, coop's PTY nudge after) and, once it lands, writes it
     into the agent's Commons thread as the person's message. One that does not land says why (busy at a prompt, stopped,
     gone) and the words stay in the field.
   - Respond (can_respond): while the agent is parked at a prompt, GET /garden/glass/<id>/prompt renders it with the shared
     respond component (internal/respond; the Commons tool-permission card answers with the same one): what it asks and
     every option by its label, never a bare number. Each answer is its own small form posting to /send; the server checks
     it against the prompt as coop has it now, and a 409 means the prompt changed, so it is read again. The prompt is read
     when the Glass opens and whenever the stream's state says the agent's prompt changed. A Glass opened from a decision
     or permission card's See the screen (glass:to, web/glass.js) is brought to its prompt once that is read (spec 3.3).
     A permission answered here closes its Commons card too (glass_send.go Answered).
   input, keys and resize are not here: they need the keyboard (attach, GL-9, step 6.6). glass.js tells this module about
   its Glass with glass:open {id, doc, slot}, glass:state {id, state} and glass:close events on the document, and this module
   tells it what each read of the prompt found (glass:prompt {id, on, prompt: {type, tool}}), so its state chip says Prompt
   on coop's word, not the bead's agent_state (GL-4). The prompt is HTML the server rendered with html/template; everything
   else goes in through textContent. */
var GLASSACT = (function () {
  /* ---------- pure logic (also run under node by the Go tests) ---------- */
  function sendURL(id) { return '/garden/glass/' + encodeURIComponent(id) + '/send'; }
  function promptURL(id) { return '/garden/glass/' + encodeURIComponent(id) + '/prompt'; }
  var NUDGE_MAX = 4096;   // bytes, as the server caps a nudge (glass_send.go glassNudgeMax)
  /* What about a state (the stream's state event, internal/glass State) would change the prompt shown: '' for no prompt. */
  function promptKey(s) {
    if (!s || String(s.state || '').toLowerCase() !== 'prompt') return '';
    try { return 'prompt:' + JSON.stringify(s.prompt || null); } catch (e) { return 'prompt'; }
  }
  /* The words for a send's answer, {ok, text, again}: the server's note (GlassSent) when it sent one, else the status said
     plainly. again: the prompt changed or went, so read it again. */
  function sentLine(status, body) {
    var ok = status >= 200 && status < 300, b = body && typeof body === 'object' ? body : null;
    var text = b && b.note ? String(b.note) : typeof body === 'string' && body.trim() ? body.trim() : '';
    if (!text) {
      if (ok) text = 'Sent.';
      else if (status === 403) text = 'Not sent: you may not do that here.';
      else if (status === 404) text = 'Not sent: the agent is not there.';
      else if (status === 409) text = 'Not sent: the agent moved on.';
      else text = 'Not sent: the garden did not answer (' + status + '). Try again.';
    }
    var again = !!(b && (b.result === 'changed' || b.result === 'no_prompt' || b.result === 'not_live'));
    return { ok: ok && !(b && b.ok === false), text: text, again: again };
  }
  /* A string's UTF-8 length, the length the server counts. */
  function utf8(s) {
    var n = 0;
    s = String(s || '');
    for (var i = 0; i < s.length; i++) { var c = s.charCodeAt(i); n += c < 0x80 ? 1 : c < 0x800 ? 2 : c >= 0xd800 && c < 0xdc00 ? (i++, 4) : 3; }
    return n;
  }
  var api = { sendURL: sendURL, promptURL: promptURL, promptKey: promptKey, sentLine: sentLine, utf8: utf8, NUDGE_MAX: NUDGE_MAX };
  if (typeof document === 'undefined') return api;

  /* ---------- DOM ---------- */
  var B = document.body, base = B.dataset.base || '';
  if (B.dataset.glass !== '1') return api;
  var cur = null;   // the Glass written to: {id, doc, slot, box, region, key, seq, to}
  var to = null;    // glass:to's {id, to}: the Glass being opened was asked for at its prompt (a card's See the screen)

  function tell(name, detail) { try { document.dispatchEvent(new CustomEvent(name, { detail: detail })); } catch (e) { /* no CustomEvent */ } }
  function el(t, cls, text) { var e = document.createElement(t); if (cls) e.className = cls; if (text !== undefined) e.textContent = text; return e; }
  function say(note, line) { note.textContent = line.text; note.className = 'bv-note gl-sent' + (line.ok ? ' ok' : ' err'); }

  /* POST a form's fields (and the button that sent it) to the Glass's /send; resolves to sentLine's answer. */
  function post(id, fields) {
    var body = new URLSearchParams();
    fields.forEach(function (kv) { body.append(kv[0], kv[1]); });
    return fetch(base + sendURL(id), { method: 'POST', credentials: 'same-origin', cache: 'no-store',
      headers: { Accept: 'application/json', 'Content-Type': 'application/x-www-form-urlencoded' }, body: body })
      .then(function (r) {
        var json = (r.headers.get('Content-Type') || '').indexOf('application/json') === 0;
        return (json ? r.json() : r.text()).then(function (b) { return sentLine(r.status, b); }, function () { return sentLine(r.status, null); });
      }, function () { return { ok: false, text: 'Not sent: the garden could not be reached. Try again.', again: false }; });
  }

  /* ---------- the prompt ---------- */
  function clearPrompt(c) { c.region.textContent = ''; c.region.hidden = true; }
  /* Opened from a card's See the screen (spec 3.3): once, bring the prompt into view and put the focus on it, or the
     terminal when there is no prompt to show. */
  function reveal(c, e) {
    if (!c.to) return;
    c.to = false;
    if (!e) return;
    try { e.scrollIntoView({ block: 'start' }); } catch (x) { e.scrollIntoView(); }
    if (!e.hasAttribute('tabindex')) e.setAttribute('tabindex', '-1');
    try { e.focus({ preventScroll: true }); } catch (x) { e.focus(); }
  }
  /* Read the prompt and show it; line, when set, is the outcome of the answer that made it stale, said under it again. */
  function loadPrompt(c, line) {
    var seq = ++c.seq;
    fetch(base + promptURL(c.id), { credentials: 'same-origin', cache: 'no-store', headers: { Accept: 'text/html' } }).then(function (r) {
      if (cur !== c || seq !== c.seq) return;
      if (r.status === 204) tell('glass:prompt', { id: c.id, on: false });   // coop says no prompt (an error says nothing)
      if (r.status === 204 || !r.ok) {
        clearPrompt(c);
        if (line) { c.region.hidden = false; say(c.region.appendChild(el('p', 'bv-note gl-sent')), line); }
        reveal(c, c.box);
        return;
      }
      return r.text().then(function (html) {
        if (cur !== c || seq !== c.seq) return;
        c.region.innerHTML = html;   // rendered by html/template (glass_send.go glassPromptTmpl, internal/respond)
        c.region.hidden = false;
        var sec = c.region.querySelector('.gl-prompt'), tool = c.region.querySelector('.gl-prompt-head code');
        tell('glass:prompt', { id: c.id, on: true, prompt: { type: (sec && sec.dataset.prompt) || '', tool: tool ? tool.textContent : '' } });
        var note = c.region.appendChild(el('p', 'bv-note gl-sent'));
        note.setAttribute('role', 'status');
        if (line) say(note, line);
        reveal(c, c.region.querySelector('.gl-prompt') || c.region);
      });
    }, function () { reveal(c, c.box); /* the next state event reads it again */ });
  }
  function answer(c, form, submitter) {
    var fields = [];
    new FormData(form).forEach(function (v, k) { fields.push([k, String(v)]); });
    if (submitter && submitter.name) fields.push([submitter.name, submitter.value]);
    var buttons = c.region.querySelectorAll('button');
    Array.prototype.forEach.call(buttons, function (b) { b.disabled = true; });
    var note = c.region.querySelector('.gl-sent') || c.region.appendChild(el('p', 'bv-note gl-sent'));
    note.textContent = 'Answering…'; note.className = 'bv-note gl-sent';
    post(c.id, fields).then(function (line) {
      if (cur !== c) return;
      Array.prototype.forEach.call(buttons, function (b) { b.disabled = false; });
      say(note, line);
      if (line.again) loadPrompt(c, line);
    });
  }

  /* ---------- the nudge box ---------- */
  function nudgeBox(id, name) {
    var form = el('form', 'gl-nudge');
    form.setAttribute('aria-label', 'Nudge ' + name);
    var label = form.appendChild(el('label', 'gl-nudge-label'));
    label.appendChild(el('span', '', 'Nudge ' + name));
    label.appendChild(el('span', 'gl-nudge-hint', ' — a sentence for its next turn; it shows in its thread.'));
    var field = label.appendChild(el('textarea'));
    field.name = 'message'; field.rows = 2; field.maxLength = NUDGE_MAX; field.required = true;
    field.placeholder = 'Look at the failing test before you go on';
    var row = form.appendChild(el('div', 'gl-nudge-row'));
    var send = row.appendChild(el('button', 'btn small', 'Send the nudge')); send.type = 'submit';
    var note = row.appendChild(el('p', 'bv-note gl-sent')); note.setAttribute('role', 'status');
    field.addEventListener('keydown', function (e) {
      if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) { e.preventDefault(); if (form.requestSubmit) form.requestSubmit(); else send.click(); }
    });
    form.addEventListener('submit', function (e) {
      e.preventDefault();
      var text = field.value.trim();
      if (!text) { say(note, { ok: false, text: 'Write something to send.' }); return; }
      if (utf8(text) > NUDGE_MAX) { say(note, { ok: false, text: 'That is too long for a nudge: keep it under 4 KB.' }); return; }
      send.disabled = true; note.textContent = 'Sending…'; note.className = 'bv-note gl-sent';
      post(id, [['type', 'nudge'], ['message', text]]).then(function (line) {
        send.disabled = false;
        if (!form.isConnected) return;
        say(note, line);
        if (line.ok) field.value = '';
      });
    });
    return form;
  }

  document.addEventListener('glass:to', function (e) {
    var d = e.detail || {};
    to = d.id && d.to === 'prompt' ? { id: d.id } : null;
    if (!to || !cur || cur.id !== to.id) return;
    to = null;   // that Glass is open already (the panel keeps it live, no new glass:open): bring its prompt into view now
    cur.to = true;
    reveal(cur, (!cur.region.hidden && cur.region.querySelector('.gl-prompt')) || cur.box);
  });
  document.addEventListener('glass:open', function (e) {
    var d = e.detail || {}, doc = d.doc || {}, want = !!(to && to.id === d.id);
    cur = null; to = null;
    if (!d.slot || !d.id || !(doc.can_nudge || doc.can_respond)) { if (want) reveal({ to: true }, d.box); return; }
    // key null: no stream state seen yet, so the first one always settles the prompt shown (one read at open may be stale)
    var c = { id: d.id, doc: doc, slot: d.slot, box: d.box || null, region: null, key: null, seq: 0, to: want };
    d.slot.textContent = '';
    c.region = d.slot.appendChild(el('div', 'gl-respond'));
    c.region.setAttribute('aria-live', 'polite');
    c.region.hidden = true;
    if (doc.can_respond) {
      c.region.addEventListener('submit', function (ev) {
        var f = ev.target;
        if (!f || f.tagName !== 'FORM' || !f.dataset || f.dataset.glass !== 'respond') return;   // the respond component's forms
        ev.preventDefault();
        answer(c, f, ev.submitter);
      });
    }
    if (doc.can_nudge) d.slot.appendChild(nudgeBox(d.id, doc.agent || d.id));
    cur = c;
    if (doc.can_respond && doc.live) loadPrompt(c);
    else reveal(c, c.box);
  });
  document.addEventListener('glass:state', function (e) {
    var d = e.detail || {};
    if (!cur || d.id !== cur.id || !cur.doc.can_respond) return;
    var k = promptKey(d.state);
    if (k === cur.key) return;
    cur.key = k;
    if (k) loadPrompt(cur); else { cur.seq++; clearPrompt(cur); }
  });
  document.addEventListener('glass:close', function () { cur = null; });
  document.addEventListener('toggle', function (e) {   // the Commons agent card's Nudge… (render.go, GL-7)
    var d = e.target;
    if (d.open && d.matches('.cm-nudge') && !d.querySelector('form')) d.appendChild(nudgeBox(d.dataset.agent, d.dataset.name)).querySelector('textarea').focus();
  }, true);
  /* The workbench's agent head (web/commons2.js, node.html's page-nudge.js): the nudge box for agent id, put right after at. */
  api.mount = function (at, id, name) {
    var f = at.parentNode.insertBefore(nudgeBox(id, name || id), at.nextSibling);
    f.querySelector('textarea').focus();
    return f;
  };
  return api;
})();
if (typeof module !== 'undefined') module.exports = GLASSACT;
