// up.js — the /up single-card action footer + global keymap driver.
//
// Task ka-0000004641 (GV phase-2). PRD §4.2 (global keymap), §4.3 (per-type
// footer), §4.4 (action result UX), §8.4 (chatbox-suspend gate).
//
// SELF-GATING (mirrors deck_swipe.js): this module bails instantly on any page
// that is not /up — its gate is the presence of `[data-surface="up"]`. So it is
// safe to ship in the shared layout <script> list; every other page pays only a
// single querySelector.
//
// SOURCE OF TRUTH. The Go side (up_actions.go) emits BOTH the visible footer
// (rendered buttons carrying data-up-* attributes) AND `window.UP_KEYMAP` (the
// JSON island). This module reads UP_KEYMAP for keyboard dispatch and the
// buttons carry the same attributes for click dispatch, so the two can never
// drift — there is exactly one action table.
//
// WHAT IS LIVE vs DEFERRED. Actions whose backend exists today (close, comment,
// reassign, decision resolve/dismiss, inbox-pin) POST for real and apply the
// §4.4 result UX. Actions the PRD marks NEW / sibling-owned (snooze, escalate-
// create, oracle stack ops, molecule/formula apply, agent verbs, schedule
// run-now) carry data-up-deferred and, on dispatch, surface a footer toast
// ("lands with a later phase") instead of hitting a missing endpoint or lying
// about success. The keymap + drawers compose now; the wire lands later.
//
// CHATBOX-SUSPEND GATE (§8.4). The chatbox is a sibling task (ka-0000000086).
// This module already routes every keypress through `chatboxFocused()`, a stub
// that returns false today. When the chatbox ships and marks its input, the
// gate flips and the card keymap suspends with no change here — the footer's
// `.up-footer-suspended` hint is toggled from the same gate.
(function () {
  'use strict';

  // --- gate: bail instantly on any non-/up page -------------------------------
  var surface = document.querySelector('[data-surface="up"]');
  if (!surface) return;

  var BASE = (window.BEADS_VIEWER_BASE_PATH || '').replace(/\/+$/, '');
  var keymap = window.UP_KEYMAP || null;

  var card = surface.querySelector('.up-card');
  // No card (empty stack) ⇒ nothing to drive. The empty state is a plain page.
  if (!card) return;

  var beadID = card.getAttribute('data-bead-id') || '';
  var beadType = card.getAttribute('data-bead-type') || '';

  // ---- small DOM helpers -----------------------------------------------------
  function q(sel, root) { return (root || surface).querySelector(sel); }
  function qa(sel, root) {
    return Array.prototype.slice.call((root || surface).querySelectorAll(sel));
  }
  function isEditable(el) {
    if (!el) return false;
    var tag = (el.tagName || '').toLowerCase();
    if (tag === 'input' || tag === 'textarea' || tag === 'select') return true;
    return !!el.isContentEditable;
  }

  // §8.4 chatbox-suspend gate. The chatbox lands in a sibling task; until then
  // this is always false. When the chatbox ships it will mark its input with
  // [data-up-chatbox-input] (or set data-up-chat-focused on the surface) and
  // this gate flips with no other change here.
  function chatboxFocused() {
    if (surface.getAttribute('data-up-chat-focused') === '1') return true;
    var ae = document.activeElement;
    return !!(ae && ae.closest && ae.closest('[data-up-chatbox-input]'));
  }

  // Reflect the suspend state into the footer hint (§8.4 "footer dims").
  function reflectSuspend() {
    var hint = q('.up-footer-suspended');
    var live = q('.up-footer-hint');
    var suspended = chatboxFocused();
    if (hint) hint.hidden = !suspended;
    if (live) live.hidden = suspended;
    card.classList.toggle('up-keys-suspended', suspended);
  }

  // ---- footer toast (§4.4 failure / deferred) --------------------------------
  var toastTimer = null;
  function toast(msg, kind) {
    var el = q('.up-footer-toast');
    if (!el) { return; }
    el.textContent = msg;
    el.className = 'up-footer-toast' + (kind ? ' up-toast-' + kind : '');
    el.hidden = false;
    if (toastTimer) window.clearTimeout(toastTimer);
    // Deferred/info toasts self-dismiss; failure toasts with a retry stay until
    // the next dispatch (retry re-issues the same action).
    if (kind !== 'error') {
      toastTimer = window.setTimeout(function () { el.hidden = true; }, 3200);
    }
  }
  function clearToast() {
    var el = q('.up-footer-toast');
    if (el) { el.hidden = true; el.textContent = ''; }
  }

  // Flash ✓ on the primary for 800ms (§4.4 row 2: succeeded-still-in-stack).
  function flashOK() {
    var p = q('.up-action-primary');
    if (!p) return;
    p.classList.add('up-flash-ok');
    window.setTimeout(function () { p.classList.remove('up-flash-ok'); }, 800);
  }

  // ---- navigation (skip / advance / back) ------------------------------------
  // The real per-operator stack + in-page card swap land with the WS task
  // (ka-0000005517). In this proto skip/advance re-query the /up auto-picker via
  // a full navigation, which gives browser-back card history for free (each card
  // is a page load). A terminal action removes the bead from the picker's set,
  // so advancing genuinely yields the next card; skip re-queries and is honest
  // about landing on the same top card until the stack op exists.
  function advance() { window.location.assign(BASE + '/up'); }
  function skip() { window.location.assign(BASE + '/up'); }
  function back() {
    if (window.history.length > 1) window.history.back();
    else window.location.assign(BASE + '/up');
  }
  function openBead(newTab) {
    var href = BASE + '/' + beadID;
    if (newTab) window.open(href, '_blank', 'noopener');
    else window.location.assign(href);
  }

  // ---- drawers + overlay -----------------------------------------------------
  // The action that opened the drawer, so submit knows whether to auto-advance
  // (bug's "comment + close" advances; a plain `c` comment stays — §4.4).
  var drawerAction = null;

  function openDrawer(name, action) {
    closeAllDrawers();
    var d = q('[data-up-drawer="' + name + '"]') || q('[data-up-overlay="' + name + '"]');
    if (!d) return;
    drawerAction = action || null;
    d.hidden = false;
    var input = q('.up-drawer-input', d);
    if (input) { input.value = ''; setTimeout(function () { input.focus(); }, 0); }
  }
  function closeAllDrawers() {
    qa('[data-up-drawer], [data-up-overlay]').forEach(function (d) { d.hidden = true; });
    drawerAction = null;
  }
  function anyDrawerOpen() {
    return qa('[data-up-drawer], [data-up-overlay]').some(function (d) { return !d.hidden; });
  }

  // ---- help overlay (§4.2 `?`) -----------------------------------------------
  var helpDialog = null;
  function buildHelp() {
    if (helpDialog) return helpDialog;
    helpDialog = document.createElement('dialog');
    helpDialog.className = 'up-help';
    var rows = [];
    function row(a) {
      if (!a) return;
      rows.push('<tr><td><kbd>' + escapeHTML(a.key) + '</kbd></td><td>' +
        escapeHTML(a.label) + (a.deferred ? ' <span class="up-help-deferred">(soon)</span>' : '') +
        '</td><td>' + escapeHTML(a.help || '') + '</td></tr>');
    }
    if (keymap) {
      if (keymap.primary) {
        rows.push('<tr class="up-help-section"><td colspan="3">Primary (this ' + escapeHTML(beadType || 'bead') + ')</td></tr>');
        row(keymap.primary);
      }
      if (keymap.secondaries && keymap.secondaries.length) {
        rows.push('<tr class="up-help-section"><td colspan="3">Secondary actions</td></tr>');
        keymap.secondaries.forEach(row);
      }
      if (keymap.global && keymap.global.length) {
        rows.push('<tr class="up-help-section"><td colspan="3">Always available</td></tr>');
        keymap.global.forEach(row);
      }
    }
    helpDialog.innerHTML =
      '<form method="dialog" class="up-help-form">' +
      '<h2>Keyboard shortcuts</h2>' +
      '<table class="up-help-table"><tbody>' + rows.join('') + '</tbody></table>' +
      '<p class="up-help-actions"><button type="submit" autofocus>Close</button></p>' +
      '</form>';
    document.body.appendChild(helpDialog);
    return helpDialog;
  }
  function toggleHelp() {
    var d = buildHelp();
    if (d.open) { d.close(); return; }
    if (typeof d.showModal === 'function') d.showModal();
    else d.setAttribute('open', '');
  }
  function helpOpen() { return !!(helpDialog && helpDialog.open); }

  function escapeHTML(s) {
    return String(s == null ? '' : s)
      .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;');
  }

  // ---- POST dispatch (§4.4) --------------------------------------------------
  // Same-origin form POST; CSRF posture is SameSite=Lax cookies + same-origin
  // (middleware.go), matching how htmx posts these very endpoints. We only care
  // about the status: ok ⇒ apply the success UX, else ⇒ failure toast + retry.
  function post(endpoint, body, opts) {
    opts = opts || {};
    var url = BASE + endpoint.replace('{id}', encodeURIComponent(beadID));
    var params = new URLSearchParams();
    if (body) { Object.keys(body).forEach(function (k) { params.append(k, body[k]); }); }
    clearToast();
    setBusy(true);
    return fetch(url, {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body: params.toString(),
    }).then(function (res) {
      setBusy(false);
      if (res.ok) {
        if (opts.advance) advance();
        else { flashOK(); if (opts.onStay) opts.onStay(); }
        return true;
      }
      failureToast(res.status, opts.retry);
      return false;
    }).catch(function () {
      setBusy(false);
      failureToast(0, opts.retry);
      return false;
    });
  }

  var inFlight = false;
  function setBusy(b) { inFlight = b; card.classList.toggle('up-busy', b); }

  function failureToast(status, retry) {
    var el = q('.up-footer-toast');
    if (!el) return;
    var label = status ? ('Action failed (' + status + ')') : 'Action failed (network)';
    el.innerHTML = '';
    el.className = 'up-footer-toast up-toast-error';
    var span = document.createElement('span');
    span.textContent = label + ' — ';
    el.appendChild(span);
    var btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'up-toast-retry';
    btn.textContent = 'Retry';
    btn.addEventListener('click', function () { if (typeof retry === 'function') retry(); });
    el.appendChild(btn);
    el.hidden = false;
  }

  // ---- action dispatch -------------------------------------------------------
  // Route one resolved action (from a key or a button) to its behavior.
  function dispatch(action) {
    if (!action) return;
    // Deferred actions never hit the wire — honest phase toast (§10 / Q8-C).
    if (action.deferred) {
      var verb = (action.label || 'This action');
      toast(verb + ' lands with a later phase (chatbox / WS / endpoint pending).', 'info');
      return;
    }
    switch (action.kind) {
      case 'post':
        if (!action.endpoint) return;
        // The decision primary is "Resolve: recommended" — it must carry the
        // recommended (first) option id, or the resolve endpoint 400s on an
        // empty body ("Pick an option or write a response"). Route it through
        // resolveDecision so Enter resolves with the recommended option and the
        // chips resolve with theirs, one path.
        if (action.endpoint.indexOf('/decision/resolve') !== -1) {
          var rec = recommendedOption();
          if (!rec) { toast('This decision has no options to resolve — open the bead to respond.', 'info'); return; }
          resolveDecision(rec);
          return;
        }
        var self = function () { dispatch(action); };
        post(action.endpoint, {}, { advance: !!action.autoAdvance, retry: self });
        return;
      case 'drawer-comment':
        openDrawer('comment', action);
        return;
      case 'drawer-escalate':
        openDrawer('escalate', action);
        return;
      case 'overlay-reassign':
        openDrawer('reassign', action);
        return;
      case 'skip':
        skip();
        return;
      case 'back':
        back();
        return;
      case 'open':
        openBead(true);
        return;
      case 'help':
        toggleHelp();
        return;
      case 'palette':
        openPalette();
        return;
      case 'focus-chat':
        toast('Chatbox lands with a later phase.', 'info');
        return;
      case 'chatbox':
        toast((action.label || 'This action') + ' lands with a later phase.', 'info');
        return;
      default:
        return;
    }
  }

  // cmdk.js owns Cmd-K globally; the ⋯more button and `palette` action open it
  // by re-dispatching that same shortcut so there is one palette entry point.
  function openPalette() {
    var ev;
    try {
      ev = new KeyboardEvent('keydown', { key: 'k', metaKey: true, bubbles: true, cancelable: true });
    } catch (_) { ev = null; }
    if (ev) document.dispatchEvent(ev);
  }

  // Resolve a keyboard key to an action from the keymap. Handles the aliases in
  // §4.2 (space/j → skip, ← / k → back) and the read-only Enter→skip rule.
  function actionForKey(key) {
    if (!keymap) return null;
    // Enter: primary, or skip when read-only (§5.2 last paragraph).
    if (key === 'Enter') {
      if (keymap.readOnly || !keymap.primary) return { kind: 'skip' };
      return keymap.primary;
    }
    // Skip aliases.
    if (key === '→' || key === 'ArrowRight' || key === ' ' || key === 'Spacebar' || key === 'j') {
      return { kind: 'skip' };
    }
    // Back aliases.
    if (key === '←' || key === 'ArrowLeft' || key === 'k' || key === 'Escape') {
      return { kind: 'back' };
    }
    // Otherwise search secondaries then global for a matching key.
    var pools = [keymap.secondaries || [], keymap.global || []];
    for (var i = 0; i < pools.length; i++) {
      for (var j = 0; j < pools[i].length; j++) {
        if (pools[i][j].key === key) return pools[i][j];
      }
    }
    return null;
  }

  // ---- drawer submit handling ------------------------------------------------
  // A plain `c` comment stays on the card and flashes ✓ (§4.4 row 2). The bug
  // primary "Comment + close" (a drawer-comment action flagged autoAdvance)
  // must actually CLOSE the bead after the comment lands — the comment is the
  // confirm — then advance. So on that path we chain the close POST and only
  // advance once the close succeeds; a failed close leaves a retryable toast
  // rather than silently advancing past an un-closed bug.
  function submitComment() {
    var input = q('#up-comment-text');
    if (!input) return;
    var text = input.value.trim();
    if (!text) { input.focus(); return; }
    var closeAfter = !!(drawerAction && drawerAction.autoAdvance);
    var invoking = drawerAction;
    var closeStep = function () {
      // The comment already landed; only the close needs retrying here — never
      // re-post the comment (that would duplicate it).
      post('/b/{id}/close', {}, { advance: true, retry: closeStep });
    };
    post('/b/{id}/comments', { text: text }, {
      advance: false,
      // Comment POST failed before landing ⇒ safe to retry the whole submit.
      retry: function () { drawerAction = invoking; submitComment(); },
      onStay: function () {
        closeAllDrawers();
        if (closeAfter) closeStep();
        else flashOK();
      },
    });
  }

  function submitReassign() {
    var input = q('#up-reassign-input');
    if (!input) return;
    var assignee = input.value.trim();
    post('/b/{id}/assignee', { assignee: assignee }, {
      advance: false,
      retry: function () { submitReassign(); },
      onStay: function () { closeAllDrawers(); flashOK(); },
    });
  }

  function submitEscalate() {
    // Escalation-create is sibling-owned (PRD §10). The drawer composes now;
    // submitting surfaces the honest phase toast until the endpoint lands.
    closeAllDrawers();
    toast('Escalate lands with the escalation endpoint (PRD §10).', 'info');
  }

  function submitDrawer(name) {
    if (name === 'comment') submitComment();
    else if (name === 'reassign') submitReassign();
    else if (name === 'escalate') submitEscalate();
  }

  // ---- decision resolve chips ------------------------------------------------
  // Clicking a chip resolves the decision with that option id (§3.3). The
  // primary "Resolve: recommended" resolves with the FIRST chip's option.
  function resolveDecision(optionID) {
    if (!optionID) { toast('No option selected to resolve.', 'info'); return; }
    var self = function () { resolveDecision(optionID); };
    post('/b/{id}/decision/resolve', { option_id: optionID }, { advance: true, retry: self });
  }

  // recommendedOption is the first (starred) decision chip's option id — the
  // "recommended" the Enter primary resolves with. Returns "" when the decision
  // carried no parsed options (the Go footer still renders a plain "Resolve"
  // primary, but there is nothing to one-tap resolve — up.js routes the operator
  // to the full bead view instead of POSTing an empty body).
  function recommendedOption() {
    var first = q('.up-decision-chip');
    return first ? (first.getAttribute('data-up-option') || '') : '';
  }

  // ---- event wiring ----------------------------------------------------------
  // Footer / primary / secondary button clicks reuse the same dispatch as keys.
  surface.addEventListener('click', function (e) {
    var t = e.target;
    // Decision chip.
    var chip = t.closest && t.closest('.up-decision-chip');
    if (chip) {
      e.preventDefault();
      resolveDecision(chip.getAttribute('data-up-option') || '');
      return;
    }
    // Drawer submit / cancel.
    var submitBtn = t.closest && t.closest('[data-up-drawer-submit]');
    if (submitBtn) { e.preventDefault(); submitDrawer(submitBtn.getAttribute('data-up-drawer-submit')); return; }
    var cancelBtn = t.closest && t.closest('[data-up-drawer-cancel]');
    if (cancelBtn) { e.preventDefault(); closeAllDrawers(); focusPrimary(); return; }
    // "Open full bead view" is a plain link — let it navigate.
    if (t.closest && t.closest('[data-up-open]')) return;
    // Every footer/primary/⋯more button carries its full action as data-up-*
    // attributes, so a click builds the action straight off the button — the
    // same action shape a key resolves to, dispatched through one path.
    var btn = t.closest && t.closest('[data-up-key]');
    if (btn) {
      e.preventDefault();
      dispatch(actionFromButton(btn));
      return;
    }
  });

  // actionFromButton reads the data-up-* attributes the Go footer stamped onto a
  // button into the same action shape actionForKey returns from the keymap.
  function actionFromButton(btn) {
    return {
      kind: btn.getAttribute('data-up-kind'),
      endpoint: btn.getAttribute('data-up-endpoint') || '',
      method: btn.getAttribute('data-up-method') || 'POST',
      autoAdvance: btn.getAttribute('data-up-advance') === '1',
      deferred: btn.getAttribute('data-up-deferred') === '1',
      label: (btn.textContent || '').trim(),
    };
  }

  function focusPrimary() {
    var p = q('.up-action-primary');
    if (p && typeof p.focus === 'function') p.focus();
  }

  // Global keydown — the §4.2 keymap. Order of precedence:
  //   1. A modal drawer/overlay is open ⇒ Esc closes it, Enter/⌘Enter submits.
  //   2. The help overlay is open ⇒ Esc/`?` closes it (native <dialog> Esc too).
  //   3. An editable element or the chatbox is focused ⇒ card keys suspended.
  //   4. Otherwise ⇒ resolve the key against the keymap and dispatch.
  document.addEventListener('keydown', function (e) {
    // Never fight modifier chords except our own ⌘Enter drawer-submit; cmd-K is
    // owned by cmdk.js and passes straight through.
    if (e.metaKey || e.ctrlKey) {
      if ((e.key === 'Enter') && anyDrawerOpen()) {
        e.preventDefault();
        var openD = qa('[data-up-drawer], [data-up-overlay]').find(function (d) { return !d.hidden; });
        if (openD) submitDrawer(openD.getAttribute('data-up-drawer') || openD.getAttribute('data-up-overlay'));
      }
      return;
    }
    if (e.altKey) return;

    // (1) Modal drawer/overlay open.
    if (anyDrawerOpen()) {
      var d = qa('[data-up-drawer], [data-up-overlay]').find(function (x) { return !x.hidden; });
      var name = d && (d.getAttribute('data-up-drawer') || d.getAttribute('data-up-overlay'));
      if (e.key === 'Escape') { e.preventDefault(); closeAllDrawers(); focusPrimary(); return; }
      // Single-line inputs (escalate/reassign) submit on plain Enter; the
      // multi-line comment textarea reserves Enter for newlines (⌘Enter above).
      if (e.key === 'Enter' && name !== 'comment') {
        var ae = document.activeElement;
        if (ae && ae.closest && ae.closest('[data-up-drawer], [data-up-overlay]')) {
          e.preventDefault();
          submitDrawer(name);
        }
      }
      return;
    }

    // (2) Help overlay open — Esc / ? close it (native dialog also closes on Esc).
    if (helpOpen()) {
      if (e.key === '?' ) { e.preventDefault(); toggleHelp(); }
      return;
    }

    // (3) Editable / chatbox focus ⇒ card keymap suspended (§8.4).
    if (isEditable(document.activeElement) || chatboxFocused()) return;

    // (4) Resolve + dispatch. Guard double-fire while a POST is in flight (Enter
    // spam would otherwise queue duplicate closes).
    var action = actionForKey(e.key);
    if (!action) return;
    if (inFlight && action.kind === 'post') { e.preventDefault(); return; }
    e.preventDefault();
    dispatch(action);
  });

  // Keep the suspend hint honest as focus moves in/out of the (future) chatbox.
  document.addEventListener('focusin', reflectSuspend);
  document.addEventListener('focusout', function () { setTimeout(reflectSuspend, 0); });

  // Focus the primary on load so Enter is one keystroke (§11 focus management).
  if (document.readyState !== 'loading') focusPrimary();
  else document.addEventListener('DOMContentLoaded', focusPrimary);
})();
