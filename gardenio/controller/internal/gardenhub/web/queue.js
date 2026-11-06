/* web/queue.js: the Queues panel (Lands workbench-glass spec WB-6, step 5.5, gd-HuJdsFabSb), the Workbench's queue kind,
   /garden?queue=<tab>. Three tabs, Inbox, Decisions and Escalations, each read from GET /garden/queue/<tab> (queue.go:
   the viewer's own inbox, decisions and escalations builders, sorted by attention, what needs the viewer first, then the
   one that has waited longest). It defaults to the viewer's own ("Mine"); "Everyone's" asks for ?all=1, and the address
   keeps it (all=1) so a reload or a shared link shows the same list. The Mailbox opens it on the tab that has something
   for the viewer.

   Each row's title opens its bead here. An open decision carries the Mailbox's own answer box and an escalation its
   Approve and Reject (web/answer.js posts them, to an account that may approve); the rest are the viewer's own POST routes
   asked for JSON: Pin and Worthy on an inbox row, Dismiss and Reflag on a decision, offered where the page lets the viewer
   write (data-edit). An answered row leaves the list and focus moves to the next one. Text goes in through textContent. */
var QUEUE = (function () {
  /* ---------- pure logic (also run under node by the Go tests) ---------- */
  var TABS = [['inbox', 'Inbox'], ['decisions', 'Decisions'], ['escalations', 'Escalations']];
  function isTab(t) { return TABS.some(function (x) { return x[0] === t; }); }
  function tabWord(t) { var x = TABS.filter(function (x) { return x[0] === t; })[0]; return x ? x[1] : t; }
  /* The tab's JSON under base. */
  function url(tab, all) { return '/garden/queue/' + encodeURIComponent(tab) + (all ? '?all=1' : ''); }
  /* The tab the Mailbox opens, from the types of the asks it holds: decisions when it holds one, escalations when it holds
     only those, else the inbox (nothing is asked of the viewer, so what is left for them is there). */
  function pick(types) {
    types = types || [];
    if (types.indexOf('decision') >= 0) return 'decisions';
    if (types.indexOf('escalation') >= 0) return 'escalations';
    return 'inbox';
  }
  function span(sec) {
    if (sec < 3600) return Math.max(1, Math.round(sec / 60)) + 'm';
    if (sec < 86400) return Math.round(sec / 3600) + 'h';
    return Math.round(sec / 86400) + 'd';
  }
  var KIND = { intake: 'intake', forwarded: 'forwarded', epic: 'epic', agent: 'agent', decision: 'decision', escalation: 'escalation' };
  /* The bead's state, as the search panel words it (web/search.js). An open ask says nothing more: waiting is its state. */
  var STATUS = { open: 'open', in_progress: 'in progress', blocked: 'blocked', deferred: 'deferred', closed: 'closed' };
  function state(r) {
    if (r.kind === 'escalation' && r.state && r.state !== 'pending') return r.state;   // approved, rejected, done or failed: not its bead status
    if (!r.status || ((r.kind === 'decision' || r.kind === 'escalation') && r.status === 'open')) return '';
    return STATUS[r.status] || r.status.replace(/_/g, ' ');
  }
  /* Whether an escalation row may still be approved or rejected: pending (or unsaid, an older server). An approved one
     waits on its executor, not on anyone here (gd-yuOG6cX6VI). */
  function pending(r) { return r.kind === 'escalation' && (!r.state || r.state === 'pending'); }
  /* Who answers an escalation, in words. */
  var AUDIENCE = { owner: 'the owner', operator: 'an operator', desk: 'the merge desk' };
  /* Whether a row waits on this viewer: an escalation the viewer may not approve waits on whoever may, not on them. */
  function needs(r, approve) { return !!r.needs_you && !(r.kind === 'escalation' && (!approve || !pending(r))); }
  /* The line a row carries in place of an answer box it cannot offer: who approves it, and that this account cannot. */
  function who(r, approve) {
    if (r.kind !== 'escalation' || !pending(r) || approve) return '';
    return (AUDIENCE[r.audience] || AUDIENCE.owner).replace(/^./, function (c) { return c.toUpperCase(); }) + ' approves or rejects this; your account cannot.';
  }
  /* An ask waits; an inbox row was last active. */
  function waited(r, now) {
    if (!(r.since > 0)) return '';
    var s = span(Math.max(60, now - r.since));
    return r.kind === 'decision' || r.kind === 'escalation' ? 'waiting ' + s : s + ' ago';
  }
  /* The words under a row's title: why it needs the viewer first, then what it is, its state, its land, who, how long, its
     MR. Every state is a word (never colour alone). */
  function line(r, now) {
    var out = [];
    if (r.needs_you) out.push(r.why || 'needs you');
    var k = KIND[r.kind] || r.kind || '';
    if (r.kind === 'escalation' && r.operation) k = r.operation.replace(/_/g, ' ');
    if (k) out.push(k);
    var st = state(r);
    if (st) out.push(st);
    if (r.worthy) out.push('inbox-worthy');
    if (r.kind === 'escalation' && r.audience && r.audience !== 'owner') out.push('in hand: ' + r.audience);
    if (r.sensitivity) out.push(r.sensitivity);
    if (r.project) out.push(r.project);
    if (r.actor) out.push(r.actor);
    var w = waited(r, now);
    if (w) out.push(w);
    if (r.mr) out.push('MR ' + r.mr.replace(/_/g, ' '));
    return out;
  }
  /* The writes a row offers besides the answer box, each the viewer's own route: Pin and Worthy (both toggles, labelled by
     what a tap does, pressed while set) on an inbox row, Dismiss and Reflag on a decision. None without edit. */
  function verbs(r, edit) {
    if (!edit || !r || !r.id) return [];
    var b = '/b/' + encodeURIComponent(r.id);
    if (r.kind === 'decision') return [
      { act: 'dismiss', path: b + '/decision/dismiss', word: 'Dismiss', arm: 'Tap again to dismiss', reason: true, done: 'Dismissed ' + r.id + '.', gone: true },
      { act: 'reflag', path: b + '/decision/reflag', word: 'Reflag', arm: 'Tap again to reflag', reason: true, done: 'Reflagged ' + r.id + '.' }
    ];
    if (r.kind === 'escalation') return [];
    return [
      { act: 'pin', path: b + '/inbox-pin/toggle', word: r.pinned ? 'Unpin' : 'Pin', pressed: !!r.pinned, done: (r.pinned ? 'Unpinned ' : 'Pinned ') + r.id + '.' },
      { act: 'worthy', path: b + '/inbox-worthy/toggle', word: r.worthy ? 'Clear inbox-worthy' : 'Mark inbox-worthy', pressed: !!r.worthy, done: (r.worthy ? 'Cleared inbox-worthy on ' : 'Marked inbox-worthy: ') + r.id + '.' }
    ];
  }
  /* The answer box a row carries (workbench.js answerBox's input), or null: an open decision's options, an escalation's
     Approve and Reject only for an account that may approve, and only while it is pending. */
  function asks(r, approve) {
    if (r.kind === 'decision') return { kind: 'decision', id: r.id, title: r.title, options: r.options || [] };
    if (pending(r) && approve) { var a = { kind: 'escalation', id: r.id, title: r.title }; if (r.held) a.held = r.held; return a; }
    return null;
  }
  /* The empty tab, in words. */
  function empty(tab, all) {
    if (tab === 'decisions') return all ? 'No decision is open.' : 'No decision is waiting on you.';
    if (tab === 'escalations') return all ? 'No escalation is open.' : 'No escalation is waiting on the owner.';
    return all ? 'The inbox is empty.' : 'Your inbox is empty.';
  }
  /* The query with all=1 set or dropped, every other parameter kept as written. */
  function withAll(search, all) {
    var keep = String(search || '').replace(/^\?/, '').split('&').filter(function (p) { return p && p.split('=')[0] !== 'all'; });
    if (all) keep.push('all=1');
    return keep.length ? '?' + keep.join('&') : '';
  }
  /* Whether a lists change (garden.js: rm ids, up {id, sec, ty}, or reset) concerns tab, listing ids: a row of it moved or
     left, or an ask of its kind came or changed. */
  function touches(tab, ch, ids) {
    function hit(id) { return ids.indexOf(id) >= 0; }
    return !!ch.reset || (ch.rm || []).some(hit) || (ch.up || []).some(function (f) { return hit(f.id) || (f.sec === 'decision' && tab !== 'inbox' && (!f.ty || f.ty + 's' === tab)); });
  }
  var api = { TABS: TABS, isTab: isTab, tabWord: tabWord, url: url, pick: pick, line: line, needs: needs, who: who, verbs: verbs, asks: asks, empty: empty, withAll: withAll, waited: waited, touches: touches };
  if (typeof document === 'undefined' || typeof WORKBENCH === 'undefined') return api;

  /* ---------- DOM ---------- */
  var W = WORKBENCH, B = document.body, base = B.dataset.base || '';
  var titleEl = document.getElementById('bv-title'), kindEl = document.getElementById('bv-kind'), bodyEl = document.getElementById('bv-body');
  if (!bodyEl) return api;
  var listEl = null, noteEl = null;

  function el(t, cls, text) { var e = document.createElement(t); if (cls) e.className = cls; if (text !== undefined) e.textContent = text; return e; }
  function href(id) { return base + '/garden?bead=' + encodeURIComponent(id); }
  function cur() { var c = W.current(); return c && c.kind === 'queue' ? c.id : ''; }
  function say(msg, err) { if (!noteEl) return; noteEl.textContent = msg; noteEl.className = 'bv-note' + (err ? ' err' : ''); }
  /* "Everyone's" lives in the address only, so Back to a queue entry shows the list it showed. */
  function allOn() { return W.param(location.search, 'all') === '1'; }
  function setAll(on) {
    try { if (history.replaceState) history.replaceState(history.state, '', location.pathname + withAll(location.search, on) + location.hash); } catch (e) { /* a sandboxed frame */ }
  }
  function ref(r, word) {
    if (!r || !r.id) return null;
    var name = r.short_title || r.title, a = el('a', 'bl', name || r.id); a.href = href(r.id); a.title = r.title ? r.title + ' (' + r.id + ')' : r.id;
    if (name && typeof TITLES !== 'undefined') TITLES.learn(r.id, name);
    var s = el('span', 'qv-ref'); s.appendChild(document.createTextNode(word + ' ')); s.appendChild(a);
    if (typeof TITLES !== 'undefined') TITLES.unseen(a, r.id, r.title);   // gd-im6yFl7Nk1
    return s;
  }

  function tabs(tab) {
    var row = el('div', 'qv-tabs');
    row.setAttribute('role', 'tablist'); row.setAttribute('aria-label', 'Queues');
    TABS.forEach(function (t) {
      var b = el('button', 'btn small' + (t[0] === tab ? '' : ' ghost'), t[1]);
      b.type = 'button'; b.setAttribute('role', 'tab'); b.setAttribute('aria-selected', t[0] === tab ? 'true' : 'false');
      b.setAttribute('aria-controls', 'bv-body');
      b.onclick = function () { if (t[0] !== tab) W.open('queue', t[0], { replace: true }); };
      row.appendChild(b);
    });
    /* left and right walk the tabs, as a tablist does */
    row.addEventListener('keydown', function (e) {
      if (e.key !== 'ArrowLeft' && e.key !== 'ArrowRight') return;
      var i = TABS.map(function (t) { return t[0]; }).indexOf(tab), n = TABS.length;
      e.preventDefault();
      W.open('queue', TABS[(i + (e.key === 'ArrowRight' ? 1 : n - 1)) % n][0], { replace: true });
    });
    return row;
  }
  function scopeRow(tab, d) {
    var row = el('div', 'qv-scope');
    row.setAttribute('role', 'group'); row.setAttribute('aria-label', 'Whose');
    [['Mine', false], ['Everyone’s', true]].forEach(function (o) {
      var b = el('button', 'btn small' + (o[1] === !!d.all ? '' : ' ghost'), o[0]);
      b.type = 'button'; b.setAttribute('aria-pressed', o[1] === !!d.all ? 'true' : 'false');
      b.onclick = function () { if (o[1] === allOn()) return; setAll(o[1]); W.reload('queue', tab); };
      row.appendChild(b);
    });
    return row;
  }
  /* One write: a toggle posts at once; Dismiss and Reflag answer on their second tap, with the reason typed in the row. */
  function verbButton(v, r, li, tab) {
    var b = el('button', 'btn small ghost', v.word);
    b.type = 'button'; b.dataset.qv = v.act;
    if (v.pressed !== undefined) b.setAttribute('aria-pressed', v.pressed ? 'true' : 'false');
    b.onclick = function () {
      if (v.arm && ANSWER.arm(b, v.arm)) return;
      var ta = li.querySelector('.qv-reason textarea'), params = {};
      if (v.reason && ta && ta.value.trim()) params.reason = ta.value.trim();
      Array.prototype.forEach.call(li.querySelectorAll('[data-qv]'), function (x) { x.disabled = true; });
      say('Sending…');
      ANSWER.post(v.path, params).then(function () {
        say(v.done);
        if (v.gone) gone(r.id); else W.reload('queue', tab);
      }, function (err) {
        Array.prototype.forEach.call(li.querySelectorAll('[data-qv]'), function (x) { x.disabled = false; });
        if (v.arm) b.textContent = v.word;
        say(err.message || 'That did not go through.', true);
      });
    };
    return b;
  }
  function row(r, d, now) {
    var need = needs(r, d.approve), li = el('li', 'qv-row' + (need ? ' needs' : ''));
    li.dataset.id = r.id;
    var a = el('a', 'bl qv-t', r.short_title || r.title || r.id);
    a.href = href(r.id); a.title = (r.title || r.id) + ' (' + r.id + ')';
    if (r.title && typeof TITLES !== 'undefined') TITLES.learn(r.id, r.title);
    li.appendChild(a);
    var m = el('p', 'qv-line');
    line(r, now).forEach(function (w, i) {
      if (i) m.appendChild(document.createTextNode(' · '));
      m.appendChild(i === 0 && need ? el('b', '', w) : document.createTextNode(w));
    });
    li.appendChild(m);
    var wh = who(r, d.approve);
    if (wh) li.appendChild(el('p', 'qv-who', wh));
    if (r.text) li.appendChild(el('p', 'bv-text qv-text', r.text));
    var o = [ref(r.agent, 'from'), ref(r.task, 'for'), ref(r.plan, 'in')].filter(Boolean);
    if (o.length) { var op = el('p', 'qv-origin'); o.forEach(function (x, i) { if (i) op.appendChild(document.createTextNode(' ')); op.appendChild(x); }); li.appendChild(op); }
    var box = asks(r, d.approve);
    if (box && typeof BEAD !== 'undefined' && BEAD.answerBox) li.appendChild(BEAD.answerBox(box));
    var vs = verbs(r, d.edit);
    if (vs.length) {
      var acts = el('div', 'bv-acts qv-acts');
      if (vs.some(function (v) { return v.reason; })) {
        var rs = el('div', 'qv-reason'), ta = el('textarea');
        ta.rows = 2; ta.maxLength = 10000; ta.placeholder = 'Reason (optional)'; ta.setAttribute('aria-label', 'Reason for ' + (r.title || r.id));
        rs.hidden = true; rs.appendChild(ta);
        var more = el('button', 'btn small ghost', 'Dismiss or reflag…'); more.type = 'button'; more.setAttribute('aria-expanded', 'false');
        more.onclick = function () { rs.hidden = !rs.hidden; more.setAttribute('aria-expanded', rs.hidden ? 'false' : 'true'); vs.forEach(function (v, i) { acts.children[i + 1].hidden = rs.hidden; }); if (!rs.hidden) ta.focus(); };
        acts.appendChild(more);
        vs.forEach(function (v) { var b = verbButton(v, r, li, d.kind); b.hidden = true; acts.appendChild(b); });
        li.appendChild(acts); li.appendChild(rs);
      } else {
        vs.forEach(function (v) { acts.appendChild(verbButton(v, r, li, d.kind)); });
        li.appendChild(acts);
      }
    }
    return li;
  }
  /* A row answered or dismissed here leaves the list; focus goes to the next row's first control, else the list's end. */
  function gone(id) {
    if (!listEl) return;
    var li = listEl.querySelector('[data-id="' + (window.CSS && CSS.escape ? CSS.escape(id) : id) + '"]');
    if (!li) return;
    var had = document.activeElement && li.contains(document.activeElement), next = li.nextElementSibling || li.previousElementSibling;
    li.remove();
    if (!listEl.children.length) { listEl.parentNode.insertBefore(el('p', 'bv-empty', empty(cur(), allOn())), listEl); }
    if (had) { var f = next && next.querySelector('[data-act],a.qv-t'); if (f) f.focus(); else if (titleEl) titleEl.focus(); }
  }

  function paint(tab, d, note, isErr, retry) {
    kindEl.textContent = 'queues';
    titleEl.textContent = tabWord(tab);
    bodyEl.textContent = '';
    listEl = null;
    bodyEl.appendChild(tabs(tab));
    if (d && (d.scoped || d.all)) bodyEl.appendChild(scopeRow(tab, d));
    noteEl = el('p', 'bv-note' + (isErr ? ' err' : ''), note || '');
    noteEl.setAttribute('role', 'status');
    if (isErr && retry) {   // a failure a second try may pass: Try again (the tab already shown does nothing when clicked)
      var b = el('button', 'btn small bv-retry', 'Try again'); b.type = 'button';
      b.onclick = function () { b.disabled = true; b.textContent = 'Loading…'; W.refresh('queue', cur()); };
      noteEl.appendChild(document.createTextNode(' ')); noteEl.appendChild(b);
    }
    bodyEl.appendChild(noteEl);
    if (!d) return;
    if (d.note) bodyEl.appendChild(el('p', 'bv-note', d.note));
    var now = Math.floor(Date.now() / 1000), rows = d.rows || [];
    if (!rows.length) bodyEl.appendChild(el('p', 'bv-empty', empty(tab, d.all)));
    listEl = el('ul', 'qv-list');
    listEl.setAttribute('aria-label', tabWord(tab));
    rows.forEach(function (r) { listEl.appendChild(row(r, d, now)); });
    bodyEl.appendChild(listEl);
    if (d.more) bodyEl.appendChild(el('p', 'bv-note', '+' + d.more + ' more not listed.'));
    if (d.in_hand) bodyEl.appendChild(el('p', 'bv-note', d.in_hand + ' (Everyone’s lists them).'));
    if (typeof TITLES !== 'undefined') TITLES.dress(bodyEl);
  }

  W.register('queue', {
    noun: 'queue',
    url: function (tab) { return url(isTab(tab) ? tab : 'decisions', allOn()); },
    paint: function (tab, d, note, isErr, retry) { paint(isTab(tab) ? tab : 'decisions', d, note, isErr, retry); },
    closed: function () { listEl = null; noteEl = null; if (allOn()) setAll(false); },   // before the host writes the next address
    hold: function () { return BEAD.holding(bodyEl) || !!bodyEl.querySelector('.free:not([hidden]), .qv-reason:not([hidden])'); },   // the viewer is writing in a row
    stale: function () {
      if (!noteEl) return;
      say('This queue changed while you were writing here. ');
      var b = el('button', 'btn small', 'Show the latest'); b.type = 'button';
      b.onclick = function () { W.refresh('queue', cur()); };
      noteEl.appendChild(b);
    },
    failed: function (id, text) { say(W.refreshFailure(text, 'queue'), true); }
  });

  /* Live (gd-6I6gmi6851): no stream of its own; a change to the page's lists (garden.js) that concerns the tab refetches it. */
  document.addEventListener('garden:lists', function (e) {
    var tab = cur(), ch = e.detail || {};
    if (!tab || document.hidden) return;   // the host refetches on return
    var up = (ch.up || []).map(function (f) { var li = document.getElementById('i-' + f.id); return { id: f.id, sec: f.sec, ty: li ? li.dataset.ty : '' }; });
    if (touches(isTab(tab) ? tab : 'decisions', { reset: ch.reset, rm: ch.rm, up: up }, listEl ? Array.prototype.map.call(listEl.children, function (li) { return li.dataset.id; }) : [])) W.moved('queue', tab);
  });

  /* A row as this panel draws it, with its answer box and none of its other writes (the project panel's Decisions tab,
     web/project.js). */
  api.row = function (r, d, now) { return row(r, { approve: !!(d && d.approve), edit: false, kind: r.kind === 'escalation' ? 'escalations' : 'decisions' }, now); };

  /* An answer sent from a row here (web/answer.js) takes the row away. */
  document.addEventListener('garden:answered', function (e) { if (cur() && e.detail && e.detail.id) gone(e.detail.id); });

  /* The Mailbox's way in: its "All queues" link names the tab that has something for the viewer, as the Mailbox stands. */
  var mailLink = document.getElementById('mail-queues'), mail = document.getElementById('d-mail');
  function aim() {
    if (!mailLink || !mail) return;
    var types = Array.prototype.map.call(mail.children, function (li) { return li.dataset.ty || ''; });
    mailLink.href = W.href('queue', pick(types));
  }
  if (mailLink && mail && typeof MutationObserver !== 'undefined') new MutationObserver(aim).observe(mail, { childList: true });
  aim();
  return api;
})();
if (typeof module !== 'undefined') module.exports = QUEUE;
