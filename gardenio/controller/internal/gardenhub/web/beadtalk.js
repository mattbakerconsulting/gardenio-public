/* The bead view's talk (gd-tx2wMH48MV): the in-garden bead panel's (web/workbench.js) Comments section, and for a bead in a
   thread its Conversation section in place of it, the thread's lines and the bead's kd comments as one timeline. The panel
   hands over its DOM helpers once (use) and asks for a section (comments, render); what this file keeps is per bead id: a
   conversation unfolded, its Commons place, the lines sent from here and the reply typed, until the panel closes (reset); and
   the open panel's Commons stream, which keeps its conversation live (listen). */
var BEADTALK = (function () {
  /* ---------- pure logic (also run under node by the Go tests) ---------- */
  /* The Conversation section (gd-tx2wMH48MV): the thread's messages (kin.conversation, internal/kin) and the bead's own kd
     comments as one timeline, oldest first, a comment after a message of the same second. Each line is {k, at, ...}: a
     message {k: 'msg', id, who, voice, via, text, gone}, a card {k: 'card', id, who, state, agent} (the agent bead it is the
     card of, its name and state word from kin.agents), a comment {k: 'comment', who, text}. null for a bead in no thread. */
  var CONV_FOLD = 12;
  function words(s) { return String(s == null ? '' : s).replace(/[_]+/g, ' ').trim(); }   // as workbench.js words
  var VIA = { slack: 'in Slack', squawk: 'squawk', commons: 'in the Commons', say: 'said', card: 'card', mention: 'mentions this', mail: 'mail' };
  function who(p) {
    p = String(p || '');
    if (/^oidc:/.test(p)) return p.slice(5).split('@')[0];
    if (/^(agent|slack):/.test(p)) return p.replace(/^[a-z]+:/, '');
    return p || 'someone';
  }
  function talk(d) {
    var k = (d || {}).kin, agents = {}, lines = [];
    if (!k || !k.conversation) return null;
    (k.agents || []).forEach(function (a) { agents[a.id] = a; });
    k.conversation.forEach(function (l, i) {
      if (l.via === 'card') {
        var a = agents[l.subject];
        lines.push({ k: 'card', id: l.id, at: l.at || 0, n: i, agent: l.subject || '', who: a ? a.name : String(l.text || '').replace(/^Agent:\s*/, '') || who(l.author),
          state: a ? words(a.state) || (a.live ? 'at work' : 'left') : '' });
        return;
      }
      lines.push({ k: 'msg', id: l.id, at: l.at || 0, n: i, who: who(l.author), voice: l.voice || 'agent', via: VIA[l.via] || '', text: l.text || '', gone: !!l.deleted });
    });
    ((d || {}).comments || []).forEach(function (c, i) { lines.push({ k: 'comment', at: c.at || 0, n: k.conversation.length + i, who: who(c.author), text: c.text || '' }); });
    return lines.sort(function (a, b) { return a.at - b.at || a.n - b.n; });
  }
  /* The voice of a line by author (kin.go voiceOf): steward for one of the land's steward runs, person for a person, else agent. */
  function voice(author, land) {
    var a = String(author || ''), name = a.replace(/^agent:/, '').replace(/^oidc:/, '');
    if (land && name.indexOf(land + '-steward-') === 0) return 'steward';
    return /^(oidc|slack):/.test(a) || a.indexOf('@') >= 0 ? 'person' : 'agent';
  }
  function cardState(c) { return words(c.state) || (c.live ? 'at work' : 'left'); }
  /* The line a message the Commons stream brought makes (web/commons2.js put), l: {id, author, voice, via, at, text, gone,
     card: {agent, state, live, name}}, voice and via as the row says them (kin.Speaker), text null when the row does not carry
     its words. Over was, the line the conversation has for it: its words, its deletion or its card's state as the stream has
     them now. Else a new line, named and voiced as talk names one. */
  function heard(l, was, land) {
    if (was) {
      var n = {};
      Object.keys(was).forEach(function (k) { n[k] = was[k]; });
      if (was.k === 'card') { if (l.card) n.state = cardState(l.card); } else if (l.gone) n.gone = true; else if (l.text != null) n.text = l.text;
      return n;
    }
    if (l.card) return { k: 'card', id: l.id, at: l.at || 0, agent: l.card.agent || '', who: l.card.name || who(l.author), state: cardState(l.card) };
    return { k: 'msg', id: l.id, at: l.at || 0, who: who(l.author), voice: l.voice || voice(l.author, land), via: VIA[l.via] || VIA.commons, text: l.text || '', gone: !!l.gone };
  }

  var api = { talk: talk, voice: voice, heard: heard, CONV_FOLD: CONV_FOLD };
  if (typeof document === 'undefined') return api;

  /* ---------- DOM ---------- */
  var convAll = {}, places = {}, sent = {}, drafts = {}, drawn = {};   // by bead id: its conversation unfolded, its Commons place, the lines sent from here (or heard), the reply typed, the lines its document drew by id, their titles and the list
  var el, sec, button, href, ago, plural, markdown, mdInto, fold, rich, retryButton, ask, B, W, BW, base, own;   // the panel's (use); own: the page has the panel
  api.use = function (h) {
    el = h.el; sec = h.sec; button = h.button; href = h.href; ago = h.ago; plural = h.plural; markdown = h.markdown; mdInto = h.mdInto; fold = h.fold;
    rich = h.rich; retryButton = h.retryButton; ask = h.ask; B = h.B; W = h.W; BW = h.BW; base = h.base; own = !!h.live;
  };
  api.reset = function () { convAll = {}; places = {}; hush(); want = null; };

  /* The Comments section of a bead in no thread: the composer, then the newest comments first. */
  function comments(k, x, titles) {
    var cs = (x.comments || []).slice().reverse(), cl = el('ul', 'bv-comments');   // newest first
    cs.forEach(function (c) {
      var li = el('li'), h = el('div', 'bv-ch');
      h.appendChild(el('b', '', c.author || 'someone'));
      if (c.at) h.appendChild(el('span', '', ' · ' + ago(c.at) + ' ago'));
      li.appendChild(h); li.appendChild(rich('bv-text', c.text, titles)); cl.appendChild(li);
    });
    var total = x.comments_total || 0, cbox = el('div'), cf = BW && BW.composer(x);
    var unread = (x.partial || []).indexOf('comments') >= 0;   // never "No comments." for unsent ones (WB-15)
    if (cf) cbox.appendChild(cf);   // the composer first, newest comments under it (WB-3)
    if (unread) {
      var un = el('p', 'bv-note err', 'The comments could not be read. '), ub = retryButton(k.id);
      ub.setAttribute('aria-label', 'Try reading the comments again');
      un.setAttribute('role', 'status'); un.appendChild(ub); cbox.appendChild(un);
    } else if (cs.length) cbox.appendChild(cl); else cbox.appendChild(el('p', 'bv-empty', 'No comments.'));
    var next = !unread && BW ? BW.older(cs.length, total) : 0;
    if (next) {   // Show older: ask for more of the newest comments (bead.go ?comments=N) and repaint
      var ob = button('Show older comments (' + (total - cs.length) + ' more)', function () { ask(x.id, next); ob.disabled = true; ob.textContent = 'Loading…'; W.reload('bead', x.id); });
      ob.className = 'btn small ghost bv-more'; cbox.appendChild(ob);
    } else if (!unread && total > cs.length) cbox.appendChild(el('p', 'bv-note', plural(total - cs.length, 'older comment is', 'older comments are') + ' not shown.'));
    return sec(unread ? 'Comments' : 'Comments (' + total + ')', cbox);
  }

  /* The Conversation section (gd-tx2wMH48MV): the thread's lines and the bead's comments, oldest first (talk), the latest
     CONV_FOLD shown and the rest behind Show older, which shows them in place; with every line shown, older comments the
     document did not send are asked for as the Comments section asks for them (?comments=N). On a phone the list scrolls by
     itself and opens on its newest line. Its foot is the thread's reply composer (sayBox), and a writer's comment composer. */
  function render(x, lines, titles) {
    var box = el('div', 'bv-cv-box'), ol = el('ol', 'bv-cv-list'), hid = convAll[x.id] ? 0 : Math.max(0, lines.length - CONV_FOLD), have = {};
    drawn[x.id] = { lines: have, titles: titles, ol: ol };
    var cs = x.comments || [], total = x.comments_total || 0, unread = (x.partial || []).indexOf('comments') >= 0, next = !unread && BW ? BW.older(cs.length, total) : 0;
    var older = function () {   // the comments before the ones sent, once every line sent is shown
      if (next) {
        var ob = button('Show older comments (' + (total - cs.length) + ' more)', function () { ask(x.id, next); ob.disabled = true; ob.textContent = 'Loading…'; W.reload('bead', x.id); });
        ob.className = 'btn small ghost bv-more'; box.insertBefore(ob, ol);
      } else if (!unread && total > cs.length) box.insertBefore(el('p', 'bv-note', plural(total - cs.length, 'older comment is', 'older comments are') + ' not shown.'), ol);
      if (x.kin.more) box.insertBefore(el('p', 'bv-note', 'Older messages of the thread are not shown here.'), ol);
    };
    ol.setAttribute('aria-label', 'Conversation, oldest first');
    lines.forEach(function (l, i) { var li = ol.appendChild(convLine(l, titles)); li.hidden = i < hid; if (l.id) have[l.id] = l; });
    sent[x.id] = (sent[x.id] || []).filter(function (m) { return !have[m.id]; });   // a line sent here stays until the document carries it
    sent[x.id].forEach(function (m) { m.li = ol.appendChild(convLine(mine(x.id, m), titles)); });
    if (!ol.children.length) box.appendChild(el('p', 'bv-empty', 'No messages yet.'));
    box.appendChild(ol);
    if (hid) {
      var sb = button('Show older (' + hid + ')', function () {
        convAll[x.id] = true; sb.remove();
        var first = null;
        Array.prototype.forEach.call(ol.children, function (li) { if (li.hidden && !first) first = li; li.hidden = false; });
        older();
        if (first) { first.tabIndex = -1; first.focus({ preventScroll: true }); first.scrollIntoView({ block: 'nearest' }); }
      });
      sb.className = 'btn small ghost bv-more'; box.insertBefore(sb, ol);
    } else older();
    var foot = box.appendChild(el('div', 'bv-cv-foot'));
    if (!x.kin.no_thread) sayBox(x, foot, ol);   // a bead in no thread has no thread to reply in: its comment composer is open
    else listen(x, null);
    var cf = BW && BW.composer(x);
    if (cf) {   // a writer's kd comment, which joins the timeline in the comment voice
      var kd = box.appendChild(el('details', 'bv-fold bv-cv-kd'));
      kd.open = !!x.kin.no_thread;
      kd.appendChild(el('summary', '', 'Add a comment to the bead'));
      kd.appendChild(cf);
    }
    var raf = window.requestAnimationFrame || function (f) { return setTimeout(f, 16); };
    var says = sent[x.id].filter(function (m) { return m.line ? m.line.k === 'msg' && !m.line.gone : !m.gone; }).length;   // replies the face's count lacks
    raf(function () {
      if (says && ol.isConnected) bump(ol, 0, says);
      newest(ol);
    });
    return sec('Conversation (' + (lines.length + sent[x.id].length) + ')', box, 'conversation');
  }
  /* A phone's list (garden.css: it scrolls by itself under 900 px) no taller than the pane shows under it at the pane's top,
     so its newest line is on screen unscrolled; never under about three lines (rubric C11, gd-1jjjx7CMU3). */
  /* A phone's list opens on its newest line, once it is laid out: a document drawn behind a phone's tree screen is not yet. */
  function newest(ol) {
    var go = function () { fit(ol); if (ol.scrollHeight > ol.clientHeight) ol.scrollTop = ol.scrollHeight; };
    if (!ol.isConnected || ol.offsetHeight || typeof ResizeObserver === 'undefined') { if (ol.isConnected) go(); return; }
    var ro = new ResizeObserver(function () { if (!ol.isConnected) ro.disconnect(); else if (ol.offsetHeight) { ro.disconnect(); go(); } });
    ro.observe(ol);
  }
  function fit(ol) {
    if (!ol.isConnected || getComputedStyle(ol).overflowY !== 'auto') return;
    var pane = ol.parentElement, top = ol.getBoundingClientRect().top, bottom = window.innerHeight;
    for (; pane && pane !== document.body; pane = pane.parentElement) {   // the panes it scrolls in: their top, unscrolled, and the lowest bottom
      if (!/auto|scroll/.test(getComputedStyle(pane).overflowY) || pane.scrollHeight <= pane.clientHeight) continue;
      top += pane.scrollTop; bottom = Math.min(bottom, pane.getBoundingClientRect().bottom);
    }
    ol.style.maxHeight = Math.max(160, Math.min(Math.floor(window.innerHeight * 0.55), Math.floor(bottom - top) - 8)) + 'px';   // garden.css's 55vh at most
  }
  /* One line of the conversation: a voice mark (a squawk's its own), who, how it came, when, then its text as markdown (bead
     ids and URLs as links). A card is one line: its agent, linked to the agent's panel, and the agent's state, never prose. */
  function convLine(l, titles) {
    var li = el('li', 'bv-cv bv-cv--' + (l.k === 'msg' ? l.voice : l.k)), m = li.appendChild(el('p', 'bv-cv-meta'));
    if (l.id) li.dataset.id = l.id;
    if (l.k === 'msg' && l.via === VIA.squawk) li.dataset.via = 'squawk';
    m.appendChild(el('span', 'bv-cv-mark')).setAttribute('aria-hidden', 'true');
    if (l.k === 'card' && l.agent) { var a = m.appendChild(el('a', 'bl bv-cv-who', l.who)); a.href = href(l.agent); a.title = l.who + ' (' + l.agent + ')'; }
    else m.appendChild(el('b', 'bv-cv-who', l.who));
    if (l.k === 'card' && l.state) m.appendChild(el('span', 'pc bv-cv-state', l.state));
    var via = l.k === 'comment' ? 'comment' : l.k === 'card' ? 'card' : l.via;
    if (via) m.appendChild(el('span', 'bv-cv-via', via));
    if (l.at) { var t = m.appendChild(el('time', 'bv-cv-at', ago(l.at) + ' ago')); t.dateTime = new Date(l.at * 1000).toISOString(); }
    if (l.pending) {
      var st = m.appendChild(el('span', 'bv-cv-st' + (l.retry ? ' err' : ''), l.pending));
      if (l.retry) { st.setAttribute('role', 'alert'); m.appendChild(button('Retry', l.retry)).className = 'btn small bv-cv-retry'; }
    }
    if (l.k === 'card') return li;
    if (l.gone) li.appendChild(el('p', 'bv-cv-text bv-cv-gone', 'Message deleted'));
    else if (l.text) li.appendChild(longText(l.text, titles));
    return li;
  }
  /* A line's words as markdown, past about 600 characters folded behind Show all, opened and shut in place (rubric C11, as
     the face's lead folds; web/cuts.js). */
  function longText(text, titles) {
    var f = fold ? fold(text) : { rest: '' }, box = mdInto(el('div', 'bv-cv-text bv-md'), markdown(f.rest ? f.head : text), titles);
    if (!f.rest) return box;
    return CUTS.make(box, function () { return mdInto(el('div', 'bv-cv-text bv-md'), markdown(text), titles); }, 'Show all ' + f.total.toLocaleString('en-US') + ' characters');
  }
  /* The thread's reply composer, where the Commons offers one: GET /garden/c/bead/{id} (commons place.go) names the thread's
     reply route and the line the thread's fragment shows instead of it, or above it. No Commons, or no place: no composer.
     Enter sends (Shift+Enter, or any Enter under a finger, is a new line), as in the thread pane. */
  function sayBox(x, foot, ol) {
    var got = function (p) {   // the foot is not yet in the page when p was read before: a repaint
      listen(x, p);
      if (!p) return;
      if (p.note) foot.appendChild(el('p', 'bv-note bv-cv-note', p.note)).setAttribute('role', 'status');
      if (!p.reply) return;
      var f = foot.appendChild(el('form', 'bv-cv-say')), ta = f.appendChild(el('textarea', 'bw-text')), go = f.appendChild(el('button', 'btn small', 'Send'));
      ta.name = 'text'; ta.rows = 2; ta.maxLength = 10000; ta.placeholder = B.dataset.emptyPrompt || ''; ta.setAttribute('aria-label', 'Reply in thread');
      ta.value = drafts[x.id] || '';
      ta.oninput = function () { if (ta.value) drafts[x.id] = ta.value; else delete drafts[x.id]; };
      go.type = 'submit';
      var say = function () { var t = ta.value.trim(); if (!t) { ta.focus(); return; } ta.value = ''; delete drafts[x.id]; sendLine(x.id, p.reply, t, ol); ta.focus(); };
      ta.onkeydown = function (e) { if (e.key === 'Enter' && !e.shiftKey && !e.isComposing && !(window.matchMedia && matchMedia('(pointer: coarse)').matches)) { e.preventDefault(); say(); } };
      f.onsubmit = function (e) { e.preventDefault(); say(); };
    };
    if (places[x.id]) { got(places[x.id]); return; }
    fetch(base + '/garden/c/bead/' + encodeURIComponent(x.id), { credentials: 'same-origin', cache: 'no-store', headers: { Accept: 'application/json' } })
      .then(function (r) { return r.ok ? r.json() : null; }, function () { return null; })
      .then(function (p) { if (p) places[x.id] = p; if (foot.isConnected) got(p); }, function () { /* no place: no composer */ });
  }
  /* Send, as the thread pane's composer sends (web/commons2.js post): the words and a nonce to the thread's reply route, so a
     Retry of a send that was only slow writes nothing twice (commons/sendonce.go). The line shows at once, pending; once the
     message is written the bead is read again, and its document carries it. */
  function sendLine(id, action, text, ol) {
    var m = { id: '', text: text, at: Math.floor(Date.now() / 1000), nonce: nonce(), action: action, why: '' };
    (sent[id] = sent[id] || []).push(m);
    m.li = ol.appendChild(convLine(mine(id, m), {}));
    bump(ol, 1, 1);
    m.li.scrollIntoView({ block: 'nearest' });
    postLine(id, m);
  }
  function mine(id, m) {
    if (m.line) return m.line;   // heard
    return { k: 'msg', id: m.id, at: m.at, who: 'You', voice: 'person', via: VIA.commons, text: m.text, gone: m.gone, pending: m.id ? '' : m.why || 'Sending…',
      retry: m.why ? function () { m.why = ''; redrawLine(id, m); postLine(id, m); } : null };
  }
  function redrawLine(id, m) { if (m.li && m.li.isConnected) { var n = convLine(mine(id, m), (drawn[id] || {}).titles || {}); m.li.replaceWith(n); m.li = n; } }
  /* One attempt at m's send. With no answer in 3 s it says so and offers Retry, the same nonce again, while this attempt goes
     on; any attempt's answer settles it, and only the last one's refusal says so (as commons2.js post, gd-JpYV1FWtnZ). */
  function postLine(id, m) {
    var n = m.n = (m.n || 0) + 1;
    clearTimeout(m.t); m.t = setTimeout(function () { if (!m.id && !m.why) { m.why = 'Not sent yet.'; redrawLine(id, m); } }, 3000);
    fetch(m.action, { method: 'POST', credentials: 'same-origin', cache: 'no-store', headers: { 'Content-Type': 'application/x-www-form-urlencoded', 'X-Commons': 'fragment' },
      body: new URLSearchParams({ text: m.text, nonce: m.nonce }).toString() })
      .then(function (r) { return r.text().then(function (t) { if (!r.ok) throw new Error(t.trim() || 'That did not go through (' + r.status + ').'); return t; }); },
        function () { throw new Error('The garden could not be reached.'); })
      .then(function (t) {
        var tpl = document.createElement('template'); tpl.innerHTML = t;   // the thread's own row for it (commons render.go "message"); only its id is read
        var row = tpl.content.querySelector('[data-id]');
        clearTimeout(m.t);
        if (m.id) return;   // another attempt, or the stream, settled it first
        m.id = row ? row.dataset.id : 'sent:' + m.nonce; m.why = '';
        redrawLine(id, m);
        W.refresh('bead', id);
      }, function (e) { if (n !== m.n || m.id) return; clearTimeout(m.t); m.why = 'Not sent: ' + e.message; redrawLine(id, m); });
  }
  /* A message the Commons stream brought for bead id's thread (web/commons2.js put, the node's document), l as heard takes it
     plus its nonce, in a thread of land. A line sent from here (its nonce) is settled; a line already in the conversation ol,
     drawn by the document or heard before, is drawn again as the stream has it now (heard); any other goes at the end of ol,
     kept, as a sent line is, until the document carries it; a line the stream has as it is drawn (a replay) is left alone.
     False when the row did not carry what the line shows (its text drawn as a document, or a face other than an agent's
     card): the document's read draws that. */
  api.hear = function (id, ol, l, land) {
    var s = sent[id] = sent[id] || [], dr = drawn[id] || {}, can = l.text != null || !!l.gone || !!l.card;
    var m = s.filter(function (x) { return (l.nonce && x.nonce === l.nonce) || x.id === l.id; })[0];
    var same = function (a, b) { return JSON.stringify(a) === JSON.stringify(b); };
    if (m) {
      var before = mine(id, m);
      if (!m.id) { m.id = l.id; m.why = ''; clearTimeout(m.t); }
      if (m.line) m.line = heard(l, m.line, land); else if (l.gone) m.gone = true; else if (l.text != null) m.text = l.text;
      if (!same(before, mine(id, m))) redrawLine(id, m);
      return can;
    }
    var li = ol.querySelector(':scope > li[data-id="' + CSS.escape(l.id) + '"]'), was = li && dr.lines && dr.lines[l.id], now = was && heard(l, was, land);
    if (now && !same(now, was)) { var n = convLine(dr.lines[l.id] = now, dr.titles || {}); n.hidden = li.hidden; li.replaceWith(n); }
    if (li) return can;
    var none = ol.parentNode && ol.parentNode.querySelector(':scope > .bv-empty');
    if (none) none.remove();
    m = { id: l.id, nonce: l.nonce || '', line: heard(l, null, land) };
    s.push(m); m.li = ol.appendChild(convLine(m.line, dr.titles || {}));
    bump(ol, 1, m.line.k === 'msg' && !m.line.gone ? 1 : 0);
    return can;
  };
  /* The counts drawn over the conversation ol, by n lines and r replies more than its document carries: its section's heading
     and a wish face's replies (web/wishface.js, the part that links to the section). */
  function bump(ol, n, r) {
    var s = ol.closest('.bv-sec'), h = s && s.querySelector(':scope > h3'), a = s && s.parentNode && s.parentNode.querySelector('.bv-face-sum a[data-sec="conversation"]');
    if (h && n) h.textContent = h.textContent.replace(/\((\d+)\)$/, function (all, k) { return '(' + (+k + n) + ')'; });
    if (a && r) { var k = +((/^\d+/.exec(a.textContent) || [0])[0]) + r; a.textContent = plural(k, 'reply', 'replies'); }
  }
  /* A message row of the Commons (commons render.go "message", as the thread draws it and the stream carries it) as the line
     hear takes, and the thread it is in (thread, '' for a head); null for no row. */
  api.row = function (li) {
    if (!li || !li.dataset || !li.dataset.id) return null;
    var t = li.querySelector('.cm-meta time'), au = li.querySelector('.cm-meta .cm-author'), card = li.querySelector('.cm-acard'), gone = !!li.querySelector('.cm-tombstone'), nm;
    var l = { id: li.dataset.id, thread: li.dataset.thread || '', nonce: li.dataset.nonce || '', author: au ? au.title : '', voice: li.dataset.voice || '', via: li.dataset.via || '',
      at: t ? Math.floor(Date.parse(t.getAttribute('datetime')) / 1000) || 0 : 0, gone: gone, text: gone ? null : rowWords(li.querySelector(':scope > .cm-text')) };
    if (card) l.card = { agent: card.dataset.subject || '', state: card.dataset.state || '', live: card.classList.contains('cm-acard--live'), name: (nm = card.querySelector('.cm-acard-name')) ? nm.textContent.trim() : '' };
    return l;
  };
  /* the words a row's text was written as (commons render.go linkify: a bead link is its id, a URL link its href); null for
     none, or for text drawn as a document (cm-doc) */
  function rowWords(tx) {
    if (!tx || tx.classList.contains('cm-doc')) return null;
    return [].map.call(tx.childNodes, function (n) { return n.nodeType === 3 ? n.data : (n.dataset && n.dataset.bead) || (n.classList && n.classList.contains('cm-link') && n.title) || n.textContent; }).join('');
  }

  /* The conversation live (gd-MpqfrPhLZP): the panel's bead holds one Commons stream (commons stream.go), its id the root and,
     once its place is read (sayBox), in its thread's channel. A message or card of the thread goes in in place (hear); a bead
     under it that moved (an agent's state, a task closing) patches its Work row (web/beadwork.js node). The document is read
     again only for a row hear cannot draw, or a reset. None without an EventSource, in a hidden tab, or on a page without the
     panel (the Commons node's streams in commons2.js); the panel's own stream and poll (workbench.js live) go on as they are. */
  var tap = null, want = null;   // the stream {id, q, thread, land, es}; the bead and place it was asked for
  function listen(x, p) {
    want = { x: x, p: p };
    if (!own || typeof LIVE === 'undefined' || typeof EventSource === 'undefined' || document.hidden) return;
    var q = '?root=' + encodeURIComponent(x.id) + (p && p.land && p.channel ? '&land=' + encodeURIComponent(p.land) + '&channel=' + encodeURIComponent(p.channel) : '');
    if (tap && tap.id === x.id && tap.q === q) return;
    hush();
    var t = tap = { id: x.id, q: q, thread: (p && p.thread) || '', land: (p && p.land) || '' }, es = t.es = LIVE.source(base + '/garden/c/events' + q);   // web/live.js
    var on = function (type, fn) {
      es.addEventListener(type, function (e) {
        var c = W.current(), d;
        if (tap !== t) return;
        if (!c || c.kind !== 'bead' || c.id !== t.id) { hush(); return; }   // the panel went on to another bead
        try { d = JSON.parse(e.data); } catch (z) { return; }
        fn(d);
      });
    };
    ['message', 'card'].forEach(function (type) {
      on(type, function (d) {
        var ol = (drawn[t.id] || {}).ol, tpl, l;
        if (!t.thread || (d.thread || d.id) !== t.thread || !ol || !ol.isConnected) return;
        if (!d.html) { if (d.stale) again(t.id); return; }
        tpl = document.createElement('template'); tpl.innerHTML = d.html;
        l = api.row(tpl.content.firstElementChild);
        if (!l || l.thread !== t.thread) return;   // the head's own row is the bead's: its own stream says it moved
        if (!api.hear(t.id, ol, l, t.land)) again(t.id);
      });
    });
    on('node', function (d) { if (d.node && d.node.id !== t.id && typeof BEADWORK !== 'undefined') BEADWORK.node(d.node); });
    on('reset', function () { again(t.id); });
    es.onopen = function () { t.open = true; };
    es.onerror = function () { t.open = false; };
  }
  function hush() { if (tap) { tap.es.close(); tap = null; } }
  function again(id) { clearTimeout(again.t); again.t = setTimeout(function () { W.moved('bead', id); }, 400); }   // read once its events pause, unless the viewer writes there
  document.addEventListener('visibilitychange', function () {
    var o = want && (drawn[want.x.id] || {}).ol;
    if (document.hidden) hush(); else if (o && o.isConnected) listen(want.x, want.p);
  });
  api.listening = function () { return tap && tap.open ? tap.id : ''; };   // the bead whose stream is open, '' for none
  function nonce() { try { return crypto.randomUUID(); } catch (e) { return Date.now().toString(36) + '-' + Math.random().toString(36).slice(2, 12); } }

  api.comments = comments;
  api.render = render;
  return api;
})();
if (typeof module !== 'undefined') module.exports = BEADTALK;
