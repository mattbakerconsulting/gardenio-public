/* Send a gardener (gd-I3Qx07esJj): the in-garden bead view (web/workbench.js) offers "Send a gardener" on every bead. One press
   starts a gardener on it: it says an @gardenio mention in the bead's land's general channel (POST /garden/c/<land>/general/say,
   the Commons compose path of internal/commons mention.go, which starts a thread agent as a typed mention would, the same way
   Adopt asks one, web/adopt.js), with the brief as its prompt. The default brief is the owner's: figure out what's up with
   this bead and make progress on it, or say what unblocks it; "With a note…" lets the viewer write their own first. The mention
   opens with one fixed phrase, "@gardenio tend <bead>:", which the farm reads back off the agent bead (tend.go), so the
   gardener stands on the bead's plot, the Porch says where it was sent (web/porch.js), and this view lists the gardeners
   sent to the bead with what they are doing and, once they leave, what they said (the farm stream's exits, FarmExit.Tends).
   The viewer's own gardeners come first everywhere: they asked for them. The page shows the button only to a viewer the
   server says may send (body data-tend); the Commons checks the land role again. Text goes in through textContent only. */
var TEND = (function () {
  var W = typeof WORLD !== 'undefined' ? WORLD : require('./world.js');
  var R = typeof REASON !== 'undefined' ? REASON : require('./reason.js');
  /* ---------- pure logic (also run under node by the Go tests) ---------- */
  var DEFAULT = "Figure out what's up with this bead and make progress on it, or say what unblocks it.";
  var CHANNEL = 'general';   // a land's general channel: a mention of @gardenio there starts a thread agent (internal/commons mention.go)
  var LEFT_S = 86400;        // how long what a gardener said when it left is shown
  /* The opening phrase of a send, as tend.go reads it (tendRe): the bead id after "@gardenio tend", closed by a colon or the
     end. tends(text) -> that bead id, or ''. */
  var PHRASE = /^\s*@gardenio\s+tend\s+([A-Za-z]{2}-[A-Za-z0-9]{2,20})\s*(?::|$)/;
  function tends(text) { var m = PHRASE.exec(String(text || '')); return m ? m[1] : ''; }
  /* Which bead documents (GET /garden/bead/<id>) get the button: any bead, open or closed, but a gardener, which is asked in
     its own thread. */
  function offered(d) { return !!(d && d.id && d.type !== 'agent'); }
  function clean(s) { return String(s || '').replace(/\s+/g, ' ').replace(/"/g, "'").trim(); }
  function article(t) { t = String(t || '').trim(); return !t ? 'a bead' : (/^[aeiou]/i.test(t) ? 'an ' : 'a ') + t; }
  /* ask(d, prompt) -> the mention that sends a gardener to bead d with the brief prompt (the default when blank): the phrase the
     farm reads, the brief as typed, the bead in words, how to read it, what to do and to say back. A brief that already opens
     with the phrase for this bead is kept as it is. */
  function ask(d, prompt) {
    var p = String(prompt || '').trim() || DEFAULT, head = tends(p) === d.id ? p : '@gardenio tend ' + d.id + ': ' + p;
    var what = 'It is ' + article(d.type) + (d.project ? ' in ' + d.project : '') + (d.title ? ' ("' + clean(d.title).slice(0, 120) + '")' : '') + '.';
    return head + ' ' + what + ' Read it (kd show ' + d.id + ') and what it links to, then act: make progress and land it, or say in a line or two what blocks it and exactly who must do what to unblock it. Reply here either way.';
  }
  /* askPath(base, d) -> where the mention is said, the land's general channel; '' when the bead has no project, so no land. */
  function askPath(base, d) { return d && d.project ? base + '/garden/c/' + encodeURIComponent(d.project) + '/' + CHANNEL + '/say' : ''; }
  /* threadPath(base, d, mid) -> the thread the mention roots, to follow the gardener in: its node in the workbench. */
  function threadPath(base, d, mid) { return base + '/garden/c/n/' + encodeURIComponent(mid); }
  /* asked(status, note, id, mid) -> what the viewer is told once the mention is answered (note is the X-Commons-Note, the
     bridge's word on the agent; mid the id of the message the Commons wrote, from its fragment). Sent only when a message came
     back: a 2xx with none is some other page answering a path that has no Commons behind it, not a gardener. */
  var WHY = { 401: 'Your session ended. Reload the page to sign in again.', 403: 'You may not post in this land, so no gardener can be sent.', 404: 'This land has no Commons, so no gardener can be sent.', 429: 'The Commons is busy. Try again in a moment.', 501: 'The Commons cannot start gardeners here.', 503: 'The Commons is not enabled here.' };
  function asked(status, note, id, mid) {
    if ((status === 201 || status === 200) && mid) return { ok: true, text: 'Sent a gardener to ' + id + '.' + (note ? ' ' + note : '') };
    if (status === 201 || status === 200) status = 404;
    return { ok: false, text: WHY[status] || 'The send did not go through (' + status + ').' };
  }
  /* here(gardeners, id, me) -> the farm document's gardeners on bead id: sent to it (tends), holding it as their task, or
     spawned to run it (escalation: the bridge's executor of an escalation bead). The viewer's own first (they asked for
     them), then need first (WORLD.byNeed: down, asking, at work, the rest). */
  function here(gardeners, id, me) {
    if (!id) return [];
    var mine = function (g) { return !!me && g.operator === me; };
    return (gardeners || []).filter(function (g) { return g && (g.tends === id || g.task === id || g.escalation === id); })
      .sort(function (a, b) { return (mine(b) ? 1 : 0) - (mine(a) ? 1 : 0) || W.byNeed(a, b); });
  }
  /* left(prev, exits, id, nowS) -> what gardeners sent to bead id (or spawned to run it, an escalation's executor) said when
     they left: prev (what was kept) and the new exits together, once each, newest first, none older than a day. An
     interrupted exit has no words, and says so. */
  function left(prev, exits, id, nowS) {
    var by = {}, out = [];
    (prev || []).concat(exits || []).forEach(function (x) { if (x && x.id && (x.tends === id || x.escalation === id) && !(x.at < nowS - LEFT_S)) by[x.id] = x; });
    Object.keys(by).forEach(function (k) { out.push(by[k]); });
    return out.sort(function (a, b) { return (b.at || 0) - (a.at || 0) || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0); });
  }
  function leftWords(x) { return x.kind === 'interrupted' ? 'left without a word' : x.kind === 'parked' ? 'parked' : 'finished'; }

  var api = { DEFAULT: DEFAULT, CHANNEL: CHANNEL, tends: tends, offered: offered, ask: ask, askPath: askPath, threadPath: threadPath, asked: asked, here: here, left: left, leftWords: leftWords, mount: function () {}, live: function () {} };
  if (typeof document === 'undefined') return api;

  /* ---------- DOM ---------- */
  var B = document.body, base = B.dataset.base || '', me = (B.dataset.me || '').toLowerCase(), farm = typeof PLANS !== 'undefined' && PLANS.doc ? PLANS.doc() : null, said = {}, handed = {}, gone = {};
  document.addEventListener('farmdoc', function (e) { farm = e.detail || farm; });
  document.addEventListener('farmlive', function (e) {   // a patch's exits (web/plans.js): what a gardener sent to a bead (or spawned to run it) said when it left
    var xs = (e.detail || {}).exits || [], now = Date.now() / 1000;
    xs.forEach(function (x) { [x && x.tends, x && x.escalation].forEach(function (id) { if (id) gone[id] = left(gone[id], [x], id, now); }); });
  });
  function el(t, cls, text) { var e = document.createElement(t); if (cls) e.className = cls; if (text !== undefined) e.textContent = text; return e; }
  function button(text, cls, fn) { var b = el('button', cls, text); b.type = 'button'; b.onclick = fn; return b; }

  /* Send a gardener to bead d with the brief prompt: say the mention in its land's general channel. A promise of asked() plus
     href, the thread to follow (the message roots it; the fragment the Commons answers with carries its id). */
  function hand(d, prompt) {
    var p = askPath(base, d);
    if (!p) return Promise.resolve({ ok: false, text: d.id + ' has no project, so there is no land to ask in.' });
    return fetch(p, { method: 'POST', credentials: 'same-origin', redirect: 'manual', cache: 'no-store', headers: { 'Content-Type': 'application/x-www-form-urlencoded', 'X-Commons': 'fragment' }, body: new URLSearchParams({ text: ask(d, prompt) }).toString() })
      .then(function (r) {
        if (r.type === 'opaqueredirect') return asked(401, '', d.id);
        var note = ''; try { note = decodeURIComponent(r.headers.get('X-Commons-Note') || ''); } catch (e) { note = ''; }
        return r.text().then(function (t) {
          var m = /<li\b[^>]*\bclass="cm-msg\b[^>]*\bdata-id="([^"]+)"/.exec(t), o = asked(r.status, note, d.id, m ? m[1] : '');
          if (o.ok) { o.thread = m[1]; o.href = threadPath(base, d, m[1]); }
          return o;
        }, function () { return asked(r.status, note, d.id, ''); });
      }, function () { return { ok: false, text: 'The garden did not answer. Try again.' }; });
  }
  /* Into note: what the viewer is told, and the thread to follow when there is one (it opens in the workbench). */
  function tell(note, o, d) {
    note.textContent = o.text; note.className = 'bv-note' + (o.ok ? '' : ' err');
    if (!o.href) return;
    var a = el('a', 'td-follow', 'Follow it in the Commons'); a.href = o.href;
    note.appendChild(document.createTextNode(' ')); note.appendChild(a); note.appendChild(document.createTextNode('.'));
  }
  /* Into host, for bead document d: "Send a gardener" (one press, the default brief) and "With a note…" (the brief first); once
     sent, the note and its thread instead, so one press is one gardener, and "Send another" for a second. */
  function mount(host, d) {
    if (B.dataset.tend !== '1' || !host || !offered(d)) return;
    var box = el('div', 'td'), note = el('p', 'bv-note');
    note.setAttribute('role', 'status');
    if (said[d.id]) { note.textContent = said[d.id].text; if (!said[d.id].ok) note.className = 'bv-note err'; delete said[d.id]; }
    var acts = el('div', 'bv-acts'), go, own;
    function send(prompt, after) {
      go.disabled = own.disabled = true; note.textContent = 'Sending a gardener to ' + d.id + '…'; note.className = 'bv-note';
      if (!note.parentNode) box.appendChild(note);
      hand(d, prompt).then(function (o) {
        go.disabled = own.disabled = false;
        tell(note, o, d);
        if (o.ok) { handed[d.id] = o; go.textContent = 'Send another'; own.hidden = true; }
        if (after) after(o);
        (o.ok ? go : own.hidden ? go : own).focus();
      });
    }
    go = button(handed[d.id] ? 'Send another' : 'Send a gardener', 'btn small td-send', function () { send('', null); });
    own = button('With a note…', 'btn small td-own', function () { note.textContent = ''; note.className = 'bv-note'; brief(box, d, note, acts, send); });
    if (handed[d.id]) { tell(note, handed[d.id], d); own.hidden = true; }
    acts.appendChild(go); acts.appendChild(own); box.appendChild(acts);
    if (note.textContent) box.appendChild(note);
    host.appendChild(box);
  }
  /* The brief: a textarea with the default words, Send and Cancel. acts (the two buttons) hides while it is open. */
  function brief(box, d, note, acts, send) {
    acts.hidden = true;
    var form = el('form', 'td-form'), lab = el('label', 'td-lab', 'What to tell the gardener'), ta = el('textarea', 'td-text');
    ta.id = 'td-text'; lab.htmlFor = 'td-text'; ta.rows = 3; ta.value = DEFAULT; ta.maxLength = 4000;
    var go = el('button', 'btn small', 'Send'), no = button('Cancel', 'btn small', function () { form.remove(); note.textContent = ''; note.className = 'bv-note'; acts.hidden = false; acts.firstChild.focus(); });
    go.type = 'submit';
    var row = el('div', 'bv-acts'); row.appendChild(go); row.appendChild(no);
    form.appendChild(lab); form.appendChild(ta); form.appendChild(row);
    box.appendChild(form);
    form.onsubmit = function (e) {
      e.preventDefault();
      go.disabled = ta.disabled = true;
      box.appendChild(note);   // under the form while it works
      send(ta.value, function (o) { if (o.ok) { form.remove(); acts.hidden = false; return; } go.disabled = ta.disabled = false; ta.focus(); });
    };
    ta.focus(); ta.select();
  }
  /* Into host, for bead id: the gardeners on it now (what each is doing, its reason, its thread), then what the gardeners
     sent to it said when they left. Nothing when there are none. Re-run per farm document (web/workbench.js renderActs). */
  function live(host, id, opt) {
    opt = opt || {};
    var gs = here(farm && farm.gardeners, id, me), xs = left(gone[id], [], id, Date.now() / 1000);
    if (!host || (!gs.length && !xs.length)) return;
    var sec = el('section', 'bv-sec td-here');
    sec.appendChild(el('h3', '', 'Gardeners on it' + (gs.length ? ' (' + gs.length + ')' : '')));
    if (gs.length) {
      var ul = el('ul', 'bv-rels td-gs');
      gs.forEach(function (g) {
        var li = el('li', (me && g.operator === me ? 'mine' : '') + (R.needsYou(g) ? ' need' : '')), a = el('a', 'bl', g.agent || g.id);
        a.href = base + g.href; li.appendChild(a);
        var bits = [W.gardenerWord(g)], why = R.of(g);
        if (g.tends === id) bits.push(me && g.operator === me ? 'you sent it' : 'sent here');
        else if (g.escalation === id) bits.push('running it');
        if (why && why.text) bits.push(why.text);
        else if (g.doing && typeof ACTIVITY !== 'undefined') bits.push(ACTIVITY.toolWords(g.doing, Date.now() / 1000));
        li.appendChild(el('span', 'sm', bits.join(' · ')));
        if (g.chat && typeof CHAT !== 'undefined' && CHAT.open) li.appendChild(button('Thread', 'btn small ghost td-chat', function () { CHAT.open(g.id); }));
        ul.appendChild(li);
      });
      sec.appendChild(ul);
    }
    if (xs.length) {
      var xl = el('ul', 'bv-rels td-left');
      xs.forEach(function (x) {
        var li = el('li'), a = el('a', 'bl', x.agent || x.id); a.href = base + '/garden?bead=' + encodeURIComponent(x.id); li.appendChild(a);
        li.appendChild(el('span', 'sm', leftWords(x) + (opt.ago ? ' ' + opt.ago(x.at || 0) + ' ago' : '')));
        if (x.tldr) li.appendChild(el('p', 'td-said', x.tldr));
        xl.appendChild(li);
      });
      sec.appendChild(xl);
    }
    host.appendChild(sec);
  }
  api.mount = mount; api.live = live;
  return api;
})();
if (typeof module !== 'undefined') module.exports = TEND;
