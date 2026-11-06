/* web/answer.js: act in place. Answer a decision, or approve / reject an escalation, from the Mailbox's row or the bead
   view's copy of it. These are the viewer's own endpoints (the same ones the bead page posts to), so permission and audit
   are unchanged. garden.js installs it with the page's base path, its toast, the Mailbox's list and its counts(), and asks
   it (stale, landed, clear) which asks were answered here, so a stale stream patch cannot bring one back (gd-LEfzdQl057). */
var ANSWER = (function () {
  var env = null, armed = null, armTimer = 0, gone = {}, GONE_MS = 60000;
  var WHY = { 400: 'The form was rejected.', 401: 'Your session ended. Reload the page to sign in again.', 403: 'You are not allowed to do that.', 404: 'That bead is gone.', 409: 'Someone already answered this.', 413: 'That answer is too long.' };
  function disarm() { if (armed) { armed.textContent = armed.dataset.was; armed.classList.remove('armed'); armed = null; } clearTimeout(armTimer); }
  /* arm(b, words) -> true when this tap only armed b: it says words (its data-arm, else "Confirm: …", a decision option's whole
     label when its button shows a short word, as a phone shows no title tooltip) until a second tap,
     another button's arm, or a few seconds pass. False on that second tap, which disarms it: go ahead. One armed button on
     the page, so the Mailbox and the Queues panel (web/queue.js) confirm alike. */
  function arm(b, words) {
    if (armed === b) { disarm(); return false; }
    disarm(); armed = b; b.dataset.was = b.textContent;
    b.textContent = words || b.dataset.arm || 'Confirm: ' + (b.dataset.opt && b.title ? cap(b.title, 60) : b.dataset.was); b.classList.add('armed'); armTimer = setTimeout(disarm, 4000);
    return true;
  }
  /* The viewer's write routes answer JSON when asked (respondAction, gd-jgVYg6kH5K): {"ok":true,"bead":id}, or
     {"ok":false,"error":"…","status":409} with the message the bead page's banner would show. */
  function post(path, params) {
    return fetch(env.base + path, { method: 'POST', credentials: 'same-origin', redirect: 'manual', headers: { 'Content-Type': 'application/x-www-form-urlencoded', 'Accept': 'application/json' }, body: new URLSearchParams(params).toString() }).then(function (r) {
      if (r.type === 'opaqueredirect') throw new Error(WHY[401]);
      return r.json().catch(function () { return null; }).then(function (j) {
        var e = failure(r.status, j); if (e) throw new Error(e);
        return j;
      });
    });
  }
  /* What went wrong, in words, or '' when the action went through. */
  function failure(status, j) {
    if (j && j.ok === true) return '';
    if (status >= 200 && status < 300 && !j) return '';
    var m = j && typeof j.error === 'string' ? j.error.replace(/\s+/g, ' ').trim() : '', code = (j && j.status) || status;
    return m || WHY[code] || 'That did not go through (' + code + ').';
  }
  /* An answered ask leaves the screen and the next one takes its place. The Ledger row goes, so the Mailbox closes up and
     the map's signpost falls; a bead view open on it (its copy of the row answered there) moves on to the next waiting
     decision or escalation the Mailbox shows, or closes when none is left (one the Mailbox does not list, answered from the
     bead's face, reloads in place); in the Mailbox itself, focus (lost with the
     row's button) moves to the next row's first button. The stream's removal arrives a moment later and finds nothing to
     do; a stale patch cannot bring the ask back meanwhile (gone[]). The Queues panel (web/queue.js) hears garden:answered
     and drops its row. */
  function answered(id, msg) {
    env.toast(msg);
    var mail = env.mail, row = mail.querySelector('[data-src="i-' + id + '"]'), after = row && row.nextElementSibling, inMail = !!(row && document.activeElement && row.contains(document.activeElement));
    var src = document.getElementById('i-' + id); if (src) src.remove();
    gone[id] = Date.now();
    env.counts();
    try { document.dispatchEvent(new CustomEvent('garden:answered', { detail: { id: id } })); } catch (e) { /* no CustomEvent: the Queues panel keeps the row until it reloads */ }
    var next = nextAsk(mail, after), nid = next ? next.dataset.src.slice(2) : '';
    var bv = typeof BEAD !== 'undefined' ? BEAD : null;
    if (bv && bv.current && bv.current() === id) {
      // a face's own answer box on an ask the Mailbox does not list (someone else's): the panel stays and shows it answered
      if (!row && typeof WORKBENCH !== 'undefined') { WORKBENCH.reload('bead', id); return; }
      if (nid) bv.open(nid, { replace: true }); else bv.close();
      return;
    }
    if (inMail && next) { var b = next.querySelector('[data-act]'); if (b) b.focus(); }
  }
  /* nextAsk(mail, after) -> the Mailbox row the bead view moves on to: after or the first past it, else the first from the
     top, that is not in hand. An in-hand escalation is the desk's or the operators', so answering one ask never opens one
     (gd-Mq0uDXHswK). null when none is left. */
  function nextAsk(mail, after) {
    var start = after && after.parentNode === mail ? after : null, n;
    for (n = start; n; n = n.nextElementSibling) if (!n.classList.contains('inhand')) return n;
    for (n = mail.firstElementChild; n && n !== start; n = n.nextElementSibling) if (!n.classList.contains('inhand')) return n;
    return null;
  }
  /* heldNote(held): an agent-held escalation's line, render.go Item.HeldNote's words. */
  function heldNote(held) { return held.charAt(0).toUpperCase() + held.slice(1) + ' has this; nothing waits on you.'; }
  /* A box that is not the ask's own row (a Nursery pot's card, an escalation cell's, data-keep) stays where it is and says it was answered, with
     what (button b's words) and what happens next; focus, which left with its button, goes to that line. */
  function settle(box, b) {
    if (!box.dataset.keep) return;
    var had = box.contains(document.activeElement), p = document.createElement('p');
    p.className = 'note'; p.setAttribute('role', 'status'); p.tabIndex = -1; p.textContent = 'Answered: ' + (b.dataset.opt || b.dataset.act === 'approve' || b.dataset.act === 'reject' ? b.textContent.trim() : 'your written answer') + '. ' + box.dataset.keep;
    box.textContent = ''; box.appendChild(p);
    if (had) p.focus();
  }
  function act(b) {
    var box = b.closest('.act'), id = b.dataset.id, note = box.querySelector('.note'), kind = b.dataset.act, path, params = {}, done;
    if (kind === 'resolve') {
      path = '/b/' + id + '/decision/resolve';
      if (b.dataset.opt) { params.option_id = b.dataset.opt; done = 'Answered ' + id + ' with ' + b.dataset.opt + '.'; } else {
        var ta = box.querySelector('textarea'); params.response_text = ta.value.trim();
        if (!params.response_text) { note.textContent = 'Write an answer first.'; note.className = 'note err'; ta.focus(); return; }
        done = 'Answered ' + id + '.';
      }
    } else {
      path = '/b/' + id + '/escalation/' + kind; done = (kind === 'approve' ? 'Approved ' : 'Rejected ') + id + '.';
      var ct = box.querySelector('textarea'), comment = ct ? ct.value.trim() : '';   // optional: a comment on the approval, the reason of a rejection (gd-yuOG6cX6VI)
      if (comment) params[kind === 'approve' ? 'comment' : 'reason'] = comment;
    }
    var btns = box.querySelectorAll('button,textarea');
    Array.prototype.forEach.call(btns, function (x) { x.disabled = true; });
    note.className = 'note'; note.textContent = 'Sending…';
    post(path, params).then(function () { settle(box, b); answered(id, done); }, function (err) {
      Array.prototype.forEach.call(btns, function (x) { x.disabled = false; });
      note.className = 'note err'; note.textContent = err.message || 'That did not go through.';
    });
  }
  function onClick(e) {
    var b = e.target.closest ? e.target.closest('.dlist [data-act],.bv [data-act],.stip [data-act]') : null; if (!b) return;   // the Mailbox, a bead view holding a copy of its row, or a Nursery pot's card (web/stip.js)
    if (document.body.dataset.ro === '1') return;   // a read-only viewer's boxes have no buttons (garden.css body[data-ro]); nothing answers
    var kind = b.dataset.act;
    if (kind === 'other' || kind === 'comment') { var f = b.closest('.act').querySelector('.free'); f.hidden = !f.hidden; b.setAttribute('aria-expanded', f.hidden ? 'false' : 'true'); if (!f.hidden) f.querySelector('textarea').focus(); return; }
    /* An answer cannot be taken back, so a button answers on its second tap; it disarms itself after a few seconds. */
    if (!(kind === 'resolve' && !b.dataset.opt) && arm(b)) return;
    act(b);
  }
  function el(t, cls, text) { var e = document.createElement(t); if (cls) e.className = cls; if (text !== undefined) e.textContent = text; return e; }
  function cap(s, n) { s = Array.from(s); return s.length > n ? s.slice(0, n - 1).join('') + '…' : s.join(''); }
  /* box(a): the answer box this file drives, as the Mailbox row's own (render.go itemTmpl) is built: a decision's options, Other and its
     free answer; an escalation's Approve and Reject (shown only to an account that may approve, body[data-esc]) and Comment…, an
     optional note sent with either: a comment on the approval, the reason of a rejection (gd-yuOG6cX6VI). One an agent lane
     holds (a.held, AgentLaneHolder) says so first and offers a secondary "Approve anyway" that names the holder,
     no primary Approve (gd-Mq0uDXHswK). */
  function box(a) {
    var bx = el('div', 'act'), acts = el('div', 'acts'), b = function (text, act, cls) { var x = el('button', 'btn small' + (cls ? ' ' + cls : ''), text); x.type = 'button'; x.dataset.act = act; x.dataset.id = a.id; acts.appendChild(x); return x; };
    bx.dataset.kind = a.kind;
    bx.appendChild(acts);
    if (a.kind === 'decision') {
      a.options.forEach(function (o) { var x = b(o.text, 'resolve'); x.dataset.opt = o.id; if (o.label && o.label !== o.text) x.title = o.label; });
      b('Other…', 'other', 'ghost');
      var free = el('div', 'free'), ta = el('textarea');
      free.hidden = true; ta.rows = 3; ta.maxLength = 10000; ta.setAttribute('aria-label', 'Your answer'); ta.placeholder = 'Write your answer';
      free.appendChild(ta);
      var send = el('button', 'btn small', 'Send answer'); send.type = 'button'; send.dataset.act = 'resolve'; send.dataset.id = a.id; free.appendChild(send);
      bx.appendChild(free);
    } else {
      var what = a.title ? '“' + cap(a.title, 40) + '”' : 'this escalation';   // the confirm names its target (rubric C8)
      if (a.held) {
        bx.insertBefore(el('p', 'who held', heldNote(a.held)), acts);
        b('Approve anyway', 'approve', 'ghost').dataset.arm = 'Tap again to approve ahead of ' + a.held;
      } else b('Approve', 'approve').dataset.arm = 'Tap again to approve ' + what;
      b('Reject', 'reject', 'ghost danger').dataset.arm = 'Tap again to reject ' + what;
      b('Comment\u2026', 'comment', 'ghost').setAttribute('aria-expanded', 'false');
      var cf = el('div', 'free'), cta = el('textarea');
      cf.hidden = true; cta.rows = 2; cta.maxLength = 4000; cta.setAttribute('aria-label', 'Comment (optional)'); cta.placeholder = 'Comment (optional): sent with your approval or rejection';
      cf.appendChild(cta); bx.appendChild(cf);
    }
    var n = el('p', 'note'); n.setAttribute('role', 'status'); bx.appendChild(n);
    return bx;
  }
  /* evidence(ev, src, href): the bead panel's evidence strip, decisionview.EvidenceStrip's markup (gd-Pi7C83262N), or null.
     src(url) is the picture through the image proxy, '' for one it does not serve, which is left out. */
  var evSeq = 0;
  function evidence(ev, src, href) {
    var s = el('section', 'dec-ev'), h = el('h4', 'dec-ev-head', 'Screenshots '), ul = el('ul', 'dec-ev-list'), n = 0;
    (ev || []).forEach(function (x) {
      var u = src(x.url), id = 'dec-ev-bp-' + (++evSeq), li = el('li', 'dec-ev-fig'), fig = el('figure'), btn = el('button', 'dec-ev-thumb'), cap = el('figcaption');
      var box = el('div', 'dec-ev-box'), p = el('p', 'dec-ev-box-cap'), full = el('a', '', 'Open full size'), close = el('button', 'dec-ev-close', 'Close');
      if (!u) return;
      var img = function () { var i = el('img'); i.src = u; i.alt = x.alt || 'screenshot'; i.loading = 'lazy'; i.decoding = 'async'; i.referrerPolicy = 'no-referrer'; return i; };
      n++;
      btn.type = close.type = 'button'; btn.title = 'Enlarge: ' + (x.alt || 'screenshot'); btn.setAttribute('popovertarget', id); btn.appendChild(img());
      cap.appendChild(el('span', 'dec-ev-alt', x.alt || ''));
      if (x.from) { var f = cap.appendChild(el('span', 'dec-ev-from', 'from ')), a = f.appendChild(el('a', 'bl', x.from_title || x.from)); a.href = href(x.from); a.title = x.from; }
      box.id = id; box.setAttribute('popover', ''); full.href = u; full.target = '_blank'; full.rel = 'noopener noreferrer';
      close.setAttribute('popovertarget', id); close.setAttribute('popovertargetaction', 'hide');
      p.appendChild(full);
      [img(), p, close].forEach(function (e) { box.appendChild(e); });
      fig.appendChild(btn); fig.appendChild(cap); li.appendChild(fig); li.appendChild(box); ul.appendChild(li);
    });
    if (!n) return null;
    s.setAttribute('aria-label', 'Screenshots from the beads this decision is about');
    h.appendChild(el('span', 'dec-ev-n', String(n))); s.appendChild(h); s.appendChild(ul);
    return s;
  }
  return {
    install: function (e) { env = e; document.addEventListener('click', onClick); },
    evidence: evidence,
    arm: arm,
    box: box,   // the bead view's, the Queues panel's and a pot card's answer box (web/workbench.js answerBox, web/stip.js)
    post: post,   // the chore tour (web/tour.js) posts its chores through the same call
    /* For the stream: an ask answered here in the last minute is gone whatever an older snapshot says (the viewer answers
       200 only once the daemon closed it); its removal, or a reset, is the server's word. */
    stale: function (id) { if (!gone[id]) return false; if (Date.now() - gone[id] < GONE_MS) return true; delete gone[id]; return false; },
    landed: function (id) { delete gone[id]; },
    clear: function () { gone = {}; },
    nextAsk: nextAsk,   // pure over the Mailbox list: for the node test
    failure: failure   // pure: for the node test
  };
})();
if (typeof module !== 'undefined') module.exports = ANSWER;
