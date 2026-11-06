/* STIP: the steward's tip (Lands 9.8, homestead-agents-as-people.md section 4.4). Its card (WORLD.stewardCard) beside its
   sprite, so its reason ("Parked: the purse is empty", "Waiting on you: approve plan X") shows on hover and focus, and on a
   finger's tap: the first tap on the steward opens the tip, the second follows its link (its thread in the Commons while a run
   is live, else its bead). The tip's own links work as the card's. Placed in board px from the sprite, above it where there is
   room, else below, 16 px in from every edge (SIGNFIT.fit), and moved with the camera. garden.js hands over its state at init (env()),
   wires each steward's link (wire), re-places the tip on each camera change (place) and re-points it at the steward's new hot
   on each farm document (land). Browser only.
   A Nursery pot wears the same tip (9.11b, gd-m38yKRX0hE): the plan's title, its state in words (drafting, waiting on your
   answer, ready) and, while its plan decision is open (seeds[].decision), that decision's card: the Mailbox's own answer box
   (ANSWER.box), which web/answer.js answers through the same POST. It stays while the pointer crosses from the pot to it,
   Tab from the pot goes into it, Escape goes back to the pot, and it follows each farm document (closing when the plan leaves
   the Nursery). potCard is the same card for the bead view's Nursery section (web/workbench.js). A staked plot (a ready or
   asked seed laid out in the fields, homestead-world-design.md section 3) wears the pot's tip as its pot did, and drops it
   when the plan is approved and planted in place. Its tip lists the plan's steps under the card (n and title, each opening
   the step's bead view), and a finger's first tap on it opens the tip (tap, from garden.js cellClick), its second goes in.
   potSteps is the bead view's Steps section: every step with its body, then the plan's narrative.
   A goat wears it too (gd-2cPnm71cxT): its wish, who asked, tended or not, its thread and "Zoom to it on the map", a cursor jump
   to its plot or its pen cell (GOATS.card, GOATS.goTo; env().go, gd-xlRpUpTjPZ). A goat is drawn, not a
   link, so garden.js hit-tests it (GOATS.at) and calls goat(): on hover, and pinned on a finger's first tap, whose second opens
   the thread (goat() below). A press on the map does not close a goat's tip; the click that follows decides.
   A headstone wears it as a steward does (3.2, gd-s9KzRk6boh): its link's hover, focus or first tap shows GOATS.stoneCard, how its
   wish ended, why, and the merge requests its kin merged; a click, or a second tap, opens the wish's head bead. A release truck
   too (TRUCK.card, gd-yOOVahJMCh): the release, its counts and where it runs; a click, or a second tap, opens the release.
   An escalation's cell in a home strip too (28.11, gd-4FIF5pzC6O): ESCKIND.tip's words and, for a pending one the viewer may
   approve (ESCKIND.answerable, body[data-esc]), the Mailbox's Approve, Reject and Comment box, which keeps it as a pot's card
   does; it follows each farm document and closes when the escalation is no longer open. A click, or Enter, opens its bead.
   An escalation's executor beside its icon (WORLD.escStands, step 7 gd-B5aDmNhmjz) too: its name and WORLD.gardenerCard on
   hover, focus or a finger's first tap; a click opens its thread as a figure on a plot does (garden.js makeHot). */
var STIP = (function () {
  "use strict";
  var tip = null, at = null, atNode = null, pinned = false, env = null, seeds = {}, escs = {}, shownKey = '', hush = false, leaveTimer = 0, LEAVE_MS = 300;
  var KEEP = 'The plan leaves the Nursery when it starts building.', ESC_KEEP = 'It leaves the Escalations cell once it closes.';
  function el(tagName, cls, text) { var e = document.createElement(tagName); if (cls) e.className = cls; if (text !== undefined) e.textContent = text; return e; }
  function me() { return ((document.body && document.body.dataset.me) || '').toLowerCase(); }
  function handle(who) { return String(who || '').replace(/^oidc:/, '').split('@')[0] || 'the owner'; }
  /* Does hot h wear a pot's card: a Nursery pot, or a staked plot (WORLD.layout)? */
  function isPot(h) { return !!h && (h.kind === 'seed' || (h.kind === 'plot' && !!h.plot && !!h.plot.staked)); }
  function seedOf(h) { return h.kind === 'seed' ? h.it.seed : h.plot.seed || {}; }
  /* Is h a hot this tip shows: a steward, a headstone, a goat, a pot (a staked plot's node outlives its staking), an
     escalation's cell or the executor beside it. */
  function tipped(h) { return !!h && (isPot(h) || h.kind === 'steward' || h.kind === 'gesc' || h.kind === 'stone' || h.kind === 'goat' || h.kind === 'escalation' || h.kind === 'truck'); }
  /* An escalation cell's FarmWait as the newest farm document has it; may the viewer approve it (body[data-esc], not read-only)? */
  function escWait(h) { var w = h && h.node && h.node.ref && h.node.ref.wait; return (w && escs[w.id]) || w || {}; }
  function mayEsc() { var d = document.body && document.body.dataset; return !!(d && d.esc && d.ro !== '1'); }
  function escKey(w) { return [w.id, w.title, w.state, w.human ? 1 : 0, w.waits_on || '', mayEsc() ? 1 : 0, typeof ANSWER !== 'undefined' && ANSWER.stale(w.id) ? 1 : 0].join('|'); }
  function answering(h) { return !!h && h.kind === 'escalation' && typeof ESCKIND !== 'undefined' && ESCKIND.answerable(escWait(h), mayEsc()); }
  /* Does h's tip hold a card to work in (a pot's, an escalation's answer box): it stays while the pointer crosses onto it, and
     Tab goes into it. */
  function carded(h) { return isPot(h) || answering(h); }
  /* A pot's state in words and its tone: drafting, waiting on an answer (yours, or whose), ready with no card yet. */
  function potState(sd, viewer) {
    var d = sd.decision;
    if (d) return [!d.for || d.for === viewer ? 'Waiting on your answer' : 'Waiting on ' + handle(d.for) + '’s answer', 'ask'];
    if (sd.state === 'frosted') return ['Drafting: the plan is still being written', 'dim'];
    return ['Ready, with no card to answer yet', ''];
  }
  /* A pot's one-line summary, its link's accessible name (garden.js cardData): a Nursery pot counts its phases; a staked plot
     its steps, and says approving it plants it there. */
  function potTip(sd, staked) {
    var n = staked ? (sd.steps || []).length : sd.phases || 0, st = potState(sd, me())[0];
    return [sd.title, (staked ? 'Staked out, ' : 'Seed, ') + st.charAt(0).toLowerCase() + st.slice(1) + (sd.decision ? ': ' + sd.decision.prompt : '') + ' · ' + n + (staked ? ' step' : ' phase') + (n === 1 ? '' : 's') +
      (staked ? '. Approving it plants it here.' + (sd.project ? ' ' + sd.project : '') : '')];
  }
  /* The pot's card: its state, then the decision's question and answer box (an answer sent from this page in the last minute
     reads as answered, ANSWER.stale). head: the plan's title first (the tip; the bead view has its own). */
  function potKey(sd) { var d = sd.decision; return [sd.id, sd.title, sd.state, d ? d.id : '', d && typeof ANSWER !== 'undefined' && ANSWER.stale(d.id) ? 1 : 0, JSON.stringify(sd.steps || [])].join('|'); }
  function potCard(sd, head) {
    var c = el('div', 'pot-card'), st = potState(sd, me()), d = sd.decision;
    if (head) c.appendChild(el('b', '', sd.title || 'Untitled plan'));
    c.appendChild(el('div', 'pst' + (st[1] ? ' pst-' + st[1] : ''), st[0]));
    if (!d) return c;
    if (typeof ANSWER !== 'undefined' && ANSWER.stale(d.id)) { c.appendChild(el('p', 'note', 'Answered. ' + KEEP)); return c; }
    var box = typeof ANSWER !== 'undefined' && ANSWER.box ? ANSWER.box({ kind: 'decision', id: d.id, options: d.options || [] }) : null;
    if (!box) return c;
    box.insertBefore(el('p', 'ask', d.prompt || 'Run it?'), box.firstChild);
    var drop = box.querySelector('[data-opt=drop]');   // the steward's "drop the plan" undoes the most: marked, and its confirm names the plan (rubric C8)
    if (drop) { drop.className += ' ghost danger'; drop.dataset.arm = 'Tap again to drop “' + String(sd.short_title || sd.title || sd.id).slice(0, 40) + '”'; }
    box.dataset.keep = KEEP;
    c.appendChild(box);
    if (typeof TITLES !== 'undefined') TITLES.dress(c);   // a bead id the question names reads as its title (gd-9yOATV2qCs)
    return c;
  }
  /* A step's title without the "3. " a plan's steps are often titled with, when it is the number the list already shows. */
  function stepTitle(st) { return String(st.title || st.id).replace(/^(\d+)[.)]\s+/, function (m, d) { return +d === st.n ? '' : m; }) || st.id; }
  /* A staked plot's steps in the tip, numbered as its stakes; each opens the step's bead view. */
  function stepList(steps, base) {
    var ol = el('ol', 'pot-steps');
    steps.forEach(function (st) {
      var li = el('li'), a = el('a', '', stepTitle(st));
      a.href = base + '/garden?bead=' + encodeURIComponent(st.id);
      li.appendChild(el('span', 'n', String(st.n || ''))); li.appendChild(a); ol.appendChild(li);
    });
    return ol;
  }
  /* The first BODY_RUNES runes of s, with an ellipsis when cut, or '' when s is that short already. */
  var BODY_RUNES = 280, opened = {};   // opened: the step bodies opened whole, by step id, kept across the view's repaints
  function clipRunes(s, n) { var r = Array.from(s); return r.length > n ? r.slice(0, n).join('').replace(/\s+$/, '') + '…' : ''; }
  /* The bead view's Steps section of a ready or asked plan (web/workbench.js): every step in step order, numbered, its title
     opening its bead view and its body (bodies: {step id: description}, the bead route's child rows) cut to its first
     BODY_RUNES runes until a click opens it whole; then the plan's narrative (seeds[].summary) unless noSum (the view shows the
     description itself). href(id) is the bead's link. have: the ids the bead route listed as children (null before it
     answered); a step it does not list is one the daemon no longer has, and says so. */
  function potSteps(sd, bodies, href, have, noSum) {
    var box = el('div', 'pot-steps-sec'), ol = el('ol', 'bv-rels bv-phases pot-steps');
    (sd.steps || []).forEach(function (st) {
      var li = el('li'), pb = el('span', 'bv-pb'), a = el('a', 'bl', stepTitle(st)), body = String(bodies[st.id] || '').trim();
      a.href = href(st.id);
      li.appendChild(el('span', 'bv-n', String(st.n || ''))); pb.appendChild(a); li.appendChild(pb);
      if (have && !have[st.id]) { li.className = 'gone'; pb.appendChild(el('p', 'bv-text pot-gone', 'Not found: its bead is no longer in the daemon.')); }
      if (body) {
        var short = clipRunes(body, BODY_RUNES), p = el('p', 'bv-text pot-body', short && !opened[st.id] ? short : body);
        if (short) {   // a click (or Enter) opens the whole body and closes it again
          p.tabIndex = 0; p.setAttribute('role', 'button'); p.setAttribute('aria-expanded', String(!!opened[st.id])); p.title = 'Read the whole step';
          var flip = function () { var open = opened[st.id] = !opened[st.id]; p.textContent = open ? body : short; p.setAttribute('aria-expanded', String(open)); };
          p.addEventListener('click', function (e) { if (!(e.target.closest && e.target.closest('a'))) flip(); });
          p.addEventListener('keydown', function (e) { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); flip(); } });
        }
        pb.appendChild(p);
      }
      ol.appendChild(li);
    });
    if (ol.children.length) box.appendChild(ol); else box.appendChild(el('p', 'bv-empty', 'No steps yet.'));
    if (sd.summary && !noSum) { box.appendChild(el('h4', '', 'The plan')); box.appendChild(el('p', 'bv-text pot-sum', sd.summary)); }
    return box;
  }
  /* A finger's tap on a staked plot's link (garden.js cellClick, which a plot's click reaches before wire's): the first opens
     its tip, pinned, and says so; the next closes it and is the plot's own (the cursor goes in). A mouse's click is the plot's. */
  function tap(node, e) {
    var h = node._h;
    if (!isPot(h) || h.kind !== 'plot' || !finger(e, node._ptr)) return false;
    node._ptr = '';
    if (pinned && at && at.kind === h.kind && at.id === h.id) { show(null); return false; }   // by id: a farm document between the taps re-makes the hot
    show(h, true, node); return true;
  }
  function finger(e, ptr) { var p = e.pointerType || ptr; return !!p && p !== 'mouse' || ptr === 'touch' || ptr === 'pen'; }
  /* An answer is on its way or half-typed in the tip: a farm document does not repaint it. */
  function busy() { return !!(tip && (tip.querySelector('.act .armed, .act [disabled]') || [].some.call(tip.querySelectorAll('.act textarea'), function (t) { return t.value; }))); }
  /* An escalation's card: its title, ESCKIND.tip's line, the answer box when it is answerable (an answer sent from this page in
     the last minute reads as answered), and the way to its bead. */
  function escCard(h) {
    var w = escWait(h), s = env(), now = Date.now(), t = typeof ESCKIND !== 'undefined' ? ESCKIND.tip(w, now, s.lin) : [w.title || w.id, ''], ans = answering(h);
    shownKey = escKey(w);
    tip.setAttribute('role', ans ? 'group' : 'tooltip');
    if (ans) tip.setAttribute('aria-label', t[0]); else tip.removeAttribute('aria-label');
    tip.appendChild(el('b', '', t[0]));
    tip.appendChild(el('div', 'esc-st', t[1]));
    if (ans && typeof ANSWER !== 'undefined') {
      if (ANSWER.stale(w.id)) tip.appendChild(el('p', 'note', 'Answered. ' + ESC_KEEP));
      else { var box = ANSWER.box({ kind: 'escalation', id: w.id, title: w.title, held: w.held }); box.dataset.keep = ESC_KEEP; tip.appendChild(box); }
    }
    var open = el('a', 'pot-open', 'Open the escalation'); open.href = s.base + '/garden?bead=' + encodeURIComponent(w.id);
    tip.appendChild(open);
  }
  /* Open the tip on hot h (pinned: it stays when the pointer leaves), or close it on null. node: h's link, for a pot. */
  function show(h, pin, node) {
    clearTimeout(leaveTimer);
    at = h; atNode = h ? node || atNode : null; pinned = !!(h && pin);
    if (!h || !tip) { if (tip) tip.hidden = true; return; }
    var s = env();
    tip.textContent = ''; tip.scrollTop = 0;
    if (isPot(h)) {
      var sd = seeds[h.id] || seedOf(h);
      shownKey = potKey(sd);
      tip.setAttribute('role', 'group'); tip.setAttribute('aria-label', sd.title || 'Nursery pot');
      tip.appendChild(potCard(sd, true));
      var open = el('a', 'pot-open', 'Open the plan'), n = (sd.steps || []).length; open.href = s.base + '/garden?bead=' + encodeURIComponent(h.id);
      tip.appendChild(open);   // before the steps, so a tip that scrolls never hides the way in
      if (h.kind === 'plot' && n) { tip.appendChild(el('div', 'pot-sh', n + (n === 1 ? ' step' : ' steps'))); tip.appendChild(stepList(sd.steps, s.base)); }   // a staked plot: what its stakes name
      tip.hidden = false; place();
      return;
    }
    if (h.kind === 'escalation') { escCard(h); tip.hidden = false; place(); return; }
    tip.setAttribute('role', 'tooltip'); tip.removeAttribute('aria-label');
    var c = h.kind === 'goat' ? GOATS.card(h.g, s.base, s.ago) : h.kind === 'stone' ? GOATS.stoneCard(h.it.wish, s.base, s.ago) : h.kind === 'truck' ? TRUCK.card(h.tr, s.base, s.ago) : h.kind === 'gesc' ? WORLD.gardenerCard(h.g, s.gs, s.world, s.base, s.ago) : WORLD.stewardCard(h.g, s.base, s.ago);
    var t = tip.appendChild(el('b', '', c.title));
    if (typeof ARMS !== 'undefined') (c.arms || []).slice().reverse().forEach(function (a) { var p = ARMS.lineArms({ arms: a }); if (p) t.insertBefore(p, t.firstChild); });
    c.lines.forEach(function (l) {
      var d = el('div', l[0]);
      if (typeof l[1] === 'string') d.textContent = l[1];
      else l[1].forEach(function (k, i) { if (i) d.appendChild(document.createTextNode(' · ')); var a = el('a', k.cls || '', k.text); a.href = k.href; if (k.id) a.dataset.id = k.id; d.appendChild(a); });
      tip.appendChild(d);
    });
    tip.hidden = false; place();
  }
  function place() {
    if (!tip || tip.hidden || !at) return;
    var s = env(), ar = SIGNFIT.area(s.view || s.vp);
    // a long card scrolls inside the map, 16 px above its foot: a phone's title bar and tab bar (gd-DzceM7DY86)
    var v = s.view || s.vp;
    if (v.bottom) ar.h = Math.min(ar.h, v.h - v.bottom - 16 - ar.y);
    tip.style.maxHeight = Math.max(120, Math.floor(ar.h)) + 'px';
    if (!isPot(at) && tip.scrollHeight > tip.clientHeight + 1) {   // its last links row moves up under the title
      var ways = tip.querySelectorAll('.why'), way = ways[ways.length - 1], t = tip.firstChild;
      if (way && t && t.nextSibling !== way) tip.insertBefore(way, t.nextSibling);
    }
    var a = WORLD.toScreen(s.cam, s.vp, at.x, at.y), b = WORLD.toScreen(s.cam, s.vp, at.x + at.w, at.y + at.h), sz = tip.getBoundingClientRect(), w = sz.width, hh = sz.height;   // fractional, so the tip's foot never rounds onto the title bar
    var th = { x: a.x, y: a.y, w: b.x - a.x, h: b.y - a.y }, bx = { x: th.x + th.w / 2 - w / 2, y: th.y - 8 - hh, w: w, h: hh }, k = SIGNFIT.keep();
    var fit = function (ks) { return SIGNFIT.fit(bx, th, 'above', ar, 8, k.concat(ks)); }, f = fit(SIGNFIT.rings(s.bcns).concat([th]));   // 16 px in, clear of the header, camera buttons and arrows (web/signfit.js), and of its Thing, which stays clickable
    if (SIGNFIT.hit({ x: f.x, y: f.y, w: w, h: hh }, th)) f = fit([th]);   // no spot clears them all (a phone held sideways): beside its Thing, over an arrow or its label (gd-1SFmkrSqId)
    tip.style.left = f.x + 'px'; tip.style.top = f.y + 'px';
  }
  /* A steward's or a pot's link: node._h is its current hot (garden.js fillHot). */
  function wire(node) {
    var live = function () { var h = node._h; return tipped(h) ? h : null; }, pot = function () { return carded(live()); };   // a plot planted since it was staked wears no tip
    node.addEventListener('pointerdown', function (e) { node._ptr = e.pointerType; });   // a tap's click does not always say it was a finger
    node.addEventListener('pointerenter', function (e) { if (e.pointerType === 'mouse' && live() && !(pinned && at === live())) show(live(), false, node); });
    node.addEventListener('pointerleave', function (e) {
      if (e.pointerType !== 'mouse' || pinned || !live()) return;
      if (!pot()) { show(null); return; }
      clearTimeout(leaveTimer); leaveTimer = setTimeout(function () { if (!pinned) show(null); }, LEAVE_MS);   // time to cross onto the card
    });
    node.addEventListener('focus', function () { if (hush) { hush = false; return; } if (live()) show(live(), pinned && at === live(), node); });   // hush: focus handed back by the card's Escape
    node.addEventListener('blur', function (e) { if (live() && !pinned && !(e.relatedTarget && tip && tip.contains(e.relatedTarget))) show(null); });
    node.addEventListener('keydown', function (e) {   // Tab from a pot, or an escalation that offers its answer, goes into its card
      if (e.key !== 'Tab' || e.shiftKey || !pot() || !tip || tip.hidden || at !== live()) return;
      var first = tip.querySelector('button:not([disabled]),a[href]');
      if (first) { e.preventDefault(); pinned = true; first.focus(); }
    });
    node.addEventListener('click', function (e) {
      var h = live(); if (!h || e.defaultPrevented || e.button || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
      var fg = finger(e, node._ptr); node._ptr = '';
      if (at !== h || (!pinned && fg)) { e.preventDefault(); show(h, true, node); return; }   // a tap opens the tip; the next one goes
      if (isPot(h) || h.kind === 'escalation') { show(null); return; }   // the bead view opens with the same card (web/workbench.js), an escalation's with its answer
      if (h.kind === 'steward' && h.g.chat && h.g.run && typeof CHAT !== 'undefined') { e.preventDefault(); CHAT.open(h.g.run, { none: function () { location.href = node.href; } }); }
    });
  }
  /* A goat's tip (GOATS.at's goat g, its box in world px) on hover, or pinned (a finger's tap); null closes an open goat tip
     (pinned too, with pin). A hover's close waits LEAVE_MS, time for the mouse to cross onto the card and its links (the tip's
     pointerenter keeps it, as a pot's; gd-xlRpUpTjPZ); a click's (pin) closes it now. goatOf() is the goat it shows, or ''. */
  function goat(g, pin) {
    if (!g) {
      if (!(at && at.kind === 'goat') || (pinned && !pin)) return;
      if (pin) { clearTimeout(leaveTimer); show(null); return; }
      clearTimeout(leaveTimer); leaveTimer = setTimeout(function () { if (!pinned && at && at.kind === 'goat') show(null); }, LEAVE_MS);
      return;
    }
    clearTimeout(leaveTimer);
    if (at && at.kind === 'goat' && at.id === g.id && (pinned || !pin)) return;   // already up
    show({ kind: 'goat', id: g.id, g: g, x: g.x, y: g.y, w: g.w, h: g.h }, pin);
  }
  function goatOf() { return at && at.kind === 'goat' ? at.id : ''; }
  /* A new farm document's steward and executor hots: the open tip follows its figure's hot, and closes when it is gone. */
  function land(hots) {
    if (!at || (at.kind !== 'steward' && at.kind !== 'gesc')) return;
    var id = at.id, kind = at.kind; show(hots.filter(function (h) { return h.kind === kind && h.id === id; })[0] || null, pinned);
  }
  /* A new farm document: an open pot's card shows the seed as it now is, and closes when the plan has left the Nursery. */
  function onDoc(doc) {
    seeds = {};
    ((doc && doc.seeds) || []).forEach(function (s) { if (s && s.id) seeds[s.id] = s; });
    if (at && at.kind === 'goat' && !(((doc && doc.wishes) || {}).open || []).some(function (w) { return w && w.id === at.id; })) { show(null); return; }   // the wish closed
    if (at && at.kind === 'stone' && !(((doc && doc.wishes) || {}).closed || []).some(function (w) { return w && 'wish:' + w.id === at.id; })) { show(null); return; }   // the stone left the graveyard
    escs = {};
    (((doc && doc.escalations) || {}).open || []).forEach(function (w) { if (w && w.id) escs[w.id] = w; });
    if (at && at.kind === 'escalation') {   // closed, or answered here: the card goes with it; else it shows the escalation as it now is
      var ew = escWait(at), on = !!escs[ew.id], hadE = tip && tip.contains(document.activeElement), n1 = atNode;
      if (!on) { show(null); if (hadE && n1 && n1.isConnected) { hush = true; n1.focus(); hush = false; } return; }
      if (!busy() && escKey(ew) !== shownKey) show(at, pinned, atNode);
      return;
    }
    if (!isPot(at) || busy()) return;
    var had = tip && tip.contains(document.activeElement);
    if (!seeds[at.id]) { var n0 = atNode; show(null); if (had && n0) { hush = true; n0.focus(); hush = false; } return; }
    if (potKey(seeds[at.id]) !== shownKey) show(at, pinned, atNode);   // unchanged: the focus in the card stays where it is
  }
  function init(e) {
    env = e; tip = document.getElementById('stip'); if (!tip) return;
    tip.addEventListener('click', function (ev) {
      var a = ev.target.closest && ev.target.closest('a'); if (!a) return;   // a pot card's buttons answer in place (web/answer.js); a link goes
      if (a.classList.contains('gchat') && typeof CHAT !== 'undefined') { ev.preventDefault(); CHAT.open(a.dataset.id, { none: function () { location.href = a.href; } }); }
      else if (a.classList.contains('ggoto') && env().go && env().go(a.dataset.id)) ev.preventDefault();   // a goat's "Zoom to it on the map" (garden.js cursorTo); off the map, the link's ?at= loads it
      show(null);
    });
    tip.addEventListener('pointerenter', function (ev) { if (ev.pointerType === 'mouse') clearTimeout(leaveTimer); });
    tip.addEventListener('pointerleave', function (ev) { if (ev.pointerType === 'mouse' && !pinned && !busy()) show(null); });
    ['pointerdown', 'dblclick', 'wheel'].forEach(function (t) { tip.addEventListener(t, function (ev) { ev.stopPropagation(); if (t === 'pointerdown' && carded(at)) pinned = true; }, { passive: true }); });   // a press on the card is not a drag of the map, and keeps the card
    tip.addEventListener('keydown', function (ev) {   // keys typed in the card are the card's, not the cursor's; Escape goes back to the pot
      ev.stopPropagation();
      if (ev.key === 'Escape') { ev.preventDefault(); var n = atNode; show(null); if (n) { hush = true; n.focus(); hush = false; } }
    });
    tip.addEventListener('focusout', function (ev) { var r = ev.relatedTarget; if (carded(at) && r && !tip.contains(r) && r !== atNode && !busy()) show(null); });   // Tab out of the card closes it
    document.addEventListener('pointerdown', function (ev) { if (at && !tip.contains(ev.target) && !(ev.target.closest && ev.target.closest('.w-steward,.w-gesc,.w-seed,.w-staked,.w-stone,.w-truck,.w-escalation')) && !(at.kind === 'goat' && ev.target.id === 'cv')) show(null); }, true);
    document.addEventListener('keydown', function (ev) { if (ev.key === 'Escape' && at) show(null); });
    document.addEventListener('farmdoc', function (ev) { onDoc(ev.detail); });
    document.addEventListener('garden:answered', function (ev) {   // answered from the Mailbox or the bead view: the card says so
      var id = ev.detail && ev.detail.id;
      if (isPot(at) && seeds[at.id] && seeds[at.id].decision && seeds[at.id].decision.id === id && !tip.contains(document.activeElement)) show(at, pinned, atNode);
    });
  }
  return { init: init, wire: wire, place: place, land: land, goat: goat, goatOf: goatOf, potCard: potCard, potState: potState, potTip: potTip, potSteps: potSteps, tap: tap };
})();
if (typeof module !== "undefined") module.exports = STIP;
