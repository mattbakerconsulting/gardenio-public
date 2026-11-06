// beads-viewer interactive thread composer — optimistic render + reconcile.
//
// Phase 2 task 2-5 (ka-0000006034); hardened in Phase 3 task 3-5
// (ka-0000001087). The composer form (#thread-composer) posts the operator's
// message to the merged 2-2 write path (POST /b/{id}/thread-reply) via htmx
// (hx-swap="none"). This script layers the UX on top:
//
//   - On submit (htmx:beforeRequest for the form): paint the operator's line
//     into the conversation list (#thread2-convo) IMMEDIATELY as a pending
//     bubble, snapshot + clear the textarea so the UI feels instant. The bubble
//     carries data-optimistic-key (it is a local paint) AND data-msg-key (the
//     content-derived de-dup key the server stamps onto its authoritative echo).
//   - On a 2xx ack (htmx:afterRequest): fire a `thread-refresh` event at the
//     live timeline region (#thread-timeline) so its ?since= poll re-fetches.
//   - RECONCILE (htmx:beforeSwap/afterSwap on #thread-timeline): the timeline
//     swap replaces #thread2-convo wholesale, which would destroy the optimistic
//     bubbles. Before each swap we snapshot the live optimistic bubbles; after
//     the swap we reconcile them against the authoritative render COUNT-wise, not
//     merely by presence. For each content key we count how many authoritative
//     (server) bubbles carry it versus how many optimistic bubbles the operator
//     sent, drop one optimistic twin per echo, and re-append the surplus. This is
//     what makes an IDENTICAL repeated message survive (ka-0000005746): sending
//     "yes" twice paints two optimistic bubbles with the SAME data-msg-key, and a
//     presence test would delete BOTH against the first message's single echo.
//     Counting keeps `sent - echoed` bubbles, so each repeat persists until its
//     own echo lands (slow bridge forward / 204 between send and materialise),
//     then is removed exactly once — never a flicker, never a duplicate.
//   - On failure: mark the optimistic bubble failed, restore the draft into the
//     textarea, and surface the error line so the operator can retry. A failed
//     bubble is NOT preserved across swaps (no echo is coming); the error line
//     + restored draft, both outside the swap region, keep the operator informed.
//   - Keyboard: Cmd/Ctrl+Enter submits the form from the textarea (documented in
//     the composer hint), so the composer is fully keyboard-operable.
//
// Pure client-side, no dependencies beyond htmx. The send itself is entirely
// server-side and bridge-mediated; this file only manages the local DOM.
(function () {
  'use strict';

  var FORM_ID = 'thread-composer';
  var CONVO_ID = 'thread2-convo';
  var TIMELINE_ID = 'thread-timeline';
  var pendingSeq = 0;

  // RECONCILE_MISS_CAP bounds how many timeline swaps an acked "sent" optimistic
  // bubble may survive WITHOUT matching a server echo before it is dropped
  // (ka-0000006057 gap 5b). The reconcile pairs an optimistic bubble to its echo
  // by content key; if the key never matches — a key drift, or a bridge forward
  // that silently never materialised the bead — the bubble would otherwise linger
  // FOREVER as a permanent duplicate beside (or instead of) the real message. A
  // normal async forward echoes within 1–2 swaps, far under this cap, so the cap
  // only bites the genuinely-stuck case: after this many misses we drop the
  // optimistic bubble (the server echo, if it ever lands, still renders on its
  // own). At the 8s poll cadence this is ~40s of grace.
  var RECONCILE_MISS_CAP = 5;

  // pendingByXhr correlates each in-flight submit's XHR to the optimistic bubble
  // and draft it created, so two CONCURRENT sends never clobber each other
  // (ka-0000006057 gap 5a). The old code stashed a SINGLE pendingId/pendingDraft
  // on form.dataset, so a second submit fired before the first resolved would
  // overwrite the first's id — the first's afterRequest would then promote/fail
  // the WRONG bubble (mitigated today only by hx-disabled-elt serialising sends).
  // Keying on the request's own xhr (shared between htmx:beforeRequest and
  // htmx:afterRequest) makes each request carry its own state. A WeakMap lets a
  // dropped xhr be garbage-collected without an explicit cleanup path.
  var pendingByXhr = typeof WeakMap === 'function' ? new WeakMap() : null;

  // optimisticSnapshot holds {key, html} for each live optimistic bubble
  // captured in htmx:beforeSwap, consumed in the matching htmx:afterSwap.
  var optimisticSnapshot = [];

  // preSwapEchoes maps content key -> count of authoritative (server, non-
  // optimistic) bubbles present BEFORE the swap. Captured in htmx:beforeSwap and
  // diffed against the post-swap counts in htmx:afterSwap so the reconcile drops
  // only as many optimistic twins as there are NEWLY-arrived echoes this swap —
  // a count DELTA, not a cumulative presence test (ka-0000005746).
  var preSwapEchoes = Object.create(null);

  // echoCounts tallies authoritative (server) conversation bubbles by content
  // key within `convo`. Optimistic bubbles (data-optimistic-key) are excluded —
  // only server echoes count toward reconciling a just-sent message away.
  function echoCounts(convo) {
    var counts = Object.create(null);
    convo.querySelectorAll('.thread2-msg[data-msg-key]:not([data-optimistic-key])')
      .forEach(function (b) {
        var k = b.getAttribute('data-msg-key') || '';
        counts[k] = (counts[k] || 0) + 1;
      });
    return counts;
  }

  function el(id) { return document.getElementById(id); }

  // keyTrim strips leading/trailing whitespace for the de-dup key using the EXACT
  // SAME character set as Go's threadKeyTrim() (threads_curation.go), so the two
  // halves produce a byte-identical key. It is deliberately NOT plain
  // String.prototype.trim(): the JS \s set and Go's unicode.IsSpace set differ
  // at the edges (ka-0000006057 gap 1) — JS \s strips U+FEFF (BOM) but NOT
  // U+0085 (NEL), while Go's unicode.IsSpace strips NEL but not BOM. A leading or
  // trailing NEL/BOM would otherwise make the client key differ from the server
  // key so the optimistic bubble never reconciles and lingers as a permanent
  // duplicate. The regex trims the UNION: \s covers the shared set + BOM, and
  // \u0085 adds NEL — matching Go's unicode.IsSpace ∪ {U+FEFF} exactly.
  function keyTrim(s) {
    return String(s == null ? '' : s).replace(/^[\s\u0085]+|[\s\u0085]+$/g, '');
  }

  // msgKey computes the content-derived de-dup key for a message bubble. It MUST
  // stay byte-identical to threadMsgKey() in threads_curation.go: trim author +
  // text VIA keyTrim (the SAME union whitespace set — see keyTrim; plain .trim()
  // would drift on NEL/BOM), join with a single NUL byte, FNV-1a/32 over the
  // UTF-8 bytes, render base-16. The server stamps this key onto every
  // authoritative conversation bubble as data-msg-key; the optimistic bubble
  // carries the same key so the reconcile can match a just-sent message to its
  // server echo exactly.
  function msgKey(author, text) {
    var s = keyTrim(author) + '\u0000' + keyTrim(text);
    var bytes = new TextEncoder().encode(s);
    var h = 0x811c9dc5; // FNV-1a/32 offset basis
    for (var i = 0; i < bytes.length; i++) {
      h ^= bytes[i];
      h = Math.imul(h, 0x01000193) >>> 0; // FNV-1a/32 prime, kept unsigned
    }
    return (h >>> 0).toString(16);
  }

  // escapeHTML keeps an operator-typed message from injecting markup into the
  // optimistic bubble. The authoritative server render (next poll) re-escapes
  // through Go's html/template + goldmark, so this only guards the transient
  // local paint (which renders the body verbatim, not as markdown).
  function escapeHTML(s) {
    return String(s)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;')
      .replace(/'/g, '&#39;');
  }

  function composerForm(elt) {
    if (!elt || !elt.closest) return null;
    return elt.closest('#' + FORM_ID);
  }

  // dropPlaceholder removes the "no conversation yet" empty-state line so the
  // first optimistic (or re-appended) bubble does not sit beside it.
  function dropPlaceholder(convo) {
    if (!convo) return;
    var placeholder = convo.querySelector('.thread2-empty-line');
    if (placeholder) placeholder.remove();
  }

  // bubbleHTML builds the inner markup of a conversation bubble for the given
  // author/text. The body mirrors the server's `.thread2-msg-body comment-body`
  // shape (a single <p>) so the optimistic paint sits flush with the
  // authoritative markdown render that replaces it.
  function bubbleHTML(text, who, statusTag) {
    return '' +
      '<div class="thread2-msg-meta">' +
      '<span class="thread2-msg-author">' + escapeHTML(who || 'you') + '</span>' +
      '<span class="thread2-msg-pending-tag">' + statusTag + '</span>' +
      '</div>' +
      '<div class="thread2-msg-body comment-body"><p>' + escapeHTML(text) + '</p></div>';
  }

  // paintPending inserts a dashed "sending…" bubble at the end of the
  // conversation list and returns its element so the result handler can
  // promote it to failed or leave it for the poll to reconcile. The bubble
  // carries data-optimistic-key (it is a local paint) and data-msg-key (the
  // content key the server echo will also carry) so the reconcile can match it.
  function paintPending(text, who) {
    var convo = el(CONVO_ID);
    if (!convo) return null;
    dropPlaceholder(convo);

    pendingSeq += 1;
    var li = document.createElement('li');
    li.className = 'thread2-msg is-pending';
    li.setAttribute('data-pending-id', String(pendingSeq));
    li.setAttribute('data-optimistic-key', String(pendingSeq));
    li.setAttribute('data-msg-key', msgKey(who, text));
    li.innerHTML = bubbleHTML(text, who, 'sending…');
    convo.appendChild(li);
    li.scrollIntoView({ block: 'nearest' });
    return li;
  }

  function authorFromForm(form) {
    var strong = form.querySelector('.thread2-composer-label strong');
    return (strong && strong.textContent) ? strong.textContent.trim() : 'you';
  }

  function showError(form, msg) {
    var p = form.querySelector('.thread2-composer-error');
    if (!p) return;
    p.textContent = msg;
    p.hidden = false;
  }

  function clearError(form) {
    var p = form.querySelector('.thread2-composer-error');
    if (!p) return;
    p.textContent = '';
    p.hidden = true;
  }

  // Keyboard: Cmd/Ctrl+Enter submits the composer from the textarea. Plain Enter
  // keeps inserting a newline (the field is multi-line), so the shortcut is the
  // documented send path (see the composer hint). Uses requestSubmit so the
  // form's `required`/maxlength validation + htmx hx-post fire exactly as a Send
  // click would.
  document.addEventListener('keydown', function (evt) {
    if (evt.key !== 'Enter' || !(evt.metaKey || evt.ctrlKey)) return;
    var t = evt.target;
    if (!t || t.id !== 'thread-composer-text') return;
    var form = composerForm(t);
    if (!form) return;
    evt.preventDefault();
    if (typeof form.requestSubmit === 'function') {
      form.requestSubmit();
    } else {
      // Older engines: dispatch a submit event htmx is listening for.
      form.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    }
  });

  // stashPending records this request's optimistic state, keyed by its own xhr so
  // concurrent sends never share one slot (ka-0000006057 gap 5a). Falls back to
  // form.dataset only when WeakMap is unavailable (very old engines) — the legacy
  // single-slot behaviour, no worse than before.
  function stashPending(form, xhr, draft, pendingID) {
    if (pendingByXhr && xhr) {
      pendingByXhr.set(xhr, { draft: draft, pendingID: pendingID });
      return;
    }
    form.dataset.pendingDraft = draft;
    form.dataset.pendingId = pendingID;
  }

  // takePending retrieves and clears this request's optimistic state by its xhr,
  // falling back to the form.dataset single-slot when WeakMap is unavailable.
  function takePending(form, xhr) {
    if (pendingByXhr && xhr && pendingByXhr.has(xhr)) {
      var rec = pendingByXhr.get(xhr);
      pendingByXhr.delete(xhr);
      return rec;
    }
    var rec2 = { draft: form.dataset.pendingDraft || '', pendingID: form.dataset.pendingId || '' };
    delete form.dataset.pendingDraft;
    delete form.dataset.pendingId;
    return rec2;
  }

  // Before the POST fires: snapshot the draft, paint the optimistic bubble, and
  // clear the textarea so the field is ready for the next message.
  document.addEventListener('htmx:beforeRequest', function (evt) {
    var detail = evt && evt.detail;
    if (!detail || !detail.elt) return;
    var form = composerForm(detail.elt);
    if (!form) return;
    var ta = form.querySelector('textarea[name="text"]');
    if (!ta) return;
    var text = ta.value.trim();
    if (!text) return; // required attr + server both reject empty; nothing to paint.
    clearError(form);
    var li = paintPending(text, authorFromForm(form));
    // Stash per-request (keyed by this send's xhr): the draft (to restore on
    // failure) and the pending bubble id (to promote to sent/failed). Keying on
    // the xhr keeps two concurrent sends from clobbering each other's ids.
    stashPending(form, detail.xhr, text, li ? li.getAttribute('data-pending-id') : '');
    ta.value = '';
  });

  // After the POST resolves: success -> trigger a timeline refresh so the poll
  // reconciles; failure -> mark the bubble failed and restore the draft.
  document.addEventListener('htmx:afterRequest', function (evt) {
    var detail = evt && evt.detail;
    if (!detail || !detail.elt) return;
    var form = composerForm(detail.elt);
    if (!form) return;
    var xhr = detail.xhr;
    var ok = detail.successful === true ||
      (xhr && xhr.status >= 200 && xhr.status < 300);

    var rec = takePending(form, xhr);
    var pendingID = rec.pendingID || '';
    var bubble = pendingID
      ? document.querySelector('.thread2-msg[data-pending-id="' + pendingID + '"]')
      : null;

    if (ok) {
      // Promote the optimistic bubble from "sending…" to "sent" — the POST
      // succeeded; the bead is now in flight to Slack via the bridge and will
      // appear in the authoritative render on a subsequent poll (the bridge
      // forward is async, so the immediate refresh below may still 204). The
      // bubble lingers as a confirmed "sent" line — preserved across timeline
      // swaps by the reconcile below — until its server echo (matched by
      // data-msg-key) lands and replaces it (or the reconcile grace period
      // elapses; see RECONCILE_MISS_CAP).
      if (bubble) {
        bubble.classList.remove('is-pending');
        bubble.classList.add('is-sent');
        var sentTag = bubble.querySelector('.thread2-msg-pending-tag');
        if (sentTag) sentTag.textContent = 'sent';
      }
      // The authoritative render arrives on the next poll; nudge it now so the
      // reconcile is near-instant rather than waiting the full poll interval.
      var tl = el(TIMELINE_ID);
      if (tl && window.htmx && typeof window.htmx.trigger === 'function') {
        window.htmx.trigger(tl, 'thread-refresh');
      }
      return;
    }

    // Failure: surface it, mark the optimistic bubble, and restore the draft so
    // the operator can retry without retyping. Clearing data-optimistic-key
    // exempts the failed bubble from cross-swap preservation (no echo is coming);
    // it is destroyed by the next swap, while the error line + restored draft
    // (both outside #thread-timeline) keep the failure visible.
    if (bubble) {
      bubble.classList.remove('is-pending');
      bubble.classList.add('is-failed');
      bubble.removeAttribute('data-optimistic-key');
      var tag = bubble.querySelector('.thread2-msg-pending-tag');
      if (tag) tag.textContent = 'failed to send';
    }
    var ta = form.querySelector('textarea[name="text"]');
    if (ta && !ta.value) ta.value = rec.draft || '';
    var msg = 'Failed to send. Your message was restored — try again.';
    if (xhr && xhr.status === 409) msg = 'This wish is not bound to a Slack conversation.';
    if (xhr && xhr.status === 403) msg = 'You do not have permission to post here.';
    showError(form, msg);
  });

  // isTimelineSwap reports whether an htmx swap event targets the live timeline
  // region (#thread-timeline), so the reconcile only runs for the poll swap and
  // never for unrelated swaps elsewhere on the page.
  function isTimelineSwap(evt) {
    var tgt = evt && evt.detail && evt.detail.target;
    return !!tgt && tgt.id === TIMELINE_ID;
  }

  // Before the timeline swap destroys the current #thread2-convo, snapshot every
  // live optimistic bubble (pending or sent, but not failed) AND tally the server
  // echoes already present per key. afterSwap re-paints the surviving optimistic
  // bubbles and drops only as many as there are NEWLY-arrived echoes this swap.
  document.addEventListener('htmx:beforeSwap', function (evt) {
    if (!isTimelineSwap(evt)) return;
    optimisticSnapshot = [];
    var convo = el(CONVO_ID);
    // Server echoes already rendered before this swap. Diffed against the post-
    // swap counts so only the delta (new echoes) reconciles optimistic twins.
    preSwapEchoes = convo ? echoCounts(convo) : Object.create(null);
    var bubbles = document.querySelectorAll(
      '#' + CONVO_ID + ' .thread2-msg[data-optimistic-key]:not(.is-failed)');
    bubbles.forEach(function (b) {
      optimisticSnapshot.push({
        key: b.getAttribute('data-msg-key') || '',
        html: b.outerHTML,
      });
    });
  });

  // After the swap brings in the authoritative render, reconcile the snapshotted
  // optimistic bubbles against it by COUNT DELTA, grouped by content key. For a
  // key K let newEchoes = (server bubbles with key K after the swap) - (server
  // bubbles with key K before it): the number of echoes that MATERIALISED on this
  // swap. We drop exactly `newEchoes` optimistic twins for K and re-append the
  // rest, so an optimistic bubble survives until ITS OWN echo lands.
  //
  // This is the fix for ka-0000005746. The old reconcile was presence-based: it
  // dropped an optimistic bubble whenever ANY server bubble shared its key, so
  // sending the same text twice ("yes"/"yes") lost the second send against the
  // first message's single echo. A raw cumulative count over-corrects the other
  // way — on a later poll the still-rendered earlier echo would be re-counted and
  // over-drop the surplus. The pre/post delta counts only what arrived THIS swap,
  // so N identical sends reconcile against exactly N echoes, one per poll they
  // land on, never a duplicate and never a vanished repeat. Keyless snapshots
  // (defensive — real bubbles always carry a key) can never match an echo and are
  // always re-appended.
  document.addEventListener('htmx:afterSwap', function (evt) {
    if (!isTimelineSwap(evt)) return;
    var snap = optimisticSnapshot;
    var before = preSwapEchoes;
    optimisticSnapshot = [];
    preSwapEchoes = Object.create(null);
    if (!snap.length) return;
    var convo = el(CONVO_ID);
    if (!convo) return;

    // newByKey[K] = echoes that arrived on THIS swap = after - before, floored at
    // zero. Decremented as we consume one echo per dropped optimistic twin so the
    // surplus (sends still awaiting their echo) is re-appended in order.
    var after = echoCounts(convo);
    var newByKey = Object.create(null);
    Object.keys(after).forEach(function (k) {
      var delta = after[k] - (before[k] || 0);
      if (delta > 0) newByKey[k] = delta;
    });

    snap.forEach(function (item) {
      if (item.key && newByKey[item.key] > 0) {
        // This send's echo materialised on this swap — consume it so a second
        // same-key send is NOT also matched against the same echo.
        newByKey[item.key] -= 1;
        return;
      }
      // No fresh echo to cover this send: re-paint the optimistic bubble so the
      // operator's message persists to the next poll.
      var wrap = document.createElement('div');
      wrap.innerHTML = item.html;
      var li = wrap.firstElementChild;
      if (!li) return;
      // Grace-period drop (ka-0000006057 gap 5b): an ACKED "sent" bubble that
      // never matches a server echo — a residual content-key drift, or a bridge
      // forward that silently never materialised the bead — would otherwise
      // linger FOREVER as a duplicate. Bound its life: count the swaps it has
      // survived without an echo (persisted in data-reconcile-misses, which
      // round-trips through the outerHTML snapshot), and once it reaches
      // RECONCILE_MISS_CAP, drop it instead of re-appending. Only acked (is-sent)
      // bubbles are aged — a still-"sending…" bubble has not been promised an
      // echo yet, so it always re-appends. A normal echo reconciles in 1–2 swaps,
      // well under the cap; the server echo (if it ever lands) still renders.
      if (li.classList.contains('is-sent')) {
        var misses = (parseInt(li.getAttribute('data-reconcile-misses'), 10) || 0) + 1;
        if (misses >= RECONCILE_MISS_CAP) return; // drop, don't re-append
        li.setAttribute('data-reconcile-misses', String(misses));
      }
      dropPlaceholder(convo);
      convo.appendChild(li);
    });
  });
})();
