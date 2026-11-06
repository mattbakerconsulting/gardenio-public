// beads-viewer comment draft autosave.
//
// Pure client-side, persists to localStorage so an in-progress comment
// survives navigation/tab close. See bead ka-0000001970.
//
// Behavior:
//   - On `input` in #comment-text, debounced 500ms, store the value at
//     localStorage["bv:draft:" + beadID].
//   - On page load (or htmx swap that re-renders the comment form), if the
//     textarea is empty (no server-side {{.CommentDraft}}) and a stored draft
//     exists, prefill it.
//   - On a successful POST (status 2xx) of the comment form itself, clear
//     that bead's stored draft and cancel the old textarea's pending save.
//     This runs on htmx:beforeSwap, because htmx fires htmx:afterSwap (which
//     re-binds and prefills the new, empty textarea) BEFORE
//     htmx:afterRequest; clearing only on afterRequest put the just-posted
//     text back in the composer (gd-cDR7idwi3b). afterRequest still clears,
//     for a 2xx that does not swap. The textarea's own preview POST
//     (.../comments/preview) never clears the draft.
//   - Total localStorage usage capped at ~200 KB by an LRU index over bead
//     IDs (oldest evicted first).
//   - Server-side draft (re-rendered after a POST error) wins over the
//     localStorage draft — we never overwrite a non-empty textarea.
(function () {
  'use strict';

  var KEY_PREFIX = 'bv:draft:';
  var INDEX_KEY = 'bv:drafts:index';
  var MAX_BYTES = 200 * 1024; // ~200 KB total budget for our keys
  var DEBOUNCE_MS = 500;

  function safeGet(key) {
    try { return window.localStorage.getItem(key); } catch (e) { return null; }
  }
  function safeSet(key, value) {
    try { window.localStorage.setItem(key, value); return true; } catch (e) { return false; }
  }
  function safeRemove(key) {
    try { window.localStorage.removeItem(key); } catch (e) { /* ignore */ }
  }

  function readIndex() {
    var raw = safeGet(INDEX_KEY);
    if (!raw) return [];
    try {
      var arr = JSON.parse(raw);
      if (Object.prototype.toString.call(arr) === '[object Array]') {
        // Filter to strings only.
        var out = [];
        for (var i = 0; i < arr.length; i++) {
          if (typeof arr[i] === 'string') out.push(arr[i]);
        }
        return out;
      }
    } catch (e) { /* fall through */ }
    return [];
  }
  function writeIndex(ids) {
    safeSet(INDEX_KEY, JSON.stringify(ids));
  }

  function totalBytes(ids) {
    var n = 0;
    for (var i = 0; i < ids.length; i++) {
      var v = safeGet(KEY_PREFIX + ids[i]);
      if (v != null) n += v.length + ids[i].length;
    }
    return n;
  }

  function touch(id) {
    var ids = readIndex();
    var out = [];
    for (var i = 0; i < ids.length; i++) {
      if (ids[i] !== id) out.push(ids[i]);
    }
    out.push(id); // most-recently-used at the end
    writeIndex(out);
  }

  function evictUntilUnder(budgetBytes) {
    var ids = readIndex();
    while (ids.length > 0 && totalBytes(ids) > budgetBytes) {
      var dropped = ids.shift();
      safeRemove(KEY_PREFIX + dropped);
    }
    writeIndex(ids);
  }

  function saveDraft(id, value) {
    if (!id) return;
    if (value === '') {
      clearDraft(id);
      return;
    }
    if (!safeSet(KEY_PREFIX + id, value)) {
      // Storage full — evict and retry once.
      evictUntilUnder(MAX_BYTES / 2);
      if (!safeSet(KEY_PREFIX + id, value)) return;
    }
    touch(id);
    evictUntilUnder(MAX_BYTES);
  }

  function loadDraft(id) {
    if (!id) return null;
    return safeGet(KEY_PREFIX + id);
  }

  function clearDraft(id) {
    if (!id) return;
    safeRemove(KEY_PREFIX + id);
    var ids = readIndex();
    var out = [];
    for (var i = 0; i < ids.length; i++) {
      if (ids[i] !== id) out.push(ids[i]);
    }
    writeIndex(out);
  }

  // Read the bead ID from the form's data-bead-id attribute (preferred) or
  // by parsing the hx-post URL (`.../b/<id>/comments`) as a fallback.
  function beadIDFromForm(form) {
    if (!form) return null;
    var id = form.getAttribute('data-bead-id');
    if (id) return id;
    var hx = form.getAttribute('hx-post') || '';
    var m = hx.match(/\/b\/([^\/]+)\/comments(?:[?#].*)?$/);
    return m ? decodeURIComponent(m[1]) : null;
  }

  // Returns a debounced fn with a .cancel() that drops a pending call.
  function debounce(fn, ms) {
    var t = null;
    var debounced = function () {
      var args = arguments;
      var ctx = this;
      if (t) clearTimeout(t);
      t = setTimeout(function () { t = null; fn.apply(ctx, args); }, ms);
    };
    debounced.cancel = function () {
      if (t) clearTimeout(t);
      t = null;
    };
    return debounced;
  }

  // Per-textarea state — bead ID + debounced saver. Stamped on the element
  // with a data-attribute so re-binding after htmx swaps is idempotent.
  function bindForm(form) {
    if (!form) return;
    if (form.getAttribute('data-draft-bound') === '1') return;
    var ta = form.querySelector('#comment-text');
    if (!ta) return;
    var id = beadIDFromForm(form);
    if (!id) return;
    form.setAttribute('data-draft-bound', '1');

    // Prefill: only if textarea is empty (server-side draft wins).
    if (ta.value === '' || ta.value == null) {
      var stored = loadDraft(id);
      if (stored != null && stored !== '') {
        ta.value = stored;
      }
    } else {
      // A non-empty server-side draft exists; sync localStorage to match so
      // the LRU index reflects recency, but don't overwrite the textarea.
      saveDraft(id, ta.value);
    }

    var save = debounce(function () { saveDraft(id, ta.value); }, DEBOUNCE_MS);
    ta.addEventListener('input', save);
    // Expando so the POST-success path can cancel this form's pending save
    // before the form is swapped out.
    form._bvCancelDraftSave = save.cancel;
  }

  function bindAll() {
    var forms = document.querySelectorAll('form.comment-form');
    for (var i = 0; i < forms.length; i++) bindForm(forms[i]);
  }

  // Initial bind on DOM ready.
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', bindAll);
  } else {
    bindAll();
  }

  // The bead ID when this htmx event belongs to a comment form's own POST
  // (`.../b/<id>/comments`), else null. Keyed on the request, not on
  // detail.elt: htmx fires beforeSwap on the swap target (#comments-section),
  // and the textarea's preview POST has the form as its closest ancestor.
  function commentPostBeadID(detail) {
    var cfg = detail && detail.requestConfig;
    if (!cfg || String(cfg.verb).toLowerCase() !== 'post') return null;
    var m = String(cfg.path || '').match(/\/b\/([^\/?#]+)\/comments(?:[?#].*)?$/);
    return m ? decodeURIComponent(m[1]) : null;
  }

  function isSuccess(detail) {
    if (detail.successful === true) return true;
    var xhr = detail.xhr;
    return !!(xhr && xhr.status >= 200 && xhr.status < 300);
  }

  // Forget a posted draft: cancel every bound form's pending save for the
  // bead (the old textarea would otherwise re-save the posted text after the
  // clear), then drop the stored entry.
  function forgetPostedDraft(id) {
    var forms = document.querySelectorAll('form.comment-form');
    for (var i = 0; i < forms.length; i++) {
      if (beadIDFromForm(forms[i]) === id && forms[i]._bvCancelDraftSave) {
        forms[i]._bvCancelDraftSave();
      }
    }
    clearDraft(id);
  }

  function onCommentPostDone(evt) {
    var detail = evt && evt.detail;
    if (!detail) return;
    var id = commentPostBeadID(detail);
    if (!id || !isSuccess(detail)) return;
    forgetPostedDraft(id);
  }

  // Clear before the swap, so the afterSwap re-bind below finds no stored
  // draft to prefill into the fresh, empty textarea.
  document.addEventListener('htmx:beforeSwap', onCommentPostDone);

  // Re-bind after htmx swaps (the comments section auto-refreshes for
  // in_progress beads, and the form is re-rendered after a POST).
  document.addEventListener('htmx:afterSwap', bindAll);

  // Also clear after the request, for a 2xx that does not swap.
  document.addEventListener('htmx:afterRequest', onCommentPostDone);
})();
