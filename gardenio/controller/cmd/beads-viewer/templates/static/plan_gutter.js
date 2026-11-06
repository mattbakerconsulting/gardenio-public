// plan_gutter.js — inline line-critique gutter + composer wiring for the plan
// cockpit (epic ka-0000002316, task ka-0000000167). Vanilla JS, no deps, no
// htmx-sse extension. Loaded UNCONDITIONALLY from plan_detail.html — the
// feature ships ON BY DEFAULT (operator decision ka-0000004596).
//
// Responsibilities:
//   1. Inject a "critique" gutter icon into every build-step row
//      (li.plan-tl-step[data-bead-id] → .plan-tl-body).
//   2. On icon click, bind the right-margin overlay (#plan-critique-overlay,
//      static shell from plan_critique.html) to that step, fetch the flat
//      thread fragment T3 renders (GET /plans/{id}/comments?step={beadID} →
//      <section id="plan-critique-thread">), inject it into the mount, and
//      call htmx.process so the fragment's hx-* wiring is live.
//   3. Text-selection-to-compose: a selection landing inside a [data-block-id]
//      block records a W3C-style hybrid anchor (block_id + exact quote + ≤32
//      chars of surrounding prefix/suffix) into the compose form's hidden
//      inputs — matching T3's POST field names.
//   4. Re-run gutter injection after each SSE `plan-updated` swap replaces
//      #plan-steps (rows get fresh DOM), via the `plan-events:health` custom
//      event plan_events.js dispatches, backed by a MutationObserver.
//
// Contract note (verified against merged T3, plans_critique.go): the STEP is a
// ?step= QUERY PARAM and the anchor rides in FOUR SEPARATE form fields
// (block_id/quote/prefix/suffix), NOT a single JSON `anchor` input. The
// compose form's hx-post carries only the plan path; this shim appends
// ?step={current} at request time via the htmx:configRequest event, which
// htmx re-reads fresh (it does not re-read the hx-post attribute after
// processing).
(function () {
  'use strict';

  if (typeof document === 'undefined') return;

  var ANCHOR_CONTEXT = 32; // W3C prefix/suffix window, matches critiqueAnchor.

  // critiqueDebug gates the per-comment anchor-resolve trace, which echoes
  // operator-authored quote text and internal block ids on every resolve pass.
  // Off by default; opt in with `window.CRITIQUE_DEBUG = true` (or
  // localStorage 'critique_debug'='1') in devtools when diagnosing anchoring.
  function critiqueDebug() {
    try {
      if (typeof window !== 'undefined' && window.CRITIQUE_DEBUG) return true;
      if (typeof localStorage !== 'undefined' && localStorage.getItem('critique_debug') === '1') return true;
    } catch (e) { /* storage may throw in sandboxed frames */ }
    return false;
  }

  // Inline SVG (speech-bubble). Kept tiny; no external asset request.
  var ICON_SVG =
    '<svg viewBox="0 0 16 16" aria-hidden="true" focusable="false">' +
    '<path fill="currentColor" d="M2 2h12a1 1 0 0 1 1 1v8a1 1 0 0 1-1 1H6l-3 3v-3H2a1 1 0 0 1-1-1V3a1 1 0 0 1 1-1z"/>' +
    '</svg>';

  var overlay = null;   // #plan-critique-overlay (resolved lazily)
  var currentStep = ''; // step bead id the overlay is bound to

  function byId(id) { return document.getElementById(id); }

  function getOverlay() {
    if (!overlay) overlay = byId('plan-critique-overlay');
    return overlay;
  }

  function basePath() {
    var o = getOverlay();
    return o ? (o.getAttribute('data-base-path') || '') : '';
  }

  function planId() {
    var o = getOverlay();
    return o ? (o.getAttribute('data-plan-id') || '') : '';
  }

  // ---- Gutter icon injection -------------------------------------------------

  function injectGutterIcons() {
    var rows = document.querySelectorAll('li.plan-tl-step[data-bead-id]');
    for (var i = 0; i < rows.length; i++) {
      var row = rows[i];
      // Inject at the END of the step's title line so the icon sits at the far
      // right of the row (the title's flex:1 pushes it there) — AFTER any
      // operator Done/Reopen buttons, never overlapping them. .plan-tl-line
      // lives inside .plan-tl-body, so this still hangs the gutter affordance
      // off the step body per the T4 spec.
      var line = row.querySelector('.plan-tl-body .plan-tl-line');
      if (!line) continue;
      if (line.querySelector(':scope > .critique-gutter-icon')) continue; // idempotent
      var beadID = row.getAttribute('data-bead-id');
      if (!beadID) continue;
      var btn = document.createElement('button');
      btn.type = 'button';
      btn.className = 'critique-gutter-icon';
      btn.setAttribute('data-step-id', beadID);
      btn.setAttribute('aria-expanded', 'false');
      btn.setAttribute('aria-label', 'Critique this step');
      btn.title = 'Critique this step';
      btn.innerHTML = ICON_SVG;
      line.appendChild(btn);
    }
    // An SSE `plan-updated` swap re-mints these rows, dropping the open overlay's
    // gutter-icon active state and selected-row tint. Re-apply them so the bound
    // step stays visibly selected across a live step re-render.
    if (currentStep && getOverlay() && !getOverlay().hidden) {
      markActiveIcon(currentStep);
      markSelectedRow(currentStep);
    }
  }

  // ---- Overlay open / close --------------------------------------------------

  function openOverlay(stepID, stepTitle) {
    var o = getOverlay();
    if (!o) return;
    // Switching to a DIFFERENT step drops the prior step's pending anchor so it
    // never leaks across steps. Re-opening the SAME step keeps it. In neither
    // case do we re-capture the live selection: the gutter click has already
    // collapsed any selection, and the "select-then-click" gesture recorded its
    // anchor on the selection's mouseup (pending-selection state).
    if (currentStep && currentStep !== stepID) clearAnchor();
    currentStep = stepID;
    o.hidden = false;
    // next frame so the transition runs from the off-canvas start position
    requestAnimationFrame(function () { o.classList.add('is-open'); });
    o.setAttribute('aria-hidden', 'false');
    var label = byId('plan-critique-step-label');
    if (label) label.textContent = stepTitle || stepID;
    markActiveIcon(stepID);
    markSelectedRow(stepID);
    fetchThread(stepID);
  }

  function closeOverlay() {
    var o = getOverlay();
    if (!o) return;
    o.classList.remove('is-open');
    o.setAttribute('aria-hidden', 'true');
    // Hide after the slide-out so it leaves the tab order.
    setTimeout(function () { if (!o.classList.contains('is-open')) o.hidden = true; }, 240);
    markActiveIcon(null);
    markSelectedRow(null);
    clearAnchor(); // reopening a (possibly different) step starts clean
    currentStep = '';
  }

  function markActiveIcon(stepID) {
    var icons = document.querySelectorAll('.critique-gutter-icon');
    for (var i = 0; i < icons.length; i++) {
      var on = stepID && icons[i].getAttribute('data-step-id') === stepID;
      icons[i].setAttribute('aria-expanded', on ? 'true' : 'false');
    }
  }

  // ---- Thread fetch + inject -------------------------------------------------

  function fetchThread(stepID) {
    var mount = byId('plan-critique-mount');
    if (!mount) return;
    if (typeof window.fetch !== 'function') {
      mount.innerHTML = '<p class="empty">Live critique needs a newer browser.</p>';
      return;
    }
    var url = basePath() + '/plans/' + encodeURIComponent(planId()) +
      '/comments?step=' + encodeURIComponent(stepID);
    fetch(url, { credentials: 'include', cache: 'no-store', headers: { 'Accept': 'text/html' } })
      .then(function (resp) {
        if (!resp.ok) throw new Error('status ' + resp.status);
        return resp.text();
      })
      .then(function (html) {
        if (currentStep !== stepID) return; // operator moved on; drop stale
        mount.innerHTML = html;
        // Re-wire the injected fragment's hx-* attrs (mirror swapById in
        // plan_events.js:56).
        if (window.htmx && typeof window.htmx.process === 'function') {
          try { window.htmx.process(mount); } catch (e) { /* ignore */ }
        }
        // T6: resolve every comment's anchor now that the thread is mounted —
        // tier-1/2 hits get a margin chip + hover highlight on the prose block,
        // unresolvable comments get the orphan indicator.
        resolveThreadAnchors();
      })
      .catch(function () {
        if (currentStep === stepID) {
          mount.innerHTML = '<p class="empty">Could not load critique.</p>';
        }
      });
  }

  // ---- Live critique refresh (SSE plan-comments-updated) --------------------

  // T5 (ka-0000007360): when a SECOND operator posts a critique on a step the
  // FIRST has open, the plan SSE stream emits a `plan-comments-updated` event
  // carrying the re-rendered plan_critique_thread fragment. plan_events.js owns
  // the EventSource (`es`) and re-dispatches that fragment as a DOM CustomEvent
  // (same file-bridge idiom as `plan-events:health`); we own the overlay's
  // open/bound-step state, so we do the conditional swap here. Only swap when
  // (a) the overlay is open and (b) the fragment's data-step-id matches the
  // step the overlay is bound to — a comment on some OTHER step must not clobber
  // the thread the operator is reading. This never re-fetches; the frame IS the
  // rendered thread, exactly as the POST re-render's outerHTML swap.
  function onLiveComments(ev) {
    var html = ev && ev.detail && ev.detail.html;
    if (!html) return;
    var o = getOverlay();
    if (!o || o.hidden) return; // overlay closed → nothing on-screen to refresh
    var mount = byId('plan-critique-mount');
    if (!mount) return;
    var current = byId('plan-critique-thread');
    if (!current) return; // no thread injected yet (mount still shows the hint)
    var tmp = document.createElement('div');
    tmp.innerHTML = html;
    var next = tmp.firstElementChild;
    if (!next || next.id !== 'plan-critique-thread') return;
    // Gate on the bound step: the fragment's data-step-id must be the step the
    // overlay is currently open on. Fall back to the DOM node's own step id when
    // currentStep is somehow unset (defensive; openOverlay always sets it).
    var frameStep = next.getAttribute('data-step-id') || '';
    var boundStep = currentStep || current.getAttribute('data-step-id') || '';
    if (!frameStep || frameStep !== boundStep) return;
    current.parentNode.replaceChild(next, current);
    // Re-wire the swapped fragment's hx-* attrs (mirror swapById in
    // plan_events.js and fetchThread above).
    if (window.htmx && typeof window.htmx.process === 'function') {
      try { window.htmx.process(next); } catch (e) { /* ignore */ }
    }
  }

  // ---- Text-selection → hidden anchor inputs --------------------------------

  function findBlock(node) {
    var el = (node && node.nodeType === 3) ? node.parentNode : node;
    while (el && el.nodeType === 1) {
      if (el.hasAttribute && el.hasAttribute('data-block-id')) return el;
      el = el.parentNode;
    }
    return null;
  }

  // captureSelectionAnchor reads the live text selection and, when it is a
  // non-empty range wholly inside a single [data-block-id] block (T1), records
  // the W3C-style hybrid anchor into the compose form's hidden inputs. Returns
  // true when an anchor was recorded, false otherwise (so callers can decide
  // whether to keep or clear a prior anchor). It never clobbers an existing
  // anchor on an empty/collapsed selection — a plain click leaves it intact.
  function captureSelectionAnchor() {
    var form = byId('plan-critique-form');
    if (!form) return false; // read-only viewer: no compose form
    var sel = window.getSelection ? window.getSelection() : null;
    if (!sel || sel.rangeCount === 0) return false;
    var quote = sel.toString().trim();
    if (!quote) return false;
    // Ignore selections made INSIDE the overlay itself (e.g. selecting an
    // existing comment's text): the thread <li>s carry a data-block-id data
    // attribute, which would otherwise be mistaken for a fresh plan-prose
    // anchor. Only plan-body prose selections should anchor.
    var o = getOverlay();
    if (o && sel.anchorNode && o.contains(sel.anchorNode)) return false;
    // Both ends must sit inside the SAME [data-block-id] block for a stable
    // single-block anchor.
    var startBlock = findBlock(sel.anchorNode);
    var endBlock = findBlock(sel.focusNode);
    if (!startBlock || startBlock !== endBlock) return false;
    var blockText = startBlock.textContent || '';
    var idx = blockText.indexOf(quote);
    var prefix = '', suffix = '';
    if (idx >= 0) {
      prefix = blockText.slice(Math.max(0, idx - ANCHOR_CONTEXT), idx);
      suffix = blockText.slice(idx + quote.length, idx + quote.length + ANCHOR_CONTEXT);
    }
    setAnchor(startBlock.getAttribute('data-block-id') || '', quote, prefix, suffix);
    return true;
  }

  function onMouseUp() {
    // Highlight-to-critique (ka-0000003210, Matt's ask): a text selection landing
    // in the plan prose OPENS the critique tray bound to the step that passage
    // belongs to, then records the line anchor. openTrayForSelection owns that
    // gesture; when it declines (no prose selection, or plan has no steps) we
    // fall back to the legacy capture-only behavior so a re-anchor on an already-
    // open step, or a plain click, behaves exactly as before.
    if (openTrayForSelection()) return;
    captureSelectionAnchor();
  }

  // ---- Highlight → open tray on the mapped step (ka-0000003210) --------------
  //
  // Matt's ask: "when you highlight part of the plan, it must bring up the
  // critique tray with that step selected." The prose→step map reuses the
  // section cross-ref the cockpit already renders: narrative headings carry a
  // stable slug `id` (headingAnchorTransformer, anchor.go) and each step row
  // stamps `data-section-slug` with the same slug when its description refs the
  // plan bundle's section (planSectionRefView.Slug, plan_steps.html). So a
  // passage under heading `#c1-1` maps to the step whose data-section-slug is
  // `c1-1`. When no section maps (intro prose, unlabeled heading), we still open
  // the tray on a sensible default step so the ask's "must bring up the tray"
  // always holds — a highlight is never a dead gesture.

  // proseSelectionBlock returns the [data-block-id] plan-prose block the current
  // selection STARTS in, or null when the selection is empty/collapsed, sits
  // inside the overlay (selecting an existing comment), or is not plan prose.
  function proseSelectionBlock(sel) {
    if (!sel || sel.rangeCount === 0 || sel.isCollapsed) return null;
    if (!sel.toString().trim()) return null;
    var o = getOverlay();
    if (o && sel.anchorNode && o.contains(sel.anchorNode)) return null;
    var block = findBlock(sel.anchorNode);
    if (!block) return null;
    var container = proseContainer();
    if (!container || !container.contains(block)) return null;
    return block;
  }

  function isHeadingWithId(el) {
    return !!(el && /^H[1-6]$/.test(el.tagName || '') && el.getAttribute('id'));
  }

  // sectionSlugForBlock returns the slug id of the narrative heading whose
  // section the block falls under: the block's own id when it IS a heading, else
  // the nearest heading in document order that the block follows. "" when the
  // block sits before the first heading (or the prose has none).
  function sectionSlugForBlock(block) {
    var container = proseContainer();
    if (!container || !block) return '';
    if (isHeadingWithId(block)) return block.getAttribute('id') || '';
    var headings = container.querySelectorAll('h1[id],h2[id],h3[id],h4[id],h5[id],h6[id]');
    var FOLLOWING = (typeof Node !== 'undefined' && Node.DOCUMENT_POSITION_FOLLOWING) || 4;
    var best = '';
    for (var i = 0; i < headings.length; i++) {
      // headings are in document order; the LAST one the block follows is the
      // nearest preceding heading — its section owns the block.
      if (headings[i].compareDocumentPosition(block) & FOLLOWING) {
        best = headings[i].getAttribute('id') || best;
      }
    }
    return best;
  }

  // stepRowForSlug finds the step row bound to a section slug, preferring the
  // first OPEN step (critique the live work) over a done one. Manual scan (not a
  // `[data-section-slug="…"]` selector) sidesteps CSS.escape and any selector
  // injection from a hand-authored slug.
  function stepRowForSlug(slug) {
    if (!slug) return null;
    var rows = document.querySelectorAll('li.plan-tl-step[data-section-slug]');
    var firstMatch = null;
    for (var i = 0; i < rows.length; i++) {
      if (rows[i].getAttribute('data-section-slug') !== slug) continue;
      if (!firstMatch) firstMatch = rows[i];
      if (!rows[i].classList.contains('is-done')) return rows[i];
    }
    return firstMatch;
  }

  // fallbackStep is the step the tray binds to when a highlight maps to no
  // section: the in-progress step, else the first still-open step, else the
  // first step. Null only when the plan has no steps at all.
  function fallbackStep() {
    return document.querySelector('li.plan-tl-step.is-active[data-bead-id]') ||
      document.querySelector('li.plan-tl-step:not(.is-done)[data-bead-id]') ||
      document.querySelector('li.plan-tl-step[data-bead-id]');
  }

  // markSelectedRow tints the step the tray is bound to (companion to
  // markActiveIcon's gutter-icon highlight), cleared when the tray closes.
  function markSelectedRow(stepID) {
    if (stepID) ensureAnchorStyles(); // the tint/flash CSS may not be injected yet
    var rows = document.querySelectorAll('li.plan-tl-step');
    for (var i = 0; i < rows.length; i++) {
      var on = stepID && rows[i].getAttribute('data-bead-id') === stepID;
      rows[i].classList.toggle('critique-step-selected', !!on);
    }
  }

  // flashAndRevealStep scrolls the bound step into view (a highlight in the left
  // prose column can bind a step off-screen in the right column) and pulses it
  // once so the "that step is now selected" link is unmistakable.
  function flashAndRevealStep(row) {
    if (!row) return;
    if (row.scrollIntoView) {
      try { row.scrollIntoView({ block: 'nearest', inline: 'nearest' }); }
      catch (e) { try { row.scrollIntoView(); } catch (e2) { /* ignore */ } }
    }
    row.classList.remove('critique-step-flash');
    void row.offsetWidth; // reflow so re-adding restarts the one-shot animation
    row.classList.add('critique-step-flash');
  }

  // openTrayForSelection is the mouseup entry point for highlight-to-critique.
  // Returns true when it handled the gesture (opened/rebound the tray + recorded
  // the anchor), false when it declined so onMouseUp can fall back to the legacy
  // capture-only path. Operator-only: gated on the compose form exactly like
  // captureSelectionAnchor, so read-only viewers keep the gutter-icon path.
  function openTrayForSelection() {
    if (!byId('plan-critique-form')) return false;
    var sel = window.getSelection ? window.getSelection() : null;
    var block = proseSelectionBlock(sel);
    if (!block) return false;
    var slug = sectionSlugForBlock(block);
    var stepEl = slug ? stepRowForSlug(slug) : null;
    var o = getOverlay();
    var trayOpen = !!(o && !o.hidden);
    if (!stepEl) {
      // No mapped step. If the operator already has the tray open on a step they
      // deliberately chose, keep it and just re-anchor — don't yank them to a
      // fallback step. If the tray is closed, open on the fallback so the ask's
      // "must bring up the tray" still holds.
      if (trayOpen && currentStep) { captureSelectionAnchor(); return true; }
      stepEl = fallbackStep();
    }
    if (!stepEl) return false; // plan has no steps to bind to
    var stepID = stepEl.getAttribute('data-bead-id');
    if (!stepID) return false;
    if (trayOpen && stepID === currentStep) {
      // Same step already open: refresh the anchor, no thread re-fetch/flicker.
      captureSelectionAnchor();
    } else {
      var titleEl = stepEl.querySelector('.plan-tl-title');
      openOverlay(stepID, titleEl ? titleEl.textContent.trim() : '');
      // AFTER openOverlay: a cross-step open calls clearAnchor(), so capturing
      // here is what survives onto the freshly-bound step.
      captureSelectionAnchor();
    }
    flashAndRevealStep(stepEl);
    return true;
  }

  function setField(form, name, value) {
    var el = form.querySelector('input[name="' + name + '"]');
    if (el) el.value = value;
  }

  function setAnchor(blockID, quote, prefix, suffix) {
    var form = byId('plan-critique-form');
    if (!form) return;
    setField(form, 'block_id', blockID);
    setField(form, 'quote', quote);
    setField(form, 'prefix', prefix);
    setField(form, 'suffix', suffix);
    var chip = byId('plan-critique-anchor-chip');
    var q = byId('plan-critique-anchor-quote');
    if (q) q.textContent = '“' + quote + '”';
    if (chip) chip.hidden = false;
  }

  function clearAnchor() {
    var form = byId('plan-critique-form');
    if (form) {
      setField(form, 'block_id', '');
      setField(form, 'quote', '');
      setField(form, 'prefix', '');
      setField(form, 'suffix', '');
    }
    var chip = byId('plan-critique-anchor-chip');
    if (chip) chip.hidden = true;
  }

  // ---- 3-tier anchor resolver + orphan state (T6, ka-0000000456) ------------
  //
  // Given a stored critique anchor {block_id, quote, prefix, suffix}, locate the
  // plan-prose block it points at, resolving through three tiers of decreasing
  // confidence, and NEVER silently mis-anchor: a comment that no tier can place
  // is marked orphaned rather than pinned to a wrong block.
  //
  //   Tier 1 — exact block-id: the block the anchor was captured against still
  //            exists (querySelectorAll over [data-block-id]). Block ids are
  //            per-document (`blk-N`), and #plan-narrative holds MULTIPLE
  //            documents (the epic prose + each bundle doc), so a bare id can be
  //            ambiguous — we require the stored quote to still be inside the
  //            id-matched block. If the id exists but its text changed so the
  //            quote is gone, we fall through rather than mis-anchor.
  //   Tier 2 — exact-quote walk: the originating block was deleted/renumbered
  //            but its exact text survives in some other block.
  //   Tier 3 — bounded fuzzy: the text itself was lightly edited. A bounded
  //            approximate-substring (Sellers) match of prefix+quote+suffix
  //            against each block's visible text, accepted only within a small
  //            edit-distance budget; compute is bounded so a pathological block
  //            can never hang the page.
  //   Tier 0 — orphaned: no tier placed it. The comment chip is marked
  //            `critique-comment-orphaned` with a visible "Document changed —
  //            anchor lost" indicator.
  //
  // The resolver is purely client-side and reads only what T1 stamps into the
  // DOM (data-block-id) and what T3 stamps onto each comment <li>
  // (data-block-id/quote/prefix/suffix). No server changes.

  var FUZZY_MIN_LEN = 4;         // ignore too-short needles: fuzzy is unreliable
  var FUZZY_MAX_NEEDLE = 400;    // skip tier-3 for very long needles
  var FUZZY_MAX_DIST = 12;       // absolute edit-distance ceiling
  var FUZZY_RATIO = 0.2;         // per-length distance budget (20%)
  var FUZZY_MAX_CELLS = 2000000; // DP cell cap per BLOCK (bounds one comparison)
  var FUZZY_PASS_CELLS = 8000000;// DP cell cap per resolveThreadAnchors PASS
  var STYLE_ID = 'critique-anchor-styles';

  // fuzzyBudget bounds the AGGREGATE tier-3 DP work across a single
  // resolveThreadAnchors pass (comments × blocks), on top of the per-block
  // FUZZY_MAX_CELLS cap. resolveThreadAnchors resets it before iterating so no
  // pathological plan (many near-cap blocks × many fuzzy-falling comments) can
  // stall the main thread; when exhausted, remaining comments simply orphan
  // (correct fallback — never a mis-anchor, never a hang). A resolveAnchor call
  // made outside a pass (e.g. a direct/unit call) runs with a fresh full budget.
  var fuzzyBudget = FUZZY_PASS_CELLS;

  // narrativeObserver watches the plan-prose column for the SSE narrative swap.
  // plan_events.js onNarrativeEvent does a vanilla replaceChild of
  // #plan-narrative that fires NEITHER htmx:afterSwap NOR plan-events:health, so
  // a MutationObserver is the only client-side signal that the prose blocks the
  // chips sit on were replaced. resolveThreadAnchors disconnects it around its
  // own chip mutations so those never re-trigger it (see rearmNarrativeObserver).
  var narrativeObserver = null;

  // proseContainer is the single region that holds anchorable plan-prose blocks
  // (the narrative summary + bundle doc cards, all under #plan-narrative). The
  // critique overlay's thread <li>s ALSO carry data-block-id, so scoping the
  // resolver here is what keeps a comment from ever resolving onto itself.
  function proseContainer() { return byId('plan-narrative'); }

  // resolveAnchor locates the plan-prose block a stored anchor points at.
  // Returns {tier: 1|2|3|0, block: Element|null}; tier 0 (block null) is orphan.
  function resolveAnchor(anchor, container) {
    anchor = anchor || {};
    container = container || proseContainer();
    var result = { tier: 0, block: null };
    if (!container) return result;
    var blocks = container.querySelectorAll('[data-block-id]');
    if (!blocks.length) return result;

    var id = (anchor.block_id || '').trim();
    var quote = anchor.quote || '';
    var quoteTrim = quote.trim();

    // Tier 1 — exact block-id, quote-guarded against per-document id collisions.
    if (id) {
      var idMatches = [];
      for (var i = 0; i < blocks.length; i++) {
        if (blocks[i].getAttribute('data-block-id') === id) idMatches.push(blocks[i]);
      }
      if (idMatches.length) {
        if (!quoteTrim) {
          // No quote to disambiguate: accept the id match ONLY when it is
          // unique. With duplicate `blk-N` ids across the multiple documents
          // under #plan-narrative, pinning to idMatches[0] would be a silent
          // mis-anchor — the one thing this resolver must never do — so we
          // fall through to orphan instead. (Unreachable for any anchor the
          // capture path produces, which always carries a non-empty quote;
          // this guards hand-crafted/corrupted stored anchors.)
          if (idMatches.length === 1) { result.tier = 1; result.block = idMatches[0]; return result; }
        } else {
          for (var m = 0; m < idMatches.length; m++) {
            if ((idMatches[m].textContent || '').indexOf(quoteTrim) >= 0) {
              result.tier = 1; result.block = idMatches[m]; return result;
            }
          }
          // id present but its text no longer contains the quote — the block
          // was edited. Fall through to the quote-first tiers rather than
          // mis-anchor.
        }
      }
    }

    // Tier 2 — exact-quote walk: first block that still contains the quote.
    if (quoteTrim) {
      for (var j = 0; j < blocks.length; j++) {
        if ((blocks[j].textContent || '').indexOf(quoteTrim) >= 0) {
          result.tier = 2; result.block = blocks[j]; return result;
        }
      }
    }

    // Tier 3 — bounded fuzzy over prefix+quote+suffix vs each block's text.
    var needle = ((anchor.prefix || '') + quote + (anchor.suffix || '')).trim();
    if (needle.length >= FUZZY_MIN_LEN && needle.length <= FUZZY_MAX_NEEDLE) {
      var k = Math.min(FUZZY_MAX_DIST, Math.max(1, Math.floor(needle.length * FUZZY_RATIO)));
      var bestBlock = null, bestDist = k + 1;
      for (var f = 0; f < blocks.length; f++) {
        var text = blocks[f].textContent || '';
        if (!text) continue;
        var cells = text.length * needle.length;
        if (cells > FUZZY_MAX_CELLS) continue;      // per-block bound
        if (fuzzyBudget <= 0) break;                // aggregate pass bound exhausted
        fuzzyBudget -= cells;                       // charge worst-case DP cells
        var d = fuzzySubstringDistance(text, needle, k);
        if (d < bestDist) { bestDist = d; bestBlock = blocks[f]; if (d === 0) break; }
      }
      if (bestBlock) { result.tier = 3; result.block = bestBlock; return result; }
    }

    return result; // tier 0 → orphaned
  }

  // fuzzySubstringDistance returns the minimum Levenshtein distance between
  // `pattern` and any substring of `text` (Sellers' approximate-substring DP:
  // row 0 is all-zero so a match may start at any text position for free). It
  // early-exits once a whole DP row exceeds maxK — the per-row minimum is
  // monotonically non-decreasing in the pattern index, so no completing match
  // can then come in under budget. Returns maxK+1 when it early-exits.
  function fuzzySubstringDistance(text, pattern, maxK) {
    var n = text.length, mLen = pattern.length;
    if (mLen === 0) return 0;
    var prev = new Array(n + 1);
    for (var j = 0; j <= n; j++) prev[j] = 0;
    for (var i = 1; i <= mLen; i++) {
      var cur = new Array(n + 1);
      cur[0] = i; // column 0: i deletions of the pattern prefix
      var rowMin = cur[0];
      var pc = pattern.charCodeAt(i - 1);
      for (var c = 1; c <= n; c++) {
        var cost = (text.charCodeAt(c - 1) === pc) ? 0 : 1;
        var del = prev[c] + 1;
        var ins = cur[c - 1] + 1;
        var sub = prev[c - 1] + cost;
        var v = del < ins ? del : ins;
        if (sub < v) v = sub;
        cur[c] = v;
        if (v < rowMin) rowMin = v;
      }
      if (rowMin > maxK) return maxK + 1; // no completion can beat the budget
      prev = cur;
    }
    var best = prev[0];
    for (var b = 1; b <= n; b++) if (prev[b] < best) best = prev[b];
    return best;
  }

  // ---- Overlay-render integration: chips, orphan marks, hover highlight -----

  function anchorFromComment(li) {
    return {
      block_id: li.getAttribute('data-block-id') || '',
      quote: li.getAttribute('data-quote') || '',
      prefix: li.getAttribute('data-prefix') || '',
      suffix: li.getAttribute('data-suffix') || ''
    };
  }

  // A comment carries an anchor only when T3 stamped one. Unanchored plan/step-
  // level comments are never orphaned — they were never pinned to a line.
  function anchorHasData(a) {
    return !!(a.block_id || a.quote || a.prefix || a.suffix);
  }

  function markOrphaned(li) {
    li.classList.add('critique-comment-orphaned');
    if (li.querySelector('.critique-orphan-indicator')) return; // idempotent
    var ind = document.createElement('span');
    ind.className = 'critique-orphan-indicator';
    ind.textContent = 'Document changed — anchor lost';
    var meta = li.querySelector('.comment-meta');
    if (meta) meta.appendChild(ind); else li.insertBefore(ind, li.firstChild);
  }

  function clearOrphaned(li) {
    li.classList.remove('critique-comment-orphaned');
    var ind = li.querySelector('.critique-orphan-indicator');
    if (ind && ind.parentNode) ind.parentNode.removeChild(ind);
  }

  // Remove all injected margin chips + highlight state so a re-resolve (thread
  // swap, SSE refresh, narrative live-edit) rebuilds from a clean slate.
  function clearAnchorChips(container) {
    container = container || proseContainer();
    if (!container) return;
    var chips = container.querySelectorAll('.critique-anchor-chip');
    for (var i = 0; i < chips.length; i++) {
      if (chips[i].parentNode) chips[i].parentNode.removeChild(chips[i]);
    }
    var hot = container.querySelectorAll('.critique-block-highlight');
    for (var h = 0; h < hot.length; h++) hot[h].classList.remove('critique-block-highlight');
  }

  function injectBlockChip(block, count) {
    ensureAnchorStyles();
    // A chip needs a positioned ancestor to sit in the block's margin.
    if (window.getComputedStyle && window.getComputedStyle(block).position === 'static') {
      block.style.position = 'relative';
    }
    var chip = document.createElement('span');
    chip.className = 'critique-anchor-chip';
    var noun = 'critique comment' + (count === 1 ? '' : 's');
    chip.title = count + ' ' + noun + ' anchored here';
    chip.setAttribute('aria-label', chip.title);
    chip.innerHTML = ICON_SVG + '<span class="critique-anchor-chip-count"></span>';
    var c = chip.querySelector('.critique-anchor-chip-count');
    if (c) c.textContent = String(count);
    block.appendChild(chip);
  }

  // rangeForQuote maps the first occurrence of `quote` in a block's textContent
  // to a DOM Range across its text nodes, so the exact quoted span (not the
  // whole block) can be highlighted. Returns null when unavailable/absent.
  function rangeForQuote(block, quote) {
    if (!block || !quote || !document.createRange || typeof document.createTreeWalker !== 'function') return null;
    var full = block.textContent || '';
    var at = full.indexOf(quote);
    if (at < 0) return null;
    var start = at, end = at + quote.length;
    var walker = document.createTreeWalker(block, NodeFilter.SHOW_TEXT, null, false);
    var pos = 0, range = document.createRange(), haveStart = false, node;
    while ((node = walker.nextNode())) {
      var nlen = node.nodeValue.length;
      if (!haveStart && pos + nlen > start) { range.setStart(node, start - pos); haveStart = true; }
      if (haveStart && pos + nlen >= end) { range.setEnd(node, end - pos); return range; }
      pos += nlen;
    }
    return null;
  }

  function highlightBlockRange(block, quote) {
    if (!block) return;
    block.classList.add('critique-block-highlight');
    var sel = window.getSelection ? window.getSelection() : null;
    if (!sel || !quote) return;
    // Never clobber a live user selection.
    if (sel.rangeCount && !sel.isCollapsed) return;
    var r = rangeForQuote(block, quote.trim());
    if (!r) return;
    try { sel.removeAllRanges(); sel.addRange(r); } catch (e) { /* ignore */ }
  }

  function clearHighlight(block, quote) {
    if (block) block.classList.remove('critique-block-highlight');
    var sel = window.getSelection ? window.getSelection() : null;
    // Only clear the selection if it is the range we programmatically set.
    if (sel && quote && sel.rangeCount && sel.toString() === quote.trim()) {
      try { sel.removeAllRanges(); } catch (e) { /* ignore */ }
    }
  }

  // hoverWired keeps per-<li> hover listeners from stacking when a re-resolve
  // reuses the same DOM nodes (e.g. a narrative live-swap with the thread
  // unchanged). Fresh thread swaps mint fresh <li>s, which the WeakSet drops.
  var hoverWired = (typeof WeakSet === 'function') ? new WeakSet() : null;

  function wireHover(li, block, quote) {
    li._critiqueBlock = block;
    li._critiqueQuote = quote;
    if (hoverWired) { if (hoverWired.has(li)) return; hoverWired.add(li); }
    li.addEventListener('mouseenter', function () { highlightBlockRange(li._critiqueBlock, li._critiqueQuote); });
    li.addEventListener('mouseleave', function () { clearHighlight(li._critiqueBlock, li._critiqueQuote); });
  }

  // resolveThreadAnchors is the overlay-render hook: for every anchored comment
  // in the mounted thread it resolves the anchor, tallies a margin chip on the
  // matched prose block, wires hover-highlight, or marks the comment orphaned.
  // Idempotent — clears prior chips/marks first so it is safe to re-run on every
  // thread swap, SSE refresh, or narrative live-edit.
  function resolveThreadAnchors() {
    // Suspend the narrative observer for the whole pass: the chip add/remove +
    // style mutations below all land inside #plan-narrative and would otherwise
    // re-trigger the observer, looping. disconnect() also drops any queued
    // records, so a swap that arrived mid-pass is coalesced into this run. The
    // finally clause re-arms on EVERY exit path (including the early return).
    if (narrativeObserver) { try { narrativeObserver.disconnect(); } catch (e) { /* ignore */ } }
    try {
      var container = proseContainer();
      clearAnchorChips(container);
      fuzzyBudget = FUZZY_PASS_CELLS; // reset the aggregate tier-3 budget per pass
      var thread = byId('plan-critique-thread') || byId('plan-critique-mount');
      if (!thread) return;
      var comments = thread.querySelectorAll('.plan-critique-comment');
      var tally = []; // [{block, total}]
      for (var i = 0; i < comments.length; i++) {
        var li = comments[i];
        var anchor = anchorFromComment(li);
        if (!anchorHasData(anchor)) { clearOrphaned(li); continue; }
        var res = resolveAnchor(anchor, container);
        if (critiqueDebug()) {
          console.debug('[critique] anchor resolve', {
            comment: li.getAttribute('data-comment-id') || '',
            tier: res.tier || 'orphaned',
            block_id: anchor.block_id,
            quote: anchor.quote
          });
        }
        // Neutralize any hover wiring retained from a prior resolve on this
        // reused <li>: null block/quote so the listeners no-op instead of
        // flashing a now-detached old block on hover.
        if (res.tier === 0 || !res.block) { wireHover(li, null, ''); markOrphaned(li); continue; }
        clearOrphaned(li);
        var entry = null;
        for (var t = 0; t < tally.length; t++) { if (tally[t].block === res.block) { entry = tally[t]; break; } }
        if (!entry) { entry = { block: res.block, total: 0 }; tally.push(entry); }
        entry.total++;
        // Tier 1/2 give an exact quote range; tier 3 is approximate, so we flash
        // the whole matched block rather than a possibly-wrong character span.
        wireHover(li, res.block, res.tier <= 2 ? (anchor.quote || '') : '');
      }
      for (var e = 0; e < tally.length; e++) injectBlockChip(tally[e].block, tally[e].total);
    } finally {
      rearmNarrativeObserver();
    }
  }

  // rearmNarrativeObserver (re)connects the narrative MutationObserver to the
  // CURRENT #plan-narrative's parent. Re-resolved each call because the SSE swap
  // replaces the #plan-narrative node itself — observing its parent subtree
  // survives the replaceChild. No-op when the browser lacks MutationObserver or
  // the prose column is absent (read-only viewer with no narrative).
  function rearmNarrativeObserver() {
    if (typeof MutationObserver !== 'function') return;
    var narr = proseContainer();
    var parent = narr && narr.parentNode;
    if (!parent) return;
    if (!narrativeObserver) {
      narrativeObserver = new MutationObserver(onNarrativeMutation);
    }
    try { narrativeObserver.observe(parent, { childList: true, subtree: true }); } catch (e) { /* ignore */ }
  }

  // onNarrativeMutation re-resolves after the SSE narrative swap rewrites the
  // prose blocks the chips were sitting on — but only while the overlay is open
  // (nothing on-screen to re-anchor otherwise) and only when the mutation is a
  // real node change (childList), so a reconnect storm or unrelated attribute
  // tick cannot thrash it.
  function onNarrativeMutation(records) {
    if (!getOverlay() || getOverlay().hidden) return;
    for (var i = 0; i < records.length; i++) {
      if (records[i].type === 'childList' &&
          (records[i].addedNodes.length || records[i].removedNodes.length)) {
        resolveThreadAnchors();
        return;
      }
    }
  }

  // ensureAnchorStyles injects the chip / orphan / highlight CSS once. Kept in
  // JS (not the template) so T6 touches a single file; id-guarded, so repeated
  // calls are no-ops.
  function ensureAnchorStyles() {
    if (byId(STYLE_ID)) return;
    var css =
      '.critique-anchor-chip{position:absolute;top:0.1rem;right:0.1rem;z-index:2;' +
      'display:inline-flex;align-items:center;gap:0.15rem;padding:0.05rem 0.4rem;' +
      'border-radius:999px;font-size:0.7rem;line-height:1.4;' +
      'border:1px solid var(--accent,#1d4ed8);background:var(--accent-soft,#dbeafe);' +
      'color:var(--text,#111);cursor:default;opacity:0.9;}' +
      '.critique-anchor-chip svg{width:0.72rem;height:0.72rem;display:block;}' +
      '.critique-anchor-chip-count{font-weight:600;}' +
      '.critique-block-highlight{background:var(--accent-soft,#dbeafe);' +
      'border-radius:4px;box-shadow:0 0 0 3px var(--accent-soft,#dbeafe);' +
      'transition:background 0.15s ease,box-shadow 0.15s ease;}' +
      '.critique-comment-orphaned{opacity:0.85;border-style:dashed;' +
      'border-color:var(--rule,#e6c96a);}' +
      '.critique-orphan-indicator{display:inline-flex;align-items:center;' +
      'margin-left:0.4rem;padding:0.02rem 0.4rem;border-radius:999px;' +
      'font-size:0.68rem;font-weight:600;background:#fff3cd;' +
      'border:1px solid #e6c96a;color:#5a4200;}' +
      // The step the critique tray is bound to (ka-0000003210): a soft accent
      // wash + left rule so an operator can see, at a glance, which step the open
      // tray belongs to — the counterpart to the gutter icon's aria-expanded skin.
      '.critique-step-selected{background:var(--accent-soft,#dbeafe);' +
      'border-radius:6px;box-shadow:inset 3px 0 0 var(--accent,#1d4ed8);}' +
      '@keyframes critique-step-flash{from{background:var(--accent,#1d4ed8);}' +
      'to{background:transparent;}}' +
      '.critique-step-flash{animation:critique-step-flash 0.9s ease-out;border-radius:6px;}' +
      '@media (prefers-reduced-motion: reduce){.critique-block-highlight{transition:none;}' +
      '.critique-step-flash{animation:none;}}';
    var style = document.createElement('style');
    style.id = STYLE_ID;
    style.textContent = css;
    (document.head || document.documentElement).appendChild(style);
  }

  // ---- Event wiring ----------------------------------------------------------

  function onDocClick(ev) {
    var icon = ev.target.closest ? ev.target.closest('.critique-gutter-icon') : null;
    if (icon) {
      ev.preventDefault();
      var stepID = icon.getAttribute('data-step-id');
      var row = icon.closest('li.plan-tl-step');
      var titleEl = row ? row.querySelector('.plan-tl-title') : null;
      var title = titleEl ? titleEl.textContent.trim() : '';
      if (currentStep === stepID && getOverlay() && !getOverlay().hidden) {
        closeOverlay(); // toggle: same icon closes
      } else {
        openOverlay(stepID, title);
      }
      return;
    }
    if (ev.target.closest && ev.target.closest('#plan-critique-anchor-clear')) {
      ev.preventDefault();
      clearAnchor();
      return;
    }
    if (ev.target.closest && ev.target.closest('#plan-critique-close')) {
      ev.preventDefault();
      closeOverlay();
    }
  }

  // htmx re-reads detail.path fresh at request time, so appending the current
  // step's ?step= here is the reliable way to route the compose POST to the
  // bound step (the hx-post attribute is stamped once at process time).
  function onConfigRequest(ev) {
    var form = byId('plan-critique-form');
    if (!form || !ev.detail || ev.detail.elt !== form) return;
    if (!currentStep) return;
    var sep = ev.detail.path.indexOf('?') >= 0 ? '&' : '?';
    ev.detail.path = ev.detail.path + sep + 'step=' + encodeURIComponent(currentStep);
  }

  // After a successful compose POST, T3 re-renders the thread (outerHTML swap
  // of #plan-critique-thread); reset the composer so the next critique starts
  // clean.
  function onAfterRequest(ev) {
    var form = byId('plan-critique-form');
    if (!form || !ev.detail || ev.detail.elt !== form) return;
    if (!ev.detail.successful) return;
    var ta = form.querySelector('textarea[name="text"]');
    if (ta) ta.value = '';
    clearAnchor();
  }

  // onAfterSwap re-resolves anchors after htmx replaces the critique thread —
  // the compose-POST re-render and T5's SSE live-refresh both outerHTML-swap
  // #plan-critique-thread, minting fresh comment <li>s whose anchors need
  // (re)placing. Ignores swaps elsewhere on the page.
  function onAfterSwap(ev) {
    var tgt = ev && ev.detail && ev.detail.target;
    if (!tgt) return;
    if (tgt.id === 'plan-critique-thread' || (tgt.querySelector && tgt.querySelector('#plan-critique-thread')) ||
        (tgt.closest && tgt.closest('#plan-critique-mount'))) {
      resolveThreadAnchors();
    }
  }

  // onPlanHealth is a COARSE secondary net, gated to the post-reconnect `live`
  // transition. The PRIMARY narrative-swap catch is narrativeObserver (the SSE
  // swap is a vanilla replaceChild that emits no htmx:afterSwap and no health
  // event, so only a MutationObserver sees it). This hook only re-resolves once
  // the stream goes `live` again after a drop — a cheap belt-and-suspenders pass
  // in case a swap landed while we were disconnected. Gating on `live` (rather
  // than every connecting/offline/error tick) avoids the reconnect-storm thrash
  // a blanket re-resolve would cause. Only meaningful while the overlay is open.
  function onPlanHealth(ev) {
    var state = ev && ev.detail && ev.detail.state;
    if (state && state !== 'live') return;
    if (getOverlay() && !getOverlay().hidden) resolveThreadAnchors();
  }

  function init() {
    injectGutterIcons();

    document.addEventListener('click', onDocClick);
    document.addEventListener('mouseup', onMouseUp);
    document.addEventListener('keydown', function (ev) {
      if (ev.key === 'Escape') closeOverlay();
    });
    document.body.addEventListener('htmx:configRequest', onConfigRequest);
    document.body.addEventListener('htmx:afterRequest', onAfterRequest);

    // T6 re-resolve nets. Anchors must be recomputed whenever EITHER the thread
    // (comment set changed) OR the plan prose (block ids/text changed) is
    // replaced, so a chip never lingers on a stale block and a freshly-broken
    // anchor flips to orphaned immediately. Three complementary nets, because
    // the two DOM regions are swapped by different mechanisms:
    //   1. htmx:afterSwap — the thread (#plan-critique-thread) re-renders via an
    //      htmx outerHTML swap on the compose-POST and on T5's SSE live-refresh.
    //   2. narrativeObserver — the prose (#plan-narrative) is swapped by
    //      plan_events.js via a VANILLA replaceChild that emits neither
    //      htmx:afterSwap nor plan-events:health, so a MutationObserver on its
    //      parent subtree is the only client-side signal; armed below.
    //   3. plan-events:health(state=live) — a coarse post-reconnect backstop for
    //      a swap that landed while the SSE stream was disconnected.
    document.body.addEventListener('htmx:afterSwap', onAfterSwap);
    document.addEventListener('plan-events:health', onPlanHealth);
    rearmNarrativeObserver();

    // Re-inject gutter icons after the SSE `plan-updated` swap replaces
    // #plan-steps (plan_events.js swaps outerHTML → new rows lose their
    // injected icons). Two coverage nets: (1) the `plan-events:health` custom
    // event plan_events.js dispatches on every lifecycle tick, and (2) a
    // MutationObserver on #plan-steps' parent so a bare DOM replace is also
    // caught. Both funnel to the idempotent injector.
    document.addEventListener('plan-events:health', injectGutterIcons);

    // Live critique refresh: swap an open overlay's thread when a matching
    // `plan-comments-updated` fragment arrives from the SSE stream (T5).
    document.addEventListener('plan-comments-updated', onLiveComments);
    var steps = byId('plan-steps');
    if (steps && steps.parentNode && typeof MutationObserver === 'function') {
      var obs = new MutationObserver(function () { injectGutterIcons(); });
      obs.observe(steps.parentNode, { childList: true, subtree: true });
    }
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
