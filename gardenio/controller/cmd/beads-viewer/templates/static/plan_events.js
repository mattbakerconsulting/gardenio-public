// plan_events.js — live plan progress (epic ka-0000001409, task ka-0000001793;
// Matt's ask ka-0000008267: "the progress on it should be visible immediately").
//
// Subscribes to /plans/{id}/events (SSE). Unlike the agent activity lane
// (agent_events.js), which PREPENDS a row per event, the plan cockpit re-renders
// the WHOLE #plan-steps section server-side on each relevant child-bead change
// and emits it under the `plan-updated` event; this shim swaps the section's
// outerHTML in place, so the step timeline + progress bar reflect reality the
// moment a crew agent closes a step — no reload. The lifecycle (probe → connect →
// backoff → close) mirrors agent_events.js verbatim so auth-expiry handling stays
// consistent across the viewer. No htmx-sse extension is used.
(function () {
  'use strict';

  // syncOpenDecisionsChip mirrors the page-header "N to answer" chip to the
  // current #plan-decisions section's data-open-decisions count (hidden at
  // zero). Hoisted so the htmx:afterSwap listener below — which is registered
  // BEFORE the EventSource early-returns — can call it even when SSE is entirely
  // unavailable (task ka-0000003157, finding 1: the operator's own inline
  // resolve/dismiss is an htmx swap, not an SSE frame, so the header must sync
  // on the swap or it silently lags the section).
  function syncOpenDecisionsChip() {
    var section = document.getElementById('plan-decisions');
    var chip = document.getElementById('plan-open-decisions-chip');
    if (!section || !chip) return;
    var n = parseInt(section.getAttribute('data-open-decisions'), 10);
    if (isNaN(n)) return;
    if (n > 0) {
      chip.textContent = n + ' to answer';
      chip.hidden = false;
    } else {
      chip.hidden = true;
    }
  }

  // The header-chip sync runs on ANY htmx swap (syncOpenDecisionsChip is a cheap
  // idempotent read), registered here — before the EventSource early-returns
  // below — so it works even when EventSource is unsupported or no plan-events
  // anchor is present. This is the ONLY path that keeps the header count honest
  // after the operator's own inline answer when the SSE stream is down.
  if (typeof document !== 'undefined' && typeof document.addEventListener === 'function') {
    document.addEventListener('htmx:afterSwap', function () {
      syncOpenDecisionsChip();
    });
  }

  // composerInFlight holds the text of a plan-composer send that is currently in
  // flight — set on htmx:beforeRequest for the compose form, cleared when the
  // request settles (planComposerAfterRequest). It is the signal onAgentEvent's
  // draft-restore consults so a racing `plan-agent-updated` frame never RE-FILLS
  // a just-sent message into the freshly-swapped (empty) composer — the "text I
  // send doesn't always clear" bug (ka-0000002657). The mint of the operator's
  // turn bead fires a plan-agent-updated frame at send time; if that frame's
  // onAgentEvent runs while the composer still holds the sent text (before the
  // POST response's own swap lands — and when the frame WINS the race, the POST
  // then swaps a now-detached node, so its clear never reaches the screen), the
  // pre-swap snapshot below would otherwise resurrect it. The guard is keyed on
  // an IN-FLIGHT send, not a lingering "last sent" marker, so it is active only
  // during the exact window the race can happen and never false-suppresses a
  // genuine draft the operator re-typed later.
  var composerInFlight = null;
  function isPlanComposer(elt) {
    return !!(elt && elt.classList && elt.classList.contains('plan-assistant-form'));
  }
  if (typeof document !== 'undefined' && typeof document.addEventListener === 'function') {
    // beforeRequest ALWAYS fires while the form is still attached (the racing
    // frame can only detach it later), so a document-level listener reliably
    // captures the in-flight text.
    document.addEventListener('htmx:beforeRequest', function (evt) {
      var elt = evt && evt.detail && evt.detail.elt;
      if (!isPlanComposer(elt)) return;
      var ta = elt.querySelector('.plan-assistant-input');
      composerInFlight = ta ? ta.value : null;
    });
  }

  // planComposerAfterRequest is invoked by the compose form's inline
  // hx-on::after-request (NOT a document-level listener) so it runs even when a
  // racing frame has already swapped #plan-agent: htmx then fires afterRequest on
  // the now-DETACHED form, whose event does not bubble to document, but an inline
  // hx-on handler still runs on that element. It reconciles the VISIBLE composer
  // against the in-flight text captured at beforeRequest (composerInFlight is the
  // reliable source — the detached form's own textarea may have been reset):
  //   - success → if a racing restore resurrected the sent text into the visible
  //     composer, clear it (guarded on equality so a NEW message the operator
  //     started typing after the send is never wiped). The normal swap already
  //     empties the composer; this only undoes a resurrected value.
  //   - failure (non-2xx → htmx did not swap) → if the visible composer is empty
  //     because a racing frame swapped the card with the restore suppressed, put
  //     the operator's message back so it is never lost (ka-0000006168).
  window.planComposerAfterRequest = function (evt) {
    var d = evt && evt.detail;
    var ok = !!(d && (d.successful === true || (d.xhr && d.xhr.status >= 200 && d.xhr.status < 300)));
    var sent = composerInFlight;
    composerInFlight = null;
    if (!sent) return;
    var live = document.querySelector('#plan-agent .plan-assistant-input');
    if (!live) return;
    if (ok) {
      if (live.value === sent) live.value = '';
    } else if (!live.value) {
      live.value = sent;
    }
  };

  if (typeof window === 'undefined' || typeof window.EventSource === 'undefined') {
    return; // EventSource missing — the server-rendered steps stay static.
  }

  var anchor = document.querySelector('[data-plan-events]');
  if (!anchor) return;
  var url = anchor.getAttribute('data-plan-events');
  if (!url) return;

  // Same-origin auth probe before opening EventSource, so an expired AVA session
  // never trips the browser's "Blocked" indicator on the SSE request. Reuse the
  // fleet-wide /agents/events-status endpoint — it carries no per-stream state,
  // only the auth verdict (same reuse as agent_events.js).
  var statusUrl = (function () {
    var basePath = url.split('?')[0].replace(/\/plans\/.*$/, '');
    return basePath + '/agents/events-status';
  })();

  var es = null;
  var backoff = 1000;
  var maxBackoff = 30000;
  var closed = false;

  // swapById replaces the DOM node with matching id with the freshly-rendered
  // fragment the server sent. A soft flash class is applied so the operator's
  // eye catches the change; it is stripped on the next frame so a rapid burst
  // of updates keeps re-triggering the animation. Used by `plan-updated` (the
  // step timeline, S1), `plan-agent-updated` (the merged plan-agent card,
  // step 6 ka-0000000116) AND `plan-narrative-updated` (the summary + bundle
  // docs, S3, task ka-0000002473).
  function swapById(elementID, flashClass, html) {
    if (!html) return;
    var current = document.getElementById(elementID);
    if (!current) return;
    var tmp = document.createElement('div');
    tmp.innerHTML = html;
    var next = tmp.firstElementChild;
    if (!next || next.id !== elementID) return;
    current.parentNode.replaceChild(next, current);
    // Re-process the swapped fragment so its hx-* buttons are wired by htmx
    // again after the vanilla-DOM replace.
    if (window.htmx && typeof window.htmx.process === 'function') {
      try { window.htmx.process(next); } catch (e) { /* ignore */ }
    }
    next.classList.remove(flashClass);
    // force reflow so the animation restarts even on consecutive swaps
    void next.offsetWidth;
    next.classList.add(flashClass);
  }

  function onStepsEvent(ev) {
    swapById('plan-steps', 'plan-steps-updated', ev.data);
  }

  // The merged plan-agent card (step 6, ka-0000000116) re-renders on the ONE
  // `plan-agent-updated` frame — a gardener toggle/retire, an agent_state flip,
  // a plan_state change, or a new transcript turn all swap the same #plan-agent
  // section (this replaced the two former plan-gardener-updated /
  // plan-assistant-updated frames). Since 2b (ka-0000004607) the card carries
  // the persistent chat transcript (the shared bead_chat log) and 2c
  // (ka-0000000311) fires this frame on a new turn bead minted with
  // fields.plan_id, so a new operator/assistant line — and the "assistant is
  // typing…" bubble — appear through this one swap.
  //
  // The plan-agent-updated frame re-renders the ENTIRE #plan-agent card as one
  // node (transcript + compose box + levers), so a naive replaceChild would
  // (a) reset the transcript scroll to the top and (b) destroy the operator's
  // in-progress draft + caret. Both were reported as frustrating on /plans
  // (ka-0000000059 "rescrolls from the top", ka-0000006168 "clears any text I
  // have entered"). So we SNAPSHOT that state before the swap and RESTORE it
  // after — the whole-card swap contract stays, but it no longer clobbers what
  // the operator was reading or typing.
  //
  // Scroll: keep the standard chat "stick to bottom only if following along"
  // heuristic — pin to the newest line only when they were already near the
  // bottom, otherwise HOLD their prior scroll position (a background turn, a
  // typing-bubble flip, or a JetStream backfill replay must never yank someone
  // who scrolled UP to read older turns). Critically, the restore is done with
  // scroll-behavior:auto so it is an instant jump: the .bead-chat-log CSS sets
  // scroll-behavior:smooth, which was ANIMATING the post-swap scrollTop=0 →
  // bottom pin on every message — that visible animation IS the "rescroll from
  // the top" bug.
  var transcriptStickThreshold = 48; // px slack for "at the bottom"
  function onAgentEvent(ev) {
    var before = document.querySelector('#plan-agent [data-bead-chat-log]');
    var wasAtBottom = true; // default to sticky on first paint / when unmeasurable
    var prevScrollTop = 0;
    if (before) {
      prevScrollTop = before.scrollTop;
      wasAtBottom = (before.scrollHeight - before.scrollTop - before.clientHeight) <= transcriptStickThreshold;
    }

    // Snapshot the compose textarea draft + caret + focus so a background frame
    // never wipes what the operator is typing (ka-0000006168).
    var beforeInput = document.querySelector('#plan-agent .plan-assistant-input');
    var draft = null;
    if (beforeInput) {
      draft = {
        value: beforeInput.value,
        start: beforeInput.selectionStart,
        end: beforeInput.selectionEnd,
        focused: (document.activeElement === beforeInput)
      };
    }

    swapById('plan-agent', 'plan-agent-updated', ev.data);

    var after = document.querySelector('#plan-agent [data-bead-chat-log]');
    if (after) {
      // Instant jump, not a smooth animation — see comment above. Toggle
      // scroll-behavior off around the assignment and restore it, so any
      // future user-initiated scroll still eases.
      var prevBehavior = after.style.scrollBehavior;
      after.style.scrollBehavior = 'auto';
      after.scrollTop = wasAtBottom ? after.scrollHeight : prevScrollTop;
      void after.offsetWidth; // apply the jump before smooth is restored
      after.style.scrollBehavior = prevBehavior;
    }

    // Restore the operator's unsent draft. Only when the re-rendered textarea
    // came back EMPTY (a server re-render never carries the draft) — so we never
    // clobber a value the server intentionally set, and a just-sent message
    // (textarea already reset by the POST swap) leaves nothing to restore.
    //
    // Crucially, do NOT restore a message whose send is IN FLIGHT
    // (draft.value === composerInFlight): the mint of the operator's turn fires
    // this frame at send time, and re-filling the just-sent text is the "text I
    // send doesn't always clear" bug (ka-0000002657). The successful POST swap
    // will leave the composer empty; a failed send re-fills it on afterRequest,
    // so suppressing here never loses the message (ka-0000006168).
    if (draft && draft.value && draft.value !== composerInFlight) {
      var afterInput = document.querySelector('#plan-agent .plan-assistant-input');
      if (afterInput && !afterInput.value) {
        afterInput.value = draft.value;
        if (draft.focused) {
          afterInput.focus();
          try { afterInput.setSelectionRange(draft.start, draft.end); } catch (e) { /* ignore */ }
        }
      }
    }
  }

  // The narrative (summary + bundle docs) re-renders when the assistant edits
  // the plan — the server emits `plan-narrative-updated` on an epic-description
  // or bundle-doc-content change (S3, ka-0000002473). Same outerHTML swap +
  // flash idiom as the other lanes.
  function onNarrativeEvent(ev) {
    swapById('plan-narrative', 'plan-narrative-updated', ev.data);
  }

  // The clarifying-question decisions section (task ka-0000003157) re-renders
  // when a question is asked, answered, or dismissed — the server emits
  // `plan-decisions-updated` carrying the freshly-rendered #plan-decisions
  // section (root carries data-open-decisions). Same outerHTML swap + flash
  // idiom as the other lanes, PLUS a client-side update of the header
  // "N to answer" chip from the swapped section's data-open-decisions attr, so
  // answering in place reveals the next question live AND keeps the header count
  // honest (never section-live / header-static).
  function onDecisionsEvent(ev) {
    swapById('plan-decisions', 'plan-decisions-updated', ev.data);
    syncOpenDecisionsChip();
  }

  // The blocked-by-escalation banner (epic ka-0000005480, WS4) re-renders when
  // a blocking escalation closes (operator approves/rejects) or a new blocks
  // edge lands — the server emits `plan-escalations-updated` carrying the
  // freshly-rendered #plan-escalations section. The wrapper always renders, so
  // the swap clears the banner in place when the last escalation resolves.
  function onEscalationsEvent(ev) {
    swapById('plan-escalations', 'plan-escalations-updated', ev.data);
  }

  // The crew roster (ka-0000000150) re-renders when a crew agent the gardener
  // dispatched flips state (working→done, failed) or a new crew is spawned onto
  // a step — the server emits `plan-crew-updated` carrying the freshly-rendered
  // #plan-crew section. Same outerHTML swap + flash idiom as the other lanes;
  // the wrapper always renders, so the first crew spawn swaps the empty state
  // into the populated roster in place.
  function onCrewEvent(ev) {
    swapById('plan-crew', 'plan-crew-updated', ev.data);
  }

  // Live critique refresh (T5, ka-0000007360). The server emits
  // `plan-comments-updated` when a critique comment is posted on a plan STEP
  // that is in the stream's interest set; the payload is the freshly-rendered
  // plan_critique_thread fragment (root <section id="plan-critique-thread"
  // data-step-id=...>). Unlike the other lanes, the swap target is NOT a fixed
  // id in the base page — it lives inside the critique overlay owned by
  // plan_gutter.js, and must only swap when the overlay is open on THAT step.
  // plan_events.js owns the EventSource but not that overlay state, so it
  // re-dispatches the fragment as a DOM CustomEvent and lets plan_gutter.js
  // decide (mirrors the existing `plan-events:health` bridge between the two
  // files). No new EventSource — one stream feeds every plan lane.
  function onCommentsEvent(ev) {
    if (!ev || !ev.data) return;
    try {
      document.dispatchEvent(new CustomEvent('plan-comments-updated', {
        detail: { html: ev.data }
      }));
    } catch (e) { /* ignore — older browsers without CustomEvent */ }
  }

  function emitHealth(state) {
    try {
      document.dispatchEvent(new CustomEvent('plan-events:health', { detail: { state: state } }));
    } catch (e) { /* ignore — older browsers without CustomEvent */ }
    if (anchor) { anchor.setAttribute('data-plan-events-state', state); }
  }

  function probeAuth() {
    if (typeof window.fetch !== 'function') return Promise.resolve(null);
    var ctrl = (typeof window.AbortController === 'function') ? new AbortController() : null;
    var t = setTimeout(function () { if (ctrl) try { ctrl.abort(); } catch (e) {} }, 5000);
    return window.fetch(statusUrl, {
      method: 'GET',
      credentials: 'include',
      cache: 'no-store',
      redirect: 'manual',
      headers: { 'Accept': 'application/json' },
      signal: ctrl ? ctrl.signal : undefined
    }).then(function (resp) {
      clearTimeout(t);
      if (resp.type === 'opaqueredirect' || resp.status === 0) return 'auth-expired';
      if (resp.status === 401 || resp.status === 403) return 'auth-expired';
      if (resp.status >= 200 && resp.status < 300) return 'ok';
      return null;
    }).catch(function () {
      clearTimeout(t);
      return null;
    });
  }

  function openEventSource() {
    if (closed) return;
    try {
      es = new EventSource(url, { withCredentials: true });
    } catch (e) {
      emitHealth('error');
      scheduleReconnect();
      return;
    }
    es.addEventListener('plan-updated', onStepsEvent);
    es.addEventListener('plan-agent-updated', onAgentEvent);
    es.addEventListener('plan-narrative-updated', onNarrativeEvent);
    es.addEventListener('plan-decisions-updated', onDecisionsEvent);
    es.addEventListener('plan-escalations-updated', onEscalationsEvent);
    es.addEventListener('plan-crew-updated', onCrewEvent);
    es.addEventListener('plan-comments-updated', onCommentsEvent);
    es.onopen = function () {
      backoff = 1000;
      emitHealth('live');
    };
    es.onerror = function () {
      if (es) { try { es.close(); } catch (e) { /* ignore */ } }
      es = null;
      emitHealth('offline');
      probeAuth().then(function (verdict) {
        if (verdict === 'auth-expired') {
          closed = true;
          emitHealth('auth-expired');
          if (window.bvReauthOnEdgeExpiry) { window.bvReauthOnEdgeExpiry(); }
          return;
        }
        scheduleReconnect();
      });
    };
  }

  function connect() {
    if (closed) return;
    emitHealth('connecting');
    probeAuth().then(function (verdict) {
      if (closed) return;
      if (verdict === 'auth-expired') {
        closed = true;
        emitHealth('auth-expired');
        if (window.bvReauthOnEdgeExpiry) { window.bvReauthOnEdgeExpiry(); }
        return;
      }
      openEventSource();
    });
  }

  function scheduleReconnect() {
    if (closed) return;
    var delay = backoff;
    backoff = Math.min(backoff * 2, maxBackoff);
    setTimeout(connect, delay);
  }

  window.addEventListener('beforeunload', function () {
    closed = true;
    if (es) { try { es.close(); } catch (e) { /* ignore */ } }
  });

  connect();
})();
